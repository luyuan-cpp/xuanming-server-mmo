package logic

// 评分匹配(cross-zone-matchmaking.md §11)不经 SQL 的部分:容差曲线、5v5 蛇形分队、结果回流的不计分分支与队伍拆分。
// 入账事务与幂等在 authority 包(authority/rating.go,需要 TiDB)。

import (
	"testing"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

// ---- 容差曲线 ----

// 默认曲线:100 起、每 5s +100、上限 1000;配置覆盖生效;漏配按默认。
func TestRatingToleranceCurve(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	require.Equal(t, 100.0, ratingToleranceFor(svcCtx, 0))
	require.Equal(t, 100.0, ratingToleranceFor(svcCtx, 4))
	require.Equal(t, 200.0, ratingToleranceFor(svcCtx, 5))
	require.Equal(t, 300.0, ratingToleranceFor(svcCtx, 10))
	require.Equal(t, 1000.0, ratingToleranceFor(svcCtx, 45))
	require.Equal(t, 1000.0, ratingToleranceFor(svcCtx, 3600))
	require.Equal(t, 100.0, ratingToleranceFor(svcCtx, -3), "负等待按 0")

	svcCtx.Config.RatingToleranceBase = 50
	svcCtx.Config.RatingToleranceStepSeconds = 10
	svcCtx.Config.RatingToleranceStepDelta = 25
	svcCtx.Config.RatingToleranceMax = 120
	require.Equal(t, 50.0, ratingToleranceFor(svcCtx, 9))
	require.Equal(t, 75.0, ratingToleranceFor(svcCtx, 10))
	require.Equal(t, 120.0, ratingToleranceFor(svcCtx, 100))
}

// ---- Elo 结果回流 ----

func resultEvent(battleId uint64, mode matchpb.MatchMode, outcome battlepb.EBattleOutcome, teamA, teamB []uint64) *kafkapb.BattleResultEvent {
	return &kafkapb.BattleResultEvent{
		BattleId:  battleId,
		MatchMode: uint32(mode),
		Outcome:   outcome,
		Teams: []*kafkapb.BattleResultTeam{
			{TeamIndex: 0, PlayerIds: teamA},
			{TeamIndex: 1, PlayerIds: teamB},
		},
	}
}

// 不计分的结果在任何 SQL 之前结束:这里的 ServiceContext 没有 Authority,走到入账就 panic。
func TestApplyBattleResultIgnoresUnratedResults(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	sideA := battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN
	mode1v1 := matchpb.MatchMode_MATCH_MODE_1V1
	oneTeam := resultEvent(9004, mode1v1, sideA, []uint64{1}, []uint64{2})
	oneTeam.Teams = oneTeam.Teams[:1]
	cases := map[string]*kafkapb.BattleResultEvent{
		"PVE 不计分": resultEvent(9001, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, sideA, []uint64{1}, []uint64{2}),
		"切磋不计分":   resultEvent(9002, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, sideA, []uint64{1}, []uint64{2}),
		"没有胜负结果":  resultEvent(9003, mode1v1, battlepb.EBattleOutcome(0), []uint64{1}, []uint64{2}),
		"只有一支队":   oneTeam,
		"有一支队没人":  resultEvent(9005, mode1v1, sideA, []uint64{1}, nil),
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			outcome, err := ApplyBattleResult(svcCtx, event)
			require.NoError(t, err)
			require.Equal(t, ratingOutcomeIgnored, outcome)
		})
	}
}

func TestSplitResultTeamsRejectsMalformedTeams(t *testing.T) {
	a, b, ok := splitResultTeams(resultEvent(1, matchpb.MatchMode_MATCH_MODE_5V5, battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, []uint64{1, 2}, []uint64{3}))
	require.True(t, ok)
	require.Equal(t, []uint64{1, 2}, a)
	require.Equal(t, []uint64{3}, b)

	badIndex := resultEvent(2, matchpb.MatchMode_MATCH_MODE_1V1, battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, []uint64{1}, []uint64{2})
	badIndex.Teams[1].TeamIndex = 2
	_, _, ok = splitResultTeams(badIndex)
	require.False(t, ok, "team_index 只认 0 / 1")

	sameSide := resultEvent(3, matchpb.MatchMode_MATCH_MODE_1V1, battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, []uint64{1}, []uint64{2})
	sameSide.Teams[1].TeamIndex = 0
	_, _, ok = splitResultTeams(sameSide)
	require.False(t, ok, "两支都写成 0 队等于另一队没人")
}

// ---- 5v5 蛇形分队 ----

// 按评分降序蛇形 0/1/1/0/0/1/1/0/0/1:第 1、4、5、8、9 名一队,其余一队;同分按弹出序稳定。
func TestAssignBalancedTeamsSnake(t *testing.T) {
	members := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	ratings := map[uint64]float64{}
	for i, pid := range members {
		ratings[pid] = float64(2000 - 100*i) // 成员 1 最高
	}
	teams := assignBalancedTeams(members, ratings)
	require.Equal(t, []uint32{0, 1, 1, 0, 0, 1, 1, 0, 0, 1}, teams)
	sum := [2]float64{}
	for i, pid := range members {
		sum[teams[i]] += ratings[pid]
	}
	// 等差评分 10 人(5 对)蛇形分配两队总分只差一个步长(7800 / 7700),
	// 远优于"前 5 后 5"的 8500 / 7000。
	require.InDelta(t, sum[0], sum[1], 100, "蛇形分配两队总分接近")

	// 乱序输入:分队跟评分走,不跟下标走。
	shuffled := []uint64{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	teams = assignBalancedTeams(shuffled, ratings)
	byPlayer := map[uint64]uint32{}
	for i, pid := range shuffled {
		byPlayer[pid] = teams[i]
	}
	require.Equal(t, map[uint64]uint32{1: 0, 2: 1, 3: 1, 4: 0, 5: 0, 6: 1, 7: 1, 8: 0, 9: 0, 10: 1}, byPlayer)

	// 全员同分:按弹出序蛇形,前后各 5 人且第 1 位在 0 队。
	same := map[uint64]float64{}
	for _, pid := range members {
		same[pid] = defaultRating
	}
	teams = assignBalancedTeams(members, same)
	require.Equal(t, []uint32{0, 1, 1, 0, 0, 1, 1, 0, 0, 1}, teams)
}
