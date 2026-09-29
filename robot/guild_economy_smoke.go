package main

// guild-smoke 的经济段(帮会二期 B5c,设计 docs/design/guild-phase2/05-economy.md §5.40):
// 帮主 A(robot_9214)与成员 B(robot_9215)都在 zone_a,走一遍 捐献 → 升级 → 兑换,
// 断言帮会资金、个人帮贡、每日次数 / 限购,以及 scene 上的银两 / 灵石与背包物品。
// 由 RunGuildSmoke 在 GUILD_SMOKE_OK 之后调用(guild_smoke.economy=true),账号与管理段错开(90-consistency Y-09)。
//
// 两种模式。计数器按玩家、按游戏日(UTC+8 05:00 切日)记,解散帮会清不掉,所以同一游戏日只能完整跑一次:
//
//	full     —— A 今天没捐过大捐 / 灵石、没兑过 101 / 301:GM 发钱 → 捐献三次 → 升级 → 兑换,全量断言数值;
//	degraded —— 同一游戏日重跑(或 full 中途失败后重跑):只跑不依赖今日次数的拒绝类断言。
//
// 共同步骤:
//
//	1. A、B 撤掉遗留申请、离开任何帮会;A 建帮(Lv.1、资金 0、上限 30),B 申请、A 审批通过;
//	2. A 读捐献页(3 个选项、没有结算中)与商店(11 件),按今日用量判模式,打印 economy mode=…;
//	3. 读 B 的背包,灵石 > 0 就 GM 扣到 0(第 8 步要 B 余额不足)。
//
// full:
//
//	4. 记下 A 背包初值;GM 给 A 加 300,000 银两、100 灵石;
//	5. A 大捐 → 资金 12,000,A 的累计 / 可用帮贡都是 120(结算中就轮询捐献页,至多 10 秒);
//	6. A 再大捐 → 资金 24,000;第三次 → kGuildDonateLimit,资金不变;
//	7. A 灵石捐 → 资金 44,000、可用帮贡 440;A 的银两 = 初值 + 300,000 − 200,000,灵石 = 初值 + 100 − 100;
//	8. B 灵石捐 → kGuildCurrencyInsufficient、视图 REJECTED;B 的捐献页里该项今日用量仍为 0,最近结果是货币不足;
//	9. B 升级 → kGuildRankTooLow 且回包带帮会;A 升级 → Lv.2、资金 24,000、上限 35;A 带旧等级再升一次 → 受理、仍是 Lv.2 / 24,000;
//	10. A 读商店 → 103 已解锁、104 未解锁、可用帮贡 440、202 单次最多 1 份;
//	11. A 兑换 101 → 可用帮贡 410,背包物品 15 数量 +5;
//	12. A 兑换 104 → kGuildShopLevelTooLow;B 兑换 101 → kGuildContributionInsufficient;A 一次兑 2 份 202 → kGuildShopLimit;
//	13. A 连兑 5 次 301(可用帮贡 160),第 6 次 → kGuildShopLimit;
//	14. 打印 GUILD_ECONOMY_SMOKE_OK mode=full …,A 解散。
//
// degraded:
//
//	4'. A 今日大捐已用满 2 次 → 再捐 → kGuildDonateLimit;否则打印 SKIP(捐了会给帮会加钱,打乱第 6' 步);
//	5'. 同 full 第 8 步;
//	6'. B 升级 → kGuildRankTooLow;A 升级 → kGuildFundsInsufficient,回包帮会资金为 0;
//	7'. A 读商店 → 可用帮贡 0;同 full 第 12 步三条;
//	8'. 打印 GUILD_ECONOMY_SMOKE_OK mode=degraded …,A 解散。
//
// 前置(在 guild_smoke.yaml 头注释的基础上):guild 的 AssetOp.Enabled=true 且 scene 能收资产指令
//(关着时经济写 RPC 一律回 kGuildAssetPending,本段在第 5 步失败并提示);scene 运行模式为 dev,
// GM 客户端消息放行(cpp/nodes/gate/SECURITY.md §3);robot_9214 / 9215 首次在 zone_a 建角。
// 可选手动项:第 5 步前强杀 guild 再重启,A 再查 → 捐献由重投循环完成、资金只加一次。

import (
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	guildpb "proto/guild"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

// 期望值来自默认配表 GuildDonate / GuildShop / GuildLevel。**故意写死**(理由同 guildSmokeLevel1MaxOfficers):
// 冒烟要同时盯住"配表被人改了"和"服务端没按配表算",跟着表走就只剩后者。配表真要改,改这里。
const (
	econDonateBigID         uint32 = 2      // 银两大捐
	econDonateBigCost       uint64 = 100000 // 银两
	econDonateBigGain       uint64 = 120    // 帮贡
	econDonateBigFunds      uint64 = 12000  // 帮会资金
	econDonateBigDailyLimit uint32 = 2
	econDonateSpiritID      uint32 = 3   // 灵石捐献
	econDonateSpiritCost    uint64 = 100 // 灵石
	econDonateSpiritGain    uint64 = 200
	econDonateSpiritFunds   uint64 = 20000
	econDonateOptionCount          = 3

	econLevel1UpgradeCost uint64 = 20000 // GuildLevel[1].upgrade_cost_funds
	econLevel1MaxMembers  uint32 = 30
	econLevel2MaxMembers  uint32 = 35

	econShopGoodsCount           = 11
	econGoodsPill         uint32 = 101 // 培元丹:物品 15 ×5,帮贡 30,每日 10 份
	econGoodsPillItem     uint32 = 15
	econGoodsPillCount    uint64 = 5
	econGoodsPillCost     uint64 = 30
	econGoodsLevel2       uint32 = 103 // 需帮会 Lv.2
	econGoodsLevel3       uint32 = 104 // 需帮会 Lv.3
	econGoodsAmulet       uint32 = 202 // 堆叠 1 的物品:单次最多 1 份
	econGoodsLantern      uint32 = 301 // 花灯:帮贡 50,每日 5 份
	econGoodsLanternCost  uint64 = 50
	econGoodsLanternLimit        = 5

	econGmSilver uint64 = 300000
	econGmSpirit uint64 = 100
)

// 货币槽位与 cpp/libs/modules/currency/constants/currency.h 同序(契约 §0-1:银两 = kCurrencyGold,灵石 = kCurrencyDiamond)。
// 与 currencyTypeGold 同一理由写死:proto 侧只有 uint32,没有生成的 Go 枚举。
const (
	econCurrencySilver uint32 = 0
	econCurrencySpirit uint32 = 1
)

// 结算中的单最多等多久。同步投递预算 2.5s,之后由重投循环按退避接手(retry_base 1s),10s 足够本机两三轮。
const econSettleTimeout = 10 * time.Second
const econSettlePoll = 500 * time.Millisecond

var (
	tipGuildDonateLimit              = uint32(tiptable.GuildError_kGuildDonateLimit)
	tipGuildCurrencyInsufficient     = uint32(tiptable.GuildError_kGuildCurrencyInsufficient)
	tipGuildFundsInsufficient        = uint32(tiptable.GuildError_kGuildFundsInsufficient)
	tipGuildShopLevelTooLow          = uint32(tiptable.GuildError_kGuildShopLevelTooLow)
	tipGuildShopLimit                = uint32(tiptable.GuildError_kGuildShopLimit)
	tipGuildContributionInsufficient = uint32(tiptable.GuildError_kGuildContributionInsufficient)
	tipGuildAssetPending             = uint32(tiptable.GuildError_kGuildAssetPending)
	reasonAssetCurrencyInsufficient  = uint32(tiptable.AssetError_kAssetCurrencyInsufficient)
)

// runGuildEconomySmoke 任一步失败直接以退出码 1 结束进程(与 RunGuildSmoke 同一约定)。
func runGuildEconomySmoke(cfg *config.Config, stats *metrics.Stats) {
	sc := cfg.GuildSmoke
	var bots []*guildSmokeBot
	cleanup := func() {
		for _, bot := range bots {
			_ = leaveGame(bot.gc, stats)
			sendDisconnectBestEffort(bot.gc)
			gameobject.PlayerList.Delete(bot.gc.PlayerId)
			bot.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("GUILD_SMOKE_FAIL step=economy-%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 登录(两人顺序登录即可,不值得为省几秒写并发) ----
	for _, account := range []string{sc.EconomyLeader, sc.EconomyMember} {
		zoneCfg := *cfg
		zoneCfg.ZoneID = sc.ZoneA
		bot, err := guildSmokeLogin(&zoneCfg, account, stats)
		if err != nil {
			fail("login", "account=%s zone=%d err=%v", account, sc.ZoneA, err)
		}
		bots = append(bots, bot)
	}
	a, b := bots[0], bots[1]

	// ---- 步骤 1:干净的起点 + 建帮 + B 入帮 ----
	for _, bot := range bots {
		if err := bot.cancelAllApplications(); err != nil {
			fail("1-prepare", "account=%s: %v", bot.account, err)
		}
		if err := bot.leaveAnyGuild(); err != nil {
			fail("1-prepare", "account=%s: %v", bot.account, err)
		}
	}
	created := &guildpb.CreateGuildResponse{}
	if err := a.expect(game.GuildServiceCreateGuildMessageId,
		&guildpb.CreateGuildRequest{Name: "经济测" + guildSmokeNonce()}, created, 0); err != nil {
		fail("1-create", "%v", err)
	}
	guild := created.GetGuild()
	guildID := guild.GetGuildId()
	if guildID == 0 || guild.GetLevel() != 1 || guild.GetFunds() != 0 || guild.GetMaxMembers() != econLevel1MaxMembers {
		fail("1-create", "新帮会 id=%d level=%d funds=%d max_members=%d,期望 Lv.1 / 0 / %d",
			guildID, guild.GetLevel(), guild.GetFunds(), guild.GetMaxMembers(), econLevel1MaxMembers)
	}
	if err := b.expect(game.GuildServiceApplyJoinGuildMessageId,
		&guildpb.ApplyJoinGuildRequest{GuildId: guildID}, &guildpb.ApplyJoinGuildResponse{}, 0); err != nil {
		fail("1-apply", "%v", err)
	}
	if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
		&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: b.gc.PlayerId, Approve: true},
		&guildpb.ReviewGuildApplicationResponse{}, 0); err != nil {
		fail("1-review", "%v", err)
	}
	if _, ok := guildSmokeRoleOf(mustMyGuild(b, fail, "1-joined"), b.gc.PlayerId); !ok {
		fail("1-joined", "审批通过后 B(%d) 不在帮会 %d 的成员表里", b.gc.PlayerId, guildID)
	}

	// ---- 步骤 2:读两页,判模式 ----
	options := econDonateOptions(a, fail, "2-options")
	if len(options.GetOptions()) != econDonateOptionCount || len(options.GetPendingDonations()) != 0 {
		fail("2-options", "捐献选项 %d 个、结算中 %d 笔,期望 %d 个、0 笔(新帮会不该有结算中的单)",
			len(options.GetOptions()), len(options.GetPendingDonations()), econDonateOptionCount)
	}
	shop := econShop(a, fail, "2-shop")
	if len(shop.GetGoods()) != econShopGoodsCount {
		fail("2-shop", "商品 %d 件,期望 %d 件(GuildShop 默认行)", len(shop.GetGoods()), econShopGoodsCount)
	}
	bigUsed := econOption(options, econDonateBigID).GetUsedToday()
	spiritUsed := econOption(options, econDonateSpiritID).GetUsedToday()
	pillUsed := econGoods(shop, econGoodsPill).GetUsedCount()
	lanternUsed := econGoods(shop, econGoodsLantern).GetUsedCount()
	mode := "degraded"
	if bigUsed == 0 && spiritUsed == 0 && pillUsed == 0 && lanternUsed == 0 {
		mode = "full"
	}
	zap.L().Info("[guild-smoke] economy mode="+mode,
		zap.Uint64("guild_id", guildID), zap.Uint32("big_used", bigUsed), zap.Uint32("spirit_used", spiritUsed),
		zap.Uint32("pill_used", pillUsed), zap.Uint32("lantern_used", lanternUsed))

	// ---- 步骤 3:B 的灵石清零(第 8 步要 B 余额不足) ----
	bBag := econBag(b, fail, "3-bag")
	if spirit := econCurrency(bBag, econCurrencySpirit); spirit > 0 {
		econGmDeduct(b, fail, "3-deduct", econCurrencySpirit, spirit)
	}

	if mode == "full" {
		runGuildEconomyFull(a, b, guildID, fail)
	} else {
		runGuildEconomyDegraded(a, b, guildID, bigUsed, fail)
	}

	if err := a.expect(game.GuildServiceDisbandGuildMessageId, &guildpb.DisbandGuildRequest{}, &guildpb.DisbandGuildResponse{}, 0); err != nil {
		fail("disband", "%v", err)
	}
	cleanup()
	_ = zap.L().Sync()
}

type econFail func(step, format string, args ...any)

func runGuildEconomyFull(a, b *guildSmokeBot, guildID uint64, fail econFail) {
	// ---- 步骤 4:GM 发钱 ----
	before := econBag(a, fail, "4-bag")
	silver0, spirit0 := econCurrency(before, econCurrencySilver), econCurrency(before, econCurrencySpirit)
	econGmAdd(a, fail, "4-gm-silver", econCurrencySilver, econGmSilver)
	econGmAdd(a, fail, "4-gm-spirit", econCurrencySpirit, econGmSpirit)

	// ---- 步骤 5 / 6:大捐两次,第三次撞每日上限 ----
	econDonateApplied(a, econDonateBigID, fail, "5-donate")
	econCheckGuild(a, fail, "5-funds", 1, econDonateBigFunds, econDonateBigGain, econDonateBigGain)
	econDonateApplied(a, econDonateBigID, fail, "6-donate")
	econCheckGuild(a, fail, "6-funds", 1, 2*econDonateBigFunds, 2*econDonateBigGain, 2*econDonateBigGain)
	if tip, _ := econDonate(a, econDonateBigID, fail, "6-limit"); tip != tipGuildDonateLimit {
		fail("6-limit", "第 %d 次大捐 tip=%d,期望 kGuildDonateLimit(%d)", econDonateBigDailyLimit+1, tip, tipGuildDonateLimit)
	}
	econCheckGuild(a, fail, "6-limit-funds", 1, 2*econDonateBigFunds, 2*econDonateBigGain, 2*econDonateBigGain)

	// ---- 步骤 7:灵石捐,核对货币 ----
	econDonateApplied(a, econDonateSpiritID, fail, "7-donate")
	funds := 2*econDonateBigFunds + econDonateSpiritFunds // 44,000
	balance := 2*econDonateBigGain + econDonateSpiritGain // 440
	econCheckGuild(a, fail, "7-funds", 1, funds, balance, balance)
	after := econBag(a, fail, "7-bag")
	wantSilver := silver0 + econGmSilver - 2*econDonateBigCost
	wantSpirit := spirit0 + econGmSpirit - econDonateSpiritCost
	if got := econCurrency(after, econCurrencySilver); got != wantSilver {
		fail("7-silver", "A 的银两=%d,期望 初值 %d + %d − %d = %d", got, silver0, econGmSilver, 2*econDonateBigCost, wantSilver)
	}
	if got := econCurrency(after, econCurrencySpirit); got != wantSpirit {
		fail("7-spirit", "A 的灵石=%d,期望 初值 %d + %d − %d = %d", got, spirit0, econGmSpirit, econDonateSpiritCost, wantSpirit)
	}

	// ---- 步骤 8:B 余额不足 ----
	econMemberCannotAfford(b, fail, "8")

	// ---- 步骤 9:升级 ----
	econUpgradeRankTooLow(b, fail, "9-member")
	upgraded := &guildpb.UpgradeGuildResponse{}
	if err := a.expect(game.GuildServiceUpgradeGuildMessageId, &guildpb.UpgradeGuildRequest{ExpectedLevel: 1}, upgraded, 0); err != nil {
		fail("9-upgrade", "%v", err)
	}
	funds -= econLevel1UpgradeCost // 24,000
	if g := upgraded.GetGuild(); g.GetLevel() != 2 || g.GetFunds() != funds || g.GetMaxMembers() != econLevel2MaxMembers {
		fail("9-upgrade", "升级后 level=%d funds=%d max_members=%d,期望 2 / %d / %d",
			g.GetLevel(), g.GetFunds(), g.GetMaxMembers(), funds, econLevel2MaxMembers)
	}
	// 带着旧等级再点一次:服务端认作"已被升过",受理但不再扣钱(重复点击不会连升两级)。
	again := &guildpb.UpgradeGuildResponse{}
	if err := a.expect(game.GuildServiceUpgradeGuildMessageId, &guildpb.UpgradeGuildRequest{ExpectedLevel: 1}, again, 0); err != nil {
		fail("9-upgrade-again", "%v", err)
	}
	if g := again.GetGuild(); g.GetLevel() != 2 || g.GetFunds() != funds {
		fail("9-upgrade-again", "重复升级后 level=%d funds=%d,期望仍是 2 / %d", g.GetLevel(), g.GetFunds(), funds)
	}

	// ---- 步骤 10:商店随等级解锁 ----
	shop := econShop(a, fail, "10-shop")
	if !econGoods(shop, econGoodsLevel2).GetUnlocked() || econGoods(shop, econGoodsLevel3).GetUnlocked() {
		fail("10-shop", "Lv.2 帮会:%d unlocked=%v、%d unlocked=%v,期望 true / false", econGoodsLevel2,
			econGoods(shop, econGoodsLevel2).GetUnlocked(), econGoodsLevel3, econGoods(shop, econGoodsLevel3).GetUnlocked())
	}
	if shop.GetContributionBalance() != balance {
		fail("10-shop", "商店可用帮贡=%d,期望 %d", shop.GetContributionBalance(), balance)
	}
	if got := econGoods(shop, econGoodsAmulet).GetMaxBuyCount(); got != 1 {
		fail("10-shop", "商品 %d(堆叠 1 的物品)max_buy_count=%d,期望 1", econGoodsAmulet, got)
	}

	// ---- 步骤 11:兑换一份,背包到货 ----
	pill0 := econItemCount(econBag(a, fail, "11-bag"), econGoodsPillItem)
	balance -= econGoodsPillCost // 410
	econBuyApplied(a, econGoodsPill, balance, fail, "11-buy")
	if got := econItemCount(econBag(a, fail, "11-bag-after"), econGoodsPillItem); got != pill0+econGoodsPillCount {
		fail("11-item", "物品 %d 数量=%d,期望 %d + %d", econGoodsPillItem, got, pill0, econGoodsPillCount)
	}

	// ---- 步骤 12:三种拒绝 ----
	econShopRejections(a, b, fail, "12")

	// ---- 步骤 13:限购 ----
	for i := 1; i <= econGoodsLanternLimit; i++ {
		// 同一消息号连发 6 次:每次之间退火,别让 gate 的写类限流(5 次 / 秒)把回包吞掉。
		time.Sleep(guildSmokeSameIdCooldown)
		balance -= econGoodsLanternCost
		econBuyApplied(a, econGoodsLantern, balance, fail, fmt.Sprintf("13-buy-%d", i))
	}
	time.Sleep(guildSmokeSameIdCooldown)
	if tip, _ := econBuy(a, econGoodsLantern, 1, fail, "13-limit"); tip != tipGuildShopLimit {
		fail("13-limit", "第 %d 份花灯 tip=%d,期望 kGuildShopLimit(%d)", econGoodsLanternLimit+1, tip, tipGuildShopLimit)
	}

	zap.L().Info(fmt.Sprintf("GUILD_ECONOMY_SMOKE_OK mode=full guild_id=%d funds=%d level=2 balance=%d", guildID, funds, balance))
}

func runGuildEconomyDegraded(a, b *guildSmokeBot, guildID uint64, bigUsed uint32, fail econFail) {
	// ---- 步骤 4':只有今日大捐已用满时才断言上限;否则一捐就会给帮会加钱,打乱第 6' 步 ----
	if bigUsed == econDonateBigDailyLimit {
		if tip, _ := econDonate(a, econDonateBigID, fail, "4'-limit"); tip != tipGuildDonateLimit {
			fail("4'-limit", "今日大捐已用 %d 次,再捐 tip=%d,期望 kGuildDonateLimit(%d)", bigUsed, tip, tipGuildDonateLimit)
		}
	} else {
		zap.L().Info(fmt.Sprintf("[guild-smoke] SKIP step4' used_today=%d", bigUsed))
	}

	// ---- 步骤 5':B 余额不足(与 full 第 8 步相同,可重复) ----
	econMemberCannotAfford(b, fail, "5'")

	// ---- 步骤 6':升级的两种拒绝 ----
	econUpgradeRankTooLow(b, fail, "6'-member")
	poor := &guildpb.UpgradeGuildResponse{}
	if err := a.expect(game.GuildServiceUpgradeGuildMessageId, &guildpb.UpgradeGuildRequest{ExpectedLevel: 1}, poor, tipGuildFundsInsufficient); err != nil {
		fail("6'-upgrade", "%v", err)
	}
	if g := poor.GetGuild(); g == nil || g.GetFunds() != 0 {
		fail("6'-upgrade", "资金不足的回包应带帮会且资金为 0,实得 guild=%v funds=%d", g != nil, g.GetFunds())
	}

	// ---- 步骤 7':商店 ----
	if shop := econShop(a, fail, "7'-shop"); shop.GetContributionBalance() != 0 {
		fail("7'-shop", "新帮会里 A 的可用帮贡=%d,期望 0", shop.GetContributionBalance())
	}
	econShopRejections(a, b, fail, "7'")

	zap.L().Info(fmt.Sprintf("GUILD_ECONOMY_SMOKE_OK mode=degraded guild_id=%d", guildID))
}

// econMemberCannotAfford:B 灵石为 0 时捐灵石 → 货币不足且视图 REJECTED;次数退回、最近结果记下原因。
func econMemberCannotAfford(b *guildSmokeBot, fail econFail, step string) {
	tip, donation := econDonate(b, econDonateSpiritID, fail, step+"-donate")
	switch {
	case tip == tipGuildCurrencyInsufficient && donation.GetStatus() == guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED:
	case tip == 0 && donation.GetStatus() == guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING:
		// scene 已判拒但还没落盘(未 durable):视图是结算中,等它落定。
		settled := econAwaitDonation(b, donation.GetOpId(), fail, step+"-settle")
		if settled.GetStatus() != guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED {
			fail(step+"-donate", "B 灵石捐献最终状态=%s,期望 REJECTED", settled.GetStatus())
		}
	default:
		fail(step+"-donate", "B 灵石捐献 tip=%d status=%s,期望 kGuildCurrencyInsufficient(%d) + REJECTED",
			tip, donation.GetStatus(), tipGuildCurrencyInsufficient)
	}
	options := econDonateOptions(b, fail, step+"-options")
	if used := econOption(options, econDonateSpiritID).GetUsedToday(); used != 0 {
		fail(step+"-options", "被拒之后 B 的灵石捐献今日用量=%d,期望 0(REJECTED 要退回次数)", used)
	}
	recent := options.GetRecentResults()
	if len(recent) == 0 || recent[0].GetStatus() != guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED ||
		recent[0].GetReasonTipId() != reasonAssetCurrencyInsufficient {
		fail(step+"-options", "B 的最近结果=%v,期望第一条 REJECTED、原因 kAssetCurrencyInsufficient(%d)", recent, reasonAssetCurrencyInsufficient)
	}
}

func econUpgradeRankTooLow(b *guildSmokeBot, fail econFail, step string) {
	resp := &guildpb.UpgradeGuildResponse{}
	if err := b.expect(game.GuildServiceUpgradeGuildMessageId, &guildpb.UpgradeGuildRequest{ExpectedLevel: 1}, resp, tipGuildRankTooLow); err != nil {
		fail(step, "%v", err)
	}
	// 业务拒绝也带最新快照(05 §5.26):客户端据此刷新资金显示。
	if resp.GetGuild().GetGuildId() == 0 {
		fail(step, "成员升级被拒的回包没有带帮会快照")
	}
}

// econShopRejections:等级不够 / 帮贡不够 / 超过单次份数,三种都在发号之前拒绝,不会留下指令行。
func econShopRejections(a, b *guildSmokeBot, fail econFail, step string) {
	if tip, _ := econBuy(a, econGoodsLevel3, 1, fail, step+"-level"); tip != tipGuildShopLevelTooLow {
		fail(step+"-level", "兑换 %d tip=%d,期望 kGuildShopLevelTooLow(%d)", econGoodsLevel3, tip, tipGuildShopLevelTooLow)
	}
	if tip, _ := econBuy(b, econGoodsPill, 1, fail, step+"-contribution"); tip != tipGuildContributionInsufficient {
		fail(step+"-contribution", "B 兑换 %d tip=%d,期望 kGuildContributionInsufficient(%d)", econGoodsPill, tip, tipGuildContributionInsufficient)
	}
	if tip, _ := econBuy(a, econGoodsAmulet, 2, fail, step+"-count"); tip != tipGuildShopLimit {
		fail(step+"-count", "一次兑 2 份 %d tip=%d,期望 kGuildShopLimit(%d)", econGoodsAmulet, tip, tipGuildShopLimit)
	}
}

// ---------------------------------------------------------------------------
// 请求助手
// ---------------------------------------------------------------------------

func econDonate(bot *guildSmokeBot, donateID uint32, fail econFail, step string) (uint32, *guildpb.GuildDonationView) {
	resp := &guildpb.DonateToGuildResponse{}
	tip, err := bot.rpc(game.GuildServiceDonateToGuildMessageId, &guildpb.DonateToGuildRequest{DonateId: donateID}, resp)
	if err != nil {
		fail(step, "%v", err)
	}
	return tip, resp.GetDonation()
}

// econDonateApplied 捐一次并等到入账(结算中就轮询捐献页)。
func econDonateApplied(bot *guildSmokeBot, donateID uint32, fail econFail, step string) {
	tip, donation := econDonate(bot, donateID, fail, step)
	if tip != 0 {
		fail(step, "捐献 %d tip=%d(%s)", donateID, tip, econTipHint(tip))
	}
	switch donation.GetStatus() {
	case guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED:
		return
	case guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING:
		if settled := econAwaitDonation(bot, donation.GetOpId(), fail, step+"-settle"); settled.GetStatus() != guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED {
			fail(step, "捐献 %d(op %d)最终状态=%s reason=%d,期望 APPLIED", donateID, donation.GetOpId(), settled.GetStatus(), settled.GetReasonTipId())
		}
	default:
		fail(step, "捐献 %d 视图状态=%s reason=%d,期望 APPLIED 或结算中", donateID, donation.GetStatus(), donation.GetReasonTipId())
	}
}

// econAwaitDonation 轮询捐献页,直到这笔不在结算中、并出现在最近结果里。
func econAwaitDonation(bot *guildSmokeBot, opID uint64, fail econFail, step string) *guildpb.GuildDonationView {
	deadline := time.Now().Add(econSettleTimeout)
	for {
		options := econDonateOptions(bot, fail, step)
		stillPending := false
		for _, pending := range options.GetPendingDonations() {
			stillPending = stillPending || pending.GetOpId() == opID
		}
		if !stillPending {
			for _, result := range options.GetRecentResults() {
				if result.GetOpId() == opID {
					return result
				}
			}
			fail(step, "op %d 已不在结算中,但最近结果里也没有它", opID)
		}
		if !time.Now().Before(deadline) {
			fail(step, "op %d 结算超时(%s)。看 guild 日志 [GuildEconomy] 与 scene 日志 [AssetOp] 的该 op", opID, econSettleTimeout)
		}
		time.Sleep(econSettlePoll)
	}
}

func econBuy(bot *guildSmokeBot, goodsID, count uint32, fail econFail, step string) (uint32, *guildpb.BuyGuildShopGoodsResponse) {
	resp := &guildpb.BuyGuildShopGoodsResponse{}
	tip, err := bot.rpc(game.GuildServiceBuyGuildShopGoodsMessageId, &guildpb.BuyGuildShopGoodsRequest{GoodsId: goodsID, Count: count}, resp)
	if err != nil {
		fail(step, "%v", err)
	}
	return tip, resp
}

// econBuyApplied 兑换一份并等到发放,再核对提交后的可用帮贡。
func econBuyApplied(bot *guildSmokeBot, goodsID uint32, wantBalance uint64, fail econFail, step string) {
	tip, resp := econBuy(bot, goodsID, 1, fail, step)
	if tip != 0 {
		fail(step, "兑换 %d tip=%d(%s)", goodsID, tip, econTipHint(tip))
	}
	if resp.GetContributionBalance() != wantBalance {
		fail(step, "兑换 %d 后可用帮贡=%d,期望 %d", goodsID, resp.GetContributionBalance(), wantBalance)
	}
	order := resp.GetOrder()
	switch order.GetStatus() {
	case guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED:
		return
	case guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING:
		deadline := time.Now().Add(econSettleTimeout)
		for {
			time.Sleep(econSettlePoll)
			shop := econShop(bot, fail, step+"-settle")
			pending := false
			for _, p := range shop.GetPendingOrders() {
				pending = pending || p.GetOpId() == order.GetOpId()
			}
			if !pending {
				for _, recent := range shop.GetRecentOrders() {
					if recent.GetOpId() == order.GetOpId() && recent.GetStatus() == guildpb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED {
						return
					}
				}
				fail(step, "兑换 %d(op %d)已结算但不是 APPLIED:%v", goodsID, order.GetOpId(), shop.GetRecentOrders())
			}
			if !time.Now().Before(deadline) {
				fail(step, "兑换 %d(op %d)待发放超时(%s),最近原因 %d(27001 = 背包满)", goodsID, order.GetOpId(), econSettleTimeout, order.GetReasonTipId())
			}
		}
	default:
		fail(step, "兑换 %d 视图状态=%s reason=%d,期望 APPLIED 或待发放", goodsID, order.GetStatus(), order.GetReasonTipId())
	}
}

func econDonateOptions(bot *guildSmokeBot, fail econFail, step string) *guildpb.GetGuildDonateOptionsResponse {
	resp := &guildpb.GetGuildDonateOptionsResponse{}
	if err := bot.expect(game.GuildServiceGetGuildDonateOptionsMessageId, &guildpb.GetGuildDonateOptionsRequest{}, resp, 0); err != nil {
		fail(step, "%v", err)
	}
	return resp
}

func econShop(bot *guildSmokeBot, fail econFail, step string) *guildpb.GetGuildShopResponse {
	resp := &guildpb.GetGuildShopResponse{}
	if err := bot.expect(game.GuildServiceGetGuildShopMessageId, &guildpb.GetGuildShopRequest{}, resp, 0); err != nil {
		fail(step, "%v", err)
	}
	return resp
}

// econCheckGuild 读自己的帮会,核对等级、资金与本人两列帮贡。
func econCheckGuild(bot *guildSmokeBot, fail econFail, step string, level uint32, funds, total, balance uint64) {
	g := mustMyGuild(bot, fail, step)
	var me *guildpb.GuildMember
	for _, member := range g.GetMembers() {
		if member.GetPlayerId() == bot.gc.PlayerId {
			me = member
		}
	}
	if g.GetLevel() != level || g.GetFunds() != funds || me.GetContributionTotal() != total || me.GetContributionBalance() != balance {
		fail(step, "level=%d funds=%d 本人帮贡 累计 %d / 可用 %d,期望 %d / %d / %d / %d",
			g.GetLevel(), g.GetFunds(), me.GetContributionTotal(), me.GetContributionBalance(), level, funds, total, balance)
	}
}

func mustMyGuild(bot *guildSmokeBot, fail econFail, step string) *guildpb.GuildInfo {
	g, err := bot.myGuild()
	if err != nil {
		fail(step, "%v", err)
	}
	return g
}

// econBag 读主背包(bag_type 0):物品与货币都在这一份快照里。
func econBag(bot *guildSmokeBot, fail econFail, step string) *scene.BagInfo {
	resp := &scene.GetBagResponse{}
	if err := bot.expect(game.SceneBagClientPlayerGetBagMessageId, &scene.GetBagRequest{BagType: 0}, resp, 0); err != nil {
		fail(step, "读背包: %v", err)
	}
	if resp.GetBag() == nil {
		fail(step, "背包回包为空")
	}
	return resp.GetBag()
}

func econCurrency(bag *scene.BagInfo, currencyType uint32) uint64 {
	values := bag.GetCurrency().GetValues()
	if int(currencyType) >= len(values) {
		return 0 // repeated 可以比货币种类短:没写到的槽位就是 0
	}
	return values[currencyType]
}

func econItemCount(bag *scene.BagInfo, configID uint32) uint64 {
	var total uint64
	for _, item := range bag.GetItems() {
		if item.GetConfigId() == configID {
			total += uint64(item.GetCount())
		}
	}
	return total
}

// econGmAdd / econGmDeduct 照 gmAddGold(currency_crash_window_scenario.go)发 GM 货币请求,只在 dev 运行模式下被放行
// (cpp/nodes/gate/SECURITY.md §3)。回包由 guildSmokeBot 按消息号认领,见 guildEconomyIsSceneMessage。
func econGmAdd(bot *guildSmokeBot, fail econFail, step string, currencyType uint32, amount uint64) {
	if err := bot.expect(game.SceneCurrencyClientPlayerGmAddCurrencyMessageId,
		&scene.GmAddCurrencyRequest{CurrencyType: currencyType, Amount: int64(amount)}, &scene.GmAddCurrencyResponse{}, 0); err != nil {
		fail(step, "GM 加货币(需 dev 运行模式放行 GM 消息): %v", err)
	}
}

func econGmDeduct(bot *guildSmokeBot, fail econFail, step string, currencyType uint32, amount uint64) {
	if err := bot.expect(game.SceneCurrencyClientPlayerGmDeductCurrencyMessageId,
		&scene.GmDeductCurrencyRequest{CurrencyType: currencyType, Amount: int64(amount)}, &scene.GmDeductCurrencyResponse{}, 0); err != nil {
		fail(step, "GM 扣货币(需 dev 运行模式放行 GM 消息): %v", err)
	}
}

func econOption(options *guildpb.GetGuildDonateOptionsResponse, donateID uint32) *guildpb.GuildDonateOptionView {
	for _, option := range options.GetOptions() {
		if option.GetDonateId() == donateID {
			return option
		}
	}
	return nil
}

func econGoods(shop *guildpb.GetGuildShopResponse, goodsID uint32) *guildpb.GuildShopGoodsView {
	for _, goods := range shop.GetGoods() {
		if goods.GetGoodsId() == goodsID {
			return goods
		}
	}
	return nil
}

// econTipHint 只给最常见的一种误配写人话:资产通道没开时,经济写 RPC 一律回 kGuildAssetPending。
func econTipHint(tip uint32) string {
	if tip == tipGuildAssetPending {
		return "kGuildAssetPending:资产通道未开(guild.yaml AssetOp.Enabled=false),或还有未结算的单"
	}
	return "见 data/tip/Tip.xlsx"
}

// guildEconomyIsSceneMessage:经济段自己发的 scene 请求(读背包、GM 加 / 扣货币)。它们的回包由
// guildSmokeBot 按消息号认领,不交给通用分发 —— 通用处理器只记最后一次余额,认领不了"这一次请求的回包"。
func guildEconomyIsSceneMessage(messageId uint32) bool {
	switch messageId {
	case game.SceneBagClientPlayerGetBagMessageId,
		game.SceneCurrencyClientPlayerGmAddCurrencyMessageId,
		game.SceneCurrencyClientPlayerGmDeductCurrencyMessageId:
		return true
	}
	return false
}
