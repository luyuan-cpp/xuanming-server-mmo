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
	"testing"

	"scene_manager/internal/config"
	"scene_manager/internal/svc"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// seedChannels 手工铺一组频道及其人数,绕开 initWorldScenesForZone。
func seedChannels(t *testing.T, sc *svc.ServiceContext, zoneID uint32, confID uint64, playersByScene map[uint64]int64) {
	t.Helper()
	for sceneID, players := range playersByScene {
		_, err := sc.Redis.Sadd(worldChannelsKey(zoneID, confID), strconv.FormatUint(sceneID, 10))
		require.NoError(t, err)
		require.NoError(t, sc.Redis.Set(sceneNodeKey(sceneID), "10"))
		require.NoError(t, sc.Redis.Set(sceneZoneKey(sceneID), fmt.Sprintf("%d", zoneID)))
		require.NoError(t, sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneID), strconv.FormatInt(players, 10)))
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
	assert.Equal(t, 3, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID))
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
	assert.Equal(t, 2, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID))
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
	assert.Equal(t, 2, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID))
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
	assert.Equal(t, 1, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID))

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
	assert.Equal(t, 1, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID))
}

// MinChannelsPerMap 配成 0 也要被钳回 1。
func TestAutoscale_MinChannelsIsClampedToOne(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Config.WorldAutoscale.MinChannelsPerMap = 0
	sc.Redis.Zadd(testLoadKey(), 0, "10")
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 5})

	_, in := AutoscaleWorldChannelsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 0, in)
	assert.GreaterOrEqual(t, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID), 1)
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
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, 102), "12")
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
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, 102), "0")
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

	assert.Equal(t, 4, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID), "首次访问用配置播种")

	setDesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID, 7)
	assert.Equal(t, 7, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID))

	// 配置改了也不该覆盖 Redis 里的值。
	sc.Config.WorldChannelCount = 2
	assert.Equal(t, 7, DesiredWorldChannelCount(sc, testZoneId, autoscaleTestConfID))
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
