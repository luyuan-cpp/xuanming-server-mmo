package gatepick

import (
	"fmt"
	"testing"
)

type cand struct {
	id       uint32
	count    uint32
	max      uint32
	stale    bool
	draining bool
}

func candState(c cand) State {
	return State{PlayerCount: c.count, MaxConnections: c.max, Stale: c.stale, Draining: c.draining}
}

func candIDs(cs []cand) string {
	ids := make([]uint32, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.id)
	}
	return fmt.Sprint(ids)
}

// excludedCounts 按 stale、draining、full 的顺序列出 EachExcluded 报出的剔除台数,用来一次比对。
func excludedCounts(ex Exclusion) string {
	got := ""
	ex.EachExcluded(func(reason string, n int) { got += fmt.Sprintf("%s=%d ", reason, n) })
	return got
}

// 水位按整数算:上限 15000 时 13500 人到水位,13499 人不到;没声明上限不判;上限接近 uint32 也不溢出。
// (2026-11-05 从 login 的 TestGateFull_IntegerThreshold 移来,规则搬进本包。)
func TestFull_IntegerThreshold(t *testing.T) {
	cases := []struct {
		count, max uint32
		want       bool
	}{
		{13499, 15000, false},
		{13500, 15000, true},
		{0, 0, false},
		{4_294_967_295, 0, false},
		{3_865_470_565, 4_294_967_295, false}, // 差一点到 90%
		{3_865_470_566, 4_294_967_295, true},
	}
	for _, tc := range cases {
		if got := Full(tc.count, tc.max); got != tc.want {
			t.Errorf("Full(count=%d, max=%d) = %v, want %v", tc.count, tc.max, got, tc.want)
		}
	}
}

// 合格返回空串;几条同时成立时按停写、排空、满载的先后取;没声明上限的人再多也合格。
func TestVerdict_StaleThenDrainingThenFull(t *testing.T) {
	cases := []struct {
		s    State
		want string
	}{
		{State{PlayerCount: 10, MaxConnections: 100}, ""},
		{State{PlayerCount: 0, MaxConnections: 100, Stale: true}, ReasonStale},
		{State{PlayerCount: 0, MaxConnections: 100, Draining: true}, ReasonDraining},
		{State{PlayerCount: 95, MaxConnections: 100}, ReasonFull},
		{State{PlayerCount: 100, MaxConnections: 100, Stale: true}, ReasonStale},
		{State{Stale: true, Draining: true}, ReasonStale},
		{State{PlayerCount: 100, MaxConnections: 100, Draining: true}, ReasonDraining},
		{State{PlayerCount: 1_000_000}, ""},
	}
	for _, tc := range cases {
		if got := Verdict(tc.s); got != tc.want {
			t.Errorf("Verdict(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// 有完全合格的候选时只留它们:停写的、排空中的、到水位的都剔掉;差一人不到水位的、没声明上限的照常留下;
// 顺序不变;按原因计数,几样都占的只计排在前面的那一条。
func TestEligible_KeepsOnlyFullyEligible(t *testing.T) {
	items := []cand{
		{id: 1, count: 10, max: 100},
		{id: 2, count: 0, max: 100, stale: true},   // 人最少,但卡住了
		{id: 3, count: 90, max: 100},               // 恰好到水位
		{id: 4, count: 89, max: 100},               // 差一人
		{id: 5, count: 1_000_000},                  // 没声明上限(dev 不限)
		{id: 6, count: 100, max: 100, stale: true}, // 又停写又满载,只计停写
		{id: 7, count: 1, max: 100, draining: true},
	}
	got, ex := Eligible(items, candState)
	if ids := candIDs(got); ids != "[1 4 5]" {
		t.Fatalf("eligible = %s, want [1 4 5]", ids)
	}
	if ex != (Exclusion{Stale: 2, Draining: 1, Full: 1}) {
		t.Fatalf("exclusion = %+v, want {Stale:2 Draining:1 Full:1 Fallback:none}", ex)
	}
	if ex.FellBack() || ex.FallbackReason() != "" {
		t.Fatalf("FellBack = %v, FallbackReason = %q without a fallback", ex.FellBack(), ex.FallbackReason())
	}
	if got := excludedCounts(ex); got != "stale=2 draining=1 full=1 " {
		t.Fatalf("excluded = %q, want every ineligible gate counted", got)
	}
}

// 没有要剔除的:原样返回同一个切片,不复制;空输入返回空,由调用方报「没有候选」。
func TestEligible_NothingToDropReturnsInput(t *testing.T) {
	items := []cand{{id: 1, count: 5, max: 100}, {id: 2}}
	got, ex := Eligible(items, candState)
	if len(got) != len(items) || &got[0] != &items[0] {
		t.Fatalf("eligible = %+v, want the input slice itself", got)
	}
	if ex != (Exclusion{}) {
		t.Fatalf("exclusion = %+v, want zero", ex)
	}
	if got, ex := Eligible([]cand(nil), candState); len(got) != 0 || ex != (Exclusion{}) {
		t.Fatalf("Eligible(nil) = %+v, %+v; want empty and zero exclusion", got, ex)
	}
}

// 回归(2026-11-06):没排空的全部停写、只有排空中的健康时,挑排空中的那台,不再退到卡住的 gate 上。
// 分层回落时排空那层先剔掉 1 号,停写那层只能在卡住的 2 号里回落,票签给一台连不上的 gate。
func TestEligible_ReachableDrainingGateBeatsStale(t *testing.T) {
	items := []cand{
		{id: 1, count: 40, draining: true},
		{id: 2, count: 0, stale: true},
	}
	got, ex := Eligible(items, candState)
	if ids := candIDs(got); ids != "[1]" {
		t.Fatalf("eligible = %s, want the reachable draining gate [1]", ids)
	}
	if ex.Fallback != FallbackReachable || ex.FallbackReason() != ReasonDraining {
		t.Fatalf("exclusion = %+v reason %q, want reachable fallback with reason draining", ex, ex.FallbackReason())
	}
	if got := excludedCounts(ex); got != "stale=1 " {
		t.Fatalf("excluded = %q, want only the stale gate left out (the draining one was used)", got)
	}
}

// 没有完全合格的候选时逐档退:先退到连得上的(排空或到水位),全部停写才退到全部。总会给出非空的一档。
func TestEligible_FallbackTiers(t *testing.T) {
	cases := []struct {
		name     string
		items    []cand
		want     string
		fallback Fallback
		reason   string
		excluded string
	}{
		{"all stale", []cand{{id: 1, stale: true}, {id: 2, stale: true}}, "[1 2]", FallbackAll, ReasonStale, ""},
		{"all full", []cand{{id: 1, count: 100, max: 100}, {id: 2, count: 95, max: 100}}, "[1 2]", FallbackReachable, ReasonFull, ""},
		{"all draining", []cand{{id: 1, draining: true}, {id: 2, draining: true}}, "[1 2]", FallbackReachable, ReasonDraining, ""},
		{"draining and full", []cand{{id: 1, draining: true}, {id: 2, count: 100, max: 100}}, "[1 2]", FallbackReachable, ReasonMixed, ""},
		{"stale and full", []cand{{id: 1, stale: true}, {id: 2, count: 100, max: 100}}, "[2]", FallbackReachable, ReasonFull, "stale=1 "},
		{"stale, draining and full", []cand{{id: 1, stale: true}, {id: 2, draining: true}, {id: 3, count: 99, max: 100}},
			"[2 3]", FallbackReachable, ReasonMixed, "stale=1 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ex := Eligible(tc.items, candState)
			if ids := candIDs(got); ids != tc.want {
				t.Fatalf("eligible = %s, want %s", ids, tc.want)
			}
			if !ex.FellBack() || ex.Fallback != tc.fallback || ex.FallbackReason() != tc.reason {
				t.Fatalf("exclusion = %+v reason %q, want fallback %d with reason %q", ex, ex.FallbackReason(), tc.fallback, tc.reason)
			}
			if got := excludedCounts(ex); got != tc.excluded {
				t.Fatalf("excluded = %q, want %q (gates used by the fallback are not left out)", got, tc.excluded)
			}
		})
	}
}

// 退到连得上的、又没有停写的要剔:原样返回同一个切片,不复制。
func TestEligible_ReachableFallbackWithoutStaleReturnsInput(t *testing.T) {
	items := []cand{{id: 1, draining: true}, {id: 2, count: 100, max: 100}}
	got, _ := Eligible(items, candState)
	if len(got) != len(items) || &got[0] != &items[0] {
		t.Fatalf("eligible = %+v, want the input slice itself", got)
	}
}

// Excluded 只认三种不合格原因;mixed 只是回落原因,不是剔除原因。
func TestExclusion_ExcludedUnknownReasonIsZero(t *testing.T) {
	ex := Exclusion{Stale: 1, Draining: 2, Full: 3}
	for _, reason := range []string{"", ReasonMixed, "other"} {
		if n := ex.Excluded(reason); n != 0 {
			t.Errorf("Excluded(%q) = %d, want 0", reason, n)
		}
	}
}

// 停写阈值是三个上报周期:连续漏两次不算。
func TestStaleAfter_IsThreeReportPeriods(t *testing.T) {
	if StaleAfter != 3*ReportPeriod {
		t.Fatalf("StaleAfter = %s, want 3 x ReportPeriod (%s)", StaleAfter, 3*ReportPeriod)
	}
}
