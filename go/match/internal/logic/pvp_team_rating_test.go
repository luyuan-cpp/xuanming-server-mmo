package logic

// 人数档真人 PVP 计分组排的撮合输入（docs/design/global-match-hierarchical-pools.md 开头「计分组排」）：
// 队伍分、池规则与提交点拒绝的分类。选人与分边见 poolgroup 包，提交点复核见 authority 包。

import (
	"fmt"
	"math"
	"testing"

	"match/internal/authority"
	"match/internal/catalog"

	pb "proto/match"
	teampb "proto/team"

	"github.com/stretchr/testify/require"
)

func TestPoolUnitInputScoresPartiesWithBonus(t *testing.T) {
	s := poolFixtureCtx(&authority.Store{}, nil)
	s.Config.GlobalMatch.PartyRatingBonusPerMember = 25
	n := uint64(100000)
	a, b := globalCandidate(1, 0, n, 20, 100), globalCandidate(2, 0, n, 20, 100)
	a.RatingCenti, b.RatingCenti = 140000, 160000

	solo, ok := poolUnitInput(s, poolUnit{tickets: []*pb.MatchAuthorityTicketRecord{a}}, n)
	require.True(t, ok)
	require.Equal(t, int64(140000), solo.RatingCenti, "单排就是本人评分，不加组队加成")

	launch := &teampb.TeamLaunchRecord{ActorId: 1, EnqueuedMs: 5, MemberCount: 2}
	team, ok := poolUnitInput(s, poolUnit{tickets: []*pb.MatchAuthorityTicketRecord{a, b}, launch: launch}, n)
	require.True(t, ok)
	require.Equal(t, int64(152500), team.RatingCenti, "双排 = 平均 1500 + 每多一人 25 分")
}

func TestPoolPolicyRatesPvpTeamPools(t *testing.T) {
	s := poolFixtureCtx(&authority.Store{}, nil)
	s.Config.GlobalMatch.PremadeGapStepSeconds = 10

	pvp := poolPolicy(s, catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_PVP_TEAM, ConfigId: 1, TeamSize: 3})
	require.Equal(t, 6, pvp.Target, "3V3 一局 6 人")
	require.True(t, pvp.Sides)
	require.True(t, pvp.Rated, "人数档真人 PVP 计分")
	require.Equal(t, uint64(10_000), pvp.PremadeGapStepMs)
	require.Equal(t, 10_000.0, pvp.ToleranceCenti(0), "默认曲线起始 ±100 分")
	require.True(t, math.IsInf(pvp.ToleranceCenti(90_000), 1), "等满终态兜底后不限")

	pve := poolPolicy(s, catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_PVE_TEAM, ConfigId: 1, TeamSize: 3})
	require.False(t, pve.Rated)
	require.False(t, pve.Sides)
	require.True(t, math.IsInf(pve.ToleranceCenti(0), 1), "PVE 组队不看评分")
}

func TestRatedModeIncludesPvpTeam(t *testing.T) {
	require.True(t, isRatedMode(pb.MatchMode_MATCH_MODE_PVP_TEAM))
	require.True(t, isRatedMode(pb.MatchMode_MATCH_MODE_1V1))
	require.True(t, isRatedMode(pb.MatchMode_MATCH_MODE_5V5))
	require.False(t, isRatedMode(pb.MatchMode_MATCH_MODE_PVE_TEAM))
}

func TestRatingSpreadIsExpectedClaimRejection(t *testing.T) {
	require.True(t, expectedClaimRejection(authority.ErrRatingSpread), "评分在提交点不满足是撮合与提交之间的正常变化")
	require.True(t, expectedClaimRejection(fmt.Errorf("claim: %w", authority.ErrRatingSpread)))
}
