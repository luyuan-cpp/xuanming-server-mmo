//go:build integration

package schemamigrate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

// 真库冒烟:go test -tags integration -run TestIntegration ./...
//
// SCHEMAMIGRATE_TEST_MYSQL_DSN 为空则 Skip。**只能指向一次性库**:本测试会 DROP
// smit_listing / smit_favorite 两张表与 schema_migrations 台账。DSN 必须带库名,
// parseTime 开不开都行(台账时间列扫进字符串)。示例:
//
//	appuser:<密码>@tcp(127.0.0.1:3306)/schemamigrate_it
//
// 该库要先手工建好:本包不建库。
const integrationDSNEnv = "SCHEMAMIGRATE_TEST_MYSQL_DSN"

func TestIntegrationUpLifecycle(t *testing.T) {
	dsn := os.Getenv(integrationDSNEnv)
	if dsn == "" {
		t.Skipf("%s 未设置,跳过真库用例", integrationDSNEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(4)

	var current sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
		t.Fatalf("SELECT DATABASE(): %v", err)
	}
	if !current.Valid || current.String == "" {
		t.Fatalf("%s 必须带库名", integrationDSNEnv)
	}
	database := current.String

	drop := func() {
		for _, stmt := range []string{
			"DROP TABLE IF EXISTS `smit_favorite`",
			"DROP TABLE IF EXISTS `smit_listing`",
			"DROP TABLE IF EXISTS `schema_migrations`",
		} {
			if _, err := db.ExecContext(context.Background(), stmt); err != nil {
				t.Errorf("%s: %v", stmt, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)

	listingSpec := listingTable()
	listingSpec.table = "smit_listing"
	favoriteSpec := favoriteTable()
	favoriteSpec.table = "smit_favorite"
	listing := listingSpec.build(t)
	favorite := favoriteSpec.build(t)
	opts := func(tables ...proto.Message) Options {
		return Options{Database: database, Tables: tables, Logf: t.Logf}
	}

	// 1. 首次 Up:建表 + 写基线台账。
	report, err := Up(ctx, db, opts(listing))
	if err != nil || ExitCode(report, err) != ExitOK || len(report.Statements) != 1 {
		t.Fatalf("首次 Up: report=%+v err=%v", report, err)
	}
	assertLedgerRows(ctx, t, db, 1)

	// 2. 重复 Up 幂等。
	report, err = Up(ctx, db, opts(listing))
	if err != nil || !report.Clean() {
		t.Fatalf("重复 Up: report=%+v err=%v", report, err)
	}

	// 3. 后加表被建出(缺陷回归),且 Plan 先能看见。
	plan, err := Plan(ctx, db, opts(listing, favorite))
	if err != nil || plan.Clean() {
		t.Fatalf("加表前 Plan 应不 Clean: report=%+v err=%v", plan, err)
	}
	report, err = Up(ctx, db, opts(listing, favorite))
	if err != nil || len(report.Statements) != 1 || !strings.Contains(report.Statements[0], "`smit_favorite`") {
		t.Fatalf("后加表 Up: report=%+v err=%v", report, err)
	}
	assertLedgerRows(ctx, t, db, 2)
	plan, err = Plan(ctx, db, opts(listing, favorite))
	if err != nil || !plan.Clean() {
		t.Fatalf("加表后 Plan 应 Clean: report=%+v err=%v", plan, err)
	}

	// 4. proto 加字段 → ADD COLUMN。
	grownSpec := listingSpec.withField(testField{"level", 8, tUint32})
	grown := grownSpec.build(t)
	report, err = Up(ctx, db, opts(grown, favorite))
	if err != nil || len(report.Statements) != 1 || !strings.Contains(report.Statements[0], "ADD COLUMN `level`") {
		t.Fatalf("加列 Up: report=%+v err=%v", report, err)
	}

	// 5. 类型漂移 → Manual / ExitCode 4,且不执行 MODIFY。
	if _, err := db.ExecContext(ctx, "ALTER TABLE `smit_listing` MODIFY COLUMN `price_fen` varchar(64) NOT NULL DEFAULT ''"); err != nil {
		t.Fatalf("制造类型漂移: %v", err)
	}
	report, err = Up(ctx, db, opts(grown, favorite))
	if err != nil || ExitCode(report, err) != ExitManual || len(report.Statements) != 0 {
		t.Fatalf("类型漂移 Up: report=%+v err=%v", report, err)
	}
	var columnType string
	if err := db.QueryRowContext(ctx,
		"SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'smit_listing' AND COLUMN_NAME = 'price_fen'",
		database).Scan(&columnType); err != nil {
		t.Fatalf("读列类型: %v", err)
	}
	if !strings.HasPrefix(strings.ToLower(columnType), "varchar") {
		t.Fatalf("默认不许 MODIFY COLUMN,列类型却变成了 %q", columnType)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE `smit_listing` MODIFY COLUMN `price_fen` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:6'"); err != nil {
		t.Fatalf("恢复列类型: %v", err)
	}

	// 6. 锁被另一条连接持有 → ErrLockBusy / ExitCode 3。
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	var got sql.NullInt64
	if err := holder.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", advisoryLockName(database)).Scan(&got); err != nil || got.Int64 != 1 {
		_ = holder.Close()
		t.Fatalf("holder GET_LOCK: got=%v err=%v", got, err)
	}
	busyOpts := opts(grown, favorite)
	busyOpts.AdvisoryLockWait = time.Second
	report, err = Up(ctx, db, busyOpts)
	if !errors.Is(err, ErrLockBusy) || ExitCode(report, err) != ExitLockBusy {
		t.Errorf("锁忙: report=%+v err=%v", report, err)
	}
	_, _ = holder.ExecContext(ctx, "DO RELEASE_LOCK(?)", advisoryLockName(database))
	_ = holder.Close()

	// 7. dirty 台账 → ErrDirty。
	if _, err := db.ExecContext(ctx, "UPDATE `schema_migrations` SET dirty = 1 WHERE version = 1"); err != nil {
		t.Fatalf("制造 dirty: %v", err)
	}
	if _, err := Up(ctx, db, opts(grown, favorite)); !errors.Is(err, ErrDirty) {
		t.Fatalf("dirty Up err = %v, want ErrDirty", err)
	}
	if _, err := Plan(ctx, db, opts(grown, favorite)); !errors.Is(err, ErrDirty) {
		t.Fatalf("dirty Plan err = %v, want ErrDirty", err)
	}

	// 8. 库名不符 → ErrDatabaseMismatch。
	if _, err := Plan(ctx, db, Options{Database: database + "_other", Tables: []proto.Message{grown}}); !errors.Is(err, ErrDatabaseMismatch) {
		t.Fatalf("库名不符 err = %v, want ErrDatabaseMismatch", err)
	}
}

func assertLedgerRows(ctx context.Context, t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `schema_migrations` WHERE dirty = 0").Scan(&n); err != nil {
		t.Fatalf("读台账: %v", err)
	}
	if n != want {
		t.Fatalf("台账已完成行数 = %d, want %d", n, want)
	}
}
