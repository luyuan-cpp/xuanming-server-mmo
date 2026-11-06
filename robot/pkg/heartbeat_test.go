package pkg

// 客户端心跳(heartbeat.go)的单测。
//
// 发送与节拍都经测试替身:heartbeatSend 记下发出的心跳(带发往的连接),heartbeatAfter 把「等一个间隔」换成手动的 ticks 通道,
// 并把请求等待的时长写进 waits。心跳 goroutine 每次停下来等节拍都会往 waits 写一条,所以「推一拍 → 再等到下一条 waits」
// 之间那次 heartbeatTick 一定已经跑完,断言「没有发」不靠睡眠。
// muduo 连接接到本机监听端口,只为让 gc.conn() / SwapConn / Close 走真实路径;心跳不经过它发送。
//
// 收包认领另有一条端到端用例:假 gate 是一条真实的 TCP 连接,按 gate codec 写帧,RecvOne 读到心跳回包要跳过。

import (
	"encoding/binary"
	"hash/adler32"
	"net"
	"testing"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
)

type sentHeartbeat struct {
	conn *muduo.Client
	seq  uint64
}

type heartbeatHarness struct {
	gc    *GameClient
	port  int
	sent  chan sentHeartbeat
	ticks chan time.Time
	waits chan time.Duration
}

func newHeartbeatHarness(t *testing.T) *heartbeatHarness {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	c, err := muduo.NewClient("127.0.0.1", port, &muduo.TcpCodec{})
	if err != nil {
		t.Fatalf("muduo client: %v", err)
	}
	h := &heartbeatHarness{
		gc:    &GameClient{client: c, gateIP: "127.0.0.1", gatePort: port, Account: "heartbeat-test"},
		port:  port,
		sent:  make(chan sentHeartbeat, 64),
		ticks: make(chan time.Time, 8),
		waits: make(chan time.Duration, 64),
	}
	h.gc.heartbeatSend = func(conn *muduo.Client, request *base.ClientHeartbeatRequest) {
		h.sent <- sentHeartbeat{conn: conn, seq: request.GetSeq()}
	}
	h.gc.heartbeatAfter = func(d time.Duration) <-chan time.Time {
		h.waits <- d
		return h.ticks
	}
	// Close 幂等,用例里自己 Close 过也无妨。
	t.Cleanup(h.gc.Close)
	return h
}

func advert(intervalMs, timeoutMs uint32) *base.ClientTokenVerifyResponse {
	return &base.ClientTokenVerifyResponse{Success: true, HeartbeatIntervalMs: intervalMs, HeartbeatTimeoutMs: timeoutMs}
}

func (h *heartbeatHarness) nextSent(t *testing.T) sentHeartbeat {
	t.Helper()
	select {
	case s := <-h.sent:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat was sent within 5s")
		return sentHeartbeat{}
	}
}

func (h *heartbeatHarness) assertNothingSent(t *testing.T) {
	t.Helper()
	select {
	case s := <-h.sent:
		t.Fatalf("unexpected heartbeat seq=%d", s.seq)
	default:
	}
}

// parked 等心跳 goroutine 停下来等下一拍,返回它这次请求等待的时长。goroutine 每停一次写一条 waits,只能消费一次:
// 起 goroutine 之后先调一次 parked,之后每拍用 tick。
func (h *heartbeatHarness) parked(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-h.waits:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat goroutine did not wait for the next tick within 5s")
		return 0
	}
}

// tick 推一拍,等心跳 goroutine 处理完这一拍、重新停下;返回它重新停下时请求等待的时长。
// 调用前 goroutine 必须已经停着(上一次 parked / tick 之后)。返回时这一拍的 heartbeatTick 已经跑完。
func (h *heartbeatHarness) tick(t *testing.T) time.Duration {
	t.Helper()
	h.ticks <- time.Now()
	return h.parked(t)
}

// assertSingleGoroutine:每次 tick 之后 waits 应该正好被消费空;多出来的一条说明有第二个心跳 goroutine 也停着。
func (h *heartbeatHarness) assertSingleGoroutine(t *testing.T) {
	t.Helper()
	select {
	case d := <-h.waits:
		t.Fatalf("a second heartbeat goroutine is waiting (%s)", d)
	default:
	}
}

func TestHeartbeatStartsRightAfterVerifyAndThenFollowsTheAdvertisedInterval(t *testing.T) {
	h := newHeartbeatHarness(t)
	conn := h.gc.conn()
	h.gc.onGateVerified(conn, advert(10000, 30000))

	// 第一条不等满间隔,验签成功当场就发。
	if first := h.nextSent(t); first.seq != 1 || first.conn != conn {
		t.Fatalf("first heartbeat = seq %d on %p, want seq 1 on the verified connection %p", first.seq, first.conn, conn)
	}
	if got := h.gc.HeartbeatInterval(); got != 10*time.Second {
		t.Fatalf("HeartbeatInterval() = %s, want 10s", got)
	}
	if got := h.gc.HeartbeatTimeout(); got != 30*time.Second {
		t.Fatalf("HeartbeatTimeout() = %s, want 30s", got)
	}
	if waited := h.parked(t); waited != 10*time.Second {
		t.Fatalf("heartbeat goroutine waits %s for the next heartbeat, want the advertised 10s", waited)
	}

	for seq := uint64(2); seq <= 3; seq++ {
		if waited := h.tick(t); waited != 10*time.Second {
			t.Fatalf("heartbeat goroutine waits %s between heartbeats, want the advertised 10s", waited)
		}
		if s := h.nextSent(t); s.seq != seq || s.conn != conn {
			t.Fatalf("heartbeat = seq %d on %p, want seq %d on %p", s.seq, s.conn, seq, conn)
		}
	}
	h.assertNothingSent(t)
	h.assertSingleGoroutine(t)
}

// 旧 gate / 还没实现心跳的那一版服务器不通告:一条都不发,也不起 goroutine(旧 gate 会把心跳当非法帧强关连接)。
func TestNoAdvertMeansNoHeartbeatAndNoGoroutine(t *testing.T) {
	h := newHeartbeatHarness(t)
	h.gc.onGateVerified(h.gc.conn(), &base.ClientTokenVerifyResponse{Success: true})

	h.assertNothingSent(t)
	if got := h.gc.HeartbeatInterval(); got != 0 {
		t.Fatalf("HeartbeatInterval() = %s, want 0 for a gate that does not advertise heartbeats", got)
	}
	h.gc.hb.mu.Lock()
	running := h.gc.hb.running
	h.gc.hb.mu.Unlock()
	if running {
		t.Fatal("heartbeat goroutine started although the gate advertised no interval")
	}
}

// 跟随重定向:SwapConn 换上的新连接在验签之前一条都不发(gate 会把验签前的心跳断开),验签之后当场补发一条,
// 之后照常每拍一条;始终只有一个心跳 goroutine。
func TestHeartbeatWaitsForTheSwappedConnectionToBeVerified(t *testing.T) {
	h := newHeartbeatHarness(t)
	oldConn := h.gc.conn()
	h.gc.onGateVerified(oldConn, advert(10000, 30000))
	h.nextSent(t)
	h.parked(t)

	if err := h.gc.SwapConn("127.0.0.1", h.port); err != nil {
		t.Fatalf("SwapConn: %v", err)
	}
	newConn := h.gc.conn()
	if newConn == oldConn {
		t.Fatal("SwapConn did not replace the connection")
	}
	h.tick(t)
	h.assertNothingSent(t)

	h.gc.onGateVerified(newConn, advert(10000, 30000))
	if s := h.nextSent(t); s.seq != 2 || s.conn != newConn {
		t.Fatalf("heartbeat after re-verify = seq %d on %p, want seq 2 on the new connection %p", s.seq, s.conn, newConn)
	}
	h.tick(t)
	if s := h.nextSent(t); s.seq != 3 || s.conn != newConn {
		t.Fatalf("heartbeat = seq %d on %p, want seq 3 on %p", s.seq, s.conn, newConn)
	}
	h.assertNothingSent(t)
	h.assertSingleGoroutine(t) // 第二次验签没有再起一个 goroutine
}

// 重定向到不支持心跳的 gate:不往那条连接发;goroutine 留着,按上一次的间隔空转。
func TestRedirectToAGateWithoutHeartbeatStopsSending(t *testing.T) {
	h := newHeartbeatHarness(t)
	h.gc.onGateVerified(h.gc.conn(), advert(10000, 30000))
	h.nextSent(t)
	h.parked(t)

	if err := h.gc.SwapConn("127.0.0.1", h.port); err != nil {
		t.Fatalf("SwapConn: %v", err)
	}
	h.gc.onGateVerified(h.gc.conn(), &base.ClientTokenVerifyResponse{Success: true})
	h.assertNothingSent(t)
	if got := h.gc.HeartbeatInterval(); got != 0 {
		t.Fatalf("HeartbeatInterval() = %s after redirect to a gate without heartbeat, want 0", got)
	}
	if waited := h.tick(t); waited != 10*time.Second {
		t.Fatalf("idle heartbeat goroutine waits %s, want the last advertised 10s", waited)
	}
	h.assertNothingSent(t)
}

func TestPauseStopsHeartbeats(t *testing.T) {
	h := newHeartbeatHarness(t)
	h.gc.onGateVerified(h.gc.conn(), advert(10000, 30000))
	h.nextSent(t)
	h.parked(t)

	h.gc.PauseHeartbeat()
	h.tick(t)
	h.tick(t)
	h.assertNothingSent(t)
}

// Close 是终态:不再发,之后再验签也不重起。
func TestCloseStopsHeartbeatsForGood(t *testing.T) {
	h := newHeartbeatHarness(t)
	conn := h.gc.conn()
	h.gc.onGateVerified(conn, advert(10000, 30000))
	h.nextSent(t)
	h.parked(t)

	h.gc.Close()
	h.gc.onGateVerified(conn, advert(10000, 30000))
	h.assertNothingSent(t)
	if got := h.gc.HeartbeatInterval(); got != 0 {
		t.Fatalf("HeartbeatInterval() = %s after Close, want 0", got)
	}
	h.gc.heartbeatTick()
	h.assertNothingSent(t)
}

func TestTakeHeartbeatReplyClaimsOnlyHeartbeatReplies(t *testing.T) {
	gc := &GameClient{}
	if !gc.takeHeartbeatReply(&base.ClientHeartbeatResponse{Seq: 9}) {
		t.Fatal("heartbeat reply was not claimed")
	}
	if gc.takeHeartbeatReply(&base.MessageContent{MessageId: 79}) {
		t.Fatal("a MessageContent was claimed as a heartbeat reply")
	}
	if got := gc.HeartbeatReplies(); got != 1 {
		t.Fatalf("HeartbeatReplies() = %d, want 1", got)
	}
}

// gateFrame 按 gate 的 ProtobufCodec::fillEmptyBuffer 编一帧:[4B len][4B nameLen][名字 + '\0'][body][4B adler32]。
// 玩家协议的消息没有 package,全名就是短名。
func gateFrame(t *testing.T, message proto.Message) []byte {
	t.Helper()
	name := string(message.ProtoReflect().Descriptor().FullName())
	body, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	nameLen := len(name) + 1
	frame := binary.BigEndian.AppendUint32(nil, uint32(4+nameLen+len(body)+4))
	frame = binary.BigEndian.AppendUint32(frame, uint32(nameLen))
	frame = append(frame, name...)
	frame = append(frame, 0)
	frame = append(frame, body...)
	return binary.BigEndian.AppendUint32(frame, adler32.Checksum(frame[4:]))
}

// 登录握手里 RecvOne 读到 gate 的心跳回包:计数、跳过,把后面那条真正的消息交给调用方。
func TestRecvOneSkipsHeartbeatRepliesDuringTheHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	c, err := muduo.NewClient("127.0.0.1", port, &muduo.TcpCodec{})
	if err != nil {
		t.Fatalf("muduo client: %v", err)
	}
	gc := &GameClient{client: c, gateIP: "127.0.0.1", gatePort: port, Account: "heartbeat-recv-test"}
	t.Cleanup(gc.Close)

	if err := ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("listener deadline: %v", err)
	}
	server, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept the robot's connection: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	var stream []byte
	stream = append(stream, gateFrame(t, &base.ClientHeartbeatResponse{Seq: 7})...)
	stream = append(stream, gateFrame(t, &base.MessageContent{MessageId: 79})...)
	if _, err := server.Write(stream); err != nil {
		t.Fatalf("write frames: %v", err)
	}

	type recvResult struct {
		msg *base.MessageContent
		err error
	}
	// RecvOne 阻塞在没有截止时间的通道上;用例只等 5s,超时即失败(那个 goroutine 留到进程退出,同 runBounded 的说明)。
	result := make(chan recvResult, 1)
	go func() {
		msg, err := gc.RecvOne()
		result <- recvResult{msg: msg, err: err}
	}()
	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("RecvOne: %v", r.err)
		}
		if r.msg.GetMessageId() != 79 {
			t.Fatalf("RecvOne returned message_id=%d, want 79 (the heartbeat reply must be skipped)", r.msg.GetMessageId())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecvOne did not return within 5s")
	}
	if got := gc.HeartbeatReplies(); got != 1 {
		t.Fatalf("HeartbeatReplies() = %d, want 1", got)
	}
}
