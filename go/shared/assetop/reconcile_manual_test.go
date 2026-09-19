package assetop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	assetpb "proto/common/asset"
)

// 本文件覆盖两件「循环之外」的事:租约被别的副本接管时重排落空要看得见,
// 以及人工终结必须能在没有 scene、没有 Applier 的工具里用。

// leaseLostStore 的 Reschedule 永远 CAS 落空:租约在处理期间被另一个副本接管了。
type leaseLostStore struct{ *fakeStore }

func (s leaseLostStore) Reschedule(ctx context.Context, op Op, nextAttemptMs uint64, res Result, nowMs uint64) error {
	// 仍旧记一笔,便于断言循环确实算出了重排;但照契约回 ErrLeaseLost。
	_ = s.fakeStore.Reschedule(ctx, op, nextAttemptMs, res, nowMs)
	return fmt.Errorf("%w: op_id=%d", ErrLeaseLost, op.OpID)
}

// rescheduleErrStore 的 Reschedule 是真的存储故障,不是租约被接管。
type rescheduleErrStore struct{ *fakeStore }

func (s rescheduleErrStore) Reschedule(context.Context, Op, uint64, Result, uint64) error {
	return errors.New("连接断了")
}

// 租约被接管:不是处理失败,但必须有计数 —— 否则「我的结果被丢弃」在任何地方都看不见。
func TestRescheduleLeaseLostCountedNotFailed(t *testing.T) {
	op := testOp(1)
	store := leaseLostStore{newFakeStore(op)}
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return resultApplied(false), nil // 结局有了但没落盘 → await_durable 重排
	}}
	loop, m := newTestLoop(t, store, applier, nil)

	if _, err := loop.ProcessOne(context.Background(), op); err != nil {
		t.Fatalf("租约被接管不算处理失败,不该返回错误: %v", err)
	}
	if got := testutil.ToFloat64(m.rescheduleLostTotal.WithLabelValues("guild_debit")); got != 1 {
		t.Fatalf("重排落空指标应为 1,实际 %v", got)
	}
	if got := testutil.ToFloat64(m.rescheduleTotal.WithLabelValues("guild_debit", "await_durable")); got != 0 {
		t.Fatalf("没排成的重排不得计入 reschedule_total,实际 %v", got)
	}
	if got := testutil.ToFloat64(m.storeErrorsTotal.WithLabelValues("reschedule")); got != 0 {
		t.Fatalf("租约被接管不是存储故障,不得计入 store_errors,实际 %v", got)
	}
}

// 真的存储故障仍然要当故障报,不能被上一条的宽容顺手吞掉。
func TestRescheduleStoreErrorStillFails(t *testing.T) {
	op := testOp(1)
	store := rescheduleErrStore{newFakeStore(op)}
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return resultApplied(false), nil
	}}
	loop, m := newTestLoop(t, store, applier, nil)

	if _, err := loop.ProcessOne(context.Background(), op); err == nil {
		t.Fatal("存储故障必须报错")
	}
	if got := testutil.ToFloat64(m.storeErrorsTotal.WithLabelValues("reschedule")); got != 1 {
		t.Fatalf("reschedule 出错指标应为 1,实际 %v", got)
	}
	if got := testutil.ToFloat64(m.rescheduleLostTotal.WithLabelValues("guild_debit")); got != 0 {
		t.Fatalf("普通故障不得计成租约被接管,实际 %v", got)
	}
}

// fakeManual 是人工终结通道的替身。
type fakeManual struct {
	calls    int
	last     ManualResolution
	lastNow  uint64
	resolved bool
	err      error
}

func (f *fakeManual) ResolveManually(_ context.Context, r ManualResolution, nowMs uint64) (bool, error) {
	f.calls++
	f.last = r
	f.lastNow = nowMs
	return f.resolved, f.err
}

func validResolution() ManualResolution {
	return ManualResolution{
		OpID:     7,
		Final:    StatusApplied,
		Operator: "ops-li",
		Reason:   "scene 流水已见 correlation_id=7",
	}
}

// assetopfix 这类工具不连 scene:没有 Applier、没有 LoopConfig,也要能人工终结一行。
func TestResolveManuallyWithoutLoop(t *testing.T) {
	manual := &fakeManual{resolved: true}

	// 指标传 nil:CLI 不接注册表,这条路径必须能走通。
	resolved, err := ResolveManually(context.Background(), manual, validResolution(), nil, fixedNow)
	if err != nil {
		t.Fatalf("人工终结出错: %v", err)
	}
	if !resolved {
		t.Fatal("Store 说终结成功,函数就该回 true")
	}
	if manual.calls != 1 || manual.last.OpID != 7 || manual.last.Final != StatusApplied {
		t.Fatalf("落库入参不对: calls=%d last=%+v", manual.calls, manual.last)
	}
	if manual.lastNow != testNowMs {
		t.Fatalf("应当用注入的时钟,实际 %d", manual.lastNow)
	}
}

// 没有人工通道时必须报错:以为自己终结了一行、实际什么也没做,是钱路径上最坏的"成功"。
func TestResolveManuallyRejectsNilResolver(t *testing.T) {
	if _, err := ResolveManually(context.Background(), nil, validResolution(), nil, nil); err == nil {
		t.Fatal("没有人工终结通道时必须报错")
	}
}

// 校验不过就绝不碰库。
func TestResolveManuallyRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		build func(*ManualResolution)
	}{
		{"状态不是终局", func(r *ManualResolution) { r.Final = StatusPending }},
		{"状态越界", func(r *ManualResolution) { r.Final = Status(99) }},
		{"没有操作人", func(r *ManualResolution) { r.Operator = "" }},
		{"操作人超长", func(r *ManualResolution) { r.Operator = strings.Repeat("操", 65) }},
		{"理由超长", func(r *ManualResolution) { r.Reason = strings.Repeat("字", 192) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := validResolution()
			c.build(&r)
			manual := &fakeManual{resolved: true}
			if _, err := ResolveManually(context.Background(), manual, r, nil, fixedNow); err == nil {
				t.Fatal("非法入参必须被拒")
			}
			if manual.calls != 0 {
				t.Fatalf("校验不过不得落库,实际调了 %d 次", manual.calls)
			}
		})
	}
}

// Loop 上的方法只是薄委托:校验、计数、留痕一样不少。
func TestLoopResolveManuallyDelegates(t *testing.T) {
	store := newFakeStore()
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return Result{}, nil }}
	loop, m := newTestLoop(t, store, applier, nil)
	manual := &fakeManual{resolved: true}
	loop.Manual = manual

	r := validResolution()
	r.Final = StatusRejected
	resolved, err := loop.ResolveManually(context.Background(), r)
	if err != nil {
		t.Fatalf("人工终结出错: %v", err)
	}
	if !resolved || manual.calls != 1 {
		t.Fatalf("应当落库一次并回 true,实际 resolved=%t calls=%d", resolved, manual.calls)
	}
	if manual.lastNow != testNowMs {
		t.Fatalf("应当用 Loop 的时钟,实际 %d", manual.lastNow)
	}
	if got := testutil.ToFloat64(m.manualResolveTotal.WithLabelValues("rejected")); got != 1 {
		t.Fatalf("人工终结指标应为 1,实际 %v", got)
	}
}

// 没挂人工通道的 Loop 同样要报错,不能静默当成终结了。
func TestLoopResolveManuallyWithoutChannel(t *testing.T) {
	store := newFakeStore()
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return Result{}, nil }}
	loop, _ := newTestLoop(t, store, applier, nil)

	if _, err := loop.ResolveManually(context.Background(), validResolution()); err == nil {
		t.Fatal("没有人工终结通道时必须报错")
	}
}
