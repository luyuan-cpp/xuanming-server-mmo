package assetop

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

// splitLedger 让 ReadPersistedLedger 与 ReadSQLCommittedLedger 各给各的账本,用来断言终结只认后者。
type splitLedger struct {
	persisted *fakeLedger
	sql       *fakeLedger
}

func (s *splitLedger) ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return s.persisted.ReadPersistedLedger(ctx, playerID)
}

func (s *splitLedger) ReadSQLCommittedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return s.sql.ReadPersistedLedger(ctx, playerID)
}

// persistedOnlyLedger 只实现 LedgerReader,没实现 SQLCommitLedgerReader:装错读取器的样子。
type persistedOnlyLedger struct{ inner *fakeLedger }

func (b persistedOnlyLedger) ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return b.inner.ReadPersistedLedger(ctx, playerID)
}

// 独立 SQL 账本事实，不由 RPC 的 durable 字段派生。
func sqlLedgerFixture(applied bool, ops ...Op) *fakeLedger {
	comp := &componentpb.PlayerAssetOpLedgerComp{}
	for _, op := range ops {
		var stream *componentpb.AssetOpStreamLedger
		for _, entry := range comp.Streams {
			if entry.Stream == op.Stream && entry.StreamEpoch == op.StreamEpoch {
				stream = entry
			}
		}
		if stream == nil {
			stream = newStreamLedger(op.Stream, op.StreamEpoch, 0, op.Seq)
			comp.Streams = append(comp.Streams, stream)
		}
		if op.Seq > stream.MaxSeq {
			stream.MaxSeq = op.Seq
		}
		markSeen(stream, op.Seq, applied)
		stream.Receipts = append(stream.Receipts, identityReceiptFixture(op))
	}
	return &fakeLedger{ledger: comp}
}

func TestSQLProofRequiredBeforeFinalizing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader *fakeLedger
		commit bool
	}{
		{"无装配", nil, false}, {"没有证据", &fakeLedger{}, false},
		{"读失败", &fakeLedger{err: errors.New("SQL 证据不可用")}, false},
		{"SQL 未见本次", sqlLedgerFixture(true, testOp(2)), false},
		{"独立 SQL 已提交", sqlLedgerFixture(true, testOp(1)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(testOp(1))
			loop, _ := newTestLoop(t, store, &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return resultApplied(true), nil }}, nil)
			if tc.reader != nil {
				loop.Ledger = tc.reader
			}
			got, err := loop.ProcessOne(context.Background(), testOp(1))
			if err != nil || got.Finalized != tc.commit {
				t.Fatalf("SQL 确认证据决定终结: got=%+v err=%v", got, err)
			}
			if got.Finalized && !got.Result.SQLCommitted {
				t.Fatal("终结的结果必须带 SQLCommitted")
			}
		})
	}
}

// 离线终结只认已提交账本:另一份账本已见本笔也不算,已提交账本见了才终结。
func TestOfflineFinalizeOnlyTrustsSQLLedger(t *testing.T) {
	op := testOp(1)
	op.Attempts = 5
	notHere := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, nil
	}}
	loop, _ := newTestLoop(t, newFakeStore(op), notHere, nil)
	ledger := &splitLedger{persisted: sqlLedgerFixture(true, op), sql: sqlLedgerFixture(true, testOp(2))}
	loop.Ledger = ledger

	got, err := loop.ProcessOne(context.Background(), op)
	if err != nil || got.Finalized {
		t.Fatalf("已提交账本未见时不得终结: %+v %v", got, err)
	}
	if ledger.persisted.reads.Load() != 0 {
		t.Fatal("离线终结不得读 ReadPersistedLedger")
	}
	ledger.sql = sqlLedgerFixture(true, op)
	got, err = loop.ProcessOne(context.Background(), op)
	if err != nil || !got.Finalized || !got.Result.SQLCommitted {
		t.Fatalf("已提交账本已见应终结并带 SQLCommitted: %+v %v", got, err)
	}
}

// 没有能读已提交账本的 Ledger:启动时拒启(fence-inflight.md F6 (b))。
func TestCheckWiringRequiresSQLLedger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ledger LedgerReader
		ok     bool
	}{
		{"没挂", nil, false},
		{"读不了已提交账本", persistedOnlyLedger{inner: &fakeLedger{}}, false},
		{"能读已提交账本", &fakeLedger{}, true},
		{"DataServiceLedger", &DataServiceLedger{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop, _ := newTestLoop(t, newFakeStore(), appliedApplier(), nil)
			if tc.ledger != nil {
				loop.Ledger = tc.ledger
			}
			if err := loop.CheckWiring(); (err == nil) != tc.ok {
				t.Fatalf("CheckWiring err=%v,期望通过=%t", err, tc.ok)
			}
		})
	}
}

func TestSQLProofCanConfirmBeforeRedisReply(t *testing.T) {
	op := testOp(1)
	loop, _ := newTestLoop(t, newFakeStore(op), &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return resultApplied(false), nil }}, nil)
	loop.Ledger = sqlLedgerFixture(true, op)
	got, err := loop.ProcessOne(context.Background(), op)
	if err != nil || !got.Finalized {
		t.Fatalf("SQL 提交独立于 Redis 回包: %+v %v", got, err)
	}
}

func TestSQLProofRetainsCompleteEscrowReceipt(t *testing.T) {
	op := testOp(1)
	op.Stream = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	op.Bundle = &assetpb.AssetBundle{ItemUuids: []uint64{99}}
	reader := sqlLedgerFixture(true, op)
	loop, _ := newTestLoop(t, newFakeStore(op), &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		res := resultApplied(true)
		res.Snapshot = &assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{{ItemUuid: 99, InstanceProto: []byte("仅RPC中的原件")}}}
		return res, nil
	}}, nil)
	loop.Ledger = reader
	got, err := loop.ProcessOne(context.Background(), op)
	if err != nil || got.Finalized {
		t.Fatalf("SQL 缺托管原件时不得终结: %+v %v", got, err)
	}
	digest, _ := ReceiptDigest(op)
	reader.ledger.Streams[0].Receipts = []*componentpb.AssetOpReceipt{{Seq: op.Seq, CorrelationId: op.CorrelationID, RequestDigest: digest[:], Snapshot: &assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{{ItemUuid: 99, InstanceProto: []byte{1, 2, 3}}}}}}
	res, err := loop.verifySQLCommit(context.Background(), op, resultApplied(true))
	if err != nil || !res.Durable || len(res.Snapshot.GetItems()) != 1 {
		t.Fatalf("原件应完整恢复: %+v %v", res, err)
	}
	for _, corrupt := range []func(*componentpb.AssetOpReceipt){
		func(r *componentpb.AssetOpReceipt) { r.RequestDigest = nil },
		func(r *componentpb.AssetOpReceipt) { r.CorrelationId++ },
		func(r *componentpb.AssetOpReceipt) { r.Snapshot.Items[0].ItemUuid++ },
		func(r *componentpb.AssetOpReceipt) { r.Snapshot.Items[0].InstanceProto = nil },
	} {
		copy := proto.Clone(reader.ledger).(*componentpb.PlayerAssetOpLedgerComp)
		corrupt(copy.Streams[0].Receipts[0])
		got := resultWithReceipt(copy, op, res)
		if got.Durable || got.Snapshot != nil {
			t.Fatal("摘要、关联或实例身份不符不能形成到账证明")
		}
	}
}

// 托管扣出的原件只认已提交账本:身份一致才终结并把原件交给 Finalize,身份不符或缺实例字节都不终结。
func TestEscrowReceiptFromSQLLedgerReachesFinalize(t *testing.T) {
	op := testOp(1)
	op.Stream = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	op.Bundle = &assetpb.AssetBundle{ItemUuids: []uint64{99}}
	for _, tc := range []struct {
		name     string
		uuid     uint64
		instance []byte
		finalize bool
	}{
		{"原件身份一致", 99, []byte{1}, true},
		{"原件身份不符", 98, []byte{1}, false},
		{"原件缺实例字节", 99, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(op)
			loop, _ := newTestLoop(t, store, appliedApplier(), nil)
			reader := sqlLedgerFixture(true, op)
			reader.ledger.Streams[0].Receipts[0].Snapshot = &assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{{ItemUuid: tc.uuid, InstanceProto: tc.instance}}}
			loop.Ledger = reader
			got, err := loop.ProcessOne(context.Background(), op)
			if err != nil || got.Finalized != tc.finalize {
				t.Fatalf("托管原件判据: %+v %v", got, err)
			}
			if tc.finalize {
				finalized, _ := store.snapshot()
				if len(finalized) != 1 || len(finalized[0].Res.Snapshot.GetItems()) != 1 {
					t.Fatalf("终结时必须带上原件: %+v", finalized)
				}
			}
		})
	}
}
