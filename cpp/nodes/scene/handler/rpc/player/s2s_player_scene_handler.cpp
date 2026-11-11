
#include "s2s_player_scene_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/type_define/player_guid_comp.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_scene.h"
#include "player/system/player_team.h"
#include "player/system/player_tip.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/scene_error_tip.pb.h"
#include "proto/scene/player_scene.pb.h"
#include "modules/scene/comp/scene_comp.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"
#include "network/client_message_sender.h"

///<<< END WRITING YOUR CODE

void SceneScenePlayerHandler::EnterScene(entt::entity player,const ::GsEnterSceneRequest* request,
	::google::protobuf::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE

	// Player should already be loaded by PlayerEnterGameNode (Node Handler).
	// We just handle the logical entry into the specific scene instance here.
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "Player entity not found in EnterScene handler. This should not happen if PlayerEnterGameNode was called first.";
		return;
	}

	// 以前直接 entt::entity{scene_id} 当场景句柄:要么查不到任何场景,要么撞上一个无关场景;
	// 而且不管进没进成功都会接着跑 OnEnteredScene(ecs-entt-misuse-audit-20261015 F091)。
	// 现在场景不在本节点就拒绝,后面的入场副作用一个都不做。
	const SceneEntity sceneEntity = PlayerSceneSystem::FindSceneById(request->scene_id());
	if (const auto result = PlayerSceneSystem::CheckEnterScene(player, sceneEntity); result != kSuccess)
	{
		LOG_ERROR << "Scene entry rejected scene_id=" << request->scene_id() << " tip=" << result
				  << " player=" << tlsEcs.GetPlayerId(player)
				  << " entity=" << entt::to_integral(player);
		PlayerTipSystem::SendToPlayer(player, result, {});
		return;
	}

	if (!PlayerSceneSystem::HandleEnterScene(player, sceneEntity))
	{
		if (tlsEcs.actorRegistry.valid(player))
			PlayerTipSystem::SendToPlayer(player, kEnterSceneFailed, {});
		return;
	}
	// 组队刷新与同节点跟随放在 HandleEnterScene 之后(其幂等早退会漏刷新,team-system.md §F.2)
	PlayerTeamSystem::OnEnteredScene(player);
	///<<< END WRITING YOUR CODE

}

void SceneScenePlayerHandler::LeaveScene(entt::entity player,const ::GsLeaveSceneRequest* request,
	::google::protobuf::Empty* response)
{
	///<<< BEGIN WRITING YOUR CODE
	LOG_DEBUG << "Handling GsLeaveSceneRequest for player: " << tlsEcs.GetPlayerId(player);

	// Save player data to Redis and handle exit logic.
	// This replaces the legacy ChangeSceneInfo state machine checks with a direct save-and-exit flow.
	PlayerLifecycleSystem::HandleExitGameNode(player);

	///<<< END WRITING YOUR CODE

}

void SceneScenePlayerHandler::EnterSceneS2C(entt::entity player,const ::EnterSceneS2CRequest* request,
	::EnterSceneS2CResponse* response)
{
	///<<< BEGIN WRITING YOUR CODE
	LOG_INFO << "Handling EnterSceneS2CRequest for player: " << tlsEcs.GetPlayerId(player);

	const auto sceneEntity = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (sceneEntity == nullptr)
	{
		LOG_ERROR << "Player " << tlsEcs.GetPlayerId(player) << " has not entered any scene.";
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
	SendToPlayerClient(SceneSceneClientPlayerNotifyEnterSceneMessageId, message, player);
	///<<< END WRITING YOUR CODE

}
