#include <gtest/gtest.h>
#include <algorithm>
#include <cmath>
#include <memory>
#include <random>
#include <vector>
#include "memory_battle_data_provider.h"
#include "system/turn_battle_engine.h"
#include "system/equipment_battle_rules.h"
#include "system/combat_damage_rules.h"

namespace {
using turnbattle::TurnBattleEngine;
using turnbattle::BattleEffectOp;
using turnbattle::BattleModifier;
constexpr uint64_t kSelf = 101, kAlly = 102, kEnemy = 201, kOtherEnemy = 202;
BattleAction Command(eBattleActionType type, uint64_t target = 0, uint32_t skill = 0) {
    BattleAction action; action.set_action_type(type); action.set_target_id(target); action.set_skill_table_id(skill); return action;
}
const BattleActorState& Unit(const BattleStateS2C& state, uint64_t id) {
    for (const auto& actor : state.actors()) if (actor.actor_id() == id) return actor;
    ADD_FAILURE() << "Missing actor " << id; return BattleActorState::default_instance();
}
uint64_t EventSum(const TurnResultS2C& result, eBattleEventType type, uint64_t source, uint64_t target) {
    uint64_t sum = 0;
    for (const auto& event : result.events()) if (event.event_type() == type && event.source_id() == source && event.target_id() == target) sum += event.value();
    return sum;
}
uint32_t Uses(const BattleStateS2C& state, uint64_t actor, uint64_t item) {
    for (const auto& entry : Unit(state, actor).artifact_states()) if (entry.item_id() == item) return entry.used_count();
    ADD_FAILURE() << "Missing artifact " << item; return 0;
}
uint64_t Capacity(const BattleStateS2C& state, uint64_t actor, uint32_t buff) {
    for (const auto& entry : Unit(state, actor).buffs()) if (entry.buff_table_id() == buff) return entry.remaining_capacity();
    return 0;
}
class EquipmentV1BattleTest : public ::testing::Test {
protected:
    void SetUp() override {
        data = std::make_shared<MemoryBattleDataProvider>();
        request.set_battle_id(70801); request.set_match_mode(3); request.set_seed(2);
        for (const uint64_t id : {kSelf, kAlly, kEnemy, kOtherEnemy}) {
            auto* player = request.add_players(); player->set_player_id(id); player->set_team_index(id < 200 ? 0 : 1);
            player->set_level(30); player->set_max_health(10000); player->set_max_mana(10000);
            player->mutable_base_attributes()->set_health(10000); player->mutable_base_attributes()->set_mana(10000);
            player->mutable_base_attributes()->set_speed(id == kSelf ? 400 : id == kAlly ? 300 : id == kEnemy ? 200 : 100);
        }
    }
    BattlePlayerSnapshot& Player(uint64_t id) {
        for (auto& player : *request.mutable_players()) if (player.player_id() == id) return player;
        return *request.mutable_players(0);
    }
    SkillTable& Skill(uint32_t id, BattleEffectOp op, uint32_t targetTeam = 1, uint32_t count = 1) {
        auto& row = data->AddSkill(id); row.Clear(); row.set_id(id); row.set_battle_model(1); row.set_caster_mask(3); row.set_ability_kind(1);
        row.set_unlock_level(1); row.set_max_rank(10); row.set_target_team(targetTeam); row.set_max_targets(count);
        row.set_pet_max_targets(1); row.add_skill_type(1); row.set_mana_percent(.1); row.add_effect(id * 10);
        auto& buff = data->AddBuff(id * 10); buff.Clear(); buff.set_id(id * 10); buff.set_battle_op(static_cast<uint32_t>(op)); buff.set_flat_value(1000);
        return row;
    }
    void Learn(uint64_t actor, uint32_t id) {
        Player(actor).add_skill_table_ids(id); (*Player(actor).mutable_skill_ranks())[id] = 1;
    }
    void Artifact(uint64_t actor, uint32_t effect, uint64_t item) {
        auto* artifact = Player(actor).mutable_equipment_effects()->add_artifacts();
        artifact->set_item_id(item); artifact->set_effect_id(effect); artifact->set_per_round_limit(1);
        switch (effect) {
        case 1: case 2: artifact->set_bonus_bps(2000); break;
        case 3: artifact->set_bonus_bps(600); artifact->set_battle_limit(2); break;
        case 4: artifact->set_bonus_bps(1200); artifact->set_health_threshold_bps(3000); break;
        case 5: artifact->set_bonus_bps(1200); break;
        case 6: artifact->set_bonus_bps(2000); artifact->set_cap_bps(600); artifact->set_active_rounds(2);
            artifact->set_per_round_limit(0); artifact->set_battle_limit(1); break;
        case 7: artifact->set_bonus_bps(500); artifact->set_battle_limit(3); break;
        case 8: artifact->set_bonus_bps(800); artifact->set_battle_limit(2); break;
        }
    }
    void SnapshotBuff(uint64_t actor, uint32_t id, uint64_t capacity = 0, uint64_t caster = 0) {
        auto& row = data->AddBuff(id); row.set_infinite_duration(1);
        auto* entry = Player(actor).add_buffs(); entry->set_buff_id(id); entry->set_buff_table_id(id);
        entry->set_caster_id(caster == 0 ? actor : caster); entry->set_layer(1); entry->set_remaining_capacity(capacity);
        entry->set_snapshot_value(capacity); entry->set_tick_round(999);
    }
    void Shield(uint64_t actor, uint32_t id, uint64_t capacity, uint32_t priority = 20, uint32_t filter = 0) {
        SnapshotBuff(actor, id, capacity);
        auto& row = data->AddBuff(id); row.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Shield));
        row.set_effect_priority(priority); row.set_damage_filter(filter);
    }
    void Modifier(uint64_t actor, uint32_t id, BattleModifier stat, double ratio) {
        SnapshotBuff(actor, id);
        auto& row = data->AddBuff(id); row.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Modifier));
        row.set_modifier_stat(static_cast<uint32_t>(stat)); row.set_ratio(ratio);
    }
    std::unique_ptr<TurnBattleEngine> Start() {
        auto engine = std::make_unique<TurnBattleEngine>(data); EXPECT_TRUE(engine->Initialize(request)); return engine;
    }
    TurnResultS2C Round(TurnBattleEngine& engine, uint64_t caster, const BattleAction& action) {
        const auto state = engine.BuildStateSnapshot();
        for (const auto& actor : state.actors()) if (!actor.is_dead() && !actor.fled())
            engine.SubmitAction(actor.actor_id(), actor.actor_id() == caster ? action : Command(BATTLE_ACTION_DEFEND));
        return engine.ResolveCurrentRound();
    }
    std::shared_ptr<MemoryBattleDataProvider> data;
    CreateBattleRequest request;
};
TEST(EquipmentBattleRulesTest, IntegerManaRoundingCapsAndZeroAreExplicit) {
    EXPECT_EQ(equipbattle::DiscountMana(0, 3000), 0u); EXPECT_EQ(equipbattle::DiscountMana(1, 3000), 1u);
    EXPECT_EQ(equipbattle::DiscountMana(11, 1200), 10u); EXPECT_EQ(equipbattle::DiscountMana(11, 9999), 8u);
    EXPECT_EQ(equipbattle::DiscountMana(UINT64_MAX, 3000), UINT64_MAX - equipbattle::FloorBps(UINT64_MAX, 3000));
}
TEST(EquipmentBattleRulesTest, TypedChanceClampDoesNotCreateDisabledEffects) {
    EXPECT_DOUBLE_EQ(equipbattle::AilmentChance(0, 1600, 0, 0, 0, false), 0);
    EXPECT_NEAR(equipbattle::AilmentChance(.60, 2200, 2400, 0, 0, false), .58, 1e-12);
    EXPECT_DOUBLE_EQ(equipbattle::AilmentChance(.99, 1600, 0, 0, 0, false), .85);
    EXPECT_DOUBLE_EQ(equipbattle::AilmentChance(.99, 1600, 0, 0, 0, true), .95);
    EXPECT_DOUBLE_EQ(equipbattle::AilmentChance(.1, 0, 1600, 10000, 0, false), .05);
    EXPECT_NEAR(equipbattle::ReceivedRatio(0, 0, 600, 30), .94, 1e-12);
    EXPECT_DOUBLE_EQ(equipbattle::ReceivedRatio(UINT64_MAX, UINT64_MAX, 10000, 30), .4);
}
TEST_F(EquipmentV1BattleTest, SnapshotRejectsDuplicateSourcesAndUnknownEffects) {
    Artifact(kSelf, 7, 77); Artifact(kSelf, 7, 77);
    TurnBattleEngine duplicate(data); EXPECT_FALSE(duplicate.Initialize(request));
    Player(kSelf).mutable_equipment_effects()->mutable_artifacts(1)->set_item_id(78);
    Player(kSelf).mutable_equipment_effects()->mutable_artifacts(1)->set_effect_id(9);
    TurnBattleEngine unknown(data); EXPECT_FALSE(unknown.Initialize(request));
}
TEST_F(EquipmentV1BattleTest, PanelIsNotAppliedAgainAndTypedCriticalResistanceAreAddedOnce) {
    auto& skill = Skill(900, BattleEffectOp::Damage); skill.set_damage_type(combatdamage::kPhysicalDamage);
    data->SetSkillDamage(900, 1000); Learn(kSelf, 900);
    Player(kSelf).set_physical_attack(1000); Player(kSelf).mutable_base_attributes()->set_critchance(92);
    auto* own = Player(kSelf).mutable_equipment_effects(); own->set_physical_attack_bps(3200); own->set_physical_crit_bps(800);
    Player(kEnemy).mutable_equipment_effects()->set_physical_resist_bps(600);
    auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 900));
    EXPECT_EQ(EventSum(result, BATTLE_EVENT_DAMAGE, kSelf, kEnemy), 1128u);
    bool critical = false;
    for (const auto& event : result.events()) if (event.event_type() == BATTLE_EVENT_DAMAGE && event.source_id() == kSelf) critical = event.is_critical();
    EXPECT_TRUE(critical);
}
TEST_F(EquipmentV1BattleTest, HealingHasItsOwnLayerAndOnlyPrimaryReceivesBothArtifacts) {
    Skill(901, BattleEffectOp::Heal, 2, 2); Learn(kSelf, 901);
    Player(kSelf).mutable_base_attributes()->set_health(2500); Player(kAlly).mutable_base_attributes()->set_health(2500);
    Player(kSelf).mutable_equipment_effects()->set_outgoing_heal_bps(3200);
    Player(kSelf).mutable_equipment_effects()->set_shield_capacity_bps(3200);
    Artifact(kSelf, 4, 41); Artifact(kSelf, 4, 42);
    auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kAlly, 901));
    EXPECT_EQ(EventSum(result, BATTLE_EVENT_HEAL, kSelf, kAlly), 1560u);
    EXPECT_EQ(EventSum(result, BATTLE_EVENT_HEAL, kSelf, kSelf), 1320u);
    EXPECT_EQ(Uses(result.state(), kSelf, 41), 1u); EXPECT_EQ(Uses(result.state(), kSelf, 42), 1u);
}
TEST_F(EquipmentV1BattleTest, HealingWithNoMarginalBenefitDoesNotSpendArtifacts) {
    Skill(901, BattleEffectOp::Heal, 2); Learn(kSelf, 901); data->AddBuff(9010).set_flat_value(9900);
    Player(kAlly).mutable_base_attributes()->set_health(3000); Artifact(kSelf, 4, 41); Artifact(kSelf, 4, 42);
    auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kAlly, 901));
    EXPECT_EQ(EventSum(result, BATTLE_EVENT_HEAL, kSelf, kAlly), 7000u);
    EXPECT_EQ(Uses(result.state(), kSelf, 41), 0u); EXPECT_EQ(Uses(result.state(), kSelf, 42), 0u);
}
TEST_F(EquipmentV1BattleTest, BothShieldArtifactsParticipateInOverwriteAndStayOnPrimary) {
    Skill(902, BattleEffectOp::Shield, 2, 2); Learn(kSelf, 902);
    auto& shield = data->AddBuff(9020); shield.set_flat_value(800); shield.set_refresh_policy(2); shield.set_stack_key(9020);
    shield.set_duration_rounds(3); shield.set_tick_on_apply(0);
    Player(kSelf).mutable_equipment_effects()->set_shield_capacity_bps(3200);
    Player(kSelf).mutable_equipment_effects()->set_outgoing_heal_bps(3200); Artifact(kSelf, 5, 51); Artifact(kSelf, 5, 52);
    Shield(kAlly, 9030, 1100); data->AddBuff(9030).set_stack_key(9020);
    auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kAlly, 902));
    EXPECT_EQ(Capacity(result.state(), kAlly, 9020), 1248u); EXPECT_EQ(Capacity(result.state(), kSelf, 9020), 1056u);
    EXPECT_EQ(Capacity(result.state(), kAlly, 9030), 0u);
    EXPECT_EQ(Uses(result.state(), kSelf, 51), 1u); EXPECT_EQ(Uses(result.state(), kSelf, 52), 1u);
}
TEST_F(EquipmentV1BattleTest, RejectedShieldReplacementDoesNotSpendEitherArtifact) {
    Skill(902, BattleEffectOp::Shield, 2); Learn(kSelf, 902);
    auto& shield = data->AddBuff(9020); shield.set_flat_value(800); shield.set_refresh_policy(2); shield.set_stack_key(9020);
    Artifact(kSelf, 5, 51); Artifact(kSelf, 5, 52); Shield(kAlly, 9030, 2000); data->AddBuff(9030).set_stack_key(9020);
    auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kAlly, 902));
    EXPECT_EQ(Capacity(result.state(), kAlly, 9030), 2000u);
    EXPECT_EQ(Uses(result.state(), kSelf, 51), 0u); EXPECT_EQ(Uses(result.state(), kSelf, 52), 0u);
}
TEST_F(EquipmentV1BattleTest, EachDamageTypeBreaksBothExistingLayersWithoutHealthSpill) {
    for (const uint32_t type : {combatdamage::kPhysicalDamage, combatdamage::kMagicDamage}) {
        SCOPED_TRACE(type);
        Player(kSelf).clear_skill_table_ids(); Player(kSelf).clear_skill_ranks();
        auto& skill = Skill(904, BattleEffectOp::Damage); skill.set_damage_type(type); Learn(kSelf, 904);
        data->SetSkillDamage(904, 2000); Player(kSelf).clear_equipment_effects();
        Artifact(kSelf, type == combatdamage::kPhysicalDamage ? 1 : 2, 11);
        Artifact(kSelf, type == combatdamage::kPhysicalDamage ? 1 : 2, 12);
        Player(kEnemy).clear_buffs(); Shield(kEnemy, 9101, 610, 10); Shield(kEnemy, 9102, 500, 20);
        auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 904));
        EXPECT_EQ(EventSum(result, BATTLE_EVENT_DAMAGE, kSelf, kEnemy), 0u);
        EXPECT_EQ(Capacity(result.state(), kEnemy, 9101), 0u); EXPECT_EQ(Capacity(result.state(), kEnemy, 9102), 270u);
        EXPECT_EQ(Uses(result.state(), kSelf, 11), 1u); EXPECT_EQ(Uses(result.state(), kSelf, 12), 1u);
        EXPECT_EQ(Unit(result.state(), kEnemy).attributes().health(), 10000u);
        EXPECT_EQ(std::count_if(result.events().begin(), result.events().end(), [](const auto& event) { return event.event_type() == BATTLE_EVENT_DAMAGE; }), 1);
    }
}
TEST_F(EquipmentV1BattleTest, ShieldBreakUsesPostShareBudgetAndSkipsWrongTypeLayers) {
    auto& skill = Skill(904, BattleEffectOp::Damage); skill.set_damage_type(combatdamage::kPhysicalDamage); Learn(kSelf, 904);
    data->SetSkillDamage(904, 2000); Artifact(kSelf, 1, 11); Artifact(kSelf, 1, 12);
    SnapshotBuff(kEnemy, 9110, 0, kOtherEnemy);
    auto& share = data->AddBuff(9110); share.set_battle_op(static_cast<uint32_t>(BattleEffectOp::ShareDamage)); share.set_ratio(.5); share.set_effect_priority(40);
    Shield(kEnemy, 9101, 500, 10, 1); Shield(kEnemy, 9102, 500, 20, 2);
    auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 904));
    EXPECT_EQ(EventSum(result, BATTLE_EVENT_DAMAGE, kSelf, kOtherEnemy), 300u);
    EXPECT_EQ(Capacity(result.state(), kEnemy, 9101), 80u); EXPECT_EQ(Capacity(result.state(), kEnemy, 9102), 500u);
}
TEST_F(EquipmentV1BattleTest, TwoHeartArtifactsUseSameIncomingDamageAndCannotRefillNextRound) {
    auto& skill = Skill(904, BattleEffectOp::Damage); skill.set_damage_type(combatdamage::kPhysicalDamage); Learn(kSelf, 904);
    data->SetSkillDamage(904, 10000); Artifact(kEnemy, 6, 61); Artifact(kEnemy, 6, 62); Shield(kEnemy, 9101, 1000);
    auto engine = Start(); const auto first = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 904));
    EXPECT_EQ(EventSum(first, BATTLE_EVENT_DAMAGE, kSelf, kEnemy), 1200u);
    EXPECT_EQ(Uses(first.state(), kEnemy, 61), 1u); EXPECT_EQ(Uses(first.state(), kEnemy, 62), 1u);
    const auto second = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 904));
    EXPECT_EQ(EventSum(second, BATTLE_EVENT_DAMAGE, kSelf, kEnemy), 3000u);
}
TEST_F(EquipmentV1BattleTest, HeartExpiresAfterRoundTwoAndFullAbsorptionDoesNotSpend) {
    auto& skill = Skill(904, BattleEffectOp::Damage); skill.set_damage_type(combatdamage::kPhysicalDamage); Learn(kSelf, 904);
    data->SetSkillDamage(904, 2000); Artifact(kEnemy, 6, 61); Shield(kEnemy, 9101, 600);
    auto engine = Start(); const auto first = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 904));
    EXPECT_EQ(Uses(first.state(), kEnemy, 61), 0u);
    Round(*engine, kSelf, Command(BATTLE_ACTION_DEFEND));
    const auto third = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 904));
    EXPECT_EQ(EventSum(third, BATTLE_EVENT_DAMAGE, kSelf, kEnemy), 600u); EXPECT_EQ(Uses(third.state(), kEnemy, 61), 0u);
}
TEST_F(EquipmentV1BattleTest, TwoGourdsRecoverTogetherAndStopAtThreeUsesEach) {
    Player(kSelf).mutable_base_attributes()->set_mana(0); Artifact(kSelf, 7, 71); Artifact(kSelf, 7, 72);
    auto engine = Start();
    for (uint32_t round = 1; round <= 4; ++round) {
        const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_DEFEND));
        EXPECT_EQ(Unit(result.state(), kSelf).attributes().mana(), std::min(round, 3u) * 1000u);
        EXPECT_EQ(Uses(result.state(), kSelf, 71), std::min(round, 3u)); EXPECT_EQ(Uses(result.state(), kSelf, 72), std::min(round, 3u));
    }
}
TEST_F(EquipmentV1BattleTest, GourdOnlyChargesTheInstanceThatActuallyFillsMissingMana) {
    Player(kSelf).mutable_base_attributes()->set_mana(9900); Artifact(kSelf, 7, 72); Artifact(kSelf, 7, 71);
    auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_DEFEND));
    EXPECT_EQ(EventSum(result, BATTLE_EVENT_MANA, kSelf, kSelf), 100u);
    EXPECT_EQ(Uses(result.state(), kSelf, 71), 1u); EXPECT_EQ(Uses(result.state(), kSelf, 72), 0u);
}
TEST_F(EquipmentV1BattleTest, BothEchoesConsumeSameTypeWithoutFailureRegeneratingCompensation) {
    Skill(905, BattleEffectOp::Control); Learn(kSelf, 905);
    auto& control = data->AddBuff(9050); control.set_state_kind(1); control.set_application_chance(.01); control.set_duration_rounds(1);
    Artifact(kSelf, 3, 31); Artifact(kSelf, 3, 32);
    auto engine = Start(); const auto first = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 905));
    EXPECT_EQ(Uses(first.state(), kSelf, 31), 1u); EXPECT_EQ(Uses(first.state(), kSelf, 32), 1u);
    for (const auto& state : Unit(first.state(), kSelf).artifact_states()) { EXPECT_EQ(state.pending_round(), 2u); EXPECT_EQ(state.pending_ailment_kind(), 1u); }
    const auto second = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kOtherEnemy, 905));
    EXPECT_EQ(Uses(second.state(), kSelf, 31), 1u); EXPECT_EQ(Uses(second.state(), kSelf, 32), 1u);
    for (const auto& state : Unit(second.state(), kSelf).artifact_states()) EXPECT_EQ(state.pending_round(), 0u);
}
TEST_F(EquipmentV1BattleTest, EchoExpiresAtEndOfNextGlobalRoundAndKeepsBattleBudget) {
    Skill(905, BattleEffectOp::Control); Learn(kSelf, 905);
    auto& control = data->AddBuff(9050); control.set_state_kind(2); control.set_application_chance(.01); control.set_duration_rounds(1);
    Artifact(kSelf, 3, 31); Artifact(kSelf, 3, 32);
    auto engine = Start(); Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 905));
    const auto second = Round(*engine, kSelf, Command(BATTLE_ACTION_DEFEND));
    for (const auto& state : Unit(second.state(), kSelf).artifact_states()) { EXPECT_EQ(state.pending_round(), 0u); EXPECT_EQ(state.used_count(), 1u); }
}
TEST_F(EquipmentV1BattleTest, BothPurityArtifactsSpendOnOneLegalControlRollEvenWhenItFails) {
    Skill(905, BattleEffectOp::Control); Learn(kSelf, 905);
    auto& control = data->AddBuff(9050); control.set_state_kind(3); control.set_application_chance(.01); control.set_duration_rounds(1);
    Artifact(kEnemy, 8, 81); Artifact(kEnemy, 8, 82);
    auto engine = Start();
    for (uint32_t round = 1; round <= 3; ++round) {
        const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 905));
        EXPECT_EQ(Uses(result.state(), kEnemy, 81), std::min(round, 2u)); EXPECT_EQ(Uses(result.state(), kEnemy, 82), std::min(round, 2u));
    }
}
TEST_F(EquipmentV1BattleTest, PoisonDisabledAndImmuneControlsDoNotConsumePurityOrGenerateEcho) {
    Skill(905, BattleEffectOp::Control); Learn(kSelf, 905);
    auto& control = data->AddBuff(9050); control.set_state_kind(1); control.set_application_chance(0); (*control.mutable_tag())["Control"] = true;
    Artifact(kSelf, 3, 31); Artifact(kEnemy, 8, 81);
    auto engine = Start(); const auto disabled = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 905));
    EXPECT_EQ(Uses(disabled.state(), kSelf, 31), 0u); EXPECT_EQ(Uses(disabled.state(), kEnemy, 81), 0u);
    control.set_application_chance(.5); SnapshotBuff(kEnemy, 9120); (*data->AddBuff(9120).mutable_immune_tag())["Control"] = true;
    engine = Start(); const auto immune = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 905));
    EXPECT_EQ(Uses(immune.state(), kSelf, 31), 0u); EXPECT_EQ(Uses(immune.state(), kEnemy, 81), 0u);
    Player(kEnemy).clear_buffs(); control.set_state_kind(7); control.set_battle_op(static_cast<uint32_t>(BattleEffectOp::PeriodicDamage)); control.set_tick_count(1);
    engine = Start(); const auto poison = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 905));
    EXPECT_EQ(Uses(poison.state(), kEnemy, 81), 0u);
}
TEST_F(EquipmentV1BattleTest, PeriodicHealingFreezesAllCasterModifiersAndExcludesInstantHealArtifact) {
    Skill(906, BattleEffectOp::ReviveMark, 2); Learn(kSelf, 906);
    auto& mark = data->AddBuff(9060); mark.set_tick_count(3); mark.set_tick_on_apply(1); mark.set_keep_on_death(1); mark.set_secondary_ratio(.2);
    Player(kSelf).mutable_equipment_effects()->set_outgoing_heal_bps(3200); Artifact(kSelf, 4, 41);
    Player(kAlly).mutable_base_attributes()->set_health(100); Modifier(kSelf, 9130, BattleModifier::OutgoingHeal, .2);
    data->AddBuff(9130).set_duration_rounds(1);
    Player(kSelf).mutable_buffs(0)->set_tick_round(1); Player(kSelf).mutable_buffs(0)->set_remain_rounds(1);
    auto engine = Start(); const auto first = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kAlly, 906));
    const auto second = Round(*engine, kSelf, Command(BATTLE_ACTION_DEFEND));
    EXPECT_EQ(EventSum(first, BATTLE_EVENT_HEAL, kSelf, kAlly), 1520u);
    EXPECT_EQ(EventSum(second, BATTLE_EVENT_HEAL, kSelf, kAlly), 1520u); EXPECT_EQ(Uses(second.state(), kSelf, 41), 0u);
}
TEST_F(EquipmentV1BattleTest, DamageWithPoisonUsesAttackDiscountAndPurePoisonUsesPoisonDiscount) {
    for (const bool hasDamage : {false, true}) {
        SCOPED_TRACE(hasDamage); Player(kSelf).clear_skill_table_ids(); Player(kSelf).clear_skill_ranks();
        auto& skill = Skill(907, hasDamage ? BattleEffectOp::Damage : BattleEffectOp::PeriodicDamage);
        skill.set_action_tags(turnbattle::kActionTagMagic | turnbattle::kActionTagControl); skill.set_damage_type(combatdamage::kMagicDamage);
        data->SetSkillDamage(907, 10); Learn(kSelf, 907);
        data->AddBuff(9070).set_state_kind(7); data->AddBuff(9070).set_application_chance(.5); data->AddBuff(9070).set_tick_count(1);
        auto* equipment = Player(kSelf).mutable_equipment_effects(); (*equipment->mutable_mana_reduction_bps())[2] = 300;
        (*equipment->mutable_mana_reduction_bps())[11] = 1200;
        auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kEnemy, 907));
        EXPECT_EQ(EventSum(result, BATTLE_EVENT_MANA, kSelf, kSelf), hasDamage ? 970u : 880u);
    }
}
TEST_F(EquipmentV1BattleTest, ManaCostRoundsOriginalOnceThenCombinesTemporaryAndEquipmentReduction) {
    Skill(901, BattleEffectOp::Heal, 2, 2); Learn(kSelf, 901);
    Player(kSelf).set_max_mana(101); Player(kSelf).mutable_base_attributes()->set_mana(101);
    (*Player(kSelf).mutable_equipment_effects()->mutable_mana_reduction_bps())[3] = 1200;
    Modifier(kSelf, 9131, BattleModifier::ManaCost, -.5);
    auto engine = Start(); const auto result = Round(*engine, kSelf, Command(BATTLE_ACTION_SKILL, kAlly, 901));
    EXPECT_EQ(EventSum(result, BATTLE_EVENT_MANA, kSelf, kSelf), 8u);
}
TEST_F(EquipmentV1BattleTest, EquipmentStateIsDeterministicAndRemovedFromClientState) {
    Player(kSelf).mutable_base_attributes()->set_mana(0); Artifact(kSelf, 7, 72); Artifact(kSelf, 7, 71);
    auto first = Start(), second = Start();
    const auto a = Round(*first, kSelf, Command(BATTLE_ACTION_DEFEND)); const auto b = Round(*second, kSelf, Command(BATTLE_ACTION_DEFEND));
    EXPECT_EQ(a.SerializeAsString(), b.SerializeAsString());
    auto visible = a.state(); TurnBattleEngine::StripEngineOnlyState(visible);
    EXPECT_FALSE(Unit(visible, kSelf).has_equipment_effects()); EXPECT_EQ(Unit(visible, kSelf).artifact_states_size(), 0);
    EXPECT_EQ(Unit(visible, kSelf).entry_max_health(), 0u); EXPECT_EQ(Unit(visible, kSelf).entry_max_mana(), 0u);
    EXPECT_EQ(Unit(first->BuildStateSnapshot(), kSelf).artifact_states_size(), 2);
}
} // namespace
