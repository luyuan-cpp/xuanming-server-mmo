package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	event "proto/common/event"
	kafkacontracts "proto/contracts/kafka"
	"proto/scene_manager"
	game "scene_manager/generated/pb/game"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/kafkacmd"
)

// 托管落点(enter_hosted_placement.go,docs/design/player-connection-lost-hold.md §4.2):离线托管的角色跟着队长换场景,
// 请求不带 gate,提交点是直接发给目标 scene 节点的 PlayerHostedPlacementEvent。

const (
	hostedTestSession    = uint32(77)
	hostedTestGeneration = uint64(5)
)

// setKnownNodeInstanceForTest 往本副本的节点镜像里塞一条带实例号的注册:托管落点要靠它填 target_instance_id。
// SetKnownNodeForTest 不带实例号(它服务的是 Pod IP 映射),这里在它之上补一个字段,测试结束还原。
func setKnownNodeInstanceForTest(t *testing.T, zoneID uint32, nodeID, instance string) {
	t.Helper()
	key := fmt.Sprintf("SceneNodeService.rpc/hosted-test/zone/%d/node/%s", zoneID, nodeID)
	restore := SetKnownNodeForTest(key, nodeID, "", zoneID, 0)
	knownNodesMu.Lock()
	entry := knownNodes[key]
	entry.reg.NodeUuid = instance
	knownNodes[key] = entry
	knownNodesMu.Unlock()
	t.Cleanup(restore)
}

// hostedPlacementRequest 是 scene 节点替托管角色发的形状:没有 gate,带断开那次登录的身份,带关联号(scene 自发)。
func hostedPlacementRequest(playerID, sceneID uint64) *scene_manager.EnterSceneRequest {
	return &scene_manager.EnterSceneRequest{OwnerRevision: 1, PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
		GateZoneId: testZoneId, SessionId: hostedTestSession, LoginGeneration: hostedTestGeneration,
		HostedPlacement: true, CorrelationId: 9001}
}

// hostedRouteCapture 同时记下发出的 Kafka 消息与 "route" 事件(与 releaseDispatchRecorder 的 "release" 排在同一条时间线上)。
type hostedRouteCapture struct {
	mu       sync.Mutex
	messages []kafka.Message
}

func (c *hostedRouteCapture) snapshot() []kafka.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]kafka.Message(nil), c.messages...)
}

// installHostedRouteCapture 必须排在 installReleaseDispatchRecorder 之后:那里也会替换 sc.Kafka。
func installHostedRouteCapture(sc *svc.ServiceContext, rec *releaseDispatchRecorder, writeErr error) *hostedRouteCapture {
	capture := &hostedRouteCapture{}
	sc.Kafka = &countingKafkaWriter{err: writeErr, onWrite: func(msgs []kafka.Message) {
		capture.mu.Lock()
		capture.messages = append(capture.messages, msgs...)
		capture.mu.Unlock()
		if rec != nil {
			rec.mu.Lock()
			rec.events = append(rec.events, "route")
			rec.mu.Unlock()
		}
	}}
	sc.Config.KafkaWriteTimeoutSeconds = 1
	return capture
}

func decodeHostedPlacement(t *testing.T, msg kafka.Message) (*kafkacontracts.SceneCommand, *event.PlayerHostedPlacementEvent) {
	t.Helper()
	cmd := &kafkacontracts.SceneCommand{}
	require.NoError(t, gproto.Unmarshal(msg.Value, cmd))
	require.Equal(t, uint32(game.PlayerHostedPlacementEventEventId), cmd.GetEventId())
	require.Equal(t, kafkacontracts.SceneCommand_DispatchEvent, cmd.GetCommandType())
	placement := &event.PlayerHostedPlacementEvent{}
	require.NoError(t, gproto.Unmarshal(cmd.GetPayload(), placement))
	return cmd, placement
}

// 同节点换图:不铸造,location 指到新场景,事件发给同一个节点(实体就在那里,收到后就地换场景),不派 ReleasePlayer。
func TestEnterScene_HostedPlacementSameNodeDeliversSceneCommand(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	rec := installReleaseDispatchRecorder(t, sc, nil)
	capture := installHostedRouteCapture(sc, rec, nil)
	const (
		playerID = uint64(6971)
		oldScene = uint64(7981)
		target   = uint64(7982)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, target, "10", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	adoptSceneMember(mr, oldScene, playerID)
	setKnownNodeInstanceForTest(t, testZoneId, "10", "scene-10-uuid")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(hostedPlacementRequest(playerID, target))
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)

	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, target, loc.SceneId)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "同节点换图持有者没换,不铸造")

	messages := capture.snapshot()
	require.Len(t, messages, 1, "提交点是一条托管落点事件,不发 gate 路由")
	assert.Equal(t, kafkacmd.SceneCommandTopic(), messages[0].Topic)
	assert.Equal(t, kafkacmd.SceneCommandPartition(10), messages[0].Partition, "落点是 node_id % P,不是 key 哈希")
	assert.Equal(t, strconv.FormatUint(playerID, 10), string(messages[0].Key), "key = player_id,同一玩家保序")
	cmd, placement := decodeHostedPlacement(t, messages[0])
	assert.Equal(t, uint32(10), cmd.GetTargetSceneId())
	assert.Equal(t, "scene-10-uuid", cmd.GetTargetInstanceId(), "防僵尸:必须带目标节点的实例号")
	assert.Equal(t, playerID, cmd.GetPlayerId())
	assert.Equal(t, playerID, placement.GetPlayerId())
	assert.Equal(t, target, placement.GetSceneId())
	assert.Equal(t, uint64(1), placement.GetOwnerEpoch())
	assert.Equal(t, hostedTestSession, placement.GetSessionId())
	assert.Equal(t, hostedTestGeneration, placement.GetLoginGeneration())
	assert.Equal(t, uint64(1), placement.GetOwnerRevision())
	assert.Equal(t, testZoneId, placement.GetHomeZoneId(), "托管落点照常查归属 zone(单测按 gate zone 回落)")

	calls, _ := rec.snapshot()
	assert.Empty(t, calls, "同节点换图不派 ReleasePlayer")
}

// 跨节点:凭源节点为当前代写的标记放行,铸造新 epoch,事件发给目标节点、带新 epoch;提交之后才派 ReleasePlayer 给源节点。
func TestEnterScene_HostedPlacementCrossNodeWithMarkerMintsAndDeliversToTarget(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	rec := installReleaseDispatchRecorder(t, sc, nil)
	capture := installHostedRouteCapture(sc, rec, nil)
	const (
		playerID = uint64(6972)
		oldScene = uint64(7983)
		target   = uint64(7984)
	)
	releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, target, true)
	setKnownNodeInstanceForTest(t, testZoneId, "20", "scene-20-uuid")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(hostedPlacementRequest(playerID, target))
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)

	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "换了持有者必须铸造,源节点迟到的写才会被写权行拒掉")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, target, loc.SceneId)
	assert.Equal(t, "20", loc.NodeId)

	messages := capture.snapshot()
	require.Len(t, messages, 1)
	assert.Equal(t, kafkacmd.SceneCommandPartition(20), messages[0].Partition)
	cmd, placement := decodeHostedPlacement(t, messages[0])
	assert.Equal(t, uint32(20), cmd.GetTargetSceneId())
	assert.Equal(t, "scene-20-uuid", cmd.GetTargetInstanceId())
	assert.Equal(t, uint64(2), placement.GetOwnerEpoch(), "目标节点拿新铸的 epoch 认领")

	calls, events := rec.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "10", calls[0].oldNodeID)
	assert.Equal(t, uint64(1), calls[0].expectedOwnerEpoch)
	assert.Equal(t, "20", calls[0].locationNodeAtDispatch, "派发时落点必须已提交")
	assert.Equal(t, []string{"route", "release"}, events, "先过提交点(broker ACK),再派 ReleasePlayer")
}

// 跨节点第一跳没有标记:照常回 18(源节点据此冻结、存盘、写标记后重发),一个字节都不改,不发事件。
func TestEnterScene_HostedPlacementCrossNodeWithoutMarkerIsHandoffPending(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	rec := installReleaseDispatchRecorder(t, sc, nil)
	capture := installHostedRouteCapture(sc, rec, nil)
	const (
		playerID = uint64(6973)
		oldScene = uint64(7985)
		target   = uint64(7986)
	)
	releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, target, false)
	setKnownNodeInstanceForTest(t, testZoneId, "20", "scene-20-uuid")
	targetBefore := sceneCountText(sc, target)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(hostedPlacementRequest(playerID, target))
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode)

	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Equal(t, "10", loc.NodeId)
	assert.Empty(t, capture.snapshot())
	assert.Equal(t, targetBefore, sceneCountText(sc, target))
	calls, _ := rec.snapshot()
	assert.Empty(t, calls)
}

// 投递事件失败:与推 gate 路由失败同一套回滚 —— location 退回源节点,epoch 只进不退(凭标记铸造的走 bump),不派 ReleasePlayer。
func TestEnterScene_HostedPlacementRouteFailureRollsBack(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	rec := installReleaseDispatchRecorder(t, sc, nil)
	capture := installHostedRouteCapture(sc, rec, errors.New("broker unavailable"))
	const (
		playerID = uint64(6974)
		oldScene = uint64(7987)
		target   = uint64(7988)
	)
	releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, target, true)
	setKnownNodeInstanceForTest(t, testZoneId, "20", "scene-20-uuid")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(hostedPlacementRequest(playerID, target))
	require.NoError(t, err)
	require.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Len(t, capture.snapshot(), 1, "必须实际尝试一次同步投递")

	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, oldScene, loc.SceneId, "回滚:location 退回本次之前的落点")
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID), "铸出 2 之后 bump 回滚到 3,源节点凭回执采纳(GO-2)")
	calls, _ := rec.snapshot()
	assert.Empty(t, calls, "路由失败与回滚都不得派发 ReleasePlayer")
}

// 请求形状不对:在任何去重、预占、落点写之前拒绝。
func TestEnterScene_HostedPlacementRejectsMalformedRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*scene_manager.EnterSceneRequest)
	}{
		{"carries_gate", func(in *scene_manager.EnterSceneRequest) { in.GateId = "1"; in.GateInstanceId = "gate-uuid-test" }},
		{"carries_gate_instance_only", func(in *scene_manager.EnterSceneRequest) { in.GateInstanceId = "gate-uuid-test" }},
		{"no_session", func(in *scene_manager.EnterSceneRequest) { in.SessionId = 0 }},
		{"no_login_generation", func(in *scene_manager.EnterSceneRequest) { in.LoginGeneration = 0 }},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			capture := installHostedRouteCapture(sc, nil, nil)
			playerID := uint64(6980 + i)
			oldScene, target := uint64(7990+2*i), uint64(7991+2*i)
			seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
			seedSceneOnNode(mr, testZoneId, target, "10", "4")
			require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
			setKnownNodeInstanceForTest(t, testZoneId, "10", "scene-10-uuid")
			targetBefore := sceneCountText(sc, target)

			in := hostedPlacementRequest(playerID, target)
			c.mutate(in)
			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(in)
			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidGateID, resp.ErrorCode)

			loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
			require.NotNil(t, loc)
			assert.Equal(t, oldScene, loc.SceneId)
			assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
			assert.Empty(t, capture.snapshot())
			assert.Equal(t, targetBefore, sceneCountText(sc, target))
		})
	}
}

// 本副本镜像里拿不到目标节点唯一的实例号:投递不了防僵尸的事件,在任何落点写之前可重试地拒绝(4),预占成对释放。
func TestEnterScene_HostedPlacementWithoutRegisteredInstanceRejectsBeforeAnyWrite(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	rec := installReleaseDispatchRecorder(t, sc, nil)
	capture := installHostedRouteCapture(sc, rec, nil)
	const (
		playerID = uint64(6990)
		oldScene = uint64(8001)
		target   = uint64(8002)
	)
	releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, target, true)
	// 目标节点 20 只有不带实例号的旧版注册。
	t.Cleanup(SetKnownNodeForTest("SceneNodeService.rpc/hosted-test/legacy/20", "20", "", testZoneId, 0))
	targetBefore := sceneCountText(sc, target)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(hostedPlacementRequest(playerID, target))
	require.NoError(t, err)
	assert.Equal(t, constants.ErrInvalidNodeID, resp.ErrorCode)

	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Empty(t, capture.snapshot())
	assert.Equal(t, targetBefore, sceneCountText(sc, target))
	calls, _ := rec.snapshot()
	assert.Empty(t, calls)
}

// 跨 zone:没有 gate 可重定向,预检拒绝,不写等待落点。
func TestEnterScene_HostedPlacementAcrossZonesIsRejected(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capture := installHostedRouteCapture(sc, nil, nil)
	const (
		playerID = uint64(6991)
		oldScene = uint64(8003)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	writeHandoffMarker(t, sc, playerID, 1)

	in := hostedPlacementRequest(playerID, 0)
	in.ZoneId = 2
	in.SceneConfId = defaultWorldTestConf
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(in)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrInvalidGateID, resp.ErrorCode)

	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, testZoneId, loc.ZoneId, "不得写成目标 zone 的等待落点")
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Empty(t, capture.snapshot())
}

// location 已经指向这个场景(上一条事件还没被消费):照样再发一次让实体收敛,不铸造、不改落点。
func TestEnterScene_HostedPlacementSamePlacementRedeliversRoute(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capture := installHostedRouteCapture(sc, nil, nil)
	const (
		playerID = uint64(6992)
		target   = uint64(8004)
	)
	seedSceneOnNode(mr, testZoneId, target, "10", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, target, "10", testZoneId))
	adoptSceneMember(mr, target, playerID)
	setKnownNodeInstanceForTest(t, testZoneId, "10", "scene-10-uuid")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(hostedPlacementRequest(playerID, target))
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))

	messages := capture.snapshot()
	require.Len(t, messages, 1)
	cmd, placement := decodeHostedPlacement(t, messages[0])
	assert.Equal(t, "scene-10-uuid", cmd.GetTargetInstanceId())
	assert.Equal(t, target, placement.GetSceneId())
	assert.Equal(t, uint64(1), placement.GetOwnerEpoch())
}
