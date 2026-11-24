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
