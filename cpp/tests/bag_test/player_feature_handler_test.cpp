#include <gtest/gtest.h>
#include <memory>

#include "../../nodes/scene/handler/rpc/player/player_activity_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_bag_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_mission_handler.h"
#include "modules/bag/comp/player_bags_comp.h"
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
} // namespace
