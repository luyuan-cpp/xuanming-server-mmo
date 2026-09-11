#include <gtest/gtest.h>
#include <memory>

#include "../../nodes/scene/handler/rpc/player/player_activity_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_bag_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_mission_handler.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/mission/comp/mission_comp.h"
#include "modules/mission/system/mission.h"
#include "modules/condition/condition_type.h"
#include "proto/common/event/mission_event.pb.h"
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
#include "thread_context/ecs_context.h"

namespace {
class TestBagService : public SceneBagClientPlayer {};
class TestMissionService : public SceneMissionClientPlayer {};
class TestActivityService : public SceneActivityClientPlayer {};

TEST(PlayerFeatureHandlerTest, BagRejectionSurvivesGeneratedErrorTransferAndDoesNotLeak) {
    auto& tip = tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity());
    tip.Clear();
    const auto player = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
    SceneBagClientPlayerHandler handler(std::make_unique<TestBagService>());
    GetBagRequest request;
    request.set_bag_type(4);
    GetBagResponse response;
    handler.CallMethod(SceneBagClientPlayer::descriptor()->method(0), player, &request, &response);
    EXPECT_EQ(kInvalidParameter, response.error_message().id());
    EXPECT_FALSE(response.has_bag());
    EXPECT_EQ(0u, tip.id());
    request.set_bag_type(0);
    response.Clear();
    handler.CallMethod(SceneBagClientPlayer::descriptor()->method(0), player, &request, &response);
    EXPECT_EQ(0u, response.error_message().id());
    EXPECT_TRUE(response.has_bag());
    tlsEcs.actorRegistry.destroy(player);
}

TEST(PlayerFeatureHandlerTest, MissionInvalidEntityRemainsAnErrorAfterGeneratedTransfer) {
    tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).Clear();
    SceneMissionClientPlayerHandler handler(std::make_unique<TestMissionService>());
    GetMissionListRequest request;
    GetMissionListResponse response;
    handler.CallMethod(SceneMissionClientPlayer::descriptor()->method(0), entt::null, &request, &response);
    EXPECT_EQ(kThisEntityIsInvalid, response.error_message().id());
    EXPECT_TRUE(response.missions().empty());
}

TEST(PlayerFeatureHandlerTest, ActivityInvalidEntityRemainsAnErrorAfterGeneratedTransfer) {
    tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).Clear();
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
        Tip().Clear();
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<Guid>(player, Guid{454545});
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(Guid{454545});
        ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
    }
    void TearDown() override {
        tlsEcs.dispatcher.clear<AcceptMissionEvent>();
        tlsEcs.dispatcher.clear<ConditionEvent>();
        tlsEcs.dispatcher.clear<OnAcceptedMissionEvent>();
        tlsEcs.dispatcher.clear<OnMissionAwardEvent>();
        Tip().Clear();
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
        EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
    }
    TipInfoMessage& Tip() { return tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()); }
    MissionsComp& Missions() { return tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(0); }
    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
    GetMissionListResponse Act(bool claim, uint32_t id = 12, uint32_t scope = 0) {
        MissionActionRequest request;
        request.set_scope(scope);
        request.set_mission_id(id);
        GetMissionListResponse response;
        handler.CallMethod(claim ? SceneMissionClientPlayerClaimMissionRewardMethod :
                                   SceneMissionClientPlayerAcceptMissionMethod, player, &request, &response);
        EXPECT_EQ(0u, Tip().id());
        return response;
    }
    GetMissionListResponse Read() {
        GetMissionListRequest request;
        GetMissionListResponse response;
        handler.CallMethod(SceneMissionClientPlayerGetMissionListMethod, player, &request, &response);
        EXPECT_EQ(0u, Tip().id());
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
        kill.set_entity(entt::to_integral(player));
        kill.set_condition_type(static_cast<uint32_t>(eConditionType::kConditionKillMonster));
        kill.add_condition_ids(1);
        kill.set_amount(1);
        MissionSystem::HandleConditionEvent(kill, Missions(), MissionConfig::GetSingleton());
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
        EXPECT_EQ(0u, Tip().id());
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
} // namespace
