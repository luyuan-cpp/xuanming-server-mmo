package main

// ordering-smoke 的 O5–O7:补齐 docs/design/client-request-ordering.md §12 批次 R 剩下的三组流水线。
// 入口 runOrderingSmokeGroups 由 RunOrderingSmoke 在 O1 之前调用;三组各用自己的账号与连接,
// 不读也不改主机器人(robot_9901)的状态。
//
// 编号:O4 留给分支 claude/practical-bouman-1beb0d 正在加的「LeaveGame → JoinQueue」(见该分支设计文档 §17,
// 未合入主线;主线 client-request-ordering.md 目前只到 §16)。O4 让主机器人离开游戏,所以必须是**主机器人**的最后一步;
// 这条约束只管主机器人。O5–O7 不依赖主机器人,O4 合入后排在哪都不影响这三组。
//
// 每组每轮的判定方式与 O3 相同:不等回包连发 X → Y,等两条回包,再查权威状态,与「后发的 Y 最后执行」应得的状态比较。
// 对不上即本轮乱序:先收尾到干净的起点再进下一轮,全部轮次跑完后汇总本组的乱序轮次,作为乱序类失败交回;它不中断
// 后面的组与 O1–O3,由 RunOrderingSmoke 在 O3 之后与 O3 的乱序汇总成一行 FAIL(理由见 runOrderingSmokeGroups)。
// gate 路由 lane(批次 C)上线之前预期会间歇乱序,上线之后必须每轮通过。回包码与最终状态组合不出任何一种执行顺序时
// 立即失败(那不是顺序问题)。
//
//	O5 帮会(robot_9902 帮主、robot_9903 申请人):帮主建临时帮;申请人连发 ApplyJoinGuild(218)→ CancelGuildApplication(219)。
//	   顺序执行 = 申请后撤回(撤回回 0),帮主侧 ListGuildApplications(221,直接读 MySQL 申请行)里没有申请人;
//	   颠倒 = 撤回先执行(回 kGuildApplicationNotFound)、申请后执行,申请人留在待审名单里 → 记乱序,补一次撤回收尾。
//	   两条并发执行时两个事务可能撞锁,其中一条回 kGuildBusyRetry → 同样记乱序(串行执行互不争锁),补一次撤回收尾。
//	   全部轮次后解散临时帮(连带删掉它的待审申请)。
//	O6 好友(robot_9904 接收方、robot_9905 申请方):申请方先 AddFriend(234)拿 request_id;接收方连发
//	   AcceptFriend(238)→ RemoveFriend(11)。顺序执行 = 成为好友后删除,双方 GetFriendList(12,直接读 MySQL)都看不到对方;
//	   颠倒 = 删除先执行(删非好友是幂等成功)、同意后执行,双方成了好友 → 记乱序,补一次删除收尾。
//	   两条回包在两种顺序下都是受理,分不出顺序,只能看最终的好友关系;只有一方看得到对方是好友边单边残留,直接失败。
//	O7 登录(robot_9906):先完整登录一次拿到 player_id 并离开;此后每轮新开一条连接,**不等 Login 回包**连发
//	   Login(48)→ EnterGame(26,带已知的 player_id)。顺序执行 = EnterGame 不被判「未登录」且最终收到进场推送;
//	   颠倒 = EnterGame 先执行,login 查不到这条连接的登录态,回 kLoginSessionNotFound(entergamelogic.go「2. Get account」)
//	   → 记乱序,断开收尾。
//
// 清场(每组开始前)选「安全清掉再复核」,不学 O2 直接失败:帮会与好友数据都在 MySQL 里跨轮次存活,这些账号只给本场景用,
// 残留只可能是上一次运行中途失败留下的;清理动作(撤回申请、退帮 / 解散、删好友、拒绝待处理申请)不消耗申请配额,
// 也碰不到别的冒烟的账号。清完按权威查询复核,仍不干净才失败。中途失败留下的临时帮 / 申请 / 好友关系都由下一次运行的清场收掉。
// O7 没有需要清的持久状态:引导登录会顶掉上一次运行遗留的会话。
//
// 轮数与间隔按服务端现码定,保证不因配额 / 冷却误报:
//   - guild ApplicationQuotaPerMinute = 10(go/guild/etc/guild.yaml;固定 60s 窗口,刷新与业务拒绝也计数):O5 每轮申请 1 次。
//     撤回不计数;撤回后再申请没有冷却(申请行按 (帮, 人) 重新插入)。
//   - friend RequestQuotaPerMinute = 10(go/friend/etc/friend.yaml;固定 60s 窗口,只计 AddFriend):O6 每轮 1 次。
//     删好友后立即再申请没有冷却(终态申请按 (from, to) 重新发起、换新代次)。
//     两组每次运行各用 5 次,所以同一分钟内最多连跑两次,第三次会撞 kRateLimitExceeded(失败原因里写明)。
//   - gate MessageLimiter(按会话、按消息号;message_limiter.cpp):表里帮会 / 好友的写号上限 5、读号上限 10,
//     48 / 26 / 17 / 58 不在表里吃默认上限 3,窗口都是 1。时间戳按整秒记、超过窗口才淘汰,所以一条请求最多被计约 2 秒,
//     不是滑动 1 秒。每轮同一会话的同一个号正常 1 次、乱序轮的收尾再多 1 次;O5 / O6 每轮之前(含第 1 轮,与清场里的
//     同号请求错开)隔 orderingSmokeRoundSpacing,任意 2 秒里至多落进相邻两轮。按 2 秒算的最坏计数:同一会话的
//     219 / 11 ≤ 4(上限 5),221 / 12 ≤ 4(上限 10),218 / 234 / 238 ≤ 2(上限 5);O7 每轮是新连接,
//     48 / 26 / 17 / 58 每条连接各 1 次(上限 3),限流状态随会话重置。
//   - login:同账号的 Login 由账号锁串行(冲突回 kLoginInProgress);EnterGame 的玩家锁持有到进场链收尾才放,
//     所以 O7 每条连接断开后先隔 orderingLoginRoundGap 再开下一条。O7 每轮像真实客户端重连一样重走 AssignGate;
//     robot 调 AssignGate 不带账号(main.go httpAssignGateRequest.Account 为空),网关的账号冷却对空账号不生效,
//     开了 gate.rate-limit 时也只受 IP 桶约束(application.yaml 默认 ip-rps 5 / ip-burst 20),整场不到十次
//     AssignGate,碰不到它。
//
// 账号:robot_ 前缀才命中 login 的 DevPasswordAuth。9902–9906 在 ordering-smoke 自己的 990x 段里(9901 是主机器人,
// 9911 / 9912 是 sync-smoke),与其它冒烟的号段错开。帮会两人必须首次在 zone_id 建角:申请要求申请人与帮会同归属区,
// 不同区时 guild 回 kGuildNotFound(不向客户端透露帮会在别区)。
//
// 为什么不直接调 guild / friend 冒烟里的 cancelAllApplications / leaveAnyGuild / friendSmokePreclean:它们是绑在
// guildSmokeBot / friendSmokeBot 上的方法,而一条连接只能挂一个收包循环;它们的 call 经 smokeCaller 同步等一次调用,
// 没法在同一连接上流水线连发两条再一起等。所以三组统一用 orderingSmokeBot 的 callAll(每条请求一个 gc.Call,
// 回包按请求号各自认领),只复用那两个文件里的纯函数与 tip 常量。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	friendpb "proto/friend"
	guildpb "proto/guild"
	"proto/login"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

const (
	orderingGuildLeaderAccount     = "robot_9902" // O5:建临时帮、查待审名单、解散
	orderingGuildApplicantAccount  = "robot_9903" // O5:连发申请 → 撤回
	orderingFriendAccepterAccount  = "robot_9904" // O6:收申请,连发同意 → 删除
	orderingFriendRequesterAccount = "robot_9905" // O6:每轮先发好友申请
	orderingLoginAccount           = "robot_9906" // O7:每轮新连接连发 Login → EnterGame
)

const (
	// O5 / O6 每次运行的轮数:都受每分钟 10 次的申请配额约束(见文件头),5 轮让同一分钟内连跑两次仍不超额。
	orderingGuildRounds  = 5
	orderingFriendRounds = 5
	// O7 的轮数:每轮要新开连接、完整进场再离开,单轮数秒,3 轮把这一组控制在 20 秒左右。
	orderingLoginRounds = 3
	// O7 每条连接断开之后、下一条连接登录之前的间隔。LeaveGame 与断线通知异步落到 login,上一条连接的进场链
	// 放玩家锁也晚于进场推送;不隔开,下一轮 EnterGame 可能撞 kLoginInProgress,把前置问题误报成失败。
	orderingLoginRoundGap = 2 * time.Second
)

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。帮会 / 好友的码复用两个冒烟文件里已有的变量。
var (
	tipOrderingRateLimited     = uint32(tiptable.CommonError_kRateLimitExceeded)
	tipOrderingNotLoggedIn     = uint32(tiptable.LoginError_kLoginSessionNotFound)
	tipOrderingLoginInProgress = uint32(tiptable.LoginError_kLoginInProgress)
	// guild 写事务连续死锁 3 次或锁等待超时(1205 / 1213 归一成写冲突)时回的码(go/guild/internal/constants ErrBusyRetry)。
	tipOrderingGuildBusyRetry = uint32(tiptable.GuildError_kGuildBusyRetry)
)

// orderingSmokePassFunc 是 RunOrderingSmoke 里打 `[ordering-smoke] PASS step=…` 的那个闭包。
type orderingSmokePassFunc func(step, format string, args ...any)

// orderingStepError 把失败步骤带回 RunOrderingSmoke,由它统一打 ORDERING_SMOKE_FAIL。
// reordered 标出乱序类失败(各组与 O3 的 *-order):不中断后面的步骤,O3 之后汇总成一行 FAIL;其余失败立即打 FAIL 并退出。
// 各组不直接调 fail:fail 会 os.Exit,组内收尾连接的 defer 就跑不到了。
type orderingStepError struct {
	step      string
	err       error
	reordered bool
}

func (e *orderingStepError) Error() string { return e.step + ": " + e.err.Error() }

func orderingStepFail(step, format string, args ...any) error {
	return &orderingStepError{step: step, err: fmt.Errorf(format, args...)}
}

// orderingReorderFail 构造乱序类失败:本组(或 O3)全部轮次已跑完、每个乱序轮都已收尾到干净的起点。
func orderingReorderFail(step, format string, args ...any) *orderingStepError {
	return &orderingStepError{step: step, err: fmt.Errorf(format, args...), reordered: true}
}

// orderingReorderSummary 把登记的乱序拼成一行 FAIL 的 step 与 reason:step 是按执行顺序逗号分隔的乱序步骤
// (如 O5-order,O6-order,O3-order),reason 逐条写「步骤: 原因」。
func orderingReorderSummary(reorders []*orderingStepError) (string, string) {
	steps := make([]string, 0, len(reorders))
	reasons := make([]string, 0, len(reorders))
	for _, r := range reorders {
		steps = append(steps, r.step)
		reasons = append(reasons, r.Error())
	}
	return strings.Join(steps, ","), strings.Join(reasons, ";")
}

// orderingTipResponse:三组用到的回包都把业务拒绝码放在响应体 error_message 里(guild / friend / login 同形)。
type orderingTipResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
}

// orderingSmokeCall 是流水线里的一条请求:消息号、请求体,以及解码回包用的响应对象。
type orderingSmokeCall struct {
	messageId uint32
	request   proto.Message
	response  orderingTipResponse
}

// runOrderingSmokeGroups 依次跑 O5、O6、O7,返回登记下的乱序类失败(可能为空)与第一个非乱序失败(nil = 没有)。
// 乱序只登记、接着跑下一组:lane 上线之前每组都大概率乱序,遇到第一个就退出的话一次运行只看得到 O5,
// 证明不了 O6 / O7 与其后的 O3 各自抓得到乱序。登录、清场、业务拒绝、超时、状态对不上任何一种执行顺序这类失败
// 说明前置条件或环境有问题,立即带回由调用方退出;此前登记的乱序已经各打过一行 REORDERED,留在日志里。
// 每组返回前已收尾自己的连接。
func runOrderingSmokeGroups(cfg *config.Config, stats *metrics.Stats, pass orderingSmokePassFunc) ([]*orderingStepError, *orderingStepError) {
	groups := []func(*config.Config, *metrics.Stats, orderingSmokePassFunc) error{
		runOrderingGuildGroup,
		runOrderingFriendGroup,
		runOrderingLoginGroup,
	}
	var reorders []*orderingStepError
	for _, group := range groups {
		err := group(cfg, stats, pass)
		if err == nil {
			continue
		}
		var stepErr *orderingStepError
		if !errors.As(err, &stepErr) {
			stepErr = &orderingStepError{step: "O5-O7", err: err}
		}
		if !stepErr.reordered {
			return reorders, stepErr
		}
		zap.L().Warn(fmt.Sprintf("[ordering-smoke] REORDERED step=%s reason=%s (continuing; summarized into ORDERING_SMOKE_FAIL after O3)",
			stepErr.step, stepErr.err))
		reorders = append(reorders, stepErr)
	}
	return reorders, nil
}

// ---------------------------------------------------------------------------
// O5:ApplyJoinGuild → CancelGuildApplication
// ---------------------------------------------------------------------------

func runOrderingGuildGroup(cfg *config.Config, stats *metrics.Stats, pass orderingSmokePassFunc) error {
	var opened []*orderingSmokeBot
	defer func() { orderingSmokeCloseBots(opened) }()

	leader, err := orderingGroupLogin(cfg, orderingGuildLeaderAccount, stats, &opened)
	if err != nil {
		return orderingStepFail("O5-login", "%v", err)
	}
	applicant, err := orderingGroupLogin(cfg, orderingGuildApplicantAccount, stats, &opened)
	if err != nil {
		return orderingStepFail("O5-login", "%v", err)
	}
	pass("O5-login", "leader=%d applicant=%d", leader.gc.PlayerId, applicant.gc.PlayerId)

	// 清场:两人都回到「不在任何帮会、没有待审申请」,再由帮主建本次运行专用的临时帮(帮名全局唯一,每次换新名)。
	for _, bot := range opened {
		if err := bot.guildCancelAllApplications(); err != nil {
			return orderingStepFail("O5-precheck", "account=%s 撤回遗留申请: %v", bot.account, err)
		}
		if err := bot.guildLeaveAny(); err != nil {
			return orderingStepFail("O5-precheck", "account=%s 退出遗留帮会: %v", bot.account, err)
		}
	}
	created := &guildpb.CreateGuildResponse{}
	if err := leader.expectTip(game.GuildServiceCreateGuildMessageId,
		&guildpb.CreateGuildRequest{Name: "保序" + guildSmokeNonce()}, created, 0); err != nil {
		return orderingStepFail("O5-precheck", "建临时帮: %v", err)
	}
	guildID := created.GetGuild().GetGuildId()
	if guildID == 0 {
		return orderingStepFail("O5-precheck", "建帮受理了,回包里却没有帮会")
	}
	pass("O5-precheck", "guild_id=%d(临时帮),两人都不在其它帮会、没有遗留申请", guildID)

	reordered := make([]int, 0)
	busyRounds := make([]int, 0)
	for round := 1; round <= orderingGuildRounds; round++ {
		// 第 1 轮前也隔开:清场可能刚撤过申请(219),与本轮及乱序收尾同号,三次挤进同一个限流窗口就贴着上限了。
		time.Sleep(orderingSmokeRoundSpacing)
		applyTip, cancelTip, err := applicant.pipeline(
			orderingSmokeCall{game.GuildServiceApplyJoinGuildMessageId,
				&guildpb.ApplyJoinGuildRequest{GuildId: guildID}, &guildpb.ApplyJoinGuildResponse{}},
			orderingSmokeCall{game.GuildServiceCancelGuildApplicationMessageId,
				&guildpb.CancelGuildApplicationRequest{GuildId: guildID}, &guildpb.CancelGuildApplicationResponse{}})
		if err != nil {
			return orderingStepFail("O5-pipeline", "round=%d/%d: %v", round, orderingGuildRounds, err)
		}
		// 申请只有受理一种合法答复;撤回有 0(申请在、已删)与「申请不存在」(撤回执行时申请还没落库)两种。
		// 两条都可能回 kGuildBusyRetry,见下面并发撞锁一支。
		if applyTip != 0 && applyTip != tipOrderingGuildBusyRetry {
			return orderingStepFail("O5-pipeline", "round=%d/%d ApplyJoinGuild 业务拒绝 tip=%d%s",
				round, orderingGuildRounds, applyTip, orderingTipHint(applyTip))
		}
		if cancelTip != 0 && cancelTip != tipGuildApplicationNotFound && cancelTip != tipOrderingGuildBusyRetry {
			return orderingStepFail("O5-pipeline", "round=%d/%d CancelGuildApplication 业务拒绝 tip=%d%s",
				round, orderingGuildRounds, cancelTip, orderingTipHint(cancelTip))
		}
		if applyTip == tipOrderingGuildBusyRetry || cancelTip == tipOrderingGuildBusyRetry {
			// 两条在 guild 上并发执行、两个事务撞锁(guild_manage_repo.go:申请锁帮会行 / 状态行 / 申请行,撤回点锁申请行),
			// 落败的一条回写冲突。同一会话的两条串行执行时互不争锁,所以这与颠倒一样是「没按收包顺序串行执行」,记一轮乱序;
			// 不能按业务拒绝直接失败,否则 lane 上线之前它会挡住后面的组与 O3。哪条落败决定申请在不在,收尾一律撤回再复核。
			reordered = append(reordered, round)
			busyRounds = append(busyRounds, round)
			zap.L().Warn("[ordering-smoke] ApplyJoinGuild and CancelGuildApplication ran concurrently and hit a write conflict",
				zap.Int("round", round), zap.Uint32("apply_tip", applyTip), zap.Uint32("cancel_tip", cancelTip))
			if err := orderingGuildCancelAndVerify(leader, applicant, guildID); err != nil {
				return orderingStepFail("O5-cleanup", "round=%d/%d 并发撞锁后收尾撤回失败: %v", round, orderingGuildRounds, err)
			}
			continue
		}
		pending, err := leader.guildHasApplicant(applicant.gc.PlayerId)
		if err != nil {
			return orderingStepFail("O5-status", "round=%d/%d ListGuildApplications: %v", round, orderingGuildRounds, err)
		}
		switch {
		case !pending && cancelTip == 0:
			// 符合发送顺序:申请后撤回。
		case pending && cancelTip == tipGuildApplicationNotFound:
			// 撤回先于申请执行:撤回时还没有申请行,申请随后落库。撤掉这条申请,下一轮才是干净的起点。
			reordered = append(reordered, round)
			zap.L().Warn("[ordering-smoke] Execution order reversed: CancelGuildApplication ran before ApplyJoinGuild, application is still pending",
				zap.Int("round", round), zap.Uint64("guild_id", guildID))
			if err := orderingGuildCancelAndVerify(leader, applicant, guildID); err != nil {
				return orderingStepFail("O5-cleanup", "round=%d/%d 乱序后收尾撤回失败: %v", round, orderingGuildRounds, err)
			}
		default:
			return orderingStepFail("O5-status", "round=%d/%d CancelGuildApplication tip=%d、帮主侧待审名单里有申请人=%v:"+
				"与任何一种执行顺序都对不上(撤回受理就不该还在名单里,撤回回「申请不存在」就该在),查 guild 日志",
				round, orderingGuildRounds, cancelTip, pending)
		}
	}
	// 收尾:解散临时帮(连带删掉它的全部待审申请)。放在汇总乱序之前,乱序时也留下干净的起点。
	if err := leader.expectTip(game.GuildServiceDisbandGuildMessageId,
		&guildpb.DisbandGuildRequest{}, &guildpb.DisbandGuildResponse{}, 0); err != nil {
		return orderingStepFail("O5-cleanup", "解散临时帮 %d: %v", guildID, err)
	}
	if len(reordered) > 0 {
		return orderingReorderFail("O5-order", "%d/%d 轮没有按发送顺序串行执行,乱序轮次=%v(其中两条并发撞锁、回 kGuildBusyRetry 的轮次=%v,"+
			"其余是先发的 ApplyJoinGuild 晚于后发的 CancelGuildApplication 执行)。"+
			"gate 路由 lane(client-request-ordering.md 批次 C)上线之前这是预期结果;上线之后出现即为缺陷",
			len(reordered), orderingGuildRounds, reordered, busyRounds)
	}
	pass("O5-order", "rounds=%d 全部按发送顺序执行", orderingGuildRounds)
	return nil
}

// orderingGuildCancelAndVerify 是乱序轮的收尾:申请人撤回本帮申请,再按帮主侧待审名单复核已经撤掉。
// 「申请不存在」也接受:并发撞锁那一轮若是申请落败,申请根本没落库;申请明明还在却回这个码时,下面的复核会拦住。
func orderingGuildCancelAndVerify(leader, applicant *orderingSmokeBot, guildID uint64) error {
	tip, err := applicant.rpcTip(game.GuildServiceCancelGuildApplicationMessageId,
		&guildpb.CancelGuildApplicationRequest{GuildId: guildID}, &guildpb.CancelGuildApplicationResponse{})
	if err != nil {
		return err
	}
	if tip != 0 && tip != tipGuildApplicationNotFound {
		return fmt.Errorf("CancelGuildApplication 回 tip=%d%s", tip, orderingTipHint(tip))
	}
	pending, err := leader.guildHasApplicant(applicant.gc.PlayerId)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("撤回(tip=%d)之后帮主侧待审名单里仍有申请人", tip)
	}
	return nil
}

// guildHasApplicant 用帮主侧 ListGuildApplications 查申请人是否还在本帮待审名单里。
// 这是帮会侧的权威查询:repo 直接读 MySQL 申请行(只数未过期的),不经缓存。
func (b *orderingSmokeBot) guildHasApplicant(applicant uint64) (bool, error) {
	resp := &guildpb.ListGuildApplicationsResponse{}
	if err := b.expectTip(game.GuildServiceListGuildApplicationsMessageId,
		&guildpb.ListGuildApplicationsRequest{}, resp, 0); err != nil {
		return false, err
	}
	return guildSmokeHasApplicant(resp.GetApplicants(), applicant), nil
}

// guildCancelAllApplications 撤回本人全部待审申请,再列一次复核已经清空。
// 已入帮的人列不出自己的申请(服务端回空表),但「在册」与「有待审申请」本就互斥,所以先撤后退不影响清理效果。
func (b *orderingSmokeBot) guildCancelAllApplications() error {
	list := func() ([]*guildpb.GuildApplicationView, error) {
		resp := &guildpb.ListMyGuildApplicationsResponse{}
		if err := b.expectTip(game.GuildServiceListMyGuildApplicationsMessageId,
			&guildpb.ListMyGuildApplicationsRequest{}, resp, 0); err != nil {
			return nil, err
		}
		return resp.GetApplications(), nil
	}
	applications, err := list()
	if err != nil {
		return err
	}
	if len(applications) == 0 {
		return nil
	}
	for _, application := range applications {
		tip, err := b.rpcTip(game.GuildServiceCancelGuildApplicationMessageId,
			&guildpb.CancelGuildApplicationRequest{GuildId: application.GetGuildId()}, &guildpb.CancelGuildApplicationResponse{})
		if err != nil {
			return fmt.Errorf("撤回帮会 %d 的申请: %w", application.GetGuildId(), err)
		}
		// 「申请不存在」:列出之后已过期或被处理,目的同样达到了。
		if tip != 0 && tip != tipGuildApplicationNotFound {
			return fmt.Errorf("撤回帮会 %d 的申请回 tip=%d", application.GetGuildId(), tip)
		}
	}
	if applications, err = list(); err != nil {
		return err
	}
	if len(applications) != 0 {
		return fmt.Errorf("撤回之后仍列得出 %d 条待审申请", len(applications))
	}
	return nil
}

// guildLeaveAny 把机器人带回「不在任何帮会」:帮主解散,成员退出,直到 GetPlayerGuild 回「不在帮会」。
func (b *orderingSmokeBot) guildLeaveAny() error {
	for attempt := 0; attempt < 3; attempt++ {
		resp := &guildpb.GetPlayerGuildResponse{}
		tip, err := b.rpcTip(game.GuildServiceGetPlayerGuildMessageId, &guildpb.GetPlayerGuildRequest{}, resp)
		if err != nil {
			return err
		}
		switch {
		case tip == tipGuildNotInGuild:
			return nil
		case tip != 0:
			return fmt.Errorf("GetPlayerGuild tip=%d%s", tip, orderingTipHint(tip))
		case resp.GetGuild().GetLeaderId() == b.gc.PlayerId:
			err = b.expectTip(game.GuildServiceDisbandGuildMessageId, &guildpb.DisbandGuildRequest{}, &guildpb.DisbandGuildResponse{}, 0)
		default:
			err = b.expectTip(game.GuildServiceLeaveGuildMessageId, &guildpb.LeaveGuildRequest{}, &guildpb.LeaveGuildResponse{}, 0)
		}
		if err != nil {
			return fmt.Errorf("退出 / 解散遗留帮会 %d: %w", resp.GetGuild().GetGuildId(), err)
		}
	}
	return errors.New("连续 3 次退出 / 解散后仍在帮会里")
}

// ---------------------------------------------------------------------------
// O6:AcceptFriend → RemoveFriend
// ---------------------------------------------------------------------------

func runOrderingFriendGroup(cfg *config.Config, stats *metrics.Stats, pass orderingSmokePassFunc) error {
	var opened []*orderingSmokeBot
	defer func() { orderingSmokeCloseBots(opened) }()

	accepter, err := orderingGroupLogin(cfg, orderingFriendAccepterAccount, stats, &opened)
	if err != nil {
		return orderingStepFail("O6-login", "%v", err)
	}
	requester, err := orderingGroupLogin(cfg, orderingFriendRequesterAccount, stats, &opened)
	if err != nil {
		return orderingStepFail("O6-login", "%v", err)
	}
	pass("O6-login", "accepter=%d requester=%d", accepter.gc.PlayerId, requester.gc.PlayerId)

	if err := orderingFriendReset(accepter, requester); err != nil {
		return orderingStepFail("O6-precheck", "%v", err)
	}
	pass("O6-precheck", "两人互不是好友,接收方没有来自申请方的待处理申请")

	reordered := make([]int, 0)
	for round := 1; round <= orderingFriendRounds; round++ {
		// 第 1 轮前也隔开:清场必发一次 RemoveFriend(11),与本轮及乱序收尾同号,不隔开会贴着每秒 5 次的上限。
		time.Sleep(orderingSmokeRoundSpacing)
		added := &friendpb.AddFriendResponse{}
		if err := requester.expectTip(game.ClientPlayerFriendAddFriendMessageId,
			&friendpb.AddFriendRequest{TargetPlayerId: accepter.gc.PlayerId}, added, 0); err != nil {
			return orderingStepFail("O6-request", "round=%d/%d: %v", round, orderingFriendRounds, err)
		}
		if added.GetRequestId() == 0 {
			return orderingStepFail("O6-request", "round=%d/%d 好友申请受理了却没有返回 request_id", round, orderingFriendRounds)
		}
		acceptTip, removeTip, err := accepter.pipeline(
			orderingSmokeCall{game.ClientPlayerFriendAcceptFriendMessageId,
				&friendpb.AcceptFriendRequest{RequestId: added.GetRequestId(), FromPlayerId: requester.gc.PlayerId},
				&friendpb.AcceptFriendResponse{}},
			orderingSmokeCall{game.ClientPlayerFriendRemoveFriendMessageId,
				&friendpb.RemoveFriendRequest{TargetPlayerId: requester.gc.PlayerId}, &friendpb.RemoveFriendResponse{}})
		if err != nil {
			return orderingStepFail("O6-pipeline", "round=%d/%d: %v", round, orderingFriendRounds, err)
		}
		if acceptTip != 0 || removeTip != 0 {
			return orderingStepFail("O6-pipeline", "round=%d/%d 业务拒绝:AcceptFriend tip=%d,RemoveFriend tip=%d(两种执行顺序下两条都该受理)",
				round, orderingFriendRounds, acceptTip, removeTip)
		}
		accepterSees, requesterSees, err := orderingFriendViews(accepter, requester)
		if err != nil {
			return orderingStepFail("O6-status", "round=%d/%d GetFriendList: %v", round, orderingFriendRounds, err)
		}
		switch {
		case !accepterSees && !requesterSees:
			// 符合发送顺序:同意后删除。
		case accepterSees && requesterSees:
			// 删除先于同意执行:删除时还不是好友(幂等成功),同意随后建出双向好友边。删掉它,下一轮才是干净的起点。
			reordered = append(reordered, round)
			zap.L().Warn("[ordering-smoke] Execution order reversed: RemoveFriend ran before AcceptFriend, players are still friends",
				zap.Int("round", round), zap.Uint64("request_id", added.GetRequestId()))
			if err := orderingFriendRemoveAndVerify(accepter, requester); err != nil {
				return orderingStepFail("O6-cleanup", "round=%d/%d 乱序后收尾删除失败: %v", round, orderingFriendRounds, err)
			}
		default:
			return orderingStepFail("O6-status", "round=%d/%d 好友边单边残留:接收方看得到申请方=%v,申请方看得到接收方=%v。"+
				"双向两行本该在同一事务里同增同减,与执行顺序无关,查 friend 日志", round, orderingFriendRounds, accepterSees, requesterSees)
		}
	}
	if len(reordered) > 0 {
		return orderingReorderFail("O6-order", "%d/%d 轮执行顺序被颠倒(先发的 AcceptFriend 晚于后发的 RemoveFriend 执行),乱序轮次=%v。"+
			"gate 路由 lane(client-request-ordering.md 批次 C)上线之前这是预期结果;上线之后出现即为缺陷",
			len(reordered), orderingFriendRounds, reordered)
	}
	pass("O6-order", "rounds=%d 全部按发送顺序执行", orderingFriendRounds)
	return nil
}

// orderingFriendReset 把好友组两人恢复成「互不是好友、接收方没有来自申请方的待处理申请」,再按双方好友列表复核。
// 拉黑不在这里清:本场景从不拉黑,出现拉黑只能是账号被别的流程用过,AddFriend 会回 kFriendBlocked 并在失败原因里写明。
func orderingFriendReset(accepter, requester *orderingSmokeBot) error {
	pending := &friendpb.GetPendingRequestsResponse{}
	if err := accepter.expectTip(game.ClientPlayerFriendGetPendingRequestsMessageId,
		&friendpb.GetPendingRequestsRequest{}, pending, 0); err != nil {
		return fmt.Errorf("拉取待处理申请: %w", err)
	}
	for _, request := range pending.GetRequests() {
		// request_id 为 0 的旧行拒绝不了(RejectFriend 要求非 0);留着也无妨:下一次 AddFriend 会给它补发代次并原样返回。
		if request.GetFromPlayerId() != requester.gc.PlayerId || request.GetRequestId() == 0 ||
			request.GetStatus() != friendpb.FriendRequestStatus_FRIEND_REQUEST_PENDING {
			continue
		}
		tip, err := accepter.rpcTip(game.ClientPlayerFriendRejectFriendMessageId,
			&friendpb.RejectFriendRequest{RequestId: request.GetRequestId()}, &friendpb.RejectFriendResponse{})
		if err != nil {
			return fmt.Errorf("拒绝遗留申请 request_id=%d: %w", request.GetRequestId(), err)
		}
		// 「没有待处理申请」:列出之后已被处理或过期,目的同样达到了。
		if tip != 0 && tip != tipFriendNoPendingRequest {
			return fmt.Errorf("拒绝遗留申请 request_id=%d 回 tip=%d", request.GetRequestId(), tip)
		}
	}
	if err := orderingFriendRemoveAndVerify(accepter, requester); err != nil {
		return fmt.Errorf("删除遗留好友关系: %w", err)
	}
	return nil
}

// orderingFriendRemoveAndVerify 由接收方删掉与申请方的好友关系(不是好友时是幂等成功),再按双方好友列表复核。
func orderingFriendRemoveAndVerify(accepter, requester *orderingSmokeBot) error {
	if err := accepter.expectTip(game.ClientPlayerFriendRemoveFriendMessageId,
		&friendpb.RemoveFriendRequest{TargetPlayerId: requester.gc.PlayerId}, &friendpb.RemoveFriendResponse{}, 0); err != nil {
		return err
	}
	accepterSees, requesterSees, err := orderingFriendViews(accepter, requester)
	if err != nil {
		return err
	}
	if accepterSees || requesterSees {
		return fmt.Errorf("删除受理之后仍是好友(接收方看得到申请方=%v,申请方看得到接收方=%v)", accepterSees, requesterSees)
	}
	return nil
}

// orderingFriendViews 从双方视角各查一次好友列表(GetFriendList 直接读 MySQL 好友边),返回各自是否看得到对方。
func orderingFriendViews(accepter, requester *orderingSmokeBot) (accepterSees, requesterSees bool, err error) {
	if accepterSees, err = accepter.friendSees(requester.gc.PlayerId); err != nil {
		return false, false, fmt.Errorf("account=%s: %w", accepter.account, err)
	}
	if requesterSees, err = requester.friendSees(accepter.gc.PlayerId); err != nil {
		return false, false, fmt.Errorf("account=%s: %w", requester.account, err)
	}
	return accepterSees, requesterSees, nil
}

func (b *orderingSmokeBot) friendSees(target uint64) (bool, error) {
	list := &friendpb.GetFriendListResponse{}
	if err := b.expectTip(game.ClientPlayerFriendGetFriendListMessageId, &friendpb.GetFriendListRequest{}, list, 0); err != nil {
		return false, err
	}
	return friendSmokeFindFriend(list.GetFriends(), target) != nil, nil
}

// ---------------------------------------------------------------------------
// O7:Login → EnterGame
// ---------------------------------------------------------------------------

func runOrderingLoginGroup(cfg *config.Config, stats *metrics.Stats, pass orderingSmokePassFunc) error {
	// 引导:完整登录一次拿到 player_id(首次运行顺带建角),随即离开。流水线里发 EnterGame 时 Login 的回包
	// (角色列表)还没到,EnterGame 要带的 player_id 只能事先知道。
	boot, err := orderingSmokeLogin(cfg, orderingLoginAccount, stats)
	if err != nil {
		return orderingStepFail("O7-bootstrap", "account=%s zone=%d: %v", orderingLoginAccount, cfg.ZoneID, err)
	}
	playerID := boot.gc.PlayerId
	orderingSmokeCloseBot(boot)
	pass("O7-bootstrap", "player_id=%d", playerID)

	reordered := make([]int, 0)
	for round := 1; round <= orderingLoginRounds; round++ {
		time.Sleep(orderingLoginRoundGap)
		enterRanFirst, err := orderingLoginRound(cfg, playerID, stats, round)
		if err != nil {
			return err
		}
		if enterRanFirst {
			reordered = append(reordered, round)
			zap.L().Warn("[ordering-smoke] Execution order reversed: EnterGame ran before Login and was refused as not logged in",
				zap.Int("round", round), zap.Uint64("player_id", playerID))
		}
	}
	if len(reordered) > 0 {
		return orderingReorderFail("O7-order", "%d/%d 轮执行顺序被颠倒(后发的 EnterGame 先于 Login 执行,被判「未登录」tip=%d),乱序轮次=%v。"+
			"gate 路由 lane(client-request-ordering.md 批次 C)上线之前这是预期结果;上线之后出现即为缺陷",
			len(reordered), orderingLoginRounds, tipOrderingNotLoggedIn, reordered)
	}
	pass("O7-order", "rounds=%d 全部按发送顺序执行(EnterGame 都在 Login 之后执行并进场)", orderingLoginRounds)
	return nil
}

// orderingLoginRound 新开一条连接,不等 Login 回包连发 Login → EnterGame。返回 true 表示 EnterGame 先于 Login 执行
// (被判「未登录」);顺序执行时还要等到进场推送才算这一轮通过。连接无论成败都在返回前断开。
func orderingLoginRound(cfg *config.Config, playerID uint64, stats *metrics.Stats, round int) (bool, error) {
	bot, err := orderingSmokeConnect(cfg, orderingLoginAccount, playerID, stats)
	if err != nil {
		return false, orderingStepFail("O7-connect", "round=%d/%d: %v", round, orderingLoginRounds, err)
	}
	defer orderingSmokeCloseBot(bot)

	loginRequest, err := buildScenarioLoginRequest(orderingLoginAccount, cfg.Password)
	if err != nil {
		return false, orderingStepFail("O7-pipeline", "round=%d/%d 构造 LoginRequest: %v", round, orderingLoginRounds, err)
	}
	loginResp := &login.LoginResponse{}
	loginTip, enterTip, err := bot.pipeline(
		orderingSmokeCall{game.ClientPlayerLoginLoginMessageId, loginRequest, loginResp},
		orderingSmokeCall{game.ClientPlayerLoginEnterGameMessageId,
			&login.EnterGameRequest{PlayerId: playerID}, &login.EnterGameResponse{}})
	if err != nil {
		return false, orderingStepFail("O7-pipeline", "round=%d/%d: %v", round, orderingLoginRounds, err)
	}
	if loginTip != 0 {
		return false, orderingStepFail("O7-pipeline", "round=%d/%d Login 业务拒绝 tip=%d%s",
			round, orderingLoginRounds, loginTip, orderingTipHint(loginTip))
	}
	if !orderingLoginListsPlayer(loginResp, playerID) {
		return false, orderingStepFail("O7-pipeline", "round=%d/%d Login 回包的角色列表里没有引导登录拿到的 player_id=%d",
			round, orderingLoginRounds, playerID)
	}
	switch enterTip {
	case 0:
	case tipOrderingNotLoggedIn:
		return true, nil
	default:
		return false, orderingStepFail("O7-pipeline", "round=%d/%d EnterGame 业务拒绝 tip=%d%s",
			round, orderingLoginRounds, enterTip, orderingTipHint(enterTip))
	}
	// EnterGame 无错只表示「已受理」(entergamelogic.go 对客户端的契约),进场成功以收到进场推送为准。
	waitCtx, waitCancel := context.WithTimeout(context.Background(), orderingSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		return false, orderingStepFail("O7-enter", "round=%d/%d EnterGame 受理了,但 %s 内没有收到进场推送"+
			"(进场链失败时 gate 会推 SendTipToClient kEnterSceneFailed,本场景把它记成一行 [ordering-smoke] Gate pushed SendTipToClient;"+
			"login 日志查 EnterGame preload / apply session failed): %v",
			round, orderingLoginRounds, orderingSmokeSceneReadyTimeout, err)
	}
	return false, nil
}

func orderingLoginListsPlayer(resp *login.LoginResponse, playerID uint64) bool {
	for _, wrapper := range resp.GetPlayers() {
		if wrapper.GetPlayer().GetPlayerId() == playerID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 连接与请求的公共部分(请求都经 orderingSmokeBot.callAll 发出并按请求号认领回包)
// ---------------------------------------------------------------------------

// orderingGroupLogin 登录一个组内账号并登记到 opened,让组的 defer 无论成败都能收尾它。
func orderingGroupLogin(cfg *config.Config, account string, stats *metrics.Stats, opened *[]*orderingSmokeBot) (*orderingSmokeBot, error) {
	bot, err := orderingSmokeLogin(cfg, account, stats)
	if err != nil {
		return nil, fmt.Errorf("account=%s zone=%d: %w", account, cfg.ZoneID, err)
	}
	*opened = append(*opened, bot)
	return bot, nil
}

// orderingSmokeConnect 走 AssignGate → 连接验票,挂上 orderingSmokeBot 的收包循环,但**不登录**:
// O7 要在这条新连接上自己连发 Login → EnterGame。playerID 预先填进连接与 PlayerList:
// 通用分发按 client.PlayerId 找 Player,进场推送(NotifyEnterScene)才记得到这个 Player 上。
func orderingSmokeConnect(cfg *config.Config, account string, playerID uint64, stats *metrics.Stats) (*orderingSmokeBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad gate port %q: %w", portStr, err)
	}
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("connect gate: %w", err)
	}
	gc.PlayerId = playerID
	bot := &orderingSmokeBot{
		account: account,
		gate:    host + ":" + portStr,
		gc:      gc,
		player:  gameobject.NewPlayer(playerID),
		stats:   stats,
	}
	gameobject.PlayerList.Set(playerID, bot.player)
	go gc.RecvLoop(bot.onMessage)
	return bot, nil
}

// orderingSmokeCloseBot 收尾一条组内连接:离开游戏、提示 login 断线、注销 Player、关连接(与 guild / friend 冒烟的 cleanup 同序)。
func orderingSmokeCloseBot(bot *orderingSmokeBot) {
	_ = leaveGame(bot.gc, bot.stats)
	gameobject.PlayerList.Delete(bot.gc.PlayerId)
	bot.gc.Close()
}

func orderingSmokeCloseBots(bots []*orderingSmokeBot) {
	for _, bot := range bots {
		orderingSmokeCloseBot(bot)
	}
}

// pipeline 不等回包连发 first → second(模拟客户端连点),等两条各自的回包(按请求号认领,与到达顺序无关),
// 按发送顺序返回两条的业务 tip,回包体解进各自的 response。
// 任一条被 gate / 路由服在信封上拒绝都按 error 返回:请求没到业务逻辑,本轮的顺序无从判断。
func (b *orderingSmokeBot) pipeline(first, second orderingSmokeCall) (uint32, uint32, error) {
	replies, err := b.callAll(
		orderingSmokeRequest{first.messageId, first.request},
		orderingSmokeRequest{second.messageId, second.request})
	if err != nil {
		return 0, 0, err
	}
	firstTip, err := orderingBusinessTip(replies[0], first.response)
	if err != nil {
		return 0, 0, err
	}
	secondTip, err := orderingBusinessTip(replies[1], second.response)
	if err != nil {
		return 0, 0, err
	}
	return firstTip, secondTip, nil
}

// rpcTip 发一条请求并只等它自己的回包,返回业务 tip(0 = 受理)。
func (b *orderingSmokeBot) rpcTip(messageId uint32, request proto.Message, response orderingTipResponse) (uint32, error) {
	replies, err := b.callAll(orderingSmokeRequest{messageId, request})
	if err != nil {
		return 0, err
	}
	return orderingBusinessTip(replies[0], response)
}

// expectTip 发一条请求并断言业务 tip 等于 want。
func (b *orderingSmokeBot) expectTip(messageId uint32, request proto.Message, response orderingTipResponse, want uint32) error {
	got, err := b.rpcTip(messageId, request, response)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("message_id=%d account=%s 期望 tip=%d,实得 tip=%d%s", messageId, b.account, want, got, orderingTipHint(got))
	}
	return nil
}

// orderingBusinessTip 解出 callAll 认领到的一条回包的业务 tip(0 = 受理),回包体解进 response。
// 信封拒绝(body 为空)按 error 返回:它说明请求没到业务逻辑,不能当成业务码去断言。
func orderingBusinessTip(record orderingSmokeRecord, response orderingTipResponse) (uint32, error) {
	if record.envelopeTip != 0 {
		return 0, fmt.Errorf("message_id=%d 被 gate / 路由服拒绝(信封 tip=%d:gate 限流、lane 满、排队超时或路由服 / 后端不可用,请求没到业务逻辑)",
			record.messageId, record.envelopeTip)
	}
	if err := proto.Unmarshal(record.body, response); err != nil {
		return 0, fmt.Errorf("解析 message_id=%d 的回包: %w", record.messageId, err)
	}
	return response.GetErrorMessage().GetId(), nil
}

// orderingTipHint 把几种环境类的业务拒绝码翻成可读原因,免得把前置条件问题读成顺序问题。只用于业务码:
// 信封上的 kRateLimitExceeded 是 gate 限流,与这里的每分钟申请配额不是一回事。
func orderingTipHint(tip uint32) string {
	switch tip {
	case tipOrderingRateLimited:
		return "(撞上每分钟申请配额:guild ApplicationQuotaPerMinute / friend RequestQuotaPerMinute 默认 10,固定 60s 窗口;" +
			"本场景每次运行各用 5 次,同一分钟内最多连跑两次,等一分钟再跑)"
	case tipGuildHomeZoneUnknown:
		return "(data_service 里没有该角色的归属区映射;换一个首次在 zone_id 建角的 robot_ 账号,或跑 tools/merge_zone -backfill-home-zone)"
	case tipGuildNotFound:
		return "(申请人与帮会不在同一归属区时也回这个码:帮会两人都要首次在 zone_id 建角)"
	case tipFriendBlocked:
		return "(两人之间有拉黑关系:本场景从不拉黑,说明账号被别的流程用过,Unblock 后重跑)"
	case tipOrderingLoginInProgress:
		return "(同账号的登录 / 进场锁还没放:上一条连接的进场链未收尾,或有别的进程在用这个账号)"
	}
	return ""
}
