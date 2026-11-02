package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

type retentionCall struct {
	olderThan uint64
	limit     int
}

// fakeRetentionTable 按 results 依次回删除行数,错误由 failAt 指定第几次调用失败(从 0 起,-1 不失败)。
type fakeRetentionTable struct {
	results []int64
	failAt  int
	calls   []retentionCall
}

func (f *fakeRetentionTable) deleteOlder(_ context.Context, olderThan uint64, limit int) (int64, error) {
	index := len(f.calls)
	f.calls = append(f.calls, retentionCall{olderThan: olderThan, limit: limit})
	if index == f.failAt {
		return 0, errors.New("injected")
	}
	if index < len(f.results) {
		return f.results[index], nil
	}
	return 0, nil
}

func newTestRetention(now time.Time, batch, maxBatches int, tables ...auditRetentionTable) *auditRetention {
	return &auditRetention{tables: tables, batchSize: batch, maxBatches: maxBatches, now: func() time.Time { return now }}
}

func TestAuditRetention_DeletesUntilShortBatchWithFixedCutoff(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	table := &fakeRetentionTable{results: []int64{10, 10, 3}, failAt: -1}
	r := newTestRetention(now, 10, 100, auditRetentionTable{name: "t", maxAge: time.Hour, deleteOlder: table.deleteOlder})

	r.runRound(context.Background())

	if len(table.calls) != 3 {
		t.Fatalf("不满一批就要停: calls=%d", len(table.calls))
	}
	for _, call := range table.calls {
		if call.olderThan != uint64(now.Add(-time.Hour).Unix()) || call.limit != 10 {
			t.Fatalf("截止时刻和批大小不对: %+v", call)
		}
	}
}

func TestAuditRetention_ErrorStopsThisTableOnly(t *testing.T) {
	failing := &fakeRetentionTable{failAt: 0}
	healthy := &fakeRetentionTable{results: []int64{1}, failAt: -1}
	var observed []string
	var failures int
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 100,
		auditRetentionTable{name: "failing", maxAge: time.Hour, deleteOlder: failing.deleteOlder},
		auditRetentionTable{name: "healthy", maxAge: time.Hour, deleteOlder: healthy.deleteOlder})
	r.observe = func(table string, _ int64, err error) {
		observed = append(observed, table)
		if err != nil {
			failures++
		}
	}

	r.runRound(context.Background())

	if len(failing.calls) != 1 || len(healthy.calls) != 1 {
		t.Fatalf("一张表失败不能影响另一张: failing=%d healthy=%d", len(failing.calls), len(healthy.calls))
	}
	if failures != 1 || len(observed) != 2 {
		t.Fatalf("每条 DELETE 都要观测: observed=%v failures=%d", observed, failures)
	}
}

func TestAuditRetention_RoundBudgetBoundsWork(t *testing.T) {
	table := &fakeRetentionTable{results: []int64{10, 10, 10, 10, 10}, failAt: -1}
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 2, auditRetentionTable{name: "t", maxAge: time.Hour, deleteOlder: table.deleteOlder})

	r.runRound(context.Background())

	if len(table.calls) != 2 {
		t.Fatalf("预算用完要收手,下一轮接着删: calls=%d", len(table.calls))
	}
}

func TestAuditRetention_CancelStopsBetweenBatches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	table := &fakeRetentionTable{results: []int64{10, 10}, failAt: -1}
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 100, auditRetentionTable{name: "t", maxAge: time.Hour, deleteOlder: table.deleteOlder})
	r.pause = time.Hour

	r.sweep(ctx, r.tables[0])

	if len(table.calls) != 1 {
		t.Fatalf("取消后不再发下一批: calls=%d", len(table.calls))
	}
}

func TestRunAuditRetention_PausedSkipsAndCancelReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	opts := AuditRetentionOptions{
		SnapshotMaxAge: time.Hour, TransactionLogMaxAge: time.Hour, BatchSize: 1, Interval: time.Millisecond,
		Paused: func() bool { cancel(); return true },
	}
	go func() {
		// 暂停时不碰库,所以 store 可以是 nil;碰了会 panic。
		RunAuditRetention(ctx, opts, &SnapshotStore{}, &TransactionLogStore{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消之后要退出")
	}
}
