package war

import (
	"testing"

	"github.com/stretchr/testify/require"
	pb "proto/guild"
	tablepb "shared/generated/pb/table"
)

func TestGoldLeylineDeterministicHourlyRotation(t *testing.T) {
	id0, end0 := GoldLeylineForHour(0)
	require.Equal(t, uint32(1), id0)
	require.Equal(t, uint64(3_600_000), end0)

	id1, end1 := GoldLeylineForHour(3_600_000)
	require.Equal(t, uint32(2), id1)
	require.Equal(t, uint64(7_200_000), end1)

	id2, _ := GoldLeylineForHour(7_200_000)
	require.Equal(t, uint32(3), id2)

	id3, _ := GoldLeylineForHour(10_800_000)
	require.Equal(t, uint32(1), id3)
}

func TestDiminishAndStreakJackpotAndRivalry(t *testing.T) {
	require.Equal(t, uint32(100), DiminishPercent(0))
	require.Equal(t, uint32(50), DiminishPercent(1))
	require.Equal(t, uint32(20), DiminishPercent(2))
	require.Equal(t, uint32(0), DiminishPercent(3))

	// 同对手刷连胜（distinctOpponents < 2）不发赏金。
	c, s := StreakBountyJackpot(5, 1)
	require.Equal(t, uint64(0), c)
	require.Equal(t, uint64(0), s)

	c, s = StreakBountyJackpot(5, 2)
	require.Equal(t, uint64(200), c)
	require.Equal(t, uint64(6000), s)

	c, s = StreakBountyJackpot(8, 3)
	require.Equal(t, uint64(400), c)
	require.Equal(t, uint64(12000), s)

	require.False(t, IsRivalryActive(4, false))
	require.True(t, IsRivalryActive(5, false))
	require.True(t, IsRivalryActive(1, true))

	// 金脉 1.5x + 宿敌 +20%：80 -> 120 -> 144。
	r := ComputeSideReward(BattleRewardInput{
		BattleKind:      pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_LEYLINE_PVP,
		Won:             true,
		DiminishPercent: 100,
		IsGoldLeyline:   true,
		IsRivalGuild:    true,
	})
	require.Equal(t, uint64(144), r.Contribution)
	require.Equal(t, uint64(54), r.Merit)
}

func TestBountyTiersAndBannerMaxOneLeyline(t *testing.T) {
	spec1, ok := ResolveBountyTier(nil, pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_NORMAL_SILVER)
	require.True(t, ok)
	require.Equal(t, uint64(5000), spec1.CostSilver)
	require.Equal(t, uint64(500), spec1.TaxSilver)
	require.Equal(t, uint64(4500), spec1.PoolSilver)

	spec3, ok := ResolveBountyTier(nil, pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC)
	require.True(t, ok)
	require.True(t, spec3.RequiresOfficer)
	require.Equal(t, uint64(2000), spec3.CostGuildFunds)

	// 护盾期内不扣血。
	dShield := ApplyBannerImpact(nil, 1000, 200, 600, 5000, 100, false, true, true, false)
	require.Equal(t, uint32(600), dShield.HPAfter)
	require.False(t, dShield.BrokeBanner)

	// 未占领任何灵脉时破旗 -> 易主并给 15 分钟护盾。
	dCap := ApplyBannerImpact(nil, 10000, 200, 100, 0, 100, false, true, true, false)
	require.True(t, dCap.BrokeBanner)
	require.True(t, dCap.CapturedByWinner)
	require.Equal(t, uint64(100), dCap.OwnerAfter)
	require.Equal(t, uint32(600), dCap.HPAfter)
	require.Equal(t, uint64(10000+900_000), dCap.ShieldUntilAfter)

	// 已占领 1 座灵脉时破旗 -> 只清敌旗领赏金，不兼并第 2 座。
	dNoMonopoly := ApplyBannerImpact(nil, 10000, 200, 100, 0, 100, true, true, true, false)
	require.True(t, dNoMonopoly.BrokeBanner)
	require.False(t, dNoMonopoly.CapturedByWinner)
	require.Equal(t, uint64(0), dNoMonopoly.OwnerAfter)
	require.Greater(t, dNoMonopoly.BreakBonusSilver, uint64(0))
}

func TestValidateWarTables(t *testing.T) {
	rule := &tablepb.GuildWarRuleTable{
		Id:                    1,
		LeylineMaxBannerHp:    1000,
		LeylineCaptureInitHp:  600,
		GarrisonMaxSlots:      5,
		BountyTaxBps:          1000,
		BountyCounterKillTear: 2,
		BattleConfigId:        1,
	}
	milestones := []*tablepb.GuildWarMilestoneTable{
		{Id: 1, RequiredGuildMerit: 300, RewardItem1Id: 3403, RewardItem1Count: 2},
		{Id: 2, RequiredGuildMerit: 1200, RewardItem1Id: 3406, RewardItem1Count: 3},
	}
	require.NoError(t, validateWarTables(rule, milestones, func(uint32) bool { return true }, func(uint32) bool { return true }))
	require.Error(t, validateWarTables(rule, milestones, func(uint32) bool { return false }, func(uint32) bool { return true }))
}

func TestFeudRulesDiminishBonusWinnerSummaryAndStreakAlerts(t *testing.T) {
	// 2 小时宿敌约战期间前 3 场 100%，第 4 场 20%，第 5 场起 0%
	require.Equal(t, uint32(100), DiminishPercentWithFeud(0, true))
	require.Equal(t, uint32(100), DiminishPercentWithFeud(2, true))
	require.Equal(t, uint32(20), DiminishPercentWithFeud(3, true))
	require.Equal(t, uint32(0), DiminishPercentWithFeud(4, true))

	// +25% 约战收益替代 +20% 宿敌加成（不叠加）
	feudWin := ComputeSideReward(BattleRewardInput{
		BattleKind:      pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_PANEL_PVP,
		Won:             true,
		DiminishPercent: 100,
		IsRivalGuild:    true,
		IsFeudActive:    true,
	})
	require.Equal(t, uint64(100), feudWin.Contribution) // 80 * 1.25 = 100
	require.Equal(t, uint64(3750), feudWin.Silver)      // 3000 * 1.25 = 3750

	// 约战胜负先比胜场、再比战功、仍同为 0
	require.Equal(t, uint64(10), ResolveFeudWinner(10, 20, 6, 4, 100, 999))
	require.Equal(t, uint64(20), ResolveFeudWinner(10, 20, 5, 5, 200, 350))
	require.Equal(t, uint64(0), ResolveFeudWinner(10, 20, 5, 5, 200, 200))

	sum := FormatFeudSummary(&pb.GuildWarFeudRecord{
		Status:          uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_FINISHED),
		IssuerGuildId:   10,
		IssuerGuildName: "沧澜盟",
		TargetGuildId:   20,
		TargetGuildName: "青云阁",
		IssuerWins:      12,
		TargetWins:      8,
		WinnerGuildId:   10,
	})
	require.Equal(t, "约战大捷：沧澜盟 12:8 胜 青云阁", sum)

	require.True(t, ShouldBroadcastStreakAlert(5, 0))
	require.True(t, ShouldBroadcastStreakAlert(8, 0))
	require.True(t, ShouldBroadcastStreakAlert(1, 6))
	require.False(t, ShouldBroadcastStreakAlert(4, 4))
}
