package main

// route_store.go 的单测:AST 守卫、DSN、探针判据、参数校验,以及写事务的语句序列(假 database/sql 驱动,仿
// go/data_service/internal/store/route_store_contract_test.go;本 module 没有 sqlmock,按约定不为测试引新依赖)。
//
// 假驱动**不模拟 TiDB 的锁、唯一约束与 CHECK**:并发下谁赢、会不会 1213,只有真库能证(route_store_integration_test.go)。
// 这里只证「给定库的应答,本层的判定、语句、实参与提交 / 回滚不变」。

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

// routeSQLTestContext 是写事务要的上下文:本机 TOOL owner + 一份冻结证明(实际判定由假驱动的 NORMAL_FREEZE 应答决定)。
// 不造 P0 / 对外原件。
func routeSQLTestContext(t *testing.T) context.Context {
	t.Helper()
	proof, err := parseNormalFreezeProof(normalFreezeOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := testMaintenanceContext()
	return withNormalFreezeProof(ctx, proof)
}

const (
	testRouteGen = "0123456789abcdef0123456789abcdef"
	testRouteRun = "101-102-20261018T000000Z-cafebabe"
	// testFreezeLockSQL 是 normalFreezeOptions 那份证明的 lockSQL(库名 fixture)。
	testFreezeLockSQL = "SELECT writable FROM `fixture`.`route_meta` WHERE id = 1 FOR UPDATE"
)

// rsFrozen 给脚本补 n 次「锁 meta + 强读冻结收据」的成功应答(每个写事务开头一次)。
func rsFrozen(s routeScript, n int) routeScript {
	for i := 0; i < n; i++ {
		s = s.on("LOCK_META", rsRows([]driver.Value{int64(0)})).on("NORMAL_FREEZE", rsOne())
	}
	return s
}

// ── AST 守卫 ──────────────────────────────────────────────────────────────────

// TestNoRouteTableWritesOutsideMergeRouteStore:merge_zone 只写 zone_merged_into 一张路由表,且写语句只许出现在 route_store.go
// (仿 data_service 的 TestNoPlayerRouteWritesOutsideRouteStore)。player_route / route_meta / route_reconcile 本工具一律不写:
// 别名合服不动玩家,冻结 / 解冻 / 对账只属于 data_service。
//
// 只检查字符串字面量(先 unquote),不看注释;跨字面量拼出来的语句("UPDATE " + table)查不到;_test.go 不检查。
func TestNoRouteTableWritesOutsideMergeRouteStore(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve tools/merge_zone: %v", err)
	}
	const verbs = "(?i)(INSERT\\s+INTO|UPDATE|DELETE\\s+FROM|REPLACE\\s+INTO|TRUNCATE\\s+TABLE)\\s+`?"
	routeWrite := regexp.MustCompile(verbs + "zone_merged_into\\b")
	neverWrite := regexp.MustCompile(verbs + "(player_route|zone_merge_fence|route_meta|route_reconcile)\\b")
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
			if neverWrite.MatchString(s) {
				offenders = append(offenders, rel+" (never written by merge_zone): "+s)
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
	// insert / update / delete 共 3 条。
	if allowedHits < 3 {
		t.Fatalf("%s 里只匹配到 %d 条 zone_merged_into 写语句(至少应有 3 条):正则失效,本守卫已经拦不住任何东西", allowed, allowedHits)
	}
	if len(offenders) > 0 {
		t.Fatalf("路由表的写入只许是 %s 里的 zone_merged_into,以下位置绕过了它:\n%s", allowed, strings.Join(offenders, "\n"))
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
	st := &mergeRouteStore{db: db, sleep: f.sleep, backoff: func() time.Duration { return time.Millisecond }}
	return st, f
}

// routeSQLLabel 把 SQL 文本映射成标签。只认逐字相同的生产文本;认不出来就是空串,假驱动对它报错。
func routeSQLLabel(q string) string {
	switch q {
	case routeProbeSessionSQL:
		return "SESSION"
	case currentTiDBVersionSQL:
		return "VERSION"
	case currentTiDBSessionSQL:
		return "TXN_MODE"
	case "SELECT VERSION(),@@version_comment,@@read_only,@@super_read_only,@@transaction_isolation":
		return "MYSQL_SESSION" // authoritysql 的 mysql 档读回,文本属于那个包
	case routeReadMetaSQL:
		return "META"
	case testFreezeLockSQL:
		return "LOCK_META"
	case normalFrozenProofSQLForSchema("fixture"):
		return "NORMAL_FREEZE"
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
	if got := f.labels(); (len(got) != 0 || len(want) != 0) && !reflect.DeepEqual(got, want) {
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
	return c.BeginTx(context.Background(), driver.TxOptions{})
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

// rsRouteRow 是按 home_zone 翻页读到的一行(列序同 routeScanByHomeSQL:route_shard、player_id、home_zone)。
func rsRouteRow(r mergeRouteRow) []driver.Value {
	return []driver.Value{int64(routeRowShard(r.PlayerID)), int64(r.PlayerID), int64(r.HomeZone)}
}

func rsOne() fakeRouteSQLReply { return rsRows([]driver.Value{int64(1)}) }

func rsNone() fakeRouteSQLReply { return fakeRouteSQLReply{} }

// ── DSN ───────────────────────────────────────────────────────────────────────

func TestRouteDBDSNPinsSessionVariables(t *testing.T) {
	out, err := routeDBDSN("appuser:s3cret@tcp(127.0.0.1:3306)/testdb?charset=utf8mb4&tx_isolation=REPEATABLE-READ"+
		"&transaction_isolation=REPEATABLE-READ&innodb_lock_wait_timeout=50&parseTime=true", authoritysql.BackendTiDB)
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
			_, err := routeDBDSN(tc.dsn, authoritysql.BackendTiDB)
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

// routeProbeHappy 是一次通过的TiDB探针应答脚本，仅验证SQL状态机，不充当引擎或锁的真实验收。
func routeProbeHappy() routeScript {
	return routeScript{}.
		on("SESSION", rsRows([]driver.Value{"READ-COMMITTED", int64(1)})).
		on("VERSION", rsRows([]driver.Value{"8.0.11-TiDB-v8.5.0", "TiDB Server"})).
		on("TXN_MODE", rsRows([]driver.Value{"pessimistic", "READ-COMMITTED", "leader", "strict", "", int64(0), int64(0)})).
		on("META", rsRows([]driver.Value{testRouteGen, int64(1)}))
}

// TestRouteStoreProbe_OnlyReads:探针不发任何 DML(本工具不写 player_route,不再需要回滚事务里的 CHECK 探针)。
func TestRouteStoreProbe_OnlyReads(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeProbeHappy())
	meta, err := st.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.Generation != testRouteGen || meta.Writable != 1 {
		t.Fatalf("meta = %+v", meta)
	}
	assertRouteLabels(t, f, "SESSION", "VERSION", "TXN_MODE", "META")
}

func TestRouteStoreProbe_Refusals(t *testing.T) {
	session := func(iso string, wait int64) routeScript {
		return routeScript{}.on("SESSION", rsRows([]driver.Value{iso, wait}))
	}
	engineOK := func() routeScript {
		return routeScript{}.on("SESSION", rsRows([]driver.Value{"READ-COMMITTED", int64(1)})).
			on("VERSION", rsRows([]driver.Value{"8.0.11-TiDB-v8.5.0", "TiDB Server"}))
	}
	for _, tc := range []struct {
		name   string
		script routeScript
		want   error
	}{
		{"隔离级别不是RC", session("REPEATABLE-READ", 1), errRouteSessionMisconfigured},
		{"锁等待不是1s", session("READ-COMMITTED", 50), errRouteSessionMisconfigured},
		{"乐观事务", engineOK().on("TXN_MODE", rsRows([]driver.Value{"optimistic", "READ-COMMITTED", "leader", "strict", "", int64(0), int64(0)})), errCurrentTiDB},
		{"历史MySQL拒绝", routeScript{}.on("SESSION", rsRows([]driver.Value{"READ-COMMITTED", int64(1)})).on("VERSION", rsRows([]driver.Value{"8.0.36", "MySQL Community Server"})), errCurrentTiDB},
		{"route_meta缺行", engineOK().on("TXN_MODE", rsRows([]driver.Value{"pessimistic", "READ-COMMITTED", "leader", "strict", "", int64(0), int64(0)})).on("META", rsNone()), errRouteMetaMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newFakeRouteSQLStore(t, tc.script)
			if _, err := st.probe(context.Background()); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// ── 写事务 ────────────────────────────────────────────────────────────────────

// TestRouteTxNeedsToolAndFreezeProof:没有 TOOL 身份、或有身份没有冻结证明,一条语句都不发。
func TestRouteTxNeedsToolAndFreezeProof(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{})
	body := func(*sql.Tx) error { return errors.New("body must not run") }
	if err := st.runTx(context.Background(), "x", body); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("no tool owner error = %v", err)
	}
	ctx, _ := testMaintenanceContext()
	if err := st.runTx(ctx, "x", body); !errors.Is(err, errNormalFreezeProof) {
		t.Fatalf("no freeze proof error = %v", err)
	}
	assertRouteLabels(t, f)
}

// TestRouteTxRetriesDeadlockNotLockWait:1213 整事务重跑(退避后新开事务),1205 不重试,4 次 1213 后放弃且错误仍带 MySQLError。
func TestRouteTxRetriesDeadlockNotLockWait(t *testing.T) {
	script := rsFrozen(routeScript{}, 2).on("DELETE_MERGED", rsMySQLErr(mysqlErrDeadlock, "Deadlock found"), rsAffected(1))
	st, f := newFakeRouteSQLStore(t, script)
	deleted, err := st.ClearMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun)
	if err != nil || !deleted {
		t.Fatalf("ClearMergedInto after one deadlock = (%v, %v)", deleted, err)
	}
	assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "DELETE_MERGED", "ROLLBACK",
		"SLEEP", "BEGIN", "LOCK_META", "NORMAL_FREEZE", "DELETE_MERGED", "COMMIT")

	st, f = newFakeRouteSQLStore(t, rsFrozen(routeScript{}, 1).on("DELETE_MERGED", rsMySQLErr(mysqlErrLockWaitTimeout, "Lock wait timeout exceeded")))
	_, err = st.ClearMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun)
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlErrLockWaitTimeout {
		t.Fatalf("1205 error = %v, want the MySQLError unretried", err)
	}
	assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "DELETE_MERGED", "ROLLBACK")

	script = rsFrozen(routeScript{}, routeTxAttempts)
	for i := 0; i < routeTxAttempts; i++ {
		script = script.on("DELETE_MERGED", rsMySQLErr(mysqlErrDeadlock, "Deadlock found"))
	}
	st, _ = newFakeRouteSQLStore(t, script)
	_, err = st.ClearMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun)
	if !errors.As(err, &me) || me.Number != mysqlErrDeadlock || !strings.Contains(err.Error(), "gave up after 4 attempts") {
		t.Fatalf("exhausted retries error = %v", err)
	}
}

// ── zone_merged_into ──────────────────────────────────────────────────────────

func TestMarkMergedInto(t *testing.T) {
	wantValue := `{"dst":102,"run_id":"` + testRouteRun + `"}`
	frozen := func() routeScript { return rsFrozen(routeScript{}, 1) }

	// 首写:两行都没有 → 插入。锁按 zone 升序。
	st, f := newFakeRouteSQLStore(t, frozen().on("LOCK_MERGED", rsNone(), rsNone()).on("INSERT_MERGED", rsAffected(1)))
	value, err := st.MarkMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun)
	if err != nil || value != wantValue {
		t.Fatalf("insert = (%q, %v), want %q", value, err, wantValue)
	}
	assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "LOCK_MERGED", "LOCK_MERGED", "INSERT_MERGED", "COMMIT")
	assertRouteArgs(t, f.callsOf("LOCK_MERGED")[0], uint64(101))
	assertRouteArgs(t, f.callsOf("LOCK_MERGED")[1], uint64(102))
	assertRouteArgs(t, f.callsOf("INSERT_MERGED")[0], uint64(101), uint64(102), testRouteRun)

	// 同 dst 不同 run:续跑换清单,改 run_id。
	st, f = newFakeRouteSQLStore(t, frozen().on("LOCK_MERGED", rsRows([]driver.Value{int64(102), "older-run"}), rsNone()).on("UPDATE_MERGED", rsAffected(1)))
	if _, err := st.MarkMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun); err != nil {
		t.Fatalf("same dst: %v", err)
	}
	assertRouteArgs(t, f.callsOf("UPDATE_MERGED")[0], testRouteRun, uint64(101), uint64(102))

	// 同 dst 同 run:幂等,不写。
	st, f = newFakeRouteSQLStore(t, frozen().on("LOCK_MERGED", rsRows([]driver.Value{int64(102), testRouteRun}), rsNone()))
	if _, err := st.MarkMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun); err != nil {
		t.Fatalf("same dst and run: %v", err)
	}
	assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "LOCK_MERGED", "LOCK_MERGED", "COMMIT")

	// 异 dst:拒绝,回滚。
	st, f = newFakeRouteSQLStore(t, frozen().on("LOCK_MERGED", rsRows([]driver.Value{int64(103), "x-run"}), rsNone()))
	if _, err := st.MarkMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun); !errors.Is(err, errRouteMergedIntoConflict) {
		t.Fatalf("other dst error = %v", err)
	}
	assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "LOCK_MERGED", "LOCK_MERGED", "ROLLBACK")

	// 插入撞并发写者:整事务重来,这次读到同 dst 同 run。
	st, f = newFakeRouteSQLStore(t, rsFrozen(routeScript{}, 2).
		on("LOCK_MERGED", rsNone(), rsNone(), rsRows([]driver.Value{int64(102), testRouteRun}), rsNone()).on("INSERT_MERGED", rsAffected(0)))
	if _, err := st.MarkMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun); err != nil {
		t.Fatalf("raced insert: %v", err)
	}
	assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "LOCK_MERGED", "LOCK_MERGED", "INSERT_MERGED", "ROLLBACK",
		"BEGIN", "LOCK_META", "NORMAL_FREEZE", "LOCK_MERGED", "LOCK_MERGED", "COMMIT")
}

// TestMarkMergedInto_TargetMergedAwayIsRefusedInTheTx:连续合服的拒绝规则在提交点再核一次(事务外的检查只是先证明)。
// 锁按 zone 升序,与 src / dst 谁大无关。
func TestMarkMergedInto_TargetMergedAwayIsRefusedInTheTx(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, rsFrozen(routeScript{}, 1).on("LOCK_MERGED", rsNone(), rsRows([]driver.Value{int64(103), "x-run"})))
	if _, err := st.MarkMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun); !errors.Is(err, errRouteMergedIntoConflict) {
		t.Fatalf("target merged away error = %v", err)
	}
	assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "LOCK_MERGED", "LOCK_MERGED", "ROLLBACK")

	st, f = newFakeRouteSQLStore(t, rsFrozen(routeScript{}, 1).on("LOCK_MERGED", rsRows([]driver.Value{int64(103), "x-run"})))
	if _, err := st.MarkMergedInto(routeSQLTestContext(t), 102, 101, testRouteRun); !errors.Is(err, errRouteMergedIntoConflict) {
		t.Fatalf("lower-numbered target merged away error = %v", err)
	}
	assertRouteArgs(t, f.callsOf("LOCK_MERGED")[0], uint64(101))
}

func TestMarkMergedInto_ArgumentsAreCheckedFirst(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{})
	ctx := routeSQLTestContext(t)
	for _, tc := range []struct {
		src, dst uint32
		run      string
	}{{101, 101, testRouteRun}, {0, 102, testRouteRun}, {101, routeHomeZoneMax + 1, testRouteRun}, {101, 102, ""}, {101, 102, "bad run"}} {
		if _, err := st.MarkMergedInto(ctx, tc.src, tc.dst, tc.run); !errors.Is(err, errRouteInvalidArgument) {
			t.Fatalf("MarkMergedInto(%d, %d, %q) error = %v", tc.src, tc.dst, tc.run, err)
		}
		if _, err := st.ClearMergedInto(ctx, tc.src, tc.dst, tc.run); !errors.Is(err, errRouteInvalidArgument) {
			t.Fatalf("ClearMergedInto(%d, %d, %q) error = %v", tc.src, tc.dst, tc.run, err)
		}
	}
	assertRouteLabels(t, f)
}

func TestClearMergedInto_DeletesOnlyTheMatchingRow(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, rsFrozen(routeScript{}, 1).on("DELETE_MERGED", rsAffected(0)))
	deleted, err := st.ClearMergedInto(routeSQLTestContext(t), 101, 102, testRouteRun)
	if err != nil || deleted {
		t.Fatalf("ClearMergedInto = (%v, %v), want (false, nil)", deleted, err)
	}
	assertRouteArgs(t, f.callsOf("DELETE_MERGED")[0], uint64(101), uint64(102), testRouteRun)
}

func TestReadMergedInto(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.on("READ_MERGED", rsRows([]driver.Value{int64(102), testRouteRun}), rsNone()))
	dst, run, found, err := st.ReadMergedInto(context.Background(), 101)
	if err != nil || !found || dst != 102 || run != testRouteRun {
		t.Fatalf("ReadMergedInto = (%d, %q, %v, %v)", dst, run, found, err)
	}
	if _, _, found, err := st.ReadMergedInto(context.Background(), 101); err != nil || found {
		t.Fatalf("no row = (%v, %v), want (false, nil)", found, err)
	}
	if _, _, _, err := st.ReadMergedInto(context.Background(), 0); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("zone 0 error = %v", err)
	}
	assertRouteArgs(t, f.callsOf("READ_MERGED")[0], uint64(101))
}

// ── player_route 只读 ─────────────────────────────────────────────────────────

func TestScanByHome_ValidatesThePage(t *testing.T) {
	st, f := newFakeRouteSQLStore(t, routeScript{}.
		on("SCAN_HOME", rsRows(rsRouteRow(mergeRouteRow{PlayerID: 65, HomeZone: 101}), rsRouteRow(mergeRouteRow{PlayerID: 2, HomeZone: 101})),
			rsRows(rsRouteRow(mergeRouteRow{PlayerID: 65, HomeZone: 101}), rsRouteRow(mergeRouteRow{PlayerID: 129, HomeZone: 101})),
			rsRows(rsRouteRow(mergeRouteRow{PlayerID: 66, HomeZone: 999}))))
	after := routeScanCursor{Shard: 1, PlayerID: 1}
	ctx := context.Background()
	rows, err := st.ScanByHome(ctx, 101, after, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ScanByHome = (%v, %v)", rows, err)
	}
	assertRouteArgs(t, f.callsOf("SCAN_HOME")[0], uint64(101), uint64(1), uint64(1), uint64(1), int64(10))
	// 第二页的游标就是 (1, 65),库却又回了 (1, 65):键没有严格大于游标,整页作废。
	if _, err := st.ScanByHome(ctx, 101, routeScanCursor{Shard: 1, PlayerID: 65}, 10); !errors.Is(err, errRouteInvariant) {
		t.Fatalf("a key not after the cursor must void the page: %v", err)
	}
	if _, err := st.ScanByHome(ctx, 101, routeScanCursor{}, 10); !errors.Is(err, errRouteInvariant) {
		t.Fatalf("a row of another zone must void the page: %v", err)
	}
	if _, err := st.ScanByHome(ctx, 0, routeScanCursor{}, 10); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("zone 0 error = %v", err)
	}
}

// ── 开库入口 ──────────────────────────────────────────────────────────────────

type fakeWatermarkReader struct {
	calls int
}

func (r *fakeWatermarkReader) readWatermark(context.Context) (string, bool, error) {
	r.calls++
	return "", false, nil
}

func TestOpenPairedRouteStore_BadDSNIsRefusedBeforeRedis(t *testing.T) {
	reader := &fakeWatermarkReader{}
	if _, err := openPairedRouteStore(context.Background(), "not a dsn", authoritysql.BackendTiDB, reader); err == nil || reader.calls != 0 {
		t.Fatalf("a bad DSN = %v (watermark reads %d), want a refusal before touching Redis", err, reader.calls)
	}
}

func TestCheckRouteFlags(t *testing.T) {
	ok := options{routeDBDSN: "u@tcp(127.0.0.1:4000)/testdb", waitAOFLocal: 1, waitAOFTimeoutMs: 1500}
	if err := checkRouteFlags(ok); err != nil {
		t.Fatalf("complete flags: %v", err)
	}
	noDSN := ok
	noDSN.routeDBDSN = ""
	if err := checkRouteFlags(noDSN); err == nil || !strings.Contains(err.Error(), "-route-db-dsn") {
		t.Fatalf("missing DSN error = %v", err)
	}
	badAOF := ok
	badAOF.waitAOFLocal, badAOF.waitAOFReplicas = 0, 0
	if err := checkRouteFlags(badAOF); err == nil {
		t.Fatal("WAITAOF 0 0 must be refused")
	}
}

func TestCheckMode_OnlyTheThreeModes(t *testing.T) {
	for _, mode := range []string{"merge", "unmerge", "audit"} {
		if err := checkMode(options{mode: mode}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	for _, mode := range []string{"", "pin-placement", "relocate", "relocate-abort", "storage-audit", "capability-check"} {
		if err := checkMode(options{mode: mode}); err == nil {
			t.Fatalf("-mode %q must be refused", mode)
		}
	}
}
