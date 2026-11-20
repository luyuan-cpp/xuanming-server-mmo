package data

import (
	"context"
	"database/sql"
	"errors"

	paymentpb "proto/payment"
)

// 未成年人充值额度(docs/design/real-name-anti-addiction.md §5.6;充值设计 §12)。
//
// 额度不另记计数器,由这个人名下的意图行算出:CREATED 的(还能付款)全额占着,PAID 的按 paid_ms 计入当月与当日,
// CLOSED 的(过期关闭、被渠道撤销)不占。「商城关单和玩家取消不释放,只在过期或转已付时变」(§12)由意图的状态机自然成立,
// 意图转已付、到期关闭都不用另改计数。
//
// 在途的意图不分月份全额计入:它可能跨月之后才到账。每次预占都按「本月起已付 + 全部在途 + 本次 ≤ 上限」核对,
// 任何一个月最终到账的总额就不会超过上限,在途的意图到账时不用再核一次。不在这个保证里的只有意图已关闭之后才到的钱
// (迟到的付款、App Store 撤回退款),由 ReviveMinorIntent 在同一把锁下单独复核。

var (
	ErrMonthlyLimit = errors.New("payment: minor monthly spending limit reached")
	ErrDailyLimit   = errors.New("payment: minor daily spending limit reached")
)

// SpendLimit 是一次预占或复核适用的未成年人额度(分)与计数窗口的起点(北京时间本月一日零点、本日零点,Unix 毫秒)。
type SpendLimit struct {
	MonthlyFen   uint64
	DailyFen     uint64 // 0 = 不设单日上限
	MonthStartMs uint64
	DayStartMs   uint64
}

// heldSpend 是这个人名下占着额度的一行意图。
type heldSpend struct {
	state  uint32
	amount uint64
	paidMs uint64
}

// admit 核对再花 amount 之后是否超限。已付的按 paid_ms 计:不早于本月一日零点的都算本月,时钟偏差下记在「未来」的也算。
func (l SpendLimit) admit(held []heldSpend, amount uint64) error {
	var inflight, month, day uint64
	for _, h := range held {
		switch {
		case h.state == IntentCreated:
			inflight += h.amount
		case h.state == IntentPaid && h.paidMs >= l.MonthStartMs:
			month += h.amount
			if h.paidMs >= l.DayStartMs {
				day += h.amount
			}
		}
	}
	if amount+inflight+month > l.MonthlyFen {
		return ErrMonthlyLimit
	}
	if l.DailyFen > 0 && amount+inflight+day > l.DailyFen {
		return ErrDailyLimit
	}
	return nil
}

// lockSpender 锁这个人的额度行(没有就建):同一个人的预占与复核在这里排队,直到本事务提交或回滚。
func lockSpender(ctx context.Context, tx *sql.Tx, hi, lo, now uint64) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO payment_spend (spender_hi,spender_lo,updated_ms) VALUES (?,?,?) ON DUPLICATE KEY UPDATE updated_ms=?",
		hi, lo, now, now)
	return err
}

// spendTx 开额度事务。读已提交:每条语句读最新提交的行,拿到额度行锁之后看得见排在前面刚提交的那一笔
// (TiDB 可重复读下不加锁的 SELECT 读事务开始时的快照,看不见它);MySQL 下也不加间隙锁,不同人的预占互不牵连。
func (r *Repo) spendTx(ctx context.Context) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

// checkSpend 在额度行锁之下读这个人占着额度的意图,核对再花 rec 这一笔是否超限;rec 自己的订单不计入。
// 锁读(FOR UPDATE):核对到提交之间,读到的这些行不会被别的语句改掉。
func checkSpend(ctx context.Context, tx *sql.Tx, rec *paymentpb.PaymentIntentRecord, limit SpendLimit) error {
	rows, err := tx.QueryContext(ctx, "SELECT state,amount_minor,paid_ms FROM payment_intent WHERE spender_hi=? AND spender_lo=? "+
		"AND (state=? OR (state=? AND paid_ms>=?)) AND NOT (order_hi=? AND order_lo=?) FOR UPDATE",
		rec.SpenderHi, rec.SpenderLo, IntentCreated, IntentPaid, limit.MonthStartMs, rec.OrderHi, rec.OrderLo)
	held, err := scanAll(rows, err, func(row scanner) (heldSpend, error) {
		var h heldSpend
		err := row.Scan(&h.state, &h.amount, &h.paidMs)
		return h, err
	})
	if err != nil {
		return err
	}
	return limit.admit(held, rec.AmountMinor)
}

// ReserveIntent 为未成年人新建意图并预占额度:在他的额度行锁下核对,通过才插入。返回值同 InsertIntent;
// 超限回 ErrMonthlyLimit / ErrDailyLimit,什么都不写。同一订单并发建意图时,后到的拿到已有的那一行(核对时不计它,不会被自己挡住)。
func (r *Repo) ReserveIntent(ctx context.Context, rec *paymentpb.PaymentIntentRecord, limit SpendLimit) (*paymentpb.PaymentIntentRecord, bool, error) {
	tx, err := r.spendTx(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSpender(ctx, tx, rec.SpenderHi, rec.SpenderLo, rec.CreatedMs); err != nil {
		return nil, false, err
	}
	if err := checkSpend(ctx, tx, rec, limit); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, insertIntentSQL, intentArgs(rec)...); err != nil {
		if !duplicateKey(err) {
			return nil, false, err
		}
		_ = tx.Rollback()
		existing, err := r.Intent(ctx, rec.OrderHi, rec.OrderLo)
		if err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// ReviveMinorIntent 在额度行锁下复核一笔已关闭的未成年人意图重新转成已付(迟到的付款、App Store 撤回退款)。
// 额度够:转 PAID、计入 paidMs 所在的月与日,回 true。不够:force 为假时什么都不改、回 false(这笔钱转人工,不发货);
// force 为真时照样转 PAID(钱又回到商户,商城照常收敛),回 false 让调用方记合规事件。
// 意图已经不是 CLOSED(同一笔付款的另一次报告先转了已付,或者仍在途)时它的额度早已计入,回 true、不改。
func (r *Repo) ReviveMinorIntent(ctx context.Context, intent *paymentpb.PaymentIntentRecord, limit SpendLimit, paidMs, now uint64, force bool) (bool, error) {
	tx, err := r.spendTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSpender(ctx, tx, intent.SpenderHi, intent.SpenderLo, now); err != nil {
		return false, err
	}
	var state uint32
	err = tx.QueryRowContext(ctx, "SELECT state FROM payment_intent WHERE order_hi=? AND order_lo=? FOR UPDATE", intent.OrderHi, intent.OrderLo).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotFound
	case err != nil:
		return false, err
	case state != IntentClosed:
		return true, nil
	}
	within := true
	if err := checkSpend(ctx, tx, intent, limit); err != nil {
		if !errors.Is(err, ErrMonthlyLimit) && !errors.Is(err, ErrDailyLimit) {
			return false, err
		}
		within = false
	}
	if !within && !force {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE payment_intent SET state=?,paid_ms=?,channel_ref='',updated_ms=? WHERE order_hi=? AND order_lo=? AND state=?",
		IntentPaid, paidMs, now, intent.OrderHi, intent.OrderLo, IntentClosed); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return within, nil
}
