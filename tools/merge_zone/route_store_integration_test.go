//go:build merge_integration

package main

// 路由写入、发布与围栏的真实TiDB/Redis回归，共用TestMain的受控本机TiDB独占route库。
// DDL从DS权威源加载，不依赖用户预建外置路由库；依赖缺失遵循MERGE_ZONE_IT_REQUIRE门禁。
// 发布使用真实WAITAOF 1 0，不改Redis配置。
import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	itRouteZoneA = uint32(981)
	itRouteZoneB = uint32(982)
	itRouteRunA  = "981-982-20261019T000000Z-aaaaaaaa"
	itRouteRunB  = "981-982-20261019T000001Z-bbbbbbbb"
)

// itRoutePlayers 落在不同行片上:9_000_000_448 % 64 = 0,…449 → 1,…511 → 63。
var itRoutePlayers = []uint64{9_000_000_448, 9_000_000_449, 9_000_000_511}

// itRouteNoRow 是一个不在库里的玩家(RemapHomeBatch 判 missing)。
const itRouteNoRow = uint64(9_000_000_450)

// itRouteStore打开本套独占TiDB路由库，跑当前强读及P1–P5真实探针；无法打开直接失败。
func itRouteStore(t *testing.T) *mergeRouteStore {
	t.Helper()
	raw := itRouteDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, meta, err := openMergeRouteStore(ctx, raw, newMergeRouteWriter("merge-zone-it", itRouteRunA))
	if err != nil {
		t.Fatalf("open the route DB of %s (probes P1-P5): %v", itRouteDBEnv, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if meta.Writable != 1 {
		t.Fatalf("route_meta.writable=%d on the throwaway route DB; these cases need writable route tables", meta.Writable)
	}
	itRouteCleanup(t, st)
	t.Cleanup(func() { itRouteCleanup(t, st) })
	return st
}

// itRouteCleanup 删掉本组用例写过的东西(只动 zone 981 / 982 与本组的玩家 id)。测试代码直接写表:AST 守卫不扫 _test.go。
func itRouteCleanup(t *testing.T, st *mergeRouteStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, "DELETE FROM zone_merge_fence WHERE zone_id IN (?, ?)", uint64(itRouteZoneA), uint64(itRouteZoneB)); err != nil {
		t.Fatalf("cleanup zone_merge_fence: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, "DELETE FROM zone_merged_into WHERE src_zone IN (?, ?)", uint64(itRouteZoneA), uint64(itRouteZoneB)); err != nil {
		t.Fatalf("cleanup zone_merged_into: %v", err)
	}
	for _, id := range append(append([]uint64(nil), itRoutePlayers...), itRouteNoRow) {
		if _, err := st.db.ExecContext(ctx, "DELETE FROM player_route WHERE route_shard = ? AND player_id = ?", routeShardArg(id), id); err != nil {
			t.Fatalf("cleanup player_route %d: %v", id, err)
		}
	}
}

// itSeedRoutes 给玩家插只有 home 的行(rev = 1)。
func itSeedRoutes(t *testing.T, st *mergeRouteStore, home uint32, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		if _, err := st.db.ExecContext(context.Background(),
			"INSERT INTO player_route (route_shard, player_id, home_zone, rev, updated_by, created_at_ms, updated_at_ms) VALUES (?, ?, ?, 1, 'merge-zone-it', 0, 0)",
			routeShardArg(id), id, uint64(home)); err != nil {
			t.Fatalf("seed player_route %d: %v", id, err)
		}
	}
}

// itRouteRedis 是 mapping Redis(DB 9):先清空;不支持 WAITAOF 1 0 → SKIP / REQUIRE 时 FAIL。
func itRouteRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := itRedis(t, itMappingRD)
	ctx := context.Background()
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis db %d: %v", itMappingRD, err)
	}
	t.Cleanup(func() { _ = rdb.FlushDB(context.Background()).Err() })
	if err := rdb.Do(ctx, "WAITAOF", 1, 0, 100).Err(); err != nil {
		itMissing(t, fmt.Sprintf("WAITAOF 1 0 on %s failed (%v): the route publish cases need Redis >= 7.2 with appendonly yes", itRedisAddr, err))
	}
	return rdb
}

func itRouteFence(run, purpose string) routeFenceAcquire {
	return routeFenceAcquire{Zones: []uint32{itRouteZoneA, itRouteZoneB}, RunID: run, Purpose: purpose, Src: itRouteZoneA, Dst: itRouteZoneB,
		Holder: mergeInProgressValue{Operator: "merge-zone-it"}, TTL: time.Hour}
}

func itRouteDurability(t *testing.T) routeDurability {
	t.Helper()
	dur, err := newRouteDurability(1, 0, 1500)
	if err != nil {
		t.Fatal(err)
	}
	return dur
}

// TestIT_Route_FenceMutualExclusionRenewAndRelease:获取互斥、同 run 重入 epoch +1、续期、按库时钟过期即失权(续期与批事务都拒)、
// 过期后别的运行能接手、释放只清自己的分片且 fence_epoch 保留。
func TestIT_Route_FenceMutualExclusionRenewAndRelease(t *testing.T) {
	requireIT(t)
	st := itRouteStore(t)
	ctx := context.Background()
	zones := []uint32{itRouteZoneA, itRouteZoneB}

	g1, err := st.AcquireFence(ctx, itRouteFence(itRouteRunA, routeFencePurposeMerge))
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	if _, err := st.AcquireFence(ctx, itRouteFence(itRouteRunB, routeFencePurposeMerge)); !errors.Is(err, errRouteFenceHeld) {
		t.Fatalf("acquire B while A holds the fence: %v, want errRouteFenceHeld", err)
	}
	g2, err := st.AcquireFence(ctx, itRouteFence(itRouteRunA, routeFencePurposeMerge))
	if err != nil || g2.Epoch != g1.Epoch+1 {
		t.Fatalf("re-acquire A = (%+v, %v), want epoch %d", g2, err, g1.Epoch+1)
	}
	state, err := st.ReadFence(ctx, itRouteZoneB)
	if err != nil || !state.Active || state.RunID != itRouteRunA || state.Epoch != g2.Epoch || state.HolderJSON != g2.HolderJSON ||
		state.ExpiresAtMs != g2.ExpiresAtMs {
		t.Fatalf("fence state = (%+v, %v), grant %+v", state, err, g2)
	}
	exp, err := st.RenewFence(ctx, zones, itRouteRunA, 2*time.Hour)
	if err != nil || exp[itRouteZoneA] <= g2.ExpiresAtMs || exp[itRouteZoneB] <= g2.ExpiresAtMs {
		t.Fatalf("renew = (%v, %v), want later than %d", exp, err, g2.ExpiresAtMs)
	}

	// 按库时钟过期:续期是确定失权,批事务的围栏复核也拒绝。
	if _, err := st.db.ExecContext(ctx, "UPDATE zone_merge_fence SET expires_at_ms = 1 WHERE zone_id IN (?, ?)",
		uint64(itRouteZoneA), uint64(itRouteZoneB)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RenewFence(ctx, zones, itRouteRunA, time.Hour); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("renew after expiry: %v, want errRouteFenceLost", err)
	}
	itSeedRoutes(t, st, itRouteZoneA, itRoutePlayers[0])
	if _, err := st.RemapHomeBatch(ctx, itRouteZoneA, itRouteZoneB, itRouteRunA, itRoutePlayers[:1]); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("batch under an expired fence: %v, want errRouteFenceLost", err)
	}

	g3, err := st.AcquireFence(ctx, itRouteFence(itRouteRunB, routeFencePurposeMerge))
	if err != nil || g3.Epoch != g2.Epoch+1 {
		t.Fatalf("acquire B after A expired = (%+v, %v)", g3, err)
	}
	released, err := st.ReleaseFence(ctx, zones, itRouteRunA)
	if err != nil || released[itRouteZoneA] != 0 || released[itRouteZoneB] != 0 {
		t.Fatalf("release by the stale run A = (%v, %v), want nothing released", released, err)
	}
	released, err = st.ReleaseFence(ctx, zones, itRouteRunB)
	if err != nil || released[itRouteZoneA] != int64(routeFenceShards) || released[itRouteZoneB] != int64(routeFenceShards) {
		t.Fatalf("release by B = (%v, %v), want 64 shards each", released, err)
	}
	state, err = st.ReadFence(ctx, itRouteZoneA)
	if err != nil || state.Active || state.RunID != "" || state.Epoch != g3.Epoch {
		t.Fatalf("released fence = (%+v, %v), want inactive with fence_epoch kept at %d", state, err, g3.Epoch)
	}
}

// TestIT_Route_TakeOverChecksZonePairAndManifest(MZ2-2,真库):接手核对读的是库里真实的 src_zone / dst_zone 列与 holder_json 的
// manifest_path —— zone 对写反、清单路径不同都 errRouteInvalidArgument,事务不写,围栏原样留在旧 run_id 上(epoch 不变);
// 同一份清单(路径写法不同、规整后相同)照常接手,epoch + 1。
func TestIT_Route_TakeOverChecksZonePairAndManifest(t *testing.T) {
	requireIT(t)
	st := itRouteStore(t)
	ctx := context.Background()
	const manifest = "merge_981_to_982_it.json"
	withManifest := func(req routeFenceAcquire, path string) routeFenceAcquire {
		req.Holder.ManifestPath = path
		return req
	}
	g1, err := st.AcquireFence(ctx, withManifest(itRouteFence(itRouteRunA, routeFencePurposeMerge), manifest))
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	reversed := withManifest(itRouteFence(itRouteRunB, routeFencePurposeMerge), manifest)
	reversed.Src, reversed.Dst = itRouteZoneB, itRouteZoneA
	for _, tc := range []struct {
		name string
		req  routeFenceAcquire
	}{
		{"reversed-zone-pair", reversed},
		{"other-manifest", withManifest(itRouteFence(itRouteRunB, routeFencePurposeMerge), "merge_981_to_982_other.json")},
	} {
		if _, err := st.TakeOverFence(ctx, tc.req, itRouteRunA); !errors.Is(err, errRouteInvalidArgument) {
			t.Fatalf("%s: take over = %v, want errRouteInvalidArgument", tc.name, err)
		}
		for _, zone := range []uint32{itRouteZoneA, itRouteZoneB} {
			state, err := st.ReadFence(ctx, zone)
			if err != nil || !state.Active || state.RunID != itRouteRunA || state.Epoch != g1.Epoch {
				t.Fatalf("%s: zone %d fence = (%+v, %v), want A's fence untouched at epoch %d", tc.name, zone, state, err, g1.Epoch)
			}
		}
	}
	g2, err := st.TakeOverFence(ctx, withManifest(itRouteFence(itRouteRunB, routeFencePurposeUnmerge), "./"+manifest), itRouteRunA)
	if err != nil || g2.Epoch != g1.Epoch+1 {
		t.Fatalf("take over with the same manifest (unmerge after a merge) = (%+v, %v), want epoch %d", g2, err, g1.Epoch+1)
	}
	state, err := st.ReadFence(ctx, itRouteZoneB)
	if err != nil || !state.Active || state.RunID != itRouteRunB || state.Epoch != g2.Epoch {
		t.Fatalf("fence after the take-over = (%+v, %v), want B at epoch %d", state, err, g2.Epoch)
	}
}

// TestIT_Route_RemapHomeBatchThenPublish:批事务改 home → 发布 + WAITAOF;重跑走「已是目标」分支、发布回 Confirmed;
// catch-up 认得出缓存已是这一版。
func TestIT_Route_RemapHomeBatchThenPublish(t *testing.T) {
	requireIT(t)
	st := itRouteStore(t)
	rdb := itRouteRedis(t)
	ctx := context.Background()
	itSeedRoutes(t, st, itRouteZoneA, itRoutePlayers...)
	if _, err := st.AcquireFence(ctx, itRouteFence(itRouteRunA, routeFencePurposeMerge)); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ids := append(append([]uint64(nil), itRoutePlayers...), itRouteNoRow)

	res, err := st.RemapHomeBatch(ctx, itRouteZoneA, itRouteZoneB, itRouteRunA, ids)
	if err != nil {
		t.Fatalf("RemapHomeBatch: %v", err)
	}
	if res.Changed != len(itRoutePlayers) || res.Verdicts[itRouteNoRow] != mappingMissing || len(res.Target) != len(itRoutePlayers) {
		t.Fatalf("remap = %+v", res)
	}
	for _, row := range res.Target {
		if row.HomeZone != itRouteZoneB || row.Rev != 2 {
			t.Fatalf("target row = %+v, want home %d rev 2", row, itRouteZoneB)
		}
	}
	pub := newRoutePublisher(rdb, itRouteDurability(t))
	rep, err := pub.publishRoutes(ctx, res.Target)
	if err != nil || rep.Applied != len(itRoutePlayers) {
		t.Fatalf("publish = (%s, %v)", rep, err)
	}
	for _, id := range itRoutePlayers {
		home, _ := rdb.Get(ctx, playerZoneKey(id)).Result()
		rev, _ := rdb.Get(ctx, mapRevKey(id)).Result()
		n, _ := rdb.Exists(ctx, placementKey(id)).Result()
		if home != "982" || rev != "2" || n != 0 {
			t.Fatalf("player %d cache = home %q maprev %q placement exists %d", id, home, rev, n)
		}
	}

	again, err := st.RemapHomeBatch(ctx, itRouteZoneA, itRouteZoneB, itRouteRunA, ids)
	if err != nil || again.Changed != 0 || !reflect.DeepEqual(again.Target, res.Target) {
		t.Fatalf("re-run = (%+v, %v), want the same target rows and nothing changed", again, err)
	}
	rep, err = pub.publishRoutes(ctx, again.Target)
	if err != nil || rep.Confirmed != len(itRoutePlayers) {
		t.Fatalf("re-publish = (%s, %v), want all confirmed", rep, err)
	}
	cu, err := pub.catchUpRoutes(ctx, again.Target)
	if err != nil || cu.Current != len(itRoutePlayers) || cu.Published.Rows != 0 {
		t.Fatalf("catch-up = (%+v, %v), want every row current", cu, err)
	}
	if _, err := st.ReleaseFence(ctx, []uint32{itRouteZoneA, itRouteZoneB}, itRouteRunA); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// itClientIDSandwich 在每条 pipeline 的前后各塞一条 CLIENT ID:两者相等 ⇔ 整批(EVAL… 与 WAITAOF)走的是同一条连接。
type itClientIDSandwich struct {
	mu    sync.Mutex
	names [][]string
	ids   [][2]int64
}

func (h *itClientIDSandwich) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *itClientIDSandwich) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (h *itClientIDSandwich) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		before := redis.NewIntCmd(ctx, "client", "id")
		after := redis.NewIntCmd(ctx, "client", "id")
		err := next(ctx, append(append([]redis.Cmder{before}, cmds...), after))
		var names []string
		for _, c := range cmds {
			names = append(names, c.Name())
		}
		h.mu.Lock()
		h.names = append(h.names, names)
		h.ids = append(h.ids, [2]int64{before.Val(), after.Val()})
		h.mu.Unlock()
		return err
	}
}

// TestIT_Route_PublishWaitAOFSameConnection:发布的 EVAL 与 WAITAOF 在同一条 pipeline、同一条连接上(CLIENT ID 夹心),
// 规格 §3 表第 4 行的取证用例(data_service 侧对应 TestPublishRoute_RealRedisSameConnection)。
func TestIT_Route_PublishWaitAOFSameConnection(t *testing.T) {
	requireIT(t)
	rdb := itRouteRedis(t)
	hook := &itClientIDSandwich{}
	rdb.AddHook(hook)
	pub := newRoutePublisher(rdb, itRouteDurability(t))
	rows := []mergeRouteRow{
		{PlayerID: itRoutePlayers[0], HomeZone: itRouteZoneB, Rev: 3},
		{PlayerID: itRoutePlayers[1], HomeZone: itRouteZoneB, StorageID: itRouteZoneA, PlacementVersion: 1, Rev: 4},
	}
	rep, err := pub.publishRoutes(context.Background(), rows)
	if err != nil || rep.Applied != 2 {
		t.Fatalf("publish = (%s, %v)", rep, err)
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if len(hook.names) != 1 || !reflect.DeepEqual(hook.names[0], []string{"eval", "eval", "waitaof"}) {
		t.Fatalf("pipelines = %v, want one [eval eval waitaof]", hook.names)
	}
	if ids := hook.ids[0]; ids[0] == 0 || ids[0] != ids[1] {
		t.Fatalf("CLIENT ID before / after the batch = %v: EVAL and WAITAOF did not share one connection", ids)
	}
	if plc, _ := rdb.Get(context.Background(), placementKey(itRoutePlayers[1])).Result(); plc != "981:1" {
		t.Fatalf("placement after publish = %q, want 981:1", plc)
	}
}

// TestIT_Route_PairingAgainstTheWatermarkKey:「库与 Redis 成对」核对(规格 §13.3 第 3 条)对着真库的 route_meta.generation。
func TestIT_Route_PairingAgainstTheWatermarkKey(t *testing.T) {
	requireIT(t)
	st := itRouteStore(t)
	rdb := itRouteRedis(t)
	ctx := context.Background()
	pub := newRoutePublisher(rdb, itRouteDurability(t))
	writer := newMergeRouteWriter("merge-zone-it", itRouteRunA)

	if got, err := openRouteBackend(ctx, routeSourceRedis, "", writer, pub); err != nil || got != nil {
		t.Fatalf("redis mode without a watermark = (%v, %v)", got, err)
	}
	meta, err := readRouteMeta(ctx, st.db)
	if err != nil {
		t.Fatalf("read route_meta: %v", err)
	}
	if err := rdb.Set(ctx, routeWatermarkKey, `{"v":1,"generation":"`+meta.Generation+`","watermark_ms":42}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := openRouteBackend(ctx, routeSourceRedis, "", writer, pub); !errors.Is(err, errRoutePairing) {
		t.Fatalf("redis mode with a watermark: %v, want errRoutePairing", err)
	}
	paired, err := openRouteBackend(ctx, routeSourceDB, itRouteDSN(), writer, pub)
	if err != nil || paired == nil {
		t.Fatalf("db mode with a matching generation = (%v, %v)", paired, err)
	}
	_ = paired.Close()
	if err := rdb.Set(ctx, routeWatermarkKey, `{"v":1,"generation":"ff","watermark_ms":42}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := openRouteBackend(ctx, routeSourceDB, itRouteDSN(), writer, pub); !errors.Is(err, errRoutePairing) {
		t.Fatalf("db mode with another generation: %v, want errRoutePairing", err)
	}
}

// TestIT_Route_MergedIntoBlocksBackfillUntilCleared:W6 / W9 / 5'a 的库内语义 —— 围栏生效时回填拒绝;合走标记在库里时回填拒绝
// (Redis 丢了也拦得住);撤销按 (src, dst, run_id) 删掉标记之后回填照常插入。
func TestIT_Route_MergedIntoBlocksBackfillUntilCleared(t *testing.T) {
	requireIT(t)
	st := itRouteStore(t)
	ctx := context.Background()
	zones := []uint32{itRouteZoneA, itRouteZoneB}
	id := itRoutePlayers[2]

	if _, err := st.AcquireFence(ctx, itRouteFence(itRouteRunA, routeFencePurposeMerge)); err != nil {
		t.Fatalf("acquire merge fence: %v", err)
	}
	value, err := st.MarkMergedInto(ctx, itRouteZoneA, itRouteZoneB, itRouteRunA, itRouteRunA)
	if err != nil || value != `{"dst":982,"run_id":"`+itRouteRunA+`"}` {
		t.Fatalf("MarkMergedInto = (%q, %v)", value, err)
	}
	if _, err := st.BackfillHomeBatch(ctx, itRouteZoneA, []uint64{id}); !errors.Is(err, errRouteZoneFenced) {
		t.Fatalf("backfill under the fence: %v, want errRouteZoneFenced", err)
	}
	if _, err := st.ReleaseFence(ctx, zones, itRouteRunA); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := st.BackfillHomeBatch(ctx, itRouteZoneA, []uint64{id}); !errors.Is(err, errRouteZoneMergedAway) {
		t.Fatalf("backfill of a merged-away zone: %v, want errRouteZoneMergedAway", err)
	}

	if _, err := st.AcquireFence(ctx, itRouteFence(itRouteRunB, routeFencePurposeUnmerge)); err != nil {
		t.Fatalf("acquire unmerge fence: %v", err)
	}
	if deleted, err := st.ClearMergedInto(ctx, itRouteZoneA, itRouteZoneB, itRouteRunB, "981-982-20200101T000000Z-00000000"); err != nil || deleted {
		t.Fatalf("clear with another merge run id = (%v, %v), want nothing deleted", deleted, err)
	}
	if deleted, err := st.ClearMergedInto(ctx, itRouteZoneA, itRouteZoneB, itRouteRunB, itRouteRunA); err != nil || !deleted {
		t.Fatalf("clear = (%v, %v), want deleted", deleted, err)
	}
	if _, err := st.ReleaseFence(ctx, zones, itRouteRunB); err != nil {
		t.Fatalf("release unmerge fence: %v", err)
	}
	res, err := st.BackfillHomeBatch(ctx, itRouteZoneA, []uint64{id})
	if err != nil || res.Verdicts[id] != routeBackfillInserted || len(res.Target) != 1 || res.Target[0].HomeZone != itRouteZoneA {
		t.Fatalf("backfill after unmerge = (%+v, %v)", res, err)
	}
	rows, err := st.ReadRoutes(ctx, []uint64{id})
	if err != nil || rows[id].HomeZone != itRouteZoneA || rows[id].Rev != 1 {
		t.Fatalf("ReadRoutes after backfill = (%v, %v)", rows, err)
	}
}
