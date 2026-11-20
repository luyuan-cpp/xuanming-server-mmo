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
		{"official_regrant_2", 4, buy},
		{"official_reverse_1", 3, uint32(rollbackpb.TransactionType_TX_PAYMENT_REVERSAL)},
		{"official_reverse_3#1", 3, uint32(rollbackpb.TransactionType_TX_PAYMENT_REVERSAL)},
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

// 退回腿只有付回给托管它的那个角色(escrow_owner_id)才算退回:人工改绑把收件人换成同账号的另一名角色之后,
// 那名角色拿到的不是自己托管的东西,按成交所得判,照常过获取封禁(2026-11-20 独立评审)。交付腿不看托管者。
func TestLegTxTypeReturnsOnlyToTheEscrowOwner(t *testing.T) {
	ret := uint32(rollbackpb.TransactionType_TX_AUCTION_RETURN)
	buy := uint32(rollbackpb.TransactionType_TX_AUCTION_BUY)
	cases := []struct {
		name string
		leg  *tradepb.TradeCommerceLeg
		want uint32
	}{
		{"付回本人", &tradepb.TradeCommerceLeg{Role: "seller_return", Stream: 4, PlayerId: 7, EscrowOwnerId: 7}, ret},
		{"改绑后付给另一名角色", &tradepb.TradeCommerceLeg{Role: "seller_return", Stream: 4, PlayerId: 9, EscrowOwnerId: 7}, buy},
		{"改绑后的补发腿", &tradepb.TradeCommerceLeg{Role: "buyer_debit/refund#2", Stream: 4, PlayerId: 9, EscrowOwnerId: 7}, buy},
		{"差价退回付回买家本人", &tradepb.TradeCommerceLeg{Role: "buyer_surplus", Stream: 4, PlayerId: 3, EscrowOwnerId: 3}, ret},
		{"没记托管者的旧腿按角色名判", &tradepb.TradeCommerceLeg{Role: "bid_return", Stream: 4, PlayerId: 9}, ret},
		{"交付腿不看托管者", &tradepb.TradeCommerceLeg{Role: "buyer_receive", Stream: 4, PlayerId: 2, EscrowOwnerId: 2}, buy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legTxType(tc.leg); got != tc.want {
				t.Fatalf("legTxType(%v) = %d, want %d", tc.leg, got, tc.want)
			}
		})
	}
}
