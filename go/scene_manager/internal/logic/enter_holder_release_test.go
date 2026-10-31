package logic

// 登录撞上未释放的持有者:EnterScene 同 zone 跨节点换手门以 18 拒绝之后,同步请持有节点按会话结束释放
// (docs/design/location-release-binding.md §12)。
// 夹具沿用 enterscene_identity_test.go(ident*)、release_dispatch_test.go(installReleaseDispatchRecorder)、
// leave_release_test.go(installLeaveReleaseRecorder)与 leavescene_cas_test.go(leaveCasValue);
// 本文件的辅助标识符统一加 enterHolderRelease 前缀。
//
// 场景由请求显式指定(SceneId = 节点 20 上的 targetScene):login 的真实请求不带场景、由 scene_manager 按负载选频道,
// 被派到别的节点是这条残余的前提;用指定场景把「被派到别的节点」固定下来,判定本身与场景怎么选出来的无关。

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// enterHolderReleaseCall 是一次被钩子截下的同步释放请求。locationNodeAtCall 是请求那一刻 Redis 里 location 指向的节点,
// 用来证明请求发生在任何落点写之前(18 一个字节都不改)。
type enterHolderReleaseCall struct {
	zoneID             uint32
	nodeID             string
	playerID           uint64
	expectedOwnerEpoch uint64
	covered            placementIdentity // ReleasePlayerRequest.covered_*(§13)
	locationNodeAtCall string
}

// installEnterHolderReleaseRecorder 把 enterHolderReleaseRequester 换成同步记录器,每次返回 replyErr;用例结束还原。
func installEnterHolderReleaseRecorder(t *testing.T, replyErr error) func() []enterHolderReleaseCall {
	t.Helper()
	var mu sync.Mutex
	var calls []enterHolderReleaseCall
	prev := enterHolderReleaseRequester
	enterHolderReleaseRequester = func(_ context.Context, svcCtx *svc.ServiceContext, zoneID uint32, nodeID string,
		playerID uint64, expected uint64, covered placementIdentity) error {
		nodeAtCall := ""
		if loc, err := GetPlayerLocation(context.Background(), svcCtx, playerID); err == nil && loc != nil {
			nodeAtCall = loc.NodeId
		}
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, enterHolderReleaseCall{zoneID: zoneID, nodeID: nodeID, playerID: playerID,
			expectedOwnerEpoch: expected, covered: covered, locationNodeAtCall: nodeAtCall})
		return replyErr
	}
	t.Cleanup(func() { enterHolderReleaseRequester = prev })
	return func() []enterHolderReleaseCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]enterHolderReleaseCall(nil), calls...)
	}
}

// enterHolderReleaseCount 从默认注册表读 scene_manager_enter_holder_release_total 的一个取值;没发射过读作 0。
// 计数是进程级的,用例一律比较前后增量。
func enterHolderReleaseCount(t *testing.T, verdict string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "scene_manager_enter_holder_release_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == "verdict" && pair.GetValue() == verdict {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// enterHolderReleaseZombie 是 location 里记的旧会话身份:gate 崩溃前的那次登录。
var enterHolderReleaseZombie = placementIdentity{sessionID: 11, loginGeneration: identGenA}

// enterHolderReleaseSeed 摆一条持有节点 "10" 的 holderScene 上、owner_epoch = 3、身份为 locIdentity 的 location
// (owner_epoch 键同值,没有 handoff 标记),目标 targetScene 在节点 "20"。装好交接释放与路由记录器:放行时第 7b 步
// 不发真实 RPC,带 GateId 的请求有 Kafka 可写。返回交接释放记录器。
func enterHolderReleaseSeed(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis,
	playerID, holderScene, targetScene uint64, locIdentity placementIdentity) *releaseDispatchRecorder {
	t.Helper()
	transfer := installReleaseDispatchRecorder(t, sc, nil)
	seedSceneOnNode(mr, testZoneId, holderScene, "10", "1")
	seedSceneOnNode(mr, testZoneId, targetScene, "20", "0")
	seedNodeMembers(mr, testZoneId, "10", 1)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: holderScene, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 3, UpdateTime: 100,
		SessionId: locIdentity.sessionID, LoginGeneration: locIdentity.loginGeneration,
	})
	adoptSceneMember(mr, holderScene, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)
	return transfer
}

// enterHolderReleaseLogin 是从新 gate 重登的 EnterScene:会话 22、代次 identGenB(比 location 记的新),不带关联号(login 发起)。
func enterHolderReleaseLogin(playerID, targetScene uint64) *scene_manager.EnterSceneRequest {
	return &scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetScene, ZoneId: testZoneId, SessionId: 22, LoginGeneration: identGenB,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid",
	}
}

// requireHandoffRejectedUnchanged:回 18,且 location / owner_epoch 与摆好的一致(拒绝路径一个字节都不改)。
func requireHandoffRejectedUnchanged(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, playerID uint64,
	resp *scene_manager.EnterSceneResponse, err error, locIdentity placementIdentity) {
	t.Helper()
	require.NoError(t, err)
	require.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "location 指向未释放的持有者:跨节点落点必须被换手门拒绝")
	assert.Equal(t, "3", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)), "拒绝不铸造")
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, locIdentity, identityOfLocation(loc), "拒绝不改 location 身份")
}

// ── 纯判定 ──────────────────────────────────────────────────────────────────

func TestDecideEnterHolderRelease_Table(t *testing.T) {
	eligible := enterHolderReleaseFacts{enabled: true, loginRequest: true, identityNewer: true, epoch: 3,
		holderAddressable: true, chainAlive: true}
	cases := []struct {
		name   string
		mutate func(*enterHolderReleaseFacts)
		want   enterHolderReleaseVerdict
	}{
		{"all conditions hold", func(*enterHolderReleaseFacts) {}, enterHolderReleaseEligible},
		{"switch off wins over everything", func(f *enterHolderReleaseFacts) { f.enabled = false }, enterHolderReleaseDisabled},
		{"scene-initiated request", func(f *enterHolderReleaseFacts) { f.loginRequest = false }, enterHolderReleaseNotLogin},
		{"scene-initiated is decided before identity and chain", func(f *enterHolderReleaseFacts) {
			f.loginRequest, f.identityNewer, f.chainAlive = false, false, false
		}, enterHolderReleaseNotLogin},
		{"identity not strictly newer", func(f *enterHolderReleaseFacts) { f.identityNewer = false }, enterHolderReleaseIdentityNotNewer},
		{"identity is decided before epoch", func(f *enterHolderReleaseFacts) { f.identityNewer, f.epoch = false, 0 },
			enterHolderReleaseIdentityNotNewer},
		{"unminted epoch", func(f *enterHolderReleaseFacts) { f.epoch = 0 }, enterHolderReleaseLegacyEpochZero},
		{"holder not addressable", func(f *enterHolderReleaseFacts) { f.holderAddressable = false }, enterHolderReleaseUnaddressable},
		{"login chain already gone", func(f *enterHolderReleaseFacts) { f.chainAlive = false }, enterHolderReleaseChainGone},
		{"zero value never dispatches", func(f *enterHolderReleaseFacts) { *f = enterHolderReleaseFacts{} }, enterHolderReleaseDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := eligible
			tc.mutate(&facts)
			assert.Equal(t, tc.want, decideEnterHolderRelease(facts))
		})
	}
}

func TestEnterHolderReleaseVerdictNamesCoverEveryValue(t *testing.T) {
	seen := map[string]bool{enterHolderReleaseOutcomeDelivered: true, enterHolderReleaseOutcomeFailed: true}
	for v := enterHolderReleaseVerdict(0); v < enterHolderReleaseVerdictCount; v++ {
		name := v.String()
		require.NotEmpty(t, name, "verdict %d 没有名字", v)
		require.NotEqual(t, "unknown", name)
		require.False(t, seen[name], "名字 %q 重复(含 delivered / failed 两个结果标签)", name)
		seen[name] = true
	}
	assert.Equal(t, "unknown", enterHolderReleaseVerdictCount.String())
}

// identityStrictlyNewer 只认「location 记着代次、请求的代次更大、会话不同」:newer 里「location 无代次」那一行按 legacy 处理。
func TestIdentityStrictlyNewer(t *testing.T) {
	cases := []struct {
		name string
		loc  placementIdentity
		req  placementIdentity
		want bool
	}{
		{"newer login replaces the location session", enterHolderReleaseZombie, placementIdentity{22, identGenB}, true},
		{"location without generation", placementIdentity{11, 0}, placementIdentity{22, identGenB}, false},
		{"same session re-entry", enterHolderReleaseZombie, placementIdentity{11, identGenB}, false},
		{"request without generation", enterHolderReleaseZombie, placementIdentity{22, 0}, false},
		{"location without identity", placementIdentity{}, placementIdentity{22, identGenB}, false},
		{"stale login", placementIdentity{11, identGenB}, placementIdentity{22, identGenA}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, _ := decidePlacementIdentity(tc.loc, true, tc.req)
			assert.Equal(t, tc.want, identityStrictlyNewer(verdict, tc.loc), "placement verdict=%s", verdict)
		})
	}
}

// ── EnterScene 入口 ─────────────────────────────────────────────────────────

// 主场景:gate 崩溃后从别的 gate 重登,被派到别的节点。回 18 之前请持有节点释放**一次**(期望 = 观察到的 epoch 3),
// 走的是同步钩子而不是 LeaveScene 的异步钩子;持有节点写出 A1′ 之后的重登放行并铸造,放行路径不再请会话结束释放。
func TestEnterScene_LoginBehindUnreleasedHolderRequestsReleaseOnce(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(8741)
		holderScene = uint64(9641)
		targetScene = uint64(9642)
	)
	transfer := enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	requested := installEnterHolderReleaseRecorder(t, nil)
	leaveRequested := installLeaveReleaseRecorder(t)
	deliveredBefore := enterHolderReleaseCount(t, enterHolderReleaseOutcomeDelivered)
	login := enterHolderReleaseLogin(playerID, targetScene)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(login)
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	calls := requested()
	require.Len(t, calls, 1, "登录撞上未释放的持有者:回 18 之前请持有节点释放一次")
	assert.Equal(t, enterHolderReleaseCall{zoneID: testZoneId, nodeID: "10", playerID: playerID,
		expectedOwnerEpoch: 3, covered: placementIdentity{22, identGenB}, locationNodeAtCall: "10"}, calls[0],
		"覆盖到这次登录自己的身份,不是 location 记的旧身份(§13.2)")
	assert.Empty(t, leaveRequested(), "EnterScene 走同步请求,不走 LeaveScene 的异步派发")
	assert.Equal(t, deliveredBefore+1, enterHolderReleaseCount(t, enterHolderReleaseOutcomeDelivered))
	assert.Equal(t, "0", sceneMemberCount(mr, targetScene), "拒绝不预占目标场景人数")

	// 持有节点按断线退出、存盘落地后 A1′ 写出当前代标记 → 重登放行并铸造 4,location 指向节点 20 与新登录。
	writeHandoffMarker(t, sc, playerID, 3)
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(login)
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "4", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, "20", loc.NodeId)
	assert.Equal(t, placementIdentity{22, identGenB}, identityOfLocation(loc))
	assert.Len(t, requested(), 1, "放行路径不请会话结束释放")
	transferCalls, _ := transfer.snapshot()
	require.Len(t, transferCalls, 1, "放行后照常通知旧节点(交接释放)")
	assert.Equal(t, "10", transferCalls[0].oldNodeID)
	assert.Equal(t, uint64(3), transferCalls[0].expectedOwnerEpoch)
}

// 不是当前代的标记(上一次换图留下的 E-1 标记还在 TTL 内)与没有标记同理:持有者还没释放 E,照样请。
func TestEnterScene_LoginBehindStaleMarkerAlsoRequestsRelease(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(8742)
		holderScene = uint64(9643)
		targetScene = uint64(9644)
	)
	enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	writeHandoffMarker(t, sc, playerID, 2)
	requested := installEnterHolderReleaseRecorder(t, nil)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(enterHolderReleaseLogin(playerID, targetScene))
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	calls := requested()
	require.Len(t, calls, 1)
	assert.Equal(t, uint64(3), calls[0].expectedOwnerEpoch, "期望取观察到的当前代,不取标记里的旧代")
}

// scene 自发的请求(带关联号)被 18 拒:持有者就是发起方,收到 18 自己冻结、存盘、写标记,一律不请 ——
// 第一跳还没冻结,请了等于把正在换图的玩家按断线踢下线。只看来源:即使身份字段看起来更新也不请。
func TestEnterScene_SceneInitiatedRejectionDoesNotRequestRelease(t *testing.T) {
	cases := []struct {
		name string
		req  func(playerID, targetScene uint64) *scene_manager.EnterSceneRequest
	}{
		{"holder's own first hop", func(playerID, targetScene uint64) *scene_manager.EnterSceneRequest {
			req := enterHolderReleaseLogin(playerID, targetScene)
			req.SessionId, req.LoginGeneration, req.CorrelationId = 11, identGenA, 7
			return req
		}},
		{"correlated request with newer identity", func(playerID, targetScene uint64) *scene_manager.EnterSceneRequest {
			req := enterHolderReleaseLogin(playerID, targetScene)
			req.CorrelationId = 8
			return req
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			playerID := uint64(8743) + uint64(i)*10
			holderScene, targetScene := uint64(9645)+uint64(i)*10, uint64(9646)+uint64(i)*10
			enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
			requested := installEnterHolderReleaseRecorder(t, nil)
			before := enterHolderReleaseCount(t, "not_login")

			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(tc.req(playerID, targetScene))
			requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
			assert.Empty(t, requested(), "scene 自发的请求不请会话结束释放")
			assert.Equal(t, before+1, enterHolderReleaseCount(t, "not_login"))
		})
	}
}

// 登录请求,但落点身份不是严格更新:证明不了 location 的会话已被这次登录取代,只等不请。
func TestEnterScene_LoginWithoutStrictlyNewerIdentityDoesNotRequestRelease(t *testing.T) {
	cases := []struct {
		name        string
		locIdentity placementIdentity
		mutate      func(*scene_manager.EnterSceneRequest)
	}{
		// LeaveGame 后同连接重进:同一会话,gate 收到 LeaveGame 时已代发 ExitGame。
		{"same session re-entry", placementIdentity{22, identGenA}, func(*scene_manager.EnterSceneRequest) {}},
		// 旧版 login:请求没有代次,location 可能属于一次更新的登录。
		{"request without generation", enterHolderReleaseZombie, func(req *scene_manager.EnterSceneRequest) { req.LoginGeneration = 0 }},
		// 旧版写入方留下的 location:没有代次可比。
		{"location without generation", placementIdentity{11, 0}, func(*scene_manager.EnterSceneRequest) {}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			playerID := uint64(8745) + uint64(i)*10
			holderScene, targetScene := uint64(9647)+uint64(i)*10, uint64(9648)+uint64(i)*10
			enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, tc.locIdentity)
			requested := installEnterHolderReleaseRecorder(t, nil)
			before := enterHolderReleaseCount(t, "identity_not_newer")
			req := enterHolderReleaseLogin(playerID, targetScene)
			tc.mutate(req)

			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(req)
			requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, tc.locIdentity)
			assert.Empty(t, requested(), "身份不是严格更新时不请会话结束释放")
			assert.Equal(t, before+1, enterHolderReleaseCount(t, "identity_not_newer"))
		})
	}
}

// 发起的登录链已经放弃(login 超时 / 丢锁取消链 → 服务端请求上下文结束):它不再持玩家锁,可能已有更新的登录链在跑,不替它请。
func TestEnterScene_LoginReleaseSkippedWhenChainContextDone(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(8781)
		holderScene = uint64(9681)
		targetScene = uint64(9682)
	)
	enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	requested := installEnterHolderReleaseRecorder(t, nil)
	before := enterHolderReleaseCount(t, "chain_gone")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := NewEnterSceneLogic(ctx, sc).EnterScene(enterHolderReleaseLogin(playerID, targetScene))
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	assert.Empty(t, requested())
	assert.Equal(t, before+1, enterHolderReleaseCount(t, "chain_gone"))
}

// 旧负预算配置不能关掉安全恢复：最新登录撞门仍请原持有者按原会话释放。
func TestEnterScene_NegativeWaitBudgetStillRequestsHolderRelease(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.LeaveSceneHolderReleaseWaitSeconds = -1
	const (
		playerID    = uint64(8782)
		holderScene = uint64(9683)
		targetScene = uint64(9684)
	)
	enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	requested := installEnterHolderReleaseRecorder(t, nil)
	before := enterHolderReleaseCount(t, "delivered")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(enterHolderReleaseLogin(playerID, targetScene))
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	assert.Len(t, requested(), 1)
	assert.Equal(t, before+1, enterHolderReleaseCount(t, "delivered"))
}

// 持有节点不可达 / 超时:照回 18、什么都不改,计 failed;下一次重登再请。
func TestEnterScene_LoginReleaseFailureStillRejectsUnchanged(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(8783)
		holderScene = uint64(9685)
		targetScene = uint64(9686)
	)
	enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	requested := installEnterHolderReleaseRecorder(t, errors.New("holder unreachable"))
	before := enterHolderReleaseCount(t, enterHolderReleaseOutcomeFailed)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(enterHolderReleaseLogin(playerID, targetScene))
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	assert.Len(t, requested(), 1)
	assert.Equal(t, before+1, enterHolderReleaseCount(t, enterHolderReleaseOutcomeFailed))
}

// 真实的 RequestNodeReleaseEndedSession + 共享 fake scene node:请求在 EnterScene 返回之前已到达持有节点(同步),
// 带 session_ended、期望 = 观察到的 epoch,没有接手节点。
func TestEnterScene_LoginReleaseReachesHolderAsSessionEnded(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(8784)
		holderScene = uint64(9687)
		targetScene = uint64(9688)
	)
	enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	fake := installDefaultFakeSceneNode(t)
	before := len(fake.releaseRequestsFor(playerID))

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(enterHolderReleaseLogin(playerID, targetScene))
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	requests := fake.releaseRequestsFor(playerID)
	require.Len(t, requests, before+1, "同步请求:EnterScene 返回时持有节点已收到")
	got := requests[len(requests)-1]
	assert.True(t, got.GetSessionEnded())
	assert.Equal(t, uint64(3), got.GetExpectedOwnerEpoch())
	assert.Zero(t, got.GetTargetSceneId())
	assert.Empty(t, got.GetTargetNodeId())
	// §13:线上带着这次登录的身份,持有节点据此不退已绑上更新登录的实体。
	assert.Equal(t, uint32(22), got.GetCoveredSessionId())
	assert.Equal(t, identGenB, got.GetCoveredLoginGeneration())
}
