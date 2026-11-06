// gate_eligibility.go — 挑 gate 之前先给候选分档:完全合格的(没停写、没排空、没到水位)优先,
// 没有时退到连得上的(排空中或到水位,但没停写),全部停写才退到全部。
//
// 三处挑 gate 都先过 eligibleGates:快速通道 PickGate、dispatcher 一拍的 newBatchPickFn、排队领票时的复核
// (admit_recheck.go)。分档规则在 shared/gatepick.Eligible(scene_manager 跨 zone 重定向共用同一份),
// 这里只做 GateCandidate 的适配、指标与日志。
// 它只影响「挑哪台」,不影响容量记账:FreeSlots 仍按全部候选的人数算 online,卡住、排空中或满载的 gate 上的人照样占着名额。
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
// (FullPercent、StaleAfter)。login_gate_pick_excluded_total 的 reason 取 gatepick.ReasonStale / ReasonDraining /
// ReasonFull,login_gate_pick_fallback_total 的 reason 取 gatepick.Exclusion.FallbackReason()(stale / draining /
// full / mixed),都是封闭集合。

// gatePickLogEvery:下面两条日志各自的最小间隔。每次 AssignGate、dispatcher 每拍、每次排队领票都会走到这里,
// 不节流就是按登录 QPS 刷盘;持续多久看指标,日志只负责告诉运维是哪几台。
const gatePickLogEvery = 30 * time.Second

var (
	gatePickExcluded = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "login",
		Subsystem: "gate_pick",
		Name:      "excluded_total",
		Help: "Gate candidates left out of a pick, by reason: 'stale' = the gate has not rewritten its etcd record " +
			"for three report periods (likely hung while its lease lives on); 'draining' = marked draining for a " +
			"scale-in; 'full' = player_count at 90% of the gate's self-reported max_connections. Gates used by a " +
			"fallback tier are not counted. Counted once per pick per excluded gate (AssignGate fast path per " +
			"request, queue dispatcher per tick, queue consume-time recheck per redeemed admit).",
		Labels: []string{"reason"},
	})

	gatePickFallback = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "login",
		Subsystem: "gate_pick",
		Name:      "fallback_total",
		Help: "Picks where no candidate gate was fully eligible. reason: 'stale' = every gate of the zone stopped " +
			"writing its record, so the pick fell back to all of them (tickets likely land on hung gates); " +
			"'draining' / 'full' / 'mixed' = the pick used the reachable gates, all of which are marked draining " +
			"(whole zone marked, likely by mistake) / at 90% of max_connections (ZoneCapacity above the sum of " +
			"GateMaxConnections) / either. Any increase is worth an alert.",
		Labels: []string{"reason"},
	})

	staleGateLog    logThrottle
	gateFallbackLog logThrottle
)

// eligibleGates 给候选分档,返回最高的非空一档,保持原顺序:完全合格的(没停写、没排空、没到水位)优先;
// 没有时退到连得上的(排空中或到水位,但没停写);全部停写才退到全部。结果就是全部候选时原样返回 candidates,
// 不复制;调用方不得修改返回的切片。
//
// 总会给出非空的一档,退档时计 fallback,不拒绝(理由见 gatepick.Eligible):
//   - 全部停写:要么本 zone 的 gate 全卡住了,分过去照样失败;要么是本 login 的视图有问题,一律拒绝会把误判放大成整区登不上。
//   - 连得上的全在排空:整个 zone 都标了排空多半是误操作,拒绝所有登录比分到待缩容的 gate 更糟。
//   - 连得上的全到水位:拦人的地方是区级准入(FreeSlots),走到这里说明 ZoneCapacity 配得比 gate 上限之和还大;
//     gate 按 GateMaxConnections 拒连兜底。
//
// 候选为空时返回空,由调用方报「没有候选」。
func eligibleGates(candidates []GateCandidate) []GateCandidate {
	eligible, ex := gatepick.Eligible(candidates, candidateState)
	ex.EachExcluded(func(reason string, count int) {
		gatePickExcluded.Add(float64(count), reason)
	})
	if ex.Excluded(gatepick.ReasonStale) > 0 && staleGateLog.allow(gatePickLogEvery) {
		logx.Errorf("[GateSelect] gates left out of the pick because their etcd record has not been rewritten "+
			"for three report periods (likely hung while the lease lives on): node_ids=%v (zone=%d); "+
			"further reports are suppressed for %s, see login_gate_pick_excluded_total{reason=\"stale\"}",
			staleNodeIDs(candidates), candidates[0].ZoneID, gatePickLogEvery)
	}
	if ex.FellBack() {
		gatePickFallback.Inc(ex.FallbackReason())
		if gateFallbackLog.allow(gatePickLogEvery) {
			scope := "the reachable ones (draining or at 90% of max_connections)"
			if ex.Fallback == gatepick.FallbackAll {
				scope = "all of them (every gate is stale, tickets will likely land on hung gates)"
			}
			logx.Errorf("[GateSelect] no gate candidate is fully eligible (stale=%d draining=%d full=%d of %d, zone=%d); "+
				"picking among %s; further reports are suppressed for %s, see login_gate_pick_fallback_total{reason=%q}",
				ex.Stale, ex.Draining, ex.Full, len(candidates), candidates[0].ZoneID, scope, gatePickLogEvery,
				ex.FallbackReason())
		}
	}
	return eligible
}

// candidateState 是 gatepick.Eligible / Verdict 判一台候选时看的那四项。
func candidateState(c GateCandidate) gatepick.State {
	return gatepick.State{
		PlayerCount:    c.PlayerCount,
		MaxConnections: c.MaxConnections,
		Stale:          c.Stale,
		Draining:       c.Draining,
	}
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
