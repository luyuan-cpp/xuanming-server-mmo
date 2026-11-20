#include "connection_admission.h"

#include <deque>
#include <memory>
#include <random>
#include <utility>

#include "client_heartbeat.h"
#include "metrics/metrics_registry.h"
#include "muduo/base/Logging.h"
#include "muduo/net/Endian.h"
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

// 本线程的准入状态。Configure 之前(单测、启动早期)是缺省值:上限取 session id 空间,速率闸与未验签预算按它推导,
// 按来源限速关闭。
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
	// 按来源限速;空 = 关闭(缺省)。
	std::optional<SourceRateGate> sourceRate;
	// 全局速率闸最近一次拒绝的时刻:承压的判据之一(按来源限速在它之后一个握手期限内生效)。
	std::optional<SteadyTime> lastAcceptRateRejectAt;
	// 最近一次准入判定时按来源限速是否在生效(gate_source_rate_active)。
	bool sourceRateActive{false};
	Counters counters;
	// 握手耗时直方图(建连到验签成功,毫秒)。
	HandshakeHistogram handshake;
};
thread_local State tlsState;

// 按概率早拒用的随机源。单测经 SetEarlyDropRandomForTesting 换成定值;ResetForTesting 换回。
// 放在 State 外面:类与结构体不放指针成员(AGENTS.md §10.1 的 no-raw-pointer-member 门禁,函数指针也算)。
thread_local UniformBelow tlsUniformBelow = &RandomBelow;

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
	case RejectReason::kSourceRate:
		// 承压期间单个来源前缀建连太快。前缀只进日志,不进指标标签(基数由攻击者控制)。
		LOG_WARN << "Per-source accept rate exceeded while the gate is under pressure, rejecting (sampled)."
				 << " source_rate_per_second=" << (tlsState.sourceRate ? tlsState.sourceRate->Limit().perSecond : 0)
				 << " source_burst=" << (tlsState.sourceRate ? tlsState.sourceRate->Limit().burst : 0)
				 << " unauthenticated=" << tlsUnauthenticated << " rejected_total=" << total
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

// 承压:未验签池过了早拒起点,或全局速率闸在最近一个握手期限内拒过连接。按来源限速只在承压时生效(设计稿 §7「告警后自动开启」)。
bool UnderPressure(const uint64_t unauthenticated, const SteadyTime now)
{
	if (unauthenticated >= tlsState.preAuthBudget.earlyDropStart)
	{
		return true;
	}
	const auto &rejectedAt = tlsState.lastAcceptRateRejectAt;
	return rejectedAt && now - *rejectedAt < client_liveness::kClientLiveness.handshakeTimeout;
}

// 前缀表满、这个前缀没登记上就放行了(fail-open):计数,第一条必打,此后每 1024 条一行。
void NoteSourceUntracked(const muduo::net::InetAddress &peer)
{
	const uint64_t total = ++tlsState.counters.sourceUntracked;
	if ((total & 0x3FF) == 1)
	{
		LOG_WARN << "Per-source prefix table is full, admitting a connection without per-source accounting (sampled)."
				 << " tracked=" << (tlsState.sourceRate ? tlsState.sourceRate->Tracked() : 0)
				 << " max_tracked=" << kMaxTrackedSources << " untracked_total=" << total
				 << " latest_peer=" << peer.toIpPort();
	}
}

} // namespace

void Configure(const uint32_t configuredMaxConnections, const RateLimit rateLimit, const PreAuthBudget preAuthBudget,
			   const std::optional<RateLimit> sourceRateLimit)
{
	tlsState.maxConnections = EffectiveMaxConnections(configuredMaxConnections);
	tlsState.rateGate.Reset(rateLimit);
	tlsState.preAuthBudget = preAuthBudget;
	if (sourceRateLimit)
	{
		tlsState.sourceRate.emplace(*sourceRateLimit);
	}
	else
	{
		tlsState.sourceRate.reset();
	}
}

void Configure(const uint32_t configuredMaxConnections, const RateLimit rateLimit)
{
	Configure(configuredMaxConnections, rateLimit, DefaultPreAuthBudget(EffectiveMaxConnections(configuredMaxConnections)),
			  std::nullopt);
}

std::optional<SourcePrefix> SourcePrefixOf(const muduo::net::InetAddress &peer)
{
	const struct sockaddr *address = peer.getSockAddr();
	if (peer.family() == AF_INET)
	{
		const auto *v4 = reinterpret_cast<const struct sockaddr_in *>(address);
		return SourcePrefixOfIpv4(muduo::net::sockets::networkToHost32(v4->sin_addr.s_addr));
	}
	if (peer.family() == AF_INET6)
	{
		const auto *v6 = reinterpret_cast<const struct sockaddr_in6 *>(address);
		std::array<uint8_t, 16> bytes{};
		for (size_t i = 0; i < bytes.size(); ++i)
		{
			bytes[i] = static_cast<uint8_t>(v6->sin6_addr.s6_addr[i]);
		}
		return SourcePrefixOfIpv6(bytes);
	}
	return std::nullopt;
}

void CloseSource()
{
	tlsState.sourceClosed = true;
}

bool AdmitConnection(const muduo::net::InetAddress &peer, const SteadyTime now)
{
	const uint64_t unauthenticated = tlsUnauthenticated;
	// 最老的一条未验签连接只在池承压时用得上,也只在那时才找(找的时候顺手丢掉队头已失效的条目)。
	// 本端 loop 刚卡过就不找:那时「建连多久还没验签」不可信,验签请求可能还在接收缓冲里没读,不该据此淘汰。
	SessionId oldestId = kInvalidSessionId;
	SessionInfo *const oldest = unauthenticated >= tlsState.preAuthBudget.earlyDropStart &&
										!gate_client_heartbeat::LoopRecentlyStalled(now)
									? OldestUnauthenticated(&oldestId)
									: nullptr;
	// 按来源限速:开启且承压时才问这条连接所在前缀的桶。
	tlsState.sourceRateActive = tlsState.sourceRate.has_value() && UnderPressure(unauthenticated, now);
	const std::optional<SourcePrefix> prefix = tlsState.sourceRateActive ? SourcePrefixOf(peer) : std::nullopt;
	const AdmissionState state{
		.sourceClosed = tlsState.sourceClosed,
		.currentSessions = tlsSessionManager.sessions().size(),
		.maxConnections = tlsState.maxConnections,
		.unauthenticated = unauthenticated,
		.oldestUnauthenticatedAge = oldest != nullptr
										? std::optional<SteadyTime::duration>(now - oldest->preAuthSlot->OpenedAt())
										: std::nullopt,
		.preAuthBudget = tlsState.preAuthBudget,
		.sourceOverRate = prefix && !tlsState.sourceRate->WouldAdmit(*prefix, now),
	};
	const Admission admission = Decide(state, tlsState.rateGate, now, tlsUniformBelow);
	if (admission.rejected)
	{
		if (*admission.rejected == RejectReason::kAcceptRate)
		{
			tlsState.lastAcceptRateRejectAt = now;
		}
		const uint64_t total = ++tlsState.counters.rejected[static_cast<size_t>(*admission.rejected)];
		LogRejectionSampled(*admission.rejected, total, peer);
		return false;
	}
	// 所有闸都放行了,这时才给来源前缀记一笔:被别的闸拒掉的连接不消耗这个前缀的名额。
	if (prefix && !tlsState.sourceRate->Take(*prefix, now))
	{
		NoteSourceUntracked(peer);
	}
	// 判定只在找到了最老的一条时才会要求淘汰;这里再判一次空,不让一个空指针走进淘汰。
	if (admission.evictOldestUnauthenticated && oldest != nullptr)
	{
		EvictUnauthenticated(oldestId, *oldest, now);
	}
	return true;
}

void NoteRejectedAfterAccept(const RejectReason reason)
{
	++tlsState.counters.rejected[static_cast<size_t>(reason)];
}

void OnSessionOpened(const SessionId sessionId, SessionInfo &session, const SteadyTime now)
{
	++tlsState.counters.accepted;
	if (session.verified)
	{
		return;
	}
	session.preAuthSlot = std::make_shared<PreAuthSlot>(tlsUnauthenticated, now);
	tlsState.preAuthOrder.push_back(PreAuthEntry{sessionId, session.incarnation});
	// 顺手丢掉队头已失效的条目,让队列长度跟着在场的未验签连接走,不随累计建连数增长。
	OldestUnauthenticated(nullptr);
}

void OnSessionVerified(SessionInfo &session, const SteadyTime now)
{
	// 把占位从会话上拿下来;函数返回时最后一份引用销毁,计数随之减一。
	const std::shared_ptr<PreAuthSlot> slot = std::exchange(session.preAuthSlot, nullptr);
	if (!slot)
	{
		return;
	}
	tlsState.handshake.Observe(slot->OpenedAt(), now);
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

PreAuthBudget CurrentPreAuthBudget()
{
	return tlsState.preAuthBudget;
}

std::optional<RateLimit> CurrentSourceRateLimit()
{
	if (!tlsState.sourceRate)
	{
		return std::nullopt;
	}
	return tlsState.sourceRate->Limit();
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
					   "Client connections refused since process start. capacity / accept_rate / shutting_down / "
					   "preauth_budget / preauth_early_drop / source_rate are refused right after accept() with a TCP reset, "
					   "before any per-connection state is built; not_ready / no_token_secret are startup misconfiguration "
					   "backstops.",
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
	writer.Histogram({}, kHandshakeUpperBoundsMs, state.handshake.buckets, state.handshake.sumMs);

	writer.BeginFamily("gate_preauth_reaped_total",
					   "Connections closed because they had not passed token verification when the handshake deadline expired.",
					   MetricType::kCounter);
	writer.Sample({}, gate_client_heartbeat::GetCounters().handshakeTimeouts);

	writer.BeginFamily("gate_preauth_evicted_total",
					   "Unauthenticated connections closed to admit a new one while the unauthenticated pool was under "
					   "pressure (the oldest goes first, and only once it has been open for the minimum evictable age).",
					   MetricType::kCounter);
	writer.Sample({}, state.counters.preAuthEvicted);

	writer.BeginFamily("gate_preauth_budget",
					   "Unauthenticated connections allowed at most when none of them is old enough to be evicted.",
					   MetricType::kGauge);
	writer.Sample({}, state.preAuthBudget.full);

	writer.BeginFamily("gate_preauth_early_drop_start",
					   "Unauthenticated connections at which the pool counts as under pressure: the oldest are evicted "
					   "for newcomers, or newcomers are refused with a probability rising linearly to 1 at the budget.",
					   MetricType::kGauge);
	writer.Sample({}, state.preAuthBudget.earlyDropStart);

	writer.BeginFamily("gate_preauth_evictable_age_ms",
					   "How long an unauthenticated connection must have been open before it can be evicted for a newcomer.",
					   MetricType::kGauge);
	writer.Sample({}, static_cast<uint64_t>(state.preAuthBudget.minEvictableAge.count()));

	const std::optional<RateLimit> sourceLimit =
		state.sourceRate ? std::optional<RateLimit>(state.sourceRate->Limit()) : std::nullopt;
	writer.BeginFamily("gate_source_rate_limit_per_second",
					   "Sustained new-connection rate allowed per source prefix (IPv4 /24, IPv6 /64) while the gate is "
					   "under pressure; 0 when per-source limiting is off.",
					   MetricType::kGauge);
	writer.Sample({}, sourceLimit ? sourceLimit->perSecond : 0);

	writer.BeginFamily("gate_source_burst",
					   "New connections one source prefix may open at once while the gate is under pressure; 0 when off.",
					   MetricType::kGauge);
	writer.Sample({}, sourceLimit ? sourceLimit->burst : 0);

	writer.BeginFamily("gate_source_rate_active",
					   "1 when per-source limiting applied to the most recent admission decision (enabled and under "
					   "pressure), else 0.",
					   MetricType::kGauge);
	writer.Sample({}, state.sourceRateActive ? 1 : 0);

	writer.BeginFamily("gate_source_tracked", "Source prefixes currently tracked by the per-source rate gate.",
					   MetricType::kGauge);
	writer.Sample({}, state.sourceRate ? state.sourceRate->Tracked() : 0);

	writer.BeginFamily("gate_source_untracked_total",
					   "Connections admitted without per-source accounting because the source prefix table was full.",
					   MetricType::kCounter);
	writer.Sample({}, state.counters.sourceUntracked);

	// 小游戏连接(WebSocket)的握手与协议错误(network/codec/websocket.h)。连接数已经算在上面的 gate_connections_current 里。
	const websocket::Counters &webSocket = websocket::GetCounters();
	writer.BeginFamily("gate_websocket_handshakes_total",
					   "WebSocket upgrade requests on the client port since process start (mini-game clients).",
					   MetricType::kCounter);
	writer.Sample({{"result", "accepted"}}, webSocket.accepted);
	writer.Sample({{"result", "rejected"}}, webSocket.rejected);

	writer.BeginFamily("gate_websocket_protocol_closed_total",
					   "WebSocket connections closed by this gate because a frame violated the protocol "
					   "(text frame, unmasked frame, oversized frame, bad fragmentation).",
					   MetricType::kCounter);
	writer.Sample({}, webSocket.protocolClosed);
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
	tlsUniformBelow = &RandomBelow;
}

void SetEarlyDropRandomForTesting(const UniformBelow uniformBelow)
{
	tlsUniformBelow = uniformBelow != nullptr ? uniformBelow : &RandomBelow;
}

} // namespace gate_connection_admission
