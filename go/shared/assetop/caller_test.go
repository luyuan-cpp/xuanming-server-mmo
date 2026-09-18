package assetop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	smpb "proto/scene_manager"

	"shared/scenenode"
)

// 本文件用 bufconn 起一个**假 scene**:走真实的 gRPC 编解码与客户端存根,
// 只把 loop 线程里的逻辑换成脚本。这样契约(方法名、字段、签名进了包体)是被真的跑过的,
// 而不是靠一个手写的接口替身糊过去。

// sceneCall 是假 scene 收到的一次调用。请求已克隆,断言时不怕被后续调用改掉。
type sceneCall struct {
	RPC RPC
	Req *assetpb.AssetOpRequest
}

// sceneHandler 按「第几次调用」给答复。n 从 1 开始。
type sceneHandler func(rpc RPC, req *assetpb.AssetOpRequest, n int) (*assetpb.AssetOpResponse, error)

type fakeScene struct {
	smpb.UnimplementedSceneNodeGrpcServer

	mu      sync.Mutex
	calls   []sceneCall
	handler sceneHandler
}

func (f *fakeScene) AssetDebit(_ context.Context, req *assetpb.AssetOpRequest) (*assetpb.AssetOpResponse, error) {
	return f.record(RPCDebit, req)
}

func (f *fakeScene) AssetCredit(_ context.Context, req *assetpb.AssetOpRequest) (*assetpb.AssetOpResponse, error) {
	return f.record(RPCCredit, req)
}

func (f *fakeScene) AssetAbortDebit(_ context.Context, req *assetpb.AssetOpRequest) (*assetpb.AssetOpResponse, error) {
	return f.record(RPCAbort, req)
}

func (f *fakeScene) record(rpc RPC, req *assetpb.AssetOpRequest) (*assetpb.AssetOpResponse, error) {
	f.mu.Lock()
	cloned, _ := proto.Clone(req).(*assetpb.AssetOpRequest)
	f.calls = append(f.calls, sceneCall{RPC: rpc, Req: cloned})
	n := len(f.calls)
	h := f.handler
	f.mu.Unlock()
	return h(rpc, req, n)
}

func (f *fakeScene) recorded() []sceneCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sceneCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeScene) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// startFakeScene 起一个 bufconn 上的假 scene,返回它与一个连上它的客户端存根。
func startFakeScene(t *testing.T, h sceneHandler) (*fakeScene, smpb.SceneNodeGrpcClient) {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeScene{handler: h}
	smpb.RegisterSceneNodeGrpcServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("连假 scene 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	})
	return fake, smpb.NewSceneNodeGrpcClient(conn)
}

// fakeResolver 按调用次序吐出目标;次数超过列表长度时一直用最后一个。
type fakeResolver struct {
	mu      sync.Mutex
	targets []scenenode.Target
	err     error
	calls   int
}

func (r *fakeResolver) Resolve(_ context.Context, _ uint64) (scenenode.Target, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return scenenode.Target{}, r.err
	}
	idx := r.calls - 1
	if idx >= len(r.targets) {
		idx = len(r.targets) - 1
	}
	return r.targets[idx], nil
}

func (r *fakeResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

const testSecret = "unit-test-asset-op-secret-0000000000"

func testSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner("guild", testSecret)
	if err != nil {
		t.Fatalf("建签名器失败: %v", err)
	}
	return s
}

// testRequest 与 auth_test 的 golden 用同一组取值,方便对照。
func testRequest() *assetpb.AssetOpRequest {
	return &assetpb.AssetOpRequest{
		PlayerId:      42,
		Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		Seq:           7,
		CorrelationId: 99,
		TxType:        24,
		StreamEpoch:   1700000000000,
		Bundle: &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 30}},
		},
	}
}

func applied(durable bool) *assetpb.AssetOpResponse {
	return &assetpb.AssetOpResponse{
		Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED,
		Durable: durable,
	}
}

func newTestCaller(t *testing.T, r Resolver, requery []time.Duration) *Caller {
	t.Helper()
	return &Caller{
		Resolver:    r,
		Signer:      testSigner(t),
		CallTimeout: 500 * time.Millisecond,
		Requery:     requery,
	}
}

func targetOf(endpoint string, client smpb.SceneNodeGrpcClient) scenenode.Target {
	return scenenode.Target{
		Location: &smpb.PlayerLocation{NodeId: endpoint, ZoneId: 1},
		Endpoint: endpoint,
		Client:   client,
	}
}

// ① 首答未 durable,重查一次拿到 durable。
func TestCallerRequeriesUntilDurable(t *testing.T) {
	scene, client := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, n int) (*assetpb.AssetOpResponse, error) {
		return applied(n >= 2), nil
	})
	caller := newTestCaller(t, &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}},
		[]time.Duration{10 * time.Millisecond, 20 * time.Millisecond})

	res, err := caller.Do(context.Background(), RPCDebit, testRequest())
	if err != nil {
		t.Fatalf("Do 出错: %v", err)
	}
	if !res.Terminal() {
		t.Fatalf("应当可终结,实际 outcome=%v durable=%v", res.Outcome, res.Durable)
	}
	if got := scene.callCount(); got != 2 {
		t.Fatalf("应当恰好调用 2 次,实际 %d", got)
	}
}

// ② ctx 预算用完:返回最后一次(未 durable)结果且不报错。
func TestCallerStopsRequeryWhenContextExpires(t *testing.T) {
	scene, client := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		return applied(false), nil
	})
	caller := newTestCaller(t, &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}},
		[]time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	res, err := caller.Do(ctx, RPCDebit, testRequest())
	if err != nil {
		t.Fatalf("预算用完不应报错,实际: %v", err)
	}
	if res.Durable {
		t.Fatal("不该拿到 durable")
	}
	if res.Terminal() {
		t.Fatal("未 durable 的结果不得可终结")
	}
	if got := scene.callCount(); got > 2 {
		t.Fatalf("150ms 内最多 2 次调用,实际 %d", got)
	}
}

// ③ NOT_HERE 且位置换了节点:只对新节点重调一次。
func TestCallerRetriesOnNewNodeAfterNotHere(t *testing.T) {
	oldScene, oldClient := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		return &assetpb.AssetOpResponse{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE}, nil
	})
	newScene, newClient := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		return applied(true), nil
	})

	resolver := &fakeResolver{targets: []scenenode.Target{
		targetOf("scene-old:9000", oldClient),
		targetOf("scene-new:9000", newClient),
	}}
	caller := newTestCaller(t, resolver, nil)

	res, err := caller.Do(context.Background(), RPCDebit, testRequest())
	if err != nil {
		t.Fatalf("Do 出错: %v", err)
	}
	if !res.Terminal() {
		t.Fatalf("应当在新节点拿到可终结结果,实际 %+v", res)
	}
	if got := oldScene.callCount(); got != 1 {
		t.Fatalf("旧节点应当只被调 1 次,实际 %d", got)
	}
	if got := newScene.callCount(); got != 1 {
		t.Fatalf("新节点应当只被调 1 次,实际 %d", got)
	}
	// 定位恰好两次:首次 + NOT_HERE 之后的复查。多于两次说明退化成了轮询。
	if got := resolver.callCount(); got != 2 {
		t.Fatalf("应当定位 2 次,实际 %d", got)
	}
}

// ④ NOT_HERE 但位置没变:不重调(重调只会得到同样答复,还白占 scene 的 poller)。
func TestCallerDoesNotRecallSameNodeOnNotHere(t *testing.T) {
	scene, client := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		return &assetpb.AssetOpResponse{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE}, nil
	})
	caller := newTestCaller(t, &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}}, nil)

	res, err := caller.Do(context.Background(), RPCDebit, testRequest())
	if err != nil {
		t.Fatalf("Do 出错: %v", err)
	}
	if res.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE {
		t.Fatalf("应当仍是 NOT_HERE,实际 %v", res.Outcome)
	}
	if res.Local {
		t.Fatal("这次是 scene 答的 NOT_HERE,不是本地合成")
	}
	if got := scene.callCount(); got != 1 {
		t.Fatalf("应当只调 1 次,实际 %d", got)
	}
}

// ⑤ 结局在两次查询之间变了:必须报 ErrOutcomeFlip(违反不变量 I2)。
func TestCallerDetectsOutcomeFlip(t *testing.T) {
	_, client := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, n int) (*assetpb.AssetOpResponse, error) {
		if n == 1 {
			return applied(false), nil
		}
		return &assetpb.AssetOpResponse{
			Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED,
			Durable: true,
		}, nil
	})
	caller := newTestCaller(t, &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}},
		[]time.Duration{10 * time.Millisecond})

	_, err := caller.Do(context.Background(), RPCDebit, testRequest())
	if !errors.Is(err, ErrOutcomeFlip) {
		t.Fatalf("应当报 ErrOutcomeFlip,实际: %v", err)
	}
}

// ⑥ 玩家不在线:本地合成 NOT_HERE,一次 RPC 都不发。
func TestCallerNotOnlineIsLocalNotHere(t *testing.T) {
	scene, _ := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		return applied(true), nil
	})
	caller := newTestCaller(t, &fakeResolver{err: fmt.Errorf("查位置: %w", scenenode.ErrNotOnline)}, nil)

	res, err := caller.Do(context.Background(), RPCDebit, testRequest())
	if err != nil {
		t.Fatalf("不在线不是错误,实际: %v", err)
	}
	if !res.Local || res.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE {
		t.Fatalf("应当是本地合成的 NOT_HERE,实际 %+v", res)
	}
	if got := scene.callCount(); got != 0 {
		t.Fatalf("不该发出任何 RPC,实际 %d 次", got)
	}
}

// ⑦ 传输层错误原样上抛,由调用方按重试处理。
func TestCallerPropagatesTransportError(t *testing.T) {
	_, client := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		return nil, status.Error(codes.Unavailable, "scene 正在重启")
	})
	caller := newTestCaller(t, &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}}, nil)

	if _, err := caller.Do(context.Background(), RPCDebit, testRequest()); err == nil {
		t.Fatal("传输失败必须返回错误")
	}
}

// 同一个 seq 重投:scene 只读答复,不会重办。这条是整条通道「一次且仅一次」的核心,
// 假 scene 这里显式记账,重投时直接回原结局。
func TestCallerReplayOfSameSeqReadsBackOutcome(t *testing.T) {
	var (
		mu      sync.Mutex
		applies int
		seen    = map[uint64]bool{}
	)
	scene, client := startFakeScene(t, func(_ RPC, req *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if !seen[req.GetSeq()] {
			seen[req.GetSeq()] = true
			applies++
		}
		return applied(true), nil
	})
	caller := newTestCaller(t, &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}}, nil)

	req := testRequest()
	for i := 0; i < 3; i++ {
		res, err := caller.Do(context.Background(), RPCDebit, req)
		if err != nil {
			t.Fatalf("第 %d 次 Do 出错: %v", i+1, err)
		}
		if !res.Terminal() {
			t.Fatalf("第 %d 次应当可终结,实际 %+v", i+1, res)
		}
	}
	mu.Lock()
	gotApplies := applies
	mu.Unlock()
	if gotApplies != 1 {
		t.Fatalf("同一个 seq 只应被应用 1 次,实际 %d", gotApplies)
	}
	if got := scene.callCount(); got != 3 {
		t.Fatalf("三次投递应当各发 1 个包,实际 %d", got)
	}
}

// Do 绝不能就地改调用方的请求:Auth 必须写在克隆件上。
func TestCallerDoesNotMutateCallerRequest(t *testing.T) {
	_, client := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		return applied(true), nil
	})
	caller := newTestCaller(t, &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}}, nil)

	req := testRequest()
	if _, err := caller.Do(context.Background(), RPCDebit, req); err != nil {
		t.Fatalf("Do 出错: %v", err)
	}
	if req.GetAuth() != nil {
		t.Fatal("调用方的请求被就地改了:Auth 不该出现在原件上")
	}
}

// 防止有人把 canonical 里的 rpc 名写飘:它同时是签名串字段与指标 label。
func TestRPCNamesMatchCanonicalContract(t *testing.T) {
	want := map[RPC]string{RPCDebit: "debit", RPCCredit: "credit", RPCAbort: "abort_debit"}
	for rpc, name := range want {
		if got := rpc.String(); got != name {
			t.Fatalf("RPC %d 的名字应为 %q,实际 %q", uint8(rpc), name, got)
		}
	}
	if !strings.HasPrefix(CanonicalVersion, "mmorpg-asset-op/") {
		t.Fatalf("canonical 版本行不对: %q", CanonicalVersion)
	}
}
