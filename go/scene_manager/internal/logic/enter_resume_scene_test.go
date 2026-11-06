package logic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
	"scene_manager/internal/svc"
)

// 短重连 / 顶号回原场景(docs/design/player-connection-lost-hold.md §6)。

func TestResumeSceneOf(t *testing.T) {
	onNode := &scene_manager.PlayerLocation{SceneId: 7401, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 3}
	noDestination := &scene_manager.EnterSceneRequest{PlayerId: 6300}
	cases := []struct {
		name              string
		in                *scene_manager.EnterSceneRequest
		sceneConfID       uint64
		loc               *scene_manager.PlayerLocation
		awaitingPlacement bool
		currentZoneID     uint32
		targetZoneID      uint32
		want              uint64
	}{
		{"login 短重连:没有去向、location 落在本 zone 的节点上", noDestination, 0, onNode, false, testZoneId, testZoneId, 7401},
		{"首登带本 zone", &scene_manager.EnterSceneRequest{ZoneId: testZoneId}, 0, onNode, false, testZoneId, testZoneId, 0},
		{"跟随 / 客户端选线带 scene_id", &scene_manager.EnterSceneRequest{SceneId: 7999}, 0, onNode, false, testZoneId, testZoneId, 0},
		{"请求或等待落点给了地图", noDestination, 3300, onNode, false, testZoneId, testZoneId, 0},
		{"没有 location", noDestination, 0, nil, false, 0, testZoneId, 0},
		{"跨 zone 传送途中的等待落点", noDestination, 0, &scene_manager.PlayerLocation{ZoneId: testZoneId, OwnerEpoch: 3}, true, testZoneId, testZoneId, 0},
		{"location 没有节点", noDestination, 0, &scene_manager.PlayerLocation{SceneId: 7401, ZoneId: testZoneId}, false, testZoneId, testZoneId, 0},
		{"location 没有场景", noDestination, 0, &scene_manager.PlayerLocation{NodeId: "10", ZoneId: testZoneId}, false, testZoneId, testZoneId, 0},
		{"location 的 zone 不明", noDestination, 0, onNode, false, 0, testZoneId, 0},
		{"location 在别的 zone", noDestination, 0, onNode, false, testZoneId + 1, testZoneId, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resumeSceneOf(c.in, c.sceneConfID, c.loc, c.awaitingPlacement, c.currentZoneID, c.targetZoneID))
		})
	}
}

// 在节点 10 的副本里断线、租约内重连(login 发 ZoneId = 0):回到原副本,是同落点重连 ——
// 不铸造、不改 location、人数不重复计,路由带原场景与当前 epoch。默认大世界另有一个活频道在节点 11 上:
// 加回原场景之前,这个请求会被挑去那个频道、撞上换手门(18),断线保留在节点 10 的实体也接不回来。
func TestEnterScene_ZeroZoneReconnectReturnsToPreviousScene(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	sc.HomeZone = &fakeHomeZoneClient{zone: testZoneId}

	const (
		playerID    = uint64(6301)
		dungeonID   = uint64(7401)
		defaultConf = uint64(3307)
		channelID   = uint64(7402)
	)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{defaultConf}))
	mr.SAdd(worldChannelsKey(testZoneId, defaultConf), fmt.Sprintf("%d", channelID))
	seedSceneOnNode(mr, testZoneId, channelID, "11", "0")

	seedSceneOnNode(mr, testZoneId, dungeonID, "10", "1")
	seedNodeMembers(mr, testZoneId, "10", 1)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, dungeonID, "10", testZoneId))
	adoptSceneMember(mr, dungeonID, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	beforeRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		t.Error("location 就在 gate zone:不应触发跨区重定向")
		return nil, errors.New("unexpected redirect")
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)
	assert.Nil(t, resp.Redirect)

	require.Len(t, *captured, 1)
	event := decodeRoutePlayerEvent(t, (*captured)[0])
	assert.Equal(t, dungeonID, event.SceneId, "回到 location 记的场景,不是默认大世界的频道")
	assert.Equal(t, uint32(10), event.TargetNodeId)
	assert.Equal(t, uint64(1), event.OwnerEpoch, "同落点重连原样带当前 epoch")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "同落点重连不铸造")
	afterRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, beforeRaw, afterRaw, "同落点重连不改写 location")
	assert.Equal(t, "1", sceneCountText(sc, dungeonID), "重连不重复计人数")
	assert.Equal(t, "0", sceneCountText(sc, channelID), "没有去挑默认频道,也就没有预占")
}

// 原场景是正在回收的频道(scene:{id}:draining 在,映射还在):不回去 —— 排空自己会把人改派到默认大世界,
// 回进去只会再被搬一次。直接落到默认大世界在役的频道;两条线同在节点 10 上,走同节点换图、不铸造。
func TestEnterScene_ZeroZoneReconnectSkipsDrainingChannel(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	sc.HomeZone = &fakeHomeZoneClient{zone: testZoneId}

	const (
		playerID    = uint64(6303)
		drainingID  = uint64(7405)
		defaultConf = uint64(3309)
		activeID    = uint64(7406)
	)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{defaultConf}))
	mr.SAdd(worldChannelsKey(testZoneId, defaultConf), fmt.Sprintf("%d", activeID))
	seedSceneOnNode(mr, testZoneId, activeID, "10", "0")
	// 回收中的线:已从在役集合摘到回收中集合,映射还在,等人走空。
	seedSceneOnNode(mr, testZoneId, drainingID, "10", "1")
	mr.SAdd(worldDrainingSetKey(testZoneId, defaultConf), fmt.Sprintf("%d", drainingID))
	require.NoError(t, mr.Set(sceneDrainingKey(drainingID), "1"))
	seedNodeMembers(mr, testZoneId, "10", 1)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, drainingID, "10", testZoneId))
	adoptSceneMember(mr, drainingID, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		t.Error("location 就在 gate zone:不应触发跨区重定向")
		return nil, errors.New("unexpected redirect")
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)

	require.Len(t, *captured, 1)
	event := decodeRoutePlayerEvent(t, (*captured)[0])
	assert.Equal(t, activeID, event.SceneId, "回收中的线不回去,按排空的去向落到在役频道")
	assert.Equal(t, uint32(10), event.TargetNodeId)
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, activeID, loc.SceneId)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "同节点换图不铸造")
}

// 原场景已经回收(scene→node 映射没了):回落默认大世界,与加回原场景之前相同。频道与 location 同在节点 10 上,
// 走的是同节点换图:不铸造,location 改到频道,路由带频道。
func TestEnterScene_ZeroZoneReconnectFallsBackWhenPreviousSceneIsGone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	sc.HomeZone = &fakeHomeZoneClient{zone: testZoneId}

	const (
		playerID    = uint64(6302)
		goneID      = uint64(7403)
		defaultConf = uint64(3308)
		channelID   = uint64(7404)
	)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{defaultConf}))
	mr.SAdd(worldChannelsKey(testZoneId, defaultConf), fmt.Sprintf("%d", channelID))
	seedSceneOnNode(mr, testZoneId, channelID, "10", "0")
	seedNodeMembers(mr, testZoneId, "10", 1)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, goneID, "10", testZoneId))
	adoptNodeMember(mr, testZoneId, "10", playerID)

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		t.Error("location 就在 gate zone:不应触发跨区重定向")
		return nil, errors.New("unexpected redirect")
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, resp.ErrorMessage)

	require.Len(t, *captured, 1)
	event := decodeRoutePlayerEvent(t, (*captured)[0])
	assert.Equal(t, channelID, event.SceneId, "原场景不在了:回落默认大世界")
	assert.Equal(t, uint32(10), event.TargetNodeId)
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, channelID, loc.SceneId)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "同节点换图不铸造")
}
