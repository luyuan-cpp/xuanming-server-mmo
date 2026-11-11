package gatepick

import (
	"math"
	"testing"
)

// 到水位的人数恰是 Full 第一次成立的人数:再少一人就不算满。区级准入与挑 gate 对「满」的口径靠这条对齐。
func TestWatermarkCapacity_IsFirstFullCount(t *testing.T) {
	for _, limit := range []uint32{1, 2, 9, 10, 11, 15, 99, 100, 101, 1024, 20000, 131071, math.MaxUint32} {
		w := WatermarkCapacity(limit)
		if w == 0 || w > uint64(limit) {
			t.Fatalf("WatermarkCapacity(%d) = %d, want within [1, %d]", limit, w, limit)
		}
		if !Full(uint32(w), limit) {
			t.Errorf("Full(%d, %d) = false, want true: a gate holding its watermark count is full", w, limit)
		}
		if Full(uint32(w-1), limit) {
			t.Errorf("Full(%d, %d) = true, want false: one player below the watermark is not full", w-1, limit)
		}
	}
	if got := WatermarkCapacity(20000); got != 18000 {
		t.Errorf("WatermarkCapacity(20000) = %d, want 18000 (the default GateMaxConnections)", got)
	}
	if got := WatermarkCapacity(0); got != 0 {
		t.Errorf("WatermarkCapacity(0) = %d, want 0 (no declared limit)", got)
	}
}

func TestAdmissionCapacity(t *testing.T) {
	gate := func(limit uint32, stale, draining bool) State {
		return State{MaxConnections: limit, Stale: stale, Draining: draining}
	}
	cases := []struct {
		name        string
		states      []State
		wantCap     uint64
		wantBounded bool
	}{
		{"empty", nil, 0, true},
		// deploy/k8s/zones.json 的形状:每区 2 台、每台 20000,到水位一共 36000,装不下开服档 45000。
		{"two default gates", []State{gate(20000, false, false), gate(20000, false, false)}, 36000, true},
		{"a full gate still counts its watermark", []State{{PlayerCount: 20000, MaxConnections: 20000}, gate(100, false, false)}, 18090, true},
		{"draining and stale gates left out while a serving gate exists",
			[]State{gate(100, false, false), gate(1000, false, true), gate(1000, true, false)}, 90, true},
		{"undeclared limit on a serving gate: unbounded", []State{gate(100, false, false), gate(0, false, false)}, 0, false},
		{"undeclared limit on a left-out gate does not matter",
			[]State{gate(100, false, false), gate(0, false, true), gate(0, true, false)}, 90, true},
		// 同 Eligible 退到「连得上的」:新玩家会被分给排空中的 gate,容量照数它们,停写的仍不数。
		{"every reachable gate draining: count the draining ones",
			[]State{gate(100, false, true), gate(1000, false, true), gate(1000, true, false)}, 990, true},
		// 同 Eligible 退到全部。
		{"every gate stale: count all", []State{gate(100, true, false), gate(1000, true, true)}, 990, true},
		{"every gate stale, one undeclared: unbounded", []State{gate(100, true, false), gate(0, true, false)}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bounded := AdmissionCapacity(tc.states, func(s State) State { return s })
			if got != tc.wantCap || bounded != tc.wantBounded {
				t.Errorf("AdmissionCapacity = (%d, %v), want (%d, %v)", got, bounded, tc.wantCap, tc.wantBounded)
			}
		})
	}
}
