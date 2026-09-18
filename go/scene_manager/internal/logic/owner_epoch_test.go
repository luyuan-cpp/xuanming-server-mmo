package logic

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	gproto "google.golang.org/protobuf/proto"

	kafkacontracts "proto/contracts/kafka"
	dspb "proto/data_service"
	"proto/scene_manager"
	game "scene_manager/generated/pb/game"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// ---------------------------------------------------------------------------
// owner_epoch 铸造 / handoff 换手门 / home_zone 查询(cross-zone-scene-travel.md
// CZ-3 / CZ-4 / CZ-5,scene-owner-reentry-barrier.md §3.3)
//
// 这些用例只走 EnterScene 这一个公开入口,断言的是 Redis 里两把键的终态与
// RoutePlayerEvent 里带出去的值 —— 那是 C++ 节点与 db 消费者真正依赖的契约。
// ---------------------------------------------------------------------------

// fakeHomeZoneClient 是 data_service GetPlayerHomeZone 的最小假实现。
type fakeHomeZoneClient struct {
	zone  uint32
	err   error
	calls int
}

func (f *fakeHomeZoneClient) GetPlayerHomeZone(_ context.Context, _ *dspb.GetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.GetPlayerHomeZoneResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &dspb.GetPlayerHomeZoneResponse{HomeZoneId: f.zone}, nil
}

// capturingKafkaWriter 把发出的消息原样留下,供解码 RoutePlayerEvent。
func capturingKafkaWriter(sc *svc.ServiceContext) *[]kafka.Message {
	captured := &[]kafka.Message{}
	sc.Kafka = &countingKafkaWriter{onWrite: func(msgs []kafka.Message) {
		*captured = append(*captured, msgs...)
	}}
	sc.Config.KafkaWriteTimeoutSeconds = 1
	return captured
}

func decodeRoutePlayerEvent(t *testing.T, msg kafka.Message) *kafkacontracts.RoutePlayerEvent {
	t.Helper()
	cmd := &kafkacontracts.GateCommand{}
	require.NoError(t, gproto.Unmarshal(msg.Value, cmd))
	require.Equal(t, uint32(game.ContractsKafkaRoutePlayerEventEventId), cmd.EventId)
	event := &kafkacontracts.RoutePlayerEvent{}
	require.NoError(t, gproto.Unmarshal(cmd.Payload, event))
	return event
}

func ownerEpochRaw(t *testing.T, sc *svc.ServiceContext, playerID uint64) string {
	t.Helper()
	raw, err := sc.Redis.Get(ownerepoch.OwnerEpochKey(playerID))
	require.NoError(t, err)
	return raw
}

// seedSceneOnNode 摆一个已存在的场景实例:scene→node 映射、zone 映射、人数。
func seedSceneOnNode(mr *miniredis.Miniredis, zoneID uint32, sceneID uint64, nodeID string, playerCount string) {
	mr.ZAdd(nodeLoadKey(zoneID), 0, nodeID)
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneID), nodeID)
	mr.Set(fmt.Sprintf(SceneZoneKeyFmt, sceneID), fmt.Sprintf("%d", zoneID))
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, sceneID), playerCount)
}

// writeHandoffMarker 模拟 C++ 源 scene 在 Redis 落地回调之后写出的交接标记。
func writeHandoffMarker(t *testing.T, sc *svc.ServiceContext, playerID uint64, epoch uint64) {
	t.Helper()
	marker := ownerepoch.Handoff{Epoch: epoch, SavedAtMs: 1757000000000}
	require.NoError(t, sc.Redis.Setex(ownerepoch.HandoffKey(playerID), marker.String(), int(ownerepoch.HandoffTTL.Seconds())))
}

// --- 铸造 ------------------------------------------------------------------

func TestEnterScene_FirstLandingMintsEpochOneAndCarriesItInRouteEvent(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6101)
		targetID = uint64(7101)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "0")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)

	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "首次分配从 1 开始铸造")
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, uint64(1), loc.OwnerEpoch, "location 里必须带同一个 epoch")
	assert.Equal(t, targetID, loc.SceneId)

	require.Len(t, *captured, 1)
	event := decodeRoutePlayerEvent(t, (*captured)[0])
	assert.Equal(t, uint64(1), event.OwnerEpoch, "路由事件必须带本次铸造的 epoch")
	// 本进程没配 DataServiceRpc:归属按 gate zone 填,不能是 0。
	assert.Equal(t, testZoneId, event.HomeZoneId)
}

func TestEnterScene_SamePlacementReconnectDoesNotMintEpoch(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6102)
		sceneID  = uint64(7102)
	)
	seedSceneOnNode(mr, testZoneId, sceneID, "10", "1")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, sceneID, "10", testZoneId))
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	beforeRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)

	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "同一落点重连不得推进 epoch:持有节点的缓存值必须仍然合法")
	afterRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, beforeRaw, afterRaw, "重连不改写 location")
	require.Len(t, *captured, 1)
	event := decodeRoutePlayerEvent(t, (*captured)[0])
	assert.Equal(t, uint64(1), event.OwnerEpoch, "重连事件原样带当前 epoch")
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneID))
	assert.Equal(t, "1", count, "重连不重复计人数")
}

func TestMintEpochLuaRejectsStaleExpectedValueWithoutTouchingKeys(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6103)
	require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), "3"))
	require.NoError(t, sc.Redis.Set(getPlayerLocationKey(playerID), "old-location-bytes"))

	// 调用方读到的是 2(并发请求已经推到 3):CAS 必须落败且两把键一个字节不动。
	result, err := sc.Redis.Eval(luaMintEpochAndSetLocation,
		[]string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)},
		"2", "new-location-bytes", "")
	require.NoError(t, err)
	assert.Equal(t, "0", fmt.Sprint(result))
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))
	locRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, "old-location-bytes", locRaw)

	// 期望值对上:INCR 到 4 并写 location,一次原子完成。
	result, err = sc.Redis.Eval(luaMintEpochAndSetLocation,
		[]string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)},
		"3", "new-location-bytes", "")
	require.NoError(t, err)
	assert.Equal(t, "4", fmt.Sprint(result))
	assert.Equal(t, "4", ownerEpochRaw(t, sc, playerID))
	locRaw, _ = sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, "new-location-bytes", locRaw)
}

func TestMintEpochLuaTreatsMissingKeyAsZero(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6104)

	result, err := sc.Redis.Eval(luaMintEpochAndSetLocation,
		[]string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)},
		"0", "loc", "")
	require.NoError(t, err)
	assert.Equal(t, "1", fmt.Sprint(result), "键不存在与 \"0\" 同义:首次铸造得到 1")

	// 回滚到 "0" 之后再铸造同样得到 1,两条路径汇合。
	require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), "0"))
	result, err = sc.Redis.Eval(luaMintEpochAndSetLocation,
		[]string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)},
		"0", "loc", "")
	require.NoError(t, err)
	assert.Equal(t, "1", fmt.Sprint(result))
}

func TestRollbackEpochFor(t *testing.T) {
	minted := func(epoch uint64) placedLocation { return placedLocation{epoch: epoch, minted: true} }
	assert.Equal(t, uint64(5), rollbackEpochFor(&scene_manager.PlayerLocation{OwnerEpoch: 5}, minted(6)), "优先退回旧 location 记录的 epoch")
	assert.Equal(t, uint64(5), rollbackEpochFor(&scene_manager.PlayerLocation{OwnerEpoch: 0}, minted(6)), "旧 location 无 epoch 时退回 minted-1")
	assert.Equal(t, uint64(0), rollbackEpochFor(nil, minted(1)), "首次落点回滚退到 0")
	assert.Equal(t, uint64(0), rollbackEpochFor(nil, minted(0)))
	// 没铸造的落点(同节点换图 / dev 旁路):回滚只动 location,epoch 原样。
	assert.Equal(t, uint64(7), rollbackEpochFor(&scene_manager.PlayerLocation{OwnerEpoch: 3}, placedLocation{epoch: 7}))
}

// --- 换手门:标记 ------------------------------------------------------------

func TestEnterScene_CrossNodeWithStaleMarkerIsRejectedAsHandoffPending(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6105)
		oldScene = uint64(7105)
		targetID = uint64(7205)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	mr.Set(nodePlayerCountKey(testZoneId, "20"), "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	// 标记来自更早一代(epoch 0):不能证明当前持有者已落盘。
	writeHandoffMarker(t, sc, playerID, 0)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode)
	assert.Empty(t, *captured, "拒绝时不得发路由")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "拒绝时不得铸造 epoch")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, oldScene, loc.SceneId)
	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "20"))
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "4", targetCount)
	assert.Equal(t, "4", nodeCount)
	assert.Equal(t, "3", oldCount)
}

func TestEnterScene_CrossNodeWithCurrentMarkerIsAllowedAndMintsNextEpoch(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6106)
		oldScene = uint64(7106)
		targetID = uint64(7206)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	mr.Set(nodePlayerCountKey(testZoneId, "20"), "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	// 源 scene 已为当前 epoch(1)落盘并写标记。
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode, "标记齐 → 放行")

	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "换手必须推进 epoch,旧节点的迟到写才会被拒")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, targetID, loc.SceneId)
	assert.Equal(t, "20", loc.NodeId)
	assert.Equal(t, uint64(2), loc.OwnerEpoch)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(2), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "5", targetCount)
	assert.Equal(t, "2", oldCount)
	// 标记由 scene_manager 只读不删,靠 TTL 过期。
	markerRaw, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
	assert.NotEmpty(t, markerRaw)
}

func TestEnterScene_CrossZoneWithCurrentMarkerReleasesToAwaitingPlacement(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6107)
		oldScene = uint64(7107)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	writeHandoffMarker(t, sc, playerID, 1)

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(_ context.Context, _ *svc.ServiceContext, zone uint32, pid uint64) (*scene_manager.RedirectToGateInfo, error) {
		assert.Equal(t, uint32(2), zone)
		assert.Equal(t, playerID, pid)
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: 0, SceneConfId: 0, ZoneId: 2,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	require.NotNil(t, resp.Redirect)

	// 第一条腿:location 更新为等待落点(不是删除),epoch 推进,旧场景人数释放。
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, uint32(2), loc.ZoneId)
	assert.Equal(t, "", loc.NodeId)
	assert.Equal(t, uint64(0), loc.SceneId)
	assert.Equal(t, uint64(2), loc.OwnerEpoch)
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "2", oldCount, "源 scene 收到应答后销毁实体,旧场景人数在放行时释放")
	require.Len(t, *captured, 1, "RedirectToGateEvent 推给当前 Gate")
}

func TestEnterScene_CrossZoneRedirectKafkaFailureRestoresLocationAndEpoch(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const (
		playerID = uint64(6108)
		oldScene = uint64(7108)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	writeHandoffMarker(t, sc, playerID, 1)
	oldRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)

	// 源 scene 会解冻继续持有玩家,它缓存的 epoch(1)必须仍然是 Redis 当前值。
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	restoredRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, oldRaw, restoredRaw, "location 退回精确旧值")
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "3", oldCount, "旧场景人数退回")
}

// --- 换手门:等待落点 --------------------------------------------------------

func TestEnterScene_AwaitingPlacementSecondLegIsAllowedWithoutMarker(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6109)
		targetID = uint64(7109)
	)
	seedSceneOnNode(mr, 2, targetID, "10", "0")
	// 第一条腿留下的等待落点位置:zone 2、无节点、epoch 1。没有 handoff 标记。
	placed, err := placePlayerLocation(sc, playerID, 0, "", 2, placementGuard{observedEpoch: 0, mint: true})
	require.NoError(t, err)
	require.Equal(t, uint64(1), placed.epoch)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: 2,
		GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode, "没有节点持有玩家,第二条腿不需要标记")

	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "落点照常铸造下一代")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, targetID, loc.SceneId)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, uint32(2), loc.ZoneId)
	assert.Equal(t, uint64(2), loc.OwnerEpoch)
	require.Len(t, *captured, 1)
	event := decodeRoutePlayerEvent(t, (*captured)[0])
	assert.Equal(t, uint64(2), event.OwnerEpoch)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	assert.Equal(t, "1", count)
}

func TestEnterScene_AwaitingPlacementCrossZoneAgainIsAllowedWithoutMarker(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)

	const playerID = uint64(6110)
	// zone 2 有活节点:等待落点的位置不会被当成「已下线 zone 的陈旧位置」过滤掉,
	// 走的必须是 awaitingPlacement 放行,而不是陈旧位置放行。
	mr.ZAdd(nodeLoadKey(2), 0, "10")
	placed, err := placePlayerLocation(sc, playerID, 0, "", 2, placementGuard{observedEpoch: 0, mint: true})
	require.NoError(t, err)
	require.Equal(t, uint64(1), placed.epoch)

	// 玩家没跟着票据去 zone 2,而是从 zone 1 的 Gate 再次登录并要去 zone 3。
	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(_ context.Context, _ *svc.ServiceContext, zone uint32, _ uint64) (*scene_manager.RedirectToGateInfo, error) {
		assert.Equal(t, uint32(3), zone)
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.3.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 3,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, uint32(3), loc.ZoneId)
	assert.Equal(t, "", loc.NodeId)
	assert.Equal(t, uint64(2), loc.OwnerEpoch)
}

// --- 路由失败回滚 -------------------------------------------------------------

func TestEnterScene_RouteFailureAfterHandoffRestoresEpochForOldOwner(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const (
		playerID = uint64(6111)
		oldScene = uint64(7111)
		targetID = uint64(7211)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "3")
	mr.Set(nodePlayerCountKey(testZoneId, "20"), "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	writeHandoffMarker(t, sc, playerID, 1)
	oldRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	withReachableSceneNode(t, sc, "10", "20")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)

	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "路由没发出去,epoch 必须退回旧持有者手里的值")
	restoredRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, oldRaw, restoredRaw)
	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	targetNode, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "20"))
	oldNode, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "4", targetCount)
	assert.Equal(t, "3", oldCount)
	assert.Equal(t, "4", targetNode)
	assert.Equal(t, "3", oldNode)
}

func TestRollbackPlayerPlacementSkipsWhenEpochAdvancedConcurrently(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6112)
	placed, err := placePlayerLocation(sc, playerID, 7112, "10", testZoneId, placementGuard{observedEpoch: 0, mint: true})
	require.NoError(t, err)
	require.Equal(t, uint64(1), placed.epoch)
	// 并发请求已把归属推到 2(location 也换了)。
	later, err := placePlayerLocation(sc, playerID, 7212, "20", testZoneId, placementGuard{observedEpoch: 1, mint: true})
	require.NoError(t, err)
	require.Equal(t, uint64(2), later.epoch)

	restored := rollbackPlayerPlacement(sc, NewEnterSceneLogic(context.Background(), sc).Logger, playerID, placed, "", 0)
	assert.False(t, restored, "本次写入已被覆盖,回滚必须跳过")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
	raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, later.raw, raw)
}

// --- dev 旁路 -----------------------------------------------------------------

// dev 旁路下无标记的跨节点交接**不铸造**:旧节点 ReleasePlayer 的释放存盘必然晚于
// 这里的落点,铸了它就会被 C++ 的 CAS 拒掉(每次跨节点换图确定性回档)。
func TestEnterScene_DevBypassWithoutMarkerDoesNotMintEpoch(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.AllowUnsafeCrossNodeHandoff = true
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6113)
		oldScene = uint64(7113)
		targetID = uint64(7213)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	// 没有任何 handoff 标记。
	withReachableSceneNode(t, sc, "10", "20")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode, "旁路放宽标记要求")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "无标记的旁路放行不铸造,旧节点手里的 1 仍然合法")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, "20", loc.NodeId)
	assert.Equal(t, uint64(1), loc.OwnerEpoch)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(1), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
}

// 旁路开着、但源确实写了当前代际的标记:照样走安全路径(铸造)。
func TestEnterScene_DevBypassWithCurrentMarkerStillMints(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.AllowUnsafeCrossNodeHandoff = true
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6114)
		oldScene = uint64(7114)
		targetID = uint64(7214)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(2), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
}

// --- 持有者没换就不铸造 ---------------------------------------------------------

// 同节点换图:持有者没换。铸了,持有节点要等 Kafka → gate → scene 才知道新值,窗口内
// 它的周期 / 退出存盘被 C++ CAS 拒掉,合法持有者被当成废黜销毁(踢人 + 回档)。
func TestEnterScene_SameNodeSceneSwitchDoesNotMintEpoch(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6115)
		oldScene = uint64(7115)
		targetID = uint64(7215)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode, "同节点换图不需要标记")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "持有者没换:旧 epoch 的存盘必须继续合法")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, targetID, loc.SceneId)
	assert.Equal(t, uint64(1), loc.OwnerEpoch)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(1), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
}

// 不铸造的落点同样受 CAS 约束:观察之后 epoch 被并发推进,location 一个字节都不改。
func TestSetLocationIfEpochRejectsStaleObservation(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6116)
	first, err := placePlayerLocation(sc, playerID, 7116, "10", testZoneId, placementGuard{observedEpoch: 0, mint: true})
	require.NoError(t, err)

	_, err = placePlayerLocation(sc, playerID, 7216, "10", testZoneId, placementGuard{observedEpoch: 0, mint: false})
	require.ErrorIs(t, err, errOwnerEpochConflict, "观察值 0 已过期(当前是 1)")
	raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, first.raw, raw)

	moved, err := placePlayerLocation(sc, playerID, 7216, "10", testZoneId, placementGuard{observedEpoch: 1, mint: false})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), moved.epoch)
	assert.False(t, moved.minted)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
}

// 换手门的预检与落点 CAS 必须锚在同一次观察上:预检通过之后 epoch 被并发 EnterScene
// 推进,本次落点要被拒,而不是读到对方推进后的值再 CAS 成功(两个节点同时被派到)。
func TestPlacePlayerLocationIsAnchoredToObservedEpoch(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6117)
	_, err := placePlayerLocation(sc, playerID, 7117, "10", testZoneId, placementGuard{observedEpoch: 0, mint: true})
	require.NoError(t, err)

	// 请求 A、B 都在 epoch=1 时通过了预检;B 先落点。
	winner, err := placePlayerLocation(sc, playerID, 7217, "20", testZoneId, placementGuard{observedEpoch: 1, mint: true})
	require.NoError(t, err)
	require.Equal(t, uint64(2), winner.epoch)

	_, err = placePlayerLocation(sc, playerID, 7317, "30", testZoneId, placementGuard{observedEpoch: 1, mint: true})
	require.ErrorIs(t, err, errOwnerEpochConflict, "A 的观察值已过期,不得再铸造 3")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
	raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, winner.raw, raw)
}

// 凭标记放行的落点:标记必须在铸造的同一段 Lua 里原样还在。源 scene 超时后先 DEL 标记、
// 再读 epoch 判断自己有没有被放行 —— 这个判断成立的前提就是 DEL 之后不可能再有凭它的铸造。
func TestMintIsRefusedOnceTheSourceWithdrewItsHandoffMarker(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6124)
	_, err := placePlayerLocation(sc, playerID, 7124, "10", testZoneId, placementGuard{mint: true})
	require.NoError(t, err)
	writeHandoffMarker(t, sc, playerID, 1)
	verdict, err := checkHandoffCommitted(sc, playerID, 1)
	require.NoError(t, err)
	require.True(t, verdict.committed)
	before, _ := sc.Redis.Get(getPlayerLocationKey(playerID))

	// 预检之后、落点之前,源 scene 撤回了标记。
	mr.Del(ownerepoch.HandoffKey(playerID))
	_, err = placePlayerLocation(sc, playerID, 7224, "20", testZoneId,
		placementGuard{observedEpoch: 1, mint: true, requiredMarker: verdict.marker})
	require.ErrorIs(t, err, errHandoffWithdrawn)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "撤回之后不得再铸造")
	after, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, before, after)

	// 标记原样还在:正常铸造。
	writeHandoffMarker(t, sc, playerID, 1)
	placed, err := placePlayerLocation(sc, playerID, 7224, "20", testZoneId,
		placementGuard{observedEpoch: 1, mint: true, requiredMarker: verdict.marker})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), placed.epoch)
}

// 跨 zone 传送指定的目标地图:第一条腿记进等待落点,第二条腿(请求不带地图)由 scene_manager 读出来用。
func TestEnterScene_TravelTargetMapIsCarriedByAwaitingPlacement(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)

	const (
		playerID = uint64(6126)
		oldScene = uint64(7126)
		confID   = uint64(3326)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "1")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	writeHandoffMarker(t, sc, playerID, 1)
	// 这张图在目标 zone 开着(频道集合非空):第一条腿的只读地图预检只看这一点,不解析、不预占。
	mr.SAdd(worldChannelsKey(2, confID), "7226")

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2, SceneConfId: confID,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)

	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, "", loc.NodeId)
	assert.Equal(t, confID, loc.PendingSceneConfId, "目标地图必须随等待落点一起记下")
	reservedCount, countErr := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, uint64(7226)))
	require.NoError(t, countErr)
	assert.Equal(t, "", reservedCount, "第一条腿的地图预检是只读的,不得预占目标频道人数")

	// 常规落点不得带着 pending 地图(它只属于等待落点)。
	placed, err := placePlayerLocation(sc, playerID, 7226, "20", 2, placementGuard{observedEpoch: 2, mint: true})
	require.NoError(t, err)
	settled := &scene_manager.PlayerLocation{}
	require.NoError(t, gproto.Unmarshal([]byte(placed.raw), settled))
	assert.Equal(t, uint64(0), settled.PendingSceneConfId)
}

// 第一条腿:指定的目标地图在目标 zone 一个世界频道都没有(副本 / 镜像 conf、乱填的 id、只在别的
// zone 开的图)→ 必须在不可回头点之前拒绝,一个字节都不改。源 scene 据此解冻并回 tip(CZ-5)。
// 标记已就绪、gate 也签得出票据:能挡住这次放行的只有地图预检。
func TestEnterScene_TravelToUnopenedMapIsRejectedBeforeReleasingOwnership(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6128)
		oldScene = uint64(7128)
		confID   = uint64(9328)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	writeHandoffMarker(t, sc, playerID, 1)
	oldRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		t.Error("地图预检不过就不该去签目标 zone 的票据")
		return nil, errors.New("unexpected redirect")
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2, SceneConfId: confID,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
	assert.Nil(t, resp.Redirect)

	// 源 scene 仍持有玩家:它缓存的 epoch(1)、location、旧场景人数都必须原样。
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, oldRaw, raw, "被拒的传送不得留下等待落点")
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "3", oldCount)
	assert.Empty(t, *captured, "被拒的传送不得发出 RedirectToGateEvent")
}

// 第二条腿:等待落点里记的目标地图在本 zone 解析不出来(两条腿之间频道被回收,或第一条腿的预检
// 因 Redis 抖动放过了)→ 回落默认大世界,人必须能落地。走到这一步的玩家已被源 scene 销毁,没有
// 「原地」可回;硬拒的话这条等待落点在票据有效期内会让他每一次登录都读到同一个 pending 值再失败一次。
func TestEnterScene_TravelSecondLegFallsBackToDefaultWorldWhenPendingMapIsUnavailable(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID    = uint64(6127)
		defaultConf = uint64(3327)
		missingConf = uint64(9327)
		defaultID   = uint64(7127)
	)
	// 默认大世界 = World 表第一行;单测里 World 表是空的,用覆盖值顶上。
	t.Cleanup(SetWorldConfIdsForTest([]uint64{defaultConf}))
	mr.SAdd(worldChannelsKey(2, defaultConf), fmt.Sprintf("%d", defaultID))
	seedSceneOnNode(mr, 2, defaultID, "10", "0")
	mr.Set(nodePlayerCountKey(2, "10"), "0")
	// 第一条腿留下的等待落点:zone 2、无节点、epoch 1,目标地图 missingConf 在 zone 2 没有任何频道。
	placed, err := placePlayerLocation(sc, playerID, 0, "", 2,
		placementGuard{observedEpoch: 0, mint: true, pendingSceneConfID: missingConf})
	require.NoError(t, err)
	require.Equal(t, uint64(1), placed.epoch)

	// 目标 zone 的 login 发来的第二条腿:不带场景、不带地图。
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2,
		GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, "pending 地图不可用不能把人挡在目标 zone 门外")

	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, defaultID, loc.SceneId, "回落到默认大世界的频道")
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, uint64(0), loc.PendingSceneConfId, "落点之后 pending 地图清零,不再牵引后续登录")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "落点照常铸造下一代")
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(2), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, defaultID))
	assert.Equal(t, "1", count)
}

// 请求自己指定的地图解析失败仍然硬拒:回落只属于「等待落点里记下的地图」。指定地图的请求,其发起方
// (源 scene / 客户端)还持有玩家、能把失败告诉他;悄悄把人送进另一张图才是错的。
func TestEnterScene_ExplicitMapIsNotSubjectToPendingMapFallback(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)

	const (
		playerID    = uint64(6129)
		defaultConf = uint64(3329)
		missingConf = uint64(9329)
		defaultID   = uint64(7129)
	)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{defaultConf}))
	mr.SAdd(worldChannelsKey(2, defaultConf), fmt.Sprintf("%d", defaultID))
	seedSceneOnNode(mr, 2, defaultID, "10", "0")
	mr.Set(nodePlayerCountKey(2, "10"), "0")
	_, err := placePlayerLocation(sc, playerID, 0, "", 2,
		placementGuard{observedEpoch: 0, mint: true, pendingSceneConfID: defaultConf})
	require.NoError(t, err)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2, SceneConfId: missingConf,
		GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, defaultID))
	assert.Equal(t, "0", count, "硬拒的请求不得占用默认大世界的人数")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "硬拒不改归属")
}

// EnterScene 的每一条返回路径都要回显 player_id(scene 节点的异步应答回调靠它对回玩家)。
func TestEnterScene_ResponseEchoesPlayerID(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)
	const (
		playerID = uint64(6125)
		targetID = uint64(7125)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "0")

	ok, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), ok.ErrorCode)
	assert.Equal(t, playerID, ok.PlayerId)

	rejected, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "not-a-number", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.NotEqual(t, uint32(0), rejected.ErrorCode)
	assert.Equal(t, playerID, rejected.PlayerId, "失败应答同样要能对回玩家")
}

// 不铸造的落点回滚:只退 location,epoch 键不动(键不存在时也不能凭空写出 "0")。
func TestRollbackUnmintedPlacementLeavesEpochUntouched(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6118)
	log := NewEnterSceneLogic(context.Background(), sc).Logger

	placed, err := placePlayerLocation(sc, playerID, 7118, "10", testZoneId, placementGuard{observedEpoch: 0, mint: false})
	require.NoError(t, err)
	require.True(t, rollbackPlayerPlacement(sc, log, playerID, placed, "", rollbackEpochFor(nil, placed)))
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.False(t, mr.Exists(ownerepoch.OwnerEpochKey(playerID)), "从未铸造过的 epoch 键不能被回滚写出来")
}

// --- 跨 zone 重定向:只送连接 vs 真正离开 ----------------------------------------

// 访客掉线后从别区 gate 登录:目标 zone 就是玩家所在 zone,重定向只送连接,归属不动、
// 不要求标记(持有节点没在交接,永远写不出标记)。
func TestEnterScene_CrossZoneRedirectIntoOwnZoneLeavesOwnershipUntouched(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)

	const (
		playerID = uint64(6119)
		sceneID  = uint64(7119)
	)
	seedSceneOnNode(mr, 2, sceneID, "10", "1")
	mr.ZAdd(nodeLoadKey(2), 0, "10")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, sceneID, "10", 2))
	before, _ := sc.Redis.Get(getPlayerLocationKey(playerID))

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(_ context.Context, _ *svc.ServiceContext, zone uint32, _ uint64) (*scene_manager.RedirectToGateInfo, error) {
		assert.Equal(t, uint32(2), zone)
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode, "没有 handoff 标记也必须能被送回自己所在的 zone")
	require.NotNil(t, resp.Redirect)
	after, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, before, after, "只送连接:location 不动")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "只送连接:不铸造")
}

func TestAwaitingPlacementExpired(t *testing.T) {
	now := time.Unix(1_757_000_000, 0)
	at := func(ageSeconds int64) *scene_manager.PlayerLocation {
		return &scene_manager.PlayerLocation{ZoneId: 2, OwnerEpoch: 1, UpdateTime: uint64(now.Unix() - ageSeconds)}
	}
	assert.False(t, awaitingPlacementExpired(at(0), now))
	assert.False(t, awaitingPlacementExpired(at(redirectTokenTTLSeconds), now), "恰好到期的票据仍然有效")
	assert.True(t, awaitingPlacementExpired(at(redirectTokenTTLSeconds+1), now))
}

// seedAwaitingPlacement 直接摆一条「等待落点」位置(zone 2、无节点、epoch 1),
// UpdateTime 由调用方决定,用来区分票据有效期内 / 过期两种情形。
func seedAwaitingPlacement(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, playerID uint64, updatedAt time.Time) {
	t.Helper()
	// zone 2 有活节点:这条位置不会被当成「已下线 zone 的陈旧位置」过滤掉。
	mr.ZAdd(nodeLoadKey(2), 0, "10")
	raw, err := gproto.Marshal(&scene_manager.PlayerLocation{ZoneId: 2, OwnerEpoch: 1, UpdateTime: uint64(updatedAt.Unix())})
	require.NoError(t, err)
	require.NoError(t, sc.Redis.Set(getPlayerLocationKey(playerID), string(raw)))
	require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), "1"))
}

// 票据有效期内:没指定去向的登录被等待落点牵去目标 zone(只送连接,归属不动)。
func TestEnterScene_FreshAwaitingPlacementRedirectsToTargetZone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)
	const playerID = uint64(6121)
	seedAwaitingPlacement(t, sc, mr, playerID, time.Now())

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(_ context.Context, _ *svc.ServiceContext, zone uint32, _ uint64) (*scene_manager.RedirectToGateInfo, error) {
		assert.Equal(t, uint32(2), zone)
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	require.NotNil(t, resp.Redirect)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "目标 zone 没变:只送连接,不再铸造")
}

// 票据过期 = 传送失败:同样的登录不再被牵去目标 zone,按 gate zone 走常规落点(回家)。
// 本用例不摆世界频道,所以常规落点以「无可用节点」收场 —— 要证明的只是它**没有**
// 再去签目标 zone 的票据。
func TestEnterScene_ExpiredAwaitingPlacementNoLongerRedirects(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)
	const playerID = uint64(6123)
	seedAwaitingPlacement(t, sc, mr, playerID, time.Now().Add(-(redirectTokenTTLSeconds+60)*time.Second))

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		t.Error("过期的等待落点不应再触发跨区重定向")
		return nil, errors.New("unexpected redirect")
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneConfId: 2001,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Nil(t, resp.Redirect)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
}

// --- home_zone:未配置 ---------------------------------------------------------

// 没配 DataServiceRpc 且没声明单 zone:fail-closed。多 zone 下按 gate zone 当归属会让
// 访客的存盘落错库,而且除一条日志外零报错。
func TestEnterScene_HomeZoneUnconfiguredIsRejectedUnlessSingleZoneDeclared(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	sc.Config.AllowGateZoneAsHomeZone = false

	const (
		playerID = uint64(6122)
		targetID = uint64(7122)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "0")
	req := &scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	}
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnavailable, resp.ErrorCode)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)), "拒绝时不得留下任何状态")
	assert.Empty(t, *captured)

	sc.Config.AllowGateZoneAsHomeZone = true
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(req)
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.Len(t, *captured, 1)
	assert.Equal(t, testZoneId, decodeRoutePlayerEvent(t, (*captured)[0]).HomeZoneId)
}

// --- home_zone --------------------------------------------------------------

func TestEnterScene_HomeZoneMappedIsCarriedInRouteEvent(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	fake := &fakeHomeZoneClient{zone: 7}
	sc.HomeZone = fake

	const (
		playerID = uint64(6114)
		targetID = uint64(7114)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "0")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, 1, fake.calls)
	require.Len(t, *captured, 1)
	event := decodeRoutePlayerEvent(t, (*captured)[0])
	assert.Equal(t, uint32(7), event.HomeZoneId, "访客的存盘落库目的地由映射决定,不是进程 zone")
}

func TestEnterScene_HomeZoneUnmappedFallsBackToGateZone(t *testing.T) {
	// 新版 data_service 的契约:NotFound + 文案;老版是 Unknown + 文案,两种都要认。
	cases := []struct {
		name     string
		err      error
		playerID uint64
		targetID uint64
	}{
		{name: "not_found", err: status.Error(codes.NotFound, "error_code=2: no home zone mapping for player 6115"), playerID: 6115, targetID: 7115},
		{name: "legacy_unknown", err: status.Error(codes.Unknown, "no home zone mapping for player 6116"), playerID: 6116, targetID: 7116},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			captured := capturingKafkaWriter(sc)
			sc.HomeZone = &fakeHomeZoneClient{err: tc.err}
			seedSceneOnNode(mr, testZoneId, tc.targetID, "10", "0")

			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
				PlayerId: tc.playerID, SceneId: tc.targetID, ZoneId: testZoneId,
				GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
			})
			require.NoError(t, err)
			assert.Equal(t, uint32(0), resp.ErrorCode, "未映射 = 首登,不是故障")
			require.Len(t, *captured, 1)
			assert.Equal(t, testZoneId, decodeRoutePlayerEvent(t, (*captured)[0]).HomeZoneId)
		})
	}
}

func TestEnterScene_HomeZoneUnavailableIsRejectedWithoutSideEffects(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	sc.HomeZone = &fakeHomeZoneClient{err: status.Error(codes.Unavailable, "error_code=8: mapping redis down")}

	const (
		playerID = uint64(6117)
		targetID = uint64(7117)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "4")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "home-zone-down",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnavailable, resp.ErrorCode, "归属未知不得静默落进程 zone 库")
	assert.Empty(t, *captured)
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	assert.Nil(t, loc, "拒绝前不得写位置")
	assert.Equal(t, "", ownerEpochRaw(t, sc, playerID), "拒绝前不得铸造 epoch")
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "4", count)
	assert.Equal(t, "4", nodeCount)
	exists, existsErr := sc.Redis.Exists(fmt.Sprintf("enter_scene:dedup:%d:home-zone-down", playerID))
	require.NoError(t, existsErr)
	assert.False(t, exists, "可重试拒绝必须释放 request_id 占位")
}

func TestEnterScene_HomeZoneUnavailableReleasesAutoReservedChannel(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)
	sc.HomeZone = &fakeHomeZoneClient{err: status.Error(codes.DeadlineExceeded, "timeout")}

	const (
		playerID = uint64(6118)
		targetID = uint64(7118)
		confID   = uint64(8118)
	)
	// 自动选频道会在解析时预占人数,拒绝时必须成对释放。
	mr.SAdd(worldChannelsKey(testZoneId, confID), fmt.Sprintf("%d", targetID))
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "4")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneConfId: confID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnavailable, resp.ErrorCode)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "4", count, "拒绝后必须回滚频道预占")
	assert.Equal(t, "4", nodeCount, "拒绝后必须回滚节点预占")
}

func TestEnterScene_HomeZoneNotQueriedWithoutGateRoute(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	fake := &fakeHomeZoneClient{err: status.Error(codes.Unavailable, "down")}
	sc.HomeZone = fake

	const (
		playerID = uint64(6119)
		targetID = uint64(7119)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "0")

	// 没有 GateId 就不会发 RoutePlayerEvent,归属查询没有消费者,不该为它多一次 RPC。
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, 0, fake.calls)
}

// --- 标记解析边界 -----------------------------------------------------------

func TestCheckHandoffCommitted(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6120)

	verdict, err := checkHandoffCommitted(sc, playerID, 0)
	require.NoError(t, err)
	assert.False(t, verdict.committed, "既无 epoch 也无标记:不放行")

	verdict, err = checkHandoffCommitted(sc, playerID, 4)
	require.NoError(t, err)
	assert.False(t, verdict.committed, "无标记:不放行")
	assert.Equal(t, uint64(4), verdict.epoch, "比对用的是调用方观察到的 epoch,不重新读 Redis")

	writeHandoffMarker(t, sc, playerID, 3)
	verdict, err = checkHandoffCommitted(sc, playerID, 4)
	require.NoError(t, err)
	assert.False(t, verdict.committed, "旧一代的标记:不放行")

	writeHandoffMarker(t, sc, playerID, 4)
	verdict, err = checkHandoffCommitted(sc, playerID, 4)
	require.NoError(t, err)
	assert.True(t, verdict.committed, "当前代际的标记:放行")
	verdict, err = checkHandoffCommitted(sc, playerID, 5)
	require.NoError(t, err)
	assert.False(t, verdict.committed, "观察值已被推进到 5,4 的标记不再算数")

	require.NoError(t, sc.Redis.Set(ownerepoch.HandoffKey(playerID), "garbage"))
	verdict, err = checkHandoffCommitted(sc, playerID, 4)
	require.NoError(t, err, "标记写坏不是 Redis 故障")
	assert.False(t, verdict.committed, "标记写坏:不放行")
}
