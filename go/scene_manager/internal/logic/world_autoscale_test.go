package logic

// 大世界频道按人数自动扩缩容的单元测试。
//
// 强制迁移那一步在 C++ 侧(PlayerLifecycleSystem::BeginSceneDrain),这里
// 只能验证 SceneManager 的决策与状态机:摘路由、期望频道数、排空索引、
// 收敛清理。"玩家真的被改派到了另一个频道"必须由 dev 集群 E2E 验证。

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"

	"scene_manager/internal/config"
	"scene_manager/internal/svc"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx"
)

const autoscaleTestConfID = uint64(1)

// newAutoscaleCtx 造一个打开自动扩缩容的 svcCtx,并把 confID 注册成大世界地图。
func newAutoscaleCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.WorldAutoscale = config.WorldAutoscaleConfig{
		Enabled:                 true,
		CheckIntervalSeconds:    30,
		ScaleOutPlayerThreshold: 2000,
		ScaleInPlayerThreshold:  100,
		MinChannelsPerMap:       1,
		MaxChannelsPerMap:       16,
		CooldownSeconds:         120,
		DrainTimeoutSeconds:     300,
	}
	// 让 worldConfIds() 认得这张图。
	t.Cleanup(SetWorldConfIdsForTest([]uint64{autoscaleTestConfID}))
	return sc, mr
}

// mustDesiredWorldChannelCount 读期望频道数;miniredis 正常时不该读失败,失败即用例前提不成立。
func mustDesiredWorldChannelCount(t *testing.T, sc *svc.ServiceContext, zoneID uint32, confID uint64) int {
	t.Helper()
	n, err := DesiredWorldChannelCount(sc, zoneID, confID)
	require.NoError(t, err)
	return n
}

// setDesiredWorldChannelCount 直接写期望频道数,并钳到 [min, max]。2026-11-11 起只给用例铺数据:生产路径的扩容与缩容都在
// 提交脚本里比对旧值后再写(luaCommitWorldScaleOut、luaBeginDrainWorldChannel),这个函数没有生产调用方了,从
// world_autoscale.go 挪到这里,签名与行为不变。
func setDesiredWorldChannelCount(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, n int) {
	n = clampDesiredWorldChannelCount(svcCtx, n)
	if err := svcCtx.Redis.Hset(worldDesiredChannelsKey(zoneID), strconv.FormatUint(confID, 10), strconv.Itoa(n)); err != nil {
		logx.Errorf("[WorldAutoscale] set desired channels zone=%d conf=%d=%d failed: %v", zoneID, confID, n, err)
	}
}

// seedChannels 手工铺一组频道及其人数,绕开 initWorldScenesForZone。
func seedChannels(t *testing.T, sc *svc.ServiceContext, zoneID uint32, confID uint64, playersByScene map[uint64]int64) {
	t.Helper()
	for sceneID, players := range playersByScene {
		_, err := sc.Redis.Sadd(worldChannelsKey(zoneID, confID), strconv.FormatUint(sceneID, 10))
		require.NoError(t, err)
		require.NoError(t, sc.Redis.Set(sceneNodeKey(sceneID), "10"))
		require.NoError(t, sc.Redis.Set(sceneZoneKey(sceneID), fmt.Sprintf("%d", zoneID)))
		seedSceneMembersVia(sc, sceneID, int(players))
	}
	// 期望频道数与实际铺的数量对齐,否则 initWorldScenesForZone 会补建。
	setDesiredWorldChannelCount(sc, zoneID, confID, len(playersByScene))
}

// ── 扩容 ────────────────────────────────────────────────────────────────

// 所有频道都到 2000 才扩容。
func TestAutoscale_ScalesOutWhenEveryChannelIsFull(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{
		101: 2000,
		102: 2100,
	})

	out, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 1, out)
	assert.Equal(t, 0, in)
	assert.Equal(t, 3, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
}

// 只要有一个频道没到线就不扩 —— 否则负载不均时会扩出一堆空频道。
func TestAutoscale_DoesNotScaleOutWhenOneChannelHasRoom(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{
		101: 2500,
		102: 1200, // 还有空位
	})

	out, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 0, out)
	assert.Equal(t, 0, in)
	assert.Equal(t, 2, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
}

// 到达 MaxChannelsPerMap 之后不再扩,只告警。
func TestAutoscale_RespectsMaxChannels(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Config.WorldAutoscale.MaxChannelsPerMap = 2
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{
		101: 3000,
		102: 3000,
	})

	out, _ := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 0, out)
	assert.Equal(t, 2, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
}

// ── 缩容 ────────────────────────────────────────────────────────────────

// 人少的频道被摘出路由并开始排空。
func TestAutoscale_DrainsUnderpopulatedChannel(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{
		101: 1500,
		102: 40, // < 100
	})

	out, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 0, out)
	assert.Equal(t, 1, in)

	// 先摘路由:新玩家不能再被分进正在销毁的频道。
	members, _ := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	assert.NotContains(t, members, "102")
	assert.Contains(t, members, "101")

	// 期望频道数同步减 1,否则 initWorldScenesForZone 会立刻补回来。
	assert.Equal(t, 1, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))

	// 排空索引里有它,后续 tick 才找得回来继续收敛。
	draining, _ := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	assert.Contains(t, draining, "102")
	assert.True(t, mr.Exists(sceneDrainingKey(102)))
}

// **最少一个频道**:只剩一个时,再空也不缩。
func TestAutoscale_NeverDrainsTheLastChannel(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{
		101: 0, // 一个人都没有
	})

	out, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 0, out)
	assert.Equal(t, 0, in)

	members, _ := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	assert.Contains(t, members, "101", "the last channel of a world map must never be drained")
	assert.Equal(t, 1, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
}

// MinChannelsPerMap 配成 0 也要被钳回 1。
func TestAutoscale_MinChannelsIsClampedToOne(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Config.WorldAutoscale.MinChannelsPerMap = 0
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 5})

	_, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 0, in)
	assert.GreaterOrEqual(t, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID), 1)
}

// 其余频道装不下就不缩 —— 否则把人挤过去立刻触发扩容,来回抖。
func TestAutoscale_RefusesDrainWithoutHeadroom(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{
		101: 1990, // 只剩 10 个位置
		102: 90,   // <100,但 90 个人塞不进去
	})

	_, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 0, in)

	members, _ := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	assert.Contains(t, members, "102")
}

// hasHeadroomFor 的边界:刚好装得下 / 差一个。
func TestHasHeadroomFor_Boundaries(t *testing.T) {
	victim := channelLoad{sceneID64: 2, players: 100}
	loads := []channelLoad{victim, {sceneID64: 1, players: 1900}}
	assert.True(t, hasHeadroomFor(loads, victim, 2000), "1900+100 == 2000 刚好装下")

	loads = []channelLoad{victim, {sceneID64: 1, players: 1901}}
	assert.False(t, hasHeadroomFor(loads, victim, 2000), "差 1 个位置就不能缩")
}

// ── 收敛 ────────────────────────────────────────────────────────────────

// 排空中的频道:还有人时保持排空态,不清状态。
func TestAutoscale_DrainingChannelWithResidentsStaysDraining(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})

	ctx := context.Background()
	AutoscaleWorldChannelsForZone(ctx, sc, testZoneId) // 开始排空 102

	// 人还没走完。
	seedSceneMembersVia(sc, 102, 12)
	AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)

	assert.True(t, mr.Exists(sceneNodeKey(102)), "还有人时不能清掉 scene->node 映射")
	draining, _ := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	assert.Contains(t, draining, "102")
}

// 人走干净之后,下一拍把状态全部清掉。
func TestAutoscale_DrainedChannelIsCleanedUp(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})
	sc.Redis.Sadd(nodeScenesKey(testZoneId, "10"), "102")
	sc.Redis.Set(nodeSceneCountKey(testZoneId, "10"), "2")

	ctx := context.Background()
	AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)

	// C++ 侧把人改派走了。
	seedSceneMembersVia(sc, 102, 0)
	AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)

	assert.False(t, mr.Exists(sceneNodeKey(102)))
	assert.False(t, mr.Exists(sceneZoneKey(102)))
	assert.False(t, mr.Exists(sceneDrainingKey(102)))

	draining, _ := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	assert.NotContains(t, draining, "102")

	nodeScenes, _ := sc.Redis.Smembers(nodeScenesKey(testZoneId, "10"))
	assert.NotContains(t, nodeScenes, "102")

	cnt, _ := sc.Redis.Get(nodeSceneCountKey(testZoneId, "10"))
	assert.Equal(t, "1", cnt, "node scene_count 必须减回去")
}

// 期望频道数的权威在 Redis:改配置不会覆盖已经伸缩过的值。
// 这一条是缩容能不能成立的关键 —— 直接读配置的话 initWorldScenesForZone
// 会把刚摘掉的频道补回来。
func TestDesiredWorldChannelCount_RedisWinsOverConfig(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Config.WorldChannelCount = 4

	assert.Equal(t, 4, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID), "首次访问用配置播种")

	setDesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID, 7)
	assert.Equal(t, 7, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))

	// 配置改了也不该覆盖 Redis 里的值。
	sc.Config.WorldChannelCount = 2
	assert.Equal(t, 7, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
}

// 冷却窗口内不做任何伸缩决策。
func TestAutoscale_CooldownSuppressesDecisions(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 3000, 102: 3000})

	ctx := context.Background()
	out1, _ := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	require.Equal(t, 1, out1)

	// 紧接着再跑一轮:冷却期内不能再扩。
	out2, _ := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	assert.Equal(t, 0, out2)
}

// 关闭时 StartWorldAutoscaler 不起 goroutine(不崩、不动 Redis 即可)。
func TestAutoscale_DisabledIsInert(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Config.WorldAutoscale.Enabled = false
	StartWorldAutoscaler(context.Background(), sc)
	// 没有可断言的副作用 —— 这条用例保证的是"不 panic 也不起循环"。
}

// ── 重叠领导者:判冷却、比对期望数、写新值在一段脚本里(2026-11-11,infra-reconnect-overview.md §4.1 R7) ──────

// hookBeforeFirstDesiredRead 在这一轮第一次读期望频道数(Go 侧的 HGET,在冷却预检之后)之前跑一次 mutate,模拟另一个领导者
// 恰好在本轮读完冷却、还没读期望数时提交了一次伸缩。钩子跑在 miniredis 的服务端协程上、还没拿库锁,mutate 里可以调 mr.*
// (同 identHookOnceBeforeEval);只触发一次 —— 之后脚本里的 HGET 也经过钩子,那时库锁在手,再调 mr.* 会死锁。
// mutate 里只能用 t.Errorf,不能用 require(FailNow 必须在用例协程里调)。
func hookBeforeFirstDesiredRead(t *testing.T, mr *miniredis.Miniredis, zoneID uint32, mutate func()) *atomic.Bool {
	t.Helper()
	var fired atomic.Bool
	key := worldDesiredChannelsKey(zoneID)
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if cmd != "HGET" || len(args) == 0 || args[0] != key || !fired.CompareAndSwap(false, true) {
			return false
		}
		mutate()
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	return &fired
}

// 两个领导者都过了冷却预检。另一个领导者恰在本轮读完冷却、还没读期望数时提交了扩容(期望数 2 → 3,起冷却);本轮读到 3,
// 提交脚本看到冷却标记不是本次的令牌,什么都不写:期望数停在 3,冷却标记仍是对方的。
// 修复前红:本轮在脚本外把读到的 3 写成 4(不再看冷却),一轮多扩一条频道。
func TestAutoscale_ScaleOutYieldsToConcurrentLeaderCommit(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 3000, 102: 3000})
	cooldownKey := worldAutoscaleCooldownKey(testZoneId, autoscaleTestConfID)

	fired := hookBeforeFirstDesiredRead(t, mr, testZoneId, func() {
		mr.HSet(worldDesiredChannelsKey(testZoneId), strconv.FormatUint(autoscaleTestConfID, 10), "3")
		if err := mr.Set(cooldownKey, "other-leader"); err != nil {
			t.Errorf("simulate the other leader's cooldown: %v", err)
		}
	})
	out, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	require.True(t, fired.Load(), "前提不成立:这一轮没有读期望频道数")
	assert.Zero(t, out)
	assert.Zero(t, in)
	assert.Equal(t, 3, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID), "对方写的 3 不能再被加一")
	cooldown, err := mr.Get(cooldownKey)
	require.NoError(t, err)
	assert.Equal(t, "other-leader", cooldown, "冷却标记仍是先提交的那一方的")
}

// 缩容同理。另一个领导者恰在本轮读完冷却、还没读期望数时排空了 103(移进排空集合,期望数 3 → 2,起冷却);本轮挑中的 102
// 不能再摘:路由集合里仍有 101、102,期望数停在 2,102 没有排空标记。
// 修复前红:本轮读到 2,排空脚本照样摘掉 102、把期望数写成 1,一轮里缩掉两条。
func TestAutoscale_ScaleInYieldsToConcurrentLeaderCommit(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40, 103: 50})

	fired := hookBeforeFirstDesiredRead(t, mr, testZoneId, func() {
		if _, err := mr.SRem(worldChannelsKey(testZoneId, autoscaleTestConfID), "103"); err != nil {
			t.Errorf("simulate the other leader's drain (SREM): %v", err)
		}
		if _, err := mr.SAdd(worldDrainingSetKey(testZoneId, autoscaleTestConfID), "103"); err != nil {
			t.Errorf("simulate the other leader's drain (SADD): %v", err)
		}
		mr.HSet(worldDesiredChannelsKey(testZoneId), strconv.FormatUint(autoscaleTestConfID, 10), "2")
		if err := mr.Set(worldAutoscaleCooldownKey(testZoneId, autoscaleTestConfID), "other-leader"); err != nil {
			t.Errorf("simulate the other leader's cooldown: %v", err)
		}
	})
	_, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	require.True(t, fired.Load(), "前提不成立:这一轮没有读期望频道数")
	assert.Zero(t, in)
	routed, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"101", "102"}, routed, "本轮挑中的 102 不能再摘")
	assert.Equal(t, 2, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
	assert.False(t, mr.Exists(sceneDrainingKey(102)))
}

// 扩容提交脚本本身:同一令牌重发(go-zero 句柄读超时后重发同一条 EVAL)回已提交、期望数只加一次;冷却期内别的令牌回冷却;
// 冷却过后旧值对不上回期望数已变,不写、不起冷却。去掉令牌比对的写法红在第二次运行上(回冷却)。
func TestCommitWorldScaleOutScript_ReplayCooldownAndStaleRead(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	setDesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID, 2)
	conf := strconv.FormatUint(autoscaleTestConfID, 10)
	cooldownKey := worldAutoscaleCooldownKey(testZoneId, autoscaleTestConfID)
	keys := []string{cooldownKey, worldDesiredChannelsKey(testZoneId)}
	run := func(oldDesired, newDesired, token string) int64 {
		t.Helper()
		res, err := sc.Redis.Eval(luaCommitWorldScaleOut, keys, conf, oldDesired, newDesired, int64(120), token)
		require.NoError(t, err)
		return toInt64(res)
	}

	for i := range 2 {
		assert.EqualValues(t, worldScaleCommitted, run("2", "3", "token-1"), "run %d of the same command", i+1)
	}
	assert.Equal(t, 3, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID), "重发不能再加一次")
	assert.Positive(t, mr.TTL(cooldownKey), "冷却标记要带 TTL")

	assert.EqualValues(t, worldScaleCooldown, run("3", "4", "token-2"))
	assert.Equal(t, 3, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))

	mr.Del(cooldownKey)
	assert.EqualValues(t, worldScaleDesiredChanged, run("2", "3", "token-3"))
	assert.Equal(t, 3, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID))
	assert.False(t, mr.Exists(cooldownKey), "没提交就不起冷却")
}

// 排空脚本:同一令牌重发回已移动,一个键都不再动(此前重发回 0,本拍不推进排空);冷却标记与期望数在同一步里写好。
// 冷却过后旧值对不上回期望数已变,频道留在路由集合里。去掉令牌比对的写法红在第二次运行上(回冷却)。
func TestBeginDrainScript_ReplayAndStaleRead(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})
	conf := strconv.FormatUint(autoscaleTestConfID, 10)
	cooldownKey := worldAutoscaleCooldownKey(testZoneId, autoscaleTestConfID)
	run := func(sceneID string, sceneID64 uint64, newDesired int, oldDesired, token string) int64 {
		t.Helper()
		res, err := sc.Redis.Eval(luaBeginDrainWorldChannel,
			[]string{
				worldChannelsKey(testZoneId, autoscaleTestConfID),
				worldDrainingSetKey(testZoneId, autoscaleTestConfID),
				sceneDrainingKey(sceneID64),
				worldDesiredChannelsKey(testZoneId),
				cooldownKey,
			},
			sceneID, int64(300), conf, newDesired, oldDesired, int64(120), token)
		require.NoError(t, err)
		return toInt64(res)
	}

	for i := range 2 {
		assert.EqualValues(t, worldScaleCommitted, run("102", 102, 1, "2", "token-1"), "run %d of the same command", i+1)
	}
	routed, err := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	assert.Equal(t, []string{"101"}, routed)
	draining, err := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	assert.Equal(t, []string{"102"}, draining)
	assert.Equal(t, 1, mustDesiredWorldChannelCount(t, sc, testZoneId, autoscaleTestConfID), "重发不能再减一次")
	cooldown, err := mr.Get(cooldownKey)
	require.NoError(t, err)
	assert.Equal(t, "token-1", cooldown, "冷却标记在同一步里写,值是这次决策的令牌")
	assert.Positive(t, mr.TTL(cooldownKey))

	mr.Del(cooldownKey)
	assert.EqualValues(t, worldScaleDesiredChanged, run("101", 101, 1, "2", "token-2"))
	routed, err = sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	require.NoError(t, err)
	assert.Equal(t, []string{"101"}, routed, "旧值对不上时不能摘")
	assert.False(t, mr.Exists(cooldownKey), "没提交就不起冷却")
}
