package logic

// c:ReleasePlayer 只在本请求提交之后派发,并带 expected_owner_epoch = 观察值
// (docs/design/scene-enter-chain-residuals.md §4.2、§4.7)。
//
// 派发经包级钩子 releaseDispatcher 同步记录,不靠 sleep;路由与派发的先后用同一条事件序列断言。
// 最后一条用例走真实的 dispatchReleasePlayer + 共享 fake scene node,验证重试带同一个期望值。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// releaseDispatchCall 是一次被钩子截下的派发。locationNodeAtDispatch 是派发那一刻 Redis 里 location 指向的节点,
// 用来证明「落点已提交之后才派发」。
type releaseDispatchCall struct {
	oldZoneID              uint32
	oldNodeID              string
	playerID               uint64
	targetSceneID          uint64
	targetNodeID           string
	expectedOwnerEpoch     uint64
	locationNodeAtDispatch string
}

// releaseDispatchRecorder 记录派发与路由的先后:events 里 "route" 来自 Kafka writer,"release" 来自派发钩子。
type releaseDispatchRecorder struct {
	mu     sync.Mutex
	calls  []releaseDispatchCall
	events []string
}

func (r *releaseDispatchRecorder) snapshot() ([]releaseDispatchCall, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]releaseDispatchCall(nil), r.calls...), append([]string(nil), r.events...)
}

// installReleaseDispatchRecorder 把 releaseDispatcher 换成同步记录器,并给 sc 装一个记录 "route" 事件的 Kafka writer
// (writeErr 非 nil 时每次写都失败)。用例结束还原钩子。
func installReleaseDispatchRecorder(t *testing.T, sc *svc.ServiceContext, writeErr error) *releaseDispatchRecorder {
	t.Helper()
	rec := &releaseDispatchRecorder{}
	prev := releaseDispatcher
	releaseDispatcher = func(svcCtx *svc.ServiceContext, _ logx.Logger, oldZoneID uint32, oldNodeID string,
		playerID uint64, targetSceneID uint64, targetNodeID string, expectedOwnerEpoch uint64) {
		nodeAtDispatch := ""
		if loc, err := GetPlayerLocation(context.Background(), svcCtx, playerID); err == nil && loc != nil {
			nodeAtDispatch = loc.NodeId
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.calls = append(rec.calls, releaseDispatchCall{
			oldZoneID: oldZoneID, oldNodeID: oldNodeID, playerID: playerID,
			targetSceneID: targetSceneID, targetNodeID: targetNodeID,
			expectedOwnerEpoch: expectedOwnerEpoch, locationNodeAtDispatch: nodeAtDispatch,
		})
		rec.events = append(rec.events, "release")
	}
	t.Cleanup(func() { releaseDispatcher = prev })
	sc.Kafka = &countingKafkaWriter{err: writeErr, onWrite: func([]kafka.Message) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.events = append(rec.events, "route")
	}}
	sc.Config.KafkaWriteTimeoutSeconds = 1
	return rec
}

// releaseDispatchCrossNodeFixture:玩家在节点 10 的 oldScene(epoch 1),目标 targetScene 在节点 20。
// withMarker=true 时源 scene 已为 epoch 1 写出落盘标记(换手门放行、落点凭标记铸造)。
func releaseDispatchCrossNodeFixture(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis,
	playerID, oldScene, targetScene uint64, withMarker bool) {
	t.Helper()
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetScene, "20", "4")
	seedNodeMembers(mr, testZoneId, "20", 4)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	adoptSceneMember(mr, oldScene, playerID)
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	if withMarker {
		writeHandoffMarker(t, sc, playerID, 1)
	}
}

func releaseDispatchRequest(playerID, targetScene uint64, withGate bool) *scene_manager.EnterSceneRequest {
	req := &scene_manager.EnterSceneRequest{OwnerRevision: 1, PlayerId: playerID, SceneId: targetScene, ZoneId: testZoneId, GateZoneId: testZoneId}
	if withGate {
		req.GateId = "1"
		req.GateInstanceId = "gate-uuid-test"
	}
	return req
}

// 跨节点凭标记放行、铸出 2 之后推路由失败走 bump 回滚 → 一次都不派发(变体 B5:回滚后源实体采纳 3、解冻继续玩)。
func TestEnterScene_RouteFailureDoesNotDispatchRelease(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	rec := installReleaseDispatchRecorder(t, sc, errors.New("broker unavailable"))
	const (
		playerID    = uint64(6901)
		oldScene    = uint64(7901)
		targetScene = uint64(7902)
	)
	releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, targetScene, true)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(releaseDispatchRequest(playerID, targetScene, true))
	require.NoError(t, err)
	require.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID), "前提:bump 回滚已生效")
	calls, events := rec.snapshot()
	assert.Empty(t, calls, "路由失败与回滚都不得派发 ReleasePlayer")
	assert.Equal(t, []string{"route"}, events)
}

// 第 6 步落点失败(B4,epoch 不动)→ 不派发:源实体解冻后缓存仍等于期望值,只靠「相等才释放」挡不住。
func TestEnterScene_PlacementFailureDoesNotDispatchRelease(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(mr *miniredis.Miniredis, playerID uint64)
		wantCode uint32
	}{
		{
			// 预检之后源 scene 撤回 / 换掉了标记 → 落点 Lua 回 -1。
			name: "handoff_withdrawn",
			mutate: func(mr *miniredis.Miniredis, playerID uint64) {
				_ = mr.Set(ownerepoch.HandoffKey(playerID), "1:1")
			},
			wantCode: constants.ErrHandoffPending,
		},
		{
			// 预检之后并发请求推进了 epoch → 落点 Lua 回 0。
			name: "owner_epoch_conflict",
			mutate: func(mr *miniredis.Miniredis, playerID uint64) {
				_, _ = mr.Incr(ownerepoch.OwnerEpochKey(playerID), 1)
			},
			wantCode: constants.ErrOwnerEpochConflict,
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			rec := installReleaseDispatchRecorder(t, sc, nil)
			playerID := uint64(6911 + i)
			oldScene := uint64(7911 + 10*i)
			targetScene := oldScene + 1
			releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, targetScene, true)
			fired := identHookOnceBeforeEval(t, mr, getPlayerLocationKey(playerID), func() { c.mutate(mr, playerID) })

			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(releaseDispatchRequest(playerID, targetScene, true))
			require.NoError(t, err)
			require.True(t, fired.Load(), "钩子没触发,用例空转")
			assert.Equal(t, c.wantCode, resp.ErrorCode)
			calls, events := rec.snapshot()
			assert.Empty(t, calls, "落点失败不得派发 ReleasePlayer")
			assert.Empty(t, events, "落点失败也不发路由")
		})
	}
}

// 成功交接:派发恰好一次,期望值 = 标记 epoch E(1),不是铸出的 E+1;派发在路由 broker ACK 之后。
func TestEnterScene_ReleaseCarriesObservedOwnerEpoch(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	rec := installReleaseDispatchRecorder(t, sc, nil)
	const (
		playerID    = uint64(6921)
		oldScene    = uint64(7931)
		targetScene = uint64(7932)
	)
	releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, targetScene, true)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(releaseDispatchRequest(playerID, targetScene, true))
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "前提:凭标记铸造了 2")

	calls, events := rec.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, uint64(1), calls[0].expectedOwnerEpoch, "期望值是落点 CAS 用过的观察值,不是铸出值")
	assert.Equal(t, testZoneId, calls[0].oldZoneID)
	assert.Equal(t, "10", calls[0].oldNodeID)
	assert.Equal(t, playerID, calls[0].playerID)
	assert.Equal(t, targetScene, calls[0].targetSceneID)
	assert.Equal(t, "20", calls[0].targetNodeID)
	assert.Equal(t, []string{"route", "release"}, events, "派发必须在路由 broker ACK 之后")
}

// dev 旁路:无标记放行、不铸造;期望值仍是观察值 E,且在路由成功之后才派发。
func TestEnterScene_DevBypassReleaseCarriesUnmintedEpoch(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.AllowUnsafeCrossNodeHandoff = true
	rec := installReleaseDispatchRecorder(t, sc, nil)
	const (
		playerID    = uint64(6931)
		oldScene    = uint64(7941)
		targetScene = uint64(7942)
	)
	releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, targetScene, false)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(releaseDispatchRequest(playerID, targetScene, true))
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "前提:旁路不铸造")

	calls, events := rec.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, uint64(1), calls[0].expectedOwnerEpoch)
	assert.Equal(t, []string{"route", "release"}, events)
}

// 无 GateId:落点成功即提交点,之后派发、带 E;落点失败不派发。
func TestEnterScene_NoGateIdCrossNodeDispatchesAfterPlacement(t *testing.T) {
	t.Run("placement_succeeds", func(t *testing.T) {
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		rec := installReleaseDispatchRecorder(t, sc, nil)
		const (
			playerID    = uint64(6941)
			oldScene    = uint64(7951)
			targetScene = uint64(7952)
		)
		releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, targetScene, true)

		resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(releaseDispatchRequest(playerID, targetScene, false))
		require.NoError(t, err)
		require.Equal(t, uint32(0), resp.ErrorCode)
		calls, events := rec.snapshot()
		require.Len(t, calls, 1)
		assert.Equal(t, uint64(1), calls[0].expectedOwnerEpoch)
		assert.Equal(t, "20", calls[0].locationNodeAtDispatch, "派发时落点必须已提交(location 已指向目标节点)")
		assert.Equal(t, []string{"release"}, events, "无 GateId 不发路由")
	})
	t.Run("placement_fails", func(t *testing.T) {
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		rec := installReleaseDispatchRecorder(t, sc, nil)
		const (
			playerID    = uint64(6942)
			oldScene    = uint64(7961)
			targetScene = uint64(7962)
		)
		releaseDispatchCrossNodeFixture(t, sc, mr, playerID, oldScene, targetScene, true)
		fired := identHookOnceBeforeEval(t, mr, getPlayerLocationKey(playerID), func() {
			_, _ = mr.Incr(ownerepoch.OwnerEpochKey(playerID), 1)
		})

		resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(releaseDispatchRequest(playerID, targetScene, false))
		require.NoError(t, err)
		require.True(t, fired.Load(), "钩子没触发,用例空转")
		assert.Equal(t, constants.ErrOwnerEpochConflict, resp.ErrorCode)
		calls, _ := rec.snapshot()
		assert.Empty(t, calls)
	})
}

// 真实的 dispatchReleasePlayer:首发与两次重试中前两次回 Unavailable,第三次成功;三次请求的期望值相同。
// 退避缩短到毫秒级,等待用有上限的 Eventually,不靠固定 sleep。
func TestDispatchReleasePlayer_RetriesCarrySameExpectedEpoch(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(6951)
		targetScene = uint64(7971)
		expected    = uint64(7)
	)
	fake := installDefaultFakeSceneNode(t)
	fake.setReleaseFailNext(playerID, 2)
	t.Cleanup(func() { fake.setReleaseFailNext(0, 0) })
	prevBackoffs := releasePlayerRetryBackoffs
	releasePlayerRetryBackoffs = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { releasePlayerRetryBackoffs = prevBackoffs })

	dispatchReleasePlayer(sc, logx.WithContext(context.Background()), testZoneId, "10", playerID, targetScene, "20", expected)

	require.Eventually(t, func() bool { return len(fake.releaseRequestsFor(playerID)) >= 3 },
		5*time.Second, 10*time.Millisecond, "首发失败后应重试到第三次")
	requests := fake.releaseRequestsFor(playerID)
	require.Len(t, requests, 3, "第三次成功后不再重试")
	for i, req := range requests {
		assert.Equal(t, expected, req.GetExpectedOwnerEpoch(), "第 %d 次请求", i+1)
		assert.Equal(t, targetScene, req.GetTargetSceneId())
		assert.Equal(t, "20", req.GetTargetNodeId())
	}
}
