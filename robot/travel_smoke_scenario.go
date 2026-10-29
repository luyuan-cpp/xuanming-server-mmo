package main

// travel-smoke 场景:单机器人对「跨 zone 场景传送」做往返端到端冒烟
// (docs/design/cross-zone-scene-travel.md §4 数据流 / §5 阶段 3):
//
//	T1 登 home_zone(首登允许建角:home zone 就是在建角时登记的)→ 记下 player_id、gate 地址;
//	   读金币 → 在 home 区 GmAddCurrency +11 并读回,得到**非 0 且刚刚才变过**的金币出发值(为什么必须这样见 travelSmokeHomeGoldDelta);
//	   登录途中就被 msg 124 送走 = 上一轮死在访客区的残留,直接失败(此时"出发区 == home"不成立);
//	T2 负向:TravelToZone{target_zone_id=0} 必须被**同步**拒绝(回包体 tip != 0)且不发生重定向、金币不变;
//	   T2b:TravelToZone{visit_zone, 不存在的 scene_config_id} 同样必须同步被拒(scene 在冻结之前按 World 表校验目标地图);
//	T3 去程:TravelToZone{visit_zone} → 等 msg 124 → pkg.FollowRedirect(探测 / 先连新后关旧 / 票据原样验签 / 严格重登)
//	   → 等排在 124 **之后**的 NotifyEnterScene;断言 player_id 不变、票据 zone == visit_zone、gate 地址变了;
//	T4 访客区:金币 == 出发值(读到的是 home 刚落盘的档)→ GmAddCurrency +7 生效并可读回(scene(B) 可写)
//	   → 移动探针(尽力而为)→ 停留 dwell_seconds(熬过 home 区 login 的 30s 断线租约)→ 没被踢、没被二次重定向、金币仍是 +7;
//	T5 回程:TravelToZone{home_zone}(CZ-9:回家 = 同一条链反向)→ 同 T3 的三条断言(票据 zone == home_zone);
//	T6 回家:金币 == 出发值 + 7(访客期间的存盘按 home_zone 落库,回家不回档 —— CZ-2 / 不变量 2);全程恰好 2 次重定向。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `TRAVEL_SMOKE_OK player_id=… home_zone=… visit_zone=… home_gate=… visit_gate=… back_gate=…
//	        gold_home=… gold_back=… move_ack=… ticket_binding=… reject_tip=… reject_map_tip=…`,退出码 0;
//	任一步失败 → `TRAVEL_SMOKE_FAIL step=… reason=…`,退出码 1。
//
// 与其它冒烟不一样、不照做就会假绿或卡死的四件事:
//   - 登录后必须 pkg.Clients.Register:msg 124 的 handler 靠它从 player_id 反查连接,查不到只打一行 WARN 就返回,
//     现象是 TravelToZone 之后静默超时(prepareBehaviorClient / teamSmokeLogin 都不登记,不能照抄);
//   - 重登必须"严格":main.go 注册的通用重登在角色列表为空时会**自动建角**再用 Players[0] 进游戏。访客区 login
//     读不到账号 blob(两 zone 的 Redis 没共享)时,它会建出一个新角色并"成功进场" —— 假绿加脏数据。
//     本场景用 travelSmokeStrictRelogin 覆盖它:按 player_id 选角,找不到就失败。SetRedirectRelogin 是进程全局的,
//     本模式独占进程,覆盖是安全的;
//   - "到了"不能用 player.WaitSceneReady 判:它由 sync.Once 关闭,第二次进场会立即返回。这里自己按到达顺序留底,
//     要求 NotifyEnterScene 排在 msg 124 之后;
//   - 只等 NotifyEnterScene 也不够:CZ-8(目标区 login 认票据 target_zone_id、不弹回)没生效时,目标区会立刻再发一个
//     124 把人送回家,机器人会乖乖跟随,表面上"传送成功"。所以每一跳都要断言票据 zone 与 gate 地址,
//     并且一次传送窗口内出现第二个 124 直接判失败。
//
// 请求-响应调用(TravelToZone、金币读写)一律走 gc.Call,回包按请求号认领,不进留底、不经通用分发
// (docs/design/client-rpc-callback.md §6);留底只收推送(124 / NotifyEnterScene / SendTipToClient / KickPlayer / MoveAck)。
//
// TravelToZone 的回包是**可选**的:124 一到,SwapConn 就关旧连接并让旧连接上的这次调用以 pkg.ErrRPCDisconnected 收场,
// 之后才从旧 gate 发出的回包收不到。只能当"到了且 tip != 0 就立即失败",不能当必需条件;ErrRPCDisconnected 是正常路径。
//
// tip 码一律不写数字(AGENTS.md §7 不变量 5)。正向流程不需要任何 tip 常量;负向只断言 tip != 0,
// 这样不依赖还没导出的 kZoneTravel* 枚举名,日志里打印的是服务器回来的运行时值。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/common/component"
	"proto/login"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。
// 96xx 与 battle-smoke(9001-9004)、chat-smoke(9005/9006)、属性(9101)、宠物(9102)、guild-smoke(9201-9203,
// 帮会二期预留 9211-9219)、team-smoke(9301-9304)、trade-smoke(9401-9403)错开,避免并行冒烟互相顶号。
// 不用 95xx:docs/design/guild-phase2/04-asset-channel.md 已把 robot_9501 预留给资产通道冒烟,
// 那个冒烟要精确核对金币账本,而本场景每轮会给账号净加金币,两者不能共用账号。
// travel_smoke.account 缺省沿用 robot_9601；显式账号仍必须匹配当前 login 的开发账号白名单。
// 必须是**首次在 home_zone 建角**的账号:home zone 在建角时登记,之后不随登录区变化。

const (
	// 单次请求 / 回包的等待预算(金币读写、负向传送的同步回包)。
	travelSmokeRpcTimeout = 10 * time.Second
	// 首登等进场的预算,与 prepareBehaviorClient / team-smoke 同值。
	travelSmokeSceneReadyTimeout = 15 * time.Second
	// 一跳传送的总预算的缺省值:源端存盘落地 + 写 handoff 标记 + scene_manager.EnterScene + Kafka 下发 124,
	// 再加 FollowRedirect 自身各段的上限(探测 5s、验签 10s、Login 与 EnterGame 各 15s),最后等新 zone 的 NotifyEnterScene。
	// 实际用 travel_smoke.hop_timeout_seconds(config.TravelSmokeConfig,缺省同值);验收冻结硬上限时调到 ≥ 75s。
	travelSmokeRedirectTimeout = 60 * time.Second
	// 移动探针等 MoveAck 的窗口。纠偏是同步裁决、随手回发,本地毫秒级;3s 内没有就当没有。
	travelSmokeMoveAckTimeout = 3 * time.Second
	// 同一机器人相邻两个请求的最小间隔。TravelToZone 是新消息号,不在 MessageLimiter 表里时吃 gate 默认档
	// 3 次 / 窗口(推导见 chat_smoke_scenario.go 的 chatSmokeRequestSpacing),1.1s 间隔下不会被信封拒绝。
	travelSmokeRequestSpacing = 1100 * time.Millisecond
	// 留底扫描的轮询粒度。
	travelSmokePollInterval = 50 * time.Millisecond
	// 出发前在 home 区用 GM 加的金币数,每轮**无条件**加。它让 T4「访客区金币 == 出发值」成为真断言:
	//   - 出发值必须非 0:新建角金币是 0;访客区 scene 读不到档时会把人当新号建一个空实体(金币同样是 0)并照常进场,
	//     之后 +7、读回、回家 == 7 全都成立,而空实体一存盘就按 home_zone 的 topic 覆盖原档(等级 / 背包全丢)。
	//     出发值为 0 时 0 == 0 抓不到这种串档 —— 它恰恰是本冒烟最该抓的形态;
	//   - 出发值必须是刚刚才变过的:余额几轮不变时,读到一份**旧**档也能相等。出发前几秒才加的这一笔
	//     只存在于 scene(A) 的内存里,访客区读得到它,才证明读到的是传送前那次存盘(CZ-5「先存盘后放行」)。
	// 不做成"为 0 才补"的分支:那样这段代码一辈子只在首轮跑一次,而且第二条保证就没有了。
	// 与访客区的 7 取不同的小质数,日志里一眼分得清是哪一笔没到账。
	travelSmokeHomeGoldDelta int64 = 11
	// 访客区用 GM 加的金币数。选一个不起眼的小质数:回家后余额恰好多 7,几乎不可能是别的流程碰巧造成的。
	travelSmokeGoldDelta int64 = 7
	// T2b 用的"不存在的目标地图"。BaseScene / World 表的 id 都是小整数,这个值任何部署的 World 表里都不会有。
	// 不能取 0:0 是合法值(= 由目标 zone 的 scene_manager 挑默认大世界)。
	travelSmokeBogusSceneConfigId uint32 = 4000000000
	// 移动探针上报的坐标:故意远在任何地图之外,服务器有导航网格时必然纠偏并回 MoveAck。
	// 副作用:没有导航网格的场景 fail-open 会原样接受这个位置并存盘 —— 只影响本冒烟专用账号,
	// 且下次进有导航网格的场景时 SceneSpawnSystem::EnsureValidEnterLocation 会把它改写回出生点。
	travelSmokeProbeCoord = 100000.0
)

// errTravelSmokeTimeout 是金币读写等回包超时的哨兵(smokeCaller 的 timeoutErr),拼进失败原因。
var errTravelSmokeTimeout = errors.New("no reply within")

// travelSmokeRecord 是一条留底消息。
type travelSmokeRecord struct {
	messageId uint32
	// gate 级拒绝(限流 / 不认识的消息号)走信封 MessageContent.error_message,此时 body 为空;推送恒为 0。
	envelopeTip uint32
	body        []byte
}

// travelSmokeProgress 是重定向 / 重登的进度快照(连同没回显请求号的 TravelToZone 回包诊断),
// 与留底在同一把锁下读出,保证两者互相一致。零值 = 什么都还没发生(ownership_fault_fullstack_test.go 用字面量构造机器人)。
type travelSmokeProgress struct {
	// msg 124 的分发(含整个 pkg.FollowRedirect)已经返回的次数。
	redirectsHandled int
	// 严格重登已开始 / 已结束的次数,以及最近一次的结果。
	reloginStarted  int
	reloginFinished int
	reloginErr      error
	// uncorrelatedTravel 只用于超时诊断,不参与判定;取窗口内的部分用 since。
	uncorrelatedTravel travelSmokeUncorrelatedReplies
}

// travelSmokeUncorrelatedReplies 累计 onMessage 收到的、没带请求号(id = 0)的 TravelToZone 回包。
// 服务端没回显 ClientRequest.id 时(例如直连模式的 gate 缺 10-19 rpc_reply_bridge 修复),回包认领不到在途调用,
// 只能在 onMessage 丢弃(smokeDropUncorrelatedReply),调用随后以超时收场。丢弃前记在这里,超时文案才能说出
// "回包其实到了、而且可能是拒绝",而不是"回包与 124 都没到"。判定仍只按 Call 的结局,这里不改变任何结论。
// 零值 = 没收到过;由 onMessage 持 b.mu 写,mark / snapshot 随进度一起读出。
type travelSmokeUncorrelatedReplies struct {
	count int
	// 最近一条的信封 tip(gate 信封拒绝,此时回包体为空)与回包体 tip(scene 写进 TravelToZoneResponse.error_message
	// 的同步拒绝码,0 = 受理)。lastBodyUndecodable 表示回包体解不开,此时 lastBodyTip 为 0。
	lastEnvelopeTip     uint32
	lastBodyTip         uint32
	lastBodyUndecodable bool
}

// note 记下一条没带请求号的 TravelToZone 回包。msg 必须是 TravelToZone 消息号上 id = 0 的消息。
func (u *travelSmokeUncorrelatedReplies) note(msg *base.MessageContent) {
	u.count++
	u.lastEnvelopeTip = msg.GetErrorMessage().GetId()
	// 信封拒绝时回包体为空,解出来是 tip=0,不影响上面的信封 tip。
	tip, err := travelToZoneRejectTip(msg.GetSerializedMessage())
	u.lastBodyTip, u.lastBodyUndecodable = tip, err != nil
}

// since 返回 before 之后新到的部分(before 取自等待开始前的 mark)。窗口内一条都没有时返回零值:
// 那时"最近一条"属于更早的等待,不能拿来解释这一次。
func (u travelSmokeUncorrelatedReplies) since(before travelSmokeUncorrelatedReplies) travelSmokeUncorrelatedReplies {
	if u.count <= before.count {
		return travelSmokeUncorrelatedReplies{}
	}
	u.count -= before.count
	return u
}

// describe 把诊断写成一句话(拼进超时原因);count 为 0 返回空串。
// 信封拒绝时回包体为空,不写 body tip 与图例:否则「body tip=0 / 0 = accepted」会被读成 scene 已受理,
// 与信封提示「请求没到 scene」自相矛盾。图例只在真正写出 body tip 时才附上。
func (u travelSmokeUncorrelatedReplies) describe() string {
	if u.count == 0 {
		return ""
	}
	var last string
	switch {
	case u.lastEnvelopeTip != 0:
		last = fmt.Sprintf("envelope tip=%d%s, body empty (envelope rejection)",
			u.lastEnvelopeTip, travelSmokeEnvelopeHint(u.lastEnvelopeTip))
	case u.lastBodyUndecodable:
		last = "envelope tip=0, body undecodable"
	default:
		last = fmt.Sprintf("envelope tip=0, body tip=%d; body tip 0 = accepted, non-zero = the scene's synchronous rejection code",
			u.lastBodyTip)
	}
	return fmt.Sprintf("received %d TravelToZone reply(ies) without a request id (last one: %s): the server did not echo ClientRequest.id, "+
		"so the call could not claim the reply (direct mode needs a gate with the 2026-10-19 rpc_reply_bridge fix; "+
		"on the scene TCP path check set_id in scene_handler.cpp)",
		u.count, last)
}

// travelSmokeUncorrelatedCause 返回传送等待超时时应优先给出的「回包没回显请求号」原因,不适用时返回空串。
// 只在调用没见到回包、也没见到 msg 124 时适用:那时通用提示是"都没到",而回包其实到了,只是认领不到。
// uncorrelated 必须已按等待窗口截取(since)。travel-smoke 与 sync-smoke 的超时文案共用。
func travelSmokeUncorrelatedCause(scan travelSmokeScan, uncorrelated travelSmokeUncorrelatedReplies) string {
	if scan.hop != nil || scan.replySeen {
		return ""
	}
	return uncorrelated.describe()
}

// travelSmokeHop 是一跳传送落地后留下的证据。
type travelSmokeHop struct {
	// msg 124 指向的目标 gate("ip:port")。
	targetAddr string
	// 票据 GateTokenPayload.zone_id:签发方(scene_manager AssignGateForZone)填的是目标 gate 所在的 zone。
	// 0 = notify 没带票据或票据里没有 zone,无法证明落区。
	tokenZone uint32
	// CZ-8 的两个绑定字段(票据持有者与本跳目标 zone)。notify 没带票据时它们保持 0,
	// 但那种情况 verifyArrival 会先被 tokenZone == 0 挡下,轮不到这两条断言。
	ticketPlayer     uint64
	ticketTargetZone uint32
	// 124 之后第一条 NotifyEnterScene 的场景。
	sceneId       uint64
	sceneConfigId uint32
}

// travelSmokeScan 是扫一遍传送窗口留底的结果。
type travelSmokeScan struct {
	// hop != nil 表示已见到 msg 124。
	hop *travelSmokeHop
	// arrived 表示 124 之后已见到 NotifyEnterScene。
	arrived bool
	// replySeen 表示 TravelToZone 的回包(含信封拒绝)赶在旧连接被换掉之前到了。取自那次调用的结局
	// (travelSmokeReplyOutcome),不是留底;可选,只用于诊断。
	replySeen bool
	// lateTips 是 124 之后收到的 SendTipToClient(不判失败,超时诊断时带上)。
	lateTips []uint32
}

// travelSmokeReplyOutcome 是一次 TravelToZone 调用的结局快照。settled=false 表示还没有结局;
// 有结局时 reply / err 的形状同 pkg.GameClient.Call 的 done:(回包, nil)、(信封, *pkg.ServerTipError)、(nil, 其余错误)。
type travelSmokeReplyOutcome struct {
	settled bool
	reply   *base.MessageContent
	err     error
}

// travelSmokeTravelCall 是在途的一次 TravelToZone。传送等待不能阻塞在回包上:回包可有可无,而 124 / 重登 /
// NotifyEnterScene 要同时盯着。所以用 gc.Call 异步登记,done 把结局写进来,等待循环每圈取一次快照。
// done 跑在 RecvLoop / 定时器 / SwapConn 所在的 goroutine 上,只写结局,不阻塞;mu 只保护 outcome。
type travelSmokeTravelCall struct {
	mu      sync.Mutex
	outcome travelSmokeReplyOutcome
}

func (c *travelSmokeTravelCall) settle(reply *base.MessageContent, err error) {
	c.mu.Lock()
	c.outcome = travelSmokeReplyOutcome{settled: true, reply: reply, err: err}
	c.mu.Unlock()
}

// snapshot 返回当前结局。结局只写一次(Call 的 done 恰好一次),之后的快照都相同。
func (c *travelSmokeTravelCall) snapshot() travelSmokeReplyOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outcome
}

// travelSmokeStartTravel 用 gc.Call 发一次 TravelToZone 并立刻返回在途调用,travel-smoke 与 sync-smoke 共用。
// timeout 是回包的认领窗口:到期仍无回包,调用以 pkg.ErrRPCTimeout 收场,之后到达的回包由 GameClient 按迟到丢弃;
// 传送等待要在整个落地窗口里都认得回包,调用方传那个窗口。
// 本地没发出时返回 error(done 已在 Call 返回前同步写入 pkg.ErrRPCNotSent)。
// 计数:确认发出后计 MsgSent;回包(含信封拒绝)到达时在 done 里计 MsgRecv —— 它不经过场景的 onMessage。
// 调用方负责请求节流。
func travelSmokeStartTravel(gc *pkg.GameClient, stats *metrics.Stats, targetZone, sceneConfigId uint32, timeout time.Duration) (*travelSmokeTravelCall, error) {
	call := &travelSmokeTravelCall{}
	gc.Call(travelToZoneMessageId, newTravelToZoneRequest(targetZone, sceneConfigId), timeout,
		func(reply *base.MessageContent, err error) {
			if reply != nil {
				stats.MsgRecv()
			}
			call.settle(reply, err)
		})
	if outcome := call.snapshot(); outcome.settled && errors.Is(outcome.err, pkg.ErrRPCNotSent) {
		return nil, outcome.err
	}
	stats.MsgSent()
	return call, nil
}

// travelSmokeBot 是本场景唯一的机器人会话。
type travelSmokeBot struct {
	account  string
	password string
	gc       *pkg.GameClient
	player   *gameobject.Player
	stats    *metrics.Stats
	// homePlayer 是首登拿到的 player_id,严格重登只认它。RecvLoop 启动前写入,之后只读。
	homePlayer uint64

	// RecvLoop goroutine 写、主流程读,mu 保护。records 只追加。
	mu       sync.Mutex
	records  []travelSmokeRecord
	progress travelSmokeProgress

	// 一跳传送的总预算(travel_smoke.hop_timeout_seconds),构造后只读。
	hopTimeout time.Duration

	// 以下只由主流程读写。
	lastRequestAt time.Time
	moveInputSeq  uint32
}

// RunTravelSmoke 是 main.go `mode: travel-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunTravelSmoke(cfg *config.Config) {
	// loginAndEnterScenario / buildScenarioLoginRequest 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.TravelSmoke

	var bot *travelSmokeBot
	cleanup := func() {
		if bot == nil {
			return
		}
		// 死在搬迁途中(旧连接已关、重登失败)时这两条发不到任何 login,属预期:
		// 残留靠 login 断线租约 / handoff 门自己过期,yaml 文件头写了"等一会儿再重跑"。
		_ = leaveGame(bot.gc, stats)
		sendDisconnectBestEffort(bot.gc)
		pkg.Clients.Unregister(bot.homePlayer, bot.gc)
		gameobject.PlayerList.Delete(bot.homePlayer)
		bot.gc.Close()
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("TRAVEL_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- T1:登 home_zone ----
	homeCfg := *cfg
	homeCfg.ZoneID = sc.HomeZone
	var err error
	bot, err = travelSmokeLogin(&homeCfg, sc.Account, stats)
	if err != nil {
		fail("login", "account=%s zone=%d err=%v", sc.Account, sc.HomeZone, err)
	}
	homeGate := bot.gateAddr()
	if bot.countSince(0, game.SceneClientPlayerCommonRedirectToGateMessageId) > 0 {
		fail("login-redirected", "Received msg 124 while logging into zone %d (currently connected to %s): the account location still points to another zone, "+
			"probably left over from the previous run dying in a visitor zone. Wait for the ticket / pending landing to expire (300s), or clear player:{id}:location before rerunning",
			sc.HomeZone, homeGate)
	}
	goldLogin, err := bot.readGold()
	if err != nil {
		fail("home-gold-read", "%v", err)
	}
	// 造出发值:登录时的余额 + 一笔刚加的(travelSmokeHomeGoldDelta 的注释写了为什么不能直接拿登录余额当出发值)。
	// 加完必须读回:balance_after 只说明 scene(A) 内存里改了,后面所有断言比的都是 GetCurrencyList 读到的值。
	goldHome := goldLogin + uint64(travelSmokeHomeGoldDelta)
	gold, err := bot.addGold(travelSmokeHomeGoldDelta)
	if err != nil {
		// 这是全程第一条 GM 指令,环境没放行 GM 时就死在这里。2026-10-15 起 gate 的 GM 闸拒绝回带请求 id 的信封
		// (message_id = GmAddCurrency,tip = kFeatureUnavailable 1006;client-request-ordering.md O6),
		// travelSmokeReplyRejection 会按 travelSmokeEnvelopeHint 把它报成 gate GM 闸拒绝;以前只推一条 23 号 tip,表现是等回包超时。
		fail("home-gold-seed", "%v (an envelope tip 1006 from the gate GM guard means the home-zone gate GATE_RUN_MODE is not dev)", err)
	}
	if gold != goldHome {
		fail("home-gold-seed", "Home-zone GmAddCurrency returned balance_after=%d, expected %d (login balance %d + %d)",
			gold, goldHome, goldLogin, travelSmokeHomeGoldDelta)
	}
	if gold, err = bot.readGold(); err != nil {
		fail("home-gold-seed-readback", "%v", err)
	}
	if gold != goldHome {
		fail("home-gold-seed-readback", "After the home-zone addition, read gold=%d, expected %d", gold, goldHome)
	}
	zap.L().Info("[travel-smoke] at home",
		zap.Uint64("player_id", bot.homePlayer), zap.Uint32("home_zone", sc.HomeZone),
		zap.String("home_gate", homeGate), zap.Uint64("gold_login", goldLogin), zap.Uint64("gold_home", goldHome))

	// ---- T2:负向 —— 非法目标必须同步被拒,且不发生重定向 ----
	// 0 在服务端任何一层都是非法 zone(PlayerTravelHandoffComp 注释:targetZoneId 0 非法),不依赖部署里有哪些 zone。
	// 这一步同时是 C++ 侧“拒绝码直接写本次应答”（player_service_interface.h）的回归点：
	// handler 漏写 response->error_message 时，客户端看到的是 tip=0 的“受理”。
	rejectTip, err := bot.expectTravelRejected(0, sc.SceneConfigId)
	if err != nil {
		fail("reject-invalid-zone", "%v", err)
	}
	// T2b:合法的目标 zone + 不存在的目标地图,同样必须**同步**被拒。
	// scene_config_id 是客户端可控字段,scene 要在冻结之前按 World 表校验(RequestZoneTravel)。漏了这道校验,请求会被
	// 受理 → 源端冻结、存盘、放行、销毁实体,目标 zone 才发现这张图落不进去,那时玩家已经没有"原地"可回
	// (scene_manager 两条腿上各有一道兜底,但那是给漏网的请求准备的,不该靠它们挡客户端乱填的 id)。
	// 这一步若报"回包 tip=0(被受理了)",expectTravelRejected 提示里的"没校验目标 zone"应读作"没校验目标地图";
	// 此时服务端不会留下残局:scene_manager 第一条腿会拒掉这张图,源 scene 解冻并推一条 tip。
	rejectMapTip, err := bot.expectTravelRejected(sc.VisitZone, travelSmokeBogusSceneConfigId)
	if err != nil {
		fail("reject-invalid-map", "scene_config_id=%d: %v", travelSmokeBogusSceneConfigId, err)
	}
	// 拒绝不该留下任何状态。会话是否还活着用一次读来验;"没被留在冻结 / 传送中"由 T3 被受理来证明
	// (残留 PlayerFrozenComp / PlayerTravelHandoffComp 时 T3 会被同步拒绝)。
	// 读到的值也要比:丢掉返回值的读是空断言(会话活着但读到别的东西也算过)。
	if gold, err = bot.readGold(); err != nil {
		fail("reject-session-alive", "Session is unusable after rejecting an invalid target: %v", err)
	}
	if gold != goldHome {
		fail("reject-session-alive", "After rejecting an invalid target gold=%d, expected the departure value %d (rejection should not modify any state)", gold, goldHome)
	}

	// ---- T3:去访客区 ----
	zap.L().Info("[travel-smoke] travel out", zap.Uint32("visit_zone", sc.VisitZone), zap.Uint32("scene_config_id", sc.SceneConfigId))
	outHop, err := bot.travelTo(sc.VisitZone, sc.SceneConfigId)
	if err != nil {
		fail("travel-out", "%v", err)
	}
	if step, err := bot.verifyArrival(outHop, sc.VisitZone, sc.SceneConfigId, homeGate); err != nil {
		fail("visit-"+step, "%v", err)
	}
	visitGate := bot.gateAddr()
	visitMark, _ := bot.mark()
	zap.L().Info("[travel-smoke] arrived at visit zone",
		zap.Uint32("visit_zone", sc.VisitZone), zap.String("visit_gate", visitGate),
		zap.Uint64("scene_id", outHop.sceneId), zap.Uint32("scene_config_id", outHop.sceneConfigId))

	// ---- T4:访客区断言 ----
	// 数据连续:scene(B) 从共享 Redis 读到的必须是 scene(A) 传送前刚落盘的那一份(CZ-5 "先存盘后放行")。
	// 这条断言的分辨力来自 T1:出发值 = 登录余额 + 出发前几秒才加的一笔,所以读到 0 = 没读到档、被当新号建了空实体;
	// 读到登录余额 = 读到的是旧档(传送前那次存盘没落地就放行了)。
	if gold, err = bot.readGold(); err != nil {
		fail("visit-gold-read", "%v", err)
	}
	if gold != goldHome {
		fail("visit-gold", "Visitor-zone gold=%d, expected departure value %d (= login balance %d + %d added in the home zone before departure). "+
			"Reading 0 = the save was not loaded and an empty entity was created as a new account (saving it will overwrite the original save); reading %d = an old save was loaded because travel was allowed before the pre-travel save persisted",
			gold, goldHome, goldLogin, travelSmokeHomeGoldDelta, goldLogin)
	}
	// scene(B) 可写:用请求 / 响应型 RPC 当硬断言(移动类 RPC 的返回是 Empty,见 probeMove)。
	goldVisit := goldHome + uint64(travelSmokeGoldDelta)
	if gold, err = bot.addGold(travelSmokeGoldDelta); err != nil {
		fail("visit-gold-add", "%v", err)
	}
	if gold != goldVisit {
		fail("visit-gold-add", "Visitor-zone GmAddCurrency returned balance_after=%d, expected %d (departure value %d + %d)", gold, goldVisit, goldHome, travelSmokeGoldDelta)
	}
	if gold, err = bot.readGold(); err != nil {
		fail("visit-gold-readback", "%v", err)
	}
	if gold != goldVisit {
		fail("visit-gold-readback", "After the addition, read gold=%d, expected %d", gold, goldVisit)
	}
	moveAck, err := bot.probeMove()
	if err != nil {
		fail("visit-move", "%v", err)
	}
	if sc.RequireMoveAck && !moveAck {
		fail("visit-move", "No MoveAck received within %s (require_move_ack=true; a scene without a navigation mesh fails open without sending an ack)", travelSmokeMoveAckTimeout)
	}
	// 停留:home 区的 gate 发现旧 TCP 断开后会通知 login(A) 进入 30s 断线租约。租约到期的清理如果误伤了
	// 已经指向访客区的 location / 会话,访客会在这之后被踢或读写失败 —— 所以缺省停留 35s 再验一次。
	if sc.DwellSeconds > 0 {
		zap.L().Info("[travel-smoke] dwelling at visit zone", zap.Int("seconds", sc.DwellSeconds))
		time.Sleep(time.Duration(sc.DwellSeconds) * time.Second)
	}
	if err := bot.verifyUndisturbed(visitMark, visitGate); err != nil {
		fail("visit-dwell", "%v", err)
	}
	if gold, err = bot.readGold(); err != nil {
		fail("visit-dwell", "Visitor session is unusable after staying %ds (did expired home-zone disconnect-lease cleanup incorrectly affect the visitor?): %v", sc.DwellSeconds, err)
	}
	if gold != goldVisit {
		fail("visit-dwell", "After staying %ds gold=%d, expected %d", sc.DwellSeconds, gold, goldVisit)
	}

	// ---- T5:回家(同一条链反向,CZ-9) ----
	zap.L().Info("[travel-smoke] travel home", zap.Uint32("home_zone", sc.HomeZone), zap.Uint32("scene_config_id", sc.SceneConfigId))
	backHop, err := bot.travelTo(sc.HomeZone, sc.SceneConfigId)
	if err != nil {
		fail("travel-home", "%v", err)
	}
	if step, err := bot.verifyArrival(backHop, sc.HomeZone, sc.SceneConfigId, visitGate); err != nil {
		fail("home-"+step, "%v", err)
	}
	backGate := bot.gateAddr()
	backMark, _ := bot.mark()

	// ---- T6:回家断言 ----
	// 不回档:访客期间的存盘按 home_zone 选 topic 落库、Redis 物理共享(CZ-2),回家读到的必须是访客区的最终值。
	goldBack, err := bot.readGold()
	if err != nil {
		fail("home-gold-read", "%v", err)
	}
	if goldBack != goldVisit {
		fail("home-gold", "After returning home gold=%d, expected %d (departure value %d + %d added in the visitor zone): state rolled back, or the visitor-zone save went to another database",
			goldBack, goldVisit, goldHome, travelSmokeGoldDelta)
	}
	if err := bot.verifyUndisturbed(backMark, backGate); err != nil {
		fail("home-settled", "%v", err)
	}
	// 全程恰好两次重定向(去一次、回一次)。多出来的就是被弹来弹去(pkg.MaxRedirectHops=3 按会话计,往返用掉 2 次)。
	if n := bot.countSince(0, game.SceneClientPlayerCommonRedirectToGateMessageId); n != 2 {
		fail("hop-count", "Received %d msg 124 packets in total, expected exactly 2", n)
	}

	// 去程 / 回程两跳都跑完了 verifyArrival,CZ-8 的两条票据断言必然已经执行过(它没有
	// "取不到就跳过"的分支了),所以这里恒为 checked —— 留着这个词只是为了结果行格式不变,
	// 外部解析 TRAVEL_SMOKE_OK 的脚本不用改。
	ticketBinding := "checked"
	zap.L().Info(fmt.Sprintf("TRAVEL_SMOKE_OK player_id=%d home_zone=%d visit_zone=%d home_gate=%s visit_gate=%s back_gate=%s "+
		"gold_home=%d gold_back=%d move_ack=%t ticket_binding=%s reject_tip=%d reject_map_tip=%d",
		bot.homePlayer, sc.HomeZone, sc.VisitZone, homeGate, visitGate, backGate,
		goldHome, goldBack, moveAck, ticketBinding, rejectTip, rejectMapTip))
	cleanup()
	_ = zap.L().Sync()
}

// travelSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调。
// 与 teamSmokeLogin 同形,多出来的两步是本场景的命门:注册严格重登、登记 pkg.Clients(都必须早于 RecvLoop 启动,
// 因为登录握手期间暂存的推送 —— 可能就有一条 msg 124 —— 会在 RecvLoop 入口被补投)。
func travelSmokeLogin(cfg *config.Config, account string, stats *metrics.Stats) (*travelSmokeBot, error) {
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
	// 首登走通用登录:账号第一次跑时要在 home_zone 建角(home zone 就是这时登记的),这里允许自动建角。
	// 不允许自动建角的是**重定向之后**的重登,见 travelSmokeStrictRelogin。
	if err := loginAndEnterScenario(gc, cfg.Password, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("login and enter: %w", err)
	}

	bot := &travelSmokeBot{
		account:    account,
		password:   cfg.Password,
		gc:         gc,
		player:     gameobject.NewPlayer(gc.PlayerId),
		stats:      stats,
		homePlayer: gc.PlayerId,
		hopTimeout: time.Duration(cfg.TravelSmoke.HopTimeoutSeconds) * time.Second,
	}
	// 覆盖 main.go 注册的通用重登(它会自动建角)。进程全局,本模式独占进程。
	pkg.SetRedirectRelogin(bot.relogin)
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	// 不登记的话 msg 124 的 handler 反查不到连接,只打一行 WARN 就返回,传送表现为静默超时。
	pkg.Clients.Register(gc.PlayerId, gc)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), travelSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		sendDisconnectBestEffort(gc)
		pkg.Clients.Unregister(gc.PlayerId, gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready(account=%s): %w", account, err)
	}
	return bot, nil
}

// travelSmokeIsRecorded:本场景按到达顺序留底的消息号,全是推送。
// 请求-响应调用(TravelToZone、金币读写)的回包按请求号交给各自的 Call,不进留底(见 travelSmokeIsCallReply)。
func travelSmokeIsRecorded(messageId uint32) bool {
	switch messageId {
	case game.SceneClientPlayerCommonRedirectToGateMessageId,
		game.SceneSceneClientPlayerNotifyEnterSceneMessageId,
		game.SceneClientPlayerCommonSendTipToClientMessageId,
		game.SceneClientPlayerCommonKickPlayerMessageId,
		game.SceneMovementClientPlayerNotifyMoveAckMessageId:
		return true
	}
	return false
}

// travelSmokeIsCallReply:本场景用 gc.Call 发出的请求消息号。它们的回包带请求号、不会走到 onMessage;
// 走到 onMessage 的(id = 0)说明服务端没回显请求号,只能丢弃(TravelToZone 的先记诊断,见 onMessage)。
func travelSmokeIsCallReply(messageId uint32) bool {
	switch messageId {
	case travelToZoneMessageId,
		game.SceneCurrencyClientPlayerGetCurrencyListMessageId,
		game.SceneCurrencyClientPlayerGmAddCurrencyMessageId:
		return true
	}
	return false
}

// onMessage:推送先留底,再转发给通用分发。
//   - 请求消息号上 id = 0 的消息(travelSmokeIsCallReply)交给 smokeDropUncorrelatedReply 丢弃,不进通用分发:
//     信封拒绝落进生成的 handler 会被解成全零响应,看上去像受理;对应的那次调用将以超时收场。
//     TravelToZone 的丢弃前持 b.mu 记一条诊断(含回包体里的同步拒绝码,travelSmokeUncorrelatedReplies),
//     超时文案据此说出"回包到了但没回显请求号",判定不变。
//   - 其余无条件转发:NotifyEnterScene 靠通用 handler 放行首登的 WaitSceneReady;msg 124 在转发里同步完成整个搬迁
//     (pkg.FollowRedirect),期间 RecvLoop goroutine 一直被占用。
//   - 转发时**绝不能持有 b.mu**:FollowRedirect 末尾的 ReplayDeferred 会在同一个 goroutine 里重入本函数
//     (补投新 zone 的 NotifyEnterScene),持锁转发就是自锁。严格重登(b.relogin)也在这条调用链里拿 b.mu。
func (b *travelSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	messageId := msg.GetMessageId()
	if travelSmokeIsCallReply(messageId) {
		if messageId == travelToZoneMessageId {
			b.mu.Lock()
			b.progress.uncorrelatedTravel.note(msg)
			b.mu.Unlock()
		}
		smokeDropUncorrelatedReply(b.account, msg)
		return
	}
	if travelSmokeIsRecorded(messageId) {
		b.mu.Lock()
		b.records = append(b.records, travelSmokeRecord{
			messageId:   messageId,
			envelopeTip: msg.GetErrorMessage().GetId(),
			body:        msg.GetSerializedMessage(),
		})
		b.mu.Unlock()
	}

	handler.MessageBodyHandler(client, msg)

	if messageId == game.SceneClientPlayerCommonRedirectToGateMessageId {
		// FollowRedirect 失败只打日志、没有返回值。记下"124 的处理已经返回":主流程据此区分
		// "还在搬"与"没走到重登就失败了"(探测不通 / 验签被拒 / 环路熔断 / Clients 没登记),不必干等到超时。
		b.mu.Lock()
		b.progress.redirectsHandled++
		b.mu.Unlock()
	}
}

// relogin 是注册给 pkg.SetRedirectRelogin 的重登实现:严格重登 + 把过程与结果记给主流程。
// 跑在 RecvLoop goroutine 上(pkg.FollowRedirect 调用),此刻 onMessage 不持有 b.mu。
func (b *travelSmokeBot) relogin(gc *pkg.GameClient) error {
	b.mu.Lock()
	b.progress.reloginStarted++
	b.mu.Unlock()

	err := travelSmokeStrictRelogin(gc, b.password, b.stats, b.homePlayer)

	b.mu.Lock()
	b.progress.reloginFinished++
	b.progress.reloginErr = err
	b.mu.Unlock()
	return err
}

// travelSmokeStrictRelogin 在新 gate 的连接上重跑 Login + EnterGame,但**只认 want 这一个角色**:
// 角色列表里没有它就失败,绝不 CreatePlayer。对照 login.go 的 loginAndEnter:那里 len(Players)==0 就建角、
// 再用 Players[0] 进游戏 —— 放在跨 zone 重登上,就是"目标区读不到账号 blob 时悄悄建个新号并成功进场"。
// 认证方式跟随配置(buildScenarioLoginRequest:password / satoken);不走 access_token 与 /api/login 快路径,
// 后者会带 cfg.ZoneID(= home zone)去访客区登录。
func travelSmokeStrictRelogin(gc *pkg.GameClient, password string, stats *metrics.Stats, want uint64) error {
	request, err := buildScenarioLoginRequest(gc.Account, password)
	if err != nil {
		return fmt.Errorf("build login request: %w", err)
	}
	var lr login.LoginResponse
	if err := sendAndRecv(gc, stats, game.ClientPlayerLoginLoginMessageId, request, &lr); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if tip := lr.GetErrorMessage().GetId(); tip != 0 {
		return fmt.Errorf("login: server tip=%d", tip)
	}
	if lr.GetAccessToken() != "" {
		gc.SetTokens(lr.GetAccessToken(), lr.GetRefreshToken(), lr.GetAccessTokenExpire(), lr.GetRefreshTokenExpire())
	}

	found := false
	for _, p := range lr.GetPlayers() {
		if p.GetPlayer().GetPlayerId() == want {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("目标 zone 的 login 角色列表(%d 个)里没有 player_id=%d:账号 blob 没有跨 zone 共享"+
			"(CZ-2 契约:两个 zone 的 login 必须指向同一个 Redis),或 Login 被 gate 信封拒绝(回包体为空)。"+
			"拒绝自动建角 —— 建了就是假绿加脏数据", len(lr.GetPlayers()), want)
	}

	var er login.EnterGameResponse
	if err := sendAndRecv(gc, stats, game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: want}, &er); err != nil {
		return fmt.Errorf("enter game: %w", err)
	}
	if tip := er.GetErrorMessage().GetId(); tip != 0 {
		return fmt.Errorf("enter game: server tip=%d(票据持票者不符 / 换手门未放行 / 目标 zone 无可用 scene,"+
			"看目标 zone 的 login 与 scene_manager 日志)", tip)
	}
	if er.GetPlayerId() != want {
		return fmt.Errorf("enter game: 回的 player_id=%d,期望 %d", er.GetPlayerId(), want)
	}
	gc.PlayerId = er.GetPlayerId()
	return nil
}

// pace 让出与上一个请求的最小间隔(travelSmokeRequestSpacing)。
func (b *travelSmokeBot) pace() {
	if wait := travelSmokeRequestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
}

// send 按节奏发一个不关联回包的请求(ClientRequest.id = 0)。只用于单向消息(移动上报,应答类型是 Empty,
// scene TCP 路径上不回包);请求-响应调用走 call / travelSmokeStartTravel。
func (b *travelSmokeBot) send(messageId uint32, request proto.Message) error {
	b.pace()
	if err := b.gc.SendRequest(messageId, request); err != nil {
		return fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	b.lastRequestAt = time.Now()
	b.stats.MsgSent()
	return nil
}

// call 按节奏发一个请求-响应调用,只等本次请求自己的回包(smokeCaller.call;超时包裹 errTravelSmokeTimeout)。
// 返回值同 smokeCaller.call:envelopeTip 非 0 = gate 信封拒绝,此时 response 未填充;为 0 时 response 已解码。
// 节奏沿用改造前的金币读写:相邻请求的间隔从上一次调用返回时起算(比从发出时起算更保守)。
//
// 每次现建 smokeCaller 而不做成字段:ownership_fault_fullstack_test.go 用结构体字面量构造 travelSmokeBot,
// 加字段就得同步改那边;本场景也不接 gate kServiceUnavailable 的快速失败(scene 链路的拒绝走带请求号的信封),
// smokeCaller 没有要跨调用保留的状态。只由主流程调用,不得在 RecvLoop goroutine 上调用(CallSync 的约束)。
func (b *travelSmokeBot) call(messageId uint32, request, response proto.Message) (uint32, error) {
	b.pace()
	caller := newSmokeCaller(b.gc, b.stats, b.account, travelSmokeRpcTimeout, errTravelSmokeTimeout)
	envelopeTip, err := caller.call(messageId, request, response)
	b.lastRequestAt = time.Now()
	return envelopeTip, err
}

// readGold / addGold 读 / 加金币:只等本次请求自己的回包(按请求号认领)并直接解回包,被拒一律是失败
// (gate 信封拒绝与回包体 tip 非 0,判定见 travelSmokeReplyRejection)。
//
// 没有直接用 currency_crash_window_scenario.go 的 readGoldBalance / gmAddGold(它们同样按请求号认领、区分信封与业务拒绝):
// 本场景要让金币读写与传送请求共用 pace / lastRequestAt 节流(gate 按消息号限流),超时也要包裹本场景的
// errTravelSmokeTimeout。金币槽规则与信封原因复用共享的 goldSlotValue / sceneRequestEnvelopeHint,不另写一份。
// 本场景的断言全建立在余额上,把拒绝读成 0 会让失败原因指错方向,所以被拒一律报错。
//
// 不按"列表为空 / 长度不足"判失败:新号还没有任何货币记录时,空列表是合法回包(T1 的第一次读就是它)。
// 需要非 0 的地方由调用点拿余额与期望值比。
func (b *travelSmokeBot) readGold() (uint64, error) {
	response := &scene.GetCurrencyListResponse{}
	envelopeTip, err := b.call(game.SceneCurrencyClientPlayerGetCurrencyListMessageId, &scene.GetCurrencyListRequest{}, response)
	if err == nil {
		err = travelSmokeReplyRejection(envelopeTip, response)
	}
	if err != nil {
		return 0, fmt.Errorf("GetCurrencyList: %w", err)
	}
	return goldSlotValue(response), nil
}

func (b *travelSmokeBot) addGold(amount int64) (uint64, error) {
	response := &scene.GmAddCurrencyResponse{}
	envelopeTip, err := b.call(game.SceneCurrencyClientPlayerGmAddCurrencyMessageId,
		&scene.GmAddCurrencyRequest{CurrencyType: currencyTypeGold, Amount: amount}, response)
	if err == nil {
		err = travelSmokeReplyRejection(envelopeTip, response)
	}
	if err != nil {
		return 0, fmt.Errorf("GmAddCurrency{amount=%d}: %w", amount, err)
	}
	return response.GetBalanceAfter(), nil
}

// travelSmokeTipReply 是带 error_message 的回包(请求 / 响应型 RPC 的回包都长这样)。
type travelSmokeTipReply interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
}

// travelSmokeEnvelopeHint 给 gate 信封拒绝码配一句原因(拼在 "tip=<数字>" 后面),本场景三处信封分支
// (金币读写、传送等待、负向传送)共用。码的来源与定位线索以共享的 sceneRequestEnvelopeHint 为准,
// 这里只把限流的提示指到本场景自己的节流常量。
func travelSmokeEnvelopeHint(tip uint32) string {
	if tip == uint32(tiptable.CommonError_kRateLimitExceeded) {
		return " (kRateLimitExceeded: gate rate limit, the request never reached the scene; see travelSmokeRequestSpacing)"
	}
	return sceneRequestEnvelopeHint(tip)
}

// travelSmokeReplyRejection 判一次请求-响应调用是否被拒:envelopeTip 是 gate 信封拒绝码(非 0 时 reply 未填充),
// reply 是已解码的回包体。被拒返回 error,受理返回 nil。信封拒绝的原因(含 GM 闸)见 travelSmokeEnvelopeHint。
func travelSmokeReplyRejection(envelopeTip uint32, reply travelSmokeTipReply) error {
	if envelopeTip != 0 {
		return fmt.Errorf("被 gate 信封拒绝 tip=%d%s", envelopeTip, travelSmokeEnvelopeHint(envelopeTip))
	}
	if tip := reply.GetErrorMessage().GetId(); tip != 0 {
		return fmt.Errorf("scene 业务拒绝 tip=%d(常见:跨 zone 冻结闸 / 货币封禁;GmAddCurrency 还可能是 scene 的 GM 闸 —— SCENE_RUN_MODE 不是 dev)", tip)
	}
	return nil
}

// gateAddr返回当前连着的 gate("ip:port";重定向后是新 zone 的 gate)。
func (b *travelSmokeBot) gateAddr() string {
	ip, port := b.gc.GateAddr()
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

// mark 返回当前留底条数与进度快照,作为"从这之后"的扫描起点。必须在触发动作之前调用。
func (b *travelSmokeBot) mark() (int, travelSmokeProgress) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.records), b.progress
}

// snapshot 返回 since 之后的留底副本与当前进度(同一把锁下读出)。
func (b *travelSmokeBot) snapshot(since int) ([]travelSmokeRecord, travelSmokeProgress) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if since > len(b.records) {
		since = len(b.records)
	}
	return append([]travelSmokeRecord(nil), b.records[since:]...), b.progress
}

// countSince 数 since 之后留底里消息号为 messageId 的条数。
func (b *travelSmokeBot) countSince(since int, messageId uint32) int {
	records, _ := b.snapshot(since)
	n := 0
	for _, r := range records {
		if r.messageId == messageId {
			n++
		}
	}
	return n
}

// travelTo 发起一次传送并等到"人已经在目标 zone 的场景里":
// msg 124 → 严格重登成功 → 排在 124 之后的 NotifyEnterScene。返回这一跳的证据,断言由 verifyArrival 做。
//
// TravelToZone 用 gc.Call 异步发出(travelSmokeStartTravel),认领窗口与整跳预算相同;回包可有可无,
// 每圈连同留底一起交给 travelSmokeScanHop:回包带拒绝码立即失败,124 之后旧连接上的调用以 ErrRPCDisconnected 收场属正常。
// 轮询里不能调 gc.RedirectHops():它与 FollowRedirect 抢同一把 redirectMu,搬迁进行中会一直阻塞到结束。
func (b *travelSmokeBot) travelTo(targetZone, sceneConfigId uint32) (*travelSmokeHop, error) {
	b.pace()
	since, before := b.mark()
	call, err := travelSmokeStartTravel(b.gc, b.stats, targetZone, sceneConfigId, b.hopTimeout)
	if err != nil {
		return nil, fmt.Errorf("send TravelToZone{zone=%d}: %w", targetZone, err)
	}
	b.lastRequestAt = time.Now()

	deadline := time.Now().Add(b.hopTimeout)
	for {
		records, progress := b.snapshot(since)
		scan, err := travelSmokeScanHop(call.snapshot(), records)
		if err != nil {
			return nil, fmt.Errorf("TravelToZone{zone=%d}: %w", targetZone, err)
		}
		reloginDone := progress.reloginFinished > before.reloginFinished
		if scan.hop != nil {
			if reloginDone && progress.reloginErr != nil {
				return nil, fmt.Errorf("TravelToZone{zone=%d}: 已跟随 msg 124 到 %s,但严格重登失败(旧连接已关,本会话作废): %w",
					targetZone, scan.hop.targetAddr, progress.reloginErr)
			}
			if progress.redirectsHandled > before.redirectsHandled && progress.reloginStarted == before.reloginStarted {
				return nil, fmt.Errorf("TravelToZone{zone=%d}: msg 124(目标 %s)的处理已返回,但没走到重登:"+
					"地址非法 / 票据已过期 / 目标 gate 探测不通 / 验签被拒 / 环路熔断,原因见上方日志 follow gate redirect failed;"+
					"若日志是 redirect ignored: no live connection,则是 pkg.Clients 没登记", targetZone, scan.hop.targetAddr)
			}
			if scan.arrived && reloginDone {
				return scan.hop, nil
			}
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("TravelToZone{zone=%d}: %s", targetZone, travelSmokeDescribeTimeout(scan, progress, before, b.gateAddr(), b.hopTimeout))
		}
		time.Sleep(travelSmokePollInterval)
	}
}

// travelSmokeDescribeTimeout 把一跳超时的现场写成一句话:走到了哪一步、下一步该去看哪里的日志。
// progress / before 是超时那一圈与等待开始前的进度快照,gate 是当前连着的 gate。
// 窗口内收到过没回显请求号的 TravelToZone 回包、而调用既没见到回包也没见到 124 时,优先报它
// (travelSmokeUncorrelatedCause):回包其实到了(还可能是同步拒绝),"都没到"会把排查指向错误的方向。
// 写成纯函数是为了单测各分支的提示(sync_smoke_scenario_test.go)。
func travelSmokeDescribeTimeout(scan travelSmokeScan, progress, before travelSmokeProgress, gate string, hopTimeout time.Duration) string {
	redirectSeen := scan.hop != nil
	reloginStarted := progress.reloginStarted > before.reloginStarted
	reloginDone := progress.reloginFinished > before.reloginFinished
	uncorrelated := progress.uncorrelatedTravel.since(before.uncorrelatedTravel)
	uncorrelatedCause := travelSmokeUncorrelatedCause(scan, uncorrelated)
	var hint string
	switch {
	case uncorrelatedCause != "":
		hint = uncorrelatedCause
	case !redirectSeen && !scan.replySeen:
		hint = "回包与 msg 124 都没到:gate / scene 没换上带 TravelToZone 的新二进制,或请求在 scene 被静默丢弃"
	case !redirectSeen:
		hint = "已受理但没等到 msg 124:源端存盘没落地 / handoff 标记没写出 / scene_manager 换手门没放行 / 目标 zone 没有可用 gate" +
			"(看 scene 与 scene_manager 日志;受理后失败本该有 SendTipToClient,连它也没有就是源端超时看门狗也没触发)"
	case !reloginStarted:
		hint = "msg 124 已到但还没开始重登:卡在目标 gate 探测(5s)/ 换连接 / 票据验签(10s)"
	case !reloginDone:
		hint = "重登进行中:卡在目标 zone 的 Login 或 EnterGame(各 15s 上限)"
	default:
		hint = "重登成功但没等到目标 zone 的 NotifyEnterScene:第二条腿的 EnterScene / RoutePlayerEvent 没把人送进场景" +
			"(看目标 zone 的 scene_manager 与 scene 日志)"
	}
	return fmt.Sprintf("等待超时(%s):reply_seen=%t uncorrelated_replies=%d redirect_seen=%t relogin_started=%t relogin_done=%t arrived=%t 当前 gate=%s late_tips=%v。%s",
		hopTimeout, scan.replySeen, uncorrelated.count, redirectSeen, reloginStarted, reloginDone, scan.arrived, gate, scan.lateTips, hint)
}

// travelSmokeScanHop 判一跳传送窗口的现状:先看 TravelToZone 调用的结局(reply,判据见 travelSmokeJudgeTravelReply),
// 再按到达顺序扫窗口内的留底。返回 error 表示可以立刻判这一跳失败。
func travelSmokeScanHop(reply travelSmokeReplyOutcome, records []travelSmokeRecord) (travelSmokeScan, error) {
	var scan travelSmokeScan
	seen, err := travelSmokeJudgeTravelReply(reply)
	scan.replySeen = seen
	if err != nil {
		return scan, err
	}
	for _, r := range records {
		switch r.messageId {
		case game.SceneClientPlayerCommonSendTipToClientMessageId:
			var tip base.TipInfoMessage
			if err := proto.Unmarshal(r.body, &tip); err != nil || tip.GetId() == 0 {
				continue
			}
			if scan.hop == nil {
				// 受理之后、124 之前的 tip = 异步失败通知:scene 已解冻,玩家留在原地。
				return scan, fmt.Errorf("受理后失败 SendTipToClient tip=%d(目标 zone 不存在 / 无可用 gate / 繁忙,scene 已解冻)", tip.GetId())
			}
			scan.lateTips = append(scan.lateTips, tip.GetId())

		case game.SceneClientPlayerCommonKickPlayerMessageId:
			return scan, errors.New("传送途中收到 KickPlayer(目标 zone 的顶号 / 旧会话清理踢到了新会话?)")

		case game.SceneClientPlayerCommonRedirectToGateMessageId:
			hop, err := travelSmokeDecodeRedirect(r.body)
			if err != nil {
				return scan, err
			}
			if scan.hop != nil {
				return scan, fmt.Errorf("一次传送收到第二条 msg 124(第一条去 %s 票据 zone=%d,第二条去 %s 票据 zone=%d):"+
					"目标 zone 的 login 又把人弹走了,CZ-8(认票据 target_zone_id、不按 home_zone 弹回)没生效?",
					scan.hop.targetAddr, scan.hop.tokenZone, hop.targetAddr, hop.tokenZone)
			}
			scan.hop = hop

		case game.SceneSceneClientPlayerNotifyEnterSceneMessageId:
			// 只认排在 124 之后的第一条:之前的是源 zone 的,之后的才是目标 zone 的。
			if scan.hop == nil || scan.arrived {
				continue
			}
			var entered scene.EnterSceneS2C
			if err := proto.Unmarshal(r.body, &entered); err != nil {
				return scan, fmt.Errorf("decode NotifyEnterScene: %w", err)
			}
			scan.arrived = true
			scan.hop.sceneId = entered.GetSceneInfo().GetSceneId()
			scan.hop.sceneConfigId = entered.GetSceneInfo().GetSceneConfigId()
		}
	}
	return scan, nil
}

// travelSmokeJudgeTravelReply 把传送等待期间 TravelToZone 调用的结局翻成判据。seen 表示回包(含信封拒绝)到了;
// err 非 nil 表示可以立刻判这一跳失败。
//
// 回包是可选的,下列都不算失败,落地与否由留底里的 124 / NotifyEnterScene 决定:还没有结局;
// pkg.ErrRPCDisconnected(124 一到 SwapConn 就让旧连接上的在途调用以它收场,回包从此收不到 —— 正常路径);
// pkg.ErrRPCTimeout(认领窗口内没回包)。回包带拒绝码(信封或回包体)、回包体解不开、其余错误立即失败。
func travelSmokeJudgeTravelReply(reply travelSmokeReplyOutcome) (seen bool, err error) {
	if !reply.settled {
		return false, nil
	}
	var tipErr *pkg.ServerTipError
	switch {
	case reply.err == nil:
		tip, decodeErr := travelToZoneRejectTip(reply.reply.GetSerializedMessage())
		if decodeErr != nil {
			return true, fmt.Errorf("decode TravelToZoneResponse: %w", decodeErr)
		}
		if tip != 0 {
			return true, fmt.Errorf("scene 同步拒绝 tip=%d(战斗 / 组队在途、已有传送在途、目标 zone 非法等 CZ-6 校验)", tip)
		}
		return true, nil
	case errors.As(reply.err, &tipErr):
		return true, fmt.Errorf("gate 信封拒绝 tip=%d%s", tipErr.Tip, travelSmokeEnvelopeHint(tipErr.Tip))
	case errors.Is(reply.err, pkg.ErrRPCDisconnected), errors.Is(reply.err, pkg.ErrRPCTimeout):
		return false, nil
	default:
		return false, fmt.Errorf("TravelToZone call failed: %w", reply.err)
	}
}

// travelSmokeDecodeRedirect 解 msg 124 与它带的票据。
// token_payload 是 GateTokenPayload 的序列化结果(scene_manager gate_redirect.go AssignGateForZone 签发),
// 客户端原样转发给目标 gate 验签;这里只是读出来当落区证据,不做验签。
func travelSmokeDecodeRedirect(body []byte) (*travelSmokeHop, error) {
	var notify scene.RedirectToGateNotify
	if err := proto.Unmarshal(body, &notify); err != nil {
		return nil, fmt.Errorf("decode RedirectToGateNotify: %w", err)
	}
	hop := &travelSmokeHop{
		targetAddr: net.JoinHostPort(notify.GetTargetIp(), strconv.Itoa(int(notify.GetTargetPort()))),
	}
	if len(notify.GetTokenPayload()) == 0 {
		// 没带票据:tokenZone 保持 0,由 verifyArrival 以"无法证明落区"判失败。
		return hop, nil
	}
	var payload base.GateTokenPayload
	if err := proto.Unmarshal(notify.GetTokenPayload(), &payload); err != nil {
		return nil, fmt.Errorf("decode GateTokenPayload(msg 124 目标 %s): %w", hop.targetAddr, err)
	}
	hop.tokenZone = payload.GetZoneId()
	hop.ticketPlayer, hop.ticketTargetZone = travelSmokeTicketBinding(&payload)
	return hop, nil
}

// verifyArrival 对一跳落地做断言,返回失败的子步骤名(拼进 TRAVEL_SMOKE_FAIL 的 step)与原因。
// 不能只看"等到了 NotifyEnterScene":被弹回家、落到别的 zone、换了角色,都等得到它。
func (b *travelSmokeBot) verifyArrival(hop *travelSmokeHop, wantZone, wantSceneConfigId uint32, prevGate string) (string, error) {
	// 读 gc.PlayerId 是安全的:travelTo 是在锁下看到 124 之后的 NotifyEnterScene 才返回的,
	// 那条留底由 RecvLoop goroutine 在重登(写 PlayerId)返回之后才追加。
	if b.gc.PlayerId != b.homePlayer {
		return "identity", fmt.Errorf("player_id 变了 %d → %d(重登进了别的角色)", b.homePlayer, b.gc.PlayerId)
	}
	if hop.tokenZone == 0 {
		return "zone", fmt.Errorf("msg 124(目标 %s)没带票据或票据 zone_id=0,无法证明落区"+
			"(scene_manager 的 GateTokenSecret 为空时本该直接拒绝签发)", hop.targetAddr)
	}
	if hop.tokenZone != wantZone {
		return "zone", fmt.Errorf("票据 zone=%d,期望 %d(目标 %s):被送到了别的 zone", hop.tokenZone, wantZone, hop.targetAddr)
	}
	// CZ-8 的两条:票据必须绑定到本人、且指明本跳的目标 zone。这两条无条件执行 ——
	// 曾经它们挂在一个"取不到字段就跳过"的开关后面,开关长期为 false,整场冒烟照样报 OK。
	if hop.ticketPlayer != b.homePlayer {
		return "ticket", fmt.Errorf("票据 player_id=%d,期望持票者 %d(CZ-8:票据必须绑定本人)", hop.ticketPlayer, b.homePlayer)
	}
	if hop.ticketTargetZone != wantZone {
		return "ticket", fmt.Errorf("票据 target_zone_id=%d,期望 %d(CZ-8:目标 zone 的 login 靠它决定不弹回)", hop.ticketTargetZone, wantZone)
	}
	gate := b.gateAddr()
	if gate == prevGate {
		return "gate", fmt.Errorf("gate 地址没变(%s):没有真正换到另一个 zone 的 gate", gate)
	}
	if gate != hop.targetAddr {
		return "gate", fmt.Errorf("当前连着 %s,但 msg 124 指向的是 %s(落地之后又被重定向走了?)", gate, hop.targetAddr)
	}
	if wantSceneConfigId != 0 && hop.sceneConfigId != wantSceneConfigId {
		return "scene", fmt.Errorf("落地地图 scene_config_id=%d,期望 %d(目标地图没有随「等待落点」传到第二条腿,或该图在 zone %d 不存在)",
			hop.sceneConfigId, wantSceneConfigId, wantZone)
	}
	return "", nil
}

// verifyUndisturbed 断言 since 之后没有被踢、没有被再次重定向,并且还连着 wantGate。
// 用来兜住"落地断言做完之后才发生的弹回":那时 travelTo 已经返回,没人再盯着 124。
func (b *travelSmokeBot) verifyUndisturbed(since int, wantGate string) error {
	records, _ := b.snapshot(since)
	for _, r := range records {
		switch r.messageId {
		case game.SceneClientPlayerCommonKickPlayerMessageId:
			return errors.New("收到 KickPlayer(源 zone 断线租约到期的清理 / 顶号踢到了已经搬走的会话?)")
		case game.SceneClientPlayerCommonRedirectToGateMessageId:
			return errors.New("落地之后又收到一条 msg 124:被二次重定向了")
		}
	}
	if gate := b.gateAddr(); gate != wantGate {
		return fmt.Errorf("当前连着 %s,期望仍在 %s", gate, wantGate)
	}
	return nil
}

// expectTravelRejected 发一个必须被**同步**拒绝的传送请求,返回服务器给的拒绝码(只保证 != 0,不与任何常量比较)。
// 没有重定向发生,旧连接不会关,所以这里的回包是必需的(与 travelTo 相反):按请求号认领,认领窗口
// travelSmokeRpcTimeout 内没回包即失败。
//
// 用 gc.Call 异步发出而不是阻塞等回包:等待期间还要盯着留底里的 msg 124。每圈先取调用结局、再数 124 ——
// 124 的留底在 FollowRedirect 换连接之前追加,所以结局是 ErrRPCDisconnected 时这一圈一定数得到它,报的是重定向。
// 被受理的回包先于 124 到达时报「回包 tip=0」,与改造前按到达顺序判的结论相同。
// 有结局后才读没回显请求号的回包诊断:超时结局要等满认领窗口,窗口内落到 onMessage 的那条此时已经记下。
func (b *travelSmokeBot) expectTravelRejected(targetZone, sceneConfigId uint32) (uint32, error) {
	b.pace()
	since, before := b.mark()
	call, err := travelSmokeStartTravel(b.gc, b.stats, targetZone, sceneConfigId, travelSmokeRpcTimeout)
	if err != nil {
		return 0, fmt.Errorf("send TravelToZone{zone=%d}: %w", targetZone, err)
	}
	b.lastRequestAt = time.Now()

	// 不另设截止时间:Call 的定时器保证结局在 travelSmokeRpcTimeout 内一定写入。
	for {
		reply := call.snapshot()
		if b.countSince(since, game.SceneClientPlayerCommonRedirectToGateMessageId) > 0 {
			return 0, fmt.Errorf("TravelToZone{zone=%d} 本该被拒,却触发了 msg 124 重定向", targetZone)
		}
		if reply.settled {
			_, progress := b.snapshot(since)
			return travelSmokeJudgeRejection(targetZone, reply, progress.uncorrelatedTravel.since(before.uncorrelatedTravel))
		}
		time.Sleep(travelSmokePollInterval)
	}
}

// travelSmokeJudgeRejection 判 expectTravelRejected 那次调用的结局:回包体拒绝码非 0 才通过并返回该码;
// 受理(tip=0)、信封拒绝(请求没到 scene)、认领窗口内没回包、其余错误都返回 error。reply 必须已有结局。
// uncorrelated 是等待窗口内没回显请求号的 TravelToZone 回包(已按窗口截取),只改写超时分支的原因:
// 那时回包其实到了(回包体里可能正是要等的拒绝码),但认领不到,判定仍是失败。
func travelSmokeJudgeRejection(targetZone uint32, reply travelSmokeReplyOutcome, uncorrelated travelSmokeUncorrelatedReplies) (uint32, error) {
	var tipErr *pkg.ServerTipError
	switch {
	case !reply.settled:
		return 0, fmt.Errorf("TravelToZone{zone=%d}: judged before the call settled (caller bug)", targetZone)
	case reply.err == nil:
		tip, err := travelToZoneRejectTip(reply.reply.GetSerializedMessage())
		if err != nil {
			return 0, fmt.Errorf("decode TravelToZoneResponse: %w", err)
		}
		if tip == 0 {
			return 0, fmt.Errorf("TravelToZone{zone=%d} 回包 tip=0（被受理了）：scene 没校验目标 zone，"+
				"或 handler 没把拒绝码写进 response->error_message", targetZone)
		}
		return tip, nil
	case errors.As(reply.err, &tipErr):
		return 0, fmt.Errorf("TravelToZone{zone=%d} 被 gate 信封拒绝 tip=%d%s:不是 scene 业务逻辑给的拒绝码,不能算业务拒绝",
			targetZone, tipErr.Tip, travelSmokeEnvelopeHint(tipErr.Tip))
	case errors.Is(reply.err, pkg.ErrRPCTimeout):
		if cause := uncorrelated.describe(); cause != "" {
			return 0, fmt.Errorf("TravelToZone{zone=%d}: no claimable reply within %s: %s", targetZone, travelSmokeRpcTimeout, cause)
		}
		return 0, fmt.Errorf("TravelToZone{zone=%d} %s 内没有回包(同步拒绝必须有回包;gate / scene 没换新二进制?)",
			targetZone, travelSmokeRpcTimeout)
	default:
		return 0, fmt.Errorf("TravelToZone{zone=%d}: %w", targetZone, reply.err)
	}
}

// probeMove 是"访客能不能动"的尽力探针,返回是否收到 MoveAck。
//
// 为什么只能尽力:MoveStart / MoveStop 的返回类型是 Empty,没有回包可断言;MoveAck 只在服务器裁决位置与上报位置
// 水平差超过阈值时才回发(player_movement_handler.cpp ApplyReportedLocation),场景没有导航网格时 fail-open 不回。
// 所以上报一个远在地图之外的点来触发纠偏。硬断言由请求 / 响应型的金币读写承担。
// 返回 error 只表示"发不出去"(连接已断),那是真实故障。
func (b *travelSmokeBot) probeMove() (bool, error) {
	since, _ := b.mark()
	location := &component.Location{X: travelSmokeProbeCoord, Y: travelSmokeProbeCoord}

	b.moveInputSeq++
	if err := b.send(game.SceneMovementClientPlayerMoveStartMessageId, &scene.MoveStartC2S{
		StartLocation: location,
		ClientTimeMs:  uint64(time.Now().UnixMilli()),
		InputSeq:      b.moveInputSeq,
	}); err != nil {
		return false, err
	}
	b.moveInputSeq++
	if err := b.send(game.SceneMovementClientPlayerMoveStopMessageId, &scene.MoveStopC2S{
		EndLocation:  location,
		ClientTimeMs: uint64(time.Now().UnixMilli()),
		InputSeq:     b.moveInputSeq,
	}); err != nil {
		return false, err
	}

	deadline := time.Now().Add(travelSmokeMoveAckTimeout)
	for {
		if b.countSince(since, game.SceneMovementClientPlayerNotifyMoveAckMessageId) > 0 {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(travelSmokePollInterval)
	}
}
