
#include "player_activity_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "services/scene/player/system/player_feature_snapshot.h"
#include "services/scene/player/system/player_activity.h"
#include "services/scene/player/system/player_festival_beast.h"
#include "services/scene/player/system/player_travel_pass.h"
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

void SceneActivityClientPlayerHandler::GetFestivalBeastDetail(entt::entity player,const ::GetFestivalBeastDetailRequest* request,
	::GetFestivalBeastDetailResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerFestivalBeastSystem::GetDetail(player, nowMs, *request, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}

void SceneActivityClientPlayerHandler::StartFestivalBeastChallenge(entt::entity player,const ::StartFestivalBeastChallengeRequest* request,
	::StartFestivalBeastChallengeResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerFestivalBeastSystem::StartChallenge(player, nowMs, *request, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}

void SceneActivityClientPlayerHandler::ClaimFestivalBeastReward(entt::entity player,const ::ClaimFestivalBeastRewardRequest* request,
	::ClaimFestivalBeastRewardResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerFestivalBeastSystem::ClaimReward(player, nowMs, *request, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}

void SceneActivityClientPlayerHandler::GetTravelPassState(entt::entity player,const ::GetTravelPassStateRequest* request,
	::GetTravelPassStateResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerTravelPassSystem::GetState(player, nowMs, request->pass_id(), *response->mutable_snapshot());
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}

void SceneActivityClientPlayerHandler::UnlockTravelPassPremium(entt::entity player,const ::UnlockTravelPassPremiumRequest* request,
	::UnlockTravelPassPremiumResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerTravelPassSystem::UnlockPremium(player, *request, nowMs, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}

void SceneActivityClientPlayerHandler::ClaimTravelPassReward(entt::entity player,const ::ClaimTravelPassRewardRequest* request,
	::ClaimTravelPassRewardResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerTravelPassSystem::ClaimReward(player, *request, nowMs, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}

void SceneActivityClientPlayerHandler::EquipTravelPassCosmetic(entt::entity player,const ::EquipTravelPassCosmeticRequest* request,
	::EquipTravelPassCosmeticResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerTravelPassSystem::EquipCosmetic(player, nowMs, *request, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}

void SceneActivityClientPlayerHandler::SelectActiveTravelPass(entt::entity player,const ::SelectActiveTravelPassRequest* request,
	::SelectActiveTravelPassResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerTravelPassSystem::SelectActivePass(player, nowMs, *request, *response);
    if (result != kSuccess) response->mutable_error_message()->set_id(result);
///<<< END WRITING YOUR CODE

}
