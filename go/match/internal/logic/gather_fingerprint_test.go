package logic

// 开局管线里不经 SQL 的判定:配表指纹比对(设计决策 D14)、备战失败的定罪口径(global-match-hierarchical-pools.md §5.5)。
// 节点镜像用 NodeWatcher.Upsert 直接灌,不起 etcd。

import (
	"errors"
	"sync/atomic"
	"testing"

	"match/internal/discovery"
	"match/internal/playercontract"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"
	scenepb "proto/scene"

	"shared/generated/pb/table"
	"shared/generated/tip"
	"shared/idsegment"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// testBattle7Instance 是 newGatherSvcCtx 灌的 battle 节点 7 的实例 uuid。
const testBattle7Instance = "5b0f3c1e-7a42-4d0e-9a51-2f6c8d9e0b17"

// issuerShapedBattleIDs 发与生产 segmentBattleIDs 同形的号:计数器值经 MixID 打散,没有位布局、不单调,
// 不会撞上用例里手写的小号。battle_id 只由发号器发(AGENTS.md §7 第 1 条),替身不能用 snowflake。
type issuerShapedBattleIDs struct{ counter atomic.Uint64 }

func (g *issuerShapedBattleIDs) Generate() (uint64, error) {
	return idsegment.MixID(g.counter.Add(1)), nil
}

// newGatherSvcCtx 在 newTestSvcCtx 之上补齐发号器与节点镜像:
// zone1 / zone2 各一个 node_id=1 的 scene(与 setPlayerLocation 写的 NodeId "1" 对齐),battle 池一个节点。
func newGatherSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.BattleIDGen = &issuerShapedBattleIDs{}
	svcCtx.SceneNodes = discovery.NewNodeWatcher("scene", svc.SceneNodeRpcPrefix, nil, nil)
	svcCtx.SceneNodes.Upsert(svc.SceneNodeRpcPrefix+"zone/1/1", discovery.NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "scene-z1"})
	svcCtx.SceneNodes.Upsert(svc.SceneNodeRpcPrefix+"zone/2/1", discovery.NodeEntry{NodeId: 1, ZoneId: 2, Endpoint: "scene-z2"})
	svcCtx.BattleNodes = discovery.NewNodeWatcher("battle", svc.BattleNodeRpcPrefix, nil, nil)
	svcCtx.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"7", discovery.NodeEntry{NodeId: 7, NodeUuid: testBattle7Instance, Endpoint: "battle-7"})
	return svcCtx, mr
}

// checkTableFingerprints 的多数派 / 肇事者选择(纯函数,不走 gather):
// 三人组两同一异 → 异者是肇事者;空指纹不参与多数派;全员皆空记第一位。
func TestCheckTableFingerprintsPicksMinorityAsOffender(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.Config.TableFingerprintMode = tableFingerprintEnforce
	group := func(fps ...string) []preparedMember {
		out := make([]preparedMember, 0, len(fps))
		for i, fp := range fps {
			out = append(out, preparedMember{playerId: uint64(100 + i), tableFingerprint: fp})
		}
		return out
	}

	fp, offender, ok := checkTableFingerprints(svcCtx, "m", 1, group("A", "A", "A"))
	require.True(t, ok)
	require.Equal(t, "A", fp)
	require.Zero(t, offender)

	fp, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("A", "B", "A"))
	require.False(t, ok)
	require.Empty(t, fp)
	require.Equal(t, uint64(101), offender, "与多数派 A 不一致的第二人是肇事者")

	fp, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("", "B", "B"))
	require.False(t, ok)
	require.Empty(t, fp)
	require.Equal(t, uint64(100), offender, "空指纹不能凑多数派,不回报者是肇事者")

	_, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("", ""))
	require.False(t, ok)
	require.Equal(t, uint64(100), offender, "全员皆空没有多数派,记第一位")

	// warn(默认,空值同义):不一致放行,不透传任何一方的指纹。
	svcCtx.Config.TableFingerprintMode = tableFingerprintWarn
	fp, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("A", "B"))
	require.True(t, ok, "warn 放行")
	require.Empty(t, fp)
	require.Zero(t, offender)
	svcCtx.Config.TableFingerprintMode = ""
	_, _, ok = checkTableFingerprints(svcCtx, "m", 1, group("A", "B"))
	require.True(t, ok, "空值按默认 warn 处理")

	// off:不比对、不透传,一致也不透传。
	svcCtx.Config.TableFingerprintMode = tableFingerprintOff
	fp, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("A", "A"))
	require.True(t, ok)
	require.Empty(t, fp, "off 下即使一致也不透传")
	require.Zero(t, offender)
}

// 备战失败的定罪口径:只有确定由玩家自身状态导致的失败才带 errPrepareRejected(终止他的单排票);
// 读位置失败、RPC 超时或断连、scene 内部故障码都是结果未知,不定罪。endpoint 为空 = 还没定位到对端(no_location)。
func TestPreparePlayerFencedClassifiesFailures(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	const player, battle = uint64(9001), uint64(77)
	var reply *scenepb.PrepareBattleResponse
	var replyErr error
	var requests []*scenepb.PrepareBattleRequest
	prev := prepareBattleFn
	prepareBattleFn = func(_ string, req *scenepb.PrepareBattleRequest) (*scenepb.PrepareBattleResponse, error) {
		requests = append(requests, req)
		return reply, replyErr
	}
	t.Cleanup(func() { prepareBattleFn = prev })
	prepare := func(before func(string) error) (string, *scenepb.PrepareBattleResponse, error) {
		return preparePlayerFenced(svcCtx, player, battle, 7, 5000, 4000, 1, uint32(matchpb.MatchMode_MATCH_MODE_PVE_SOLO), nil, nil, before)
	}

	// 没有位置:玩家不在线或未进场,定罪,不发 RPC。
	endpoint, _, err := prepare(nil)
	require.Empty(t, endpoint)
	require.ErrorIs(t, err, errPrepareRejected)
	require.Empty(t, requests)

	setPlayerLocation(t, mr, player, 1)
	// 成局要从会话记录取未成年人截止时刻(下一条用例),没有会话记录就是不在线
	setPlayerSessionOnline(t, mr, player)

	// 持久化端点失败(工作租约已失):不发冻结 RPC,不定罪。
	endpoint, _, err = prepare(func(got string) error {
		require.Equal(t, "scene-z1", got)
		return errors.New("work lease lost")
	})
	require.Equal(t, "scene-z1", endpoint)
	require.Error(t, err)
	require.NotErrorIs(t, err, errPrepareRejected)
	require.Empty(t, requests, "端点持久化之前不得冻结玩家")

	// RPC 失败:结果未知,不定罪。
	replyErr = errors.New("context deadline exceeded")
	_, _, err = prepare(nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, errPrepareRejected)
	replyErr = nil

	// scene 业务拒绝:定罪。
	bizTip := uint32(table.CommonError_kInvalidParameter)
	require.False(t, tip.IsFault(bizTip))
	reply = &scenepb.PrepareBattleResponse{ErrorMessage: tipErr(bizTip, "拒绝")}
	_, _, err = prepare(nil)
	require.ErrorIs(t, err, errPrepareRejected)

	// scene 内部故障码:不定罪。
	faultTip := uint32(table.TeamError_kTeamInternal)
	require.True(t, tip.IsFault(faultTip))
	reply = &scenepb.PrepareBattleResponse{ErrorMessage: tipErr(faultTip, "故障")}
	_, _, err = prepare(nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, errPrepareRejected)

	// 空快照:scene 协议异常,不定罪。
	reply = &scenepb.PrepareBattleResponse{}
	_, _, err = prepare(nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, errPrepareRejected)

	// 快照玩家号不是备战玩家(或为 0):同样是 scene 协议异常,不定罪;组进 Create 的话 COMPLETE 永远裁决不了(2026-11-05)。
	for _, wrong := range []uint64{player + 1, 0} {
		reply = &scenepb.PrepareBattleResponse{Snapshot: &battlepb.BattlePlayerSnapshot{PlayerId: wrong}, TableFingerprint: "fp"}
		_, resp, err := prepare(nil)
		require.Error(t, err, "快照玩家 %d", wrong)
		require.NotErrorIs(t, err, errPrepareRejected)
		require.Nil(t, resp, "不把别人的快照交给成局")
	}

	// 成功:期限与落点按参数下发。
	requests = nil
	reply = &scenepb.PrepareBattleResponse{Snapshot: &battlepb.BattlePlayerSnapshot{PlayerId: player}, TableFingerprint: "fp"}
	endpoint, resp, err := prepare(nil)
	require.NoError(t, err)
	require.Equal(t, "scene-z1", endpoint)
	require.Equal(t, "fp", resp.GetTableFingerprint())
	require.Len(t, requests, 1)
	require.Equal(t, player, requests[0].GetPlayerId())
	require.Equal(t, battle, requests[0].GetBattleId())
	require.Equal(t, uint32(7), requests[0].GetBattleNodeId())
	require.Equal(t, uint64(5000), requests[0].GetDeadlineMs())
	require.Equal(t, uint64(4000), requests[0].GetPrepareDeadlineMs())
}

// 未成年人当天的截止时刻在会话记录里(PlayerSession.minor_play_deadline_ms),成局时盖到快照上带给 battle:
// battle 据此给他的票封顶、到点关他的战斗直连(docs/design/real-name-anti-addiction.md §5.4)。
// 读不到会话记录就不知道他有没有截止时刻,不能当成不限放进战斗:都在冻结之前判。
func TestPreparePlayerFencedStampsTheMinorPlayDeadlineFromTheSession(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	const player, battle = uint64(9002), uint64(78)
	const deadlineMs = uint64(1_795_784_400_000)
	var reply *scenepb.PrepareBattleResponse
	var requests []*scenepb.PrepareBattleRequest
	prev := prepareBattleFn
	prepareBattleFn = func(_ string, req *scenepb.PrepareBattleRequest) (*scenepb.PrepareBattleResponse, error) {
		requests = append(requests, req)
		return reply, nil
	}
	t.Cleanup(func() { prepareBattleFn = prev })
	prepare := func() (string, *scenepb.PrepareBattleResponse, error) {
		return preparePlayerFenced(svcCtx, player, battle, 7, 5000, 4000, 1, uint32(matchpb.MatchMode_MATCH_MODE_PVE_SOLO), nil, nil, nil)
	}
	setPlayerLocation(t, mr, player, 1)

	// 有位置、没有会话记录:不在线,与没有位置同口径定罪;不冻结。
	endpoint, _, err := prepare()
	require.Empty(t, endpoint)
	require.ErrorIs(t, err, errPrepareRejected)
	require.Empty(t, requests)

	// 会话记录读不出(字节损坏):结果未知,与读位置失败同口径不定罪;同样不冻结。
	require.NoError(t, mr.Set(playercontract.SessionKey(player), "\x0a\x05ab"))
	endpoint, _, err = prepare()
	require.Empty(t, endpoint)
	require.Error(t, err)
	require.NotErrorIs(t, err, errPrepareRejected)
	require.Empty(t, requests)

	// 未成年人:截止时刻按会话记录盖到快照上;scene 不知道年龄,它填的值不作数。
	setPlayerSessionOnline(t, mr, player)
	setSessionMinorPlayDeadline(t, mr, player, deadlineMs)
	reply = &scenepb.PrepareBattleResponse{Snapshot: &battlepb.BattlePlayerSnapshot{PlayerId: player, MinorPlayDeadlineMs: 1}}
	_, resp, err := prepare()
	require.NoError(t, err)
	require.Equal(t, deadlineMs, resp.GetSnapshot().GetMinorPlayDeadlineMs())
	require.Len(t, requests, 1)

	// 成年人(会话记录里是 0):如实写 0,scene 填的值同样被覆盖。
	setPlayerSessionOnline(t, mr, player)
	reply = &scenepb.PrepareBattleResponse{Snapshot: &battlepb.BattlePlayerSnapshot{PlayerId: player, MinorPlayDeadlineMs: deadlineMs}}
	_, resp, err = prepare()
	require.NoError(t, err)
	require.Zero(t, resp.GetSnapshot().GetMinorPlayDeadlineMs())
}
