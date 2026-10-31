package logic

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

// EnterScene 的陈旧位置判定(playerLocationOwnerGone)只认 etcd 注册表的正面证据:zone 在注册表里已没有任何
// 场景节点。负载集 scene_nodes:zone:{z}:load 整键丢失 / 被淘汰时 ZCARD 读作 0、ZSCORE 为空、也没有 death_at,
// 只看负载集会把活着的持有节点所在的 zone 判成已下线:陈旧位置被过滤、绕过换手门、在别的 zone 落点并铸造新 epoch,
// 持有节点之后带旧 epoch 的存盘全被 owner CAS 拒绝(回档)。这是第二轮审计 r2-sm#3 的同源缺陷,
// 见 location-release-binding.md §14。

// resetRegistryMirrorForTest 清空 etcd 注册表的内存镜像,并标记「已完成首次全量同步」;用例结束时还原。
func resetRegistryMirrorForTest(t *testing.T) {
	t.Helper()
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	markKnownNodesSyncedForTest(t)
}

// forgetNodeGoneObservationForTest 删掉本副本「看到 (zone,node) 从注册表消失」的本地记录(开始和结束各删一次),
// 免得别的用例留下的观察把本用例压在 localGoneBarrierBlocks 里。
func forgetNodeGoneObservationForTest(t *testing.T, zoneID uint32, nodeID string) {
	t.Helper()
	forget := func() {
		nodeGoneObservedMu.Lock()
		delete(nodeGoneObservedAt, nodeGoneObservedKey(zoneID, nodeID))
		nodeGoneObservedMu.Unlock()
	}
	forget()
	t.Cleanup(forget)
}

// markZoneGoneFromRegistryForTest 摆出「zone 在 etcd 注册表里已没有任何场景节点」(合服后源区永久下线):
// 注册表已同步、zone 里一条注册都没有,本副本也没有这些节点刚消失的本地观察(屏障早已过)。
func markZoneGoneFromRegistryForTest(t *testing.T, zoneID uint32, nodeIDs ...string) {
	t.Helper()
	resetRegistryMirrorForTest(t)
	for _, nodeID := range nodeIDs {
		forgetNodeGoneObservationForTest(t, zoneID, nodeID)
	}
}

// unregisterKnownNodeForTest 从注册表镜像里拿掉一条注册(= 租约到期 / 主动注销)。
func unregisterKnownNodeForTest(key string) {
	knownNodesMu.Lock()
	delete(knownNodes, key)
	knownNodesMu.Unlock()
}

// seedHolderInZone1MissingLoadSet 摆出:玩家位置指向 zone 1 的 node 10 / oldScene(owner_epoch = 1),zone 1 的负载集
// **整键不存在**,也没有 death_at。zone 1 在注册表里是否还有节点由用例决定。返回 location 原文。
func seedHolderInZone1MissingLoadSet(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, playerID, oldScene uint64) string {
	t.Helper()
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, oldScene), "10")
	mr.Set(fmt.Sprintf(SceneZoneKeyFmt, oldScene), "1")
	seedSceneMembers(mr, oldScene, 5)
	raw := identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: oldScene, NodeId: "10", ZoneId: 1, OwnerEpoch: 1, UpdateTime: 100,
	})
	adoptSceneMember(mr, oldScene, playerID)
	require.False(t, mr.Exists(nodeLoadKey(1)), "前提:zone 1 的负载集整键不存在")
	return raw
}

// 玩家已连到 zone 2 的 gate、显式落到 zone 2 的场景;zone 1 的 node 10 还在注册表里,只是负载键丢了。
// 旧实现把位置当成「已下线 zone 的陈旧位置」过滤掉,不过换手门直接落点并铸出 epoch 2,node 10 之后带 1 的存盘全被拒。
func TestEnterScene_MissingLoadSetWhileZoneRegisteredKeepsHandoffGate(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6901)
		oldScene = uint64(7901)
		targetID = uint64(7902)
	)
	resetRegistryMirrorForTest(t)
	forgetNodeGoneObservationForTest(t, 1, "10")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/stale-z1-10", 1, "10")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/stale-z2-20", 2, "20")
	locRaw := seedHolderInZone1MissingLoadSet(t, sc, mr, playerID, oldScene)
	seedSceneOnNode(mr, 2, targetID, "20", "3")
	seedNodeMembers(mr, 2, "20", 8)
	withReachableSceneNode(t, sc, "20")

	logic := NewEnterSceneLogic(context.Background(), sc)
	require.False(t, logic.playerLocationOwnerGone(identLocation(t, sc, playerID), 1), "负载键丢失不是整 zone 下线的证据")

	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: 2,
		GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "持有节点还在注册表里:必须过换手门")
	assert.Equal(t, locRaw, identLocationRaw(t, sc, playerID), "location 一个字节都不改")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "不得铸造新 epoch")
	assert.Empty(t, *captured, "不得发路由")
	assert.Equal(t, "3", sceneCountText(sc, targetID))
	assert.Equal(t, "8", nodeCountText(sc, 2, "20"))
	assert.Equal(t, "5", sceneCountText(sc, oldScene))
}

// 登录不指定去向(ZoneId = 0、SceneId = 0),从 zone 2 的 gate 进来;位置记在 zone 1 的 node 10,zone 1 的负载集整键丢失。
//   - zone 1 的节点还在注册表里:位置照常有效,GO-5 跟随它的 zone,只把连接送回 zone 1(不写 location、不铸造)。
//     旧实现在这一拍里把它当陈旧位置过滤,改在 gate zone 落点并铸造 epoch 2 —— node 10 名下还没落盘的进度随之回档。
//   - zone 1 已从注册表注销(合服后源区永久下线):陈旧位置照旧被过滤,在 gate zone 落点并铸造,不会被换手门永久挡住。
func TestEnterScene_LoginOverMissingLoadSetDecidedByRegistry(t *testing.T) {
	const (
		playerID    = uint64(6911)
		oldScene    = uint64(7911)
		defaultConf = uint64(3911)
		defaultID   = uint64(7912)
	)
	cases := []struct {
		name            string
		zone1Registered bool
	}{
		{name: "源区节点仍在注册表", zone1Registered: true},
		{name: "源区已从注册表注销", zone1Registered: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			captured := capturingKafkaWriter(sc)
			resetRegistryMirrorForTest(t)
			forgetNodeGoneObservationForTest(t, 1, "10")
			registerKnownNodeForTest(t, "SceneNodeService.rpc/stale-z2-20", 2, "20")
			if tc.zone1Registered {
				registerKnownNodeForTest(t, "SceneNodeService.rpc/stale-z1-10", 1, "10")
			}
			locRaw := seedHolderInZone1MissingLoadSet(t, sc, mr, playerID, oldScene)
			// gate zone 2 开着默认大世界:位置一旦被过滤,这里就能落点(旧实现回档走的正是这条路)。
			t.Cleanup(SetWorldConfIdsForTest([]uint64{defaultConf}))
			mr.SAdd(worldChannelsKey(2, defaultConf), fmt.Sprintf("%d", defaultID))
			seedSceneOnNode(mr, 2, defaultID, "20", "0")
			seedNodeMembers(mr, 2, "20", 0)
			withReachableSceneNode(t, sc, "20")

			logic := NewEnterSceneLogic(context.Background(), sc)
			var redirectZones []uint32
			logic.assignGateForZone = func(_ context.Context, _ *svc.ServiceContext, zone uint32, _ uint64) (*scene_manager.RedirectToGateInfo, error) {
				redirectZones = append(redirectZones, zone)
				return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.1.0.8", TargetGatePort: 7001}, nil
			}
			resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
				PlayerId: playerID, GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
			})
			require.NoError(t, err)
			require.Equal(t, uint32(0), resp.ErrorCode)
			require.Len(t, *captured, 1)

			if tc.zone1Registered {
				require.NotNil(t, resp.Redirect, "位置仍然有效:跟随它的 zone 送连接")
				assert.Equal(t, []uint32{1}, redirectZones)
				assert.Equal(t, locRaw, identLocationRaw(t, sc, playerID), "只送连接,不改 location")
				assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "不得铸造新 epoch")
				assert.Equal(t, "0", sceneCountText(sc, defaultID), "不得在 gate zone 预占")
				return
			}
			assert.Nil(t, resp.Redirect)
			assert.Empty(t, redirectZones)
			loc := identLocation(t, sc, playerID)
			require.NotNil(t, loc)
			assert.Equal(t, defaultID, loc.GetSceneId())
			assert.Equal(t, uint32(2), loc.GetZoneId())
			assert.Equal(t, "20", loc.GetNodeId())
			assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "陈旧位置被过滤后按首次落点铸造")
			event := decodeRoutePlayerEvent(t, (*captured)[0])
			assert.Equal(t, uint64(2), event.GetOwnerEpoch())
			assert.Equal(t, "5", sceneCountText(sc, oldScene), "下线 zone 的旧场景计数不去碰")
		})
	}
}

// 逐条钉住 playerLocationOwnerGone 的放行条件。负载集在整个用例里只在第 5 步出现,其余时刻整键不存在 ——
// 旧实现在第 1、2 步就会放行。
func TestPlayerLocationOwnerGone_RequiresZoneGoneFromRegistry(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	logic := NewEnterSceneLogic(context.Background(), sc)
	holder := &scene_manager.PlayerLocation{SceneId: 7921, NodeId: "10", ZoneId: 1, OwnerEpoch: 1}
	awaiting := &scene_manager.PlayerLocation{ZoneId: 1, OwnerEpoch: 1}
	require.False(t, mr.Exists(nodeLoadKey(1)), "前提:zone 1 的负载集整键不存在")

	resetRegistryMirrorForTest(t)
	forgetNodeGoneObservationForTest(t, 1, "10")
	const otherKey = "SceneNodeService.rpc/gone-z1-11"

	// 1. 本副本还没完成首次全量同步:拿不到证据。
	knownNodesSynced.Store(false)
	assert.False(t, logic.playerLocationOwnerGone(holder, 1), "注册表未同步")
	assert.False(t, logic.playerLocationOwnerGone(awaiting, 1), "注册表未同步")
	knownNodesSynced.Store(true)

	// 2. zone 1 在注册表里还有别的节点(node 10 自己已不在):zone 没下线。单节点判死归 playerLocationOwnerDead。
	registerKnownNodeForTest(t, otherKey, 1, "11")
	assert.False(t, logic.playerLocationOwnerGone(holder, 1), "zone 里还有注册着的节点")
	assert.False(t, logic.playerLocationOwnerGone(awaiting, 1), "等待落点所在 zone 还有注册着的节点")

	// 3. zone 1 在注册表里一个节点都没有了:放行。有持有节点时它比单节点判死更严 —— 这里放行,OwnerDead 必放行。
	unregisterKnownNodeForTest(otherKey)
	assert.True(t, logic.playerLocationOwnerGone(holder, 1))
	assert.True(t, logic.playerLocationOwnerGone(awaiting, 1))
	assert.True(t, logic.playerLocationOwnerDead(holder, 1), "陈旧位置放行的,单节点判死也必须放行")

	// 4. 本副本刚看到 node 10 从注册表消失:屏障内不放行(不依赖 leader 写的 death_at);等待落点没有持有节点,不受影响。
	key := nodeGoneObservedKey(1, "10")
	noteNodeGoneFromRegistry(1, "10")
	assert.False(t, logic.playerLocationOwnerGone(holder, 1), "本地观察到的消失还在屏障内")
	assert.True(t, logic.playerLocationOwnerGone(awaiting, 1))
	nodeGoneObservedMu.Lock()
	nodeGoneObservedAt[key] = time.Now().Add(-2 * sceneReentryBarrier(sc))
	nodeGoneObservedMu.Unlock()
	assert.True(t, logic.playerLocationOwnerGone(holder, 1), "本地观察已过屏障")

	// 5. 注册表已注销、负载集还没摘干净(leader 先写 death_at 再摘):不抢跑。
	mr.ZAdd(nodeLoadKey(1), 0, "11")
	assert.False(t, logic.playerLocationOwnerGone(holder, 1), "负载集里还有成员")
	assert.False(t, logic.playerLocationOwnerGone(awaiting, 1), "负载集里还有成员")
	mr.ZRem(nodeLoadKey(1), "11")

	// 6. death_at 刚写入:再入屏障压着。
	require.NoError(t, markNodeDeath(sc, 1, "10"))
	assert.False(t, logic.playerLocationOwnerGone(holder, 1), "再入屏障未到")
	mr.Del(nodeDeathAtKey(1, "10"))
	assert.True(t, logic.playerLocationOwnerGone(holder, 1))
}
