package barrierrestore

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"shared/ownerepoch"
	"shared/placement"
	"shared/savebarrier"
)

// 不复制路由 DDL：只解析现有 store/schema.go 的字符串常量，不执行源码。
func productionRouteDDL(t *testing.T) []string {
	t.Helper()
	path := os.Getenv("BARRIER_RESTORE_ROUTE_SCHEMA_SOURCE")
	if path == "" {
		path = filepath.Join("..", "..", "..", "data_service", "internal", "store", "schema.go")
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoError(t, err)
	want := []string{"playerRouteBootstrapDDL", "routeMetaBootstrapDDL", "routeReconcileBootstrapDDL", "zoneMergeFenceBootstrapDDL", "zoneMergedIntoBootstrapDDL"}
	found := map[string]string{}
	var value func(ast.Expr) string
	value = func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.BasicLit:
			require.Equal(t, token.STRING, e.Kind)
			s, err := strconv.Unquote(e.Value)
			require.NoError(t, err)
			return s
		case *ast.BinaryExpr:
			require.Equal(t, token.ADD, e.Op)
			return value(e.X) + value(e.Y)
		default:
			t.Fatalf("正式DDL常量表达式不受支持：%T", e)
			return ""
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		v, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range v.Names {
			for _, key := range want {
				if name.Name == key {
					require.Less(t, i, len(v.Values))
					found[key] = value(v.Values[i])
				}
			}
		}
		return false
	})
	out := []string{}
	for _, key := range want {
		require.NotEmpty(t, found[key])
		out = append(out, found[key])
	}
	return out
}

func TestIntegrationDBFirstSplitRedisAndGlobalStorage(t *testing.T) {
	dsn, addr, strict := os.Getenv("BARRIER_RESTORE_TEST_DSN"), os.Getenv("BARRIER_RESTORE_TEST_REDIS"), os.Getenv("BARRIER_RESTORE_REQUIRE_TIDB")
	if dsn == "" && addr == "" && strict == "" {
		t.Skip("未启用独立恢复真库集成")
	}
	mappingAddr := os.Getenv("BARRIER_RESTORE_TEST_MAPPING_REDIS")
	require.NotEmpty(t, dsn)
	require.NotEmpty(t, addr)
	require.Equal(t, "1", strict)
	require.NotEmpty(t, mappingAddr, "第二批必须指定独立 mapping Redis，不能用同实例冒充跨实例验证")
	require.NotEqual(t, addr, mappingAddr)
	f := newRestoreIntegrationFixture(t, dsn, addr, true)
	id, first, second := f.player(t, 0, 0)
	before := f.assets(t, id)
	routeSchema := fmt.Sprintf("restore_routes_%d", f.zone)
	_, err := f.schemaAdmin.ExecContext(f.ctx, "CREATE DATABASE `"+routeSchema+"`")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := f.schemaAdmin.ExecContext(ctx, "DROP DATABASE `"+routeSchema+"`")
		require.NoError(t, err)
	})
	parsed, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	parsed.DBName = routeSchema
	migration, err := openRestoreAdminPool(t, f.ctx, parsed.FormatDSN(), routeSchema, true, 1, true)
	require.NoError(t, err)
	t.Cleanup(func() { migration.Close() })
	for _, ddl := range productionRouteDDL(t) {
		_, err = migration.ExecContext(f.ctx, ddl)
		require.NoError(t, err)
	}
	generation := strings.Repeat("a", 32)
	_, err = migration.ExecContext(f.ctx, "INSERT INTO route_meta(id,generation,writable) VALUES(1,?,0)", generation)
	require.NoError(t, err)
	_, err = migration.ExecContext(f.ctx, "INSERT INTO route_reconcile(id,watermark_ms) VALUES(1,1000)")
	require.NoError(t, err)
	storageID, ok := placement.StorageIDFromDBName(f.schema)
	require.True(t, ok)
	_, err = migration.ExecContext(f.ctx, "INSERT INTO player_route(route_shard,player_id,home_zone,storage_id,placement_version,rev,created_at_ms,updated_at_ms) VALUES(?,?,?,?,1,4,1000,1000)", placement.RouteRowShard(id), id, f.zone, storageID)
	require.NoError(t, err)
	require.NoError(t, migration.Close())
	cache := redis.NewClient(&redis.Options{Addr: addr, DB: 2, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	mapping := redis.NewClient(&redis.Options{Addr: mappingAddr, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	t.Cleanup(func() { cache.Close(); mapping.Close() })
	cacheKeys := []string{MaintenanceKey(f.zone), ownerepoch.OwnerEpochKey(id), savebarrier.Key(id)}
	mapKeys := []string{MaintenanceKey(f.zone), placement.MergeFenceKey(f.zone), placement.HomeZoneKey(id), placement.Key(id), placement.MapRevKey(id)}
	for _, group := range []struct {
		client *redis.Client
		keys   []string
	}{{cache, cacheKeys}, {mapping, mapKeys}} {
		for _, key := range group.keys {
			n, err := group.client.Exists(f.ctx, key).Result()
			require.NoError(t, err)
			require.Zero(t, n, "随机输入不能复用既有键")
		}
	}
	// 水位是映射实例级元数据。本测试要求第二映射实例专供本轮，不覆盖任何既有水位。
	mapKeys = append(mapKeys, placement.RouteWatermarkKey)
	watermark, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: generation, WatermarkMs: 1000})
	require.NoError(t, err)
	created, err := mapping.SetNX(f.ctx, placement.RouteWatermarkKey, watermark, 0).Result()
	require.NoError(t, err)
	require.True(t, created)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, group := range []struct {
			client *redis.Client
			keys   []string
		}{{cache, cacheKeys}, {mapping, mapKeys}} {
			require.NoError(t, group.client.Del(ctx, group.keys...).Err())
			n, err := group.client.Exists(ctx, group.keys...).Result()
			require.NoError(t, err)
			require.Zero(t, n)
		}
		t.Log("第二批自有 Redis 键清理后剩余 0")
	})
	created, err = cache.SetNX(f.ctx, ownerepoch.OwnerEpochKey(id), "19", 0).Result()
	require.NoError(t, err)
	require.True(t, created)
	r := Restorer{DB: f.r.DB, Redis: cache, MappingRedis: mapping, RoutingMode: RoutingModeDBFirst, RouteSchema: routeSchema, AuditedOwnerUpperBound: "18"}
	target := Plan{Format: FormatV2, Schema: f.schema, Zone: f.zone, RedisAddr: addr, RedisDB: 2, MappingAddr: mappingAddr, RoutingMode: RoutingModeDBFirst, RouteSchema: routeSchema, MaintenanceID: f.maintenance}
	require.NoError(t, r.Guard(f.ctx, target))
	p, err := r.Prepare(f.ctx, f.schema, f.zone, f.maintenance, []string{strconv.FormatUint(id, 10)}, true)
	require.NoError(t, err)
	require.Equal(t, FormatV2, p.Format)
	require.Equal(t, "4", p.Players[0].RouteRevision)
	for attempt := 0; attempt < 2; attempt++ {
		require.NoError(t, r.ApplyOne(f.ctx, p, 0))
		require.Equal(t, before, f.assets(t, id))
	}
	first.FenceOwnerEpoch, first.FenceSaveSeq = 19, 1
	second.FenceOwnerEpoch, second.FenceSaveSeq = 19, 1
	f.rowsEqual(t, first, second)
	for _, field := range []string{"pending", "player_database", "player_database_1"} {
		value, err := cache.HGet(f.ctx, savebarrier.Key(id), field).Result()
		require.NoError(t, err)
		require.Equal(t, savebarrier.Version(19, 1), value)
	}
	// SQL 已锁路由期间，真实解冻必须排队失败；不能穿过发布点。
	parsed.DBName = f.schema
	other, err := openRestoreAdminPool(t, f.ctx, parsed.FormatDSN(), f.schema, true, 1, false)
	require.NoError(t, err)
	defer other.Close()
	tx, err := f.r.DB.BeginTx(f.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { tx.Rollback() })
	_, err = r.lockRoute(f.ctx, tx, p, strconv.FormatUint(id, 10))
	require.NoError(t, err)
	lockCtx, cancel := context.WithTimeout(f.ctx, 300*time.Millisecond)
	unfreeze, err := other.BeginTx(lockCtx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	_, err = unfreeze.ExecContext(lockCtx, "UPDATE `"+routeSchema+"`.route_meta SET writable=1 WHERE id=1")
	cancel()
	// 不让取消的自动提交语句留下结局不明：本竞争事务从不提交，必定回滚。
	_ = unfreeze.Rollback()
	require.Error(t, err, "解冻不能穿过恢复事务的路由锁")
	require.NoError(t, tx.Rollback())
	var writable uint64
	require.NoError(t, other.QueryRowContext(f.ctx, "SELECT writable FROM `"+routeSchema+"`.route_meta WHERE id=1").Scan(&writable))
	require.Zero(t, writable)
	_, err = other.ExecContext(f.ctx, "UPDATE `"+routeSchema+"`.player_route SET rev=5 WHERE route_shard=? AND player_id=?", placement.RouteRowShard(id), id)
	require.NoError(t, err)
	require.ErrorContains(t, r.ApplyOne(f.ctx, p, 0), "权威路由偏离")
	require.Equal(t, before, f.assets(t, id))

	t.Run("独立Redis权威全局落点恢复和路由变化拒绝", func(t *testing.T) {
		// 同一隔离资产改用 Redis 真源；不能以 db_first 的成功冒充此部署分支。
		for _, entry := range []struct{ key, value string }{
			{placement.HomeZoneKey(id), strconv.FormatUint(uint64(f.zone), 10)},
			{placement.Key(id), placement.StableValue(storageID, 1)},
		} {
			created, err := mapping.SetNX(f.ctx, entry.key, entry.value, 0).Result()
			require.NoError(t, err)
			require.True(t, created, "不覆盖既有权威路由")
		}
		n, err := cache.Del(f.ctx, savebarrier.Key(id)).Result()
		require.NoError(t, err)
		require.EqualValues(t, 1, n, "只清本测试刚创建的屏障以模拟缓存缺失")
		split := r
		split.RoutingMode, split.RouteSchema = RoutingModeSplit, ""
		plan, err := split.Prepare(f.ctx, f.schema, f.zone, f.maintenance, []string{strconv.FormatUint(id, 10)}, false)
		require.NoError(t, err)
		require.Empty(t, plan.Players[0].RouteGeneration)
		var restored string
		for attempt := 0; attempt < 2; attempt++ {
			require.NoError(t, split.ApplyOne(f.ctx, plan, 0))
			fields, err := cache.HGetAll(f.ctx, savebarrier.Key(id)).Result()
			require.NoError(t, err)
			next := assertRestoredFence(t, fields["handoff_fence"], "19", "", "0", strings.Repeat("0", 32))
			require.NotEqual(t, restored, next, "同一计划重放也必须轮换撤销令牌")
			restored = next
			require.Equal(t, map[string]string{"pending": savebarrier.Version(19, 1), "player_database": savebarrier.Version(19, 1), "player_database_1": savebarrier.Version(19, 1), "schema_gen": "1", "handoff_fence": restored}, fields)
			ttl, err := cache.PTTL(f.ctx, savebarrier.Key(id)).Result()
			require.NoError(t, err)
			require.Equal(t, -time.Nanosecond, ttl)
			require.Equal(t, before, f.assets(t, id))
			f.rowsEqual(t, first, second)
		}
		n, err = cache.Del(f.ctx, savebarrier.Key(id)).Result()
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
		require.NoError(t, mapping.Set(f.ctx, placement.Key(id), placement.StableValue(storageID, 2), 0).Err())
		require.Error(t, split.ApplyOne(f.ctx, plan, 0), "同库落点版本变化也必须重新审定")
		n, err = cache.Exists(f.ctx, savebarrier.Key(id)).Result()
		require.NoError(t, err)
		require.Zero(t, n, "拒绝时不得发布屏障")
		require.Equal(t, before, f.assets(t, id))
	})

	t.Run("SQL本区空落点只按锁定home恢复", func(t *testing.T) {
		local := newRestoreIntegrationFixture(t, dsn, addr)
		localID, localFirst, localSecond := local.player(t, 8, 90)
		localBefore := local.assets(t, localID)
		_, err := local.r.DB.ExecContext(local.ctx, "INSERT INTO `"+routeSchema+"`.player_route(route_shard,player_id,home_zone,storage_id,placement_version,rev,created_at_ms,updated_at_ms) VALUES(?,?,?,NULL,NULL,1,1000,1000)", placement.RouteRowShard(localID), localID, local.zone)
		require.NoError(t, err)
		localCacheKeys := []string{MaintenanceKey(local.zone), ownerepoch.OwnerEpochKey(localID), savebarrier.Key(localID)}
		localMapKeys := []string{MaintenanceKey(local.zone), placement.MergeFenceKey(local.zone)}
		for _, group := range []struct {
			client *redis.Client
			keys   []string
		}{{cache, localCacheKeys}, {mapping, localMapKeys}} {
			n, err := group.client.Exists(local.ctx, group.keys...).Result()
			require.NoError(t, err)
			require.Zero(t, n, "本区夹具不复用既有维护键")
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, group := range []struct {
				client *redis.Client
				keys   []string
			}{{cache, localCacheKeys}, {mapping, localMapKeys}} {
				require.NoError(t, group.client.Del(ctx, group.keys...).Err())
				n, err := group.client.Exists(ctx, group.keys...).Result()
				require.NoError(t, err)
				require.Zero(t, n)
			}
		})
		localOwnerFloor := "19" // 本角色原Redis发号floor；SQL的8:90不降低撤销围栏。
		created, err := cache.SetNX(local.ctx, ownerepoch.OwnerEpochKey(localID), localOwnerFloor, 0).Result()
		require.NoError(t, err)
		require.True(t, created)
		restore := r
		restore.DB = local.r.DB
		localTarget := target
		localTarget.Schema, localTarget.Zone, localTarget.MaintenanceID = local.schema, local.zone, local.maintenance
		require.NoError(t, restore.Guard(local.ctx, localTarget))
		plan, err := restore.Prepare(local.ctx, local.schema, local.zone, local.maintenance, []string{strconv.FormatUint(localID, 10)}, false)
		require.NoError(t, err)
		require.Empty(t, plan.Players[0].Placement)
		var restored string
		for attempt := 0; attempt < 2; attempt++ {
			require.NoError(t, restore.ApplyOne(local.ctx, plan, 0))
			require.Equal(t, localBefore, local.assets(t, localID))
			local.rowsEqual(t, localFirst, localSecond)
			fence, err := cache.HGet(local.ctx, savebarrier.Key(localID), "handoff_fence").Result()
			require.NoError(t, err)
			next := assertRestoredFence(t, fence, localOwnerFloor, "", "0", strings.Repeat("0", 32))
			require.NotEqual(t, restored, next, "同一计划重放也必须轮换撤销令牌")
			restored = next
		}
		fields, err := cache.HGetAll(local.ctx, savebarrier.Key(localID)).Result()
		require.NoError(t, err)
		require.Equal(t, map[string]string{"pending": savebarrier.Version(8, 90), "player_database": savebarrier.Version(8, 90), "player_database_1": savebarrier.Version(8, 90), "schema_gen": "1", "handoff_fence": restored}, fields)
		ttl, err := cache.PTTL(local.ctx, savebarrier.Key(localID)).Result()
		require.NoError(t, err)
		require.Equal(t, -time.Nanosecond, ttl)
		local.noBarrier(t, localID)
	})
}
