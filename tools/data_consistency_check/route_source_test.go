package main

// route_source.go 与 checkFriendOrphans 的单测。本 module 只依赖 go-sql-driver/mysql 与 go-redis,没有 sqlmock:
// dbRouteSource 的循环经 routeTable 假实现驱动,sqlRouteTable 的语句与参数经下面注册的最小 database/sql 假驱动核对。
// 真库语义见 route_source_integration_test.go。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ── 假 routeTable(驱动 dbRouteSource 的循环)─────────────────────

// fakeRouteTable:homes 是 player_id → home_zone(nil 指针 = NULL);next 非 nil 时覆盖默认实现;
// countErr 是 CountWithHome 返回的错误,errOn 是 HomeOf 返回的错误;countCalls 记 CountWithHome 的调用次数。
type fakeRouteTable struct {
	homes map[uint64]*uint32

	next     func(from uint64) (uint32, bool, error)
	countErr error
	errOn    error

	countCalls int
}

func zonePtr(z uint32) *uint32 {
	return &z
}

func (f *fakeRouteTable) NextHomeZone(_ context.Context, from uint64) (uint32, bool, error) {
	if f.next != nil {
		return f.next(from)
	}
	best, found := uint32(0), false
	for _, h := range f.homes {
		if h == nil || uint64(*h) < from {
			continue
		}
		if !found || *h < best {
			best, found = *h, true
		}
	}
	return best, found, nil
}

func (f *fakeRouteTable) CountWithHome(context.Context) (int64, error) {
	f.countCalls++
	if f.countErr != nil {
		return 0, f.countErr
	}
	var n int64
	for _, h := range f.homes {
		if h != nil {
			n++
		}
	}
	return n, nil
}

func (f *fakeRouteTable) HomeOf(_ context.Context, playerID uint64) (uint32, bool, error) {
	if f.errOn != nil {
		return 0, false, f.errOn
	}
	h, ok := f.homes[playerID]
	if !ok || h == nil {
		return 0, false, nil
	}
	return *h, true, nil
}

// TestDBRouteSourceLiveZonesAndCount:越界 zone 计畸形、不进活区但玩家照样计数;NULL 归属不计;
// 玩家数是一条计数查询(不按 zone 逐页拉主键)。
func TestDBRouteSourceLiveZonesAndCount(t *testing.T) {
	const zone3Players = 25
	homes := map[uint64]*uint32{}
	for i := uint64(1); i <= zone3Players; i++ {
		homes[i] = zonePtr(3)
	}
	homes[900001] = zonePtr(5)
	homes[900002] = zonePtr(0)
	homes[900003] = zonePtr(maxHomeZone + 1)
	homes[900004] = nil
	homes[900005] = nil
	f := &fakeRouteTable{homes: homes}
	src := dbRouteSource{t: f, dbName: "testdb"}

	zones, rep, err := src.LiveZones(context.Background())
	if err != nil {
		t.Fatalf("LiveZones: %v", err)
	}
	if fmt.Sprint(sortedKeys(zones)) != "[3 5]" {
		t.Fatalf("zones = %v, want [3 5]", sortedKeys(zones))
	}
	if rep.Scanned != 4 || rep.Malformed != 2 {
		t.Fatalf("report = %+v, want scanned=4 malformed=2", rep)
	}
	if got := strings.Join(rep.Samples, ","); got != "home_zone=0,home_zone=1000000" {
		t.Fatalf("samples = %q", got)
	}
	if res := shapeCheckResult(rep); res.Severity != "block" || res.BadCount != 2 || res.TotalCount != 4 {
		t.Fatalf("shape result = %+v, want block 2/4", res)
	}

	n, err := src.CountPlayers(context.Background())
	if err != nil {
		t.Fatalf("CountPlayers: %v", err)
	}
	// zone 3 的 25 个 + zone 5 + 越界的 zone 0 / 1000000,NULL 两个不计。
	if want := int64(zone3Players + 3); n != want {
		t.Fatalf("CountPlayers = %d, want %d", n, want)
	}
	if f.countCalls != 1 {
		t.Fatalf("CountWithHome calls = %d, want 1 (one indexed count, no per-zone paging)", f.countCalls)
	}
	if !strings.Contains(src.Describe(), `"testdb"`) || !strings.Contains(src.Describe(), "route source: db") {
		t.Fatalf("Describe() = %q", src.Describe())
	}
}

func TestDBLiveZonesSampleLimit(t *testing.T) {
	homes := map[uint64]*uint32{}
	for i := 0; i < shapeSampleLimit+5; i++ {
		homes[uint64(i+1)] = zonePtr(maxHomeZone + 1 + uint32(i))
	}
	_, rep, err := dbRouteSource{t: &fakeRouteTable{homes: homes}}.LiveZones(context.Background())
	if err != nil {
		t.Fatalf("LiveZones: %v", err)
	}
	if rep.Malformed != int64(shapeSampleLimit+5) || len(rep.Samples) != shapeSampleLimit {
		t.Fatalf("malformed=%d samples=%d, want %d / %d", rep.Malformed, len(rep.Samples), shapeSampleLimit+5, shapeSampleLimit)
	}
}

func TestDBLiveZonesCleanIsInfo(t *testing.T) {
	_, rep, err := dbRouteSource{t: &fakeRouteTable{homes: map[uint64]*uint32{1: zonePtr(2)}}}.LiveZones(context.Background())
	if err != nil {
		t.Fatalf("LiveZones: %v", err)
	}
	if res := shapeCheckResult(rep); res.Severity != "info" || res.BadCount != 0 {
		t.Fatalf("shape result = %+v, want info", res)
	}
}

// TestDBRouteSourceRejectsBrokenOrdering:home_zone 游标不前进时报错,不死循环。
func TestDBRouteSourceRejectsBrokenOrdering(t *testing.T) {
	stuck := &fakeRouteTable{next: func(uint64) (uint32, bool, error) { return 3, true, nil }}
	if _, _, err := (dbRouteSource{t: stuck}).LiveZones(context.Background()); err == nil {
		t.Fatal("home_zone 不递增必须报错")
	}
}

func TestDBRouteSourceErrorsAreErrors(t *testing.T) {
	boom := errors.New("Error 1045: access denied")
	nextErr := &fakeRouteTable{next: func(uint64) (uint32, bool, error) { return 0, false, boom }}
	if _, _, err := (dbRouteSource{t: nextErr}).LiveZones(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("LiveZones err = %v", err)
	}
	countErr := &fakeRouteTable{homes: map[uint64]*uint32{1: zonePtr(3)}, countErr: boom}
	if n, err := (dbRouteSource{t: countErr}).CountPlayers(context.Background()); !errors.Is(err, boom) || n != 0 {
		t.Fatalf("CountPlayers = %d, %v; want 0 + %v", n, err, boom)
	}
	homeErr := &fakeRouteTable{errOn: boom}
	if has, err := (dbRouteSource{t: homeErr}).HasHome(context.Background(), 1); !errors.Is(err, boom) || has {
		t.Fatalf("HasHome = %v, %v", has, err)
	}
}

func TestDBRouteSourceHasHome(t *testing.T) {
	src := dbRouteSource{t: &fakeRouteTable{homes: map[uint64]*uint32{1: zonePtr(3), 2: nil}}}
	for id, want := range map[uint64]bool{1: true, 2: false, 3: false} {
		has, err := src.HasHome(context.Background(), id)
		if err != nil || has != want {
			t.Errorf("HasHome(%d) = %v, %v; want %v", id, has, err, want)
		}
	}
}

// ── 最小 database/sql 假驱动 ──────────────────────────────────────
//
// 每个 DSN 对应一个 fakeSQLServer;连接实现 QueryerContext,database/sql 直接把语句与(已转换的)参数交给 handle。

type fakeSQLQuery struct {
	query string
	args  []any
}

type fakeSQLServer struct {
	mu      sync.Mutex
	handle  func(query string, args []any) (cols []string, rows [][]driver.Value, err error)
	queries []fakeSQLQuery
}

// fakeSQLServers:dsn → *fakeSQLServer。
var fakeSQLServers sync.Map

var fakeSQLRegister sync.Once

const fakeSQLDriverTag = "dcc_fake_sql"

type fakeSQLDriver struct{}

func (fakeSQLDriver) Open(name string) (driver.Conn, error) {
	v, ok := fakeSQLServers.Load(name)
	if !ok {
		return nil, fmt.Errorf("fake sql: unknown dsn %q", name)
	}
	return &fakeSQLConn{srv: v.(*fakeSQLServer)}, nil
}

type fakeSQLConn struct {
	srv *fakeSQLServer
}

func (c *fakeSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake sql: prepare not supported")
}

func (c *fakeSQLConn) Close() error {
	return nil
}

func (c *fakeSQLConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fake sql: tx not supported")
}

func (c *fakeSQLConn) QueryContext(_ context.Context, query string, named []driver.NamedValue) (driver.Rows, error) {
	args := make([]any, len(named))
	for i, nv := range named {
		args[i] = nv.Value
	}
	c.srv.mu.Lock()
	c.srv.queries = append(c.srv.queries, fakeSQLQuery{query: query, args: args})
	c.srv.mu.Unlock()
	cols, rows, err := c.srv.handle(query, args)
	if err != nil {
		return nil, err
	}
	return &fakeSQLRows{cols: cols, rows: rows}, nil
}

type fakeSQLRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeSQLRows) Columns() []string {
	return r.cols
}

func (r *fakeSQLRows) Close() error {
	return nil
}

func (r *fakeSQLRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// openFakeSQL 起一个假库,handle 按语句回结果。
func openFakeSQL(t *testing.T, handle func(query string, args []any) ([]string, [][]driver.Value, error)) (*sql.DB, *fakeSQLServer) {
	t.Helper()
	fakeSQLRegister.Do(func() { sql.Register(fakeSQLDriverTag, fakeSQLDriver{}) })
	srv := &fakeSQLServer{handle: handle}
	dsn := t.Name()
	fakeSQLServers.Store(dsn, srv)
	t.Cleanup(func() { fakeSQLServers.Delete(dsn) })
	db, err := sql.Open(fakeSQLDriverTag, dsn)
	if err != nil {
		t.Fatalf("open fake sql: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, srv
}

// TestSQLRouteTableStatementsAndArgs:三条语句与参数顺序(含点查的行片 = player_id % 64)、NULL 与无行的口径。
func TestSQLRouteTableStatementsAndArgs(t *testing.T) {
	boom := errors.New("Error 2013: lost connection")
	var failNext, failCount bool
	db, srv := openFakeSQL(t, func(query string, args []any) ([]string, [][]driver.Value, error) {
		switch query {
		case sqlNextHomeZone:
			if failNext {
				return nil, nil, boom
			}
			if args[0] == int64(0) {
				return []string{"home_zone"}, [][]driver.Value{{int64(3)}}, nil
			}
			return []string{"home_zone"}, nil, nil
		case sqlCountWithHome:
			if failCount {
				return nil, nil, boom
			}
			return []string{"COUNT(*)"}, [][]driver.Value{{int64(7)}}, nil
		case sqlHomeOf:
			switch args[1] {
			case int64(130):
				return []string{"home_zone"}, [][]driver.Value{{int64(9)}}, nil
			case int64(131):
				return []string{"home_zone"}, [][]driver.Value{{nil}}, nil
			case int64(132):
				return nil, nil, boom
			}
			return []string{"home_zone"}, nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected query %q", query)
	})
	tbl := sqlRouteTable{db: db}
	ctx := context.Background()

	if z, ok, err := tbl.NextHomeZone(ctx, 0); err != nil || !ok || z != 3 {
		t.Fatalf("NextHomeZone(0) = %d, %v, %v", z, ok, err)
	}
	if _, ok, err := tbl.NextHomeZone(ctx, 4); err != nil || ok {
		t.Fatalf("NextHomeZone(4) ok=%v err=%v, want no rows", ok, err)
	}
	failNext = true
	if _, _, err := tbl.NextHomeZone(ctx, 4); !errors.Is(err, boom) {
		t.Fatalf("NextHomeZone error = %v", err)
	}

	if n, err := tbl.CountWithHome(ctx); err != nil || n != 7 {
		t.Fatalf("CountWithHome = %d, %v; want 7", n, err)
	}
	failCount = true
	if n, err := tbl.CountWithHome(ctx); !errors.Is(err, boom) || n != 0 {
		t.Fatalf("CountWithHome error = %d, %v; want 0 + %v", n, err, boom)
	}

	if z, ok, err := tbl.HomeOf(ctx, 130); err != nil || !ok || z != 9 {
		t.Fatalf("HomeOf(130) = %d, %v, %v", z, ok, err)
	}
	if _, ok, err := tbl.HomeOf(ctx, 131); err != nil || ok {
		t.Fatalf("HomeOf(131) NULL home: ok=%v err=%v, want absent", ok, err)
	}
	if _, ok, err := tbl.HomeOf(ctx, 129); err != nil || ok {
		t.Fatalf("HomeOf(129) no row: ok=%v err=%v, want absent", ok, err)
	}
	if _, _, err := tbl.HomeOf(ctx, 132); !errors.Is(err, boom) {
		t.Fatalf("HomeOf(132) error = %v", err)
	}

	want := []fakeSQLQuery{
		{sqlNextHomeZone, []any{int64(0)}},
		{sqlNextHomeZone, []any{int64(4)}},
		{sqlNextHomeZone, []any{int64(4)}},
		{sqlCountWithHome, []any{}},
		{sqlCountWithHome, []any{}},
		{sqlHomeOf, []any{int64(130 % 64), int64(130)}},
		{sqlHomeOf, []any{int64(131 % 64), int64(131)}},
		{sqlHomeOf, []any{int64(129 % 64), int64(129)}},
		{sqlHomeOf, []any{int64(132 % 64), int64(132)}},
	}
	if fmt.Sprint(srv.queries) != fmt.Sprint(want) {
		t.Fatalf("queries =\n%v\nwant\n%v", srv.queries, want)
	}
}

// TestSQLStatementsFollowReadTemplate:语句形状钉住提案 §11.2 G1a 的读方模板(活区在 idx_route_home 上跳取、玩家数是
// 一条范围计数、点查带行片),改语句时要同时想清楚索引。
func TestSQLStatementsFollowReadTemplate(t *testing.T) {
	if sqlCountWithHome != "SELECT COUNT(*) FROM player_route WHERE home_zone IS NOT NULL" {
		t.Errorf("sqlCountWithHome %q must stay a single count over idx_route_home (no paging, no GROUP BY)", sqlCountWithHome)
	}
	if !strings.Contains(sqlNextHomeZone, "WHERE home_zone >= ? ORDER BY home_zone LIMIT 1") {
		t.Errorf("sqlNextHomeZone %q is not a skip scan on idx_route_home", sqlNextHomeZone)
	}
	if !strings.Contains(sqlHomeOf, "WHERE route_shard = ? AND player_id = ?") {
		t.Errorf("sqlHomeOf %q must be a primary key point read", sqlHomeOf)
	}
	for _, q := range []string{sqlNextHomeZone, sqlCountWithHome, sqlHomeOf} {
		if strings.Contains(strings.ToUpper(q), "FOR UPDATE") || strings.Contains(strings.ToUpper(q), "DISTINCT") {
			t.Errorf("read-only tool statement %q must not lock or use DISTINCT", q)
		}
	}
}

// TestMirroredRouteConstantsMatchSources:routeRowShards / maxHomeZone 与源头逐字一致,索引形状仍是语句所依赖的那样。
// 源头:go/shared/placement/route.go 的 RouteRowShards,go/data_service/internal/store/schema.go 的 player_route DDL。
func TestMirroredRouteConstantsMatchSources(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../../go/shared/placement/route.go", nil, 0)
	if err != nil {
		t.Fatalf("parse shared/placement/route.go: %v", err)
	}
	found := ""
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if name.Name == "RouteRowShards" && i < len(spec.Values) {
				if lit, ok := spec.Values[i].(*ast.BasicLit); ok {
					found = lit.Value
				}
			}
		}
		return true
	})
	if found != strconv.FormatUint(routeRowShards, 10) {
		t.Fatalf("placement.RouteRowShards = %q, mirrored routeRowShards = %d", found, routeRowShards)
	}

	schema, err := os.ReadFile("../../go/data_service/internal/store/schema.go")
	if err != nil {
		t.Fatalf("read data_service store/schema.go: %v", err)
	}
	for _, want := range []string{
		"KEY `idx_route_home` (`home_zone`, `route_shard`, `player_id`)",
		"PRIMARY KEY (`route_shard`, `player_id`)",
		"CHECK (`route_shard` = `player_id` % " + strconv.FormatUint(routeRowShards, 10) + ")",
		"`home_zone` BETWEEN 1 AND " + strconv.FormatUint(uint64(maxHomeZone), 10) + ")",
	} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("player_route DDL no longer contains %q: update route_source.go together with it", want)
		}
	}
}

// ── checkFriendOrphans:归属查询失败整条判 fail ────────────────────

// fakeRoutes 是 routeSource 的最小假实现,给好友检查与玩家数检查用;count / countErr 是 CountPlayers 的回答。
type fakeRoutes struct {
	has      map[uint64]bool
	errOn    map[uint64]error
	called   []uint64
	count    int64
	countErr error
}

func (f *fakeRoutes) Describe() string {
	return "route source: fake"
}

func (f *fakeRoutes) LiveZones(context.Context) (map[uint32]struct{}, zoneShapeReport, error) {
	return nil, zoneShapeReport{}, errors.New("not used")
}

func (f *fakeRoutes) CountPlayers(context.Context) (int64, error) {
	return f.count, f.countErr
}

func (f *fakeRoutes) HasHome(_ context.Context, id uint64) (bool, error) {
	f.called = append(f.called, id)
	if err := f.errOn[id]; err != nil {
		return false, err
	}
	return f.has[id], nil
}

func friendDBWithSample(t *testing.T, pairs [][2]int64) *sql.DB {
	t.Helper()
	db, _ := openFakeSQL(t, func(query string, _ []any) ([]string, [][]driver.Value, error) {
		switch {
		case query == "SELECT COUNT(*) FROM friend":
			return []string{"n"}, [][]driver.Value{{int64(len(pairs))}}, nil
		case strings.HasPrefix(query, "SELECT player_id, friend_player_id FROM friend"):
			rows := make([][]driver.Value, len(pairs))
			for i, p := range pairs {
				rows[i] = []driver.Value{p[0], p[1]}
			}
			return []string{"player_id", "friend_player_id"}, rows, nil
		}
		return nil, nil, fmt.Errorf("unexpected query %q", query)
	})
	return db
}

// TestFriendOrphansRouteErrorFailsWholeCheck:以前 Exists(...).Val() 吞掉错误、把读失败数成孤儿;
// 现在读失败整条检查判 fail(进程退出 1),孤儿数不报。
func TestFriendOrphansRouteErrorFailsWholeCheck(t *testing.T) {
	routes := &fakeRoutes{
		has:   map[uint64]bool{11: false, 12: true},
		errOn: map[uint64]error{13: errors.New("i/o timeout")},
	}
	cfg := runConfig{friendDB: friendDBWithSample(t, [][2]int64{{1, 11}, {1, 12}, {1, 13}, {1, 14}}), routes: routes}

	r := checkFriendOrphans(context.Background(), cfg)
	if r.Severity != "fail" || r.BadCount != 0 {
		t.Fatalf("result = %+v, want fail with BadCount 0", r)
	}
	if !strings.Contains(r.SampleNotes, "friend_player_id=13") || !strings.Contains(r.SampleNotes, "i/o timeout") {
		t.Fatalf("notes = %q", r.SampleNotes)
	}
	if fmt.Sprint(routes.called) != "[11 12 13]" {
		t.Fatalf("lookups = %v, want stop at the first error", routes.called)
	}
	if n := printReport([]CheckResult{r}, "route source: fake"); n != 1 {
		t.Fatalf("printReport counted %d failing rows, want 1 (fail must exit non-zero)", n)
	}
}

func TestFriendOrphansCountsMissingHomes(t *testing.T) {
	routes := &fakeRoutes{has: map[uint64]bool{11: true, 12: true}}
	cfg := runConfig{friendDB: friendDBWithSample(t, [][2]int64{{1, 11}, {1, 12}, {1, 13}}), routes: routes}

	r := checkFriendOrphans(context.Background(), cfg)
	// 1/3 ≥ 5% → block。
	if r.Severity != "block" || r.BadCount != 1 {
		t.Fatalf("result = %+v, want block with 1 orphan", r)
	}
}

// TestFriendOrphansScanErrorFailsWholeCheck:样本行扫描失败整条检查判 fail(2026-10-20 之前 continue,样本静默变少)。
func TestFriendOrphansScanErrorFailsWholeCheck(t *testing.T) {
	db, _ := openFakeSQL(t, func(query string, _ []any) ([]string, [][]driver.Value, error) {
		switch {
		case query == "SELECT COUNT(*) FROM friend":
			return []string{"n"}, [][]driver.Value{{int64(2)}}, nil
		case strings.HasPrefix(query, "SELECT player_id, friend_player_id FROM friend"):
			return []string{"player_id", "friend_player_id"}, [][]driver.Value{{int64(1), int64(11)}, {int64(1), "not-a-number"}}, nil
		}
		return nil, nil, fmt.Errorf("unexpected query %q", query)
	})
	routes := &fakeRoutes{has: map[uint64]bool{11: true}}
	r := checkFriendOrphans(context.Background(), runConfig{friendDB: db, routes: routes})

	if r.Severity != "fail" || r.BadCount != 0 {
		t.Fatalf("result = %+v, want fail with BadCount 0", r)
	}
	if !strings.Contains(r.SampleNotes, "scan sample row after 1 sampled") {
		t.Fatalf("notes = %q", r.SampleNotes)
	}
	if fmt.Sprint(routes.called) != "[11]" {
		t.Fatalf("lookups = %v, want only the row before the bad one", routes.called)
	}
}

// ── checkPlayerHomeZoneVsAccount:玩家数读失败判 fail ──────────────

// TestPlayerHomeZoneCountFailureFailsCheck:CountPlayers 读失败判 fail、进程退出 1(2026-10-20 之前报 warn,
// 其余检查干净时打印 "all checks pass." 并退出 0);失败时不再去数 user_accounts(cfg.db 为 nil 也不能碰到)。
func TestPlayerHomeZoneCountFailureFailsCheck(t *testing.T) {
	routes := &fakeRoutes{countErr: errors.New("context deadline exceeded")}
	r := checkPlayerHomeZoneVsAccount(context.Background(), runConfig{routes: routes})

	if r.Severity != "fail" || r.BadCount != 0 || r.TotalCount != 0 {
		t.Fatalf("result = %+v, want fail with zero counts", r)
	}
	if !strings.Contains(r.SampleNotes, "CHECK FAILED") || !strings.Contains(r.SampleNotes, "context deadline exceeded") {
		t.Fatalf("notes = %q", r.SampleNotes)
	}
	if n := printReport([]CheckResult{r}, "route source: fake"); n != 1 {
		t.Fatalf("printReport counted %d failing rows, want 1 (a failed count must exit non-zero)", n)
	}
}

func TestPlayerHomeZoneUsesGlobalSQLRoleLinkage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		total, bad int64
		severity   string
	}{{"missing-or-tombstone", 12, 2, "block"}, {"linked", 10, 0, "info"}} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openFakeSQL(t, func(query string, _ []any) ([]string, [][]driver.Value, error) {
				if query != accountLinkageSQL {
					return nil, nil, fmt.Errorf("unexpected query %q", query)
				}
				return []string{"total", "bad"}, [][]driver.Value{{tc.total, tc.bad}}, nil
			})
			r := checkPlayerHomeZoneVsAccount(context.Background(), runConfig{accountDB: db, routes: &fakeRoutes{count: tc.total}})
			if r.Severity != tc.severity || r.BadCount != tc.bad || r.TotalCount != tc.total {
				t.Fatalf("result=%+v", r)
			}
		})
	}
}
func TestPlayerHomeZoneMissingGlobalHandleAndSQLFailureAreVisible(t *testing.T) {
	r := checkPlayerHomeZoneVsAccount(context.Background(), runConfig{routes: &fakeRoutes{}})
	if r.Severity != "warn" || !strings.Contains(r.SampleNotes, "NOT CHECKED") {
		t.Fatalf("missing global handle=%+v", r)
	}
	db, _ := openFakeSQL(t, func(string, []any) ([]string, [][]driver.Value, error) { return nil, nil, errors.New("SQL down") })
	r = checkPlayerHomeZoneVsAccount(context.Background(), runConfig{accountDB: db, routes: &fakeRoutes{}})
	if r.Severity != "fail" || !strings.Contains(r.SampleNotes, "SQL down") {
		t.Fatalf("dependency failure=%+v", r)
	}
}
