package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"shared/serviceauth"
	"trade/internal/commerce"
	"trade/internal/config"
)

const testSecret = "commercefix-test-secret-0123456789abcdef"

// fakeClient 回预设的应答,记下收到的请求。
type fakeClient struct {
	list        *tradepb.ListManualContractsResponse
	resolve     *tradepb.ResolveManualContractResponse
	err         error
	listCalls   []*tradepb.ListManualContractsRequest
	resolveCall []*tradepb.ResolveManualContractRequest
}

func (f *fakeClient) ListManualContracts(_ context.Context, in *tradepb.ListManualContractsRequest, _ ...grpc.CallOption) (*tradepb.ListManualContractsResponse, error) {
	f.listCalls = append(f.listCalls, in)
	return f.list, f.err
}

func (f *fakeClient) ResolveManualContract(_ context.Context, in *tradepb.ResolveManualContractRequest, _ ...grpc.CallOption) (*tradepb.ResolveManualContractResponse, error) {
	f.resolveCall = append(f.resolveCall, in)
	return f.resolve, f.err
}

func testDeps(client *fakeClient, dialed *int) deps {
	return deps{
		dial: func(string) (adminClient, func(), error) {
			*dialed++
			return client, func() {}, nil
		},
		getenv: func(name string) string {
			if name == defaultSecretEnv {
				return testSecret
			}
			return ""
		},
		now: time.Now,
	}
}

func runWith(t *testing.T, client *fakeClient, args ...string) (int, string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	dialed := 0
	code := run(args, &stdout, &stderr, testDeps(client, &dialed))
	return code, stdout.String(), stderr.String(), dialed
}

func manualWorkflowRecord(t *testing.T) *tradepb.TradeCommerceRecord {
	t.Helper()
	w := &tradepb.TradeCommerceWorkflow{
		Stage:      "manual",
		Quarantine: &tradepb.TradeCommerceQuarantine{Stage: "deliver", Status: commerce.Delivering, Reason: "applied_partial", Detail: "buyer_receive partial"},
		Legs: []*tradepb.TradeCommerceLeg{
			{Role: "buyer_receive", PlayerId: 2, Stream: 4, OpId: 12, Bundle: &assetpb.AssetBundle{
				Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 2}}, Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 30}}}},
		},
	}
	raw, err := proto.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return &tradepb.TradeCommerceRecord{CommerceId: 42, Kind: 2, Status: commerce.Manual, SellerPlayerId: 1, BuyerPlayerId: 2, Version: 7, WorkflowProto: raw}
}

// 参数写错不签名、不拨号;缺端点、未知子命令、resolve 缺参数一律退出码 2。
func TestRunRejectsBadArgsBeforeDialing(t *testing.T) {
	valid := []string{"-endpoint", "trade:1", "resolve", "-commerce", "42", "-action", "refund", "-expect-version", "7", "-expect-target", "staging-global",
		"-operator", "ops", "-reason", "ticket-1", "-confirm", "42"}
	replace := func(flag, value string) []string {
		out := append([]string(nil), valid...)
		for i := range out {
			if out[i] == flag {
				out[i+1] = value
			}
		}
		return out
	}
	for name, args := range map[string][]string{
		"没有参数":                  nil,
		"缺端点":                   {"list"},
		"未知子命令":                 {"-endpoint", "trade:1", "nope"},
		"list limit 越界":         {"-endpoint", "trade:1", "list", "-limit", "0"},
		"show 缺合同":              {"-endpoint", "trade:1", "show"},
		"缺合同":                   replace("-commerce", "0"),
		"未知动作":                  replace("-action", "delete"),
		"缺版本":                   replace("-expect-version", "0"),
		"缺目标环境":                 replace("-expect-target", " "),
		"操作人带换行":                replace("-operator", "a\nb"),
		"缺依据":                   replace("-reason", " "),
		"二次确认不符":                replace("-confirm", "43"),
		"多余参数":                  append(append([]string(nil), valid...), "extra"),
		"refund 带 -part":        append(append([]string(nil), valid...), "-part", "a=pet"),
		"partial 缺 -part":       replace("-action", "partial"),
		"partial 缺口写错":          append(replace("-action", "partial"), "-part", "a=item:10:2"),
		"redirect 缺 -to":        append(replace("-action", "redirect"), "-party", "seller"),
		"redirect 一方不对":         append(replace("-action", "redirect"), "-party", "both", "-to", "9"),
		"void-auction 缺出价版本":    append(replace("-action", "void-auction"), "-linked", "8"),
		"refund 带 -linked":      append(append([]string(nil), valid...), "-linked", "8", "-linked-version", "3"),
	} {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{}
			code, _, _, dialed := runWith(t, client, args...)
			if code != exitRejected || dialed != 0 {
				t.Fatalf("exit=%d dialed=%d", code, dialed)
			}
		})
	}
}

// 签名先于拨号:密钥变量缺失或太短直接拒绝,不碰服务。
func TestRunRequiresTheSigningSecret(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dialed := 0
	d := testDeps(&fakeClient{}, &dialed)
	d.getenv = func(string) string { return "short" }
	code := run([]string{"-endpoint", "trade:1", "show", "-commerce", "42"}, &stdout, &stderr, d)
	if code != exitRejected || dialed != 0 {
		t.Fatalf("exit=%d dialed=%d", code, dialed)
	}
	if strings.Contains(stderr.String(), "short") || !strings.Contains(stderr.String(), defaultSecretEnv) {
		t.Fatalf("只报变量名,不报值: %s", stderr.String())
	}
}

// 请求带着 ops-commerce 的签名,服务端拿同一把密钥验得过。
func TestRunSignsTheRequest(t *testing.T) {
	client := &fakeClient{resolve: &tradepb.ResolveManualContractResponse{Verdict: tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_NOT_FOUND}}
	code, _, _, _ := runWith(t, client, "-endpoint", "trade:1", "show", "-commerce", "42")
	if code != exitRejected || len(client.resolveCall) != 1 {
		t.Fatalf("exit=%d calls=%d", code, len(client.resolveCall))
	}
	verifier, err := serviceauth.New(map[string]string{config.ManualResolutionCaller: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := verifier.Verify(context.Background(), tradepb.TradeAdmin_ResolveManualContract_FullMethodName, client.resolveCall[0])
	if err != nil || caller != config.ManualResolutionCaller {
		t.Fatalf("caller=%q err=%v", caller, err)
	}
}

func resolveArgs(extra ...string) []string {
	args := []string{"-endpoint", "trade:1", "resolve", "-commerce", "42", "-action", "partial", "-expect-version", "7", "-expect-target", "staging-global",
		"-operator", "ops", "-reason", "ticket-1", "-confirm", "42", "-part", "buyer_receive=currency:1", "-part", "seller_receive=pet"}
	return append(args, extra...)
}

// 参数原样进请求:-part 可重复,默认 DRY_RUN,加 -execute 才是 EXECUTE。
func TestResolveBuildsTheRequest(t *testing.T) {
	client := &fakeClient{resolve: &tradepb.ResolveManualContractResponse{Verdict: tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK}}
	code, out, _, _ := runWith(t, client, resolveArgs()...)
	if code != exitOK {
		t.Fatalf("exit=%d", code)
	}
	in := client.resolveCall[0]
	if in.GetMode() != tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_DRY_RUN || in.GetAction() != "partial" || in.GetExpectTarget() != "staging-global" ||
		strings.Join(in.GetParts(), ";") != "buyer_receive=currency:1;seller_receive=pet" {
		t.Fatalf("request=%v", in)
	}
	if !strings.Contains(out, "Plan only") {
		t.Fatalf("stdout=%s", out)
	}

	client = &fakeClient{resolve: &tradepb.ResolveManualContractResponse{Verdict: tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK}}
	code, out, _, _ = runWith(t, client, resolveArgs("-execute")...)
	if code != exitOK || client.resolveCall[0].GetMode() != tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE || !strings.Contains(out, "updated") {
		t.Fatalf("exit=%d mode=%v stdout=%s", code, client.resolveCall[0].GetMode(), out)
	}

	client = &fakeClient{resolve: &tradepb.ResolveManualContractResponse{Verdict: tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK}}
	code, _, _, _ = runWith(t, client, "-endpoint", "trade:1", "resolve", "-commerce", "42", "-action", "redirect", "-expect-version", "7",
		"-expect-target", "staging-global", "-operator", "ops", "-reason", "ticket-1", "-confirm", "42", "-party", "seller", "-to", "9")
	if code != exitOK || client.resolveCall[0].GetParty() != "seller" || client.resolveCall[0].GetToPlayerId() != 9 {
		t.Fatalf("exit=%d request=%v", code, client.resolveCall[0])
	}
}

// 带内结论与 gRPC 错误映射成退出码;审计行原样打到 stdout。
func TestResolveExitCodes(t *testing.T) {
	cases := []struct {
		name    string
		verdict tradepb.ManualContractVerdict
		err     error
		want    int
	}{
		{"成功", tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK, nil, exitOK},
		{"拒绝", tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED, nil, exitRejected},
		{"不存在", tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_NOT_FOUND, nil, exitRejected},
		{"版本已变", tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_VERSION_CHANGED, nil, exitVersionChanged},
		{"稍后重试", tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_RETRY, nil, exitError},
		{"入口关闭", 0, status.Error(codes.PermissionDenied, "disabled"), exitRejected},
		{"提交结果未知", 0, status.Error(codes.Unavailable, "commit outcome unknown"), exitError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := &fakeClient{err: c.err, resolve: &tradepb.ResolveManualContractResponse{Verdict: c.verdict, AuditLine: "[CommerceManual] operator=\"ops\""}}
			code, out, _, _ := runWith(t, client, resolveArgs("-execute")...)
			if code != c.want {
				t.Fatalf("exit=%d want=%d", code, c.want)
			}
			if c.err == nil && !strings.Contains(out, "[CommerceManual]") {
				t.Fatalf("审计行要打到 stdout: %s", out)
			}
		})
	}
}

// show 打印转人工记录、腿(带资产摘要)、两方归属、关联合同与每种动作能否处理。
func TestShowPrintsTheReport(t *testing.T) {
	client := &fakeClient{resolve: &tradepb.ResolveManualContractResponse{
		Verdict: tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK,
		Target:  "staging-global",
		Record:  manualWorkflowRecord(t),
		Legs: []*tradepb.ManualContractLeg{{Leg: &tradepb.TradeCommerceLeg{Role: "buyer_receive", PlayerId: 2, Stream: 4, OpId: 12,
			Bundle: &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 2}}, Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 30}}}},
			OpStatus: tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL, FinalMs: 99}},
		Parties: []*tradepb.ManualContractParty{{Party: "seller", PlayerId: 1, ContractOwnerRevision: 1, CurrentOwnerRevision: 2, Changed: true}},
		Actions: []*tradepb.ManualContractActionVerdict{{Action: "partial", Allowed: true, Detail: "legs buyer_receive"}, {Action: "refund", Detail: "already delivered"}},
	}}
	code, out, errOut, _ := runWith(t, client, "-endpoint", "trade:1", "show", "-commerce", "42")
	if code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"target=staging-global", "quarantine: stage=deliver", "reason=applied_partial", "APPLIED_PARTIAL",
		"item:10x2,currency:1=30", "seller", "partial: yes", "refund: no"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show 少了 %q:\n%s", want, out)
		}
	}
}

// list 打印转人工原因与下一页游标。
func TestListPrintsQuarantineAndCursor(t *testing.T) {
	client := &fakeClient{list: &tradepb.ListManualContractsResponse{Target: "staging-global", Contracts: []*tradepb.TradeCommerceRecord{manualWorkflowRecord(t)},
		NextAfterUpdatedMs: 5, NextAfterCommerceId: 42}}
	code, out, _, _ := runWith(t, client, "-endpoint", "trade:1", "list", "-limit", "1")
	if code != exitOK {
		t.Fatalf("exit=%d", code)
	}
	for _, want := range []string{"42", "applied_partial", "next page: list -after-updated 5 -after-commerce 42"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list 少了 %q:\n%s", want, out)
		}
	}
	if client.listCalls[0].GetLimit() != 1 {
		t.Fatalf("limit=%d", client.listCalls[0].GetLimit())
	}

	client = &fakeClient{list: &tradepb.ListManualContractsResponse{Retry: true}}
	if code, _, _, _ := runWith(t, client, "-endpoint", "trade:1", "list"); code != exitError {
		t.Fatalf("读库失败应当退出码 1: %d", code)
	}
}
