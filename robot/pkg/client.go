package pkg

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/luyuancpp/muduoclient/muduo"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/battle"
	"proto/common/base"
)

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

	seq atomic.Uint64
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
		old.Close()
	}
	return nil
}

// SendRequest sends a ClientRequest to the gate.
func (gc *GameClient) SendRequest(messageId uint32, body proto.Message) error {
	c := gc.conn()
	if c == nil {
		return fmt.Errorf("game client has no connection")
	}
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	seq := gc.seq.Add(1)
	req := &base.ClientRequest{
		Id:        seq,
		MessageId: messageId,
		Body:      bodyBytes,
	}
	c.Send(req)
	return nil
}

// RecvOne reads and returns a single MessageContent from the gate.
func (gc *GameClient) RecvOne() (*base.MessageContent, error) {
	c := gc.conn()
	if c == nil {
		return nil, fmt.Errorf("game client has no connection")
	}
	msg, err := c.Recv()
	if err != nil {
		return nil, err
	}
	switch m := msg.(type) {
	case *base.MessageContent:
		return m, nil
	default:
		return nil, fmt.Errorf("unexpected message type: %T", msg)
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
		onMessage(gc, msg)
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
			onMessage(gc, m)
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
func (gc *GameClient) Close() {
	if c := gc.conn(); c != nil {
		c.Close()
	}
}
