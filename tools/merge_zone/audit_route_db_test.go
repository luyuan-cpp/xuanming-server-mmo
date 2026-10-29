package main

// -route-source=db 时只读读方(storage_audit.go 的 routeReadSideDB、audit_checks.go 的 verify:route_cache 与改读库的检查,
// 规格 §6.3 R8、§13.3 第 6 条)的单测。库一侧用 relocate_db_test.go 的 memRouteDB;storage-audit 读 N 库 player_database 的
// 两条语句由 playerIDsSQL 假驱动应答;mapping Redis 用 fakeRouteRedis + memRouteCache。不连库、不拨号。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// ── N 库 player_database 的假驱动 ─────────────────────────────────────────────

// playerIDsSQL 只认 storage-audit 的两条语句:assertSchemaExists 的 information_schema 查询与 mysqlPlayerIDSource 的键集翻页。
// 其余语句一律报错(脚本外的查询不能被当成正常路径悄悄通过)。
type playerIDsSQL struct {
	schema string
	ids    []uint64 // 该库 player_database 里的玩家
}

func (f *playerIDsSQL) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(playerIDsSQLConnector{f: f})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type playerIDsSQLConnector struct{ f *playerIDsSQL }

func (c playerIDsSQLConnector) Connect(context.Context) (driver.Conn, error) {
	return playerIDsSQLConn{f: c.f}, nil
}
func (c playerIDsSQLConnector) Driver() driver.Driver { return playerIDsSQLDriver{} }

type playerIDsSQLDriver struct{}

func (playerIDsSQLDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("playerIDsSQL: use the connector")
}

type playerIDsSQLConn struct{ f *playerIDsSQL }

func (playerIDsSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("playerIDsSQL: Prepare is not supported")
}
func (playerIDsSQLConn) Close() error { return nil }
func (playerIDsSQLConn) Begin() (driver.Tx, error) {
	return nil, errors.New("playerIDsSQL: transactions are not supported")
}

// CheckNamedValue 原样接受实参(uint64 不经默认转换)。
func (playerIDsSQLConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c playerIDsSQLConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	f := c.f
	switch {
	case query == "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?":
		if len(args) == 1 && args[0].Value == f.schema {
			return &playerIDsSQLRows{cols: []string{"SCHEMA_NAME"}, rows: [][]driver.Value{{f.schema}}}, nil
		}
		return &playerIDsSQLRows{cols: []string{"SCHEMA_NAME"}}, nil
	case query == "SELECT player_id FROM "+f.schema+".player_database WHERE player_id > ? ORDER BY player_id LIMIT ?":
		after, limit := playerIDsSQLUint(args[0].Value), playerIDsSQLUint(args[1].Value)
		ids := append([]uint64(nil), f.ids...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		r := &playerIDsSQLRows{cols: []string{"player_id"}}
		for _, id := range ids {
			if id > after && uint64(len(r.rows)) < limit {
				r.rows = append(r.rows, []driver.Value{int64(id)})
			}
		}
		return r, nil
	}
	return nil, fmt.Errorf("playerIDsSQL: unexpected query %q", query)
}

func playerIDsSQLUint(v driver.Value) uint64 {
	switch n := v.(type) {
	case uint64:
		return n
	case int64:
		return uint64(n)
	case int:
		return uint64(n)
	}
	panic(fmt.Sprintf("playerIDsSQL: unexpected argument %T", v))
}

type playerIDsSQLRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
}

func (r *playerIDsSQLRows) Columns() []string { return r.cols }
func (r *playerIDsSQLRows) Close() error      { return nil }
func (r *playerIDsSQLRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

// storageAuditDBFixture 是 TestIT_StorageAudit_CountsLiveColdAndUnmappedRows 的同一组情形,落在库里:
//
//	9901 无落点、home 901 → 真源;9902 落点 902、home 901 → 冷副本;9903 库里没有行 → 无主行;
//	9904 落点 901、home 902 → 真源;9905 冻结在 901(901 库里没有行也算有效落点);9906 落点 901 但冻结 run_id 不合法 → 畸形。
func storageAuditDBFixture() *memRouteDB {
	return newMemRouteDB(
		mergeRouteRow{PlayerID: 9901, HomeZone: 901, Rev: 1},
		mergeRouteRow{PlayerID: 9902, HomeZone: 901, StorageID: 902, PlacementVersion: 1, Rev: 2},
		mergeRouteRow{PlayerID: 9904, HomeZone: 902, StorageID: 901, PlacementVersion: 2, Rev: 3},
		mergeRouteRow{PlayerID: 9905, HomeZone: 901, StorageID: 901, PlacementVersion: 1, FrozenRunID: "reloc-x", Rev: 2},
		mergeRouteRow{PlayerID: 9906, HomeZone: 903, StorageID: 901, PlacementVersion: 1, FrozenRunID: "bad run", Rev: 2},
	)
}

// TestRouteReadSideDB_StoragePlayers:按落点 / home 翻库,口径同 Redis 版(有记录看记录,无记录看 home);畸形行单列、不计入。
func TestRouteReadSideDB_StoragePlayers(t *testing.T) {
	sp, err := routeReadSideDB{db: storageAuditDBFixture()}.storagePlayers(context.Background(), 901)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sp.ByRecord, []uint64{9904, 9905}) || sp.Frozen != 1 || !reflect.DeepEqual(sp.ByHome, []uint64{9901}) ||
		!reflect.DeepEqual(sp.Malformed, []uint64{9906}) {
		t.Fatalf("storage players = %+v", sp)
	}
	// 非 zone 落点库没有「无记录看 home」这一支。
	db := newMemRouteDB(mergeRouteRow{PlayerID: 7, HomeZone: 901, StorageID: relocTestT, PlacementVersion: 4, Rev: 9})
	sp, err = routeReadSideDB{db: db}.storagePlayers(context.Background(), relocTestT)
	if err != nil || !reflect.DeepEqual(sp.ByRecord, []uint64{7}) || len(sp.ByHome) != 0 {
		t.Fatalf("player store = (%+v, %v)", sp, err)
	}
}

// TestRouteReadSideDB_PagesThroughMoreThanOneBatch:翻页跨过一页上限(键集游标接着翻,不足一页即停),结果升序去重。
func TestRouteReadSideDB_PagesThroughMoreThanOneBatch(t *testing.T) {
	db := newMemRouteDB()
	want := make([]uint64, 0, routeReadBatchLimit+3)
	for id := uint64(1); id <= routeReadBatchLimit+3; id++ {
		db.rows[id] = mergeRouteRow{PlayerID: id, HomeZone: 905, Rev: 1}
		want = append(want, id)
	}
	got, err := routeReadSideDB{db: db}.playersWithHome(context.Background(), 905)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("players with home 905 = %d ids (%v), want %d", len(got), err, len(want))
	}
}

// TestScanRouteDBPages_OneLoopForPagesAndRows(R8-6):包内唯一的翻页循环。恰好满一页时还要再翻一页(空页也交给按页回调,
// 与 scanZoneHomeRows 改动前一致),逐行包装不为空页调 visit;回调报错即停、不再翻页。
func TestScanRouteDBPages_OneLoopForPagesAndRows(t *testing.T) {
	db := newMemRouteDB()
	for id := uint64(1); id <= routeReadBatchLimit; id++ {
		db.rows[id] = mergeRouteRow{PlayerID: id, HomeZone: 906, Rev: 1}
	}
	var pageSizes []int
	if err := scanZoneHomeRows(context.Background(), db, 906, func(page []mergeRouteRow) error {
		pageSizes = append(pageSizes, len(page))
		return nil
	}); err != nil || !reflect.DeepEqual(pageSizes, []int{routeReadBatchLimit, 0}) {
		t.Fatalf("page sizes = %v (%v), want one full page then an empty one", pageSizes, err)
	}

	rowsSeen := 0
	if err := scanZoneHomeRows(context.Background(), db, 906, eachRouteDBRow(func(mergeRouteRow) error {
		rowsSeen++
		return nil
	})); err != nil || rowsSeen != routeReadBatchLimit {
		t.Fatalf("rows seen = %d (%v), want %d", rowsSeen, err, routeReadBatchLimit)
	}

	stop := errors.New("stop")
	pages := 0
	err := scanRouteDBPages(func(after routeScanCursor) ([]mergeRouteRow, error) {
		pages++
		return db.ScanByHome(context.Background(), 906, after, routeReadBatchLimit)
	}, eachRouteDBRow(func(row mergeRouteRow) error {
		if row.PlayerID == 3 {
			return stop
		}
		return nil
	}))
	if !errors.Is(err, stop) || pages != 1 {
		t.Fatalf("visit error = %v after %d pages, want stop after the first page", err, pages)
	}

	db.readErr = errors.New("1205")
	if _, err := playerIDsWithHomeZoneDB(context.Background(), db, 906); err == nil {
		t.Fatal("a scan failure must surface")
	}
}

// TestRouteDBReadHelpers_SharedByMergeAndReadSide(R8-6):合服侧的 readPlacementsDB / collectPlayerIDsWithHomeZoneDB 与只读读方
// 走同一份编码与翻页,对同一组行给出同一个结论;空 ids 时读方不碰库(与改动前一致)。
func TestRouteDBReadHelpers_SharedByMergeAndReadSide(t *testing.T) {
	db := storageAuditDBFixture()
	ids := []uint64{9905, 9903, 9901, 9906}
	direct, err := placementsFromRouteDB(context.Background(), db, ids)
	if err != nil {
		t.Fatal(err)
	}
	viaReadSide, err := routeReadSideDB{db: db}.placements(context.Background(), ids)
	if err != nil || !reflect.DeepEqual(direct, viaReadSide) {
		t.Fatalf("read side = (%+v, %v), want the shared encoding %+v", viaReadSide, err, direct)
	}
	if direct[3].RecordErr == nil {
		t.Fatalf("9906 carries a malformed frozen run_id: %+v, want RecordErr", direct[3])
	}
	homes, err := playerIDsWithHomeZoneDB(context.Background(), db, 901)
	if err != nil || !reflect.DeepEqual(homes, []uint64{9901, 9902, 9905}) {
		t.Fatalf("home 901 = (%v, %v)", homes, err)
	}
	viaHome, err := routeReadSideDB{db: db}.playersWithHome(context.Background(), 901)
	if err != nil || !reflect.DeepEqual(viaHome, homes) {
		t.Fatalf("read side home 901 = (%v, %v), want %v", viaHome, err, homes)
	}
	broken := &memRouteDB{readErr: errors.New("boom")}
	if reads, err := (routeReadSideDB{db: broken}).placements(context.Background(), nil); err != nil || reads == nil || len(reads) != 0 {
		t.Fatalf("empty ids = (%v, %v), want an empty result without touching the DB", reads, err)
	}
}

// TestRouteReadSideDB_PlacementsUseTheRedisEncoding:读库给出的值串与 Redis 里同一编码;没有行 = 两键都不存在。
func TestRouteReadSideDB_PlacementsUseTheRedisEncoding(t *testing.T) {
	reads, err := routeReadSideDB{db: storageAuditDBFixture()}.placements(context.Background(), []uint64{9905, 9903, 9901})
	if err != nil {
		t.Fatal(err)
	}
	if reads[0].RecordRaw != frozenValue(901, 1, "reloc-x") || reads[0].HomeRaw != "901" {
		t.Fatalf("9905 = %+v", reads[0])
	}
	if reads[1].RecordPresent || reads[1].HomePresent || reads[1].HomeRaw != "" {
		t.Fatalf("9903 has no row: %+v", reads[1])
	}
	if reads[2].RecordPresent || reads[2].HomeRaw != "901" {
		t.Fatalf("9901 = %+v", reads[2])
	}
	if _, err := (routeReadSideDB{db: &memRouteDB{readErr: errors.New("boom")}}).placements(context.Background(), []uint64{1}); err == nil {
		t.Fatal("a DB read failure must surface")
	}
}

// TestAuditStorageReadsTheDB:-route-source=db 的 storage-audit 只按库判(mapping Redis 一把键都不读),结论与 Redis 口径的同一组情形一致。
func TestAuditStorageReadsTheDB(t *testing.T) {
	db := (&playerIDsSQL{schema: zoneDBName(901), ids: []uint64{9901, 9902, 9903, 9904}}).open(t)
	rep, err := auditStorageWith(context.Background(), db, routeReadSideDB{db: storageAuditDBFixture()}, 901)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ByRecord != 2 || rep.FrozenOnRecord != 1 || rep.ByHome != 1 || rep.Rows != 4 || rep.LiveRows != 2 ||
		rep.ColdCopies != 1 || rep.UnmappedRows != 1 || rep.UnreadableRows != 0 || rep.MalformedRecords != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if fmt.Sprint(rep.ColdSample, rep.UnmappedSample, rep.MalformedSample) != "[9902] [9903] [9906]" {
		t.Errorf("samples = %v %v %v", rep.ColdSample, rep.UnmappedSample, rep.MalformedSample)
	}
	if len(rep.RetireBlockers()) == 0 {
		t.Fatal("storage 901 still has players placed on it: must not be retirable")
	}
	// 库读失败 = 没查成,不能当作「没人指向」。
	if _, err := auditStorageWith(context.Background(), db, routeReadSideDB{db: &memRouteDB{readErr: errors.New("1205")}}, 901); err == nil {
		t.Fatal("a route DB failure must fail the audit")
	}
}

// writableAfter 是写开关替身:前 okCalls 次可写,之后返回 err(模拟扫描途中 leader 把写者冻结)。
type writableAfter struct {
	okCalls int
	err     error
	calls   int
}

func (w *writableAfter) RequireWritable(context.Context) error {
	w.calls++
	if w.calls <= w.okCalls {
		return nil
	}
	return w.err
}

// TestAuditStorageChecked_FrozenRouteWritersGiveNoVerdict(R8-1):db 源下 route_meta.writable != 1(回档恢复中,库是已知落后的
// 那一份)不给退役结论 —— 扫描前冻结不扫、扫描途中冻结丢弃结论,错误包着 errRouteNotWritable 并指向 runbook §9.4;读不到开关同样
// 不给结论;可写时扫描前后各核一次,报告与 auditStorageWith 相同;authority 为 nil(redis 源)不查开关。
func TestAuditStorageChecked_FrozenRouteWritersGiveNoVerdict(t *testing.T) {
	ctx := context.Background()
	db := (&playerIDsSQL{schema: zoneDBName(901), ids: []uint64{9901, 9902, 9903, 9904}}).open(t)
	routes := routeReadSideDB{db: storageAuditDBFixture()}
	want, err := auditStorageWith(ctx, db, routes, 901)
	if err != nil {
		t.Fatal(err)
	}
	frozen := fmt.Errorf("%w (writable=0)", errRouteNotWritable)

	// 扫描之前已冻结:不扫,报告是空的。
	before := newMemRouteDB()
	before.writableErr = frozen
	rep, err := auditStorageChecked(ctx, db, routes, before, 901)
	if !errors.Is(err, errRouteNotWritable) || !strings.Contains(err.Error(), "runbook §9.4") || rep.Rows != 0 {
		t.Fatalf("frozen before the scan = (%s, %v), want no scan and errRouteNotWritable naming runbook §9.4", rep, err)
	}

	// 扫描途中被冻结:扫完也不给结论。
	during := &writableAfter{okCalls: 1, err: frozen}
	if _, err := auditStorageChecked(ctx, db, routes, during, 901); !errors.Is(err, errRouteNotWritable) || during.calls != 2 {
		t.Fatalf("frozen during the scan = %v after %d checks, want errRouteNotWritable from the second check", err, during.calls)
	}

	// 读不到开关:同样不给结论,原因原样带出,不当成「冻结」。
	unreadable := newMemRouteDB()
	unreadable.writableErr = errors.New("read route_meta.writable: connection refused")
	if _, err := auditStorageChecked(ctx, db, routes, unreadable, 901); err == nil || errors.Is(err, errRouteNotWritable) ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("unreadable switch = %v, want the cause", err)
	}

	// 可写:前后各核一次,报告与不核开关时相同。
	ok := newMemRouteDB()
	got, err := auditStorageChecked(ctx, db, routes, ok, 901)
	if err != nil || !reflect.DeepEqual(got, want) || ok.writableChecks != 2 {
		t.Fatalf("writable = (%s, %v) after %d checks, want %s after 2", got, err, ok.writableChecks, want)
	}

	// redis 源(authority 为无类型 nil):不查开关,与 auditStorageWith 逐字相同。
	got, err = auditStorageChecked(ctx, db, routes, nil, 901)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("no authority = (%s, %v), want %s", got, err, want)
	}
}

// TestOpenRouteReadSide_RefusesAMissingSource:装配层没传 -route-source 时 fail-closed,不默认成任何一种来源。
func TestOpenRouteReadSide_RefusesAMissingSource(t *testing.T) {
	if _, _, err := openRouteReadSide(context.Background(), routeEntryOptions{}, nil, "audit"); err == nil || !strings.Contains(err.Error(), "-route-source") {
		t.Fatalf("missing source = %v, want a refusal naming -route-source", err)
	}
	if _, _, err := openRouteReadSide(context.Background(), routeEntryOptions{source: routeSourceDB, dbDSN: "x"}, nil, "audit"); err == nil {
		t.Fatal("no mapping Redis handle: the pairing cannot be checked, must refuse")
	}
}

// ── 合服审计在 db 源下读库 ────────────────────────────────────────────────────

func routeAuditManifest(ids ...uint64) *mergeManifest {
	m := newMergeManifest("r", 901, 902, "op", time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
	m.PlayerIDs = ids
	return m
}

// TestVerifyManifestMapping_DBSourceReadsTheDB:db 源下逐 id 看库里的 home_zone(缓存里写着什么不影响结论)。
func TestVerifyManifestMapping_DBSourceReadsTheDB(t *testing.T) {
	db := newMemRouteDB(
		mergeRouteRow{PlayerID: 1, HomeZone: 902, Rev: 2},
		mergeRouteRow{PlayerID: 2, HomeZone: 901, Rev: 1}, // 还停在源区
	)
	cfg := auditConfig{src: 901, dst: 902, manifest: routeAuditManifest(1, 2, 3), expectedSrcPlayers: -1,
		routeSource: routeSourceDB, routes: routeReadSideDB{db: db}, routeDB: db}
	r := verifyManifestMapping(context.Background(), cfg)
	if r.Severity != "block" || r.ConflictCount != 2 || !strings.Contains(r.Notes, "[2 3]") {
		t.Fatalf("verify:manifest_mapping = %+v, want players 2 (still 901) and 3 (no row) flagged", r)
	}
	db.rows[2] = mergeRouteRow{PlayerID: 2, HomeZone: 902, Rev: 2}
	db.rows[3] = mergeRouteRow{PlayerID: 3, HomeZone: 902, Rev: 2}
	if r := verifyManifestMapping(context.Background(), cfg); r.Severity != "info" {
		t.Fatalf("all manifest players on 902 in the DB: %+v", r)
	}
	// 读方没装成(库与 Redis 不成对等):INFRA,带原因。
	cfg.routes, cfg.routeErr = nil, errors.New("route DB and mapping Redis are not paired")
	if r := verifyManifestMapping(context.Background(), cfg); !isInfraAudit(r) || !strings.Contains(r.Notes, "not paired") {
		t.Fatalf("no route read side = %+v, want INFRA with the cause", r)
	}
}

// TestRunAuditMode_RouteCacheOnlyWithTheDBSource:verify:route_cache 只在 -route-source=db 的合服后验证里出现。
func TestRunAuditMode_RouteCacheOnlyWithTheDBSource(t *testing.T) {
	names := func(cfg auditConfig) map[string]bool {
		out := map[string]bool{}
		for _, r := range runAuditMode(context.Background(), cfg) {
			out[r.Name] = true
		}
		return out
	}
	base := auditConfig{verify: true, tradeSchema: defaultTradeSchema, guildSchema: defaultGuildSchema, src: 901, dst: 902}
	if names(base)["verify:route_cache"] {
		t.Fatal("redis source: there is no second copy to compare, verify:route_cache must not run")
	}
	base.routeSource = routeSourceDB
	if !names(base)["verify:route_cache"] {
		t.Fatal("db source: -verify-merged must include verify:route_cache")
	}
	base.verify = false
	if names(base)["verify:route_cache"] {
		t.Fatal("the pre-merge gates do not include verify:route_cache")
	}
}

// ── verify:route_cache ───────────────────────────────────────────────────────

func TestRouteCacheMatches(t *testing.T) {
	row := mergeRouteRow{PlayerID: 9, HomeZone: 902, StorageID: 901, PlacementVersion: 1, Rev: 5}
	for _, tc := range []struct {
		name           string
		row            mergeRouteRow
		present        bool
		home, plc, rev any
		want           bool
		why            string
	}{
		{"一致", row, true, "902", "901:1", "5", true, ""},
		{"缓存rev落后", row, true, "902", "901:1", "4", false, `player:maprev "4" != DB rev 5`},
		{"缓存rev比库新也不算一致", row, true, "902", "901:1", "6", false, `player:maprev "6" is AHEAD of DB rev 5`},
		{"缓存rev不规范按不相等报", row, true, "902", "901:1", "06", false, `player:maprev "06" != DB rev 5`},
		{"缓存没有maprev", row, true, "902", "901:1", nil, false, "player:maprev is absent"},
		{"home不同", row, true, "901", "901:1", "5", false, `player:zone "901" != DB home_zone "902"`},
		{"库有落点缓存没有", row, true, "902", nil, "5", false, `player:placement <absent> != DB placement "901:1"`},
		{"库没有落点缓存有", mergeRouteRow{PlayerID: 9, HomeZone: 902, Rev: 5}, true, "902", "901:1", "5", false, "player:placement"},
		{"home空串不等于缺席", mergeRouteRow{PlayerID: 9, StorageID: 901, PlacementVersion: 1, Rev: 5}, true, "", "901:1", "5", false, "player:zone"},
		{"库里没有这一行", mergeRouteRow{}, false, "902", nil, "5", false, "no player_route row"},
		{"库行畸形", mergeRouteRow{PlayerID: 9, HomeZone: 902, StorageID: 901, PlacementVersion: 1, FrozenRunID: "bad run", Rev: 5}, true,
			"902", "901:1:frozen:bad run", "5", false, "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why, err := routeCacheMatches(tc.row, tc.present, tc.home, tc.plc, tc.rev)
			if err != nil || got != tc.want || !strings.Contains(why, tc.why) {
				t.Fatalf("routeCacheMatches = (%v, %q, %v), want (%v, ~%q)", got, why, err, tc.want, tc.why)
			}
		})
	}
	if _, _, err := routeCacheMatches(row, true, int64(902), nil, "5"); err == nil {
		t.Fatal("an MGET element of the wrong type means the check did not run, must be an error")
	}
}

// TestVerifyRouteCache:清单玩家逐人比对库与缓存,不一致 block(带样本),一致 info;MGET 失败 INFRA。
func TestVerifyRouteCache(t *testing.T) {
	db := newMemRouteDB(
		mergeRouteRow{PlayerID: 1, HomeZone: 902, StorageID: 901, PlacementVersion: 1, Rev: 3},
		mergeRouteRow{PlayerID: 2, HomeZone: 902, Rev: 4},
	)
	cache := newMemRouteCache()
	cache.seed(db.rows[1])
	cache.seed(mergeRouteRow{PlayerID: 2, HomeZone: 901, Rev: 3}) // 发布丢了:缓存停在合服前
	rdb, _ := newFakeRouteRedis(t, cache.reply)
	cfg := auditConfig{src: 901, dst: 902, manifest: routeAuditManifest(1, 2), routeSource: routeSourceDB,
		routes: routeReadSideDB{db: db}, routeDB: db, mappingDB: rdb}

	r := verifyRouteCache(context.Background(), cfg)
	if r.Severity != "block" || r.ConflictCount != 1 || !strings.Contains(r.Notes, `2: player:maprev "3" != DB rev 4`) {
		t.Fatalf("verify:route_cache = %+v, want player 2 flagged", r)
	}
	// B2-5:block 文案同时给出「缓存领先 = 库回档」的处置(leader 与重跑都压不低缓存 rev)。
	if !strings.Contains(r.Notes, "AHEAD") || !strings.Contains(r.Notes, "runbook §9.4") {
		t.Fatalf("verify:route_cache notes = %q, want the rollback (cache AHEAD) guidance", r.Notes)
	}
	cache.seed(db.rows[2])
	if r := verifyRouteCache(context.Background(), cfg); r.Severity != "info" || r.TargetCount != 2 {
		t.Fatalf("after the leader caught up: %+v", r)
	}
	cache.down = true
	if r := verifyRouteCache(context.Background(), cfg); !isInfraAudit(r) {
		t.Fatalf("mapping Redis down: %+v, want INFRA", r)
	}
	cfg.routeDB, cfg.routeErr = nil, errors.New("open -route-db-dsn: refused")
	if r := verifyRouteCache(context.Background(), cfg); !isInfraAudit(r) || !strings.Contains(r.Notes, "refused") {
		t.Fatalf("no route DB = %+v, want INFRA with the cause", r)
	}
}

// TestVerifyFenceReleased_DBSourceAlsoChecksTheDBFence:db 源下围栏的真源是库;库里仍生效即 block(镜像已删也一样)。
func TestVerifyFenceReleased_DBSourceAlsoChecksTheDBFence(t *testing.T) {
	cache := newMemRouteCache()
	rdb, _ := newFakeRouteRedisWithExists(t, cache)
	db := newMemRouteDB()
	db.fences[902] = routeFenceState{Present: true, RunID: "901-902-run", Active: true}
	cfg := auditConfig{src: 901, dst: 902, mappingDB: rdb, routeDB: db}
	r := verifyFenceReleased(context.Background(), cfg)
	if r.Severity != "block" || !strings.Contains(r.Notes, "zone_merge_fence(zone=902, run_id=901-902-run)") {
		t.Fatalf("verify:merge_fence = %+v, want the live DB fence reported", r)
	}
	db.fences[902] = routeFenceState{Present: true, Epoch: 3}
	if r := verifyFenceReleased(context.Background(), cfg); r.Severity != "info" {
		t.Fatalf("released fence = %+v", r)
	}
}

// TestVerifyFenceReleased_DBSourceGuidance(B2-4 / R8-3):-route-source=db 时
//   - 库读方没装成 = 没查真源:INFRA(带原因),不能报「已释放」;
//   - 库围栏仍生效:block,处置指向持有它的那次运行(-take-over-fence)或等库时钟过期,明确不许 DEL 镜像;
//   - 只剩镜像、库围栏已释放:block,等对账 leader 删掉过期镜像(没有 fence_epoch 的才手工 DEL);
//   - 都已释放:info,说明库与镜像都核过。
//
// redis 源的判定与文案逐字不变。
func TestVerifyFenceReleased_DBSourceGuidance(t *testing.T) {
	ctx := context.Background()
	cache := newMemRouteCache()
	rdb, _ := newFakeRouteRedisWithExists(t, cache)
	db := newMemRouteDB()
	cfg := auditConfig{src: 901, dst: 902, mappingDB: rdb, routeSource: routeSourceDB}

	cfg.routeErr = errors.New("route DB and mapping Redis are not paired")
	if r := verifyFenceReleased(ctx, cfg); !isInfraAudit(r) || !strings.Contains(r.Notes, "not paired") {
		t.Fatalf("no route DB (with cause) = %+v, want INFRA with the cause", r)
	}
	cfg.routeErr = nil
	if r := verifyFenceReleased(ctx, cfg); !isInfraAudit(r) || !strings.Contains(r.Notes, "zone_merge_fence was not checked") {
		t.Fatalf("no route DB = %+v, want INFRA saying the DB fence was not checked", r)
	}

	cfg.routeDB = db
	cache.kv[mergeFenceKey(902)] = `{"run_id":"901-902-run","fence_epoch":3}`
	db.fences[902] = routeFenceState{Present: true, RunID: "901-902-run", Epoch: 3, Active: true}
	r := verifyFenceReleased(ctx, cfg)
	if r.Severity != "block" || !strings.Contains(r.Notes, "Do NOT DEL") || !strings.Contains(r.Notes, "-take-over-fence") ||
		strings.Contains(r.Notes, "DEL them once") || r.SourceCount != 2 {
		t.Fatalf("active DB fence = %+v, want block pointing at the owning run and forbidding DEL of the mirror", r)
	}

	db.fences[902] = routeFenceState{Present: true, Epoch: 3}
	r = verifyFenceReleased(ctx, cfg)
	if r.Severity != "block" || !strings.Contains(r.Notes, "already released") || !strings.Contains(r.Notes, "compare-and-deletes") ||
		r.SourceCount != 1 {
		t.Fatalf("stale mirror only = %+v, want block waiting for the reconcile leader", r)
	}

	delete(cache.kv, mergeFenceKey(902))
	if r := verifyFenceReleased(ctx, cfg); r.Severity != "info" || !strings.Contains(r.Notes, "zone_merge_fence released") {
		t.Fatalf("both released = %+v, want info naming the DB fence", r)
	}

	// redis 源:文案逐字不变,不碰库。
	redisCfg := auditConfig{src: 901, dst: 902, mappingDB: rdb, routeSource: routeSourceRedis}
	cache.kv[mergeFenceKey(901)] = "holder"
	want := "merge fence still set: [" + mergeFenceKey(901) + "] — data_service RegisterPlayerZone and guild CreateGuild " +
		"are refusing new objects in those zones. DEL them once no merge_zone process is alive."
	if r := verifyFenceReleased(ctx, redisCfg); r.Severity != "block" || r.Notes != want {
		t.Fatalf("redis source held = %+v, want the unchanged notes %q", r, want)
	}
	delete(cache.kv, mergeFenceKey(901))
	if r := verifyFenceReleased(ctx, redisCfg); r.Severity != "info" || r.Notes != "no merge:in_progress:* fence left behind" {
		t.Fatalf("redis source released = %+v", r)
	}
}

// newFakeRouteRedisWithExists 在 memRouteCache 之上再认 EXISTS(verify:merge_fence 查镜像用)。
func newFakeRouteRedisWithExists(t *testing.T, cache *memRouteCache) (*redis.Client, *fakeRouteRedis) {
	t.Helper()
	return newFakeRouteRedis(t, func(n int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "exists" {
			var hits int64
			for _, k := range cmd.Args()[1:] {
				if _, ok := cache.kv[k.(string)]; ok {
					hits++
				}
			}
			return hits, nil
		}
		return cache.reply(n, cmd)
	})
}
