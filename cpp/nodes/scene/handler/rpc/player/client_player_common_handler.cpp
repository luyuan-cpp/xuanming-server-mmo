
#include "client_player_common_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/time/system/time.h"
#include "services/scene/player/system/player_rename.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneClientPlayerCommonHandler::SendTipToClient(entt::entity player,const ::TipInfoMessage* request,
	::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE
	///<<< END WRITING YOUR CODE

}

void SceneClientPlayerCommonHandler::KickPlayer(entt::entity player,const ::GameKickPlayerRequest* request,
	::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE
	///<<< END WRITING YOUR CODE

}

void SceneClientPlayerCommonHandler::RedirectToGate(entt::entity player,const ::RedirectToGateNotify* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneClientPlayerCommonHandler::RenamePlayer(entt::entity player,const ::RenamePlayerRequest* request,
	::RenamePlayerResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (request == nullptr || response == nullptr)
	{
		return;
	}
	const uint32_t tipId = PlayerRenameSystem::RequestRename(player, request->name(), TimeSystem::NowMillisecondsUTC());
	if (tipId != kSuccess)
	{
		response->mutable_error_message()->set_id(tipId);
	}
///<<< END WRITING YOUR CODE

}

void SceneClientPlayerCommonHandler::NotifyPlayerRenamed(entt::entity player,const ::PlayerRenamedNotify* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}
