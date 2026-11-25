// 瑞兽迎春限时活动战斗引擎单测（docs/design/battle-festival-beast-implementation-contract.md）。
// 覆盖：准入防绕过、R2/5/8/11 确定性预告与重连不重抽、R3/6/9/12 180% 慢速物理重击、
// 低速被点名者防御生效与上轮防御不跨轮、减攻 Debuff 生效、受控取消（无补普攻/无顺延）、
// 睡眠唤醒后正常发动、目标失效取消不换目标、逃跑携宠离场、禁道具/捕捉与 0 常规奖励、
// 12 回合上限判负、以及物攻/法攻/控辅/治疗配输出宠四类流派 12 回合内通关验证。

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>
#include <string_view>
#include <vector>

#include "constants/turn_battle_constants.h"
#include "memory_battle_data_provider.h"
#include "system/combat_damage_rules.h"
#include "system/turn_battle_engine.h"
#include "table/proto/tip/common_error_tip.pb.h"

namespace {

using turnbattle::BattleEffectOp;
using turnbattle::BattleStateKind;
using turnbattle::TurnBattleEngine;

constexpr uint64_t kHeroId = 6001;
constexpr uint64_t kPetId = 8001;
constexpr uint32_t kDungeonId = 111;
constexpr uint32_t kMonsterId = 108;

BattleAction MakeAction(eBattleActionType type, uint64_t target = 0, uint32_t skill = 0, uint32_t item = 0) {
    BattleAction action;
    action.set_action_type(type);
    action.set_target_id(target);
    action.set_skill_table_id(skill);
    action.set_item_table_id(item);
    return action;
}

class FestivalBeastBattleTest : public ::testing::Test {
protected:
    void SetUp() override {
        data = std::make_shared<MemoryBattleDataProvider>();
        data->AddBattleReward(kDungeonId, "");

        auto& dungeon = data->AddDungeon(kDungeonId);
        dungeon.set_id(kDungeonId);
        dungeon.set_scene_id(17);
        dungeon.set_max_team_size(1);
        dungeon.set_time_limit(600);
        dungeon.add_monster(kMonsterId);
        dungeon.set_allow_pet_capture(0);
        dungeon.set_boss_trial_kind(turnbattle::kBossTrialKindFestivalBeast);
        dungeon.set_difficulty(1);
        dungeon.set_round_limit(12);

        auto& monster = data->AddMonster(kMonsterId);
        monster.set_id(kMonsterId);
        monster.set_health(2000);
        monster.set_strength(0);
        monster.set_armor(0);
        monster.set_resistance(5);
        monster.set_critchance(0);
        monster.set_speed(320);
        monster.set_is_boss(1);

        auto& item = data->AddItem(9001);
        item.set_id(9001);
        item.set_battle_usable(1);
        item.set_battle_heal_hp(500);
        item.set_max_stack_size(99);
    }

    CreateBattleRequest BuildRequest(uint64_t bossHp = 26000,
                                     uint64_t bossAtk = 1100,
                                     uint64_t bossDef = 1300,
                                     uint64_t bossRes = 12,
                                     uint64_t bossSpeed = 760,
                                     uint64_t seed = 42) const {
        CreateBattleRequest req;
        req.set_battle_id(770001);
        req.set_battle_config_id(kDungeonId);
        req.set_match_mode(turnbattle::kMatchModePveSolo);
        req.set_seed(seed);

        auto* p = req.add_players();
        p->set_player_id(kHeroId);
        p->set_player_name("少侠");
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(5000);
        p->set_max_mana(4000);
        p->mutable_base_attributes()->set_health(5000);
        p->mutable_base_attributes()->set_mana(4000);
        p->mutable_base_attributes()->set_speed(1000);
        p->set_physical_attack(5000);
        p->set_magic_attack(4000);
        p->set_defense(3000);

        auto* pet = p->add_pets();
        pet->set_pet_id(kPetId);
        pet->set_owner_player_id(kHeroId);
        pet->set_pet_name("输出灵狐");
        pet->set_pet_table_id(1);
        pet->set_level(30);
        pet->set_max_health(3500);
        pet->set_max_mana(2000);
        pet->mutable_base_attributes()->set_health(3500);
        pet->mutable_base_attributes()->set_mana(2000);
        pet->mutable_base_attributes()->set_speed(900);
        pet->set_physical_attack(4200);
        pet->set_magic_attack(2000);
        pet->set_defense(2200);

        auto* actx = req.mutable_activity_context();
        actx->set_kind(BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST);
        actx->set_initiator_player_id(kHeroId);
        auto* fb = actx->mutable_festival_beast();
        fb->set_activity_instance_id(202601);
        fb->set_config_revision(1);
        fb->set_reward_identity("festival_beast_chunxishou_v1");
        fb->set_reward_definition_version(1);
        fb->set_reward_pet_table_id(101);
        fb->set_required_wins(3);
        fb->set_locked_pet_id(kPetId);
        fb->set_locked_owner_level(30);
        fb->set_tier_id(3);
        fb->set_round_limit(12);
        fb->set_cycle_rounds(3);
        fb->set_first_heavy_round(3);
        fb->set_telegraph_lead_rounds(1);
        fb->set_heavy_damage_percent(180);
        fb->set_boss_name("春禧兽");
        fb->set_boss_health(bossHp);
        fb->set_boss_physical_attack(bossAtk);
        fb->set_boss_defense(bossDef);
        fb->set_boss_resistance(bossRes);
        fb->set_boss_speed(bossSpeed);
        fb->set_effective_start_at_ms(1795564800000ULL);
        fb->set_effective_end_at_ms(1796774400000ULL);
        fb->set_client_request_id("11223344-5566-4788-99aa-bbccddeeff01");
        return req;
    }

    uint64_t FindBossId(const TurnBattleEngine& engine) const {
        for (const auto& a : engine.BuildStateSnapshot().actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) return a.actor_id();
        }
        return 0;
    }

    uint64_t FindPetActorId(const TurnBattleEngine& engine) const {
        for (const auto& a : engine.BuildStateSnapshot().actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_PET && a.owner_player_id() == kHeroId) return a.actor_id();
        }
        return 0;
    }

    const BattleActorState* ActorInState(const BattleStateS2C& state, uint64_t id) const {
        for (const auto& a : state.actors()) {
            if (a.actor_id() == id) return &a;
        }
        return nullptr;
    }

    uint32_t Submit(TurnBattleEngine& eng, uint64_t actorId, const BattleAction& act) const {
        const uint32_t err = eng.ValidateAction(actorId, act);
        if (err == kSuccess) eng.SubmitAction(actorId, act);
        return err;
    }

    uint64_t DamageFromBossTo(const TurnResultS2C& tr, uint64_t bossId, uint64_t targetId) const {
        uint64_t total = 0;
        for (const auto& ev : tr.events()) {
            if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == bossId &&
                ev.target_id() == targetId) {
                total += ev.value();
            }
        }
        return total;
    }

    void AddControlSkill(uint32_t skillId, uint32_t buffId, BattleStateKind stateKind, uint32_t duration = 2) {
        auto& row = data->AddSkill(skillId);
        row.Clear();
        row.set_id(skillId);
        row.set_battle_model(1);
        row.set_caster_mask(1);
        row.set_ability_kind(1);
        row.set_unlock_level(1);
        row.set_max_rank(10);
        row.set_target_team(1);
        row.set_max_targets(1);
        row.add_skill_type(1);
        row.set_mana_percent(0.01);
        row.add_effect(buffId);

        auto& buff = data->AddBuff(buffId);
        buff.Clear();
        buff.set_id(buffId);
        buff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Control));
        buff.set_state_kind(static_cast<uint32_t>(stateKind));
        buff.set_duration_rounds(duration);
        buff.set_application_chance(1.0);
        (*buff.mutable_tag())["Control"] = true;
        (*buff.mutable_tag())["Debuff"] = true;
    }

    void AddAttackDebuffSkill(uint32_t skillId, uint32_t buffId, double deltaRatio) {
        auto& row = data->AddSkill(skillId);
        row.Clear();
        row.set_id(skillId);
        row.set_battle_model(1);
        row.set_caster_mask(1);
        row.set_ability_kind(1);
        row.set_unlock_level(1);
        row.set_max_rank(10);
        row.set_target_team(1);
        row.set_max_targets(1);
        row.add_skill_type(1);
        row.set_mana_percent(0.01);
        row.add_effect(buffId);

        auto& buff = data->AddBuff(buffId);
        buff.Clear();
        buff.set_id(buffId);
        buff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Modifier));
        buff.set_modifier_stat(static_cast<uint32_t>(turnbattle::BattleModifier::PhysicalAttack));
        buff.set_ratio(deltaRatio);
        buff.set_duration_rounds(2);
        (*buff.mutable_tag())["Debuff"] = true;
    }

    void AddHealSkill(uint32_t skillId, uint32_t buffId, double flatHeal) {
        auto& row = data->AddSkill(skillId);
        row.Clear();
        row.set_id(skillId);
        row.set_battle_model(1);
        row.set_caster_mask(1);
        row.set_ability_kind(1);
        row.set_unlock_level(1);
        row.set_max_rank(10);
        row.set_target_team(2);
        row.set_max_targets(2);
        row.add_skill_type(1);
        row.set_mana_percent(0.01);
        row.add_effect(buffId);

        auto& buff = data->AddBuff(buffId);
        buff.Clear();
        buff.set_id(buffId);
        buff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Heal));
        buff.set_flat_value(flatHeal);
    }

    void AddDamageSkill(uint32_t skillId, uint32_t buffId, uint32_t damageType, double flatDamage) {
        auto& row = data->AddSkill(skillId);
        row.Clear();
        row.set_id(skillId);
        row.set_battle_model(1);
        row.set_caster_mask(1);
        row.set_ability_kind(1);
        row.set_unlock_level(1);
        row.set_max_rank(10);
        row.set_target_team(1);
        row.set_max_targets(1);
        row.add_skill_type(1);
        row.set_damage_type(damageType);
        row.set_mana_percent(0.01);
        row.add_effect(buffId);

        auto& buff = data->AddBuff(buffId);
        buff.Clear();
        buff.set_id(buffId);
        buff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Damage));
        buff.set_flat_value(flatDamage);
        data->SetSkillDamage(skillId, flatDamage);
    }

    std::shared_ptr<MemoryBattleDataProvider> data;
};

TEST_F(FestivalBeastBattleTest, InitializeRejectsBypassAndMismatchedLockedPet) {
    // 无 activity_context 企图直接开 boss_trial_kind=7 副本 → 拒绝。
    auto noCtx = BuildRequest();
    noCtx.clear_activity_context();
    TurnBattleEngine eng1(data);
    EXPECT_FALSE(eng1.Initialize(noCtx));

    // 无宝宝或宝宝 pet_id 与锁定值不符 → 拒绝。
    auto wrongPet = BuildRequest();
    wrongPet.mutable_players(0)->mutable_pets(0)->set_pet_id(kPetId + 99);
    TurnBattleEngine eng2(data);
    EXPECT_FALSE(eng2.Initialize(wrongPet));

    // 多人进入瑞兽迎春 → 拒绝。
    auto twoPlayers = BuildRequest();
    *twoPlayers.add_players() = twoPlayers.players(0);
    twoPlayers.mutable_players(1)->set_player_id(kHeroId + 1);
    TurnBattleEngine eng3(data);
    EXPECT_FALSE(eng3.Initialize(twoPlayers));
}

TEST_F(FestivalBeastBattleTest, TelegraphScheduleDeterministicSlowHeavyDefendAndNoCarryover) {
    // 玩家与宝宝速度均低于首领（验证低速被点名者在 R3 仍能先防御再吃重击），且首领超高血量跑满周期。
    auto req = BuildRequest(/*bossHp=*/999999, /*bossAtk=*/1000, /*bossDef=*/0, /*bossRes=*/0, /*bossSpeed=*/2000, 77);
    req.mutable_players(0)->mutable_base_attributes()->set_speed(100);
    req.mutable_players(0)->set_max_health(50000);
    req.mutable_players(0)->mutable_base_attributes()->set_health(50000);
    req.mutable_players(0)->mutable_pets(0)->mutable_base_attributes()->set_speed(100);
    req.mutable_players(0)->mutable_pets(0)->set_max_health(50000);
    req.mutable_players(0)->mutable_pets(0)->mutable_base_attributes()->set_health(50000);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    const uint64_t bossId = FindBossId(engine);
    const uint64_t petActorId = FindPetActorId(engine);
    ASSERT_NE(0u, bossId);
    ASSERT_NE(0u, petActorId);

    // R1 开局无预告，首领普攻。
    const auto s1 = engine.BuildStateSnapshot();
    EXPECT_EQ(1u, s1.round_index());
    EXPECT_TRUE(s1.telegraphs().empty());
    ASSERT_EQ(kSuccess, Submit(engine, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
    ASSERT_EQ(kSuccess, Submit(engine, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
    const auto r1 = engine.ResolveCurrentRound();
    EXPECT_TRUE(r1.resolved_telegraphs().empty());

    // R2 操作窗口前已确定性预告 R3 重击目标；多次读取 BuildStateSnapshot（重连）不重抽。
    const auto s2a = engine.BuildStateSnapshot();
    const auto s2b = engine.BuildStateSnapshot();
    ASSERT_EQ(1, s2a.telegraphs_size());
    ASSERT_EQ(1, s2b.telegraphs_size());
    EXPECT_EQ(2u, s2a.round_index());
    EXPECT_EQ(2u, s2a.telegraphs(0).telegraph_round());
    EXPECT_EQ(3u, s2a.telegraphs(0).execute_round());
    EXPECT_EQ(turnbattle::kTelegraphKindFestivalHeavyStrike, s2a.telegraphs(0).intent_kind());
    EXPECT_EQ(turnbattle::kTelegraphStatusUpcoming, s2a.telegraphs(0).status());
    const uint64_t targetR3 = s2a.telegraphs(0).target_actor_id();
    EXPECT_TRUE(targetR3 == kHeroId || targetR3 == petActorId);
    EXPECT_EQ(targetR3, s2b.telegraphs(0).target_actor_id());
    EXPECT_EQ(s2a.telegraphs(0).state_version(), s2b.telegraphs(0).state_version());

    // R2 让被点名者防御（测试上轮防御绝不跨轮到 R3）。
    ASSERT_EQ(kSuccess, Submit(engine, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
    ASSERT_EQ(kSuccess, Submit(engine, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
    const auto r2 = engine.ResolveCurrentRound();
    const auto s3 = engine.BuildStateSnapshot();
    ASSERT_EQ(1, s3.telegraphs_size());
    EXPECT_EQ(3u, s3.round_index());
    EXPECT_EQ(turnbattle::kTelegraphStatusExecuting, s3.telegraphs(0).status());
    EXPECT_EQ(targetR3, s3.telegraphs(0).target_actor_id());

    // R3 不防御吃满 180% 重击，记录未防御重击伤害，并与同配置 R3 防御对比（应精确减半）。
    TurnBattleEngine defendedEngine(data);
    ASSERT_TRUE(defendedEngine.Initialize(req));
    // 推进 defendedEngine 到 R3 同步状态。
    ASSERT_EQ(kSuccess, Submit(defendedEngine, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
    ASSERT_EQ(kSuccess, Submit(defendedEngine, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
    defendedEngine.ResolveCurrentRound();
    ASSERT_EQ(kSuccess, Submit(defendedEngine, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
    ASSERT_EQ(kSuccess, Submit(defendedEngine, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
    defendedEngine.ResolveCurrentRound();

    // engine 在 R3 不防御（验证 R2 防御未跨轮）
    ASSERT_EQ(kSuccess, Submit(engine, kHeroId, MakeAction(BATTLE_ACTION_ATTACK, bossId)));
    ASSERT_EQ(kSuccess, Submit(engine, petActorId, MakeAction(BATTLE_ACTION_ATTACK, bossId)));
    const auto r3Undef = engine.ResolveCurrentRound();
    const uint64_t dmgUndef = DamageFromBossTo(r3Undef, bossId, targetR3);
    ASSERT_GT(dmgUndef, 0u);
    ASSERT_EQ(1, r3Undef.resolved_telegraphs_size());
    EXPECT_EQ(turnbattle::kTelegraphStatusExecuted, r3Undef.resolved_telegraphs(0).status());
    ASSERT_EQ(1, r3Undef.state().last_resolved_telegraphs_size());
    EXPECT_EQ(turnbattle::kTelegraphStatusExecuted, r3Undef.state().last_resolved_telegraphs(0).status());

    // defendedEngine 在 R3 由低速被点名者防御（尽管速度 100 << 首领 2000，慢速重击后置让防御必然生效）。
    ASSERT_EQ(kSuccess, Submit(defendedEngine, kHeroId, MakeAction(targetR3 == kHeroId ? BATTLE_ACTION_DEFEND : BATTLE_ACTION_ATTACK, bossId)));
    ASSERT_EQ(kSuccess, Submit(defendedEngine, petActorId, MakeAction(targetR3 == petActorId ? BATTLE_ACTION_DEFEND : BATTLE_ACTION_ATTACK, bossId)));
    const auto r3Def = defendedEngine.ResolveCurrentRound();
    const uint64_t dmgDef = DamageFromBossTo(r3Def, bossId, targetR3);
    EXPECT_NEAR(static_cast<double>(dmgDef), static_cast<double>(dmgUndef) * 0.5, 1.5);

    // 验证 R3 重击 = 普通攻击基础伤害的 180%（同目标无防御普攻 vs 无防御重击）。
    const auto* targetActor = ActorInState(s3, targetR3);
    ASSERT_NE(nullptr, targetActor);
    const double basicExpected = combatdamage::DamageBeforeCritical(
        turnbattle::kBasicAttackBaseDamage, 0, 1000, 1.0,
        targetActor->attributes().armor(), targetActor->defense(), targetActor->attributes().resistance(), targetActor->level());
    const uint64_t heavyExpected = combatdamage::DamageToHealth(basicExpected * 1.8, 50000);
    EXPECT_EQ(heavyExpected, dmgUndef);
}

TEST_F(FestivalBeastBattleTest, ControlCancelsHeavyWithoutFallbackSleepWakesAndAttackDebuffReducesDamage) {
    AddControlSkill(5001, 50010, BattleStateKind::Freeze, 2);
    AddControlSkill(5002, 50020, BattleStateKind::Sleep, 2);
    AddAttackDebuffSkill(5003, 50030, -0.40);

    // 场景 A：R3 冰冻控制首领 → 重击受控取消（status=4），无补普攻，R4 正常普攻不追补重击。
    {
        auto req = BuildRequest(999999, 1000, 0, 0, 500, 101);
        req.mutable_players(0)->add_skill_table_ids(5001);
        (*req.mutable_players(0)->mutable_skill_ranks())[5001] = 1;
        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(req));
        const uint64_t bossId = FindBossId(eng);
        const uint64_t petActorId = FindPetActorId(eng);

        for (int r = 1; r <= 2; ++r) {
            ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
            ASSERT_EQ(kSuccess, Submit(eng, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
            eng.ResolveCurrentRound();
        }
        const uint64_t targetR3 = eng.BuildStateSnapshot().telegraphs(0).target_actor_id();
        ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_SKILL, bossId, 5001)));
        ASSERT_EQ(kSuccess, Submit(eng, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
        const auto r3 = eng.ResolveCurrentRound();
        EXPECT_EQ(0u, DamageFromBossTo(r3, bossId, targetR3));
        EXPECT_EQ(0u, DamageFromBossTo(r3, bossId, kHeroId));
        EXPECT_EQ(0u, DamageFromBossTo(r3, bossId, petActorId));
        ASSERT_EQ(1, r3.resolved_telegraphs_size());
        EXPECT_EQ(turnbattle::kTelegraphStatusCancelledByControl, r3.resolved_telegraphs(0).status());
        // R4 不追补重击（R4 无预告）。
        EXPECT_TRUE(r3.state().telegraphs().empty());
    }

    // 场景 B：R2 昏睡首领，R3 玩家普攻打醒首领 → 遵循公共睡眠唤醒规则，R3 重击照常发动。
    {
        auto req = BuildRequest(999999, 1000, 0, 0, 500, 102);
        req.mutable_players(0)->add_skill_table_ids(5002);
        (*req.mutable_players(0)->mutable_skill_ranks())[5002] = 1;
        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(req));
        const uint64_t bossId = FindBossId(eng);
        const uint64_t petActorId = FindPetActorId(eng);

        ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
        ASSERT_EQ(kSuccess, Submit(eng, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
        eng.ResolveCurrentRound();

        ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_SKILL, bossId, 5002)));
        ASSERT_EQ(kSuccess, Submit(eng, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
        eng.ResolveCurrentRound();

        const uint64_t targetR3 = eng.BuildStateSnapshot().telegraphs(0).target_actor_id();
        ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_ATTACK, bossId)));
        ASSERT_EQ(kSuccess, Submit(eng, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
        const auto r3 = eng.ResolveCurrentRound();
        EXPECT_GT(DamageFromBossTo(r3, bossId, targetR3), 0u);
        ASSERT_EQ(1, r3.resolved_telegraphs_size());
        EXPECT_EQ(turnbattle::kTelegraphStatusExecuted, r3.resolved_telegraphs(0).status());
    }

    // 场景 C：减攻 Debuff 对首领重击生效。
    {
        auto req = BuildRequest(999999, 1000, 0, 0, 500, 103);
        req.mutable_players(0)->add_skill_table_ids(5003);
        (*req.mutable_players(0)->mutable_skill_ranks())[5003] = 1;
        TurnBattleEngine plain(data), debuffed(data);
        ASSERT_TRUE(plain.Initialize(req));
        ASSERT_TRUE(debuffed.Initialize(req));
        const uint64_t bossId = FindBossId(plain);
        const uint64_t petActorId = FindPetActorId(plain);
        for (int r = 1; r <= 2; ++r) {
            ASSERT_EQ(kSuccess, Submit(plain, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
            ASSERT_EQ(kSuccess, Submit(plain, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
            plain.ResolveCurrentRound();
            ASSERT_EQ(kSuccess, Submit(debuffed, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
            ASSERT_EQ(kSuccess, Submit(debuffed, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
            debuffed.ResolveCurrentRound();
        }
        const uint64_t targetR3 = plain.BuildStateSnapshot().telegraphs(0).target_actor_id();
        ASSERT_EQ(kSuccess, Submit(plain, kHeroId, MakeAction(BATTLE_ACTION_ATTACK, bossId)));
        ASSERT_EQ(kSuccess, Submit(plain, petActorId, MakeAction(BATTLE_ACTION_ATTACK, bossId)));
        const uint64_t plainDmg = DamageFromBossTo(plain.ResolveCurrentRound(), bossId, targetR3);

        ASSERT_EQ(kSuccess, Submit(debuffed, kHeroId, MakeAction(BATTLE_ACTION_SKILL, bossId, 5003)));
        ASSERT_EQ(kSuccess, Submit(debuffed, petActorId, MakeAction(BATTLE_ACTION_ATTACK, bossId)));
        const uint64_t debuffedDmg = DamageFromBossTo(debuffed.ResolveCurrentRound(), bossId, targetR3);
        EXPECT_LT(debuffedDmg, plainDmg);
    }
}

TEST_F(FestivalBeastBattleTest, TargetInvalidCancelsWithoutSwitchingFleeLeavesTogetherAndDisablesItemsAndDrops) {
    // 1. 禁用背包道具与捕捉。
    {
        auto req = BuildRequest(2000, 100, 100, 0, 200, 201);
        auto* item = req.mutable_players(0)->add_items();
        item->set_item_table_id(9001);
        item->set_count(3);
        req.mutable_players(0)->set_pet_capture_slots(1);
        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(req));
        const uint64_t bossId = FindBossId(eng);
        EXPECT_NE(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_ITEM, kHeroId, 0, 9001)));
        EXPECT_NE(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_CAPTURE, bossId)));

        // 击败首领后结算零常规掉落/经验/银两/击杀任务计数，且带出冻结 festival_beast 上下文。
        ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_ATTACK, bossId)));
        ASSERT_EQ(kSuccess, Submit(eng, FindPetActorId(eng), MakeAction(BATTLE_ACTION_ATTACK, bossId)));
        eng.ResolveCurrentRound();
        ASSERT_EQ(BATTLE_OUTCOME_SIDE_A_WIN, eng.Outcome());
        const auto settlement = eng.BuildSettlement(kHeroId);
        EXPECT_EQ(0u, settlement.exp_gain());
        EXPECT_EQ(0u, settlement.gold_gain());
        EXPECT_TRUE(settlement.currency_gains().empty());
        EXPECT_TRUE(settlement.items_gained().empty());
        EXPECT_TRUE(settlement.defeated_monsters().empty());
        EXPECT_TRUE(settlement.has_festival_beast());
        EXPECT_EQ(202601u, settlement.festival_beast().activity_instance_id());
        EXPECT_EQ("festival_beast_chunxishou_v1", settlement.festival_beast().reward_identity());
    }

    // 2. 目标在 R2 倒地且 R3 未复活 → 重击因目标失效取消（status=5），绝不偷偷换打另一方。
    {
        // 找到一个 R2 预告点名宝宝的种子。
        uint64_t petTargetSeed = 0;
        for (uint64_t s = 1; s <= 50; ++s) {
            auto probeReq = BuildRequest(999999, 1000, 0, 0, 500, s);
            probeReq.mutable_players(0)->set_max_health(50000);
            probeReq.mutable_players(0)->mutable_base_attributes()->set_health(50000);
            probeReq.mutable_players(0)->mutable_pets(0)->set_max_health(1);
            probeReq.mutable_players(0)->mutable_pets(0)->mutable_base_attributes()->set_health(1);
            TurnBattleEngine probe(data);
            ASSERT_TRUE(probe.Initialize(probeReq));
            const uint64_t petId = FindPetActorId(probe);
            ASSERT_EQ(kSuccess, Submit(probe, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
            ASSERT_EQ(kSuccess, Submit(probe, petId, MakeAction(BATTLE_ACTION_DEFEND)));
            probe.ResolveCurrentRound();
            const auto st2 = probe.BuildStateSnapshot();
            if (st2.telegraphs_size() == 1 && st2.telegraphs(0).target_actor_id() == petId &&
                ActorInState(st2, petId)->attributes().health() > 0) {
                petTargetSeed = s;
                break;
            }
        }
        ASSERT_NE(0u, petTargetSeed);
        // 构造宝宝 1 血，若在 R2 吃普攻倒地则 R3 目标失效取消，玩家受到 0 伤害。
        bool verifiedCancel = false;
        for (uint64_t s = 1; s <= 100 && !verifiedCancel; ++s) {
            auto req = BuildRequest(999999, 5000, 0, 0, 500, s);
            req.mutable_players(0)->set_max_health(99999);
            req.mutable_players(0)->mutable_base_attributes()->set_health(99999);
            req.mutable_players(0)->mutable_pets(0)->set_max_health(10);
            req.mutable_players(0)->mutable_pets(0)->mutable_base_attributes()->set_health(10);
            TurnBattleEngine eng(data);
            ASSERT_TRUE(eng.Initialize(req));
            const uint64_t bossId = FindBossId(eng);
            const uint64_t petId = FindPetActorId(eng);
            ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
            ASSERT_EQ(kSuccess, Submit(eng, petId, MakeAction(BATTLE_ACTION_DEFEND)));
            eng.ResolveCurrentRound();
            const auto st2 = eng.BuildStateSnapshot();
            if (st2.telegraphs_size() != 1 || st2.telegraphs(0).target_actor_id() != petId) continue;
            ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
            ASSERT_EQ(kSuccess, Submit(eng, petId, MakeAction(BATTLE_ACTION_DEFEND)));
            eng.ResolveCurrentRound();
            if (!ActorInState(eng.BuildStateSnapshot(), petId)->is_dead()) continue;
            ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_ATTACK, bossId)));
            const auto r3 = eng.ResolveCurrentRound();
            EXPECT_EQ(0u, DamageFromBossTo(r3, bossId, kHeroId));
            EXPECT_EQ(0u, DamageFromBossTo(r3, bossId, petId));
            ASSERT_EQ(1, r3.resolved_telegraphs_size());
            EXPECT_EQ(turnbattle::kTelegraphStatusCancelledTargetInvalid, r3.resolved_telegraphs(0).status());
            verifiedCancel = true;
        }
        EXPECT_TRUE(verifiedCancel);
    }

    // 3. 成功逃跑时本人与宝宝一并离场并结束战斗。
    {
        auto req = BuildRequest(999999, 100, 100, 0, 100, 303);
        req.mutable_players(0)->mutable_base_attributes()->set_speed(5000);
        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(req));
        const uint64_t petActorId = FindPetActorId(eng);
        bool fled = false;
        for (int i = 0; i < 6 && eng.Outcome() == BATTLE_OUTCOME_ONGOING; ++i) {
            ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_FLEE)));
            ASSERT_EQ(kSuccess, Submit(eng, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
            eng.ResolveCurrentRound();
            if (ActorInState(eng.BuildStateSnapshot(), kHeroId)->fled()) {
                fled = true;
                break;
            }
        }
        ASSERT_TRUE(fled);
        EXPECT_TRUE(ActorInState(eng.BuildStateSnapshot(), petActorId)->fled());
        EXPECT_EQ(BATTLE_OUTCOME_SIDE_B_WIN, eng.Outcome());
    }

    // 4. 打满 12 回合未击败首领 → 进攻方判负。
    {
        auto req = BuildRequest(999999, 10, 0, 0, 200, 404);
        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(req));
        const uint64_t petActorId = FindPetActorId(eng);
        uint32_t lastRound = 0;
        for (int r = 1; r <= 12; ++r) {
            ASSERT_EQ(BATTLE_OUTCOME_ONGOING, eng.Outcome());
            ASSERT_EQ(kSuccess, Submit(eng, kHeroId, MakeAction(BATTLE_ACTION_DEFEND)));
            ASSERT_EQ(kSuccess, Submit(eng, petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
            lastRound = eng.ResolveCurrentRound().round_index();
        }
        EXPECT_EQ(BATTLE_OUTCOME_SIDE_B_WIN, eng.Outcome());
        EXPECT_EQ(12u, lastRound);
    }
}

TEST_F(FestivalBeastBattleTest, FourArchetypesWinWithinTwelveRoundsAndSingleMistakeIsRecoverable) {
    // 使用 Tier 3（Lv30: boss_health=26000, atk=1100, def=1300, res=12, spd=760）验证四类流派与容错性。
    AddDamageSkill(6001, 60010, combatdamage::kPhysicalDamage, 2200.0);
    AddDamageSkill(6002, 60020, combatdamage::kMagicDamage, 2300.0);
    AddControlSkill(6003, 60030, BattleStateKind::Freeze, 1);
    AddHealSkill(6004, 60040, 1400.0);

    struct ArchetypeCase {
        std::string_view name;
        uint64_t hp;
        uint64_t phyAtk;
        uint64_t magAtk;
        uint64_t def;
        uint32_t skillId;
        bool isControl;
        bool isHeal;
    };
    const std::vector<ArchetypeCase> cases = {
        {"PhysicalDPS", 3500, 6750, 1200, 3720, 6001, false, false},
        {"MagicDPS",    3500, 1500, 5400, 3720, 6002, false, false},
        {"ControlSup",  5000, 1500, 2400, 5520, 6003, true,  false},
        {"HealerSup",   5000, 1500, 2400, 5520, 6004, false, true },
    };

    for (const auto& c : cases) {
        auto req = BuildRequest(26000, 1100, 1300, 12, 760, 505);
        auto* p = req.mutable_players(0);
        p->set_max_health(c.hp);
        p->mutable_base_attributes()->set_health(c.hp);
        p->set_physical_attack(c.phyAtk);
        p->set_magic_attack(c.magAtk);
        p->set_defense(c.def);
        p->add_skill_table_ids(c.skillId);
        (*p->mutable_skill_ranks())[c.skillId] = 5;

        // 普通 30 级输出宝宝
        auto* pet = p->mutable_pets(0);
        pet->set_max_health(3000);
        pet->mutable_base_attributes()->set_health(3000);
        pet->set_physical_attack(5600);
        pet->set_defense(2800);

        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(req)) << c.name;
        const uint64_t bossId = FindBossId(eng);
        const uint64_t petId = FindPetActorId(eng);
        bool firstHeavyMistakeSpent = false;
        uint32_t completedRounds = 0;

        while (eng.Outcome() == BATTLE_OUTCOME_ONGOING) {
            const auto st = eng.BuildStateSnapshot();
            uint64_t heavyTarget = 0;
            for (const auto& t : st.telegraphs()) {
                if (t.execute_round() == st.round_index()) heavyTarget = t.target_actor_id();
            }
            // 故意让第 1 次重击失误一次（不防御/不控场），验证一次失误不直接必败。
            const bool intentionalMistake = (heavyTarget != 0 && !firstHeavyMistakeSpent);
            if (intentionalMistake) firstHeavyMistakeSpent = true;

            BattleAction heroAct = MakeAction(BATTLE_ACTION_ATTACK, bossId);
            BattleAction petAct = MakeAction(BATTLE_ACTION_ATTACK, bossId);
            if (heavyTarget != 0 && !intentionalMistake) {
                if (c.isControl) {
                    heroAct = MakeAction(BATTLE_ACTION_SKILL, bossId, c.skillId);
                } else if (c.isHeal) {
                    heroAct = MakeAction(BATTLE_ACTION_SKILL, heavyTarget, c.skillId);
                    if (heavyTarget == petId) petAct = MakeAction(BATTLE_ACTION_DEFEND);
                } else {
                    if (heavyTarget == kHeroId) heroAct = MakeAction(BATTLE_ACTION_DEFEND);
                    else {
                        heroAct = MakeAction(BATTLE_ACTION_SKILL, bossId, c.skillId);
                        petAct = MakeAction(BATTLE_ACTION_DEFEND);
                    }
                }
            } else if (!c.isControl && !c.isHeal) {
                heroAct = MakeAction(BATTLE_ACTION_SKILL, bossId, c.skillId);
            } else if (c.isHeal && ActorInState(st, kHeroId)->attributes().health() < c.hp * 7 / 10) {
                heroAct = MakeAction(BATTLE_ACTION_SKILL, kHeroId, c.skillId);
            }

            if (!ActorInState(st, kHeroId)->is_dead()) {
                ASSERT_EQ(kSuccess, Submit(eng, kHeroId, heroAct)) << c.name;
            }
            if (!ActorInState(st, petId)->is_dead()) {
                ASSERT_EQ(kSuccess, Submit(eng, petId, petAct)) << c.name;
            }
            completedRounds = eng.ResolveCurrentRound().round_index();
        }
        EXPECT_EQ(BATTLE_OUTCOME_SIDE_A_WIN, eng.Outcome()) << c.name;
        EXPECT_LE(completedRounds, 12u) << c.name;
    }
}

}  // namespace
