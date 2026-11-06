package main

// heartbeat-idle-smoke 场景:gate 客户端心跳判死的端到端冒烟(C++ 仓 docs/design/gate-client-heartbeat.md;
// 断线保留见 docs/design/player-connection-lost-hold.md)。
//
//	H1 登录进场:验签回包通告了心跳(间隔 10s、判死 30s,两版服务器同值),验签后立即发出的第一条心跳收到回包,
//	   说明 gate 已在按空闲超时跟踪这条连接;记下所在场景与自己的 actor 号。
//	H2 静默:主机器人停发心跳,连接保持打开、一帧都不发(本场景不起 AI 循环与令牌刷新器)。
//	H3 线上判死(趁 H2 在等时做):另开一条裸 TCP,只验签、发一条心跳、收到回包后静默。gate 必须在这条心跳之后
//	   29.5–35 秒内关掉它(本端读到 EOF / 连接重置)。不走 pkg.GameClient:vendored muduo 断线后静默重拨、Recv 不报错,
//	   看不到「gate 关了连接」这件事。裸连接没有绑定角色,只量判死的时刻。
//	H4 等到主机器人静默满「判死阈值 + 余量」。
//	H5 同账号新连接重登:落回原场景,自己的 actor 号不变。actor 号按实体发、进程内不复用
//	   (cpp/libs/services/scene/actor/actor_net_id.h),号不变说明断线保留接回的是同一个实体,不是从库里重新加载。
//	H6 旧连接没收到踢线推送(34):旧连接在重登之前已被 gate 关掉,login 走的是短重连。gate 没判死的话,
//	   重登会走顶号,gate 先往旧连接推 34 再关,这里就会看到。
//
// H5 + H6 合起来证明:客户端静默 → gate 判死关连接 → scene 断线保留 → 短重连接回同一个实体。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `HEARTBEAT_IDLE_SMOKE_OK player_id=… scene_id=… entity=… wire_close_ms=…`,退出码 0;
//	任一步失败 → `HEARTBEAT_IDLE_SMOKE_FAIL step=… reason=…`,退出码 1。每步通过另打一行 `[heartbeat-idle-smoke] PASS step=…`。
//
// 跑一次约 45 秒,大部分时间在等 gate 判死。前置条件见 etc/heartbeat_idle_smoke.yaml 文件头。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"
	"go.uber.org/zap"

	"proto/common/base"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。995x 与其它冒烟的账号段错开
// (battle 900x、属性 / 宠物 910x、guild 92xx、team 93xx、trade 94xx、资产通道 95xx、travel 96xx、friend 97xx、
// channel 98xx、ordering 990x),避免并行冒烟互相顶号。
const heartbeatIdleSmokeAccount = "robot_9951"

const (
	// gate 通告的心跳参数(gate-client-heartbeat.md §4,两版服务器同值)。对不上就是契约漂移,H1 失败。
	heartbeatIdleWantInterval = 10 * time.Second
	heartbeatIdleWantTimeout  = 30 * time.Second
	// 首登 / 重登等进场的预算,与 ordering-smoke 同值。
	heartbeatIdleSceneReadyTimeout = 15 * time.Second
	// 等第一条心跳回包、等自己的 actor 号的预算。正常是毫秒级。
	heartbeatIdleReplyTimeout = 5 * time.Second
	// 停发心跳之后等「判死阈值 + 本余量」再重登。gate 最晚在最后一帧之后 31s 判死(扫描节拍 1s),余量留给扫描迟到与
	// 断线通知的传播。最后一帧不晚于停发时刻,所以重登时断线保留(25s)还剩至少 10s,login 断线租约(30s)也还在。
	heartbeatIdleSilenceMargin = 4 * time.Second
	// 裸连接判死时刻的容许区间(从发出那条心跳算):下限留 0.5s 给两端计时误差;上限留 5s 给 gate 的扫描节拍、
	// Windows poll 粒度与本机负载。
	heartbeatIdleWireEarlySlack = 500 * time.Millisecond
	heartbeatIdleWireLateSlack  = 5 * time.Second
	// 裸连接建连、验签、心跳往返的预算。gate 处理验签与心跳都是同步的,正常是毫秒级。
	heartbeatIdleWireHandshakeTimeout = 10 * time.Second
	// 重登进场之后再等多久确认旧连接没收到踢线推送。顶号的 34 经 Kafka 到旧连接所在的 gate,通常在新会话进场之前就到了。
	heartbeatIdleKickGrace = 3 * time.Second
)

var errHeartbeatIdleNoToken = errors.New("gate issued no token (empty GateTokenSecret, dev pass-through): " +
	"heartbeat parameters only come back in the token verify reply, so the robot never starts heartbeating")

type heartbeatIdleBot struct {
	gc     *pkg.GameClient
	player *gameobject.Player
	stats  *metrics.Stats

	// silent 置位之后(H2 起),这条连接上收到的消息只登记、不交给通用分发:同一个 player_id 的新会话会注册新的 Player,
	// 旧连接上迟到的推送不能写进它。主流程写,RecvLoop goroutine 读。
	silent       atomic.Bool
	kicked       atomic.Bool
	afterSilence atomic.Uint64
}

func (b *heartbeatIdleBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	if b.silent.Load() {
		b.afterSilence.Add(1)
		if msg.GetMessageId() == game.SceneClientPlayerCommonKickPlayerMessageId {
			b.kicked.Store(true)
		}
		return
	}
	handler.MessageBodyHandler(client, msg)
}

// RunHeartbeatIdleSmoke 是 main.go `mode: heartbeat-idle-smoke` 的入口。任一步失败以退出码 1 结束进程。
func RunHeartbeatIdleSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	var silentBot, resumedBot *heartbeatIdleBot
	cleanup := func() {
		if resumedBot != nil {
			_ = leaveGame(resumedBot.gc, stats)
			gameobject.PlayerList.Delete(resumedBot.gc.PlayerId)
			resumedBot.gc.Close()
		}
		if silentBot != nil {
			// 到这里 gate 早已关掉它原来的那条 TCP(vendored muduo 会静默重拨出一条没验签的新连接),关掉不再影响服务端状态。
			gameobject.PlayerList.Delete(silentBot.gc.PlayerId)
			silentBot.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("HEARTBEAT_IDLE_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	pass := func(step, format string, args ...any) {
		zap.L().Info(fmt.Sprintf("[heartbeat-idle-smoke] PASS step=%s %s", step, fmt.Sprintf(format, args...)))
	}

	// ---- H1:登录进场,心跳已通告、已有回包 ----
	var err error
	silentBot, err = heartbeatIdleLogin(cfg, stats)
	if err != nil {
		fail("H1-login", "account=%s zone=%d err=%v", heartbeatIdleSmokeAccount, cfg.ZoneID, err)
	}
	gc := silentBot.gc
	if got := gc.HeartbeatInterval(); got != heartbeatIdleWantInterval {
		fail("H1-advert", "token verify reply advertised heartbeat_interval_ms=%d, want %d "+
			"(0 = this gate predates the client heartbeat, or this server version has not implemented it)",
			got.Milliseconds(), heartbeatIdleWantInterval.Milliseconds())
	}
	if got := gc.HeartbeatTimeout(); got != heartbeatIdleWantTimeout {
		fail("H1-advert", "token verify reply advertised heartbeat_timeout_ms=%d, want %d",
			got.Milliseconds(), heartbeatIdleWantTimeout.Milliseconds())
	}
	if !waitUntil(heartbeatIdleReplyTimeout, func() bool { return gc.HeartbeatReplies() >= 1 }) {
		fail("H1-reply", "no ClientHeartbeatResponse within %s: verification started idle tracking, but heartbeat responses are missing",
			heartbeatIdleReplyTimeout)
	}
	playerID := gc.PlayerId
	sceneID, entity := silentBot.player.GetSceneID(), silentBot.player.GetEntityID()
	pass("H1-login", "player_id=%d scene_id=%d entity=%d heartbeat_replies=%d", playerID, sceneID, entity, gc.HeartbeatReplies())

	// ---- H2:停发心跳,连接保持打开 ----
	gc.PauseHeartbeat()
	silentBot.silent.Store(true)
	silentSince := time.Now()
	pass("H2-silent", "heartbeats paused; the connection stays open and sends nothing")

	// ---- H3:裸连接上量 gate 判死的时刻 ----
	wireClose, err := heartbeatIdleWireCheck(cfg)
	if err != nil {
		fail("H3-wire", "%v", err)
	}
	pass("H3-wire", "gate closed a silent verified connection %d ms after its last heartbeat", wireClose.Milliseconds())

	// ---- H4:等主机器人静默满判死阈值 + 余量 ----
	if wait := time.Until(silentSince.Add(heartbeatIdleWantTimeout + heartbeatIdleSilenceMargin)); wait > 0 {
		time.Sleep(wait)
	}
	pass("H4-wait", "silent for %s", time.Since(silentSince).Truncate(time.Millisecond))

	// ---- H5:同账号重登,接回原场景里的同一个实体 ----
	resumedBot, err = heartbeatIdleLogin(cfg, stats)
	if err != nil {
		fail("H5-relogin", "account=%s err=%v", heartbeatIdleSmokeAccount, err)
	}
	if resumedBot.gc.PlayerId != playerID {
		fail("H5-relogin", "re-login entered player %d, want the same player %d", resumedBot.gc.PlayerId, playerID)
	}
	if got := resumedBot.player.GetSceneID(); got != sceneID {
		fail("H5-resume", "re-login landed in scene %d, want the original scene %d", got, sceneID)
	}
	if got := resumedBot.player.GetEntityID(); got != entity {
		fail("H5-resume", "actor id changed %d -> %d: the scene loaded a fresh entity instead of resuming the held one "+
			"(the disconnect hold expired before the re-login, or was never entered)", entity, got)
	}
	pass("H5-resume", "same scene_id=%d and actor id=%d after re-login", sceneID, entity)

	// ---- H6:旧连接没收到踢线推送 ----
	time.Sleep(heartbeatIdleKickGrace)
	if silentBot.kicked.Load() {
		fail("H6-no-kick", "the silent connection received KickPlayer(34) when the account logged in again: the gate had not closed it, "+
			"so login took the replace-login path instead of a short reconnect")
	}
	pass("H6-no-kick", "the silent connection was already closed by the gate (messages received after going silent=%d)",
		silentBot.afterSilence.Load())

	zap.L().Info(fmt.Sprintf("HEARTBEAT_IDLE_SMOKE_OK player_id=%d scene_id=%d entity=%d wire_close_ms=%d",
		playerID, sceneID, entity, wireClose.Milliseconds()))
	cleanup()
	_ = zap.L().Sync()
}

// heartbeatIdleLogin 用场景账号登录进场,等到进场推送与自己的 actor 号都到了才返回。
func heartbeatIdleLogin(cfg *config.Config, stats *metrics.Stats) (*heartbeatIdleBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate address: %w", err)
	}
	if len(tokenPayload) == 0 {
		return nil, errHeartbeatIdleNoToken
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad gate port %q: %w", portStr, err)
	}
	gc, err := connectAndVerify(host, port, heartbeatIdleSmokeAccount, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("connect gate %s:%s: %w", host, portStr, err)
	}
	if err := loginAndEnterScenario(gc, cfg.Password, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("login and enter: %w", err)
	}

	bot := &heartbeatIdleBot{gc: gc, player: gameobject.NewPlayer(gc.PlayerId), stats: stats}
	// 同一个 player_id 的新会话覆盖旧会话的 Player;旧会话此时已静默,不再写 Player(onMessage)。
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	ctx, cancel := context.WithTimeout(context.Background(), heartbeatIdleSceneReadyTimeout)
	defer cancel()
	if err := bot.player.WaitSceneReady(ctx); err != nil {
		gc.Close()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	if !waitUntil(heartbeatIdleReplyTimeout, func() bool { return bot.player.GetEntityID() != 0 }) {
		gc.Close()
		return nil, errors.New("entered the scene but never received its own ActorCreate (no actor id)")
	}
	return bot, nil
}

// heartbeatIdleWireCheck 是 H3:裸连接只验签、发一条心跳,收到回包后静默,量 gate 多久关掉它。
// 返回从发出心跳到读到断开的时长。帧的读写复用 network_probe.go(gate 与 battle 是同一种 codec,玩家协议的消息没有 package)。
func heartbeatIdleWireCheck(cfg *config.Config) (time.Duration, error) {
	host, portStr, payload, signature, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return 0, fmt.Errorf("resolve gate address: %w", err)
	}
	if len(payload) == 0 {
		return 0, errHeartbeatIdleNoToken
	}
	addr := net.JoinHostPort(host, portStr)
	conn, err := (&net.Dialer{Timeout: heartbeatIdleWireHandshakeTimeout}).Dial("tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	codec := &muduo.TcpCodec{}

	if err := conn.SetDeadline(time.Now().Add(heartbeatIdleWireHandshakeTimeout)); err != nil {
		return 0, fmt.Errorf("set handshake deadline: %w", err)
	}
	if err := writeNetworkProbeFrame(conn, &base.ClientTokenVerifyRequest{Payload: payload, Signature: signature}); err != nil {
		return 0, fmt.Errorf("send ClientTokenVerifyRequest: %w", err)
	}
	message, err := readNetworkProbeFrame(conn, codec)
	if err != nil {
		return 0, fmt.Errorf("read token verify reply: %w", err)
	}
	verified, ok := message.(*base.ClientTokenVerifyResponse)
	if !ok || !verified.GetSuccess() {
		return 0, fmt.Errorf("token verify failed on the raw connection: got %T %v", message, message)
	}
	if got := time.Duration(verified.GetHeartbeatIntervalMs()) * time.Millisecond; got != heartbeatIdleWantInterval {
		return 0, fmt.Errorf("raw connection: advertised heartbeat_interval_ms=%d, want %d", got.Milliseconds(), heartbeatIdleWantInterval.Milliseconds())
	}
	timeout := time.Duration(verified.GetHeartbeatTimeoutMs()) * time.Millisecond
	if timeout != heartbeatIdleWantTimeout {
		return 0, fmt.Errorf("raw connection: advertised heartbeat_timeout_ms=%d, want %d", timeout.Milliseconds(), heartbeatIdleWantTimeout.Milliseconds())
	}

	if err := writeNetworkProbeFrame(conn, &base.ClientHeartbeatRequest{Seq: 1}); err != nil {
		return 0, fmt.Errorf("send ClientHeartbeatRequest: %w", err)
	}
	sentAt := time.Now()
	message, err = readNetworkProbeFrame(conn, codec)
	if err != nil {
		return 0, fmt.Errorf("read heartbeat reply: %w", err)
	}
	if reply, ok := message.(*base.ClientHeartbeatResponse); !ok || reply.GetSeq() != 1 {
		return 0, fmt.Errorf("want ClientHeartbeatResponse{seq:1}, got %T %v", message, message)
	}

	// 从现在起一帧都不发,等 gate 关连接。gate 不往没绑定角色的会话推任何东西,所以下一次读要么是断开,要么超时。
	if err := conn.SetDeadline(sentAt.Add(timeout + heartbeatIdleWireLateSlack)); err != nil {
		return 0, fmt.Errorf("set idle deadline: %w", err)
	}
	message, err = readNetworkProbeFrame(conn, codec)
	elapsed := time.Since(sentAt)
	if err == nil {
		return elapsed, fmt.Errorf("gate sent %T while the connection was silent; expected nothing until it closes the connection", message)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return elapsed, fmt.Errorf("gate did not close the silent connection within %s of its heartbeat (heartbeat_timeout_ms=%d)",
			timeout+heartbeatIdleWireLateSlack, timeout.Milliseconds())
	}
	if elapsed < timeout-heartbeatIdleWireEarlySlack {
		return elapsed, fmt.Errorf("gate closed the connection %s after its heartbeat, earlier than heartbeat_timeout_ms=%d (%v)",
			elapsed.Truncate(time.Millisecond), timeout.Milliseconds(), err)
	}
	return elapsed, nil
}
