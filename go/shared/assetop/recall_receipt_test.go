package assetop

import (
	"crypto/sha256"
	"testing"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

func recallOp() Op {
	op := testOp(1)
	op.Stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL
	return op
}

func recallReceipt(op Op, snapshot *assetpb.AssetSnapshot) *componentpb.PlayerAssetOpLedgerComp {
	req := op.Request()
	req.Auth = &assetpb.AssetOpAuth{Caller: "data_service"}
	digest := sha256.Sum256(Canonical(RPCDebit, req, 0))
	return &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{{
		Stream:      op.Stream,
		StreamEpoch: op.StreamEpoch,
		Receipts:    []*componentpb.AssetOpReceipt{{Seq: op.Seq, CorrelationId: op.CorrelationID, RequestDigest: digest[:], Snapshot: snapshot}},
	}}}
}

func recallSnapshot(line *assetpb.AssetRecallLine) *assetpb.AssetSnapshot {
	return &assetpb.AssetSnapshot{RecallLines: []*assetpb.AssetRecallLine{line}}
}

// testOp 的包是一条货币:币种 1,数量 30。
func TestRecallLineOf_RequiresALineThatMatchesTheRequest(t *testing.T) {
	op := recallOp()
	cases := []struct {
		name     string
		snapshot *assetpb.AssetSnapshot
		ok       bool
	}{
		{name: "全部扣到", snapshot: recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 1, Requested: 30, Taken: 30}), ok: true},
		{name: "差额转欠款", snapshot: recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 1, Requested: 30, Taken: 10, Debt: 20}), ok: true},
		{name: "没有快照", snapshot: nil},
		{name: "没有明细", snapshot: &assetpb.AssetSnapshot{}},
		{name: "两条明细", snapshot: &assetpb.AssetSnapshot{RecallLines: []*assetpb.AssetRecallLine{
			{CurrencyType: 1, Requested: 30, Taken: 30}, {CurrencyType: 1, Requested: 30, Taken: 30}}}},
		{name: "币种对不上", snapshot: recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 2, Requested: 30, Taken: 30})},
		{name: "数量对不上", snapshot: recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 1, Requested: 31, Taken: 31})},
		{name: "总和对不上", snapshot: recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 1, Requested: 30, Taken: 10})},
		{name: "扣的比要的多", snapshot: recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 1, Requested: 30, Taken: 31})},
		{name: "货币带了道具配置号", snapshot: recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 1, ConfigId: 10, Requested: 30, Taken: 30})},
		{name: "带了实例原件", snapshot: &assetpb.AssetSnapshot{
			RecallLines: []*assetpb.AssetRecallLine{{CurrencyType: 1, Requested: 30, Taken: 30}},
			Items:       []*assetpb.AssetItemInstance{{ItemUuid: 1}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := RecallLineOf(op, tc.snapshot); ok != tc.ok {
				t.Fatalf("RecallLineOf = %t, 应为 %t", ok, tc.ok)
			}
		})
	}
}

func TestRecallLineOf_ItemLineCarriesNoDebt(t *testing.T) {
	op := recallOp()
	op.Bundle = &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 5}}, ItemUuids: []uint64{900}}
	if _, ok := RecallLineOf(op, recallSnapshot(&assetpb.AssetRecallLine{ConfigId: 10, Requested: 5, Taken: 2, Shortfall: 3})); !ok {
		t.Fatal("有缺口的道具明细也要能终结,缺口由业务层报失败")
	}
	if _, ok := RecallLineOf(op, recallSnapshot(&assetpb.AssetRecallLine{ConfigId: 10, Requested: 5, Taken: 2, Debt: 3})); ok {
		t.Fatal("道具没有欠款")
	}
}

// 回收的结局只认收据明细:没有明细不能终结,否则只能按请求金额记账。
func TestResultWithReceipt_RecallAppliedNeedsItsLine(t *testing.T) {
	op := recallOp()
	applied := Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true, SQLCommitted: true}

	proved := resultWithReceipt(recallReceipt(op, recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 1, Requested: 30, Taken: 30})), op, applied)
	if !proved.Terminal() || len(proved.Snapshot.GetRecallLines()) != 1 {
		t.Fatalf("有明细应当能终结: %+v", proved)
	}
	missing := resultWithReceipt(recallReceipt(op, nil), op, applied)
	if missing.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN {
		t.Fatalf("没有明细不能终结: %+v", missing)
	}
	wrong := resultWithReceipt(recallReceipt(op, recallSnapshot(&assetpb.AssetRecallLine{CurrencyType: 1, Requested: 30, Taken: 1})), op, applied)
	if wrong.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN {
		t.Fatalf("明细对不上不能终结: %+v", wrong)
	}
}

// 中止和拒绝没有明细,收据身份对得上就够。
func TestResultWithReceipt_RecallRejectedNeedsNoLine(t *testing.T) {
	op := recallOp()
	rejected := Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true, SQLCommitted: true}
	if proved := resultWithReceipt(recallReceipt(op, nil), op, rejected); !proved.Terminal() {
		t.Fatalf("拒绝应当能终结: %+v", proved)
	}
}

// 回收流的调用方要和 scene 的白名单一字不差,否则收据摘要对不上,所有回收都卡在未终结。
func TestReceiptDigest_RecallUsesDataServiceCaller(t *testing.T) {
	op := recallOp()
	got, ok := ReceiptDigest(op)
	if !ok {
		t.Fatal("回收流要有收据身份")
	}
	req := op.Request()
	req.Auth = &assetpb.AssetOpAuth{Caller: "data_service"}
	if want := sha256.Sum256(Canonical(RPCDebit, req, 0)); got != want {
		t.Fatal("摘要的调用方不是 data_service")
	}
}
