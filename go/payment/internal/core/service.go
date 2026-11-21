// Package core 是支付服务的业务:建支付意图、记渠道付款事实与撤销、把付款与修订推进到商城、执行商城的退款待办。
//
// 不变量(docs/design/payment-recharge.md §22):
//   - 订单只在商城:买家、金额、币种都读商城冻结值,客户端不提交金额,也不能声称付过款。
//   - 一张订单至多一个支付意图、一个渠道;我们定价的渠道商户单号由订单号派生,商店渠道用订单号做交易绑定。
//   - 钱的事实先落本库(payment_txn,按渠道交易号幂等),再推进商城;商城确认之前一直重投。推进与回调应答解耦:
//     回调只等事实落库就应答,商城暂时不可达不会让渠道重发。
//   - 渠道撤销(用户在渠道侧退款、作废)按修订号翻转事实,经商城与 trade 扣回;本服务自己执行的退款不算撤销。
//   - 退款只执行商城列出的退款待办,退款单号由被退那笔付款确定性派生,重试不会多退;渠道退不了的转人工。
//   - 合规闸开着时,建意图前按买家的实名判年龄(小游戏渠道上没有实名的账号由平台管);未成年人的意图同时是他的额度预占(minor.go)。
package core

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"payment/internal/channel"
	"payment/internal/data"
	"payment/internal/mall"
	paymentpb "proto/payment"
	"shared/authoritysql"
)

// 业务拒绝。server 把它们映射成 tip 码;其余错误(存储失败)一律按服务故障回。
var (
	ErrInvalid             = errors.New("payment: invalid request")
	ErrChannelUnavailable  = errors.New("payment: channel is not available for this order or platform")
	ErrOrderNotFound       = errors.New("payment: order not found")
	ErrOrderNotPayable     = errors.New("payment: order is not awaiting payment")
	ErrOrderExpired        = errors.New("payment: order payment window has passed")
	ErrChannelMismatch     = errors.New("payment: order already uses another channel")
	ErrCurrencyUnsupported = errors.New("payment: channel does not take the order currency")
	ErrLoginRequired       = errors.New("payment: a fresh mini-game login is required")
	// ErrBusy:渠道或商城这一次没答上来。本服务没有结局未知的写入,玩家稍后重试即可。
	ErrBusy = errors.New("payment: channel or mall is temporarily unreachable")
	// ErrFactConflict:同一渠道交易号再次报来,金额或商户单号却和已记下的不同。不改已有事实,告警人工核对。
	ErrFactConflict = errors.New("payment: channel reported a transaction that differs from the recorded one")
)

// Store 是本服务对自己库的全部依赖。生产装配 *data.Repo,用例用内存替身走同一接口。
type Store interface {
	Intent(ctx context.Context, hi, lo uint64) (*paymentpb.PaymentIntentRecord, error)
	InsertIntent(ctx context.Context, rec *paymentpb.PaymentIntentRecord) (*paymentpb.PaymentIntentRecord, bool, error)
	ReserveIntent(ctx context.Context, rec *paymentpb.PaymentIntentRecord, limit data.SpendLimit) (*paymentpb.PaymentIntentRecord, bool, error)
	RecordMinorPaymentTxn(ctx context.Context, rec *paymentpb.PaymentTxnRecord, intent *paymentpb.PaymentIntentRecord, limit data.SpendLimit, paidMs, now uint64, overLimitReason string) (data.RecordMinorPaymentOutcome, error)
	ReviveMinorIntent(ctx context.Context, intent *paymentpb.PaymentIntentRecord, limit data.SpendLimit, paidMs, now uint64, force bool) (bool, error)
	SetChannelRef(ctx context.Context, hi, lo uint64, ref string, refMs uint64, payer string, now uint64) error
	ClaimActiveQuery(ctx context.Context, rec *paymentpb.PaymentIntentRecord, now uint64) (bool, error)
	DueIntents(ctx context.Context, now uint64, limit int) ([]*paymentpb.PaymentIntentRecord, error)
	ClaimCheck(ctx context.Context, rec *paymentpb.PaymentIntentRecord, lease, now uint64) (bool, error)
	ScheduleCheck(ctx context.Context, rec *paymentpb.PaymentIntentRecord, lease, next, now uint64) error
	CloseIntent(ctx context.Context, hi, lo, now uint64) (bool, error)
	MarkIntentPaid(ctx context.Context, hi, lo, now uint64) error
	SetIntentState(ctx context.Context, hi, lo uint64, state uint32, now uint64) error
	Txn(ctx context.Context, channel uint32, hi, lo uint64) (*paymentpb.PaymentTxnRecord, error)
	InsertTxn(ctx context.Context, rec *paymentpb.PaymentTxnRecord) (*paymentpb.PaymentTxnRecord, bool, error)
	FlipFact(ctx context.Context, rec *paymentpb.PaymentTxnRecord, observedMs, now uint64) (bool, error)
	TouchFactObserved(ctx context.Context, rec *paymentpb.PaymentTxnRecord, observedMs, now uint64) error
	OldestMallPendingMs(ctx context.Context) (uint64, error)
	OldestFinalizePendingMs(ctx context.Context) (uint64, error)
	DueMallTxns(ctx context.Context, now uint64, limit int) ([]*paymentpb.PaymentTxnRecord, error)
	ClaimMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, now uint64) (bool, error)
	SettleMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease uint64, s data.MallSync, now uint64) error
	DeferMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, next uint64, lastErr string, now uint64) error
	ClaimRefund(ctx context.Context, rec *paymentpb.PaymentTxnRecord, outRefundNo string, lease, now uint64) (bool, error)
	SaveRefund(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease uint64, p data.RefundProgress, now uint64) error
	DueFinalize(ctx context.Context, now uint64, limit int) ([]*paymentpb.PaymentTxnRecord, error)
	ClaimFinalize(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, now uint64) (bool, error)
	SaveFinalize(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease uint64, state uint32, next, now uint64) error
	ClaimCursor(ctx context.Context, channel uint32, owner, initialMs, lease, now uint64) (uint64, bool, error)
	AdvanceCursor(ctx context.Context, channel uint32, owner, cursorMs, now uint64) error
}

var _ Store = (*data.Repo)(nil)

// Mall 是本服务对商城的全部依赖。生产装配 *mall.Client。
type Mall interface {
	OrderView(ctx context.Context, orderID string) (mall.Order, error)
	RecordPayment(ctx context.Context, payment mall.Payment) (mall.Outcome, error)
	PushRevision(ctx context.Context, revision mall.Revision) (uint32, error)
	PendingRefunds(ctx context.Context, cursor string) (mall.PendingRefundPage, error)
	ReportRefund(ctx context.Context, refund mall.Refund) error
	CloseRefund(ctx context.Context, closure mall.RefundClosure) error
}

var _ Mall = (*mall.Client)(nil)

// Product 是一个商城 SKU 在各渠道的商品号(App Store / Google Play / 微信小游戏道具 ID)。
type Product struct {
	SkuID     uint64
	Title     string            // 收银台展示名(抖音)
	ByChannel map[string]string // 渠道名 → 商品号
}

// TestPolicy 是一个渠道的测试交易(App Store 沙箱、Google 许可测试、微信沙箱环境)发不发货。
// Players 为空表示接受全部玩家;生产环境只给审核账号开(docs/design/payment-recharge.md §22.8)。
type TestPolicy struct {
	Accept  bool
	Players map[uint64]bool
}

// Options 是时间、批量与商品参数。零值取缺省(withDefaults)。
type Options struct {
	MinPayWindow          time.Duration // 我们定价的渠道离截止不足这么久就不再调起:微信 time_expire 至少比下单晚 1 分钟
	ReceiptWindow         time.Duration // 商店渠道的意图在商城截止之后还开多久(家长审批、延迟付款)
	FirstCheckDelay       time.Duration // 建意图后第一次后台查单
	CheckInterval         time.Duration // 截止前的后台查单间隔
	CloseGrace            time.Duration // 截止后再等这么久,关渠道订单并做最后一次查单
	GiveUpAfter           time.Duration // 截止后这么久还确认不了就关掉意图(迟到的付款仍由回调记账)
	ActiveQueryInterval   time.Duration // 客户端轮询触发查单的最小间隔
	RequestCallTimeout    time.Duration // 请求路径上单次渠道或商城调用的上限
	BackgroundCallTimeout time.Duration // 后台单次渠道或商城调用的上限
	Lease                 time.Duration // 后台领取一行的租约
	Batch                 int           // 后台每轮最多处理的行数
	Interval              time.Duration // 查单、推进商城、收尾的轮询间隔
	RefundPollInterval    time.Duration // 拉商城退款待办的间隔
	HistoryInterval       time.Duration // 补拉渠道历史(作废列表、通知历史)的间隔
	HistoryLookback       time.Duration // 第一次补拉往回看多久
	MallBackoffMax        time.Duration
	RefundBackoffMax      time.Duration
	RefundReason          string // 渠道退款单上给用户看的原因
	Products              []Product
	TestPolicies          map[string]TestPolicy // 渠道名 → 规则;没写的渠道不接受测试交易
	// RealNames 非 nil 时执行未成年人充值限额(Compliance.MinorSpendingGate=realname,minor.go);nil = 不接实名。
	// 单次、每日、每月的数字都在 shared/antiaddiction.SpendLimitAt。
	RealNames RealNames
}

func (o Options) withDefaults() Options {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&o.MinPayWindow, time.Minute)
	set(&o.ReceiptWindow, 72*time.Hour)
	set(&o.FirstCheckDelay, time.Minute)
	set(&o.CheckInterval, 2*time.Minute)
	set(&o.CloseGrace, 2*time.Minute)
	set(&o.GiveUpAfter, 24*time.Hour)
	set(&o.ActiveQueryInterval, 3*time.Second)
	set(&o.RequestCallTimeout, 2*time.Second)
	set(&o.BackgroundCallTimeout, 8*time.Second)
	set(&o.Lease, 30*time.Second)
	set(&o.Interval, 2*time.Second)
	set(&o.RefundPollInterval, 10*time.Second)
	set(&o.HistoryInterval, 10*time.Minute)
	set(&o.HistoryLookback, 72*time.Hour)
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
	products  map[uint64]Product           // SKU → 商品
	bySku     map[string]map[string]uint64 // 渠道名 → 商品号 → SKU
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
		platforms: map[paymentpb.PayClientPlatform][]paymentpb.PayChannel{}, products: map[uint64]Product{}, bySku: map[string]map[string]uint64{},
		opts: opts.withDefaults(), now: time.Now, kick: make(chan struct{}, 1)}
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
	for _, product := range s.opts.Products {
		if product.SkuID == 0 {
			return nil, errors.New("payment: product mapping has sku 0")
		}
		if _, dup := s.products[product.SkuID]; dup {
			return nil, errors.New("payment: sku mapped twice: " + strconv.FormatUint(product.SkuID, 10))
		}
		s.products[product.SkuID] = product
		for name, id := range product.ByChannel {
			if s.bySku[name] == nil {
				s.bySku[name] = map[string]uint64{}
			}
			if _, dup := s.bySku[name][id]; dup {
				return nil, errors.New("payment: product id " + id + " is mapped to two skus on " + name)
			}
			s.bySku[name][id] = product.SkuID
		}
	}
	return s, nil
}

// Channel 按渠道名找渠道,回调入口按路径用它。
func (s *Service) Channel(name string) (channel.Channel, bool) {
	ch, ok := s.byName[name]
	return ch, ok
}

// Kick 叫醒推进任务:刚记下的付款或撤销马上推进商城,不等下一个轮询点。
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

func isReceiptChannel(ch channel.Channel) bool {
	_, ok := ch.(channel.ReceiptChannel)
	return ok
}

// productFor 是 SKU 在渠道上的商品号;没有映射回空串。
func (s *Service) productFor(sku uint64, name string) string {
	return s.products[sku].ByChannel[name]
}

// ListChannels 是这种发行形态当前开放的渠道。
func (s *Service) ListChannels(platform paymentpb.PayClientPlatform) ([]paymentpb.PayChannel, error) {
	if !validPlatform(platform) {
		return nil, ErrInvalid
	}
	return append([]paymentpb.PayChannel(nil), s.platforms[platform]...), nil
}

// CreateRequest 是建意图的入参(客户端字段原样)。
type CreateRequest struct {
	Player    uint64
	OrderID   string
	Channel   paymentpb.PayChannel
	Platform  paymentpb.PayClientPlatform
	LoginCode string
	DeviceOS  string
}

// Created 是建意图的结果。Intent 在业务拒绝时也可能有值(已有意图的当前状态)。
type Created struct {
	Intent *paymentpb.PayIntentView
	Params channel.ClientParams
}

// CreateIntent 为玩家的一张待付订单建支付意图并签出调起参数。重复调用幂等:同一订单、同一渠道再签一次参数。
func (s *Service) CreateIntent(ctx context.Context, in CreateRequest) (Created, error) {
	if in.Player == 0 || !validPlatform(in.Platform) || len(in.LoginCode) > 256 || len(in.DeviceOS) > 16 {
		return Created{}, ErrInvalid
	}
	hi, lo, err := data.OrderKey(in.OrderID)
	if err != nil {
		return Created{}, ErrInvalid
	}
	ch, ok := s.channels[in.Channel]
	if !ok || !s.allowed(in.Platform, in.Channel) {
		return Created{}, ErrChannelUnavailable
	}
	now := s.now()
	intent, err := s.store.Intent(ctx, hi, lo)
	switch {
	case err == nil:
	case errors.Is(err, data.ErrNotFound):
		if intent, err = s.openIntent(ctx, in.Player, in.OrderID, hi, lo, ch, now); err != nil {
			return Created{}, err
		}
	default:
		return Created{}, err
	}
	if intent.PlayerId != in.Player {
		return Created{}, ErrOrderNotFound // 别人的订单表现为不存在,不泄露它是否存在
	}
	view := intentView(intent)
	if intent.Channel != uint32(in.Channel) {
		return Created{Intent: view}, ErrChannelMismatch
	}
	switch intent.State {
	case data.IntentPaid:
		return Created{Intent: view}, nil
	case data.IntentClosed:
		return Created{Intent: view}, ErrOrderExpired
	}
	if !isReceiptChannel(ch) && uint64(now.Add(s.opts.MinPayWindow).UnixMilli()) >= intent.ExpireMs {
		return Created{Intent: view}, ErrOrderExpired
	}
	product := s.productFor(intent.SkuId, ch.Name())
	if ch.NeedsProduct() && product == "" {
		return Created{Intent: view}, ErrChannelUnavailable
	}
	prepared, err := s.prepare(ctx, ch, intent, product, in, now)
	switch {
	case err == nil:
		countIntent(ch.Name(), "prepared")
		return Created{Intent: view, Params: prepared.Params}, nil
	case errors.Is(err, channel.ErrOrderPaid):
		// 渠道说这单已经付过:当场查单记账,玩家看到的是已付,而不是一个失败。
		refreshed, queryErr := s.activeQuery(ctx, intent, true)
		if queryErr != nil {
			return Created{Intent: view}, queryErr
		}
		return Created{Intent: intentView(refreshed)}, nil
	case errors.Is(err, channel.ErrLoginRequired):
		return Created{Intent: view}, ErrLoginRequired
	case authoritysql.ClassifyOutcome(err) == authoritysql.OutcomeUnknown:
		return Created{Intent: view}, err
	default:
		logx.WithContext(ctx).Errorf("[payment] Preparing a %s payment failed: order=%s: %v", ch.Name(), in.OrderID, err)
		return Created{Intent: view}, ErrBusy
	}
}

// openIntent 读商城订单的冻结条款,核对后新建意图;合规闸开着时先判买家的实名与未成年人额度(minor.go)。
// 商城或账号目录只读失败时本服务什么都没写,回 ErrBusy。
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
	receipt := isReceiptChannel(ch)
	switch {
	case order.BuyerID != strconv.FormatUint(player, 10):
		return nil, ErrOrderNotFound
	case order.State != mall.OrderStateAwaitingPayment || order.AmountMinor == 0:
		return nil, ErrOrderNotPayable
	case !ch.Supports(order.Currency):
		return nil, ErrCurrencyUnsupported
	case ch.NeedsProduct() && s.productFor(order.SkuID, ch.Name()) == "":
		return nil, ErrChannelUnavailable // 寄售订单(SKU 0)与没登记商品号的 SKU 不能走按商品定价的渠道
	case !receipt && order.PaymentDeadlineMs <= uint64(now.Add(s.opts.MinPayWindow).UnixMilli()):
		return nil, ErrOrderExpired
	}
	expire := order.PaymentDeadlineMs
	if receipt {
		// 商店渠道不认我们的截止时刻(家长审批、延迟付款可能晚很久),意图多开一段;迟到的付款无论如何照常记账。
		expire = order.PaymentDeadlineMs + uint64(s.opts.ReceiptWindow.Milliseconds())
	}
	nowMs := uint64(now.UnixMilli())
	rec := &paymentpb.PaymentIntentRecord{OrderHi: hi, OrderLo: lo, OrderId: orderID, PlayerId: player, Channel: uint32(ch.ID()),
		AmountMinor: order.AmountMinor, Currency: order.Currency, State: data.IntentCreated, ExpireMs: expire,
		NextCheckMs: uint64(now.Add(s.opts.FirstCheckDelay).UnixMilli()), CreatedMs: nowMs, UpdatedMs: nowMs, SkuId: order.SkuID}
	if err := s.gateBuyer(ctx, rec, now); err != nil {
		return nil, err
	}
	stored, inserted, err := s.insertIntent(ctx, rec, now)
	if err != nil {
		return nil, err
	}
	if inserted {
		countIntent(ch.Name(), "created")
	}
	return stored, nil
}

// prepare 向渠道要调起参数,新拿到的渠道引用与付款人存回意图。
func (s *Service) prepare(ctx context.Context, ch channel.Channel, intent *paymentpb.PaymentIntentRecord, product string, in CreateRequest, now time.Time) (channel.Prepared, error) {
	outTradeNo, err := data.OutTradeNo(intent.OrderId)
	if err != nil {
		return channel.Prepared{}, err
	}
	cached := channel.Ref{Value: intent.ChannelRef}
	if intent.ChannelRefMs != 0 {
		cached.At = time.UnixMilli(int64(intent.ChannelRefMs))
	}
	callCtx, cancel := context.WithTimeout(ctx, s.opts.RequestCallTimeout)
	prepared, err := ch.Prepare(callCtx, channel.Order{OrderID: intent.OrderId, OutTradeNo: outTradeNo, PlayerID: intent.PlayerId,
		AmountMinor: intent.AmountMinor, Currency: intent.Currency, ExpireAt: time.UnixMilli(int64(intent.ExpireMs)), ProductID: product,
		Title: s.products[intent.SkuId].Title, LoginCode: in.LoginCode, DeviceOS: in.DeviceOS}, cached)
	cancel()
	observeCall(ch.Name(), "prepare", err)
	if err != nil {
		return channel.Prepared{}, err
	}
	if prepared.Ref.Value != "" || prepared.PayerID != "" {
		ref, payer := prepared.Ref.Value, prepared.PayerID
		if ref == "" {
			ref = intent.ChannelRef
		}
		if payer == "" {
			payer = intent.PayerId
		}
		// 微信 APP 的 prepay_id 只是缓存;微信小游戏的会话密钥是后台查单要用的。存储结局未知要报出去,让排空证明挂起。
		if err := s.store.SetChannelRef(ctx, intent.OrderHi, intent.OrderLo, ref, uint64(prepared.Ref.At.UnixMilli()), payer, uint64(now.UnixMilli())); err != nil {
			if authoritysql.ClassifyOutcome(err) == authoritysql.OutcomeUnknown {
				return channel.Prepared{}, err
			}
			logx.WithContext(ctx).Errorf("[payment] Saving the channel reference failed: order=%s: %v", intent.OrderId, err)
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
	if !ok || isReceiptChannel(ch) {
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
	callCtx, cancel := context.WithTimeout(ctx, s.opts.RequestCallTimeout)
	result, err := s.query(callCtx, ch, intent)
	cancel()
	if err != nil || result.Payment == nil || (result.State != channel.TradePaid && result.State != channel.TradeRefundedOutside) {
		return intent, nil
	}
	if _, err := s.record(ctx, ch, *result.Payment, result.State == channel.TradeRefundedOutside); err != nil {
		return intent, unknownOnly(err)
	}
	// 记账之后意图可能转已付,也可能关掉(渠道侧已退、这笔钱不算订单的付款):重读,不猜。
	if refreshed, err := s.store.Intent(ctx, intent.OrderHi, intent.OrderLo); err == nil {
		return refreshed, nil
	}
	return intent, nil
}

// query 按意图查一次渠道(带上查单要用的付款人与渠道引用)。
func (s *Service) query(ctx context.Context, ch channel.Channel, intent *paymentpb.PaymentIntentRecord) (channel.QueryResult, error) {
	outTradeNo, err := data.OutTradeNo(intent.OrderId)
	if err != nil {
		return channel.QueryResult{}, err
	}
	result, err := ch.Query(ctx, channel.QueryRequest{OutTradeNo: outTradeNo, PayerID: intent.PayerId,
		Ref: channel.Ref{Value: intent.ChannelRef, At: time.UnixMilli(int64(intent.ChannelRefMs))}, AmountMinor: intent.AmountMinor, Currency: intent.Currency})
	if !errors.Is(err, channel.ErrQueryUnsupported) {
		observeCall(ch.Name(), "query", err)
	}
	if err == nil && (result.State == channel.TradePaid || result.State == channel.TradeRefundedOutside) && result.Payment == nil {
		return channel.QueryResult{}, errors.New("payment: channel reported a payment without its details")
	}
	return result, err
}

// unknownOnly 只放行「写库结局未知」的错误,其余吞掉:请求路径上的顺手查单是尽力而为,后台还会再查。
func unknownOnly(err error) error {
	if authoritysql.ClassifyOutcome(err) == authoritysql.OutcomeUnknown {
		return err
	}
	return nil
}

// SubmitRequest 是商店渠道的交易提交。
type SubmitRequest struct {
	Player        uint64
	Channel       paymentpb.PayChannel
	OrderID       string
	TransactionID string
	PurchaseToken string
	ProductID     string
}

// Submitted 是交易提交的结论。Finish 为真:客户端可以结束这笔交易。
type Submitted struct {
	Intent *paymentpb.PayIntentView
	Finish bool
}

// SubmitPurchase 处理客户端提交的商店交易:向商店核实,记账,告诉客户端能不能结束交易。
// 交易属于哪张订单以商店里的绑定为准;提交者与订单买家不一致时照样记账(钱是真的,货发给订单买家),只记日志。
func (s *Service) SubmitPurchase(ctx context.Context, in SubmitRequest) (Submitted, error) {
	if in.Player == 0 || len(in.TransactionID) > 64 || len(in.PurchaseToken) > 512 || len(in.ProductID) > 128 {
		return Submitted{}, ErrInvalid
	}
	ch, ok := s.channels[in.Channel]
	if !ok {
		return Submitted{}, ErrChannelUnavailable
	}
	receiptChannel, ok := ch.(channel.ReceiptChannel)
	if !ok || (in.TransactionID == "" && in.PurchaseToken == "") {
		return Submitted{}, ErrInvalid
	}
	callCtx, cancel := context.WithTimeout(ctx, s.opts.RequestCallTimeout)
	result, err := receiptChannel.Verify(callCtx, channel.Receipt{TransactionID: in.TransactionID, PurchaseToken: in.PurchaseToken, ProductID: in.ProductID})
	cancel()
	observeCall(ch.Name(), "verify", err)
	if err != nil {
		logx.WithContext(ctx).Errorf("[payment] Verifying a %s purchase failed: %v", ch.Name(), err)
		return Submitted{Intent: s.ownedView(ctx, in.Player, in.OrderID)}, ErrBusy
	}
	finish := true
	switch result.State {
	case channel.TradePaid, channel.TradeRefundedOutside:
		if result.Payment.OrderID != "" && result.Payment.OrderID != in.OrderID {
			logx.WithContext(ctx).Infof("[payment] Submitted %s purchase is bound to another order than the client claimed: submitted_order=%s bound_order=%s",
				ch.Name(), in.OrderID, result.Payment.OrderID)
		}
		stored, err := s.record(ctx, ch, *result.Payment, result.State == channel.TradeRefundedOutside)
		if err != nil {
			if errors.Is(err, ErrFactConflict) {
				// Google Play 不能消耗/确认冲突交易(让 Google 3 天自动退款);App Store 必须结束交易以免锁死账号复购。
				finish = ch.ID() != paymentpb.PayChannel_PAY_CHANNEL_GOOGLE
				break
			}
			return Submitted{Intent: s.ownedView(ctx, in.Player, in.OrderID)}, err
		}
		if ch.ID() == paymentpb.PayChannel_PAY_CHANNEL_GOOGLE {
			// Google Play 只有进入商城记账链路的交易才允许客户端结束/确认;转人工(ORPHAN)或拒收的不确认,保留 Google 3 天自动退款。
			finish = stored != nil && (stored.MallState == data.MallPending || stored.MallState == data.MallBound || stored.MallState == data.MallRefundQueued)
		}
		if result.State == channel.TradePaid && result.Payment.OrderID != "" {
			in.OrderID = result.Payment.OrderID
		}
	case channel.TradeNotPaid:
		finish = false // 家长审批、现金付款等待中:交易保持未结束,付完再交
	case channel.TradeClosed:
		if result.Payment != nil {
			if err := s.revoke(ctx, ch, channel.Revocation{TransactionID: result.Payment.TransactionID, Revoked: true, ObservedAt: s.now(), Detail: "verify"}); err != nil {
				return Submitted{}, unknownOnly(err)
			}
		}
	}
	return Submitted{Intent: s.ownedView(ctx, in.Player, in.OrderID), Finish: finish}, nil
}

// ownedView 是玩家自己那张订单的意图视图;读不到或不是他的返回 nil。
func (s *Service) ownedView(ctx context.Context, player uint64, orderID string) *paymentpb.PayIntentView {
	hi, lo, err := data.OrderKey(orderID)
	if err != nil {
		return nil
	}
	intent, err := s.store.Intent(ctx, hi, lo)
	if err != nil || intent.PlayerId != player {
		return nil
	}
	return intentView(intent)
}

// HandleNotification 处理一条验过签的回调:付款记账、撤销翻转、要回查的交易逐个核实。
// 返回的错误里有写库结局未知时,调用方回失败让渠道重发,并挂起排空证明。
func (s *Service) HandleNotification(ctx context.Context, ch channel.Channel, note channel.Notification) error {
	var errs []error
	if note.Payment != nil {
		if _, err := s.record(ctx, ch, *note.Payment, false); err != nil && !errors.Is(err, ErrFactConflict) {
			errs = append(errs, err)
		}
	}
	for _, revocation := range note.Revocations {
		if err := s.revoke(ctx, ch, revocation); err != nil && !errors.Is(err, ErrFactConflict) {
			errs = append(errs, err)
		}
	}
	if receiptChannel, ok := ch.(channel.ReceiptChannel); ok {
		for _, receipt := range note.Receipts {
			if err := s.verifyReceipt(ctx, ch, receiptChannel, receipt); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// verifyReceipt 是没有会话的回查(Google 的购买通知):核实后记账或按撤销处理。
func (s *Service) verifyReceipt(ctx context.Context, ch channel.Channel, receiptChannel channel.ReceiptChannel, receipt channel.Receipt) error {
	callCtx, cancel := context.WithTimeout(ctx, s.opts.BackgroundCallTimeout)
	result, err := receiptChannel.Verify(callCtx, receipt)
	cancel()
	observeCall(ch.Name(), "verify", err)
	if err != nil {
		return err
	}
	switch result.State {
	case channel.TradePaid, channel.TradeRefundedOutside:
		_, err := s.record(ctx, ch, *result.Payment, result.State == channel.TradeRefundedOutside)
		return err
	case channel.TradeClosed:
		if result.Payment != nil {
			return s.revoke(ctx, ch, channel.Revocation{TransactionID: result.Payment.TransactionID, Revoked: true, ObservedAt: s.now(), Detail: "recheck"})
		}
	}
	return nil
}

// RecordPayment 记一笔渠道核实过的付款(回调入口、用例用)。重复的通知幂等。
func (s *Service) RecordPayment(ctx context.Context, ch channel.Channel, payment channel.Payment) error {
	_, err := s.record(ctx, ch, payment, false)
	return err
}

// revocationTombstoneReason 标记「退款通知先于付款通知到达」写下的墓碑行,挡住晚到的付款通知防止给已退款单发货。
const revocationTombstoneReason = "revoked at the channel before payment notification arrived; not delivered"

// minorLateOverLimitReason 标记未成年人迟到付款在额度行锁下复核超限。
const minorLateOverLimitReason = "minor spending limit reached by a payment that arrived after its intent closed"

// record 先写付款事实(幂等),再改意图状态,最后叫醒推进任务。
// 未成年人正常付款把「额度行锁 + 意图行锁 + 写付款事实 + 转 PAID」收口为单事务(RecordMinorPaymentTxn),消除 TOCTOU 与提前占额。
func (s *Service) record(ctx context.Context, ch channel.Channel, payment channel.Payment, refundedOutside bool) (*paymentpb.PaymentTxnRecord, error) {
	now := s.now()
	nowMs := uint64(now.UnixMilli())
	paidAt := payment.PaidAt
	if paidAt.IsZero() {
		paidAt = now
	}
	rec := &paymentpb.PaymentTxnRecord{Channel: uint32(ch.ID()), TransactionId: payment.TransactionID, OutTradeNo: payment.OutTradeNo,
		PaidAmountMinor: payment.AmountMinor, PaidCurrency: payment.Currency, PaidMs: uint64(paidAt.UnixMilli()),
		MallState: data.MallPending, MallDue: 1, NextAttemptMs: nowMs, CreatedMs: nowMs, UpdatedMs: nowMs,
		ChannelToken: payment.ChannelToken, ChannelOrderId: payment.ChannelOrderID, StoreAmountMinor: payment.StoreAmountMinor,
		StoreCurrency: payment.StoreCurrency, ProductId: payment.ProductID, FactObservedMs: uint64(paidAt.UnixMilli())}
	rec.TxnHi, rec.TxnLo = data.TxnKey(payment.TransactionID)
	orderID := payment.OrderID
	if orderID == "" {
		orderID, _ = data.OrderIDFromOutTradeNo(payment.OutTradeNo)
	}
	if rec.OutTradeNo == "" && orderID != "" {
		rec.OutTradeNo, _ = data.OutTradeNo(orderID)
	}
	orphan := func(reason string) {
		if rec.MallState != data.MallOrphan {
			rec.MallState, rec.MallDue, rec.MallLastError = data.MallOrphan, 0, reason
		}
	}
	var intent *paymentpb.PaymentIntentRecord
	hi, lo, keyErr := data.OrderKey(orderID)
	switch {
	case !data.ValidTransactionID(payment.TransactionID):
		orphan("transaction id cannot be reported to the mall")
	case keyErr != nil:
		orphan("the payment is not bound to an order issued by this service")
	default:
		rec.OrderHi, rec.OrderLo = hi, lo
		found, err := s.store.Intent(ctx, hi, lo)
		switch {
		case err == nil:
			intent = found
			rec.PlayerId = found.PlayerId
		case !errors.Is(err, data.ErrNotFound):
			return nil, err
		}
	}
	if rec.MallState == data.MallPending {
		s.screen(ch, payment, intent, rec, orphan)
	}
	if refundedOutside {
		// 记成奇数修订号 1(REVOKED),后续若 App Store 撤回退款(REFUND_REVERSED)能识别方向翻转而非静默丢弃。
		rec.FactRevision = 1
		orphan("refunded at the channel before this service recorded it; not delivered")
	}
	if payment.NeedsFinalize && rec.MallState == data.MallPending {
		rec.FinalizeState, rec.FinalizeNextMs = data.FinalizePending, nowMs
	}
	var (
		stored        *paymentpb.PaymentTxnRecord
		inserted      bool
		intentSettled bool
		err           error
	)
	if intent != nil && minorIntent(intent) && countsForIntent(rec, intent) && !refundedOutside {
		paidMs := max(nowMs, intent.CreatedMs)
		limit := s.spendLimit(intent.MinorAge, time.UnixMilli(int64(paidMs)))
		outcome, recErr := s.store.RecordMinorPaymentTxn(ctx, rec, intent, limit, paidMs, nowMs, minorLateOverLimitReason)
		if recErr != nil {
			countFact(ch.Name(), "store_error")
			return nil, recErr
		}
		stored, inserted = outcome.Stored, outcome.Inserted
		if outcome.Inserted {
			intentSettled = true
			if outcome.WasClosed {
				if outcome.OverLimit {
					countMinorGate("late_payment_over_limit")
				} else {
					countMinorGate("late_payment_admitted")
				}
			}
		}
	} else {
		stored, inserted, err = s.store.InsertTxn(ctx, rec)
		if err != nil {
			countFact(ch.Name(), "store_error")
			return nil, err
		}
	}
	// 退款通知先于付款通知到达时留下的墓碑行:直接按已撤销重复吞掉,不报冲突、不发货。
	if !inserted && stored.FactRevision > 0 && data.Revoked(stored.FactRevision) &&
		stored.MallLastError == revocationTombstoneReason && stored.TransactionId == rec.TransactionId {
		countFact(ch.Name(), "duplicate")
		return stored, nil
	}
	if !inserted && !sameFact(stored, rec) {
		countFact(ch.Name(), "conflict")
		logx.WithContext(ctx).Errorf("[payment] Channel %s reported transaction ...%s again with a different amount or binding; the recorded fact is kept, check it manually",
			ch.Name(), tail(payment.TransactionID))
		return nil, ErrFactConflict
	}
	// 非未成年人事务路径或重复报告补做第二步;已被渠道撤销的事实不再改意图(revoke 管它)。
	if !intentSettled && (stored.OrderHi != 0 || stored.OrderLo != 0) && (inserted || !data.Revoked(stored.FactRevision)) {
		if err := s.settleIntent(ctx, ch, stored, intent, inserted, refundedOutside, nowMs); err != nil {
			return nil, err
		}
	}
	switch {
	case !inserted:
		countFact(ch.Name(), "duplicate")
	case stored.MallState == data.MallOrphan:
		countFact(ch.Name(), "orphan")
		logx.WithContext(ctx).Errorf("[payment] Payment needs manual handling: channel=%s transaction=...%s order=%s reason=%s",
			ch.Name(), tail(payment.TransactionID), orderID, stored.MallLastError)
	default:
		countFact(ch.Name(), "recorded")
	}
	s.Kick()
	return stored, nil
}

// settleIntent 是记账的第二步:按记下的这笔钱改订单的意图。首次记账与重复报告都走这里,幂等。
//   - 渠道侧已退,或这笔钱不算订单的付款(见 countsForIntent)而渠道一个商户单号只能付成一次(我们定价的渠道):意图再也付不成,
//     关掉。不关的话后台每次查单都得到「已付」、记账判重复后就返回,意图一直停在等付款,每个租约周期查一次渠道、永不停,
//     未成年人的在途额度也一直占着。CloseIntent 只改还在等付款的意图,已由别的付款付清的不受影响。
//   - 商店渠道上不算数的那笔:不动意图,同一张订单之后还可能有一笔对得上的购买,意图照常到期。
//   - 算数的:转已付。重复报告时未成年人的意图已到期关闭(额度已释放)的,在额度行锁下重新计入。
func (s *Service) settleIntent(ctx context.Context, ch channel.Channel, fact *paymentpb.PaymentTxnRecord, intent *paymentpb.PaymentIntentRecord, inserted, refundedOutside bool, nowMs uint64) error {
	counts := countsForIntent(fact, intent)
	switch {
	case refundedOutside || (!counts && !isReceiptChannel(ch)):
		_, err := s.store.CloseIntent(ctx, fact.OrderHi, fact.OrderLo, nowMs)
		return err
	case !counts:
		return nil
	case !inserted && intent != nil && intent.State == data.IntentClosed && minorIntent(intent):
		return s.recountMinor(ctx, intent, nowMs)
	default:
		return s.store.MarkIntentPaid(ctx, fact.OrderHi, fact.OrderLo, nowMs)
	}
}

// countsForIntent 报告这笔钱算不算订单意图的付款:没转人工、没被本服务退掉,金额与币种和意图冻结的一致。
// 对不上的商城按异常付款退掉、不绑定订单(AMOUNT_MISMATCH / CURRENCY_MISMATCH)。没有意图时无从比较,照常算。
func countsForIntent(fact *paymentpb.PaymentTxnRecord, intent *paymentpb.PaymentIntentRecord) bool {
	if fact.MallState == data.MallOrphan || fact.RefundState == data.RefundSucceeded || fact.RefundState == data.RefundReported {
		return false
	}
	return intent == nil || (fact.PaidAmountMinor == intent.AmountMinor && fact.PaidCurrency == intent.Currency)
}

// screen 按订单意图核对一笔付款,不合格的记成人工处理。只在付款认得出订单时调用。
func (s *Service) screen(ch channel.Channel, payment channel.Payment, intent *paymentpb.PaymentIntentRecord, rec *paymentpb.PaymentTxnRecord, orphan func(string)) {
	if payment.Anomaly != "" {
		orphan(payment.Anomaly)
		return
	}
	if payment.StorePriced {
		if intent == nil {
			orphan("store-priced payment has no payment intent to take the order amount from")
			return
		}
		// 商店按店面定价,商城的金额校验对它是恒等式:报订单冻结值,商店实收另记。真正的保护是下面的商品与价格核对。
		rec.PaidAmountMinor, rec.PaidCurrency = intent.AmountMinor, intent.Currency
		if payment.StoreAmountMinor > 0 && payment.StoreCurrency == intent.Currency && payment.StoreAmountMinor != intent.AmountMinor {
			orphan("store price differs from the order price")
			return
		}
	}
	if intent != nil && payment.ProductID != "" && ch.NeedsProduct() && s.productFor(intent.SkuId, ch.Name()) != payment.ProductID {
		orphan("product does not match the order sku")
		return
	}
	if receiptChannel, ok := ch.(channel.ReceiptChannel); ok && intent != nil && payment.AccountRef != "" && receiptChannel.AccountRef(intent.PlayerId) != payment.AccountRef {
		orphan("account binding does not match the order buyer")
		return
	}
	if payment.Test && !s.acceptsTest(ch.Name(), intent) {
		orphan("test transaction is not accepted in this environment")
	}
}

func (s *Service) acceptsTest(name string, intent *paymentpb.PaymentIntentRecord) bool {
	policy, ok := s.opts.TestPolicies[name]
	if !ok || !policy.Accept {
		return false
	}
	if len(policy.Players) == 0 {
		return true
	}
	return intent != nil && policy.Players[intent.PlayerId]
}

// revoke 按渠道的撤销翻转一笔已记下的付款。
//   - 没记过的交易:渠道给了完整交易就按「撤销后才记账」记成人工(不发货);否则没有东西可扣,忽略。
//   - 本服务自己发起过退款的交易:这次撤销就是那笔退款,不翻转(退款不需要扣回:要退的都是没发货的钱)。
//   - 观测时刻不晚于已记的观测:过时,忽略。只有 App Store 允许撤销被撤回。
//   - 修订号乐观并发:翻转失败说明别的观测先到,重读再判,最多 3 次。
func (s *Service) revoke(ctx context.Context, ch channel.Channel, revocation channel.Revocation) error {
	hi, lo := data.TxnKey(revocation.TransactionID)
	observedMs := uint64(revocation.ObservedAt.UnixMilli())
	if revocation.ObservedAt.IsZero() {
		observedMs = uint64(s.now().UnixMilli())
	}
	for attempt := 0; attempt < 3; attempt++ {
		txn, err := s.store.Txn(ctx, uint32(ch.ID()), hi, lo)
		if errors.Is(err, data.ErrNotFound) {
			if revocation.Revoked && revocation.Payment != nil {
				_, err := s.record(ctx, ch, *revocation.Payment, true)
				return err
			}
			if revocation.Revoked {
				if revocation.OutRefundNo != "" && revocation.OutRefundNo == data.OutRefundNo(ch.Name(), revocation.TransactionID) {
					return nil // 己方退款回调先到或本机未记过:忽略
				}
				// 退款回调先于迟到的付款回调到达:写一条 FactRevision=1 墓碑行,挡住晚到的付款通知防止发货。
				var orderHi, orderLo uint64
				if oid, parseErr := data.OrderIDFromOutTradeNo(revocation.OutTradeNo); parseErr == nil {
					orderHi, orderLo, _ = data.OrderKey(oid)
				}
				nowMs := uint64(s.now().UnixMilli())
				tomb := &paymentpb.PaymentTxnRecord{Channel: uint32(ch.ID()), TxnHi: hi, TxnLo: lo, TransactionId: revocation.TransactionID,
					OrderHi: orderHi, OrderLo: orderLo, OutTradeNo: revocation.OutTradeNo,
					MallState: data.MallOrphan, MallDue: 0, MallLastError: revocationTombstoneReason,
					FactRevision: 1, FactObservedMs: observedMs, CreatedMs: nowMs, UpdatedMs: nowMs}
				_, inserted, insErr := s.store.InsertTxn(ctx, tomb)
				if insErr != nil {
					return insErr
				}
				if !inserted {
					continue // 并发付款刚落库:重读并按正常撤销翻转
				}
				countFact(ch.Name(), "revoked")
				logx.WithContext(ctx).Infof("[payment] Recorded revocation tombstone for %s transaction ...%s before payment notification arrived", ch.Name(), tail(revocation.TransactionID))
				return nil
			}
			logx.WithContext(ctx).Infof("[payment] Revocation for a transaction this service never recorded: channel=%s transaction=...%s", ch.Name(), tail(revocation.TransactionID))
			return nil
		}
		if err != nil {
			return err
		}
		if txn.TransactionId != revocation.TransactionID {
			return ErrFactConflict
		}
		switch {
		case txn.RefundState != data.RefundNone || (revocation.OutRefundNo != "" && revocation.OutRefundNo == data.OutRefundNo(ch.Name(), txn.TransactionId)):
			return nil
		case (txn.FactRevision > 0 && observedMs <= txn.FactObservedMs) || (txn.FactRevision == 0 && observedMs < txn.FactObservedMs):
			return nil // 比已记观测更早的通知:过时(App Store 等通知可能乱序到达)
		case data.Revoked(txn.FactRevision) == revocation.Revoked:
			// 方向未变但观测时刻更新(例如 App Store 乱序先收到 REFUND_REVERSED):推高 fact_observed_ms 以拒绝迟到的旧 REFUND。
			if observedMs > txn.FactObservedMs {
				return s.store.TouchFactObserved(ctx, txn, observedMs, uint64(s.now().UnixMilli()))
			}
			return nil
		case !revocation.Revoked && ch.ID() != paymentpb.PayChannel_PAY_CHANNEL_APPLE:
			return nil
		}
		nowMs := uint64(s.now().UnixMilli())
		flipped, err := s.store.FlipFact(ctx, txn, observedMs, nowMs)
		if err != nil {
			return err
		}
		if !flipped {
			continue
		}
		state := data.IntentClosed
		if !revocation.Revoked {
			state = data.IntentPaid
		}
		if (txn.OrderHi != 0 || txn.OrderLo != 0) && drivesIntent(txn) {
			if err := s.applyFlip(ctx, txn.OrderHi, txn.OrderLo, state, nowMs); err != nil {
				return err
			}
		}
		outcome := "revoked"
		if !revocation.Revoked {
			outcome = "reinstated"
		}
		countFact(ch.Name(), outcome)
		logx.WithContext(ctx).Infof("[payment] Channel %s %s transaction ...%s (revision %d, %s); the mall will reverse the delivery",
			ch.Name(), outcome, tail(revocation.TransactionID), txn.FactRevision+1, revocation.Detail)
		s.Kick()
		return nil
	}
	return errors.New("payment: the transaction fact kept changing while applying a revocation")
}

// sameFact 比对同一交易号的两次报告。玩家号与商店订单金额不比:意图可能晚于第一次报告才读到(商店渠道的金额取自意图);
// 非商店渠道(支付宝/微信/抖音)若两次报告均带实付金额/币种,必须一致,防止篡改或冲突金额被静默放过。
func sameFact(a, b *paymentpb.PaymentTxnRecord) bool {
	if a.TransactionId != b.TransactionId || a.OrderHi != b.OrderHi || a.OrderLo != b.OrderLo {
		return false
	}
	if a.StoreAmountMinor != 0 && b.StoreAmountMinor != 0 && a.StoreAmountMinor != b.StoreAmountMinor {
		return false
	}
	if a.PaidAmountMinor != 0 && b.PaidAmountMinor != 0 && a.PaidAmountMinor != b.PaidAmountMinor {
		return false
	}
	if a.PaidCurrency != "" && b.PaidCurrency != "" && !strings.EqualFold(a.PaidCurrency, b.PaidCurrency) {
		return false
	}
	return true
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
