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

// guild_war_battle.bounty_outcome_tag 里追杀令的三种去向(0 无 / 1 终结连胜 / 5 破旗首功由结算其余步骤写)。
const (
	BountyTagHunted      uint32 = 2 // 追杀成功销单
	BountyTagCounterKill uint32 = 3 // 目标反杀一名追兵(未到撕单数)
	BountyTagTorn        uint32 = 4 // 目标反杀够数撕单
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

// BountyHuntable 报告这张追杀令此刻能否由 hunterGuildID 的成员在 zoneID 接单结单(r2-gw#1):同区、ACTIVE 且未过期、
// 挂在全区江湖榜或就是本帮发的单。开战入口(追杀榜直打、直打活人自动挂钩)、追踪与结算第 7 步(行锁内)共用这一处判定。
func BountyHuntable(b *pb.GuildWarBountyRecord, zoneID uint32, hunterGuildID uint64, nowMs uint64) bool {
	return b != nil && b.GetZoneId() == zoneID &&
		b.GetStatus() == uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE) &&
		b.GetExpiresAtMs() > nowMs &&
		(b.GetPublicBoard() != 0 || b.GetIssuerGuildId() == hunterGuildID)
}

// BountyFacts 是结算第 7 步判定一张追杀令去向要用的本局事实。全部取自结算消息与开战时服务端冻结的上下文,
// 没有一项是客户端能改的值(r2-gw#1)。追杀局的防守方就是目标本人,所以 TargetID 取防守方真人首人。
type BountyFacts struct {
	WarBattleKind   pb.GuildWarBattleKind
	ZoneID          uint32
	HunterGuildID   uint64 // 本局攻方帮(开战时冻结)
	HunterID        uint64 // 攻方真人首人
	TargetID        uint64 // 防守方真人首人
	HunterWon       bool
	TargetWon       bool
	CounterKillTear uint32
	NowMs           uint64 // 本局结束时刻:到期与否按它判
}

// BountyResolution 是一张追杀令在本局之后的去向。Touch=false 表示不动追杀令(本局其余结算照常)。
// 前五个状态字段是要写回 guild_war_bounty 的整行新值;其余是同一事务里要发生的转账。
type BountyResolution struct {
	Touch                 bool
	Status                uint32
	CounterKills          uint32
	LastCounterAttackerID uint64
	HunterPlayerID        uint64
	ResolvedAtMs          uint64
	OutcomeTag            uint32
	// RewardSilver 给 RewardRecipientID(追杀成功 = 猎手,撕单 = 目标),受每日领赏上限约束。
	RewardRecipientID uint64
	RewardSilver      uint64
	// 撕单时退给发布者的另一半:银两档退银两给发布者本人,公费档退帮会资金给发单帮(只会有一项非 0)。
	IssuerRefundSilver     uint64
	IssuerRefundGuildFunds uint64
}

// ResolveBounty 按追杀令行与本局事实给出去向(r2-gw#1 / r2-gw#2)。结算事务先按普通读调一次定计划、在锁序末尾锁住追杀令行
// 再调一次,两次结果不同就说明中间被别的结算或到期收尾改过,整笔重算。
//
// 只有「追杀局 + 同区 + 有接单资格 + ACTIVE 未过期 + 防守方就是目标」才动追杀令:
//   - 猎手胜:销单,托管全给猎手;
//   - 目标胜且这名追兵与上一名反杀的不是同一人:反杀数 +1,到撕单数则撕单 —— 托管的一半(向下取整)给目标,
//     其余退发布者:银两档退扣税后托管的另一半银两;公费档的赏金池是系统配给的、不是谁出的,退的是发单帮实付资金的一半
//     (按实付比例退,与销单时「资金换赏金」的比例一致,撕单不会让发单帮倒赚)。
func ResolveBounty(b *pb.GuildWarBountyRecord, f BountyFacts) BountyResolution {
	if f.WarBattleKind != pb.GuildWarBattleKind_GUILD_WAR_BATTLE_KIND_BOUNTY_HUNT || f.HunterID == 0 ||
		!BountyHuntable(b, f.ZoneID, f.HunterGuildID, f.NowMs) || b.GetTargetPlayerId() != f.TargetID {
		return BountyResolution{}
	}
	res := BountyResolution{
		Touch:                 true,
		Status:                b.GetStatus(),
		CounterKills:          b.GetCounterKills(),
		LastCounterAttackerID: b.GetLastCounterAttackerId(),
		HunterPlayerID:        b.GetHunterPlayerId(),
		ResolvedAtMs:          b.GetResolvedAtMs(),
	}
	switch {
	case f.HunterWon:
		res.Status = uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED)
		res.HunterPlayerID = f.HunterID
		res.ResolvedAtMs = f.NowMs
		res.OutcomeTag = BountyTagHunted
		res.RewardRecipientID = f.HunterID
		res.RewardSilver = b.GetEscrowSilver()
		return res
	case f.TargetWon && f.HunterID != b.GetLastCounterAttackerId():
		res.CounterKills++
		res.LastCounterAttackerID = f.HunterID
		if res.CounterKills < f.CounterKillTear {
			res.OutcomeTag = BountyTagCounterKill
			return res
		}
		res.Status = uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_TORN_BY_TARGET)
		res.ResolvedAtMs = f.NowMs
		res.OutcomeTag = BountyTagTorn
		res.RewardRecipientID = b.GetTargetPlayerId()
		res.RewardSilver = b.GetEscrowSilver() / 2
		if b.GetTier() == uint32(pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC) {
			res.IssuerRefundGuildFunds = b.GetEscrowGuildFunds() - b.GetEscrowGuildFunds()/2
		} else {
			res.IssuerRefundSilver = b.GetEscrowSilver() - res.RewardSilver
		}
		return res
	}
	return BountyResolution{}
}

// BountyExpiryRefund 是一张 ACTIVE 追杀令到期时原路退回的本金(r2-gw#2):银两档退扣税后的托管银两给发布者本人
// (10% 税发单时已销毁,不退);公费档退发单时预扣的帮会资金给发单帮(系统配给的赏金池不是谁出的,不退)。
func BountyExpiryRefund(b *pb.GuildWarBountyRecord) (silver, guildFunds uint64) {
	if b.GetTier() == uint32(pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC) {
		return 0, b.GetEscrowGuildFunds()
	}
	return b.GetEscrowSilver(), 0
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
// …Before 与 AttackerOwnsOther 是算它时用的那份事务外快照,结算事务在行锁内逐项比对(BasisHolds,r2-gw#3)。
type BannerDelta struct {
	HPBefore          uint32
	OwnerBefore       uint64
	ShieldUntilBefore uint64
	AttackerOwnsOther bool
	HPAfter           uint32
	OwnerAfter        uint64
	ShieldUntilAfter  uint64
	BrokeBanner       bool
	CapturedByWinner  bool
	BreakBonusFunds   uint64
	BreakBonusSilver  uint64
}

// BasisHolds 报告结算事务里锁住的本区灵脉行是否仍是算出 d 的那份快照(r2-gw#3):目标灵脉的主人、旗势、护盾到期都没变,
// 攻方「是否已占着别的灵脉」也没变。不成立说明另一场结算抢先提交了,调用方回冲突、整笔重读重算。
func (d BannerDelta) BasisHolds(cur *pb.GuildWarLeylineRecord, attackerOwnsOther bool) bool {
	return cur != nil && cur.GetOwnerGuildId() == d.OwnerBefore && cur.GetBannerHp() == d.HPBefore &&
		cur.GetShieldUntilMs() == d.ShieldUntilBefore && attackerOwnsOther == d.AttackerOwnsOther
}

// FindLeylineBasis 从本区灵脉行里取出目标灵脉,并判攻方帮是否已占着别的灵脉。结算的事务外快照与事务内行锁共用这一处,
// 两边对「每帮限占 1 座」的判定因此同源。
func FindLeylineBasis(rows []*pb.GuildWarLeylineRecord, leylineID uint32, attackerGuildID uint64) (cur *pb.GuildWarLeylineRecord, attackerOwnsOther bool) {
	for _, lr := range rows {
		if lr.GetLeylineId() == leylineID {
			cur = lr
		} else if attackerGuildID != 0 && lr.GetOwnerGuildId() == attackerGuildID {
			attackerOwnsOther = true
		}
	}
	return cur, attackerOwnsOther
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
		HPBefore:          currentHP,
		OwnerBefore:       currentOwner,
		ShieldUntilBefore: currentShieldUntil,
		AttackerOwnsOther: attackerAlreadyOwnsOther,
		HPAfter:           currentHP,
		OwnerAfter:        currentOwner,
		ShieldUntilAfter:  currentShieldUntil,
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
