// Package core 是支付服务的业务:建支付意图、记渠道付款事实、把付款推进到商城、执行商城的退款待办。
//
// 不变量(docs/design/payment-recharge.md §22):
//   - 订单只在商城:买家、金额、币种、截止时刻都读商城冻结值,客户端不提交金额,也不能声称付过款。
//   - 一张订单至多一个支付意图、一个渠道;商户单号由订单号派生,渠道保证同一商户单号至多收一次款。
//   - 钱的事实先落本库(payment_txn,按渠道交易号幂等),再投商城;商城回 200 之前一直重投。投商城与回调应答解耦:
//     回调只等事实落库就应答,商城暂时不可达不会让渠道重发。
//   - 退款只执行商城列出的退款待办,退款单号由被退那笔付款确定性派生,重试不会多退。
package core

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"payment/internal/channel"
	"payment/internal/data"
	"payment/internal/mall"
	paymentpb "proto/payment"
	"shared/authoritysql"
)

// 业务拒绝。server 把它们映射成 tip 码;其余错误(存储失败)一律按服务故障回。
var (
	ErrInvalid             = errors.New("payment: invalid request")
	ErrChannelUnavailable  = errors.New("payment: channel is not available on this platform")
	ErrOrderNotFound       = errors.New("payment: order not found")
	ErrOrderNotPayable     = errors.New("payment: order is not awaiting payment")
	ErrOrderExpired        = errors.New("payment: order payment window has passed")
	ErrChannelMismatch     = errors.New("payment: order already uses another channel")
	ErrCurrencyUnsupported = errors.New("payment: channel does not take the order currency")
	// ErrBusy:渠道或商城这一次没答上来。本服务没有结局未知的写入,玩家稍后重试即可。
	ErrBusy = errors.New("payment: channel or mall is temporarily unreachable")
	// ErrFactConflict:同一渠道交易号再次报来,金额或商户单号却和已记下的不同。不改已有事实,告警人工核对。
	ErrFactConflict = errors.New("payment: channel reported a transaction that differs from the recorded one")
)

// Store 是本服务对自己库的全部依赖。生产装配 *data.Repo,用例用内存替身走同一接口。
type Store interface {
	Intent(ctx context.Context, hi, lo uint64) (*paymentpb.PaymentIntentRecord, error)
	InsertIntent(ctx context.Context, rec *paymentpb.PaymentIntentRecord) (*paymentpb.PaymentIntentRecord, bool, error)
	SetChannelRef(ctx context.Context, hi, lo uint64, ref string, refMs, now uint64) error
	ClaimActiveQuery(ctx context.Context, rec *paymentpb.PaymentIntentRecord, now uint64) (bool, error)
	DueIntents(ctx context.Context, now uint64, limit int) ([]*paymentpb.PaymentIntentRecord, error)
	ClaimCheck(ctx context.Context, rec *paymentpb.PaymentIntentRecord, lease, now uint64) (bool, error)
	ScheduleCheck(ctx context.Context, rec *paymentpb.PaymentIntentRecord, lease, next, now uint64) error
	CloseIntent(ctx context.Context, hi, lo, now uint64) (bool, error)
	MarkIntentPaid(ctx context.Context, hi, lo, now uint64) error
	Txn(ctx context.Context, channel uint32, hi, lo uint64) (*paymentpb.PaymentTxnRecord, error)
	InsertTxn(ctx context.Context, rec *paymentpb.PaymentTxnRecord) (*paymentpb.PaymentTxnRecord, bool, error)
	DueMallTxns(ctx context.Context, now uint64, limit int) ([]*paymentpb.PaymentTxnRecord, error)
	ClaimMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, now uint64) (bool, error)
	SettleMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease uint64, state uint32, reason, lastErr string, now uint64) error
	DeferMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, next uint64, lastErr string, now uint64) error
	ClaimRefund(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, now uint64) (bool, error)
	SaveRefund(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease uint64, p data.RefundProgress, now uint64) error
}

var _ Store = (*data.Repo)(nil)

// Mall 是本服务对商城的全部依赖。生产装配 *mall.Client。
type Mall interface {
	OrderView(ctx context.Context, orderID string) (mall.Order, error)
	RecordPayment(ctx context.Context, payment mall.Payment) (mall.Outcome, error)
	PendingRefunds(ctx context.Context) ([]mall.PendingRefund, error)
	ReportRefund(ctx context.Context, refund mall.Refund) error
}

var _ Mall = (*mall.Client)(nil)

// Options 是时间与批量参数。零值取缺省(withDefaults)。
type Options struct {
	MinPayWindow          time.Duration // 离截止不足这么久就不再调起:微信 time_expire 至少比下单晚 1 分钟
	FirstCheckDelay       time.Duration // 建意图后第一次后台查单
	CheckInterval         time.Duration // 截止前的后台查单间隔
	CloseGrace            time.Duration // 截止后再等这么久,关渠道订单并做最后一次查单
	GiveUpAfter           time.Duration // 截止后这么久还确认不了就关掉意图(迟到的付款仍由回调记账)
	ActiveQueryInterval   time.Duration // 客户端轮询触发查单的最小间隔
	RequestCallTimeout    time.Duration // 请求路径上单次渠道或商城调用的上限
	BackgroundCallTimeout time.Duration // 后台单次渠道或商城调用的上限
	Lease                 time.Duration // 后台领取一行的租约
	Batch                 int           // 后台每轮最多处理的行数
	Interval              time.Duration // 查单与推进商城的轮询间隔
	RefundPollInterval    time.Duration // 拉商城退款待办的间隔
	MallBackoffMax        time.Duration
	RefundBackoffMax      time.Duration
	RefundReason          string // 渠道退款单上给用户看的原因
}

func (o Options) withDefaults() Options {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&o.MinPayWindow, time.Minute)
	set(&o.FirstCheckDelay, time.Minute)
	set(&o.CheckInterval, 2*time.Minute)
	set(&o.CloseGrace, 2*time.Minute)
	set(&o.GiveUpAfter, 24*time.Hour)
	set(&o.ActiveQueryInterval, 3*time.Second)
	set(&o.RequestCallTimeout, 1500*time.Millisecond)
	set(&o.BackgroundCallTimeout, 8*time.Second)
	set(&o.Lease, 30*time.Second)
	set(&o.Interval, 2*time.Second)
	set(&o.RefundPollInterval, 10*time.Second)
	set(&o.MallBackoffMax, 10*time.Minute)
	set(&o.RefundBackoffMax, 30*time.Minute)
	if o.Batch <= 0 {
		o.Batch = 50
	}
	if o.RefundReason == "" {
		o.RefundReason = "游戏订单退款"
	}
	return o
}

type Service struct {
	store     Store
	mall      Mall
	channels  map[paymentpb.PayChannel]channel.Channel
	byName    map[string]channel.Channel
	platforms map[paymentpb.PayClientPlatform][]paymentpb.PayChannel
	opts      Options
	now       func() time.Time
	kick      chan struct{}
}

// New 装配服务。platforms 是每种发行形态开放的渠道(按展示顺序),里面的渠道必须都在 channels 里。
func New(store Store, mallClient Mall, channels []channel.Channel, platforms map[paymentpb.PayClientPlatform][]paymentpb.PayChannel, opts Options) (*Service, error) {
	if store == nil || mallClient == nil {
		return nil, errors.New("payment: store and mall client are required")
	}
	s := &Service{store: store, mall: mallClient, channels: map[paymentpb.PayChannel]channel.Channel{}, byName: map[string]channel.Channel{},
		platforms: map[paymentpb.PayClientPlatform][]paymentpb.PayChannel{}, opts: opts.withDefaults(), now: time.Now, kick: make(chan struct{}, 1)}
	for _, ch := range channels {
		if ch == nil {
			return nil, errors.New("payment: nil channel")
		}
		if _, dup := s.channels[ch.ID()]; dup {
			return nil, errors.New("payment: channel registered twice: " + ch.Name())
		}
		s.channels[ch.ID()] = ch
		s.byName[ch.Name()] = ch
	}
	for platform, list := range platforms {
		for _, id := range list {
			if _, ok := s.channels[id]; !ok {
				return nil, errors.New("payment: platform " + platform.String() + " lists a channel that is not enabled: " + id.String())
			}
		}
		s.platforms[platform] = append([]paymentpb.PayChannel(nil), list...)
	}
	return s, nil
}

// Channel 按商城渠道名找渠道,回调入口按路径用它。
func (s *Service) Channel(name string) (channel.Channel, bool) {
	ch, ok := s.byName[name]
	return ch, ok
}

// Kick 叫醒推进任务:刚记下的付款马上投商城,不等下一个轮询点。
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Service) allowed(platform paymentpb.PayClientPlatform, id paymentpb.PayChannel) bool {
	for _, candidate := range s.platforms[platform] {
		if candidate == id {
			return true
		}
	}
	return false
}

func validPlatform(platform paymentpb.PayClientPlatform) bool {
	_, known := paymentpb.PayClientPlatform_name[int32(platform)]
	return known && platform != paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_UNSPECIFIED
}

// ListChannels 是这种发行形态当前开放的渠道。
func (s *Service) ListChannels(platform paymentpb.PayClientPlatform) ([]paymentpb.PayChannel, error) {
	if !validPlatform(platform) {
		return nil, ErrInvalid
	}
	return append([]paymentpb.PayChannel(nil), s.platforms[platform]...), nil
}

// Created 是建意图的结果。Intent 在业务拒绝时也可能有值(已有意图的当前状态)。
type Created struct {
	Intent *paymentpb.PayIntentView
	Params channel.ClientParams
}

// CreateIntent 为玩家的一张待付订单建支付意图并签出调起参数。重复调用幂等:同一订单、同一渠道再签一次参数。
func (s *Service) CreateIntent(ctx context.Context, player uint64, orderID string, id paymentpb.PayChannel, platform paymentpb.PayClientPlatform) (Created, error) {
	if player == 0 || !validPlatform(platform) {
		return Created{}, ErrInvalid
	}
	hi, lo, err := data.OrderKey(orderID)
	if err != nil {
		return Created{}, ErrInvalid
	}
	ch, ok := s.channels[id]
	if !ok || !s.allowed(platform, id) {
		return Created{}, ErrChannelUnavailable
	}
	now := s.now()
	intent, err := s.store.Intent(ctx, hi, lo)
	switch {
	case err == nil:
	case errors.Is(err, data.ErrNotFound):
		if intent, err = s.openIntent(ctx, player, orderID, hi, lo, ch, now); err != nil {
			return Created{}, err
		}
	default:
		return Created{}, err
	}
	if intent.PlayerId != player {
		return Created{}, ErrOrderNotFound // 别人的订单表现为不存在,不泄露它是否存在
	}
	view := intentView(intent)
	if intent.Channel != uint32(id) {
		return Created{Intent: view}, ErrChannelMismatch
	}
	switch intent.State {
	case data.IntentPaid:
		return Created{Intent: view}, nil
	case data.IntentClosed:
		return Created{Intent: view}, ErrOrderExpired
	}
	if uint64(now.Add(s.opts.MinPayWindow).UnixMilli()) >= intent.ExpireMs {
		return Created{Intent: view}, ErrOrderExpired
	}
	prepared, err := s.prepare(ctx, ch, intent, now)
	if errors.Is(err, channel.ErrOrderPaid) {
		// 渠道说这单已经付过:当场查单记账,玩家看到的是已付,而不是一个失败。
		refreshed, queryErr := s.activeQuery(ctx, intent, true)
		if queryErr != nil {
			return Created{Intent: view}, queryErr
		}
		return Created{Intent: intentView(refreshed)}, nil
	}
	if err != nil {
		if authoritysql.ClassifyOutcome(err) == authoritysql.OutcomeUnknown {
			return Created{Intent: view}, err
		}
		return Created{Intent: view}, ErrBusy
	}
	countIntent(ch.Name(), "prepared")
	return Created{Intent: view, Params: prepared.Params}, nil
}

// openIntent 读商城订单的冻结条款,核对后新建意图。商城只读失败时本服务什么都没写,回 ErrBusy。
func (s *Service) openIntent(ctx context.Context, player uint64, orderID string, hi, lo uint64, ch channel.Channel, now time.Time) (*paymentpb.PaymentIntentRecord, error) {
	callCtx, cancel := context.WithTimeout(ctx, s.opts.RequestCallTimeout)
	order, err := s.mall.OrderView(callCtx, orderID)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, mall.ErrNotFound), errors.Is(err, mall.ErrInvalid):
		return nil, ErrOrderNotFound
	default:
		logx.WithContext(ctx).Errorf("[payment] Reading the mall order view failed: %v", err)
		return nil, ErrBusy
	}
	switch {
	case order.BuyerID != strconv.FormatUint(player, 10):
		return nil, ErrOrderNotFound
	case order.State != mall.OrderStateAwaitingPayment || order.AmountMinor == 0:
		return nil, ErrOrderNotPayable
	case !ch.Supports(order.Currency):
		return nil, ErrCurrencyUnsupported
	case order.PaymentDeadlineMs <= uint64(now.Add(s.opts.MinPayWindow).UnixMilli()):
		return nil, ErrOrderExpired
	}
	nowMs := uint64(now.UnixMilli())
	rec := &paymentpb.PaymentIntentRecord{OrderHi: hi, OrderLo: lo, OrderId: orderID, PlayerId: player, Channel: uint32(ch.ID()),
		AmountMinor: order.AmountMinor, Currency: order.Currency, State: data.IntentCreated, ExpireMs: order.PaymentDeadlineMs,
		NextCheckMs: uint64(now.Add(s.opts.FirstCheckDelay).UnixMilli()), CreatedMs: nowMs, UpdatedMs: nowMs}
	stored, inserted, err := s.store.InsertIntent(ctx, rec)
	if err != nil {
		return nil, err
	}
	if inserted {
		countIntent(ch.Name(), "created")
	}
	return stored, nil
}

// prepare 向渠道要调起参数,新拿到的预下单引用存回意图。
func (s *Service) prepare(ctx context.Context, ch channel.Channel, intent *paymentpb.PaymentIntentRecord, now time.Time) (channel.Prepared, error) {
	outTradeNo, err := data.OutTradeNo(intent.OrderId)
	if err != nil {
		return channel.Prepared{}, err
	}
	cached := channel.Ref{Value: intent.ChannelRef}
	if intent.ChannelRefMs != 0 {
		cached.At = time.UnixMilli(int64(intent.ChannelRefMs))
	}
	callCtx, cancel := context.WithTimeout(ctx, s.opts.RequestCallTimeout)
	prepared, err := ch.Prepare(callCtx, channel.Order{OrderID: intent.OrderId, OutTradeNo: outTradeNo, AmountMinor: intent.AmountMinor,
		Currency: intent.Currency, ExpireAt: time.UnixMilli(int64(intent.ExpireMs))}, cached)
	cancel()
	observeCall(ch.Name(), "prepare", err)
	if err != nil {
		return channel.Prepared{}, err
	}
	if prepared.Ref.Value != "" {
		// 引用只是缓存:没存上,下次再调起会重新下单,这次的参数照样有效。存储结局未知仍要报出去,让排空证明挂起。
		if err := s.store.SetChannelRef(ctx, intent.OrderHi, intent.OrderLo, prepared.Ref.Value, uint64(prepared.Ref.At.UnixMilli()), uint64(now.UnixMilli())); err != nil {
			if authoritysql.ClassifyOutcome(err) == authoritysql.OutcomeUnknown {
				return channel.Prepared{}, err
			}
			logx.WithContext(ctx).Errorf("[payment] Saving the channel prepay reference failed: order=%s: %v", intent.OrderId, err)
		}
	}
	return prepared, nil
}

// GetIntent 是玩家查自己订单的支付状态。等付款期间顺手查一次渠道(限频),回调慢的时候玩家也能很快看到已付。
func (s *Service) GetIntent(ctx context.Context, player uint64, orderID string) (*paymentpb.PayIntentView, error) {
	hi, lo, err := data.OrderKey(orderID)
	if player == 0 || err != nil {
		return nil, ErrInvalid
	}
	intent, err := s.store.Intent(ctx, hi, lo)
	if errors.Is(err, data.ErrNotFound) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	if intent.PlayerId != player {
		return nil, ErrOrderNotFound
	}
	if intent.State == data.IntentCreated {
		if intent, err = s.activeQuery(ctx, intent, false); err != nil {
			return nil, err
		}
	}
	return intentView(intent), nil
}

// activeQuery 在请求路径上查一次渠道。force 为假时按 ActiveQueryInterval 限频,并用 checked_ms 领取,同一时刻只有一个请求去查。
// 查单本身失败不算错(玩家下一次轮询再查);只有写库结局未知才把错误交回去,让 RPC 报故障、排空证明挂起。
func (s *Service) activeQuery(ctx context.Context, intent *paymentpb.PaymentIntentRecord, force bool) (*paymentpb.PaymentIntentRecord, error) {
	ch, ok := s.channels[paymentpb.PayChannel(intent.Channel)]
	if !ok {
		return intent, nil
	}
	nowMs := uint64(s.now().UnixMilli())
	if !force {
		if nowMs < intent.CheckedMs+uint64(s.opts.ActiveQueryInterval.Milliseconds()) {
			return intent, nil
		}
		claimed, err := s.store.ClaimActiveQuery(ctx, intent, nowMs)
		if err != nil {
			return intent, unknownOnly(err)
		}
		if !claimed {
			return intent, nil
		}
	}
	outTradeNo, err := data.OutTradeNo(intent.OrderId)
	if err != nil {
		return intent, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, s.opts.RequestCallTimeout)
	result, err := ch.Query(callCtx, outTradeNo)
	cancel()
	observeCall(ch.Name(), "query", err)
	if err != nil || result.Payment == nil || (result.State != channel.TradePaid && result.State != channel.TradeRefundedOutside) {
		return intent, nil
	}
	if err := s.record(ctx, ch, *result.Payment, result.State == channel.TradeRefundedOutside); err != nil {
		return intent, unknownOnly(err)
	}
	refreshed := proto.Clone(intent).(*paymentpb.PaymentIntentRecord)
	refreshed.State = data.IntentPaid
	if result.State == channel.TradeRefundedOutside {
		refreshed.State = data.IntentClosed
	}
	return refreshed, nil
}

// unknownOnly 只放行「写库结局未知」的错误,其余吞掉:请求路径上的顺手查单是尽力而为,后台还会再查。
func unknownOnly(err error) error {
	if authoritysql.ClassifyOutcome(err) == authoritysql.OutcomeUnknown {
		return err
	}
	return nil
}

// RecordPayment 记一笔渠道核实过的付款(回调入口用)。重复的通知幂等。
func (s *Service) RecordPayment(ctx context.Context, ch channel.Channel, payment channel.Payment) error {
	return s.record(ctx, ch, payment, false)
}

// record 先写付款事实(幂等),再改意图状态,最后叫醒推进任务。两步之间崩溃时意图仍在等付款,下一次查单或回调会补上第二步。
//
// refundedOutside:渠道显示这笔付过、但已在本服务之外转了退款(不是商城退款待办发起的)。记成人工处理,不投商城、不发货,意图关掉。
func (s *Service) record(ctx context.Context, ch channel.Channel, payment channel.Payment, refundedOutside bool) error {
	nowMs := uint64(s.now().UnixMilli())
	rec := &paymentpb.PaymentTxnRecord{Channel: uint32(ch.ID()), TransactionId: payment.TransactionID, OutTradeNo: payment.OutTradeNo,
		PaidAmountMinor: payment.AmountMinor, PaidCurrency: payment.Currency, PaidMs: uint64(payment.PaidAt.UnixMilli()),
		MallState: data.MallPending, NextAttemptMs: nowMs, CreatedMs: nowMs, UpdatedMs: nowMs}
	rec.TxnHi, rec.TxnLo = data.TxnKey(payment.TransactionID)
	orderID, keyErr := data.OrderIDFromOutTradeNo(payment.OutTradeNo)
	switch {
	case !data.ValidTransactionID(payment.TransactionID):
		rec.MallState, rec.MallLastError = data.MallOrphan, "transaction id cannot be reported to the mall"
	case keyErr != nil:
		rec.MallState, rec.MallLastError = data.MallOrphan, "out_trade_no was not issued by this service"
	default:
		rec.OrderHi, rec.OrderLo, _ = data.OrderKey(orderID)
		// 意图不在也照样报商城:商户单号由订单号派生,订单是商城的,买家与金额由商城核对。
		intent, err := s.store.Intent(ctx, rec.OrderHi, rec.OrderLo)
		switch {
		case err == nil:
			rec.PlayerId = intent.PlayerId
		case !errors.Is(err, data.ErrNotFound):
			return err
		}
		if refundedOutside {
			rec.MallState, rec.MallLastError = data.MallOrphan, "refunded at the channel before this service recorded it; not delivered"
		}
	}
	stored, inserted, err := s.store.InsertTxn(ctx, rec)
	if err != nil {
		countFact(ch.Name(), "store_error")
		return err
	}
	if !inserted && !sameFact(stored, rec) {
		countFact(ch.Name(), "conflict")
		logx.WithContext(ctx).Errorf("[payment] Channel %s reported transaction ...%s again with a different amount or order; the recorded fact is kept, check it manually",
			ch.Name(), tail(payment.TransactionID))
		return ErrFactConflict
	}
	if stored.OrderHi != 0 || stored.OrderLo != 0 {
		if refundedOutside {
			_, err = s.store.CloseIntent(ctx, stored.OrderHi, stored.OrderLo, nowMs)
		} else {
			err = s.store.MarkIntentPaid(ctx, stored.OrderHi, stored.OrderLo, nowMs)
		}
		if err != nil {
			return err
		}
	}
	switch {
	case !inserted:
		countFact(ch.Name(), "duplicate")
	case rec.MallState == data.MallOrphan:
		countFact(ch.Name(), "orphan")
		logx.WithContext(ctx).Errorf("[payment] Payment needs manual handling: channel=%s transaction=...%s out_trade_no=%s reason=%s",
			ch.Name(), tail(payment.TransactionID), payment.OutTradeNo, rec.MallLastError)
	default:
		countFact(ch.Name(), "recorded")
	}
	s.Kick()
	return nil
}

// sameFact 比对同一交易号的两次报告。玩家号不比:意图可能晚于第一次回调才被读到。
func sameFact(a, b *paymentpb.PaymentTxnRecord) bool {
	return a.TransactionId == b.TransactionId && a.OutTradeNo == b.OutTradeNo && a.PaidAmountMinor == b.PaidAmountMinor && a.PaidCurrency == b.PaidCurrency
}

func intentView(r *paymentpb.PaymentIntentRecord) *paymentpb.PayIntentView {
	return &paymentpb.PayIntentView{OrderId: r.OrderId, Channel: paymentpb.PayChannel(r.Channel), State: paymentpb.PayIntentState(r.State),
		AmountMinor: r.AmountMinor, Currency: r.Currency, ExpireMs: r.ExpireMs}
}

// tail 是交易号的后 6 位:日志只写这么多(设计 §11)。
func tail(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}
