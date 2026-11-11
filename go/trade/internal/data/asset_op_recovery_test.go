package data

// asset_op_recovery_test.go —— 落地恢复(docs/design/landing-recovery-sync-digest.md §5.3 / §6)在 trade outbox 上的回归,不连库。
//
// 被测对象是 AssetOpRepo 的三个可选能力(PlayerPendingLister / PendingClassCounter / Abandoner)、
// last_send_ms 契约(go/shared/assetop/abandon.go 文件头)的五处 SQL 写法,以及 ABANDONED 的状态映射与终态清理口径。
// 真库行为(UPDATE 的求值顺序、IF(?, …) 绑 bool 的结果、落空的 CAS 一个字不写、端到端接线)在
// asset_op_landing_integration_test.go(integration 标签,TRADE_TEST_MYSQL_DSN 门控);这里钉的是"发了什么语句、什么顺序、
// 带了什么条件、结果怎么解释"这些纯 SQL 层的事实 —— 它们写错了在编译期与单元测试之外都不会报错,
// 只会让放弃"永远不发生"或"不该发生时发生"。本文件不依赖 MySQL,任何环境的 go test 都跑。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	assetpb "proto/common/asset"
	tradepb "proto/trade"

	"shared/assetop"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// ── 录制驱动(只录语句与实参,按配置回结果)──────────────────────

// recovCall 是录音带上的一条:语句(已折叠空白)与驱动层拿到的实参。BEGIN / COMMIT / ROLLBACK 以伪语句入带。
type recovCall struct {
	sql  string
	args []driver.Value
}

// recovScript 是一次用例的脚本:每条写语句回 affected,每条查询由 rowsFor 给结果集(nil 或回 nil 列 = 空结果)。
type recovScript struct {
	mu       sync.Mutex
	calls    []recovCall
	affected int64
	rowsFor  func(query string, args []driver.Value) (cols []string, data [][]driver.Value)
}

func (s *recovScript) record(stmt string, args []driver.NamedValue) []driver.Value {
	vals := make([]driver.Value, 0, len(args))
	for _, a := range args {
		vals = append(vals, a.Value)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recovCall{sql: collapseSpaces(stmt), args: vals})
	return vals
}

// recordedCalls 返回录下的全部调用(拷贝)。
func (s *recovScript) recordedCalls() []recovCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recovCall(nil), s.calls...)
}

// callsWithPrefix 返回以 prefix 打头的调用,保持发出顺序。
func (s *recovScript) callsWithPrefix(prefix string) []recovCall {
	var out []recovCall
	for _, c := range s.recordedCalls() {
		if strings.HasPrefix(c.sql, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (s *recovScript) repo(t *testing.T) *AssetOpRepo {
	t.Helper()
	db := sql.OpenDB(recovConnector{s: s})
	t.Cleanup(func() { _ = db.Close() })
	repo, err := NewAssetOpRepo(db, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	return repo
}

type recovConnector struct{ s *recovScript }

func (c recovConnector) Connect(context.Context) (driver.Conn, error) {
	return &recovConn{s: c.s}, nil
}

func (c recovConnector) Driver() driver.Driver { return recovDriver{s: c.s} }

type recovDriver struct{ s *recovScript }

func (d recovDriver) Open(string) (driver.Conn, error) { return &recovConn{s: d.s}, nil }

type recovConn struct{ s *recovScript }

func (c *recovConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("录制驱动不支持 Prepare:被测路径全部走 QueryContext / ExecContext")
}
func (c *recovConn) Close() error { return nil }
func (c *recovConn) Begin() (driver.Tx, error) {
	return nil, errors.New("录制驱动只支持 BeginTx")
}

func (c *recovConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.s.record("BEGIN", nil)
	return recovTx{s: c.s}, nil
}

func (c *recovConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals := c.s.record(query, args)
	if c.s.rowsFor == nil {
		return &seqFakeRows{cols: []string{"op_id"}}, nil
	}
	cols, data := c.s.rowsFor(collapseSpaces(query), vals)
	if cols == nil {
		cols = []string{"op_id"}
	}
	return &seqFakeRows{cols: cols, data: data}, nil
}

func (c *recovConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.s.record(query, args)
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return driver.RowsAffected(c.s.affected), nil
}

type recovTx struct{ s *recovScript }

func (t recovTx) Commit() error {
	t.s.record("COMMIT", nil)
	return nil
}

func (t recovTx) Rollback() error {
	t.s.record("ROLLBACK", nil)
	return nil
}

// argInt 把驱动层实参解成 uint64(database/sql 把各宽度整数统一转成 int64)。
func argInt(t *testing.T, v driver.Value) uint64 {
	t.Helper()
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("实参不是整数: %#v", v)
	}
	return uint64(n)
}

// ── 状态映射与列 ────────────────────────────────────────────

// TestStatusToRecordIsABijection:assetop.Status(语义)与 TradeAssetOpStatus(库值)必须一一对应 ——
// 每个合法语义值落成一个互不相同、非 UNSPECIFIED 的库值,每个非 UNSPECIFIED 的库值恰好被一个语义值命中。
// 两边任一边加了状态却没同改映射(这次的 ABANDONED 就是例子),放弃出来的行会落成 UNSPECIFIED、客服查不到结局。
func TestStatusToRecordIsABijection(t *testing.T) {
	seen := map[tradepb.TradeAssetOpStatus]assetop.Status{}
	for s := assetop.StatusPending; s <= assetop.StatusAbandoned; s++ {
		rec := StatusToRecord(s)
		if rec == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED {
			t.Errorf("语义值 %s 映射成 UNSPECIFIED", s)
			continue
		}
		if prev, dup := seen[rec]; dup {
			t.Errorf("语义值 %s 与 %s 映射到同一个库值 %v", s, prev, rec)
		}
		seen[rec] = s
	}
	values := tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED.Descriptor().Values()
	for i := 0; i < values.Len(); i++ {
		rec := tradepb.TradeAssetOpStatus(values.Get(i).Number())
		if rec == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED {
			continue
		}
		if _, ok := seen[rec]; !ok {
			t.Errorf("库值 %v 没有任何语义值映射过来:加了库内状态却没同改 StatusToRecord", rec)
		}
	}
	if got := StatusToRecord(assetop.StatusAbandoned); got != tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED {
		t.Errorf("StatusToRecord(Abandoned) = %v, want ABANDONED", got)
	}
}

// TestAssetOpHasLastSendMsColumn:last_send_ms 是自动放弃条件 4 的唯一依据。proto 缺字段或手写列清单漏了它,
// Claim 回读的 LastSendMs 恒为 0,放弃路径一律按"Store 没维护"跳过 —— 不出错,只是放弃永远不发生。
func TestAssetOpHasLastSendMsColumn(t *testing.T) {
	field := (&tradepb.TradeAssetOpRecord{}).ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name("last_send_ms"))
	if field == nil {
		t.Fatal("TradeAssetOpRecord 缺 last_send_ms")
	}
	if field.Number() != 24 {
		t.Errorf("last_send_ms 字段号 = %d, want 24(字段号上线后不复用)", field.Number())
	}
	if !strings.Contains(assetOpColumns, "`last_send_ms`") {
		t.Error("assetOpColumns 缺 last_send_ms:插行与回读都碰不到这一列")
	}
}

// TestInsertLastSendMsNeverBelowCreated:LastSendMs 契约第 1 条 —— 插行写的值不小于 created_ms
// (插行即持租约,提交后的同步投递紧接着就可能发出);生产者给了更晚的值照用(只许偏大)。
func TestInsertLastSendMsNeverBelowCreated(t *testing.T) {
	cases := []struct {
		name              string
		created, lastSend uint64
		want              uint64
	}{
		{"生产者漏填", 1000, 0, 1000},
		{"与创建同刻", 1000, 1000, 1000},
		{"生产者给了更晚的时刻", 1000, 1500, 1500},
		{"生产者给了更早的时刻(不许偏小)", 1000, 900, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &tradepb.TradeAssetOpRecord{CreatedMs: tc.created, LastSendMs: tc.lastSend}
			if got := insertLastSendMs(rec); got != tc.want {
				t.Errorf("insertLastSendMs = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFinalizeAndResolveRefuseAbandoned:ABANDONED 只能经 Abandon(带令牌、六条证据齐全)写入。
// Finalize / ResolveManually 必须在碰事务之前拒掉它 —— 本用例的仓库没有连接池,越过这一步会当场失败。
func TestFinalizeAndResolveRefuseAbandoned(t *testing.T) {
	repo, err := NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	if done, err := repo.Finalize(context.Background(), assetop.Op{OpID: 1}, assetop.StatusAbandoned, assetop.Result{}, 1); err == nil || done {
		t.Errorf("Finalize(Abandoned) = (%v, %v),必须拒绝", done, err)
	}
	if done, err := repo.ResolveManually(context.Background(),
		assetop.ManualResolution{OpID: 1, Final: assetop.StatusAbandoned, Operator: "ops", Reason: "test"}, 1); err == nil || done {
		t.Errorf("ResolveManually(Abandoned) = (%v, %v),必须拒绝(人工判定放弃应按证据选 ABORTED)", done, err)
	}
}

// ── last_send_ms 契约的五处 SQL ──────────────────────────────

// TestClaimSQLFoldsOldLeaseBeforeOverwritingIt:契约第 2、5 条。last_send_ms 的赋值必须写在改写 lease_until_ms
// 与 updated_ms **之前**:MySQL 单表 UPDATE 从左到右求值,写在后面读到的就是本次新写的租约 / 时刻,
// 上一个没收尾的租约(发完请求就崩溃的那一次)就被盖掉了,放弃会撞上仍可能在途的请求。
func TestClaimSQLFoldsOldLeaseBeforeOverwritingIt(t *testing.T) {
	stmt := collapseSpaces(sqlClaimAssetOp)
	assign := strings.Index(stmt, "`last_send_ms` =")
	lease := strings.Index(stmt, "`lease_until_ms` = ?")
	updated := strings.Index(stmt, "`updated_ms` = ?")
	if assign < 0 || lease < 0 || updated < 0 {
		t.Fatalf("领取语句缺少必要的赋值: %s", stmt)
	}
	if assign > lease || assign > updated {
		t.Errorf("last_send_ms 必须先于 lease_until_ms / updated_ms 赋值: %s", stmt)
	}
	for _, want := range []string{
		// 存量行懒初始化(契约第 5 条):0 时取 GREATEST(created_ms, updated_ms)。
		"IF(`last_send_ms` = 0, GREATEST(`created_ms`, `updated_ms`), `last_send_ms`)",
		// 折旧租约(契约第 2 条)。
		", `lease_until_ms`),",
		// 领取条件不变:只看 status 与租约。
		"WHERE `op_id` = ? AND `status` = ? AND `lease_until_ms` < ?",
	} {
		if !strings.Contains(stmt, want) {
			t.Errorf("领取语句缺 %q: %s", want, stmt)
		}
	}
}

// TestRescheduleSQLAdvancesOnlyWhenSent:契约第 3 条。本地 NOT_HERE(res.Local)根本没发出请求,推进它会让
// 停放中的离线行永远"刚发过",放弃永远不会发生;非 Local 一律按"发过"推进到 nowMs。
func TestRescheduleSQLAdvancesOnlyWhenSent(t *testing.T) {
	stmt := collapseSpaces(sqlRescheduleAssetOp)
	if !strings.Contains(stmt, "`last_send_ms` = IF(?, GREATEST(`last_send_ms`, ?), `last_send_ms`)") {
		t.Errorf("重排语句的 last_send_ms 写法不符契约第 3 条: %s", stmt)
	}
	if !strings.Contains(stmt, "WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?") {
		t.Errorf("重排语句必须带令牌与 PENDING 条件: %s", stmt)
	}
}

// TestPoisonAndReleaseLeaveLastSendMsAlone:契约第 4 条。毒行推迟与归还租约只清租约;
// 推进 last_send_ms 会让放弃方自己的领取 / 归还把"最后一次可能的发送"不断刷新,放弃永远不会发生。
func TestPoisonAndReleaseLeaveLastSendMsAlone(t *testing.T) {
	for name, stmt := range map[string]string{"毒行推迟": sqlPoisonAssetOp, "归还租约": sqlReleaseAssetOpLease} {
		s := collapseSpaces(stmt)
		if strings.Contains(s, "last_send_ms") {
			t.Errorf("%s 语句不得改 last_send_ms: %s", name, s)
		}
		if !strings.Contains(s, "`lease_until_ms` = 0") {
			t.Errorf("%s 语句必须清租约: %s", name, s)
		}
		if !strings.Contains(s, "`lease_token` = ?") || !strings.Contains(s, "`status` = ?") {
			t.Errorf("%s 语句必须以令牌与 PENDING 为条件: %s", name, s)
		}
	}
	release := collapseSpaces(sqlReleaseAssetOpLease)
	for _, col := range []string{"`attempts`", "`next_attempt_ms`"} {
		if strings.Contains(release, col) {
			t.Errorf("归还租约不得改 %s(Abandoner.ReleaseLease 契约): %s", col, release)
		}
	}
}

// TestClaimReadsBackCreatedAndLastSendMs:领取成功后回读的 Op 必须带上 created_ms 与 last_send_ms ——
// 自动放弃只认这两个回读值(0 一律 fail-closed 跳过)。漏填的表现是放弃永远不发生,且没有任何报错。
func TestClaimReadsBackCreatedAndLastSendMs(t *testing.T) {
	const (
		opID       = 77
		createdMs  = 1_000
		lastSendMs = 5_000
		token      = 99
	)
	s := &recovScript{affected: 1}
	s.rowsFor = func(query string, _ []driver.Value) ([]string, [][]driver.Value) {
		if !strings.HasPrefix(query, "SELECT `op_id`, `player_id`") {
			return nil, nil
		}
		cols := splitColumns(assetOpColumns)
		row := make([]driver.Value, len(cols))
		for i, col := range cols {
			switch col {
			case "op_id":
				row[i] = int64(opID)
			case "player_id":
				row[i] = int64(42)
			case "stream":
				row[i] = int64(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT)
			case "status":
				row[i] = int64(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING)
			case "created_ms":
				row[i] = int64(createdMs)
			case "last_send_ms":
				row[i] = int64(lastSendMs)
			case "payload":
				row[i] = nil
			case "resolved_by", "resolve_reason":
				row[i] = ""
			default:
				row[i] = int64(0)
			}
		}
		return cols, [][]driver.Value{row}
	}
	repo := s.repo(t)

	op, ok, err := repo.Claim(context.Background(), opID, 2_000, 12_000, 3_602_000, token)
	if err != nil || !ok {
		t.Fatalf("Claim = (%v, %v)", ok, err)
	}
	if op.CreatedMs != createdMs || op.LastSendMs != lastSendMs {
		t.Errorf("回读 CreatedMs=%d LastSendMs=%d, want %d / %d", op.CreatedMs, op.LastSendMs, createdMs, lastSendMs)
	}
	if op.LeaseToken != token {
		t.Errorf("回读 LeaseToken=%d, want %d(Reschedule / Abandon / ReleaseLease 都要带回它)", op.LeaseToken, token)
	}
	claims := s.callsWithPrefix("UPDATE " + AssetOpTableName + " SET `last_send_ms`")
	if len(claims) != 1 {
		t.Fatalf("应发出恰一条领取 CAS,录到 %d 条: %+v", len(claims), s.recordedCalls())
	}
}

// TestRescheduleBindsNotLocal:重排的 IF(?, …) 条件位绑定的必须是 !res.Local。绑反了,
// 真发过请求的行不推进(放弃可能撞上在途请求),本地 NOT_HERE 的行反而推进(放弃永远不发生)。
func TestRescheduleBindsNotLocal(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(fmt.Sprintf("local=%v", local), func(t *testing.T) {
			s := &recovScript{affected: 1, rowsFor: lockRowsFor(true)}
			repo := s.repo(t)
			op := assetop.Op{OpID: 5, LeaseToken: 9}
			if err := repo.Reschedule(context.Background(), op, 60_000, assetop.Result{Local: local}, 1_234); err != nil {
				t.Fatalf("Reschedule: %v", err)
			}
			calls := s.callsWithPrefix("UPDATE " + AssetOpTableName + " SET `attempts`")
			if len(calls) != 1 {
				t.Fatalf("应发出恰一条重排语句,录到 %+v", s.recordedCalls())
			}
			args := calls[0].args
			// 实参顺序:next, durable, outcome, reason, updated_ms, sent, now, op_id, status, token。
			if len(args) != 10 {
				t.Fatalf("重排语句实参数 = %d, want 10", len(args))
			}
			sent, ok := args[5].(bool)
			if !ok {
				t.Fatalf("第 6 个实参应是布尔(!res.Local),得到 %#v", args[5])
			}
			if sent != !local {
				t.Errorf("sent = %v, want %v(= !res.Local)", sent, !local)
			}
			if got := argInt(t, args[6]); got != 1_234 {
				t.Errorf("推进到的时刻 = %d, want nowMs 1234", got)
			}
		})
	}
}

// TestRescheduleAndPoisonLockRowFirst:重排与毒行推迟都改 idx_0 的 next_attempt_ms,必须是
// BEGIN → 主键点锁 → 带复核的点改 → COMMIT(与终结同形,见 asset_op_repo.go"op 行的写者:先主键点锁再 CAS")。
// 自动提交在 TiDB 上会与终结各持一半互等、检测不到(2026-10-18 真 TiDB 实测,事故文档 §9.7);改回自动提交这里会红。
// 重排落空(点锁读不到行 / CAS 0 行)回 ErrLeaseLost 并回滚。
func TestRescheduleAndPoisonLockRowFirst(t *testing.T) {
	op := assetop.Op{OpID: 5, LeaseToken: 9}

	t.Run("重排提交", func(t *testing.T) {
		s := &recovScript{affected: 1, rowsFor: lockRowsFor(true)}
		if err := s.repo(t).Reschedule(context.Background(), op, 60_000, assetop.Result{}, 1_234); err != nil {
			t.Fatalf("Reschedule: %v", err)
		}
		want := []string{"BEGIN", collapseSpaces(sqlLockAssetOp), collapseSpaces(sqlRescheduleAssetOp), "COMMIT"}
		if got := callSQLs(s.recordedCalls()); !reflect.DeepEqual(got, want) {
			t.Fatalf("重排应是 BEGIN → 主键点锁 → CAS → COMMIT,录到 %+v", got)
		}
	})
	t.Run("重排行已不存在", func(t *testing.T) {
		s := &recovScript{affected: 1, rowsFor: lockRowsFor(false)}
		err := s.repo(t).Reschedule(context.Background(), op, 60_000, assetop.Result{}, 1_234)
		if !errors.Is(err, assetop.ErrLeaseLost) {
			t.Fatalf("点锁读不到行应回 ErrLeaseLost,得到 %v", err)
		}
		want := []string{"BEGIN", collapseSpaces(sqlLockAssetOp), "ROLLBACK"}
		if got := callSQLs(s.recordedCalls()); !reflect.DeepEqual(got, want) {
			t.Fatalf("读不到行应不发 CAS、回滚,录到 %+v", got)
		}
	})
	t.Run("重排 CAS 落空", func(t *testing.T) {
		s := &recovScript{affected: 0, rowsFor: lockRowsFor(true)}
		err := s.repo(t).Reschedule(context.Background(), op, 60_000, assetop.Result{}, 1_234)
		if !errors.Is(err, assetop.ErrLeaseLost) {
			t.Fatalf("CAS 0 行应回 ErrLeaseLost,得到 %v", err)
		}
		want := []string{"BEGIN", collapseSpaces(sqlLockAssetOp), collapseSpaces(sqlRescheduleAssetOp), "ROLLBACK"}
		if got := callSQLs(s.recordedCalls()); !reflect.DeepEqual(got, want) {
			t.Fatalf("CAS 落空应回滚且不重跑(ErrLeaseLost 不可重试),录到 %+v", got)
		}
	})
	t.Run("毒行推迟", func(t *testing.T) {
		s := &recovScript{affected: 1, rowsFor: lockRowsFor(true)}
		s.repo(t).markPoison(context.Background(), op.OpID, op.LeaseToken, 2_000, 3_602_000)
		want := []string{"BEGIN", collapseSpaces(sqlLockAssetOp), collapseSpaces(sqlPoisonAssetOp), "COMMIT"}
		if got := callSQLs(s.recordedCalls()); !reflect.DeepEqual(got, want) {
			t.Fatalf("毒行推迟应是 BEGIN → 主键点锁 → CAS → COMMIT,录到 %+v", got)
		}
	})
}

// TestCommerceOpWritesLockEachRowByPrimaryKey:合同名下 op 行的锁定写必须是「候选普通读 → 按 op_id 升序逐行
// 主键点锁 → 主键点改」,不能退回 `UPDATE … WHERE ref_kind=2 AND ref_id=?`(TiDB 上与终结反序成环,事故文档 §9.7)。
// 点锁读不到的行跳过。
func TestCommerceOpWritesLockEachRowByPrimaryKey(t *testing.T) {
	rowsFor := func(query string, args []driver.Value) ([]string, [][]driver.Value) {
		switch query {
		case collapseSpaces(sqlListOpIDsByCommerce):
			return []string{"op_id"}, [][]driver.Value{{int64(3)}, {int64(7)}, {int64(9)}}
		case collapseSpaces(sqlLockAssetOp):
			if argInt(t, args[0]) == 7 { // 7 号已被清理
				return nil, nil
			}
			return []string{"op_id"}, [][]driver.Value{{args[0]}}
		}
		return nil, nil
	}
	for name, tc := range map[string]struct {
		call func(*AssetOpRepo, *sql.Tx) error
		stmt string
	}{
		"取消提前截止": {func(r *AssetOpRepo, tx *sql.Tx) error {
			return r.ExpireCommerceEscrowDebitsTx(context.Background(), tx, 42, 5_000)
		}, sqlExpireEscrowDebitOp},
		"结案标记消费": {func(r *AssetOpRepo, tx *sql.Tx) error {
			return r.MarkCommerceOpsConsumedTx(context.Background(), tx, 42)
		}, sqlConsumeCommerceOp},
	} {
		t.Run(name, func(t *testing.T) {
			s := &recovScript{affected: 1, rowsFor: rowsFor}
			repo := s.repo(t)
			tx, err := repo.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			if err := tc.call(repo, tx); err != nil {
				t.Fatalf("call: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			lock, upd := collapseSpaces(sqlLockAssetOp), collapseSpaces(tc.stmt)
			want := []string{"BEGIN", collapseSpaces(sqlListOpIDsByCommerce), lock, upd, lock, lock, upd, "COMMIT"}
			calls := s.recordedCalls()
			if got := callSQLs(calls); !reflect.DeepEqual(got, want) {
				t.Fatalf("应是 候选读 → 3 点锁 → 3 点改 → 7 点锁(无行,跳过)→ 9 点锁 → 9 点改,录到 %+v", got)
			}
			if got := argInt(t, calls[3].args[len(calls[3].args)-1]); got != 3 {
				t.Errorf("第一条点改的 op_id = %d, want 3", got)
			}
			for _, c := range calls {
				if strings.Contains(c.sql, "ref_id") && strings.HasPrefix(c.sql, "UPDATE") {
					t.Fatalf("不许按 ref 批量 UPDATE:%s", c.sql)
				}
			}
		})
	}
}

// ── Abandoner ───────────────────────────────────────────────

// TestAbandonSQLShape:放弃 CAS 以 (op_id, status=PENDING, lease_token) 为条件;不置 durable(不是 scene 确认的结局)、
// 不改 last_outcome(保留最后一次真实答复作证据)、不动 last_send_ms;next_attempt_ms 同写终结时刻(清理按它判龄)。
func TestAbandonSQLShape(t *testing.T) {
	stmt := collapseSpaces(sqlAbandonAssetOp)
	if !strings.Contains(stmt, "WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?") {
		t.Errorf("放弃 CAS 缺令牌或 PENDING 条件: %s", stmt)
	}
	if !strings.Contains(stmt, "`next_attempt_ms` = ?") || !strings.Contains(stmt, "`lease_until_ms` = 0") {
		t.Errorf("放弃必须清租约并把 next_attempt_ms 写成终结时刻: %s", stmt)
	}
	for _, col := range []string{"`durable`", "`last_outcome`", "`last_send_ms`", "`resolved_by`"} {
		if strings.Contains(stmt, col) {
			t.Errorf("放弃语句不得改 %s: %s", col, stmt)
		}
	}
}

// TestAbandonRefusesZeroToken:令牌 0 是"没有租约"的形态,拿它做 CAS 证明不了"这一刻租约在我手里",而放弃不可逆。
// 必须在碰库之前拒掉(本用例没有连接池)。
func TestAbandonRefusesZeroToken(t *testing.T) {
	repo, err := NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	if err := repo.Abandon(context.Background(), assetop.Op{OpID: 1}, 1); err == nil {
		t.Fatal("令牌为 0 时必须拒绝放弃")
	}
}

// lockRowsFor 是录制驱动的结果集:sqlLockAssetOp(op 行主键点锁)按 present 回"有这一行 / 没有";其余查询回空。
func lockRowsFor(present bool) func(string, []driver.Value) ([]string, [][]driver.Value) {
	return func(query string, args []driver.Value) ([]string, [][]driver.Value) {
		if present && query == "SELECT ref_kind FROM "+AssetOpTableName+" WHERE op_id=?" {
			return []string{"ref_kind"}, [][]driver.Value{{int64(1)}}
		}
		if !present || query != collapseSpaces(sqlLockAssetOp) {
			return nil, nil
		}
		return []string{"op_id"}, [][]driver.Value{{args[0]}}
	}
}

// callSQLs 只取录音带上的语句文本,便于整段比对顺序。
func callSQLs(calls []recovCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.sql)
	}
	return out
}

// TestAbandonCommitsWithTokenAndReportsLeaseLost:先主键点锁 → RowsAffected==1 → 在一个事务里提交;
// RowsAffected==0 或点锁读不到行(已被清理)→ 必须回 ErrLeaseLost(租约被接管,别人可能已经发出请求),并回滚。
func TestAbandonCommitsWithTokenAndReportsLeaseLost(t *testing.T) {
	op := assetop.Op{OpID: 11, LeaseToken: 22, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT}

	t.Run("提交", func(t *testing.T) {
		s := &recovScript{affected: 1, rowsFor: lockRowsFor(true)}
		repo := s.repo(t)
		if err := repo.Abandon(context.Background(), op, 3_000); err != nil {
			t.Fatalf("Abandon: %v", err)
		}
		calls := s.recordedCalls()
		// COMMIT 之后在事务外读这条指令的归属,好唤醒所属合同(缺口清单第 16 条,wakeContractOf);
		// 录制驱动对这条读回空行,所以后面没有唤醒 UPDATE。
		want := []string{"BEGIN", collapseSpaces(sqlLockAssetOp), collapseSpaces(sqlAbandonAssetOp), "COMMIT", sqlReadAssetOpOwner}
		if !reflect.DeepEqual(callSQLs(calls), want) {
			t.Fatalf("放弃应是 BEGIN → 主键点锁 → CAS → COMMIT → 事务外读归属,录到 %+v", callSQLs(calls))
		}
		if lockArgs := calls[1].args; len(lockArgs) != 1 || argInt(t, lockArgs[0]) != op.OpID {
			t.Errorf("点锁实参 = %v, want (op_id=%d)", lockArgs, op.OpID)
		}
		args := calls[2].args
		// 实参顺序:status, next_attempt_ms, updated_ms, op_id, PENDING, token。
		if len(args) != 6 {
			t.Fatalf("放弃 CAS 实参数 = %d, want 6", len(args))
		}
		if got := argInt(t, args[0]); got != uint64(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED) {
			t.Errorf("写入状态 = %d, want ABANDONED", got)
		}
		if got := argInt(t, args[1]); got != 3_000 {
			t.Errorf("next_attempt_ms = %d, want 终结时刻 3000", got)
		}
		if argInt(t, args[3]) != op.OpID || argInt(t, args[4]) != uint64(PendingStatus()) || argInt(t, args[5]) != op.LeaseToken {
			t.Errorf("CAS 条件实参 = %v, want (op_id=%d, PENDING, token=%d)", args[3:], op.OpID, op.LeaseToken)
		}
	})

	t.Run("租约丢失", func(t *testing.T) {
		s := &recovScript{affected: 0, rowsFor: lockRowsFor(true)}
		repo := s.repo(t)
		err := repo.Abandon(context.Background(), op, 3_000)
		if !errors.Is(err, assetop.ErrLeaseLost) {
			t.Fatalf("RowsAffected==0 时 err = %v, want errors.Is(..., ErrLeaseLost)", err)
		}
		if len(s.callsWithPrefix("COMMIT")) != 0 || len(s.callsWithPrefix("ROLLBACK")) != 1 {
			t.Errorf("租约丢失时必须回滚、不得提交,录到 %+v", callSQLs(s.recordedCalls()))
		}
	})

	t.Run("行已被清理", func(t *testing.T) {
		s := &recovScript{affected: 1, rowsFor: lockRowsFor(false)}
		repo := s.repo(t)
		err := repo.Abandon(context.Background(), op, 3_000)
		if !errors.Is(err, assetop.ErrLeaseLost) {
			t.Fatalf("点锁读不到行时 err = %v, want errors.Is(..., ErrLeaseLost)", err)
		}
		want := []string{"BEGIN", collapseSpaces(sqlLockAssetOp), "ROLLBACK"}
		if got := callSQLs(s.recordedCalls()); !reflect.DeepEqual(got, want) {
			t.Errorf("行已不存在时不得发 CAS,录到 %+v, want %+v", got, want)
		}
	})
}

// TestLockAssetOpIsPrimaryKeyPointLock:op 行的点锁只许是完整主键等值 + FOR UPDATE、不带任何复核条件 ——
// TiDB 只有这种形状走 Point_Get 快路径("PRIMARY key → 行 key"两次独立加锁);带上 `status = ?` 之类的复核条件,
// 它就退化成语句末尾并行批锁的那一种,先点锁这一步也就失去了意义(与 guild 的同名语句逐字同形)。
func TestLockAssetOpIsPrimaryKeyPointLock(t *testing.T) {
	want := "SELECT `op_id` FROM " + AssetOpTableName + " WHERE `op_id` = ? FOR UPDATE"
	if got := collapseSpaces(sqlLockAssetOp); got != want {
		t.Errorf("sqlLockAssetOp = %q, want %q", got, want)
	}
}

// TestTerminalWritersLockRowBeforeCAS:终结 / 人工终结 / 放弃在同一个事务里**先**主键点锁、**再**带复核谓词的 CAS
// (asset_op_repo.go"op 行的写者:先主键点锁再 CAS")。点锁读不到行(已被清理)时一条 CAS 都不发:
// 终结与人工终结回 (false, nil)(与 CAS 落空同义,不做对侧账),放弃回 ErrLeaseLost。
func TestTerminalWritersLockRowBeforeCAS(t *testing.T) {
	const opID = 31
	debit := assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	cases := []struct {
		name string
		cas  string
		// run 执行被测写者,回报"是否本次终结"与错误。
		run func(repo *AssetOpRepo) (bool, error)
		// missingErr 是点锁读不到行时应满足的错误判定(nil = 应无错误)。
		missingErr error
	}{
		{
			name: "Finalize",
			cas:  sqlFinalizeAssetOp,
			run: func(repo *AssetOpRepo) (bool, error) {
				return repo.Finalize(context.Background(), assetop.Op{OpID: opID, LeaseToken: 5, Stream: debit},
					assetop.StatusApplied, assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}, 9_000)
			},
		},
		{
			name: "ResolveManually",
			cas:  sqlResolveAssetOp,
			run: func(repo *AssetOpRepo) (bool, error) {
				return repo.ResolveManually(context.Background(),
					assetop.ManualResolution{OpID: opID, Final: assetop.StatusAborted, Operator: "ops", Reason: "test"}, 9_000)
			},
		},
		{
			name: "Abandon",
			cas:  sqlAbandonAssetOp,
			run: func(repo *AssetOpRepo) (bool, error) {
				err := repo.Abandon(context.Background(), assetop.Op{OpID: opID, LeaseToken: 5, Stream: debit}, 9_000)
				return err == nil, err
			},
			missingErr: assetop.ErrLeaseLost,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/行在", func(t *testing.T) {
			s := &recovScript{affected: 1, rowsFor: lockRowsFor(true)}
			done, err := tc.run(s.repo(t))
			if err != nil || !done {
				t.Fatalf("= (%v, %v), want (true, nil)", done, err)
			}
			calls := s.recordedCalls()
			// 三个写者终结成功之后都在事务外读一次归属,好唤醒所属合同(缺口清单第 16 条,wakeContractOf);
			// 录制驱动对这条读回空行,所以后面没有唤醒 UPDATE。唤醒本身由 TestTerminalWritersWakeOwningContractAfterCommit 钉住。
			want := []string{"BEGIN", collapseSpaces(sqlLockAssetOp), collapseSpaces(tc.cas), "COMMIT", sqlReadAssetOpOwner}
			if tc.name == "Finalize" {
				want = []string{"BEGIN", collapseSpaces(sqlLockAssetOp), "SELECT ref_kind FROM " + AssetOpTableName + " WHERE op_id=?", collapseSpaces(tc.cas), "COMMIT", sqlReadAssetOpOwner}
			}
			if got := callSQLs(calls); !reflect.DeepEqual(got, want) {
				t.Fatalf("语句序列 = %+v\nwant %+v", got, want)
			}
			if lockArgs := calls[1].args; len(lockArgs) != 1 || argInt(t, lockArgs[0]) != opID {
				t.Errorf("点锁实参 = %v, want (op_id=%d)", lockArgs, opID)
			}
		})
		t.Run(tc.name+"/行已被清理", func(t *testing.T) {
			s := &recovScript{affected: 1, rowsFor: lockRowsFor(false)}
			done, err := tc.run(s.repo(t))
			if done {
				t.Fatal("点锁读不到行时不得报告本次终结")
			}
			if tc.missingErr == nil && err != nil {
				t.Fatalf("err = %v, want nil(与 CAS 落空同义)", err)
			}
			if tc.missingErr != nil && !errors.Is(err, tc.missingErr) {
				t.Fatalf("err = %v, want errors.Is(..., %v)", err, tc.missingErr)
			}
			for _, c := range s.recordedCalls() {
				if strings.HasPrefix(c.sql, "UPDATE ") {
					t.Errorf("点锁读不到行时不得发 CAS,录到 %s", c.sql)
				}
			}
		})
	}
}

// TestTerminalWritersWakeOwningContractAfterCommit:终结 / 人工终结 / 放弃提交之后,指令属于交易合同(ref_kind=ORDER)时,
// 在事务外把那张合同提前到终结时刻到期(缺口清单第 16 条):先读归属,再发一条只往前拉、不改版本的唤醒 UPDATE。
// 唤醒排在 COMMIT 之后:合同推进的锁序是「合同行 → op 行」,在 op 行锁里再锁合同行会成环。
func TestTerminalWritersWakeOwningContractAfterCommit(t *testing.T) {
	const opID, contractID, nowMs = 41, 77, 9_000
	debit := assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	owner := func(query string, args []driver.Value) ([]string, [][]driver.Value) {
		switch query {
		case collapseSpaces(sqlLockAssetOp):
			return []string{"op_id"}, [][]driver.Value{{args[0]}}
		case "SELECT ref_kind FROM " + AssetOpTableName + " WHERE op_id=?":
			return []string{"ref_kind"}, [][]driver.Value{{int64(tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_ORDER)}}
		case sqlReadAssetOpOwner:
			return []string{"ref_kind", "ref_id"}, [][]driver.Value{{int64(tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_ORDER), int64(contractID)}}
		}
		return nil, nil
	}
	writers := map[string]func(repo *AssetOpRepo) error{
		"Finalize": func(repo *AssetOpRepo) error {
			// 合同的腿(ORDER)终结要带玩家 SQL 已提交且已落盘的证据,否则 Finalize 拒绝。
			_, err := repo.Finalize(context.Background(), assetop.Op{OpID: opID, LeaseToken: 5, Stream: debit},
				assetop.StatusApplied, assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true, SQLCommitted: true}, nowMs)
			return err
		},
		"ResolveManually": func(repo *AssetOpRepo) error {
			_, err := repo.ResolveManually(context.Background(),
				assetop.ManualResolution{OpID: opID, Final: assetop.StatusAborted, Operator: "ops", Reason: "test"}, nowMs)
			return err
		},
		"Abandon": func(repo *AssetOpRepo) error {
			return repo.Abandon(context.Background(), assetop.Op{OpID: opID, LeaseToken: 5, Stream: debit}, nowMs)
		},
	}
	for name, run := range writers {
		t.Run(name, func(t *testing.T) {
			s := &recovScript{affected: 1, rowsFor: owner}
			if err := run(s.repo(t)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			calls := s.recordedCalls()
			commit := -1
			for i, c := range calls {
				if c.sql == "COMMIT" {
					commit = i
				}
			}
			if commit < 0 || len(calls) != commit+3 {
				t.Fatalf("COMMIT 之后应恰好是读归属、唤醒两条,录到 %+v", callSQLs(calls))
			}
			if calls[commit+1].sql != sqlReadAssetOpOwner || argInt(t, calls[commit+1].args[0]) != opID {
				t.Fatalf("COMMIT 之后先读归属 (op_id=%d),录到 %s %v", opID, calls[commit+1].sql, calls[commit+1].args)
			}
			wake := calls[commit+2]
			if !strings.HasPrefix(wake.sql, "UPDATE trade_commerce SET updated_ms=? WHERE commerce_id=? AND updated_ms>?") {
				t.Fatalf("再唤醒所属合同,录到 %s", wake.sql)
			}
			if len(wake.args) != 3 || argInt(t, wake.args[0]) != nowMs || argInt(t, wake.args[1]) != contractID || argInt(t, wake.args[2]) != nowMs {
				t.Fatalf("唤醒实参 = %v, want (updated_ms=%d, commerce_id=%d, 只往前拉 %d)", wake.args, nowMs, contractID, nowMs)
			}
		})
	}
}

// TestAbandonOnlyTouchesDebitStream:trade 的对侧账没有实现(P3),放弃 DELIVER_CREDIT 会让买家钱货两空。
// 自动放弃只碰 TRADE_DEBIT:候选读按流过滤、绑定的流就是 TRADE_DEBIT;Abandon 对非 DEBIT 的行在碰库之前拒绝,
// 且不回 ErrLeaseLost(那是"租约被接管",这里是调用方越界)。对侧账落地、放开 CREDIT 时同改本用例。
func TestAbandonOnlyTouchesDebitStream(t *testing.T) {
	if abandonableStream != assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT {
		t.Fatalf("abandonableStream = %v:对侧账落地之前只许放弃 TRADE_DEBIT", abandonableStream)
	}
	if !strings.Contains(collapseSpaces(sqlListAbandonCandidates), "`stream` = ?") {
		t.Fatalf("放弃候选读必须按流过滤: %s", sqlListAbandonCandidates)
	}

	s := &recovScript{}
	repo := s.repo(t)
	if _, err := repo.ListAbandonCandidates(context.Background(), 100, 0, 5); err != nil {
		t.Fatalf("ListAbandonCandidates: %v", err)
	}
	calls := s.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("候选读应恰一条,录到 %+v", calls)
	}
	if got := argInt(t, calls[0].args[1]); got != uint64(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT) {
		t.Errorf("候选读绑定的流 = %d, want TRADE_DEBIT(%d)", got, assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT)
	}

	s = &recovScript{affected: 1, rowsFor: lockRowsFor(true)}
	repo = s.repo(t)
	credit := assetop.Op{OpID: 12, LeaseToken: 22, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT}
	err := repo.Abandon(context.Background(), credit, 3_000)
	if err == nil {
		t.Fatal("非 TRADE_DEBIT 的行必须拒绝放弃")
	}
	if errors.Is(err, assetop.ErrLeaseLost) {
		t.Errorf("err = %v:流越界不是租约被接管,不得回 ErrLeaseLost", err)
	}
	if n := len(s.recordedCalls()); n != 0 {
		t.Errorf("流越界必须在碰库之前拒绝,录到 %d 条: %+v", n, callSQLs(s.recordedCalls()))
	}
}

// TestReleaseLeaseReportsLeaseLost:归还租约落空(行已被接管或已终结)必须回 ErrLeaseLost,成功回 nil。
func TestReleaseLeaseReportsLeaseLost(t *testing.T) {
	op := assetop.Op{OpID: 3, LeaseToken: 4}
	for _, tc := range []struct {
		affected int64
		wantLost bool
	}{{1, false}, {0, true}} {
		s := &recovScript{affected: tc.affected}
		repo := s.repo(t)
		err := repo.ReleaseLease(context.Background(), op, 1)
		if got := errors.Is(err, assetop.ErrLeaseLost); got != tc.wantLost {
			t.Errorf("affected=%d: err = %v, want ErrLeaseLost=%v", tc.affected, err, tc.wantLost)
		}
		if !tc.wantLost && err != nil {
			t.Errorf("affected=1: err = %v, want nil", err)
		}
		calls := s.callsWithPrefix("UPDATE " + AssetOpTableName + " SET `lease_until_ms` = 0")
		if len(calls) != 1 {
			t.Fatalf("应发出恰一条归还语句,录到 %+v", s.recordedCalls())
		}
		// 实参顺序:updated_ms, op_id, PENDING, token。
		if args := calls[0].args; argInt(t, args[1]) != op.OpID || argInt(t, args[3]) != op.LeaseToken {
			t.Errorf("归还条件实参 = %v, want op_id=%d token=%d", args, op.OpID, op.LeaseToken)
		}
	}
}

// TestListAbandonCandidatesArgs:候选读是非锁读、按 op_id 翻页;limit ≤ 0 不发 SQL。
func TestListAbandonCandidatesArgs(t *testing.T) {
	s := &recovScript{}
	s.rowsFor = func(string, []driver.Value) ([]string, [][]driver.Value) {
		return []string{"op_id"}, [][]driver.Value{{int64(8)}, {int64(9)}}
	}
	repo := s.repo(t)
	if ids, err := repo.ListAbandonCandidates(context.Background(), 100, 7, 0); err != nil || ids != nil {
		t.Errorf("limit=0 = (%v, %v), want (nil, nil)", ids, err)
	}
	if n := len(s.recordedCalls()); n != 0 {
		t.Fatalf("limit=0 不应发 SQL,录到 %d 条", n)
	}
	ids, err := repo.ListAbandonCandidates(context.Background(), 100, 7, 2)
	if err != nil {
		t.Fatalf("ListAbandonCandidates: %v", err)
	}
	if !reflect.DeepEqual(ids, []uint64{8, 9}) {
		t.Errorf("ids = %v, want [8 9]", ids)
	}
	calls := s.recordedCalls()
	if len(calls) != 1 || strings.Contains(calls[0].sql, "FOR UPDATE") {
		t.Fatalf("候选读必须是一条非锁读,录到 %+v", calls)
	}
	// 实参顺序:PENDING, stream(TRADE_DEBIT), createdBefore, afterOpID, limit。
	args := calls[0].args
	if len(args) != 5 {
		t.Fatalf("候选读实参数 = %d, want 5", len(args))
	}
	if argInt(t, args[0]) != uint64(PendingStatus()) ||
		argInt(t, args[1]) != uint64(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT) ||
		argInt(t, args[2]) != 100 || argInt(t, args[3]) != 7 || argInt(t, args[4]) != 2 {
		t.Errorf("候选读实参 = %v, want (PENDING, TRADE_DEBIT, 100, 7, 2)", args)
	}
}

// ── PlayerPendingLister ─────────────────────────────────────

// TestListPendingByPlayer:落地唤醒按玩家列 PENDING 行,非锁读,**不**看 next_attempt_ms / lease_until_ms
// (唤醒就是要提前,租约由随后的 Claim CAS 判);limit ≤ 0 不发 SQL。
func TestListPendingByPlayer(t *testing.T) {
	stmt := collapseSpaces(sqlListPendingByPlayer)
	for _, banned := range []string{"next_attempt_ms", "lease_until_ms", "FOR UPDATE"} {
		if strings.Contains(stmt, banned) {
			t.Errorf("按玩家列未决行不得带 %s: %s", banned, stmt)
		}
	}

	s := &recovScript{}
	s.rowsFor = func(string, []driver.Value) ([]string, [][]driver.Value) {
		return []string{"op_id"}, [][]driver.Value{{int64(31)}, {int64(30)}}
	}
	repo := s.repo(t)
	if ids, err := repo.ListPendingByPlayer(context.Background(), 42, 0); err != nil || ids != nil {
		t.Errorf("limit=0 = (%v, %v), want (nil, nil)", ids, err)
	}
	if n := len(s.recordedCalls()); n != 0 {
		t.Fatalf("limit=0 不应发 SQL,录到 %d 条", n)
	}
	ids, err := repo.ListPendingByPlayer(context.Background(), 42, 32)
	if err != nil {
		t.Fatalf("ListPendingByPlayer: %v", err)
	}
	if !reflect.DeepEqual(ids, []uint64{31, 30}) {
		t.Errorf("ids = %v, want 按查询顺序原样返回 [31 30]", ids)
	}
	args := s.recordedCalls()[0].args
	// 实参顺序:player_id, PENDING, limit。
	if argInt(t, args[0]) != 42 || argInt(t, args[1]) != uint64(PendingStatus()) || argInt(t, args[2]) != 32 {
		t.Errorf("实参 = %v, want (42, PENDING, 32)", args)
	}
}

// ── PendingClassCounter ─────────────────────────────────────

// TestCountPendingByClassPagesAndUsesSharedClassifier:日对账按主键分页读全部 PENDING 行(满页才读下一页,
// 下一页从上一页最后一个 op_id 之后接着读),分类口径只用 assetop.PendingTally —— 期望值由同一个累加器算出,
// 这里只证明"每一行都喂进去了、一行不多一行不少",不在测试里另抄一份分类规则。
func TestCountPendingByClassPagesAndUsesSharedClassifier(t *testing.T) {
	const nowMs, abandonBeforeMs = 10_000_000, 5_000_000
	// 第一页恰满 auditPageSize 行,第二页 3 行:应恰好读两页。
	var facts []assetop.PendingFacts
	var pages [][][]driver.Value
	var page [][]driver.Value
	outcomes := []assetpb.AssetOpOutcome{
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN,
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE,
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY,
	}
	streams := []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT,
	}
	total := auditPageSize + 3
	for i := 0; i < total; i++ {
		f := assetop.PendingFacts{
			Stream:      streams[i%len(streams)],
			CreatedMs:   uint64(1_000_000 * (i%10 + 1)),
			Attempts:    uint32(i % 4),
			LastOutcome: outcomes[i%len(outcomes)],
			LastReason:  0,
		}
		facts = append(facts, f)
		page = append(page, []driver.Value{
			int64(i + 1), int64(f.Stream), int64(f.CreatedMs), int64(f.Attempts), int64(f.LastOutcome), int64(f.LastReason),
		})
		if len(page) == auditPageSize {
			pages = append(pages, page)
			page = nil
		}
	}
	pages = append(pages, page)

	s := &recovScript{}
	s.rowsFor = func(_ string, args []driver.Value) ([]string, [][]driver.Value) {
		cols := []string{"op_id", "stream", "created_ms", "attempts", "last_outcome", "last_reason"}
		after := args[1].(int64)
		switch after {
		case 0:
			return cols, pages[0]
		case int64(auditPageSize):
			return cols, pages[1]
		default:
			return cols, nil
		}
	}
	repo := s.repo(t)

	got, err := repo.CountPendingByClass(context.Background(), nowMs, abandonBeforeMs)
	if err != nil {
		t.Fatalf("CountPendingByClass: %v", err)
	}
	want := assetop.NewPendingTally(nowMs, abandonBeforeMs)
	for _, f := range facts {
		want.Add(f)
	}
	if !reflect.DeepEqual(got, want.Counts()) {
		t.Errorf("计数 = %+v\nwant  %+v", got, want.Counts())
	}

	calls := s.recordedCalls()
	if len(calls) != 2 {
		t.Fatalf("应恰好读两页,录到 %d 条: %+v", len(calls), calls)
	}
	for i, c := range calls {
		if strings.Contains(c.sql, "FOR UPDATE") {
			t.Errorf("第 %d 页是锁定读: %s", i+1, c.sql)
		}
		// 实参顺序:PENDING, afterOpID, pageSize。
		if argInt(t, c.args[0]) != uint64(PendingStatus()) || argInt(t, c.args[2]) != auditPageSize {
			t.Errorf("第 %d 页实参 = %v", i+1, c.args)
		}
	}
	if argInt(t, calls[1].args[1]) != uint64(auditPageSize) {
		t.Errorf("第二页应从第一页最后一个 op_id(%d)之后接着读,得到 %v", auditPageSize, calls[1].args[1])
	}
}

// ── 终态行清理 ──────────────────────────────────────────────

// TestCleanupStatusesTreatAbandonedAsTerminal:ABANDONED 与其它终态一样过保留期就删(设计 §6.3「状态与展示」);
// PENDING 永不删,APPLIED_PARTIAL 要人工补偿,证据不能自动消失。
func TestCleanupStatusesTreatAbandonedAsTerminal(t *testing.T) {
	got := map[int32]bool{}
	for _, st := range cleanupStatuses() {
		got[st] = true
	}
	for _, must := range []tradepb.TradeAssetOpStatus{
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED,
	} {
		if !got[int32(must)] {
			t.Errorf("清理口径漏了终态 %v", must)
		}
	}
	for _, never := range []tradepb.TradeAssetOpStatus{
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED,
	} {
		if got[int32(never)] {
			t.Errorf("清理口径不得包含 %v", never)
		}
	}
}

// TestRunCleanupRefusesInvalidConf:间隔或保留期非正时不启动、立刻返回(不碰库;本用例没有连接池)。
// 保留期 0 等于"删光所有终态行",不可能是有意的配置。
func TestRunCleanupRefusesInvalidConf(t *testing.T) {
	repo, err := NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	for _, c := range []CleanupConf{
		{Interval: 0, TerminalRetention: 30 * 24 * time.Hour},
		{Interval: time.Minute, TerminalRetention: 0},
	} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			repo.RunCleanup(context.Background(), c)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("非法配置 %+v 下 RunCleanup 没有立刻返回", c)
		}
	}
}

// TestRunCleanupReturnsOnCancel:ctx 取消即返回(svc 的 stop 会等它退出,再去关库)。
// 间隔取 1 小时,随机初始延迟在本用例里不会到点,于是不会碰到没有连接池的仓库。
func TestRunCleanupReturnsOnCancel(t *testing.T) {
	repo, err := NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		repo.RunCleanup(ctx, CleanupConf{Interval: time.Hour, TerminalRetention: 30 * 24 * time.Hour})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 RunCleanup 没有返回")
	}
}

// ── 终态行清理的语句序列 ──────────────────────────────────────

// cleanupTestNow / cleanupTestRetention 是清理用例的固定时刻与保留期;截止 = now − retention(毫秒)。
var (
	cleanupTestNow       = time.UnixMilli(1_800_000_000_000)
	cleanupTestRetention = 30 * 24 * time.Hour
	cleanupTestCutoffMs  = uint64(cleanupTestNow.UnixMilli() - cleanupTestRetention.Milliseconds())
)

// captureCleanupDeleted 把 recordCleanupDeleted 换成记录器(用例结束时还原),断言不去读全局注册表。
func captureCleanupDeleted(t *testing.T) func() []int64 {
	t.Helper()
	var got []int64
	prev := recordCleanupDeleted
	recordCleanupDeleted = func(n int64) { got = append(got, n) }
	t.Cleanup(func() { recordCleanupDeleted = prev })
	return func() []int64 { return got }
}

// cleanupRowsFor 是清理用例的结果集:候选读按调用次数依次回 batches[i](超出回空),点锁按 lockPresent 回"有 / 没有"。
// reads 记候选读次数 —— 清理在同一个 goroutine 里串行发语句,不需要加锁。
func cleanupRowsFor(batches [][]uint64, lockPresent bool, reads *int) func(string, []driver.Value) ([]string, [][]driver.Value) {
	return func(query string, args []driver.Value) ([]string, [][]driver.Value) {
		switch query {
		case collapseSpaces(sqlListCleanupTerminalOps):
			i := *reads
			*reads++
			if i >= len(batches) {
				return []string{"op_id"}, nil
			}
			data := make([][]driver.Value, 0, len(batches[i]))
			for _, id := range batches[i] {
				data = append(data, []driver.Value{int64(id)})
			}
			return []string{"op_id"}, data
		case collapseSpaces(sqlLockAssetOp):
			if !lockPresent {
				return []string{"op_id"}, nil
			}
			return []string{"op_id"}, [][]driver.Value{{args[0]}}
		}
		return nil, nil
	}
}

// TestCleanupTerminalOpsStatementOrder:每个候选一个 RC 短事务,事务里**先**主键点锁(不带复核条件)、**再**带复核条件的主键点删
// (BEGIN → SELECT … FOR UPDATE → DELETE → COMMIT,见 asset_op_repo.go"op 行的写者:先主键点锁再 CAS");
// 候选读与点删的 IN (?, ?, ?, ?) 都按 cleanupStatuses 的顺序绑四个终态,截止时刻 = now − 保留期;
// 候选不足一批(cleanupBatchSize)时只读这一批就收工。
func TestCleanupTerminalOpsStatementOrder(t *testing.T) {
	const opID = 4242
	reads := 0
	s := &recovScript{affected: 1, rowsFor: cleanupRowsFor([][]uint64{{opID}}, true, &reads)}
	deletedRec := captureCleanupDeleted(t)
	repo := s.repo(t)

	n, err := repo.CleanupTerminalOps(context.Background(), cleanupTestNow, cleanupTestRetention)
	if err != nil || n != 1 {
		t.Fatalf("CleanupTerminalOps = (%d, %v), want (1, nil)", n, err)
	}
	if !reflect.DeepEqual(deletedRec(), []int64{1}) {
		t.Errorf("清理计数 = %v, want [1]", deletedRec())
	}
	if reads != 1 {
		t.Errorf("候选不足一批时应只读一批,读了 %d 次", reads)
	}

	calls := s.recordedCalls()
	want := []string{
		collapseSpaces(sqlListCleanupTerminalOps),
		"BEGIN", collapseSpaces(sqlLockAssetOp), collapseSpaces(sqlCleanupTerminalOp), "COMMIT",
	}
	if got := callSQLs(calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("语句序列 = %+v\nwant %+v", got, want)
	}

	st := cleanupStatuses()
	// 候选读:四个终态, cutoff, 批大小。
	listArgs := calls[0].args
	if len(listArgs) != 6 {
		t.Fatalf("候选读实参数 = %d, want 6", len(listArgs))
	}
	for i := 0; i < 4; i++ {
		if argInt(t, listArgs[i]) != uint64(st[i]) {
			t.Errorf("候选读第 %d 个实参 = %v, want 终态 %d", i+1, listArgs[i], st[i])
		}
	}
	if argInt(t, listArgs[4]) != cleanupTestCutoffMs || argInt(t, listArgs[5]) != cleanupBatchSize {
		t.Errorf("候选读 (cutoff, limit) = (%v, %v), want (%d, %d)", listArgs[4], listArgs[5], cleanupTestCutoffMs, cleanupBatchSize)
	}
	// 点锁:只有完整主键。
	if lockArgs := calls[2].args; len(lockArgs) != 1 || argInt(t, lockArgs[0]) != opID {
		t.Errorf("点锁实参 = %v, want (op_id=%d)", lockArgs, opID)
	}
	// 点删:op_id, 四个终态, cutoff。
	delArgs := calls[3].args
	if len(delArgs) != 6 || argInt(t, delArgs[0]) != opID || argInt(t, delArgs[5]) != cleanupTestCutoffMs {
		t.Fatalf("点删实参 = %v, want (op_id=%d, 四个终态, cutoff=%d)", delArgs, opID, cleanupTestCutoffMs)
	}
	for i := 0; i < 4; i++ {
		if argInt(t, delArgs[i+1]) != uint64(st[i]) {
			t.Errorf("点删第 %d 个实参 = %v, want 终态 %d", i+2, delArgs[i+1], st[i])
		}
	}
}

// TestCleanupTerminalOpsRollsBackMultiRowDelete:点删影响超过 1 行只可能是 WHERE 写坏了(不再是完整主键等值),
// 必须整体回滚并报错,删除数不计入(那一行事务已回滚,什么都没删)。
func TestCleanupTerminalOpsRollsBackMultiRowDelete(t *testing.T) {
	reads := 0
	s := &recovScript{affected: 2, rowsFor: cleanupRowsFor([][]uint64{{77}}, true, &reads)}
	deletedRec := captureCleanupDeleted(t)
	repo := s.repo(t)

	n, err := repo.CleanupTerminalOps(context.Background(), cleanupTestNow, cleanupTestRetention)
	if err == nil {
		t.Fatal("点删影响 2 行必须报错")
	}
	if n != 0 || len(deletedRec()) != 0 {
		t.Errorf("回滚的事务不得计删除:n=%d 计数=%v", n, deletedRec())
	}
	want := []string{
		collapseSpaces(sqlListCleanupTerminalOps),
		"BEGIN", collapseSpaces(sqlLockAssetOp), collapseSpaces(sqlCleanupTerminalOp), "ROLLBACK",
	}
	if got := callSQLs(s.recordedCalls()); !reflect.DeepEqual(got, want) {
		t.Errorf("语句序列 = %+v\nwant %+v", got, want)
	}
}

// TestCleanupTerminalOpsContinuesWhileBatchIsFull:"还有没有下一批"看候选数,不看删除数。
// 一整批(cleanupBatchSize 个)候选全部复核落空(点锁读不到 = 已被别的副本删掉)时删除数为 0,
// 但候选满批说明后面可能还有,必须再读一批;第二批不足一批才收工。按删除数判会在这里提前收工。
func TestCleanupTerminalOpsContinuesWhileBatchIsFull(t *testing.T) {
	full := make([]uint64, 0, cleanupBatchSize)
	for i := 1; i <= cleanupBatchSize; i++ {
		full = append(full, uint64(i))
	}
	reads := 0
	s := &recovScript{affected: 1, rowsFor: cleanupRowsFor([][]uint64{full}, false, &reads)}
	captureCleanupDeleted(t)
	repo := s.repo(t)

	n, err := repo.CleanupTerminalOps(context.Background(), cleanupTestNow, cleanupTestRetention)
	if err != nil || n != 0 {
		t.Fatalf("CleanupTerminalOps = (%d, %v), want (0, nil)", n, err)
	}
	if reads != 2 {
		t.Errorf("满批之后应再读一批,共读 %d 次, want 2", reads)
	}
	if got := len(s.callsWithPrefix("DELETE ")); got != 0 {
		t.Errorf("点锁读不到行时不得发点删,录到 %d 条", got)
	}
	if got := len(s.callsWithPrefix(collapseSpaces(sqlLockAssetOp))); got != cleanupBatchSize {
		t.Errorf("每个候选各点锁一次:录到 %d 次, want %d", got, cleanupBatchSize)
	}
}
