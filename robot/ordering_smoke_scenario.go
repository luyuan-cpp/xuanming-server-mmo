package main

// ordering-smoke 场景:验证「同一会话经路由服的请求按收包顺序执行」
// (docs/design/client-request-ordering.md §1.3 / §12 批次 R):
//
//	O1 登录进场;
//	O2 清场:GetQueueStatus(153)必须是 NOT_QUEUED;还在排队(上一轮残留)直接失败并说明,不自动取消;
//	O3 先走一次地域探测(network_probe.go),再重复 orderingSmokeRounds 轮:**不等回包**,连发 JoinQueue(157,1V1)
//	   → CancelQueue(148,与这次 Join 共用同一个 request_id),等两条回包都到,再带 request_id 查 GetQueueStatus。
//	   客户端的意图是「加入后取消」,所以最终必须不在队列里,且撤销墓碑已提交(request_cancelled)。
//	   撤销在 SQL 留永久墓碑(go/match authority.CancelIntent):执行顺序被颠倒(Cancel 先提交,或插在 Join 的两段事务之间)时,
//	   Join 被墓碑挡下、回 kMatchInternal,玩家同样不在队列里,所以这一轮不能靠「停在 QUEUED」发现,判据是
//	   「Join 被拒 + request_cancelled」;本轮记为乱序。万一停在 QUEUED(墓碑没挡住),同样记乱序,并带 Join 回包里的票号收尾。
//
// 判据:任何一轮 Join 被墓碑挡下或停在 QUEUED 即失败。gate 路由 lane(批次 C)上线之前,路由服按条随机选副本、业务服按条并发执行,
// 本场景**预期会间歇失败**(这正是它要证明的缺陷);上线之后必须每轮都通过。单副本也可能失败:同一 HTTP/2 连接上的
// 两个 unary 在服务端是两个 goroutine 并发执行(§1.2)。
//
// O5–O7(ordering_smoke_groups.go):帮会 ApplyJoinGuild → CancelGuildApplication、好友 AcceptFriend → RemoveFriend、
// 登录 Login → EnterGame 三组,各用独立账号(robot_9902–9906)与连接,在 O1 之前跑完、不碰主机器人;判定方式与 O3 相同,
// 细节见该文件头。各组与 O3 的乱序不当场退出,O3 之后汇总成一行 FAIL,step 是逗号分隔的乱序步骤
// (如 step=O5-order,O3-order);其余失败仍立即退出。O4 编号留给另一条分支正在加的「LeaveGame → JoinQueue」:
// 它让主机器人离开游戏,所以必须是主机器人的最后一步,这条约束只管主机器人(robot_9901)。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `ORDERING_SMOKE_OK player_id=… rounds=…`,退出码 0;
//	任一步失败 → `ORDERING_SMOKE_FAIL step=… reason=…`,退出码 1。每步通过另打一行 `[ordering-smoke] PASS step=…`。
//
// 不照做就会假绿或误报的几件事:
//   - 必须是路由模式(GATE_CLIENT_RPC_ROUTER=1):直连模式下 match 不回写会话头,gate 收不到 match 的回包
//     (client-request-ordering.md §7.2 第 1 条),O2 就会超时;直连模式本方案也不保序(O7);
//   - 环境里不能有别的玩家在排 1V1:1V1 两人凑对,另一个人在队列里时,本机器人可能在 Join 与 Cancel 之间被凑走,
//     Cancel 回「太迟」、状态变成 MATCHED,本场景按前置条件不满足失败(reason 里写明);
//   - battle 公网端口要从 robot 所在机器可达:O3 入队前要探测,拨不通报 O3-pipeline;
//   - 限流:157 / 148 / 153 都不在 MessageLimiter 表里,吃 gate 默认档(每个消息号滑动 1 秒内最多 3 次)。每轮每个号
//     各发 1 次(乱序轮多 1 次 148),轮间隔 orderingSmokeRoundSpacing 保证不超限。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/match"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。99xx 与其它冒烟的账号段错开
// (battle 9001-9004、chat 9005/9006、属性 9101、宠物 9102、guild 92xx、team 93xx、trade 94xx、
// 资产通道 95xx、travel 9601、friend 97xx、channel 9801),避免并行冒烟互相顶号。账号不能在任何队伍里。
const orderingSmokeAccount = "robot_9901"

const (
	// 轮数:乱序是概率事件,轮数越多越容易在 lane 上线前暴露。20 轮约 30 秒(间隔 1.1s × 20 加往返)。
	orderingSmokeRounds = 20
	// 两轮之间的间隔:每个消息号每轮至多 2 次(148 在乱序轮会多发一次收尾),1.1s 保证滑动 1 秒内不超过 3 次。
	orderingSmokeRoundSpacing = 1100 * time.Millisecond
	// 每条请求等回包的预算(gc.Call 的超时,从发出时起算):正常是毫秒级。按不变式 I4(client-request-ordering.md §5.2)
	// 不得低于 gate→路由服 deadline 8000(base_deploy_config.yaml)+ lane 排队上限 kMaxLaneWait 5000 + 1000:
	// 流水线的第二条在 lane 里排在第一条完成之后才发出,定时器却从客户端发出时就起算。与大厅 GameClient.RpcTimeoutSec 同为 15s。
	orderingSmokeReplyTimeout = 15 * time.Second
	// 首登等进场的预算,与 channel-smoke / team-smoke 同值。
	orderingSmokeSceneReadyTimeout = 15 * time.Second
)

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipOrderingAlreadyQueued = uint32(tiptable.MatchError_kMatchAlreadyQueued)
	tipOrderingMatchInternal = uint32(tiptable.MatchError_kMatchInternal)
)

var errOrderingSmokeTimeout = errors.New("在等待窗口内没有收到回包")

// orderingSmokeRecord 是 callAll 认领到的一条回包。gate / 路由服级拒绝(限流 / lane 满 / 排队超时 / 无路由服 /
// 后端失败)走带请求号的信封 error_message:envelopeTip 非 0,body 为空。
type orderingSmokeRecord struct {
	messageId   uint32
	envelopeTip uint32
	body        []byte
}

// orderingSmokeRequest 是 callAll 的一条请求。
type orderingSmokeRequest struct {
	messageId uint32
	request   proto.Message
}

// orderingSmokeOutcome 是一次 gc.Call 的结局,形状与 done 的参数相同。
type orderingSmokeOutcome struct {
	reply *base.MessageContent
	err   error
}

type orderingSmokeBot struct {
	account string
	gate    string
	gc      *pkg.GameClient
	player  *gameobject.Player
	stats   *metrics.Stats

	// mu 保护 abort:主流程在 callAll 里写,RecvLoop goroutine 在 onGateTip 里读。
	mu    sync.Mutex
	abort context.CancelCauseFunc // 正在进行的 callAll;nil = 没在等
}

// RunOrderingSmoke 是 main.go `mode: ordering-smoke` 的入口。任一步失败以退出码 1 结束进程。
func RunOrderingSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	var bot *orderingSmokeBot
	cleanup := func() {
		if bot == nil {
			return
		}
		_ = leaveGame(bot.gc, stats)
		gameobject.PlayerList.Delete(bot.gc.PlayerId)
		bot.gc.Close()
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("ORDERING_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	pass := func(step, format string, args ...any) {
		zap.L().Info(fmt.Sprintf("[ordering-smoke] PASS step=%s %s", step, fmt.Sprintf(format, args...)))
	}

	// ---- O5–O7:独立账号的三组流水线(ordering_smoke_groups.go),不碰主机器人,先于 O1 跑完 ----
	// 乱序只登记,O3 之后与 O3 的乱序汇总成一行 FAIL;其余失败立即退出(见 runOrderingSmokeGroups)。
	reorderFailures, groupErr := runOrderingSmokeGroups(cfg, stats, pass)
	if groupErr != nil {
		fail(groupErr.step, "%v", groupErr.err)
	}

	// ---- O1:登录进场 ----
	var err error
	bot, err = orderingSmokeLogin(cfg, orderingSmokeAccount, stats)
	if err != nil {
		fail("O1-login", "account=%s zone=%d err=%v", orderingSmokeAccount, cfg.ZoneID, err)
	}
	pass("O1-login", "player_id=%d gate=%s", bot.gc.PlayerId, bot.gate)

	// ---- O2:清场 ----
	state, err := bot.queueState()
	if err != nil {
		fail("O2-precheck", "GetQueueStatus: %v(需要路由模式 GATE_CLIENT_RPC_ROUTER=1,见文件头)", err)
	}
	if state != match.QueueState_QUEUE_STATE_NOT_QUEUED && state != match.QueueState_QUEUE_STATE_UNSPECIFIED {
		fail("O2-precheck", "进场后队列状态是 %s,需要先清掉上一轮残留(QUEUED 时等票据 TTL 或手工取消;MATCHED / READY 时等那一局收尾)", state)
	}
	pass("O2-precheck", "queue_state=%s", state)

	// ---- O3:流水线连发 Join → Cancel ----
	reordered := make([]int, 0)
	for round := 1; round <= orderingSmokeRounds; round++ {
		if round > 1 {
			time.Sleep(orderingSmokeRoundSpacing)
		}
		pipeline, err := bot.pipelineJoinThenCancel()
		if err != nil {
			fail("O3-pipeline", "round=%d/%d: %v", round, orderingSmokeRounds, err)
		}
		status, err := bot.queueStatus(pipeline.requestId)
		if err != nil {
			fail("O3-status", "round=%d/%d GetQueueStatus: %v", round, orderingSmokeRounds, err)
		}
		switch status.GetState() {
		case match.QueueState_QUEUE_STATE_NOT_QUEUED, match.QueueState_QUEUE_STATE_UNSPECIFIED:
			// Cancel 带原请求编号,无论先后都留墓碑。查不到说明撤销没生效,Join 被拒也就不能归因于它。
			if !status.GetRequestCancelled() {
				fail("O3-status", "round=%d/%d Cancel replied but request_cancelled=false, join_error_code=%d (the cancel tombstone was not committed)",
					round, orderingSmokeRounds, pipeline.joinErrorCode)
			}
			if pipeline.joinErrorCode != 0 {
				// 墓碑先于 Join 提交,Join 被挡下:执行顺序与发送顺序相反。玩家不在队列里,不用收尾。
				reordered = append(reordered, round)
				zap.L().Warn("[ordering-smoke] Execution order reversed: Cancel committed before Join, the tombstone rejected Join",
					zap.Int("round", round), zap.Uint32("join_error_code", pipeline.joinErrorCode))
			}
			// 否则符合发送顺序:加入后取消。
		case match.QueueState_QUEUE_STATE_QUEUED:
			// 墓碑本该挡住 Join,仍在排队说明顺序或撤销出了问题。带 Join 回包里的票号收尾,下一轮才是干净的起点。
			reordered = append(reordered, round)
			zap.L().Warn("[ordering-smoke] Player is still queued after Join and Cancel both replied",
				zap.Int("round", round), zap.String("ticket", pipeline.ticket))
			if err := bot.cancelWithTicket(pipeline.ticket); err != nil {
				fail("O3-cleanup", "round=%d/%d 乱序后收尾取消失败 ticket=%s: %v", round, orderingSmokeRounds, pipeline.ticket, err)
			}
		default:
			fail("O3-status", "round=%d/%d 队列状态 %s:环境里有别的玩家在排 1V1,本机器人在 Join 与 Cancel 之间被凑走(前置条件不满足,见文件头)",
				round, orderingSmokeRounds, status.GetState())
		}
	}
	if len(reordered) > 0 {
		reorderFailures = append(reorderFailures, orderingReorderFail("O3-order", "%d/%d 轮执行顺序被颠倒(先发的 JoinQueue 晚于后发的 CancelQueue 执行),乱序轮次=%v。"+
			"gate 路由 lane(client-request-ordering.md 批次 C)上线之前这是预期结果;上线之后出现即为缺陷",
			len(reordered), orderingSmokeRounds, reordered))
	} else {
		pass("O3-order", "rounds=%d 全部按发送顺序执行", orderingSmokeRounds)
	}
	// O5–O7 与 O3 的乱序在这里汇总成一行 FAIL:一次运行就能看到每组各自是否抓到乱序。
	if len(reorderFailures) > 0 {
		step, reason := orderingReorderSummary(reorderFailures)
		fail(step, "%s", reason)
	}

	zap.L().Info(fmt.Sprintf("ORDERING_SMOKE_OK player_id=%d rounds=%d", bot.gc.PlayerId, orderingSmokeRounds))
	cleanup()
	_ = zap.L().Sync()
}

func orderingSmokeLogin(cfg *config.Config, account string, stats *metrics.Stats) (*orderingSmokeBot, error) {
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
	if err := loginAndEnterScenario(gc, cfg.Password, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("login and enter: %w", err)
	}

	bot := &orderingSmokeBot{
		account: account,
		gate:    host + ":" + portStr,
		gc:      gc,
		player:  gameobject.NewPlayer(gc.PlayerId),
		stats:   stats,
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), orderingSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready(account=%s): %w", account, err)
	}
	return bot, nil
}

// orderingSmokeIsRequestMessage:本场景经 callAll 发出的请求消息号。它们的回包带请求号,由 GameClient 交给 callAll,
// 不进 onMessage;同号却 id = 0 的消息由 onMessage 丢弃。
func orderingSmokeIsRequestMessage(messageId uint32) bool {
	switch messageId {
	case game.MatchServiceJoinQueueMessageId,
		game.MatchServiceCancelQueueMessageId,
		game.MatchServiceGetQueueStatusMessageId:
		return true
	// O5–O7(ordering_smoke_groups.go)发的帮会 / 好友 / 登录请求。两边的推送(NotifyGuildChanged、NotifyFriendEvent)
	// 不是任何请求的回包,不在此列,照旧交给通用分发。
	case game.GuildServiceCreateGuildMessageId,
		game.GuildServiceGetPlayerGuildMessageId,
		game.GuildServiceLeaveGuildMessageId,
		game.GuildServiceDisbandGuildMessageId,
		game.GuildServiceApplyJoinGuildMessageId,
		game.GuildServiceCancelGuildApplicationMessageId,
		game.GuildServiceListMyGuildApplicationsMessageId,
		game.GuildServiceListGuildApplicationsMessageId,
		game.ClientPlayerFriendAddFriendMessageId,
		game.ClientPlayerFriendAcceptFriendMessageId,
		game.ClientPlayerFriendRejectFriendMessageId,
		game.ClientPlayerFriendRemoveFriendMessageId,
		game.ClientPlayerFriendGetFriendListMessageId,
		game.ClientPlayerFriendGetPendingRequestsMessageId,
		game.ClientPlayerLoginLoginMessageId,
		game.ClientPlayerLoginEnterGameMessageId:
		return true
	}
	return false
}

// onMessage:本场景请求的回包带请求号,由 GameClient 交给 callAll 的 done,不会到这里;同号却 id = 0 的消息
// 记一行后丢弃,不交给通用分发(信封拒绝会被解成全零响应,看上去像受理)。gate 的 SendTipToClient 先交给 onGateTip;
// 推送(进场推送等)照旧交给通用分发更新 Player。
func (b *orderingSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	if orderingSmokeIsRequestMessage(msg.GetMessageId()) {
		smokeDropUncorrelatedReply(b.account, msg)
		return
	}
	if msg.GetMessageId() == game.SceneClientPlayerCommonSendTipToClientMessageId {
		b.onGateTip(msg)
	}
	handler.MessageBodyHandler(client, msg)
}

// onGateTip 处理 gate 推的 SendTipToClient(RecvLoop goroutine 上调用)。它不带请求号,不再代表「某一条请求失败」:
// gate / 路由服对一条请求的拒绝与失败,直连与路由两种模式现在都回带请求号的信封错误,由 callAll 按条认领。
// 只有 kServiceUnavailable 让正在进行的 callAll 立刻失败:gate 转发失败却认领不到请求(既不在路由 lane、也不是直连调用)
// 时推的是它,对应的请求不会再有回包,不中止就只能等满超时。其余码(如进场链失败的 kEnterSceneFailed)只记一行留作诊断。
func (b *orderingSmokeBot) onGateTip(msg *base.MessageContent) {
	tipInfo := &base.TipInfoMessage{}
	if err := proto.Unmarshal(msg.GetSerializedMessage(), tipInfo); err != nil || tipInfo.GetId() == 0 {
		return
	}
	tip := tipInfo.GetId()
	zap.L().Warn("[ordering-smoke] Gate pushed SendTipToClient, which carries no request id",
		zap.String("account", b.account), zap.Uint32("tip", tip))
	if tip != smokeTipServiceUnavailable {
		return
	}
	b.mu.Lock()
	abort := b.abort
	b.mu.Unlock()
	if abort != nil {
		abort(fmt.Errorf("收到 gate 的失败回执 SendTipToClient tip=%d(没有请求 id,分不清是哪一条)", tip))
	}
}

// callAll 不等回包依次发出 requests(每条一个 gc.Call,各自分配请求号),再等每一条自己的结局,返回与 requests
// 一一对应的回包记录。回包只按 MessageContent.id 认领(docs/design/client-rpc-callback.md §6):结果按请求对应,
// 与到达顺序无关;上一次超时请求的迟到回包由 GameClient 计数后丢弃,不会被认成本次的。
//
// 信封拒绝不算 error,记在 record.envelopeTip 里交调用方判断。返回 error 的情形:某条本地没发出(其后的不再发);
// 任一条在 orderingSmokeReplyTimeout 内没有回包(包裹 errOrderingSmokeTimeout,等齐全部结局后报,列出哪几条收到了);
// 连接被关 / 换掉;gate 推 SendTipToClient(kServiceUnavailable)(见 onGateTip,立刻返回,不等满超时)。
//
// 只能在驱动本机器人的那个 goroutine 上调用,不得在 RecvLoop 上(onMessage、done):回包只能由 RecvLoop 派发,
// 在那里阻塞等回包就是自锁。同一时刻只服务一次等待:abort 只有一个槽位。
func (b *orderingSmokeBot) callAll(requests ...orderingSmokeRequest) ([]orderingSmokeRecord, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	b.mu.Lock()
	b.abort = cancel
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.abort = nil
		b.mu.Unlock()
	}()

	// 每条一个容量 1 的槽:done 恰好写一次、从不阻塞(它跑在 RecvLoop / 定时器 goroutine 上),
	// 哪怕本函数已经因 abort 或后一条没发出而提前返回。
	slots := make([]chan orderingSmokeOutcome, len(requests))
	for i, r := range requests {
		slot := make(chan orderingSmokeOutcome, 1)
		slots[i] = slot
		b.gc.Call(r.messageId, r.request, orderingSmokeReplyTimeout, func(reply *base.MessageContent, err error) {
			if reply != nil {
				b.stats.MsgRecv() // 回包与信封错误不再经过 onMessage,在这里计数
			}
			slot <- orderingSmokeOutcome{reply: reply, err: err}
		})
		// 本地没发出(已 Close / 无连接)在 Call 返回前同步完成:后面的不再发,也不计发送。
		select {
		case outcome := <-slot:
			if errors.Is(outcome.err, pkg.ErrRPCNotSent) {
				return nil, fmt.Errorf("send message_id=%d account=%s: %w", r.messageId, b.account, outcome.err)
			}
			// 已完成但不是本地未发出(回包 / 信封错误抢先到,或并发的 Close / SwapConn 以 ErrRPCDisconnected 完成):
			// 放回槽里(done 只写一次,槽此刻是空的,不会阻塞),下面统一归类。
			slot <- outcome
		default:
		}
		b.stats.MsgSent()
	}

	records := make([]orderingSmokeRecord, len(requests))
	received := make([]bool, len(requests))
	timedOut := false
	for i, r := range requests {
		var outcome orderingSmokeOutcome
		select {
		case outcome = <-slots[i]:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
		var tipErr *pkg.ServerTipError
		switch {
		case outcome.err == nil:
			records[i] = orderingSmokeRecord{messageId: r.messageId, body: outcome.reply.GetSerializedMessage()}
		case errors.As(outcome.err, &tipErr):
			records[i] = orderingSmokeRecord{messageId: r.messageId, envelopeTip: tipErr.Tip}
		case errors.Is(outcome.err, pkg.ErrRPCTimeout):
			// 先不报:其余各条的定时器几乎同时起算,等齐后一并说明哪几条收到了。
			timedOut = true
			continue
		default:
			return nil, fmt.Errorf("message_id=%d account=%s: %w", r.messageId, b.account, outcome.err)
		}
		received[i] = true
	}
	if timedOut {
		wantIds := make([]uint32, len(requests))
		for i, r := range requests {
			wantIds[i] = r.messageId
		}
		return nil, fmt.Errorf("%w(等待 message_id=%v,已收到 %v)", errOrderingSmokeTimeout, wantIds, received)
	}
	return records, nil
}

// orderingPipelineResult 是一轮 Join → Cancel 流水线的结局。
type orderingPipelineResult struct {
	requestId     string // Join 与 Cancel 共用的意图编号
	ticket        string // Join 回包里的票号(仍在排队时用来收尾)
	joinErrorCode uint32 // Join 的业务拒绝码,0 = 受理;非 0 只可能是 kMatchInternal,见 pipelineJoinThenCancel
}

// pipelineJoinThenCancel 不等回包连发 JoinQueue → CancelQueue(同一个 request_id,模拟「点了匹配马上又点取消」),
// 等两条各自的回包(按请求号认领)。
//
// Join 回 kMatchInternal 不在这里判失败:撤销墓碑抢先提交时 Join 就回这个码(authority.ErrJoinCancelled 经 joinRejection),
// 响应体分不出它和真的内部错误,由调用方查 GetQueueStatus 的 request_cancelled 判定。真的内部错误之后 Cancel 照样留墓碑,
// 会被记成乱序,但结果同样是 ORDERING_SMOKE_FAIL,不会假绿。其余业务拒绝(已在队列中、探测失效等)直接报错。
func (b *orderingSmokeBot) pipelineJoinThenCancel() (orderingPipelineResult, error) {
	planId, err := ensureNetworkProbe(b.gc, b.stats, b.account)
	if err != nil {
		return orderingPipelineResult{}, err
	}
	requestId := chatSmokeRequestId()
	replies, err := b.callAll(
		orderingSmokeRequest{game.MatchServiceJoinQueueMessageId, &match.JoinQueueRequest{
			Mode:           match.MatchMode_MATCH_MODE_1V1,
			BattleConfigId: crossZoneBattleConfigId,
			RequestId:      requestId,
			NetworkPlanId:  planId,
		}},
		orderingSmokeRequest{game.MatchServiceCancelQueueMessageId, &match.CancelQueueRequest{RequestId: requestId}})
	if err != nil {
		return orderingPipelineResult{}, err
	}
	join, cancel := replies[0], replies[1]
	if join.envelopeTip != 0 || cancel.envelopeTip != 0 {
		return orderingPipelineResult{}, fmt.Errorf("gate / 路由服拒绝:JoinQueue 信封 tip=%d,CancelQueue 信封 tip=%d", join.envelopeTip, cancel.envelopeTip)
	}
	resp := &match.JoinQueueResponse{}
	if err := proto.Unmarshal(join.body, resp); err != nil {
		return orderingPipelineResult{}, fmt.Errorf("解析 JoinQueueResponse: %w", err)
	}
	if code := resp.GetErrorCode(); code != 0 && code != tipOrderingMatchInternal {
		hint := ""
		if code == tipOrderingAlreadyQueued {
			hint = "(已在队列中:上一轮残留未清,见 O2)"
		}
		return orderingPipelineResult{}, fmt.Errorf("JoinQueue 业务拒绝 error_code=%d%s", code, hint)
	}
	return orderingPipelineResult{requestId: requestId, ticket: resp.GetQueueTicket(), joinErrorCode: resp.GetErrorCode()}, nil
}

// queueStatus 查一次权威队列状态(GetQueueStatus)。requestId 非空时,回包的 request_cancelled 说明这次 Join 的撤销墓碑是否已提交。
func (b *orderingSmokeBot) queueStatus(requestId string) (*match.GetQueueStatusResponse, error) {
	replies, err := b.callAll(orderingSmokeRequest{game.MatchServiceGetQueueStatusMessageId, &match.GetQueueStatusRequest{RequestId: requestId}})
	if err != nil {
		return nil, err
	}
	if replies[0].envelopeTip != 0 {
		return nil, fmt.Errorf("gate / 路由服拒绝 GetQueueStatus:信封 tip=%d", replies[0].envelopeTip)
	}
	resp := &match.GetQueueStatusResponse{}
	if err := proto.Unmarshal(replies[0].body, resp); err != nil {
		return nil, fmt.Errorf("解析 GetQueueStatusResponse: %w", err)
	}
	return resp, nil
}

// queueState 只要状态、不关心撤销墓碑时的 queueStatus。
func (b *orderingSmokeBot) queueState() (match.QueueState, error) {
	resp, err := b.queueStatus("")
	if err != nil {
		return match.QueueState_QUEUE_STATE_UNSPECIFIED, err
	}
	return resp.GetState(), nil
}

// cancelWithTicket 带票号取消(乱序轮的收尾)。
func (b *orderingSmokeBot) cancelWithTicket(ticket string) error {
	replies, err := b.callAll(orderingSmokeRequest{game.MatchServiceCancelQueueMessageId, &match.CancelQueueRequest{QueueTicket: ticket}})
	if err != nil {
		return err
	}
	if replies[0].envelopeTip != 0 {
		return fmt.Errorf("信封 tip=%d", replies[0].envelopeTip)
	}
	state, err := b.queueState()
	if err != nil {
		return err
	}
	if state == match.QueueState_QUEUE_STATE_QUEUED {
		return fmt.Errorf("带票号取消之后仍在排队")
	}
	return nil
}
