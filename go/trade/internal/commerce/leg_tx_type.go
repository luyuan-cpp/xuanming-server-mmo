package commerce

import (
	"strings"

	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"
)

// legTxType 是一条腿进资产通道时带的交易类型,scene 按它记流水,并决定要不要判获取封禁:
//   - 扣出(托管)腿:TX_AUCTION_SELL。
//   - 付款撤销后的扣回腿(official_reverse_<修订号>,official_reversal.go):TX_PAYMENT_REVERSAL,同在交易扣出流。
//   - 退回腿:TX_AUCTION_RETURN。把玩家自己托管的资产还给他,不算新增获得,scene 不按获取封禁拒绝
//     (C++ GainBlockService::CountsAsGain,数据覆写审计 r2-trade#1)。原先退回腿也带 TX_AUCTION_BUY,
//     被 GM 封禁获取的卖家取不回自己托管的物品,买家的退款也进不了账,合同只能转人工。
//     只有付回给托管它的那个角色(escrow_owner_id)才算;人工改绑把收件人换成同账号的另一名角色之后,
//     那名角色拿到的不是自己托管的东西,按成交所得判(2026-11-20 独立评审)。escrow_owner_id 为 0 的旧腿按角色名判。
//   - 交付腿(成交所得、官方发货、人民币交付):TX_AUCTION_BUY,照常判获取封禁。
//
// scene 那边 TX_AUCTION_RETURN 要在交易发放流的允许表里(asset_op_system.cpp 的 kAssetOpStreamRules)。
// 旧 scene 不认识它时回 UNKNOWN、不记账,这里照常按退避重投,升级之后才落地,不会被判成终局拒绝。
func legTxType(l *tradepb.TradeCommerceLeg) uint32 {
	switch {
	case l.GetStream() == 3 && isOfficialReverseLeg(l.GetRole()):
		return uint32(rollbackpb.TransactionType_TX_PAYMENT_REVERSAL)
	case l.GetStream() == 3:
		return uint32(rollbackpb.TransactionType_TX_AUCTION_SELL)
	case isReturnLeg(l.GetRole()) && (l.GetEscrowOwnerId() == 0 || l.GetEscrowOwnerId() == l.GetPlayerId()):
		return uint32(rollbackpb.TransactionType_TX_AUCTION_RETURN)
	default:
		return uint32(rollbackpb.TransactionType_TX_AUCTION_BUY)
	}
}

// isReturnLeg 判断一条发放腿是不是退回本人托管的资产。退回腿的角色名只有这几种:
//   - seller_return、buyer_return、bid_return:撤单、过期、托管不成、竞价退款、交付被拒整单退回时由 plan / planRmb 建;
//   - buyer_surplus:撮合按卖价成交时,把买单按自己限价多冻的差价退给买家(market.go);
//   - "<扣出腿>/refund":人工整单退回给每条已扣出的腿加的(manual.go 的 ManualRefund);
//   - seller_return_<揭榜序号>:求购单揭榜交上的书不符合条款,原样退给揭榜人(wanted_skill_book.go);
//   - 以上几种被人工补发后带的 "#k" 后缀(manual.go 的 reissueRole)。
//
// 交付腿是 seller_receive、buyer_receive、rmb_buyer_credit、official_credit、official_regrant_<修订号> 及其补发。新增退回腿时要在这里登记,
// leg_tx_type_test.go 逐个钉住了现有的角色名。
func isReturnLeg(role string) bool {
	base, _, _ := strings.Cut(role, "#")
	switch base {
	case "seller_return", "buyer_return", "bid_return", "buyer_surplus":
		return true
	}
	return strings.HasSuffix(base, "/refund") || strings.HasPrefix(base, wantedReturnPrefix)
}
