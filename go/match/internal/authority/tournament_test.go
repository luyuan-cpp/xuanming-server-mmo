package authority

import (
	"testing"

	"match/internal/tournament"

	"github.com/stretchr/testify/require"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
)

func TestTournamentSeasonWindowsAndSchemaRegistered(t *testing.T) {
	cat := tournament.DefaultFallback()
	const start uint64 = 1_700_000_000_000
	s1 := buildNewTournamentSeason(1, start, cat)
	require.Equal(t, uint32(1), s1.SeasonId)
	require.Equal(t, "ts1", s1.SeasonKey)
	require.Equal(t, uint32(pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION), s1.Stage)
	require.Equal(t, uint32(3), s1.BattleTeamSize)
	require.Equal(t, uint32(4), s1.RosterMaxSize)
	require.Equal(t, start+42*millisPerDay, s1.QualifierStartMs)
	require.Equal(t, s1.QualifierStartMs+7*millisPerDay, s1.KnockoutStartMs)
	require.Equal(t, s1.KnockoutStartMs+2*millisPerDay, s1.SettleStartMs)
	require.Equal(t, s1.SettleStartMs+5*millisPerDay, s1.EndMs)

	// 校验 9 张锦标赛权威表(含 V2 小局表与 V3 神机竞猜表；原 V2 每日观道表已移除)全部登记在 Tables() 迁移清单中。
	found := map[string]bool{}
	for _, msg := range Tables() {
		found[string(msg.ProtoReflect().Descriptor().Name())] = true
	}
	for _, want := range []string{
		"MatchTournamentSeasonRecord",
		"MatchTournamentSquadRecord",
		"MatchTournamentMemberRecord",
		"MatchTournamentInviteRecord",
		"MatchTournamentMatchRecord",
		"MatchTournamentMatchGameRecord",
		"MatchTournamentCheerRecord",
		"MatchTournamentRewardRecord",
		"MatchTournamentPredictionRecord",
	} {
		require.True(t, found[want], "missing tournament authority table %s in Tables()", want)
	}
}

func TestResolveAndValidateTournamentLineup(t *testing.T) {
	members := []*pb.TournamentMemberView{
		{PlayerId: 101, Role: pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_CAPTAIN, StarterSlot: 1},
		{PlayerId: 102, Role: pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_STARTER, StarterSlot: 2},
		{PlayerId: 103, Role: pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_STARTER, StarterSlot: 3},
		{PlayerId: 104, Role: pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_SUBSTITUTE, StarterSlot: 0},
	}

	// 空请求自动采用已保存首发。
	got, err := resolveAndValidateLineup(members, nil, 3)
	require.NoError(t, err)
	require.Equal(t, []uint64{101, 102, 103}, got)

	// 允许队长把替补 104 换上首发。
	gotSub, err := resolveAndValidateLineup(members, []uint64{101, 102, 104}, 3)
	require.NoError(t, err)
	require.Equal(t, []uint64{101, 102, 104}, gotSub)

	// 拒绝夹带外部玩家。
	_, err = resolveAndValidateLineup(members, []uint64{101, 102, 999}, 3)
	require.ErrorIs(t, err, ErrTournamentLineupInvalid)

	// 拒绝重复成员 ID。
	_, err = resolveAndValidateLineup(members, []uint64{101, 101, 102}, 3)
	require.ErrorIs(t, err, ErrTournamentLineupInvalid)

	// 拒绝人数不足的战队。
	_, err = resolveAndValidateLineup(members[:2], nil, 3)
	require.ErrorIs(t, err, ErrTournamentMemberNotEnough)
}

func TestExtractKnockoutTieMetricsFromBattleResult(t *testing.T) {
	ev := &kafkapb.BattleResultEvent{
		Settlements: []*battlepb.BattleSettlementData{
			{
				PlayerId: 1, PlayerTeamIndex: 0, Health: 500, IsDead: false,
				Pets: []*battlepb.BattlePetSettlementData{{PetId: 11, Health: 0, IsDead: true}},
			},
			{
				PlayerId: 2, PlayerTeamIndex: 1, Health: 300, IsDead: false,
				Pets: []*battlepb.BattlePetSettlementData{{PetId: 22, Health: 200, IsDead: false}},
			},
		},
	}
	require.True(t, hasSideSurvivors(ev, 0))
	require.True(t, hasSideSurvivors(ev, 1))

	mA := extractKnockoutTieMetrics(ev, 0, 10, 1)
	mB := extractKnockoutTieMetrics(ev, 1, 20, 2)
	require.Equal(t, uint32(1), mA.SurvivingUnits)
	require.Equal(t, uint32(2), mB.SurvivingUnits)
	// B 方存活 2 个单位多于 A 方 1 个单位,平局决胜 B 晋级。
	require.Equal(t, 1, tournament.ResolveKnockoutDrawWinner(mA, mB))
}
