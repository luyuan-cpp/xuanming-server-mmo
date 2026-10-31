package assetoptest

import (
	"context"
	"testing"

	assetpb "proto/common/asset"

	"shared/assetop"
)

// 替身记下的结局必须能过 Loop 的终结核对:位图给结局,收据绑定请求身份。
func TestCommittedLedgerProvesCommittedOutcome(t *testing.T) {
	op := assetop.Op{
		OpID:          1,
		PlayerID:      42,
		Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
		Seq:           3,
		StreamEpoch:   7,
		CorrelationID: 1,
		Bundle:        &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 30}}},
	}
	store := &singleRowStore{op: op}
	applier := &committingApplier{res: assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}}
	loop, err := assetop.NewLoop(assetop.DefaultLoopConfig(), store, applier, nil, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	loop.Ledger = &applier.ledger
	if err := loop.CheckWiring(); err != nil {
		t.Fatalf("替身必须过装配检查: %v", err)
	}
	got, err := loop.ProcessOne(context.Background(), op)
	if err != nil || !got.Finalized || !got.Result.SQLCommitted {
		t.Fatalf("已记账的结局应当终结并带 SQLCommitted: %+v %v", got, err)
	}
}

// 没落盘的结局不留痕,读到的还是「没有写权行」。
func TestCommittedLedgerIgnoresNonTerminal(t *testing.T) {
	var ledger CommittedLedger
	req := &assetpb.AssetOpRequest{PlayerId: 42, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, Seq: 1, StreamEpoch: 7, CorrelationId: 1}
	if err := ledger.Commit(req, assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := ledger.ReadSQLCommittedLedger(context.Background(), 42)
	if err != nil || got != nil {
		t.Fatalf("未落盘的结局不该留痕: %v %v", got, err)
	}
}

// 已记账的结局不变,同一 seq 也不多出第二张收据(多一张 Loop 就不认了)。
func TestCommittedLedgerKeepsFirstOutcome(t *testing.T) {
	var ledger CommittedLedger
	req := &assetpb.AssetOpRequest{PlayerId: 42, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, Seq: 1, StreamEpoch: 7, CorrelationId: 1}
	applied := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
	rejected := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}
	for _, res := range []assetop.Result{applied, rejected, applied} {
		if err := ledger.Commit(req, res); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	got, err := ledger.ReadSQLCommittedLedger(context.Background(), 42)
	if err != nil {
		t.Fatalf("读账本: %v", err)
	}
	outcome, _, _ := assetop.ClassifyPersisted(got, req.GetStream(), req.GetStreamEpoch(), req.GetSeq())
	if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED {
		t.Fatalf("结局应保持第一次的 APPLIED,实际 %v", outcome)
	}
	if n := len(assetop.FindStream(got, req.GetStream()).GetReceipts()); n != 1 {
		t.Fatalf("同一 seq 只该有一张收据,实际 %d", n)
	}
}

// 夹具写错要报出来,不能静默丢掉结局。
func TestCommittedLedgerRejectsBadRequest(t *testing.T) {
	var ledger CommittedLedger
	applied := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
	for name, req := range map[string]*assetpb.AssetOpRequest{
		"缺关联号":    {PlayerId: 42, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, Seq: 1, StreamEpoch: 7},
		"未知流":     {PlayerId: 42, Seq: 1, StreamEpoch: 7, CorrelationId: 1},
		"seq 超窗口": {PlayerId: 42, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, Seq: assetop.WindowBits + 1, StreamEpoch: 7, CorrelationId: 1},
	} {
		if err := ledger.Commit(req, applied); err == nil {
			t.Errorf("%s: 应回错误", name)
		}
	}
}

// committingApplier 是假 scene:回固定结局,并把结局记进自己的账本。
type committingApplier struct {
	res    assetop.Result
	ledger CommittedLedger
}

func (a *committingApplier) Do(_ context.Context, _ assetop.RPC, req *assetpb.AssetOpRequest) (assetop.Result, error) {
	if err := a.ledger.Commit(req, a.res); err != nil {
		return assetop.Result{}, err
	}
	return a.res, nil
}

// singleRowStore 只够 ProcessOne 走完:终结回本次终结成功,其余不会被调用。
type singleRowStore struct{ op assetop.Op }

func (s *singleRowStore) ListDue(context.Context, uint64, int) ([]uint64, error) {
	return []uint64{s.op.OpID}, nil
}

func (s *singleRowStore) Claim(context.Context, uint64, uint64, uint64, uint64, uint64) (assetop.Op, bool, error) {
	return s.op, true, nil
}

func (s *singleRowStore) Finalize(context.Context, assetop.Op, assetop.Status, assetop.Result, uint64) (bool, error) {
	return true, nil
}

func (s *singleRowStore) Reschedule(context.Context, assetop.Op, uint64, assetop.Result, uint64) error {
	return nil
}
