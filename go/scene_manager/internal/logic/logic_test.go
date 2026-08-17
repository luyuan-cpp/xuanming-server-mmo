package logic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/config"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/snowflake"
)

// registerTypedNode seeds a scene node with both a load-set entry and a
// mirrored scene_node_type so purpose-based selection works in tests.
// Tests that don't care about type should keep using mr.ZAdd directly;
// unclassified nodes are allowed for any purpose by design.
func registerTypedNode(mr *miniredis.Miniredis, zoneId uint32, nodeId string, sceneNodeType uint32, loadScore float64) {
	mr.ZAdd(nodeLoadKey(zoneId), loadScore, nodeId)
	mr.Set(nodeSceneNodeTypeKey(zoneId, nodeId), fmt.Sprintf("%d", sceneNodeType))
}

// nowUnix returns the current Unix timestamp for test helpers that need to
// backdate sorted-set scores to drive the idle-instance cleanup pass.
func nowUnix() int64 { return time.Now().Unix() }

// testZoneId is the zone ID used across all tests.
const testZoneId uint32 = 1

// testLoadKey returns the Redis load-set key for the test zone.
func testLoadKey() string {
	return nodeLoadKey(testZoneId)
}

// ---------------------------------------------------------------------------
// Test helper
// ---------------------------------------------------------------------------

func newTestSvcCtx(t *testing.T, nodeID string) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)

	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	t.Cleanup(func() {
		// go-zero Redis doesn't expose Close, but miniredis shuts down on test end
	})

	c := config.Config{}
	c.NodeID = nodeID

	return &svc.ServiceContext{
		Config:     c,
		Redis:      rds,
		SceneIDGen: snowflake.NewNode(0),
	}, mr
}

type countingKafkaWriter struct {
	mu       sync.Mutex
	messages int
	err      error
	onWrite  func([]kafka.Message)
}

func (w *countingKafkaWriter) WriteMessages(_ context.Context, messages ...kafka.Message) error {
	w.mu.Lock()
	w.messages += len(messages)
	hook := w.onWrite
	err := w.err
	w.mu.Unlock()
	if hook != nil {
		hook(messages)
	}
	return err
}

func (w *countingKafkaWriter) Close() error { return nil }

func (w *countingKafkaWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.messages
}

// ---------------------------------------------------------------------------
// PlayerLocation tests
// ---------------------------------------------------------------------------

func TestUpdateAndGetPlayerLocation(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	ctx := context.Background()

	err := UpdatePlayerLocation(ctx, sc, 1001, 42, "scene-node-1", testZoneId)
	require.NoError(t, err)

	loc, err := GetPlayerLocation(ctx, sc, 1001)
	require.NoError(t, err)
	require.NotNil(t, loc)

	assert.Equal(t, uint64(42), loc.SceneId)
	assert.Equal(t, "scene-node-1", loc.NodeId)
	assert.True(t, loc.UpdateTime > 0)
	assert.Equal(t, testZoneId, loc.ZoneId)
}

func TestGetPlayerLocation_NotFound(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	ctx := context.Background()

	loc, err := GetPlayerLocation(ctx, sc, 9999)
	// go-zero Redis.Get returns redis.Nil as error for missing keys
	// The function may return error or nil depending on error handling
	if err != nil {
		// Acceptable: returns error for missing key
		return
	}
	assert.Nil(t, loc)
}

func TestDeletePlayerLocation(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	ctx := context.Background()

	// Create
	err := UpdatePlayerLocation(ctx, sc, 2001, 100, "node-a", testZoneId)
	require.NoError(t, err)

	// Verify exists
	loc, err := GetPlayerLocation(ctx, sc, 2001)
	require.NoError(t, err)
	require.NotNil(t, loc)

	// Delete
	err = DeletePlayerLocation(ctx, sc, 2001)
	require.NoError(t, err)

	// Verify gone
	loc, err = GetPlayerLocation(ctx, sc, 2001)
	if err == nil {
		assert.Nil(t, loc)
	}
}

func TestUpdatePlayerLocation_Overwrite(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	ctx := context.Background()

	// First location
	require.NoError(t, UpdatePlayerLocation(ctx, sc, 3001, 10, "node-a", testZoneId))

	// Overwrite with new location (player changed scene)
	require.NoError(t, UpdatePlayerLocation(ctx, sc, 3001, 20, "node-b", testZoneId))

	loc, err := GetPlayerLocation(ctx, sc, 3001)
	require.NoError(t, err)
	assert.Equal(t, uint64(20), loc.SceneId)
	assert.Equal(t, "node-b", loc.NodeId)
}

// ---------------------------------------------------------------------------
// Load balancing tests
// ---------------------------------------------------------------------------

func TestGetBestNode_SelectsLowestLoad(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-self")
	ctx := context.Background()

	// Register 3 nodes with different loads (numeric IDs)
	mr.ZAdd(testLoadKey(), 50, "3")
	mr.ZAdd(testLoadKey(), 5, "1")
	mr.ZAdd(testLoadKey(), 20, "2")

	best, err := GetBestNode(ctx, sc, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, "1", best)
}

func TestGetBestNode_NoNodes_ReturnsError(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-fallback")
	ctx := context.Background()

	// No nodes registered in the ZSet
	_, err := GetBestNode(ctx, sc, testZoneId)
	assert.Error(t, err)
}

func TestGetBestNode_TiedLoad(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-self")
	ctx := context.Background()

	// All nodes have same load (numeric IDs)
	mr.ZAdd(testLoadKey(), 10, "10")
	mr.ZAdd(testLoadKey(), 10, "20")
	mr.ZAdd(testLoadKey(), 10, "30")

	best, err := GetBestNode(ctx, sc, testZoneId)
	require.NoError(t, err)
	// Should return one of the tied nodes (Redis ZRANGE returns lexicographic order on tie)
	assert.Contains(t, []string{"10", "20", "30"}, best)
}

func TestGetBestNode_SingleNode(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-self")
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 99, "42")

	best, err := GetBestNode(ctx, sc, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, "42", best)
}

// ---------------------------------------------------------------------------
// Key format test
// ---------------------------------------------------------------------------

func TestPlayerLocationKeyFormat(t *testing.T) {
	key := getPlayerLocationKey(12345)
	assert.Equal(t, "player:12345:location", key)
}

// ---------------------------------------------------------------------------
// Multiple players coexist
// ---------------------------------------------------------------------------

func TestMultiplePlayersLocation(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	ctx := context.Background()

	// Place multiple players
	require.NoError(t, UpdatePlayerLocation(ctx, sc, 100, 1, "node-a", testZoneId))
	require.NoError(t, UpdatePlayerLocation(ctx, sc, 200, 2, "node-b", testZoneId))
	require.NoError(t, UpdatePlayerLocation(ctx, sc, 300, 1, "node-a", testZoneId))

	// Each player has their own location
	loc1, _ := GetPlayerLocation(ctx, sc, 100)
	loc2, _ := GetPlayerLocation(ctx, sc, 200)
	loc3, _ := GetPlayerLocation(ctx, sc, 300)

	assert.Equal(t, uint64(1), loc1.SceneId)
	assert.Equal(t, "node-a", loc1.NodeId)

	assert.Equal(t, uint64(2), loc2.SceneId)
	assert.Equal(t, "node-b", loc2.NodeId)

	assert.Equal(t, uint64(1), loc3.SceneId)
	assert.Equal(t, "node-a", loc3.NodeId)

	// Delete one, others unaffected
	require.NoError(t, DeletePlayerLocation(ctx, sc, 200))
	loc1After, _ := GetPlayerLocation(ctx, sc, 100)
	assert.NotNil(t, loc1After)
}

func TestNodeRedisStateIsScopedByZoneAndNodeID(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "manager")
	zone1 := nodeEntry{nodeID: "10"}
	zone1.reg.ZoneId = 1
	zone1.reg.SceneNodeType = constants.SceneNodeTypeMainWorld
	zone2 := nodeEntry{nodeID: "10"}
	zone2.reg.ZoneId = 2
	zone2.reg.SceneNodeType = constants.SceneNodeTypeInstance

	require.NoError(t, sc.Redis.Set(nodeSceneCountKey(1, "10"), "3"))
	require.NoError(t, sc.Redis.Set(nodePlayerCountKey(1, "10"), "30"))
	require.NoError(t, sc.Redis.Set(nodeSceneCountKey(2, "10"), "7"))
	require.NoError(t, sc.Redis.Set(nodePlayerCountKey(2, "10"), "70"))
	updateNodeLoad(sc, zone1)
	updateNodeLoad(sc, zone2)

	type1, ok1 := readNodeSceneType(sc, 1, "10")
	type2, ok2 := readNodeSceneType(sc, 2, "10")
	require.True(t, ok1)
	require.True(t, ok2)
	assert.Equal(t, constants.SceneNodeTypeMainWorld, type1)
	assert.Equal(t, constants.SceneNodeTypeInstance, type2)
	assert.EqualValues(t, 3, readInt64(sc, nodeSceneCountKey(1, "10")))
	assert.EqualValues(t, 7, readInt64(sc, nodeSceneCountKey(2, "10")))
	assert.NotEqual(t, nodeScenesKey(1, "10"), nodeScenesKey(2, "10"))
}

func TestNodeSelectionRejectsDuplicateZoneNodeIdentity(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "manager")
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)

	first := nodeEntry{nodeID: "10"}
	first.reg.ZoneId = 1
	first.reg.SceneNodeType = constants.SceneNodeTypeInstance
	second := first
	second.reg.GrpcEndpoint.IP = "10.0.0.2"

	knownNodesMu.Lock()
	knownNodes["SceneNodeService.rpc/first"] = first
	knownNodes["SceneNodeService.rpc/second"] = second
	knownNodesMu.Unlock()
	mr.ZAdd(nodeLoadKey(1), 0, "10")
	mr.Set(nodeSceneNodeTypeKey(1, "10"), fmt.Sprintf("%d", constants.SceneNodeTypeInstance))

	_, err := GetBestNodeForPurpose(context.Background(), sc, 1, constants.NodePurposeInstance)
	require.Error(t, err)
	assert.False(t, IsNodeAlive(sc, 1, "10"), "重复注册不能被 Kafka 路由视为存活目标")
}

// IsNodeAlive 的三态契约:只有「成员确实不在负载集」才算已死,Redis 本身不可用
// 必须按存活处理。这两个用例是成对的,少任何一个都挡不住回归:
//
//   - 只钉「故障按存活」→ 有人可以把函数改成恒 true 让它通过,判死能力全失;
//   - 只钉「缺成员判死」→ 就是修复前的 `return err == nil`,Redis 抖动即误判死。
//
// 误判死的代价不是「查不到」:它会驱动 resolveScene 改写 scene:{id}:node 并让新
// 节点 CreateScene,而 C++ 老节点丢租约后还有 15s emergency relocate drain 仍在
// SavePlayerToRedis —— 同一玩家双写 / 回档。详见
// docs/design/scene-owner-reentry-barrier.md。
func TestIsNodeAliveTreatsRedisOutageAsAlive(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)

	sc, mr := newTestSvcCtx(t, "1")
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)
	require.True(t, IsNodeAlive(sc, testZoneId, "10"), "前置条件:节点在负载集里应判存活")

	// 模拟 Redis 不可用(连接被拒),而不是「成员不存在」。
	mr.Close()

	assert.True(t, IsNodeAlive(sc, testZoneId, "10"),
		"Redis 不可用时状态未知,必须按存活处理,绝不能触发场景改派(修复前此处返回 false)")
}

func TestIsNodeAliveReportsDeadWhenMemberMissing(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)

	sc, mr := newTestSvcCtx(t, "1")
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)
	require.True(t, IsNodeAlive(sc, testZoneId, "10"))

	// Redis 健在,但节点已从负载集摘除 —— 这是唯一可以断言「已死」的证据。
	mr.ZRem(nodeLoadKey(testZoneId), "10")

	assert.False(t, IsNodeAlive(sc, testZoneId, "10"),
		"Redis 可用且成员不在负载集时必须判死,否则死节点永远不会被自愈")
}

func TestFindNodeByPodIPRejectsDuplicateRegistration(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)

	entry := nodeEntry{nodeID: "10"}
	entry.reg.ZoneId = 1
	entry.reg.SceneNodeType = constants.SceneNodeTypeInstance
	entry.reg.GrpcEndpoint.IP = "10.0.0.10"
	knownNodesMu.Lock()
	knownNodes["SceneNodeService.rpc/first"] = entry
	knownNodes["SceneNodeService.rpc/second"] = entry
	knownNodesMu.Unlock()

	_, _, _, ok := FindNodeByPodIP("10.0.0.10")
	assert.False(t, ok, "Agones PodIP 反查不得吞掉重复 (zone,node) 注册")
}

func TestEnterSceneDuplicateMappedIdentityFailsClosedWithoutReassignment(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)

	first := nodeEntry{nodeID: "10"}
	first.reg.ZoneId = testZoneId
	first.reg.SceneNodeType = constants.SceneNodeTypeMainWorld
	second := first
	second.reg.GrpcEndpoint.IP = "10.0.0.2"
	knownNodesMu.Lock()
	knownNodes["SceneNodeService.rpc/first"] = first
	knownNodes["SceneNodeService.rpc/second"] = second
	knownNodesMu.Unlock()

	const sceneID = uint64(7710)
	mr.Set(sceneNodeKey(sceneID), "10")
	mr.Set(sceneZoneKey(sceneID), fmt.Sprintf("%d", testZoneId))
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 1)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: 7710, SceneId: sceneID, ZoneId: testZoneId,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
	mapped, getErr := sc.Redis.Get(sceneNodeKey(sceneID))
	require.NoError(t, getErr)
	assert.Equal(t, "10", mapped, "重复身份不能被当作 dead node 并静默改写 scene ownership")
}

// ---------------------------------------------------------------------------
// World scene helpers
// ---------------------------------------------------------------------------

func newTestSvcCtxWithWorldScenes(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})

	c := config.Config{}
	c.WorldChannelCount = 1
	c.InstanceIdleTimeoutSeconds = 300
	c.InstanceCheckIntervalSeconds = 10

	// CreateScene 在 C++ CreateScene RPC 失败时会回滚(不再留 phantom scene),
	// 所以本包里几乎所有"创建一个场景再断言点什么"的用例都需要一个能应答的
	// 节点。默认装上,个别要验证 RPC 失败的用例再自己覆盖 createShouldFail。
	installDefaultFakeSceneNode(t)

	return &svc.ServiceContext{
		Config:     c,
		Redis:      rds,
		SceneIDGen: snowflake.NewNode(0),
	}, mr
}

func TestIsWorldConf(t *testing.T) {
	// IsWorldConf reads from the World table, which is empty in unit tests.
	assert.False(t, IsWorldConf(1001))
	assert.False(t, IsWorldConf(0))
}

func TestGetBestWorldChannel_NotFound(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	id, nodeId, _ := GetBestWorldChannel(ctx, sc, 9999, testZoneId)
	assert.Equal(t, uint64(0), id)
	assert.Equal(t, "", nodeId)
}

func TestAssignNodeByHash_Deterministic(t *testing.T) {
	nodes := []string{"1", "2", "3", "4"}

	result1 := assignNodeByHash(1001, nodes)
	result2 := assignNodeByHash(1001, nodes)
	assert.Equal(t, result1, result2, "Same confId should always map to same node")

	// Different confIds may map to different nodes (not guaranteed, but likely with 4 nodes)
	results := make(map[string]bool)
	for confId := uint64(1); confId <= 100; confId++ {
		results[assignNodeByHash(confId, nodes)] = true
	}
	assert.Greater(t, len(results), 1, "100 different confIds should map to more than 1 node")
}

// ---------------------------------------------------------------------------
// Instance lifecycle tests
// ---------------------------------------------------------------------------

func TestInstancePlayerCount_IncrDecr(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	sceneId := uint64(42)
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "0")

	IncrInstancePlayerCount(sc, testZoneId, sceneId)
	IncrInstancePlayerCount(sc, testZoneId, sceneId)

	val, err := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	require.NoError(t, err)
	assert.Equal(t, "2", val)

	DecrInstancePlayerCount(sc, testZoneId, sceneId)
	val, _ = sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	assert.Equal(t, "1", val)
}

func TestCreateScene_MainWorld_Idempotent(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	// Register a node so GetBestNode works.
	mr.ZAdd(testLoadKey(), 0, "10")

	logic := NewCreateSceneLogic(ctx, sc)

	// First create — triggers on-demand channel init.
	resp1, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 1001,
		ZoneId:      testZoneId,
		SceneType:   1,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp1.ErrorCode)
	assert.NotEqual(t, uint64(0), resp1.SceneId)

	// Second create for same confId — should return same scene (1 channel).
	resp2, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 1001,
		ZoneId:      testZoneId,
		SceneType:   1,
	})
	require.NoError(t, err)
	assert.Equal(t, resp1.SceneId, resp2.SceneId, "single-channel main scene should be idempotent")
	assert.Equal(t, resp1.NodeId, resp2.NodeId)
}

func TestCreateScene_Instance_UniquIds(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")
	// CreateScene 现在会在 C++ CreateScene RPC 失败时回滚(不再留 phantom
	// scene),所以想断言"创建成功"的用例必须给一个真的能应答的节点。
	withReachableSceneNode(t, sc, "10")

	logic := NewCreateSceneLogic(ctx, sc)

	resp1, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp1.ErrorCode)

	resp2, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp2.ErrorCode)
	assert.NotEqual(t, resp1.SceneId, resp2.SceneId, "instances should get unique IDs")
}

func TestCreateScene_Instance_TrackedInActiveSet(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, _ := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
	})

	// Should be in the active instances sorted set.
	instKey := activeInstancesKey(testZoneId)
	members, err := sc.Redis.ZrangeWithScores(instKey, 0, -1)
	require.NoError(t, err)
	assert.Len(t, members, 1)
	assert.Equal(t, fmt.Sprintf("%d", resp.SceneId), members[0].Key)
}

// ---------------------------------------------------------------------------
// EnterScene / LeaveScene player-count tests
// ---------------------------------------------------------------------------

func TestEnterScene_IncrementsPlayerCount(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	// Setup: create an instance scene so there's a scene->node mapping.
	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")
	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{SceneConfId: 2001, ZoneId: testZoneId})
	require.NoError(t, err)
	sceneId := resp.SceneId

	// Player count should start at 0.
	countKey := fmt.Sprintf(InstancePlayerCountKey, sceneId)
	val, _ := sc.Redis.Get(countKey)
	assert.Equal(t, "0", val)

	// EnterScene should increment.
	enterLogic := NewEnterSceneLogic(ctx, sc)
	enterResp, err := enterLogic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: 1001,
		SceneId:  sceneId,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), enterResp.ErrorCode)

	val, _ = sc.Redis.Get(countKey)
	assert.Equal(t, "1", val)
}

func TestEnterScene_CrossNodeRejectedWithoutSideEffectsAndRetryStaysRejected(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	writer := &countingKafkaWriter{}
	sc.Kafka = writer
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const (
		playerID   = uint64(5101)
		oldSceneID = uint64(1101)
		targetID   = uint64(2201)
		confID     = uint64(3301)
	)
	// 自动选频道会先预占目标人数，门禁拒绝后必须成对回滚。
	mr.SAdd(worldChannelsKey(testZoneId, confID), fmt.Sprintf("%d", targetID))
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "20")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "4")
	mr.Set(nodePlayerCountKey(testZoneId, "20"), "4")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")

	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, oldSceneID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, oldSceneID), "7")
	require.NoError(t, UpdatePlayerLocation(ctx, sc, playerID, oldSceneID, "10", testZoneId))

	fake := withReachableSceneNode(t, sc, "10", "20")
	releasesBefore := fake.releaseCalls.Load()
	request := &scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneConfId: confID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "cross-node-retry",
	}

	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := NewEnterSceneLogic(ctx, sc).EnterScene(request)
		require.NoError(t, err)
		assert.Equal(t, constants.ErrUnsafeCrossNodeHandoff, resp.ErrorCode, "attempt %d", attempt)

		targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
		nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "20"))
		oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldSceneID))
		assert.Equal(t, "4", targetCount, "拒绝后必须回滚目标场景预占")
		assert.Equal(t, "4", nodeCount, "拒绝后必须回滚目标节点预占")
		assert.Equal(t, "7", oldCount, "拒绝时不得扣减旧场景人数")

		loc, locErr := GetPlayerLocation(ctx, sc, playerID)
		require.NoError(t, locErr)
		require.NotNil(t, loc)
		assert.Equal(t, oldSceneID, loc.SceneId)
		assert.Equal(t, "10", loc.NodeId)
		assert.Equal(t, 0, writer.count(), "拒绝时不得发送 Gate 路由")
		assert.Equal(t, releasesBefore, fake.releaseCalls.Load(), "拒绝时不得调用旧节点 ReleasePlayer")

		exists, existsErr := sc.Redis.Exists(fmt.Sprintf("enter_scene:dedup:%d:cross-node-retry", playerID))
		require.NoError(t, existsErr)
		assert.False(t, exists, "失败终态必须释放 request_id 占位，重试不能伪成功")
	}
}

func TestEnterScene_ExistingLocationCrossZoneRejectedAndReservationRolledBack(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	writer := &countingKafkaWriter{}
	sc.Kafka = writer

	const (
		playerID = uint64(5102)
		oldScene = uint64(1102)
		targetID = uint64(2202)
		confID   = uint64(3302)
	)
	mr.SAdd(worldChannelsKey(2, confID), fmt.Sprintf("%d", targetID))
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "3")
	mr.Set(nodePlayerCountKey(2, "10"), "8")
	mr.ZAdd(nodeLoadKey(2), 0, "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, oldScene), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, oldScene), "5")
	require.NoError(t, UpdatePlayerLocation(ctx, sc, playerID, oldScene, "10", 1))

	resp, err := NewEnterSceneLogic(ctx, sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneConfId: confID, ZoneId: 2,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrUnsafeCrossNodeHandoff, resp.ErrorCode)

	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(2, "10"))
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "3", targetCount)
	assert.Equal(t, "8", nodeCount)
	assert.Equal(t, "5", oldCount)
	assert.Equal(t, 0, writer.count())
	loc, locErr := GetPlayerLocation(ctx, sc, playerID)
	require.NoError(t, locErr)
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Equal(t, uint32(1), loc.ZoneId)
}

func TestEnterScene_ZoneScopedNodeIDCollisionStillRejected(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	writer := &countingKafkaWriter{}
	sc.Kafka = writer

	const (
		playerID = uint64(5106)
		oldScene = uint64(1106)
		targetID = uint64(2206)
	)
	// node_id=10 在两个 zone 各自合法存在；只比 node 字符串会把它们误判成
	// 同一物理节点。玩家已经连到目标区 Gate，因此 crossZoneRedirect=false，
	// 这正是旧门禁会漏掉的路径。
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "3")
	mr.Set(nodePlayerCountKey(2, "10"), "8")
	mr.ZAdd(nodeLoadKey(2), 0, "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, oldScene), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, oldScene), "5")
	require.NoError(t, UpdatePlayerLocation(ctx, sc, playerID, oldScene, "10", 1))

	resp, err := NewEnterSceneLogic(ctx, sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: 2,
		GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrUnsafeCrossNodeHandoff, resp.ErrorCode)
	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "3", targetCount)
	assert.Equal(t, "5", oldCount)
	assert.Equal(t, 0, writer.count())
	loc, locErr := GetPlayerLocation(ctx, sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Equal(t, uint32(1), loc.ZoneId)
}

func TestEnterScene_SameNodeSwitchStillSucceeds(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	writer := &countingKafkaWriter{}
	sc.Kafka = writer
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const (
		playerID = uint64(5103)
		oldScene = uint64(1103)
		targetID = uint64(2203)
	)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, oldScene), "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, oldScene), "1")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "0")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "1")
	require.NoError(t, UpdatePlayerLocation(ctx, sc, playerID, oldScene, "10", testZoneId))

	fake := withReachableSceneNode(t, sc, "10")
	releasesBefore := fake.releaseCalls.Load()
	resp, err := NewEnterSceneLogic(ctx, sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	loc, locErr := GetPlayerLocation(ctx, sc, playerID)
	require.NoError(t, locErr)
	assert.Equal(t, targetID, loc.SceneId)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, 1, writer.count())
	assert.Equal(t, releasesBefore, fake.releaseCalls.Load(), "同节点切换不得调用 ReleasePlayer")
}

func TestEnterScene_FirstLandingStillSucceeds(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	writer := &countingKafkaWriter{}
	sc.Kafka = writer
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const targetID = uint64(2204)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "0")

	resp, err := NewEnterSceneLogic(ctx, sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: 5104, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	loc, locErr := GetPlayerLocation(ctx, sc, 5104)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, targetID, loc.SceneId)
	assert.Equal(t, 1, writer.count())
}

func TestEnterScene_CorruptPlayerLocationFailsClosedBeforeReservation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	writer := &countingKafkaWriter{}
	sc.Kafka = writer

	const (
		playerID = uint64(5107)
		targetID = uint64(2207)
	)
	mr.Set(getPlayerLocationKey(playerID), "not-a-player-location-protobuf")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "4")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "4")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "corrupt-location",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrRedis, resp.ErrorCode)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "4", count)
	assert.Equal(t, "4", nodeCount)
	assert.Equal(t, 0, writer.count())
	raw, rawErr := sc.Redis.Get(getPlayerLocationKey(playerID))
	require.NoError(t, rawErr)
	assert.Equal(t, "not-a-player-location-protobuf", raw)
	exists, existsErr := sc.Redis.Exists(fmt.Sprintf("enter_scene:dedup:%d:corrupt-location", playerID))
	require.NoError(t, existsErr)
	assert.False(t, exists, "失败终态不能缓存成功或保留 pending owner")
}

func TestEnterScene_KafkaRouteFailureRollsBackFirstLanding(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	writer := &countingKafkaWriter{err: errors.New("broker unavailable")}
	sc.Kafka = writer
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const (
		playerID = uint64(5108)
		targetID = uint64(2208)
	)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "0")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "0")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "route-broker-failure",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Equal(t, 1, writer.count(), "必须实际尝试一次同步投递")
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "0", count)
	assert.Equal(t, "0", nodeCount)
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	assert.Nil(t, loc)
	assert.NoError(t, locErr)
	exists, existsErr := sc.Redis.Exists(fmt.Sprintf("enter_scene:dedup:%d:route-broker-failure", playerID))
	require.NoError(t, existsErr)
	assert.False(t, exists, "broker 未 ACK 时不得缓存假成功")
}

func TestEnterScene_InvalidGateIDFailsBeforeReservation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{}

	const targetID = uint64(2218)
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "4")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "4")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: 5118, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "not-a-number",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrInvalidGateID, resp.ErrorCode)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "4", count)
	assert.Equal(t, "4", nodeCount)
	loc, locErr := GetPlayerLocation(context.Background(), sc, 5118)
	require.NoError(t, locErr)
	assert.Nil(t, loc)
}

func TestEnterScene_InvalidSceneNodeIDReleasesAutoReservation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{}

	const (
		playerID = uint64(5119)
		targetID = uint64(2219)
		confID   = uint64(3319)
	)
	mr.SAdd(worldChannelsKey(testZoneId, confID), fmt.Sprintf("%d", targetID))
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "not-a-number")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "2")
	mr.Set(nodePlayerCountKey(testZoneId, "not-a-number"), "2")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "not-a-number")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneConfId: confID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrInvalidNodeID, resp.ErrorCode)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "not-a-number"))
	assert.Equal(t, "2", count)
	assert.Equal(t, "2", nodeCount)
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	assert.Nil(t, loc)
}

func TestEnterScene_RouteFailureRestoresResolvedOldZoneCounters(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const (
		playerID = uint64(5120)
		oldScene = uint64(1120)
		newScene = uint64(2220)
	)
	mr.Set(sceneNodeKey(oldScene), "10")
	mr.Set(sceneZoneKey(oldScene), fmt.Sprintf("%d", testZoneId))
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, oldScene), "1")
	mr.Set(sceneNodeKey(newScene), "10")
	mr.Set(sceneZoneKey(newScene), fmt.Sprintf("%d", testZoneId))
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, newScene), "0")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "1")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	// 模拟 zone_id 字段上线前写入的旧 location；旧 zone 必须由 scene key 补出。
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 0))
	oldRaw, err := sc.Redis.Get(getPlayerLocationKey(playerID))
	require.NoError(t, err)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: newScene, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	restoredRaw, getErr := sc.Redis.Get(getPlayerLocationKey(playerID))
	require.NoError(t, getErr)
	assert.Equal(t, oldRaw, restoredRaw, "位置必须恢复为写入前的精确 protobuf bytes")
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	newCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, newScene))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	legacyZoneCount, _ := sc.Redis.Get(nodePlayerCountKey(0, "10"))
	assert.Equal(t, "1", oldCount)
	assert.Equal(t, "0", newCount)
	assert.Equal(t, "1", nodeCount)
	assert.Empty(t, legacyZoneCount, "不得把旧节点 aggregate 恢复到 zone=0")
}

func TestEnterScene_RouteFailureCASDoesNotOverwriteConcurrentLocation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID        = uint64(5121)
		oldScene        = uint64(1121)
		newScene        = uint64(2221)
		concurrentScene = uint64(3321)
	)
	mr.Set(sceneNodeKey(oldScene), "10")
	mr.Set(sceneZoneKey(oldScene), fmt.Sprintf("%d", testZoneId))
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, oldScene), "1")
	mr.Set(sceneNodeKey(newScene), "10")
	mr.Set(sceneZoneKey(newScene), fmt.Sprintf("%d", testZoneId))
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, newScene), "0")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "1")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))

	writer := &countingKafkaWriter{err: errors.New("broker unavailable")}
	writer.onWrite = func(_ []kafka.Message) {
		require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, concurrentScene, "10", testZoneId))
	}
	sc.Kafka = writer
	sc.Config.KafkaWriteTimeoutSeconds = 1

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: newScene, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, concurrentScene, loc.SceneId,
		"route 失败请求只能回滚自己写入的精确值，不能覆盖并发推进")
}

func TestEnterScene_RedirectKafkaFailureIsNotCached(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const (
		playerID = uint64(5109)
		targetID = uint64(2209)
		confID   = uint64(3309)
	)
	mr.SAdd(worldChannelsKey(2, confID), fmt.Sprintf("%d", targetID))
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "20")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "2")
	mr.Set(nodePlayerCountKey(2, "20"), "2")
	mr.ZAdd(nodeLoadKey(2), 0, "20")

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32) (*scene_manager.RedirectToGateInfo, error) {
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneConfId: confID, ZoneId: 2, GateZoneId: 1,
		GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "redirect-broker-failure",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(2, "20"))
	assert.Equal(t, "2", count, "重定向前的预占必须释放")
	assert.Equal(t, "2", nodeCount)
	exists, existsErr := sc.Redis.Exists(fmt.Sprintf("enter_scene:dedup:%d:redirect-broker-failure", playerID))
	require.NoError(t, existsErr)
	assert.False(t, exists, "broker 未 ACK 时不得缓存 redirect 成功")
}

func TestEnterScene_DirectSuccessReplaysFullCachedResponse(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	writer := &countingKafkaWriter{}
	sc.Kafka = writer
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const targetID = uint64(2210)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "0")
	request := &scene_manager.EnterSceneRequest{
		PlayerId: 5110, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test", RequestId: "direct-success-replay",
	}
	dedupeKey := fmt.Sprintf("enter_scene:dedup:%d:%s", request.PlayerId, request.RequestId)

	// 旧格式/损坏缓存必须先做 compare-delete，再真正执行本次请求。
	mr.Set(dedupeKey, "done:not-base64")
	first, err := NewEnterSceneLogic(ctx, sc).EnterScene(request)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), first.ErrorCode)
	assert.Equal(t, 1, writer.count())
	assert.Equal(t, time.Minute, mr.TTL(dedupeKey))

	// 删除目标映射：若第二次还进入解析路径就会失败；成功只能来自完整响应重放。
	mr.Del(fmt.Sprintf(SceneNodeKeyFmt, targetID))
	second, err := NewEnterSceneLogic(ctx, sc).EnterScene(request)
	require.NoError(t, err)
	assert.True(t, gproto.Equal(first, second))
	assert.Equal(t, 1, writer.count(), "重放不得再次发送 Gate 路由")
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	assert.Equal(t, "1", count, "重放不得再次增加目标场景人数")
}

func TestEnterScene_RedirectSuccessReplaysRedirectPayload(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	const (
		targetID = uint64(2211)
		confID   = uint64(3311)
	)
	mr.SAdd(worldChannelsKey(2, confID), fmt.Sprintf("%d", targetID))
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "20")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "2")
	mr.Set(nodePlayerCountKey(2, "20"), "2")
	mr.ZAdd(nodeLoadKey(2), 0, "20")

	logic := NewEnterSceneLogic(ctx, sc)
	assignCalls := 0
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32) (*scene_manager.RedirectToGateInfo, error) {
		assignCalls++
		return &scene_manager.RedirectToGateInfo{
			TargetGateIp: "10.2.0.8", TargetGatePort: 7001,
			TokenPayload: []byte{1, 2, 3}, TokenSignature: []byte("signature"), TokenDeadline: 123456,
		}, nil
	}
	request := &scene_manager.EnterSceneRequest{
		PlayerId: 5111, SceneConfId: confID, ZoneId: 2,
		GateZoneId: 1, RequestId: "redirect-success-replay",
	}

	first, err := logic.EnterScene(request)
	require.NoError(t, err)
	require.NotNil(t, first.Redirect)
	assert.Equal(t, uint32(0), first.ErrorCode)
	assert.Equal(t, 1, assignCalls)

	// 破坏目标解析前提，第二次仍必须重放第一次的完整 redirect，而非空成功。
	mr.Del(worldChannelsKey(2, confID))
	second, err := NewEnterSceneLogic(ctx, sc).EnterScene(request)
	require.NoError(t, err)
	assert.True(t, gproto.Equal(first, second))
	require.NotNil(t, second.Redirect)
	assert.Equal(t, "10.2.0.8", second.Redirect.TargetGateIp)
	assert.Equal(t, []byte("signature"), second.Redirect.TokenSignature)
	assert.Equal(t, 1, assignCalls, "重放不得再次分配 Gate")

	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(2, "20"))
	assert.Equal(t, "2", targetCount, "重定向路径必须释放目标场景预占")
	assert.Equal(t, "2", nodeCount, "重定向路径必须释放目标节点预占")
}

func TestEnterScene_PendingDedupeReturnsRetryableErrorWithoutDeletingOwner(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	request := &scene_manager.EnterSceneRequest{PlayerId: 5112, RequestId: "still-running"}
	fingerprint, err := enterSceneRequestFingerprint(request)
	require.NoError(t, err)
	key := fmt.Sprintf("enter_scene:dedup:%d:%s", request.PlayerId, request.RequestId)
	owner := "pending:" + fingerprint + ":first-owner"
	mr.Set(key, owner)
	mr.SetTTL(key, time.Minute)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(request)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrEnterSceneInProgress, resp.ErrorCode)
	value, getErr := sc.Redis.Get(key)
	require.NoError(t, getErr)
	assert.Equal(t, owner, value, "并发命中者不得删除首个请求的 owner")
}

func TestEnterScene_SamePlayerRequestIDWithDifferentPayloadIsRejected(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	first := &scene_manager.EnterSceneRequest{
		PlayerId: 5113, SceneId: 2213, ZoneId: testZoneId, RequestId: "reused-request",
	}
	fingerprint, err := enterSceneRequestFingerprint(first)
	require.NoError(t, err)
	key := fmt.Sprintf("enter_scene:dedup:%d:%s", first.PlayerId, first.RequestId)
	owner := "pending:" + fingerprint + ":first-owner"
	mr.Set(key, owner)
	mr.SetTTL(key, time.Minute)

	conflict := gproto.Clone(first).(*scene_manager.EnterSceneRequest)
	conflict.SceneId++
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(conflict)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrEnterSceneIdempotencyConflict, resp.ErrorCode)
	value, getErr := sc.Redis.Get(key)
	require.NoError(t, getErr)
	assert.Equal(t, owner, value, "冲突请求不得覆盖或删除首个 owner")
}

func TestEnterScene_RequestIDIsScopedByPlayer(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const requestID = "same-client-generated-id"
	first := &scene_manager.EnterSceneRequest{PlayerId: 5114, RequestId: requestID}
	fingerprint, err := enterSceneRequestFingerprint(first)
	require.NoError(t, err)
	firstKey := fmt.Sprintf("enter_scene:dedup:%d:%s", first.PlayerId, requestID)
	mr.Set(firstKey, "pending:"+fingerprint+":first-owner")
	mr.SetTTL(firstKey, time.Minute)

	// 第二名玩家使用相同 request_id 时必须拥有独立 key；这里没有场景可选，
	// 应走正常解析并返回 ErrNoAvailableNode，而不是命中第一名玩家的 pending。
	second := &scene_manager.EnterSceneRequest{PlayerId: 5115, RequestId: requestID}
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(second)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
	assert.NotEqual(t, constants.ErrEnterSceneInProgress, resp.ErrorCode)
}

func TestEnterScene_FirstLandingCrossZoneReachesRedirectPath(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	const (
		targetID = uint64(2205)
		confID   = uint64(3305)
	)
	mr.SAdd(worldChannelsKey(2, confID), fmt.Sprintf("%d", targetID))
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "20")
	mr.Set(fmt.Sprintf(InstancePlayerCountKey, targetID), "2")
	mr.Set(nodePlayerCountKey(2, "20"), "2")
	mr.ZAdd(nodeLoadKey(2), 0, "20")
	// 未配置 GateTokenSecret 会让真正的重定向返回“无可用 Gate”；关键断言是
	// 首次落点没有被安全门禁误判为不安全交接，且目标预占仍能正确回滚。
	resp, err := NewEnterSceneLogic(ctx, sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: 5105, SceneConfId: confID, ZoneId: 2, GateZoneId: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNoAvailableNode, resp.ErrorCode)
	assert.NotEqual(t, constants.ErrUnsafeCrossNodeHandoff, resp.ErrorCode)
	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	nodeCount, _ := sc.Redis.Get(nodePlayerCountKey(2, "20"))
	assert.Equal(t, "2", targetCount)
	assert.Equal(t, "2", nodeCount)
}

func TestLeaveScene_DecrementsPlayerCount(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")
	logic := NewCreateSceneLogic(ctx, sc)
	resp, _ := logic.CreateScene(&scene_manager.CreateSceneRequest{SceneConfId: 2001, ZoneId: testZoneId})
	sceneId := resp.SceneId

	// Enter first.
	enterLogic := NewEnterSceneLogic(ctx, sc)
	enterLogic.EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: 1001,
		SceneId:  sceneId,
	})

	// Leave.
	leaveLogic := NewLeaveSceneLogic(ctx, sc)
	leaveLogic.LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: 1001,
		SceneId:  sceneId,
	})

	countKey := fmt.Sprintf(InstancePlayerCountKey, sceneId)
	val, _ := sc.Redis.Get(countKey)
	assert.Equal(t, "0", val)
}

func TestDecrPlayerCount_NeverGoesNegative(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")

	// Decrement without any prior increment.
	DecrInstancePlayerCount(sc, testZoneId, 999)

	countKey := fmt.Sprintf(InstancePlayerCountKey, 999)
	val, _ := sc.Redis.Get(countKey)
	assert.Equal(t, "0", val, "player count should be clamped to 0, not negative")
}

// ---------------------------------------------------------------------------
// Idempotent re-init tests (node restart / re-appearance)
// ---------------------------------------------------------------------------

func TestInitWorldScenes_Idempotent_NoDuplicateRedisEntries(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	confIds := []uint64{1001, 1002}
	ctx := context.Background()

	// Register a node in the load set.
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")

	// First init: allocates scene IDs in Redis.
	initWorldScenesForZone(ctx, sc, testZoneId, confIds, true)

	lines1, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	lines2, _ := GetAllWorldChannels(ctx, sc, 1002, testZoneId)
	assert.Len(t, lines1, 1, "conf 1001 should have 1 channel")
	assert.Len(t, lines2, 1, "conf 1002 should have 1 channel")
	assert.NotEqual(t, lines1[0], lines2[0], "different configs should get different scene IDs")

	// Second init (simulates node re-appearance): must NOT allocate new IDs.
	initWorldScenesForZone(ctx, sc, testZoneId, confIds, true)

	lines1After, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	lines2After, _ := GetAllWorldChannels(ctx, sc, 1002, testZoneId)
	assert.Equal(t, lines1, lines1After, "re-init must not change scene IDs for conf 1001")
	assert.Equal(t, lines2, lines2After, "re-init must not change scene IDs for conf 1002")
}

// ---------------------------------------------------------------------------
// Main scene channel (分线) tests
// ---------------------------------------------------------------------------

func TestInitWorldScenes_MultipleChannels(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	confIds := []uint64{1001}
	sc.Config.WorldChannelCount = 3
	ctx := context.Background()

	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "20")

	initWorldScenesForZone(ctx, sc, testZoneId, confIds, true)

	lines, err := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	require.NoError(t, err)
	assert.Len(t, lines, 3, "should create 3 channels")

	// All scene IDs should be unique.
	seen := make(map[uint64]bool)
	for _, id := range lines {
		assert.False(t, seen[id], "duplicate scene ID %d", id)
		seen[id] = true
	}
}

func TestInitWorldScenes_MultipleChannels_Idempotent(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	confIds := []uint64{1001}
	sc.Config.WorldChannelCount = 3
	ctx := context.Background()

	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")

	// First init.
	initWorldScenesForZone(ctx, sc, testZoneId, confIds, true)
	linesBefore, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	assert.Len(t, linesBefore, 3)

	// Second init — must not add extra channels.
	initWorldScenesForZone(ctx, sc, testZoneId, confIds, true)
	linesAfter, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	assert.Len(t, linesAfter, 3, "re-init must not create extra channels")
}

func TestGetBestWorldChannel_SelectsLowestPlayerCount(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	confIds := []uint64{1001}
	sc.Config.WorldChannelCount = 3
	ctx := context.Background()

	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")

	initWorldScenesForZone(ctx, sc, testZoneId, confIds, true)

	lines, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	require.Len(t, lines, 3)

	// Set different player counts: lines[0]=10, lines[1]=2, lines[2]=5.
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, lines[0]), "10")
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, lines[1]), "2")
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, lines[2]), "5")

	bestId, bestNode, err := GetBestWorldChannel(ctx, sc, 1001, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, lines[1], bestId, "should pick channel with lowest player count")
	assert.NotEmpty(t, bestNode)
}

func TestGetBestWorldChannel_AllEmpty(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	confIds := []uint64{1001}
	sc.Config.WorldChannelCount = 3
	ctx := context.Background()

	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")

	initWorldScenesForZone(ctx, sc, testZoneId, confIds, true)

	// All channels have player_count = 0 (initialized by init).
	bestId, _, err := GetBestWorldChannel(ctx, sc, 1001, testZoneId)
	require.NoError(t, err)
	assert.NotEqual(t, uint64(0), bestId, "should return some channel even when all empty")
}

func TestCreateScene_MainWorld_MultipleChannels_ReturnsLeastLoaded(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.WorldChannelCount = 3
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")

	logic := NewCreateSceneLogic(ctx, sc)

	// First call creates channels on demand, returns best.
	resp1, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 1001,
		ZoneId:      testZoneId,
		SceneType:   1,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp1.ErrorCode)

	// 3 channels should now exist.
	lines, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	assert.Len(t, lines, 3)

	// Simulate load: put 100 players on the channel that was returned.
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, resp1.SceneId), "100")

	// Second call should pick a different (less loaded) channel.
	resp2, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 1001,
		ZoneId:      testZoneId,
		SceneType:   1,
	})
	require.NoError(t, err)
	assert.NotEqual(t, resp1.SceneId, resp2.SceneId, "should pick a less loaded channel")
}

func TestCreateScene_MainWorld_ChannelCountDefault1_BackwardCompat(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	// WorldChannelCount defaults to 1 via newTestSvcCtxWithWorldScenes.
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")

	logic := NewCreateSceneLogic(ctx, sc)

	resp1, _ := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 1001,
		ZoneId:      testZoneId,
		SceneType:   1,
	})
	resp2, _ := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 1001,
		ZoneId:      testZoneId,
		SceneType:   1,
	})

	assert.Equal(t, resp1.SceneId, resp2.SceneId, "ChannelCount=1 -> same scene every time")

	lines, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	assert.Len(t, lines, 1, "should have exactly 1 channel")
}

// ---------------------------------------------------------------------------
// Mirror co-location — additional edge cases
// ---------------------------------------------------------------------------
//
// The happy path, dead-source fallback, overload fallback, and explicit
// TargetNodeId override are covered above. These extra cases pin:
//   - single-node deployments (trivial co-location candidate),
//   - missing scene->node mapping (different from "node dead"),
//   - under-cap still co-locates (guards against the inequality flipping),
//   - non-mirror requests do NOT accidentally consume source data.

func TestCreateScene_Mirror_SingleNode_Trivial(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	// Only one node in the zone. Co-location is trivially satisfied; this
	// test makes sure the single-node path doesn't regress when we evolve
	// the decision logic later.
	mr.ZAdd(testLoadKey(), 0, "7")
	withReachableSceneNode(t, sc, "7")
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(55555)), "7")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      2,
		SourceSceneId:  55555,
		MirrorConfigId: 9002,
	})
	require.NoError(t, err)
	assert.Equal(t, "7", resp.NodeId)
}

func TestCreateScene_Mirror_NoSourceMapping_FallsBackToBestNode(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	// Two live nodes; SourceSceneId has NO scene:<id>:node key at all
	// (different from "node is dead"). Expect clean fallback to GetBestNode.
	mr.ZAdd(testLoadKey(), 5, "1")
	mr.ZAdd(testLoadKey(), 50, "2")
	withReachableSceneNode(t, sc, "1", "2")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      2,
		SourceSceneId:  999999,
		MirrorConfigId: 9003,
	})
	require.NoError(t, err)
	assert.Equal(t, "1", resp.NodeId, "no source mapping -> fall back to GetBestNode")
}

func TestCreateScene_Mirror_SourceUnderLoadCap_StillColocates(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MirrorSourceNodeLoadCap = 100
	ctx := context.Background()

	// Source node "hot" is within the cap (scene_count=3 < cap=100), so
	// co-location still wins even though "cool" has a lower best-node score.
	mr.ZAdd(testLoadKey(), 100, "hot")
	mr.ZAdd(testLoadKey(), 0, "cool")
	withReachableSceneNode(t, sc, "hot", "cool")
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(33334)), "hot")
	sc.Redis.Set(nodeSceneCountKey(testZoneId, "hot"), "3")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      2,
		SourceSceneId:  33334,
		MirrorConfigId: 9006,
	})
	require.NoError(t, err)
	assert.Equal(t, "hot", resp.NodeId, "source under cap -> still co-locate")
}

func TestCreateScene_Instance_NonMirror_UsesGetBestNode(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	// Seed a scene->node mapping just to verify non-mirror requests do NOT
	// accidentally read it. SourceSceneId omitted, so GetBestNode wins.
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(11111)), "A")
	mr.ZAdd(testLoadKey(), 100, "A")
	mr.ZAdd(testLoadKey(), 0, "B")
	withReachableSceneNode(t, sc, "A", "B")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   2,
	})
	require.NoError(t, err)
	assert.Equal(t, "B", resp.NodeId, "non-mirror path must use GetBestNode")
}

// ---------------------------------------------------------------------------
// Mirror lifecycle — idle mirrors get destroyed faster than regular instances
// ---------------------------------------------------------------------------
//
// Mirrors re-initialize NPCs on every entry, so keeping an empty mirror
// around wastes memory. These tests pin the per-type idle timeouts and the
// mirror-flag bookkeeping that drives that behavior.

func TestCreateScene_Mirror_SetsMirrorFlag(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(77777)), "10")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      2,
		SourceSceneId:  77777,
		MirrorConfigId: 9010,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)

	flag, _ := sc.Redis.Get(sceneMirrorFlagKey(resp.SceneId))
	assert.Equal(t, "1", flag, "mirror scene must carry the mirror flag")
}

func TestCreateScene_NonMirror_NoMirrorFlag(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   2,
	})
	require.NoError(t, err)

	flag, err := sc.Redis.Get(sceneMirrorFlagKey(resp.SceneId))
	// miniredis returns "" with no error for missing keys.
	assert.Empty(t, flag, "non-mirror must not set mirror flag (got %q, err=%v)", flag, err)
}

func TestCleanupIdleInstances_MirrorDestroyedFaster(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")
	logic := NewCreateSceneLogic(ctx, sc)

	// Seed the source scene so mirror co-location picks node "10".
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(88888)), "10")

	mirrorResp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      2,
		SourceSceneId:  88888,
		MirrorConfigId: 9011,
	})
	require.NoError(t, err)
	instanceResp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2002,
		ZoneId:      testZoneId,
		SceneType:   2,
	})
	require.NoError(t, err)

	// Backdate both to 100s ago so they're idle.
	instKey := activeInstancesKey(testZoneId)
	oldScore := nowUnix() - 100
	sc.Redis.Zadd(instKey, oldScore, fmt.Sprintf("%d", mirrorResp.SceneId))
	sc.Redis.Zadd(instKey, oldScore, fmt.Sprintf("%d", instanceResp.SceneId))

	// Mirror timeout 30s < 100s idle -> destroy.
	// Instance timeout 300s > 100s idle -> keep.
	cleanupZoneIdleInstances(ctx, sc, testZoneId, 300, 30)

	members, _ := sc.Redis.ZrangeWithScores(instKey, 0, -1)
	keys := make([]string, 0, len(members))
	for _, p := range members {
		keys = append(keys, p.Key)
	}
	assert.NotContains(t, keys, fmt.Sprintf("%d", mirrorResp.SceneId), "idle mirror must be destroyed")
	assert.Contains(t, keys, fmt.Sprintf("%d", instanceResp.SceneId), "idle instance under its own cap must survive")

	// Mirror flag also cleaned up.
	flag, _ := sc.Redis.Get(sceneMirrorFlagKey(mirrorResp.SceneId))
	assert.Empty(t, flag, "mirror flag must be cleared on destroy")
}

func TestCleanupIdleInstances_MirrorWithPlayers_NotDestroyed(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(88889)), "10")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      2,
		SourceSceneId:  88889,
		MirrorConfigId: 9012,
	})
	require.NoError(t, err)

	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, resp.SceneId), "1")

	instKey := activeInstancesKey(testZoneId)
	sc.Redis.Zadd(instKey, nowUnix()-100, fmt.Sprintf("%d", resp.SceneId))

	cleanupZoneIdleInstances(ctx, sc, testZoneId, 300, 30)

	members, _ := sc.Redis.ZrangeWithScores(instKey, 0, -1)
	require.Len(t, members, 1)
	assert.Equal(t, fmt.Sprintf("%d", resp.SceneId), members[0].Key,
		"mirror with active player must NOT be destroyed, even past timeout")
}

func TestDestroyScene_ClearsMirrorFlag(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	mr.ZAdd(testLoadKey(), 0, "10")
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(99990)), "10")

	createLogic := NewCreateSceneLogic(ctx, sc)
	resp, err := createLogic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      2,
		SourceSceneId:  99990,
		MirrorConfigId: 9013,
	})
	require.NoError(t, err)

	flag, _ := sc.Redis.Get(sceneMirrorFlagKey(resp.SceneId))
	require.Equal(t, "1", flag)

	destroyLogic := NewDestroySceneLogic(ctx, sc)
	_, err = destroyLogic.DestroyScene(&scene_manager.DestroySceneRequest{
		SceneId: resp.SceneId,
		ZoneId:  testZoneId,
	})
	require.NoError(t, err)

	flag, _ = sc.Redis.Get(sceneMirrorFlagKey(resp.SceneId))
	assert.Empty(t, flag, "explicit DestroyScene must also clear the mirror flag")
}

func TestResolveMirrorTimeout_FallsBackToInstance(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	sc.Config.MirrorIdleTimeoutSeconds = 0
	sc.Config.InstanceIdleTimeoutSeconds = 300
	assert.Equal(t, int64(300), resolveMirrorTimeout(sc), "0 must fall back to instance timeout")

	sc.Config.MirrorIdleTimeoutSeconds = 15
	assert.Equal(t, int64(15), resolveMirrorTimeout(sc), "positive value wins")
}

// ---------------------------------------------------------------------------
// Node-type routing (WoW-style world server vs instance server separation)
// ---------------------------------------------------------------------------
//
// The main-world and instance paths must land on nodes whose
// scene_node_type matches the purpose. Mirror co-location is the only
// intentional exception — it prefers the source scene's node so the
// already-resident map/AI/spawn data can be reused.

func TestGetNodesForPurpose_FiltersByType(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)

	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeInstance, 0)
	registerTypedNode(mr, testZoneId, "30", constants.SceneNodeTypeInstance, 0)

	worldPool := getNodesForPurpose(sc, testZoneId, constants.NodePurposeWorld)
	assert.ElementsMatch(t, []string{"10"}, worldPool)

	instancePool := getNodesForPurpose(sc, testZoneId, constants.NodePurposeInstance)
	assert.ElementsMatch(t, []string{"20", "30"}, instancePool)
}

func TestGetNodesForPurpose_StrictMode_NoFallback(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.StrictNodeTypeSeparation = true

	// Only instance-type nodes registered; world request must return nil.
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeInstance, 0)
	registerTypedNode(mr, testZoneId, "30", constants.SceneNodeTypeInstance, 0)

	worldPool := getNodesForPurpose(sc, testZoneId, constants.NodePurposeWorld)
	assert.Empty(t, worldPool, "strict mode must not leak instance nodes into the world pool")
}

func TestGetNodesForPurpose_NonStrict_FallsBackToAll(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.StrictNodeTypeSeparation = false

	// Only instance-type nodes; world request should fall back in non-strict.
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeInstance, 0)
	registerTypedNode(mr, testZoneId, "30", constants.SceneNodeTypeInstance, 5)

	worldPool := getNodesForPurpose(sc, testZoneId, constants.NodePurposeWorld)
	assert.ElementsMatch(t, []string{"20", "30"}, worldPool,
		"non-strict: world request falls back to full pool when no world nodes exist")
}

func TestGetNodesForPurpose_UnclassifiedAllowedForAnyPurpose(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.StrictNodeTypeSeparation = true

	// No scene_node_type mirrored — simulates a node in the middle of
	// registration, or a legacy node that never set GameConfig.scene_node_type.
	mr.ZAdd(testLoadKey(), 0, "99")

	assert.ElementsMatch(t, []string{"99"},
		getNodesForPurpose(sc, testZoneId, constants.NodePurposeWorld),
		"unclassified node must be available to world purpose")
	assert.ElementsMatch(t, []string{"99"},
		getNodesForPurpose(sc, testZoneId, constants.NodePurposeInstance),
		"unclassified node must be available to instance purpose")
}

func TestGetBestNodeForPurpose_RespectsLoadOrderWithinPool(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	// world-type nodes with different scores; instance-type "hot" has the
	// lowest score but must not win a world pick.
	registerTypedNode(mr, testZoneId, "world-cool", constants.SceneNodeTypeMainWorld, 5)
	registerTypedNode(mr, testZoneId, "world-warm", constants.SceneNodeTypeMainWorld, 10)
	registerTypedNode(mr, testZoneId, "instance-hot", constants.SceneNodeTypeInstance, 0)

	best, err := GetBestNodeForPurpose(ctx, sc, testZoneId, constants.NodePurposeWorld)
	require.NoError(t, err)
	assert.Equal(t, "world-cool", best, "must pick lowest-load node in the world pool, not the hot instance node")
}

func TestCreateScene_Instance_StrictMode_RejectsWhenNoInstanceNode(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.StrictNodeTypeSeparation = true
	ctx := context.Background()

	// Only main-world nodes in the zone.
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNoNodeForPurpose, resp.ErrorCode,
		"strict mode: instance request with only world nodes must fail with ErrNoNodeForPurpose")
}

func TestCreateScene_Mirror_BypassesPurposeFilter(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.StrictNodeTypeSeparation = true
	ctx := context.Background()

	// World-type node hosts the source scene; instance-type node is the
	// "natural" target for a fresh instance request under strict mode.
	// Mirror path must still pick the world node to preserve map reuse.
	registerTypedNode(mr, testZoneId, "world", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "instance", constants.SceneNodeTypeInstance, 0)
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(55555)), "world")

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId:  55555,
		MirrorConfigId: 9999,
	})
	require.NoError(t, err)
	assert.Equal(t, "world", resp.NodeId,
		"mirror must co-locate on source node even when it's a world-type node and strict mode is on")
}

func TestInitWorldScenes_StrictMode_SkipsInstanceOnlyZone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.StrictNodeTypeSeparation = true
	ctx := context.Background()

	// Only instance-type nodes; strict mode must refuse to place world channels.
	registerTypedNode(mr, testZoneId, "inst1", constants.SceneNodeTypeInstance, 0)

	initWorldScenesForZone(ctx, sc, testZoneId, []uint64{1001}, true)

	channels, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	assert.Empty(t, channels, "strict mode must not create world channels on instance-only nodes")
}

// ---------------------------------------------------------------------------
// Per-confId channel-count override
// ---------------------------------------------------------------------------

func TestChannelCountFor_PerConfIdOverride(t *testing.T) {
	c := &config.Config{
		WorldChannelCount: 2,
		WorldChannelCountByConfId: map[string]int{
			"1001": 5,
			"1002": 1,
		},
	}

	assert.Equal(t, 5, c.ChannelCountFor(1001), "explicit override wins")
	assert.Equal(t, 1, c.ChannelCountFor(1002), "override can shrink too")
	assert.Equal(t, 2, c.ChannelCountFor(9999), "non-overridden confId uses default")
}

func TestChannelCountFor_ClampsToOne(t *testing.T) {
	c := &config.Config{WorldChannelCount: 0}
	assert.Equal(t, 1, c.ChannelCountFor(1001),
		"WorldChannelCount=0 must clamp to 1 (every world scene needs at least one channel)")
}

func TestInitWorldScenes_PerConfIdOverride(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.WorldChannelCount = 2
	sc.Config.WorldChannelCountByConfId = map[string]int{
		"1001": 4, // hot city
		"1002": 1, // tutorial
	}
	ctx := context.Background()

	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)

	initWorldScenesForZone(ctx, sc, testZoneId, []uint64{1001, 1002, 1003}, true)

	ch1001, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	ch1002, _ := GetAllWorldChannels(ctx, sc, 1002, testZoneId)
	ch1003, _ := GetAllWorldChannels(ctx, sc, 1003, testZoneId)

	assert.Len(t, ch1001, 4, "1001 uses its override (4)")
	assert.Len(t, ch1002, 1, "1002 uses its override (1)")
	assert.Len(t, ch1003, 2, "1003 falls back to the default (2)")
}

// ---------------------------------------------------------------------------
// Per-node player count aggregate (feeds composite load score)
// ---------------------------------------------------------------------------

func TestIncrDecrPlayerCount_UpdatesPerNodeAggregate(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")

	sceneId := uint64(777)
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneId), "node-A")

	IncrInstancePlayerCount(sc, testZoneId, sceneId)
	IncrInstancePlayerCount(sc, testZoneId, sceneId)

	nodeVal, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "node-A"))
	assert.Equal(t, "2", nodeVal, "per-node counter must track enters")

	DecrInstancePlayerCount(sc, testZoneId, sceneId)
	nodeVal, _ = sc.Redis.Get(nodePlayerCountKey(testZoneId, "node-A"))
	assert.Equal(t, "1", nodeVal, "per-node counter must track leaves")
}

func TestDecrPlayerCount_NodeAggregate_ClampsToZero(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")

	sceneId := uint64(777)
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneId), "node-A")

	// Decrement without any prior increment.
	DecrInstancePlayerCount(sc, testZoneId, sceneId)

	nodeVal, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "node-A"))
	assert.Equal(t, "0", nodeVal, "per-node counter must clamp to 0 like the per-scene counter")
}

func TestDestroyInstance_DrainsResidualFromNodeAggregate(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	logic := NewCreateSceneLogic(ctx, sc)
	resp, err := logic.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)

	// Pump three players into the instance.
	enter := NewEnterSceneLogic(ctx, sc)
	for i := uint64(1); i <= 3; i++ {
		_, err := enter.EnterScene(&scene_manager.EnterSceneRequest{PlayerId: i, SceneId: resp.SceneId})
		require.NoError(t, err)
	}
	nodeVal, _ := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "3", nodeVal)

	// Admin-style destroy while players are still in it — must drain the
	// aggregate, not leak three ghost players forever. Use the force path
	// explicitly: the non-force destroyInstance intentionally CAS-aborts
	// when player_count > 0 so the lifecycle manager can't race with
	// EnterScene.
	destroyInstanceForce(ctx, sc, testZoneId, resp.SceneId, "explicit")
	nodeVal, _ = sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	assert.Equal(t, "0", nodeVal, "node aggregate must be drained by the residual on force-destroy")
}

// TestDestroyInstance_CAS_AbortsWhenPlayersPresent verifies the atomic
// CAS inside destroyInstance refuses to wipe a scene that picked up a
// player between the caller's idle check and the destroy call — the
// destroy-while-entering race.
func TestDestroyInstance_CAS_AbortsWhenPlayersPresent(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)

	// Seed one player so destroyInstance (non-force) should bail.
	_, err = NewEnterSceneLogic(ctx, sc).EnterScene(&scene_manager.EnterSceneRequest{
		PlayerId: 1, SceneId: resp.SceneId,
	})
	require.NoError(t, err)

	destroyInstance(ctx, sc, testZoneId, resp.SceneId)

	// Scene must still exist: the atomic CAS refused to destroy.
	nodeId, err := sc.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, resp.SceneId))
	require.NoError(t, err)
	assert.Equal(t, "10", nodeId, "scene:{id}:node must survive when destroyInstance CAS aborts")

	pc, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, resp.SceneId))
	assert.Equal(t, "1", pc, "player_count must be preserved on CAS abort")
}

// ---------------------------------------------------------------------------
// Composite load score
// ---------------------------------------------------------------------------

func TestComputeNodeLoadScore_WeightedSum(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	sc.Config.NodeLoadWeightSceneCount = 1.0
	sc.Config.NodeLoadWeightPlayerCount = 0.01

	// 2 scenes + 50 players = 2 + 0.5 = 2.5
	score := computeNodeLoadScore(sc, 2, 50)
	assert.InDelta(t, 2.5, score, 0.0001)

	// Defaults clamp negative weights to sane values.
	sc.Config.NodeLoadWeightSceneCount = -1
	sc.Config.NodeLoadWeightPlayerCount = -1
	// With clamped defaults (1.0 and 0) the score reduces to scene_count only.
	assert.InDelta(t, 2.0, computeNodeLoadScore(sc, 2, 50), 0.0001)
}

// ---------------------------------------------------------------------------
// World channel rebalance — planner tests
//
// Integration of the full CreateScene+DestroyScene dance needs a real gRPC
// scene-node fleet which isn't available in unit tests; the planner
// (PlanWorldChannelRebalance) is pure wrt Redis state and sufficiently
// covers the decision logic we actually care about.
// ---------------------------------------------------------------------------

// seedWorldChannel primes Redis with the scene-channel membership and its
// current node mapping, mimicking what initWorldScenesForZone would leave
// behind. Returns nothing — tests assert state via mr.
func seedWorldChannel(sc *svc.ServiceContext, zoneId uint32, confId, sceneId uint64, nodeId string) {
	sc.Redis.Sadd(worldChannelsKey(zoneId, confId), fmt.Sprintf("%d", sceneId))
	sc.Redis.Set(fmt.Sprintf("scene:%d:node", sceneId), nodeId)
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "0")
}

func TestPlanRebalance_UrgentWhenCurrentNodeNotInLivePool(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MaxRebalanceMigrationsPerTick = 10
	confIds := []uint64{1001}

	// "100" is dead; "10" and "20" are live world-hosting nodes.
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)
	seedWorldChannel(sc, testZoneId, 1001, 555, "100") // "100" is not in live set

	urgent, opp, budget := PlanWorldChannelRebalance(sc, testZoneId, confIds)
	assert.Equal(t, 10, budget)
	assert.Empty(t, opp)
	require.Len(t, urgent, 1)
	assert.Equal(t, uint64(555), urgent[0].SceneId)
	assert.Equal(t, "100", urgent[0].OldNode)
	assert.Contains(t, []string{"10", "20"}, urgent[0].NewNode)
	assert.Equal(t, reasonNodeGone, urgent[0].Reason)
}

func TestPlanRebalance_LiveRoleFlipWithPlayersFailsClosed(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MaxRebalanceMigrationsPerTick = 10
	sc.Config.StrictNodeTypeSeparation = true
	confIds := []uint64{1001}

	// 10 仍在 zone load set，但 role 已切成 instance；20 是可用 world 节点。
	// 旧实现把 10 当作 node_gone 直接迁移，即使频道仍有在线玩家。
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)
	seedWorldChannel(sc, testZoneId, 1001, 556, "10")
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, 556), "5")

	urgent, opportunistic, _ := PlanWorldChannelRebalance(sc, testZoneId, confIds)
	assert.Empty(t, urgent, "活节点 role flip 不能绕过跨节点玩家交接门禁")
	assert.Empty(t, opportunistic)
}

func TestPlanRebalance_OpportunisticOnlyWhenEmpty(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MaxRebalanceMigrationsPerTick = 10
	confIds := []uint64{1001}

	// Three live world-hosting nodes. The hash of sceneId 555 lands on ONE
	// of them; we arrange for the current mapping to point at a *different*
	// live node so the channel is a candidate.
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "30", constants.SceneNodeTypeMainWorld, 0)

	nodes := []string{"10", "20", "30"}
	// Find a sceneId whose hash target is a node different from the one we
	// pretend to currently host on. We'll fix "10" as the current, and pick
	// a sceneId whose hash lands on "20" or "30".
	var sceneId uint64 = 1
	for ; sceneId < 100; sceneId++ {
		if assignNodeByHash(sceneId, nodes) != "10" {
			break
		}
	}
	seedWorldChannel(sc, testZoneId, 1001, sceneId, "10")

	// Empty channel → opportunistic candidate.
	_, opp, _ := PlanWorldChannelRebalance(sc, testZoneId, confIds)
	require.Len(t, opp, 1)
	assert.Equal(t, reasonBetterHome, opp[0].Reason)

	// Populate the channel → no longer a candidate.
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "7")
	_, opp2, _ := PlanWorldChannelRebalance(sc, testZoneId, confIds)
	assert.Empty(t, opp2, "hot channels must be skipped even when misaligned")
}

func TestPlanRebalance_NoMovesWhenAlreadyAligned(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MaxRebalanceMigrationsPerTick = 10
	confIds := []uint64{1001}

	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)

	nodes := []string{"10", "20"}
	var sceneId uint64 = 777
	seedWorldChannel(sc, testZoneId, 1001, sceneId, assignNodeByHash(sceneId, nodes))

	urgent, opp, _ := PlanWorldChannelRebalance(sc, testZoneId, confIds)
	assert.Empty(t, urgent)
	assert.Empty(t, opp, "aligned channels must not appear in the plan")
}

func TestPlanRebalance_BudgetZeroDisables(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MaxRebalanceMigrationsPerTick = 0
	confIds := []uint64{1001}

	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	seedWorldChannel(sc, testZoneId, 1001, 555, "deadnode")

	urgent, opp, budget := PlanWorldChannelRebalance(sc, testZoneId, confIds)
	assert.Equal(t, 0, budget)
	assert.Empty(t, urgent)
	assert.Empty(t, opp)
}

// ---------------------------------------------------------------------------
// Orphan cleanup tests
// ---------------------------------------------------------------------------

func TestCleanupOrphanWorldChannels_RefusesEmptyConfTable(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	// World table is empty in unit tests — refuse to wipe.
	sc.Redis.Sadd(worldChannelsKey(testZoneId, 9999), "12345")
	sc.Redis.Set(fmt.Sprintf("scene:%d:node", 12345), "10")

	removed := CleanupOrphanWorldChannels(context.Background(), sc)
	assert.Equal(t, 0, removed, "empty conf table must be treated as 'load failed', not 'wipe everything'")

	// Key still exists.
	assert.True(t, mr.Exists(worldChannelsKey(testZoneId, 9999)))
}

func TestParseWorldChannelsKey(t *testing.T) {
	zone, conf, ok := parseWorldChannelsKey("world_channels:zone:5:1001")
	require.True(t, ok)
	assert.Equal(t, uint32(5), zone)
	assert.Equal(t, uint64(1001), conf)

	_, _, ok = parseWorldChannelsKey("world_channels:zone:5")
	assert.False(t, ok, "missing confId must fail parse")

	_, _, ok = parseWorldChannelsKey("some_other_key:1:2")
	assert.False(t, ok, "wrong prefix must fail parse")

	_, _, ok = parseWorldChannelsKey("world_channels:zone:abc:1001")
	assert.False(t, ok, "non-numeric zone must fail parse")
}

func TestPlanRebalance_NonWorldHostingNotConsidered(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MaxRebalanceMigrationsPerTick = 10
	sc.Config.StrictNodeTypeSeparation = true
	confIds := []uint64{1001}

	// Only instance nodes registered — world pool is empty.
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeInstance, 0)
	seedWorldChannel(sc, testZoneId, 1001, 555, "dead")

	// No world-hosting nodes → planner returns empty even with misaligned
	// channels. We can't migrate to nowhere; the caller will log and move on.
	urgent, opp, _ := PlanWorldChannelRebalance(sc, testZoneId, confIds)
	assert.Empty(t, urgent)
	assert.Empty(t, opp)
}

// ---------------------------------------------------------------------------
// Destroy-while-entering race / atomic CAS
// ---------------------------------------------------------------------------

// TestAtomicIncrPlayerCount_SceneExists is the happy path for the CAS Lua
// helper used inside EnterScene.
func TestAtomicIncrPlayerCount_SceneExists(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	sceneId := uint64(42)
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneId), "10")
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "0")

	count, err := AtomicIncrPlayerCountIfSceneExists(sc, sceneId)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	pc, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	assert.Equal(t, "1", pc)
}

// TestAtomicIncrPlayerCount_SceneGone verifies the CAS path refuses to
// create an orphan player_count key when the scene has already been
// destroyed.
func TestAtomicIncrPlayerCount_SceneGone(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	sceneId := uint64(42)
	// No scene:{id}:node key — simulate a scene that was destroyed before
	// EnterScene's INCR hit Redis.

	count, err := AtomicIncrPlayerCountIfSceneExists(sc, sceneId)
	require.NoError(t, err)
	assert.Equal(t, int64(-1), count, "must return -1 when scene:{id}:node is gone")

	// Most important: we must NOT have created instance:{id}:player_count.
	exists, _ := sc.Redis.Exists(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	assert.False(t, exists, "orphan player_count key must not be created")
}

// TestAtomicDestroyIfIdle_AbortsOnPlayers ensures the destroy-while-
// entering race cannot wipe a scene that picked up a player.
func TestAtomicDestroyIfIdle_AbortsOnPlayers(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	sceneId := uint64(42)

	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneId), "10")
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "1")
	sc.Redis.Zadd(activeInstancesKey(testZoneId), nowUnix(), fmt.Sprintf("%d", sceneId))

	nodeId, _, err := AtomicDestroyIfIdle(sc, testZoneId, sceneId)
	require.NoError(t, err)
	assert.Equal(t, "", nodeId, "must abort when player_count > 0")

	// Scene state must be intact.
	nid, _ := sc.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, sceneId))
	assert.Equal(t, "10", nid)
	pc, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	assert.Equal(t, "1", pc)
}

// TestAtomicDestroyIfIdle_DestroysWhenIdle checks the success path wipes
// all scene-scoped keys and returns the nodeId.
func TestAtomicDestroyIfIdle_DestroysWhenIdle(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	sceneId := uint64(42)

	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneId), "10")
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "0")
	sc.Redis.Set(sceneMirrorFlagKey(sceneId), "1")
	sc.Redis.Set(sceneSourceKey(sceneId), "99")
	sc.Redis.Set(sceneZoneKey(sceneId), "1")
	sc.Redis.Zadd(activeInstancesKey(testZoneId), nowUnix(), fmt.Sprintf("%d", sceneId))

	nodeId, _, err := AtomicDestroyIfIdle(sc, testZoneId, sceneId)
	require.NoError(t, err)
	assert.Equal(t, "10", nodeId)

	for _, key := range []string{
		fmt.Sprintf(SceneNodeKeyFmt, sceneId),
		fmt.Sprintf(InstancePlayerCountKey, sceneId),
		sceneMirrorFlagKey(sceneId),
		sceneSourceKey(sceneId),
		sceneZoneKey(sceneId),
	} {
		exists, _ := sc.Redis.Exists(key)
		assert.False(t, exists, "scene-scoped key %s must be wiped", key)
	}
	score, err := sc.Redis.Zscore(activeInstancesKey(testZoneId), fmt.Sprintf("%d", sceneId))
	_ = score
	assert.Error(t, err, "ZREM from active set must succeed (Zscore should fail for missing member)")
}

// TestEnterScene_AtomicIncr_NoOrphanPlayerCount is the tighter end-to-end
// test: call EnterScene for a sceneId that was mapped but is now gone,
// then verify no orphan player_count key was left behind.
func TestEnterScene_AtomicIncr_NoOrphanPlayerCount(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	// Orphan sceneId that was never created — no scene:{id}:node entry.
	orphanSceneId := uint64(99999)

	_, _ = AtomicIncrPlayerCountIfSceneExists(sc, orphanSceneId)

	// The key MUST NOT have been created.
	exists, _ := sc.Redis.Exists(fmt.Sprintf(InstancePlayerCountKey, orphanSceneId))
	assert.False(t, exists, "EnterScene must never create an orphan player_count for a gone scene")
}

// ---------------------------------------------------------------------------
// Reverse indexes
// ---------------------------------------------------------------------------

// TestCreateInstance_PopulatesNodeScenesIndex ensures the reverse index
// used by the node-death reconciliation loop is kept in sync.
func TestCreateInstance_PopulatesNodeScenesIndex(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)

	members, _ := sc.Redis.Smembers(nodeScenesKey(testZoneId, "10"))
	assert.Contains(t, members, fmt.Sprintf("%d", resp.SceneId),
		"node:zone:{zoneId}:{nodeId}:scenes must contain the freshly-created scene")
}

// TestCreateMirror_PopulatesMirrorSourceIndex ensures cascade destroy has
// something to work with — the reverse index scene:{source}:mirrors.
func TestCreateMirror_PopulatesMirrorSourceIndex(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	// Seed a pretend source scene on the same node so co-location kicks in.
	sourceId := uint64(12345)
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "10")

	mirrorResp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		MirrorConfigId: 9001,
		SourceSceneId:  sourceId,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)

	members, _ := sc.Redis.Smembers(sceneMirrorsKey(sourceId))
	assert.Contains(t, members, fmt.Sprintf("%d", mirrorResp.SceneId),
		"scene:{source}:mirrors must track the new mirror")

	// The reverse source pointer also needs to be there so the mirror can
	// remove itself from the source's mirrors set on destroy.
	src, _ := sc.Redis.Get(sceneSourceKey(mirrorResp.SceneId))
	assert.Equal(t, fmt.Sprintf("%d", sourceId), src)
}

// TestDestroyMirror_UnlinksFromSourceSet verifies a mirror drops itself
// out of scene:{source}:mirrors when it is destroyed — otherwise cascade
// destroy would re-destroy already-gone mirrors forever.
func TestDestroyMirror_UnlinksFromSourceSet(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	sourceId := uint64(12345)
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "10")

	mirrorResp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		MirrorConfigId: 9001,
		SourceSceneId:  sourceId,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)

	destroyInstance(ctx, sc, testZoneId, mirrorResp.SceneId)

	members, _ := sc.Redis.Smembers(sceneMirrorsKey(sourceId))
	assert.NotContains(t, members, fmt.Sprintf("%d", mirrorResp.SceneId),
		"destroyed mirror must be SREM'd from scene:{source}:mirrors")
}

// ---------------------------------------------------------------------------
// Cascade destroy
// ---------------------------------------------------------------------------

// TestDestroyScene_CascadesToMirrors: explicit DestroyScene of a source
// scene must also destroy every mirror it spawned. Leaving them behind
// would point to a dead source, and their NPCs/spawners would still tick
// on the C++ node until idle timeout.
func TestDestroyScene_CascadesToMirrors(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	sourceId := uint64(100)
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "10")
	sc.Redis.Set(sceneZoneKey(sourceId), fmt.Sprintf("%d", testZoneId))
	sc.Redis.Zadd(activeInstancesKey(testZoneId), nowUnix(), fmt.Sprintf("%d", sourceId))

	var mirrorIds []uint64
	for i := 0; i < 3; i++ {
		resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
			SceneConfId:    2001,
			MirrorConfigId: 9001,
			SourceSceneId:  sourceId,
			ZoneId:         testZoneId,
			SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		})
		require.NoError(t, err)
		mirrorIds = append(mirrorIds, resp.SceneId)
	}

	// Explicit destroy of the source.
	_, err := NewDestroySceneLogic(ctx, sc).DestroyScene(&scene_manager.DestroySceneRequest{
		SceneId: sourceId,
		ZoneId:  testZoneId,
	})
	require.NoError(t, err)

	// Every mirror must be gone.
	for _, mid := range mirrorIds {
		nid, _ := sc.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, mid))
		assert.Equal(t, "", nid, "mirror %d must be destroyed by cascade", mid)
	}
	// Mirrors index is empty (and ideally gone).
	members, _ := sc.Redis.Smembers(sceneMirrorsKey(sourceId))
	assert.Empty(t, members, "scene:{source}:mirrors must be drained after cascade")
}

// ---------------------------------------------------------------------------
// Node-death reconciliation
// ---------------------------------------------------------------------------

// TestReconcileDeadNode_DestroysOrphanInstances verifies the sweep run
// from removeNodeFromRedis walks node:zone:{zoneId}:{nodeId}:scenes and force-destroys
// instance scenes whose mapping still points at the dead node.
func TestReconcileDeadNode_DestroysOrphanInstances(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	// Create two instances on node 10.
	var sceneIds []uint64
	for i := 0; i < 2; i++ {
		resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
			SceneConfId: 2001,
			ZoneId:      testZoneId,
			SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		})
		require.NoError(t, err)
		sceneIds = append(sceneIds, resp.SceneId)
	}

	// Simulate node-10 death.
	entry := nodeEntry{nodeID: "10"}
	entry.reg.ZoneId = testZoneId
	entry.reg.SceneNodeType = constants.SceneNodeTypeInstance
	mr.ZRem(nodeLoadKey(testZoneId), "10")
	reconcileDeadNodeScenes(sc, entry)

	// All orphan instances must be wiped.
	for _, sid := range sceneIds {
		nid, _ := sc.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, sid))
		assert.Equal(t, "", nid, "orphan instance %d must be force-destroyed after node death", sid)
	}

	// node:zone:{zoneId}:{nodeId}:scenes set itself is cleared.
	members, _ := sc.Redis.Smembers(nodeScenesKey(testZoneId, "10"))
	assert.Empty(t, members)
}

// TestReconcileDeadNode_PreservesWorldChannels makes sure the
// reconciliation sweep doesn't blow away world channels — those are
// handled by a different pipeline (rebalance / lazy-reassign) and
// accidentally destroying one would drop every player in that shard.
func TestReconcileDeadNode_PreservesWorldChannels(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)

	initWorldScenesForZone(ctx, sc, testZoneId, []uint64{1001}, true)

	channels, _ := GetAllWorldChannels(ctx, sc, 1001, testZoneId)
	require.NotEmpty(t, channels)

	entry := nodeEntry{nodeID: "10"}
	entry.reg.ZoneId = testZoneId
	entry.reg.SceneNodeType = constants.SceneNodeTypeMainWorld
	mr.ZRem(nodeLoadKey(testZoneId), "10")
	reconcileDeadNodeScenes(sc, entry)

	// World channels' scene:{id}:node mapping must still be there — the
	// rebalance loop reassigns them; we do NOT destroy.
	for _, sid := range channels {
		nid, _ := sc.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, sid))
		assert.NotEmpty(t, nid, "world channel %d must NOT be wiped by reconciliation", sid)
	}
}

// TestReconcileDeadNode_SkipsReassignedScenes covers the rare-but-
// possible race: a scene was moved off the dying node before reconciliation
// ran. It must not be destroyed just because the stale zone-scoped old-node scenes set
// set still lists it.
func TestReconcileDeadNode_SkipsReassignedScenes(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	// Pretend scene 77 used to live on "10" and got moved to "20".
	sc.Redis.Sadd(nodeScenesKey(testZoneId, "10"), "77")
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, uint64(77)), "20")
	sc.Redis.Zadd(activeInstancesKey(testZoneId), nowUnix(), "77")

	entry := nodeEntry{nodeID: "10"}
	entry.reg.ZoneId = testZoneId
	reconcileDeadNodeScenes(sc, entry)

	nid, _ := sc.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, uint64(77)))
	assert.Equal(t, "20", nid, "reassigned scene must survive the reconciliation sweep")
}

// ---------------------------------------------------------------------------
// Mirror defensive checks (source-exists + dedup)
// ---------------------------------------------------------------------------

// TestCreateMirror_RejectsWhenSourceGone proves that a mirror create with
// source_scene_id pointing at a non-existent scene fails fast with
// ErrSourceSceneGone, rather than silently producing an "orphan mirror"
// that would never be able to resolve its source-derived state.
func TestCreateMirror_RejectsWhenSourceGone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	// Note: no scene:{12345}:node key seeded — the source is "gone".
	resp, err := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:   2001,
		ZoneId:        testZoneId,
		SceneType:     scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId: 12345,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrSourceSceneGone, resp.ErrorCode)
	assert.Zero(t, resp.SceneId, "no scene should be allocated when source is gone")
}

// TestCreateMirror_AllowsWhenSourceExists is the positive control for the
// defensive check above — without it the rejection path could shadow real
// mirror traffic.
func TestCreateMirror_AllowsWhenSourceExists(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	// Seed an existing source scene.
	const sourceId uint64 = 999
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "10")

	resp, err := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:   2001,
		ZoneId:        testZoneId,
		SceneType:     scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId: sourceId,
	})
	require.NoError(t, err)
	assert.Zero(t, resp.ErrorCode)
	assert.NotZero(t, resp.SceneId)
}

// TestCreateMirror_DedupReusesExisting verifies that with
// MirrorDedupBySource=true a second mirror request for the same source
// returns the existing mirror's scene id (no fresh allocate, no second
// CreateScene RPC equivalent in the metric).
func TestCreateMirror_DedupReusesExisting(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MirrorDedupBySource = true
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	const sourceId uint64 = 555
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "10")

	first, err := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:   2001,
		ZoneId:        testZoneId,
		SceneType:     scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId: sourceId,
	})
	require.NoError(t, err)
	require.NotZero(t, first.SceneId)

	second, err := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:   2001,
		ZoneId:        testZoneId,
		SceneType:     scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId: sourceId,
	})
	require.NoError(t, err)
	assert.Equal(t, first.SceneId, second.SceneId, "dedup should return the same mirror id")
	assert.Equal(t, first.NodeId, second.NodeId)
}

// TestCreateMirror_DedupOff_AllocatesFresh is the negative control — with
// the flag off (default), each request must produce a distinct mirror.
// This guards against an accidental flip of the default that would change
// gameplay semantics for every existing operator.
func TestCreateMirror_DedupOff_AllocatesFresh(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	// Explicitly leave MirrorDedupBySource at its default (false).
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	const sourceId uint64 = 444
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "10")

	first, _ := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001, ZoneId: testZoneId,
		SceneType: scene_manager.SceneType_SCENE_TYPE_INSTANCE, SourceSceneId: sourceId,
	})
	second, _ := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001, ZoneId: testZoneId,
		SceneType: scene_manager.SceneType_SCENE_TYPE_INSTANCE, SourceSceneId: sourceId,
	})
	assert.NotEqual(t, first.SceneId, second.SceneId, "without dedup, each mirror request must allocate fresh")
}

// TestCreateMirror_DedupSkipsStaleEntry covers the "mirrors set still
// references a destroyed mirror" edge case. The dedup path must SREM the
// stale id and fall through to a fresh allocation, not return the dead
// scene.
func TestCreateMirror_DedupSkipsStaleEntry(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MirrorDedupBySource = true
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	const sourceId uint64 = 333
	const staleMirrorId uint64 = 99999
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "10")
	// Seed a dangling membership: id is in the set, but no scene:{id}:node.
	sc.Redis.Sadd(sceneMirrorsKey(sourceId), fmt.Sprintf("%d", staleMirrorId))

	resp, err := NewCreateSceneLogic(context.Background(), sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:   2001,
		ZoneId:        testZoneId,
		SceneType:     scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId: sourceId,
	})
	require.NoError(t, err)
	assert.Zero(t, resp.ErrorCode)
	assert.NotEqual(t, staleMirrorId, resp.SceneId, "must not return the dangling mirror id")
	assert.NotZero(t, resp.SceneId)

	// Stale membership should have been cleaned up.
	members, _ := sc.Redis.Smembers(sceneMirrorsKey(sourceId))
	assert.NotContains(t, members, fmt.Sprintf("%d", staleMirrorId))
}

// ---------------------------------------------------------------------------
// Mirror co-location — zone-isolation guard
// ---------------------------------------------------------------------------

// TestCreateScene_Mirror_CrossZoneSourceFallsBack ensures a mirror request
// whose source_scene_id resolves to a DIFFERENT zone does NOT co-locate
// onto the source's zone node — that would punch through zone isolation
// and orphan the mirror because the requesting zone's reconciliation loop
// can't see it. Expectation: clean fallback to GetBestNode within the
// requesting zone.
func TestCreateScene_Mirror_CrossZoneSourceFallsBack(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	// Source scene 4242 lives on node "X" in zone 2.
	const otherZone uint32 = 2
	const sourceId uint64 = 4242
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "X")
	sc.Redis.Set(sceneZoneKey(sourceId), fmt.Sprintf("%d", otherZone))
	registerTypedNode(mr, otherZone, "X", constants.SceneNodeTypeInstance, 0)

	// Mirror is requested from zone 1 with only "Y" available.
	registerTypedNode(mr, testZoneId, "Y", constants.SceneNodeTypeInstance, 0)

	resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId:  sourceId,
		MirrorConfigId: 9020,
	})
	require.NoError(t, err)
	assert.Zero(t, resp.ErrorCode)
	assert.Equal(t, "Y", resp.NodeId,
		"cross-zone source must fall back to a node in the requesting zone, not co-locate on X")
}

// TestCreateScene_Mirror_SameZoneSourceColocates is the positive control —
// without it, an over-zealous zone check could break the happy path.
func TestCreateScene_Mirror_SameZoneSourceColocates(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()

	const sourceId uint64 = 5252
	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sourceId), "10")
	sc.Redis.Set(sceneZoneKey(sourceId), fmt.Sprintf("%d", testZoneId))
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeInstance, 0)

	resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId:  sourceId,
		MirrorConfigId: 9021,
	})
	require.NoError(t, err)
	assert.Equal(t, "10", resp.NodeId, "same-zone source must still co-locate on its node")
}

// TestCreateScene_EchoesCreatorIds pins the contract that
// CreateSceneResponse.CreatorIds mirrors CreateSceneRequest.CreatorIds. The
// C++ scene node uses this echo as the entire correlation key for the
// follow-up EnterScene after a mirror is created — losing the echo silently
// breaks the mirror enter-flow without any test in CI screaming.
//
// Covered paths:
//  1. Fresh instance create (non-mirror).
//  2. Fresh mirror create (source_scene_id set).
//  3. Mirror dedup-reuse hit (MirrorDedupBySource=true, existing mirror
//     present) — the reuse response must still echo the *new* request's
//     creators, not the original creator that produced the existing mirror.
//     Otherwise the second caller would never be auto-entered.
func TestCreateScene_EchoesCreatorIds(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	// 1. Plain instance — creators are echoed verbatim.
	alice := []uint64{1001}
	instResp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		CreatorIds:  alice,
	})
	require.NoError(t, err)
	assert.Equal(t, alice, instResp.CreatorIds,
		"instance create must echo CreatorIds")

	// 2. Mirror create — same contract, different code path
	//    (resolveMirrorSourceNode drives node selection but the response
	//    still flows through the same return site).
	bob := []uint64{2002, 2003}
	mirrorResp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId:  instResp.SceneId,
		MirrorConfigId: 9090,
		CreatorIds:     bob,
	})
	require.NoError(t, err)
	assert.Equal(t, bob, mirrorResp.CreatorIds,
		"mirror create must echo CreatorIds")
	require.NotZero(t, mirrorResp.SceneId)

	// 3. Dedup reuse — flip dedup on. The mirror create above already
	//    populated scene:{source}:mirrors + scene:{mirrorId}:node, which
	//    is exactly what findExistingMirror + the dedup branch need.
	//    Second caller is a fresh player; the reuse response must echo
	//    *their* id so the C++ caller drives EnterScene for them, not
	//    for Bob (who already triggered the original create).
	sc.Config.MirrorDedupBySource = true

	carol := []uint64{3030}
	dedupResp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
		SourceSceneId:  instResp.SceneId,
		MirrorConfigId: 9090,
		CreatorIds:     carol,
	})
	require.NoError(t, err)
	assert.Equal(t, mirrorResp.SceneId, dedupResp.SceneId,
		"dedup must reuse the existing mirror")
	assert.Equal(t, carol, dedupResp.CreatorIds,
		"dedup reuse must echo the *new* caller's CreatorIds, not the original")
}

// ---------------------------------------------------------------------------
// Mirror cascade — source on a dying node
// ---------------------------------------------------------------------------

// TestReconcileDeadNode_DestroysCoLocatedMirrors makes sure mirrors that
// share a dead node with their source are reaped by the reconciliation
// loop. Both source (instance) and mirrors live in zone-scoped node scenes sets, so
// reconcile must walk them all instead of stopping at the first source-
// triggered cascade.
//
// The source is itself an instance here (cascade path through
// destroyInstanceInternal handles its mirrors). The test pins that the
// cascade fires regardless of iteration order over the zone-scoped node scenes set.
func TestReconcileDeadNode_DestroysCoLocatedMirrors(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeInstance, 0)

	// Create a parent instance, then mirrors co-located on the same node.
	parent, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType_SCENE_TYPE_INSTANCE,
	})
	require.NoError(t, err)
	require.Equal(t, "10", parent.NodeId)

	var mirrorIds []uint64
	for i := 0; i < 2; i++ {
		m, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scene_manager.CreateSceneRequest{
			SceneConfId:    2001,
			ZoneId:         testZoneId,
			SceneType:      scene_manager.SceneType_SCENE_TYPE_INSTANCE,
			SourceSceneId:  parent.SceneId,
			MirrorConfigId: 9040,
		})
		require.NoError(t, err)
		require.Equal(t, "10", m.NodeId, "mirror %d should co-locate", i)
		mirrorIds = append(mirrorIds, m.SceneId)
	}

	// Kill node 10.
	entry := nodeEntry{nodeID: "10"}
	entry.reg.ZoneId = testZoneId
	entry.reg.SceneNodeType = constants.SceneNodeTypeInstance
	mr.ZRem(nodeLoadKey(testZoneId), "10")
	reconcileDeadNodeScenes(sc, entry)

	for _, sid := range append([]uint64{parent.SceneId}, mirrorIds...) {
		nid, _ := sc.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, sid))
		assert.Equal(t, "", nid, "scene %d must be wiped after node death", sid)
	}
}

// ---------------------------------------------------------------------------
// Concurrency stress: enter / destroy race
// ---------------------------------------------------------------------------

// TestStress_EnterDestroyRace_NoOrphanPlayerCount fires N concurrent enter
// and destroy attempts at the same scene id and asserts that, regardless
// of who wins each race:
//
//  1. instance:{id}:player_count is consistent with scene:{id}:node
//     (either both present, or both absent — no orphan key).
//  2. The atomic CAS in EnterScene only ever increments when the scene
//     still exists (no negative drift of the counter).
//  3. No goroutine panics or returns a non-categorical error.
//
// This is the property the Lua-script CAS was designed to guarantee, and
// regressions usually show up as orphan player_count keys that survive
// scene destruction (the bug that motivated the fix).
func TestStress_EnterDestroyRace_NoOrphanPlayerCount(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const sceneId uint64 = 4242
	const iterations = 200

	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(2)

		// Re-create the scene each iteration so destroy has something to wipe.
		sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneId), "10")

		go func() {
			defer wg.Done()
			_, _ = AtomicIncrPlayerCountIfSceneExists(sc, sceneId)
		}()
		go func() {
			defer wg.Done()
			_, _, _ = AtomicDestroyIfIdle(sc, testZoneId, sceneId)
		}()
	}
	wg.Wait()

	// Final consistency check: if scene:{id}:node is gone, player_count
	// must also be gone (no orphan). If it's present, player_count may
	// hold any non-negative integer.
	nodeId, _ := sc.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, sceneId))
	pcStr, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	if nodeId == "" {
		assert.Equal(t, "", pcStr,
			"orphan player_count detected — atomic CAS regression")
	} else if pcStr != "" {
		var pc int64
		fmt.Sscanf(pcStr, "%d", &pc)
		assert.GreaterOrEqual(t, pc, int64(0), "player_count went negative")
	}
}
