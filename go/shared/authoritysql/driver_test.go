package authoritysql

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	pb "proto/common/database"
	"reflect"
	"testing"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

// 所有用例均为未运行源码。fake只替代驱动接缝，不伪造生产catalog实现。
type writerFakeWire struct {
	events                                                                             []string
	execs, queries, begins, commits, rollbacks, closes, prepares                       int
	execErr, queryErr, commitErr, rollbackErr, rowsCloseErr, rowsNextErr, stmtCloseErr error
	sets                                                                               int
	lastArgs                                                                           []driver.NamedValue
	onCommit                                                                           func()
	onExec                                                                             func()
}

func (w *writerFakeWire) record(s string) { w.events = append(w.events, s) }
func (w *writerFakeWire) Prepare(q string) (driver.Stmt, error) {
	return w.PrepareContext(context.Background(), q)
}
func (w *writerFakeWire) PrepareContext(context.Context, string) (driver.Stmt, error) {
	w.prepares++
	w.record("prepare")
	return &writerFakeStmt{wire: w}, nil
}
func (w *writerFakeWire) Close() error { w.closes++; w.record("conn-close"); return nil }
func (w *writerFakeWire) Begin() (driver.Tx, error) {
	return w.BeginTx(context.Background(), driver.TxOptions{})
}
func (w *writerFakeWire) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	w.begins++
	w.record("begin")
	return &writerFakeTx{wire: w}, nil
}
func (w *writerFakeWire) ExecContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Result, error) {
	w.execs++
	w.record("exec")
	w.lastArgs = append([]driver.NamedValue(nil), args...)
	if w.onExec != nil {
		w.onExec()
	}
	return driver.RowsAffected(1), w.execErr
}
func (w *writerFakeWire) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	w.queries++
	w.record("query")
	w.lastArgs = append([]driver.NamedValue(nil), args...)
	if w.queryErr != nil {
		return nil, w.queryErr
	}
	sets := w.sets
	if sets == 0 {
		sets = 1
	}
	return &writerFakeRows{wire: w, count: sets}, nil
}

type writerFakeTx struct{ wire *writerFakeWire }

func (tx *writerFakeTx) Commit() error {
	tx.wire.commits++
	tx.wire.record("commit")
	if tx.wire.onCommit != nil {
		tx.wire.onCommit()
	}
	return tx.wire.commitErr
}
func (tx *writerFakeTx) Rollback() error {
	tx.wire.rollbacks++
	tx.wire.record("rollback")
	return tx.wire.rollbackErr
}

type writerFakeStmt struct{ wire *writerFakeWire }

func (s *writerFakeStmt) Close() error { s.wire.record("stmt-close"); return s.wire.stmtCloseErr }
func (*writerFakeStmt) NumInput() int  { return -1 }
func (s *writerFakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}
func (s *writerFakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}
func (s *writerFakeStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.wire.ExecContext(ctx, "UPDATE t SET n=n+1", args)
}
func (s *writerFakeStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.wire.QueryContext(ctx, "SELECT n FROM t", args)
}

type writerFakeRows struct {
	wire         *writerFakeWire
	count, at    int
	read, closed bool
}

func (*writerFakeRows) Columns() []string { return []string{"n"} }
func (r *writerFakeRows) Close() error {
	if !r.closed {
		r.closed = true
		r.wire.record("rows-close")
	}
	return r.wire.rowsCloseErr
}
func (r *writerFakeRows) Next(values []driver.Value) error {
	if r.wire.rowsNextErr != nil {
		return r.wire.rowsNextErr
	}
	if r.read {
		return io.EOF
	}
	r.read = true
	values[0] = int64(r.at + 1)
	return nil
}
func (r *writerFakeRows) HasNextResultSet() bool { return r.at+1 < r.count }
func (r *writerFakeRows) NextResultSet() error {
	if !r.HasNextResultSet() {
		return io.EOF
	}
	r.at++
	r.read = false
	return nil
}

type writerFakeGate struct {
	wire                *writerFakeWire
	beforeErr, enterErr error
}

func (g *writerFakeGate) beforeBegin(context.Context, driver.Conn) (gateAttempt, error) {
	g.wire.record("pre-BEGIN")
	if g.beforeErr != nil {
		return nil, g.beforeErr
	}
	return g, nil
}
func (g *writerFakeGate) enter(context.Context, driver.Conn) error {
	g.wire.record("slot-current-ACTIVE")
	return g.enterErr
}

type writerFakeSession struct{ wire *writerFakeWire }

func (p writerFakeSession) pin(context.Context, driver.Conn) error { p.wire.record("pin"); return nil }

type writerFakeConnector struct {
	wire     *writerFakeWire
	connects int
}

func (c *writerFakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects++
	return c.wire, nil
}
func (*writerFakeConnector) Driver() driver.Driver { return opaqueDriver{} }
func writerFixture(wire *writerFakeWire) *guardedConn {
	connector, err := newGuardedConnector(&writerFakeConnector{wire: wire}, &writerFakeGate{wire: wire}, writerFakeSession{wire: wire}, mysqlStatementPolicy{}, writerUnitPermission())
	if err != nil {
		panic(err)
	}
	conn, err := connector.Connect(context.Background())
	if err != nil {
		panic(err)
	}
	// 建连已走同一生产构造；行为断言从本次业务开始，不混入准备期pin事件。
	wire.events = nil
	return conn.(*guardedConn)
}

func writerUnitPermission() Permission {
	return Writer(WriterConfig{boot: &writerBoot{grant: &pb.SqlWriterBootGrant{
		Format: "SQL_WRITER_BOOT_V1", Permission: "NORMAL", Generation: "0123456789abcdef0123456789abcdef",
		WriterKey: bytes.Repeat([]byte{1}, 16), InstanceId: bytes.Repeat([]byte{2}, 16),
		GrantId: bytes.Repeat([]byte{3}, 16), RegistrationDigest: bytes.Repeat([]byte{4}, 32),
		AuthoritySchema: "authority_test", ProtectedSchemas: []string{"authority_test", "game_test"},
		SqlPrincipal: []byte("writer_test@%"), DeploymentId: "unit-boot", GrantedMs: 1,
		PhysicalAuthoritySha256: bytes.Repeat([]byte{5}, 32), LockPrimitive: pb.SqlWriterBootGrant_SELECT_ONLY,
		LockQualificationSha256: bytes.Repeat([]byte{6}, 32),
	}}})
}

func TestWriterDriverZeroPermissionNeverDelegates(t *testing.T) {
	wire := &writerFakeWire{}
	inner := &writerFakeConnector{wire: wire}
	conn, err := newGuardedConnector(inner, &writerFakeGate{wire: wire}, writerFakeSession{wire: wire}, mysqlStatementPolicy{}, Permission{})
	if err == nil || conn != nil || inner.connects != 0 || len(wire.events) != 0 {
		t.Fatalf("零权限发生委托: conn=%v err=%v connects=%d events=%v", conn, err, inner.connects, wire.events)
	}
	for _, query := range []string{"SELECT 1", "UPDATE t SET n=1"} {
		if err := validatePermissionStatement(query, permissionInvalid); err == nil {
			t.Fatalf("零权限放行SQL: %s", query)
		}
	}
}

func TestWriterDriverAutoExecHasOneGuardedTransaction(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	result, err := c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("affected=%d err=%v", affected, err)
	}
	want := []string{"pre-BEGIN", "begin", "slot-current-ACTIVE", "exec", "commit"}
	if !reflect.DeepEqual(w.events, want) {
		t.Fatalf("步骤=%v", w.events)
	}
	if !c.IsValid() {
		t.Fatal("确定提交后可复用")
	}
}

func TestWriterDriverDatabaseSQLDoesNotReplayAfterSend(t *testing.T) {
	for _, phase := range []string{"exec", "commit"} {
		t.Run(phase, func(t *testing.T) {
			w := &writerFakeWire{}
			if phase == "exec" {
				w.execErr = errors.Join(errors.New("transport"), driver.ErrBadConn)
			} else {
				w.commitErr = driver.ErrBadConn
			}
			inner := &writerFakeConnector{wire: w}
			connector, err := newGuardedConnector(inner, &writerFakeGate{wire: w}, writerFakeSession{wire: w}, mysqlStatementPolicy{}, writerUnitPermission())
			if err != nil {
				t.Fatal(err)
			}
			db := sql.OpenDB(connector)
			defer db.Close()
			_, err = db.ExecContext(context.Background(), "UPDATE t SET n=n+1")
			if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) {
				t.Fatalf("必须opaque UNKNOWN: %v", err)
			}
			if w.execs != 1 || inner.connects != 1 {
				t.Fatalf("发生重放 exec=%d connect=%d", w.execs, inner.connects)
			}
		})
	}
}

func TestWriterDriverExplicit1062KeepsOriginalTransaction(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil); err != nil {
		t.Fatal(err)
	}
	duplicate := &mysql.MySQLError{Number: 1062, Message: "fixture duplicate"}
	w.execErr = duplicate
	if _, err = c.ExecContext(context.Background(), "INSERT INTO t VALUES (1)", nil); err != duplicate {
		t.Fatalf("应保确定1062: %v", err)
	}
	if w.rollbacks != 0 {
		t.Fatal("1062不得毁掉原Tx或slot")
	}
	w.execErr = nil
	rows, err := c.QueryContext(context.Background(), "SELECT n FROM t FOR UPDATE", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if w.commits != 0 || w.begins != 1 {
		t.Fatal("继续转读必须保原Tx")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if w.commits != 1 || w.execs != 2 {
		t.Fatalf("commit=%d exec=%d", w.commits, w.execs)
	}
}

type writerFakeOutcome struct{ outcome serverOutcome }

func (p writerFakeOutcome) classify(error) serverOutcome { return p.outcome }
func TestWriterDriverOutcomePolicyBranches(t *testing.T) {
	for _, outcome := range []serverOutcome{outcomeStatementRejected, outcomeTransactionAborted, outcomeUnknown} {
		w := &writerFakeWire{execErr: errors.New("fixture server outcome")}
		c := writerFixture(w)
		c.outcomes = writerFakeOutcome{outcome: outcome}
		tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
		if outcome == outcomeStatementRejected {
			if err != w.execErr || w.rollbacks != 0 {
				t.Fatal("确定语句错误应保Tx")
			}
			_ = tx.Rollback()
		} else {
			if w.rollbacks != 1 {
				t.Fatal("abort/unknown必须终止Tx")
			}
			if outcome == outcomeUnknown && (!errors.Is(err, ErrOutcomeUnknown) || c.IsValid()) {
				t.Fatal("unknown应隔离")
			}
		}
	}
}

func TestWriterDriverBadConnOverridesOptimisticClassifier(t *testing.T) {
	w := &writerFakeWire{execErr: errors.Join(&mysql.MySQLError{Number: 1062}, driver.ErrBadConn)}
	c := writerFixture(w)
	c.outcomes = writerFakeOutcome{outcome: outcomeStatementRejected}
	_, err := c.ExecContext(context.Background(), "INSERT INTO t VALUES (1)", nil)
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) || c.IsValid() {
		t.Fatalf("混合错误不得升级确定: %v", err)
	}
}

func TestWriterDriverAutoRowsWaitForAllResultSets(t *testing.T) {
	w := &writerFakeWire{sets: 2}
	c := writerFixture(w)
	raw, err := c.QueryContext(context.Background(), "SELECT n FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := raw.(*guardedRows)
	values := make([]driver.Value, 1)
	if err = r.Next(values); err != nil {
		t.Fatal(err)
	}
	if err = r.Next(values); err != io.EOF || !r.HasNextResultSet() || w.commits != 0 {
		t.Fatalf("不能在第一结果EOF提交: %v", err)
	}
	if err = r.NextResultSet(); err != nil {
		t.Fatal(err)
	}
	if err = r.Next(values); err != nil {
		t.Fatal(err)
	}
	if err = r.Next(values); err != io.EOF {
		t.Fatal(err)
	}
	if w.commits != 1 {
		t.Fatal("最终EOF必须恰好一次提交")
	}
	if err = r.Close(); err != nil || w.commits != 1 {
		t.Fatal("重复Close不得再提交")
	}
	if n := len(w.events); n < 2 || w.events[n-2] != "rows-close" || w.events[n-1] != "commit" {
		t.Fatalf("先关闭raw rows再提交: %v", w.events)
	}
}

func TestWriterDriverRowsCommitFailureVisibleThroughSQLRowsErr(t *testing.T) {
	w := &writerFakeWire{commitErr: driver.ErrBadConn}
	inner := &writerFakeConnector{wire: w}
	connector, err := newGuardedConnector(inner, &writerFakeGate{wire: w}, writerFakeSession{wire: w}, mysqlStatementPolicy{}, writerUnitPermission())
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT n FROM t")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int64
		if err = rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if err = rows.Err(); !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Rows.Err丢失commit UNKNOWN: %v", err)
	}
	if w.queries != 1 || w.commits != 1 {
		t.Fatal("Query不能重放")
	}
}

func TestWriterDriverCanceledRowsRollbackAndQuarantine(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	ctx, cancel := context.WithCancel(context.Background())
	rows, err := c.QueryContext(ctx, "SELECT n FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	err = rows.Close()
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, context.Canceled) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("取消不得把UNKNOWN降类: %v", err)
	}
	if w.commits != 0 || w.rollbacks != 1 || c.IsValid() {
		t.Fatal("取消后不得提交/复用")
	}
}

func TestWriterDriverOldTxCannotCloseNextTransactionRows(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	old, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = old.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.QueryContext(context.Background(), "SELECT n FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = old.Rollback(); err != errGuardTxDone {
		t.Fatalf("旧Tx应已结束: %v", err)
	}
	if c.rows == nil || w.rollbacks != 0 {
		t.Fatal("旧Tx破坏了下一Tx")
	}
	_ = rows.Close()
	_ = current.Rollback()
}

func TestWriterDriverAdmissionDenialPreventsBusiness(t *testing.T) {
	for _, before := range []bool{true, false} {
		w := &writerFakeWire{}
		c := writerFixture(w)
		denied := errors.New("fixture denied")
		g := &writerFakeGate{wire: w}
		if before {
			g.beforeErr = denied
		} else {
			g.enterErr = denied
		}
		c.gate = g
		_, err := c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
		if err != denied || w.execs != 0 || w.commits != 0 {
			t.Fatal("拒绝后仍执行业务")
		}
		if before && w.begins != 0 || !before && w.rollbacks != 1 {
			t.Fatal("预读/锁后拒绝边界错误")
		}
	}
}

func TestWriterDriverPreparedExecutionReacquiresAdmission(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	stmt, err := c.PrepareContext(context.Background(), "UPDATE t SET n=?")
	if err != nil {
		t.Fatal(err)
	}
	if w.begins != 0 {
		t.Fatal("prepare不持业务slot")
	}
	for i := 0; i < 2; i++ {
		_, err = stmt.(driver.StmtExecContext).ExecContext(context.Background(), []driver.NamedValue{{Ordinal: 1, Value: int64(i)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if w.begins != 2 || w.commits != 2 || w.lastArgs[0].Value != int64(1) {
		t.Fatal("每次Stmt执行都应取得新资格且保参数")
	}
	_ = stmt.Close()
}

type writerLegacyConn struct{ wire *writerFakeWire }

func (c writerLegacyConn) Prepare(q string) (driver.Stmt, error) { return c.wire.Prepare(q) }
func (c writerLegacyConn) Close() error                          { return c.wire.Close() }
func (c writerLegacyConn) Begin() (driver.Tx, error)             { return c.wire.Begin() }
func TestWriterDriverFallbackPreparesInsideSingleAdmission(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	c.raw = writerLegacyConn{wire: w}
	if _, err := c.ExecContext(context.Background(), "UPDATE t SET n=?", []driver.NamedValue{{Ordinal: 1, Value: int64(3)}}); err != nil {
		t.Fatal(err)
	}
	if w.begins != 1 || w.prepares != 1 || w.execs != 1 || w.commits != 1 {
		t.Fatalf("fallback重复或绕过: %v", w.events)
	}
	if _, err := c.BeginTx(context.Background(), driver.TxOptions{ReadOnly: true}); err != errUnsupportedDriver {
		t.Fatal("legacy不应谎称ReadOnly支持")
	}
}

func TestWriterDriverRawAndDriverOpenExposeOnlyGuard(t *testing.T) {
	w := &writerFakeWire{}
	inner := &writerFakeConnector{wire: w}
	connector, err := newGuardedConnector(inner, &writerFakeGate{wire: w}, writerFakeSession{wire: w}, mysqlStatementPolicy{}, writerUnitPermission())
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	if _, err = db.Driver().Open("ignored"); err != errConnectorOnly {
		t.Fatal("Driver.Open泄漏raw入口")
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.Raw(func(v any) error {
		if _, ok := v.(*guardedConn); !ok {
			t.Fatal("Raw不是guard wrapper")
		}
		if _, ok := v.(*writerFakeWire); ok {
			t.Fatal("Raw泄漏底层")
		}
		_, err := v.(driver.ExecerContext).ExecContext(context.Background(), "COMMIT", nil)
		return err
	}); err != errUnsupportedStatement {
		t.Fatalf("Raw控制SQL必须拒绝: %v", err)
	}
	if w.execs != 0 {
		t.Fatal("拒绝前发出了SQL")
	}
}

func TestWriterDriverCommit1062IsUnknownNotStatementRejected(t *testing.T) {
	w := &writerFakeWire{commitErr: &mysql.MySQLError{Number: 1062}}
	c := writerFixture(w)
	_, err := c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
	if !errors.Is(err, ErrOutcomeUnknown) || c.IsValid() || w.execs != 1 {
		t.Fatalf("COMMIT阶段不能套用execute policy: %v", err)
	}
}

func TestWriterDriverRows1062WithCleanupFailureIsUnknown(t *testing.T) {
	for _, failStmt := range []bool{false, true} {
		w := &writerFakeWire{rowsNextErr: &mysql.MySQLError{Number: 1062}}
		c := writerFixture(w)
		c.outcomes = writerFakeOutcome{outcome: outcomeStatementRejected}
		if failStmt {
			c.raw = writerLegacyConn{wire: w}
			w.stmtCloseErr = driver.ErrBadConn
		} else {
			w.rowsCloseErr = driver.ErrBadConn
		}
		tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := c.QueryContext(context.Background(), "SELECT n FROM t", nil)
		if err != nil {
			t.Fatal(err)
		}
		err = raw.Next(make([]driver.Value, 1))
		if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) || w.rollbacks != 1 || c.IsValid() {
			t.Fatalf("1062不能掩盖清理失败: %v", err)
		}
		_ = tx.Rollback()
	}
}

func TestWriterDriverAuto1062RollsBackWithoutPoison(t *testing.T) {
	duplicate := &mysql.MySQLError{Number: 1062}
	w := &writerFakeWire{execErr: duplicate}
	c := writerFixture(w)
	_, err := c.ExecContext(context.Background(), "INSERT INTO t VALUES (1)", nil)
	if err != duplicate || w.rollbacks != 1 || w.commits != 0 || !c.IsValid() {
		t.Fatalf("确定auto语句失败应回滚但可复用: %v", err)
	}
}

func TestWriterDriverUnproved1213StaysUnknown(t *testing.T) {
	w := &writerFakeWire{execErr: &mysql.MySQLError{Number: 1213}}
	c := writerFixture(w)
	_, err := c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
	var server *mysql.MySQLError
	if !errors.Is(err, ErrOutcomeUnknown) || errors.As(err, &server) || c.IsValid() {
		t.Fatalf("未证明的目标Tx-abort不可伪称可重试: %v", err)
	}
}

func TestWriterDriverConfirmedCommitWinsLaterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &writerFakeWire{onCommit: cancel}
	c := writerFixture(w)
	_, err := c.ExecContext(ctx, "UPDATE t SET n=n+1", nil)
	if err != nil || w.commits != 1 || w.rollbacks != 0 {
		t.Fatalf("已收到COMMIT成功不能伪报取消: %v", err)
	}
}

type writerOptionalConn struct {
	*writerFakeWire
	resetErr error
	valid    bool
}

func (c writerOptionalConn) Ping(context.Context) error         { c.record("ping"); return nil }
func (c writerOptionalConn) ResetSession(context.Context) error { c.record("reset"); return c.resetErr }
func (c writerOptionalConn) IsValid() bool                      { return c.valid }
func (c writerOptionalConn) CheckNamedValue(v *driver.NamedValue) error {
	v.Value = int64(42)
	return nil
}
func TestWriterDriverSessionOptionalInterfacesPreservePolicy(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	c.raw = writerOptionalConn{writerFakeWire: w, valid: true}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.ResetSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.events, []string{"ping", "reset", "pin"}) {
		t.Fatalf("reset后必须重核强会话/boot: %v", w.events)
	}
	value := driver.NamedValue{Ordinal: 1, Value: "fixture"}
	if err := c.CheckNamedValue(&value); err != nil || value.Value != int64(42) {
		t.Fatal("NamedValueChecker未委托")
	}
	c.raw = writerOptionalConn{writerFakeWire: w, valid: false}
	if c.IsValid() {
		t.Fatal("不能覆盖底层Validator=false")
	}
}

func TestWriterDriverResetFailurePoisonsWithoutBadConnReplay(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	c.raw = writerOptionalConn{writerFakeWire: w, valid: true, resetErr: driver.ErrBadConn}
	err := c.ResetSession(context.Background())
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) || c.IsValid() {
		t.Fatalf("reset失败不可回池: %v", err)
	}
	if _, err = c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil); err != errGuardClosed || w.execs != 0 {
		t.Fatal("poison后仍发SQL")
	}
}

func TestWriterDriverRowsMetadataFallbackAndCloseError(t *testing.T) {
	w := &writerFakeWire{rowsCloseErr: driver.ErrBadConn}
	c := writerFixture(w)
	raw, err := c.QueryContext(context.Background(), "SELECT n FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.(driver.RowsColumnTypeScanType).ColumnTypeScanType(0); got != reflect.TypeOf((*any)(nil)).Elem() {
		t.Fatal("ScanType fallback错误")
	}
	if name := raw.(driver.RowsColumnTypeDatabaseTypeName).ColumnTypeDatabaseTypeName(0); name != "" {
		t.Fatal("DatabaseType fallback错误")
	}
	if _, ok := raw.(driver.RowsColumnTypeLength).ColumnTypeLength(0); ok {
		t.Fatal("Length不应谎报支持")
	}
	if _, ok := raw.(driver.RowsColumnTypeNullable).ColumnTypeNullable(0); ok {
		t.Fatal("Nullable不应谎报支持")
	}
	if _, _, ok := raw.(driver.RowsColumnTypePrecisionScale).ColumnTypePrecisionScale(0); ok {
		t.Fatal("Precision不应谎报支持")
	}
	err = raw.Close()
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) || raw.Close() != err || w.rollbacks != 1 {
		t.Fatalf("Close失败不能吞掉或重复结束: %v", err)
	}
}

func TestWriterDriverStatementDefaultConverter(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	stmt, err := c.Prepare("UPDATE t SET n=?")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	value, err := stmt.(driver.ColumnConverter).ColumnConverter(0).ConvertValue(int(5))
	if err != nil || value != int64(5) {
		t.Fatalf("默认converter未保留: %v %v", value, err)
	}
}

func TestWriterDriverUnknownDoesNotExposeRetryableCause(t *testing.T) {
	err := unknown("fixture", errors.Join(driver.ErrBadConn, context.Canceled, context.DeadlineExceeded))
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("UNKNOWN不应降为底层可重试类别")
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("UNKNOWN不能暴露cause链")
	}
}

func TestWriterDriver1062WithConcurrentCancelCannotKeepTransaction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &writerFakeWire{execErr: &mysql.MySQLError{Number: 1062}, onExec: cancel}
	c := writerFixture(w)
	tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ExecContext(ctx, "INSERT INTO t VALUES (1)", nil)
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, context.Canceled) || w.rollbacks != 1 || c.IsValid() {
		t.Fatalf("取消混合结果不能当确定1062: %v", err)
	}
	_ = tx.Rollback()
}

func retirementPlanFixture(t *testing.T) (*Catalog, *pb.SqlWriterMaintenanceReceipt_Plan, *pb.SqlWriterLifecycleReceipt) {
	t.Helper()
	key := bytes.Repeat([]byte{1}, 16)
	instance := bytes.Repeat([]byte{2}, 16)
	run := bytes.Repeat([]byte{3}, 16)
	c := &Catalog{config: CatalogConfig{AuthoritySchema: "authority", Generation: "abcdef", ProtectedSchemas: []string{"authority", "game"}, PhysicalAuthoritySHA256: bytes.Repeat([]byte{4}, 32), Principal: "operator@%"}}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{AuthoritySchema: c.config.AuthoritySchema, ProtectedSchemas: c.config.ProtectedSchemas, PhysicalClusterIdentity: c.config.PhysicalAuthoritySHA256, RunId: run, BeforeEpoch: 7, CatalogRevision: 11, ActiveWriterKeys: [][]byte{key}, RootIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{{IssuerId: bytes.Repeat([]byte{5}, 16), InstanceId: instance, PhysicalWriterKey: key, LogicalRole: "SOURCE", PermanentRevivalFenceId: bytes.Repeat([]byte{6}, 16), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: "/fixture/launcher", Sha256: bytes.Repeat([]byte{7}, 32)}}}}}
	request := &pb.SqlWriterLifecycleReceipt_Request{RequestId: bytes.Repeat([]byte{8}, 16), Kind: pb.SqlWriterLifecycleReceipt_RETIRE, Writer: &pb.SqlWriterRegistry{WriterKey: key, InstanceId: instance, Generation: c.config.Generation, SqlPrincipal: []byte("writer@%"), RegistrationDigest: bytes.Repeat([]byte{9}, 32), State: pb.SqlWriterRegistry_ACTIVE}, ExpectedEpoch: 7, ExpectedCatalogRevision: 11, AuthoritySchema: c.config.AuthoritySchema, ProtectedSchemas: c.config.ProtectedSchemas, DeploymentDigest: c.config.PhysicalAuthoritySHA256, DeploymentId: "fixture-boot", MaintenanceRunId: run}
	raw, digest, err := c.validateLifecycle(request, pb.SqlWriterLifecycleReceipt_RETIRE)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &pb.SqlWriterLifecycleReceipt{RequestId: request.RequestId, Kind: request.Kind, Generation: c.config.Generation, RequestDigest: digest, RequestProto: raw, PreviousEpoch: 7, ResultingEpoch: 7, CatalogRevision: 12}
	return c, plan, receipt
}

func TestRetirementFoldPreservesImmutablePlanAndAllowsEmptyActiveSet(t *testing.T) {
	c, plan, receipt := retirementPlanFixture(t)
	original := proto.Clone(plan)
	keys, err := c.foldRetirements(plan, 11, nil)
	if err != nil || !exactKeys(keys, plan.ActiveWriterKeys) {
		t.Fatalf("P0目录不应变化: %v", err)
	}
	keys, err = c.foldRetirements(plan, 12, []*pb.SqlWriterLifecycleReceipt{receipt})
	if err != nil || len(keys) != 0 {
		t.Fatalf("已证明退役应得到空ACTIVE集: %v", err)
	}
	if !proto.Equal(plan, original) {
		t.Fatal("不能为退役重拍或回写P0")
	}
}

func TestRetirementFoldRejectsUnrelatedOrIncompleteCatalogChanges(t *testing.T) {
	mutations := map[string]func(*pb.SqlWriterLifecycleReceipt){
		"外来注册": func(r *pb.SqlWriterLifecycleReceipt) { r.Kind = pb.SqlWriterLifecycleReceipt_REGISTER },
		"目录跳号": func(r *pb.SqlWriterLifecycleReceipt) { r.CatalogRevision++ },
		"跨世代":  func(r *pb.SqlWriterLifecycleReceipt) { r.Generation = "fedcba" },
		"偷偷改E": func(r *pb.SqlWriterLifecycleReceipt) { r.ResultingEpoch++ },
		"正文漂移": func(r *pb.SqlWriterLifecycleReceipt) { r.RequestProto = append(r.RequestProto, 0) },
		"别的run": func(r *pb.SqlWriterLifecycleReceipt) {
			q := &pb.SqlWriterLifecycleReceipt_Request{}
			if proto.Unmarshal(r.RequestProto, q) != nil {
				t.Fatal("fixture解码")
			}
			q.MaintenanceRunId = bytes.Repeat([]byte{90}, 16)
			r.RequestProto, _ = deterministic(q)
			r.RequestDigest, _ = messageDigest(q)
		},
		"未知writer": func(r *pb.SqlWriterLifecycleReceipt) {
			q := &pb.SqlWriterLifecycleReceipt_Request{}
			if proto.Unmarshal(r.RequestProto, q) != nil {
				t.Fatal("fixture解码")
			}
			q.Writer.WriterKey = bytes.Repeat([]byte{91}, 16)
			r.RequestProto, _ = deterministic(q)
			r.RequestDigest, _ = messageDigest(q)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c, plan, r := retirementPlanFixture(t)
			mutate(r)
			if _, err := c.foldRetirements(plan, 12, []*pb.SqlWriterLifecycleReceipt{r}); err == nil {
				t.Fatal("接受了非本轮合法退役的目录漂移")
			}
		})
	}
	c, plan, r := retirementPlanFixture(t)
	for _, receipts := range [][]*pb.SqlWriterLifecycleReceipt{nil, {r, r}} {
		if _, err := c.foldRetirements(plan, 12, receipts); err == nil {
			t.Fatal("缺失或重复收据不得通过")
		}
	}
	plan.RootIssuers = nil
	if c.validatePlan(plan) == nil {
		t.Fatal("P0不能遗漏ACTIVE writer的关闭责任人")
	}
}

func TestClosureRequiresSameBootFenceAndSettledFrontier(t *testing.T) {
	_, plan, _ := retirementPlanFixture(t)
	seed := plan.RootIssuers[0]
	closed := proto.Clone(seed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	closed.AcceptingClosedMs = 100
	closed.AcceptedSettledMs = 101
	closed.AcceptedFrontierDigest = bytes.Repeat([]byte{4}, 32)
	if !closureMatches(seed, closed) {
		t.Fatal("同boot实际settled原件应匹配")
	}
	closed.UnresolvedPublications = 1
	if closureMatches(seed, closed) {
		t.Fatal("UNKNOWN不能充当settled")
	}
	closed.UnresolvedPublications = 0
	closed.InstanceId = bytes.Repeat([]byte{99}, 16)
	if closureMatches(seed, closed) {
		t.Fatal("进程重启不能冒旧boot闭合")
	}
}

func TestFreezeTicketDigestBindsWindowAndIgnoresOnlyCommittedFields(t *testing.T) {
	ticket := &pb.SqlWriterFreezeTicket{TicketId: bytes.Repeat([]byte{1}, 16), OperationId: bytes.Repeat([]byte{2}, 16), AttemptRevision: 1, Generation: "abcdef", NotBeforeMs: 100, ExpiresMs: 200, State: pb.SqlWriterFreezeTicket_AVAILABLE, AuthorizationKind: pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE}
	original, err := SealFreezeTicketRequest(ticket)
	if err != nil {
		t.Fatal(err)
	}
	committed := proto.Clone(ticket).(*pb.SqlWriterFreezeTicket)
	committed.UpdatedMs = 123
	committed.GrantReceiptDigest = bytes.Repeat([]byte{3}, 32)
	got, err := SealFreezeTicketRequest(committed)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("SQL提交字段不能改变授权输入摘要")
	}
	ticket.ExpiresMs++
	got, err = SealFreezeTicketRequest(ticket)
	if err != nil || bytes.Equal(got, original) {
		t.Fatal("授权窗口必须进入原请求摘要")
	}
}
