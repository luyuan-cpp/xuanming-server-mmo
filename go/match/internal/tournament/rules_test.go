package tournament

import (
	"testing"

	"github.com/stretchr/testify/require"
	matchpb "proto/match"
)

func TestNormalizeSquadName(t *testing.T) {
	display, norm, ok := NormalizeSquadName("  青云 剑派  ")
	require.True(t, ok)
	require.Equal(t, "青云 剑派", display)
	require.Equal(t, "青云剑派", norm)

	_, normA, okA := NormalizeSquadName("TeamOne")
	_, normB, okB := NormalizeSquadName("team one")
	require.True(t, okA && okB)
	require.Equal(t, normA, normB, "同季大小写与空格差异须归一化为同一唯一键")

	_, _, badShort := NormalizeSquadName("A")
	require.False(t, badShort)
	_, _, badLong := NormalizeSquadName("一二三四五六七八九十甲乙丙")
	require.False(t, badLong)
	_, _, badCtrl := NormalizeSquadName("青云\n派")
	require.False(t, badCtrl)
	_, _, badSpaces := NormalizeSquadName("  a  ")
	require.False(t, badSpaces)
}

func TestKnockoutBracketSizeAndSeeding(t *testing.T) {
	require.Equal(t, 0, KnockoutBracketSize(0, 16))
	require.Equal(t, 0, KnockoutBracketSize(1, 16))
	require.Equal(t, 2, KnockoutBracketSize(2, 16))
	require.Equal(t, 4, KnockoutBracketSize(3, 16))
	require.Equal(t, 4, KnockoutBracketSize(4, 16))
	require.Equal(t, 8, KnockoutBracketSize(5, 16))
	require.Equal(t, 8, KnockoutBracketSize(8, 16))
	require.Equal(t, 16, KnockoutBracketSize(9, 16))
	require.Equal(t, 16, KnockoutBracketSize(32, 16))
	require.Equal(t, 8, KnockoutBracketSize(16, 8))

	pairs8 := SeedPairs(8)
	require.Equal(t, [][2]int{{1, 8}, {4, 5}, {2, 7}, {3, 6}}, pairs8)
	require.Equal(t, 3, TotalKnockoutRounds(8))
	require.Equal(t, 4, TotalKnockoutRounds(16))

	require.Equal(t, uint32(9), RankForEliminatedRound(16, 1))
	require.Equal(t, uint32(5), RankForEliminatedRound(16, 2))
	require.Equal(t, uint32(3), RankForEliminatedRound(16, 3))
	require.Equal(t, uint32(2), RankForEliminatedRound(16, 4))
	require.Equal(t, uint32(2), RankForEliminatedRound(2, 1))
}

func TestSortStandingsAndDrawTieBreaker(t *testing.T) {
	entries := []StandingEntry{
		{SquadID: 30, QualifierPoints: 9, QualifierWins: 3, QualifierDraws: 0, RegisteredMs: 200},
		{SquadID: 10, QualifierPoints: 9, QualifierWins: 3, QualifierDraws: 0, RegisteredMs: 100},
		{SquadID: 20, QualifierPoints: 10, QualifierWins: 3, QualifierDraws: 1, RegisteredMs: 300},
		{SquadID: 40, QualifierPoints: 9, QualifierWins: 2, QualifierDraws: 3, RegisteredMs: 50},
	}
	SortStandings(entries)
	require.Equal(t, uint64(20), entries[0].SquadID)
	require.Equal(t, uint64(10), entries[1].SquadID)
	require.Equal(t, uint64(30), entries[2].SquadID)
	require.Equal(t, uint64(40), entries[3].SquadID)

	// 存活人数多胜
	require.Equal(t, 1, ResolveKnockoutDrawWinner(
		KnockoutTieMetrics{SquadID: 1, SeedRank: 1, SurvivingUnits: 2, RemainingHpBpsSum: 15000},
		KnockoutTieMetrics{SquadID: 2, SeedRank: 8, SurvivingUnits: 3, RemainingHpBpsSum: 12000},
	))
	// 存活相同比残血比例和
	require.Equal(t, 0, ResolveKnockoutDrawWinner(
		KnockoutTieMetrics{SquadID: 1, SeedRank: 8, SurvivingUnits: 2, RemainingHpBpsSum: 15000},
		KnockoutTieMetrics{SquadID: 2, SeedRank: 1, SurvivingUnits: 2, RemainingHpBpsSum: 14999},
	))
	// 均相同取种子高者
	require.Equal(t, 1, ResolveKnockoutDrawWinner(
		KnockoutTieMetrics{SquadID: 1, SeedRank: 4, SurvivingUnits: 2, RemainingHpBpsSum: 10000},
		KnockoutTieMetrics{SquadID: 2, SeedRank: 2, SurvivingUnits: 2, RemainingHpBpsSum: 10000},
	))
}

func TestCatalogRewardsAndLineupCodec(t *testing.T) {
	cat := DefaultFallback()
	require.True(t, cat.Rule().GetEnabled())
	require.Equal(t, uint32(3), cat.Rule().GetBattleTeamSize())
	require.Equal(t, uint32(4), cat.Rule().GetRosterMaxSize())

	champ := cat.RewardForPlacement(1, 6)
	require.NotNil(t, champ)
	require.Equal(t, uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHAMPION), champ.GetTierKind())
	att, raw, err := BuildMailAttachments(champ)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	require.Len(t, att.Currencies, 1)
	require.Equal(t, uint64(120000), att.Currencies[0].Amount)
	require.Len(t, att.Items, 3)

	part := cat.RewardForPlacement(25, 2)
	require.NotNil(t, part)
	require.Equal(t, uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_PARTICIPANT), part.GetTierKind())
	require.Nil(t, cat.RewardForPlacement(25, 1), "完赛不足2场不发参与保底")

	cheer := cat.CheerReward()
	require.NotNil(t, cheer)
	require.Equal(t, uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHEER), cheer.GetTierKind())

	encoded := EncodeLineup([]uint64{101, 202, 303})
	require.Equal(t, []uint64{101, 202, 303}, DecodeLineup(encoded))
	require.Nil(t, DecodeLineup([]byte{1, 2, 3}))
}
