package core

import (
	"context"
	"testing"
	"time"

	"payment/internal/channel"
	"payment/internal/data"
	"payment/internal/mall"
	paymentpb "proto/payment"
)

// 用例的桩与夹具(gateStore、gateService 等)在 minor_test.go。

func (s *gateStore) CloseIntent(context.Context, uint64, uint64, uint64) (bool, error) {
	s.closed++
	return true, nil
}

// googleStub 是商店渠道的桩:借 devpay 的实现,补上商店核实的两个方法。
type googleStub struct {
	*channel.DevPay
	verify func(context.Context, channel.Receipt) (channel.QueryResult, error)
}

func (googleStub) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_GOOGLE }
func (googleStub) Name() string             { return channel.NameGoogle }
func (g googleStub) Verify(ctx context.Context, r channel.Receipt) (channel.QueryResult, error) {
	if g.verify != nil {
		return g.verify(ctx, r)
	}
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

func TestSameFactRejectsMismatchedPaidAmountOrCurrency(t *testing.T) {
	base := &paymentpb.PaymentTxnRecord{TransactionId: "tx-1", OrderHi: 1, OrderLo: 2, PaidAmountMinor: 5000, PaidCurrency: "CNY"}
	same := &paymentpb.PaymentTxnRecord{TransactionId: "tx-1", OrderHi: 1, OrderLo: 2, PaidAmountMinor: 5000, PaidCurrency: "cny"}
	diffAmount := &paymentpb.PaymentTxnRecord{TransactionId: "tx-1", OrderHi: 1, OrderLo: 2, PaidAmountMinor: 4999, PaidCurrency: "CNY"}
	diffCurrency := &paymentpb.PaymentTxnRecord{TransactionId: "tx-1", OrderHi: 1, OrderLo: 2, PaidAmountMinor: 5000, PaidCurrency: "USD"}
	if !sameFact(base, same) {
		t.Fatal("sameFact rejected identical paid fact with case-insensitive currency")
	}
	if sameFact(base, diffAmount) {
		t.Fatal("sameFact accepted conflicting PaidAmountMinor for same transaction")
	}
	if sameFact(base, diffCurrency) {
		t.Fatal("sameFact accepted conflicting PaidCurrency for same transaction")
	}
}

// Google Play 孤儿单 / 冲突单必须回 Finish=false(保留 Google 3 天自动退款),正常记账回 Finish=true。
func TestSubmitPurchaseGoogleDoesNotFinishOrphanOrConflict(t *testing.T) {
	devpay, err := channel.NewDevPay("0123456789abcdef0123456789abcdef", nil)
	if err != nil {
		t.Fatal(err)
	}
	hi, lo, _ := data.OrderKey(gateOrder)
	makeSvc := func(store *gateStore, res channel.QueryResult) *Service {
		order := mall.Order{OrderID: gateOrder, BuyerID: "1001", SkuID: 1, Quantity: 1, AmountMinor: 600, Currency: "CNY",
			State: mall.OrderStateAwaitingPayment, PaymentDeadlineMs: uint64(gateNow.Add(30 * time.Minute).UnixMilli())}
		g := googleStub{DevPay: devpay, verify: func(context.Context, channel.Receipt) (channel.QueryResult, error) { return res, nil }}
		svc, err := New(store, gateMall{order: order}, []channel.Channel{g}, map[paymentpb.PayClientPlatform][]paymentpb.PayChannel{
			paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_ANDROID_APP: {paymentpb.PayChannel_PAY_CHANNEL_GOOGLE},
		}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		svc.now = func() time.Time { return gateNow }
		return svc
	}

	// 1. 孤儿单(商品异常)→ Finish=false
	store := newGateStore()
	store.intents[[2]uint64{hi, lo}] = &paymentpb.PaymentIntentRecord{OrderHi: hi, OrderLo: lo, OrderId: gateOrder, PlayerId: gateBuyerID,
		Channel: uint32(paymentpb.PayChannel_PAY_CHANNEL_GOOGLE), AmountMinor: 600, Currency: "CNY", State: data.IntentCreated}
	svc := makeSvc(store, channel.QueryResult{State: channel.TradePaid, Payment: &channel.Payment{
		TransactionID: "g-orphan", OrderID: gateOrder, PaidAt: gateNow, StorePriced: true, Anomaly: "quantity mismatch",
	}})
	sub, err := svc.SubmitPurchase(context.Background(), SubmitRequest{Player: gateBuyerID, Channel: paymentpb.PayChannel_PAY_CHANNEL_GOOGLE,
		OrderID: gateOrder, PurchaseToken: "tok-orphan"})
	if err != nil || sub.Finish {
		t.Fatalf("orphan google submit: finish=%v err=%v, want finish=false err=nil", sub.Finish, err)
	}

	// 2. 正常单 → Finish=true
	store2 := newGateStore()
	store2.intents[[2]uint64{hi, lo}] = &paymentpb.PaymentIntentRecord{OrderHi: hi, OrderLo: lo, OrderId: gateOrder, PlayerId: gateBuyerID,
		Channel: uint32(paymentpb.PayChannel_PAY_CHANNEL_GOOGLE), AmountMinor: 600, Currency: "CNY", State: data.IntentCreated}
	svc2 := makeSvc(store2, channel.QueryResult{State: channel.TradePaid, Payment: &channel.Payment{
		TransactionID: "g-ok", OrderID: gateOrder, PaidAt: gateNow, StorePriced: true,
	}})
	sub2, err := svc2.SubmitPurchase(context.Background(), SubmitRequest{Player: gateBuyerID, Channel: paymentpb.PayChannel_PAY_CHANNEL_GOOGLE,
		OrderID: gateOrder, PurchaseToken: "tok-ok"})
	if err != nil || !sub2.Finish {
		t.Fatalf("normal google submit: finish=%v err=%v, want finish=true err=nil", sub2.Finish, err)
	}
}

// 乱序保护:
// 1. App Store 先收到较新的 REFUND_REVERSED(T2)再收到滞后的 REFUND(T1<T2)时,fact_observed_ms 已被 T2 推高,旧 REFUND 不得翻转事实。
// 2. 退款通知先于付款通知到达时写下墓碑行(FactRevision=1),晚到的付款通知被按已撤销吞掉、不转冲突、不投商城。
func TestOutOfOrderRevocationsAreSafe(t *testing.T) {
	ctx := context.Background()
	hi, lo, _ := data.OrderKey(gateOrder)
	outTradeNo, _ := data.OutTradeNo(gateOrder)

	// 1. Apple REFUND_REVERSED (T2) before stale REFUND (T1)
	store := newGateStore()
	appleID := uint32(paymentpb.PayChannel_PAY_CHANNEL_APPLE)
	txHi, txLo := data.TxnKey("apple-ooo")
	store.txns[[3]uint64{uint64(appleID), txHi, txLo}] = &paymentpb.PaymentTxnRecord{Channel: appleID, TxnHi: txHi, TxnLo: txLo,
		TransactionId: "apple-ooo", OrderHi: hi, OrderLo: lo, MallState: data.MallBound,
		FactRevision: 0, FactObservedMs: uint64(gateNow.UnixMilli())}
	svc := gateService(t, nil, store, 5000, "CNY")
	appleCh, _ := svc.Channel(channel.NameApple)
	t1 := gateNow.Add(10 * time.Second)
	t2 := gateNow.Add(20 * time.Second)
	if err := svc.HandleNotification(ctx, appleCh, channel.Notification{Revocations: []channel.Revocation{
		{TransactionID: "apple-ooo", Revoked: false, ObservedAt: t2, Detail: "REFUND_REVERSED"},
		{TransactionID: "apple-ooo", Revoked: true, ObservedAt: t1, Detail: "REFUND"},
	}}); err != nil {
		t.Fatalf("apple out-of-order: %v", err)
	}
	got := store.txns[[3]uint64{uint64(appleID), txHi, txLo}]
	if got.FactRevision != 0 || got.FactObservedMs != uint64(t2.UnixMilli()) {
		t.Fatalf("stale REFUND flipped state: rev=%d observed=%d", got.FactRevision, got.FactObservedMs)
	}

	// 2. Refund webhook arrives before delayed payment webhook
	store2 := newGateStore()
	svc2 := gateService(t, nil, store2, 5000, "CNY")
	devCh, _ := svc2.Channel(channel.NameDevPay)
	if err := svc2.HandleNotification(ctx, devCh, channel.Notification{Revocations: []channel.Revocation{
		{TransactionID: "tx-early-refund", OutTradeNo: outTradeNo, Revoked: true, ObservedAt: gateNow, Detail: "REFUND"},
	}}); err != nil {
		t.Fatalf("early refund: %v", err)
	}
	if err := svc2.RecordPayment(ctx, devCh, channel.Payment{
		TransactionID: "tx-early-refund", OutTradeNo: outTradeNo, AmountMinor: 5000, Currency: "CNY", PaidAt: gateNow,
	}); err != nil {
		t.Fatalf("delayed payment after early refund should not fail or conflict: %v", err)
	}
	rHi, rLo := data.TxnKey("tx-early-refund")
	tomb := store2.txns[[3]uint64{uint64(paymentpb.PayChannel_PAY_CHANNEL_DEVPAY), rHi, rLo}]
	if tomb == nil || !data.Revoked(tomb.FactRevision) || tomb.MallState != data.MallOrphan || tomb.MallDue != 0 {
		t.Fatalf("expected revoked orphan tombstone without mall delivery, got %+v", tomb)
	}
}
