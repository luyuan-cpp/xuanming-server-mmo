package debugutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// 真库用例(r2-ds#1,2026-10-22):只在给了本机一次性实例的 DSN 时跑,缺省跳过。每个用例建一个随机库、结束删掉,
// 不碰实例上已有的库。DSN 不带库名,例如:
//
//	DEBUGUTIL_MYSQL_TEST_DSN=root:<pw>@tcp(127.0.0.1:13306)/
//	DEBUGUTIL_TIDB_TEST_DSN=root@tcp(127.0.0.1:14000)/

func TestRealMySQLServerIdentityAndMarker(t *testing.T) {
	db := openIdentityTestDB(t, "DEBUGUTIL_MYSQL_TEST_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// MySQL 上 tidb_version() 报错之后,同一个池还要能接着取 server_uuid。
	if id := MySQLServerIdentity(ctx, db); len(id) != 36 {
		t.Fatalf("MySQL 应取到 36 位 server_uuid,得到 %q", id)
	}
	requireMarkerLifecycle(ctx, t, db)
}

func TestRealTiDBIdentityUnknownAndMarker(t *testing.T) {
	db := openIdentityTestDB(t, "DEBUGUTIL_TIDB_TEST_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if id := MySQLServerIdentity(ctx, db); id != "" {
		t.Fatalf("TiDB 的实例身份必须按未知处理,得到 %q", id)
	}
	// 只记录、不断言:TiDB 对 @@server_uuid 的实际回答,供设计文档核实(本仓库此前未核实)。
	var raw sql.NullString
	err := db.QueryRowContext(ctx, serverUUIDQuery).Scan(&raw)
	t.Logf("TiDB SELECT @@server_uuid -> value=%q valid=%v err=%v", raw.String, raw.Valid, err)
	requireMarkerLifecycle(ctx, t, db)
}

// requireMarkerLifecycle:新库没有标记;执行手册给出的建标记语句之后有;语句可重复执行。
func requireMarkerLifecycle(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	if HasLocalTargetMarker(ctx, db) {
		t.Fatal("新建的库不应带本地标记")
	}
	for round := 0; round < 2; round++ {
		for _, stmt := range strings.Split(LocalTargetMarkerSetupSQL, ";") {
			if stmt = strings.TrimSpace(stmt); stmt == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("第 %d 轮执行建标记语句 %q 失败: %v", round+1, stmt, err)
			}
		}
		if !HasLocalTargetMarker(ctx, db) {
			t.Fatalf("第 %d 轮建标记之后仍读不到标记", round+1)
		}
	}
}

func openIdentityTestDB(t *testing.T, env string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("未配置 %s", env)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLoopbackHost(host) {
		t.Fatalf("%s 只能指向本机一次性实例,得到 %s", env, cfg.Addr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "debugutil_identity_it_" + hex.EncodeToString(nonce[:])
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		_ = admin.Close()
	})

	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
