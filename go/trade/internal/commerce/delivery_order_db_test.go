package commerce

// delivery_order_db_test.go —— 成交交付「先买家、后卖家」与买家那条被拒时整单退回的真库回归(delivery_order.go,数据覆写审计 r2-trade#1)。
// 门控同 tick_isolation_db_test.go(COMMERCE_TEST_MYSQL_DSN + COMMERCE_REQUIRE_MYSQL=1,或 TiDB 那一组);都没设时 Skip,
// Skip 不代表通过。资产终局由夹具直接落在 outbox 上,验证的是交易 SQL 状态机,不冒充 scene 端到端。

import (
	"context"
	"testing"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

var txAuctionReturn = uint32(rollbackpb.TransactionType_TX_AUCTION_RETURN)

// opTxType 读一条资产指令带的交易类型(scene 按它判要不要查获取封禁)。
func opTxType(t *testing.T, e *Engine, opID uint64) uint32 {
	t.Helper()
	var txType uint32
	require.NoError(t, e.Repo.DB.QueryRow("SELECT tx_type FROM trade_asset_op WHERE op_id=?", opID).Scan(&txType))
	return txType
}

// 双方交易:卖家所得压到买家那条到账;买家那条被拒(例如被封禁获取这件物品),卖家所得永远不发,
// 货退卖家、钱退买家(都带 TX_AUCTION_RETURN),合同按撤单结案,不转人工。
func TestRealDBRejectedBuyerUnwindsPlayerTrade(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	offer, err := e.CreatePlayerTrade(ctx, 181, &tradepb.CreatePlayerTradeRequest{ClientRequestId: "unwind-trade-01", BuyerPlayerId: 182,
		SellerAssets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 2}},
		BuyerAssets:  &tradepb.TradeAssetSelection{CurrencyType: 1, CurrencyAmount: 70}})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, err = e.Mutate(ctx, 182, offer.CommerceId, "confirm")
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, err = e.Mutate(ctx, 181, offer.CommerceId, "confirm")
	require.NoError(t, err)
	tick(t, e)
	_, w := state(t, e, offer.CommerceId)
	require.Equal(t, "deliver", w.Stage)
	require.NotZero(t, leg(w, "buyer_receive").OpId, "买家那条先发")
	require.Zero(t, leg(w, "seller_receive").OpId, "卖家所得等买家那条到账才发")

	setOpStatus(t, e, leg(w, "buyer_receive").OpId, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED)
	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, CancelPending, r.Status, "整单退回,不转人工")
	require.Equal(t, "return", w.Stage)
	require.Equal(t, unwoundBy, leg(w, "seller_receive").SupersededBy)
	require.Zero(t, leg(w, "seller_receive").OpId, "被取代的卖家所得永远不发")
	goodsBack, moneyBack := leg(w, "seller_return"), leg(w, "buyer_return")
	require.NotZero(t, goodsBack.OpId)
	require.NotZero(t, moneyBack.OpId)
	require.Equal(t, txAuctionReturn, opTxType(t, e, goodsBack.OpId), "退回不受获取封禁约束")
	require.Equal(t, txAuctionReturn, opTxType(t, e, moneyBack.OpId))
	goods := opPayload(t, e, goodsBack.OpId)
	require.Len(t, goods.Items, 1)
	require.EqualValues(t, 10, goods.Items[0].ConfigId)
	require.EqualValues(t, 2, goods.Items[0].Count, "货原样退给卖家")
	require.True(t, proto.Equal(&assetpb.CurrencyAmount{CurrencyType: 1, Amount: 70}, opPayload(t, e, moneyBack.OpId).Currencies[0]), "钱原样退给买家")

	settleLegs(t, e, 2)
	tick(t, e)
	r, _ = state(t, e, offer.CommerceId)
	require.Equal(t, Cancelled, r.Status)
	require.Equal(t, 1, opCount(t, e, 181, 4), "卖家只收到退回的货,没有货款")
	require.Equal(t, 2, opCount(t, e, 182, 4), "买家:被拒的那条 + 退款")
}

// 拍卖:赢家那条被拒时拍品只退货,不退钱;拍品按撤单结案、唤醒赢家的出价,出价按被超价同一条路(bid_refund)退回冻结款。
// 卖家所得从没发出,冻结款只退一次。
func TestRealDBRejectedAuctionWinnerRefundsThroughTheBid(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	lotID, bidID := openLotWithBid(t, e, 183, 184, "unwind-02")
	// 结拍:截止与下次到期一起改到过去(生产里两者由同一次排期写入)。
	_, err := e.Repo.DB.Exec("UPDATE trade_commerce SET expires_ms=1,updated_ms=1 WHERE commerce_id=?", lotID)
	require.NoError(t, err)
	tick(t, e)
	lr, lw := state(t, e, lotID)
	require.Equal(t, Delivering, lr.Status)
	require.Equal(t, "deliver", lw.Stage)
	require.Zero(t, leg(lw, "seller_receive").OpId, "卖家所得等赢家那条到账才发")

	setOpStatus(t, e, leg(lw, "buyer_receive").OpId, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED)
	tick(t, e)
	lr, lw = state(t, e, lotID)
	require.Equal(t, CancelPending, lr.Status)
	require.Equal(t, "return", lw.Stage)
	require.Nil(t, leg(lw, "buyer_return"), "拍品不退钱,钱还在出价合同里")
	goodsBack := leg(lw, "seller_return")
	require.NotZero(t, goodsBack.OpId)
	require.Equal(t, txAuctionReturn, opTxType(t, e, goodsBack.OpId))
	require.EqualValues(t, 1, opPayload(t, e, goodsBack.OpId).Items[0].Count, "拍品原样退给卖家")
	_, bw := state(t, e, bidID)
	require.Equal(t, "bid_held", bw.Stage, "拍品还没结案,出价不动")

	settleLegs(t, e, 2)
	tick(t, e)
	lr, _ = state(t, e, lotID)
	require.Equal(t, Cancelled, lr.Status, "退货到账,拍品按撤单结案并唤醒出价")
	// 出价看到拍品撤了改 bid_refund,下一步建退款腿并发出;多推一轮不影响结果。
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	_, bw = state(t, e, bidID)
	require.Equal(t, "return", bw.Stage)
	refund := leg(bw, "bid_return")
	require.NotNil(t, refund)
	require.NotZero(t, refund.OpId)
	require.Equal(t, txAuctionReturn, opTxType(t, e, refund.OpId))
	require.True(t, proto.Equal(&assetpb.CurrencyAmount{CurrencyType: 1, Amount: 80}, opPayload(t, e, refund.OpId).Currencies[0]), "冻结款原样退回赢家")

	settleLegs(t, e, 2)
	tick(t, e)
	br, _ := state(t, e, bidID)
	require.Equal(t, Cancelled, br.Status)
	require.Equal(t, 1, opCount(t, e, 183, 4), "卖家只收到退回的拍品,没有货款")
	require.Equal(t, 2, opCount(t, e, 184, 4), "赢家:被拒的那条 + 冻结款退回")
}
