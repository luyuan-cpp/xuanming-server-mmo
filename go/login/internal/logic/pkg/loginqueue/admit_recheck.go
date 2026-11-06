// admit_recheck.go — 排队玩家取走放行时,复核放行时选定的 gate。
//
// dispatcher 放行时只挑 gate、不签票(R17 R1),放行槽在 Redis 里最多待 AdmitTTL。这段时间里选定的那台 gate 可能
// 卡住(停写)、被摘掉或标了排空(不在候选里了)、在滚动升级里换了 node_id,或者被别人填满。取走时照签它,
// 玩家拿着票连过去才失败;放行槽已经取走,只能从 /assign-gate 重新排队。
//
// 所以取走现签之前先用此刻的候选复核一次:选定的 gate 仍合格就签它;不合格就按快速通道的规则
// (eligibleGates + 随机二选一)在此刻的候选里重挑一台。重挑只换 gate,不动名额:在途占位按 zone 记,与签给哪台无关。
// 读不到此刻的候选(出错、为空)时照签选定的 gate,与没有复核时相同:这一步只能改善,不能让已放行的玩家多失败一次。
//
// 代价:每次取走多一次 CandidatesForZone(读 gate 目录镜像,镜像不新鲜时直读 etcd;外加一次排空标记 MGET)。
// 设计见 docs/design/gate-load-balancing-design.md「满载与停写的 gate」。
package loginqueue

import (
	"context"
	"fmt"
	mathrand "math/rand/v2"
	"time"

	"shared/gatepick"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// 复核结局,用作 login_queue_admit_gate_recheck_total 的 outcome 标签(封闭集合)。
const (
	admitRecheckKept      = "kept"      // 选定的 gate 仍合格,签它
	admitRecheckGone      = "gone"      // 选定的 gate 不在此刻的候选里(下线、换了 node_id、标了排空),重挑
	admitRecheckStale     = "stale"     // 选定的 gate 停写,重挑
	admitRecheckFull      = "full"      // 选定的 gate 人数到水位,重挑
	admitRecheckUnchecked = "unchecked" // 读不到此刻的候选(出错或为空),照签选定的 gate
)

var (
	admitGateRecheck = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "admit_gate_recheck_total",
		Help: "Consume-time recheck of the gate the dispatcher picked when it admitted a queue entry, by outcome: " +
			"'kept' = still eligible, token signed for it; 'gone' = no longer among the zone's candidates (left etcd, " +
			"replaced by a new node_id, or marked draining), re-picked; 'stale' / 'full' = it stopped rewriting its " +
			"etcd record or reached 90% of max_connections, re-picked; 'unchecked' = candidates unavailable (read " +
			"error or none), token signed for the originally picked gate as before this check existed.",
		Labels: []string{"outcome"},
	})

	admitRepickLog    logThrottle
	admitUncheckedLog logThrottle
)

// GateCandidateSource 是复核要用的那一项能力:此刻某个 zone 的 gate 候选(已按客户端地址去重、剔除排空)。
// CapacityProvider 满足它。
type GateCandidateSource interface {
	CandidatesForZone(ctx context.Context, zoneID uint32) ([]GateCandidate, error)
}

// NewAdmitSigner 返回排队玩家取走放行时用的 SignAdmitFn:先复核放行时选定的 gate,再按 ttl 现签。
// 两个取走入口(AssignGate 携排队令牌重入、QueryQueueStatus)都用它,签法只有这一份。
// ctx 是本次请求的 ctx,复核读候选时用。source 为 nil 时不复核,照签选定的 gate(计 unchecked)。
// 返回的函数并发安全(复核只读候选、记指标);签名失败才回错误,复核本身不回错误。
func NewAdmitSigner(ctx context.Context, source GateCandidateSource, secret []byte, ttl time.Duration) SignAdmitFn {
	return func(slot *AdmitSlot) (*AdmitToken, error) {
		if slot == nil {
			return nil, fmt.Errorf("nil admit slot")
		}
		return SignGateToken(recheckAdmitGate(ctx, source, slot), secret, ttl)
	}
}

// recheckAdmitGate 读此刻的候选并决定签哪台 gate;读不到候选时照签选定的那台。副作用只有指标与节流日志。
func recheckAdmitGate(ctx context.Context, source GateCandidateSource, slot *AdmitSlot) *GateCandidate {
	var (
		candidates []GateCandidate
		err        error
	)
	if source != nil {
		candidates, err = source.CandidatesForZone(ctx, slot.ZoneID)
	}
	if len(candidates) == 0 {
		admitGateRecheck.Inc(admitRecheckUnchecked)
		if admitUncheckedLog.allow(gatePickLogEvery) {
			logx.Errorf("[loginqueue] consume-time gate recheck skipped (zone=%d gate=%d): no candidates (err=%v); "+
				"signing for the gate picked at admit time; further reports are suppressed for %s, see "+
				"login_queue_admit_gate_recheck_total{outcome=\"unchecked\"}",
				slot.ZoneID, slot.NodeID, err, gatePickLogEvery)
		}
		return slotGate(slot)
	}
	gate, outcome := chooseAdmitGate(slot, candidates, mathrand.IntN)
	admitGateRecheck.Inc(outcome)
	if outcome != admitRecheckKept && admitRepickLog.allow(gatePickLogEvery) {
		logx.Infof("[loginqueue] gate %d picked at admit time no longer takes new players (%s, zone=%d); "+
			"signed for gate %d instead; further reports are suppressed for %s, see login_queue_admit_gate_recheck_total",
			slot.NodeID, outcome, slot.ZoneID, gate.NodeID, gatePickLogEvery)
	}
	return gate
}

// chooseAdmitGate 是复核的纯逻辑:candidates 是此刻本 zone 的候选(非空),intn 是随机源(生产 math/rand/v2,单测传固定序列)。
//   - 选定的 gate 按 node_id 认(gate 票绑的就是 node_id,gate 拿它判 token_gate_node_mismatch)。它仍在合格的候选里就签它,
//     地址取此刻候选里的那份(同一个 node_id 的客户端地址以最新的注册记录为准)。
//   - 否则按快速通道的规则在合格的候选里重挑,并说明原因:停写、满载,或不在候选里(gone)。
//   - 全部不合格时 eligibleGates 回落全集,选定的 gate 只要还在候选里就留着它,与放行时一样。
func chooseAdmitGate(slot *AdmitSlot, candidates []GateCandidate, intn func(int) int) (*GateCandidate, string) {
	eligible := eligibleGates(candidates)
	if c, ok := findCandidate(eligible, slot.NodeID); ok {
		return &c, admitRecheckKept
	}
	outcome := admitRecheckGone
	if c, ok := findCandidate(candidates, slot.NodeID); ok {
		switch gatepick.Verdict(candidateState(c)) {
		case gatepick.ReasonStale:
			outcome = admitRecheckStale
		case gatepick.ReasonFull:
			outcome = admitRecheckFull
		}
	}
	gate, err := pickTwoChoices(eligible, intn)
	if err != nil {
		// candidates 非空时 eligible 不会为空(回落全集),走不到这里;防的是以后改了调用方。
		return slotGate(slot), admitRecheckUnchecked
	}
	return gate, outcome
}

// findCandidate 按 node_id 找候选,返回副本。
func findCandidate(candidates []GateCandidate, nodeID uint32) (GateCandidate, bool) {
	for _, c := range candidates {
		if c.NodeID == nodeID {
			return c, true
		}
	}
	return GateCandidate{}, false
}

// slotGate 把放行槽里记的 gate 还原成签票要的候选(只有签票用得到的四项)。
func slotGate(slot *AdmitSlot) *GateCandidate {
	return &GateCandidate{NodeID: slot.NodeID, IP: slot.IP, Port: slot.Port, ZoneID: slot.ZoneID}
}
