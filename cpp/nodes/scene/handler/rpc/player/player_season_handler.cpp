
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

void SceneSeasonClientPlayerHandler::EquipWuxingFormation(entt::entity player,const ::EquipWuxingFormationRequest* request,
	::EquipWuxingFormationResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::EquipFormation(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::GetWuxingTowerState(entt::entity player,const ::GetWuxingTowerStateRequest* request,
	::GetWuxingTowerStateResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	(void)request;
	if (const uint32_t err = PlayerSeasonSystem::GetWuxingTowerState(player, TimeSystem::NowMillisecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::StartWuxingTowerRun(entt::entity player,const ::StartWuxingTowerRunRequest* request,
	::StartWuxingTowerRunResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::StartWuxingTowerRun(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::PickWuxingTowerSigil(entt::entity player,const ::PickWuxingTowerSigilRequest* request,
	::PickWuxingTowerSigilResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::PickWuxingTowerSigil(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::AbandonWuxingTowerRun(entt::entity player,const ::AbandonWuxingTowerRunRequest* request,
	::AbandonWuxingTowerRunResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::AbandonWuxingTowerRun(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::SaveBattleBuildBookmark(entt::entity player,const ::SaveBattleBuildBookmarkRequest* request,
	::SaveBattleBuildBookmarkResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::SaveBattleBuildBookmark(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::DeleteBattleBuildBookmark(entt::entity player,const ::DeleteBattleBuildBookmarkRequest* request,
	::DeleteBattleBuildBookmarkResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::DeleteBattleBuildBookmark(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::SetRetroHonorTarget(entt::entity player,const ::SetRetroHonorTargetRequest* request,
	::SetRetroHonorTargetResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::SetRetroHonorTarget(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::ChooseWuxingRealmBranch(entt::entity player,const ::ChooseWuxingRealmBranchRequest* request,
	::ChooseWuxingRealmBranchResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::ChooseWuxingRealmBranch(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneSeasonClientPlayerHandler::ResolveWuxingRealmEvent(entt::entity player,const ::ResolveWuxingRealmEventRequest* request,
	::ResolveWuxingRealmEventResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerSeasonSystem::ResolveWuxingRealmEvent(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}
