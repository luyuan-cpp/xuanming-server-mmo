package loginqueue

import (
	"fmt"
	"testing"
)

func candidateIDs(cands []GateCandidate) []uint32 {
	ids := make([]uint32, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.NodeID)
	}
	return ids
}

// 停写的、人数到上限 90% 的被剔除;差一人不到水位的、没声明上限的照常留下;顺序不变。
func TestEligibleGates_DropsStaleAndFull(t *testing.T) {
	cands := []GateCandidate{
		{NodeID: 1, PlayerCount: 10, MaxConnections: 100},
		{NodeID: 2, PlayerCount: 0, MaxConnections: 100, Stale: true}, // 人最少,但卡住了
		{NodeID: 3, PlayerCount: 90, MaxConnections: 100},             // 恰好到水位
		{NodeID: 4, PlayerCount: 89, MaxConnections: 100},             // 差一人
		{NodeID: 5, PlayerCount: 1_000_000},                           // 没声明上限(dev 不限)
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

// 全部不合格时回落全集(与没有这层过滤时相同),不把一次可能的误判放大成整区登不上。
func TestEligibleGates_AllIneligibleFallsBackToAll(t *testing.T) {
	cases := map[string][]GateCandidate{
		"all stale": {{NodeID: 1, Stale: true}, {NodeID: 2, Stale: true}},
		"all full":  {{NodeID: 1, PlayerCount: 100, MaxConnections: 100}, {NodeID: 2, PlayerCount: 95, MaxConnections: 100}},
		"mixed":     {{NodeID: 1, Stale: true}, {NodeID: 2, PlayerCount: 100, MaxConnections: 100}},
	}
	for name, cands := range cases {
		t.Run(name, func(t *testing.T) {
			if got := fmt.Sprint(candidateIDs(eligibleGates(cands))); got != "[1 2]" {
				t.Fatalf("eligible = %s, want the full set [1 2]", got)
			}
		})
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
