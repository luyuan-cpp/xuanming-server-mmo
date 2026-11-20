package data

import (
	"errors"
	"testing"
)

// 额度主体键由 pi 派生,库里只存它:派生一变,已有的额度全部对不上。期望值由与本实现无关的 Python(hashlib)离线算出。
func TestSpenderKeyIsStable(t *testing.T) {
	hi, lo := SpenderKey("1hpfml09b57f3f8185f8cb5094ea3f26278efb")
	if hi != 3136422662644718812 || lo != 13892314174709868598 {
		t.Fatalf("SpenderKey = %d %d", hi, lo)
	}
}

// 额度怎么算:在途的不分月份全额占着,已付的按 paid_ms 计入当月与当日(记在「未来」的也算),单日上限 0 = 不设。
func TestSpendLimitAdmit(t *testing.T) {
	const (
		monthStart = 1_000_000
		dayStart   = 5_000_000
	)
	limit := SpendLimit{MonthlyFen: 20000, DailyFen: 8000, MonthStartMs: monthStart, DayStartMs: dayStart}
	cases := []struct {
		name   string
		limit  SpendLimit
		held   []heldSpend
		amount uint64
		want   error
	}{
		{"empty month takes the whole monthly limit", SpendLimit{MonthlyFen: 20000, MonthStartMs: monthStart, DayStartMs: dayStart}, nil, 20000, nil},
		{"one fen over the monthly limit", SpendLimit{MonthlyFen: 20000, MonthStartMs: monthStart, DayStartMs: dayStart}, nil, 20001, ErrMonthlyLimit},
		{"in-flight intents count in full, whatever month they were made in", limit,
			[]heldSpend{{state: IntentCreated, amount: 5000}, {state: IntentCreated, amount: 5000}}, 3000, ErrDailyLimit},
		{"paid before this month does not count", limit,
			[]heldSpend{{state: IntentPaid, amount: 15000, paidMs: monthStart - 1}}, 8000, nil},
		{"paid earlier this month counts for the month but not for today", limit,
			[]heldSpend{{state: IntentPaid, amount: 15000, paidMs: monthStart}}, 5000, nil},
		{"paid earlier this month leaves less than the order", limit,
			[]heldSpend{{state: IntentPaid, amount: 15000, paidMs: monthStart}}, 5001, ErrMonthlyLimit},
		{"paid today counts for today", limit,
			[]heldSpend{{state: IntentPaid, amount: 5000, paidMs: dayStart}}, 3001, ErrDailyLimit},
		{"paid in the future under clock skew still counts", limit,
			[]heldSpend{{state: IntentPaid, amount: 18000, paidMs: dayStart * 10}}, 2001, ErrMonthlyLimit},
		{"closed intents hold nothing", limit,
			[]heldSpend{{state: IntentClosed, amount: 20000, paidMs: dayStart}}, 8000, nil},
		{"no daily limit configured", SpendLimit{MonthlyFen: 20000, MonthStartMs: monthStart, DayStartMs: dayStart},
			[]heldSpend{{state: IntentPaid, amount: 10000, paidMs: dayStart}}, 10000, nil},
		{"the monthly limit is reported before the daily one", limit,
			[]heldSpend{{state: IntentPaid, amount: 20000, paidMs: dayStart}}, 1, ErrMonthlyLimit},
	}
	for _, c := range cases {
		if got := c.limit.admit(c.held, c.amount); !errors.Is(got, c.want) {
			t.Errorf("%s: admit = %v, want %v", c.name, got, c.want)
		}
	}
}
