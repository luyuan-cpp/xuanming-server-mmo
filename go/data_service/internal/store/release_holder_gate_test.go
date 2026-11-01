package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"shared/authoritysql"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/syncx"
)

// fakeGateRowConnector 只回答闸门行那条读。
type fakeGateRowConnector struct {
	writable int64
	missing  bool
	err      error
}

func (c fakeGateRowConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeGateRowConn{c}, nil
}
func (c fakeGateRowConnector) Driver() driver.Driver { return fakeNameDriver{} }

type fakeGateRowConn struct{ fakeGateRowConnector }

func (c fakeGateRowConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("不支持 Prepare")
}
func (c fakeGateRowConn) Close() error              { return nil }
func (c fakeGateRowConn) Begin() (driver.Tx, error) { return fakeGateRowTx{}, nil }

// BeginTx:shardTx 显式要 READ COMMITTED,没有这个方法 database/sql 直接拒绝。
func (c fakeGateRowConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return fakeGateRowTx{}, nil
}
func (c fakeGateRowConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "route_meta") || !strings.Contains(query, "FOR UPDATE") {
		return nil, fmt.Errorf("未预期的查询 %q", query)
	}
	if c.err != nil {
		return nil, c.err
	}
	rows := &fakeNameRows{cols: []string{"writable"}}
	if !c.missing {
		rows.rows = [][]driver.Value{{c.writable}}
	}
	return rows, nil
}

type fakeGateRowTx struct{}

func (fakeGateRowTx) Commit() error   { return nil }
func (fakeGateRowTx) Rollback() error { return nil }

// 按需开池撞上闸门冻结不进冷却:窗口是秒级,库没有坏,窗口一过下一个请求就要能打开。别的失败才冷却。
func TestPlayerStorePoolOpenFrozenDoesNotBackOff(t *testing.T) {
	const storage = uint32(1000000)
	opens := 0
	var result error
	pools := &PlayerStorePools{allowed: map[uint32]bool{storage: true}, now: time.Now, flight: syncx.NewSingleFlight(), pools: map[uint32]*sql.DB{}, failures: map[uint32]playerStoreOpenFailure{},
		opener: func(string) (*sql.DB, error) {
			opens++
			if result != nil {
				return nil, result
			}
			db := sql.OpenDB(fakeGateRowConnector{writable: 1})
			t.Cleanup(func() { _ = db.Close() })
			return db, nil
		}}
	ctx := context.Background()

	result = authoritysql.ErrWriterFrozen
	_, _, err := pools.DB(ctx, storage)
	require.True(t, authoritysql.IsWriterFrozen(err), "错误链要分得清冻结: %v", err)
	_, _, err = pools.DB(ctx, storage)
	require.True(t, authoritysql.IsWriterFrozen(err))
	require.Equal(t, 2, opens, "冻结不进冷却,每次都再试")
	require.Empty(t, pools.failures)

	result = errors.New("connection refused")
	_, _, err = pools.DB(ctx, storage)
	require.Error(t, err)
	_, _, err = pools.DB(ctx, storage)
	require.Error(t, err)
	require.Equal(t, 3, opens, "真的打不开才冷却")

	// 解冻之后打开成功。
	pools.failures = map[uint32]playerStoreOpenFailure{}
	result = nil
	db, _, err := pools.DB(ctx, storage)
	require.NoError(t, err)
	require.NotNil(t, db)
}

// admission_recover 用维护身份,不过闸门。闸门冻结时同事务拒绝,什么都不写;别的读失败也不能当成开着。
func TestRequireGateOpenTx(t *testing.T) {
	run := func(connector fakeGateRowConnector) error {
		db := sql.OpenDB(connector)
		defer db.Close()
		return shardTx(context.Background(), db, func(tx *sql.Tx) error {
			return requireGateOpenTx(context.Background(), tx)
		})
	}
	require.NoError(t, run(fakeGateRowConnector{writable: 1}))
	require.ErrorIs(t, run(fakeGateRowConnector{writable: 0}), authoritysql.ErrWriterFrozen)
	require.True(t, authoritysql.IsWriterFrozen(run(fakeGateRowConnector{writable: 0})))

	cause := errors.New("connection lost")
	err := run(fakeGateRowConnector{err: cause})
	require.ErrorIs(t, err, cause)
	require.False(t, authoritysql.IsWriterFrozen(err))
	require.Error(t, run(fakeGateRowConnector{missing: true}), "没有闸门行不是这个库的形状")
}
