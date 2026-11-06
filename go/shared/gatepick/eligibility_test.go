package gatepick

import (
	"fmt"
	"testing"
)

type cand struct {
	id    uint32
	count uint32
	max   uint32
	stale bool
}

func candState(c cand) State {
	return State{PlayerCount: c.count, MaxConnections: c.max, Stale: c.stale}
}

func candIDs(cs []cand) string {
	ids := make([]uint32, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.id)
	}
	return fmt.Sprint(ids)
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

// 合格返回空串;既停写又满载的算停写;没声明上限的人再多也合格。
func TestVerdict_StaleTakesPrecedence(t *testing.T) {
	cases := []struct {
		s    State
		want string
	}{
		{State{PlayerCount: 10, MaxConnections: 100}, ""},
		{State{PlayerCount: 0, MaxConnections: 100, Stale: true}, ReasonStale},
		{State{PlayerCount: 95, MaxConnections: 100}, ReasonFull},
		{State{PlayerCount: 100, MaxConnections: 100, Stale: true}, ReasonStale},
		{State{PlayerCount: 1_000_000}, ""},
	}
	for _, tc := range cases {
		if got := Verdict(tc.s); got != tc.want {
			t.Errorf("Verdict(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// 停写的、到水位的被剔除;差一人不到水位的、没声明上限的照常留下;顺序不变;按原因计数,两样都占的只计停写。
func TestEligible_DropsStaleAndFull(t *testing.T) {
	items := []cand{
		{id: 1, count: 10, max: 100},
		{id: 2, count: 0, max: 100, stale: true}, // 人最少,但卡住了
		{id: 3, count: 90, max: 100},             // 恰好到水位
		{id: 4, count: 89, max: 100},             // 差一人
		{id: 5, count: 1_000_000},                // 没声明上限(dev 不限)
		{id: 6, count: 100, max: 100, stale: true},
	}
	got, ex := Eligible(items, candState)
	if ids := candIDs(got); ids != "[1 4 5]" {
		t.Fatalf("eligible = %s, want [1 4 5]", ids)
	}
	if ex != (Exclusion{Stale: 2, Full: 1}) {
		t.Fatalf("exclusion = %+v, want {Stale:2 Full:1 FellBack:false}", ex)
	}
	if r := ex.FallbackReason(); r != "" {
		t.Fatalf("FallbackReason = %q without a fallback, want empty", r)
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

// 全部不合格时回落全集(与没有这层过滤时相同),并说明是哪一类。
func TestEligible_AllIneligibleFallsBackToAll(t *testing.T) {
	cases := []struct {
		name   string
		items  []cand
		reason string
	}{
		{"all stale", []cand{{id: 1, stale: true}, {id: 2, stale: true}}, ReasonStale},
		{"all full", []cand{{id: 1, count: 100, max: 100}, {id: 2, count: 95, max: 100}}, ReasonFull},
		{"mixed", []cand{{id: 1, stale: true}, {id: 2, count: 100, max: 100}}, ReasonMixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ex := Eligible(tc.items, candState)
			if ids := candIDs(got); ids != "[1 2]" {
				t.Fatalf("eligible = %s, want the full set [1 2]", ids)
			}
			if !ex.FellBack || ex.FallbackReason() != tc.reason {
				t.Fatalf("exclusion = %+v reason %q, want a fallback with reason %q", ex, ex.FallbackReason(), tc.reason)
			}
		})
	}
}

// 停写阈值是三个上报周期:连续漏两次不算。
func TestStaleAfter_IsThreeReportPeriods(t *testing.T) {
	if StaleAfter != 3*ReportPeriod {
		t.Fatalf("StaleAfter = %s, want 3 x ReportPeriod (%s)", StaleAfter, 3*ReportPeriod)
	}
}
