package logic

// 世界频道占 Agones rooms 名额的单元测试。
//
// 这条路径是补"大世界人数涨了 Pod 不跟着扩"那个缺口时加的:世界频道以前
// 完全不预占房间名额,rooms 少算,而 FleetAutoscaler 按的正是 rooms。
// 这里覆盖它的三条分支 + 频道迁移时的名额转移。名额是 GameServer 上按身份的条目,
// 记录是「GameServer/条目」(docs/design/agones-scene-node-high-density.md §13)。

import (
	"context"
	"testing"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 正常路径:按哈希目标节点预占,交回占到的 GameServer 与条目。agones_gs 由登记脚本写(scene_registration_test.go)。
func TestWorldChannel_ReservesRoomOnHashTargetNode(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-a", podIP: "10.0.0.8", state: "Ready", capacity: 4},
		&fakeGameServer{name: "gs-b", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)

	node, placement, ok := ReserveAgonesRoomForWorldChannel(context.Background(), sc, 5001, "10", testZoneId)
	require.True(t, ok)
	assert.Equal(t, "10", node, "哈希目标节点有容量时必须落在它上面,否则 rebalance 会一直想搬回去")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-a"))
	assert.EqualValues(t, 0, alloc.roomsOf("gs-b"))
	require.NotNil(t, placement)
	assert.Equal(t, "gs-a", placement.GameServerName, "登记要拿它写 agones_gs,否则崩溃后找不回该向谁归还")
	assert.Equal(t, []string{placement.Entry}, alloc.entriesOf("gs-a"))
	assert.Equal(t, []string{placement.Entry}, agonesHoldsOf(t, sc, testZoneId), "登记之前在途占用一直在")

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5001))
	assert.Equal(t, "", gs, "预占不写记录:agones_gs 与频道其余状态在同一段登记脚本里写")
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

	node, placement, ok := ReserveAgonesRoomForWorldChannel(context.Background(), sc, 5002, "10", testZoneId)
	require.True(t, ok)
	assert.Equal(t, "20", node)
	require.NotNil(t, placement)
	assert.Equal(t, "gs-free", placement.GameServerName)
	assert.EqualValues(t, 2, alloc.roomsOf("gs-full"), "满了的节点不能被超卖")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-free"))
	assert.Equal(t, []string{placement.Entry}, agonesHoldsOf(t, sc, testZoneId), "哈希目标那一次的在途占用已放弃")
}

// 全都没容量 -> fail-closed,调用方不建这个频道。
func TestWorldChannel_FailsClosedWhenNoCapacityAnywhere(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-full", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 2},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)

	node, placement, ok := ReserveAgonesRoomForWorldChannel(context.Background(), sc, 5003, "10", testZoneId)
	assert.False(t, ok)
	assert.Equal(t, "", node)
	assert.Nil(t, placement)

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5003))
	assert.Equal(t, "", gs, "没占到名额就不能留下映射")
	assert.Empty(t, agonesHoldsOf(t, sc, testZoneId), "没占到名额就不能留下在途占用")
}

// Agones 关闭时完全惰性:原样返回首选节点,没有名额,不写任何映射。
func TestWorldChannel_InertWhenAgonesDisabled(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	node, placement, ok := ReserveAgonesRoomForWorldChannel(context.Background(), sc, 5004, "10", testZoneId)
	assert.True(t, ok)
	assert.Equal(t, "10", node)
	assert.Nil(t, placement)

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5004))
	assert.Equal(t, "", gs)
}

// ── 频道换节点时的名额:先占名额再搬 ────────────────────────────────────────
//
// 改派与新建频道一样先占名额再搬(docs/design/agones-scene-node-high-density.md §13.10):改派之前在新节点上占名额,
// 改派 CAS 落地之后提交(moveAgonesRoomForTest)。提交时归属已经是新节点,所以这些用例都先把 scene:{id}:node
// 摆成新节点。四条改派路径各自的占名额 / 回落 / 放弃见 world_channel_reserve_before_move_test.go。

// moveAgonesRoomForTest 走一遍改派路径的名额两步:改派之前在 newNode 上占名额(不回落,同 rebalance 换更好的家),
// 改派 CAS 落地之后提交。占不到时回 false,什么都没提交。
func moveAgonesRoomForTest(t *testing.T, sc *svc.ServiceContext, sceneID uint64, newNode string) bool {
	t.Helper()
	ctx := context.Background()
	node, placement, ok := reserveAgonesRoomForWorldChannel(ctx, sc, testZoneId, sceneID, newNode, false)
	if !ok {
		return false
	}
	require.Equal(t, newNode, node, "不回落时落点就是指定节点")
	commitReservedAgonesRoom(ctx, sc, testZoneId, sceneID, newNode, placement)
	return true
}

// 搬家:新节点占上、旧节点还回去、记录改到新节点的那一条。
func TestWorldChannel_TransferMovesRoomToNewNode(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(5005), "20"))
	seedAgonesRoom(t, sc, alloc, 5005, "gs-old")

	require.True(t, moveAgonesRoomForTest(t, sc, 5005, "20"))

	assert.EqualValues(t, 0, alloc.roomsOf("gs-old"), "旧节点的名额必须还回去,否则容量凭空蒸发")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-new"), "新节点必须记账,否则容量被超卖")
	requireAgonesRecordOn(t, sc, alloc, 5005, "gs-new")
	assert.Empty(t, agonesHoldsOf(t, sc, testZoneId), "CAS 在同一步消费了在途占用")
}

// 新节点满了 -> 占不到就不搬:旧名额与记录原封不动,也不留在途占用。
// 以前先改归属、事后才转名额,满了就把名额留在旧 GameServer 上:整个 Fleet 的总数对,却记错了台,GSA 按新的那台
// 「还有空位」继续往里放,那台进程超出压测定下的容量(§13.10)。
func TestWorldChannel_MoveRefusesAFullTargetWithoutFallback(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Allocated", count: 2, capacity: 2},
		&fakeGameServer{name: "gs-spare", podIP: "10.0.0.7", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "30", "10.0.0.7", testZoneId, constants.SceneNodeTypeMainWorld)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(5006), "10"))
	record := seedAgonesRoom(t, sc, alloc, 5006, "gs-old")

	require.False(t, moveAgonesRoomForTest(t, sc, 5006, "20"), "目标满了就不搬")

	assert.EqualValues(t, 1, alloc.roomsOf("gs-old"), "没搬就不能释放旧的")
	assert.EqualValues(t, 2, alloc.roomsOf("gs-new"), "满了的节点不能被超卖")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-spare"), "不回落时不交给 GSA 另挑")
	allocations, _, _ := alloc.stats()
	assert.Zero(t, allocations)
	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5006))
	assert.Equal(t, record, gs, "记录必须仍指向真正持有名额的那一条")
	assert.Empty(t, agonesHoldsOf(t, sc, testZoneId), "没占到名额就不留在途占用")
}

// Agones 关闭时占名额与提交都是 no-op。
func TestWorldChannel_TransferInertWhenAgonesDisabled(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(5007), "20"))
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(5007), "gs-old"))

	require.True(t, moveAgonesRoomForTest(t, sc, 5007, "20"))

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(5007))
	assert.Equal(t, "gs-old", gs)
}

// 占名额之后、提交之前,场景被销毁(销毁方已按旧记录删过条目)或被另一个写者再次搬走:属主已不是
// 新节点。只删这次占的那一条,旧记录一个字节都不动。
// 修复前预读旧记录后无条件覆盖:销毁后重建出一把孤儿 agones_gs、新名额永久泄漏,旧名额被还第二次。
func TestWorldChannel_TransferBacksOutWhenOwnerMovedOn(t *testing.T) {
	cases := []struct {
		name    string
		sceneID uint64
		owner   string // 空串 = 场景已被销毁
	}{
		{name: "moved_again", sceneID: 5008, owner: "30"},
		{name: "destroyed", sceneID: 5009, owner: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, _ := newTestSvcCtxWithWorldScenes(t)
			alloc := newFakeAllocator(
				&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 4},
				&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
			)
			withAgones(t, sc, alloc, true)
			bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
			bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
			record := ""
			if tc.owner != "" {
				require.NoError(t, sc.Redis.Set(sceneNodeKey(tc.sceneID), tc.owner))
				record = seedAgonesRoom(t, sc, alloc, tc.sceneID, "gs-old")
			}
			oldRooms := alloc.roomsOf("gs-old")

			require.True(t, moveAgonesRoomForTest(t, sc, tc.sceneID, "20"))

			assert.EqualValues(t, 0, alloc.roomsOf("gs-new"), "属主已不是新节点,这次占的那一条必须删掉")
			assert.EqualValues(t, oldRooms, alloc.roomsOf("gs-old"), "旧名额归属主或销毁方处理,这里不得再还")
			gs, err := sc.Redis.Get(sceneAgonesGsKey(tc.sceneID))
			require.NoError(t, err)
			assert.Equal(t, record, gs, "已销毁的场景不得重建出 agones_gs;被搬走的场景记录不动")
			assert.Empty(t, agonesHoldsOf(t, sc, testZoneId))
		})
	}
}

// 同一目标重复提交(两个写者先后把同一场景搬到同一节点,各占了一条):第二次读到记录已在新节点的
// GameServer 上,删掉自己这次占的那一条,净结果只记一次。
func TestWorldChannel_TransferIsIdempotentForSameTarget(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(5010), "20"))
	seedAgonesRoom(t, sc, alloc, 5010, "gs-old")

	require.True(t, moveAgonesRoomForTest(t, sc, 5010, "20"))
	require.True(t, moveAgonesRoomForTest(t, sc, 5010, "20"))

	assert.EqualValues(t, 1, alloc.roomsOf("gs-new"), "重复转移只记一次")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-old"), "旧名额只还一次")
	requireAgonesRecordOn(t, sc, alloc, 5010, "gs-new")
}

// 两路转移交错落在同一台 GameServer 上:改派成功的写者与「CAS 落空、发现属主恰是自己挑的节点」补转的写者,
// 都先占了名额、再先后提交。后提交的那一路读到记录已在这台上,只删自己占的那一条,先提交那一路的条目留着。
// 条目只用 scene_id 时(设计稿 §13 初稿)两路共用一个值:后一路「删掉自己刚加的」删掉的是记录正指向的那条,少记、会超卖。
func TestWorldChannel_InterleavedTransfersToSameGameServerKeepTheRecordedEntry(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	const sceneID uint64 = 5011
	require.NoError(t, sc.Redis.Set(sceneNodeKey(sceneID), "20"))
	seedAgonesRoom(t, sc, alloc, sceneID, "gs-old")
	ctx := context.Background()

	winner, err := acquireAgonesRoomOnGameServer(ctx, sc, testZoneId, sceneID, "20", "gs-new")
	require.NoError(t, err)
	adopter, err := acquireAgonesRoomOnGameServer(ctx, sc, testZoneId, sceneID, "20", "gs-new")
	require.NoError(t, err)
	require.NotEqual(t, winner.Entry, adopter.Entry, "每次占用一个条目")

	assert.Equal(t, agonesTransferMoved, commitAgonesRoomTransfer(ctx, sc, sceneID, "20", testZoneId, winner))
	assert.Equal(t, agonesTransferNotNeeded, commitAgonesRoomTransfer(ctx, sc, sceneID, "20", testZoneId, adopter))

	assert.Equal(t, []string{winner.Entry}, alloc.entriesOf("gs-new"), "只留先提交那一路的条目")
	assert.Equal(t, winner.Entry, requireAgonesRecordOn(t, sc, alloc, sceneID, "gs-new"))
	assert.EqualValues(t, 0, alloc.roomsOf("gs-old"))
	assert.Empty(t, agonesHoldsOf(t, sc, testZoneId))
}

// 排空一个世界频道时,名额要归还。
// (排空收尾按认领快照里的记录删条目,这里验证端到端不漏。)
func TestWorldChannel_DrainReleasesRoom(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-a", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)

	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 1500, 102: 40})
	seedAgonesRoom(t, sc, alloc, 102, "gs-a")

	ctx := context.Background()
	_, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	require.Equal(t, 1, in)

	// 人走干净后下一拍收尾。
	sc.Redis.Set(sceneNodeKey(102), "10")
	seedSceneMembersVia(sc, 102, 0)
	AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)

	assert.EqualValues(t, 1, alloc.roomsOf("gs-a"), "排空销毁的频道必须把名额还回去")
}
