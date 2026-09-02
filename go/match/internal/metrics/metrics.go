// Package metrics 暴露 match 服务的 Prometheus 指标:排队入口、matcher 凑单、
// gather 开局管线与挑战(切磋)链路的低基数计数,以及节点发现缓存的规模。
// 端口约定见 CLAUDE.md §6:9101=login / 9150=scene_manager / 9160=db / 9170=match。
// 注意:player_id / battle_id 一律只进日志,不进指标 label(高基数会爆)。
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
)

const subsystem = "match"

var (
	joinQueueTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "join_queue_total",
		Help:      "JoinQueue requests by mode and outcome (ok|in_battle|already_queued|mode_not_open|no_team_size|internal).",
	}, []string{"mode", "outcome"})

	gatherTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "gather_total",
		Help:      "Gather pipeline runs by mode and outcome (success|no_battle_node|no_location|prepare_failed|create_failed|internal).",
	}, []string{"mode", "outcome"})

	gatherDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "gather_duration_seconds",
		Help:      "End-to-end gather latency (battle_id mint -> CreateBattle ack).",
		Buckets:   []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"mode"})

	challengeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "challenge_total",
		Help:      "Challenge flow events by stage (invite|respond) and outcome.",
	}, []string{"stage", "outcome"})

	queueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "queue_depth",
		Help:      "Current Redis queue length per (mode, battle_config_id). Config ids are bounded table ids, not player ids.",
	}, []string{"mode", "config"})

	discoveredNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "discovered_nodes",
		Help:      "Nodes currently visible in the etcd watch cache, by kind (scene|battle).",
	}, []string{"kind"})

	kafkaPushTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "kafka_push_total",
		Help:      "Challenge S2C pushes via gate Kafka by outcome (ok|error).",
	}, []string{"outcome"})

	watchBattleTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "watch_battle_total",
		Help:      "WatchBattle requests by outcome (ok|queued|in_battle|already_watching|offline|no_battle|not_found|rejected|internal).",
	}, []string{"outcome"})
)

var registerOnce sync.Once

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			joinQueueTotal,
			gatherTotal,
			gatherDuration,
			challengeTotal,
			queueDepth,
			discoveredNodes,
			kafkaPushTotal,
			watchBattleTotal,
		)
	})
}

// ObserveJoinQueue 记录一次 JoinQueue 请求结果。
func ObserveJoinQueue(mode string, outcome string) {
	joinQueueTotal.WithLabelValues(mode, outcome).Inc()
}

// ObserveGather 记录一次 gather 管线的终态与耗时。
func ObserveGather(mode string, outcome string, elapsed time.Duration) {
	gatherTotal.WithLabelValues(mode, outcome).Inc()
	gatherDuration.WithLabelValues(mode).Observe(elapsed.Seconds())
}

// ObserveChallenge 记录挑战链路某一阶段的结果。
func ObserveChallenge(stage string, outcome string) {
	challengeTotal.WithLabelValues(stage, outcome).Inc()
}

// SetQueueDepth 更新某个队列的当前长度(matcher loop 扫描时刷新)。
func SetQueueDepth(mode string, config string, depth int) {
	queueDepth.WithLabelValues(mode, config).Set(float64(depth))
}

// SetDiscoveredNodes 更新节点发现缓存规模。
func SetDiscoveredNodes(kind string, count int) {
	discoveredNodes.WithLabelValues(kind).Set(float64(count))
}

// ObserveKafkaPush 记录一次 gate Kafka 推送结果。
func ObserveKafkaPush(outcome string) {
	kafkaPushTotal.WithLabelValues(outcome).Inc()
}

// ObserveWatchBattle 记录一次 WatchBattle 请求结果。
func ObserveWatchBattle(outcome string) {
	watchBattleTotal.WithLabelValues(outcome).Inc()
}

// Start 启动 Prometheus /metrics 端点;addr 为空则关闭(与 scene_manager 同模式)。
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
