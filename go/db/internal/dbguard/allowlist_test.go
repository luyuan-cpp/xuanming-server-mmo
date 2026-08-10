package dbguard

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"db/internal/dbtest"
)

// selectDatabaseHandler 只回答 SELECT DATABASE(),其余语句报错。
func selectDatabaseHandler(name string) dbtest.Handler {
	return func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		if query != "SELECT DATABASE()" {
			return nil, errors.New("unexpected query: " + query)
		}
		return &dbtest.Rows{
			Columns: []string{"DATABASE()"},
			Values:  [][]driver.Value{{name}},
		}, nil
	}
}

func TestAllowlistResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "allow.txt")
	if err := os.WriteFile(file, []byte("# 注释行\nzone_from_file\n\n"), 0o600); err != nil {
		t.Fatalf("write allow file: %v", err)
	}

	spec := AllowlistSpec{File: file, Inline: []string{"zone_from_inline"}}

	t.Setenv(AllowedDatabasesEnv, " zone_from_env , zone_from_env_2 ")
	got, err := spec.Resolve()
	if err != nil {
		t.Fatalf("resolve with env: %v", err)
	}
	if got.Source != "env:"+AllowedDatabasesEnv {
		t.Fatalf("env must win, got source=%s", got.Source)
	}
	if !got.Contains("zone_from_env") || !got.Contains("zone_from_env_2") {
		t.Fatalf("env list not parsed: %v", got.Names)
	}
	if got.Contains("zone_from_file") || got.Contains("zone_from_inline") {
		t.Fatalf("resolve must not merge lower-priority sources: %v", got.Names)
	}

	// env 为空 -> 落到文件
	t.Setenv(AllowedDatabasesEnv, "")
	got, err = spec.Resolve()
	if err != nil {
		t.Fatalf("resolve with file: %v", err)
	}
	if got.Source != "file:"+file || !got.Contains("zone_from_file") {
		t.Fatalf("file source expected, got %s %v", got.Source, got.Names)
	}
	if got.Contains("# 注释行") {
		t.Fatalf("comment line must be stripped: %v", got.Names)
	}

	// 没有文件 -> 落到 yaml 内联
	got, err = AllowlistSpec{Inline: []string{"zone_from_inline"}}.Resolve()
	if err != nil {
		t.Fatalf("resolve inline: %v", err)
	}
	if got.Source != "config:ServerConfig.Database.AllowedDatabases" || !got.Contains("zone_from_inline") {
		t.Fatalf("inline source expected, got %s %v", got.Source, got.Names)
	}

	// 三个来源全空
	got, err = AllowlistSpec{}.Resolve()
	if err != nil {
		t.Fatalf("resolve empty: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("expected empty allowlist, got %v", got.Names)
	}
}

func TestAllowlistResolveMissingFileIsFatal(t *testing.T) {
	// 显式配了文件却读不到 = 部署事故,绝不能静默回落到 yaml 内联清单,
	// 否则「我收紧了白名单」会被镜像里的旧 yaml 悄悄放宽。
	_, err := AllowlistSpec{File: filepath.Join(t.TempDir(), "missing.txt"), Inline: []string{"zone_1_db"}}.Resolve()
	if err == nil {
		t.Fatal("missing allow list file must be fatal, not silently ignored")
	}
}

func TestAssertDatabaseRejectsUnlisted(t *testing.T) {
	db, _ := dbtest.Open(t, selectDatabaseHandler("zone_11_db"))

	// 这就是 ZoneId 填错一位的场景:进程自己推导出 zone_11_db 并连上了它,
	// 但部署侧注入的清单里只有 zone_1_db。必须拒启。
	_, err := AssertDatabase(context.Background(), db, AssertOptions{
		Expected: "zone_11_db",
		Allow:    Allowlist{Names: []string{"zone_1_db"}, Source: "test"},
	})
	if !errors.Is(err, ErrDatabaseNotAllowed) {
		t.Fatalf("expected ErrDatabaseNotAllowed, got %v", err)
	}
}

func TestAssertDatabaseAcceptsListed(t *testing.T) {
	db, _ := dbtest.Open(t, selectDatabaseHandler("zone_1_db"))

	name, err := AssertDatabase(context.Background(), db, AssertOptions{
		Expected: "zone_1_db",
		Allow:    Allowlist{Names: []string{"zone_1_db", "zone_2_db"}, Source: "test"},
	})
	if err != nil {
		t.Fatalf("listed database must pass: %v", err)
	}
	if name != "zone_1_db" {
		t.Fatalf("unexpected connected name %q", name)
	}
}

func TestAssertDatabaseRejectsMismatchWithDerivedName(t *testing.T) {
	// 服务端实际选中的库和进程推导出来的不一致(DSN 被改 / 连错实例)。
	db, _ := dbtest.Open(t, selectDatabaseHandler("zone_2_db"))

	_, err := AssertDatabase(context.Background(), db, AssertOptions{
		Expected: "zone_1_db",
		Allow:    Allowlist{Names: []string{"zone_1_db", "zone_2_db"}, Source: "test"},
	})
	if !errors.Is(err, ErrDatabaseNotAllowed) {
		t.Fatalf("expected mismatch to be rejected even when both names are allowed, got %v", err)
	}
}

func TestAssertDatabaseEmptyAllowlistIsFatalByDefault(t *testing.T) {
	db, _ := dbtest.Open(t, selectDatabaseHandler("zone_1_db"))

	_, err := AssertDatabase(context.Background(), db, AssertOptions{Expected: "zone_1_db"})
	if !errors.Is(err, ErrAllowlistMissing) {
		t.Fatalf("empty allowlist must be fail-closed, got %v", err)
	}
}

func TestAssertDatabaseEmptyAllowlistRelaxedWarns(t *testing.T) {
	db, _ := dbtest.Open(t, selectDatabaseHandler("zone_1_db"))

	var warned int
	_, err := AssertDatabase(context.Background(), db, AssertOptions{
		Expected:            "zone_1_db",
		RelaxEmptyAllowlist: true,
		Warnf:               func(string, ...any) { warned++ },
	})
	if err != nil {
		t.Fatalf("relaxed mode must pass: %v", err)
	}
	if warned != 1 {
		t.Fatalf("relaxed mode must emit exactly one warning, got %d", warned)
	}
}

func TestAssertDatabaseRejectsNoDefaultSchema(t *testing.T) {
	db, _ := dbtest.Open(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		return &dbtest.Rows{Columns: []string{"DATABASE()"}, Values: [][]driver.Value{{nil}}}, nil
	})

	if _, err := AssertDatabase(context.Background(), db, AssertOptions{
		Allow: Allowlist{Names: []string{"zone_1_db"}},
	}); err == nil {
		t.Fatal("a connection without a default schema must be rejected")
	}
}
