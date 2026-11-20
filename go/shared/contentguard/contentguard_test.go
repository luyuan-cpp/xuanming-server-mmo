package contentguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"

	cgpb "proto/content_guard"
)

// fakeRPC 是 ContentGuardClient 的替身:记下收到的请求,按预设回包。
type fakeRPC struct {
	out   *cgpb.CheckTextResponse
	err   error
	delay time.Duration
	calls []*cgpb.CheckTextRequest
}

func (f *fakeRPC) CheckText(ctx context.Context, in *cgpb.CheckTextRequest, _ ...grpc.CallOption) (*cgpb.CheckTextResponse, error) {
	f.calls = append(f.calls, in)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.out, f.err
}

func TestFromResponseOnlyTrustsPassAndMasked(t *testing.T) {
	cases := []struct {
		name string
		out  *cgpb.CheckTextResponse
		want Result
	}{
		{"pass", &cgpb.CheckTextResponse{Verdict: cgpb.TextVerdict_TEXT_VERDICT_PASS}, Result{Verdict: Pass}},
		{"masked", &cgpb.CheckTextResponse{Verdict: cgpb.TextVerdict_TEXT_VERDICT_MASKED, MaskedText: "你个**", Category: cgpb.TextCategory_TEXT_CATEGORY_ABUSE},
			Result{Verdict: Masked, Masked: "你个**", Category: cgpb.TextCategory_TEXT_CATEGORY_ABUSE}},
		{"masked without text is a defect, not a pass", &cgpb.CheckTextResponse{Verdict: cgpb.TextVerdict_TEXT_VERDICT_MASKED}, Result{Verdict: Reject}},
		{"reject", &cgpb.CheckTextResponse{Verdict: cgpb.TextVerdict_TEXT_VERDICT_REJECT, Category: cgpb.TextCategory_TEXT_CATEGORY_AD},
			Result{Verdict: Reject, Category: cgpb.TextCategory_TEXT_CATEGORY_AD}},
		{"unspecified", &cgpb.CheckTextResponse{}, Result{Verdict: Reject}},
		{"a verdict added later", &cgpb.CheckTextResponse{Verdict: cgpb.TextVerdict(99)}, Result{Verdict: Reject}},
		{"nil response", nil, Result{Verdict: Reject}},
	}
	for _, c := range cases {
		if got := FromResponse(c.out); got != c.want {
			t.Errorf("%s: FromResponse = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestCheckSendsSceneTextAndPlayer(t *testing.T) {
	rpc := &fakeRPC{out: &cgpb.CheckTextResponse{Verdict: cgpb.TextVerdict_TEXT_VERDICT_PASS}}
	client := NewWithClient(rpc, time.Second)
	got := client.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_GUILD_NAME, 42, "逍遥帮")
	if got.Verdict != Pass || !got.Usable() {
		t.Fatalf("result = %+v", got)
	}
	if len(rpc.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(rpc.calls))
	}
	call := rpc.calls[0]
	if call.GetScene() != cgpb.TextScene_TEXT_SCENE_GUILD_NAME || call.GetText() != "逍遥帮" || call.GetPlayerId() != 42 {
		t.Fatalf("request = %+v", call)
	}
}

// Optional(本机开发)的客户端在服务不可达时放行;服务回了明确的拒绝仍然照拒。
func TestOptionalClientPassesOnlyWhenTheGuardIsUnreachable(t *testing.T) {
	unreachable := &Client{rpc: &fakeRPC{err: errors.New("connection refused")}, timeout: time.Second, optional: true}
	if got := unreachable.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, 1, "hello"); got.Verdict != Pass {
		t.Fatalf("optional + unreachable: %+v, want Pass", got)
	}
	rejecting := &Client{rpc: &fakeRPC{out: &cgpb.CheckTextResponse{Verdict: cgpb.TextVerdict_TEXT_VERDICT_REJECT}}, timeout: time.Second, optional: true}
	if got := rejecting.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, 1, "hello"); got.Verdict != Reject {
		t.Fatalf("optional + explicit reject: %+v, want Reject", got)
	}
}

func TestCheckFailsClosedOnErrorsAndTimeouts(t *testing.T) {
	failing := NewWithClient(&fakeRPC{err: errors.New("connection refused")}, time.Second)
	if got := failing.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, 1, "hello"); got.Verdict != Unavailable || got.Usable() {
		t.Fatalf("rpc error: %+v, want Unavailable", got)
	}
	slow := NewWithClient(&fakeRPC{out: &cgpb.CheckTextResponse{Verdict: cgpb.TextVerdict_TEXT_VERDICT_PASS}, delay: time.Second}, 20*time.Millisecond)
	started := time.Now()
	if got := slow.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, 1, "hello"); got.Verdict != Unavailable {
		t.Fatalf("timeout: %+v, want Unavailable", got)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Check did not honour its own timeout: took %v", elapsed)
	}
}

func TestEmptyTextAndDisabledClientPassWithoutCalling(t *testing.T) {
	rpc := &fakeRPC{err: errors.New("must not be called")}
	enabled := NewWithClient(rpc, time.Second)
	if got := enabled.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 1, ""); got.Verdict != Pass {
		t.Fatalf("empty text: %+v", got)
	}
	if len(rpc.calls) != 0 {
		t.Fatalf("empty text must not be sent")
	}
	disabled := &Client{}
	if disabled.Enabled() {
		t.Fatalf("a client without a target must report disabled")
	}
	if got := disabled.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 1, "anything"); got.Verdict != Pass {
		t.Fatalf("disabled client: %+v", got)
	}
	var missing *Client
	if got := missing.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 1, "anything"); got.Verdict != Pass {
		t.Fatalf("nil client: %+v", got)
	}
}

func TestFuncIsAChecker(t *testing.T) {
	var checker Checker = Func(func(_ context.Context, scene cgpb.TextScene, _ uint64, text string) Result {
		if scene == cgpb.TextScene_TEXT_SCENE_ROLE_NAME && text == "bad" {
			return Result{Verdict: Reject}
		}
		return Result{Verdict: Pass}
	})
	if checker.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 0, "bad").Usable() {
		t.Fatalf("Func must delegate to the function")
	}
}

func TestConfValidate(t *testing.T) {
	configured := Conf{Rpc: zrpc.RpcClientConf{Endpoints: []string{"127.0.0.1:50250"}}, TimeoutMs: 300}
	if !configured.Configured() || configured.Validate() != nil {
		t.Fatalf("a conf with endpoints must be configured and valid: %v", configured.Validate())
	}
	if (Conf{}).Configured() {
		t.Fatalf("empty conf must not count as configured")
	}
	// 缺省严格:没配目标就是配置错误;只有显式 Optional 才允许不配。
	if err := (Conf{}).Validate(); err == nil {
		t.Fatalf("a conf without a target must be rejected unless Optional is set")
	}
	if err := (Conf{Optional: true}).Validate(); err != nil {
		t.Fatalf("Optional without a target is valid for local development: %v", err)
	}
	for _, timeout := range []int64{0, 19, 5001} {
		bad := configured
		bad.TimeoutMs = timeout
		if err := bad.Validate(); err == nil {
			t.Errorf("TimeoutMs=%d must be rejected", timeout)
		}
	}
	// 只写了 etcd 地址、没写发现键,不算配了目标。
	onlyHosts := Conf{}
	onlyHosts.Rpc.Etcd.Hosts = []string{"127.0.0.1:2379"}
	if onlyHosts.Configured() {
		t.Fatalf("etcd hosts without a key must not count as configured")
	}
	onlyHosts.Rpc.Etcd.Key = "contentguard.rpc"
	if !onlyHosts.Configured() {
		t.Fatalf("etcd hosts with a key must count as configured")
	}
}

func TestNewWithoutTargetNeedsOptional(t *testing.T) {
	client, err := New("test", Conf{Optional: true})
	if err != nil || client == nil || client.Enabled() {
		t.Fatalf("New with Optional and no target: client=%v err=%v", client, err)
	}
	if _, err = New("test", Conf{}); err == nil {
		t.Fatalf("New must refuse a conf without a target unless Optional is set")
	}
}
