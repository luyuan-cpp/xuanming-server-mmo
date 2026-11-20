#include "connection_admission.h"

#include <deque>
#include <memory>
#include <random>
#include <utility>

#include "client_heartbeat.h"
#include "metrics/metrics_registry.h"
#include "muduo/base/Logging.h"
#include "muduo/net/InetAddress.h"
#include "network/codec/websocket.h"
#include "session/manager/session_manager.h"

namespace gate_connection_admission
{

namespace
{

// 按概率早拒用的伪随机数,[0, bound) 均匀。不参与任何安全判定:早拒对谁都一样,猜中序列也只知道下一条会不会被早拒。
uint32_t RandomBelow(const uint32_t bound)
{
	thread_local std::minstd_rand engine{std::random_device{}()};
	return std::uniform_int_distribution<uint32_t>(0, bound - 1)(engine);
}

// 淘汰索引的一条:一条领了未验签占位的会话。
struct PreAuthEntry
{
	SessionId sessionId;
	uint64_t incarnation;
};

// 本线程的准入状态。Configure 之前(单测、启动早期)是缺省值:上限取 session id 空间,速率闸与未验签预算按它推导。
struct State
{
	bool sourceClosed{false};
	uint32_t maxConnections{EffectiveMaxConnections(0)};
	AcceptRateGate rateGate{DefaultRateLimit(EffectiveMaxConnections(0))};
	PreAuthBudget preAuthBudget{DefaultPreAuthBudget(EffectiveMaxConnections(0))};
	// 未验签连接按建连先后排的淘汰索引。入队时刻就是建连时刻(单调时钟、只在 loop 线程上取),所以队头最老。
	// 验签、断开、被淘汰之后条目不删,轮到它在队头时按 (session_id, 会话代次) 核对后丢弃(与 client_liveness.h 的
	// 检查点堆同一做法),每条只进出一次。队头的有效条目最多活到握手期限,所以队列只装着最近一个握手期限左右建的未验签连接。
	std::deque<PreAuthEntry> preAuthOrder;
	UniformBelow uniformBelow{&RandomBelow};
	Counters counters;
	// 握手耗时直方图(建连到验签成功,毫秒)。
	HandshakeHistogram handshake;
};
thread_local State tlsState;

// 当前持有未验签占位的连接数。单独放、且只是一个整数:PreAuthSlot 析构时要减它,而会话表(另一个 thread_local)
// 在线程退出时才清,那时这里必须仍然可写 —— 平凡类型的 thread_local 没有析构,存储一直有效。
// ResetForTesting 不清它:仍被会话持有的占位之后还会来减。
thread_local uint64_t tlsUnauthenticated = 0;

// 采样日志:被拒的量由公网流量决定,第一条必打,此后每 1024 条一行。total 是该原因已累计的次数(含本次)。
// 对端地址只进日志,不进指标标签。
void LogRejectionSampled(const RejectReason reason, const uint64_t total, const muduo::net::InetAddress &peer)
{
	if ((total & 0x3FF) != 1)
	{
		return;
	}
	const RateLimit &limit = tlsState.rateGate.Limit();
	switch (reason)
	{
	case RejectReason::kCapacity:
		// 满员是运维必须看见的状态:login 在 90% 时就该停止往这台派人,走到这里说明分配没跟上或有人绕过了分配。
		LOG_ERROR << "Gate at connection capacity, rejecting new connections (sampled)."
				  << " max_connections=" << tlsState.maxConnections
				  << " current_sessions=" << tlsSessionManager.sessions().size()
				  << " rejected_total=" << total << " latest_peer=" << peer.toIpPort();
		break;
	case RejectReason::kAcceptRate:
		LOG_WARN << "Gate accept rate limit exceeded, rejecting new connections (sampled)."
				 << " rate_limit_per_second=" << limit.perSecond << " burst=" << limit.burst
				 << " current_sessions=" << tlsSessionManager.sessions().size()
				 << " unauthenticated=" << tlsUnauthenticated
				 << " rejected_total=" << total << " latest_peer=" << peer.toIpPort();
		break;
	case RejectReason::kPreAuthBudget:
	case RejectReason::kPreAuthEarlyDrop:
		// 未验签池承压,而池里还没有建连够久、可以让位的连接:多半是一次只建连不验签的突发。
		LOG_WARN << "Unauthenticated connection budget under pressure, rejecting new connections (sampled)."
				 << " reason=" << RejectReasonName(reason).data() << " unauthenticated=" << tlsUnauthenticated
				 << " early_drop_start=" << tlsState.preAuthBudget.earlyDropStart
				 << " preauth_budget=" << tlsState.preAuthBudget.full << " rejected_total=" << total
				 << " latest_peer=" << peer.toIpPort();
		break;
	case RejectReason::kShuttingDown:
	case RejectReason::kNotReady:
	case RejectReason::kNoTokenSecret:
	case RejectReason::kCount:
		// 关源期间的拒绝是预期行为,只进指标;后两种只在连接回调的纵深防御分支出现,那里自己打日志。
		break;
	}
}

// 淘汰索引从队头起逐条核对,失效的丢掉,返回最老的一条仍未验签的会话(没有为空);sessionId 不为空时写回它的会话号。
// 有效 = 会话还在、是同一代(session_id 没被新连接复用)、还没验签、还持有占位:已验签或已换人的会话不会被选中。
SessionInfo *OldestUnauthenticated(SessionId *sessionId)
{
	auto &order = tlsState.preAuthOrder;
	auto &sessions = tlsSessionManager.sessions();
	while (!order.empty())
	{
		const PreAuthEntry entry = order.front();
		const auto it = sessions.find(entry.sessionId);
		if (it != sessions.end() && it->second.incarnation == entry.incarnation && !it->second.verified &&
			it->second.preAuthSlot)
		{
			if (sessionId != nullptr)
			{
				*sessionId = entry.sessionId;
			}
			return &it->second;
		}
		order.pop_front();
	}
	return nullptr;
}

// 承压时给新连接腾位置。先收回最老那条未验签连接的占位:这一刻起它不再算在未验签池里。再经会话里的 weak 连接 forceClose:
// forceClose 只把连接置成 kDisconnecting、真正的关闭排到之后的 loop 轮次,随后照常走断开回调删会话(没验签的会话没有角色、
// 没向 login 发过登录,断开回调只删会话)。这里不在 muduo 的回调栈上同步摘会话。连接一进 kDisconnecting,codec 就不再
// 分发它的任何帧(network/codec/codec.cpp ProtobufCodec::onMessage),所以被选中之后它不会再验签成功,不会「刚验签就被关」。
void EvictUnauthenticated(const SessionId sessionId, SessionInfo &victim, const SteadyTime now)
{
	const auto ageMs =
		std::chrono::duration_cast<std::chrono::milliseconds>(now - victim.preAuthSlot->OpenedAt()).count();
	victim.preAuthSlot.reset();
	const uint64_t total = ++tlsState.counters.preAuthEvicted;
	const auto conn = victim.conn.lock();
	// 淘汰的量与只建连不验签的流量同一量级:第一条必打,此后每 1024 条一行。对端地址只进日志。
	if ((total & 0x3FF) == 1)
	{
		LOG_WARN << "Evicting the oldest unauthenticated connection to admit a new one (sampled)."
				 << " age_ms=" << ageMs << " unauthenticated=" << tlsUnauthenticated
				 << " early_drop_start=" << tlsState.preAuthBudget.earlyDropStart
				 << " preauth_budget=" << tlsState.preAuthBudget.full << " evicted_total=" << total
				 << " latest_session_id=" << sessionId
				 << " latest_peer=" << (conn ? conn->peerAddress().toIpPort() : std::string{"<none>"});
	}
	if (conn)
	{
		conn->forceClose();
	}
}

} // namespace

void Configure(const uint32_t configuredMaxConnections, const RateLimit rateLimit)
{
	tlsState.maxConnections = EffectiveMaxConnections(configuredMaxConnections);
	tlsState.rateGate.Reset(rateLimit);
	tlsState.preAuthBudget = DefaultPreAuthBudget(tlsState.maxConnections);
}

void CloseSource()
{
	tlsState.sourceClosed = true;
}

bool AdmitConnection(const muduo::net::InetAddress &peer, const SteadyTime now)
{
	const uint64_t unauthenticated = tlsUnauthenticated;
	// 最老的一条未验签连接只在池承压时用得上,也只在那时才找(找的时候顺手丢掉队头已失效的条目)。
	SessionId oldestId = kInvalidSessionId;
	SessionInfo *const oldest =
		unauthenticated >= tlsState.preAuthBudget.earlyDropStart ? OldestUnauthenticated(&oldestId) : nullptr;
	const AdmissionState state{
		.sourceClosed = tlsState.sourceClosed,
		.currentSessions = tlsSessionManager.sessions().size(),
		.maxConnections = tlsState.maxConnections,
		.unauthenticated = unauthenticated,
		.oldestUnauthenticatedAge = oldest != nullptr
										? std::optional<SteadyTime::duration>(now - oldest->preAuthSlot->Open