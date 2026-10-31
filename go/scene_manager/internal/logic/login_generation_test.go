package logic

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"shared/redisslot"
)

// ---------------------------------------------------------------------------
// 登录代次(login_generation)在 scene_manager 只透传、不校验、不进去重指纹
// (scene-enter-session-fence.md §4、§9 残余 2、§10「透传与指纹」)。
//
// 真正的裁决在 scene 的入场栅栏;scene_manager 这里要守住的只有两件事:
//   1. 请求带来的代次原样进 RoutePlayerEvent —— 同节点顶号时 owner_epoch 不变,
//      scene 只能靠这个值区分新旧两次登录;
//   2. 同一 request_id 仅代次不同(login 重走登录链会铸新值)仍是同一逻辑操作,
//      走重放 / 进行中,不能判成 IdempotencyConflict。
// ---------------------------------------------------------------------------

func TestEnterScene_RouteEventCarriesRequestLoginGenerationAcrossSamePlacementTakeover(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6401)
		sceneID  = uint64(7401)
		genA     = uint64(1757000000001)
		genB     = uint64(1757000000002)
	)
	seedSceneOnNode(mr, testZoneId, sceneID, "10", "0")

	// 设备 A 首登:走首次落点(铸 epoch 1)那一处 routePlayerToGate。
	respA, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId, SessionId: 11,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-a-uuid", LoginGeneration: genA,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), respA.ErrorCode)

	// 设备 B 从另一个 gate 顶号、落到同一场景同一节点:走"同一落点重连"那一处 routePlayerToGate。
	respB, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId, SessionId: 22,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid", LoginGeneration: genB,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), respB.ErrorCode)

	// scene 自发的重路由(交接重发、同图换图等)不带代次:scene_manager 不得替它补值,
	// 盖章由 gate 按会话值做(§4「scene 自发的 EnterScene」)。
	respC, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId, SessionId: 22,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), respC.ErrorCode)

	require.Len(t, *captured, 3)
	eventA := decodeRoutePlayerEvent(t, (*captured)[0])
	eventB := decodeRoutePlayerEvent(t, (*captured)[1])
	eventC := decodeRoutePlayerEvent(t, (*captured)[2])

	assert.Equal(t, uint64(1), eventA.OwnerEpoch)
	assert.Equal(t, eventA.OwnerEpoch, eventB.OwnerEpoch,
		"同节点顶号不换持有节点,owner_epoch 不变 —— 它区分不了两次登录,这正是要带登录代次的原因")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))

	assert.Equal(t, uint32(11), eventA.SessionId)
	assert.Equal(t, genA, eventA.LoginGeneration, "首次落点的路由必须原样带请求的代次")
	assert.Equal(t, uint32(22), eventB.SessionId)
	assert.Equal(t, genB, eventB.LoginGeneration, "同一落点重连的路由必须原样带本次请求的代次,而不是上一次的")
	assert.Equal(t, uint64(0), eventC.LoginGeneration, "请求不带代次时原样发 0,scene_manager 不补值")
}

func TestEnterSceneFingerprint_IgnoresLoginGeneration(t *testing.T) {
	base := &scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: 6402, SceneId: 7402, ZoneId: testZoneId, SessionId: 33, RequestId: "fp-login-gen",
	}
	want, err := enterSceneRequestFingerprint(base)
	require.NoError(t, err)

	for _, gen := range []uint64{1, 1757000000000} {
		req := gproto.Clone(base).(*scene_manager.EnterSceneRequest)
		req.LoginGeneration = gen
		got, fpErr := enterSceneRequestFingerprint(req)
		require.NoError(t, fpErr)
		assert.Equal(t, want, got, "login_generation=%d 不得改变指纹", gen)
		assert.Equal(t, gen, req.LoginGeneration, "算指纹不得原地修改入参")
	}

	// 两个不进指纹的字段同时出现,也只清副本。
	both := gproto.Clone(base).(*scene_manager.EnterSceneRequest)
	both.LoginGeneration = 42
	both.CorrelationId = 7
	got, err := enterSceneRequestFingerprint(both)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, uint64(42), both.LoginGeneration)
	assert.Equal(t, uint64(7), both.CorrelationId)
}

func TestEnterScene_SameRequestIDDifferentLoginGenerationReplaysCachedSuccess(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	captured := capturingKafkaWriter(sc)

	const targetID = uint64(7403)
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.Set(fmt.Sprintf(SceneNodeKeyFmt, targetID), "10")
	seedSceneMembers(mr, targetID, 0)
	first := &scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: 6403, SceneId: targetID, ZoneId: testZoneId, SessionId: 44,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
		RequestId: "login-gen-replay", LoginGeneration: 100,
	}

	firstResp, err := NewEnterSceneLogic(ctx, sc).EnterScene(first)
	require.NoError(t, err)
	require.Equal(t, uint32(0), firstResp.ErrorCode)
	require.Len(t, *captured, 1)

	// 删除目标映射:若第二次还进入解析路径就会失败;成功只能来自完整响应重放。
	mr.Del(fmt.Sprintf(SceneNodeKeyFmt, targetID))
	retry := gproto.Clone(first).(*scene_manager.EnterSceneRequest)
	retry.LoginGeneration = 200
	retryResp, err := NewEnterSceneLogic(ctx, sc).EnterScene(retry)
	require.NoError(t, err)
	assert.NotEqual(t, constants.ErrEnterSceneIdempotencyConflict, retryResp.ErrorCode,
		"只差 login_generation 的同一 request_id 是同一逻辑操作,不是冲突")
	require.Equal(t, uint32(0), retryResp.ErrorCode)
	assert.True(t, gproto.Equal(firstResp, retryResp), "必须是首次成功应答的完整重放")

	// 重放不补发路由:scene 上的已放行代次允许落后于会话记录(G4),新代次由 Bind 带给 gate。
	require.Len(t, *captured, 1, "重放不得再次发送 Gate 路由")
	assert.Equal(t, uint64(100), decodeRoutePlayerEvent(t, (*captured)[0]).LoginGeneration)
	count := sceneCountText(sc, targetID)
	assert.Equal(t, "1", count, "重放不得再次增加目标场景人数")
}

func TestEnterScene_SameRequestIDDifferentLoginGenerationIsInProgressNotConflict(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	base := &scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: 6404, SceneId: 7404, ZoneId: testZoneId, SessionId: 55,
		RequestId: "login-gen-in-progress", LoginGeneration: 300,
	}
	fingerprint, err := enterSceneRequestFingerprint(base)
	require.NoError(t, err)
	key := redisslot.EnterSceneDedupKey(base.PlayerId, base.RequestId)
	owner := "pending:" + fingerprint + ":first-owner"
	mr.Set(key, owner)
	mr.SetTTL(key, time.Minute)

	retry := gproto.Clone(base).(*scene_manager.EnterSceneRequest)
	retry.LoginGeneration = 301
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(retry)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrEnterSceneInProgress, resp.ErrorCode,
		"只差 login_generation 的同一 request_id 仍在执行时应回进行中,不是冲突")
	value, getErr := sc.Redis.Get(key)
	require.NoError(t, getErr)
	assert.Equal(t, owner, value, "并发命中者不得删除首个请求的 owner")
}
