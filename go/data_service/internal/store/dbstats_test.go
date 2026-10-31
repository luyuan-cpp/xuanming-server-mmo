package store

import (
	"database/sql"
	"testing"
)

// TestDBStatsOfReportsEachStorePool 钉住 DBStatsOf 的封闭集合:五种 store 各报自己那个池,
// nil 指针、没有连接池的 store、未知类型一律 ok=false。新增 store 时在 DBStatsOf 与这张表各加一行。
func TestDBStatsOfReportsEachStorePool(t *testing.T) {
	// sql.Open 不拨号:端口 1 上没有服务也无所谓,这里只读池的静态参数。
	pool := func(maxOpen int) *sql.DB {
		t.Helper()
		db, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/x")
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		db.SetMaxOpenConns(maxOpen)
		return db
	}

	cases := []struct {
		name        string
		store       any
		wantOK      bool
		wantMaxOpen int
	}{
		{name: "snapshot", store: &SnapshotStore{db: pool(11)}, wantOK: true, wantMaxOpen: 11},
		{name: "transaction_log", store: &TransactionLogStore{db: pool(12)}, wantOK: true, wantMaxOpen: 12},
		{name: "player_name", store: &PlayerNameStore{db: pool(14)}, wantOK: true, wantMaxOpen: 14},
		{name: "player_route", store: &RouteStore{db: pool(15)}, wantOK: true, wantMaxOpen: 15},
		{name: "nil snapshot pointer", store: (*SnapshotStore)(nil)},
		{name: "nil route pointer", store: (*RouteStore)(nil)},
		{name: "store without a pool", store: &TransactionLogStore{}},
		{name: "untyped nil", store: nil},
		{name: "unknown type", store: struct{}{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats, ok := DBStatsOf(tc.store)
			if ok != tc.wantOK {
				t.Fatalf("DBStatsOf ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && stats.MaxOpenConnections != tc.wantMaxOpen {
				t.Fatalf("MaxOpenConnections = %d, want %d", stats.MaxOpenConnections, tc.wantMaxOpen)
			}
		})
	}
}
