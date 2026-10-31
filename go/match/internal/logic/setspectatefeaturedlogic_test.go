package logic

// 赛事局运营入口(SetSpectateFeatured)与观战列表的赛事局排序(battle-spectate-tiered-fanout.md §15.6)的 miniredis 测试。
// 成局原件走 fakeSpectateFormations(spectate_test.go)。

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"match/internal/config"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"
	base "proto/common/base"
	pb "proto/match"

	"shared/generated/pb/table"
	"shared/serviceauth"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

var testOpsCallerSecret = strings.Repeat("p", 32)

func newFeaturedSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	svcCtx, mr := newRelaySvcCtx(t)
	verifier, err := serviceauth.New(map[string]string{config.SpectateOpsCaller: testOpsCallerSecret})
	require.NoError(t, err)
	svcCtx.SpectateOpsAuth = verifier
	return svcCtx, mr
}

func signedFeatured(t *testing.T, battleId uint64, featured bool) *pb.SetSpectateFeaturedRequest {
	t.Helper()
	request := &pb.SetSpectateFeaturedRequest{BattleId: battleId, Featured: featured}
	require.NoError(t, serviceauth.Sign(pb.MatchInternal_SetSpectateFeatured_FullMethodName, request, config.SpectateOpsCaller, testOpsCallerSecret, uint64(time.Now().UnixMilli())))
	return request
}

func setFeatured(t *testing.T, svcCtx *svc.ServiceContext, battleId uint64, featured bool) *pb.SetSpectateFeaturedResponse {
	t.Helper()
	resp, err := NewSetSpectateFeaturedLogic(context.Background(), svcCtx).SetSpectateFeatured(signedFeatured(t, battleId, featured))
	require.NoError(t, err)
	return resp
}

// 只放行 ops 对本方法的调用:OpsCallerSecret 为空(verifier 为 nil)、未签名、别的调用方、别的方法、改过的正文、过期、客户端来源一律回 DENIED,
// 且什么都没写。回的是业务拒绝而不是 gRPC 错误:发布准入把 gRPC 错误记成未知结局,调用一次本副本的排空证明就一直 HOLD。
func TestSetSpectateFeaturedRejectsBadCallerBeforeAnyWrite(t *testing.T) {
	const battleId = uint64(884001)
	sign := func(method, caller string, stamp uint64) *pb.SetSpectateFeaturedRequest {
		request := &pb.SetSpectateFeaturedRequest{BattleId: battleId, Featured: true}
		require.NoError(t, serviceauth.Sign(method, request, caller, testOpsCallerSecret, stamp))
		return request
	}
	now := uint64(time.Now().UnixMilli())
	method := pb.MatchInternal_SetSpectateFeatured_FullMethodName
	cases := []struct {
		name    string
		request func() *pb.SetSpectateFeaturedRequest
		mutate  func(*svc.ServiceContext)
		ctx     context.Context
	}{
		{name: "运营入口未开启", request: func() *pb.SetSpectateFeaturedRequest { return sign(method, config.SpectateOpsCaller, now) },
			mutate: func(s *svc.ServiceContext) { s.SpectateOpsAuth = nil }},
		{name: "未签名", request: func() *pb.SetSpectateFeaturedRequest {
			return &pb.SetSpectateFeaturedRequest{BattleId: battleId, Featured: true}
		}},
		{name: "别的调用方", request: func() *pb.SetSpectateFeaturedRequest { return sign(method, "guild", now) }},
		{name: "别的方法", request: func() *pb.SetSpectateFeaturedRequest {
			return sign(pb.MatchInternal_CancelActivityBattle_FullMethodName, config.SpectateOpsCaller, now)
		}},
		{name: "改过的正文", request: func() *pb.SetSpectateFeaturedRequest {
			request := sign(method, config.SpectateOpsCaller, now)
			request.BattleId++
			return request
		}},
		{name: "过期", request: func() *pb.SetSpectateFeaturedRequest { return sign(method, config.SpectateOpsCaller, 1) }},
		{name: "客户端来源", request: func() *pb.SetSpectateFeaturedRequest { return sign(method, config.SpectateOpsCaller, now) },
			ctx: ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{SessionId: 1, PlayerId: 11})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newFeaturedSvcCtx(t)
			relay := stubRelayRPCs(t)
			registerBattle(t, svcCtx, battleId, nowMs())
			if c.mutate != nil {
				c.mutate(svcCtx)
			}
			ctx := c.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			resp, err := NewSetSpectateFeaturedLogic(ctx, svcCtx).SetSpectateFeatured(c.request())
			require.NoError(t, err, "拒绝不能回 gRPC 错误")
			require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_DENIED, resp.GetReject())
			require.False(t, mr.Exists(spectateFeaturedKey(battleId)))
			require.Empty(t, relay.promotes)
		})
	}
}

func TestSetSpectateFeaturedRejectsMissingAndPrivateBattles(t *testing.T) {
	svcCtx, mr := newFeaturedSvcCtx(t)
	relay := stubRelayRPCs(t)
	const missing, private = uint64(884010), uint64(884011)
	registerPrivateBattle(t, svcCtx, private, false)

	for _, featured := range []bool{true, false} {
		require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NOT_FOUND, setFeatured(t, svcCtx, missing, featured).GetReject())
		require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_PRIVATE, setFeatured(t, svcCtx, private, featured).GetReject())
	}
	require.False(t, mr.Exists(spectateFeaturedKey(missing)))
	require.False(t, mr.Exists(spectateFeaturedKey(private)))
	require.Empty(t, relay.promotes)
}

// 置位:写标记(TTL = spectateTTLSeconds)并立即升 L2;直连席没满的场也一样。取消只删标记、不降档。重复设置同值幂等成功。
func TestSetSpectateFeaturedPromotesAndCancelKeepsTier(t *testing.T) {
	svcCtx, mr := newFeaturedSvcCtx(t)
	relay := stubRelayRPCs(t)
	const battleId = uint64(884020)
	registerBattle(t, svcCtx, battleId, nowMs())

	resp := setFeatured(t, svcCtx, battleId, true)
	require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, resp.GetReject())
	require.Equal(t, tierBroadcast, resp.GetTier())
	require.Equal(t, time.Duration(spectateTTLSeconds)*time.Second, mr.TTL(spectateFeaturedKey(battleId)))
	require.Len(t, relay.promotes, 1)
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx))

	require.Equal(t, tierBroadcast, setFeatured(t, svcCtx, battleId, true).GetTier())
	require.Len(t, relay.promotes, 1, "已是广播档不再调 battle")

	resp = setFeatured(t, svcCtx, battleId, false)
	require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, resp.GetReject())
	require.Equal(t, tierBroadcast, resp.GetTier(), "取消不降档")
	require.False(t, mr.Exists(spectateFeaturedKey(battleId)))
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx), "本场还在广播,配额不退")
}

// 配额满:标记照写,档位不变,不报错;之后这场有观众进 L1 时自动再试(featured 触发升档)。
func TestSetSpectateFeaturedQuotaFullThenPromotedByViewer(t *testing.T) {
	svcCtx, mr := newFeaturedSvcCtx(t)
	svcCtx.Config.Spectate.BroadcastMaxConcurrent = 1
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 0 // 只认运营标记
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7501), uint64(884030)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), "999999")
	require.NoError(t, err)

	resp := setFeatured(t, svcCtx, battleId, true)
	require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, resp.GetReject())
	require.Equal(t, tierRelay, resp.GetTier())
	require.True(t, mr.Exists(spectateFeaturedKey(battleId)))
	require.Empty(t, relay.promotes)

	_, err = svcCtx.MatchRedis.Zrem(spectateBroadcastActiveKey, "999999")
	require.NoError(t, err)
	mr.FastForward(spectatePromoteLeaseSeconds * time.Second) // 上一次尝试的租约到期
	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Len(t, relay.promotes, 1, "赛事局的观众触发升档")
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
}

// 标记时 battle 回房间不存在(战斗结束后成局仍是 state 4,读得到记录):回 NOT_FOUND 并撤掉标记,
// 否则一场不存在的战斗会排在观战列表最前。其它升档失败不算错,标记照留。
func TestSetSpectateFeaturedMissingRoomRejectsAndUnmarks(t *testing.T) {
	reply := func(tip uint32) func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		return func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
			return &battlepb.PromoteSpectateBroadcastResponse{ErrorMessage: tipErr(tip, "拒绝")}, nil
		}
	}
	t.Run("房间不存在", func(t *testing.T) {
		svcCtx, mr := newFeaturedSvcCtx(t)
		relay := stubRelayRPCs(t)
		relay.onPromote = reply(uint32(table.CommonError_kEntityIsNull))
		const battleId = uint64(884032)
		registerBattle(t, svcCtx, battleId, nowMs())

		require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NOT_FOUND, setFeatured(t, svcCtx, battleId, true).GetReject())
		require.False(t, mr.Exists(spectateFeaturedKey(battleId)))
		count, err := svcCtx.MatchRedis.Zcard(spectateFeaturedActiveKey)
		require.NoError(t, err)
		require.Zero(t, count)
		require.Zero(t, broadcastQuotaCount(t, svcCtx))
	})
	t.Run("其它拒绝", func(t *testing.T) {
		svcCtx, mr := newFeaturedSvcCtx(t)
		relay := stubRelayRPCs(t)
		relay.onPromote = reply(uint32(table.CommonError_kInvalidParameter))
		const battleId = uint64(884033)
		registerBattle(t, svcCtx, battleId, nowMs())

		resp := setFeatured(t, svcCtx, battleId, true)
		require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, resp.GetReject())
		require.Equal(t, tierDirect, resp.GetTier())
		require.True(t, mr.Exists(spectateFeaturedKey(battleId)))
	})
}

// 中继不可用(Q6):只写标记,不升 L2;列表照样排在前面。
func TestSetSpectateFeaturedRelayUnusableOnlyMarks(t *testing.T) {
	svcCtx, mr := newFeaturedSvcCtx(t)
	svcCtx.Config.Spectate = config.SpectateConf{BroadcastMaxConcurrent: 4}
	relay := stubRelayRPCs(t)
	const battleId = uint64(884040)
	registerBattle(t, svcCtx, battleId, nowMs())

	resp := setFeatured(t, svcCtx, battleId, true)
	require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, resp.GetReject())
	require.Equal(t, tierDirect, resp.GetTier())
	require.True(t, mr.Exists(spectateFeaturedKey(battleId)))
	require.Empty(t, relay.promotes)
}

func listIds(t *testing.T, svcCtx *svc.ServiceContext, limit uint32) ([]uint64, []bool) {
	t.Helper()
	resp, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).ListWatchableBattles(&pb.ListWatchableBattlesRequest{Limit: limit})
	require.NoError(t, err)
	ids := make([]uint64, 0, len(resp.GetBattles()))
	flags := make([]bool, 0, len(resp.GetBattles()))
	for _, battle := range resp.GetBattles() {
		ids = append(ids, battle.GetBattleId())
		flags = append(flags, battle.GetFeatured())
	}
	return ids, flags
}

// 赛事局排最前,即使开局最早、已被挤出第一页也要在;只出现一次;取消后回到按开局时间的位置。
func TestListWatchableBattlesPutsFeaturedFirst(t *testing.T) {
	svcCtx, _ := newFeaturedSvcCtx(t)
	stubRelayRPCs(t)
	now := nowMs()
	const oldest, middle, newest = uint64(884050), uint64(884051), uint64(884052)
	registerBattle(t, svcCtx, oldest, now-3000)
	registerBattle(t, svcCtx, middle, now-2000)
	registerBattle(t, svcCtx, newest, now-1000)

	ids, flags := listIds(t, svcCtx, 2)
	require.Equal(t, []uint64{newest, middle}, ids)
	require.Equal(t, []bool{false, false}, flags)

	require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, setFeatured(t, svcCtx, oldest, true).GetReject())
	ids, flags = listIds(t, svcCtx, 2)
	require.Equal(t, []uint64{oldest, newest}, ids, "赛事局不靠活跃集合的第一页,limit 也不超")
	require.Equal(t, []bool{true, false}, flags)

	require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, setFeatured(t, svcCtx, newest, true).GetReject())
	ids, flags = listIds(t, svcCtx, 10)
	require.Len(t, ids, 3, "赛事局不重复出现")
	require.Equal(t, []bool{true, true, false}, flags)
	require.Equal(t, middle, ids[2])

	require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, setFeatured(t, svcCtx, oldest, false).GetReject())
	require.Equal(t, pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, setFeatured(t, svcCtx, newest, false).GetReject())
	ids, flags = listIds(t, svcCtx, 10)
	require.Equal(t, []uint64{newest, middle, oldest}, ids)
	require.Equal(t, []bool{false, false, false}, flags)
}

// 赛事局的成局已不在:不出现在列表里,索引成员现场摘掉。
func TestListWatchableBattlesDropsFeaturedWithoutRecord(t *testing.T) {
	svcCtx, mr := newFeaturedSvcCtx(t)
	const gone, live = uint64(884060), uint64(884061)
	registerBattle(t, svcCtx, live, nowMs())
	_, err := mr.ZAdd(spectateFeaturedActiveKey, float64(testRelayDeadlineMs), strconv.FormatUint(gone, 10))
	require.NoError(t, err)

	ids, _ := listIds(t, svcCtx, 10)
	require.Equal(t, []uint64{live}, ids)
	count, err := svcCtx.MatchRedis.Zcard(spectateFeaturedActiveKey)
	require.NoError(t, err)
	require.Zero(t, count)
}

// 赛事局索引按到期清理:定期清理顺手清掉过期成员,未到期的保留(谁来调见 TestSpectateMaintenanceLoopPrunesIndexes)。
func TestCleanupExpiredSpectateIndexPrunesFeaturedIndex(t *testing.T) {
	svcCtx, mr := newFeaturedSvcCtx(t)
	_, err := mr.ZAdd(spectateFeaturedActiveKey, float64(nowMs()-1000), "884070")
	require.NoError(t, err)
	_, err = mr.ZAdd(spectateFeaturedActiveKey, float64(testRelayDeadlineMs), "884071")
	require.NoError(t, err)

	cleanupExpiredSpectateIndex(svcCtx)

	members, err := mr.ZMembers(spectateFeaturedActiveKey)
	require.NoError(t, err)
	require.Equal(t, []string{"884071"}, members)
}
