package placement

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// roleRouteRow 是假库里一行 player_route。
type roleRouteRow struct {
	home   any // nil = NULL
	rev    int64
	frozen any // nil = NULL
}

// fakeRoleRouteDB 只认 LockRoleRoutes 发的两条点查:按 player_id 回 routes 里的行,记下每条语句。
// beforeLock 非 nil 时在每条 FOR UPDATE 执行前调用,用来模拟预读之后路由行被改。
type fakeRoleRouteDB struct {
	mu         sync.Mutex
	routes     map[uint64]roleRouteRow
	queries    []string
	locked     []uint64
	beforeLock func(player uint64)
}

func (f *fakeRoleRouteDB) Connect(context.Context) (driver.Conn, error) {
	return &fakeRoleRouteConn{f}, nil
}
func (f *fakeRoleRouteDB) Driver() driver.Driver { return nil }

type fakeRoleRouteConn struct{ db *fakeRoleRouteDB }

func (c *fakeRoleRouteConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *fakeRoleRouteConn) Close() error              { return nil }
func (c *fakeRoleRouteConn) Begin() (driver.Tx, error) { return fakeRoleRouteTx{}, nil }

type fakeRoleRouteTx struct{}

func (fakeRoleRouteTx) Commit() error   { return nil }
func (fakeRoleRouteTx) Rollback() error { return nil }

func (c *fakeRoleRouteConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	if len(args) != 2 {
		return nil, fmt.Errorf("%d args", len(args))
	}
	player := uint64(args[1].Value.(int64))
	if uint32(args[0].Value.(int64)) != RouteRowShard(player) {
		return nil, fmt.Errorf("player %d bound shard %v", player, args[0].Value)
	}
	if strings.HasSuffix(query, "FOR UPDATE") {
		f.locked = append(f.locked, player)
		if f.beforeLock != nil {
			f.beforeLock(player)
		}
	}
	row, ok := f.routes[player]
	return &fakeRoleRouteRows{row: row, left: ok}, nil
}

type fakeRoleRouteRows struct {
	row  roleRouteRow
	left bool
}

func (r *fakeRoleRouteRows) Columns() []string { return []string{"home_zone", "rev", "frozen_run_id"} }
func (r *fakeRoleRouteRows) Close() error      { return nil }
func (r *fakeRoleRouteRows) Next(dest []driver.Value) error {
	if !r.left {
		return io.EOF
	}
	r.left = false
	dest[0], dest[1], dest[2] = r.row.home, r.row.rev, r.row.frozen
	return nil
}

func lockRoleRoutesOn(t *testing.T, f *fakeRoleRouteDB, players []uint64) error {
	t.Helper()
	db := sql.OpenDB(f)
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	return LockRoleRoutes(context.Background(), tx, "mmorpg_global", players)
}

// 每名玩家只锁自己的路由行:预读完整集合、按聚簇键 (route_shard, player_id) 升序 FOR UPDATE,不碰任何共享行。
func TestLockRoleRoutes_LocksOnlyEachPlayersOwnRowInClusterOrder(t *testing.T) {
	f := &fakeRoleRouteDB{routes: map[uint64]roleRouteRow{
		1:  {home: int64(5), rev: 1}, // 片 1
		65: {home: int64(5), rev: 2}, // 片 1
		66: {home: int64(6), rev: 3}, // 片 2
	}}
	if err := lockRoleRoutesOn(t, f, []uint64{66, 65, 1, 65}); err != nil {
		t.Fatalf("LockRoleRoutes: %v", err)
	}
	if fmt.Sprint(f.locked) != "[1 65 66]" {
		t.Fatalf("locked in order %v, want [1 65 66] (cluster key order, deduplicated)", f.locked)
	}
	if len(f.queries) != 6 {
		t.Fatalf("issued %d statements, want 3 pre-reads + 3 locks: %q", len(f.queries), f.queries)
	}
	for i, q := range f.queries {
		if !strings.Contains(q, "`mmorpg_global`.player_route") {
			t.Fatalf("statement %d touches something other than the player's route row: %s", i, q)
		}
		if lock := strings.HasSuffix(q, "FOR UPDATE"); lock != (i >= 3) {
			t.Fatalf("statement %d lock=%t: every pre-read must come before every lock: %q", i, lock, f.queries)
		}
	}
}

// 合服不改路由行,所以源区玩家照常通过;这里不看任何合服状态。
func TestLockRoleRoutes_Rejections(t *testing.T) {
	boom := sql.ErrNoRows
	for _, tc := range []struct {
		name       string
		routes     map[uint64]roleRouteRow
		players    []uint64
		beforeLock func(f *fakeRoleRouteDB, player uint64)
		want       error
	}{
		{name: "空集合", players: nil, want: ErrRoleRouteFenced},
		{name: "player 0", players: []uint64{0}, want: ErrRoleRouteFenced},
		{name: "没有行是 SQL 错误原样返回", players: []uint64{7}, routes: map[uint64]roleRouteRow{}, want: boom},
		{name: "home 为 NULL", players: []uint64{7}, routes: map[uint64]roleRouteRow{7: {rev: 1}}, want: ErrRoleRouteFenced},
		{name: "rev 为 0", players: []uint64{7}, routes: map[uint64]roleRouteRow{7: {home: int64(5)}}, want: ErrRoleRouteFenced},
		{name: "预读时已冻结", players: []uint64{7}, routes: map[uint64]roleRouteRow{7: {home: int64(5), rev: 1, frozen: "run"}}, want: ErrRoleRouteFenced},
		{
			name: "预读之后被冻结", players: []uint64{7}, routes: map[uint64]roleRouteRow{7: {home: int64(5), rev: 1}},
			beforeLock: func(f *fakeRoleRouteDB, p uint64) { f.routes[p] = roleRouteRow{home: int64(5), rev: 2, frozen: "run"} },
			want:       ErrRoleRouteFenced,
		},
		{
			name: "预读之后 rev 变了", players: []uint64{7}, routes: map[uint64]roleRouteRow{7: {home: int64(5), rev: 1}},
			beforeLock: func(f *fakeRoleRouteDB, p uint64) { f.routes[p] = roleRouteRow{home: int64(5), rev: 2} },
			want:       ErrRoleRouteFenced,
		},
		{
			name: "预读之后 home 变了", players: []uint64{7}, routes: map[uint64]roleRouteRow{7: {home: int64(5), rev: 1}},
			beforeLock: func(f *fakeRoleRouteDB, p uint64) { f.routes[p] = roleRouteRow{home: int64(6), rev: 1} },
			want:       ErrRoleRouteFenced,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRoleRouteDB{routes: tc.routes}
			if tc.beforeLock != nil {
				f.beforeLock = func(p uint64) { tc.beforeLock(f, p) }
			}
			if err := lockRoleRoutesOn(t, f, tc.players); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// CheckRoleRoute 只发一条不加锁的点查,判据与 LockRoleRoutes 同一份。
func TestCheckRoleRoute_PlainReadWithSameVerdicts(t *testing.T) {
	boom := sql.ErrNoRows
	for _, tc := range []struct {
		name   string
		routes map[uint64]roleRouteRow
		want   error
	}{
		{name: "放行", routes: map[uint64]roleRouteRow{7: {home: int64(5), rev: 1}}},
		{name: "没有行是 SQL 错误原样返回", routes: map[uint64]roleRouteRow{}, want: boom},
		{name: "home 为 NULL", routes: map[uint64]roleRouteRow{7: {rev: 1}}, want: ErrRoleRouteFenced},
		{name: "rev 为 0", routes: map[uint64]roleRouteRow{7: {home: int64(5)}}, want: ErrRoleRouteFenced},
		{name: "已冻结", routes: map[uint64]roleRouteRow{7: {home: int64(5), rev: 1, frozen: "run"}}, want: ErrRoleRouteFenced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRoleRouteDB{routes: tc.routes}
			db := sql.OpenDB(f)
			t.Cleanup(func() { _ = db.Close() })
			if err := CheckRoleRoute(context.Background(), db, "mmorpg_global", 7); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(f.queries) != 1 || strings.HasSuffix(f.queries[0], "FOR UPDATE") || len(f.locked) != 0 {
				t.Fatalf("必须恰好一条不加锁的点查: %q", f.queries)
			}
		})
	}
}

func TestCheckRoleRoute_RejectsBadInputBeforeAnyStatement(t *testing.T) {
	f := &fakeRoleRouteDB{routes: map[uint64]roleRouteRow{7: {home: int64(5), rev: 1}}}
	db := sql.OpenDB(f)
	t.Cleanup(func() { _ = db.Close() })
	if err := CheckRoleRoute(context.Background(), db, "bad`schema", 7); !errors.Is(err, ErrRoleRouteFenced) {
		t.Fatalf("坏 schema: %v", err)
	}
	if err := CheckRoleRoute(context.Background(), db, "mmorpg_global", 0); !errors.Is(err, ErrRoleRouteFenced) {
		t.Fatalf("player 0: %v", err)
	}
	if len(f.queries) != 0 {
		t.Fatalf("issued %q", f.queries)
	}
}

func TestLockRoleRoutes_RejectsBadSchemaBeforeAnyStatement(t *testing.T) {
	f := &fakeRoleRouteDB{routes: map[uint64]roleRouteRow{7: {home: int64(5), rev: 1}}}
	db := sql.OpenDB(f)
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := LockRoleRoutes(context.Background(), tx, "bad`schema", []uint64{7}); !errors.Is(err, ErrRoleRouteFenced) {
		t.Fatalf("err = %v, want ErrRoleRouteFenced", err)
	}
	if len(f.queries) != 0 {
		t.Fatalf("issued %q", f.queries)
	}
}
