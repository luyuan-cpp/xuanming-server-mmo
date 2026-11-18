#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <map>
#include <memory>
#include <set>
#include <string>
#include <vector>

#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>

#include "constants/battle_effect_constants.h"
#include "constants/turn_battle_constants.h"
#include "memory_battle_data_provider.h"
#include "system/turn_battle_engine.h"
#include "table/proto/tip/common_error_tip.pb.h"

// 使用正式表的操作/事件契约，通过公开行动和快照验证；不注入引擎私有状态。
namespace {
using turnbattle::BattleDamageOrigin;
using turnbattle::BattleEffectOp;
using turnbattle::BattleModifier;
using turnbattle::BattleTrigger;
using turnbattle::TurnBattleEngine;
constexpr uint64_t kOwner = 101, kAlly = 102, kEnemy = 201, kSecondEnemy = 202;
constexpr uint32_t kIdle = 85000, kStrike = 85001, kListener = 85002, kChild = 85003;
constexpr uint32_t kSacrifice = 85004, kRevive = 85005;

BattleAction Cast(uint32_t skill, uint64_t target) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_SKILL);
    action.set_skill_table_id(skill);
    action.set_target_id(target);
    return action;
}
BattleAction Attack(uint64_t target) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_ATTACK);
    action.set_target_id(target);
    return action;
}
SkillTable& Skill(MemoryBattleDataProvider& data, uint32_t id, uint32_t kind = 1,
                  uint32_t team = 1, uint32_t targets = 1) {
    auto& row = data.AddSkill(id);
    row.set_battle_model(1);
    row.set_caster_mask(3);
    row.set_ability_kind(kind);
    row.set_target_team(team);
    row.set_max_targets(targets);
    row.set_pet_max_targets(targets);
    row.set_unlock_level(1);
    row.set_max_rank(10);
    row.add_skill_type(1);
    return row;
}
BuffTable& Effect(MemoryBattleDataProvider& data, uint32_t id, BattleEffectOp op) {
    auto& row = data.AddBuff(id);
    row.set_battle_op(static_cast<uint32_t>(op));
    row.set_max_layer(1);
    return row;
}
BuffTable& Listen(MemoryBattleDataProvider& data, uint32_t skill, uint32_t effect,
                  BattleEffectOp op, BattleTrigger trigger, uint32_t kind = 2) {
    Skill(data, skill, kind, 3).add_init_effect(effect);
    auto& row = Effect(data, effect, op);
    row.set_trigger_event(static_cast<uint32_t>(trigger));
    row.set_infinite_duration(1);
    row.set_per_round_limit(1);
    return row;
}
std::shared_ptr<MemoryBattleDataProvider> Data() {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    data->AddBattleReward(0);
    data->AddPet(1);
    // 等待使用无数值回蓝操作，避免防御姿态干扰自动伤害的比较。
    Skill(*data, kIdle, 1, 3).add_effect(kIdle);
    Effect(*data, kIdle, BattleEffectOp::RestoreMana);
    auto& strike = Skill(*data, kStrike);
    strike.add_effect(kStrike);
    strike.set_damage_type(1);
    strike.set_action_tags(turnbattle::kActionTagPhysical);
    strike.set_attack_multiplier(1);
    Effect(*data, kStrike, BattleEffectOp::Damage);
    data->SetSkillDamage(kStrike, 10);
    return data;
}
void Learn(BattlePlayerSnapshot& actor, uint32_t id, bool active = false) {
    (*actor.mutable_skill_ranks())[id] = 1;
    if (active) actor.add_skill_table_ids(id);
}
void Learn(BattlePetSnapshot& actor, uint32_t id, bool active = false) {
    (*actor.mutable_skill_ranks())[id] = 1;
    if (active) actor.add_skill_table_ids(id);
}
CreateBattleRequest Request(bool withPet = false, uint32_t enemyCount = 2) {
    CreateBattleRequest request;
    request.set_battle_id(85001);
    request.set_match_mode(3);
    request.set_seed(20261117);
    for (uint32_t side = 0; side < 2; ++side) {
        for (uint32_t i = 0; i < (side == 0 ? 2u : enemyCount); ++i) {
            auto* actor = request.add_players();
            actor->set_player_id(side == 0 ? 101 + i : 201 + i);
            actor->set_team_index(side);
            actor->set_level(30);
            actor->set_max_health(10000);
            actor->set_max_mana(1000);
            actor->set_physical_attack(100);
            actor->set_magic_attack(100);
            actor->mutable_base_attributes()->set_health(10000);
            actor->mutable_base_attributes()->set_mana(1000);
            actor->mutable_base_attributes()->set_speed(side == 0 ? 300 - i * 10 : 100 - i * 10);
            Learn(*actor, kIdle, true);
            Learn(*actor, kStrike, true);
        }
    }
    if (withPet) {
        auto* pet = request.mutable_players(0)->add_pets();
        pet->set_pet_id(1001);
        pet->set_owner_player_id(kOwner);
        pet->set_pet_table_id(1);
        pet->set_level(30);
        pet->set_max_health(10000);
        pet->set_max_mana(1000);
        pet->set_physical_attack(100);
        pet->set_magic_attack(100);
        pet->mutable_base_attributes()->set_health(10000);
        pet->mutable_base_attributes()->set_mana(100);
        pet->mutable_base_attributes()->set_speed(250);
        Learn(*pet, kIdle, true);
        Learn(*pet, kStrike, true);
    }
    return request;
}
const BattleActorState& Actor(const BattleStateS2C& state, uint64_t id) {
    for (const auto& actor : state.actors()) if (actor.actor_id() == id) return actor;
    ADD_FAILURE() << "missing actor " << id;
    return BattleActorState::default_instance();
}
uint64_t PetId(const TurnBattleEngine& engine, uint64_t owner = kOwner) {
    const auto state = engine.BuildStateSnapshot();
    for (const auto& actor : state.actors())
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && actor.owner_player_id() == owner) return actor.actor_id();
    return 0;
}
const BattleBuffEntry* Buff(const BattleStateS2C& state, uint64_t actor, uint32_t table) {
    for (const auto& entry : Actor(state, actor).buffs()) if (entry.buff_table_id() == table) return &entry;
    return nullptr;
}
TurnResultS2C Round(TurnBattleEngine& engine, const std::map<uint64_t, BattleAction>& actions = {}) {
    const auto state = engine.BuildStateSnapshot();
    for (const auto& actor : state.actors()) {
        if (actor.is_dead() || actor.fled()) continue;
        const auto found = actions.find(actor.actor_id());
        const auto action = found == actions.end() ? Cast(kIdle, actor.actor_id()) : found->second;
        EXPECT_EQ(engine.ValidateAction(actor.actor_id(), action), kSuccess) << actor.actor_id();
        engine.SubmitAction(actor.actor_id(), action);
    }
    return engine.ResolveCurrentRound();
}
std::vector<BattleEventItem> Damage(const TurnResultS2C& result, uint64_t source,
                                  BattleDamageOrigin origin = BattleDamageOrigin::Direct) {
    std::vector<BattleEventItem> events;
    for (const auto& event : result.events())
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.source_id() == source &&
            event.damage_origin() == static_cast<uint32_t>(origin)) events.push_back(event);
    return events;
}
uint64_t SingleDamage(const TurnResultS2C& result, uint64_t source) {
    const auto hits = Damage(result, source);
    EXPECT_EQ(hits.size(), 1u);
    return hits.empty() ? 0 : hits.front().value();
}
std::string Canonical(const google::protobuf::MessageLite& message) {
    std::string result;
    {
        google::protobuf::io::StringOutputStream output(&result);
        google::protobuf::io::CodedOutputStream coded(&output);
        coded.SetSerializationDeterministic(true);
        EXPECT_TRUE(message.SerializeToCodedStream(&coded));
        EXPECT_FALSE(coded.HadError());
    }
    return result;
}
void AddSacrificeAndRevival(MemoryBattleDataProvider& data, CreateBattleRequest& request) {
    auto& sacrifice = Skill(data, kSacrifice, 1, 2);
    sacrifice.set_health_cost_mode(2);
    sacrifice.add_effect(kSacrifice);
    Effect(data, kSacrifice, BattleEffectOp::Heal).set_flat_value(1);
    Learn(*request.mutable_players(0), kSacrifice, true);
    auto& revive = Skill(data, kRevive, 1, 2);
    revive.set_allow_dead_target(1);
    revive.add_effect(kRevive);
    auto& mark = Effect(data, kRevive, BattleEffectOp::ReviveMark);
    mark.set_keep_on_death(1);
    mark.set_tick_count(1);
    mark.set_duration_rounds(1);
    mark.set_tick_on_apply(1);
    mark.set_secondary_ratio(.5);
    Learn(*request.mutable_players(1), kRevive, true);
}

TEST(SkillTriggerTest, PhysicalAndMagicMirrorUseOwnStatsSameSkillAndZeroManaWithoutRecursion) {
    for (const uint32_t type : {1u, 0u}) {
        SCOPED_TRACE(type);
        auto data = Data();
        auto& strike = data->AddSkill(kStrike);
        strike.set_damage_type(type);
        strike.set_action_tags(type == 1 ? turnbattle::kActionTagPhysical : turnbattle::kActionTagMagic);
        strike.set_mana_percent(.1);
        strike.set_cooldown_rounds(3);
        auto& mirror = Listen(*data, kListener, kListener, BattleEffectOp::MirrorSkill, BattleTrigger::AfterTake);
        mirror.set_damage_filter(type == 1 ? 1 : 2);
        mirror.set_ratio(1);
        auto request = Request();
        auto* defender = request.mutable_players(2);
        defender->set_physical_attack(400);
        defender->set_magic_attack(700);
        defender->mutable_base_attributes()->set_mana(0);
        // 反击无需学习来袭技能，且双方持有反击都不能递归。
        defender->clear_skill_table_ids(); defender->clear_skill_ranks();
        Learn(*defender, kIdle, true); Learn(*defender, kListener);
        Learn(*request.mutable_players(0), kListener);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        const auto result = Round(engine, {{kOwner, Cast(kStrike, kEnemy)}});
        const auto originals = Damage(result, kOwner);
        const auto reflected = Damage(result, kEnemy, BattleDamageOrigin::Mirror);
        ASSERT_EQ(originals.size(), 1u);
        ASSERT_EQ(reflected.size(), 1u);
        EXPECT_GT(reflected.front().value(), originals.front().value());
        EXPECT_EQ(reflected.front().skill_table_id(), kStrike);
        EXPECT_EQ(reflected.front().target_id(), kOwner);
        EXPECT_EQ(reflected.front().hit_kind(), BATTLE_HIT_MIRROR);
        EXPECT_TRUE(Damage(result, kOwner, BattleDamageOrigin::Mirror).empty());
        const auto state = engine.BuildStateSnapshot();
        EXPECT_EQ(Actor(state, kEnemy).attributes().mana(), 0u);
        EXPECT_EQ(Actor(state, kOwner).attributes().mana(), 900u);
        EXPECT_TRUE(Actor(state, kEnemy).skill_cooldown_rounds().empty());

        // 与同属性、同技能的一次正常施放比较，排除偷用攻击者强度的实现。
        auto baseline = request;
        baseline.mutable_players(0)->mutable_skill_ranks()->erase(kListener);
        auto* normal = baseline.mutable_players(2);
        normal->mutable_skill_ranks()->erase(kListener);
        normal->mutable_base_attributes()->set_mana(1000);
        Learn(*normal, kStrike, true);
        TurnBattleEngine control(data);
        ASSERT_TRUE(control.Initialize(baseline));
        EXPECT_EQ(reflected.front().value(), SingleDamage(Round(control, {{kEnemy, Cast(kStrike, kOwner)}}), kEnemy));
    }
}

TEST(SkillTriggerTest, PetManaRecoveryUsesFirstActualEnemyHitEachRound) {
    auto data = Data();
    auto& recovery = Listen(*data, kListener, kListener, BattleEffectOp::ConditionalMana, BattleTrigger::AfterTake);
    recovery.set_value_basis(4); recovery.set_ratio(.05);
    auto request = Request(true);
    Learn(*request.mutable_players(0)->mutable_pets(0), kListener);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto pet = PetId(engine);
    ASSERT_NE(pet, 0u);
    for (uint64_t round = 1; round <= 2; ++round) {
        const auto result = Round(engine, {{kEnemy, Cast(kStrike, pet)}, {kSecondEnemy, Cast(kStrike, pet)}});
        EXPECT_EQ(Damage(result, kEnemy).size() + Damage(result, kSecondEnemy).size(), 2u);
        uint32_t returns = 0;
        for (const auto& event : result.events())
            if (event.event_type() == BATTLE_EVENT_MANA && event.source_id() == pet && event.value() > 0) ++returns;
        EXPECT_EQ(returns, 1u);
        EXPECT_EQ(Actor(engine.BuildStateSnapshot(), pet).attributes().mana(), 100 + round * 50);
    }
}

TEST(SkillTriggerTest, PetBasicPursuitHitsTwoDistinctEnemiesOnlyOnce) {
    auto data = Data();
    auto& pursuit = Listen(*data, kListener, kListener, BattleEffectOp::PursuitDamage, BattleTrigger::AfterBasic, 3);
    pursuit.set_damage_filter(1); pursuit.set_ratio(.5);
    pursuit.set_target_count(1); pursuit.set_pet_target_count(2);
    auto request = Request(true, 3);
    Learn(*request.mutable_players(0)->mutable_pets(0), kListener);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto pet = PetId(engine);
    const auto result = Round(engine, {{pet, Attack(kEnemy)}});
    EXPECT_EQ(Damage(result, pet).size(), 1u);
    const auto hits = Damage(result, pet, BattleDamageOrigin::Pursuit);
    ASSERT_EQ(hits.size(), 2u);
    EXPECT_EQ(hits.front().target_id(), kEnemy);
    EXPECT_NE(hits.front().target_id(), hits.back().target_id());
    const auto state = engine.BuildStateSnapshot();
    ASSERT_NE(Buff(state, pet, kListener), nullptr);
    EXPECT_EQ(Buff(state, pet, kListener)->trigger_count(), 1u);
}

TEST(SkillTriggerTest, PetMagicEchoTriggersOnceForTwoTargetSpellAndNeverForPhysicalSkill) {
    auto data = Data();
    auto& strike = data->AddSkill(kStrike);
    strike.set_damage_type(0); strike.set_action_tags(turnbattle::kActionTagMagic);
    strike.set_max_targets(2); strike.set_pet_max_targets(2);
    auto& echo = Listen(*data, kListener, kListener, BattleEffectOp::PursuitDamage, BattleTrigger::AfterSkill, 3);
    echo.set_damage_filter(2); echo.set_ratio(.4); echo.set_target_count(2); echo.set_pet_target_count(2);
    auto request = Request(true, 3);
    Learn(*request.mutable_players(0)->mutable_pets(0), kListener);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto pet = PetId(engine);
    const auto result = Round(engine, {{pet, Cast(kStrike, kEnemy)}});
    EXPECT_EQ(Damage(result, pet).size(), 2u);
    const auto hits = Damage(result, pet, BattleDamageOrigin::Pursuit);
    ASSERT_EQ(hits.size(), 2u);
    EXPECT_NE(hits[0].target_id(), hits[1].target_id());
    const auto state = engine.BuildStateSnapshot();
    ASSERT_NE(Buff(state, pet, kListener), nullptr);
    EXPECT_EQ(Buff(state, pet, kListener)->trigger_count(), 1u);
    EXPECT_TRUE(Damage(Round(engine, {{pet, Attack(kEnemy)}}), pet, BattleDamageOrigin::Pursuit).empty());
}

TEST(SkillTriggerTest, ArmorPursuitAddsOneDebuffWithoutExtraDamage) {
    auto data = Data();
    auto& listener = Listen(*data, kListener, kListener, BattleEffectOp::ApplyChildren, BattleTrigger::AfterBasic, 3);
    listener.add_target_sub_buff(kChild);
    auto& debuff = Effect(*data, kChild, BattleEffectOp::Modifier);
    debuff.set_modifier_stat(static_cast<uint32_t>(BattleModifier::Defense));
    debuff.set_ratio(-.2); debuff.set_duration_rounds(3); debuff.set_tick_on_apply(1);
    (*debuff.mutable_tag())["Break"] = true;
    auto request = Request(true);
    Learn(*request.mutable_players(0)->mutable_pets(0), kListener);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto pet = PetId(engine);
    const auto result = Round(engine, {{pet, Attack(kEnemy)}});
    ASSERT_EQ(Damage(result, pet).size(), 1u);
    EXPECT_TRUE(Damage(result, pet, BattleDamageOrigin::Pursuit).empty());
    const auto state = engine.BuildStateSnapshot();
    ASSERT_NE(Buff(state, kEnemy, kChild), nullptr);
    EXPECT_EQ(Buff(state, kEnemy, kChild)->caster_id(), pet);
    EXPECT_EQ(Buff(state, kSecondEnemy, kChild), nullptr);
}

TEST(SkillTriggerTest, ManaPursuitRemainsAListenerAcrossRounds) {
    auto data = Data();
    auto& drain = Listen(*data, kListener, kListener, BattleEffectOp::DrainMana, BattleTrigger::AfterBasic, 3);
    drain.set_value_basis(4); drain.set_ratio(.1);
    auto request = Request();
    Learn(*request.mutable_players(0), kListener);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    EXPECT_EQ(Actor(engine.BuildStateSnapshot(), kEnemy).attributes().mana(), 1000u);
    for (uint64_t round = 1; round <= 2; ++round) {
        Round(engine, {{kOwner, Attack(kEnemy)}});
        const auto state = engine.BuildStateSnapshot();
        EXPECT_EQ(Actor(state, kEnemy).attributes().mana(), 1000 - round * 100);
        EXPECT_NE(Buff(state, kOwner, kListener), nullptr);
    }
}

TEST(SkillTriggerTest, GrowingOpeningAuraStartsThirdRoundPausesOnDeathAndDoesNotCatchUp) {
    auto data = Data();
    auto& opening = Skill(*data, kListener, 4, 2, 7);
    opening.add_init_effect(kListener);
    auto& aura = Effect(*data, kListener, BattleEffectOp::GrowingAura);
    aura.set_trigger_event(static_cast<uint32_t>(BattleTrigger::RoundStart));
    aura.set_modifier_stat(static_cast<uint32_t>(BattleModifier::OutgoingDamage));
    aura.set_ratio(.05); aura.set_start_round(3); aura.set_stack_limit(3);
    aura.set_require_living_caster(1); aura.set_keep_on_death(1); aura.set_infinite_duration(1);
    auto request = Request();
    Learn(*request.mutable_players(0), kListener);
    AddSacrificeAndRevival(*data, request);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto initial = engine.BuildStateSnapshot();
    ASSERT_NE(Buff(initial, kAlly, kListener), nullptr);
    const auto instance = Buff(initial, kAlly, kListener)->buff_id();
    EXPECT_EQ(Buff(initial, kAlly, kListener)->stacks(), 0u);
    const auto baseline = SingleDamage(Round(engine, {{kAlly, Cast(kStrike, kEnemy)}}), kAlly);
    Round(engine);
    EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kAlly, kListener)->stacks(), 0u);
    Round(engine, {{kOwner, Cast(kSacrifice, kAlly)}}); // 第3轮先增长一层，然后来源牺牲。
    auto state = engine.BuildStateSnapshot();
    ASSERT_TRUE(Actor(state, kOwner).is_dead());
    ASSERT_NE(Buff(state, kAlly, kListener), nullptr);
    EXPECT_EQ(Buff(state, kAlly, kListener)->stacks(), 1u);
    EXPECT_EQ(SingleDamage(Round(engine, {{kAlly, Cast(kStrike, kEnemy)}}), kAlly), baseline);
    EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kAlly, kListener)->stacks(), 1u);
    Round(engine, {{kAlly, Cast(kRevive, kOwner)}}); // 第5轮末复活，不补第4/5轮层数。
    state = engine.BuildStateSnapshot();
    EXPECT_FALSE(Actor(state, kOwner).is_dead());
    ASSERT_NE(Buff(state, kAlly, kListener), nullptr);
    EXPECT_EQ(Buff(state, kAlly, kListener)->buff_id(), instance);
    EXPECT_EQ(Buff(state, kAlly, kListener)->stacks(), 1u);
    EXPECT_GT(SingleDamage(Round(engine, {{kAlly, Cast(kStrike, kEnemy)}}), kAlly), baseline);
    EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kAlly, kListener)->stacks(), 2u);
    Round(engine); Round(engine);
    EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kAlly, kListener)->stacks(), 3u);
}

void AddOpeningGuard(MemoryBattleDataProvider& data) {
    Skill(data, kListener, 4, 2, 7).add_init_effect(kListener);
    auto& aura = Effect(data, kListener, BattleEffectOp::Modifier);
    aura.set_modifier_stat(static_cast<uint32_t>(BattleModifier::IncomingDamage));
    aura.set_ratio(-.2); aura.set_duration_rounds(2); aura.set_tick_on_apply(1);
    aura.set_require_living_caster(1); aura.set_keep_on_death(1);
}

TEST(SkillTriggerTest, OpeningGuardReducesExactlyFirstTwoGlobalRounds) {
    auto data = Data(); AddOpeningGuard(*data);
    auto request = Request(); Learn(*request.mutable_players(0), kListener);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto first = SingleDamage(Round(engine, {{kEnemy, Cast(kStrike, kAlly)}}), kEnemy);
    const auto second = SingleDamage(Round(engine, {{kEnemy, Cast(kStrike, kAlly)}}), kEnemy);
    const auto state = engine.BuildStateSnapshot();
    EXPECT_EQ(Buff(state, kAlly, kListener), nullptr);
    const auto third = SingleDamage(Round(engine, {{kEnemy, Cast(kStrike, kAlly)}}), kEnemy);
    EXPECT_EQ(first, second);
    EXPECT_LT(second, third);
}

TEST(SkillTriggerTest, OpeningGuardDeadlineContinuesWhileSourceDeadAndRevivalDoesNotRestartIt) {
    auto data = Data(); AddOpeningGuard(*data);
    auto request = Request(); Learn(*request.mutable_players(0), kListener);
    AddSacrificeAndRevival(*data, request);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto first = SingleDamage(Round(engine, {{kOwner, Cast(kSacrifice, kAlly)}, {kEnemy, Cast(kStrike, kAlly)}}), kEnemy);
    auto state = engine.BuildStateSnapshot();
    EXPECT_TRUE(Actor(state, kOwner).is_dead());
    ASSERT_NE(Buff(state, kAlly, kListener), nullptr);
    EXPECT_EQ(Buff(state, kAlly, kListener)->remain_rounds(), 1u);
    Round(engine, {{kAlly, Cast(kRevive, kOwner)}});
    state = engine.BuildStateSnapshot();
    EXPECT_FALSE(Actor(state, kOwner).is_dead());
    EXPECT_EQ(Buff(state, kAlly, kListener), nullptr);
    EXPECT_EQ(Buff(state, kOwner, kListener), nullptr);
    const auto third = SingleDamage(Round(engine, {{kEnemy, Cast(kStrike, kAlly)}}), kEnemy);
    EXPECT_EQ(first, third);
}

TEST(SkillTriggerTest, ConfusionUsesOneOtherAllyNormalManaAndSkipsCoolingSkills) {
    auto data = Data();
    Skill(*data, kListener, 2, 3).add_init_effect(kListener);
    auto& confusion = Effect(*data, kListener, BattleEffectOp::Control);
    confusion.set_state_kind(static_cast<uint32_t>(turnbattle::BattleStateKind::Confusion));
    confusion.set_infinite_duration(1);
    auto& strike = data->AddSkill(kStrike);
    strike.set_max_targets(5); strike.set_pet_max_targets(2);
    strike.set_damage_type(0); strike.set_action_tags(turnbattle::kActionTagMagic);
    strike.set_mana_percent(.1); strike.set_cooldown_rounds(3);
    auto request = Request(); Learn(*request.mutable_players(0), kListener);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto first = Round(engine, {{kOwner, Cast(kStrike, kEnemy)}});
    const auto hits = Damage(first, kOwner, BattleDamageOrigin::Confusion);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_EQ(hits.front().target_id(), kAlly);
    EXPECT_EQ(hits.front().skill_table_id(), kStrike);
    auto state = engine.BuildStateSnapshot();
    EXPECT_EQ(Actor(state, kOwner).attributes().mana(), 900u);
    EXPECT_EQ(Actor(state, kOwner).attributes().health(), 10000u);
    EXPECT_EQ(Actor(state, kEnemy).attributes().health(), 10000u);
    const auto second = Damage(Round(engine), kOwner, BattleDamageOrigin::Confusion);
    ASSERT_EQ(second.size(), 1u);
    EXPECT_EQ(second.front().target_id(), kAlly);
    EXPECT_EQ(second.front().skill_table_id(), 0u); // 在冷却的法术不再被混乱绕过，退回普攻。
    EXPECT_EQ(Actor(engine.BuildStateSnapshot(), kOwner).attributes().mana(), 900u);
}

TEST(SkillTriggerTest, ActiveMagicAndPhysicalSealsKeepTheirDifferentActionBoundaries) {
    for (const uint32_t stateKind : {1u, 2u, 3u}) {
        SCOPED_TRACE(stateKind);
        auto data = Data();
        Skill(*data, kListener, 2, 3).add_init_effect(kListener);
        auto& seal = Effect(*data, kListener, BattleEffectOp::Control);
        seal.set_state_kind(stateKind); seal.set_infinite_duration(1);
        auto& magic = Skill(*data, kChild);
        magic.set_action_tags(turnbattle::kActionTagMagic);
        magic.set_damage_type(0); magic.add_effect(kStrike);
        auto& heal = Skill(*data, kRevive, 1, 2);
        heal.set_action_tags(turnbattle::kActionTagHeal); heal.add_effect(kRevive);
        Effect(*data, kRevive, BattleEffectOp::Heal).set_flat_value(10);
        data->AddSkill(kIdle).set_action_tags(turnbattle::kActionTagSupport);
        auto request = Request();
        Learn(*request.mutable_players(0), kListener);
        Learn(*request.mutable_players(0), kChild, true);
        Learn(*request.mutable_players(0), kRevive, true);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        EXPECT_EQ(engine.ValidateAction(kOwner, Cast(kStrike, kEnemy)) == kSuccess, stateKind == 2);
        EXPECT_EQ(engine.ValidateAction(kOwner, Cast(kChild, kEnemy)) == kSuccess, stateKind == 3);
        EXPECT_EQ(engine.ValidateAction(kOwner, Cast(kIdle, kOwner)) == kSuccess, stateKind != 1);
        EXPECT_EQ(engine.ValidateAction(kOwner, Cast(kRevive, kAlly)) == kSuccess, stateKind != 1);
        EXPECT_EQ(engine.ValidateAction(kOwner, Attack(kEnemy)) == kSuccess, stateKind != 3);
        BattleAction defend; defend.set_action_type(BATTLE_ACTION_DEFEND);
        EXPECT_EQ(engine.ValidateAction(kOwner, defend), kSuccess);
    }
}

TEST(SkillTriggerTest, AdaptiveDamageChoosesEachBeneficiaryOwnStrongerAttack) {
    auto data = Data();
    Skill(*data, kListener, 2, 3).add_init_effect(kListener);
    auto& adaptive = Effect(*data, kListener, BattleEffectOp::Modifier);
    adaptive.set_modifier_stat(static_cast<uint32_t>(BattleModifier::AdaptiveDamage));
    adaptive.set_ratio(.2); adaptive.set_infinite_duration(1);
    auto& magic = Skill(*data, kChild);
    magic.set_damage_type(0); magic.set_action_tags(turnbattle::kActionTagMagic);
    magic.set_attack_multiplier(1); magic.add_effect(kStrike);
    data->SetSkillDamage(kChild, 10);
    auto request = Request();
    request.mutable_players(0)->set_physical_attack(400);
    request.mutable_players(1)->set_magic_attack(500);
    for (int i = 0; i < 2; ++i) { Learn(*request.mutable_players(i), kListener); Learn(*request.mutable_players(i), kChild, true); }
    auto baseline = request;
    for (int i = 0; i < 2; ++i) baseline.mutable_players(i)->mutable_skill_ranks()->erase(kListener);
    TurnBattleEngine engine(data), control(data);
    ASSERT_TRUE(engine.Initialize(request)); ASSERT_TRUE(control.Initialize(baseline));
    const auto state = engine.BuildStateSnapshot();
    ASSERT_NE(Buff(state, kOwner, kListener), nullptr); ASSERT_NE(Buff(state, kAlly, kListener), nullptr);
    EXPECT_EQ(Buff(state, kOwner, kListener)->chosen_stat(), 1u);
    EXPECT_EQ(Buff(state, kAlly, kListener)->chosen_stat(), 2u);
    const std::map<uint64_t, BattleAction> physical = {{kOwner, Cast(kStrike, kEnemy)}, {kAlly, Cast(kStrike, kSecondEnemy)}};
    const auto boostedPhysical = Round(engine, physical), basePhysical = Round(control, physical);
    EXPECT_GT(SingleDamage(boostedPhysical, kOwner), SingleDamage(basePhysical, kOwner));
    EXPECT_EQ(SingleDamage(boostedPhysical, kAlly), SingleDamage(basePhysical, kAlly));
    const std::map<uint64_t, BattleAction> magical = {{kOwner, Cast(kChild, kEnemy)}, {kAlly, Cast(kChild, kSecondEnemy)}};
    const auto boostedMagic = Round(engine, magical), baseMagic = Round(control, magical);
    EXPECT_EQ(SingleDamage(boostedMagic, kOwner), SingleDamage(baseMagic, kOwner));
    EXPECT_GT(SingleDamage(boostedMagic, kAlly), SingleDamage(baseMagic, kAlly));
}

TEST(SkillTriggerTest, RepeatedDodgesStoreOneChargeAndOnlyNextSingleDamageConsumesIt) {
    auto data = Data();
    auto& dodge = Effect(*data, kChild, BattleEffectOp::Modifier);
    dodge.set_modifier_stat(static_cast<uint32_t>(BattleModifier::Evasion)); dodge.set_ratio(1); dodge.set_infinite_duration(1);
    auto& listener = Listen(*data, kListener, kListener, BattleEffectOp::EvasionCharge, BattleTrigger::AfterDodge);
    listener.set_per_round_limit(0); listener.add_sub_buff(kRevive);
    data->AddSkill(kListener).add_init_effect(kChild);
    auto& charge = Effect(*data, kRevive, BattleEffectOp::Modifier);
    charge.set_modifier_stat(static_cast<uint32_t>(BattleModifier::NextDirectDamage));
    charge.set_ratio(.3); charge.set_duration_rounds(2); charge.set_hit_limit(1);
    auto request = Request(false, 5); Learn(*request.mutable_players(0), kListener);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    bool repeatedDodge = false;
    for (uint32_t round = 0; round < 8 && !repeatedDodge; ++round) {
        std::map<uint64_t, BattleAction> attacks;
        for (uint64_t id = 201; id < 206; ++id) attacks.emplace(id, Cast(kStrike, kOwner));
        const auto result = Round(engine, attacks);
        const auto misses = std::count_if(result.events().begin(), result.events().end(), [](const auto& event) {
            return event.event_type() == BATTLE_EVENT_MISS && event.target_id() == kOwner;
        });
        repeatedDodge = misses >= 2;
    }
    ASSERT_TRUE(repeatedDodge);
    const auto state = engine.BuildStateSnapshot();
    EXPECT_EQ(std::count_if(Actor(state, kOwner).buffs().begin(), Actor(state, kOwner).buffs().end(),
        [](const auto& entry) { return entry.buff_table_id() == kRevive; }), 1);
    const auto boosted = SingleDamage(Round(engine, {{kOwner, Cast(kStrike, kEnemy)}}), kOwner);
    EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kOwner, kRevive), nullptr);
    const auto normal = SingleDamage(Round(engine, {{kOwner, Cast(kStrike, kEnemy)}}), kOwner);
    EXPECT_GT(boosted, normal);
    EXPECT_LE(boosted, static_cast<uint64_t>(normal * 1.3) + 1);
}

void AddPoison(MemoryBattleDataProvider& data, uint32_t id, const char* tag, uint32_t state) {
    auto& dot = Effect(data, id, BattleEffectOp::PeriodicDamage);
    dot.set_flat_value(10); dot.set_tick_count(3); dot.set_duration_rounds(3);
    dot.set_state_kind(state); dot.set_refresh_policy(1);
    // 这些用例验证周期机制；显式启用施加判定，零概率配置按正式规则不挂毒。
    dot.set_application_chance(1);
    (*dot.mutable_tag())[tag] = true;
}

TEST(SkillTriggerTest, PoisonExtensionIsOncePerInstanceAndRefreshRetainsThatBudget) {
    auto data = Data();
    data->AddSkill(kStrike).clear_effect(); data->AddSkill(kStrike).add_effect(kStrike);
    AddPoison(*data, kStrike, "Poison", 7);
    Skill(*data, kListener).add_effect(kListener);
    auto& extend = Effect(*data, kListener, BattleEffectOp::ExtendPeriodic);
    extend.set_required_tag("Poison"); extend.set_effect_count(1);
    auto request = Request(); Learn(*request.mutable_players(0), kListener, true);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    Round(engine, {{kOwner, Cast(kStrike, kEnemy)}});
    Round(engine, {{kOwner, Cast(kListener, kEnemy)}});
    auto state = engine.BuildStateSnapshot();
    ASSERT_NE(Buff(state, kEnemy, kStrike), nullptr);
    EXPECT_EQ(Buff(state, kEnemy, kStrike)->remaining_ticks(), 3u);
    EXPECT_EQ(Buff(state, kEnemy, kStrike)->extensions_used(), 1u);
    Round(engine, {{kOwner, Cast(kListener, kEnemy)}});
    EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kEnemy, kStrike)->remaining_ticks(), 2u);
    Round(engine, {{kOwner, Cast(kStrike, kEnemy)}});
    state = engine.BuildStateSnapshot();
    ASSERT_NE(Buff(state, kEnemy, kStrike), nullptr);
    EXPECT_EQ(Buff(state, kEnemy, kStrike)->extensions_used(), 1u);
    EXPECT_EQ(Buff(state, kEnemy, kStrike)->remaining_ticks(), 3u);
    Round(engine, {{kOwner, Cast(kListener, kEnemy)}});
    EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kEnemy, kStrike)->remaining_ticks(), 2u);
}

TEST(SkillTriggerTest, PoisonBurnBurstRequiresBothAndConsumesEachTickOnlyOnce) {
    for (const bool withBurn : {false, true}) {
        SCOPED_TRACE(withBurn);
        auto data = Data();
        data->AddSkill(kStrike).clear_effect(); data->AddSkill(kStrike).add_effect(kStrike);
        AddPoison(*data, kStrike, "Poison", 7);
        if (withBurn) { data->AddSkill(kStrike).add_effect(kChild); AddPoison(*data, kChild, "Burn", 8); }
        Skill(*data, kListener).add_effect(kListener);
        auto& consume = Effect(*data, kListener, BattleEffectOp::ConsumePeriodic);
        consume.set_required_tag("Poison"); consume.set_second_required_tag("Burn"); consume.set_effect_count(1);
        auto request = Request(); Learn(*request.mutable_players(0), kListener, true);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        Round(engine, {{kOwner, Cast(kStrike, kEnemy)}});
        const auto burst = Damage(Round(engine, {{kOwner, Cast(kListener, kEnemy)}}), kOwner, BattleDamageOrigin::Periodic);
        ASSERT_EQ(burst.size(), withBurn ? 4u : 1u); // 每状态一次催动＋一次正常轮末，无烧则不能催动毒。
        for (const auto& event : burst) { EXPECT_EQ(event.value(), 10u); EXPECT_EQ(event.skill_table_id(), kStrike); }
        const auto state = engine.BuildStateSnapshot();
        ASSERT_NE(Buff(state, kEnemy, kStrike), nullptr);
        EXPECT_EQ(Buff(state, kEnemy, kStrike)->remaining_ticks(), withBurn ? 1u : 2u);
        if (withBurn) {
            ASSERT_NE(Buff(state, kEnemy, kChild), nullptr);
            EXPECT_EQ(Buff(state, kEnemy, kChild)->remaining_ticks(), 1u);
            EXPECT_EQ(Damage(Round(engine), kOwner, BattleDamageOrigin::Periodic).size(), 2u);
            EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kEnemy, kStrike), nullptr);
            EXPECT_EQ(Buff(engine.BuildStateSnapshot(), kEnemy, kChild), nullptr);
        }
    }
}

TEST(SkillTriggerTest, PetRetreatIsNotDeathAndOwnerRecallWaitsTwoFullRoundsThenConsumesOneAction) {
    auto data = Data();
    auto& retreatSkill = Skill(*data, kListener);
    retreatSkill.set_caster_mask(2); retreatSkill.add_effect(kListener);
    auto& retreat = Effect(*data, kListener, BattleEffectOp::RetreatPet);
    retreat.set_application_chance(1); retreat.set_duration_rounds(2); retreat.set_state_kind(9);
    auto request = Request(true);
    Learn(*request.mutable_players(0)->mutable_pets(0), kListener, true);
    const auto copiedPet = request.players(0).pets(0);
    auto* otherPet = request.mutable_players(2)->add_pets();
    *otherPet = copiedPet; otherPet->set_pet_id(2001); otherPet->set_owner_player_id(kEnemy);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto caster = PetId(engine), target = PetId(engine, kEnemy);
    ASSERT_NE(caster, 0u); ASSERT_NE(target, 0u);
    EXPECT_NE(engine.ValidateAction(caster, Cast(kListener, kEnemy)), kSuccess);
    const auto result = Round(engine, {{caster, Cast(kListener, target)}});
    auto state = engine.BuildStateSnapshot();
    EXPECT_TRUE(Actor(state, target).fled()); EXPECT_FALSE(Actor(state, target).is_dead());
    EXPECT_EQ(Actor(state, target).attributes().health(), 10000u);
    EXPECT_EQ(Actor(state, target).withdraw_until_round(), 4u);
    EXPECT_EQ(std::count_if(result.events().begin(), result.events().end(), [target](const auto& event) {
        return event.event_type() == BATTLE_EVENT_DEATH && event.target_id() == target;
    }), 0);
    BattleAction recall; recall.set_action_type(BATTLE_ACTION_RECALL_PET); recall.set_target_id(target);
    EXPECT_NE(engine.ValidateAction(kOwner, recall), kSuccess);
    EXPECT_NE(engine.ValidateAction(kEnemy, recall), kSuccess);
    EXPECT_NE(engine.ValidateAction(target, Attack(kOwner)), kSuccess);
    Round(engine);
    EXPECT_NE(engine.ValidateAction(kEnemy, recall), kSuccess);
    Round(engine);
    ASSERT_EQ(engine.ValidateAction(kEnemy, recall), kSuccess);
    const auto returned = Round(engine, {{kEnemy, recall}});
    state = engine.BuildStateSnapshot();
    EXPECT_FALSE(Actor(state, target).fled()); EXPECT_FALSE(Actor(state, target).is_dead());
    EXPECT_TRUE(Damage(returned, target).empty());
    EXPECT_TRUE(Damage(returned, kEnemy).empty());
    EXPECT_EQ(std::count_if(returned.events().begin(), returned.events().end(), [target](const auto& event) {
        return event.event_type() == BATTLE_EVENT_PET_RETREAT && event.target_id() == target && event.value() == 0;
    }), 1);
    EXPECT_EQ(Damage(Round(engine, {{target, Attack(kOwner)}}), target).size(), 1u);
}

TEST(SkillTriggerTest, PoisonRecoveryIgnoresOwnBurnOnAnotherCastersPoisonButAcceptsOwnLethalPoison) {
    for (const bool ownPoison : {false, true}) {
        SCOPED_TRACE(ownPoison);
        auto data = Data();
        auto& recovery = Listen(*data, kListener, kListener, BattleEffectOp::ConditionalHeal, BattleTrigger::AfterPeriodic);
        recovery.set_required_tag("Poison"); recovery.set_value_basis(3); recovery.set_ratio(.04);
        data->AddSkill(kStrike).clear_effect(); data->AddSkill(kStrike).add_effect(kStrike);
        AddPoison(*data, kStrike, "Poison", 7);
        Skill(*data, kChild).add_effect(kChild); AddPoison(*data, kChild, "Burn", 8);
        auto request = Request();
        request.mutable_players(0)->mutable_base_attributes()->set_health(5000);
        if (ownPoison) request.mutable_players(2)->mutable_base_attributes()->set_health(5);
        Learn(*request.mutable_players(0), kListener); Learn(*request.mutable_players(0), kChild, true);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        if (ownPoison) Round(engine, {{kOwner, Cast(kStrike, kEnemy)}});
        else Round(engine, {{kOwner, Cast(kChild, kEnemy)}, {kAlly, Cast(kStrike, kEnemy)}});
        const auto tick = Round(engine);
        const auto state = engine.BuildStateSnapshot();
        EXPECT_EQ(Actor(state, kOwner).attributes().health(), ownPoison ? 5400u : 5000u);
        EXPECT_EQ(Actor(state, kEnemy).is_dead(), ownPoison);
        const auto heals = std::count_if(tick.events().begin(), tick.events().end(), [](const auto& event) {
            return event.event_type() == BATTLE_EVENT_HEAL && event.source_id() == kOwner && event.value() > 0;
        });
        EXPECT_EQ(heals, ownPoison ? 1 : 0);
    }
}

TEST(SkillTriggerTest, HealingReductionApplicationChanceHasBothOutcomesAndDeterministicReplay) {
    auto data = Data();
    Skill(*data, kListener).add_effect(kListener);
    auto& debuff = Effect(*data, kListener, BattleEffectOp::Modifier);
    debuff.set_modifier_stat(static_cast<uint32_t>(BattleModifier::IncomingHeal));
    debuff.set_ratio(-.35); debuff.set_application_chance(.8);
    debuff.set_duration_rounds(5); debuff.set_tick_on_apply(1);
    uint32_t applied = 0, resisted = 0;
    for (uint64_t seed = 1; seed <= 48; ++seed) {
        SCOPED_TRACE(seed);
        auto request = Request(); request.set_seed(seed); Learn(*request.mutable_players(0), kListener, true);
        TurnBattleEngine first(data), replay(data);
        ASSERT_TRUE(first.Initialize(request)); ASSERT_TRUE(replay.Initialize(request));
        const auto result = Round(first, {{kOwner, Cast(kListener, kEnemy)}});
        const auto repeated = Round(replay, {{kOwner, Cast(kListener, kEnemy)}});
        EXPECT_EQ(Canonical(result), Canonical(repeated));
        const auto state = first.BuildStateSnapshot();
        const bool present = Buff(state, kEnemy, kListener) != nullptr;
        const auto failures = std::count_if(result.events().begin(), result.events().end(), [](const auto& event) {
            return event.event_type() == BATTLE_EVENT_RESIST && event.buff_table_id() == kListener;
        });
        EXPECT_EQ(failures, present ? 0 : 1);
        if (present) ++applied; else ++resisted;
    }
    EXPECT_GT(applied, 0u); EXPECT_GT(resisted, 0u);
    EXPECT_EQ(applied + resisted, 48u);
}

TEST(SkillTriggerTest, InitializationRejectsZeroRanksAndWrongSubjectsBeforeAddingActionsOrListeners) {
    auto data = Data();
    Listen(*data, kListener, kListener, BattleEffectOp::ConditionalMana, BattleTrigger::AfterTake);
    Skill(*data, kChild).set_caster_mask(2);
    Skill(*data, kRevive).set_caster_mask(1);
    Skill(*data, kSacrifice);
    auto request = Request(true);
    auto* player = request.mutable_players(0);
    (*player->mutable_skill_ranks())[kStrike] = 0;
    (*player->mutable_skill_ranks())[kListener] = 0;
    Learn(*player, kChild, true);
    player->add_skill_table_ids(kSacrifice); // 人物缺少已学等级，不能仅靠列表获得技能。
    auto* pet = player->mutable_pets(0);
    (*pet->mutable_skill_ranks())[kStrike] = 0;
    (*pet->mutable_skill_ranks())[kListener] = 0;
    Learn(*pet, kRevive, true);
    pet->add_skill_table_ids(kSacrifice); // 旧宝宝列表兼容：缺键可补1，显式0不可补。
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto petId = PetId(engine);
    const auto state = engine.BuildStateSnapshot();
    EXPECT_NE(engine.ValidateAction(kOwner, Cast(kStrike, kEnemy)), kSuccess);
    EXPECT_NE(engine.ValidateAction(petId, Cast(kStrike, kEnemy)), kSuccess);
    EXPECT_NE(engine.ValidateAction(kOwner, Cast(kChild, kEnemy)), kSuccess);
    EXPECT_NE(engine.ValidateAction(petId, Cast(kRevive, kEnemy)), kSuccess);
    EXPECT_NE(engine.ValidateAction(kOwner, Cast(kSacrifice, kEnemy)), kSuccess);
    EXPECT_EQ(engine.ValidateAction(petId, Cast(kSacrifice, kEnemy)), kSuccess);
    EXPECT_EQ(Buff(state, kOwner, kListener), nullptr);
    EXPECT_EQ(Buff(state, petId, kListener), nullptr);
    EXPECT_FALSE(Actor(state, kOwner).skill_ranks().contains(kStrike));
    EXPECT_FALSE(Actor(state, petId).skill_ranks().contains(kStrike));
}

TEST(SkillTriggerTest, NegativeIncomingDamageIsBenefitForModernAndLegacyDispelClassification) {
    // 0=A33式净化，1=旧职业净化，2=A34式驱散；O02减伤是负数配置的友方增益。
    for (uint32_t mode = 0; mode < 3; ++mode) {
        SCOPED_TRACE(mode);
        auto data = Data(); AddOpeningGuard(*data);
        auto request = Request(); Learn(*request.mutable_players(0), kListener);
        const uint64_t caster = mode == 2 ? kEnemy : kOwner;
        auto* casterSnapshot = request.mutable_players(mode == 2 ? 2 : 0);
        auto& dispel = Skill(*data, kChild, 1, mode == 2 ? 1 : 2);
        if (mode == 1) {
            dispel.set_battle_model(0); dispel.set_class_id(1); dispel.set_dispel_count(10);
            casterSnapshot->set_class_id(1);
        } else {
            dispel.add_effect(kChild);
            Effect(*data, kChild, mode == 2 ? BattleEffectOp::DispelBuff : BattleEffectOp::DispelDebuff).set_effect_count(10);
        }
        Learn(*casterSnapshot, kChild, true);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        const auto initial = engine.BuildStateSnapshot();
        ASSERT_NE(Buff(initial, kAlly, kListener), nullptr);
        const auto instance = Buff(initial, kAlly, kListener)->buff_id();
        const auto result = Round(engine, {{caster, Cast(kChild, kAlly)}});
        const auto state = engine.BuildStateSnapshot();
        const auto* remaining = Buff(state, kAlly, kListener);
        if (mode == 2) EXPECT_EQ(remaining, nullptr);
        else {
            ASSERT_NE(remaining, nullptr);
            EXPECT_EQ(remaining->buff_id(), instance);
            EXPECT_EQ(remaining->remain_rounds(), 1u);
        }
        EXPECT_EQ(std::count_if(result.events().begin(), result.events().end(), [instance](const auto& event) {
            return event.event_type() == BATTLE_EVENT_BUFF_REMOVE && event.buff_instance_id() == instance;
        }), mode == 2 ? 1 : 0);
    }
}

TEST(SkillTriggerTest, PermanentManaDiscountSurvivesLegacyCleanseAndMatchingTagDispel) {
    for (const bool byTag : {false, true}) {
        SCOPED_TRACE(byTag);
        auto data = Data();
        Skill(*data, kListener, 2, 3).add_init_effect(kListener);
        auto& discount = Effect(*data, kListener, BattleEffectOp::Modifier);
        discount.set_modifier_stat(static_cast<uint32_t>(BattleModifier::ManaCost));
        discount.set_ratio(-.15); discount.set_infinite_duration(1);
        // 主动给监听匹配标签，证明保护来自永久被动身份，而非碰巧没匹配上。
        (*discount.mutable_tag())["Buff"] = true;
        data->AddSkill(kStrike).set_mana_percent(.1);
        auto& legacy = Skill(*data, kChild, 1, byTag ? 1 : 2);
        legacy.set_battle_model(0); legacy.set_class_id(1);
        if (byTag) {
            legacy.add_effect(kChild);
            auto& dispel = data->AddBuff(kChild);
            dispel.set_buff_type(turnbattle::kBuffTypeDispel);
            (*dispel.mutable_dispel_tag())["Buff"] = true;
        } else legacy.set_dispel_count(10);
        auto request = Request(); Learn(*request.mutable_players(0), kListener);
        auto* caster = request.mutable_players(byTag ? 2 : 1);
        caster->set_class_id(1); Learn(*caster, kChild, true);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        const auto initial = engine.BuildStateSnapshot();
        ASSERT_NE(Buff(initial, kOwner, kListener), nullptr);
        const auto instance = Buff(initial, kOwner, kListener)->buff_id();
        const auto cleansing = Round(engine, {{byTag ? kEnemy : kAlly, Cast(kChild, kOwner)}});
        const auto state = engine.BuildStateSnapshot();
        ASSERT_NE(Buff(state, kOwner, kListener), nullptr);
        EXPECT_EQ(Buff(state, kOwner, kListener)->buff_id(), instance);
        EXPECT_EQ(std::count_if(cleansing.events().begin(), cleansing.events().end(), [](const auto& event) {
            return event.event_type() == BATTLE_EVENT_BUFF_REMOVE && event.buff_table_id() == kListener;
        }), 0);
        Round(engine, {{kOwner, Cast(kStrike, kEnemy)}});
        EXPECT_EQ(Actor(engine.BuildStateSnapshot(), kOwner).attributes().mana(), 915u);
    }
}
} // namespace
