package main

// route_store.go 的单测:AST 守卫、DSN、探针判据、参数校验,以及批原语的语句序列(假 database/sql 驱动,仿
// go/data_service/internal/store/route_store_contract_test.go;本 module 没有 sqlmock,按约定不为测试引新依赖)。
//
// 假驱动**不模拟 InnoDB 的锁、唯一约束与 CHECK**:并发下谁赢、会不会 1213,只有真库能证(route_store_integration_test.go)。
// 这里只证「给定库的应答,本层的判定、语句、实参与提交 / 回滚不变」。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
	"shared/drainproof"
)

// 这些是同步SQL状态机域测，原writable=1语义保留。只注入明确本机owner，
// 实际fake driver仍验证全部BEGIN/DML/COMMIT/ROLLBACK；不造P0/Kafka或对外原件。
func routeSQLTestContext() context.Context {
	config := authoritysql.WriterConfig{}
	owner := &maintenanceTool{normalSQL: &config, session: drainproof.New(), check: func(context.Context) error { return nil }}
	ctx := context.WithValue(context.Background(), maintenanceContextKey{}, owner)
	owner.context = ctx
	return ctx
}

const (
	testRouteWriter = "merge_zone@unit-test:run-1"
	testRouteRun    = "101-102-20261019T000000Z-deadbeef"
	testRouteGen    = "0123456789abcdef0123456789abcdef"
)

// ── AST 守卫 ──────────────────────────────────────────────────────────────────

// TestNoRouteTableWritesOutsideMergeRouteStore:merge_zone 里写 player_route / zone_merge_fence / zone_merged_into 的语句只许出现在
// route_store.go(规格 §6.1;仿 data_service 的 TestNoPlayerRouteWritesOutsideRouteStore)。绕过它的写者不走 rev、不走围栏、不发布,
// 缓存与库会静默分叉。route_meta / route_reconcile 本工具一律不写(冻结 / 解冻 / 对账只属于 data_service)。
//
// 只检查字符串字面量(先 unquote),不看注释;跨字面量拼出来的语句("UPDATE " + table)查不到;_test.go 不检查。
func TestNoRouteTableWritesOutsideMergeRouteStore(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve tools/merge_zone: %v", err)
	}
	const verbs = "(?i)(INSERT\\s+INTO|UPDATE|DELETE\\s+FROM|REPLACE\\s+INTO|TRUNCATE\\s+TABLE)\\s+`?"
	routeWrite := regexp.MustCompile(verbs + "(player_route|zone_merge_fence|zone_merged_into)\\b")
	metaWrite := regexp.MustCompile(verbs + "(route_meta|route_reconcile)\\b")
	const allowed = "route_store.go"

	var files, allowedHits int
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case "generated", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if metaWrite.MatchString(s) {
				offenders = append(offenders, rel+" (route_meta / route_reconcile are never written by merge_zone): "+s)
			}
			if !routeWrite.MatchString(s) {
				return true
			}
			if rel == allowed {
				allowedHits++
			} else {
				offenders = append(offenders, rel+": "+s)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if files < 10 {
		t.Fatalf("只扫到 %d 个 .go 文件:守卫失效", files)
	}
	// 至少:P4、P5、围栏 ensure / acquire / renew / release、merged_into insert / update / delete、
	// player_route move / fill / insert / pin / set-placement 共 14 条。
	if allowedHits < 14 {
		t.Fatalf("%s 里只匹配到 %d 条路由表写语句(至少应有 14 条):正则失效,本守卫已经拦不住任何东西", allowed, allowedHits)
	}
	if len(offenders) > 0 {
		t.Fatalf("路由表的写入只许出现在 %s(规格 §6.1),以下位置绕过了它:\n%s", allowed, strings.Join(offenders, "\n"))
	}
}

// ── 假驱动 ────────────────────────────────────────────────────────────────────

// fakeRouteSQLReply 是一条语句的应答:Exec 看 affected,Query 看 rows(nil = 零行 → sql.ErrNoRows);err 非 nil 时其余字段忽略。
type fakeRouteSQLReply struct {
	affected int64
	rows     [][]driver.Value
	err      error
}

// fakeRouteSQLCall 是真的发到驱动的一次调用。label 见 routeSQLLabel;另有 BEGIN / COMMIT / ROLLBACK / SLEEP。
type fakeRouteSQLCall struct {
	label string
	query string
	args  []driver.Value
}

// routeScript 是一次用例的应答脚本:按语句标签排队,按调用顺序消费。
type routeScript map[string][]fakeRouteSQLReply

func (s routeScript) on(label string, replies ...fakeRouteSQLReply) routeScript {
	s[label] = append(s[label], replies...)
	return s
}

// fakeRouteSQL 是假库:记录每次调用,按脚本应答;脚本用尽就报错(脚本写少了不能被当成正常路径悄悄通过)。
type fakeRouteSQL struct {
	mu      sync.Mutex
	replies routeScript
	calls   []fakeRouteSQLCall
}

// newFakeRouteSQLStore 把假驱动接成一个 mergeRouteStore(不跑探针),退避不真睡、只记一条 SLEEP。
func newFakeRouteSQLStore(t *testing.T, script routeScript) (*mergeRouteStore, *fakeRouteSQL) {
	t.Helper()
	f := &fakeRouteSQL{replies: routeScript{}}
	for k, v := range script {
		f.replies[k] = append([]fakeRouteSQLReply(nil), v...)
	}
	db := sql.OpenDB(fakeRouteSQLConnector{f: f})
	t.Cleanup(func() { _ = db.Close() })
	st := &mergeRouteStore{db: db, writer: testRouteWriter, sleep: f.sleep, backoff: func() time.Duration { return time.Millisecond }}
	return st, f
}

// routeSQLLabel 把 SQL 文本映射成标签。只认逐字相同的生产文本(动态语句认前缀);认不出来就是空串,假驱动对它报错。
func routeSQLLabel(q string) string {
	switch q {
	case routeProbeSessionSQL:
		return "SESSION"
	case currentTiDBVersionSQL:
		return "VERSION"
	case currentTiDBSessionSQL:
		return "TXN_MODE"
	case routeProbeShardCheckSQL:
		return "P4"
	case routeProbeShardModulusSQL:
		return "P5"
	case routeReadWritableSQL:
		return "WRITABLE"
	case routeLockProbeGuardSQL:
		return "GUARD"
	case routeReadMetaSQL:
		return "META"
	case routeNowSQL:
		return "NOW"
	case routeFenceShardExistsSQL:
		return "FENCE_EXISTS"
	case routeLockFenceAllSQL:
		return "LOCK_FENCE_ALL"
	case routeAcquireFenceSQL:
		return "ACQUIRE"
	case routeRenewFenceSQL:
		return "RENEW"
	case routeReadFenceExpirySQL:
		return "FENCE_EXPIRY"
	case routeReleaseFenceSQL:
		return "RELEASE"
	case routeLockFenceShardSQL:
		return "LOCK_FENCE"
	case routeReadFenceSQL:
		return "READ_FENCE"
	case routeLockMergedIntoSQL:
		return "LOCK_MERGED"
	case routeReadMergedIntoSQL:
		return "READ_MERGED"
	case routeInsertMergedIntoSQL:
		return "INSERT_MERGED"
	case routeUpdateMergedIntoRunSQL:
		return "UPDATE_MERGED"
	case routeDeleteMergedIntoSQL:
		return "DELETE_MERGED"
	case routeScanByHomeSQL:
		return "SCAN_HOME"
	case routeScanByStorageSQL:
		return "SCAN_STORAGE"
	case routePinZoneSelectSQL:
		return "PIN_ZONE_SELECT"
	case routeMoveHomeSQL:
		return "MOVE_HOME"
	case routeFillHomeSQL:
		return "FILL_HOME"
	case routePinSQL:
		return "PIN"
	case routeSetPlacementSQL:
		return "SET_PLACEMENT"
	}
	switch {
	case strings.HasPrefix(q, routeEnsureFenceHeadSQL):
		return "ENSURE_FENCE"
	case strings.HasPrefix(q, routeInsertHomeHeadSQL):
		return "INSERT_HOME"
	case strings.HasPrefix(q, routeSelectRowsHeadSQL) && strings.HasSuffix(q, " FOR UPDATE"):
		return "LOCK_ROWS"
	case strings.HasPrefix(q, routeSelectRowsHeadSQL):
		return "READ_ROWS"
	}
	return ""
}

func (f *fakeRouteSQL) next(query string, args []driver.NamedValue) (fakeRouteSQLReply, error) {
	label := routeSQLLabel(query)
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeRouteSQLCall{label: label, query: query, args: vals})
	if label == "" {
		return fakeRouteSQLReply{}, fmt.Errorf("fake route sql: unexpected statement %q", query)
	}
	queue := f.replies[label]
	if len(queue) == 0 {
		return fakeRouteSQLReply{}, fmt.Errorf("fake route sql: script exhausted for %s", label)
	}
	f.replies[label] = queue[1:]
	return queue[0], nil
}

func (f *fakeRouteSQL) record(label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeRouteSQLCall{label: label})
}

func (f *fakeRouteSQL) sleep(ctx context.Context, _ time.Duration) error {
	f.record("SLEEP")
	return ctx.Err()
}

func (f *fakeRouteSQL) labels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.label)
	}
	return out
}

func (f *fakeRouteSQL) callsOf(label string) []fakeRouteSQLCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRouteSQLCall
	for _, c := range f.calls {
		if c.label == label {
			out = append(out, c)
		}
	}
	return out
}

// assertRouteLabels 钉住语句序列(含 BEGIN / COMMIT / ROLLBACK / SLEEP),并要求脚本恰好用完。
func assertRouteLabels(t *testing.T, f *fakeRouteSQL, want ...string) {
	t.Helper()
	if got := f.labels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("statement sequence\n got  %v\n want %v", got, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for label, queue := range f.replies {
		if len(queue) > 0 {
			t.Fatalf("script for %s has %d unused replies", label, len(queue))
		}
	}
}

func assertRouteArgs(t *testing.T, c fakeRouteSQLCall, want ...driver.Value) {
	t.Helper()
	if !reflect.DeepEqual(c.args, want) {
		t.Fatalf("%s args\n got  %#v\n want %#v", c.label, c.args, want)
	}
}

type fakeRouteSQLConnector struct{ f *fakeRouteSQL }

func (c fakeRouteSQLConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeRouteSQLConn{f: c.f}, nil
}

func (c fakeRouteSQLConnector) Driver() driver.Driver { return fakeRouteSQLDriver{} }

type fakeRouteSQLDriver struct{}

func (fakeRouteSQLDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake route sql: only sql.OpenDB(connector) is supported")
}

type fakeRouteSQLConn struct{ f *fakeRouteSQL }

// Prepare 不会被用到:database/sql 对实现了 ExecerContext / QueryerContext 的连接不走 Prepare。
func (c *fakeRouteSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake route sql: Prepare is not supported")
}

func (c *fakeRouteSQLConn) Close() error { return nil }

func (c *fakeRouteSQLConn) Begin() (driver.Tx, error) {
	return c.BeginTx(routeSQLTestContext(), driver.TxOptions{})
}

func (c *fakeRouteSQLConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.f.record("BEGIN")
	return fakeRouteSQLTx{f: c.f}, nil
}

// CheckNamedValue 保留 uint64(go-sql-driver 的口径;database/sql 的默认转换器会把它改成 int64),其余按驱动常见形状转换。
func (c *fakeRouteSQLConn) CheckNamedValue(nv *driver.NamedValue) error {
	switch v := nv.Value.(type) {
	case nil, int64, uint64, string, []byte, bool, float64, time.Time:
	case int:
		nv.Value = int64(v)
	case uint32:
		nv.Value = uint64(v)
	default:
		return fmt.Errorf("fake route sql: unsupported argument type %T", nv.Value)
	}
	return nil
}

func (c *fakeRouteSQLConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	reply, err := c.f.next(query, args)
	if err != nil {
		return nil, err
	}
	if reply.err != nil {
		return nil, reply.err
	}
	return driver.RowsAffected(reply.affected), nil
}

func (c *fakeRouteSQLConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	reply, err := c.f.next(query, args)
	if err != nil {
		return nil, err
	}
	if reply.err != nil {
		return nil, reply.err
	}
	cols := 1
	if len(reply.rows) > 0 {
		cols = len(reply.rows[0])
	}
	return &fakeRouteSQLRows{cols: cols, rows: append([][]driver.Value(nil), reply.rows...)}, nil
}

type fakeRouteSQLTx struct{ f *fakeRouteSQL }

func (t fakeRouteSQLTx) Commit() error {
	t.f.record("COMMIT")
	return nil
}

func (t fakeRouteSQLTx) Rollback() error {
	t.f.record("ROLLBACK")
	return nil
}

type fakeRouteSQLRows struct {
	cols int
	rows [][]driver.Value
}

func (r *fakeRouteSQLRows) Columns() []string {
	names := make([]string, r.cols)
	for i := range names {
		names[i] = "c" + strconv.Itoa(i)
	}
	return names
}

func (r *fakeRouteSQLRows) Close() error { return nil }

func (r *fakeRouteSQLRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// ── 应答的小工具 ──────────────────────────────────────────────────────────────

func rsRows(rows ...[]driver.Value) fakeRouteSQLReply { return fakeRouteSQLReply{rows: rows} }

func rsAffected(n int64) fakeRouteSQLReply { return fakeRouteSQLReply{affected: n} }

func rsErr(err error) fakeRouteSQLReply { return fakeRouteSQLReply{err: err} }

func rsMySQLErr(number uint16, msg string) fakeRouteSQLReply {
	return rsErr(&mysql.MySQLError{Number: number, Message: msg})
}

func rsNull32(v uint32) driver.Value {
	if v == 0 {
		return nil
	}
	return int64(v)
}

func rsNull64(v uint64) driver.Value {
	if v == 0 {
		return nil
	}
	return int64(v)
}

func rsNullStr(s string) driver.Value {
	if s == "" {
		return nil
	}
	return s
}

// rsRouteRow 是 player_route 读到的一行(列序同 routeSelectRowsHeadSQL:route_shard、player_id、routeRowColumnsSQL)。
func rsRouteRow(r mergeRouteRow) []driver.Value {
	return []driver.Value{int64(routeRowShard(r.PlayerID)), int64(r.PlayerID), rsNull32(r.HomeZone), rsNull32(r.StorageID),
		rsNull64(r.PlacementVersion), rsNullStr(r.FrozenRunID), int64(r.Rev)}
}

func rsOne() fakeRouteSQLReply { return rsRows([]driver.Value{int64(1)}) }

func rsNone() fakeRouteSQLReply { return fakeRouteSQLReply{} }

func rsFenceHeld(run string) fakeRouteSQLReply { return rsRows([]driver.Value{run, int64(1)}) }

func rsFenceExpired(run string) fakeRouteSQLReply { return rsRows([]driver.Value{run, int64(0)}) }

func rsFenceFree() fakeRouteSQLReply { return rsRows([]driver.Value{nil, int64(0)}) }

// rsFenceAll 是一个 zone 的 64 片(LOCK_FENCE_ALL 的应答):全部同一个 run_id / 生效与否 / epoch。有 run_id 的片记着
// testFenceAcquire 的 zone 对(101 → 102),holder_json 不带 manifest_path(接手核对只比 zone 对)。
func rsFenceAll(run string, active bool, epoch uint64) fakeRouteSQLReply {
	holder := ""
	if run != "" {
		holder = `{"run_id":"` + run + `"}`
	}
	return rsFenceAllHeld(run, active, epoch, 101, 102, holder)
}

// rsFenceAllHeld 同 rsFenceAll,分片上的 zone 对(src_zone / dst_zone)与 holder_json 由调用方给;run 为空时这三列一律 NULL
// (列序同 routeLockFenceAllSQL:shard、run_id、active、fence_epoch、holder_json、src_zone、dst_zone)。
func rsFenceAllHeld(run string, active bool, epoch uint64, src, dst uint32, holder string) fakeRouteSQLReply {
	rows := make([][]driver.Value, routeFenceShards)
	for s := range rows {
		var runV, holderV, srcV, dstV driver.Value
		if run != "" {
			runV, holderV, srcV, dstV = run, rsNullStr(holder), int64(src), int64(dst)
		}
		act := int64(0)
		if active {
			act = 1
		}
		rows[s] = []driver.Value{int64(s), runV, act, int64(epoch), holderV, srcV, dstV}
	}
	return fakeRouteSQLReply{rows: rows}
}

// ── DSN ───────────────────────────────────────────────────────────────────────

func TestRouteDBDSNPinsSessionVariables(t *testing.T) {
	out, err := routeDBDSN("appuser:s3cret@tcp(127.0.0.1:3306)/testdb?charset=utf8mb4&tx_isolation=REPEATABLE-READ" +
		"&transaction_isolation=REPEATABLE-READ&innodb_lock_wait_timeout=50&parseTime=true")
	if err != nil {
		t.Fatalf("routeDBDSN: %v", err)
	}
	cfg, err := mysql.ParseDSN(out)
	if err != nil {
		t.Fatalf("parse normalized DSN: %v", err)
	}
	if cfg.DBName != "testdb" || cfg.User != "appuser" || cfg.Passwd != "s3cret" || !cfg.ParseTime || cfg.ClientFoundRows {
		t.Fatalf("normalized DSN lost settings: db=%q user=%q parseTime=%v clientFoundRows=%v", cfg.DBName, cfg.User, cfg.ParseTime, cfg.ClientFoundRows)
	}
	for k, want := range map[string]string{
		mysqlIsolationParam: mysqlReadCommitted,
		routeLockWaitParam:  "1",
		routeSQLModeParam:   routeSQLModeStrict,
	} {
		if got := cfg.Params[k]; got != want {
			t.Fatalf("param %s = %q, want %q", k, got, want)
		}
	}
	if _, ok := cfg.Params[mysqlLegacyIsolationParam]; ok {
		t.Fatalf("legacy %s must be dropped: %v", mysqlLegacyIsolationParam, cfg.Params)
	}
}

func TestRouteDBDSNRejects(t *testing.T) {
	for _, tc := range []struct {
		name, dsn, want string
	}{
		{"没有库名", "u:p@tcp(127.0.0.1:3306)/", "must name the global database"},
		{"clientFoundRows", "u:p@tcp(127.0.0.1:3306)/testdb?clientFoundRows=true", "clientFoundRows"},
		{"格式错", "u:p@tcp(127.0.0.1:3306", "parse -route-db-dsn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := routeDBDSN(tc.dsn)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("routeDBDSN(%q) error = %v, want it to mention %q", tc.dsn, err, tc.want)
			}
			if strings.Contains(err.Error(), ":p@") {
				t.Fatalf("the error must not echo the DSN (it carries the password): %v", err)
			}
		})
	}
}

// ── 探针 ──────────────────────────────────────────────────────────────────────

const routeCheckViolated = "Check constraint 'chk_route_shard' is violated."

// routeProbeHappy 是一次通过的TiDB探针应答脚本，仅验证SQL状态机，不充当引擎或锁的真实验收。
func routeProbeHappy() routeScript {
	return routeScript{}.
		on("SESSION", rsRows([]driver.Value{"READ-COMMITTED", int64(1)})).
		on("VERSION", rsRows([]driver.Value{"8.0.11-TiDB-v8.5.0", "TiDB Server"})).
		on("TXN_MODE", rsRows([]driver.Value{"pessimistic", "READ-COMMITTED", "leader", "strict", "", int64(0), int64(0)})).
		on("META", rsRows([]driver.Value{testRouteGen, int64(1)})).
		on("GUARD", rsOne()).
		on("P4", rsMySQLErr(mysqlErrCheckConstraintViolated, routeCheckViolated)).
		on("P5", rsAffected(1), rsAffected(1))
}

func TestRouteStoreProbe_TiDBHappyPath(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeProbeHappy())
	meta, err := st.probe(routeSQLTestContext())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.Generation != testRouteGen || meta.Writable != 1 {
		t.Fatalf("meta = %+v", meta)
	}
	assertRouteLabels(t, f, "SESSION", "VERSION", "TXN_MODE", "META", "BEGIN", "GUARD", "P4", "P5", "P5", "ROLLBACK")
	p5 := f.callsOf("P5")
	assertRouteArgs(t, p5[0], uint64(0), uint64(64))
	assertRouteArgs(t, p5[1], uint64(63), uint64(63))
}

func TestRouteStoreProbe_TiDBNeedsPessimistic(t *testing.T) {
	script := routeProbeHappy()
	st, f := newFakeRouteSQLStore(t, script)
	if _, err := st.probe(routeSQLTestContext()); err != nil {
		t.Fatalf("pessimistic TiDB must pass: %v", err)
	}
	assertRouteLabels(t, f, "SESSION", "VERSION", "TXN_MODE", "META", "BEGIN", "GUARD", "P4", "P5", "P5", "ROLLBACK")

	st, _ = newFakeRouteSQLStore(t, routeScript{}.
		on("SESSION", rsRows([]driver.Value{"READ-COMMITTED", int64(1)})).
		on("VERSION", rsRows([]driver.Value{"8.0.11-TiDB-v8.5.0", "TiDB Server"})).
		on("TXN_MODE", rsRows([]driver.Value{"optimistic", "READ-COMMITTED", "leader", "strict", "", int64(0), int64(0)})))
	if _, err := st.probe(routeSQLTestContext()); !errors.Is(err, errCurrentTiDB) {
		t.Fatalf("optimistic TiDB error = %v, want errCurrentTiDB", err)
	}
}

func TestRouteStoreProbe_Refusals(t *testing.T) {
	session := func(iso string, wait int64) routeScript {
		return routeScript{}.on("SESSION", rsRows([]driver.Value{iso, wait}))
	}
	untilGuard := func() routeScript {
		return routeScript{}.
			on("SESSION", rsRows([]driver.Value{"READ-COMMITTED", int64(1)})).
			on("VERSION", rsRows([]driver.Value{"8.0.11-TiDB-v8.5.0", "TiDB Server"})).
			on("TXN_MODE", rsRows([]driver.Value{"pessimistic", "READ-COMMITTED", "leader", "strict", "", int64(0), int64(0)})).
			on("META", rsRows([]driver.Value{testRouteGen, int64(1)})).
			on("GUARD", rsOne())
	}
	for _, tc := range []struct {
		name   string
		script routeScript
		want   error
		text   string
	}{
		{"隔离级别不是RC", session("REPEATABLE-READ", 1), errRouteSessionMisconfigured, ""},
		{"锁等待不是1s", session("READ-COMMITTED", 50), errRouteSessionMisconfigured, ""},
		{"route_meta缺行", routeScript{}.on("SESSION", rsRows([]driver.Value{"READ-COMMITTED", int64(1)})).
			on("VERSION", rsRows([]driver.Value{"8.0.11-TiDB-v8.5.0", "TiDB Server"})).
			on("TXN_MODE", rsRows([]driver.Value{"pessimistic", "READ-COMMITTED", "leader", "strict", "", int64(0), int64(0)})).on("META", rsNone()), errRouteMetaMissing, ""},
		{"历史MySQL拒绝", routeScript{}.on("SESSION", rsRows([]driver.Value{"READ-COMMITTED", int64(1)})).on("VERSION", rsRows([]driver.Value{"8.0.36", "MySQL Community Server"})), errCurrentTiDB, ""},
		{"P4被接受_CHECK不生效", untilGuard().on("P4", rsAffected(1)), errRouteCheckNotEnforced, ""},
		{"P4被别的约束拒绝", untilGuard().on("P4", rsMySQLErr(mysqlErrCheckConstraintViolated, "Check constraint 'chk_route_home' is violated.")),
			errRouteCheckNotEnforced, ""},
		{"P5被拒_模数不是64", untilGuard().on("P4", rsMySQLErr(mysqlErrCheckConstraintViolated, routeCheckViolated)).
			on("P5", rsMySQLErr(mysqlErrCheckConstraintViolated, routeCheckViolated)), errRouteShardModulusMismatch, ""},
		{"P4缺INSERT权限", untilGuard().on("P4", rsMySQLErr(mysqlErrTableAccessDenied, "INSERT command denied")), nil, "INSERT privilege"},
		{"P4撞1205不重试", untilGuard().on("P4", rsMySQLErr(mysqlErrLockWaitTimeout, "Lock wait timeout exceeded")), nil, "Lock wait timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newFakeRouteSQLStore(t, tc.script)
			_, err := st.probe(routeSQLTestContext())
			switch {
			case err == nil:
				t.Fatal("probe must refuse")
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("error = %v, want %v", err, tc.want)
			case tc.text != "" && !strings.Contains(err.Error(), tc.text):
				t.Fatalf("error = %v, want it to mention %q", err, tc.text)
			}
		})
	}
}

// TestRouteStoreProbe_DeadlockRetriesWholeProbeTx:P4 + P5 的事务撞 1213 整个重跑(SLEEP 之后新开事务)。
func TestRouteStoreProbe_DeadlockRetriesWholeProbeTx(t *testing.T) {
	script := routeProbeHappy()
	script["GUARD"] = []fakeRouteSQLReply{rsOne(), rsOne()}
	script["P4"] = []fakeRouteSQLReply{rsMySQLErr(mysqlErrDeadlock, "Deadlock found"), rsMySQLErr(mysqlErrCheckConstraintViolated, routeCheckViolated)}
	st, f := newFakeRouteSQLStore(t, script)
	if _, err := st.probe(routeSQLTestContext()); err != nil {
		t.Fatalf("probe after one deadlock: %v", err)
	}
	assertRouteLabels(t, f, "SESSION", "VERSION", "TXN_MODE", "META", "BEGIN", "GUARD", "P4", "ROLLBACK", "SLEEP",
		"BEGIN", "GUARD", "P4", "P5", "P5", "ROLLBACK")
}

// ── 参数 ──────────────────────────────────────────────────────────────────────

func TestRouteSourceFlags(t *testing.T) {
	const dsn = "u:p@tcp(127.0.0.1:3306)/testdb"
	opt := func(mode, source, routeDSN string, local, replicas, timeoutMs int) options {
		return options{mode: mode, routeSource: source, routeDBDSN: routeDSN, waitAOFLocal: local, waitAOFReplicas: replicas, waitAOFTimeoutMs: timeoutMs}
	}
	backfill := opt("merge", "", "", 1, 0, 1500)
	backfill.backfill = true
	for _, tc := range []struct {
		name string
		o    options
		want string
	}{
		{"merge缺source", opt("merge", "", "", 1, 0, 1500), "-route-source is required"},
		{"audit缺source", opt("audit", "", "", 1, 0, 1500), "-route-source is required"},
		{"storage-audit缺source", opt("storage-audit", "", "", 1, 0, 1500), "-route-source is required"},
		{"relocate缺source", opt("relocate", "", "", 1, 0, 1500), "-route-source is required"},
		{"backfill缺source", backfill, "-route-source is required"},
		{"capability-check不需要", opt(modeCapabilityCheck, "", "", 1, 0, 1500), ""},
		{"未知模式交给后面", opt("bogus", "", "", 1, 0, 1500), ""},
		{"db缺dsn", opt("merge", routeSourceDB, "", 1, 0, 1500), "requires -route-db-dsn"},
		{"db合法", opt("merge", routeSourceDB, dsn, 1, 0, 1500), ""},
		{"db_redis-ha份数", opt("unmerge", routeSourceDB, dsn, 1, 1, 1500), ""},
		{"db两项都是0", opt("merge", routeSourceDB, dsn, 0, 0, 1500), "both 0"},
		{"db_local越界", opt("merge", routeSourceDB, dsn, 2, 0, 1500), "-waitaof-local"},
		{"db_replicas为负", opt("merge", routeSourceDB, dsn, 1, -1, 1500), "-waitaof-replicas"},
		{"db_timeout为0", opt("merge", routeSourceDB, dsn, 1, 0, 0), "-waitaof-timeout-ms"},
		{"db_timeout过大", opt("merge", routeSourceDB, dsn, 1, 0, 2001), "-waitaof-timeout-ms"},
		{"redis合法", opt("merge", routeSourceRedis, "", 1, 0, 1500), ""},
		{"redis却给了dsn", opt("pin-placement", routeSourceRedis, dsn, 1, 0, 1500), "only used with -route-source=db"},
		{"未知source", opt("merge", "mysql", "", 1, 0, 1500), "want db or redis"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRouteSourceFlags(tc.o)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkRouteSourceFlags: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNewMergeRouteWriter(t *testing.T) {
	if got := newMergeRouteWriter("host-1", testRouteRun); got != "merge_zone@host-1:"+testRouteRun {
		t.Fatalf("writer = %q", got)
	}
	if got := newMergeRouteWriter("宿主 机/1", ""); got != "merge_zone@1:norun" {
		t.Fatalf("sanitized writer = %q", got)
	}
	if got := newMergeRouteWriter("", "run"); got != "merge_zone@unknown:run" {
		t.Fatalf("empty host writer = %q", got)
	}
	long := newMergeRouteWriter(strings.Repeat("h", 200)+"中文", strings.Repeat("r", 80))
	if len(long) > routeWriterMaxLen || !validRouteWriter(long) || !strings.HasSuffix(long, ":"+strings.Repeat("r", routeWriterRunIDPrefix)) {
		t.Fatalf("long writer = %q (len %d)", long, len(long))
	}
}

func TestPrepareRouteBatch(t *testing.T) {
	got, err := prepareRouteBatch([]uint64{130, 7, 65, 64}, 10)
	if err != nil {
		t.Fatalf("prepareRouteBatch: %v", err)
	}
	// 按聚簇键 (id % 64, id) 升序:64(0)、65(1)、130(2)、7(7)。
	if want := []uint64{64, 65, 130, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for _, bad := range [][]uint64{{0}, {5, 5}, {1, 2, 3}} {
		if _, err := prepareRouteBatch(bad, 2); !errors.Is(err, errRouteInvalidArgument) {
			t.Fatalf("prepareRouteBatch(%v) error = %v, want errRouteInvalidArgument", bad, err)
		}
	}
	if got, err := prepareRouteBatch(nil, 2); err != nil || len(got) != 0 {
		t.Fatalf("empty batch = (%v, %v)", got, err)
	}
}

func TestRoutePlacementReadMatchesRedisEncoding(t *testing.T) {
	rd := routePlacementRead(mergeRouteRow{PlayerID: 9, HomeZone: 101, StorageID: 5, PlacementVersion: 3, FrozenRunID: "run1", Rev: 4}, true)
	if rd.RecordRaw != "5:3:frozen:run1" || !rd.RecordPresent || rd.RecordErr != nil || rd.HomeRaw != "101" || !rd.HomePresent || rd.Home != 101 {
		t.Fatalf("read = %+v", rd)
	}
	if rd := routePlacementRead(mergeRouteRow{PlayerID: 9, HomeZone: 101, Rev: 1}, true); rd.RecordPresent || rd.HomeRaw != "101" {
		t.Fatalf("home only = %+v", rd)
	}
	if rd := routePlacementRead(mergeRouteRow{}, false); rd != (placementRead{}) {
		t.Fatalf("absent row = %+v", rd)
	}
	bad := routePlacementRead(mergeRouteRow{PlayerID: 9, HomeZone: 101, PlacementVersion: 3, Rev: 1}, true)
	if !bad.RecordPresent || !errors.Is(bad.RecordErr, errPlacementMalformed) {
		t.Fatalf("malformed placement columns must fail closed: %+v", bad)
	}
}

// ── 重试 ──────────────────────────────────────────────────────────────────────

// TestRouteTxRetriesDeadlockNotLockWait:1213 整事务重跑(退避后新开事务),1205 不重试,4 次 1213 后放弃且错误仍带 MySQLError。
func TestRouteTxRetriesDeadlockNotLockWait(t *testing.T) {
	const mark = "101-102-20261018T000000Z-cafebabe"
	held := func() routeScript {
		return routeScript{}.on("FENCE_EXISTS", rsOne(), rsOne())
	}

	script := held().on("WRITABLE", rsOne(), rsOne()).on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun),
		rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).on("DELETE_MERGED", rsMySQLErr(mysqlErrDeadlock, "Deadlock found"), rsAffected(1))
	st, f := newFakeRouteSQLStore(t, script)
	deleted, err := st.ClearMergedInto(routeSQLTestContext(), 101, 102, testRouteRun, mark)
	if err != nil || !deleted {
		t.Fatalf("ClearMergedInto after one deadlock = (%v, %v)", deleted, err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "DELETE_MERGED", "ROLLBACK",
		"SLEEP", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "DELETE_MERGED", "COMMIT")

	st, f = newFakeRouteSQLStore(t, held().on("WRITABLE", rsOne()).on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).
		on("DELETE_MERGED", rsMySQLErr(mysqlErrLockWaitTimeout, "Lock wait timeout exceeded")))
	_, err = st.ClearMergedInto(routeSQLTestContext(), 101, 102, testRouteRun, mark)
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlErrLockWaitTimeout {
		t.Fatalf("1205 error = %v, want the MySQLError unretried", err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "DELETE_MERGED", "ROLLBACK")

	script = held()
	for i := 0; i < routeTxAttempts; i++ {
		script.on("WRITABLE", rsOne()).on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).
			on("DELETE_MERGED", rsMySQLErr(mysqlErrDeadlock, "Deadlock found"))
	}
	st, _ = newFakeRouteSQLStore(t, script)
	_, err = st.ClearMergedInto(routeSQLTestContext(), 101, 102, testRouteRun, mark)
	if !errors.As(err, &me) || me.Number != mysqlErrDeadlock || !strings.Contains(err.Error(), "gave up after 4 attempts") {
		t.Fatalf("exhausted retries error = %v", err)
	}
}

// ── 围栏 ──────────────────────────────────────────────────────────────────────

func testFenceAcquire(run string) routeFenceAcquire {
	return routeFenceAcquire{Zones: []uint32{102, 101}, RunID: run, Purpose: routeFencePurposeMerge, Src: 101, Dst: 102,
		Holder: mergeInProgressValue{Operator: "unit/pid=1", ManifestPath: "merge_101_to_102.json"}, TTL: time.Hour}
}

func TestAcquireFence_EpochHolderAndStatements(t *testing.T) {
	const now = int64(1_760_000_000_000)
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("ENSURE_FENCE", rsAffected(128)).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE_ALL", rsFenceAll("", false, 4), rsFenceAll("old-run", false, 9)).
		on("NOW", rsRows([]driver.Value{now})).
		on("ACQUIRE", rsAffected(64), rsAffected(64)))
	grant, err := st.AcquireFence(routeSQLTestContext(), testFenceAcquire(testRouteRun))
	if err != nil {
		t.Fatalf("AcquireFence: %v", err)
	}
	wantExpires := uint64(now) + uint64(time.Hour.Milliseconds())
	if grant.Epoch != 10 || grant.ExpiresAtMs != wantExpires {
		t.Fatalf("grant = %+v, want epoch 10 (max 9 + 1) expires %d", grant, wantExpires)
	}
	if epoch, ok := routeFenceHolderEpoch(grant.HolderJSON); !ok || epoch != 10 {
		t.Fatalf("holder %s carries fence_epoch (%d, %v), want 10", grant.HolderJSON, epoch, ok)
	}
	var holder mergeInProgressValue
	if err := json.Unmarshal([]byte(grant.HolderJSON), &holder); err != nil {
		t.Fatalf("holder json: %v", err)
	}
	if holder.RunID != testRouteRun || holder.SourceZone != 101 || holder.TargetZone != 102 ||
		holder.ExpiresAtUnixMs != int64(wantExpires) || holder.Tool != "tools/merge_zone" || holder.ManifestPath != "merge_101_to_102.json" {
		t.Fatalf("holder = %+v", holder)
	}
	assertRouteLabels(t, f, "ENSURE_FENCE", "BEGIN", "WRITABLE", "LOCK_FENCE_ALL", "LOCK_FENCE_ALL", "NOW", "ACQUIRE", "ACQUIRE", "COMMIT")
	ensure := f.callsOf("ENSURE_FENCE")[0]
	if ensure.query != routeEnsureFenceSQL(128) || len(ensure.args) != 256 {
		t.Fatalf("ensure must be one ODKU over 2 x 64 shard rows: %d args, query %q", len(ensure.args), ensure.query)
	}
	if ensure.args[0] != uint64(101) || ensure.args[1] != uint64(0) || ensure.args[254] != uint64(102) || ensure.args[255] != uint64(63) {
		t.Fatalf("ensure must cover (101,0)..(102,63) in ascending order: first %v %v last %v %v",
			ensure.args[0], ensure.args[1], ensure.args[254], ensure.args[255])
	}
	locks := f.callsOf("LOCK_FENCE_ALL")
	assertRouteArgs(t, locks[0], uint64(101), uint64(64))
	assertRouteArgs(t, locks[1], uint64(102), uint64(64))
	acq := f.callsOf("ACQUIRE")
	assertRouteArgs(t, acq[0], testRouteRun, routeFencePurposeMerge, uint64(101), uint64(102), wantExpires, grant.HolderJSON, uint64(10), uint64(101), uint64(64))
	assertRouteArgs(t, acq[1], testRouteRun, routeFencePurposeMerge, uint64(101), uint64(102), wantExpires, grant.HolderJSON, uint64(10), uint64(102), uint64(64))
}

func TestAcquireFence_RefusesAnotherLiveHolderAllowsTheSameRun(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("ENSURE_FENCE", rsAffected(0)).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE_ALL", rsFenceAll("other-run", true, 3)))
	if _, err := st.AcquireFence(routeSQLTestContext(), testFenceAcquire(testRouteRun)); !errors.Is(err, errRouteFenceHeld) {
		t.Fatalf("error = %v, want errRouteFenceHeld", err)
	}
	assertRouteLabels(t, f, "ENSURE_FENCE", "BEGIN", "WRITABLE", "LOCK_FENCE_ALL", "ROLLBACK")

	st, _ = newFakeRouteSQLStore(t, routeScript{}.
		on("ENSURE_FENCE", rsAffected(0)).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE_ALL", rsFenceAll(testRouteRun, true, 3), rsFenceAll(testRouteRun, true, 3)).
		on("NOW", rsRows([]driver.Value{int64(1_760_000_000_000)})).
		on("ACQUIRE", rsAffected(64), rsAffected(64)))
	grant, err := st.AcquireFence(routeSQLTestContext(), testFenceAcquire(testRouteRun))
	if err != nil || grant.Epoch != 4 {
		t.Fatalf("re-acquiring this run's own fence = (%+v, %v), want epoch 4", grant, err)
	}

	st, f = newFakeRouteSQLStore(t, routeScript{}.on("ENSURE_FENCE", rsAffected(0)).on("WRITABLE", rsRows([]driver.Value{int64(0)})))
	if _, err := st.AcquireFence(routeSQLTestContext(), testFenceAcquire(testRouteRun)); !errors.Is(err, errRouteNotWritable) {
		t.Fatalf("frozen error = %v, want errRouteNotWritable", err)
	}
	assertRouteLabels(t, f, "ENSURE_FENCE", "BEGIN", "WRITABLE", "ROLLBACK")

	bad := testFenceAcquire(testRouteRun)
	bad.TTL = time.Minute
	if _, err := st.AcquireFence(routeSQLTestContext(), bad); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("short ttl error = %v", err)
	}
}

// TestTakeOverFence_OnlyThePreviousRunIsTakenOver:接手与获取同一个事务形状,只多放行「生效且 run_id == previousRunID」的分片;
// 第三个 run_id 生效照样 errRouteFenceHeld;previousRunID 不合法 / 等于本次直接拒绝、一条语句都不发。
func TestTakeOverFence_OnlyThePreviousRunIsTakenOver(t *testing.T) {
	const prev = "101-102-20261018T000000Z-0ld0ld00"
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("ENSURE_FENCE", rsAffected(0)).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE_ALL", rsFenceAll(prev, true, 5), rsFenceAll(prev, true, 5)).
		on("NOW", rsRows([]driver.Value{int64(1_760_000_000_000)})).
		on("ACQUIRE", rsAffected(64), rsAffected(64)))
	grant, err := st.TakeOverFence(routeSQLTestContext(), testFenceAcquire(testRouteRun), prev)
	if err != nil || grant.Epoch != 6 {
		t.Fatalf("take over = (%+v, %v), want epoch 6", grant, err)
	}
	assertRouteLabels(t, f, "ENSURE_FENCE", "BEGIN", "WRITABLE", "LOCK_FENCE_ALL", "LOCK_FENCE_ALL", "NOW", "ACQUIRE", "ACQUIRE", "COMMIT")
	assertRouteArgs(t, f.callsOf("ACQUIRE")[0], testRouteRun, routeFencePurposeMerge, uint64(101), uint64(102),
		grant.ExpiresAtMs, grant.HolderJSON, uint64(6), uint64(101), uint64(64))

	st, f = newFakeRouteSQLStore(t, routeScript{}.
		on("ENSURE_FENCE", rsAffected(0)).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE_ALL", rsFenceAll("third-run", true, 5)))
	_, err = st.TakeOverFence(routeSQLTestContext(), testFenceAcquire(testRouteRun), prev)
	if !errors.Is(err, errRouteFenceHeld) {
		t.Fatalf("a third run's live fence: error = %v, want errRouteFenceHeld", err)
	}
	// MZ2-5:Held 文案点名持有者并给出唯一的出路(工具没有别的释放入口),不再说「release it through the tool that owns it」。
	if msg := err.Error(); !strings.Contains(msg, "-take-over-fence third-run") || !strings.Contains(msg, "same -manifest-path") ||
		strings.Contains(msg, "tool that owns it") {
		t.Fatalf("held message = %q", msg)
	}
	assertRouteLabels(t, f, "ENSURE_FENCE", "BEGIN", "WRITABLE", "LOCK_FENCE_ALL", "ROLLBACK")

	for _, bad := range []string{"", testRouteRun, "bad run"} {
		st, f = newFakeRouteSQLStore(t, routeScript{})
		if _, err := st.TakeOverFence(routeSQLTestContext(), testFenceAcquire(testRouteRun), bad); !errors.Is(err, errRouteInvalidArgument) {
			t.Fatalf("previous run %q: error = %v, want errRouteInvalidArgument", bad, err)
		}
		if got := f.labels(); len(got) != 0 {
			t.Fatalf("previous run %q sent statements %v", bad, got)
		}
	}
}

// TestTakeOverFence_RefusesAMismatchedOrPartialFence:被接手的分片必须记着本次的 zone 对、holder_json 里的清单路径与本次相同,
// 且每个 zone 要么 64 片全接、要么一片不接;不满足一律 errRouteInvalidArgument,事务在第一次写之前回滚,旧围栏原样留在 previousRunID 上。
// (MZ2-2 / B1-2:zone 对写错时只接走一区、另一区挂在已死的运行上过期解封;续跑落到别的清单会被当成首跑。)
func TestTakeOverFence_RefusesAMismatchedOrPartialFence(t *testing.T) {
	const prev = "101-102-20261018T000000Z-0ld0ld00"
	partial := rsFenceAll(prev, true, 5)
	partial.rows[10] = []driver.Value{int64(10), nil, int64(0), int64(5), nil, nil, nil}
	for _, tc := range []struct {
		name  string
		zone  fakeRouteSQLReply
		wants []string
	}{
		{"wrong-zone-pair", rsFenceAllHeld(prev, true, 5, 101, 103, `{"run_id":"`+prev+`"}`), []string{"zone pair 101 → 103", "this run is 101 → 102"}},
		{"reversed-zone-pair", rsFenceAllHeld(prev, true, 5, 102, 101, `{"run_id":"`+prev+`"}`), []string{"zone pair 102 → 101"}},
		{"other-manifest", rsFenceAllHeld(prev, true, 5, 101, 102, `{"run_id":"`+prev+`","manifest_path":"/srv/merge_101_to_102_old.json"}`),
			[]string{"-manifest-path", "/srv/merge_101_to_102_old.json"}},
		{"unreadable-holder", rsFenceAllHeld(prev, true, 5, 101, 102, `{"run_id":`), []string{"holder_json is not readable"}},
		{"partial", partial, []string{"only 63 of 64 shards"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f := newFakeRouteSQLStore(t, routeScript{}.
				on("ENSURE_FENCE", rsAffected(0)).
				on("WRITABLE", rsOne()).
				on("LOCK_FENCE_ALL", tc.zone))
			_, err := st.TakeOverFence(routeSQLTestContext(), testFenceAcquire(testRouteRun), prev)
			if !errors.Is(err, errRouteInvalidArgument) {
				t.Fatalf("error = %v, want errRouteInvalidArgument", err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lacks %q", err, want)
				}
			}
			assertRouteLabels(t, f, "ENSURE_FENCE", "BEGIN", "WRITABLE", "LOCK_FENCE_ALL", "ROLLBACK")
		})
	}

	// 同一份清单(路径写法不同,filepath.Clean 后相同)照常接手;那次运行在另一区的围栏已按库时钟过期 = 那一区按普通获取(接 0 片)。
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("ENSURE_FENCE", rsAffected(0)).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE_ALL", rsFenceAllHeld(prev, true, 5, 101, 102, `{"run_id":"`+prev+`","manifest_path":"./merge_101_to_102.json"}`),
			rsFenceAll(prev, false, 5)).
		on("NOW", rsRows([]driver.Value{int64(1_760_000_000_000)})).
		on("ACQUIRE", rsAffected(64), rsAffected(64)))
	grant, err := st.TakeOverFence(routeSQLTestContext(), testFenceAcquire(testRouteRun), prev)
	if err != nil || grant.Epoch != 6 {
		t.Fatalf("take over with the same manifest = (%+v, %v), want epoch 6", grant, err)
	}
	assertRouteLabels(t, f, "ENSURE_FENCE", "BEGIN", "WRITABLE", "LOCK_FENCE_ALL", "LOCK_FENCE_ALL", "NOW", "ACQUIRE", "ACQUIRE", "COMMIT")
}

// TestCheckTakeOverShard:接手核对的纯判定 —— 只比 zone 对与清单路径,不比 purpose(合服中止后改跑撤销是认可的续跑路径)。
func TestCheckTakeOverShard(t *testing.T) {
	req := testFenceAcquire(testRouteRun)
	req.Purpose = routeFencePurposeUnmerge
	st := routeFenceShardState{shard: 63, runID: "prev", active: true, srcZone: 101, dstZone: 102,
		holderJSON: `{"run_id":"prev","manifest_path":"merge_101_to_102.json"}`}
	if err := checkTakeOverShard(101, st, req); err != nil {
		t.Fatalf("same zone pair and manifest, other purpose: %v", err)
	}
	st.holderJSON = ""
	if err := checkTakeOverShard(101, st, req); err != nil {
		t.Fatalf("no holder_json (nothing to compare the manifest with): %v", err)
	}
	st.holderJSON = `{"run_id":"prev"}`
	if err := checkTakeOverShard(101, st, req); err != nil {
		t.Fatalf("holder_json without manifest_path: %v", err)
	}
	st.srcZone = 0
	if err := checkTakeOverShard(101, st, req); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("NULL src_zone on a live shard = %v, want errRouteInvalidArgument", err)
	}
}

func TestRenewFence(t *testing.T) {
	const now = int64(1_760_000_000_000)
	ttl := 90 * time.Minute
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("NOW", rsRows([]driver.Value{now})).
		on("RENEW", rsAffected(64), rsAffected(64)).
		on("FENCE_EXPIRY", rsRows([]driver.Value{int64(now + 5_400_000)}), rsRows([]driver.Value{int64(now + 5_400_001)})))
	exp, err := st.RenewFence(routeSQLTestContext(), []uint32{102, 101}, testRouteRun, ttl)
	if err != nil {
		t.Fatalf("RenewFence: %v", err)
	}
	if exp[101] != uint64(now+5_400_000) || exp[102] != uint64(now+5_400_001) {
		t.Fatalf("expiries = %v", exp)
	}
	assertRouteLabels(t, f, "BEGIN", "NOW", "RENEW", "RENEW", "FENCE_EXPIRY", "FENCE_EXPIRY", "COMMIT")
	assertRouteArgs(t, f.callsOf("RENEW")[0], uint64(now)+uint64(ttl.Milliseconds()), uint64(101), uint64(64), testRouteRun, uint64(now))

	// 受影响行数不足 = 确定失权:整笔回滚,不读过期时刻。
	st, f = newFakeRouteSQLStore(t, routeScript{}.on("NOW", rsRows([]driver.Value{now})).on("RENEW", rsAffected(64), rsAffected(0)))
	if _, err := st.RenewFence(routeSQLTestContext(), []uint32{101, 102}, testRouteRun, ttl); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("short renew error = %v, want errRouteFenceLost", err)
	}
	assertRouteLabels(t, f, "BEGIN", "NOW", "RENEW", "RENEW", "ROLLBACK")

	// 1205:原样返回,不是失权(调用方退避重试)。
	st, _ = newFakeRouteSQLStore(t, routeScript{}.on("NOW", rsRows([]driver.Value{now})).
		on("RENEW", rsMySQLErr(mysqlErrLockWaitTimeout, "Lock wait timeout exceeded")))
	_, err = st.RenewFence(routeSQLTestContext(), []uint32{101}, testRouteRun, ttl)
	var me *mysql.MySQLError
	if errors.Is(err, errRouteFenceLost) || !errors.As(err, &me) || me.Number != mysqlErrLockWaitTimeout {
		t.Fatalf("1205 renew error = %v, want the MySQLError and not errRouteFenceLost", err)
	}
}

func TestReleaseFence_ReturnsPerZoneCounts(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.on("RELEASE", rsAffected(64), rsAffected(0)))
	got, err := st.ReleaseFence(routeSQLTestContext(), []uint32{102, 101}, testRouteRun)
	if err != nil {
		t.Fatalf("ReleaseFence: %v", err)
	}
	if got[101] != 64 || got[102] != 0 {
		t.Fatalf("released = %v", got)
	}
	assertRouteLabels(t, f, "BEGIN", "RELEASE", "RELEASE", "COMMIT")
	assertRouteArgs(t, f.callsOf("RELEASE")[0], uint64(101), uint64(64), testRouteRun)
}

// ── 失权判据 ──────────────────────────────────────────────────────────────────

type fakeFenceRenewer struct {
	errs  []error
	calls int
}

func (r *fakeFenceRenewer) RenewFence(context.Context, []uint32, string, time.Duration) (map[uint32]uint64, error) {
	r.calls++
	if len(r.errs) > 0 {
		err := r.errs[0]
		r.errs = r.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	return map[uint32]uint64{101: 1_760_000_000_000}, nil
}

func TestRouteFenceGuard_LocalDeadlineAndLoss(t *testing.T) {
	t0 := time.Unix(1_760_000_000, 0)
	if got := routeFenceLocalDeadline(t0, time.Hour); !got.Equal(t0.Add(59 * time.Minute)) {
		t.Fatalf("deadline = %v, want last renewal + ttl - 60s", got)
	}
	r := &fakeFenceRenewer{}
	g, err := newRouteFenceGuard(r, []uint32{101, 102}, testRouteRun, time.Hour, t0)
	if err != nil {
		t.Fatalf("newRouteFenceGuard: %v", err)
	}
	now := t0
	g.now = func() time.Time { return now }

	now = t0.Add(58*time.Minute + 59*time.Second)
	if err := g.Check(); err != nil {
		t.Fatalf("before the local deadline: %v", err)
	}
	now = t0.Add(59 * time.Minute)
	if err := g.Check(); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("at the local deadline error = %v, want errRouteFenceLost", err)
	}

	// 续期成功把 lastOK 推到这次续期开始的时刻。
	now = t0.Add(30 * time.Minute)
	var refreshed map[uint32]uint64
	g.onRenewed = func(_ context.Context, exp map[uint32]uint64) error {
		refreshed = exp
		return errors.New("mirror refresh failed")
	}
	if err := g.renewOnce(routeSQLTestContext()); err != nil {
		t.Fatalf("renewOnce: %v", err)
	}
	if refreshed == nil {
		t.Fatal("onRenewed must see the new expiries")
	}
	now = t0.Add(88 * time.Minute)
	if err := g.Check(); err != nil {
		t.Fatalf("after a renewal at +30m the deadline is +89m: %v", err)
	}

	// 1205 / 网络错误不当失权。
	r.errs = []error{&mysql.MySQLError{Number: mysqlErrLockWaitTimeout}}
	if err := g.renewOnce(routeSQLTestContext()); err == nil || errors.Is(err, errRouteFenceLost) {
		t.Fatalf("transient renew error = %v", err)
	}
	if err := g.Check(); err != nil {
		t.Fatalf("a transient renew failure must not mark the fence lost: %v", err)
	}

	// 受影响行数不足 = 确定失权,之后 Check 一直失败。
	r.errs = []error{fmt.Errorf("%w: zone 101 renewed 0/64 shards", errRouteFenceLost)}
	if err := g.renewOnce(routeSQLTestContext()); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("lost renew error = %v", err)
	}
	if err := g.Check(); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("Check after loss = %v", err)
	}

	if _, err := newRouteFenceGuard(r, []uint32{101}, testRouteRun, 2*time.Minute, t0); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("ttl not above twice the safety margin must be refused: %v", err)
	}
}

// TestRouteFenceGuard_LoopRetriesTransientAndStopsOnLoss:续期循环成功后按 interval 等,瞬时错误按 retryWait 重试,
// 确定失权即退出(不再续期)。interval = min(ttl/3, routeFenceRenewMaxInterval):ttl=1h 时是 30s 而不是 20 分钟
// (MZ2-3:被接手而其实还活着的旧进程,不碰映射的写只能靠下一次续期失败才停)。睡眠注入,不依赖墙钟。
func TestRouteFenceGuard_LoopRetriesTransientAndStopsOnLoss(t *testing.T) {
	r := &fakeFenceRenewer{errs: []error{errors.New("connection refused"), nil, fmt.Errorf("%w: zone 101", errRouteFenceLost)}}
	g, err := newRouteFenceGuard(r, []uint32{101}, testRouteRun, time.Hour, time.Unix(1_760_000_000, 0))
	if err != nil {
		t.Fatalf("newRouteFenceGuard: %v", err)
	}
	var waits []time.Duration
	g.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	g.done = make(chan struct{})
	g.loop(routeSQLTestContext())
	if want := []time.Duration{routeFenceRenewMaxInterval, routeFenceRetryWait, routeFenceRenewMaxInterval}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	if r.calls != 3 {
		t.Fatalf("renew calls = %d, want 3 (the loop stops on loss)", r.calls)
	}
	if err := g.Check(); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("Check after loop exit = %v", err)
	}
}

// ── 批原语 ────────────────────────────────────────────────────────────────────

func TestRemapHomeBatch_StatementSequenceAndTarget(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("FENCE_EXISTS", rsOne(), rsOne()).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).
		on("LOCK_ROWS", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 65, HomeZone: 101, Rev: 3}),
			rsRouteRow(mergeRouteRow{PlayerID: 130, HomeZone: 102, StorageID: 101, PlacementVersion: 1, Rev: 5}),
			rsRouteRow(mergeRouteRow{PlayerID: 7, HomeZone: 999, Rev: 1}))).
		on("MOVE_HOME", rsAffected(1)))
	res, err := st.RemapHomeBatch(routeSQLTestContext(), 101, 102, testRouteRun, []uint64{200, 7, 130, 65})
	if err != nil {
		t.Fatalf("RemapHomeBatch: %v", err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "LOCK_ROWS", "MOVE_HOME", "COMMIT")
	fences := f.callsOf("LOCK_FENCE")
	assertRouteArgs(t, fences[0], uint64(101), uint64(63))
	assertRouteArgs(t, fences[1], uint64(102), uint64(63))
	lock := f.callsOf("LOCK_ROWS")[0]
	if lock.query != routeSelectRowsSQL(4, true) {
		t.Fatalf("lock query = %q", lock.query)
	}
	assertRouteArgs(t, lock, uint64(1), uint64(65), uint64(2), uint64(130), uint64(7), uint64(7), uint64(8), uint64(200))
	assertRouteArgs(t, f.callsOf("MOVE_HOME")[0], uint64(102), testRouteWriter, uint64(1), uint64(65), uint64(3), uint64(101))

	wantVerdicts := map[uint64]mappingOutcome{65: mappingChanged, 130: mappingAlready, 7: mappingForeign, 200: mappingMissing}
	if !reflect.DeepEqual(res.Verdicts, wantVerdicts) || res.Changed != 1 {
		t.Fatalf("verdicts = %v changed = %d", res.Verdicts, res.Changed)
	}
	wantTarget := []mergeRouteRow{
		{PlayerID: 65, HomeZone: 102, Rev: 4},
		{PlayerID: 130, HomeZone: 102, StorageID: 101, PlacementVersion: 1, Rev: 5},
	}
	if !reflect.DeepEqual(res.Target, wantTarget) {
		t.Fatalf("target = %+v, want %+v", res.Target, wantTarget)
	}
}

func TestRemapHomeBatch_RefusalsWriteNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script routeScript
		want   error
		labels []string
	}{
		{"围栏被别的运行持有", routeScript{}.on("FENCE_EXISTS", rsOne(), rsOne()).on("WRITABLE", rsOne()).on("LOCK_FENCE", rsFenceHeld("other-run")),
			errRouteFenceLost, []string{"FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "ROLLBACK"}},
		{"围栏按库时钟过期", routeScript{}.on("FENCE_EXISTS", rsOne(), rsOne()).on("WRITABLE", rsOne()).on("LOCK_FENCE", rsFenceExpired(testRouteRun)),
			errRouteFenceLost, []string{"FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "ROLLBACK"}},
		{"围栏片行读不到", routeScript{}.on("FENCE_EXISTS", rsOne(), rsOne()).on("WRITABLE", rsOne()).on("LOCK_FENCE", rsNone()),
			errRouteFenceRowMissing, []string{"FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "ROLLBACK"}},
		{"映射写者冻结", routeScript{}.on("FENCE_EXISTS", rsOne(), rsOne()).on("WRITABLE", rsRows([]driver.Value{int64(0)})),
			errRouteNotWritable, []string{"FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "ROLLBACK"}},
		{"CAS受影响0行", routeScript{}.on("FENCE_EXISTS", rsOne(), rsOne()).on("WRITABLE", rsOne()).
			on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).
			on("LOCK_ROWS", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 65, HomeZone: 101, Rev: 3}))).on("MOVE_HOME", rsAffected(0)),
			errRouteInvariant, []string{"FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "LOCK_ROWS", "MOVE_HOME", "ROLLBACK"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f := newFakeRouteSQLStore(t, tc.script)
			if _, err := st.RemapHomeBatch(routeSQLTestContext(), 101, 102, testRouteRun, []uint64{65}); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			assertRouteLabels(t, f, tc.labels...)
		})
	}
}

// TestRemapHomeBatch_EnsuresMissingCheckShard:63 号片行不在时先在事务外 ODKU 补上(只补这一片),再进事务。
func TestRemapHomeBatch_EnsuresMissingCheckShard(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("FENCE_EXISTS", rsNone(), rsOne()).
		on("ENSURE_FENCE", rsAffected(1)).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).
		on("LOCK_ROWS", rsNone()))
	res, err := st.RemapHomeBatch(routeSQLTestContext(), 101, 102, testRouteRun, []uint64{65})
	if err != nil || res.Verdicts[65] != mappingMissing || len(res.Target) != 0 {
		t.Fatalf("RemapHomeBatch = (%+v, %v)", res, err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "ENSURE_FENCE", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "LOCK_ROWS", "COMMIT")
	assertRouteArgs(t, f.callsOf("ENSURE_FENCE")[0], uint64(101), uint64(63))
}

func TestRestoreHomeBatch_MovesDstBackToSrc(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("FENCE_EXISTS", rsOne(), rsOne()).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).
		on("LOCK_ROWS", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 65, HomeZone: 102, Rev: 4}), rsRouteRow(mergeRouteRow{PlayerID: 66, HomeZone: 101, Rev: 2}))).
		on("MOVE_HOME", rsAffected(1)))
	res, err := st.RestoreHomeBatch(routeSQLTestContext(), 101, 102, testRouteRun, []uint64{65, 66})
	if err != nil {
		t.Fatalf("RestoreHomeBatch: %v", err)
	}
	assertRouteArgs(t, f.callsOf("MOVE_HOME")[0], uint64(101), testRouteWriter, uint64(1), uint64(65), uint64(4), uint64(102))
	if res.Verdicts[65] != mappingChanged || res.Verdicts[66] != mappingAlready || len(res.Target) != 2 || res.Target[0].HomeZone != 101 || res.Target[0].Rev != 5 {
		t.Fatalf("restore result = %+v", res)
	}
}

func TestPinPlacementBatch_Verdicts(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("FENCE_EXISTS", rsOne(), rsOne()).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).
		on("LOCK_ROWS", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 64, HomeZone: 101, Rev: 1}),
			rsRouteRow(mergeRouteRow{PlayerID: 1, HomeZone: 101, StorageID: 101, PlacementVersion: 1, Rev: 2}),
			rsRouteRow(mergeRouteRow{PlayerID: 2, HomeZone: 101, StorageID: 5, PlacementVersion: 3, Rev: 4}),
			rsRouteRow(mergeRouteRow{PlayerID: 3, HomeZone: 102, Rev: 1}))).
		on("PIN", rsAffected(1)))
	res, err := st.PinPlacementBatch(routeSQLTestContext(), 101, 102, testRouteRun, []uint64{1, 2, 3, 4, 64})
	if err != nil {
		t.Fatalf("PinPlacementBatch: %v", err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "LOCK_ROWS", "PIN", "COMMIT")
	assertRouteArgs(t, f.callsOf("PIN")[0], uint64(101), testRouteWriter, uint64(0), uint64(64), uint64(1), uint64(101))
	want := map[uint64]pinVerdict{64: pinVerdictPin, 1: pinVerdictAlreadyPinned, 2: pinVerdictKept, 3: pinVerdictHomeMoved, 4: pinVerdictHomeMoved}
	if !reflect.DeepEqual(res.Verdicts, want) || res.Changed != 1 {
		t.Fatalf("verdicts = %v changed = %d", res.Verdicts, res.Changed)
	}
	var ids []uint64
	for _, r := range res.Target {
		ids = append(ids, r.PlayerID)
	}
	if !reflect.DeepEqual(ids, []uint64{1, 2, 64}) || res.Target[2].StorageID != 101 || res.Target[2].PlacementVersion != 1 || res.Target[2].Rev != 2 {
		t.Fatalf("target = %+v", res.Target)
	}
}

// TestPinZonePlacementBatch_CursorTemplateAndTarget:W10 用提案模板的键集游标(不用行比较)、≤ 200、围栏未生效 + 无 merged_into。
func TestPinZonePlacementBatch_CursorTemplateAndTarget(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("FENCE_EXISTS", rsOne()).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE", rsFenceFree()).
		on("LOCK_MERGED", rsNone()).
		on("PIN_ZONE_SELECT", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 64, HomeZone: 101, Rev: 1}), rsRouteRow(mergeRouteRow{PlayerID: 129, HomeZone: 101, Rev: 6}))).
		on("PIN", rsAffected(1), rsAffected(1)))
	res, next, done, err := st.PinZonePlacementBatch(routeSQLTestContext(), 101, routeScanCursor{}, 2)
	if err != nil {
		t.Fatalf("PinZonePlacementBatch: %v", err)
	}
	if !strings.Contains(routePinZoneSelectSQL, "(route_shard > ? OR (route_shard = ? AND player_id > ?))") ||
		strings.Contains(routePinZoneSelectSQL, "(route_shard, player_id) >") {
		t.Fatalf("W10 must use the proposal's keyset template, not a row comparison: %s", routePinZoneSelectSQL)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_MERGED", "PIN_ZONE_SELECT", "PIN", "PIN", "COMMIT")
	assertRouteArgs(t, f.callsOf("PIN_ZONE_SELECT")[0], uint64(101), uint64(0), uint64(0), uint64(0), int64(2))
	if next != (routeScanCursor{Shard: 1, PlayerID: 129}) || done || res.Changed != 2 || len(res.Target) != 2 || res.Target[1].Rev != 7 {
		t.Fatalf("result = %+v next = %+v done = %v", res, next, done)
	}

	if _, _, _, err := st.PinZonePlacementBatch(routeSQLTestContext(), 101, routeScanCursor{}, routePinZoneBatchLimit+1); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("over-limit batch error = %v", err)
	}

	st, f = newFakeRouteSQLStore(t, routeScript{}.on("FENCE_EXISTS", rsOne()).on("WRITABLE", rsOne()).on("LOCK_FENCE", rsFenceHeld("merge-run")))
	if _, _, _, err := st.PinZonePlacementBatch(routeSQLTestContext(), 101, routeScanCursor{}, 2); !errors.Is(err, errRouteZoneFenced) {
		t.Fatalf("fenced zone error = %v", err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "ROLLBACK")
}

func TestBackfillHomeBatch_InsertFillAlreadyConflict(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("FENCE_EXISTS", rsOne()).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE", rsFenceFree()).
		on("LOCK_MERGED", rsNone()).
		on("LOCK_ROWS", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 1, StorageID: 5, PlacementVersion: 2, Rev: 2}),
			rsRouteRow(mergeRouteRow{PlayerID: 2, HomeZone: 101, Rev: 1}),
			rsRouteRow(mergeRouteRow{PlayerID: 3, HomeZone: 999, Rev: 1}))).
		on("FILL_HOME", rsAffected(1)).
		on("INSERT_HOME", rsAffected(1)))
	res, err := st.BackfillHomeBatch(routeSQLTestContext(), 101, []uint64{3, 2, 1, 64})
	if err != nil {
		t.Fatalf("BackfillHomeBatch: %v", err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_MERGED", "LOCK_ROWS", "FILL_HOME", "INSERT_HOME", "COMMIT")
	assertRouteArgs(t, f.callsOf("FILL_HOME")[0], uint64(101), testRouteWriter, uint64(1), uint64(1), uint64(2))
	ins := f.callsOf("INSERT_HOME")[0]
	if ins.query != routeInsertHomeSQL(1) {
		t.Fatalf("insert query = %q", ins.query)
	}
	assertRouteArgs(t, ins, uint64(0), uint64(64), uint64(101), testRouteWriter)
	want := map[uint64]routeBackfillVerdict{64: routeBackfillInserted, 1: routeBackfillFilled, 2: routeBackfillAlready, 3: routeBackfillConflict}
	if !reflect.DeepEqual(res.Verdicts, want) || res.Changed != 2 {
		t.Fatalf("verdicts = %v changed = %d", res.Verdicts, res.Changed)
	}
	wantTarget := []mergeRouteRow{
		{PlayerID: 1, HomeZone: 101, StorageID: 5, PlacementVersion: 2, Rev: 3},
		{PlayerID: 2, HomeZone: 101, Rev: 1},
		{PlayerID: 64, HomeZone: 101, Rev: 1},
	}
	if !reflect.DeepEqual(res.Target, wantTarget) {
		t.Fatalf("target = %+v", res.Target)
	}
}

func TestBackfillHomeBatch_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script routeScript
		want   error
	}{
		{"合服围栏生效", routeScript{}.on("FENCE_EXISTS", rsOne()).on("WRITABLE", rsOne()).on("LOCK_FENCE", rsFenceHeld("merge-run")), errRouteZoneFenced},
		{"围栏run非空而过期时刻为NULL", routeScript{}.on("FENCE_EXISTS", rsOne()).on("WRITABLE", rsOne()).
			on("LOCK_FENCE", rsRows([]driver.Value{"merge-run", nil})), errRouteZoneFenced},
		{"已被合走", routeScript{}.on("FENCE_EXISTS", rsOne()).on("WRITABLE", rsOne()).on("LOCK_FENCE", rsFenceFree()).
			on("LOCK_MERGED", rsRows([]driver.Value{int64(102), "run-a"})), errRouteZoneMergedAway},
		{"围栏片行读不到", routeScript{}.on("FENCE_EXISTS", rsOne()).on("WRITABLE", rsOne()).on("LOCK_FENCE", rsNone()), errRouteFenceRowMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f := newFakeRouteSQLStore(t, tc.script)
			if _, err := st.BackfillHomeBatch(routeSQLTestContext(), 101, []uint64{1}); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got := f.labels(); got[len(got)-1] != "ROLLBACK" || len(f.callsOf("LOCK_ROWS")) != 0 {
				t.Fatalf("a refusal must roll back before touching player_route: %v", got)
			}
		})
	}
}

// TestBackfillHomeBatch_RacedInsertRetriesWholeTx:批量插入少插了行(加锁读之后有 W1 并发插入同一个 id)→ 整批回滚、不退避直接重来。
func TestBackfillHomeBatch_RacedInsertRetriesWholeTx(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("FENCE_EXISTS", rsOne()).
		on("WRITABLE", rsOne(), rsOne()).
		on("LOCK_FENCE", rsFenceFree(), rsFenceFree()).
		on("LOCK_MERGED", rsNone(), rsNone()).
		on("LOCK_ROWS", rsNone(), rsRows(rsRouteRow(mergeRouteRow{PlayerID: 64, HomeZone: 101, Rev: 1}))).
		on("INSERT_HOME", rsAffected(0)))
	res, err := st.BackfillHomeBatch(routeSQLTestContext(), 101, []uint64{64})
	if err != nil || res.Verdicts[64] != routeBackfillAlready {
		t.Fatalf("BackfillHomeBatch = (%+v, %v)", res, err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_MERGED", "LOCK_ROWS", "INSERT_HOME", "ROLLBACK",
		"BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_MERGED", "LOCK_ROWS", "COMMIT")
}

func TestMarkMergedInto(t *testing.T) {
	const mark = "101-102-20261018T000000Z-cafebabe"
	held := func() routeScript {
		return routeScript{}.on("FENCE_EXISTS", rsOne(), rsOne()).on("WRITABLE", rsOne()).
			on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun))
	}
	wantValue := `{"dst":102,"run_id":"` + mark + `"}`

	st, f := newFakeRouteSQLStore(t, held().on("LOCK_MERGED", rsNone()).on("INSERT_MERGED", rsAffected(1)))
	value, err := st.MarkMergedInto(routeSQLTestContext(), 101, 102, testRouteRun, mark)
	if err != nil || value != wantValue {
		t.Fatalf("insert = (%q, %v), want %q", value, err, wantValue)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "LOCK_MERGED", "INSERT_MERGED", "COMMIT")
	assertRouteArgs(t, f.callsOf("INSERT_MERGED")[0], uint64(101), uint64(102), mark)

	st, f = newFakeRouteSQLStore(t, held().on("LOCK_MERGED", rsRows([]driver.Value{int64(102), "older-run"})).on("UPDATE_MERGED", rsAffected(1)))
	if _, err := st.MarkMergedInto(routeSQLTestContext(), 101, 102, testRouteRun, mark); err != nil {
		t.Fatalf("same dst: %v", err)
	}
	assertRouteArgs(t, f.callsOf("UPDATE_MERGED")[0], mark, uint64(101), uint64(102))

	st, f = newFakeRouteSQLStore(t, held().on("LOCK_MERGED", rsRows([]driver.Value{int64(102), mark})))
	if _, err := st.MarkMergedInto(routeSQLTestContext(), 101, 102, testRouteRun, mark); err != nil {
		t.Fatalf("same dst and run: %v", err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "LOCK_MERGED", "COMMIT")

	st, f = newFakeRouteSQLStore(t, held().on("LOCK_MERGED", rsRows([]driver.Value{int64(103), "x-run"})))
	if _, err := st.MarkMergedInto(routeSQLTestContext(), 101, 102, testRouteRun, mark); !errors.Is(err, errRouteMergedIntoConflict) {
		t.Fatalf("other dst error = %v", err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "LOCK_MERGED", "ROLLBACK")
}

func TestClearMergedInto_DeletesOnlyTheMatchingMarker(t *testing.T) {
	const mark = "101-102-20261018T000000Z-cafebabe"
	st, f := newFakeRouteSQLStore(t, routeScript{}.on("FENCE_EXISTS", rsOne(), rsOne()).on("WRITABLE", rsOne()).
		on("LOCK_FENCE", rsFenceHeld(testRouteRun), rsFenceHeld(testRouteRun)).on("DELETE_MERGED", rsAffected(0)))
	deleted, err := st.ClearMergedInto(routeSQLTestContext(), 101, 102, testRouteRun, mark)
	if err != nil || deleted {
		t.Fatalf("ClearMergedInto = (%v, %v), want (false, nil)", deleted, err)
	}
	assertRouteArgs(t, f.callsOf("DELETE_MERGED")[0], uint64(101), uint64(102), mark)
}

func TestRelocateCAS_OutcomesMirrorTheScript(t *testing.T) {
	ops := []placementCAS{
		{PlayerID: 64, ExpectAbsent: true, ExpectHome: "101", FenceZone: 101, NewValue: "101:1:frozen:run1"},
		{PlayerID: 65, ExpectRecord: "5:3", ExpectHome: "101", FenceZone: 101, NewValue: "5:3:frozen:run1"},
		{PlayerID: 66, ExpectRecord: "5:3", ExpectHome: "101", FenceZone: 101, NewValue: "5:3:frozen:run1"},
		{PlayerID: 67, ExpectAbsent: true, ExpectHome: "102", FenceZone: 102, NewValue: "102:1:frozen:run1"},
		{PlayerID: 68, ExpectRecord: "5:3:frozen:run1", ExpectHome: "101", NewValue: "6:4"},
	}
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("FENCE_EXISTS", rsOne(), rsOne()).
		on("WRITABLE", rsOne()).
		on("LOCK_FENCE", rsFenceFree(), rsFenceHeld("merge-run")).
		on("LOCK_ROWS", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 64, HomeZone: 101, Rev: 1}),
			rsRouteRow(mergeRouteRow{PlayerID: 65, HomeZone: 101, StorageID: 5, PlacementVersion: 4, Rev: 2}),
			rsRouteRow(mergeRouteRow{PlayerID: 66, HomeZone: 102, StorageID: 5, PlacementVersion: 3, Rev: 2}),
			rsRouteRow(mergeRouteRow{PlayerID: 67, HomeZone: 102, Rev: 1}),
			rsRouteRow(mergeRouteRow{PlayerID: 68, HomeZone: 101, StorageID: 5, PlacementVersion: 3, FrozenRunID: "run1", Rev: 7}))).
		on("SET_PLACEMENT", rsAffected(1), rsAffected(1)))
	res, err := st.RelocateCAS(routeSQLTestContext(), ops)
	if err != nil {
		t.Fatalf("RelocateCAS: %v", err)
	}
	assertRouteLabels(t, f, "FENCE_EXISTS", "FENCE_EXISTS", "BEGIN", "WRITABLE", "LOCK_FENCE", "LOCK_FENCE", "LOCK_ROWS", "SET_PLACEMENT", "SET_PLACEMENT", "COMMIT")
	set := f.callsOf("SET_PLACEMENT")
	assertRouteArgs(t, set[0], uint64(101), uint64(1), "run1", testRouteWriter, uint64(0), uint64(64), uint64(1))
	assertRouteArgs(t, set[1], uint64(6), uint64(4), nil, testRouteWriter, uint64(4), uint64(68), uint64(7))
	want := []placementCASOutcome{placementCASWritten, placementCASRecordChanged, placementCASHomeChanged, placementCASFenced, placementCASWritten}
	for i, w := range want {
		if res[i].Outcome != w {
			t.Fatalf("op %d (player %d) outcome = %d, want %d", i, ops[i].PlayerID, res[i].Outcome, w)
		}
	}
	if res[1].CurrentRecord != "5:4" || res[2].CurrentHome != "102" {
		t.Fatalf("current values must be the DB encoding: %+v / %+v", res[1], res[2])
	}
	if got := res[4].Row; got.StorageID != 6 || got.PlacementVersion != 4 || got.FrozenRunID != "" || got.Rev != 8 {
		t.Fatalf("switched row = %+v", got)
	}
}

// TestRelocateCAS_P2AssertionRollsBack:稳定态的新 version 小于当前 version(P-2)→ 整批回滚、返回错误,一条都不写。
func TestRelocateCAS_P2AssertionRollsBack(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("WRITABLE", rsOne()).
		on("LOCK_ROWS", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 68, HomeZone: 101, StorageID: 5, PlacementVersion: 3, FrozenRunID: "run1", Rev: 7}))))
	_, err := st.RelocateCAS(routeSQLTestContext(), []placementCAS{{PlayerID: 68, ExpectRecord: "5:3:frozen:run1", ExpectHome: "101", NewValue: "5:2"}})
	if !errors.Is(err, errRouteInvariant) {
		t.Fatalf("error = %v, want errRouteInvariant", err)
	}
	assertRouteLabels(t, f, "BEGIN", "WRITABLE", "LOCK_ROWS", "ROLLBACK")

	if _, err := st.RelocateCAS(routeSQLTestContext(), []placementCAS{
		{PlayerID: 9, ExpectAbsent: true, NewValue: "5:1"}, {PlayerID: 9, ExpectAbsent: true, NewValue: "5:1"},
	}); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("duplicate players error = %v", err)
	}
}

func TestScanByHome_ValidatesThePage(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("SCAN_HOME", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 65, HomeZone: 101, Rev: 1}), rsRouteRow(mergeRouteRow{PlayerID: 2, HomeZone: 101, Rev: 1})),
			rsRows(rsRouteRow(mergeRouteRow{PlayerID: 65, HomeZone: 101, Rev: 1}), rsRouteRow(mergeRouteRow{PlayerID: 129, HomeZone: 101, Rev: 1})),
			rsRows(rsRouteRow(mergeRouteRow{PlayerID: 66, HomeZone: 999, Rev: 1}))))
	after := routeScanCursor{Shard: 1, PlayerID: 1}
	rows, err := st.ScanByHome(routeSQLTestContext(), 101, after, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ScanByHome = (%v, %v)", rows, err)
	}
	assertRouteArgs(t, f.callsOf("SCAN_HOME")[0], uint64(101), uint64(1), uint64(1), uint64(1), int64(10))
	// 第二页的游标就是 (1, 65),库却又回了 (1, 65):键没有严格大于游标,整页作废。
	if _, err := st.ScanByHome(routeSQLTestContext(), 101, routeScanCursor{Shard: 1, PlayerID: 65}, 10); !errors.Is(err, errRouteInvariant) {
		t.Fatalf("a key not after the cursor must void the page: %v", err)
	}
	if _, err := st.ScanByHome(routeSQLTestContext(), 101, routeScanCursor{}, 10); !errors.Is(err, errRouteInvariant) {
		t.Fatalf("a row of another zone must void the page: %v", err)
	}
	if _, err := st.ScanByHome(routeSQLTestContext(), 0, routeScanCursor{}, 10); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("zone 0 error = %v", err)
	}
}

// ── 成对核对的入口 ────────────────────────────────────────────────────────────

type fakeWatermarkReader struct {
	raw     string
	present bool
	err     error
	calls   int
}

func (r *fakeWatermarkReader) readWatermark(context.Context) (string, bool, error) {
	r.calls++
	return r.raw, r.present, r.err
}

func TestOpenRouteBackend(t *testing.T) {
	ctx := routeSQLTestContext()
	st, err := openRouteBackend(ctx, routeSourceRedis, "", testRouteWriter, &fakeWatermarkReader{})
	if err != nil || st != nil {
		t.Fatalf("redis mode without a watermark = (%v, %v)", st, err)
	}
	if _, err := openRouteBackend(ctx, routeSourceRedis, "", testRouteWriter, &fakeWatermarkReader{raw: "{}", present: true}); !errors.Is(err, errRoutePairing) {
		t.Fatalf("redis mode with a watermark error = %v, want errRoutePairing", err)
	}
	if _, err := openRouteBackend(ctx, routeSourceRedis, "", testRouteWriter, &fakeWatermarkReader{err: errors.New("WRONGTYPE")}); err == nil {
		t.Fatal("a watermark read failure must refuse (fail-closed)")
	}
	reader := &fakeWatermarkReader{}
	if _, err := openRouteBackend(ctx, routeSourceDB, "not a dsn", testRouteWriter, reader); err == nil || reader.calls != 0 {
		t.Fatalf("db mode with a bad DSN = %v (watermark reads %d), want a refusal before touching Redis", err, reader.calls)
	}
	if _, err := openRouteBackend(ctx, "", "", testRouteWriter, reader); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("empty source error = %v", err)
	}
}
