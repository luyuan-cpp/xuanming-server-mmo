package playerstore

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"db/internal/dbtest"
)

type fakeRouteReader struct {
	route storageRoute
	found bool
	err   error
}

func (f fakeRouteReader) readRoute(context.Context, uint64) (storageRoute, bool, error) {
	return f.route, f.found, f.err
}

// 选库的五种结局。读错不能被当成没有落点:没有落点要查数据,读错只要等库恢复。
func TestResolveOutcomes(t *testing.T) {
	readFailure := errors.New("Error 2013: lost connection")
	cases := []struct {
		name    string
		reader  fakeRouteReader
		want    uint32
		wantErr error
	}{
		{name: "路由读到", reader: fakeRouteReader{route: storageRoute{storageID: 7}, found: true}, want: 7},
		{name: "非 zone 落点", reader: fakeRouteReader{route: storageRoute{storageID: 1000000}, found: true}, want: 1000000},
		{name: "没有路由行", reader: fakeRouteReader{}, wantErr: ErrPlayerStorageMissing},
		{name: "storage_id 为 NULL", reader: fakeRouteReader{found: true}, wantErr: ErrPlayerStorageMissing},
		{name: "搬库冻结", reader: fakeRouteReader{route: storageRoute{storageID: 7, frozen: true}, found: true}, wantErr: ErrStorageFrozen},
		{name: "SQL 读错", reader: fakeRouteReader{err: readFailure}, wantErr: readFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&storageRouter{routes: tc.reader}).resolve(context.Background(), 42)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("storage = %d, want %d", got, tc.want)
			}
			if tc.reader.err != nil && errors.Is(err, ErrPlayerStorageMissing) {
				t.Fatalf("a read failure must not look like a missing placement: %v", err)
			}
		})
	}
}

func routeRow(storage, frozen driver.Value) *dbtest.Rows {
	return &dbtest.Rows{Columns: []string{"storage_id", "frozen_run_id"}, Values: [][]driver.Value{{storage, frozen}}}
}

// 真实的读路径:按聚簇键点查、库名限定、不加锁;NULL 列和无行都走到对的结局。
func TestNewStorageResolverReadsGlobalRouteRow(t *testing.T) {
	const player = uint64(130) // route_shard = 130 % 64 = 2
	var query string
	var args []driver.NamedValue
	db, rec := dbtest.Open(t, func(_ context.Context, q string, a []driver.NamedValue) (*dbtest.Rows, error) {
		query, args = q, a
		return routeRow(int64(7), nil), nil
	})
	resolve, err := NewStorageResolver(ResolverOptions{DB: db, RouteSchema: "mmorpg_global"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolve(context.Background(), player)
	if err != nil || got != 7 {
		t.Fatalf("resolve = %d, %v", got, err)
	}
	const wantQuery = "SELECT storage_id, frozen_run_id FROM `mmorpg_global`.`player_route` WHERE route_shard = ? AND player_id = ?"
	if query != wantQuery {
		t.Fatalf("query = %q", query)
	}
	if len(args) != 2 || args[0].Value != int64(2) || args[1].Value != int64(player) {
		t.Fatalf("args = %+v", args)
	}
	if got := rec.Statements(); len(got) != 1 {
		t.Fatalf("a claim reads the route exactly once: %v", got)
	}
}

func TestNewStorageResolverReadOutcomes(t *testing.T) {
	sqlFailure := errors.New("Error 1142: SELECT command denied")
	cases := []struct {
		name    string
		rows    *dbtest.Rows
		err     error
		want    uint32
		wantErr error
	}{
		{name: "路由读到", rows: routeRow(int64(1000000), nil), want: 1000000},
		{name: "没有路由行", rows: &dbtest.Rows{Columns: []string{"storage_id", "frozen_run_id"}}, wantErr: ErrPlayerStorageMissing},
		{name: "storage_id 为 NULL", rows: routeRow(nil, nil), wantErr: ErrPlayerStorageMissing},
		{name: "搬库冻结", rows: routeRow(int64(7), "run-1"), wantErr: ErrStorageFrozen},
		{name: "SQL 读错", err: sqlFailure, wantErr: sqlFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := dbtest.Open(t, func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) {
				return tc.rows, tc.err
			})
			resolve, err := NewStorageResolver(ResolverOptions{DB: db, RouteSchema: "mmorpg_global"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolve(context.Background(), 42)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("storage = %d, want %d", got, tc.want)
			}
		})
	}
}

// 列值读不出来也是读错,不能当成没有落点。
func TestNewStorageResolverUnreadableColumnIsAReadFailure(t *testing.T) {
	db, _ := dbtest.Open(t, func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) {
		return routeRow("not-a-number", nil), nil
	})
	resolve, err := NewStorageResolver(ResolverOptions{DB: db, RouteSchema: "mmorpg_global"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(context.Background(), 42); err == nil || errors.Is(err, ErrPlayerStorageMissing) {
		t.Fatalf("err = %v, want a read failure", err)
	}
}

func TestNewStorageResolverRejectsBadConfig(t *testing.T) {
	if _, err := NewStorageResolver(ResolverOptions{RouteSchema: "mmorpg_global"}); err == nil {
		t.Fatal("a nil pool must be rejected")
	}
	db, _ := dbtest.Open(t, noQuery(t))
	for _, schema := range []string{"", "bad`schema", "global db", "1global"} {
		if _, err := NewStorageResolver(ResolverOptions{DB: db, RouteSchema: schema}); err == nil {
			t.Fatalf("schema %q must be rejected", schema)
		}
	}
}
