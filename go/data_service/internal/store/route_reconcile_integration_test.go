//go:build integration

package store_test

// 对账 leader 库原语(route_reconcile.go 的 R23–R31)在真实 InnoDB 上的语义。假驱动(route_reconcile_contract_test.go)证不了这几条
// (R31 的「63 号片有行即列出」另见 TestRouteReconcile_ListMergeFenceZones):
//  1. 租约判据真的在库时钟上:同一时刻只有一个持有者,过期(lease_until_ms 改成 1 模拟,不靠睡眠)之后才可被接管,
//     旧 leader 的每拍与释放受影响 0 行、零写入;
//  2. 回档检测(M3):水位键水位 > 库水位(容差 0)或世代不符 → writable=0,W1 随即 ErrRouteNotWritable;冻结时水位不动;
//     解冻把库水位抬到水位键之后,同一把水位键不再触发冻结(规格 §13.1 第 4 条的出口);
//  3. 水位只增不减(GREATEST),cvm 按给定值精确写、可以变小;R27 值不变(0 行)时仍判 Advanced;
//  4. 锁序 route_reconcile → route_meta 与第 1b-1 批解冻同向:并发的每拍 / leader 冻结、route_tool 冻结 / 解冻、W1 不成环;
//  5. R29 按片键集翻页无漏无重、不串片,计划的候选索引里有 idx_route_updated。
//
// 跑法同 route_store_integration_test.go(本地 docker 的 appuser 没有建库权限,用 root):
//   DATA_SERVICE_IT_MYSQL_USER=root DATA_SERVICE_IT_MYSQL_PASSWORD=<deploy/docker-compose.yml> \
//     go test -tags=integration ./internal/store/... -run "RouteReconcile|ScanRoutesUpdatedSince" -count=1 -v
// 加 DATA_SERVICE_IT_REQUIRE=1:依赖不可达(MySQL 连不上、没有建库权限)从 SKIP 变成 FAIL(requireReadsIT,同包
// route_reads_integration_test.go 定义,全包同一个开关)。前提:建 route store 要求 MySQL ≥ 8.0.16(探针 P4;更老是 FAIL 不是 SKIP)。
// 全部不依赖墙钟:只比较相等与相对大小,时间一律取库时钟。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	pb "proto/common/database"
	"shared/authoritysql"
	"shared/placement"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcileITLeaseMs 是用例的租约时长:足够长,整个用例内不会自然过期(过期一律用 UPDATE lease_until_ms=1 模拟)。
const reconcileITLeaseMs uint64 = 60_000

// newReconcileLeaderID 生成一个 leader 标识(生产同一个构造函数,随机源用 crypto/rand)。
func newReconcileLeaderID(t *testing.T, host string) string {
	t.Helper()
	id, err := store.NewRouteLeaderID(host, rand.Reader)
	require.NoError(t, err)
	return id
}

// reconcileTick 是缺省的每拍请求(冻结时限取下限);mut 可改其中字段。
func reconcileTick(leaderID string, mut func(*store.ReconcileTickRequest)) store.ReconcileTickRequest {
	req := store.ReconcileTickRequest{LeaderID: leaderID, FreezeTimeout: store.RouteFreezeTimeoutMin}
	if mut != nil {
		mut(&req)
	}
	return req
}

// reconcileITCtx 是每个用例的总时限。
func reconcileITCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// mustHoldLease 让 leaderID 获取 / 续约租约,并断言持有。
func mustHoldLease(t *testing.T, ctx context.Context, st *store.RouteStore, leaderID string) {
	t.Helper()
	lease, err := st.RenewReconcileLease(ctx, leaderID, reconcileITLeaseMs)
	require.NoError(t, err)
	require.Truef(t, lease.HeldBy(leaderID), "%s must hold the lease: %+v", leaderID, lease)
}

// reconcileRowIT 是 route_reconcile(id=1) 的原样内容(绕过 store 直接读)。lease_until_ms 为 NULL 记 0。
type reconcileRowIT struct {
	Leader          string
	LeaseUntilMs    uint64
	WatermarkMs     uint64
	CacheVerifiedMs uint64
}

func readReconcileRowIT(t *testing.T, db *storetest.DB) reconcileRowIT {
	t.Helper()
	var leader sql.NullString
	var lease sql.Null[uint64]
	var row reconcileRowIT
	require.NoError(t, db.Raw.QueryRow("SELECT leader, lease_until_ms, watermark_ms, cache_verified_ms FROM route_reconcile WHERE id = 1").
		Scan(&leader, &lease, &row.WatermarkMs, &row.CacheVerifiedMs))
	row.Leader = leader.String
	if lease.Valid {
		row.LeaseUntilMs = lease.V
	}
	return row
}

// setReconcileWatermarkIT 绕过 store 改库水位:扮演 PITR / 异步复制切主之后回退了的库,或解冻抬过的水位。
func setReconcileWatermarkIT(t *testing.T, db *storetest.DB, wm uint64) {
	t.Helper()
	_, err := db.Raw.Exec("UPDATE route_reconcile SET watermark_ms = ? WHERE id = 1", wm)
	require.NoError(t, err)
}

// expireReconcileLeaseIT 让当前租约在库时钟上立即过期(不靠睡眠、不依赖墙钟)。
func expireReconcileLeaseIT(t *testing.T, db *storetest.DB) {
	t.Helper()
	_, err := db.Raw.Exec("UPDATE route_reconcile SET lease_until_ms = 1 WHERE id = 1")
	require.NoError(t, err)
}

// TestRouteReconcile_LeaseContention:两个 store、两个不同的 leader 标识,任一时刻只有一个持有;过期之后另一个接管;
// 旧 leader 的每拍(LeaseLost)、leader 冻结与释放都是零写入。
func TestRouteReconcile_LeaseContention(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	a, b := newRouteStore(t, db), newRouteStore(t, db)
	ctx := reconcileITCtx(t)
	idA, idB := newReconcileLeaderID(t, "it-a"), newReconcileLeaderID(t, "it-b")
	require.NotEqual(t, idA, idB)

	peek, err := a.PeekReconcileLease(ctx)
	require.NoError(t, err)
	assert.False(t, peek.Held, "① the migration seeds route_reconcile without a leader")
	assert.Empty(t, peek.Holder)

	mustHoldLease(t, ctx, a, idA)
	lb, err := b.RenewReconcileLease(ctx, idB, reconcileITLeaseMs)
	require.NoError(t, err)
	assert.False(t, lb.HeldBy(idB), "② B must be refused while A holds a valid lease")
	assert.True(t, lb.Held)
	assert.Equal(t, idA, lb.Holder)
	mustHoldLease(t, ctx, a, idA) // A 续约照常持有
	peek, err = b.PeekReconcileLease(ctx)
	require.NoError(t, err)
	require.True(t, peek.HeldBy(idA))
	assert.GreaterOrEqual(t, peek.LeaseUntilMs, peek.NowMs, "the lease ends after the database clock it was judged with")

	first, err := a.AdvanceReconcile(ctx, reconcileTick(idA, nil))
	require.NoError(t, err)
	require.Equal(t, store.ReconcileTickAdvanced, first.Outcome)

	// ③ 模拟 A 的租约过期:持有者仍记着 A,但已可被接管。A 这时的每拍、leader 冻结都是 LeaseLost、零写入。
	expireReconcileLeaseIT(t, db)
	peek, err = a.PeekReconcileLease(ctx)
	require.NoError(t, err)
	assert.False(t, peek.Held, "③ an expired lease is not held")
	assert.Equal(t, idA, peek.Holder)
	before := readReconcileRowIT(t, db)
	cvm := first.NowMs
	lost, err := a.AdvanceReconcile(ctx, reconcileTick(idA, func(r *store.ReconcileTickRequest) { r.SetCacheVerifiedMs = &cvm }))
	require.NoError(t, err)
	assert.Equal(t, store.ReconcileTickLeaseLost, lost.Outcome)
	frozeLost, err := a.FreezeByLeader(ctx, idA, store.RouteFreezeCacheAhead, store.RouteFreezeTimeoutMin)
	require.NoError(t, err)
	assert.Equal(t, store.ReconcileTickLeaseLost, frozeLost.Outcome)
	assert.Equal(t, before, readReconcileRowIT(t, db), "a lost lease must write nothing")
	meta, err := a.ReadRouteMeta(ctx)
	require.NoError(t, err)
	assert.False(t, meta.Frozen(), "a lost lease must not freeze the writers")

	// ④ B 接管;A 再续约被拒;A 的释放不影响 B。
	mustHoldLease(t, ctx, b, idB)
	la, err := a.RenewReconcileLease(ctx, idA, reconcileITLeaseMs)
	require.NoError(t, err)
	assert.False(t, la.HeldBy(idA), "④ the old leader must be refused after the takeover")
	assert.Equal(t, idB, la.Holder)
	released, err := a.ReleaseReconcileLease(ctx, idA)
	require.NoError(t, err)
	assert.False(t, released, "the old leader's release must not clear the new leader")
	peek, err = a.PeekReconcileLease(ctx)
	require.NoError(t, err)
	require.True(t, peek.HeldBy(idB))
	second, err := b.AdvanceReconcile(ctx, reconcileTick(idB, nil))
	require.NoError(t, err)
	require.Equal(t, store.ReconcileTickAdvanced, second.Outcome)
	assert.GreaterOrEqual(t, second.WatermarkMs, first.WatermarkMs, "the watermark never goes back across a takeover")

	// ⑤ B 释放 → 无人持有;A 可以重新获取。
	released, err = b.ReleaseReconcileLease(ctx, idB)
	require.NoError(t, err)
	assert.True(t, released)
	peek, err = a.PeekReconcileLease(ctx)
	require.NoError(t, err)
	assert.False(t, peek.Held, "⑤ released")
	assert.Empty(t, peek.Holder)
	mustHoldLease(t, ctx, a, idA)
	t.Logf("lockRetries a=%d b=%d (1213/9007 retries, recorded only)", a.LockRetriesForTest(), b.LockRetriesForTest())
}

// TestRouteReconcile_WatermarkKeyDetectsRollback:水位键水位 > 库水位(容差 0)→ writable=0、水位不动,随后 RegisterHome 返回
// ErrRouteNotWritable;容差之内照常推进;解冻把库水位抬到水位键(FloorFor)之后,同一把水位键不再触发冻结、W1 恢复。
func TestRouteReconcile_WatermarkKeyDetectsRollback(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := reconcileITCtx(t)
	id := newReconcileLeaderID(t, "it-rollback")
	mustHoldLease(t, ctx, st, id)
	meta, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)

	// ① 正常两拍:水位键 = 上一拍提交后的水位(leader 在 R27 提交之后写水位键的样子),相等不触发。
	r1, err := st.AdvanceReconcile(ctx, reconcileTick(id, nil))
	require.NoError(t, err)
	require.Equal(t, store.ReconcileTickAdvanced, r1.Outcome)
	require.Equal(t, meta.Generation, r1.Generation)
	key := &placement.RouteWatermark{V: placement.RouteWatermarkFormatVersion, Generation: r1.Generation, WatermarkMs: r1.WatermarkMs}
	r2, err := st.AdvanceReconcile(ctx, reconcileTick(id, func(r *store.ReconcileTickRequest) { r.WatermarkKey = key }))
	require.NoError(t, err)
	require.Equal(t, store.ReconcileTickAdvanced, r2.Outcome, "a watermark key equal to the DB watermark is not a rollback")
	require.GreaterOrEqual(t, r2.WatermarkMs, r1.WatermarkMs)

	// ② 容差:库水位比水位键低 500ms,容差 1000 → 照常推进。
	key = &placement.RouteWatermark{V: placement.RouteWatermarkFormatVersion, Generation: r2.Generation, WatermarkMs: r2.WatermarkMs}
	setReconcileWatermarkIT(t, db, key.WatermarkMs-500)
	r3, err := st.AdvanceReconcile(ctx, reconcileTick(id, func(r *store.ReconcileTickRequest) { r.WatermarkKey, r.ToleranceMs = key, 1000 }))
	require.NoError(t, err)
	require.Equal(t, store.ReconcileTickAdvanced, r3.Outcome, "② within the tolerance")

	// ③ 模拟库回档:库水位回到水位键之前 5s(PITR / 异步复制切主丢了一段已提交的写)。容差 0 → 冻结,水位与 cvm 都不动。
	key = &placement.RouteWatermark{V: placement.RouteWatermarkFormatVersion, Generation: r3.Generation, WatermarkMs: r3.WatermarkMs}
	rolledBack := key.WatermarkMs - 5000
	setReconcileWatermarkIT(t, db, rolledBack)
	before := readReconcileRowIT(t, db)
	cvm := r3.NowMs
	r4, err := st.AdvanceReconcile(ctx, reconcileTick(id, func(r *store.ReconcileTickRequest) {
		r.WatermarkKey, r.SetCacheVerifiedMs = key, &cvm
	}))
	var danger *store.RouteEmergencyError
	require.ErrorAs(t, err, &danger)
	require.Zero(t, r4, "失败不返回可提交结果")
	r4 = danger.Observation
	require.Equal(t, store.ReconcileTickFrozen, r4.Outcome, "③ watermark key ahead of the DB watermark is a rollback")
	assert.Equal(t, store.RouteFreezeWatermarkRegressed, r4.FreezeReason)
	assert.Equal(t, rolledBack, r4.WatermarkMs)
	assert.Equal(t, before, readReconcileRowIT(t, db), "a freeze must not touch the watermark (GREATEST would wash the evidence) nor cvm")
	observed, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	require.False(t, observed.Frozen(), "危险观察不冒充控制提交")
	frozenReceipt, err := db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	frozenStore := newMaintenanceRouteStore(t, db, frozenReceipt)
	frozen, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	require.True(t, frozen.Frozen(), "writable=0")
	assert.Equal(t, meta.Generation, frozen.Generation, "the freeze keeps the generation (deviation B1)")
	_, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 8801, HomeZone: 7})
	require.ErrorIs(t, err, authoritysql.ErrWriterDenied, "正式冻结已永久退役原boot")

	r5, err := frozenStore.AdvanceReconcile(ctx, reconcileTick(id, func(r *store.ReconcileTickRequest) { r.WatermarkKey = key }))
	require.NoError(t, err)
	assert.Equal(t, store.ReconcileTickAlreadyFrozen, r5.Outcome, "the next tick sees the freeze and writes nothing")
	assert.Equal(t, rolledBack, readReconcileWatermark(t, db))

	// ④ 出口(规格 §13.1 第 4 条):解冻把库水位抬到水位键给本世代的下限,之后同一把水位键不再触发冻结,W1 恢复。
	released, err := db.Release(freezeCtx(t), key.FloorFor(meta.Generation))
	require.NoError(t, err)
	require.Equal(t, pb.SqlWriterMaintenanceReceipt_RELEASED, released.Phase)
	st = newRouteStore(t, db)
	mustHoldLease(t, ctx, st, id)
	assert.GreaterOrEqual(t, readReconcileWatermark(t, db), key.WatermarkMs)
	r6, err := st.AdvanceReconcile(ctx, reconcileTick(id, func(r *store.ReconcileTickRequest) { r.WatermarkKey = key }))
	require.NoError(t, err)
	assert.Equal(t, store.ReconcileTickAdvanced, r6.Outcome, "④ after unfreeze the same watermark key must not refreeze")
	res, err := st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 8801, HomeZone: 7})
	require.NoError(t, err)
	assert.Equal(t, store.RouteRegisterInserted, res.Outcome)
}

// TestRouteReconcile_GenerationMismatchFreezes:水位键的世代不是本库的 → writable=0(库被重置或换了一份库);水位、cvm、generation 都不动。
func TestRouteReconcile_GenerationMismatchFreezes(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := reconcileITCtx(t)
	id := newReconcileLeaderID(t, "it-generation")
	mustHoldLease(t, ctx, st, id)
	meta, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, meta.Generation)

	// 另一个世代:把本库世代的首字符换成另一个十六进制字符(仍是合法的 32 个 [0-9a-f])。
	other := []byte(meta.Generation)
	if other[0] == 'f' {
		other[0] = '0'
	} else {
		other[0] = 'f'
	}
	key := &placement.RouteWatermark{V: placement.RouteWatermarkFormatVersion, Generation: string(other), WatermarkMs: 1}

	before := readReconcileRowIT(t, db)
	res, err := st.AdvanceReconcile(ctx, reconcileTick(id, func(r *store.ReconcileTickRequest) { r.WatermarkKey = key }))
	var danger *store.RouteEmergencyError
	require.ErrorAs(t, err, &danger)
	require.Zero(t, res)
	res = danger.Observation
	require.Equal(t, store.ReconcileTickFrozen, res.Outcome)
	assert.Equal(t, store.RouteFreezeGenerationMismatch, res.FreezeReason)
	assert.Equal(t, meta.Generation, res.Generation)
	assert.Equal(t, before, readReconcileRowIT(t, db), "a freeze writes neither the watermark nor cvm")
	observed, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	require.False(t, observed.Frozen(), "观察必须零控制DML")
	frozenReceipt, err := db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	frozenStore := newMaintenanceRouteStore(t, db, frozenReceipt)
	after, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	assert.True(t, after.Frozen())
	assert.Equal(t, meta.Generation, after.Generation, "the leader never changes the generation")
	_, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 8901, HomeZone: 7})
	require.ErrorIs(t, err, authoritysql.ErrWriterDenied)
	again, err := frozenStore.FreezeByLeader(ctx, id, store.RouteFreezeCacheAhead, store.RouteFreezeTimeoutMin)
	require.NoError(t, err)
	assert.Equal(t, store.ReconcileTickAlreadyFrozen, again.Outcome)
}

// TestRouteReconcile_FreezeByLeaderCacheAhead:持有租约的 leader 冻结(cache_ahead)→ writable=0、水位不动;再冻结是 AlreadyFrozen;
// 别的标识(不持有租约)冻结是 LeaseLost、零写入。
func TestRouteReconcile_FreezeByLeaderCacheAhead(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := reconcileITCtx(t)
	id := newReconcileLeaderID(t, "it-cache-ahead")
	mustHoldLease(t, ctx, st, id)

	outsider, err := st.FreezeByLeader(ctx, newReconcileLeaderID(t, "it-outsider"), store.RouteFreezeCacheAhead, store.RouteFreezeTimeoutMin)
	require.NoError(t, err)
	assert.Equal(t, store.ReconcileTickLeaseLost, outsider.Outcome)
	meta, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	require.False(t, meta.Frozen(), "a non-leader freeze writes nothing")

	before := readReconcileRowIT(t, db)
	res, err := st.FreezeByLeader(ctx, id, store.RouteFreezeCacheAhead, store.RouteFreezeTimeoutMin)
	var danger *store.RouteEmergencyError
	require.ErrorAs(t, err, &danger)
	require.Zero(t, res)
	res = danger.Observation
	require.Equal(t, store.ReconcileTickFrozen, res.Outcome)
	assert.Equal(t, store.RouteFreezeCacheAhead, res.FreezeReason)
	assert.Equal(t, before, readReconcileRowIT(t, db))
	observed, err := st.ReadRouteMeta(ctx)
	require.NoError(t, err)
	require.False(t, observed.Frozen(), "观察必须零控制DML")
	frozenReceipt, err := db.Freeze(freezeCtx(t))
	require.NoError(t, err)
	frozenStore := newMaintenanceRouteStore(t, db, frozenReceipt)
	_, err = st.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 9001, HomeZone: 7})
	require.ErrorIs(t, err, authoritysql.ErrWriterDenied)
	again, err := frozenStore.FreezeByLeader(ctx, id, store.RouteFreezeCacheAhead, store.RouteFreezeTimeoutMin)
	require.NoError(t, err)
	assert.Equal(t, store.ReconcileTickAlreadyFrozen, again.Outcome)
}

// TestRouteReconcile_WatermarkMonotonic:水位只增不减且不低于库时钟;解冻抬过的、比库时钟还大的水位不会被拉回
// (这一拍值不变、R27 受影响 0 行,仍判 Advanced);cvm 给定才写、按给定值精确写、可以变小,不给定不动,超过库时钟按库时钟写。
func TestRouteReconcile_WatermarkMonotonic(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := reconcileITCtx(t)
	id := newReconcileLeaderID(t, "it-monotonic")
	mustHoldLease(t, ctx, st, id)

	lo := readDBNowMs(t, db)
	now, err := st.DBNowMs(ctx)
	require.NoError(t, err)
	hi := readDBNowMs(t, db)
	assert.GreaterOrEqual(t, now, lo, "DBNowMs is the database clock")
	assert.LessOrEqual(t, now, hi, "DBNowMs is the database clock")

	var last uint64
	for i := 0; i < 5; i++ {
		res, err := st.AdvanceReconcile(ctx, reconcileTick(id, nil))
		require.NoError(t, err)
		require.Equal(t, store.ReconcileTickAdvanced, res.Outcome)
		require.GreaterOrEqual(t, res.WatermarkMs, last, "tick %d: the watermark only grows", i)
		require.GreaterOrEqual(t, res.WatermarkMs, res.NowMs, "tick %d: the watermark reaches the database clock", i)
		require.Equal(t, res.WatermarkMs, readReconcileWatermark(t, db), "tick %d: the result is what got committed", i)
		last = res.WatermarkMs
	}

	future := last + 3_600_000
	setReconcileWatermarkIT(t, db, future)
	res, err := st.AdvanceReconcile(ctx, reconcileTick(id, nil))
	require.NoError(t, err)
	require.Equal(t, store.ReconcileTickAdvanced, res.Outcome, "an unchanged row (0 affected rows) still advances")
	assert.Equal(t, future, res.WatermarkMs)
	assert.Equal(t, future, readReconcileWatermark(t, db), "GREATEST never pulls a raised watermark back")

	tick := func(cvm *uint64) store.ReconcileTickResult {
		t.Helper()
		got, err := st.AdvanceReconcile(ctx, reconcileTick(id, func(r *store.ReconcileTickRequest) { r.SetCacheVerifiedMs = cvm }))
		require.NoError(t, err)
		require.Equal(t, store.ReconcileTickAdvanced, got.Outcome)
		require.Equal(t, got.CacheVerifiedMs, readReconcileRowIT(t, db).CacheVerifiedMs, "the result is what got committed")
		return got
	}
	cvm1 := res.NowMs - 1000
	assert.Equal(t, cvm1, tick(&cvm1).CacheVerifiedMs, "cvm is written exactly as given")
	cvm2 := cvm1 - 4000
	assert.Equal(t, cvm2, tick(&cvm2).CacheVerifiedMs, "cvm may go down (after an identity change it measures the new instance)")
	assert.Equal(t, cvm2, tick(nil).CacheVerifiedMs, "cvm is left alone when not given")
	ahead := res.NowMs + 3_600_000
	clamped := tick(&ahead)
	assert.LessOrEqual(t, clamped.CacheVerifiedMs, clamped.NowMs, "a cvm ahead of the database clock is written as the clock")
}

// TestRouteReconcile_TickAndUnfreezeNoDeadlock:并发的 leader 每拍 / leader 冻结(route_reconcile → route_meta)、
// route_tool 冻结 / 解冻(R19;R20 → R18 → R21 → R22,同向)、W1 建角(只普通读 route_meta)。锁序同向就不成环:
// 每次调用都不该报错,1213 / 9007 的重来次数(lockRetries)断言为 0 —— 只看「最终结果正确」分不清是真的不成环,
// 还是环被重试悄悄吸收了。
func TestRouteReconcile_TickAndUnfreezeNoDeadlock(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	ctx := reconcileITCtx(t)
	id := newReconcileLeaderID(t, "it-concurrent")
	const rounds = 40
	var mu sync.Mutex
	var failures []string
	outcomes := map[string]int{}
	var leaderRetries, w1Retries uint64
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	count := func(label string) { mu.Lock(); defer mu.Unlock(); outcomes[label]++ }
	// 每轮必须采用新boot；该SQL夹具没有外部队列，不把本用例当作生产非SQL排空验收。
	for round := 0; round < rounds; round++ {
		leaderStore, w1Store := newRouteStore(t, db), newRouteStore(t, db)
		mustHoldLease(t, ctx, leaderStore, id)
		floor, err := leaderStore.DBNowMs(ctx)
		require.NoError(t, err)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			res, err := leaderStore.AdvanceReconcile(ctx, reconcileTick(id, nil))
			switch {
			case errors.Is(err, authoritysql.ErrWriterDenied):
				count("tick_retired")
			case err != nil:
				fail("tick %d: %v", round, err)
			case res.Outcome == store.ReconcileTickAdvanced || res.Outcome == store.ReconcileTickAlreadyFrozen:
				count("tick_" + res.Outcome.String())
			default:
				fail("tick %d: unexpected %s", round, res.Outcome)
			}
			if round%4 != 3 {
				return
			}
			fr, err := leaderStore.FreezeByLeader(ctx, id, store.RouteFreezeCacheAhead, store.RouteFreezeTimeoutMin)
			var danger *store.RouteEmergencyError
			switch {
			case errors.Is(err, authoritysql.ErrWriterDenied):
				count("leader_retired")
			case errors.As(err, &danger):
				if danger.Observation.FreezeReason != store.RouteFreezeCacheAhead {
					fail("危险原因不符")
				}
				count("leader_danger")
			case err != nil:
				fail("leader danger %d: %v", round, err)
			case fr.Outcome == store.ReconcileTickAlreadyFrozen:
				count("leader_already_frozen")
			default:
				fail("leader danger %d: unexpected %s", round, fr.Outcome)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			res, err := w1Store.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 950001 + uint64(round), HomeZone: 7})
			switch {
			case errors.Is(err, authoritysql.ErrWriterDenied) || errors.Is(err, store.ErrRouteNotWritable):
				count("w1_not_writable")
			case err != nil:
				fail("W1 %d: %v", round, err)
			default:
				count("w1_" + res.Outcome.String())
			}
		}()
		close(start)
		_, freezeErr := db.Freeze(freezeCtx(t))
		wg.Wait()
		if freezeErr != nil {
			t.Fatalf("正式控制器冻结 %d: %v", round, freezeErr)
		}
		_, err = db.Release(freezeCtx(t), floor+1)
		require.NoError(t, err)
		_, err = w1Store.RegisterHome(ctx, store.RouteRegisterRequest{PlayerID: 960001 + uint64(round), HomeZone: 7})
		require.ErrorIs(t, err, authoritysql.ErrWriterDenied, "释放后旧池也不得复活")
		leaderRetries += leaderStore.LockRetriesForTest()
		w1Retries += w1Store.LockRetriesForTest()
		require.NoError(t, leaderStore.Close())
		require.NoError(t, w1Store.Close())
	}
	t.Logf("outcomes=%v lockRetries leader=%d w1=%d", outcomes, leaderRetries, w1Retries)
	require.Empty(t, failures, "并发tick/正式控制器/W1不能形成锁环")
	assert.Zero(t, leaderRetries, "新的统一锁顺序不应触发死锁重试")
	assert.Zero(t, w1Retries, "W1与正式控制器不应死锁")
	last := newRouteStore(t, db)
	peek, err := last.PeekReconcileLease(ctx)
	require.NoError(t, err)
	assert.True(t, peek.HeldBy(id), "控制器不夺取对账租约")
}

func seedRouteRowUpdatedAtIT(t *testing.T, db *storetest.DB, player uint64, updatedAtMs uint64) {
	t.Helper()
	_, err := db.Raw.Exec(
		"INSERT INTO player_route (route_shard, player_id, home_zone, rev, updated_by, created_at_ms, updated_at_ms)"+
			" VALUES (?, ?, 7, 1, 'it-seed', 1, ?)",
		uint64(placement.RouteRowShard(player)), player, updatedAtMs)
	require.NoError(t, err)
}

// TestScanRoutesUpdatedSince_UsesIdxRouteUpdated:R29 在一个行片里按 (updated_at_ms, player_id) 键集翻页无漏无重(含并列的
// updated_at_ms)、起点「>= since」、不串到别的片;计划的候选索引里有 idx_route_updated,完整计划原样留档。
func TestScanRoutesUpdatedSince_UsesIdxRouteUpdated(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := reconcileITCtx(t)

	// 片 5:5、69、133、197、261(64 片下都 ≡ 5),updated_at_ms 有两组并列;片 6:6、70 是干扰项。
	for _, r := range []struct{ player, updatedAt uint64 }{
		{261, 1500}, {5, 1000}, {197, 1500}, {69, 1000}, {133, 1200}, {6, 1100}, {70, 1300},
	} {
		seedRouteRowUpdatedAtIT(t, db, r.player, r.updatedAt)
	}

	scanAll := func(shard uint32, from store.RouteUpdatedCursor, page int) []store.RouteUpdatedCursor {
		t.Helper()
		var out []store.RouteUpdatedCursor
		cursor := from
		for pages := 0; ; pages++ {
			require.Less(t, pages, 20, "pagination does not terminate")
			rows, err := st.ScanRoutesUpdatedSince(ctx, shard, cursor, page)
			require.NoError(t, err)
			for _, row := range rows {
				out = append(out, store.RouteUpdatedCursorAt(row))
			}
			if len(rows) < page {
				return out
			}
			cursor = store.RouteUpdatedCursorAt(rows[len(rows)-1])
		}
	}

	at := func(updatedAtMs, player uint64) store.RouteUpdatedCursor {
		return store.RouteUpdatedCursor{UpdatedAtMs: updatedAtMs, PlayerID: player}
	}
	assert.Equal(t, []store.RouteUpdatedCursor{at(1000, 5), at(1000, 69), at(1200, 133), at(1500, 197), at(1500, 261)},
		scanAll(5, store.RouteUpdatedSince(1000), 2), "shard 5 from >= 1000, two rows per page across ties")
	assert.Equal(t, []store.RouteUpdatedCursor{at(1200, 133), at(1500, 197), at(1500, 261)},
		scanAll(5, store.RouteUpdatedSince(1001), 10), "the start is >= since")
	assert.Equal(t, []store.RouteUpdatedCursor{at(1100, 6), at(1300, 70)},
		scanAll(6, store.RouteUpdatedSince(0), 10), "only rows of the requested shard")

	stmt := inlineRouteArgs(t, store.RouteScanUpdatedSinceSQLForTest, []uint64{5, 1000, 1000, 69, 2})
	if plan, ok := explainUnlessTiDB(t, db, stmt); ok {
		t.Logf("R29 plan: %v", plan)
		assert.Contains(t, plan["possible_keys"], "idx_route_updated", "R29 must be able to use idx_route_updated")
	}
}

// TestRouteReconcile_ListMergeFenceZones(核实意见 A1-4 / LR-1):R31 列出 63 号片上**有行**的全部 zone,按 zone_id 升序 ——
// 生效、已过期、未封锁的空行都算(本方案写过镜像的 zone 都在里面);只在别的片有行的 zone 不算。没有行回空。
func TestRouteReconcile_ListMergeFenceZones(t *testing.T) {
	requireReadsIT(t)
	db := storetest.NewMigratedDB(t)
	st := newRouteStore(t, db)
	ctx := reconcileITCtx(t)
	const run = "71-72-20261020T000000Z-0a1b2c3d"
	readShard := uint32(store.RouteFenceReadShardForTest)

	empty, err := st.ListMergeFenceZones(ctx)
	require.NoError(t, err)
	assert.Empty(t, empty)

	// 72 生效、71 已过期、73 是未封锁的空行(都在 63 号片,故意乱序插入);74 只在 0 号片有行。
	seedReadFence(t, db, 72, readShard, run, 3_600_000, 2)
	seedReadFence(t, db, 71, readShard, run, -3_600_000, 1)
	seedReadFence(t, db, 73, readShard, "", 0, 0)
	seedReadFence(t, db, 74, 0, run, 3_600_000, 1)

	got, err := st.ListMergeFenceZones(ctx)
	require.NoError(t, err)
	assert.Equal(t, []uint32{71, 72, 73}, got)
}
