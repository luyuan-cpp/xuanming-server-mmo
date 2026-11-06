package sqlpoolstats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

// unusedConnector 让单测拿到一个真的 *sql.DB 而不连任何库:sql.OpenDB 不拨号,Stats() 只读内存里的计数。
type unusedConnector struct{}

func (unusedConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("unit test connector never dials")
}
func (unusedConnector) Driver() driver.Driver { return unusedDriver{} }

type unusedDriver struct{}

func (unusedDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("unit test driver never dials")
}

// Single:db 为 nil 时不报样本,声明的类别照常输出 pools=0;有 db 时恰好报一条,类别与池上限原样带出。
func TestSingleReportsOnePoolOrNone(t *testing.T) {
	if got := Single(PrimaryPool, nil)(); len(got) != 0 {
		t.Fatalf("Single(nil)() = %+v, want no samples", got)
	}
	empty := gatherPools(t, NewCollector(testService, []string{PrimaryPool}, Single(PrimaryPool, nil)))
	if v, ok := empty["mmorpg_sql_pool_pools"][PrimaryPool]; !ok || v != 0 {
		t.Fatalf("mmorpg_sql_pool_pools{pool=%q} with nil db = %v (present=%v), want 0", PrimaryPool, v, ok)
	}

	db := sql.OpenDB(unusedConnector{})
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(7)

	got := Single(PrimaryPool, db)()
	if len(got) != 1 || got[0].Pool != PrimaryPool || got[0].Stats.MaxOpenConnections != 7 {
		t.Fatalf("Single(db)() = %+v, want exactly one %q sample with MaxOpenConnections=7", got, PrimaryPool)
	}
	gathered := gatherPools(t, NewCollector(testService, []string{PrimaryPool}, Single(PrimaryPool, db)))
	if v := gathered["mmorpg_sql_pool_pools"][PrimaryPool]; v != 1 {
		t.Fatalf("mmorpg_sql_pool_pools{pool=%q} = %v, want 1", PrimaryPool, v)
	}
	if v := gathered["mmorpg_sql_pool_max_open_connections"][PrimaryPool]; v != 7 {
		t.Fatalf("mmorpg_sql_pool_max_open_connections{pool=%q} = %v, want 7", PrimaryPool, v)
	}
}
