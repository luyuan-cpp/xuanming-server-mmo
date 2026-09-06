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
	dto "github.com/prometheus/client_model/go"
	"github.com/zeromicro/go-zero/core/logx"
)

const subsystem = "match"

var (
	joinQueueTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "join_queue_total",
		Help:      "JoinQueue requests by mode and outcome (ok|in_battle|already_queued|mode_not_open|no_team_size|not_in_scene|internal).",
	}, []string{"mode", "outcome"})

	// gatherZoneMix 回答"跨 zone 对局占比":组内成员 zone 去重数 ==1 记 single,
	// 否则 cross(设计决策 D8;queue_depth 不加 zone 标签,队列本就不分 zone)。
	gatherZoneMix = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "gather_zone_mix_total",
		Help:      "Gather groups whose members all share one zone (single) vs span multiple zones (cross), by mode.",
	}, []string{"mode", "mix"})

	gatherTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "gather_total",
		Help:      "Gather pipeline runs by mode and outcome (success|no_battle_node|no_location|prepare_failed|fingerprint_mismatch|create_failed|internal).",
	}, []string{"mode", "outcome"})

	// tableFingerprintMismatch 记录 gather 收齐快照后配表指纹不一致(含部分为空)
	// 的次数;label mode 是当时生效的 TableFingerprintMode(warn|enforce),off 不比对
	// 不计数。任何非零值都意味着有 scene 节点跑着不同版本的战斗表(设计决策 D14)。
	tableFingerprintMismatch = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "table_fingerprint_mismatch_total",
		Help:      "Gather groups whose scene-side battle table fingerprints disagree (or are partially missing), by TableFingerprintMode (warn|enforce).",
	}, []string{"mode"})

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

	// requestBattleTicketTotal 丢票补签(MatchService.RequestBattleTicket → BattleNode.IssueBattleTicket)
	// 的出口分布:rejected = battle 核对名单后拒签,no_node = 观战索引指向的 battle 节点未发现。
	requestBattleTicketTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "request_battle_ticket_total",
		Help:      "RequestBattleTicket requests by outcome (ok|no_session|not_found|no_node|rpc_error|rejected|internal).",
	}, []string{"outcome"})

	// ---- 评分匹配(设计文档 §11)----

	// ratingUpdateTotal 对局结果回流(Kafka match-results)的入账结果:
	// applied 已更新 / duplicate 重复投递 / ignored 不计分(PVE、未结束等)/
	// partial 只落账了一部分玩家(标记留 applying,等重投续写)/
	// error Redis 出错(一人未写)/ decode_error 反序列化失败。
	ratingUpdateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "rating_update_total",
		Help:      "Battle result events consumed from Kafka by mode and outcome (applied|duplicate|ignored|partial|error|decode_error).",
	}, []string{"mode", "outcome"})

	// groupRatingSpread 每次凑组成功时组内最高分与最低分之差:分布右移说明
	// 容差放宽得太快或某段位人太少。
	groupRatingSpread = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "group_rating_spread",
		Help:      "Max minus min rating inside each formed group, by mode.",
		Buckets:   []float64{0, 25, 50, 100, 200, 300, 500, 800, 1000, 1600},
	}, []string{"mode"})

	// waitSeconds 凑组成功时锚点(队首)已等待的秒数:回答"评分匹配让人多等了多久"。
	waitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "wait_seconds",
		Help:      "Seconds the anchor (queue head) waited before its group formed, by mode.",
		Buckets:   []float64{1, 2, 5, 10, 20, 30, 45, 60, 120, 300},
	}, []string{"mode"})

	// starvedAnchorWait 本轮 popGroup 尝试过却凑不到候选的锚点里等得最久的秒数
	// (0 = 没有这样的锚点):wait_seconds 只在成组时记样本,饥饿中的锚点在
	// 那里不可见;这条 gauge 持续上升就是"容差 / 人数不够,有人在饿"。
	starvedAnchorWait = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "starved_anchor_wait_seconds",
		Help:      "Longest wait (seconds) among anchors the last matcher round tried but could not fill, per (mode, config); 0 when none.",
	}, []string{"mode", "config"})

	// ratingRoundCapDraw PVP 队列模式回合打满(total_rounds >= RatingDrawRoundCap)
	// 被改按平局结算的局数:C++ 引擎回合打满一律判 SIDE_B_WIN,match 侧纠正。
	ratingRoundCapDraw = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "rating_round_cap_draw_total",
		Help:      "Rated battles whose win/loss outcome was settled as a draw because total_rounds hit the round cap, by mode.",
	}, []string{"mode"})
)

var registerOnce sync.Once

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			joinQueueTotal,
			gatherTotal,
			gatherDuration,
			gatherZoneMix,
			tableFingerprintMismatch,
			challengeTotal,
			queueDepth,
			discoveredNodes,
			kafkaPushTotal,
			watchBattleTotal,
			requestBattleTicketTotal,
			ratingUpdateTotal,
			groupRatingSpread,
			waitSeconds,
			starvedAnchorWait,
			ratingRoundCapDraw,
		)
	})
}

// SetStarvedAnchorWait 更新某队列本轮凑不到候选的锚点的最长等待秒数(0 = 无)。
func SetStarvedAnchorWait(mode string, config string, seconds float64) {
	starvedAnchorWait.WithLabelValues(mode, config).Set(seconds)
}

// ObserveRatingRoundCapDraw 记录一局回合打满被改按平局结算。
func ObserveRatingRoundCapDraw(mode string) {
	ratingRoundCapDraw.WithLabelValues(mode).Inc()
}

// RatingRoundCapDrawValue 读回某 mode 的回合打满改平局计数(单测断言用)。
func RatingRoundCapDrawValue(mode string) float64 {
	var m dto.Metric
	if err := ratingRoundCapDraw.WithLabelValues(mode).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// ObserveRatingUpdate 记录一条对局结果的入账结果(mode = MatchMode 名)。
func ObserveRatingUpdate(mode string, outcome string) {
	ratingUpdateTotal.WithLabelValues(mode, outcome).Inc()
}

// RatingUpdateValue 读回某 (mode, outcome) 的入账计数(单测断言用)。
func RatingUpdateValue(mode string, outcome string) float64 {
	var m dto.Metric
	if err := ratingUpdateTotal.WithLabelValues(mode, outcome).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// ObserveGroupRatingSpread 记录一次凑组的组内最大分差。
func ObserveGroupRatingSpread(mode string, spread float64) {
	groupRatingSpread.WithLabelValues(mode).Observe(spread)
}

// ObserveMatchWait 记录一次凑组时锚点已等待的秒数。
func ObserveMatchWait(mode string, seconds float64) {
	waitSeconds.WithLabelValues(mode).Observe(seconds)
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

// ObserveGatherZoneMix 记录一组成员的 zone 组成(mix = single|cross)。
func ObserveGatherZoneMix(mode string, mix string) {
	gatherZoneMix.WithLabelValues(mode, mix).Inc()
}

// ObserveTableFingerprintMismatch 记录一次配表指纹不一致(mode = warn|enforce)。
func ObserveTableFingerprintMismatch(mode string) {
	tableFingerprintMismatch.WithLabelValues(mode).Inc()
}

// TableFingerprintMismatchValue 读回某 mode 的不一致计数(单测断言用;计数器
// 本身不导出,避免业务代码绕过 Observe* 直接改指标)。
func TableFingerprintMismatchValue(mode string) float64 {
	var m dto.Metric
	if err := tableFingerprintMismatch.WithLabelValues(mode).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
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

// ObserveRequestBattleTicket 记录一次 RequestBattleTicket(丢票补签)请求结果。
func ObserveRequestBattleTicket(outcome string) {
	requestBattleTicketTotal.WithLabelValues(outcome).Inc()
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
