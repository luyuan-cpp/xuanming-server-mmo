package authoritysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// R6(2026-11-10)的契约用例,夹具复用 driver_test.go 的 writerFakeWire / writerFixture。未运行,待 Codex 验证。

// transportFailure 是连接层的失败:混着 ErrBadConn,不是直接的服务端回包。
func transportFailure() error {
	return errors.Join(errors.New("fixture transport"), driver.ErrBadConn)
}

func TestPlainDMLStatement(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		keyword string
		plain   bool
	}{
		{"大写 SELECT", "SELECT n FROM t", "SELECT", true},
		{"小写 update", "update t set n=n+1", "UPDATE", true},
		{"大小写混写 Insert", "Insert INTO t VALUES (1)", "INSERT", true},
		{"DELETE", "DELETE FROM t WHERE id=1", "DELETE", true},
		{"REPLACE", "REPLACE INTO t VALUES (1)", "REPLACE", true},
		{"WITH", "WITH c AS (SELECT 1 AS n) SELECT n FROM c", "WITH", true},
		{"前导空白", " \t\r\n\fSELECT 1", "SELECT", true},
		{"井号注释", "# note\nSELECT 1", "SELECT", true},
		{"双横线注释", "-- note\r\nUPDATE t SET n=1", "UPDATE", true},
		{"块注释", "/* note */ DELETE FROM t", "DELETE", true},
		{"空块注释", "/**/SELECT 1", "SELECT", true},
		{"关键字前的优化器提示", "/*+ MAX_EXECUTION_TIME(100) */ SELECT 1", "SELECT", true},
		{"关键字后的优化器提示", "SELECT /*+ MAX_EXECUTION_TIME(100) */ n FROM t", "SELECT", true},
		{"连续注释", "/* a */ # b\n-- c\n/* d */SELECT 1", "SELECT", true},
		{"ALTER", "ALTER TABLE t ADD COLUMN m INT", "ALTER", false},
		{"注释里写着 SELECT 的 ALTER", "/* SELECT */ ALTER TABLE t ADD COLUMN m INT", "ALTER", false},
		{"BEGIN", "BEGIN", "BEGIN", false},
		{"START TRANSACTION", "start transaction", "START", false},
		{"LOCK TABLES", "LOCK TABLES t WRITE", "LOCK", false},
		{"SET", "SET SESSION lock_wait_timeout=1, innodb_lock_wait_timeout=1", "SET", false},
		{"SHOW 不算纯 DML", "SHOW TABLES", "SHOW", false},
		{"更长的单词", "SELECTX FROM t", "SELECTX", false},
		{"关键字后紧跟下划线", "SELECT_n FROM t", "", false},
		{"关键字后紧跟数字", "SELECT1", "", false},
		{"空串", "", "", false},
		{"只有空白", " \n\t", "", false},
		{"只有注释", "/* SELECT 1 */", "", false},
		{"注释没闭合", "/* SELECT 1", "", false},
		{"行注释之后注释没闭合", "-- x\n/* unclosed SELECT 1", "", false},
		{"MySQL 可执行注释", "/*!50000 ALTER TABLE t ENGINE=InnoDB */ SELECT 1", "", false},
		{"TiDB 可执行注释", "/*T![clustered_index] */ SELECT 1", "", false},
		{"双横线后没有空白不是注释", "--SELECT 1", "", false},
		{"以括号开头", "(SELECT 1)", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := leadingKeyword(test.query); got != test.keyword {
				t.Fatalf("leadingKeyword(%q) = %q, want %q", test.query, got, test.keyword)
			}
			if got := plainDMLStatement(test.query); got != test.plain {
				t.Fatalf("plainDMLStatement(%q) = %t, want %t", test.query, got, test.plain)
			}
		})
	}
}

// ErrTxAbortedBeforeCommit 与 ErrOutcomeUnknown 互斥,也不是 ErrBadConn;错误树里混进结局未知时整体不能按确定没写处理。
func TestTxAbortedBeforeCommitExcludesOutcomeUnknown(t *testing.T) {
	if errors.Is(ErrTxAbortedBeforeCommit, ErrOutcomeUnknown) || errors.Is(ErrOutcomeUnknown, ErrTxAbortedBeforeCommit) {
		t.Fatal("the two sentinels must not match each other")
	}
	if errors.Is(ErrTxAbortedBeforeCommit, driver.ErrBadConn) {
		t.Fatal("database/sql must not replay an aborted-before-commit error as a bad connection")
	}
	unknownErr := unknown("业务发送后", driver.ErrBadConn)
	if errors.Is(unknownErr, ErrTxAbortedBeforeCommit) || IsAbortedBeforeCommit(unknownErr) {
		t.Fatal("an unknown outcome must not read as aborted-before-commit")
	}
	// assetop.runTx 把业务错误与回滚错误 Join 起来,两边都是这个哨兵时仍要认得。
	joined := errors.Join(fmt.Errorf("exec: %w", ErrTxAbortedBeforeCommit), fmt.Errorf("assetop: rollback: %w", ErrTxAbortedBeforeCommit))
	if !IsAbortedBeforeCommit(ErrTxAbortedBeforeCommit) || !IsAbortedBeforeCommit(joined) {
		t.Fatal("wrapped or joined aborted-before-commit must still be recognized")
	}
	if IsAbortedBeforeCommit(errors.Join(ErrTxAbortedBeforeCommit, unknownErr)) {
		t.Fatal("an unknown outcome anywhere in the tree must win")
	}
	if IsAbortedBeforeCommit(nil) || IsAbortedBeforeCommit(errors.New("other")) || IsAbortedBeforeCommit(&mysql.MySQLError{Number: mysqlErrDeadlock}) {
		t.Fatal("other errors are not aborted-before-commit")
	}
}

// 显式事务里只发过纯 DML,COMMIT 之前遇到传输错误:确定中止,结果是 ErrTxAbortedBeforeCommit,不是结局未知、不置观察位。
// 连接状态不明,照旧隔离:同一个事务后面的语句发不出去,COMMIT 也不会再发。
func TestExplicitTxTransportAbortBeforeCommitIsDeterminate(t *testing.T) {
	w := &writerFakeWire{}
	c := writerFixture(w)
	ctx, _ := ObserveWriterFrozen(context.Background())
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ExecContext(ctx, "UPDATE t SET n=n+1", nil); err != nil {
		t.Fatal(err)
	}
	w.execErr = transportFailure()
	_, err = c.ExecContext(ctx, "INSERT INTO t VALUES (1)", nil)
	if !errors.Is(err, ErrTxAbortedBeforeCommit) || !IsAbortedBeforeCommit(err) || errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("transport failure before COMMIT = %v, want ErrTxAbortedBeforeCommit", err)
	}
	if WriterOutcomeUnknownSeen(ctx) {
		t.Fatal("aborted-before-commit must not set the unknown-outcome observation")
	}
	if c.IsValid() || w.rollbacks != 1 || w.commits != 0 {
		t.Fatalf("connection must be rolled back and quarantined: valid=%t rollbacks=%d commits=%d", c.IsValid(), w.rollbacks, w.commits)
	}
	w.execErr = nil
	if _, err = c.ExecContext(ctx, "UPDATE t SET n=n+1", nil); err != errGuardClosed {
		t.Fatalf("statement on the quarantined connection = %v, want errGuardClosed", err)
	}
	if err = tx.Commit(); !IsAbortedBeforeCommit(err) || w.commits != 0 {
		t.Fatalf("commit after the abort = %v (commits=%d), want the abort and no COMMIT", err, w.commits)
	}
	if w.execs != 2 {
		t.Fatalf("execs=%d, want 2: nothing may be sent after the abort", w.execs)
	}
}

// 事务里先执行过可能隐式提交的语句(ALTER),之后的传输错误仍是结局未知:那条语句可能已经把前面的写提交了。
// 预编译语句的执行同样要记下(guardedStmt 把 s.query 传给 execLocked)。
func TestTxAfterNonDMLTransportFailureStaysUnknown(t *testing.T) {
	const alter = "ALTER TABLE t ADD COLUMN m INT"
	for _, prepared := range []bool{false, true} {
		name := "direct"
		if prepared {
			name = "prepared"
		}
		t.Run(name, func(t *testing.T) {
			w := &writerFakeWire{}
			c := writerFixtureWith(w, Schema("schema_test@%"))
			ctx, _ := ObserveWriterFrozen(context.Background())
			tx, err := c.BeginTx(ctx, driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if prepared {
				var stmt driver.Stmt
				if stmt, err = c.PrepareContext(ctx, alter); err != nil {
					t.Fatal(err)
				}
				if _, err = stmt.(driver.StmtExecContext).ExecContext(ctx, nil); err != nil {
					t.Fatal(err)
				}
				if err = stmt.Close(); err != nil {
					t.Fatal(err)
				}
			} else if _, err = c.ExecContext(ctx, alter, nil); err != nil {
				t.Fatal(err)
			}
			w.execErr = transportFailure()
			_, err = c.ExecContext(ctx, "UPDATE t SET n=n+1", nil)
			if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrTxAbortedBeforeCommit) || IsAbortedBeforeCommit(err) || errors.Is(err, driver.ErrBadConn) {
				t.Fatalf("transport failure after a non-DML statement = %v, want ErrOutcomeUnknown", err)
			}
			if !WriterOutcomeUnknownSeen(ctx) || c.IsValid() || w.rollbacks != 1 || w.commits != 0 {
				t.Fatalf("unknown=%t valid=%t rollbacks=%d commits=%d", WriterOutcomeUnknownSeen(ctx), c.IsValid(), w.rollbacks, w.commits)
			}
			_ = tx.Rollback()
		})
	}
}

// 纯 DML 事务里服务端回包(没有列在分类里的 1146):连接是好的,原样返回原错误,不是结局未知、不置观察位。
// 与 1205/1213/9007 同一支,显式事务置 abortedTx:调用方吞掉错误接着用同一个事务,后面的语句回原错误,
// 不会落进新的自动事务单独提交;事务结束之后连接回池可用。
func TestExplicitTxServerErrorReturnsOriginal(t *testing.T) {
	server := &mysql.MySQLError{Number: 1146, Message: "fixture table missing"}
	w := &writerFakeWire{}
	c := writerFixture(w)
	ctx, _ := ObserveWriterFrozen(context.Background())
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w.execErr = server
	_, err = c.ExecContext(ctx, "UPDATE t SET n=n+1", nil)
	if err != server || errors.Is(err, ErrOutcomeUnknown) || IsAbortedBeforeCommit(err) {
		t.Fatalf("server error in a plain-DML transaction = %v, want the original error", err)
	}
	if WriterOutcomeUnknownSeen(ctx) || w.rollbacks != 1 || w.commits != 0 {
		t.Fatalf("unknown=%t rollbacks=%d commits=%d", WriterOutcomeUnknownSeen(ctx), w.rollbacks, w.commits)
	}
	w.execErr = nil
	if _, err = c.ExecContext(ctx, "UPDATE t SET m=1", nil); err != server {
		t.Fatalf("statement after the abort = %v, want the original error", err)
	}
	if err = tx.Commit(); err != server || w.commits != 0 {
		t.Fatalf("commit after the abort = %v (commits=%d), want the original error and no COMMIT", err, w.commits)
	}
	if !c.IsValid() {
		t.Fatal("connection must be reusable once the transaction ends")
	}
	if _, err = c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil); err != nil || w.commits != 1 {
		t.Fatalf("reuse after the abort: err=%v commits=%d", err, w.commits)
	}
}

// 回滚没确认:COMMIT 没发出去,事务里只发过纯 DML 时确定没写,不让它被改写成结局未知;连接照旧隔离。
// 执行过可能隐式提交的语句之后,回滚没确认仍是结局未知。
func TestTxRollbackFailureBeforeCommit(t *testing.T) {
	rollbackErr := errors.New("fixture connection reset")
	t.Run("explicit rollback after plain DML", func(t *testing.T) {
		w := &writerFakeWire{rollbackErr: rollbackErr}
		c := writerFixture(w)
		ctx, _ := ObserveWriterFrozen(context.Background())
		tx, err := c.BeginTx(ctx, driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.ExecContext(ctx, "UPDATE t SET n=n+1", nil); err != nil {
			t.Fatal(err)
		}
		err = tx.Rollback()
		if !IsAbortedBeforeCommit(err) || errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("unconfirmed rollback after plain DML = %v, want ErrTxAbortedBeforeCommit", err)
		}
		if WriterOutcomeUnknownSeen(ctx) || c.IsValid() || w.commits != 0 {
			t.Fatalf("unknown=%t valid=%t commits=%d", WriterOutcomeUnknownSeen(ctx), c.IsValid(), w.commits)
		}
	})
	t.Run("server abort then unconfirmed rollback", func(t *testing.T) {
		w := &writerFakeWire{execErr: &mysql.MySQLError{Number: mysqlErrDeadlock}, rollbackErr: rollbackErr}
		c := writerFixture(w)
		ctx, _ := ObserveWriterFrozen(context.Background())
		_, err := c.ExecContext(ctx, "UPDATE t SET n=n+1", nil)
		if !IsAbortedBeforeCommit(err) || errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("deadlock followed by an unconfirmed rollback = %v, want ErrTxAbortedBeforeCommit", err)
		}
		if WriterOutcomeUnknownSeen(ctx) || c.IsValid() || w.rollbacks != 1 || w.commits != 0 {
			t.Fatalf("unknown=%t valid=%t rollbacks=%d commits=%d", WriterOutcomeUnknownSeen(ctx), c.IsValid(), w.rollbacks, w.commits)
		}
	})
	t.Run("unconfirmed rollback after a non-DML statement", func(t *testing.T) {
		w := &writerFakeWire{rollbackErr: rollbackErr}
		c := writerFixtureWith(w, Schema("schema_test@%"))
		ctx, _ := ObserveWriterFrozen(context.Background())
		tx, err := c.BeginTx(ctx, driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.ExecContext(ctx, "ALTER TABLE t ADD COLUMN m INT", nil); err != nil {
			t.Fatal(err)
		}
		err = tx.Rollback()
		if !errors.Is(err, ErrOutcomeUnknown) || IsAbortedBeforeCommit(err) || IsCommitOutcomeUnknown(err) {
			t.Fatalf("unconfirmed rollback after a non-DML statement = %v, want a non-commit unknown outcome", err)
		}
		if !WriterOutcomeUnknownSeen(ctx) || c.IsValid() {
			t.Fatalf("unknown=%t valid=%t", WriterOutcomeUnknownSeen(ctx), c.IsValid())
		}
	})
}

// 自动事务(写者权限、不开显式事务,直接执行一条 UPDATE)遇到传输错误:COMMIT 没发,确定中止;语句只发一次,连接隔离。
func TestAutoTxTransportAbortIsDeterminate(t *testing.T) {
	w := &writerFakeWire{execErr: transportFailure()}
	c := writerFixture(w)
	ctx, _ := ObserveWriterFrozen(context.Background())
	_, err := c.ExecContext(ctx, "UPDATE t SET n=n+1", nil)
	if !errors.Is(err, ErrTxAbortedBeforeCommit) || errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("automatic transaction transport failure = %v, want ErrTxAbortedBeforeCommit", err)
	}
	if w.execs != 1 || w.commits != 0 || w.rollbacks != 1 || c.IsValid() || WriterOutcomeUnknownSeen(ctx) {
		t.Fatalf("execs=%d commits=%d rollbacks=%d valid=%t unknown=%t", w.execs, w.commits, w.rollbacks, c.IsValid(), WriterOutcomeUnknownSeen(ctx))
	}
}

// COMMIT 自身的错误不在 R6 之列:COMMIT 已经发出去,可能已经提交,仍是提交结局未知。
func TestTxCommitFailureStaysUnknown(t *testing.T) {
	w := &writerFakeWire{commitErr: transportFailure()}
	c := writerFixture(w)
	ctx, _ := ObserveWriterFrozen(context.Background())
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ExecContext(ctx, "UPDATE t SET n=n+1", nil); err != nil {
		t.Fatal(err)
	}
	err = tx.Commit()
	if !errors.Is(err, ErrOutcomeUnknown) || !IsCommitOutcomeUnknown(err) || IsAbortedBeforeCommit(err) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("COMMIT failure = %v, want a commit unknown outcome", err)
	}
	if !WriterOutcomeUnknownSeen(ctx) || c.IsValid() || w.commits != 1 {
		t.Fatalf("unknown=%t valid=%t commits=%d", WriterOutcomeUnknownSeen(ctx), c.IsValid(), w.commits)
	}
}
