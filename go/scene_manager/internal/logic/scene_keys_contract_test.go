package logic

// 玩家域与控制域的键形契约(docs/design/redis-player-store-cluster.md §5.2、§14.1)。
//
// 玩家域分片成 Redis Cluster 之后,一段 Lua 的全部 KEYS 必须同 slot,否则 Redis 回 CROSSSLOT;
// 控制域留在单实例,键里不能出现 hash tag,否则它看起来像玩家键,混进玩家域脚本时不会有任何提示。
// 开发环境连的还是单实例,跨 slot 不会报错,所以由这里提前拦住。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"proto/scene_manager"
	"shared/handofffence"
	"shared/ownerepoch"
	"shared/redisslot"
)

// 玩家域每段多键脚本的 KEYS 都落在这名玩家自己的 slot 上。KEYS 由生产代码的函数给出,脚本加键时这里跟着变。
func TestPlayerDomainScriptKeysShareThePlayerSlot(t *testing.T) {
	const playerID = uint64(1234567890123)
	want := redisslot.PlayerSlot(playerID)
	scripts := map[string][]string{
		"落点(铸造与不铸造)":                              placementKeys(playerID),
		"落点回滚 luaRollbackPlayerPlacement":         rollbackPlacementKeys(playerID),
		"离场删除 luaDeletePlayerLocationIfUnchanged": leaveDeleteKeys(playerID),
		"撤销无资格持有者 luaRevokeUnqualifiedHolder":     unqualifiedRevokeKeys(playerID),
		"位置快照 luaReadLocationSnapshot":            locationSnapshotKeys(playerID),
		"进场去重收尾 luaCompleteEnterSceneDedupe":      enterSceneDedupeCompleteKeys(playerID, "req-1"),
	}
	for name, keys := range scripts {
		require.GreaterOrEqual(t, len(keys), 2, name)
		for _, key := range keys {
			assert.True(t, redisslot.HasHashTag(key), "%s: %s 必须带玩家 tag", name, key)
			assert.Equal(t, want, redisslot.HashSlot(key), "%s: %s 不在玩家的 slot 上", name, key)
		}
	}
}

// 单键也要在玩家的 slot 上:它们会和上面的键出现在同一段脚本里,或者由同一个玩家域句柄读写。
func TestPlayerDomainSingleKeysShareThePlayerSlot(t *testing.T) {
	const playerID = uint64(1234567890123)
	want := redisslot.PlayerSlot(playerID)
	for _, key := range []string{
		getPlayerLocationKey(playerID),
		locationRevisionKey(playerID),
		locationRevisionFloorKey(playerID),
		leaveReleaseWaitKey(playerID),
		channelSwitchCooldownKey(playerID),
		redisslot.EnterSceneDedupKey(playerID, "req-1"),
		ownerepoch.OwnerEpochKey(playerID),
		ownerepoch.HandoffKey(playerID),
		handofffence.Key(playerID),
	} {
		assert.True(t, redisslot.HasHashTag(key), "%s 必须带玩家 tag", key)
		assert.Equal(t, want, redisslot.HashSlot(key), "%s 不在玩家的 slot 上", key)
	}
}

// 控制域的键一律不带 hash tag。
func TestControlDomainKeysCarryNoHashTag(t *testing.T) {
	const (
		sceneID = uint64(1234567890123)
		confID  = uint64(3300)
		zoneID  = uint32(7)
		nodeID  = "10"
	)
	for _, key := range []string{
		sceneNodeKey(sceneID),
		sceneZoneKey(sceneID),
		sceneMirrorFlagKey(sceneID),
		sceneSourceKey(sceneID),
		sceneMirrorsKey(sceneID),
		sceneAgonesGsKey(sceneID),
		sceneDrainingKey(sceneID),
		instanceMembersKey(sceneID),
		nodeMembersKey(zoneID, nodeID),
		nodeScenesKey(zoneID, nodeID),
		nodeSceneCountKey(zoneID, nodeID),
		nodeSceneNodeTypeKey(zoneID, nodeID),
		nodeLoadKey(zoneID),
		nodeDeathAtKey(zoneID, nodeID),
		nodeReconcileScenesKey(zoneID, nodeID),
		activeInstancesKey(zoneID),
		worldChannelsKey(zoneID, confID),
		worldDrainingSetKey(zoneID, confID),
		worldDesiredChannelsKey(zoneID),
		worldAutoscaleCooldownKey(zoneID, confID),
		worldChannelLineNoKey(zoneID, confID),
		worldChannelDirectoryKey(zoneID, confID),
		mysteryMerchantKey(zoneID),
		deadNodeReconcileTasksKey,
		sceneManagerZonesKey,
		fmt.Sprintf(worldInitLockKeyFmt, zoneID),
		fmt.Sprintf(gateDrainingKeyFmt, uint32(5)),
	} {
		assert.False(t, redisslot.HasHashTag(key), "控制域键 %s 不得带 hash tag", key)
	}
}

// evalKeyRecorder 记下每条 EVAL / EVALSHA 的 KEYS。钩子里只记录、不调 mr.*:Lua 里的 redis.call 会带着 miniredis 的锁重入钩子。
type evalKeyRecorder struct {
	mu   sync.Mutex
	keys [][]string
}

func installEvalKeyRecorder(t *testing.T, mr *miniredis.Miniredis) *evalKeyRecorder {
	t.Helper()
	rec := &evalKeyRecorder{}
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if (cmd != "EVAL" && cmd != "EVALSHA") || len(args) < 2 {
			return false
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 || 2+n > len(args) {
			return false
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.keys = append(rec.keys, slices.Clone(args[2:2+n]))
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	return rec
}

// requireOneDomain 断言每段脚本的 KEYS 要么全是同一 slot 的玩家键,要么全是控制域键;返回两边各有几段脚本。
func (r *evalKeyRecorder) requireOneDomain(t *testing.T) (playerScripts, controlScripts int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, keys := range r.keys {
		tagged := 0
		for _, key := range keys {
			if redisslot.HasHashTag(key) {
				tagged++
			}
		}
		switch tagged {
		case 0:
			controlScripts++
		case len(keys):
			playerScripts++
			for _, key := range keys {
				assert.Equal(t, redisslot.HashSlot(keys[0]), redisslot.HashSlot(key), "同一段脚本的玩家键必须同 slot: %v", keys)
			}
		default:
			assert.Fail(t, "玩家键和控制域键混在一段脚本里", "%v", keys)
		}
	}
	return playerScripts, controlScripts
}

// 走一遍真实的进场、重连、换图、路由失败回滚、离场:途中发出的每一段脚本都不跨域、不跨 slot。上面的静态清单只覆盖
// 列出来的脚本,这里兜住新加的脚本和条件分支里才拼出来的 KEYS。
func TestEnterAndLeaveScriptsNeverMixDomains(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{}
	sc.Config.KafkaWriteTimeoutSeconds = 1
	const (
		playerID = uint64(6701)
		sceneA   = uint64(7801)
		sceneB   = uint64(7802)
		confID   = uint64(3801)
	)
	_, err := mr.SAdd(worldChannelsKey(testZoneId, confID), strconv.FormatUint(sceneA, 10))
	require.NoError(t, err)
	seedSceneOnNode(mr, testZoneId, sceneA, "10", "0")
	seedSceneOnNode(mr, testZoneId, sceneB, "10", "0")
	withReachableSceneNode(t, sc, "10")
	rec := installEvalKeyRecorder(t, mr)

	enter := func(in *scene_manager.EnterSceneRequest) uint32 {
		t.Helper()
		in.OwnerRevision, in.PlayerId, in.ZoneId = 1, playerID, testZoneId
		in.GateZoneId, in.GateId, in.GateInstanceId = testZoneId, "1", "gate-uuid-test"
		resp, enterErr := NewEnterSceneLogic(context.Background(), sc).EnterScene(in)
		require.NoError(t, enterErr)
		return resp.ErrorCode
	}

	// 首次落点:自动选线预占(控制域)、铸造、去重收尾(玩家域)、提交成员(控制域)。
	require.Zero(t, enter(&scene_manager.EnterSceneRequest{SceneConfId: confID, RequestId: "contract-first", SessionId: 11, LoginGeneration: identGenA}))
	// 同落点重连带新身份:只写身份。
	require.Zero(t, enter(&scene_manager.EnterSceneRequest{SceneId: sceneA, SessionId: 22, LoginGeneration: identGenB}))
	// 同节点换图推路由失败:回滚脚本。
	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	failed := &scene_manager.EnterSceneRequest{SceneId: sceneB, SessionId: 22, LoginGeneration: identGenB}
	require.NotZero(t, enter(failed))
	// 同节点换图成功:不铸造的落点。
	sc.Kafka = &countingKafkaWriter{}
	require.Zero(t, enter(&scene_manager.EnterSceneRequest{SceneId: sceneB, SessionId: 22, LoginGeneration: identGenB}))
	// 离场:持有节点已写出当前代的释放标记,删 location(玩家域),再摘成员(控制域)。
	require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "1:1"))
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: playerID, SceneId: sceneB, SessionId: 22, LoginGeneration: identGenB,
	})
	require.NoError(t, err)
	require.False(t, mr.Exists(getPlayerLocationKey(playerID)), "前提:离场走到了删除")
	require.Equal(t, "0", sceneMemberCount(mr, sceneB), "前提:离场走到了摘成员")

	playerScripts, controlScripts := rec.requireOneDomain(t)
	assert.GreaterOrEqual(t, playerScripts, 5, "铸造、去重收尾、身份写、回滚、落点、删除至少各一段")
	assert.GreaterOrEqual(t, controlScripts, 3, "预占、提交、摘成员至少各一段")
}

// 把玩家域放到另一个实例上走一遍进场、重连、换图、回滚、离场:玩家键只出现在玩家域实例,成员与场景路由只出现在控制域实例。
// 生产上这两个域可以不在一处,哪条路径用错句柄,这里读到的就是空的,流程走不通。
func TestPlayerDomainAndControlDomainCanLiveOnSeparateInstances(t *testing.T) {
	sc, controlMr := newTestSvcCtxWithWorldScenes(t)
	playerMr := miniredis.RunT(t)
	sc.PlayerRedis = newTestPlayerRedis(t, playerMr)
	// 铸造走玩家域:测试替身同样换到玩家域实例上。
	sc.OwnerEpochMint = newFakeOwnerEpochMinter(redis.MustNewRedis(redis.RedisConf{Host: playerMr.Addr(), Type: "node"}))
	sc.Kafka = &countingKafkaWriter{}
	sc.Config.KafkaWriteTimeoutSeconds = 1
	const (
		playerID = uint64(6711)
		sceneA   = uint64(7811)
		sceneB   = uint64(7812)
		confID   = uint64(3811)
	)
	_, err := controlMr.SAdd(worldChannelsKey(testZoneId, confID), strconv.FormatUint(sceneA, 10))
	require.NoError(t, err)
	seedSceneOnNode(controlMr, testZoneId, sceneA, "10", "0")
	seedSceneOnNode(controlMr, testZoneId, sceneB, "10", "0")
	withReachableSceneNode(t, sc, "10")

	enter := func(in *scene_manager.EnterSceneRequest) uint32 {
		t.Helper()
		in.OwnerRevision, in.PlayerId, in.ZoneId = 1, playerID, testZoneId
		in.GateZoneId, in.GateId, in.GateInstanceId = testZoneId, "1", "gate-uuid-test"
		resp, enterErr := NewEnterSceneLogic(context.Background(), sc).EnterScene(in)
		require.NoError(t, enterErr)
		return resp.ErrorCode
	}

	require.Zero(t, enter(&scene_manager.EnterSceneRequest{SceneConfId: confID, RequestId: "separate-first", SessionId: 11, LoginGeneration: identGenA}))
	require.True(t, playerMr.Exists(getPlayerLocationKey(playerID)), "location 在玩家域")
	require.True(t, playerMr.Exists(ownerepoch.OwnerEpochKey(playerID)), "owner_epoch 在玩家域")
	require.True(t, playerMr.Exists(redisslot.EnterSceneDedupKey(playerID, "separate-first")), "进场去重在玩家域")
	require.False(t, controlMr.Exists(getPlayerLocationKey(playerID)), "玩家键不得落到控制域")
	require.False(t, controlMr.Exists(ownerepoch.OwnerEpochKey(playerID)), "玩家键不得落到控制域")
	require.Equal(t, "1", sceneMemberCount(controlMr, sceneA), "成员在控制域")
	require.Equal(t, "1", nodeMemberCount(controlMr, testZoneId, "10"), "成员在控制域")
	require.False(t, playerMr.Exists(instanceMembersKey(sceneA)), "成员不得落到玩家域")

	require.Zero(t, enter(&scene_manager.EnterSceneRequest{SceneId: sceneA, SessionId: 22, LoginGeneration: identGenB}))

	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	require.NotZero(t, enter(&scene_manager.EnterSceneRequest{SceneId: sceneB, SessionId: 22, LoginGeneration: identGenB}))
	require.Equal(t, "1", sceneMemberCount(controlMr, sceneA), "回滚之后还在旧场景")
	require.Equal(t, "0", sceneMemberCount(controlMr, sceneB))

	sc.Kafka = &countingKafkaWriter{}
	require.Zero(t, enter(&scene_manager.EnterSceneRequest{SceneId: sceneB, SessionId: 22, LoginGeneration: identGenB}))
	require.Equal(t, "0", sceneMemberCount(controlMr, sceneA), "换图摘旧场景:读 location 走玩家域、摘成员走控制域")
	require.Equal(t, "1", sceneMemberCount(controlMr, sceneB))

	require.NoError(t, playerMr.Set(ownerepoch.HandoffKey(playerID), "1:1"))
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: playerID, SceneId: sceneB, SessionId: 22, LoginGeneration: identGenB,
	})
	require.NoError(t, err)
	require.False(t, playerMr.Exists(getPlayerLocationKey(playerID)), "离场第一步在玩家域删 location")
	require.Equal(t, "0", sceneMemberCount(controlMr, sceneB), "离场第二步在控制域摘成员")
	require.Equal(t, "0", nodeMemberCount(controlMr, testZoneId, "10"))
}
