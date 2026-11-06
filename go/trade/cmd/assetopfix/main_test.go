package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	tradepb "proto/trade"
	"shared/assetop"
	"trade/internal/data"
)

const (
	testOpID  uint64 = 70001
	testEpoch uint64 = 1700000000000
	testSeq   uint64 = 5
	testGap          = 30 * time.Minute
)

// fakeStore 记下人工终结的调用。resolved 决定 CAS 结果。
type fakeStore struct {
	row       *tradepb.TradeAssetOpRecord
	readErr   error
	resolved  bool
	calls     []assetop.ManualResolution
	witnesses []assetop.QuietWitness
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

func (f *fakeStore) ResolveManuallyIfQuiet(_ context.Context, m assetop.ManualResolution, witness assetop.QuietWitness, _ uint64) (bool, error) {
	f.calls = append(f.calls, m)
	f.witnesses = append(f.witnesses, witness)
	return f.resolved, nil
}

// fakeLedger 是已提交账本读取器的替身(生产是 assetop.DataServiceLedger)。
type fakeLedger struct {
	snapshot assetop.StoreLedger
	err      error
	reads    int
}

func (f *fakeLedger) ReadStoreLedger(context.Context, uint64) (assetop.StoreLedger, error) {
	f.reads++
	return f.snapshot, f.err
}

// stuckRow 是一行满足全部行级前置的卡死托管扣款:PENDING、够老、反复 UNKNOWN,30 多分钟没人投递过。
func stuckRow() *tradepb.TradeAssetOpRecord {
	payload, err := proto.Marshal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 30}}})
	if err != nil {
		panic(err)
	}
	now := time.Now()
	return &tradepb.TradeAssetOpRecord{
		OpId:        testOpID,
		PlayerId:    5,
		Stream:      uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT),
		StreamEpoch: testEpoch,
		Seq:         testSeq,
		TxType:      24,
		Payload:     payload,
		Status:      tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING,
		Attempts:    resolveMinAttempts,
		LastOutcome: uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN),
		CreatedMs:   uint64(now.Add(-2 * resolveMinAge).UnixMilli()),
		LastSendMs:  uint64(now.Add(-testGap - time.Minute).UnixMilli()),
	}
}

// ledgerFor 造一份这名玩家已提交的账本。seen=false 表示窗口内没见过 testSeq;seen 时带上这一行请求的收据,
// editReceipt 可以把收据改成别的请求。
func ledgerFor(row *tradepb.TradeAssetOpRecord, seen, applied, partial, released bool, editReceipt func(*componentpb.AssetOpReceipt)) *fakeLedger {
	stream := &componentpb.AssetOpStreamLedger{
		Stream:      assetpb.AssetOpStream(row.GetStream()),
		StreamEpoch: row.GetStreamEpoch(),
		SeenBits:    make([]uint64, assetop.WindowWords),
		AppliedBits: make([]uint64, assetop.WindowWords),
	}
	if seen {
		// 位序与 assetop.ClassifySeq 相同:watermark=0 时 seq 对应第 seq-1 位。
		bit := row.GetSeq() - 1
		stream.SeenBits[bit/64] |= uint64(1) << (bit % 64)
		if applied {
			stream.AppliedBits[bit/64] |= uint64(1) << (bit % 64)
		}
		stream.MaxSeq = row.GetSeq()
		op, err := data.OpFromRecord(row, 0)
		if err != nil {
			panic(err)
		}
		digest, ok := assetop.ReceiptDigest(op)
		if !ok {
			panic("receipt digest")
		}
		receipt := &componentpb.AssetOpReceipt{Seq: op.Seq, CorrelationId: op.CorrelationID, RequestDigest: digest[:]}
		if editReceipt != nil {
			editReceipt(receipt)
		}
		stream.Receipts = []*componentpb.AssetOpReceipt{receipt}
	}
	if partial {
		stream.PartialSeqs = []uint64{row.GetSeq()}
	}
	return &fakeLedger{snapshot: assetop.StoreLedger{
		Found:    true,
		Released: released,
		Ledger:   &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{stream}},
	}}
}

func deps(store opStore, ledger assetop.StoreLedgerReader) toolDeps {
	return toolDeps{store: store, ledger: ledger, quietGap: testGap}
}

func applyOptions() resolveOptions {
	return resolveOptions{opID: testOpID, as: "applied", final: assetop.StatusApplied, operator: "ops", reason: "ticket-1", txlogChecked: true}
}

func abortOptions() resolveOptions {
	o := applyOptions()
	o.as, o.final = "aborted", assetop.StatusAborted
	return o
}

func TestRunResolve_FinalizesAndPrintsAudit(t *testing.T) {
	row := stuckRow()
	store := &fakeStore{row: row, resolved: true}
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(store, ledgerFor(row, true, true, false, false, nil)), applyOptions(), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if len(store.calls) != 1 || store.calls[0].OpID != testOpID || store.calls[0].Final != assetop.StatusApplied ||
		store.calls[0].Operator != "ops" || store.calls[0].Reason != "ticket-1" {
		t.Fatalf("calls=%+v", store.calls)
	}
	if !strings.Contains(stdout.String(), "[AssetOpManual] operator=ops") || !strings.Contains(stdout.String(), "evidence=applied evidence_source=player_store") {
		t.Fatalf("审计行要打到 stdout,并带上账本证据: %s", stdout.String())
	}
}

// 终结的 CAS 带的是读行时的租约与发送时刻:读完之后有人领过这一行,终结就落空。
func TestRunResolve_PassesTheReadWitnessToTheCAS(t *testing.T) {
	row := stuckRow()
	row.LeaseUntilMs = uint64(time.Now().Add(-time.Hour).UnixMilli())
	store := &fakeStore{row: row, resolved: true}
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(store, ledgerFor(row, true, false, false, false, nil)), abortOptions(), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	want := assetop.QuietWitness{LeaseUntilMs: row.GetLeaseUntilMs(), LastSendMs: row.GetLastSendMs()}
	if len(store.witnesses) != 1 || store.witnesses[0] != want {
		t.Fatalf("witnesses=%+v want %+v", store.witnesses, want)
	}
}

func TestRunResolve_CASLostReportsNotPending(t *testing.T) {
	row := stuckRow()
	store := &fakeStore{row: row, resolved: false}
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(store, ledgerFor(row, true, true, false, false, nil)), applyOptions(), &stdout, &stderr); code != exitNotPending {
		t.Fatalf("exit=%d", code)
	}
}

func TestRunResolve_WithoutTxlogCheckedPrintsChecklistAndWritesNothing(t *testing.T) {
	row := stuckRow()
	store := &fakeStore{row: row, resolved: true}
	opts := applyOptions()
	opts.txlogChecked = false
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(store, ledgerFor(row, true, true, false, false, nil)), opts, &stdout, &stderr); code != exitRejected {
		t.Fatalf("exit=%d", code)
	}
	if len(store.calls) != 0 {
		t.Fatalf("不得写库: %+v", store.calls)
	}
	if !strings.Contains(stderr.String(), "correlation_id=70001") || !strings.Contains(stderr.String(), "evidence=applied") {
		t.Fatalf("清单要带 correlation_id 与账本证据: %s", stderr.String())
	}
}

func TestRunResolve_RowErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(&fakeStore{}, &fakeLedger{}), applyOptions(), &stdout, &stderr); code != exitRejected {
		t.Fatalf("行不存在 exit=%d", code)
	}
	if code := runResolve(context.Background(), deps(&fakeStore{readErr: errors.New("boom")}, &fakeLedger{}), applyOptions(), &stdout, &stderr); code != exitError {
		t.Fatalf("读失败 exit=%d", code)
	}
}

// 证据决定能选的结局(判据在 assetop.JudgeManual,这里核工具把它接对了)。
func TestRunResolve_EvidenceDecidesTheOutcome(t *testing.T) {
	recentRow := func() *tradepb.TradeAssetOpRecord {
		row := stuckRow()
		row.LastSendMs = uint64(time.Now().Add(-time.Minute).UnixMilli())
		return row
	}
	partialRow := func() *tradepb.TradeAssetOpRecord {
		row := stuckRow()
		row.LastReason = assetop.ReasonPartialApplied
		return row
	}
	otherReceipt := func(r *componentpb.AssetOpReceipt) { r.RequestDigest[0] ^= 1 }
	cases := []struct {
		name   string
		row    *tradepb.TradeAssetOpRecord
		ledger func(*tradepb.TradeAssetOpRecord) *fakeLedger
		opts   resolveOptions
		want   int
	}{
		{"applied allows applied", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, true, true, false, false, nil) }, applyOptions(), exitOK},
		{"applied refuses aborted", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, true, true, false, true, nil) }, abortOptions(), exitRejected},
		{"receipt of another request refuses applied", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, true, true, false, false, otherReceipt) }, applyOptions(), exitRejected},
		{"rejected allows aborted", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, true, false, false, false, nil) }, abortOptions(), exitOK},
		{"rejected refuses applied", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, true, false, false, false, nil) }, applyOptions(), exitRejected},
		{"partial flag refuses applied", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, true, true, true, false, nil) }, applyOptions(), exitRejected},
		{"sticky partial reason refuses applied", partialRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, true, true, false, false, nil) }, applyOptions(), exitRejected},
		{"never applied and quiet allows aborted", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, false, false, false, true, nil) }, abortOptions(), exitOK},
		{"never applied refuses applied", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, false, false, false, true, nil) }, applyOptions(), exitRejected},
		{"released but sent a minute ago refuses aborted", recentRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, false, false, false, true, nil) }, abortOptions(), exitRejected},
		{"unseen while a scene holds the player refuses aborted", stuckRow(), func(r *tradepb.TradeAssetOpRecord) *fakeLedger { return ledgerFor(r, false, false, false, false, nil) }, abortOptions(), exitRejected},
		{"no owner row refuses aborted", stuckRow(), func(*tradepb.TradeAssetOpRecord) *fakeLedger { return &fakeLedger{} }, abortOptions(), exitRejected},
		{"no owner row refuses applied", stuckRow(), func(*tradepb.TradeAssetOpRecord) *fakeLedger { return &fakeLedger{} }, applyOptions(), exitRejected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{row: c.row, resolved: true}
			var stdout, stderr bytes.Buffer
			if code := runResolve(context.Background(), deps(store, c.ledger(c.row)), c.opts, &stdout, &stderr); code != c.want {
				t.Fatalf("exit=%d want %d stderr=%s", code, c.want, stderr.String())
			}
			wantCalls := 0
			if c.want == exitOK {
				wantCalls = 1
			}
			if len(store.calls) != wantCalls {
				t.Fatalf("calls=%+v", store.calls)
			}
		})
	}
}

// 读不到已提交账本就什么都证明不了:不终结。
func TestRunResolve_LedgerReadFailureFinalizesNothing(t *testing.T) {
	store := &fakeStore{row: stuckRow(), resolved: true}
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(store, &fakeLedger{err: errors.New("data_service down")}), applyOptions(), &stdout, &stderr); code != exitError {
		t.Fatalf("exit=%d", code)
	}
	if len(store.calls) != 0 {
		t.Fatalf("不得写库: %+v", store.calls)
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
	}{
		{name: "全部满足", row: stuckRow(), wantOK: true, wantForce: true},
		{name: "已终结", row: mutate(func(r *tradepb.TradeAssetOpRecord) {
			r.Status = tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED
		})},
		{name: "太新", row: mutate(func(r *tradepb.TradeAssetOpRecord) { r.CreatedMs = nowMs })},
		{name: "次数不够", row: mutate(func(r *tradepb.TradeAssetOpRecord) { r.Attempts = resolveMinAttempts - 1 }), wantForce: true},
		{name: "不是 UNKNOWN", row: mutate(func(r *tradepb.TradeAssetOpRecord) {
			r.LastOutcome = uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY)
		}), wantForce: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := checkResolvable(c.row, nowMs, false) == nil; got != c.wantOK {
				t.Fatalf("force=false: got %v", got)
			}
			if got := checkResolvable(c.row, nowMs, true) == nil; got != c.wantForce {
				t.Fatalf("force=true: got %v", got)
			}
		})
	}
}

// 可终结列合并行级前置与证据:证据不支持任何结局时一律 no。
func TestResolvableLabel(t *testing.T) {
	nowMs := uint64(time.Now().UnixMilli())
	young := stuckRow()
	young.CreatedMs = nowMs
	retry := stuckRow()
	retry.LastOutcome = uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY)
	cases := []struct {
		name    string
		row     *tradepb.TradeAssetOpRecord
		verdict assetop.ManualVerdict
		want    string
	}{
		{"evidence and preconditions met", stuckRow(), assetop.ManualVerdictApplied, "yes"},
		{"evidence met, only the UNKNOWN condition missing", retry, assetop.ManualVerdictRejected, "force"},
		{"no evidence", stuckRow(), assetop.ManualVerdictNone, "no"},
		{"partial grant", stuckRow(), assetop.ManualVerdictPartial, "no"},
		{"too young", young, assetop.ManualVerdictApplied, "no"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvableLabel(c.row, c.verdict, nowMs); got != c.want {
				t.Fatalf("label=%s want %s", got, c.want)
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

func TestRunList_PrintsEvidenceAndResolvableColumns(t *testing.T) {
	row := stuckRow()
	var stdout, stderr bytes.Buffer
	if code := runList(context.Background(), deps(&fakeStore{row: row}, ledgerFor(row, true, true, false, false, nil)), listOptions{minAgeMin: 60, limit: 10}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "70001") || !strings.Contains(out, "applied") || !strings.Contains(out, "yes") {
		t.Fatalf("stdout=%s", out)
	}
}

// 读不到账本的行标 evidence=error、不可终结,命令以 1 退出,不能把残缺的表当完整结论。
func TestRunList_LedgerReadFailureIsReported(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runList(context.Background(), deps(&fakeStore{row: stuckRow()}, &fakeLedger{err: errors.New("data_service down")}), listOptions{minAgeMin: 60, limit: 10}, &stdout, &stderr)
	if code != exitError {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout.String(), "error") {
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
