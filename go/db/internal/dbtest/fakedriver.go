// Package dbtest 提供一个极小的内存版 database/sql 驱动,专供 go/db 的单测使用。
//
// 为什么要自己写:database/sql 的 *sql.Row / *sql.Rows 都是不可从外部构造的
// 结构体,想给「拿到连接之后的那段逻辑」写单测,只能从 driver 层伪造。仓库里
// 没有 sqlmock 之类的依赖,也不打算为单测引入新依赖,所以这里做一个自包含的。
//
// 只实现被测代码真正用到的那几个接口:ExecerContext / QueryerContext / Pinger。
// 不支持事务与 Prepare —— 用到了就说明被测代码走了预期外的路径,直接报错更好。
package dbtest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Rows 是一次查询的返回集。Values 为空表示零行。
type Rows struct {
	Columns []string
	Values  [][]driver.Value
}

// Handler 处理一条语句。返回 nil rows 表示「只是执行,没有结果集」。
type Handler func(ctx context.Context, query string, args []driver.NamedValue) (*Rows, error)

// Recorder 按执行顺序记录所有被下发的语句。
type Recorder struct {
	mu         sync.Mutex
	statements []string
}

func (r *Recorder) record(query string) {
	r.mu.Lock()
	r.statements = append(r.statements, query)
	r.mu.Unlock()
}

// Statements 返回已执行语句的快照。
func (r *Recorder) Statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.statements...)
}

// Contains 报告是否执行过含有 substr 的语句。
func (r *Recorder) Contains(substr string) bool {
	for _, s := range r.Statements() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

var driverSeq atomic.Int64

// Open 注册一个一次性驱动并返回连上它的 *sql.DB。
// t.Cleanup 负责关闭,调用方不需要再管。
func Open(t *testing.T, h Handler) (*sql.DB, *Recorder) {
	t.Helper()
	rec := &Recorder{}
	name := fmt.Sprintf("dbtest-%d", driverSeq.Add(1))
	sql.Register(name, &fakeDriver{handler: h, recorder: rec})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open fake driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, rec
}

type fakeDriver struct {
	handler  Handler
	recorder *Recorder
}

func (d *fakeDriver) Open(string) (driver.Conn, error) {
	return &fakeConn{handler: d.handler, recorder: d.recorder}, nil
}

type fakeConn struct {
	handler  Handler
	recorder *Recorder
}

var (
	_ driver.Conn           = (*fakeConn)(nil)
	_ driver.ExecerContext  = (*fakeConn)(nil)
	_ driver.QueryerContext = (*fakeConn)(nil)
	_ driver.Pinger         = (*fakeConn)(nil)
)

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("dbtest: Prepare is not supported (query=%q)", query)
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("dbtest: transactions are not supported")
}

func (c *fakeConn) Ping(context.Context) error { return nil }

func (c *fakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.recorder.record(query)
	if _, err := c.handler(ctx, query, args); err != nil {
		return nil, err
	}
	return driver.RowsAffected(0), nil
}

func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.recorder.record(query)
	rows, err := c.handler(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = &Rows{}
	}
	return &fakeRows{columns: rows.Columns, values: rows.Values}, nil
}

type fakeRows struct {
	columns []string
	values  [][]driver.Value
	cursor  int
}

func (r *fakeRows) Columns() []string { return r.columns }

func (r *fakeRows) Close() error { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.cursor >= len(r.values) {
		return io.EOF
	}
	row := r.values[r.cursor]
	r.cursor++
	for i := range dest {
		if i < len(row) {
			dest[i] = row[i]
		}
	}
	return nil
}
