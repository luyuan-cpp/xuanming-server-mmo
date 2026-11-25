// Battle 引擎侧赛季技能复核与单槽赛季印（方案 A）机制回归测试（docs/design/season-skills-and-seal-design.md）。

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <memory>

#include "memory_battle_data_provider.h"
#include "system/combat_damage_rules.h"
#include "system/trial_journal_rules.h"
#include "system/turn_battle_engine.h"

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
