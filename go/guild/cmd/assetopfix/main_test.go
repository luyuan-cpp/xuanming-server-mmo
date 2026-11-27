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
//
// 时间一律从 testNow 派生、经 toolDeps.now 注入,不读真实墙钟:行够不够老、安静不安静、终结写下的时刻都是确定的。

const testOpID uint64 = 70001

// testNow 是全部用例共用的固定时刻(2025-11-01T08:00:00Z)。
var testNow = time.UnixMilli(1_761_984_000_000)

func fixedClock() time.Time { return testNow }

type fakeStore struct {
	row      data.AssetOpRow
	resolved bool
	calls    []assetop.ManualResolution
	witness  []assetop.QuietWitness
	// nowMs 是每次人工终结收到的时刻:应当就是注入的时钟。
	nowMs []uint64
}

func (f *fakeStore) GetOp(context.Context, uint64) (data.AssetOpRow, bool, error) {
	return f.row, true, nil
}

func (f *fakeStore) ListStuck(context.Context, uint64, int) ([]data.AssetOpRow, error) {
	return []data.AssetOpRow{f.row}, nil
}

func (f *fakeStore) ResolveManuallyIfQuiet(_ context.Context, r assetop.ManualResolution, witness assetop.QuietWitness, nowMs uint64) (bool, error) {
	f.calls = append(f.calls, r)
	f.witness = append(f.witness, witness)
	f.nowMs = append(f.nowMs, nowMs)
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

// testWindow 是用例里的观察窗口,与一分钟下限分开,好认出等的是它。
const testWindow = 3 * time.Minute

// fakeSleep 是观察窗口等待的替身:记下每次要等多久,不真等;during 在「等待期间」改夹具(循环领走这一行、账本变了)。
type fakeSleep struct {
	waits  []time.Duration
	during func()
	err    error
}

func (f *fakeSleep) sleep(_ context.Context, d time.Duration) error {
	f.waits = append(f.waits, d)
	if f.during != nil {
		f.during()
	}
	return f.err
}

// stuckRow 是一行满足全部行级前置的卡死捐献:PENDING、够老、反复 UNKNOWN;一小时前发过最后一次,现在没人持租约。
func stuckRow(t *testing.T) data.AssetOpRow {
	t.Helper()
	payload, err := proto.Marshal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: 10000}}})
	if err != nil {
		t.Fatal(err)
	}
	return data.AssetOpRow{
		OpID: testOpID, PlayerID: 5, GuildID: 9,
		Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, Seq: 3, StreamEpoch: 1700000000000,
		Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, Status: pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		Attempts: resolveMinAttempts, LastOutcome: uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN),
		CreatedMs: uint64(testNow.Add(-2 * time.Hour).UnixMilli()), Payload: payload, TxType: 24,
		LastSendMs: uint64(testNow.Add(-time.Hour).UnixMilli()),
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

// deps 的观察窗口等待不真等,期间什么都不变;要看等待或改夹具的用例换上自己的 fakeSleep。
func deps(store *fakeStore, ledger *fakeLedger) toolDeps {
	return toolDeps{store: store, ledger: ledger, quietGap: assetop.ManualQuietFloor, observeWindow: testWindow, now: fixedClock,
		sleep: (&fakeSleep{}).sleep}
}

// 已提交账本说已应用、收据是这一条请求:判 applied,终结带着读行时的租约、发送时刻与 attempts。
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
	if store.witness[0] != (assetop.QuietWitness{LeaseUntilMs: row.LeaseUntilMs, LastSendMs: row.LastSendMs, Attempts: row.Attempts}) {
		t.Fatalf("witness=%+v, want the row's lease / last send / attempts as read", store.witness[0])
	}
	if store.nowMs[0] != uint64(testNow.UnixMilli()) {
		t.Fatalf("终结时刻=%d,要的是注入的时钟 %d(不读真实墙钟)", store.nowMs[0], testNow.UnixMilli())
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
	recentSend.LastSendMs = uint64(testNow.Add(-time.Minute).UnixMilli())
	leaseHeld := base
	leaseHeld.LeaseUntilMs = uint64(testNow.Add(time.Minute).UnixMilli())
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

// neverAppliedLedger 是「写权行已释放、账本没见过这个 seq」的已提交账本:配上 stuckRow 的安静行,证据就是 never_applied。
func neverAppliedLedger(t *testing.T, row data.AssetOpRow) *fakeLedger {
	t.Helper()
	return &fakeLedger{snapshot: assetop.StoreLedger{Found: true, Released: true, Ledger: committedLedger(t, row, false, false, false, nil)}}
}

// 「从未应用」先在本机等满观察窗口,再读一次行与账本:都没变才终结,CAS 带的仍是第一次读到的整份见证。
func TestRunResolve_NeverAppliedWaitsTheObservationWindowThenFinalizes(t *testing.T) {
	row := stuckRow(t)
	store := &fakeStore{row: row, resolved: true}
	ledger := neverAppliedLedger(t, row)
	sleeper := &fakeSleep{}
	d := deps(store, ledger)
	d.sleep = sleeper.sleep
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), d, resolveAs("aborted", assetop.StatusAborted, false), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if len(sleeper.waits) != 1 || sleeper.waits[0] != testWindow {
		t.Fatalf("waits=%v, want one wait of %v", sleeper.waits, testWindow)
	}
	if ledger.reads != 2 {
		t.Fatalf("ledger reads=%d, want 2(读证据一次,观察窗口之后重判一次)", ledger.reads)
	}
	want := assetop.QuietWitness{LeaseUntilMs: row.LeaseUntilMs, LastSendMs: row.LastSendMs, Attempts: row.Attempts}
	if len(store.calls) != 1 || store.witness[0] != want {
		t.Fatalf("calls=%+v witness=%+v want %+v", store.calls, store.witness, want)
	}
	if !strings.Contains(stderr.String(), "waiting 3m0s on the local monotonic clock") {
		t.Fatalf("等待前要告诉操作员等多久、为什么: %s", stderr.String())
	}
}

// 观察期间这一行被领过、发过或终结了:不终结,退出码 3,打审计行并提示重跑。只有 attempts 变了不在这里,见下一条。
func TestRunResolve_ObservationWindowRefusesWhenTheRowChanged(t *testing.T) {
	cases := map[string]func(*data.AssetOpRow){
		"claimed":               func(r *data.AssetOpRow) { r.LeaseUntilMs = uint64(testNow.Add(10 * time.Second).UnixMilli()) },
		"sent and rescheduled":  func(r *data.AssetOpRow) { r.Attempts++; r.LastSendMs++ },
		"finalized by the loop": func(r *data.AssetOpRow) { r.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			row := stuckRow(t)
			store := &fakeStore{row: row, resolved: true}
			d := deps(store, neverAppliedLedger(t, row))
			d.sleep = (&fakeSleep{during: func() { change(&store.row) }}).sleep
			var stdout, stderr bytes.Buffer
			if code := runResolve(context.Background(), d, resolveAs("aborted", assetop.StatusAborted, false), &stdout, &stderr); code != exitNotPending {
				t.Fatalf("exit=%d want %d stderr=%s", code, exitNotPending, stderr.String())
			}
			if len(store.calls) != 0 {
				t.Fatalf("不得终结: %+v", store.calls)
			}
			if out := stdout.String(); !strings.Contains(out, "finalized=false") || !strings.Contains(out, "rerun") {
				t.Fatalf("要打审计行并提示重跑: %s", out)
			}
		})
	}
}

// 观察期间只有 attempts 变了(只领没发的本地重排,或钟慢的副本发过、结局不明又重排):跨窗口不比它,照样终结,
// 条件写带第二次读到的见证(attempts 是新的),SQL 按三列比(asset_store.go 的 sqlResolveAssetOpQuiet),
// 第二次读到条件写之间再有重排也落空;钟慢副本那次发送若被 scene 应用,由下一条用例的账本重判覆盖。
func TestRunResolve_ObservationWindowToleratesAttemptsOnlyAndUsesTheSecondRead(t *testing.T) {
	row := stuckRow(t)
	store := &fakeStore{row: row, resolved: true}
	d := deps(store, neverAppliedLedger(t, row))
	d.sleep = (&fakeSleep{during: func() { store.row.Attempts++ }}).sleep
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), d, resolveAs("aborted", assetop.StatusAborted, false), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	want := assetop.QuietWitness{LeaseUntilMs: row.LeaseUntilMs, LastSendMs: row.LastSendMs, Attempts: row.Attempts + 1}
	if len(store.calls) != 1 || store.witness[0] != want {
		t.Fatalf("calls=%+v witness=%+v want %+v(第二次读到的见证)", store.calls, store.witness, want)
	}
}

// 观察期间账本变了(scene 存盘见过了这个 seq,或玩家上线被 scene 持有):行没变也不终结。
func TestRunResolve_ObservationWindowRefusesWhenTheLedgerChanged(t *testing.T) {
	row := stuckRow(t)
	cases := map[string]assetop.StoreLedger{
		"ledger has seen the seq":    {Found: true, Released: true, Ledger: committedLedger(t, row, true, true, false, nil)},
		"player now held by a scene": {Found: true, Ledger: committedLedger(t, row, false, false, false, nil)},
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{row: row, resolved: true}
			ledger := neverAppliedLedger(t, row)
			d := deps(store, ledger)
			d.sleep = (&fakeSleep{during: func() { ledger.snapshot = after }}).sleep
			var stdout, stderr bytes.Buffer
			if code := runResolve(context.Background(), d, resolveAs("aborted", assetop.StatusAborted, false), &stdout, &stderr); code != exitNotPending {
				t.Fatalf("exit=%d want %d stderr=%s", code, exitNotPending, stderr.String())
			}
			if len(store.calls) != 0 {
				t.Fatalf("不得终结: %+v", store.calls)
			}
		})
	}
}

// applied / rejected 有账本里的收据作证,不等观察窗口,账本只读一次。
func TestRunResolve_ReceiptEvidenceDoesNotWait(t *testing.T) {
	row := stuckRow(t)
	cases := []struct {
		name     string
		snapshot assetop.StoreLedger
		opts     resolveOptions
	}{
		{"applied", assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, true, false, nil)}, resolveAs("applied", assetop.StatusApplied, false)},
		{"rejected", assetop.StoreLedger{Found: true, Ledger: committedLedger(t, row, true, false, false, nil)}, resolveAs("aborted", assetop.StatusAborted, false)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{row: row, resolved: true}
			ledger := &fakeLedger{snapshot: c.snapshot}
			sleeper := &fakeSleep{}
			d := deps(store, ledger)
			d.sleep = sleeper.sleep
			var stdout, stderr bytes.Buffer
			if code := runResolve(context.Background(), d, c.opts, &stdout, &stderr); code != exitOK {
				t.Fatalf("exit=%d stderr=%s", code, stderr.String())
			}
			if len(sleeper.waits) != 0 || ledger.reads != 1 {
				t.Fatalf("waits=%v ledger reads=%d, want no wait and one read", sleeper.waits, ledger.reads)
			}
		})
	}
}

// 观察窗口的等待被打断(整条命令的预算到期、被取消):执行出错,不改数据。
func TestRunResolve_InterruptedObservationFinalizesNothing(t *testing.T) {
	row := stuckRow(t)
	store := &fakeStore{row: row, resolved: true}
	d := deps(store, neverAppliedLedger(t, row))
	d.sleep = (&fakeSleep{err: context.Canceled}).sleep
	var stdout, stderr bytes.Buffer
	if code := runResolve(context.Background(), d, resolveAs("aborted", assetop.StatusAborted, false), &stdout, &stderr); code != exitError {
		t.Fatalf("exit=%d want %d", code, exitError)
	}
	if len(store.calls) != 0 {
		t.Fatalf("不得终结: %+v", store.calls)
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
