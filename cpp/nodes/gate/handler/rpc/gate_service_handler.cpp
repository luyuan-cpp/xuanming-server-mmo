
#include "gate_service_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "muduo/base/Logging.h"

#include "gate_codec.h"
#include "gate_security.h"
#include "node/system/node/node.h"
#include "network/rpc_controller.h"

#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/base/common.pb.h"
#include "proto/common/base/node.pb.h"
#include "network/broadcast_target_codec.h"
#include "handler/event/scene_entry_dispatch.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "thread_context/node_context_manager.h"
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

	// 会话当前绑定的 scene 进程实例号(踢线来源栅栏用)。没绑定、实体已失效、注册不带 uuid 时返回空。
	std::string BoundSceneInstance(const SessionInfo &session)
	{
		const auto sceneNode = session.GetNodeEntity(common::base::SceneNodeService);
		auto &sceneNodeRegistry = tlsNodeContextManager.GetRegistry(common::base::SceneNodeService);
		if (sceneNode == entt::null || !sceneNodeRegistry.valid(sceneNode))
		{
			return {};
		}
		const auto *info = sceneNodeRegistry.try_get<NodeInfo>(sceneNode);
		return info != nullptr ? info->node_uuid() : std::string{};
	}

	// 本次 RPC 所在连接对应的 scene 进程实例号。scene 经 gate 拨过去的那条连接回推;只认在**当前连接**上握手核验过
	// 对端 uuid 的连接(SceneLinkHandshakeComp,scene_entry_dispatch.cpp OnSceneLinkHandshaken 写入):同地址换了进程、
	// 旧 RpcClient 重连到新进程而握手被拒时,不能拿注册表里的旧 uuid 认发送方。认不出返回空(按 kDeliverUnverified 照发)。
	// 只在踢线这条罕见消息上遍历,不进普通推送的热路径。
	std::string SenderSceneInstance(const muduo::net::TcpConnectionPtr &conn)
	{
		if (!conn)
		{
			return {};
		}
		auto &sceneNodeRegistry = tlsNodeContextManager.GetRegistry(common::base::SceneNodeService);
		for (const auto &[entity, handshake, info] :
			 sceneNodeRegistry.view<gate_scene_entry::SceneLinkHandshakeComp, NodeInfo>().each())
		{
			// lock() 的强引用只活在本次比较内,不留存连接(AGENTS §11.7)。
			const auto linked = handshake.conn.lock();
			if (linked && linked.get() == conn.get())
			{
				return info.node_uuid();
			}
		}
		return {};
	}
} // namespace

///<<< END WRITING YOUR CODE

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
	// 会话不持有连接；发送前锁定，连接已释放时丢弃本次推送。
	auto conn = sessionIt->second.conn.lock();
	if (!conn)
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
	// 踢线来源栅栏:34 只认会话当前绑定的 scene 进程(session_identity_fence.h ClassifySceneKickSource)。
	// 同 zone 交接其实已放行、会话已改绑到目标节点时,源 scene 收口推来的 34 会断掉这条合法会话。
	if (request->message_content().message_id() == SceneClientPlayerCommonKickPlayerMessageId)
	{
		const auto boundScene = BoundSceneInstance(sessionIt->second);
		const auto senderScene = SenderSceneInstance(RpcController::From(controller).conn());
		if (gate_session_fence::ClassifySceneKickSource(boundScene, senderScene) ==
			gate_session_fence::SceneKickSourceVerdict::kDropNotBoundScene)
		{
			LOG_WARN << "SendMessageToPlayer: dropping KickPlayer from a scene process the session is no longer bound to"
					 << " (the session was re-routed, e.g. a same-zone handoff that was admitted although the source"
					 << " missed the reply). session_id=" << request->header().session_id()
					 << " player_id=" << sessionIt->second.playerId << " sender_scene=" << senderScene
					 << " bound_scene=" << boundScene;
			return;
		}
	}
	GetGateCodec().send(conn, request->message_content());
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
		auto conn = sessionIt->second.conn.lock();
		if (!conn)
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
		GetGateCodec().send(conn, request->message_content());
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
		if (info.sceneId != sceneId)
		{
			continue;
		}
		if (const auto conn = info.conn.lock())
		{
			GetGateCodec().send(conn, request->message_content());
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
		if (const auto conn = info.conn.lock())
		{
			GetGateCodec().send(conn, request->message_content());
		}
	}
	///<<< END WRITING YOUR CODE

}

void GateHandler::NodeHandshake(::google::protobuf::RpcController* controller, const ::NodeHandshakeRequest* request,
	::NodeHandshakeResponse* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE
	gNode->GetNodeRegistrationManager().OnNodeHandshake(RpcController::From(controller), *request, *response);
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
	// (proto/common/base/gm_admin.proto),而这条 RPC 走的是节点间 muduo RPC0 通道
	// (RpcChannel)—— 框架传入的 controller 是本次派发的 RpcController(只携带所在
	// 连接,**不是 metadata 边信道**,见 network/rpc_controller.h);done 是本次派发的
	// 完成句柄(非空,可经 RpcDoneCallback::Adopt 接管),同样不携带调用方身份;改 proto 则要
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
		// 被拒以 RPC_ERROR(HANDLER_FAILED) 回给调用方(调用方得到 kRemoteFailed),与「鉴权通过但没有可关的会话」
		// (affected_count = 0 的正常应答)区分开。线上只带错误码,不回拒绝原因(不给探测者提示),原因只在本地日志。
		controller->SetFailed("GM graceful shutdown rejected");
		return;
	}

	LOG_INFO << "GM graceful shutdown requested by operator=" << gmAuth.operatorName
			 << " reason=" << request->reason();

	auto& sessions = tlsSessionManager.sessions();
	uint32_t count = 0;
	for (auto& [sessionId, info] : sessions)
	{
		if (const auto conn = info.conn.lock())
		{
			conn->forceClose();
			++count;
		}
	}

	LOG_INFO << "GM graceful shutdown: closed " << count << " client sessions, scheduling node shutdown.";
	response->set_affected_count(count);

	// 应答由框架负责发送:本 handler 不调 done->Run(),也不接管 done(不调 RpcDoneCallback::Adopt)。
	//
	// RpcChannel::CallServiceMethod 在 handler 同步返回之后立即序列化 response 并回包
	// (调用方带非 0 调用 id 时必回,affected_count 为 0 也回;id 为 0 是单向调用,永不回包)。
	// 下面的 queueInLoop 依赖这条时序:回包先于 Shutdown。若改成 Adopt 延后完成,
	// 回包就可能落在 Shutdown 之后,调用方只能等到超时或断连。
	//
	// 历史:旧框架传进来的 done 恒为 nullptr,旧代码 `done->Run()` 是确定性的空指针解引用 ——
	// GM 一敲停机,进程当场崩在这一行,上面刚把所有客户端 forceClose 了,而 gNode->Shutdown()
	// 的租约注销与优雅收尾根本没机会跑,"优雅停机"退化成硬杀。
	//
	// 停机延后到本次 RPC 应答发出之后:queueInLoop 排到当前事件循环回合末尾,
	// 此时 handler 已返回、应答已写进发送缓冲。
	gNode->GetLoop()->queueInLoop([] { gNode->Shutdown(); });
	return;

///<<< END WRITING YOUR CODE

}
