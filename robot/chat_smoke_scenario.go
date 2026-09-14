package main

// chat-smoke 场景:两个机器人对全局聊天服务 go/chat v1 做端到端冒烟。
//
// 兑现 zone_contract_v1 §9「冒烟」段与 §7 D-1 验收第三条(robot 端到端冒烟,代替未授权的客户端 UI):
//   1. A 登 zone_a、B 登 zone_b(cross_zone=true 时两 zone 必须不同,且断言两人的 gate 地址不同);
//   2. A 发 WORLD "world <nonce>"(带 request_id)→ chat 受理(tip=0);
//   3. B 拉 WORLD 最近 20 条 → 看到该 nonce 恰好一次,且 sender_player_id == A(服务端用会话身份覆盖 sender);
//   4. A 发 PRIVATE 给 B → B 拉 PRIVATE(peer=A)→ 看到,sender == A;
//   5. A 发 600 字节 ASCII 的 WORLD → chat 回 kMessageSizeExceeded(chat 侧二道闸:
//      MaxContentBytes 默认 512;序列化后仍 <1024B,gate 的 1KB 闸不会先拒);
//   6. A 用第 2 步的同一个 request_id 重发同一条 WORLD → 受理(tip=0)但 B 拉到的历史里该 nonce 仍只一条(幂等键 SET NX)。
//
// 为什么跨 zone 有意义:chat 是全局服务(§0 / §9),两个 zone 的 gate 经路由服到达的是同一份 chat,
// 世界频道与私聊历史都在 ChatRedis 里、不分 zone —— 只有 A、B 真的落在不同 gate 上,
// "B 看到 A 的消息"才证明路由服模式下的跨 zone 可达性,而不是同一个 gate 内部的巧合。
//
// 结果约定(供外层脚本消费):
//   全过 → 日志一行 `CHAT_SMOKE_OK player_a=… player_b=… gate_a=… gate_b=…`,退出码 0;
//   任一步失败 → `CHAT_SMOKE_FAIL step=… reason=…`,退出码 1。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/chat"
	"proto/common/base"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧的 DevPasswordAuth。
// 9005/9006 与 battle-smoke(9001/9002)、跨 zone 匹配(9003/9004)、属性(9101)错开:
// 冒烟可能被脚本先后甚至并行跑,同账号会互相顶成 ReplaceLogin。
const (
	chatSmokeAccountA = "robot_9005" // 发送方
	chatSmokeAccountB = "robot_9006" // 接收 / 拉历史方
)

// 单次聊天 RPC 的等待预算。chat 是一次 Redis Lua 往返,正常 <50ms;
// 10s 覆盖 gate→路由服→chat 首次拨号建连,并且大于 chat 的 zrpc Timeout(4000ms,§3)
// 与路由服 ForwardTimeoutMs,保证先看到路由服回的 kServiceUnavailable 而不是本地超时。
const chatSmokeRpcTimeout = 10 * time.Second

// 等进场的预算,与 prepareBehaviorClient 同值。
const chatSmokeSceneReadyTimeout = 15 * time.Second

// PullChatHistory 的 limit:契约 §9 冒烟段写死 20(0 也会取默认 20,这里显式给出)。
const chatSmokeHistoryLimit uint32 = 20

// 超长内容的字节数:必须 > chat MaxContentBytes(默认 512)才触发 chat 侧拒绝,
// 又必须让整个 ClientRequest 序列化后 < 1024B,否则 gate 的 CheckMessageSize 先拒,测到的就不是 chat。
const chatSmokeOversizeBytes = 600

// 同一机器人相邻两个请求的最小间隔。
// gate MessageLimiter 对不在 MessageLimiter 表里的消息号(SendChat=61 / PullChatHistory=28 当前都不在)
// 吃默认档 3 次 / 1 秒窗口,且按**秒粒度**时间戳、`now - oldest > window` 才淘汰 —— 实际窗口是 1~2 秒。
// A 在第 2/4/5/6 步共发 4 次 SendChat,不留间隔时第 4 次可能被 gate 以 kRateLimitExceeded 信封拒绝,
// 第 6 步"只出现一次"就会因为重发根本没到 chat 而假绿。1.1s 间隔下任一请求与前两次的秒差都 ≥2,缓冲区至多 2 条。
const chatSmokeRequestSpacing = 1100 * time.Millisecond

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipChatMessageSizeExceeded = uint32(tiptable.CommonError_kMessageSizeExceeded)
	tipChatServiceUnavailable  = uint32(tiptable.CommonError_kServiceUnavailable)
)

// chatSmokeEnvelopeError 是 gate / 路由服级拒绝:MessageContent.error_message 非空、body 为空。
type chatSmokeEnvelopeError struct {
	messageId uint32
	tip       uint32
}

// chatSmokeBot 是一个已登录进场的机器人会话。
type chatSmokeBot struct {
	account string
	gate    string // assign-gate 分配到的 gate 地址(host:port);跨 zone 冒烟用它证明两人落在不同 zone
	gc      *pkg.GameClient
	player  *gameobject.Player
	stats   *metrics.Stats

	// RecvLoop goroutine 写、场景主流程读,mu 保护。
	mu        sync.Mutex
	envelopes []chatSmokeEnvelopeError // 本次请求期间收到的聊天消息号信封拒绝
	gateTips  []uint32                 // 本次请求期间收到的 SendTipToClient(gate 找不到路由服时走这条)

	lastRequestAt time.Time // 仅主流程读写,用于 chatSmokeRequestSpacing
}

// RunChatSmoke 是 main.go `mode: chat-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunChatSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	// cross_zone=false 是同 zone 弱验收(§9):两人都登 cfg.ZoneID(0 = server-list 自动选区),不断言 gate 不同。
	crossZone := cfg.ChatSmoke.CrossZone
	zoneA, zoneB := cfg.ZoneID, cfg.ZoneID
	if crossZone {
		zoneA, zoneB = cfg.ChatSmoke.ZoneA, cfg.ChatSmoke.ZoneB
	}

	// bots 收集已登录的会话,fail 时统一优雅收尾(同 attribute-smoke:LeaveGame + Disconnect,
	// 避免残留 ONLINE 会话把下一次冒烟顶成 ReplaceLogin)。
	var bots []*chatSmokeBot
	cleanup := func() {
		for _, b := range bots {
			if b == nil || b.gc == nil {
				continue
			}
			_ = leaveGame(b.gc, stats)
			sendDisconnectBestEffort(b.gc)
			handler.ResetChatInbox(b.gc.PlayerId)
			gameobject.PlayerList.Delete(b.gc.PlayerId)
			b.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("CHAT_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 步骤 1:A 登 zone_a、B 登 zone_b(并发) ----
	// 复制一份 cfg 只改 ZoneID:resolveGateViaHTTPLocal 用 cfg.ZoneID 向 /api/assign-gate 要该 zone 的 gate。
	zap.L().Info("[chat-smoke] step 1: login both robots",
		zap.Bool("cross_zone", crossZone),
		zap.String("a", chatSmokeAccountA), zap.Uint32("zone_a", zoneA),
		zap.String("b", chatSmokeAccountB), zap.Uint32("zone_b", zoneB))

	type loginSpec struct {
		account string
		zone    uint32
	}
	specs := []loginSpec{{chatSmokeAccountA, zoneA}, {chatSmokeAccountB, zoneB}}
	botsArr := make([]*chatSmokeBot, len(specs))
	errsArr := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, s := range specs {
		wg.Add(1)
		go func(idx int, s loginSpec) {
			defer wg.Done()
			zoneCfg := *cfg
			zoneCfg.ZoneID = s.zone
			botsArr[idx], errsArr[idx] = chatSmokeLogin(&zoneCfg, s.account, stats)
		}(i, s)
	}
	wg.Wait()
	// 先把登上的会话全部收进 bots 再判错:否则 A 失败时 B 已登录的会话不会被 cleanup 收尾。
	for _, b := range botsArr {
		if b != nil {
			bots = append(bots, b)
		}
	}
	for i, err := range errsArr {
		if err != nil {
			fail("login", "account=%s zone=%d err=%v", specs[i].account, specs[i].zone, err)
		}
	}
	botA, botB := botsArr[0], botsArr[1]

	zap.L().Info("[chat-smoke] step 1: both robots in scene",
		zap.Uint64("a_player", botA.gc.PlayerId), zap.Uint32("zone_a", zoneA), zap.String("a_gate", botA.gate),
		zap.Uint64("b_player", botB.gc.PlayerId), zap.Uint32("zone_b", zoneB), zap.String("b_gate", botB.gate))
	// 落区证据:本地部署里各 zone 的 gate 端口不同(gate_1 / gate_2)。地址相同意味着两人其实进了同一个 zone,
	// 后面即便互相看得见也不能叫"跨 zone",直接判失败。
	if crossZone && botA.gate == botB.gate {
		fail("zone-placement", "both robots assigned the same gate %s (zone_a=%d zone_b=%d); not a cross-zone run",
			botA.gate, zoneA, zoneB)
	}

	// ---- 步骤 2:A 发 WORLD,chat 受理 ----
	worldContent := "world " + chatSmokeNonce()
	worldRequestId := chatSmokeRequestId()
	// 第 6 步原样重发这一条(同内容、同 request_id),所以留住这个请求消息对象。
	// sender_player_id 故意留 0:服务端必须用会话 player_id 覆盖(§3),第 3 步断言 sender == A 就是在验这一条。
	worldMessage := &chat.ChatMessage{
		Channel: chat.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
		Content: worldContent,
	}
	tip, err := botA.sendChat(worldMessage, worldRequestId)
	if err != nil {
		fail("world-send", "%v", err)
	}
	if tip != 0 {
		fail("world-send-tip", "A 发 WORLD 应受理(tip=0),实得 tip=%d", tip)
	}
	zap.L().Info("[chat-smoke] step 2: A world message accepted",
		zap.String("content", worldContent), zap.String("request_id", worldRequestId))

	// ---- 步骤 3:B 拉 WORLD,看到 A 的消息 ----
	messages, err := botB.pullHistory(chat.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, 0, chatSmokeHistoryLimit)
	if err != nil {
		fail("world-pull", "%v", err)
	}
	hits := chatSmokeFindContent(messages, worldContent)
	if len(hits) == 0 {
		fail("world-visible", "B 拉 WORLD 最近 %d 条里没有 %q(共返回 %d 条)", chatSmokeHistoryLimit, worldContent, len(messages))
	}
	if len(hits) != 1 {
		// 首发就出现多次说明写入路径本身重复落库;不在这里拦,第 6 步的幂等断言就分不清是谁重复的
		fail("world-duplicate", "A 只发了一次,B 的 WORLD 历史里 %q 却出现 %d 次", worldContent, len(hits))
	}
	if got := hits[0].GetSenderPlayerId(); got != botA.gc.PlayerId {
		fail("world-sender", "WORLD 消息 sender_player_id=%d,期望 A=%d(服务端应以会话身份覆盖 sender)", got, botA.gc.PlayerId)
	}
	zap.L().Info("[chat-smoke] step 3: B sees A's world message",
		zap.Int("history_size", len(messages)), zap.Int64("send_time_ms", hits[0].GetSendTimeMs()))

	// ---- 步骤 4:A 私聊 B,B 拉 PRIVATE(peer=A)看到 ----
	privateContent := "private " + chatSmokeNonce()
	tip, err = botA.sendChat(&chat.ChatMessage{
		Channel:        chat.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE,
		TargetPlayerId: botB.gc.PlayerId,
		Content:        privateContent,
	}, chatSmokeRequestId())
	if err != nil {
		fail("private-send", "%v", err)
	}
	if tip != 0 {
		fail("private-send-tip", "A 私聊 B 应受理(tip=0),实得 tip=%d", tip)
	}
	messages, err = botB.pullHistory(chat.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, botA.gc.PlayerId, chatSmokeHistoryLimit)
	if err != nil {
		fail("private-pull", "%v", err)
	}
	hits = chatSmokeFindContent(messages, privateContent)
	if len(hits) == 0 {
		fail("private-visible", "B 拉 PRIVATE(peer=A=%d)最近 %d 条里没有 %q(共返回 %d 条)",
			botA.gc.PlayerId, chatSmokeHistoryLimit, privateContent, len(messages))
	}
	if got := hits[0].GetSenderPlayerId(); got != botA.gc.PlayerId {
		fail("private-sender", "PRIVATE 消息 sender_player_id=%d,期望 A=%d", got, botA.gc.PlayerId)
	}
	zap.L().Info("[chat-smoke] step 4: B sees A's private message", zap.Int("history_size", len(messages)))

	// ---- 步骤 5:超长内容被 chat 侧二道闸拒绝 ----
	tip, err = botA.sendChat(&chat.ChatMessage{
		Channel: chat.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
		Content: strings.Repeat("x", chatSmokeOversizeBytes),
	}, chatSmokeRequestId())
	if err != nil {
		// sendChat 把 gate / 路由服信封拒绝一律报成 error:走到这里说明不是 chat 给的码,不能算通过
		fail("oversize-send", "%v", err)
	}
	if tip != tipChatMessageSizeExceeded {
		fail("oversize-tip", "%d 字节 WORLD 应被 chat 以 tip=%d(kMessageSizeExceeded)拒绝,实得 tip=%d",
			chatSmokeOversizeBytes, tipChatMessageSizeExceeded, tip)
	}
	zap.L().Info("[chat-smoke] step 5: oversize content rejected by chat", zap.Uint32("tip", tip))

	// ---- 步骤 6:同 request_id 重发,历史里仍只一条 ----
	tip, err = botA.sendChat(worldMessage, worldRequestId)
	if err != nil {
		fail("dedup-resend", "%v", err)
	}
	if tip != 0 {
		// 必须确认重发真的到达 chat 且被受理:被限流 / 拒绝的重发也会让"只出现一次"成立,那是假绿
		fail("dedup-resend-tip", "同 request_id 重发应按幂等回成功(tip=0),实得 tip=%d", tip)
	}
	messages, err = botB.pullHistory(chat.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, 0, chatSmokeHistoryLimit)
	if err != nil {
		fail("dedup-pull", "%v", err)
	}
	if n := len(chatSmokeFindContent(messages, worldContent)); n != 1 {
		fail("dedup", "同 request_id=%s 重发后 WORLD 历史里 %q 出现 %d 次,应恰好 1 次", worldRequestId, worldContent, n)
	}
	zap.L().Info("[chat-smoke] step 6: resend with same request_id deduplicated", zap.String("request_id", worldRequestId))

	// ---- 全部断言通过 ----
	zap.L().Info(fmt.Sprintf("CHAT_SMOKE_OK player_a=%d player_b=%d gate_a=%s gate_b=%s",
		botA.gc.PlayerId, botB.gc.PlayerId, botA.gate, botB.gate),
		zap.Bool("cross_zone", crossZone), zap.Uint32("zone_a", zoneA), zap.Uint32("zone_b", zoneB))
	cleanup()
	_ = zap.L().Sync()
	// 正常返回,main 里该分支 return 后进程退出码为 0。
}

// chatSmokeLogin 为一个账号走完整的 AssignGate → 连接 → 登录 → 进场流程,并挂上本场景自己的 RecvLoop 回调。
//
// 为什么不直接用 prepareBehaviorClient:它的 RecvLoop 回调只替属性消息拦截信封错误,其余消息一律交给
// MessageBodyHandler。聊天请求被 gate(限流 / 超长)或路由服(无 chat 实例 / 上游 gRPC 错误,见
// go/client_rpc_router/internal/logic/forwardlogic.go rejected)拒绝时,回的是 body 为空、
// error_message 在信封上的 MessageContent —— 分发下去会被解成全零 SendChatResponse,收件箱里记成 tip=0,
// 冒烟就把"根本没到 chat"误判成"chat 受理成功"(第 6 步幂等断言会因此假绿)。
// 其余步骤(connectAndVerify / loginAndEnterScenario / NewPlayer / WaitSceneReady)与 prepareBehaviorClient 一致;
// 省掉 ListSkills + 1.5s 等待:聊天不依赖技能列表。
func chatSmokeLogin(cfg *config.Config, account string, stats *metrics.Stats) (*chatSmokeBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad gate port %q: %w", portStr, err)
	}
	zap.L().Info("[chat-smoke] logging in",
		zap.String("account", account), zap.Uint32("zone", cfg.ZoneID), zap.String("gate", host+":"+portStr))

	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("connect gate: %w", err)
	}
	if err := loginAndEnterScenario(gc, cfg.Password, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("login and enter: %w", err)
	}

	bot := &chatSmokeBot{
		account: account,
		gate:    host + ":" + portStr,
		gc:      gc,
		player:  gameobject.NewPlayer(gc.PlayerId),
		stats:   stats,
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), chatSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		// 已 EnterGame 成功,补一个 Disconnect 再关连接,避免残留 ONLINE 会话顶掉下一次冒烟
		sendDisconnectBestEffort(gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	return bot, nil
}

// onMessage 是本场景的 RecvLoop 回调:先拦聊天消息号的信封错误与 gate 的 SendTipToClient,再交给通用分发。
func (b *chatSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	if tip := msg.GetErrorMessage().GetId(); tip != 0 {
		zap.L().Error("[chat-smoke] gate envelope error",
			zap.String("account", b.account), zap.Uint32("message_id", msg.GetMessageId()), zap.Uint32("tip", tip))
		if chatSmokeIsChatMessage(msg.GetMessageId()) {
			b.mu.Lock()
			b.envelopes = append(b.envelopes, chatSmokeEnvelopeError{messageId: msg.GetMessageId(), tip: tip})
			b.mu.Unlock()
			return // 不再分发空 body(否则 handler 会把它记成 tip=0 的成功响应)
		}
	}
	if msg.GetMessageId() == game.SceneClientPlayerCommonSendTipToClientMessageId {
		// gate 在路由模式下找不到路由服实例时不回信封,而是单独推一条 SendTipToClient(kServiceUnavailable),
		// 与请求没有消息号关联;记下来,等待方据此快速失败并给出可读原因,而不是干等 10s 超时。
		var tipInfo base.TipInfoMessage
		if err := proto.Unmarshal(msg.GetSerializedMessage(), &tipInfo); err == nil && tipInfo.GetId() != 0 {
			zap.L().Warn("[chat-smoke] gate tip", zap.String("account", b.account), zap.Uint32("tip", tipInfo.GetId()))
			b.mu.Lock()
			b.gateTips = append(b.gateTips, tipInfo.GetId())
			b.mu.Unlock()
		}
	}
	handler.MessageBodyHandler(client, msg)
}

// send 发一个请求:先按 chatSmokeRequestSpacing 让出间隔,再清空本玩家的聊天收件箱与 gate 级拒绝记录
// (上一步迟到的响应不能被这一步当成自己的结果消费),最后发出。
func (b *chatSmokeBot) send(messageId uint32, request proto.Message) error {
	if wait := chatSmokeRequestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
	handler.ResetChatInbox(b.gc.PlayerId)
	b.mu.Lock()
	b.envelopes = nil
	b.gateTips = nil
	b.mu.Unlock()

	if err := b.gc.SendRequest(messageId, request); err != nil {
		return fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	b.lastRequestAt = time.Now()
	b.stats.MsgSent()
	return nil
}

// sendChat 发 SendChat 并等 chat 的响应 tip(0 = 受理)。
// gate / 路由服级拒绝(信封错误、SendTipToClient kServiceUnavailable)与超时一律返回 error:
// 它们说明请求没有到达 chat 业务逻辑,调用方不能把它们当成 chat 给的码去断言。
func (b *chatSmokeBot) sendChat(message *chat.ChatMessage, requestId string) (uint32, error) {
	messageId := uint32(game.ClientPlayerChatSendChatMessageId)
	if err := b.send(messageId, &chat.SendChatRequest{Message: message, RequestId: requestId}); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(chatSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		if tips := handler.ChatSendTipsOf(b.gc.PlayerId); len(tips) > 0 {
			return tips[0], nil
		}
		if reason, rejected := b.gateRejection(messageId); rejected {
			return 0, errors.New(reason)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, fmt.Errorf("等 SendChat 响应超时(%s) account=%s gate_tips=%v", chatSmokeRpcTimeout, b.account, b.gateTipsSnapshot())
}

// pullHistory 发 PullChatHistory 并等响应;chat 在响应体里给的拒绝码返回 error。
func (b *chatSmokeBot) pullHistory(channel chat.ChatChannelType, peerPlayerId uint64, limit uint32) ([]*chat.ChatMessage, error) {
	messageId := uint32(game.ClientPlayerChatPullChatHistoryMessageId)
	if err := b.send(messageId, &chat.PullChatHistoryRequest{
		Channel:      channel,
		PeerPlayerId: peerPlayerId,
		Limit:        limit,
	}); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(chatSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		if responses := handler.ChatHistoriesOf(b.gc.PlayerId); len(responses) > 0 {
			response := responses[0]
			if tip := response.GetErrorMessage().GetId(); tip != 0 {
				return nil, fmt.Errorf("chat 拒绝 PullChatHistory channel=%s peer=%d: tip=%d", channel, peerPlayerId, tip)
			}
			return response.GetMessages(), nil
		}
		if reason, rejected := b.gateRejection(messageId); rejected {
			return nil, errors.New(reason)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("等 PullChatHistory 响应超时(%s) account=%s channel=%s gate_tips=%v",
		chatSmokeRpcTimeout, b.account, channel, b.gateTipsSnapshot())
}

// gateRejection 返回本次请求期间收到的、足以判定"请求没到 chat"的 gate / 路由服级拒绝。
// 只有 kServiceUnavailable 的 SendTipToClient 算数:其它 tip 可能是场景顺手推的无关提示,
// 只进超时原因,不据此提前失败。
func (b *chatSmokeBot) gateRejection(messageId uint32) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.envelopes {
		if e.messageId == messageId {
			return fmt.Sprintf("gate/路由服信封拒绝 message_id=%d tip=%d(请求未到达 chat 业务逻辑:"+
				"tip=%d 多为路由服无 chat 实例或上游 gRPC 错误,其它常见为 gate 限流 / 超长)",
				e.messageId, e.tip, tipChatServiceUnavailable), true
		}
	}
	for _, tip := range b.gateTips {
		if tip == tipChatServiceUnavailable {
			return fmt.Sprintf("gate SendTipToClient tip=%d(gate 选不到目标:没有可用路由服实例,"+
				"或 gate 未以 GATE_CLIENT_RPC_ROUTER=1 启动而直连模式下 chat 不可达)", tip), true
		}
	}
	return "", false
}

func (b *chatSmokeBot) gateTipsSnapshot() []uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uint32(nil), b.gateTips...)
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

func chatSmokeIsChatMessage(messageId uint32) bool {
	return messageId == game.ClientPlayerChatSendChatMessageId ||
		messageId == game.ClientPlayerChatPullChatHistoryMessageId
}

// chatSmokeFindContent 返回历史里内容与 content 完全相同的消息(按返回顺序)。
// 内容里带随机 nonce,世界频道是全服共享的,不能按"最新一条"去认,只能按内容认。
func chatSmokeFindContent(messages []*chat.ChatMessage, content string) []*chat.ChatMessage {
	var hits []*chat.ChatMessage
	for _, m := range messages {
		if m.GetContent() == content {
			hits = append(hits, m)
		}
	}
	return hits
}

// chatSmokeNonce 返回 16 个十六进制字符的随机串:世界频道历史跨运行保留 7 天,
// 同内容的旧消息会让"看到 / 只出现一次"两条断言失真,所以每次运行必须用新 nonce。
func chatSmokeNonce() string {
	return hex.EncodeToString(chatSmokeRandomBytes(8))
}

// chatSmokeRequestId 生成 UUIDv4 形状的幂等键。
// robot 的 go.mod / vendor 里没有 google/uuid,为一个冒烟 id 引依赖不值得,按 RFC 4122 置版本位即可。
func chatSmokeRequestId() string {
	b := chatSmokeRandomBytes(16)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func chatSmokeRandomBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 在受支持平台上不会失败;真失败时退回纳秒时间戳混入下标,
		// 只需保证本进程内几次调用互不相同(每次调用时间戳都在变)。
		ts := uint64(time.Now().UnixNano())
		for i := range buf {
			buf[i] = byte(ts>>(8*(uint(i)%8))) ^ byte(i)
		}
	}
	return buf
}
