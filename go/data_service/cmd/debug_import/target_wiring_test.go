package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"data_service/cmd/debugutil"
	"data_service/internal/config"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	zeroredis "github.com/zeromicro/go-zero/core/stores/redis"
)

// r2-ds#1 评审补充(2026-10-22):写前闸门二的接线。judgeImportTargets 的纯判定另有用例;这里钉住「取身份与本地标记
// 再交给判定」这一步:把 checkBundleWriteTargets 改回旧的「空身份跳过」写法、漏读 Redis 标记、或旧 player 模式
// 漏掉某个写目标,这里都会变红。MySQL / TiDB 目标用只认身份探测与标记查询的假驱动,Redis 目标用 miniredis
// (miniredis 不回 INFO server 的 run_id,Redis 目标身份因此为空,正好说明 Redis 只靠标记)。

func TestCheckBundleWriteTargetsTiDBWithoutMarkerRejected(t *testing.T) {
	ctx := context.Background()
	redisTarget := markedRedis(t)
	targets := bundleWriteTargets{
		zoneDB:       openFakeStore(t, fakeStore{tidb: true, serverUUID: "per-tidb-server-value"}),
		globalDB:     openFakeStore(t, fakeStore{tidb: true, marker: true}),
		coreRedis:    redisTarget,
		mappingRedis: redisTarget,
	}
	err := checkBundleWriteTargets(ctx, tidbExportSources(), targets, false)
	if err == nil {
		t.Fatal("TiDB 落点库没有本地标记却被放行(线上 TiDB 经 port-forward 混进配置时会被写)")
	}
	if !strings.Contains(err.Error(), "MySQL 落点库(") || strings.Contains(err.Error(), "全局账号目录SQL(") {
		t.Fatalf("只有没建标记的 TiDB 落点库该被拒: %v", err)
	}

	targets.zoneDB = openFakeStore(t, fakeStore{tidb: true, marker: true})
	if err := checkBundleWriteTargets(ctx, tidbExportSources(), targets, false); err != nil {
		t.Fatalf("两个 TiDB 库都建了标记、Redis 也有标记,应放行: %v", err)
	}
	if err := checkBundleWriteTargets(ctx, tidbExportSources(), bundleWriteTargets{
		zoneDB: openFakeStore(t, fakeStore{tidb: true}), globalDB: openFakeStore(t, fakeStore{tidb: true}),
		coreRedis: redisTarget, mappingRedis: redisTarget,
	}, true); err != nil {
		t.Fatalf("显式放行时应放行(只打警告): %v", err)
	}
}

func TestCheckBundleWriteTargetsRedisNeedsMarker(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	core := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = core.Close() })
	targets := bundleWriteTargets{
		zoneDB:       openFakeStore(t, fakeStore{serverUUID: "local-mysql-uuid"}),
		globalDB:     openFakeStore(t, fakeStore{serverUUID: "local-mysql-uuid"}),
		coreRedis:    core,
		mappingRedis: core,
	}
	// MySQL 两侧身份齐全且不同(由身份比对证明);核心 Redis 没有标记,必须拒绝。
	err := checkBundleWriteTargets(ctx, mysqlExportSources(), targets, false)
	if err == nil || !strings.Contains(err.Error(), "核心 Redis(") || strings.Contains(err.Error(), "MySQL 落点库(") {
		t.Fatalf("没有标记的 Redis 必须拒绝,身份已证明不同的 MySQL 不该被拒: %v", err)
	}

	mr.Set(debugutil.LocalRedisMarkerKey, debugutil.LocalTargetMarkerValue)
	if err := checkBundleWriteTargets(ctx, mysqlExportSources(), targets, false); err != nil {
		t.Fatalf("Redis 建了标记之后应放行: %v", err)
	}

	// data_service MappingRedis 是另一台实例时也要有自己的标记。
	unmarked := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = unmarked.Close() })
	targets.dataServiceMapping = unmarked
	if err := checkBundleWriteTargets(ctx, mysqlExportSources(), targets, false); err == nil ||
		!strings.Contains(err.Error(), "data_service MappingRedis(") {
		t.Fatalf("data_service MappingRedis 没有标记必须拒绝: %v", err)
	}
}

// MySQL 目标的 server_uuid 必须真取出来参与比对:与导出来源相同时,标记与放行都越不过。
func TestCheckBundleWriteTargetsRejectsMySQLExportSource(t *testing.T) {
	redisTarget := markedRedis(t)
	targets := bundleWriteTargets{
		zoneDB:       openFakeStore(t, fakeStore{serverUUID: "prod-mysql-uuid", marker: true}),
		globalDB:     openFakeStore(t, fakeStore{serverUUID: "local-mysql-uuid"}),
		coreRedis:    redisTarget,
		mappingRedis: redisTarget,
	}
	if err := checkBundleWriteTargets(context.Background(), mysqlExportSources(), targets, true); err == nil ||
		!strings.Contains(err.Error(), "同一个实例") {
		t.Fatalf("落点库就是导出来源却没被拒: %v", err)
	}
}

// 旧 player 模式:数据 Redis 恒要标记;homeZone>0 时还要登记映射,MappingRedis 也要标记。
func TestCheckLegacyPlayerTargetsMarked(t *testing.T) {
	ctx := context.Background()
	data := markedRedis(t)
	mappingServer := miniredis.RunT(t)
	cfg := config.Config{MappingRedis: zeroredis.RedisConf{Host: mappingServer.Addr(), Type: "node"}}

	if err := checkLegacyPlayerTargetsMarked(ctx, cfg, data, 0, false); err != nil {
		t.Fatalf("homeZone=0 不登记映射,只要数据 Redis 有标记就该放行: %v", err)
	}
	err := checkLegacyPlayerTargetsMarked(ctx, cfg, data, 1, false)
	if err == nil || !strings.Contains(err.Error(), "data_service MappingRedis(") || strings.Contains(err.Error(), "data_service 数据 Redis(") {
		t.Fatalf("要登记映射时,没有标记的 MappingRedis 必须拒绝: %v", err)
	}
	mappingServer.Set(debugutil.LocalRedisMarkerKey, debugutil.LocalTargetMarkerValue)
	if err := checkLegacyPlayerTargetsMarked(ctx, cfg, data, 1, false); err != nil {
		t.Fatalf("两台 Redis 都有标记应放行: %v", err)
	}

	unmarkedData := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = unmarkedData.Close() })
	if err := checkLegacyPlayerTargetsMarked(ctx, cfg, unmarkedData, 1, false); err == nil ||
		!strings.Contains(err.Error(), "data_service 数据 Redis(") {
		t.Fatalf("没有标记的数据 Redis 必须拒绝: %v", err)
	}
	if err := checkLegacyPlayerTargetsMarked(ctx, cfg, unmarkedData, 1, true); err != nil {
		t.Fatalf("显式放行时应放行(只打警告): %v", err)
	}
}

// 旧 playerdb 模式:落点库只能凭本地标记证明(旧导出文件不记来源身份,身份比对无从谈起)。
func TestLegacyPlayerDBTargetNeedsMarker(t *testing.T) {
	ctx := context.Background()
	check := func(store fakeStore, allow bool) error {
		return admitWriteTargets(nil, []importTarget{mysqlImportTarget(ctx, roleZoneDB, "MySQL 落点库", openFakeStore(t, store))}, allow)
	}
	if err := check(fakeStore{serverUUID: "local-mysql-uuid"}, false); err == nil || !strings.Contains(err.Error(), "MySQL 落点库(") {
		t.Fatalf("没有标记的落点库必须拒绝(身份取得到也不行): %v", err)
	}
	if err := check(fakeStore{tidb: true}, false); err == nil || !strings.Contains(err.Error(), "TiDB") {
		t.Fatalf("没有标记的 TiDB 落点库必须拒绝: %v", err)
	}
	if err := check(fakeStore{serverUUID: "local-mysql-uuid", marker: true}, false); err != nil {
		t.Fatalf("带标记的落点库应放行: %v", err)
	}
	if err := check(fakeStore{tidb: true}, true); err != nil {
		t.Fatalf("显式放行时应放行(只打警告): %v", err)
	}
}

// markedRedis 返回一台建了本地标记的 miniredis 的客户端。
func markedRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.Set(debugutil.LocalRedisMarkerKey, debugutil.LocalTargetMarkerValue)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// ── 假 MySQL / TiDB:只回答身份探测(tidb_version()、@@server_uuid)与本地标记查询,别的语句一律报错 ──

// fakeStore 描述一个写目标库。tidb=true 时 tidb_version() 答得出(TiDB),否则报 1305(MySQL);
// @@server_uuid 在 TiDB 上也照答,用来证明 TiDB 不取它。marker=false 时标记表不存在。
type fakeStore struct {
	tidb       bool
	serverUUID string
	marker     bool
}

func openFakeStore(t *testing.T, store fakeStore) *sql.DB {
	t.Helper()
	db := sql.OpenDB(fakeStoreConnector{store: store})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fakeStoreConnector struct{ store fakeStore }

func (c fakeStoreConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeStoreConn{store: c.store}, nil
}

func (fakeStoreConnector) Driver() driver.Driver { return fakeStoreDriver{} }

type fakeStoreDriver struct{}

func (fakeStoreDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("假库只经 sql.OpenDB(fakeStoreConnector) 使用")
}

type fakeStoreConn struct{ store fakeStore }

func (c fakeStoreConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "tidb_version()"):
		if c.store.tidb {
			return &fakeStoreRows{value: "Release Version: v8.5.2"}, nil
		}
		return nil, &mysql.MySQLError{Number: 1305, Message: "FUNCTION zone_1_db.tidb_version does not exist"}
	case strings.Contains(query, "@@server_uuid"):
		return &fakeStoreRows{value: c.store.serverUUID}, nil
	case strings.Contains(query, debugutil.LocalTargetMarkerTable):
		if !c.store.marker {
			return nil, &mysql.MySQLError{Number: 1146, Message: "Table '" + debugutil.LocalTargetMarkerTable + "' doesn't exist"}
		}
		return &fakeStoreRows{value: int64(1)}, nil
	}
	return nil, fmt.Errorf("unexpected query %q", query)
}

func (fakeStoreConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("unexpected prepare %q", query)
}

func (fakeStoreConn) Close() error { return nil }

func (fakeStoreConn) Begin() (driver.Tx, error) { return nil, errors.New("假库不支持事务") }

type fakeStoreRows struct {
	value driver.Value
	done  bool
}

func (r *fakeStoreRows) Columns() []string { return []string{"value"} }

func (r *fakeStoreRows) Close() error { return nil }

func (r *fakeStoreRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}
