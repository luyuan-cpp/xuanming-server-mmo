
#include "player_jianghu_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/time/system/time.h"
#include "services/scene/player/system/player_jianghu.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneJianghuClientPlayerHandler::GetJianghuProfile(entt::entity player,const ::GetJianghuProfileRequest* request,
	::GetJianghuProfileResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	(void)request;
	if (const auto err = PlayerJianghuSystem::GetProfile(player, TimeSystem::NowMillisecondsUTC(), *response->mutable_profile());
		err != kSuccess)
	{
		response->clear_profile();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneJianghuClientPlayerHandler::SetJianghuRefuseStrangers(entt::entity player,const ::SetJianghuRefuseStrangersRequest* request,
	::SetJianghuRefuseStrangersResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerJianghuSystem::SetRefuseStrangers(
			player, request->refuse_strangers(), TimeSystem::NowMillisecondsUTC(), *response->mutable_profile());
		err != kSuccess)
	{
		response->clear_profile();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneJianghuClientPlayerHandler::ClearJianghuDefeatMark(entt::entity player,const ::ClearJianghuDefeatMarkRequest* request,
	::ClearJianghuDefeatMarkResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerJianghuSystem::ClearDefeatMark(
			player, *request, TimeSystem::NowMillisecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}
