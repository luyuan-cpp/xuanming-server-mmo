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

// ── r2-gw#1 / #2 / #3:追杀令去向、到期退款与灵脉快照复核 ────────────────────

const (
	testZone        uint32 = 7
	testIssuerGuild uint64 = 100
	testHunterGuild uint64 = 200
	testTargetGuild uint64 = 300
	testIssuer      uint64 = 1001
	testHunter      uint64 = 2001
	testHunter2     uint64 = 2002
	testTarget      uint64 = 3001
	testNow         uint64 = 1_700_000_000_000
)

func activeSilverBounty() *pb.GuildWarBountyRecord {
	return &pb.GuildWarBountyRecord{
		BountyId: 9, ZoneId: testZone, IssuerPlayerId: testIssuer, IssuerGuildId: testIssuerGuild,
		TargetPlayerId: testTarget, TargetGuildId: testTargetGuild,
		Tier:         uint32(pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_NORMAL_SILVER),
		EscrowSilver: 4501, // 奇数:撕单两半的取整去向要钉住
		PublicBoard:  1,
		Status:       uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE),
		ExpiresAtMs:  testNow + 1000,
	}
}

func huntFacts(hunter uint64, hunterWon bool) BountyFacts {
	return BountyFacts{
		WarBattleKind: pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_BOUNTY_HUNT,
		ZoneID:        testZone, HunterGuildID: testHunterGuild, HunterID: hunter, TargetID: testTarget,
		HunterWon: hunterWon, TargetWon: !hunterWon, CounterKillTear: 2, NowMs: testNow,
	}
}

func TestBountyHuntableNeedsSameZoneActiveUnexpiredAndEligibleGuild(t *testing.T) {
	b := activeSilverBounty()
	require.True(t, BountyHuntable(b, testZone, testHunterGuild, testNow))
	require.False(t, BountyHuntable(nil, testZone, testHunterGuild, testNow))
	require.False(t, BountyHuntable(b, testZone+1, testHunterGuild, testNow), "别的区的追杀令不能接")
	require.False(t, BountyHuntable(b, testZone, testHunterGuild, b.GetExpiresAtMs()), "到期那一刻起不能再接")

	private := activeSilverBounty()
	private.PublicBoard = 0
	require.False(t, BountyHuntable(private, testZone, testHunterGuild, testNow), "仅本帮可见的单,别帮不能接")
	require.True(t, BountyHuntable(private, testZone, testIssuerGuild, testNow), "发单帮自己可以接")

	for _, st := range []pb.GuildWarBountyStatus{
		pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ESCROW_PENDING,
		pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED,
		pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_TORN_BY_TARGET,
		pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_EXPIRED,
		pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ESCROW_FAILED,
	} {
		other := activeSilverBounty()
		other.Status = uint32(st)
		require.False(t, BountyHuntable(other, testZone, testHunterGuild, testNow), st.String())
	}
}

// 结算第 7 步:目标不符、战斗种类不符、区服不符、接单资格不符都不动追杀令(r2-gw#1 的攻击面)。
func TestResolveBountyLeavesBountyUntouchedWhenFactsDoNotMatch(t *testing.T) {
	b := activeSilverBounty()
	cases := map[string]func(*BountyFacts){
		"防守方不是目标(带别人的追杀令号打任意一局)": func(f *BountyFacts) { f.TargetID = testTarget + 1 },
		"不是追杀局":           func(f *BountyFacts) { f.WarBattleKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_FIELD_ROAD },
		"守旗阵(没有真人守方)":    func(f *BountyFacts) { f.WarBattleKind = pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_GARRISON_SIEGE; f.TargetID = 0 },
		"别的区":             func(f *BountyFacts) { f.ZoneID = testZone + 1 },
		"对局结束时已过期":        func(f *BountyFacts) { f.NowMs = b.GetExpiresAtMs() },
		"没有攻方真人":          func(f *BountyFacts) { f.HunterID = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := huntFacts(testHunter, true)
			mutate(&f)
			require.Equal(t, BountyResolution{}, ResolveBounty(b, f))
		})
	}
	private := activeSilverBounty()
	private.PublicBoard = 0
	require.Equal(t, BountyResolution{}, ResolveBounty(private, huntFacts(testHunter, true)), "猎手帮没有接单资格")

	draw := huntFacts(testHunter, true)
	draw.HunterWon, draw.TargetWon = false, false
	require.Equal(t, BountyResolution{}, ResolveBounty(b, draw), "平局不动追杀令")
}

func TestResolveBountyHunterWinPaysFullEscrowToHunter(t *testing.T) {
	res := ResolveBounty(activeSilverBounty(), huntFacts(testHunter, true))
	require.True(t, res.Touch)
	require.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED), res.Status)
	require.Equal(t, testHunter, res.HunterPlayerID)
	require.Equal(t, testNow, res.ResolvedAtMs)
	require.Equal(t, BountyTagHunted, res.OutcomeTag)
	require.Equal(t, testHunter, res.RewardRecipientID)
	require.Equal(t, uint64(4501), res.RewardSilver)
	require.Zero(t, res.IssuerRefundSilver)
	require.Zero(t, res.IssuerRefundGuildFunds)
}

// 撕单两半去向:一半(向下取整)给目标,其余退发布者;银两档退银两,公费档按发单帮实付资金退一半(r2-gw#2)。
func TestResolveBountyTearSplitsEscrowBetweenTargetAndIssuer(t *testing.T) {
	b := activeSilverBounty()
	first := ResolveBounty(b, huntFacts(testHunter, false))
	require.True(t, first.Touch)
	require.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE), first.Status)
	require.Equal(t, uint32(1), first.CounterKills)
	require.Equal(t, testHunter, first.LastCounterAttackerID)
	require.Equal(t, BountyTagCounterKill, first.OutcomeTag)
	require.Zero(t, first.RewardSilver, "未到撕单数不转账")

	b.CounterKills, b.LastCounterAttackerId = first.CounterKills, first.LastCounterAttackerID
	require.Equal(t, BountyResolution{}, ResolveBounty(b, huntFacts(testHunter, false)), "同一名追兵连输不叠反杀数")

	torn := ResolveBounty(b, huntFacts(testHunter2, false))
	require.Equal(t, uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_TORN_BY_TARGET), torn.Status)
	require.Equal(t, BountyTagTorn, torn.OutcomeTag)
	require.Equal(t, testTarget, torn.RewardRecipientID)
	require.Equal(t, uint64(2250), torn.RewardSilver)
	require.Equal(t, uint64(2251), torn.IssuerRefundSilver)
	require.Equal(t, b.GetEscrowSilver(), torn.RewardSilver+torn.IssuerRefundSilver, "两半合计等于托管,一两不丢不多")
	require.Zero(t, torn.IssuerRefundGuildFunds)

	pub := activeSilverBounty()
	pub.Tier = uint32(pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC)
	pub.EscrowSilver, pub.EscrowGuildFunds = 6000, 2001
	pub.CounterKills, pub.LastCounterAttackerId = 1, testHunter
	pubTorn := ResolveBounty(pub, huntFacts(testHunter2, false))
	require.Equal(t, uint64(3000), pubTorn.RewardSilver, "目标拿系统赏金池的一半")
	require.Zero(t, pubTorn.IssuerRefundSilver, "公费档的赏金池是系统配给的,不退银两")
	require.Equal(t, uint64(1001), pubTorn.IssuerRefundGuildFunds, "按实付资金退一半(其余一半)")
}

func TestBountyExpiryRefundReturnsWhatTheIssuerPaid(t *testing.T) {
	silver, funds := BountyExpiryRefund(activeSilverBounty())
	require.Equal(t, uint64(4501), silver, "银两档退扣税后的托管")
	require.Zero(t, funds)

	pub := activeSilverBounty()
	pub.Tier = uint32(pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC)
	pub.EscrowSilver, pub.EscrowGuildFunds = 6000, 2000
	silver, funds = BountyExpiryRefund(pub)
	require.Zero(t, silver, "系统赏金池不退")
	require.Equal(t, uint64(2000), funds, "公费档退实扣的帮会资金")
}

// 灵脉快照复核(r2-gw#3):主人、旗势、护盾、攻方是否已占他脉任一项变了都判过时。
func TestBannerDeltaBasisHoldsOnlyForTheSnapshotItWasComputedFrom(t *testing.T) {
	rows := []*pb.GuildWarLeylineRecord{
		{LeylineId: 1, OwnerGuildId: 300, BannerHp: 200, ShieldUntilMs: 0},
		{LeylineId: 2, OwnerGuildId: 0, BannerHp: 600},
		{LeylineId: 3, OwnerGuildId: 400, BannerHp: 900},
	}
	cur, ownsOther := FindLeylineBasis(rows, 1, 100)
	require.Same(t, rows[0], cur)
	require.False(t, ownsOther)
	d := ApplyBannerImpact(nil, testNow, cur.GetOwnerGuildId(), cur.GetBannerHp(), cur.GetShieldUntilMs(), 100, ownsOther, true, true, false)
	require.Equal(t, uint64(300), d.OwnerBefore)
	require.True(t, d.BasisHolds(cur, ownsOther))

	_, ownsOther400 := FindLeylineBasis(rows, 1, 400)
	require.True(t, ownsOther400, "攻方帮占着 3 号灵脉")

	stale := []func(*pb.GuildWarLeylineRecord) bool{
		func(r *pb.GuildWarLeylineRecord) bool { r.BannerHp -= 120; return false },     // 另一场先扣了旗势
		func(r *pb.GuildWarLeylineRecord) bool { r.OwnerGuildId = 500; return false },  // 已易主
		func(r *pb.GuildWarLeylineRecord) bool { r.ShieldUntilMs = testNow; return false }, // 护盾变了
		func(*pb.GuildWarLeylineRecord) bool { return true },                            // 攻方同时拿下了别的灵脉
	}
	for i, mutate := range stale {
		row := &pb.GuildWarLeylineRecord{LeylineId: 1, OwnerGuildId: 300, BannerHp: 200}
		other := mutate(row)
		require.False(t, d.BasisHolds(row, other), "case %d", i)
	}
	require.False(t, d.BasisHolds(nil, false), "锁内读不到目标灵脉行")
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
