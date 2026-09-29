package logic

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// ---------------------------------------------------------------------------
// 合服围栏(player-storage-placement.md §8.2 / §12 A16)
//
// data_service 在归属 zone 处于合服围栏内时回 home_zone_merging=true,EnterScene 必须在对
// location / owner_epoch 的**任何写之前**回 ErrHomeZoneMerging(可重试)。用例断言的都是
// 「一个字节都不改」:两把键的原值、人数、路由消息、去重占位;以及同样的输入在 merging=false 时
// 行为与加字段之前完全相同。假实现与种数据的工具沿用 owner_epoch_test.go。
// ---------------------------------------------------------------------------

const rejectReasonMergingForTest = "home_zone_merging"

// 同区落点(首次落点 / 同落点重连):merging 时拒绝且不写 location / owner_epoch;
// 不 merging 时照常落点,归属随路由事件下发。
func TestEnterScene_HomeZoneMergingGatesSameZoneLanding(t *testing.T) {
	const (
		playerID = uint64(6301)
		targetID = uint64(7301)
	)
	cases := []struct {
		name      string
		merging   bool
		reconnect bool // true:玩家已在 targetID / 节点 10 上,本次是同落点重连
	}{
		{name: "first_landing_merging", merging: true},
		{name: "first_landing_not_merging", merging: false},
		{name: "reconnect_merging", merging: true, reconnect: true},
		{name: "reconnect_not_merging", merging: false, reconnect: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			captured := capturingKafkaWriter(sc)
			fake := &fakeHomeZoneClient{zone: testZoneId, merging: tc.merging}
			sc.HomeZone = fake

			seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
			mr.Set(nodePlayerCountKey(testZoneId, "10"), "4")
			if tc.reconnect {
				require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, targetID, "10", testZoneId))
			}
			locBefore, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
			epochBefore := ownerEpochRaw(t, sc, playerID)
			countBefore, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
			rejectedBefore := enterSceneRejectedCount(t, testZoneId, rejectReasonMergingForTest)

			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
				PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
				GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "merge-fence",
			})
			require.NoError(t, err)
			assert.Equal(t, 1, fake.calls)

			if tc.merging {
				assert.Equal(t, constants.ErrHomeZoneMerging, resp.ErrorCode, "合服窗口内源区归属玩家不得进场")
				assert.Equal(t, rejectedBefore+1, enterSceneRejectedCount(t, testZoneId, rejectReasonMergingForTest))
				assert.Empty(t, *captured, "拒绝时不发路由:节点收不到就不会往源 topic 存盘")
				locAfter, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
				assert.Equal(t, locBefore, locAfter, "拒绝前不得写 location")
				assert.Equal(t, epochBefore, ownerEpochRaw(t, sc, playerID), "拒绝前不得写 owner_epoch")
				countAfter, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
				nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
				assert.Equal(t, countBefore, countAfter)
				assert.Equal(t, "4", nodeCount)
				exists, existsErr := sc.Redis.Exists(fmt.Sprintf("enter_scene:dedup:%d:merge-fence", playerID))
				require.NoError(t, existsErr)
				assert.False(t, exists, "可重试拒绝必须释放 request_id 占位")
				return
			}

			assert.Equal(t, uint32(0), resp.ErrorCode, "不在合服窗口:行为与加字段之前相同")
			assert.Equal(t, rejectedBefore, enterSceneRejectedCount(t, testZoneId, rejectReasonMergingForTest))
			loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
			require.NotNil(t, loc)
			assert.Equal(t, targetID, loc.SceneId)
			assert.Equal(t, "10", loc.NodeId)
			require.Len(t, *captured, 1)
			assert.Equal(t, testZoneId, decodeRoutePlayerEvent(t, (*captured)[0]).HomeZoneId)
		})
	}
}

// 排序回归:同节点换图遇到 owner_epoch 键单独丢失(location 记着 5)时,EnterScene 会先按 location
// 把键补种回 5 —— 那是同区路径上对 owner_epoch 的第一次写。归属查询(含合服围栏判定)必须排在它
// 前面:merging 时键必须仍然不存在。把 3b 放回补种之后,这条用例会看到键被写成 "5"。
// merging=false 的对照组证明真实查询分支下补种行为不变。
func TestEnterScene_HomeZoneMergingRejectsBeforeSameNodeEpochReseed(t *testing.T) {
	const (
		playerID = uint64(6302)
		oldScene = uint64(7302)
		targetID = uint64(7402)
	)
	for _, merging := range []bool{true, false} {
		t.Run(fmt.Sprintf("merging=%v", merging), func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			captured := capturingKafkaWriter(sc)
			fake := &fakeHomeZoneClient{zone: testZoneId, merging: merging}
			sc.HomeZone = fake

			seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
			seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
			raw, err := gproto.Marshal(&scene_manager.PlayerLocation{
				SceneId: oldScene, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 5, UpdateTime: uint64(time.Now().Unix()),
			})
			require.NoError(t, err)
			require.NoError(t, sc.Redis.Set(getPlayerLocationKey(playerID), string(raw)))
			require.False(t, mr.Exists(ownerepoch.OwnerEpochKey(playerID)))
			withReachableSceneNode(t, sc, "10")

			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
				PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
				GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
			})
			require.NoError(t, err)
			assert.Equal(t, 1, fake.calls)

			if merging {
				assert.Equal(t, constants.ErrHomeZoneMerging, resp.ErrorCode)
				assert.False(t, mr.Exists(ownerepoch.OwnerEpochKey(playerID)), "合服拒绝必须先于 owner_epoch 补种")
				after, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
				assert.Equal(t, string(raw), after, "拒绝前不得写 location")
				assert.Empty(t, *captured)
				oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
				targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
				assert.Equal(t, "3", oldCount)
				assert.Equal(t, "4", targetCount)
				return
			}

			assert.Equal(t, uint32(0), resp.ErrorCode)
			assert.Equal(t, "5", ownerEpochRaw(t, sc, playerID), "不在合服窗口:键照常按 location 补种回 5")
			loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
			require.NotNil(t, loc)
			assert.Equal(t, targetID, loc.SceneId)
			assert.Equal(t, uint64(5), loc.OwnerEpoch)
			require.Len(t, *captured, 1)
			event := decodeRoutePlayerEvent(t, (*captured)[0])
			assert.Equal(t, uint64(5), event.OwnerEpoch)
			assert.Equal(t, testZoneId, event.HomeZoneId)
		})
	}
}

// 跨 zone 传送第一条腿:标记已就绪、目标地图开着、gate 签得出票据,能挡住放行的只有合服围栏。
// 拒绝发生在换手门、铸造与等待落点之前,源 scene 仍持有玩家并按失败应答解冻 —— epoch、location、
// 标记、旧场景人数原样,不签票据、不发重定向。
func TestEnterScene_HomeZoneMergingRejectsTravelFirstLegWithoutSideEffects(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	fake := &fakeHomeZoneClient{zone: 1, merging: true}
	sc.HomeZone = fake

	const (
		playerID = uint64(6303)
		oldScene = uint64(7303)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)
	locBefore, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	epochBefore := ownerEpochRaw(t, sc, playerID)
	markerBefore, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
	rejectedBefore := enterSceneRejectedCount(t, 1, rejectReasonMergingForTest)

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		t.Error("合服围栏内不该去签目标 zone 的票据")
		return nil, errors.New("unexpected redirect")
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneMerging, resp.ErrorCode)
	assert.Nil(t, resp.Redirect)
	assert.Equal(t, 1, fake.calls)
	assert.Equal(t, rejectedBefore+1, enterSceneRejectedCount(t, 1, rejectReasonMergingForTest), "zone_id 记 gate zone")
	assert.Equal(t, epochBefore, ownerEpochRaw(t, sc, playerID), "被拒的传送不得铸造 epoch")
	locAfter, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, locBefore, locAfter, "被拒的传送不得留下等待落点")
	markerAfter, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
	assert.Equal(t, markerBefore, markerAfter)
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "3", oldCount)
	assert.Empty(t, *captured, "被拒的传送不得发出 RedirectToGateEvent")
}

// 跨 zone 传送第二条腿(消费等待落点):第一条腿放行之后合服围栏才立起来。落点照样拒,等待落点、
// epoch、目标场景人数原样;围栏撤掉后重试即可落地。
func TestEnterScene_HomeZoneMergingRejectsTravelSecondLeg(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	fake := &fakeHomeZoneClient{zone: 1, merging: true}
	sc.HomeZone = fake

	const (
		playerID = uint64(6304)
		targetID = uint64(7304)
	)
	seedSceneOnNode(mr, 2, targetID, "10", "4")
	mr.Set(nodePlayerCountKey(2, "10"), "4")
	placed, err := placePlayerLocation(sc, playerID, 0, "", 2, placementGuard{observedEpoch: 0, mint: true})
	require.NoError(t, err)
	require.Equal(t, uint64(1), placed.epoch)
	awaitingRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: 2,
		GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneMerging, resp.ErrorCode)
	assert.Equal(t, 1, fake.calls)
	assert.Empty(t, *captured)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "拒绝前不得铸造 epoch")
	raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, awaitingRaw, raw, "等待落点原样留着")
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(2, "10"))
	assert.Equal(t, "4", count)
	assert.Equal(t, "4", nodeCount)
}

// 映射缺失时 merging 必为 false:没有归属 zone 就谈不上围栏。即使服务端(旧版以 HomeZoneId=0 表示
// 缺席)同时带了 merging=true,也按未映射策略处理 —— 首次落点回落 gate zone 并照常落点。
func TestEnterScene_HomeZoneMergingIgnoredWhenUnmapped(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	fake := &fakeHomeZoneClient{zone: 0, merging: true}
	sc.HomeZone = fake

	const (
		playerID = uint64(6305)
		targetID = uint64(7305)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "0")
	rejectedBefore := enterSceneRejectedCount(t, testZoneId, rejectReasonMergingForTest)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, rejectedBefore, enterSceneRejectedCount(t, testZoneId, rejectReasonMergingForTest))
	require.Len(t, *captured, 1)
	assert.Equal(t, testZoneId, decodeRoutePlayerEvent(t, (*captured)[0]).HomeZoneId, "未映射的首次落点按 gate zone")
}
