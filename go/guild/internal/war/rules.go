// Package war 收口「帮会全天帮战 V2」(docs/design/guild-play-v2.md)的纯规则、配置校验与确定性数值算法。
// 零数据库 / Redis / RPC 依赖，便于单测全覆盖并确保 Go 与 Java 双端规则逐项同源。
package war

import (
	"errors"
	"fmt"
	"sort"

	pb "proto/guild"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

const (
	RuleRowID uint32 = 1

	// SafeZoneSceneConfigID 天墉城永久安全区（不可开野外直打、不可在此被追杀）。
	SafeZoneSceneConfigID uint32 = 1

	// MaxLeylinesPerGuild 每帮同时至多占领 1 座灵脉，防大帮垄断。
	MaxLeylinesPerGuild = 1

	// RivalryClashThreshold 同周两帮交手 >= 5 次自动结为宿敌，帮贡与战功 +20%。
	RivalryClashThreshold uint32 = 5

	// MaxBountyTrackSeconds / MaxGarrisonSlots 等默认值（当未传配表行时用于单测兜底）。
	DefaultMinLevel                uint32 = 15
	DefaultPeaceCooldownSec        uint32 = 60
	DefaultPostBattleProtectSec    uint32 = 10
	DefaultLeylineMaxBannerHP      uint32 = 1000
	DefaultLeylineCaptureInitHP    uint32 = 600
	DefaultLeylineShieldSec        uint32 = 900
	DefaultPvpWinBannerDamage      uint32 = 120
	DefaultPvpDefendBannerHeal     uint32 = 40
	DefaultSiegeWinBannerDamage    uint32 = 80
	DefaultGarrisonMaxSlots        uint32 = 5
	DefaultGarrisonRefreshCD       uint32 = 60
	DefaultBountyNormalSilver      uint64 = 5000
	DefaultBountyHeavySilver       uint64 = 15000
	DefaultBountyTaxBps            uint32 = 1000
	DefaultBountyPublicFunds       uint64 = 2000
	DefaultBountyPublicSilverPool  uint64 = 6000
	DefaultBountyPublicDailyLimit  uint32 = 2
	DefaultBountyTTLSec            uint32 = 86400
	DefaultBountyProtectSec        uint32 = 1800
	DefaultBountyCounterKillTear   uint32 = 2
	DefaultBountyDailyPayoutLimit  uint32 = 5
	DefaultTrackCooldownSec        uint32 = 15
	DefaultDiminishWindowSec       uint32 = 1800
	DefaultDividendIntervalSec     uint32 = 3600
	DefaultDividendGuildWeeklyCap  uint32 = 24
	DefaultDividendPlayerWeeklyCap uint32 = 12
	DefaultDividendMinMerit        uint64 = 15
	DefaultGoldLeylineBps          uint32 = 15000
	DefaultRivalryBonusBps         uint32 = 2000
	DefaultFeudBonusBps            uint32 = 2500 // 2 小时宿敌约战收益 +25%（替代宿敌 +20%，不叠加）
	DefaultFeudAcceptWindowSec     uint32 = 900  // 战书应战窗 15 分钟
	DefaultFeudDurationSec         uint32 = 7200 // 宿敌约战持续 2 小时
	DefaultFeudCooldownSec         uint32 = 86400
	FeudFreeDiminishFights         uint32 = 3 // 约战对手 30 分钟内前 3 场保持 100% 收益
	StreakBroadcastMin             uint32 = 5 // 达成 5/8 连胜或终结 >=5 连胜触发全帮/战区捷报
	DefaultDividendSilverPerShare  uint64 = 1500
	DefaultDividendContribPerShare uint64 = 40
)

// LeylineInfo 灵脉静态元数据。
type LeylineInfo struct {
	ID            uint32
	Name          string
	SceneConfigID uint32
}

var AllLeylines = []LeylineInfo{
	{ID: 1, Name: "八仙岛", SceneConfigID: 2},
	{ID: 2, Name: "碧渊水宫", SceneConfigID: 5},
	{ID: 3, Name: "月汐湾", SceneConfigID: 3},
}

func LeylineByID(id uint32) (LeylineInfo, bool) {
	for _, l := range AllLeylines {
		if l.ID == id {
			return l, true
		}
	}
	return LeylineInfo{}, false
}

func LeylineByScene(sceneConfigID uint32) (LeylineInfo, bool) {
	for _, l := range AllLeylines {
		if l.SceneConfigID == sceneConfigID {
			return l, true
		}
	}
	return LeylineInfo{}, false
}

func IsSafeZoneScene(sceneConfigID uint32) bool {
	return sceneConfigID == SafeZoneSceneConfigID
}

// GoldLeylineForHour 每小时确定性轮换 1 座 1.5× 旺脉（金脉），全服玩家自然汇聚。
func GoldLeylineForHour(nowMs uint64) (leylineID uint32, endsAtMs uint64) {
	hour := nowMs / 3_600_000
	return uint32(hour%3) + 1, (hour + 1) * 3_600_000
}

// IsRivalryActive 两帮同周交战达 5 场或互发过追杀令即成宿敌。
func IsRivalryActive(clashCount uint32, bountyMarked bool) bool {
	return clashCount >= RivalryClashThreshold || bountyMarked
}

// DiminishPercent 同对手 30 分钟内收益递减：第 1 次 100%、第 2 次 50%、第 3 次 20%、第 4 次起 0%。
func DiminishPercent(recentCount uint32) uint32 {
	return DiminishPercentWithFeud(recentCount, false)
}

// DiminishPercentWithFeud 2 小时宿敌约战期间，对阵约战敌帮前 3 场保持 100%，第 4 场 20%，第 5 场起 0%（仍守防互刷底线）。
func DiminishPercentWithFeud(recentCount uint32, feudActive bool) uint32 {
	if feudActive {
		if recentCount < FeudFreeDiminishFights {
			return 100
		}
		if recentCount == FeudFreeDiminishFights {
			return 20
		}
		return 0
	}
	switch recentCount {
	case 0:
		return 100
	case 1:
		return 50
	case 2:
		return 20
	default:
		return 0
	}
}

// ResolveFeudWinner 约战收官胜负判定：先比胜场，同胜场比约战战功，仍平局返回 0。
func ResolveFeudWinner(issuerGuildID, targetGuildID uint64, issuerWins, targetWins uint32, issuerMerit, targetMerit uint64) uint64 {
	if issuerWins > targetWins {
		return issuerGuildID
	}
	if targetWins > issuerWins {
		return targetGuildID
	}
	if issuerMerit > targetMerit {
		return issuerGuildID
	}
	if targetMerit > issuerMerit {
		return targetGuildID
	}
	return 0
}

// FormatFeudSummary 生成约战收官战报碑摘要（纯服务端拼接，零玩家自由文本）。
func FormatFeudSummary(rec *pb.GuildWarFeudRecord) string {
	if rec == nil || rec.GetStatus() != uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_FINISHED) {
		return ""
	}
	in := rec.GetIssuerGuildName()
	tn := rec.GetTargetGuildName()
	if rec.GetWinnerGuildId() == rec.GetIssuerGuildId() && rec.GetWinnerGuildId() != 0 {
		return fmt.Sprintf("约战大捷：%s %d:%d 胜 %s", in, rec.GetIssuerWins(), rec.GetTargetWins(), tn)
	}
	if rec.GetWinnerGuildId() == rec.GetTargetGuildId() && rec.GetWinnerGuildId() != 0 {
		return fmt.Sprintf("约战大捷：%s %d:%d 胜 %s", tn, rec.GetTargetWins(), rec.GetIssuerWins(), in)
	}
	return fmt.Sprintf("势均力敌：%s %d:%d 平 %s", in, rec.GetIssuerWins(), rec.GetTargetWins(), tn)
}

// ShouldBroadcastStreakAlert 达成 5/8 连胜或终结 >=5 连胜时推送非阻断捷报。
func ShouldBroadcastStreakAlert(newStreak uint32, endedLoserStreak uint32) bool {
	return newStreak == 5 || newStreak == 8 || endedLoserStreak >= StreakBroadcastMin
}

// ScaleReward 确定性整数倍率计算（先递减，再乘金脉/宿敌加成）。
func ScaleReward(base uint64, diminishPct uint32, goldLeyline bool, goldBps uint32, rival bool, rivalBps uint32) uint64 {
	if base == 0 || diminishPct == 0 {
		return 0
	}
	v := (base * uint64(diminishPct)) / 100
	if v == 0 {
		return 0
	}
	if goldLeyline && goldBps > 10000 {
		v = (v * uint64(goldBps)) / 10000
	}
	if rival && rivalBps > 0 {
		v = (v * (10000 + uint64(rivalBps))) / 10000
	}
	return v
}

// StreakBountyJackpot 终结连胜额外赏金：要求被终结者连胜 >= 3 且连胜期内击败过 >= 2 名不同对手。
func StreakBountyJackpot(endedStreak uint32, distinctOpponents uint32) (contribution uint64, silver uint64) {
	if endedStreak < 3 || distinctOpponents < 2 {
		return 0, 0
	}
	switch {
	case endedStreak >= 8:
		return 400, 12000
	case endedStreak >= 5:
		return 200, 6000
	default:
		return 80, 2500
	}
}

// BountyTierSpec 追杀令三档固定费用、10% 销毁税与入池赏金（严禁自由填金额）。
type BountyTierSpec struct {
	CostSilver      uint64
	CostGuildFunds  uint64
	TaxSilver       uint64
	PoolSilver      uint64
	RequiresOfficer bool
}

func ResolveBountyTier(rule *tablepb.GuildWarRuleTable, tier pb.GuildWarBountyTier) (BountyTierSpec, bool) {
	normalSilver := DefaultBountyNormalSilver
	heavySilver := DefaultBountyHeavySilver
	taxBps := DefaultBountyTaxBps
	pubFunds := DefaultBountyPublicFunds
	if rule != nil {
		if rule.GetBountyNormalSilver() > 0 {
			normalSilver = rule.GetBountyNormalSilver()
		}
		if rule.GetBountyHeavySilver() > 0 {
			heavySilver = rule.GetBountyHeavySilver()
		}
		if rule.GetBountyTaxBps() > 0 {
			taxBps = rule.GetBountyTaxBps()
		}
		if rule.GetBountyPublicFunds() > 0 {
			pubFunds = rule.GetBountyPublicFunds()
		}
	}
	switch tier {
	case pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_NORMAL_SILVER:
		tax := (normalSilver * uint64(taxBps)) / 10000
		return BountyTierSpec{CostSilver: normalSilver, TaxSilver: tax, PoolSilver: normalSilver - tax}, true
	case pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_HEAVY_SILVER:
		tax := (heavySilver * uint64(taxBps)) / 10000
		return BountyTierSpec{CostSilver: heavySilver, TaxSilver: tax, PoolSilver: heavySilver - tax}, true
	case pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC:
		return BountyTierSpec{CostGuildFunds: pubFunds, PoolSilver: DefaultBountyPublicSilverPool, RequiresOfficer: true}, true
	default:
		return BountyTierSpec{}, false
	}
}

// BattleRewardInput 单局帮战结算输入（每方按胜负分别求单人基础奖励）。
type BattleRewardInput struct {
	BattleKind        pb.GuildWarBattleKind
	Won               bool
	Draw              bool
	DiminishPercent   uint32
	IsGoldLeyline     bool
	IsRivalGuild      bool
	IsFeudActive      bool
	GoldLeylineBps    uint32
	RivalryBonusBps   uint32
	Rule              *tablepb.GuildWarRuleTable
}

type SideReward struct {
	Contribution uint64
	Silver       uint64
	GuildFunds   uint64
	Merit        uint64
}

// ComputeSideReward 计算进攻方或防守方单人基础帮战收益（不含连胜终结、破旗首功、追杀赏金）。
func ComputeSideReward(in BattleRewardInput) SideReward {
	goldBps := in.GoldLeylineBps
	if goldBps == 0 {
		goldBps = DefaultGoldLeylineBps
	}
	rivalBps := in.RivalryBonusBps
	if rivalBps == 0 {
		rivalBps = DefaultRivalryBonusBps
	}
	hasRivalOrFeud := in.IsRivalGuild || in.IsFeudActive
	if in.IsFeudActive && DefaultFeudBonusBps > rivalBps {
		rivalBps = DefaultFeudBonusBps
	}
	isSiege := in.BattleKind == pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_GARRISON_SIEGE
	var baseContrib, baseSilver, baseFunds, baseMerit uint64
	switch {
	case isSiege && in.Won:
		baseContrib, baseSilver, baseFunds, baseMerit = 55, 2000, 260, 20
	case isSiege:
		baseContrib, baseSilver, baseFunds, baseMerit = 15, 500, 60, 5
	case in.Won:
		baseContrib, baseSilver, baseFunds, baseMerit = 80, 3000, 400, 30
	case in.Draw:
		baseContrib, baseSilver, baseFunds, baseMerit = 45, 1600, 200, 18
	default:
		baseContrib, baseSilver, baseFunds, baseMerit = 35, 1200, 150, 12
	}
	return SideReward{
		Contribution: ScaleReward(baseContrib, in.DiminishPercent, in.IsGoldLeyline, goldBps, hasRivalOrFeud, rivalBps),
		Silver:       ScaleReward(baseSilver, in.DiminishPercent, in.IsGoldLeyline, goldBps, hasRivalOrFeud, rivalBps),
		GuildFunds:   ScaleReward(baseFunds, in.DiminishPercent, in.IsGoldLeyline, goldBps, hasRivalOrFeud, rivalBps),
		Merit:        ScaleReward(baseMerit, in.DiminishPercent, in.IsGoldLeyline, goldBps, hasRivalOrFeud, rivalBps),
	}
}

// BannerDelta 灵脉旗势变化与易主判定（每帮最多占领 1 座灵脉，已占 1 座破旗只发赏金不兼并）。
type BannerDelta struct {
	HPBefore         uint32
	HPAfter          uint32
	OwnerAfter       uint64
	ShieldUntilAfter uint64
	BrokeBanner      bool
	CapturedByWinner bool
	BreakBonusFunds  uint64
	BreakBonusSilver uint64
}

func ApplyBannerImpact(
	rule *tablepb.GuildWarRuleTable,
	nowMs uint64,
	currentOwner uint64,
	currentHP uint32,
	currentShieldUntil uint64,
	attackerGuildID uint64,
	attackerAlreadyOwnsOther bool,
	attackerWon bool,
	isLivePvp bool,
	isGoldLeyline bool,
) BannerDelta {
	maxHP := DefaultLeylineMaxBannerHP
	initHP := DefaultLeylineCaptureInitHP
	shieldSec := DefaultLeylineShieldSec
	pvpDmg := DefaultPvpWinBannerDamage
	pvpHeal := DefaultPvpDefendBannerHeal
	siegeDmg := DefaultSiegeWinBannerDamage
	goldBps := DefaultGoldLeylineBps
	if rule != nil {
		if rule.GetLeylineMaxBannerHp() > 0 {
			maxHP = rule.GetLeylineMaxBannerHp()
		}
		if rule.GetLeylineCaptureInitHp() > 0 {
			initHP = rule.GetLeylineCaptureInitHp()
		}
		if rule.GetLeylineShieldSec() > 0 {
			shieldSec = rule.GetLeylineShieldSec()
		}
		if rule.GetPvpWinBannerDamage() > 0 {
			pvpDmg = rule.GetPvpWinBannerDamage()
		}
		if rule.GetPvpDefendBannerHeal() > 0 {
			pvpHeal = rule.GetPvpDefendBannerHeal()
		}
		if rule.GetSiegeWinBannerDamage() > 0 {
			siegeDmg = rule.GetSiegeWinBannerDamage()
		}
		if rule.GetGoldLeylineMultiplierBps() > 0 {
			goldBps = rule.GetGoldLeylineMultiplierBps()
		}
	}

	out := BannerDelta{
		HPBefore:         currentHP,
		HPAfter:          currentHP,
		OwnerAfter:       currentOwner,
		ShieldUntilAfter: currentShieldUntil,
	}
	// 护盾期内旗势免疫扣减（仍可打架赚帮贡与连胜）。
	if nowMs < currentShieldUntil {
		return out
	}
	if !attackerWon {
		if isLivePvp && currentOwner != 0 {
			healed := currentHP + pvpHeal
			if healed > maxHP {
				healed = maxHP
			}
			out.HPAfter = healed
		}
		return out
	}

	dmg := siegeDmg
	if isLivePvp {
		dmg = pvpDmg
	}
	if isGoldLeyline && goldBps > 10000 {
		dmg = uint32((uint64(dmg) * uint64(goldBps)) / 10000)
	}
	if dmg >= currentHP {
		out.BrokeBanner = true
		out.BreakBonusFunds = 1500
		out.BreakBonusSilver = 5000
		if attackerAlreadyOwnsOther {
			// 已占 1 座：破旗只清敌旗并享破旗首功赏金，不兼并第 2 座。
			out.OwnerAfter = 0
			out.HPAfter = initHP
			out.ShieldUntilAfter = 0
		} else {
			out.CapturedByWinner = true
			out.OwnerAfter = attackerGuildID
			out.HPAfter = initHP
			out.ShieldUntilAfter = nowMs + uint64(shieldSec)*1000
		}
	} else {
		out.HPAfter = currentHP - dmg
	}
	return out
}

// ValidateTables 校验 GuildWarRule 与 GuildWarMilestone 全表配置（启动与热更共用）。
func ValidateTables() error {
	rule, _ := table.GuildWarRuleTableManagerInstance.FindById(RuleRowID)
	return validateWarTables(
		rule,
		table.GuildWarMilestoneTableManagerInstance.FindAll(),
		table.ItemTableManagerInstance.Exists,
		table.DungeonTableManagerInstance.Exists,
	)
}

func ValidateTablesIn(ts *table.TableSet) error {
	if ts == nil || ts.GuildWarRule == nil || ts.GuildWarMilestone == nil || ts.Item == nil || ts.Dungeon == nil {
		return errors.New("guild war tables: TableSet missing required managers")
	}
	rule, _ := ts.GuildWarRule.FindById(RuleRowID)
	return validateWarTables(
		rule,
		ts.GuildWarMilestone.FindAll(),
		ts.Item.Exists,
		ts.Dungeon.Exists,
	)
}

func validateWarTables(
	rule *tablepb.GuildWarRuleTable,
	milestones []*tablepb.GuildWarMilestoneTable,
	itemExists func(uint32) bool,
	dungeonExists func(uint32) bool,
) error {
	if rule == nil {
		return fmt.Errorf("GuildWarRule 缺少规则行 id=%d", RuleRowID)
	}
	if rule.GetLeylineMaxBannerHp() == 0 || rule.GetLeylineCaptureInitHp() == 0 || rule.GetLeylineCaptureInitHp() > rule.GetLeylineMaxBannerHp() {
		return fmt.Errorf("GuildWarRule id=%d 灵脉旗势范围无效(init=%d, max=%d)", RuleRowID, rule.GetLeylineCaptureInitHp(), rule.GetLeylineMaxBannerHp())
	}
	if rule.GetGarrisonMaxSlots() == 0 || rule.GetGarrisonMaxSlots() > 5 {
		return fmt.Errorf("GuildWarRule id=%d garrison_max_slots=%d 越界(1..5)", RuleRowID, rule.GetGarrisonMaxSlots())
	}
	if rule.GetBountyTaxBps() >= 10000 {
		return fmt.Errorf("GuildWarRule id=%d bounty_tax_bps=%d 必须小于 10000", RuleRowID, rule.GetBountyTaxBps())
	}
	if rule.GetBountyCounterKillTear() == 0 {
		return fmt.Errorf("GuildWarRule id=%d bounty_counter_kill_tear 必须大于 0", RuleRowID)
	}
	if rule.GetBattleConfigId() == 0 || !dungeonExists(rule.GetBattleConfigId()) {
		return fmt.Errorf("GuildWarRule id=%d battle_config_id=%d 在 Dungeon 表中不存在", RuleRowID, rule.GetBattleConfigId())
	}
	if len(milestones) == 0 || len(milestones) > 8 {
		return fmt.Errorf("GuildWarMilestone 行数=%d 越界(1..8)", len(milestones))
	}
	ordered := append([]*tablepb.GuildWarMilestoneTable(nil), milestones...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].GetId() < ordered[j].GetId() })
	var prevMerit uint64
	for idx, row := range ordered {
		if row == nil || row.GetId() != uint32(idx+1) {
			return fmt.Errorf("GuildWarMilestone id 必须从 1 连续递增")
		}
		if row.GetRequiredGuildMerit() <= prevMerit {
			return fmt.Errorf("GuildWarMilestone id=%d required_guild_merit=%d 必须严格递增", row.GetId(), row.GetRequiredGuildMerit())
		}
		prevMerit = row.GetRequiredGuildMerit()
		if row.GetRewardItem1Id() != 0 && (!itemExists(row.GetRewardItem1Id()) || row.GetRewardItem1Count() == 0) {
			return fmt.Errorf("GuildWarMilestone id=%d reward_item1 无效", row.GetId())
		}
		if row.GetRewardItem2Id() != 0 && (!itemExists(row.GetRewardItem2Id()) || row.GetRewardItem2Count() == 0) {
			return fmt.Errorf("GuildWarMilestone id=%d reward_item2 无效", row.GetId())
		}
	}
	return nil
}
