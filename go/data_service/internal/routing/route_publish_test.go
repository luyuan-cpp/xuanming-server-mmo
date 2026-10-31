package routing

// db_first 的 W1 编排与发布脚本(route_publish.go,第 1a 批规格 f 节「routing/route_publish_test.go」)。
//
// miniredis + 假 RouteStore,不连库。WAITAOF 两项一律显式配 0:miniredis 是否支持 WAITAOF 没有核实(规格 Q4),
// 真实的 WAITAOF 与「同一条连接」只在 router_integration_test.go 的真 Redis 用例里验。
// 用例名刻意不用 TestRegisterPlayerZone_ 前缀:复验清单的 -run "TestRegisterPlayerZone_" 只该命中 redis 模式那 10 个。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/breaker"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// routeFakeStore 是 RouteStore 的假实现:记下每次入参,按预置结果回答。
type routeFakeStore struct {
	reqs   []store.RouteRegisterRequest
	result store.RouteRegisterResult
	err    error
	// hook 在 RegisterHome 里执行(Redis 预检之后、发布之前),n 是第几次调用(从 1 起)。
	hook func(n int)
}

var _ RouteStore = (*routeFakeStore)(nil)

func (f *routeFakeStore) RegisterHome(_ context.Context, req store.RouteRegisterRequest) (store.RouteRegisterResult, error) {
	f.reqs = append(f.reqs, req)
	if f.hook != nil {
		f.hook(len(f.reqs))
	}
	if f.err != nil {
		return store.RouteRegisterResult{}, f.err
	}
	return f.result, nil
}

func intPtr(v int) *int { return &v }

// newDBFirstRouter 建一个 db_first 的 Router(mapping Redis 指向 miniredis)。fake 为 nil 时不注入 store ——
// 不能把 nil 的 *routeFakeStore 传给 WithRouteStore,那是「非 nil 接口包着 nil 指针」。tweak 可改 RouteStore 段。
func newDBFirstRouter(t *testing.T, fake *routeFakeStore, tweak func(*config.RouteStoreConfig)) (*Router, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return newDBFirstRouterOn(t, mr, fake, tweak), mr
}

// newDBFirstRouterOn 同 newDBFirstRouter,但 mapping Redis 指向调用方给的 miniredis。
func newDBFirstRouterOn(t *testing.T, mr *miniredis.Miniredis, fake *routeFakeStore, tweak func(*config.RouteStoreConfig)) *Router {
	t.Helper()
	rc := config.RouteStoreConfig{
		Mode:               config.RouteStoreModeDBFirst,
		WaitAOFNumLocal:    intPtr(0),
		WaitAOFNumReplicas: intPtr(0),
	}
	if tweak != nil {
		tweak(&rc)
	}
	c := config.Config{
		MappingRedis: redis.RedisConf{Host: mr.Addr(), Type: "node"},
		RouteStore:   rc,
	}
	var opts []RouterOption
	if fake != nil {
		opts = append(opts, WithRouteStore(fake))
	}
	r := NewRouter(c, opts...)
	t.Cleanup(r.Close)
	return r
}

// committed 拼假 store 回给编排的结果:行的 player / home 与请求一致(编排会核对),rev 至少为 1。
func committed(outcome store.RouteRegisterOutcome, playerID uint64, home, storageID uint32, version, rev uint64) store.RouteRegisterResult {
	if rev == 0 {
		rev = 1
	}
	return store.RouteRegisterResult{
		Outcome: outcome,
		Row:     store.RouteRow{PlayerID: playerID, HomeZone: home, StorageID: storageID, PlacementVersion: version, Rev: rev},
	}
}

// routeKeysOf 读一名玩家的 [player:zone, player:placement, player:maprev](缺席为 "")。
func routeKeysOf(t *testing.T, mr *miniredis.Miniredis, playerID uint64) [3]string {
	t.Helper()
	var out [3]string
	for i, key := range []string{placement.HomeZoneKey(playerID), placement.Key(playerID), placement.MapRevKey(playerID)} {
		if !mr.Exists(key) {
			continue
		}
		v, err := mr.Get(key)
		require.NoError(t, err)
		out[i] = v
	}
	return out
}

// presetRouteKeys 按 [zone, placement, maprev] 预置缓存("" = 不写)。
func presetRouteKeys(t *testing.T, mr *miniredis.Miniredis, playerID uint64, vals [3]string) {
	t.Helper()
	for i, key := range []string{placement.HomeZoneKey(playerID), placement.Key(playerID), placement.MapRevKey(playerID)} {
		if vals[i] != "" {
			require.NoError(t, mr.Set(key, vals[i]))
		}
	}
}

// evalPublish 绕过编排直接跑发布脚本,回分类后的回复。
func evalPublish(t *testing.T, r *Router, playerID uint64, rev, home, plc string) placement.PublishReply {
	t.Helper()
	raw, err := r.mappingRedis.EvalCtx(context.Background(), placement.PublishRouteScript,
		placement.PublishRouteKeys(playerID), rev, home, plc)
	require.NoError(t, err)
	reply, err := placement.ClassifyPublishReply(raw, rev, home, plc)
	require.NoError(t, err, "raw reply %v", raw)
	return reply
}

// ── 脚本语义(8 个)──────────────────────────────────────────────

func TestPublishRouteScript_AppliesWhenAbsent(t *testing.T) {
	r, mr := newDBFirstRouter(t, nil, nil)
	plc := placement.StableValue(placement.DefaultGlobalStorageID, 1)

	reply := evalPublish(t, r, 9001, "1", "5", plc)
	assert.Equal(t, placement.PublishApplied, reply.Outcome)
	assert.Equal(t, [3]string{"5", plc, "1"}, routeKeysOf(t, mr, 9001), "三键一起写下,maprev 最后写")
}

// TestPublishRouteScript_LengthBeforeLexicographic:rev 先比长度再逐字节比。按字典序 "10" < "9",那样 10 永远发布不上去。
func TestPublishRouteScript_LengthBeforeLexicographic(t *testing.T) {
	r, mr := newDBFirstRouter(t, nil, nil)
	presetRouteKeys(t, mr, 9002, [3]string{"5", "", "9"})

	reply := evalPublish(t, r, 9002, "10", "5", "")
	assert.Equal(t, placement.PublishApplied, reply.Outcome, "缓存 9、发 10:10 更新,必须写入")
	assert.Equal(t, [3]string{"5", "", "10"}, routeKeysOf(t, mr, 9002))

	reply = evalPublish(t, r, 9002, "9", "5", "")
	assert.Equal(t, placement.PublishSuperseded, reply.Outcome, "缓存 10、发 9:9 更旧,不许写")
	assert.Equal(t, "10", reply.CurRev)
	assert.Equal(t, [3]string{"5", "", "10"}, routeKeysOf(t, mr, 9002))
}

// TestPublishRouteScript_EqualSameIsConfirmed:同 rev 同值 = 幂等重发,已确认;maprev 原值写回一次(touch),
// 让同一连接随后的 WAITAOF 覆盖「此刻已在缓存里的状态」。SET 会清掉 TTL,借此观察到 touch 确实发生了。
func TestPublishRouteScript_EqualSameIsConfirmed(t *testing.T) {
	r, mr := newDBFirstRouter(t, nil, nil)
	presetRouteKeys(t, mr, 9003, [3]string{"5", "7:3", "4"})
	mr.SetTTL(placement.MapRevKey(9003), time.Hour)

	reply := evalPublish(t, r, 9003, "4", "5", "7:3")
	assert.Equal(t, placement.PublishConfirmed, reply.Outcome)
	assert.Equal(t, [3]string{"5", "7:3", "4"}, routeKeysOf(t, mr, 9003))
	assert.Zero(t, mr.TTL(placement.MapRevKey(9003)), "返回 0 的分支必须把 maprev 原值写回一次(touch)")
}

// TestPublishRouteScript_EqualDifferentIsConflict:同 rev 异值 = 有写者绕过了脚本,或库回档后 rev 被复用。零写入。
func TestPublishRouteScript_EqualDifferentIsConflict(t *testing.T) {
	r, mr := newDBFirstRouter(t, nil, nil)
	before := [3]string{"5", placement.StableValue(placement.DefaultGlobalStorageID, 1), "5"}
	presetRouteKeys(t, mr, 9004, before)

	reply := evalPublish(t, r, 9004, "5", "5", placement.StableValue(placement.DefaultGlobalStorageID, 2))
	assert.Equal(t, placement.PublishConflict, reply.Outcome)
	assert.Equal(t, before, routeKeysOf(t, mr, 9004))

	reply = evalPublish(t, r, 9004, "5", "6", before[1])
	assert.Equal(t, placement.PublishConflict, reply.Outcome, "home 不同同样是冲突")
	assert.Equal(t, before, routeKeysOf(t, mr, 9004))
}

// TestPublishRouteScript_EmptyArgDeletes:空参数 = DEL 对应的键(库里那一列是 NULL)。
func TestPublishRouteScript_EmptyArgDeletes(t *testing.T) {
	r, mr := newDBFirstRouter(t, nil, nil)
	presetRouteKeys(t, mr, 9005, [3]string{"5", "7:1", "1"})

	reply := evalPublish(t, r, 9005, "2", "5", "")
	assert.Equal(t, placement.PublishApplied, reply.Outcome)
	assert.Equal(t, [3]string{"5", "", "2"}, routeKeysOf(t, mr, 9005))

	reply = evalPublish(t, r, 9005, "3", "", "")
	assert.Equal(t, placement.PublishApplied, reply.Outcome)
	assert.Equal(t, [3]string{"", "", "3"}, routeKeysOf(t, mr, 9005), "墓碑:两列都 DEL,maprev 仍记下这一版")
}

// TestPublishRouteScript_RejectsBadRevAndMalformedMapRev:坏 rev 参数与畸形 maprev 都是错误回复,三把键零变更。
func TestPublishRouteScript_RejectsBadRevAndMalformedMapRev(t *testing.T) {
	// 每个拒绝场景用独立客户端:连续的预期 Lua 错误会触发 go-zero 的概率熔断,
	// 后续调用可能根本没执行脚本,无法验证对应的错误与零写入契约。
	// 由父测试持有清理,让各实例同时存活,避免端口复用命中按地址缓存的旧客户端。
	for _, bad := range []string{"", "0", "01", "-1", "1a", " 1", "123456789012345678901"} {
		r, mr := newDBFirstRouter(t, nil, nil)
		t.Run(fmt.Sprintf("bad_rev_%q", bad), func(t *testing.T) {
			before := [3]string{"5", "7:1", "3"}
			presetRouteKeys(t, mr, 9006, before)
			_, err := r.mappingRedis.EvalCtx(context.Background(), placement.PublishRouteScript,
				placement.PublishRouteKeys(9006), bad, "5", "7:1")
			require.Error(t, err, "rev=%q", bad)
			assert.Contains(t, err.Error(), "bad rev argument", "rev=%q", bad)
			assert.Equal(t, before, routeKeysOf(t, mr, 9006), "rev=%q", bad)
		})
	}

	r, mr := newDBFirstRouter(t, nil, nil)
	t.Run("malformed_maprev", func(t *testing.T) {
		before := [3]string{"5", "7:1", "abc"}
		presetRouteKeys(t, mr, 9006, before)
		_, err := r.mappingRedis.EvalCtx(context.Background(), placement.PublishRouteScript,
			placement.PublishRouteKeys(9006), "4", "5", "7:1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "malformed maprev")
		assert.Equal(t, before, routeKeysOf(t, mr, 9006))
	})
}

// TestPublishRouteScript_LegacyValuesWithoutMapRev 是 B2:缓存没有 maprev 时,遗留的 zone / placement 与本次不同
// 一律不覆盖(回 {2},零写入);遗留值与本次相同、或根本没有遗留值时照常写入。
func TestPublishRouteScript_LegacyValuesWithoutMapRev(t *testing.T) {
	cases := []struct {
		name     string
		zone     string
		plc      string
		sendHome string
		sendPlc  string
		want     placement.PublishOutcome
	}{
		{"遗留 zone 不同", "4", "", "5", "", placement.PublishLegacyConflict},
		{"遗留落点存在而本次为空", "", "7:1", "5", "", placement.PublishLegacyConflict},
		{"遗留落点不同", "5", "7:1", "5", "7:2", placement.PublishLegacyConflict},
		{"遗留值相同", "5", "7:1", "5", "7:1", placement.PublishApplied},
		{"全部缺席", "", "", "5", "7:1", placement.PublishApplied},
		{"只有遗留落点且相同、zone 缺席", "", "7:1", "5", "7:1", placement.PublishApplied},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, mr := newDBFirstRouter(t, nil, nil)
			playerID := uint64(9100 + i)
			before := [3]string{tc.zone, tc.plc, ""}
			presetRouteKeys(t, mr, playerID, before)

			reply := evalPublish(t, r, playerID, "1", tc.sendHome, tc.sendPlc)
			assert.Equal(t, tc.want, reply.Outcome)
			if tc.want == placement.PublishLegacyConflict {
				assert.Equal(t, before, routeKeysOf(t, mr, playerID), "遗留值不同:零写入,也不写 maprev")
				assert.Equal(t, tc.zone, reply.CurHome)
				assert.Equal(t, tc.plc, reply.CurPlacement)
				return
			}
			assert.Equal(t, [3]string{tc.sendHome, tc.sendPlc, "1"}, routeKeysOf(t, mr, playerID))
		})
	}
}

// ── db_first 编排(17 个)──────────────────────────────────────────

func TestDBFirstRegister_InsertPublishesThreeKeys(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterInserted, 9201, 5, placement.DefaultGlobalStorageID, 1, 1)}
	r, mr := newDBFirstRouter(t, fake, nil)

	require.NoError(t, r.RegisterPlayerZone(context.Background(), 9201, 5, placement.DefaultGlobalStorageID))
	require.Len(t, fake.reqs, 1)
	assert.Equal(t, store.RouteRegisterRequest{
		PlayerID: 9201, HomeZone: 5, StorageID: placement.DefaultGlobalStorageID, ExistingRowOnly: false,
	}, fake.reqs[0], "缓存里没有这名玩家:允许新插")
	assert.Equal(t, [3]string{"5", placement.StableValue(placement.DefaultGlobalStorageID, 1), "1"}, routeKeysOf(t, mr, 9201))
}

// TestDBFirstRegister_SameHomeRepublishesMissingKeys:上次提交了却没发布成(缓存为空)时,重试走库里的幂等分支
// 并重新发布,把三键补齐;缓存已齐时再重试是 Confirmed,照样成功。
func TestDBFirstRegister_SameHomeRepublishesMissingKeys(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterAlreadySame, 9202, 5, 0, 0, 2)}
	r, mr := newDBFirstRouter(t, fake, nil)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 9202, 5, 0))
	assert.False(t, fake.reqs[0].ExistingRowOnly)
	assert.Equal(t, [3]string{"5", "", "2"}, routeKeysOf(t, mr, 9202), "幂等分支同样要发布,补齐上次失败的发布")

	require.NoError(t, r.RegisterPlayerZone(ctx, 9202, 5, 0))
	require.Len(t, fake.reqs, 2)
	assert.True(t, fake.reqs[1].ExistingRowOnly, "缓存已认得这名玩家:只认已有行")
	assert.Equal(t, [3]string{"5", "", "2"}, routeKeysOf(t, mr, 9202))
}

func TestDBFirstRegister_ConflictFromStore(t *testing.T) {
	fake := &routeFakeStore{result: store.RouteRegisterResult{Outcome: store.RouteRegisterConflict, ExistingHome: 3}}
	r, mr := newDBFirstRouter(t, fake, nil)

	err := r.RegisterPlayerZone(context.Background(), 9203, 5, 0)
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, HomeZoneConflictError{PlayerID: 9203, Existing: 3, Requested: 5}, *conflict)
	assert.Equal(t, [3]string{}, routeKeysOf(t, mr, 9203), "冲突不发布")
}

// TestDBFirstRegister_RedisPrecheckConflictSkipsStore:缓存里已是别的 zone 时预检就拒绝,不进库;
// 值解析不了用 redis 模式同一文案(不是冲突,也不进库)。
func TestDBFirstRegister_RedisPrecheckConflictSkipsStore(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterInserted, 9204, 5, 0, 0, 1)}
	r, mr := newDBFirstRouter(t, fake, nil)
	ctx := context.Background()
	require.NoError(t, mr.Set(placement.HomeZoneKey(9204), "3"))

	err := r.RegisterPlayerZone(ctx, 9204, 5, 0)
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, uint32(3), conflict.Existing)
	assert.Empty(t, fake.reqs, "预检冲突不许进库")

	require.NoError(t, mr.Set(placement.HomeZoneKey(9205), "abc"))
	err = r.RegisterPlayerZone(ctx, 9205, 5, 0)
	require.Error(t, err)
	assert.False(t, errors.As(err, &conflict))
	assert.Contains(t, err.Error(), "unparsable zone mapping")
	assert.Empty(t, fake.reqs)
}

// TestDBFirstRegister_RedisKnownPlayerRequiresExistingRow:三键任一在 = 缓存认得这名玩家,store 必须收到
// ExistingRowOnly=true;库里没有行(RowMissing)时回 ErrRouteStore + ErrRouteUnknownToStore,Redis 零变更。
func TestDBFirstRegister_RedisKnownPlayerRequiresExistingRow(t *testing.T) {
	for i, present := range [][3]string{
		{"5", "", ""},
		{"", "7:1", ""},
		{"", "", "3"},
	} {
		t.Run(fmt.Sprintf("present=%q", present), func(t *testing.T) {
			fake := &routeFakeStore{result: store.RouteRegisterResult{Outcome: store.RouteRegisterRowMissing}}
			r, mr := newDBFirstRouter(t, fake, nil)
			playerID := uint64(9210 + i)
			presetRouteKeys(t, mr, playerID, present)

			err := r.RegisterPlayerZone(context.Background(), playerID, 5, 0)
			require.ErrorIs(t, err, ErrRouteStore)
			require.ErrorIs(t, err, ErrRouteUnknownToStore)
			require.Len(t, fake.reqs, 1)
			assert.True(t, fake.reqs[0].ExistingRowOnly)
			assert.Equal(t, present, routeKeysOf(t, mr, playerID), "缺行拒绝必须零变更")
		})
	}
}

func TestDBFirstRegister_StoreErrorWrapsErrRouteStore(t *testing.T) {
	fake := &routeFakeStore{err: fmt.Errorf("begin tx: %w", store.ErrRouteNotWritable)}
	r, mr := newDBFirstRouter(t, fake, nil)

	err := r.RegisterPlayerZone(context.Background(), 9220, 5, 0)
	require.ErrorIs(t, err, ErrRouteStore)
	assert.ErrorIs(t, err, store.ErrRouteNotWritable, "原因要保留,errors.Is 能取到")
	assert.Equal(t, [3]string{}, routeKeysOf(t, mr, 9220))
}

// TestDBFirstRegister_NilStoreFailsClosed:db_first 而 store 没装配,一律 ErrRouteStore,绝不退化成 redis 模式。
func TestDBFirstRegister_NilStoreFailsClosed(t *testing.T) {
	r, mr := newDBFirstRouter(t, nil, nil)

	err := r.RegisterPlayerZone(context.Background(), 9221, 5, 0)
	require.ErrorIs(t, err, ErrRouteStore)
	assert.Equal(t, [3]string{}, routeKeysOf(t, mr, 9221))
}

// TestDBFirstRegister_PublishFailureAfterCommitThenRetryRepairs:库已提交、发布时 Redis 不可达 → 发布错误
// (不是 ErrRouteStore);Redis 恢复后同一请求重试,库里落到幂等分支,重新发布补齐三键。
// PublishAttempts=1:只为让失败快;重试语义另有用例覆盖。
//
// 「Redis 恢复」用一个**新地址**上的全新 miniredis + 新 Router(同一个假 store)表达,而不是在原地址上 Restart:
// go-zero 按地址缓存底层 go-redis 客户端,同地址的 Router 仍拿到 Close 之前的旧连接(Windows 上 go-redis 不做
// 存活探测),旧连接逐条失败还会记进 Router 的熔断器,紧接着那次登记的三条命令就可能被按概率丢弃 —— 用例结果
// 取决于墙钟与平台(AGENTS.md §11.4)。新地址 = 新连接池 + 新熔断器,结果确定。状态上两者等价:上一次发布一把键
// 都没写成(Redis 在发布之前就关了),新实例同样是空的。
func TestDBFirstRegister_PublishFailureAfterCommitThenRetryRepairs(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterInserted, 9222, 5, 0, 0, 1)}
	attemptsOnce := func(rc *config.RouteStoreConfig) { rc.PublishAttempts = 1 }
	r, mr := newDBFirstRouter(t, fake, attemptsOnce)
	ctx := context.Background()
	fake.hook = func(n int) {
		if n == 1 {
			mr.Close()
		}
	}

	err := r.RegisterPlayerZone(ctx, 9222, 5, 0)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRouteStore, "已提交之后的发布失败不是库故障")

	recovered := miniredis.RunT(t)
	r2 := newDBFirstRouterOn(t, recovered, fake, attemptsOnce)
	fake.result = committed(store.RouteRegisterAlreadySame, 9222, 5, 0, 0, 1)
	require.NoError(t, r2.RegisterPlayerZone(ctx, 9222, 5, 0))
	require.Len(t, fake.reqs, 2)
	assert.False(t, fake.reqs[1].ExistingRowOnly, "缓存里还没有键:按新玩家走,由库的 ODKU 落到幂等分支")
	assert.Equal(t, [3]string{"5", "", "1"}, routeKeysOf(t, recovered, 9222))
}

// TestDBFirstRegister_InvalidArgsNeverReachStore:player_id 为 0、home 为 0 或越界 → ErrInvalidRegistration,
// 不碰 Redis 也不碰库。
func TestDBFirstRegister_InvalidArgsNeverReachStore(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterInserted, 9230, 5, 0, 0, 1)}
	r, mr := newDBFirstRouter(t, fake, nil)
	rec := recordCommands(mr)

	for _, tc := range []struct {
		playerID uint64
		home     uint32
	}{{0, 5}, {9230, 0}, {9230, store.RouteHomeZoneMax + 1}} {
		err := r.RegisterPlayerZone(context.Background(), tc.playerID, tc.home, 0)
		require.ErrorIs(t, err, ErrInvalidRegistration, "player=%d home=%d", tc.playerID, tc.home)
	}
	assert.Empty(t, fake.reqs, "参数非法不许进库")
	assert.Empty(t, rec.snapshot(), "参数非法不许碰 Redis")
}

// TestDBFirstRegister_PlacementKeptSucceeds:行里 home 为 NULL、落点已在(请求也带 storage)→ 补 home、保留落点,
// 打 ERROR 后成功(与 redis 模式 zoneRegisteredPlacementKept 同义)。缓存里那条遗留落点与库一致,发布照常写入。
func TestDBFirstRegister_PlacementKeptSucceeds(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterFilledHomePlacementKept, 9231, 5, 7, 3, 2)}
	r, mr := newDBFirstRouter(t, fake, nil)
	presetRouteKeys(t, mr, 9231, [3]string{"", "7:3", ""})

	require.NoError(t, r.RegisterPlayerZone(context.Background(), 9231, 5, placement.DefaultGlobalStorageID))
	require.Len(t, fake.reqs, 1)
	assert.True(t, fake.reqs[0].ExistingRowOnly, "缓存里有落点:只认已有行")
	assert.Equal(t, [3]string{"5", "7:3", "2"}, routeKeysOf(t, mr, 9231), "落点保持 7:3,不被请求的 storage 覆盖")
}

// TestDBFirstRegister_LegacyPlacementMismatchRefused 是 B2:缓存里有 zone 与落点、没有 maprev(P1 导入之前、
// 或绕过脚本的写者留下的),而库里的落点不同 → 发布拒绝(ErrRoutePublishConflict),两键原值不变,也不写 maprev。
func TestDBFirstRegister_LegacyPlacementMismatchRefused(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterAlreadySame, 9232, 5, placement.DefaultGlobalStorageID, 1, 1)}
	r, mr := newDBFirstRouter(t, fake, nil)
	before := [3]string{"5", "7:1", ""}
	presetRouteKeys(t, mr, 9232, before)

	err := r.RegisterPlayerZone(context.Background(), 9232, 5, 0)
	require.ErrorIs(t, err, ErrRoutePublishConflict)
	assert.NotErrorIs(t, err, ErrRouteStore)
	assert.Equal(t, before, routeKeysOf(t, mr, 9232), "遗留值不许被库值静默覆盖")
}

// TestDBFirstRegister_SupersededWithDifferentHomeFails 是 M3:缓存里出现更新的 rev,home 却不同 —— 第 1a 批 home
// 写定后不再改,这只可能是库被回档或有写者绕过了脚本 → ErrRoutePublishConflict,零写入。home 相同的 Superseded 照常成功。
func TestDBFirstRegister_SupersededWithDifferentHomeFails(t *testing.T) {
	t.Run("home 不同", func(t *testing.T) {
		fake := &routeFakeStore{result: committed(store.RouteRegisterAlreadySame, 9233, 5, 0, 0, 2)}
		r, mr := newDBFirstRouter(t, fake, nil)
		fake.hook = func(int) { presetRouteKeys(t, mr, 9233, [3]string{"6", "", "9"}) }

		err := r.RegisterPlayerZone(context.Background(), 9233, 5, 0)
		require.ErrorIs(t, err, ErrRoutePublishConflict)
		assert.Contains(t, err.Error(), "superseded_home_differs")
		assert.Equal(t, [3]string{"6", "", "9"}, routeKeysOf(t, mr, 9233))
	})
	t.Run("home 相同", func(t *testing.T) {
		fake := &routeFakeStore{result: committed(store.RouteRegisterAlreadySame, 9234, 5, 0, 0, 2)}
		r, mr := newDBFirstRouter(t, fake, nil)
		fake.hook = func(int) { presetRouteKeys(t, mr, 9234, [3]string{"5", "", "9"}) }

		require.NoError(t, r.RegisterPlayerZone(context.Background(), 9234, 5, 0))
		assert.Equal(t, [3]string{"5", "", "9"}, routeKeysOf(t, mr, 9234), "更新的版本留着,不倒退")
	})
}

// TestDBFirstRegister_DeterministicScriptErrorNotRetried 是 m4:Redis 错误回复(这里是畸形 maprev)是确定性的,
// 只发一次 EVAL,不按 PublishAttempts 重试。
func TestDBFirstRegister_DeterministicScriptErrorNotRetried(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterAlreadySame, 9235, 5, 0, 0, 1)}
	r, mr := newDBFirstRouter(t, fake, func(rc *config.RouteStoreConfig) { rc.PublishAttempts = 5 })
	require.NoError(t, mr.Set(placement.MapRevKey(9235), "garbage"))
	rec := recordCommands(mr)

	err := r.RegisterPlayerZone(context.Background(), 9235, 5, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed maprev")
	assert.Contains(t, err.Error(), "not retried")
	assert.NotErrorIs(t, err, ErrRouteStore)
	assert.Equal(t, 1, rec.count("EVAL"), "确定性错误只许发一次 EVAL,不按 PublishAttempts=5 重试")
	assert.Equal(t, "garbage", routeKeysOf(t, mr, 9235)[2])
}

// publishBreakerHook 只拦**含发布脚本 EVAL** 的 pipeline:前 reject 个这样的批不调下一层、直接回
// breaker.ErrServiceUnavailable —— 与 go-zero 熔断打开时 breakerHook.ProcessPipelineHook 的行为逐字相同(命令的 Err() / Val()
// 都留空);之后放行。rejected / passed 分别数被挡下与放行的发布批。
//
// 其余 pipeline 一律放行(2026-10-18 修正:第一版不分命令全拦):go-redis 每条新连接初始化时(initConn)经同一条钩子链用
// pipeline 发两条 CLIENT SETINFO,挡了它新连接就建不起来 —— MustNewRedis 的连通性 PING 随之失败,logx.Must 直接 os.Exit(1),
// 整个测试进程退出;侥幸不退出,后台补空闲连接也会吃掉计数,断言就取决于调度。过滤口径同 login createplayer_seed_test.go 的
// probePipelineHook(按批内命令认人,不按次数)。
// installed 由自检批(ECHO selfCheckEcho)置位,证明这个钩子确实挂在了本地址的客户端上;自检批不计入上面两个数。
type publishBreakerHook struct {
	reject    int32
	rejected  atomic.Int32
	passed    atomic.Int32
	installed atomic.Bool
}

const selfCheckEcho = "route-publish-breaker-hook-self-check"

func (h *publishBreakerHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h *publishBreakerHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }

func (h *publishBreakerHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		switch {
		case pipelineHasCommand(cmds, "eval", placement.PublishRouteScript):
			if h.rejected.Load() < h.reject {
				h.rejected.Add(1)
				return breaker.ErrServiceUnavailable
			}
			h.passed.Add(1)
		case pipelineHasCommand(cmds, "echo", selfCheckEcho):
			h.installed.Store(true)
		}
		return next(ctx, cmds)
	}
}

// pipelineHasCommand 判断批里有没有一条名为 name(小写)、第一个参数恰为 arg 的命令(EVAL 的第一个参数是脚本正文)。
func pipelineHasCommand(cmds []goredis.Cmder, name, arg string) bool {
	for _, c := range cmds {
		if args := c.Args(); c.Name() == name && len(args) > 1 {
			if s, _ := args[1].(string); s == arg {
				return true
			}
		}
	}
	return false
}

// newRouterWithRejectedPipelines 建一个 db_first Router,前 n 个发布批被测试钩子挡下。
// 发布已使用独立的 publishRedis,钩子必须挂在这个客户端上;mappingRedis 的 go-zero 地址缓存不再承载发布。
// 自检只发 ECHO,确认钩子安装在实际发布客户端上,不消耗挡下的名额。
func newRouterWithRejectedPipelines(t *testing.T, fake *routeFakeStore, n int32, attempts int) (*Router, *miniredis.Miniredis, *publishBreakerHook) {
	t.Helper()
	ctx := context.Background()
	mr := miniredis.RunT(t)
	r := newDBFirstRouterOn(t, mr, fake, func(rc *config.RouteStoreConfig) { rc.PublishAttempts = attempts })
	hook := &publishBreakerHook{reject: n}
	r.publishRedis.AddHook(hook)
	_, selfCheck := r.publishRedis.Pipelined(ctx, func(p goredis.Pipeliner) error {
		p.Echo(ctx, selfCheckEcho)
		return nil
	})
	require.NoError(t, selfCheck, "自检批不该被挡")
	require.True(t, hook.installed.Load(), "实际发布客户端必须装有测试钩子")
	return r, mr, hook
}

// TestDBFirstRegister_PublishRetriesWhenPipelineNotExecuted(2026-10-18,第 1b-1 批核实 B0-5 的疑点):熔断打开时 pipeline 的回调
// 照样被调用、命令排了队却一条都没执行(Err() 为 nil、Val() 为 nil)。以前这会落到 ClassifyPublishReply(nil) →
// ErrUnexpectedPublishReply,被当成确定性错误、不再重试;现在按可重试的不可用处理:第一次被挡、第二次放行 → 登记成功、三键写入。
// 一直被挡则重试用尽后失败,错误里认得出熔断拒绝,而不是「回复形状不对」。
func TestDBFirstRegister_PublishRetriesWhenPipelineNotExecuted(t *testing.T) {
	t.Run("第一次被挡_重试成功", func(t *testing.T) {
		fake := &routeFakeStore{result: committed(store.RouteRegisterInserted, 9240, 5, 0, 0, 1)}
		r, mr, hook := newRouterWithRejectedPipelines(t, fake, 1, 2)

		require.NoError(t, r.RegisterPlayerZone(context.Background(), 9240, 5, 0))
		assert.Equal(t, [3]string{"5", "", "1"}, routeKeysOf(t, mr, 9240))
		assert.Equal(t, int32(1), hook.rejected.Load(), "恰好一个发布批被挡")
		assert.Equal(t, int32(1), hook.passed.Load(), "恰好一个发布批放行")
	})
	t.Run("一直被挡_重试用尽_不是形状错误", func(t *testing.T) {
		fake := &routeFakeStore{result: committed(store.RouteRegisterInserted, 9241, 5, 0, 0, 1)}
		r, mr, hook := newRouterWithRejectedPipelines(t, fake, 3, 3)

		err := r.RegisterPlayerZone(context.Background(), 9241, 5, 0)
		require.Error(t, err)
		assert.ErrorIs(t, err, breaker.ErrServiceUnavailable)
		assert.NotErrorIs(t, err, placement.ErrUnexpectedPublishReply, "没执行不是「回复形状不对」")
		assert.NotErrorIs(t, err, ErrRouteStore, "已提交之后的发布失败不是库故障")
		assert.Contains(t, err.Error(), "after 3 attempt(s)")
		assert.Equal(t, int32(3), hook.rejected.Load(), "按 PublishAttempts=3 重试满三次,三个发布批都被挡")
		assert.Zero(t, hook.passed.Load())
		assert.Equal(t, [3]string{}, routeKeysOf(t, mr, 9241), "一条命令都没执行,三键都不在")
	})
}

// commandRecorder 记下 miniredis 收到的命令名(大写)。连接握手(HELLO / CLIENT / PING / AUTH / SELECT)不记:
// go-zero 的连接池带 MinIdleConns,后台异步建连时 go-redis 会自己发握手,落在哪段测量里取决于调度,
// 所以不用 CommandCount 的前后差(同 go/friend/internal/data/session_reader_test.go 的做法)。
// 脚本里 redis.call 发出的命令也会经过钩子,所以只按命令名计数,不断言完整序列。
type commandRecorder struct {
	mu   sync.Mutex
	cmds []string
}

func recordCommands(mr *miniredis.Miniredis) *commandRecorder {
	rec := &commandRecorder{}
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		switch cmd { // miniredis 交给钩子的命令名已经转成大写
		case "HELLO", "CLIENT", "PING", "AUTH", "SELECT":
			return false
		}
		rec.mu.Lock()
		rec.cmds = append(rec.cmds, cmd)
		rec.mu.Unlock()
		return false // false = 命令照常执行
	})
	return rec
}

func (rec *commandRecorder) snapshot() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.cmds...)
}

func (rec *commandRecorder) count(name string) int {
	n := 0
	for _, cmd := range rec.snapshot() {
		if cmd == name {
			n++
		}
	}
	return n
}

// TestDBFirstRegister_RedisModeNeverCallsStore:redis 模式下即使注入了 store 也一次都不调,行为与今天逐字相同
// (SETNX 脚本写 player:zone,不写 maprev)。
func TestDBFirstRegister_RedisModeNeverCallsStore(t *testing.T) {
	fake := &routeFakeStore{result: committed(store.RouteRegisterInserted, 9236, 5, 0, 0, 1)}
	r, mr := newDBFirstRouter(t, fake, func(rc *config.RouteStoreConfig) { rc.Mode = config.RouteStoreModeRedis })

	require.NoError(t, r.RegisterPlayerZone(context.Background(), 9236, 5, 0))
	assert.Empty(t, fake.reqs)
	assert.Equal(t, [3]string{"5", "", ""}, routeKeysOf(t, mr, 9236))
}

// ── 纯函数(3 个;2026-10-18 起加 TestPublishPipelineNotExecuted 共 4 个)──────────────────

func TestCheckWaitAOFReply(t *testing.T) {
	require.NoError(t, checkWaitAOFReply([]any{int64(1), int64(0)}, 1, 0))
	require.NoError(t, checkWaitAOFReply([]any{int64(1), int64(2)}, 1, 1), "多于要求也算满足")
	require.NoError(t, checkWaitAOFReply([]any{int64(0), int64(0)}, 0, 0))

	err := checkWaitAOFReply([]any{int64(0), int64(0)}, 1, 0)
	require.Error(t, err, "本地没落盘")
	assert.NotErrorIs(t, err, errUnexpectedWaitAOFReply, "份数不足可重试,不是形状错")

	err = checkWaitAOFReply([]any{int64(1), int64(0)}, 1, 1)
	require.Error(t, err, "本地满足、副本不足")
	assert.NotErrorIs(t, err, errUnexpectedWaitAOFReply)

	for _, bad := range [][]any{nil, {}, {int64(1)}, {int64(1), int64(0), int64(0)}, {"1", int64(0)}, {int64(1), "0"}, {int64(-1), int64(0)}} {
		assert.ErrorIs(t, checkWaitAOFReply(bad, 1, 0), errUnexpectedWaitAOFReply, "reply=%v", bad)
	}
}

// TestWaitAOFBudgetMs 是 m5:先截断成整毫秒再判 < 1 —— WAITAOF 的 timeout 为 0 表示无限阻塞。
func TestWaitAOFBudgetMs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const configured, margin = 1500 * time.Millisecond, 100 * time.Millisecond

	ms, ok := waitAOFBudgetMs(now, now.Add(100*time.Millisecond+500*time.Microsecond), true, configured, margin)
	assert.False(t, ok, "剩余 100.5ms 减去 100ms 留量 = 0.5ms,截断成 0,不得发送")
	assert.Zero(t, ms)

	ms, ok = waitAOFBudgetMs(now, time.Time{}, false, configured, margin)
	assert.True(t, ok)
	assert.Equal(t, int64(1500), ms, "没有截止时间:取配置值")

	ms, ok = waitAOFBudgetMs(now, now.Add(5*time.Second), true, configured, margin)
	assert.True(t, ok)
	assert.Equal(t, int64(1500), ms, "配置值小于剩余:取配置值")

	ms, ok = waitAOFBudgetMs(now, now.Add(time.Second+700*time.Microsecond), true, configured, margin)
	assert.True(t, ok)
	assert.Equal(t, int64(900), ms, "剩余小于配置值:取剩余减留量并截断")

	_, ok = waitAOFBudgetMs(now, now.Add(-time.Second), true, configured, margin)
	assert.False(t, ok, "截止时间已过")
}

// TestPublishPipelineNotExecuted(2026-10-18):pipeline 整批是否按「没执行」处理的判据,与 scene_manager 的
// pipelineBatchRejected 同口径。命令的 err 用 SetErr 造(没执行的命令 Err() 为 nil)。
func TestPublishPipelineNotExecuted(t *testing.T) {
	ctx := context.Background()
	cmd := func(err error) *goredis.Cmd {
		c := goredis.NewCmd(ctx, "EVAL")
		if err != nil {
			c.SetErr(err)
		}
		return c
	}
	someErr := errors.New("boom")
	for _, tc := range []struct {
		name    string
		pipeErr error
		cmds    []*goredis.Cmd
		want    bool
	}{
		{"executed cleanly", nil, []*goredis.Cmd{cmd(nil), cmd(nil)}, false},
		{"redis.Nil is the first error", goredis.Nil, []*goredis.Cmd{cmd(goredis.Nil)}, false},
		{"breaker open", breaker.ErrServiceUnavailable, []*goredis.Cmd{cmd(nil), cmd(nil)}, true},
		{"breaker open even if a command carries an error", breaker.ErrServiceUnavailable, []*goredis.Cmd{cmd(someErr)}, true},
		{"context done before exec", context.DeadlineExceeded, []*goredis.Cmd{cmd(nil)}, true},
		{"error landed on no command", someErr, []*goredis.Cmd{cmd(nil), nil}, true},
		{"a command carries its own error", someErr, []*goredis.Cmd{cmd(nil), cmd(someErr)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, publishPipelineNotExecuted(tc.pipeErr, tc.cmds...))
		})
	}
}

// TestNewRouterPanicsOnInvalidRouteStore:非法 RouteStore 段在 NewRouter 里就 panic(与 MustNewRedis 同口径),
// 不能静默跑成 redis 模式;panic 发生在建任何连接之前,所以这里的 MappingRedis 地址根本不会被拨。
func TestNewRouterPanicsOnInvalidRouteStore(t *testing.T) {
	for _, rc := range []config.RouteStoreConfig{
		{Mode: "DB_FIRST"},
		{Mode: "db-first"},
		{MergedZoneRefreshSec: 61},
		{PublishAttempts: 6},
		{WaitAOFTimeoutMs: 1501},
		{WaitAOFNumLocal: intPtr(2)},
	} {
		c := config.Config{
			MappingRedis: redis.RedisConf{Host: "127.0.0.1:1", Type: "node"},
			RouteStore:   rc,
		}
		assert.Panics(t, func() { NewRouter(c) }, "RouteStore=%+v", rc)
	}
}
