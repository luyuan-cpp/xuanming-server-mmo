// Package metrics exposes Prometheus instrumentation for the db service.
//
// Why this exists: Round 15 (45k×1zone) saw login-side
// dataloader_preload_callback_wait_seconds{ok} avg climb 11.8 ms → 31.4 ms
// (+166 %) over the 18-min window, while kafka_send stayed flat at ~3.3 ms.
// `cb_wait` is the gap between "last Kafka ack returned" and "dispatcher
// fan-in complete", i.e. **db consumer SELECT + Redis Pub/Sub roundtrip**.
// Up to Round 15 the db side was a black box. These histograms split the
// per-task budget so Round 16 can see exactly which sub-step regresses.
//
// Stages (per processed Kafka message, in time order):
//
//	op_total       — entire processTaskWithoutLock invocation
//	op_handler     — DB op handler (read = MySQL SELECT + cache write-back;
//	                 write = MySQL Save + cache write-back)
//	cache_write    — Redis SET of the write-back cache key (subset of
//	                 op_handler, recorded again standalone so we can see
//	                 cache contention separately)
//	result_publish — LPush task:result:{taskId} + Expire + Publish notify
//	                 (read ops only). The Publish is the trigger that wakes
//	                 the login dispatcher.
//
// Counter labels: op = read | write | unknown, result = ok | error.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
)

const subsystem = "db"

var stageBuckets = []float64{
	0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5,
}

var (
	taskStageSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "task_stage_seconds",
		Help:      "Per-task sub-stage latency. stage: op_total | op_handler | cache_write | result_publish. op: read | write | unknown.",
		Buckets:   stageBuckets,
	}, []string{"stage", "op"})

	taskResultTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "task_result_total",
		Help:      "Terminal outcome of each processed task. op: read | write | unknown; result: ok | error.",
	}, []string{"op", "result"})

	blobBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "blob_bytes",
		Help:      "Stored (base64-encoded) byte size of each pb blob column on the write path. Read p99, not max.",
		Buckets:   blobBuckets,
	}, []string{"table", "column"})

	blobRowBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "blob_row_bytes",
		Help:      "Sum of all stored pb blob column bytes per written row. Read p99, not max.",
		Buckets:   blobBuckets,
	}, []string{"table"})

	blobGuardTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "blob_guard_total",
		Help:      "Blob size gate hits. level: warn (>= WarnRatio of limit) | reject (over limit). column=__row__ means the per-row total.",
	}, []string{"table", "column", "level"})

	// ownerEpochGuardTotal 是落库前归属 epoch 守卫的全量分类计数(每条 write 任务记一次)。
	// 它回答的是「兼容窗口还剩多少旧版生产者(legacy_zero)」「Redis 是不是被清过(ahead /
	// missing_key)」这类部署期问题;真正的健康红线是下面的 staleOwnerWriteRejectedTotal。
	ownerEpochGuardTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "owner_epoch_guard_total",
		Help:      "Owner-epoch guard verdicts on write tasks. outcome: match | legacy_zero | missing_key | ahead | stale | read_error.",
	}, []string{"outcome"})

	// staleOwnerWriteRejectedTotal 是 cross-zone-scene-travel.md §6 不变量 3 点名的健康信号:
	// 被废黜节点的迟到 DBTask 被拒的次数,压测期应恒为 0。单独建一个无 label 的 counter,
	// 是为了让 Codex 验证清单与告警规则能按固定名字直接引用,不依赖 label 匹配。
	staleOwnerWriteRejectedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "stale_owner_write_rejected_total",
		Help:      "Write tasks dropped because their owner_epoch is older than player:{id}:owner_epoch in Redis. Must stay 0 under stress.",
	})

	registerOnce sync.Once
)

// blobBuckets 覆盖 256B ~ 8MiB(每档 ×4)。
//
// 之所以要 histogram 而不是 gauge/max:max 只会告诉你「有过一次 3MB 的写入」,
// 既定位不到是哪条业务线在涨,也分不清是常态还是孤例。p99 才是「设计上限该
// 定在哪」以及「本周有没有变胖」的唯一可读信号。上界给到 8MiB 是为了在
// MEDIUMBLOB 的 16MiB 天花板之前还能看见分布,而不是全挤在 +Inf 桶里。
var blobBuckets = []float64{
	256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 8388608,
}

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(taskStageSeconds, taskResultTotal, blobBytes, blobRowBytes, blobGuardTotal,
			ownerEpochGuardTotal, staleOwnerWriteRejectedTotal)
	})
}

// 归属 epoch 守卫的 outcome label 取值。与 internal/kafka 的守卫判定一一对应;
// 集合有界(6 个),不含 player_id 等高基数维度(AGENTS.md §9)。
const (
	OwnerEpochMatch      = "match"       // 任务 epoch == 已落库的最大 epoch:同一持有者的后续写,放行
	OwnerEpochLegacyZero = "legacy_zero" // 任务 epoch == 0:旧版生产者,兼容窗口内放行不比对
	OwnerEpochFirst      = "first"       // 该 key 还没有带 epoch 的写落过库,放行
	OwnerEpochAdvance    = "advance"     // 任务 epoch > 已落库的最大 epoch:归属变更后新主的第一笔写,放行
	OwnerEpochStale      = "stale"       // 任务 epoch < 已落库的最大 epoch:新主落库之后才到的老 epoch 写,拒绝
	OwnerEpochReadError  = "read_error"  // Redis 读失败:按可重试错误处理,不放行也不丢
)

// CountOwnerEpochGuard 记一次守卫判定。outcome 用上面的 OwnerEpoch* 常量。
func CountOwnerEpochGuard(outcome string) {
	register()
	ownerEpochGuardTotal.WithLabelValues(outcome).Inc()
}

// CountStaleOwnerWriteRejected 记一次「老 epoch 写被拒」。与 CountOwnerEpochGuard(OwnerEpochStale)
// 同时调用是有意的:前者是固定名字的健康红线,后者是全量分类。
func CountStaleOwnerWriteRejected() {
	register()
	staleOwnerWriteRejectedTotal.Inc()
}

// BlobObserver 把 dbguard 的量测结果接到 prometheus 上。
//
// label 基数是有界的:table 来自 mysql_database_table_list.json(9 张;player_snapshot /
// rollback_audit_log 只在全局库 data_service,已从 zone 清单摘掉),
// column 来自 proto 声明(每表 ≤ 10 列)—— 不含 player_id 之类的高基数维度
// (见仓库 CLAUDE.md §9)。
type BlobObserver struct{}

// ObserveBlobBytes 记录单列入库字节。
func (BlobObserver) ObserveBlobBytes(table, column string, bytes int64) {
	register()
	blobBytes.WithLabelValues(table, column).Observe(float64(bytes))
}

// ObserveRowBytes 记录整行 blob 入库字节之和。
func (BlobObserver) ObserveRowBytes(table string, bytes int64) {
	register()
	blobRowBytes.WithLabelValues(table).Observe(float64(bytes))
}

// CountGate 记录一次 warn / reject。
func (BlobObserver) CountGate(table, column, level string) {
	register()
	blobGuardTotal.WithLabelValues(table, column, level).Inc()
}

// ObserveStage records one sub-stage latency. Use one of the StageXxx
// constants for stage. op should be "read" / "write" / "unknown".
func ObserveStage(stage, op string, d time.Duration) {
	register()
	taskStageSeconds.WithLabelValues(stage, op).Observe(d.Seconds())
}

// ObserveResult records one terminal task outcome.
func ObserveResult(op, result string) {
	register()
	taskResultTotal.WithLabelValues(op, result).Inc()
}

// Stage labels — keep in sync with the call sites in
// internal/kafka/key_ordered_consumer.go.
const (
	StageOpTotal       = "op_total"
	StageOpHandler     = "op_handler"
	StageCacheWrite    = "cache_write"
	StageResultPublish = "result_publish"
)

// Start exposes /metrics over HTTP. Failures log but do not block service
// startup — metrics are observability, not critical path. Empty addr
// disables the endpoint (production default; opt-in via config).
func Start(addr string) {
	if addr == "" {
		logx.Info("metrics endpoint disabled (MetricsListenAddr empty)")
		return
	}
	register()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logx.Infof("db /metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("db /metrics ListenAndServe failed: %v", err)
		}
	}()
}
