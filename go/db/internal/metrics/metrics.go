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
//	                 the login dispatcher. 2026-10-18 起只发一次:发起方 login
//	                 的实例频道,缺回复目标时发全服旧频道(计数见
//	                 task_result_notify.go)。
//
// Round 20 起补的四个 stage(stress_summarize.ps1 2d 段按这些名字读):
//
//	sql        — runDBOp 里 handler 的耗时(写 = 大字段闸 + Save,读 = SELECT),op_handler 的子集
//	task_total — handleTask 整段(含排序锁、游标 / epoch 守卫、选库、SQL、复核、核锁、缓存、游标推进、释放锁)
//	queue_wait — 任务进 worker 通道到 handleTask 开始
//	kafka_e2e  — 生产者 CreateTime 到 handleTask 结束,只统计 Kafka 来源(重试源不计);跨机有时钟偏差,
//	             负值丢弃;大于 5s 落进 +Inf 桶,但 sum / count 求出的均值仍然准确
//
// Redis 分步、游标守卫、子 shard 批量、排序锁租约与 go-redis 连接池的指标见 pipeline.go。
//
// Counter labels: op = read | write | unknown, result = ok | error.
package metrics

import (
	"net/http"
	"shared/sqlpoolstats"
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
		Help:      "Per-task sub-stage latency. stage: op_total | op_handler | cache_write | result_publish | sql | task_total | queue_wait | kafka_e2e (producer CreateTime to task end, Kafka-origin only, subject to cross-host clock skew). op: read | write | unknown.",
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
	// 它回答的是「兼容窗口还剩多少旧版生产者(legacy_zero,收口时应归零)」「该 key 第一次带
	// epoch 落库(first)」「归属换主后的第一笔写(advance,应与传送 / 交接次数对得上)」这类
	// 部署期问题;真正的健康红线是下面的 staleOwnerWriteRejectedTotal。
	//
	// outcome 的取值**必须**与下面 OwnerEpoch* 常量一一对应 —— 改常量必须同改这里的 Help,
	// 否则按 Help 配出来的告警 / 看板表达式会匹配不到任何序列,而空表达式在 Prometheus 里
	// 不报错、只静默不告警,和「一切正常」长得一模一样。
	ownerEpochGuardTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "owner_epoch_guard_total",
		Help:      "Owner-epoch guard verdicts on write tasks. outcome: match | legacy_zero | first | advance | stale | read_error | regressed (the owner epoch counter is behind the applied epoch or SQL fence).",
	}, []string{"outcome"})

	// staleOwnerWriteRejectedTotal 是 cross-zone-scene-travel.md §6 不变量 3 点名的健康信号:
	// 被废黜节点的迟到 DBTask 被拒的次数,压测期应恒为 0。单独建一个无 label 的 counter,
	// 是为了让 Codex 验证清单与告警规则能按固定名字直接引用,不依赖 label 匹配。
	staleOwnerWriteRejectedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "stale_owner_write_rejected_total",
		Help:      "Write tasks dropped because their owner_epoch is older than the max epoch already applied for this (topic,key,msg_type) — Redis key consumer:applied_epoch:* (reentry-barrier 6.3 / cross-zone-scene-travel R3). Must stay 0 under stress.",
	})

	// placementGuardTotal 是按落点选库(player-storage-placement.md §6.2)与落库后复核(§6.3)的
	// 全量分类计数:每条 read / write 任务选库时记一次,复核不通过或复核读失败时再记一次。
	// 上线观察口径见设计 §13:stale_topic 应恒 0(非 0 先查 C++ home_zone_unknown);
	// frozen_deferred 持续上涨 = 搬库工具冻结后没有切换 / 解冻(工具崩溃),应告警。
	//
	// outcome 取值**必须**与下面 Placement* 常量一一对应,改常量同改 Help —— 理由同 ownerEpochGuardTotal。
	placementGuardTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "placement_guard_total",
		Help:      "Player storage placement routing verdicts (player-storage-placement.md 6.2/6.3). op: read | write. outcome: placed | home | legacy | stale_topic | frozen_deferred | missing_required | lookup_error | recheck_moved | route_not_ready (Placement.RequireRouteReady=true and the mapping Redis replica is not trusted: deferred, never routed).",
	}, []string{"op", "outcome"})

	// placementStoreOpenTotal 记每一次**真正发生**的按需打开落点库(§6.1);打开失败后的冷却期内
	// 直接复用上次错误的请求不重复计数,所以 error 的速率就是「每个冷却期一次」,不会被任务量放大。
	placementStoreOpenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "placement_store_open_total",
		Help:      "On-demand placement store opens (player-storage-placement.md 6.1). result: ok | error (ping / schema gate / unknown database) | rejected (neither in the allow list nor admitted by AllowStoreFamilies).",
	}, []string{"result"})

	// placementOpenStores 是本进程当前常驻的落点库个数(含本 zone 库)。
	placementOpenStores = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "placement_open_stores",
		Help:      "Placement stores (MySQL databases) currently open in this db process, including the zone's own database.",
	})

	// 以下四个是 L-04(docs/design/db-task-store-unavailable-deferral.md §3.7)的观测面:任务为什么没落库、
	// 多少进了死信、重试队列有多长(哪类库在冷却见 storeCooldownCollector)。label 全是封闭集合,不含 player_id(AGENTS.md §9)。

	// taskDeferredTotal 在 deferFailedTask 一处计数,覆盖所有延后原因。
	taskDeferredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "task_deferred_total",
		Help:      "Tasks pushed back to the retry queue instead of completing. op: read | write | unknown. reason: conn_refused | conn_lost | store_readonly | store_busy | store_cooldown | store_open_failed | ordering_lock_busy | fence_race | fence_evidence_conflict | lease_lost | placement_frozen | placement_moved | route_not_ready | expand_lock_busy | redis_unavailable | schema_too_new | other.",
	}, []string{"op", "reason"})

	// taskDeadLetterTotal 在每个死信写入点成功写入后计数。
	taskDeadLetterTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "task_dead_letter_total",
		Help:      "Tasks moved into kafka:dead:queue:{topic}. reason: budget_exhausted | malformed_payload | legacy_payload | invalid_partition | ordering_incomparable | stale_topic | missing_required | poison | fence_stale_owner | fence_legacy_after_seq | owner_epoch_regressed | persist_unstamped | schema_regression.",
	}, []string{"reason"})

	// taskReadAbandonedTotal 记读任务被放弃的次数:读不改数据,不进死信。
	taskReadAbandonedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "task_read_abandoned_total",
		Help:      "Read tasks answered with a failure or dropped instead of retried / dead-lettered. reason: store_unavailable (failure receipt sent at once) | budget_exhausted (dropped after the retry budget) | schema_too_new (row written by a newer persist schema generation than this binary; failure receipt sent at once).",
	}, []string{"reason"})

	// 玩家持久 schema 世代(docs/design/mixed-version-persist-field-protection.md §6)的观测面:本二进制的世代、本 zone
	// 引导期是否已关闭、仍在出现的未盖戳写。部署预检与「引导期能否关闭」(§8.1 第 4 步)都看这三个。
	persistSchemaGenGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "persist_schema_gen",
		Help:      "Player persist schema generation compiled into this go/db binary (docs/design/mixed-version-persist-field-protection.md §3).",
	})
	persistSchemaBootstrapClosed = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "persist_schema_bootstrap_closed",
		Help:      "1 once this zone's persist schema bootstrap period is closed (unstamped player writes are then dead-lettered as persist_unstamped); 0 while it is open.",
	})
	persistSchemaUnstampedWriteTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "persist_schema_unstamped_write_total",
		Help:      "Player table write bodies whose persist_schema_gen is 0, i.e. produced by a writer from before the mechanism. Must stay at 0 for a full save period before the bootstrap period may be closed.",
	})

	// retryQueueLength 是重试链路各列表的长度,重试循环每拍采样一次。ready、partitioned 与 dead 是同一 topic 所有实例
	// 共用的列表,每个副本报的是同一个数,告警里取 max;processing 是本实例自己的列表。
	// partitioned 是按原始分区拆开的 ready(kafka:retry:queue:{topic}:p{N},docs/design/db-retry-owner-routing.md)之和,
	// 任一 p 键读失败时本拍不更新;S1 之前的版本不报这个取值。
	retryQueueLength = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "retry_queue_length",
		Help:      "Length of the retry lists, sampled every retry tick. queue: ready (shared inbox of the topic) | partitioned (sum of the per-origin-partition ready lists kafka:retry:queue:{topic}:p{N}) | processing (this instance) | dead (shared by the topic).",
	}, []string{"queue"})

	// producerPartitionTotal 是 db_task 分区契约(docs/design/db-task-kafka-partition-contract.md「分区函数契约」)
	// 的消费端观测:ConsumeClaim 解码成功后,对每条消息按契约重算应在的分区并核对 Kafka key。只计数,不拒绝。
	// 写的 verdict!=ok 说明有生产者破坏契约(critical);读的不符说明还有旧版 login 在跑。
	// label 是封闭集合(3×4=12 条序列),不含 player_id(AGENTS.md §9)。
	producerPartitionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "task_producer_partition_total",
		Help:      "Kafka db_task messages checked against the producer partition contract (CRC-32(key) mod N, docs/design/db-task-kafka-partition-contract.md). op: read | write | other. verdict: ok | partition_mismatch | key_mismatch | invalid. Observation only: no message is rejected.",
	}, []string{"op", "verdict"})

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
			ownerEpochGuardTotal, staleOwnerWriteRejectedTotal,
			placementGuardTotal, placementStoreOpenTotal, placementOpenStores,
			taskDeferredTotal, taskDeadLetterTotal, taskReadAbandonedTotal, retryQueueLength)
		// 计数器序列从无到 1 时 increase() 会漏掉第一次(deploy/k8s/owner-epoch-alerts.yaml 记过这条经验),
		// 所以告警要用到的取值先以 0 建出序列。
		for _, outcome := range ownerEpochGuardOutcomes {
			ownerEpochGuardTotal.WithLabelValues(outcome).Add(0)
		}
		for _, op := range []string{"read", "write"} {
			for _, reason := range deferReasons {
				taskDeferredTotal.WithLabelValues(op, reason).Add(0)
			}
		}
		for _, reason := range deadLetterReasons {
			taskDeadLetterTotal.WithLabelValues(reason).Add(0)
		}
		for _, reason := range []string{ReadAbandonedStoreUnavailable, ReadAbandonedBudgetExhausted, ReadAbandonedSchemaTooNew, ReadAbandonedMissingPlayerRow} {
			taskReadAbandonedTotal.WithLabelValues(reason).Add(0)
		}
		// 世代仪表在 SetPersistSchemaGen / SetPersistSchemaBootstrapClosed 里赋值;计数器注册即以 0 存在。
		prometheus.MustRegister(persistSchemaGenGauge, persistSchemaBootstrapClosed, persistSchemaUnstampedWriteTotal)
		// db_task 分区契约观测:12 条序列全部以 0 建出,写不符的 critical 告警才能抓到第一次。
		prometheus.MustRegister(producerPartitionTotal)
		for _, op := range producerPartitionOps {
			for _, verdict := range producerPartitionVerdicts {
				producerPartitionTotal.WithLabelValues(op, verdict).Add(0)
			}
		}
		// 消费组按 zone 拆的交接闸(docs/design/db-consumer-group-per-zone.md):告警按 result / outcome 的具体取值与
		// conflict 计数写,先以 0 建出全部序列,第一次出现才抓得到。
		prometheus.MustRegister(consumerGroupRebalanceGapSeconds, consumerGroupHandoffTotal, consumerGroupHandoffGateTotal,
			consumerGroupHandoffWaitingSeconds, consumerGroupHandoffWaitSeconds, consumerGroupHandoffConflictTotal)
		for _, result := range handoffSeedResults {
			consumerGroupHandoffTotal.WithLabelValues(result).Add(0)
		}
		for _, outcome := range handoffGateOutcomes {
			consumerGroupHandoffGateTotal.WithLabelValues(outcome).Add(0)
		}
		consumerGroupHandoffConflictTotal.Add(0)
		// 重试只交给分区属主与入队落盘确认(docs/design/db-retry-owner-routing.md §11):告警按 mode / action / origin /
		// target / result 的具体取值写,先以 0 建出全部序列(retry_route 2×6、ordering_lock_busy 2、durable_enqueue 2×4、
		// retry_routing_mode 2)。db_retry_routing_mode 的两条都要存在:DbZoneNoPartitionClaims 数的是 mode="owner" 序列的
		// 条数(每个 S1 版 Pod 一条,与取值无关),SetRetryRoutingMode 只改值。
		prometheus.MustRegister(retryRouteTotal, ownedPartitionsGauge, retryRoutingModeGauge, orderingLockBusyTotal,
			durableEnqueueTotal, durableEnqueueSeconds, claimAcquiredTotal)
		for _, mode := range retryModes {
			for _, action := range retryRouteActions {
				retryRouteTotal.WithLabelValues(mode, action).Add(0)
			}
			retryRoutingModeGauge.WithLabelValues(mode).Set(0)
		}
		for _, origin := range orderingLockBusyOrigins {
			orderingLockBusyTotal.WithLabelValues(origin).Add(0)
		}
		for _, target := range durableTargets {
			for _, result := range durableResults {
				durableEnqueueTotal.WithLabelValues(target, result).Add(0)
			}
		}
		claimAcquiredTotal.Add(0)
		// 待重搬收据与消费组错误(R7 复核):告警按固定名字与 source 取值写,先建出序列。
		prometheus.MustRegister(retryStrandedReceipts, consumerGroupErrorsTotal)
		retryStrandedReceipts.Set(0)
		for _, source := range consumerGroupErrorSources {
			consumerGroupErrorsTotal.WithLabelValues(source).Add(0)
		}
	})
}

// 延后原因(db_task_deferred_total 的 reason)。前四个与 storeerr.Kind.Reason() 的取值一一对应。
const (
	DeferConnRefused           = "conn_refused"            // 连接数满(1040 / 1203 / 1226):服务端在应答,只拒新连接
	DeferConnLost              = "conn_lost"               // 连接断开或连不上
	DeferStoreReadOnly         = "store_readonly"          // 库只读(主从切换期间)
	DeferStoreBusy             = "store_busy"              // 锁等待超时、死锁、TiDB 写冲突
	DeferStoreCooldown         = "store_cooldown"          // 库在冷却期内(已确认不可达),选库时直接挡回
	DeferStoreOpenFailed       = "store_open_failed"       // 按需打开落点库失败
	DeferOrderingLockBusy      = "ordering_lock_busy"      // 排序锁被上一任持有者占着
	DeferFenceEvidenceConflict = "fence_evidence_conflict" // 同一围栏版本的 SQL 证据内容冲突
	DeferFenceRace             = "fence_race"              // 条件写竞态,仍耗重试预算
	DeferLeaseLost             = "lease_lost"              // 排序锁失租(SQL 前 / SQL 中被取消 / 写完核锁时发现)
	DeferPlacementFrozen       = "placement_frozen"        // 搬库冻结中
	DeferPlacementMoved        = "placement_moved"         // 落库后复核发现落点变了
	DeferRouteNotReady         = "route_not_ready"         // mapping Redis 的副本不可信(就绪标记,D1 第 3 批):写不耗预算、读耗
	DeferExpandLockBusy        = "expand_lock_busy"        // 扩容锁被占
	DeferRedisUnavailable      = "redis_unavailable"       // Redis 侧的网络错误或超时(守卫、选库、复核、缓存)
	DeferSchemaTooNew          = "schema_too_new"          // 写方的持久 schema 世代高于本实例(C1):写不耗预算,等新版实例处理
	DeferOther                 = "other"                   // 其它:SQL 的非存储侧错误、编程错误等,照旧耗预算
)

var deferReasons = []string{
	DeferConnRefused, DeferConnLost, DeferStoreReadOnly, DeferStoreBusy, DeferStoreCooldown, DeferStoreOpenFailed,
	DeferFenceEvidenceConflict, DeferFenceRace, DeferOrderingLockBusy, DeferLeaseLost, DeferPlacementFrozen, DeferPlacementMoved, DeferExpandLockBusy,
	DeferRedisUnavailable, DeferOther, DeferRouteNotReady, DeferSchemaTooNew,
}

// 进死信的原因(db_task_dead_letter_total 的 reason)。
const (
	DeadLetterFenceStaleOwner      = "fence_stale_owner"      // SQL 围栏拒绝旧持有者
	DeadLetterFenceLegacyAfterSeq  = "fence_legacy_after_seq" // 带序号写之后又来了无序号写
	DeadLetterOwnerEpochRegressed  = "owner_epoch_regressed"  // 计数器低于已落库 epoch,保留证据不得自动回灌
	DeadLetterBudgetExhausted      = "budget_exhausted"       // 写任务的重试次数用完
	DeadLetterMalformedPayload     = "malformed_payload"      // 重试载荷解不出来
	DeadLetterLegacyPayload        = "legacy_payload"         // 旧格式重试载荷,没有可信的原始分区
	DeadLetterInvalidPartition     = "invalid_partition"      // 原始分区越界或为负
	DeadLetterOrderingIncomparable = "ordering_incomparable"  // 游标不可比(跨分区等)
	DeadLetterStaleTopic           = "stale_topic"            // 写所在 topic 的 zone 不是玩家的 home_zone
	DeadLetterMissingRequired      = "missing_required"       // Placement.Required=true 且无落点记录的写
	DeadLetterPoison               = "poison"                 // Kafka 消息解不出 DBTask
	// 玩家持久 schema 世代(docs/design/mixed-version-persist-field-protection.md §6.2):两者都是确定性拒写,重试修复不了。
	DeadLetterPersistUnstamped = "persist_unstamped" // 引导期关闭后仍收到未盖戳的写(机制前的写方,C0)
	DeadLetterSchemaRegression = "schema_regression" // 写方世代低于 SQL 行世代:链路上有人截断过数据(C1b)
)

var deadLetterReasons = []string{
	DeadLetterBudgetExhausted, DeadLetterMalformedPayload, DeadLetterLegacyPayload, DeadLetterInvalidPartition,
	DeadLetterOrderingIncomparable, DeadLetterStaleTopic, DeadLetterMissingRequired, DeadLetterPoison, DeadLetterFenceStaleOwner, DeadLetterFenceLegacyAfterSeq, DeadLetterOwnerEpochRegressed,
	DeadLetterPersistUnstamped, DeadLetterSchemaRegression,
}

// 读任务被放弃的原因(db_task_read_abandoned_total 的 reason)。
const (
	ReadAbandonedStoreUnavailable = "store_unavailable"  // 库打不开或在冷却期内:立即回失败回执
	ReadAbandonedBudgetExhausted  = "budget_exhausted"   // 重试次数用完:丢弃,不进死信
	ReadAbandonedSchemaTooNew     = "schema_too_new"     // 行的持久 schema 世代高于本实例(C2):立即回失败回执,不回填缓存
	ReadAbandonedMissingPlayerRow = "player_row_missing" // 正式玩家双表缺行：立即回失败，不把请求体冒充真实存档
)

// SetPersistSchemaGen 记下本二进制的持久 schema 世代,启动时调用一次。
func SetPersistSchemaGen(gen uint32) {
	register()
	persistSchemaGenGauge.Set(float64(gen))
}

// SetPersistSchemaBootstrapClosed 记下本 zone 引导期是否已关闭。
func SetPersistSchemaBootstrapClosed(closed bool) {
	register()
	if closed {
		persistSchemaBootstrapClosed.Set(1)
		return
	}
	persistSchemaBootstrapClosed.Set(0)
}

// CountPersistSchemaUnstampedWrite 记一条未盖戳的玩家表写(重试会重复计,看的是「还在不在出现」)。
func CountPersistSchemaUnstampedWrite() {
	register()
	persistSchemaUnstampedWriteTotal.Inc()
}

// 重试链路的列表(db_retry_queue_length 的 queue)。
// 取值被 tools/scripts/stress_summarize.ps1 的 2f 段按精确字符串读取(ready / partitioned / processing / dead),
// 改名必须同改该段与 test_stress_summarize.ps1;TestRetryRoutingSeriesPrecreated 钉住 RetryQueuePartitioned。
const (
	RetryQueueReady       = "ready"
	RetryQueuePartitioned = "partitioned" // 按原始分区拆开的 ready(p*)之和
	RetryQueueProcessing  = "processing"
	RetryQueueDead        = "dead"
)

// CountTaskDeferred 记一次延后。op 为 read / write / unknown,reason 用 Defer* 常量。
func CountTaskDeferred(op, reason string) {
	register()
	taskDeferredTotal.WithLabelValues(op, reason).Inc()
}

// CountTaskDeadLetter 记一次进死信,reason 用 DeadLetter* 常量。
func CountTaskDeadLetter(reason string) {
	register()
	taskDeadLetterTotal.WithLabelValues(reason).Inc()
}

// CountReadAbandoned 记一次读任务被放弃,reason 用 ReadAbandoned* 常量。
func CountReadAbandoned(reason string) {
	register()
	taskReadAbandonedTotal.WithLabelValues(reason).Inc()
}

// SetRetryQueueLength 更新一个重试列表的长度,queue 用 RetryQueue* 常量。
func SetRetryQueueLength(queue string, n int64) {
	register()
	retryQueueLength.WithLabelValues(queue).Set(float64(n))
}

// db_task_producer_partition_total 的 op 取值:DBTask.op 为 read / write 原样记,其余一律 other。
const (
	ProducerPartitionOpRead  = "read"
	ProducerPartitionOpWrite = "write"
	ProducerPartitionOpOther = "other"
)

// db_task_producer_partition_total 的 verdict 取值。
const (
	ProducerPartitionOK          = "ok"                 // Kafka key == DBTaskKey(task.key),且分区 == CRC-32(key) mod N
	ProducerPartitionMismatch    = "partition_mismatch" // key 合规,但消息所在分区不是契约分区
	ProducerPartitionKeyMismatch = "key_mismatch"       // Kafka key 不是 DBTaskKey(task.key)(含 nil / 空 key)
	ProducerPartitionInvalid     = "invalid"            // 无法判定:消息或任务为空,或本消费者的分区数非正
)

var producerPartitionOps = []string{ProducerPartitionOpRead, ProducerPartitionOpWrite, ProducerPartitionOpOther}

var producerPartitionVerdicts = []string{
	ProducerPartitionOK, ProducerPartitionMismatch, ProducerPartitionKeyMismatch, ProducerPartitionInvalid,
}

// CountProducerPartition 记一次分区契约核对结果。op 用 ProducerPartitionOp* 常量,verdict 用 ProducerPartition* 常量。
func CountProducerPartition(op, verdict string) {
	register()
	producerPartitionTotal.WithLabelValues(op, verdict).Inc()
}

// ── 消费组按 zone 拆与不停服交接(docs/design/db-consumer-group-per-zone.md;go/db/internal/kafka/group_handoff.go)──
//
// 六个指标都不带 player_id,label 是封闭集合(4 + 8 个取值),每个 Pod 一份。告警见 deploy/k8s/db-task-retry-alerts.yaml
// 的 db-task.consumer-group-handoff 组。result / outcome 的取值**必须**与下面 HandoffSeed* / HandoffGate* 常量一一对应,
// 改常量同改 Help 与告警,理由同 ownerEpochGuardTotal(空表达式不报错、只静默不告警)。
var (
	// consumerGroupRebalanceGapSeconds 从一个 session 的 Cleanup 到下一个 session 的 Setup 完成,含交接闸前的等待与
	// Setup 失败后的重新入组;回答「再平衡让本 zone 的存盘消费停了多久」。进程第一个 session 没有前驱,不记。
	consumerGroupRebalanceGapSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "consumer_group_rebalance_gap_seconds",
		Help:      "Time from a consumer group session Cleanup to the next session Setup completing, including the group handoff gate wait. The first session of a process is not observed.",
		Buckets:   []float64{0.5, 1, 2, 3, 5, 10, 15, 20, 30, 45, 60, 120},
	})

	// consumerGroupHandoffTotal 是 Setup 里逐分区的播种结果:每个分区每个 session 至多记一次;已落定(settled)的分区
	// 不再查询,也不计数。
	consumerGroupHandoffTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "consumer_group_handoff_total",
		Help:      "Per-partition group handoff seeding results in session Setup. result: seeded (marked the peer group's committed offset, not +1) | own_present (this group already has a committed offset) | no_offsets (neither group has one; starts from Offsets.Initial) | aborted (offset query failed; Setup fails and the member rejoins).",
	}, []string{"result"})

	// consumerGroupHandoffGateTotal 是第一次入组前交接闸的每次判定(轮询 1s,所以 ≤1/s/Pod)。
	consumerGroupHandoffGateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "consumer_group_handoff_gate_total",
		Help:      "Group handoff gate verdicts before joining the consumer group, one per poll. outcome: no_peer_offsets | peer_released (pass) | wait_peer_holding | wait_peer_rebalancing | wait_peer_state_unknown | wait_own_behind_peer | wait_peer_unavailable | wait_own_unavailable (wait).",
	}, []string{"outcome"})

	// consumerGroupHandoffWaitingSeconds 是当前在交接闸前已经等了多久;没在等时为 0。告警按它的阈值写(45s / 300s)。
	consumerGroupHandoffWaitingSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "consumer_group_handoff_waiting_seconds",
		Help:      "How long this db process has been waiting at the group handoff gate so far; 0 when not waiting.",
	})

	// consumerGroupHandoffWaitSeconds 在交接闸放行时记一次本次等待的总时长。
	consumerGroupHandoffWaitSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "consumer_group_handoff_wait_seconds",
		Help:      "Total time spent at the group handoff gate, observed once when the gate releases.",
		Buckets:   []float64{0.5, 1, 2, 5, 10, 15, 30, 45, 60, 120, 300, 600, 1800},
	})

	// consumerGroupHandoffConflictTotal 记守望发现对方组正在消费本 topic(Stable、持有本 topic 分区、已提交位点前进)
	// 并让出本 session 的次数;非 0 多半是旧发布物被误部署。单独无 label,告警按固定名字引用。
	consumerGroupHandoffConflictTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "consumer_group_handoff_conflict_total",
		Help:      "Times the group handoff watcher saw the peer group consuming this topic (Stable, holding its partitions, committed offsets advancing) and yielded this session.",
	})
)

// db_consumer_group_handoff_total 的 result 取值。
const (
	HandoffSeedSeeded     = "seeded"      // 本组没有位点、对方有:MarkOffset(对方原值,不加 1)
	HandoffSeedOwnPresent = "own_present" // 本组已有位点:不动
	HandoffSeedNoOffsets  = "no_offsets"  // 两组都没有:从 Offsets.Initial(oldest)起步;从未写入的分区属正常
	HandoffSeedAborted    = "aborted"     // 位点查询失败:Setup 报错,成员 1s 后重新入组
)

var handoffSeedResults = []string{HandoffSeedSeeded, HandoffSeedOwnPresent, HandoffSeedNoOffsets, HandoffSeedAborted}

// db_consumer_group_handoff_gate_total 的 outcome 取值(修订稿 R5 / R12 后的 8 个;peer_unreachable_own_complete 已删除)。
const (
	HandoffGateNoPeerOffsets        = "no_peer_offsets"         // 放行:对方在本 topic 上没有位点,也没有成员持有分区
	HandoffGatePeerReleased         = "peer_released"           // 放行:对方 Empty / Dead,或 Stable 且不持有本 topic
	HandoffGateWaitPeerHolding      = "wait_peer_holding"       // 等待:对方 Stable 且持有本 topic 的分区
	HandoffGateWaitPeerRebalancing  = "wait_peer_rebalancing"   // 等待:对方 PreparingRebalance / CompletingRebalance
	HandoffGateWaitPeerStateUnknown = "wait_peer_state_unknown" // 等待:对方状态为空串或未知(只凭正面证据放行)
	HandoffGateWaitOwnBehindPeer    = "wait_own_behind_peer"    // 等待(R7):两组在同一分区都有位点且对方更大
	HandoffGateWaitPeerUnavailable  = "wait_peer_unavailable"   // 等待:对方位点或 DescribeGroups 查询失败
	HandoffGateWaitOwnUnavailable   = "wait_own_unavailable"    // 等待:本组位点查询失败,或 inspector 建不出来
)

var handoffGateOutcomes = []string{
	HandoffGateNoPeerOffsets, HandoffGatePeerReleased, HandoffGateWaitPeerHolding, HandoffGateWaitPeerRebalancing,
	HandoffGateWaitPeerStateUnknown, HandoffGateWaitOwnBehindPeer, HandoffGateWaitPeerUnavailable, HandoffGateWaitOwnUnavailable,
}

// ObserveRebalanceGap 记一次「上一个 session 结束 → 本 session Setup 完成」的间隔。
func ObserveRebalanceGap(d time.Duration) {
	register()
	consumerGroupRebalanceGapSeconds.Observe(d.Seconds())
}

// CountHandoffSeed 记一个分区的播种结果,result 用 HandoffSeed* 常量。
func CountHandoffSeed(result string) {
	register()
	consumerGroupHandoffTotal.WithLabelValues(result).Inc()
}

// CountHandoffGate 记一次交接闸判定,outcome 用 HandoffGate* 常量。
func CountHandoffGate(outcome string) {
	register()
	consumerGroupHandoffGateTotal.WithLabelValues(outcome).Inc()
}

// SetHandoffWaiting 更新当前在交接闸前已等待的时长;放行或退出等待时传 0。
func SetHandoffWaiting(d time.Duration) {
	register()
	consumerGroupHandoffWaitingSeconds.Set(d.Seconds())
}

// ObserveHandoffWait 在交接闸放行时记一次本次等待的总时长。
func ObserveHandoffWait(d time.Duration) {
	register()
	consumerGroupHandoffWaitSeconds.Observe(d.Seconds())
}

// CountHandoffConflict 记一次守望让出(对方组正在消费本 topic)。
func CountHandoffConflict() {
	register()
	consumerGroupHandoffConflictTotal.Inc()
}

// ── 重试只交给分区属主 + 重试 / 死信 / 毒消息入队的落盘确认(docs/design/db-retry-owner-routing.md;
// go/db/internal/kafka/retry_routing.go、durable_enqueue.go)──
//
// label 全是封闭集合(2×6、2、2×4 个取值),不含 player_id(AGENTS.md §9),每个 Pod 一份。告警见
// deploy/k8s/db-task-retry-alerts.yaml。mode / action / queue 的取值被 tools/scripts/stress_summarize.ps1 的 2f 段
// 按精确字符串读取(client_golang 按 label 名字母序输出,action 在 mode 之前,脚本的正则依赖这个顺序);
// 下面常量**改名必须同改 stress_summarize.ps1 的 2f 段与 test_stress_summarize.ps1**,并同改告警。
// 理由同 ownerEpochGuardTotal:按旧名字写的表达式只会静默落空,不报错。
var (
	// retryRouteTotal 是每条重试的路由决定。executed / executed_nonowner 在派发进 worker 通道时计
	// (不代表落库成功);forwarded 是 owner 实例把共享收件箱分拣进 p{N} 的条数;requeued_revoked 是 claim 撤销后
	// 原样搬回 p{origin} 的条数(派发前或 handleTask 复核时);misrouted 是放错 p 键、按载荷挪回 p{origin} 的条数;
	// dead 是路由阶段判进死信的条数(坏载荷 / 越界分区 / 次数用完,细分原因看 db_task_dead_letter_total)。
	retryRouteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "retry_route_total",
		Help:      "Retry routing decisions. mode: shared | owner. action: executed | executed_nonowner | forwarded | requeued_revoked | misrouted | dead.",
	}, []string{"mode", "action"})

	// ownedPartitionsGauge 是本进程此刻存活的 claim 租约数(= 本实例持有、可以执行 owner 重试的分区数)。
	// owner 模式下拿不到分区的热备为 0;一个 zone 所有 Pod 之和稳态应等于 PartitionCnt。
	ownedPartitionsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "owned_partitions",
		Help:      "Partition claims currently alive in this db process (claim leases that may execute owner-routed retries).",
	})

	// retryRoutingModeGauge 是本进程生效的 RetryRouting:生效的那个为 1、另一个为 0,两条序列都始终存在。
	// 切 owner 的前置是该 zone 每个 Pod 都暴露它(旧版 Pod 没有这条序列)。
	retryRoutingModeGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "retry_routing_mode",
		Help:      "Effective retry routing mode of this db process: 1 for the active mode, 0 for the other. mode: shared | owner.",
	}, []string{"mode"})

	// orderingLockBusyTotal 是任务因排序锁被占而延后的次数,按任务来源分。owner 模式稳态下 origin=retry 应接近 0
	// (重试与同分区新消息在同一个子 shard 串行,不再与别的实例抢锁)。
	orderingLockBusyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "ordering_lock_busy_total",
		Help:      "Tasks deferred because the per-key ordering lock was held by someone else. origin: kafka | retry.",
	}, []string{"origin"})

	// durableEnqueueTotal 只覆盖「Kafka 位点提交之前,任务唯一副本首次写进 Redis」的入队点(Kafka 源失败入重试、
	// Kafka 源隔离进死信、毒消息)。not_durable / error 时位点不提交、该 claim 失败并重投;canceled 是停机或调用方
	// ctx 取消(滚动重启时会增长,告警不看它)。重试源的搬移(LPUSH+LREM 脚本)不经过这里。
	durableEnqueueTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "durable_enqueue_total",
		Help:      "Enqueues of the only copy of a Kafka-origin task into Redis before its offset is committed (retry / dead / poison), confirmed by WAITAOF when enabled. target: retry | dead. result: ok | not_durable (group commit not confirmed within the batch budget: too few WAITAOF copies, or network errors to Redis until the budget ran out, so it can also mean Redis is unreachable) | error (deterministic error reply to LPUSH / WAITAOF, malformed WAITAOF reply, used before start, or every LPUSH attempt failed with WAITAOF disabled) | canceled (shutdown or caller context canceled; the offset stays uncommitted and replays).",
	}, []string{"target", "result"})

	// durableEnqueueSeconds 从提交给入队层到结果回调(含排队、LPUSH 与 WAITAOF);canceled 不观测。
	durableEnqueueSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "durable_enqueue_seconds",
		Help:      "Time from submitting a Kafka-origin task copy to the durable enqueue layer until its result (queueing + LPUSH + WAITAOF). Canceled requests are not observed. target: retry | dead.",
		Buckets:   stageBuckets,
	}, []string{"target"})

	// claimAcquiredTotal 每次 ConsumeClaim 登记租约加 1(一次 claim 开始一次),用来区分再平衡时刻的撤销与异常撤销
	// (DbRetryRevokedOutsideRebalance)。单独无 label,告警按固定名字引用。
	claimAcquiredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "claim_acquired_total",
		Help:      "Partition claims registered by ConsumeClaim (one per claim start); used to tell rebalance-time revocations from anomalies.",
	})

	// retryStrandedReceipts 是本实例「已认领进自己 processing、该搬走却因 Redis 出错没搬成」的收据数(retry_stranded.go):
	// 重试协程每拍重搬,搬成或确认已不在 processing 才摘掉。单独无 label,告警按固定名字引用;持续 > 0 说明 Redis
	// 一直拒绝这次搬移(处置见 runbook)。没有这张表时,这类收据只能等本实例重启或停机 release 才回到收件箱。
	retryStrandedReceipts = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "retry_stranded_receipts",
		Help:      "Retry receipts claimed into this instance's processing list whose move (requeue after revocation, misroute fix, reschedule, dead letter) failed on a Redis error; they are moved again on every retry tick. Stays > 0 while Redis keeps refusing the move.",
	})

	// consumerGroupErrorsTotal 是 sarama 消费组错误通道(Consumer.Return.Errors=true)上收到的错误数,按来源分
	// (consumer_group_errors.go)。这些错误 sarama 不打日志、通道满了就丢;以前没有任何代码读它,心跳失败、位点提交失败
	// 完全不可见。heartbeat 非 0 时本成员可能已被 coordinator 逐出,而本进程要过一段时间才察觉(双属主窗口,
	// retry_routing.go 文件头)。
	consumerGroupErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "consumer_group_errors_total",
		Help:      "Errors received on the sarama consumer group error channel. source: heartbeat (group level, not tied to a partition: heartbeat / coordinator lookup; the coordinator may already have evicted this member) | partition (per partition from sarama: offset commit or fetch) | claim (returned by this process's ConsumeClaim).",
	}, []string{"source"})
)

// db_consumer_group_errors_total 的 source 取值。
const (
	ConsumerGroupErrorHeartbeat = "heartbeat" // 不属于某个分区的组级错误:心跳、找 coordinator
	ConsumerGroupErrorPartition = "partition" // sarama 自己的分区级错误:位点提交或拉取(两者在错误通道上无法区分)
	ConsumerGroupErrorClaim     = "claim"     // 本进程 ConsumeClaim 返回的错误
)

var consumerGroupErrorSources = []string{ConsumerGroupErrorHeartbeat, ConsumerGroupErrorPartition, ConsumerGroupErrorClaim}

// db_retry_route_total / db_retry_routing_mode 的 mode 取值(stress_summarize.ps1 2f 段按这两个字符串读)。
const (
	RetryModeShared = "shared"
	RetryModeOwner  = "owner"
)

var retryModes = []string{RetryModeShared, RetryModeOwner}

// db_retry_route_total 的 action 取值(stress_summarize.ps1 2f 段按这六个字符串读)。
const (
	RetryRouteExecuted         = "executed"          // 派发给本地 worker:shared 模式下本实例持有该分区的 claim,或 owner 模式
	RetryRouteExecutedNonOwner = "executed_nonowner" // shared 模式下派发了一条本实例没有持有原始分区 claim 的重试
	RetryRouteForwarded        = "forwarded"         // owner:共享收件箱 → p{origin}
	RetryRouteRequeuedRevoked  = "requeued_revoked"  // owner:claim 撤销,收据原样搬回 p{origin},不执行、不耗预算
	RetryRouteMisrouted        = "misrouted"         // owner:收据放错了 p 键,按载荷里的原始分区挪回 p{origin}
	RetryRouteDead             = "dead"              // 路由阶段判进死信(或读任务次数用完被丢弃)
)

var retryRouteActions = []string{
	RetryRouteExecuted, RetryRouteExecutedNonOwner, RetryRouteForwarded,
	RetryRouteRequeuedRevoked, RetryRouteMisrouted, RetryRouteDead,
}

// db_ordering_lock_busy_total 的 origin 取值。
const (
	OriginKafka = "kafka"
	OriginRetry = "retry"
)

var orderingLockBusyOrigins = []string{OriginKafka, OriginRetry}

// db_durable_enqueue_total / db_durable_enqueue_seconds 的 target 与 result 取值。
const (
	DurableTargetRetry = "retry" // Kafka 源失败入重试 ready
	DurableTargetDead  = "dead"  // Kafka 源隔离进死信、毒消息

	// result 的归类见 durable_enqueue.go 的 durableEnqueueResult(canceled 先判,其次 not_durable,其余非 nil 为 error):
	//   - not_durable 只来自组提交(开启 WAITAOF):单批预算内没拿到足够确认,包括 WAITAOF 份数不足,以及连 Redis 的
	//     网络错误反复出现直到预算耗尽 —— 所以它也可能是 Redis 不可达或网络分区,不只是 AOF / fsync 慢;
	//   - error:确定性错误回复(LPUSH / WAITAOF 被拒)、WAITAOF 回复形状不对、未 start 就 submit,以及关闭 WAITAOF 时
	//     directEnqueue 的 4 次 LPUSH 全部失败(含网络错误)。同一种网络故障在两条路径上计成不同的 result。
	DurableOK         = "ok"          // 已按配置确认(关闭 WAITAOF 时 = LPUSH 成功)
	DurableNotDurable = "not_durable" // 组提交单批预算内没拿到足够确认(份数不足,或网络错误重试到预算耗尽)
	DurableError      = "error"       // 确定性错误回复、回复形状不对、未 start;关闭 WAITAOF 时 LPUSH 重试用尽
	DurableCanceled   = "canceled"    // 停机或调用方 ctx 取消:位点不提交、之后重投,不算故障
)

var (
	durableTargets = []string{DurableTargetRetry, DurableTargetDead}
	durableResults = []string{DurableOK, DurableNotDurable, DurableError, DurableCanceled}
)

// CountRetryRoute 记一次重试路由决定。mode 用 RetryMode* 常量,action 用 RetryRoute* 常量。
func CountRetryRoute(mode, action string) {
	register()
	retryRouteTotal.WithLabelValues(mode, action).Inc()
}

// CountRetryRouteN 一次记 n 条同类决定(分拣脚本一次搬多条);n ≤ 0 时什么都不做。
func CountRetryRouteN(mode, action string, n int) {
	if n <= 0 {
		return
	}
	register()
	retryRouteTotal.WithLabelValues(mode, action).Add(float64(n))
}

// SetOwnedPartitions 更新本进程存活的 claim 租约数。
func SetOwnedPartitions(n int) {
	register()
	ownedPartitionsGauge.Set(float64(n))
}

// SetRetryRoutingMode 把生效模式置 1、另一个置 0。mode 不是 RetryMode* 之一时两条都置 0 并记 ERROR
// (调用方应已校验过配置,走到这里说明是编程错误;宁可让告警看见「没有生效模式」,也不报一个猜出来的值)。
func SetRetryRoutingMode(mode string) {
	register()
	known := false
	for _, m := range retryModes {
		value := 0.0
		if m == mode {
			value = 1
			known = true
		}
		retryRoutingModeGauge.WithLabelValues(m).Set(value)
	}
	if !known {
		logx.Errorf("[metrics] unknown retry routing mode %q; db_retry_routing_mode set to 0 for every mode", mode)
	}
}

// CountOrderingLockBusy 记一次因排序锁被占而延后,origin 用 OriginKafka / OriginRetry。
func CountOrderingLockBusy(origin string) {
	register()
	orderingLockBusyTotal.WithLabelValues(origin).Inc()
}

// ObserveDurableEnqueue 记一次入队结果:target 用 DurableTarget*,result 用 Durable*,d 从提交到结果回调。
// canceled 只计数、不进直方图(停机时的取消耗时没有意义,还会把均值拉偏)。
func ObserveDurableEnqueue(target, result string, d time.Duration) {
	register()
	durableEnqueueTotal.WithLabelValues(target, result).Inc()
	if result == DurableCanceled {
		return
	}
	durableEnqueueSeconds.WithLabelValues(target).Observe(d.Seconds())
}

// CountClaimAcquired 记一次 claim 租约登记(ownedPartitions.acquire)。
func CountClaimAcquired() {
	register()
	claimAcquiredTotal.Inc()
}

// SetRetryStrandedReceipts 更新本实例待重搬的收据数。
func SetRetryStrandedReceipts(n int) {
	register()
	retryStrandedReceipts.Set(float64(n))
}

// CountConsumerGroupError 记一次 sarama 消费组错误,source 用 ConsumerGroupError* 常量。
func CountConsumerGroupError(source string) {
	register()
	consumerGroupErrorsTotal.WithLabelValues(source).Inc()
}

// CountTaskDeadLetterN 一次记 n 条进死信(分拣脚本一次搬多条坏载荷),reason 用 DeadLetter* 常量;n ≤ 0 时什么都不做。
func CountTaskDeadLetterN(reason string, n int) {
	if n <= 0 {
		return
	}
	register()
	taskDeadLetterTotal.WithLabelValues(reason).Add(float64(n))
}

// storeCooldownCollector 在每次抓取时现算 db_store_cooldown_stores:此刻仍在库级冷却期内(探活确认整库不可达)的库个数,
// 按连接池类别分。不用 Gauge 在进出冷却时设值:冷却到期后要等下一次选库探活才删记录,某个库到期后再也没有流量时
// 设值式的指标会一直挂着,告警跟着常亮。
type storeCooldownCollector struct {
	source func() map[string]int
	desc   *prometheus.Desc
}

func (c *storeCooldownCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *storeCooldownCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.source()
	for _, pool := range []string{PoolHome, PoolExtraZone, PoolExtraNonZone} {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[pool]), pool)
	}
}

// NewStoreCooldownCollector 只构造不注册(单测用自己的注册表)。
func NewStoreCooldownCollector(source func() map[string]int) prometheus.Collector {
	desc := prometheus.NewDesc(prometheus.BuildFQName("", subsystem, "store_cooldown_stores"),
		"Databases currently cooling down after a probe confirmed they are unreachable. pool: home | extra_zone | extra_non_zone.",
		[]string{"pool"}, nil)
	return &storeCooldownCollector{source: source, desc: desc}
}

// RegisterStoreCooldown 把落点库注册表的冷却计数接到默认注册表。进程内调一次;注册失败只记日志,不挡落库。
func RegisterStoreCooldown(source func() map[string]int) {
	register()
	if err := prometheus.Register(NewStoreCooldownCollector(source)); err != nil {
		logx.Errorf("[metrics] register store cooldown collector failed (db_store_cooldown_stores unavailable): %v", err)
	}
}

// 落点选库的 outcome label 取值(player-storage-placement.md §6.2 表的 outcome 列 + §14 的 recheck_moved
// + D1 第 3 批的 route_not_ready)。集合有界(9 个),op 只有 read / write,不含 player_id 等高基数维度(AGENTS.md §9)。
const (
	PlacementPlaced          = "placed"           // 有落点记录:落记录指向的库(读任务的冻结记录也算这一格)
	PlacementHome            = "home"             // 无记录、有 home_zone:落 home 库(写任务此时 home 必然 == 本进程 zone)
	PlacementLegacy          = "legacy"           // 无记录、无 home_zone:落本进程 zone 库(旧语义)
	PlacementStaleTopic      = "stale_topic"      // 写任务所在 topic 的 zone ≠ 玩家 home_zone:进死信,不落库(P-6)
	PlacementFrozenDeferred  = "frozen_deferred"  // 记录处于搬库冻结:写延后重试,不耗重试次数
	PlacementMissingRequired = "missing_required" // Placement.Required=true 且无记录:写进死信,读回失败
	PlacementLookupError     = "lookup_error"     // MGET 失败或值畸形:按可重试错误处理(耗重试次数),fail-closed
	PlacementRecheckMoved    = "recheck_moved"    // 落库后复核发现路由变化(或写任务遇到冻结):不标记游标、不写共享缓存,重试
	// PlacementRouteNotReady:Placement.RequireRouteReady=true 且 mapping Redis 的副本不可信(就绪标记不在 / 身份不符等,
	// spec_D1_batch1b2_2_3.md §5.1):值与缺键都不用来选库。选库时写延后不耗预算、读耗预算;复核时按「路由变了」重试。
	PlacementRouteNotReady = "route_not_ready"
)

// ── 读方就绪闸(D1 第 3 批,spec_D1_batch1b2_2_3.md §5.3 / §13.2)────────────────────────
//
// 只在 Placement.RequireRouteReady=true 时注册(PrimeRouteReady):开关缺省 false 时 /metrics 的输出与引入之前相同。
// verdict 的取值由调用方传入(shared/placement.RouteReadyVerdictNames 加 RouteReadyVerdictScriptError),本包不依赖 placement;
// 集合有界(7 个 verdict + script_error)。
var (
	routeReadyGate = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "route_ready_gate",
		Help:      "Process-level route ready gate (Placement.RequireRouteReady=true only): 1 = the last refresh found the mapping Redis replica trusted and Kafka / retry consumption proceeds; 0 = consumption paused (marker absent or untrusted, or the read script failed). Not exported while the switch is off.",
	})

	routeReadyVerdictTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "route_ready_verdict_total",
		Help:      "Route ready read verdicts (spec D1 M1) from the gate refresh, the placement lookup and the post-write recheck. verdict: trusted | absent | malformed | newer_version | identity_unavailable | not_master | identity_mismatch, or script_error (a gate refresh whose read script failed or replied in an unexpected shape: mapping Redis unreachable, timeout, INFO not allowed; same label value as data_service and guild). A script error in a per-task lookup or recheck is counted as db_placement_guard_total{outcome=\"lookup_error\"} instead. Not exported while Placement.RequireRouteReady=false.",
	}, []string{"verdict"})

	routeReadyOnce sync.Once
)

func registerRouteReady() {
	routeReadyOnce.Do(func() {
		prometheus.MustRegister(routeReadyGate, routeReadyVerdictTotal)
	})
}

// PrimeRouteReady 注册就绪闸的两个指标并以 0 建出全部 verdict 序列与闸仪表(告警用 increase(),序列从无到 1 时会漏掉第一次)。
// 只在 Placement.RequireRouteReady=true 装配就绪闸时调用;可重复调用。
func PrimeRouteReady(verdicts []string) {
	registerRouteReady()
	for _, verdict := range verdicts {
		routeReadyVerdictTotal.WithLabelValues(verdict).Add(0)
	}
	routeReadyGate.Set(0)
}

// SetRouteReadyGate 记录就绪闸当前是否放行。
func SetRouteReadyGate(open bool) {
	registerRouteReady()
	if open {
		routeReadyGate.Set(1)
		return
	}
	routeReadyGate.Set(0)
}

// CountRouteReadyVerdict 记一次读脚本的结论,verdict 取 placement.RouteReadyVerdict.String() 或 RouteReadyVerdictScriptError。
func CountRouteReadyVerdict(verdict string) {
	registerRouteReady()
	routeReadyVerdictTotal.WithLabelValues(verdict).Inc()
}

// RouteReadyVerdictScriptError 是 db_route_ready_verdict_total{verdict} 在 placement 的 7 个 verdict 之外的一个取值:
// 读脚本没执行成功或回复形状不对(连不上 mapping Redis、超时、脚本里调不了 INFO),没有得到 verdict。
// 取值与 data_service(metrics.RouteReadyVerdictScriptError)、guild(guild_route_ready_verdict_total)逐字相同,
// 跨服务的「读方 verdict 非 trusted」面板与告警按同一个字符串过滤。调用方把它追加在 placement.RouteReadyVerdictNames()
// 之后交给 PrimeRouteReady 预置。
const RouteReadyVerdictScriptError = "script_error"

// 按需打开落点库的 result label 取值。
const (
	StoreOpenOK       = "ok"
	StoreOpenError    = "error"
	StoreOpenRejected = "rejected"
)

// 连接池类别(mmorpg_sql_pool_*{sql_client="db",pool=...},G2b);封闭集合,不含库名(落点库数不设上限)。
// deploy/k8s/mysql-connection-alerts.yaml 的 DbExtraStorePoolsBeyondPlan 按 extra_zone / extra_non_zone 两个取值写死,
// 改常量必须同改告警。
const (
	PoolHome         = "home"           // 本 zone 库(InitDB 打开,Database.MaxOpenConn)
	PoolExtraZone    = "extra_zone"     // 额外 zone 家族库 zone_{1..999999}_db(本 zone 库除外)
	PoolExtraNonZone = "extra_non_zone" // 非 zone 落点库 player_store_{>=1000000}_db
)

// NewSQLPoolCollector 只构造不注册(单测用自己的注册表)。sql_client 取本包的 subsystem("db")。
func NewSQLPoolCollector(source sqlpoolstats.Source) prometheus.Collector {
	return sqlpoolstats.NewCollector(subsystem, []string{PoolHome, PoolExtraZone, PoolExtraNonZone}, source)
}

// RegisterSQLPools 把连接池快照接到默认注册表(Start 暴露的那个)。进程内调一次;注册失败只记日志:
// 指标是观测面,不能挡住落库(与 Start 同口径)。MetricsListenAddr 留空时调用也无害(没人抓)。
func RegisterSQLPools(source sqlpoolstats.Source) {
	register()
	if err := prometheus.Register(NewSQLPoolCollector(source)); err != nil {
		logx.Errorf("[metrics] register sql pool collector failed (pool metrics unavailable): %v", err)
	}
}

// CountPlacementGuard 记一次选库 / 复核判定。op 为 "read" / "write",outcome 用上面的 Placement* 常量。
func CountPlacementGuard(op, outcome string) {
	register()
	placementGuardTotal.WithLabelValues(op, outcome).Inc()
}

// CountPlacementStoreOpen 记一次按需打开落点库的结果,result 用 StoreOpen* 常量。
func CountPlacementStoreOpen(result string) {
	register()
	placementStoreOpenTotal.WithLabelValues(result).Inc()
}

// SetPlacementOpenStores 更新常驻落点库个数。
func SetPlacementOpenStores(n int) {
	register()
	placementOpenStores.Set(float64(n))
}

// 归属 epoch 守卫的 outcome label 取值。与 internal/kafka 的守卫判定一一对应;
// 集合有界(7 个),不含 player_id 等高基数维度(AGENTS.md §9)。
const (
	OwnerEpochMatch      = "match"       // 任务 epoch == 已落库的最大 epoch:同一持有者的后续写,放行
	OwnerEpochLegacyZero = "legacy_zero" // 任务 epoch == 0:旧版生产者,兼容窗口内放行不比对
	OwnerEpochFirst      = "first"       // 该 key 还没有带 epoch 的写落过库,放行
	OwnerEpochAdvance    = "advance"     // 任务 epoch > 已落库的最大 epoch:归属变更后新主的第一笔写,放行
	OwnerEpochStale      = "stale"       // 任务 epoch < 已落库的最大 epoch:新主落库之后才到的老 epoch 写,拒绝
	OwnerEpochReadError  = "read_error"  // Redis 读失败:按可重试错误处理,不放行也不丢
	OwnerEpochRegressed  = "regressed"   // 计数器低于 applied epoch 或 SQL 行围栏:拒绝并保留证据
)

var ownerEpochGuardOutcomes = []string{
	OwnerEpochMatch, OwnerEpochLegacyZero, OwnerEpochFirst, OwnerEpochAdvance, OwnerEpochStale, OwnerEpochReadError, OwnerEpochRegressed,
}

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
	// 以下四个的含义见包注释;名字被 tools/scripts/stress_summarize.ps1 的 2d 段按精确 key 读取,改名同改脚本。
	StageSQL       = "sql"
	StageTaskTotal = "task_total"
	StageQueueWait = "queue_wait"
	StageKafkaE2E  = "kafka_e2e"
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
	// pipeline.go 的指标走独立的 sync.Once;在暴露端点之前注册,告警要用的取值才会以 0 出现在第一次抓取里。
	registerPipeline()
	// task_result_notify.go 同理(读回执通知分频道,B0-3 第 2 批)。
	registerTaskResultNotify()
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

// ── 落点计数器预置(deploy/k8s/player-placement-alerts.yaml)────────────────────────
//
// db_placement_guard_total 与 db_placement_store_open_total 原先只在第一次计数时才建出序列:increase() 对「序列从无到 1」
// 只有一个样本,恰好漏掉第一次,「出现一次就报」的告警(stale_topic / missing_required / rejected / error)因此在告警文件里
// 多写了一支 `unless … offset 10m`。PrimePlacement 以 0 建出两者的全部序列。label 都是封闭集合(op 2 × outcome 9 = 18 条,
// result 3 条),不含 player_id(AGENTS.md §9)。

// placementGuardOps / placementGuardOutcomes 是 db_placement_guard_total 的封闭 label 集合,outcome 与上面的 Placement* 常量
// 一一对应。其中 read×stale_topic、read×frozen_deferred 不可达(读不适用 P-6、不看冻结),预置后恒为 0;route_not_ready 只在
// Placement.RequireRouteReady=true 时出现,与 db_task_deferred_total{reason="route_not_ready"} 同口径无条件预置,开关关着时恒为 0。
var (
	placementGuardOps      = []string{"read", "write"}
	placementGuardOutcomes = []string{
		PlacementPlaced, PlacementHome, PlacementLegacy, PlacementStaleTopic, PlacementFrozenDeferred,
		PlacementMissingRequired, PlacementLookupError, PlacementRecheckMoved, PlacementRouteNotReady,
	}
	placementStoreOpenResults = []string{StoreOpenOK, StoreOpenError, StoreOpenRejected}
)

// PrimePlacement 注册指标并以 0 建出 db_placement_guard_total 与 db_placement_store_open_total 的全部序列。由按落点选库的
// 消费者装配时调用(kafka.NewKeyOrderedKafkaConsumer,早于 Start 暴露 /metrics,第一次抓取就有这些序列);可重复调用。
func PrimePlacement() {
	register()
	for _, op := range placementGuardOps {
		for _, outcome := range placementGuardOutcomes {
			placementGuardTotal.WithLabelValues(op, outcome).Add(0)
		}
	}
	for _, result := range placementStoreOpenResults {
		placementStoreOpenTotal.WithLabelValues(result).Add(0)
	}
}
