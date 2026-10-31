// Package metrics exposes Prometheus counters / histograms for data_service
// cross-server observability: 归属映射、账号目录、发号、名字、账本读与审计落库。玩家存档的存盘指标在 go/db。
//
// Style mirrors scene_manager/internal/metrics — Observe* helpers that lazy-
// register on first use. Keep label cardinality low: never label by player_id.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"shared/sqlpoolstats"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
)

const subsystem = "data_service"

var (
	// ── Kafka 落库消费者(transaction_log / player_snapshot)──────
	// up=0 且进程还活着 = 消费者因 DB 故障停下(宁可积压不丢审计),必须告警:
	// 积压超过 topic 保留期(默认 30 天)就真丢了。
	kafkaConsumerUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_up",
		Help:      "1 while the consumer loop is running, 0 once it stopped (DB failure or shutdown).",
	}, []string{"consumer"})

	kafkaConsumerMessagesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_messages_total",
		Help:      "Consumed messages by consumer (transaction_log|player_snapshot) and outcome (inserted|duplicate|decode_error|invalid|oversize|db_error).",
	}, []string{"consumer", "outcome"})

	// ── 审计管道观测(G2a)────────────────────────────────────────
	// label 只有 consumer / result,全部是封闭集合;**不要**按分区或 player_id 出序列(AGENTS.md §9)。
	// 告警规则见 deploy/k8s/data-service-audit-alerts.yaml。

	// 单次 DB 尝试的耗时:拿到落库名额之后到 sink 返回,不含名额排队与重试退避;ctx 取消导致的失败不计。
	// 所以它慢 = 库慢(全局库开始排队),不会被本实例自己的并发上限或一次重试带偏。
	kafkaConsumerFlushSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_flush_seconds",
		Help:      "Latency of one database attempt of a consumer flush (after acquiring the flush slot; excludes slot wait and retry backoff), by consumer and result (ok|error).",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"consumer", "result"})

	// 每次 flush 的有效行数(纯坏消息的批不计,免得 0 拉低均值)。
	kafkaConsumerBatchRows = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_batch_rows",
		Help:      "Rows written per consumer flush (flushes carrying only skipped messages are not observed).",
		Buckets:   []float64{1, 2, 4, 8, 16, 32, 64, 128, 256},
	}, []string{"consumer"})

	// 每条消息 value 的字节数(含坏消息)。1000000 那一档对着 C++ 生产者 librdkafka 默认的 message.max.bytes:
	// 超过它的快照在生产端就被拒了,这里看不到 —— 所以 p99 贴近这一档就该查 scene 日志的 "Kafka send failed"。
	kafkaConsumerMessageBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_message_bytes",
		Help:      "Size of each consumed message value in bytes (bad messages included).",
		Buckets:   []float64{1 << 10, 16 << 10, 64 << 10, 128 << 10, 256 << 10, 512 << 10, 768 << 10, 1000000, 2 << 20, 4 << 20, 8 << 20, 16 << 20},
	}, []string{"consumer"})

	// broker 上这个消费组在这个 topic 上还差多少条(全部分区求和)。每个实例报同一个数,告警取 max。
	kafkaConsumerLagMessages = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_lag_messages",
		Help:      "Consumer group lag summed over all partitions, as seen by the broker (every replica reports the same group value; alert on max).",
	}, []string{"consumer"})

	// 同一个组在这个 topic 上**单个分区**的最大积压。broker 的 retention.bytes 按分区执行:积压集中在少数分区时
	// (持有它的实例卡住 / 毒丸行卡住所在分区)合计值还远没到线,那个分区已经开始被删 —— 「离删除线还有多远」的告警比的是它,
	// 阈值因此与分区数无关。每个 consumer 一条序列,基数恒定;**不是**按分区出序列。每个实例报同一个数,告警取 max。
	kafkaConsumerLagMaxPartitionMessages = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_lag_max_partition_messages",
		Help:      "Largest single-partition lag of the consumer group on its topic, as seen by the broker (retention.bytes is enforced per partition; every replica reports the same group value; alert on max).",
	}, []string{"consumer"})

	// 保留期已经删掉、而这个组还没消费到的消息条数:> 0 = 审计数据已经丢了。
	// 消费者越过缺口并提交之后它会自行归零 —— 「丢了多少」以日志为准(kafka-go v0.4.47,已按源码核对,见 kafka.newReader 的说明):
	// 运行中被保留期追上是 [kafka-go] 的 `skipping from offset X to Y (N messages)` 行(精确);reader (重新)初始化时
	// (进程重启 / 停机冷却后重建 / rebalance / 断线重连)kafka-go 静默跳到日志起点,看 [kafka-go] 的 `starting at offset` 与
	// `seeking to offset` 两行之差;lag 探针的 `AUDIT DATA LOST` 行是 30s 采样值,只是下界。
	kafkaConsumerRetentionGapMessages = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_retention_gap_messages",
		Help:      "Messages deleted by topic retention before this consumer group consumed them (sum over partitions of log-start minus committed). > 0 means audit data is lost.",
	}, []string{"consumer"})

	kafkaConsumerLagProbeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_lag_probe_total",
		Help:      "Consumer group lag probe outcomes by consumer and result (ok|error).",
	}, []string{"consumer", "result"})

	kafkaConsumerIsolationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "kafka_consumer_isolation_total",
		Help:      "Row-by-row fallback after a failed batch insert, by consumer and result (recovered: every row landed | stopped: a row could not be inserted and the consumer stopped).",
	}, []string{"consumer", "result"})

	// ── AllocateIdSegment(号段发号)───────────────────────────────
	idSegmentAllocateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "id_segment_allocate_total",
		Help:      "AllocateIdSegment outcomes by biz_tag (ok|invalid|exhausted|unknown_tag|records_missing|source_error).",
	}, []string{"biz_tag", "outcome"})

	// idIssuerReserveTotal 是发号器预留块的结果。ok 正常很少发生;one_record_missing / record_behind
	// 说明某一份预留记录丢失或回退过,ahead_of_clock 说明时钟曾跳到未来,都要人看一眼。
	idIssuerReserveTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "id_issuer_reserve_total",
		Help:      "Id issuer block reservations by result (ok|conflict|store_error|refused|one_record_missing|record_behind|ahead_of_clock).",
	}, []string{"result"})

	// ── 玩家名字注册表(设计 docs/design/guild-phase2/03-names.md §3.6)────
	//
	// 指标名在设计里写死成 data_service_player_name_*;Subsystem="data_service" 加
	// 下面的 Name 拼出来正是那三个名字,不需要也不应该再写一遍前缀。
	//
	// 【为什么不放在 logic 里用 go-zero 的 core/metric】go-zero 的 metric.Inc /
	// ObserveFloat 内部都先问 prometheus.Enabled(),那个开关只有 metric agent
	// (yaml 里的 Prometheus 段 → StartAgent)才会打开。data_service 没有那一段,
	// 也不该加——加了会再起一个 /metrics 端口,与 MetricsListenAddr 上已有的这套并存。
	// 用 core/metric 在本服务里的实际效果是:指标永远是 0,而且不报任何错。
	playerNameOpsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "player_name_ops_total",
		Help: "Player name registry outcomes by op (reserve|release|batch_get) and result " +
			"(ok|already_owned|taken|invalid|conflict|outside_window|error). already_owned is the " +
			"idempotent reserve retry: a rising rate means callers are retrying CreatePlayer.",
	}, []string{"op", "result"})

	// 桶按"一次本地 SQL"的量级铺:1ms 是理想值,25ms 以内算健康,超过 250ms 说明
	// 全局库开始排队(建角是同步路径,reserve 的 p99 直接进 CreatePlayer 的耗时预算)。
	playerNameOpSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "player_name_op_seconds",
		Help:      "Player name registry op latency in seconds, cache + SQL included.",
		Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"op"})

	playerNameCacheTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "player_name_cache_total",
		Help: "BatchGetPlayerName cache lookups counted per id: hit (name cached) | " +
			"negative_hit (cache remembers the row is absent) | miss (falls through to MySQL).",
	}, []string{"result"})

	// ── 资产通道:已落盘账本读取(docs/design/guild-phase2/07-rollback-fail-closed.md §7.9.1)──
	//
	// 调用方是 guild 重投循环的离线读账本(LedgerReadMinAttempts 之后才读),量很小。
	// absent 持续偏高 = 被问到的玩家在 zone Redis 里没有 blob(从未入场或 Redis 丢数据);
	// error 非零 = 路由 / Redis / blob 矛盾,详情看同一时刻的 ERROR 日志。
	assetOpLedgerReadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "asset_op_ledger_read_total",
		Help:      "GetPlayerAssetOpLedger outcomes: found (row read, ledger returned) | absent (no owner row) | error (routing, Redis or corrupt blob).",
	}, []string{"result"})

	// ── 玩家收件箱:已落盘账本读取(docs/design/player-inbox-service.md §9.3)──
	//
	// 调用方是 go/player_inbox 的对账(领导者每小时一轮 + 运维 ReconcilePlayer),GM 域量很小。
	// result 集合与上面的资产账本相同;absent 由对账计为 unknown、不告警;error 非零看同一时刻的 ERROR 日志。
	playerInboxLedgerReadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "player_inbox_ledger_read_total",
		Help:      "GetPlayerInboxLedger outcomes: found (row read, ledger returned) | absent (no owner row) | error (routing, Redis or corrupt blob).",
	}, []string{"result"})

	// ── 玩家归属映射(player_route 只在全局库一份,docs/design/player-route-mapping-durability-proposal.md §15)──
	//
	// 两个 label 都是封闭集合,启动时由 PrimeRoute 全部预置为 0。**不要**把 player_id 加成 label(AGENTS.md §9),
	// 逐条明细在 `[RegisterPlayerZone]` / `[DataRoute]` 的 ERROR 日志里。
	//
	// RegisterPlayerZone 的 error_code 只在 gRPC status 的 message 里,grpcstats 只分得出 Unavailable、分不出码,
	// 所以每次 W1 恰好计一次结局:库里的判定走这个计数,不靠状态码。
	routeRegisterTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "route_register_total",
		Help: "RegisterPlayerZone outcomes, exactly one per request: inserted|filled_home|filled_home_placement_kept|already_same " +
			"(committed in player_route) | conflict (player_route holds another home zone) | store_error|invalid.",
	}, []string{"outcome"})

	// player_route 的 SQL 读:home 每次读计一次,batch 每块计一次。error 持续非零 = 全局库读失败,归属查询整体 fail-closed;
	// 调用方自己取消 / 超时不计(由 gRPC 层的状态码统计)。逐条明细在 `[DataRoute]` 日志里。
	routeReadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "route_read_total",
		Help: "player_route SQL reads: path home|batch (per chunk), result ok (row read) | not_found (authoritative absence, home only) | " +
			"error (the read failed; the RPC answers Unavailable).",
	}, []string{"path", "result"})

	registerOnce sync.Once
)

// route_register_total{outcome} 的取值(封闭集合,PrimeRoute 逐个预置;新增取值必须同时加进 routeRegisterOutcomes)。
const (
	RouteRegisterInserted                = "inserted"
	RouteRegisterFilledHome              = "filled_home"
	RouteRegisterFilledHomePlacementKept = "filled_home_placement_kept"
	RouteRegisterAlreadySame             = "already_same"
	RouteRegisterConflict                = "conflict"
	RouteRegisterStoreError              = "store_error"
	RouteRegisterInvalid                 = "invalid"
)

// route_read_total{path,result} 的取值(封闭集合,PrimeRoute 逐个预置;新增取值必须同时加进 routeReadSeries)。
const (
	RouteReadPathHome  = "home"
	RouteReadPathBatch = "batch"

	RouteReadResultOK       = "ok"
	RouteReadResultNotFound = "not_found"
	RouteReadResultError    = "error"
)

var (
	routeRegisterOutcomes = []string{
		RouteRegisterInserted,
		RouteRegisterFilledHome,
		RouteRegisterFilledHomePlacementKept,
		RouteRegisterAlreadySame,
		RouteRegisterConflict,
		RouteRegisterStoreError,
		RouteRegisterInvalid,
	}

	// routeReadSeries 是 route_read_total 里**会出现**的 path × result 组合(5 条):batch 没有逐 id 的缺席结论,
	// 不出现的组合不预置,免得面板上多出恒为 0、看着像「在跑」的序列。
	routeReadSeries = [][2]string{
		{RouteReadPathHome, RouteReadResultOK},
		{RouteReadPathHome, RouteReadResultNotFound},
		{RouteReadPathHome, RouteReadResultError},
		{RouteReadPathBatch, RouteReadResultOK},
		{RouteReadPathBatch, RouteReadResultError},
	}
)

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			kafkaConsumerUp, kafkaConsumerMessagesTotal,
			kafkaConsumerFlushSeconds, kafkaConsumerBatchRows, kafkaConsumerMessageBytes,
			kafkaConsumerLagMessages, kafkaConsumerLagMaxPartitionMessages, kafkaConsumerRetentionGapMessages,
			kafkaConsumerLagProbeTotal, kafkaConsumerIsolationTotal,
			idSegmentAllocateTotal,
			idIssuerReserveTotal,
			playerNameOpsTotal, playerNameOpSeconds, playerNameCacheTotal,
			assetOpLedgerReadTotal,
			playerInboxLedgerReadTotal,
			routeRegisterTotal,
			routeReadTotal,
		)
	})
}

// PrimeRoute 把 route_register_total(7 条)与 route_read_total(5 条)的封闭集合逐个预置为 0(data_service.go 启动路径):
// store_error 这类一辈子可能只发生几次的结局,序列不预先存在的话 `increase(...) > 0` 恰好漏掉第一次。
func PrimeRoute() {
	register()
	for _, outcome := range routeRegisterOutcomes {
		routeRegisterTotal.WithLabelValues(outcome).Add(0)
	}
	for _, s := range routeReadSeries {
		routeReadTotal.WithLabelValues(s[0], s[1]).Add(0)
	}
}

// ObserveRouteRegister 记一次 RegisterPlayerZone 的结局。outcome 取 RouteRegister* 常量(封闭集合),本函数不校验。
func ObserveRouteRegister(outcome string) {
	register()
	routeRegisterTotal.WithLabelValues(outcome).Inc()
}

// ObserveRouteRead 记一次 player_route 的 SQL 读。path / result 取 RouteRead* 常量(封闭集合),本函数不校验。
func ObserveRouteRead(path, result string) {
	register()
	routeReadTotal.WithLabelValues(path, result).Inc()
}

// merged_zone_refresh_total{result} 的取值:定时重读 zone_merged_into 的结局(placement.MergedZoneCache)。
const (
	MergedZoneRefreshOK    = "ok"
	MergedZoneRefreshError = "error"
)

var (
	mergedZoneRefreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "merged_zone_refresh_total",
		Help:      "Periodic zone_merged_into reloads; on error the previous mapping is kept, so sustained errors mean merged zone resolution is going stale.",
	}, []string{"result"})
	mergedZoneRegisterOnce sync.Once
)

func registerMergedZone() {
	mergedZoneRegisterOnce.Do(func() { prometheus.MustRegister(mergedZoneRefreshTotal) })
}

// PrimeMergedZoneRefresh 预置两条序列为 0(理由同 PrimeRoute:error 可能很久才发生一次)。可重复调用。
func PrimeMergedZoneRefresh() {
	registerMergedZone()
	mergedZoneRefreshTotal.WithLabelValues(MergedZoneRefreshOK).Add(0)
	mergedZoneRefreshTotal.WithLabelValues(MergedZoneRefreshError).Add(0)
}

// ObserveMergedZoneRefresh 记一次定时重读的结局。result 取 MergedZoneRefresh* 常量。
func ObserveMergedZoneRefresh(result string) {
	registerMergedZone()
	mergedZoneRefreshTotal.WithLabelValues(result).Inc()
}

// 连接池类别(mmorpg_sql_pool_*{sql_client="data_service",pool=...},G2b):一个 store 一个独立连接池,类别即 store 名。
// 封闭集合;svc.ServiceContext.SQLPoolSamples 按这些常量报样本。
const (
	PoolSnapshot       = "snapshot"        // player_snapshot / rollback_audit_log(SnapshotMySQL.MaxOpenConn)
	PoolTransactionLog = "transaction_log" // transaction_log(SnapshotMySQL.MaxOpenConn)
	PoolPlayerName     = "player_name"     // player_name(PlayerName.MaxOpenConn)
	PoolPlayerRoute    = "player_route"    // 路由表(RouteStore.MaxOpenConn)
)

// NewSQLPoolCollector 只构造不注册(单测用自己的注册表)。sql_client 取本包的 subsystem("data_service")。
func NewSQLPoolCollector(source sqlpoolstats.Source) prometheus.Collector {
	return sqlpoolstats.NewCollector(subsystem, []string{PoolSnapshot, PoolTransactionLog, PoolPlayerName, PoolPlayerRoute}, source)
}

// RegisterSQLPools 把连接池快照接到默认注册表(Start 暴露的那个)。进程内调一次(data_service.go 启动路径;
// 不能放进 svc.NewServiceContext:单测会多次构造 ServiceContext,默认注册表重复注册会报错)。
// 注册失败只记日志:指标是观测面,不能挡住业务。
func RegisterSQLPools(source sqlpoolstats.Source) {
	register()
	if err := prometheus.Register(NewSQLPoolCollector(source)); err != nil {
		logx.Errorf("[metrics] register sql pool collector failed (pool metrics unavailable): %v", err)
	}
}

// SetKafkaConsumerUp flips the up gauge for one consumer ("transaction_log" | "player_snapshot").
func SetKafkaConsumerUp(consumer string, up bool) {
	register()
	v := 0.0
	if up {
		v = 1
	}
	kafkaConsumerUp.WithLabelValues(consumer).Set(v)
}

// ObserveKafkaConsumerMessages adds n messages with one outcome for one consumer.
func ObserveKafkaConsumerMessages(consumer, outcome string, n int) {
	if n <= 0 {
		return
	}
	register()
	kafkaConsumerMessagesTotal.WithLabelValues(consumer, outcome).Add(float64(n))
}

// kafka_consumer_* 审计管道指标(G2a)的封闭取值集合,PrimeKafkaConsumer 逐个预置;新增取值必须同时加进下面的切片。
const (
	KafkaResultOK    = "ok"
	KafkaResultError = "error"

	KafkaIsolationRecovered = "recovered"
	KafkaIsolationStopped   = "stopped"
)

var (
	// kafkaConsumerOutcomes 是 kafka_consumer_messages_total{outcome} 的全集(与它的 Help 一致)。
	kafkaConsumerOutcomes = []string{"inserted", "duplicate", "decode_error", "invalid", "oversize", "db_error"}
	kafkaLagProbeResults  = []string{KafkaResultOK, KafkaResultError}
	kafkaIsolationResults = []string{KafkaIsolationRecovered, KafkaIsolationStopped}
)

// PrimeKafkaConsumer 把一条消费者的 messages_total(6 个 outcome)、lag_probe_total(ok / error)、
// isolation_total(recovered / stopped)逐个预置为 0。CounterVec 的子序列在首次 WithLabelValues 之前不存在:坏消息跳过、保留期丢失、
// 批量兜底停机这类一辈子可能只发生几次的结局,序列不预先存在的话 `increase(...) > 0` 恰好漏掉第一次。
// 启动路径(data_service.go startKafkaConsumers)对两条消费者各调一次;txlog 没有逐条兜底,它的 isolation 序列恒为 0,无害。
func PrimeKafkaConsumer(consumer string) {
	register()
	for _, outcome := range kafkaConsumerOutcomes {
		kafkaConsumerMessagesTotal.WithLabelValues(consumer, outcome).Add(0)
	}
	for _, result := range kafkaLagProbeResults {
		kafkaConsumerLagProbeTotal.WithLabelValues(consumer, result).Add(0)
	}
	for _, result := range kafkaIsolationResults {
		kafkaConsumerIsolationTotal.WithLabelValues(consumer, result).Add(0)
	}
}

// ObserveKafkaConsumerFlushAttempt 记一次 DB 尝试的耗时;result 取 KafkaResultOK / KafkaResultError。
// 调用方只计"拿到落库名额之后到 sink 返回"这一段,ctx 取消导致的失败不要调(关停 / 兄弟分区停机不是 DB 慢)。
func ObserveKafkaConsumerFlushAttempt(consumer, result string, d time.Duration) {
	register()
	kafkaConsumerFlushSeconds.WithLabelValues(consumer, result).Observe(d.Seconds())
}

// ObserveKafkaConsumerBatchRows 记一次 flush 写了几行。rows <= 0(纯坏消息的批)直接返回。
func ObserveKafkaConsumerBatchRows(consumer string, rows int) {
	if rows <= 0 {
		return
	}
	register()
	kafkaConsumerBatchRows.WithLabelValues(consumer).Observe(float64(rows))
}

// ObserveKafkaConsumerMessageBytes 记一条消息 value 的字节数。
func ObserveKafkaConsumerMessageBytes(consumer string, n int) {
	register()
	kafkaConsumerMessageBytes.WithLabelValues(consumer).Observe(float64(n))
}

// SetKafkaConsumerLag 写 lag 探针的结果:全部分区合计的积压条数、单分区最大积压条数、保留期已删掉的未消费条数。
func SetKafkaConsumerLag(consumer string, lag, maxPartitionLag, gap int64) {
	register()
	kafkaConsumerLagMessages.WithLabelValues(consumer).Set(float64(lag))
	kafkaConsumerLagMaxPartitionMessages.WithLabelValues(consumer).Set(float64(maxPartitionLag))
	kafkaConsumerRetentionGapMessages.WithLabelValues(consumer).Set(float64(gap))
}

// ObserveKafkaConsumerLagProbe 记一次 lag 探测的结局(KafkaResultOK / KafkaResultError)。
func ObserveKafkaConsumerLagProbe(consumer, result string) {
	register()
	kafkaConsumerLagProbeTotal.WithLabelValues(consumer, result).Inc()
}

// ObserveKafkaConsumerIsolation 记一次批量失败后逐条兜底的结局(KafkaIsolationRecovered / KafkaIsolationStopped)。
func ObserveKafkaConsumerIsolation(consumer, result string) {
	register()
	kafkaConsumerIsolationTotal.WithLabelValues(consumer, result).Inc()
}

// ObserveIdSegmentAllocate records one AllocateIdSegment outcome. biz_tag cardinality is
// bounded by the store's tag validation and the handful of real tags (player/guild/item).
func ObserveIdSegmentAllocate(bizTag, outcome string) {
	register()
	idSegmentAllocateTotal.WithLabelValues(bizTag, outcome).Inc()
}

// ObserveIdIssuerReserve records one id issuer block reservation attempt. result is a fixed set (idissuer.ReserveXxx).
func ObserveIdIssuerReserve(result string) {
	register()
	idIssuerReserveTotal.WithLabelValues(result).Inc()
}

// ObservePlayerNameOp 记一次玩家名字注册表操作:结果计数 + 耗时。
//
// op 是 "reserve" | "release" | "batch_get";result 取 Help 里那组有界取值。
// 两个 label 的取值集合由调用方(logic 的 playerNameOp* / playerNameResult* 常量)
// 封闭,本函数不做校验——它在每次操作的 defer 里跑,不是校验的地方。
//
// dur 由调用方从函数入口处的 time.Now() 算出,这样每一条出口(含提前 return 的
// 失败分支)都被计进直方图。**不要**把 player_id 加成 label(AGENTS.md §9:
// 高基数会把时间序列打爆),需要它的排障信息进日志。
func ObservePlayerNameOp(op, result string, dur time.Duration) {
	register()
	playerNameOpsTotal.WithLabelValues(op, result).Inc()
	playerNameOpSeconds.WithLabelValues(op).Observe(dur.Seconds())
}

// ObservePlayerNameCache 按 id 累计一次 BatchGet 的缓存命中情况。
// result 是 "hit" | "negative_hit" | "miss";n 是本批里落在该结果上的 id 个数
// (调用方先在循环里累加、退出循环后一次报,省掉逐 id 的 label 查找)。
// n <= 0 直接返回,让调用方三种结果都能无条件调用。
func ObservePlayerNameCache(result string, n int) {
	if n <= 0 {
		return
	}
	register()
	playerNameCacheTotal.WithLabelValues(result).Add(float64(n))
}

// ObserveAssetOpLedgerRead 记一次 GetPlayerAssetOpLedger 的结果。
// result 是 "found" | "absent" | "error",取值集合由调用方(logic 的 assetOpLedgerRead* 常量)封闭,
// 本函数不做校验。**不要**把 player_id 加成 label(AGENTS.md §9),排障信息进日志。
func ObserveAssetOpLedgerRead(result string) {
	register()
	assetOpLedgerReadTotal.WithLabelValues(result).Inc()
}

// ObservePlayerInboxLedgerRead 记一次 GetPlayerInboxLedger 的结果。
// result 是 "found" | "absent" | "error",取值集合由调用方(logic 的 inboxLedgerRead* 常量)封闭,
// 本函数不做校验。**不要**把 player_id 加成 label(AGENTS.md §9),排障信息进日志。
func ObservePlayerInboxLedgerRead(result string) {
	register()
	playerInboxLedgerReadTotal.WithLabelValues(result).Inc()
}

// ── Observe helpers ─────────────────────────────────────────────────

// ── HTTP /metrics endpoint ──────────────────────────────────────────

// Start exposes /metrics over HTTP. Empty addr disables the endpoint.
// Mirrors scene_manager/internal/metrics.Start so operators can wire the
// same Prometheus scrape config against either service.
func Start(addr string) {
	if addr == "" {
		logx.Info("[metrics] MetricsListenAddr empty; Prometheus /metrics endpoint disabled")
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
		logx.Infof("[metrics] Prometheus /metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("[metrics] HTTP server exited: %v", err)
		}
	}()
}

// ── 登录取归属 zone 与账号目录(开服线 X-3;L-14 / L-18 压测口径,docs/design/player-route-mapping-durability-proposal.md §12.2)──
//
// 每次登录(HTTP 与 gate 各一次)都要 ReadAccountDirectory(全局 TiDB 悲观事务)+ BatchGetPlayerHomeZone,EnterGame 与
// scene_manager 的每次 EnterScene 还要 LookupAccountRole / GetPlayerHomeZone。这三组直方图给出服务端视角的单次耗时与结局,
// 与 login 侧 go-zero 的 rpc_client_requests_duration_ms 对照,就能分清慢在网络 / 排队还是慢在 data_service 自己。
// 全部是封闭集合,PrimeLoginReads 在构造 gRPC server 时逐个预建(理由同 PrimeRoute:error 这类偶发结局的序列
// 不预先存在,`increase(...) > 0` 恰好漏掉第一次)。label 不带 player_id / account(AGENTS.md §9)。
// 单独一把 sync.Once 注册,不动上面那张 MustRegister 清单(只追加)。
// 取值名同时是 tools/scripts/stress_summarize.ps1 的契约($homeZoneReadResults / $accountDirectoryRpcs),
// metrics_test.go 读脚本逐项比对。

// home_zone_read_seconds{rpc,result} 的取值。
const (
	HomeZoneRPCHome  = "home"  // GetPlayerHomeZone(单查)
	HomeZoneRPCBatch = "batch" // BatchGetPlayerHomeZone(login 角色列表)

	// 按返回的 gRPC code 归类(server.homeZoneReadResult):OK → ok,NotFound(映射的权威缺席)→ not_found,
	// 其余(全局库故障、调用方取消或超时)→ error。批量查没有缺席结论,not_found 恒为 0。
	HomeZoneReadResultOK       = "ok"
	HomeZoneReadResultNotFound = "not_found"
	HomeZoneReadResultError    = "error"
)

// account_directory_rpc_seconds{rpc,result} 的取值。rpc 覆盖 internal/server/account_directory.go 里的全部账号目录 RPC
// (角色交易与入场登记在各自的文件里,不在这组)。
const (
	AccountDirectoryRPCRead   = "read"   // ReadAccountDirectory
	AccountDirectoryRPCCreate = "create" // CreateAccountRole
	AccountDirectoryRPCLookup = "lookup" // LookupAccountRole
	AccountDirectoryRPCRepair = "repair" // RepairAccountRole
	AccountDirectoryRPCRemove = "remove" // RemoveAccountRoles

	// 按返回的 gRPC code 归类(server.directoryRPCResult,与 directoryError / admitDirectory 的映射一一对应):
	//   ok        = OK
	//   not_found = NotFound(ErrMissing,目录里没有这名角色)
	//   rejected  = 明确拒绝:PermissionDenied(调用证明无效)、InvalidArgument(ErrInvalid)、
	//               ResourceExhausted(ErrFull)、FailedPrecondition(ErrConflict,归属或身份已变化)
	//   error     = 依赖故障:Unavailable(目录未就绪、SQL 失败)及其余一切(调用方取消 / 超时等)
	AccountDirectoryResultOK       = "ok"
	AccountDirectoryResultNotFound = "not_found"
	AccountDirectoryResultRejected = "rejected"
	AccountDirectoryResultError    = "error"
)

var (
	homeZoneRPCs        = []string{HomeZoneRPCHome, HomeZoneRPCBatch}
	homeZoneReadResults = []string{HomeZoneReadResultOK, HomeZoneReadResultNotFound, HomeZoneReadResultError}

	accountDirectoryRPCs = []string{
		AccountDirectoryRPCRead, AccountDirectoryRPCCreate, AccountDirectoryRPCLookup,
		AccountDirectoryRPCRepair, AccountDirectoryRPCRemove,
	}
	accountDirectoryResults = []string{
		AccountDirectoryResultOK, AccountDirectoryResultNotFound, AccountDirectoryResultRejected, AccountDirectoryResultError,
	}

	// loginReadLatencyBuckets:0.5ms 起步盖住同地域的 TiDB 点查,2.5s 封顶盖住 login / scene_manager 1.5s 级的调用方超时与 TiDB 锁等待。
	loginReadLatencyBuckets = []float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}

	homeZoneReadSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "home_zone_read_seconds",
		Help:      "GetPlayerHomeZone (rpc=home) / BatchGetPlayerHomeZone (rpc=batch) handler latency by result (ok|not_found|error).",
		Buckets:   loginReadLatencyBuckets,
	}, []string{"rpc", "result"})

	homeZoneBatchIDs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "home_zone_batch_ids",
		Help:      "Number of player ids per BatchGetPlayerHomeZone call.",
		Buckets:   []float64{1, 2, 4, 8, 16, 32, 64, 128},
	})

	accountDirectoryRPCSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "account_directory_rpc_seconds",
		Help:      "Account directory RPC handler latency by rpc (read|create|lookup|repair|remove) and result (ok|not_found|rejected|error).",
		Buckets:   loginReadLatencyBuckets,
	}, []string{"rpc", "result"})

	loginReadRegisterOnce sync.Once
)

func registerLoginReads() {
	loginReadRegisterOnce.Do(func() {
		prometheus.MustRegister(homeZoneReadSeconds, homeZoneBatchIDs, accountDirectoryRPCSeconds)
	})
}

// PrimeLoginReads 预建 home_zone_read_seconds(2 × 3 = 6 条,批量查的 not_found 也建,让两种 rpc 的列形状一致)与
// account_directory_rpc_seconds(5 × 4 = 20 条)的全部组合;home_zone_batch_ids 无 label,注册即导出。
// server.NewDataServiceServer 调用;可重复调用(只建子序列,不打点)。
func PrimeLoginReads() {
	registerLoginReads()
	for _, rpc := range homeZoneRPCs {
		for _, result := range homeZoneReadResults {
			homeZoneReadSeconds.WithLabelValues(rpc, result)
		}
	}
	for _, rpc := range accountDirectoryRPCs {
		for _, result := range accountDirectoryResults {
			accountDirectoryRPCSeconds.WithLabelValues(rpc, result)
		}
	}
}

// ObserveHomeZoneRead 记一次归属读 handler 的耗时与结局。rpc / result 取 HomeZone* 常量(封闭集合),本函数不校验。
func ObserveHomeZoneRead(rpc, result string, d time.Duration) {
	registerLoginReads()
	homeZoneReadSeconds.WithLabelValues(rpc, result).Observe(d.Seconds())
}

// ObserveHomeZoneBatchIDs 记一次 BatchGetPlayerHomeZone 的 id 个数(含 0 个,落在 le=1 桶)。
func ObserveHomeZoneBatchIDs(n int) {
	registerLoginReads()
	homeZoneBatchIDs.Observe(float64(n))
}

// ObserveAccountDirectoryRPC 记一次账号目录 RPC 的耗时与结局。rpc / result 取 AccountDirectory* 常量(封闭集合),本函数不校验。
func ObserveAccountDirectoryRPC(rpc, result string, d time.Duration) {
	registerLoginReads()
	accountDirectoryRPCSeconds.WithLabelValues(rpc, result).Observe(d.Seconds())
}
