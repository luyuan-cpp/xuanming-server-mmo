package data

import "testing"

// 主键换算是三方对账的地基:订单号 ↔ 主键两列 ↔ 报给渠道的商户单号必须一一对应,交易号与退款单号的派生必须稳定。
// 期望值由与本实现无关的 Python(hashlib)离线算出。
func TestOrderKeysRoundTrip(t *testing.T) {
	const orderID = "00112233-4455-6677-8899-aabbccddeeff"
	hi, lo, err := OrderKey(orderID)
	if err != nil || hi != 0x0011223344556677 || lo != 0x8899aabbccddeeff {
		t.Fatalf("OrderKey = %#x %#x %v", hi, lo, err)
	}
	if got := OrderID(hi, lo); got != orderID {
		t.Fatalf("OrderID = %q", got)
	}
	outTradeNo, err := OutTradeNo(orderID)
	if err != nil || outTradeNo != "00112233445566778899aabbccddeeff" {
		t.Fatalf("OutTradeNo = %q %v", outTradeNo, err)
	}
	if back, err := OrderIDFromOutTradeNo(outTradeNo); err != nil || back != orderID {
		t.Fatalf("OrderIDFromOutTradeNo = %q %v", back, err)
	}
	for _, bad := range []string{"", "00000000-0000-0000-0000-000000000000", "00112233-4455-6677-8899-AABBCCDDEEFF", "00112233445566778899aabbccddeeff", orderID + "0"} {
		if _, _, err := OrderKey(bad); err == nil {
			t.Fatalf("OrderKey accepted %q", bad)
		}
	}
	for _, bad := range []string{"", "00000000000000000000000000000000", "0011223344556677-8899aabbccddeeff", "00112233445566778899AABBCCDDEEFF"} {
		if _, err := OrderIDFromOutTradeNo(bad); err == nil {
			t.Fatalf("OrderIDFromOutTradeNo accepted %q", bad)
		}
	}
}

func TestTransactionAndRefundKeysAreStable(t *testing.T) {
	hi, lo := TxnKey("tx-1")
	if hi != 314959043592269601 || lo != 3807249578561971725 {
		t.Fatalf("TxnKey = %d %d", hi, lo)
	}
	if got := OutRefundNo("wechat_pay", "tx-1"); got != "rf3e799fbd4e673d5aa81322f02f26e6" {
		t.Fatalf("OutRefundNo = %q", got)
	}
	if OutRefundNo("alipay", "tx-1") == OutRefundNo("wechat_pay", "tx-1") {
		t.Fatal("同一交易号在不同渠道必须得到不同的退款单号")
	}
	for id, want := range map[string]bool{"4200001234567890": true, "tx_1-A": true, "": false, "a b": false, "中文": false} {
		if ValidTransactionID(id) != want {
			t.Fatalf("ValidTransactionID(%q) != %v", id, want)
		}
	}
	if Revoked(0) || !Revoked(1) || Revoked(2) || !Revoked(3) {
		t.Fatal("奇数修订号 = 已撤销")
	}
}
