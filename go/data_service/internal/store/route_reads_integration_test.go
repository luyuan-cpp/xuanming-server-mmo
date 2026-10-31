//go:build integration

package store_test

// 第 3 批读方(route_reads.go 的 RD2–RD3)在真实 InnoDB 上的语义。假驱动(route_reads_contract_test.go)证不了这几条:
//  1. RD2 / RD3 沿 (route_shard, player_id) 键集翻页无漏无重、不混入别的 zone / 落点,计划能用上 idx_route_home / idx_route_storage;
//
// 跑法同 route_store_integration_test.go(本地 docker 的 appuser 没有建库权限,用 root):
//   DATA_SERVICE_IT_MYSQL_USER=root DATA_SERVICE_IT_MYSQL_PASSWORD=<deploy/docker-compose.yml> \
//     go test -tags=integration ./internal/store/... -run "RouteReads" -count=1 -v
// 加 DATA_SERVICE_IT_REQUIRE=1:依赖不可达(MySQL 连不上、没有建库权限)从 SKIP 变成 FAIL,杜绝把 SKIP 报成通过。
// 前提同 route_store_integration_test.go:建 route store 要求 MySQL ≥ 8.0.16(探针 P4;更老是 FAIL 不是 SKIP)。

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"shared/placement"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envReadsITRequire 设为 1 时,本文件的用例被跳过即判失败(PASS / FAIL / SKIP 必须可区分)。
const envReadsITRequire = "DATA_SERVICE_IT_REQUIRE"

// requireReadsIT 必须是每个用例的第一句:它登记的清理最后一个执行(在 storetest 删库之后),
// 发现用例被跳过(依赖不可达)且设了 DATA_SERVICE_IT_REQUIRE=1 就判失败。
// 用清理而不是自己再探测一遍依赖:跳过的判定只在 storetest 一处,这里不复制第二份。
func requireReadsIT(t *testing.T) {
	t.Helper()
	if os.Getenv(envReadsITRequire) != "1" {
		return
	}
	t.Cleanup(func() {
		if t.Skipped() {
			t.Errorf("%s=1: this integration test was skipped (a dependency is missing or unusable, see the SKIP reason above); treated as FAIL",
				envReadsITRequire)
		}
	})
}

// readDBNowMs 读一次库时钟(与 store 同一个表达式),用来夹住 RD4 回的 now_ms。
func readDBNowMs(t *testing.T, db *storetest.DB) uint64 {
	t.Helper()
	var ms uint64
	require.NoError(t, db.Raw.QueryRow("SELECT "+store.RouteDBNowMsSQLForTest).Scan(&ms))
	return ms
}

// TestRouteReads_ScanRoutesByHome_RealDB:RD2 沿键集翻页覆盖本 zone 全部玩家(跨多个行片)、不重不漏、不混入别的 zone,
// 且按聚簇键严格升序;计划的候选索引里有 idx_route_home。
func TestRouteReads_ScanRoutesByHome_RealDB(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()
	const zone, other uint32 = 51, 52

	want := map[uint64]bool{}
	for i := uint64(0); i < 40; i++ {
		player := 200001 + i*3 // 步长 3:40 个号散到 40 个不同的行片上
		seedRouteRow(t, db, player, zone, 0, 0, "", 1)
		want[player] = true
	}
	for i := uint64(0); i < 10; i++ {
		seedRouteRow(t, db, 300001+i, other, 0, 0, "", 1)
	}
	seedRouteRow(t, db, 300100, 0, placement.DefaultGlobalStorageID, 1, "", 1) // home 为 NULL:不属于任何 zone

	const page = 7
	var cursor store.RouteScanCursor
	var got []store.RouteRow
	for pages := 0; ; pages++ {
		require.Less(t, pages, 20, "pagination does not terminate")
		rows, err := st.ScanRoutesByHome(ctx, zone, cursor, page)
		require.NoError(t, err)
		got = append(got, rows...)
		if len(rows) < page {
			break
		}
		cursor = store.RouteScanCursorAt(rows[len(rows)-1])
	}
	require.Len(t, got, len(want))
	for i, row := range got {
		assert.True(t, want[row.PlayerID], "player %d is not in zone %d", row.PlayerID, zone)
		assert.Equal(t, zone, row.HomeZone)
		if i > 0 {
			assert.True(t, store.RouteScanCursorAt(got[i-1]).Less(store.RouteScanCursorAt(row)), "keyset order broken at %d", i)
		}
	}
	shards := map[uint32]bool{}
	for _, row := range got {
		shards[placement.RouteRowShard(row.PlayerID)] = true
	}
	assert.Greater(t, len(shards), page, "the fixture must span more shards than one page, or cross-shard paging is untested")

	// 计划取证:候选索引里必须有 idx_route_home(由谓词决定,与表大小无关);实际选中的 key / Extra 原样留档。
	stmt := inlineRouteArgs(t, store.RouteScanByHomeSQLForTest, []uint64{uint64(zone), 3, 3, 200010, page})
	if plan, ok := explainUnlessTiDB(t, db, stmt); ok {
		t.Logf("RD2 plan: %v", plan)
		assert.Contains(t, plan["possible_keys"], "idx_route_home", "RD2 must be able to use idx_route_home")
	}
}

// TestRouteReads_ScanRoutesByStorage_RealDB:RD3 只列 storage_id 等于参数的行,翻页无漏无重。
func TestRouteReads_ScanRoutesByStorage_RealDB(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := context.Background()
	storage := placement.DefaultGlobalStorageID

	var want []uint64
	for i := uint64(0); i < 5; i++ {
		player := 400001 + i*7
		seedRouteRow(t, db, player, uint32(61+i), storage, 1, "", 1) // 跨 zone,同一个落点
		want = append(want, player)
	}
	// 两个干扰项:没有落点的、另一个落点的。
	seedRouteRow(t, db, 400100, 61, 0, 0, "", 1)
	seedRouteRow(t, db, 400101, 61, storage+1, 1, "", 1)

	var got []uint64
	var cursor store.RouteScanCursor
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10, "pagination does not terminate")
		rows, err := st.ScanRoutesByStorage(ctx, storage, cursor, 2)
		require.NoError(t, err)
		for _, row := range rows {
			assert.Equal(t, storage, row.StorageID)
			got = append(got, row.PlayerID)
		}
		if len(rows) < 2 {
			break
		}
		cursor = store.RouteScanCursorAt(rows[len(rows)-1])
	}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	assert.Equal(t, want, got)

	stmt := inlineRouteArgs(t, store.RouteScanByStorageSQLForTest, []uint64{uint64(storage), 0, 0, 0, 2})
	if plan, ok := explainUnlessTiDB(t, db, stmt); ok {
		t.Logf("RD3 plan: %v", plan)
		assert.Contains(t, plan["possible_keys"], "idx_route_storage", "RD3 must be able to use idx_route_storage")
	}
}

// explainUnlessTiDB 在 MySQL 上取 EXPLAIN FORMAT=TRADITIONAL 的第一行;TiDB 上计划形状不同,只记一笔、回 ok=false(不 Skip:
// 设了 DATA_SERVICE_IT_REQUIRE=1 时 Skip 会被判失败,而这一段只是取证,不是依赖)。
func explainUnlessTiDB(t *testing.T, db *storetest.DB, stmt string) (map[string]string, bool) {
	t.Helper()
	var version string
	require.NoError(t, db.Raw.QueryRow("SELECT VERSION()").Scan(&version))
	if strings.Contains(strings.ToUpper(version), "TIDB") {
		t.Logf("TiDB %s: EXPLAIN FORMAT=TRADITIONAL not applicable, plan not checked", version)
		return nil, false
	}
	return explainPlan(t, db, stmt), true
}

// logExplainUnlessTiDB 只把计划原样留档。
func logExplainUnlessTiDB(t *testing.T, db *storetest.DB, label, stmt string) {
	t.Helper()
	if plan, ok := explainUnlessTiDB(t, db, stmt); ok {
		t.Logf("%s plan: %v", label, plan)
	}
}
