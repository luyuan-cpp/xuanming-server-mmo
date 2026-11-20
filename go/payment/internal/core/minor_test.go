package core

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"payment/internal/channel"
	"payment/internal/data"
	"payment/internal/mall"
	paymentpb "proto/payment"
	"shared/antiaddiction"
)

// 用例时刻:北京时间 2026-11-19 12:00。
var gateNow = time.Date(2026, 11, 19, 12, 0, 0, 0, antiaddiction.Beijing)

const (
	gateOrder   = "00112233-4455-6677-8899-aabbccddeeff"
	gateBuyerID = 1001
)

// piBorn 拼一个出生日期为 y-m-d 的 pi:前 6 位是 YYYYMMDD 的 26 进制(技术规范 V1.9),后 32 位取规范样例。
func piBorn(y, m, d int) string {
	const digits = "0123456789abcdefghijklmnop"
	v := y*10000 + m*100 + d
	head := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		head[i] = digits[v%26]
		v /= 26
	}
	return string(head) + "09b57f3f8185f8cb5094ea3f26278efb"
}

type gateNames struct {
	pi    string
	found bool
	err   error
	calls int
}

func (g *gateNames) RealNameOf(context.Context, uint64) (string, bool, error) {
	g.calls++
	return g.pi, g.found, g.err
}

// gateMall、gateStore 只实现用例走到的方法;嵌入的接口是 nil,走到别的方法就 panic,说明用例的假设错了。
type gateMall struct {
	Mall
	order mall.Order
}

func (m gateMall) OrderView(context.Context, string) (mall.Order, error) { return m.order, nil }
func (m gateMall) ReportRefund(context.Context, mall.Refund) error       { return nil }

type gateStore struct {
	Store
	intents    map[[2]uint64]*paymentpb.PaymentIntentRecord
	txns       map[[3]uint64]*paymentpb.PaymentTxnRecord
	reserveErr error
	within     bool
	inserted   int
	reserved   []data.SpendLimit
	revives    []bool // 每次 ReviveMinorIntent 的 force
	marked     int
	states     []uint32
}

func newGateStore() *gateStore {
	return &gateStore{intents: map[[2]uint64]*paymentpb.PaymentIntentRecord{}, txns: map[[3]uint64]*paymentpb.PaymentTxnRecord{}}
}

func (s *gateStore) Intent(_ context.Context, hi, lo uint64) (*paymentpb.PaymentIntentRecord, error) {
	if rec, ok := s.intents[[2]uint64{hi, lo}]; ok {
		return rec, nil
	}
	return nil, data.ErrNotFound
}

func (s *gateStore) InsertIntent(_ context.Context, rec *paymentpb.PaymentIntentRecord) (*paymentpb.PaymentIntentRecord, bool, error) {
	s.inserted++
	s.intents[[2]uint64{rec.OrderHi, rec.OrderLo}] = rec
	return rec, true, nil
}

func (s *gateStore) ReserveIntent(_ context.Context, rec *paymentpb.PaymentIntentRecord, limit data.SpendLimit) (*paymentpb.PaymentIntentRecord, bool, error) {
	s.reserved = append(s.reserved, limit)
	if s.reserveErr != nil {
		return nil, false, s.reserveErr
	}
	s.intents[[2]uint64{rec.OrderHi, rec.OrderLo}] = rec
	return rec, true, nil
}

func (s *gateStore) ReviveMinorIntent(_ context.Context, _ *paymentpb.PaymentIntentRecord, _ data.SpendLimit, _, _ uint64, force bool) (bool, error) {
	s.revives = append(s.revives, force)
	return s.within, nil
}

func (s *gateStore) Txn(_ context.Context, ch uint32, hi, lo uint64) (*paymentpb.PaymentTxnRecord, error) {
	if rec, ok := s.txns[[3]uint64{uint64(ch), hi, lo}]; ok {
		return rec, nil
	}
	return nil, data.ErrNotFound
}

func (s *gateStore) InsertTxn(_ context.Context, rec *paymentpb.PaymentTxnRecord) (*paymentpb.PaymentTxnRecord, bool, error) {
	s.txns[[3]uint64{uint64(rec.Channel), rec.TxnHi, rec.TxnLo}] = rec
	return rec, true, nil
}

func (s *gateStore) MarkIntentPaid(context.Context, uint64, uint64, uint64) error {
	s.marked++
	return nil
}

func (s *gateStore) FlipFact(context.Context, *paymentpb.PaymentTxnRecord, uint64, uint64) (bool, error) {
	return true, nil
}

func (s *gateStore) SetIntentState(_ context.Context, _, _ uint64, state uint32, _ uint64) error {
	s.states = append(s.states, state)
	return nil
}

func (s *gateStore) ClaimRefund(context.Context, *paymentpb.PaymentTxnRecord, uint64, uint64) (bool, error) {
	return true, nil
}

func (s *gateStore) SaveRefund(context.Context, *paymentpb.PaymentTxnRecord, uint64, data.RefundProgress, uint64) error {
	return nil
}

// 两个桩渠道借 devpay 的实现,只换身份:抖音小游戏(平台管限额)与 App Store(撤销能被撤回)。
type douyinStub struct{ *channel.DevPay }

func (douyinStub) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_DOUYIN_MINIGAME }
func (douyinStub) Name() string             { return channel.NameDouyinMinigame }

type appleStub struct{ *channel.DevPay }

func (appleStub) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_APPLE }
func (appleStub) Name() string             { return channel.NameApple }

func gateService(t *testing.T, names RealNames, store *gateStore, amount uint64, currency string) *Service {
	t.Helper()
	devpay, err := channel.NewDevPay("0123456789abcdef0123456789abcdef", nil)
	if err != nil {
		t.Fatal(err)
	}
	order := mall.Order{OrderID: gateOrder, BuyerID: "1001", SkuID: 1, Quantity: 1, AmountMinor: amount, Currency: currency,
		State: mall.OrderStateAwaitingPayment, PaymentDeadlineMs: uint64(gateNow.Add(30 * time.Minute).UnixMilli())}
	platforms := map[paymentpb.PayClientPlatform][]paymentpb.PayChannel{
		paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_ANDROID_APP: {paymentpb.PayChannel_PAY_CHANNEL_DEVPAY, paymentpb.PayChannel_PAY_CHANNEL_DOUYIN_MINIGAME},
	}
	svc, err := New(store, gateMall{order: order}, []channel.Channel{devpay, douyinStub{devpay}, appleStub{devpay}}, platforms,
		Options{RealNames: names, DailyLimitFenAge8To16: 10000, DailyLimitFenAge16To18: 20000})
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return gateNow }
	return svc
}

func createGateIntent(svc *Service, ch paymentpb.PayChannel) error {
	_, err := svc.CreateIntent(context.Background(), CreateRequest{Player: gateBuyerID, OrderID: gateOrder, Channel: ch,
		Platform: paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_ANDROID_APP})
	return err
}

// 谁经闸、怎么拒:闸没开时不查实名;小游戏渠道上没实名的账号交给平台,有实名的照样判、照样计额;App 渠道没实名、
// 未满 8 周岁、超单次上限、非人民币、月 / 日累计超限各回各的码;成年人不预占;读不到实名回「繁忙」(小游戏渠道也一样)。
func TestCreateIntentMinorSpendingGate(t *testing.T) {
	devpay, douyin := paymentpb.PayChannel_PAY_CHANNEL_DEVPAY, paymentpb.PayChannel_PAY_CHANNEL_DOUYIN_MINIGAME
	minor, adult := piBorn(2011, 6, 1), piBorn(2000, 1, 1)
	cases := []struct {
		name       string
		gateOff    bool
		pi         string
		found      bool
		lookupErr  error
		channel    paymentpb.PayChannel
		amount     uint64
		currency   string
		reserveErr error
		want       error
		lookups    int
		inserted   int
		reserved   int
	}{
		{name: "gate off: no lookup", gateOff: true, pi: minor, found: true, channel: devpay, amount: 9900, currency: "CNY", inserted: 1},
		{name: "mini-game accounts without a real name are left to the platform", found: true, channel: douyin, amount: 9900, currency: "CNY",
			lookups: 1, inserted: 1},
		{name: "a minor with a real name is limited on mini-game channels too", pi: minor, found: true, channel: douyin, amount: 5001, currency: "CNY",
			want: ErrMinorSingleLimit, lookups: 1},
		{name: "mini-game spending of a minor is reserved", pi: minor, found: true, channel: douyin, amount: 5000, currency: "CNY", lookups: 1, reserved: 1},
		{name: "mini-game channels fail closed when the directory is down", lookupErr: errors.New("down"), channel: douyin, amount: 600, currency: "CNY",
			want: ErrBusy, lookups: 1},
		{name: "adults pay without a reservation", pi: adult, found: true, channel: devpay, amount: 99900, currency: "CNY", lookups: 1, inserted: 1},
		{name: "no real name", found: true, channel: devpay, amount: 600, currency: "CNY", want: ErrRealNameRequired, lookups: 1},
		{name: "buyer unknown to the directory", channel: devpay, amount: 600, currency: "CNY", want: ErrRealNameRequired, lookups: 1},
		{name: "directory unreachable", lookupErr: errors.New("down"), channel: devpay, amount: 600, currency: "CNY", want: ErrBusy, lookups: 1},
		{name: "under 8", pi: piBorn(2019, 6, 1), found: true, channel: devpay, amount: 100, currency: "CNY", want: ErrMinorNotAllowed, lookups: 1},
		{name: "over the single-payment limit", pi: minor, found: true, channel: devpay, amount: 5001, currency: "CNY", want: ErrMinorSingleLimit, lookups: 1},
		{name: "limits are in yuan", pi: minor, found: true, channel: devpay, amount: 600, currency: "USD", want: ErrCurrencyUnsupported, lookups: 1},
		{name: "monthly limit", pi: minor, found: true, channel: devpay, amount: 5000, currency: "CNY", reserveErr: data.ErrMonthlyLimit,
			want: ErrMinorMonthlyLimit, lookups: 1, reserved: 1},
		{name: "daily limit", pi: minor, found: true, channel: devpay, amount: 5000, currency: "CNY", reserveErr: data.ErrDailyLimit,
			want: ErrMinorDailyLimit, lookups: 1, reserved: 1},
		{name: "a minor within the limits is reserved", pi: minor, found: true, channel: devpay, amount: 5000, currency: "CNY", lookups: 1, reserved: 1},
	}
	for _, c := range cases {
		names := &gateNames{pi: c.pi, found: c.found, err: c.lookupErr}
		var gate RealNames = names
		if c.gateOff {
			gate = nil
		}
		store := newGateStore()
		store.reserveErr = c.reserveErr
		err := createGateIntent(gateService(t, gate, store, c.amount, c.currency), c.channel)
		if !errors.Is(err, c.want) || names.calls != c.lookups || store.inserted != c.inserted || len(store.reserved) != c.reserved {
			t.Errorf("%s: err=%v lookups=%d inserted=%d reserved=%d", c.name, err, names.calls, store.inserted, len(store.reserved))
		}
	}
}

// 预占带的参数:按年龄段的每月上限与配置的单日上限,北京时间本月一日、本日零点;意图上记下 pi 派生的主体键与周岁。
func TestCreateIntentReservesAgainstTheBuyersAgeBracket(t *testing.T) {
	if piBorn(2010, 1, 1)[:6] != "1hpfml" {
		t.Fatal("piBorn must agree with the specification sample")
	}
	monthStart := uint64(time.Date(2026, 11, 1, 0, 0, 0, 0, antiaddiction.Beijing).UnixMilli())
	dayStart := uint64(time.Date(2026, 11, 19, 0, 0, 0, 0, antiaddiction.Beijing).UnixMilli())
	for _, c := range []struct {
		pi     string
		amount uint64
		age    uint32
		limit  data.SpendLimit
	}{
		{piBorn(2011, 6, 1), 5000, 15, data.SpendLimit{MonthlyFen: 20000, DailyFen: 10000, MonthStartMs: monthStart, DayStartMs: dayStart}},
		{piBorn(2009, 6, 1), 10000, 17, data.SpendLimit{MonthlyFen: 40000, DailyFen: 20000, MonthStartMs: monthStart, DayStartMs: dayStart}},
	} {
		store := newGateStore()
		if err := createGateIntent(gateService(t, &gateNames{pi: c.pi, found: true}, store, c.amount, "CNY"), paymentpb.PayChannel_PAY_CHANNEL_DEVPAY); err != nil {
			t.Fatalf("age %d: %v", c.age, err)
		}
		hi, lo, _ := data.OrderKey(gateOrder)
		got := store.intents[[2]uint64{hi, lo}]
		wantHi, wantLo := data.SpenderKey(c.pi)
		if len(store.reserved) != 1 || store.reserved[0] != c.limit || got.SpenderHi != wantHi || got.SpenderLo != wantLo || got.MinorAge != c.age {
			t.Errorf("age %d: reserved=%+v intent spender=%d/%d age=%d", c.age, store.reserved, got.SpenderHi, got.SpenderLo, got.MinorAge)
		}
	}
}

// 未成年人的意图到期关闭(额度已释放)之后才到的钱:锁下复核,超限的转人工、不改意图,不超限的照常记账。
func TestLatePaymentOfAMinorIsRecheckedUnderTheLimit(t *testing.T) {
	for _, within := range []bool{false, true} {
		store := newGateStore()
		store.within = within
		hi, lo, _ := data.OrderKey(gateOrder)
		store.intents[[2]uint64{hi, lo}] = &paymentpb.PaymentIntentRecord{OrderHi: hi, OrderLo: lo, OrderId: gateOrder, PlayerId: gateBuyerID,
			Channel: uint32(paymentpb.PayChannel_PAY_CHANNEL_DEVPAY), AmountMinor: 5000, Currency: "CNY", State: data.IntentClosed,
			CreatedMs: uint64(gateNow.Add(-96 * time.Hour).UnixMilli()), SpenderHi: 1, SpenderLo: 2, MinorAge: 15}
		svc := gateService(t, nil, store, 5000, "CNY")
		ch, _ := svc.Channel(channel.NameDevPay)
		outTradeNo, _ := data.OutTradeNo(gateOrder)
		if err := svc.RecordPayment(context.Background(), ch, channel.Payment{TransactionID: "devpay-late", OutTradeNo: outTradeNo,
			AmountMinor: 5000, Currency: "CNY", PaidAt: gateNow}); err != nil {
			t.Fatalf("within=%v: %v", within, err)
		}
		txHi, txLo := data.TxnKey("devpay-late")
		rec := store.txns[[3]uint64{uint64(paymentpb.PayChannel_PAY_CHANNEL_DEVPAY), txHi, txLo}]
		wantState, wantMarked := data.MallOrphan, 0
		if within {
			wantState, wantMarked = data.MallPending, 1
		}
		if rec == nil || rec.MallState != wantState || store.marked != wantMarked || !slices.Equal(store.revives, []bool{false}) {
			t.Errorf("within=%v: txn=%+v marked=%d revives=%v", within, rec, store.marked, store.revives)
		}
	}
}

// 渠道翻转只跟订单本身那笔钱:重复付款(商城已转退款)被撤销,不关意图、不释放额度;订单那笔(已绑定,或取消后才到账、
// 商城为它本身建了退款)被撤销,关意图;未成年人那笔撤销被撤回(App Store),在额度行锁下重新计入(force)。
func TestChannelFlipsFollowOnlyTheOrdersOwnPayment(t *testing.T) {
	hi, lo, _ := data.OrderKey(gateOrder)
	apple := uint32(paymentpb.PayChannel_PAY_CHANNEL_APPLE)
	txHi, txLo := data.TxnKey("tx-1")
	for _, c := range []struct {
		name        string
		mallState   uint32
		reason      string
		revision    uint32
		revoked     bool
		wantStates  []uint32
		wantRevives []bool
	}{
		{"revoking a duplicate payment", data.MallRefundQueued, "DUPLICATE_PAYMENT", 0, true, nil, nil},
		{"revoking the order's own payment", data.MallBound, "", 0, true, []uint32{data.IntentClosed}, nil},
		{"revoking the order's own late payment", data.MallRefundQueued, "CANCELLED_LATE", 0, true, []uint32{data.IntentClosed}, nil},
		{"reinstating a minor's payment", data.MallBound, "", 1, false, nil, []bool{true}},
	} {
		store := newGateStore()
		store.intents[[2]uint64{hi, lo}] = &paymentpb.PaymentIntentRecord{OrderHi: hi, OrderLo: lo, OrderId: gateOrder, PlayerId: gateBuyerID,
			Channel: apple, AmountMinor: 5000, Currency: "CNY", State: data.IntentClosed, SpenderHi: 1, SpenderLo: 2, MinorAge: 15}
		store.txns[[3]uint64{uint64(apple), txHi, txLo}] = &paymentpb.PaymentTxnRecord{Channel: apple, TxnHi: txHi, TxnLo: txLo, TransactionId: "tx-1",
			OrderHi: hi, OrderLo: lo, MallState: c.mallState, MallRefundReason: c.reason, FactRevision: c.revision, FactObservedMs: 1}
		svc := gateService(t, nil, store, 5000, "CNY")
		ch, _ := svc.Channel(channel.NameApple)
		note := channel.Notification{Revocations: []channel.Revocation{{TransactionID: "tx-1", Revoked: c.revoked, ObservedAt: gateNow}}}
		if err := svc.HandleNotification(context.Background(), ch, note); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal(store.states, c.wantStates) || !slices.Equal(store.revives, c.wantRevives) {
			t.Errorf("%s: states=%v revives=%v", c.name, store.states, store.revives)
		}
	}
}

// 商城退的是订单绑定的那笔(取消后才到账、交付被拒):渠道退成就关意图,未成年人的额度随之释放;退重复付款不动意图。
func TestRefundOfTheOrdersOwnPaymentClosesTheIntent(t *testing.T) {
	hi, lo, _ := data.OrderKey(gateOrder)
	devpay := uint32(paymentpb.PayChannel_PAY_CHANNEL_DEVPAY)
	txHi, txLo := data.TxnKey("devpay-own")
	for _, c := range []struct {
		reason     string
		wantStates []uint32
	}{
		{"CANCELLED_LATE", []uint32{data.IntentClosed}},
		{"DELIVERY_REJECTED", []uint32{data.IntentClosed}},
		{"DUPLICATE_PAYMENT", nil},
	} {
		store := newGateStore()
		store.txns[[3]uint64{uint64(devpay), txHi, txLo}] = &paymentpb.PaymentTxnRecord{Channel: devpay, TxnHi: txHi, TxnLo: txLo, TransactionId: "devpay-own",
			OrderHi: hi, OrderLo: lo, PaidAmountMinor: 5000, PaidCurrency: "CNY", MallState: data.MallRefundQueued, MallRefundReason: c.reason}
		svc := gateService(t, nil, store, 5000, "CNY")
		ch, _ := svc.Channel(channel.NameDevPay)
		item := mall.PendingRefund{OrderID: gateOrder, Channel: channel.NameDevPay, TransactionID: "devpay-own", AmountMinor: 5000, Currency: "CNY", Reason: c.reason}
		if err := svc.refundOne(context.Background(), ch, item); err != nil {
			t.Fatalf("%s: %v", c.reason, err)
		}
		if !slices.Equal(store.states, c.wantStates) {
			t.Errorf("%s: states=%v", c.reason, store.states)
		}
	}
}
