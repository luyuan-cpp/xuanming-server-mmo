package tournament

import (
	"testing"

	"github.com/stretchr/testify/require"
	battlepb "proto/battle"
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
	require.Len(t, att.Items, 4)
	require.Equal(t, uint32(3407), att.Items[3].ConfigId)
	require.Equal(t, uint32(3), att.Items[3].Count)

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

func TestV2Bo3LeylineDrawAndUtc8DayKey(t *testing.T) {
	// 16 强: R1(16队)/R2(8队) 为 BO1; R3(半决赛4队)/R4(决赛2队) 为 BO3
	require.Equal(t, uint32(1), BestOfForKnockout(16, 1, true))
	require.Equal(t, uint32(1), BestOfForKnockout(16, 2, true))
	require.Equal(t, uint32(3), BestOfForKnockout(16, 3, true))
	require.Equal(t, uint32(3), BestOfForKnockout(16, 4, true))
	require.Equal(t, uint32(1), BestOfForKnockout(16, 4, false))
	require.Equal(t, uint32(3), BestOfForKnockout(2, 1, true))

	// 五行灵脉抽签确定性且 3 局互不重复
	seq1 := DrawLeylines(1, 900001, 3)
	seq2 := DrawLeylines(1, 900001, 3)
	require.Len(t, seq1, 3)
	require.Equal(t, seq1, seq2)
	seen := map[int]bool{}
	for _, el := range seq1 {
		require.GreaterOrEqual(t, int(el), 1)
		require.LessOrEqual(t, int(el), 5)
		require.False(t, seen[int(el)], "BO3 三局灵脉不得重复")
		seen[int(el)] = true
	}
	encoded := EncodeLeylineSequence(seq1)
	require.Equal(t, seq1, DecodeLeylineSequence(encoded))
	require.Equal(t, seq1[0], LeylineForGame(seq1, 1))
	require.Equal(t, seq1[2], LeylineForGame(seq1, 3))

	// UTC+8 跨日检验: 2026-11-25 15:59:59 UTC == 2026-11-25 23:59:59 UTC+8; +2秒即 20261126
	require.Equal(t, uint32(20261125), Utc8DayKey(1795622399000))
	require.Equal(t, uint32(20261126), Utc8DayKey(1795622401000))
}

func TestV2SpectateCheerHubWhitelistAndCooldown(t *testing.T) {
	hub := NewSpectateCheerHub()
	const battleID uint64 = 777001

	// 非法短语 ID(0 或 7)与非法边(2)必须被白名单拒绝
	_, _, invalid := hub.Send(battleID, 1001, "青云子", 0, 0, 10000)
	require.True(t, invalid)
	_, _, invalid = hub.Send(battleID, 1001, "青云子", 7, 0, 10000)
	require.True(t, invalid)
	_, _, invalid = hub.Send(battleID, 1001, "青云子", 3, 2, 10000)
	require.True(t, invalid)

	// 合法短语 3(妙法天成)支持 A 队(0)
	snap, cd, invalid := hub.Send(battleID, 1001, "青云子", 3, 0, 10000)
	require.False(t, cd)
	require.False(t, invalid)
	require.Equal(t, uint64(1), snap.CountA)
	require.Equal(t, uint64(0), snap.CountB)
	require.Equal(t, uint64(15000), snap.CooldownUntilMs)
	require.Len(t, snap.RecentCheers, 1)
	require.Equal(t, uint64(1), snap.RecentCheers[0].Seq)

	// 5 秒内重复点击触发冷却且不增加人气
	snapCd, cd, invalid := hub.Send(battleID, 1001, "青云子", 6, 0, 12000)
	require.True(t, cd)
	require.False(t, invalid)
	require.Equal(t, uint64(1), snapCd.CountA)

	// 另一观众支持 B 队(1),再 5 秒后首观众解冻
	snap2, cd, invalid := hub.Send(battleID, 1002, "凌霄客", 6, 1, 13000)
	require.False(t, cd)
	require.False(t, invalid)
	require.Equal(t, uint64(1), snap2.CountA)
	require.Equal(t, uint64(1), snap2.CountB)

	// after_seq 增量拉取仅返回 seq > 1 的新条
	delta := hub.Snapshot(battleID, 1001, 1, 15000)
	require.Equal(t, uint64(0), delta.CooldownUntilMs)
	require.Len(t, delta.RecentCheers, 1)
	require.Equal(t, uint64(2), delta.RecentCheers[0].Seq)
	require.Equal(t, uint32(6), delta.RecentCheers[0].PhraseId)
}

func TestV3EmblemsPredictionWeightsMvpAndThirdPlaceRewards(t *testing.T) {
	// 1. 队徽白名单与建队默认归一化
	norm0, ok0 := NormalizeEmblemID(0)
	require.True(t, ok0)
	require.Equal(t, uint32(1), norm0)
	norm8, ok8 := NormalizeEmblemID(8)
	require.True(t, ok8)
	require.Equal(t, uint32(8), norm8)
	_, bad9 := NormalizeEmblemID(9)
	require.False(t, bad9)
	require.False(t, IsValidEmblemID(0))
	require.True(t, IsValidEmblemID(5))

	// 2. 神机竞猜轮次权重: 16强=1, 8强=2, 半决赛=3, 季军战=3, 总决赛=5
	require.Equal(t, uint32(1), PredictionWeightForRound(16, 1, false))
	require.Equal(t, uint32(2), PredictionWeightForRound(16, 2, false))
	require.Equal(t, uint32(3), PredictionWeightForRound(16, 3, false))
	require.Equal(t, uint32(5), PredictionWeightForRound(16, 4, false))
	require.Equal(t, uint32(3), PredictionWeightForRound(16, 4, true))

	// 3. MVP 评定公式与确定性平局决胜
	mvp, ok := ComputeMatchMvp([]*battlepb.BattleSettlementData{
		{PlayerId: 2001, PlayerTeamIndex: 0, TotalDamageDealt: 10000, TotalHealingDone: 0, TotalControlsLanded: 1, IsDead: false},   // 100 + 0 + 60 + 120 + 180 = 460
		{PlayerId: 2002, PlayerTeamIndex: 0, TotalDamageDealt: 4000, TotalHealingDone: 8000, TotalControlsLanded: 2, IsDead: false}, // 40 + 100 + 120 + 120 + 180 = 560
		{PlayerId: 2003, PlayerTeamIndex: 1, TotalDamageDealt: 20000, TotalHealingDone: 0, TotalControlsLanded: 0, IsDead: true},    // 200 + 0 + 0 + 0 + 0 = 200
	}, 0)
	require.True(t, ok)
	require.Equal(t, uint64(2002), mvp.PlayerID)
	require.Equal(t, uint32(560), mvp.Score)

	// 4. 季军独立奖(Tier 8)与第4名四强奖(Tier 3)及神机竞猜奖(Tier 9)
	cat := DefaultFallback()
	third := cat.RewardForPlacement(3, 6)
	require.NotNil(t, third)
	require.Equal(t, uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_THIRD_PLACE), third.GetTierKind())
	fourth := cat.RewardForPlacement(4, 6)
	require.NotNil(t, fourth)
	require.Equal(t, uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_TOP4), fourth.GetTierKind())
	pred := cat.PredictionReward()
	require.NotNil(t, pred)
	require.Equal(t, uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_PREDICTION), pred.GetTierKind())
}

func TestV4LeylineBansHonorBitsAndHighlightClassification(t *testing.T) {
	// 1. 模块 H：禁选后抽取 3 条不重复灵脉且绝不含任何被禁灵脉；无禁时与 DrawLeylines 逐位一致
	require.Equal(t, DrawLeylines(1, 9001, 3), DrawLeylinesWithBans(1, 9001, 3, 0, 0))
	bannedSeq := DrawLeylinesWithBans(1, 9001, 3,
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_METAL,
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_FIRE)
	require.Len(t, bannedSeq, 3)
	seen := map[battlepb.TournamentLeylineElement]bool{}
	for _, e := range bannedSeq {
		require.NotEqual(t, battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_METAL, e)
		require.NotEqual(t, battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_FIRE, e)
		require.False(t, seen[e], "leyline must be distinct")
		seen[e] = true
	}
	// 双方同禁同一条灵脉时同样产出 3 条不重复剩余灵脉
	sameBanSeq := DrawLeylinesWithBans(1, 9001, 3,
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_WATER,
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_WATER)
	require.Len(t, sameBanSeq, 3)
	for _, e := range sameBanSeq {
		require.NotEqual(t, battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_WATER, e)
	}

	// 2. 模块 J：海选与淘汰赛荣耀铭牌位图单调累加
	bits := ComputeQualifierHonorBits(0, 3, 3, 0, 2)
	require.Equal(t, HonorBitDebut|HonorBitStreak|HonorBitUnbeaten, bits)
	// 后续输球不撤销已点亮的不败金身（单调递增）
	bits = ComputeQualifierHonorBits(bits, 4, 3, 1, 2)
	require.NotZero(t, bits&HonorBitUnbeaten)
	bits = ComputeKnockoutSeriesHonorBits(bits, 3, 2, 1, false)
	require.NotZero(t, bits&HonorBitComeback)
	require.Zero(t, bits&HonorBitSweep)
	bits = ComputeKnockoutSeriesHonorBits(bits, 3, 2, 0, true)
	require.NotZero(t, bits&HonorBitSweep)
	require.NotZero(t, bits&HonorBitChampion)

	// 3. 模块 I：名局录标签与优先级
	tagF, _, prioF := ClassifyHighlightTag(false, 2, 2, 3, 2, 0)
	require.Equal(t, uint32(1), tagF)
	require.Equal(t, 0, prioF)
	tagBo3, _, prioBo3 := ClassifyHighlightTag(false, 1, 2, 3, 2, 1)
	require.Equal(t, uint32(2), tagBo3)
	require.Equal(t, 1, prioBo3)
	tag3rd, _, prio3rd := ClassifyHighlightTag(true, 2, 2, 3, 2, 0)
	require.Equal(t, uint32(3), tag3rd)
	require.Equal(t, 2, prio3rd)
	tagMvp, _, prioMvp := ClassifyHighlightTag(false, 1, 2, 1, 1, 0)
	require.Equal(t, uint32(4), tagMvp)
	require.Equal(t, 3, prioMvp)
}
