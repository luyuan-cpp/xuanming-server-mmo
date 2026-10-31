//go:build integration

package store_test

// 映射持久化第 1a 批的路由表与 RouteStore 在真实 InnoDB 上的不变量。这几条只有真库能证明,
// 假驱动(route_store_contract_test.go)一条都证不了:
//  1. 迁移建出四张表、主键正确、route_meta / route_reconcile 各恰好种一行;第二次迁移零变更、generation 不换;
//  2. CHECK 约束真的生效(MySQL ≥ 8.0.16),含 G1a 的 chk_route_shard;形状守卫拦得住被改过的存量表与 G1a 之前的旧形状;
//  3. RegisterHome 的各个分支落到库里的样子(含 NULL 绑定、冻结行);
//  4. 并发:同一玩家 32 路并发恰好一个插入;同 zone 多玩家全部成功;
//  5. 锁等待封顶(innodb_lock_wait_timeout=1):持锁者不放时建角最多等约 1s,不会被拖成 50s;
//  6. meta 缺失 / 不可写一律 fail-closed;
//  7. 导入的分类、幂等与 rev 规则;沿聚簇键 (route_shard, player_id) 的键集翻页无漏无重;
//  8. G1a:建池探针 P4 拒绝 chk_route_shard 不生效的库、P5 拒绝按另一个模数建的表;P5 的探针键已被真实玩家占着时
//     撞键照常通过、不改行,被持锁时以 1205 失败、不重试;错片行只有 ScanRoutes 看得见;
//     按键读走 PRIMARY range;可选的真 TiDB 用例(聚簇、P4 / P5 含撞键分支、tidb-prepare 预切与幂等)。
//
// 前提:P4 让所有建 route store 的用例要求 MySQL ≥ 8.0.16(更老的版本上是 FAIL 不是 SKIP)。
//
// 跑法同 schema_integration_test.go(本地 docker 的 appuser 没有建库权限,用 root):
//   DATA_SERVICE_IT_MYSQL_USER=root DATA_SERVICE_IT_MYSQL_PASSWORD=<deploy/docker-compose.yml> \
//     go test -tags=integration ./internal/store/... -run "Route"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/placement"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routeTestOpts 是用例统一的 store 参数(固定写者)。
func routeTestOpts() store.RouteStoreOptions {
	return store.RouteStoreOptions{
		Writer: store.NewRouteWriterID("data_service_it", "localhost"),
	}
}

// newRouteStore 开一个指向一次性库的 RouteStore(探针在这里跑),用例结束自动关。
func newRouteStore(t *testing.T, db *storetest.DB) *store.RouteStore {
	t.Helper()
	st, err := store.NewRouteStore(db.Cfg, routeTestOpts())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newMaintenanceRouteStore 使用真实Operator；普通业务池的权限与boot不改变。
func newMaintenanceRouteStore(t *testing.T, db *storetest.DB, frozen *pb.SqlWriterMaintenanceReceipt) *store.RouteStore {
	t.Helper()
	cfg, err := db.MaintenanceCfg(context.Background())
	require.NoError(t, err)
	opts := routeTestOpts()
	opts.MaintenanceFreeze = frozen
	st, err := store.NewRouteStore(cfg, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// 行为探针只在测试专用Schema池执行；NORMAL启动仅做只读资格核验。
func openRouteBehaviorProbe(ctx context.Context, db *storetest.DB) (*store.RouteStore, error) {
	pool, err := db.Fixture().OpenSchema(ctx, db.Name)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	if _, err = pool.ExecContext(ctx, "SET SESSION lock_wait_timeout=1,innodb_lock_wait_timeout=1"); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return store.NewRouteStoreFromDBForTest(ctx, pool, routeTestOpts())
}

func itNull32(v uint32) any {
	if v == 0 {
		return nil
	}
	return uint64(v)
}

func itNull64(v uint64) any {
	if v == 0 {
		return nil
	}
	return v
}

func itNullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// seedRouteRow 绕过 store 直接造一行(home 为 NULL 的存量行、冻结行这类 W1 自己写不出来的形状)。
// route_shard 按 placement.RouteRowShard 给(聚簇主键首列,chk_route_shard 钉住)。
func seedRouteRow(t *testing.T, db *storetest.DB, player uint64, home, storage uint32, version uint64, frozen string, rev uint64) {
	t.Helper()
	_, err := db.Raw.Exec(
		"INSERT INTO player_route (route_shard, player_id, home_zone, storage_id, placement_version, frozen_run_id, rev, updated_by, created_at_ms, updated_at_ms)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, 'it-seed', 1, 1)",
		uint64(placement.RouteRowShard(player)), player, itNull32(home), itNull32(storage), itNull64(version), itNullStr(frozen), rev)
	require.NoError(t, err)
}

// readRoute 用 store 的只读接口读一行。
func readRoute(t *testing.T, st *store.RouteStore, player uint64) (store.RouteRow, bool) {
	t.Helper()
	got, err := st.GetRoutes(context.Background(), []uint64{player})
	require.NoError(t, err)
	row, ok := got[player]
	return row, ok
}

// mysqlVersionAtLeast 判断 "8.0.36" / "8.0.36-0ubuntu" 这类版本串是否 ≥ major.minor.patch。解析不了按 false。
func mysqlVersionAtLeast(version string, major, minor, patch int) bool {
	head := version
	if i := strings.IndexFunc(version, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }); i >= 0 {
		head = version[:i]
	}
	parts := strings.SplitN(head, ".", 3)
	if len(parts) < 3 {
		return false
	}
	var got [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		got[i] = n
	}
	want := [3]int{major, minor, patch}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

// skipUnlessMySQLEnforcesCheck:TiDB 上 CHECK 要求 ≥ 7.2 且建表前打开 tidb_enable_check_constraint(默认关),
// MySQL < 8.0.16 只解析不执行;这两种库上「CHECK 生效」类用例不适用(TiDB 另有可选用例)。
func skipUnlessMySQLEnforcesCheck(t *testing.T, db *storetest.DB) {
	t.Helper()
	var version string
	require.NoError(t, db.Raw.QueryRow("SELECT VERSION()").Scan(&version))
	if strings.Contains(strings.ToUpper(version), "TIDB") {
		t.Skipf("TiDB %s:CHECK 要求 ≥ 7.2 且 tidb_enable_check_constraint=ON(默认关),本用例按 MySQL 写,不适用"+
			"(TiDB 见 TestRouteStore_TiDBClusteredShardsAndPresplit)", version)
	}
	if !mysqlVersionAtLeast(version, 8, 0, 16) {
		t.Skipf("MySQL %s < 8.0.16:CHECK 只解析不执行,本用例不适用", version)
	}
}

// ── 迁移 ─────────────────────────────────────────────────────────────────────────

func TestRouteSchema_MigrationCreatesTablesSeedsMetaAndIsIdempotent(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, db.Migrate(ctx, store.MigrateOptions{}), "first migration")

	wantPK := map[string][]string{
		store.PlayerRouteTableName:    {"route_shard", "player_id"},
		store.ZoneMergedIntoTableName: {"src_zone"},
		store.RouteMetaTableName:      {"id"},
		store.RouteReconcileTableName: {"id"},
	}
	before := map[string]string{}
	for table, pk := range wantPK {
		require.NotEmpty(t, db.Columns(t, table), "table %s must exist after migration", table)
		got, err := store.PrimaryKeyColumnsForTest(ctx, db.Raw, db.Name, table)
		require.NoError(t, err)
		assert.Equal(t, pk, got, "table %s primary key", table)
		before[table] = db.ShowCreateTable(t, table)
	}

	assert.EqualValues(t, 1, db.Count(t, store.RouteMetaTableName, ""), "route_meta 必须恰好一行")
	var generation string
	var writable int
	require.NoError(t, db.Raw.QueryRow("SELECT generation, writable FROM route_meta WHERE id = 1").Scan(&generation, &writable))
	assert.Regexp(t, "^[0-9a-f]{32}$", generation, "generation 是 16 字节随机数的十六进制")
	assert.Equal(t, 1, writable)

	// G1a 拆表:常写的水位 / 租约只在 route_reconcile,开关表 route_meta 里没有它们。
	for _, col := range []string{"watermark_ms", "cache_verified_ms", "leader", "lease_until_ms"} {
		assert.Falsef(t, db.Columns(t, store.RouteMetaTableName)[col], "route_meta 不该再有 %s", col)
	}
	assert.EqualValues(t, 1, db.Count(t, store.RouteReconcileTableName, ""), "route_reconcile 必须恰好一行")
	var watermark, verified uint64
	var leader sql.NullString
	require.NoError(t, db.Raw.QueryRow("SELECT watermark_ms, cache_verified_ms, leader FROM route_reconcile WHERE id = 1").
		Scan(&watermark, &verified, &leader))
	assert.Zero(t, watermark)
	assert.Zero(t, verified)
	assert.False(t, leader.Valid, "种出来的对账行没有 leader")

	require.NoError(t, db.Migrate(ctx, store.MigrateOptions{}), "second migration must be a no-op")
	for table := range wantPK {
		assert.Equalf(t, before[table], db.ShowCreateTable(t, table), "table %s DDL changed on second migration", table)
	}
	var again string
	require.NoError(t, db.Raw.QueryRow("SELECT generation FROM route_meta WHERE id = 1").Scan(&again))
	assert.Equal(t, generation, again, "二次迁移不许换 generation(它是库世代,只有回档恢复流程才换)")
	assert.EqualValues(t, 1, db.Count(t, store.RouteMetaTableName, ""))
	assert.EqualValues(t, 1, db.Count(t, store.RouteReconcileTableName, ""))
}

func TestRouteSchema_CheckConstraintsEnforced(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	skipUnlessMySQLEnforcesCheck(t, db)

	// 每条违例 INSERT 都带**正确的** route_shard(这些 id < 64,片 = id):否则先撞 1364 或 chk_route_shard,
	// 用例就不再证它想证的那一条约束。
	for _, tc := range []struct {
		name string
		stmt string
		// check 非空时要求报错点名这条约束(只对「只违反这一条」的语句有意义)。
		check string
	}{
		{"home为0", "INSERT INTO player_route (route_shard, player_id, home_zone, rev, created_at_ms, updated_at_ms) VALUES (1, 1, 0, 1, 1, 1)", ""},
		{"home越界", "INSERT INTO player_route (route_shard, player_id, home_zone, rev, created_at_ms, updated_at_ms) VALUES (2, 2, 1000000, 1, 1, 1)", ""},
		{"有storage没有version", "INSERT INTO player_route (route_shard, player_id, storage_id, rev, created_at_ms, updated_at_ms) VALUES (3, 3, 7, 1, 1, 1)", ""},
		{"version为0", "INSERT INTO player_route (route_shard, player_id, storage_id, placement_version, rev, created_at_ms, updated_at_ms) VALUES (4, 4, 7, 0, 1, 1, 1)", ""},
		{"冻结却没有storage", "INSERT INTO player_route (route_shard, player_id, frozen_run_id, rev, created_at_ms, updated_at_ms) VALUES (5, 5, 'run', 1, 1, 1)", ""},
		{"rev为0", "INSERT INTO player_route (route_shard, player_id, home_zone, rev, created_at_ms, updated_at_ms) VALUES (6, 6, 7, 0, 1, 1)", ""},
		// G1a:player_id 的唯一性靠它(主键是 (route_shard, player_id)),1 % 64 = 1 ≠ 2。
		{"route_shard与player_id不符", "INSERT INTO player_route (route_shard, player_id, home_zone, rev, created_at_ms, updated_at_ms) VALUES (2, 1, 7, 1, 1, 1)", "chk_route_shard"},
	} {
		_, err := db.Raw.Exec(tc.stmt)
		require.Errorf(t, err, "%s: CHECK 没有拦住", tc.name)
		assert.Truef(t, isMySQLErrNumber(err, 3819), "%s: want 3819 ER_CHECK_CONSTRAINT_VIOLATED, got %v", tc.name, err)
		if tc.check != "" {
			assert.Containsf(t, err.Error(), tc.check, "%s: 报错没有点名 %s", tc.name, tc.check)
		}
	}
	assert.EqualValues(t, 0, db.Count(t, store.PlayerRouteTableName, ""))
}

func TestRouteSchema_ShapeGuardRejectsTamperedTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Run("丢列", func(t *testing.T) {
		db := storetest.NewMigratedDB(t)
		// 不能删 rev:它被 chk_route_rev 引用,MySQL 会以 3959 拒绝删列。
		_, err := db.Raw.Exec("ALTER TABLE player_route DROP COLUMN updated_by")
		require.NoError(t, err)
		err = db.Migrate(ctx, store.MigrateOptions{})
		require.Error(t, err, "形状不对的存量表必须让迁移失败")
		assert.Contains(t, err.Error(), "updated_by")
	})

	t.Run("换主键", func(t *testing.T) {
		db := storetest.NewMigratedDB(t)
		_, err := db.Raw.Exec("ALTER TABLE zone_merged_into DROP PRIMARY KEY, ADD PRIMARY KEY (src_zone, dst_zone)")
		require.NoError(t, err)
		err = db.Migrate(ctx, store.MigrateOptions{})
		require.Error(t, err, "主键被改过的存量表必须让迁移失败(ODKU 的判定建在主键上)")
		assert.Contains(t, err.Error(), "primary key")
	})

	t.Run("player_route主键退回player_id", func(t *testing.T) {
		db := storetest.NewMigratedDB(t)
		_, err := db.Raw.Exec("ALTER TABLE player_route DROP PRIMARY KEY, ADD PRIMARY KEY (player_id)")
		require.NoError(t, err)
		err = db.Migrate(ctx, store.MigrateOptions{})
		require.Error(t, err, "player_route 主键不是 (route_shard, player_id) 时迁移必须失败")
		assert.Contains(t, err.Error(), "primary key")
		assert.Contains(t, err.Error(), "G1a", "主键分支也要带上 G1a 的处理提示")
	})

	// 评审 M2:真实的旧表**没有 route_shard 列**,守卫先查列,提示若只挂在主键分支就永远走不到。
	// 旧 DDL 原文写成字面量(不引用生产常量:生产常量已经是新形状)。
	t.Run("G1a之前的真实旧形状", func(t *testing.T) {
		db := storetest.NewMigratedDB(t)
		_, err := db.Raw.Exec("DROP TABLE player_route")
		require.NoError(t, err)
		_, err = db.Raw.Exec(preG1aPlayerRouteDDL)
		require.NoError(t, err)
		err = db.Migrate(ctx, store.MigrateOptions{})
		require.Error(t, err, "G1a 之前的旧形状必须让迁移失败,且绝不自动 DROP")
		assert.Contains(t, err.Error(), "route_shard")
		assert.Contains(t, err.Error(), "DROP TABLE player_route, zone_merged_into, route_meta",
			"提示必须要求 DROP 全部路由表(只 DROP player_route 会留下守卫查不出的混合态)")
		cols := db.Columns(t, store.PlayerRouteTableName)
		assert.True(t, cols["player_id"] && !cols["route_shard"], "迁移不许自己 DROP / 改写旧表:旧形状必须原样留着,等人处理")
	})

	t.Run("丢route_reconcile的列", func(t *testing.T) {
		db := storetest.NewMigratedDB(t)
		_, err := db.Raw.Exec("ALTER TABLE route_reconcile DROP COLUMN cache_verified_ms")
		require.NoError(t, err)
		err = db.Migrate(ctx, store.MigrateOptions{})
		require.Error(t, err, "route_reconcile 缺列必须让迁移失败")
		assert.Contains(t, err.Error(), "cache_verified_ms")
	})
}

// preG1aPlayerRouteDDL 是 G1a 之前第 1a 批 player_route 的 DDL 原文(主键 player_id NONCLUSTERED、没有 route_shard)。
// 只给「旧形状必须被形状守卫拒绝」的用例造表用。
const preG1aPlayerRouteDDL = "CREATE TABLE IF NOT EXISTS `player_route` (\n" +
	"  `player_id` bigint unsigned NOT NULL COMMENT 'pb:1',\n" +
	"  `home_zone` int unsigned NULL COMMENT 'pb:2',\n" +
	"  `storage_id` int unsigned NULL COMMENT 'pb:3',\n" +
	"  `placement_version` bigint unsigned NULL COMMENT 'pb:4',\n" +
	"  `frozen_run_id` varchar(64) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'pb:5',\n" +
	"  `rev` bigint unsigned NOT NULL COMMENT 'pb:6',\n" +
	"  `updated_by` varchar(96) CHARACTER SET ascii NOT NULL DEFAULT '' COMMENT 'pb:7',\n" +
	"  `created_at_ms` bigint unsigned NOT NULL COMMENT 'pb:8',\n" +
	"  `updated_at_ms` bigint unsigned NOT NULL COMMENT 'pb:9',\n" +
	"  PRIMARY KEY (`player_id`) /*T![clustered_index] NONCLUSTERED */,\n" +
	"  KEY `idx_route_home` (`home_zone`, `player_id`),\n" +
	"  KEY `idx_route_storage` (`storage_id`, `player_id`),\n" +
	"  KEY `idx_route_updated` (`updated_at_ms`),\n" +
	"  CONSTRAINT `chk_route_home` CHECK (`home_zone` IS NULL OR `home_zone` BETWEEN 1 AND 999999),\n" +
	"  CONSTRAINT `chk_route_place` CHECK ((`storage_id` IS NULL) = (`placement_version` IS NULL)),\n" +
	"  CONSTRAINT `chk_route_ver` CHECK (`placement_version` IS NULL OR `placement_version` >= 1),\n" +
	"  CONSTRAINT `chk_route_frozen` CHECK (`frozen_run_id` IS NULL OR `storage_id` IS NOT NULL),\n" +
	"  CONSTRAINT `chk_route_rev` CHECK (`rev` >= 1)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin" +
	" /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */ COMMENT='player_route';"

// ── W1 ──────────────────────────────────────────────────────────────────────────

func TestRouteStore_RegisterHomeBranches(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()
	const home uint32 = 7
	const storage uint32 = 1000000

	// 插入
	res, err := st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1001, HomeZone: home, StorageID: storage})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterInserted, res.Outcome)
	assert.Equal(t, uint64(1), res.Row.Rev)
	row, ok := readRoute(t, st, 1001)
	require.True(t, ok)
	assert.Equal(t, home, row.HomeZone)
	assert.Equal(t, storage, row.StorageID)
	assert.Equal(t, uint64(1), row.PlacementVersion)
	assert.Equal(t, routeTestOpts().Writer, row.UpdatedBy)
	assert.Positive(t, row.CreatedAtMs, "时间戳取库时钟")
	assert.Equal(t, row.CreatedAtMs, row.UpdatedAtMs)

	// 同值:幂等,不写
	res, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1001, HomeZone: home, StorageID: storage})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterAlreadySame, res.Outcome)
	assert.Equal(t, row, res.Row, "AlreadySame 回的是加锁读读到的整行")

	// ExistingRowOnly 同值:同样幂等
	res, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1001, HomeZone: home, ExistingRowOnly: true})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterAlreadySame, res.Outcome)

	// 冲突:不写,带出既有值
	res, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1001, HomeZone: 8})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterConflict, res.Outcome)
	assert.Equal(t, home, res.ExistingHome)
	after, _ := readRoute(t, st, 1001)
	assert.Equal(t, row, after, "冲突不许改行")

	// home 为 NULL、落点为 NULL、请求带 storage:补 home + 落点
	seedRouteRow(t, db, 1002, 0, 0, 0, "", 3)
	res, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1002, HomeZone: home, StorageID: storage})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterFilledHome, res.Outcome)
	assert.Equal(t, uint64(4), res.Row.Rev)
	row, _ = readRoute(t, st, 1002)
	assert.Equal(t, home, row.HomeZone)
	assert.Equal(t, storage, row.StorageID)
	assert.Equal(t, uint64(1), row.PlacementVersion)
	assert.Equal(t, uint64(4), row.Rev)

	// home 为 NULL、请求不带 storage:只补 home
	seedRouteRow(t, db, 1003, 0, 0, 0, "", 1)
	res, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1003, HomeZone: home})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterFilledHome, res.Outcome)
	row, _ = readRoute(t, st, 1003)
	assert.Equal(t, home, row.HomeZone)
	assert.False(t, row.HasPlacement())
	assert.Equal(t, uint64(2), row.Rev)

	// home 为 NULL、已有落点、请求带 storage:补 home,落点保留
	seedRouteRow(t, db, 1004, 0, 55, 3, "", 2)
	res, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1004, HomeZone: home, StorageID: storage})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterFilledHomePlacementKept, res.Outcome)
	row, _ = readRoute(t, st, 1004)
	assert.Equal(t, uint32(55), row.StorageID, "已有落点指向数据真正所在的库,不许被请求盖掉")
	assert.Equal(t, uint64(3), row.PlacementVersion)
	assert.Equal(t, uint64(3), row.Rev)

	// 冻结行:补 home 时冻结态原样留着,回给发布的行里也是冻结态
	seedRouteRow(t, db, 1005, 0, 55, 3, "run-x", 2)
	res, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1005, HomeZone: home, StorageID: storage})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterFilledHomePlacementKept, res.Outcome)
	plc, err := res.Row.PlacementValue()
	require.NoError(t, err)
	assert.Equal(t, placement.FrozenValue(55, 3, "run-x"), plc)
	res, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 1005, HomeZone: home, ExistingRowOnly: true})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterAlreadySame, res.Outcome)
	assert.Equal(t, "run-x", res.Row.FrozenRunID, "重读冻结行必须带回 run_id")
}

func TestRouteStore_NullPlacementBindsNull(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()

	res, err := st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 2001, HomeZone: 7})
	require.NoError(t, err)
	require.Equal(t, store.RouteRegisterInserted, res.Outcome)
	var storageNull, versionNull, frozenNull bool
	require.NoError(t, db.Raw.QueryRow(
		"SELECT storage_id IS NULL, placement_version IS NULL, frozen_run_id IS NULL FROM player_route WHERE route_shard = ? AND player_id = ?",
		uint64(placement.RouteRowShard(2001)), 2001).
		Scan(&storageNull, &versionNull, &frozenNull))
	assert.True(t, storageNull && versionNull && frozenNull, "不带落点时三列都必须是 NULL,不是 0")

	_, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 2002, HomeZone: 7, StorageID: 1000000})
	require.NoError(t, err)
	var storageID, version uint64
	require.NoError(t, db.Raw.QueryRow("SELECT storage_id, placement_version FROM player_route WHERE route_shard = ? AND player_id = ?",
		uint64(placement.RouteRowShard(2002)), 2002).
		Scan(&storageID, &version))
	assert.Equal(t, uint64(1000000), storageID)
	assert.Equal(t, uint64(1), version)
}

// registerOutcome 是一次并发 RegisterHome 的结果。
type registerOutcome struct {
	res store.RouteRegisterResult
	err error
}

// registerConcurrently 让 reqs 同时起跑(一道闸门放行),收齐结果。
func registerConcurrently(t *testing.T, st *store.RouteStore, reqs []store.RouteRegisterRequest) []registerOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out := make([]registerOutcome, len(reqs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := st.RegisterHome(ctx, req)
			out[i] = registerOutcome{res: res, err: err}
		}()
	}
	close(start)
	wg.Wait()
	return out
}

func TestRouteStore_ConcurrentSamePlayer(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	const workers = 32

	t.Run("同home_恰好一个插入其余幂等", func(t *testing.T) {
		reqs := make([]store.RouteRegisterRequest, workers)
		for i := range reqs {
			reqs[i] = store.RouteRegisterRequest{PlayerID: 3001, HomeZone: 7, StorageID: 1000000}
		}
		var inserted, same int
		for i, o := range registerConcurrently(t, st, reqs) {
			require.NoErrorf(t, o.err, "worker %d", i)
			switch o.res.Outcome {
			case store.RouteRegisterInserted:
				inserted++
			case store.RouteRegisterAlreadySame:
				same++
			default:
				t.Fatalf("worker %d: unexpected outcome %s", i, o.res.Outcome)
			}
		}
		assert.Equal(t, 1, inserted)
		assert.Equal(t, workers-1, same)
	})

	t.Run("不同home_恰好一个胜者其余冲突", func(t *testing.T) {
		reqs := make([]store.RouteRegisterRequest, workers)
		for i := range reqs {
			reqs[i] = store.RouteRegisterRequest{PlayerID: 3002, HomeZone: uint32(10 + i)}
		}
		outcomes := registerConcurrently(t, st, reqs)
		winner := uint32(0)
		for i, o := range outcomes {
			require.NoErrorf(t, o.err, "worker %d", i)
			if o.res.Outcome == store.RouteRegisterInserted {
				require.Zerof(t, winner, "两个胜者:%d 与 %d", winner, reqs[i].HomeZone)
				winner = reqs[i].HomeZone
			}
		}
		require.NotZero(t, winner, "没有胜者")
		for i, o := range outcomes {
			if o.res.Outcome == store.RouteRegisterInserted {
				continue
			}
			require.Equalf(t, store.RouteRegisterConflict, o.res.Outcome, "worker %d", i)
			assert.Equalf(t, winner, o.res.ExistingHome, "worker %d 的 Existing 必须是胜者的 home", i)
		}
		row, ok := readRoute(t, st, 3002)
		require.True(t, ok)
		assert.Equal(t, winner, row.HomeZone)
	})
	t.Logf("lockRetries=%d(1213/9007 重来次数,只记录不断言)", st.LockRetriesForTest())
}

func TestRouteStore_ConcurrentManyPlayersSameZone(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	const players = 64
	reqs := make([]store.RouteRegisterRequest, players)
	for i := range reqs {
		reqs[i] = store.RouteRegisterRequest{PlayerID: uint64(4100 + i), HomeZone: 7}
	}
	for i, o := range registerConcurrently(t, st, reqs) {
		require.NoErrorf(t, o.err, "player %d", reqs[i].PlayerID)
		assert.Equalf(t, store.RouteRegisterInserted, o.res.Outcome, "player %d", reqs[i].PlayerID)
	}
	assert.EqualValues(t, players, db.Count(t, store.PlayerRouteTableName, "home_zone = 7"))
	t.Logf("lockRetries=%d(同 zone %d 名玩家;只记录不断言)", st.LockRetriesForTest(), players)
}

// TestRouteStore_LockWaitBounded(规格 M1):持锁者不放时,同一玩家的 RegisterHome 最多等约 1s 就以 1205 失败,
// 不会被默认 50s 的锁等待拖住(ctx 到期时驱动只关连接,服务端那个等锁的线程同样最多再占 1s)。
func TestRouteStore_LockWaitBounded(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const zone uint32 = 7
	const p uint64 = 4001
	_, err := st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: p, HomeZone: zone})
	require.NoError(t, err)

	// 持锁者:原始连接开事务,用与生产同一段文本把 p 的行锁住,并一直持有到用例结束。
	holder, err := db.Raw.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	row := holder.QueryRowContext(ctx, store.RouteLockRowSQLForTest, uint64(placement.RouteRowShard(p)), p)
	var home, storage sql.NullInt64
	var version sql.NullInt64
	var frozen sql.NullString
	var rev, created, updated uint64
	var writer string
	require.NoError(t, row.Scan(&home, &storage, &version, &frozen, &rev, &writer, &created, &updated))

	start := time.Now()
	qctx, qcancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = st.RegisterHome(qctx, store.RouteRegisterRequest{PlayerID: p, HomeZone: zone})
	qcancel()
	elapsed := time.Since(start)
	var me *mysql.MySQLError
	require.Truef(t, errors.As(err, &me) && me.Number == 1205, "want ER_LOCK_WAIT_TIMEOUT, got %v", err)
	assert.Lessf(t, elapsed, 2*time.Second, "等了 %v:锁等待没有被 innodb_lock_wait_timeout=1 封顶", elapsed)
}

func TestRouteStore_MetaMissingAndNotWritableFailClosed(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()
	req := store.RouteRegisterRequest{PlayerID: 7101, HomeZone: 7}

	originalMeta, err := db.Fixture().Catalog.CurrentMeta(ctx)
	require.NoError(t, err)

	_, err = db.Raw.Exec("UPDATE route_meta SET writable = 0 WHERE id = 1")
	require.NoError(t, err)
	_, err = st.RegisterHome(ctx, req)
	require.ErrorIs(t, err, authoritysql.ErrWriterDenied)

	_, err = db.Raw.Exec("DELETE FROM route_meta WHERE id = 1")
	require.NoError(t, err)
	_, err = st.RegisterHome(ctx, req)
	require.ErrorIs(t, err, authoritysql.ErrWriterDenied)
	assert.EqualValues(t, 0, db.Count(t, store.PlayerRouteTableName, ""), "meta 缺失 / 不可写时零写入")

	_, err = store.NewRouteStore(db.Cfg, routeTestOpts())
	require.ErrorIs(t, err, store.ErrRouteMetaMissing, "建 store 的探针同样拒绝")

	// 故障注入恢复本用例保存的真实原身份；不能用新 generation 让旧 boot 复活。
	_, err = db.Raw.Exec("INSERT INTO route_meta (id,generation,writable,writer_epoch,catalog_revision,freeze_run_id) VALUES (1,?,?,?,?,?)",
		originalMeta.Generation, originalMeta.Writable, originalMeta.WriterEpoch, originalMeta.CatalogRevision, originalMeta.FreezeRunId)
	require.NoError(t, err)
	migrateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	require.NoError(t, db.Migrate(migrateCtx, store.MigrateOptions{}))
	restoredMeta, err := db.Fixture().Catalog.CurrentMeta(ctx)
	require.NoError(t, err)
	assert.True(t, proto.Equal(originalMeta, restoredMeta), "幂等迁移保留原 generation / epoch / catalog / freeze 身份")
	res, err := st.RegisterHome(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterInserted, res.Outcome)
}

func TestRouteStore_ExistingRowOnlyNeverInserts(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	res, err := st.RegisterHome(context.Background(),
		store.RouteRegisterRequest{PlayerID: 7001, HomeZone: 7, StorageID: 1000000, ExistingRowOnly: true})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterRowMissing, res.Outcome)
	assert.EqualValues(t, 0, db.Count(t, store.PlayerRouteTableName, ""), "缓存认得、库里没有:绝不新插")
}

// TestRouteStore_ProbeRejectsPoolWithoutSessionVariables:不经 buildRouteDSN 的池(服务端默认 RR + 50s)必须被探针拒绝。
func TestRouteStore_ProbeRejectsPoolWithoutSessionVariables(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	plain, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true", db.Cfg.User, db.Cfg.Password, db.Cfg.Host, db.Name))
	require.NoError(t, err)
	t.Cleanup(func() { _ = plain.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := store.NewRouteStoreFromDBForTest(ctx, plain, routeTestOpts())
	if err == nil {
		_ = st.Close()
		t.Skip("服务端全局已是 READ-COMMITTED + innodb_lock_wait_timeout=1,本用例证不了探针(不代表通过)")
	}
	require.ErrorIs(t, err, store.ErrRouteSessionMisconfigured)
}

// ── 导入与只读接口 ───────────────────────────────────────────────────────────────

func TestRouteStore_ImportClassifiesAndIsIdempotent(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()

	_, err := st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 8001, HomeZone: 7})
	require.NoError(t, err)
	seedRouteRow(t, db, 8002, 0, 0, 0, "", 1)
	_, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 8003, HomeZone: 8})
	require.NoError(t, err)
	seedRouteRow(t, db, 8004, 7, 0, 0, "", 2)

	rows := []store.RouteImportRow{
		{PlayerID: 8000, HomeZone: 7, Placement: &placement.Record{StorageID: 1000000, Version: 1}, MapRev: 5}, // 新行,rev=maprev
		{PlayerID: 8001, HomeZone: 7},            // match
		{PlayerID: 8002, HomeZone: 7},            // fill
		{PlayerID: 8003, HomeZone: 7},            // conflict(库是 8)
		{PlayerID: 8004, HomeZone: 7, MapRev: 5}, // rev_regression(库 rev=2)
	}
	ids := []uint64{8000, 8001, 8002, 8003, 8004}
	before, err := st.GetRoutes(ctx, ids)
	require.NoError(t, err)

	// dry-run:统计与真跑相同,零写入
	stats, err := st.ImportRoutes(ctx, rows, true)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Inserted)
	assert.Equal(t, 1, stats.Matched)
	assert.Equal(t, 1, stats.Filled)
	require.Len(t, stats.Conflicts, 1)
	require.Len(t, stats.RevRegressions, 1)
	afterDry, err := st.GetRoutes(ctx, ids)
	require.NoError(t, err)
	assert.Equal(t, before, afterDry, "dry-run 必须零写入")

	// 真跑
	stats, err = st.ImportRoutes(ctx, rows, false)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Inserted)
	assert.Equal(t, 1, stats.Filled)
	assert.Equal(t, uint64(8003), stats.Conflicts[0].PlayerID)
	assert.Equal(t, uint64(8004), stats.RevRegressions[0].PlayerID)

	after, err := st.GetRoutes(ctx, ids)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), after[8000].Rev, "新行的 rev 取 maprev")
	assert.Equal(t, uint32(1000000), after[8000].StorageID)
	assert.Equal(t, uint32(7), after[8002].HomeZone)
	assert.Equal(t, uint64(2), after[8002].Rev, "补列 rev+1")
	assert.Equal(t, before[8003], after[8003], "冲突行不许改")
	assert.Equal(t, before[8004], after[8004], "rev 倒退的行不许改")

	// 第二次:写入为 0
	stats, err = st.ImportRoutes(ctx, rows, false)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Inserted)
	assert.Equal(t, 0, stats.Filled)
	assert.Equal(t, 3, stats.Matched)
	again, err := st.GetRoutes(ctx, ids)
	require.NoError(t, err)
	assert.Equal(t, after, again, "二次导入零变更")

	// merged_into:插入 → 幂等 → 冲突不覆盖
	mi := []store.MergedIntoRow{{SrcZone: 101, DstZone: 102, RunID: "101-102-20261012T000000Z-abcd1234"}}
	miStats, err := st.ImportMergedInto(ctx, mi, false)
	require.NoError(t, err)
	assert.Equal(t, 1, miStats.Inserted)
	miStats, err = st.ImportMergedInto(ctx, mi, false)
	require.NoError(t, err)
	assert.Equal(t, 1, miStats.Matched)
	miStats, err = st.ImportMergedInto(ctx, []store.MergedIntoRow{{SrcZone: 101, DstZone: 103, RunID: "101-103-x"}}, false)
	require.NoError(t, err)
	require.Len(t, miStats.Conflicts, 1)
	listed, err := st.ListMergedInto(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, uint32(102), listed[0].DstZone, "冲突不覆盖")
	assert.Positive(t, listed[0].MergedAtMs, "merged_at_ms 取导入时刻的库时钟")
}

func TestRouteStore_ScanRoutesKeysetCoversAll(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()
	const total = 2500
	const first uint64 = 100001

	for lo := 0; lo < total; lo += store.RouteBatchLimit {
		hi := lo + store.RouteBatchLimit
		if hi > total {
			hi = total
		}
		batch := make([]store.RouteImportRow, 0, hi-lo)
		for i := lo; i < hi; i++ {
			batch = append(batch, store.RouteImportRow{PlayerID: first + uint64(i), HomeZone: 7})
		}
		stats, err := st.ImportRoutes(ctx, batch, false)
		require.NoError(t, err)
		require.Equal(t, len(batch), stats.Inserted)
	}

	seen := make(map[uint64]bool, total)
	shardsSeen := make(map[uint32]bool)
	var ordered []store.RouteScanCursor
	var after store.RouteScanCursor
	for {
		page, err := st.ScanRoutes(ctx, after, 100)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			require.Falsef(t, seen[r.PlayerID], "player %d 出现了两次", r.PlayerID)
			seen[r.PlayerID] = true
			key := store.RouteScanCursorAt(r)
			shardsSeen[key.Shard] = true
			ordered = append(ordered, key)
		}
		after = store.RouteScanCursorAt(page[len(page)-1])
	}
	assert.Len(t, ordered, total, "翻页漏了行")
	assert.True(t, sort.SliceIsSorted(ordered, func(i, j int) bool { return ordered[i].Less(ordered[j]) }),
		"必须按聚簇键 (route_shard, player_id) 升序")
	assert.Len(t, shardsSeen, int(placement.RouteRowShards), "2500 个连续 id 必然覆盖全部 64 个行片")
}

// ── G1a:建池探针、错片行、按键读的执行计划、真 TiDB ───────────────────────────────

// TestRouteStore_ProbeRejectsUnenforcedShardCheck:chk_route_shard 被关掉(NOT ENFORCED)的库,建 store 的探针 P4
// 必须拒绝(player_id 的唯一性靠它);探针那一行必须已回滚。
func TestRouteStore_ProbeRejectsUnenforcedShardCheck(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	skipUnlessMySQLEnforcesCheck(t, db)

	_, err := db.Raw.Exec("ALTER TABLE player_route ALTER CHECK chk_route_shard NOT ENFORCED")
	require.NoError(t, err)
	_, err = store.NewRouteStore(db.Cfg, routeTestOpts())
	require.ErrorIs(t, err, store.ErrRouteCheckNotEnforced)
	assert.EqualValues(t, 0, db.Count(t, store.PlayerRouteTableName, "player_id = 0"), "探针行必须已回滚")

	// 恢复之后探针放行(表里没有违例行,ENFORCED 能加回来)。
	_, err = db.Raw.Exec("ALTER TABLE player_route ALTER CHECK chk_route_shard ENFORCED")
	require.NoError(t, err)
	newRouteStore(t, db)
}

// TestRouteStore_ProbeRejectsTableBuiltWithAnotherShardModulus:按另一个模数建的 player_route(手工把建表语句里
// chk_route_shard 的模数换成 16 / 128 重建,模拟跑过 d5cbb8a8e 迁移的库)过得了迁移的形状守卫与 P4,建池时的正向
// 探针 P5 必须认出来 → ErrRouteShardModulusMismatch,探针行必须已回滚。两个模数各撞一行:16 拒 (63, 63)
// (63 % 16 = 15),128 拒 (0, 64)(64 % 128 = 64)。最后换回按本二进制模数建的表,建池通过(对照:正确的库上不误报)。
func TestRouteStore_ProbeRejectsTableBuiltWithAnotherShardModulus(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	skipUnlessMySQLEnforcesCheck(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	modulusExpr := func(modulus uint32) string {
		return "`player_id` % " + strconv.FormatUint(uint64(modulus), 10) + ")"
	}
	current := modulusExpr(placement.RouteRowShards)
	require.Equalf(t, 1, strings.Count(store.PlayerRouteBootstrapDDLForTest, current),
		"建表语句里找不到唯一一处 %q:用例没法按别的模数重建", current)
	rebuild := func(t *testing.T, modulus uint32) {
		t.Helper()
		_, err := db.Raw.ExecContext(ctx, "DROP TABLE player_route")
		require.NoError(t, err)
		_, err = db.Raw.ExecContext(ctx, strings.Replace(store.PlayerRouteBootstrapDDLForTest, current, modulusExpr(modulus), 1))
		require.NoError(t, err)
	}

	for _, modulus := range []uint32{16, 128} {
		rebuild(t, modulus)
		// 迁移照样通过(CREATE TABLE IF NOT EXISTS 不碰已有的表,形状守卫只查列名与主键):这正是要靠探针认出来的原因。
		require.NoErrorf(t, db.Migrate(ctx, store.MigrateOptions{}), "按 %% %d 建的表,迁移不该报错", modulus)
		_, err := store.NewRouteStore(db.Cfg, routeTestOpts())
		require.ErrorIsf(t, err, store.ErrRouteShardModulusMismatch, "按 %% %d 建的表必须被建池探针 P5 认出", modulus)
		assert.EqualValuesf(t, 0, db.Count(t, store.PlayerRouteTableName, ""), "按 %% %d 建的表:探针行必须已回滚", modulus)
	}

	// 生产常态是 63 / 64 号已建角、P5 撞键(1062 按「已接受」处理后同一事务继续),旧模数的表照样要认出来:
	// 按 % 16 建并 seed 64 号((0, 64) 在 % 16 下合法)→ (0, 64) 撞键被接受、(63, 63) 被 chk_route_shard 拒 → 模数哨兵;
	// seed 的那一行原样留着,探针什么也没留下。
	rebuild(t, 16)
	seedRouteRow(t, db, uint64(placement.RouteRowShards), 7, 0, 0, "", 1)
	_, err := store.NewRouteStore(db.Cfg, routeTestOpts())
	require.ErrorIs(t, err, store.ErrRouteShardModulusMismatch, "首行撞键之后第二行照常判定,按模数 16 建的表仍要被认出")
	assert.EqualValues(t, 1, db.Count(t, store.PlayerRouteTableName, ""), "只剩 seed 的那一行")
	assert.EqualValues(t, 1, db.Count(t, store.PlayerRouteTableName,
		"route_shard = 0 AND player_id = ? AND rev = 1 AND updated_by = 'it-seed' AND updated_at_ms = 1", uint64(placement.RouteRowShards)),
		"seed 的那一行不许被探针改")

	rebuild(t, placement.RouteRowShards)
	newRouteStore(t, db)
	assert.EqualValues(t, 0, db.Count(t, store.PlayerRouteTableName, ""), "模数一致时 P5 的两行真的插进去过,必须已回滚")
}

// TestRouteStore_ProbeKeysHeldByRealPlayers:生产上 P5 的常态 —— 号段从 1 发号,63 / 64 号是开服后最早的真实玩家,
// P5 的两行 (0, 64)、(63, 63) 都已有行。假驱动只钉了「回 1062 → 继续」,这里在真 InnoDB 上取证:
//
//	① 两行都已存在时建池通过(撞键之后只回滚那一条语句、事务仍可用,第二行照常判定),且这两行一个字节都不改;
//	② 64 号玩家的行被别的事务 FOR UPDATE 持着时,P5 撞键要取的共享锁等满会话锁等待(1s)后以 1205 失败:
//	   原样包装(不是模数哨兵)、不重试(提案 §11.2「正向探针 P5」:这次建池失败,重启即可);放锁之后建池通过。
func TestRouteStore_ProbeKeysHeldByRealPlayers(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	skipUnlessMySQLEnforcesCheck(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	n := uint64(placement.RouteRowShards)
	probeIDs := []uint64{n, n - 1} // 与 routeShardModulusProbeKeys 同:(0, 64)、(63, 63)
	for _, id := range probeIDs {
		seedRouteRow(t, db, id, 7, 0, 0, "", 1)
	}
	requireSeedUntouched := func(t *testing.T) {
		t.Helper()
		require.EqualValues(t, len(probeIDs), db.Count(t, store.PlayerRouteTableName, ""), "探针撞键不许多出行")
		for _, id := range probeIDs {
			require.EqualValuesf(t, 1, db.Count(t, store.PlayerRouteTableName,
				"route_shard = ? AND player_id = ? AND home_zone = 7 AND rev = 1 AND updated_by = 'it-seed' AND updated_at_ms = 1",
				uint64(placement.RouteRowShard(id)), id), "player %d 被探针改了", id)
		}
	}

	// ① 两行都撞键:建池通过,行不变。
	newRouteStore(t, db)
	requireSeedUntouched(t)

	// ② 另一个事务持着 (0, 64) 的行锁(RC + 主键等值 FOR UPDATE,只锁这一行)。
	holder, err := db.Raw.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	var locked uint64
	require.NoError(t, holder.QueryRowContext(ctx,
		"SELECT player_id FROM player_route WHERE route_shard = ? AND player_id = ? FOR UPDATE",
		uint64(placement.RouteRowShard(n)), n).Scan(&locked))

	normal, err := store.NewRouteStore(db.Cfg, routeTestOpts())
	require.NoError(t, err, "NORMAL只读启动不应等待玩家行锁")
	require.NoError(t, normal.Close())
	start := time.Now()
	_, err = openRouteBehaviorProbe(ctx, db)
	elapsed := time.Since(start)
	t.Logf("建池撞上被持锁的探针键:耗时 %s,err=%v", elapsed, err)
	var me *mysql.MySQLError
	require.Truef(t, errors.As(err, &me) && me.Number == 1205, "期望原样包装的 1205,got %v", err)
	assert.NotErrorIs(t, err, store.ErrRouteShardModulusMismatch, "锁等待超时不是模数不一致")
	// 会话锁等待 1s、1205 不重试;上限放宽到 4s(锁等待超时的检测有粒度),仍明显低于探针总时限 5s。
	assert.Less(t, elapsed, 4*time.Second, "1205 被重试了,或会话锁等待没生效")

	require.NoError(t, holder.Rollback())
	newRouteStore(t, db)
	requireSeedUntouched(t)
}

// TestRouteStore_ProbesQueueOnMetaGuard(2026-10-16,R4):建池探针先锁 route_meta(id=1) 这行已提交的守卫,
// 才去插必然回滚的探针行。于是并发探针在守卫上排成一列,不会两个以上排在别人必然回滚的探针行后面 —— 那是
// 事故文档 docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md §9.1 现场二(先到者回滚,任何写法 10/10 成环;
// 2026-10-16 在本表 RC 下复现 10/10,加守卫后 0/10)。
//
// 编排:holder 用**同一段生产文本**按探针的顺序持守卫、插下 P5 的第一行 (0, N) 不提交,扮演正在探针的另一个副本;
// 这时建池必须排在 route_meta 上,player_route 上一个等待者都没有。holder 回滚后建池通过、表里不留探针行。
// 只放一个建池者:探针会话的锁等待封顶 1s,多个等待者在慢机器上会让编排本身抖;「守卫先于插入」成立,
// 就不可能有两个后到者排在一条未提交的探针行后面。
func TestRouteStore_ProbesQueueOnMetaGuard(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	skipUnlessMySQLEnforcesCheck(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	holder, err := db.Raw.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	var writable uint64
	require.NoError(t, holder.QueryRowContext(ctx, store.RouteProbeGuardSQLForTest).Scan(&writable))
	n := uint64(placement.RouteRowShards)
	_, err = holder.ExecContext(ctx, store.RouteProbeShardModulusSQLForTest, uint64(placement.RouteRowShard(n)), n)
	require.NoError(t, err, "空表上 P5 的第一行应当插得进去(未提交)")

	normal, err := store.NewRouteStore(db.Cfg, routeTestOpts())
	require.NoError(t, err, "NORMAL只读启动不应等待metaX")
	require.NoError(t, normal.Close())
	done := make(chan error, 1)
	go func() {
		st, err := openRouteBehaviorProbe(ctx, db)
		if st != nil {
			_ = st.Close()
		}
		done <- err
	}()

	if err := awaitLockWaiters(ctx, db.Raw, store.RouteMetaTableName, 1, 5*time.Second); err != nil {
		skipOrFailOnLockWait(t, err, "建池应当排在 route_meta(id=1) 的守卫上")
	}
	var onRoute int
	require.NoError(t, db.Raw.QueryRowContext(ctx, lockWaitersQuery, store.PlayerRouteTableName).Scan(&onRoute))
	require.Zero(t, onRoute, "建池排到了 player_route 的未提交探针行后面:守卫没有先于探针插入")

	require.NoError(t, holder.Rollback())
	require.NoError(t, <-done, "holder 回滚之后建池应当通过")
	assert.EqualValues(t, 0, db.Count(t, store.PlayerRouteTableName, ""), "探针行必须全部回滚")
}

// TestRouteStore_ShardMismatchRowFailsClosed(评审 M1):CHECK 关掉期间被写进来的错片行 (3, 6) ——
//
//	① 全表键集扫描 ScanRoutes 看得见它,报 ErrRouteShardMismatch;
//	② 按 id 点读 GetRoutes([6]) 按正确片 (6, 6) 查,**看不见它、表现为缺席**(结构性局限,不是 bug);
//	③ 于是导入会把 6 当新行再插一条 (6, 6):同一个 player_id 两行 —— 这就是 P4 必须在建池时 fail-closed 的理由;
//	④ 之后 ScanRoutes 仍报错((3, 6) 排在 (6, 6) 之前)。
func TestRouteStore_ShardMismatchRowFailsClosed(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	skipUnlessMySQLEnforcesCheck(t, db)
	st := newRouteStore(t, db) // CHECK 生效时建池,P4 通过
	ctx := context.Background()

	// 关掉之后保持关闭:存量违例行在,ALTER CHECK … ENFORCED 会失败。
	_, err := db.Raw.Exec("ALTER TABLE player_route ALTER CHECK chk_route_shard NOT ENFORCED")
	require.NoError(t, err)
	_, err = db.Raw.Exec("INSERT INTO player_route (route_shard, player_id, home_zone, rev, updated_by, created_at_ms, updated_at_ms)" +
		" VALUES (3, 6, 7, 1, 'it-bad-shard', 1, 1)")
	require.NoError(t, err)

	_, err = st.ScanRoutes(ctx, store.RouteScanCursor{}, 100)
	require.ErrorIs(t, err, store.ErrRouteShardMismatch, "① 全表扫描必须发现错片行")

	got, err := st.GetRoutes(ctx, []uint64{6})
	require.NoError(t, err)
	assert.Empty(t, got, "② 点读按正确片查,错片行对它不可见(检测靠 ScanRoutes,防范靠 P4)")

	stats, err := st.ImportRoutes(ctx, []store.RouteImportRow{{PlayerID: 6, HomeZone: 7}}, false)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Inserted, "③ 导入看不见错片行,把 6 当新行插入")
	assert.EqualValues(t, 2, db.Count(t, store.PlayerRouteTableName, "player_id = 6"),
		"③ CHECK 不生效时同一个 player_id 有了两行:这正是 P4 必须拒建 store 的原因")

	_, err = st.ScanRoutes(ctx, store.RouteScanCursor{}, 100)
	require.ErrorIs(t, err, store.ErrRouteShardMismatch, "④ 错片行仍在,全表扫描仍然报错")
}

// explainPlan 对一条已内联实参的语句跑 EXPLAIN FORMAT=TRADITIONAL,返回第一行(单表查询只有一行)的列 → 值。
func explainPlan(t *testing.T, db *storetest.DB, stmt string) map[string]string {
	t.Helper()
	rows, err := db.Raw.Query("EXPLAIN FORMAT=TRADITIONAL " + stmt)
	require.NoError(t, err, "EXPLAIN 失败")
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	require.True(t, rows.Next(), "EXPLAIN 没有返回任何行")
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	require.NoError(t, rows.Scan(ptrs...))
	plan := make(map[string]string, len(cols))
	for i, c := range cols {
		plan[c] = vals[i].String
	}
	return plan
}

// inlineRouteArgs 把语句里的 ? 依次换成给定的数字(只用于 EXPLAIN:实参全是无符号整数,直接内联安全)。
func inlineRouteArgs(t *testing.T, stmt string, args []uint64) string {
	t.Helper()
	for _, a := range args {
		require.Contains(t, stmt, "?", "占位符数量与参数不符")
		stmt = strings.Replace(stmt, "?", strconv.FormatUint(a, 10), 1)
	}
	require.NotContains(t, stmt, "?", "占位符数量与参数不符")
	return stmt
}

// routeKeyArgs 返回 ids 的 (行片, player_id) 交错实参,与 loadRoutes 绑定的形状相同(调用方保证 ids 已按聚簇键升序)。
func routeKeyArgs(ids []uint64) []uint64 {
	out := make([]uint64, 0, 2*len(ids))
	for _, id := range ids {
		out = append(out, uint64(placement.RouteRowShard(id)), id)
	}
	return out
}

// TestRouteStore_KeyedReadsUsePrimaryRange:按聚簇键的读(R8 / R13 的行构造器 IN、R12 的键集翻页)必须走 PRIMARY 的
// range,不能退化成全表 / 全索引扫描 —— R8 的 FOR UPDATE 形态一旦全表扫描,RC 下会逐行加锁再放,与 W1 互等成 1205。
// 判据取否定式(拦住 index / ALL),同 player_name 的 EXPLAIN 用例;每份计划原样留档。
//
// 覆盖面:单个行构造器(MySQL 的行构造器 range 优化写明要求「多于一个」,单个时是否被改写成等值未核实)、3 个、
// 1000 个 + FOR UPDATE(eq_range_index_dive_limit=200 之后改用统计估算;range_optimizer_max_mem_size 超限会静默退回
// 全表并只给 3170 警告)、R12 的中段游标与零游标(覆盖全表,最容易退化成 ALL + filesort)。
//
// 夹具取 20 批而不是刚够两批,原因见函数体。同一条成本规律对生产也成立(推算,未实跑):表还不到批量的几倍时,
// 真实导入的 R8 同样会走全表 / 全索引扫描 + FOR UPDATE。P1 导入时全部副本是 redis 模式、库上没有并发 W1,无害;
// P3 的 -allow-db-first 导入发生在表远大于批量之后。放大夹具后若仍是 index / ALL,连同 SELECT VERSION() 与五份计划
// 原样回报,不改 SQL、不改断言。
func TestRouteStore_KeyedReadsUsePrimaryRange(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	var version string
	require.NoError(t, db.Raw.QueryRow("SELECT VERSION()").Scan(&version))
	if strings.Contains(strings.ToUpper(version), "TIDB") {
		t.Skipf("TiDB %s:计划形状不同(期望 Batch_Point_Get),见 TestRouteStore_TiDBClusteredShardsAndPresplit", version)
	}
	st := newRouteStore(t, db)
	ctx := context.Background()

	// 先导入 2 万行:1000 对的 IN 只占全表 5%。表太小时(行数不到批量的约 3.4 到 10 倍)全表扫描本来就比 1000 次
	// 点查便宜,会掩盖要测的事 —— range 优化是否被 eq_range_index_dive_limit / range_optimizer_max_mem_size 打回。
	// 倍数按 MySQL 8.0.36 的成本公式推算(未实跑):1000 个唯一区间约 1000×(0.25~1.0)+100,全表基线约 0.1×行数。
	const first uint64 = 100001
	const total = 20 * store.RouteBatchLimit
	for lo := 0; lo < total; lo += store.RouteBatchLimit {
		batch := make([]store.RouteImportRow, 0, store.RouteBatchLimit)
		for i := lo; i < lo+store.RouteBatchLimit; i++ {
			batch = append(batch, store.RouteImportRow{PlayerID: first + uint64(i), HomeZone: 7})
		}
		_, err := st.ImportRoutes(ctx, batch, false)
		require.NoError(t, err)
	}
	// ANALYZE 回一个结果集(Table / Op / Msg_type / Msg_text),用 Query 读完再关,不用 Exec。
	analyzed, err := db.Raw.Query("ANALYZE TABLE player_route")
	require.NoError(t, err)
	for analyzed.Next() {
	}
	require.NoError(t, analyzed.Err())
	require.NoError(t, analyzed.Close())

	sortedIDs := func(n int) []uint64 {
		ids := make([]uint64, n)
		for i := range ids {
			ids[i] = first + uint64(i)
		}
		sort.Slice(ids, func(i, j int) bool {
			return store.RouteScanCursorAt(store.RouteRow{PlayerID: ids[i]}).Less(store.RouteScanCursorAt(store.RouteRow{PlayerID: ids[j]}))
		})
		return ids
	}

	// R12 的中段游标落在正中间那一片(由 placement.RouteRowShards 算,不写片号字面量):游标之后约剩半张表。
	// 16 片时这里写的是片 5;照抄到 64 片下,游标只跳过表头不到一成,算不上「中段」。
	midShard := uint64(placement.RouteRowShards / 2)

	for _, tc := range []struct {
		name   string
		stmt   string
		noSort bool // R12:ORDER BY 必须由主键序满足,不许 filesort
	}{
		{name: "R13_单个行构造器", stmt: inlineRouteArgs(t, store.RouteSelectRowsSQLForTest(1, false), routeKeyArgs(sortedIDs(1)))},
		{name: "R13_3个行构造器", stmt: inlineRouteArgs(t, store.RouteSelectRowsSQLForTest(3, false), routeKeyArgs(sortedIDs(3)))},
		{name: "R8_1000个行构造器_FOR_UPDATE",
			stmt: inlineRouteArgs(t, store.RouteSelectRowsSQLForTest(store.RouteBatchLimit, true), routeKeyArgs(sortedIDs(store.RouteBatchLimit)))},
		{name: "R12_中段游标", stmt: inlineRouteArgs(t, store.RouteScanSQLForTest, []uint64{midShard, midShard, first, 100}), noSort: true},
		{name: "R12_零游标", stmt: inlineRouteArgs(t, store.RouteScanSQLForTest, []uint64{0, 0, 0, 100}), noSort: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainPlan(t, db, tc.stmt)
			t.Logf("%s 的执行计划: %v", tc.name, plan)
			assert.Equalf(t, "PRIMARY", plan["key"], "%s 没有走聚簇主键。完整计划: %v", tc.name, plan)
			assert.NotContainsf(t, []string{"index", "ALL"}, plan["type"],
				"%s 的访问类型是 %q:index=全索引扫描 / ALL=全表扫描(FOR UPDATE 形态会逐行加锁)。完整计划: %v", tc.name, plan["type"], plan)
			if tc.noSort {
				assert.NotContainsf(t, plan["Extra"], "filesort", "%s 要 filesort:ORDER BY 没有由聚簇键序满足。完整计划: %v", tc.name, plan)
			}
		})
	}
}

// TestRouteStore_TiDBClusteredShardsAndPresplit 只在真 TiDB 上跑(本机 deploy/docker-compose.tidb.yml,且建表前
// `SET GLOBAL tidb_enable_check_constraint=ON`):两张表是聚簇主键;P4 在 TiDB 上拿到 3819 + 约束名;注册后全表扫描覆盖;
// 63 / 64 号已在库时再建池,P5 的撞键分支(1062 之后同一事务继续)在 TiDB 上也通过;
// tidb-prepare 预切出 Region、再跑一次不报错(幂等)。二次预切期间并发建角的结果(是否出现 8028)只记录不断言(HR9 取证)。
//
// 末尾给按键读取证:R13(行构造器 IN)与 R12(键集翻页的中段游标 / 零游标)的 EXPLAIN 原样留档,R13 与 R12 中段游标
// 不许全表扫描。MySQL 的 EXPLAIN 用例在 TiDB 上整体 Skip,这里是 TiDB 一侧唯一的取证。全表扫描的算子名凭记忆
// 【未核实】,所以先用一条必然全表扫描的对照语句校准:对照里认不出它时用例报红,不让「不含」空过。
//
// 不能用 NewMigratedDB:全局库的整套迁移在 TiDB 上会因 player_snapshot 的可空唯一键失败,只建路由表。
func TestRouteStore_TiDBClusteredShardsAndPresplit(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var version string
	require.NoError(t, db.Raw.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version))
	if !strings.Contains(strings.ToUpper(version), "TIDB") {
		t.Skipf("%s 不是 TiDB:本用例只在真 TiDB 上跑", version)
	}
	var checkOn string
	require.NoError(t, db.Raw.QueryRowContext(ctx, "SELECT @@GLOBAL.tidb_enable_check_constraint").Scan(&checkOn))
	if !strings.EqualFold(checkOn, "ON") && checkOn != "1" {
		t.Skipf("TiDB %s 的 tidb_enable_check_constraint=%s:先 `SET GLOBAL tidb_enable_check_constraint=ON` 再跑(建表前就要开)", version, checkOn)
	}

	require.NoError(t, store.BootstrapRouteTablesOnlyForTest(ctx, db.Raw, db.Name))
	require.NoError(t, db.Fixture().GrantTables(ctx, db.Name, "player_route", "zone_merged_into", "route_reconcile"))
	pkTypes := map[string]string{}
	rows, err := db.Raw.QueryContext(ctx, "SELECT TABLE_NAME, TIDB_PK_TYPE FROM information_schema.TABLES"+
		" WHERE TABLE_SCHEMA = ? AND TABLE_NAME IN ('player_route')", db.Name)
	require.NoError(t, err)
	for rows.Next() {
		var name, pkType string
		require.NoError(t, rows.Scan(&name, &pkType))
		pkTypes[strings.ToLower(name)] = pkType
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.Equal(t, map[string]string{"player_route": "CLUSTERED"}, pkTypes)

	// P4 在 TiDB 上同样要拿到 3819 + chk_route_shard;P5 接着在同一个事务里插两行合法行 —— TiDB 在 P4 的语句失败
	// 之后事务能否继续用【未核实】,这里就是取证:建池失败时原样回报报错文本。
	st := newRouteStore(t, db)

	const zone uint32 = 7
	for i := uint64(0); i < 40; i++ {
		res, err := st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 7001 + i, HomeZone: zone})
		require.NoError(t, err)
		require.Equal(t, store.RouteRegisterInserted, res.Outcome)
	}
	var scanned int
	var after store.RouteScanCursor
	for {
		page, err := st.ScanRoutes(ctx, after, 7)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		scanned += len(page)
		after = store.RouteScanCursorAt(page[len(page)-1])
	}
	assert.Equal(t, 40, scanned)

	// 生产常态在 TiDB 上的取证:63 / 64 号已建角时 P5 的两行都撞 1062,按「已接受」在同一事务里继续 —— TiDB 上撞键之后
	// 事务能否继续用【未核实】,这里再建一个 store 就是取证;失败时原样回报报错文本。放在上面的全表扫描之后,不改它的 40 行口径。
	probeN := uint64(placement.RouteRowShards)
	for _, id := range []uint64{probeN, probeN - 1} {
		res, err := st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: id, HomeZone: zone})
		require.NoError(t, err)
		require.Equal(t, store.RouteRegisterInserted, res.Outcome)
	}
	second, err := store.NewRouteStore(db.Cfg, routeTestOpts())
	require.NoErrorf(t, err, "TiDB 上 63 / 64 号已在库时建池失败(P5 撞键之后事务是否可用),原样回报报错文本")
	require.NoError(t, second.Close())
	assert.EqualValues(t, 2, db.Count(t, store.PlayerRouteTableName, "player_id IN (?, ?) AND rev = 1", probeN, probeN-1),
		"P5 撞键不许改这两行")

	// DDL 预切由自有 Schema 池执行，正常业务仍使用受围栏保护的 NORMAL 池。
	schemaTool, err := openRouteBehaviorProbe(ctx, db)
	require.NoError(t, err)
	defer schemaTool.Close()
	req := store.RouteTiDBPrepareRequest{Zones: []uint32{zone}, Storages: []uint32{1000000}}
	rep, err := schemaTool.PrepareTiDB(ctx, req)
	require.NoError(t, err)
	for _, step := range rep.Steps {
		t.Logf("tidb-prepare: %s → executed=%t split_regions=%d", step.SQL, step.Executed, step.SplitRegions)
	}
	assert.Positive(t, rep.SplitRegions(), "第一次预切应当切出 Region")

	countRegions := func(stmt string) int {
		t.Helper()
		rs, err := db.Raw.QueryContext(ctx, stmt)
		require.NoError(t, err, stmt)
		defer rs.Close()
		n := 0
		for rs.Next() {
			n++
		}
		require.NoError(t, rs.Err())
		t.Logf("%s → %d regions", stmt, n)
		return n
	}
	// 行数据每片一段(placement.RouteRowShards 段);idx_route_home 每个 zone 的切点数见 route_tidb.go
	// (这里只有 1 个 zone)。两个下限都不写字面量。
	assert.GreaterOrEqual(t, countRegions("SHOW TABLE player_route REGIONS"), int(placement.RouteRowShards))
	assert.GreaterOrEqual(t, countRegions("SHOW TABLE player_route INDEX idx_route_home REGIONS"), int(store.RouteTiDBPrefixSplitPointsForTest))

	// 再跑一次必须不报错(幂等)。期间并发建角:ALTER … ATTRIBUTES 是 DDL,可能让在途悲观事务以 8028 失败(HR9),
	// 结果只记录、不断言。
	var wg sync.WaitGroup
	outcomes := make([]registerOutcome, 4)
	for i := range outcomes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 7101 + uint64(i), HomeZone: zone})
			outcomes[i] = registerOutcome{res: res, err: err}
		}()
	}
	_, err = schemaTool.PrepareTiDB(ctx, req)
	wg.Wait()
	require.NoError(t, err, "第二次 tidb-prepare 必须幂等")
	for i, o := range outcomes {
		var me *mysql.MySQLError
		errNo := uint16(0)
		if errors.As(o.err, &me) {
			errNo = me.Number
		}
		t.Logf("二次预切期间的并发建角 %d: outcome=%s err_no=%d err=%v", i, o.res.Outcome, errNo, o.err)
	}

	ids := []uint64{7001, 7002, 7003}
	sort.Slice(ids, func(i, j int) bool {
		return store.RouteScanCursorAt(store.RouteRow{PlayerID: ids[i]}).Less(store.RouteScanCursorAt(store.RouteRow{PlayerID: ids[j]}))
	})
	stmt := inlineRouteArgs(t, store.RouteSelectRowsSQLForTest(len(ids), false), routeKeyArgs(ids))

	// explainTiDB 跑一次 EXPLAIN,把整份计划(每行的各列用 " | " 连起来)原样留档并返回。
	// 不假设列名、列数与算子所在的列:判据只在整份文本上找子串。
	explainTiDB := func(name, query string) string {
		t.Helper()
		planRows, err := db.Raw.QueryContext(ctx, "EXPLAIN "+query)
		require.NoError(t, err, name)
		defer planRows.Close()
		cols, err := planRows.Columns()
		require.NoError(t, err, name)
		var plan strings.Builder
		for planRows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			require.NoError(t, planRows.Scan(ptrs...), name)
			for _, v := range vals {
				plan.WriteString(v.String)
				plan.WriteString(" | ")
			}
			plan.WriteString("\n")
		}
		require.NoError(t, planRows.Err(), name)
		t.Logf("%s 在 TiDB 上的计划:\n%s", name, plan.String())
		return plan.String()
	}

	// TiDB 把全表 / 全索引扫描的算子叫 TableFullScan / IndexFullScan(凭记忆,【未核实】),判据取两者共有的子串。
	// 先拿一条必然全表扫描的语句校准:它的计划里认得出这个子串,下面的「不含」才构成证据;认不出就报红,由人按留档的
	// 计划判读,不让否定式断言在不认识的格式上空过。
	const tidbFullScanOp = "FullScan"
	assert.Containsf(t, explainTiDB("对照(无条件读全表)", "SELECT * FROM player_route"), tidbFullScanOp,
		"对照语句必然全表扫描,计划里却没有 %q:这一版 TiDB 的 EXPLAIN 格式与用例的假设不同,"+
			"下面两条「不含」不构成证据,请按留档的计划人工判读", tidbFullScanOp)
	assert.NotContains(t, explainTiDB("R13", stmt), tidbFullScanOp, "R13 不许全表扫描")

	// R12(键集翻页)。中段游标取真实翻页会用到的那一个:从头翻 20 行(此时至少已有 40 行)的最后一行,
	// 片由 RouteScanCursorAt(即 placement.RouteRowShard)算,不写片号字面量。实参顺序同 R12:游标片、游标片、
	// 游标 player_id、limit。零游标本来就覆盖全表,只留档不断言。
	firstHalf, err := st.ScanRoutes(ctx, store.RouteScanCursor{}, 20)
	require.NoError(t, err)
	require.Len(t, firstHalf, 20)
	scanStmt := func(cursor store.RouteScanCursor) string {
		return inlineRouteArgs(t, store.RouteScanSQLForTest, []uint64{uint64(cursor.Shard), uint64(cursor.Shard), cursor.PlayerID, 7})
	}
	assert.NotContains(t, explainTiDB("R12 中段游标", scanStmt(store.RouteScanCursorAt(firstHalf[len(firstHalf)-1]))), tidbFullScanOp,
		"R12 中段游标不许全表扫描(否则键集翻页每页都从表头扫起,audit 方向 A 与全量预热是 O(N²/页大小))")
	explainTiDB("R12 零游标", scanStmt(store.RouteScanCursor{}))
}

// ── 冻结、解冻与反向修库(第 1b-1 批)──────────────────────────────────────────────

// freezeCtx 是冻结 / 解冻调用的时限(store.RouteFreezeTimeoutMin,见它的注释)。
func freezeCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), store.RouteFreezeTimeoutMin)
	t.Cleanup(cancel)
	return ctx
}

// readReconcileWatermark 读 route_reconcile(id=1).watermark_ms。
func readReconcileWatermark(t *testing.T, db *storetest.DB) uint64 {
	t.Helper()
	var wm uint64
	require.NoError(t, db.Raw.QueryRow("SELECT watermark_ms FROM route_reconcile WHERE id = 1").Scan(&wm))
	return wm
}

// TestRouteStore_RepairFromCacheRestoresRolledBackRows:模拟库回档 —— 库里是 rev 3 的旧值,缓存里是 rev 5 的新值。
// 可写时修库拒绝;冻结后修库把缓存那一版连同 rev 写回库(rev = maprev,不是 +1),缓存有而库没有的玩家插入;
// 原样重跑全部 Matched、updated_at_ms 不变(幂等);冻结期 W1 拒写;解冻之后 W1 恢复。只比较相等与相对大小,不依赖墙钟。
func TestRouteStore_RepairFromCacheRestoresRolledBackRows(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()

	seeded, err := st.ImportRoutes(ctx, []store.RouteImportRow{{PlayerID: 8101, HomeZone: 7, MapRev: 3}}, false)
	require.NoError(t, err)
	require.Equal(t, 1, seeded.Inserted)

	cache := []store.RouteImportRow{
		{PlayerID: 8101, HomeZone: 8, Placement: &placement.Record{StorageID: 1000000, Version: 1}, MapRev: 5},
		{PlayerID: 8102, HomeZone: 7, MapRev: 2},
	}
	_, err = st.RepairRoutesFromCache(ctx, cache, store.RouteRepairOptions{})
	require.Error(t, err, "NORMAL且没有正式冻结原件时不许反向修库")

	normalBefore := st
	frozenReceipt, err := db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	require.Equal(t, pb.SqlWriterMaintenanceReceipt_FROZEN, frozenReceipt.Phase)
	againReceipt, err := db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	assert.True(t, proto.Equal(frozenReceipt, againReceipt), "二次冻结复用同一永久收据，不能生成新轮次")
	_, err = normalBefore.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 8199, HomeZone: 7})
	require.ErrorIs(t, err, authoritysql.ErrWriterDenied, "冻结前boot已永久退役")
	st = newMaintenanceRouteStore(t, db, frozenReceipt)

	ids := []uint64{8101, 8102}
	before, err := st.GetRoutes(ctx, ids)
	require.NoError(t, err)
	dry, err := st.RepairRoutesFromCache(ctx, cache, store.RouteRepairOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, 1, dry.Restored)
	assert.Equal(t, 1, dry.Inserted)
	afterDry, err := st.GetRoutes(ctx, ids)
	require.NoError(t, err)
	assert.Equal(t, before, afterDry, "dry-run 必须零写入")

	stats, err := st.RepairRoutesFromCache(ctx, cache, store.RouteRepairOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Restored)
	assert.Equal(t, 1, stats.Inserted)
	after, err := st.GetRoutes(ctx, ids)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), after[8101].Rev, "rev 置为 maprev,不是 +1")
	assert.Equal(t, uint32(8), after[8101].HomeZone)
	assert.Equal(t, uint32(1000000), after[8101].StorageID)
	assert.Equal(t, uint64(2), after[8102].Rev, "新行 rev = maprev")
	assert.Equal(t, before[8101].CreatedAtMs, after[8101].CreatedAtMs, "覆盖不动 created_at_ms")

	again, err := st.RepairRoutesFromCache(ctx, cache, store.RouteRepairOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, again.Matched, "重跑幂等")
	assert.Zero(t, again.Restored+again.Inserted+again.Filled+len(again.TiesResolved))
	afterAgain, err := st.GetRoutes(ctx, ids)
	require.NoError(t, err)
	assert.Equal(t, after, afterAgain, "重跑零变更(含 updated_at_ms)")

	mi := []store.MergedIntoRow{{SrcZone: 201, DstZone: 202, RunID: "201-202-20261018T000000Z-abcd1234"}}
	miStats, err := st.RepairMergedIntoFromCache(ctx, mi, false)
	require.NoError(t, err)
	assert.Equal(t, 1, miStats.Inserted)
	miStats, err = st.RepairMergedIntoFromCache(ctx, mi, false)
	require.NoError(t, err)
	assert.Equal(t, 1, miStats.Matched)

	released, err := db.Release(freezeCtx(t), 0)
	require.NoError(t, err)
	require.Equal(t, pb.SqlWriterMaintenanceReceipt_RELEASED, released.Phase)
	_, err = st.RepairRoutesFromCache(ctx, cache, store.RouteRepairOptions{})
	require.ErrorIs(t, err, store.ErrRouteNotFrozen, "解冻之后不许再修库")
	_, err = normalBefore.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 8199, HomeZone: 7})
	require.ErrorIs(t, err, authoritysql.ErrWriterDenied, "释放不复活旧boot")
	normalAfter := newRouteStore(t, db)
	res, err := normalAfter.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 8199, HomeZone: 7})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterInserted, res.Outcome, "解冻之后 W1 恢复")
}

// TestRouteStore_RepairTieReportedUnlessResolved:同 rev 而值不同的平局默认只报告、不写;显式 ResolveTies 才用缓存覆盖(rev 不变);
// 覆盖之后再跑就是 Matched。
func TestRouteStore_RepairTieReportedUnlessResolved(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()

	_, err := st.ImportRoutes(ctx, []store.RouteImportRow{{PlayerID: 8201, HomeZone: 7, MapRev: 4}}, false)
	require.NoError(t, err)
	frozenReceipt, err := db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	st = newMaintenanceRouteStore(t, db, frozenReceipt)
	before, ok := readRoute(t, st, 8201)
	require.True(t, ok)

	cache := []store.RouteImportRow{{PlayerID: 8201, HomeZone: 9, MapRev: 4}}
	stats, err := st.RepairRoutesFromCache(ctx, cache, store.RouteRepairOptions{})
	require.NoError(t, err)
	require.Len(t, stats.Ties, 1)
	assert.Empty(t, stats.TiesResolved)
	unchanged, _ := readRoute(t, st, 8201)
	assert.Equal(t, before, unchanged, "默认只报告平局,一个字节都不改")

	stats, err = st.RepairRoutesFromCache(ctx, cache, store.RouteRepairOptions{ResolveTies: true})
	require.NoError(t, err)
	require.Len(t, stats.TiesResolved, 1)
	resolved, _ := readRoute(t, st, 8201)
	assert.Equal(t, uint32(9), resolved.HomeZone, "ResolveTies 用缓存覆盖")
	assert.Equal(t, uint64(4), resolved.Rev, "平局覆盖 rev 不变")

	stats, err = st.RepairRoutesFromCache(ctx, cache, store.RouteRepairOptions{ResolveTies: true})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Matched)
}

// TestRouteStore_FreezeUnfreezeRoundTrip:解冻按 GREATEST 抬水位(只增不减)、generation 不换;已可写时解冻是 no-op、水位不动。
func TestRouteStore_FreezeUnfreezeRoundTrip(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()
	meta0, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	require.False(t, meta0.Frozen())
	wm0 := readReconcileWatermark(t, db)
	// 无本轮票据的释放现在明确拒绝；既有可写状态和水位仍须不变。
	missing, err := db.Fixture().Catalog.Release(freezeCtx(t), nil, nil)
	require.Error(t, err)
	assert.Nil(t, missing)
	assert.Equal(t, wm0, readReconcileWatermark(t, db), "无正式释放权限不得抬水位")
	_, err = db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	frozen, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	require.True(t, frozen.Frozen())
	ticket, request, err := db.PrepareRelease(freezeCtx(t), wm0+12345)
	require.NoError(t, err)
	released, err := db.Fixture().Catalog.Release(freezeCtx(t), ticket, request)
	require.NoError(t, err)
	require.Equal(t, pb.SqlWriterMaintenanceReceipt_RELEASED, released.Phase)
	assert.Equal(t, wm0+12345, readReconcileWatermark(t, db), "解冻先抬到请求绑定水位")
	replay, err := db.Fixture().Catalog.Release(freezeCtx(t), ticket, request)
	require.NoError(t, err)
	assert.True(t, proto.Equal(released, replay), "同请求重放必须复用永久收据")
	assert.Equal(t, wm0+12345, readReconcileWatermark(t, db), "重放不再次改水位")
	require.NoError(t, db.ReopenAfterRelease(freezeCtx(t)))
	meta1, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	assert.False(t, meta1.Frozen())
	assert.Equal(t, meta0.Generation, meta1.Generation, "解冻不换generation")
	_, err = db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	released, err = db.Release(freezeCtx(t), wm0+100)
	require.NoError(t, err)
	require.Equal(t, pb.SqlWriterMaintenanceReceipt_RELEASED, released.Phase)
	assert.Equal(t, wm0+12345, readReconcileWatermark(t, db), "水位只增不减")
}

func TestRouteStore_UnfreezeWaitsForRepairMetaLock(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err := db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	wm0 := readReconcileWatermark(t, db)
	ticket, request, err := db.PrepareRelease(freezeCtx(t), wm0+777)
	require.NoError(t, err)

	holder, err := db.Raw.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	var generation string
	var writable uint64
	require.NoError(t, holder.QueryRowContext(ctx, store.RouteLockMetaSQLForTest).Scan(&generation, &writable))
	require.Zero(t, writable)

	released, err := db.Fixture().Catalog.Release(freezeCtx(t), ticket, request)
	var me *mysql.MySQLError
	require.Truef(t, errors.As(err, &me) && me.Number == 1205, "解冻必须排在修库事务之后、等满 1s 以 1205 失败,got (%v, %v)", released, err)
	assert.Nil(t, released)
	meta, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	assert.True(t, meta.Frozen(), "1205 之后整笔回滚:仍冻结")
	assert.Equal(t, wm0, readReconcileWatermark(t, db), "1205 之后整笔回滚:水位不动")

	require.NoError(t, holder.Rollback())
	released, err = db.Fixture().Catalog.Release(freezeCtx(t), ticket, request)
	require.NoError(t, err)
	assert.Equal(t, pb.SqlWriterMaintenanceReceipt_RELEASED, released.Phase)
	require.NoError(t, db.ReopenAfterRelease(freezeCtx(t)))
}
