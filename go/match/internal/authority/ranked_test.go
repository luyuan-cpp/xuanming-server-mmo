package authority

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"match/internal/ranked"
	battlepb "proto/battle"
	pb "proto/match"

	"github.com/stretchr/testify/require"
)

func TestEvaluateRankedParticipation(t *testing.T) {
	t.Run("zero rounds never participates", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ManualActionAccepted: true}
		require.False(t, evaluateRankedParticipation(fact, nil, 0, true))
	})

	t.Run("fled never participates even with manual action", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ManualActionAccepted: true}
		st := &battlepb.BattleSettlementData{PlayerId: 1, Fled: true, Participated: true, ManualActionRounds: 3, EffectiveCombatContribution: true}
		require.False(t, evaluateRankedParticipation(fact, st, 3, true))
	})

	t.Run("fast win with manual action or explicit auto qualifies", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ManualActionAccepted: true}
		require.True(t, evaluateRankedParticipation(fact, &battlepb.BattleSettlementData{PlayerId: 1}, 1, true))
		autoFact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ExplicitAutoEnabled: true}
		require.True(t, evaluateRankedParticipation(autoFact, &battlepb.BattleSettlementData{PlayerId: 1}, 2, true))
	})

	t.Run("loss or draw blocks auto feeding and short afk games", func(t *testing.T) {
		autoFact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ExplicitAutoEnabled: true}
		require.False(t, evaluateRankedParticipation(autoFact, &battlepb.BattleSettlementData{
			PlayerId: 1, ManualActionRounds: 0, EffectiveCombatContribution: true,
		}, 4, false))

		shortManual := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ManualActionAccepted: true}
		require.False(t, evaluateRankedParticipation(shortManual, &battlepb.BattleSettlementData{
			PlayerId: 1, ManualActionRounds: 2, EffectiveCombatContribution: true,
		}, 2, false))
		require.False(t, evaluateRankedParticipation(shortManual, &battlepb.BattleSettlementData{
			PlayerId: 1, ManualActionRounds: 2, EffectiveCombatContribution: false,
		}, 3, false))

		// 打满 3 回合、2 回合手动且有有效贡献的负/平局正常计入赛季场次
		require.True(t, evaluateRankedParticipation(shortManual, &battlepb.BattleSettlementData{
			PlayerId: 1, ManualActionRounds: 2, EffectiveCombatContribution: true,
		}, 3, false))
	})

	t.Run("entered room plus no action opportunity still qualifies on loss", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, NoActionOpportunity: true}
		require.True(t, evaluateRankedParticipation(fact, &battlepb.BattleSettlementData{PlayerId: 1}, 1, false))
	})

	t.Run("afk without entering room or action does not qualify", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: false, ExplicitAutoEnabled: true}
		require.False(t, evaluateRankedParticipation(fact, &battlepb.BattleSettlementData{PlayerId: 1, ManualActionRounds: 3, EffectiveCombatContribution: true}, 5, true))
		fact2 := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true}
		require.False(t, evaluateRankedParticipation(fact2, &battlepb.BattleSettlementData{PlayerId: 1, ManualActionRounds: 3, EffectiveCombatContribution: true}, 5, false))
	})

	t.Run("falls back to settlement participated flag when fact absent", func(t *testing.T) {
		require.True(t, evaluateRankedParticipation(nil, &battlepb.BattleSettlementData{PlayerId: 1, Participated: true}, 2, true))
		require.True(t, evaluateRankedParticipation(nil, &battlepb.BattleSettlementData{
			PlayerId: 1, Participated: true, ManualActionRounds: 2, EffectiveCombatContribution: true,
		}, 3, false))
		require.False(t, evaluateRankedParticipation(nil, &battlepb.BattleSettlementData{PlayerId: 1, Participated: true}, 2, false))
		require.False(t, evaluateRankedParticipation(nil, &battlepb.BattleSettlementData{PlayerId: 1, Participated: false}, 3, true))
	})
}

func TestRankedTierCrossingAndEffectiveDefaultTalisman(t *testing.T) {
	cat := ranked.DefaultFallback()
	require.Equal(t, pb.RankedTier_RANKED_TIER_UNSPECIFIED, cat.TierFor(160000, false))
	require.Equal(t, pb.RankedTier_RANKED_TIER_CHUSHI, cat.TierFor(150000, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_CHUSHI, cat.TierFor(151999, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_QINGFENG, cat.TierFor(152000, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_QINGFENG, cat.TierFor(159999, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_YUNZHANG, cat.TierFor(160000, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_TIANSHU, cat.TierFor(172000, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_WUSHUANG, cat.TierFor(185000, true))

	prefs := &pb.MatchRankedPlayerPrefsRecord{PlayerId: 1}
	rewards := []*pb.MatchRankedRewardRecord{
		{PlayerId: 1, SeasonId: 1, RewardKind: uint32(pb.RankedRewardKind_RANKED_REWARD_TALISMAN), ItemCode: "s1_qingyun_talisman"},
		{PlayerId: 1, SeasonId: 2, RewardKind: uint32(pb.RankedRewardKind_RANKED_REWARD_TALISMAN), ItemCode: "s2_qingyun_talisman"},
		{PlayerId: 1, SeasonId: 2, RewardKind: uint32(pb.RankedRewardKind_RANKED_REWARD_WIN_MILESTONE), ItemCode: "s2_win_milestone_6"},
	}
	eff := effectiveCosmeticLoadout(prefs, rewards)
	require.Equal(t, "s2_qingyun_talisman", eff.GetEquippedTalismanCode(), "未显式隐藏且未手选时默认挂最新青云玉符")
	require.True(t, ownsTitleRewardCode(rewards, "s2_win_milestone_6"), "6 胜锋芒称号必须可作为称号栏装备")
	require.False(t, ownsTitleRewardCode(rewards, "s2_unknown_title"))

	rawHidden, err := proto.Marshal(&pb.RankedCosmeticLoadout{HideTalisman: true})
	require.NoError(t, err)
	prefs.CosmeticLoadout = rawHidden
	effHidden := effectiveCosmeticLoadout(prefs, rewards)
	require.Empty(t, effHidden.GetEquippedTalismanCode(), "显式隐藏玉符后必须保持空串")

	// V2 封榜段位徽章与称号起发线：云章起发
	_, okChushi := rankedHonorTierCode(pb.RankedTier_RANKED_TIER_CHUSHI)
	_, okQingfeng := rankedHonorTierCode(pb.RankedTier_RANKED_TIER_QINGFENG)
	codeYunzhang, okYunzhang := rankedHonorTierCode(pb.RankedTier_RANKED_TIER_YUNZHANG)
	codeTianshu, okTianshu := rankedHonorTierCode(pb.RankedTier_RANKED_TIER_TIANSHU)
	codeWushuang, okWushuang := rankedHonorTierCode(pb.RankedTier_RANKED_TIER_WUSHUANG)
	require.False(t, okChushi)
	require.False(t, okQingfeng)
	require.True(t, okYunzhang)
	require.Equal(t, "yunzhang", codeYunzhang)
	require.True(t, okTianshu)
	require.Equal(t, "tianshu", codeTianshu)
	require.True(t, okWushuang)
	require.Equal(t, "wushuang", codeWushuang)

	var nilStore *Store
	info := nilStore.toSeasonInfo(&pb.MatchRankedSeasonRecord{SeasonId: 1, SeasonKey: "s1", RuleVersion: 2})
	require.Equal(t, uint32(152000), info.GetTierQingfengCenti())
	require.Equal(t, uint32(160000), info.GetTierYunzhangCenti())
	require.Equal(t, uint32(172000), info.GetTierTianshuCenti())
	require.Equal(t, uint32(185000), info.GetTierWushuangCenti())
	require.Equal(t, uint32(6), info.GetMilestoneWinMatches())

	summary := nilStore.toPlayerSeasonSummary(&pb.MatchRankedPlayerSeasonRecord{
		SeasonId:         1,
		Qualified:        true,
		RatingCenti:      162000,
		CurrentWinStreak: 4,
		BestWinStreak:    6,
	}, "s1")
	require.Equal(t, uint32(4), summary.GetCurrentWinStreak())
	require.Equal(t, uint32(6), summary.GetBestWinStreak())
}
