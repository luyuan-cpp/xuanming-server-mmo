package nppa

import (
	"context"
	"errors"
	"testing"
	"time"

	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
)

// reporterEnv 是一个副本的发送循环加它背后的账本。at 同时拨 Redis 的时钟与副本自己的时钟(副本的钟快 1 秒:
// 事件的时刻取自 Redis,要早于副本的现在才发)。
type reporterEnv struct {
	r      *Reporter
	l      *Ledger
	mr     *miniredis.Miniredis
	now    time.Time
	sent   [][]Behavior
	result func() ([]int, error)
}

func newReporterEnv(t *testing.T) *reporterEnv {
	t.Helper()
	l, mr := newTestLedger(t)
	e := &reporterEnv{l: l, mr: mr, result: func() ([]int, error) { return nil, nil }}
	e.r = &Reporter{ledger: l, maxAge: reportMaxAge, now: func() time.Time { return e.now },
		send: func(_ context.Context, batch []Behavior) ([]int, error) {
			e.sent = append(e.sent, append([]Behavior(nil), batch...))
			return e.result()
		}}
	e.at(ledgerStart)
	return e
}

func (e *reporterEnv) at(redisNow time.Time) {
	e.mr.SetTime(redisNow)
	e.now = redisNow.Add(time.Second)
}

// pending 是账本里还没删掉的事件数(含已被领走、暂时不可见的)。
func (e *reporterEnv) pending(t *testing.T) int {
	t.Helper()
	n, err := e.l.rdb.ZCard(context.Background(), redisslot.NppaOutboxKey()).Result()
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func TestTickReportsLedgerEventsOnceAndRemovesThem(t *testing.T) {
	e := newReporterEnv(t)
	ctx := context.Background()
	_ = e.l.Begin(ctx, connA, "si-1", testPI, time.Time{})
	_ = e.l.End(ctx, connA)

	e.r.tick(ctx)
	if len(e.sent) != 1 || len(e.sent[0]) != 2 || e.sent[0][0].BT != BehaviorLogin || e.sent[0][1].BT != BehaviorLogout {
		t.Fatalf("sent = %+v, want one batch with the login then the logout", e.sent)
	}
	e.at(ledgerStart.Add(time.Minute))
	e.r.tick(ctx)
	if len(e.sent) != 1 || e.pending(t) != 0 {
		t.Fatalf("reported events must be removed: batches=%d pending=%d", len(e.sent), e.pending(t))
	}
}

// 国家系统拒收的条目重发也不会被接受:照样删掉,不反复报。
func TestTickRemovesBehaviorsTheSystemRejected(t *testing.T) {
	e := newReporterEnv(t)
	e.result = func() ([]int, error) { return []int{1}, nil }
	_ = e.l.Begin(context.Background(), connA, "si-1", testPI, time.Time{})
	e.r.tick(context.Background())
	if len(e.sent) != 1 || e.pending(t) != 0 {
		t.Fatalf("batches=%d pending=%d, want the rejected behavior gone", len(e.sent), e.pending(t))
	}
}

// 发送失败的批次留在账本里,领取到期后再发;期间不会每拍都打过去。
func TestTickRetriesAFailedBatchAfterTheVisibilityTimeout(t *testing.T) {
	e := newReporterEnv(t)
	ctx := context.Background()
	e.result = func() ([]int, error) { return nil, errors.New("network down") }
	_ = e.l.Begin(ctx, connA, "si-1", testPI, time.Time{})

	e.r.tick(ctx)
	e.r.tick(ctx)
	if len(e.sent) != 1 || e.pending(t) != 1 {
		t.Fatalf("batches=%d pending=%d, want one attempt and the event kept", len(e.sent), e.pending(t))
	}
	e.result = func() ([]int, error) { return nil, nil }
	e.at(ledgerStart.Add(claimVisibility + time.Second))
	e.r.tick(ctx)
	if len(e.sent) != 2 || e.pending(t) != 0 {
		t.Fatalf("batches=%d pending=%d, want the retry to succeed and remove it", len(e.sent), e.pending(t))
	}
}

// 超过 150 秒的事件报了也会被拒:删掉并计数,不占上报额度。
func TestTickDropsBehaviorsTooOldToReport(t *testing.T) {
	e := newReporterEnv(t)
	_ = e.l.Begin(context.Background(), connA, "si-1", testPI, time.Time{})
	e.at(ledgerStart.Add(reportMaxAge + 5*time.Second))
	e.r.tick(context.Background())
	if len(e.sent) != 0 || e.pending(t) != 0 {
		t.Fatalf("batches=%d pending=%d, want nothing sent and the stale event gone", len(e.sent), e.pending(t))
	}
}

// 本副本的钟比 Redis 慢:事件的时刻还没「到」,这一拍不发也不删,等钟追上、领取到期后再发。
func TestTickLeavesBehaviorsStampedAheadOfTheLocalClock(t *testing.T) {
	e := newReporterEnv(t)
	ctx := context.Background()
	_ = e.l.Begin(ctx, connA, "si-1", testPI, time.Time{})
	e.now = ledgerStart.Add(-5 * time.Second)
	e.r.tick(ctx)
	if len(e.sent) != 0 || e.pending(t) != 1 {
		t.Fatalf("batches=%d pending=%d, want the event kept for later", len(e.sent), e.pending(t))
	}
	e.at(ledgerStart.Add(claimVisibility + time.Second))
	e.r.tick(ctx)
	if len(e.sent) != 1 {
		t.Fatalf("batches=%d, want it sent once the clock caught up", len(e.sent))
	}
}

// 上报额度在所有副本之间共用:这一刻的名额已被别的副本用掉时不发,事件放回账本,马上可以再领。
func TestTickReleasesTheBatchWhenTheSharedQuotaIsUsedUp(t *testing.T) {
	e := newReporterEnv(t)
	ctx := context.Background()
	e.r.quota = NewQuota(e.l.rdb)
	if ok, err := e.r.quota.Allow(ctx, LoginoutQuota); err != nil || !ok {
		t.Fatalf("another replica takes this instant's slot: ok=%v err=%v", ok, err)
	}
	_ = e.l.Begin(ctx, connA, "si-1", testPI, time.Time{})
	e.r.tick(ctx)
	if len(e.sent) != 0 {
		t.Fatalf("sent %d batches without a quota slot", len(e.sent))
	}
	if claimed, _ := e.l.Claim(ctx, MaxBatch); len(claimed) != 1 {
		t.Fatalf("the unsent batch must be claimable at once, got %+v", claimed)
	}
}

// 每一拍先清扫:租约到期的会话补一条下线,和别的事件一起发出去。
func TestTickSweepsBeforeSending(t *testing.T) {
	e := newReporterEnv(t)
	ctx := context.Background()
	_ = e.l.Begin(ctx, connA, "si-1", testPI, ledgerStart.Add(time.Minute))
	e.at(ledgerStart.Add(2 * time.Minute))
	e.r.tick(ctx)
	if len(e.sent) != 1 || len(e.sent[0]) != 2 || e.sent[0][1].BT != BehaviorLogout || e.sent[0][1].OT != ledgerStart.Unix()+60 {
		t.Fatalf("sent = %+v, want the login and a logout stamped with the lease end", e.sent)
	}
}
