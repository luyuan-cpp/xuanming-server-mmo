package logic

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
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
	// onCall 非 nil 时在每次查询里先跑一遍。归属查询夹在「解析场景」与「预占人数」之间,
	// 用例借它在这个窗口里改 Redis(destroy-while-entering)。
	onCall func()
}

func (f *fakeHomeZoneClient) GetPlayerHomeZone(_ context.Context, _ *dspb.GetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.GetPlayerHomeZoneResponse, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall()
	}
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

// defaultWorldTestConf 是单测里顶替「World 表第一行」的默认大世界 conf。
const defaultWorldTestConf = uint64(3300)

// seedDefaultWorldChannel 让 zoneID 开着默认大世界:World 表覆盖成 {defaultWorldTestConf},并在该 zone 的
// 频道集合里登记一个频道。没指定地图(scene_conf_id = 0)的跨 zone 第一条腿会先只读预检这一点;
// 预检只看集合是否为空,所以不摆 scene→node 映射,也不会被预占人数。
func seedDefaultWorldChannel(t *testing.T, mr *miniredis.Miniredis, zoneID uint32) {
	t.Helper()
	t.Cleanup(SetWorldConfIdsForTest([]uint64{defaultWorldTestConf}))
	mr.SAdd(worldChannelsKey(zoneID, defaultWorldTestConf), "7300")
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
	// 请求不指定地图(conf=0):第一条腿预检目标 zone 的默认大世界,这里让它开着。
	seedDefaultWorldChannel(t, mr, 2)

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
	seedDefaultWorldChannel(t, mr, 2)
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
	// 再次重定向同样是「要离开 zone 2」:不指定地图时预检 zone 3 的默认大世界。
	seedDefaultWorldChannel(t, mr, 3)

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

// 第一条腿:请求没指定地图(scene_conf_id = 0 = 由目标 zone 挑默认大世界),而目标 zone 的默认大世界
// 一个频道都没有,或者根本没配默认大世界(World 表为空)→ 同样在不可回头点之前拒绝,一个字节都不改。
// 第二条腿对 conf=0 只找默认大世界、不回落,放行就是让源实体销毁后落不了地。
// 标记已就绪、gate 也签得出票据:能挡住这次放行的只有地图预检。
func TestEnterScene_TravelWithoutMapRejectedWhenTargetDefaultWorldUnavailable(t *testing.T) {
	cases := []struct {
		name     string
		worldIDs []uint64 // World 表覆盖值;空切片 = 没配默认大世界
		playerID uint64
		oldScene uint64
	}{
		// 默认大世界配了,但只在 zone 1 开着频道:按「目标 zone」查,不能被源 zone 的频道糊弄过去。
		{name: "default_world_has_no_channel_in_target_zone", worldIDs: []uint64{defaultWorldTestConf}, playerID: 6160, oldScene: 7160},
		{name: "default_world_unconfigured", worldIDs: []uint64{}, playerID: 6161, oldScene: 7161},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			captured := capturingKafkaWriter(sc)
			t.Cleanup(SetWorldConfIdsForTest(tc.worldIDs))
			mr.SAdd(worldChannelsKey(1, defaultWorldTestConf), "7300")

			seedSceneOnNode(mr, 1, tc.oldScene, "10", "3")
			require.NoError(t, UpdatePlayerLocation(context.Background(), sc, tc.playerID, tc.oldScene, "10", 1))
			writeHandoffMarker(t, sc, tc.playerID, 1)
			oldRaw, _ := sc.Redis.Get(getPlayerLocationKey(tc.playerID))
			markerRaw, _ := sc.Redis.Get(ownerepoch.HandoffKey(tc.playerID))
			before := enterSceneRejectedCount(t, 2, rejectReasonTravelMapUnavailable)

			logic := NewEnterSceneLogic(context.Background(), sc)
			logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
				t.Error("地图预检不过就不该去签目标 zone 的票据")
				return nil, errors.New("unexpected redirect")
			}
			resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
				PlayerId: tc.playerID, ZoneId: 2, SceneConfId: 0,
				GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "travel-no-default-world",
			})
			require.NoError(t, err)
			assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
			assert.Nil(t, resp.Redirect)
			assert.Equal(t, before+1, enterSceneRejectedCount(t, 2, rejectReasonTravelMapUnavailable),
				"沿用 travel_map_unavailable,告警 SceneManagerZoneTravelMapUnavailable 才看得见")

			// 源 scene 仍持有玩家并将据此解冻:epoch(1)、location、标记、旧场景人数都必须原样。
			assert.Equal(t, "1", ownerEpochRaw(t, sc, tc.playerID), "被拒的传送不得铸造 epoch")
			raw, _ := sc.Redis.Get(getPlayerLocationKey(tc.playerID))
			assert.Equal(t, oldRaw, raw, "被拒的传送不得留下等待落点")
			markerAfter, _ := sc.Redis.Get(ownerepoch.HandoffKey(tc.playerID))
			assert.Equal(t, markerRaw, markerAfter)
			oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, tc.oldScene))
			assert.Equal(t, "3", oldCount)
			assert.Empty(t, *captured, "被拒的传送不得发出 RedirectToGateEvent")
			exists, existsErr := sc.Redis.Exists(fmt.Sprintf("enter_scene:dedup:%d:travel-no-default-world", tc.playerID))
			require.NoError(t, existsErr)
			assert.False(t, exists, "拒绝必须释放 request_id 占位")
		})
	}
}

// 第一条腿:请求没指定地图,目标 zone 的默认大世界开着 → 照常放行到等待落点。预检只读:不预占频道
// 人数,也不把解析出的默认 conf 写进等待落点(pending 仍为 0,第二条腿按同一口径自己解析)。
func TestEnterScene_TravelWithoutMapReleasesWhenTargetDefaultWorldIsOpen(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6162)
		oldScene = uint64(7162)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)
	before := enterSceneRejectedCount(t, 2, rejectReasonTravelMapUnavailable)

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2, SceneConfId: 0,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.NotNil(t, resp.Redirect)
	assert.Equal(t, before, enterSceneRejectedCount(t, 2, rejectReasonTravelMapUnavailable))

	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, uint32(2), loc.ZoneId)
	assert.Equal(t, "", loc.NodeId)
	assert.Equal(t, uint64(0), loc.PendingSceneConfId, "没指定地图:等待落点不记 pending,由第二条腿按默认大世界口径解析")
	reservedCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, uint64(7300)))
	assert.Equal(t, "", reservedCount, "地图预检是只读的,不得预占默认大世界频道人数")
	require.Len(t, *captured, 1, "RedirectToGateEvent 推给当前 Gate")
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

// 「未映射 = 按 gate zone」对已有**同 zone** 位置记录的存量号同样保留:未回填的老玩家一直就是
// 这么存盘的,不能因为缺映射突然换不了图。只有跨 zone 传送的两条腿才拒(见下面两个用例)。
func TestEnterScene_HomeZoneUnmappedSameZoneSceneSwitchStillFallsBackToGateZone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	sc.HomeZone = &fakeHomeZoneClient{err: status.Error(codes.NotFound, "error_code=2: no home zone mapping for player 6140")}

	const (
		playerID = uint64(6140)
		oldScene = uint64(7140)
		targetID = uint64(7240)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode, "同 zone 换图不是传送,未映射的存量号照常放行")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, targetID, loc.SceneId)
	require.Len(t, *captured, 1)
	assert.Equal(t, testZoneId, decodeRoutePlayerEvent(t, (*captured)[0]).HomeZoneId)
}

// 跨 zone 传送第一条腿(GO-1 主修复):玩家要离开现在所在的 zone,却没有 player:zone 映射(或
// 映射为 0 / 查询失败)→ 在过换手门、铸 epoch、写等待落点之前拒绝,一个字节都不改。放行的话
// 第二条腿会按目标 zone 的 gate zone 当归属,访客存盘静默写进目标 zone 的库。
// 标记已就绪、gate 也签得出票据:能挡住这次放行的只有归属前置检查。
func TestEnterScene_TravelFirstLegRejectsUnmappedHomeZoneWithoutSideEffects(t *testing.T) {
	cases := []struct {
		name     string
		homeZone *fakeHomeZoneClient
		playerID uint64
		oldScene uint64
	}{
		{name: "not_found", homeZone: &fakeHomeZoneClient{err: status.Error(codes.NotFound, "error_code=2: no home zone mapping for player 6141")}, playerID: 6141, oldScene: 7141},
		{name: "legacy_unknown", homeZone: &fakeHomeZoneClient{err: status.Error(codes.Unknown, "no home zone mapping for player 6142")}, playerID: 6142, oldScene: 7142},
		{name: "mapped_to_zero", homeZone: &fakeHomeZoneClient{zone: 0}, playerID: 6143, oldScene: 7143},
		{name: "lookup_failed", homeZone: &fakeHomeZoneClient{err: status.Error(codes.Unavailable, "error_code=8: mapping redis down")}, playerID: 6144, oldScene: 7144},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			captured := capturingKafkaWriter(sc)
			sc.HomeZone = tc.homeZone

			seedSceneOnNode(mr, 1, tc.oldScene, "10", "3")
			require.NoError(t, UpdatePlayerLocation(context.Background(), sc, tc.playerID, tc.oldScene, "10", 1))
			writeHandoffMarker(t, sc, tc.playerID, 1)
			// 地图预检(先于归属检查)要过:目标 zone 开着默认大世界,能挡住这次放行的只剩归属。
			seedDefaultWorldChannel(t, mr, 2)
			oldRaw, _ := sc.Redis.Get(getPlayerLocationKey(tc.playerID))
			markerRaw, _ := sc.Redis.Get(ownerepoch.HandoffKey(tc.playerID))

			logic := NewEnterSceneLogic(context.Background(), sc)
			logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
				t.Error("归属前置检查不过就不该去签目标 zone 的票据")
				return nil, errors.New("unexpected redirect")
			}
			resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
				PlayerId: tc.playerID, ZoneId: 2,
				GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "travel-unmapped",
			})
			require.NoError(t, err)
			assert.Equal(t, constants.ErrHomeZoneUnavailable, resp.ErrorCode)
			assert.Nil(t, resp.Redirect)
			assert.Equal(t, 1, tc.homeZone.calls)

			// 源 scene 仍持有玩家并将据此解冻:它缓存的 epoch(1)、location、标记、旧场景人数都必须原样。
			assert.Equal(t, "1", ownerEpochRaw(t, sc, tc.playerID), "被拒的传送不得铸造 epoch")
			raw, _ := sc.Redis.Get(getPlayerLocationKey(tc.playerID))
			assert.Equal(t, oldRaw, raw, "被拒的传送不得留下等待落点")
			markerAfter, _ := sc.Redis.Get(ownerepoch.HandoffKey(tc.playerID))
			assert.Equal(t, markerRaw, markerAfter)
			oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, tc.oldScene))
			assert.Equal(t, "3", oldCount)
			assert.Empty(t, *captured, "被拒的传送不得发出 RedirectToGateEvent")
			exists, existsErr := sc.Redis.Exists(fmt.Sprintf("enter_scene:dedup:%d:travel-unmapped", tc.playerID))
			require.NoError(t, existsErr)
			assert.False(t, exists, "拒绝必须释放 request_id 占位")
		})
	}
}

// 前置检查只要「有映射」,不挑映射值:有映射的玩家照常放行到等待落点。
func TestEnterScene_TravelFirstLegWithMappedHomeZoneStillReleases(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	fake := &fakeHomeZoneClient{zone: 1}
	sc.HomeZone = fake

	const (
		playerID = uint64(6145)
		oldScene = uint64(7145)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	require.NotNil(t, resp.Redirect)
	assert.Equal(t, 1, fake.calls)
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, uint32(2), loc.ZoneId)
	assert.Equal(t, "", loc.NodeId)
	require.Len(t, *captured, 1)
}

// 只送连接的重定向(没有位置记录的首登,login 的 RedirectOnEnter)不动归属,不该被归属前置
// 检查误伤:未映射的新号照样拿到重定向,而且根本不查。
func TestEnterScene_RedirectOnlyFirstLandingDoesNotQueryHomeZone(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	fake := &fakeHomeZoneClient{err: status.Error(codes.NotFound, "error_code=2: no home zone mapping for player 6146")}
	sc.HomeZone = fake

	const playerID = uint64(6146)
	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, ZoneId: 2,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	require.NotNil(t, resp.Redirect)
	assert.Equal(t, 0, fake.calls)
	assert.Equal(t, "", ownerEpochRaw(t, sc, playerID), "只送连接:不铸造")
	require.Len(t, *captured, 1)
}

// 跨 zone 传送第二条腿(GO-1 纵深防御):正在消费「等待落点」的落点,gate zone 是目标 zone,
// 不能拿来当归属。未映射 → 拒绝,不回落;等待落点、epoch、目标场景人数原样,不发路由。
func TestEnterScene_TravelSecondLegRejectsUnmappedHomeZone(t *testing.T) {
	cases := []struct {
		name     string
		homeZone *fakeHomeZoneClient
		playerID uint64
		targetID uint64
	}{
		{name: "not_found", homeZone: &fakeHomeZoneClient{err: status.Error(codes.NotFound, "error_code=2: no home zone mapping for player 6147")}, playerID: 6147, targetID: 7147},
		{name: "mapped_to_zero", homeZone: &fakeHomeZoneClient{zone: 0}, playerID: 6148, targetID: 7148},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			captured := capturingKafkaWriter(sc)
			sc.HomeZone = tc.homeZone

			seedSceneOnNode(mr, 2, tc.targetID, "10", "4")
			mr.Set(nodePlayerCountKey(2, "10"), "4")
			// 第一条腿留下的等待落点:zone 2、无节点、epoch 1。
			placed, err := placePlayerLocation(sc, tc.playerID, 0, "", 2, placementGuard{observedEpoch: 0, mint: true})
			require.NoError(t, err)
			require.Equal(t, uint64(1), placed.epoch)
			awaitingRaw, _ := sc.Redis.Get(getPlayerLocationKey(tc.playerID))

			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
				PlayerId: tc.playerID, SceneId: tc.targetID, ZoneId: 2,
				GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
			})
			require.NoError(t, err)
			assert.Equal(t, constants.ErrHomeZoneUnavailable, resp.ErrorCode, "第二条腿的 gate zone 是目标 zone,不得当归属")
			assert.Empty(t, *captured)
			assert.Equal(t, "1", ownerEpochRaw(t, sc, tc.playerID))
			raw, _ := sc.Redis.Get(getPlayerLocationKey(tc.playerID))
			assert.Equal(t, awaitingRaw, raw, "等待落点原样留着,映射补上后重试即可落地")
			count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, tc.targetID))
			nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(2, "10"))
			assert.Equal(t, "4", count)
			assert.Equal(t, "4", nodeCount)
		})
	}
}

// 过期的等待落点 + 未映射:票据过期后玩家从自己原来的 gate zone 登录(CZ-9「回家」),不再被牵去
// 目标 zone,但这仍是在消费等待落点 —— gate zone 证明不了归属,同样拒绝,状态原样。
// 把 3b 的条件收窄成「未过期才拒」的话,这里会回落 gate zone 并成功落点。
func TestEnterScene_ExpiredAwaitingPlacementStillRejectsUnmappedHomeZone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	fake := &fakeHomeZoneClient{err: status.Error(codes.NotFound, "error_code=2: no home zone mapping for player 6149")}
	sc.HomeZone = fake

	const (
		playerID = uint64(6149)
		targetID = uint64(7149)
	)
	// 等待落点在 zone 2(有活节点,不会被当成陈旧位置过滤掉),已过票据有效期。
	seedAwaitingPlacement(t, sc, mr, playerID, time.Now().Add(-(redirectTokenTTLSeconds+60)*time.Second))
	awaitingRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "4")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnavailable, resp.ErrorCode, "过期的等待落点仍是跨 zone 传送的残留,gate zone 不得当归属")
	assert.Nil(t, resp.Redirect)
	assert.Equal(t, 1, fake.calls)
	assert.Empty(t, *captured)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, awaitingRaw, raw)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "4", count)
	assert.Equal(t, "4", nodeCount)
}

// enterSceneRejectedCount 从默认注册表读 scene_manager_enter_scene_rejected_total 的一个取值
// (metrics 包不导出计数器)。还没发射过的 label 组合读作 0;用例只比较前后差值。
func enterSceneRejectedCount(t *testing.T, zoneID uint32, reason string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	zone := fmt.Sprintf("%d", zoneID)
	for _, family := range families {
		if family.GetName() != "scene_manager_enter_scene_rejected_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["zone_id"] == zone && labels["reason"] == reason {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// destroy-while-entering:请求指定的场景在解析之后、预占人数之前被回收 → ErrNoAvailableNode,
// 记 enter_scene_rejected_total{reason="scene_gone"}(告警 SceneManagerEnterSceneSceneGone 钉在
// 它上面;这个发射点丢过一次,告警恒空了几个月而零报错),不写位置、不铸 epoch、不重建人数键。
// 归属查询正好夹在两步之间,借假实现的 onCall 在那个窗口里删掉 scene→node 映射。
func TestEnterScene_SceneDestroyedWhileEnteringIsCountedAsSceneGone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6150)
		targetID = uint64(7150)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "4")
	sc.HomeZone = &fakeHomeZoneClient{zone: testZoneId, onCall: func() {
		mr.Del(fmt.Sprintf(SceneNodeKeyFmt, targetID))
	}}
	before := enterSceneRejectedCount(t, testZoneId, "scene_gone")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
	assert.Equal(t, before+1, enterSceneRejectedCount(t, testZoneId, "scene_gone"))
	assert.Empty(t, *captured)
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	assert.Nil(t, loc)
	assert.Equal(t, "", ownerEpochRaw(t, sc, playerID))
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "4", count, "场景已不存在:不得再 INCR 出人数")
	assert.Equal(t, "4", nodeCount)
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

// --- 单节点判死后的接管 ---------------------------------------------------------

// seedPlayerOnDeadNode 摆出「zone 还活着(node 20 在负载集里),但玩家位置记录指向的 node 10
// 已经不在负载集里」的局面,目标场景在活着的 node 20 上。生产配置(不开 dev 旁路),没有
// handoff 标记 —— 死掉的节点永远写不出来。
// markKnownNodesSyncedForTest 模拟「本进程已完成首次 etcd 全量同步」。单测不跑
// StartLoadReporter,不置这个标志的话 isNodeGoneFromRegistry 恒为 false。
func markKnownNodesSyncedForTest(t *testing.T) {
	t.Helper()
	prev := knownNodesSynced.Load()
	knownNodesSynced.Store(true)
	t.Cleanup(func() { knownNodesSynced.Store(prev) })
}

// registerKnownNodeForTest 往 etcd 注册表的内存镜像里放一个节点(= 它的租约还活着)。
func registerKnownNodeForTest(t *testing.T, key string, zoneID uint32, nodeID string) {
	t.Helper()
	entry := nodeEntry{nodeID: nodeID}
	entry.reg.ZoneId = zoneID
	knownNodesMu.Lock()
	knownNodes[key] = entry
	knownNodesMu.Unlock()
}

func seedPlayerOnDeadNode(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, playerID, oldScene, targetID uint64) {
	t.Helper()
	markKnownNodesSyncedForTest(t)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	mr.ZRem(nodeLoadKey(testZoneId), "10")
	withReachableSceneNode(t, sc, "20")
}

// 节点已确认死亡、再入屏障已过:没有任何进程可能还在写这名玩家,无标记也必须放行,
// 否则一次单节点崩溃会把它名下的玩家永久挡在门外(只能等运维手工清 location)。
// 放行必须铸造新 epoch —— 那是对「其实没死的僵尸节点」的兜底。
func TestEnterScene_DeadOwnerAfterBarrierIsTakenOverWithoutMarker(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6130)
		oldScene = uint64(7130)
		targetID = uint64(7230)
	)
	seedPlayerOnDeadNode(t, sc, mr, playerID, oldScene, targetID)
	require.False(t, sc.Config.AllowUnsafeCrossNodeHandoff, "本用例必须跑在生产口径下")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode, "属主已死且屏障已过,不应再要求 handoff 标记")

	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "接管必须铸造新 epoch,僵尸节点手里的 1 从此作废")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, targetID, loc.SceneId)
	assert.Equal(t, "20", loc.NodeId)
	assert.Equal(t, uint64(2), loc.OwnerEpoch)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(2), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)

	// 旧场景还在(大世界频道会沿用同一个 scene_id 迁到活节点,人数原值保留、没有任何重算路径):
	// 接管成功后必须把这名玩家占的那一个人数还回去,否则那个频道永久虚高。
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "2", oldCount)
}

// 旧场景是已被 dead-node reconcile 销毁的副本实例(scene:{id}:node 与计数键都没了):
// 不得为了"还人数"把计数键重新建出来。
func TestEnterScene_DeadOwnerTakeoverDoesNotRecreateCountOfDestroyedScene(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)
	const (
		playerID = uint64(6135)
		oldScene = uint64(7135)
		targetID = uint64(7235)
	)
	seedPlayerOnDeadNode(t, sc, mr, playerID, oldScene, targetID)
	mr.Del(fmt.Sprintf(SceneNodeKeyFmt, oldScene))
	mr.Del(fmt.Sprintf(InstancePlayerCountKey, oldScene))

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.False(t, mr.Exists(fmt.Sprintf(InstancePlayerCountKey, oldScene)))
}

// leader 缺位时节点丢了租约:没有人写 Redis 的 death_at。本副本亲眼看到它从注册表消失,
// 屏障就从那一刻起算 —— 老进程 15s 的紧急疏散存盘还没跑完,不得接管。
func TestPlayerLocationOwnerDeadHonoursLocallyObservedDisappearance(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	markKnownNodesSyncedForTest(t)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
	loc := &scene_manager.PlayerLocation{SceneId: 7136, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1}
	logic := NewEnterSceneLogic(context.Background(), sc)
	key := nodeGoneObservedKey(testZoneId, "10")
	t.Cleanup(func() {
		nodeGoneObservedMu.Lock()
		delete(nodeGoneObservedAt, key)
		nodeGoneObservedMu.Unlock()
	})

	noteNodeGoneFromRegistry(testZoneId, "10")
	assert.False(t, logic.playerLocationOwnerDead(loc, testZoneId), "刚看到它消失,Redis 里又没有 death_at:仍在屏障内")

	nodeGoneObservedMu.Lock()
	nodeGoneObservedAt[key] = time.Now().Add(-2 * sceneReentryBarrier(sc))
	nodeGoneObservedMu.Unlock()
	assert.True(t, logic.playerLocationOwnerDead(loc, testZoneId), "本地观察到的消失已过屏障")
}

// 刚判死:C++ 老进程还在 kDrainBudget 的紧急疏散里存盘。屏障没走完之前位置记录仍代表一个
// 可能在写的属主 —— 照旧以可重试的 18 暂拒,且不改任何状态。
func TestEnterScene_DeadOwnerWithinBarrierIsStillRejected(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6131)
		oldScene = uint64(7131)
		targetID = uint64(7231)
	)
	seedPlayerOnDeadNode(t, sc, mr, playerID, oldScene, targetID)
	markNodeDeath(sc, testZoneId, "10")
	before, _ := sc.Redis.Get(getPlayerLocationKey(playerID))

	req := &scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	}
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	after, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, before, after)
	assert.Empty(t, *captured)
	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	assert.Equal(t, "4", targetCount, "拒绝时不得留下目标场景预占")

	// 屏障过后(死亡标记消失)同一请求放行。
	mr.Del(nodeDeathAtKey(testZoneId, "10"))
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(req)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
}

// 进程死了但 etcd 租约还没到期(或者节点其实活着、只是这一刻不在负载集里:leader 刷新间隔、
// leader 缺位等):注册表里还有它 = 没有死亡的正面证据,不得接管。
func TestEnterScene_OwnerMissingFromLoadSetButStillRegisteredIsNotTakenOver(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6133)
		oldScene = uint64(7133)
		targetID = uint64(7233)
	)
	seedPlayerOnDeadNode(t, sc, mr, playerID, oldScene, targetID)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/still-registered-10", testZoneId, "10")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "只是不在负载集不算死:照旧走安全交接")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Empty(t, *captured)
}

// 本进程还没完成首次 etcd 全量同步:注册表是空的,「表里没有」不是证据。
func TestPlayerLocationOwnerDeadRequiresSyncedRegistry(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
	prev := knownNodesSynced.Load()
	knownNodesSynced.Store(false)
	t.Cleanup(func() { knownNodesSynced.Store(prev) })

	loc := &scene_manager.PlayerLocation{SceneId: 7134, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1}
	logic := NewEnterSceneLogic(context.Background(), sc)
	assert.False(t, logic.playerLocationOwnerDead(loc, testZoneId))
	knownNodesSynced.Store(true)
	assert.True(t, logic.playerLocationOwnerDead(loc, testZoneId))
}

// (zone,node) 身份有歧义(node_id 被复用、两代进程并存)时 IsNodeAlive 返回 false,但那不是
// 「死了」的证据 —— 不能据此接管,必须 fail-closed 沿用换手门的拒绝。
func TestPlayerLocationOwnerDeadFailsClosedOnAmbiguousIdentity(t *testing.T) {
	// knownNodes 是包级全局表:别的用例留下的同号节点会让「前提:无歧义」不成立。
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	markKnownNodesSyncedForTest(t)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")
	loc := &scene_manager.PlayerLocation{SceneId: 7132, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1}
	logic := NewEnterSceneLogic(context.Background(), sc)
	require.True(t, logic.playerLocationOwnerDead(loc, testZoneId), "前提:无歧义、不在负载集、无 death_at → 判死")

	first := nodeEntry{nodeID: "10"}
	first.reg.ZoneId = testZoneId
	first.reg.GrpcEndpoint.IP = "10.0.0.1"
	second := nodeEntry{nodeID: "10"}
	second.reg.ZoneId = testZoneId
	second.reg.GrpcEndpoint.IP = "10.0.0.2"
	knownNodesMu.Lock()
	knownNodes["SceneNodeService.rpc/owner-dead-a"] = first
	knownNodes["SceneNodeService.rpc/owner-dead-b"] = second
	knownNodesMu.Unlock()
	assert.False(t, logic.playerLocationOwnerDead(loc, testZoneId), "歧义身份不是死亡证据")
	clearKnownNodesForTest()

	// 其余 fail-closed 输入。
	assert.False(t, logic.playerLocationOwnerDead(nil, testZoneId))
	assert.False(t, logic.playerLocationOwnerDead(&scene_manager.PlayerLocation{NodeId: "30", OwnerEpoch: 1}, 0), "zone 无法确定")
	assert.False(t, logic.playerLocationOwnerDead(&scene_manager.PlayerLocation{ZoneId: testZoneId, OwnerEpoch: 1}, testZoneId), "等待落点另有规则")
	assert.False(t, logic.playerLocationOwnerDead(&scene_manager.PlayerLocation{NodeId: "20", ZoneId: testZoneId}, testZoneId), "节点还活着")
}
