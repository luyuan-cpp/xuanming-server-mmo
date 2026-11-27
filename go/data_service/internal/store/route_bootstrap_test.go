package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeRouteRowsConnector 只回答「路由表有没有行」那条读:err 非空回错误,否则按 present 回一行或空。
type fakeRouteRowsConnector struct {
	present bool
	err     error
}

func (c fakeRouteRowsConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeRouteRowsConn{c}, nil
}
func (c fakeRouteRowsConnector) Driver() driver.Driver { return fakeNameDriver{} }

type fakeRouteRowsConn struct{ fakeRouteRowsConnector }

func (fakeRouteRowsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("不支持 Prepare")
}
func (fakeRouteRowsConn) Close() error              { return nil }
func (fakeRouteRowsConn) Begin() (driver.Tx, error) { return nil, errors.New("不支持事务") }
func (c fakeRouteRowsConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query != "SELECT 1 FROM player_route LIMIT 1" {
		return nil, fmt.Errorf("未预期的查询 %q", query)
	}
	if c.err != nil {
		return nil, c.err
	}
	rows := &fakeNameRows{cols: []string{"1"}}
	if c.present {
		rows.rows = [][]driver.Value{{int64(1)}}
	}
	return rows, nil
}

func routeStoreOverShards(t *testing.T, shards ...fakeRouteRowsConnector) *RouteStore {
	t.Helper()
	pools := &shardPools{k: len(shards), names: make([]string, len(shards)), pools: make([]*sql.DB, len(shards))}
	for i, conn := range shards {
		pools.names[i] = fmt.Sprintf("mmorpg_global_%02d", i)
		db := sql.OpenDB(conn)
		t.Cleanup(func() { _ = db.Close() })
		pools.pools[i] = db
	}
	return &RouteStore{pools: pools}
}

// 空环境判据只问「路由表是不是全空」(docs/design/data-service-startup-isolation.md §4):任一片有行就不是;
// 常驻进程里关着的片读不了,先跳过;一行都没找到而又有片读不了时,分不清「全空」和「没读到」,回错误。
func TestHasAnyRouteSkipsUnreadableShards(t *testing.T) {
	ctx := context.Background()
	closed := errors.New("logical database is not open")

	found, err := routeStoreOverShards(t, fakeRouteRowsConnector{err: closed}, fakeRouteRowsConnector{present: true}).HasAnyRoute(ctx)
	require.NoError(t, err)
	require.True(t, found, "别的片有行就不是空环境,读不了的片不影响结论")

	found, err = routeStoreOverShards(t, fakeRouteRowsConnector{}, fakeRouteRowsConnector{}).HasAnyRoute(ctx)
	require.NoError(t, err)
	require.False(t, found)

	_, err = routeStoreOverShards(t, fakeRouteRowsConnector{}, fakeRouteRowsConnector{err: closed}).HasAnyRoute(ctx)
	require.ErrorIs(t, err, closed, "没找到行又有片读不了,不能当成空环境")
	require.ErrorContains(t, err, "mmorpg_global_01")
}
