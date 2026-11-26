#include <gtest/gtest.h>

#include <algorithm>
#include <cmath>
#include <filesystem>
#include <map>
#include <memory>
#include <set>
#include <string>
#include <vector>
#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>

#include "../test_config_helper.h"
#include "constants/battle_effect_constants.h"
#include "memory_battle_data_provider.h"
#include "system/battle_actor_command_rules.h"
#include "system/turn_battle_engine.h"
#include "table/code/skill_table.h"
#include "table/code/buff_table.h"
#include "table/code/monster_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"

namespace {
using turnbattle::TurnBattleEngine;
using turnbattle::BattleEffectOp;
constexpr uint64_t kSelf = 101, kAlly = 102, kEnemy = 201;

BattleAction Cast(uint32_t skill, uint64_t target) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_SKILL);
    action.set_skill_table_id(skill);
    action.set_target_id(target);
    return action;
}
BattleAction Action(eBattleActionType type, uint64_t target = 0) {
    BattleAction action;
    action.set_action_type(type);
    action.set_target_id(target);
    return action;
}
std::string Canonical(const google::protobuf::MessageLite& value) {
    std::string output;
    google::protobuf::io::StringOutputStream buffer(&output);
    {
        google::protobuf::io::CodedOutputStream stream(&buffer);
        stream.SetSerializationDeterministic(true);
        EXPECT_TRUE(value.SerializeToCodedStream(&stream));
    }
    return output;
}
const BattleActorState& Actor(const BattleStateS2C& state, uint64_t id) {
    for (const auto& actor : state.actors()) if (actor.actor_id() == id) return actor;
    ADD_FAILURE() << "missing actor " << id;
    return BattleActorState::default_instance();
}
uint64_t PetActor(const BattleStateS2C& state, uint64_t owner) {
    for (const auto& actor : state.actors())
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && actor.owner_player_id() == owner) return actor.actor_id();
    return 0;
}
BattlePetSnapshot* AddPet(BattlePlayerSnapshot& owner) {
    auto* pet = owner.add_pets();
    pet->set_pet_id(owner.player_id() + 1000);
    pet->set_owner_player_id(owner.player_id());
    pet->set_pet_table_id(1);
    pet->set_level(30);
    pet->set_max_health(10000);
    pet->set_max_mana(10000);
    pet->set_physical_attack(500);
    pet->set_magic_attack(600);
    pet->mutable_base_attributes()->set_health(5000);
    pet->mutable_base_attributes()->set_mana(10000);
    pet->mutable_base_attributes()->set_speed(100);
    return pet;
}
CreateBattleRequest Request(uint32_t eachSide = 2, bool withPets = false) {
    CreateBattleRequest request;
    request.set_battle_id(70001);
    request.set_match_mode(3);
    request.set_seed(20261117);
    for (uint32_t side = 0; side < 2; ++side) {
        for (uint32_t i = 0; i < eachSide; ++i) {
            auto* player = request.add_players();
            player->set_player_id(101 + side * 100 + i);
            player->set_team_index(side);
            player->set_level(30);
            player->set_class_id(1);
            player->set_max_health(10000);
            player->set_max_mana(10000);
            player->set_physical_attack(500);
            player->set_magic_attack(600);
            player->mutable_base_attributes()->set_health(5000);
            player->mutable_base_attributes()->set_mana(10000);
            player->mutable_base_attributes()->set_speed(side == 0 ? 200 : 50);
            if (withPets) AddPet(*player);
        }
    }
    return request;
}
void Learn(BattlePlayerSnapshot& player, const SkillTable& row) {
    (*player.mutable_skill_ranks())[row.id()] = 1;
    if (row.ability_kind() == 1) player.add_skill_table_ids(row.id());
}
void Learn(BattlePetSnapshot& pet, const SkillTable& row) {
    (*pet.mutable_skill_ranks())[row.id()] = 1;
    if (row.ability_kind() == 1) pet.add_skill_table_ids(row.id());
}
void DefendAllExcept(TurnBattleEngine& engine, uint64_t except = 0) {
    const auto state = engine.BuildStateSnapshot();
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() != except && !actor.is_dead() && !actor.fled())
            engine.SubmitAction(actor.actor_id(), Action(BATTLE_ACTION_DEFEND));
    }
}

TEST(BattleActorCommandRulesTest, OnlyTicketOwnerCanCommandSelfOrOwnPet) {
    BattleStateS2C state;
    state.set_battle_id(9);
    state.set_round_index(3);
    auto* player = state.add_actors();
    player->set_actor_id(101);
    player->set_actor_type(BATTLE_ACTOR_TYPE_PLAYER);
    auto* ally = state.add_actors();
    ally->set_actor_id(102);
    ally->set_actor_type(BATTLE_ACTOR_TYPE_PLAYER);
    auto* pet = state.add_actors();
    pet->set_actor_id(1001);
    pet->set_actor_type(BATTLE_ACTOR_TYPE_PET);
    pet->set_owner_player_id(101);
    SubmitBattleActionRequest request;
    request.set_battle_id(9);
    *request.mutable_action() = Action(BATTLE_ACTION_ATTACK, 200);
    EXPECT_TRUE(turnbattle::CanCommandBattleActor(state, 101, request));
    request.set_round_index(3);
    request.set_actor_id(1001);
    EXPECT_TRUE(turnbattle::CanCommandBattleActor(state, 101, request));
    EXPECT_FALSE(turnbattle::CanCommandBattleActor(state, 102, request));
    request.set_actor_id(102);
    EXPECT_FALSE(turnbattle::CanCommandBattleActor(state, 101, request));
    request.set_actor_id(1001);
    request.set_round_index(2);
    EXPECT_FALSE(turnbattle::CanCommandBattleActor(state, 101, request));
    request.set_round_index(0);
    EXPECT_FALSE(turnbattle::CanCommandBattleActor(state, 101, request));
    request.set_round_index(3);
    for (const auto forbidden : {BATTLE_ACTION_ITEM, BATTLE_ACTION_CAPTURE, BATTLE_ACTION_FLEE}) {
        request.mutable_action()->set_action_type(forbidden);
        EXPECT_FALSE(turnbattle::CanCommandBattleActor(state, 101, request));
    }
}

class ModernSkillCatalogTest : public ::testing::Test {
protected:
    static inline std::filesystem::path oldDirectory;
    static inline bool loaded = false;
    static void SetUpTestSuite() {
        oldDirectory = std::filesystem::current_path();
        loaded = test_config::FindAndLoadTestConfig(0, nullptr);
        if (loaded) {
            SkillTableManager::Instance().Load();
            BuffTableManager::Instance().Load();
            MonsterTableManager::Instance().Load();
        }
    }
    static void TearDownTestSuite() { std::filesystem::current_path(oldDirectory); }
    void SetUp() override {
        ASSERT_TRUE(loaded);
        data = std::make_shared<MemoryBattleDataProvider>();
        data->AddBattleReward(0);
        data->AddPet(1);
        SkillTableManager::Instance().SetDamageParam({30.0});
        for (const auto& [id, row] : SkillTableManager::Instance().GetIdMap()) {
            data->AddSkill(id) = *row;
            data->SetSkillDamage(id, SkillTableManager::Instance().GetDamage(id));
            if (row->battle_model() == 1 && row->season_id() == 0) catalog[id] = *row;
        }
        for (const auto& [id, row] : BuffTableManager::Instance().GetIdMap()) data->AddBuff(id) = *row;
        for (const auto& [id, row] : MonsterTableManager::Instance().GetIdMap()) data->AddMonster(id) = *row;
        ASSERT_EQ(catalog.size(), 95u);
    }
    const SkillTable& Row(uint32_t id) { return catalog.at(id); }
    std::shared_ptr<MemoryBattleDataProvider> data;
    std::map<uint32_t, SkillTable> catalog;
};

TEST_F(ModernSkillCatalogTest, ExactAgreedCatalogHasExecutableEffectsAndSharedIdentity) {
    const std::map<char, std::vector<uint32_t>> expected = {
        {'A',{2,3,6,8,10,12,14,15,16,18,19,20,21,22,23,24,25,26,27,29,30,31,33,34,35,36,38,39,40,41,
              101,102,103,104,105,106,107,108,109,110,111,112,113,114,115,116}},
        {'B',{5,6,7,8,9,11,12,13,14,15,16,101,102}},
        {'P',{1,2,3,4,5,6,7,8,9,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,101,102}},
        {'T',{1,5,6,101,102}}, {'O',{1,2,3,101,102}}};
    const std::map<char,uint32_t> bases = {{'A',10000},{'B',11000},{'P',12000},{'T',13000},{'O',14000}};
    std::set<std::string> codes;
    for (const auto& [prefix, numbers] : expected) {
        for (const auto number : numbers) {
            const auto id = bases.at(prefix) + number;
            ASSERT_TRUE(catalog.contains(id)) << id;
            const auto& row = Row(id);
            EXPECT_EQ(row.class_id(), 0u);
            EXPECT_GE(row.caster_mask(), 1u);
            EXPECT_LE(row.caster_mask(), 3u);
            EXPECT_TRUE(codes.insert(row.prototype_code()).second);
            EXPECT_FALSE(row.name().empty());
            EXPECT_FALSE(row.description().empty());
            EXPECT_GT(row.max_rank(), 0u);
            EXPECT_TRUE(row.effect_size() || row.init_effect_size() || !row.passive_stat().empty()) << id;
            const auto validate = [&](uint32_t buffId) {
                const auto* buff = data->FindBuff(buffId);
                ASSERT_NE(buff, nullptr) << id << " -> " << buffId;
                EXPECT_GT(buff->battle_op(), 0u);
                EXPECT_LT(buff->battle_op(), static_cast<uint32_t>(BattleEffectOp::kCount));
                EXPECT_TRUE(std::isfinite(buff->ratio()));
                for (const auto child : buff->sub_buff()) EXPECT_NE(data->FindBuff(child), nullptr);
                for (const auto child : buff->target_sub_buff()) EXPECT_NE(data->FindBuff(child), nullptr);
            };
            for (const auto buffId : row.effect()) validate(buffId);
            for (const auto buffId : row.init_effect()) validate(buffId);
        }
    }
    const std::set<uint32_t> petShared = {2,6,10,14,16,18,19,25,26,27,33,34};
    for (const auto number : expected.at('A'))
        EXPECT_EQ((Row(10000 + number).caster_mask() & 2u) != 0, petShared.contains(number));
    EXPECT_EQ(Row(10002).max_targets(), 5u);
    EXPECT_EQ(Row(10002).pet_max_targets(), 2u);
    EXPECT_EQ(Row(10010).max_targets(), 7u);
    EXPECT_EQ(Row(10010).pet_max_targets(), 1u);
    EXPECT_EQ(Row(10018).caster_mask(), 2u);
    EXPECT_EQ(Row(10018).max_targets(), 2u);
    EXPECT_EQ(Row(10018).pet_max_targets(), 2u);
    EXPECT_EQ(Row(11012).max_targets(), 2u);
    EXPECT_EQ(Row(11012).pet_max_targets(), 2u);
    EXPECT_EQ(Row(13001).caster_mask(), 2u);
    EXPECT_EQ(Row(10040).caster_mask(), 1u);
    EXPECT_EQ(Row(10040).name(), "太清灵盾");
    EXPECT_EQ(Row(10041).caster_mask(), 1u);
    EXPECT_EQ(Row(10041).forbid_copy(), 1u);
    EXPECT_EQ(Row(10115).forbid_copy(), 1u);
    EXPECT_EQ(Row(10116).forbid_copy(), 1u);
    EXPECT_EQ(Row(12101).exclusive_group(), 1u);
    EXPECT_EQ(Row(12102).exclusive_group(), 1u);
    EXPECT_EQ(Row(11013).forbid_copy(), 1u);
    EXPECT_EQ(Row(11015).forbid_copy(), 1u);
    EXPECT_EQ(Row(11016).forbid_copy(), 1u);
    for (const auto retired : {10007u, 10017u, 10028u, 10032u, 10037u, 12010u, 13002u, 13003u})
        EXPECT_FALSE(catalog.contains(retired)) << retired;
}

TEST_F(ModernSkillCatalogTest, EveryActivePrototypeCanBeCastByEveryEligibleActor) {
    size_t casts = 0;
    for (const auto& [id, row] : catalog) {
        if (row.ability_kind() != 1) continue;
        for (const auto mask : {1u, 2u}) {
            if ((row.caster_mask() & mask) == 0) continue;
            SCOPED_TRACE(std::to_string(id) + "/actor-mask=" + std::to_string(mask));
            auto request = Request(2, true);
            if (mask == 1) Learn(*request.mutable_players(0), row);
            else Learn(*request.mutable_players(0)->mutable_pets(0), row);
            TurnBattleEngine engine(data);
            ASSERT_TRUE(engine.Initialize(request));
            const auto state = engine.BuildStateSnapshot();
            const auto source = mask == 1 ? kSelf : PetActor(state, kSelf);
            uint64_t target = row.target_team() == 1 ? PetActor(state, kEnemy) : kAlly;
            if (row.target_team() == 3) target = source;
            const auto action = Cast(id, target);
            ASSERT_EQ(engine.ValidateAction(source, action), kSuccess);
            DefendAllExcept(engine, source);
            engine.SubmitAction(source, action);
            const auto result = engine.ResolveCurrentRound();
            EXPECT_TRUE(std::any_of(result.events().begin(), result.events().end(), [&](const auto& event) {
                return event.event_type() == BATTLE_EVENT_SKILL && event.source_id() == source && event.skill_table_id() == id;
            }));
            ++casts;
        }
    }
    EXPECT_EQ(casts, 70u); // 既有52 + 策略人物14 + 率土蓄力2 + 策略宝宝2。
}

TEST_F(ModernSkillCatalogTest, SharedAttackPreservesMainTargetAndUsesCasterTargetCap) {
    for (const auto mask : {1u, 2u}) {
        auto request = Request(5, true);
        if (mask == 1) Learn(*request.mutable_players(0), Row(10002));
        else Learn(*request.mutable_players(0)->mutable_pets(0), Row(10002));
        TurnBattleEngine engine(data), replay(data);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_TRUE(replay.Initialize(request));
        const auto source = mask == 1 ? kSelf : PetActor(engine.BuildStateSnapshot(), kSelf);
        for (auto* run : {&engine, &replay}) {
            DefendAllExcept(*run, source);
            run->SubmitAction(source, Cast(10002, 205));
        }
        const auto result = engine.ResolveCurrentRound();
        EXPECT_EQ(Canonical(result), Canonical(replay.ResolveCurrentRound()));
        std::set<uint64_t> victims;
        for (const auto& event : result.events())
            if (event.event_type() == BATTLE_EVENT_DAMAGE && event.source_id() == source) victims.insert(event.target_id());
        EXPECT_EQ(victims.size(), mask == 1 ? 5u : 2u);
        EXPECT_TRUE(victims.contains(205));
    }
}

TEST_F(ModernSkillCatalogTest, CharacterOnlySkillCannotBeSmuggledIntoPetSnapshot) {
    auto request = Request(2, true);
    Learn(*request.mutable_players(0)->mutable_pets(0), Row(10040));
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto source = PetActor(engine.BuildStateSnapshot(), kSelf);
    EXPECT_NE(engine.ValidateAction(source, Cast(10040, kSelf)), kSuccess);
}

TEST_F(ModernSkillCatalogTest, ShieldAbsorbsDirectDamageButPoisonStillLosesHealth) {
    auto request = Request();
    Learn(*request.mutable_players(0), Row(10040));
    Learn(*request.mutable_players(2), Row(10002));
    Learn(*request.mutable_players(2), Row(10019));
    request.mutable_players(2)->set_physical_attack(50);
    data->AddBuff(100190).set_application_chance(1);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    DefendAllExcept(engine);
    engine.SubmitAction(kSelf, Cast(10040, kSelf));
    engine.SubmitAction(kEnemy, Cast(10002, kSelf));
    auto result = engine.ResolveCurrentRound();
    uint64_t absorbed = 0;
    for (const auto& event : result.events())
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.target_id() == kSelf) absorbed += event.absorbed();
    EXPECT_GT(absorbed, 0u);
    EXPECT_EQ(Actor(result.state(), kSelf).attributes().health(), 5000u);
    DefendAllExcept(engine);
    engine.SubmitAction(kEnemy, Cast(10019, kSelf));
    result = engine.ResolveCurrentRound();
    bool poison = false;
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.target_id() == kSelf
            && event.damage_origin() == static_cast<uint32_t>(turnbattle::BattleDamageOrigin::Periodic)) {
            poison = true;
            EXPECT_GT(event.value(), 0u);
            EXPECT_EQ(event.absorbed(), 0u);
        }
    }
    EXPECT_TRUE(poison);
    EXPECT_LT(Actor(result.state(), kSelf).attributes().health(), 5000u);
}

TEST_F(ModernSkillCatalogTest, ShieldMustNotTruncateIncomingDamageToCurrentHealthBeforeAbsorption) {
    auto request = Request();
    request.mutable_players(0)->mutable_base_attributes()->set_health(1);
    Learn(*request.mutable_players(0), Row(10040));
    Learn(*request.mutable_players(2), Row(10002));
    request.mutable_players(2)->set_physical_attack(10000);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    DefendAllExcept(engine);
    engine.SubmitAction(kSelf, Cast(10040, kSelf));
    engine.SubmitAction(kEnemy, Cast(10002, kSelf));
    const auto result = engine.ResolveCurrentRound();
    EXPECT_TRUE(Actor(result.state(), kSelf).is_dead());
    bool damage = false;
    for (const auto& event : result.events()) if (event.event_type() == BATTLE_EVENT_DAMAGE && event.target_id() == kSelf) {
        damage = true;
        EXPECT_GT(event.absorbed(), 1u);
        EXPECT_EQ(event.value(), 1u);
    }
    EXPECT_TRUE(damage);
}

TEST_F(ModernSkillCatalogTest, LifestealUsesActualHealthLossAndNeverAbsorbedDamage) {
    for (const bool shielded : {false, true}) {
        auto request = Request(2, true);
        Learn(*request.mutable_players(0)->mutable_pets(0), Row(11012));
        Learn(*request.mutable_players(2), Row(10040));
        request.mutable_players(2)->set_magic_attack(100000);
        request.mutable_players(2)->mutable_base_attributes()->set_speed(500);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        const auto source = PetActor(engine.BuildStateSnapshot(), kSelf);
        DefendAllExcept(engine);
        if (shielded) engine.SubmitAction(kEnemy, Cast(10040, kEnemy));
        engine.SubmitAction(source, Cast(11012, kEnemy));
        const auto result = engine.ResolveCurrentRound();
        uint64_t damage = 0, healed = 0;
        std::set<uint64_t> victims;
        for (const auto& event : result.events()) {
            if (event.source_id() != source) continue;
            if (event.event_type() == BATTLE_EVENT_DAMAGE) { damage += event.value(); victims.insert(event.target_id()); }
            if (event.event_type() == BATTLE_EVENT_HEAL && event.target_id() == source) healed += event.value();
        }
        EXPECT_EQ(victims.size(), 2u);
        if (shielded) { EXPECT_EQ(damage, 0u); EXPECT_EQ(healed, 0u); }
        else { EXPECT_GT(damage, 0u); EXPECT_GT(healed, 0u); EXPECT_LE(healed, damage); }
        EXPECT_EQ(Actor(result.state(), source).attributes().health(), 5000u + healed);
    }
}

TEST_F(ModernSkillCatalogTest, ReviveKeepsActorIdentityAndConsumesOnlyDeadTargetsMark) {
    auto request = Request();
    request.mutable_players(1)->mutable_base_attributes()->set_health(1);
    Learn(*request.mutable_players(0), Row(10026));
    Learn(*request.mutable_players(2), Row(10002));
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto initialSlot = Actor(engine.BuildStateSnapshot(), kAlly).formation_slot();
    DefendAllExcept(engine);
    engine.SubmitAction(kSelf, Cast(10026, kAlly));
    engine.SubmitAction(kEnemy, Cast(10002, kAlly));
    const auto result = engine.ResolveCurrentRound();
    bool died = false, revived = false;
    for (const auto& event : result.events()) if (event.target_id() == kAlly) {
        if (event.event_type() == BATTLE_EVENT_DEATH) died = true;
        if (event.event_type() == BATTLE_EVENT_REVIVE) { EXPECT_TRUE(died); revived = true; }
    }
    EXPECT_TRUE(died);
    EXPECT_TRUE(revived);
    const auto& ally = Actor(result.state(), kAlly);
    EXPECT_FALSE(ally.is_dead());
    EXPECT_GT(ally.attributes().health(), 0u);
    EXPECT_EQ(ally.formation_slot(), initialSlot);
    EXPECT_FALSE(std::any_of(ally.buffs().begin(), ally.buffs().end(), [](const auto& buff) { return buff.buff_table_id() == 100260; }));
    const auto& caster = Actor(result.state(), kSelf);
    EXPECT_TRUE(std::any_of(caster.buffs().begin(), caster.buffs().end(), [](const auto& buff) { return buff.buff_table_id() == 100260; }));
}

TEST_F(ModernSkillCatalogTest, DeadCharacterRemainsSelectableOnlyByReviveAbility) {
    auto request = Request();
    request.mutable_players(1)->mutable_base_attributes()->set_health(1);
    Learn(*request.mutable_players(0), Row(10026));
    Learn(*request.mutable_players(0), Row(10010));
    Learn(*request.mutable_players(2), Row(10002));
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    DefendAllExcept(engine);
    engine.SubmitAction(kEnemy, Cast(10002, kAlly));
    auto result = engine.ResolveCurrentRound();
    ASSERT_TRUE(Actor(result.state(), kAlly).is_dead());
    EXPECT_EQ(engine.ValidateAction(kSelf, Cast(10026, kAlly)), kSuccess);
    EXPECT_NE(engine.ValidateAction(kSelf, Cast(10010, kAlly)), kSuccess);
    DefendAllExcept(engine);
    engine.SubmitAction(kSelf, Cast(10026, kAlly));
    result = engine.ResolveCurrentRound();
    EXPECT_FALSE(Actor(result.state(), kAlly).is_dead());
    EXPECT_GT(Actor(result.state(), kAlly).attributes().health(), 0u);
}

TEST_F(ModernSkillCatalogTest, PoisonCatalystCanConsumeAlliedPoisonWithoutChangingOriginalSource) {
    auto request = Request();
    Learn(*request.mutable_players(0), Row(10019));
    Learn(*request.mutable_players(0), Row(10030));
    Learn(*request.mutable_players(1), Row(10031));
    data->AddBuff(100190).set_application_chance(1);
    data->AddBuff(100301).set_application_chance(1);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    DefendAllExcept(engine);
    engine.SubmitAction(kSelf, Cast(10019, kEnemy));
    engine.ResolveCurrentRound();
    DefendAllExcept(engine);
    engine.SubmitAction(kSelf, Cast(10030, kEnemy));
    engine.ResolveCurrentRound();
    DefendAllExcept(engine);
    engine.SubmitAction(kAlly, Cast(10031, kEnemy));
    const auto result = engine.ResolveCurrentRound();
    size_t catalyzed = 0;
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.target_id() == kEnemy && event.instigator_id() == kAlly) {
            EXPECT_EQ(event.source_id(), kSelf);
            EXPECT_GT(event.value(), 0u);
            ++catalyzed;
        }
    }
    EXPECT_EQ(catalyzed, 2u);
    const auto& enemy = Actor(result.state(), kEnemy);
    EXPECT_FALSE(std::any_of(enemy.buffs().begin(), enemy.buffs().end(), [](const auto& buff) { return buff.buff_table_id() == 100190; }));
}

TEST_F(ModernSkillCatalogTest, FreezeBlocksDamageAndNewControlUntilItExpires) {
    auto request = Request();
    Learn(*request.mutable_players(0), Row(10038));
    Learn(*request.mutable_players(1), Row(10002));
    Learn(*request.mutable_players(1), Row(10024));
    data->AddBuff(100380).set_application_chance(1);
    data->AddBuff(100240).set_application_chance(1);
    request.mutable_players(1)->mutable_base_attributes()->set_speed(150);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    DefendAllExcept(engine);
    engine.SubmitAction(kSelf, Cast(10038, kEnemy));
    engine.SubmitAction(kAlly, Cast(10002, kEnemy));
    auto result = engine.ResolveCurrentRound();
    EXPECT_EQ(Actor(result.state(), kEnemy).attributes().health(), 5000u);
    DefendAllExcept(engine);
    engine.SubmitAction(kAlly, Cast(10024, kEnemy));
    result = engine.ResolveCurrentRound();
    const auto& enemy = Actor(result.state(), kEnemy);
    EXPECT_FALSE(std::any_of(enemy.buffs().begin(), enemy.buffs().end(), [](const auto& buff) { return buff.buff_table_id() == 100240; }));
}

TEST_F(ModernSkillCatalogTest, LifeToManaDrainPaysHealthOnceAndRejectsInvalidTargetBeforePayment) {
    auto request = Request(2, true);
    Learn(*request.mutable_players(0)->mutable_pets(0), Row(11013));
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto source = PetActor(engine.BuildStateSnapshot(), kSelf);
    EXPECT_NE(engine.ValidateAction(source, Cast(11013, kSelf)), kSuccess);
    EXPECT_EQ(Actor(engine.BuildStateSnapshot(), source).attributes().health(), 5000u);
    DefendAllExcept(engine);
    engine.SubmitAction(source, Cast(11013, kEnemy));
    const auto result = engine.ResolveCurrentRound();
    EXPECT_EQ(Actor(result.state(), source).attributes().health(), 1u);
    EXPECT_FALSE(Actor(result.state(), source).is_dead());
    EXPECT_EQ(Actor(result.state(), kEnemy).attributes().mana(), 10000u - 6248u);
}

TEST_F(ModernSkillCatalogTest, SacrificeFinishesAcceptedHealAfterCasterDies) {
    auto request = Request(2, true);
    request.mutable_players(0)->mutable_base_attributes()->set_health(1000);
    Learn(*request.mutable_players(0)->mutable_pets(0), Row(11016));
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto source = PetActor(engine.BuildStateSnapshot(), kSelf);
    EXPECT_NE(engine.ValidateAction(source, Cast(11016, source)), kSuccess);
    DefendAllExcept(engine);
    engine.SubmitAction(source, Cast(11016, kSelf));
    const auto result = engine.ResolveCurrentRound();
    EXPECT_TRUE(Actor(result.state(), source).is_dead());
    EXPECT_EQ(Actor(result.state(), source).attributes().health(), 0u);
    EXPECT_EQ(Actor(result.state(), kSelf).attributes().health(), 7000u);
    EXPECT_EQ(Actor(result.state(), source).ability_use_counts().at(11016), 1u);
}

TEST_F(ModernSkillCatalogTest, TemporaryMaximumHealthExpiresAndClampsWithoutPermanentGrowth) {
    auto request = Request(2, true);
    Learn(*request.mutable_players(0)->mutable_pets(0), Row(11015));
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto source = PetActor(engine.BuildStateSnapshot(), kSelf);
    DefendAllExcept(engine);
    engine.SubmitAction(source, Cast(11015, source));
    auto result = engine.ResolveCurrentRound();
    EXPECT_EQ(Actor(result.state(), source).max_health(), 15000u);
    EXPECT_EQ(Actor(result.state(), source).attributes().health(), 10000u);
    for (int i = 0; i < 2; ++i) { DefendAllExcept(engine); result = engine.ResolveCurrentRound(); }
    EXPECT_EQ(Actor(result.state(), source).max_health(), 10000u);
    EXPECT_LE(Actor(result.state(), source).attributes().health(), 10000u);
}

TEST_F(ModernSkillCatalogTest, ManaShieldSpillsDamageWhenRecipientsManaRunsOut) {
    auto request = Request(2, true);
    request.mutable_players(1)->mutable_base_attributes()->set_mana(1);
    Learn(*request.mutable_players(0)->mutable_pets(0), Row(11014));
    Learn(*request.mutable_players(2), Row(10002));
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto source = PetActor(engine.BuildStateSnapshot(), kSelf);
    DefendAllExcept(engine);
    engine.SubmitAction(source, Cast(11014, kAlly));
    engine.SubmitAction(kEnemy, Cast(10002, kAlly));
    const auto result = engine.ResolveCurrentRound();
    EXPECT_EQ(Actor(result.state(), kAlly).attributes().mana(), 0u);
    bool found = false;
    for (const auto& event : result.events())
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.target_id() == kAlly) {
            found = true; EXPECT_EQ(event.absorbed(), 1u); EXPECT_GT(event.value(), 0u);
        }
    EXPECT_TRUE(found);
}

TEST_F(ModernSkillCatalogTest, EmergencyHealHasLowerBaseAndHigherLowHealthRecovery) {
    std::vector<uint64_t> recovery;
    for (const auto health : {5000u, 1000u}) for (const auto skill : {10010u, 10012u}) {
        auto request = Request();
        request.mutable_players(1)->mutable_base_attributes()->set_health(health);
        Learn(*request.mutable_players(0), Row(skill));
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        DefendAllExcept(engine);
        engine.SubmitAction(kSelf, Cast(skill, kAlly));
        const auto result = engine.ResolveCurrentRound();
        recovery.push_back(Actor(result.state(), kAlly).attributes().health() - health);
    }
    ASSERT_EQ(recovery.size(), 4u);
    EXPECT_LT(recovery[1], recovery[0]);
    EXPECT_GT(recovery[3], recovery[2]);
}

TEST_F(ModernSkillCatalogTest, CopyTargetSkillMirrorsPlannedEnemySkillWithOwnStatsRankScaleAndNoRecursion) {
    // 对手挂 100% 触发的 P19 法术反击，验证复制施放（Mirror）不触发递归反击。
    data->AddBuff(120190).set_trigger_chance(1.0);
    auto run = [&](uint32_t copyRank) {
        auto request = Request();
        request.mutable_players(0)->set_magic_attack(1000); // 施法者高法攻
        request.mutable_players(2)->set_magic_attack(100);  // 目标低法攻，验证走施法者自身属性
        Learn(*request.mutable_players(0), Row(10041));
        (*request.mutable_players(0)->mutable_skill_ranks())[10041] = copyRank;
        Learn(*request.mutable_players(2), Row(10006));
        Learn(*request.mutable_players(2), Row(12019));
        TurnBattleEngine engine(data);
        EXPECT_TRUE(engine.Initialize(request));
        DefendAllExcept(engine);
        engine.SubmitAction(kSelf, Cast(10041, kEnemy));
        engine.SubmitAction(kEnemy, Cast(10006, kSelf));
        return engine.ResolveCurrentRound();
    };
    const auto rank1 = run(1);
    const auto rank1Repeat = run(1);
    const auto rank10 = run(10);
    EXPECT_EQ(Canonical(rank1), Canonical(rank1Repeat));

    // 只扣 A41 自身 10% 法力（1000）并仅让 10041 进入 CD（3），不扣 10006 冷却。
    EXPECT_EQ(Actor(rank1.state(), kSelf).attributes().mana(), 9000u);
    EXPECT_EQ(Actor(rank1.state(), kSelf).skill_cooldown_rounds().at(10041), 2u);
    EXPECT_FALSE(Actor(rank1.state(), kSelf).skill_cooldown_rounds().contains(10006));

    int copyCastEvents = 0, mirroredCastEvents = 0, enemyMirrorCounterEvents = 0;
    uint64_t rank1Damage = 0, rank10Damage = 0;
    for (const auto& ev : rank1.events()) {
        if (ev.event_type() == BATTLE_EVENT_SKILL && ev.source_id() == kSelf && ev.skill_table_id() == 10041) {
            ++copyCastEvents;
            EXPECT_EQ(ev.hit_kind(), BATTLE_HIT_NORMAL);
        }
        if (ev.event_type() == BATTLE_EVENT_SKILL && ev.source_id() == kSelf && ev.skill_table_id() == 10006) {
            ++mirroredCastEvents;
            EXPECT_EQ(ev.hit_kind(), BATTLE_HIT_MIRROR);
        }
        if (ev.event_type() == BATTLE_EVENT_SKILL && ev.source_id() == kEnemy && ev.hit_kind() == BATTLE_HIT_MIRROR) {
            ++enemyMirrorCounterEvents;
        }
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == kSelf && ev.target_id() == kEnemy &&
            ev.skill_table_id() == 10006 && ev.hit_kind() == BATTLE_HIT_MIRROR) {
            rank1Damage += ev.value();
        }
    }
    for (const auto& ev : rank10.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == kSelf && ev.target_id() == kEnemy &&
            ev.skill_table_id() == 10006 && ev.hit_kind() == BATTLE_HIT_MIRROR) {
            rank10Damage += ev.value();
        }
    }
    EXPECT_EQ(copyCastEvents, 1);
    EXPECT_EQ(mirroredCastEvents, 1);
    EXPECT_EQ(enemyMirrorCounterEvents, 0);
    EXPECT_GT(rank1Damage, 0u);
    EXPECT_GT(rank10Damage, rank1Damage);
}

TEST_F(ModernSkillCatalogTest, CopyTargetSkillFallsBackToLearnedPoolAndResistsWhenOnlyUncopyable) {
    // 目标防御未放技能、身上有 A41（禁复制）与 A06（可复制）-> 方案 A 兜底抽到 A06。
    {
        auto request = Request();
        Learn(*request.mutable_players(0), Row(10041));
        Learn(*request.mutable_players(2), Row(10041));
        Learn(*request.mutable_players(2), Row(10006));
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        DefendAllExcept(engine);
        engine.SubmitAction(kSelf, Cast(10041, kEnemy));
        engine.SubmitAction(kEnemy, Cast(10041, kSelf)); // 双方互放 A41 也绝不套娃
        const auto result = engine.ResolveCurrentRound();
        bool selfMirroredA06 = false, enemyResisted = false;
        for (const auto& ev : result.events()) {
            if (ev.event_type() == BATTLE_EVENT_SKILL && ev.source_id() == kSelf &&
                ev.skill_table_id() == 10006 && ev.hit_kind() == BATTLE_HIT_MIRROR) selfMirroredA06 = true;
            if (ev.event_type() == BATTLE_EVENT_RESIST && ev.source_id() == kEnemy &&
                ev.target_id() == kSelf && ev.buff_table_id() == 100410) enemyResisted = true;
        }
        EXPECT_TRUE(selfMirroredA06);
        EXPECT_TRUE(enemyResisted); // kSelf 只有 A41（禁复制），kEnemy 复制 kSelf 时发 RESIST 兜底。
    }
    // 目标为只持有禁复制牺牲技 B16 的宝宝 -> 不免费复制牺牲技，产出 RESIST。
    {
        auto request = Request(1, true);
        Learn(*request.mutable_players(0), Row(10041));
        Learn(*request.mutable_players(1)->mutable_pets(0), Row(11016));
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        const auto enemyPet = PetActor(engine.BuildStateSnapshot(), kEnemy);
        DefendAllExcept(engine);
        engine.SubmitAction(kSelf, Cast(10041, enemyPet));
        const auto result = engine.ResolveCurrentRound();
        bool resisted = false, mirroredSacrifice = false;
        for (const auto& ev : result.events()) {
            if (ev.event_type() == BATTLE_EVENT_RESIST && ev.buff_table_id() == 100410) resisted = true;
            if (ev.event_type() == BATTLE_EVENT_SKILL && ev.skill_table_id() == 11016 && ev.hit_kind() == BATTLE_HIT_MIRROR)
                mirroredSacrifice = true;
        }
        EXPECT_TRUE(resisted);
        EXPECT_FALSE(mirroredSacrifice);
    }
}

TEST_F(ModernSkillCatalogTest, CopyTargetSkillRemapsEnemySupportSkillToCasterTeamWithScale) {
    auto request = Request();
    request.mutable_players(0)->mutable_base_attributes()->set_health(2000);
    request.mutable_players(1)->mutable_base_attributes()->set_health(2000);
    request.mutable_players(2)->mutable_base_attributes()->set_health(2000);
    Learn(*request.mutable_players(0), Row(10041));
    Learn(*request.mutable_players(2), Row(10010)); // 敌方队列友方群体治疗 A10
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    DefendAllExcept(engine);
    engine.SubmitAction(kSelf, Cast(10041, kEnemy));
    engine.SubmitAction(kEnemy, Cast(10010, kEnemy));
    const auto result = engine.ResolveCurrentRound();
    // A10 Lv30 rank1 基础 600，经 85% 折算为 510，作用己方全员而不给敌方回血。
    EXPECT_EQ(Actor(result.state(), kSelf).attributes().health(), 2510u);
    EXPECT_EQ(Actor(result.state(), kAlly).attributes().health(), 2510u);
    for (const auto& ev : result.events()) {
        if (ev.event_type() == BATTLE_EVENT_HEAL && ev.source_id() == kSelf) {
            EXPECT_NE(ev.target_id(), kEnemy);
        }
    }
}

TEST_F(ModernSkillCatalogTest, BossUnitsAndSkillCopyImmuneTargetsAndPassiveMirrorBossSkillsAreBlocked) {
    // 1. 正式表 Monster 16 标记为 Boss，Monster 11 为普通怪。
    ASSERT_NE(data->FindMonster(16), nullptr);
    EXPECT_EQ(data->FindMonster(16)->is_boss(), 1u);
    ASSERT_NE(data->FindMonster(11), nullptr);
    EXPECT_EQ(data->FindMonster(11)->is_boss(), 0u);

    data->SetDungeonMonsters(3, {11, 16});
    CreateBattleRequest pve;
    pve.set_battle_id(70099);
    pve.set_match_mode(turnbattle::kMatchModePveSolo);
    pve.set_battle_config_id(3);
    pve.set_seed(20261123);
    auto* player = pve.add_players();
    player->set_player_id(kSelf);
    player->set_team_index(0);
    player->set_level(30);
    player->set_class_id(1);
    player->set_max_health(10000);
    player->set_max_mana(10000);
    player->set_magic_attack(600);
    player->mutable_base_attributes()->set_health(10000);
    player->mutable_base_attributes()->set_mana(10000);
    player->mutable_base_attributes()->set_speed(300);
    Learn(*player, Row(10041));
    Learn(*player, Row(12019)); // P19 法术反击
    data->AddBuff(120190).set_trigger_chance(1.0);

    // 剧本给 Boss 绑定零耗蓝法攻技（老模型），验证 Boss 实际命中玩家后，100% 触发的 P19 也绝不复制 Boss 技能。
    auto* bossScript = pve.add_team_scripts();
    bossScript->set_team_index(1);
    bossScript->mutable_script()->set_schema_version(1);
    bossScript->mutable_script()->set_script_id(99);
    auto* bossRole = bossScript->mutable_script()->add_roles();
    bossRole->set_role_id(1);
    bossRole->set_order(10);
    bossRole->set_actor_type(BATTLE_ACTOR_TYPE_MONSTER);
    bossRole->set_monster_id(16);
    auto* rule = bossRole->add_rules();
    rule->set_priority(10);
    rule->set_action(BATTLE_ACTION_SKILL);
    rule->set_skill_id(99001);
    rule->mutable_target()->set_side(BATTLE_SCRIPT_TARGET_ENEMY);

    auto& bossSkill = data->AddSkill(99001);
    bossSkill.set_battle_model(0);
    bossSkill.set_damage_type(0);
    bossSkill.add_skill_type(1);
    bossSkill.add_targeting_mode(1);
    bossSkill.set_require_target(1);
    bossSkill.set_attack_multiplier(1.0);
    data->SetSkillDamage(99001, 200.0);

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(pve));
    const auto snapshot = engine.BuildStateSnapshot();
    uint64_t minionId = 0, bossId = 0;
    for (const auto& actor : snapshot.actors()) {
        if (actor.monster_table_id() == 11) { minionId = actor.actor_id(); EXPECT_FALSE(actor.is_boss()); }
        if (actor.monster_table_id() == 16) { bossId = actor.actor_id(); EXPECT_TRUE(actor.is_boss()); }
    }
    ASSERT_NE(minionId, 0u);
    ASSERT_NE(bossId, 0u);
    EXPECT_EQ(engine.ValidateAction(kSelf, Cast(10041, bossId)), kSkillInvalidTarget);
    EXPECT_EQ(engine.ValidateAction(kSelf, Cast(10041, minionId)), kSuccess);

    // Boss 释放 99001 实际打伤玩家，验证玩家 100% 触发的 P19 不产生镜像反击。
    engine.SubmitAction(kSelf, Action(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();
    bool bossCastLanded = false;
    for (const auto& ev : result.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == bossId && ev.target_id() == kSelf &&
            ev.skill_table_id() == 99001 && ev.value() > 0) bossCastLanded = true;
        EXPECT_FALSE(ev.source_id() == kSelf && ev.hit_kind() == BATTLE_HIT_MIRROR);
    }
    EXPECT_TRUE(bossCastLanded);

    // 2. 场上仅有 Boss 时，target_id = 0 也拒绝施放 A41。
    data->SetDungeonMonsters(4, {16});
    CreateBattleRequest bossOnly = pve;
    bossOnly.set_battle_config_id(4);
    bossOnly.clear_team_scripts();
    TurnBattleEngine bossOnlyEngine(data);
    ASSERT_TRUE(bossOnlyEngine.Initialize(bossOnly));
    EXPECT_EQ(bossOnlyEngine.ValidateAction(kSelf, Cast(10041, 0)), kSkillInvalidTarget);

    // 3. kasan BuffImmuneTag("SkillCopy") 免疫拦截（通过开局常驻 Buff 挂载 immune_tag）。
    auto& immuneBuff = data->AddBuff(99100);
    immuneBuff.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Modifier));
    immuneBuff.set_modifier_stat(static_cast<uint32_t>(turnbattle::BattleModifier::Defense));
    immuneBuff.set_infinite_duration(1);
    (*immuneBuff.mutable_immune_tag())["SkillCopy"] = true;

    auto pvp = Request();
    Learn(*pvp.mutable_players(0), Row(10041));
    Learn(*pvp.mutable_players(2), Row(10006));
    auto* preBuff = pvp.mutable_players(2)->add_buffs();
    preBuff->set_buff_id(500);
    preBuff->set_buff_table_id(99100);
    TurnBattleEngine immuneEngine(data);
    ASSERT_TRUE(immuneEngine.Initialize(pvp));
    ASSERT_FALSE(Actor(immuneEngine.BuildStateSnapshot(), kEnemy).buffs().empty());
    DefendAllExcept(immuneEngine);
    immuneEngine.SubmitAction(kSelf, Cast(10041, kEnemy));
    immuneEngine.SubmitAction(kEnemy, Cast(10006, kSelf));
    const auto immuneResult = immuneEngine.ResolveCurrentRound();
    bool sawResist = false, sawMirror = false;
    for (const auto& ev : immuneResult.events()) {
        if (ev.event_type() == BATTLE_EVENT_RESIST && ev.source_id() == kSelf &&
            ev.target_id() == kEnemy && ev.buff_table_id() == 100410) sawResist = true;
        if (ev.event_type() == BATTLE_EVENT_SKILL && ev.source_id() == kSelf && ev.hit_kind() == BATTLE_HIT_MIRROR)
            sawMirror = true;
    }
    EXPECT_TRUE(sawResist);
    EXPECT_FALSE(sawMirror);
}

TEST_F(ModernSkillCatalogTest, StrategySkillsSelectDeterministicSecondaryTargetsAndConsumeOneShotMarks) {
    // 1. S05 逐命斩(10105): 主目标 201，其余 2 个名额按当前气血数值最低选中 203(1000) 与 205(1500)。
    auto req = Request(5, false);
    Learn(*req.mutable_players(0), Row(10105));
    req.mutable_players(5)->mutable_base_attributes()->set_health(9000); // 201 主目标
    req.mutable_players(6)->mutable_base_attributes()->set_health(8000); // 202
    req.mutable_players(7)->mutable_base_attributes()->set_health(1000); // 203 最低
    req.mutable_players(8)->mutable_base_attributes()->set_health(7000); // 204
    req.mutable_players(9)->mutable_base_attributes()->set_health(1500); // 205 次低

    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    DefendAllExcept(engine, kSelf);
    engine.SubmitAction(kSelf, Cast(10105, 201));
    const auto r1 = engine.ResolveCurrentRound();
    std::set<uint64_t> victims;
    for (const auto& ev : r1.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == kSelf && ev.skill_table_id() == 10105) {
            victims.insert(ev.target_id());
        }
    }
    EXPECT_EQ(victims, (std::set<uint64_t>{201, 203, 205}));

    // 2. S09 授锋战印(10109): 友方物攻最高补选 + 强化下一次物理群攻并一次性消耗。
    auto markReq = Request(2, false);
    Learn(*markReq.mutable_players(0), Row(10109));
    Learn(*markReq.mutable_players(0), Row(10002));
    markReq.mutable_players(0)->mutable_base_attributes()->set_speed(300);
    markReq.mutable_players(1)->set_physical_attack(999);
    TurnBattleEngine markEngine(data);
    ASSERT_TRUE(markEngine.Initialize(markReq));
    DefendAllExcept(markEngine, kSelf);
    markEngine.SubmitAction(kSelf, Cast(10109, kSelf));
    markEngine.ResolveCurrentRound();
    auto stateAfterMark = markEngine.BuildStateSnapshot();
    const auto hasMark = [](const BattleActorState& a, uint32_t buffId) {
        return std::any_of(a.buffs().begin(), a.buffs().end(),
                           [buffId](const BattleBuffEntry& b) { return b.buff_table_id() == buffId; });
    };
    EXPECT_TRUE(hasMark(Actor(stateAfterMark, kSelf), 101090));
    EXPECT_TRUE(hasMark(Actor(stateAfterMark, kAlly), 101090));

    DefendAllExcept(markEngine, kSelf);
    markEngine.SubmitAction(kSelf, Cast(10002, kEnemy));
    markEngine.ResolveCurrentRound();
    EXPECT_FALSE(hasMark(Actor(markEngine.BuildStateSnapshot(), kSelf), 101090));
}

TEST_F(ModernSkillCatalogTest, PrepareCastChargesAndUnleashesNextRoundOrGetsInterruptedByFreeze) {
    // 1. E01 蓄锋破阵(10115): 第1回合蓄力零伤且公开 charging_skill_id=10115，第2回合自动爆发清蓄力。
    auto req = Request(2, false);
    Learn(*req.mutable_players(0), Row(10115));
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    DefendAllExcept(engine, kSelf);
    engine.SubmitAction(kSelf, Cast(10115, kEnemy));
    const auto r1 = engine.ResolveCurrentRound();
    for (const auto& ev : r1.events()) {
        EXPECT_FALSE(ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == kSelf);
    }
    EXPECT_EQ(Actor(engine.BuildStateSnapshot(), kSelf).charging_skill_id(), 10115u);

    DefendAllExcept(engine, kSelf);
    engine.SubmitAction(kSelf, Action(BATTLE_ACTION_DEFEND));
    const auto r2 = engine.ResolveCurrentRound();
    bool unleashedDamage = false;
    for (const auto& ev : r2.events()) {
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == kSelf &&
            ev.skill_table_id() == 10115 && ev.value() > 0) {
            unleashedDamage = true;
        }
    }
    EXPECT_TRUE(unleashedDamage);
    EXPECT_EQ(Actor(engine.BuildStateSnapshot(), kSelf).charging_skill_id(), 0u);

    // 2. 第2回合出手前被敌方高速冰冻(A38 10038)打断，不触发蓄力伤害且清除蓄力状态。
    auto intReq = Request(2, false);
    Learn(*intReq.mutable_players(0), Row(10115));
    Learn(*intReq.mutable_players(2), Row(10038));
    intReq.mutable_players(0)->mutable_base_attributes()->set_speed(100);
    intReq.mutable_players(2)->mutable_base_attributes()->set_speed(500);
    auto freezeData = std::make_shared<MemoryBattleDataProvider>(*data);
    freezeData->AddBuff(100380).set_application_chance(1.0);
    TurnBattleEngine intEngine(freezeData);
    ASSERT_TRUE(intEngine.Initialize(intReq));
    DefendAllExcept(intEngine, kSelf);
    intEngine.SubmitAction(kSelf, Cast(10115, kEnemy));
    intEngine.ResolveCurrentRound();
    ASSERT_EQ(Actor(intEngine.BuildStateSnapshot(), kSelf).charging_skill_id(), 10115u);

    DefendAllExcept(intEngine, kEnemy);
    intEngine.SubmitAction(kEnemy, Cast(10038, kSelf));
    const auto r2Int = intEngine.ResolveCurrentRound();
    for (const auto& ev : r2Int.events()) {
        EXPECT_FALSE(ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == kSelf && ev.skill_table_id() == 10115);
    }
    EXPECT_EQ(Actor(intEngine.BuildStateSnapshot(), kSelf).charging_skill_id(), 0u);
}

TEST_F(ModernSkillCatalogTest, SpecializationPassivesForbidOppositeAttacksWhileBoostingOwnSide) {
    auto req = Request(2, false);
    Learn(*req.mutable_players(0), Row(12101)); // E03 灵法专修
    Learn(*req.mutable_players(0), Row(10002)); // 物理主动
    Learn(*req.mutable_players(0), Row(10006)); // 法术主动
    Learn(*req.mutable_players(0), Row(10010)); // 治疗
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(req));
    EXPECT_EQ(engine.ValidateAction(kSelf, Action(BATTLE_ACTION_ATTACK, kEnemy)), kSkillCannotBeCastInCurrentState);
    EXPECT_EQ(engine.ValidateAction(kSelf, Cast(10002, kEnemy)), kSkillCannotBeCastInCurrentState);
    EXPECT_EQ(engine.ValidateAction(kSelf, Cast(10006, kEnemy)), kSuccess);
    EXPECT_EQ(engine.ValidateAction(kSelf, Cast(10010, kSelf)), kSuccess);
}

TEST_F(ModernSkillCatalogTest, PursuitSealVulnerabilityStackingAndControlImmunityWorkEndToEnd) {
    auto customData = std::make_shared<MemoryBattleDataProvider>(*data);
    customData->AddBuff(131010).set_trigger_chance(1.0);
    customData->AddBuff(131011).set_application_chance(1.0);
    customData->AddBuff(100380).set_application_chance(1.0);

    auto req = Request(2, false);
    Learn(*req.mutable_players(0), Row(13101)); // E05 截灵追击
    Learn(*req.mutable_players(0), Row(14101)); // E07 连击破绽
    Learn(*req.mutable_players(0), Row(14102)); // E08 净心护阵
    Learn(*req.mutable_players(2), Row(10038)); // 敌方冰封玄狱
    req.mutable_players(0)->mutable_base_attributes()->set_speed(200);
    req.mutable_players(2)->mutable_base_attributes()->set_speed(300);

    TurnBattleEngine engine(customData);
    ASSERT_TRUE(engine.Initialize(req));

    // 开场状态校验：敌方 201 挂连击破绽(141010)，我方 101 挂净心护阵(141020)。
    const auto start = engine.BuildStateSnapshot();
    const auto findBuff = [](const BattleActorState& actor, uint32_t tableId) -> const BattleBuffEntry* {
        for (const auto& b : actor.buffs()) if (b.buff_table_id() == tableId) return &b;
        return nullptr;
    };
    ASSERT_NE(findBuff(Actor(start, kEnemy), 141010), nullptr);
    ASSERT_NE(findBuff(Actor(start, kSelf), 141020), nullptr);

    DefendAllExcept(engine, kSelf);
    engine.SubmitAction(kSelf, Action(BATTLE_ACTION_ATTACK, kEnemy));
    engine.SubmitAction(kEnemy, Cast(10038, kSelf));
    const auto r1 = engine.ResolveCurrentRound();

    // 敌方冰冻应被净心护阵挡下并发出 RESIST，且 101 正常出手普攻叠 1 层连击破绽 + 触发 E05 封法(131011)。
    bool sawImmuneResist = false;
    for (const auto& ev : r1.events()) {
        if (ev.event_type() == BATTLE_EVENT_RESIST && ev.target_id() == kSelf && ev.buff_table_id() == 100380) {
            sawImmuneResist = true;
        }
    }
    EXPECT_TRUE(sawImmuneResist);
    const auto afterR1 = engine.BuildStateSnapshot();
    EXPECT_EQ(findBuff(Actor(afterR1, kSelf), 141020), nullptr);
    const auto* vuln = findBuff(Actor(afterR1, kEnemy), 141010);
    ASSERT_NE(vuln, nullptr);
    EXPECT_EQ(vuln->stacks(), 1u);
    EXPECT_NE(findBuff(Actor(afterR1, kEnemy), 131011), nullptr);
}

} // namespace
