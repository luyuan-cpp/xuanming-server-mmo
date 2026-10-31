//go:build integration

package routetool

// route_tool 端到端:真 MySQL(storetest 一次性库,真 store.RouteStore)+ miniredis 造数据(第 1a 批规格 f 节)。
//
//  1. import(dry-run)零写入;真导入之后 audit 为 0,再导一次全部 matched(幂等),maprev 成为新行的 rev;
//  2. 改一个 Redis 值 → audit 退出码 1(value_mismatch);
//  3. 库里插一行而 Redis 不写 → db_only_unpublished,退出码 1。
//
// 跑法同 store 包的集成用例(本地 docker 的 appuser 没有建库权限,用 root):
//   DATA_SERVICE_IT_MYSQL_USER=root DATA_SERVICE_IT_MYSQL_PASSWORD=<deploy/docker-compose.yml> \
//     go test -tags=integration ./internal/routetool/...

import (
	"bytes"
	"context"
	"testing"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	pb "proto/common/database"
	"shared/drainproof"
	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// SQL控制链使用真实owned fixture；miniredis数据只验证工具业务逻辑，不代表真实外部队列排空。
func toolMaintenanceStore(t *testing.T, db *storetest.DB, receipt *pb.SqlWriterMaintenanceReceipt) *store.RouteStore {
	t.Helper()
	cfg, err := db.MaintenanceCfg(context.Background())
	require.NoError(t, err)
	st, err := store.NewRouteStore(cfg, store.RouteStoreOptions{Writer: store.NewRouteWriterID("route_tool_it", "localhost"), MaintenanceFreeze: receipt})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRouteToolImportThenAuditOnRealStore(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st, err := store.NewRouteStore(db.Cfg, store.RouteStoreOptions{
		Writer: store.NewRouteWriterID("route_tool_it", "localhost"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	mr := miniredis.RunT(t)
	rd := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	setKeys(t, mr, map[string]string{
		"player:zone:9001":      "1",
		"player:placement:9001": "1:1",
		"player:zone:9002":      "2",
		"player:placement:9003": "3:1",
		"player:zone:9004":      "4",
		"player:maprev:9004":    "3",
		"merge:merged_into:7":   `{"dst":8,"run_id":"7-8-20261012T000000Z-0123abcd"}`,
	})
	ctx := context.Background()

	importOnce := func(dryRun bool) (ImportReport, int) {
		t.Helper()
		var out, errOut bytes.Buffer
		rep, err := Import(ctx, rd, st, ImportOptions{DryRun: dryRun, Batch: 2, MaxReport: 100, Stdout: &out, Stderr: &errOut})
		code := ImportExitCode(rep, err)
		t.Logf("import dry_run=%t exit=%d stdout=%s stderr=%s", dryRun, code, out.String(), errOut.String())
		return rep, code
	}
	auditOnce := func() (AuditReport, int) {
		t.Helper()
		var out, errOut bytes.Buffer
		rep, err := Audit(ctx, rd, st, AuditOptions{Batch: 2, MaxReport: 100, MaxRecheck: 100, Stdout: &out, Stderr: &errOut})
		code := AuditExitCode(rep, err)
		t.Logf("audit exit=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
		return rep, code
	}

	// 1. dry-run:统计照出,库里零写入。
	rep, code := importOnce(true)
	require.Equal(t, ExitClean, code)
	assert.Equal(t, 4, rep.Inserted)
	assert.Equal(t, 1, rep.MergedIntoInserted)
	assert.Equal(t, int64(0), db.Count(t, store.PlayerRouteTableName, ""))
	assert.Equal(t, int64(0), db.Count(t, store.ZoneMergedIntoTableName, ""))

	// 真导入 → audit 为 0。
	rep, code = importOnce(false)
	require.Equal(t, ExitClean, code)
	assert.Equal(t, 4, rep.Inserted)
	assert.Equal(t, 1, rep.MergedIntoInserted)
	got, err := st.GetRoutes(ctx, []uint64{9003, 9004})
	require.NoError(t, err)
	assert.Equal(t, uint64(3), got[9004].Rev, "新行 rev = maprev")
	assert.Equal(t, uint64(1), got[9003].Rev, "无 maprev 的新行 rev = 1")
	assert.Equal(t, uint32(0), got[9003].HomeZone, "只有落点的玩家导入后 home 仍为 NULL")

	audit, code := auditOnce()
	assert.Equal(t, ExitClean, code)
	assert.Equal(t, 0, audit.Differences())
	assert.Equal(t, 4, audit.DBRows)
	assert.Equal(t, 4, audit.RedisPlayers)

	// 再导一次:全部 matched,零写入(幂等)。
	rep, code = importOnce(false)
	require.Equal(t, ExitClean, code)
	assert.Equal(t, 0, rep.Inserted)
	assert.Equal(t, 4, rep.Matched)
	assert.Equal(t, 1, rep.MergedIntoMatched)

	// 2. 改一个 Redis 值 → value_mismatch。
	require.NoError(t, mr.Set("player:zone:9002", "5"))
	audit, code = auditOnce()
	assert.Equal(t, ExitAttention, code)
	assert.Equal(t, 1, audit.ValueMismatch)
	assert.Equal(t, 1, audit.Differences())
	require.NoError(t, mr.Set("player:zone:9002", "2"))

	// 3. 库里有行、缓存三键全无 → db_only_unpublished(W1 已提交未发布的孤儿行就是这个形状)。
	// route_shard = 9005 % 64 = 45(9005 = 140×64 + 45;聚簇主键首列,chk_route_shard 钉住)。
	_, err = db.Raw.Exec("INSERT INTO player_route (route_shard, player_id, home_zone, storage_id, placement_version, frozen_run_id,"+
		" rev, updated_by, created_at_ms, updated_at_ms) VALUES (?, 9005, 1, NULL, NULL, NULL, 1, 'it-orphan', 1, 1)",
		uint64(placement.RouteRowShard(9005)))
	require.NoError(t, err)
	audit, code = auditOnce()
	assert.Equal(t, ExitAttention, code)
	assert.Equal(t, 1, audit.DBOnlyUnpublished)
	assert.Equal(t, 1, audit.Differences())
}

// TestReconcileFromCache_RealDBEndToEnd(第 1b-1 批):真库 + miniredis 走一遍全局库回档后的恢复流程。
//
//  1. 导入两名玩家,库里另有一行只在库里(孤儿行);
//  2. 在缓存里把 9101 的 maprev 加到 rev+2 并改值 —— 等价于「库被回档丢了两版」;默认口径的 audit 报 redis_rev_ahead;
//  3. freeze → reconcile-from-cache(dry-run 零写入;真跑把缓存那一版连同 rev 写回库)→ audit -frozen-recovery 退出码 0
//     (孤儿行照常计 db_only_unpublished,但不计入退出码;默认口径仍是 1)→ unfreeze(没有就绪标记,显式确认放行)
//     → W1 恢复可写。
func TestReconcileFromCache_RealDBEndToEnd(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st, err := store.NewRouteStore(db.Cfg, store.RouteStoreOptions{
		Writer: store.NewRouteWriterID("route_tool_it", "localhost"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	mr := miniredis.RunT(t)
	rd := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	setKeys(t, mr, map[string]string{
		"player:zone:9101":   "1",
		"player:maprev:9101": "2",
		"player:zone:9102":   "2",
	})
	ctx := context.Background()

	var out, errOut bytes.Buffer
	imp, err := Import(ctx, rd, st, ImportOptions{Batch: 2, MaxReport: 100, Stdout: &out, Stderr: &errOut})
	require.Equal(t, ExitClean, ImportExitCode(imp, err), errOut.String())
	_, err = db.Raw.Exec("INSERT INTO player_route (route_shard, player_id, home_zone, storage_id, placement_version, frozen_run_id,"+
		" rev, updated_by, created_at_ms, updated_at_ms) VALUES (?, 9105, 1, NULL, NULL, NULL, 1, 'it-orphan', 1, 1)",
		uint64(placement.RouteRowShard(9105)))
	require.NoError(t, err)

	require.NoError(t, mr.Set("player:zone:9101", "5"))
	require.NoError(t, mr.Set("player:maprev:9101", "4"))
	auditOnce := func(frozenRecovery bool) (AuditReport, int) {
		t.Helper()
		var out, errOut bytes.Buffer
		rep, err := Audit(ctx, rd, st, AuditOptions{Batch: 2, MaxReport: 100, MaxRecheck: 100, Stdout: &out, Stderr: &errOut,
			Sleep: noSleep, FrozenRecovery: frozenRecovery})
		code := AuditExitCode(rep, err)
		t.Logf("audit frozen_recovery=%t exit=%d stdout=%s stderr=%s", frozenRecovery, code, out.String(), errOut.String())
		return rep, code
	}
	before, code := auditOnce(false)
	require.Equal(t, ExitAttention, code)
	require.Equal(t, 1, before.RedisRevAhead, "缓存 maprev 大于库 rev:库被回档的信号")

	freezeCtx, cancel := context.WithTimeout(ctx, store.RouteFreezeTimeoutMin)
	defer cancel()
	frozenReceipt, err := db.Freeze(freezeCtx)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	st = toolMaintenanceStore(t, db, frozenReceipt)

	repair := func(dryRun bool) RepairReport {
		t.Helper()
		var out, errOut bytes.Buffer
		// 此会话只收口本用例的同步SQL；miniredis不是全CLI排空证据，也不输出MAINTENANCE closure。
		work := drainproof.New()
		rep, err := ReconcileFromCache(ctx, rd, st, RepairOptions{Catalog: db.Fixture().Catalog, FrozenReceipt: frozenReceipt, Maintenance: work, DryRun: dryRun, Batch: 2, MaxReport: 100, Stdout: &out, Stderr: &errOut})
		work.CloseAdmission()
		require.NoError(t, work.Wait(ctx))
		require.Equal(t, ExitClean, RepairExitCode(rep, err), "stdout=%s stderr=%s", out.String(), errOut.String())
		return rep
	}
	dry := repair(true)
	assert.Equal(t, 1, dry.Restored)
	row, _ := st.GetRoutes(ctx, []uint64{9101})
	assert.Equal(t, uint64(2), row[9101].Rev, "dry-run 零写入")

	applied := repair(false)
	assert.Equal(t, 1, applied.Restored)
	assert.Equal(t, 1, applied.Matched, "9102 两边一致")
	row, _ = st.GetRoutes(ctx, []uint64{9101})
	assert.Equal(t, uint64(4), row[9101].Rev, "rev 置为 maprev")
	assert.Equal(t, uint32(5), row[9101].HomeZone, "值取缓存那一版")

	recovered, code := auditOnce(true)
	assert.Equal(t, ExitClean, code, "冻结期恢复口径:孤儿行不计入退出码,其余为 0")
	assert.Equal(t, 1, recovered.DBOnlyUnpublished)
	assert.Zero(t, recovered.RedisRevAhead)
	_, code = auditOnce(false)
	assert.Equal(t, ExitAttention, code, "默认口径下孤儿行仍算差异")

	ticket, request, err := db.PrepareRelease(ctx, 0)
	require.NoError(t, err)
	var uout, uerr bytes.Buffer
	urep, err := Unfreeze(ctx, rd, st, UnfreezeOptions{
		Catalog: db.Fixture().Catalog, TicketID: ticket, Request: request,
		CacheUnverifiedAcknowledged: true,
		Timeout:                     store.RouteFreezeTimeoutMin,
		Identity:                    func(context.Context) (RedisIdentity, error) { return RedisIdentity{}, nil },
		Stdout:                      &uout,
		Stderr:                      &uerr,
	})
	require.Equal(t, ExitClean, UnfreezeExitCode(urep, err), uerr.String())
	assert.True(t, urep.Changed)
	require.NoError(t, db.ReopenAfterRelease(ctx))
	normal, err := store.NewRouteStore(db.Cfg, store.RouteStoreOptions{Writer: store.NewRouteWriterID("route_tool_it", "localhost")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = normal.Close() })
	res, err := normal.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 9199, HomeZone: 1})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterInserted, res.Outcome, "解冻之后 W1 恢复")
}

// TestUnfreeze_RealDBRaisesWatermarkToKey(2026-10-19 第 1b-2 批收尾):真库 + miniredis —— 水位键 {库世代, X} 在、没有可信标记
// (显式确认)时,解冻把 route_reconcile.watermark_ms 抬到 ≥ X(R21 的 GREATEST),解冻之后水位键仍是 {库世代, X}:
// 不高于库水位,对账 leader 下一拍不会按「水位键 > 库水位」再冻结(规格 §2 M3、§13.1 第 4 条)。
func TestUnfreeze_RealDBRaisesWatermarkToKey(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st, err := store.NewRouteStore(db.Cfg, store.RouteStoreOptions{
		Writer: store.NewRouteWriterID("route_tool_it", "localhost"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	meta, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	const keyWm = uint64(1_900_000_000_000)
	key, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: meta.Generation, WatermarkMs: keyWm})
	require.NoError(t, err)
	mr := miniredis.RunT(t)
	rd := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	require.NoError(t, mr.Set(placement.RouteWatermarkKey, key))

	freezeCtx, cancel := context.WithTimeout(ctx, store.RouteFreezeTimeoutMin)
	defer cancel()
	frozenReceipt, err := db.Freeze(freezeCtx)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	st = toolMaintenanceStore(t, db, frozenReceipt)

	ticket, request, err := db.PrepareRelease(ctx, keyWm)
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	rep, err := Unfreeze(ctx, rd, st, UnfreezeOptions{
		Catalog: db.Fixture().Catalog, TicketID: ticket, Request: request,
		CacheUnverifiedAcknowledged: true,
		Timeout:                     store.RouteFreezeTimeoutMin,
		Identity:                    func(context.Context) (RedisIdentity, error) { return RedisIdentity{}, nil },
		Stdout:                      &out,
		Stderr:                      &errOut,
	})
	require.Equal(t, ExitClean, UnfreezeExitCode(rep, err), errOut.String())
	assert.True(t, rep.Changed)
	assert.Equal(t, keyWm, rep.MinWatermarkMs)
	assert.Equal(t, WatermarkKeyKept, rep.WatermarkKey)

	var wm uint64
	require.NoError(t, db.Raw.QueryRow("SELECT watermark_ms FROM route_reconcile WHERE id = 1").Scan(&wm))
	assert.GreaterOrEqual(t, wm, keyWm, "解冻先把库水位抬到水位键")
	got, err := mr.Get(placement.RouteWatermarkKey)
	require.NoError(t, err)
	assert.Equal(t, key, got)
	require.NoError(t, db.ReopenAfterRelease(ctx))
}
