package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 按池撮合的指标（docs/design/global-match-hierarchical-pools.md §5、§11 R0；撮合段见 §5.2.1 第 8 条）。
// label 只用模式名、人数档、范围层级（zone / group / global）、机房阶段（local / cross）、撮合段角色（pool / region / sub / cross）、
// battle 节点选点状态（五个值）与有限的结果枚举；模板号、机房号、子池号、玩家号、对局号、节点实例号一律不进 label。

// 成局提交结果（ObservePoolClaim 的 outcome）。
const (
	PoolClaimCommitted = "committed"   // 提交成功，已开始备战
	PoolClaimConflict  = "conflict"    // 提交点复核失败（票已变化、被别的撮合段或副本先提交、范围或跨机房等待未满足）
	PoolClaimNoNode    = "no_node"     // 目标机房此刻没有可选的 battle 节点
	PoolClaimNoSlot    = "no_slot"     // 本副本备战并发已满，留到下一轮
	PoolClaimIDError   = "id_error"    // battle_id 发号失败（发号器不可用）
	PoolClaimPartyGone = "party_ended" // 组里的整队已失效（成员变化或下线），整队已撤单
)

var (
	poolRoundSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem, Name: "pool_round_seconds",
		Help:    "按池撮合一轮（本副本负责的全部撮合段）的耗时。",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5},
	})
	poolOwned = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "pool_owned",
		Help: "本副本当前负责的撮合段数（按 match 实例注册表做最高随机权重哈希）。role：pool 整池段、region 机房段、sub 子池段、cross 跨机房段。",
	}, []string{"role"})
	poolPageUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "pool_page_units",
		Help: "本轮各本机房段（pool / region / sub）读到的可撮合单位数之和（单排票 + 整队，受页大小截断，近似排队深度）；跨机房段的页与机房段重叠，不计入。",
	}, []string{"mode", "team_size"})
	poolClaimTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "pool_claim_total",
		Help: "按池撮合的成局提交结果。level 为范围层级（zone / group / global），pass 为机房阶段（local / cross）。",
	}, []string{"mode", "level", "pass", "outcome"})
	poolSegmentClaimTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "pool_segment_claim_total",
		Help: "按撮合段角色计的成局提交结果（role 同 pool_owned，outcome 同 pool_claim_total）。cross 段的 conflict 偏高说明跨机房段与机房段常争同一批老票。",
	}, []string{"role", "outcome"})
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
	battleNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "battle_nodes",
		Help: "本副本看到的 battle 节点数，按选点状态分（battleplace.State*，R6）：open、unspecified 可选；closed（节点自报不接新房间）、cooldown（本副本熔断中）、invalid（注册不完整）不可选。每个撮合轮刷新。",
	}, []string{"state"})
)

func poolCollectors() []prometheus.Collector {
	return []prometheus.Collector{poolRoundSeconds, poolOwned, poolPageUnits, poolClaimTotal, poolSegmentClaimTotal, poolWaitSeconds, poolExpiredTotal, immediateClaimTotal, battleNodes}
}

func ObservePoolRound(elapsed time.Duration) { poolRoundSeconds.Observe(elapsed.Seconds()) }

// SetPoolOwned 写一种角色的撮合段数（match_pool_owned{role}）。调用方每轮对每种角色都写一次，没有的写 0。
func SetPoolOwned(role string, count int) { poolOwned.WithLabelValues(role).Set(float64(count)) }

// SetPoolPageUnits 写一个 (模式, 人数档) 本轮读到的可撮合单位数；调用方先在各段之间累加，本轮没有了的标签写 0。
func SetPoolPageUnits(mode, teamSize string, units int) {
	poolPageUnits.WithLabelValues(mode, teamSize).Set(float64(units))
}
func ObservePoolClaim(mode, level, pass, outcome string) {
	poolClaimTotal.WithLabelValues(mode, level, pass, outcome).Inc()
}

// ObservePoolSegmentClaim 按撮合段角色计一次成局提交结果（match_pool_segment_claim_total{role,outcome}）。
func ObservePoolSegmentClaim(role, outcome string) {
	poolSegmentClaimTotal.WithLabelValues(role, outcome).Inc()
}
func ObservePoolWait(mode, level string, seconds float64) {
	poolWaitSeconds.WithLabelValues(mode, level).Observe(seconds)
}
func ObservePoolExpired(mode string, count int) {
	if count > 0 {
		poolExpiredTotal.WithLabelValues(mode).Add(float64(count))
	}
}
func ObserveImmediateClaim(mode, outcome string) {
	immediateClaimTotal.WithLabelValues(mode, outcome).Inc()
}

// SetBattleNodes 写一种选点状态下的 battle 节点数（match_battle_nodes{state}）。state 只取 battleplace.State* 的五个值。
func SetBattleNodes(state string, count int) {
	battleNodes.WithLabelValues(state).Set(float64(count))
}
