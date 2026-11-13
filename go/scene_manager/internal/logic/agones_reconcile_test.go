package logic

// Agones rooms 名额对账(agones_reconcile.go,docs/design/agones-scene-node-high-density.md §13.5)的回归。
//
// 2026-11-10 之前 rooms 是计数,对账只比「rooms 计数 == 节点 scene_count」、只告警:下面每一种不一致在旧代码上都只会让
// drift 变成 1,多记的名额永远收不回来,少记的一直少记(节点被超卖)。现在名额是按身份的条目,对账逐条判、两个方向都改。
//
// 场景号 7001~7019。

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func agonesReconciledCount(t *testing.T, action string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_agones_rooms_reconciled_total",
		map[string]string{"zone": agonesZoneLabel(testZoneId), "action": action})
}

// holdAgonesRoomUntil 写一条截止时刻为 deadline 的在途占用。
func holdAgonesRoomUntil(t *testing.T, sc *svc.ServiceContext, entry string, deadline time.Time) {
	t.Helper()
	_, err := sc.Redis.Zadd(agonesRoomHoldsKey(testZoneId), deadline.Unix(), entry)
	require.NoError(t, err)
}

// reconcileOnce 跑一轮本 zone 的对账,回未收敛数。
func reconcileOnce(sc *svc.ServiceContext) int {
	return ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId)
}

// 占了名额之后进程崩溃,没有记录、也没有在途占用:孤儿,删掉。记录指向的那条不动。
func TestAgonesReconcile_RemovesOrphanEntryAndKeepsRecordedOne(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", capacity: 4})
	withAgones(t, sc, alloc, true)
	orphan := newAgonesRoomEntry(7001)
	alloc.addEntryForTest("gs-1", orphan)
	recorded := parseAgonesRoom(seedAgonesRoom(t, sc, alloc, 7002, "gs-1")).Entry
	before := agonesReconciledCount(t, metrics.AgonesRoomsOrphanRemoved)

	require.Equal(t, 0, ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId))
	assert.Equal(t, []string{recorded}, alloc.entriesOf("gs-1"))
	assert.Equal(t, before+1, agonesReconciledCount(t, metrics.AgonesRoomsOrphanRemoved))

	require.Equal(t, 0, ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId), "收敛之后再跑一轮什么都不改")
	assert.Equal(t, []string{recorded}, alloc.entriesOf("gs-1"))
}

// 在途占用没到截止时刻:提交可能还在路上,留着。过了截止时刻:同一步取消在途占用、删条目;之后晚到的登记被挡住,
// 一个键都不写 —— 不靠宽限期猜,所以不会出现「登记落地时条目已被删」的少记。
func TestAgonesReconcile_HeldEntryIsKeptUntilItsDeadlineThenFencedOff(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", capacity: 4})
	withAgones(t, sc, alloc, true)
	const sceneID uint64 = 7003
	entry := newAgonesRoomEntry(sceneID)
	alloc.addEntryForTest("gs-1", entry)
	holdAgonesRoomUntil(t, sc, entry, time.Now().Add(time.Minute))
	ctx := context.Background()

	require.Equal(t, 0, ReconcileAgonesRoomsForZone(ctx, sc, testZoneId))
	assert.Equal(t, []string{entry}, alloc.entriesOf("gs-1"), "在途的条目不收")
	assert.Equal(t, []string{entry}, agonesHoldsOf(t, sc, testZoneId))

	before := agonesReconciledCount(t, metrics.AgonesRoomsHoldExpiredRemoved)
	holdAgonesRoomUntil(t, sc, entry, time.Now().Add(-time.Second))
	require.Equal(t, 0, ReconcileAgonesRoomsForZone(ctx, sc, testZoneId))
	assert.Empty(t, alloc.entriesOf("gs-1"), "过了截止时刻的条目收回")
	assert.Empty(t, agonesHoldsOf(t, sc, testZoneId), "在途占用在同一步取消")
	assert.Equal(t, before+1, agonesReconciledCount(t, metrics.AgonesRoomsHoldExpiredRemoved))

	reg := sceneRegistration{SceneID: sceneID, ZoneID: testZoneId, NodeID: "10", AgonesRoom: agonesRoom{GameServer: "gs-1", Entry: entry}}
	require.Equal(t, registrationFenced, registerWorldChannel(sc, reg, 4242, 1), "取消之后晚到的登记被挡住")
	assert.False(t, mr.Exists(sceneNodeKey(testZoneId, sceneID)))
	assert.False(t, mr.Exists(sceneAgonesGsKey(testZoneId, sceneID)), "记录不会指向一条已经删掉的条目")
}

// 场景已销毁、拆除任务快照指向这一条:等拆除任务删(实体销毁之后才还名额);任务做完之后剩下的就是孤儿。
func TestAgonesReconcile_KeepsEntryOfAPendingTeardown(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", capacity: 4})
	withAgones(t, sc, alloc, true)
	const sceneID uint64 = 7004
	room := agonesRoom{GameServer: "gs-1", Entry: newAgonesRoomEntry(sceneID)}
	alloc.addEntryForTest("gs-1", room.Entry)
	reply, err := json.Marshal([]any{"claimed", "10", room.record(), "", ""})
	require.NoError(t, err)
	mr.HSet(sceneTeardownJobKey(testZoneId, sceneID), "token", "t-1", "kind", teardownKindInstance, "reason", "idle",
		"claimed_at", "1", "reply", string(reply))
	ctx := context.Background()

	require.Equal(t, 0, ReconcileAgonesRoomsForZone(ctx, sc, testZoneId))
	assert.Equal(t, []string{room.Entry}, alloc.entriesOf("gs-1"), "拆除任务还没做完,条目归它删")

	mr.Del(sceneTeardownJobKey(testZoneId, sceneID))
	require.Equal(t, 0, ReconcileAgonesRoomsForZone(ctx, sc, testZoneId))
	assert.Empty(t, alloc.entriesOf("gs-1"))
}

// 条目里的场景不在本 zone 的键里(登记在别的 zone,或已销毁):按孤儿删,只删 GameServer 上这一条,别的 zone 的场景键
// 一把都不读、不动(docs/design/redis-cluster-all-domains-20261110.md §3.3、§10.1)。原来靠 scene:<id>:zone 认出别的 zone、
// 留着不删(foreign),那把键已删。条目不是本服务写的格式(人工改过)的仍不碰,计入未收敛。
func TestAgonesReconcile_ReclaimsEntryOfSceneNotInZoneAndLeavesUnrecognized(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 4})
	withAgones(t, sc, alloc, true)
	const foreignScene uint64 = 7005
	foreign := newAgonesRoomEntry(foreignScene)
	alloc.addEntryForTest("gs-1", foreign)
	// 同一个场景号在别的 zone 有完整的记录,指着这一条:本 zone 的对账看不到它。
	otherZone := testZoneId + 1
	require.NoError(t, mr.Set(sceneNodeKey(otherZone, foreignScene), "10"))
	require.NoError(t, mr.Set(sceneAgonesGsKey(otherZone, foreignScene), agonesRoom{GameServer: "gs-1", Entry: foreign}.record()))
	require.Len(t, alloc.entriesOf("gs-1"), 2, "前提:一条认不出的占位,一条别的 zone 场景的条目")

	require.Equal(t, 1, ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId), "只剩认不出的那一条没收敛")
	after := alloc.entriesOf("gs-1")
	assert.NotContains(t, after, foreign, "场景不在本 zone 的条目按孤儿删")
	assert.Len(t, after, 1, "认不出的条目留着")
	assert.Equal(t, "10", mustGet(t, mr, sceneNodeKey(otherZone, foreignScene)), "别的 zone 的场景映射不动")
	assert.True(t, mr.Exists(sceneAgonesGsKey(otherZone, foreignScene)), "别的 zone 的名额记录不动")
}

// 过期的在途占用,条目不在任何一台 GameServer 上(分配根本没发出去):取消。没到期的留着。
func TestAgonesReconcile_CancelsExpiredHoldWithoutEntry(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Ready", capacity: 4})
	withAgones(t, sc, alloc, true)
	expired, live := newAgonesRoomEntry(7006), newAgonesRoomEntry(7007)
	holdAgonesRoomUntil(t, sc, expired, time.Now().Add(-time.Second))
	holdAgonesRoomUntil(t, sc, live, time.Now().Add(time.Minute))
	before := agonesReconciledCount(t, metrics.AgonesRoomsHoldCanceled)

	require.Equal(t, 0, ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId))
	assert.Equal(t, []string{live}, agonesHoldsOf(t, sc, testZoneId))
	assert.Equal(t, before+1, agonesReconciledCount(t, metrics.AgonesRoomsHoldCanceled))
}

// newRestoreCase 铺「记录 → 条目」方向的用例:节点 10 在 gs-1(Pod 10.0.0.8)上,节点 20 在 gs-2(Pod 10.0.0.9)上;
// 场景 sceneID 的属主是 10、在节点 10 的场景集合里。
func newRestoreCase(t *testing.T, sceneID uint64, servers ...*fakeGameServer) (*svc.ServiceContext, *fakeAllocator) {
	t.Helper()
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(servers...)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	require.NoError(t, mr.Set(sceneNodeKey(testZoneId, sceneID), "10"))
	_, err := mr.SAdd(nodeScenesKey(testZoneId, "10"), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	return sc, alloc
}

// 记录指向这台、条目却不在列表里(有人手工删过,或 Agones 丢了):补加。旧代码只告警,节点一直被超卖一份。
func TestAgonesReconcile_AddsBackMissingRecordedEntry(t *testing.T) {
	const sceneID uint64 = 7008
	sc, alloc := newRestoreCase(t, sceneID,
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", capacity: 4})
	room := agonesRoom{GameServer: "gs-1", Entry: newAgonesRoomEntry(sceneID)}
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(testZoneId, sceneID), room.record()))
	before := agonesReconciledCount(t, metrics.AgonesRoomsMissingAdded)

	require.Equal(t, 0, reconcileOnce(sc))
	assert.Equal(t, []string{room.Entry}, alloc.entriesOf("gs-1"))
	assert.Equal(t, before+1, agonesReconciledCount(t, metrics.AgonesRoomsMissingAdded))
}

// 同上,但这台满了:补不上,不挤掉别人,计入未收敛并记 missing_no_capacity。
func TestAgonesReconcile_MissingEntryOnFullGameServerIsReported(t *testing.T) {
	const sceneID uint64 = 7009
	sc, alloc := newRestoreCase(t, sceneID,
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 2})
	room := agonesRoom{GameServer: "gs-1", Entry: newAgonesRoomEntry(sceneID)}
	require.NoError(t, sc.Redis.Set(sceneAgonesGsKey(testZoneId, sceneID), room.record()))
	before := agonesReconciledCount(t, metrics.AgonesRoomsMissingNoCapacity)

	// 两条占位条目认不出、各计一次;缺的那一条补不上再计一次。
	require.Equal(t, 3, reconcileOnce(sc))
	assert.NotContains(t, alloc.entriesOf("gs-1"), room.Entry)
	assert.EqualValues(t, 2, alloc.roomsOf("gs-1"), "满了不挤掉别人")
	assert.Equal(t, before+1, agonesReconciledCount(t, metrics.AgonesRoomsMissingNoCapacity))
}

// 场景在节点 10 上,名额却记在别的 GameServer 上(改派转移时 gs-1 满了,旧的没敢动):gs-1 有空位之后对账转过来,
// 旧条目删掉。旧代码:旧 GameServer 一直多记、新 GameServer 一直少记,直到场景销毁。
func TestAgonesReconcile_MovesRoomRecordedOnAnotherGameServer(t *testing.T) {
	const sceneID uint64 = 7010
	sc, alloc := newRestoreCase(t, sceneID,
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Ready", capacity: 4},
		&fakeGameServer{name: "gs-2", podIP: "10.0.0.9", state: "Allocated", capacity: 4})
	seedAgonesRoom(t, sc, alloc, sceneID, "gs-2")
	before := agonesReconciledCount(t, metrics.AgonesRoomsRehomed)

	require.Equal(t, 0, reconcileOnce(sc))
	requireAgonesRecordOn(t, sc, alloc, sceneID, "gs-1")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-1"))
	assert.EqualValues(t, 0, alloc.roomsOf("gs-2"), "旧条目删掉")
	assert.Empty(t, agonesHoldsOf(t, sc, testZoneId))
	assert.Equal(t, before+1, agonesReconciledCount(t, metrics.AgonesRoomsRehomed))
}

// 同上,但 gs-1 仍然满:不动旧的(不能让场景一份名额都不占),计入未收敛,下一轮再试。
func TestAgonesReconcile_RehomeOnFullGameServerKeepsTheOldRoom(t *testing.T) {
	const sceneID uint64 = 7011
	sc, alloc := newRestoreCase(t, sceneID,
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", capacity: 1},
		&fakeGameServer{name: "gs-2", podIP: "10.0.0.9", state: "Allocated", capacity: 4})
	recordedElsewhere := seedAgonesRoom(t, sc, alloc, sceneID, "gs-2")
	occupant := seedAgonesRoom(t, sc, alloc, 7012, "gs-1") // gs-1 被另一个场景占满
	before := agonesReconciledCount(t, metrics.AgonesRoomsRehomeNoCapacity)

	require.Equal(t, 1, reconcileOnce(sc))
	record, err := sc.Redis.Get(sceneAgonesGsKey(testZoneId, sceneID))
	require.NoError(t, err)
	assert.Equal(t, recordedElsewhere, record)
	assert.Equal(t, []string{parseAgonesRoom(occupant).Entry}, alloc.entriesOf("gs-1"))
	assert.EqualValues(t, 1, alloc.roomsOf("gs-2"), "旧名额留着")
	assert.Empty(t, agonesHoldsOf(t, sc, testZoneId), "没占到名额不留在途占用")
	assert.Equal(t, before+1, agonesReconciledCount(t, metrics.AgonesRoomsRehomeNoCapacity))
}

// 列不出 GameServer 时整轮跳过,一个键都不改。
func TestAgonesReconcile_SkipsTheRoundWhenListingFails(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	alloc := newFakeAllocator(&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", capacity: 4})
	withAgones(t, sc, alloc, true)
	orphan := newAgonesRoomEntry(7013)
	alloc.addEntryForTest("gs-1", orphan)
	holdAgonesRoomUntil(t, sc, newAgonesRoomEntry(7014), time.Now().Add(-time.Second))
	alloc.listErr = assert.AnError

	require.Equal(t, 0, ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId))
	assert.Equal(t, []string{orphan}, alloc.entriesOf("gs-1"))
	assert.Len(t, agonesHoldsOf(t, sc, testZoneId), 1, "过期的在途占用也不动")
}

// 条目格式:scene_id 在前,对账才能从条目找到记录;每次占用各不相同。
func TestAgonesRoomEntry_CarriesTheSceneIDAndIsUniquePerReservation(t *testing.T) {
	first, second := newAgonesRoomEntry(7015), newAgonesRoomEntry(7015)
	require.NotEqual(t, first, second)
	for _, entry := range []string{first, second} {
		sceneID, ok := agonesRoomEntrySceneID(entry)
		require.True(t, ok, entry)
		require.EqualValues(t, 7015, sceneID)
	}
	for _, bad := range []string{"", "7015", "7015.", ".abc", "x.abc", "0.abc", "seed-gs-1-0"} {
		_, ok := agonesRoomEntrySceneID(bad)
		require.False(t, ok, bad)
	}

	room := parseAgonesRoom("gs-a/" + first)
	require.Equal(t, agonesRoom{GameServer: "gs-a", Entry: first}, room)
	require.Equal(t, "gs-a/"+first, room.record())
	require.Equal(t, agonesRoom{GameServer: "gs-a"}, parseAgonesRoom("gs-a"), "非高密度模式只记 GameServer 名")
	require.Equal(t, "gs-a", agonesRoom{GameServer: "gs-a"}.record())
	require.Equal(t, agonesRoom{}, parseAgonesRoom(""))
}
