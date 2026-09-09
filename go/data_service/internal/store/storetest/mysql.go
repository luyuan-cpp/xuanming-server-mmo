//go:build integration

// Package storetest 给 data_service 的 MySQL 集成测试提供一次性(throwaway)数据库。
//
// 只在 `-tags=integration` 下编译(与 shared/snowflakealloc、scene_manager/noderegistry
// 的集成测试同一约定),默认 `go test ./...` 完全不碰它。
//
// 连接参数默认取 etc/data_service.yaml 的 SnapshotMySQL 段(host / user / password);
// 但 **DBName 刻意不用**:每个用例都 CREATE 一个 `data_service_it_<pid>_<n>` 库,
// 用完 DROP,yaml 里指向的库(testdb)一行都不会被碰。
//
// 为什么留环境变量覆盖:本地 docker MySQL(deploy/docker-compose.yml)的 appuser 只对
// mmorpg / zone_*_db 有权限,没有全局 CREATE,建不了一次性库。此时用
// DATA_SERVICE_IT_MYSQL_USER / DATA_SERVICE_IT_MYSQL_PASSWORD 指到 root 跑一次即可,
// 不需要对容器做任何持久的授权改动。建库被拒(1044)时用例 Skip 并给出提示,不算失败。
package storetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/conf"
)

// 环境变量:全部可选。
const (
	// EnvHost 覆盖 yaml 的 SnapshotMySQL.Host(host:port)。
	EnvHost = "DATA_SERVICE_IT_MYSQL_HOST"
	// EnvUser 覆盖 yaml 的 SnapshotMySQL.User;需要有 CREATE/DROP DATABASE 权限。
	EnvUser = "DATA_SERVICE_IT_MYSQL_USER"
	// EnvPassword 覆盖 yaml 的 SnapshotMySQL.Password。
	EnvPassword = "DATA_SERVICE_IT_MYSQL_PASSWORD"
	// EnvYaml 覆盖 data_service.yaml 的路径;默认从测试 cwd 向上找 etc/data_service.yaml。
	EnvYaml = "DATA_SERVICE_IT_YAML"

	// dbNamePrefix 一次性库名前缀;MySQL 标识符上限 64 字符,pid + 计数器远够。
	dbNamePrefix = "data_service_it_"

	connectTimeout = 5 * time.Second
	// mysqlErrDBAccessDenied 1044:账号对目标库没有权限(这里 = 没有 CREATE DATABASE 权)。
	mysqlErrDBAccessDenied = 1044
	// mysqlErrAccessDenied 1045:账号/密码错。
	mysqlErrAccessDenied = 1045
)

// dbCounter 让同一进程内的多个用例各拿到不同的库名(用例可能并行)。
var dbCounter atomic.Uint32

// DB 是一个已建好的一次性库。
type DB struct {
	// Name 库名。
	Name string
	// Cfg 指向本库的 store 连接参数,直接喂给 store.New*Store / store.MigrateSchema。
	Cfg store.MySQLConfig
	// Raw 是一条已 USE 本库的管理连接,用于用例里的直接断言 / 造数据(种子行、查列)。
	Raw *sql.DB
}

// mysqlSection 只装 yaml 里本包关心的一段;go-zero conf 忽略多余键,
// 不必把整份 Config(含 RpcServerConf 的必填校验)都加载进来。
type mysqlSection struct {
	SnapshotMySQL config.SnapshotMySQLConfig `json:",optional"`
}

// NewEmptyDB 建一个空的一次性库(不建表),测试结束自动 DROP。
// MySQL 连不上或没有建库权限时 Skip。
func NewEmptyDB(t *testing.T) *DB {
	t.Helper()

	host, user, password := resolveCredentials(t)
	adminDSN := fmt.Sprintf("%s:%s@tcp(%s)/?parseTime=true&charset=utf8mb4", user, password, host)
	admin, err := sql.Open("mysql", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		if isMySQLErr(err, mysqlErrAccessDenied) {
			t.Skipf("MySQL %s rejected user %q (set %s/%s to an account that can CREATE DATABASE): %v",
				host, user, EnvUser, EnvPassword, err)
		}
		t.Skipf("MySQL %s unreachable, skipping integration test: %v", host, err)
	}

	name := fmt.Sprintf("%s%d_%d", dbNamePrefix, os.Getpid(), dbCounter.Add(1))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		admin.Close()
		if isMySQLErr(err, mysqlErrDBAccessDenied) {
			t.Skipf("user %q cannot CREATE DATABASE on %s; rerun with %s/%s pointing at an admin account "+
				"(local docker: root, password in deploy/docker-compose.yml): %v", user, host, EnvUser, EnvPassword, err)
		}
		t.Fatalf("create throwaway database %s: %v", name, err)
	}
	t.Logf("[storetest] created throwaway database %s on %s as %s", name, host, user)

	rawDSN := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4", user, password, host, name)
	raw, err := sql.Open("mysql", rawDSN)
	if err != nil {
		admin.Close()
		t.Fatalf("open raw connection to %s: %v", name, err)
	}
	raw.SetMaxOpenConns(4)

	t.Cleanup(func() {
		raw.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), connectTimeout)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			t.Errorf("[storetest] drop throwaway database %s failed (drop it by hand): %v", name, err)
		}
		admin.Close()
	})

	return &DB{
		Name: name,
		Cfg: store.MySQLConfig{
			Host:     host,
			User:     user,
			Password: password,
			DBName:   name,
			// 并发用例(32 goroutine 领段)需要比默认 5 大的池,免得把锁等待误判成池饥饿。
			MaxOpenConn: 40,
			MaxIdleConn: 8,
		},
		Raw: raw,
	}
}

// NewMigratedDB 建库并跑一遍 store.MigrateSchema(四张表就位)。
//
// 刻意**不**预建任何 id_segment 行(MigrateOptions 为零值):多数用例要的是"表在、行空"
// 的起点,好断言首次使用 / 拒绝路径对表零写入。需要 bootstrap 行的用例自己再调一次
// store.MigrateSchema 并传 BootstrapTags(迁移幂等,重复跑不改已建好的表)。
func NewMigratedDB(t *testing.T) *DB {
	t.Helper()
	db := NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}); err != nil {
		t.Fatalf("MigrateSchema on %s: %v", db.Name, err)
	}
	return db
}

// Columns 返回表的列名集合(INFORMATION_SCHEMA),用于结构断言。
func (d *DB) Columns(t *testing.T, table string) map[string]bool {
	t.Helper()
	rows, err := d.Raw.Query(
		`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`,
		d.Name, table)
	if err != nil {
		t.Fatalf("list columns of %s.%s: %v", d.Name, table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan column of %s: %v", table, err)
		}
		out[c] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns of %s: %v", table, err)
	}
	return out
}

// ShowCreateTable 返回 SHOW CREATE TABLE 的 DDL 文本,用于"第二次迁移零变更"的比对。
func (d *DB) ShowCreateTable(t *testing.T, table string) string {
	t.Helper()
	var name, ddl string
	if err := d.Raw.QueryRow("SHOW CREATE TABLE `"+table+"`").Scan(&name, &ddl); err != nil {
		t.Fatalf("SHOW CREATE TABLE %s.%s: %v", d.Name, table, err)
	}
	return ddl
}

// Count 返回 `SELECT COUNT(*) FROM <table> WHERE <where>` 的结果。
func (d *DB) Count(t *testing.T, table, where string, args ...interface{}) int64 {
	t.Helper()
	q := "SELECT COUNT(*) FROM `" + table + "`"
	if where != "" {
		q += " WHERE " + where
	}
	var n int64
	if err := d.Raw.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count %s where %q: %v", table, where, err)
	}
	return n
}

// resolveCredentials 读 yaml 的 SnapshotMySQL 段,再套环境变量覆盖。
func resolveCredentials(t *testing.T) (host, user, password string) {
	t.Helper()
	var sec mysqlSection
	yamlPath := findYaml(t)
	if err := conf.Load(yamlPath, &sec); err != nil {
		t.Fatalf("load %s: %v", yamlPath, err)
	}
	host, user, password = sec.SnapshotMySQL.Host, sec.SnapshotMySQL.User, sec.SnapshotMySQL.Password
	if v := os.Getenv(EnvHost); v != "" {
		host = v
	}
	if v := os.Getenv(EnvUser); v != "" {
		user = v
	}
	if v := os.Getenv(EnvPassword); v != "" {
		password = v
	}
	if host == "" || user == "" {
		t.Fatalf("SnapshotMySQL host/user empty after loading %s and env overrides", yamlPath)
	}
	return host, user, password
}

// findYaml 从测试进程 cwd(包目录)向上最多 6 层找 etc/data_service.yaml。
func findYaml(t *testing.T) string {
	t.Helper()
	if v := os.Getenv(EnvYaml); v != "" {
		return v
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "etc", "data_service.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("etc/data_service.yaml not found above %s; set %s", dir, EnvYaml)
	return ""
}

func isMySQLErr(err error, number uint16) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == number
}
