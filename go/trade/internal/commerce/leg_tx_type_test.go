package commerce

import (
	"testing"

	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"
)

// 退回本人托管的资产带 TX_AUCTION_RETURN(scene 不判获取封禁),成交所得带 TX_AUCTION_BUY,扣出带 TX_AUCTION_SELL
// (数据覆写审计 r2-trade#1)。角色名逐个钉住:plan / planRmb / manual 新增退回腿时,这里要跟着登记。
func TestLegTxType(t *testing.T) {
	sell := uint32(rollbackpb.TransactionType_TX_AUCTION_SELL)
	ret := uint32(rollbackpb.TransactionType_TX_AUCTION_RETURN)
	buy := uint32(rollbackpb.TransactionType_TX_AUCTION_BUY)
	cases := []struct {
		role   string
		stream uint32
		want   uint32
	}{
		{"seller_debit", 3, sell},
		{"buyer_debit", 3, sell},
		{"bid_debit", 3, sell},
		{"buyer_debit_3", 3, sell},
		{"seller_return", 4, ret},
		{"buyer_return", 4, ret},
		{"bid_return", 4, ret},
		{"seller_debit/refund", 4, ret},
		{"buyer_debit_3/refund", 4, ret},
		{"seller_return#2", 4, ret},
		{"buyer_debit/refund#3", 4, ret},
		{"buyer_surplus", 4, ret},
		{"buyer_surplus#2", 4, ret},
		{"seller_receive", 4, buy},
		{"buyer_receive", 4, buy},
		{"buyer_receive#2", 4, buy},
		{"seller_receive#2", 4, buy},
		{"rmb_buyer_credit", 4, buy},
		{"official_credit", 4, buy},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			got := legTxType(&tradepb.TradeCommerceLeg{Role: tc.role, Stream: tc.stream})
			if got != tc.want {
				t.Fatalf("legTxType(%q, stream %d) = %d, want %d", tc.role, tc.stream, got, tc.want)
			}
		})
	}
}
