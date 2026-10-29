package barrierrestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"shared/placement"
)

type routeFixture struct {
	generation, watermark, home, storage, version, revision string
	writable                                                uint64
	merged, merging                                         uint64
	frozen                                                  driver.Value
	queries                                                 []string
}
type routeDriver struct {
	assets *testStore
	route  *routeFixture
}

func (d routeDriver) Open(string) (driver.Conn, error) {
	return &routeConn{storeConn: &storeConn{s: d.assets}, route: d.route}, nil
}

type routeConn struct {
	*storeConn
	route *routeFixture
}

func (c *routeConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "player_database") {
		return c.storeConn.QueryContext(ctx, q, args)
	}
	if c.tx != nil && strings.Contains(q, "SELECT COUNT(*)") {
		v := c.route.merging
		if strings.Contains(q, "zone_merged_into") {
			v = c.route.merged
		}
		return &routeRows{columns: []string{"COUNT(*)"}, values: []driver.Value{int64(v)}}, nil
	}
	if c.tx == nil || !strings.HasSuffix(q, " FOR UPDATE") {
		return nil, errors.New("必须在资产同一事务内锁路由")
	}
	c.route.queries = append(c.route.queries, q)
	switch {
	case strings.Contains(q, "route_reconcile"):
		return &routeRows{columns: []string{"watermark_ms"}, values: []driver.Value{[]byte(c.route.watermark)}}, nil
	case strings.Contains(q, "route_meta"):
		return &routeRows{columns: []string{"generation", "writable"}, values: []driver.Value{[]byte(c.route.generation), int64(c.route.writable)}}, nil
	case strings.Contains(q, "player_route"):
		if len(args) != 2 || fmt.Sprint(args[0].Value) != "7" || fmt.Sprint(args[1].Value) != "7" {
			return nil, errors.New("路由必须完整聚簇主键")
		}
		return &routeRows{columns: []string{"home_zone", "storage_id", "placement_version", "frozen_run_id", "rev"}, values: []driver.Value{[]byte(c.route.home), []byte(c.route.storage), []byte(c.route.version), c.route.frozen, []byte(c.route.revision)}}, nil
	}
	return nil, errors.New("未知路由查询")
}

type routeRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (r *routeRows) Columns() []string { return r.columns }
func (r *routeRows) Close() error      { return nil }
func (r *routeRows) Next(v []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(v, r.values)
	r.done = true
	return nil
}

func splitFixture(t *testing.T, dbFirst bool) (Restorer, *testStore, *miniredis.Miniredis, *miniredis.Miniredis, *routeFixture) {
	t.Helper()
	r, s, cache := fixture(t, "0", "0")
	mapping := miniredis.RunT(t)
	for _, key := range []string{placement.HomeZoneKey(7), placement.Key(7)} {
		cache.Del(key)
	}
	mapping.Set(placement.HomeZoneKey(7), "17")
	mapping.Set(placement.Key(7), "1000000:3")
	r.Redis = redis.NewClient(&redis.Options{Addr: cache.Addr(), DB: 2, MaxRetries: -1})
	t.Cleanup(func() { r.Redis.Close() })
	r.Redis.Set(context.Background(), "player:{7}:owner_epoch", "9", 0)
	r.MappingRedis = redis.NewClient(&redis.Options{Addr: mapping.Addr(), MaxRetries: -1})
	t.Cleanup(func() { r.MappingRedis.Close() })
	r.RoutingMode = RoutingModeSplit
	r.AuditedOwnerUpperBound = "8"
	route := &routeFixture{generation: strings.Repeat("a", 32), watermark: "1000", home: "17", storage: "1000000", version: "3", revision: "4"}
	if dbFirst {
		r.RoutingMode, r.RouteSchema = RoutingModeDBFirst, "global_routes"
		name := fmt.Sprintf("route-restore-%d", driverID.Add(1))
		sql.Register(name, routeDriver{assets: s, route: route})
		db, err := sql.Open(name, "")
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })
		r.DB = db
		mapping.Set(placement.MapRevKey(7), "4")
		watermark, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: route.generation, WatermarkMs: 1000})
		require.NoError(t, err)
		mapping.Set(placement.RouteWatermarkKey, watermark)
	}
	p := Plan{Format: FormatV2, Schema: "player_store_1000000_db", Zone: 17, RedisAddr: cache.Addr(), RedisDB: 2, MappingAddr: mapping.Addr(), RoutingMode: r.RoutingMode, RouteSchema: r.RouteSchema, MaintenanceID: "维护工单"}
	require.NoError(t, r.Guard(context.Background(), p))
	return r, s, cache, mapping, route
}

func TestSplitRedisGlobalPlacementAndNonzeroCacheDB(t *testing.T) {
	r, s, _, mapping, _ := splitFixture(t, false)
	p, err := r.Prepare(context.Background(), "player_store_1000000_db", 17, "维护工单", []string{"7"}, true)
	require.NoError(t, err)
	require.Equal(t, FormatV2, p.Format)
	require.NoError(t, r.ApplyOne(context.Background(), p, 0))
	require.NoError(t, r.ApplyOne(context.Background(), p, 0))
	require.Equal(t, 1, s.commits)
	require.True(t, mapping.Exists(placement.MergeFenceKey(17)))
	wrong := r
	wrong.MappingRedis = r.Redis
	require.ErrorContains(t, wrong.ApplyOne(context.Background(), p, 0), "实际 Redis")
}

func TestV1RejectsIndependentMappingBeforeSQL(t *testing.T) {
	r, _, _ := fixture(t, "8", "90")
	p := prepare(t, r, false)
	other := miniredis.RunT(t)
	mapping := redis.NewClient(&redis.Options{Addr: other.Addr(), MaxRetries: -1})
	defer mapping.Close()
	r.MappingRedis = mapping
	// 撤掉 SQL 依赖；若未在 BeginTx 前拒绝，这里会 panic，不能依靠后续 Lua 偶然拒绝。
	r.DB = nil
	require.ErrorContains(t, r.ApplyOne(context.Background(), p, 0), "v1 实际映射")
}

func TestDBFirstBindsFrozenSQLRouteAndRechecksAfterCommit(t *testing.T) {
	r, s, _, _, route := splitFixture(t, true)
	p, err := r.Prepare(context.Background(), "player_store_1000000_db", 17, "维护工单", []string{"7"}, true)
	require.NoError(t, err)
	s.onCommit = func() { route.revision = "5" }
	require.ErrorContains(t, r.ApplyOne(context.Background(), p, 0), "权威路由偏离")
	barrier, err := r.Redis.Exists(context.Background(), "player:{7}:save_barrier").Result()
	require.NoError(t, err)
	require.Zero(t, barrier)
	route.revision = "4"
	require.NoError(t, r.ApplyOne(context.Background(), p, 0))
	require.Equal(t, 1, s.commits)
	for i, q := range route.queries {
		switch i % 3 {
		case 0:
			require.Contains(t, q, "route_reconcile")
		case 1:
			require.Contains(t, q, "route_meta")
		case 2:
			require.Contains(t, q, "route_shard=? AND player_id=?")
		}
	}
}

func TestDBFirstRejectsDriftRollbackAndUnsafeCache(t *testing.T) {
	for _, kind := range []string{"unfrozen", "generation", "revision", "watermark", "placement", "ready", "cache_partial", "cache_ahead", "guard_missing", "guard_ttl", "merged", "merging"} {
		t.Run(kind, func(t *testing.T) {
			r, s, _, mapping, route := splitFixture(t, true)
			p, err := r.Prepare(context.Background(), "player_store_1000000_db", 17, "维护工单", []string{"7"}, true)
			require.NoError(t, err)
			switch kind {
			case "merged":
				route.merged = 1
			case "merging":
				route.merging = 1
			case "unfrozen":
				route.writable = 1
			case "generation":
				route.generation = strings.Repeat("b", 32)
			case "revision":
				route.revision = "5"
			case "watermark":
				route.watermark = "999"
			case "placement":
				route.storage = "17"
			case "ready":
				mapping.Set(placement.RouteReadyKey, "不可信标记")
			case "cache_partial":
				mapping.Del(placement.MapRevKey(7))
			case "cache_ahead":
				mapping.Set(placement.RouteWatermarkKey, `{"v":1,"generation":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","watermark_ms":1001}`)
			case "guard_missing":
				mapping.Del(placement.MergeFenceKey(17))
			case "guard_ttl":
				mapping.SetTTL(placement.MergeFenceKey(17), 1)
			}
			require.Error(t, r.ApplyOne(context.Background(), p, 0))
			require.Zero(t, s.commits)
			n, err := r.Redis.Exists(context.Background(), "player:{7}:save_barrier").Result()
			require.NoError(t, err)
			require.Zero(t, n)
		})
	}
}

func TestDBFirstAbsentReplicaUsesLockedSQLAndGuardConflictPreservesPrefix(t *testing.T) {
	r, _, _, mapping, _ := splitFixture(t, true)
	for _, key := range []string{placement.HomeZoneKey(7), placement.Key(7), placement.MapRevKey(7)} {
		mapping.Del(key)
	}
	p, err := r.Prepare(context.Background(), "player_store_1000000_db", 17, "维护工单", []string{"7"}, true)
	require.NoError(t, err)
	require.NoError(t, r.ApplyOne(context.Background(), p, 0))
	mapping.Set(placement.MergeFenceKey(17), "其他维护")
	require.Error(t, r.Guard(context.Background(), p))
	value, err := r.Redis.Get(context.Background(), MaintenanceKey(17)).Result()
	require.NoError(t, err)
	require.Equal(t, "维护工单", value)
	got, _ := mapping.Get(placement.MergeFenceKey(17))
	require.Equal(t, "其他维护", got)
}

func TestV2RequiresIndependentOldOwnerUpperBound(t *testing.T) {
	for _, bound := range []string{"", "09", "9", "10", "18446744073709551616"} {
		t.Run("bound="+bound, func(t *testing.T) {
			r, s, _, _, _ := splitFixture(t, true)
			r.AuditedOwnerUpperBound = bound
			_, err := r.Prepare(context.Background(), "player_store_1000000_db", 17, "维护工单", []string{"7"}, true)
			require.Error(t, err)
			require.Zero(t, s.commits)
		})
	}
	r, s, _ := fixture(t, "0", "0")
	r.AuditedOwnerUpperBound = "8"
	p := prepare(t, r, true)
	require.Equal(t, FormatV2, p.Format)
	require.NoError(t, r.ApplyOne(context.Background(), p, 0))
	require.Equal(t, 1, s.commits)
}
