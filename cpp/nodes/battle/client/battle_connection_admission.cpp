#include "battle_connection_admission.h"

#include <random>

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

Verdict Admission::Admit(const size_t currentConnections, const size_t maxConnections, const uint64_t unauthenticated,
                         const std::optional<SteadyTime::duration> oldestUnauthenticatedAge, const SteadyTime now)
{
    const AdmissionState state{
        .sourceClosed = sourceClosed_,
        .currentConnections = currentConnections,
        .maxConnections = maxConnections,
        .unauthenticated = unauthenticated,
        .oldestUnauthenticatedAge = oldestUnauthenticatedAge,
        .preAuthBudget = preAuthBudget_,
    };
    const Verdict verdict = Decide(state, rateGate_, now, tlsUniformBelow);
    if (verdict.rejected)
    {
        ++counters_.rejected[static_cast<size_t>(*verdict.rejected)];
    }
    return verdict;
}

void Admission::NotePreAuthEvicted()
{
    ++counters_.preAuthEvicted;
}

void Admission::NoteRejectedAfterAccept(const RejectReason reason)
{
    ++counters_.rejected[static_cast<size_t>(reason)];
}

void Admission::NoteAccepted()
{
    ++counters_.accepted;
}

void Admission::NoteHandshake(const SteadyTime openedAt, const SteadyTime verifiedAt)
{
    handshake_.Observe(openedAt, verifiedAt);
    ++counters_.handshakes;
}

void Admission::NoteHandshakeTimeout()
{
    ++counters_.handshakeTimeouts;
}

void Admission::CollectMetrics(prometheus_text::Writer &writer, const size_t currentConnections,
                               const size_t maxConnections, const size_t unauthenticatedConnections) const
{
    using prometheus_text::MetricType;
    const RateLimit &limit = rateGate_.Limit();

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
    writer.Sample({}, counters_.accepted);

    writer.BeginFamily("battle_rejected_total",
                       "Direct connections refused since process start. capacity / accept_rate / shutting_down / "
                       "preauth_budget / preauth_early_drop are refused right after accept() with a TCP reset, before any "
                       "per-connection state is built; no_token_secret is a startup misconfiguration backstop.",
                       MetricType::kCounter);
    for (size_t reason = 0; reason < kRejectReasonCount; ++reason)
    {
        writer.Sample({{"reason", kRejectReasonNames[reason]}}, counters_.rejected[reason]);
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
    writer.Histogram({}, accept_admission::kHandshakeUpperBoundsMs, handshake_.buckets, handshake_.sumMs);

    writer.BeginFamily("battle_preauth_reaped_total",
                       "Direct connections closed because they had not completed the ticket handshake when the handshake "
                       "deadline expired.",
                       MetricType::kCounter);
    writer.Sample({}, counters_.handshakeTimeouts);

    writer.BeginFamily("battle_preauth_evicted_total",
                       "Unauthenticated direct connections closed to admit a new one while the unauthenticated pool was "
                       "under pressure (the oldest goes first, and only once it has been open for the minimum evictable age).",
                       MetricType::kCounter);
    writer.Sample({}, counters_.preAuthEvicted);

    writer.BeginFamily("battle_preauth_budget",
                       "Unauthenticated direct connections allowed at most when none of them is old enough to be evicted.",
                       MetricType::kGauge);
    writer.Sample({}, preAuthBudget_.full);

    writer.BeginFamily("battle_preauth_early_drop_start",
                       "Unauthenticated direct connections at which the pool counts as under pressure: the oldest are "
                       "evicted for newcomers, or newcomers are refused with a probability rising linearly to 1 at the budget.",
                       MetricType::kGauge);
    writer.Sample({}, preAuthBudget_.earlyDropStart);

    writer.BeginFamily("battle_preauth_evictable_age_ms",
                       "How long an unauthenticated direct connection must have been open before it can be evicted.",
                       MetricType::kGauge);
    writer.Sample({}, static_cast<uint64_t>(preAuthBudget_.minEvictableAge.count()));
}

} // namespace battle_connection_admission
