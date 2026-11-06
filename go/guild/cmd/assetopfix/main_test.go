package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"guild/internal/data"
	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	pb "proto/guild"
	"shared/assetop"
)

// 人工终结的证据只认已提交账本(05-economy.md §5.23)。流水在内存改动时就发出、早于存盘提交,不能凭它判 applied。

const testOpID uint64 = 70001

type fakeStore struct {
	row      data.AssetOpRow
	resolved bool
	calls    []assetop.ManualResolution
	witness  []assetop.QuietWitness
}

func (f *fakeStore) GetOp(context.Context, uint64) (data.AssetOpRow, bool, error) {
	return f.row, true, nil
}

func (f *fakeStore) ListStuck(context.Context, uint64, int) ([]data.AssetOpRow, error) {
	return []data.AssetOpRow{f.row}, nil
}

func (f *fakeStore) ResolveManuallyIfQuiet(_ context.Context, r assetop.ManualResolution, witness assetop.QuietWitness, _ uint64) (bool, error) {
	f.calls = append(f.calls, r)
	f.witness = append(f.witness, witness)
	return f.resolved, nil
}

type fakeLedger struct {
	snapshot assetop.StoreLedger
	err      error
	reads    int
}

func (f *fakeLedger) ReadStoreLedger(context.Context, uint64) (assetop.StoreLedger, error) {
	f.reads++
	return f.snapshot, f.err
}

// stuckRow 是一行满足全部行级前置的卡死捐献:PENDING、够老、反复 UNKNOWN;一小时前发过最后一次,现在没人持租约。
func stuckRow(t *testing.T) data.AssetOpRow {
	t.Helper()
	payload, err := proto.Marshal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: 10000}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return data.AssetOpRow{
		OpID: testOpID, PlayerID: 5, GuildID: 9,
		Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, Seq: 3, StreamEpoch: 1700000000000,
		Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, Status: pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		Attempts: resolveMinAttempts, LastOutcome: uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN),
		CreatedMs: uint64(now.Add(-2 * time.Hour).UnixMilli()), Payload: payload, TxType: 24,
		LastSendMs: uint64(now.Add(-time.Hour).UnixMilli()),
	}
}

// committedLedger 是这名玩家已提交的账本。seen=false 时这个 seq 没有结局也没有收据;edit 可以改坏收据。
func committedLedger(t *testing.T, r data.AssetOpRow, seen, applied, partial bool, edit func(*componentpb.AssetOpReceipt)) *componentpb.PlayerAssetOpLedgerComp {
	t.Helper()
	stream := &componentpb.AssetOpStreamLedger{Stream: r.Stream, StreamEpoch: r.StreamEpoch,
		SeenBits: make([]uint64, assetop.WindowWords), AppliedBits: make([]uint64, assetop.WindowWords)}
	if seen {
		op, err := r.AssetOp()
		if err != nil {
			t.Fatal(err)
		}
		digest, ok := assetop.ReceiptDigest(op)
		if !ok {
			t.Fatal("receipt digest unavailable")
		}
		i := r.Seq - 1
		stream.SeenBits[i/64] |= uint64(1) << (i % 64)
		if applied {
			stream.AppliedBits[i/64] |= uint64(1) << (i % 64)
		}
		if partial {
			stream.PartialSeqs = []uint64{r.Seq}
		}
		stream.MaxSeq = r.Seq
		receipt := &componentpb.AssetOpReceipt{Seq: r.Seq, CorrelationId: r.OpID, RequestDigest: digest[:]}
		if edit != nil {
			edit(receipt)
		}
		stream.Receipts = []*componentpb.AssetOpReceipt{receipt}
	}
	return &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{stream}}
}

func resolveAs(as string, final assetop.Status, force bool) resolveOptions {
	return resolveOptions{opID: testOpID, as: as, final: final, operator: "ops", reason: "ticket-1", txlogChecked: true, force: force}
}

func deps(store *fakeStore, ledger *fakeLedger) toolDeps {
	return toolDeps{store: store, ledger: ledger, quietGap: assetop.ManualQuietFloor}
}

// 已提交账本说已应用、收据是这一条请求:判 applied,终结带着读行时的租约与发送时刻。
func TestRunResolve_AppliedWithCommittedReceiptFinalizes(t *testing.T) {
	row := stuckRow(t)
	store := &fakeStore{row: row, resolved: true}
	ledger := &fakeLedger{snapshot: assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, true, false, nil)}}
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(store, ledger), resolveAs("applied", assetop.StatusApplied, false), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if len(store.calls) != 1 || store.calls[0].Final != assetop.StatusApplied {
		t.Fatalf("calls=%+v", store.calls)
	}
	if store.witness[0] != (assetop.QuietWitness{LeaseUntilMs: row.LeaseUntilMs, LastSendMs: row.LastSendMs}) {
		t.Fatalf("witness=%+v, want the row's lease / last send as read", store.witness[0])
	}
	if !strings.Contains(stdout.String(), "evidence=applied evidence_source=player_store") {
		t.Fatalf("审计行要写明证据与来源: %s", stdout.String())
	}
}

// 回归:账本里没有已应用的结局就不能判 applied。原来核对清单让值班凭「流水里有这条 correlation_id」判 applied,
// 而那条流水可能来自一次从没落盘的内存改动(崩溃重载、确定性拒存)。
func TestRunResolve_AppliedNeedsTheCommittedLedger(t *testing.T) {
	row := stuckRow(t)
	cases := []struct {
		name     string
		snapshot assetop.StoreLedger
	}{
		{"没有写权行", assetop.StoreLedger{}},
		{"账本没见过,玩家还被 scene 持有", assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, false, false, false, nil)}},
		{"账本没见过,写权行已释放", assetop.StoreLedger{Found: true, Released: true, Ledger: committedLedger(t, row, false, false, false, nil)}},
		{"账本记的是拒绝", assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, false, false, nil)}},
		{"同一个 seq 上是别的请求", assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, true, false,
			func(r *componentpb.AssetOpReceipt) { r.RequestDigest[0] ^= 1 })}},
		{"部分发放", assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, true, true, nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{row: row, resolved: true}
			var stdout, stderr bytes.Buffer
			code := runResolve(context.Background(), deps(store, &fakeLedger{snapshot: tc.snapshot}), resolveAs("applied", assetop.StatusApplied, false), &stdout, &stderr)
			if code != exitRejected || len(store.calls) != 0 {
				t.Fatalf("exit=%d calls=%d stderr=%s", code, len(store.calls), stderr.String())
			}
			if !strings.Contains(stderr.String(), "source: committed asset ledger") {
				t.Fatalf("拒绝时要写明证据来源: %s", stderr.String())
			}
		})
	}
}

// 回归:-force 只跳过「反复 UNKNOWN」这一条,证据照查。原来 -force 能终结任何超过 30 分钟的 PENDING 行。
// 这里的行是离线停放(last_outcome=NOT_HERE),只有 -force 才过得了行级前置。
func TestRunResolve_ForceAbortNeedsLedgerProof(t *testing.T) {
	base := stuckRow(t)
	base.LastOutcome = uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE)
	unseen := committedLedger(t, base, false, false, false, nil)
	slid := committedLedger(t, base, false, false, false, nil)
	slid.Streams[0].Watermark = 10 // seq 3 已滑出窗口,结局不可知

	recentSend := base
	recentSend.LastSendMs = uint64(time.Now().Add(-time.Minute).UnixMilli())
	leaseHeld := base
	leaseHeld.LeaseUntilMs = uint64(time.Now().Add(time.Minute).UnixMilli())
	notMaintained := base
	notMaintained.LastSendMs = 0

	cases := []struct {
		name     string
		row      data.AssetOpRow
		snapshot assetop.StoreLedger
		want     int
	}{
		{"已释放、账本没见过、安静:确认从未应用", base, assetop.StoreLedger{Found: true, Released: true, Ledger: unseen}, exitOK},
		{"账本记的是拒绝", base, assetop.StoreLedger{Found: true, Ledger: committedLedger(t, base, true, false, false, nil)}, exitOK},
		{"玩家还被 scene 持有", base, assetop.StoreLedger{Found: true, Ledger: unseen}, exitRejected},
		{"刚投递过", recentSend, assetop.StoreLedger{Found: true, Released: true, Ledger: unseen}, exitRejected},
		{"现在有人持租约", leaseHeld, assetop.StoreLedger{Found: true, Released: true, Ledger: unseen}, exitRejected},
		{"last_send_ms 没维护", notMaintained, assetop.StoreLedger{Found: true, Released: true, Ledger: unseen}, exitRejected},
		{"滑出窗口,账本说不清", base, assetop.StoreLedger{Found: true, Released: true, Ledger: slid}, exitRejected},
		{"账本记的是已应用", base, assetop.StoreLedger{Found: true, Released: true, Ledger: committedLedger(t, base, true, true, false, nil)}, exitRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{row: tc.row, resolved: true}
			var stdout, stderr bytes.Buffer
			code := runResolve(context.Background(), deps(store, &fakeLedger{snapshot: tc.snapshot}), resolveAs("aborted", assetop.StatusAborted, true), &stdout, &stderr)
			if code != tc.want {
				t.Fatalf("exit=%d want=%d stderr=%s", code, tc.want, stderr.String())
			}
			if (len(store.calls) == 1) != (tc.want == exitOK) {
				t.Fatalf("calls=%d", len(store.calls))
			}
		})
	}
}

// 读不到已提交账本就没有证据:执行出错,不改任何数据。
func TestRunResolve_LedgerReadFailureIsAnError(t *testing.T) {
	store := &fakeStore{row: stuckRow(t), resolved: true}
	var stdout, stderr bytes.Buffer
	code := runResolve(context.Background(), deps(store, &fakeLedger{err: errors.New("data_service unavailable")}),
		resolveAs("applied", assetop.StatusApplied, false), &stdout, &stderr)
	if code != exitError || len(store.calls) != 0 {
		t.Fatalf("exit=%d calls=%d", code, len(store.calls))
	}
}

// 读证据之后有人领过这一行:安静 CAS 落空,如实报「未做改动」。
func TestRunResolve_ClaimedAfterEvidenceReportsNotPending(t *testing.T) {
	row := stuckRow(t)
	store := &fakeStore{row: row, resolved: false}
	ledger := &fakeLedger{snapshot: assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, true, false, nil)}}
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(store, ledger), resolveAs("applied", assetop.StatusApplied, false), &stdout, &stderr); code != exitNotPending {
		t.Fatalf("exit=%d", code)
	}
}

// 核对清单不再让人凭流水下结论:流水只作线索,结论来自工具读到的已提交账本。
func TestPrintChecklist_TransactionLogIsOnlyALead(t *testing.T) {
	row := stuckRow(t)
	store := &fakeStore{row: row, resolved: true}
	ledger := &fakeLedger{snapshot: assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, true, false, nil)}}
	opts := resolveAs("applied", assetop.StatusApplied, false)
	opts.txlogChecked = false
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), deps(store, ledger), opts, &stdout, &stderr); code != exitRejected {
		t.Fatalf("exit=%d", code)
	}
	out := stderr.String()
	if strings.Contains(out, "record present = scene applied") {
		t.Fatalf("清单不得把流水当已应用的证据: %s", out)
	}
	for _, want := range []string{"only leads, not evidence", "evidence=applied", "-as applied"} {
		if !strings.Contains(out, want) {
			t.Fatalf("清单缺 %q: %s", want, out)
		}
	}
	if len(store.calls) != 0 {
		t.Fatal("没带 -txlog-checked 不得改数据")
	}
}

// lastColumns 取表格第一行数据(表头之后)的最后三列:evidence、allowed、resolvable。
func lastColumns(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("no data row: %s", out)
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 3 {
		t.Fatalf("data row too short: %q", lines[1])
	}
	return fields[len(fields)-3:]
}

// list 的 resolvable 合并行级前置与证据:证据不支持任何结局时一律 no。
func TestRunList_ResolvableNeedsEvidence(t *testing.T) {
	row := stuckRow(t)
	store := &fakeStore{row: row}
	var stdout, stderr bytes.Buffer
	ledger := &fakeLedger{snapshot: assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, false, false, false, nil)}}
	if code := runList(context.Background(), deps(store, ledger), listOptions{limit: 10}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if got := lastColumns(t, stdout.String()); strings.Join(got, " ") != "none none no" {
		t.Fatalf("账本说不清时 evidence=none、allowed=none、resolvable=no,got %v", got)
	}

	stdout.Reset()
	ledger.snapshot = assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, true, false, nil)}
	if code := runList(context.Background(), deps(store, ledger), listOptions{limit: 10}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d", code)
	}
	if got := lastColumns(t, stdout.String()); strings.Join(got, " ") != "applied applied yes" {
		t.Fatalf("账本证实已应用时 allowed=applied、resolvable=yes,got %v", got)
	}

	ledger.err = errors.New("data_service unavailable")
	if code := runList(context.Background(), deps(store, ledger), listOptions{limit: 10}, &stdout, &stderr); code != exitError {
		t.Fatalf("读不到账本时 list 以 1 退出,exit=%d", code)
	}
}

// 本工具只开放 applied / aborted 两种结局,结论规则用共享的 ManualVerdict.Allows(与 trade 工具同一份)。
func TestVerdictAllows(t *testing.T) {
	cases := []struct {
		v       assetop.ManualVerdict
		applied bool
		aborted bool
	}{
		{assetop.ManualVerdictNone, false, false},
		{assetop.ManualVerdictApplied, true, false},
		{assetop.ManualVerdictPartial, false, false},
		{assetop.ManualVerdictRejected, false, true},
		{assetop.ManualVerdictNeverApplied, false, true},
	}
	for _, tc := range cases {
		if tc.v.Allows(assetop.StatusApplied) != tc.applied || tc.v.Allows(assetop.StatusAborted) != tc.aborted {
			t.Fatalf("%s: applied=%v aborted=%v", tc.v, tc.v.Allows(assetop.StatusApplied), tc.v.Allows(assetop.StatusAborted))
		}
		for _, final := range []assetop.Status{assetop.StatusRejected, assetop.StatusAppliedPartial, assetop.StatusAbandoned} {
			if tc.v.Allows(final) {
				t.Fatalf("%s must never allow %s", tc.v, final)
			}
		}
	}
}

// 请求还原不出(载荷缺失)就核不了收据:即使写权行已释放、账本没见过、行也安静,也按证据不足拒绝,不改数据。
// 改调共享判据之前,这种行在「没见过」分支里不用还原请求,会被判成 never_applied、放行 aborted。
func TestRunResolve_UnrebuildableRequestIsNoEvidence(t *testing.T) {
	row := stuckRow(t)
	unseen := committedLedger(t, row, false, false, false, nil)
	row.Payload = nil
	store := &fakeStore{row: row, resolved: true}
	var stdout, stderr bytes.Buffer
	code := runResolve(context.Background(), deps(store, &fakeLedger{snapshot: assetop.StoreLedger{Found: true, Released: true, Ledger: unseen}}),
		resolveAs("aborted", assetop.StatusAborted, false), &stdout, &stderr)
	if code != exitRejected || len(store.calls) != 0 {
		t.Fatalf("exit=%d calls=%d stderr=%s", code, len(store.calls), stderr.String())
	}
	for _, want := range []string{"evidence=none", "cannot rebuild the request"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("拒绝时要写明 %q: %s", want, stderr.String())
		}
	}
}
