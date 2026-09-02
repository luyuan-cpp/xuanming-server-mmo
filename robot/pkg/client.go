package pkg

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/luyuancpp/muduoclient/muduo"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

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
	client   *muduo.Client
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

	seq atomic.Uint64
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
	return &GameClient{client: c}, nil
}

// SendRequest sends a ClientRequest to the gate.
func (gc *GameClient) SendRequest(messageId uint32, body proto.Message) error {
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
	gc.client.Send(req)
	return nil
}

// RecvOne reads and returns a single MessageContent from the gate.
func (gc *GameClient) RecvOne() (*base.MessageContent, error) {
	msg, err := gc.client.Recv()
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
	req := &base.ClientTokenVerifyRequest{
		Payload:   payload,
		Signature: signature,
	}
	gc.client.Send(req)

	msg, err := gc.client.Recv()
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

// RecvLoop reads messages from the gate and dispatches them via onMessage.
func (gc *GameClient) RecvLoop(onMessage func(*GameClient, *base.MessageContent)) {
	gc.replayDeferredMessages(onMessage)
	for {
		msg, err := gc.client.Recv()
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

// Close shuts down the connection.
func (gc *GameClient) Close() {
	gc.client.Close()
}
