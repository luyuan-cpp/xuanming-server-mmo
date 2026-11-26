
#include "player_rewarded_ad_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/time/system/time.h"
#include "services/scene/player/system/player_rewarded_ad.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneRewardedAdClientPlayerHandler::GetRewardedAdInfo(entt::entity player,const ::GetRewardedAdInfoRequest* request,
	::GetRewardedAdInfoResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerRewardedAdSystem::GetInfo(player, *request, TimeSystem::NowMillisecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE
}

void SceneRewardedAdClientPlayerHandler::BeginRewardedAdSession(entt::entity player,const ::BeginRewardedAdSessionRequest* request,
	::BeginRewardedAdSessionResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerRewardedAdSystem::BeginSession(player, *request, TimeSystem::NowMillisecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE
}

void SceneRewardedAdClientPlayerHandler::ClaimRewardedAd(entt::entity player,const ::ClaimRewardedAdRequest* request,
	::ClaimRewardedAdResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerRewardedAdSystem::Claim(player, *request, TimeSystem::NowMillisecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE
}
