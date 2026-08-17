
#include "player_scene_handler.h"

///<<< BEGIN WRITING YOUR CODE

#include "node/system/node/node.h"
#include "table/proto/tip/scene_error_tip.pb.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "engine/thread_context/node_context_manager.h"
#include "network/network_utils.h"
#include "network/node_utils.h"
#include "network/player_message_utils.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"
#include "player/system/player_scene.h"
#include "battle/system/player_battle.h"
///<<< END WRITING YOUR CODE

void SceneSceneClientPlayerHandler::EnterScene(entt::entity player,const ::EnterSceneC2SRequest* request,
	::EnterSceneC2SResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
	LOG_TRACE << "EnterSceneC2S request received for player: " << (g ? *g : 0)
		<< ", scene_info: " << request->scene_info().ShortDebugString();

	auto game_node_type = gNode->GetNodeInfo().scene_node_type();
	if (game_node_type == eSceneNodeType::kSceneNode ||
		game_node_type == eSceneNodeType::kSceneSceneCrossNode)
	{
		LOG_ERROR << "EnterSceneC2S request rejected due to server type: " << game_node_type;
		response->mutable_error_message()->set_id(kEnterSceneServerType);
		return;
	}

	// 回合制战斗冻结拦截:战斗在途拒绝切场景(含镜像分支;设计文档 §5.3 冻结清单)。
	// 结算落地摘除 InBattleComp 前,玩家必须留在当前场景节点接收结算事件。
	if (PlayerBattleSystem::IsInBattle(player))
	{
		LOG_WARN << "[PlayerBattle] EnterSceneC2S 被拒: 玩家战斗在途, player_id=" << (g ? *g : 0);
		response->mutable_error_message()->set_id(kEnterSceneFailed);
		return;
	}

	const auto& scene_info = request->scene_info();
	if (scene_info.scene_config_id() <= 0 && scene_info.scene_id() <= 0
	    && scene_info.mirror_config_id() <= 0)
	{
		LOG_ERROR << "EnterSceneC2S request rejected due to invalid scene_info: " << scene_info.ShortDebugString();
		response->mutable_error_message()->set_id(kEnterSceneParamError);
		return;
	}

	// Mirror-entry branch: mirror_config_id > 0 AND scene_id == 0 means
	// "allocate a fresh mirror of my current scene, then put me in it".
	// scene_id > 0 with mirror_config_id > 0 is still a plain EnterScene
	// (joining an existing mirror by id) and falls through to the normal
	// path below. We dispatch via PlayerSceneSystem::RequestEnterMirrorScene,
	// which fires CreateScene at SceneManager; the async CreateScene reply
	// handler echoes creator_ids and drives the follow-up EnterScene.
	// Success here is "request queued", not "player entered" — mirror
	// creation is async and the client sees the normal EnterSceneS2C
	// when the pipeline completes.
	if (scene_info.mirror_config_id() > 0 && scene_info.scene_id() == 0)
	{
		if (PlayerSceneSystem::RequestEnterMirrorScene(player, scene_info.mirror_config_id()))
		{
			LOG_INFO << "EnterSceneC2S: mirror request dispatched player=" << (g ? *g : 0)
			         << " mirror_config=" << scene_info.mirror_config_id();
			return;
		}
		LOG_ERROR << "EnterSceneC2S: mirror dispatch failed player=" << (g ? *g : 0)
		          << " mirror_config=" << scene_info.mirror_config_id();
		response->mutable_error_message()->set_id(kEnterSceneParamError);
		return;
	}

	if (auto current_scene_comp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player))
	{
		const auto current_scene_info = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(current_scene_comp->sceneEntity);
		if (current_scene_info && current_scene_info->scene_id() == scene_info.scene_id() && scene_info.scene_id() > 0)
		{
			LOG_WARN << "Player " << (g ? *g : 0) << " is already in the requested scene: " << scene_info.scene_id();
			response->mutable_error_message()->set_id(kEnterSceneYouInCurrentScene);
			return;
		}
	}

	const auto* playerSessionPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (!playerSessionPB)
	{
		LOG_ERROR << "EnterSceneC2S: PlayerSessionSnapshotComp missing for player " << (g ? *g : 0);
		response->mutable_error_message()->set_id(kEnterSceneParamError);
		return;
	}

	// Resolve gate info. Same-zone callers only — scene never reaches a
	// cross-zone gate directly; cross-zone traffic is redirected at the
	// SceneManager layer. (zone, node_id) is sufficient here.
	NodeId gateNodeId = GetGateNodeId(playerSessionPB->gate_session_id());
	std::string gateInstanceId;
	if (auto gateEntityOpt = ResolveLocalZoneGateEntity(playerSessionPB->gate_session_id()); gateEntityOpt)
	{
		auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
		if (const auto* gateNodeInfo = gateRegistry.try_get<NodeInfo>(*gateEntityOpt))
		{
			gateInstanceId = gateNodeInfo->node_uuid();
		}
	}

	// Find a SceneManager gRPC node
	auto smEntity = GetSceneManagerEntity(playerSessionPB->player_id());
	if (smEntity == entt::null)
	{
		LOG_ERROR << "EnterSceneC2S: No SceneManager node available for player " << playerSessionPB->player_id();
		response->mutable_error_message()->set_id(kEnterSceneParamError);
		return;
	}

	// Build EnterSceneRequest for SceneManager
	auto &smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
	::scene_manager::EnterSceneRequest req;
	req.set_player_id(playerSessionPB->player_id());
	req.set_scene_id(scene_info.scene_id());
	req.set_scene_conf_id(scene_info.scene_config_id());
	req.set_session_id(playerSessionPB->gate_session_id());
	req.set_gate_id(std::to_string(gateNodeId));
	req.set_gate_instance_id(gateInstanceId);
	req.set_gate_zone_id(GetZoneId());
	req.set_zone_id(GetZoneId());

	scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req);

	LOG_TRACE << "EnterSceneC2S: Sent EnterScene to SceneManager for player " << playerSessionPB->player_id()
			  << ", scene_config_id=" << scene_info.scene_config_id()
			  << ", scene_id=" << scene_info.scene_id();
	///<<< END WRITING YOUR CODE

}

void SceneSceneClientPlayerHandler::NotifyEnterScene(entt::entity player,const ::EnterSceneS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
SendMessageToClientViaGate(SceneSceneClientPlayerNotifyEnterSceneMessageId, *request, player);
///<<< END WRITING YOUR CODE

}

void SceneSceneClientPlayerHandler::SceneInfoC2S(entt::entity player,const ::SceneInfoRequest* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
	auto* sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (!sceneEntityComp || sceneEntityComp->sceneEntity == entt::null)
	{
		LOG_WARN << "SceneInfoC2S: Player not in any scene";
		return;
	}

	const auto* sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntityComp->sceneEntity);
	if (!sceneInfo)
	{
		LOG_WARN << "SceneInfoC2S: Scene info not found for player's scene entity";
		return;
	}

	SceneInfoS2C message;
	message.add_scene_info()->CopyFrom(*sceneInfo);
	SendMessageToClientViaGate(SceneSceneClientPlayerNotifySceneInfoMessageId, message, player);
///<<< END WRITING YOUR CODE

}

void SceneSceneClientPlayerHandler::NotifySceneInfo(entt::entity player,const ::SceneInfoS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
SendMessageToClientViaGate(SceneSceneClientPlayerNotifySceneInfoMessageId, *request, player);
///<<< END WRITING YOUR CODE

}

void SceneSceneClientPlayerHandler::NotifyActorCreate(entt::entity player,const ::ActorCreateS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneSceneClientPlayerHandler::NotifyActorDestroy(entt::entity player,const ::ActorDestroyS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneSceneClientPlayerHandler::NotifyActorListCreate(entt::entity player,const ::ActorListCreateS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneSceneClientPlayerHandler::NotifyActorListDestroy(entt::entity player,const ::ActorListDestroyS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}
