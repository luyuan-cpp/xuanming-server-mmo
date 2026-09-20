// Package metrics exposes Prometheus counters / histograms for data_service
// cross-server observability. The intent is to surface SavePlayerData /
// LoadPlayerData / per-player lock / rollback / cross-scene transition
// behavior so operators can tell at a glance whether the data plane is
// healthy.
//
// Style mirrors scene_manager/internal/metrics — Observe* helpers that lazy-
// register on first use. Keep label cardinality low: never label by player_id.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
)

const subsystem = "data_service"

var (
	// ── SavePlayerData / SetPlayerField — write path ──────────────
	savePlayerDataTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "save_player_data_total",
		Help:      "SavePlayerData outcomes (ok|version_mismatch|lock_conflict|redis_error).",
	}, []string{"outcome"})

	saveLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "save_latency_seconds",
		Help:      "End-to-end SavePlayerData latency including lock acquire + version check + Redis pipeline.",
		Buckets:   prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms .. ~4s
	}, []string{"outcome"})

	// ── Per-player lock — Layer 2 of consistency defense ──────────
	playerLockTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "player_lock_total",
		Help:      "Per-player lock outcomes (acquired|conflict|error).",
	}, []string{"outcome"})

	// ── Version mismatch — optimistic lock conflicts ──────────────
	versionMismatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "version_mismatch_total",
		Help:      "SavePlayerData / SetPlayerField optimistic-lock mismatches by op.",
	}, []string{"op"})

	// ── Cross-scene transition — Single Writer enforcement ────────
	// Reports the SceneManager-orchestrated old-Scene-release → new-Scene-load
	// sequence. data_service is the data plane that backs both steps; phase
	// labels record where time goes so we can spot Scene nodes that are
	// slow to flush vs slow to load.
	crossSceneTransitionLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "cross_scene_transition_latency_seconds",
		Help:      "Per-phase cross-scene transition latency (release|save|load|total).",
		Buckets:   prometheus.ExponentialBuckets(0.005, 2, 12), // 5ms .. ~20s
	}, []string{"phase"})

	crossSceneTransitionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "cross_scene_transition_total",
		Help:      "Cross-scene transition outcomes (ok|release_timeout|save_failed|load_failed|aborted).",
	}, []string{"outcome"})

	// ── Rollback — snapshot / zone / full-server ──────────────────
	rollbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "rollback_total",
		Help:      "Rollback attempts by scope (player|zone|server) and outcome (ok|partial|failed).",
	}, []string{"scope", "outcome"})

	// rollbackPlayersAffected accumulates the count of players touched so
	// operators can dashboard "how big was this rollback" without scraping logs.
	rollbackPlayersAffectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "rollback_players_affected_total",
		Help:      "Total players whose data was restored by a rollback, by scope.",
	}, []string{"scope"})

	rollbackOrphansCleanedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "rollback_orphans_cleaned_total",
		Help:      "Orphan characters (created after target_time) cleaned during zone/server rollback.",
	}, []string{"scope"})

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

	// ── AllocateIdSegment(号段发号)───────────────────────────────
	idSegmentAllocateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "id_segment_allocate_total",
		Help:      "AllocateIdSegment outcomes by biz_tag (ok|invalid|exhausted|unknown_tag|db_error).",
	}, []string{"biz_tag", "outcome"})

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

	registerOnce sync.Once
)

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			savePlayerDataTotal, saveLatencySeconds,
			playerLockTotal,
			versionMismatchTotal,
			crossSceneTransitionLatency, crossSceneTransitionTotal,
			rollbackTotal, rollbackPlayersAffectedTotal, rollbackOrphansCleanedTotal,
			kafkaConsumerUp, kafkaConsumerMessagesTotal,
			idSegmentAllocateTotal,
			playerNameOpsTotal, playerNameOpSeconds, playerNameCacheTotal,
		)
	})
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

// ObserveIdSegmentAllocate records one AllocateIdSegment outcome. biz_tag cardinality is
// bounded by the store's tag validation and the handful of real tags (player/guild/item).
func ObserveIdSegmentAllocate(bizTag, outcome string) {
	register()
	idSegmentAllocateTotal.WithLabelValues(bizTag, outcome).Inc()
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

// ── Observe helpers ─────────────────────────────────────────────────

// ObserveSavePlayerData records one SavePlayerData / SetPlayerField outcome
// along with end-to-end latency. outcome must be one of
//
//	"ok" | "version_mismatch" | "lock_conflict" | "redis_error"
//
// Pass startTime = time.Now() captured at the very top of the RPC handler
// so the histogram captures every code path including failure exits.
func ObserveSavePlayerData(outcome string, startTime time.Time) {
	register()
	savePlayerDataTotal.WithLabelValues(outcome).Inc()
	saveLatencySeconds.WithLabelValues(outcome).Observe(time.Since(startTime).Seconds())
}

// ObservePlayerLock records one player-lock attempt. outcome is
// "acquired" | "conflict" | "error".
func ObservePlayerLock(outcome string) {
	register()
	playerLockTotal.WithLabelValues(outcome).Inc()
}

// ObserveVersionMismatch records one optimistic-lock collision. op is
// "save_player_data" | "set_player_field".
func ObserveVersionMismatch(op string) {
	register()
	versionMismatchTotal.WithLabelValues(op).Inc()
}

// ObserveCrossSceneTransition records latency for one phase of the
// SceneManager-orchestrated transition. phase is "release" | "save" |
// "load" | "total". Use a separate call per phase so dashboards can
// stack them.
func ObserveCrossSceneTransition(phase string, dur time.Duration) {
	register()
	crossSceneTransitionLatency.WithLabelValues(phase).Observe(dur.Seconds())
}

// ObserveCrossSceneTransitionOutcome records the final outcome of one
// transition attempt. outcome is "ok" | "release_timeout" | "save_failed"
// | "load_failed" | "aborted".
func ObserveCrossSceneTransitionOutcome(outcome string) {
	register()
	crossSceneTransitionTotal.WithLabelValues(outcome).Inc()
}

// ObserveRollback records one rollback attempt. scope is "player" |
// "zone" | "server"; outcome is "ok" | "partial" | "failed".
// affectedPlayers is the count of players whose data was actually
// restored (0 on failure / orphan-only sweeps). orphansCleaned is the
// count of orphan characters scrubbed (0 for player-scope rollback).
func ObserveRollback(scope, outcome string, affectedPlayers, orphansCleaned uint32) {
	register()
	rollbackTotal.WithLabelValues(scope, outcome).Inc()
	if affectedPlayers > 0 {
		rollbackPlayersAffectedTotal.WithLabelValues(scope).Add(float64(affectedPlayers))
	}
	if orphansCleaned > 0 {
		rollbackOrphansCleanedTotal.WithLabelValues(scope).Add(float64(orphansCleaned))
	}
}

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

// CommonLatencyHelper — convenience: returns a closure that observes a
// histogram with the elapsed time since the helper was created. Saves
// callers from having to capture startTime manually around early returns.
//
// Example:
//
//	defer metrics.WithSaveLatency("ok")() // observes 'ok' on normal exit
//	... if err != nil { defer metrics.WithSaveLatency("redis_error")() }
//
// (Use ObserveSavePlayerData directly if you want explicit control.)
func WithSaveLatency(outcome string) func() {
	register()
	start := time.Now()
	return func() {
		savePlayerDataTotal.WithLabelValues(outcome).Inc()
		saveLatencySeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	}
}

// FormatZone is a tiny helper that callers can use when they need to
// expose a zone-labeled metric (none currently do — kept here so future
// observe helpers can stay consistent with scene_manager).
func FormatZone(zoneID uint32) string {
	return strconv.FormatUint(uint64(zoneID), 10)
}
