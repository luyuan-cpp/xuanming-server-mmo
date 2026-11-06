package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"trade/internal/commerce"
	"trade/internal/data"
)

// fakeStore 回预设的结果,记下 Resolve 的调用。
type fakeStore struct {
	record     *tradepb.TradeCommerceRecord
	resolveErr error
	result     commerce.ManualResult
	calls      []bool
}

func (f *fakeStore) ListManual(context.Context, int) ([]*tradepb.TradeCommerceRecord, error) {
	return []*tradepb.TradeCommerceRecord{f.record}, nil
}

func (f *fakeStore) Get(context.Context, uint64) (*tradepb.TradeCommerceRecord, error) {
	if f.record == nil {
		return nil, errors.New("not found")
	}
	return f.record, nil
}

func (f *fakeStore) LegOutcome(context.Context, uint64) (data.LegOutcome, error) {
	return data.LegOutcome{Status: tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED}, nil
}

func (f *fakeStore) Resolve(_ context.Context, _ commerce.ManualRequest, execute bool) (commerce.ManualResult, error) {
	f.calls = append(f.calls, execute)
	return f.result, f.resolveErr
}

func currency(amount uint64) *assetpb.AssetBundle {
	return &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: amount}}}
}

// manualRecord 是一份转人工的寄售:原阶段 pay,两条托管都已扣到。
func manualRecord(t *testing.T) *tradepb.TradeCommerceRecord {
	t.Helper()
	w := &tradepb.TradeCommerceWorkflow{
		Stage:      "manual",
		Quarantine: &tradepb.TradeCommerceQuarantine{Stage: "pay", Status: commerce.Escrowing, Reason: "corrupt", Detail: "坏数据"},
		Legs: []*tradepb.TradeCommerceLeg{
			{Role: "seller_debit", PlayerId: 1, Stream: 3, OpId: 11, Outcome: 2, Bundle: currency(10)},
			{Role: "buyer_debit_1_k", PlayerId: 2, Stream: 3, OpId: 12, Bundle: currency(30)},
		},
	}
	raw, err := proto.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return &tradepb.TradeCommerceRecord{CommerceId: 42, Kind: 3, Status: commerce.Manual, SellerPlayerId: 1, BuyerPlayerId: 2, Version: 7, WorkflowProto: raw}
}

func resolveOpts(execute bool) resolveOptions {
	return resolveOptions{
		req:     commerce.ManualRequest{CommerceID: 42, ExpectVersion: 7, Action: commerce.ManualRefund, Operator: "ops", Reason: "ticket-1"},
		execute: execute,
	}
}

func TestRunResolve_ExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "成功", want: exitOK},
		{name: "版本已变", err: fmt.Errorf("%w(现在是 8)", commerce.ErrManualVersionChanged), want: exitVersionChanged},
		{name: "前置条件", err: fmt.Errorf("%w: 已有发放到账", commerce.ErrManualNotAllowed), want: exitRejected},
		{name: "合同不存在", err: commerce.ErrManualNotFound, want: exitRejected},
		{name: "存储错误", err: errors.New("boom"), want: exitError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{resolveErr: c.err}
			var stdout, stderr bytes.Buffer
			if got := runResolve(context.Background(), store, resolveOpts(true), &stdout, &stderr); got != c.want {
				t.Fatalf("exit=%d want=%d stderr=%s", got, c.want, stderr.String())
			}
		})
	}
}

func TestRunResolve_PlanOnlyByDefault(t *testing.T) {
	store := &fakeStore{result: commerce.ManualResult{VersionBefore: 7, Stage: "return", NewLegs: []string{"seller_debit/refund"}}}
	var stdout, stderr bytes.Buffer
	if got := runResolve(context.Background(), store, resolveOpts(false), &stdout, &stderr); got != exitOK {
		t.Fatalf("exit=%d", got)
	}
	if len(store.calls) != 1 || store.calls[0] {
		t.Fatalf("不带 -execute 只算计划: calls=%v", store.calls)
	}
	out := stdout.String()
	if !strings.Contains(out, "[CommerceManual] operator=ops commerce_id=42 action=refund") || !strings.Contains(out, "executed=false") ||
		!strings.Contains(out, "Plan only") {
		t.Fatalf("stdout=%s", out)
	}
}

func TestParseResolve(t *testing.T) {
	valid := []string{"-commerce", "42", "-action", "reissue", "-expect-version", "7", "-operator", "ops", "-reason", "ticket-1", "-confirm", "42"}
	var stderr bytes.Buffer
	opts, ok := parseResolve(append(valid, "-execute"), &stderr)
	if !ok || !opts.execute || opts.req.Action != commerce.ManualReissue || opts.req.ExpectVersion != 7 {
		t.Fatalf("opts=%+v ok=%v stderr=%s", opts, ok, stderr.String())
	}
	if opts, ok := parseResolve(valid, &stderr); !ok || opts.execute {
		t.Fatal("默认不写库")
	}
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
		"缺合同":    replace("-commerce", "0"),
		"未知动作":   replace("-action", "delete"),
		"缺版本":    replace("-expect-version", "0"),
		"操作人带换行": replace("-operator", "a\nb"),
		"缺依据":    replace("-reason", " "),
		"二次确认不符": replace("-confirm", "43"),
		"多余参数":   append(append([]string(nil), valid...), "extra"),
	} {
		if _, ok := parseResolve(args, &stderr); ok {
			t.Errorf("%s 应该被拒绝", name)
		}
	}
}

func TestRunShow_ListsEveryActionVerdict(t *testing.T) {
	store := &fakeStore{record: manualRecord(t)}
	var stdout, stderr bytes.Buffer
	if got := runShow(context.Background(), store, 42, &stdout, &stderr); got != exitOK {
		t.Fatalf("exit=%d stderr=%s", got, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"quarantine: stage=pay", "resume: yes", "reissue: no", "refund: yes", "buyer_debit_1_k/refund"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show 少了 %q:\n%s", want, out)
		}
	}
}

func TestRunList_PrintsQuarantineColumns(t *testing.T) {
	store := &fakeStore{record: manualRecord(t)}
	var stdout, stderr bytes.Buffer
	if got := runList(context.Background(), store, 10, &stdout, &stderr); got != exitOK {
		t.Fatalf("exit=%d", got)
	}
	if !strings.Contains(stdout.String(), "42") || !strings.Contains(stdout.String(), "corrupt") {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestRun_RejectsBadArgsBeforeTouchingConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{nil, {"nope"}, {"-f", "missing.yaml", "show"}, {"-f", "missing.yaml", "resolve", "-commerce", "1"}, {"list", "-limit", "0"}} {
		if got := run(args, &stdout, &stderr); got != exitRejected {
			t.Errorf("%v: exit=%d", args, got)
		}
	}
}
