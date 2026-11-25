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
		require.False(t, evaluateRankedParticipation(fact, nil, 0))
	})

	t.Run("fled never participates even with manual action", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ManualActionAccepted: true}
		st := &battlepb.BattleSettlementData{PlayerId: 1, Fled: true, Participated: true}
		require.False(t, evaluateRankedParticipation(fact, st, 3))
	})

	t.Run("manual action accepted qualifies", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ManualActionAccepted: true}
		require.True(t, evaluateRankedParticipation(fact, &battlepb.BattleSettlementData{PlayerId: 1}, 1))
	})

	t.Run("entered room plus explicit auto qualifies", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, ExplicitAutoEnabled: true}
		require.True(t, evaluateRankedParticipation(fact, &battlepb.BattleSettlementData{PlayerId: 1}, 2))
	})

	t.Run("entered room plus no action opportunity qualifies", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true, NoActionOpportunity: true}
		require.True(t, evaluateRankedParticipation(fact, &battlepb.BattleSettlementData{PlayerId: 1}, 1))
	})

	t.Run("afk without entering room or action does not qualify", func(t *testing.T) {
		fact := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: false, ExplicitAutoEnabled: true}
		require.False(t, evaluateRankedParticipation(fact, &battlepb.BattleSettlementData{PlayerId: 1}, 5))
		fact2 := &battlepb.BattleParticipationFact{PlayerId: 1, EnteredRoom: true}
		require.False(t, evaluateRankedParticipation(fact2, &battlepb.BattleSettlementData{PlayerId: 1}, 5))
	})

	t.Run("falls back to settlement participated flag when fact absent", func(t *testing.T) {
		require.True(t, evaluateRankedParticipation(nil, &battlepb.BattleSettlementData{PlayerId: 1, Participated: true}, 2))
		require.False(t, evaluateRankedParticipation(nil, &battlepb.BattleSettlementData{PlayerId: 1, Participated: false}, 2))
	})
}

func TestRankedTierCrossingAndEffectiveDefaultTalisman(t *testing.T) {
	cat := ranked.DefaultFallback()
	require.Equal(t, pb.RankedTier_RANKED_TIER_UNSPECIFIED, cat.TierFor(155600, false))
	require.Equal(t, pb.RankedTier_RANKED_TIER_CHUSHI, cat.TierFor(139999, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_QINGFENG, cat.TierFor(140000, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_QINGFENG, cat.TierFor(154000, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_YUNZHANG, cat.TierFor(155600, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_TIANSHU, cat.TierFor(170000, true))
	require.Equal(t, pb.RankedTier_RANKED_TIER_WUSHUANG, cat.TierFor(185000, true))

	prefs := &pb.MatchRankedPlayerPrefsRecord{PlayerId: 1}
	rewards := []*pb.MatchRankedRewardRecord{
		{PlayerId: 1, SeasonId: 1, RewardKind: uint32(pb.RankedRewardKind_RANKED_REWARD_TALISMAN), ItemCode: "s1_qingyun_talisman"},
		{PlayerId: 1, SeasonId: 2, RewardKind: uint32(pb.RankedRewardKind_RANKED_REWARD_TALISMAN), ItemCode: "s2_qingyun_talisman"},
	}
	eff := effectiveCosmeticLoadout(prefs, rewards)
	require.Equal(t, "s2_qingyun_talisman", eff.GetEquippedTalismanCode(), "未显式隐藏且未手选时默认挂最新青云玉符")

	rawHidden, err := proto.Marshal(&pb.RankedCosmeticLoadout{HideTalisman: true})
	require.NoError(t, err)
	prefs.CosmeticLoadout = rawHidden
	effHidden := effectiveCosmeticLoadout(prefs, rewards)
	require.Empty(t, effHidden.GetEquippedTalismanCode(), "显式隐藏玉符后必须保持空串")
}
