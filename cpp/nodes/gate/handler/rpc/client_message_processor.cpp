#include "client_message_processor.h"

#include <algorithm>
#include <cassert>
#include <functional>
#include <iomanip>
#include <memory>
#include <unordered_map>
#include <optional>
#include <sstream>
#include <string_view>
#include <openssl/crypto.h>
#include <openssl/evp.h>
#include <openssl/hmac.h>

#include "gate_codec.h"
#include "message_limiter/illegal_packet_counter.h"
#include "error_reporter/error_reporter.h"
#include "node/system/node/node.h"
#include "grpc_client/login/login_grpc_client.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "rpc/service_metadata/player_lifecycle_service_metadata.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "proto/common/base/node.pb.h"
#include "node/system/node/node_util.h"
#include "proto/common/event/node_event.pb.h"
#include "thread_context/node_context_manager.h"
#include <session/manager/session_manager.h>
#include <network/node_utils.h>
#include <node_config_manager.h>
#include "handler/event/battle_binding_helper.h"

namespace
{
	std::string BytesToHex(const unsigned char *data, unsigned int size)
	{
		std::ostringstream stream;
		stream << std::hex << std::setfill('0');
		for (unsigned int index = 0; index < size; ++index)
		{
			stream << std::setw(2) << static_cast<unsigned int>(data[index]);
		}
		return stream.str();
	}

	std::string HmacSha256Hex(std::string_view secret, std::string_view payload)
	{
		unsigned char digest[EVP_MAX_MD_SIZE];
		unsigned int digestLength = 0;
		const auto *result = HMAC(EVP_sha256(),
								  secret.data(),
								  static_cast<int>(secret.size()),
								  reinterpret_cast<const unsigned char *>(payload.data()),
								  payload.size(),
								  digest,
								  &digestLength);
		if (result == nullptr)
		{
			return {};
		}
		return BytesToHex(digest, digestLength);
	}
}

static std::optional<entt::entity> PickRandomNode(uint32_t nodeType)
{
	std::vector<entt::entity> candidates;
	auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	auto view = registry.view<NodeInfo>();
	for (auto entity : view)
	{
		const auto &node = view.get<NodeInfo>(entity);
		if (node.zone_id() == GetNodeInfo().zone_id())
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

static inline uint64_t GetEffectiveNodeId(
	const SessionInfo &session,
	uint32_t nodeType)
{
	// Despite the name, this is the integral form of an entt::entity, not a
	// business node_id. The handshake layer (registration_manager /
	// gate_event_handler) always stores `entt::to_integral(entity)` here.
	const auto storedEntityInt = session.GetEntityId(nodeType);
	if (storedEntityInt == SessionInfo::kInvalidEntityId)
	{
		return storedEntityInt;
	}

	auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	const entt::entity storedEntity{storedEntityInt};
	if (registry.valid(storedEntity))
	{
		return storedEntityInt;
	}

	// Post uuid-refactor there is no node_id -> entity fallback: when the
	// stored entity has been destroyed it means the remote node is gone, and
	// any re-registration will be surfaced through event handlers that refresh
	// the session binding via entity integers. Returning kInvalidEntityId lets
	// the caller short-circuit rather than forward to a phantom node.
	return SessionInfo::kInvalidEntityId;
}

RpcClientSessionHandler::RpcClientSessionHandler(ProtobufCodec &codec,
												 ProtobufDispatcher &dispatcherParam)
	: protobufCodec(codec),
	  messageDispatcher(dispatcherParam)
{
	messageDispatcher.registerMessageCallback<ClientRequest>(
		std::bind(&RpcClientSessionHandler::DispatchClientRpcMessage, this, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));

	messageDispatcher.registerMessageCallback<ClientTokenVerifyRequest>(
		std::bind(&RpcClientSessionHandler::DispatchTokenVerify, this, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));

	tlsEcs.dispatcher.sink<OnNodeRemoveEvent>().connect<&RpcClientSessionHandler::OnNodeRemoveEventHandler>(*this);
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

	if (!session.HasEntityId(nodeType))
	{
		LOG_ERROR << "No node bound for nodeType: " << nodeType << ", session id: " << sessionId;
		return std::nullopt;
	}

	const auto &registry = tlsNodeContextManager.GetRegistry(nodeType);
	const auto effectiveEntityInt = GetEffectiveNodeId(session, nodeType);
	entt::entity nodeEntity = entt::entity{effectiveEntityInt};
	if (!registry.valid(nodeEntity))
	{
		LOG_ERROR << "Bound node is invalid. nodeType: " << nodeType
				  << ", session id: " << sessionId
				  << ", stored_entity_int=" << session.GetEntityId(nodeType);
		session.SetEntityId(nodeType, SessionInfo::kInvalidEntityId);
		return std::nullopt;
	}

	if (effectiveEntityInt != session.GetEntityId(nodeType))
	{
		session.SetEntityId(nodeType, effectiveEntityInt);
	}

	return nodeEntity;
}

void RpcClientSessionHandler::OnConnection(const muduo::net::TcpConnectionPtr &conn)
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

void RpcClientSessionHandler::SendMessageToClient(const muduo::net::TcpConnectionPtr &conn, const ::google::protobuf::Message &message) const
{
	protobufCodec.send(conn, message);
}

SessionId RpcClientSessionHandler::GetSessionId(const muduo::net::TcpConnectionPtr &conn)
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

void RpcClientSessionHandler::SendTipToClient(const muduo::net::TcpConnectionPtr &conn, uint32_t tipId)
{
	TipInfoMessage tipMessage;
	tipMessage.set_id(tipId);
	MessageContent message;
	message.set_serialized_message(tipMessage.SerializeAsString());
	message.set_message_id(SceneClientPlayerCommonSendTipToClientMessageId);
	GetGateCodec().send(conn, message);

	LOG_TRACE << "Sent tip message to session id: " << GetSessionId(conn) << ", tip id: " << tipId;
}

bool RpcClientSessionHandler::CheckMessageSize(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const
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

bool RpcClientSessionHandler::CheckMessageLimit(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const
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

void RpcClientSessionHandler::HandleConnectionDisconnection(const muduo::net::TcpConnectionPtr &conn)
{
	const auto sessionId = GetSessionId(conn);
	const std::string peer = conn ? conn->peerAddress().toIpPort() : std::string{"<null>"};

	// Retrieve session info before erasing so we can notify both Login and Scene.
	auto &sessions = tlsSessionManager.sessions();
	auto sessionIt = sessions.find(sessionId);

	const bool sessionFound = (sessionIt != sessions.end());
	bool sceneNotified = false;
	uint32_t sceneNodeId = 0;

	if (sessionFound)
	{
		// Notify Scene to exit the player immediately (saves state and stops broadcasting).
		if (sessionIt->second.HasEntityId(SceneNodeService))
		{
			const auto sceneEntityId = sessionIt->second.GetEntityId(SceneNodeService);
			sceneNodeId = sceneEntityId;
			auto &sceneRegistry = tlsNodeContextManager.GetRegistry(SceneNodeService);
			entt::entity sceneEntity{sceneEntityId};
			if (sceneRegistry.valid(sceneEntity))
			{
				const auto *rpcClient = sceneRegistry.try_get<RpcClientPtr>(sceneEntity);
				if (rpcClient && *rpcClient)
				{
					ProcessClientPlayerMessageRequest exitMsg;
					exitMsg.set_session_id(sessionId);
					exitMsg.mutable_message_content()->set_message_id(ScenePlayerExitGameMessageId);
					(*rpcClient)->CallRemoteMethod(SceneProcessClientPlayerMessageMessageId, exitMsg);
					sceneNotified = true;
				}
			}
		}
	}

	// Disconnect notification goes to Login; its session manager owns the disconnect lease.
	// Login nodes are stateless -- pick any available node, no session affinity needed.
	//
	// IMPORTANT: carry SessionInfo.playerId through SessionDetails. Login's
	// markPlayerSessionDisconnecting() early-returns when playerId == 0,
	// which meant a TCP close without explicit Logout would leave the
	// player_locator session in ONLINE forever — later tripping EnterGame
	// into ReplaceLogin against a dead gate. See
	// docs/design/stress-test-2026-05-http-login.md §四 #B-1.
	const auto loginNode = PickRandomNode(eNodeType::LoginNodeService);
	if (loginNode)
	{
		loginpb::LoginNodeDisconnectRequest request;
		request.set_session_id(sessionId);
		SessionDetails sessionDetails;
		sessionDetails.set_session_id(sessionId);
		sessionDetails.set_gate_node_id(gNode->GetNodeId());
		sessionDetails.set_gate_instance_id(gNode->GetNodeInfo().node_uuid());
		if (sessionFound)
		{
			sessionDetails.set_player_id(sessionIt->second.playerId);
		}
		loginpb::SendClientPlayerLoginDisconnect(tlsNodeContextManager.GetRegistry(eNodeType::LoginNodeService), *loginNode, request, {kSessionBinMetaKey}, SerializeSessionDetails(sessionDetails));
	}

	sessions.erase(sessionId);
	// 会话没了,battle_id 绑定记录一并清理(战斗侧照打,重连由 scene 重发 Bind)。
	gate_battle_binding::ClearBattleRecord(sessionId);

	LOG_INFO << "Client disconnected, session_id=" << sessionId
			 << ", peer=" << peer
			 << ", session_found=" << sessionFound
			 << ", scene_node_id=" << sceneNodeId
			 << ", scene_notified=" << sceneNotified
			 << ", remaining_sessions=" << sessions.size();
}

// High-water-mark: output buffer exceeded threshold — client not consuming (disconnect/cheat/slow), force close.
static constexpr size_t kClientHighWaterMark = 2 * 1024 * 1024; // 2MB

static void OnClientHighWaterMark(const muduo::net::TcpConnectionPtr &conn, size_t oldLen)
{
	const auto sessionId = RpcClientSessionHandler::GetSessionId(conn);
	LOG_WARN << "Client high water mark triggered, session_id=" << sessionId
			 << ", buffered=" << oldLen
			 << " bytes, threshold=" << kClientHighWaterMark
			 << " bytes, forcing close";
	conn->forceClose();
}

void RpcClientSessionHandler::HandleConnectionEstablished(const muduo::net::TcpConnectionPtr &conn)
{
	// fail-closed:node 段没种好就绝不发号。带 node 段 0 的 session_id 是"坏号" ——
	// scene 侧 GetGateNodeId() 得到 0、永远解析不出归属 gate,玩家整局静默不可用,
	// 且无任何自愈路径。宁可当场拒连让客户端重连,也不要放一个坏号进系统。
	// 正常路径下 main.cpp 已在装 connection 回调之前 set_node_id,这里是纵深防御。
	if (tlsSessionManager.session_id_gen().node_id_prefix() == 0)
	{
		LOG_ERROR << "Rejecting connection: session id generator has no node id yet, peer="
				  << conn->peerAddress().toIpPort();
		conn->forceClose();
		return;
	}

	auto sessionId = tlsSessionManager.session_id_gen().Generate();
	while (tlsSessionManager.sessions().find(sessionId) != tlsSessionManager.sessions().end())
	{
		sessionId = tlsSessionManager.session_id_gen().Generate();
	}

	// Session ID prevents packet tampering / message misdirection

	conn->setContext(sessionId);
	conn->setHighWaterMarkCallback(OnClientHighWaterMark, kClientHighWaterMark);

	SessionInfo session;
	session.conn = conn;

	// Dev mode: if no gate_token_secret configured, auto-verify all connections
	const auto &secret = gNodeConfigManager.GetBaseDeployConfig().gate_token_secret();
	if (secret.empty())
	{
		session.verified = true;
	}

	tlsSessionManager.sessions().emplace(sessionId, std::move(session));

	const std::string peer = conn ? conn->peerAddress().toIpPort() : std::string{"<null>"};
	LOG_INFO << "Client connected, session_id=" << sessionId
			 << ", peer=" << peer
			 << ", total_sessions=" << tlsSessionManager.sessions().size();
}

// Handle messages related to the game node
void HandleTcpNodeMessage(const SessionInfo &session, const RpcClientMessagePtr &request, SessionId sessionId, const muduo::net::TcpConnectionPtr &conn)
{
	assert(request->message_id() < gRpcMethodRegistry.size());
	auto &handlerMeta = gRpcMethodRegistry[request->message_id()];

	// Player sent message without being logged in — invalid node binding
	entt::entity targetNodeEntity = entt::entity{GetEffectiveNodeId(session, handlerMeta.targetNodeType)};
	auto &registry = tlsNodeContextManager.GetRegistry(handlerMeta.targetNodeType);
	if (!registry.valid(targetNodeEntity))
	{
		LOG_WARN << "[TCP Node] Scene not ready, dropping message_id: " << request->message_id()
				 << ", session_id: " << sessionId
				 << ", node_type: " << handlerMeta.targetNodeType;

		RpcClientSessionHandler::SendTipToClient(conn, kServiceUnavailable);
		return;
	}

	// 跨实体查询用 try_get 不用 get(CLAUDE.md §7 不变量 5)。
	// registry.valid() 只保证实体活着,不保证 RpcClientPtr 已经挂上去:
	// 节点实体在握手/发现阶段就被 create() 出来,RpcClientPtr 是随后才 emplace 的,
	// 而且组件在但 shared_ptr 为空同样可能(连接已断开正在重连)。
	// 这两种情况下旧写法分别是 get 抛异常和空指针解引用 —— 都是把 gate 打崩,
	// 而 gate 崩 = 该节点上所有在线玩家一起掉线。
	// 本文件 333 行、gate_service_handler.cpp:143 已经是 try_get + 判空的写法。
	const auto *tcpNode = registry.try_get<RpcClientPtr>(targetNodeEntity);
	if (tcpNode == nullptr || !*tcpNode)
	{
		LOG_WARN << "[TCP Node] RpcClient not attached, dropping message_id: " << request->message_id()
				 << ", session_id: " << sessionId
				 << ", node_type: " << handlerMeta.targetNodeType;

		RpcClientSessionHandler::SendTipToClient(conn, kServiceUnavailable);
		return;
	}

	ProcessClientPlayerMessageRequest message;
	message.mutable_message_content()->set_serialized_message(request->body());
	message.set_session_id(sessionId);
	message.mutable_message_content()->set_id(request->id());
	message.mutable_message_content()->set_message_id(request->message_id());
	(*tcpNode)->CallRemoteMethod(SceneProcessClientPlayerMessageMessageId, message);

	LOG_TRACE << "Sent message to game node, session id: " << sessionId << ", message id: " << request->message_id();
}

void HandleGrpcNodeMessage(SessionId sessionId, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn)
{
	assert(request->message_id() < gRpcMethodRegistry.size());
	auto &rpcHandlerMeta = gRpcMethodRegistry[request->message_id()];
	if (!ParseMessageFromRequestBody(*rpcHandlerMeta.requestProto, request, sessionId))
	{
		// 坏包不转发。共享原型已被清空,不会把残留数据递给后端。
		RpcClientSessionHandler::SendTipToClient(conn, kRequestMessageParseError);
		return;
	}

	SessionDetails sessionDetails;
	sessionDetails.set_session_id(sessionId);
	const auto sessionIt = tlsSessionManager.sessions().find(sessionId);
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LOG_ERROR << "Session not found for session id: " << sessionId;
		return;
	}
	sessionDetails.set_player_id(sessionIt->second.playerId);
	sessionDetails.set_gate_node_id(gNode->GetNodeId());
	sessionDetails.set_gate_instance_id(gNode->GetNodeInfo().node_uuid());

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
		//   1. 会话对目标 nodeType 已有绑定(SessionInfo.SetEntityId 过)→ 一律走绑定实体;
		//   2. 无绑定时,有状态节点(Scene / Battle)不能随机路由 —— Scene 的玩家实体、
		//      Battle 的战斗房间都只在特定节点上,随机挑一个只会得到"玩家/战斗不存在";
		//      ResolveSessionTargetNode 对无绑定会话返回 nullopt,统一落到下面的错误应答;
		//   3. 其余 nodeType(login/guild/friend/chat 等无状态 Go 服务)维持 PickRandomNode 现状。
		// Battle 的绑定由 BindBattleEvent/UnbindBattleEvent 维护(battle_binding_helper.cpp)。
		std::optional<entt::entity> node;
		const bool requiresSessionBinding =
			rpcHandlerMeta.targetNodeType == eNodeType::SceneNodeService ||
			rpcHandlerMeta.targetNodeType == eNodeType::BattleNodeService;
		if (requiresSessionBinding || sessionIt->second.HasEntityId(rpcHandlerMeta.targetNodeType))
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
			RpcClientSessionHandler::SendTipToClient(conn, kServiceUnavailable);
			return;
		}

		rpcHandlerMeta.sender(tlsNodeContextManager.GetRegistry(rpcHandlerMeta.targetNodeType),
							  *node,
							  *rpcHandlerMeta.requestProto,
							  {kSessionBinMetaKey},
							  SerializeSessionDetails(sessionDetails));
	}
}

bool RpcClientSessionHandler::ValidateClientMessage(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const
{
	if (!CheckMessageSize(session, request, conn))
		return false;
	if (!CheckMessageLimit(session, request, conn))
		return false;
	return true;
}

// Main request handler, forwards the request to the appropriate service
void RpcClientSessionHandler::DispatchClientRpcMessage(const muduo::net::TcpConnectionPtr &conn,
													   const RpcClientMessagePtr &request,
													   muduo::Timestamp)
{
	auto sessionId = GetSessionId(conn);
	const auto sessionIt = tlsSessionManager.sessions().find(sessionId);
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LOG_ERROR << "[Invalid Session] No session found for conn session_id: " << sessionId
				  << ", message_id: " << request->message_id();
		return;
	}

	auto &session = sessionIt->second;

	// Reject all game messages until the client passes token verification.
	// If gate_token_secret is empty (dev mode), all sessions are auto-verified on connect.
	if (!session.verified)
	{
		LOG_WARN << "[Token] Unverified session rejected message_id: " << request->message_id()
				 << ", session_id: " << sessionId;
		conn->shutdown();
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

	auto &messageInfo = gRpcMethodRegistry[request->message_id()];
	if (messageInfo.protocol == PROTOCOL_TCP)
	{
		HandleTcpNodeMessage(session, request, sessionId, conn);
	}
	else if (messageInfo.protocol == PROTOCOL_GRPC)
	{
		HandleGrpcNodeMessage(sessionId, request, conn);
	}
}

void RpcClientSessionHandler::DispatchTokenVerify(const muduo::net::TcpConnectionPtr &conn,
												  const ClientTokenVerifyRequestPtr &message,
												  muduo::Timestamp)
{
	auto sessionId = GetSessionId(conn);
	const auto sessionIt = tlsSessionManager.sessions().find(sessionId);
	if (sessionIt == tlsSessionManager.sessions().end())
	{
		LOG_ERROR << "[Token] No session found for session_id: " << sessionId;
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

	if (session.verified)
	{
		sendReply(true, "");
		return;
	}

	const auto &secret = gNodeConfigManager.GetBaseDeployConfig().gate_token_secret();
	if (secret.empty())
	{
		// Dev mode: no secret configured, accept all
		session.verified = true;
		sendReply(true, "");
		return;
	}

	// Recompute HMAC-SHA256(secret, payload) and compare with client-provided signature (hex)
	const auto &payloadBytes = message->payload();
	const auto &clientSig = message->signature();

	auto expectedHex = HmacSha256Hex(secret, payloadBytes);
	std::string clientSigStr(clientSig.begin(), clientSig.end());

	// 常数时间比较。std::string 的 != 在首个不匹配字节处提前返回,
	// 攻击者可用计时差逐字节猜签名。长度不等可以直接拒(长度不是秘密)。
	const bool sigMatches =
		expectedHex.size() == clientSigStr.size() && !expectedHex.empty() &&
		CRYPTO_memcmp(expectedHex.data(), clientSigStr.data(), expectedHex.size()) == 0;
	if (!sigMatches)
	{
		LOG_WARN << "[Token] HMAC mismatch for session_id: " << sessionId;
		sendReply(false, "invalid token signature");
		conn->shutdown();
		return;
	}

	// Deserialize payload and validate fields
	GateTokenPayload payload;
	if (!payload.ParseFromString(payloadBytes))
	{
		LOG_WARN << "[Token] Failed to parse token payload for session_id: " << sessionId;
		sendReply(false, "malformed token payload");
		conn->shutdown();
		return;
	}

	// Check gate_node_id matches this gate
	if (payload.gate_node_id() != gNode->GetNodeId())
	{
		LOG_WARN << "[Token] gate_node_id mismatch: token=" << payload.gate_node_id()
				 << " self=" << gNode->GetNodeId() << " session_id: " << sessionId;
		sendReply(false, "token not for this gate");
		conn->shutdown();
		return;
	}

	// Check expiry
	auto now = static_cast<int64_t>(std::time(nullptr));
	if (payload.expire_timestamp() <= now)
	{
		LOG_WARN << "[Token] Expired token for session_id: " << sessionId
				 << " expire=" << payload.expire_timestamp() << " now=" << now;
		sendReply(false, "token expired");
		conn->shutdown();
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

	session.verified = true;
	sendReply(true, "");
	LOG_DEBUG << "[Token] Session verified, session_id: " << sessionId
			  << " hmac_key_len=" << session.hmacSessionKey.size();
}

void RpcClientSessionHandler::OnNodeRemoveEventHandler(const OnNodeRemoveEvent &pb)
{
	auto &registry = tlsNodeContextManager.GetRegistry(pb.node_type());
	for (auto &session : tlsSessionManager.sessions())
	{
		if (session.second.GetEntityId(pb.node_type()) != pb.entity())
			continue;
		session.second.SetEntityId(pb.node_type(), SessionInfo::kInvalidEntityId);
		// battle 节点被摘除 = 该节点上的战斗全部作废(节点无持久状态,设计文档 §8),
		// 同步清掉 battle_id 记录,避免迟到的 UnbindBattleEvent 去匹配一条僵尸记录;
		// 玩家解冻由 scene reaper 按 InBattleComp.deadline_ms 兜底。
		if (pb.node_type() == eNodeType::BattleNodeService)
		{
			gate_battle_binding::ClearBattleRecord(session.first);
		}
	}
}
