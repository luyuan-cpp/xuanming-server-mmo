package main

// guild-smoke 的活动段(帮会二期 B6a-cli,设计 docs/design/guild-phase2/06-activities.md §6.44):
// 帮主 A(robot_9216)与成员 B / C / D(robot_9217–9219)都在 zone_a,走一遍 元宵灯会 → 中秋团圆,
// 断言活动视图、个人次数、帮会进度与阈值、帮会资金、个人帮贡,以及团圆物品奖励真的进了背包。
// 由 RunGuildSmoke 在经济段之后调用(guild_smoke.activities=true),账号与管理段、经济段错开(90-consistency Y-09)。
//
// 步骤(编号与设计 §6.44 的表一致;S0 是表上方的"前置"):
//
//	S0. 四人登录进场;各自撤掉遗留申请、离开任何帮会(帮主则解散);A 建帮,B / C / D 申请、A 审批;
//	    记下帮会资金与四人帮贡的基线;
//	S1. A 读活动页 → 3 条,type 依次 1 / 2 / 3;灯会、团圆 OPEN,历练 DISABLED(trial=true 时 OPEN);
//	    四人的灯会 / 团圆今日次数都是 0,否则判 same-game-day-rerun(见下);灯会 blocked_tip_id=0;
//	    卡片上的数值等于默认配表;
//	S2. A 点灯 → 今日 1 次、进度 1;A 的帮贡 = 基线 + 20;
//	S3. A 再点 → kGuildActivityAlreadyClaimed;
//	S4. B、C 各点一次 → C 的回包进度 3、已达阈值、资金已发;帮会资金 = 基线 + 500;
//	    A 在 5 秒内收到 ACTIVITY_CHANGED 推送(没收到只告警:推送是至多一次,不判失败);
//	S5. D 点灯 → 受理、进度 4;资金仍是 基线 + 500(本档期只发一次);
//	S6. A 领团圆(四人在线,达到阈值 3)→ 帮贡再 + 30;10 秒内活动页的待发放数归 0;背包里物品 1 多 4 个;
//	S7. D 下线,B 领团圆 → 受理,回包显示已锁存(达成一次之后不必再凑人);B 的物品同样到账;
//	S8. D 重新登录(仍在帮);A 再领 → kGuildActivityAlreadyClaimed,帮贡不变;
//	收尾:A 解散帮会(连带删掉本帮的活动进度行),打印 GUILD_SMOKE_ACTIVITIES_OK guild_id=…。
//
// S9–S15(同道历练的邀请与开战)属 B6b-cli,本文件尚未包含:trial=true 时跑完 S8 在 S9 明确失败,不会假装通过。
//
// 结果约定:全过打印 `GUILD_SMOKE_ACTIVITIES_OK guild_id=…`;任一步失败打印
// `GUILD_SMOKE_ACTIVITIES_FAIL step=Sx reason=…` 并以退出码 1 结束进程。
//
// 可重复性:个人次数按玩家、按游戏日(UTC+8 05:00 切日)记在 guild_daily_counter 里,解散帮会清不掉,
// 所以同一游戏日只能完整跑一次。重跑会在 S1 失败,reason 以 same-game-day-rerun 开头并带上整条清理 SQL
// (只删这四个账号当天的活动计数;帮会进度行由 S0 的解散删)。冒烟自己不碰数据库。
//
// 前置(在 guild_smoke.yaml 头注释的基础上):默认配表 GuildActivity / GuildRule(activity_join_min_hours=0);
// 团圆带物品奖励,所以 guild 的 AssetOp.Enabled=true(关着时领奖回 kGuildAssetPending,本段在 S6 失败并提示);
// 活动 5 个消息号的 MessageLimiter 行已回填(没回填时吃默认 3 次 / 窗口,本段的发送节奏仍在其内);
// 四个账号首次在 zone_a 建角。

import (
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	guildpb "proto/guild"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

// 期望值来自默认配表 GuildActivity / Reward。**故意写死**(理由同 guildSmokeLevel1MaxOfficers):
// 冒烟要同时盯住"配表被人改了"和"服务端没按配表算",跟着表走就只剩后者。配表真要改,改这里。
const (
	actLanternContribution uint64 = 20  // GuildActivity[1].personal_contribution
	actLanternFunds        uint64 = 500 // 本档期达阈值时发一次
	actLanternThreshold    uint32 = 3   // 本档期点灯人次
	actLanternDailyLimit   uint32 = 1

	actReunionContribution uint64 = 30 // GuildActivity[2].personal_contribution
	actReunionThreshold    uint32 = 3  // 同时在线人数
	actReunionDailyLimit   uint32 = 1
	actReunionItem         uint32 = 1 // Reward[1] 的两个槽都是物品 1 ×2,服务端按 item_id 合并成一项
	actReunionItemCount    uint32 = 4
)

// 四个机器人在 activity_accounts 里的位次。帮主固定在首位,与 yaml 注释一致。
const (
	actSlotA = iota // 帮主
	actSlotB
	actSlotC
	actSlotD
	actSlotCount
)

// 物品奖励最多等多久(设计 S6:10 秒内轮询到待发放数归 0)。同步投递预算 2.5s,之后由重投循环按退避接手。
const actRewardTimeout = 10 * time.Second

// 下线之后等多久再继续。scene 的登出存盘是异步的,过快重登会被判成"重连取代登出"
// (同 attributeSmokeSession.relogin);这段时间也让 player_locator 的会话真正变成离线。
const actLogoutSettle = 2 * time.Second

var (
	tipGuildActivityNotOpen             = uint32(tiptable.GuildError_kGuildActivityNotOpen)
	tipGuildActivityAlreadyClaimed      = uint32(tiptable.GuildError_kGuildActivityAlreadyClaimed)
	tipGuildActivityThresholdNotReached = uint32(tiptable.GuildError_kGuildActivityThresholdNotReached)
	tipGuildActivityJoinTooRecent       = uint32(tiptable.GuildError_kGuildActivityJoinTooRecent)
)

// runGuildActivitySmoke 任一步失败直接以退出码 1 结束进程(与 RunGuildSmoke 同一约定)。
func runGuildActivitySmoke(cfg *config.Config, stats *metrics.Stats) {
	sc := cfg.GuildSmoke
	// 按位次存而不是追加:S7 里 D 会下线、S8 再登录,同一个位次先置空再换成新会话,收尾时不会把已关闭的连接再关一次。
	var bots [actSlotCount]*guildSmokeBot
	cleanup := func() {
		for _, bot := range bots {
			if bot != nil {
				actLogout(bot)
			}
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("GUILD_SMOKE_ACTIVITIES_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	login := func(account string) (*guildSmokeBot, error) {
		zoneCfg := *cfg
		zoneCfg.ZoneID = sc.ZoneA
		return guildSmokeLogin(&zoneCfg, account, stats)
	}

	// config 的校验已保证恰好四个互不相同的账号;这里再挡一次,免得有人绕开校验直接调用时按下标越界。
	if len(sc.ActivityAccounts) != actSlotCount {
		fail("S0-config", "activity_accounts 有 %d 个,期望 %d 个(帮主 A、成员 B / C / D)", len(sc.ActivityAccounts), actSlotCount)
	}

	// ---- S0:登录(顺序登录即可,不值得为省几秒写并发) ----
	for slot, account := range sc.ActivityAccounts {
		bot, err := login(account)
		if err != nil {
			fail("S0-login", "account=%s zone=%d err=%v", account, sc.ZoneA, err)
		}
		bots[slot] = bot
	}
	a, b, c, d := bots[actSlotA], bots[actSlotB], bots[actSlotC], bots[actSlotD]

	// ---- S0:干净的起点 + 建帮 + 三人入帮 ----
	// 帮主排在首位先清:上一轮若停在半路,A 解散之后另外三人自然不在帮里,不必各退一次。
	for _, bot := range bots {
		if err := bot.cancelAllApplications(); err != nil {
			fail("S0-prepare", "account=%s: %v", bot.account, err)
		}
		if err := bot.leaveAnyGuild(); err != nil {
			fail("S0-prepare", "account=%s: %v", bot.account, err)
		}
	}
	created := &guildpb.CreateGuildResponse{}
	if err := a.expect(game.GuildServiceCreateGuildMessageId,
		&guildpb.CreateGuildRequest{Name: "活动测" + guildSmokeNonce()}, created, 0); err != nil {
		fail("S0-create", "%v", err)
	}
	guildID := created.GetGuild().GetGuildId()
	if guildID == 0 {
		fail("S0-create", "建帮受理但响应里没有帮会")
	}
	for _, member := range []*guildSmokeBot{b, c, d} {
		if err := member.expect(game.GuildServiceApplyJoinGuildMessageId,
			&guildpb.ApplyJoinGuildRequest{GuildId: guildID}, &guildpb.ApplyJoinGuildResponse{}, 0); err != nil {
			fail("S0-apply", "account=%s: %v", member.account, err)
		}
		if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
			&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: member.gc.PlayerId, Approve: true},
			&guildpb.ReviewGuildApplicationResponse{}, 0); err != nil {
			fail("S0-review", "审批 %s: %v", member.account, err)
		}
	}

	// ---- S0:基线。新帮会理应资金 0、帮贡 0,但断言一律写成"基线 + 增量",不把这个巧合当前提 ----
	baseline := mustMyGuild(a, fail, "S0-baseline")
	if len(baseline.GetMembers()) != actSlotCount {
		fail("S0-baseline", "帮会 %d 有 %d 名成员,期望 %d 名", guildID, len(baseline.GetMembers()), actSlotCount)
	}
	funds := baseline.GetFunds()
	var total, balance [actSlotCount]uint64
	for slot, bot := range bots {
		member := actMemberOf(baseline, bot.gc.PlayerId)
		if member == nil {
			fail("S0-baseline", "审批通过后 %s(%d) 不在帮会 %d 的成员表里", bot.account, bot.gc.PlayerId, guildID)
		}
		total[slot], balance[slot] = member.GetContributionTotal(), member.GetContributionBalance()
	}
	zap.L().Info("[guild-smoke] activities S0: guild ready",
		zap.Uint64("guild_id", guildID), zap.Uint64("funds", funds),
		zap.Uint64("a", a.gc.PlayerId), zap.Uint64("b", b.gc.PlayerId), zap.Uint64("c", c.gc.PlayerId), zap.Uint64("d", d.gc.PlayerId))

	// ---- S1:活动页 ----
	views := actViews(a, fail, "S1")
	if len(views) != 3 {
		fail("S1", "活动页有 %d 条,期望 3 条(灯会 / 团圆 / 历练;GuildActivity 默认行)", len(views))
	}
	for i, view := range views {
		if want := guildpb.GuildActivityType(i + 1); view.GetType() != want {
			fail("S1", "第 %d 条 type=%s,期望 %s(每种类型至多一条、按 type 升序)", i+1, view.GetType(), want)
		}
	}
	lantern, reunion, trial := views[0], views[1], views[2]
	// 先判重跑再判其余:同一游戏日重跑时灯会的 blocked_tip_id 会是"今日已领",先报那一条只会把人带偏。
	// 四个人都要看:次数挂在玩家身上,A 干净而 B 用过的话,要到 S4 才以一句"今日已领"失败。
	cleanupSQL := fmt.Sprintf("DELETE FROM mmorpg_guild.guild_daily_counter WHERE player_id IN (%d,%d,%d,%d) AND counter_kind=3 AND period_key=%d;",
		a.gc.PlayerId, b.gc.PlayerId, c.gc.PlayerId, d.gc.PlayerId, lantern.GetPeriodKey())
	for _, bot := range bots {
		mine := views
		if bot != a {
			mine = actViews(bot, fail, "S1")
		}
		for _, typ := range []guildpb.GuildActivityType{
			guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_LANTERN,
			guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_REUNION,
		} {
			if used := actOfType(mine, typ).GetMyUsedCount(); used != 0 {
				fail("S1", "same-game-day-rerun account=%s 的 %s 今日已用 %d 次(个人次数按游戏日记、解散清不掉)。重跑前执行:%s",
					bot.account, typ, used, cleanupSQL)
			}
		}
	}
	open := guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_OPEN
	wantTrial := guildpb.GuildActivityState_GUILD_ACTIVITY_STATE_DISABLED
	if sc.Trial {
		wantTrial = open
	}
	if lantern.GetState() != open || reunion.GetState() != open || trial.GetState() != wantTrial {
		fail("S1", "灯会=%s 团圆=%s 历练=%s,期望 %s / %s / %s(trial=%v)",
			lantern.GetState(), reunion.GetState(), trial.GetState(), open, open, wantTrial, sc.Trial)
	}
	if blocked := lantern.GetBlockedTipId(); blocked != 0 {
		fail("S1", "灯会 blocked_tip_id=%d,期望 0(%s)", blocked, actTipHint(blocked))
	}
	if lantern.GetPersonalContribution() != actLanternContribution || lantern.GetGuildFunds() != actLanternFunds ||
		lantern.GetGuildThreshold() != actLanternThreshold || lantern.GetDailyLimit() != actLanternDailyLimit {
		fail("S1", "灯会卡片 帮贡=%d 资金=%d 阈值=%d 每日=%d,期望 %d / %d / %d / %d(GuildActivity 默认行)",
			lantern.GetPersonalContribution(), lantern.GetGuildFunds(), lantern.GetGuildThreshold(), lantern.GetDailyLimit(),
			actLanternContribution, actLanternFunds, actLanternThreshold, actLanternDailyLimit)
	}
	if reunion.GetPersonalContribution() != actReunionContribution || reunion.GetGuildThreshold() != actReunionThreshold ||
		reunion.GetDailyLimit() != actReunionDailyLimit {
		fail("S1", "团圆卡片 帮贡=%d 阈值=%d 每日=%d,期望 %d / %d / %d(GuildActivity 默认行)",
			reunion.GetPersonalContribution(), reunion.GetGuildThreshold(), reunion.GetDailyLimit(),
			actReunionContribution, actReunionThreshold, actReunionDailyLimit)
	}
	if items := reunion.GetRewardItems(); len(items) != 1 || items[0].GetItemId() != actReunionItem || items[0].GetCount() != actReunionItemCount {
		fail("S1", "团圆奖励物品=%v,期望只有一项:物品 %d ×%d(Reward[1] 两个槽按 item_id 合并)", items, actReunionItem, actReunionItemCount)
	}
	// 团圆未锁存时 progress 是此刻的合格在线人数。这里只留底不断言:它在 S6 才是判据,届时不足会带着人数失败。
	zap.L().Info("[guild-smoke] activities S1: activity page ok",
		zap.Uint32("period_key", lantern.GetPeriodKey()), zap.Uint32("reunion_online", reunion.GetProgress()),
		zap.Uint32("reunion_blocked_tip", reunion.GetBlockedTipId()), zap.String("trial_state", trial.GetState().String()))

	// ---- S2:A 点灯 ----
	tip, view := actLight(a, lantern.GetActivityId(), fail, "S2")
	if tip != 0 {
		fail("S2", "A 点灯 tip=%d(%s)", tip, actTipHint(tip))
	}
	if view.GetMyUsedCount() != 1 || view.GetProgress() != 1 || view.GetThresholdReached() || view.GetFundsGranted() {
		fail("S2", "A 点灯后视图(有=%v) 今日 %d 次、进度 %d、达阈值=%v、资金已发=%v,期望 1 / 1 / false / false",
			view != nil, view.GetMyUsedCount(), view.GetProgress(), view.GetThresholdReached(), view.GetFundsGranted())
	}
	total[actSlotA] += actLanternContribution
	balance[actSlotA] += actLanternContribution
	actCheckSelf(a, fail, "S2", funds, total[actSlotA], balance[actSlotA])

	// ---- S3:每日一次 ----
	if tip, _ := actLight(a, lantern.GetActivityId(), fail, "S3"); tip != tipGuildActivityAlreadyClaimed {
		fail("S3", "A 第二次点灯 tip=%d,期望 kGuildActivityAlreadyClaimed(%d)", tip, tipGuildActivityAlreadyClaimed)
	}

	// ---- S4:B、C 点灯,第三盏达到阈值 ----
	tip, view = actLight(b, lantern.GetActivityId(), fail, "S4-b")
	if tip != 0 {
		fail("S4-b", "B 点灯 tip=%d(%s)", tip, actTipHint(tip))
	}
	if view.GetProgress() != 2 || view.GetThresholdReached() || view.GetFundsGranted() {
		fail("S4-b", "B 点灯后视图(有=%v) 进度 %d、达阈值=%v、资金已发=%v,期望 2 / false / false",
			view != nil, view.GetProgress(), view.GetThresholdReached(), view.GetFundsGranted())
	}
	total[actSlotB] += actLanternContribution
	balance[actSlotB] += actLanternContribution
	// 清空留底必须在 C 点灯之前:达到阈值的那次提交一完成服务端就推,晚清一步会把这条推送一起丢掉。
	a.clearPushes()
	tip, view = actLight(c, lantern.GetActivityId(), fail, "S4-c")
	if tip != 0 {
		fail("S4-c", "C 点灯 tip=%d(%s)", tip, actTipHint(tip))
	}
	if view.GetProgress() != actLanternThreshold || !view.GetThresholdReached() || !view.GetFundsGranted() {
		fail("S4-c", "C 点灯后视图(有=%v) 进度 %d、达阈值=%v、资金已发=%v,期望 %d / true / true",
			view != nil, view.GetProgress(), view.GetThresholdReached(), view.GetFundsGranted(), actLanternThreshold)
	}
	total[actSlotC] += actLanternContribution
	balance[actSlotC] += actLanternContribution
	funds += actLanternFunds
	// 资金从 A 的视角读:A 不是这次的操作者,读得到新值才说明提交后的缓存失效对全帮生效。
	actCheckSelf(a, fail, "S4-funds", funds, total[actSlotA], balance[actSlotA])
	actCheckSelf(c, fail, "S4-c", funds, total[actSlotC], balance[actSlotC])
	// 推送至多一次(guild → Kafka → gate),设计只要求告警:活动页打开时客户端本来就会自己拉。
	if err := a.waitPush(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_ACTIVITY_CHANGED, guildID, guildSmokePushTimeout); err != nil {
		zap.L().Warn("[guild-smoke] activities S4: A 没等到 ACTIVITY_CHANGED 推送(只告警,不判失败)", zap.Error(err))
	}

	// ---- S5:第四盏照常受理,资金不再发 ----
	tip, view = actLight(d, lantern.GetActivityId(), fail, "S5")
	if tip != 0 {
		fail("S5", "D 点灯 tip=%d(%s)", tip, actTipHint(tip))
	}
	if view.GetProgress() != actLanternThreshold+1 || !view.GetThresholdReached() || !view.GetFundsGranted() {
		fail("S5", "D 点灯后视图(有=%v) 进度 %d、达阈值=%v、资金已发=%v,期望 %d / true / true",
			view != nil, view.GetProgress(), view.GetThresholdReached(), view.GetFundsGranted(), actLanternThreshold+1)
	}
	total[actSlotD] += actLanternContribution
	balance[actSlotD] += actLanternContribution
	actCheckSelf(d, fail, "S5", funds, total[actSlotD], balance[actSlotD])

	// ---- S6:A 领团圆(四人在线) ----
	total[actSlotA] += actReunionContribution
	balance[actSlotA] += actReunionContribution
	actClaimDelivered(a, reunion.GetActivityId(), funds, total[actSlotA], balance[actSlotA], fail, "S6")

	// ---- S7:D 下线,B 照样能领(已锁存) ----
	actLogout(d)
	bots[actSlotD] = nil
	time.Sleep(actLogoutSettle)
	total[actSlotB] += actReunionContribution
	balance[actSlotB] += actReunionContribution
	actClaimDelivered(b, reunion.GetActivityId(), funds, total[actSlotB], balance[actSlotB], fail, "S7")

	// ---- S8:D 重登;A 每日一次 ----
	d, err := login(sc.ActivityAccounts[actSlotD])
	if err != nil {
		fail("S8-relogin", "account=%s zone=%d err=%v", sc.ActivityAccounts[actSlotD], sc.ZoneA, err)
	}
	bots[actSlotD] = d
	if g := mustMyGuild(d, fail, "S8-relogin"); g.GetGuildId() != guildID {
		fail("S8-relogin", "D 重登后所在帮会=%d,期望 %d", g.GetGuildId(), guildID)
	}
	if tip, _ := actClaim(a, reunion.GetActivityId(), fail, "S8"); tip != tipGuildActivityAlreadyClaimed {
		fail("S8", "A 第二次领团圆 tip=%d,期望 kGuildActivityAlreadyClaimed(%d)", tip, tipGuildActivityAlreadyClaimed)
	}
	actCheckSelf(a, fail, "S8", funds, total[actSlotA], balance[actSlotA])

	// ---- S9–S15:同道历练(B6b-cli)。fail-closed:要求跑却没有实现时,不能只跑前八步就报通过 ----
	if sc.Trial {
		fail("S9", "trial=true,但历练段 S9–S15 属 B6b-cli,本二进制尚未包含;B6b 落地前请保持 trial: false")
	}

	// ---- 收尾:解散。帮名与进度行不留给下一轮(下一轮的 S0 也会清,但别让一个空帮会一直挂在本区榜上) ----
	if err := a.expect(game.GuildServiceDisbandGuildMessageId, &guildpb.DisbandGuildRequest{}, &guildpb.DisbandGuildResponse{}, 0); err != nil {
		fail("disband", "%v", err)
	}
	zap.L().Info(fmt.Sprintf("GUILD_SMOKE_ACTIVITIES_OK guild_id=%d leader=%d funds=%d trial=%v", guildID, a.gc.PlayerId, funds, sc.Trial))
	cleanup()
	_ = zap.L().Sync()
}

// actClaimDelivered:领一次团圆并等物品到账。S6(A)与 S7(B)是同一套判据:
// 受理 → 回包显示今日 1 次且已锁存 → 帮贡到账、资金不变 → 待发放数在限时内归 0 → 背包里物品真的多了。
// 最后一条不能省:待发放数归 0 既可能是到账,也可能是被永久拒绝(封禁 / 非法包),只有数背包分得清。
func actClaimDelivered(bot *guildSmokeBot, activityID uint32, wantFunds, wantTotal, wantBalance uint64, fail econFail, step string) {
	before := econItemCount(econBag(bot, fail, step+"-bag"), actReunionItem)
	tip, resp := actClaim(bot, activityID, fail, step)
	if tip != 0 {
		fail(step, "%s 领团圆 tip=%d parameters=%v(%s)", bot.account, tip, resp.GetErrorMessage().GetParameters(), actTipHint(tip))
	}
	// 一经达成即锁存,锁存后 progress 固定填 0(guild.proto 的 progress 注释):这两项就是"之后不必再凑人"的可观察证据。
	view := resp.GetActivity()
	if view.GetMyUsedCount() != 1 || !view.GetThresholdReached() || view.GetProgress() != 0 {
		fail(step, "%s 领团圆后视图(有=%v) 今日 %d 次、已锁存=%v、进度 %d,期望 1 / true / 0",
			bot.account, view != nil, view.GetMyUsedCount(), view.GetThresholdReached(), view.GetProgress())
	}
	actCheckSelf(bot, fail, step, wantFunds, wantTotal, wantBalance)
	actAwaitReward(bot, guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_REUNION, fail, step+"-reward")
	want := before + uint64(actReunionItemCount)
	if got := econItemCount(econBag(bot, fail, step+"-bag-after"), actReunionItem); got != want {
		fail(step+"-item", "%s 的物品 %d 数量=%d,期望 %d + %d(待发放数已归 0 而物品没到 = 指令被永久拒绝,看活动页的 my_last_reward_reject_tip_id 与 scene 日志 [AssetOp])",
			bot.account, actReunionItem, got, before, actReunionItemCount)
	}
}

// actAwaitReward 轮询活动页,直到本人该活动的待发放数归 0。
// 总是至少读一次、不信写 RPC 的回包:回包里的待发状态是提交后 best-effort 回读的,读失败时就是 0。
func actAwaitReward(bot *guildSmokeBot, typ guildpb.GuildActivityType, fail econFail, step string) {
	deadline := time.Now().Add(actRewardTimeout)
	for {
		view := actOfType(actViews(bot, fail, step), typ)
		if view == nil {
			fail(step, "活动页里没有 %s", typ)
		}
		if view.GetMyPendingRewardCount() == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			fail(step, "%s 的物品奖励 %s 后仍有 %d 件待发放,最近原因 tip=%d(27001 = 背包满)。看 guild 日志 [GuildActivity] 与 scene 日志 [AssetOp]",
				bot.account, actRewardTimeout, view.GetMyPendingRewardCount(), view.GetMyPendingReasonTipId())
		}
		// 500ms 一次:读类消息号 10 次 / 秒;即使限流行还没回填、吃默认 3 次 / 窗口,这个节奏也不会被 gate 丢包。
		time.Sleep(econSettlePoll)
	}
}

// ---------------------------------------------------------------------------
// 请求助手
// ---------------------------------------------------------------------------

func actViews(bot *guildSmokeBot, fail econFail, step string) []*guildpb.GuildActivityView {
	resp := &guildpb.GetGuildActivitiesResponse{}
	if err := bot.expect(game.GuildServiceGetGuildActivitiesMessageId, &guildpb.GetGuildActivitiesRequest{}, resp, 0); err != nil {
		fail(step, "%s 读活动页: %v", bot.account, err)
	}
	return resp.GetActivities()
}

// actLight 点一次灯,返回业务 tip 与提交后的视图(被拒时视图为空)。
func actLight(bot *guildSmokeBot, activityID uint32, fail econFail, step string) (uint32, *guildpb.GuildActivityView) {
	resp := &guildpb.LightGuildLanternResponse{}
	tip, err := bot.rpc(game.GuildServiceLightGuildLanternMessageId, &guildpb.LightGuildLanternRequest{ActivityId: activityID}, resp)
	if err != nil {
		fail(step, "%v", err)
	}
	return tip, resp.GetActivity()
}

// actClaim 领一次团圆。返回整个回包:被拒时调用方要读 tip 的参数(人数不足带 [在线, 阈值])。
func actClaim(bot *guildSmokeBot, activityID uint32, fail econFail, step string) (uint32, *guildpb.ClaimGuildReunionResponse) {
	resp := &guildpb.ClaimGuildReunionResponse{}
	tip, err := bot.rpc(game.GuildServiceClaimGuildReunionMessageId, &guildpb.ClaimGuildReunionRequest{ActivityId: activityID}, resp)
	if err != nil {
		fail(step, "%v", err)
	}
	return tip, resp
}

// actCheckSelf 读 bot 自己的帮会,核对帮会资金与本人两列帮贡(活动给的帮贡同时进累计与可用)。
func actCheckSelf(bot *guildSmokeBot, fail econFail, step string, funds, total, balance uint64) {
	g := mustMyGuild(bot, fail, step)
	me := actMemberOf(g, bot.gc.PlayerId)
	if me == nil {
		fail(step, "%s(%d) 不在自己帮会 %d 的成员表里", bot.account, bot.gc.PlayerId, g.GetGuildId())
	}
	if g.GetFunds() != funds || me.GetContributionTotal() != total || me.GetContributionBalance() != balance {
		fail(step, "%s 看到帮会资金=%d、本人帮贡 累计 %d / 可用 %d,期望 %d / %d / %d",
			bot.account, g.GetFunds(), me.GetContributionTotal(), me.GetContributionBalance(), funds, total, balance)
	}
}

// actLogout 让一个机器人下线:LeaveGame + Disconnect + 关连接,与各场景收尾时的做法一致。
func actLogout(bot *guildSmokeBot) {
	_ = leaveGame(bot.gc, bot.stats)
	sendDisconnectBestEffort(bot.gc)
	gameobject.PlayerList.Delete(bot.gc.PlayerId)
	bot.gc.Close()
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

// actOfType 取某一类型的活动;没有返回 nil(生成的 getter 对 nil 安全,读出来是零值)。
func actOfType(views []*guildpb.GuildActivityView, typ guildpb.GuildActivityType) *guildpb.GuildActivityView {
	for _, view := range views {
		if view.GetType() == typ {
			return view
		}
	}
	return nil
}

func actMemberOf(guild *guildpb.GuildInfo, playerID uint64) *guildpb.GuildMember {
	for _, member := range guild.GetMembers() {
		if member.GetPlayerId() == playerID {
			return member
		}
	}
	return nil
}

// actTipHint 给本段最常见的几种误配写人话;其余的码去查表。
func actTipHint(tip uint32) string {
	switch tip {
	case tipGuildAssetPending:
		return "kGuildAssetPending:团圆带物品奖励,资产通道未开(guild.yaml AssetOp.Enabled=false),或该玩家待发放的单已满"
	case tipGuildActivityNotOpen:
		return "kGuildActivityNotOpen:guild 没装配活动依赖,或 GuildActivity 该行未启用 / 不在档期"
	case tipGuildActivityAlreadyClaimed:
		return "kGuildActivityAlreadyClaimed:今日次数已用完;同一游戏日重跑前先清 guild_daily_counter(见文件头)"
	case tipGuildActivityJoinTooRecent:
		return "kGuildActivityJoinTooRecent:GuildRule.activity_join_min_hours 不是开发值 0,刚入帮的机器人不满足"
	case tipGuildActivityThresholdNotReached:
		return "kGuildActivityThresholdNotReached:合格在线人数不足(参数为 [在线, 阈值]);只数 player_locator 会话在线、且入帮满 activity_join_min_hours 的成员"
	}
	return "见 data/tip/Tip.xlsx 的 guild_error 段"
}
