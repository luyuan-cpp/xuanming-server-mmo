// gate_eligibility.go — 挑 gate 之前先剔除不该再接新玩家的候选:停写的(gate 多半卡住了)和满载的。
//
// 三处挑 gate 都先过 eligibleGates:快速通道 PickGate、dispatcher 一拍的 newBatchPickFn、排队领票时的复核
// (admit_recheck.go)。剔除与回落的规则在 shared/gatepick.Eligible(scene_manager 跨 zone 重定向共用同一份),
// 这里只做 GateCandidate 的适配、指标与日志。
// 它只影响「挑哪台」,不影响容量记账:FreeSlots 仍按全部候选的人数算 online,卡住或满载的 gate 上的人照样占着名额。
// 设计与取舍见 docs/design/gate-load-balancing-design.md「满载与停写的 gate」。
package loginqueue

import (
	"sync/atomic"
	"time"

	"shared/gatepick"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// 满载水位(人数到自报上限 GateCandidate.MaxConnections 的 90%)与停写阈值(三个上报周期)都在 shared/gatepick
// (FullPercent、StaleAfter)。login_gate_pick_excluded_total 与 login_gate_pick_fallback_total 的 reason 标签取
// gatepick.ReasonStale / ReasonFull / ReasonMixed(封闭集合)。

// gatePickLogEvery:下面两条日志各自的最小间隔。每次 AssignGate、dispatcher 每拍、每次排队领票都会走到这里,
// 不节流就是按登录 QPS 刷盘;持续多久看指标,日志只负责告诉运维是哪几台。
const gatePickLogEvery = 30 * time.Second

var (
	gatePickExcluded = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "login",
		Subsystem: "gate_pick",
		Name:      "excluded_total",
		Help: "Gate candidates left out of a pick, by reason: 'stale' = the gate has not rewritten its etcd record " +
			"for three report periods (likely hung while its lease lives on); 'full' = player_count at 90% of the " +
			"gate's self-reported max_connections. Counted once per pick per excluded gate (AssignGate fast path per " +
			"request, queue dispatcher per tick, queue consume-time recheck per redeemed admit).",
		Labels: []string{"reason"},
	})

	gatePickFallback = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "login",
		Subsystem: "gate_pick",
		Name:      "fallback_total",
		Help: "Picks where every candidate gate was stale or full, so the pick fell back to all candidates (the " +
			"behavior before this filter). reason: 'stale' = every gate of the zone stopped writing its record; " +
			"'full' = zone admission let in more players than the gates can hold (ZoneCapacity above the sum of " +
			"GateMaxConnections); 'mixed' = both. Any increase is worth an alert.",
		Labels: []string{"reason"},
	})

	staleGateLog    logThrottle
	gateFallbackLog logThrottle
)

// eligibleGates 返回可以接新玩家的候选,保持原顺序:去掉停写的(Stale)和人数到水位的(gatepick.Full)。
// 没有要去掉的就原样返回 candidates,不复制;调用方不得修改返回的切片。
//
// 全部不合格时回落全集并计 fallback,而不是拒绝(理由见 gatepick.Eligible):
//   - 全部停写:要么本 zone 的 gate 全卡住了,分过去照样失败;要么是本 login 的视图有问题,一律拒绝会把误判放大成整区登不上。
//   - 全部满载:拦人的地方是区级准入(FreeSlots),走到这里说明 ZoneCapacity 配得比 gate 上限之和还大;
//     gate 按 GateMaxConnections 拒连兜底。
//
// 候选为空时返回空,由调用方报「没有候选」。
func eligibleGates(candidates []GateCandidate) []GateCandidate {
	eligible, ex := gatepick.Eligible(candidates, candidateState)
	if ex.Stale > 0 {
		gatePickExcluded.Add(float64(ex.Stale), gatepick.ReasonStale)
		if staleGateLog.allow(gatePickLogEvery) {
			logx.Errorf("[GateSelect] gates left out of the pick because their etcd record has not been rewritten "+
				"for three report periods (likely hung while the lease lives on): node_ids=%v (zone=%d); "+
				"further reports are suppressed for %s, see login_gate_pick_excluded_total{reason=\"stale\"}",
				staleNodeIDs(candidates), candidates[0].ZoneID, gatePickLogEvery)
		}
	}
	if ex.Full > 0 {
		gatePickExcluded.Add(float64(ex.Full), gatepick.ReasonFull)
	}
	if ex.FellBack {
		gatePickFallback.Inc(ex.FallbackReason())
		if gateFallbackLog.allow(gatePickLogEvery) {
			logx.Errorf("[GateSelect] every gate candidate is stale or full (stale=%d full=%d of %d, zone=%d); "+
				"falling back to all candidates; further reports are suppressed for %s, see login_gate_pick_fallback_total",
				ex.Stale, ex.Full, len(candidates), candidates[0].ZoneID, gatePickLogEvery)
		}
	}
	return eligible
}

// candidateState 是 gatepick.Eligible / Verdict 判一台候选时看的那三项。
func candidateState(c GateCandidate) gatepick.State {
	return gatepick.State{PlayerCount: c.PlayerCount, MaxConnections: c.MaxConnections, Stale: c.Stale}
}

func staleNodeIDs(candidates []GateCandidate) []uint32 {
	ids := make([]uint32, 0, len(candidates))
	for _, c := range candidates {
		if c.Stale {
			ids = append(ids, c.NodeID)
		}
	}
	return ids
}

// logThrottle 让一条日志每个窗口至多打一次,并发安全;窗口按单调时钟算,墙钟回拨不会把日志压住。
type logThrottle struct {
	// last 是上一次放行时距 logThrottleBase 的纳秒数加 1;0 = 还没放行过。
	last atomic.Int64
}

var logThrottleBase = time.Now()

func (l *logThrottle) allow(every time.Duration) bool {
	now := int64(time.Since(logThrottleBase)) + 1
	last := l.last.Load()
	if last != 0 && now-last < int64(every) {
		return false
	}
	return l.last.CompareAndSwap(last, now)
}
