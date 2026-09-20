package logic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/routing"
	"data_service/internal/store"
	"data_service/internal/svc"

	"shared/playername"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 玩家名字 logic 的单测(设计 docs/design/guild-phase2/03-names.md §3.19)。
//
// 边界:**不连真库**。store 用 fake(它只记调用、按脚本回终局),缓存用 miniredis
// (真的跑 SET/SETNX/MGET/DEL,所以负缓存的 NX 语义是被真正验证过的,不是假装的)。
// 真 MySQL 的部分(唯一键、并发抢名、条件删除)归集成测试
// store/player_name_store_integration_test.go,那边跑 -tags=integration。

// ── fake store ─────────────────────────────────────────────────

type playerNameReserveCall struct {
	playerID uint64
	name     string // 展示名
	norm     string // 判重键
	nowMs    uint64
}

type playerNameReleaseCall struct {
	playerID     uint64
	norm         string
	minCreatedMs uint64
}

// fakePlayerNameStore 按 svc.PlayerNameStore 这个接口注入,测的是 logic 与 store 之间的
// **契约**(传了什么参数、怎么翻译终局),而不是 SQL。
type fakePlayerNameStore struct {
	reserveCalls []playerNameReserveCall
	releaseCalls []playerNameReleaseCall
	batchCalls   [][]uint64

	reserveOutcome store.ReserveOutcome
	reserveOwner   uint64
	reserveErr     error

	releaseOutcome store.ReleaseOutcome
	releaseErr     error

	// names 是"库里已有的名字",BatchGet 按它作答;没有的 id 不出现在返回值里。
	names    map[uint64]string
	batchErr error
}

// 编译期钉住接口形状:装配层(svc.ServiceContext)换签名时,这一行先炸,
// 而不是等到某个用例跑起来才发现 fake 与真 store 不是一回事。
var _ svc.PlayerNameStore = (*fakePlayerNameStore)(nil)

func (f *fakePlayerNameStore) Close() error { return nil }

func (f *fakePlayerNameStore) Reserve(_ context.Context, playerID uint64, name, norm string, nowMs uint64) (store.ReserveOutcome, uint64, error) {
	f.reserveCalls = append(f.reserveCalls, playerNameReserveCall{
		playerID: playerID, name: name, norm: norm, nowMs: nowMs,
	})
	if f.reserveErr != nil {
		return 0, 0, f.reserveErr
	}
	return f.reserveOutcome, f.reserveOwner, nil
}

func (f *fakePlayerNameStore) Release(_ context.Context, playerID uint64, norm string, minCreatedMs uint64) (store.ReleaseOutcome, error) {
	f.releaseCalls = append(f.releaseCalls, playerNameReleaseCall{
		playerID: playerID, norm: norm, minCreatedMs: minCreatedMs,
	})
	if f.releaseErr != nil {
		return 0, f.releaseErr
	}
	return f.releaseOutcome, nil
}

func (f *fakePlayerNameStore) BatchGet(_ context.Context, ids []uint64) (map[uint64]string, error) {
	// 存副本:logic 复用底层数组时,断言看到的仍然是当时传进来的那一批。
	f.batchCalls = append(f.batchCalls, append([]uint64(nil), ids...))
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make(map[uint64]string, len(ids))
	for _, id := range ids {
		if name, ok := f.names[id]; ok {
			out[id] = name
		}
	}
	return out, nil
}

// ── 测试脚手架 ─────────────────────────────────────────────────

// playerNameTestReleaseWindow 等参数与 config 的默认值一致(§3.6a),
// 这样用例里断言的窗口下界就是生产会用的那一个。
const (
	playerNameTestReleaseWindow    = 10 * time.Minute
	playerNameTestCacheTTL         = 24 * time.Hour
	playerNameTestNegativeCacheTTL = 60 * time.Second
)

// newPlayerNameTestCtx 名字带前缀,避免与同包 data_logic_test.go 的 newTestSvcCtx 撞名。
func newPlayerNameTestCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, *fakePlayerNameStore) {
	t.Helper()
	mr := miniredis.RunT(t)

	c := config.Config{
		MappingRedis: redis.RedisConf{
			Host: mr.Addr(),
			Type: "node",
		},
		DevRedis: config.DevRedisConfig{
			Host: mr.Addr(),
			DB:   0,
		},
		PlayerLockTTLSec: 3,
		PlayerName: config.PlayerNameConfig{
			MaxOpenConn:      20,
			MaxIdleConn:      5,
			ReleaseWindow:    playerNameTestReleaseWindow,
			CacheTTL:         playerNameTestCacheTTL,
			NegativeCacheTTL: playerNameTestNegativeCacheTTL,
		},
	}

	r := routing.NewRouter(c)
	t.Cleanup(func() { r.Close() })

	fake := &fakePlayerNameStore{
		reserveOutcome: store.ReserveInserted,
		releaseOutcome: store.ReleaseDeleted,
		names:          map[uint64]string{},
	}

	return &svc.ServiceContext{Config: c, Router: r, PlayerNameStore: fake}, mr, fake
}

// ── Reserve ────────────────────────────────────────────────────

// 不合规的名字必须在**碰库之前**被拒:那是纯计算,库挂着也能给确定答案。
func TestReservePlayerName_InvalidNameNeverTouchesStore(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空白名", "   "},
		{"超过结构上限 32 字", strings.Repeat("云", playername.StructuralMaxRunes+1)},
		{"含空格(字符集不放行)", "hello world"},
		{"含标点", "云中君!"},
		{"敏感词", "官方客服"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, mr, fake := newPlayerNameTestCtx(t)

			result, owner, err := ReservePlayerName(context.Background(), svcCtx, 1001, tc.raw)
			require.NoError(t, err, "不合规是业务拒绝,不是故障,不能返回 error")
			assert.Equal(t, playername.ReserveInvalid, result)
			assert.Zero(t, owner, "owner 只在被他人占用时才有意义")
			assert.Empty(t, fake.reserveCalls, "不合规的名字不许打到库上")
			assert.False(t, mr.Exists(routing.PlayerNameKey(1001)), "更不许写进缓存")
		})
	}
}

// 登记成功后名字必须落进缓存;同时钉住 display/norm 的分工:
// 库里存的展示名保留大小写,判重键是小写。
func TestReservePlayerName_InsertedWritesCache(t *testing.T) {
	for _, outcome := range []store.ReserveOutcome{store.ReserveInserted, store.ReserveAlreadyOwned} {
		t.Run(outcome.String(), func(t *testing.T) {
			svcCtx, mr, fake := newPlayerNameTestCtx(t)
			fake.reserveOutcome = outcome
			before := uint64(time.Now().UnixMilli())

			result, owner, err := ReservePlayerName(context.Background(), svcCtx, 2001, "AbC云中君")
			require.NoError(t, err)
			assert.Equal(t, playername.ReserveOK, result, "幂等重试(AlreadyOwned)在 RPC 上也是成功")
			assert.Zero(t, owner)

			require.Len(t, fake.reserveCalls, 1)
			call := fake.reserveCalls[0]
			assert.Equal(t, uint64(2001), call.playerID)
			assert.Equal(t, "AbC云中君", call.name, "展示名保留大小写")
			assert.Equal(t, "abc云中君", call.norm, "判重键是小写")
			assert.GreaterOrEqual(t, call.nowMs, before, "登记时刻由 logic 显式给出,不能是 0")

			got, err := mr.Get(routing.PlayerNameKey(2001))
			require.NoError(t, err)
			assert.Equal(t, "AbC云中君", got, "缓存里放的是展示名")
			assert.InDelta(t, playerNameTestCacheTTL.Seconds(), mr.TTL(routing.PlayerNameKey(2001)).Seconds(), 1)
		})
	}
}

// 缓存是加速不是真相:写缓存失败绝不能把一次已经提交的建角翻成失败。
func TestReservePlayerName_CacheSetFailureStillSucceeds(t *testing.T) {
	svcCtx, mr, fake := newPlayerNameTestCtx(t)
	mr.Close() // Redis 整体不可用

	result, owner, err := ReservePlayerName(context.Background(), svcCtx, 2002, "云中君")
	require.NoError(t, err)
	assert.Equal(t, playername.ReserveOK, result)
	assert.Zero(t, owner)
	assert.Len(t, fake.reserveCalls, 1, "库仍然被正常写入")
}

// 被别人占用时必须把占用者回出去:login 靠它识别"丢了响应的重试"。
func TestReservePlayerName_TakenReturnsOwner(t *testing.T) {
	svcCtx, mr, fake := newPlayerNameTestCtx(t)
	fake.reserveOutcome = store.ReserveTaken
	fake.reserveOwner = 4242

	result, owner, err := ReservePlayerName(context.Background(), svcCtx, 2003, "云中君")
	require.NoError(t, err, "被占用是业务结果,不是故障")
	assert.Equal(t, playername.ReserveTaken, result)
	assert.Equal(t, uint64(4242), owner)
	assert.False(t, mr.Exists(routing.PlayerNameKey(2003)), "没占到就不许写自己的缓存")
}

func TestReservePlayerName_ConflictAndUnavailable(t *testing.T) {
	t.Run("同 id 已有别名 → ErrPlayerNameConflict", func(t *testing.T) {
		svcCtx, _, fake := newPlayerNameTestCtx(t)
		fake.reserveOutcome = store.ReserveConflict

		result, owner, err := ReservePlayerName(context.Background(), svcCtx, 2004, "云中君")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPlayerNameConflict)
		assert.Zero(t, result)
		assert.Zero(t, owner)
	})

	t.Run("store 未装配 → ErrPlayerNameStoreUnavailable", func(t *testing.T) {
		svcCtx, _, _ := newPlayerNameTestCtx(t)
		svcCtx.PlayerNameStore = nil

		_, _, err := ReservePlayerName(context.Background(), svcCtx, 2005, "云中君")
		assert.ErrorIs(t, err, ErrPlayerNameStoreUnavailable)
	})

	t.Run("store 未装配但名字不合规 → 仍先回 invalid", func(t *testing.T) {
		// 顺序很重要:库挂着的时候,"名字打错字"不该变成"服务不可用"。
		svcCtx, _, _ := newPlayerNameTestCtx(t)
		svcCtx.PlayerNameStore = nil

		result, _, err := ReservePlayerName(context.Background(), svcCtx, 2006, "云中君!")
		require.NoError(t, err)
		assert.Equal(t, playername.ReserveInvalid, result)
	})

	t.Run("SQL 报错原样上抛", func(t *testing.T) {
		svcCtx, _, fake := newPlayerNameTestCtx(t)
		fake.reserveErr = errors.New("boom")

		result, _, err := ReservePlayerName(context.Background(), svcCtx, 2007, "云中君")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "boom")
		assert.Zero(t, result, "故障不能被当成 result=0(成功)下发")
	})

	t.Run("player_id=0 被拒", func(t *testing.T) {
		svcCtx, _, fake := newPlayerNameTestCtx(t)

		_, _, err := ReservePlayerName(context.Background(), svcCtx, 0, "云中君")
		assert.ErrorIs(t, err, store.ErrPlayerNameInvalidArgument)
		assert.Empty(t, fake.reserveCalls)
	})
}

// ── Release ────────────────────────────────────────────────────

// 释放窗口是这条路径上唯一拦住"删掉在役角色名字"的东西,必须逐参数钉死。
func TestReleasePlayerName_WindowFloor(t *testing.T) {
	t.Run("无 admin token → now - ReleaseWindow", func(t *testing.T) {
		svcCtx, _, fake := newPlayerNameTestCtx(t)
		before := time.Now()

		require.NoError(t, ReleasePlayerName(context.Background(), svcCtx, 3001, "云中君", false))

		require.Len(t, fake.releaseCalls, 1)
		call := fake.releaseCalls[0]
		assert.Equal(t, uint64(3001), call.playerID)
		assert.Equal(t, "云中君", call.norm, "Release 只吃 norm,不吃展示名")
		wantFloor := before.Add(-playerNameTestReleaseWindow).UnixMilli()
		// 容差 5s:两次取时钟之间隔着一次 fake 调用,不可能逐毫秒相等。
		assert.InDelta(t, float64(wantFloor), float64(call.minCreatedMs), 5000)
	})

	t.Run("admin=true → 不限时间(0)", func(t *testing.T) {
		svcCtx, _, fake := newPlayerNameTestCtx(t)

		require.NoError(t, ReleasePlayerName(context.Background(), svcCtx, 3002, "云中君", true))

		require.Len(t, fake.releaseCalls, 1)
		assert.Zero(t, fake.releaseCalls[0].minCreatedMs, "过了 admin token 才允许不限登记时间")
	})

	t.Run("窗口漏配时 fail-closed,绝不退化成不限时间", func(t *testing.T) {
		// 0 等于"不限时间",任何能发 RPC 的内部调用方都能删掉在役角色的名字。
		// 配置漏填时正确的方向是"几乎什么都删不掉",而不是"什么都能删"。
		svcCtx, _, fake := newPlayerNameTestCtx(t)
		svcCtx.Config.PlayerName.ReleaseWindow = 0
		before := time.Now()

		require.NoError(t, ReleasePlayerName(context.Background(), svcCtx, 3009, "云中君", false))

		require.Len(t, fake.releaseCalls, 1)
		floor := fake.releaseCalls[0].minCreatedMs
		assert.NotZero(t, floor, "0 = 不限时间,正是这里最不该出现的值")
		assert.InDelta(t, float64(before.UnixMilli()), float64(floor), 5000, "下界应当就是'此刻'")
	})
}

// 删库成功之后才删缓存(顺序反了会被两步之间的一次读重新缓存回来)。
func TestReleasePlayerName_DeletedDropsCache(t *testing.T) {
	svcCtx, mr, fake := newPlayerNameTestCtx(t)
	fake.releaseOutcome = store.ReleaseDeleted
	require.NoError(t, mr.Set(routing.PlayerNameKey(3003), "云中君"))

	require.NoError(t, ReleasePlayerName(context.Background(), svcCtx, 3003, "云中君", false))
	assert.False(t, mr.Exists(routing.PlayerNameKey(3003)), "释放后缓存键必须消失")
}

func TestReleasePlayerName_Outcomes(t *testing.T) {
	t.Run("行不存在 → 幂等成功", func(t *testing.T) {
		// login 的补偿会发两次(立即 + 约 10s 后),第二次必然打空,不能报错。
		svcCtx, _, fake := newPlayerNameTestCtx(t)
		fake.releaseOutcome = store.ReleaseAbsent

		assert.NoError(t, ReleasePlayerName(context.Background(), svcCtx, 3004, "云中君", false))
	})

	t.Run("窗外 → ErrPlayerNameReleaseOutsideWindow", func(t *testing.T) {
		svcCtx, mr, fake := newPlayerNameTestCtx(t)
		fake.releaseOutcome = store.ReleaseOutsideWindow
		require.NoError(t, mr.Set(routing.PlayerNameKey(3005), "云中君"))

		err := ReleasePlayerName(context.Background(), svcCtx, 3005, "云中君", false)
		assert.ErrorIs(t, err, ErrPlayerNameReleaseOutsideWindow)
		assert.True(t, mr.Exists(routing.PlayerNameKey(3005)), "拒绝必须是零变更,缓存也不许动")
	})

	t.Run("store 未装配 → ErrPlayerNameStoreUnavailable", func(t *testing.T) {
		svcCtx, _, _ := newPlayerNameTestCtx(t)
		svcCtx.PlayerNameStore = nil

		assert.ErrorIs(t, ReleasePlayerName(context.Background(), svcCtx, 3006, "云中君", false),
			ErrPlayerNameStoreUnavailable)
	})
}

func TestReleasePlayerName_NameValidation(t *testing.T) {
	t.Run("归一化后为空或非法 → InvalidArgument 且不碰库", func(t *testing.T) {
		for _, raw := range []string{"", "   ", "云中君!", strings.Repeat("云", playername.StructuralMaxRunes+1)} {
			svcCtx, _, fake := newPlayerNameTestCtx(t)

			err := ReleasePlayerName(context.Background(), svcCtx, 3007, raw, false)
			assert.ErrorIsf(t, err, store.ErrPlayerNameInvalidArgument, "raw=%q", raw)
			assert.Emptyf(t, fake.releaseCalls, "raw=%q 不该打到库上", raw)
		}
	})

	t.Run("敏感词仍可释放", func(t *testing.T) {
		// Reserve 会拒敏感词,所以表里本不该有;但敏感词表是可替换的,换一版词库后
		// 早先合法登记的名字可能变成"敏感"。那时如果连释放都做不了,孤儿行就永远清不掉。
		svcCtx, _, fake := newPlayerNameTestCtx(t)

		require.NoError(t, ReleasePlayerName(context.Background(), svcCtx, 3008, "官方客服", true))
		require.Len(t, fake.releaseCalls, 1)
		assert.Equal(t, "官方客服", fake.releaseCalls[0].norm)
	})

	t.Run("player_id=0 被拒", func(t *testing.T) {
		svcCtx, _, fake := newPlayerNameTestCtx(t)

		assert.ErrorIs(t, ReleasePlayerName(context.Background(), svcCtx, 0, "云中君", true),
			store.ErrPlayerNameInvalidArgument)
		assert.Empty(t, fake.releaseCalls)
	})
}

// ── BatchGet ───────────────────────────────────────────────────

func TestBatchGetPlayerName_AllCachedSkipsStore(t *testing.T) {
	svcCtx, mr, fake := newPlayerNameTestCtx(t)
	require.NoError(t, mr.Set(routing.PlayerNameKey(4001), "云中君"))
	require.NoError(t, mr.Set(routing.PlayerNameKey(4002), "风伯"))

	names, err := BatchGetPlayerName(context.Background(), svcCtx, []uint64{4001, 4002})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]string{4001: "云中君", 4002: "风伯"}, names)
	assert.Empty(t, fake.batchCalls, "全部命中就不该回源")
}

func TestBatchGetPlayerName_PartialMissRefills(t *testing.T) {
	svcCtx, mr, fake := newPlayerNameTestCtx(t)
	require.NoError(t, mr.Set(routing.PlayerNameKey(4003), "云中君"))
	fake.names[4004] = "风伯"

	names, err := BatchGetPlayerName(context.Background(), svcCtx, []uint64{4003, 4004})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]string{4003: "云中君", 4004: "风伯"}, names)

	require.Len(t, fake.batchCalls, 1)
	assert.Equal(t, []uint64{4004}, fake.batchCalls[0], "只回源没命中的那一个")

	got, err := mr.Get(routing.PlayerNameKey(4004))
	require.NoError(t, err)
	assert.Equal(t, "风伯", got, "回源查到的名字要回填缓存")
	assert.InDelta(t, playerNameTestCacheTTL.Seconds(), mr.TTL(routing.PlayerNameKey(4004)).Seconds(), 1)
}

// 负缓存的全部价值就在这条用例上:查不到的 id 第二次不许再打库。
func TestBatchGetPlayerName_NegativeCacheStopsSecondLookup(t *testing.T) {
	svcCtx, mr, fake := newPlayerNameTestCtx(t)
	ctx := context.Background()

	names, err := BatchGetPlayerName(ctx, svcCtx, []uint64{4005})
	require.NoError(t, err)
	assert.Empty(t, names, "缺席的 id 不出现在结果里,而且不是错误")
	require.Len(t, fake.batchCalls, 1)

	assert.True(t, mr.Exists(routing.PlayerNameKey(4005)), "查不到也要记一笔(负缓存)")
	assert.InDelta(t, playerNameTestNegativeCacheTTL.Seconds(), mr.TTL(routing.PlayerNameKey(4005)).Seconds(), 1,
		"负缓存 TTL 必须短(60s),否则刚建的角色会长时间显示没名字")

	names, err = BatchGetPlayerName(ctx, svcCtx, []uint64{4005})
	require.NoError(t, err)
	assert.Empty(t, names)
	assert.Len(t, fake.batchCalls, 1, "第二次必须被负缓存挡住,不许再查库")
}

// SETNX 的方向性:负缓存永远盖不掉真名字。
func TestBatchGetPlayerName_NegativeCacheNeverOverwritesRealName(t *testing.T) {
	svcCtx, mr, _ := newPlayerNameTestCtx(t)
	require.NoError(t, mr.Set(routing.PlayerNameKey(4006), "云中君"))

	require.NoError(t, svcCtx.Router.SetPlayerNamesAbsent(context.Background(),
		[]uint64{4006}, playerNameTestNegativeCacheTTL))

	got, err := mr.Get(routing.PlayerNameKey(4006))
	require.NoError(t, err)
	assert.Equal(t, "云中君", got, "SET NX 必须失败,真名留下")
}

// 反方向:Reserve 写的是真值,必须能盖掉负缓存。
func TestReservePlayerName_OverwritesNegativeCache(t *testing.T) {
	svcCtx, mr, fake := newPlayerNameTestCtx(t)
	ctx := context.Background()
	key := routing.PlayerNameKey(4007)

	require.NoError(t, svcCtx.Router.SetPlayerNamesAbsent(ctx, []uint64{4007}, playerNameTestNegativeCacheTTL))

	// 先确认负缓存确实被识别成"库里没有":这一次不该回源。
	names, err := BatchGetPlayerName(ctx, svcCtx, []uint64{4007})
	require.NoError(t, err)
	assert.Empty(t, names)
	assert.Empty(t, fake.batchCalls)

	result, _, err := ReservePlayerName(ctx, svcCtx, 4007, "云中君")
	require.NoError(t, err)
	require.Equal(t, playername.ReserveOK, result)

	got, err := mr.Get(key)
	require.NoError(t, err)
	assert.Equal(t, "云中君", got, "Reserve 的 SET 必须覆盖负缓存")

	names, err = BatchGetPlayerName(ctx, svcCtx, []uint64{4007})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]string{4007: "云中君"}, names)
	assert.Empty(t, fake.batchCalls, "名字已在缓存里,仍然不必回源")
}

func TestBatchGetPlayerName_StoreErrorPropagates(t *testing.T) {
	svcCtx, _, fake := newPlayerNameTestCtx(t)
	fake.batchErr = errors.New("boom")

	names, err := BatchGetPlayerName(context.Background(), svcCtx, []uint64{4008})
	require.Error(t, err)
	assert.Nil(t, names, "不回半份结果:调用方分不清'没名字'和'这次没查出来'")
}

func TestBatchGetPlayerName_StoreUnavailableOnMiss(t *testing.T) {
	svcCtx, _, _ := newPlayerNameTestCtx(t)
	svcCtx.PlayerNameStore = nil

	_, err := BatchGetPlayerName(context.Background(), svcCtx, []uint64{4009})
	assert.ErrorIs(t, err, ErrPlayerNameStoreUnavailable)

	// 一个都不需要回源时,store 没装配也不该报错(空请求是合法的)。
	names, err := BatchGetPlayerName(context.Background(), svcCtx, nil)
	require.NoError(t, err)
	assert.Empty(t, names)
}

// Redis 挂了只意味着"全部未命中",读接口不该整体失败。
func TestBatchGetPlayerName_CacheErrorDegradesToStore(t *testing.T) {
	svcCtx, mr, fake := newPlayerNameTestCtx(t)
	fake.names[4010] = "云中君"
	mr.Close()

	names, err := BatchGetPlayerName(context.Background(), svcCtx, []uint64{4010})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]string{4010: "云中君"}, names)
	require.Len(t, fake.batchCalls, 1)
	assert.Equal(t, []uint64{4010}, fake.batchCalls[0])
}

func TestBatchGetPlayerName_DedupAndDropsZero(t *testing.T) {
	svcCtx, _, fake := newPlayerNameTestCtx(t)
	fake.names[4011] = "云中君"

	names, err := BatchGetPlayerName(context.Background(), svcCtx,
		[]uint64{0, 4011, 4011, 0, 4012, 4011})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]string{4011: "云中君"}, names, "4012 缺席,不出现在结果里")

	require.Len(t, fake.batchCalls, 1)
	assert.Equal(t, []uint64{4011, 4012}, fake.batchCalls[0],
		"0 被丢掉、重复只查一次、顺序按首次出现")
}

// 去重要发生在限额判定**之前**:上游很容易把同一个帮主 id 重复塞进来,
// 按原始长度拒绝会把合法请求判成超限。
func TestBatchGetPlayerName_RejectsOversizeBatch(t *testing.T) {
	svcCtx, _, fake := newPlayerNameTestCtx(t)

	oversize := make([]uint64, 0, store.PlayerNameBatchLimit+1)
	for i := 1; i <= store.PlayerNameBatchLimit+1; i++ {
		oversize = append(oversize, uint64(i))
	}
	_, err := BatchGetPlayerName(context.Background(), svcCtx, oversize)
	assert.ErrorIs(t, err, store.ErrPlayerNameBatchTooLarge)
	assert.Empty(t, fake.batchCalls, "超限请求不许打到库上")

	// 同样多的元素、但去重后正好等于上限 → 必须放行。
	atLimit := append(oversize[:store.PlayerNameBatchLimit:store.PlayerNameBatchLimit], oversize[0])
	_, err = BatchGetPlayerName(context.Background(), svcCtx, atLimit)
	require.NoError(t, err)
	require.Len(t, fake.batchCalls, 1)
	assert.Len(t, fake.batchCalls[0], store.PlayerNameBatchLimit)
}
