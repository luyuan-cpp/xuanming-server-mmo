package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"shared/assetop"
)

const testOpID uint64 = 70001

// fakeStore 记下人工终结的调用。resolved 决定 CAS 结果。
type fakeStore struct {
	row      *tradepb.TradeAssetOpRecord
	readErr  error
	resolved bool
	calls    []assetop.ManualResolution
}

func (f *fakeStore) GetOp(context.Context, uint64) (*tradepb.TradeAssetOpRecord, bool, error) {
	if f.readErr != nil {
		return nil, false, f.readErr
	}
	return f.row, f.row != nil, nil
}

func (f *fakeStore) ListStuck(context.Context, uint64, int) ([]*tradepb.TradeAssetOpRecord, error) {
	return []*tradepb.TradeAssetOpRecord{f.row}, nil
}

func (f *fakeStore) ResolveManually(_ context.Context, m assetop.ManualResolution, _ uint64) (bool, error) {
	f.calls = append(f.calls, m)
	return f.resolved, nil
}

// stuckRow 是一行满足全部前置的卡死指令:PENDING、够老、反复 UNKNOWN。
func stuckRow() *tradepb.TradeAssetOpRecord {
	return &tradepb.TradeAssetOpRecord{
		OpId:        testOpID,
		PlayerId:    5,
		Stream:      uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT),
		Status:      tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING,
		Attempts:    resolveMinAttempts,
		LastOutcome: uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN),
		CreatedMs:   uint64(time.Now().Add(-2 * resolveMinAge).UnixMilli()),
	}
}

func applyOptions() resolveOptions {
	return resolveOptions{opID: testOpID, as: "applied", final: assetop.StatusApplied, operator: "ops", reason: "ticket-1", txlogChecked: true}
}

func TestRunResolve_FinalizesAndPrintsAudit(t *testing.T) {
	store := &fakeStore{row: stuckRow(), resolved: true}
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), store, applyOptions(), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if len(store.calls) != 1 || store.calls[0].OpID != testOpID || store.calls[0].Final != assetop.StatusApplied ||
		store.calls[0].Operator != "ops" || store.calls[0].Reason != "ticket-1" {
		t.Fatalf("calls=%+v", store.calls)
	}
	if !strings.Contains(stdout.String(), "[AssetOpManual] operator=ops") {
		t.Fatalf("审计行要打到 stdout: %s", stdout.String())
	}
}

func TestRunResolve_CASLostReportsNotPending(t *testing.T) {
	store := &fakeStore{row: stuckRow(), resolved: false}
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), store, applyOptions(), &stdout, &stderr); code != exitNotPending {
		t.Fatalf("exit=%d", code)
	}
}

func TestRunResolve_WithoutTxlogCheckedPrintsChecklistAndWritesNothing(t *testing.T) {
	store := &fakeStore{row: stuckRow(), resolved: true}
	opts := applyOptions()
	opts.txlogChecked = false
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), store, opts, &stdout, &stderr); code != exitRejected {
		t.Fatalf("exit=%d", code)
	}
	if len(store.calls) != 0 {
		t.Fatalf("不得写库: %+v", store.calls)
	}
	if !strings.Contains(stderr.String(), "correlation_id=70001") {
		t.Fatalf("清单要带 correlation_id: %s", stderr.String())
	}
}

func TestRunResolve_RowErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), &fakeStore{}, applyOptions(), &stdout, &stderr); code != exitRejected {
		t.Fatalf("行不存在 exit=%d", code)
	}
	if code := runResolve(context.Background(), &fakeStore{readErr: errors.New("boom")}, applyOptions(), &stdout, &stderr); code != exitError {
		t.Fatalf("读失败 exit=%d", code)
	}
}

func TestCheckResolvable(t *testing.T) {
	nowMs := uint64(time.Now().UnixMilli())
	mutate := func(change func(*tradepb.TradeAssetOpRecord)) *tradepb.TradeAssetOpRecord {
		row := stuckRow()
		change(row)
		return row
	}
	cases := []struct {
		name      string
		row       *tradepb.TradeAssetOpRecord
		wantOK    bool
		wantForce bool
		label     string
	}{
		{name: "全部满足", row: stuckRow(), wantOK: true, wantForce: true, label: "yes"},
		{name: "已终结", row: mutate(func(r *tradepb.TradeAssetOpRecord) {
			r.Status = tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED
		}), label: "no"},
		{name: "太新", row: mutate(func(r *tradepb.TradeAssetOpRecord) { r.CreatedMs = nowMs }), label: "no"},
		{name: "次数不够", row: mutate(func(r *tradepb.TradeAssetOpRecord) { r.Attempts = resolveMinAttempts - 1 }), wantForce: true, label: "force"},
		{name: "不是 UNKNOWN", row: mutate(func(r *tradepb.TradeAssetOpRecord) {
			r.LastOutcome = uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY)
		}), wantForce: true, label: "force"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := checkResolvable(c.row, nowMs, false) == nil; got != c.wantOK {
				t.Fatalf("force=false: got %v", got)
			}
			if got := checkResolvable(c.row, nowMs, true) == nil; got != c.wantForce {
				t.Fatalf("force=true: got %v", got)
			}
			if got := resolvableLabel(c.row, nowMs); got != c.label {
				t.Fatalf("label=%s", got)
			}
		})
	}
}

func TestParseResolve(t *testing.T) {
	valid := []string{"-op", "70001", "-as", "aborted", "-operator", "ops", "-reason", "ticket-1", "-confirm", "70001"}
	var stderr bytes.Buffer
	opts, ok := parseResolve(valid, &stderr)
	if !ok || opts.final != assetop.StatusAborted || opts.txlogChecked || opts.force {
		t.Fatalf("opts=%+v ok=%v stderr=%s", opts, ok, stderr.String())
	}
	bad := map[string][]string{
		"缺 op":   {"-as", "applied", "-operator", "ops", "-reason", "r", "-confirm", "0"},
		"结局非法":   {"-op", "1", "-as", "rejected", "-operator", "ops", "-reason", "r", "-confirm", "1"},
		"缺操作人":   {"-op", "1", "-as", "applied", "-reason", "r", "-confirm", "1"},
		"操作人带换行": {"-op", "1", "-as", "applied", "-operator", "a\nb", "-reason", "r", "-confirm", "1"},
		"缺理由":    {"-op", "1", "-as", "applied", "-operator", "ops", "-confirm", "1"},
		"二次确认不符": {"-op", "1", "-as", "applied", "-operator", "ops", "-reason", "r", "-confirm", "2"},
		"多余位置参数": {"-op", "1", "-as", "applied", "-operator", "ops", "-reason", "r", "-confirm", "1", "extra"},
	}
	for name, args := range bad {
		if _, ok := parseResolve(args, &stderr); ok {
			t.Errorf("%s 应该被拒绝", name)
		}
	}
}

func TestParseList(t *testing.T) {
	var stderr bytes.Buffer
	if _, ok := parseList(nil, &stderr); !ok {
		t.Fatal("默认参数要通过")
	}
	for _, args := range [][]string{{"-limit", "0"}, {"-limit", "501"}, {"-min-age-min", "-1"}, {"extra"}} {
		if _, ok := parseList(args, &stderr); ok {
			t.Errorf("%v 应该被拒绝", args)
		}
	}
}

func TestRunList_PrintsResolvableColumn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runList(context.Background(), &fakeStore{row: stuckRow()}, listOptions{minAgeMin: 60, limit: 10}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "70001") || !strings.Contains(stdout.String(), "yes") {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestRun_RejectsBadArgsBeforeTouchingConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != exitRejected {
		t.Fatalf("没有子命令 exit=%d", code)
	}
	if code := run([]string{"-f", "does-not-exist.yaml", "resolve", "-op", "1"}, &stdout, &stderr); code != exitRejected {
		t.Fatalf("参数错误要先于读配置拒绝 exit=%d", code)
	}
	if code := run([]string{"nope"}, &stdout, &stderr); code != exitRejected {
		t.Fatalf("未知子命令 exit=%d", code)
	}
}
