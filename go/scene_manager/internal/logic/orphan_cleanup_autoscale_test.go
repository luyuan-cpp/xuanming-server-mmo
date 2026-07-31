package logic

// 地图从 World 表里删掉时,孤儿清理必须把**自动伸缩引入的那几样状态**也带走:
// Agones 名额、期望频道数、排空索引、以及正在排空的那些频道本身。
//
// 排空的第一步就是把频道从 world_channels 摘掉,所以只看 world_channels 的
// 清理逻辑会把它们整个漏掉 —— 而且此时 confId 已经不在 World 表里,
// sweepDrainingWorldChannels 也不会再扫到,那些 key 会永久残留。

import (
	"context"
	"strconv"
	"testing"

	"scene_manager/internal/constants"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 被删地图的普通频道:归还 Agones 名额、清 agones_gs、清期望频道数。
func TestOrphanCleanup_ReleasesAgonesRoomAndDesiredCount(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-a", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)

	// conf 777 不在 World 表里(newAutoscaleCtx 只登记了 autoscaleTestConfID)。
	const deadConf uint64 = 777
	seedChannels(t, sc, testZoneId, deadConf, map[uint64]int64{701: 0})
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(701), "gs-a"))
	require.NoError(t, sc.Redis.Hset(worldDesiredChannelsKey(testZoneId),
		strconv.FormatUint(deadConf, 10), "8"))

	removed := CleanupOrphanWorldChannels(ctx, sc)
	require.Equal(t, 1, removed)

	assert.EqualValues(t, 1, alloc.roomsOf("gs-a"), "被删地图的频道必须把 Agones 名额还回去")

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(701))
	assert.Equal(t, "", gs, "agones_gs 映射不能泄漏")

	desired, _ := sc.Redis.Hget(worldDesiredChannelsKey(testZoneId), strconv.FormatUint(deadConf, 10))
	assert.Equal(t, "", desired, "期望频道数不清的话,地图加回来会复活旧的伸缩结果")
}

// 正在排空的频道也必须被清掉 —— 它已经不在 world_channels 里了。
func TestOrphanCleanup_AlsoCleansDrainingChannels(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-a", podIP: "10.0.0.8", state: "Allocated", count: 3, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)

	const deadConf uint64 = 778
	// 留一个还在路由里的频道,外加一个已经摘出去排空中的。
	seedChannels(t, sc, testZoneId, deadConf, map[uint64]int64{801: 0})
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(801), "gs-a"))

	const drainingScene uint64 = 802
	drainingStr := strconv.FormatUint(drainingScene, 10)
	_, err := sc.Redis.Sadd(worldDrainingSetKey(testZoneId, deadConf), drainingStr)
	require.NoError(t, err)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(drainingScene), "10"))
	require.NoError(t, sc.Redis.Set(sceneZoneKey(drainingScene),
		strconv.FormatUint(uint64(testZoneId), 10)))
	require.NoError(t, sc.Redis.Setex(sceneDrainingKey(drainingScene), "1", 300))
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(drainingScene), "gs-a"))

	removed := CleanupOrphanWorldChannels(ctx, sc)
	require.Equal(t, 1, removed)

	node, _ := sc.Redis.Get(sceneNodeKey(drainingScene))
	assert.Equal(t, "", node, "排空中的频道必须一起清掉,否则它的 key 永久残留")

	mark, _ := sc.Redis.Exists(sceneDrainingKey(drainingScene))
	assert.False(t, mark, "排空标记必须清掉")

	// 两个频道各还一份名额:3 - 2 = 1。
	assert.EqualValues(t, 1, alloc.roomsOf("gs-a"))

	drainSet, _ := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, deadConf))
	assert.Empty(t, drainSet, "排空索引本身也不能泄漏")
}

// 仍在 World 表里的地图一个都不能动 —— 这是既有的安全规则,别被新逻辑破坏。
func TestOrphanCleanup_LeavesValidConfUntouched(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 500})
	require.NoError(t, sc.Redis.Hset(worldDesiredChannelsKey(testZoneId),
		strconv.FormatUint(autoscaleTestConfID, 10), "3"))

	assert.Equal(t, 0, CleanupOrphanWorldChannels(ctx, sc))

	node, _ := sc.Redis.Get(sceneNodeKey(101))
	assert.NotEqual(t, "", node)

	desired, _ := sc.Redis.Hget(worldDesiredChannelsKey(testZoneId),
		strconv.FormatUint(autoscaleTestConfID, 10))
	assert.Equal(t, "3", desired, "有效地图的期望频道数不能被清掉")
}
