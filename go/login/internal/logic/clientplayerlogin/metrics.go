// metrics.go — Prometheus instrumentation for the EnterGame async chain.
//
// Goal of this file: pinpoint which stage of the async chain stalls under
// stress so we can stop guessing about err25 root cause.
//
// Background (2026-05-27 stress §5): single-zone 25k smoke shows err25
// (`kLoginInProgress`) starts at T+11m / conn≈61k, with `player_locker`
// staying held for the full 120s TTL. The lock is only released when the
// async chain calls `onPreloadComplete` → `releaseLock`. That means the
// chain must be stalling at one of seven discrete stages between TryLock
// and Release. This file makes each stage a separate histogram so we can
// see in Grafana exactly which one regresses when err25 starts.
//
// All histograms use go-zero's metric.NewHistogramVec, exposed at the
// service's existing /metrics endpoint (login.yaml Prometheus block,
// :9101 by default). Bucket choice: same shape as `login_queue` for
// consistency, but biased lower (0.01s..30s) because individual stages
// should be sub-second in steady state.
//
// Naming: `entergame_*_seconds`. The leading `entergame_` keeps these
// distinct from `login_queue_*`.
package clientplayerloginlogic

import (
	"time"

	"github.com/zeromicro/go-zero/core/metric"

	"login/internal/logic/pkg/sessionmanager"
)

const metricNamespace = "entergame"

// Buckets for the per-stage histograms.
//
// Why this shape:
//   - 0.005 .. 0.1s : "fast and healthy" — Redis local hop, Kafka send
//     into a non-saturated producer.
//   - 0.25 .. 1s   : "warm but acceptable" — stage hits a brief queueing
//     delay; under load this is the common case.
//   - 2 .. 5s      : "starting to drag" — first sign of stage saturation;
//     SLO bell should ring here.
//   - 10 .. 30s    : "definitely a problem" — caller almost certainly
//     timed out and resubmitted; this is what we expect to see in the
//     "stalled chain → err25" scenario.
//   - +Inf         : open bucket; anything over 30s is "the chain is
//     stuck" and the count alone is enough.
var stageBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30,
}

var (
	// totalSeconds: full async-chain latency, t0=TryLock acquired,
	// t1=releaseLock about to fire. Result label distinguishes the four
	// terminal states so dashboards can split success vs failure tails.
	totalSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "total_seconds",
		Help: "End-to-end EnterGame async-chain latency, from TryLock " +
			"acquire to releaseLock fire. Result label: success | " +
			"preload_failed | apply_failed | lock_lost.",
		Labels:  []string{"result"},
		Buckets: stageBuckets,
	})

	// preloadSeconds: the EnsurePlayerAllDataInRedisAsync stage —
	// Kafka SyncProducer.SendMessages + dispatcher-callback wait.
	// Expected to dominate the total under load (per the postmortem
	// hypothesis). Result label: success | failed | timeout.
	preloadSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "preload_seconds",
		Help: "EnsurePlayerAllDataInRedisAsync latency (Kafka send + " +
			"dispatcher callback wait). Result: success | failed | timeout.",
		Labels:  []string{"result"},
		Buckets: stageBuckets,
	})

	// applySeconds: applyLoadedPlayerSession total — GetSession +
	// persist + BindGate + EnterScene. If preloadSeconds is fine but
	// applySeconds is high, the bottleneck is one of the four sub-stages
	// below.
	applySeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "apply_seconds",
		Help: "applyLoadedPlayerSession total latency, summing GetSession " +
			"+ persistEnterGameSession + SendBindSessionToGate + EnterScene.",
		Labels:  []string{"result"},
		Buckets: stageBuckets,
	})

	// Sub-stages of applyLoadedPlayerSession. Each is a single RPC or
	// Kafka send, so a single un-labelled histogram is enough; we don't
	// need a result label because the caller already records apply_seconds
	// with the failure tagged.

	applyGetSessionSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "apply_get_session_seconds",
		Help:      "GetSession RPC latency from login → player_locator.",
		Labels:    []string{},
		Buckets:   stageBuckets,
	})

	// persist label distinguishes first/replace (writes a new session)
	// from reconnect (CAS into existing session). They take very
	// different paths in player_locator; separate buckets prevent the
	// fast reconnect path from being hidden by tail of the slow first
	// path.
	applyPersistSessionSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "apply_persist_session_seconds",
		Help: "persistEnterGameSession latency (SetSession for first/replace, " +
			"Reconnect CAS for reconnect).",
		Labels:  []string{"decision"},
		Buckets: stageBuckets,
	})

	applyBindGateSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "apply_bind_gate_seconds",
		Help:      "SendBindSessionToGate latency (Kafka send to gate-cmd_g<N>, partition gateId%P).",
		Labels:    []string{},
		Buckets:   stageBuckets,
	})

	applyEnterSceneSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "apply_enter_scene_seconds",
		Help:      "SceneManagerClient.EnterScene RPC latency.",
		Labels:    []string{},
		Buckets:   stageBuckets,
	})

	// totalCounter is redundant with totalSeconds.Count() but is cheaper
	// to alert on (`rate(entergame_total[1m])`) than parsing histogram
	// metadata. Cheap to maintain; pays for itself the first time you
	// write a Grafana panel.
	totalCounter = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "total",
		Help: "EnterGame async chain terminal-state count. " +
			"sum by (result) gives the success/failure split.",
		Labels: []string{"result"},
	})
)

// Result label values — exported so call sites self-document.
const (
	ResultSuccess        = "success"
	ResultPreloadFailed  = "preload_failed"
	ResultPreloadTimeout = "preload_timeout"
	ResultApplyFailed    = "apply_failed"
	ResultLockLost       = "lock_lost"
)

// observeTotal records both the histogram observation and the counter
// increment for one terminal-state event. Centralised so call sites
// don't accidentally update one without the other.
func observeTotal(start time.Time, result string) {
	totalSeconds.ObserveFloat(time.Since(start).Seconds(), result)
	totalCounter.Inc(result)
}

// observeStage is a tiny helper to convert a "deferred timer" pattern
// into a one-liner at each call site. Returns a func to be deferred:
//
//	defer observeStage(applyEnterSceneSeconds)()
//
// The trailing () runs the closure at function return, observing the
// elapsed time. The label slice is empty for unlabelled vectors, or
// e.g. ("first") for the labelled persist stage.
func observeStage(h metric.HistogramVec, labels ...string) func() {
	start := time.Now()
	return func() {
		h.ObserveFloat(time.Since(start).Seconds(), labels...)
	}
}

// decisionLabel converts a sessionmanager.EnterGameDecision into a stable
// string for the persist-stage histogram label. Kept as a small helper
// so the entergamelogic.go call site stays readable.
//
// Imported here (not in entergamelogic.go) because the histogram lives
// here and labels should be defined alongside the metric they go on.
func decisionLabel(d sessionmanager.EnterGameDecision) string {
	switch d {
	case sessionmanager.FirstLogin:
		return "first"
	case sessionmanager.ShortReconnect:
		return "reconnect"
	case sessionmanager.ReplaceLogin:
		return "replace"
	default:
		return "unknown"
	}
}
