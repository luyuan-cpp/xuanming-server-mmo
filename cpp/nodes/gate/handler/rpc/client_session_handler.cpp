#include "client_session_handler.h"
#include "client_request_lane.h"
#include "login_admission.h"
#include "rpc_reply_bridge.h"
#include "scene_client_call_reply.h"

#include <algorithm>
#include <cassert>
#include <chrono>
#include <functional>
#include <memory>
#include <unordered_map>
#include <optional>
#include <sstream>
#include <string_view>

#include "gate_codec.h"
#include "gate_gm_client_messages.h"
#include "gate_router_mode.h"
#include "gate_security.h"
#include "message_limiter/illegal_packet_counter.h"
#include "error_reporter/error_reporter.h"
#include "node/system/node/node.h"
#include "grpc_client/login/login_grpc_client.h"
#include "handler/rpc/router_muduo_forward.h"
#include "handler/event/scene_entry_dispatch.h"
#include "spectate/gate_spectate_client.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "proto/client_rpc_router/client_rpc_router.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/login_error_tip.pb.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "proto/common/base/empty.pb.h"
#include "proto/scene/scene.pb.h"
#include "rpc/service_metadata/player_lifecycle_service_metadata.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "proto/common/base/node.pb.h"
#include "node/system/node/node_util.h"
#include "node/system/node/node_events.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include <session/manager/session_manager.h>
#include <network/node_utils.h>
#include "core/network/grpc_client_runtime.h"
#include <node_config_manager.h>

// BytesToHex / HmacSha256Hex 原来是本文件匿名 namespace 里的两个静态函数,
// 已经提到 gate_security.h —— GM 面鉴权(gate_service_handler.cpp)要复用同一份
// 实现,两份 HMAC 拼装代码迟早会漂移。

static void LogClientSecurityRejectionSampled(const char *reason, SessionId sessionId)
{
	// reason 来自进程内常量,不包含攻击者输入。所有计数都在 gate 单 EventLoop
	// 线程更新;每 1024 次留一条趋势证据,不能按公网帧量逐条写盘。
	static uint64_t rejectedCount = 0;
	if ((rejectedCount++ & 0x3FF) == 0)
	{
		LOG_WARN << "Unauthenticated client traffic rejected (sampled), reason=" << reason
				 << ", latest_session_id=" << sessionId
				 << ", rejected_total=" << rejectedCount;
	}
}

// gate → scene 转发客户端请求类消息(内层非 Empty)的超时。不变式 I5(与 client-request-ordering.md §5 的 I4 同一条预算链):
//   scene 侧下游最大 deadline(当前只有 SceneManager gRPC,10000;scene 不调 login)+ 1000 ≤ 本值 ≤ 客户端请求超时(15000)− 1000。
// 今天 scene 的玩家 handler 都是同步完成的,超时只兜「scene 卡住 / 回包丢失」;取上限附近是为了以后玩家 handler
// 延后完成(等下游)时不必再改。超时 = 结果未知:回带请求 id 的 1003,不重发。
constexpr std::chrono::milliseconds kGateToSceneClientCallTimeout{12000};

// scene 对一条客户端消息的回包 / 失败落到客户端(docs/design/muduo-rpc-channel.md §8.1)。
// 会话按 (session_id, 会话代次) 复核:session_id 被新连接复用之后,旧会话迟到的回包不得写进新会话的 socket。
// 只由 HandleTcpNodeMessage 为内层响应非 Empty 的请求-响应类消息登记,RPC0 保证每次调用恰好回调一次,
// 所以下面三种处置(原样下发 / 1003 信封 / 不下发)对同一请求恰好发生一次,判定见 scene_client_call_reply.h。
static void ForwardSceneClientCallResult(SessionId sessionId, uint64_t incarnation, uint64_t clientRequestId,
										 uint32_t innerMessageId, const RpcStatus &status,
										 const std::shared_ptr<ProcessClientPlayerMessageResponse> &response)
{
	SessionInfo *session = gate_lane::FindLiveSession(sessionId, incarnation);
	if (session == nullptr)
	{
		return; // 会话已关闭或号已复用:没有客户端可回(断线后的回包本来就无处可去)
	}
	using gate_scene_client_reply::Disposition;
	const Disposition disposition = gate_scene_client_reply::Decide(status, response.get(), clientRequestId);
	if (disposition == Disposition::kForwardContent)
	{
		if (const auto clientConn = session->conn.lock())
		{
			GetGateCodec().send(clientConn, response->message_content());
		}
		return;
	}
	if (!status.ok())
	{
		// 确定未执行(没发出去 / 对端派发前拒绝)或结果未知(超时 / 断连 / 关机):都回带请求 id 的 1003,
		// 客户端不必干等自己的 15s 超时。结果未知时下游可能已执行,有副作用的请求靠业务侧栅栏与幂等兜底。
		static uint64_t failedCount = 0;
		if ((failedCount++ & 0x3FF) == 0)
		{
			LOG_WARN << "[TCP Node] scene call failed (sampled): status=" << status.name()
					 << ", remote_error=" << status.remoteError() << ", message_id=" << innerMessageId
					 << ", failed_total=" << failedCount;
		}
	}
	else
	{
		// scene 派发前早退、应答里没有 message_content(docs/design/client-rpc-callback.md §7 R1):请求确定未执行。
		// 请求号非 0 回带请求 id 的 1003,客户端不必干等 15s 超时;请求号为 0 没法关联,维持不下发。
		// 早退的具体原因与会话在 scene 自己的日志里,这里只留趋势(每 1024 次一条)。
		static uint64_t noContentCount = 0;
		if ((noContentCount++ & 0x3FF) == 0)
		{
			LOG_WARN << "[TCP Node] scene reply has no message content (sampled): message_id=" << innerMessageId
					 << ", envelope_sent=" << (disposition == Disposition::kServiceUnavailableEnvelope)
					 << ", no_content_total=" << noContentCount;
		}
	}
	if (disposition == Disposition::kServiceUnavailableEnvelope)
	{
		gate_lane::SendEnvelope(*session, clientRequestId, innerMessageId, kServiceUnavailable);
	}
}

// 直连调用按发出时的会话交桥,不依赖服务端是否回显会话 metadata。
static void ForwardDirectGrpcCallResult(SessionId sessionId, uint64_t incarnation,
                                 const gate_rpc_reply_bridge::DirectClientCall &clientCall,
                                 const std::string &serializedSessionDetails, const GrpcCallOutcome &outcome,
                                 const google::protobuf::Message *reply)
{
	// session_id 复用后旧完成不得进入新连接;成功和失败都按发出时的代次认领。
	if (gate_lane::FindLiveSession(sessionId, incarnation) == nullptr)
	{
		return;
	}
	if (!outcome.ok() || reply == nullptr)
	{
		const auto &rpcMethodMetadata = gRpcMethodRegistry[clientCall.messageId];
		const std::string rpcMethodName = std::string(rpcMethodMetadata.serviceName) + "." + rpcMethodMetadata.methodName;
		gate_rpc_reply_bridge::NotifyCallFailure(rpcMethodName.c_str(), static_cast<int>(outcome.code), outcome.message,
		                                        serializedSessionDetails.empty() ? nullptr : &serializedSessionDetails,
		                                        &clientCall);
		return;
	}
	SessionDetails requestSessionDetails;
	if (!requestSessionDetails.ParseFromString(serializedSessionDetails))
	{
		// 解析失败不能把默认的会话身份交给回包桥。
		LOG_ERROR << "Invalid direct gRPC reply session metadata: message_id=" << clientCall.messageId;
		return;
	}
	gate_rpc_reply_bridge::DeliverReply(requestSessionDetails, *reply, &clientCall);
}

static std::optional<entt::entity> PickRandomNode(uint32_t nodeType)
{
	std::vector<entt::entity> candidates;
	auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	auto view = registry.view<NodeInfo>();
	// 全局池类型(如 match)不比对 zone:任一 zone 的实例都能服务本 zone 玩家,
	// 某 zone 的实例全挂时自动落到其他 zone(设计文档 cross-zone-matchmaking.md D11)。
	const bool globalPool = NodeUtils::IsGlobalPoolNodeType(nodeType);
	const auto selfZoneId = GetNodeInfo().zone_id();
	for (auto entity : view)
	{
		if (!HasUsableGrpcClient(registry, entity))
		{
			continue;
		}
		const auto &node = view.get<NodeInfo>(entity);
		if (globalPool || node.zone_id() == selfZoneId)
		{
			candidates.push_back(entity);
		}
	}

	if (candidates.empty())
	{
		return std::nullopt;
	}

	// Pick a random candidate
	std::random_device rd;
	std::mt19937 gen(rd());
	std::uniform_int_distribution<> dis(0, candidates.size() - 1);
	return candidates[dis(gen)];
}

// SessionDetails 是 gate 递给 Go 服务的会话身份(gRPC metadata x-session-detail-bin,
// Go 侧 SessionInterceptor 据此识别玩家,并在响应头回写供 gate 找回会话)。
// 直连模式、路由模式的请求转发与断线通知都用这一份;playerId 传 0 表示"尚未绑定玩家"
// (proto3 隐式存在:0 与不设置在线上是同一形状)。
// session 非空时带上重定向票据的绑定信息(CZ-8)与会话代次。两种转发模式**必须**走同一个构造点:
// 曾经直连模式是内联手写的一份,只改其中一个 = 一种模式下 login 永远读到 0、访客被弹回家,
// 而生产恰恰是路由模式。断线通知不需要票据字段,传 nullptr,会话代次由调用方显式写入。
static SessionDetails BuildSessionDetails(const SessionId sessionId, const Guid playerId,
										  const SessionInfo *session = nullptr)
{
	SessionDetails sessionDetails;
	sessionDetails.set_session_id(sessionId);
	sessionDetails.set_player_id(playerId);
	sessionDetails.set_gate_node_id(gNode->GetNodeId());
	sessionDetails.set_gate_instance_id(gNode->GetNodeInfo().node_uuid());
	if (session != nullptr)
	{
		sessionDetails.set_ticket_player_id(session->ticketPlayerId);
		sessionDetails.set_ticket_target_zone_id(session->ticketTargetZoneId);
		// 会话代次(client-request-ordering.md §4.8):login 按 (gate_instance_id, 会话代次) 认登录态与会话栅栏。
		// 原先只有路由 lane 在 Admit 里写(同一个值),直连模式发出的 Login / CreatePlayer / EnterGame 带 0,
		// 落进 login 的旧版兼容路径、仍按 session_id 认登录态,gate 重启撞号即可继承别人的登录态(审计 §2.3)。
		sessionDetails.set_session_incarnation(session->incarnation);
		sessionDetails.set_owner_revision(session->ownerRevision);
	}
	return sessionDetails;
}

// 路由模式专用:把一条 ClientRequest **原包**交给路由服(client-rpc-router.md D30
// 不透明转发)。不解析 body、不挑业务实例 —— 目标由路由服按生成的路由表和
// zone_id 选;会话身份仍走同一段 metadata,Go 侧拦截器看到的与 gate 直连时完全相同。
//
// sessionDetailsBin 是序列化好的 SessionDetails,原样作为 x-session-detail-bin。
// 返回是否已发出;本地尚未发现任何可用的路由服实例(启动窗口 / 路由服全部下线 / 缺通道)返回 false。
// 调用方决定后果:路由 lane 回客户端带请求 id 的 kServiceUnavailable;断线通知没有客户端可回,只留痕。
// 路由 lane 经 gate_lane::SetRouterSender 注入本函数(client_request_lane.h)。
static bool SendRawViaRouter(const ClientRequest &request, const std::string &sessionDetailsBin)
{
	const auto routerNode = PickRandomNode(eNodeType::ClientRpcRouterNodeService);
	if (!routerNode)
	{
		// 路由服全部下线时这里的频率 = 全部在线玩家的 gRPC 类消息频率,必须采样;
		// 每 1024 条留一行趋势证据,配合客户端收到的 kServiceUnavailable 定性。
		static uint64_t unavailableCount = 0;
		if ((unavailableCount++ & 0x3FF) == 0)
		{
			LOG_WARN << "No available router instance;message not forwarded(sampled), latest_message_id=" << request.message_id()
					 << ", unavailable_total=" << unavailableCount;
		}
		return false;
	}

	client_rpc_router::ForwardRequest forwardRequest;
	forwardRequest.mutable_request()->CopyFrom(request);
	// zone-scoped 目标(login)由路由服按这个 zone 挑同 zone 实例,与直连模式下
	// PickRandomNode 的 zone 过滤语义一致(D33)。
	forwardRequest.set_zone_id(gNode->GetNodeInfo().zone_id());

	// 传输只有一种:路由服节点实体上的 muduo_grpc 通道(NodeConnector::ConnectToGrpcNode 建,
	// docs/design/grpc-muduo-transport.md §14)。一次调用只发一次,失败不重发。
	// 缺通道按「没有路由服」处理(fail-closed),绝不对缺失的组件解引用。关机阶段 0(FinalizeShutdownInLoop)清掉
	// MuduoGrpcChannelPtr、NodeInfo 要到拆除才清,这段窗口(排空预算到期后强行 Finalize、Finalize 之后才断开的会话)
	// 靠 PickRandomNode 的候选条件(HasUsableGrpcClient)挡住,正常关机另有排空门(main.cpp 的 SetShutdownDrainComplete:
	// beforeShutdown 记下的会话都走完断线处理、通道都空闲才 Finalize)。这里再挡一层,防选点条件日后改松时落成空指针解引用。
	// 返回 false 后 lane 回带 id 的 1003,断线通知只留痕、由 login 租约兜底。
	auto &routerRegistry = tlsNodeContextManager.GetRegistry(eNodeType::ClientRpcRouterNodeService);
	const auto *routerChannel = routerRegistry.try_get<MuduoGrpcChannelPtr>(*routerNode);
	if (routerChannel == nullptr || !*routerChannel)
	{
		static uint64_t noTransportCount = 0;
		if ((noTransportCount++ & 0x3FF) == 0)
		{
			LOG_WARN << "Router instance has no gRPC channel;message not forwarded(sampled)"
					 << ", router_entity=" << entt::to_integral(*routerNode)
					 << ", latest_message_id=" << request.message_id() << ", no_transport_total=" << noTransportCount;
		}
		return false;
	}
	gate_router_muduo::SendForward(**routerChannel, forwardRequest, sessionDetailsBin);
	return true;
}

// 不经路由 lane 的发送(gate 合成的断线通知):SessionDetails 的 gate_call_seq 留 0,回包不归 lane 认领。
static bool SendViaRouter(const ClientRequest &request, const SessionDetails &sessionDetails)
{
	return SendRawViaRouter(request, sessionDetails.SerializeAsString());
}

GateClientSessionHandler::GateClientSessionHandler(ProtobufCodec &codec,
												 ProtobufDispatcher &dispatcherParam)
	: protobufCodec(codec),
	  messageDispatcher(dispatcherParam)
{
	messageDispatcher.registerMessageCallback<ClientRequest>(
		std::bind(&GateClientSessionHandler::DispatchClientRpcMessage, this, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));

	// 路由 lane 的发送出口(client_request_lane.h)。函数指针,不捕获任何对象(AGENTS.md §11.7)。
	gate_lane::SetRouterSender(&SendRawViaRouter);

	messageDispatcher.registerMessageCallback<ClientTokenVerifyRequest>(
		std::bind(&GateClientSessionHandler::DispatchTokenVerify, this, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));

	nodeRemovedSubscription_ = tlsEcs.dispatcher.sink<OnNodeRemoveEvent>().connect<&GateClientSessionHandler::OnNodeRemoveEventHandler>(*this);
}

// Scene node binding must be set explicitly by scene manager (e.g. EnterScene).
// Do NOT pick randomly -- a random scene node has no player entity.
std::optional<entt::entity> ResolveSessionTargetNode(SessionId sessionId, uint32_t nodeType)
{
	const auto sessionIt = tlsSessionManager.sessions().find(sessionId);
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LOG_ERROR << "Session not found for session id: " << sessionId;
		return std::nullopt;
	}

	auto &session = sessionIt->second;

	if (!session.HasNodeEntity(nodeType))
	{
		LOG_ERROR << "No node bound for nodeType: " << nodeType << ", session id: " << sessionId;
		return std::nullopt;
	}

	// 已销毁的节点必须等注册事件重新绑定，不能用旧 index 猜测继任节点。
	const auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	const auto node = session.GetNodeEntity(nodeType);
	if (!registry.valid(node))
	{
		LOG_ERROR << "Bound node is invalid. nodeType: " << nodeType
				  << ", session id: " << sessionId << ", stored_entity=" << entt::to_integral(node);
		session.SetNodeEntity(nodeType, entt::null);
		return std::nullopt;
	}
	return node;
}

void GateClientSessionHandler::OnConnection(const muduo::net::TcpConnectionPtr &conn)
{
	// Token verification is handled by DispatchTokenVerify (ClientTokenVerifyRequest).
	// Until verified, DispatchClientRpcMessage rejects all ClientRequest messages.
	if (conn->connected())
	{
		HandleConnectionEstablished(conn);
	}
	else
	{
		HandleConnectionDisconnection(conn);
	}
}

void GateClientSessionHandler::SendMessageToClient(const muduo::net::TcpConnectionPtr &conn, const ::google::protobuf::Message &message) const
{
	protobufCodec.send(conn, message);
}

SessionId GateClientSessionHandler::GetSessionId(const muduo::net::TcpConnectionPtr &conn)
{
	try
	{
		return boost::any_cast<SessionId>(conn->getContext());
	}
	catch (const boost::bad_any_cast &e)
	{
		LOG_ERROR << "Failed to cast session ID from connection context: " << e.what();
		return kInvalidSessionId;
	}
}

void GateClientSessionHandler::SendTipToClient(const muduo::net::TcpConnectionPtr &conn, uint32_t tipId)
{
	TipInfoMessage tipMessage;
	tipMessage.set_id(tipId);
	MessageContent message;
	message.set_serialized_message(tipMessage.SerializeAsString());
	message.set_message_id(SceneClientPlayerCommonSendTipToClientMessageId);
	GetGateCodec().send(conn, message);

	LOG_TRACE << "Sent tip message to session id: " << GetSessionId(conn) << ", tip id: " << tipId;
}

bool GateClientSessionHandler::CheckMessageSize(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const
{
	constexpr size_t kMaxClientMessageSize = 1024;
	if (request->ByteSizeLong() > kMaxClientMessageSize)
	{
		LOG_WARN << "Message size exceeds 1KB. Message ID: " << request->message_id()
				 << ", player_id: " << session.playerId;
		MessageContent errResponse;
		errResponse.set_id(request->id());
		errResponse.set_message_id(request->message_id());
		errResponse.mutable_error_message()->set_id(kMessageSizeExceeded);
		// 必须走 codec(长度头+类型名+校验和)。裸 conn->send 序列化字节会以
		// 无帧形式插进客户端的解析流:客户端把 protobuf 字段字节当长度头读,
		// 之后整条连接的分帧全部错位 —— 一次限流/超限应答就毁掉整条连接。
		protobufCodec.send(conn, errResponse);

		// 超限包同样计入非法包闸门。旧写法只回错误不计数,而且这一检查排在
		// CheckMessageLimit 之前 —— 于是超长包既不占限流额度、也永远触发不了
		// 踢人阈值:客户端可以无限发 1KB+ 的包,gate 每包都要序列化一次应答、
		// 打一条 ERROR 级日志,CPU 与磁盘被白白吃掉,连接永不关闭。
		if (IllegalPacketCounter::RegisterAndShouldKill(session.illegalPacketCount))
		{
			LOG_WARN << "Session illegal-packet threshold exceeded (oversized) — forceClose."
					 << " count=" << session.illegalPacketCount
					 << " message_id=" << request->message_id();
			conn->forceClose();
		}
		return false;
	}
	return true;
}

bool GateClientSessionHandler::CheckMessageLimit(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const
{
	if (const auto err = session.messageLimiter.CanSend(request->message_id()); err != kSuccess)
	{
		LOG_ERROR << "Failed to send message. Message ID: " << request->message_id() << ", Error: " << err;
		MessageContent errResponse;
		errResponse.set_id(request->id());
		errResponse.set_message_id(request->message_id());
		errResponse.mutable_error_message()->set_id(err);
		// 同 CheckMessageSize:必须带帧,裸 send 会让客户端流错位。
		protobufCodec.send(conn, errResponse);

		// todo.md #236: count this rejection toward the per-session illegal-
		// packet kill switch. A misbehaving / hostile client that keeps
		// hammering past the rate limit gets dropped at the threshold
		// (GATE_ILLEGAL_PACKET_THRESHOLD env, default 50) so we stop
		// burning CPU sending error replies it ignores.
		const bool shouldKill = IllegalPacketCounter::RegisterAndShouldKill(session.illegalPacketCount);

		// todo.md #250 slice A — record into process-wide buffer. Carries
		// the running illegal-packet count so an attack pattern shows up
		// as a step function in the buffer dump, not just per-event noise.
		{
			std::ostringstream m;
			m << "message_id=" << request->message_id()
			  << " illegal_count=" << session.illegalPacketCount
			  << " player_id=" << session.playerId;
			error_reporter::Record(err, "illegal_packet", m.str());
		}

		if (shouldKill)
		{
			LOG_WARN << "Session illegal-packet threshold exceeded — forceClose."
					 << " count=" << session.illegalPacketCount
					 << " message_id=" << request->message_id();
			conn->forceClose();
		}
		return false;
	}
	return true;
}

// 返回 false 表示本次请求体不可用,调用方**必须**放弃转发。
//
// message 是 gRpcMethodRegistry 里按 message_id 共享的**进程级单例原型**,
// 上一次任何玩家的同类请求解析结果都还留在里面。因此:
//   - 必须先 Clear() 再解析 —— 旧实现空包直接 return,共享原型里残留的
//     上一个玩家的参数会被原样转发到后端(挂着当前会话的 player_id)。
//     攻击者故意发空 body 就能重放别人的请求参数;
//   - 解析失败必须让调用方知道 —— 旧实现返回 void,调用方拿着
//     Clear 后部分填充(攻击者可控前缀)的消息照样转发。
template <typename Message, typename Request>
bool ParseMessageFromRequestBody(Message &message, const Request &request, const SessionId sessionId)
{
	message.Clear();

	const std::string &requestBody = request->body();
	if (requestBody.empty())
	{
		// 空体是合法的(无参 RPC),Clear 之后就是干净的默认消息。
		return true;
	}

	if (!message.ParseFromString(requestBody))
	{
		LOG_ERROR << "Failed to parse client message body for session id: " << sessionId;
		message.Clear();
		return false;
	}
	return true;
}

bool GateClientSessionHandler::NotifySceneExitGame(const SessionId sessionId, const SessionInfo &session,
												  uint32_t &sceneNodeId)
{
	sceneNodeId = 0;
	if (!session.HasNodeEntity(SceneNodeService))
	{
		return false;
	}
	const auto sceneNode = session.GetNodeEntity(SceneNodeService);
	auto &sceneNodeRegistry = tlsNodeContextManager.GetRegistry(SceneNodeService);
	if (!sceneNodeRegistry.valid(sceneNode))
	{
		return false;
	}
	if (const auto *info = sceneNodeRegistry.try_get<NodeInfo>(sceneNode)) sceneNodeId = info->node_id();
	const auto *rpcClient = sceneNodeRegistry.try_get<RpcClientPtr>(sceneNode);
	if (!rpcClient || !*rpcClient)
	{
		return false;
	}
	ProcessClientPlayerMessageRequest exitMsg;
	exitMsg.set_session_id(sessionId);
	// 反向身份栅栏(routing-identity-audit-20260908.md R13 的反向面):
	// scene 的 SessionMap 里可能还留着"旧 gate 的同号 session -> 别的玩家"
	// 的映射,不带玩家身份的话这条 ExitGame 会把**别人**踢下线。
	// 未绑定玩家(kInvalidGuid)时留 0 走兼容位。
	if (session.playerId != 0 && session.playerId != kInvalidGuid)
	{
		exitMsg.set_player_id(session.playerId);
	}
	exitMsg.mutable_message_content()->set_message_id(ScenePlayerExitGameMessageId);
	// 单向:断线时会话随后就 erase,LeaveGame 时会话随后清掉 scene 指向,回包都无处可去。返回值只用于日志(未连接时为 false)。
	return (*rpcClient)->SendRequest<SceneProcessClientPlayerMessageRpc>(exitMsg);
}

void GateClientSessionHandler::HandleConnectionDisconnection(const muduo::net::TcpConnectionPtr &conn)
{
	const auto sessionId = GetSessionId(conn);

	// 从未建立过会话的连接(HandleConnectionEstablished 里三条 fail-closed 拒连:
	// 容量超限 / node 段未就绪 / prod 空密钥)在这里必须直接返回。
	//
	// 它们没有 session、没有 player,下面每一步都是无意义的:
	//   * sessions.find 必然落空;
	//   * **而 Login 断线通知并没有被 sessionFound 守住** —— 会带着
	//     session_id=kInvalidSessionId 向 login 发一次 gRPC。于是"连接洪峰"
	//     被原样放大成"对 login 的 RPC 洪峰",限流闸门反倒成了新的放大器;
	//   * 每条还要再打一行 LOG_INFO。
	// 拒连路径已经 setContext(kInvalidSessionId),所以这里判得到,
	// 且 GetSessionId 不会再抛 bad_any_cast(那条 catch 每次都要打 ERROR,
	// 同样是按攻击流量放大的日志写入)。
	if (sessionId == kInvalidSessionId)
	{
		return;
	}
	const std::string peer = conn ? conn->peerAddress().toIpPort() : std::string{"<null>"};

	// Retrieve session info before erasing so we can notify both Login and Scene.
	auto &sessions = tlsSessionManager.sessions();
	auto sessionIt = sessions.find(sessionId);

	const bool sessionFound = (sessionIt != sessions.end());
	const bool sessionVerified = sessionFound && sessionIt->second.verified;
	const bool loginStarted = sessionFound && sessionIt->second.loginStarted;
	const Guid playerId = sessionFound ? sessionIt->second.playerId : kInvalidGuid;
	const bool hasBoundPlayer = playerId != 0 && playerId != kInvalidGuid;
	const bool shouldNotifyLogin = loginStarted || hasBoundPlayer;
	bool sceneNotified = false;
	uint32_t sceneNodeId = 0;

	if (sessionFound)
	{
		// Notify Scene to exit the player immediately (saves state and stops broadcasting).
		sceneNotified = NotifySceneExitGame(sessionId, sessionIt->second, sceneNodeId);
	}

	// Disconnect notification goes to Login; its session manager owns the disconnect lease.
	// Login nodes are stateless -- pick any available node, no session affinity needed.
	//
	// 只有真正向 Login 发起过登录、或已经绑定玩家的会话才需要通知。未认证裸连、
	// 以及只做完 token verify 的空闲连接若也逐条通知,攻击者只需反复
	// connect-close(或重放短期 gate token)就能把 TCP 洪峰放大成 Gate->Login RPC
	// 洪峰;默认的 kInvalidGuid 还会让 Login/PlayerLocator 做无意义查询和重试。
	//
	// 已 dispatch Login.Login 但尚未 BindSession 的窗口仍要通知:Login 可能已经按
	// session_id 写了临时状态。此时 player_id 保持 proto 默认 0,Login 会清 session
	// 状态后在 markPlayerSessionDisconnecting 的 playerID==0 门禁早退。真正绑定
	// 玩家后才携带 SessionInfo.playerId,否则 TCP close 会把 player_locator 会话
	// 永久留在 ONLINE。
	if (shouldNotifyLogin)
	{
		loginpb::LoginNodeDisconnectRequest request;
		request.set_session_id(sessionId);
		// 未绑定玩家时 player_id 保持 0(见上面的注释:Login 会在 playerID==0 门禁早退)。
		SessionDetails sessionDetails = BuildSessionDetails(sessionId, hasBoundPlayer ? playerId : 0, &sessionIt->second);
		// 必须带会话代次:login 会话栅栏按 (gate_instance_id, session_incarnation) 记闭合标记,
		// 代次为 0 时 Close 整段跳过,经 lane 发出的 Login / EnterGame 建起的栅栏就永远关不上
		// (client-request-ordering.md §6.2)。shouldNotifyLogin 蕴含 sessionFound,且此处在
		// 下面的 OnSessionClosing / erase 之前,sessionIt 有效。gate_call_seq 留 0:回包不归 lane。
		sessionDetails.set_session_incarnation(sessionIt->second.incarnation);

		if (gate_router_mode::IsRouterModeEnabled())
		{
			// 路由模式(client-rpc-router.md D34):gate 对 login 零 stub,断线通知合成一条
			// ClientRequest 走与客户端请求同一条 Forward 路径,路由服按 message_id 转给
			// 同 zone 的 login,Login 侧看到的请求体与 metadata 与直连时完全相同。
			// id=0:这是单向通知;回包 MessageContent 到达时会话已 erase,桥接找不到会话
			// 直接丢弃 —— 与直连模式下 LoginNodeDisconnectResponse 的处置一致。
			// 无路由服时 SendViaRouter 已采样 WARN 留痕;这里没有客户端可回 tip,Login 侧
			// 的会话状态由其自身租约超时兜底(与直连模式下 login 全下线时的行为一致)。
			ClientRequest disconnectRequest;
			disconnectRequest.set_id(0);
			disconnectRequest.set_message_id(ClientPlayerLoginDisconnectMessageId);
			disconnectRequest.set_body(request.SerializeAsString());
			SendViaRouter(disconnectRequest, sessionDetails);
		}
		else
		{
			const auto loginNode = PickRandomNode(eNodeType::LoginNodeService);
			if (loginNode)
			{
				GrpcCallOptions options;
				std::string bin;
				if (sessionDetails.SerializeToString(&bin))
				{
					options.metadata.emplace_back(kSessionBinMetaKey, std::move(bin));
				}
				loginpb::SendClientPlayerLoginDisconnect(
					tlsNodeContextManager.GetRegistry(eNodeType::LoginNodeService), *loginNode, request,
					[sessionId](const GrpcCallOutcome &outcome, const loginpb::LoginEmptyResponse *)
					{
						if (outcome.ok())
						{
							return; // 会话随后就 erase,没有客户端可回。
						}
						static uint64_t failedCount = 0;
						if ((failedCount++ & 0x3FF) == 0)
						{
							LOG_WARN << "Login disconnect call failed (sampled), session_id=" << sessionId
							         << ", code=" << outcome.code << ", message=" << outcome.message
							         << ", failed_total=" << failedCount;
						}
					},
					options);
			}
		}
	}

	// 路由 lane:注销在途调用(晚到的回包一律丢弃)、丢弃没发出的排队项;不等在途调用(O8)。
	// gate 合成的 Disconnect 不进 lane(上面已直接发出,gate_call_seq = 0),由 login 会话栅栏兜底。
	if (sessionFound)
	{
		gate_lane::OnSessionClosing(sessionIt->second);
		// 观战订阅只存 session_id:不摘除的话 session_id 复用后新连接会收到旧订阅的帧。
		gate_spectate::OnSessionClosed(sessionId);
	}
	sessions.erase(sessionId);

	if (hasBoundPlayer)
	{
		LOG_INFO << "Client disconnected, session_id=" << sessionId
				 << ", player_id=" << playerId
				 << ", peer=" << peer
				 << ", session_found=" << sessionFound
				 << ", scene_node_id=" << sceneNodeId
				 << ", scene_notified=" << sceneNotified
				 << ", remaining_sessions=" << sessions.size();
	}
	else
	{
		// 未绑定连接的建立/断开次数完全由公网流量决定,不能逐连接写日志。
		static uint64_t unboundDisconnectCount = 0;
		if ((unboundDisconnectCount++ & 0x3FF) == 0)
		{
			LOG_INFO << "Unbound client disconnects sampled, latest_session_id=" << sessionId
					 << ", latest_peer=" << peer
					 << ", latest_verified=" << sessionVerified
					 << ", latest_login_started=" << loginStarted
					 << ", disconnect_total=" << unboundDisconnectCount
					 << ", remaining_sessions=" << sessions.size();
		}
	}
}

// High-water-mark: output buffer exceeded threshold — client not consuming (disconnect/cheat/slow), force close.
static constexpr size_t kClientHighWaterMark = 2 * 1024 * 1024; // 2MB

static void OnClientHighWaterMark(const muduo::net::TcpConnectionPtr &conn, size_t oldLen)
{
	const auto sessionId = GateClientSessionHandler::GetSessionId(conn);
	LOG_WARN << "Client high water mark triggered, session_id=" << sessionId
			 << ", buffered=" << oldLen
			 << " bytes, threshold=" << kClientHighWaterMark
			 << " bytes, forcing close";
	conn->forceClose();
}

void GateClientSessionHandler::HandleConnectionEstablished(const muduo::net::TcpConnectionPtr &conn)
{
	// 并发连接上限。必须是第一道闸:在发 session_id、建 SessionInfo 之前拒掉,
	// 否则限流本身就先付出了它想省下的那份内存。
	//
	// gate 是唯一对公网开放的端口,而 muduo 的 TcpServer 无条件 accept:
	// 在这条闸门之前,全仓没有**任何**连接数上限。不需要任何凭证,只要一直建连
	// 就能把 gate 的 fd、SessionMap 和每连接读写缓冲吃光,直到 accept 撞 EMFILE
	// (muduo 此时会用 idleFd 兜底,但连接已经建不上了)或进程 OOM —— 纯资源
	// 耗尽面,且现有的两层防护都拦不住它:token 校验发生在这之后,
	// IllegalPacketCounter 只按**已建立的会话**计数。
	//
	// 用当前会话数而不是另立计数器:sessions() 与连接是严格一一对应的
	// (本函数插入、HandleConnectionDisconnection 删除),不会漂移。
	// 拒绝时直接 forceClose,不回任何应答 —— 已经在容量边界上了,
	// 再为每条被拒连接序列化一条 tip 正好是攻击者想要的放大。
	//
	// ⚠️ 这条闸门的口径依赖 gate 是**单 IO 线程**:tlsSessionManager 是
	// thread_local,下面的静态计数器也没有同步。全仓没有任何 setThreadNum,
	// muduo 默认 0 个 IO 线程(全部回调都在主 loop),所以现在成立。
	// 将来谁给 TcpServer 开了线程池,这个上限会退化成"每线程一份",
	// 且 SessionMap 本身就会先分裂 —— 那时必须先解决会话表的线程模型。
	const auto configuredMaxConnections =
		gNodeConfigManager.GetBaseDeployConfig().gate_max_connections();
	// dev/test 的 0 只关闭“运维配置阈值”,不能关闭 session-id 空间的硬上限。
	// 否则 131071 个安全 ID 全占满后,下面的碰撞循环会永远找不到空号。
	const auto effectiveMaxConnections = configuredMaxConnections > 0
		? configuredMaxConnections
		: SessionIdGenerator::kSeqMask;
	if (tlsSessionManager.sessions().size() >= effectiveMaxConnections)
	{
		// 打日志必须限流:被拒的连接量正是攻击流量的量级,每条一行 ERROR
		// (同步磁盘 I/O)就是把 DoS 从连接层转成日志层,这个坑本轮审计
		// 在 CheckMessageSize 那里刚踩过。每 1024 条汇总一行。
		static uint64_t rejectedCount = 0;
		if ((rejectedCount++ & 0x3FF) == 0)
		{
			LOG_ERROR << "Gate at connection capacity, rejecting new connections."
					  << " configured_limit=" << configuredMaxConnections
					  << " effective_limit=" << effectiveMaxConnections
					  << " current=" << tlsSessionManager.sessions().size()
					  << " rejected_total=" << rejectedCount
					  << " peer=" << conn->peerAddress().toIpPort();
		}
		// 必须显式置成 kInvalidSessionId:否则断开回调里 GetSessionId 会对空
		// context 抛 bad_any_cast,那条 catch 每次都打一行 ERROR —— 按被拒流量
		// 放大的同步磁盘写入。见 HandleConnectionDisconnection 顶部的早退。
		conn->setContext(kInvalidSessionId);
		conn->forceClose();
		return;
	}

	// fail-closed:node 段没种好就绝不发号。带 node 段 0 的 session_id 是"坏号" ——
	// scene 侧 GetGateNodeId() 得到 0、永远解析不出归属 gate,玩家整局静默不可用,
	// 且无任何自愈路径。宁可当场拒连让客户端重连,也不要放一个坏号进系统。
	// 正常路径下 main.cpp 已在装 connection 回调之前 set_node_id,这里是纵深防御。
	if (tlsSessionManager.session_id_gen().node_id_prefix() == 0)
	{
		LOG_ERROR << "Rejecting connection: session id generator has no node id yet, peer="
				  << conn->peerAddress().toIpPort();
		conn->setContext(kInvalidSessionId); // 同上:让断开回调走早退,不惊动 login
		conn->forceClose();
		return;
	}

	// fail-closed(其二):空 gate_token_secret 不再等于"放行"。
	//
	// 旧写法唯一的判据是"密钥是不是空字符串",注释写着 Dev mode,可代码里根本
	// 没有任何 dev/prod 判别。于是生产上只要 GateTokenSecret 忘配、或 ConfigMap
	// 挂载失败读成空,gate 就把**每一条**连接直接标成 verified —— 整条令牌校验
	// 链路静默失效,任何人裸连这个端口就能当作已登录玩家发消息,而日志里连一
	// 条异常都没有。这类"降级成不设防"必须由显式运行模式授权,不能由一个配置
	// 项恰好为空来隐式触发。
	//
	// 现在的判据是 GATE_RUN_MODE(见 gate_security.h,默认 prod):
	//   * 配了密钥          -> 正常校验;
	//   * 空密钥 + dev/test -> 放行,但打醒目 WARN 留痕;
	//   * 空密钥 + prod     -> 当场拒连。
	// main.cpp 的启动门禁已经会在这种配置下直接 LOG_FATAL 拒绝启动,这里是
	// 纵深防御(比如有人把启动门禁改掉了,连接层仍然不会放行)。
	const auto runMode = gate_security::CurrentRunMode();
	const auto &tokenSecret = gNodeConfigManager.GetBaseDeployConfig().gate_token_secret();
	const auto secretVerdict = gate_security::ClassifyTokenSecret(tokenSecret, runMode);
	if (secretVerdict == gate_security::TokenSecretVerdict::kRefuse)
	{
		LOG_ERROR << "Rejecting connection: gate_token_secret is empty while run_mode="
				  << gate_security::RunModeName(runMode)
				  << " (fail-closed), peer=" << conn->peerAddress().toIpPort();
		conn->setContext(kInvalidSessionId); // 同上:让断开回调走早退,不惊动 login
		conn->forceClose();
		return;
	}

	auto sessionId = tlsSessionManager.session_id_gen().Generate();
	// kInvalidSessionId(UINT32_MAX) 是“连接从未建立会话”的保留哨兵。
	// 复合发号器在 node_id=32767 且 seq=131071 时确实能生成这个值;
	// 不显式跳过的话,真实会话断开时会命中顶部早退,漏掉 SessionMap、Scene
	// 与 Login 清理,也会让连接计数永久漂移。
	while (sessionId == kInvalidSessionId ||
		   tlsSessionManager.sessions().find(sessionId) != tlsSessionManager.sessions().end())
	{
		sessionId = tlsSessionManager.session_id_gen().Generate();
	}

	// Session ID prevents packet tampering / message misdirection

	conn->setContext(sessionId);
	conn->setHighWaterMarkCallback(OnClientHighWaterMark, kClientHighWaterMark);

	SessionInfo session;
	session.conn = conn;
	// 会话代次:进程内单调,随 SessionDetails 发出,路由 lane 认领回包与 login 会话栅栏都用它(client_request_lane.h)。
	session.incarnation = gate_lane::NextIncarnation();

	if (secretVerdict == gate_security::TokenSecretVerdict::kDevBypass)
	{
		// 明确授权的降级路径:非生产环境且没配密钥,自动标记已验证。
		// WARN 只打一次 —— 每条连接都打会在压测/开服洪峰里把日志刷爆,
		// 而这条信息是进程级常量,一次就够定性。
		static bool sBypassWarned = false;
		if (!sBypassWarned)
		{
			sBypassWarned = true;
			LOG_WARN << "SECURITY WARNING: gate_token_secret is EMPTY and " << gate_security::kRunModeEnv
					 << "=" << gate_security::RunModeName(runMode)
					 << " — every client connection is auto-verified without a token."
					 << " NEVER run this configuration in production.";
		}
		session.verified = true;
	}

	tlsSessionManager.sessions().emplace(sessionId, std::move(session));

	// 所有新连接此刻都还没绑定玩家,数量完全由公网流量决定;逐连接 INFO 会让
	// connect-close 攻击在不触碰并发上限的情况下无限放大日志。每 1024 条采样。
	static uint64_t acceptedConnectionCount = 0;
	if ((acceptedConnectionCount++ & 0x3FF) == 0)
	{
		const std::string peer = conn ? conn->peerAddress().toIpPort() : std::string{"<null>"};
		LOG_INFO << "Client connections accepted (sampled), latest_session_id=" << sessionId
				 << ", latest_peer=" << peer
				 << ", accepted_total=" << acceptedConnectionCount
				 << ", current_sessions=" << tlsSessionManager.sessions().size();
	}
}

// gate 级失败回执:带请求 id 与原 message_id 的错误信封(client-request-ordering.md O6、§4.7),
// 形状与 CheckMessageLimit / gate_lane::SendEnvelope 相同。不用 SendEnvelope 是因为它要 SessionInfo&
// 去 lock 会话里的弱连接,而 HandleTcpNodeMessage 只拿到 const SessionInfo&;这里的 conn 是本次收包回调
// 上的连接,调用期间必然有效,直接用它,不为一次 lock 去 const_cast。
// 不带 id 的 23 号 tip 会被客户端当成别的请求的失败(同一会话可能还有其他请求在等回包),所以请求回执一律走信封;
// 推送类的 23(进场失败、被踢等)不是请求回执,不走这里。
static void SendRequestErrorEnvelope(const muduo::net::TcpConnectionPtr &conn, const ClientRequest &request, uint32_t tipId)
{
	MessageContent envelope;
	envelope.set_id(request.id());
	envelope.set_message_id(request.message_id());
	envelope.mutable_error_message()->set_id(tipId);
	GetGateCodec().send(conn, envelope);
}

// Handle messages related to the game node
void HandleTcpNodeMessage(const SessionInfo &session, const RpcClientMessagePtr &request, SessionId sessionId, const muduo::net::TcpConnectionPtr &conn)
{
	assert(request->message_id() < gRpcMethodRegistry.size());
	auto &handlerMeta = gRpcMethodRegistry[request->message_id()];

	// Player sent message without being logged in — invalid node binding
	const auto targetNodeEntity = session.GetNodeEntity(handlerMeta.targetNodeType);
	auto &registry = tlsNodeContextManager.GetRegistry(handlerMeta.targetNodeType);
	if (!registry.valid(targetNodeEntity))
	{
		LOG_WARN << "[TCP Node] Scene not ready, dropping message_id: " << request->message_id()
				 << ", session_id: " << sessionId
				 << ", node_type: " << handlerMeta.targetNodeType;

		// 确定未发出:回带请求 id 的 1003(2026-10-15 起;原为 23 号 tip),客户端重试安全。不计非法包、不断连。
		SendRequestErrorEnvelope(conn, *request, kServiceUnavailable);
		return;
	}

	// 跨实体查询用 try_get 不用 get(CLAUDE.md §7 不变量 5)。
	// registry.valid() 只保证实体活着,不保证 RpcClientPtr 已经挂上去:
	// 节点实体在握手/发现阶段就被 create() 出来,RpcClientPtr 是随后才 emplace 的,
	// 而且组件在但 shared_ptr 为空同样可能(连接已断开正在重连)。
	// 这两种情况下旧写法分别是 get 抛异常和空指针解引用 —— 都是把 gate 打崩,
	// 而 gate 崩 = 该节点上所有在线玩家一起掉线。
	// 本文件的 NotifySceneExitGame、gate_service_handler.cpp 的 BoundSceneInstance 已经是 try_get + 判空的写法。
	const auto *tcpNode = registry.try_get<RpcClientPtr>(targetNodeEntity);
	if (tcpNode == nullptr || !*tcpNode)
	{
		LOG_WARN << "[TCP Node] RpcClient not attached, dropping message_id: " << request->message_id()
				 << ", session_id: " << sessionId
				 << ", node_type: " << handlerMeta.targetNodeType;

		// 同上:确定未发出,回带请求 id 的 1003(原为 23 号 tip)。
		SendRequestErrorEnvelope(conn, *request, kServiceUnavailable);
		return;
	}

	ProcessClientPlayerMessageRequest message;
	message.mutable_message_content()->set_serialized_message(request->body());
	message.set_session_id(sessionId);
	// 反向身份栅栏(R13):gate 的 routing node_id 被复用后,scene 侧同号 session 可能
	// 仍映射着前任 gate 上的另一个玩家 —— 不带玩家身份的话,本客户端的每一条业务消息
	// 都会被派发到**那个玩家**的实体上执行。未绑定玩家(kInvalidGuid)时留 0 走兼容位。
	if (session.playerId != kInvalidGuid && session.playerId != 0)
	{
		message.set_player_id(session.playerId);
	}
	message.mutable_message_content()->set_id(request->id());
	message.mutable_message_content()->set_message_id(request->message_id());

	// 内层方法的响应是 Empty(MoveStart / MoveStop / MoveSync 等):单向发送,scene 不回包 —— 线上与改造前逐字节相同
	// (client-request-ordering.md O2 的前提不变),不登记在途、不推失败回执,gate 最热的路径上零新增开销。
	const auto *innerResponse = handlerMeta.responseProto.get();
	if (innerResponse == nullptr || innerResponse->GetDescriptor() == ::Empty::GetDescriptor())
	{
		(*tcpNode)->SendRequest<SceneProcessClientPlayerMessageRpc>(message);
		LOG_TRACE << "Sent one-way message to game node, session id: " << sessionId << ", message id: " << request->message_id();
		return;
	}

	// 其余走每次调用回调(docs/design/muduo-rpc-channel.md §8.1):回包与失败都在这里落到客户端,
	// 上下文在发出时捕获,不再靠 scene 回显 session_id 去查全局回包表。
	(*tcpNode)->CallMethod<SceneProcessClientPlayerMessageRpc>(
		message,
		[sessionId, incarnation = session.incarnation, clientRequestId = request->id(), innerMessageId = request->message_id()](
			const RpcStatus &status, const std::shared_ptr<ProcessClientPlayerMessageResponse> &response)
		{
			ForwardSceneClientCallResult(sessionId, incarnation, clientRequestId, innerMessageId, status, response);
		},
		RpcCallOptions{kGateToSceneClientCallTimeout});

	LOG_TRACE << "Sent message to game node, session id: " << sessionId << ", message id: " << request->message_id();
}

void HandleGrpcNodeMessage(SessionId sessionId, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn)
{
	assert(request->message_id() < gRpcMethodRegistry.size());
	auto &rpcHandlerMeta = gRpcMethodRegistry[request->message_id()];
	if (!ParseMessageFromRequestBody(*rpcHandlerMeta.requestProto, request, sessionId))
	{
		// 坏包不转发。共享原型已被清空,不会把残留数据递给后端。
		// 回带请求 id 的信封(client-request-ordering.md O6,2026-10-15 起;原为 23 号 tip)。不计非法包、不断连,与改前一致。
		SendRequestErrorEnvelope(conn, *request, kRequestMessageParseError);
		return;
	}

	const auto sessionIt = tlsSessionManager.sessions().find(sessionId);
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LOG_ERROR << "Session not found for session id: " << sessionId;
		return;
	}

	// 本连接没有成功 Login 就不发 CreatePlayer / EnterGame / LeaveGame(login_admission.h)。直连没有 lane,
	// 收包即发出,所以在这里判;成功标记由 rpc_reply_bridge 的直连回包分支写。确定未执行,回与 login 同一个「未登录」码。
	if (gate_login_admission::RequiresSuccessfulLogin(request->message_id()) && !sessionIt->second.loginSucceeded)
	{
		static uint64_t rejectedCount = 0;
		if ((rejectedCount++ & 0x3FF) == 0)
		{
			LOG_WARN << "Login-required request before a successful Login on this connection; rejected (sampled)"
					 << " latest_session_id=" << sessionId << " latest_message_id=" << request->message_id()
					 << " rejected_total=" << rejectedCount;
		}
		SendRequestErrorEnvelope(conn, *request, kLoginSessionNotFound);
		return;
	}

	const SessionDetails sessionDetails =
		BuildSessionDetails(sessionId, sessionIt->second.playerId, &sessionIt->second);

	// Stress diagnostic 2026-05-24: scene_manager observed gate_id="0" in
	// EnterScene requests during 3-zone × 15000 round 2, even though cpp
	// gate had long since completed NodeId CAS. Logging the actual values
	// at the SessionDetails build site to confirm whether (a) the cast
	// drops the high bits, (b) gNode->GetNodeId() is genuinely 0 here, or
	// (c) login overwrites the field somewhere downstream.
	LOG_INFO << "[diag] SessionDetails to login: session_id=" << sessionId
			 << " player_id=" << sessionIt->second.playerId
			 << " gate_node_id=" << gNode->GetNodeId()
			 << " message_id=" << request->message_id();

	if (rpcHandlerMeta.sender)
	{
		// 路由规则(保持向后兼容):
		//   1. 会话对目标 nodeType 已有绑定(SessionInfo.SetNodeEntity 过)→ 一律走绑定实体;
		//   2. 无绑定时,有状态节点(Scene)不能随机路由 —— 玩家实体只在特定 scene 节点上,
		//      随机挑一个只会得到"玩家不存在";ResolveSessionTargetNode 对无绑定会话返回 nullopt,
		//      统一落到下面的错误应答;
		//   3. 其余 nodeType(login/guild/friend/chat 等无状态 Go 服务)维持 PickRandomNode 现状。
		// 战斗消息(BattleNodeService)到不了这里:DispatchClientRpcMessage 在分派之前已统一拒绝
		// (turn-based §22 D66),gate 也不再连 battle。
		std::optional<entt::entity> node;
		const bool requiresSessionBinding =
			rpcHandlerMeta.targetNodeType == eNodeType::SceneNodeService;
		if (requiresSessionBinding || sessionIt->second.HasNodeEntity(rpcHandlerMeta.targetNodeType))
		{
			node = ResolveSessionTargetNode(sessionId, rpcHandlerMeta.targetNodeType);
		}
		else
		{
			node = PickRandomNode(rpcHandlerMeta.targetNodeType);
		}
		if (!node)
		{
			LOG_ERROR << "Node not found for session id: " << sessionId << ", message id: " << request->message_id();
			// 确定未发出:回带请求 id 的 1003(原为 23 号 tip)。
			SendRequestErrorEnvelope(conn, *request, kServiceUnavailable);
			return;
		}

		// 断开时只有确实走到过 Login.Login 的会话才需要清理 Login 的
		// session-id 状态。标记必须放在选到节点之后、实际 dispatch 之前。
		if (request->message_id() == ClientPlayerLoginLoginMessageId)
		{
			sessionIt->second.loginStarted = true;
		}

		std::string serializedSessionDetails;
		GrpcCallOptions options;
		if (sessionDetails.SerializeToString(&serializedSessionDetails))
		{
			options.metadata.emplace_back(kSessionBinMetaKey, serializedSessionDetails);
		}
		rpcHandlerMeta.sender(
			tlsNodeContextManager.GetRegistry(rpcHandlerMeta.targetNodeType), *node, *rpcHandlerMeta.requestProto,
			// 捕获客户端信封的请求号:回包与失败回执都回显它,客户端按 id 认领(client-rpc-callback.md §3)。
			[sessionId, incarnation = sessionIt->second.incarnation,
			 clientCall = gate_rpc_reply_bridge::DirectClientCall{request->id(), request->message_id()},
			 serializedSessionDetails](const GrpcCallOutcome &outcome, const google::protobuf::Message *reply)
			{ ForwardDirectGrpcCallResult(sessionId, incarnation, clientCall, serializedSessionDetails, outcome, reply); },
			options);
	}
}

// 路由模式下 PROTOCOL_GRPC 消息的唯一出口(HandleGrpcNodeMessage 的替身,
// client-rpc-router.md §3)。与直连路径的差别只有两处:
//   * 不解析 body —— gRpcMethodRegistry 里的共享原型 requestProto 完全不被触碰,
//     原包字节交给路由服,由目标 Go 服务自己解;
//   * 不挑业务实例 —— 只挑路由服实例。
// 三道闸(IsClientMessageId / 体积 / 限速)、GM 闸与战斗消息拒绝仍在调用方
// DispatchClientRpcMessage 里,顺序与直连模式完全相同;本函数只在闸门全部通过后被调用。
//
// 发出走会话路由 lane(docs/design/client-request-ordering.md §4):同一会话的请求按收包顺序单飞,
// 上一条观察到完成才发下一条;没有路由服、lane 满、排队超时等拒绝都由 lane 回带请求 id 的信封。
// Login.Login 的 loginStarted 置位也随 lane 移到真正发出的那一刻(client_request_lane.cpp Pump)。
static void HandleRouterForward(SessionId sessionId, const RpcClientMessagePtr &request)
{
	assert(request->message_id() < gRpcMethodRegistry.size());

	const auto sessionIt = tlsSessionManager.sessions().find(sessionId);
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LOG_ERROR << "Session not found for session id: " << sessionId;
		return;
	}

	gate_lane::Admit(sessionId, sessionIt->second, request,
					 BuildSessionDetails(sessionId, sessionIt->second.playerId, &sessionIt->second));
}

bool GateClientSessionHandler::ValidateClientMessage(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const
{
	if (!CheckMessageSize(session, request, conn))
		return false;
	if (!CheckMessageLimit(session, request, conn))
		return false;
	return true;
}

// Main request handler, forwards the request to the appropriate service
void GateClientSessionHandler::DispatchClientRpcMessage(const muduo::net::TcpConnectionPtr &conn,
													   const RpcClientMessagePtr &request,
													   muduo::Timestamp)
{
	auto sessionId = GetSessionId(conn);
	const auto sessionIt = tlsSessionManager.sessions().find(sessionId);
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LogClientSecurityRejectionSampled("client_request_missing_session", sessionId);
		conn->forceClose();
		return;
	}

	auto &session = sessionIt->second;

	// Reject all game messages until the client passes token verification.
	// 唯一会跳过令牌校验的情形:gate_token_secret 为空**且** GATE_RUN_MODE 是
	// dev/test —— 那种情况下 HandleConnectionEstablished 会在建连时就标记
	// verified 并打一条 SECURITY WARNING。生产模式下空密钥根本连不进来。
	if (!session.verified)
	{
		LogClientSecurityRejectionSampled("client_request_before_token_verify", sessionId);
		conn->forceClose();
		return;
	}

	// 白名单校验必须排在鉴权闸门之后,并且要计入非法包闸门。
	//
	// 旧写法把这一段放在 `!session.verified` 之前,而且只 LOG_ERROR + 裸 return:
	// 既不计数、也不关连接。于是一个**未认证**的客户端可以无限发不存在的
	// message_id —— 每一个包都让 gate 写一条 ERROR 级日志(同步磁盘 I/O),
	// 却永远碰不到 GATE_ILLEGAL_PACKET_THRESHOLD 的踢人阈值,连接也永不断开。
	// 这是一条不需要任何凭证就能打的日志放大 DoS。
	// SessionInfo::illegalPacketCount 的注释本来就把 "unknown messageId" 和
	// "unauthenticated send" 列为应当计数的情形,这里只是把它真正接上。
	if (request->message_id() >= gRpcMethodRegistry.size() || !IsClientMessageId(request->message_id()))
	{
		LOG_DEBUG << "Invalid or unauthorized message ID: " << request->message_id()
				  << ", session_id: " << sessionId;
		if (IllegalPacketCounter::RegisterAndShouldKill(session.illegalPacketCount))
		{
			LOG_WARN << "Session illegal-packet threshold exceeded (bad message_id) — forceClose."
					 << " count=" << session.illegalPacketCount
					 << " message_id=" << request->message_id();
			conn->forceClose();
		}
		return;
	}

	if (!ValidateClientMessage(session, request, conn))
		return;

	// GM 客户端消息闸(P0-a,gate_security.h「GM 客户端消息闸」一节)。
	//
	// GmAddCurrency(37)/ GmDeductCurrency(49)/ GmBlockCurrency(94)/
	// GmUnblockCurrency(95)/ GmSetPlayerLevel(175)/ GmGrantPet(187) 挂在标了
	// OptionIsClientProtocolService 的 player 服务上,因此一直与 GetBag / MoveStart
	// 走同一条路径进 scene,零鉴权 —— 任何已登录客户端发一个包就能给自己加钱、
	// 升满级、发宝宝。聚宝斋要用人民币寄售(jubaozhai-market.md §12 P0-a),这条
	// 口子等于印钞机接到交易所上。
	//
	// 放在 ValidateClientMessage **之后**:GM 包同样要先过体积与限流两道闸,
	// 否则"被拒绝的消息不占限流额度"会让攻击者用 GM 号无成本刷日志和 CPU
	// (与本函数上面那段"白名单校验必须计入非法包闸门"同一条教训)。
	//
	// 判据是 GATE_RUN_MODE:未设置 = prod = 拒绝(部署链从不设它)。本地联调由
	// tools/scripts/start_game.ps1 显式设 dev。scene 侧另有第二道锁
	// (SCENE_RUN_MODE,player_gm_guard.h),防的是绕开 gate 直连 scene RPC 端口。
	//
	// 拒绝要计非法包:正常客户端根本不知道这些号,连发只可能是在试探。
	if (gate_gm_client_messages::IsGmClientMessage(request->message_id()) &&
		!gate_security::GmClientMessagesAllowed())
	{
		// 趋势证据走采样 WARN(每 1024 次一条);逐条明细只打 DEBUG ——
		// 与上面「坏 message_id」那一支同一口径。每条被拒的包都写一行 WARN
		// 等于把这条闸本身变成日志放大面(拒绝发生在踢线阈值之前)。
		LogClientSecurityRejectionSampled("gm_client_message_refused_in_prod", sessionId);
		LOG_DEBUG << "GM client message refused: GATE_RUN_MODE="
				  << gate_security::RunModeName(gate_security::CurrentRunMode())
				  << " message_id=" << request->message_id()
				  << " session_id=" << sessionId
				  << " player_id=" << session.playerId;
		// 回执而不是静默丢:本地把 GATE_RUN_MODE 忘了设时,robot 冒烟要能立刻
		// 看出"是被闸拦了",而不是超时到一半去猜。kFeatureUnavailable 是既有码,
		// 新增 tip 要改 data/tip/Tip.xlsx(二进制文件,多个并行会话在排队改它)。
		// 回执带请求 id 与原消息号(client-request-ordering.md O6,2026-10-15 起;原为 23 号 tip),理由同下方 battle 拒绝。
		gate_lane::SendEnvelope(session, request->id(), request->message_id(), kFeatureUnavailable);
		if (IllegalPacketCounter::RegisterAndShouldKill(session.illegalPacketCount))
		{
			LOG_WARN << "Session illegal-packet threshold exceeded (GM message in prod) — forceClose."
					 << " count=" << session.illegalPacketCount
					 << " message_id=" << request->message_id();
			conn->forceClose();
		}
		return;
	}

	auto &messageInfo = gRpcMethodRegistry[request->message_id()];

	// gate 本地服务(目前只有观战订阅 GateClientSpectate,battle-spectate-tiered-fanout.md §15.2):不转发、不进路由 lane,
	// 同时覆盖直连与路由两种模式。排在体积 / 限流 / GM 几道闸之后,订阅消息与其它消息占同一份额度。
	if (messageInfo.targetNodeType == eNodeType::GateNodeService)
	{
		gate_spectate::HandleClientRequest(sessionId, session, *request);
		return;
	}

	// 战斗消息 gate 两种模式都不中继(turn-based §22 D66):BattleClientPlayer 的上行只走客户端
	// 直连 battle;gate 出站白名单里没有 BattleNodeService,也不维护会话到 battle 的绑定。
	// 这组号仍在 IsClientMessageId 里(它们同时是直连面与 Notify* 下行的协议号,不能摘掉
	// OptionIsClientProtocolService),所以放在上面几道闸之后、按协议分派之前一处拦下,
	// 同时覆盖直连模式与路由模式,HandleGrpcNodeMessage / HandleRouterForward 都见不到它们。
	// 回 kServiceUnavailable、不计非法包、不断连:这是合法协议号(旧客户端或误走大厅),
	// 踢线会把大厅连接一起拆掉。逐条只打 DEBUG,与上面几道闸的明细日志同一口径。
	// 回执是带请求 id 与原消息号的信封(client-request-ordering.md O6、band-routing 阶段 0d):它排在 lane 之前、
	// 可能先于更早排队请求的回包到达,不带 id 的 23 号 tip 会被客户端当成别的请求的失败。2026-10-15 两版同批改
	// (原为 23 {1003},Java scene-battle-spec D12 同步修订)。
	if (messageInfo.targetNodeType == eNodeType::BattleNodeService)
	{
		LOG_DEBUG << "Battle message through gate rejected(battle uses client direct connections only), session_id=" << sessionId
				  << ", message_id=" << request->message_id();
		gate_lane::SendEnvelope(session, request->id(), request->message_id(), kServiceUnavailable);
		return;
	}

	// 客户端 LeaveGame(世界 → 选角,TCP 不断):先替这条会话通知绑定的 scene 退出,再清掉会话的 scene 路由状态,然后照常转发给
	// login(docs/design/location-release-binding.md §3.5)。以前 gate 不处理 LeaveGame,ExitGame 只在 TCP 断开时代发:
	// 持有节点的实体不退出也不存盘,login 侧却随即删会话、删 location,下一次进场按首次落点铸新 epoch,持有节点之后的存盘被拒、
	// 实体被当废黜销毁(回档,审计 §2.1)。在收包时做:直连 / 路由两种模式共用这一个点;login 侧失败时实体退出、location 保留,
	// 下一次 EnterGame 走同落点重连或换手门,都不回档。
	if (request->message_id() == ClientPlayerLoginLeaveGameMessageId)
	{
		gate_scene_entry::OnClientLeaveGame(sessionId, session);
	}
	// 客户端 EnterGame:同一个收包点记下"LeaveGame 之后已再次请求进场"。在这之前到达 gate 的 Bind / 路由只可能属于 LeaveGame
	// 之前那条登录链(login 只在处理 EnterGame 时发 Bind),一律丢弃;按收包顺序记,路由模式下 lane 里排在 LeaveGame 之前的
	// EnterGame 不会被误当成新的一次(第二轮审计 r2-gate#1,location-release-binding.md §3.5)。
	else if (request->message_id() == ClientPlayerLoginEnterGameMessageId)
	{
		gate_scene_entry::OnClientEnterGame(sessionId, session);
	}

	if (messageInfo.protocol == PROTOCOL_TCP)
	{
		HandleTcpNodeMessage(session, request, sessionId, conn);
	}
	else if (messageInfo.protocol == PROTOCOL_GRPC)
	{
		// 路由模式(GATE_CLIENT_RPC_ROUTER=1):原包交给路由服,不解析、不挑业务实例;
		// 直连模式:typed sender 直发业务服务,除战斗(上面已拒)外与引入路由服之前一致(D34)。
		if (gate_router_mode::IsRouterModeEnabled())
		{
			HandleRouterForward(sessionId, request);
		}
		else
		{
			HandleGrpcNodeMessage(sessionId, request, conn);
		}
	}
}

void GateClientSessionHandler::DispatchTokenVerify(const muduo::net::TcpConnectionPtr &conn,
												  const ClientTokenVerifyRequestPtr &message,
												  muduo::Timestamp)
{
	auto sessionId = GetSessionId(conn);
	const auto sessionIt = tlsSessionManager.sessions().find(sessionId);
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LogClientSecurityRejectionSampled("token_verify_missing_session", sessionId);
		conn->forceClose();
		return;
	}

	auto &session = sessionIt->second;

	auto sendReply = [&](bool success, const std::string &error)
	{
		ClientTokenVerifyResponse resp;
		resp.set_success(success);
		if (!error.empty())
			resp.set_error(error);
		protobufCodec.send(conn, resp);
	};
	auto rejectAndClose = [&](const char *reason, const std::string &clientError)
	{
		LogClientSecurityRejectionSampled(reason, sessionId);
		sendReply(false, clientError);
		// shutdown 立刻把状态切到 kDisconnecting,codec 会停止分发 pipeline;
		// 短延迟给错误应答一个 flush 窗口,随后强关,不让恶意端长期占槽。
		conn->shutdown();
		conn->forceCloseWithDelay(0.1);
	};

	if (session.verified)
	{
		sendReply(true, "");
		return;
	}

	// 与 HandleConnectionEstablished 同一条判据:空密钥的处置由显式运行模式
	// 决定,不能只看"密钥是不是空字符串"。这条路径尤其致命 —— 旧写法下客户端
	// 只要发一条 ClientTokenVerifyRequest,不带任何签名,就能拿到 success=true
	// 并被标成已验证。
	const auto runMode = gate_security::CurrentRunMode();
	const auto &secret = gNodeConfigManager.GetBaseDeployConfig().gate_token_secret();
	const auto secretVerdict = gate_security::ClassifyTokenSecret(secret, runMode);
	if (secretVerdict == gate_security::TokenSecretVerdict::kDevBypass)
	{
		session.verified = true;
		sendReply(true, "");
		return;
	}
	if (secretVerdict == gate_security::TokenSecretVerdict::kRefuse)
	{
		rejectAndClose("token_secret_not_configured", "gate token secret not configured");
		return;
	}

	// Recompute HMAC-SHA256(secret, payload) and compare with client-provided signature (hex)
	const auto &payloadBytes = message->payload();
	const auto &clientSig = message->signature();

	auto expectedHex = gate_security::HmacSha256Hex(secret, payloadBytes);
	std::string clientSigStr(clientSig.begin(), clientSig.end());

	// 常数时间比较。std::string 的 != 在首个不匹配字节处提前返回,
	// 攻击者可用计时差逐字节猜签名。长度不等可以直接拒(长度不是秘密)。
	if (!gate_security::ConstantTimeEquals(expectedHex, clientSigStr))
	{
		rejectAndClose("token_hmac_mismatch", "invalid token signature");
		return;
	}

	// Deserialize payload and validate fields
	GateTokenPayload payload;
	if (!payload.ParseFromString(payloadBytes))
	{
		rejectAndClose("token_payload_parse_failed", "malformed token payload");
		return;
	}

	// Check gate_node_id matches this gate
	if (payload.gate_node_id() != gNode->GetNodeId())
	{
		rejectAndClose("token_gate_node_mismatch", "token not for this gate");
		return;
	}

	// gate_node_id 只在 zone 内唯一:给 zone B gate#3 的重定向票据,在 zone A 的 gate#3 上同样能过
	// 上面那道检查。重定向票据带 target_zone_id,在这里把"不是发给本 zone 的"挡掉;
	// 0 = 普通 AssignGate 票据(不带目标 zone),照旧放行。
	if (payload.target_zone_id() != 0 && payload.target_zone_id() != gNode->GetNodeInfo().zone_id())
	{
		rejectAndClose("token_target_zone_mismatch", "token not for this zone");
		return;
	}

	// Check expiry
	auto now = static_cast<int64_t>(std::time(nullptr));
	if (payload.expire_timestamp() <= now)
	{
		rejectAndClose("token_expired", "token expired");
		return;
	}

	// todo.md #76 slice A — install the per-session HMAC key carried in
	// the verified token payload. Empty bytes mean "client/server pair
	// hasn't rolled out the signed-message path yet" — we accept the
	// verification anyway and fall back to the adler32-only path in
	// codec.cpp. Once the wire-format slot for `hmac_tag` lands
	// (slice B), absent key here flags the connection as "unsigned-
	// allowed" so the gate's per-message verifier knows whether to
	// require a tag.
	//
	// Resetting the illegal-packet counter on a verified handshake is
	// intentional: a fresh ClientTokenVerify means the client has just
	// completed AssignGate, so any pre-handshake packet noise is
	// behind us. If illegal packets resume after this point, that's a
	// real attack signal and the threshold counter starts again from
	// zero.
	session.hmacSessionKey.assign(payload.hmac_session_key());
	IllegalPacketCounter::Reset(session.illegalPacketCount);

	// 重定向票据的绑定信息(CZ-8):只存不判,随 SessionDetails 交给 login EnterGame 校验。
	session.ticketPlayerId = payload.player_id();
	session.ticketTargetZoneId = payload.target_zone_id();

	session.verified = true;
	sendReply(true, "");
	LOG_DEBUG << "[Token] Session verified, session_id: " << sessionId
			  << " hmac_key_len=" << session.hmacSessionKey.size();
}

void GateClientSessionHandler::OnNodeRemoveEventHandler(const OnNodeRemoveEvent &pb)
{
	// 路由服节点摘除不需要在这里处理路由 lane:Node 先摘掉实体上的 MuduoGrpcChannelPtr,~Channel 把其上的在途调用
	// 以 UNAVAILABLE 经 queueInLoop 排进下一轮,再触发本事件、DestroyEntity。那些失败回调经 rpc_reply_bridge 交给
	// gate_lane::OnRouterCallFailed 认领(恰好一次)、回 1003、放行同一会话的下一条;放行时 PickRandomNode 要求实体
	// 挂着通道,不会再选中被摘节点。这里只清会话上记着的目标节点。
	for (auto &session : tlsSessionManager.sessions())
	{
		if (session.second.GetNodeEntity(pb.nodeType) != pb.node)
			continue;
		session.second.SetNodeEntity(pb.nodeType, entt::null);
	}
}
