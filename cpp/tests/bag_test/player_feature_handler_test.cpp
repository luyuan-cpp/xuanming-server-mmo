#include <gtest/gtest.h>
#include <google/protobuf/util/message_differencer.h>
#include "engine/core/type_define/player_guid_comp.h"
#include <functional>
#include <limits>
#include <map>
#include <memory>
#include <optional>
#include <vector>

#include "../../nodes/scene/handler/rpc/player/player_activity_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_attribute_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_bag_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_mission_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_pet_handler.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/scene/player_pet.pb.h"
#include "services/scene/actor/action_state/constants/actor_state.h"
#include "services/scene/actor/action_state/system/actor_action_state.h"
#include "services/scene/actor/attribute/comp/actor_attribute_comp.h"
#include "services/scene/actor/attribute/system/actor_attribute_calculator.h"
#include "services/scene/player/system/player_pet.h"
#include "table/code/actoractionstate_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/pet_table.h"
#include "table/code/petrule_table.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/mission/comp/mission_comp.h"
#include "modules/mission/system/mission.h"
#include "modules/condition/condition_type.h"
#include "modules/mission/mission_events.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/system/player_mission.h"
#include "table/code/condition_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/item_table.h"
#include "table/code/monster_table.h"
#include "table/code/reward_table.h"
#include "table/proto/tip/mission_error_tip.pb.h"
#include "table/proto/tip/reward_error_tip.pb.h"
#include "rpc/service_metadata/player_mission_service_metadata.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
// 比较两份 proto:map 字段的序列化顺序不保证,两个不同实例的 SerializeAsString 不能直接比
using google::protobuf::util::MessageDifferencer;

class TestBagService : public SceneBagClientPlayer {};
class TestMissionService : public SceneMissionClientPlayer {};
class TestActivityService : public SceneActivityClientPlayer {};
class TestAttributeService : public SceneAttributeClientPlayer {};
class TestPetService : public ScenePetClientPlayer {};

TEST(PlayerFeatureHandlerTest, AttributePanelMissingPersistentDataIsWrittenToResponse) {
    const auto player = tlsEcs.actorRegistry.create();
    SceneAttributeClientPlayerHandler handler(std::make_unique<TestAttributeService>());
    GetAttributePanelRequest request;
    GetAttributePanelResponse response;
    response.mutable_panel()->set_level(30);

    handler.CallMethod(SceneAttributeClientPlayer::descriptor()->method(0), player, &request, &response);

    EXPECT_EQ(kServiceUnavailable, response.error_message().id());
    EXPECT_FALSE(response.has_panel());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerAttributeComp>(player));
    tlsEcs.actorRegistry.destroy(player);
}

TEST(PlayerFeatureHandlerTest, PetListMissingPersistentDataIsWrittenToResponse) {
    const auto player = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<LevelComp>(player);
    ScenePetClientPlayerHandler handler(std::make_unique<TestPetService>());
    GetPetListRequest request;
    GetPetListResponse response;
    response.mutable_pets()->set_active_pet_id(901);

    handler.CallMethod(ScenePetClientPlayer::descriptor()->method(0), player, &request, &response);

    EXPECT_EQ(kServiceUnavailable, response.error_message().id());
    EXPECT_FALSE(response.has_pets());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerPetComp>(player));
    tlsEcs.actorRegistry.destroy(player);
}

TEST(PlayerFeatureHandlerTest, BagRejectionIsWrittenToResponseAndDoesNotLeak) {
    const auto player = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
    SceneBagClientPlayerHandler handler(std::make_unique<TestBagService>());
    GetBagRequest request;
    request.set_bag_type(4);
    GetBagResponse response;
    handler.CallMethod(SceneBagClientPlayer::descriptor()->method(0), player, &request, &response);
    EXPECT_EQ(kInvalidParameter, response.error_message().id());
    EXPECT_FALSE(response.has_bag());
    request.set_bag_type(0);
    response.Clear();
    handler.CallMethod(SceneBagClientPlayer::descriptor()->method(0), player, &request, &response);
    EXPECT_EQ(0u, response.error_message().id());
    EXPECT_TRUE(response.has_bag());
    tlsEcs.actorRegistry.destroy(player);
}

TEST(PlayerFeatureHandlerTest, MissionInvalidEntityIsWrittenToResponse) {
    SceneMissionClientPlayerHandler handler(std::make_unique<TestMissionService>());
    GetMissionListRequest request;
    GetMissionListResponse response;
    handler.CallMethod(SceneMissionClientPlayer::descriptor()->method(0), entt::null, &request, &response);
    EXPECT_EQ(kThisEntityIsInvalid, response.error_message().id());
    EXPECT_TRUE(response.missions().empty());
}

TEST(PlayerFeatureHandlerTest, ActivityInvalidEntityIsWrittenToResponse) {
    SceneActivityClientPlayerHandler handler(std::make_unique<TestActivityService>());
    GetActivityListRequest request;
    GetActivityListResponse response;
    handler.CallMethod(SceneActivityClientPlayer::descriptor()->method(0), entt::null, &request, &response);
    EXPECT_EQ(kThisEntityIsInvalid, response.error_message().id());
    EXPECT_TRUE(response.activities().empty());
}
class MissionActionHandlerTest : public testing::Test {
protected:
    static bool ArmItemSegment(uint64_t lo, uint64_t hi) {
        auto& segment = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
        segment.Reset();
        GuidSegmentClient::Options options;
        options.kindName = "item";
        options.initialStep = 100000;
        if (!segment.Enable(options, [](const std::string&, uint32_t) { return true; },
            [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; })) return false;
        segment.Warm();
        segment.OnResponse(0, lo, hi);
        return segment.IsReady();
    }
    void SetUp() override {
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
        RewardTableManager::Instance().Load();
        ItemTableManager::Instance().Load();
        DungeonTableManager::Instance().Load();
        MonsterTableManager::Instance().Load();
        ASSERT_TRUE(ArmItemSegment(700000, 800000));
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{454545});
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(Guid{454545});
        ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
    }
    void TearDown() override {
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
    }
    MissionsComp& Missions() { return tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0); }
    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    GetMissionListResponse Act(bool claim, uint32_t id = 12, uint32_t scope = 0) {
        MissionActionRequest request;
        request.set_scope(scope);
        request.set_mission_id(id);
        GetMissionListResponse response;
        handler.CallMethod(claim ? SceneMissionClientPlayerClaimMissionRewardMethod :
                                   SceneMissionClientPlayerAcceptMissionMethod, player, &request, &response);
        return response;
    }
    GetMissionListResponse Read() {
        GetMissionListRequest request;
        GetMissionListResponse response;
        handler.CallMethod(SceneMissionClientPlayerGetMissionListMethod, player, &request, &response);
        return response;
    }
    static const PlayerMissionInfo* Find(const GetMissionListResponse& response, uint32_t id = 12) {
        for (const auto& info : response.missions())
            if (info.scope() == 0 && info.mission_id() == id) return &info;
        return nullptr;
    }
    void CompleteManualMission() {
        ASSERT_EQ(0u, Act(false).error_message().id());
        ConditionEvent kill;
        kill.player = player;
        kill.conditionType = static_cast<uint32_t>(eConditionType::kConditionKillMonster);
        kill.conditionIds.push_back(1);
        kill.amount = 1;
        PlayerMissionSystem::OnCondition(player, kill);
        ASSERT_TRUE(Missions().IsClaimable(12));
    }
    uint32_t ReadBagCount() {
        SceneBagClientPlayerHandler bagHandler(std::make_unique<TestBagService>());
        GetBagRequest request;
        request.set_bag_type(kInventory);
        GetBagResponse response;
        bagHandler.CallMethod(SceneBagClientPlayer::descriptor()->method(0), player, &request, &response);
        EXPECT_EQ(0u, response.error_message().id());
        uint32_t count = 0;
        for (const auto& item : response.bag().items()) count += item.count();
        return count;
    }
    entt::entity player{entt::null};
    SceneMissionClientPlayerHandler handler{std::make_unique<TestMissionService>()};
};

TEST_F(MissionActionHandlerTest, ActionRpcIdsAndInvalidEntityErrorsUseGeneratedDispatch) {
    EXPECT_EQ(194u, SceneMissionClientPlayerAcceptMissionMessageId);
    EXPECT_EQ(195u, SceneMissionClientPlayerClaimMissionRewardMessageId);
    MissionActionRequest request;
    request.set_mission_id(12);
    for (const auto* method : {SceneMissionClientPlayerAcceptMissionMethod, SceneMissionClientPlayerClaimMissionRewardMethod}) {
        GetMissionListResponse response;
        handler.CallMethod(method, entt::null, &request, &response);
        EXPECT_EQ(kThisEntityIsInvalid, response.error_message().id());
        EXPECT_TRUE(response.missions().empty());
        EXPECT_FALSE(response.state_persistent());
    }
}

TEST_F(MissionActionHandlerTest, AcceptReturnsActiveSnapshotAndDuplicateFailureDoesNotLeakIntoRead) {
    const auto accepted = Act(false);
    ASSERT_EQ(0u, accepted.error_message().id());
    EXPECT_TRUE(accepted.state_persistent());
    const auto* row = Find(accepted);
    ASSERT_NE(nullptr, row);
    EXPECT_EQ(PLAYER_MISSION_ACTIVE, row->status());
    EXPECT_FALSE(row->can_accept());
    EXPECT_FALSE(row->can_claim());
    const auto duplicate = Act(false);
    EXPECT_EQ(kMissionIdRepeated, duplicate.error_message().id());
    EXPECT_TRUE(duplicate.missions().empty());
    const auto snapshot = Read();
    EXPECT_EQ(0u, snapshot.error_message().id());
    ASSERT_NE(nullptr, Find(snapshot));
    EXPECT_EQ(PLAYER_MISSION_ACTIVE, Find(snapshot)->status());
    EXPECT_EQ(0u, ReadBagCount());
}

TEST_F(MissionActionHandlerTest, PrematureOrAlternateScopeClaimCannotGrantReward) {
    EXPECT_EQ(kMissionIdNotInRewardList, Act(true).error_message().id());
    EXPECT_EQ(kInvalidParameter, Act(false, 12, 1).error_message().id());
    CompleteManualMission();
    EXPECT_EQ(kInvalidParameter, Act(true, 12, 1).error_message().id());
    EXPECT_TRUE(Missions().IsClaimable(12));
    EXPECT_EQ(0u, ReadBagCount());
}

TEST_F(MissionActionHandlerTest, ClaimReturnsCompletedSnapshotAndBagReadShowsOneReward) {
    CompleteManualMission();
    const auto before = Read();
    ASSERT_NE(nullptr, Find(before));
    EXPECT_EQ(PLAYER_MISSION_CLAIMABLE, Find(before)->status());
    EXPECT_TRUE(Find(before)->can_claim());
    const auto claimed = Act(true);
    ASSERT_EQ(0u, claimed.error_message().id());
    ASSERT_NE(nullptr, Find(claimed));
    EXPECT_EQ(PLAYER_MISSION_COMPLETED, Find(claimed)->status());
    EXPECT_FALSE(Find(claimed)->can_claim());
    EXPECT_TRUE(claimed.state_persistent());
    EXPECT_EQ(4u, ReadBagCount());
    const auto duplicate = Act(true);
    EXPECT_EQ(kRewardAlreadyClaimed, duplicate.error_message().id());
    EXPECT_TRUE(duplicate.missions().empty());
    EXPECT_EQ(4u, ReadBagCount());
}

TEST_F(MissionActionHandlerTest, FullBagErrorRetainsClaimAndRetryReturnsFreshSnapshot) {
    CompleteManualMission();
    Inventory().SetCapacityForRestore(3);
    const auto failed = Act(true);
    EXPECT_NE(0u, failed.error_message().id());
    EXPECT_TRUE(failed.missions().empty());
    EXPECT_TRUE(Missions().IsClaimable(12));
    EXPECT_EQ(0u, ReadBagCount());
    const auto beforeRetry = Read();
    ASSERT_NE(nullptr, Find(beforeRetry));
    EXPECT_TRUE(Find(beforeRetry)->can_claim());
    Inventory().SetCapacityForRestore(4);
    const auto retried = Act(true);
    ASSERT_EQ(0u, retried.error_message().id());
    ASSERT_NE(nullptr, Find(retried));
    EXPECT_EQ(PLAYER_MISSION_COMPLETED, Find(retried)->status());
    EXPECT_EQ(4u, ReadBagCount());
}

TEST_F(MissionActionHandlerTest, FrozenActionsPreserveStateAndReadOnlySnapshotDisablesClaim) {
    CompleteManualMission();
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    EXPECT_EQ(kInvalidParameter, Act(true).error_message().id());
    EXPECT_EQ(kInvalidParameter, Act(false, 13).error_message().id());
    const auto frozen = Read();
    ASSERT_NE(nullptr, Find(frozen));
    EXPECT_EQ(PLAYER_MISSION_CLAIMABLE, Find(frozen)->status());
    EXPECT_FALSE(Find(frozen)->can_claim());
    EXPECT_TRUE(Missions().IsClaimable(12));
    EXPECT_FALSE(Missions().IsAccepted(13));
    EXPECT_EQ(0u, ReadBagCount());
}

// ═════════════════════════════════════════════════════════════════════════
// 三类组件分别验证:派生 CombatStateFlagsComp 在 actor 创建时挂,
// 持久 PlayerPetComp 由加载链路挂;按需 ActorStateComp 在第一次状态写入时创建。
// 初始化链不编进本工程,派生与持久组件缺失场景由夹具显式构造,按需状态调用真实写入口。
// ═════════════════════════════════════════════════════════════════════════

// 用例自己建的实体统一登记、TearDown 销毁,不把组件留给后面的用例。
class ActorEntityFixture : public testing::Test {
protected:
    void TearDown() override {
        for (const auto entity : entities) {
            if (tlsEcs.actorRegistry.valid(entity)) tlsEcs.actorRegistry.destroy(entity);
        }
    }
    entt::entity NewEntity() {
        const auto entity = tlsEcs.actorRegistry.create();
        entities.push_back(entity);
        return entity;
    }
    template <typename Comp>
    bool Has(entt::entity entity) const { return tlsEcs.actorRegistry.all_of<Comp>(entity); }

    std::vector<entt::entity> entities;
};

// ── 属性脏标记(F035):按需状态,标脏时挂上、消费后摘掉 ─────────────────────

class ActorAttributeCalculatorTest : public ActorEntityFixture {};

// 修复前 Update 只清位不摘组件:标过一次脏的 actor 永远留在 view 里,每 tick 白遍历一遍。
TEST_F(ActorAttributeCalculatorTest, UpdateRemovesTheDirtyFlagComponentOnceConsumed) {
    const auto actor = NewEntity();
    tlsEcs.actorRegistry.emplace<CombatStateFlagsComp>(actor);
    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(actor, kCombatState);
    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(actor, kHealth);
    ASSERT_TRUE(Has<AttributeDirtyFlagsComp>(actor));
    {
        const auto &bits = tlsEcs.actorRegistry.get<AttributeDirtyFlagsComp>(actor).attributeBits;
        EXPECT_TRUE(bits.test(kCombatState)) << "marks within one tick must merge, not replace each other";
        EXPECT_TRUE(bits.test(kHealth));
    }

    ActorAttributeCalculatorSystem::Update();
    EXPECT_FALSE(Has<AttributeDirtyFlagsComp>(actor));
}

// 跨 zone 冻结中的玩家不重算,标记原样留到冻结解除(view 的 exclude 口径不变),解冻后消费即摘。
TEST_F(ActorAttributeCalculatorTest, FrozenPlayerKeepsItsDirtyFlagsUntilTheFreezeEnds) {
    const auto actor = NewEntity();
    auto& flags = tlsEcs.actorRegistry.emplace<CombatStateFlagsComp>(actor);
    constexpr uint32_t kExistingCombatState = 2;
    (*flags.mutable_state_flags())[kExistingCombatState] = true;
    const auto before = flags.SerializeAsString();
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(actor);
    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(actor, kCombatState);

    ActorAttributeCalculatorSystem::Update();
    ASSERT_TRUE(Has<AttributeDirtyFlagsComp>(actor));
    EXPECT_EQ(before, tlsEcs.actorRegistry.get<CombatStateFlagsComp>(actor).SerializeAsString());

    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(actor);
    ActorAttributeCalculatorSystem::Update();
    EXPECT_FALSE(Has<AttributeDirtyFlagsComp>(actor));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<CombatStateFlagsComp>(actor).state_flags().empty());
}

// 没走 actor 初始化的实体:重算函数只取不建,缺组件报错跳过;标记照样消费,不每 tick 重报。
// 修复前 ResetCombatStateFlags 用 get_or_emplace，会把缺失的同步投影补出来。
TEST_F(ActorAttributeCalculatorTest, RecalculationDoesNotCreateMissingRequiredComponents) {
    const auto actor = NewEntity();
    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(actor, kCombatState);

    ActorAttributeCalculatorSystem::Update();
    EXPECT_FALSE(Has<CombatStateFlagsComp>(actor));
    EXPECT_FALSE(Has<AttributeDirtyFlagsComp>(actor));
}

// 走过初始化的 actor:同步投影照旧等于当前激活的战斗状态(键存在即激活)。修复前后都应成立。
TEST_F(ActorAttributeCalculatorTest, CombatStateFlagsMirrorTheActiveCombatStates) {
    constexpr uint32_t kSomeCombatState = 2;
    const auto actor = NewEntity();
    tlsEcs.actorRegistry.emplace<CombatStateFlagsComp>(actor);
    auto &collection = tlsEcs.actorRegistry.emplace<CombatStateCollectionComp>(actor);
    (*(*collection.mutable_states())[kSomeCombatState].mutable_sources())[uint64_t{42}] = false;
    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(actor, kCombatState);

    ActorAttributeCalculatorSystem::Update();
    const auto &flags = tlsEcs.actorRegistry.get<CombatStateFlagsComp>(actor).state_flags();
    ASSERT_EQ(1u, flags.size());
    ASSERT_TRUE(flags.contains(kSomeCombatState));
    EXPECT_TRUE(flags.at(kSomeCombatState));
}

// 越界的属性位不能碰组件:修复前先 get_or_emplace 出标记组件,再由 bitset::set 抛 out_of_range。
TEST_F(ActorAttributeCalculatorTest, MarkIgnoresAnOutOfRangeAttributeBit) {
    const auto actor = NewEntity();
    EXPECT_NO_THROW(ActorAttributeCalculatorSystem::MarkAttributeForUpdate(actor, kAttributeCalculatorMax));
    EXPECT_FALSE(Has<AttributeDirtyFlagsComp>(actor));
}

// ── actor 行为状态(F034):空缺是无激活状态,AddState 是唯一延迟创建入口 ───────────

class ActorActionStateTest : public ActorEntityFixture {
protected:
    void SetUp() override {
        ActorActionStateTableManager::Instance().Load();
        // 前置:「释放技能」这一行必须在表里,否则查表先失败,下面的用例就验不到组件这一步。
        ASSERT_NE(nullptr, ActorActionStateTableManager::Instance().FindByIdSilent(kActorActionUseSkill).first)
            << "ActorActionState row " << kActorActionUseSkill << " is missing: run the data table exporter first";
    }
};

// 只读与幂等移除不创建空表;活实体缺表是正常空状态,不是初始化失败。
TEST_F(ActorActionStateTest, MissingStateIsEmptyAndReadOrRemovalCreatesNothing) {
    const auto actor = NewEntity();
    EXPECT_EQ(kSuccess,
              ActorActionStateSystem::CanExecuteActionWithoutStateChange(actor, kActorActionUseSkill));
    EXPECT_FALSE(ActorActionStateSystem::HasState(actor, kActorStateCombat));
    EXPECT_EQ(kSuccess, ActorActionStateSystem::RemoveState(actor, kActorStateCombat));
    EXPECT_FALSE(Has<ActorStateComp>(actor));
}

// 修复前把空表当成必需组件,首次合法状态写入会被拒绝。
TEST_F(ActorActionStateTest, FirstActionCreatesTheStateAndRepeatedAddsPreserveIt) {
    const auto actor = NewEntity();
    EXPECT_EQ(kSuccess, ActorActionStateSystem::TryPerformAction(actor, kActorActionUseSkill, kActorStateCombat));
    EXPECT_TRUE(ActorActionStateSystem::HasState(actor, kActorStateCombat));
    EXPECT_EQ(kSuccess, ActorActionStateSystem::AddState(actor, kActorStateCombat));
    EXPECT_EQ(1u, tlsEcs.actorRegistry.get<ActorStateComp>(actor).state_list().size());
    EXPECT_EQ(kSuccess, ActorActionStateSystem::CanExecuteActionWithoutStateChange(actor, kActorActionUseSkill));
}

TEST_F(ActorActionStateTest, InvalidFirstStateCreatesNothing) {
    const auto actor = NewEntity();
    EXPECT_EQ(kInvalidParameter, ActorActionStateSystem::AddState(actor, kActorStateActorStateMax));
    EXPECT_EQ(kInvalidParameter,
              ActorActionStateSystem::TryPerformAction(actor, kActorActionUseSkill, kActorStateActorStateMax));
    EXPECT_FALSE(Has<ActorStateComp>(actor));
}

TEST_F(ActorActionStateTest, DestroyedEntityIsRejectedInsteadOfBeingTreatedAsEmpty) {
    const auto actor = NewEntity();
    tlsEcs.actorRegistry.destroy(actor);

    EXPECT_EQ(kThisEntityIsInvalid, ActorActionStateSystem::AddState(actor, kActorStateCombat));
    EXPECT_EQ(kThisEntityIsInvalid,
              ActorActionStateSystem::TryPerformAction(actor, kActorActionUseSkill, kActorStateCombat));
    EXPECT_EQ(kThisEntityIsInvalid,
              ActorActionStateSystem::CanExecuteActionWithoutStateChange(actor, kActorActionUseSkill));
    EXPECT_EQ(kThisEntityIsInvalid, ActorActionStateSystem::RemoveState(actor, kActorStateCombat));
    EXPECT_FALSE(ActorActionStateSystem::HasState(actor, kActorStateCombat));
    EXPECT_FALSE(Has<ActorStateComp>(actor));
}

// 非法的目标状态在任何状态改动之前被拒(校验前置),已有状态原样保留。
// 覆盖范围如实说明:现行 ActorActionState 表里没有任何 kActionStateInterrupt 行,打断分支走不到,
// 所以本用例只钉住返回码与「状态不变」,守不住「successState 校验排在打断之前」这一处顺序 ——
// 修复前后它都是绿的。要守住顺序,需要表里有打断行,或给表管理器加注入测试数据的接缝。
TEST_F(ActorActionStateTest, InvalidSuccessStateIsRejectedBeforeAnyStateChange) {
    const auto actor = NewEntity();
    auto &states = tlsEcs.actorRegistry.emplace<ActorStateComp>(actor);
    (*states.mutable_state_list())[static_cast<uint32_t>(kActorStateMounted)] = true;
    EXPECT_EQ(kInvalidParameter,
              ActorActionStateSystem::TryPerformAction(actor, kActorActionUseSkill, kActorStateActorStateMax));
    EXPECT_TRUE(ActorActionStateSystem::HasState(actor, kActorStateMounted));
    EXPECT_EQ(1u, tlsEcs.actorRegistry.get<ActorStateComp>(actor).state_list().size());
}

// ── 宝宝组件(F074):PlayerPetComp 由加载链路挂上,PetSystem 所有入口只取不建 ─────────

class PetComponentOwnershipTest : public ActorEntityFixture {
protected:
    void SetUp() override {
        PetTableManager::Instance().Load();
        PetRuleTableManager::Instance().Load();
        AttributePoolTableManager::Instance().Load();
        AttributeDimensionTableManager::Instance().Load();
        const auto &rows = PetTableManager::Instance().FindAll();
        ASSERT_GT(rows.data_size(), 0) << "Pet table is empty: run the data table exporter first";
        petTableId = rows.data(0).id();
        // 前置:宝宝池存在,否则 Allocate / Reset / GrantPet 会先因表缺失返回,验不到组件这一步。
        ASSERT_FALSE(AttributePoolTableManager::Instance().GetByOwnerType(PetSystem::kPoolOwnerPet).empty());
        // 没走加载链路的玩家:身份在,宝宝组件不在。
        player = NewEntity();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{565656});
    }

    entt::entity player{entt::null};
    uint32_t petTableId{0};
};

// 只读 / 重算 / 战斗接缝:缺组件时不带宝宝、不推列表、不补建组件;列表接口报故障码,
// 让 handler 回错误而不是把空列表当成功发给客户端。
// 修复前 EnsureComp 会在第一个调用(BuildList)上就补出一个空的 PlayerPetComp(且 BuildList 没有返回值)。
TEST_F(PetComponentOwnershipTest, ReadAndRecalculateEntriesCreateNothing) {
    PetListInfo list;
    list.set_active_pet_id(99);
    EXPECT_EQ(kServiceUnavailable, PetSystem::BuildList(player, list));
    EXPECT_EQ(0u, list.active_pet_id());
    EXPECT_EQ(0, list.pets_size());

    ::BattlePetSnapshot snapshot;
    EXPECT_FALSE(PetSystem::BuildBattleSnapshot(player, snapshot));
    ::BattlePetSettlementData settlement;
    settlement.set_pet_id(1);
    PetSystem::ApplyBattleSettlement(player, settlement);
    PetSystem::InitializeOnLoad(player);
    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLevelChanged);
    PetSystem::PushList(player);

    EXPECT_FALSE(Has<PlayerPetComp>(player));
}

// 写入口与交易原语:一律在扣费 / 铸号 / 改动之前拒绝,返回「缺必需组件」的服务端故障码
// kServiceUnavailable(与 player_mission.cpp / skill.cpp 同口径;实体无效才是 kEntityIsNull)。
// 修复前它们都先补出空组件再往下走:Summon 答「没有这只宝宝」,GrantPet 真的发出一只,
// RestorePetFromSnapshot 把交易快照塞进一个凭空造出来的组件并答成功。
TEST_F(PetComponentOwnershipTest, WriteEntriesAndTradePrimitivesRejectWithoutTheComponent) {
    EXPECT_EQ(kServiceUnavailable, PetSystem::Summon(player, 1));
    EXPECT_EQ(kServiceUnavailable, PetSystem::Recall(player));
    EXPECT_EQ(kServiceUnavailable, PetSystem::Allocate(player, 1, std::map<uint32_t, uint32_t>{}));
    EXPECT_EQ(kServiceUnavailable, PetSystem::Reset(player, 1));
    std::map<uint32_t, uint32_t> suggested{{1u, 1u}};
    EXPECT_EQ(kServiceUnavailable, PetSystem::AutoAllocate(player, 1, suggested));
    EXPECT_TRUE(suggested.empty());
    EXPECT_EQ(kServiceUnavailable, PetSystem::Rename(player, 1, "小白"));
    uint64_t petId = 77;
    EXPECT_EQ(kServiceUnavailable, PetSystem::GrantPet(player, petTableId, petId));
    EXPECT_EQ(0u, petId);

    PetInstance taken;
    taken.set_pet_id(5);
    EXPECT_EQ(kServiceUnavailable, PetSystem::RemovePetForTrade(player, 1, taken));
    EXPECT_EQ(0u, taken.pet_id()) << "a refused take-out must leave the snapshot cleared";
    PetInstance snapshot;
    snapshot.set_pet_id(900001);
    snapshot.set_pet_table_id(petTableId);
    EXPECT_EQ(kServiceUnavailable, PetSystem::RestorePetFromSnapshot(player, snapshot));

    EXPECT_FALSE(Has<PlayerPetComp>(player));
}

// 列表接口的两种失败要分得开:实体无效 → kEntityIsNull;加载链路挂好组件后 → kSuccess(空宝宝也是成功)。
TEST_F(PetComponentOwnershipTest, BuildListReportsInvalidEntityAndSucceedsOnceLoaded) {
    PetListInfo list;
    EXPECT_EQ(kEntityIsNull, PetSystem::BuildList(entt::null, list));

    tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
    EXPECT_EQ(kSuccess, PetSystem::BuildList(player, list));
    EXPECT_EQ(0, list.pets_size());
}

// 已加载的宝宝仍是持久数据;缺少主人等级时不能拿默认 1 级覆盖它。
class PetOwnerLevelTest : public PetComponentOwnershipTest {
protected:
    void SetUp() override {
        PetComponentOwnershipTest::SetUp();
        if (HasFatalFailure()) return;
        const auto* row = PetTableManager::Instance().FindById(petTableId).first;
        ASSERT_NE(nullptr, row);
        ASSERT_GE(row->level_cap(), kSavedLevel);
        const auto& pools = AttributePoolTableManager::Instance().GetByOwnerType(PetSystem::kPoolOwnerPet);
        ASSERT_FALSE(pools.empty());
        pool = std::cref(*pools.front());
        const auto& dimensions = AttributeDimensionTableManager::Instance().GetByPoolId(pool->get().id());
        ASSERT_FALSE(dimensions.empty());
        dimensionId = dimensions.front()->id();
        ASSERT_LE(pool->get().unlock_level(), kSavedLevel);
        ASSERT_TRUE(pool->get().dimension_cap() == 0 || pool->get().dimension_cap() >= 101);
        ASSERT_GT(pool->get().base_points() + pool->get().points_per_level() * (kSavedLevel - 1), 101u);
        auto& comp = tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
        auto* pet = comp.add_pets();
        pet->set_pet_id(kPetId);
        pet->set_pet_table_id(petTableId);
        pet->set_level(kSavedLevel);
        pet->set_health(123);
        pet->set_mana(234);
        (*pet->mutable_allocated())[dimensionId] = 100;
        (*pet->mutable_aptitude())[dimensionId] = 10000;
    }
    PlayerPetComp& Comp() { return tlsEcs.actorRegistry.get<PlayerPetComp>(player); }
    static constexpr uint64_t kPetId = 900001;
    static constexpr uint32_t kSavedLevel = 85;
    std::optional<std::reference_wrapper<const AttributePoolTable>> pool;
    uint32_t dimensionId{0};
};

TEST_F(PetOwnerLevelTest, MissingOwnerLevelSkipsLoadRepairAndRecalculationWithoutChangingSavedPet) {
    // 失效出战引用:证明守卫在 InitializeOnLoad 唯一的修补副作用(纠正出战指针)之前。
    // 未知维度:本版本表里没有的键原样携带(加载本来就不再按表清理加点,加点统一规则 1),同一份字节前后不变。
    Comp().set_active_pet_id(kPetId + 1);
    (*Comp().mutable_pets(0)->mutable_allocated())[std::numeric_limits<uint32_t>::max()] = 7;
    const auto before = Comp().SerializeAsString();
    PetSystem::InitializeOnLoad(player);
    EXPECT_EQ(before, Comp().SerializeAsString());
    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLevelChanged);
    EXPECT_EQ(before, Comp().SerializeAsString());
    EXPECT_FALSE(Has<LevelComp>(player));
}

TEST_F(PetOwnerLevelTest, TradedGrowthSurvivesLowerOwnerLoadAndRoundTripButCannotSummonOrBattle) {
    auto& level = tlsEcs.actorRegistry.emplace<LevelComp>(player);
    level.set_level(10);
    PetSystem::InitializeOnLoad(player);
    EXPECT_EQ(kSavedLevel, Comp().pets(0).level());
    EXPECT_EQ(100u, Comp().pets(0).allocated().at(dimensionId));
    EXPECT_EQ(kPetOwnerLevelNotEnough, PetSystem::Summon(player, kPetId));
    Comp().set_active_pet_id(kPetId); // 存量出战指针也不能绕过战斗入口。
    BattlePetSnapshot battle;
    EXPECT_FALSE(PetSystem::BuildBattleSnapshot(player, battle));
    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLevelChanged);
    EXPECT_EQ(0u, Comp().active_pet_id());
    PlayerPetComp restored;
    ASSERT_TRUE(restored.ParseFromString(Comp().SerializeAsString()));
    EXPECT_EQ(kSavedLevel, restored.pets(0).level());
    EXPECT_EQ(100u, restored.pets(0).allocated().at(dimensionId));
    level.set_level(kSavedLevel);
    EXPECT_EQ(kSuccess, PetSystem::Summon(player, kPetId));
    level.set_level(10);
    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLevelChanged);
    EXPECT_EQ(0u, Comp().active_pet_id());
    EXPECT_EQ(kSavedLevel, Comp().pets(0).level());
    EXPECT_EQ(100u, Comp().pets(0).allocated().at(dimensionId));
}

TEST_F(PetOwnerLevelTest, MissingOwnerLevelRejectsSummonBeforeChangingActivePet) {
    const auto before = Comp().SerializeAsString();
    EXPECT_EQ(kServiceUnavailable, PetSystem::Summon(player, kPetId));
    EXPECT_EQ(before, Comp().SerializeAsString());
    EXPECT_FALSE(Has<LevelComp>(player));
}

TEST_F(PetOwnerLevelTest, MissingOwnerLevelRejectsAllocationBeforeChangingPointsOrLevel) {
    const auto before = Comp().SerializeAsString();
    EXPECT_EQ(kServiceUnavailable, PetSystem::Allocate(player, kPetId, {{dimensionId, 101}}));
    EXPECT_EQ(before, Comp().SerializeAsString());
    EXPECT_FALSE(Has<LevelComp>(player));
}

TEST_F(PetOwnerLevelTest, MissingOwnerLevelRejectsResetBeforeChargingOrClearingPoints) {
    ASSERT_GT(pool->get().reset_cost_gold(), 0u);
    ASSERT_LE(pool->get().reset_free_below_level(), kSavedLevel);
    auto& currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
    for (uint32_t index = 0; index < kCurrencyMax; ++index) currency.add_values(index == kCurrencyGold ? 10000 : 0);
    ASSERT_GT(currency.values(kCurrencyGold), pool->get().reset_cost_gold());
    auto& debt = tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
    const auto petBefore = Comp().SerializeAsString();
    const auto currencyBefore = currency.SerializeAsString();
    EXPECT_EQ(kServiceUnavailable, PetSystem::Reset(player, kPetId));
    EXPECT_EQ(petBefore, Comp().SerializeAsString());
    EXPECT_EQ(currencyBefore, currency.SerializeAsString());
    EXPECT_FALSE(debt.dirty);
    EXPECT_FALSE(Has<LevelComp>(player));
}

TEST_F(PetOwnerLevelTest, ListUsesSavedPetLevelWithoutRequiringOwnerLevel) {
    const auto before = Comp().SerializeAsString();
    PetListInfo list;
    ASSERT_EQ(kSuccess, PetSystem::BuildList(player, list));
    ASSERT_EQ(1, list.pets_size());
    EXPECT_EQ(kSavedLevel, list.pets(0).level());
    EXPECT_EQ(before, Comp().SerializeAsString());
    EXPECT_FALSE(Has<LevelComp>(player));
}

// ── 宝宝加点的生效判定(加点统一规则,attribute_allocation_rules.h;
//    mixed-version-persist-field-protection.md §12.1 D3 / D4;player-pet.md §3.3)──────────────
// 加载与重算不改写、不返还宝宝的已分配点:宝宝池之外的键是未知键(不生效、计入已用),超额的加点整池按 0 生效;
// 只有玩家主动的加点 / 洗点会改它。以前加载时删掉不属于宝宝池的键(SanitizeAllocations)、超额整只清空返还。
// 夹具:在 PetOwnerLevelTest 的基础上挂主人等级(= 宝宝已存等级,召唤与战斗快照放行)与足够的金币。
// 当前表的宝宝池 dimension_cap 为 0,单项上限分支只在纯规则单测里覆盖。
class PetAllocationTest : public PetOwnerLevelTest {
protected:
    void SetUp() override {
        PetOwnerLevelTest::SetUp();
        if (HasFatalFailure()) return;
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(kSavedLevel);
        auto& currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        for (uint32_t index = 0; index < kCurrencyMax; ++index) currency.add_values(index == kCurrencyGold ? kGold : 0);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        // 未知键的两种来源:新版本加的维度(表里没有这一行)与角色池维度(本版本归不到宝宝)。按表独立找,不经被测代码。
        ASSERT_EQ(nullptr, AttributeDimensionTableManager::Instance().FindByIdSilent(kFutureDimension).first);
        for (const auto& dim : AttributeDimensionTableManager::Instance().FindAll().data()) {
            const auto* owner = AttributePoolTableManager::Instance().FindByIdSilent(dim.pool_id()).first;
            if (owner != nullptr && owner->owner_type() != PetSystem::kPoolOwnerPet) {
                playerDimensionId = dim.id();
                break;
            }
        }
        ASSERT_NE(0u, playerDimensionId) << "the table has no dimension outside the pet pool";
        // 宝宝池里另一个已知维度(与夹具已加 100 点的 dimensionId 不同)
        for (const auto* dim : AttributeDimensionTableManager::Instance().GetByPoolId(Pool().id())) {
            if (dim->id() != dimensionId) {
                otherDimensionId = dim->id();
                break;
            }
        }
        ASSERT_NE(0u, otherDimensionId) << "the pet pool needs at least two dimensions";
        ASSERT_EQ(kKnownPoints, Pet().allocated().at(dimensionId)) << "the fixture pet carries 100 points on one known dimension";
        ASSERT_GT(Total(), kKnownPoints + kFuturePoints + kForeignPoints);
    }

    const AttributePoolTable& Pool() const { return pool->get(); }
    PetInstance& Pet() { return *Comp().mutable_pets(0); }
    // 宝宝池在夹具宝宝等级时的总点数,按表独立算一遍(不经被测代码)。
    uint32_t Total() const {
        return Pool().base_points() + Pool().points_per_level() * (kSavedLevel - Pool().unlock_level() + 1);
    }
    uint64_t Gold() const { return tlsEcs.actorRegistry.get<CurrencyComp>(player).values(kCurrencyGold); }

    // 与夹具宝宝同种类、同等级、同资质、同当前 HP/MP 的孪生宝宝(只换 pet_id、清空加点),allocated 由调用方再填。
    PetInstance& AddTwin(uint64_t petId) {
        PetInstance twin = Comp().pets(0);
        twin.set_pet_id(petId);
        twin.clear_allocated();
        auto* added = Comp().add_pets();
        *added = twin;
        return *added;
    }
    static const PetInfo* FindInfo(const PetListInfo& list, uint64_t petId) {
        for (const auto& info : list.pets()) {
            if (info.pet_id() == petId) return &info;
        }
        return nullptr;
    }
    static std::map<uint32_t, uint32_t> AllocationOf(const PetInstance& pet) {
        std::map<uint32_t, uint32_t> result;
        for (const auto& [key, value] : pet.allocated()) result[key] = value;
        return result;
    }

    static constexpr uint32_t kFutureDimension = 987654;  // 本版本表里没有:代表新版本加的维度
    static constexpr uint32_t kKnownPoints = 100;         // PetOwnerLevelTest 给 dimensionId 加的点
    static constexpr uint32_t kFuturePoints = 30;
    static constexpr uint32_t kForeignPoints = 20;
    static constexpr uint64_t kGold = 100000;
    static constexpr uint64_t kTwinPetId = kPetId + 1;
    uint32_t otherDimensionId{0};
    uint32_t playerDimensionId{0};
};

// 本版本表里没有的维度、角色池维度(宝宝身上归不到宝宝池):加载、升级重算、再加载与序列化往返都原样携带。
TEST_F(PetAllocationTest, UnknownDimensionSurvivesLoadAndRecalculation) {
    (*Pet().mutable_allocated())[kFutureDimension] = kFuturePoints;
    (*Pet().mutable_allocated())[playerDimensionId] = kForeignPoints;
    const auto expected = AllocationOf(Pet());
    ASSERT_EQ(3u, expected.size());

    PetSystem::InitializeOnLoad(player);
    EXPECT_EQ(expected, AllocationOf(Pet()));
    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLevelChanged);
    EXPECT_EQ(expected, AllocationOf(Pet()));
    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLoad);
    EXPECT_EQ(expected, AllocationOf(Pet()));

    PlayerPetComp reloaded;
    ASSERT_TRUE(reloaded.ParseFromString(Comp().SerializeAsString()));
    ASSERT_EQ(1, reloaded.pets_size());
    EXPECT_EQ(expected, AllocationOf(reloaded.pets(0)));
}

// 未知键计入已用、减少剩余点,但永不生效:二级属性、列表维度与战斗快照都等于不带这些键的孪生宝宝。
TEST_F(PetAllocationTest, UnknownDimensionReducesRemainingButNeverTakesEffect) {
    auto& twin = AddTwin(kTwinPetId);
    (*twin.mutable_allocated())[dimensionId] = kKnownPoints;
    (*Pet().mutable_allocated())[kFutureDimension] = kFuturePoints;
    (*Pet().mutable_allocated())[playerDimensionId] = kForeignPoints;
    PetSystem::InitializeOnLoad(player);

    PetListInfo list;
    ASSERT_EQ(kSuccess, PetSystem::BuildList(player, list));
    const auto* carrying = FindInfo(list, kPetId);
    const auto* plain = FindInfo(list, kTwinPetId);
    ASSERT_NE(nullptr, carrying);
    ASSERT_NE(nullptr, plain);
    EXPECT_EQ(Total(), carrying->total_points());
    EXPECT_EQ(Total() - kKnownPoints - kFuturePoints - kForeignPoints, carrying->remaining_points())
        << "points parked on unknown keys stay spent";
    EXPECT_EQ(Total() - kKnownPoints, plain->remaining_points());
    EXPECT_TRUE(MessageDifferencer::Equals(plain->derived(), carrying->derived()))
        << "unknown keys must never take effect";
    ASSERT_EQ(plain->dimensions_size(), carrying->dimensions_size());
    for (int i = 0; i < plain->dimensions_size(); ++i) {
        EXPECT_TRUE(MessageDifferencer::Equals(plain->dimensions(i), carrying->dimensions(i)))
            << "dimension " << plain->dimensions(i).dimension_id();
    }

    ASSERT_EQ(kSuccess, PetSystem::Summon(player, kPetId));
    BattlePetSnapshot carryingSnapshot;
    ASSERT_TRUE(PetSystem::BuildBattleSnapshot(player, carryingSnapshot));
    ASSERT_EQ(kSuccess, PetSystem::Recall(player));
    ASSERT_EQ(kSuccess, PetSystem::Summon(player, kTwinPetId));
    BattlePetSnapshot plainSnapshot;
    ASSERT_TRUE(PetSystem::BuildBattleSnapshot(player, plainSnapshot));
    plainSnapshot.set_pet_id(kPetId);  // 两只只差身份
    EXPECT_TRUE(MessageDifferencer::Equals(plainSnapshot, carryingSnapshot));
}

// 在已知维度上加点:未知键原样留着;目标里带 0 的已知维度不落新键;剩余点、加点上限都扣掉未知键;
// 目标里塞未知键按「不在本池」拒绝。
TEST_F(PetAllocationTest, AllocateOnKnownDimensionKeepsUnknownDimension) {
    constexpr uint32_t kAdded = 10;
    (*Pet().mutable_allocated())[kFutureDimension] = kFuturePoints;
    PetSystem::InitializeOnLoad(player);
    const uint32_t remaining = Total() - kKnownPoints - kFuturePoints;

    ASSERT_EQ(kSuccess, PetSystem::Allocate(player, kPetId, {{dimensionId, kKnownPoints + kAdded}, {otherDimensionId, 0}}));
    EXPECT_EQ(kKnownPoints + kAdded, Pet().allocated().at(dimensionId));
    EXPECT_FALSE(Pet().allocated().contains(otherDimensionId)) << "an unchanged zero target must not be written";
    ASSERT_TRUE(Pet().allocated().contains(kFutureDimension));
    EXPECT_EQ(kFuturePoints, Pet().allocated().at(kFutureDimension));
    EXPECT_EQ(2u, Pet().allocated().size());

    PetListInfo list;
    ASSERT_EQ(kSuccess, PetSystem::BuildList(player, list));
    const auto* info = FindInfo(list, kPetId);
    ASSERT_NE(nullptr, info);
    EXPECT_EQ(remaining - kAdded, info->remaining_points());

    const PlayerPetComp before = Comp();
    EXPECT_EQ(kPetDimensionNotFound, PetSystem::Allocate(player, kPetId, {{kFutureDimension, kFuturePoints + 1}}));
    EXPECT_EQ(kPetPointsNotEnough,
              PetSystem::Allocate(player, kPetId, {{dimensionId, kKnownPoints + kAdded + (remaining - kAdded) + 1}}));
    EXPECT_TRUE(MessageDifferencer::Equals(before, Comp()));
}

// 洗点按表收费,只清宝宝池已知维度;两种未知键原样留下,洗完的剩余点 = 总量 − 未知键。
TEST_F(PetAllocationTest, ResetKeepsUnknownKeys) {
    ASSERT_GT(Pool().reset_cost_gold(), 0u);
    ASSERT_LE(Pool().reset_free_below_level(), kSavedLevel) << "the fixture pet must be past the free-reset threshold";
    ASSERT_GT(kGold, Pool().reset_cost_gold());
    (*Pet().mutable_allocated())[kFutureDimension] = kFuturePoints;
    (*Pet().mutable_allocated())[playerDimensionId] = kForeignPoints;
    PetSystem::InitializeOnLoad(player);

    PetListInfo list;
    ASSERT_EQ(kSuccess, PetSystem::BuildList(player, list));
    const auto* info = FindInfo(list, kPetId);
    ASSERT_NE(nullptr, info);
    EXPECT_EQ(Pool().reset_cost_gold(), info->reset_cost_gold()) << "the list quotes the price the reset charges";

    ASSERT_EQ(kSuccess, PetSystem::Reset(player, kPetId));
    EXPECT_EQ(kGold - Pool().reset_cost_gold(), Gold());
    EXPECT_FALSE(Pet().allocated().contains(dimensionId));
    ASSERT_EQ(2u, Pet().allocated().size()) << "a reset must not touch keys this version cannot place in the pet pool";
    EXPECT_EQ(kFuturePoints, Pet().allocated().at(kFutureDimension));
    EXPECT_EQ(kForeignPoints, Pet().allocated().at(playerDimensionId));

    ASSERT_EQ(kSuccess, PetSystem::BuildList(player, list));
    info = FindInfo(list, kPetId);
    ASSERT_NE(nullptr, info);
    EXPECT_EQ(Total() - kFuturePoints - kForeignPoints, info->remaining_points());
}

// 宝宝池已知维度一点没加、只剩未知键:洗点是「无可洗」,不扣费,什么都不动。
TEST_F(PetAllocationTest, OnlyUnknownKeysResetIsNothingToChangeAndFree) {
    Pet().mutable_allocated()->erase(dimensionId);
    (*Pet().mutable_allocated())[kFutureDimension] = kFuturePoints;
    PetSystem::InitializeOnLoad(player);
    const auto before = AllocationOf(Pet());

    EXPECT_EQ(kPetNothingToChange, PetSystem::Reset(player, kPetId));
    EXPECT_EQ(kGold, Gold());
    EXPECT_EQ(before, AllocationOf(Pet()));
}

// 超额(本版本总量装不下)的加点:加载与升级重算都原样携带,整池按 0 生效 —— 二级属性、列表 value 与战斗快照
// 都等于一点没加的孪生宝宝;剩余 0,洗点报价 0。以前 RecalculateOne 直接清空返还(D4)。
TEST_F(PetAllocationTest, OverAllocationIsKeptButNotEffective) {
    const uint32_t over = Total() + 1;
    (*Pet().mutable_allocated())[dimensionId] = over;
    AddTwin(kTwinPetId);  // 一点没加
    PetSystem::InitializeOnLoad(player);
    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLevelChanged);
    ASSERT_EQ(1u, Pet().allocated().size());
    EXPECT_EQ(over, Pet().allocated().at(dimensionId)) << "an over-allocation is carried, not refunded";

    PetListInfo list;
    ASSERT_EQ(kSuccess, PetSystem::BuildList(player, list));
    const auto* carrying = FindInfo(list, kPetId);
    const auto* plain = FindInfo(list, kTwinPetId);
    ASSERT_NE(nullptr, carrying);
    ASSERT_NE(nullptr, plain);
    EXPECT_EQ(Total(), carrying->total_points());
    EXPECT_EQ(0u, carrying->remaining_points());
    EXPECT_EQ(0u, carrying->reset_cost_gold()) << "resetting an ineffective allocation is free";
    EXPECT_TRUE(MessageDifferencer::Equals(plain->derived(), carrying->derived()));
    ASSERT_EQ(plain->dimensions_size(), carrying->dimensions_size());
    for (int i = 0; i < carrying->dimensions_size(); ++i) {
        const auto& dimension = carrying->dimensions(i);
        EXPECT_EQ(plain->dimensions(i).value(), dimension.value()) << "dimension " << dimension.dimension_id();
        EXPECT_EQ(dimension.dimension_id() == dimensionId ? over : 0u, dimension.allocated())
            << "the list still shows the stored allocation; dimension " << dimension.dimension_id();
    }

    ASSERT_EQ(kSuccess, PetSystem::Summon(player, kPetId));
    BattlePetSnapshot carryingSnapshot;
    ASSERT_TRUE(PetSystem::BuildBattleSnapshot(player, carryingSnapshot));
    ASSERT_EQ(kSuccess, PetSystem::Recall(player));
    ASSERT_EQ(kSuccess, PetSystem::Summon(player, kTwinPetId));
    BattlePetSnapshot plainSnapshot;
    ASSERT_TRUE(PetSystem::BuildBattleSnapshot(player, plainSnapshot));
    plainSnapshot.set_pet_id(kPetId);
    EXPECT_TRUE(MessageDifferencer::Equals(plainSnapshot, carryingSnapshot));
}

// 加点不生效时剩余为 0:任何增量都回「点数不足」,组件一个字节不动。
TEST_F(PetAllocationTest, OverAllocationRejectsAllocateWithoutChange) {
    (*Pet().mutable_allocated())[dimensionId] = Total() + 1;
    PetSystem::InitializeOnLoad(player);
    const PlayerPetComp before = Comp();

    EXPECT_EQ(kPetPointsNotEnough, PetSystem::Allocate(player, kPetId, {{otherDimensionId, 1}}));
    EXPECT_EQ(kPetPointsNotEnough, PetSystem::Allocate(player, kPetId, {{dimensionId, Total() + 2}}));
    EXPECT_TRUE(MessageDifferencer::Equals(before, Comp()));
}

// 从不生效状态洗点:免费;旧上限 / 新上限都是生效值(前后都等于一点没加),当前气血 / 法力一点不动。
TEST_F(PetAllocationTest, ResetFromIneffectiveAllocationKeepsHealthAndMana) {
    (*Pet().mutable_allocated())[dimensionId] = Total() + 1;
    PetSystem::InitializeOnLoad(player);
    PetListInfo before;
    ASSERT_EQ(kSuccess, PetSystem::BuildList(player, before));
    const auto* beforeInfo = FindInfo(before, kPetId);
    ASSERT_NE(nullptr, beforeInfo);
    const uint64_t health = Pet().health();
    const uint64_t mana = Pet().mana();
    ASSERT_GT(health, 0u);
    ASSERT_LT(health, beforeInfo->derived().max_health()) << "a wounded pet shows whether the reset heals";
    ASSERT_LT(mana, beforeInfo->derived().max_mana());

    ASSERT_EQ(kSuccess, PetSystem::Reset(player, kPetId));
    EXPECT_EQ(kGold, Gold()) << "resetting an ineffective allocation is free";
    EXPECT_TRUE(Pet().allocated().empty());
    EXPECT_EQ(health, Pet().health());
    EXPECT_EQ(mana, Pet().mana());

    PetListInfo after;
    ASSERT_EQ(kSuccess, PetSystem::BuildList(player, after));
    const auto* afterInfo = FindInfo(after, kPetId);
    ASSERT_NE(nullptr, afterInfo);
    EXPECT_TRUE(MessageDifferencer::Equals(beforeInfo->derived(), afterInfo->derived()))
        << "the effective maximum does not move when an ineffective allocation is reset";
}

// 主人的生效等级低于出战宝宝(交易来的高级宝宝 / 超上限存储等级按上限读 / 混版本):加载不写 active_pet_id,
// 战斗快照照样把它拦在门外;等级真的变化(kLevelChanged)时才取消出战。
TEST_F(PetAllocationTest, ActivePetSurvivesLoadWhenOwnerLevelIsBelowIt) {
    constexpr uint32_t kLowOwnerLevel = 10;
    ASSERT_LT(kLowOwnerLevel, kSavedLevel);
    tlsEcs.actorRegistry.get<LevelComp>(player).set_level(kLowOwnerLevel);
    Comp().set_active_pet_id(kPetId);

    PetSystem::InitializeOnLoad(player);
    EXPECT_EQ(kPetId, Comp().active_pet_id()) << "loading must not rewrite the active pet";
    EXPECT_EQ(kSavedLevel, Pet().level());
    BattlePetSnapshot battle;
    EXPECT_FALSE(PetSystem::BuildBattleSnapshot(player, battle)) << "the battle entry still refuses a pet above its owner";
    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLoad);
    EXPECT_EQ(kPetId, Comp().active_pet_id());

    PetSystem::RecalculateAll(player, PetSystem::RecalcReason::kLevelChanged);
    EXPECT_EQ(0u, Comp().active_pet_id());
}

// 召唤入口独立按主人等级拦截:未出战的高级宝宝召唤不出来,出战状态不变。
TEST_F(PetAllocationTest, SummonOfNonActiveHigherLevelPetIsRejected) {
    constexpr uint32_t kLowOwnerLevel = 10;
    ASSERT_LT(kLowOwnerLevel, kSavedLevel);
    tlsEcs.actorRegistry.get<LevelComp>(player).set_level(kLowOwnerLevel);
    PetSystem::InitializeOnLoad(player);
    ASSERT_EQ(0u, Comp().active_pet_id());

    EXPECT_EQ(kPetOwnerLevelNotEnough, PetSystem::Summon(player, kPetId));
    EXPECT_EQ(0u, Comp().active_pet_id());
}
} // namespace
