package logic

// 持有者存活租约(holder_lease.go):判死接管与判死释放之前先看持有进程还在不在续租。
// 夹具沿用各接管点已有的摆法(seedPlayerOnDeadNode / seedReplacedHolder / seedStaleLocationFromZone1 / seedDeadHolderLeave),
// 只多写一个 player:{id}:holder_lease 键;本文件的辅助标识符统一加 holderLease 前缀。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"shared/ownerepoch"
)

// holderLeaseInstance 是还活着的持有进程的实例号(租约的值)。
const holderLeaseInstance = "66666666-6666-4666-8666-666666666666"

// holderLeaseHold 写下一份租约:持有进程刚续过。
func holderLeaseHold(mr *miniredis.Miniredis, playerID uint64) {
	_ = mr.Set(ownerepoch.HolderLeaseKey(playerID), holderLeaseInstance)
	mr.SetTTL(ownerepoch.HolderLeaseKey(playerID), ownerepoch.HolderLeaseTTL)
}

// holderLeaseBlockedCount 读 scene_manager_holder_lease_blocked_total{site,state} 的当前值(各 zone 求和)。
func holderLeaseBlockedCount(t *testing.T, site, state string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	total := 0.0
	for _, family := range families {
		if family.GetName() != "scene_manager_holder_lease_blocked_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["site"] == site && labels["state"] == state {
				total += metric.GetCounter().GetValue()
			}
		}
	}
	return total
}

func holderLeaseEnterRequest(playerID, sceneID uint64) *scene_manager.EnterSceneRequest {
	return &scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	}
}

// 读租约只有三种结论;键在不在说了算,值只进日志。读失败(这里用类型不对的键造一次 WRONGTYPE)不能读成「不在」。
func TestReadHolderLease(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6500)

	state, instance, err := readHolderLease(sc, playerID)
	require.NoError(t, err)
	assert.Equal(t, holderLeaseAbsent, state)
	assert.Empty(t, instance)
	assert.False(t, state.keepsOwner(), "键不存在才允许按判死处理")

	holderLeaseHold(mr, playerID)
	state, instance, err = readHolderLease(sc, playerID)
	require.NoError(t, err)
	assert.Equal(t, holderLeaseHeld, state)
	assert.Equal(t, holderLeaseInstance, instance)
	assert.True(t, state.keepsOwner())

	require.NoError(t, mr.Set(ownerepoch.HolderLeaseKey(playerID), ""))
	state, _, err = readHolderLease(sc, playerID)
	require.NoError(t, err)
	assert.Equal(t, holderLeaseHeld, state, "值为空也是在:判的是键")

	mr.Del(ownerepoch.HolderLeaseKey(playerID))
	mr.HSet(ownerepoch.HolderLeaseKey(playerID), "field", "value")
	state, _, err = readHolderLease(sc, playerID)
	require.Error(t, err)
	assert.Equal(t, holderLeaseUnknown, state)
	assert.True(t, state.keepsOwner(), "读不出来按在处理")
}

// 节点只和 etcd 失联:注册表里没有它、再入屏障也过了,但进程活着、在续租。不许接管 —— 位置记录、owner_epoch 一个字节不动,
// 请求照旧被换手门以可重试的 18 暂拒。等它不再续租(玩家离开、实体销毁,或进程退出),租约过期之后同一请求才接管。
// 以前这里直接铸新代次派到别的节点,老进程最后一次提交之后的进度被写权行拒掉(保障地图 §6.2 第 4 条)。
func TestEnterScene_DeadOwnerStillRenewingHolderLeaseIsNotTakenOver(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6501)
		oldScene = uint64(7501)
		targetID = uint64(7601)
	)
	seedPlayerOnDeadNode(t, sc, mr, playerID, oldScene, targetID)
	holderLeaseHold(mr, playerID)
	before, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	heldBefore := holderLeaseBlockedCount(t, holderLeaseSiteDeadOwnerTakeover, holderLeaseHeld.String())

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(holderLeaseEnterRequest(playerID, targetID))
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "持有进程还活着:不按判死接管,照旧走换手门")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "不铸新代次")
	after, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, before, after, "位置记录一个字节不动")
	assert.Empty(t, *captured, "不发路由")
	assert.Equal(t, "4", sceneCountText(sc, targetID), "拒绝时不得留下目标场景预占")
	assert.Equal(t, heldBefore+1, holderLeaseBlockedCount(t, holderLeaseSiteDeadOwnerTakeover, holderLeaseHeld.String()))

	// 租约过期(不再续):同一请求按判死接管,铸新代次。
	mr.Del(ownerepoch.HolderLeaseKey(playerID))
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(holderLeaseEnterRequest(playerID, targetID))
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
}

// 持有者自己为当前代写出的落盘标记优先于租约:它声明这一代已落盘、不再写(疏散改派、退出释放都会写),接管不丢东西,
// 不能把这类协作式的流程挡在门外。别的代的标记不算。
func TestEnterScene_DeadOwnerHolderLeaseYieldsToCurrentEpochMarker(t *testing.T) {
	cases := []struct {
		name        string
		markerEpoch uint64
		wantCode    uint32
		wantEpoch   string
	}{
		{"current_epoch_marker_allows_takeover", 1, 0, "2"},
		{"other_epoch_marker_keeps_blocking", 7, constants.ErrHandoffPending, "1"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearKnownNodesForTest()
			t.Cleanup(clearKnownNodesForTest)
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			capturingKafkaWriter(sc)
			playerID := uint64(6510 + i)
			oldScene, targetID := uint64(7510+i), uint64(7610+i)
			seedPlayerOnDeadNode(t, sc, mr, playerID, oldScene, targetID)
			holderLeaseHold(mr, playerID)
			writeHandoffMarker(t, sc, playerID, c.markerEpoch)

			resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(holderLeaseEnterRequest(playerID, targetID))
			require.NoError(t, err)
			assert.Equal(t, c.wantCode, resp.ErrorCode)
			assert.Equal(t, c.wantEpoch, ownerEpochRaw(t, sc, playerID))
		})
	}
}

// 租约读不出来:拿不准持有进程还在不在,按在处理,不接管(fail-closed),并按 unknown 计数。
func TestEnterScene_DeadOwnerUnreadableHolderLeaseFailsClosed(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6502)
		oldScene = uint64(7502)
		targetID = uint64(7602)
	)
	seedPlayerOnDeadNode(t, sc, mr, playerID, oldScene, targetID)
	mr.HSet(ownerepoch.HolderLeaseKey(playerID), "field", "value") // GET 回 WRONGTYPE
	unknownBefore := holderLeaseBlockedCount(t, holderLeaseSiteDeadOwnerTakeover, holderLeaseUnknown.String())

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(holderLeaseEnterRequest(playerID, targetID))
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Empty(t, *captured)
	assert.Equal(t, unknownBefore+1, holderLeaseBlockedCount(t, holderLeaseSiteDeadOwnerTakeover, holderLeaseUnknown.String()))
}

// 节点号已被新进程拿走(GO-3),但被顶替的旧进程只是和 etcd 失联、还在续租:实体还在它内存里,不接管。
// 回可重试的 17(与屏障未过同一个码):照旧路由会落到同号新进程上按原 epoch 登记,必被拒。
func TestEnterScene_ReplacedOwnerStillRenewingHolderLeaseIsRejectedRetryably(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6503)
		oldScene = uint64(7503)
		targetID = uint64(7603)
	)
	raw := seedReplacedHolder(t, sc, mr, playerID, oldScene, targetID)
	observeInstanceGoneAtForTest(replacedOldInstance, 2*sceneReentryBarrier(sc))
	stubFreshRegistryForTest(t, false, nil)
	holderLeaseHold(mr, playerID)
	heldBefore := holderLeaseBlockedCount(t, holderLeaseSiteReplacedOwnerTakeover, holderLeaseHeld.String())

	resp := enterReplacedForTest(t, sc, playerID, targetID)
	assert.Equal(t, constants.ErrSceneReentryBarrier, resp.ErrorCode)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	after, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, raw, after, "位置记录一个字节不动")
	assert.Empty(t, *captured)
	assert.Equal(t, heldBefore+1, holderLeaseBlockedCount(t, holderLeaseSiteReplacedOwnerTakeover, holderLeaseHeld.String()))

	// 租约过期之后同一请求按顶替接管。
	mr.Del(ownerepoch.HolderLeaseKey(playerID))
	resp = enterReplacedForTest(t, sc, playerID, targetID)
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
}

// 整个 zone 从 etcd 消失(合服下线是这个形态,整个 zone 和 etcd 失联也是这个形态)。后一种进程都活着、在续租:
// 不能把它名下的玩家当成没有位置记录,从别的 zone 的 gate 直接落点。
func TestEnterScene_StaleZoneLocationStillRenewingHolderLeaseIsNotIgnored(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	ctx := context.Background()
	writer := &countingKafkaWriter{}
	sc.Kafka = writer
	sc.Config.KafkaWriteTimeoutSeconds = 1
	const (
		playerID = uint64(5130)
		oldScene = uint64(1130)
		targetID = uint64(2230)
	)
	seedStaleLocationFromZone1(t, sc, mr, playerID, oldScene, targetID)
	backdateForeignAbsenceForTest(foreignZoneAbsence, "1", sceneReentryBarrier(sc)+time.Second)
	holderLeaseHold(mr, playerID)
	before, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	request := &scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: 2,
		GateZoneId: 2, GateId: "1", GateInstanceId: "gate-uuid-test",
	}

	resp, err := NewEnterSceneLogic(ctx, sc).EnterScene(request)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "持有进程还在续租:陈旧位置不得当不存在")
	after, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, before, after)
	assert.Equal(t, 0, writer.count())

	mr.Del(ownerepoch.HolderLeaseKey(playerID))
	resp, err = NewEnterSceneLogic(ctx, sc).EnterScene(request)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode, "租约过期之后按整 zone 下线放行")
}

// LeaveScene 的判死释放同理:持有进程还在续租时,不删位置记录、不去存档库释放它的写权(那会把活着的持有者围掉),
// 回 FailedPrecondition 让清理任务重试。租约过期之后照原流程释放并删除。
func TestLeaveScene_OwnerDeadStillRenewingHolderLeaseKeepsLocationAndClaim(t *testing.T) {
	const (
		playerID = uint64(8351)
		sceneID  = uint64(9451)
	)
	holders := &fakePlayerStoreHolders{holder: deadHolderRow()}
	sc, mr, raw := seedDeadHolderLeave(t, playerID, sceneID, holders)
	stubDeadHolderRegistryForTest(t, nil, "44444444-4444-4444-8444-444444444444")
	holderLeaseHold(mr, playerID)
	heldBefore := holderLeaseBlockedCount(t, holderLeaseSiteLeaveRelease, holderLeaseHeld.String())

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "持有进程还活着:位置记录不许删")
	assert.Zero(t, holders.reads, "不判 owner_dead,走不到判死释放")
	assert.Empty(t, holders.players, "不许释放活着的持有者的写权")
	assert.Equal(t, heldBefore+1, holderLeaseBlockedCount(t, holderLeaseSiteLeaveRelease, holderLeaseHeld.String()))

	mr.Del(ownerepoch.HolderLeaseKey(playerID))
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, []uint64{playerID}, holders.players)
}

// 持有者已经为这一代写出落盘标记时,LeaveScene 不看租约:它声明不再写,判死释放与删除照常进行。
func TestLeaveScene_OwnerDeadHolderLeaseYieldsToCurrentEpochMarker(t *testing.T) {
	const (
		playerID = uint64(8352)
		sceneID  = uint64(9452)
	)
	holders := &fakePlayerStoreHolders{holder: deadHolderRow()}
	sc, mr, _ := seedDeadHolderLeave(t, playerID, sceneID, holders)
	stubDeadHolderRegistryForTest(t, nil, "44444444-4444-4444-8444-444444444444")
	holderLeaseHold(mr, playerID)
	writeHandoffMarker(t, sc, playerID, 3) // seedDeadHolderLeave 的 owner_epoch 是 3

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
}

// 键名与存活时间是和 C++ 持有进程的跨语言契约:一边改了另一边不改,租约要么读不到、要么活得比再入屏障还久。
// C++ 那两行按原文核对;只拷了 go/ 的构建环境里找不到 C++ 源码时跳过。
func TestHolderLeaseContractMatchesCpp(t *testing.T) {
	assert.Equal(t, "player:{42}:holder_lease", ownerepoch.HolderLeaseKey(42))
	assert.Less(t, ownerepoch.HolderLeaseTTL, constants.SceneReentryBarrier,
		"租约要比再入屏障短:进程真死了,不能让它的租约把接管拖到屏障之后")

	data, err := os.ReadFile(filepath.Join(crossLangRepoRoot, "cpp/libs/services/scene/player/comp/player_ownership_comp.h"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("only the Go sources are present; the C++ header cannot be compared")
	}
	require.NoError(t, err)
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	assert.Contains(t, source, `return "player:" + redis_slot::PlayerTag(playerId) + ":holder_lease";`)
	ttlMs := strconv.FormatInt(ownerepoch.HolderLeaseTTL.Milliseconds(), 10)
	assert.Contains(t, source, `inline constexpr std::string_view kHolderLeaseTtlMs = "`+ttlMs+`";`)
}
