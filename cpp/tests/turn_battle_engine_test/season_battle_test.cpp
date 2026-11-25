// Battle 引擎侧赛季技能复核与单槽赛季印（方案 A）机制回归测试（docs/design/season-skills-and-seal-design.md）。

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <memory>

#include "memory_battle_data_provider.h"
#include "system/combat_damage_rules.h"
#include "system/turn_battle_engine.h"

namespace {

using turnbattle::BattleEffectOp;
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
