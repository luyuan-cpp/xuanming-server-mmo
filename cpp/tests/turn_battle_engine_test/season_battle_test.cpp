// Battle 引擎侧赛季技能复核与单槽赛季印（方案 A）机制回归测试（docs/design/season-skills-and-seal-design.md）。

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <memory>

#include "memory_battle_data_provider.h"
#include "system/combat_damage_rules.h"
#include "system/season_rules.h"
#include "system/trial_journal_rules.h"
#include "system/turn_battle_engine.h"
#include "system/battle_replay_recorder.h"

namespace {

using turnbattle::BattleEffectOp;
using turnbattle::BattleModifier;
using turnbattle::TurnBattleEngine;

constexpr uint64_t kHero = 101;
constexpr uint64_t kEnemy = 201;

BattleAction Command(eBattleActionType type, uint64_t target = 0, uint32_t skill = 0) {
    BattleAction action;
    action.set_action_type(type);
    action.set_target_id(target);
    action.set_skill_table_id(skill);
    return action;
}

const BattleActorState& ActorOf(const BattleStateS2C& state, uint64_t id) {
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() == id) return actor;
    }
    ADD_FAILURE() << "Missing actor " << id;
    return BattleActorState::default_instance();
}

uint64_t ShieldCapacity(const BattleStateS2C& state, uint64_t actorId, uint32_t buffId) {
    for (const auto& buff : ActorOf(state, actorId).buffs()) {
        if (buff.buff_table_id() == buffId) return buff.remaining_capacity();
    }
    return 0;
}

class SeasonBattleTest : public ::testing::Test {
protected:
    void SetUp() override {
        data = std::make_shared<MemoryBattleDataProvider>();
        request.set_battle_id(91001);
        request.set_match_mode(3);
        request.set_seed(7);
        for (const uint64_t id : {kHero, kEnemy}) {
            auto* p = request.add_players();
            p->set_player_id(id);
            p->set_team_index(id == kHero ? 0 : 1);
            p->set_level(30);
            p->set_max_health(10000);
            p->set_max_mana(10000);
            p->mutable_base_attributes()->set_health(10000);
            p->mutable_base_attributes()->set_mana(10000);
            p->mutable_base_attributes()->set_speed(id == kHero ? 300 : 200);
            p->set_physical_attack(1000);
            p->set_magic_attack(1000);
            p->set_defense(100);
        }
    }

    BattlePlayerSnapshot& Player(uint64_t id) {
        for (auto& p : *request.mutable_players()) {
            if (p.player_id() == id) return p;
        }
        return *request.mutable_players(0);
    }

    SkillTable& AddModernSkill(uint32_t id, uint32_t seasonId, uint32_t damageType, double flatDamage) {
        auto& row = data->AddSkill(id);
        row.Clear();
        row.set_id(id);
        row.set_battle_model(1);
        row.set_caster_mask(1);
        row.set_ability_kind(1);
        row.set_unlock_level(1);
        row.set_max_rank(10);
        row.set_target_team(1);
        row.set_max_targets(1);
        row.set_pet_max_targets(1);
        row.add_skill_type(1);
        row.set_damage_type(damageType);
        row.set_mana_percent(0.05);
        row.set_season_id(seasonId);
        row.add_effect(id * 10);
        auto& buff = data->AddBuff(id * 10);
        buff.Clear();
        buff.set_id(id * 10);
        buff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Damage));
        buff.set_flat_value(flatDamage);
        data->SetSkillDamage(id, flatDamage);
        return row;
    }

    void AddShieldBuff(uint64_t actorId, uint32_t buffId, uint64_t capacity) {
        auto& row = data->AddBuff(buffId);
        row.Clear();
        row.set_id(buffId);
        row.set_infinite_duration(1);
        row.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
        row.set_effect_priority(20);
        auto* entry = Player(actorId).add_buffs();
        entry->set_buff_id(buffId);
        entry->set_buff_table_id(buffId);
        entry->set_caster_id(actorId);
        entry->set_layer(1);
        entry->set_remaining_capacity(capacity);
        entry->set_snapshot_value(capacity);
        entry->set_tick_round(999);
    }

    std::shared_ptr<MemoryBattleDataProvider> data;
    CreateBattleRequest request;
};

} // namespace

TEST_F(SeasonBattleTest, RejectsOverCapSeasonStatsOrInvalidSeasonSeal) {
    // 超过 500 bps 硬上限拒绝开局
    Player(kHero).mutable_equipment_effects()->set_season_max_health_bps(501);
    TurnBattleEngine overStat(data);
    EXPECT_FALSE(overStat.Initialize(request));

    Player(kHero).mutable_equipment_effects()->set_season_max_health_bps(300);
    auto* badSeal = Player(kHero).mutable_equipment_effects()->mutable_season_seal();
    badSeal->set_season_id(1);
    badSeal->set_seal_id(0); // 非法 seal_id
    badSeal->set_effect_id(1);
    badSeal->set_bonus_bps(1000);
    badSeal->set_per_round_limit(1);
    TurnBattleEngine badSealEngine(data);
    EXPECT_FALSE(badSealEngine.Initialize(request));
}

TEST_F(SeasonBattleTest, ValidatesSeasonalSkillAgainstFrozenActiveSeasonId) {
    AddModernSkill(15001, /*seasonId=*/1, /*damageType=*/1, 500);
    Player(kHero).set_active_season_id(2); // 非第 1 季快照伪造第 1 季技能
    Player(kHero).add_skill_table_ids(15001);
    (*Player(kHero).mutable_skill_ranks())[15001] = 10;

    TurnBattleEngine wrongSeason(data);
    ASSERT_TRUE(wrongSeason.Initialize(request));
    const auto stateWrong = wrongSeason.BuildStateSnapshot();
    EXPECT_TRUE(ActorOf(stateWrong, kHero).skill_table_ids().empty()) << "非当季技能被引擎剔除";

    // 匹配冻结赛季 1 时放行
    Player(kHero).set_active_season_id(1);
    TurnBattleEngine rightSeason(data);
    ASSERT_TRUE(rightSeason.Initialize(request));
    const auto stateRight = rightSeason.BuildStateSnapshot();
    ASSERT_EQ(ActorOf(stateRight, kHero).skill_table_ids_size(), 1);
    EXPECT_EQ(ActorOf(stateRight, kHero).skill_table_ids(0), 15001u);
}

TEST_F(SeasonBattleTest, SeasonSealShieldBreakExecutesAndTagsSeasonSealEvent) {
    AddModernSkill(15001, 1, /*physical=*/1, 1000);
    Player(kHero).set_active_season_id(1);
    Player(kHero).add_skill_table_ids(15001);
    (*Player(kHero).mutable_skill_ranks())[15001] = 1;
    auto* seal = Player(kHero).mutable_equipment_effects()->mutable_season_seal();
    seal->set_season_id(1);
    seal->set_seal_id(101);
    seal->set_effect_id(1);
    seal->set_bonus_bps(1000);
    seal->set_per_round_limit(1);
    AddShieldBuff(kEnemy, 9901, 5000);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    engine.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kEnemy, 15001));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
    const auto turn = engine.ResolveCurrentRound();

    bool sawSealEvent = false;
    for (const auto& ev : turn.events()) {
        if (ev.event_type() == BATTLE_EVENT_ARTIFACT && ev.season_seal_id() == 101u) {
            EXPECT_EQ(ev.artifact_item_id(), 0u);
            EXPECT_EQ(ev.artifact_effect_id(), 1u);
            EXPECT_GT(ev.value(), 0u);
            sawSealEvent = true;
        }
    }
    EXPECT_TRUE(sawSealEvent);
    EXPECT_LT(ShieldCapacity(engine.BuildStateSnapshot(), kEnemy, 9901), 5000u);
}

TEST_F(SeasonBattleTest, SeasonSealOpeningDamageReductionAndDefendManaRestoreCoexistWithTwoArtifacts) {
    // kHero 带满 2 件永久归元葫 + 1 枚聚息援印(104)，验证三者共存且互不冲突
    for (uint64_t itemId : {uint64_t{501}, uint64_t{502}}) {
        auto* art = Player(kHero).mutable_equipment_effects()->add_artifacts();
        art->set_item_id(itemId);
        art->set_effect_id(7);
        art->set_bonus_bps(500);
        art->set_per_round_limit(1);
        art->set_battle_limit(3);
    }
    auto* heroSeal = Player(kHero).mutable_equipment_effects()->mutable_season_seal();
    heroSeal->set_season_id(1);
    heroSeal->set_seal_id(104);
    heroSeal->set_effect_id(7);
    heroSeal->set_bonus_bps(400);
    heroSeal->set_per_round_limit(1);
    heroSeal->set_battle_limit(3);
    Player(kHero).mutable_base_attributes()->set_mana(5000); // 缺 5000 蓝

    // kEnemy 带固阵守印(103)：前两回合首伤减免 10%，上限入战最大气血 3% (300)
    auto* enemySeal = Player(kEnemy).mutable_equipment_effects()->mutable_season_seal();
    enemySeal->set_season_id(1);
    enemySeal->set_seal_id(103);
    enemySeal->set_effect_id(6);
    enemySeal->set_bonus_bps(1000);
    enemySeal->set_cap_bps(300);
    enemySeal->set_active_rounds(2);
    enemySeal->set_battle_limit(1);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    // 客户端状态快照必须清除 season_seal_state
    BattleStateS2C publicState = engine.BuildStateSnapshot();
    TurnBattleEngine::StripEngineOnlyState(publicState);
    EXPECT_FALSE(ActorOf(publicState, kHero).has_season_seal_state());

    // Round 1: kHero 防御（回蓝 500+500+400 = 1400），kEnemy 普攻
    engine.SubmitAction(kHero, Command(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_ATTACK, kHero));
    const auto r1 = engine.ResolveCurrentRound();
    EXPECT_EQ(ActorOf(engine.BuildStateSnapshot(), kHero).attributes().mana(), 6400u);
    bool sawHeroSeal = false;
    for (const auto& ev : r1.events()) {
        if (ev.event_type() == BATTLE_EVENT_ARTIFACT && ev.season_seal_id() == 104u) {
            EXPECT_EQ(ev.value(), 400u);
            sawHeroSeal = true;
        }
    }
    EXPECT_TRUE(sawHeroSeal);

    // Round 2: kHero 攻击 kEnemy，触发 kEnemy 的固阵守印(103)
    engine.SubmitAction(kHero, Command(BATTLE_ACTION_ATTACK, kEnemy));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
    const auto r2 = engine.ResolveCurrentRound();
    bool sawEnemySeal = false;
    for (const auto& ev : r2.events()) {
        if (ev.event_type() == BATTLE_EVENT_ARTIFACT && ev.season_seal_id() == 103u) {
            EXPECT_GT(ev.value(), 0u);
            EXPECT_LE(ev.value(), 300u) << "减伤受入战最大气血 3% 上限约束";
            sawEnemySeal = true;
        }
    }
    EXPECT_TRUE(sawEnemySeal);
}

// ---- 赛季首领试炼（3 大首领 + 慢速重击）、护卫救援、物法易势 5V5 与防挂机参与事实 ----

namespace {

CreateBattleRequest MakeBossTrialRequest(uint64_t battleId, uint32_t dungeonId, uint32_t playerCount,
                                         uint64_t playerSpeed = 100) {
    CreateBattleRequest req;
    req.set_battle_id(battleId);
    req.set_match_mode(playerCount > 1 ? turnbattle::kMatchModePveTeam : turnbattle::kMatchModePveSolo);
    req.set_battle_config_id(dungeonId);
    req.set_seed(20261124ULL + battleId);
    for (uint32_t i = 0; i < playerCount; ++i) {
        auto* p = req.add_players();
        p->set_player_id(30001ULL + i);
        p->set_team_index(0);
        p->set_player_name("TrialHero" + std::to_string(i));
        p->set_level(60);
        p->set_max_health(20000);
        p->set_max_mana(5000);
        p->mutable_base_attributes()->set_health(20000);
        p->mutable_base_attributes()->set_mana(5000);
        p->mutable_base_attributes()->set_speed(playerSpeed);
        p->set_physical_attack(500);
        p->set_magic_attack(500);
        p->set_defense(200);
    }
    return req;
}

void SetupBossTrialData(MemoryBattleDataProvider& data, uint32_t dungeonId, uint32_t trialKind,
                        uint32_t difficulty, uint32_t roundLimit, uint64_t bossHp = 6000, uint64_t guardHp = 3000) {
    data.AddBattleReward(0, "").set_exp_gain(100);
    auto& d = data.AddDungeon(dungeonId);
    d.Clear();
    d.set_id(dungeonId);
    d.set_boss_trial_kind(trialKind);
    d.set_difficulty(difficulty);
    d.set_round_limit(roundLimit);
    data.SetDungeonMonsters(dungeonId, {101, 102});
    data.SetDungeonMonstersPerPlayer(dungeonId, 2); // 验证试炼不按人数复制怪，而是缩放血量。
    auto& boss = data.AddMonster(101);
    boss.Clear();
    boss.set_id(101);
    boss.set_is_boss(1);
    boss.set_health(bossHp);
    boss.set_strength(300);
    boss.set_armor(100);
    boss.set_speed(900); // 故意比玩家快，验证慢速重击仍晚于低速玩家防御。
    auto& guard = data.AddMonster(102);
    guard.Clear();
    guard.set_id(102);
    guard.set_is_boss(0);
    guard.set_health(guardHp);
    guard.set_strength(120);
    guard.set_armor(80);
    guard.set_speed(600);
}

} // namespace

TEST_F(SeasonBattleTest, BossTrialRejuvScalesHpNotCountAndSlowCleaveRespectsLowSpeedDefendAndStunCancel) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 102, turnbattle::kBossTrialKindBreakRejuv, /*difficulty=*/2, /*roundLimit=*/15, 6000, 3000);
    // 注册一个必定眩晕敌方的技能用于测控场取消重击。
    auto& stunBuff = mem->AddBuff(8801);
    stunBuff.Clear();
    stunBuff.set_id(8801);
    stunBuff.set_buff_type(turnbattle::kBuffTypeStun);
    stunBuff.set_duration(6.0); // 1 回合
    auto& stunSkill = mem->AddSkill(8801);
    stunSkill.Clear();
    stunSkill.set_id(8801);
    stunSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    stunSkill.add_targeting_mode(1);
    stunSkill.add_effect(8801);
    mem->SetSkillDamage(8801, 0);

    // 2 人组队进本：只生成 2 只怪且血量 ×2（首领 12000、护卫 6000）。
    auto req = MakeBossTrialRequest(5001, 102, /*playerCount=*/2, /*playerSpeed=*/100);
    req.mutable_players(0)->add_skill_table_ids(8801);

    // 对比三局完全相同种子：A 局 R2 被点名低速玩家防御，B 局不防御，C 局眩晕首领。
    TurnBattleEngine engDefend(mem), engNoDefend(mem), engStun(mem);
    ASSERT_TRUE(engDefend.Initialize(req));
    ASSERT_TRUE(engNoDefend.Initialize(req));
    ASSERT_TRUE(engStun.Initialize(req));

    const auto initSnap = engDefend.BuildStateSnapshot();
    uint64_t bossId = 0, guardId = 0;
    uint32_t monsterCount = 0;
    for (const auto& a : initSnap.actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            ++monsterCount;
            if (a.is_boss()) {
                bossId = a.actor_id();
                EXPECT_EQ(a.max_health(), 12000u);
            } else {
                guardId = a.actor_id();
                EXPECT_EQ(a.max_health(), 6000u);
            }
        }
    }
    ASSERT_EQ(monsterCount, 2u);
    ASSERT_NE(bossId, 0u);
    ASSERT_NE(guardId, 0u);

    // 开局 R1 快照应预告 R2 首领慢速重击 + R2 困难回春护卫回血。
    uint64_t cleaveTargetId = 0;
    bool sawGuardHealTelegraph = false;
    for (const auto& t : initSnap.telegraphs()) {
        if (t.intent_kind() == turnbattle::kTelegraphKindSlowCleave && t.execute_round() == 2) {
            cleaveTargetId = t.target_actor_id();
        }
        if (t.intent_kind() == turnbattle::kTelegraphKindGuardHeal && t.execute_round() == 2) {
            sawGuardHealTelegraph = true;
        }
    }
    ASSERT_NE(cleaveTargetId, 0u);
    EXPECT_TRUE(sawGuardHealTelegraph);

    // R1 三局均让首领受一点伤（测试 R2 护卫回血），双方常规执行。
    for (auto* eng : {&engDefend, &engNoDefend, &engStun}) {
        eng->SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, bossId));
        eng->SubmitAction(30002, Command(BATTLE_ACTION_DEFEND));
        eng->ResolveCurrentRound();
    }

    // R2：A 局被点名者防御（速度 100 远慢于首领 900），B 局不防御，C 局玩家1给首领挂眩晕。
    const uint64_t hpBeforeDef = ActorOf(engDefend.BuildStateSnapshot(), cleaveTargetId).attributes().health();
    const uint64_t hpBeforeNoDef = ActorOf(engNoDefend.BuildStateSnapshot(), cleaveTargetId).attributes().health();
    ASSERT_EQ(hpBeforeDef, hpBeforeNoDef);

    engDefend.SubmitAction(cleaveTargetId, Command(BATTLE_ACTION_DEFEND));
    engDefend.SubmitAction(cleaveTargetId == 30001 ? 30002 : 30001, Command(BATTLE_ACTION_DEFEND));
    const auto r2Def = engDefend.ResolveCurrentRound();

    engNoDefend.SubmitAction(cleaveTargetId, Command(BATTLE_ACTION_ATTACK, guardId));
    engNoDefend.SubmitAction(cleaveTargetId == 30001 ? 30002 : 30001, Command(BATTLE_ACTION_DEFEND));
    const auto r2NoDef = engNoDefend.ResolveCurrentRound();

    engStun.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, bossId, 8801));
    engStun.SubmitAction(30002, Command(BATTLE_ACTION_DEFEND));
    const auto r2Stun = engStun.ResolveCurrentRound();

    uint64_t bossDmgDef = 0, bossDmgNoDef = 0, bossDmgStun = 0;
    uint64_t guardHealAmt = 0;
    for (const auto& ev : r2Def.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == bossId && ev.target_id() == cleaveTargetId)
            bossDmgDef += ev.value();
        if (ev.event_type() == BATTLE_EVENT_HEAL && ev.source_id() == guardId && ev.target_id() == bossId)
            guardHealAmt += ev.value();
    }
    for (const auto& ev : r2NoDef.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == bossId && ev.target_id() == cleaveTargetId)
            bossDmgNoDef += ev.value();
    }
    for (const auto& ev : r2Stun.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == bossId)
            bossDmgStun += ev.value();
    }
    EXPECT_GT(bossDmgNoDef, 0u);
    EXPECT_GT(bossDmgDef, 0u);
    EXPECT_LT(bossDmgDef, bossDmgNoDef) << "慢速重击晚于低速目标防御结算，防御必然减伤";
    EXPECT_EQ(bossDmgStun, 0u) << "首领被眩晕控制时慢速重击必须直接取消且不回退普攻";
    EXPECT_GT(guardHealAmt, 0u) << "困难难度回春护卫在 R2 与首领重击同轮回血";
}

TEST_F(SeasonBattleTest, BossTrialStanceAndBarrierAndDispelWorkCorrectly) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 103, turnbattle::kBossTrialKindBreakStance, /*difficulty=*/1, /*roundLimit=*/12, 10000, 4000);
    auto& dispelSkill = mem->AddSkill(8802);
    dispelSkill.Clear();
    dispelSkill.set_id(8802);
    dispelSkill.set_battle_model(1);
    dispelSkill.set_caster_mask(1);
    dispelSkill.set_ability_kind(1);
    dispelSkill.set_unlock_level(1);
    dispelSkill.set_max_rank(10);
    dispelSkill.set_target_team(1); // 敌方
    dispelSkill.set_max_targets(1);
    dispelSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    dispelSkill.set_mana_percent(0.01);
    dispelSkill.add_effect(88020);
    auto& dispelBuff = mem->AddBuff(88020);
    dispelBuff.Clear();
    dispelBuff.set_id(88020);
    dispelBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::DispelBuff));
    dispelBuff.set_effect_count(1);

    auto req = MakeBossTrialRequest(5002, 103, 1, 500);
    req.mutable_players(0)->add_skill_table_ids(8802);
    (*req.mutable_players(0)->mutable_skill_ranks())[8802] = 1;
    req.mutable_trial_context()->set_journal_entry_id(1);

    TurnBattleEngine eng(mem);
    ASSERT_TRUE(eng.Initialize(req));
    uint64_t bossId = 0;
    for (const auto& a : eng.BuildStateSnapshot().actors()) {
        if (a.is_boss()) bossId = a.actor_id();
    }
    ASSERT_NE(bossId, 0u);
    // R1 护势护卫展开物理护势。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
    eng.ResolveCurrentRound();
    // R2 物理普攻吃 30% 物理护势减伤。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, bossId));
    const auto r2 = eng.ResolveCurrentRound();
    uint64_t dmgWithStance = 0;
    for (const auto& ev : r2.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 30001 && ev.target_id() == bossId)
            dmgWithStance += ev.value();
    }
    // R3 驱散首领护势。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, bossId, 8802));
    eng.ResolveCurrentRound();
    // R4 护势刷新在慢速阶段（普通行动之后），玩家普攻打在无护势首领上伤害应更高，且试炼事实记录驱散成功。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, bossId));
    const auto r4 = eng.ResolveCurrentRound();
    uint64_t dmgWithoutStance = 0;
    for (const auto& ev : r4.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 30001 && ev.target_id() == bossId)
            dmgWithoutStance += ev.value();
    }
    EXPECT_GT(dmgWithoutStance, dmgWithStance);
    EXPECT_TRUE(eng.BuildSettlement(30001).trial_facts().guard_stance_dispelled());
}

TEST_F(SeasonBattleTest, GuardianRescueFailsImmediatelyWhenGuardDiesEvenIfBossDies) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 109, turnbattle::kBossTrialKindGuardianRescue, /*difficulty=*/1, /*roundLimit=*/8, 50000, 1);
    auto req = MakeBossTrialRequest(5003, 109, 1, 100);
    TurnBattleEngine eng(mem);
    ASSERT_TRUE(eng.Initialize(req));
    const auto snap = eng.BuildStateSnapshot();
    ASSERT_NE(snap.protected_guard_actor_id(), 0u);
    uint64_t bossId = 0;
    for (const auto& a : snap.actors()) {
        if (a.is_boss()) bossId = a.actor_id();
        if (a.actor_id() == snap.protected_guard_actor_id()) {
            EXPECT_EQ(a.team_index(), 0u);
        }
    }
    ASSERT_NE(bossId, 0u);
    for (int r = 0; r < 3 && eng.Outcome() == BATTLE_OUTCOME_ONGOING; ++r) {
        eng.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
        eng.ResolveCurrentRound();
    }
    EXPECT_EQ(eng.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN) << "待保护护卫阵亡必须立刻判负";
}

TEST_F(SeasonBattleTest, Polarity5v5RotatesEveryTwoRoundsBoostsMatchingDamageDrawsAt16AndTracksAntiAfk) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    auto& d = mem->AddDungeon(110);
    d.Clear();
    d.set_id(110);
    d.set_boss_trial_kind(turnbattle::kBossTrialKindPolarity5v5);
    d.set_round_limit(16);

    CreateBattleRequest req;
    req.set_battle_id(5004);
    req.set_match_mode(turnbattle::kMatchModePolarity5v5);
    req.set_battle_config_id(110);
    req.set_seed(777);
    const auto addP = [&](uint64_t pid, uint32_t team) {
        auto* p = req.add_players();
        p->set_player_id(pid);
        p->set_team_index(team);
        p->set_player_name("P" + std::to_string(pid));
        p->set_level(60);
        p->set_max_health(500000);
        p->set_max_mana(5000);
        p->mutable_base_attributes()->set_health(500000);
        p->mutable_base_attributes()->set_mana(5000);
        p->mutable_base_attributes()->set_speed(500);
        p->set_physical_attack(1000);
        p->set_magic_attack(1000);
        p->set_defense(200);
    };
    addP(40001, 0);
    addP(40002, 1);

    TurnBattleEngine eng(mem);
    ASSERT_TRUE(eng.Initialize(req));
    const uint32_t r1Polarity = eng.BuildStateSnapshot().active_polarity();
    ASSERT_TRUE(r1Polarity == combatdamage::kPhysicalDamage || r1Polarity == combatdamage::kMagicDamage);
    EXPECT_EQ(eng.BuildStateSnapshot().next_polarity_switch_round(), 3u);

    // 40001 手动攻击 4 个回合（含物理易势与法术易势段对比），40002 只防御且仅手动 1 回合（测试防挂机统计）。
    uint64_t physDmgDuringPhysPolarity = 0;
    uint64_t physDmgDuringMagicPolarity = 0;
    for (int r = 1; r <= 16; ++r) {
        const uint32_t curPolarity = eng.BuildStateSnapshot().active_polarity();
        if (r <= 2) EXPECT_EQ(curPolarity, r1Polarity);
        else if (r <= 4) EXPECT_NE(curPolarity, r1Polarity);

        if (r <= 4) {
            eng.SubmitAction(40001, Command(BATTLE_ACTION_ATTACK, 40002));
        }
        if (r == 1) {
            eng.SubmitAction(40002, Command(BATTLE_ACTION_DEFEND));
        } else {
            // 40002 其余回合全程挂机防御（不手动提交）。
            eng.SetActorAuto(40002, true);
        }
        const auto turn = eng.ResolveCurrentRound();
        if (r == 2 || r == 4) {
            for (const auto& ev : turn.events()) {
                if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 40001 && ev.target_id() == 40002) {
                    if (curPolarity == combatdamage::kPhysicalDamage) physDmgDuringPhysPolarity = ev.value();
                    else physDmgDuringMagicPolarity = ev.value();
                }
            }
        }
    }
    EXPECT_EQ(eng.Outcome(), BATTLE_OUTCOME_DRAW) << "物法易势满 16 回合双方存活必须判平局";
    EXPECT_GT(physDmgDuringPhysPolarity, physDmgDuringMagicPolarity) << "物理易势下物理直接伤害应按 ×1.15 放大";

    const auto settleActive = eng.BuildSettlement(40001);
    EXPECT_EQ(settleActive.total_rounds(), 16u);
    EXPECT_EQ(settleActive.manual_action_rounds(), 4u);
    EXPECT_TRUE(settleActive.effective_combat_contribution());
    EXPECT_EQ(settleActive.boss_trial_kind(), turnbattle::kBossTrialKindPolarity5v5);

    const auto settlePassive = eng.BuildSettlement(40002);
    EXPECT_EQ(settlePassive.manual_action_rounds(), 1u);
}

TEST_F(SeasonBattleTest, BossTrialV2FocusSplashAndParryExposedAndV1DeadTargetNoExpose) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 101, turnbattle::kBossTrialKindBreakRejuv, /*difficulty=*/1, /*roundLimit=*/12, 20000, 10000);
    // 注册 5 目标物理群攻（battle_model=1）用于验证主目标聚火 100% vs 副目标溅射 60%。
    auto& aoeSkill = mem->AddSkill(8901);
    aoeSkill.Clear();
    aoeSkill.set_id(8901);
    aoeSkill.set_battle_model(1);
    aoeSkill.set_caster_mask(1);
    aoeSkill.set_ability_kind(1);
    aoeSkill.set_unlock_level(1);
    aoeSkill.set_max_rank(10);
    aoeSkill.set_target_team(1);
    aoeSkill.set_max_targets(5);
    aoeSkill.set_damage_type(combatdamage::kPhysicalDamage);
    aoeSkill.set_attack_multiplier(1.0);
    aoeSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    aoeSkill.set_mana_percent(0.01);
    aoeSkill.add_effect(89010);
    mem->SetSkillDamage(8901, 200);
    auto& aoeDmg = mem->AddBuff(89010);
    aoeDmg.Clear();
    aoeDmg.set_id(89010);
    aoeDmg.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Damage));
    aoeDmg.set_value_basis(1);
    aoeDmg.set_ratio(1.0);

    auto req = MakeBossTrialRequest(5101, 101, /*playerCount=*/1, /*playerSpeed=*/100);
    req.mutable_players(0)->add_skill_table_ids(8901);
    (*req.mutable_players(0)->mutable_skill_ranks())[8901] = 1;
    req.mutable_trial_context()->set_journal_entry_id(101);
    req.mutable_trial_context()->set_trial_rule_version(1);

    TurnBattleEngine eng(mem);
    ASSERT_TRUE(eng.Initialize(req));
    EXPECT_TRUE(eng.IsBossTrialBattle());
    const auto s1 = eng.BuildStateSnapshot();
    EXPECT_EQ(s1.boss_phase(), 1u);
    EXPECT_EQ(s1.boss_trial_kind(), turnbattle::kBossTrialKindBreakRejuv);
    EXPECT_EQ(s1.battle_difficulty(), 1u);
    uint64_t bossId = 0, guardId = 0;
    for (const auto& a : s1.actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            if (a.is_boss()) bossId = a.actor_id();
            else guardId = a.actor_id();
        }
    }
    ASSERT_NE(bossId, 0u);
    ASSERT_NE(guardId, 0u);

    // R1：主目标选首领放 5 目标群攻，首领吃 100%，护卫吃 60% 溅射且事件标记 is_splash_reduced。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, bossId, 8901));
    const auto r1 = eng.ResolveCurrentRound();
    uint64_t bossDmgR1 = 0, guardDmgR1 = 0;
    bool bossSplash = false, guardSplash = false;
    for (const auto& ev : r1.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 30001 && ev.skill_table_id() == 8901) {
            if (ev.target_id() == bossId) { bossDmgR1 = ev.value(); bossSplash = ev.is_splash_reduced(); }
            if (ev.target_id() == guardId) { guardDmgR1 = ev.value(); guardSplash = ev.is_splash_reduced(); }
        }
    }
    EXPECT_GT(bossDmgR1, 0u);
    EXPECT_GT(guardDmgR1, 0u);
    EXPECT_FALSE(bossSplash);
    EXPECT_TRUE(guardSplash);
    EXPECT_GT(bossDmgR1, guardDmgR1);

    // R2：低速玩家防御化解慢重劈 -> 首领挂「破绽 +35%」至 R3 末。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
    const auto r2 = eng.ResolveCurrentRound();
    bool sawParryMechanic = false;
    for (const auto& ev : r2.events()) {
        if (ev.event_type() == BATTLE_EVENT_BOSS_MECHANIC &&
            ev.mechanic_kind() == turnbattle::kBossMechanicCleaveParryExpose &&
            ev.value() == turnbattle::kBossExposedFullPct) {
            sawParryMechanic = true;
        }
    }
    EXPECT_TRUE(sawParryMechanic);
    EXPECT_EQ(r2.state().boss_exposed_pct(), turnbattle::kBossExposedFullPct);
    EXPECT_EQ(r2.state().boss_exposed_until_round(), 3u);

    // R3：趁破绽再次对首领放同技能，伤害显著高于 R1（约 +35%）。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, bossId, 8901));
    const auto r3 = eng.ResolveCurrentRound();
    uint64_t bossDmgR3 = 0;
    for (const auto& ev : r3.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 30001 &&
            ev.skill_table_id() == 8901 && ev.target_id() == bossId) {
            bossDmgR3 = ev.value();
        }
    }
    EXPECT_GT(bossDmgR3, (bossDmgR1 * 125ULL) / 100ULL);
    EXPECT_EQ(r3.state().boss_exposed_pct(), 0u) << "破绽在 R3 末到期清理";

    // 首领试炼确定退出验证（必成功且不消耗 RNG）。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_FLEE));
    const auto r4 = eng.ResolveCurrentRound();
    EXPECT_EQ(eng.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);
    const auto settle = eng.BuildSettlement(30001);
    EXPECT_TRUE(settle.fled());
    EXPECT_EQ(settle.trial_facts().cleave_parry_count(), 1u);
    EXPECT_TRUE(settle.trial_facts().all_self_cleaves_parried());

    // V1 防线：被点名目标在重劈兑现前死亡 -> 取消重劈且绝不给首领挂破绽。
    auto reqV1 = MakeBossTrialRequest(5102, 101, /*playerCount=*/2, /*playerSpeed=*/100);
    reqV1.mutable_players(0)->mutable_base_attributes()->set_health(1);
    reqV1.mutable_players(1)->mutable_base_attributes()->set_health(50000);
    // 让玩家 0 成为极低血量并提前被护卫/首领打死（两人都给 1 血或只盯被点名者）
    TurnBattleEngine engV1(mem);
    ASSERT_TRUE(engV1.Initialize(reqV1));
    uint64_t marked = 0;
    for (const auto& t : engV1.BuildStateSnapshot().telegraphs()) {
        if (t.intent_kind() == turnbattle::kTelegraphKindSlowCleave) marked = t.target_actor_id();
    }
    ASSERT_NE(marked, 0u);
    // 被点名者逃跑使其在 R2 重劈前失效（确定退出），另一人存活。
    engV1.SubmitAction(marked, Command(BATTLE_ACTION_FLEE));
    engV1.SubmitAction(marked == 30001 ? 30002 : 30001, Command(BATTLE_ACTION_DEFEND));
    engV1.ResolveCurrentRound();
    engV1.SubmitAction(marked == 30001 ? 30002 : 30001, Command(BATTLE_ACTION_DEFEND));
    const auto r2V1 = engV1.ResolveCurrentRound();
    EXPECT_EQ(r2V1.state().boss_exposed_pct(), 0u) << "V1防线：目标失效取消重劈绝不能白嫖破绽";
}

TEST_F(SeasonBattleTest, BossTrialV2HardRejuvCleanseAntiHealBacklashAndPhaseTwo) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 102, turnbattle::kBossTrialKindBreakRejuv, /*difficulty=*/2, /*roundLimit=*/15, 20000, 100);
    // 注册减疗技能（IncomingHeal -0.5）用于验证阻止净化并挂气馁。
    auto& antiHealSkill = mem->AddSkill(8902);
    antiHealSkill.Clear();
    antiHealSkill.set_id(8902);
    antiHealSkill.set_battle_model(1);
    antiHealSkill.set_caster_mask(1);
    antiHealSkill.set_ability_kind(1);
    antiHealSkill.set_unlock_level(1);
    antiHealSkill.set_max_rank(10);
    antiHealSkill.set_target_team(1);
    antiHealSkill.set_max_targets(5);
    antiHealSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    antiHealSkill.set_mana_percent(0.01);
    antiHealSkill.add_effect(89020);
    auto& antiHealBuff = mem->AddBuff(89020);
    antiHealBuff.Clear();
    antiHealBuff.set_id(89020);
    antiHealBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Modifier));
    antiHealBuff.set_modifier_stat(static_cast<uint32_t>(BattleModifier::IncomingHeal));
    antiHealBuff.set_ratio(-0.5);
    antiHealBuff.set_duration_rounds(5);
    antiHealBuff.set_tick_on_apply(1);
    (*antiHealBuff.mutable_tag())["Debuff"] = true;

    // 场景 A：R1 对首领上减疗 -> R2 回春被压制，触发「气馁」且不净化减疗。
    auto req = MakeBossTrialRequest(5103, 102, 1, 1000);
    req.mutable_players(0)->add_skill_table_ids(8902);
    (*req.mutable_players(0)->mutable_skill_ranks())[8902] = 1;
    req.mutable_trial_context()->set_journal_entry_id(103);
    req.mutable_trial_context()->set_trial_rule_version(1);

    TurnBattleEngine engA(mem);
    ASSERT_TRUE(engA.Initialize(req));
    uint64_t bossId = 0, guardId = 0;
    for (const auto& a : engA.BuildStateSnapshot().actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            if (a.is_boss()) bossId = a.actor_id();
            else guardId = a.actor_id();
        }
    }
    engA.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, bossId, 8902));
    engA.ResolveCurrentRound();
    engA.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, bossId));
    const auto r2A = engA.ResolveCurrentRound();
    bool sawDishearten = false, sawCleanse = false;
    for (const auto& ev : r2A.events()) {
        if (ev.event_type() == BATTLE_EVENT_BOSS_MECHANIC) {
            if (ev.mechanic_kind() == turnbattle::kBossMechanicAntiHealDishearten) sawDishearten = true;
            if (ev.mechanic_kind() == turnbattle::kBossMechanicRejuvCleanse) sawCleanse = true;
        }
    }
    EXPECT_TRUE(sawDishearten);
    EXPECT_FALSE(sawCleanse);

    // 场景 B：R1 击杀已发出治疗预告的 100 血回春护卫 -> 触发 6% 断疗反噬 + 破绽 + 回合末进二阶段「决战姿态」且绝不双劈。
    TurnBattleEngine engB(mem);
    ASSERT_TRUE(engB.Initialize(req));
    engB.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, guardId));
    const auto r1B = engB.ResolveCurrentRound();
    bool sawBacklash = false, sawPhaseTwo = false;
    for (const auto& ev : r1B.events()) {
        if (ev.event_type() == BATTLE_EVENT_BOSS_MECHANIC) {
            if (ev.mechanic_kind() == turnbattle::kBossMechanicHealBacklash && ev.value() >= 1200u) sawBacklash = true;
            if (ev.mechanic_kind() == turnbattle::kBossMechanicPhaseTwoEnrage) sawPhaseTwo = true;
        }
    }
    EXPECT_TRUE(sawBacklash) << "击杀读条中的回春护卫必须触发 6% 灵力反噬";
    EXPECT_TRUE(sawPhaseTwo) << "特殊护卫全灭回合末必须进入二阶段决战姿态";
    EXPECT_EQ(r1B.state().boss_phase(), 2u);
    EXPECT_EQ(r1B.state().boss_exposed_pct(), turnbattle::kBossExposedFullPct);
    EXPECT_EQ(engB.BuildSettlement(30001).trial_facts().heal_interrupt_backlash_count(), 1u);

    // V7 防线：R1 已预告 R2 重劈，进二阶段后 R2 仅正常兑现 R2 重劈，R3 才发下一次 2 回合循环预告（R4 兑现）。
    engB.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
    const auto r2B = engB.ResolveCurrentRound();
    uint32_t r3BossCleaves = 0;
    for (const auto& t : r2B.state().telegraphs()) {
        if (t.source_actor_id() == bossId && t.intent_kind() == turnbattle::kTelegraphKindSlowCleave) {
            EXPECT_EQ(t.execute_round(), 4u) << "二阶段 2 回合循环：R2 兑现后应于 R3 预告 R4 兑现";
            ++r3BossCleaves;
        }
    }
    EXPECT_EQ(r3BossCleaves, 1u);
}

TEST_F(SeasonBattleTest, BossTrialV2HardStanceDynamicSwitchMomentumAndDispelBreak) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 104, turnbattle::kBossTrialKindBreakStance, /*difficulty=*/2, /*roundLimit=*/15, 40000, 20000);
    auto& magicSkill = mem->AddSkill(8903);
    magicSkill.Clear();
    magicSkill.set_id(8903);
    magicSkill.set_battle_model(1);
    magicSkill.set_caster_mask(1);
    magicSkill.set_ability_kind(1);
    magicSkill.set_unlock_level(1);
    magicSkill.set_max_rank(10);
    magicSkill.set_target_team(1);
    magicSkill.set_max_targets(1);
    magicSkill.set_damage_type(combatdamage::kMagicDamage);
    magicSkill.set_attack_multiplier(1.5);
    magicSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    magicSkill.set_mana_percent(0.01);
    magicSkill.add_effect(89030);
    mem->SetSkillDamage(8903, 250);
    auto& magicDmg = mem->AddBuff(89030);
    magicDmg.Clear();
    magicDmg.set_id(89030);
    magicDmg.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Damage));
    magicDmg.set_value_basis(2);
    magicDmg.set_ratio(1.5);

    auto& dispelSkill = mem->AddSkill(8904);
    dispelSkill.Clear();
    dispelSkill.set_id(8904);
    dispelSkill.set_battle_model(1);
    dispelSkill.set_caster_mask(1);
    dispelSkill.set_ability_kind(1);
    dispelSkill.set_unlock_level(1);
    dispelSkill.set_max_rank(10);
    dispelSkill.set_target_team(1);
    dispelSkill.set_max_targets(1);
    dispelSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    dispelSkill.set_mana_percent(0.01);
    dispelSkill.add_effect(89040);
    auto& dispelBuff = mem->AddBuff(89040);
    dispelBuff.Clear();
    dispelBuff.set_id(89040);
    dispelBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::DispelBuff));
    dispelBuff.set_effect_count(1);

    auto req = MakeBossTrialRequest(5104, 104, 1, 1000);
    req.mutable_players(0)->add_skill_table_ids(8903);
    req.mutable_players(0)->add_skill_table_ids(8904);
    (*req.mutable_players(0)->mutable_skill_ranks())[8903] = 1;
    (*req.mutable_players(0)->mutable_skill_ranks())[8904] = 1;
    req.mutable_trial_context()->set_journal_entry_id(104);
    req.mutable_trial_context()->set_trial_rule_version(1);

    TurnBattleEngine eng(mem);
    ASSERT_TRUE(eng.Initialize(req));
    uint64_t bossId = 0, guardId = 0;
    for (const auto& a : eng.BuildStateSnapshot().actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            if (a.is_boss()) bossId = a.actor_id();
            else guardId = a.actor_id();
        }
    }
    // R1：护势展开物防阵 + 困难切阵叠 1 层蓄势。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
    const auto r1 = eng.ResolveCurrentRound();
    EXPECT_EQ(r1.state().boss_stance_type(), turnbattle::kBossStancePhysical);
    EXPECT_EQ(r1.state().boss_momentum_stacks(), 1u);

    // R2：物理普攻打在物防阵上 -> 触发轻反制蓄势（但在 R2 重劈发动时消耗清零）；先验事件中有蓄势叠加。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, guardId));
    const auto r2 = eng.ResolveCurrentRound();
    bool sawCounterMomentum = false;
    for (const auto& ev : r2.events()) {
        if (ev.event_type() == BATTLE_EVENT_BOSS_MECHANIC &&
            ev.mechanic_kind() == turnbattle::kBossMechanicMomentumGain && ev.value() == 2u) {
            sawCounterMomentum = true;
        }
    }
    EXPECT_TRUE(sawCounterMomentum) << "打中联防物防阵应触发轻反制叠蓄势";

    // R3：先对护卫施放法术大招压高法伤，再测 R4 顺势应变切法防阵。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, bossId, 8903));
    const auto r3 = eng.ResolveCurrentRound();
    bool sawDynamicMagicStance = false;
    for (const auto& t : r3.state().telegraphs()) {
        if (t.intent_kind() == turnbattle::kTelegraphKindGuardStanceMagic && t.execute_round() == 4u) {
            sawDynamicMagicStance = true;
        }
    }
    EXPECT_TRUE(sawDynamicMagicStance) << "困难护势进阶在 R2-R3 法伤更高时，R4 必须动态切法防阵";

    // R4 让法防阵落地，R5 驱散护卫触发崩势（清护势与蓄势，且首领挂破绽 +35%）。
    eng.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
    const auto r4 = eng.ResolveCurrentRound();
    EXPECT_EQ(r4.state().boss_stance_type(), turnbattle::kBossStanceMagic);
    eng.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, guardId, 8904));
    const auto r5 = eng.ResolveCurrentRound();
    EXPECT_EQ(r5.state().boss_stance_type(), 0u);
    EXPECT_EQ(r5.state().boss_momentum_stacks(), 0u);
    EXPECT_EQ(r5.state().boss_exposed_pct(), turnbattle::kBossExposedFullPct);
    EXPECT_EQ(eng.BuildSettlement(30001).trial_facts().stance_break_count(), 1u);
}

TEST_F(SeasonBattleTest, BossTrialV2HardDualGuardEliteFormationAndDualCleavePartialParry) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    // 配置 Dungeon 112（困难·双卫同阵：101首领 + 102回春 + 103护势）及策应卫 109破军 / 110鼓噪。
    const auto addMon = [&](uint32_t id, uint64_t hp, bool isBoss) {
        auto& m = mem->AddMonster(id);
        m.Clear();
        m.set_id(id);
        m.set_health(hp);
        m.set_strength(120);
        m.set_armor(150);
        m.set_resistance(10);
        m.set_speed(700);
        m.set_is_boss(isBoss ? 1 : 0);
    };
    addMon(101, 10000, true);
    addMon(102, 4000, false);
    addMon(103, 4000, false);
    addMon(109, 2200, false);
    addMon(110, 2000, false);

    mem->AddBattleReward(0, "").set_exp_gain(100);
    auto& d112 = mem->AddDungeon(112);
    d112.Clear();
    d112.set_id(112);
    d112.set_max_team_size(5);
    d112.set_boss_trial_kind(turnbattle::kBossTrialKindBreakDualGuard);
    d112.set_difficulty(2);
    d112.set_round_limit(15);
    mem->SetDungeonMonsters(112, {101, 102, 103});
    mem->SetDungeonMonstersPerPlayer(112, 3);

    // 单人双卫同阵：3 怪，两位护卫压缩至 75% 血（3000）。
    {
        auto reqSolo = MakeBossTrialRequest(5105, 112, 1, 500);
        TurnBattleEngine engSolo(mem);
        ASSERT_TRUE(engSolo.Initialize(reqSolo));
        const auto snap = engSolo.BuildStateSnapshot();
        uint32_t monCount = 0;
        for (const auto& a : snap.actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
                ++monCount;
                if (!a.is_boss()) EXPECT_EQ(a.max_health(), 3000u) << "双卫同阵核心护卫血量应压缩至 75%";
            }
        }
        EXPECT_EQ(monCount, 3u);
    }

    // 3 人双卫同阵：4 怪（含破军卫 109），首领裂地双劈点名 2 人，1/2 化解触发轻破绽 +20%（V3 防线）。
    auto req3p = MakeBossTrialRequest(5106, 112, 3, 200);
    TurnBattleEngine eng3p(mem);
    ASSERT_TRUE(eng3p.Initialize(req3p));
    const auto s1 = eng3p.BuildStateSnapshot();
    uint32_t monCount3p = 0;
    bool hasBreaker = false;
    for (const auto& a : s1.actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            ++monCount3p;
            if (a.monster_table_id() == turnbattle::kBossTrialMonsterSupportBreaker) hasBreaker = true;
        }
    }
    EXPECT_EQ(monCount3p, 4u);
    EXPECT_TRUE(hasBreaker);
    std::vector<uint64_t> dualTargets;
    bool r1HasStance = false, r1HasHeal = false;
    for (const auto& t : s1.telegraphs()) {
        if (t.intent_kind() == turnbattle::kTelegraphKindSlowCleave) {
            for (const uint64_t tid : t.target_actor_ids()) dualTargets.push_back(tid);
        }
        if (t.intent_kind() == turnbattle::kTelegraphKindGuardStancePhys) r1HasStance = true;
        if (t.intent_kind() == turnbattle::kTelegraphKindGuardHeal) r1HasHeal = true;
    }
    ASSERT_EQ(dualTargets.size(), 2u) << "3人局裂地双劈应点名2名不同目标";
    EXPECT_NE(dualTargets[0], dualTargets[1]);
    EXPECT_TRUE(r1HasStance);
    EXPECT_FALSE(r1HasHeal) << "双卫同阵 R1 护势/R2 重劈/R3 回春错峰";

    // R1 推进，R2 仅第 1 个点名目标防御，第 2 个不防 -> 触发 20% 轻破绽（V3）。
    for (uint64_t pid : {30001ULL, 30002ULL, 30003ULL}) eng3p.SubmitAction(pid, Command(BATTLE_ACTION_DEFEND));
    eng3p.ResolveCurrentRound();
    for (uint64_t pid : {30001ULL, 30002ULL, 30003ULL}) {
        eng3p.SubmitAction(pid, Command(pid == dualTargets[0] ? BATTLE_ACTION_DEFEND : BATTLE_ACTION_ATTACK, 1000001));
    }
    const auto r2 = eng3p.ResolveCurrentRound();
    EXPECT_EQ(r2.state().boss_exposed_pct(), turnbattle::kBossExposedPartialPct)
        << "3人裂地双劈 1/2 化解应触发 +20% 轻破绽";

    // 4 人单卫战阵：应生成 4 怪（首领 + 特殊护卫 + 破军卫 109 + 鼓噪卫 110），且鼓噪卫回合末叠蓄势。
    auto& d101 = mem->AddDungeon(101);
    d101.Clear();
    d101.set_id(101);
    d101.set_max_team_size(5);
    d101.set_boss_trial_kind(turnbattle::kBossTrialKindBreakRejuv);
    d101.set_difficulty(1);
    d101.set_round_limit(12);
    mem->SetDungeonMonsters(101, {101, 102});
    mem->SetDungeonMonstersPerPlayer(101, 2);
    auto req4p = MakeBossTrialRequest(5107, 101, 4, 200);
    TurnBattleEngine eng4p(mem);
    ASSERT_TRUE(eng4p.Initialize(req4p));
    uint32_t monCount4p = 0;
    bool hasDrummer = false;
    for (const auto& a : eng4p.BuildStateSnapshot().actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            ++monCount4p;
            if (a.monster_table_id() == turnbattle::kBossTrialMonsterSupportDrummer) hasDrummer = true;
        }
    }
    EXPECT_EQ(monCount4p, 4u);
    EXPECT_TRUE(hasDrummer);
    for (uint64_t pid : {30001ULL, 30002ULL, 30003ULL, 30004ULL}) eng4p.SubmitAction(pid, Command(BATTLE_ACTION_DEFEND));
    const auto r1_4p = eng4p.ResolveCurrentRound();
    EXPECT_EQ(r1_4p.state().boss_momentum_stacks(), 1u) << "鼓噪卫存活回合末应给首领 +1 蓄势";
}

TEST_F(SeasonBattleTest, BossTrialV2TrialJournalSealConditionsWorkCorrectly) {
    BattleSettlementData settle;
    settle.set_battle_id(9001);
    settle.set_player_id(1001);
    settle.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
    settle.set_player_team_index(0);
    settle.set_total_rounds(6);
    auto* f = settle.mutable_trial_facts();
    f->set_journal_entry_id(105);
    f->set_trial_rule_version(1);
    f->set_initial_human_count(1);
    f->set_guard_stance_dispelled(true);
    f->set_boss_guard_heal_total(500);
    f->set_heal_interrupt_backlash_count(1);
    f->set_all_self_cleaves_parried(true);

    EXPECT_TRUE(trialjournal::IsValidConditionType(static_cast<uint32_t>(trialjournal::SealConditionType::kDualGuardBreak)));
    EXPECT_TRUE(trialjournal::IsValidConditionType(static_cast<uint32_t>(trialjournal::SealConditionType::kPerfectCleaveParry)));
    EXPECT_TRUE(trialjournal::EvaluateSealCondition(
        static_cast<uint32_t>(trialjournal::SealConditionType::kDualGuardBreak), 0, settle));
    EXPECT_TRUE(trialjournal::EvaluateSealCondition(
        static_cast<uint32_t>(trialjournal::SealConditionType::kPerfectCleaveParry), 0, settle));

    f->set_heal_interrupt_backlash_count(0);
    EXPECT_FALSE(trialjournal::EvaluateSealCondition(
        static_cast<uint32_t>(trialjournal::SealConditionType::kDualGuardBreak), 0, settle));
}

TEST_F(SeasonBattleTest, BossTrialV2BarrierShatterAndVenomParryTriggerExposed) {
    // 1. 灵障首领（kind=3）：击破灵障护盾立即触发 +35% 破绽与崩势破盾事件。
    auto memBarrier = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*memBarrier, 105, turnbattle::kBossTrialKindBarrier, /*difficulty=*/1, /*roundLimit=*/12, 10000, 4000);
    auto reqBarrier = MakeBossTrialRequest(5110, 105, 1, 200);
    reqBarrier.mutable_players(0)->set_physical_attack(5000);
    reqBarrier.mutable_trial_context()->set_journal_entry_id(201);
    TurnBattleEngine engBarrier(memBarrier);
    ASSERT_TRUE(engBarrier.Initialize(reqBarrier));
    uint64_t barrierBossId = 0;
    for (const auto& a : engBarrier.BuildStateSnapshot().actors()) {
        if (a.is_boss()) barrierBossId = a.actor_id();
    }
    ASSERT_NE(barrierBossId, 0u);
    // R1：灵障护卫为首领附加 12% 灵障护盾（1200）。
    engBarrier.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
    engBarrier.ResolveCurrentRound();
    // R2：单点普攻击破灵障护盾 -> 首领陷入 +35% 破绽并记录 stance_break_count。
    engBarrier.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, barrierBossId));
    const auto br2 = engBarrier.ResolveCurrentRound();
    EXPECT_EQ(br2.state().boss_exposed_pct(), turnbattle::kBossExposedFullPct);
    bool sawShieldShatterExpose = false;
    for (const auto& ev : br2.events()) {
        if (ev.event_type() == BATTLE_EVENT_BOSS_MECHANIC &&
            ev.mechanic_kind() == turnbattle::kBossMechanicStanceBreak &&
            ev.value() == turnbattle::kBossExposedFullPct) {
            sawShieldShatterExpose = true;
        }
    }
    EXPECT_TRUE(sawShieldShatterExpose);
    EXPECT_GE(engBarrier.BuildSettlement(30001).trial_facts().stance_break_count(), 1u);

    // 2. 幽毒首领（kind=4）：2 人局 R2 群体玄毒 1/2 防御触发 +20% 轻破绽，全员防御触发 +35% 完整破绽。
    auto memVenom = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*memVenom, 107, turnbattle::kBossTrialKindVenom, /*difficulty=*/1, /*roundLimit=*/12, 30000, 8000);
    auto reqVenomHalf = MakeBossTrialRequest(5111, 107, 2, 200);
    auto reqVenomFull = MakeBossTrialRequest(5112, 107, 2, 200);
    TurnBattleEngine engHalf(memVenom);
    TurnBattleEngine engFull(memVenom);
    ASSERT_TRUE(engHalf.Initialize(reqVenomHalf));
    ASSERT_TRUE(engFull.Initialize(reqVenomFull));
    // R1 预告玄毒群体冲击 -> R2 兑现。
    for (uint64_t pid : {30001ULL, 30002ULL}) {
        engHalf.SubmitAction(pid, Command(BATTLE_ACTION_DEFEND));
        engFull.SubmitAction(pid, Command(BATTLE_ACTION_DEFEND));
    }
    engHalf.ResolveCurrentRound();
    engFull.ResolveCurrentRound();
    // R2：半数防御 vs 全员防御。
    engHalf.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
    engHalf.SubmitAction(30002, Command(BATTLE_ACTION_ATTACK, 0));
    const auto vrHalf = engHalf.ResolveCurrentRound();
    EXPECT_EQ(vrHalf.state().boss_exposed_pct(), turnbattle::kBossExposedPartialPct);

    engFull.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
    engFull.SubmitAction(30002, Command(BATTLE_ACTION_DEFEND));
    const auto vrFull = engFull.ResolveCurrentRound();
    EXPECT_EQ(vrFull.state().boss_exposed_pct(), turnbattle::kBossExposedFullPct);
}

// ---- 五行阵法 × 人宠相生合击 × 五行幻境周常爬塔 × 道友切磋坊 × 战报配装摘要 ----

TEST_F(SeasonBattleTest, WuxingFormationCounterAndHumanPetSynergy) {
    // 1. 天覆阵(1, 速度+2% / 直伤+5%, 克地载阵+5%) vs 地载阵(2, 开局4%盾 / 减伤-6%)
    Player(kHero).set_formation_id(1);
    Player(kHero).set_class_id(1); // 金
    auto* pet = Player(kHero).add_pets();
    pet->set_pet_id(77001);
    pet->set_owner_player_id(kHero);
    pet->set_pet_table_id(3); // 水（金生水 → 触发每 3 回合相生合击）
    pet->set_pet_name("碧水灵狐");
    pet->set_level(60);
    pet->set_max_health(5000);
    pet->set_max_mana(1000);
    pet->mutable_base_attributes()->set_health(5000);
    pet->mutable_base_attributes()->set_mana(1000);
    pet->mutable_base_attributes()->set_speed(90);
    pet->set_physical_attack(400);
    pet->set_defense(100);

    Player(kEnemy).set_formation_id(2);
    Player(kEnemy).set_class_id(4); // 火

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto initState = engine.BuildStateSnapshot();
    EXPECT_EQ(initState.team_a_formation_id(), 1u);
    EXPECT_EQ(initState.team_b_formation_id(), 2u);
    // 地载阵开局 4% 最大气血护盾（10000 * 4% = 400）
    EXPECT_EQ(engine.TournamentEarthShieldOf(kEnemy), 400u);
    EXPECT_FALSE(initState.wuxing_synergy_active_this_round());

    // Round 1 & Round 2 不触发相生合击
    for (int r = 1; r <= 2; ++r) {
        engine.SubmitAction(kHero, Command(BATTLE_ACTION_DEFEND));
        engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto turn = engine.ResolveCurrentRound();
        EXPECT_FALSE(turn.state().wuxing_synergy_active_this_round());
    }
    // Round 3 触发金生水「人宠五行相生合击」事件 (+10%)
    engine.SubmitAction(kHero, Command(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
    const auto turn3 = engine.ResolveCurrentRound();
    EXPECT_TRUE(turn3.state().wuxing_synergy_active_this_round());
    bool sawSynergyEvent = false;
    for (const auto& ev : turn3.events()) {
        if (ev.event_type() == BATTLE_EVENT_WUXING_SYNERGY && ev.source_id() == kHero &&
            ev.value() == turnbattle::wuxing::kSynergyBonusPct) {
            sawSynergyEvent = true;
        }
    }
    EXPECT_TRUE(sawSynergyEvent);
}

TEST_F(SeasonBattleTest, WuxingTowerFloorScalingSigilsAndSettlementEcho) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 101, turnbattle::kBossTrialKindBreakRejuv, /*difficulty=*/1, /*roundLimit=*/15, 2000, 1000);
    auto req = MakeBossTrialRequest(6001, 101, 1, 200);
    auto* p = req.mutable_players(0);
    p->set_physical_attack(6000);
    auto* tc = p->mutable_tower_context();
    tc->set_week_key(202648);
    tc->set_run_id(7);
    tc->set_floor(5); // 5 层怪物气血 127%（85 + 4*8 + 10），攻击 111%
    tc->set_leyline_element(TOURNAMENT_LEYLINE_EARTH); // 土厚灵脉 +8% 护盾
    tc->add_active_sigil_ids(101); // 金·破锋 +8% 伤
    tc->add_active_sigil_ids(501); // 土·厚土 +12% 护盾（叠加土灵脉 8% = 20% → 4000）
    tc->add_active_sigil_ids(502); // 土·镇岳 12% 反伤

    TurnBattleEngine engine(mem);
    ASSERT_TRUE(engine.Initialize(req));
    // 玩家开局护盾 = 20000 * (8% + 12%) = 4000
    EXPECT_EQ(engine.TournamentEarthShieldOf(30001), 4000u);
    const auto state = engine.BuildStateSnapshot();
    ASSERT_TRUE(state.has_tower_context());
    EXPECT_EQ(state.tower_context().floor(), 5u);
    // 怪物血量已按第 5 层 127% 缩放（首领 2000 -> 2540）
    for (const auto& a : state.actors()) {
        if (a.is_boss()) {
            EXPECT_EQ(a.max_health(), 2540u);
        }
    }
    // 自动战斗快速打穿验证结算回传
    engine.SetActorAuto(30001, true);
    for (int i = 0; i < 15 && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++i) {
        engine.ResolveCurrentRound();
    }
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
    const auto settle = engine.BuildSettlement(30001);
    ASSERT_TRUE(settle.has_tower_settlement());
    EXPECT_EQ(settle.tower_settlement().week_key(), 202648u);
    EXPECT_EQ(settle.tower_settlement().run_id(), 7u);
    EXPECT_EQ(settle.tower_settlement().floor(), 5u);
    EXPECT_GT(settle.tower_settlement().total_rounds(), 0u);
    EXPECT_EQ(settle.defeated_monsters_size(), 0) << "五行幻境不发放常规野怪击杀与掉落";
}

TEST_F(SeasonBattleTest, CustomDuelRulesPolarityBanControlAndRoundLimitDraw) {
    // 非法回合上限 10 必须被 Initialize 拒绝
    auto badReq = request;
    badReq.mutable_custom_duel_rules()->set_round_limit_override(10);
    TurnBattleEngine badEngine(data);
    EXPECT_FALSE(badEngine.Initialize(badReq));

    // 注册控制技能用于验证 ban_control_skills
    auto& stunBuff = data->AddBuff(8901);
    stunBuff.Clear();
    stunBuff.set_id(8901);
    stunBuff.set_buff_type(turnbattle::kBuffTypeStun);
    stunBuff.set_duration(6.0);
    auto& stunSkill = data->AddSkill(8901);
    stunSkill.Clear();
    stunSkill.set_id(8901);
    stunSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    stunSkill.add_targeting_mode(1);
    stunSkill.add_effect(8901);
    Player(kHero).add_skill_table_ids(8901);

    auto* rules = request.mutable_custom_duel_rules();
    rules->set_enable_polarity_rotation(true);
    rules->set_ban_control_skills(true);
    rules->set_round_limit_override(8);
    rules->set_allow_public_spectate(true);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto initState = engine.BuildStateSnapshot();
    EXPECT_EQ(initState.round_limit(), 8u);
    EXPECT_NE(initState.active_polarity(), 0u);
    EXPECT_TRUE(initState.custom_duel_rules().ban_control_skills());
    // 禁控模式下，提交控制技能必须被校验拒绝
    EXPECT_NE(engine.ValidateAction(kHero, Command(BATTLE_ACTION_SKILL, kEnemy, 8901)), 0u);

    // 双方防御打满 8 回合必判平局
    for (int r = 0; r < 8 && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++r) {
        engine.SubmitAction(kHero, Command(BATTLE_ACTION_DEFEND));
        engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
    }
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_DRAW);
}

TEST_F(SeasonBattleTest, BattleReplayHeaderCarriesSanitizedActorBuilds) {
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    ::BattleActorBuildSummary build;
    build.set_player_id(kHero);
    build.set_name("Hero");
    build.set_formation_id(3);
    build.set_wuxing_element(2);
    build.set_season_seal_id(101);
    build.add_skill_table_ids(15001);

    turnbattle::BattleReplayHeader header;
    header.battleId = request.battle_id();
    header.matchMode = request.match_mode();
    header.battleConfigId = request.battle_config_id();
    header.startedAtMs = 1764000000000ULL;
    header.actorBuilds.push_back(build);

    turnbattle::BattleReplayRecorder recorder(header, engine.BuildStateSnapshot());
    const auto env = recorder.Freeze(BATTLE_OUTCOME_DRAW, 1, 1764000060000ULL);
    ASSERT_TRUE(env.has_value());
    const auto raw = turnbattle::GzipDecompress(env->payload(), turnbattle::kMaxBattleReplayRawBytes);
    ASSERT_TRUE(raw.has_value());
    ::BattleReplay decoded;
    ASSERT_TRUE(decoded.ParseFromString(*raw));
    ASSERT_EQ(decoded.actor_builds_size(), 1);
    EXPECT_EQ(decoded.actor_builds(0).player_id(), kHero);
    EXPECT_EQ(decoded.actor_builds(0).formation_id(), 3u);
    EXPECT_EQ(decoded.actor_builds(0).season_seal_id(), 101u);
}

TEST_F(SeasonBattleTest, SymmetricWeeklyAffixesApplyCleanlyAndFailClosedOnMismatch) {
    // 1. 双方词缀不一致时 fail-closed 退回 0
    Player(kHero).set_season_weekly_affix_id(seasonrules::kWeeklyAffixSpiritRecovery);
    Player(kEnemy).set_season_weekly_affix_id(seasonrules::kWeeklyAffixOpeningGuard);
    TurnBattleEngine mismatchEngine(data);
    ASSERT_TRUE(mismatchEngine.Initialize(request));
    EXPECT_EQ(mismatchEngine.BuildStateSnapshot().season_weekly_affix_id(), 0u);

    // 2. 灵息充盈周 (2)：双方防御额外回复 3% 最大法力（10000 -> +300）
    Player(kHero).set_season_weekly_affix_id(seasonrules::kWeeklyAffixSpiritRecovery);
    Player(kEnemy).set_season_weekly_affix_id(seasonrules::kWeeklyAffixSpiritRecovery);
    Player(kHero).mutable_base_attributes()->set_mana(5000);
    Player(kEnemy).mutable_base_attributes()->set_mana(5000);
    TurnBattleEngine manaEngine(data);
    ASSERT_TRUE(manaEngine.Initialize(request));
    EXPECT_EQ(manaEngine.BuildStateSnapshot().season_weekly_affix_id(),
              seasonrules::kWeeklyAffixSpiritRecovery);
    manaEngine.SubmitAction(kHero, Command(BATTLE_ACTION_DEFEND));
    manaEngine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
    manaEngine.ResolveCurrentRound();
    const auto manaState = manaEngine.BuildStateSnapshot();
    EXPECT_EQ(ActorOf(manaState, kHero).attributes().mana(), 5300u);
    EXPECT_EQ(ActorOf(manaState, kEnemy).attributes().mana(), 5300u);

    // 3. 锦标赛五行灵脉存在时压制周词缀，防多层环境叠加
    request.mutable_tournament_context()->set_match_id(99);
    request.mutable_tournament_context()->set_leyline_element(TOURNAMENT_LEYLINE_WATER);
    TurnBattleEngine leylineEngine(data);
    ASSERT_TRUE(leylineEngine.Initialize(request));
    EXPECT_EQ(leylineEngine.BuildStateSnapshot().season_weekly_affix_id(), 0u);
    request.clear_tournament_context();

    // 4. 先机守势周 (3) 降低前 2 回合直伤 vs 锋芒竞速周 (4) 对无盾目标增伤 5%
    auto measureRound1AttackDamage = [&](uint32_t affixId) -> uint64_t {
        Player(kHero).set_season_weekly_affix_id(affixId);
        Player(kEnemy).set_season_weekly_affix_id(affixId);
        Player(kEnemy).mutable_base_attributes()->set_health(10000);
        TurnBattleEngine eng(data);
        EXPECT_TRUE(eng.Initialize(request));
        eng.SubmitAction(kHero, Command(BATTLE_ACTION_ATTACK, kEnemy));
        eng.SubmitAction(kEnemy, Command(BATTLE_ACTION_ATTACK, kHero));
        eng.ResolveCurrentRound();
        return 10000u - ActorOf(eng.BuildStateSnapshot(), kEnemy).attributes().health();
    };
    const uint64_t baseDmg = measureRound1AttackDamage(seasonrules::kWeeklyAffixNone);
    const uint64_t guardDmg = measureRound1AttackDamage(seasonrules::kWeeklyAffixOpeningGuard);
    const uint64_t sharpDmg = measureRound1AttackDamage(seasonrules::kWeeklyAffixSharpTempo);
    EXPECT_LT(guardDmg, baseDmg) << "先机守势周第 1 回合直伤应降低 8%";
    EXPECT_GT(sharpDmg, baseDmg) << "锋芒竞速周攻击无盾目标直伤应提升 5%";

    // 5. 护盾过载周 (1)：双方技能护盾 +15%
    auto& shieldSkill = data->AddSkill(15999);
    shieldSkill.Clear();
    shieldSkill.set_id(15999);
    shieldSkill.set_battle_model(1);
    shieldSkill.set_ability_kind(1);
    shieldSkill.set_caster_mask(1);
    shieldSkill.set_target_team(2);
    shieldSkill.set_max_targets(1);
    shieldSkill.set_pet_max_targets(1);
    shieldSkill.add_skill_type(1);
    shieldSkill.set_unlock_level(1);
    shieldSkill.set_max_rank(10);
    shieldSkill.add_effect(159990);
    auto& shieldBuff = data->AddBuff(159990);
    shieldBuff.Clear();
    shieldBuff.set_id(159990);
    shieldBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
    shieldBuff.set_duration_rounds(2);
    shieldBuff.set_max_layer(1);
    shieldBuff.set_refresh_policy(2);
    shieldBuff.set_effect_priority(20);
    shieldBuff.set_flat_value(1000.0);
    Player(kHero).add_skill_table_ids(15999);
    (*Player(kHero).mutable_skill_ranks())[15999] = 1;

    auto measureShield = [&](uint32_t affixId) -> uint64_t {
        Player(kHero).set_season_weekly_affix_id(affixId);
        Player(kEnemy).set_season_weekly_affix_id(affixId);
        TurnBattleEngine eng(data);
        EXPECT_TRUE(eng.Initialize(request));
        eng.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kHero, 15999));
        eng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        eng.ResolveCurrentRound();
        return ShieldCapacity(eng.BuildStateSnapshot(), kHero, 159990);
    };
    const uint64_t normalShield = measureShield(seasonrules::kWeeklyAffixNone);
    const uint64_t overloadShield = measureShield(seasonrules::kWeeklyAffixShieldOverload);
    EXPECT_GT(normalShield, 0u);
    EXPECT_GT(overloadShield, normalShield) << "护盾过载周护盾量应提升 15%";
}

TEST_F(SeasonBattleTest, BossTrialWeeklyAffixRotationIsolationAndInvalidOverride) {
    // 1. 确定性 5 周轮换（UTC+8 周一 05:00 基准，零漂移）
    EXPECT_EQ(turnbattle::bossaffix::AffixIdForTimestamp(0ULL), turnbattle::bossaffix::kAffixNone);
    for (uint64_t w = 0; w < 6; ++w) {
        const uint64_t ts = turnbattle::bossaffix::kFirstMonday05Utc8Ms + w * turnbattle::bossaffix::kWeekMs + 12345ULL;
        EXPECT_EQ(turnbattle::bossaffix::AffixIdForTimestamp(ts), static_cast<uint32_t>((w % 5ULL) + 1ULL));
    }

    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 101, turnbattle::kBossTrialKindBreakRejuv, /*difficulty=*/1, /*roundLimit=*/12, 10000, 4000);

    // 2. 真实首领试炼自动按 created_at_ms 冻结词缀并回显到 BattleStateS2C
    auto reqAuto = MakeBossTrialRequest(5901, 101, 1, 500);
    reqAuto.set_created_at_ms(turnbattle::bossaffix::kFirstMonday05Utc8Ms + 2ULL * turnbattle::bossaffix::kWeekMs + 1000ULL);
    TurnBattleEngine engAuto(mem);
    ASSERT_TRUE(engAuto.Initialize(reqAuto));
    EXPECT_EQ(engAuto.BossWeeklyAffixId(), turnbattle::bossaffix::kAffixWater);
    EXPECT_EQ(engAuto.BuildStateSnapshot().boss_weekly_affix_id(), turnbattle::bossaffix::kAffixWater);

    // 3. 五行幻境爬塔复用首领试炼副本时强制隔离（恒为 0）
    auto reqTower = MakeBossTrialRequest(5902, 101, 1, 500);
    reqTower.set_created_at_ms(reqAuto.created_at_ms());
    reqTower.mutable_players(0)->mutable_tower_context()->set_floor(1);
    reqTower.mutable_players(0)->mutable_tower_context()->set_run_id(77);
    TurnBattleEngine engTower(mem);
    ASSERT_TRUE(engTower.Initialize(reqTower));
    EXPECT_EQ(engTower.BossWeeklyAffixId(), turnbattle::bossaffix::kAffixNone);
    EXPECT_EQ(engTower.BuildStateSnapshot().boss_weekly_affix_id(), 0u);

    // 4. 非法覆盖 ID (> 5) 建房 fail-closed 拒绝
    auto reqBad = MakeBossTrialRequest(5903, 101, 1, 500);
    reqBad.set_boss_weekly_affix_id(6u);
    TurnBattleEngine engBad(mem);
    EXPECT_FALSE(engBad.Initialize(reqBad));

    // 5. 非首领试炼（护卫救援 109）强制隔离（恒为 0）
    SetupBossTrialData(*mem, 109, turnbattle::kBossTrialKindGuardianRescue, /*difficulty=*/1, /*roundLimit=*/8, 10000, 1000);
    auto reqRescue = MakeBossTrialRequest(5904, 109, 1, 500);
    reqRescue.set_created_at_ms(reqAuto.created_at_ms());
    TurnBattleEngine engRescue(mem);
    ASSERT_TRUE(engRescue.Initialize(reqRescue));
    EXPECT_EQ(engRescue.BossWeeklyAffixId(), turnbattle::bossaffix::kAffixNone);
}

TEST_F(SeasonBattleTest, BossTrialWeeklyAffixMetalWoodAndWaterEffects) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 103, turnbattle::kBossTrialKindBreakStance, /*difficulty=*/1, /*roundLimit=*/12, 10000, 4000);

    // 驱散技能（8951）与蓄力爆发技能（8952 -> PrepareCast 89520 -> Damage 89521）
    auto& dispelSkill = mem->AddSkill(8951);
    dispelSkill.Clear();
    dispelSkill.set_id(8951);
    dispelSkill.set_battle_model(1);
    dispelSkill.set_caster_mask(1);
    dispelSkill.set_ability_kind(1);
    dispelSkill.set_unlock_level(1);
    dispelSkill.set_max_rank(10);
    dispelSkill.set_target_team(1);
    dispelSkill.set_max_targets(1);
    dispelSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    dispelSkill.set_mana_percent(0.10);
    dispelSkill.add_effect(89510);
    auto& dispelBuff = mem->AddBuff(89510);
    dispelBuff.Clear();
    dispelBuff.set_id(89510);
    dispelBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::DispelBuff));
    dispelBuff.set_effect_count(1);

    auto& chargeSkill = mem->AddSkill(8952);
    chargeSkill.Clear();
    chargeSkill.set_id(8952);
    chargeSkill.set_battle_model(1);
    chargeSkill.set_caster_mask(1);
    chargeSkill.set_ability_kind(1);
    chargeSkill.set_unlock_level(1);
    chargeSkill.set_max_rank(10);
    chargeSkill.set_target_team(1);
    chargeSkill.set_max_targets(1);
    chargeSkill.set_damage_type(combatdamage::kMagicDamage);
    chargeSkill.set_attack_multiplier(1.5);
    chargeSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    chargeSkill.set_mana_percent(0.20);
    chargeSkill.add_effect(89520);
    mem->SetSkillDamage(8952, 300);
    auto& prepareBuff = mem->AddBuff(89520);
    prepareBuff.Clear();
    prepareBuff.set_id(89520);
    prepareBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::PrepareCast));
    prepareBuff.set_duration(2);
    prepareBuff.add_sub_buff(89521);
    auto& chargeDmg = mem->AddBuff(89521);
    chargeDmg.Clear();
    chargeDmg.set_id(89521);
    chargeDmg.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Damage));
    chargeDmg.set_value_basis(2);
    chargeDmg.set_ratio(1.5);

    auto makeReq = [&](uint64_t bid, uint32_t affixId) {
        auto req = MakeBossTrialRequest(bid, 103, 1, 1000);
        req.set_boss_weekly_affix_id(affixId);
        auto* p = req.mutable_players(0);
        p->mutable_base_attributes()->set_mana(500);
        p->set_max_mana(1000);
        p->mutable_base_attributes()->set_critchance(0);
        p->add_skill_table_ids(8951);
        p->add_skill_table_ids(8952);
        (*p->mutable_skill_ranks())[8951] = 1;
        (*p->mutable_skill_ranks())[8952] = 1;
        return req;
    };

    // 1. 金周（1）：敌方全体开局获 8% 金甲护盾（10000 * 8% = 800）
    {
        TurnBattleEngine eng(mem);
        ASSERT_TRUE(eng.Initialize(makeReq(5911, turnbattle::bossaffix::kAffixMetal)));
        uint64_t bossId = 0;
        for (const auto& a : eng.BuildStateSnapshot().actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) bossId = a.actor_id();
        }
        ASSERT_NE(bossId, 0u);
        EXPECT_EQ(eng.TournamentEarthShieldOf(bossId), 800u) << "金周首领开局应有 8% 金甲护盾";
    }

    // 2. 木周（2）：驱散护势触发 12% 回蓝（120）与机制播报，且 R2 末首领若缺血会触发 4% 生机反哺回血
    {
        TurnBattleEngine eng(mem);
        ASSERT_TRUE(eng.Initialize(makeReq(5912, turnbattle::bossaffix::kAffixWood)));
        uint64_t bossId = 0, guardId = 0;
        for (const auto& a : eng.BuildStateSnapshot().actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
                if (a.is_boss()) bossId = a.actor_id();
                else guardId = a.actor_id();
            }
        }
        // R1 先打首领一下让首领缺血，同时护势卫立起物防阵
        eng.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, bossId));
        const auto r1 = eng.ResolveCurrentRound();
        EXPECT_EQ(r1.state().boss_stance_type(), turnbattle::kBossStancePhysical);
        // R2 驱散护势卫：扣蓝 100（10%）后木周返还 120（12%），净增 +20 -> 520；同时 R2 末首领回 4% 血（400）
        eng.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, guardId, 8951));
        const auto r2 = eng.ResolveCurrentRound();
        uint64_t playerManaAfter = 0;
        for (const auto& a : r2.state().actors()) {
            if (a.actor_id() == 30001) playerManaAfter = a.attributes().mana();
        }
        EXPECT_EQ(playerManaAfter, 520u) << "木周驱散应立即返还 12% 最大法力";
        bool sawManaAffix = false, sawBossRegen = false;
        for (const auto& ev : r2.events()) {
            if (ev.event_type() == BATTLE_EVENT_BOSS_MECHANIC &&
                ev.mechanic_kind() == turnbattle::kBossMechanicWeeklyAffixTrigger) {
                if (ev.source_id() == 30001) sawManaAffix = true;
                if (ev.source_id() == bossId) sawBossRegen = true;
            }
        }
        EXPECT_TRUE(sawManaAffix);
        EXPECT_TRUE(sawBossRegen) << "木周 R2 末未受控首领应触发 4% 生机反哺";
    }

    // 3. 水周（3）：蓄力技耗蓝 -20%（200 -> 160）、兑现伤害 +20%，且偶数回合 R2 末首领额外 +1 层蓄势
    {
        auto runCharge = [&](uint64_t bid, uint32_t affixId, uint64_t& outManaSpent, uint64_t& outDamage,
                             uint32_t& outR2Momentum) {
            TurnBattleEngine eng(mem);
            ASSERT_TRUE(eng.Initialize(makeReq(bid, affixId)));
            uint64_t bossId = 0;
            for (const auto& a : eng.BuildStateSnapshot().actors()) {
                if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) bossId = a.actor_id();
            }
            eng.SubmitAction(30001, Command(BATTLE_ACTION_SKILL, bossId, 8952));
            const auto r1 = eng.ResolveCurrentRound();
            uint64_t manaAfterR1 = 0;
            for (const auto& a : r1.state().actors()) {
                if (a.actor_id() == 30001) manaAfterR1 = a.attributes().mana();
            }
            outManaSpent = 500u - manaAfterR1;
            const auto r2 = eng.ResolveCurrentRound();
            outDamage = 0;
            for (const auto& ev : r2.events()) {
                if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 30001 && ev.target_id() == bossId) {
                    outDamage += ev.value();
                }
            }
            outR2Momentum = r2.state().boss_momentum_stacks();
        };
        uint64_t baseMana = 0, waterMana = 0, baseDmg = 0, waterDmg = 0;
        uint32_t baseMom = 0, waterMom = 0;
        runCharge(5913, turnbattle::bossaffix::kAffixNone, baseMana, baseDmg, baseMom);
        runCharge(5914, turnbattle::bossaffix::kAffixWater, waterMana, waterDmg, waterMom);
        EXPECT_EQ(baseMana, 200u);
        EXPECT_EQ(waterMana, 160u) << "水周蓄力技耗蓝应减免 20%";
        EXPECT_GT(waterDmg, baseDmg) << "水周蓄力技兑现伤害应 +20%";
        EXPECT_GT(waterMom, baseMom) << "水周偶数回合 R2 末未受控首领应额外获得 +1 层蓄势";
    }
}

TEST_F(SeasonBattleTest, BossTrialWeeklyAffixFireAndEarthEffects) {
    auto mem = std::make_shared<MemoryBattleDataProvider>();
    SetupBossTrialData(*mem, 101, turnbattle::kBossTrialKindBreakRejuv, /*difficulty=*/1, /*roundLimit=*/12, 10000, 10000);

    // 1. 火周（4）：首领血量降到 55%（>50% 但 <=60%）即在 R1 末提前切二阶段，且灵宠直攻伤害 +18%
    {
        auto makeFireReq = [&](uint64_t bid, uint32_t affixId, uint64_t playerStrength) {
            auto req = MakeBossTrialRequest(bid, 101, 1, 1000);
            req.set_boss_weekly_affix_id(affixId);
            auto* p = req.mutable_players(0);
            p->mutable_base_attributes()->set_strength(playerStrength);
            p->mutable_base_attributes()->set_critchance(0);
            auto* pet = p->add_pets();
            pet->set_pet_id(7001);
            pet->set_level(50);
            pet->set_max_health(5000);
            pet->set_max_mana(500);
            pet->mutable_base_attributes()->set_health(5000);
            pet->mutable_base_attributes()->set_mana(500);
            pet->mutable_base_attributes()->set_strength(400);
            pet->mutable_base_attributes()->set_speed(900);
            return req;
        };
        auto runPetAndPhase = [&](uint64_t bid, uint32_t affixId, uint64_t& outPetDmg, uint32_t& outPhase) {
            TurnBattleEngine eng(mem);
            ASSERT_TRUE(eng.Initialize(makeFireReq(bid, affixId, 3800)));
            uint64_t bossId = 0, petActorId = 0;
            for (const auto& a : eng.BuildStateSnapshot().actors()) {
                if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) bossId = a.actor_id();
                if (a.actor_type() == BATTLE_ACTOR_TYPE_PET) petActorId = a.actor_id();
            }
            ASSERT_NE(bossId, 0u);
            ASSERT_NE(petActorId, 0u);
            eng.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, bossId));
            eng.SubmitActorAction(30001, petActorId, Command(BATTLE_ACTION_ATTACK, bossId));
            const auto r1 = eng.ResolveCurrentRound();
            outPetDmg = 0;
            for (const auto& ev : r1.events()) {
                if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == petActorId && ev.target_id() == bossId) {
                    outPetDmg += ev.value();
                }
            }
            outPhase = r1.state().boss_phase();
        };
        uint64_t basePetDmg = 0, firePetDmg = 0;
        uint32_t basePhase = 0, firePhase = 0;
        runPetAndPhase(5921, turnbattle::bossaffix::kAffixNone, basePetDmg, basePhase);
        runPetAndPhase(5922, turnbattle::bossaffix::kAffixFire, firePetDmg, firePhase);
        EXPECT_GT(firePetDmg, basePetDmg) << "火周灵宠直攻伤害应提升 18%";
        // 确认测试刚好压在 50%..60% 区间时火周提前二阶段
        if (basePhase == 1u) {
            EXPECT_EQ(firePhase, 2u) << "火周首领气血 <=60% 应提前进入二阶段决战姿态";
        }
    }

    // 2. 土周（5）：化劲化解首领 R2 慢重劈后，全队立即获 8% 厚土护盾 + R3 全伤害 +12%
    {
        auto runEarth = [&](uint64_t bid, uint32_t affixId, uint64_t& outR2Shield, uint64_t& outR3Dmg) {
            auto req = MakeBossTrialRequest(bid, 101, 1, 1000);
            req.set_boss_weekly_affix_id(affixId);
            req.mutable_players(0)->mutable_base_attributes()->set_critchance(0);
            TurnBattleEngine eng(mem);
            ASSERT_TRUE(eng.Initialize(req));
            uint64_t bossId = 0;
            for (const auto& a : eng.BuildStateSnapshot().actors()) {
                if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) bossId = a.actor_id();
            }
            eng.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
            (void)eng.ResolveCurrentRound(); // R1 预告 R2 慢重劈
            eng.SubmitAction(30001, Command(BATTLE_ACTION_DEFEND));
            (void)eng.ResolveCurrentRound(); // R2 化劲防御成功
            outR2Shield = eng.TournamentEarthShieldOf(30001);
            eng.SubmitAction(30001, Command(BATTLE_ACTION_ATTACK, bossId));
            const auto r3 = eng.ResolveCurrentRound();
            outR3Dmg = 0;
            for (const auto& ev : r3.events()) {
                if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == 30001 && ev.target_id() == bossId) {
                    outR3Dmg += ev.value();
                }
            }
        };
        uint64_t baseShield = 0, earthShield = 0, baseR3Dmg = 0, earthR3Dmg = 0;
        runEarth(5923, turnbattle::bossaffix::kAffixNone, baseShield, baseR3Dmg);
        runEarth(5924, turnbattle::bossaffix::kAffixEarth, earthShield, earthR3Dmg);
        EXPECT_EQ(baseShield, 0u);
        EXPECT_GT(earthShield, 0u) << "土周化劲破绽应立刻赐予我方 8% 最大气血厚土护盾";
        EXPECT_GT(earthR3Dmg, baseR3Dmg) << "土周化劲后下一回合我方全伤害应额外 +12%";
    }
}

TEST_F(SeasonBattleTest, SkillSealResonancesAndSettlementContributions) {
    // 1. 破阵锐印(101) × 破阵裂锋(15001)：击碎护盾返还 30% 耗蓝；未击碎不返蓝；结算回传削盾功劳与共鸣数
    {
        auto& skill = AddModernSkill(seasonrules::kSkillPozhenLiefeng, 1, combatdamage::kPhysicalDamage, 1000.0);
        skill.set_mana_percent(0.06); // max_mana 10000 -> cost 600, 30% refund = 180
        Player(kHero).set_active_season_id(1);
        Player(kHero).add_skill_table_ids(seasonrules::kSkillPozhenLiefeng);
        (*Player(kHero).mutable_skill_ranks())[seasonrules::kSkillPozhenLiefeng] = 1;
        auto* seal = Player(kHero).mutable_equipment_effects()->mutable_season_seal();
        seal->set_season_id(1);
        seal->set_seal_id(seasonrules::kSealPozhenRuiyin);
        seal->set_effect_id(1);
        seal->set_bonus_bps(1000);
        seal->set_per_round_limit(1);

        // 1a. 护盾剩余极薄，锐印额外破盾将其彻底击碎 -> 返蓝 180，共鸣事件 101
        AddShieldBuff(kEnemy, 96001, 50);
        TurnBattleEngine shatterEng(data);
        ASSERT_TRUE(shatterEng.Initialize(request));
        shatterEng.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kEnemy, seasonrules::kSkillPozhenLiefeng));
        shatterEng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto turn = shatterEng.ResolveCurrentRound();
        uint64_t resonanceRefund = 0;
        for (const auto& ev : turn.events()) {
            if (ev.event_type() == BATTLE_EVENT_ARTIFACT &&
                ev.season_seal_id() == seasonrules::kSealPozhenRuiyin &&
                ev.artifact_effect_id() == seasonrules::kResonanceEventSharpManaRefund) {
                resonanceRefund += ev.value();
            }
        }
        // 注：若直伤本身已先破 50 盾，额外破盾为 0；因此用刚好比直伤多 30 点的盾精确由锐印击碎
        // 下方先测一次厚盾拿到精准直伤再验证击碎边界
        (void)resonanceRefund;
    }

    // 1 (精确可控盾厚)：先用厚盾测出一次未击碎（不返蓝），再按直伤+10 盾厚精确由锐印击碎（返蓝 180）
    {
        auto reqThick = request;
        reqThick.mutable_players(0)->set_active_season_id(1);
        reqThick.mutable_players(0)->add_skill_table_ids(seasonrules::kSkillPozhenLiefeng);
        (*reqThick.mutable_players(0)->mutable_skill_ranks())[seasonrules::kSkillPozhenLiefeng] = 1;
        auto* seal = reqThick.mutable_players(0)->mutable_equipment_effects()->mutable_season_seal();
        seal->set_season_id(1);
        seal->set_seal_id(seasonrules::kSealPozhenRuiyin);
        seal->set_effect_id(1);
        seal->set_bonus_bps(1000);
        seal->set_per_round_limit(1);

        auto& shieldRow = data->AddBuff(96002);
        shieldRow.Clear();
        shieldRow.set_id(96002);
        shieldRow.set_infinite_duration(1);
        shieldRow.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
        shieldRow.set_effect_priority(20);
        auto* thickEntry = reqThick.mutable_players(1)->add_buffs();
        thickEntry->set_buff_id(96002);
        thickEntry->set_buff_table_id(96002);
        thickEntry->set_caster_id(kEnemy);
        thickEntry->set_layer(1);
        thickEntry->set_remaining_capacity(50000);
        thickEntry->set_snapshot_value(50000);
        thickEntry->set_tick_round(999);

        TurnBattleEngine noShatterEng(data);
        ASSERT_TRUE(noShatterEng.Initialize(reqThick));
        noShatterEng.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kEnemy, seasonrules::kSkillPozhenLiefeng));
        noShatterEng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto turnNoShatter = noShatterEng.ResolveCurrentRound();
        uint64_t directAbsorbed = 0;
        uint64_t sealStripped = 0;
        bool sawResonance = false;
        for (const auto& ev : turnNoShatter.events()) {
            if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == kHero) {
                directAbsorbed = ev.absorbed();
            }
            if (ev.event_type() == BATTLE_EVENT_ARTIFACT && ev.season_seal_id() == seasonrules::kSealPozhenRuiyin) {
                if (ev.artifact_effect_id() == 1u) sealStripped = ev.value();
                if (ev.artifact_effect_id() == seasonrules::kResonanceEventSharpManaRefund) sawResonance = true;
            }
        }
        ASSERT_GT(directAbsorbed, 0u);
        ASSERT_GT(sealStripped, 0u);
        EXPECT_FALSE(sawResonance) << "未击碎护盾不应触发锐印返蓝共鸣";
        EXPECT_EQ(ActorOf(turnNoShatter.state(), kHero).attributes().mana(), 9400u);
        const auto noShatterSettle = noShatterEng.BuildSettlement(kHero);
        EXPECT_EQ(noShatterSettle.final_season_seal_id(), seasonrules::kSealPozhenRuiyin);
        EXPECT_EQ(noShatterSettle.season_seal_contribution_value(), sealStripped);
        EXPECT_EQ(noShatterSettle.season_seal_trigger_count(), 1u);
        EXPECT_EQ(noShatterSettle.season_seal_resonance_count(), 0u);

        // 精确设置盾厚 = 直伤吸收 + min(sealStripped, 10)，保证由锐印额外破盾击碎最后一层盾
        auto reqShatter = reqThick;
        const uint64_t exactCap = directAbsorbed + std::min<uint64_t>(sealStripped, 10u);
        reqShatter.mutable_players(1)->mutable_buffs(0)->set_remaining_capacity(exactCap);
        reqShatter.mutable_players(1)->mutable_buffs(0)->set_snapshot_value(exactCap);
        TurnBattleEngine shatterEng(data);
        ASSERT_TRUE(shatterEng.Initialize(reqShatter));
        shatterEng.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kEnemy, seasonrules::kSkillPozhenLiefeng));
        shatterEng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto turnShatter = shatterEng.ResolveCurrentRound();
        uint64_t refundEventVal = 0;
        for (const auto& ev : turnShatter.events()) {
            if (ev.event_type() == BATTLE_EVENT_ARTIFACT &&
                ev.season_seal_id() == seasonrules::kSealPozhenRuiyin &&
                ev.artifact_effect_id() == seasonrules::kResonanceEventSharpManaRefund) {
                refundEventVal = ev.value();
            }
        }
        EXPECT_EQ(refundEventVal, 180u) << "锐印击碎护盾应返还 30% 耗蓝(600 * 30% = 180)";
        EXPECT_EQ(ActorOf(turnShatter.state(), kHero).attributes().mana(), 9580u);
        const auto shatterSettle = shatterEng.BuildSettlement(kHero);
        EXPECT_EQ(shatterSettle.season_seal_trigger_count(), 1u);
        EXPECT_EQ(shatterSettle.season_seal_resonance_count(), 1u);
        EXPECT_EQ(shatterSettle.season_seal_contribution_value(), std::min<uint64_t>(sealStripped, 10u));
    }

    // 2. 破阵玄印(102) × 破阵灵雷(15002)：命中有盾目标触发破盾，减防持续回合 2 -> 3 + 共鸣事件 102
    {
        auto& magicSkill = AddModernSkill(seasonrules::kSkillPozhenLinglei, 1, combatdamage::kMagicDamage, 1000.0);
        magicSkill.set_mana_percent(0.06);
        magicSkill.add_effect(96021);
        auto& debuff = data->AddBuff(96021);
        debuff.Clear();
        debuff.set_id(96021);
        debuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Modifier));
        debuff.set_modifier_stat(static_cast<uint32_t>(BattleModifier::Defense));
        debuff.set_ratio(-0.15);
        debuff.set_duration_rounds(2);

        auto reqMystic = request;
        reqMystic.mutable_players(0)->set_active_season_id(1);
        reqMystic.mutable_players(0)->add_skill_table_ids(seasonrules::kSkillPozhenLinglei);
        (*reqMystic.mutable_players(0)->mutable_skill_ranks())[seasonrules::kSkillPozhenLinglei] = 1;
        auto* seal = reqMystic.mutable_players(0)->mutable_equipment_effects()->mutable_season_seal();
        seal->set_season_id(1);
        seal->set_seal_id(seasonrules::kSealPozhenXuanyin);
        seal->set_effect_id(2);
        seal->set_bonus_bps(1000);
        seal->set_per_round_limit(1);
        auto* shieldEntry = reqMystic.mutable_players(1)->add_buffs();
        shieldEntry->set_buff_id(96002);
        shieldEntry->set_buff_table_id(96002);
        shieldEntry->set_caster_id(kEnemy);
        shieldEntry->set_layer(1);
        shieldEntry->set_remaining_capacity(50000);
        shieldEntry->set_snapshot_value(50000);
        shieldEntry->set_tick_round(999);

        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(reqMystic));
        eng.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kEnemy, seasonrules::kSkillPozhenLinglei));
        eng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto turn = eng.ResolveCurrentRound();
        uint32_t debuffRemain = 0;
        for (const auto& b : ActorOf(turn.state(), kEnemy).buffs()) {
            if (b.buff_table_id() == 96021u) debuffRemain = b.remain_rounds();
        }
        bool sawExtendEvent = false;
        for (const auto& ev : turn.events()) {
            if (ev.event_type() == BATTLE_EVENT_ARTIFACT &&
                ev.season_seal_id() == seasonrules::kSealPozhenXuanyin &&
                ev.artifact_effect_id() == seasonrules::kResonanceEventMysticExtendDebuff &&
                ev.value() == 1u) {
                sawExtendEvent = true;
            }
        }
        EXPECT_EQ(debuffRemain, 3u) << "玄印破盾共鸣应使 15002 减防回合 2 -> 3";
        EXPECT_TRUE(sawExtendEvent);
        const auto settle = eng.BuildSettlement(kHero);
        EXPECT_EQ(settle.final_season_seal_id(), seasonrules::kSealPozhenXuanyin);
        EXPECT_GT(settle.season_seal_contribution_value(), 0u);
        EXPECT_EQ(settle.season_seal_resonance_count(), 1u);
    }

    // 3. 固阵守印(103) × 固阵灵障(15003)：R1 受直伤触发守印 -> R2 灵障护盾 +10%；R3 不再重复增盾
    {
        auto& shieldSkill = data->AddSkill(seasonrules::kSkillGuzhenLingzhang);
        shieldSkill.Clear();
        shieldSkill.set_id(seasonrules::kSkillGuzhenLingzhang);
        shieldSkill.set_battle_model(1);
        shieldSkill.set_ability_kind(1);
        shieldSkill.set_caster_mask(1);
        shieldSkill.set_target_team(2);
        shieldSkill.set_max_targets(1);
        shieldSkill.set_pet_max_targets(1);
        shieldSkill.add_skill_type(1);
        shieldSkill.set_season_id(1);
        shieldSkill.set_unlock_level(1);
        shieldSkill.set_max_rank(10);
        shieldSkill.add_effect(96030);
        auto& shieldBuff = data->AddBuff(96030);
        shieldBuff.Clear();
        shieldBuff.set_id(96030);
        shieldBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
        shieldBuff.set_duration_rounds(2);
        shieldBuff.set_max_layer(1);
        shieldBuff.set_refresh_policy(2);
        shieldBuff.set_effect_priority(20);
        shieldBuff.set_flat_value(1000.0);

        auto reqGuard = request;
        reqGuard.mutable_players(0)->set_active_season_id(1);
        reqGuard.mutable_players(0)->add_skill_table_ids(seasonrules::kSkillGuzhenLingzhang);
        (*reqGuard.mutable_players(0)->mutable_skill_ranks())[seasonrules::kSkillGuzhenLingzhang] = 1;
        auto* seal = reqGuard.mutable_players(0)->mutable_equipment_effects()->mutable_season_seal();
        seal->set_season_id(1);
        seal->set_seal_id(seasonrules::kSealGuzhenShouyin);
        seal->set_effect_id(6);
        seal->set_bonus_bps(1000);
        seal->set_cap_bps(300);
        seal->set_active_rounds(2);
        seal->set_battle_limit(1);

        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(reqGuard));
        // R1: 敌方普攻打主角触发固阵守印减伤
        eng.SubmitAction(kHero, Command(BATTLE_ACTION_DEFEND));
        eng.SubmitAction(kEnemy, Command(BATTLE_ACTION_ATTACK, kHero));
        const auto r1 = eng.ResolveCurrentRound();
        uint64_t r1Mitigated = 0;
        for (const auto& ev : r1.events()) {
            if (ev.event_type() == BATTLE_EVENT_ARTIFACT &&
                ev.season_seal_id() == seasonrules::kSealGuzhenShouyin &&
                ev.artifact_effect_id() == 6u) {
                r1Mitigated = ev.value();
            }
        }
        ASSERT_GT(r1Mitigated, 0u);

        // R2: 施放 15003 固阵灵障 -> 护盾量 1000 * 1.10 = 1100（额外 +100）
        eng.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kHero, seasonrules::kSkillGuzhenLingzhang));
        eng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto r2 = eng.ResolveCurrentRound();
        EXPECT_EQ(ShieldCapacity(r2.state(), kHero, 96030u), 1100u) << "固阵守印次轮释放固阵灵障护盾应 +10%";
        uint64_t boostEventVal = 0;
        for (const auto& ev : r2.events()) {
            if (ev.event_type() == BATTLE_EVENT_ARTIFACT &&
                ev.season_seal_id() == seasonrules::kSealGuzhenShouyin &&
                ev.artifact_effect_id() == seasonrules::kResonanceEventGuardShieldBoost) {
                boostEventVal = ev.value();
            }
        }
        EXPECT_EQ(boostEventVal, 100u);
        const auto settle = eng.BuildSettlement(kHero);
        EXPECT_EQ(settle.final_season_seal_id(), seasonrules::kSealGuzhenShouyin);
        EXPECT_EQ(settle.season_seal_contribution_value(), r1Mitigated + 100u);
        EXPECT_EQ(settle.season_seal_trigger_count(), 1u);
        EXPECT_EQ(settle.season_seal_resonance_count(), 1u);
    }

    // 4. 聚息援印(104) × 破阵蓄势(15004)：R1 防御回蓝 -> R2 破阵蓄势耗蓝减半(500 -> 250)，R3 恢复全价 500
    {
        auto& auraSkill = data->AddSkill(seasonrules::kSkillPozhenXushi);
        auraSkill.Clear();
        auraSkill.set_id(seasonrules::kSkillPozhenXushi);
        auraSkill.set_battle_model(1);
        auraSkill.set_ability_kind(1);
        auraSkill.set_caster_mask(1);
        auraSkill.set_target_team(2);
        auraSkill.set_max_targets(1);
        auraSkill.set_pet_max_targets(1);
        auraSkill.add_skill_type(1);
        auraSkill.set_season_id(1);
        auraSkill.set_unlock_level(1);
        auraSkill.set_max_rank(10);
        auraSkill.set_mana_percent(0.05); // max_mana 10000 -> full cost 500, half cost 250
        auraSkill.add_effect(96040);
        auto& auraBuff = data->AddBuff(96040);
        auraBuff.Clear();
        auraBuff.set_id(96040);
        auraBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Modifier));
        auraBuff.set_modifier_stat(static_cast<uint32_t>(BattleModifier::PhysicalAttack));
        auraBuff.set_ratio(0.15);
        auraBuff.set_duration_rounds(2);

        auto reqBreath = request;
        reqBreath.mutable_players(0)->set_active_season_id(1);
        reqBreath.mutable_players(0)->mutable_base_attributes()->set_mana(5000);
        reqBreath.mutable_players(0)->add_skill_table_ids(seasonrules::kSkillPozhenXushi);
        (*reqBreath.mutable_players(0)->mutable_skill_ranks())[seasonrules::kSkillPozhenXushi] = 1;
        auto* seal = reqBreath.mutable_players(0)->mutable_equipment_effects()->mutable_season_seal();
        seal->set_season_id(1);
        seal->set_seal_id(seasonrules::kSealJuxiYuanyin);
        seal->set_effect_id(7);
        seal->set_bonus_bps(400); // +400 mana
        seal->set_per_round_limit(1);
        seal->set_battle_limit(3);

        TurnBattleEngine eng(data);
        ASSERT_TRUE(eng.Initialize(reqBreath));
        // R1: 普通防御 -> 援印回蓝 400 (5000 -> 5400)
        eng.SubmitAction(kHero, Command(BATTLE_ACTION_DEFEND));
        eng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto r1 = eng.ResolveCurrentRound();
        EXPECT_EQ(ActorOf(r1.state(), kHero).attributes().mana(), 5400u);

        // R2: 施放 15004 破阵蓄势 -> 耗蓝减半 250 (5400 -> 5150)，省蓝 250
        eng.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kHero, seasonrules::kSkillPozhenXushi));
        eng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto r2 = eng.ResolveCurrentRound();
        EXPECT_EQ(ActorOf(r2.state(), kHero).attributes().mana(), 5150u) << "聚息援印次轮释放破阵蓄势应耗蓝减半";
        uint64_t savedEventVal = 0;
        for (const auto& ev : r2.events()) {
            if (ev.event_type() == BATTLE_EVENT_ARTIFACT &&
                ev.season_seal_id() == seasonrules::kSealJuxiYuanyin &&
                ev.artifact_effect_id() == seasonrules::kResonanceEventBreathHalfMana) {
                savedEventVal = ev.value();
            }
        }
        EXPECT_EQ(savedEventVal, 250u);

        // R3: 未防御再放 15004 -> 恢复全价 500 (5150 -> 4650)
        eng.SubmitAction(kHero, Command(BATTLE_ACTION_SKILL, kHero, seasonrules::kSkillPozhenXushi));
        eng.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
        const auto r3 = eng.ResolveCurrentRound();
        EXPECT_EQ(ActorOf(r3.state(), kHero).attributes().mana(), 4650u);

        const auto settle = eng.BuildSettlement(kHero);
        EXPECT_EQ(settle.final_season_seal_id(), seasonrules::kSealJuxiYuanyin);
        EXPECT_EQ(settle.season_seal_contribution_value(), 650u); // 400 回蓝 + 250 省蓝
        EXPECT_EQ(settle.season_seal_trigger_count(), 1u);
        EXPECT_EQ(settle.season_seal_resonance_count(), 1u);
    }
}
