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

// ── CreatePlayer(建角)指标 ─────────────────────────────────────────────
//
// 与上面 EnterGame 那批同一套库(go-zero core/metric)、同一个 /metrics 端点。
// 已核实会被采集:core/metric 的每次更新都先过 prometheus.Enabled() 这个全局开关,
// 而 login 走 zrpc.MustNewServer → ServiceConf.SetUp → prometheus.StartAgent,
// login.yaml 配了 Prometheus.Host 就会把开关打开(data_service 当初踩坑是因为它
// 从不走这条启动路径)。代价是单测里开关默认是关的,要读数必须先
// prometheus.Enable(),见 createplayer_name_test.go 的 createPlayerCounterValue。
//
// 全称 `login_create_player_*`(不沿用上面的 `entergame` 命名空间:这是另一条链)。
// **任何 label 都不许放 player_id / account / 名字**(高基数);定位具体玩家靠日志。
const (
	createPlayerMetricNamespace = "login"
	createPlayerMetricSubsystem = "create_player"
)

// 建角各阶段的耗时桶。上限只到 5s:mint / name / register 三个 gRPC 阶段各有 ctx 截止时间
// 管得住的 3s 预算(name 在登记结果未知时还要加一次 ≤1s 的立即补偿释放),到不了 5s 以上。
// account_write 是例外:login 的 Redis 客户端没开 ContextTimeoutEnabled、MaxRetries 走默认值 3,
// 3s 只是**单次** socket 读超时 —— Redis 卡住时一条 EVALSHA 会被重发到约 12.5s,之后的回读
// 单次还能再等约 3s(它那 1s 的 ctx 预算只掐掉重发)。这些观测都落进 +Inf 桶,看个数就够了:
// account_write 的 +Inf 桶有数,本身就是「建角锁 TTL 可能已被击穿、正确性在靠围栏兜」的信号。
var createPlayerStageBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// createPlayerStageSeconds 的 stage 取值。建角持着账号锁串行跑这四步,锁 TTL 是按它们的
// 耗时之和估的(login.yaml Locker.AccountLockTTL;不是硬上界,理由见那边的注释),
// 哪一步拖长了要能直接看出来。
const (
	createStageMint         = "mint"          // 发号(号段 / snowflake)
	createStageName         = "name"          // 名字登记(含同名重试、生成名换名重试)
	createStageRegister     = "register"      // player:zone 归属登记
	createStageAccountWrite = "account_write" // 围栏写账号 blob(含写未确认后的回读)
)

// createPlayerNameReleaseTotal 的 label 取值。
const (
	nameReleasePhaseImmediate = "immediate" // 建角失败当场发的那一次补偿释放
	nameReleasePhaseDelayed   = "delayed"   // 登记结果未知时,延迟再发的第二次
	nameReleaseResultOK       = "ok"
	nameReleaseResultError    = "error"
)

var (
	createPlayerStageSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: createPlayerMetricNamespace,
		Subsystem: createPlayerMetricSubsystem,
		Name:      "stage_seconds",
		Help: "CreatePlayer per-stage latency while holding the account lock. " +
			"Stage: mint | name | register | account_write.",
		Labels:  []string{"stage"},
		Buckets: createPlayerStageBuckets,
	})

	// 每一次补偿释放 RPC 记一条。稳态应为 0:它只在「名字已登记、后续步骤失败」时才发。
	createPlayerNameReleaseTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: createPlayerMetricNamespace,
		Subsystem: createPlayerMetricSubsystem,
		Name:      "name_release_total",
		Help: "Compensating ReleasePlayerName calls issued by CreatePlayer. " +
			"Phase: immediate | delayed. Result: ok | error.",
		Labels: []string{"phase", "result"},
	})

	// 孤儿 = 名字留在登记表里、却没有对应角色。每 +1 都对应一条带 player_id 与名字的
	// ERROR 日志(`[player-name] orphan reservation ...`),运维据此带 x-admin-token 手工释放。
	// 计数口径(设计 §3.11):登记结果未知的那条路径只在**延迟**那次释放失败时 +1
	// (立即那次失败还有第二次兜底);其余路径在立即那次失败时 +1;写账号 blob 未确认
	// (脚本回 -1 / 报错)且回读也失败、主动保留登记时 +1。
	createPlayerNameOrphanTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: createPlayerMetricNamespace,
		Subsystem: createPlayerMetricSubsystem,
		Name:      "name_orphan_total",
		Help: "Name reservations possibly left without a character (needs manual " +
			"ReleasePlayerName with admin token; see the matching ERROR log).",
		Labels: []string{},
	})
)

// PrimeCreatePlayerMetrics 把孤儿计数器预置成 0,让这条序列从起服起就存在。
//
// 为什么需要:无 label 的 CounterVec 在第一次 Inc 之前根本不输出序列(是 absent,不是 0)。
// 孤儿稳态恒为 0,于是 `increase(login_create_player_name_orphan_total[..]) > 0` 这种告警
// 恰好漏掉**第一次**孤儿 —— 序列从无到 1,窗口里只有一个样本,increase 算不出增量。
//
// 调用时机有讲究:go-zero core/metric 的每次写入都要过 prometheus.Enabled() 全局开关,
// 开关由 zrpc.MustNewServer → ServiceConf.SetUp → prometheus.StartAgent 打开。
// 所以必须在 MustNewServer **之后**调(login.go startServer);放进 init() 或包变量初始化里
// 会被开关直接丢弃,等于没写。Prometheus.Host 留空的环境里本函数是空操作,与其它指标同口径。
//
// 只预置孤儿这一条:name_release_total 带 label、且不用于"出现即告警",不值得为它预铺四条序列。
func PrimeCreatePlayerMetrics() {
	createPlayerNameOrphanTotal.Add(0)
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
