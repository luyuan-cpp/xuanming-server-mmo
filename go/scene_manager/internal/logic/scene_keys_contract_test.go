package logic

// 玩家域与控制域的键形契约(docs/design/redis-player-store-cluster.md §5.2、§14.1;
// docs/design/redis-cluster-all-domains-20261110.md §3)。
//
// 两个域都上 Redis Cluster,一段 Lua 的全部 KEYS 必须同 slot,否则 Redis 回 CROSSSLOT:
//   - 玩家域的键带玩家的 tag(十进制 player_id 或 slot-tag),一段脚本只碰一名玩家;
//   - 控制域的键带 zone 的控制集群 tag {ctl:<z>}(登记过的族前缀),一段脚本只碰一个 zone。
// 两个域靠 tag 的形状区分(redisslot.IsControlTag)。开发环境连的还是单实例,跨 slot 不会报错,所以由这里提前拦住;
// 公用夹具装的 installSameTagGuard 让包里每个跑过 Lua 的用例都核一遍。

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		ownerepoch.HolderLeaseKey(playerID),
		handofffence.Key(playerID),
	} {
		assert.True(t, redisslot.HasHashTag(key), "%s 必须带玩家 tag", key)
		assert.Equal(t, want, redisslot.HashSlot(key), "%s 不在玩家的 slot 上", key)
	}
}

// controlDomainKeysForTest 列出 scene_manager 控制域的全部键族(以 zone 为键的与以场景号为键的),每族一把。
// 加键族时这里跟着加:TestControlDomainKeysCarryTheZoneTag 据此核对形状。
// gate 标记(shared/gatepick)不在这里:它们是 gm 族,第 4 批(2026-11-12)已改成 gate:{gm:<z>}:<id>:…(设计稿 §4.4);
// 形状、登记过的族与同槽钉在 shared/gatepick/marks_test.go,本包读标记按目标 zone 拼键钉在 gate_redirect_draining_test.go。
func controlDomainKeysForTest(zoneID uint32, sceneID, confID uint64, nodeID string) []string {
	return []string{
		sceneNodeKey(zoneID, sceneID),
		sceneMirrorFlagKey(zoneID, sceneID),
		sceneSourceKey(zoneID, sceneID),
		sceneMirrorsKey(zoneID, sceneID),
		sceneAgonesGsKey(zoneID, sceneID),
		sceneAgonesTransferReceiptKey(zoneID, sceneID, "token-1"),
		sceneReassignReceiptKey(zoneID, sceneID, "token-1"),
		scenePlacementGenKey(zoneID, sceneID),
		sceneDrainingKey(zoneID, sceneID),
		sceneTeardownJobKey(zoneID, sceneID),
		sceneRegistrationRecordKey(zoneID, sceneID),
		instanceMembersKey(zoneID, sceneID),
		sceneTeardownPendingKey(zoneID),
		agonesRoomHoldsKey(zoneID),
		nodeMembersKey(zoneID, nodeID),
		nodeScenesKey(zoneID, nodeID),
		nodeSceneCountKey(zoneID, nodeID),
		nodeSceneNodeTypeKey(zoneID, nodeID),
		nodeLoadKey(zoneID),
		nodeDeathAtKey(zoneID, nodeID),
		nodeReconcileScenesKey(zoneID, nodeID),
		nodeReconcileInstanceKey(zoneID, nodeID),
		nodeHolderKey(zoneID, nodeID),
		nodeHolderEpochKey(zoneID, nodeID),
		etcdRevisionSeenKey(zoneID),
		etcdEpochKey(zoneID),
		activeInstancesKey(zoneID),
		worldChannelsKey(zoneID, confID),
		worldDrainingSetKey(zoneID, confID),
		worldDesiredChannelsKey(zoneID),
		worldAutoscaleCooldownKey(zoneID, confID),
		worldChannelLineNoKey(zoneID, confID),
		worldChannelDirectoryKey(zoneID, confID),
		worldPlacementSeqKey(zoneID),
		worldInitLockKey(zoneID),
		mysteryMerchantKey(zoneID),
		deadNodeReconcileTasksKey(zoneID),
		LeaderLockKey(zoneID),
	}
}

// 控制域的键一律带本 zone 的控制集群 tag {ctl:<z>}(docs/design/redis-cluster-all-domains-20261110.md §3):tag 必须是登记过的族,
// 不得是玩家域的形状;同一个 zone 的键同槽,不同 zone 的键是不同的键。
func TestControlDomainKeysCarryTheZoneTag(t *testing.T) {
	const (
		sceneID = uint64(1234567890123)
		confID  = uint64(3300)
		zoneID  = uint32(7)
		nodeID  = "10"
	)
	want := sceneControlTag(zoneID)
	require.Equal(t, "{ctl:7}", want)
	for _, key := range controlDomainKeysForTest(zoneID, sceneID, confID, nodeID) {
		tag, ok := redisslot.HashTag(key)
		require.True(t, ok, "控制域键 %s 必须带 hash tag", key)
		assert.True(t, redisslot.IsControlTag(tag), "控制域键 %s 的 tag 必须是控制集群登记过的族", key)
		assert.Equal(t, want, "{"+tag+"}", "控制域键 %s 必须带本 zone 的 tag", key)
		assert.Equal(t, redisslot.HashSlot(want), redisslot.HashSlot(key), "控制域键 %s 必须与本 zone 的其余键同槽", key)
	}
	other := controlDomainKeysForTest(zoneID+1, sceneID, confID, nodeID)
	for i, key := range controlDomainKeysForTest(zoneID, sceneID, confID, nodeID) {
		assert.NotEqual(t, key, other[i], "不同 zone 必须是不同的键")
	}
}

// 跨语言键的金样:C++ scene 节点按同一形状拼键来读(scene_ownership_rules.h 的 SceneNodeKey / NodeHolderKey,
// PlayerSceneSystem::WorldChannelDirectoryKey,PlayerMysteryMerchantSystem::RecordKey)。C++ 侧的
// SceneOwnershipRules.KeysMatchTheSceneManagerKeys 与 PlayerRedisRouteContract.ControlDomainStaysOffThePlayerHandle 钉同样的串;
// 改这里必须同批改那里。
func TestCrossLanguageControlKeyGoldens(t *testing.T) {
	assert.Equal(t, "scene:{ctl:3}:0:node", sceneNodeKey(3, 0))
	assert.Equal(t, "scene:{ctl:3}:18446744073709551615:node", sceneNodeKey(3, 18446744073709551615))
	assert.Equal(t, "node:{ctl:3}:17:holder", nodeHolderKey(3, "17"))
	assert.Equal(t, "world_channel_directory:{ctl:1}:2", worldChannelDirectoryKey(1, 2))
	assert.Equal(t, "mystery_merchant:{ctl:1}", mysteryMerchantKey(1))
}

// 每段控制域脚本的全部 KEYS 同一个 tag(docs/design/redis-cluster-all-domains-20261110.md §3.1「多键命令必须同 tag」)。
// 这里列的是带可选键、分支最多的那几组,KEYS 由生产代码的函数拼:认领(带期望属主、摘集合、死节点收尾任务表)、登记
// (带镜像源的子集合)、成员。别的脚本由 installSameTagGuard 在跑过它们的用例里逐条核。
func TestControlScriptKeysShareOneTag(t *testing.T) {
	const (
		zoneID  = uint32(7)
		sceneID = uint64(1234567890123)
		source  = uint64(1234567890124)
		confID  = uint64(3300)
		nodeID  = "10"
	)
	claim := sceneTeardownRequest{ZoneID: zoneID, SceneID: sceneID, ExpectedOwner: nodeID,
		UnlistKeys:   []string{worldChannelsKey(zoneID, confID), worldDrainingSetKey(zoneID, confID)},
		DeadNodeTask: &deadNodeTaskFence{tasksKey: deadNodeReconcileTasksKey(zoneID), member: "7/10", queuedAtMs: 1}}
	instanceKeys, _ := instanceRegistrationParams(instanceRegistration{
		sceneRegistration: sceneRegistration{SceneID: sceneID, ZoneID: zoneID, NodeID: nodeID},
		Mirror:            true,
		SourceSceneID:     source,
	})
	worldReg := sceneRegistration{SceneID: sceneID, ZoneID: zoneID, NodeID: nodeID}
	scripts := map[string][]string{
		"销毁认领(期望属主 + 摘集合 + 收尾任务表)": claim.keys(),
		"销毁认领(只有场景键)":              sceneTeardownRequest{ZoneID: zoneID, SceneID: sceneID}.keys(),
		"实例登记(带镜像源)":               instanceKeys,
		"世界频道登记":                   worldChannelRegistrationKeys(worldReg, confID),
		"增减成员":                     sceneMemberKeys(sceneID, zoneID, nodeID),
	}
	want := sceneControlTag(zoneID)
	for name, keys := range scripts {
		require.GreaterOrEqual(t, len(keys), 2, name)
		for _, key := range keys {
			tag, ok := redisslot.HashTag(key)
			require.True(t, ok, "%s: %s 没有 tag", name, key)
			assert.Equal(t, want, "{"+tag+"}", "%s: %s 不在本 zone 的 tag 下", name, key)
		}
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

// requireOneDomain 断言每段脚本的 KEYS 要么全是同一 slot 的玩家键,要么全是同一个控制集群 tag 的控制域键;返回两边各有几段
// 脚本。按 tag 的形状分域(redisslot.IsControlTag):控制集群的 tag 是登记过的族前缀,玩家域是 player_id 或 slot-tag。
func (r *evalKeyRecorder) requireOneDomain(t *testing.T) (playerScripts, controlScripts int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, keys := range r.keys {
		switch scriptDomain(t, keys) {
		case scriptDomainControl:
			controlScripts++
		case scriptDomainPlayer:
			playerScripts++
		}
	}
	return playerScripts, controlScripts
}

// scriptDomainKind 是一段脚本的 KEYS 所属的域。
type scriptDomainKind int

const (
	scriptDomainNone    scriptDomainKind = iota // 不带键的脚本,或已报错
	scriptDomainControl                         // 全部是控制集群的键
	scriptDomainPlayer                          // 全部是玩家域的键
)

// scriptDomain 判一段脚本的 KEYS 属于哪个域,并断言:每把键都带 tag;不混域;控制域同一个 tag,玩家域同 slot。
func scriptDomain(t *testing.T, keys []string) scriptDomainKind {
	t.Helper()
	if len(keys) == 0 {
		return scriptDomainNone
	}
	control, player := 0, 0
	for _, key := range keys {
		tag, ok := redisslot.HashTag(key)
		switch {
		case !ok:
			assert.Fail(t, "脚本里有不带 hash tag 的键", "%s in %v", key, keys)
			return scriptDomainNone
		case redisslot.IsControlTag(tag):
			control++
		default:
			player++
		}
	}
	switch {
	case control == len(keys):
		first, _ := redisslot.HashTag(keys[0])
		for _, key := range keys {
			tag, _ := redisslot.HashTag(key)
			assert.Equal(t, first, tag, "同一段脚本的控制域键必须同一个 tag: %v", keys)
		}
		return scriptDomainControl
	case player == len(keys):
		for _, key := range keys {
			assert.Equal(t, redisslot.HashSlot(keys[0]), redisslot.HashSlot(key), "同一段脚本的玩家键必须同 slot: %v", keys)
		}
		return scriptDomainPlayer
	default:
		assert.Fail(t, "玩家键和控制域键混在一段脚本里", "%v", keys)
		return scriptDomainNone
	}
}

// installSameTagGuard 装在公用夹具的 miniredis 上(newTestSvcCtx、newTestSvcCtxWithWorldScenes、
// newIntegrationSvcCtxWithRedisOptions):记下每条 EVAL / EVALSHA 的 KEYS,用例结束时逐段核 scriptDomain。包里每个跑过 Lua
// 的用例都因此核一遍「每段控制域脚本的全部 KEYS 同 tag」——单实例上跨槽不报错,上了 Cluster 才会 CROSSSLOT。
// 用例自己装 SetPreHook(或调 mr.SetError,共用同一个钩子槽位)之后本守卫让位,之后的命令不再记录,不影响用例本身。
func installSameTagGuard(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()
	rec := installEvalKeyRecorder(t, mr)
	t.Cleanup(func() {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		for _, keys := range rec.keys {
			scriptDomain(t, keys)
		}
	})
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
	sc.OwnerEpochMint = newFakeOwnerEpochMinter(sc.PlayerRedis)
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
	require.False(t, playerMr.Exists(instanceMembersKey(testZoneId, sceneA)), "成员不得落到玩家域")

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
