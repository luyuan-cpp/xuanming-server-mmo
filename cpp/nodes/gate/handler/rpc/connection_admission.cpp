#include "connection_admission.h"

#include <memory>
#include <utility>

#include "client_heartbeat.h"
#include "metrics/metrics_registry.h"
#include "muduo/base/Logging.h"
#include "muduo/net/InetAddress.h"
#include "session/manager/session_manager.h"

namespace gate_connection_admission
{

namespace
{

// 本线程的准入状态。Configure 之前(单测、启动早期)是缺省值:上限取 session id 空间,速率闸按它推导。
struct State
{
	bool sourceClosed{false};
	uint32_t maxConnections{EffectiveMaxConnections(0)};
	AcceptRateGate rateGate{DefaultRateLimit(EffectiveMaxConnections(0))};
	Counters counters;
	// 握手耗时直方图:各桶自己的计数(不累计),最后一个是超过最大上界的部分;耗时之和(毫秒)。
	std::array<uint64_t, kHandshakeBucketCount> handshakeBuckets{};
	uint64_t handshakeSumMs{0};
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
	case RejectReason::kShuttingDown:
	case RejectReason::kNotReady:
	case RejectReason::kNoTokenSecret:
	case RejectReason::kCount:
		// 关源期间的拒绝是预期行为,只进指标;后两种只在连接回调的纵深防御分支出现,那里自己打日志。
		break;
	}
}

uint64_t ElapsedMs(const SteadyTime from, const SteadyTime to)
{
	if (to <= from)
	{
		return 0; // 时刻由调用方给;倒退时按 0 记,不让一个负数变成巨大的无符号值
	}
	return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(to - from).count());
}

} // namespace

void Configure(const uint32_t configuredMaxConnections, const RateLimit rateLimit)
{
	tlsState.maxConnections = EffectiveMaxConnections(configuredMaxConnections);
	tlsState.rateGate.Reset(rateLimit);
}

void CloseSource()
{
	tlsState.sourceClosed = true;
}

bool AdmitConnection(const muduo::net::InetAddress &peer, const SteadyTime now)
{
	const AdmissionState state{
		.sourceClosed = tlsState.sourceClosed,
		.currentSessions = tlsSessionManager.sessions().size(),
		.maxConnections = tlsState.maxConnections,
	};
	const auto rejected = Decide(state, tlsState.rateGate, now);
	if (!rejected)
	{
		return true;
	}
	const uint64_t total = ++tlsState.counters.rejected[static_cast<size_t>(*rejected)];
	LogRejectionSampled(*rejected, total, peer);
	return false;
}

void NoteRejectedAfterAccept(const RejectReason reason)
{
	++tlsState.counters.rejected[static_cast<size_t>(reason)];
}

void OnSessionOpened(SessionInfo &session, const SteadyTime now)
{
	++tlsState.counters.accepted;
	if (!session.verified)
	{
		session.preAuthSlot = std::make_shared<PreAuthSlot>(tlsUnauthenticated, now);
	}
}

void OnSessionVerified(SessionInfo &session, const SteadyTime now)
{
	// 把占位从会话上拿下来;函数返回时最后一份引用销毁,计数随之减一。
	const std::shared_ptr<PreAuthSlot> slot = std::exchange(session.preAuthSlot, nullptr);
	if (!slot)
	{
		return;
	}
	const uint64_t elapsedMs = ElapsedMs(slot->OpenedAt(), now);
	++tlsState.handshakeBuckets[HandshakeBucketIndex(elapsedMs)];
	tlsState.handshakeSumMs += elapsedMs;
	++tlsState.counters.handshakes;
}

const Counters &GetCounters()
{
	return tlsState.counters;
}

uint64_t UnauthenticatedConnections()
{
	return tlsUnauthenticated;
}

RateLimit CurrentRateLimit()
{
	return tlsState.rateGate.Limit();
}

void CollectMetrics(prometheus_text::Writer &writer)
{
	using prometheus_text::MetricType;
	const State &state = tlsState;
	const RateLimit &limit = state.rateGate.Limit();

	writer.BeginFamily("gate_connections_current",
					   "Client sessions currently held by this gate, one per admitted client connection.",
					   MetricType::kGauge);
	writer.Sample({}, tlsSessionManager.sessions().size());

	writer.BeginFamily("gate_connections_max",
					   "Connection cap in effect: GateMaxConnections, or the session id space when it is 0 (dev/test only).",
					   MetricType::kGauge);
	writer.Sample({}, state.maxConnections);

	writer.BeginFamily("gate_unauthenticated_connections",
					   "Client connections that have not passed token verification yet. High and not turning over "
					   "means connections are holding slots without authenticating.",
					   MetricType::kGauge);
	writer.Sample({}, tlsUnauthenticated);

	writer.BeginFamily("gate_accepted_total",
					   "Client connections admitted since process start (a session was created). "
					   "rate() of this series is the accept rate.",
					   MetricType::kCounter);
	writer.Sample({}, state.counters.accepted);

	writer.BeginFamily("gate_rejected_total",
					   "Client connections refused since process start. capacity / accept_rate / shutting_down are "
					   "refused right after accept() with a TCP reset, before any per-connection state is built; "
					   "not_ready / no_token_secret are startup misconfiguration backstops.",
					   MetricType::kCounter);
	for (size_t reason = 0; reason < kRejectReasonCount; ++reason)
	{
		writer.Sample({{"reason", kRejectReasonNames[reason]}}, state.counters.rejected[reason]);
	}

	writer.BeginFamily("gate_accept_rate_limit_per_second",
					   "Sustained new-connection rate the accept rate gate allows.", MetricType::kGauge);
	writer.Sample({}, limit.perSecond);

	writer.BeginFamily("gate_accept_burst", "New connections the accept rate gate allows at once when its bucket is full.",
					   MetricType::kGauge);
	writer.Sample({}, limit.burst);

	writer.BeginFamily("gate_handshake_duration_ms",
					   "Time from connection accept to successful token verification, in milliseconds.",
					   MetricType::kHistogram);
	writer.Histogram({}, kHandshakeUpperBoundsMs, state.handshakeBuckets, state.handshakeSumMs);

	writer.BeginFamily("gate_preauth_reaped_total",
					   "Connections closed because they had not passed token verification when the handshake deadline expired.",
					   MetricType::kCounter);
	writer.Sample({}, gate_client_heartbeat::GetCounters().handshakeTimeouts);
}

void RegisterMetrics()
{
	if (!GlobalMetricsRegistry().Register("gate_connection_admission", &CollectMetrics))
	{
		LOG_ERROR << "[Metrics] collector gate_connection_admission is already registered";
	}
}

void ResetForTesting()
{
	tlsState = State{};
}

} // namespace gate_connection_admission
