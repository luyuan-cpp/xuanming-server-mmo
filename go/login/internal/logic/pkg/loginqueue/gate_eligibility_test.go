package loginqueue

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	gzprom "github.com/zeromicro/go-zero/core/prometheus"
)

func candidateIDs(cands []GateCandidate) []uint32 {
	ids := make([]uint32, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.NodeID)
	}
	return ids
}

// gatePickCounterValue 读挑 gate 计数 name{reason} 的当前值,序列还没出现时回 0。
// 计数器是进程级的,用例读前后差值;要先 gzprom.Enable() 计数器才写(同 admitRecheckValue)。
func gatePickCounterValue(t *testing.T, name, reason string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// 有完全合格的 gate 时:停写的、排空中的、人数到上限 90% 的都剔除;差一人不到水位的、没声明上限的照常留下;顺序不变。
func TestEligibleGates_KeepsOnlyFullyEligible(t *testing.T) {
	cands := []GateCandidate{
		{NodeID: 1, PlayerCount: 10, MaxConnections: 100},
		{NodeID: 2, PlayerCount: 0, MaxConnections: 100, Stale: true},    // 人最少,但卡住了
		{NodeID: 3, PlayerCount: 90, MaxConnections: 100},                // 恰好到水位
		{NodeID: 4, PlayerCount: 89, MaxConnections: 100},                // 差一人
		{NodeID: 5, PlayerCount: 1_000_000},                              // 没声明上限(dev 不限)
		{NodeID: 6, PlayerCount: 1, MaxConnections: 100, Draining: true}, // 人很少,但在排空
	}
	if got := fmt.Sprint(candidateIDs(eligibleGates(cands))); got != "[1 4 5]" {
		t.Fatalf("eligible = %s, want [1 4 5]", got)
	}
}

// 水位的整数边界随规则移到了 shared/gatepick(TestFull_IntegerThreshold);这里只测 GateCandidate 的适配。

// 没有要剔除的:原样返回同一个切片,不复制。
func TestEligibleGates_NothingToDropReturnsInput(t *testing.T) {
	cands := []GateCandidate{{NodeID: 1, PlayerCount: 5, MaxConnections: 100}, {NodeID: 2}}
	got := eligibleGates(cands)
	if len(got) != len(cands) || &got[0] != &cands[0] {
		t.Fatalf("eligible = %+v, want the input slice itself", got)
	}
	if got := eligibleGates(nil); len(got) != 0 {
		t.Fatalf("eligible(nil) = %+v, want empty (the caller reports no candidates)", got)
	}
}

// 没有完全合格的 gate 时逐档退:先退到连得上的(排空中或到水位),全部停写才退到全部。
// 总会给出非空的一档,不把一次可能的误判放大成整区登不上。
func TestEligibleGates_FallbackTiers(t *testing.T) {
	cases := []struct {
		name  string
		cands []GateCandidate
		want  string
	}{
		{"all stale", []GateCandidate{{NodeID: 1, Stale: true}, {NodeID: 2, Stale: true}}, "[1 2]"},
		{"all full", []GateCandidate{{NodeID: 1, PlayerCount: 100, MaxConnections: 100}, {NodeID: 2, PlayerCount: 95, MaxConnections: 100}}, "[1 2]"},
		{"all draining", []GateCandidate{{NodeID: 1, Draining: true}, {NodeID: 2, Draining: true}}, "[1 2]"},
		{"stale and full", []GateCandidate{{NodeID: 1, Stale: true}, {NodeID: 2, PlayerCount: 100, MaxConnections: 100}}, "[2]"},
		{"stale and draining", []GateCandidate{{NodeID: 1, Stale: true}, {NodeID: 2, Draining: true}}, "[2]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fmt.Sprint(candidateIDs(eligibleGates(tc.cands))); got != tc.want {
				t.Fatalf("eligible = %s, want %s", got, tc.want)
			}
		})
	}
}

// 回落按原因计 login_gate_pick_fallback_total:整区排空计 draining;被回落用上的 gate 不计 excluded。
func TestEligibleGates_CountsFallbackByTier(t *testing.T) {
	gzprom.Enable() // 计数器要指标开关打开才写,同 admit_recheck_test.go
	fellBack := gatePickCounterValue(t, "login_gate_pick_fallback_total", "draining")
	excluded := gatePickCounterValue(t, "login_gate_pick_excluded_total", "draining")

	eligibleGates([]GateCandidate{{NodeID: 1, Draining: true, ZoneID: 3}, {NodeID: 2, Draining: true, ZoneID: 3}})

	if got := gatePickCounterValue(t, "login_gate_pick_fallback_total", "draining") - fellBack; got != 1 {
		t.Fatalf("fallback{reason=draining} grew by %v, want 1", got)
	}
	if got := gatePickCounterValue(t, "login_gate_pick_excluded_total", "draining") - excluded; got != 0 {
		t.Fatalf("excluded{reason=draining} grew by %v, want 0 (the draining gates were used)", got)
	}
}

// 快速通道:只要还有合格的 gate,停写的(哪怕人最少)和满载的都不会被选中。
func TestPickGate_SkipsStaleAndFullGates(t *testing.T) {
	cands := []GateCandidate{
		{NodeID: 1, PlayerCount: 0, MaxConnections: 100, Stale: true},
		{NodeID: 2, PlayerCount: 99, MaxConnections: 100},
		{NodeID: 3, PlayerCount: 60, MaxConnections: 100},
		{NodeID: 4, PlayerCount: 70, MaxConnections: 100},
	}
	for range 200 {
		got, err := PickGate(cands)
		if err != nil {
			t.Fatal(err)
		}
		if got.NodeID == 1 || got.NodeID == 2 {
			t.Fatalf("PickGate picked gate %d, which is stale or full", got.NodeID)
		}
	}
}

// dispatcher 一拍:记账只在合格的 gate 之间进行,停写与满载的一个人都分不到。
func TestNewBatchPickFn_SkipsStaleAndFullGates(t *testing.T) {
	pick := newBatchPickFn([]GateCandidate{
		{NodeID: 1, PlayerCount: 0, Stale: true},
		{NodeID: 2, PlayerCount: 50, MaxConnections: 1000},
		{NodeID: 3, PlayerCount: 900, MaxConnections: 1000},
		{NodeID: 4, PlayerCount: 52, MaxConnections: 1000},
	})
	counts := map[uint32]int{}
	for range 10 {
		slot, err := pick(1, "q")
		if err != nil {
			t.Fatal(err)
		}
		counts[slot.NodeID]++
	}
	// 先把 2 号从 50 填到 52,再在 2、4 号之间轮转:2 号 6 人、4 号 4 人。
	if counts[1] != 0 || counts[3] != 0 || counts[2] != 6 || counts[4] != 4 {
		t.Fatalf("batch picks = %v, want gate 2 x6 and gate 4 x4, none for stale gate 1 or full gate 3", counts)
	}
}
