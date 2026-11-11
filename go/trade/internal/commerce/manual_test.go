package commerce

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"trade/internal/data"
)

// 人工处置(manual.go、manual_plan.go)的纯函数用例:不读库,只看合同、腿的结局、两方归属与关联合同。
// 每类情形的处置见 docs/design/jubaozhai-market.md §15.7.1。

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

func identity(account string, revision uint64) *tradepb.TradeRoleIdentity {
	return &tradepb.TradeRoleIdentity{Account: account, OwnerRevision: revision}
}

// planAction 用不带参数的请求算计划。
func planAction(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, action ManualAction) (ManualResult, error) {
	return planManualAction(r, w, manualContext{req: ManualRequest{Action: action}})
}

// deliveredTrade 是一份双方交易:两边都扣到了,交付时卖家收款到账、买家收货被拒。
func deliveredTrade() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 9, Kind: 2, Status: Delivering, SellerPlayerId: 1, BuyerPlayerId: 2, Version: 5}
	w := &tradepb.TradeCommerceWorkflow{Stage: "deliver", SellerIdentity: identity("a1", 1), BuyerIdentity: identity("a2", 1), Legs: []*tradepb.TradeCommerceLeg{
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
	w := &tradepb.TradeCommerceWorkflow{Stage: "pay", QuantityReserved: 1, SellerIdentity: identity("a1", 1), BuyerIdentity: identity("a2", 1), Legs: []*tradepb.TradeCommerceLeg{
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
	markManual(r, w, manualReasonFinalCredit, "第二次", 2)

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

	result, err := planAction(r, w, ManualReissue)
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
	_, err := planAction(r, w, ManualReissue)
	require.NoError(t, err)
	second := leg(w, "buyer_receive#2")
	second.OpId, second.Outcome = 105, outcomeRejected
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, Manual, r.Status)

	result, err := planAction(r, w, ManualReissue)
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

	_, err := planAction(proto.Clone(r).(*tradepb.TradeCommerceRecord), proto.Clone(w).(*tradepb.TradeCommerceWorkflow), ManualResume)
	requireNotAllowed(t, err)

	result, err := planAction(r, w, ManualReissue)
	require.NoError(t, err)
	require.Equal(t, []string{"seller_return#2"}, result.NewLegs)
	require.Equal(t, "rmb_return", w.Stage)
	require.Equal(t, "seller_return#2", currentLeg(w, "seller_return").Role)
}

func TestReissueOnlyForFinalCreditFailures(t *testing.T) {
	r, w := pendingConsignment()
	markManual(r, w, manualReasonCorrupt, "坏数据", 1)
	_, err := planAction(r, w, ManualReissue)
	requireNotAllowed(t, err)
}

// 恢复推进:回到原阶段和原状态;最终发放没到账的合同不能恢复,重新判定还是转人工。
func TestResumeRestoresStageAndStatus(t *testing.T) {
	r, w := pendingConsignment()
	markManual(r, w, manualReasonCorrupt, "坏数据", 1)

	result, err := planAction(r, w, ManualResume)
	require.NoError(t, err)
	require.Equal(t, "pay", w.Stage)
	require.Equal(t, Escrowing, r.Status)
	require.Empty(t, result.NewLegs)

	r2, w2 := deliveredTrade()
	require.NoError(t, (&Engine{}).plan(r2, w2, 1000))
	_, err = planAction(r2, w2, ManualResume)
	requireNotAllowed(t, err)
}

// 整单退回:每条已扣出的托管各退一条给扣出它的人,合同按取消结案。
func TestRefundReturnsEveryAppliedDebitAndCancels(t *testing.T) {
	r, w := pendingConsignment()
	markManual(r, w, manualReasonCorrupt, "坏数据", 1)

	result, err := planAction(r, w, ManualRefund)
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

// 2026-11-11 修的缺陷:退回没到账转人工之后再跑一次 refund,旧代码把没到账的退回腿标「被退回取代」、同名的新退回腿又
// 加不上,合同按「没有发放腿」直接结案,托管再也不退。现在拒绝,退回没到账用 reissue。
func TestRefundTwiceIsRefusedAndReissueSendsTheReturnAgain(t *testing.T) {
	r, w := pendingConsignment()
	markManual(r, w, manualReasonCorrupt, "坏数据", 1)
	_, err := planAction(r, w, ManualRefund)
	require.NoError(t, err)
	for i, role := range []string{"seller_debit/refund", "buyer_debit_1_k/refund"} {
		l := leg(w, role)
		l.OpId, l.Outcome = uint64(300+i), outcomeRejected
	}
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, Manual, r.Status, "退回没到账:转人工,不能按取消结案")
	require.Equal(t, manualReasonFinalCredit, w.Quarantine.GetReason())

	before := proto.Clone(w)
	_, err = planAction(r, w, ManualRefund)
	requireNotAllowed(t, err)
	require.True(t, proto.Equal(before, w), "拒绝时不改合同")

	result, err := planAction(r, w, ManualReissue)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"seller_debit/refund#2", "buyer_debit_1_k/refund#2"}, result.NewLegs)
	require.Equal(t, "return", w.Stage)
}

// 退回会和别处的记账冲突的情形一律拒绝,交给对应的动作或人工判定。
func TestRefundRefusals(t *testing.T) {
	cases := map[string]func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow){
		"已有发放到账": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := deliveredTrade()
			markManual(r, w, manualReasonFinalCredit, "", 1)
			return r, w
		},
		"市场单全部成交": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := pendingConsignment()
			r.Kind, r.Side, r.Quantity, r.FilledQuantity = 4, 1, 5, 5
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
		"官方商城": func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
			r, w := pendingConsignment()
			r.Kind = 1
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
			_, err := planAction(r, w, ManualRefund)
			requireNotAllowed(t, err)
			require.True(t, proto.Equal(before, w), "拒绝时不改合同")
		})
	}
}

func TestRefundRespectsTheEngineStepLimit(t *testing.T) {
	r, w := pendingConsignment()
	for i := 0; i < maxNewLegsPerStep; i++ {
		w.Legs = append(w.Legs, manualLeg("buyer_debit_extra_"+string(rune('a'+i)), 2, 3, uint64(400+i), outcomeApplied, currencyBundle(1)))
	}
	markManual(r, w, manualReasonCorrupt, "", 1)
	_, err := planAction(r, w, ManualRefund)
	requireNotAllowed(t, err)
}

func TestPlanManualActionRequiresManualAndRecord(t *testing.T) {
	r, w := pendingConsignment()
	_, err := planAction(r, w, ManualResume)
	requireNotAllowed(t, err)

	r.Status, w.Stage = Manual, "manual"
	_, err = planAction(r, w, ManualResume)
	requireNotAllowed(t, err)
}

// ── 第 4 类:已部分成交的市场单 ──────────────────────────────────

// marketOrder 是一张转人工的市场母单:托管全部数量,已成交 2 张。
func marketOrder(side uint32) (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	bundle := &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 5}}}
	if side == 1 {
		bundle = currencyBundle(5 * 12)
	}
	r := &tradepb.TradeCommerceRecord{CommerceId: 40, Kind: 4, Side: side, Status: Escrowing, SellerPlayerId: 1, ItemConfigId: 10,
		UnitPrice: 12, Quantity: 5, FilledQuantity: 2, CurrencyType: 1, Version: 9}
	w := &tradepb.TradeCommerceWorkflow{Stage: "market_open", SellerIdentity: identity("a1", 1),
		Legs: []*tradepb.TradeCommerceLeg{manualLeg("seller_debit", 1, 3, 401, outcomeApplied, bundle)}}
	markManual(r, w, manualReasonBadCounterparty, "", 1)
	return r, w
}

// 已部分成交的母单只退剩余部分(与玩家撤单同一个算法):成交的部分已划给成交子单,由子单的腿交付。
func TestRefundOfPartiallyFilledMarketOrderReturnsOnlyTheRemainder(t *testing.T) {
	r, w := marketOrder(2)
	result, err := planAction(r, w, ManualRefund)
	require.NoError(t, err)
	require.Equal(t, []string{"seller_debit/refund"}, result.NewLegs)
	require.True(t, proto.Equal(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 3}}}, leg(w, "seller_debit/refund").Bundle), "卖单退剩余 3 件")
	require.Equal(t, "return", w.Stage)

	r, w = marketOrder(1)
	_, err = planAction(r, w, ManualRefund)
	require.NoError(t, err)
	require.True(t, proto.Equal(currencyBundle(3*12), leg(w, "seller_debit/refund").Bundle), "买单退剩余 3 件 × 单价")
}

func TestMarketRemainder(t *testing.T) {
	order := &tradepb.TradeCommerceRecord{Kind: 4, Side: 2, ItemConfigId: 10, UnitPrice: 7, Quantity: 4, FilledQuantity: 1, CurrencyType: 1}
	b, err := marketRemainder(order)
	require.NoError(t, err)
	require.True(t, proto.Equal(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 3}}}, b))

	order.Side = 1
	b, err = marketRemainder(order)
	require.NoError(t, err)
	require.True(t, proto.Equal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 21}}}, b))

	order.FilledQuantity = 4
	b, err = marketRemainder(order)
	require.NoError(t, err)
	require.True(t, emptyBundle(b), "全部成交没有剩余")

	order.FilledQuantity = 5
	_, err = marketRemainder(order)
	require.Error(t, err, "已成交量超过总量不能用回绕的减法算出一笔巨款")
}

// ── 第 3 类:竞价与拍品互相引用 ─────────────────────────────────

// heldBid 是一条转人工的出价:冻结款已扣到。
func heldBid(stage string) (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 70, Kind: BidKind, Status: Escrowing, SellerPlayerId: 7, ParentId: 60, UnitPrice: 80, CurrencyType: 1, Version: 4}
	w := &tradepb.TradeCommerceWorkflow{Stage: stage, SellerIdentity: identity("a7", 1),
		Legs: []*tradepb.TradeCommerceLeg{manualLeg("bid_debit", 7, 3, 701, outcomeApplied, currencyBundle(80))}}
	return r, w
}

// openLot 是一件有出价的拍品:卖家托管已扣到,最高价出价 70。
func openLot() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 60, Kind: 5, Status: Escrowing, SellerPlayerId: 1, BuyerPlayerId: 7, UnitPrice: 50, CurrencyType: 1, Version: 8}
	w := &tradepb.TradeCommerceWorkflow{Stage: "lot_open", SellerIdentity: identity("a1", 1), BuyerIdentity: identity("a7", 1),
		HighestBid: 80, HighestBidder: 7, WinningBidId: 70, BuyerAssets: &tradepb.TradeAssetSelection{CurrencyType: 1, CurrencyAmount: 80},
		Legs: []*tradepb.TradeCommerceLeg{manualLeg("seller_debit", 1, 3, 601, outcomeApplied, &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 1}}})}}
	return r, w
}

// 出价能不能单独退回,只看拍品是否引用它:被引用的冻结款归拍品结算,没被引用的只属于出价人自己。
func TestBidRefundDependsOnTheLotReference(t *testing.T) {
	plan := func(lot *tradepb.TradeCommerceRecord, lotW *tradepb.TradeCommerceWorkflow) error {
		r, w := heldBid("bid_ready")
		markManual(r, w, manualReasonCorrupt, "", 1)
		_, err := planManualAction(r, w, manualContext{req: ManualRequest{Action: ManualRefund}, lot: lot, lotW: lotW})
		return err
	}
	require.NoError(t, plan(nil, nil), "拍品不存在:不会引用它")

	lot, lotW := openLot()
	requireNotAllowed(t, plan(lot, lotW))

	lotW.WinningBidId = 71
	require.NoError(t, plan(lot, lotW), "拍品引用的是另一条出价")

	requireNotAllowed(t, plan(lot, nil))

	r, w := heldBid("bid_ready")
	markManual(r, w, manualReasonCorrupt, "", 1)
	result, err := planManualAction(r, w, manualContext{req: ManualRequest{Action: ManualRefund}})
	require.NoError(t, err)
	require.Equal(t, []string{"bid_debit/refund"}, result.NewLegs)
	require.Equal(t, uint64(7), leg(w, "bid_debit/refund").PlayerId)
}

// 作废拍卖:拍品托管退回卖家、按取消结案;它引用的出价改 bid_refund,由引擎退回冻结款。两张合同一起改。
func TestVoidAuctionRefundsLotAndBidTogether(t *testing.T) {
	lot, lotW := openLot()
	lotW.Stage = "deliver"
	lot.Status = Delivering
	lotW.Legs = append(lotW.Legs,
		manualLeg("buyer_receive", 7, 4, 602, outcomeRejected, &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 1}}}),
		manualLeg("seller_receive", 1, 4, 603, outcomeRejected, currencyBundle(80)))
	markManual(lot, lotW, manualReasonFinalCredit, "", 1)
	bid, bidW := heldBid("bid_held")

	result, err := planManualAction(lot, lotW, manualContext{req: ManualRequest{Action: ManualVoidAuction}, bid: bid, bidW: bidW})
	require.NoError(t, err)
	require.Equal(t, []string{"seller_debit/refund"}, result.NewLegs)
	require.ElementsMatch(t, []string{"buyer_receive", "seller_receive"}, result.SupersededLegs)
	require.Equal(t, "return", lotW.Stage)
	require.Equal(t, CancelPending, lot.Status)
	require.Equal(t, uint64(1), leg(lotW, "seller_debit/refund").PlayerId)
	require.Equal(t, uint64(70), result.LinkedID)
	require.Equal(t, "bid_refund", bidW.Stage)
	require.Equal(t, CancelPending, bid.Status)

	// 引擎接手:出价按被超价同一条路退回冻结款,拍品退回到账后按取消结案。
	require.NoError(t, (&Engine{}).plan(bid, bidW, 2000))
	require.NotNil(t, leg(bidW, "bid_return"))
	require.Equal(t, uint64(7), leg(bidW, "bid_return").PlayerId)
	refund := leg(lotW, "seller_debit/refund")
	refund.OpId, refund.Outcome = 604, outcomeApplied
	require.NoError(t, (&Engine{}).plan(lot, lotW, 2000))
	require.Equal(t, Cancelled, lot.Status)
}

func TestVoidAuctionRefusals(t *testing.T) {
	cases := map[string]func(lot *tradepb.TradeCommerceRecord, lotW *tradepb.TradeCommerceWorkflow, c *manualContext){
		"卖家已收款": func(_ *tradepb.TradeCommerceRecord, lotW *tradepb.TradeCommerceWorkflow, _ *manualContext) {
			lotW.Legs = append(lotW.Legs, manualLeg("seller_receive", 1, 4, 603, outcomeApplied, currencyBundle(80)))
		},
		"引用的不是这条出价": func(_ *tradepb.TradeCommerceRecord, lotW *tradepb.TradeCommerceWorkflow, _ *manualContext) {
			lotW.WinningBidId = 71
		},
		"出价不在 bid_held": func(_ *tradepb.TradeCommerceRecord, _ *tradepb.TradeCommerceWorkflow, c *manualContext) {
			c.bidW.Stage = "bid_refund"
		},
		"出价人已换主": func(_ *tradepb.TradeCommerceRecord, _ *tradepb.TradeCommerceWorkflow, c *manualContext) {
			c.bidChanges = []partyRoleChange{{party: partySeller, player: 7, revision: 2, contractRev: 1}}
		},
		"拍品没有出价": func(_ *tradepb.TradeCommerceRecord, lotW *tradepb.TradeCommerceWorkflow, _ *manualContext) {
			lotW.HighestBidder, lotW.WinningBidId = 0, 0
		},
		"不是拍品": func(lot *tradepb.TradeCommerceRecord, _ *tradepb.TradeCommerceWorkflow, _ *manualContext) {
			lot.Kind = 3
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			lot, lotW := openLot()
			markManual(lot, lotW, manualReasonCorrupt, "", 1)
			bid, bidW := heldBid("bid_held")
			c := manualContext{req: ManualRequest{Action: ManualVoidAuction}, bid: bid, bidW: bidW}
			change(lot, lotW, &c)
			beforeLot, beforeBid := proto.Clone(lotW), proto.Clone(c.bidW)
			_, err := planManualAction(lot, lotW, c)
			requireNotAllowed(t, err)
			require.True(t, proto.Equal(beforeLot, lotW) && proto.Equal(beforeBid, c.bidW), "拒绝时两张合同都不改")
		})
	}
}

// ── 第 2 类:部分发放 ────────────────────────────────────────────

// partialTrade 是一份交付中转人工的双方交易:买家收货腿部分发放(物品、两件实例、宝宝、货币)。
func partialTrade() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r, w := deliveredTrade()
	receive := leg(w, "buyer_receive")
	receive.Outcome = partialOutcome
	receive.Bundle = &assetpb.AssetBundle{
		Items:         []*assetpb.ItemGrant{{ConfigId: 10, Count: 2}},
		ItemInstances: []*assetpb.AssetItemInstance{{ItemUuid: 501, ConfigId: 20, StackSize: 1}, {ItemUuid: 502, ConfigId: 20, StackSize: 1}},
		PetInstance:   &assetpb.AssetPetInstance{PetId: 601, PetTableId: 3},
		Currencies:    []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 10}},
	}
	markManual(r, w, manualReasonAppliedPartial, "buyer_receive partial", 1)
	return r, w
}

func partialRequest(parts ...string) ManualRequest {
	req := ManualRequest{Action: ManualPartial}
	for _, p := range parts {
		part, err := ParsePartialPart(p)
		if err != nil {
			panic(err)
		}
		req.Parts = append(req.Parts, part)
	}
	return req
}

// 发放腿部分到账:只补缺口(按 scene 的发放顺序,缺的是后缀),原腿标被补发腿取代,补发腿到账后照常结案。
func TestPartialCreditReissuesOnlyTheShortfall(t *testing.T) {
	r, w := partialTrade()
	result, err := planManualAction(r, w, manualContext{req: partialRequest("buyer_receive=instance:502,pet,currency:1")})
	require.NoError(t, err)
	require.Equal(t, []string{"buyer_receive#2"}, result.NewLegs)
	require.Equal(t, []string{"buyer_receive"}, result.SupersededLegs)
	require.Equal(t, "deliver", w.Stage)
	shortfall := leg(w, "buyer_receive#2")
	require.Equal(t, uint64(2), shortfall.PlayerId)
	require.Len(t, shortfall.Bundle.ItemInstances, 1)
	require.Equal(t, uint64(502), shortfall.Bundle.ItemInstances[0].ItemUuid)
	require.Equal(t, uint64(601), shortfall.Bundle.PetInstance.GetPetId())
	require.Empty(t, shortfall.Bundle.Items, "按数量的物品是一整批,已到账")
	require.True(t, proto.Equal(&assetpb.CurrencyAmount{CurrencyType: 1, Amount: 10}, shortfall.Bundle.Currencies[0]))
	require.Len(t, result.PartialParts, 1)
	require.Equal(t, "buyer_receive", result.PartialParts[0].Leg)

	shortfall.OpId, shortfall.Outcome = 105, outcomeApplied
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, Settled, r.Status, "原腿被取代,只看补发腿")
}

// 缺口要与证据对得上:不在包里、不符合发放顺序、整条腿、点名不清的一律拒绝,合同不改。
func TestPartialCreditShortfallRefusals(t *testing.T) {
	for name, spec := range map[string]string{
		"不在包里":         "buyer_receive=instance:999",
		"违反发放顺序:实例缺了货币却到账": "buyer_receive=instance:502,pet",
		"违反发放顺序:前一件缺后一件到账": "buyer_receive=instance:501,pet,currency:1",
		"不是部分发放的腿":     "seller_receive=currency:1",
		"没有这条腿":        "nope=currency:1",
	} {
		t.Run(name, func(t *testing.T) {
			r, w := partialTrade()
			before := proto.Clone(w)
			_, err := planManualAction(r, w, manualContext{req: partialRequest(spec)})
			requireNotAllowed(t, err)
			require.True(t, proto.Equal(before, w), "拒绝时不改合同")
		})
	}

	// 只有一种资产的包:缺口就是整条腿,那是没到账,不是部分发放。
	r, w := partialTrade()
	leg(w, "buyer_receive").Bundle = currencyBundle(10)
	_, err := planManualAction(r, w, manualContext{req: partialRequest("buyer_receive=currency:1")})
	requireNotAllowed(t, err)
}

// 一次要给出全部部分发放的腿:漏掉的那条会被当成没到账,再按全量补发就会把已到账的部分发两遍。
func TestPartialRequiresEveryPartialLeg(t *testing.T) {
	r, w := partialTrade()
	seller := leg(w, "seller_receive")
	seller.Outcome = partialOutcome
	seller.Bundle = &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 1}}, Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 20}}}

	_, err := planManualAction(proto.Clone(r).(*tradepb.TradeCommerceRecord), proto.Clone(w).(*tradepb.TradeCommerceWorkflow),
		manualContext{req: partialRequest("buyer_receive=currency:1")})
	requireNotAllowed(t, err)

	result, err := planManualAction(r, w, manualContext{req: partialRequest("buyer_receive=currency:1", "seller_receive=currency:1")})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"buyer_receive#2", "seller_receive#2"}, result.NewLegs)
	require.Len(t, result.PartialParts, 2)
}

// 部分发放的腿不在「没到账」之列:reissue 不会按全量补发它。
func TestFailedCreditsSkipPartialLegs(t *testing.T) {
	_, w := partialTrade()
	require.Empty(t, failedCredits(w))
}

// consignmentInEscrow 是一份托管阶段转人工的寄售:卖家托管腿部分扣出(按数量的物品 + 货币)。
func consignmentInEscrow(debit *assetpb.AssetBundle) (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 11, Kind: 3, Status: Escrowing, SellerPlayerId: 1, Version: 2}
	w := &tradepb.TradeCommerceWorkflow{Stage: "escrow", SellerIdentity: identity("a1", 1),
		Legs: []*tradepb.TradeCommerceLeg{manualLeg("seller_debit", 1, 3, 111, partialOutcome, debit)}}
	markManual(r, w, manualReasonAppliedPartial, "seller_debit partial", 1)
	return r, w
}

// 托管扣款部分扣出:交易没托管齐,按整单退回结案,这条腿只退实际扣到的部分。
func TestPartialDebitRefundsWhatWasTaken(t *testing.T) {
	r, w := consignmentInEscrow(&assetpb.AssetBundle{
		Items:      []*assetpb.ItemGrant{{ConfigId: 10, Count: 2}},
		Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 5}, {CurrencyType: 2, Amount: 7}},
	})
	result, err := planManualAction(r, w, manualContext{req: partialRequest("seller_debit=currency:2")})
	require.NoError(t, err)
	require.Equal(t, []string{"seller_debit/refund"}, result.NewLegs)
	require.Equal(t, "return", w.Stage)
	require.Equal(t, CancelPending, r.Status)
	require.True(t, proto.Equal(&assetpb.AssetBundle{
		Items:      []*assetpb.ItemGrant{{ConfigId: 10, Count: 2}},
		Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 5}},
	}, leg(w, "seller_debit/refund").Bundle), "只退扣到的物品和第一条货币")
	require.Equal(t, uint32(2), result.PartialParts[0].Missing.Currencies[0].CurrencyType)

	// 退回到账后按取消结案;再跑 refund 拒绝。
	refund := leg(w, "seller_debit/refund")
	refund.OpId, refund.Outcome = 112, outcomeApplied
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, Cancelled, r.Status)
}

func TestPartialDebitRefusals(t *testing.T) {
	instanceDebit := &assetpb.AssetBundle{ItemUuids: []uint64{301, 302}, PetId: 401, Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 5}}}
	for name, c := range map[string]struct {
		debit *assetpb.AssetBundle
		spec  string
	}{
		"扣到的实例没有托管原件": {instanceDebit, "seller_debit=instance:302,pet,currency:1"},
		"实例没扣到时宝宝不会扣":  {instanceDebit, "seller_debit=instance:301,instance:302,currency:1"},
		"货币缺的不是后缀": {&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 1}},
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 5}, {CurrencyType: 2, Amount: 7}}}, "seller_debit=currency:1"},
		"整条腿都没扣到": {currencyBundle(5), "seller_debit=currency:1"},
	} {
		t.Run(name, func(t *testing.T) {
			r, w := consignmentInEscrow(c.debit)
			before := proto.Clone(w)
			_, err := planManualAction(r, w, manualContext{req: partialRequest(c.spec)})
			requireNotAllowed(t, err)
			require.True(t, proto.Equal(before, w))
		})
	}
}

func TestParsePartialPart(t *testing.T) {
	part, err := ParsePartialPart(" buyer_receive#2 = instance:7, pet ,currency:0 ")
	require.NoError(t, err)
	require.Equal(t, "buyer_receive#2", part.Leg)
	require.Equal(t, PartialMissing{Instances: []uint64{7}, Pet: true, Currencies: []uint32{0}}, part.Missing)
	require.Equal(t, "instance:7,pet,currency:0", part.Missing.String())

	for _, bad := range []string{"", "leg", "=pet", "leg=", "leg=item:10:2", "leg=instance:0", "leg=pet,pet", "leg=currency:1,currency:1", "leg=instance:x"} {
		_, err := ParsePartialPart(bad)
		requireNotAllowed(t, err)
	}
}

// ── 第 1 类:原主已换 ────────────────────────────────────────────

func owned(account string, revision uint64) *dspb.AccountRoleOwnership {
	return &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision}
}

func TestPartyChangeTable(t *testing.T) {
	p := contractParty{name: partySeller, player: 1, identity: identity("a1", 3)}
	cases := []struct {
		name    string
		fact    roleFact
		changed bool
	}{
		{"同一身份", roleFact{owned: owned("a1", 3)}, false},
		{"副本修订号落后,要自愈", roleFact{owned: owned("a1", 2)}, false},
		{"已过户", roleFact{owned: owned("b", 4)}, true},
		{"被原账号买回(修订号更大)", roleFact{owned: owned("a1", 5)}, true},
		{"同修订号账号不同", roleFact{owned: owned("b", 3)}, true},
		{"已删", roleFact{deleted: true}, true},
		{"副本缺行,判不了", roleFact{unknown: true}, false},
	}
	for _, c := range cases {
		require.Equal(t, c.changed, partyChange(p, c.fact) != nil, c.name)
	}
	require.Nil(t, partyChange(contractParty{name: partySeller, player: 1}, roleFact{owned: owned("b", 9)}), "缺原身份是坏数据,由 corrupt 一类处理")
}

// openConsignmentWithChangedSeller 是一份转人工(owner_changed)的在架寄售:卖家角色已过户。
func openConsignmentWithChangedSeller() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow, manualContext) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 12, Kind: 3, Status: Escrowing, SellerPlayerId: 1, Version: 6}
	w := &tradepb.TradeCommerceWorkflow{Stage: "open", SellerIdentity: identity("a1", 1), Legs: []*tradepb.TradeCommerceLeg{
		manualLeg("seller_debit", 1, 3, 121, outcomeApplied, currencyBundle(9)),
		manualLeg("seller_return", 1, 4, 122, outcomeRejected, currencyBundle(9)),
		manualLeg("seller_notice", 1, 4, 0, 0, currencyBundle(1)),
	}}
	markManual(r, w, manualReasonOwnerChanged, "seller player=1 owner_revision=2 (contract 1) same_account=false", 1)
	c := manualContext{
		req:     ManualRequest{Action: ManualRedirect, Party: partySeller, To: 5},
		changes: []partyRoleChange{{party: partySeller, player: 1, revision: 2, contractRev: 1}},
		target:  owned("a1", 7),
	}
	return r, w, c
}

// 改绑:这一方整体换成原账号名下的目标角色;没发出的腿原地改投,发往原角色没到账的发放腿补发给目标角色。
func TestRedirectRebindsTheChangedParty(t *testing.T) {
	r, w, c := openConsignmentWithChangedSeller()
	result, err := planManualAction(r, w, c)
	require.NoError(t, err)
	require.Equal(t, uint64(5), r.SellerPlayerId)
	require.True(t, proto.Equal(identity("a1", 7), w.SellerIdentity), "原账号 + 目标角色现在的修订号")
	require.True(t, proto.Equal(identity("a1", 1), result.IdentityBefore), "改前的原身份进留痕")
	require.Equal(t, []string{"seller_notice"}, result.RetargetedLegs)
	require.Equal(t, uint64(5), leg(w, "seller_notice").PlayerId)
	require.Equal(t, []string{"seller_return#2"}, result.NewLegs)
	require.Equal(t, uint64(5), leg(w, "seller_return#2").PlayerId)
	require.Equal(t, "seller_return#2", leg(w, "seller_return").SupersededBy)
	require.Equal(t, uint64(1), leg(w, "seller_debit").PlayerId, "已扣出的托管不动,证据留着")
	require.Equal(t, "open", w.Stage)
	require.Equal(t, Escrowing, r.Status)
}

func TestRedirectRefusals(t *testing.T) {
	for name, change := range map[string]func(r *tradepb.TradeCommerceRecord, c *manualContext){
		"目标不属于原账号": func(_ *tradepb.TradeCommerceRecord, c *manualContext) { c.target = owned("b", 7) },
		"这一方没换主":   func(_ *tradepb.TradeCommerceRecord, c *manualContext) { c.changes = nil },
		"目标是另一方":   func(r *tradepb.TradeCommerceRecord, c *manualContext) { r.BuyerPlayerId = 5 },
		"人民币寄售":    func(r *tradepb.TradeCommerceRecord, _ *manualContext) { r.Kind = 6 },
		"合同没有这一方":  func(_ *tradepb.TradeCommerceRecord, c *manualContext) { c.req.Party = partyBuyer },
	} {
		t.Run(name, func(t *testing.T) {
			r, w, c := openConsignmentWithChangedSeller()
			change(r, &c)
			before := proto.Clone(w)
			_, err := planManualAction(r, w, c)
			requireNotAllowed(t, err)
			require.True(t, proto.Equal(before, w))
		})
	}
}

// 两方都已换主:改绑一方之后合同保持人工,等另一方也改绑再回到原阶段。
func TestRedirectKeepsManualWhileTheOtherPartyStillChanged(t *testing.T) {
	r, w, c := openConsignmentWithChangedSeller()
	r.BuyerPlayerId, w.BuyerIdentity = 2, identity("a2", 1)
	c.changes = append(c.changes, partyRoleChange{party: partyBuyer, player: 2, deleted: true, contractRev: 1})
	_, err := planManualAction(r, w, c)
	require.NoError(t, err)
	require.Equal(t, uint64(5), r.SellerPlayerId)
	require.Equal(t, "manual", w.Stage)
	require.Equal(t, Manual, r.Status)
}

// 拍品的买方就是最高出价人:改绑买方时一起改,结拍时拍品发给新的一方。
func TestRedirectLotBuyerAlsoMovesTheHighestBidder(t *testing.T) {
	lot, lotW := openLot()
	markManual(lot, lotW, manualReasonOwnerChanged, "", 1)
	c := manualContext{
		req:     ManualRequest{Action: ManualRedirect, Party: partyBuyer, To: 9},
		changes: []partyRoleChange{{party: partyBuyer, player: 7, deleted: true, contractRev: 1}},
		target:  owned("a7", 3),
	}
	_, err := planManualAction(lot, lotW, c)
	require.NoError(t, err)
	require.Equal(t, uint64(9), lot.BuyerPlayerId)
	require.Equal(t, uint64(9), lotW.HighestBidder)
	require.Equal(t, "lot_open", lotW.Stage)
}

// 有一方已不归原身份时,别的动作一律拒绝:引擎每一步推进都会被原归属身份核对挡住,先改绑。
func TestOtherActionsWaitForRedirect(t *testing.T) {
	for _, action := range []ManualAction{ManualResume, ManualRefund, ManualReissue} {
		r, w, c := openConsignmentWithChangedSeller()
		c.req = ManualRequest{Action: action}
		_, err := planManualAction(r, w, c)
		requireNotAllowed(t, err)
	}
}

// ── 补记结局与请求校验 ───────────────────────────────────────────

// 补记结局:与 advance 同一判定;在途、查不到 outbox 行、没点名的部分发放、快照损坏都拒绝。
func TestSettleLegOutcomes(t *testing.T) {
	newWorkflow := func() *tradepb.TradeCommerceWorkflow {
		return &tradepb.TradeCommerceWorkflow{Legs: []*tradepb.TradeCommerceLeg{manualLeg("seller_debit", 1, 3, 11, 0, currencyBundle(1))}}
	}
	reader := func(out data.LegOutcome, err error) func(context.Context, uint64) (data.LegOutcome, error) {
		return func(context.Context, uint64) (data.LegOutcome, error) { return out, err }
	}
	status := func(s tradepb.TradeAssetOpStatus) data.LegOutcome { return data.LegOutcome{Status: s} }

	w := newWorkflow()
	require.NoError(t, settleLegOutcomes(context.Background(), w, reader(status(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED), nil), legSettling{}))
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
			requireNotAllowed(t, settleLegOutcomes(context.Background(), newWorkflow(), read, legSettling{}))
		})
	}

	// partial 点名的腿:部分发放照样记下结局。
	named := newWorkflow()
	require.NoError(t, settleLegOutcomes(context.Background(), named, reader(status(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL), nil),
		legSettling{partialLegs: map[string]bool{"seller_debit": true}}))
	require.Equal(t, partialOutcome, named.Legs[0].Outcome)

	// 没发出的腿:只有 redirect 接受。
	unsent := &tradepb.TradeCommerceWorkflow{Legs: []*tradepb.TradeCommerceLeg{manualLeg("seller_return", 1, 4, 0, 0, currencyBundle(1))}}
	requireNotAllowed(t, settleLegOutcomes(context.Background(), unsent, reader(data.LegOutcome{}, nil), legSettling{}))
	require.NoError(t, settleLegOutcomes(context.Background(), unsent, reader(data.LegOutcome{}, nil), legSettling{unissuedOK: true}))

	storageErr := errors.New("boom")
	err := settleLegOutcomes(context.Background(), newWorkflow(), reader(data.LegOutcome{}, storageErr), legSettling{})
	require.ErrorIs(t, err, storageErr)
	require.False(t, errors.Is(err, ErrManualNotAllowed), "存储错误不是前置条件")
}

func TestManualRequestValidate(t *testing.T) {
	valid := ManualRequest{CommerceID: 1, ExpectVersion: 2, Action: ManualRefund, Operator: "ops", Reason: "ticket-1"}
	require.NoError(t, valid.validate())
	partial := valid
	partial.Action, partial.Parts = ManualPartial, []PartialPart{{Leg: "a", Missing: PartialMissing{Pet: true}}}
	require.NoError(t, partial.validate())
	redirect := valid
	redirect.Action, redirect.Party, redirect.To = ManualRedirect, partySeller, 9
	require.NoError(t, redirect.validate())
	void := valid
	void.Action, void.LinkedID, void.LinkedVersion = ManualVoidAuction, 3, 4
	require.NoError(t, void.validate())

	for name, change := range map[string]func(*ManualRequest){
		"缺合同":          func(q *ManualRequest) { q.CommerceID = 0 },
		"缺版本":          func(q *ManualRequest) { q.ExpectVersion = 0 },
		"未知动作":         func(q *ManualRequest) { q.Action = "delete" },
		"缺操作人":         func(q *ManualRequest) { q.Operator = " " },
		"操作人带换行":       func(q *ManualRequest) { q.Operator = "a\nb" },
		"缺依据":          func(q *ManualRequest) { q.Reason = "" },
		"refund 带缺口":   func(q *ManualRequest) { q.Parts = partial.Parts },
		"refund 带改绑目标": func(q *ManualRequest) { q.Party, q.To = partySeller, 9 },
		"refund 带联动出价": func(q *ManualRequest) { q.LinkedID, q.LinkedVersion = 3, 4 },
		"partial 没给缺口": func(q *ManualRequest) { q.Action = ManualPartial },
		"partial 同一条腿两次": func(q *ManualRequest) {
			q.Action, q.Parts = ManualPartial, []PartialPart{partial.Parts[0], partial.Parts[0]}
		},
		"redirect 一方不对":  func(q *ManualRequest) { q.Action, q.Party, q.To = ManualRedirect, "both", 9 },
		"redirect 缺目标":   func(q *ManualRequest) { q.Action, q.Party = ManualRedirect, partySeller },
		"void 缺出价版本":     func(q *ManualRequest) { q.Action, q.LinkedID = ManualVoidAuction, 3 },
		"void 联动的是自己":    func(q *ManualRequest) { q.Action, q.LinkedID, q.LinkedVersion = ManualVoidAuction, 1, 4 },
	} {
		t.Run(name, func(t *testing.T) {
			q := valid
			change(&q)
			requireNotAllowed(t, q.validate())
		})
	}
}
