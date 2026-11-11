package commerce

import (
	"strings"

	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"
)

// legTxType 是一条腿进资产通道时带的交易类型,scene 按它记流水,并决定要不要判获取封禁:
//   - 扣出(托管)腿:TX_AUCTION_SELL。
//   - 退回腿:TX_AUCTION_RETURN。把玩家自己托管的资产还给他,不算新增获得,scene 不按获取封禁拒绝
//     (C++ GainBlockService::CountsAsGain,数据覆写审计 r2-trade#1)。原先退回腿也带 TX_AUCTION_BUY,
//     被 GM 封禁获取的卖家取不回自己托管的物品,买家的退款也进不了账,合同只能转人工。
//   - 交付腿(成交所得、官方发货、人民币交付):TX_AUCTION_BUY,照常判获取封禁。
//
// scene 那边 TX_AUCTION_RETURN 要在交易发放流的允许表里(asset_op_system.cpp 的 kAssetOpStreamRules)。
// 旧 scene 不认识它时回 UNKNOWN、不记账,这里照常按退避重投,升级之后才落地,不会被判成终局拒绝。
func legTxType(l *tradepb.TradeCommerceLeg) uint32 {
	switch {
	case l.GetStream() == 3:
		return uint32(rollbackpb.TransactionType_TX_AUCTION_SELL)
	case isReturnLeg(l.GetRole()):
		return uint32(rollbackpb.TransactionType_TX_AUCTION_RETURN)
	default:
		return uint32(rollbackpb.TransactionType_TX_AUCTION_BUY)
	}
}

// isReturnLeg 判断一条发放腿是不是退回本人托管的资产。退回腿的角色名只有这几种:
//   - seller_return、buyer_return、bid_return:撤单、过期、托管不成、竞价退款时由 plan / planRmb 建;
//   - "<扣出腿>/refund":人工整单退回给每条已扣出的腿加的(manual.go 的 ManualRefund);
//   - 以上几种被人工补发后带的 "#k" 后缀(manual.go 的 reissueRole)。
//
// 交付腿是 seller_receive、buyer_receive、rmb_buyer_credit、official_credit 及其补发。新增退回腿时要在这里登记,
// leg_tx_type_test.go 逐个钉住了现有的角色名。
func isReturnLeg(role string) bool {
	base, _, _ := strings.Cut(role, "#")
	switch base {
	case "seller_return", "buyer_return", "bid_return":
		return true
	}
	return strings.HasSuffix(base, "/refund")
}
