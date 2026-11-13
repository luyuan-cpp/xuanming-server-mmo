package commerce

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"
)

// 成交交付「先买家、后卖家」与买家那条被拒时整单退回(delivery_order.go,数据覆写审计 r2-trade#1)。纯函数用例,不读库。

const outcomePartial = uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL)

func itemBundle(config uint32, count uint32) *assetpb.AssetBundle {
	return &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: config, Count: count}}}
}

// deliveringTrade 是一份刚进交付阶段的双方交易:两边都扣到了,两条交付腿都建好、还没发(建腿顺序与 plan 相同,卖家所得在前)。
func deliveringTrade() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 21, Kind: 2, Status: Delivering, SellerPlayerId: 1, BuyerPlayerId: 2, Version: 4}
	w := &tradepb.TradeCommerceWorkflow{Stage: "deliver", SellerIdentity: identity("a1", 1), BuyerIdentity: identity("a2", 1), Legs: []*tradepb.TradeCommerceLeg{
		manualLeg("seller_debit", 1, 3, 101, outcomeApplied, itemBundle(10, 3)),
		manualLeg("buyer_debit", 2, 3, 102, outcomeApplied, currencyBundle(30)),
		manualLeg("seller_receive", 1, 4, 0, 0, currencyBundle(30)),
		manualLeg("buyer_receive", 2, 4, 0, 0, itemBundle(10, 3)),
	}}
	return r, w
}

// 卖家所得压到买家那条到账之后才发;买家那条照常先发。两条都到账后按成交结案。
func TestBuyerFirstHoldsSellerProceedsUntilBuyerApplied(t *testing.T) {
	r, w := deliveringTrade()
	require.True(t, legReadyToSend(w, leg(w, "buyer_receive")), "买家所得先发")
	require.False(t, legReadyToSend(w, leg(w, "seller_receive")), "买家那条还没到账,卖家所得压着")

	buyer := leg(w, "buyer_receive")
	buyer.OpId = 104
	require.False(t, legReadyToSend(w, leg(w, "seller_receive")), "买家那条在途,仍压着")
	require.NoError(t, (&Engine{}).plan(r, w, 1000))
	require.Equal(t, "deliver", w.Stage, "买家那条在途,合同等着")

	buyer.Outcome = outcomeApplied
	require.True(t, legReadyToSend(w, leg(w, "seller_receive")), "买家那条到账,放行卖家所得")
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, "deliver", w.Stage, "卖家所得还没发,不能结案")
	require.Equal(t, Delivering, r.Status)

	seller := leg(w, "seller_receive")
	seller.OpId, seller.Outcome = 105, outcomeApplied
	require.NoError(t, (&Engine{}).plan(r, w, 3000))
	require.Equal(t, "done", w.Stage)
	require.Equal(t, Settled, r.Status)
}

// 买家那条被拒(例如被 GM 封禁获取这件物品):卖家所得还没发,整单退回,货回卖家、钱回买家,两条都是退回腿;
// 被取代的交付腿不再发出;两条退回到账后按撤单结案,不转人工。
func TestBuyerRejectedUnwindsTheWholeDeal(t *testing.T) {
	r, w := deliveringTrade()
	buyer := leg(w, "buyer_receive")
	buyer.OpId, buyer.Outcome = 104, outcomeRejected
	require.NoError(t, (&Engine{}).plan(r, w, 1000))

	require.Equal(t, "return", w.Stage)
	require.Equal(t, CancelPending, r.Status)
	require.Equal(t, unwoundBy, leg(w, "buyer_receive").SupersededBy)
	require.Equal(t, unwoundBy, leg(w, "seller_receive").SupersededBy)
	require.False(t, legReadyToSend(w, leg(w, "seller_receive")), "被取代的卖家所得永远不发")

	goodsBack := leg(w, "seller_return")
	require.NotNil(t, goodsBack)
	require.Equal(t, uint64(1), goodsBack.PlayerId)
	require.Equal(t, uint32(4), goodsBack.Stream)
	require.True(t, proto.Equal(itemBundle(10, 3), goodsBack.Bundle), "货原样退给卖家")
	moneyBack := leg(w, "buyer_return")
	require.NotNil(t, moneyBack)
	require.Equal(t, uint64(2), moneyBack.PlayerId)
	require.True(t, proto.Equal(currencyBundle(30), moneyBack.Bundle), "钱原样退给买家")
	for _, l := range []*tradepb.TradeCommerceLeg{goodsBack, moneyBack} {
		require.True(t, legReadyToSend(w, l))
		require.Equal(t, uint32(rollbackpb.TransactionType_TX_AUCTION_RETURN), legTxType(l), "退回不受获取封禁约束")
	}

	goodsBack.OpId, goodsBack.Outcome = 106, outcomeApplied
	moneyBack.OpId, moneyBack.Outcome = 107, outcomeApplied
	require.NoError(t, (&Engine{}).plan(r, w, 2000))
	require.Equal(t, "done", w.Stage)
	require.Equal(t, Cancelled, r.Status)
}

// 买家那条部分到账:一部分货已经交出,不能整单退回,转人工;卖家所得继续压着。
// 人工补齐买家那份(补发腿)到账之后,闸沿补发找到最新一条,放行卖家所得。
func TestBuyerPartialGoesManualAndKeepsSellerHeld(t *testing.T) {
	r, w := deliveringTrade()
	buyer := leg(w, "buyer_receive")
	buyer.OpId, buyer.Outcome = 104, outcomePartial
	require.NoError(t, (&Engine{}).plan(r, w, 1000))

	require.Equal(t, Manual, r.Status)
	require.Equal(t, "deliver", w.Quarantine.GetStage())
	require.Equal(t, manualReasonFinalCredit, w.Quarantine.GetReason())
	require.Empty(t, leg(w, "seller_receive").SupersededBy)
	require.Zero(t, leg(w, "seller_receive").OpId)
	require.False(t, legReadyToSend(w, leg(w, "seller_receive")))
	require.Nil(t, leg(w, "seller_return"), "部分到账不整单退回")

	buyer.SupersededBy = "buyer_receive#2"
	w.Legs = append(w.Legs, manualLeg("buyer_receive#2", 2, 4, 108, outcomeApplied, itemBundle(10, 1)))
	require.True(t, legReadyToSend(w, leg(w, "seller_receive")), "补齐之后放行卖家所得")
}

// 撮合子单的差价腿是退给买家自己的钱:不压,照常发;买家那条被拒时它也不被取代,买家照样拿回。
func TestBuyerSurplusIsNeverHeldAndSurvivesTheUnwind(t *testing.T) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 31, Kind: 4, Status: Delivering, Version: 1}
	w := &tradepb.TradeCommerceWorkflow{Stage: "deliver", Legs: []*tradepb.TradeCommerceLeg{
		manualLeg("buyer_receive", 2, 4, 0, 0, itemBundle(10, 5)),
		manualLeg("seller_receive", 1, 4, 0, 0, currencyBundle(50)),
		manualLeg("buyer_surplus", 2, 4, 0, 0, currencyBundle(5)),
	}}
	require.True(t, legReadyToSend(w, leg(w, "buyer_surplus")))
	require.False(t, legReadyToSend(w, leg(w, "seller_receive")))

	leg(w, "buyer_receive").OpId, leg(w, "buyer_receive").Outcome = 201, outcomeRejected
	leg(w, "buyer_surplus").OpId, leg(w, "buyer_surplus").Outcome = 202, outcomeApplied
	require.NoError(t, (&Engine{}).plan(r, w, 1000))
	require.Equal(t, "return", w.Stage)
	require.Empty(t, leg(w, "buyer_surplus").SupersededBy, "差价腿已经退回,不动")
	require.True(t, proto.Equal(itemBundle(10, 5), leg(w, "seller_return").Bundle))
	require.Equal(t, uint64(1), leg(w, "seller_return").PlayerId)
	require.True(t, proto.Equal(currencyBundle(50), leg(w, "buyer_return").Bundle))
	require.Equal(t, uint64(2), leg(w, "buyer_return").PlayerId)
}

// 闸上线之前两条已经一起发出的存量合同:买家那条被拒、卖家那条已到账,不能退回,照旧转人工(与 TestPlanFinalCreditFailureRecordsQuarantine 同口径)。
func TestLegacyContractWithBothLegsSentStillGoesManual(t *testing.T) {
	r, w := deliveredTrade()
	require.NoError(t, (&Engine{}).plan(r, w, 1000))
	require.Equal(t, Manual, r.Status)
	require.Nil(t, leg(w, "seller_return"))
	require.Nil(t, leg(w, "buyer_return"))
}
