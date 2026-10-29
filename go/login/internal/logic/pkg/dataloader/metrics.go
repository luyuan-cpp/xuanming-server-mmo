// metrics.go — Prometheus instrumentation for the dataloader package.
//
// (3) in the 2026-05-28 stress series: the (f) postmortem found that
// `entergame_preload_seconds` averaged 3s on success and 35s on failure.
// That's a 100-1000x slowdown from baseline expectation (sub-second).
// `preload` is a black box from outside — it could be Kafka send, the
// dispatcher's Pub/Sub wait, sub-cache Redis Get round-trips, or the
// fan-in finalize step. This file splits preload into 5 sub-stages so
// we can see exactly which one regresses under load.
//
// Stage map (matches EnsurePlayerAllDataInRedisAsync's control flow):
//
//   1. `cache_check`        — single Redis EXISTS for the parent key
//                              (the fast-path early-return check).
//   2. `sub_cache_check`    — loop of Redis GET for each sub-table key.
//                              On full cache hit the function returns
//                              before ever touching Kafka.
//   3. `dispatcher_register`— Register N dispatcher callbacks. CPU only,
//                              should be microseconds, but instrumented
//                              for completeness so a regression here is
//                              visible.
//   4. `kafka_send`         — sendDBReadRequests: SyncProducer.SendMessages
//                              for the batch of sub-tasks. mutex-serialized
//                              inside Kafka producer, prime suspect under
//                              load.
//   5. `callback_wait`      — from "last Kafka ack returned" to "finalize
//                              fires" (i.e. all dispatcher callbacks have
//                              run + saveToRedis returned). This is the
//                              critical bucket: if `preload_seconds` is 3s
//                              and this is 2.95s, then DB-worker turnaround
//                              + Pub/Sub plumbing is the bottleneck, not
//                              Kafka send.
//
// All buckets use the same shape as login_queue / entergame for consistency.
// Result labels: success | failed | timeout, mirroring the parent histogram
// in clientplayerlogin/metrics.go.
package dataloader

import (
	"shared/savebarrier"
	"time"

	"github.com/zeromicro/go-zero/core/metric"
)

const metricNamespace = "dataloader_preload"

var stageBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30,
}

var (
	// stage1: parent-key EXISTS check. Expected sub-ms.
	cacheCheckSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "cache_check_seconds",
		Help:      "Time spent in Redis EXISTS check for the parent PlayerAllData key.",
		Labels:    []string{"hit"},
		Buckets:   stageBuckets,
	})

	// stage2: sub-table cache GET loop. Recorded only when the parent
	// key is not yet cached. Multi-key, so latency scales with sub-table
	// count.
	subCacheCheckSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "sub_cache_check_seconds",
		Help: "Time spent in the Redis GET loop over sub-table keys. " +
			"Recorded once per preload, after the parent EXISTS miss. " +
			"Label `subs` is the number of subs that needed a DB fetch " +
			"(0 = all sub keys hot, full cache hit on a multi-key path).",
		Labels:  []string{"subs"},
		Buckets: stageBuckets,
	})

	// stage3: dispatcher Register loop. CPU-bound, microseconds expected.
	dispatcherRegisterSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "dispatcher_register_seconds",
		Help:      "Time spent registering dispatcher callbacks (CPU-only loop).",
		Labels:    []string{},
		Buckets:   stageBuckets,
	})

	// stage4: Kafka SyncProducer.SendMessages call. Mutex-serialized in
	// the producer, so under load this is where contention shows up if
	// Kafka is the bottleneck.
	kafkaSendSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "kafka_send_seconds",
		Help: "Time spent in sendDBReadRequests (SyncProducer.SendMessages). " +
			"Result: success | failed.",
		Labels:  []string{"result"},
		Buckets: stageBuckets,
	})

	// stage5: wait from "kafka sent" to "finalize fired". This is the
	// big one — if it dominates total preload, the DB worker turnaround
	// + Pub/Sub Plumbing is the bottleneck. If it's small but
	// kafka_send is large, Kafka producer mutex contention is the
	// bottleneck. The split tells you which dial to turn.
	callbackWaitSeconds = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "callback_wait_seconds",
		Help: "Time from final Kafka ack to dispatcher fan-in completion. " +
			"Includes DB worker SELECT + dispatcher Pub/Sub roundtrip. " +
			"Result: success | failed | timeout.",
		Labels:  []string{"result"},
		Buckets: stageBuckets,
	})

	// Counters for terminal states of the wait stage — cheaper to alert
	// on than histogram counts; mirrors entergame_total.
	callbackResultTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "callback_result_total",
		Help: "Terminal-state count of the dispatcher fan-in wait. " +
			"Result: success | failed | timeout.",
		Labels: []string{"result"},
	})

	// persistSchemaRegressionTotal 计 C4(docs/design/mixed-version-persist-field-protection.md §5.1):
	// 父快照快路径上加载世代低于 save_barrier 记录的 schema_gen(reason=regression),
	// 或 schema_gen 存储值损坏(reason=malformed)。mode=warn 照常入场,mode=enforce 本次 EnterGame 失败。
	// 合法流程恒为 0(设计 §5.3),任何增长都要按 §8.4 的 PersistSchemaRegression 处置。
	// 名字不跟本包的 dataloader_preload 前缀:它是设计里约定的告警名。不带 player_id(高基数)。
	// 四条序列(reason × mode)起服时由 PrimePersistSchemaRegressionMetrics 预置成 0。
	persistSchemaRegressionTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "login",
		Subsystem: "",
		Name:      "persist_schema_regression_total",
		Help: "EnterGame parent-snapshot fast-path loads whose persist schema generation is below the " +
			"save barrier's recorded schema_gen (reason=regression) or whose recorded schema_gen is malformed " +
			"(reason=malformed). mode=warn admits the load, mode=enforce fails the EnterGame. Expected to stay 0.",
		Labels: []string{"reason", "mode"},
	})
)

// login_persist_schema_regression_total 的 reason 取值,集合封闭。
const (
	schemaRegressionReasonRegression = "regression"
	schemaRegressionReasonMalformed  = "malformed"
)

// PrimePersistSchemaRegressionMetrics 把 login_persist_schema_regression_total 的全部四条序列(reason × mode)预置成 0,
// 让它们从起服起就存在。
//
// 理由与 clientplayerlogin.PrimeCreatePlayerMetrics 相同:带 label 的 CounterVec 在某个组合第一次写入之前不输出序列,
// 序列从"不存在"直接跳到 1 时 `increase(...) > 0` 算不出增量,会漏掉第一次。这个计数合法流程恒为 0、出现就要处置,
// 也是切 enforce 的放行依据(设计 §8.1 第 5 步要求 warn 观察期为 0),第一次绝不能漏。
// 两种模式都预置:运行中改模式要重启,但看板与告警按全集写,不依赖当前模式。
//
// 调用时机同 PrimeCreatePlayerMetrics:必须在 zrpc.MustNewServer 之后(login.go startServer),
// 之前 go-zero 的指标开关没打开,写入会被丢弃。
func PrimePersistSchemaRegressionMetrics() {
	for _, reason := range []string{schemaRegressionReasonRegression, schemaRegressionReasonMalformed} {
		for _, mode := range []string{"warn", "enforce"} {
			persistSchemaRegressionTotal.Add(0, reason, mode)
		}
	}
}

// parentLoadVerifyTotal 计父快照核验路径(冷加载、Redis 实例身份变化、ACK 高于 pending、历史缺口)的结论
// (docs/design/zone-redis-regression-guard-20261022.md §7)。verdict 取 savebarrier.Verdict 的 String(),
// 判定之前或提交时失败记 error。adopted 每次都说明 Redis 丢过已确认的写;sql_behind 说明 SQL 侧回档或损坏，
// 合法流程恒为 0;reconciling 持续增长说明两张表停在不同版本，要转人工 save_barrier_restore。不带 player_id。
var parentLoadVerifyTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "",
	Name:      "parent_load_verify_total",
	Help: "Outcomes of the parent-snapshot SQL verification path (cold load, Redis instance identity change, " +
		"ACK ahead of pending, missing barrier history). verdict: consistent | adopted | inflight | reconciling | " +
		"sql_behind | error. sql_behind is expected to stay 0.",
	Labels: []string{"verdict"},
})

// parentLoadVerifyVerdicts 是 login_parent_load_verify_total 的全部 verdict 取值，集合封闭。
var parentLoadVerifyVerdicts = []string{
	savebarrier.VerdictConsistent.String(), savebarrier.VerdictAdoptSQL.String(), savebarrier.VerdictInflight.String(),
	savebarrier.VerdictReconciling.String(), savebarrier.VerdictSQLBehind.String(), parentLoadVerifyError,
}

const parentLoadVerifyError = "error"

// countParentLoadVerify 记一次核验结论。consistent / adopted 本该成功，带着错误返回说明提交失败，记 error;
// 其余结论本身就以错误返回(等待或拒绝)，按结论记。
func countParentLoadVerify(verdict savebarrier.Verdict, err error) {
	label := verdict.String()
	if verdict == 0 || (err != nil && (verdict == savebarrier.VerdictConsistent || verdict == savebarrier.VerdictAdoptSQL)) {
		label = parentLoadVerifyError
	}
	parentLoadVerifyTotal.Inc(label)
}

// PrimeParentLoadVerifyMetrics 把 login_parent_load_verify_total 的全部序列预置成 0,理由与调用时机同
// PrimePersistSchemaRegressionMetrics(sql_behind 的第一次绝不能漏)。
func PrimeParentLoadVerifyMetrics() {
	for _, verdict := range parentLoadVerifyVerdicts {
		parentLoadVerifyTotal.Add(0, verdict)
	}
}

const (
	resultSuccess = "success"
	resultFailed  = "failed"
	resultTimeout = "timeout"
)

// observeStage returns a deferred-timer closure for the matching
// histogram. Same pattern as clientplayerlogin/metrics.go's helper,
// duplicated here to avoid a cross-package metric helper (the two
// histogram namespaces are independent and should stay that way).
func observeStage(h metric.HistogramVec, labels ...string) func() {
	start := time.Now()
	return func() {
		h.ObserveFloat(time.Since(start).Seconds(), labels...)
	}
}
