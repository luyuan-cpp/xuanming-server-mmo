package logic

// 世界频道占 Agones rooms 名额的单元测试。
//
// 这条路径是补"大世界人数涨了 Pod 不跟着扩"那个缺口时加的:世界频道以前
// 完全不预占房间名额,rooms Counter 少算,而 FleetAutoscaler 用的正是
// Counter=rooms。这里覆盖它的三条分支 + 频道迁移时的名额转移。

import (
	"context"
	"testing"

	"scene_manager/internal/constants"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 正常路径:按哈希目标节点预占,并写下 scene:{id}:agones_gs。
func TestWorldChannel_ReservesRoomOnHashTargetNode(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-a", podIP: "10.0.0.8", state: "Ready", capacity: 4},
		&fakeGameServer{name: "gs-b", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)

	node, ok := ReserveAgonesRoomForWorldChannel(context.Background(), sc, 5001, "10", testZoneId)
	require.True(t, ok)
	assert.Equal(t, "10", node, "哈希目标节点有容量时必须落在它上面,否则 rebalance 会一直想搬回去")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-a"))
	assert.EqualValues(t, 0, alloc.roomsOf("gs-b"))

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5001))
	assert.Equal(t, "gs-a", gs, "必须在调 CreateScene 之前记下 gs,否则崩溃后找不回该向谁归还")
}

// 哈希目标满了 -> 回落自由分配,落点可能不是哈希目标(rebalance 之后会搬回去)。
func TestWorldChannel_FallsBackToFreeAllocationWhenHashTargetIsFull(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-full", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 2},
		&fakeGameServer{name: "gs-free", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)

	node, ok := ReserveAgonesRoomForWorldChannel(context.Background(), sc, 5002, "10", testZoneId)
	require.True(t, ok)
	assert.Equal(t, "20", node)
	assert.EqualValues(t, 2, alloc.roomsOf("gs-full"), "满了的节点不能被超卖")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-free"))
}

// 全都没容量 -> fail-closed,调用方不建这个频道。
func TestWorldChannel_FailsClosedWhenNoCapacityAnywhere(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-full", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 2},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)

	node, ok := ReserveAgonesRoomForWorldChannel(context.Background(), sc, 5003, "10", testZoneId)
	assert.False(t, ok)
	assert.Equal(t, "", node)

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5003))
	assert.Equal(t, "", gs, "没占到名额就不能留下映射")
}

// Agones 关闭时完全惰性:原样返回首选节点,不写任何映射。
func TestWorldChannel_InertWhenAgonesDisabled(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	node, ok := ReserveAgonesRoomForWorldChannel(context.Background(), sc, 5004, "10", testZoneId)
	assert.True(t, ok)
	assert.Equal(t, "10", node)

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5004))
	assert.Equal(t, "", gs)
}

// ── 频道迁移时的名额转移 ────────────────────────────────────────────────

// 搬家:新节点占上、旧节点还回去、映射更新。
func TestWorldChannel_TransferMovesRoomToNewNode(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(5005), "gs-old"))

	TransferAgonesRoomForScene(context.Background(), sc, 5005, "20", testZoneId)

	assert.EqualValues(t, 0, alloc.roomsOf("gs-old"), "旧节点的名额必须还回去,否则容量凭空蒸发")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-new"), "新节点必须记账,否则容量被超卖")

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5005))
	assert.Equal(t, "gs-new", gs)
}

// 新节点占不到 -> **不释放旧的**。
// 释放了等于这个频道不占任何容量,比多占一份更糟。
func TestWorldChannel_TransferKeepsOldReservationWhenNewNodeIsFull(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Allocated", count: 2, capacity: 2},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(5006), "gs-old"))

	TransferAgonesRoomForScene(context.Background(), sc, 5006, "20", testZoneId)

	assert.EqualValues(t, 1, alloc.roomsOf("gs-old"), "占不到新的就不能释放旧的")
	assert.EqualValues(t, 2, alloc.roomsOf("gs-new"), "满了的节点不能被超卖")

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5006))
	assert.Equal(t, "gs-old", gs, "映射必须仍指向真正持有名额的那个 gs")
}

// Agones 关闭时转移是 no-op。
func TestWorldChannel_TransferInertWhenAgonesDisabled(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(5007), "gs-old"))

	TransferAgonesRoomForScene(context.Background(), sc, 5007, "20", testZoneId)

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5007))
	assert.Equal(t, "gs-old", gs)
}

// 排空一个世界频道时,名额要归还。
// (finishDrainedChannel 读 agones_gs 再释放,这里验证端到端不漏。)
func TestWorldChannel_DrainReleasesRoom(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-a", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)

	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(102), "gs-a"))

	ctx := context.Background()
	_, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	require.Equal(t, 1, in)

	// 人走干净后下一拍收尾。
	sc.Redis.Set(sceneNodeKey(102), "10")
	sc.Redis.Set("instance:102:player_count", "0")
	AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)

	assert.EqualValues(t, 1, alloc.roomsOf("gs-a"), "排空销毁的频道必须把名额还回去")
}
