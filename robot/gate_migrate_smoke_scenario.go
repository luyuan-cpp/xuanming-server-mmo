package main

// gate-migrate-smoke 场景:gate 排空迁移的端到端冒烟(C++ 仓 docs/design/gate-drain-session-migration.md §11;
// 到达侧的断线保留见 docs/design/player-connection-lost-hold.md)。
//
//	G1 登录进场:gateMigrateSmokeBots 个机器人(robot_9961 起)登录进场,各自记下所在 gate(地址,以及登录票据里的节点号)、
//	   所在场景与自己的 actor 号。
//	G2 等排空标记:打印每台 gate 上有几个机器人、它的排空标记键,以及给其中一台打标记的 redis-cli 命令,然后等。
//	   本进程不碰 Redis(robot 没有 Redis 依赖,打标记本来也是运维动作):由跑冒烟的人在另一个终端执行那条命令,
//	   或由外层脚本读到 GATE_MIGRATE_SMOKE_WAITING 一行后执行。键名取自 shared/gatepick(login 读写的就是那一份定义)。
//	G3 迁移:第一个收到 msg 124 的机器人所在的 gate 就是被排空的那台;它上面的全部机器人都必须在期限内迁走。
//	   每个迁走的机器人:票据签给了别的 gate;在新连接上又进了一次场景,场景重新通告了它自己的 actor;场景号与
//	   actor 号都没变(actor 号按实体发、进程内不复用,没变说明断线保留接回的是同一个实体,不是从库里重新加载);
//	   没有收到踢线推送(34)。
//	G4 其余 gate 上的机器人不受影响:没收到 124,gate 没变,也没有再次进场。
//	G5 迁走的机器人在新连接上照常工作:发给场景的请求有应答(SceneInfoC2S → NotifySceneInfo),心跳继续有回包。
//
// 机器人收到 msg 124 之后的换连接、验票、重新 Login + EnterGame 是通用路径(pkg/redirect.go 的 FollowRedirect,
// 重登实现由 main.go 注册),本场景不另写,只观察结果。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `GATE_MIGRATE_SMOKE_OK drained_gate=… migrated=… stayed=… slowest_ms=…`,退出码 0;
//	任一步失败 → `GATE_MIGRATE_SMOKE_FAIL step=… reason=…`,退出码 1。每步通过另打一行 `[gate-migrate-smoke] PASS step=…`。
//
// 前置条件见 etc/gate_migrate_smoke.yaml 文件头。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"
	"shared/gatepick"
)

const (
	// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。996x 与其它冒烟的账号段错开(heartbeat-idle 是 995x)。
	gateMigrateSmokeFirstAccount = 9961
	// 机器人数。4 个足够同时看到「被排空那台上的全部迁走」与「别的 gate 上的不动」(两台 gate 时通常各落两个)。
	gateMigrateSmokeBots = 4

	// 首登等进场、等自己的 actor 号的预算,与 heartbeat-idle-smoke 同值。
	gateMigrateSmokeSceneReadyTimeout = 15 * time.Second
	gateMigrateSmokeActorTimeout      = 5 * time.Second
	// 等人去打排空标记的期限。
	gateMigrateSmokeMarkWait = 5 * time.Minute
	// 第一个机器人收到 124 之后,同一台 gate 上其余机器人迁完的期限。login 每 5 秒续一次许可,gate 每秒迁至多 20 个,
	// 每个机器人换连接加重登是秒级,正常远用不完;没要到票的连接 10 秒后重试一次也在期限内。
	gateMigrateSmokeDrainBudget = 60 * time.Second
	// 迁完之后再看一会儿才下结论:迟到的踢线推送、对别的 gate 的误迁都在这段时间里露头。
	gateMigrateSmokeSettle = 3 * time.Second
	// 等场景应答 SceneInfoC2S 的预算。正常是毫秒级。
	gateMigrateSmokeSceneReplyWait = 5 * time.Second
	// 等新连接上的心跳回包。gate 通告的间隔是 10 秒,验签后立即发的那一条可能在采样之前就回来了,所以留一个间隔加余量。
	gateMigrateSmokeHeartbeatWait = 15 * time.Second
	// 打印给人执行的那条 SET 命令的 TTL(秒):标记的人忘了清,这台 gate 也只停接新玩家这么久。
	gateMigrateSmokeMarkTTLSeconds = 600
)

var errGateMigrateNoToken = errors.New("gate issued no token (empty GateTokenSecret, dev pass-through): " +
	"a migration ticket is a gate token, so the gate cannot redirect anyone without the secret")

// gateMigrateStage 是一个机器人在迁移里走到的位置,按先后排列。
type gateMigrateStage int

const (
	gateMigrateStageHome       gateMigrateStage = iota // 没收到 124
	gateMigrateStageRedirected                         // 收到了 124,还连着原来的 gate
	gateMigrateStageConnected                          // 已换到新 gate,还没在新连接上进场
	gateMigrateStageEntered                            // 已再次进场,场景还没通告它自己的 actor
	gateMigrateStageArrived                            // 迁移完成
)

func (s gateMigrateStage) String() string {
	switch s {
	case gateMigrateStageHome:
		return "no redirect received"
	case gateMigrateStageRedirected:
		return "redirect received but still on the old gate (target unreachable or ticket expired, see 'follow gate redirect failed')"
	case gateMigrateStageConnected:
		return "on the new gate but not in a scene (re-login or scene entry failed or is still running, see 'follow gate redirect failed')"
	case gateMigrateStageEntered:
		return "re-entered the scene, own actor not announced yet"
	case gateMigrateStageArrived:
		return "arrived"
	default:
		return fmt.Sprintf("stage(%d)", int(s))
	}
}

type gateMigrateBot struct {
	account  string
	playerID uint64
	gc       *pkg.GameClient
	player   *gameobject.Player
	stats    *metrics.Stats

	// 登录进场时的快照(G1),之后只读。
	homeGateNode uint32 // 登录票据里的 gate_node_id,也就是排空标记键里的那个号
	homeGateZone uint32 // 这台 gate 所在的 zone,排空标记键按它打 hash tag
	homeGateAddr string
	sceneID      uint64
	entity       uint64
	entries      int // 当时已收到的进场通知数

	// 以下由 RecvLoop goroutine 写、主流程读。
	kicked        atomic.Bool
	selfAnnounced atomic.Uint64 // 场景通告「自己这个 actor」的次数(ActorCreate / ActorListCreate 里 guid = 自己)
	// 最近一条 msg 124 到达那一刻的快照:票据签给哪台 gate、当时的进场次数与自己被通告的次数。
	// 先写这三项、最后才加 redirects:主流程看到 redirects > 0 时,快照一定已经写好。
	// 124 之后的进场通知与 actor 通告都来自新连接(旧连接从换连接那一刻起没人再读),所以拿它们与快照比,
	// 就是「新连接上有没有再进场、有没有再通告」,不受首登时迟到的推送影响。
	targetGateNode      atomic.Uint32
	entriesAtRedirect   atomic.Int64
	announcedAtRedirect atomic.Uint64
	redirects           atomic.Uint64
}

func (b *gateMigrateBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	b.observe(msg)
	handler.MessageBodyHandler(client, msg)
}

// observe 在消息交给通用分发之前登记本场景要看的几种推送。
// 124 必须在分发之前登记:通用分发处理 124 时会在本 goroutine 里同步换连接、重登,并把重登期间到达的推送补投回来,
// 等它返回时新连接上的进场通知已经处理完了。
func (b *gateMigrateBot) observe(msg *base.MessageContent) {
	if msg.GetErrorMessage().GetId() != 0 {
		return // 信封拒绝没有消息体
	}
	body := msg.GetSerializedMessage()
	switch msg.GetMessageId() {
	case game.SceneClientPlayerCommonKickPlayerMessageId:
		b.kicked.Store(true)
	case game.SceneSceneClientPlayerNotifyActorCreateMessageId:
		var actor scene.ActorCreateS2C
		if proto.Unmarshal(body, &actor) == nil && actor.GetGuid() == b.playerID {
			b.selfAnnounced.Add(1)
		}
	case game.SceneSceneClientPlayerNotifyActorListCreateMessageId:
		var list scene.ActorListCreateS2C
		if proto.Unmarshal(body, &list) != nil {
			return
		}
		for _, actor := range list.GetActorList() {
			if actor.GetGuid() == b.playerID {
				b.selfAnnounced.Add(1)
				break
			}
		}
	case game.SceneClientPlayerCommonRedirectToGateMessageId:
		var notify scene.RedirectToGateNotify
		var ticket base.GateTokenPayload
		target := uint32(0)
		if proto.Unmarshal(body, &notify) == nil && proto.Unmarshal(notify.GetTokenPayload(), &ticket) == nil {
			target = ticket.GetGateNodeId()
		}
		b.targetGateNode.Store(target)
		b.entriesAtRedirect.Store(int64(b.player.GetSceneEnterCount()))
		b.announcedAtRedirect.Store(b.selfAnnounced.Load())
		b.redirects.Add(1)
	}
}

func (b *gateMigrateBot) gateAddr() string {
	ip, port := b.gc.GateAddr()
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

func (b *gateMigrateBot) stage() gateMigrateStage {
	switch {
	case b.redirects.Load() == 0:
		return gateMigrateStageHome
	case b.gateAddr() == b.homeGateAddr:
		return gateMigrateStageRedirected
	case int64(b.player.GetSceneEnterCount()) <= b.entriesAtRedirect.Load():
		return gateMigrateStageConnected
	case b.selfAnnounced.Load() <= b.announcedAtRedirect.Load():
		return gateMigrateStageEntered
	default:
		return gateMigrateStageArrived
	}
}

func (b *gateMigrateBot) describe() string {
	return fmt.Sprintf("%s(player=%d home_gate=%d %s now=%s target_gate=%d redirects=%d scene=%d entity=%d entries=%d->%d kicked=%v: %s)",
		b.account, b.playerID, b.homeGateNode, b.homeGateAddr, b.gateAddr(), b.targetGateNode.Load(), b.redirects.Load(),
		b.player.GetSceneID(), b.player.GetEntityID(), b.entries, b.player.GetSceneEnterCount(), b.kicked.Load(), b.stage())
}

// RunGateMigrateSmoke 是 main.go `mode: gate-migrate-smoke` 的入口。任一步失败以退出码 1 结束进程。
func RunGateMigrateSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	var bots []*gateMigrateBot
	cleanup := func() {
		for _, b := range bots {
			_ = leaveGame(b.gc, stats)
			pkg.Clients.Unregister(b.playerID, b.gc)
			gameobject.PlayerList.Delete(b.playerID)
			b.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("GATE_MIGRATE_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	pass := func(step, format string, args ...any) {
		zap.L().Info(fmt.Sprintf("[gate-migrate-smoke] PASS step=%s %s", step, fmt.Sprintf(format, args...)))
	}

	// ---- G1:登录进场,记下各自所在的 gate、场景与 actor 号 ----
	for i := 0; i < gateMigrateSmokeBots; i++ {
		account := fmt.Sprintf("robot_%04d", gateMigrateSmokeFirstAccount+i)
		bot, err := gateMigrateLogin(cfg, account, stats)
		if err != nil {
			fail("G1-login", "account=%s zone=%d err=%v", account, cfg.ZoneID, err)
		}
		bots = append(bots, bot)
	}
	byGate := make(map[uint32][]*gateMigrateBot)
	for _, b := range bots {
		byGate[b.homeGateNode] = append(byGate[b.homeGateNode], b)
	}
	gateNodes := make([]uint32, 0, len(byGate))
	for node := range byGate {
		gateNodes = append(gateNodes, node)
	}
	sort.Slice(gateNodes, func(i, j int) bool { return gateNodes[i] < gateNodes[j] })
	pass("G1-login", "robots=%d gates_in_use=%d", len(bots), len(gateNodes))

	// ---- G2:等人给其中一台 gate 打排空标记 ----
	for _, node := range gateNodes {
		home := byGate[node][0]
		zap.L().Info(fmt.Sprintf("[gate-migrate-smoke] gate node_id=%d zone=%d addr=%s robots=%d draining_key=%s",
			node, home.homeGateZone, home.homeGateAddr, len(byGate[node]), gatepick.DrainingKey(home.homeGateZone, node)))
	}
	if len(gateNodes) < 2 {
		zap.L().Warn("[gate-migrate-smoke] every robot landed on the same gate: step G4 (robots on other gates are left alone) " +
			"has nothing to check in this run")
	}
	example := byGate[gateNodes[0]][0]
	zap.L().Info(fmt.Sprintf("GATE_MIGRATE_SMOKE_WAITING mark ONE of the gates above as draining on the Redis that login reads gate marks from "+
		"(waiting up to %s; add -c when that Redis is a cluster), for example: redis-cli SET '%s' %d EX %d",
		gateMigrateSmokeMarkWait, gatepick.DrainingKey(example.homeGateZone, example.homeGateNode),
		time.Now().Unix(), gateMigrateSmokeMarkTTLSeconds))

	// ---- G3:被排空那台上的机器人全部迁走,落回原场景里的同一个实体 ----
	var first *gateMigrateBot
	if !waitUntil(gateMigrateSmokeMarkWait, func() bool {
		for _, b := range bots {
			if b.redirects.Load() > 0 {
				first = b
				return true
			}
		}
		return false
	}) {
		fail("G3-mark", "no robot received a redirect within %s. Check, in this order: the draining mark was written to the Redis login "+
			"reads gate marks from, under a draining_key listed above, with a TTL and a Unix-seconds value; the zone has at least two gates "+
			"(with one, login answers NO_TARGET); login runs with GateDrain.Migrate.Enabled=true and logs "+
			"'[GateDrain] gate N: migration lease sent'; login's goctl wrapper implements RequestGateMigration (go/build.ps1), "+
			"otherwise every ticket request is UNIMPLEMENTED; the gate logs '[DrainMigration] started'. robots: %s",
			gateMigrateSmokeMarkWait, gateMigrateDescribeAll(bots))
	}
	firstRedirectAt := time.Now()
	drainedGate := first.homeGateNode
	migrating := byGate[drainedGate]
	if !waitUntil(gateMigrateSmokeDrainBudget, func() bool {
		for _, b := range migrating {
			if b.stage() != gateMigrateStageArrived {
				return false
			}
		}
		return true
	}) {
		fail("G3-migrate", "gate node_id=%d is draining but not every robot on it finished migrating within %s: %s",
			drainedGate, gateMigrateSmokeDrainBudget, gateMigrateDescribeAll(migrating))
	}
	slowest := time.Since(firstRedirectAt)
	time.Sleep(gateMigrateSmokeSettle)
	for _, b := range migrating {
		if target := b.targetGateNode.Load(); target == 0 || target == drainedGate {
			fail("G3-target", "%s was redirected with a ticket for gate node_id=%d; want another gate than the draining one (%d)",
				b.account, target, drainedGate)
		}
		if got := b.player.GetSceneID(); got != b.sceneID {
			fail("G3-resume", "%s landed in scene %d after the migration, want the original scene %d: the arrival did not resume in place",
				b.account, got, b.sceneID)
		}
		if got := b.player.GetEntityID(); got != b.entity {
			fail("G3-resume", "%s actor id changed %d -> %d: the scene loaded a fresh entity instead of resuming the held one "+
				"(the disconnect hold was not entered, or expired before the new connection entered)", b.account, b.entity, got)
		}
		if b.kicked.Load() {
			fail("G3-no-kick", "%s received KickPlayer(34) during the migration: a redirected session must be closed without the "+
				"'logged in elsewhere' notice", b.account)
		}
	}
	pass("G3-migrate", "drained_gate=%d migrated=%d slowest_ms=%d", drainedGate, len(migrating), slowest.Milliseconds())

	// ---- G4:别的 gate 上的机器人不受影响 ----
	stayed := 0
	for _, b := range bots {
		if b.homeGateNode == drainedGate {
			continue
		}
		if b.redirects.Load() != 0 || b.gateAddr() != b.homeGateAddr || b.player.GetSceneEnterCount() != b.entries || b.kicked.Load() {
			fail("G4-undisturbed", "a robot on a gate that is not draining was moved or re-entered: %s", b.describe())
		}
		stayed++
	}
	pass("G4-undisturbed", "stayed=%d", stayed)

	// ---- G5:迁走的机器人在新连接上照常工作 ----
	heartbeatsBefore := make([]uint64, len(migrating))
	scenePushBefore := make([]uint64, len(migrating))
	for i, b := range migrating {
		heartbeatsBefore[i] = b.gc.HeartbeatReplies()
		_, scenePushBefore[i] = b.player.GetSceneInfoPush()
		// SceneInfoC2S 的应答类型是 Empty,场景的应答是一条 NotifySceneInfo 推送(与 channel-smoke 的列线同一条路)。
		if err := b.gc.SendRequest(game.SceneSceneClientPlayerSceneInfoC2SMessageId, &scene.SceneInfoRequest{}); err != nil {
			fail("G5-scene", "%s could not send SceneInfoC2S on the new connection: %v", b.account, err)
		}
		stats.MsgSent()
	}
	if !waitUntil(gateMigrateSmokeSceneReplyWait, func() bool {
		for i, b := range migrating {
			if _, seq := b.player.GetSceneInfoPush(); seq <= scenePushBefore[i] {
				return false
			}
		}
		return true
	}) {
		fail("G5-scene", "the scene did not answer SceneInfoC2S within %s for at least one migrated robot: the new gate session "+
			"is not routed to the scene the entity lives in. robots: %s", gateMigrateSmokeSceneReplyWait, gateMigrateDescribeAll(migrating))
	}
	if !waitUntil(gateMigrateSmokeHeartbeatWait, func() bool {
		for i, b := range migrating {
			if b.gc.HeartbeatReplies() <= heartbeatsBefore[i] {
				return false
			}
		}
		return true
	}) {
		fail("G5-heartbeat", "no heartbeat reply on the new connection within %s for at least one migrated robot: %s",
			gateMigrateSmokeHeartbeatWait, gateMigrateDescribeAll(migrating))
	}
	pass("G5-alive", "the scene answers requests and heartbeats keep getting replies on the new connections")

	zap.L().Info(fmt.Sprintf("GATE_MIGRATE_SMOKE_OK drained_gate=%d migrated=%d stayed=%d slowest_ms=%d",
		drainedGate, len(migrating), stayed, slowest.Milliseconds()))
	zap.L().Info(fmt.Sprintf("[gate-migrate-smoke] clear the mark when done: redis-cli DEL '%s' '%s'",
		gatepick.DrainingKey(first.homeGateZone, drainedGate), gatepick.DrainedKey(first.homeGateZone, drainedGate)))
	cleanup()
	_ = zap.L().Sync()
}

func gateMigrateDescribeAll(bots []*gateMigrateBot) string {
	parts := make([]string, 0, len(bots))
	for _, b := range bots {
		parts = append(parts, b.describe())
	}
	return strings.Join(parts, "; ")
}

// gateMigrateLogin 用 account 登录进场,等到进场推送与自己的 actor 号都到了才返回,并记下 G1 的快照。
func gateMigrateLogin(cfg *config.Config, account string, stats *metrics.Stats) (*gateMigrateBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate address: %w", err)
	}
	if len(tokenPayload) == 0 {
		return nil, errGateMigrateNoToken
	}
	var ticket base.GateTokenPayload
	if err := proto.Unmarshal(tokenPayload, &ticket); err != nil {
		return nil, fmt.Errorf("decode the login gate token: %w", err)
	}
	if ticket.GetGateNodeId() == 0 {
		return nil, errors.New("the login gate token carries gate_node_id 0; cannot tell which gate to mark as draining")
	}
	// 标记键按 gate 所在的 zone 打 tag。票据里的 zone 就是签给的那台 gate 的 zone;老票据不带时退到配置里要登录的 zone。
	gateZone := ticket.GetZoneId()
	if gateZone == 0 {
		gateZone = cfg.ZoneID
	}
	if gateZone == 0 {
		return nil, errors.New("neither the login gate token nor zone_id in the config names the gate's zone; " +
			"the draining mark key cannot be built without it")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad gate port %q: %w", portStr, err)
	}
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("connect gate %s:%s: %w", host, portStr, err)
	}
	if err := loginAndEnterScenario(gc, cfg.Password, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("login and enter: %w", err)
	}

	bot := &gateMigrateBot{
		account:      account,
		playerID:     gc.PlayerId,
		gc:           gc,
		player:       gameobject.NewPlayer(gc.PlayerId),
		stats:        stats,
		homeGateNode: ticket.GetGateNodeId(),
		homeGateZone: gateZone,
	}
	bot.homeGateAddr = bot.gateAddr()
	gameobject.PlayerList.Set(bot.playerID, bot.player)
	// 不登记的话 msg 124 的 handler 反查不到连接,只打一行 WARN 就返回,迁移表现为这个机器人不动。
	pkg.Clients.Register(bot.playerID, gc)
	go gc.RecvLoop(bot.onMessage)

	discard := func() {
		pkg.Clients.Unregister(bot.playerID, gc)
		gameobject.PlayerList.Delete(bot.playerID)
		gc.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), gateMigrateSmokeSceneReadyTimeout)
	defer cancel()
	if err := bot.player.WaitSceneReady(ctx); err != nil {
		discard()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	if !waitUntil(gateMigrateSmokeActorTimeout, func() bool { return bot.player.GetEntityID() != 0 }) {
		discard()
		return nil, errors.New("entered the scene but never received its own ActorCreate (no actor id)")
	}

	// 这三项在 RecvLoop 起来之后才写,但只有主流程读写它们(RecvLoop 上的 observe 不碰)。
	bot.sceneID = bot.player.GetSceneID()
	bot.entity = bot.player.GetEntityID()
	bot.entries = bot.player.GetSceneEnterCount()
	return bot, nil
}
