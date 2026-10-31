//go:build merge_integration

package main

// 路由库只读查询、「库与 Redis 成对」与镜像写入的真实TiDB/Redis回归,共用TestMain的受控本机TiDB独占route库。
// DDL从DS权威源加载,不依赖用户预建外置路由库;依赖缺失遵循MERGE_ZONE_IT_REQUIRE门禁。
// zone_merged_into 的写要真实冻结,在 integration_test.go 的 TestIT_SQLDomainMergedIntoRoundTrip。
import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"shared/authoritysql"
)

// itRouteStore 打开本套独占TiDB路由库并跑只读探针;无法打开直接失败。
func itRouteStore(t *testing.T) *mergeRouteStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, _, err := openMergeRouteStore(ctx, itRouteDSN(), authoritysql.BackendTiDB)
	if err != nil {
		t.Fatalf("open the route DB of %s: %v", itRouteDBEnv, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func itRouteDurability(t *testing.T) routeDurability {
	t.Helper()
	dur, err := newRouteDurability(1, 0, 1500)
	if err != nil {
		t.Fatal(err)
	}
	return dur
}

// TestIT_Route_ScanByHomeOverTheRealIndex:按 home_zone 键集翻页对着权威 DDL(行片、CHECK、idx_route_home)。
func TestIT_Route_ScanByHomeOverTheRealIndex(t *testing.T) {
	requireIT(t)
	itReset(t)
	st := itRouteStore(t)
	// 落在不同行片上:9_000_000_448 % 64 = 0,…449 → 1,…511 → 63。
	ids := []uint64{9_000_000_511, 9_000_000_448, 9_000_000_449}
	for _, id := range ids {
		itSetRouteHome(t, id, itSrcZone)
	}
	itSetRouteHome(t, 9_000_000_450, itDstZone)
	got, err := collectPlayerIDsWithHomeZoneDB(context.Background(), st, itSrcZone)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if want := []uint64{9_000_000_448, 9_000_000_449, 9_000_000_511}; !reflect.DeepEqual(got, want) {
		t.Fatalf("players = %v, want %v", got, want)
	}
}

// TestIT_Route_PairingAgainstTheWatermarkKey:「库与 Redis 成对」核对对着真库的 route_meta.generation。
func TestIT_Route_PairingAgainstTheWatermarkKey(t *testing.T) {
	requireIT(t)
	itReset(t)
	st := itRouteStore(t)
	rdb := itRedis(t, itMappingRD)
	ctx := context.Background()
	pub := newRoutePublisher(rdb, itRouteDurability(t))

	meta, err := readRouteMeta(ctx, st.db)
	if err != nil {
		t.Fatalf("read route_meta: %v", err)
	}
	if err := rdb.Set(ctx, routeWatermarkKey, `{"v":1,"generation":"`+meta.Generation+`","watermark_ms":42}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	paired, err := openPairedRouteStore(ctx, itRouteDSN(), authoritysql.BackendTiDB, pub)
	if err != nil || paired == nil {
		t.Fatalf("a matching generation = (%v, %v)", paired, err)
	}
	_ = paired.Close()
	if err := rdb.Set(ctx, routeWatermarkKey, `{"v":1,"generation":"ff","watermark_ms":42}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := openPairedRouteStore(ctx, itRouteDSN(), authoritysql.BackendTiDB, pub); !errors.Is(err, errRoutePairing) {
		t.Fatalf("another generation: %v, want errRoutePairing", err)
	}
	if err := rdb.Del(ctx, routeWatermarkKey).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := openPairedRouteStore(ctx, itRouteDSN(), authoritysql.BackendTiDB, pub); !errors.Is(err, errRoutePairing) {
		t.Fatalf("no watermark: %v, want errRoutePairing", err)
	}
}

// itClientIDSandwich 在每条 pipeline 的前后各塞一条 CLIENT ID:两者相等 ⇔ 整批(SET 与 WAITAOF)走的是同一条连接。
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

// TestIT_Route_MirrorWaitAOFSameConnection:镜像的 SET 与 WAITAOF 在同一条 pipeline、同一条连接上(CLIENT ID 夹心);
// 按完整值 compare-and-delete 只删这一次写的值。
func TestIT_Route_MirrorWaitAOFSameConnection(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, _ := testMaintenanceContext()
	rdb := itRedis(t, itMappingRD)
	if err := rdb.Do(ctx, "WAITAOF", 1, 0, 100).Err(); err != nil {
		itMissing(t, fmt.Sprintf("WAITAOF 1 0 on %s failed (%v): the mirror cases need Redis >= 7.2 with appendonly yes", itRedisAddr, err))
	}
	hook := &itClientIDSandwich{}
	rdb.AddHook(hook)
	pub := newRoutePublisher(rdb, itRouteDurability(t))
	value, err := encodeRouteMergedIntoValue(itDstZone, "901-902-20261030T000000Z-aaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.setMergedIntoMirror(ctx, itSrcZone, value); err != nil {
		t.Fatalf("setMergedIntoMirror: %v", err)
	}
	hook.mu.Lock()
	if len(hook.names) != 1 || !reflect.DeepEqual(hook.names[0], []string{"set", "waitaof"}) {
		hook.mu.Unlock()
		t.Fatalf("pipelines = %v, want one [set waitaof]", hook.names)
	}
	if ids := hook.ids[0]; ids[0] == 0 || ids[0] != ids[1] {
		hook.mu.Unlock()
		t.Fatalf("CLIENT ID before / after = %v: SET and WAITAOF did not share one connection", ids)
	}
	hook.mu.Unlock()
	if got, err := rdb.Get(ctx, mergedIntoKey(itSrcZone)).Result(); err != nil || got != value {
		t.Fatalf("mirror = (%q, %v), want %q", got, err, value)
	}
	if deleted, err := pub.deleteMergedIntoMirror(ctx, itSrcZone, `{"dst":903,"run_id":"other"}`); err != nil || deleted {
		t.Fatalf("delete with another value = (%v, %v), want nothing deleted", deleted, err)
	}
	if deleted, err := pub.deleteMergedIntoMirror(ctx, itSrcZone, value); err != nil || !deleted {
		t.Fatalf("delete = (%v, %v)", deleted, err)
	}
}
