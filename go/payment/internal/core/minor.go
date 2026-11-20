package core

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"

	"payment/internal/data"
	"payment/internal/mall"
	paymentpb "proto/payment"
	"shared/antiaddiction"
)

// 未成年人充值限额闸(docs/design/real-name-anti-addiction.md §5.6;国新出发〔2019〕34 号;充值设计 §12):
//   - 建意图前按买家账号上的实名 pi 判年龄。App 内的渠道(微信支付、支付宝、App Store、Google Play,以及联调用的 devpay,
//     以后新加的默认也算)账号没有实名就拒绝。小游戏渠道由平台实名并执行限额:账号没有 pi 照常放行;有 pi(同一个人也在 App 实名过)
//     时一样按下面的规则判、一起计额——法规按企业合并计算,不合并的话 App 与小游戏各自不超限、合起来却可能超。
//   - 未满 8 周岁的拒绝;单笔超过年龄段单次上限的拒绝;未成年人按 pi(同一个人的多个账号合并)在北京时间的自然月、
//     自然日累计,预占与释放的口径见 data/spend.go。成年人不计额。

// RealNames 是本服务对账号目录实名的全部依赖。生产装配 *accountdirectory.RealNameClient。
type RealNames interface {
	// RealNameOf 回玩家所属账号的 pi。found=false:目录里没有这名角色;pi 为空:账号没有在游戏方实名。
	RealNameOf(ctx context.Context, player uint64) (pi string, found bool, err error)
}

// 限额闸的业务拒绝,server 映射成 Tip.xlsx payment 段的码。
var (
	ErrRealNameRequired  = errors.New("payment: the buyer has no real-name verification on record")
	ErrMinorNotAllowed   = errors.New("payment: buyers under 8 may not pay")
	ErrMinorSingleLimit  = errors.New("payment: the order exceeds the minor single-payment limit")
	ErrMinorDailyLimit   = errors.New("payment: the minor daily spending limit is reached")
	ErrMinorMonthlyLimit = errors.New("payment: the minor monthly spending limit is reached")
)

// limitCurrency 是限额的币种:法规按人民币定额,上限以分计。
const limitCurrency = "CNY"

// platformManaged 报告这个渠道是否由平台执行实名与未成年人限额(微信 / 抖音小游戏,设计 real-name-anti-addiction.md §3)。
func platformManaged(id paymentpb.PayChannel) bool {
	return id == paymentpb.PayChannel_PAY_CHANNEL_WECHAT_MINIGAME || id == paymentpb.PayChannel_PAY_CHANNEL_DOUYIN_MINIGAME
}

// minorIntent 报告这个意图是否按未成年人预占过额度。
func minorIntent(intent *paymentpb.PaymentIntentRecord) bool {
	return intent.SpenderHi != 0 || intent.SpenderLo != 0
}

// gateBuyer 在建意图之前判买家能不能付这一单。闸没开、小游戏渠道上没有实名的账号、成年人放行且不改 rec;
// 未成年人通过单笔核对时在 rec 上记下额度主体与周岁,由调用方在额度行锁下核对月、日累计并预占(data.Repo.ReserveIntent)。
// 读不到实名时一律拒绝(含小游戏渠道):放行就可能漏掉这个人在别的渠道已经花的钱。
func (s *Service) gateBuyer(ctx context.Context, rec *paymentpb.PaymentIntentRecord, now time.Time) error {
	if s.opts.RealNames == nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, s.opts.RequestCallTimeout)
	pi, found, err := s.opts.RealNames.RealNameOf(callCtx, rec.PlayerId)
	cancel()
	if err != nil {
		countMinorGate("lookup_failed")
		logx.WithContext(ctx).Errorf("[payment] Reading the buyer's real-name record failed: order=%s: %v", rec.OrderId, err)
		return ErrBusy
	}
	switch {
	case !found:
		logx.WithContext(ctx).Errorf("[payment] The account directory has no record of the buyer of order %s; refusing the payment", rec.OrderId)
		countMinorGate("real_name_required")
		return ErrRealNameRequired
	case pi == "" && platformManaged(paymentpb.PayChannel(rec.Channel)):
		countMinorGate("platform_managed")
		return nil
	case pi == "":
		countMinorGate("real_name_required")
		return ErrRealNameRequired
	}
	birth, ok := antiaddiction.BirthDate(pi)
	if !ok {
		// RealNameOf 已经核过格式;走到这里说明两边对 pi 的认识不一致,拒绝并报错。
		countMinorGate("lookup_failed")
		logx.WithContext(ctx).Errorf("[payment] The buyer's real-name record for order %s is malformed", rec.OrderId)
		return ErrBusy
	}
	age := antiaddiction.AgeAt(birth, now)
	limit := antiaddiction.SpendLimitAt(age)
	switch {
	case limit.Unlimited:
		countMinorGate("adult")
		return nil
	case limit.SingleFen == 0:
		countMinorGate("under_8")
		return ErrMinorNotAllowed
	case rec.Currency != limitCurrency:
		// 国内发行的订单都是人民币;别的币种无从按法规金额核对,拒绝。
		countMinorGate("currency")
		return ErrCurrencyUnsupported
	case rec.AmountMinor > limit.SingleFen:
		countMinorGate("single_limit")
		return ErrMinorSingleLimit
	}
	rec.SpenderHi, rec.SpenderLo = data.SpenderKey(pi)
	rec.MinorAge = uint32(age)
	return nil
}

// insertIntent 写入新意图:未成年人的在额度行锁下核对月、日累计并预占,其余照常插入。
func (s *Service) insertIntent(ctx context.Context, rec *paymentpb.PaymentIntentRecord, now time.Time) (*paymentpb.PaymentIntentRecord, bool, error) {
	if !minorIntent(rec) {
		return s.store.InsertIntent(ctx, rec)
	}
	stored, inserted, err := s.store.ReserveIntent(ctx, rec, s.spendLimit(rec.MinorAge, now))
	switch {
	case errors.Is(err, data.ErrMonthlyLimit):
		countMinorGate("monthly_limit")
		return nil, false, ErrMinorMonthlyLimit
	case errors.Is(err, data.ErrDailyLimit):
		countMinorGate("daily_limit")
		return nil, false, ErrMinorDailyLimit
	case err == nil && inserted:
		countMinorGate("reserved")
	}
	return stored, inserted, err
}

// spendLimit 是 age 周岁的未成年人在 at 所在的北京自然月、自然日的额度。age 只会是建意图时记下的 8–17;
// 月、日的数字都取 shared/antiaddiction,不在这里另配。
func (s *Service) spendLimit(age uint32, at time.Time) data.SpendLimit {
	limit := antiaddiction.SpendLimitAt(int(age))
	b := at.In(antiaddiction.Beijing)
	month := time.Date(b.Year(), b.Month(), 1, 0, 0, 0, 0, antiaddiction.Beijing)
	day := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, antiaddiction.Beijing)
	return data.SpendLimit{MonthlyFen: limit.MonthlyFen, DailyFen: limit.DailyFen,
		MonthStartMs: uint64(month.UnixMilli()), DayStartMs: uint64(day.UnixMilli())}
}

// recheckLatePayment 复核未成年人意图关闭(额度已释放)之后才到的钱:在额度行锁下重新核对,超限的不发货、转人工(充值设计 §12「其它入账」)。
// 同一笔已经记过的不再复核,以记下的结论为准。同一笔的两次报告同时到、且这期间额度恰好变了,可能一次判超限一次放行:
// 以先写进库的那次为准,最坏是多占了额度,不会多发货。
func (s *Service) recheckLatePayment(ctx context.Context, rec *paymentpb.PaymentTxnRecord, intent *paymentpb.PaymentIntentRecord, now time.Time, orphan func(string)) error {
	if _, err := s.store.Txn(ctx, rec.Channel, rec.TxnHi, rec.TxnLo); err == nil {
		return nil
	} else if !errors.Is(err, data.ErrNotFound) {
		return err
	}
	nowMs := uint64(now.UnixMilli())
	paidMs := max(nowMs, intent.CreatedMs)
	admitted, err := s.store.ReviveMinorIntent(ctx, intent, s.spendLimit(intent.MinorAge, time.UnixMilli(int64(paidMs))), paidMs, nowMs, false)
	if err != nil {
		return err
	}
	if admitted {
		countMinorGate("late_payment_admitted")
		return nil
	}
	countMinorGate("late_payment_over_limit")
	orphan("minor spending limit reached by a payment that arrived after its intent closed")
	return nil
}

// recountMinor 把一笔照样要发货的钱重新计入未成年人已关闭的意图(App Store 撤回退款;记账的第二步没做完、意图已到期关闭):
// 在额度行锁下转 PAID。超限也照样计入(钱在商户手里,商城照常收敛),记合规日志与指标等人工(充值设计 §12「退款与撤回」)。
func (s *Service) recountMinor(ctx context.Context, intent *paymentpb.PaymentIntentRecord, nowMs uint64) error {
	paidMs := max(nowMs, intent.CreatedMs)
	within, err := s.store.ReviveMinorIntent(ctx, intent, s.spendLimit(intent.MinorAge, time.UnixMilli(int64(paidMs))), paidMs, nowMs, true)
	if err != nil {
		return err
	}
	if !within {
		countMinorGate("recounted_over_limit")
		logx.WithContext(ctx).Errorf("[payment] Compliance: a payment counted again puts a minor over the spending limit; order=%s needs manual handling", intent.OrderId)
	}
	return nil
}

// applyFlip 按付款事实的翻转改意图状态:撤销 → CLOSED(释放未成年人额度),撤销被撤回 → PAID(重新计入,未成年人的经 recountMinor)。
func (s *Service) applyFlip(ctx context.Context, hi, lo uint64, state uint32, nowMs uint64) error {
	if state == data.IntentPaid {
		intent, err := s.store.Intent(ctx, hi, lo)
		if err != nil && !errors.Is(err, data.ErrNotFound) {
			return err
		}
		if intent != nil && minorIntent(intent) {
			return s.recountMinor(ctx, intent, nowMs)
		}
	}
	return s.store.SetIntentState(ctx, hi, lo, state, nowMs)
}

// drivesIntent 报告这笔付款的翻转该不该改意图状态:只有订单绑定的那笔才算——已绑定、还没报给商城,或商城为它本身建了
// 退款(取消后才到账)。重复付款、金额币种不符这些异常付款,以及拒收、转人工的,翻转不影响订单那一笔,
// 不能拿它关掉意图、释放未成年人的额度。
func drivesIntent(txn *paymentpb.PaymentTxnRecord) bool {
	switch txn.MallState {
	case data.MallPending, data.MallBound:
		return true
	case data.MallRefundQueued:
		return mall.RefundsOrdersOwnPayment(txn.MallRefundReason)
	}
	return false
}

// releaseRefunded:商城退的是订单绑定的那笔(取消后才到账、交付被拒),渠道退成之后订单就是已退款,关掉意图,
// 未成年人的额度随之释放。异常付款的退款不动意图(订单那一笔还在)。重做幂等。
func (s *Service) releaseRefunded(ctx context.Context, txn *paymentpb.PaymentTxnRecord, reason string, nowMs uint64) error {
	if !mall.RefundsOrdersOwnPayment(reason) || (txn.OrderHi == 0 && txn.OrderLo == 0) {
		return nil
	}
	return s.store.SetIntentState(ctx, txn.OrderHi, txn.OrderLo, data.IntentClosed, nowMs)
}

var minorGate = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "payment", Name: "minor_gate_total",
	Help: "未成年人充值限额闸的结论:adult 成年放行;platform_managed 小游戏渠道上没有实名的账号放行(平台限额);reserved 未成年人预占成功;" +
		"real_name_required / under_8 / currency / single_limit / " +
		"daily_limit / monthly_limit 拒绝;lookup_failed 读不到实名;late_payment_admitted / late_payment_over_limit 迟到付款的复核;" +
		"recounted_over_limit 照样要发货的钱重新计入后超限(要人工)。"}, []string{"outcome"})

func countMinorGate(outcome string) { minorGate.WithLabelValues(outcome).Inc() }
