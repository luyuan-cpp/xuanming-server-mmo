package assetop

import (
	"testing"
	"time"

	componentpb "proto/common/component"
)

// 人工终结的结论表(manual_evidence.go):账本说得清、收据是这一条请求、行安静,才给结论;其余一律不终结。
func TestJudgeManual(t *testing.T) {
	op := testOp(7)
	const gap = 30 * time.Minute
	nowMs := uint64(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC).UnixMilli())
	quietSendMs := nowMs - uint64((gap+time.Minute)/time.Millisecond)
	recentSendMs := nowMs - uint64(time.Minute/time.Millisecond)

	// ledgerWith 造一份记了 op 这个 seq 的已提交账本;seen=false 表示窗口内没见过。edit 改收据。
	ledgerWith := func(seen, applied, partial bool, edit func(*componentpb.AssetOpReceipt)) *componentpb.PlayerAssetOpLedgerComp {
		stream := newStreamLedger(op.Stream, op.StreamEpoch, 0, 0)
		if seen {
			markSeen(stream, op.Seq, applied)
			receipt := identityReceiptFixture(op)
			if edit != nil {
				edit(receipt)
			}
			stream.Receipts = []*componentpb.AssetOpReceipt{receipt}
		}
		if partial {
			stream.PartialSeqs = []uint64{op.Seq}
		}
		return &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{stream}}
	}
	// 窗口下沿 watermark=50:op 的 seq 7 已滑出窗口,结局不可知。
	slid := newStreamLedger(op.Stream, op.StreamEpoch, 50, 60)
	markSeen(slid, 60, true)
	slidLedger := &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{slid}}

	row := func(lastReason uint32, leaseUntilMs, lastSendMs uint64) ManualRow {
		return ManualRow{Op: op, LastReason: lastReason, LeaseUntilMs: leaseUntilMs, LastSendMs: lastSendMs}
	}
	quiet := row(0, 0, quietSendMs)

	cases := []struct {
		name     string
		snapshot StoreLedger
		row      ManualRow
		want     ManualVerdict
	}{
		{"applied, receipt matches", StoreLedger{Found: true, Ledger: ledgerWith(true, true, false, nil)}, quiet, ManualVerdictApplied},
		{"applied, receipt is another request", StoreLedger{Found: true, Ledger: ledgerWith(true, true, false, func(r *componentpb.AssetOpReceipt) { r.RequestDigest[0] ^= 1 })}, quiet, ManualVerdictNone},
		{"applied, receipt correlation differs", StoreLedger{Found: true, Ledger: ledgerWith(true, true, false, func(r *componentpb.AssetOpReceipt) { r.CorrelationId++ })}, quiet, ManualVerdictNone},
		{"partial by ledger flag", StoreLedger{Found: true, Ledger: ledgerWith(true, true, true, nil)}, quiet, ManualVerdictPartial},
		{"partial by sticky last_reason", StoreLedger{Found: true, Ledger: ledgerWith(true, true, false, nil)}, row(ReasonPartialApplied, 0, quietSendMs), ManualVerdictPartial},
		{"rejected, receipt matches", StoreLedger{Found: true, Ledger: ledgerWith(true, false, false, nil)}, quiet, ManualVerdictRejected},
		{"unseen while a scene holds the player", StoreLedger{Found: true, Ledger: ledgerWith(false, false, false, nil)}, quiet, ManualVerdictNone},
		{"unseen, released, quiet", StoreLedger{Found: true, Released: true, Ledger: ledgerWith(false, false, false, nil)}, quiet, ManualVerdictNeverApplied},
		{"unseen, released, lease held now", StoreLedger{Found: true, Released: true, Ledger: ledgerWith(false, false, false, nil)}, row(0, nowMs+1000, quietSendMs), ManualVerdictNone},
		{"unseen, released, sent a minute ago", StoreLedger{Found: true, Released: true, Ledger: ledgerWith(false, false, false, nil)}, row(0, 0, recentSendMs), ManualVerdictNone},
		{"unseen, released, last_send_ms not maintained", StoreLedger{Found: true, Released: true, Ledger: ledgerWith(false, false, false, nil)}, row(0, 0, 0), ManualVerdictNone},
		{"behind the window", StoreLedger{Found: true, Released: true, Ledger: slidLedger}, quiet, ManualVerdictNone},
		{"no owner row", StoreLedger{}, quiet, ManualVerdictNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, detail := JudgeManual(c.snapshot, c.row, nowMs, gap)
			if got != c.want {
				t.Fatalf("JudgeManual = %s (%s), want %s", got, detail, c.want)
			}
			if detail == "" {
				t.Fatal("detail must explain the verdict")
			}
		})
	}
}

func TestManualVerdictAllows(t *testing.T) {
	cases := []struct {
		verdict   ManualVerdict
		applied   bool
		aborted   bool
		allowedAs string
	}{
		{ManualVerdictNone, false, false, "none"},
		{ManualVerdictApplied, true, false, "applied"},
		{ManualVerdictPartial, false, false, "none"},
		{ManualVerdictRejected, false, true, "aborted"},
		{ManualVerdictNeverApplied, false, true, "aborted"},
	}
	for _, c := range cases {
		t.Run(c.verdict.String(), func(t *testing.T) {
			if got := c.verdict.Allows(StatusApplied); got != c.applied {
				t.Fatalf("applied allowed = %v, want %v", got, c.applied)
			}
			if got := c.verdict.Allows(StatusAborted); got != c.aborted {
				t.Fatalf("aborted allowed = %v, want %v", got, c.aborted)
			}
			// 拒绝是 scene 的判定、放弃只能自动产生:人工一律不许。
			for _, final := range []Status{StatusRejected, StatusAbandoned} {
				if c.verdict.Allows(final) {
					t.Fatalf("final %v must never be allowed manually", final)
				}
			}
			if got := c.verdict.AllowedAs(); got != c.allowedAs {
				t.Fatalf("AllowedAs = %s, want %s", got, c.allowedAs)
			}
		})
	}
}

func TestManualQuietGapTakesTheLongerOfFloorAndLease(t *testing.T) {
	if got := ManualQuietGap(10 * time.Second); got != ManualQuietFloor {
		t.Fatalf("short lease: got %v, want the floor %v", got, ManualQuietFloor)
	}
	if got := ManualQuietGap(time.Hour); got != time.Hour {
		t.Fatalf("long lease: got %v, want the lease", got)
	}
}

func TestManualRowWitnessCarriesTheReadValues(t *testing.T) {
	row := ManualRow{LeaseUntilMs: 11, LastSendMs: 22}
	if w := row.Witness(); w.LeaseUntilMs != 11 || w.LastSendMs != 22 {
		t.Fatalf("witness = %+v", w)
	}
}
