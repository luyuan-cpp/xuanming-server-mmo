
#include "player_season_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/time/system/time.h"
#include "services/scene/player/system/player_season.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneSeasonClientPlayerHandler::GetSeasonState(entt::entity player,const ::GetSeasonStateRequest* request,
	::GetSeasonStateResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	(void)request;
	if (const uint32_t err = PlayerSeasonSystem::GetState(player, TimeSystem::NowMillisecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::EquipSeasonSeal(entt::entity player,const ::EquipSeasonSealRequest* request,
	::EquipSeasonSealResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::EquipSeal(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::CatchupSeasonSeal(entt::entity player,const ::CatchupSeasonSealRequest* request,
	::CatchupSeasonSealResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::Catchup(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}
