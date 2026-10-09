
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
#include "player/system/player_lifecycle.h"
#include "battle/system/player_battle.h"

namespace
{
	// 拒绝码必须写进 TLS 的 TipInfoMessage,不能直接写 response->error_message:
	// 生成的 CallMethod 在 handler 返回后执行 TRANSFER_ERROR_MESSAGE,用 TLS 那份**整体覆盖**
	// response 的 error_message。直接写 response 的码会被一个空 tip 盖掉,客户端看到的是"成功"。
	// (本文件 EnterScene 原先的 7 处拒绝就是这样全部失效的。)写法照 player_attribute_handler.cpp。
	// helper 只能放在文件头这个守护段里:守护段之外的手写内容 regen 时会整段丢失。
	void SetTip(uint32_t err)
	{
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(err);
	}
} // namespace
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
		SetTip(kEnterSceneServerType);
		return;
	}

	// 回合制战斗冻结拦截:战斗在途拒绝切场景(含镜像分支;设计文档 §5.3 冻结清单)。
	// 结算落地摘除 InBattleComp 前,玩家必须留在当前场景节点接收结算事件。
	if (PlayerBattleSystem::IsInBattle(player))
	{
		LOG_WARN << "[PlayerBattle] EnterSceneC2S 被拒: 玩家战斗在途, player_id=" << (g ? *g : 0);
		SetTip(kEnterSceneFailed);
		return;
	}

	// 发送侧闸:归属交接在途(已冻结,盘上那份才是真值),或上一条 EnterScene 的应答还没回来。
	// 应答已按 correlation_id 对号、不再串号;闸仍然必要,因为在途记录只有一个槽:第二条会覆盖第一条的号,
	// 第一条随后到达的 18 就对不上号而被丢弃(旧版 scene_manager 不回显号时还会按 player_id 串号)。
	// 放在镜像分支之前:冻结中的玩家同样不该去创建镜像。
	if (PlayerLifecycleSystem::IsSceneChangeBusy(player))
	{
		LOG_INFO << "EnterSceneC2S rejected: scene change already in flight, player_id=" << (g ? *g : 0);
		SetTip(kEnterSceneChangingScene);
		return;
	}

	const auto& scene_info = request->scene_info();
	if (scene_info.scene_config_id() <= 0 && scene_info.scene_id() <= 0
	    && scene_info.mirror_config_id() <= 0)
	{
		LOG_ERROR << "EnterSceneC2S request rejected due to invalid scene_info: " << scene_info.ShortDebugString();
		SetTip(kEnterSceneParamError);
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
		SetTip(kEnterSceneParamError);
		return;
	}

	if (auto current_scene_comp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player))
	{
		const auto current_scene_info = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(current_scene_comp->sceneEntity);
		if (current_scene_info && current_scene_info->scene_id() == scene_info.scene_id() && scene_info.scene_id() > 0)
		{
			LOG_WARN << "Player " << (g ? *g : 0) << " is already in the requested scene: " << scene_info.scene_id();
			SetTip(kEnterSceneYouInCurrentScene);
			return;
		}
	}

	const auto* playerSessionPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (!playerSessionPB)
	{
		LOG_ERROR << "EnterSceneC2S: PlayerSessionSnapshotComp missing for player " << (g ? *g : 0);
		SetTip(kEnterSceneParamError);
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
		// 一个 SceneManager 都没注册:是服务端暂时不可用,不是客户端参数错。与 EnterScene 传输失败
		// (PlayerLifecycleSystem::DispatchEnterSceneTransportFailure)回的是同一个提示「服务器繁忙,请稍后再试」。
		SetTip(kEnterSceneServerBusy);
		return;
	}

	// Build EnterSceneRequest for SceneManager
	::scene_manager::EnterSceneRequest req;
	req.set_player_id(playerSessionPB->player_id());
	req.set_scene_id(scene_info.scene_id());
	req.set_scene_conf_id(scene_info.scene_config_id());
	req.set_session_id(playerSessionPB->gate_session_id());
	req.set_gate_id(std::to_string(gateNodeId));
	req.set_gate_instance_id(gateInstanceId);
	req.set_gate_zone_id(GetZoneId());
	req.set_zone_id(GetZoneId());
	// client_channel_pick = "这个 scene_id 是玩家自己点选的"。scene_manager 据此对大世界分线做
	// 回收中 / 人数上限 / 冷却校验(docs/design/world-channel-switch.md §4.4);目标不是分线
	// (按 id 进副本 / 镜像)时它不产生任何效果。
	// 只有本 handler 置位:队伍跟随等服务器代发的请求不经过这里,所以不带它,不受上限与冷却约束。
	// 跨节点交接的重发是重新构造的请求,同样不带它 —— 有意如此:预检只在第一跳做,重发直接放行,
	// 否则第一跳记下的冷却会把重发自己挡住。
	req.set_client_channel_pick(scene_info.scene_id() > 0);

	// RequestSceneChange 在发送之前把"这次要去哪"与关联号记进单槽在途记录,再经统一出口带号发出。
	// 目标场景若在别的节点,生产配置下 scene_manager 会先以 18 暂拒(它要求源 scene 先存盘并出示标记),
	// 应答不带请求内容 —— 按 correlation_id 对回这条记录,才能用同一个目标起交接并重发
	// (PlayerLifecycleSystem::DispatchEnterSceneReply)。
	const uint64_t correlationId = PlayerLifecycleSystem::RequestSceneChange(player, smEntity, req);

	LOG_TRACE << "EnterSceneC2S: Sent EnterScene to SceneManager for player " << playerSessionPB->player_id()
			  << ", scene_config_id=" << scene_info.scene_config_id()
			  << ", scene_id=" << scene_info.scene_id()
			  << ", corr=" << correlationId;
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
	// handler 只委托:取当前场景信息、(大世界线上)异步读分线目录、推 NotifySceneInfo(31)都在系统层
	// (docs/design/world-channel-switch.md §5)。本方法的应答类型是 Empty,数据走那条推送。
	// 分线目录只在请求显式要的时候才读、才带:不带 with_channel_directory 的请求开销与改动前相同。
	PlayerSceneSystem::SendSceneInfo(player, request->with_channel_directory());
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

void SceneSceneClientPlayerHandler::TravelToZone(entt::entity player,const ::TravelToZoneRequest* request,
	::TravelToZoneResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 跨 zone 场景传送入口(cross-zone-scene-travel.md CZ-7)。handler 只委托系统层:
	// CZ-6 校验、冻结、存盘、交接全在 PlayerLifecycleSystem::RequestZoneTravel。
	// 无错应答 = 已受理,不代表已到达:到达 = 客户端随后收到 RedirectToGate;
	// 未成 = 随后收到 SendTipToClient(kZoneTravel*)且已解冻。拒绝码写 TLS tip(见文件头 SetTip)。
	if (const uint32_t tip = PlayerLifecycleSystem::RequestZoneTravel(player, request->target_zone_id(),
																	  request->scene_config_id());
		tip != PlayerLifecycleSystem::kTravelAccepted)
	{
		SetTip(tip);
	}
///<<< END WRITING YOUR CODE

}
