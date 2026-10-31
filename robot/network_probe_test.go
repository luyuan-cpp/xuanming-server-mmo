package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/adler32"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	"proto/match"
	teampb "proto/team"
	"robot/generated/pb/game"
	"robot/metrics"
	"robot/pkg"
)

// ───────────────────── 假 battle 公网端口(按 battle_client_edge.cpp 的探测状态机走) ─────────────────────

type fakeProbeBattleMode int

const (
	fakeProbeBattleCompletes            fakeProbeBattleMode = iota // 3 轮挑战后回签名回执并关连接
	fakeProbeBattleRejectsGrant                                    // 收到 Start 直接断连(OnProbeStart 验签失败的形态)
	fakeProbeBattleSilentAfterStart                                // 收到 Start 后不发挑战
	fakeProbeBattleSilentAfterFirstEcho                            // 第一次回显后不再发挑战
	fakeProbeBattleEmptyReceipt                                    // 3 轮之后回没有签名回执的 Complete
)

const (
	fakeProbeSamples      = 3
	fakeProbeMeasuredAtMs = uint64(1_000)
	fakeProbeExpiresAtMs  = uint64(121_000)
	fakeProbeValidFor     = 120 * time.Second
	fakeProbeRttMs        = uint32(7)
)

type fakeProbeBattle struct {
	mode fakeProbeBattleMode
	ln   net.Listener

	mu     sync.Mutex
	grants []*match.SignedNetworkProbe
	echoes []*match.NetworkProbeEcho
}

func startFakeProbeBattle(t *testing.T, mode fakeProbeBattleMode) *fakeProbeBattle {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	battleNode := &fakeProbeBattle{mode: mode, ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go battleNode.serve(conn)
		}
	}()
	return battleNode
}

func (b *fakeProbeBattle) target(region uint32) *match.NetworkProbeTarget {
	addr := b.ln.Addr().(*net.TCPAddr)
	payload, _ := proto.Marshal(&match.NetworkProbeGrant{PlanId: "plan", PlayerId: 44, PhysicalRegion: region})
	return &match.NetworkProbeTarget{
		PhysicalRegion: region,
		Host:           addr.IP.String(),
		Port:           uint32(addr.Port),
		Grant:          &match.SignedNetworkProbe{Payload: payload, Signature: []byte("grant-signature")},
	}
}

func (b *fakeProbeBattle) snapshot() ([]*match.SignedNetworkProbe, []*match.NetworkProbeEcho) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*match.SignedNetworkProbe(nil), b.grants...), append([]*match.NetworkProbeEcho(nil), b.echoes...)
}

func fakeProbeNonce(index uint32) []byte {
	return []byte("nonce-" + strconv.FormatUint(uint64(index), 10))
}

func (b *fakeProbeBattle) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	codec := &muduo.TcpCodec{}
	var index uint32
	challenge := func() error {
		return writeNetworkProbeFrame(conn, &match.NetworkProbeChallenge{Nonce: fakeProbeNonce(index), SampleIndex: index})
	}
	for {
		message, err := readNetworkProbeFrame(conn, codec)
		if err != nil {
			return
		}
		switch m := message.(type) {
		case *match.NetworkProbeStart:
			b.mu.Lock()
			b.grants = append(b.grants, m.GetGrant())
			b.mu.Unlock()
			switch b.mode {
			case fakeProbeBattleRejectsGrant:
				return
			case fakeProbeBattleSilentAfterStart:
				continue
			}
			if challenge() != nil {
				return
			}
		case *match.NetworkProbeEcho:
			b.mu.Lock()
			b.echoes = append(b.echoes, m)
			b.mu.Unlock()
			// 与 OnProbeEcho 同样严格:序号或 nonce 对不上就断连。
			if m.GetSampleIndex() != index || !bytes.Equal(m.GetNonce(), fakeProbeNonce(index)) {
				return
			}
			index++
			if b.mode == fakeProbeBattleSilentAfterFirstEcho {
				continue
			}
			if index < fakeProbeSamples {
				if challenge() != nil {
					return
				}
				continue
			}
			_ = writeNetworkProbeFrame(conn, b.complete())
			return
		default:
			return
		}
	}
}

func (b *fakeProbeBattle) complete() *match.NetworkProbeComplete {
	if b.mode == fakeProbeBattleEmptyReceipt {
		return &match.NetworkProbeComplete{}
	}
	grants, _ := b.snapshot()
	grant := &match.NetworkProbeGrant{}
	_ = proto.Unmarshal(grants[len(grants)-1].GetPayload(), grant)
	payload, _ := proto.Marshal(&match.NetworkProbeReceipt{
		Grant: grant, RttMs: fakeProbeRttMs, SampleCount: fakeProbeSamples,
		MeasuredAtMs: fakeProbeMeasuredAtMs, ExpiresAtMs: fakeProbeExpiresAtMs,
	})
	return &match.NetworkProbeComplete{Receipt: &match.SignedNetworkProbe{Payload: payload, Signature: []byte("receipt-signature")}}
}

// ───────────────────── 帧格式 ─────────────────────

// battle 的 ProtobufCodec 按全名反射建对象,这里手工拆帧,钉住 fillEmptyBuffer 的布局。全名是这个文件存在的理由之一:
// muduo.TcpCodec.Encode 写短名,battle 收到后回 kUnknownMessageType 并断连。
func TestEncodeNetworkProbeFrameMatchesProtobufCodecLayout(t *testing.T) {
	message := &match.NetworkProbeEcho{Nonce: []byte("abc"), SampleIndex: 2}
	frame, err := encodeNetworkProbeFrame(message)
	if err != nil {
		t.Fatal(err)
	}
	body := mustMarshal(t, message)
	name := "match.NetworkProbeEcho\x00"

	if got := binary.BigEndian.Uint32(frame[0:4]); int(got) != len(frame)-4 {
		t.Fatalf("len=%d, want frame length without the prefix %d", got, len(frame)-4)
	}
	if got := binary.BigEndian.Uint32(frame[4:8]); int(got) != len(name) {
		t.Fatalf("nameLen=%d, want %d", got, len(name))
	}
	if got := string(frame[8 : 8+len(name)]); got != name {
		t.Fatalf("type name=%q, want %q", got, name)
	}
	if !bytes.Equal(frame[8+len(name):len(frame)-4], body) {
		t.Fatal("body bytes differ")
	}
	if got, want := binary.BigEndian.Uint32(frame[len(frame)-4:]), adler32.Checksum(frame[4:len(frame)-4]); got != want {
		t.Fatalf("checksum=%d, want adler32 over nameLen, name and body=%d", got, want)
	}

	decoded, consumed, err := (&muduo.TcpCodec{}).Decode(frame)
	if err != nil || consumed != uint32(len(frame)) {
		t.Fatalf("decode: consumed=%d err=%v", consumed, err)
	}
	echo, ok := decoded.(*match.NetworkProbeEcho)
	if !ok || echo.GetSampleIndex() != 2 || !bytes.Equal(echo.GetNonce(), []byte("abc")) {
		t.Fatalf("decoded %T %v", decoded, decoded)
	}
}

// 帧头是对端写的:长度越界不能分配、nameLen 越界不能进 Decode(那里直接切片,会 panic)。
func TestReadNetworkProbeFrameRejectsMalformedHeaders(t *testing.T) {
	frameWith := func(size, nameLen uint32) []byte {
		frame := binary.BigEndian.AppendUint32(nil, size)
		frame = binary.BigEndian.AppendUint32(frame, nameLen)
		if size >= 4 && size <= networkProbeMaxFrameBytes {
			frame = append(frame, make([]byte, size-4)...)
		}
		return frame
	}
	for name, frame := range map[string][]byte{
		"too short":         frameWith(networkProbeMinFrameBytes-1, 2),
		"too long":          frameWith(networkProbeMaxFrameBytes+1, 2),
		"name length zero":  frameWith(20, 0),
		"name length large": frameWith(20, 13),
	} {
		t.Run(name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			defer func() { _ = server.Close() }()
			go func() { _, _ = server.Write(frame) }()
			if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := readNetworkProbeFrame(client, &muduo.TcpCodec{}); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("malformed frame not rejected by its header: %v", err)
			}
		})
	}
}

// ───────────────────── 单个机房的采样 ─────────────────────

func TestSampleNetworkProbeTargetEchoesEveryChallengeAndReturnsSignedReceipt(t *testing.T) {
	battleNode := startFakeProbeBattle(t, fakeProbeBattleCompletes)
	target := battleNode.target(3)

	signed, receipt, err := sampleNetworkProbeTarget(target, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signed.GetSignature(), []byte("receipt-signature")) || len(signed.GetPayload()) == 0 {
		t.Fatalf("signed receipt not returned as sent: %v", signed)
	}
	if receipt.GetSampleCount() != fakeProbeSamples || receipt.GetRttMs() != fakeProbeRttMs || receipt.GetGrant().GetPhysicalRegion() != 3 {
		t.Fatalf("decoded receipt: %v", receipt)
	}

	grants, echoes := battleNode.snapshot()
	if len(grants) != 1 || !proto.Equal(grants[0], target.GetGrant()) {
		t.Fatalf("battle received grants %v, want the plan's grant unchanged", grants)
	}
	if len(echoes) != fakeProbeSamples {
		t.Fatalf("battle received %d echoes, want %d", len(echoes), fakeProbeSamples)
	}
	for i, echo := range echoes {
		if echo.GetSampleIndex() != uint32(i) || !bytes.Equal(echo.GetNonce(), fakeProbeNonce(uint32(i))) {
			t.Fatalf("echo %d is not the challenge sent back: %v", i, echo)
		}
	}
}

// 不能挂死:battle 不说话时必须在预算内以超时失败。外层的 watchdog 只为挂死时有明确报错。
func TestSampleNetworkProbeTargetTimesOutWhenBattleGoesSilent(t *testing.T) {
	for name, mode := range map[string]fakeProbeBattleMode{
		"silent after start":      fakeProbeBattleSilentAfterStart,
		"silent after first echo": fakeProbeBattleSilentAfterFirstEcho,
	} {
		t.Run(name, func(t *testing.T) {
			battleNode := startFakeProbeBattle(t, mode)
			done := make(chan error, 1)
			go func() {
				_, _, err := sampleNetworkProbeTarget(battleNode.target(1), 200*time.Millisecond)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("want a deadline error, got %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("sampling hung past its budget")
			}
		})
	}
}

func TestSampleNetworkProbeTargetFailsFast(t *testing.T) {
	t.Run("battle closes on a rejected grant", func(t *testing.T) {
		battleNode := startFakeProbeBattle(t, fakeProbeBattleRejectsGrant)
		_, _, err := sampleNetworkProbeTarget(battleNode.target(1), 5*time.Second)
		if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("want an immediate connection error, got %v", err)
		}
	})
	t.Run("complete without a signed receipt", func(t *testing.T) {
		battleNode := startFakeProbeBattle(t, fakeProbeBattleEmptyReceipt)
		if _, _, err := sampleNetworkProbeTarget(battleNode.target(1), 5*time.Second); err == nil {
			t.Fatal("an empty receipt was accepted")
		}
	})
	t.Run("nothing listens on the endpoint", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		target := &match.NetworkProbeTarget{PhysicalRegion: 1, Host: "127.0.0.1", Port: uint32(port)}
		if _, _, err := sampleNetworkProbeTarget(target, time.Second); err == nil {
			t.Fatal("dial to a closed port succeeded")
		}
	})
	for name, target := range map[string]*match.NetworkProbeTarget{
		"empty host":     {Port: 1234},
		"zero port":      {Host: "127.0.0.1"},
		"port too large": {Host: "127.0.0.1", Port: 70000},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := sampleNetworkProbeTarget(target, time.Second); err == nil {
				t.Fatal("invalid endpoint accepted")
			}
		})
	}
}

// ───────────────────── 一轮完整探测 ─────────────────────

type fakeNetworkProbeLobby struct {
	planResponse   *match.GetNetworkProbePlanResponse
	planErr        error
	commitResponse *match.CommitNetworkProbeResponse
	commitErr      error
	commits        []*match.CommitNetworkProbeRequest
}

func (l *fakeNetworkProbeLobby) plan() (*match.GetNetworkProbePlanResponse, error) {
	return l.planResponse, l.planErr
}

func (l *fakeNetworkProbeLobby) commit(request *match.CommitNetworkProbeRequest) (*match.CommitNetworkProbeResponse, error) {
	l.commits = append(l.commits, request)
	return l.commitResponse, l.commitErr
}

func TestRunNetworkProbeCommitsOneReceiptPerTarget(t *testing.T) {
	first, second := startFakeProbeBattle(t, fakeProbeBattleCompletes), startFakeProbeBattle(t, fakeProbeBattleCompletes)
	lobby := &fakeNetworkProbeLobby{
		planResponse: &match.GetNetworkProbePlanResponse{PlanId: "plan-1", Required: true,
			Targets: []*match.NetworkProbeTarget{first.target(1), second.target(2)}},
		commitResponse: &match.CommitNetworkProbeResponse{ExpiresAtMs: fakeProbeExpiresAtMs},
	}

	result, err := runNetworkProbe(lobby, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.planID != "plan-1" || result.remaining != fakeProbeValidFor {
		t.Fatalf("result plan=%q remaining=%s, want plan-1 and %s", result.planID, result.remaining, fakeProbeValidFor)
	}
	if result.rttMsByRegion[1] != fakeProbeRttMs || result.rttMsByRegion[2] != fakeProbeRttMs {
		t.Fatalf("rtt by region: %v", result.rttMsByRegion)
	}
	if len(lobby.commits) != 1 || lobby.commits[0].GetPlanId() != "plan-1" || len(lobby.commits[0].GetReceipts()) != 2 {
		t.Fatalf("commit requests: %v", lobby.commits)
	}
	for i, signed := range lobby.commits[0].GetReceipts() {
		receipt := &match.NetworkProbeReceipt{}
		if err := proto.Unmarshal(signed.GetPayload(), receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.GetGrant().GetPhysicalRegion() != uint32(i+1) {
			t.Fatalf("receipt %d is for region %d", i, receipt.GetGrant().GetPhysicalRegion())
		}
	}
}

func TestRunNetworkProbeFailsClosed(t *testing.T) {
	healthy := startFakeProbeBattle(t, fakeProbeBattleCompletes)
	rejecting := startFakeProbeBattle(t, fakeProbeBattleRejectsGrant)
	planOf := func(targets ...*match.NetworkProbeTarget) *match.GetNetworkProbePlanResponse {
		return &match.GetNetworkProbePlanResponse{PlanId: "plan-1", Targets: targets}
	}
	okCommit := &match.CommitNetworkProbeResponse{ExpiresAtMs: fakeProbeExpiresAtMs}
	cases := []struct {
		name        string
		lobby       *fakeNetworkProbeLobby
		wantCommits int
	}{
		{name: "plan rpc failed", lobby: &fakeNetworkProbeLobby{planErr: errors.New("envelope")}},
		{name: "plan rejected", lobby: &fakeNetworkProbeLobby{planResponse: &match.GetNetworkProbePlanResponse{
			ErrorMessage: &base.TipInfoMessage{Id: tipOrderingMatchInternal}}}},
		{name: "plan without targets", lobby: &fakeNetworkProbeLobby{planResponse: planOf()}},
		{name: "plan without id", lobby: &fakeNetworkProbeLobby{planResponse: &match.GetNetworkProbePlanResponse{
			Targets: []*match.NetworkProbeTarget{healthy.target(1)}}}},
		// 任一机房失败都不提交部分回执。
		{name: "one target rejects the grant", lobby: &fakeNetworkProbeLobby{
			planResponse: planOf(healthy.target(1), rejecting.target(2)), commitResponse: okCommit}},
		{name: "commit rpc failed", wantCommits: 1, lobby: &fakeNetworkProbeLobby{
			planResponse: planOf(healthy.target(1)), commitErr: errors.New("envelope")}},
		{name: "commit rejected", wantCommits: 1, lobby: &fakeNetworkProbeLobby{planResponse: planOf(healthy.target(1)),
			commitResponse: &match.CommitNetworkProbeResponse{ErrorMessage: &base.TipInfoMessage{Id: tipOrderingMatchInternal}}}},
		{name: "commit expires before measurement", wantCommits: 1, lobby: &fakeNetworkProbeLobby{planResponse: planOf(healthy.target(1)),
			commitResponse: &match.CommitNetworkProbeResponse{ExpiresAtMs: fakeProbeMeasuredAtMs}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := runNetworkProbe(c.lobby, 5*time.Second); err == nil {
				t.Fatal("failure was swallowed")
			}
			if len(c.lobby.commits) != c.wantCommits {
				t.Fatalf("commits=%d, want %d", len(c.lobby.commits), c.wantCommits)
			}
		})
	}
}

// ───────────────────── 按玩家缓存 ─────────────────────

func TestNetworkProbeCacheReusesResultUntilRefreshMargin(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cache := newNetworkProbeCache(func() time.Time { return now })
	session := networkProbeSession{client: &pkg.GameClient{}, gateHost: "127.0.0.1", gatePort: 1}
	runs := 0
	run := func() (networkProbeResult, error) {
		runs++
		return networkProbeResult{planID: "plan-" + strconv.Itoa(runs), remaining: 120 * time.Second}, nil
	}
	expect := func(step, wantPlan string, wantRuns int) {
		t.Helper()
		plan, err := cache.ensure(44, session, run)
		if err != nil || plan != wantPlan || runs != wantRuns {
			t.Fatalf("%s: plan=%q runs=%d err=%v, want plan=%q runs=%d", step, plan, runs, err, wantPlan, wantRuns)
		}
	}

	expect("first probe", "plan-1", 1)
	expect("reused", "plan-1", 1)
	now = now.Add(120*time.Second - networkProbeRefreshMargin - time.Millisecond)
	expect("reused just before the margin", "plan-1", 1)
	now = now.Add(time.Millisecond)
	expect("probed again inside the margin", "plan-2", 2)
}

// 结果绑在会话上:重新登录(新 GameClient)、重定向到别的 gate(同一 GameClient 换地址)之后不能复用;别的玩家也不共享。
func TestNetworkProbeCacheDoesNotCrossSessionsOrPlayers(t *testing.T) {
	cache := newNetworkProbeCache(time.Now)
	client := &pkg.GameClient{}
	runs := 0
	run := func() (networkProbeResult, error) {
		runs++
		return networkProbeResult{planID: "plan-" + strconv.Itoa(runs), remaining: 120 * time.Second}, nil
	}
	original := networkProbeSession{client: client, gateHost: "127.0.0.1", gatePort: 1}
	steps := []struct {
		name    string
		player  uint64
		session networkProbeSession
		want    string
	}{
		{"first", 44, original, "plan-1"},
		{"same session", 44, original, "plan-1"},
		{"new login", 44, networkProbeSession{client: &pkg.GameClient{}, gateHost: "127.0.0.1", gatePort: 1}, "plan-2"},
		{"redirected to another gate", 44, networkProbeSession{client: client, gateHost: "127.0.0.1", gatePort: 2}, "plan-3"},
		{"another player", 45, networkProbeSession{client: client, gateHost: "127.0.0.1", gatePort: 2}, "plan-4"},
	}
	for _, step := range steps {
		plan, err := cache.ensure(step.player, step.session, run)
		if err != nil || plan != step.want {
			t.Fatalf("%s: plan=%q err=%v, want %q", step.name, plan, err, step.want)
		}
	}
}

func TestNetworkProbeCacheDoesNotKeepFailures(t *testing.T) {
	cache := newNetworkProbeCache(time.Now)
	session := networkProbeSession{client: &pkg.GameClient{}}
	failure := errors.New("probe failed")
	if _, err := cache.ensure(44, session, func() (networkProbeResult, error) { return networkProbeResult{}, failure }); !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
	plan, err := cache.ensure(44, session, func() (networkProbeResult, error) {
		return networkProbeResult{planID: "plan-1", remaining: 120 * time.Second}, nil
	})
	if err != nil || plan != "plan-1" {
		t.Fatalf("plan=%q err=%v after a failed probe", plan, err)
	}
}

// ───────────────────── 假 gate:经真实 GameClient 走大厅 RPC ─────────────────────

// fakeProbeGate 只讲 muduo 线协议:按 handle 的结果回一条带请求号的 MessageContent;handle 返回 nil 就不回包。
type fakeProbeGate struct {
	ln     net.Listener
	handle func(*base.ClientRequest) *base.MessageContent

	mu       sync.Mutex
	requests []*base.ClientRequest
}

func startFakeProbeGate(t *testing.T, handle func(*base.ClientRequest) *base.MessageContent) *fakeProbeGate {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gate := &fakeProbeGate{ln: ln, handle: handle}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go gate.serve(conn)
		}
	}()
	return gate
}

func (g *fakeProbeGate) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	codec := &muduo.TcpCodec{}
	for {
		message, err := readNetworkProbeFrame(conn, codec)
		if err != nil {
			return
		}
		request, ok := message.(*base.ClientRequest)
		if !ok {
			return
		}
		g.mu.Lock()
		g.requests = append(g.requests, request)
		g.mu.Unlock()
		if reply := g.handle(request); reply != nil {
			reply.Id, reply.MessageId = request.GetId(), request.GetMessageId()
			if _, err := conn.Write(encodeBattleNodeFrame(reply)); err != nil {
				return
			}
		}
	}
}

func (g *fakeProbeGate) requestsTo(messageId uint32) []*base.ClientRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var matched []*base.ClientRequest
	for _, request := range g.requests {
		if request.GetMessageId() == messageId {
			matched = append(matched, request)
		}
	}
	return matched
}

func (g *fakeProbeGate) dial(t *testing.T, playerId uint64) *pkg.GameClient {
	t.Helper()
	addr := g.ln.Addr().(*net.TCPAddr)
	gc, err := pkg.NewGameClient(addr.IP.String(), addr.Port)
	if err != nil {
		t.Fatalf("connect fake gate: %v", err)
	}
	gc.Account = "robot_t"
	gc.PlayerId = playerId
	t.Cleanup(gc.Close)
	go gc.RecvLoop(func(*pkg.GameClient, *base.MessageContent) {})
	return gc
}

func fakeGateReply(message proto.Message) *base.MessageContent {
	body, _ := proto.Marshal(message)
	return &base.MessageContent{SerializedMessage: body}
}

// fakeProbeLobbyReply 回答探测的两个 RPC;不是这两个消息号时 ok = false,由调用方接着处理。
func fakeProbeLobbyReply(request *base.ClientRequest, planId string, target *match.NetworkProbeTarget) (reply *base.MessageContent, ok bool) {
	switch request.GetMessageId() {
	case game.MatchServiceGetNetworkProbePlanMessageId:
		return fakeGateReply(&match.GetNetworkProbePlanResponse{PlanId: planId, Required: true,
			Targets: []*match.NetworkProbeTarget{target}}), true
	case game.MatchServiceCommitNetworkProbeMessageId:
		return fakeGateReply(&match.CommitNetworkProbeResponse{ExpiresAtMs: fakeProbeExpiresAtMs}), true
	}
	return nil, false
}

func TestEnsureNetworkProbeRunsTheProtocolOnceAndReusesThePlan(t *testing.T) {
	battleNode := startFakeProbeBattle(t, fakeProbeBattleCompletes)
	target := battleNode.target(1)
	gate := startFakeProbeGate(t, func(request *base.ClientRequest) *base.MessageContent {
		reply, _ := fakeProbeLobbyReply(request, "plan-e2e", target)
		return reply
	})
	gc := gate.dial(t, 9_100_001)

	for attempt := 0; attempt < 2; attempt++ {
		plan, err := ensureNetworkProbe(gc, nil, gc.Account)
		if err != nil || plan != "plan-e2e" {
			t.Fatalf("attempt %d: plan=%q err=%v", attempt, plan, err)
		}
	}
	if got := len(gate.requestsTo(game.MatchServiceGetNetworkProbePlanMessageId)); got != 1 {
		t.Fatalf("GetNetworkProbePlan sent %d times, want 1 (second ensure must reuse the plan)", got)
	}
	commits := gate.requestsTo(game.MatchServiceCommitNetworkProbeMessageId)
	if len(commits) != 1 {
		t.Fatalf("CommitNetworkProbe sent %d times, want 1", len(commits))
	}
	commit := &match.CommitNetworkProbeRequest{}
	if err := proto.Unmarshal(commits[0].GetBody(), commit); err != nil {
		t.Fatal(err)
	}
	if commit.GetPlanId() != "plan-e2e" || len(commit.GetReceipts()) != 1 ||
		!bytes.Equal(commit.GetReceipts()[0].GetSignature(), []byte("receipt-signature")) {
		t.Fatalf("commit request: %v", commit)
	}
	if _, echoes := battleNode.snapshot(); len(echoes) != fakeProbeSamples {
		t.Fatalf("battle saw %d echoes, want %d", len(echoes), fakeProbeSamples)
	}
}

func TestEnsureNetworkProbeSurfacesEnvelopeRejection(t *testing.T) {
	gate := startFakeProbeGate(t, func(*base.ClientRequest) *base.MessageContent {
		return &base.MessageContent{ErrorMessage: &base.TipInfoMessage{Id: smokeTipServiceUnavailable}}
	})
	gc := gate.dial(t, 9_100_002)
	_, err := ensureNetworkProbe(gc, nil, gc.Account)
	if err == nil || !strings.Contains(err.Error(), "tip="+strconv.FormatUint(uint64(smokeTipServiceUnavailable), 10)) {
		t.Fatalf("want the envelope tip in the error, got %v", err)
	}
}

// ───────────────────── 各场景发出的请求 ─────────────────────

var canonicalUUIDv4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestBattleSmokeWatchBattleSendsCanonicalRequestId(t *testing.T) {
	gate := startFakeProbeGate(t, func(*base.ClientRequest) *base.MessageContent {
		return fakeGateReply(&match.WatchBattleResponse{BattleId: 7})
	})
	bot := &battleSmokeBot{account: "robot_t", gc: gate.dial(t, 9_100_003)}

	battleId, err := battleSmokeWatchBattle(bot, nil, 5*time.Second)
	if err != nil || battleId != 7 {
		t.Fatalf("battle_id=%d err=%v", battleId, err)
	}
	sent := gate.requestsTo(game.MatchServiceWatchBattleMessageId)
	if len(sent) != 1 {
		t.Fatalf("WatchBattle sent %d times", len(sent))
	}
	request := &match.WatchBattleRequest{}
	if err := proto.Unmarshal(sent[0].GetBody(), request); err != nil {
		t.Fatal(err)
	}
	if !canonicalUUIDv4.MatchString(request.GetRequestId()) {
		t.Fatalf("request_id=%q is not a canonical UUID", request.GetRequestId())
	}
}

// O3 的前提:Cancel 与 Join 共用 request_id(不带编号也不带票号的撤销服务端直接回 InvalidArgument),Join 带开放模板与探测计划。
func TestOrderingPipelineSharesRequestIdBetweenJoinAndCancel(t *testing.T) {
	battleNode := startFakeProbeBattle(t, fakeProbeBattleCompletes)
	target := battleNode.target(1)
	gate := startFakeProbeGate(t, func(request *base.ClientRequest) *base.MessageContent {
		if reply, ok := fakeProbeLobbyReply(request, "plan-o3", target); ok {
			return reply
		}
		if request.GetMessageId() == game.MatchServiceJoinQueueMessageId {
			return fakeGateReply(&match.JoinQueueResponse{QueueTicket: "ticket-1"})
		}
		return fakeGateReply(&base.Empty{})
	})
	bot := &orderingSmokeBot{account: "robot_t", gc: gate.dial(t, 9_100_004), stats: metrics.NewStats()}

	result, err := bot.pipelineJoinThenCancel()
	if err != nil {
		t.Fatal(err)
	}
	if result.ticket != "ticket-1" || result.joinErrorCode != 0 {
		t.Fatalf("pipeline result: %+v", result)
	}
	joins, cancels := gate.requestsTo(game.MatchServiceJoinQueueMessageId), gate.requestsTo(game.MatchServiceCancelQueueMessageId)
	if len(joins) != 1 || len(cancels) != 1 {
		t.Fatalf("joins=%d cancels=%d", len(joins), len(cancels))
	}
	join, cancel := &match.JoinQueueRequest{}, &match.CancelQueueRequest{}
	if err := proto.Unmarshal(joins[0].GetBody(), join); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(cancels[0].GetBody(), cancel); err != nil {
		t.Fatal(err)
	}
	if !canonicalUUIDv4.MatchString(join.GetRequestId()) || cancel.GetRequestId() != join.GetRequestId() || result.requestId != join.GetRequestId() {
		t.Fatalf("join request_id=%q cancel request_id=%q result=%q", join.GetRequestId(), cancel.GetRequestId(), result.requestId)
	}
	if join.GetNetworkPlanId() != "plan-o3" || join.GetBattleConfigId() != crossZoneBattleConfigId || join.GetMode() != match.MatchMode_MATCH_MODE_1V1 {
		t.Fatalf("join request: %v", join)
	}
}

// Join 回 kMatchInternal 交调用方查墓碑判定(墓碑抢先时就回这个码);其余业务拒绝直接报错。
func TestOrderingPipelineClassifiesJoinRejections(t *testing.T) {
	battleNode := startFakeProbeBattle(t, fakeProbeBattleCompletes)
	target := battleNode.target(1)
	for name, c := range map[string]struct {
		code    uint32
		wantErr bool
	}{
		"tombstone or internal": {code: tipOrderingMatchInternal},
		"already queued":        {code: tipOrderingAlreadyQueued, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			gate := startFakeProbeGate(t, func(request *base.ClientRequest) *base.MessageContent {
				if reply, ok := fakeProbeLobbyReply(request, "plan-o3", target); ok {
					return reply
				}
				if request.GetMessageId() == game.MatchServiceJoinQueueMessageId {
					return fakeGateReply(&match.JoinQueueResponse{ErrorCode: c.code})
				}
				return fakeGateReply(&base.Empty{})
			})
			bot := &orderingSmokeBot{account: "robot_t", gc: gate.dial(t, 9_100_005), stats: metrics.NewStats()}
			result, err := bot.pipelineJoinThenCancel()
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if err == nil && result.joinErrorCode != c.code {
				t.Fatalf("join_error_code=%d, want %d", result.joinErrorCode, c.code)
			}
		})
	}
}

// 过渡态重试是同一次发起意图:request_id 只生成一次,且服务端按原件比较请求,整份请求都不能变。
func TestTeamSmokeStartTeamMatchReusesRequestAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	gate := startFakeProbeGate(t, func(*base.ClientRequest) *base.MessageContent {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return fakeGateReply(&teampb.TeamResponse{ErrorMessage: &base.TipInfoMessage{Id: tipTeamMemberInBattle}})
		}
		return fakeGateReply(&teampb.TeamResponse{Team: &teampb.TeamView{TeamId: 5}})
	})
	gc := gate.dial(t, 9_100_006)
	stats := metrics.NewStats()
	leader := &teamSmokeBot{name: "A", account: "robot_t", gc: gc, stats: stats,
		caller: newSmokeCaller(gc, stats, "robot_t", 5*time.Second, errTeamSmokeTimeout)}

	view, err := teamSmokeStartTeamMatch(leader, 5, 1)
	if err != nil || view.GetTeamId() != 5 {
		t.Fatalf("view=%v err=%v", view, err)
	}
	sent := gate.requestsTo(game.ClientPlayerTeamStartTeamMatchMessageId)
	if len(sent) != 2 || !bytes.Equal(sent[0].GetBody(), sent[1].GetBody()) {
		t.Fatalf("sent %d requests; retries must repeat the same body", len(sent))
	}
	request := &teampb.StartTeamMatchRequest{}
	if err := proto.Unmarshal(sent[0].GetBody(), request); err != nil {
		t.Fatal(err)
	}
	if !canonicalUUIDv4.MatchString(request.GetRequestId()) || request.GetExpectedTeamId() != 5 || request.GetBattleConfigId() != 1 {
		t.Fatalf("request: %v", request)
	}
}

// 撤销必须带票号或原请求编号:查到票号才发,带的就是查到的那个;不在排什么也不发。
func TestBackfillCancelQueueCancelsTheTicketFromStatus(t *testing.T) {
	for name, ticket := range map[string]string{"queued": "ticket-1", "not queued": ""} {
		t.Run(name, func(t *testing.T) {
			gate := startFakeProbeGate(t, func(request *base.ClientRequest) *base.MessageContent {
				if request.GetMessageId() == game.MatchServiceGetQueueStatusMessageId {
					return fakeGateReply(&match.GetQueueStatusResponse{QueueTicket: ticket})
				}
				return fakeGateReply(&base.Empty{})
			})
			gc := gate.dial(t, 9_100_007)
			stats := metrics.NewStats()
			bot := &teamSmokeBot{name: "B", account: "robot_t", gc: gc, stats: stats,
				caller: newSmokeCaller(gc, stats, "robot_t", 5*time.Second, errTeamSmokeTimeout)}

			if err := backfillCancelQueue(bot); err != nil {
				t.Fatal(err)
			}
			cancels := gate.requestsTo(game.MatchServiceCancelQueueMessageId)
			if ticket == "" {
				if len(cancels) != 0 {
					t.Fatal("CancelQueue sent while not queued")
				}
				return
			}
			if len(cancels) != 1 {
				t.Fatalf("CancelQueue sent %d times", len(cancels))
			}
			cancel := &match.CancelQueueRequest{}
			if err := proto.Unmarshal(cancels[0].GetBody(), cancel); err != nil {
				t.Fatal(err)
			}
			if cancel.GetQueueTicket() != ticket {
				t.Fatalf("queue_ticket=%q, want %q", cancel.GetQueueTicket(), ticket)
			}
		})
	}
}
