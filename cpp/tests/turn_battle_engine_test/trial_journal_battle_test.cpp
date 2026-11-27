// Battle 引擎侧五行试炼志 V2 权威战法事实冻结单测（docs/design/long-term-progression-v2.md）。
// 覆盖：无上下文不写 trial_facts；死亡后复苏仍记 self_or_pet_ever_died；
// 护卫对首领有效治疗累计 boss_guard_heal_total；物法双系直伤标记；驱散敌方护势标记；
// 破盾法宝/赛季印与清心符/赛季技能/净化标记；阵容快照（宝宝、法宝升序、赛季印、队友名）。

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>

#include "memory_battle_data_provider.h"
#include "system/battle_script.h"
#include "system/combat_damage_rules.h"
#include "system/trial_journal_rules.h"
#include "system/turn_battle_engine.h"

namespace {

using turnbattle::BattleEffectOp;
using turnbattle::BattleStateKind;
using turnbattle::TurnBattleEngine;

constexpr uint64_t kHeroA = 301;
constexpr uint64_t kHeroB = 302;
constexpr uint64_t kEnemy = 401;

BattleAction Command(eBattleActionType type, uint64_t target = 0, uint32_t skill = 0) {
    BattleAction action;
    action.set_action_type(type);
    action.set_target_id(target);
    action.set_skill_table_id(skill);
    return action;
}

class TrialJournalBattleTest : public ::testing::Test {
protected:
    void SetUp() override {
        data = std::make_shared<MemoryBattleDataProvider>();
        data->AddBattleReward(0, "");
        request.set_battle_id(92001);
        request.set_match_mode(3);
        request.set_seed(11);
    }

    BattlePlayerSnapshot& AddPlayer(uint64_t id, uint32_t team, const std::string& name, uint64_t hp = 10000,
                                    uint64_t speed = 200) {
        auto* p = request.add_players();
        p->set_player_id(id);
        p->set_player_name(name);
        p->set_team_index(team);
        p->set_level(30);
        p->set_max_health(hp);
        p->set_max_mana(10000);
        p->mutable_base_attributes()->set_health(hp);
        p->mutable_base_attributes()->set_mana(10000);
        p->mutable_base_attributes()->set_speed(speed);
        p->set_physical_attack(1000);
        p->set_magic_attack(1000);
        p->set_defense(100);
        return *p;
    }

    SkillTable& AddModernDamageSkill(uint32_t id, uint32_t damageType, double flatDamage, uint32_t seasonId = 0) {
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
        row.set_mana_percent(0.01);
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

    std::shared_ptr<MemoryBattleDataProvider> data;
    CreateBattleRequest request;
};

} // namespace

TEST_F(TrialJournalBattleTest, OmitsTrialFactsWhenTrialContextAbsent) {
    AddPlayer(kHeroA, 0, "剑客");
    AddPlayer(kEnemy, 1, "木桩", 100);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_ATTACK, kEnemy));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();

    const auto settlement = engine.BuildSettlement(kHeroA);
    EXPECT_FALSE(settlement.has_trial_facts());
}

TEST_F(TrialJournalBattleTest, TracksDualDamageShieldBreakSeasonalSkillLoadoutAndTeammates) {
    auto* ctx = request.mutable_trial_context();
    ctx->set_journal_entry_id(104);
    ctx->set_trial_rule_version(1);
    ctx->set_season_id(1);

    AddModernDamageSkill(16001, combatdamage::kPhysicalDamage, 800, /*seasonId=*/1);
    AddModernDamageSkill(16002, combatdamage::kMagicDamage, 800, /*seasonId=*/0);

    auto& pA = AddPlayer(kHeroA, 0, "道友甲", 10000, 300);
    pA.set_active_season_id(1);
    pA.add_skill_table_ids(16001);
    (*pA.mutable_skill_ranks())[16001] = 1;
    auto* pet = pA.add_pets();
    pet->set_pet_id(77001);
    pet->set_owner_player_id(kHeroA);
    pet->set_pet_table_id(2005);
    pet->set_level(30);
    pet->set_max_health(5000);
    pet->mutable_base_attributes()->set_health(5000);
    pet->mutable_base_attributes()->set_speed(150);

    auto* art1 = pA.mutable_equipment_effects()->add_artifacts();
    art1->set_item_id(88001);
    art1->set_effect_id(8);
    art1->set_bonus_bps(1000);
    art1->set_per_round_limit(1);
    art1->set_battle_limit(2);
    auto* art2 = pA.mutable_equipment_effects()->add_artifacts();
    art2->set_item_id(88002);
    art2->set_effect_id(1); // 破盾法宝
    art2->set_bonus_bps(2000);
    art2->set_per_round_limit(1);
    auto* seal = pA.mutable_equipment_effects()->mutable_season_seal();
    seal->set_season_id(1);
    seal->set_seal_id(101);
    seal->set_effect_id(1);
    seal->set_bonus_bps(1000);
    seal->set_per_round_limit(1);

    auto& pB = AddPlayer(kHeroB, 0, "道友乙", 10000, 250);
    pB.add_skill_table_ids(16002);
    (*pB.mutable_skill_ranks())[16002] = 1;

    auto& enemy = AddPlayer(kEnemy, 1, "试炼对手", 10000, 100);
    // 给敌方挂护盾以触发破盾削盾
    auto& shieldRow = data->AddBuff(19901);
    shieldRow.Clear();
    shieldRow.set_id(19901);
    shieldRow.set_infinite_duration(1);
    shieldRow.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
    shieldRow.set_effect_priority(20);
    auto* shieldBuff = enemy.add_buffs();
    shieldBuff->set_buff_id(19901);
    shieldBuff->set_buff_table_id(19901);
    shieldBuff->set_caster_id(kEnemy);
    shieldBuff->set_layer(1);
    shieldBuff->set_remaining_capacity(5000);
    shieldBuff->set_snapshot_value(5000);
    shieldBuff->set_tick_round(999);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_SKILL, kEnemy, 16001));
    engine.SubmitAction(kHeroB, Command(BATTLE_ACTION_SKILL, kEnemy, 16002));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();

    const auto sA = engine.BuildSettlement(kHeroA);
    ASSERT_TRUE(sA.has_trial_facts());
    const auto& fA = sA.trial_facts();
    EXPECT_EQ(fA.journal_entry_id(), 104u);
    EXPECT_EQ(fA.trial_rule_version(), 1u);
    EXPECT_EQ(fA.season_id(), 1u);
    EXPECT_EQ(fA.initial_human_count(), 2u);
    EXPECT_FALSE(fA.self_or_pet_ever_died());
    EXPECT_TRUE(fA.team_dealt_physical_direct_damage());
    EXPECT_TRUE(fA.team_dealt_magic_direct_damage());
    EXPECT_TRUE(fA.self_triggered_shield_break());
    EXPECT_TRUE(fA.self_triggered_anti_control_or_season_skill());
    EXPECT_EQ(fA.used_pet_table_id(), 2005u);
    ASSERT_EQ(fA.used_artifact_effect_ids_size(), 2);
    EXPECT_EQ(fA.used_artifact_effect_ids(0), 1u);
    EXPECT_EQ(fA.used_artifact_effect_ids(1), 8u);
    EXPECT_EQ(fA.used_season_seal_id(), 101u);
    ASSERT_EQ(fA.teammate_names_size(), 1);
    EXPECT_EQ(fA.teammate_names(0), "道友乙");
}

TEST_F(TrialJournalBattleTest, DeathAndReviveStillMarksEverDied) {
    auto* ctx = request.mutable_trial_context();
    ctx->set_journal_entry_id(101);
    ctx->set_trial_rule_version(1);

    AddModernDamageSkill(16101, 1, 5000);

    auto& hero = AddPlayer(kHeroA, 0, "道友", 200, 100);
    AddPlayer(kHeroB, 0, "队友", 10000, 90); // 队友存活保证回合走到回合末触发回生印复活
    // 挂回生印：回合末死亡后自动复活
    auto& reviveRow = data->AddBuff(19902);
    reviveRow.Clear();
    reviveRow.set_id(19902);
    reviveRow.set_duration(12.0);
    reviveRow.set_keep_on_death(1);
    reviveRow.set_battle_op(static_cast<uint32_t>(BattleEffectOp::ReviveMark));
    reviveRow.set_tick_count(2);
    reviveRow.set_duration_rounds(2);
    reviveRow.set_secondary_ratio(0.5);
    auto* reviveEntry = hero.add_buffs();
    reviveEntry->set_buff_id(19902);
    reviveEntry->set_buff_table_id(19902);
    reviveEntry->set_caster_id(kHeroA);
    reviveEntry->set_layer(1);
    reviveEntry->set_remaining_ticks(2);
    reviveEntry->set_remain_rounds(2);
    reviveEntry->set_tick_round(1);

    auto& enemy = AddPlayer(kEnemy, 1, "强敌", 10000, 300);
    enemy.add_skill_table_ids(16101);
    (*enemy.mutable_skill_ranks())[16101] = 1;

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kHeroB, Command(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_SKILL, kHeroA, 16101));
    engine.ResolveCurrentRound();

    const auto s = engine.BuildSettlement(kHeroA);
    EXPECT_FALSE(s.is_dead()) << "已被回生印拉起";
    ASSERT_TRUE(s.has_trial_facts());
    EXPECT_TRUE(s.trial_facts().self_or_pet_ever_died()) << "曾经倒地即使复活也不得算无损";
}

TEST_F(TrialJournalBattleTest, TracksGuardHealOnBossAndGuardStanceDispelInPve) {
    request.set_match_mode(turnbattle::kMatchModePveSolo);
    request.set_battle_config_id(8801);
    auto* ctx = request.mutable_trial_context();
    ctx->set_journal_entry_id(102);
    ctx->set_trial_rule_version(1);
    ctx->set_guard_monster_table_id(9102);
    ctx->set_guard_stance_buff_id(19910);

    auto& dungeon = data->AddDungeon(8801);
    dungeon.Clear();
    dungeon.set_id(8801);
    data->SetDungeonMonsters(8801, {9101, 9102}); // 9101=首领, 9102=回春/护势护卫
    data->AddBattleReward(8801, "");

    auto& boss = data->AddMonster(9101);
    boss.Clear();
    boss.set_id(9101);
    boss.set_is_boss(1);
    boss.set_health(5000);
    boss.set_speed(50);

    auto& guard = data->AddMonster(9102);
    guard.Clear();
    guard.set_id(9102);
    guard.set_is_boss(0);
    guard.set_health(5000);
    guard.set_speed(150);

    // 玩家技能：驱散敌方增益（16201）
    auto& dispelSkill = data->AddSkill(16201);
    dispelSkill.Clear();
    dispelSkill.set_id(16201);
    dispelSkill.set_battle_model(1);
    dispelSkill.set_caster_mask(1);
    dispelSkill.set_ability_kind(1);
    dispelSkill.set_unlock_level(1);
    dispelSkill.set_max_rank(10);
    dispelSkill.set_target_team(1);
    dispelSkill.set_max_targets(1);
    dispelSkill.add_skill_type(1);
    dispelSkill.set_mana_percent(0.01);
    dispelSkill.add_effect(19911);
    auto& dispelBuff = data->AddBuff(19911);
    dispelBuff.Clear();
    dispelBuff.set_id(19911);
    dispelBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::DispelBuff));
    dispelBuff.set_effect_count(1);

    // 护势增益（19910）与回春治疗（19912），通过怪物剧本现代零耗蓝技能 16202 施放给首领
    auto& stanceBuff = data->AddBuff(19910);
    stanceBuff.Clear();
    stanceBuff.set_id(19910);
    stanceBuff.set_duration(30.0);
    stanceBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Modifier));
    stanceBuff.set_duration_rounds(5);
    stanceBuff.set_ratio(0.2);

    auto& healBuff = data->AddBuff(19912);
    healBuff.Clear();
    healBuff.set_id(19912);
    healBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Heal));
    healBuff.set_flat_value(300);

    auto& guardHealSkill = data->AddSkill(16202);
    guardHealSkill.Clear();
    guardHealSkill.set_id(16202);
    guardHealSkill.set_battle_model(1);
    guardHealSkill.set_caster_mask(1);
    guardHealSkill.set_ability_kind(1);
    guardHealSkill.set_unlock_level(1);
    guardHealSkill.set_max_rank(10);
    guardHealSkill.set_target_team(2);
    guardHealSkill.set_max_targets(1);
    guardHealSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    guardHealSkill.set_mana_percent(0.0);
    guardHealSkill.add_effect(19912);
    guardHealSkill.add_effect(19910);

    // 给怪物方挂官方剧本：9102 对血量最低友方（被打伤的首领）施放 16202
    BattleScript script;
    script.set_schema_version(turnbattle::kBattleScriptSchemaVersion);
    script.set_script_id(1);
    script.set_name("guard_support");
    auto* role = script.add_roles();
    role->set_role_id(1);
    role->set_order(1);
    role->set_actor_type(BATTLE_ACTOR_TYPE_MONSTER);
    role->set_monster_id(9102);
    auto* rule = role->add_rules();
    rule->set_priority(1);
    rule->set_action(BATTLE_ACTION_SKILL);
    rule->set_skill_id(16202);
    rule->mutable_target()->set_side(BATTLE_SCRIPT_TARGET_ALLY);
    rule->mutable_target()->set_order(BATTLE_SCRIPT_ORDER_HP_LOWEST);
    auto* teamScript = request.add_team_scripts();
    teamScript->set_team_index(1);
    *teamScript->mutable_script() = script;
    teamScript->set_origin(BATTLE_SCRIPT_ORIGIN_OFFICIAL);

    auto& hero = AddPlayer(kHeroA, 0, "剑修", 10000, 300);
    hero.add_skill_table_ids(16201);
    (*hero.mutable_skill_ranks())[16201] = 1;

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto initState = engine.BuildStateSnapshot();
    uint64_t bossActorId = 0;
    for (const auto& a : initState.actors()) {
        if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) {
            bossActorId = a.actor_id();
        }
    }
    ASSERT_NE(bossActorId, 0u);

    // 第 1 回合：玩家普攻打伤首领 → 护卫治疗首领并挂护势 19910
    engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_ATTACK, bossActorId));
    engine.ResolveCurrentRound();

    // 第 2 回合：玩家对首领施放驱散 16201，驱掉护势 19910
    engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_SKILL, bossActorId, 16201));
    engine.ResolveCurrentRound();

    const auto settlement = engine.BuildSettlement(kHeroA);
    ASSERT_TRUE(settlement.has_trial_facts());
    EXPECT_GT(settlement.trial_facts().boss_guard_heal_total(), 0u);
    EXPECT_TRUE(settlement.trial_facts().guard_stance_dispelled());
    EXPECT_TRUE(settlement.trial_facts().guard_survived());
    EXPECT_TRUE(settlement.trial_facts().team_dealt_physical_direct_damage());
}

TEST_F(TrialJournalBattleTest, TracksBossTrialRejuvHealAndBreakStanceDispelWithMatchTrialContext) {
    // 1. 首领试炼·困难回春（Dungeon 102 → Entry 103, guard=102）：R1 预告，R2 慢速回春治疗首领必须记入 boss_guard_heal_total。
    {
        CreateBattleRequest rejuvReq;
        rejuvReq.set_battle_id(92011);
        rejuvReq.set_match_mode(turnbattle::kMatchModePveSolo);
        rejuvReq.set_battle_config_id(102);
        rejuvReq.set_seed(17);
        auto* ctx = rejuvReq.mutable_trial_context();
        ctx->set_journal_entry_id(103);
        ctx->set_trial_rule_version(1);
        ctx->set_guard_monster_table_id(102);

        auto& d102 = data->AddDungeon(102);
        d102.Clear();
        d102.set_id(102);
        d102.set_boss_trial_kind(turnbattle::kBossTrialKindBreakRejuv);
        d102.set_difficulty(2);
        d102.set_round_limit(15);
        data->SetDungeonMonsters(102, {101, 102});
        data->AddBattleReward(102, "");

        auto& mBoss = data->AddMonster(101);
        mBoss.Clear();
        mBoss.set_id(101);
        mBoss.set_is_boss(1);
        mBoss.set_health(8000);
        mBoss.set_speed(50);
        auto& mGuard = data->AddMonster(102);
        mGuard.Clear();
        mGuard.set_id(102);
        mGuard.set_is_boss(0);
        mGuard.set_health(4000);
        mGuard.set_speed(60);

        auto* p = rejuvReq.add_players();
        p->set_player_id(kHeroA);
        p->set_player_name("道友");
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(20000);
        p->set_max_mana(10000);
        p->mutable_base_attributes()->set_health(20000);
        p->mutable_base_attributes()->set_mana(10000);
        p->mutable_base_attributes()->set_speed(300);
        p->set_physical_attack(1500);
        p->set_defense(200);

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(rejuvReq));
        uint64_t bossActorId = 0;
        for (const auto& a : engine.BuildStateSnapshot().actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) {
                bossActorId = a.actor_id();
            }
        }
        ASSERT_NE(bossActorId, 0u);

        // R1 打伤首领，R2 防御等困难回春护卫在慢速阶段给首领回血
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_ATTACK, bossActorId));
        engine.ResolveCurrentRound();
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();

        const auto s = engine.BuildSettlement(kHeroA);
        ASSERT_TRUE(s.has_trial_facts());
        EXPECT_EQ(s.trial_facts().journal_entry_id(), 103u);
        EXPECT_GT(s.trial_facts().boss_guard_heal_total(), 0u);
    }

    // 2. 首领试炼·普通护势（Dungeon 103 → Entry 102, guard=103, guard_stance_buff_id=0）：R1 上护势，R2 驱散必须置位 guard_stance_dispelled。
    {
        CreateBattleRequest stanceReq;
        stanceReq.set_battle_id(92012);
        stanceReq.set_match_mode(turnbattle::kMatchModePveSolo);
        stanceReq.set_battle_config_id(103);
        stanceReq.set_seed(19);
        auto* ctx = stanceReq.mutable_trial_context();
        ctx->set_journal_entry_id(102);
        ctx->set_trial_rule_version(1);
        ctx->set_guard_monster_table_id(103);

        auto& d103 = data->AddDungeon(103);
        d103.Clear();
        d103.set_id(103);
        d103.set_boss_trial_kind(turnbattle::kBossTrialKindBreakStance);
        d103.set_difficulty(1);
        d103.set_round_limit(12);
        data->SetDungeonMonsters(103, {101, 103});
        data->AddBattleReward(103, "");

        auto& mGuard = data->AddMonster(103);
        mGuard.Clear();
        mGuard.set_id(103);
        mGuard.set_is_boss(0);
        mGuard.set_health(4000);
        mGuard.set_speed(60);

        auto& dispelSkill = data->AddSkill(16211);
        dispelSkill.Clear();
        dispelSkill.set_id(16211);
        dispelSkill.set_battle_model(1);
        dispelSkill.set_caster_mask(1);
        dispelSkill.set_ability_kind(1);
        dispelSkill.set_unlock_level(1);
        dispelSkill.set_max_rank(10);
        dispelSkill.set_target_team(1);
        dispelSkill.set_max_targets(1);
        dispelSkill.add_skill_type(1);
        dispelSkill.set_mana_percent(0.01);
        dispelSkill.add_effect(19921);
        auto& dispelBuff = data->AddBuff(19921);
        dispelBuff.Clear();
        dispelBuff.set_id(19921);
        dispelBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::DispelBuff));
        dispelBuff.set_effect_count(1);

        auto* p = stanceReq.add_players();
        p->set_player_id(kHeroA);
        p->set_player_name("道友");
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(20000);
        p->set_max_mana(10000);
        p->mutable_base_attributes()->set_health(20000);
        p->mutable_base_attributes()->set_mana(10000);
        p->mutable_base_attributes()->set_speed(300);
        p->set_physical_attack(1000);
        p->set_defense(200);
        p->add_skill_table_ids(16211);
        (*p->mutable_skill_ranks())[16211] = 1;

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(stanceReq));
        uint64_t bossActorId = 0;
        for (const auto& a : engine.BuildStateSnapshot().actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) {
                bossActorId = a.actor_id();
            }
        }
        ASSERT_NE(bossActorId, 0u);

        // R1 护势护卫在慢速阶段给首领施放物理护势，R2 玩家施放驱散技能破势
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_SKILL, bossActorId, 16211));
        engine.ResolveCurrentRound();

        const auto s = engine.BuildSettlement(kHeroA);
        ASSERT_TRUE(s.has_trial_facts());
        EXPECT_EQ(s.trial_facts().journal_entry_id(), 102u);
        EXPECT_TRUE(s.trial_facts().guard_stance_dispelled());
    }
}

TEST_F(TrialJournalBattleTest, TracksBarrierBreakGuardianRescueAndPolarityTeamBDualDamage) {
    // 1. 卷二 · 灵障首领 (Dungeon 105 → Entry 201)：击破灵障护盾置位 self_triggered_shield_break
    {
        CreateBattleRequest req;
        req.set_battle_id(92021);
        req.set_match_mode(turnbattle::kMatchModePveSolo);
        req.set_battle_config_id(105);
        req.set_seed(23);
        auto* ctx = req.mutable_trial_context();
        ctx->set_journal_entry_id(201);
        ctx->set_trial_rule_version(1);
        ctx->set_guard_monster_table_id(105);

        auto& d105 = data->AddDungeon(105);
        d105.Clear();
        d105.set_id(105);
        d105.set_boss_trial_kind(turnbattle::kBossTrialKindBarrier);
        d105.set_difficulty(1);
        d105.set_round_limit(12);
        data->SetDungeonMonsters(105, {104, 105});
        data->AddBattleReward(105, "");

        auto& boss = data->AddMonster(104);
        boss.Clear();
        boss.set_id(104);
        boss.set_is_boss(1);
        boss.set_health(2000);
        boss.set_speed(50);
        auto& guard = data->AddMonster(105);
        guard.Clear();
        guard.set_id(105);
        guard.set_is_boss(0);
        guard.set_health(1500);
        guard.set_speed(60);

        auto* p = req.add_players();
        p->set_player_id(kHeroA);
        p->set_player_name("破障客");
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(20000);
        p->set_max_mana(10000);
        p->mutable_base_attributes()->set_health(20000);
        p->mutable_base_attributes()->set_mana(10000);
        p->mutable_base_attributes()->set_speed(300);
        p->set_physical_attack(5000);
        p->set_defense(200);

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        uint64_t bossActorId = 0;
        for (const auto& a : engine.BuildStateSnapshot().actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) bossActorId = a.actor_id();
        }
        ASSERT_NE(bossActorId, 0u);
        // R1 灵障护卫在慢速阶段给首领附加 12% 护盾，R2 玩家重击直接击破灵障护盾
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_ATTACK, bossActorId));
        engine.ResolveCurrentRound();

        const auto s = engine.BuildSettlement(kHeroA);
        ASSERT_TRUE(s.has_trial_facts());
        EXPECT_EQ(s.trial_facts().journal_entry_id(), 201u);
        EXPECT_TRUE(s.trial_facts().self_triggered_shield_break());
    }

    // 2. 卷三 · 护卫救援 (Dungeon 109 → Entry 301)：击败首领且我方受保护护卫存活时 guard_survived == true
    {
        CreateBattleRequest req;
        req.set_battle_id(92022);
        req.set_match_mode(turnbattle::kMatchModePveSolo);
        req.set_battle_config_id(109);
        req.set_seed(29);
        auto* ctx = req.mutable_trial_context();
        ctx->set_journal_entry_id(301);
        ctx->set_trial_rule_version(1);
        ctx->set_season_id(1);
        ctx->set_guard_monster_table_id(102);

        auto& d109 = data->AddDungeon(109);
        d109.Clear();
        d109.set_id(109);
        d109.set_boss_trial_kind(turnbattle::kBossTrialKindGuardianRescue);
        d109.set_difficulty(1);
        d109.set_round_limit(8);
        data->SetDungeonMonsters(109, {101, 102});
        data->AddBattleReward(109, "");

        auto& boss = data->AddMonster(101);
        boss.Clear();
        boss.set_id(101);
        boss.set_is_boss(1);
        boss.set_health(500);
        boss.set_speed(50);
        auto& guard = data->AddMonster(102);
        guard.Clear();
        guard.set_id(102);
        guard.set_is_boss(0);
        guard.set_health(5000);
        guard.set_speed(60);

        auto* p = req.add_players();
        p->set_player_id(kHeroA);
        p->set_player_name("救援者");
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(20000);
        p->set_max_mana(10000);
        p->mutable_base_attributes()->set_health(20000);
        p->mutable_base_attributes()->set_mana(10000);
        p->mutable_base_attributes()->set_speed(300);
        p->set_physical_attack(5000);
        p->set_defense(200);

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        uint64_t bossActorId = 0;
        for (const auto& a : engine.BuildStateSnapshot().actors()) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.is_boss()) bossActorId = a.actor_id();
        }
        ASSERT_NE(bossActorId, 0u);
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_ATTACK, bossActorId));
        engine.ResolveCurrentRound();

        const auto s = engine.BuildSettlement(kHeroA);
        EXPECT_EQ(s.outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
        ASSERT_TRUE(s.has_trial_facts());
        EXPECT_EQ(s.trial_facts().journal_entry_id(), 301u);
        EXPECT_TRUE(s.trial_facts().guard_survived());
    }

    // 3. 卷三 · 物法易势 5V5 (Entry 302)：B 队造成物理+法术双系直伤必须准确写入 B 队结算而非仅限 A 队
    {
        AddModernDamageSkill(16301, combatdamage::kMagicDamage, 600);
        CreateBattleRequest req;
        req.set_battle_id(92023);
        req.set_match_mode(turnbattle::kMatchModePolarity5v5);
        req.set_battle_config_id(110);
        req.set_seed(31);
        auto* ctx = req.mutable_trial_context();
        ctx->set_journal_entry_id(302);
        ctx->set_trial_rule_version(1);
        ctx->set_season_id(1);

        auto& d110 = data->AddDungeon(110);
        d110.Clear();
        d110.set_id(110);
        d110.set_boss_trial_kind(turnbattle::kBossTrialKindPolarity5v5);
        d110.set_round_limit(16);

        auto addMember = [&](uint64_t id, uint32_t team, uint64_t speed) {
            auto* p = req.add_players();
            p->set_player_id(id);
            p->set_player_name("易势选手");
            p->set_team_index(team);
            p->set_level(30);
            p->set_max_health(20000);
            p->set_max_mana(10000);
            p->mutable_base_attributes()->set_health(20000);
            p->mutable_base_attributes()->set_mana(10000);
            p->mutable_base_attributes()->set_speed(speed);
            p->set_physical_attack(1200);
            p->set_magic_attack(1200);
            p->set_defense(100);
            p->add_skill_table_ids(16301);
            (*p->mutable_skill_ranks())[16301] = 1;
        };
        addMember(kHeroA, 0, 100);
        addMember(kEnemy, 1, 300);
        addMember(kEnemy + 1, 1, 250);

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
        engine.SubmitAction(kEnemy, Command(BATTLE_ACTION_ATTACK, kHeroA));
        engine.SubmitAction(kEnemy + 1, Command(BATTLE_ACTION_SKILL, kHeroA, 16301));
        engine.ResolveCurrentRound();

        const auto sA = engine.BuildSettlement(kHeroA);
        const auto sB = engine.BuildSettlement(kEnemy);
        ASSERT_TRUE(sA.has_trial_facts());
        ASSERT_TRUE(sB.has_trial_facts());
        EXPECT_FALSE(sA.trial_facts().team_dealt_physical_direct_damage());
        EXPECT_FALSE(sA.trial_facts().team_dealt_magic_direct_damage());
        EXPECT_TRUE(sB.trial_facts().team_dealt_physical_direct_damage());
        EXPECT_TRUE(sB.trial_facts().team_dealt_magic_direct_damage());
    }
}

TEST_F(TrialJournalBattleTest, Vol4WildBossMechanicsAndTrialVows) {
    // 1. 试炼誓约校验：非法掩码(>0x0F) 与非首领试炼携带誓约一律拒绝开局。
    {
        auto& d113 = data->AddDungeon(113);
        d113.Clear();
        d113.set_id(113);
        d113.set_boss_trial_kind(turnbattle::kBossTrialKindWildWind);
        d113.set_difficulty(1);
        d113.set_round_limit(12);
        data->SetDungeonMonsters(113, {111, 112});
        data->AddBattleReward(113, "");

        auto& m111 = data->AddMonster(111);
        m111.Clear();
        m111.set_id(111);
        m111.set_is_boss(1);
        m111.set_health(2000);
        m111.set_strength(200);
        m111.set_speed(80);
        auto& m112 = data->AddMonster(112);
        m112.Clear();
        m112.set_id(112);
        m112.set_is_boss(0);
        m112.set_health(1000);
        m112.set_speed(90);

        CreateBattleRequest badMaskReq;
        badMaskReq.set_battle_id(93001);
        badMaskReq.set_match_mode(turnbattle::kMatchModePveSolo);
        badMaskReq.set_battle_config_id(113);
        badMaskReq.set_seed(7);
        badMaskReq.mutable_trial_context()->set_journal_entry_id(401);
        badMaskReq.mutable_trial_context()->set_trial_rule_version(1);
        badMaskReq.mutable_trial_context()->set_challenge_vow_mask(0x10u);
        auto* p = badMaskReq.add_players();
        p->set_player_id(kHeroA);
        p->set_player_name("试炼者");
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(10000);
        p->set_max_mana(5000);
        p->mutable_base_attributes()->set_health(10000);
        p->mutable_base_attributes()->set_mana(5000);
        p->mutable_base_attributes()->set_speed(300);

        TurnBattleEngine badEngine(data);
        EXPECT_FALSE(badEngine.Initialize(badMaskReq));

        // 合法四誓约组合：无宠孤剑(不生成宝宝) + 紧缩回合(12 -> 9) + 单宝轻装 + 禁控硬破
        CreateBattleRequest vowReq = badMaskReq;
        vowReq.set_battle_id(93002);
        vowReq.mutable_trial_context()->set_challenge_vow_mask(trialjournal::kVowMaskAll);
        auto* pet = vowReq.mutable_players(0)->add_pets();
        pet->set_pet_id(77001);
        pet->set_owner_player_id(kHeroA);
        pet->set_pet_table_id(4);
        pet->set_pet_name("云鹤");
        pet->set_level(30);
        pet->set_max_health(5000);
        pet->mutable_base_attributes()->set_health(5000);
        pet->mutable_base_attributes()->set_speed(250);

        TurnBattleEngine vowEngine(data);
        ASSERT_TRUE(vowEngine.Initialize(vowReq));
        const auto snap = vowEngine.BuildStateSnapshot();
        EXPECT_EQ(snap.round_limit(), 9u);
        EXPECT_EQ(snap.trial_vow_mask(), trialjournal::kVowMaskAll);
        for (const auto& a : snap.actors()) {
            EXPECT_NE(a.actor_type(), BATTLE_ACTOR_TYPE_PET) << "无宠孤剑誓约绝不生成玩家方宝宝";
        }
        ASSERT_FALSE(snap.telegraphs().empty());
        EXPECT_EQ(snap.telegraphs(0).intent_kind(), turnbattle::kTelegraphKindWildWindGust);

        const auto settle = vowEngine.BuildSettlement(kHeroA);
        ASSERT_TRUE(settle.has_trial_facts());
        EXPECT_EQ(settle.trial_facts().challenge_vow_mask(), trialjournal::kVowMaskAll);
        EXPECT_EQ(settle.trial_facts().used_pet_table_id(), 0u);
    }

    // 2. 卷四·碧渊水宫(13)：阳钟(R1-2)物理直伤 + 阴鼓(R3-4)法术直伤精确记入双事实。
    {
        AddModernDamageSkill(16311, combatdamage::kMagicDamage, 400);
        auto& d114 = data->AddDungeon(114);
        d114.Clear();
        d114.set_id(114);
        d114.set_boss_trial_kind(turnbattle::kBossTrialKindWildBell);
        d114.set_difficulty(1);
        d114.set_round_limit(12);
        data->SetDungeonMonsters(114, {113});
        data->AddBattleReward(114, "");
        auto& m113 = data->AddMonster(113);
        m113.Clear();
        m113.set_id(113);
        m113.set_is_boss(1);
        m113.set_health(20000);
        m113.set_speed(50);

        CreateBattleRequest req;
        req.set_battle_id(93003);
        req.set_match_mode(turnbattle::kMatchModePveSolo);
        req.set_battle_config_id(114);
        req.set_seed(11);
        req.mutable_trial_context()->set_journal_entry_id(402);
        req.mutable_trial_context()->set_trial_rule_version(1);
        auto* p = req.add_players();
        p->set_player_id(kHeroA);
        p->set_player_name("听钟人");
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(20000);
        p->set_max_mana(10000);
        p->mutable_base_attributes()->set_health(20000);
        p->mutable_base_attributes()->set_mana(10000);
        p->mutable_base_attributes()->set_speed(300);
        p->set_physical_attack(500);
        p->set_magic_attack(500);
        p->add_skill_table_ids(16311);
        (*p->mutable_skill_ranks())[16311] = 1;

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        uint64_t bossId = 0;
        for (const auto& a : engine.BuildStateSnapshot().actors()) {
            if (a.is_boss()) bossId = a.actor_id();
        }
        ASSERT_NE(bossId, 0u);
        // R1 (阳钟): 物理普攻
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_ATTACK, bossId));
        engine.ResolveCurrentRound();
        // R2 (阳钟): 防御过渡
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
        // R3 (阴鼓): 法术直伤
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_SKILL, bossId, 16311));
        engine.ResolveCurrentRound();

        const auto settle = engine.BuildSettlement(kHeroA);
        ASSERT_TRUE(settle.has_trial_facts());
        EXPECT_TRUE(settle.trial_facts().dealt_yang_bell_physical_damage());
        EXPECT_TRUE(settle.trial_facts().dealt_yin_drum_magic_damage());
    }

    // 3. 卷四·月汐湾(14)：R3 退潮窗口击杀首领置位 finished_in_ebb_tide。
    {
        auto& d115 = data->AddDungeon(115);
        d115.Clear();
        d115.set_id(115);
        d115.set_boss_trial_kind(turnbattle::kBossTrialKindWildTide);
        d115.set_difficulty(1);
        d115.set_round_limit(12);
        data->SetDungeonMonsters(115, {115});
        data->AddBattleReward(115, "");
        auto& m115 = data->AddMonster(115);
        m115.Clear();
        m115.set_id(115);
        m115.set_is_boss(1);
        m115.set_health(500);
        m115.set_speed(50);

        CreateBattleRequest req;
        req.set_battle_id(93004);
        req.set_match_mode(turnbattle::kMatchModePveSolo);
        req.set_battle_config_id(115);
        req.set_seed(13);
        req.mutable_trial_context()->set_journal_entry_id(403);
        req.mutable_trial_context()->set_trial_rule_version(1);
        auto* p = req.add_players();
        p->set_player_id(kHeroA);
        p->set_player_name("赶潮人");
        p->set_team_index(0);
        p->set_level(30);
        p->set_max_health(20000);
        p->set_max_mana(10000);
        p->mutable_base_attributes()->set_health(20000);
        p->mutable_base_attributes()->set_mana(10000);
        p->mutable_base_attributes()->set_speed(300);
        p->set_physical_attack(5000);

        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(req));
        uint64_t bossId = 0;
        for (const auto& a : engine.BuildStateSnapshot().actors()) {
            if (a.is_boss()) bossId = a.actor_id();
        }
        ASSERT_NE(bossId, 0u);
        // R1/R2 涨潮期防御等待，R3 退潮一击收官
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
        engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_ATTACK, bossId));
        engine.ResolveCurrentRound();

        const auto settle = engine.BuildSettlement(kHeroA);
        EXPECT_EQ(settle.outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
        ASSERT_TRUE(settle.has_trial_facts());
        EXPECT_TRUE(settle.trial_facts().finished_in_ebb_tide());
    }
}

TEST_F(TrialJournalBattleTest, TracksSeasonSkillCastAndResonanceMaskForTrialV4) {
    // 构造 S2 玄脉之季：赛季技 15103（护盾）+ 玄脉护印（effect=5 首盾增容，resonance_kind=6 首盾回蓝）
    auto& s15103 = data->AddSkill(15103);
    s15103.Clear();
    s15103.set_id(15103);
    s15103.set_season_id(2);
    s15103.set_battle_model(1);
    s15103.set_ability_kind(1);
    s15103.set_caster_mask(1);
    s15103.set_unlock_level(1);
    s15103.set_max_rank(10);
    s15103.set_target_team(2);
    s15103.set_max_targets(1);
    s15103.set_pet_max_targets(1);
    s15103.set_mana_percent(0.05);
    s15103.add_effect(151030);
    auto& b151030 = data->AddBuff(151030);
    b151030.Clear();
    b151030.set_id(151030);
    b151030.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
    b151030.set_flat_value(300);
    b151030.set_duration_rounds(2);
    b151030.set_max_layer(1);
    b151030.set_infinite_duration(1);

    CreateBattleRequest req;
    req.set_battle_id(94001);
    req.set_match_mode(3); // PVP
    req.set_seed(29);
    req.mutable_trial_context()->set_journal_entry_id(3023);
    req.mutable_trial_context()->set_trial_rule_version(1);
    req.mutable_trial_context()->set_season_id(2);

    auto* pA = req.add_players();
    pA->set_player_id(kHeroA);
    pA->set_player_name("玄脉试剑");
    pA->set_team_index(0);
    pA->set_active_season_id(2);
    pA->set_level(30);
    pA->set_max_health(5000);
    pA->set_max_mana(2000);
    pA->mutable_base_attributes()->set_health(5000);
    pA->mutable_base_attributes()->set_mana(1000); // 留出法力缺口以触发 R6 首盾回蓝共鸣
    pA->mutable_base_attributes()->set_speed(300);
    pA->set_physical_attack(500);
    pA->add_skill_table_ids(15103);
    (*pA->mutable_skill_ranks())[15103] = 1;
    auto* seal = pA->mutable_equipment_effects()->mutable_season_seal();
    seal->set_season_id(2);
    seal->set_seal_id(203);
    seal->set_effect_id(5);
    seal->set_bonus_bps(1200);
    seal->set_per_round_limit(1);
    seal->set_battle_limit(3);
    seal->set_resonance_skill_id(15103);
    seal->set_resonance_kind(6);
    seal->set_resonance_param(250);

    auto* pB = req.add_players();
    pB->set_player_id(kHeroB);
    pB->set_player_name("对手");
    pB->set_team_index(1);
    pB->set_level(30);
    pB->set_max_health(5000);
    pB->set_max_mana(2000);
    pB->mutable_base_attributes()->set_health(5000);
    pB->mutable_base_attributes()->set_mana(2000);
    pB->mutable_base_attributes()->set_speed(100);
    pB->set_physical_attack(100);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_SKILL, kHeroA, 15103));
    engine.SubmitAction(kHeroB, Command(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();

    auto settle = engine.BuildSettlement(kHeroA);
    settle.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
    ASSERT_TRUE(settle.has_trial_facts());
    EXPECT_TRUE(settle.trial_facts().self_cast_season_skill());
    EXPECT_TRUE(settle.trial_facts().self_triggered_season_resonance());
    EXPECT_EQ(settle.trial_facts().self_season_resonance_mask(), (1u << 5));
    EXPECT_TRUE(trialjournal::EvaluateSealCondition(15, 0, settle));
    EXPECT_TRUE(trialjournal::EvaluateSealCondition(16, 0, settle));
    EXPECT_TRUE(trialjournal::EvaluateSealCondition(16, 51, settle)) << "S2 并集掩码 51(R1|R2|R5|R6) 应命中 R6";
    EXPECT_FALSE(trialjournal::EvaluateSealCondition(16, 21, settle)) << "S3 掩码 21(R1|R3|R5) 不含 R6 不应命中";
}

TEST_F(TrialJournalBattleTest, TacticalPuzzleWorkshopSetupScalingBuffsAndRoundLimit) {
    auto& d314 = data->AddDungeon(314);
    d314.Clear();
    d314.set_id(314);
    d314.set_boss_trial_kind(turnbattle::kBossTrialKindTacticalPuzzle);
    d314.set_difficulty(1);
    d314.set_round_limit(6);
    data->SetDungeonMonsters(314, {311});
    data->AddBattleReward(314, "");

    auto& m311 = data->AddMonster(311);
    m311.Clear();
    m311.set_id(311);
    m311.set_health(1000);
    m311.set_speed(100);
    m311.set_physical_attack(10);

    auto& m312 = data->AddMonster(312);
    m312.Clear();
    m312.set_id(312);
    m312.set_health(1000);
    m312.set_speed(100);
    m312.set_physical_attack(10);

    auto& bShield = data->AddBuff(turnbattle::kTacticalPuzzleShieldBuffId);
    bShield.Clear();
    bShield.set_id(turnbattle::kTacticalPuzzleShieldBuffId);
    bShield.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
    bShield.set_duration_rounds(2);

    auto& bPoison = data->AddBuff(turnbattle::kTacticalPuzzleDebuffPoisonBuffId);
    bPoison.Clear();
    bPoison.set_id(turnbattle::kTacticalPuzzleDebuffPoisonBuffId);
    bPoison.set_battle_op(static_cast<uint32_t>(BattleEffectOp::PeriodicDamage));
    bPoison.set_duration_rounds(3);
    bPoison.set_tick_count(3);
    bPoison.set_tick_on_apply(1);

    auto& bDefUp = data->AddBuff(turnbattle::kTacticalPuzzleBuffDefUpBuffId);
    bDefUp.Clear();
    bDefUp.set_id(turnbattle::kTacticalPuzzleBuffDefUpBuffId);
    bDefUp.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Modifier));
    bDefUp.set_modifier_stat(static_cast<uint32_t>(BattleModifier::Defense));
    bDefUp.set_ratio(0.25);
    bDefUp.set_duration_rounds(5);

    auto& bReflect = data->AddBuff(turnbattle::kTacticalPuzzleBuffReflectBuffId);
    bReflect.Clear();
    bReflect.set_id(turnbattle::kTacticalPuzzleBuffReflectBuffId);
    bReflect.set_battle_op(static_cast<uint32_t>(BattleEffectOp::ReflectDamage));
    bReflect.set_damage_filter(1);
    bReflect.set_secondary_ratio(0.25);
    bReflect.set_duration_rounds(3);

    CreateBattleRequest req;
    req.set_battle_id(95001);
    req.set_match_mode(turnbattle::kMatchModePveSolo);
    req.set_battle_config_id(314);
    req.set_seed(777);
    req.mutable_ranked_context()->set_use_free_preset(true);
    req.mutable_ranked_context()->set_team_size(1);

    auto* p = req.add_players();
    p->set_player_id(kHeroA);
    p->set_player_name("破局者");
    p->set_team_index(0);
    p->set_level(40);
    p->set_max_health(2000);
    p->set_max_mana(1000);
    p->mutable_base_attributes()->set_health(2000);
    p->mutable_base_attributes()->set_mana(1000);
    p->mutable_base_attributes()->set_speed(200);
    p->set_physical_attack(100);

    auto* ctx = req.mutable_tactical_puzzle_context();
    ctx->set_puzzle_id(88001);
    ctx->set_spec_revision(2);
    ctx->set_is_author_verify(true);
    ctx->set_puzzle_title("二回合破盾反杀");
    ctx->set_author_player_id(kHeroA);
    ctx->set_author_name("破局者");
    auto* spec = ctx->mutable_spec();
    spec->set_round_limit(2);
    spec->set_character_preset_id(101);
    spec->set_pet_preset_id(0);
    spec->set_formation_id(3);
    spec->set_player_initial_hp_pct(50);
    spec->set_player_initial_mp_pct(60);
    spec->set_player_initial_shield_pct(20);
    spec->set_player_initial_debuff_kind(3);

    auto* e1 = spec->add_enemies();
    e1->set_monster_table_id(311);
    e1->set_initial_hp_pct(40);
    e1->set_initial_shield_pct(30);
    e1->set_speed_delta_pct(-20);
    e1->set_initial_buff_kind(1);

    auto* e2 = spec->add_enemies();
    e2->set_monster_table_id(312);
    e2->set_initial_hp_pct(120);
    e2->set_initial_shield_pct(0);
    e2->set_speed_delta_pct(20);
    e2->set_initial_buff_kind(4);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    EXPECT_TRUE(engine.IsTacticalPuzzleWorkshopBattle());
    EXPECT_EQ(engine.MaxRounds(), 2u);
    const auto state = engine.BuildStateSnapshot();
    EXPECT_EQ(state.round_limit(), 2u);
    EXPECT_EQ(state.team_a_formation_id(), 3u);
    ASSERT_TRUE(state.has_tactical_puzzle_context());
    EXPECT_EQ(state.tactical_puzzle_context().puzzle_id(), 88001u);
    EXPECT_TRUE(state.tactical_puzzle_context().is_author_verify());
    ASSERT_EQ(state.actors_size(), 3);

    const BattleActorState* hero = nullptr;
    const BattleActorState* mon1 = nullptr;
    const BattleActorState* mon2 = nullptr;
    for (const auto& a : state.actors()) {
        if (a.actor_id() == kHeroA) hero = &a;
        else if (a.monster_table_id() == 311u) mon1 = &a;
        else if (a.monster_table_id() == 312u) mon2 = &a;
    }
    ASSERT_NE(hero, nullptr);
    ASSERT_NE(mon1, nullptr);
    ASSERT_NE(mon2, nullptr);
    EXPECT_EQ(hero->attributes().health(), 1000u);
    EXPECT_EQ(hero->attributes().mana(), 600u);
    EXPECT_EQ(mon1->max_health(), 1000u);
    EXPECT_EQ(mon1->attributes().health(), 400u);
    EXPECT_EQ(mon1->attributes().speed(), 80u);
    EXPECT_EQ(mon2->max_health(), 1200u);
    EXPECT_EQ(mon2->attributes().health(), 1200u);
    EXPECT_EQ(mon2->attributes().speed(), 120u);

    bool heroHasShield = false, heroHasPoison = false;
    for (const auto& b : hero->buffs()) {
        if (b.buff_table_id() == turnbattle::kTacticalPuzzleShieldBuffId && b.remaining_capacity() == 400u) heroHasShield = true;
        if (b.buff_table_id() == turnbattle::kTacticalPuzzleDebuffPoisonBuffId) heroHasPoison = true;
    }
    EXPECT_TRUE(heroHasShield);
    EXPECT_TRUE(heroHasPoison);

    bool mon1HasShield = false, mon1HasDefUp = false;
    for (const auto& b : mon1->buffs()) {
        if (b.buff_table_id() == turnbattle::kTacticalPuzzleShieldBuffId && b.remaining_capacity() == 300u) mon1HasShield = true;
        if (b.buff_table_id() == turnbattle::kTacticalPuzzleBuffDefUpBuffId) mon1HasDefUp = true;
    }
    EXPECT_TRUE(mon1HasShield);
    EXPECT_TRUE(mon1HasDefUp);

    bool mon2HasReflect = false;
    for (const auto& b : mon2->buffs()) {
        if (b.buff_table_id() == turnbattle::kTacticalPuzzleBuffReflectBuffId) mon2HasReflect = true;
    }
    EXPECT_TRUE(mon2HasReflect);

    engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);
    engine.SubmitAction(kHeroA, Command(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);
    const auto settle = engine.BuildSettlement(kHeroA);
    ASSERT_TRUE(settle.has_tactical_puzzle_context());
    EXPECT_EQ(settle.tactical_puzzle_context().puzzle_id(), 88001u);
    EXPECT_EQ(settle.exp_gain(), 0u);
    EXPECT_EQ(settle.gold_gain(), 0u);

    CreateBattleRequest badRound = req;
    badRound.mutable_tactical_puzzle_context()->mutable_spec()->set_round_limit(7);
    TurnBattleEngine badEngine1(data);
    EXPECT_FALSE(badEngine1.Initialize(badRound));

    CreateBattleRequest badMonster = req;
    badMonster.mutable_tactical_puzzle_context()->mutable_spec()->mutable_enemies(0)->set_monster_table_id(101);
    TurnBattleEngine badEngine2(data);
    EXPECT_FALSE(badEngine2.Initialize(badMonster));

    CreateBattleRequest noPreset = req;
    noPreset.mutable_ranked_context()->set_use_free_preset(false);
    TurnBattleEngine badEngine3(data);
    EXPECT_FALSE(badEngine3.Initialize(noPreset));
}
