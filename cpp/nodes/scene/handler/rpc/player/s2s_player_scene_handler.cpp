
#include "s2s_player_scene_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "player/system/player_lifecycle.h"
#include "player/system/player_scene.h"
#include "player/system/player_team.h"
#include "proto/scene/player_scene.pb.h"
#include "modules/scene/comp/scene_comp.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"
#include "network/player_message_utils.h"

///<<< END WRITING YOUR CODE

void SceneScenePlayerHandler::EnterScene(entt::entity player,const ::GsEnterSceneRequest* request,
	::google::protobuf::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE

	// Player should already be loaded by PlayerEnterGameNode (Node Handler).
	// We just handle the logical entry into the specific scene instance here.
	if (tlsEcs.actorRegistry.valid(player))
	{
		PlayerSceneSystem::HandleEnterScene(player, entt::to_entity(request->scene_id()));
		// 组队刷新与同节点跟随放在 HandleEnterScene 之后(其幂等早退会漏刷新,team-system.md §F.2)
		PlayerTeamSystem::OnEnteredScene(player);
	}
	else
	{
		LOG_ERROR << "Player entity not found in EnterScene handler. This should not happen if PlayerEnterGameNode was called first.";
	}
	///<<< END WRITING YOUR CODE

}

void SceneScenePlayerHandler::LeaveScene(entt::entity player,const ::GsLeaveSceneRequest* request,
	::google::protobuf::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE
	const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
	LOG_DEBUG << "Handling GsLeaveSceneRequest for player: " << (g ? *g : 0);

	// Save player data to Redis and handle exit logic.
	// This replaces the legacy ChangeSceneInfo state machine checks with a direct save-and-exit flow.
	PlayerLifecycleSystem::HandleExitGameNode(player);

	///<<< END WRITING YOUR CODE

}

void SceneScenePlayerHandler::EnterSceneS2C(entt::entity player,const ::EnterSceneS2CRequest* request,
	::EnterSceneS2CResponse* response)
{
	///<<< BEGIN WRITING YOUR CODE
	const auto* g2 = tlsEcs.actorRegistry.try_get<Guid>(player);
	LOG_INFO << "Handling EnterSceneS2CRequest for player: " << (g2 ? *g2 : 0);

	const auto sceneEntity = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (sceneEntity == nullptr)
	{
		LOG_ERROR << "Player " << (g2 ? *g2 : 0) << " has not entered any scene.";
		return;
	}

	::EnterSceneS2C message;
	const auto* sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntity->sceneEntity);
	if (!sceneInfo)
	{
		LOG_ERROR << "SceneInfoComp not found for scene entity";
		return;
	}
	message.mutable_scene_info()->CopyFrom(*sceneInfo);
	SendMessageToClientViaGate(SceneSceneClientPlayerNotifyEnterSceneMessageId, message, player);
	///<<< END WRITING YOUR CODE

}
