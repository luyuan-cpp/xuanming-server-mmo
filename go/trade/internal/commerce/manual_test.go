package commerce

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"trade/internal/data"
)

// 人工恢复(manual.go)的纯函数用例:不读库,只看合同和腿的结局。

const (
	outcomeApplied  = uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED)
	outcomeRejected = uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED)
)

func currencyBundle(amount uint64) *assetpb.AssetBundle {
	return &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: amount}}}
}

func manualLeg(role string, player uint64, stream uint32, opID uint64, outcome uint32, bundle *assetpb.AssetBundle) *tradepb.TradeCommerceLeg {
	return &tradepb.TradeCommerceLeg{Role: role, PlayerId: player, Stream: stream, OpId: opID, Outcome: outcome, Bundle: bundle}
}

// deliveredTrade 是一份双方交易:两边都扣到了,交付时卖家收款到账、买家收货被拒。
func deliveredTrade() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 9, Kind: 2, Status: Delivering, SellerPlayerId: 1, BuyerPlayerId: 2, Version: 5}
	w := &tradepb.TradeCommerceWorkflow{Stage: "deliver", Legs: []*tradepb.TradeCommerceLeg{
		manualLeg("seller_debit", 1, 3, 101, outcomeApplied, currencyBundle(10)),
		manualLeg("buyer_debit", 2, 3, 102, outcomeApplied, currencyBundle(20)),
		manualLeg("seller_receive", 1, 4, 103, outcomeApplied, currencyBundle(20)),
		manualLeg("buyer_receive", 2, 4, 104, outcomeRejected, currencyBundle(10)),
	}}
	return r, w
}

// pendingConsignment 是一份寄售:卖家货和买家款都已扣进托管,还没交付。
func pendingConsignment() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 10, Kind: 3, Status: Escrowing, SellerPlayerId: 1, BuyerPlayerId: 2, Version: 3}
	w := &tradepb.TradeCommerceWorkflow{Stage: "pay", QuantityReserved: 1, Legs: []*tradepb.TradeCommerceLeg{
		manualLeg("seller_debit", 1, 3, 201, outcomeApplied, currencyBundle(10)),
		manualLeg("buyer_debit_1_k", 2, 3, 202, outcomeApplied, currencyBundle(30)),
	}}
	return r, w
}

func requireNotAllowed(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrManualNotAllowed), "应当是前置条件不满足: %v", err)
}

// 最终发放没到账:转人工,原阶段和原因记进 workflow;stage 不是 done,op 行不会被标业务消费。
// 旧代码 stage 写成 done、只改 status,原阶段丢失,op 行随即被标消费。
func TestPlanFinalCreditFailureRecordsQuarantine(t *testing.T) {
	r, w := deliveredTrade()
	require.NoError(t, (&Engine{}).plan(r, w, 1000))

	require.Equal(t, Manual, r.Status)
	require.Equal(t, "manual", w.Stage)
	require.Equal(t, "deliver", w.Quarantine.GetStage())
	require.Equal(t, Delivering, w.Quarantine.GetStatus())
	require.Equal(t, manualReasonFinalCredit, w.Quarantine.GetReason())
	require.Equal(t, uint64(1000), w.Quarantine.GetAtMs())
}

func TestMarkManualKeepsTheFirstRecord(t *testing.T) {
	r := &tradepb.TradeCommerceRecord{Status: Escrowing}
	w := &tradepb.TradeCommerceWorkflow{Stage: "escrow"}
	markManual(r, w, manualReasonCorrupt, "第一次", 1)
	markManual(r, w, manualReasonMarketFill, "第二次", 2)

	require.Equal(t, "escrow", w.Quarantine.Stage)
	require.Equal(t, manualReasonCorrupt, w.Quarantine.Reason)
	require.Equal(t, uint64(1), w.Quarantine.AtMs)
}

func TestMarkManualTruncatesDetailOnRuneBoundary(t *testing.T) {
	r := &tradepb.TradeCommerceRecord{}
	w := &tradepb.TradeCommerceWorkflow{Stage: "escrow"}
	long := ""
	for len(long) < quarantineDetailMaxBytes+10 {
		long += "坏"
	}
	markManual(r, w, manualReasonCorrupt, long, 1)
	require.LessOrEqual(t, len(w.Quarantine.Detail), quarantineDetailMaxBytes)
	require.True(t, len(w.Quarantine.Detail) > 0 && len(w.Quarantine.Detail)%len("坏") == 0, "不能截在一个字中间")
}

// 补发:没到账的腿各补一条,原腿留作证据并标上被谁取代;补发腿到账后合同按原流程结案。
func TestReissueSupersedesFailedCreditAndSettlesWhenItArrives(t *testing.T) {
	r, w := deliveredTrade()
	require.NoError(t, (&Engine{}).plan(r, w, 1000))

	result, err := planManualAction(r, w, ManualReissue)
	require.NoError(t, err)
	require.Equal(t, []string{"buyer_receive#2"}, result.NewLegs)
	require.Equal(t, []string{"buyer_receive"}, result.SupersededLegs)
	require.Equal(t, "deliver", w.Stage)
	require.Equal(t, Delivering, r.Status)

	reissued := leg(w, "buyer_receive#2")
	require.NotNil(t, reissued)
	require.Zero(t, reissued.OpId, "新腿由引擎发号")
	require.Equal(t, uint64(2), reissued.PlayerId)
	require.Equal(t, uint32(4), reissued.Stream)
	require.True(t, proto.Equal(currencyBundle(10), reissued.Bundle))
	require.Equal(t, "buyer_receive#2", leg(w, "buyer_receive").SupersededBy)

	reissued.OpId, reissued.Outcome = 105, outcomeApplied
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, Settled, r.Status)
	require.Equal(t, "done", w.Stage)
}

// 补发又被拒:再次转人工,再补发时序号加一,currentLeg 沿取代关系找到最新的一条。
func TestReissueTwiceChainsRoles(t *testing.T) {
	r, w := deliveredTrade()
	require.NoError(t, (&Engine{}).plan(r, w, 1000))
	_, err := planManualAction(r, w, ManualReissue)
	require.NoError(t, err)
	second := leg(w, "buyer_receive#2")
	second.OpId, second.Outcome = 105, outcomeRejected
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, Manual, r.Status)

	result, err := planManualAction(r, w, ManualReissue)
	require.NoError(t, err)
	require.Equal(t, []string{"buyer_receive#3"}, result.NewLegs)
	require.Equal(t, "buyer_receive#3", currentLeg(w, "buyer_receive").Role)
}

func TestReissueRoleSkipsExistingNames(t *testing.T) {
	w := &tradepb.TradeCommerceWorkflow{Legs: []*tradepb.TradeCommerceLeg{{Role: "a#3"}}}
	require.Equal(t, "a#2", reissueRole(w, "a"))
	require.Equal(t, "a#4", reissueRole(w, "a#2"))
	require.Equal(t, "x#y#2", reissueRole(w, "x#y"))
}

// 人民币寄售退回没到账:补发退回腿,回到原阶段;原阶段的判定沿 currentLeg 找到补发腿。
func TestReissueRmbReturn(t *testing.T) {
	r := &tradepb.TradeCommerceRecord{Kind: 6, Status: CancelPending, SellerPlayerId: 1}
	w := &tradepb.TradeCommerceWorkflow{Stage: "rmb_return", Legs: []*tradepb.TradeCommerceLeg{
		manualLeg("seller_debit", 1, 3, 1, outcomeApplied, currencyBundle(5)),
		manualLeg("seller_return", 1, 4, 2, outcomeRejected, currencyBundle(5)),
	}}
	markManual(r, w, manualReasonRmbReturn, "退回没到账", 1)

	_, err := planManualAction(proto.Clone(r).(*tradepb.TradeCommerceRecord), proto.Clone(w).(*tradepb.TradeCommerceWorkflow), ManualResume)
	requireNotAllowed(t, err)

	result, err := planManualAction(r, w, ManualReissue)
	require.NoError(t, err)
	require.Equal(t, []string{"seller_return#2"}, result.NewLegs)
	require.Equal(t, "rmb_return", w.Stage)
	require.Equal(t, "seller_return#2", currentLeg(w, "seller_return").Role)
}

func TestReissueOnlyForFinalCreditFailures(t *testing.T) {
	r, w := pendingConsignment()
	markManual(r, w, manualReasonCorrupt, "坏数据", 1)
	_, err := planManualAction(r, w, ManualReissue)
	requireNotAllowed(t, err)
}

// 恢复推进:回到原阶段和原状态;最终发放没到账的合同不能恢复,重新判定还是转人工。
func TestResumeRestoresStageAndStatus(t *testing.T) {
	r, w := pendingConsignment()
	markManual(r, w, manualReasonCorrupt, "坏数据", 1)

	result, err := planManualAction(r, w, ManualResume)
	require.NoError(t, err)
	require.Equal(t, "pay", w.Stage)
	require.Equal(t, Escrowing, r.Status)
	require.Empty(t, result.NewLegs)

	r2, w2 := deliveredTrade()
	require.NoError(t, (&Engine{}).plan(r2, w2, 1000))
	_, err = planManualAction(r2, w2, ManualResume)
	requireNotAllowed(t, err)
}

// 整单退回:每条已扣出的托管各退一条给扣出它的人,合同按取消结案。
func TestRefundReturnsEveryAppliedDebitAndCancels(t *testing.T) {
	r, w := pendingConsignment()
	markManual(r, w, manualReasonCorrupt, "坏数据", 1)

	result, err := planManualAction(r, w, ManualRefund)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"seller_debit/refund", "buyer_debit_1_k/refund"}, result.NewLegs)
	require.Equal(t, "return", w.Stage)
	require.Equal(t, CancelPending, r.Status)
	require.Equal(t, uint64(1), leg(w, "seller_debit/refund").PlayerId)
	require.Equal(t, uint64(2), leg(w, "buyer_debit_1_k/refund").PlayerId)
	require.True(t, proto.Equal(currencyBundle(30), leg(w, "buyer_debit_1_k/refund").Bundle))

	for i, role := range []string{"seller_debit/refund", "buyer_debit_1_k/refund"} {
		l := leg(w, role)
		l.OpId, l.Outcome = uint64(300+i), outcomeApplied
	}
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, Cancelled, r.Status)
	require.Equal(t, "done", w.Stage)
}

// 退回会和别处的记账冲突的情形一律拒绝,交给人判定。
func TestRefundRefusals(t *testing.T) {
	cases := map[string]func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow){
		"已有发放到账": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := deliveredTrade()
			markManual(r, w, manualReasonFinalCredit, "", 1)
			return r, w
		},
		"竞价": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := pendingConsignment()
			r.Kind = BidKind
			markManual(r, w, manualReasonCorrupt, "", 1)
			return r, w
		},
		"市场单部分成交": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := pendingConsignment()
			r.Kind, r.Side, r.Quantity, r.FilledQuantity = 4, 1, 5, 2
			markManual(r, w, manualReasonCorrupt, "", 1)
			return r, w
		},
		"成交子单": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := pendingConsignment()
			r.Kind, r.Side = 4, 0
			markManual(r, w, manualReasonCorrupt, "", 1)
			return r, w
		},
		"拍品已有出价": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := pendingConsignment()
			r.Kind, w.HighestBidder = 5, 7
			markManual(r, w, manualReasonCorrupt, "", 1)
			return r, w
		},
		"实例托管没有原件": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := pendingConsignment()
			w.Legs[0].Bundle = &assetpb.AssetBundle{ItemUuids: []uint64{77}}
			markManual(r, w, manualReasonCorrupt, "", 1)
			return r, w
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			r, w := build()
			before := proto.Clone(w)
			_, err := planManualAction(r, w, ManualRefund)
			requireNotAllowed(t, err)
			if name != "实例托管没有原件" {
				require.True(t, proto.Equal(before, w), "拒绝时不改合同")
			}
		})
	}
}

func TestRefundRespectsTheEngineStepLimit(t *testing.T) {
	r, w := pendingConsignment()
	for i := 0; i < maxNewLegsPerStep; i++ {
		w.Legs = append(w.Legs, manualLeg("buyer_debit_extra_"+string(rune('a'+i)), 2, 3, uint64(400+i), outcomeApplied, currencyBundle(1)))
	}
	markManual(r, w, manualReasonCorrupt, "", 1)
	_, err := planManualAction(r, w, ManualRefund)
	requireNotAllowed(t, err)
}

func TestPlanManualActionRequiresManualAndRecord(t *testing.T) {
	r, w := pendingConsignment()
	_, err := planManualAction(r, w, ManualResume)
	requireNotAllowed(t, err)

	r.Status, w.Stage = Manual, "manual"
	_, err = planManualAction(r, w, ManualResume)
	requireNotAllowed(t, err)
}

// 补记结局:与 advance 同一判定;在途、查不到 outbox 行、部分发放、快照损坏都拒绝。
func TestSettleLegOutcomes(t *testing.T) {
	newWorkflow := func() *tradepb.TradeCommerceWorkflow {
		return &tradepb.TradeCommerceWorkflow{Legs: []*tradepb.TradeCommerceLeg{manualLeg("seller_debit", 1, 3, 11, 0, currencyBundle(1))}}
	}
	reader := func(out data.LegOutcome, err error) func(context.Context, uint64) (data.LegOutcome, error) {
		return func(context.Context, uint64) (data.LegOutcome, error) { return out, err }
	}
	status := func(s tradepb.TradeAssetOpStatus) data.LegOutcome { return data.LegOutcome{Status: s} }

	w := newWorkflow()
	require.NoError(t, settleLegOutcomes(context.Background(), w, reader(status(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED), nil)))
	require.Equal(t, uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED), w.Legs[0].Outcome)

	refused := map[string]func(context.Context, uint64) (data.LegOutcome, error){
		"在途":   reader(status(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING), nil),
		"部分发放": reader(status(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL), nil),
		"行不存在": reader(data.LegOutcome{}, sql.ErrNoRows),
		"状态非法": reader(status(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED), nil),
		"快照损坏": reader(data.LegOutcome{Status: tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, Snapshot: []byte{0xff}}, nil),
	}
	for name, read := range refused {
		t.Run(name, func(t *testing.T) {
			requireNotAllowed(t, settleLegOutcomes(context.Background(), newWorkflow(), read))
		})
	}

	unsent := &tradepb.TradeCommerceWorkflow{Legs: []*tradepb.TradeCommerceLeg{manualLeg("seller_return", 1, 4, 0, 0, currencyBundle(1))}}
	requireNotAllowed(t, settleLegOutcomes(context.Background(), unsent, reader(data.LegOutcome{}, nil)))

	storageErr := errors.New("boom")
	err := settleLegOutcomes(context.Background(), newWorkflow(), reader(data.LegOutcome{}, storageErr))
	require.ErrorIs(t, err, storageErr)
	require.False(t, errors.Is(err, ErrManualNotAllowed), "存储错误不是前置条件")
}

func TestManualRequestValidate(t *testing.T) {
	valid := ManualRequest{CommerceID: 1, ExpectVersion: 2, Action: ManualRefund, Operator: "ops", Reason: "ticket-1"}
	require.NoError(t, valid.validate())

	for name, change := range map[string]func(*ManualRequest){
		"缺合同":    func(q *ManualRequest) { q.CommerceID = 0 },
		"缺版本":    func(q *ManualRequest) { q.ExpectVersion = 0 },
		"未知动作":   func(q *ManualRequest) { q.Action = "delete" },
		"缺操作人":   func(q *ManualRequest) { q.Operator = " " },
		"操作人带换行": func(q *ManualRequest) { q.Operator = "a\nb" },
		"缺依据":    func(q *ManualRequest) { q.Reason = "" },
	} {
		t.Run(name, func(t *testing.T) {
			q := valid
			change(&q)
			requireNotAllowed(t, q.validate())
		})
	}
}
