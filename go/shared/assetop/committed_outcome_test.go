package assetop

import (
	"testing"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

// 人工终结的证据只能是已提交账本里「这一条请求」的结局:位图说这个 seq 有过结局还不够,收据要是这一条请求。
func TestCommittedOutcomeOf_NeedsTheRequestsOwnReceipt(t *testing.T) {
	op := testOp(1)
	ledgerWith := func(applied, partial bool, edit func(*componentpb.AssetOpReceipt)) *componentpb.PlayerAssetOpLedgerComp {
		stream := newStreamLedger(op.Stream, op.StreamEpoch, 0, 0)
		markSeen(stream, op.Seq, applied)
		if partial {
			stream.PartialSeqs = []uint64{op.Seq}
		}
		receipt := identityReceiptFixture(op)
		if edit != nil {
			edit(receipt)
		}
		stream.Receipts = []*componentpb.AssetOpReceipt{receipt}
		return &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{stream}}
	}
	const (
		applied  = assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED
		rejected = assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED
		unknown  = assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN
	)
	cases := []struct {
		name    string
		ledger  *componentpb.PlayerAssetOpLedgerComp
		outcome assetpb.AssetOpOutcome
		partial bool
		ok      bool
	}{
		{"已应用,收据是这一条请求", ledgerWith(true, false, nil), applied, false, true},
		{"部分应用", ledgerWith(true, true, nil), applied, true, true},
		{"已拒绝", ledgerWith(false, false, nil), rejected, false, true},
		{"同一个 seq 上是别的请求", ledgerWith(true, false, func(r *componentpb.AssetOpReceipt) { r.RequestDigest[0] ^= 1 }), unknown, false, false},
		{"收据的指令号不对", ledgerWith(true, false, func(r *componentpb.AssetOpReceipt) { r.CorrelationId++ }), unknown, false, false},
		{"收据缺请求摘要", ledgerWith(true, false, func(r *componentpb.AssetOpReceipt) { r.RequestDigest = nil }), unknown, false, false},
		{"账本没见过这个 seq", &componentpb.PlayerAssetOpLedgerComp{}, unknown, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, partial, ok := CommittedOutcomeOf(tc.ledger, op)
			if outcome != tc.outcome || partial != tc.partial || ok != tc.ok {
				t.Fatalf("CommittedOutcomeOf = (%v, %v, %v), want (%v, %v, %v)", outcome, partial, ok, tc.outcome, tc.partial, tc.ok)
			}
		})
	}
}
