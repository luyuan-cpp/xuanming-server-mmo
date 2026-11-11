package data

// asset_op_due_shard_test.go —— 资产指令重投的到期扫描按玩家分片(缺口清单第 25 条,
// docs/design/assetop-redelivery-sharding-20261110.md)在 trade outbox 上的 SQL 层回归,不连库
// (录制驱动 recovScript 见 asset_op_recovery_test.go)。钉的是「两段都带分片条件、条件在 ORDER BY / LIMIT 之前、
// 实参顺序与占位符一致」;真库上「两个副本按片分工、合起来不漏不重」在 asset_op_due_shard_integration_test.go。

import (
	"context"
	"database/sql/driver"
	"testing"

	"shared/assetop"
	"shared/workshard"
)

const testDueWorkKind = "trade-assetop"

// dueSegmentSQL 是一段到期扫描在录制驱动里看到的语句(已折叠空白)。
func dueSegmentSQL(attempts, owned string) string {
	return "SELECT `op_id` FROM " + AssetOpTableName +
		" WHERE `status` = ? AND `next_attempt_ms` <= ? AND `lease_until_ms` < ? AND `attempts` " + attempts + " ? AND " + owned +
		" ORDER BY `next_attempt_ms` ASC, `op_id` ASC LIMIT ?"
}

// assertDueArgs 核对一段到期扫描的实参:PENDING、now、now、FreshAttemptLimit、片号……、limit。
func assertDueArgs(t *testing.T, args []driver.Value, nowMs uint64, shards []uint64, limit uint64) {
	t.Helper()
	want := append([]uint64{uint64(PendingStatus()), nowMs, nowMs, uint64(assetop.FreshAttemptLimit)}, shards...)
	want = append(want, limit)
	if len(args) != len(want) {
		t.Fatalf("实参个数 = %d(%v),want %d(%v)", len(args), args, len(want), want)
	}
	for i, w := range want {
		if got := argInt(t, args[i]); got != w {
			t.Fatalf("第 %d 个实参 = %d,want %d(全部实参 %v)", i, got, w, args)
		}
	}
}

// TestListDueOwnedPutsShardFilterBeforeLimitInBothSegments:第一段(新行)不够 limit 时发第二段(老行),
// 两段都带同一个分片条件,位置在 ORDER BY / LIMIT 之前;片号按升序只走绑定参数,第二段的 LIMIT 是缺口。
func TestListDueOwnedPutsShardFilterBeforeLimitInBothSegments(t *testing.T) {
	s := &recovScript{}
	s.rowsFor = func(query string, _ []driver.Value) ([]string, [][]driver.Value) {
		if query == dueSegmentSQL("<", "MOD(player_id,64) IN(?,?)") {
			return []string{"op_id"}, [][]driver.Value{{int64(11)}}
		}
		return []string{"op_id"}, [][]driver.Value{{int64(12)}}
	}
	repo := s.repo(t)
	mine := map[string]bool{workshard.ShardKey(testDueWorkKind, 17): true, workshard.ShardKey(testDueWorkKind, 3): true}
	owned := workshard.SQLFilter(func(key string) bool { return mine[key] }, testDueWorkKind, "player_id")

	ids, err := repo.ListDueOwned(context.Background(), 5000, 4, owned)
	if err != nil {
		t.Fatalf("ListDueOwned: %v", err)
	}
	if len(ids) != 2 || ids[0] != 11 || ids[1] != 12 {
		t.Fatalf("ids = %v,want [11 12](新行在前)", ids)
	}
	calls := s.recordedCalls()
	if len(calls) != 2 {
		t.Fatalf("第一段不够 limit 时应发两段,录到 %d 条: %+v", len(calls), calls)
	}
	for i, attempts := range []string{"<", ">="} {
		if want := dueSegmentSQL(attempts, "MOD(player_id,64) IN(?,?)"); calls[i].sql != want {
			t.Fatalf("第 %d 段语句 =\n  %s\nwant\n  %s", i+1, calls[i].sql, want)
		}
	}
	assertDueArgs(t, calls[0].args, 5000, []uint64{3, 17}, 4)
	assertDueArgs(t, calls[1].args, 5000, []uint64{3, 17}, 3)
}

// TestListDueIsUnshardedListDueOwned:ListDue 就是不带分片条件的 ListDueOwned —— 条件是恒真 1=1,没有片号实参;
// 一片都不归本副本时条件恒假 1=0,同样没有片号实参。
func TestListDueIsUnshardedListDueOwned(t *testing.T) {
	s := &recovScript{}
	repo := s.repo(t)
	if _, err := repo.ListDue(context.Background(), 7000, 5); err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	none := workshard.SQLFilter(func(string) bool { return false }, testDueWorkKind, "player_id")
	if _, err := repo.ListDueOwned(context.Background(), 7000, 5, none); err != nil {
		t.Fatalf("ListDueOwned: %v", err)
	}
	calls := s.recordedCalls()
	if len(calls) != 4 {
		t.Fatalf("两次调用、每次两段(都读回空),应录到 4 条,实际 %d 条: %+v", len(calls), calls)
	}
	for i, c := range calls {
		owned, attempts := "1=1", "<"
		if i >= 2 {
			owned = "1=0"
		}
		if i%2 == 1 {
			attempts = ">="
		}
		if want := dueSegmentSQL(attempts, owned); c.sql != want {
			t.Fatalf("第 %d 条语句 =\n  %s\nwant\n  %s", i+1, c.sql, want)
		}
		assertDueArgs(t, c.args, 7000, nil, 5)
	}

	if ids, err := repo.ListDueOwned(context.Background(), 7000, 0, none); err != nil || ids != nil {
		t.Fatalf("limit=0 = (%v, %v),want (nil, nil)", ids, err)
	}
	if n := len(s.recordedCalls()); n != 4 {
		t.Fatalf("limit=0 不应发 SQL,录到 %d 条", n)
	}
}

// TestCleanupTerminalOpsOwnedPutsShardFilterBeforeLimit:终态清理按片分工(缺口清单第 25 条第二批,设计 §11)。
// 候选读带分片条件,位置在 ORDER BY / LIMIT 之前;实参是四个终态、cutoff、升序片号、LIMIT;
// 零值条件拼出来的就是 sqlListCleanupTerminalOps(CleanupTerminalOps 与既有清理用例钉的那一份)。
func TestCleanupTerminalOpsOwnedPutsShardFilterBeforeLimit(t *testing.T) {
	s := &recovScript{}
	captureCleanupDeleted(t)
	repo := s.repo(t)
	mine := map[string]bool{workshard.ShardKey(CleanupWorkKind, 33): true, workshard.ShardKey(CleanupWorkKind, 1): true}
	owned := workshard.SQLFilter(func(key string) bool { return mine[key] }, CleanupWorkKind, "player_id")

	n, err := repo.CleanupTerminalOpsOwned(context.Background(), cleanupTestNow, cleanupTestRetention, owned)
	if err != nil || n != 0 {
		t.Fatalf("CleanupTerminalOpsOwned = (%d, %v),want (0, nil)", n, err)
	}
	calls := s.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("候选读回空时只发这一条,录到 %d 条: %+v", len(calls), calls)
	}
	want := collapseSpaces(sqlListCleanupTerminalOpsHead + "MOD(player_id,64) IN(?,?)" + sqlListCleanupTerminalOpsTail)
	if calls[0].sql != want {
		t.Fatalf("候选读 =\n  %s\nwant\n  %s", calls[0].sql, want)
	}
	st := cleanupStatuses()
	wantArgs := []uint64{uint64(st[0]), uint64(st[1]), uint64(st[2]), uint64(st[3]), cleanupTestCutoffMs, 1, 33, cleanupBatchSize}
	if len(calls[0].args) != len(wantArgs) {
		t.Fatalf("候选读实参 %v,want %v", calls[0].args, wantArgs)
	}
	for i, w := range wantArgs {
		if got := argInt(t, calls[0].args[i]); got != w {
			t.Fatalf("候选读第 %d 个实参 = %d,want %d(全部 %v)", i+1, got, w, calls[0].args)
		}
	}
	if cleanupTerminalOpsQuery(workshard.Filter{}) != sqlListCleanupTerminalOps {
		t.Fatal("零值条件拼出来的必须就是不分片的 sqlListCleanupTerminalOps")
	}
	for _, other := range []string{testDueWorkKind, "trade-commerce", "trade-receipt-ack"} {
		if CleanupWorkKind == other {
			t.Fatalf("清理的类别键与 %q 重名", other)
		}
	}
}
