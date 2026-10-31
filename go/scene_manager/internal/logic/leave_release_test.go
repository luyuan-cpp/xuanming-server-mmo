package logic

// 删 location 绑定「持有者已释放」(docs/design/location-release-binding.md §3.2、§8)。
// 夹具沿用 leavescene_cas_test.go(leaveCas*)、enterscene_identity_test.go(ident*)与 release_dispatch_test.go
// (installReleaseDispatchRecorder);本文件的辅助标识符统一加 leaveRelease 前缀。

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// leaveReleaseCall 是一次被钩子截下的 session_ended 释放派发。
type leaveReleaseCall struct {
	zoneID             uint32
	nodeID             string
	playerID           uint64
	expectedOwnerEpoch uint64
	covered            placementIdentity // ReleasePlayerRequest.covered_*(§13)
}

// installLeaveReleaseRecorder 把 sessionEndedReleaseDispatcher 换成同步记录器,用例结束还原。
func installLeaveReleaseRecorder(t *testing.T) func() []leaveReleaseCall {
	t.Helper()
	var mu sync.Mutex
	var calls []leaveReleaseCall
	prev := sessionEndedReleaseDispatcher
	sessionEndedReleaseDispatcher = func(_ *svc.ServiceContext, zoneID uint32, nodeID string, playerID uint64, expected uint64,
		covered placementIdentity) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, leaveReleaseCall{zoneID: zoneID, nodeID: nodeID, playerID: playerID, expectedOwnerEpoch: expected,
			covered: covered})
	}
	t.Cleanup(func() { sessionEndedReleaseDispatcher = prev })
	return func() []leaveReleaseCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]leaveReleaseCall(nil), calls...)
	}
}

// leaveReleaseCount 从默认注册表读 scene_manager_leave_holder_release_total 的一个取值;没发射过读作 0。
func leaveReleaseCount(t *testing.T, verdict string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "scene_manager_leave_holder_release_total" {
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

// leaveReleaseSeed 摆一条持有节点 "10" 上、owner_epoch = epoch、身份 (11, identGenA) 的 location,
// 场景与节点人数都是 "2",owner_epoch 键写 epoch(0 = 不写键)。返回 location 原文。
func leaveReleaseSeed(t *testing.T, mr *miniredis.Miniredis, playerID, sceneID uint64, nodeID string, epoch uint64) string {
	t.Helper()
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: sceneID, NodeId: nodeID, ZoneId: testZoneId, OwnerEpoch: epoch, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})
	seedLeaveCasFixture(t, mr, playerID, sceneID, raw, "2")
	// 持有节点活着:zone 的负载集里有它。不摆的话 playerLocationOwnerGone 会把"整个 zone 没有节点"判成持有者已消失,直接删。
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	// seedLeaveCasFixture 默认带当前代的释放标记(它服务的用例只测删除机制);本文件要自己摆证据,先撤掉。
	mr.Del(ownerepoch.HandoffKey(playerID))
	mr.Del(ownerepoch.OwnerEpochKey(playerID))
	if epoch != 0 {
		require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), strconv.FormatUint(epoch, 10)))
	}
	return raw
}

// leaveReleaseRequest 是离场会话 (11, identGenA) 的 LeaveScene:与 location 身份相同,判 deleted。
func leaveReleaseRequest(playerID, sceneID uint64) *scene_manager.LeaveSceneRequest {
	return &scene_manager.LeaveSceneRequest{PlayerId: playerID, SceneId: sceneID, SessionId: 11, LoginGeneration: identGenA}
}

func requireHolderReleasePending(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "拿不到释放证据必须回 FailedPrecondition,不是故障码")
}

func TestDecideHolderRelease_Table(t *testing.T) {
	base := holderReleaseFacts{enabled: true, epoch: 7, identityProven: true}
	cases := []struct {
		name  string
		facts holderReleaseFacts
		want  leaveReleaseVerdict
	}{
		{"旧开关不覆盖真实marker正证", holderReleaseFacts{epoch: 7, markerReleased: true}, leaveReleaseReleased},
		{"epoch缺失不是释放证明", holderReleaseFacts{enabled: true, identityProven: true}, leaveReleasePendingUnproven},
		{"no holder", func() holderReleaseFacts { f := base; f.noHolder = true; return f }(), leaveReleaseNoHolder},
		{"owner dead", func() holderReleaseFacts { f := base; f.ownerDead = true; return f }(), leaveReleaseOwnerDead},
		{"marker releases", func() holderReleaseFacts { f := base; f.markerReleased = true; return f }(), leaveReleaseReleased},
		{"marker beats wait budget", func() holderReleaseFacts {
			f := base
			f.markerReleased, f.waitExceeded = true, true
			return f
		}(), leaveReleaseReleased},
		{"等满仍保留UNKNOWN", func() holderReleaseFacts { f := base; f.waitExceeded = true; return f }(), leaveReleasePending},
		{"pending with proven identity", base, leaveReleasePending},
		{"pending without proven identity", func() holderReleaseFacts { f := base; f.identityProven = false; return f }(), leaveReleasePendingUnproven},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, decideHolderRelease(tc.facts))
		})
	}
}

func TestLeaveReleaseVerdictNamesCoverEveryValue(t *testing.T) {
	seen := map[string]bool{}
	for v := leaveReleaseVerdict(0); v < leaveReleaseVerdictCount; v++ {
		name := v.String()
		require.NotEmpty(t, name)
		require.NotEqual(t, "unknown", name)
		require.False(t, seen[name], "名字重复: %s", name)
		seen[name] = true
	}
	assert.Equal(t, "unknown", leaveReleaseVerdictCount.String())
	// 删除类与等待类互斥且覆盖全部结论。
	for v := leaveReleaseVerdict(0); v < leaveReleaseVerdictCount; v++ {
		assert.NotEqual(t, v.deletes(), v.waits(), "结论 %s 必须恰好是删除类或等待类之一", v)
	}
}

func TestMarkerReleasesEpoch(t *testing.T) {
	assert.True(t, markerReleasesEpoch("7:1700000000000", 7))
	assert.False(t, markerReleasesEpoch("6:1700000000000", 7), "旧代标记不是释放证据")
	assert.False(t, markerReleasesEpoch("8:1700000000000", 7), "更新的一代的标记不证明 7 已释放")
	assert.False(t, markerReleasesEpoch("", 7))
	assert.False(t, markerReleasesEpoch("garbage", 7))
	assert.False(t, markerReleasesEpoch("0:1", 0), "epoch 0 不认任何标记")
}

// leaveReleaseRedisNow 是用例给 miniredis 定的服务端时钟(脚本里的 TIME)。等待计时只读 Redis 的时钟(第二轮审计 r2-sm#2),
// 定死它之后计时类用例不依赖真实墙钟。
var leaveReleaseRedisNow = time.UnixMilli(1_790_000_000_000)

// 等待计时的起点与"已等多久"都取 Redis 服务端时钟(r2-sm#2):第一次登记写 Redis 的 now,之后按 Redis 的 now 求差;
// 已登记的不重写起点;Redis 时钟回拨钳在 0;scope 变了或值坏了重新登记。
func TestNoteLeaveReleaseWait_UsesRedisClock(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const playerID = uint64(8315)
	key := leaveReleaseWaitKey(playerID)
	const scope = "3:10:11:42"
	budget := 60 * time.Second
	t0 := leaveReleaseRedisNow
	startedAt := func(at time.Time, s string) string { return s + ":" + strconv.FormatInt(at.UnixMilli(), 10) }

	mr.SetTime(t0)
	assert.Equal(t, time.Duration(0), noteLeaveReleaseWait(sc, playerID, scope, budget), "第一次见到:登记并返回 0")
	assert.Equal(t, startedAt(t0, scope), leaveCasValue(t, mr, key), "起点写 Redis 的时钟")
	assert.Equal(t, budget+leaveReleaseWaitKeyMargin, mr.TTL(key))

	mr.SetTime(t0.Add(30*time.Second + 250*time.Millisecond))
	assert.Equal(t, 30*time.Second+250*time.Millisecond, noteLeaveReleaseWait(sc, playerID, scope, budget))
	assert.Equal(t, startedAt(t0, scope), leaveCasValue(t, mr, key), "已登记的不重写起点")

	mr.SetTime(t0.Add(-time.Minute))
	assert.Equal(t, time.Duration(0), noteLeaveReleaseWait(sc, playerID, scope, budget), "Redis 时钟回拨:钳 0,只会多等")

	mr.SetTime(t0.Add(time.Minute))
	assert.Equal(t, time.Duration(0), noteLeaveReleaseWait(sc, playerID, "4:10:11:42", budget), "scope 变了:重新登记")
	assert.Equal(t, startedAt(t0.Add(time.Minute), "4:10:11:42"), leaveCasValue(t, mr, key))

	require.NoError(t, mr.Set(key, "garbage"))
	assert.Equal(t, time.Duration(0), noteLeaveReleaseWait(sc, playerID, scope, budget), "值坏了:按刚开始等重新登记")
	assert.Equal(t, startedAt(t0.Add(time.Minute), scope), leaveCasValue(t, mr, key))
}

// 核心回归(审计 §2.1):持有节点还没释放 → location 原样保留、人数不动、回 FailedPrecondition,并请持有节点按会话结束释放,
// 期望 epoch = 当前代。旧代码在这里直接删 location,下一次进场按首次落点铸新 epoch、绕过换手门。
func TestLeaveScene_HolderNotReleasedKeepsLocationAndRequestsRelease(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8301)
		sceneID  = uint64(9401)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	pendingBefore := leaveReleaseCount(t, "pending")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)

	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "location 必须原样保留")
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID), "不删就不扣人数")
	assert.Equal(t, "2", nodeMemberCount(mr, testZoneId, "10"))
	assert.Equal(t, []leaveReleaseCall{{zoneID: testZoneId, nodeID: "10", playerID: playerID, expectedOwnerEpoch: 3,
		covered: placementIdentity{11, identGenA}}}, calls(), "覆盖到被清理的会话(离场请求的身份,§13)")
	assert.Equal(t, pendingBefore+1, leaveReleaseCount(t, "pending"))
	assert.True(t, mr.Exists(leaveReleaseWaitKey(playerID)), "第一次拒绝登记等待计时")

	// 重试(player_locator 的清理通道)照旧拒绝、照旧再请一次释放。
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Len(t, calls(), 2)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
}

// §13.2:location 比离场会话旧(gen_L < gen_R,离场身份判 deleted)时 pending 照样派发,覆盖身份取离场请求的
// (被清理的那条会话),不是 location 记的旧身份;否则经 gate Bind 补发绑上更新会话的实体会被判 skip_newer_login,
// 持有者一直不释放、等到上限才 forced。上一条用例两边身份相同,分不出填的是哪一个。
func TestLeaveScene_PendingCoversLeaveRequestIdentityNotLocation(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8314)
		sceneID  = uint64(9415)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3) // location 身份 (11, identGenA)

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: playerID, SceneId: sceneID, SessionId: 22, LoginGeneration: identGenB,
	})
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "location 必须原样保留")
	got := calls()
	require.Len(t, got, 1, "gen_L < gen_R 判 deleted → pending,请持有节点释放")
	assert.Equal(t, uint64(3), got[0].expectedOwnerEpoch)
	assert.Equal(t, placementIdentity{22, identGenB}, got[0].covered, "覆盖到离场请求的身份(§13.2)")
	assert.NotEqual(t, placementIdentity{11, identGenA}, got[0].covered, "不得退回 location 记的旧身份")
}

// 持有节点写出当前代标记(A1′ / 凭证)之后,下一次重试删 location、扣一次人数、删等待计时。
func TestLeaveScene_CurrentEpochMarkerReleasesLocation(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8302)
		sceneID  = uint64(9402)
	)
	leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)

	require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "3:1700000000000"))
	releasedBefore := leaveReleaseCount(t, "released")
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)

	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))
	assert.False(t, mr.Exists(leaveReleaseWaitKey(playerID)), "删 location 时一并删等待计时")
	assert.Len(t, calls(), 1, "拿到证据之后不再请释放")
	assert.Equal(t, releasedBefore+1, leaveReleaseCount(t, "released"))
}

// 旧代的标记(上一次持有留下的,还在 300s TTL 内)不是本代的释放证据。
func TestLeaveScene_OlderEpochMarkerIsNotRelease(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8303)
		sceneID  = uint64(9403)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "2:1700000000000"))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
}

// R2:预读到标记之后、删除 Lua 执行之前标记被撤回(交接取消)→ Lua 复核不过,一个字节都不动,按未释放回拒。
func TestLeaveScene_MarkerWithdrawnBeforeDeleteKeepsLocation(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8304)
		sceneID  = uint64(9404)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	handoffKey := ownerepoch.HandoffKey(playerID)
	require.NoError(t, mr.Set(handoffKey, "3:1700000000000"))
	locKey := getPlayerLocationKey(playerID)

	// 钩子在第一次碰到 location 键的 EVAL / EVALSHA 时删掉标记(先消费一次性标志再改数据:Lua 内部的 redis.call
	// 会带着 miniredis 的锁重入钩子,那时标志已被消费,做法同 leavescene_cas_test.go)。
	var fired atomic.Bool
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if (cmd != "EVAL" && cmd != "EVALSHA") || !slices.Contains(args, locKey) || !fired.CompareAndSwap(false, true) {
			return false
		}
		mr.Del(handoffKey)
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.True(t, fired.Load(), "钩子没触发,用例空转")
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, locKey), "证据消失时不得删除")
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Len(t, calls(), 1, "按未释放处理,照样请持有节点释放")
}

// epoch缺失不能证明持有者已释放；历史零值配置不得绕过释放门。
func TestLeaveScene_LegacyEpochZeroKeepsUnknown(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8305)
		sceneID  = uint64(9405)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 0)
	before := leaveReleaseCount(t, "legacy_epoch0")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Empty(t, calls(), "不能向epoch未知持有者发无版本释放")
	assert.Equal(t, before, leaveReleaseCount(t, "legacy_epoch0"))
}

// owner_epoch 键缺失、location 记着 N:按 N 要求释放证据(与 EnterScene 同口径),不当作存量玩家直接删。
func TestLeaveScene_MissingEpochKeyUsesLocationEpoch(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8306)
		sceneID  = uint64(9406)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 5)
	mr.Del(ownerepoch.OwnerEpochKey(playerID))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	require.Len(t, calls(), 1)
	assert.Equal(t, uint64(5), calls()[0].expectedOwnerEpoch)

	// 键仍缺失时,"5:t" 标记照样是有效证据(只有键为 5 时才写得出来)。
	require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "5:1700000000000"))
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
}

// 例外:等待落点(node_id 为空)没有任何持有者,直接删。
func TestLeaveScene_AwaitingPlacementDeletes(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8307)
		sceneID  = uint64(9407)
	)
	leaveReleaseSeed(t, mr, playerID, sceneID, "", 4)
	before := leaveReleaseCount(t, "no_holder")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Empty(t, calls())
	assert.Equal(t, before+1, leaveReleaseCount(t, "no_holder"))
}

// 负预算保留配置兼容，但不能把UNKNOWN持有者变成已释放。
func TestLeaveScene_NegativeBudgetCannotDisableProof(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	sc.Config.LeaveSceneHolderReleaseWaitSeconds = -1
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8308)
		sceneID  = uint64(9408)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	before := leaveReleaseCount(t, "disabled")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Len(t, calls(), 1)
	assert.Equal(t, before, leaveReleaseCount(t, "disabled"))
}

// 开发换手旁路也不能降低离场清理的释放证明。
func TestLeaveScene_DevBypassCannotDeleteUnknownHolder(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	sc.Config.AllowUnsafeCrossNodeHandoff = true
	installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8309)
		sceneID  = uint64(9409)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
}

// 零值配置(代码里直接构造)不得把这道门关掉:0 取默认上限。
func TestEffectiveLeaveSceneHolderReleaseWait(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "")
	wait, enabled := sc.Config.EffectiveLeaveSceneHolderReleaseWait()
	assert.True(t, enabled)
	assert.Equal(t, 600*time.Second, wait)

	sc.Config.LeaveSceneHolderReleaseWaitSeconds = 30
	wait, enabled = sc.Config.EffectiveLeaveSceneHolderReleaseWait()
	assert.True(t, enabled)
	assert.Equal(t, 30*time.Second, wait)

	sc.Config.LeaveSceneHolderReleaseWaitSeconds = -1
	wait, enabled = sc.Config.EffectiveLeaveSceneHolderReleaseWait()
	assert.True(t, enabled)
	assert.Equal(t, 600*time.Second, wait)
}

// R3:离场身份是 legacy(请求不带身份,混版窗口)时 location 可能属于一次更新的登录,只等证据、不请持有节点释放。
func TestLeaveScene_LegacyIdentityWaitsWithoutRequestingRelease(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8310)
		sceneID  = uint64(9410)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	before := leaveReleaseCount(t, "pending_unproven")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: playerID, SceneId: sceneID,
	})
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Empty(t, calls(), "legacy 身份不得退出持有节点上的实体")
	assert.Equal(t, before+1, leaveReleaseCount(t, "pending_unproven"))
}

// 等满甚至时钟跳变都不是持有者已释放的证明：原文/人数不动，继续按原会话催促。
func TestLeaveScene_WaitBudgetExceededKeepsUnknown(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	sc.Config.LeaveSceneHolderReleaseWaitSeconds = 60
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8311)
		sceneID  = uint64(9411)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	loc := &scene_manager.PlayerLocation{SessionId: 11, LoginGeneration: identGenA, NodeId: "10"}
	mr.SetTime(leaveReleaseRedisNow)
	firstMs := leaveReleaseRedisNow.Add(-61 * time.Second).UnixMilli()
	require.NoError(t, mr.Set(leaveReleaseWaitKey(playerID),
		leaveReleaseWaitScope(3, loc)+":"+strconv.FormatInt(firstMs, 10)))
	before := leaveReleaseCount(t, "forced")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Len(t, calls(), 1)
	assert.Equal(t, before, leaveReleaseCount(t, "forced"))
}

// 等待计时属于某一份 location:(epoch, 节点, 登录身份) 变了就重新计时,上一份的等待不得算到新的头上。
func TestLeaveScene_WaitTimerDoesNotCarryAcrossLocations(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	sc.Config.LeaveSceneHolderReleaseWaitSeconds = 60
	installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8312)
		sceneID  = uint64(9412)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	oldScope := leaveReleaseWaitScope(2, &scene_manager.PlayerLocation{SessionId: 11, LoginGeneration: identGenA, NodeId: "10"})
	mr.SetTime(leaveReleaseRedisNow)
	require.NoError(t, mr.Set(leaveReleaseWaitKey(playerID),
		oldScope+":"+strconv.FormatInt(leaveReleaseRedisNow.Add(-time.Hour).UnixMilli(), 10)))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "别的 location 留下的计时不得触发 forced")
}

// 第二轮审计 r2-sm#2:等待计时以前用各 scene_manager 副本自己的墙钟。时钟慢的副本写下起点,时钟快的副本来算,刚等了几十秒
// 就被算成"等满"、forced 删 location。现在起点与差值都取 Redis 的时钟。本用例把 Redis 时钟定在本进程墙钟 1 小时之前
// (等价于"写起点的副本比本副本慢 1 小时"):Redis 上只过了 30s,就只算 30s。旧实现拿 time.Now() 去减,会算出 1h30s 并 forced。
func TestLeaveScene_WaitTimerUsesRedisClockNotReplicaClock(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	sc.Config.LeaveSceneHolderReleaseWaitSeconds = 60
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8316)
		sceneID  = uint64(9416)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	redisNow := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	mr.SetTime(redisNow)
	loc := &scene_manager.PlayerLocation{SessionId: 11, LoginGeneration: identGenA, NodeId: "10"}
	require.NoError(t, mr.Set(leaveReleaseWaitKey(playerID),
		leaveReleaseWaitScope(3, loc)+":"+strconv.FormatInt(redisNow.Add(-30*time.Second).UnixMilli(), 10)))
	forcedBefore := leaveReleaseCount(t, "forced")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "按 Redis 时钟只等了 30s,不得 forced")
	assert.Equal(t, forcedBefore, leaveReleaseCount(t, "forced"))
	assert.Len(t, calls(), 1, "仍在等:照样请持有节点释放")

	// Redis时钟即使走过旧预算，也不能把UNKNOWN变成释放证明。
	mr.SetTime(redisNow.Add(31 * time.Second))
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Equal(t, forcedBefore, leaveReleaseCount(t, "forced"))
}

// 第二轮审计 r2-sm#3:zone 负载集的键丢了(过期 / 淘汰 / 误删),持有节点其实活着、仍在 etcd 注册表里。旧实现把
// playerLocationOwnerGone(ZCARD == 0、ZSCORE 不在、没有 death_at)当成"整个 zone 已死",按 owner_dead 立刻删 location;
// 下一次进场按首次落点铸新 epoch,回到 §2.1 的回档路径。持有节点已死只认 etcd 注册表里的正面消失。
func TestLeaveScene_MissingLoadSetIsNotOwnerDead(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8317)
		sceneID  = uint64(9417)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	// 持有节点的租约还活着(etcd 注册表已同步且有它),但负载集整键丢失。
	markKnownNodesSyncedForTest(t)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/leave-release-10", testZoneId, "10")
	mr.Del(nodeLoadKey(testZoneId))
	ownerDeadBefore := leaveReleaseCount(t, "owner_dead")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "持有节点仍在注册表里:不得按已死删除")
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Equal(t, ownerDeadBefore, leaveReleaseCount(t, "owner_dead"))
	assert.Len(t, calls(), 1, "按未释放处理,请持有节点释放")
}

// r2-sm#3 的另一面:正面死亡证据照样立刻删,真正下线的 zone(合服后源区)不会被拖到等待上限。etcd 注册表已同步且没有
// 节点 "10"、负载集里也没有它、没有 death_at → owner_dead。
func TestLeaveScene_OwnerGoneFromRegistryDeletes(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8318)
		sceneID  = uint64(9418)
	)
	leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	markKnownNodesSyncedForTest(t)
	mr.Del(nodeLoadKey(testZoneId))
	before := leaveReleaseCount(t, "owner_dead")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
	assert.Empty(t, calls())
	assert.Equal(t, before+1, leaveReleaseCount(t, "owner_dead"))
}

type fakePlayerStoreHolders struct {
	err     error
	players []uint64
	epochs  []uint64
}

func (f *fakePlayerStoreHolders) ReleaseDeadPlayerStoreHolder(_ context.Context, player, ownerEpoch uint64) error {
	f.players = append(f.players, player)
	f.epochs = append(f.epochs, ownerEpoch)
	return f.err
}

// 判死的持有者可能只是暂停了,先在写权行上把它挡住,再删 location。
func TestLeaveScene_OwnerDeadReleasesPlayerStoreBeforeDeleting(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtx(t, "")
	installLeaveReleaseRecorder(t)
	holders := &fakePlayerStoreHolders{}
	sc.PlayerStoreHolders = holders
	const (
		playerID = uint64(8331)
		sceneID  = uint64(9431)
	)
	leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	markKnownNodesSyncedForTest(t)
	mr.Del(nodeLoadKey(testZoneId))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, []uint64{playerID}, holders.players)
	assert.Equal(t, []uint64{3}, holders.epochs)
}

// location 和 owner_epoch 键不一致:读到 location 之后,另一路 EnterScene 已经接手、铸了新代次。
// 释放只能用 location 这一代,用键里的新值会把刚接手的新持有者围掉。
func TestLeaveScene_OwnerDeadReleasesTheLocationEpochNotTheKey(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtx(t, "")
	installLeaveReleaseRecorder(t)
	holders := &fakePlayerStoreHolders{}
	sc.PlayerStoreHolders = holders
	const (
		playerID = uint64(8334)
		sceneID  = uint64(9434)
	)
	leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), "5"))
	markKnownNodesSyncedForTest(t)
	mr.Del(nodeLoadKey(testZoneId))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.Equal(t, []uint64{3}, holders.epochs)
}

// location 没记代次:不释放,也不挡清理。下一次认领照样抬代次把旧持有者挡住。
func TestLeaveScene_OwnerDeadWithoutLocationEpochSkipsPlayerStoreRelease(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtx(t, "")
	installLeaveReleaseRecorder(t)
	holders := &fakePlayerStoreHolders{}
	sc.PlayerStoreHolders = holders
	const (
		playerID = uint64(8335)
		sceneID  = uint64(9435)
	)
	leaveReleaseSeed(t, mr, playerID, sceneID, "10", 0)
	require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), "3"))
	markKnownNodesSyncedForTest(t)
	mr.Del(nodeLoadKey(testZoneId))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Empty(t, holders.players)
}

// 释放失败就保留 location 重试:不释放的话 released 一直是 0,待放弃的资产操作永远卡着。
func TestLeaveScene_OwnerDeadKeepsLocationWhenPlayerStoreReleaseFails(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtx(t, "")
	installLeaveReleaseRecorder(t)
	sc.PlayerStoreHolders = &fakePlayerStoreHolders{err: errors.New("data_service down")}
	const (
		playerID = uint64(8332)
		sceneID  = uint64(9432)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	markKnownNodesSyncedForTest(t)
	mr.Del(nodeLoadKey(testZoneId))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.Error(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
}

// 其余能删 location 的结论不调释放:持有者自己已经释放过(标记),或根本没有持有者。
func TestLeaveScene_MarkerReleaseDoesNotCallPlayerStore(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	installLeaveReleaseRecorder(t)
	holders := &fakePlayerStoreHolders{}
	sc.PlayerStoreHolders = holders
	const (
		playerID = uint64(8333)
		sceneID  = uint64(9433)
	)
	leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "3:1700000000000"))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.Empty(t, holders.players)
}

// 第二轮审计 r2-sm#1:释放判定计数(scene_manager_leave_holder_release_total)与 forced 的 ERROR 只记真正删掉的那一次。
// 判定之后、删除 Lua 之前 location 被并发改写(EnterScene 写了新落点 → superseded)或已被别的调用删掉(already_gone),
// 这一次什么都没删,不得计数。旧实现在删除之前就计,SceneManagerLeaveReleaseForced 会在没有任何 fail-open 删除时误报。
func TestLeaveScene_ReleaseVerdictCountedOnlyWhenDeleted(t *testing.T) {
	// 并发 EnterScene 写下的新落点:节点 20、owner_epoch 4、更新的登录身份。
	newerRaw := func(sceneID uint64) string {
		return leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
			SceneId: sceneID, NodeId: "20", ZoneId: testZoneId, OwnerEpoch: 4, UpdateTime: 200,
			SessionId: 22, LoginGeneration: identGenB,
		})
	}
	cases := []struct {
		name     string
		playerID uint64
		sceneID  uint64
		verdict  string
		// wantScene 是用例结束时的场景人数:成员身份跟着 location 走,新落点还在这个场景就保住,location 已不在就摘掉。
		wantScene string
		// arrange 摆出判定所需的证据 / 计时;race 在删除 Lua 执行前改动 location。
		arrange func(t *testing.T, mr *miniredis.Miniredis, playerID uint64)
		race    func(mr *miniredis.Miniredis, locKey string, sceneID uint64)
		// wantLoc 是用例结束时 location 应有的原文("" = 不存在)。
		wantLoc func(sceneID uint64) string
	}{
		{
			name: "released then superseded", playerID: 8319, sceneID: 9419, verdict: "released", wantScene: "2",
			arrange: func(t *testing.T, mr *miniredis.Miniredis, playerID uint64) {
				require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "3:1700000000000"))
			},
			race: func(mr *miniredis.Miniredis, locKey string, sceneID uint64) {
				_ = mr.Set(locKey, newerRaw(sceneID))
			},
			wantLoc: newerRaw,
		},
		{
			name: "released then already gone", playerID: 8320, sceneID: 9420, verdict: "released", wantScene: "1",
			arrange: func(t *testing.T, mr *miniredis.Miniredis, playerID uint64) {
				require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "3:1700000000000"))
			},
			race:    func(mr *miniredis.Miniredis, locKey string, _ uint64) { mr.Del(locKey) },
			wantLoc: func(uint64) string { return "" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtx(t, "")
			sc.Config.LeaveSceneHolderReleaseWaitSeconds = 60
			installLeaveReleaseRecorder(t)
			leaveReleaseSeed(t, mr, tc.playerID, tc.sceneID, "10", 3)
			tc.arrange(t, mr, tc.playerID)
			locKey := getPlayerLocationKey(tc.playerID)

			// 钩子只在第一次碰到 location 键的 EVAL / EVALSHA(即删除 Lua)时改动 location,做法同
			// TestLeaveScene_MarkerWithdrawnBeforeDeleteKeepsLocation。
			var fired atomic.Bool
			mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
				if (cmd != "EVAL" && cmd != "EVALSHA") || !slices.Contains(args, locKey) || !fired.CompareAndSwap(false, true) {
					return false
				}
				tc.race(mr, locKey, tc.sceneID)
				return false
			})
			t.Cleanup(func() { mr.Server().SetPreHook(nil) })
			before := leaveReleaseCount(t, tc.verdict)

			_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(tc.playerID, tc.sceneID))
			require.True(t, fired.Load(), "钩子没触发,用例空转")
			require.NoError(t, err, "没删成也不是故障:location 已被别人处理")
			assert.Equal(t, tc.wantLoc(tc.sceneID), leaveCasValue(t, mr, locKey))
			assert.Equal(t, tc.wantScene, sceneMemberCount(mr, tc.sceneID))
			assert.Equal(t, before, leaveReleaseCount(t, tc.verdict), "这一次没删,不计 %s", tc.verdict)
		})
	}
}

// 审计 §2.1「LeaveGame 后快速重登」:location 因持有者未释放而保留期间,被派到别的节点的登录走换手门(18),而不是按首次落点
// 铸新 epoch;持有节点写出当前代标记后放行并铸造;旧会话迟到的离场看到更新的登录身份判 keep_newer。
func TestLeaveGameQuickReloginGoesThroughHandoffGate(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	installLeaveReleaseRecorder(t)
	transfer := installReleaseDispatchRecorder(t, sc, nil)
	holderRequested := installEnterHolderReleaseRecorder(t, nil)
	const (
		playerID = uint64(8313)
		s1       = uint64(9413) // 持有节点 10
		s2       = uint64(9414) // 别的节点 20
	)
	seedSceneOnNode(mr, testZoneId, s1, "10", "1")
	seedSceneOnNode(mr, testZoneId, s2, "20", "0")
	seedNodeMembers(mr, testZoneId, "10", 1)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 3, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})
	adoptSceneMember(mr, s1, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)
	require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), "3"))

	// 1. LeaveGame → MarkOffline → LeaveScene:持有节点还没落盘释放,location 保留。
	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, s1))
	requireHolderReleasePending(t, err)
	require.NotNil(t, identLocation(t, sc, playerID))

	// 2. 快速重登(新会话 22、更大代次)被派到节点 20:换手门要求当前代标记 → 18,不铸造、不改 location。
	relogin := &scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s2, ZoneId: testZoneId, SessionId: 22, LoginGeneration: identGenB,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid",
	}
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(relogin)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "location 在就必须过换手门,不得按首次落点铸造")
	assert.Equal(t, "3", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, "10", loc.NodeId)
	// 这次登录的身份严格更新、来自 login:回 18 之前同步请持有节点按会话结束释放一次(§12;gate 已代发 ExitGame 时合并)。
	holderCalls := holderRequested()
	require.Len(t, holderCalls, 1)
	assert.Equal(t, "10", holderCalls[0].nodeID)
	assert.Equal(t, uint64(3), holderCalls[0].expectedOwnerEpoch)
	assert.Equal(t, placementIdentity{22, identGenB}, holderCalls[0].covered, "覆盖到这次登录自己的身份(§13)")

	// 3. 持有节点退出存盘落地,A1′ 写出当前代标记 → 重试放行并铸造 4,location 指向节点 20 与新登录。
	require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "3:1700000000000"))
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(relogin)
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "4", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	loc = identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, "20", loc.NodeId)
	assert.Equal(t, uint32(22), loc.SessionId)
	calls, _ := transfer.snapshot()
	require.Len(t, calls, 1, "跨节点放行后照常通知旧节点(交接释放)")
	assert.Equal(t, uint64(3), calls[0].expectedOwnerEpoch)
	assert.Len(t, holderRequested(), 1, "放行路径不请会话结束释放")

	// 4. 旧会话迟到的清理(player_locator 现读 location 拿到 s2):新登录的身份更新 → keep_newer,不删。
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, s2))
	require.NoError(t, err)
	assert.NotNil(t, identLocation(t, sc, playerID))
}
