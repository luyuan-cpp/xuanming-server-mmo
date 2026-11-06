package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// 审计表按桶清理(audit_retention.go)的纯逻辑单测:用假的桶锁与删除替身,不连库。真库上的桶锁互斥与按桶删除见
// audit_shard_integration_test.go。

type retentionCall struct {
	shard     uint32
	olderThan uint64
	limit     int
}

// fakeRetentionTable 模拟一张表的各个桶:held 里的桶被别的实例持有(claim 回 ok=false);claimErr 里的桶取锁出错;
// results[shard] 依次回该桶每批的删除行数(用完回 0);failAt[shard] 指定该桶第几次 DELETE 失败(从 0 起)。
// 用例都在单 goroutine 里跑,不加锁。
type fakeRetentionTable struct {
	held     map[uint32]bool
	claimErr map[uint32]bool
	results  map[uint32][]int64
	failAt   map[uint32]int
	onDelete func() // 每次 DELETE 之后调用,可以为 nil

	claims   []uint32
	released []uint32
	calls    []retentionCall
}

func (f *fakeRetentionTable) claim(_ context.Context, shard uint32) (retentionShard, bool, error) {
	f.claims = append(f.claims, shard)
	if f.claimErr[shard] {
		return nil, false, errors.New("injected claim error")
	}
	if f.held[shard] {
		return nil, false, nil
	}
	return &fakeRetentionShard{table: f, shard: shard}, true, nil
}

func (f *fakeRetentionTable) asTable(name string) auditRetentionTable {
	return auditRetentionTable{name: name, maxAge: time.Hour, claim: f.claim}
}

func (f *fakeRetentionTable) callsOf(shard uint32) int {
	n := 0
	for _, c := range f.calls {
		if c.shard == shard {
			n++
		}
	}
	return n
}

type fakeRetentionShard struct {
	table *fakeRetentionTable
	shard uint32
	n     int
}

func (s *fakeRetentionShard) deleteOlder(_ context.Context, olderThan uint64, limit int) (int64, error) {
	f := s.table
	f.calls = append(f.calls, retentionCall{shard: s.shard, olderThan: olderThan, limit: limit})
	index := s.n
	s.n++
	if f.onDelete != nil {
		defer f.onDelete()
	}
	if at, ok := f.failAt[s.shard]; ok && at == index {
		return 0, errors.New("injected delete error")
	}
	if r := f.results[s.shard]; index < len(r) {
		return r[index], nil
	}
	return 0, nil
}

func (s *fakeRetentionShard) release() { s.table.released = append(s.table.released, s.shard) }

func newTestRetention(now time.Time, batch, maxBatches int, tables ...auditRetentionTable) *auditRetention {
	return &auditRetention{
		tables: tables, batchSize: batch, maxBatches: maxBatches,
		now: func() time.Time { return now }, startShard: func() uint32 { return 0 },
	}
}

// 每个桶都取锁、都删到不满一批为止,所有批的截止时刻与批大小都相同(截止时刻在一轮开始时定好);拿到的锁全部放掉。
func TestAuditRetention_SweepsEveryShardUntilShortBatchWithFixedCutoff(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	table := &fakeRetentionTable{results: map[uint32][]int64{3: {10, 10, 3}}}
	r := newTestRetention(now, 10, 100, table.asTable("t"))

	r.runRound(context.Background())

	if len(table.claims) != AuditShardCount {
		t.Fatalf("每个桶都要试一次锁: claims=%v", table.claims)
	}
	if got := table.callsOf(3); got != 3 {
		t.Fatalf("桶 3 不满一批才停: calls=%d", got)
	}
	if len(table.calls) != AuditShardCount-1+3 {
		t.Fatalf("其余桶第一批就不满(0 行),各删一次: calls=%d", len(table.calls))
	}
	for _, call := range table.calls {
		if call.olderThan != uint64(now.Add(-time.Hour).Unix()) || call.limit != 10 {
			t.Fatalf("截止时刻和批大小不对: %+v", call)
		}
	}
	if !slices.Equal(table.released, table.claims) {
		t.Fatalf("拿到的锁都要放: claims=%v released=%v", table.claims, table.released)
	}
}

// 别的实例持有的桶跳过:不删、不放(没拿到就没有可放的),其余桶照删。
func TestAuditRetention_ShardHeldElsewhereIsSkipped(t *testing.T) {
	table := &fakeRetentionTable{held: map[uint32]bool{5: true, 6: true}}
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 100, table.asTable("t"))

	r.runRound(context.Background())

	for _, shard := range []uint32{5, 6} {
		if table.callsOf(shard) != 0 || slices.Contains(table.released, shard) {
			t.Fatalf("桶 %d 被别的实例持有,本实例不许碰: calls=%v released=%v", shard, table.calls, table.released)
		}
	}
	if len(table.calls) != AuditShardCount-2 {
		t.Fatalf("其余桶照删: calls=%d", len(table.calls))
	}
}

// 从 startShard 起轮一圈:几个实例同时清理时各自从不同的桶起步。
func TestAuditRetention_StartsFromGivenShardAndWrapsAround(t *testing.T) {
	table := &fakeRetentionTable{}
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 100, table.asTable("t"))
	r.startShard = func() uint32 { return 14 }

	r.runRound(context.Background())

	want := []uint32{14, 15}
	for s := uint32(0); s < 14; s++ {
		want = append(want, s)
	}
	if !slices.Equal(table.claims, want) {
		t.Fatalf("取锁顺序应从 14 起绕回: got %v want %v", table.claims, want)
	}
}

// 一个桶删失败只停这个桶(锁照放),取锁失败的桶跳过;别的桶、别的表照常。每条 DELETE 与取锁失败都要观测。
func TestAuditRetention_ErrorStopsThisShardOnly(t *testing.T) {
	failing := &fakeRetentionTable{
		failAt:   map[uint32]int{2: 0},
		claimErr: map[uint32]bool{7: true},
		results:  map[uint32][]int64{3: {10, 1}},
	}
	healthy := &fakeRetentionTable{}
	var failures, observedDeletes int
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 100, failing.asTable("failing"), healthy.asTable("healthy"))
	r.observe = func(_ string, _ int64, err error) {
		observedDeletes++
		if err != nil {
			failures++
		}
	}

	r.runRound(context.Background())

	if failing.callsOf(2) != 1 || !slices.Contains(failing.released, 2) {
		t.Fatalf("桶 2 失败后不再删,但锁要放: calls=%d released=%v", failing.callsOf(2), failing.released)
	}
	if failing.callsOf(3) != 2 {
		t.Fatalf("桶 2 失败不影响桶 3: calls=%d", failing.callsOf(3))
	}
	if failing.callsOf(7) != 0 || slices.Contains(failing.released, 7) {
		t.Fatalf("桶 7 取锁失败,不许删、不许放")
	}
	if len(healthy.calls) != AuditShardCount {
		t.Fatalf("一张表的失败不能影响另一张: healthy calls=%d", len(healthy.calls))
	}
	if failures != 2 {
		t.Fatalf("删除失败与取锁失败各观测一次: failures=%d", failures)
	}
	// 失败表:15 个拿到锁的桶各至少一条 DELETE + 桶 3 多一条 + 桶 7 的取锁失败;健康表 16 条。
	if want := (AuditShardCount - 1) + 1 + 1 + AuditShardCount; observedDeletes != want {
		t.Fatalf("每条 DELETE 都要观测: observed=%d want=%d", observedDeletes, want)
	}
}

// 每个桶每轮有预算:用完就收手、放锁,下一轮接着删。
func TestAuditRetention_ShardBudgetBoundsWork(t *testing.T) {
	table := &fakeRetentionTable{results: map[uint32][]int64{0: {10, 10, 10, 10, 10}}}
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 2, table.asTable("t"))

	r.runRound(context.Background())

	if got := table.callsOf(0); got != 2 {
		t.Fatalf("预算用完要收手: calls=%d", got)
	}
	if !slices.Contains(table.released, 0) {
		t.Fatal("收手时锁要放")
	}
}

// 取消之后:不再发下一批,也不再去取下一个桶的锁。
func TestAuditRetention_CancelStopsBetweenBatchesAndShards(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	table := &fakeRetentionTable{results: map[uint32][]int64{0: {10, 10}}, onDelete: cancel}
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 100, table.asTable("t"))
	r.pause = time.Hour

	r.sweepTable(ctx, r.tables[0])

	if len(table.calls) != 1 || len(table.claims) != 1 {
		t.Fatalf("取消后不再发下一批、不再取下一个桶: calls=%d claims=%v", len(table.calls), table.claims)
	}
	if !slices.Equal(table.released, []uint32{0}) {
		t.Fatalf("取消时手里的锁要放: released=%v", table.released)
	}
}

// 进入排空(Paused)之后不再发新的 DELETE,也不再取锁。
func TestAuditRetention_PausedStopsBeforeNextBatch(t *testing.T) {
	paused := false
	table := &fakeRetentionTable{results: map[uint32][]int64{0: {10, 10}}, onDelete: func() { paused = true }}
	r := newTestRetention(time.Unix(1_000_000, 0), 10, 100, table.asTable("t"))
	r.pause = 0
	r.paused = func() bool { return paused }

	r.runRound(context.Background())

	if len(table.calls) != 1 || len(table.claims) != 1 {
		t.Fatalf("排空后不再删、不再取锁: calls=%d claims=%v", len(table.calls), table.claims)
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
		// 暂停时不碰库,所以 store 可以是空壳;碰了会 panic。
		RunAuditRetention(ctx, opts, &SnapshotStore{}, &TransactionLogStore{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消之后要退出")
	}
}

// 锁名在整台服务器上共享:带表、桶与库名;超过 GET_LOCK 的 64 字符上限时截断而不是报错。
func TestAuditRetentionLockName(t *testing.T) {
	if got, want := auditRetentionLockName("mmorpg_global_ledger", "txlog", 7), "ds_audit_retention.txlog.07.mmorpg_global_ledger"; got != want {
		t.Fatalf("lock name = %q, want %q", got, want)
	}
	if auditRetentionLockName("db", "txlog", 1) == auditRetentionLockName("db", "snap", 1) ||
		auditRetentionLockName("db", "txlog", 1) == auditRetentionLockName("db", "txlog", 2) {
		t.Fatal("不同表、不同桶的锁名必须不同")
	}
	long := auditRetentionLockName(strings.Repeat("x", 100), "snap", 15)
	if len(long) != maxMigrateLockNameLen || !strings.HasPrefix(long, "ds_audit_retention.snap.15.") {
		t.Fatalf("超长库名要截断到 %d 字符且保留表与桶: %q", maxMigrateLockNameLen, long)
	}
}
