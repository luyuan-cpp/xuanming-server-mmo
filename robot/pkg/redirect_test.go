package pkg

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

	"proto/common/base"
)

// ───────────────────────── 假 gate(只讲 muduo 的线协议) ─────────────────────────

// encodeFrame 手搓一帧 muduo TcpCodec 报文,与 vendored muduo 的 Encode 完全一致:
//
//	[4B totalLen][4B nameLen]["TypeName "][body][4B adler32(前三段)]
//
// 手搓而不是复用 codec.Encode,是因为 Encode 的入参是 *golang/protobuf 的
// proto.Message 接口指针,复用它就得让 robot 直接依赖那个只该由 vendored 库用的
// v1 包。解码方向没这个问题(返回值可以直接类型断言),所以那边照常用 codec。
func encodeFrame(msg proto.Message) []byte {
	name := []byte(string(msg.ProtoReflect().Descriptor().Name()) + " ")
	body, err := proto.Marshal(msg)
	if err != nil {
		// 本文件只编两条固定应答,marshal 不可能失败;真失败了发一帧空包让对端超时,
		// 比在非测试 goroutine 里 t.Fatalf(那会 panic)强。
		body = nil
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

// fakeGate 是一个只会做 token 握手的假 gate:收到 ClientTokenVerifyRequest 就记下来
// 并按 reply 回一条应答。用它把"重定向后连到新 gate 并验票"这一段跑成真网络往返,
// 不需要起真服务端。
type fakeGate struct {
	ln net.Listener

	mu      sync.Mutex
	verify  []*base.ClientTokenVerifyRequest
	success bool
	errText string
}

func newFakeGate(t *testing.T, success bool, errText string) *fakeGate {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	g := &fakeGate{ln: ln, success: success, errText: errText}
	t.Cleanup(func() { _ = ln.Close() })
	go g.acceptLoop()
	return g
}

func (g *fakeGate) addr() (string, int) {
	a := g.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (g *fakeGate) acceptLoop() {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go g.serve(conn)
	}
}

func (g *fakeGate) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	codec := &muduo.TcpCodec{}
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				msg, consumed, decErr := codec.Decode(buf)
				if decErr != nil || consumed <= 0 {
					break
				}
				buf = buf[consumed:]
				req, ok := msg.(*base.ClientTokenVerifyRequest)
				if !ok {
					continue
				}
				g.mu.Lock()
				g.verify = append(g.verify, req)
				success, errText := g.success, g.errText
				g.mu.Unlock()
				_, _ = conn.Write(encodeFrame(&base.ClientTokenVerifyResponse{
					Success: success,
					Error:   errText,
				}))
			}
		}
		if err != nil {
			return
		}
	}
}

func (g *fakeGate) verifyRequests() []*base.ClientTokenVerifyRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*base.ClientTokenVerifyRequest, len(g.verify))
	copy(out, g.verify)
	return out
}

// closedAddr 返回一个"刚刚还在监听、现在已经关掉"的地址,用来构造"连不上"。
func closedAddr(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return "127.0.0.1", port
}

// withRelogin 注册重登录实现并在用例结束后摘掉(它是进程级全局)。
func withRelogin(t *testing.T, fn ReloginFunc) *int {
	t.Helper()
	calls := 0
	SetRedirectRelogin(func(gc *GameClient) error {
		calls++
		if fn == nil {
			return nil
		}
		return fn(gc)
	})
	t.Cleanup(func() { SetRedirectRelogin(nil) })
	return &calls
}

// ───────────────────────────────── 用例 ─────────────────────────────────

func TestRedirectTargetValidate(t *testing.T) {
	cases := map[string]struct {
		target  RedirectTarget
		wantErr string
	}{
		"ok": {target: RedirectTarget{IP: "10.0.0.1", Port: 9000}},
		"ok with deadline": {target: RedirectTarget{IP: "10.0.0.1", Port: 9000,
			Deadline: time.Now().Add(time.Minute).Unix()}},
		"empty ip":       {target: RedirectTarget{Port: 9000}, wantErr: "empty target_ip"},
		"zero port":      {target: RedirectTarget{IP: "10.0.0.1"}, wantErr: "invalid target_port"},
		"port too large": {target: RedirectTarget{IP: "10.0.0.1", Port: 70000}, wantErr: "invalid target_port"},
		"expired token": {target: RedirectTarget{IP: "10.0.0.1", Port: 9000,
			Deadline: time.Now().Add(-time.Second).Unix()}, wantErr: "already expired"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.target.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// 换连接之前就能判死的情况:必须**一个字节都不碰连接** —— 老 gate 还连着,
// 会话虽然作废但至少没被我们自己拆掉。
func TestFollowRedirect_FailsBeforeTouchingConnection(t *testing.T) {
	old := newFakeGate(t, true, "")
	oldIP, oldPort := old.addr()
	target := RedirectTarget{IP: "10.0.0.1", Port: 9000, Payload: []byte("p")}

	t.Run("nil client", func(t *testing.T) {
		withRelogin(t, nil)
		if err := FollowRedirect(nil, target); err == nil {
			t.Fatal("want error for nil game client")
		}
	})

	t.Run("no relogin registered", func(t *testing.T) {
		SetRedirectRelogin(nil)
		gc, err := NewGameClient(oldIP, oldPort)
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer gc.Close()
		if err := FollowRedirect(gc, target); !errors.Is(err, ErrNoReloginFunc) {
			t.Fatalf("err = %v, want ErrNoReloginFunc", err)
		}
		assertStillOn(t, gc, oldIP, oldPort)
		if gc.RedirectHops() != 0 {
			t.Fatalf("hops = %d, want 0 (nothing was attempted)", gc.RedirectHops())
		}
	})

	t.Run("invalid target", func(t *testing.T) {
		calls := withRelogin(t, nil)
		gc, err := NewGameClient(oldIP, oldPort)
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer gc.Close()
		if err := FollowRedirect(gc, RedirectTarget{IP: "", Port: 0}); err == nil {
			t.Fatal("want error for empty target")
		}
		assertStillOn(t, gc, oldIP, oldPort)
		if *calls != 0 {
			t.Fatalf("relogin ran %d time(s) on an invalid target", *calls)
		}
	})

	t.Run("hop limit", func(t *testing.T) {
		calls := withRelogin(t, nil)
		gc, err := NewGameClient(oldIP, oldPort)
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer gc.Close()
		gc.redirectHops = MaxRedirectHops // 已经跟够了:再跟就是环路
		if err := FollowRedirect(gc, target); err == nil || !strings.Contains(err.Error(), "hop limit") {
			t.Fatalf("err = %v, want a hop-limit refusal", err)
		}
		assertStillOn(t, gc, oldIP, oldPort)
		if *calls != 0 {
			t.Fatalf("relogin ran %d time(s) past the hop limit", *calls)
		}
	})

	t.Run("unreachable target", func(t *testing.T) {
		calls := withRelogin(t, nil)
		gc, err := NewGameClient(oldIP, oldPort)
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer gc.Close()
		deadIP, deadPort := closedAddr(t)
		err = FollowRedirect(gc, RedirectTarget{IP: deadIP, Port: deadPort, Payload: []byte("p")})
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("err = %v, want an unreachable-target error", err)
		}
		// 探测失败发生在换连接之前:老连接必须原封不动。
		assertStillOn(t, gc, oldIP, oldPort)
		if *calls != 0 {
			t.Fatalf("relogin ran %d time(s) against an unreachable gate", *calls)
		}
	})
}

// 全链路:换连接 → 首包验票(payload/signature 原样转发)→ 重登录 → 补投递暂存消息。
func TestFollowRedirect_SwapsVerifiesAndRelogins(t *testing.T) {
	old := newFakeGate(t, true, "")
	oldIP, oldPort := old.addr()
	newGate := newFakeGate(t, true, "")
	newIP, newPort := newGate.addr()

	gc, err := NewGameClient(oldIP, oldPort)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer gc.Close()
	gc.Account = "robot_1"

	// RecvLoop 的分发函数:重定向后必须把握手期间暂存的推送补投出来。
	var replayed []uint32
	gc.setDispatch(func(_ *GameClient, msg *base.MessageContent) {
		replayed = append(replayed, msg.GetMessageId())
	})

	calls := withRelogin(t, func(c *GameClient) error {
		// 模拟真实重登录:同步 send/recv 期间新 zone 的 NotifyEnterScene 先到,
		// 被暂存起来(见 GameClient.DeferMessage)。
		c.DeferMessage(&base.MessageContent{MessageId: 1201})
		c.PlayerId = 777
		return nil
	})

	target := RedirectTarget{
		IP:        newIP,
		Port:      newPort,
		Payload:   []byte("gate-token-payload"),
		Signature: []byte("deadbeef"),
		Deadline:  time.Now().Add(10 * time.Minute).Unix(),
	}
	if err := FollowRedirect(gc, target); err != nil {
		t.Fatalf("FollowRedirect: %v", err)
	}

	assertStillOn(t, gc, newIP, newPort)
	if gc.RedirectHops() != 1 {
		t.Fatalf("hops = %d, want 1", gc.RedirectHops())
	}
	if *calls != 1 {
		t.Fatalf("relogin ran %d time(s), want exactly 1", *calls)
	}
	if gc.PlayerId != 777 {
		t.Fatalf("player id = %d, want the relogin result 777", gc.PlayerId)
	}

	reqs := newGate.verifyRequests()
	if len(reqs) != 1 {
		t.Fatalf("target gate saw %d verify request(s), want exactly 1", len(reqs))
	}
	if string(reqs[0].GetPayload()) != "gate-token-payload" || string(reqs[0].GetSignature()) != "deadbeef" {
		t.Fatalf("verify request = %q/%q, want the notify's payload/signature verbatim",
			reqs[0].GetPayload(), reqs[0].GetSignature())
	}
	if len(old.verifyRequests()) != 0 {
		t.Fatal("old gate received a verify request; the redirect must not re-handshake the old connection")
	}
	if len(replayed) != 1 || replayed[0] != 1201 {
		t.Fatalf("replayed = %v, want the deferred NotifyEnterScene (1201) delivered exactly once", replayed)
	}
}

// 目标 gate 拒票(签名对不上 / 票据过期):FollowRedirect 必须报错而不是"看起来成功"。
func TestFollowRedirect_TokenRejected(t *testing.T) {
	old := newFakeGate(t, true, "")
	oldIP, oldPort := old.addr()
	newGate := newFakeGate(t, false, "token_hmac_mismatch")
	newIP, newPort := newGate.addr()

	gc, err := NewGameClient(oldIP, oldPort)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer gc.Close()
	calls := withRelogin(t, nil)

	err = FollowRedirect(gc, RedirectTarget{IP: newIP, Port: newPort, Payload: []byte("bad")})
	if err == nil || !strings.Contains(err.Error(), "token verify") {
		t.Fatalf("err = %v, want a token-verify failure", err)
	}
	if *calls != 0 {
		t.Fatalf("relogin ran %d time(s) after the gate rejected the ticket", *calls)
	}
	// 验票失败发生在换连接之后 —— 这条会话就死了,连接已经指向新 gate。
	assertStillOn(t, gc, newIP, newPort)
}

// payload 为空 = 服务端跑在空密钥直通档:跳过验票,直接重登录。
func TestFollowRedirect_EmptyPayloadSkipsVerify(t *testing.T) {
	old := newFakeGate(t, true, "")
	oldIP, oldPort := old.addr()
	newGate := newFakeGate(t, true, "")
	newIP, newPort := newGate.addr()

	gc, err := NewGameClient(oldIP, oldPort)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer gc.Close()
	calls := withRelogin(t, nil)

	if err := FollowRedirect(gc, RedirectTarget{IP: newIP, Port: newPort}); err != nil {
		t.Fatalf("FollowRedirect: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("relogin ran %d time(s), want 1", *calls)
	}
	if got := newGate.verifyRequests(); len(got) != 0 {
		t.Fatalf("target gate saw %d verify request(s) despite an empty payload", len(got))
	}
}

// 重登录失败要把错误原样往上抛(handler 据此结束会话,让外层重连)。
func TestFollowRedirect_ReloginFailurePropagates(t *testing.T) {
	old := newFakeGate(t, true, "")
	oldIP, oldPort := old.addr()
	newGate := newFakeGate(t, true, "")
	newIP, newPort := newGate.addr()

	gc, err := NewGameClient(oldIP, oldPort)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer gc.Close()
	withRelogin(t, func(*GameClient) error { return errors.New("account locked") })

	err = FollowRedirect(gc, RedirectTarget{IP: newIP, Port: newPort})
	if err == nil || !strings.Contains(err.Error(), "account locked") {
		t.Fatalf("err = %v, want the relogin error surfaced", err)
	}
}

func assertStillOn(t *testing.T, gc *GameClient, ip string, port int) {
	t.Helper()
	gotIP, gotPort := gc.GateAddr()
	if gotIP != ip || gotPort != port {
		t.Fatalf("client is on %s:%d, want %s:%d", gotIP, gotPort, ip, port)
	}
}
