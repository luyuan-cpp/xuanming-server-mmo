package logic

// d:落点会话身份(docs/design/scene-enter-chain-residuals.md §5.2、§5.7)。
//
// 只走 EnterScene 这一个公开入口,断言 Redis 里 location / owner_epoch / 人数的终态与发出的 Kafka 消息。
// 每处生产构造点(常规 guard 的 present / missing、跨区 guard、samePlacement 的身份写)各有一条「观察之后
// location 被改写或删除 → 19」的用例:构造点漏填 expectLoc(零值 = any)时对应用例变红。
// 本文件的辅助标识符统一加 ident 前缀,leavescene_identity_test.go 与 release_dispatch_test.go 也复用它们。

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

const (
	identGenA = uint64(1757000000001)
	identGenB = uint64(1757000000002)
)

// identMarshalLocation 用与生产相同的编码(proto.Marshal)构造 location 原文。
func identMarshalLocation(t *testing.T, loc *scene_manager.PlayerLocation) string {
	t.Helper()
	b, err := gproto.Marshal(loc)
	require.NoError(t, err)
	return string(b)
}

// identSeedLocation 直接写 location 原文,并把 owner_epoch 键设成 location 里记的 epoch(两者一致才是生产会写出的状态)。
func identSeedLocation(t *testing.T, sc *svc.ServiceContext, playerID uint64, loc *scene_manager.PlayerLocation) string {
	t.Helper()
	raw := identMarshalLocation(t, loc)
	require.NoError(t, sc.Redis.Set(getPlayerLocationKey(playerID), raw))
	require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), strconv.FormatUint(loc.OwnerEpoch, 10)))
	return raw
}

// identLocationRaw 读 location 原文;键不存在读作 ""。
func identLocationRaw(t *testing.T, sc *svc.ServiceContext, playerID uint64) string {
	t.Helper()
	raw, err := sc.Redis.Get(getPlayerLocationKey(playerID))
	require.NoError(t, err)
	return raw
}

// identLocation 读并解码 location;不存在返回 nil。
func identLocation(t *testing.T, sc *svc.ServiceContext, playerID uint64) *scene_manager.PlayerLocation {
	t.Helper()
	loc, err := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, err)
	return loc
}

// identValue 读一个计数键;不存在读作 ""。
func identValue(t *testing.T, sc *svc.ServiceContext, key string) string {
	t.Helper()
	v, err := sc.Redis.Get(key)
	require.NoError(t, err)
	return v
}

// identReadOnlySnapshotEval 识别生产原子快照读取，不把「读取前」误当成「提交前」。
// EVALSHA 沿同一生产脚本计算摘要，避免改脚本后测试仍拦截过期文本。
func identReadOnlySnapshotEval(cmd string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	return cmd == "EVAL" && args[0] == luaReadLocationSnapshot ||
		cmd == "EVALSHA" && args[0] == fmt.Sprintf("%x", sha1.Sum([]byte(luaReadLocationSnapshot)))
}

// identHookOnceBeforeEval 在第一条参数里带 key 的写入 EVAL / EVALSHA **执行之前**跑一次 mutate,模拟
// 「本次 EnterScene 观察之后、落点提交之前」的并发写入。用 CompareAndSwap 先消费「只触发一次」的标志:
// Lua 里的 redis.call 会带着 miniredis 的锁重入钩子,那时标志已被消费,走不到 mutate(否则死锁)。
// 与 mr.SetError 共用钩子槽位,装了它的用例不要再用 SetError。返回值供断言「钩子确实触发过」。
func identHookOnceBeforeEval(t *testing.T, mr *miniredis.Miniredis, key string, mutate func()) *atomic.Bool {
	t.Helper()
	var fired atomic.Bool
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if cmd != "EVAL" && cmd != "EVALSHA" {
			return false
		}
		if identReadOnlySnapshotEval(cmd, args) || !slices.Contains(args, key) || !fired.CompareAndSwap(false, true) {
			return false
		}
		mutate()
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	return &fired
}

// placementIdentityCount 从默认注册表读 scene_manager_placement_identity_total 的一个取值;没发射过读作 0。
func placementIdentityCount(t *testing.T, site, verdict string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "scene_manager_placement_identity_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["site"] == site && labels["verdict"] == verdict {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// identSameNodeTwoScenes 摆两个同节点("10")场景:s1 人数 1(玩家在里面),s2 人数 0,节点人数 1。
func identSameNodeTwoScenes(mr *miniredis.Miniredis, s1, s2 uint64) {
	seedSceneOnNode(mr, testZoneId, s1, "10", "1")
	seedSceneOnNode(mr, testZoneId, s2, "10", "0")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "1")
}

// d1:B 走同落点重连先写下 (22, gB);陈旧登录链 A(11, gA < gB)随后要把玩家换到同节点的 S2 → 25,什么都不改。
func TestEnterScene_StaleLoginAfterNewerSamePlacementRejected(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6601)
		s1       = uint64(7601)
		s2       = uint64(7602)
	)
	identSameNodeTwoScenes(mr, s1, s2)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})

	respB, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s1, ZoneId: testZoneId, SessionId: 22, LoginGeneration: identGenB,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), respB.ErrorCode)
	require.Len(t, *captured, 1)
	afterB := identLocationRaw(t, sc, playerID)

	respA, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s2, ZoneId: testZoneId, SessionId: 11, LoginGeneration: identGenA,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-a-uuid",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrPlacementSessionSuperseded, respA.ErrorCode)
	assert.Equal(t, afterB, identLocationRaw(t, sc, playerID), "陈旧登录不得改写 location")
	assert.Equal(t, "1", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)))
	assert.Equal(t, "0", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s2)))
	assert.Equal(t, "1", identValue(t, sc, nodePlayerCountKey(testZoneId, "10")))
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Len(t, *captured, 1, "陈旧登录不得发路由")
}

// d2:location 已属于 (22, gB);旧会话 11 在途的换图(scene 自发,带 CorrelationId 与实体代次 gA)→ 25,什么都没写。
func TestEnterScene_StaleSessionSceneChangeRejected(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6602)
		s1       = uint64(7611)
		s2       = uint64(7612)
	)
	identSameNodeTwoScenes(mr, s1, s2)
	before := identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 22, LoginGeneration: identGenB,
	})
	// reason 写字面量(同 owner_epoch_test.go 的 "scene_gone"):label 值是看板 / 告警查询的契约,在这里钉住。
	staleBefore := enterSceneRejectedCount(t, testZoneId, "session_stale")
	verdictBefore := placementIdentityCount(t, "enter", "stale")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s2, ZoneId: testZoneId, SessionId: 11, LoginGeneration: identGenA,
		CorrelationId: 9001, GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-a-uuid",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrPlacementSessionSuperseded, resp.ErrorCode)
	assert.Equal(t, uint64(9001), resp.CorrelationId, "拒绝应答同样回显关联号,scene 据此对回发起方")
	assert.Equal(t, before, identLocationRaw(t, sc, playerID))
	assert.Equal(t, "1", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)))
	assert.Equal(t, "0", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s2)))
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Empty(t, *captured)
	assert.Equal(t, staleBefore+1, enterSceneRejectedCount(t, testZoneId, "session_stale"))
	assert.Equal(t, verdictBefore+1, placementIdentityCount(t, "enter", "stale"))
}

// 同一会话、请求不带代次(旧版 scene 自发换图)放行,location 代次保持 gB,不被降成 0。
func TestEnterScene_SameSessionWithoutGenerationAdmittedPreservesGeneration(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6603)
		s1       = uint64(7621)
		s2       = uint64(7622)
	)
	identSameNodeTwoScenes(mr, s1, s2)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 22, LoginGeneration: identGenB,
	})

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s2, ZoneId: testZoneId, SessionId: 22, CorrelationId: 9002,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-b-uuid",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, s2, loc.SceneId)
	assert.Equal(t, uint32(22), loc.SessionId)
	assert.Equal(t, identGenB, loc.LoginGeneration, "同会话代次取 max,不因请求不带代次而降级")
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(0), decodeRoutePlayerEvent(t, (*captured)[0]).LoginGeneration, "路由事件仍原样带请求的值")
}

// 混版口径:location 无代次、会话不同、请求也无代次 → legacy 放行,写 (sid_R, 0)。
func TestEnterScene_UnversionedDifferentSessionAdmittedAsLegacy(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)
	const (
		playerID = uint64(6604)
		s1       = uint64(7631)
		s2       = uint64(7632)
	)
	identSameNodeTwoScenes(mr, s1, s2)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100, SessionId: 11,
	})
	legacyBefore := placementIdentityCount(t, "enter", "legacy")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s2, ZoneId: testZoneId, SessionId: 22,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, s2, loc.SceneId)
	assert.Equal(t, uint32(22), loc.SessionId)
	assert.Zero(t, loc.LoginGeneration)
	assert.Equal(t, legacyBefore+1, placementIdentityCount(t, "enter", "legacy"))
}

// legacy 行不降级已有身份:location (22, gB),旧实体发无代次的换图 (11, 0) → 放行,身份仍为 (22, gB)。
func TestEnterScene_LegacyAdmitDoesNotDowngradeIdentity(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)
	const (
		playerID = uint64(6605)
		s1       = uint64(7641)
		s2       = uint64(7642)
	)
	identSameNodeTwoScenes(mr, s1, s2)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 22, LoginGeneration: identGenB,
	})

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s2, ZoneId: testZoneId, SessionId: 11, CorrelationId: 9003,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-a-uuid",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, "location 寿命长于实体:无代次请求不拒,scene 栅栏在入口兜底")
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, s2, loc.SceneId, "落点照常写入")
	assert.Equal(t, uint32(22), loc.SessionId, "身份沿用 L,不降级成 (11, 0)")
	assert.Equal(t, identGenB, loc.LoginGeneration)
}

// samePlacement 的身份写只改 8 / 9 号字段:rollback_receipt / pending / update_time / owner_epoch 原样保留,
// owner_epoch 键与人数不动,路由带观察到的 epoch。
func TestEnterScene_SamePlacementUpdatesIdentityPreservingOtherFields(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6606)
		s1       = uint64(7651)
	)
	seedSceneOnNode(mr, testZoneId, s1, "10", "1")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "1")
	seeded := &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 3, UpdateTime: 12345,
		PendingSceneConfId: 3300, RollbackReceipt: "3:1757000000000",
		SessionId: 11, LoginGeneration: identGenA,
	}
	identSeedLocation(t, sc, playerID, seeded)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s1, ZoneId: testZoneId, SessionId: 22, LoginGeneration: identGenB,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)

	want := gproto.Clone(seeded).(*scene_manager.PlayerLocation)
	want.SessionId = 22
	want.LoginGeneration = identGenB
	got := identLocation(t, sc, playerID)
	require.NotNil(t, got)
	assert.True(t, gproto.Equal(want, got), "只允许 8 / 9 号字段变化: want=%v got=%v", want, got)
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID), "身份写不铸造")
	assert.Equal(t, "1", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)), "身份写不动人数")
	assert.Equal(t, "1", identValue(t, sc, nodePlayerCountKey(testZoneId, "10")))
	require.Len(t, *captured, 1)
	event := decodeRoutePlayerEvent(t, (*captured)[0])
	assert.Equal(t, uint64(3), event.OwnerEpoch)
	assert.Equal(t, identGenB, event.LoginGeneration)
}

// samePlacement 的身份写带 present 原文期望:GET 之后 location 被删(模拟迟到的 LeaveScene 先提交)→ 19、不发路由。
func TestEnterScene_SamePlacementIdentityCASFailsWhenLocationDeleted(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6607)
		s1       = uint64(7661)
	)
	seedSceneOnNode(mr, testZoneId, s1, "10", "1")
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})
	locKey := getPlayerLocationKey(playerID)
	fired := identHookOnceBeforeEval(t, mr, locKey, func() { mr.Del(locKey) })

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s1, ZoneId: testZoneId, SessionId: 22, LoginGeneration: identGenB,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid",
	})
	require.NoError(t, err)
	require.True(t, fired.Load(), "钩子没触发,用例空转")
	assert.Equal(t, constants.ErrOwnerEpochConflict, resp.ErrorCode)
	assert.False(t, mr.Exists(locKey), "身份写不得复活已删的 location")
	assert.Empty(t, *captured, "CAS 失败不发路由")
	assert.Equal(t, "1", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)))
}

// 常规 guard(present):同节点不铸造的落点,观察之后 location 的身份被另一次登录改写 → 19,不覆盖。
func TestEnterScene_PlacementRejectedWhenIdentityChangedSinceObservation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6608)
		s1       = uint64(7671)
		s2       = uint64(7672)
	)
	identSameNodeTwoScenes(mr, s1, s2)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})
	concurrent := identMarshalLocation(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 200,
		SessionId: 22, LoginGeneration: identGenB,
	})
	locKey := getPlayerLocationKey(playerID)
	fired := identHookOnceBeforeEval(t, mr, locKey, func() { _ = mr.Set(locKey, concurrent) })

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s2, ZoneId: testZoneId, SessionId: 11, LoginGeneration: identGenA,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-a-uuid",
	})
	require.NoError(t, err)
	require.True(t, fired.Load(), "钩子没触发,用例空转")
	assert.Equal(t, constants.ErrOwnerEpochConflict, resp.ErrorCode)
	assert.Equal(t, concurrent, identLocationRaw(t, sc, playerID), "后写入的身份不得被覆盖")
	assert.Equal(t, "0", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s2)), "目标场景预占必须退回")
	assert.Equal(t, "1", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)))
	assert.Equal(t, "1", identValue(t, sc, nodePlayerCountKey(testZoneId, "10")))
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Empty(t, *captured)
}

// 常规 guard(missing):观察时没有 location,提交前有人写入了一条 → 首次落点回 19,不铸造、不覆盖。
func TestEnterScene_FirstPlacementRejectedWhenLocationAppearedSinceObservation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6609)
		s1       = uint64(7681)
	)
	seedSceneOnNode(mr, testZoneId, s1, "10", "0")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "0")
	appeared := identMarshalLocation(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, UpdateTime: 200, SessionId: 22, LoginGeneration: identGenB,
	})
	locKey := getPlayerLocationKey(playerID)
	fired := identHookOnceBeforeEval(t, mr, locKey, func() { _ = mr.Set(locKey, appeared) })

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s1, ZoneId: testZoneId, SessionId: 11, LoginGeneration: identGenA,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-a-uuid",
	})
	require.NoError(t, err)
	require.True(t, fired.Load(), "钩子没触发,用例空转")
	assert.Equal(t, constants.ErrOwnerEpochConflict, resp.ErrorCode)
	assert.Equal(t, appeared, identLocationRaw(t, sc, playerID))
	assert.Equal(t, "", ownerEpochRaw(t, sc, playerID), "missing 期望不符时不得铸造")
	assert.Equal(t, "0", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)), "预占必须退回")
	assert.Empty(t, *captured)
}

// 跨区 guard:跨 zone 第一条腿凭标记放行后,观察之后 location 被改写 → 19,不写等待落点、不铸造、不发重定向。
func TestEnterScene_CrossZoneFirstLegRejectedWhenLocationChangedSinceObservation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6610)
		s1       = uint64(7691)
	)
	seedSceneOnNode(mr, 1, s1, "10", "3")
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: 1, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)
	concurrent := identMarshalLocation(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: 1, OwnerEpoch: 1, UpdateTime: 200,
		SessionId: 11, LoginGeneration: identGenB,
	})
	locKey := getPlayerLocationKey(playerID)
	fired := identHookOnceBeforeEval(t, mr, locKey, func() { _ = mr.Set(locKey, concurrent) })

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, ZoneId: 2, SessionId: 11, LoginGeneration: identGenA,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.True(t, fired.Load(), "钩子没触发,用例空转")
	assert.Equal(t, constants.ErrOwnerEpochConflict, resp.ErrorCode)
	assert.Nil(t, resp.Redirect)
	assert.Equal(t, concurrent, identLocationRaw(t, sc, playerID), "不得写等待落点")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "不得铸造")
	assert.Equal(t, "3", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)), "旧场景人数不动")
	assert.Empty(t, *captured, "不发重定向")
}

// samePlacement 写了身份之后推路由失败:回 7,身份**不回滚**(§5.2 ② v0.2),owner_epoch 键与人数不动。
func TestEnterScene_SamePlacementRouteFailureKeepsIdentity(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	sc.Config.KafkaWriteTimeoutSeconds = 1
	const (
		playerID = uint64(6611)
		s1       = uint64(7701)
	)
	seedSceneOnNode(mr, testZoneId, s1, "10", "1")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "1")
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s1, ZoneId: testZoneId, SessionId: 22, LoginGeneration: identGenB,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, uint32(22), loc.SessionId, "kafka-go 报错也可能已投递:身份保留本请求写入的值")
	assert.Equal(t, identGenB, loc.LoginGeneration)
	assert.Equal(t, s1, loc.SceneId)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Equal(t, "1", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)))
	assert.Equal(t, "1", identValue(t, sc, nodePlayerCountKey(testZoneId, "10")))
}

// 跨区第一条腿:陈旧请求(11, gA < gB)在跨区分支之前就被拒,不写等待落点、不铸造、不发重定向。
func TestEnterScene_CrossZoneFirstLegStaleRejected(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6612)
		s1       = uint64(7711)
	)
	seedSceneOnNode(mr, 1, s1, "10", "3")
	before := identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: 1, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 22, LoginGeneration: identGenB,
	})
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)

	logic := NewEnterSceneLogic(context.Background(), sc)
	assigned := false
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		assigned = true
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, ZoneId: 2, SessionId: 11, LoginGeneration: identGenA,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrPlacementSessionSuperseded, resp.ErrorCode)
	assert.Nil(t, resp.Redirect)
	assert.False(t, assigned, "拒绝发生在跨区分支之前,不挑目标 gate")
	assert.Equal(t, before, identLocationRaw(t, sc, playerID))
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Equal(t, "3", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, s1)))
	assert.Empty(t, *captured)
}
