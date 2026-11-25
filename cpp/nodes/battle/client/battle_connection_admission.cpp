#include "battle_connection_admission.h"

#include <random>

#include "network/codec/websocket.h"

namespace battle_connection_admission
{

namespace
{

// 按概率早拒用的伪随机数,[0, bound) 均匀。不参与任何安全判定:早拒对谁都一样,猜中序列也只知道下一条会不会被早拒。
uint32_t RandomBelow(const uint32_t bound)
{
    thread_local std::minstd_rand engine{std::random_device{}()};
    return std::uniform_int_distribution<uint32_t>(0, bound - 1)(engine);
}

// 早拒用的随机源;单测经 SetEarlyDropRandomForTesting 换成定值。不放进 Admission 的成员:类不放指针成员(函数指针也算)。
thread_local UniformBelow tlsUniformBelow = &RandomBelow;

} // namespace

void SetEarlyDropRandomForTesting(const UniformBelow uniformBelow)
{
    tlsUniformBelow = uniformBelow != nullptr ? uniformBelow : &RandomBelow;
}

Verdict Admission::Admit(const muduo::net::InetAddress &peer, const size_t currentConnections,
                         const size_t maxConnections, const uint64_t unauthenticated,
                         const std::optional<SteadyTime::duration> oldestUnauthenticatedAge, const SteadyTime now)
{
    // 按来源限速:开启且承压时才问这条直连所在前缀的桶;非公网来源(代理、边缘 nginx、集群内)认不出真实玩家,不按来源限。
    sourceRateActive = sourceRate.has_value() &&
                        accept_admission::SourceRateUnderPressure(unauthenticated, preAuthBudget, lastAcceptRateRejectAt, now);
    const std::optional<SourcePrefix> source =
        sourceRateActive ? accept_admission::RateLimitedSourceOf(peer) : std::nullopt;
    const AdmissionState state{
        .sourceClosed = sourceClosed,
        .currentConnections = currentConnections,
        .maxConnections = maxConnections,
        .unauthenticated = unauthenticated,
        .oldestUnauthenticatedAge = oldestUnauthenticatedAge,
        .preAuthBudget = preAuthBudget,
        .sourceOverRate = source && !sourceRate->WouldAdmit(*source, now),
    };
    Verdict verdict = Decide(state, rateGate, now, tlsUniformBelow);
    if (verdict.rejected)
    {
        if (*verdict.rejected == RejectReason::kAcceptRate)
        {
            lastAcceptRateRejectAt = now;
        }
        ++counters.rejected[static_cast<size_t>(*verdict.rejected)];
        return verdict;
    }
    // 所有闸都放行了,这时才给来源前缀记一笔:被别的闸拒掉的直连不消耗这个前缀的名额。
    if (source && !sourceRate->Take(*source, now))
    {
        ++counters.sourceUntracked;
        verdict.sourceUntracked = true;
    }
    return verdict;
}

void Admission::NotePreAuthEvicted()
{
    ++counters.preAuthEvicted;
}

void Admission::NoteRejectedAfterAccept(const RejectReason reason)
{
    ++counters.rejected[static_cast<size_t>(reason)];
}

void Admission::NoteAccepted()
{
    ++counters.accepted;
}

void Admission::NoteHandshake(const SteadyTime openedAt, const SteadyTime verifiedAt)
{
    handshake.Observe(openedAt, verifiedAt);
    ++counters.handshakes;
}

void Admission::NoteHandshakeTimeout()
{
    ++counters.handshakeTimeouts;
}

void Admission::CollectMetrics(prometheus_text::Writer &writer, const size_t currentConnections,
                               const size_t maxConnections, const size_t unauthenticatedConnections) const
{
    using prometheus_text::MetricType;
    const RateLimit &limit = rateGate.Limit();

    writer.BeginFamily("battle_connections_current",
                       "Client direct-connection sessions currently held by this battle node, one per admitted connection.",
                       MetricType::kGauge);
    writer.Sample({}, currentConnections);

    writer.BeginFamily("battle_connections_max",
                       "Direct-connection cap in effect: BattleMaxConnections, or the hard cap when it is 0 (dev/test only).",
                       MetricType::kGauge);
    writer.Sample({}, maxConnections);

    writer.BeginFamily("battle_unauthenticated_connections",
                       "Direct connections that have presented neither a valid battle ticket nor a valid network-probe "
                       "grant yet. High and not turning over means connections are holding slots without authenticating.",
                       MetricType::kGauge);
    writer.Sample({}, unauthenticatedConnections);

    writer.BeginFamily("battle_accepted_total",
                       "Direct connections admitted since process start (a session was created). "
                       "rate() of this series is the accept rate.",
                       MetricType::kCounter);
    writer.Sample({}, counters.accepted);

    writer.BeginFamily("battle_rejected_total",
                       "Direct connections refused since process start. capacity / accept_rate / shutting_down / "
                       "preauth_budget / preauth_early_drop are refused right after accept() with a TCP reset, before any "
                       "per-connection state is built; no_token_secret is a startup misconfiguration backstop.",
                       MetricType::kCounter);
    for (size_t reason = 0; reason < kRejectReasonCount; ++reason)
    {
        writer.Sample({{"reason", kRejectReasonNames[reason]}}, counters.rejected[reason]);
    }

    writer.BeginFamily("battle_accept_rate_limit_per_second",
                       "Sustained new-connection rate the accept rate gate allows.", MetricType::kGauge);
    writer.Sample({}, limit.perSecond);

    writer.BeginFamily("battle_accept_burst",
                       "New connections the accept rate gate allows at once when its bucket is full.", MetricType::kGauge);
    writer.Sample({}, limit.burst);

    writer.BeginFamily("battle_handshake_duration_ms",
                       "Time from connection accept to a successful battle ticket handshake, in milliseconds.",
                       MetricType::kHistogram);
    writer.Histogram({}, accept_admission::kHandshakeUpperBoundsMs, handshake.buckets, handshake.sumMs);

    writer.BeginFamily("battle_preauth_reaped_total",
                       "Direct connections closed because they had not completed the ticket handshake when the handshake "
                       "deadline expired.",
                       MetricType::kCounter);
    writer.Sample({}, counters.handshakeTimeouts);

    writer.BeginFamily("battle_preauth_evicted_total",
                       "Unauthenticated direct connections closed to admit a new one while the unauthenticated pool was "
                       "under pressure (the oldest goes first, and only once it has been open for the minimum evictable age).",
                       MetricType::kCounter);
    writer.Sample({}, counters.preAuthEvicted);

    writer.BeginFamily("battle_preauth_budget",
                       "Unauthenticated direct connections allowed at most when none of them is old enough to be evicted.",
                       MetricType::kGauge);
    writer.Sample({}, preAuthBudget.full);

    writer.BeginFamily("battle_preauth_early_drop_start",
                       "Unauthenticated direct connections at which the pool counts as under pressure: the oldest are "
                       "evicted for newcomers, or newcomers are refused with a probability rising linearly to 1 at the budget.",
                       MetricType::kGauge);
    writer.Sample({}, preAuthBudget.earlyDropStart);

    writer.BeginFamily("battle_preauth_evictable_age_ms",
                       "How long an unauthenticated direct connection must have been open before it can be evicted.",
                       MetricType::kGauge);
    writer.Sample({}, static_cast<uint64_t>(preAuthBudget.minEvictableAge.count()));

    const std::optional<RateLimit> sourceLimit = CurrentSourceRateLimit();
    writer.BeginFamily("battle_source_rate_limit_per_second",
                       "Sustained new-connection rate allowed per public source prefix (IPv4 /24, IPv6 /64) while the "
                       "battle node is under pressure; 0 when per-source limiting is off.",
                       MetricType::kGauge);
    writer.Sample({}, sourceLimit ? sourceLimit->perSecond : 0);

    writer.BeginFamily("battle_source_burst",
                       "New direct connections one source prefix may open at once while under pressure; 0 when off.",
                       MetricType::kGauge);
    writer.Sample({}, sourceLimit ? sourceLimit->burst : 0);

    writer.BeginFamily("battle_source_rate_active",
                       "1 when per-source limiting applied to the most recent admission decision (enabled and under "
                       "pressure), else 0.",
                       MetricType::kGauge);
    writer.Sample({}, sourceRateActive ? 1 : 0);

    writer.BeginFamily("battle_source_tracked", "Source prefixes currently tracked by the per-source rate gate.",
                       MetricType::kGauge);
    writer.Sample({}, sourceRate ? sourceRate->Tracked() : 0);

    writer.BeginFamily("battle_source_untracked_total",
                       "Direct connections admitted without per-source accounting because the source prefix table was full.",
                       MetricType::kCounter);
    writer.Sample({}, counters.sourceUntracked);

    // 小游戏直连(WebSocket)的握手与协议错误(network/codec/websocket.h),与 gate 的两族同义。计数是本线程累计的,
    // 直连都在 battle 主 loop 上;连接数已经算在上面的 battle_connections_current 里。
    const websocket::Counters &webSocket = websocket::GetCounters();
    writer.BeginFamily("battle_websocket_handshakes_total",
                       "WebSocket upgrade requests on the direct port since process start (mini-game clients).",
                       MetricType::kCounter);
    writer.Sample({{"result", "accepted"}}, webSocket.accepted);
    writer.Sample({{"result", "rejected"}}, webSocket.rejected);

    writer.BeginFamily("battle_websocket_protocol_closed_total",
                       "WebSocket connections closed by this battle node because a frame violated the protocol "
                       "(text frame, unmasked frame, oversized frame, bad fragmentation).",
                       MetricType::kCounter);
    writer.Sample({}, webSocket.protocolClosed);
}

} // namespace battle_connection_admission
