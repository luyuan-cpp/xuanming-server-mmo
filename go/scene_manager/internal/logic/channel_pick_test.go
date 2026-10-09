package logic

// 玩家主动选线预检(channel_pick.go,docs/design/world-channel-switch.md §4.4 / §4.5)的单元测试。
//
// 除了几个纯函数,用例都只走 EnterScene 这一个公开入口,断言的是应答码与 Redis 终态
// (位置、人数、冷却键)—— 那是 scene 节点与下一次请求真正依赖的东西。
// 冷却的时间流逝用 miniredis.FastForward 推进,不读墙钟。
//
// 这里验证不了的:scene 节点是否在正确的入口置位 client_channel_pick、跨节点交接重发是否真的
// 不带它(C++ 侧,要靠联调)。本文件只钉住 scene_manager 对「带 / 不带」两种请求的处理。

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"proto/scene_manager"
	"scene_manager/internal/config"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"
)

// 用例里顶替 World 表的两张大世界地图。
const (
	pickTestConf      = uint64(4401)
	pickTestOtherConf = uint64(4402)
)

// newChannelPickCtx 造一个把 pickTestConf / pickTestOtherConf 登记成大世界地图的 svcCtx,
// 并装上能留下路由事件的 Kafka writer。ChannelSwitch 配置保持零值(= 全部默认:开启、冷却 10 秒、
// 上限 2000)。
func newChannelPickCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, *[]kafka.Message) {
	t.Helper()
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{pickTestConf, pickTestOtherConf}))
	return sc, mr, captured
}

// seedPickChannel 摆一条在役的线:场景映射 + 人数 + 在役集合成员,节点登记为存活。
func seedPickChannel(t *testing.T, mr *miniredis.Miniredis, confID, sceneID uint64, nodeID, players string) {
	t.Helper()
	seedSceneOnNode(mr, testZoneId, sceneID, nodeID, players)
	_, err := mr.SAdd(worldChannelsKey(testZoneId, confID), fmt.Sprintf("%d", sceneID))
	require.NoError(t, err)
}

// seedClosingChannel 摆一条回收中的线:已不在在役集合里,只在回收中集合里,场景映射还在。
func seedClosingChannel(t *testing.T, mr *miniredis.Miniredis, confID, sceneID uint64, nodeID, players string) {
	t.Helper()
	seedSceneOnNode(mr, testZoneId, sceneID, nodeID, players)
	_, err := mr.SAdd(worldDrainingSetKey(testZoneId, confID), fmt.Sprintf("%d", sceneID))
	require.NoError(t, err)
}

// channelPickRequest 组一条同 zone 的 EnterScene 请求;pick 即 client_channel_pick。
func channelPickRequest(playerID, sceneID, confID uint64, pick bool) *scene_manager.EnterSceneRequest {
	return &scene_manager.EnterSceneRequest{
		PlayerId: playerID, SceneId: sceneID, SceneConfId: confID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
		ClientChannelPick: pick,
	}
}

func enterChannelPick(t *testing.T, sc *svc.ServiceContext, req *scene_manager.EnterSceneRequest) *scene_manager.EnterSceneResponse {
	t.Helper()
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

// locatedSceneID 返回玩家位置记录里的 scene_id;没有位置记录返回 0。
func locatedSceneID(t *testing.T, sc *svc.ServiceContext, playerID uint64) uint64 {
	t.Helper()
	loc, err := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, err)
	return loc.GetSceneId()
}

func scenePlayerCountRaw(t *testing.T, sc *svc.ServiceContext, sceneID uint64) string {
	t.Helper()
	raw, err := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneID))
	require.NoError(t, err)
	return raw
}

// placePlayerOnLine 把玩家摆在一条线上(铸出 owner_epoch 1)。
func placePlayerOnLine(t *testing.T, sc *svc.ServiceContext, playerID, sceneID uint64, nodeID string) {
	t.Helper()
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, sceneID, nodeID, testZoneId))
}

// ── 不置位:行为不变 ──────────────────────────────────────────────────────

// 回归:不带 client_channel_pick 的请求(自动选线之外的按 id 进入:队伍跟随、疏散、交接重发)
// 不受预检的任何一条约束 —— 满线照进、回收中的线照进、功能关闭也照进、冷却键在也不拦、不碰冷却键。
func TestChannelPick_UnflaggedEnterIsNotSubjectToPrecheck(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	sc.Config.ChannelSwitch = config.ChannelSwitchConfig{Disabled: true, MaxPlayersPerChannel: 5}

	const (
		playerID    = uint64(8101)
		lineOld     = uint64(8801)
		lineFull    = uint64(8802)
		lineClosing = uint64(8803)
	)
	seedPickChannel(t, mr, pickTestConf, lineOld, "10", "1")
	seedPickChannel(t, mr, pickTestConf, lineFull, "10", "5")
	seedClosingChannel(t, mr, pickTestConf, lineClosing, "10", "2")
	placePlayerOnLine(t, sc, playerID, lineOld, "10")
	cooldownKey := channelSwitchCooldownKey(playerID)
	require.NoError(t, mr.Set(cooldownKey, "1"))
	mr.SetTTL(cooldownKey, 100*time.Second)

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineFull, pickTestConf, false))
	assert.Equal(t, uint32(0), resp.ErrorCode, "未置位:已满的线照旧能进(队伍跟随不受上限约束)")
	assert.Equal(t, lineFull, locatedSceneID(t, sc, playerID))
	assert.Equal(t, "6", scenePlayerCountRaw(t, sc, lineFull))

	resp = enterChannelPick(t, sc, channelPickRequest(playerID, lineClosing, pickTestConf, false))
	assert.Equal(t, uint32(0), resp.ErrorCode, "未置位:回收中的线照旧能进(既有行为)")
	assert.Equal(t, lineClosing, locatedSceneID(t, sc, playerID))

	assert.Len(t, *captured, 2)
	assert.Equal(t, 100*time.Second, mr.TTL(cooldownKey), "未置位请求不得读写冷却键")
}

// 置位但目标不是分线(按 id 进副本 / 镜像):预检到此为止,走原逻辑。
// 连 Disabled 与上限都不看 —— 关掉切线不能顺手把进副本也关了;成功后也不起算冷却。
func TestChannelPick_NonChannelTargetSkipsPrecheck(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	sc.Config.ChannelSwitch = config.ChannelSwitchConfig{Disabled: true, MaxPlayersPerChannel: 1}

	const (
		playerID    = uint64(8102)
		lineOld     = uint64(8811)
		dungeonID   = uint64(8812)
		dungeonConf = uint64(9901) // 不在 World 表里
	)
	seedPickChannel(t, mr, pickTestConf, lineOld, "10", "1")
	seedSceneOnNode(mr, testZoneId, dungeonID, "10", "9")
	placePlayerOnLine(t, sc, playerID, lineOld, "10")

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, dungeonID, dungeonConf, true))
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, dungeonID, locatedSceneID(t, sc, playerID))
	assert.Equal(t, "10", scenePlayerCountRaw(t, sc, dungeonID))
	assert.Len(t, *captured, 1)
	assert.False(t, mr.Exists(channelSwitchCooldownKey(playerID)), "进副本不是切线,不起算冷却")
}

// ── 拒绝:22 / 23 ─────────────────────────────────────────────────────────

// assertPickRejectedCleanly 断言一次预检拒绝没有留下任何痕迹:没发路由、位置没动、归属代际没动、
// 目标线人数没动、没起算冷却。
func assertPickRejectedCleanly(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, captured *[]kafka.Message,
	playerID, stillOnScene, targetScene uint64, targetCount string) {
	t.Helper()
	assert.Empty(t, *captured, "拒绝时不得发路由")
	assert.Equal(t, stillOnScene, locatedSceneID(t, sc, playerID), "拒绝时位置不得变")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "拒绝时不得铸造 epoch")
	assert.Equal(t, targetCount, scenePlayerCountRaw(t, sc, targetScene), "预检只读,不得预占人数")
	assert.False(t, mr.Exists(channelSwitchCooldownKey(playerID)), "被预检拒绝不起算冷却")
}

// 目标线在回收中集合里 → 22(channel_closing)。
func TestChannelPick_ClosingChannelIsRejected(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	const (
		playerID    = uint64(8103)
		lineOld     = uint64(8821)
		lineClosing = uint64(8822)
	)
	seedPickChannel(t, mr, pickTestConf, lineOld, "10", "1")
	seedClosingChannel(t, mr, pickTestConf, lineClosing, "10", "3")
	placePlayerOnLine(t, sc, playerID, lineOld, "10")
	before := enterSceneRejectedCount(t, testZoneId, rejectReasonChannelClosing)

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineClosing, pickTestConf, true))

	assert.Equal(t, constants.ErrChannelUnavailable, resp.ErrorCode)
	assert.Equal(t, playerID, resp.PlayerId, "拒绝应答同样回显 player_id(scene 节点靠它对回发起玩家)")
	assert.Equal(t, before+1, enterSceneRejectedCount(t, testZoneId, rejectReasonChannelClosing))
	assertPickRejectedCleanly(t, sc, mr, captured, playerID, lineOld, lineClosing, "3")
}

// 请求的 scene_conf_id 与线所属的地图对不上 → 22(channel_conf_mismatch):这个值会被死节点改派
// 拿去建场景,不能信。请求不带 conf(0)时没有可比对的值,遍历 World 表找到所属图后照常放行。
func TestChannelPick_ConfMismatchIsRejected_UnspecifiedConfIsAccepted(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	const (
		playerID  = uint64(8104)
		lineOld   = uint64(8831)
		lineOther = uint64(8832) // 属于 pickTestOtherConf
	)
	seedPickChannel(t, mr, pickTestConf, lineOld, "10", "1")
	seedPickChannel(t, mr, pickTestOtherConf, lineOther, "10", "2")
	placePlayerOnLine(t, sc, playerID, lineOld, "10")
	before := enterSceneRejectedCount(t, testZoneId, rejectReasonChannelConfMismatch)

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineOther, pickTestConf, true))
	assert.Equal(t, constants.ErrChannelUnavailable, resp.ErrorCode)
	assert.Equal(t, before+1, enterSceneRejectedCount(t, testZoneId, rejectReasonChannelConfMismatch))
	assertPickRejectedCleanly(t, sc, mr, captured, playerID, lineOld, lineOther, "2")

	// 带的是一个根本不在 World 表里的 conf,同样对不上。
	resp = enterChannelPick(t, sc, channelPickRequest(playerID, lineOther, 9901, true))
	assert.Equal(t, constants.ErrChannelUnavailable, resp.ErrorCode)
	assert.Equal(t, before+2, enterSceneRejectedCount(t, testZoneId, rejectReasonChannelConfMismatch))
	assertPickRejectedCleanly(t, sc, mr, captured, playerID, lineOld, lineOther, "2")

	resp = enterChannelPick(t, sc, channelPickRequest(playerID, lineOther, 0, true))
	assert.Equal(t, uint32(0), resp.ErrorCode, "不带 conf:按线实际所属的图放行")
	assert.Equal(t, lineOther, locatedSceneID(t, sc, playerID))
	assert.True(t, mr.Exists(channelSwitchCooldownKey(playerID)), "它仍是一次切线,照常起算冷却")
}

// ChannelSwitch.Disabled → 22(channel_switch_disabled)。
func TestChannelPick_DisabledIsRejected(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	sc.Config.ChannelSwitch.Disabled = true
	const (
		playerID = uint64(8105)
		lineOld  = uint64(8841)
		lineNew  = uint64(8842)
	)
	seedPickChannel(t, mr, pickTestConf, lineOld, "10", "1")
	seedPickChannel(t, mr, pickTestConf, lineNew, "10", "0")
	placePlayerOnLine(t, sc, playerID, lineOld, "10")
	before := enterSceneRejectedCount(t, testZoneId, rejectReasonChannelSwitchDisabled)

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineNew, pickTestConf, true))

	assert.Equal(t, constants.ErrChannelUnavailable, resp.ErrorCode)
	assert.Equal(t, before+1, enterSceneRejectedCount(t, testZoneId, rejectReasonChannelSwitchDisabled))
	assertPickRejectedCleanly(t, sc, mr, captured, playerID, lineOld, lineNew, "0")
}

// 人数 ≥ 上限 → 23(channel_full),人数计数一个都不动;差一人满的线照常放行。
func TestChannelPick_FullChannelIsRejected(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	sc.Config.ChannelSwitch.MaxPlayersPerChannel = 5
	const (
		playerID   = uint64(8106)
		lineOld    = uint64(8851)
		lineFull   = uint64(8852)
		lineAlmost = uint64(8853)
	)
	seedPickChannel(t, mr, pickTestConf, lineOld, "10", "1")
	seedPickChannel(t, mr, pickTestConf, lineFull, "10", "5")
	seedPickChannel(t, mr, pickTestConf, lineAlmost, "10", "4")
	require.NoError(t, mr.Set(nodePlayerCountKey(testZoneId, "10"), "10"))
	placePlayerOnLine(t, sc, playerID, lineOld, "10")
	before := enterSceneRejectedCount(t, testZoneId, rejectReasonChannelFull)

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineFull, pickTestConf, true))

	assert.Equal(t, constants.ErrChannelFull, resp.ErrorCode, "人数恰等于上限就算满")
	assert.Equal(t, before+1, enterSceneRejectedCount(t, testZoneId, rejectReasonChannelFull))
	assertPickRejectedCleanly(t, sc, mr, captured, playerID, lineOld, lineFull, "5")
	nodeCount, err := sc.Redis.Get(nodePlayerCountKey(testZoneId, "10"))
	require.NoError(t, err)
	assert.Equal(t, "10", nodeCount, "节点人数同样不得被改动")
	assert.Equal(t, "1", scenePlayerCountRaw(t, sc, lineOld), "旧线人数不得被扣")

	resp = enterChannelPick(t, sc, channelPickRequest(playerID, lineAlmost, pickTestConf, true))
	assert.Equal(t, uint32(0), resp.ErrorCode, "比上限少一人:放行")
	assert.Equal(t, "5", scenePlayerCountRaw(t, sc, lineAlmost))
}

// ── 冷却 ─────────────────────────────────────────────────────────────────

// 同节点切线成功 → 起算冷却;冷却内再切 → 24,且不刷新冷却;冷却过后恢复。
func TestChannelPick_SameNodeSwitchStartsCooldown_SecondSwitchIsRejected(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	const (
		playerID = uint64(8107)
		lineA    = uint64(8861)
		lineB    = uint64(8862)
	)
	seedPickChannel(t, mr, pickTestConf, lineA, "10", "1")
	seedPickChannel(t, mr, pickTestConf, lineB, "10", "0")
	placePlayerOnLine(t, sc, playerID, lineA, "10")
	cooldownKey := channelSwitchCooldownKey(playerID)

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineB, pickTestConf, true))
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, lineB, locatedSceneID(t, sc, playerID))
	require.True(t, mr.Exists(cooldownKey), "切线成功必须起算冷却")
	assert.Equal(t, 10*time.Second, mr.TTL(cooldownKey), "默认冷却 10 秒")
	value, err := mr.Get(cooldownKey)
	require.NoError(t, err)
	assert.Equal(t, "1", value)

	mr.FastForward(3 * time.Second)
	before := enterSceneRejectedCount(t, testZoneId, rejectReasonChannelSwitchCooldown)
	resp = enterChannelPick(t, sc, channelPickRequest(playerID, lineA, pickTestConf, true))
	assert.Equal(t, constants.ErrChannelSwitchCooldown, resp.ErrorCode)
	assert.Equal(t, before+1, enterSceneRejectedCount(t, testZoneId, rejectReasonChannelSwitchCooldown))
	assert.Equal(t, 7*time.Second, mr.TTL(cooldownKey), "被冷却拒绝不得刷新冷却,否则连点会把自己永久锁住")
	assert.Equal(t, lineB, locatedSceneID(t, sc, playerID))
	assert.Len(t, *captured, 1, "被冷却拒绝不发路由")

	mr.FastForward(7 * time.Second)
	require.False(t, mr.Exists(cooldownKey), "冷却键到期自己消失")
	resp = enterChannelPick(t, sc, channelPickRequest(playerID, lineA, pickTestConf, true))
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, lineA, locatedSceneID(t, sc, playerID))
	assert.Equal(t, 10*time.Second, mr.TTL(cooldownKey), "新的一次切线重新起算")
}

// 首次落点(没有位置记录)不是「切换」:不查冷却,成功后也不起算冷却。
func TestChannelPick_FirstLandingNeitherChecksNorStartsCooldown(t *testing.T) {
	sc, mr, _ := newChannelPickCtx(t)
	const (
		freshPlayer    = uint64(8108)
		returnedPlayer = uint64(8118)
		lineA          = uint64(8871)
	)
	seedPickChannel(t, mr, pickTestConf, lineA, "10", "0")

	resp := enterChannelPick(t, sc, channelPickRequest(freshPlayer, lineA, pickTestConf, true))
	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, lineA, locatedSceneID(t, sc, freshPlayer))
	assert.False(t, mr.Exists(channelSwitchCooldownKey(freshPlayer)), "首次落点不起算冷却")

	// 冷却键恰好还在(下线前刚切过线),位置记录已经没了:照样放行,键原样不动。
	returnedKey := channelSwitchCooldownKey(returnedPlayer)
	require.NoError(t, mr.Set(returnedKey, "1"))
	mr.SetTTL(returnedKey, 100*time.Second)
	resp = enterChannelPick(t, sc, channelPickRequest(returnedPlayer, lineA, pickTestConf, true))
	assert.Equal(t, uint32(0), resp.ErrorCode, "首次落点不查冷却")
	assert.Equal(t, 100*time.Second, mr.TTL(returnedKey), "也不重写冷却键")
}

// 目标就是当前所在的线(同落点重连)同样不是「切换」:不查冷却、不起算冷却。
func TestChannelPick_SamePlacementNeitherChecksNorStartsCooldown(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	const (
		playerID = uint64(8111)
		lineA    = uint64(8876)
	)
	seedPickChannel(t, mr, pickTestConf, lineA, "10", "1")
	placePlayerOnLine(t, sc, playerID, lineA, "10")
	cooldownKey := channelSwitchCooldownKey(playerID)
	require.NoError(t, mr.Set(cooldownKey, "1"))
	mr.SetTTL(cooldownKey, 100*time.Second)

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineA, pickTestConf, true))

	assert.Equal(t, uint32(0), resp.ErrorCode)
	assert.Len(t, *captured, 1, "同落点重连照常补发路由")
	assert.Equal(t, 100*time.Second, mr.TTL(cooldownKey))
	assert.Equal(t, "1", scenePlayerCountRaw(t, sc, lineA), "同落点不重复计人数")
}

// 跨节点切线(生产配置,无交接标记):第一跳拿 18,**冷却在这一跳起算**;scene 节点存盘写标记后
// 重发的那一条不带 client_channel_pick,不过预检(冷却键在也不拦)、也不刷新冷却。
// 冷却没过时再点一次,在预检就被 24 挡住,不会再进入一轮交接。
func TestChannelPick_CrossNodeFirstHopGetsHandoffPendingAndStartsCooldown(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	sc.Config.AllowUnsafeCrossNodeHandoff = false
	const (
		playerID = uint64(8109)
		lineA    = uint64(8881)
		lineB    = uint64(8882)
	)
	seedPickChannel(t, mr, pickTestConf, lineA, "10", "3")
	seedPickChannel(t, mr, pickTestConf, lineB, "20", "4")
	placePlayerOnLine(t, sc, playerID, lineA, "10")
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	withReachableSceneNode(t, sc, "10", "20")
	cooldownKey := channelSwitchCooldownKey(playerID)

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineB, pickTestConf, true))
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "跨节点第一跳必拿 18")
	assert.Empty(t, *captured)
	assert.Equal(t, lineA, locatedSceneID(t, sc, playerID))
	assert.Equal(t, "4", scenePlayerCountRaw(t, sc, lineB))
	require.True(t, mr.Exists(cooldownKey), "18 也起算冷却:真正落点的重发不带标记,只能记在这一跳")
	assert.Equal(t, 10*time.Second, mr.TTL(cooldownKey))

	// 源 scene 冻结 → 存盘 → 写标记 → 重发(不带 client_channel_pick)。
	mr.FastForward(4 * time.Second)
	writeHandoffMarker(t, sc, playerID, 1)
	resp = enterChannelPick(t, sc, channelPickRequest(playerID, lineB, pickTestConf, false))
	assert.Equal(t, uint32(0), resp.ErrorCode, "重发不带标记,冷却键在也直接放行")
	assert.Equal(t, lineB, locatedSceneID(t, sc, playerID))
	assert.Equal(t, "5", scenePlayerCountRaw(t, sc, lineB))
	assert.Len(t, *captured, 1)
	assert.Equal(t, 6*time.Second, mr.TTL(cooldownKey), "重发那一跳不刷新冷却")

	resp = enterChannelPick(t, sc, channelPickRequest(playerID, lineA, pickTestConf, true))
	assert.Equal(t, constants.ErrChannelSwitchCooldown, resp.ErrorCode, "冷却内再切:预检先于换手门,拿 24 而不是 18")
	assert.Len(t, *captured, 1)
}

// CooldownSeconds < 0 = 不设冷却:连续切线不被拒,也不写冷却键。
func TestChannelPick_NegativeCooldownDisablesCooldown(t *testing.T) {
	sc, mr, captured := newChannelPickCtx(t)
	sc.Config.ChannelSwitch.CooldownSeconds = -1
	const (
		playerID = uint64(8110)
		lineA    = uint64(8891)
		lineB    = uint64(8892)
	)
	seedPickChannel(t, mr, pickTestConf, lineA, "10", "1")
	seedPickChannel(t, mr, pickTestConf, lineB, "10", "0")
	placePlayerOnLine(t, sc, playerID, lineA, "10")
	cooldownKey := channelSwitchCooldownKey(playerID)

	for _, target := range []uint64{lineB, lineA, lineB} {
		resp := enterChannelPick(t, sc, channelPickRequest(playerID, target, pickTestConf, true))
		require.Equal(t, uint32(0), resp.ErrorCode, "target=%d", target)
		assert.Equal(t, target, locatedSceneID(t, sc, playerID))
		assert.False(t, mr.Exists(cooldownKey), "冷却关闭时不写冷却键")
	}
	assert.Len(t, *captured, 3)
}

// 冷却关闭时,残留的冷却键(改配置之前写下的)也不再拦人。
func TestChannelPick_NegativeCooldownIgnoresLeftoverKey(t *testing.T) {
	sc, mr, _ := newChannelPickCtx(t)
	sc.Config.ChannelSwitch.CooldownSeconds = -1
	const (
		playerID = uint64(8112)
		lineA    = uint64(8895)
		lineB    = uint64(8896)
	)
	seedPickChannel(t, mr, pickTestConf, lineA, "10", "1")
	seedPickChannel(t, mr, pickTestConf, lineB, "10", "0")
	placePlayerOnLine(t, sc, playerID, lineA, "10")
	require.NoError(t, mr.Set(channelSwitchCooldownKey(playerID), "1"))

	resp := enterChannelPick(t, sc, channelPickRequest(playerID, lineB, pickTestConf, true))
	assert.Equal(t, uint32(0), resp.ErrorCode)
}

// ── fail-closed 与纯函数 ──────────────────────────────────────────────────

// 预检读 Redis 失败 → ErrRedis(fail-closed),不放行、不起算冷却。
// 直接调预检:EnterScene 在它之前还有别的 Redis 读,注入的错误会先被那些读拦下。
func TestChannelPick_RedisFailureFailsClosed(t *testing.T) {
	sc, mr, _ := newChannelPickCtx(t)
	const lineA = uint64(8901)
	seedPickChannel(t, mr, pickTestConf, lineA, "10", "0")
	logic := NewEnterSceneLogic(context.Background(), sc)

	mr.SetError("ERR injected")
	resp, chargeCooldown := logic.checkClientChannelPick(channelPickRequest(8113, lineA, pickTestConf, true), nil, testZoneId)
	mr.SetError("")

	require.NotNil(t, resp)
	assert.Equal(t, constants.ErrRedis, resp.ErrorCode)
	assert.False(t, chargeCooldown)
}

// locateWorldChannel:hint 命中 / hint 未命中后遍历 / 回收中优先于在役 / 不是分线。
func TestLocateWorldChannel(t *testing.T) {
	sc, mr, _ := newChannelPickCtx(t)
	const (
		activeInOther = uint64(8911)
		closingInMain = uint64(8912)
		inBothSets    = uint64(8913)
		notAChannel   = uint64(8914)
	)
	seedPickChannel(t, mr, pickTestOtherConf, activeInOther, "10", "0")
	seedClosingChannel(t, mr, pickTestConf, closingInMain, "10", "0")
	seedPickChannel(t, mr, pickTestConf, inBothSets, "10", "0")
	seedClosingChannel(t, mr, pickTestConf, inBothSets, "10", "0")

	cases := []struct {
		name           string
		sceneID        uint64
		hintConfID     uint64
		wantConfID     uint64
		wantMembership worldChannelMembership
	}{
		{"hint 命中", activeInOther, pickTestOtherConf, pickTestOtherConf, worldChannelActive},
		{"不带 hint,遍历命中", activeInOther, 0, pickTestOtherConf, worldChannelActive},
		{"hint 是别的图,遍历命中真正所属的图", activeInOther, pickTestConf, pickTestOtherConf, worldChannelActive},
		{"hint 不在 World 表里,遍历命中", activeInOther, 9901, pickTestOtherConf, worldChannelActive},
		{"回收中", closingInMain, pickTestConf, pickTestConf, worldChannelDraining},
		{"回收中,不带 hint", closingInMain, 0, pickTestConf, worldChannelDraining},
		{"同时在两个集合里按回收中", inBothSets, pickTestConf, pickTestConf, worldChannelDraining},
		{"不是分线", notAChannel, pickTestConf, 0, worldChannelNotMember},
		{"不是分线,不带 hint", notAChannel, 0, 0, worldChannelNotMember},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			confID, membership, err := locateWorldChannel(sc, testZoneId, tc.sceneID, tc.hintConfID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantMembership, membership)
			assert.Equal(t, tc.wantConfID, confID)
		})
	}

	// 别的 zone 里没有这些线:集合按 zone 分,不会串。
	_, membership, err := locateWorldChannel(sc, testZoneId+1, activeInOther, pickTestOtherConf)
	require.NoError(t, err)
	assert.Equal(t, worldChannelNotMember, membership)
}

// 线恰好在 locateWorldChannel 的两次 SISMEMBER 之间从在役移到回收中(一步完成的原子移动):必须判成回收中。
// 钉住的是读的顺序 —— 先读回收中、再读在役的话,这里两头落空,会判成「不是分线」,整段预检
// (上限 / 冷却 / conf 校验)被跳过。
//
// 用例模拟的是原子移动。现状下 beginDrainWorldChannel 是 SREM 与 SADD 两步,两次读都落在两步之间时
// 仍会两头落空(channel_pick.go 文件头登记的窗口),那要到 world_autoscale.go 里才能根治。
// 本用例用 SetPreHook,不能与 mr.SetError 同时用(两者共用同一个钩子槽位)。
func TestLocateWorldChannel_MoveToDrainingBetweenTheTwoReadsIsSeenAsDraining(t *testing.T) {
	sc, mr, _ := newChannelPickCtx(t)
	const (
		movingLine  = uint64(8931)
		stayingLine = uint64(8932) // 留在在役集合里:移走 movingLine 之后这把集合键仍然存在,下面才能直接查成员
	)
	seedPickChannel(t, mr, pickTestConf, movingLine, "10", "0")
	seedPickChannel(t, mr, pickTestConf, stayingLine, "10", "0")
	member := fmt.Sprintf("%d", movingLine)
	activeKey := worldChannelsKey(testZoneId, pickTestConf)
	drainingKey := worldDrainingSetKey(testZoneId, pickTestConf)

	// 钩子在第二条 SISMEMBER 执行之前(也就是第一条已经返回之后)把线移过去。
	// 钩子跑在 miniredis 的连接 goroutine 上,不在这里用 require;移没移成由下面直接读集合来断言。
	var sismemberCalls atomic.Int32
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		if cmd == "SISMEMBER" && sismemberCalls.Add(1) == 2 {
			_, _ = mr.SRem(activeKey, member)
			_, _ = mr.SAdd(drainingKey, member)
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	confID, membership, err := locateWorldChannel(sc, testZoneId, movingLine, pickTestConf)
	require.NoError(t, err)

	require.GreaterOrEqual(t, sismemberCalls.Load(), int32(2), "钩子没触发,用例空转")
	stillActive, err := mr.SIsMember(activeKey, member)
	require.NoError(t, err)
	require.False(t, stillActive, "线应当已被钩子移出在役集合,否则用例空转")
	nowDraining, err := mr.SIsMember(drainingKey, member)
	require.NoError(t, err)
	require.True(t, nowDraining, "线应当已被钩子移进回收中集合,否则用例空转")

	assert.Equal(t, worldChannelDraining, membership)
	assert.Equal(t, pickTestConf, confID)
}

func TestIsChannelSwitch(t *testing.T) {
	const target = uint64(600)
	cases := []struct {
		name string
		loc  *scene_manager.PlayerLocation
		want bool
	}{
		{"没有位置记录(首次落点)", nil, false},
		{"在别的线上", &scene_manager.PlayerLocation{SceneId: 500, NodeId: "10", ZoneId: testZoneId}, true},
		{"已经在目标线上(同落点重连)", &scene_manager.PlayerLocation{SceneId: target, NodeId: "10", ZoneId: testZoneId}, false},
		{"等待落点(node_id 为空)", &scene_manager.PlayerLocation{ZoneId: testZoneId, OwnerEpoch: 3}, false},
		{"有节点但 scene_id 为 0", &scene_manager.PlayerLocation{NodeId: "10", ZoneId: testZoneId}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isChannelSwitch(tc.loc, target))
		})
	}
}

// 起算冷却的应答:成功(0)与 ErrHandoffPending(18)。其它拒绝 —— 包括可重试的 17 / 19 —— 都不起算。
func TestChannelSwitchStartsCooldown(t *testing.T) {
	respWith := func(code uint32) *scene_manager.EnterSceneResponse {
		return &scene_manager.EnterSceneResponse{ErrorCode: code}
	}
	assert.True(t, channelSwitchStartsCooldown(respWith(0), nil))
	assert.True(t, channelSwitchStartsCooldown(respWith(constants.ErrHandoffPending), nil))

	for _, code := range []uint32{
		constants.ErrNoAvailableNode, constants.ErrUpdateLocation, constants.ErrKafkaRoute, constants.ErrRedis,
		constants.ErrSceneReentryBarrier, constants.ErrOwnerEpochConflict,
		constants.ErrHomeZoneUnavailable, constants.ErrHomeZoneMerging,
		constants.ErrChannelUnavailable, constants.ErrChannelFull, constants.ErrChannelSwitchCooldown,
	} {
		assert.False(t, channelSwitchStartsCooldown(respWith(code), nil), "code=%d", code)
	}
	assert.False(t, channelSwitchStartsCooldown(nil, nil))
	assert.False(t, channelSwitchStartsCooldown(respWith(0), assert.AnError))
}

// 新错误码的数值是 scene 节点手抄的契约(与 18 同款),钉住。
func TestChannelErrorCodes(t *testing.T) {
	assert.Equal(t, uint32(22), constants.ErrChannelUnavailable)
	assert.Equal(t, uint32(23), constants.ErrChannelFull)
	assert.Equal(t, uint32(24), constants.ErrChannelSwitchCooldown)
	assert.Equal(t, "player:%d:channel_switch_cooldown", ChannelSwitchCooldownKeyFmt)
	assert.Equal(t, "player:42:channel_switch_cooldown", channelSwitchCooldownKey(42))
}

// client_channel_pick 是业务语义,必须进去重指纹(同一 request_id 带与不带是两条不同的请求);
// 不带它的请求指纹不变(proto3 不序列化 false)—— 后者由既有的指纹用例覆盖。
func TestEnterSceneFingerprint_ClientChannelPickIsPartOfRequest(t *testing.T) {
	base := &scene_manager.EnterSceneRequest{
		PlayerId: 8114, SceneId: 8921, ZoneId: testZoneId, RequestId: "fp-pick",
	}
	plain, err := enterSceneRequestFingerprint(base)
	require.NoError(t, err)

	picked := gproto.Clone(base).(*scene_manager.EnterSceneRequest)
	picked.ClientChannelPick = true
	pickedFingerprint, err := enterSceneRequestFingerprint(picked)
	require.NoError(t, err)
	assert.NotEqual(t, plain, pickedFingerprint)
}

// ── 配置取值方法 ──────────────────────────────────────────────────────────

func TestChannelSwitchConfig_EffectiveValues(t *testing.T) {
	const day = int64(24 * 60 * 60)

	cooldownCases := []struct{ configured, want int64 }{
		{0, 10},        // 缺省 → 默认
		{-1, 0},        // 负数 → 不设冷却
		{-3600, 0},     // 负多少都一样
		{1, 1},         // 正数原样
		{30, 30},       // 正数原样
		{day, day},     // 上限本身
		{day + 1, day}, // 超过一天按一天
	}
	for _, tc := range cooldownCases {
		cfg := config.ChannelSwitchConfig{CooldownSeconds: tc.configured}
		assert.Equal(t, tc.want, cfg.EffectiveCooldownSeconds(), "CooldownSeconds=%d", tc.configured)
	}

	busyCases := []struct{ configured, want int64 }{
		{0, 60},    // 缺省 → 默认
		{-5, 1},    // 钳到下限
		{1, 1},     // 下限本身
		{60, 60},   // 范围内原样
		{100, 100}, // 上限本身
		{101, 100}, // 钳到上限
	}
	for _, tc := range busyCases {
		cfg := config.ChannelSwitchConfig{BusyPercent: tc.configured}
		assert.Equal(t, tc.want, cfg.EffectiveBusyPercent(), "BusyPercent=%d", tc.configured)
	}

	refreshCases := []struct{ configured, want int64 }{
		{0, 2},         // 缺省 → 默认
		{-1, 0},        // 负数 → 不发布
		{1, 1},         // 正数原样
		{5, 5},         // 正数原样
		{day + 1, day}, // 超过一天按一天
	}
	for _, tc := range refreshCases {
		cfg := config.ChannelSwitchConfig{DirectoryRefreshSeconds: tc.configured}
		assert.Equal(t, tc.want, cfg.EffectiveDirectoryRefreshSeconds(), "DirectoryRefreshSeconds=%d", tc.configured)
	}
}

// 每线上限:ChannelSwitch.MaxPlayersPerChannel → WorldAutoscale.ScaleOutPlayerThreshold → 2000,恒为正。
func TestConfig_ChannelMaxPlayers(t *testing.T) {
	const uint32Max = int64(1<<32 - 1)
	cases := []struct {
		name               string
		maxPlayers         int64
		scaleOutThreshold  int64
		wantChannelMaximum int64
	}{
		{"都没配 → 2000", 0, 0, 2000},
		{"只配了扩容线 → 跟随", 0, 1500, 1500},
		{"两个都配 → 自己的优先", 300, 1500, 300},
		{"自己的为负 → 当作没配,跟随扩容线", -1, 1500, 1500},
		{"两个都为负 → 2000", -1, -1, 2000},
		{"扩容线为负 → 2000", 0, -7, 2000},
		{"上限为 1", 1, 1500, 1},
		{"自己的超过 uint32 → 钳住", 1 << 40, 0, uint32Max},
		{"跟随的扩容线超过 uint32 → 钳住", 0, 1 << 40, uint32Max},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.ChannelSwitch.MaxPlayersPerChannel = tc.maxPlayers
			cfg.WorldAutoscale.ScaleOutPlayerThreshold = tc.scaleOutThreshold
			got := cfg.ChannelMaxPlayers()
			assert.Equal(t, tc.wantChannelMaximum, got)
			assert.Positive(t, got)
		})
	}
}
