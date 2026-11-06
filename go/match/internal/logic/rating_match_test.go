package logic

// 评分匹配(cross-zone-matchmaking.md §11)不经 SQL 的部分:容差曲线、5v5 蛇形分队、结果回流的不计分分支与队伍拆分。
// 入账事务与幂等在 authority 包(authority/rating.go,需要 TiDB)。

import (
	"testing"

	"match/internal/catalog"

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

// withOriginals 给结果补上版本 1 的个人原件(结果消费者入账前已核过的那种):alive 里的玩家终局时未阵亡,
// 其余阵亡;pets 里的玩家各带一只宝宝,值为宝宝是否存活。
func withOriginals(event *kafkapb.BattleResultEvent, alive, pets map[uint64]bool) *kafkapb.BattleResultEvent {
	event.SettlementFormatVersion = 1
	for _, team := range event.GetTeams() {
		for _, player := range team.GetPlayerIds() {
			settlement := &battlepb.BattleSettlementData{BattleId: event.GetBattleId(), PlayerId: player, Outcome: event.GetOutcome(),
				PlayerTeamIndex: team.GetTeamIndex(), IsDead: !alive[player], TotalRounds: event.GetTotalRounds()}
			if petAlive, hasPet := pets[player]; hasPet {
				settlement.Pets = []*battlepb.BattlePetSettlementData{{PetId: player * 10, IsDead: !petAlive}}
			}
			event.Settlements = append(event.Settlements, settlement)
		}
	}
	return event
}

// 回合打满改判平局:引擎打满只会判 1 队胜。有个人原件时看 0 队是否全灭(最后一回合真打死也分得清),
// 旧版结果只能看回合数是否到了模板回合上限;0 队胜与平局从不改判。
func TestRatedScoreAJudgesRoundCapByOriginals(t *testing.T) {
	const limit = 300
	sideA, sideB, draw := battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW
	duel := func(outcome battlepb.EBattleOutcome, rounds uint32) *kafkapb.BattleResultEvent {
		event := resultEvent(9100, matchpb.MatchMode_MATCH_MODE_1V1, outcome, []uint64{1}, []uint64{2})
		event.TotalRounds = rounds
		return event
	}
	fled := withOriginals(duel(sideB, limit), map[uint64]bool{1: true, 2: true}, nil)
	fled.Settlements[0].Fled = true // 玩家 1 逃跑离场(PVP 实际逃不掉,这里只验口径:逃跑与阵亡一样不算能行动)
	cases := []struct {
		name     string
		event    *kafkapb.BattleResultEvent
		score    float64
		roundCap string
		rated    bool
	}{
		{"0 队胜:1 队全灭", withOriginals(duel(sideA, 12), map[uint64]bool{1: true}, nil), 1, "", true},
		{"0 队胜不看回合数:最后一回合把 1 队打死", duel(sideA, limit), 1, "", true},
		{"平局:整场期限打满由 battle 盖平局章", withOriginals(duel(draw, 40), map[uint64]bool{1: true, 2: true}, nil), 0.5, "", true},
		{"1 队胜且 0 队全灭:真输,哪怕在最后一回合", withOriginals(duel(sideB, limit), map[uint64]bool{2: true}, nil), 0, "", true},
		{"1 队胜但 0 队还有人:回合打满,改判平局", withOriginals(duel(sideB, limit), map[uint64]bool{1: true, 2: true}, nil), 0.5, roundCapBySurvivors, true},
		{"0 队玩家阵亡但宝宝还活着:没全灭", withOriginals(duel(sideB, limit), map[uint64]bool{2: true}, map[uint64]bool{1: true}), 0.5, roundCapBySurvivors, true},
		{"0 队玩家与宝宝都阵亡:全灭", withOriginals(duel(sideB, limit), map[uint64]bool{2: true}, map[uint64]bool{1: false}), 0, "", true},
		{"逃跑的玩家不算能行动", fled, 0, "", true},
		{"旧版结果回合数到上限:按平局", duel(sideB, limit), 0.5, roundCapByRoundLimit, true},
		{"旧版结果回合数超过上限:按平局", duel(sideB, limit+1), 0.5, roundCapByRoundLimit, true},
		{"旧版结果回合数没到上限:算输", duel(sideB, limit-1), 0, "", true},
		{"进行中不计分", duel(battlepb.EBattleOutcome_BATTLE_OUTCOME_ONGOING, 3), 0, "", false},
		{"未知终局不计分", duel(battlepb.EBattleOutcome(99), 3), 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score, roundCap, rated := ratedScoreA(tc.event, limit)
			require.Equal(t, tc.rated, rated)
			require.Equal(t, tc.score, score)
			require.Equal(t, tc.roundCap, roundCap)
		})
	}
}

// 旧版结果的回合上限按模板 time_limit 推导(与引擎同一换算),不再读配置:60 秒的模板 10 回合打满,
// 没填 time_limit 或不在表里的模板沿用引擎缺省 300 回合(即删掉的 RatingDrawRoundCap 缺省值)。
func TestOldResultsDrawAtTemplateRoundLimit(t *testing.T) {
	c, err := catalog.New(catalog.Rows{Dungeons: []catalog.DungeonRow{
		{Id: 7, MaxTeamSize: 5, TimeLimitSeconds: 60, MonsterCount: 1},
		{Id: 8, MaxTeamSize: 5, TimeLimitSeconds: 0, MonsterCount: 1},
	}})
	require.NoError(t, err)
	require.EqualValues(t, 10, c.RoundLimit(7))
	require.EqualValues(t, catalog.EngineDefaultRoundLimit, c.RoundLimit(8))
	require.EqualValues(t, catalog.EngineDefaultRoundLimit, c.RoundLimit(42))
	event := resultEvent(9101, matchpb.MatchMode_MATCH_MODE_5V5, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, []uint64{1}, []uint64{2})
	event.BattleConfigId = 7
	event.TotalRounds = 10
	score, roundCap, rated := ratedScoreA(event, c.RoundLimit(event.GetBattleConfigId()))
	require.True(t, rated)
	require.Equal(t, 0.5, score)
	require.Equal(t, roundCapByRoundLimit, roundCap)
	event.TotalRounds = 9
	score, roundCap, _ = ratedScoreA(event, c.RoundLimit(event.GetBattleConfigId()))
	require.Zero(t, score, "没到 10 回合的 1 队胜是真赢")
	require.Empty(t, roundCap)
}

// 计分局禁止观战(§8):每个计分模式在建房时都冻结为私密,WatchBattle 回 16018,观战列表与随机选场都不收它。
// 新增计分模式时这里随 ratingrule 自动覆盖,不用记得去改观战决策表。
func TestRatedModesAreNeverSpectatable(t *testing.T) {
	rated := 0
	for _, value := range matchpb.MatchMode_value {
		mode := matchpb.MatchMode(value)
		if !isRatedMode(mode) {
			continue
		}
		rated++
		require.Equal(t, visPrivate, spectateVisibilityFor(mode, nil), "计分模式 %s 必须私密", mode)
	}
	require.NotZero(t, rated)
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
