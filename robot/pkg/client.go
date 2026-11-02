package pkg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/battle"
	"proto/common/base"
)

// 请求自带回调的结局(docs/design/client-rpc-callback.md §2、§6)。Call 的 done 恰好收到其中之一,
// 或者成功的回包。Timeout 与 Disconnected 是「结局未知」:请求可能已在服务端执行,有副作用的请求
// 重发时要复用业务幂等键,不能把它们当成「没执行」。
var (
	// ErrRPCNotSent:请求没有发出(没有连接 / 已 Close / 序列化失败 / 超时预算非正),done 在 Call 返回前同步调用。
	ErrRPCNotSent = errors.New("rpc: request not sent")
	// ErrRPCTimeout:在超时预算内没有收到带本次请求号的回包。之后到达的回包按迟到丢弃。
	ErrRPCTimeout = errors.New("rpc: timed out waiting for reply")
	// ErrRPCDisconnected:回包到达前连接被 Close 或被 SwapConn 换掉。
	ErrRPCDisconnected = errors.New("rpc: connection closed before reply")
)

// ServerTipError 是信封错误:gate / 路由服 / battle 在 MessageContent.error_message 里拒绝了这次请求,
// 响应体为空。服务自己写在响应 message 里的业务拒绝不走这里,由调用方解码响应后判断。
type ServerTipError struct {
	Tip uint32
}

func (e *ServerTipError) Error() string {
	return fmt.Sprintf("rpc: server tip=%d", e.Tip)
}

// outstandingCall 是一次在途调用。conn 记下发出请求的那条连接:SwapConn 只让旧连接上的调用失败,
// 换连接之后发出的调用不受影响。
type outstandingCall struct {
	conn *muduo.Client
	stop func() bool
	done func(*base.MessageContent, error)
}

// GameClient wraps a muduo TCP connection to a gate node.
// Each GameClient runs in its own goroutine (one-client-one-goroutine).
//
// Token fields are guarded by tokenMu because the in-session RefreshToken
// handler runs on the RecvLoop goroutine while the robot's main goroutine
// may read them at any time (e.g. when deciding whether a refresh is due,
// or when capturing the final value into runRobotOnce's named return).
type GameClient struct {
	// connMu 保护 client / gateIP / gatePort。它们**会在会话中途被换掉**:
	// 跟随 gate 重定向(msg 124)时 FollowRedirect 把整条 TCP 换成新 zone 的 gate,
	// 而 AI 循环、token 刷新器等 goroutine 随时可能并发 SendRequest。
	connMu   sync.RWMutex
	client   *muduo.Client
	gateIP   string
	gatePort int

	PlayerId uint64
	Account  string

	// 登录阶段使用同步的 SendRequest + RecvOne 等待指定响应。Scene 的
	// NotifyEnterScene 可能先于 EnterGameResponse 到达，不能在等待循环里
	// 丢弃；先暂存，等 Player 注册并切换到 RecvLoop 后按到达顺序补投递。
	deferredMu       sync.Mutex
	deferredMessages []*base.MessageContent

	tokenMu            sync.RWMutex
	AccessToken        string
	RefreshToken       string
	AccessTokenExpire  int64  // unix seconds
	RefreshTokenExpire int64  // unix seconds
	SaToken            string // SA-Token value from /auth/dev-login (kept for /auth/logout cleanup)

	// redirectMu 串行化 FollowRedirect;redirectHops 是本会话已跟随的重定向次数
	// (环路熔断,见 redirect.go MaxRedirectHops)。
	redirectMu   sync.Mutex
	redirectHops int

	// dispatchMu/dispatch 记住 RecvLoop 当前的分发函数,好让重定向后能把
	// 登录握手期间暂存的推送补投递出去(RecvLoop 只在**入口**replay 一次)。
	dispatchMu sync.Mutex
	dispatch   func(*GameClient, *base.MessageContent)

	// seq 发放 Call 的请求号:从 1 起、跳过 0(0 留给 SendRequest 的不关联请求与服务端推送)。
	seq atomic.Uint64

	// callMu 保护 calls / callsClosed。calls 是按请求号登记的在途调用;callsClosed 由 Close 置位,
	// 之后的 Call 一律 ErrRPCNotSent(Close 是终态)。回调一律在放锁之后调用,回调里重入 Call 安全。
	callMu      sync.Mutex
	calls       map[uint64]*outstandingCall
	callsClosed bool
	// lateReplies 计数带请求号却认领不到的回包(超时 / 断线 / ctx 取消之后才到),它们被丢弃。
	lateReplies atomic.Uint64

	// 测试替身注入点,nil 走生产实现:transmit 代替 muduo 发送,afterFunc 代替 time.AfterFunc。
	transmit  func(*muduo.Client, *base.ClientRequest)
	afterFunc func(time.Duration, func()) (stop func() bool)
}

// conn 取当前连接快照。所有用到内部 muduo 客户端的地方都必须走它:
// 重定向会原子换掉 gc.client,裸读字段就是数据竞争。
func (gc *GameClient) conn() *muduo.Client {
	gc.connMu.RLock()
	defer gc.connMu.RUnlock()
	return gc.client
}

// GateAddr 返回当前连着的 gate 地址(重定向后是新 zone 的 gate)。
func (gc *GameClient) GateAddr() (string, int) {
	gc.connMu.RLock()
	defer gc.connMu.RUnlock()
	return gc.gateIP, gc.gatePort
}

// SetTokens atomically replaces access/refresh token state (called after a
// successful RefreshToken RPC). Pass zero-values for fields that should not
// be touched by the caller.
func (gc *GameClient) SetTokens(access, refresh string, accessExpire, refreshExpire int64) {
	gc.tokenMu.Lock()
	defer gc.tokenMu.Unlock()
	if access != "" {
		gc.AccessToken = access
		gc.AccessTokenExpire = accessExpire
	}
	if refresh != "" {
		gc.RefreshToken = refresh
		gc.RefreshTokenExpire = refreshExpire
	}
}

// DropRefreshToken 丢弃一枚不能再用的 refresh token(被服务端拒绝,或刷新结果未知且已超出重试预算)。
// 只在它仍是 stale 那一枚时清空,不覆盖并发换上的新令牌。清空之后不再主动刷新,access 过期后的重连走完整登录;
// 再拿它去刷新会落在服务端宽限窗口之外、被当成重用而吊销整个家族。
func (gc *GameClient) DropRefreshToken(stale string) {
	gc.tokenMu.Lock()
	defer gc.tokenMu.Unlock()
	if stale != "" && gc.RefreshToken == stale {
		gc.RefreshToken = ""
		gc.RefreshTokenExpire = 0
	}
}

// SnapshotTokens returns a consistent snapshot of the current token state.
func (gc *GameClient) SnapshotTokens() (access, refresh string, accessExpire, refreshExpire int64) {
	gc.tokenMu.RLock()
	defer gc.tokenMu.RUnlock()
	return gc.AccessToken, gc.RefreshToken, gc.AccessTokenExpire, gc.RefreshTokenExpire
}

// NewGameClient connects to the gate at ip:port.
func NewGameClient(ip string, port int) (*GameClient, error) {
	codec := &muduo.TcpCodec{}
	c, err := muduo.NewClient(ip, port, codec)
	if err != nil {
		return nil, fmt.Errorf("connect to gate %s:%d: %w", ip, port, err)
	}
	return &GameClient{client: c, gateIP: ip, gatePort: port}, nil
}

// SwapConn 把当前连接换成 ip:port 的新连接,并关掉旧连接。跟随 gate 重定向时用。
//
// ⚠️ 只能由**当前 RecvLoop 所在的那个 goroutine**调用(消息处理器天然满足)。
// 原因在 vendored muduo:Connection.Close() 只关 outgoing,incoming 永不 close,
// 所以阻塞在旧连接 Recv() 上的 goroutine 关连接后**不会返回错误,而是永久阻塞**。
// 由 RecvLoop 自己换连接就不存在这个阻塞者:它换完接着从新连接读。
//
// 先换后关也是刻意的:并发的 SendRequest 最坏是把包发进一条即将关闭的连接
// (等价于丢包,上层本来就要容忍),而不是撞上 nil。
//
// 旧连接上的在途 Call 在关旧连接之前就以 ErrRPCDisconnected 完成:它们的回包只会从旧连接回来,
// 而旧连接从此没人读。换连接之后发出的 Call 登记在新连接上,不受影响(为什么没有漏网见 startCall 登记处的注释)。
func (gc *GameClient) SwapConn(ip string, port int) error {
	codec := &muduo.TcpCodec{}
	c, err := muduo.NewClient(ip, port, codec)
	if err != nil {
		return fmt.Errorf("connect to gate %s:%d: %w", ip, port, err)
	}
	gc.connMu.Lock()
	old := gc.client
	gc.client, gc.gateIP, gc.gatePort = c, ip, port
	gc.connMu.Unlock()
	if old != nil {
		gc.failCallsOn(old, ErrRPCDisconnected)
		old.Close()
	}
	return nil
}

// SendRequest 发一个不关联回包的请求(ClientRequest.id = 0)。回包同样带 id = 0,
// 由 RecvLoop 按 message_id 交给 onMessage —— 这是迁到 Call 之前的旧场景所依赖的形状。
// 要「只等本次请求自己的回包」用 Call / CallSync。
func (gc *GameClient) SendRequest(messageId uint32, body proto.Message) error {
	c := gc.conn()
	if c == nil {
		return fmt.Errorf("game client has no connection")
	}
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	gc.send(c, &base.ClientRequest{
		MessageId: messageId,
		Body:      bodyBytes,
	})
	return nil
}

// Call 发一个请求-响应调用:分配非 0 的请求号并登记 done,回包只按 MessageContent.id 认领
// (docs/design/client-rpc-callback.md §6)。done 恰好调用一次,结局是下列之一:
//   - (reply, nil):成功回包,reply.serialized_message 是响应体;
//   - (reply, *ServerTipError):信封错误,reply 是那条信封;
//   - (nil, ErrRPCNotSent):在 Call 返回前同步调用;
//   - (nil, ErrRPCTimeout):timeout 到期仍无回包,在定时器 goroutine 上调用;
//   - (nil, ErrRPCDisconnected):Close / SwapConn,在调用它们的 goroutine 上调用;
//   - (nil, context.Cause(ctx)):仅 CallSync 的 ctx 先结束时。
//
// 成功与信封错误在 RecvLoop(或同步握手里的 RecvOne)goroutine 上调用。done **不得阻塞**:
// 它跑在收包或定时器 goroutine 上,阻塞就卡住整条连接的派发。done 必须非 nil。
//
// vendored muduo 的 Send 吞错、断线后自动重拨且不关 incoming:TCP 被对端断开时在途调用不会立刻失败,
// 只能等超时,所以 timeout 必须为正。
func (gc *GameClient) Call(messageId uint32, body proto.Message, timeout time.Duration, done func(*base.MessageContent, error)) {
	gc.startCall(messageId, body, timeout, done)
}

// CallSync 是 Call 的阻塞版:在调用方 goroutine 上等本次请求的结局并返回,结局形状与 Call 的 done 相同。
// ctx 先结束时本次调用以 context.Cause(ctx) 完成并摘表,之后到达的回包按迟到丢弃;
// ctx 在发送前已结束则不发送,直接返回 context.Cause(ctx)。
//
// ⚠️ 禁止在 RecvLoop 所在的 goroutine 上调用(消息处理器、done 回调、pkg/redirect.go 的重定向重登录都在那里):
// 回包只能由 RecvLoop 派发,在那里阻塞等回包就是自锁,只能以超时收场。
func (gc *GameClient) CallSync(ctx context.Context, messageId uint32, body proto.Message, timeout time.Duration) (*base.MessageContent, error) {
	return gc.CallSyncThen(ctx, messageId, body, timeout, nil)
}

// CallSyncThen 与 CallSync 相同,只多一个 onReply:本次请求的回包(成功或信封错误)到达时,先在派发它的
// goroutine(RecvLoop,或同步握手里的 RecvOne)上以 (reply, err) 调用 onReply,再唤醒调用方。
// 超时、断线、ctx 先结束、本地未发出这些没有回包的结局不调用 onReply。
//
// 用于回包的副作用必须按收包顺序执行的调用方:例如把回包交给生成的 handler 写 Player 状态。在调用方
// goroutine 上补跑会与 RecvLoop 上同连接后到的推送处理器交错,较旧的回包可能盖掉较新的推送。onReply 返回后
// CallSyncThen 才返回,所以调用方随后读到的就是这次回包写下的状态。onReply 与 Call 的 done 一样**不得阻塞**;
// 为 nil 时等同 CallSync。CallSync 的 goroutine 限制同样适用。
func (gc *GameClient) CallSyncThen(ctx context.Context, messageId uint32, body proto.Message, timeout time.Duration,
	onReply func(*base.MessageContent, error)) (*base.MessageContent, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	type outcome struct {
		reply *base.MessageContent
		err   error
	}
	// 容量 1:done 恰好写一次且从不阻塞,哪怕本函数已经因 ctx 返回。
	result := make(chan outcome, 1)
	id := gc.startCall(messageId, body, timeout, func(reply *base.MessageContent, err error) {
		if reply != nil && onReply != nil {
			onReply(reply, err)
		}
		result <- outcome{reply: reply, err: err}
	})
	select {
	case r := <-result:
		return r.reply, r.err
	case <-ctx.Done():
		// 摘表成功则 done 写入取消原因;摘不到说明结局已先一步到达。两种情况 result 里都恰好有一条。
		gc.finishCall(id, nil, context.Cause(ctx))
		r := <-result
		return r.reply, r.err
	}
}

// LateReplies 返回本客户端丢弃的迟到回包数(带请求号却认领不到)。
func (gc *GameClient) LateReplies() uint64 {
	return gc.lateReplies.Load()
}

// startCall 是 Call 的实现,返回登记的请求号;本地未发出时返回 0(done 已同步调用)。
func (gc *GameClient) startCall(messageId uint32, body proto.Message, timeout time.Duration, done func(*base.MessageContent, error)) uint64 {
	if done == nil {
		panic("pkg.GameClient.Call: nil done callback")
	}
	if timeout <= 0 {
		done(nil, fmt.Errorf("%w: non-positive timeout %s", ErrRPCNotSent, timeout))
		return 0
	}
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		done(nil, fmt.Errorf("%w: marshal body: %w", ErrRPCNotSent, err))
		return 0
	}

	// 取连接快照与登记在同一把锁里:SwapConn 先换连接再在这把锁里清旧连接的调用,
	// 所以登记要么早于清理(随旧连接一起失败),要么晚于清理(此时取到的已是新连接)。
	gc.callMu.Lock()
	c := gc.conn()
	if gc.callsClosed || c == nil {
		gc.callMu.Unlock()
		done(nil, fmt.Errorf("%w: game client has no connection", ErrRPCNotSent))
		return 0
	}
	id := gc.nextRequestId()
	call := &outstandingCall{conn: c, done: done}
	if gc.calls == nil {
		gc.calls = make(map[uint64]*outstandingCall)
	}
	gc.calls[id] = call
	// 定时器在锁内创建:即便它立刻到期,finishCall 也要等这里放锁,届时 call.stop 已就位。
	call.stop = gc.startTimer(timeout, func() { gc.finishCall(id, nil, ErrRPCTimeout) })
	gc.callMu.Unlock()

	gc.send(c, &base.ClientRequest{
		Id:        id,
		MessageId: messageId,
		Body:      bodyBytes,
	})
	return id
}

func (gc *GameClient) nextRequestId() uint64 {
	for {
		if id := gc.seq.Add(1); id != 0 {
			return id
		}
	}
}

func (gc *GameClient) send(c *muduo.Client, req *base.ClientRequest) {
	if gc.transmit != nil {
		gc.transmit(c, req)
		return
	}
	c.Send(req)
}

func (gc *GameClient) startTimer(d time.Duration, f func()) func() bool {
	if gc.afterFunc != nil {
		return gc.afterFunc(d, f)
	}
	return time.AfterFunc(d, f).Stop
}

// finishCall 摘下请求号 id 的在途调用并以 (reply, err) 完成它。摘表成功者才调用 done,
// 所以回包、定时器、断线、ctx 取消并发到达时 done 仍恰好一次。返回是否摘到。
func (gc *GameClient) finishCall(id uint64, reply *base.MessageContent, err error) bool {
	if id == 0 {
		return false
	}
	gc.callMu.Lock()
	call, ok := gc.calls[id]
	if ok {
		delete(gc.calls, id)
	}
	gc.callMu.Unlock()
	if !ok {
		return false
	}
	call.stop()
	call.done(reply, err)
	return true
}

// failCallsOn 让连接 conn 上发出的全部在途调用以 err 完成(SwapConn 换下旧连接时)。
func (gc *GameClient) failCallsOn(conn *muduo.Client, err error) {
	gc.callMu.Lock()
	failed := gc.takeCallsLocked(func(call *outstandingCall) bool { return call.conn == conn })
	gc.callMu.Unlock()
	completeAll(failed, err)
}

// failAllCalls 让全部在途调用以 err 完成,并拒绝之后的 Call(Close 时)。
func (gc *GameClient) failAllCalls(err error) {
	gc.callMu.Lock()
	gc.callsClosed = true
	failed := gc.takeCallsLocked(func(*outstandingCall) bool { return true })
	gc.callMu.Unlock()
	completeAll(failed, err)
}

// takeCallsLocked 按请求号升序摘下满足 match 的在途调用,让失败回调的顺序与发起顺序一致。调用方持有 callMu。
func (gc *GameClient) takeCallsLocked(match func(*outstandingCall) bool) []*outstandingCall {
	ids := make([]uint64, 0, len(gc.calls))
	for id, call := range gc.calls {
		if match(call) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	taken := make([]*outstandingCall, 0, len(ids))
	for _, id := range ids {
		taken = append(taken, gc.calls[id])
		delete(gc.calls, id)
	}
	return taken
}

func completeAll(calls []*outstandingCall, err error) {
	for _, call := range calls {
		call.stop()
		call.done(nil, err)
	}
}

// claimReply 认领请求回包。返回 true 表示 m 带非 0 请求号,已交给它的 done,或作为迟到回包计数后丢弃;
// 返回 false 表示 m 是推送 / 不关联请求的回包(id = 0),由调用方按 message_id 派发。
//
// 迟到回包**不得**交给 onMessage:同号的处理器会把它当成推送或别的请求的结果(设计 §2.2)。
func (gc *GameClient) claimReply(m *base.MessageContent) bool {
	id := m.GetId()
	if id == 0 {
		return false
	}
	var err error
	if tip := m.GetErrorMessage().GetId(); tip != 0 {
		err = &ServerTipError{Tip: tip}
	}
	if !gc.finishCall(id, m, err) {
		late := gc.lateReplies.Add(1)
		zap.L().Warn("[rpc] late reply dropped",
			zap.String("account", gc.Account),
			zap.Uint64("request_id", id),
			zap.Uint32("message_id", m.GetMessageId()),
			zap.Uint64("late_total", late))
	}
	return true
}

// deliver 是收包的唯一派发点:带请求号的回包交给 Call 的 done,其余按 message_id 交给 onMessage。
func (gc *GameClient) deliver(onMessage func(*GameClient, *base.MessageContent), m *base.MessageContent) {
	if gc.claimReply(m) {
		return
	}
	onMessage(gc, m)
}

// RecvOne 读下一条推送或不关联请求的回包(id = 0),用于登录阶段的同步握手。
// 途中读到的 Call 回包(id ≠ 0)就地认领后继续读,不返回给调用方。
func (gc *GameClient) RecvOne() (*base.MessageContent, error) {
	for {
		c := gc.conn()
		if c == nil {
			return nil, fmt.Errorf("game client has no connection")
		}
		msg, err := c.Recv()
		if err != nil {
			return nil, err
		}
		m, ok := msg.(*base.MessageContent)
		if !ok {
			return nil, fmt.Errorf("unexpected message type: %T", msg)
		}
		if gc.claimReply(m) {
			continue
		}
		return m, nil
	}
}

// DeferMessage 暂存同步登录阶段提前到达的服务器推送。RecvLoop 启动时会先
// 按 FIFO 顺序补投递，避免 NotifyEnterScene 被等待其他响应的循环吞掉。
func (gc *GameClient) DeferMessage(msg *base.MessageContent) {
	if msg == nil {
		return
	}
	gc.deferredMu.Lock()
	gc.deferredMessages = append(gc.deferredMessages, msg)
	gc.deferredMu.Unlock()
}

func (gc *GameClient) popDeferredMessage() *base.MessageContent {
	gc.deferredMu.Lock()
	defer gc.deferredMu.Unlock()
	if len(gc.deferredMessages) == 0 {
		return nil
	}
	msg := gc.deferredMessages[0]
	gc.deferredMessages[0] = nil
	gc.deferredMessages = gc.deferredMessages[1:]
	return msg
}

func (gc *GameClient) replayDeferredMessages(onMessage func(*GameClient, *base.MessageContent)) {
	for {
		msg := gc.popDeferredMessage()
		if msg == nil {
			return
		}
		gc.deliver(onMessage, msg)
	}
}

// VerifyGateToken sends a ClientTokenVerifyRequest as the first message and
// waits for a ClientTokenVerifyResponse from Gate.
func (gc *GameClient) VerifyGateToken(payload, signature []byte) error {
	c := gc.conn()
	if c == nil {
		return fmt.Errorf("game client has no connection")
	}
	req := &base.ClientTokenVerifyRequest{
		Payload:   payload,
		Signature: signature,
	}
	c.Send(req)

	msg, err := c.Recv()
	if err != nil {
		return fmt.Errorf("recv token verify response: %w", err)
	}

	switch m := msg.(type) {
	case *base.ClientTokenVerifyResponse:
		if !m.Success {
			return fmt.Errorf("gate token rejected: %s", m.Error)
		}
		return nil
	default:
		return fmt.Errorf("unexpected response type for token verify: %T", msg)
	}
}

// VerifyBattleToken 是战斗直连(battle 节点客户端面)的握手:连上 BattleAssignedS2C
// 给的 host:port 后,首包必须是 BattleTokenVerifyRequest,payload / signature 原样
// 来自 BattleAssignedS2C.token_payload / token_signature。成功返回服务端回填的 battle_id。
// 线协议与 gate 完全一致(同一个 muduo codec),所以 GameClient 直接复用:握手之后
// SendRequest / RecvLoop 照常工作,只是消息号只允许 BattleClientPlayer 的四条客户端 RPC。
// 见 docs/design/turn-based-battle-server.md §18。
func (gc *GameClient) VerifyBattleToken(payload, signature []byte) (uint64, error) {
	c := gc.conn()
	if c == nil {
		return 0, fmt.Errorf("game client has no connection")
	}
	req := &battle.BattleTokenVerifyRequest{
		Payload:   payload,
		Signature: signature,
	}
	c.Send(req)

	msg, err := c.Recv()
	if err != nil {
		return 0, fmt.Errorf("recv battle token verify response: %w", err)
	}

	switch m := msg.(type) {
	case *battle.BattleTokenVerifyResponse:
		if !m.Success {
			return 0, fmt.Errorf("battle ticket rejected: %s", m.Error)
		}
		return m.BattleId, nil
	default:
		return 0, fmt.Errorf("unexpected response type for battle token verify: %T", msg)
	}
}

// RecvLoop reads messages from the gate and dispatches them via onMessage.
//
// 带请求号的回包(id ≠ 0)交给发起它的 Call,认领不到的计迟到后丢弃,都不进 onMessage;
// 推送与 SendRequest 的回包(id = 0)按 message_id 交给 onMessage。
//
// 每一圈都重新取一次连接快照:处理器里可能发生 gate 重定向(msg 124),
// 那时 gc 的底层连接已被换成新 zone 的 gate,循环要接着从新连接读。
func (gc *GameClient) RecvLoop(onMessage func(*GameClient, *base.MessageContent)) {
	gc.setDispatch(onMessage)
	gc.replayDeferredMessages(onMessage)
	for {
		c := gc.conn()
		if c == nil {
			zap.L().Error("recv loop stopped: no connection", zap.String("account", gc.Account))
			return
		}
		msg, err := c.Recv()
		if err != nil {
			zap.L().Error("recv error", zap.String("account", gc.Account), zap.Error(err))
			return
		}
		switch m := msg.(type) {
		case *base.MessageContent:
			gc.deliver(onMessage, m)
		default:
			zap.L().Warn("unexpected message type", zap.String("type", fmt.Sprintf("%T", msg)))
		}
	}
}

func (gc *GameClient) setDispatch(onMessage func(*GameClient, *base.MessageContent)) {
	gc.dispatchMu.Lock()
	gc.dispatch = onMessage
	gc.dispatchMu.Unlock()
}

// ReplayDeferred 把登录握手期间暂存的推送补投递给当前 RecvLoop 的分发函数。
// RecvLoop 只在入口 replay 一次,而重定向后会**再跑一遍登录握手**,又会攒下
// 一批暂存消息(典型的就是新 zone 的 NotifyEnterScene),必须显式补投。
// RecvLoop 还没起(或已退出)时无事发生。
func (gc *GameClient) ReplayDeferred() {
	gc.dispatchMu.Lock()
	onMessage := gc.dispatch
	gc.dispatchMu.Unlock()
	if onMessage == nil {
		return
	}
	gc.replayDeferredMessages(onMessage)
}

// Close shuts down the connection.
//
// 在途 Call 先以 ErrRPCDisconnected 完成,之后的 Call 一律 ErrRPCNotSent(Close 是终态)。
// 先失败调用再关连接:vendored muduo 的 Close 要等拨号 goroutine 退出,黑洞地址下可能阻塞到拨号的 OS 超时。
func (gc *GameClient) Close() {
	gc.failAllCalls(ErrRPCDisconnected)
	if c := gc.conn(); c != nil {
		c.Close()
	}
}
