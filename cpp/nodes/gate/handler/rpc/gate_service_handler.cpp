
#include "gate_service_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "muduo/base/Logging.h"

#include "gate_codec.h"
#include "gate_security.h"
#include "node/system/node/node.h"
#include "network/rpc_client.h"
#include "thread_context/node_context_manager.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "proto/common/base/message.pb.h"

#include "proto/common/component/player_network_comp.pb.h"
#include "network/broadcast_target_codec.h"
#include <session/manager/session_manager.h>
#include <session/system/session_identity_fence.h>

#include <algorithm>
#include <string>

namespace
{
	// 身份栅栏(routing-identity-audit-20260908.md R13/R14)的两条日志。
	//
	// 失配:这是**真正要报警的那一条** —— 要么 routing node_id 被复用后 scene 反查到了
	// 继任 gate,要么单个 gate 内 session 序号回绕后同号 session 已易主。两种都意味着
	// 有一条推送本来会写进别人的 socket。带上两个 id 才能事后判断是哪一种。
	void LogFenceMismatch(const char *where, uint32_t sessionId, uint64_t sessionPlayerId,
						  uint64_t targetPlayerId, uint32_t messageId)
	{
		LOG_WARN << where << ": target player mismatch, dropping push. session_id=" << sessionId
				 << " session_player_id=" << sessionPlayerId
				 << " target_player_id=" << targetPlayerId
				 << " message_id=" << messageId;
	}

	// 兼容位:老发送方不填 target_player_id。只在**首次**记一行 INFO ——
	// 迁移窗口里这条会对每一条推送成立,逐条打就是把 gate 日志打爆(gate 日志曾因
	// 逐条 WARN 涨到 ~2GB 并偷走 IO 线程 CPU)。看到这一行 = 灰度尚未完成;
	// 一个版本之后 unfenced 将改为丢弃。
	void LogFenceUnfencedOnce(const char *where)
	{
		static bool logged = false;
		if (logged)
		{
			return;
		}
		logged = true;
		LOG_INFO << where << ": received a push without target_player_id (legacy sender). "
				 << "Identity fence is inactive for such messages; this is logged once. "
				 << "See docs/design/routing-identity-audit-20260908.md R13.";
	}
} // namespace

///<<< END WRITING YOUR CODE

void GateHandler::PlayerEnterGameNode(::google::protobuf::RpcController* controller, const ::RegisterGameNodeSessionRequest* request,
	::RegisterGameNodeSessionResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	auto sessionIt = tlsSessionManager.sessions().find(request->session_info().session_id());
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LOG_ERROR << "Session ID not found for PlayerEnterGs, session ID: " << request->session_info().session_id();
		return;
	}
	// Resolve protocol node_id -> local registry entity before binding the session.
	if (const auto sceneNodeEntity = NodeUtils::FindNodeEntityByNodeId(SceneNodeService, request->scene_node_id()); sceneNodeEntity)
	{
		sessionIt->second.SetEntityId(SceneNodeService, entt::to_integral(*sceneNodeEntity));
	}
	else
	{
		LOG_ERROR << "PlayerEnterGs: scene node not found in registry, session_id="
				  << request->session_info().session_id()
				  << ", scene_node_id=" << request->scene_node_id();
	}
	response->mutable_session_info()->CopyFrom(request->session_info());
	LOG_INFO << "Player entered GS, session ID: " << request->session_info().session_id()
		<< ", game node ID: " << request->scene_node_id();
	///<<< END WRITING YOUR CODE
}

void GateHandler::SendMessageToPlayer(::google::protobuf::RpcController* controller, const ::NodeRouteMessageRequest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE

	auto sessionIt = tlsSessionManager.sessions().find(request->header().session_id());
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		// Expected during disconnect race: scene pushed a message (e.g. NotifyEnterScene)
		// after the player's TCP session was already closed on the gate. No state corruption.
		// LOG_DEBUG (level-guarded): under churn this fires millions of times and a
		// per-message WARN flooded the gate log to ~2GB, stealing IO-thread CPU.
		LOG_DEBUG << "Connection ID not found for PlayerMessage, session ID: " << request->header().session_id() << ", message ID:" << request->message_content().message_id();
		return;
	}
	// conn 为空即空指针解引用。BroadcastToScene/BroadcastToAll 一直有这个判定,
	// 这几条推送路径漏了 —— 统一补齐。
	if (!sessionIt->second.conn)
	{
		LOG_ERROR << "SendMessageToPlayer: session has no connection, session_id="
				  << request->header().session_id();
		return;
	}
	// 身份栅栏(R13/R14):写 socket 之前确认这条推送的目标玩家就是本会话当前的主人。
	switch (gate_session_fence::ClassifyPush(sessionIt->second.playerId,
											 request->header().target_player_id()))
	{
	case gate_session_fence::PushVerdict::kDrop:
		LogFenceMismatch("SendMessageToPlayer", request->header().session_id(),
						 sessionIt->second.playerId, request->header().target_player_id(),
						 request->message_content().message_id());
		return;
	case gate_session_fence::PushVerdict::kDeliverUnfenced:
		LogFenceUnfencedOnce("SendMessageToPlayer");
		break;
	case gate_session_fence::PushVerdict::kDeliver:
		break;
	}
	GetGateCodec().send(sessionIt->second.conn, request->message_content());
	///<<< END WRITING YOUR CODE
}

void GateHandler::RouteNodeMessage(::google::protobuf::RpcController* controller, const ::RouteMessageRequest* request,
	::RouteMessageResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	///<<< END WRITING YOUR CODE
}

void GateHandler::RoutePlayerMessage(::google::protobuf::RpcController* controller, const ::RoutePlayerMessageRequest* request,
	::RoutePlayerMessageResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	if (!request || !response) {
		return;
	}

	response->set_body(request->body());
	response->mutable_player_info()->CopyFrom(request->player_info());
	response->mutable_node_list()->CopyFrom(request->node_list());

	const Guid playerId = request->player_info().player_id();
	if (request->node_list_size() == 0) {
		auto targetSessionIt = tlsSessionManager.sessions().end();
		for (auto it = tlsSessionManager.sessions().begin(); it != tlsSessionManager.sessions().end(); ++it) {
			if (it->second.playerId == playerId) {
				targetSessionIt = it;
				break;
			}
		}

		if (targetSessionIt == tlsSessionManager.sessions().end()) {
			LOG_WARN << "RoutePlayerMessage: target player session not found on gate, player_id=" << playerId;
			return;
		}

		ClientRequest routedClientRequest;
		if (!routedClientRequest.ParseFromString(request->body())) {
			LOG_ERROR << "RoutePlayerMessage: failed to parse ClientRequest body, player_id=" << playerId;
			return;
		}

		MessageContent outbound;
		outbound.set_id(routedClientRequest.id());
		outbound.set_message_id(routedClientRequest.message_id());
		outbound.set_serialized_message(routedClientRequest.body());
		GetGateCodec().send(targetSessionIt->second.conn, outbound);
		return;
	}

	RoutePlayerMessageRequest nextRequest(*request);
	nextRequest.mutable_node_list()->DeleteSubrange(0, 1);

	const auto& nextNode = request->node_list(0);
	// node_id 是**业务节点号**,不是 entt 实体整数。uuid 主键重构之后两者不再相等,
	// 直接 `entt::entity{node_id}` 要么撞不上任何有效槽位(消息静默丢失),要么
	// 更糟 —— 撞上一个恰好有效但完全无关的节点实体,把玩家消息路由到错误的节点。
	// 全仓其他地方都走 FindNodeEntityByNodeId,这里是唯一漏网的裸转换。
	const auto nextNodeEntityOpt = NodeUtils::FindNodeEntityByNodeId(nextNode.node_type(), nextNode.node_id());
	if (!nextNodeEntityOpt) {
		LOG_ERROR << "RoutePlayerMessage: next node not found, node_type=" << nextNode.node_type()
				  << ", node_id=" << nextNode.node_id() << ", player_id=" << playerId;
		return;
	}
	const entt::entity nextNodeEntity = *nextNodeEntityOpt;
	auto& nextRegistry = tlsNodeContextManager.GetRegistry(nextNode.node_type());

	const auto *nextClient = nextRegistry.try_get<RpcClientPtr>(nextNodeEntity);
	if (!nextClient || !*nextClient) {
		LOG_ERROR << "RoutePlayerMessage: next node RpcClient missing, node_type=" << nextNode.node_type()
				  << ", node_id=" << nextNode.node_id() << ", player_id=" << playerId;
		return;
	}

	switch (nextNode.node_type()) {
	case eNodeType::SceneNodeService:
		(*nextClient)->CallRemoteMethod(SceneRoutePlayerStringMsgMessageId, nextRequest);
		return;
	case eNodeType::GateNodeService:
		(*nextClient)->CallRemoteMethod(GateRoutePlayerMessageMessageId, nextRequest);
		return;
	default:
		LOG_ERROR << "RoutePlayerMessage: unsupported next node_type=" << nextNode.node_type()
				  << ", node_id=" << nextNode.node_id() << ", player_id=" << playerId;
		return;
	}
	///<<< END WRITING YOUR CODE
}

void GateHandler::BroadcastToPlayers(::google::protobuf::RpcController* controller, const ::BroadcastToPlayersRequest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	// (session_id, player_id) 的解码与顺序契约收在 broadcast_target_codec.h,
	// 与 scene 侧的编码共用同一份实现 —— 两边各写一遍必然某天错开一格(R13)。
	auto sendToSession = [&](uint32_t sessionId, uint64_t targetPlayerId)
	{
		auto sessionIt = tlsSessionManager.sessions().find(sessionId);
		if (sessionIt == tlsSessionManager.sessions().end())
		{
			// LOG_DEBUG (level-guarded): AOI broadcasts to a session torn down in the
			// same frame are expected churn; this fired ~9.6M times and dominated the
			// ~2GB gate log, stealing IO-thread CPU that delays real pushes.
			LOG_DEBUG << "Connection ID not found for BroadCast2PlayerMessage, session ID: " << sessionId << ", message ID:" << request->message_content().message_id();
			return;
		}
		if (!sessionIt->second.conn)
		{
			return;
		}
		// 身份栅栏(R13):广播和单播用同一条判据 —— 会话易主后这一条必须丢。
		switch (gate_session_fence::ClassifyPush(sessionIt->second.playerId, targetPlayerId))
		{
		case gate_session_fence::PushVerdict::kDrop:
			LogFenceMismatch("BroadcastToPlayers", sessionId, sessionIt->second.playerId,
							 targetPlayerId, request->message_content().message_id());
			return;
		case gate_session_fence::PushVerdict::kDeliverUnfenced:
			LogFenceUnfencedOnce("BroadcastToPlayers");
			break;
		case gate_session_fence::PushVerdict::kDeliver:
			break;
		}
		GetGateCodec().send(sessionIt->second.conn, request->message_content());
	};

	broadcast_targets::ForEach(*request, sendToSession);
	///<<< END WRITING YOUR CODE
}

void GateHandler::BroadcastToScene(::google::protobuf::RpcController* controller, const ::BroadcastToSceneRequest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	const uint64_t sceneId = request->scene_id();
	for (auto &[sessionId, info] : tlsSessionManager.sessions())
	{
		if (info.sceneId == sceneId && info.conn)
		{
			GetGateCodec().send(info.conn, request->message_content());
		}
	}
	///<<< END WRITING YOUR CODE
}

void GateHandler::BroadcastToAll(::google::protobuf::RpcController* controller, const ::BroadcastToAllRequest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	for (auto &[sessionId, info] : tlsSessionManager.sessions())
	{
		if (info.conn)
		{
			GetGateCodec().send(info.conn, request->message_content());
		}
	}
	///<<< END WRITING YOUR CODE
}

void GateHandler::NodeHandshake(::google::protobuf::RpcController* controller, const ::NodeHandshakeRequest* request,
	::NodeHandshakeResponse* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE
	gNode->GetNodeRegistrationManager().OnNodeHandshake(*request, *response);
///<<< END WRITING YOUR CODE
}

void GateHandler::BindSessionToGate(::google::protobuf::RpcController* controller, const ::BindSessionToGateRequest* request,
	::BindSessionToGateResponse* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE
	// 只能**就地更新**已有会话,绝不能整体赋值。
	//
	// 旧实现 `sessions()[id] = SessionInfo{...}` 会把在线会话连同 conn(TCP 连接)、
	// verified、entityIds(scene 绑定)、sceneId 一起覆盖成默认值:
	//   - conn 变成空 shared_ptr,而 SendMessageToPlayer / BroadcastToPlayers /
	//     PushToPlayerEventHandler 都不检查 conn 就 `conn->send()` —— 直接空指针解引用;
	//   - verified 归 false,该客户端后续所有消息被判未验证并 shutdown;
	//   - scene 绑定丢失,玩家消息再也路由不到场景节点。
	// 会话不存在时也不能凭空建一条:没有 conn 的会话永远不会被 TCP 断开回调清掉,
	// 只会永久泄漏。正确形态与 Kafka 侧的 BindSessionEventHandler 保持一致。
	auto &sessions = tlsSessionManager.sessions();
	const auto sessionIt = sessions.find(request->session_id());
	if (sessionIt == sessions.end())
	{
		LOG_ERROR << "BindSessionToGate: session not found (client already disconnected?), session_id="
				  << request->session_id() << " player_id=" << request->player_id();
		return;
	}

	sessionIt->second.playerId = request->player_id();
	sessionIt->second.sessionVersion = request->session_version();

	response->set_session_id(request->session_id());
	response->set_session_version(request->session_version());
	response->set_player_id(request->player_id());
///<<< END WRITING YOUR CODE
}

void GateHandler::GmGracefulShutdown(::google::protobuf::RpcController* controller, const ::GmGracefulShutdownRequest* request,
	::GmGracefulShutdownResponse* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE

	// GM 面必须鉴权,这条 RPC 尤其如此:它会把本 gate 上**所有**在线会话
	// forceClose 再排队停机。旧实现收到请求就干活,唯一的"身份"是请求体里
	// 自报的 operator 明文字符串 —— 任何能连到本节点 RPC 端口的进程发一条
	// GmGracefulShutdown 就能停服,无凭据、无审计、不可否认性为零。
	//
	// 为什么签名寄生在 operator 字段而不是新增 proto 字段:
	// GmGracefulShutdownRequest 只有 operator / reason 两个字段
	// (proto/common/base/gm_admin.proto),而这条 RPC 走的是 muduo GameChannel
	// 通道 —— CallMethod 的 controller 与 done 都恒为 nullptr
	// (game_channel.cpp:451),**没有任何 metadata 边信道**可用;改 proto 则要
	// 连带重生成 C++/Go/Java 三侧产物,不在本次可验证范围内。信封格式与
	// canonical 串的完整定义见 gate_security.h。
	//
	// 密钥只从环境变量 GATE_GM_ADMIN_SECRET 读,未配置时一律拒绝(fail-closed),
	// 不设"开发模式免签"的口子:开发环境要停 gate,Ctrl+C / SIGTERM 就够了。
	//
	// 签名绑本节点 node_id:nonce 去重表是进程内的,不绑就意味着抓到一条合法
	// 停机请求后可以在时间窗内挨个重放给全区每一个 gate —— 一次抓包换全服掉线。
	const std::string selfNodeId = std::to_string(gNode->GetNodeId());
	const auto gmAuth = gate_security::VerifyGmRequestFromEnv(
		"Gate.GmGracefulShutdown", selfNodeId, request->operator_(), request->reason());
	if (gmAuth.result != gate_security::GmAuthResult::kOk)
	{
		// 拒绝也要留痕:这是入侵检测的唯一信号源。原始 operator / reason 都是
		// 攻击者可控的输入(不含任何秘密),照原样打有助于事后比对来源 ——
		// 但必须截断:不截断的话对方发一条几 MB 的 operator 就能把 gate 的日志
		// 盘和 IO 线程当放大器用,而这条路径连鉴权都还没过。
		constexpr size_t kMaxLoggedFieldLength = 128;
		const std::string loggedOperator = request->operator_().substr(
			0, std::min<size_t>(request->operator_().size(), kMaxLoggedFieldLength));
		const std::string loggedReason = request->reason().substr(
			0, std::min<size_t>(request->reason().size(), kMaxLoggedFieldLength));
		LOG_ERROR << "GM graceful shutdown REJECTED, reason="
				  << gate_security::GmAuthResultName(gmAuth.result)
				  << " raw_operator=" << loggedOperator
				  << " request_reason=" << loggedReason;
		return;
	}

	LOG_INFO << "GM graceful shutdown requested by operator=" << gmAuth.operatorName
			 << " reason=" << request->reason();

	auto& sessions = tlsSessionManager.sessions();
	uint32_t count = 0;
	for (auto& [sessionId, info] : sessions)
	{
		if (info.conn)
		{
			info.conn->forceClose();
			++count;
		}
	}

	LOG_INFO << "GM graceful shutdown: closed " << count << " client sessions, scheduling node shutdown.";
	response->set_affected_count(count);

	// 应答由框架负责发送,这里**不能**碰 done。
	//
	// GameChannel::CallMethod 传进来的 done 恒为 nullptr(见 game_channel.cpp:451),
	// 应答是在 CallMethod 返回之后由框架序列化 response 再发出的。旧代码 `done->Run()`
	// 是确定性的空指针解引用:GM 一敲停机,进程当场崩在这一行 —— 上面刚把所有客户端
	// forceClose 了,而 gNode->Shutdown() 的租约注销与优雅收尾根本没机会跑,
	// "优雅停机"退化成硬杀。
	//
	// 停机延后到本次 RPC 应答发出之后:queueInLoop 排到当前事件循环回合末尾,
	// 此时 CallMethod 已返回、应答已写进发送缓冲。
	gNode->GetLoop()->queueInLoop([] { gNode->Shutdown(); });
	return;

///<<< END WRITING YOUR CODE
}
