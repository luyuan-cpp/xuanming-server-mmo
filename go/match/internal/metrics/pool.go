package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 按池撮合的指标（docs/design/global-match-hierarchical-pools.md §5、§11 R0）。
// label 只用模式名、人数档、范围层级（zone / group / global）、机房阶段（local / cross）与有限的结果枚举；
// 模板号、玩家号、对局号一律不进 label。

// 成局提交结果（ObservePoolClaim 的 outcome）。
const (
	PoolClaimCommitted = "committed"   // 提交成功，已开始备战
	PoolClaimConflict  = "conflict"    // 提交点复核失败（票已变化、被别的副本先提交、范围或跨机房等待未满足）
	PoolClaimNoNode    = "no_node"     // 目标机房此刻没有可选的 battle 节点
	PoolClaimNoSlot    = "no_slot"     // 本副本备战并发已满，留到下一轮
	PoolClaimIDError   = "id_error"    // battle_id 发号失败（发号器不可用）
	PoolClaimPartyGone = "party_ended" // 组里的整队已失效（成员变化或下线），整队已撤单
)

var (
	poolRoundSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem, Name: "pool_round_seconds",
		Help:    "按池撮合一轮（本副本负责的全部池）的耗时。",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5},
	})
	poolOwned = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "pool_owned",
		Help: "本副本当前负责撮合的池数（按 match 实例注册表做最高随机权重哈希）。",
	})
	poolPageUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "pool_page_units",
		Help: "本轮读到的可撮合单位数（单排票 + 整队，受页大小截断，近似排队深度）。",
	}, []string{"mode", "team_size"})
	poolClaimTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "pool_claim_total",
		Help: "按池撮合的成局提交结果。level 为范围层级（zone / group / global），pass 为机房阶段（local / cross）。",
	}, []string{"mode", "level", "pass", "outcome"})
	poolWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem, Name: "pool_wait_seconds",
		Help:    "成局提交成功时组内最长等待（从原始入队时刻算）。",
		Buckets: []float64{1, 5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300},
	}, []string{"mode", "level"})
	poolExpiredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "pool_expired_total",
		Help: "按池清扫终止的过期单排票（探测有效期内没有续命）。",
	}, []string{"mode"})
	immediateClaimTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "immediate_claim_total",
		Help: "入队后立即成局（单人档）的结果，失败的票留给池写者兜底。outcome 同 pool_claim_total。",
	}, []string{"mode", "outcome"})
)

func poolCollectors() []prometheus.Collector {
	return []prometheus.Collector{poolRoundSeconds, poolOwned, poolPageUnits, poolClaimTotal, poolWaitSeconds, poolExpiredTotal, immediateClaimTotal}
}

func ObservePoolRound(elapsed time.Duration) { poolRoundSeconds.Observe(elapsed.Seconds()) }
func SetPoolOwned(count int)                 { poolOwned.Set(float64(count)) }
func SetPoolPageUnits(mode, teamSize string, units int) {
	poolPageUnits.WithLabelValues(mode, teamSize).Set(float64(units))
}
func ObservePoolClaim(mode, level, pass, outcome string) {
	poolClaimTotal.WithLabelValues(mode, level, pass, outcome).Inc()
}
func ObservePoolWait(mode, level string, seconds float64) {
	poolWaitSeconds.WithLabelValues(mode, level).Observe(seconds)
}
func ObservePoolExpired(mode string, count int) {
	if count > 0 {
		poolExpiredTotal.WithLabelValues(mode).Add(float64(count))
	}
}
func ObserveImmediateClaim(mode, outcome string) { immediateClaimTotal.WithLabelValues(mode, outcome).Inc() }
