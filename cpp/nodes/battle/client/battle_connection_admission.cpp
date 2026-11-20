#include "battle_connection_admission.h"

namespace battle_connection_admission
{

std::optional<RejectReason> Admission::Admit(const size_t currentConnections, const size_t maxConnections,
                                             const SteadyTime now)
{
    const AdmissionState state{
        .sourceClosed = sourceClosed_,
        .currentConnections = currentConnections,
        .maxConnections = maxConnections,
    };
    const auto rejected = Decide(state, rateGate_, now);
    if (rejected)
    {
        ++counters_.rejected[static_cast<size_t>(*rejected)];
    }
    return rejected;
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
                       "Direct connections refused since process start. capacity / accept_rate / shutting_down are "
                       "refused right after accept() with a TCP reset, before any per-connection state is built; "
                       "no_token_secret is a startup misconfiguration backstop.",
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
}

} // namespace battle_connection_admission
