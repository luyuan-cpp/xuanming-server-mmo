package assetop

import (
	"errors"
	"fmt"
	"testing"
	"time"

	assetpb "proto/common/asset"
)

func TestDecideTable(t *testing.T) {
	transportErr := errors.New("connection refused")

	cases := []struct {
		name string
		res  Result
		err  error
		want Action
	}{
		{"传输失败 → 重试", Result{}, transportErr, ActionRetry},
		{"结局翻转 → 告警", Result{}, fmt.Errorf("包一层: %w", ErrOutcomeFlip), ActionAlert},
		{"UNKNOWN → 告警", Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN}, nil, ActionAlert},
		{"APPLIED 且 durable → 终结", Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}, nil, ActionFinalize},
		{"APPLIED 未 durable → 等落盘", Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED}, nil, ActionAwaitDurable},
		{"REJECTED 且 durable → 终结", Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}, nil, ActionFinalize},
		{"REJECTED 未 durable → 等落盘", Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED}, nil, ActionAwaitDurable},
		{"RETRY → 重试", Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, Reason: ReasonInBattle}, nil, ActionRetry},
		{"NOT_HERE → 重试", Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE}, nil, ActionRetry},
		{"本地合成的 NOT_HERE → 重试", Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, nil, ActionRetry},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.res, c.err); got != c.want {
				t.Fatalf("Decide = %s,期望 %s", got, c.want)
			}
		})
	}
}

// durable 是终结的硬前置:REJECTED 也不例外(比契约 §3.3 更严,理由见规格 C9)。
func TestTerminalRequiresDurable(t *testing.T) {
	for _, outcome := range []assetpb.AssetOpOutcome{
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED,
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED,
	} {
		if (Result{Outcome: outcome}).Terminal() {
			t.Fatalf("%v 未 durable 时不得可终结", outcome)
		}
		if !(Result{Outcome: outcome, Durable: true}).Terminal() {
			t.Fatalf("%v 且 durable 时应当可终结", outcome)
		}
	}
	for _, outcome := range []assetpb.AssetOpOutcome{
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN,
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY,
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE,
	} {
		if (Result{Outcome: outcome, Durable: true}).Terminal() {
			t.Fatalf("%v 永不可终结,哪怕 durable", outcome)
		}
	}
}

func TestFinalStatus(t *testing.T) {
	appliedRes := Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
	rejectedRes := func(reason uint32) Result {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Reason: reason, Durable: true}
	}

	cases := []struct {
		name string
		rpc  RPC
		res  Result
		op   Op
		want Status
	}{
		{"扣除成功", RPCDebit, appliedRes, Op{}, StatusApplied},
		{"中止占位(reason 0)", RPCAbort, rejectedRes(0), Op{}, StatusAborted},
		{"中止时才发现余额不足", RPCAbort, rejectedRes(ReasonCurrencyInsufficient), Op{}, StatusRejected},
		{"发放被拒", RPCCredit, rejectedRes(ReasonBlocked), Op{}, StatusRejected},
		{"扣除被拒 reason 0 也算业务拒绝", RPCDebit, rejectedRes(0), Op{}, StatusRejected},
		{"本次答复带 partial", RPCCredit, Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true, Partial: true}, Op{}, StatusAppliedPartial},
		{"只读答复不带 partial,靠行上的 last_reason", RPCCredit, appliedRes, Op{LastReason: ReasonPartialApplied}, StatusAppliedPartial},
		{"非终结结局算不出状态", RPCDebit, Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY}, Op{}, StatusPending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FinalStatus(c.rpc, c.res, c.op); got != c.want {
				t.Fatalf("FinalStatus = %s,期望 %s", got, c.want)
			}
		})
	}
}

func TestNextAttemptMs(t *testing.T) {
	const now = 1_000_000

	// attempts=3、base=1s → 8s,再乘 [0.8, 1.2)。
	for _, jitter := range []float64{0, 0.5, 0.999999} {
		got := NextAttemptMs(now, 3, time.Second, time.Minute, func() float64 { return jitter })
		delay := got - now
		if delay < 6400 || delay > 9600 {
			t.Fatalf("jitter=%v 时退避 %dms 不在 [6400, 9600]", jitter, delay)
		}
	}

	// attempts 很大时封顶在 MaxBackoff,再乘抖动。
	got := NextAttemptMs(now, 20, time.Second, time.Minute, func() float64 { return 0 })
	if delay := got - now; delay != 48000 {
		t.Fatalf("封顶后 jitter=0 应为 48000ms,实际 %d", delay)
	}
	got = NextAttemptMs(now, 20, time.Second, time.Minute, func() float64 { return 0.999999 })
	if delay := got - now; delay < 48000 || delay > 72000 {
		t.Fatalf("封顶后退避 %dms 不在 [48000, 72000]", delay)
	}

	// 不注入随机源时取中值:结果确定,便于排障复现。
	mid := NextAttemptMs(now, 0, time.Second, time.Minute, nil)
	if delay := mid - now; delay != 1000 {
		t.Fatalf("attempts=0、无抖动时应为 1000ms,实际 %d", delay)
	}

	// 越界的随机实现不得把退避拉飞。
	wild := NextAttemptMs(now, 0, time.Second, time.Minute, func() float64 { return 42 })
	if delay := wild - now; delay < 800 || delay > 1200 {
		t.Fatalf("越界 jitter 应被夹回,实际 %dms", delay)
	}
}

// 状态与动作的名字会进指标 label,写飘了告警规则就全废。
func TestStatusAndActionNames(t *testing.T) {
	if got := StatusAppliedPartial.String(); got != "applied_partial" {
		t.Fatalf("StatusAppliedPartial 名字错: %q", got)
	}
	if got := Status(0).String(); got != "invalid" {
		t.Fatalf("零值状态应为 invalid,实际 %q", got)
	}
	if got := ActionAwaitDurable.String(); got != "await_durable" {
		t.Fatalf("ActionAwaitDurable 名字错: %q", got)
	}
	if got := StreamLabel(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT); got != "trade_credit" {
		t.Fatalf("流 label 错: %q", got)
	}
	if got := StreamLabel(assetpb.AssetOpStream(99)); got != "other" {
		t.Fatalf("越界流号必须归到 other,实际 %q", got)
	}
}

func TestApplyRPCOf(t *testing.T) {
	cases := map[assetpb.AssetOpStream]RPC{
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT:   RPCDebit,
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT:  RPCCredit,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT:   RPCDebit,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT:  RPCCredit,
		assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT: RPCCredit,
	}
	for stream, want := range cases {
		got, ok := ApplyRPCOf(stream)
		if !ok || got != want {
			t.Fatalf("流 %v 应当映射到 %s(ok=%v),实际 %s", stream, want, ok, got)
		}
	}
	if _, ok := ApplyRPCOf(assetpb.AssetOpStream_ASSET_OP_STREAM_UNSPECIFIED); ok {
		t.Fatal("未指定的流不得给出投递方向")
	}
}
