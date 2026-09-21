package gameday

import (
	"testing"
	"time"
)

// at 构造 Zone 时区(UTC+8)的时刻。
func at(y int, m time.Month, d, hh, mm, ss int) time.Time {
	return time.Date(y, m, d, hh, mm, ss, 0, Zone)
}

func TestDayKey(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want uint32
	}{
		{"切点前一秒仍算前一天", at(2026, 9, 16, 4, 59, 59), 20260915},
		{"切点整算当天", at(2026, 9, 16, 5, 0, 0), 20260916},
		{"午夜到切点之间算前一天", at(2026, 9, 16, 0, 0, 0), 20260915},
		{"跨月", at(2026, 10, 1, 4, 0, 0), 20260930},
		{"跨年", at(2027, 1, 1, 4, 59, 59), 20261231},
		// 入参带什么时区都不影响结果:UTC 21:00 就是 UTC+8 次日 05:00。
		{"UTC 入参", time.Date(2026, 9, 15, 21, 0, 0, 0, time.UTC), 20260916},
		{"UTC 入参切点前一秒", time.Date(2026, 9, 15, 20, 59, 59, 0, time.UTC), 20260915},
		{"B6 口径:毫秒级切点前", at(2026, 9, 21, 4, 59, 59).Add(999 * time.Millisecond), 20260920},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DayKey(c.in); got != c.want {
				t.Fatalf("DayKey(%v) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestWeekKey(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want uint32
	}{
		// 2026-09-14 是周一。
		{"周一切点整进入新一周", at(2026, 9, 14, 5, 0, 0), 202638},
		{"周一切点前一秒仍属上一周", at(2026, 9, 14, 4, 59, 59), 202637},
		{"周日深夜属本周", at(2026, 9, 20, 23, 59, 59), 202638},
		// 2026 年有 53 个 ISO 周;2027-01-01(周五)属于 2026-W53,年份取 ISO 年而不是自然年。
		{"跨年周取 ISO 年", at(2027, 1, 1, 5, 0, 0), 202653},
		{"跨年周之后回到第 1 周", at(2027, 1, 4, 5, 0, 0), 202701},
		// 2024-12-30(周一)属于 2025-W01:ISO 年也可能比自然年大。
		{"ISO 年大于自然年", at(2024, 12, 30, 5, 0, 0), 202501},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WeekKey(c.in); got != c.want {
				t.Fatalf("WeekKey(%v) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestWeekKeyIsMonotonic 钉住清理任务依赖的性质:周键随时间不减。
// 跨年那一周最容易写坏(用自然年会得到 202753 > 202701)。
func TestWeekKeyIsMonotonic(t *testing.T) {
	start := at(2026, 12, 1, 5, 0, 0)
	prev := WeekKey(start)
	for day := 1; day <= 60; day++ {
		cur := WeekKey(start.AddDate(0, 0, day))
		if cur < prev {
			t.Fatalf("WeekKey 在 %v 从 %d 回退到 %d", start.AddDate(0, 0, day), prev, cur)
		}
		prev = cur
	}
}

func TestNextDailyReset(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"切点整 → 次日(严格晚于)", at(2026, 9, 16, 5, 0, 0), at(2026, 9, 17, 5, 0, 0)},
		{"切点前 → 当天", at(2026, 9, 16, 4, 0, 0), at(2026, 9, 16, 5, 0, 0)},
		{"切点后 → 次日", at(2026, 9, 16, 5, 0, 1), at(2026, 9, 17, 5, 0, 0)},
		{"月末溢出", at(2026, 9, 30, 12, 0, 0), at(2026, 10, 1, 5, 0, 0)},
		{"年末溢出", at(2026, 12, 31, 23, 0, 0), at(2027, 1, 1, 5, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NextDailyReset(c.in)
			if !got.Equal(c.want) {
				t.Fatalf("NextDailyReset(%v) = %v, want %v", c.in, got, c.want)
			}
			if !got.After(c.in) {
				t.Fatalf("NextDailyReset(%v) = %v,必须严格晚于入参", c.in, got)
			}
			// 返回的切点属于下一个游戏日:它的 DayKey 必须比入参的大。
			if DayKey(got) <= DayKey(c.in) {
				t.Fatalf("DayKey(next)=%d 应大于 DayKey(in)=%d", DayKey(got), DayKey(c.in))
			}
		})
	}
}

func TestNextWeeklyReset(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"周三 → 下周一", at(2026, 9, 16, 12, 0, 0), at(2026, 9, 21, 5, 0, 0)},
		{"周一切点整 → 下周一(严格晚于)", at(2026, 9, 14, 5, 0, 0), at(2026, 9, 21, 5, 0, 0)},
		{"周一切点前 → 当天", at(2026, 9, 14, 4, 59, 59), at(2026, 9, 14, 5, 0, 0)},
		{"周日 → 次日", at(2026, 9, 20, 23, 0, 0), at(2026, 9, 21, 5, 0, 0)},
		{"跨年", at(2026, 12, 30, 12, 0, 0), at(2027, 1, 4, 5, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NextWeeklyReset(c.in)
			if !got.Equal(c.want) {
				t.Fatalf("NextWeeklyReset(%v) = %v, want %v", c.in, got, c.want)
			}
			if !got.After(c.in) {
				t.Fatalf("NextWeeklyReset(%v) = %v,必须严格晚于入参", c.in, got)
			}
			if got.Weekday() != time.Monday {
				t.Fatalf("NextWeeklyReset(%v) 落在 %v,应为周一", c.in, got.Weekday())
			}
			if WeekKey(got) == WeekKey(c.in) {
				t.Fatalf("切周时刻 %v 的 WeekKey 仍是 %d,应已进入下一周", got, WeekKey(got))
			}
		})
	}
}

func TestPeriodKey(t *testing.T) {
	now := at(2026, 9, 16, 12, 0, 0)
	cases := []struct {
		name    string
		period  uint32
		wantKey uint32
		wantOK  bool
	}{
		{"不限:键为 0,不占计数行", PeriodNone, 0, true},
		{"每日", PeriodDaily, 20260916, true},
		{"每周", PeriodWeekly, 202638, true},
		{"未知周期拒绝", 3, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, ok := PeriodKey(c.period, now)
			if key != c.wantKey || ok != c.wantOK {
				t.Fatalf("PeriodKey(%d) = (%d, %v), want (%d, %v)", c.period, key, ok, c.wantKey, c.wantOK)
			}
		})
	}
}

// TestKeyRangesAreDisjoint 钉住清理任务的另一个前提:日键 8 位、周键 6 位,互不落入对方数值域。
func TestKeyRangesAreDisjoint(t *testing.T) {
	const minDayKey, maxWeekKey = 19700101, 999953
	for _, in := range []time.Time{at(1970, 1, 2, 5, 0, 0), at(2026, 9, 16, 12, 0, 0), at(9999, 12, 31, 12, 0, 0)} {
		if d := DayKey(in); d < minDayKey {
			t.Fatalf("DayKey(%v) = %d,低于日键下界 %d", in, d, minDayKey)
		}
		if w := WeekKey(in); w > maxWeekKey {
			t.Fatalf("WeekKey(%v) = %d,高于周键上界 %d", in, w, maxWeekKey)
		}
	}
}
