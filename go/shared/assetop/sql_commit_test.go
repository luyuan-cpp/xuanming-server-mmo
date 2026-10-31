package assetop

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

// requireSQLCommit 打开 LoopConfig.RequireSQLCommit(生产恒开),给 newTestLoop 用。
func requireSQLCommit(c *LoopConfig) { c.RequireSQLCommit = true }

// splitLedger 让 Redis 父键 blob 与 SQL 提交证据各给各的账本:开关打开时自动放弃要两份都读,
// 「证据落后于父键」(fence-inflight.md F6)只能用两份不同的账本造出来。
type splitLedger struct {
	blob *fakeLedger
	sql  *fakeLedger
}

func (s *splitLedger) ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return s.blob.ReadPersistedLedger(ctx, playerID)
}

func (s *splitLedger) ReadSQLCommittedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return s.sql.ReadPersistedLedger(ctx, playerID)
}

// blobOnlyLedger 只会读 Redis 父键 blob(没有实现 SQLCommitLedgerReader),模拟开关打开却装错了读取器。
type blobOnlyLedger struct{ inner *fakeLedger }

func (b blobOnlyLedger) ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
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
			loop, _ := newTestLoop(t, store, &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return resultApplied(true), nil }}, requireSQLCommit)
			if tc.reader != nil {
				loop.Ledger = tc.reader
			}
			got, err := loop.ProcessOne(context.Background(), testOp(1))
			if err != nil || got.Finalized != tc.commit {
				t.Fatalf("SQL 确认证据决定终结: got=%+v err=%v", got, err)
			}
			if got.Finalized && !got.Result.SQLCommitted {
				t.Fatal("开关打开时终结的结果必须带 SQLCommitted")
			}
		})
	}
}

// 开关关闭(缺省)回到 HEAD 口径:scene 回包 durable 即终结,不读 SQL 证据,结果不带 SQLCommitted。
func TestSQLProofSwitchOffKeepsHeadSemantics(t *testing.T) {
	op := testOp(1)
	store := newFakeStore(op)
	loop, _ := newTestLoop(t, store, &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return resultApplied(true), nil }}, nil)
	ledger := &splitLedger{blob: &fakeLedger{}, sql: &fakeLedger{}}
	loop.Ledger = ledger
	if err := loop.CheckWiring(); err != nil {
		t.Fatalf("开关关闭时 Ledger 可选,不得拒启: %v", err)
	}
	got, err := loop.ProcessOne(context.Background(), op)
	if err != nil || !got.Finalized || got.Status != StatusApplied {
		t.Fatalf("开关关闭时 scene durable 即终结: %+v %v", got, err)
	}
	if got.Result.SQLCommitted {
		t.Fatal("开关关闭时不得声称 SQL 已提交")
	}
	if ledger.blob.reads.Load() != 0 || ledger.sql.reads.Load() != 0 {
		t.Fatalf("在线终结不读账本: blob=%d sql=%d", ledger.blob.reads.Load(), ledger.sql.reads.Load())
	}

	// 回包 durable=false 仍按 AwaitDurable 重排,与 HEAD 一致。
	store2 := newFakeStore(op)
	loop2, _ := newTestLoop(t, store2, &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return resultApplied(false), nil }}, nil)
	loop2.Ledger = sqlLedgerFixture(true, op)
	got, err = loop2.ProcessOne(context.Background(), op)
	if err != nil || got.Finalized {
		t.Fatalf("开关关闭时未落盘的回包不得因 SQL 证据而终结: %+v %v", got, err)
	}
}

// 离线读账本按开关选来源:关闭读 Redis 父键 blob,打开只读 SQL 证据。
func TestOfflineLedgerReadFollowsSwitch(t *testing.T) {
	op := testOp(1)
	op.Attempts = 5
	notHere := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, nil
	}}
	// blob 已见、证据未见:证据落后于父键。
	newLedger := func() *splitLedger {
		return &splitLedger{blob: sqlLedgerFixture(true, op), sql: sqlLedgerFixture(true, testOp(2))}
	}

	t.Run("关闭读blob", func(t *testing.T) {
		loop, _ := newTestLoop(t, newFakeStore(op), notHere, nil)
		ledger := newLedger()
		loop.Ledger = ledger
		got, err := loop.ProcessOne(context.Background(), op)
		if err != nil || !got.Finalized || got.Result.SQLCommitted {
			t.Fatalf("关闭时按 blob 终结且不带 SQLCommitted: %+v %v", got, err)
		}
		if ledger.sql.reads.Load() != 0 {
			t.Fatal("关闭时不得读 SQL 证据")
		}
	})
	t.Run("打开只认证据", func(t *testing.T) {
		loop, _ := newTestLoop(t, newFakeStore(op), notHere, requireSQLCommit)
		ledger := newLedger()
		loop.Ledger = ledger
		got, err := loop.ProcessOne(context.Background(), op)
		if err != nil || got.Finalized {
			t.Fatalf("证据未见时不得凭 blob 终结: %+v %v", got, err)
		}
		if ledger.blob.reads.Load() != 0 {
			t.Fatal("打开时离线终结不得读 blob")
		}
		ledger.sql = sqlLedgerFixture(true, op)
		got, err = loop.ProcessOne(context.Background(), op)
		if err != nil || !got.Finalized || !got.Result.SQLCommitted {
			t.Fatalf("证据已见应终结并带 SQLCommitted: %+v %v", got, err)
		}
	})
}

// 开关打开却没有能读 SQL 证据的 Ledger:启动时拒启(fence-inflight.md F6 (b))。
func TestCheckWiringRequiresSQLLedgerWhenSwitchOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ledger LedgerReader
		ok     bool
	}{
		{"没挂", nil, false},
		{"只会读blob", blobOnlyLedger{inner: &fakeLedger{}}, false},
		{"能读SQL证据", &fakeLedger{}, true},
		{"DataServiceLedger", &DataServiceLedger{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop, _ := newTestLoop(t, newFakeStore(), appliedApplier(), requireSQLCommit)
			if tc.ledger != nil {
				loop.Ledger = tc.ledger
			}
			if err := loop.CheckWiring(); (err == nil) != tc.ok {
				t.Fatalf("CheckWiring err=%v,期望通过=%t", err, tc.ok)
			}
		})
	}
	loop, _ := newTestLoop(t, newFakeStore(), appliedApplier(), nil)
	if err := loop.CheckWiring(); err != nil {
		t.Fatalf("开关关闭时没挂 Ledger 也合法(HEAD 口径): %v", err)
	}
}

func TestSQLProofCanConfirmBeforeRedisReply(t *testing.T) {
	op := testOp(1)
	loop, _ := newTestLoop(t, newFakeStore(op), &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return resultApplied(false), nil }}, requireSQLCommit)
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
	}}, requireSQLCommit)
	loop.Ledger = reader
	got, err := loop.ProcessOne(context.Background(), op)
	if err != nil || got.Finalized {
		t.Fatalf("SQL 缺托管原件时不得终结: %+v %v", got, err)
	}
	digest, _ := receiptDigest(op)
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

// 开关关闭时托管扣出照 HEAD 口径信 scene 回包,但回包里的原件必须与本笔实例身份一致,否则不终结。
func TestEscrowReceiptCheckedWhenSwitchOff(t *testing.T) {
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
			loop, _ := newTestLoop(t, store, &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
				res := resultApplied(true)
				res.Snapshot = &assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{{ItemUuid: tc.uuid, InstanceProto: tc.instance}}}
				return res, nil
			}}, nil)
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
