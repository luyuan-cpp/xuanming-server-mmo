package main

// guild-smoke 的活动段(帮会二期 B6a-cli + B6b-cli,设计 docs/design/guild-phase2/06-activities.md §6.44):
// 帮主 A(robot_9216)与成员 B / C / D(robot_9217–9219)都在 zone_a,走一遍 元宵灯会 → 中秋团圆 → 同道历练,
// 断言活动视图、个人次数、帮会进度与阈值、帮会资金、个人帮贡,以及团圆 / 历练的物品奖励真的进了背包。
// 由 RunGuildSmoke 在经济段之后调用(guild_smoke.activities=true),账号与管理段、经济段错开(90-consistency Y-09)。
//
// 步骤(编号与设计 §6.44 的表一致;S0 是表上方的"前置"):
//
//	S0. 四人登录进场;各自撤掉遗留申请、离开任何帮会(帮主则解散);A 建帮,B / C / D 申请、A 审批;
//	    记下帮会资金与四人帮贡的基线;
//	S1. A 读活动页 → 3 条,type 依次 1 / 2 / 3;灯会、团圆 OPEN,历练 DISABLED(trial=true 时 OPEN);
//	    四人的灯会 / 团圆(trial=true 时还有历练)今日次数都是 0,否则判 same-game-day-rerun(见下);
//	    灯会 blocked_tip_id=0;卡片上的数值等于默认配表(trial=true 时历练卡片同样核对);
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
// 同道历练 S9–S15(B6b-cli,guild_smoke.trial=true 时跑;见 runGuildTrialSmoke):
//
//	S9.  A 发起历练,名单 [A, B, C] → 回包里的邀请房间 PENDING、已同意 = [A]、有效期 30 秒;
//	     B、C 在 5 秒内各收到 ACTIVITY_CHANGED 且 target = 自己(没收到只告警);
//	S10. B 从自己的活动页取到 lobby_id → 同意 → 仍 PENDING、已同意 = [A, B];C 同样取 lobby_id → 同意 →
//	     回包房间 LAUNCHED、battle_id ≠ 0;A / B / C 20 秒内各收到 NotifyBattleStart 且 battle_id 相同,
//	     各自建战斗直连;三条直连都就绪后一起开自动战斗,都收到 NotifyBattleEnd,结果 = SIDE_A_WIN;
//	S11. 15 秒内轮询到 A 的历练卡片 progress = 1(今日已计资金胜场)、本人今日 1 次、my_trial_battle_id = 0;
//	     帮会资金再 + 300;A / B / C 帮贡各 + 50(用户决策 U2:**阵亡也得奖**,所以不看谁在战斗里倒下,
//	     只有逃跑者不得奖,而本段没有人逃跑);三人的历练物品各到账 4 个;
//	S12. 等过建房冷却(10 秒)→ A 再发起 [A, D] → D 拒绝(回包无错)→ A 的活动页里房间 ENDED、
//	     end_tip_id = kGuildTrialInviteDeclined、end_parameters = [D];
//	S13. D 下线;A 发起 [A, D] → kGuildTrialTeamInvalid ["offline", D](名单校验先于建房,不受 S12 的冷却影响);
//	S14. A 发起 [A] → kGuildTrialTeamInvalid ["size", "0"];
//	S15. B 发起 [A, C](不含自己)→ kGuildTrialTeamInvalid ["initiator_missing", "0"]。
//
// trial=false 时只跑 S1–S8,并在 S1 期望历练卡片是"未开放"(guild 没配 MatchRpc / 历练结果消费的部署)。
//
// 结果约定:全过打印 `GUILD_SMOKE_ACTIVITIES_OK guild_id=…`;任一步失败打印
// `GUILD_SMOKE_ACTIVITIES_FAIL step=Sx reason=…` 并以退出码 1 结束进程。
//
// 可重复性:个人次数按玩家、按游戏日(UTC+8 05:00 切日)记在 guild_daily_counter 里,解散帮会清不掉,
// 所以同一游戏日只能完整跑一次。重跑会在 S1 失败,reason 以 same-game-day-rerun 开头并带上整条清理 SQL
// (只删这四个账号当天的活动计数,历练的次数也在其中;帮会进度行由 S0 的解散删)。冒烟自己不碰数据库。
// 历练另有两样会被上一轮带到下一轮的易失状态,都会自己过期,等一等再跑即可:
// 上一轮停在 S9 / S12 留下的邀请房间(至多 30 秒,期间建房回 [busy, …]);
// 上一轮停在战斗中途留下的 battle:lock(对局结算后释放,期间开战回 [in_battle, …])。
//
// 前置(在 guild_smoke.yaml 头注释的基础上):默认配表 GuildActivity / GuildRule(activity_join_min_hours=0);
// 团圆带物品奖励,所以 guild 的 AssetOp.Enabled=true(关着时领奖回 kGuildAssetPending,本段在 S6 失败并提示);
// 活动 5 个消息号的 MessageLimiter 行已回填(没回填时吃默认 3 次 / 窗口,本段的发送节奏仍在其内);
// 四个账号首次在 zone_a 建角。
// trial=true 另需:guild 配了 MatchRpc 且 Activity.TrialResult.Enabled=true、Kafka.Brokers 非空(历练才算开放);
// match(带 MatchInternal)与 battle 节点(带活动结果回显)已换上 B6b-srv1 的二进制;
// GuildRule 的 trial_invite_ttl_seconds=30、trial_invite_cooldown_seconds=10(本段的等待节奏按这两个默认值排)。

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"proto/battle"
	guildpb "proto/guild"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

// 期望值来自默认配表 GuildActivity / GuildRule / Reward。**故意写死**(理由同 guildSmokeLevel1MaxOfficers):
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

	actTrialContribution uint64 = 50  // GuildActivity[3].personal_contribution:胜利后每名得奖者一份
	actTrialFunds        uint64 = 300 // 每个计资金的胜场发一次
	actTrialFundedWins   uint32 = 3   // guild_threshold:每游戏日计资金的胜场上限
	actTrialDailyLimit   uint32 = 2
	actTrialTeamMin      uint32 = 2 // 含发起人
	actTrialTeamMax      uint32 = 5
	// Reward[2] 与 Reward[1] 同形(两个槽都是物品 1 ×2)。历练副本 Dungeon[1] 的怪(Monster 1 / 2)只掉物品 10 / 11,
	// 不掉物品 1,所以 S11 可以对物品 1 的数量做"恰好多 4 个"的断言;给这两只怪加了物品 1 的掉落就要改那条断言。
	actTrialItem      uint32 = 1
	actTrialItemCount uint32 = 4

	actTrialInviteTTL      = 30 * time.Second // GuildRule[1].trial_invite_ttl_seconds
	actTrialInviteCooldown = 10 * time.Second // GuildRule[1].trial_invite_cooldown_seconds
)

// 历练名单不合法的原因码:kGuildTrialTeamInvalid 的第一个参数(第二个是当事人的 player_id,没有当事人为 "0")。
// 这是客户端可见的契约(客户端按它选文案),与 go/guild/internal/logic/activity_trial.go 的 trialReason* 逐字相同。
const (
	actTrialReasonOffline          = "offline"
	actTrialReasonSize             = "size"
	actTrialReasonInitiatorMissing = "initiator_missing"
)

// 历练段的等待预算。
const (
	// actTrialStartTimeout:全员同意之后多久必须收到开战包(设计 S10:20 秒)。match 的 gather 是异步的:
	// 逐人冻结场景、在 battle 节点建局,之后才推落点分配与开战包。
	actTrialStartTimeout = 20 * time.Second
	// actTrialEndTimeout:自动战斗打到终局的上限,与 battle-smoke 同值(覆盖"打满回合上限"的最坏情况)。
	actTrialEndTimeout = battleSmokeEndTimeout
	// actTrialSettleTimeout:终局之后多久必须结算(设计 S11:15 秒)。
	// 链路是 battle 落结果记录 → Kafka match-results → guild 消费组 guild-trial,正常在 1 秒内。
	actTrialSettleTimeout = 15 * time.Second
	// actTrialTTLSlack:房间有效期断言的容差。房间与回包视图理应出自同一个"现在",差值恰好是 30 秒;
	// 留 2 秒只为不把"视图用了稍晚的时钟"这种无害的实现差异判成失败,配表被改成别的值照样抓得到。
	actTrialTTLSlack = 2 * time.Second
	// actTrialCooldownMargin:等建房冷却时多等的余量。机器人记下的建房时刻本就晚于服务端的起点,
	// 余量只是不想让下一次建房恰好落在冷却到期的那几毫秒上。
	actTrialCooldownMargin = 1500 * time.Millisecond
	// actTrialOfflineRetry:S13 里等"D 已下线"传到 player_locator 的上限(在 actLogoutSettle 之外)。
	// 必须明显短于建房冷却:冷却一过,同一个请求就不再被冷却挡住,而是真的把房间建出来。
	actTrialOfflineRetry = 4 * time.Second
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
	// 同道历练(B6b)。
	tipGuildTrialTeamInvalid    = uint32(tiptable.GuildError_kGuildTrialTeamInvalid)
	tipGuildTrialInviteExpired  = uint32(tiptable.GuildError_kGuildTrialInviteExpired)
	tipGuildTrialInviteDeclined = uint32(tiptable.GuildError_kGuildTrialInviteDeclined)
	tipGuildTrialInviteCooldown = uint32(tiptable.GuildError_kGuildTrialInviteCooldown)
	tipGuildTrialServiceBusy    = uint32(tiptable.GuildError_kGuildTrialServiceBusy)
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
	rerunTypes := []guildpb.GuildActivityType{
		guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_LANTERN,
		guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_REUNION,
	}
	// 历练的次数只在要跑历练时才看:每日 2 次,用过 1 次仍能发起,但 S11 的"本人今日 1 次"就对不上了。
	if sc.Trial {
		rerunTypes = append(rerunTypes, guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_TRIAL)
	}
	for _, bot := range bots {
		mine := views
		if bot != a {
			mine = actViews(bot, fail, "S1")
		}
		for _, typ := range rerunTypes {
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
		fail("S1", "灯会=%s 团圆=%s 历练=%s,期望 %s / %s / %s(trial=%v;配表行启用之外,历练还要 guild 的部署配齐才开放:"+
			"配了 MatchRpc 且 Activity.TrialResult.Enabled=true、Kafka.Brokers 非空,缺哪样启动日志里有一行原因。"+
			"guild_smoke.yaml 的 trial 要与它一致)",
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
	if sc.Trial {
		if blocked := trial.GetBlockedTipId(); blocked != 0 {
			fail("S1-trial", "历练 blocked_tip_id=%d,期望 0(%s)", blocked, actTrialTipHint(blocked))
		}
		if trial.GetPersonalContribution() != actTrialContribution || trial.GetGuildFunds() != actTrialFunds ||
			trial.GetGuildThreshold() != actTrialFundedWins || trial.GetDailyLimit() != actTrialDailyLimit ||
			trial.GetTeamSizeMin() != actTrialTeamMin || trial.GetTeamSizeMax() != actTrialTeamMax {
			fail("S1-trial", "历练卡片 帮贡=%d 资金=%d 每日计资金胜场=%d 每日=%d 人数=%d–%d,期望 %d / %d / %d / %d / %d–%d(GuildActivity 默认行)",
				trial.GetPersonalContribution(), trial.GetGuildFunds(), trial.GetGuildThreshold(), trial.GetDailyLimit(),
				trial.GetTeamSizeMin(), trial.GetTeamSizeMax(),
				actTrialContribution, actTrialFunds, actTrialFundedWins, actTrialDailyLimit, actTrialTeamMin, actTrialTeamMax)
		}
		if items := trial.GetRewardItems(); len(items) != 1 || items[0].GetItemId() != actTrialItem || items[0].GetCount() != actTrialItemCount {
			fail("S1-trial", "历练奖励物品=%v,期望只有一项:物品 %d ×%d(Reward[2] 两个槽按 item_id 合并)", items, actTrialItem, actTrialItemCount)
		}
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

	// ---- S9–S15:同道历练(B6b-cli) ----
	if sc.Trial {
		funds = runGuildTrialSmoke(actTrialInput{
			bots:       &bots,
			guildID:    guildID,
			activityID: trial.GetActivityId(),
			funds:      funds,
			total:      total,
			balance:    balance,
		}, fail)
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
// 同道历练(S9–S15,B6b-cli)
// ---------------------------------------------------------------------------

// actTrialInput 是历练段从前八步接过来的现场。
type actTrialInput struct {
	// bots 是活动段的四个会话(按位次)。给指针而不是拷贝:S13 让 D 下线后要把它的位次置空,
	// 失败收尾时才不会把已经关掉的连接再关一次。
	bots    *[actSlotCount]*guildSmokeBot
	guildID uint64
	// activityID 是 S1 读到的历练卡片的 activity_id(写 RPC 必须回传它)。
	activityID uint32
	// funds / total / balance 是 S8 结束时的帮会资金与四人帮贡(累计 / 可用),历练段在它们之上继续记账。
	funds          uint64
	total, balance [actSlotCount]uint64
}

// runGuildTrialSmoke 跑 S9–S15(步骤见文件头),返回结束时的帮会资金。任一步失败由 fail 结束进程。
// 返回时 D 已下线(位次已置空),A / B / C 仍在线、仍在帮,帮主仍是 A —— 收尾的解散照常可做。
func runGuildTrialSmoke(in actTrialInput, fail econFail) uint64 {
	a, b, c, d := in.bots[actSlotA], in.bots[actSlotB], in.bots[actSlotC], in.bots[actSlotD]
	// 参战的三人与各自的位次(帮贡按位次记账)。
	fighters := []*guildSmokeBot{a, b, c}
	fighterSlots := []int{actSlotA, actSlotB, actSlotC}
	funds, total, balance := in.funds, in.total, in.balance
	pending := guildpb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING
	trialType := guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_TRIAL

	// 物品基线在开战之前读:A、B 在 S6 / S7 已经各领过 4 个同样的物品,只有"相对这一刻多了几个"才说明是历练发的。
	itemsBefore := make([]uint64, len(fighters))
	for i, bot := range fighters {
		itemsBefore[i] = econItemCount(econBag(bot, fail, "S9-bag"), actTrialItem)
	}

	// ---- S9:A 建邀请房间 [A, B, C] ----
	roster := []uint64{a.gc.PlayerId, b.gc.PlayerId, c.gc.PlayerId}
	// 清空留底必须在建房之前:房间一写进 Redis 服务端就推,晚清一步会把这条推送一起丢掉(同 S4)。
	b.clearPushes()
	c.clearPushes()
	tip, started := actStartTrial(a, in.activityID, roster, fail, "S9")
	// 建房冷却从服务端受理的那一刻起算,比这里记下的时刻略早;S12 等的时候另加了余量。
	firstLobbyAt := time.Now()
	if tip != 0 {
		fail("S9", "A 发起历练 tip=%d parameters=%v(%s)", tip, started.GetErrorMessage().GetParameters(), actTrialTipHint(tip))
	}
	view := started.GetActivity()
	lobby := view.GetTrialLobby()
	if lobby.GetLobbyId() == 0 || lobby.GetInitiatorPlayerId() != a.gc.PlayerId || lobby.GetState() != pending ||
		!slices.Equal(lobby.GetMemberPlayerIds(), roster) || !slices.Equal(lobby.GetAcceptedPlayerIds(), roster[:1]) {
		fail("S9", "建房回包里的房间(视图有=%v、房间有=%v) lobby_id=%d 发起人=%d state=%s 名单=%v 已同意=%v,"+
			"期望 非 0 / %d / %s / %v / %v(回包没带视图 = 服务端建房后回读失败,看 guild 日志 [GuildTrial] rebuild trial view failed)",
			view != nil, lobby != nil, lobby.GetLobbyId(), lobby.GetInitiatorPlayerId(), lobby.GetState(),
			lobby.GetMemberPlayerIds(), lobby.GetAcceptedPlayerIds(), a.gc.PlayerId, pending, roster, roster[:1])
	}
	// 有效期用服务端自己的两个时刻相减,不拿本机时钟比(客户端同理:倒计时以 server_time_ms 为基准)。
	remain := time.Duration(int64(lobby.GetExpireAtMs())-int64(view.GetServerTimeMs())) * time.Millisecond
	if remain <= actTrialInviteTTL-actTrialTTLSlack || remain > actTrialInviteTTL {
		fail("S9", "房间有效期还剩 %s(expire_at_ms=%d server_time_ms=%d),期望 %s(GuildRule.trial_invite_ttl_seconds 默认 30)",
			remain, lobby.GetExpireAtMs(), view.GetServerTimeMs(), actTrialInviteTTL)
	}
	lobbyID := lobby.GetLobbyId()
	// 邀请是逐人推的,target = 被邀请人自己(客户端靠它决定弹不弹邀请框)。推送至多一次,设计只要求告警:
	// 被邀请人打开活动页照样看得到这张邀请,S10 走的就是那条路。
	for _, invitee := range []*guildSmokeBot{b, c} {
		if err := invitee.waitPushTo(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_ACTIVITY_CHANGED,
			in.guildID, invitee.gc.PlayerId, guildSmokePushTimeout); err != nil {
			zap.L().Warn("[guild-smoke] activities S9: 被邀请人没等到 target=自己 的 ACTIVITY_CHANGED 推送(只告警,不判失败)",
				zap.String("account", invitee.account), zap.Error(err))
		}
	}
	zap.L().Info("[guild-smoke] activities S9: trial lobby created",
		zap.Uint64("lobby_id", lobbyID), zap.Uint64s("roster", roster), zap.Duration("ttl", remain))

	// ---- S10:B、C 同意;C 的那一票凑齐全员,由 C 的这次调用开战 ----
	actExpectInvite(b, lobbyID, fail, "S10-b")
	tip, responded := actRespondTrial(b, lobbyID, true, fail, "S10-b")
	if tip != 0 {
		fail("S10-b", "B 同意邀请 tip=%d parameters=%v(%s)", tip, responded.GetErrorMessage().GetParameters(), actTrialTipHint(tip))
	}
	lobby = responded.GetActivity().GetTrialLobby()
	if lobby.GetState() != pending || !slices.Equal(lobby.GetAcceptedPlayerIds(), roster[:2]) {
		fail("S10-b", "B 同意后房间(有=%v) state=%s 已同意=%v,期望 %s / %v(还差 C,不该开战)",
			lobby != nil, lobby.GetState(), lobby.GetAcceptedPlayerIds(), pending, roster[:2])
	}
	actExpectInvite(c, lobbyID, fail, "S10-c")
	tip, responded = actRespondTrial(c, lobbyID, true, fail, "S10-c")
	if tip != 0 {
		// 开战失败时回包同时带原因与已经 ENDED 的房间;原因的参数(如 [in_battle, 某人])在 error_message 里。
		fail("S10-c", "C 同意邀请(凑齐全员,由这次调用开战)tip=%d parameters=%v(%s)",
			tip, responded.GetErrorMessage().GetParameters(), actTrialTipHint(tip))
	}
	lobby = responded.GetActivity().GetTrialLobby()
	battleID := lobby.GetBattleId()
	if lobby.GetState() != guildpb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED || battleID == 0 {
		fail("S10-c", "C 同意后房间(有=%v) state=%s battle_id=%d,期望 %s 且 battle_id 非 0",
			lobby != nil, lobby.GetState(), battleID, guildpb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED)
	}
	actFightTrial(fighters, battleID, fail, "S10")

	// ---- S11:结算 —— 帮贡、资金、今日胜场、物品 ----
	settled := actAwaitTrialSettled(a, battleID, fail, "S11")
	if settled.GetProgress() != 1 || settled.GetMyUsedCount() != 1 || settled.GetMyTrialBattleId() != 0 {
		fail("S11", "结算后 A 的历练卡片 今日计资金胜场=%d、本人今日 %d 次、进行中的对局=%d,期望 1 / 1 / 0(battle_id=%d)",
			settled.GetProgress(), settled.GetMyUsedCount(), settled.GetMyTrialBattleId(), battleID)
	}
	// 三人各读各的帮会:资金是全帮一个数,帮贡是各人自己的。用户决策 U2:阵亡也得奖,所以不区分谁在战斗里倒下 ——
	// 只有逃跑者不得奖,本段没有人逃跑,三人都必须 + 50。数值恰好相等还顺带守住"重复投递的结果不重复发奖"
	// (battle 未销账每 10 秒重发一次)。
	// 结算的缓存失效紧跟在事务提交之后;这里与上面那次读活动页至少隔一个请求间隔(300ms),不会读到失效之前的快照。
	funds += actTrialFunds
	for i, bot := range fighters {
		slot := fighterSlots[i]
		total[slot] += actTrialContribution
		balance[slot] += actTrialContribution
		actCheckSelf(bot, fail, "S11", funds, total[slot], balance[slot])
	}
	for _, bot := range []*guildSmokeBot{b, c} {
		mine := actTrialView(bot, fail, "S11")
		if mine.GetProgress() != 1 || mine.GetMyUsedCount() != 1 || mine.GetMyTrialBattleId() != 0 {
			fail("S11", "结算后 %s 的历练卡片 今日计资金胜场=%d、本人今日 %d 次、进行中的对局=%d,期望 1 / 1 / 0",
				bot.account, mine.GetProgress(), mine.GetMyUsedCount(), mine.GetMyTrialBattleId())
		}
	}
	// 物品与团圆同一套判据(见 actClaimDelivered):待发放数归 0,且背包里真的多了。
	for i, bot := range fighters {
		actAwaitReward(bot, trialType, fail, "S11-reward")
		want := itemsBefore[i] + uint64(actTrialItemCount)
		if got := econItemCount(econBag(bot, fail, "S11-bag"), actTrialItem); got != want {
			fail("S11-item", "%s 的物品 %d 数量=%d,期望 %d + %d(待发放数已归 0 而物品没到 = 指令被永久拒绝,看活动页的 my_last_reward_reject_tip_id 与 scene 日志 [AssetOp])",
				bot.account, actTrialItem, got, itemsBefore[i], actTrialItemCount)
		}
	}
	zap.L().Info("[guild-smoke] activities S11: trial settled",
		zap.Uint64("battle_id", battleID), zap.Uint64("funds", funds))

	// ---- S12:被邀请人拒绝 → 房间解散,原因带上是谁 ----
	// 同一发起人两次建房至少隔 10 秒(S9 那次算第一次)。战斗与结算通常已经把这段时间用掉了,不够才补等。
	if wait := time.Until(firstLobbyAt.Add(actTrialInviteCooldown + actTrialCooldownMargin)); wait > 0 {
		time.Sleep(wait)
	}
	dID := d.gc.PlayerId
	withD := []uint64{a.gc.PlayerId, dID}
	tip, started = actStartTrial(a, in.activityID, withD, fail, "S12")
	if tip != 0 {
		fail("S12", "A 发起历练 [A, D] tip=%d parameters=%v(%s)", tip, started.GetErrorMessage().GetParameters(), actTrialTipHint(tip))
	}
	lobby = started.GetActivity().GetTrialLobby()
	declinedID := lobby.GetLobbyId()
	if declinedID == 0 || declinedID == lobbyID || lobby.GetState() != pending || !slices.Equal(lobby.GetMemberPlayerIds(), withD) {
		fail("S12", "第二个房间(有=%v) lobby_id=%d state=%s 名单=%v,期望 一个不同于 %d 的新 id / %s / %v",
			lobby != nil, declinedID, lobby.GetState(), lobby.GetMemberPlayerIds(), lobbyID, pending, withD)
	}
	actExpectInvite(d, declinedID, fail, "S12-d")
	tip, responded = actRespondTrial(d, declinedID, false, fail, "S12-d")
	if tip != 0 {
		fail("S12-d", "D 拒绝邀请 tip=%d parameters=%v,期望受理(%s)", tip, responded.GetErrorMessage().GetParameters(), actTrialTipHint(tip))
	}
	// 从发起人 A 的活动页读:A 不是这次的操作者,他要靠这一页知道"谁拒绝了"。
	lobby = actTrialView(a, fail, "S12").GetTrialLobby()
	wantDecliner := []string{strconv.FormatUint(dID, 10)}
	if lobby.GetLobbyId() != declinedID || lobby.GetState() != guildpb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED ||
		lobby.GetEndTipId() != tipGuildTrialInviteDeclined || !slices.Equal(lobby.GetEndParameters(), wantDecliner) {
		fail("S12", "D 拒绝后 A 看到的房间(有=%v) lobby_id=%d state=%s end_tip_id=%d end_parameters=%v,期望 %d / %s / kGuildTrialInviteDeclined(%d) / %v",
			lobby != nil, lobby.GetLobbyId(), lobby.GetState(), lobby.GetEndTipId(), lobby.GetEndParameters(),
			declinedID, guildpb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, tipGuildTrialInviteDeclined, wantDecliner)
	}

	// ---- S13:邀请已下线的人 → 名单校验拒绝(先于建房,S12 的冷却挡不到它) ----
	actLogout(d)
	in.bots[actSlotD] = nil
	time.Sleep(actLogoutSettle)
	wantOffline := []string{actTrialReasonOffline, strconv.FormatUint(dID, 10)}
	retryUntil := time.Now().Add(actTrialOfflineRetry)
	for {
		tip, rejected := actStartTrial(a, in.activityID, withD, fail, "S13")
		params := rejected.GetErrorMessage().GetParameters()
		if tip == tipGuildTrialTeamInvalid && slices.Equal(params, wantOffline) {
			break
		}
		// 回"冷却中"说明名单校验已经放行、一路走到了建房脚本 —— 也就是 player_locator 此刻仍判 D 在线
		// (下线还没传到)。只有这一种答复值得再等;别的一律直接失败。
		// 必须在 S12 的冷却用完之前放弃(actTrialOfflineRetry):冷却一过,同一个请求会真的把房间建出来。
		if tip != tipGuildTrialInviteCooldown || !time.Now().Before(retryUntil) {
			fail("S13", "A 邀请已下线的 D tip=%d parameters=%v,期望 kGuildTrialTeamInvalid(%d) %v"+
				"(tip=kGuildTrialInviteCooldown(%d) 或 0 表示名单校验放行了:D 下线 %s 之后 player_locator 的会话仍是在线)",
				tip, params, tipGuildTrialTeamInvalid, wantOffline, tipGuildTrialInviteCooldown, actLogoutSettle+actTrialOfflineRetry)
		}
		time.Sleep(guildSmokeSameIdCooldown)
	}

	// ---- S14 / S15:名单本身不合法(规范化阶段就拒绝,不读库、不碰冷却) ----
	// A 在 S12、S13 已经连发过几次 StartGuildTrial,先退火再发(见 guildSmokeSameIdCooldown)。
	time.Sleep(guildSmokeSameIdCooldown)
	actExpectTeamInvalid(a, in.activityID, []uint64{a.gc.PlayerId}, actTrialReasonSize, 0, fail, "S14")
	actExpectTeamInvalid(b, in.activityID, []uint64{a.gc.PlayerId, c.gc.PlayerId}, actTrialReasonInitiatorMissing, 0, fail, "S15")
	zap.L().Info("[guild-smoke] activities S12-S15: trial invite rejections ok",
		zap.Uint64("declined_lobby_id", declinedID), zap.Uint64("offline_player", dID))
	return funds
}

// actFightTrial 让 fighters 把 battleID 这一局打完(S10 的战斗部分):各自等开战包并建战斗直连 →
// 三条直连都就绪后一起开自动战斗 → 各自等到终局,且结果必须是己方胜(历练只有胜利才发奖)。
//
// 战斗推送不需要 guildSmokeBot 另外认领:大厅连接上的 NotifyBattleAssigned / NotifyBattleStart / NotifyBattleEnd
// 不在 onMessage 的认领名单里,落到通用分发 handler.MessageBodyHandler,由它转成 gameobject.Player 上的信号
// (与 battle-smoke 同一套);直连上的战斗帧经 dispatchBattleDirectDefault 落到同一个 Player。
// Player 上的开战 / 落点 / 终局信号都是一次性广播,只适合"一个会话只打一场" —— 本段每人恰好只打这一场。
//
// 为什么等三条直连都就绪才开自动战斗:开了自动的对局可能极快打完(battle-smoke 记录过单人 PVE 约 100ms 终局),
// 而直连晚于终局的人,握手会因房间已销毁被拒。先齐后开,就不存在"别人先把仗打完、我还没连上"的竞态;
// 剩下的只有回合超时(6 秒一回合,未出招者由 battle 填默认普攻)先于直连这一种,那要有人好几秒连不上,本身就该失败。
func actFightTrial(fighters []*guildSmokeBot, battleID uint64, fail econFail, step string) {
	directs := make([]*battleDirectConn, len(fighters))
	errs := make([]error, len(fighters))
	var wg sync.WaitGroup
	for i, bot := range fighters {
		wg.Add(1)
		go func(i int, bot *guildSmokeBot) {
			defer wg.Done()
			directs[i], errs[i] = actTrialConnect(bot, battleID)
		}(i, bot)
	}
	wg.Wait()
	// fail 走 os.Exit、不跑 defer,所以每条失败路径都显式关直连(Close 对 nil 安全)。
	closeDirects := func() {
		for _, direct := range directs {
			direct.Close()
		}
	}
	for _, err := range errs {
		if err != nil {
			closeDirects()
			fail(step+"-start", "%v(若是没收到开战包:房间已 LAUNCHED,说明 match 受理了,多半是它的 gather 在冻结场景 / 建局时失败,"+
				"看 match 日志 battle=%d;那一局不会有结果,guild 的登记行由巡检器判过期)", err, battleID)
		}
	}
	for i, bot := range fighters {
		if err := directs[i].Send(game.BattleClientPlayerSetAutoBattleMessageId,
			&battle.SetAutoBattleRequest{BattleId: battleID, Enabled: true}); err != nil {
			closeDirects()
			fail(step+"-auto", "%s 经直连发 SetAutoBattle(battle_id=%d): %v", bot.account, battleID, err)
		}
		bot.stats.MsgSent()
	}
	// 三人共用一个截止时刻:他们等的是同一局的终局。
	endCtx, cancel := context.WithTimeout(context.Background(), actTrialEndTimeout)
	defer cancel()
	for i, bot := range fighters {
		outcome, err := bot.player.WaitBattleEnd(endCtx)
		if err != nil {
			delivery := directs[i].summary()
			closeDirects()
			fail(step+"-end", "%s 在 %s 内没收到 battle_id=%d 的 NotifyBattleEnd(直连收包 %s): %v",
				bot.account, actTrialEndTimeout, battleID, delivery, err)
		}
		if got := battle.EBattleOutcome(outcome); got != battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN {
			closeDirects()
			fail(step+"-end", "%s 收到的战斗结果=%s,期望 %s(历练只有胜利才结算奖励;默认配表下三人自动战斗打 Dungeon[1] 应当获胜)",
				bot.account, got, battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN)
		}
	}
	zap.L().Info("[guild-smoke] activities S10: trial battle won",
		zap.Uint64("battle_id", battleID), zap.Int("fighters", len(fighters)), zap.String("a_direct", directs[0].summary()))
	closeDirects()
}

// actTrialConnect 等 bot 的开战包与落点分配(都经大厅连接下发,落点先于开战包),核对是不是 battleID 这一局,
// 再建战斗直连。开战即直连、不等别人:回合结算只走直连,没有活直连的那几回合直接丢。
// 在各自的 goroutine 里跑,所以只返回 error,由主流程统一报失败。
func actTrialConnect(bot *guildSmokeBot, battleID uint64) (*battleDirectConn, error) {
	startCtx, cancel := context.WithTimeout(context.Background(), actTrialStartTimeout)
	started, err := bot.player.WaitBattleStart(startCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%s 在 %s 内没收到 NotifyBattleStart: %w", bot.account, actTrialStartTimeout, err)
	}
	if started != battleID {
		return nil, fmt.Errorf("%s 收到的 NotifyBattleStart battle_id=%d,期望房间里的 %d", bot.account, started, battleID)
	}
	assignCtx, cancel := context.WithTimeout(context.Background(), battleDirectConnTimeout)
	assigned, err := bot.player.WaitBattleAssigned(assignCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%s 在 %s 内没收到 NotifyBattleAssigned(battle_id=%d): %w", bot.account, battleDirectConnTimeout, battleID, err)
	}
	if assigned.GetBattleId() != battleID || assigned.GetRole() != battle.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT {
		return nil, fmt.Errorf("%s 的 BattleAssigned battle_id=%d role=%s,期望 %d 的参战票据",
			bot.account, assigned.GetBattleId(), assigned.GetRole(), battleID)
	}
	direct, err := dialBattleDirect(bot.account, bot.gc.PlayerId, assigned, bot.stats, dispatchBattleDirectDefault)
	if err != nil {
		return nil, fmt.Errorf("%s 直连 battle_id=%d: %w", bot.account, battleID, err)
	}
	return direct, nil
}

// actAwaitTrialSettled 轮询 bot 的历练卡片,直到今日计资金胜场 ≥ 1(这一局已结算),返回那一刻的卡片。
// 帮会是本轮新建的,开战前胜场必为 0,所以"≥ 1"只可能来自 battleID 这一局;恰好等于几由调用方断言。
func actAwaitTrialSettled(bot *guildSmokeBot, battleID uint64, fail econFail, step string) *guildpb.GuildActivityView {
	deadline := time.Now().Add(actTrialSettleTimeout)
	for {
		view := actTrialView(bot, fail, step)
		if view.GetProgress() >= 1 {
			return view
		}
		if !time.Now().Before(deadline) {
			fail(step, "战斗结束 %s 后历练仍未结算(battle_id=%d):今日计资金胜场=%d、本人今日 %d 次、进行中的对局=%d。"+
				"结算链路 = battle 落 SharedRedis battle:activity_result:{battle_id} → Kafka match-results → guild 消费组 guild-trial;"+
				"看 guild 日志 [GuildTrial] 与指标 guild_trial_result_total,并确认 guild 的 Activity.TrialResult.Enabled=true",
				actTrialSettleTimeout, battleID, view.GetProgress(), view.GetMyUsedCount(), view.GetMyTrialBattleId())
		}
		time.Sleep(econSettlePoll)
	}
}

// actExpectInvite 断言 bot 自己的活动页里挂着 lobbyID 这张待确认的邀请。
// 客户端的 lobby_id 就是从这里取的:推送只说"去拉活动页",不带房间号,所以被邀请人不靠推送也必须看得到它。
func actExpectInvite(bot *guildSmokeBot, lobbyID uint64, fail econFail, step string) {
	lobby := actTrialView(bot, fail, step).GetTrialLobby()
	pending := guildpb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING
	if lobby.GetLobbyId() != lobbyID || lobby.GetState() != pending {
		fail(step, "%s 的活动页里的邀请房间(有=%v) lobby_id=%d state=%s end_tip_id=%d,期望 %d / %s",
			bot.account, lobby != nil, lobby.GetLobbyId(), lobby.GetState(), lobby.GetEndTipId(), lobbyID, pending)
	}
}

// actExpectTeamInvalid 断言一次建房被 kGuildTrialTeamInvalid 拒绝,且参数恰好是 [reason, offender]。
func actExpectTeamInvalid(bot *guildSmokeBot, activityID uint32, members []uint64, reason string, offender uint64, fail econFail, step string) {
	tip, resp := actStartTrial(bot, activityID, members, fail, step)
	want := []string{reason, strconv.FormatUint(offender, 10)}
	if got := resp.GetErrorMessage().GetParameters(); tip != tipGuildTrialTeamInvalid || !slices.Equal(got, want) {
		fail(step, "%s 发起历练(名单 %v)tip=%d parameters=%v,期望 kGuildTrialTeamInvalid(%d) %v",
			bot.account, members, tip, got, tipGuildTrialTeamInvalid, want)
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

// actTrialView 读 bot 的活动页并取出历练卡片(房间、进行中的对局都挂在这张卡片上)。
func actTrialView(bot *guildSmokeBot, fail econFail, step string) *guildpb.GuildActivityView {
	view := actOfType(actViews(bot, fail, step), guildpb.GuildActivityType_GUILD_ACTIVITY_TYPE_TRIAL)
	if view == nil {
		fail(step, "%s 的活动页里没有同道历练", bot.account)
	}
	return view
}

// actStartTrial 发起一次同道历练(只建邀请房间,不直接开战)。返回整个回包:被拒时调用方要读 tip 的参数。
func actStartTrial(bot *guildSmokeBot, activityID uint32, members []uint64, fail econFail, step string) (uint32, *guildpb.StartGuildTrialResponse) {
	resp := &guildpb.StartGuildTrialResponse{}
	tip, err := bot.rpc(game.GuildServiceStartGuildTrialMessageId,
		&guildpb.StartGuildTrialRequest{ActivityId: activityID, MemberPlayerIds: members}, resp)
	if err != nil {
		fail(step, "%v", err)
	}
	return tip, resp
}

// actRespondTrial 同意(accept=true)或拒绝一张邀请;凑齐全员的那一次同意会在本次调用里开战。
// 返回整个回包:开战失败时 tip 是原因、视图里是已经结束的房间,两样调用方都要看。
func actRespondTrial(bot *guildSmokeBot, lobbyID uint64, accept bool, fail econFail, step string) (uint32, *guildpb.RespondGuildTrialInviteResponse) {
	resp := &guildpb.RespondGuildTrialInviteResponse{}
	tip, err := bot.rpc(game.GuildServiceRespondGuildTrialInviteMessageId,
		&guildpb.RespondGuildTrialInviteRequest{LobbyId: lobbyID, Accept: accept}, resp)
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

// actTrialTipHint 是历练段的版本:同一个码在历练里的常见原因与灯会 / 团圆不同(未开放多半是部署没配齐),
// 历练自己的五个码也在这里;其余的交给 actTipHint。
func actTrialTipHint(tip uint32) string {
	switch tip {
	case tipGuildActivityNotOpen:
		return "kGuildActivityNotOpen:历练未开放 —— guild 没配 MatchRpc,或 Activity.TrialResult.Enabled 不是 true / Kafka.Brokers 为空" +
			"(guild 启动日志里有一行原因);也可能是 GuildActivity 历练行未启用"
	case tipGuildTrialServiceBusy:
		return "kGuildTrialServiceBusy:guild 调 match 内部服务 MatchInternal.StartActivityBattle 出错 / 超时 / 被判内部错误 —— " +
			"match 没起、没换上带 match_internal 的二进制,或 etcd 里没有 guild.yaml 的 MatchRpc.Etcd.Key;看 guild 日志 [GuildTrial] 与 match 日志"
	case tipGuildTrialTeamInvalid:
		return "kGuildTrialTeamInvalid:参数为 [原因, player_id]。busy = 上一轮留下的邀请房间还没过期(至多 30 秒);" +
			"in_battle = 上一轮的对局还没结算(battle:lock 还在);not_ready = match 里有在途票据或没有场景位置;offline = 会话不在线"
	case tipGuildTrialInviteCooldown:
		return "kGuildTrialInviteCooldown:参数为剩余秒数。本段按 GuildRule.trial_invite_cooldown_seconds=10 排的节奏," +
			"被拒说明配表不是默认值,或上一轮刚用同一个帮主建过房(等几秒重跑)"
	case tipGuildTrialInviteExpired:
		return "kGuildTrialInviteExpired:房间不存在 / 已过期 / 已结束,或本人不在名单里;本段按 trial_invite_ttl_seconds=30 排的节奏"
	case tipGuildTrialInviteDeclined:
		return "kGuildTrialInviteDeclined:有人拒绝或发起人取消,房间已解散"
	}
	return actTipHint(tip)
}
