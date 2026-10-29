package metrics

// pipeline.go —— 存盘管线的测量打点与排序锁租约指标(docs/design/db-task-pipeline-bottlenecks.md §6.2 / G1)。
//
// 单独成文件、走自己的 sync.Once(registerPipeline),而不是塞进 metrics.go 的 register():几条并行的改动
// 都在动 register(),新指标集中放这里,合并时互不冲突。
//
// 全部 label 都是封闭集合,不含 player_id(AGENTS.md §9):
//
//	db_task_redis_step_seconds{op,step}       op 2 个取值 × step 12 个取值
//	db_ordering_guard_total{outcome}          7 个取值
//	db_subshard_batch_size                    无 label
//	db_ordering_lease_lost_total{reason}      4 个取值
//	db_ordering_lease_renew_retry_total       无 label
//	db_ordering_lock_release_total{outcome}   4 个取值
//	db_redis_pool_*{redis_client}             main | placement
//	db_fence_write_total{class,outcome}       34 个合法组合
//	db_fence_cache_publish_total{path,result} 6 个合法组合
//	db_fence_body_mismatch_total             无 label
//	db_fence_rejected_bytes_total{outcome}    6 个取值
//	db_save_barrier_ack_ahead_total{kind}     2 个取值
//
// 指标名与取值和 tools/scripts/stress_summarize.ps1 的 2d / 2e 段逐字对应(那边按精确 key 读序列);
// 改名或改取值要同改脚本、test_stress_summarize.ps1 与本包的 TestRedisStepNamesPinned。

import (
	"reflect"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// redisStepBuckets 覆盖 0.1ms ~ 1s:单次 Redis 往返通常在 0.1~1ms,stageBuckets 从 0.5ms 起步分不开。
// 新桶只给新直方图用,stageBuckets 不动(改桶会让历史序列与新序列不可比)。
var redisStepBuckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}

// subShardBatchBuckets 覆盖 1 ~ 256 条:子 shard 通道缓冲是 256,一批不会更多。
var subShardBatchBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256}

var (
	// taskRedisStepSeconds 是一条任务每个 Redis 往返的耗时,在调用点逐个计时(不挂 go-redis Hook:placement 与主
	// client 共用同一个对象时 Hook 会挂两次、重复计数)。每个往返只记一次。重试认领、LREM、30s 一次的续租不计入:
	// 前两项只在重试源出现,续租另有 db_ordering_lease_* 指标。
	taskRedisStepSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "task_redis_step_seconds",
		Help: "Latency of each Redis round trip on the db task path, timed at the call site (one observation per round trip). " +
			"op: read | write. step: lock_acquire | cursor_get | epoch_get | owner_epoch_probe | placement_lookup | expand_get | placement_recheck | " +
			"lease_verify | cache_publish | cache_set | applied_advance | lock_release. Read tasks only have placement_lookup, expand_get, " +
			"placement_recheck and cache_set (SET NX). Write tasks use lease_verify only for older_seq, cache_publish for the atomic " +
			"token/cache/proof publication and applied_advance for the atomic token/cursor/epoch advance. Retry claims, LREM and " +
			"the periodic lease renewals are not included. The cache_set and lease_verify semantics differ from Round 20; cursor_set " +
			"and epoch_mark were replaced, so those columns must not be subtracted directly from the old baseline.",
		Buckets: redisStepBuckets,
	}, []string{"op", "step"})

	// orderingGuardTotal 是写任务游标守卫(orderingForTask)的全量分类计数。apply_no_cursor 是「没有 applied 游标、
	// 按首写放行」:Redis 游标被清空(FLUSH / 回档)之后它会成片出现,是判断游标防线是否失效的直接信号。
	orderingGuardTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "ordering_guard_total",
		Help: "Applied-cursor ordering guard verdicts on write tasks (orderingForTask). outcome: apply | apply_no_cursor " +
			"(no applied cursor yet, applied as a first write) | stale | incomparable | error (cursor read or parse failed) | stale_no_epoch | epoch_override (the last two are subsets of stale).",
	}, []string{"outcome"})

	// subShardBatchSize 是子 shard 每次合并进 processTaskBatch 的任务数(合并去重之前)。
	subShardBatchSize = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "subshard_batch_size",
		Help:      "Number of tasks a sub-shard drained into one processTaskBatch call, before write coalescing.",
		Buckets:   subShardBatchBuckets,
	})

	// orderingLeaseLostTotal 记排序锁失租(go/db/internal/kafka/ordering_lease.go)。失租会取消这条写的在途 SQL,
	// 但这不是栅栏:已经进了 MySQL 的语句撤不回。告警见 deploy/k8s/db-task-retry-alerts.yaml。
	orderingLeaseLostTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "ordering_lease_lost_total",
		Help: "Cross-instance ordering lock leases lost. reason: owner_changed (renewal, verification, token publication or " +
			"applied advance found that the lock is no longer owned) | " +
			"renew_exhausted (renewal failed the maximum number of times) | renew_deadline (no successful renewal before the " +
			"local deadline) | verify_error (the older_seq token verification or a token publication/advance round trip failed). A lost lease cancels " +
			"the write's in-flight SQL; it is not a fence.",
	}, []string{"reason"})

	// orderingLeaseRenewRetryTotal 记续租失败后在租约预算内重试的次数;持续上涨说明 Redis 抖动在吃租约余量。
	orderingLeaseRenewRetryTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "ordering_lease_renew_retry_total",
		Help:      "Failed ordering lease renewals that were retried within the lease budget.",
	})

	// orderingLockReleaseTotal 记释放排序锁的结果。not_owner 是 critical:释放时锁已不归本实例,持锁期间别的实例
	// 可能已写下更新版本,本实例的整行写可能晚于它落库。
	orderingLockReleaseTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "ordering_lock_release_total",
		Help: "Ordering lock release outcomes. outcome: released | not_owner (already expired or taken over at release; " +
			"MySQL may hold an older snapshot until the next save) | error | skipped_sql_abandoned (the SQL was canceled on " +
			"lease loss, the lock is left to its TTL).",
	}, []string{"outcome"})

	pipelineOnce sync.Once
)

// db_task_redis_step_seconds 的 step 取值。
const (
	RedisStepLockAcquire      = "lock_acquire"      // 排序锁 SETNX
	RedisStepCursorGet        = "cursor_get"        // applied 游标 GET
	RedisStepEpochGet         = "epoch_get"         // applied owner epoch GET(任务带 epoch 时)
	RedisStepOwnerEpochProbe  = "owner_epoch_probe" // owner epoch 计数器探测
	RedisStepPlacementLookup  = "placement_lookup"  // 选库 MGET 落点记录 + home_zone
	RedisStepExpandGet        = "expand_get"        // 扩容状态 GET(离线扩容模式下跳过)
	RedisStepPlacementRecheck = "placement_recheck" // 落库后复核 MGET
	RedisStepLeaseVerify      = "lease_verify"      // 写任务仅 older_seq 路径核锁(带 token 的 Lua 续期)
	RedisStepCachePublish     = "cache_publish"     // 写路径同一 Lua 核 token 并发布缓存 / SQL 证据
	RedisStepCacheSet         = "cache_set"         // 读路径共享缓存 SET NX
	RedisStepAppliedAdvance   = "applied_advance"   // 同一 Lua 核 token 并推进 applied 游标 / epoch
	RedisStepLockRelease      = "lock_release"      // 排序锁释放(带 token 的 Lua DEL)
)

// RedisSteps 是 12 个 step 取值,按一条写任务的执行顺序排列。stress_summarize.ps1 的 2e 段按同样的名字与顺序
// 出列;两边一一对应,改名 / 合并 step 时同改脚本、test_stress_summarize.ps1 与 TestRedisStepNamesPinned。
var RedisSteps = []string{
	RedisStepLockAcquire, RedisStepCursorGet, RedisStepEpochGet, RedisStepOwnerEpochProbe,
	RedisStepPlacementLookup, RedisStepExpandGet, RedisStepPlacementRecheck, RedisStepLeaseVerify,
	RedisStepCachePublish, RedisStepCacheSet, RedisStepAppliedAdvance, RedisStepLockRelease,
}

// db_ordering_guard_total 的 outcome 取值。
const (
	OrderingApply         = "apply"
	OrderingApplyNoCursor = "apply_no_cursor"
	OrderingStale         = "stale"
	OrderingIncomparable  = "incomparable"
	OrderingError         = "error"
	OrderingStaleNoEpoch  = "stale_no_epoch"
	OrderingEpochOverride = "epoch_override"
)

var orderingGuardOutcomes = []string{OrderingApply, OrderingApplyNoCursor, OrderingStale, OrderingIncomparable, OrderingError, OrderingStaleNoEpoch, OrderingEpochOverride}

// db_ordering_lease_lost_total 的 reason 取值。
const (
	LeaseLostOwnerChanged   = "owner_changed"
	LeaseLostRenewExhausted = "renew_exhausted"
	LeaseLostRenewDeadline  = "renew_deadline"
	LeaseLostVerifyError    = "verify_error"
)

var leaseLostReasons = []string{LeaseLostOwnerChanged, LeaseLostRenewExhausted, LeaseLostRenewDeadline, LeaseLostVerifyError}

// db_ordering_lock_release_total 的 outcome 取值。
const (
	LockReleaseReleased            = "released"
	LockReleaseNotOwner            = "not_owner"
	LockReleaseError               = "error"
	LockReleaseSkippedSQLAbandoned = "skipped_sql_abandoned"
)

var lockReleaseOutcomes = []string{LockReleaseReleased, LockReleaseNotOwner, LockReleaseError, LockReleaseSkippedSQLAbandoned}

// registerPipeline 注册本文件的指标。告警与汇总脚本要用到的计数器取值先以 0 建出序列:序列从无到 1 时
// increase() 会漏掉第一次(deploy/k8s/owner-epoch-alerts.yaml 记过这条经验)。
func registerPipeline() {
	pipelineOnce.Do(func() {
		prometheus.MustRegister(taskRedisStepSeconds, orderingGuardTotal, subShardBatchSize,
			orderingLeaseLostTotal, orderingLeaseRenewRetryTotal, orderingLockReleaseTotal)
		for _, outcome := range orderingGuardOutcomes {
			orderingGuardTotal.WithLabelValues(outcome).Add(0)
		}
		for _, reason := range leaseLostReasons {
			orderingLeaseLostTotal.WithLabelValues(reason).Add(0)
		}
		orderingLeaseRenewRetryTotal.Add(0)
		for _, outcome := range lockReleaseOutcomes {
			orderingLockReleaseTotal.WithLabelValues(outcome).Add(0)
		}
		registerFence()
	})
}

// ObserveRedisStep 记一次 Redis 往返。op 为 "read" / "write",step 用 RedisStep* 常量。
func ObserveRedisStep(op, step string, d time.Duration) {
	registerPipeline()
	taskRedisStepSeconds.WithLabelValues(op, step).Observe(d.Seconds())
}

// CountOrderingGuard 记一次写任务游标守卫判定,outcome 用 Ordering* 常量。
func CountOrderingGuard(outcome string) {
	registerPipeline()
	orderingGuardTotal.WithLabelValues(outcome).Inc()
}

// ObserveSubShardBatch 记一次子 shard 合并的任务数。
func ObserveSubShardBatch(n int) {
	registerPipeline()
	subShardBatchSize.Observe(float64(n))
}

// CountOrderingLeaseLost 记一次排序锁失租,reason 用 LeaseLost* 常量。
func CountOrderingLeaseLost(reason string) {
	registerPipeline()
	orderingLeaseLostTotal.WithLabelValues(reason).Inc()
}

// CountOrderingLeaseRenewRetry 记一次续租失败后的重试。
func CountOrderingLeaseRenewRetry() {
	registerPipeline()
	orderingLeaseRenewRetryTotal.Inc()
}

// CountOrderingLockRelease 记一次释放排序锁的结果,outcome 用 LockRelease* 常量。
func CountOrderingLockRelease(outcome string) {
	registerPipeline()
	orderingLockReleaseTotal.WithLabelValues(outcome).Inc()
}

// ── go-redis 连接池(db_redis_pool_*)──────────────────────────────────────────────

// RedisPoolClient 是导出连接池快照所需的最小能力,*redis.Client 满足它。实现必须是指针类型:
// 「placement 与 main 是不是同一个 client」按接口值(即指针身份)比较。
type RedisPoolClient interface {
	PoolStats() *redis.PoolStats
	Options() *redis.Options
}

// redis_client label 的取值。
const (
	RedisClientMain      = "main"      // 排序锁 / 游标 / 缓存 / 重试队列用的 RedisClient
	RedisClientPlacement = "placement" // 落点记录所在的 Placement.Redis(与 main 不是同一个对象时才导出)
)

type namedRedisPool struct {
	name   string
	client RedisPoolClient
}

// redisPoolCollector 在每次抓取时现取 PoolStats。计数器来自 go-redis 的 uint32 字段,回绕时 Prometheus 会当成
// 一次 counter reset(rate / increase 照常可用)。
type redisPoolCollector struct {
	pools []namedRedisPool

	hits, misses, timeouts, waits, waitSeconds, unusable, staleConns *prometheus.Desc
	conns, idleConns, pendingRequests, maxConns                      *prometheus.Desc
}

func newRedisPoolDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName("", subsystem, name), help, []string{"redis_client"}, nil)
}

// NewRedisPoolCollector 只构造不注册(单测用自己的注册表)。总是导出 main;只有 placement 非 nil、且与 main
// 不是同一个对象(接口值比较,即指针身份)时才再导出 placement —— Placement.Redis 缺省时 placement 就是 main 本身,
// 导出两份会让汇总脚本跨 redis_client 求和时翻倍。
func NewRedisPoolCollector(main, placement RedisPoolClient) prometheus.Collector {
	c := &redisPoolCollector{
		hits:        newRedisPoolDesc("redis_pool_hits_total", "go-redis pool: times a free connection was found in the pool."),
		misses:      newRedisPoolDesc("redis_pool_misses_total", "go-redis pool: times a free connection was not found in the pool."),
		timeouts:    newRedisPoolDesc("redis_pool_timeouts_total", "go-redis pool: times waiting for a connection timed out."),
		waits:       newRedisPoolDesc("redis_pool_wait_total", "go-redis pool: times a caller had to wait for a connection."),
		waitSeconds: newRedisPoolDesc("redis_pool_wait_seconds_total", "go-redis pool: total time spent waiting for a connection."),
		unusable:    newRedisPoolDesc("redis_pool_unusable_total", "go-redis pool: times a connection was found unusable."),
		staleConns:  newRedisPoolDesc("redis_pool_stale_conns_total", "go-redis pool: stale connections removed from the pool."),
		conns:       newRedisPoolDesc("redis_pool_conns", "go-redis pool: connections currently in the pool."),
		idleConns:   newRedisPoolDesc("redis_pool_idle_conns", "go-redis pool: idle connections currently in the pool."),
		pendingRequests: newRedisPoolDesc("redis_pool_pending_requests",
			"go-redis pool: requests currently waiting for a connection."),
		maxConns: newRedisPoolDesc("redis_pool_max_conns", "go-redis pool: configured pool size (Options.PoolSize)."),
	}
	if !isNilRedisPoolClient(main) {
		c.pools = append(c.pools, namedRedisPool{name: RedisClientMain, client: main})
	}
	if !isNilRedisPoolClient(placement) && placement != main {
		c.pools = append(c.pools, namedRedisPool{name: RedisClientPlacement, client: placement})
	}
	return c
}

// isNilRedisPoolClient 同时认 nil 接口与装着 nil 指针的接口(后者调用方法会在抓取时 panic)。
func isNilRedisPoolClient(client RedisPoolClient) bool {
	if client == nil {
		return true
	}
	v := reflect.ValueOf(client)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func (c *redisPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.hits, c.misses, c.timeouts, c.waits, c.waitSeconds, c.unusable, c.staleConns,
		c.conns, c.idleConns, c.pendingRequests, c.maxConns} {
		ch <- d
	}
}

func (c *redisPoolCollector) Collect(ch chan<- prometheus.Metric) {
	for _, pool := range c.pools {
		stats := pool.client.PoolStats()
		if stats == nil {
			continue
		}
		poolSize := 0
		if opts := pool.client.Options(); opts != nil {
			poolSize = opts.PoolSize
		}
		counter := func(d *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, pool.name)
		}
		gauge := func(d *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, pool.name)
		}
		counter(c.hits, float64(stats.Hits))
		counter(c.misses, float64(stats.Misses))
		counter(c.timeouts, float64(stats.Timeouts))
		counter(c.waits, float64(stats.WaitCount))
		counter(c.waitSeconds, float64(stats.WaitDurationNs)/1e9)
		counter(c.unusable, float64(stats.Unusable))
		counter(c.staleConns, float64(stats.StaleConns))
		gauge(c.conns, float64(stats.TotalConns))
		gauge(c.idleConns, float64(stats.IdleConns))
		gauge(c.pendingRequests, float64(stats.PendingRequests))
		gauge(c.maxConns, float64(poolSize))
	}
}

// RegisterRedisPools 把 go-redis 连接池快照接到默认注册表(Start 暴露的那个)。进程内调一次;注册失败只记日志:
// 指标是观测面,不能挡住启动(与 RegisterSQLPools 同口径)。
func RegisterRedisPools(main, placement RedisPoolClient) {
	register()
	registerPipeline()
	if err := prometheus.Register(NewRedisPoolCollector(main, placement)); err != nil {
		logx.Errorf("[metrics] register redis pool collector failed (db_redis_pool_* unavailable): %v", err)
	}
}

// 围栏指标使用固定业务取值,不以玩家身份作 label。预建仅包含设计允许的组合,不能把 unfenced 的写误算成围栏拒绝。
var fenceClasses = []string{"seq", "legacy_epoch", "legacy_zero"}
var fenceWriteOutcomes = []string{
	"updated", "inserted", "unchanged", "equal", "older_seq", "stale_owner", "owner_epoch_regressed", "legacy_after_seq", "race", "error",
	"schema_regression", // 写方的持久 schema 世代低于行世代(docs/design/mixed-version-persist-field-protection.md §6.2 C1b)
}
var fenceUnfencedOutcomes = []string{"updated", "inserted", "unchanged", "error"}
var fenceCacheWriteResults = []string{"written", "not_owner", "evidence_ahead", "evidence_conflict"}
var fenceCacheReadResults = []string{"written", "exists"}
var fenceRejectedOutcomes = []string{"equal", "older_seq", "stale_owner", "owner_epoch_regressed", "legacy_after_seq", "epoch_override", "schema_regression"}

// db_save_barrier_ack_ahead_total 的 kind 取值(docs/design/zone-redis-regression-guard-20261022.md §4.2 G2、§7)。
const (
	SaveBarrierAckAbovePending   = "above_pending"   // 本次发布的 ACK 严格大于屏障 pending
	SaveBarrierAckPendingMissing = "pending_missing" // 屏障里没有 pending,却有已提交的 SQL 版本要发布 ACK
)

var saveBarrierAckAheadKinds = []string{SaveBarrierAckAbovePending, SaveBarrierAckPendingMissing}

var (
	fenceWriteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "fence_write_total",
		Help: "SQL write outcomes. class: seq | legacy_epoch | legacy_zero | unfenced. outcome: updated | inserted | unchanged | " +
			"equal | older_seq | stale_owner | owner_epoch_regressed | legacy_after_seq | race | error | schema_regression. Unfenced writes only use " +
			"updated, inserted, unchanged or error. The transitional SQL implementation reports updated for both changed and unchanged rows.",
	}, []string{"class", "outcome"})
	fenceCacheTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "fence_cache_publish_total",
		Help: "Atomic token/cache/SQL-proof publication and read SET NX outcomes. path=write result: written | not_owner | " +
			"evidence_ahead | evidence_conflict; path=read result: written | exists. Evidence ahead is also counted as written " +
			"when the cache was published while preserving the newer proof.",
	}, []string{"path", "result"})
	fenceBodyMismatch = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "fence_body_mismatch_total",
		Help:      "Producer bodies unexpectedly carried nonzero fence metadata; counted in both legacy and dual modes.",
	})
	fenceRejectedBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "fence_rejected_bytes_total",
		Help: "Row-body bytes submitted by SQL writes rejected by fences. outcome: equal | older_seq | stale_owner | " +
			"owner_epoch_regressed | legacy_after_seq | epoch_override | schema_regression. Epoch override counts rejected writes admitted by the ordering override.",
	}, []string{"outcome"})
	// saveBarrierAckAheadTotal 记 go/db 发布的存盘屏障 ACK 高于 pending(不变量「ACK ≤ pending」被破坏)。ACK 照写,
	// 这里只计数:多半是 zone Redis 丢了已确认的写,玩家下次登录时 login 会拿 SQL 交叉核验。不带 player_id。
	saveBarrierAckAheadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "save_barrier_ack_ahead_total",
		Help: "Save barrier ACKs published above the barrier's pending version (invariant: ACK <= pending). kind: above_pending | " +
			"pending_missing. The ACK is still written because it is a committed SQL fact. A nonzero value almost always means the " +
			"zone Redis lost acknowledged writes (crash, failover or restore); login re-verifies the player against SQL on the next load.",
	}, []string{"kind"})
	fenceOnce sync.Once
)

func registerFence() {
	fenceOnce.Do(func() {
		prometheus.MustRegister(fenceWriteTotal, fenceCacheTotal, fenceBodyMismatch, fenceRejectedBytes, saveBarrierAckAheadTotal)
		for _, class := range fenceClasses {
			for _, outcome := range fenceWriteOutcomes {
				fenceWriteTotal.WithLabelValues(class, outcome).Add(0)
			}
		}
		for _, outcome := range fenceUnfencedOutcomes {
			fenceWriteTotal.WithLabelValues("unfenced", outcome).Add(0)
		}
		for _, result := range fenceCacheWriteResults {
			fenceCacheTotal.WithLabelValues("write", result).Add(0)
		}
		for _, result := range fenceCacheReadResults {
			fenceCacheTotal.WithLabelValues("read", result).Add(0)
		}
		fenceBodyMismatch.Add(0)
		for _, outcome := range fenceRejectedOutcomes {
			fenceRejectedBytes.WithLabelValues(outcome).Add(0)
		}
		for _, kind := range saveBarrierAckAheadKinds {
			saveBarrierAckAheadTotal.WithLabelValues(kind).Add(0)
		}
	})
}

// CountFenceWrite 记一次 SQL 围栏结局,class 与 outcome 使用上述固定词表。
func CountFenceWrite(class, outcome string) {
	registerFence()
	fenceWriteTotal.WithLabelValues(class, outcome).Inc()
}

// CountFenceCache 记一次令牌发布或读路径 SET NX 的结果,path 与 result 使用上述固定词表。
func CountFenceCache(path, result string) {
	registerFence()
	fenceCacheTotal.WithLabelValues(path, result).Inc()
}

// CountSaveBarrierAckAhead 记一次存盘屏障 ACK 领先 pending,kind 用 SaveBarrierAck* 常量。
func CountSaveBarrierAckAhead(kind string) {
	registerFence()
	saveBarrierAckAheadTotal.WithLabelValues(kind).Inc()
}

// CountFenceBodyMismatch 记一次生产者 body 错带非零围栏字段。
func CountFenceBodyMismatch() {
	registerFence()
	fenceBodyMismatch.Inc()
}

// CountFenceRejectedBytes 记被围栏拒绝的 SQL 行载荷字节数;空载荷不增加计数。
func CountFenceRejectedBytes(outcome string, n int) {
	registerFence()
	if n <= 0 {
		return
	}
	fenceRejectedBytes.WithLabelValues(outcome).Add(float64(n))
}
