package core

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"

	"payment/internal/channel"
	"payment/internal/data"
	"payment/internal/mall"
	paymentpb "proto/payment"
	"shared/authoritysql"
	"shared/drainproof"
)

// Run 跑后台任务直到 ctx 取消:到点查单与关单、推进商城(付款与修订)、执行商城的退款待办、渠道收尾、补拉渠道历史。
// 只在 stable 副本上跑(发布轨道 §2.5)。多副本同时跑是安全的:每一行都按租约领取,渠道与商城的调用都幂等。
// relay 记账:每一单位工作进出 relay;写库结局未知时挂起排空证明。
func (s *Service) Run(ctx context.Context, relay *drainproof.Session) {
	var wg sync.WaitGroup
	loop := func(interval time.Duration, kick <-chan struct{}, tick func(context.Context)) {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-kick:
			}
			tick(ctx)
		}
	}
	wg.Add(5)
	go loop(s.opts.Interval, nil, func(c context.Context) { s.sweepIntents(c, relay) })
	go loop(s.opts.Interval, s.kick, func(c context.Context) { s.pushMall(c, relay) })
	go loop(s.opts.RefundPollInterval, nil, func(c context.Context) { s.processRefunds(c, relay) })
	go loop(s.opts.Interval, nil, func(c context.Context) { s.finalize(c, relay) })
	go loop(s.opts.HistoryInterval, nil, func(c context.Context) { s.pollHistory(c, relay) })
	wg.Wait()
}

// unit 把一单位后台工作记进 relay。fn 返回的错误里有「写库结局未知」就挂起:排空证明从此要人工核对。
func unit(relay *drainproof.Session, fn func() error) (err error) {
	if relay != nil {
		finish, enterErr := relay.Enter()
		if enterErr != nil {
			return enterErr
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				relay.Hold()
				panic(recovered)
			}
			if err != nil && authoritysql.ClassifyOutcome(err) == authoritysql.OutcomeUnknown {
				relay.Hold()
			}
		}()
	}
	return fn()
}

func (s *Service) logWorkerError(ctx context.Context, what string, err error) {
	if err != nil && ctx.Err() == nil && !errors.Is(err, drainproof.ErrClosed) {
		logx.WithContext(ctx).Errorf("[payment] %s failed: %v", what, err)
	}
}

// backoff 是第 attempt 次失败之后的等待:1 秒起翻倍,封顶 max,加 ±20% 抖动,免得多笔同时失败的单子同时重试。
func backoff(attempt uint32, max time.Duration) time.Duration {
	if attempt > 20 {
		attempt = 20
	}
	wait := time.Second << attempt
	if wait <= 0 || wait > max {
		wait = max
	}
	jitter := time.Duration(rand.Int64N(int64(wait)/5 + 1))
	if rand.IntN(2) == 0 {
		return wait - jitter
	}
	return wait + jitter
}

func (s *Service) background(parent context.Context) (context.Context, context.CancelFunc) {
	// 已经开始的一单位工作不随停机信号中断:关停时 closeRelay 会先取消循环,再等在途的工作收尾。
	return context.WithTimeout(context.WithoutCancel(parent), 3*s.opts.BackgroundCallTimeout)
}

// ---------------------------------------------------------------------------
// 查单与关单
// ---------------------------------------------------------------------------

func (s *Service) sweepIntents(ctx context.Context, relay *drainproof.Session) {
	scan, cancel := context.WithTimeout(ctx, 2*time.Second)
	due, err := s.store.DueIntents(scan, uint64(s.now().UnixMilli()), s.opts.Batch)
	cancel()
	if err != nil {
		s.logWorkerError(ctx, "scanning due payment intents", err)
		return
	}
	for _, intent := range due {
		if ctx.Err() != nil {
			return
		}
		intent := intent
		s.logWorkerError(ctx, "checking payment intent "+intent.OrderId, unit(relay, func() error { return s.checkIntent(ctx, intent) }))
	}
}

// checkIntent 后台查一次单。截止前按间隔查;截止加宽限之后关渠道订单、最后查一次,确认没付才关意图;
// 渠道不能查单的(商店、抖音、会话已失效的微信小游戏)到期直接关意图——迟到的付款仍由回调或客户端提交记账。
func (s *Service) checkIntent(parent context.Context, intent *paymentpb.PaymentIntentRecord) error {
	ctx, cancel := s.background(parent)
	defer cancel()
	nowMs := uint64(s.now().UnixMilli())
	lease := nowMs + uint64(s.opts.Lease.Milliseconds())
	claimed, err := s.store.ClaimCheck(ctx, intent, lease, nowMs)
	if err != nil || !claimed {
		return err
	}
	deadline := intent.ExpireMs + uint64(s.opts.CloseGrace.Milliseconds())
	giveUp := intent.ExpireMs + uint64(s.opts.GiveUpAfter.Milliseconds())
	ch, ok := s.channels[paymentpb.PayChannel(intent.Channel)]
	if !ok {
		// 渠道被下掉了(配置关了):过了放弃期限就关掉;之前保留,渠道回来后接着查。
		if nowMs >= giveUp {
			_, err := s.store.CloseIntent(ctx, intent.OrderHi, intent.OrderLo, nowMs)
			return err
		}
		return s.store.ScheduleCheck(ctx, intent, lease, nowMs+uint64(time.Hour.Milliseconds()), nowMs)
	}
	result, queryErr := s.query(ctx, ch, intent)
	unsupported := errors.Is(queryErr, channel.ErrQueryUnsupported)
	if queryErr == nil {
		switch result.State {
		case channel.TradePaid:
			return s.record(ctx, ch, *result.Payment, false)
		case channel.TradeRefundedOutside:
			return s.record(ctx, ch, *result.Payment, true)
		case channel.TradeClosed:
			_, err := s.store.CloseIntent(ctx, intent.OrderHi, intent.OrderLo, nowMs)
			return err
		}
	}
	if nowMs >= deadline && (queryErr == nil || unsupported) {
		if !unsupported {
			if err := ch.Close(ctx, mustOutTradeNo(intent.OrderId)); err != nil {
				observeCall(ch.Name(), "close", err)
				return s.store.ScheduleCheck(ctx, intent, lease, nowMs+uint64(backoff(intent.CheckCount, s.opts.CheckInterval).Milliseconds()), nowMs)
			}
			final, err := s.query(ctx, ch, intent)
			if err != nil {
				return s.store.ScheduleCheck(ctx, intent, lease, nowMs+uint64(backoff(intent.CheckCount, s.opts.CheckInterval).Milliseconds()), nowMs)
			}
			switch final.State {
			case channel.TradePaid:
				return s.record(ctx, ch, *final.Payment, false)
			case channel.TradeRefundedOutside:
				return s.record(ctx, ch, *final.Payment, true)
			}
		}
		_, err := s.store.CloseIntent(ctx, intent.OrderHi, intent.OrderLo, nowMs)
		if err == nil {
			countIntent(ch.Name(), "closed")
		}
		return err
	}
	if queryErr != nil && !unsupported && nowMs >= giveUp {
		logx.WithContext(ctx).Errorf("[payment] Giving up on querying order %s on %s a day after its deadline; a late payment is still recorded from the channel callback: %v",
			intent.OrderId, ch.Name(), queryErr)
		_, err := s.store.CloseIntent(ctx, intent.OrderHi, intent.OrderLo, nowMs)
		return err
	}
	next := nowMs + uint64(s.opts.CheckInterval.Milliseconds())
	if queryErr != nil && !unsupported {
		next = nowMs + uint64(backoff(intent.CheckCount, s.opts.CheckInterval).Milliseconds())
	}
	if unsupported || (nowMs < deadline && next > deadline) {
		next = max(deadline, nowMs+1) // 不能查单的只在到期时再来一次;能查单的把最后一次对准宽限点
	}
	return s.store.ScheduleCheck(ctx, intent, lease, next, nowMs)
}

func mustOutTradeNo(orderID string) string {
	outTradeNo, _ := data.OutTradeNo(orderID)
	return outTradeNo
}

// ---------------------------------------------------------------------------
// 推进商城
// ---------------------------------------------------------------------------

func (s *Service) pushMall(ctx context.Context, relay *drainproof.Session) {
	nowMs := uint64(s.now().UnixMilli())
	scan, cancel := context.WithTimeout(ctx, 2*time.Second)
	due, err := s.store.DueMallTxns(scan, nowMs, s.opts.Batch)
	cancel()
	if err != nil {
		s.logWorkerError(ctx, "scanning payments due for the mall", err)
		return
	}
	oldest := uint64(0)
	for _, txn := range due {
		if txn.MallState == data.MallPending && nowMs > txn.CreatedMs && nowMs-txn.CreatedMs > oldest {
			oldest = nowMs - txn.CreatedMs
		}
	}
	mallOldestPending.Set(float64(oldest) / 1000)
	for _, txn := range due {
		if ctx.Err() != nil {
			return
		}
		txn := txn
		s.logWorkerError(ctx, "pushing transaction ..."+tail(txn.TransactionId)+" to the mall", unit(relay, func() error { return s.pushOne(ctx, txn) }))
	}
}

// pushOne 推进一笔付款:商城还没记下的先记账(修订号 0),之后把更大的修订号同步过去(撤销 / 撤销被撤回)。
// 商城明确拒收(400 参数非法、409 与已记下的冲突)是终局,转人工;其余失败按退避重试,一直重试到成功:钱已经收了。
func (s *Service) pushOne(parent context.Context, txn *paymentpb.PaymentTxnRecord) error {
	ctx, cancel := s.background(parent)
	defer cancel()
	nowMs := uint64(s.now().UnixMilli())
	lease := nowMs + uint64(s.opts.Lease.Milliseconds())
	claimed, err := s.store.ClaimMall(ctx, txn, lease, nowMs)
	if err != nil || !claimed {
		return err
	}
	name := channel.NameOf(paymentpb.PayChannel(txn.Channel))
	orderID := data.OrderID(txn.OrderHi, txn.OrderLo)
	retry := func(reason string) error {
		mallPushes.WithLabelValues("retry").Inc()
		return s.store.DeferMall(ctx, txn, lease, nowMs+uint64(backoff(txn.MallAttempts+1, s.opts.MallBackoffMax).Milliseconds()), reason, nowMs)
	}
	callCtx, cancelCall := context.WithTimeout(ctx, s.opts.BackgroundCallTimeout)
	defer cancelCall()
	if txn.MallState == data.MallPending {
		outcome, err := s.mall.RecordPayment(callCtx, mall.Payment{OrderID: orderID, Channel: name, TransactionID: txn.TransactionId,
			AmountMinor: txn.PaidAmountMinor, Currency: txn.PaidCurrency})
		switch {
		case err == nil && outcome.RefundReason == "":
			mallPushes.WithLabelValues("bound").Inc()
			return s.store.SettleMall(ctx, txn, lease, data.MallSync{State: data.MallBound, KeepSyncing: true}, nowMs)
		case err == nil:
			mallPushes.WithLabelValues("refund_queued").Inc()
			if outcome.RefundReason == "AMOUNT_MISMATCH" || outcome.RefundReason == "CURRENCY_MISMATCH" {
				logx.WithContext(ctx).Errorf("[payment] Mall queued a refund because the channel amount differs from the order: order=%s channel=%s reason=%s",
					orderID, name, outcome.RefundReason)
			}
			return s.store.SettleMall(ctx, txn, lease, data.MallSync{State: data.MallRefundQueued, RefundReason: outcome.RefundReason, KeepSyncing: true}, nowMs)
		case errors.Is(err, mall.ErrConflict), errors.Is(err, mall.ErrInvalid):
			mallPushes.WithLabelValues("rejected").Inc()
			logx.WithContext(ctx).Errorf("[payment] Mall rejected a payment, manual handling needed: order=%s channel=%s transaction=...%s: %v",
				orderID, name, tail(txn.TransactionId), err)
			return s.store.SettleMall(ctx, txn, lease, data.MallSync{State: data.MallRejected, Revision: txn.MallRevision, LastError: errorClass(err)}, nowMs)
		default:
			return retry(errorClass(err))
		}
	}
	fact := mall.FactPaid
	if data.Revoked(txn.FactRevision) {
		fact = mall.FactRevoked
	}
	acked, err := s.mall.PushRevision(callCtx, mall.Revision{OrderID: orderID, Channel: name, TransactionID: txn.TransactionId, Revision: txn.FactRevision, Fact: fact})
	switch {
	case err == nil && acked >= txn.FactRevision:
		mallPushes.WithLabelValues("revision").Inc()
		return s.store.SettleMall(ctx, txn, lease, data.MallSync{State: txn.MallState, RefundReason: txn.MallRefundReason, Revision: txn.FactRevision, KeepSyncing: true}, nowMs)
	case err == nil:
		logx.WithContext(ctx).Errorf("[payment] Mall acknowledged revision %d for transaction ...%s but reports %d; retrying", txn.FactRevision, tail(txn.TransactionId), acked)
		return retry("mall_revision_behind")
	case errors.Is(err, mall.ErrInvalid):
		mallPushes.WithLabelValues("rejected").Inc()
		logx.WithContext(ctx).Errorf("[payment] Mall rejected revision %d of transaction ...%s as invalid, manual handling needed", txn.FactRevision, tail(txn.TransactionId))
		return s.store.SettleMall(ctx, txn, lease, data.MallSync{State: txn.MallState, RefundReason: txn.MallRefundReason, Revision: txn.MallRevision, LastError: "mall_invalid_revision"}, nowMs)
	default:
		return retry(errorClass(err))
	}
}

// errorClass 是写进 *_last_error 的失败类别:不含请求体、密钥与个人信息。
func errorClass(err error) string {
	var status *mall.StatusError
	var call *channel.CallError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, mall.ErrConflict):
		return "mall_conflict"
	case errors.Is(err, mall.ErrInvalid):
		return "mall_invalid"
	case errors.Is(err, mall.ErrNotFound):
		return "mall_not_found"
	case errors.As(err, &status):
		return "mall_http_" + strconv.Itoa(status.Status)
	case errors.As(err, &call):
		return call.Channel + "_" + call.Op + "_" + strconv.Itoa(call.Status) + "_" + call.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "unreachable"
	}
}

// ---------------------------------------------------------------------------
// 退款
// ---------------------------------------------------------------------------

func (s *Service) processRefunds(ctx context.Context, relay *drainproof.Session) {
	callCtx, cancel := context.WithTimeout(ctx, s.opts.BackgroundCallTimeout)
	pending, err := s.mall.PendingRefunds(callCtx)
	cancel()
	if err != nil {
		s.logWorkerError(ctx, "polling mall refunds", err)
		return
	}
	for _, item := range pending {
		if ctx.Err() != nil {
			return
		}
		ch, ok := s.byName[item.Channel]
		if !ok {
			continue // 不是本服务的渠道(商城本地联调 mock 等)
		}
		item := item
		s.logWorkerError(ctx, "refunding transaction ..."+tail(item.TransactionID), unit(relay, func() error { return s.refundOne(ctx, ch, item) }))
	}
}

// refundOne 执行一条商城退款待办。退款单号由被退那笔付款确定性派生,渠道按它去重;退成之后回执商城。
// 渠道退不了(没有开发者退款接口,或明确失败)的回执商城转人工,不再列在待退款里。
func (s *Service) refundOne(parent context.Context, ch channel.Channel, item mall.PendingRefund) error {
	ctx, cancel := s.background(parent)
	defer cancel()
	hi, lo := data.TxnKey(item.TransactionID)
	txn, err := s.store.Txn(ctx, uint32(ch.ID()), hi, lo)
	if errors.Is(err, data.ErrNotFound) {
		logx.WithContext(ctx).Errorf("[payment] Mall asks to refund a payment this service never recorded: channel=%s transaction=...%s", ch.Name(), tail(item.TransactionID))
		return nil
	}
	if err != nil {
		return err
	}
	if txn.TransactionId != item.TransactionID || txn.PaidAmountMinor != item.AmountMinor || txn.PaidCurrency != item.Currency {
		logx.WithContext(ctx).Errorf("[payment] Mall refund differs from the recorded payment, manual handling needed: channel=%s transaction=...%s", ch.Name(), tail(item.TransactionID))
		return nil
	}
	callCtx, cancelCall := context.WithTimeout(ctx, s.opts.BackgroundCallTimeout)
	defer cancelCall()
	switch txn.RefundState {
	case data.RefundReported:
		return s.reportRefund(callCtx, ch, item, txn) // 商城还列着:上次的回执可能没落库,重发(幂等)
	case data.RefundFailed:
		return s.closeRefund(callCtx, ch, item, txn.RefundLastError)
	}
	nowMs := uint64(s.now().UnixMilli())
	if txn.RefundNextMs > nowMs {
		return nil
	}
	lease := nowMs + uint64(s.opts.Lease.Milliseconds())
	claimed, err := s.store.ClaimRefund(ctx, txn, lease, nowMs)
	if err != nil || !claimed {
		return err
	}
	progress := data.RefundProgress{State: txn.RefundState, OutRefundNo: txn.OutRefundNo, RefundID: txn.RefundId, RefundMs: txn.RefundMs, NextMs: lease}
	if progress.OutRefundNo == "" {
		progress.OutRefundNo = data.OutRefundNo(ch.Name(), txn.TransactionId)
	}
	retryAt := nowMs + uint64(backoff(txn.RefundAttempts+1, s.opts.RefundBackoffMax).Milliseconds())
	if progress.State != data.RefundSucceeded {
		result, err := ch.Refund(callCtx, channel.RefundRequest{OutTradeNo: txn.OutTradeNo, TransactionID: txn.TransactionId, OutRefundNo: progress.OutRefundNo,
			AmountMinor: txn.PaidAmountMinor, Currency: txn.PaidCurrency, Reason: s.opts.RefundReason, ChannelToken: txn.ChannelToken, ChannelOrderID: txn.ChannelOrderId})
		observeCall(ch.Name(), "refund", err)
		switch {
		case err != nil:
			progress.NextMs, progress.LastError = retryAt, errorClass(err)
			return s.store.SaveRefund(ctx, txn, lease, progress, nowMs)
		case result.State == channel.RefundProcessing:
			progress.State, progress.NextMs, progress.LastError = data.RefundRequested, retryAt, result.Detail
			refunds.WithLabelValues(ch.Name(), "processing").Inc()
			return s.store.SaveRefund(ctx, txn, lease, progress, nowMs)
		case result.State == channel.RefundFailed:
			progress.State, progress.LastError = data.RefundFailed, result.Detail
			refunds.WithLabelValues(ch.Name(), "failed").Inc()
			logx.WithContext(ctx).Errorf("[payment] Refund cannot be done on %s, handing it to manual handling: transaction=...%s detail=%s", ch.Name(), tail(txn.TransactionId), result.Detail)
			if err := s.store.SaveRefund(ctx, txn, lease, progress, nowMs); err != nil {
				return err
			}
			return s.closeRefund(callCtx, ch, item, result.Detail)
		default:
			at := result.At
			if at.IsZero() {
				at = s.now()
			}
			progress.State, progress.RefundID, progress.RefundMs, progress.LastError = data.RefundSucceeded, result.RefundID, uint64(at.UnixMilli()), ""
			refunds.WithLabelValues(ch.Name(), "succeeded").Inc()
			if err := s.store.SaveRefund(ctx, txn, lease, progress, nowMs); err != nil {
				return err
			}
		}
	}
	txn.RefundId = progress.RefundID
	if err := s.reportRefund(callCtx, ch, item, txn); err != nil {
		progress.NextMs, progress.LastError = retryAt, errorClass(err)
		if errors.Is(err, mall.ErrConflict) {
			progress.NextMs = nowMs + uint64(time.Hour.Milliseconds())
			logx.WithContext(ctx).Errorf("[payment] Mall rejected a refund receipt as conflicting, manual handling needed: transaction=...%s", tail(txn.TransactionId))
		}
		return s.store.SaveRefund(ctx, txn, lease, progress, nowMs)
	}
	progress.State, progress.NextMs = data.RefundReported, 0
	return s.store.SaveRefund(ctx, txn, lease, progress, nowMs)
}

func (s *Service) reportRefund(ctx context.Context, ch channel.Channel, item mall.PendingRefund, txn *paymentpb.PaymentTxnRecord) error {
	return s.mall.ReportRefund(ctx, mall.Refund{OrderID: item.OrderID, Channel: ch.Name(), PaymentTransactionID: txn.TransactionId,
		TransactionID: txn.RefundId, AmountMinor: txn.PaidAmountMinor, Currency: txn.PaidCurrency})
}

func (s *Service) closeRefund(ctx context.Context, ch channel.Channel, item mall.PendingRefund, reason string) error {
	if reason == "" {
		reason = "CHANNEL_REFUND_FAILED"
	}
	return s.mall.CloseRefund(ctx, mall.RefundClosure{OrderID: item.OrderID, Channel: ch.Name(), PaymentTransactionID: item.TransactionID, Reason: reason})
}

// ---------------------------------------------------------------------------
// 渠道收尾(Google consume)
// ---------------------------------------------------------------------------

func (s *Service) finalize(ctx context.Context, relay *drainproof.Session) {
	nowMs := uint64(s.now().UnixMilli())
	scan, cancel := context.WithTimeout(ctx, 2*time.Second)
	due, err := s.store.DueFinalize(scan, nowMs, s.opts.Batch)
	cancel()
	if err != nil {
		s.logWorkerError(ctx, "scanning payments due for channel finalization", err)
		return
	}
	for _, txn := range due {
		if ctx.Err() != nil {
			return
		}
		txn := txn
		s.logWorkerError(ctx, "finalizing transaction ..."+tail(txn.TransactionId), unit(relay, func() error { return s.finalizeOne(ctx, txn) }))
	}
}

func (s *Service) finalizeOne(parent context.Context, txn *paymentpb.PaymentTxnRecord) error {
	ctx, cancel := s.background(parent)
	defer cancel()
	nowMs := uint64(s.now().UnixMilli())
	lease := nowMs + uint64(s.opts.Lease.Milliseconds())
	claimed, err := s.store.ClaimFinalize(ctx, txn, lease, nowMs)
	if err != nil || !claimed {
		return err
	}
	ch, ok := s.channels[paymentpb.PayChannel(txn.Channel)]
	finalizer, can := ch.(channel.Finalizer)
	if !ok || !can {
		return s.store.SaveFinalize(ctx, txn, lease, data.FinalizePending, nowMs+uint64(time.Hour.Milliseconds()), nowMs)
	}
	callCtx, cancelCall := context.WithTimeout(ctx, s.opts.BackgroundCallTimeout)
	err = finalizer.Finalize(callCtx, txn.ProductId, txn.ChannelToken)
	cancelCall()
	observeCall(ch.Name(), "finalize", err)
	if err != nil {
		attempts := uint32((nowMs - txn.CreatedMs) / 60000) // 按记账后过去的分钟数退避,三天的期限内会重试很多次
		return s.store.SaveFinalize(ctx, txn, lease, data.FinalizePending, nowMs+uint64(backoff(min(attempts, 10), 10*time.Minute).Milliseconds()), nowMs)
	}
	return s.store.SaveFinalize(ctx, txn, lease, data.FinalizeDone, 0, nowMs)
}

// ---------------------------------------------------------------------------
// 补拉渠道历史
// ---------------------------------------------------------------------------

func (s *Service) pollHistory(ctx context.Context, relay *drainproof.Session) {
	for _, ch := range s.channels {
		poller, ok := ch.(channel.HistoryPoller)
		if !ok || ctx.Err() != nil {
			continue
		}
		ch := ch
		s.logWorkerError(ctx, "polling "+ch.Name()+" history", unit(relay, func() error { return s.pollOne(ctx, ch, poller) }))
	}
}

// pollOne 领渠道游标的租约,从上次拉到的时刻补拉到现在,逐条按回调处理;全部处理成功才推进游标。
func (s *Service) pollOne(parent context.Context, ch channel.Channel, poller channel.HistoryPoller) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Minute)
	defer cancel()
	var raw [8]byte
	if _, err := cryptoRandRead(raw[:]); err != nil {
		return err
	}
	owner := binary.LittleEndian.Uint64(raw[:]) | 1
	now := s.now()
	nowMs := uint64(now.UnixMilli())
	cursorMs, claimed, err := s.store.ClaimCursor(ctx, uint32(ch.ID()), owner, uint64(now.Add(-s.opts.HistoryLookback).UnixMilli()),
		nowMs+uint64((6*time.Minute).Milliseconds()), nowMs)
	if err != nil || !claimed {
		return err
	}
	notes, through, err := poller.PollHistory(ctx, time.UnixMilli(int64(cursorMs)))
	if err != nil {
		observeCall(ch.Name(), "history", err)
		return s.store.AdvanceCursor(ctx, uint32(ch.ID()), owner, cursorMs, uint64(s.now().UnixMilli()))
	}
	for _, note := range notes {
		if err := s.HandleNotification(ctx, ch, note); err != nil {
			_ = s.store.AdvanceCursor(ctx, uint32(ch.ID()), owner, cursorMs, uint64(s.now().UnixMilli()))
			return err
		}
	}
	return s.store.AdvanceCursor(ctx, uint32(ch.ID()), owner, uint64(through.UnixMilli()), uint64(s.now().UnixMilli()))
}

var cryptoRandRead = cryptorand.Read

// ---------------------------------------------------------------------------
// 指标(低基数:标签只有渠道名、动作与结论,不带玩家、订单、交易号)
// ---------------------------------------------------------------------------

var (
	channelCalls = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "payment", Name: "channel_calls_total",
		Help: "渠道接口调用结果:ok 成功;rejected 渠道明确回了失败;error 传输失败或超时。"}, []string{"channel", "op", "outcome"})
	notifications = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "payment", Name: "notifications_total",
		Help: "渠道回调处理结果:handled 已处理;rejected 验签或信封不过;retry 本服务没处理成、要渠道重发。"}, []string{"channel", "outcome"})
	intents = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "payment", Name: "intents_total",
		Help: "支付意图:created 新建;prepared 签出调起参数;closed 到期关闭。"}, []string{"channel", "outcome"})
	facts = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "payment", Name: "facts_total",
		Help: "付款事实:recorded 新记账;duplicate 重复报告;orphan 转人工;conflict 同一交易号内容不同;revoked / reinstated 渠道翻转;store_error 写库失败。"}, []string{"channel", "outcome"})
	mallPushes = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "payment", Name: "mall_pushes_total",
		Help: "推进商城的结果:bound / refund_queued 记账;revision 修订同步;rejected 商城拒收转人工;retry 稍后重试。"}, []string{"outcome"})
	refunds = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "payment", Name: "refunds_total",
		Help: "渠道退款的结果:succeeded 已退;processing 处理中;failed 退不了转人工。"}, []string{"channel", "outcome"})
	mallOldestPending = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "payment", Name: "mall_oldest_pending_seconds",
		Help: "最近一轮扫描到的、商城还没记下的付款里最老的一笔已经等了多久。超过 10 分钟要告警(设计 §15)。"})
	registerOnce sync.Once
)

// RegisterMetrics 把指标登记到默认注册表(domainhost 的 /metrics 输出它)。只调一次;用例不调,计数照常可用。
func RegisterMetrics() {
	registerOnce.Do(func() {
		prometheus.MustRegister(channelCalls, notifications, intents, facts, mallPushes, refunds, mallOldestPending)
	})
}

func observeCall(name, op string, err error) {
	var call *channel.CallError
	switch {
	case err == nil:
		channelCalls.WithLabelValues(name, op, "ok").Inc()
	case errors.As(err, &call):
		channelCalls.WithLabelValues(name, op, "rejected").Inc()
	default:
		channelCalls.WithLabelValues(name, op, "error").Inc()
	}
}

func countIntent(name, outcome string) { intents.WithLabelValues(name, outcome).Inc() }

func countFact(name, outcome string) { facts.WithLabelValues(name, outcome).Inc() }

// ObserveNotification 记一次回调的处理结论(notify 包用)。
func ObserveNotification(name, outcome string) { notifications.WithLabelValues(name, outcome).Inc() }
