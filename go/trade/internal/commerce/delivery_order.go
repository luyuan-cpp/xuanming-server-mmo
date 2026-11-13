package commerce

import (
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
)

// 成交交付「先买家、后卖家」(数据覆写审计 r2-trade#1)。
//
// 一笔成交有两条交付腿:buyer_receive 把卖家托管的货交给买家,seller_receive 把买家托管的钱交给卖家。以前两条同时发出:
// 买家被 GM 封禁获取这件物品时,scene 把 buyer_receive 判成终局拒绝,seller_receive 却照样到账,钱货分开,合同只能转人工。
//
// 现在 seller_receive 要等 buyer_receive 到账之后才发(heldForBuyerFirst,advance 的发出循环问它)。buyer_receive 终局没到账
// (判拒、中止、放弃)时,seller_receive 还没发,整单自动退回(settleBuyerFirstDelivery):货退给卖家,钱退给买家,两条都是退回腿
// (TX_AUCTION_RETURN,不受获取封禁约束),合同按撤单收尾,不转人工。
//
// 退回的正是这一笔里从双方托管里出的那两份,原样交回原主就守恒,不管托管记在哪张合同上:
//   - 双方交易、寄售:托管就在本合同的扣款腿上。寄售因此整单撤下,卖家要重新上架。
//   - 拍卖:钱在赢家的出价合同里。那张合同停在 bid_held,不会再自己退款,所以由拍品合同退回不会重复。
//   - 撮合子单:货和钱来自两张挂单的托管,子单撤回之后挂单的已成交量仍计入这一笔。只影响展示,不影响守恒。
//
// buyer_surplus(撮合时买单多冻的差价)本来就是退给买家自己的钱,不压,照常发。
//
// 仍然转人工的两种:buyer_receive 部分到账(一部分货已经到了买家手里);seller_receive 发出之后被拒(货已交付,所得留在托管里,
// 被封禁的是卖家自己,解封后人工补发)。闸上线之前两条已经一起发出的存量合同,照旧由 finalCredits 转人工。
//
// 下单时不预检获取封禁:data_service 只查得到物品封禁,四个下单入口各加一次跨服务读也只能降频,
// 挡不住下单之后才生效的封禁,提交点的判定与这里的退回反正都得有。代价是被封禁的买家买到寄售时,这单寄售会被撤下。

// unwoundBy 是自动退回时写进被取代交付腿的 SupersededBy。它不是腿名:currentLeg 停在原腿,finalCredits 与发出循环都跳过它。
const unwoundBy = "unwound"

// legReadyToSend 是 advance 发出循环的判据:还没发出、没被取代、不在等买家那条交付腿到账。
// 被取代又没发出的腿只有一种来源:自动退回取代的卖家所得腿,它永远不能再发。
func legReadyToSend(w *tradepb.TradeCommerceWorkflow, l *tradepb.TradeCommerceLeg) bool {
	return l.GetOpId() == 0 && l.GetSupersededBy() == "" && !heldForBuyerFirst(w, l)
}

// heldForBuyerFirst 报告这条腿要不要压着,等买家那条交付腿到账之后才发。只压卖家所得(seller_receive 及其补发)。
func heldForBuyerFirst(w *tradepb.TradeCommerceWorkflow, l *tradepb.TradeCommerceLeg) bool {
	base, _, _ := strings.Cut(l.GetRole(), "#")
	if base != "seller_receive" {
		return false
	}
	buyer := currentLeg(w, "buyer_receive")
	return buyer != nil && !applied(buyer)
}

// settleBuyerFirstDelivery 在交付阶段处理买家那条交付腿的终局,plan 在收尾判定(finalCredits)之前调用。
// 部分到账转人工;终局没到账、卖家那条还没发,整单退回。其余情形(在途、已到账、存量合同)不动,交给后面的收尾判定。
func settleBuyerFirstDelivery(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, nowMs uint64) error {
	buyer := currentLeg(w, "buyer_receive")
	if buyer == nil || !done(buyer) || applied(buyer) {
		return nil
	}
	seller := currentLeg(w, "seller_receive")
	if seller == nil || seller.GetOpId() != 0 {
		// 没有卖家所得腿,或者它已经发出(闸上线之前的存量合同):交给 finalCredits,两条都有结局之后转人工。
		return nil
	}
	if buyer.GetOutcome() == partialOutcome {
		// 一部分货已经到了买家手里,不能整单退回。卖家所得继续压着;人工补齐买家那份之后,currentLeg 沿补发找到最新一条,闸自动放行。
		markManual(r, w, manualReasonFinalCredit, "买家交付腿部分到账,卖家所得暂缓发出", nowMs)
		return nil
	}
	if leg(w, "seller_return") != nil || leg(w, "buyer_return") != nil {
		return errors.New("买家交付腿被拒要整单退回,合同里却已经有退回腿")
	}
	buyer.SupersededBy = unwoundBy
	seller.SupersededBy = unwoundBy
	addLeg(w, "seller_return", seller.GetPlayerId(), 4, proto.Clone(buyer.GetBundle()).(*assetpb.AssetBundle))
	addLeg(w, "buyer_return", buyer.GetPlayerId(), 4, proto.Clone(seller.GetBundle()).(*assetpb.AssetBundle))
	w.Stage = "return"
	r.Status = CancelPending
	return nil
}
