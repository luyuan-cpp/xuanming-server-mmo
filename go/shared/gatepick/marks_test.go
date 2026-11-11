package gatepick

import "testing"

// 键名是 login、scene_manager 与排空脚本之间的契约:改了任何一边都读不到对方写的标记,而且不报错。
func TestMarkKeys_Golden(t *testing.T) {
	if got := DrainingKey(7); got != "gate:7:draining" {
		t.Fatalf("DrainingKey(7) = %q, want gate:7:draining", got)
	}
	if got := StaleKey(7); got != "gate:7:stale" {
		t.Fatalf("StaleKey(7) = %q, want gate:7:stale", got)
	}
	if got := StaleKey(4_294_967_295); got != "gate:4294967295:stale" {
		t.Fatalf("StaleKey(max uint32) = %q", got)
	}
}

// 停写标记每 ReportPeriod 续写一次:过期时间要盖住至少两次漏写,否则一次网络抖动就让读者暂时看不见一台卡住的 gate。
func TestStaleMarkTTL_CoversMissedRefreshes(t *testing.T) {
	if StaleMarkTTL != StaleAfter {
		t.Fatalf("StaleMarkTTL = %s, want StaleAfter (%s)", StaleMarkTTL, StaleAfter)
	}
	if StaleMarkTTL <= 2*ReportPeriod {
		t.Fatalf("StaleMarkTTL = %s must exceed two refresh periods (%s)", StaleMarkTTL, 2*ReportPeriod)
	}
}
