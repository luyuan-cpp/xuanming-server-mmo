#include "client_message_processor.h"

#include <algorithm>
#include <cassert>
#include <functional>
#include <memory>
#include <unordered_map>
#include <optional>
#include <sstream>
#include <string_view>

#include "gate_codec.h"
#include "gate_security.h"
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
		const auto loginNode = PickRandomNode(eNodeType::LoginNodeService);
		if (loginNode)
		{
			loginpb::LoginNodeDisconnectRequest request;
			request.set_session_id(sessionId);
			SessionDetails sessionDetails;
			sessionDetails.set_session_id(sessionId);
			sessionDetails.set_gate_node_id(gNode->GetNodeId());
			sessionDetails.set_gate_instance_id(gNode->GetNodeInfo().node_uuid());
			if (hasBoundPlayer)
			{
				sessionDetails.set_player_id(playerId);
			}
			loginpb::SendClientPlayerLoginDisconnect(tlsNodeContextManager.GetRegistry(eNodeType::LoginNodeService), *loginNode, request, {kSessionBinMetaKey}, SerializeSessionDetails(sessionDetails));
		}
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
	const auto sessionId = RpcClientSessionHandler::GetSessionId(conn);
	LOG_WARN << "Client high water mark triggered, session_id=" << sessionId
			 << ", buffered=" << oldLen
			 << " bytes, threshold=" << kClientHighWaterMark
			 << " bytes, forcing close";
	conn->forceClose();
}

void RpcClientSessionHandler::HandleConnectionEstablished(const muduo::net::TcpConnectionPtr &conn)
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
		// Scene nodes hold player entities in memory -- require session affinity binding.
		// All Go microservices (login, guild, friend, chat, etc.) are stateless -- pick any available node.
		std::optional<entt::entity> node;
		if (rpcHandlerMeta.targetNodeType == eNodeType::SceneNodeService)
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

		// 断开时只有确实走到过 Login.Login 的会话才需要清理 Login 的
		// session-id 状态。标记必须放在选到节点之后、实际 dispatch 之前。
		if (request->message_id() == ClientPlayerLoginLoginMessageId)
		{
			sessionIt->second.loginStarted = true;
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
	}
}
