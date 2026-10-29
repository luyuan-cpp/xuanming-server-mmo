
#include "player_activity_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "services/scene/player/system/player_feature_snapshot.h"
#include "services/scene/player/system/player_activity.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include <muduo/base/Timestamp.h>
///<<< END WRITING YOUR CODE

void SceneActivityClientPlayerHandler::GetActivityList(entt::entity player,const ::GetActivityListRequest* request,
	::GetActivityListResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerActivityReadSystem::BuildList(player, nowMs, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}

void SceneActivityClientPlayerHandler::ClaimActivityVitalityReward(entt::entity player,const ::ClaimActivityVitalityRewardRequest* request,
	::ClaimActivityVitalityRewardResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerActivitySystem::Claim(player, nowMs, *request, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}
