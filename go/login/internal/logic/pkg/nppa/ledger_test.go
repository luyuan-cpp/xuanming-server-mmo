package nppa

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const (
	testLease = 30 * time.Minute
	connA     = "gate-uuid:1"
	connB     = "gate-uuid:2"
	testPI    = "1hpfml09b57f3f8185f8cb5094ea3f26278efb"
)

var ledgerStart = time.Unix(1_763_600_000, 0)

func newTestLedger(t *testing.T) (*Ledger, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(ledgerStart)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewLedger(rdb, testLease), mr
}

// claimAll 领走现在到期的全部事件并删掉,返回解出来的行为。
func claimAll(t *testing.T, l *Ledger) []Behavior {
	t.Helper()
	claimed, err := l.Claim(context.Background(), MaxBatch)
	if err != nil {
		t.Fatal(err)
	}
	var out []Behavior
	var members []string
	for _, c := range claimed {
		if !c.Valid {
			t.Fatalf("unreadable event %q", c.Member)
		}
		out = append(out, c.Behavior)
		members = append(members, c.Member)
	}
	if err := l.Ack(context.Background(), members); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustSweep(t *testing.T, l *Ledger) int {
	t.Helper()
	n, err := l.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// runTo 把 Redis 的时钟拨到 at,途中每 5 分钟清扫一次,返回一共收尾了几条。线上每秒清扫一次;这里只要间隔不到
// 「清扫停过」的门槛(租约的三分之一)。要模拟所有 login 副本都停了,直接 mr.SetTime 跳过去。
func runTo(t *testing.T, l *Ledger, mr *miniredis.Miniredis, from, at time.Time) int {
	t.Helper()
	swept := 0
	for now := from; now.Before(at); {
		now = now.Add(5 * time.Minute)
		if now.After(at) {
			now = at
		}
		mr.SetTime(now)
		swept += mustSweep(t, l)
	}
	return swept
}

func TestLedgerBeginThenEndReportsLoginThenLogout(t *testing.T) {
	l, mr := newTestLedger(t)
	ctx := context.Background()
	if err := l.Begin(ctx, connA, "si-1", testPI, time.Time{}); err != nil {
		t.Fatal(err)
	}
	mr.SetTime(ledgerStart.Add(90 * time.Second))
	if err := l.End(ctx, connA); err != nil {
		t.Fatal(err)
	}
	got := claimAll(t, l)
	want := []Behavior{
		{SI: "si-1", BT: BehaviorLogin, OT: ledgerStart.Unix(), PI: testPI},
		{SI: "si-1", BT: BehaviorLogout, OT: ledgerStart.Unix() + 90, PI: testPI},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
}

// 同一秒里上线又下线:上线排在前面。
func TestLedgerOrdersLoginBeforeLogoutInTheSameInstant(t *testing.T) {
	l, _ := newTestLedger(t)
	ctx := context.Background()
	_ = l.Begin(ctx, connA, "si-1", testPI, time.Time{})
	_ = l.End(ctx, connA)
	got := claimAll(t, l)
	if len(got) != 2 || got[0].BT != BehaviorLogin || got[1].BT != BehaviorLogout {
		t.Fatalf("events = %+v, want login then logout", got)
	}
}

// 断线通知会被 gate 重放,收尾也可能对着没登记过的连接:都不能多报。
func TestLedgerEndIsIdempotent(t *testing.T) {
	l, _ := newTestLedger(t)
	ctx := context.Background()
	if err := l.End(ctx, connB); err != nil {
		t.Fatal(err)
	}
	_ = l.Begin(ctx, connA, "si-1", testPI, time.Time{})
	for i := 0; i < 3; i++ {
		if err := l.End(ctx, connA); err != nil {
			t.Fatal(err)
		}
	}
	if got := claimAll(t, l); len(got) != 2 {
		t.Fatalf("events = %+v, want exactly one login and one logout", got)
	}
}

// 同一条连接没收尾就又登录了一次:上一次的会话先下线,不留只有上线的会话。
func TestLedgerRebindClosesThePreviousSession(t *testing.T) {
	l, _ := newTestLedger(t)
	ctx := context.Background()
	_ = l.Begin(ctx, connA, "si-1", testPI, time.Time{})
	_ = l.Begin(ctx, connA, "si-2", testPI, time.Time{})
	_ = l.End(ctx, connA)
	logins, logouts := map[string]int{}, map[string]int{}
	for _, b := range claimAll(t, l) {
		if b.BT == BehaviorLogin {
			logins[b.SI]++
		} else {
			logouts[b.SI]++
		}
	}
	for _, si := range []string{"si-1", "si-2"} {
		if logins[si] != 1 || logouts[si] != 1 {
			t.Fatalf("session %s: logins=%d logouts=%d, want 1 and 1", si, logins[si], logouts[si])
		}
	}
}

// gate 进程没了,断线通知永远不来:租约到期后由清扫补下线,续过租的不动。
func TestLedgerSweepFillsInLogoutWhenTheLeaseRunsOut(t *testing.T) {
	l, mr := newTestLedger(t)
	ctx := context.Background()
	_ = l.Begin(ctx, connA, "si-dead", testPI, time.Time{})
	_ = l.Begin(ctx, connB, "si-alive", testPI, time.Time{})
	claimAll(t, l) // 两条上线
	if n := mustSweep(t, l); n != 0 {
		t.Fatalf("nothing is due yet, swept %d", n)
	}

	renewAt := ledgerStart.Add(20 * time.Minute)
	if n := runTo(t, l, mr, ledgerStart, renewAt); n != 0 {
		t.Fatalf("swept %d before any lease ran out", n)
	}
	if _, err := l.Renew(ctx, connB); err != nil {
		t.Fatal(err)
	}
	if n := runTo(t, l, mr, renewAt, ledgerStart.Add(testLease+time.Second)); n != 1 {
		t.Fatalf("swept %d sessions, want only the one that never renewed", n)
	}
	got := claimAll(t, l)
	if len(got) != 1 || got[0].SI != "si-dead" || got[0].BT != BehaviorLogout || got[0].OT != ledgerStart.Add(testLease).Unix() {
		t.Fatalf("events = %+v, want one logout for si-dead at the lease end", got)
	}
	// 迟到的断线通知不再多报一条。
	_ = l.End(ctx, connA)
	if got := claimAll(t, l); len(got) != 0 {
		t.Fatalf("late End after a sweep reported again: %+v", got)
	}
}

// 未成年人的租约不超过当天的截止时刻:连接没了而断线通知又丢了,下线也按截止时刻补,不会报成玩到了 21 点以后。
func TestLedgerCapsAMinorsLeaseAtThePlayDeadline(t *testing.T) {
	l, mr := newTestLedger(t)
	ctx := context.Background()
	deadline := ledgerStart.Add(10 * time.Minute)
	_ = l.Begin(ctx, connA, "si-minor", testPI, deadline)
	claimAll(t, l)
	renewAt := ledgerStart.Add(5 * time.Minute)
	runTo(t, l, mr, ledgerStart, renewAt)
	if got, err := l.Renew(ctx, connA); err != nil || !got.Equal(deadline) { // 续租回截止时刻,租约也不能越过它
		t.Fatalf("Renew = %v, %v; want the play deadline %v", got, err, deadline)
	}
	if n := runTo(t, l, mr, renewAt, deadline.Add(2*time.Second)); n != 1 {
		t.Fatalf("swept %d, want the minor's session ended at the deadline", n)
	}
	got := claimAll(t, l)
	if len(got) != 1 || got[0].BT != BehaviorLogout || got[0].OT != deadline.Unix() {
		t.Fatalf("events = %+v, want a logout stamped with the deadline", got)
	}
}

// 补报的下线最多往回填 120 秒:再早的时刻国家系统不收。
func TestLedgerSweepBackfillsAtMostTwoMinutes(t *testing.T) {
	l, mr := newTestLedger(t)
	ctx := context.Background()
	_ = l.Begin(ctx, connA, "si-1", testPI, ledgerStart.Add(time.Minute))
	claimAll(t, l)
	mustSweep(t, l)
	late := ledgerStart.Add(6 * time.Minute) // 清扫晚了五分钟才看到它(间隔没到暂停的门槛)
	mr.SetTime(late)
	if n := mustSweep(t, l); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	got := claimAll(t, l)
	if len(got) != 1 || got[0].OT != late.Unix()-int64(sweepBackfill/time.Second) {
		t.Fatalf("events = %+v, want the logout backfilled by exactly %s", got, sweepBackfill)
	}
}

// 所有 login 副本停了很久再回来:那段时间续租也进不来,活着的连接不能被当成断了。
func TestLedgerSweepHoldsOffAfterLoginWasDown(t *testing.T) {
	l, mr := newTestLedger(t)
	ctx := context.Background()
	_ = l.Begin(ctx, connA, "si-alive", testPI, time.Time{})
	_ = l.Begin(ctx, connB, "si-dead", testPI, time.Time{})
	claimAll(t, l)
	mustSweep(t, l)

	back := ledgerStart.Add(testLease + 5*time.Minute) // 停了 35 分钟,两条租约都过期了
	mr.SetTime(back)
	if n := mustSweep(t, l); n != 0 {
		t.Fatalf("swept %d right after an outage, want a hold-off", n)
	}
	if _, err := l.Renew(ctx, connA); err != nil { // 活着的连接续上了
		t.Fatal(err)
	}
	renewAt := back.Add(testLease - time.Minute)
	if n := runTo(t, l, mr, back, renewAt); n != 0 {
		t.Fatalf("swept %d inside the hold-off", n)
	}
	// 暂停满一个租约之后,活着的那条还在续租,没续的那条才收尾。
	_, _ = l.Renew(ctx, connA)
	if n := runTo(t, l, mr, renewAt, back.Add(testLease+time.Second)); n != 1 {
		t.Fatalf("swept %d after the hold-off, want only the dead session", n)
	}
	got := claimAll(t, l)
	if len(got) != 1 || got[0].SI != "si-dead" {
		t.Fatalf("events = %+v, want a logout for si-dead only", got)
	}
}

// 领走的事件 15 秒内别的副本看不见;没删的到期后会再出现;放回去的马上能再领。
func TestLedgerClaimHidesEventsUntilAckedOrReleased(t *testing.T) {
	l, mr := newTestLedger(t)
	ctx := context.Background()
	_ = l.Begin(ctx, connA, "si-1", testPI, time.Time{})

	first, err := l.Claim(ctx, MaxBatch)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if again, _ := l.Claim(ctx, MaxBatch); len(again) != 0 {
		t.Fatalf("a claimed event must be hidden from other replicas, got %+v", again)
	}
	mr.SetTime(ledgerStart.Add(claimVisibility + time.Second))
	second, _ := l.Claim(ctx, MaxBatch)
	if len(second) != 1 || second[0].Member != first[0].Member {
		t.Fatalf("an unacked event must come back after the visibility timeout, got %+v", second)
	}
	if err := l.Release(ctx, []string{second[0].Member}); err != nil {
		t.Fatal(err)
	}
	third, _ := l.Claim(ctx, MaxBatch)
	if len(third) != 1 {
		t.Fatalf("a released event must be claimable at once, got %+v", third)
	}
	if err := l.Ack(ctx, []string{third[0].Member}); err != nil {
		t.Fatal(err)
	}
	// 已经删掉的事件,迟到的放回不会让它复活。
	_ = l.Release(ctx, []string{third[0].Member})
	mr.SetTime(ledgerStart.Add(2 * claimVisibility))
	if left, _ := l.Claim(ctx, MaxBatch); len(left) != 0 {
		t.Fatalf("an acked event came back: %+v", left)
	}
}

// 走平台渠道的未成年人不上报,只记截止时刻:进出账本都不产生事件,续租照样回截止时刻,到点照样收尾。
func TestLedgerDeadlineOnlySessionEmitsNothing(t *testing.T) {
	l, mr := newTestLedger(t)
	ctx := context.Background()
	deadline := ledgerStart.Add(10 * time.Minute)
	if err := l.Begin(ctx, connA, "", "", deadline); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Renew(ctx, connA); err != nil || !got.Equal(deadline) {
		t.Fatalf("Renew = %v, %v; want %v", got, err, deadline)
	}
	if got, err := l.Renew(ctx, connB); err != nil || !got.IsZero() {
		t.Fatalf("Renew of an unknown connection = %v, %v; want zero", got, err)
	}
	if n := runTo(t, l, mr, ledgerStart, deadline.Add(time.Second)); n != 1 {
		t.Fatalf("swept %d, want the session ended at its deadline", n)
	}
	_ = l.End(ctx, connA)
	if got := claimAll(t, l); len(got) != 0 {
		t.Fatalf("an unreported session produced events: %+v", got)
	}
}

func TestParseEventRejectsMalformedMembers(t *testing.T) {
	for _, member := range []string{"", "in|x|si|pi", "up|1|si|pi", "in|1|si", "in|1||pi", "in|1|si|"} {
		if _, ok := parseEvent(member); ok {
			t.Errorf("parseEvent(%q) accepted a malformed member", member)
		}
	}
}

func TestNewIDIs32HexCharacters(t *testing.T) {
	a, err := NewID()
	if err != nil || len(a) != 32 {
		t.Fatalf("NewID = %q, %v", a, err)
	}
	if b, _ := NewID(); a == b {
		t.Fatal("two ids must differ")
	}
}
