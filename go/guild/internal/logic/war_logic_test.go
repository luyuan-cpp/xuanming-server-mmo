package logic

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/constants"
	battlepb "proto/battle"
	assetpb "proto/common/asset"
	pb "proto/guild"
	matchpb "proto/match"
	"shared/currencytype"
	"shared/serviceauth"
)

type stubMatchInternalForWar struct {
	matchpb.MatchInternalClient
	captured *matchpb.StartGuildWarBattleRequest
}

func (s *stubMatchInternalForWar) StartGuildWarBattle(_ context.Context, in *matchpb.StartGuildWarBattleRequest, _ ...grpc.CallOption) (*matchpb.StartGuildWarBattleResponse, error) {
	s.captured = proto.Clone(in).(*matchpb.StartGuildWarBattleRequest)
	return &matchpb.StartGuildWarBattleResponse{BattleId: 778899}, nil
}

func TestGuildWarSignedStarterSignsAndPreservesPayload(t *testing.T) {
	const secret = "guild-war-signing-secret-at-least-32-bytes-long!"
	stub := &stubMatchInternalForWar{}
	starter, err := NewSignedWarStarter(stub, secret)
	require.NoError(t, err)

	req := &matchpb.StartGuildWarBattleRequest{
		MatchMode:         uint32(matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP),
		AttackerPlayerIds: []uint64{101},
		DefenderPlayerIds: []uint64{202},
		ActivityContext: &battlepb.BattleActivityContext{
			Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_WAR,
			GuildWar: &battlepb.GuildWarBattleContext{
				WarBattleKind:   uint32(pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_FIELD_ROAD),
				AttackerGuildId: 10,
				DefenderGuildId: 20,
				ZoneId:          1,
			},
		},
	}
	resp, err := starter.StartGuildWarBattle(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, uint64(778899), resp.GetBattleId())
	require.NotNil(t, stub.captured)
	require.NotNil(t, stub.captured.GetAuth())
	require.Equal(t, "guild", stub.captured.GetAuth().GetCaller())

	verifier, err := serviceauth.New(map[string]string{"guild": secret})
	require.NoError(t, err)
	_, err = verifier.Verify(context.Background(), matchpb.MatchInternal_StartGuildWarBattle_FullMethodName, stub.captured)
	require.NoError(t, err)
}

func TestGuildWarBundleAndGarrisonSnapshot(t *testing.T) {
	empty, err := buildSilverItemBundle(0)
	require.NoError(t, err)
	require.Nil(t, empty)

	raw, err := buildSilverItemBundle(5000, &assetpb.ItemGrant{ConfigId: 9001, Count: 2})
	require.NoError(t, err)
	var b assetpb.AssetBundle
	require.NoError(t, proto.Unmarshal(raw, &b))
	require.Len(t, b.GetCurrencies(), 1)
	require.Equal(t, currencytype.Gold, b.GetCurrencies()[0].GetCurrencyType())
	require.Equal(t, uint64(5000), b.GetCurrencies()[0].GetAmount())
	require.Len(t, b.GetItems(), 1)
	require.Equal(t, uint32(9001), b.GetItems()[0].GetConfigId())

	snap := buildGarrisonSnapshot(555, "云中客", 0, 0)
	require.True(t, snap.GetAiControlled())
	require.Equal(t, uint32(50), snap.GetLevel())
	require.Equal(t, uint32(1), snap.GetTeamIndex())
	require.NotNil(t, snap.GetBaseAttributes())
	require.Greater(t, snap.GetMaxHealth(), uint64(0))
}

// r2-gw#1:客户端的 leyline_id 只在打守旗阵时采信,bounty_id 只在追杀榜直打时采信;直打活人 / 野外直打 / 一键开战一律丢弃,
// 由服务端按目标推(野外按目标所在场景推灵脉,直打活人按目标自动挂钩追杀令)。
func TestClientWarTargetHintsTrustsOnlyTheFieldOfItsOwnKind(t *testing.T) {
	req := &pb.StartGuildWarBattleRequest{LeylineId: 2, BountyId: 424242, TargetPlayerId: 7}
	cases := []struct {
		kind        pb.GuildWarTargetKind
		wantLeyline uint32
		wantBounty  uint64
	}{
		{pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_LEYLINE_GARRISON, 2, 0},
		{pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_BOUNTY, 0, 424242},
		{pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_LIVE_PLAYER, 0, 0},
		{pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_FIELD_PLAYER, 0, 0},
		{pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_QUICK_AUTO, 0, 0},
		{pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_UNSPECIFIED, 0, 0},
	}
	for _, tc := range cases {
		leyline, bounty := clientWarTargetHints(tc.kind, req)
		require.Equal(t, tc.wantLeyline, leyline, tc.kind.String())
		require.Equal(t, tc.wantBounty, bounty, tc.kind.String())
	}
}

func TestWarBountyExpiryEveryRoundsFoldsToAboutOneMinute(t *testing.T) {
	require.Equal(t, 6, warBountyExpiryEveryRounds(10*time.Second))
	require.Equal(t, 60, warBountyExpiryEveryRounds(time.Second))
	require.Equal(t, 1, warBountyExpiryEveryRounds(time.Minute))
	require.Equal(t, 1, warBountyExpiryEveryRounds(0))
}

func TestGuildWarRPCsFailClosedWithoutSessionOrWarDeps(t *testing.T) {
	l := &GuildLogic{}

	// 无客户端会话一律拒收 Unauthenticated / PermissionDenied。
	_, err := l.GetGuildWarOverview(context.Background(), &pb.GetGuildWarOverviewRequest{})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Contains(t, []codes.Code{codes.Unauthenticated, codes.PermissionDenied}, st.Code())

	// 带合法客户端会话但未装配 war 依赖时回 ErrWarNotOpen（不返回 gRPC 故障）。
	ctx := clientCtx(1001)
	ovResp, err := l.GetGuildWarOverview(ctx, &pb.GetGuildWarOverviewRequest{})
	require.NoError(t, err)
	require.Equal(t, constants.ErrWarNotOpen, ovResp.GetErrorMessage().GetId())

	startResp, err := l.StartGuildWarBattle(ctx, &pb.StartGuildWarBattleRequest{
		TargetKind: pb.GuildWarTargetKind_GUILD_WAR_TARGET_KIND_QUICK_AUTO,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrWarNotOpen, startResp.GetErrorMessage().GetId())

	feudResp, err := l.IssueGuildWarFeud(ctx, &pb.IssueGuildWarFeudRequest{TargetGuildId: 20})
	require.NoError(t, err)
	require.Equal(t, constants.ErrWarNotOpen, feudResp.GetErrorMessage().GetId())

	respondResp, err := l.RespondGuildWarFeud(ctx, &pb.RespondGuildWarFeudRequest{FeudId: 1, Accept: true})
	require.NoError(t, err)
	require.Equal(t, constants.ErrWarNotOpen, respondResp.GetErrorMessage().GetId())
}
