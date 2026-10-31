package assetop

import (
	"context"
	"crypto/sha256"
	"testing"

	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

// 与场景端共享的稳定身份信封：方向固定 debit，重投/超时中止均沿用原业务载荷。
func identityReceiptFixture(op Op) *componentpb.AssetOpReceipt {
	req := op.Request()
	caller := "guild"
	switch op.Stream {
	case assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT, assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT:
		caller = "trade"
	case assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT:
		caller = "mail"
	}
	req.Auth = &assetpb.AssetOpAuth{Caller: caller}
	digest := sha256.Sum256(Canonical(RPCDebit, req, 0))
	return &componentpb.AssetOpReceipt{Seq: op.Seq, CorrelationId: op.CorrelationID, RequestDigest: digest[:]}
}

func TestSQLProofBindsEveryOutcomeToOriginalRequest(t *testing.T) {
	for _, stream := range []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT,
	} {
		for _, applied := range []bool{true, false} {
			op := testOp(1)
			op.Stream = stream
			for _, tc := range []struct {
				name string
				edit func(*componentpb.AssetOpReceipt)
				omit bool
				want bool
			}{
				{name: "同一请求", want: true},
				{name: "旧账本只有位图", omit: true},
				{name: "缺摘要", edit: func(r *componentpb.AssetOpReceipt) { r.RequestDigest = nil }},
				{name: "摘要不符", edit: func(r *componentpb.AssetOpReceipt) { r.RequestDigest[0] ^= 1 }},
				{name: "关联不符", edit: func(r *componentpb.AssetOpReceipt) { r.CorrelationId++ }},
			} {
				name := stream.String() + "/rejected/" + tc.name
				if applied {
					name = stream.String() + "/applied/" + tc.name
				}
				t.Run(name, func(t *testing.T) {
					reader := sqlLedgerFixture(applied, op)
					reader.ledger.Streams[0].Receipts = nil
					if !tc.omit {
						r := identityReceiptFixture(op)
						if tc.edit != nil {
							tc.edit(r)
						}
						reader.ledger.Streams[0].Receipts = []*componentpb.AssetOpReceipt{r}
					}
					applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
						if applied {
							return resultApplied(true), nil
						}
						return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}, nil
					}}
					loop, _ := newTestLoop(t, newFakeStore(op), applier, nil)
					loop.Ledger = reader
					got, err := loop.ProcessOne(context.Background(), op)
					if err != nil || got.Finalized != tc.want {
						t.Fatalf("只有身份一致的SQL结局可终结: got=%+v err=%v want=%t", got, err, tc.want)
					}
					if !tc.want && (got.Result.SQLCommitted || got.Result.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN) {
						t.Fatalf("缺少身份不能声称已提交或采信位图结局: %+v", got.Result)
					}
				})
			}
		}
	}
}

func TestReceiptAckRetainsIdentityAfterSnapshotRelease(t *testing.T) {
	op := testOp(1)
	op.Stream = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	op.Bundle = &assetpb.AssetBundle{ItemUuids: []uint64{99}}
	for _, tc := range []struct {
		name string
		edit func(*componentpb.AssetOpReceipt)
		omit bool
		want bool
	}{
		{name: "身份保留且原件已释放", want: true},
		{name: "缺旧收据", omit: true},
		{name: "摘要缺失", edit: func(r *componentpb.AssetOpReceipt) { r.RequestDigest = nil }},
		{name: "关联冲突", edit: func(r *componentpb.AssetOpReceipt) { r.CorrelationId++ }},
		{name: "仍有托管原件", edit: func(r *componentpb.AssetOpReceipt) {
			r.Snapshot = &assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{{ItemUuid: 99, InstanceProto: []byte{1}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := sqlLedgerFixture(true, op)
			reader.ledger.Streams[0].Receipts = nil
			if !tc.omit {
				r := proto.Clone(identityReceiptFixture(op)).(*componentpb.AssetOpReceipt)
				if tc.edit != nil {
					tc.edit(r)
				}
				reader.ledger.Streams[0].Receipts = []*componentpb.AssetOpReceipt{r}
			}
			applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return resultApplied(true), nil }}
			done, err := AcknowledgeReceipt(context.Background(), applier, reader, op.Request())
			if err != nil || done != tc.want {
				t.Fatalf("ACK必须保留身份、仅释放原件: done=%t err=%v want=%t", done, err, tc.want)
			}
		})
	}
}

func TestSQLProofRejectsChangedPayloadForSameSequence(t *testing.T) {
	original := testOp(1)
	reader := sqlLedgerFixture(true, original)
	for _, tc := range []struct {
		name string
		edit func(*Op)
	}{
		{"金额", func(op *Op) { op.Bundle.Currencies[0].Amount++ }},
		{"币种", func(op *Op) { op.Bundle.Currencies[0].CurrencyType++ }},
		{"交易类型", func(op *Op) { op.TxType++ }},
		{"实例", func(op *Op) { op.Bundle.ItemUuids = []uint64{99} }},
		{"关联", func(op *Op) { op.CorrelationID++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := original
			op.Bundle = proto.Clone(original.Bundle).(*assetpb.AssetBundle)
			tc.edit(&op)
			loop, _ := newTestLoop(t, newFakeStore(op), appliedApplier(), nil)
			loop.Ledger = reader
			got, err := loop.ProcessOne(context.Background(), op)
			if err != nil || got.Finalized || got.Result.SQLCommitted || got.Result.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN {
				t.Fatalf("同seq改载荷不得冒认旧结局: %+v %v", got, err)
			}
		})
	}
}

func TestOfflineLegacyBitmapCannotFinalizeOrAbandon(t *testing.T) {
	op := testOp(1)
	op.Attempts = 5
	reader := sqlLedgerFixture(true, op)
	reader.ledger.Streams[0].Receipts = nil
	loop, _ := newTestLoop(t, newFakeStore(op), &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, nil
	}}, nil)
	loop.Ledger = reader
	got, err := loop.ProcessOne(context.Background(), op)
	if err != nil || got.Finalized || got.Result.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN {
		t.Fatalf("离线旧位图没有身份不能终结: %+v %v", got, err)
	}
	skip, res := seenForFinalize(reader.ledger, op)
	if skip != abandonSkipLedgerUnknown || res.Durable || res.SQLCommitted {
		t.Fatalf("已见位图不可当未见放弃或已证明终结: %v %+v", skip, res)
	}
}
