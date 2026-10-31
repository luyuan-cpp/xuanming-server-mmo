package logic

// 开局管线里不经 SQL 的判定:配表指纹比对(设计决策 D14)、备战失败的定罪口径(global-match-hierarchical-pools.md §5.5)。
// 节点镜像用 NodeWatcher.Upsert 直接灌,不起 etcd。

import (
	"errors"
	"testing"

	"match/internal/discovery"
	"match/internal/svc"

	battlepb "proto/battle"
	scenepb "proto/scene"

	"shared/generated/pb/table"
	"shared/generated/tip"
	"shared/snowflake"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// testBattle7Instance 是 newGatherSvcCtx 灌的 battle 节点 7 的实例 uuid。
const testBattle7Instance = "5b0f3c1e-7a42-4d0e-9a51-2f6c8d9e0b17"

// newGatherSvcCtx 在 newTestSvcCtx 之上补齐发号器与节点镜像:
// zone1 / zone2 各一个 node_id=1 的 scene(与 setPlayerLocation 写的 NodeId "1" 对齐),battle 池一个节点。
func newGatherSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.BattleIDGen = snowflake.NewNode(1)
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
		return preparePlayerFenced(svcCtx, player, battle, 7, 5000, 4000, before)
	}

	// 没有位置:玩家不在线或未进场,定罪,不发 RPC。
	endpoint, _, err := prepare(nil)
	require.Empty(t, endpoint)
	require.ErrorIs(t, err, errPrepareRejected)
	require.Empty(t, requests)

	setPlayerLocation(t, mr, player, 1)

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
