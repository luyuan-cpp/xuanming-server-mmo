package core

import (
	"context"
	"testing"

	"payment/internal/channel"
	"payment/internal/data"
	paymentpb "proto/payment"
)

// 用例的桩与夹具(gateStore、gateService 等)在 minor_test.go。

func (s *gateStore) CloseIntent(context.Context, uint64, uint64, uint64) (bool, error) {
	s.closed++
	return true, nil
}

// googleStub 是商店渠道的桩:借 devpay 的实现,补上商店核实的两个方法。
type googleStub struct{ *channel.DevPay }

func (googleStub) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_GOOGLE }
func (googleStub) Name() string             { return channel.NameGoogle }
func (googleStub) Verify(context.Context, channel.Receipt) (channel.QueryResult, error) {
	return channel.QueryResult{}, nil
}
func (googleStub) AccountRef(uint64) string { return "" }

// 记账的第二步按这笔钱是什么改意图:正常的转已付;转人工、金额或币种与意图冻结的对不上的,我们定价的渠道关掉意图
// (一个商户单号只能付成一次,意图再也付不成;不关的话后台每 30 秒查一次渠道、永远停不下,未成年人的在途额度也一直占着),
// 商店渠道不动、照常到期。同一笔再报一次时补做同样的一步。
func TestRecordSettlesTheIntentByWhatThePaymentIs(t *testing.T) {
	for _, c := range []struct {
		name       string
		channel    string
		amount     uint64
		currency   string
		anomaly    string
		wantClosed int
		wantMarked int
	}{
		{"a matching payment marks the intent paid", channel.NameDevPay, 5000, "CNY", "", 0, 2},
		{"an amount mismatch closes the intent", channel.NameDevPay, 4999, "CNY", "", 2, 0},
		{"a currency mismatch closes the intent", channel.NameDevPay, 5000, "USD", "", 2, 0},
		{"a payment handed to manual handling closes the intent", channel.NameDevPay, 5000, "CNY", "anomaly", 2, 0},
		{"manual handling on a store channel leaves the intent waiting", channel.NameGoogle, 5000, "CNY", "anomaly", 0, 0},
	} {
		store := newGateStore()
		hi, lo, _ := data.OrderKey(gateOrder)
		svc := gateService(t, nil, store, 5000, "CNY")
		ch, _ := svc.Channel(c.channel)
		store.intents[[2]uint64{hi, lo}] = &paymentpb.PaymentIntentRecord{OrderHi: hi, OrderLo: lo, OrderId: gateOrder, PlayerId: gateBuyerID,
			Channel: uint32(ch.ID()), AmountMinor: 5000, Currency: "CNY", State: data.IntentCreated}
		outTradeNo, _ := data.OutTradeNo(gateOrder)
		payment := channel.Payment{TransactionID: "tx-settle", OutTradeNo: outTradeNo, AmountMinor: c.amount, Currency: c.currency,
			PaidAt: gateNow, Anomaly: c.anomaly}
		for report := 0; report < 2; report++ {
			if err := svc.RecordPayment(context.Background(), ch, payment); err != nil {
				t.Fatalf("%s, report %d: %v", c.name, report+1, err)
			}
		}
		if store.closed != c.wantClosed || store.marked != c.wantMarked {
			t.Errorf("%s: closed=%d marked=%d, want %d / %d", c.name, store.closed, store.marked, c.wantClosed, c.wantMarked)
		}
	}
}
