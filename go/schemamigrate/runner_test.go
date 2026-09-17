package schemamigrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"schemamigrate/internal/dbtest"

	"google.golang.org/protobuf/proto"
)

const testDatabase = "mmorpg_trade"

type fakeColumn struct{ name, typ string }

type fakeTable struct {
	columns    []fakeColumn
	primaryKey []string
	indexes    []string
}

type ledgerRow struct {
	version  int64
	name     string
	checksum string
	dirty    bool
}

// fakeMySQL 是一个有状态的「迷你 MySQL」:只懂 schemamigrate 会发的那几类语句,足以把
// Plan / Up 的完整流程(库名断言、会话超时、锁、台账、DDL、information_schema)串起来跑。
// 不认识的语句直接报错,防止被测代码走了预期外的路径还显示通过。
type fakeMySQL struct {
	mu           sync.Mutex
	database     driver.Value // DATABASE() 的返回值;nil 表示连接未选库
	connID       int64
	lockBusy     bool
	ledgerExists bool
	ledger       []ledgerRow
	tables       map[string]*fakeTable
	kills        []string
	// intercept 在语句进入状态机之前调用(不持锁),可以阻塞或返回错误,用来模拟执行中途被取消。
	intercept func(ctx context.Context, query string) error
}

func newFakeMySQL(database string) *fakeMySQL {
	return &fakeMySQL{database: database, connID: 7, tables: map[string]*fakeTable{}}
}

var (
	fakeIdentRe = regexp.MustCompile("`([^`]+)`")
	fakeAlterRe = regexp.MustCompile("^ALTER TABLE `([^`]+)` (ADD|MODIFY) COLUMN `([^`]+)` (.+)$")
)

func oneRow(v driver.Value) *dbtest.Rows {
	return &dbtest.Rows{Columns: []string{"v"}, Values: [][]driver.Value{{v}}}
}

func (f *fakeMySQL) handle(ctx context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
	q := strings.TrimSpace(query)
	f.mu.Lock()
	intercept := f.intercept
	f.mu.Unlock()
	if intercept != nil {
		if err := intercept(ctx, q); err != nil {
			return nil, err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ledgerTable := quoteIdent(SchemaMigrationsTable)
	switch {
	case q == "SELECT DATABASE()":
		return oneRow(f.database), nil
	case q == "SELECT CONNECTION_ID()":
		return oneRow(f.connID), nil
	case strings.HasPrefix(q, "SET SESSION "):
		return nil, nil
	case strings.HasPrefix(q, "SELECT GET_LOCK("):
		if f.lockBusy {
			return oneRow(int64(0)), nil
		}
		return oneRow(int64(1)), nil
	case strings.HasPrefix(q, "SELECT RELEASE_LOCK("):
		return oneRow(int64(1)), nil
	case strings.HasPrefix(q, "KILL QUERY "):
		f.kills = append(f.kills, q)
		return nil, nil
	case strings.Contains(q, "information_schema.TABLES"):
		if f.ledgerExists {
			return oneRow(int64(1)), nil
		}
		return oneRow(int64(0)), nil
	case strings.Contains(q, "information_schema.COLUMNS"):
		return f.columnRowsLocked(), nil
	case strings.Contains(q, "information_schema.STATISTICS"):
		return f.statisticsRowsLocked(), nil
	case strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS "+ledgerTable):
		f.ledgerExists = true
		return nil, nil
	case strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS `"):
		f.createTableLocked(q)
		return nil, nil
	case strings.HasPrefix(q, "ALTER TABLE `"):
		return nil, f.alterTableLocked(q)
	case strings.HasPrefix(q, "INSERT INTO "+ledgerTable):
		return nil, f.claimLocked(args)
	case strings.HasPrefix(q, "UPDATE "+ledgerTable):
		return nil, f.finishLocked(args)
	case strings.Contains(q, "FROM "+ledgerTable):
		if !f.ledgerExists {
			return nil, errors.New("Error 1146: Table 'schema_migrations' doesn't exist")
		}
		return f.ledgerRowsLocked(), nil
	}
	return nil, fmt.Errorf("fakeMySQL: 未预期的语句 %q", q)
}

func (f *fakeMySQL) sortedTableNamesLocked() []string {
	names := make([]string, 0, len(f.tables))
	for name := range f.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (f *fakeMySQL) columnRowsLocked() *dbtest.Rows {
	rows := &dbtest.Rows{Columns: []string{"TABLE_NAME", "COLUMN_NAME", "COLUMN_TYPE"}}
	for _, name := range f.sortedTableNamesLocked() {
		for _, c := range f.tables[name].columns {
			rows.Values = append(rows.Values, []driver.Value{name, c.name, c.typ})
		}
	}
	return rows
}

func (f *fakeMySQL) statisticsRowsLocked() *dbtest.Rows {
	rows := &dbtest.Rows{Columns: []string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME"}}
	for _, name := range f.sortedTableNamesLocked() {
		tbl := f.tables[name]
		for _, pk := range tbl.primaryKey {
			rows.Values = append(rows.Values, []driver.Value{name, "PRIMARY", pk})
		}
		for _, idx := range tbl.indexes {
			rows.Values = append(rows.Values, []driver.Value{name, idx, "x"})
		}
	}
	return rows
}

// createTableLocked 解析 proto2mysql 形状的建表语句;表已存在时按 IF NOT EXISTS 什么都不做。
func (f *fakeMySQL) createTableLocked(ddl string) {
	lines := strings.Split(ddl, "\n")
	name := fakeIdentRe.FindStringSubmatch(lines[0])[1]
	if _, exists := f.tables[name]; exists {
		return
	}
	tbl := &fakeTable{}
	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "  PRIMARY KEY ("):
			for _, id := range fakeIdentRe.FindAllStringSubmatch(line, -1) {
				tbl.primaryKey = append(tbl.primaryKey, id[1])
			}
		case strings.HasPrefix(line, "  INDEX `"), strings.HasPrefix(line, "  UNIQUE KEY `"):
			tbl.indexes = append(tbl.indexes, fakeIdentRe.FindStringSubmatch(line)[1])
		case strings.HasPrefix(line, "  `"):
			id := fakeIdentRe.FindStringSubmatch(line)[1]
			def := strings.TrimPrefix(line, "  `"+id+"` ")
			tbl.columns = append(tbl.columns, fakeColumn{name: id, typ: liveColumnType(def)})
		}
	}
	f.tables[name] = tbl
}

func (f *fakeMySQL) alterTableLocked(q string) error {
	m := fakeAlterRe.FindStringSubmatch(q)
	if m == nil {
		return fmt.Errorf("fakeMySQL: 看不懂的 ALTER %q", q)
	}
	tbl, ok := f.tables[m[1]]
	if !ok {
		return fmt.Errorf("Error 1146: Table '%s' doesn't exist", m[1])
	}
	for i, c := range tbl.columns {
		if c.name != m[3] {
			continue
		}
		if m[2] == "ADD" {
			return fmt.Errorf("Error 1060: Duplicate column name '%s'", m[3])
		}
		tbl.columns[i].typ = liveColumnType(m[4])
		return nil
	}
	if m[2] == "MODIFY" {
		return fmt.Errorf("Error 1054: Unknown column '%s'", m[3])
	}
	tbl.columns = append(tbl.columns, fakeColumn{name: m[3], typ: liveColumnType(m[4])})
	return nil
}

func (f *fakeMySQL) claimLocked(args []driver.NamedValue) error {
	if !f.ledgerExists {
		return errors.New("Error 1146: Table 'schema_migrations' doesn't exist")
	}
	if len(args) != 3 {
		return fmt.Errorf("fakeMySQL: 台账 INSERT 参数个数 = %d", len(args))
	}
	version, ok := args[0].Value.(int64)
	if !ok {
		return fmt.Errorf("fakeMySQL: 台账 version 参数类型 %T", args[0].Value)
	}
	for _, r := range f.ledger {
		if r.version == version {
			return fmt.Errorf("Error 1062: Duplicate entry '%d' for key 'PRIMARY'", version)
		}
	}
	f.ledger = append(f.ledger, ledgerRow{
		version:  version,
		name:     fmt.Sprint(args[1].Value),
		checksum: fmt.Sprint(args[2].Value),
		dirty:    true,
	})
	return nil
}

func (f *fakeMySQL) finishLocked(args []driver.NamedValue) error {
	if len(args) != 1 {
		return fmt.Errorf("fakeMySQL: 台账 UPDATE 参数个数 = %d", len(args))
	}
	version, _ := args[0].Value.(int64)
	for i := range f.ledger {
		if f.ledger[i].version == version {
			f.ledger[i].dirty = false
			return nil
		}
	}
	return fmt.Errorf("fakeMySQL: 台账没有 version=%d", version)
}

func (f *fakeMySQL) ledgerRowsLocked() *dbtest.Rows {
	rows := &dbtest.Rows{Columns: []string{"version", "name", "checksum", "dirty", "started_at", "finished_at"}}
	sorted := append([]ledgerRow(nil), f.ledger...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].version < sorted[j].version })
	for _, r := range sorted {
		var dirty int64
		var finished driver.Value = "2026-09-15 12:00:01"
		if r.dirty {
			dirty, finished = 1, nil
		}
		rows.Values = append(rows.Values, []driver.Value{r.version, r.name, r.checksum, dirty, "2026-09-15 12:00:00", finished})
	}
	return rows
}

func (f *fakeMySQL) seedTable(ddl string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createTableLocked(ddl)
}

func (f *fakeMySQL) setColumnType(table, column, typ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.tables[table].columns {
		if c.name == column {
			f.tables[table].columns[i].typ = typ
		}
	}
}

func (f *fakeMySQL) columnType(table, column string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.tables[table].columns {
		if c.name == column {
			return c.typ
		}
	}
	return ""
}

func (f *fakeMySQL) dropTable(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tables, name)
}

func (f *fakeMySQL) hasTable(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tables[name]
	return ok
}

func (f *fakeMySQL) ledgerSnapshot() []ledgerRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]ledgerRow(nil), f.ledger...)
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out
}

func (f *fakeMySQL) killsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kills...)
}

func (f *fakeMySQL) setIntercept(fn func(ctx context.Context, query string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intercept = fn
}

func openFake(t *testing.T, f *fakeMySQL) (*sql.DB, *dbtest.Recorder) {
	t.Helper()
	return dbtest.Open(t, f.handle)
}

func testOpts(t *testing.T, tables ...proto.Message) Options {
	return Options{Database: testDatabase, Tables: tables, Logf: t.Logf}
}

var writeVerbs = []string{"CREATE", "ALTER", "INSERT", "UPDATE", "DELETE", "DROP", "REPLACE", "TRUNCATE", "RENAME", "KILL"}

// writesAfter 返回 rec 第 from 条语句之后所有会改库的语句。SET SESSION / GET_LOCK 是会话级的,不算。
func writesAfter(rec *dbtest.Recorder, from int) []string {
	var out []string
	for _, s := range rec.Statements()[from:] {
		upper := strings.ToUpper(strings.TrimSpace(s))
		for _, verb := range writeVerbs {
			if strings.HasPrefix(upper, verb) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func mustIndex(t *testing.T, stmts []string, prefix string) int {
	t.Helper()
	for i, s := range stmts {
		if strings.HasPrefix(strings.TrimSpace(s), prefix) {
			return i
		}
	}
	t.Fatalf("没有执行过以 %q 开头的语句;实际: %v", prefix, collapseAll(stmts))
	return -1
}

func TestUpCreatesTablesAndRecordsBaseline(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	db, rec := openFake(t, f)

	report, err := Up(ctx, db, testOpts(t, listingTable().build(t), favoriteTable().build(t)))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if code := ExitCode(report, err); code != ExitOK {
		t.Fatalf("ExitCode = %d, want %d", code, ExitOK)
	}
	if len(report.Statements) != 2 ||
		!strings.HasPrefix(report.Statements[0], "CREATE TABLE IF NOT EXISTS `trade_listing`") ||
		!strings.HasPrefix(report.Statements[1], "CREATE TABLE IF NOT EXISTS `trade_favorite`") {
		t.Fatalf("首次 Up 应按表清单顺序建两张表,got %v", collapseAll(report.Statements))
	}
	if !f.hasTable("trade_listing") || !f.hasTable("trade_favorite") {
		t.Fatal("表没有建出来")
	}
	ledger := f.ledgerSnapshot()
	if len(ledger) != 1 || ledger[0].version != BaselineVersion || ledger[0].name != BaselineName || ledger[0].dirty {
		t.Fatalf("台账应只有一行已完成的基线,got %+v", ledger)
	}

	// 顺序是保证本身:库名断言 → 会话超时 → 拿锁 → 建台账 → DDL → 放锁。
	stmts := rec.Statements()
	order := []int{
		mustIndex(t, stmts, "SELECT DATABASE()"),
		mustIndex(t, stmts, "SET SESSION lock_wait_timeout"),
		mustIndex(t, stmts, "SELECT GET_LOCK("),
		mustIndex(t, stmts, "CREATE TABLE IF NOT EXISTS `schema_migrations`"),
		mustIndex(t, stmts, "INSERT INTO `schema_migrations`"),
		mustIndex(t, stmts, "CREATE TABLE IF NOT EXISTS `trade_listing`"),
		mustIndex(t, stmts, "UPDATE `schema_migrations`"),
		mustIndex(t, stmts, "SELECT RELEASE_LOCK("),
	}
	if !sort.IntsAreSorted(order) {
		t.Fatalf("语句顺序不对: %v\n%v", order, collapseAll(stmts))
	}
}

func TestUpIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	db, rec := openFake(t, f)
	opts := testOpts(t, listingTable().build(t), favoriteTable().build(t))

	if _, err := Up(ctx, db, opts); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	from := len(rec.Statements())
	report, err := Up(ctx, db, opts)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if !report.Clean() || len(report.Warnings) != 0 || ExitCode(report, err) != ExitOK {
		t.Fatalf("重复 Up 应无事可做,got %+v", report)
	}
	writes := writesAfter(rec, from)
	if len(writes) != 1 || !strings.HasPrefix(writes[0], "CREATE TABLE IF NOT EXISTS `schema_migrations`") {
		t.Fatalf("重复 Up 只允许幂等的台账建表语句,got %v", collapseAll(writes))
	}
	if n := len(f.ledgerSnapshot()); n != 1 {
		t.Fatalf("重复 Up 不应新增台账行,got %d 行", n)
	}
}

// 缺陷回归(D-14 第 3 条):基线跑过之后再往表清单加表,Up 必须把新表建出来。
// go/db 的 runner 在这里只打一句 WARN,新表永远不存在。
func TestUpCreatesTablesAddedAfterBaseline(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	db, rec := openFake(t, f)
	listing, favorite := listingTable().build(t), favoriteTable().build(t)

	if _, err := Up(ctx, db, testOpts(t, listing)); err != nil {
		t.Fatalf("baseline Up: %v", err)
	}

	// AutoMigrate=false 的启动门禁(Plan)必须看得见后加的表,且不写库。
	from := len(rec.Statements())
	plan, err := Plan(ctx, db, testOpts(t, listing, favorite))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Clean() || len(plan.Statements) != 1 || !strings.HasPrefix(plan.Statements[0], "CREATE TABLE IF NOT EXISTS `trade_favorite`") {
		t.Fatalf("Plan 应报出待建的 trade_favorite,got %v", collapseAll(plan.Statements))
	}
	if writes := writesAfter(rec, from); len(writes) != 0 {
		t.Fatalf("Plan 不许写库,got %v", collapseAll(writes))
	}

	report, err := Up(ctx, db, testOpts(t, listing, favorite))
	if err != nil {
		t.Fatalf("Up with new table: %v", err)
	}
	if len(report.Statements) != 1 || !strings.HasPrefix(report.Statements[0], "CREATE TABLE IF NOT EXISTS `trade_favorite`") {
		t.Fatalf("后加的表必须被建出来,got %v", collapseAll(report.Statements))
	}
	if !f.hasTable("trade_favorite") {
		t.Fatal("trade_favorite 没有建出来")
	}
	ledger := f.ledgerSnapshot()
	if len(ledger) != 2 || ledger[1].version != 2 || !strings.HasPrefix(ledger[1].name, driftNamePrefix) || ledger[1].dirty {
		t.Fatalf("补建应记成 version=2 的漂移迁移,got %+v", ledger)
	}

	report, err = Up(ctx, db, testOpts(t, listing, favorite))
	if err != nil || !report.Clean() {
		t.Fatalf("补建之后再 Up 应无事可做: report=%+v err=%v", report, err)
	}
	plan, err = Plan(ctx, db, testOpts(t, listing, favorite))
	if err != nil || !plan.Clean() {
		t.Fatalf("补建之后 Plan 应通过启动门禁: report=%+v err=%v", plan, err)
	}
}

func TestUpAddsColumnWhenProtoGrows(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	db, _ := openFake(t, f)

	if _, err := Up(ctx, db, testOpts(t, listingTable().build(t))); err != nil {
		t.Fatalf("baseline Up: %v", err)
	}
	grown := listingTable().withField(testField{"level", 8, tUint32}).build(t)
	report, err := Up(ctx, db, testOpts(t, grown))
	if err != nil {
		t.Fatalf("Up with new column: %v", err)
	}
	if len(report.Statements) != 1 || !strings.HasPrefix(report.Statements[0], "ALTER TABLE `trade_listing` ADD COLUMN `level` int unsigned") {
		t.Fatalf("proto 加字段应只生成 ADD COLUMN,got %v", report.Statements)
	}
	if got := f.columnType("trade_listing", "level"); got != "int unsigned" {
		t.Fatalf("level 列类型 = %q", got)
	}
	if ledger := f.ledgerSnapshot(); len(ledger) != 2 || ledger[1].dirty {
		t.Fatalf("加列应记成一条已完成的漂移迁移,got %+v", ledger)
	}
}

func TestUpReportsReappearedDriftAsManual(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	db, rec := openFake(t, f)
	listing, favorite := listingTable().build(t), favoriteTable().build(t)

	if _, err := Up(ctx, db, testOpts(t, listing)); err != nil {
		t.Fatalf("baseline Up: %v", err)
	}
	if _, err := Up(ctx, db, testOpts(t, listing, favorite)); err != nil {
		t.Fatalf("drift Up: %v", err)
	}
	f.dropTable("trade_favorite") // 台账记着建过,表却被人删了

	from := len(rec.Statements())
	report, err := Up(ctx, db, testOpts(t, listing, favorite))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if code := ExitCode(report, err); code != ExitManual {
		t.Fatalf("ExitCode = %d, want %d", code, ExitManual)
	}
	if len(report.Statements) != 0 || len(report.Manual) != 1 || !containsAll(report.Manual[0], "已在台账", "version=2") {
		t.Fatalf("已记账的漂移重现必须交人工且不重跑,got %+v", report)
	}
	for _, w := range writesAfter(rec, from) {
		if strings.Contains(w, "`trade_favorite`") {
			t.Fatalf("不许静默重建被删掉的表: %s", collapse(w))
		}
	}
}

func TestUpTreatsTablesRemovedFromListAsWarnings(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	db, _ := openFake(t, f)
	listing := listingTable().build(t)

	if _, err := Up(ctx, db, testOpts(t, listing, favoriteTable().build(t))); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// 模拟回滚到只认识 trade_listing 的旧版本二进制。
	report, err := Up(ctx, db, testOpts(t, listing))
	if err != nil || ExitCode(report, err) != ExitOK || len(report.Manual) != 0 {
		t.Fatalf("多余表不能阻断: report=%+v err=%v", report, err)
	}
	if len(report.Warnings) != 1 || !containsAll(report.Warnings[0], "多余表", "trade_favorite") {
		t.Fatalf("多余表应进 Warnings,got %v", report.Warnings)
	}
	plan, err := Plan(ctx, db, testOpts(t, listing))
	if err != nil || !plan.Clean() {
		t.Fatalf("只有 Warnings 时 Plan 仍应通过启动门禁: report=%+v err=%v", plan, err)
	}
	if !f.hasTable("trade_favorite") {
		t.Fatal("本包永不删表")
	}
}

func TestDirtyLedgerIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	f.ledgerExists = true
	f.ledger = []ledgerRow{{version: BaselineVersion, name: BaselineName, checksum: strings.Repeat("a", 64), dirty: true}}
	db, rec := openFake(t, f)
	listing := listingTable().build(t)

	report, err := Up(ctx, db, testOpts(t, listing))
	if !errors.Is(err, ErrDirty) {
		t.Fatalf("err = %v, want ErrDirty", err)
	}
	if code := ExitCode(report, err); code != ExitFailed {
		t.Fatalf("ExitCode = %d, want %d", code, ExitFailed)
	}
	for _, w := range writesAfter(rec, 0) {
		if !strings.HasPrefix(w, "CREATE TABLE IF NOT EXISTS `schema_migrations`") {
			t.Fatalf("dirty 台账下不许执行任何迁移语句,got %s", collapse(w))
		}
	}
	if f.hasTable("trade_listing") {
		t.Fatal("dirty 台账下不许建表")
	}

	from := len(rec.Statements())
	if _, err := Plan(ctx, db, testOpts(t, listing)); !errors.Is(err, ErrDirty) {
		t.Fatalf("Plan err = %v, want ErrDirty", err)
	}
	if writes := writesAfter(rec, from); len(writes) != 0 {
		t.Fatalf("Plan 不许写库,got %v", collapseAll(writes))
	}
}

func TestLockBusyReturnsErrLockBusy(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	f.lockBusy = true
	db, rec := openFake(t, f)
	listing := listingTable().build(t)

	report, err := Up(ctx, db, testOpts(t, listing))
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("err = %v, want ErrLockBusy", err)
	}
	if code := ExitCode(report, err); code != ExitLockBusy {
		t.Fatalf("ExitCode = %d, want %d", code, ExitLockBusy)
	}
	if writes := writesAfter(rec, 0); len(writes) != 0 {
		t.Fatalf("拿不到锁时不许写库(连台账都不建),got %v", collapseAll(writes))
	}
	if _, err := Plan(ctx, db, testOpts(t, listing)); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("Plan err = %v, want ErrLockBusy", err)
	}
}

func TestDatabaseMismatchIsRefusedBeforeLocking(t *testing.T) {
	cases := []struct {
		name    string
		current driver.Value
	}{
		{"连到别的库", "mmorpg"},
		{"连接未选库", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMySQL(testDatabase)
			f.database = tc.current
			db, rec := openFake(t, f)

			report, err := Up(context.Background(), db, testOpts(t, listingTable().build(t)))
			if !errors.Is(err, ErrDatabaseMismatch) {
				t.Fatalf("err = %v, want ErrDatabaseMismatch", err)
			}
			if !strings.Contains(err.Error(), testDatabase) {
				t.Fatalf("错误信息必须点名期望库名: %v", err)
			}
			if code := ExitCode(report, err); code != ExitFailed {
				t.Fatalf("ExitCode = %d, want %d", code, ExitFailed)
			}
			for _, s := range rec.Statements() {
				if strings.HasPrefix(s, "SELECT GET_LOCK(") {
					t.Fatal("库名不符时不许拿迁移锁")
				}
			}
			if writes := writesAfter(rec, 0); len(writes) != 0 {
				t.Fatalf("库名不符时不许写库,got %v", collapseAll(writes))
			}
		})
	}
}

func TestTypeDriftIsManualAndNeverModifiedByDefault(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	db, rec := openFake(t, f)
	listing := listingTable().build(t)

	if _, err := Up(ctx, db, testOpts(t, listing)); err != nil {
		t.Fatalf("baseline Up: %v", err)
	}
	f.setColumnType("trade_listing", "price_fen", "varchar(64)")

	from := len(rec.Statements())
	report, err := Up(ctx, db, testOpts(t, listing))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if code := ExitCode(report, err); code != ExitManual {
		t.Fatalf("ExitCode = %d, want %d", code, ExitManual)
	}
	if len(report.Statements) != 0 || len(report.Manual) != 1 || !containsAll(report.Manual[0], "列类型漂移", "trade_listing.price_fen") {
		t.Fatalf("类型漂移应只进 Manual,got %+v", report)
	}
	for _, s := range rec.Statements()[from:] {
		if strings.Contains(s, "MODIFY COLUMN") {
			t.Fatalf("默认不许执行 MODIFY COLUMN: %s", s)
		}
	}
	if got := f.columnType("trade_listing", "price_fen"); got != "varchar(64)" {
		t.Fatalf("列类型不应被改动,got %q", got)
	}
	plan, err := Plan(ctx, db, testOpts(t, listing))
	if err != nil || plan.Clean() || len(plan.Manual) != 1 {
		t.Fatalf("Plan 应报出同一项需人工: report=%+v err=%v", plan, err)
	}

	// 显式授权后才执行 MODIFY COLUMN。
	opts := testOpts(t, listing)
	opts.AllowModifyColumn = true
	report, err = Up(ctx, db, opts)
	if err != nil || ExitCode(report, err) != ExitOK {
		t.Fatalf("AllowModifyColumn Up: report=%+v err=%v", report, err)
	}
	if len(report.Statements) != 1 || !strings.HasPrefix(report.Statements[0], "ALTER TABLE `trade_listing` MODIFY COLUMN `price_fen` bigint unsigned") {
		t.Fatalf("授权后应生成 MODIFY COLUMN,got %v", report.Statements)
	}
	if got := f.columnType("trade_listing", "price_fen"); got != "bigint unsigned" {
		t.Fatalf("授权后列类型应被改回,got %q", got)
	}
}

func TestPlanExecutesNoWrites(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	db, rec := openFake(t, f)

	report, err := Plan(ctx, db, testOpts(t, listingTable().build(t), favoriteTable().build(t)))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if report.Clean() || len(report.Statements) != 2 || ExitCode(report, err) != ExitOK {
		t.Fatalf("空库上 Plan 应列出两条建表语句,got %+v", report)
	}
	if writes := writesAfter(rec, 0); len(writes) != 0 {
		t.Fatalf("Plan 不许执行任何写语句,got %v", collapseAll(writes))
	}
	if f.ledgerExists || f.hasTable("trade_listing") {
		t.Fatal("Plan 不许建台账或建表")
	}
	mustIndex(t, rec.Statements(), "SELECT RELEASE_LOCK(")
}

func TestInterruptedStatementIsKilledAndLeavesLedgerDirty(t *testing.T) {
	f := newFakeMySQL(testDatabase)
	f.connID = 4242
	db, _ := openFake(t, f)
	listing := listingTable().build(t)

	// 不用真实超时:在 DDL 执行中途取消调用方 ctx(部署超时 / 进程收到信号),行为与硬超时到期同一条路径。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.setIntercept(func(stmtCtx context.Context, q string) error {
		if strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS `trade_listing`") {
			cancel()
			<-stmtCtx.Done()
			return stmtCtx.Err()
		}
		return nil
	})

	report, err := Up(ctx, db, testOpts(t, listing))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if code := ExitCode(report, err); code != ExitFailed {
		t.Fatalf("ExitCode = %d, want %d", code, ExitFailed)
	}
	if len(report.Statements) != 0 {
		t.Fatalf("被打断的语句不能算执行成功,got %v", report.Statements)
	}
	kills := f.killsSnapshot()
	if len(kills) != 1 || kills[0] != "KILL QUERY 4242" {
		t.Fatalf("必须从旁路连接 KILL 迁移连接上的语句,got %v", kills)
	}
	ledger := f.ledgerSnapshot()
	if len(ledger) != 1 || !ledger[0].dirty {
		t.Fatalf("中途失败必须保留 dirty 台账,got %+v", ledger)
	}

	f.setIntercept(nil)
	if _, err := Up(context.Background(), db, testOpts(t, listing)); !errors.Is(err, ErrDirty) {
		t.Fatalf("dirty 之后再 Up 必须拒绝,err = %v", err)
	}
}

func TestSingleConnectionPoolIsRejected(t *testing.T) {
	f := newFakeMySQL(testDatabase)
	db, rec := openFake(t, f)
	db.SetMaxOpenConns(1)

	report, err := Up(context.Background(), db, testOpts(t, listingTable().build(t)))
	if err == nil || !containsAll(err.Error(), "MaxOpenConns=1", testDatabase) {
		t.Fatalf("单连接池必须拒绝(超时后 KILL QUERY 没有旁路连接可用),err = %v", err)
	}
	if ExitCode(report, err) != ExitFailed || len(rec.Statements()) != 0 {
		t.Fatalf("拒绝时不许碰库: statements=%v", rec.Statements())
	}
}

func TestOptionsAreValidatedBeforeTouchingTheDatabase(t *testing.T) {
	f := newFakeMySQL(testDatabase)
	db, rec := openFake(t, f)
	listing := listingTable().build(t)

	noDatabase := testOpts(t, listing)
	noDatabase.Database = ""
	cases := []struct {
		name    string
		db      *sql.DB
		opts    Options
		wantErr string
	}{
		{"nil db", nil, testOpts(t, listing), "为 nil"},
		{"缺库名", db, noDatabase, "Options.Database 必填"},
		{"空表清单", db, testOpts(t), "不能为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, run := range []func(context.Context, *sql.DB, Options) (Report, error){Plan, Up} {
				report, err := run(context.Background(), tc.db, tc.opts)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want 含 %q", err, tc.wantErr)
				}
				if ExitCode(report, err) != ExitFailed {
					t.Fatalf("ExitCode = %d, want %d", ExitCode(report, err), ExitFailed)
				}
			}
		})
	}
	if n := len(rec.Statements()); n != 0 {
		t.Fatalf("参数校验失败时不许碰库,got %d 条语句", n)
	}
}

// 已有 go/db 形状台账(基线 checksum 口径不同、已有漂移版本)的库:不重跑基线,漂移版本号接着往后排。
func TestUpContinuesGoDBShapedLedger(t *testing.T) {
	ctx := context.Background()
	f := newFakeMySQL(testDatabase)
	listing := listingTable().build(t)
	f.ledgerExists = true
	f.ledger = []ledgerRow{
		{version: 1, name: BaselineName, checksum: strings.Repeat("a", 64)},
		{version: 2, name: driftNamePrefix + strings.Repeat("b", 12), checksum: strings.Repeat("b", 64)},
	}
	f.seedTable(mustSchema(t, listing).tables[0].createSQL)
	db, _ := openFake(t, f)

	report, err := Up(ctx, db, testOpts(t, listing, favoriteTable().build(t)))
	if err != nil || ExitCode(report, err) != ExitOK {
		t.Fatalf("Up: report=%+v err=%v", report, err)
	}
	if len(report.Statements) != 1 || !strings.HasPrefix(report.Statements[0], "CREATE TABLE IF NOT EXISTS `trade_favorite`") {
		t.Fatalf("只应补建缺的表、不重跑基线,got %v", collapseAll(report.Statements))
	}
	ledger := f.ledgerSnapshot()
	if len(ledger) != 3 || ledger[2].version != 3 || ledger[2].dirty {
		t.Fatalf("漂移版本号应接在已有最大号后面,got %+v", ledger)
	}
}

func TestExitCode(t *testing.T) {
	wrappedBusy := fmt.Errorf("wrap: %w", ErrLockBusy)
	cases := []struct {
		name   string
		report Report
		err    error
		want   int
	}{
		{"成功", Report{Statements: []string{"CREATE TABLE ..."}}, nil, ExitOK},
		{"只有 Warnings", Report{Warnings: []string{"多余列"}}, nil, ExitOK},
		{"需人工", Report{Manual: []string{"列类型漂移"}}, nil, ExitManual},
		{"锁忙", Report{}, wrappedBusy, ExitLockBusy},
		{"锁忙优先于需人工", Report{Manual: []string{"x"}}, wrappedBusy, ExitLockBusy},
		{"dirty", Report{}, fmt.Errorf("wrap: %w", ErrDirty), ExitFailed},
		{"库名不符", Report{}, ErrDatabaseMismatch, ExitFailed},
		{"其他错误优先于需人工", Report{Manual: []string{"x"}}, errors.New("boom"), ExitFailed},
	}
	for _, tc := range cases {
		if got := ExitCode(tc.report, tc.err); got != tc.want {
			t.Errorf("%s: ExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
	if ExitOK != 0 || ExitFailed != 1 || ExitLockBusy != 3 || ExitManual != 4 {
		t.Fatal("退出码数值是 D-14 与 K8s Job 的契约,不许改")
	}
}

func TestReportClean(t *testing.T) {
	if !(Report{Warnings: []string{"多余列"}}).Clean() {
		t.Fatal("只有 Warnings 仍算 Clean")
	}
	if (Report{Statements: []string{"x"}}).Clean() || (Report{Manual: []string{"x"}}).Clean() {
		t.Fatal("有待执行语句或需人工项都不算 Clean")
	}
}

// 台账表形状必须与 go/db/internal/migrate/runner.go 的 ensureLedger 逐字一致;改任何一边都要同步另一边。
func TestLedgerDDLMatchesGoDB(t *testing.T) {
	want := "CREATE TABLE IF NOT EXISTS `schema_migrations` (\n" +
		"  version BIGINT UNSIGNED NOT NULL,\n" +
		"  name VARCHAR(191) NOT NULL,\n" +
		"  checksum CHAR(64) NOT NULL,\n" +
		"  dirty TINYINT(1) NOT NULL DEFAULT 0,\n" +
		"  started_at DATETIME NOT NULL,\n" +
		"  finished_at DATETIME NULL,\n" +
		"  PRIMARY KEY (version),\n" +
		"  KEY idx_schema_migrations_checksum (checksum)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='db schema migration ledger'"
	if ledgerDDL != want {
		t.Fatalf("台账建表语句与 go/db 不一致:\n got: %q\nwant: %q", ledgerDDL, want)
	}
}

func TestAdvisoryLockNameMatchesGoDB(t *testing.T) {
	// 前缀与 go/db 相同,同一个库上两个迁移工具互斥。
	if got := advisoryLockName("mmorpg_trade"); got != "mmorpg_db_migrate:mmorpg_trade" {
		t.Fatalf("lock name = %q", got)
	}
	if advisoryLockName("mmorpg_trade") == advisoryLockName("mmorpg_mail") {
		t.Fatal("different databases must not share a migration lock")
	}
	if long := advisoryLockName(strings.Repeat("x", 200)); len(long) > 64 {
		t.Fatalf("MySQL GET_LOCK names cap at 64 bytes, got %d", len(long))
	}
}

func TestCeilSeconds(t *testing.T) {
	cases := []struct {
		d    time.Duration
		max  int64
		want int64
	}{
		{0, 100, 1},
		{time.Nanosecond, 100, 1},
		{time.Second, 100, 1},
		{1500 * time.Millisecond, 100, 2},
		{10 * time.Second, 100, 10},
		{time.Hour, 100, 100},
	}
	for _, tc := range cases {
		if got := ceilSeconds(tc.d, tc.max); got != tc.want {
			t.Errorf("ceilSeconds(%s, %d) = %d, want %d", tc.d, tc.max, got, tc.want)
		}
	}
}

func TestDefaultsApplyToZeroOptions(t *testing.T) {
	o := Options{}.withDefaults()
	if o.AdvisoryLockWait != 10*time.Second || o.SessionLockWait != 5*time.Second || o.StatementTimeout != 60*time.Second {
		t.Fatalf("零值 Options 的默认值与规格不符: %+v", o)
	}
}
