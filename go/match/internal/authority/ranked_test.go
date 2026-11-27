package authority

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestRankedV6RivalryAndFeaturedBuildHelpers(t *testing.T) {
	// 宿敌复仇判定：无历史交手不触发；上次告负或净负触发；连胜压制不触发。
	require.False(t, isRivalryRevengeTarget(nil))
	require.False(t, isRivalryRevengeTarget(&pb.MatchRankedRivalryRecord{Wins: 0, Losses: 0}))
	require.True(t, isRivalryRevengeTarget(&pb.MatchRankedRivalryRecord{Wins: 2, Losses: 1, LastOutcome: 2}))
	require.True(t, isRivalryRevengeTarget(&pb.MatchRankedRivalryRecord{Wins: 1, Losses: 2, LastOutcome: 1}))
	require.False(t, isRivalryRevengeTarget(&pb.MatchRankedRivalryRecord{Wins: 2, Losses: 1, LastOutcome: 1}))

	// 宿敌战绩递增与对手名保留
	r1 := nextRivalryRecord(1, 101, 201, "玄烈", nil, false, false, 8801, 1000)
	require.Equal(t, uint32(0), r1.Wins)
	require.Equal(t, uint32(1), r1.Losses)
	require.Equal(t, uint32(2), r1.LastOutcome)
	require.True(t, isRivalryRevengeTarget(r1))

	r2 := nextRivalryRecord(1, 101, 201, "", r1, false, true, 8802, 2000)
	require.Equal(t, "玄烈", r2.OpponentName)
	require.Equal(t, uint32(1), r2.Wins)
	require.Equal(t, uint32(1), r2.Losses)
	require.Equal(t, uint32(1), r2.LastOutcome)
	require.False(t, isRivalryRevengeTarget(r2))

	// 流派归一键规范化：乱序、重复、超 4 招与零值归一化，保证同套配招聚合到同一行。
	keyA, csvA, normA := canonicalBuildKey(101, 3, []uint32{10018, 10002, 10018, 0, 10041, 10021, 10055})
	keyB, csvB, normB := canonicalBuildKey(101, 3, []uint32{10002, 10041, 10021, 10018})
	require.Equal(t, "101:3:10002,10018,10021,10041", keyA)
	require.Equal(t, keyA, keyB)
	require.Equal(t, "10002,10018,10021,10041", csvA)
	require.Equal(t, csvA, csvB)
	require.Equal(t, []uint32{10002, 10018, 10021, 10041}, normA)
	require.Equal(t, normA, normB)
	require.Equal(t, normA, parseCoreSkillIDs(csvA))

	emptyKey, _, _ := canonicalBuildKey(0, 0, nil)
	require.Empty(t, emptyKey)
}

func TestRankedFeaturedCacheSingleFlightAndSeasonInvalidation(t *testing.T) {
	nowTime := time.Unix(1_700_000_000, 0)
	cache := rankedFeaturedCache{nowFn: func() time.Time { return nowTime }}

	var fetches atomic.Int32
	gate := make(chan struct{})
	fetch := func(season uint32) ([]*pb.RankedFeaturedBuildView, error) {
		fetches.Add(1)
		<-gate
		return []*pb.RankedFeaturedBuildView{
			{BuildKey: "101:3:1001", SeasonSealId: 101, FeaturedBattleId: uint64(season) * 100, Wins: 5},
		}, nil
	}

	// 8 个并发请求落在同一缓存失效窗口时由 SingleFlight 合并为 1 次回源。
	var wg sync.WaitGroup
	results := make([][]*pb.RankedFeaturedBuildView, 8)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got, err := cache.Load(1, 5, func() ([]*pb.RankedFeaturedBuildView, error) { return fetch(1) })
			require.NoError(t, err)
			results[idx] = got
		}(i)
	}
	close(gate)
	wg.Wait()
	require.Equal(t, int32(1), fetches.Load(), "并发请求必须由 SingleFlight 合并为 1 次回源")
	require.Len(t, results[0], 1)
	// 调用方修改返回切片元素不得污染缓存副本。
	results[0][0].Wins = 999
	cached, err := cache.Load(1, 5, func() ([]*pb.RankedFeaturedBuildView, error) {
		fetches.Add(1)
		return nil, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), fetches.Load(), "TTL 内命中缓存不回源")
	require.Equal(t, uint32(5), cached[0].Wins, "缓存深拷贝必须隔离调用方篡改")

	// 切换赛季立即失效重查，不跨季复用旧数据。
	s2, err := cache.Load(2, 5, func() ([]*pb.RankedFeaturedBuildView, error) {
		fetches.Add(1)
		return []*pb.RankedFeaturedBuildView{{BuildKey: "201:4:2002", FeaturedBattleId: 200}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), fetches.Load())
	require.Equal(t, uint64(200), s2[0].FeaturedBattleId)

	// 超过 5 秒 TTL 自动过期刷新；显式 Invalidate 也立即失效。
	nowTime = nowTime.Add(featuredBuildsCacheTTL)
	_, err = cache.Load(2, 5, func() ([]*pb.RankedFeaturedBuildView, error) {
		fetches.Add(1)
		return []*pb.RankedFeaturedBuildView{{BuildKey: "201:4:2002", FeaturedBattleId: 201}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(3), fetches.Load())

	cache.Invalidate()
	_, err = cache.Load(2, 5, func() ([]*pb.RankedFeaturedBuildView, error) {
		fetches.Add(1)
		return []*pb.RankedFeaturedBuildView{{BuildKey: "201:4:2002", FeaturedBattleId: 202}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(4), fetches.Load())
}

func TestCollectFeaturedBuildDeltaDeterministicAggregation(t *testing.T) {
	deltas := make(map[string]*featuredBuildMatchDelta)
	stWinnerHigh := &battlepb.BattleSettlementData{
		PlayerDisplayName: "高段甲",
		FinalSeasonSealId: 101,
		StarterPetTableId: 3,
		CoreSkillIds:      []uint32{1002, 1001},
	}
	stWinnerTieLowerID := &battlepb.BattleSettlementData{
		PlayerDisplayName: "同分乙",
		FinalSeasonSealId: 101,
		StarterPetTableId: 3,
		CoreSkillIds:      []uint32{1001, 1002},
	}
	stLoser := &battlepb.BattleSettlementData{
		PlayerDisplayName: "败者丙",
		FinalSeasonSealId: 101,
		StarterPetTableId: 3,
		CoreSkillIds:      []uint32{1001, 1002},
	}
	collectFeaturedBuildDelta(deltas, 152000, 200, stWinnerHigh, 175000, true)
	collectFeaturedBuildDelta(deltas, 152000, 100, stWinnerTieLowerID, 175000, true)
	collectFeaturedBuildDelta(deltas, 152000, 300, stLoser, 180000, false)

	require.Len(t, deltas, 1, "同局乱序同套配招必须合并为单个 build_key 避免重复加锁")
	d := deltas["101:3:1001,1002"]
	require.NotNil(t, d)
	require.Equal(t, uint32(3), d.appearances)
	require.Equal(t, uint32(2), d.wins)
	require.Equal(t, uint32(2), d.highTierWins)
	require.True(t, d.hasWinner)
	require.Equal(t, uint64(100), d.bestWinnerPlayerID, "同分胜者按较小 player_id 确定性决胜")
	require.Equal(t, "同分乙", d.bestWinnerName)
}

func TestIsEligibleRankedRematchFormation(t *testing.T) {
	valid := &pb.MatchFormationRecord{
		BattleId:       9001,
		Mode:           uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM),
		RankedSeasonId: 1,
		State:          4,
		Members: []*pb.MatchFormationMember{
			{PlayerId: 101, TeamIndex: 0},
			{PlayerId: 202, TeamIndex: 1},
		},
	}
	require.True(t, isEligibleRankedRematchFormation(valid, 101, 202))
	require.True(t, isEligibleRankedRematchFormation(valid, 202, 101))
	require.False(t, isEligibleRankedRematchFormation(valid, 101, 303), "陌生人不得伪造再战")
	require.False(t, isEligibleRankedRematchFormation(nil, 101, 202))

	aborted := proto.Clone(valid).(*pb.MatchFormationRecord)
	aborted.State = 5
	require.False(t, isEligibleRankedRematchFormation(aborted, 101, 202), "作废局不得发起不服再战")

	unranked := proto.Clone(valid).(*pb.MatchFormationRecord)
	unranked.RankedSeasonId = 0
	require.False(t, isEligibleRankedRematchFormation(unranked, 101, 202), "非正式排位不得发起不服再战")

	sameTeam := proto.Clone(valid).(*pb.MatchFormationRecord)
	sameTeam.Members[1].TeamIndex = 0
	require.False(t, isEligibleRankedRematchFormation(sameTeam, 101, 202), "同队成员不得发起不服再战")

	team2v2 := proto.Clone(valid).(*pb.MatchFormationRecord)
	team2v2.Members = append(team2v2.Members, &pb.MatchFormationMember{PlayerId: 303, TeamIndex: 0}, &pb.MatchFormationMember{PlayerId: 404, TeamIndex: 1})
	require.False(t, isEligibleRankedRematchFormation(team2v2, 101, 202), "非 1V1 不得发起 1V1 不服再战")
}
