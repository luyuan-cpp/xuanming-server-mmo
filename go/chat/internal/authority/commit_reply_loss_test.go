package authority

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"shared/authoritysql"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

var errCommittedReplyLost = errors.New("测试：真实TiDB已提交但调用者未收到提交确认")

// 只在真实驱动Commit成功之后丢一次ACK，不替代SQL/P/路由或永久唯一行。
type replyLossConnector struct {
	inner driver.Connector
	armed *atomic.Bool
}

func (c replyLossConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c replyLossConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &replyLossConn{Conn: conn, armed: c.armed}, nil
}

type replyLossConn struct {
	driver.Conn
	armed *atomic.Bool
}

func (c *replyLossConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return replyLossTx{Tx: tx, armed: c.armed}, nil
}
func (c *replyLossConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *replyLossConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
func (c *replyLossConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

type replyLossTx struct {
	driver.Tx
	armed *atomic.Bool
}

func (tx replyLossTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.armed.CompareAndSwap(true, false) {
		return errCommittedReplyLost
	}
	return nil
}

func TestChatTiDBCommittedReplyLossReplaysOriginalOnce(t *testing.T) {
	s, fixture := nativeStoreFixture(t, false)
	cfg, err := mysql.ParseDSN(fixture.DSN(s.roleSchema))
	require.NoError(t, err)
	inner, err := mysql.NewConnector(cfg)
	require.NoError(t, err)
	armed := &atomic.Bool{}
	guarded, err := authoritysql.PinCurrentSession(inner, true, fixture.Permission)
	require.NoError(t, err)
	db := sql.OpenDB(replyLossConnector{inner: guarded, armed: armed})
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, cancel := context.WithTimeout(original(t, 11), 10*time.Second)
	defer cancel()
	lossy, err := New(ctx, db, s.roleSchema, s.rules)
	require.NoError(t, err)
	armed.Store(true)
	in := sendRequest("lost-commit-ack", 22)
	_, err = lossy.Send(ctx, in)
	require.ErrorIs(t, err, errCommittedReplyLost)
	require.False(t, armed.Load(), "必须真实经过Commit后丢确认，不能把发送前故障冒充提交未知")
	require.Equal(t, 1, count(t, s))
	// 用正常新实例和原UUID恢复，不重新生成ID、不重写历史、不刷新接受时间。
	restarted, err := New(ctx, s.db, s.roleSchema, s.rules)
	require.NoError(t, err)
	before := pull(t, restarted, ctx, 22)
	require.Len(t, before, 1)
	result, err := restarted.Send(ctx, in)
	require.NoError(t, err)
	require.True(t, result.Duplicate)
	require.Zero(t, result.Tip)
	after := pull(t, restarted, ctx, 22)
	require.Len(t, after, 1)
	require.Equal(t, before[0].SendTimeMs, after[0].SendTimeMs)
	require.Equal(t, 1, count(t, s))
}
