package main

import (
	"encoding/binary"
	"errors"
	"hash/adler32"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"
	"google.golang.org/protobuf/proto"

	"proto/battle"
	base "proto/common/base"
	"robot/generated/pb/game"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// ───────────────────── 假 battle 节点客户端面(只讲 muduo 线协议) ─────────────────────
//
// 用真 TCP 往返把 dialBattleDirect 的契约跑一遍(turn-based §22 D69 / D73),不需要起真服务端:
//   - 握手:收到 BattleTokenVerifyRequest 回 verify,紧接着推 afterVerify 里的帧(模拟观众握手快照);
//   - 上行:记下每个 ClientRequest;GetBattleState 按直连面的形态回一条同消息号、回显请求号的 MessageContent
//     (battle_client_edge.cpp DispatchVerifiedRequest),补拉走 Call 时靠它按请求号认领。

// encodeBattleNodeFrame 手搓一帧 muduo TcpCodec 报文:
//
//	[4B totalLen][4B nameLen]["TypeName "][body][4B adler32(前三段)]
//
// 与 pkg/redirect_test.go 的 encodeFrame 同一格式(测试文件跨包不能复用)。不用 codec.Encode 的原因
// 也相同:它的入参是 golang/protobuf v1 的接口指针,robot 不直接依赖那个包。
func encodeBattleNodeFrame(msg proto.Message) []byte {
	name := []byte(string(msg.ProtoReflect().Descriptor().Name()) + " ")
	body, err := proto.Marshal(msg)
	if err != nil {
		body = nil // 只编固定测试帧,不会失败;真失败了对端解出空包,断言会指出来
	}
	payload := make([]byte, 0, 4+len(name)+len(body)+4)
	var scratch [4]byte
	binary.BigEndian.PutUint32(scratch[:], uint32(len(name)))
	payload = append(payload, scratch[:]...)
	payload = append(payload, name...)
	payload = append(payload, body...)
	binary.BigEndian.PutUint32(scratch[:], adler32.Checksum(payload))
	payload = append(payload, scratch[:]...)

	frame := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	return append(frame, payload...)
}

func mustMarshal(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %T: %v", msg, err)
	}
	return data
}

type fakeBattleNode struct {
	ln          net.Listener
	verify      *battle.BattleTokenVerifyResponse
	afterVerify []*base.MessageContent

	mu       sync.Mutex
	requests []*base.ClientRequest
}

func startFakeBattleNode(t *testing.T, verify *battle.BattleTokenVerifyResponse, afterVerify ...*base.MessageContent) *fakeBattleNode {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	node := &fakeBattleNode{ln: ln, verify: verify, afterVerify: afterVerify}
	t.Cleanup(func() { _ = ln.Close() })
	go node.acceptLoop()
	return node
}

func (n *fakeBattleNode) port() uint32 {
	return uint32(n.ln.Addr().(*net.TCPAddr).Port)
}

func (n *fakeBattleNode) acceptLoop() {
	for {
		conn, err := n.ln.Accept()
		if err != nil {
			return
		}
		go n.serve(conn)
	}
}

func (n *fakeBattleNode) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	codec := &muduo.TcpCodec{}
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		read, err := conn.Read(chunk)
		if read > 0 {
			buf = append(buf, chunk[:read]...)
			for {
				msg, consumed, decErr := codec.Decode(buf)
				if decErr != nil || consumed <= 0 {
					break
				}
				buf = buf[consumed:]
				switch m := msg.(type) {
				case *battle.BattleTokenVerifyRequest:
					_, _ = conn.Write(encodeBattleNodeFrame(n.verify))
					for _, push := range n.afterVerify {
						_, _ = conn.Write(encodeBattleNodeFrame(push))
					}
				case *base.ClientRequest:
					n.mu.Lock()
					n.requests = append(n.requests, m)
					n.mu.Unlock()
					if m.GetMessageId() == game.BattleClientPlayerGetBattleStateMessageId {
						var req battle.GetBattleStateRequest
						_ = proto.Unmarshal(m.GetBody(), &req)
						body, _ := proto.Marshal(&battle.BattleStateS2C{BattleId: req.GetBattleId(), RoundIndex: 1})
						_, _ = conn.Write(encodeBattleNodeFrame(&base.MessageContent{
							Id: m.GetId(), MessageId: m.GetMessageId(), SerializedMessage: body,
						}))
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (n *fakeBattleNode) requestsSnapshot() []*base.ClientRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]*base.ClientRequest(nil), n.requests...)
}

// waitFor 在 timeout 内轮询 cond,超时判失败。只用于等网络往返落地。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testAssignment(port uint32, battleId uint64, role battle.EBattleTicketRole) *battle.BattleAssignedS2C {
	return &battle.BattleAssignedS2C{
		BattleId:       battleId,
		Host:           "127.0.0.1",
		Port:           port,
		TokenPayload:   []byte("payload"),
		TokenSignature: []byte("signature"),
		Role:           role,
	}
}

// 收集回调收到的消息;回调跑在直连的 RecvLoop goroutine 上。
type directInbox struct {
	mu       sync.Mutex
	messages []*base.MessageContent
	players  []uint64
}

func (in *directInbox) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.messages = append(in.messages, msg)
	in.players = append(in.players, client.PlayerId)
}

func (in *directInbox) count() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.messages)
}

// ───────────────────────────────── 用例 ─────────────────────────────────

// 参战者:握手后立刻经直连 GetBattleState 补拉(D69)。补拉走 Call、带非 0 请求号,回包不经 RecvLoop 回调,
// 由补拉的 done 计入 state_replies 再交给回调;直连推送按消息号计数后原样交给回调。
// 回调拿到的是带本人 player_id 的直连客户端(通用分发据此找回同一个 Player)。
func TestDialBattleDirectParticipantPullsStateOnReady(t *testing.T) {
	turn := &base.MessageContent{
		MessageId:         game.BattleClientPlayerNotifyTurnResultMessageId,
		SerializedMessage: mustMarshal(t, &battle.TurnResultS2C{BattleId: 7, RoundIndex: 1}),
	}
	node := startFakeBattleNode(t, &battle.BattleTokenVerifyResponse{Success: true, BattleId: 7}, turn)
	inbox := &directInbox{}

	conn, err := dialBattleDirect("robot_t", 42, testAssignment(node.port(), 7, battle.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT),
		nil, inbox.onMessage)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	waitFor(t, 5*time.Second, "turn result and GetBattleState reply", func() bool { return inbox.count() >= 2 })
	if got := conn.stateReplies.Load(); got != 1 {
		t.Fatalf("state_replies=%d, want 1 (%s)", got, conn.summary())
	}
	if got := conn.turnResults.Load(); got != 1 {
		t.Fatalf("turn_results=%d, want 1 (%s)", got, conn.summary())
	}
	inbox.mu.Lock()
	for _, playerId := range inbox.players {
		if playerId != 42 {
			inbox.mu.Unlock()
			t.Fatalf("callback client player_id=%d, want 42", playerId)
		}
	}
	inbox.mu.Unlock()

	requests := node.requestsSnapshot()
	if len(requests) != 1 || requests[0].GetMessageId() != game.BattleClientPlayerGetBattleStateMessageId {
		t.Fatalf("battle node saw %d requests (first=%v), want exactly one GetBattleState", len(requests), requests)
	}
	if requests[0].GetId() == 0 {
		t.Fatal("catch-up GetBattleState carried request id 0; it must go through Call so its reply is claimed by id")
	}
	var pull battle.GetBattleStateRequest
	if err := proto.Unmarshal(requests[0].GetBody(), &pull); err != nil || pull.GetBattleId() != 7 {
		t.Fatalf("GetBattleState body battle_id=%d err=%v, want 7", pull.GetBattleId(), err)
	}
	if late := conn.gc.LateReplies(); late != 0 {
		t.Fatalf("late_replies=%d, want 0: the catch-up reply must be claimed by its own call", late)
	}
}

// 补拉应答只认带请求号的那条:同消息号却 id = 0 的(battle 没回显请求号)记一行后丢弃,不计入 state_replies、
// 不交回调 —— 否则补拉的 Call 以超时收场时,终局的 state_replies ≥ 1 断言照样能过,掩盖回显缺失。
func TestDialBattleDirectDropsUncorrelatedStateReply(t *testing.T) {
	stray := &base.MessageContent{
		MessageId:         game.BattleClientPlayerGetBattleStateMessageId,
		SerializedMessage: mustMarshal(t, &battle.BattleStateS2C{BattleId: 7, RoundIndex: 1}),
	}
	node := startFakeBattleNode(t, &battle.BattleTokenVerifyResponse{Success: true, BattleId: 7}, stray)
	inbox := &directInbox{}

	conn, err := dialBattleDirect("robot_t", 42, testAssignment(node.port(), 7, battle.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT),
		nil, inbox.onMessage)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 同一连接上的帧按到达顺序派发:无请求号的那条紧跟握手应答、先于补拉应答到达,补拉应答交到回调时它必然已处理完。
	waitFor(t, 5*time.Second, "catch-up GetBattleState reply", func() bool { return inbox.count() >= 1 })
	if got := conn.stateReplies.Load(); got != 1 {
		t.Fatalf("state_replies=%d, want 1: the reply without request id must not be counted (%s)", got, conn.summary())
	}
	if got := conn.total.Load(); got != 1 {
		t.Fatalf("total=%d, want 1: the reply without request id must not be recorded (%s)", got, conn.summary())
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if len(inbox.messages) != 1 || inbox.messages[0].GetId() == 0 {
		t.Fatalf("callback saw %d messages (first id=%d), want only the correlated catch-up reply",
			len(inbox.messages), inbox.messages[0].GetId())
	}
}

// 观众:首帧是 battle 在握手成功时经直连推的 NotifySpectateState(D69),它紧跟握手应答到达、
// 先于回调 goroutine 起来也不能丢;观众不发 GetBattleState —— 之后发出的第一条上行必须是调用方自己的。
func TestDialBattleDirectObserverTakesHandshakeSnapshotWithoutPulling(t *testing.T) {
	snapshot := &base.MessageContent{
		MessageId: game.BattleClientPlayerNotifySpectateStateMessageId,
		SerializedMessage: mustMarshal(t, &battle.SpectateStateS2C{
			State: &battle.BattleStateS2C{BattleId: 7, RoundIndex: 1}, ObserverCount: 1,
		}),
	}
	node := startFakeBattleNode(t, &battle.BattleTokenVerifyResponse{Success: true, BattleId: 7}, snapshot)
	inbox := &directInbox{}

	conn, err := dialBattleDirect("robot_t", 43, testAssignment(node.port(), 7, battle.EBattleTicketRole_BATTLE_TICKET_ROLE_OBSERVER),
		nil, inbox.onMessage)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	waitFor(t, 5*time.Second, "handshake snapshot", func() bool { return inbox.count() >= 1 })
	if got := conn.spectateStates.Load(); got != 1 {
		t.Fatalf("spectate_states=%d, want 1 (%s)", got, conn.summary())
	}

	// 哨兵上行:若观众也补拉了,battle 节点看到的第一条会是 GetBattleState。
	if err := conn.Send(game.BattleClientPlayerStopWatchBattleMessageId, &battle.StopWatchBattleRequest{BattleId: 7}); err != nil {
		t.Fatalf("send sentinel: %v", err)
	}
	waitFor(t, 5*time.Second, "sentinel request", func() bool { return len(node.requestsSnapshot()) >= 1 })
	if first := node.requestsSnapshot()[0].GetMessageId(); first != game.BattleClientPlayerStopWatchBattleMessageId {
		t.Fatalf("first upstream message_id=%d, want the sentinel StopWatchBattle (observer must not pull state)", first)
	}
}

// 直连是唯一通路(D73):任何建连 / 握手问题都必须返回 error,不留连接、不回落。
func TestDialBattleDirectFailsClosed(t *testing.T) {
	inbox := &directInbox{}
	participant := battle.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT

	if _, err := dialBattleDirect("robot_t", 42, nil, nil, inbox.onMessage); err == nil {
		t.Fatal("nil assignment accepted")
	}
	if _, err := dialBattleDirect("robot_t", 42, &battle.BattleAssignedS2C{BattleId: 7, Port: 1}, nil, inbox.onMessage); err == nil {
		t.Fatal("empty host accepted")
	}
	if _, err := dialBattleDirect("robot_t", 42, testAssignment(1, 7, participant), nil, nil); err == nil {
		t.Fatal("nil callback accepted")
	}

	rejecting := startFakeBattleNode(t, &battle.BattleTokenVerifyResponse{Success: false, Error: "ticket expired"})
	if _, err := dialBattleDirect("robot_t", 42, testAssignment(rejecting.port(), 7, participant), nil, inbox.onMessage); err == nil ||
		!strings.Contains(err.Error(), "ticket expired") {
		t.Fatalf("rejected handshake returned err=%v, want the server's reason", err)
	}

	wrongBattle := startFakeBattleNode(t, &battle.BattleTokenVerifyResponse{Success: true, BattleId: 8})
	if _, err := dialBattleDirect("robot_t", 42, testAssignment(wrongBattle.port(), 7, participant), nil, inbox.onMessage); err == nil ||
		!strings.Contains(err.Error(), "battle_id") {
		t.Fatalf("mismatched handshake battle_id returned err=%v, want a battle_id mismatch", err)
	}
	if len(wrongBattle.requestsSnapshot()) != 0 {
		t.Fatal("a rejected connection still sent GetBattleState")
	}
}

// 信封错误(body 为空)只计总数,不算作任何一类有效应答 / 推送。
func TestBattleDirectRecordIgnoresEnvelopeErrors(t *testing.T) {
	conn := &battleDirectConn{battleId: 7}
	conn.record(&base.MessageContent{
		MessageId:    game.BattleClientPlayerGetBattleStateMessageId,
		ErrorMessage: &base.TipInfoMessage{Id: uint32(tiptable.CommonError_kServiceUnavailable)},
	})
	if conn.stateReplies.Load() != 0 || conn.total.Load() != 1 {
		t.Fatalf("envelope error counted as a reply: %s", conn.summary())
	}
	conn.record(&base.MessageContent{MessageId: game.BattleClientPlayerGetBattleStateMessageId})
	conn.record(&base.MessageContent{MessageId: game.BattleClientPlayerNotifySpectateStateMessageId})
	if conn.stateReplies.Load() != 1 || conn.spectateStates.Load() != 1 || conn.total.Load() != 3 {
		t.Fatalf("valid frames miscounted: %s", conn.summary())
	}
}

// 补拉的结局回调(不经网络、不依赖墙钟,直接按 Call 的 done 形状喂结局):没有回包的结局什么都不计、不分发;
// 信封错误只计总数、不算应答、不交回调(body 为空,通用分发会把它当成成功的空应答);
// 成功应答计入 state_replies,并带直连客户端交给回调。
func TestBattleDirectStateReplyDoneCountsAndDispatches(t *testing.T) {
	conn := &battleDirectConn{gc: &pkg.GameClient{PlayerId: 42}, battleId: 7}
	inbox := &directInbox{}
	done := conn.stateReplyDone(nil, inbox.onMessage)

	done(nil, pkg.ErrRPCTimeout)
	done(nil, pkg.ErrRPCDisconnected)
	if conn.total.Load() != 0 || inbox.count() != 0 {
		t.Fatalf("outcome without a reply was counted or dispatched: %s inbox=%d", conn.summary(), inbox.count())
	}

	tip := uint32(tiptable.CommonError_kServiceUnavailable)
	done(&base.MessageContent{
		Id:           1,
		MessageId:    game.BattleClientPlayerGetBattleStateMessageId,
		ErrorMessage: &base.TipInfoMessage{Id: tip},
	}, &pkg.ServerTipError{Tip: tip})
	if conn.stateReplies.Load() != 0 || conn.total.Load() != 1 || inbox.count() != 0 {
		t.Fatalf("envelope error counted as a reply or dispatched: %s inbox=%d", conn.summary(), inbox.count())
	}

	done(&base.MessageContent{
		Id:                2,
		MessageId:         game.BattleClientPlayerGetBattleStateMessageId,
		SerializedMessage: mustMarshal(t, &battle.BattleStateS2C{BattleId: 7, RoundIndex: 1}),
	}, nil)
	if conn.stateReplies.Load() != 1 || conn.total.Load() != 2 || inbox.count() != 1 {
		t.Fatalf("successful reply miscounted or not dispatched: %s inbox=%d", conn.summary(), inbox.count())
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if inbox.players[0] != 42 || inbox.messages[0].GetId() != 2 {
		t.Fatalf("dispatched player_id=%d request_id=%d, want the direct client (42) and the reply itself (2)",
			inbox.players[0], inbox.messages[0].GetId())
	}
}

// 补拉本地没发出(连接已不在)时 dialBattleDirect 要失败返回:ErrRPCNotSent 在 Call 返回前同步到达,
// pullBattleState 必须把它带出来,而不是当成已发出、留到终局才因 state_replies=0 失败。
func TestBattleDirectPullBattleStateReportsNotSent(t *testing.T) {
	conn := &battleDirectConn{gc: &pkg.GameClient{PlayerId: 42}, battleId: 7}
	inbox := &directInbox{}
	if err := conn.pullBattleState(nil, inbox.onMessage); !errors.Is(err, pkg.ErrRPCNotSent) {
		t.Fatalf("pullBattleState on a client without connection returned err=%v, want ErrRPCNotSent", err)
	}
	if conn.total.Load() != 0 || inbox.count() != 0 {
		t.Fatalf("unsent pull was counted or dispatched: %s inbox=%d", conn.summary(), inbox.count())
	}
}
