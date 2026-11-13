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
// 现在 seller_receive 要等 buyer_receive 到账之后才发(heldForBuyerFirst,advance 的发出循环经 legReadyToSend 问它)。
// buyer_receive 终局没到账(判拒、中止、自动放弃,都说明 scene 没发出这份货)时,seller_receive 还没发,整单自动退回
// (settleBuyerFirstDelivery):货退给卖家,钱退给买家,退回腿带 TX_AUCTION_RETURN、不受获取封禁约束,合同按撤单收尾,不转人工。
// scene 只对确定性失败回终局拒绝(获取封禁、包不合法);背包满是 RETRY,照常重投,不会触发退回。
//
// 退回的正是这一笔里从双方托管里出的那两份,由当时托管它的合同原样交回原主,所以守恒:
//   - 双方交易、寄售:货和钱都在本合同的扣款腿上,本合同退。寄售因此整单撤下,卖家要重新上架。
//   - 撮合子单:货和钱是撮合时从两张挂单的托管里划给子单的,挂单把这一笔计入已成交量、不会再退,所以由子单退。
//     挂单的已成交量仍含这一笔,只影响展示,不影响守恒。
//   - 拍卖:钱在赢家的出价合同里,拍品只在发出卖家所得时才动它。拍品只退货;拍品按撤单结案后,停在 bid_held 的出价
//     看到拍品撤了,按被超价同一条路(bid_refund)自己退款(settleHeldBid、heldBidKeepsFunds)。
//
// buyer_surplus(撮合时买单多冻的差价)本来就是退给买家自己的钱,不压,照常发。
//
// 只有两条交付腿都是原腿(没被人工补发过)、买家那条一点没到账、卖家那条从没发出时才自动退回(unwindable)。
// 补发过就说明之前有一部分到了账或发了出去(买家那份部分到账后补的缺口、改绑后重发的腿),原样退回会把已经交出的部分再退一遍,
// 所以买家那条没到账时转人工,卖家所得继续压着。另外两种也转人工:seller_receive 发出之后被拒(货已交付,所得留在托管里,
// 被封禁的是卖家自己,解封后人工补发);闸上线之前两条已经一起发出的存量合同,照旧由 finalCredits 转人工。
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
// 买家那条按 currentLeg 判:人工补齐买家那份(补发腿)到账之后,闸自动放行。
func heldForBuyerFirst(w *tradepb.TradeCommerceWorkflow, l *tradepb.TradeCommerceLeg) bool {
	base, _, _ := strings.Cut(l.GetRole(), "#")
	if base != "seller_receive" {
		return false
	}
	buyer := currentLeg(w, "buyer_receive")
	return buyer != nil && !applied(buyer)
}

// settleBuyerFirstDelivery 在交付阶段处理买家那条交付腿的终局,plan 在收尾判定(finalCredits)之前调用。
// 买家那条终局没到账、卖家那条还没发时,能整单退回就退回,不能就转人工;其余情形(在途、已到账、存量合同)不动,交给后面的收尾判定。
// 这一步必须给出结论:卖家所得压着时 finalCredits 等不到它的结局,不处理合同就会一直停在交付阶段。
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
	if !unwindable(w) {
		// 卖家所得继续压着;人工补齐买家那份之后,currentLeg 沿补发找到最新一条,闸自动放行。
		// advance 读到部分发放时已先转人工(manualReasonAppliedPartial),这里兜的是人工处置之后补发腿又没到账一类。
		markManual(r, w, manualReasonFinalCredit, "买家交付腿没有全部到账,卖家所得暂缓发出", nowMs)
		return nil
	}
	if leg(w, "seller_return") != nil || leg(w, "buyer_return") != nil {
		return errors.New("买家交付腿被拒要整单退回,合同里却已经有退回腿")
	}
	buyer.SupersededBy = unwoundBy
	seller.SupersededBy = unwoundBy
	addLeg(w, "seller_return", seller.GetPlayerId(), 4, proto.Clone(buyer.GetBundle()).(*assetpb.AssetBundle))
	if r.GetKind() != 5 {
		// 拍品(kind 5)不退钱:钱还在赢家的出价合同里,由它看到拍品撤了之后自己退(settleHeldBid)。
		addLeg(w, "buyer_return", buyer.GetPlayerId(), 4, proto.Clone(seller.GetBundle()).(*assetpb.AssetBundle))
	}
	w.Stage = "return"
	r.Status = CancelPending
	return nil
}

// unwindable 报告这一笔能不能原样整单退回:两条交付腿都是原腿(没被人工补发取代),买家那条终局没到账且一点没到
// (判拒、中止、自动放弃,不是部分到账),卖家那条从没发出。改绑只把没发出的腿原地改收件人,不算补发。
func unwindable(w *tradepb.TradeCommerceWorkflow) bool {
	buyer, seller := leg(w, "buyer_receive"), leg(w, "seller_receive")
	return buyer != nil && seller != nil &&
		buyer.GetSupersededBy() == "" && seller.GetSupersededBy() == "" &&
		done(buyer) && !applied(buyer) && buyer.GetOutcome() != partialOutcome &&
		seller.GetOpId() == 0
}

// heldBidKeepsFunds 报告拍品按撤单结案之后,停在 bid_held 的出价的冻结款是否还在出价里、可以由出价自己退回。
// 拍品不引用这条出价时,冻结款只属于出价人;引用时看拍品的卖家所得原腿:从没发出,拍品就没动过这笔钱
// (补发只给发出后没到账的腿,原腿没发出就没有补发)。发出过却按撤单结案是自相矛盾(钱可能已经付给卖家),
// 不能自动退款,settleHeldBid 按坏数据隔离成人工。
func heldBidKeepsFunds(lot *tradepb.TradeCommerceWorkflow, bidID uint64) bool {
	if lot.GetWinningBidId() != bidID {
		return true
	}
	proceeds := leg(lot, "seller_receive")
	return proceeds == nil || proceeds.GetOpId() == 0
}
