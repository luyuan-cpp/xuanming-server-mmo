package data

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"
	paymentpb "proto/payment"
)

// ErrNotFound:要找的行不存在。
var ErrNotFound = errors.New("payment: record not found")

// 状态取值,与 proto 枚举同值;SQL 里写数字列,这里给出同名常量,免得业务代码拿枚举反复转型。
const (
	IntentCreated = uint32(paymentpb.PayIntentState_PAY_INTENT_STATE_CREATED)
	IntentPaid    = uint32(paymentpb.PayIntentState_PAY_INTENT_STATE_PAID)
	IntentClosed  = uint32(paymentpb.PayIntentState_PAY_INTENT_STATE_CLOSED)

	MallPending      = uint32(paymentpb.PayMallState_PAY_MALL_STATE_PENDING)
	MallBound        = uint32(paymentpb.PayMallState_PAY_MALL_STATE_BOUND)
	MallRefundQueued = uint32(paymentpb.PayMallState_PAY_MALL_STATE_REFUND_QUEUED)
	MallRejected     = uint32(paymentpb.PayMallState_PAY_MALL_STATE_REJECTED)
	MallOrphan       = uint32(paymentpb.PayMallState_PAY_MALL_STATE_ORPHAN)

	RefundNone      = uint32(paymentpb.PayRefundState_PAY_REFUND_STATE_NONE)
	RefundRequested = uint32(paymentpb.PayRefundState_PAY_REFUND_STATE_REQUESTED)
	RefundSucceeded = uint32(paymentpb.PayRefundState_PAY_REFUND_STATE_SUCCEEDED)
	RefundReported  = uint32(paymentpb.PayRefundState_PAY_REFUND_STATE_REPORTED)
	RefundFailed    = uint32(paymentpb.PayRefundState_PAY_REFUND_STATE_FAILED)

	FinalizeNone    = uint32(paymentpb.PayFinalizeState_PAY_FINALIZE_STATE_NONE)
	FinalizePending = uint32(paymentpb.PayFinalizeState_PAY_FINALIZE_STATE_PENDING)
	FinalizeDone    = uint32(paymentpb.PayFinalizeState_PAY_FINALIZE_STATE_DONE)
)

// Revoked 报告修订号表示的事实:奇数 = 已被渠道撤销。
func Revoked(revision uint32) bool { return revision%2 == 1 }

// Repo 只用单条自动提交语句,不开显式事务:每一步都按自然键幂等,中途崩溃由下一次查单、回调或扫描补齐
// (先写付款事实、再改意图状态,见 core.Service.record)。
// 「领取」一律是带旧值比对的 UPDATE:多副本同时扫到同一行时只有一个改成功,其余放手。
// 同一条 UPDATE 里依赖本行旧值的表达式写在被赋值的列之前(MySQL 按从左到右求值,TiDB 与之兼容)。
type Repo struct{ db *sql.DB }

func New(db *sql.DB) *Repo { return &Repo{db: db} }

type scanner interface{ Scan(...any) error }

const intentColumns = "order_hi,order_lo,order_id,player_id,channel,amount_minor,currency,state,expire_ms,next_check_ms,check_count,checked_ms," +
	"created_ms,updated_ms,channel_ref,channel_ref_ms,sku_id,payer_id"

func scanIntent(row scanner) (*paymentpb.PaymentIntentRecord, error) {
	r := &paymentpb.PaymentIntentRecord{}
	err := row.Scan(&r.OrderHi, &r.OrderLo, &r.OrderId, &r.PlayerId, &r.Channel, &r.AmountMinor, &r.Currency, &r.State,
		&r.ExpireMs, &r.NextCheckMs, &r.CheckCount, &r.CheckedMs, &r.CreatedMs, &r.UpdatedMs, &r.ChannelRef, &r.ChannelRefMs, &r.SkuId, &r.PayerId)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

const txnColumns = "channel,txn_hi,txn_lo,transaction_id,order_hi,order_lo,out_trade_no,player_id,paid_amount_minor,paid_currency,paid_ms," +
	"mall_state,mall_refund_reason,mall_attempts,next_attempt_ms,mall_last_error," +
	"refund_state,out_refund_no,refund_id,refund_ms,refund_attempts,refund_next_ms,refund_last_error,created_ms,updated_ms," +
	"fact_revision,fact_observed_ms,mall_revision,mall_due,channel_token,channel_order_id,store_amount_minor,store_currency," +
	"finalize_state,finalize_next_ms,product_id"

func scanTxn(row scanner) (*paymentpb.PaymentTxnRecord, error) {
	r := &paymentpb.PaymentTxnRecord{}
	err := row.Scan(&r.Channel, &r.TxnHi, &r.TxnLo, &r.TransactionId, &r.OrderHi, &r.OrderLo, &r.OutTradeNo, &r.PlayerId,
		&r.PaidAmountMinor, &r.PaidCurrency, &r.PaidMs,
		&r.MallState, &r.MallRefundReason, &r.MallAttempts, &r.NextAttemptMs, &r.MallLastError,
		&r.RefundState, &r.OutRefundNo, &r.RefundId, &r.RefundMs, &r.RefundAttempts, &r.RefundNextMs, &r.RefundLastError, &r.CreatedMs, &r.UpdatedMs,
		&r.FactRevision, &r.FactObservedMs, &r.MallRevision, &r.MallDue, &r.ChannelToken, &r.ChannelOrderId, &r.StoreAmountMinor, &r.StoreCurrency,
		&r.FinalizeState, &r.FinalizeNextMs, &r.ProductId)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func duplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

// changed 把「改到了 1 行」当成领取或推进成功;其余行数(0:被别的副本抢先,或状态已经变了)当成没领到。
func changed(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

func scanAll[T any](rows *sql.Rows, err error, scan func(scanner) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// payment_intent
// ---------------------------------------------------------------------------

// Intent 按订单键读一行支付意图。
func (r *Repo) Intent(ctx context.Context, hi, lo uint64) (*paymentpb.PaymentIntentRecord, error) {
	return scanIntent(r.db.QueryRowContext(ctx, "SELECT "+intentColumns+" FROM payment_intent WHERE order_hi=? AND order_lo=?", hi, lo))
}

// InsertIntent 新建一行支付意图。同一订单已有意图时不改它,返回已有的那一行与 false:
// 并发建意图的两个请求只有一个插入成功,另一个拿到同一行,渠道以第一个为准。
func (r *Repo) InsertIntent(ctx context.Context, rec *paymentpb.PaymentIntentRecord) (*paymentpb.PaymentIntentRecord, bool, error) {
	_, err := r.db.ExecContext(ctx, "INSERT INTO payment_intent ("+intentColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		rec.OrderHi, rec.OrderLo, rec.OrderId, rec.PlayerId, rec.Channel, rec.AmountMinor, rec.Currency, rec.State,
		rec.ExpireMs, rec.NextCheckMs, rec.CheckCount, rec.CheckedMs, rec.CreatedMs, rec.UpdatedMs, rec.ChannelRef, rec.ChannelRefMs, rec.SkuId, rec.PayerId)
	if err == nil {
		return rec, true, nil
	}
	if !duplicateKey(err) {
		return nil, false, err
	}
	existing, err := r.Intent(ctx, rec.OrderHi, rec.OrderLo)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

// SetChannelRef 记下渠道引用与付款人。只改仍在等付款的意图:已付或已关的不需要了。
func (r *Repo) SetChannelRef(ctx context.Context, hi, lo uint64, ref string, refMs uint64, payer string, now uint64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE payment_intent SET channel_ref=?,channel_ref_ms=?,payer_id=?,updated_ms=? WHERE order_hi=? AND order_lo=? AND state=?",
		ref, refMs, payer, now, hi, lo, IntentCreated)
	return err
}

// ClaimActiveQuery 领一次「客户端轮询触发的查单」:checked_ms 仍是读到的值才改成 now。
// 多个副本、多个轮询同时到达时只有一个去查渠道,其余直接回当前状态。
func (r *Repo) ClaimActiveQuery(ctx context.Context, rec *paymentpb.PaymentIntentRecord, now uint64) (bool, error) {
	return changed(r.db.ExecContext(ctx, "UPDATE payment_intent SET checked_ms=?,updated_ms=? WHERE order_hi=? AND order_lo=? AND state=? AND checked_ms=?",
		now, now, rec.OrderHi, rec.OrderLo, IntentCreated, rec.CheckedMs))
}

// DueIntents 列出到点该主动查单的意图,按到点先后。走 (state, next_check_ms) 索引。
func (r *Repo) DueIntents(ctx context.Context, now uint64, limit int) ([]*paymentpb.PaymentIntentRecord, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+intentColumns+" FROM payment_intent WHERE state=? AND next_check_ms<=? ORDER BY next_check_ms LIMIT ?",
		IntentCreated, now, limit)
	return scanAll(rows, err, scanIntent)
}

// ClaimCheck 领一次后台查单:把 next_check_ms 从读到的值推到租约到期时刻。进程在查单途中崩溃,租约过后别的副本重新看得见它。
func (r *Repo) ClaimCheck(ctx context.Context, rec *paymentpb.PaymentIntentRecord, lease, now uint64) (bool, error) {
	return changed(r.db.ExecContext(ctx, "UPDATE payment_intent SET next_check_ms=?,check_count=check_count+1,checked_ms=?,updated_ms=? WHERE order_hi=? AND order_lo=? AND state=? AND next_check_ms=?",
		lease, now, now, rec.OrderHi, rec.OrderLo, IntentCreated, rec.NextCheckMs))
}

// ScheduleCheck 排下一次后台查单。只改自己领到的那一次(next_check_ms 仍是自己的租约)。
func (r *Repo) ScheduleCheck(ctx context.Context, rec *paymentpb.PaymentIntentRecord, lease, next, now uint64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE payment_intent SET next_check_ms=?,updated_ms=? WHERE order_hi=? AND order_lo=? AND state=? AND next_check_ms=?",
		next, now, rec.OrderHi, rec.OrderLo, IntentCreated, lease)
	return err
}

// CloseIntent 把仍在等付款的意图关掉,清掉渠道引用(含会话密钥)。迟到的付款照样记账,意图会从 CLOSED 转成 PAID。
func (r *Repo) CloseIntent(ctx context.Context, hi, lo, now uint64) (bool, error) {
	return changed(r.db.ExecContext(ctx, "UPDATE payment_intent SET state=?,channel_ref='',updated_ms=? WHERE order_hi=? AND order_lo=? AND state=?",
		IntentClosed, now, hi, lo, IntentCreated))
}

// MarkIntentPaid 把意图标成已付,清掉渠道引用。已关闭的也改:钱是渠道确认收到的,事实优先于本服务的超时判断。
func (r *Repo) MarkIntentPaid(ctx context.Context, hi, lo, now uint64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE payment_intent SET state=?,channel_ref='',updated_ms=? WHERE order_hi=? AND order_lo=? AND state<>?",
		IntentPaid, now, hi, lo, IntentPaid)
	return err
}

// SetIntentState 按付款事实的翻转改意图的展示状态(撤销 → CLOSED,撤销被撤回 → PAID)。
func (r *Repo) SetIntentState(ctx context.Context, hi, lo uint64, state uint32, now uint64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE payment_intent SET state=?,channel_ref='',updated_ms=? WHERE order_hi=? AND order_lo=?", state, now, hi, lo)
	return err
}

// ---------------------------------------------------------------------------
// payment_txn
// ---------------------------------------------------------------------------

// Txn 按 (渠道, 交易号键) 读一行付款事实。
func (r *Repo) Txn(ctx context.Context, channel uint32, hi, lo uint64) (*paymentpb.PaymentTxnRecord, error) {
	return scanTxn(r.db.QueryRowContext(ctx, "SELECT "+txnColumns+" FROM payment_txn WHERE channel=? AND txn_hi=? AND txn_lo=?", channel, hi, lo))
}

// InsertTxn 记一笔渠道付款事实。同一笔已记过时不改它,返回已有的那一行与 false,由调用方比对内容。
func (r *Repo) InsertTxn(ctx context.Context, rec *paymentpb.PaymentTxnRecord) (*paymentpb.PaymentTxnRecord, bool, error) {
	_, err := r.db.ExecContext(ctx, "INSERT INTO payment_txn ("+txnColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		rec.Channel, rec.TxnHi, rec.TxnLo, rec.TransactionId, rec.OrderHi, rec.OrderLo, rec.OutTradeNo, rec.PlayerId,
		rec.PaidAmountMinor, rec.PaidCurrency, rec.PaidMs,
		rec.MallState, rec.MallRefundReason, rec.MallAttempts, rec.NextAttemptMs, rec.MallLastError,
		rec.RefundState, rec.OutRefundNo, rec.RefundId, rec.RefundMs, rec.RefundAttempts, rec.RefundNextMs, rec.RefundLastError, rec.CreatedMs, rec.UpdatedMs,
		rec.FactRevision, rec.FactObservedMs, rec.MallRevision, rec.MallDue, rec.ChannelToken, rec.ChannelOrderId, rec.StoreAmountMinor, rec.StoreCurrency,
		rec.FinalizeState, rec.FinalizeNextMs, rec.ProductId)
	if err == nil {
		return rec, true, nil
	}
	if !duplicateKey(err) {
		return nil, false, err
	}
	existing, err := r.Txn(ctx, rec.Channel, rec.TxnHi, rec.TxnLo)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

// FlipFact 翻转一笔付款的事实(付款 ↔ 撤销),修订号加 1。乐观并发:修订号仍是读到的值才改,改不到说明别的观测先到了,调用方重读再判。
// 还在跟商城同步的付款(未投、已绑定、已转退款)标成待同步;已在推进中的那一行保留它的租约,租约结束后会看到新的修订号。
func (r *Repo) FlipFact(ctx context.Context, rec *paymentpb.PaymentTxnRecord, observedMs, now uint64) (bool, error) {
	return changed(r.db.ExecContext(ctx, "UPDATE payment_txn SET "+
		"next_attempt_ms=CASE WHEN mall_due=1 THEN next_attempt_ms ELSE ? END,"+
		"mall_due=CASE WHEN mall_state IN (?,?,?) THEN 1 ELSE mall_due END,"+
		"fact_revision=fact_revision+1,fact_observed_ms=?,updated_ms=? "+
		"WHERE channel=? AND txn_hi=? AND txn_lo=? AND fact_revision=?",
		now, MallPending, MallBound, MallRefundQueued, observedMs, now, rec.Channel, rec.TxnHi, rec.TxnLo, rec.FactRevision))
}

// DueMallTxns 列出到点该推进商城的付款,按到点先后。走 (mall_due, next_attempt_ms) 索引。
func (r *Repo) DueMallTxns(ctx context.Context, now uint64, limit int) ([]*paymentpb.PaymentTxnRecord, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+txnColumns+" FROM payment_txn WHERE mall_due=1 AND next_attempt_ms<=? ORDER BY next_attempt_ms LIMIT ?", now, limit)
	return scanAll(rows, err, scanTxn)
}

// ClaimMall 领一次推进商城:next_attempt_ms 仍是读到的值才推到租约到期时刻。
func (r *Repo) ClaimMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, now uint64) (bool, error) {
	return changed(r.db.ExecContext(ctx, "UPDATE payment_txn SET next_attempt_ms=?,mall_attempts=mall_attempts+1,updated_ms=? WHERE channel=? AND txn_hi=? AND txn_lo=? AND mall_due=1 AND next_attempt_ms=?",
		lease, now, rec.Channel, rec.TxnHi, rec.TxnLo, rec.NextAttemptMs))
}

// MallSync 是一次推进商城之后要写回的结论。
type MallSync struct {
	State        uint32 // PayMallState
	RefundReason string
	Revision     uint32 // 商城已确认到的修订号
	KeepSyncing  bool   // 为假(拒收)时不再推进,哪怕修订号没同步完
	LastError    string
}

// SettleMall 写回推进结论。修订号在租约期间又前进了,这一行继续待同步、立即到点。只改自己领到的那一次。
func (r *Repo) SettleMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease uint64, s MallSync, now uint64) error {
	keep := 0
	if s.KeepSyncing {
		keep = 1
	}
	_, err := r.db.ExecContext(ctx, "UPDATE payment_txn SET "+
		"mall_due=CASE WHEN ?=1 AND fact_revision>? THEN 1 ELSE 0 END,"+
		"mall_state=?,mall_refund_reason=?,mall_revision=?,mall_last_error=?,next_attempt_ms=?,updated_ms=? "+
		"WHERE channel=? AND txn_hi=? AND txn_lo=? AND mall_due=1 AND next_attempt_ms=?",
		keep, s.Revision, s.State, s.RefundReason, s.Revision, s.LastError, now, now, rec.Channel, rec.TxnHi, rec.TxnLo, lease)
	return err
}

// DeferMall 这次没推进成,排下一次。只改自己领到的那一次。
func (r *Repo) DeferMall(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, next uint64, lastErr string, now uint64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE payment_txn SET next_attempt_ms=?,mall_last_error=?,updated_ms=? WHERE channel=? AND txn_hi=? AND txn_lo=? AND mall_due=1 AND next_attempt_ms=?",
		next, lastErr, now, rec.Channel, rec.TxnHi, rec.TxnLo, lease)
	return err
}

// ClaimRefund 领一次退款处理:refund_next_ms 仍是读到的值才推到租约到期时刻。已回执商城、已判失败的不再领。
func (r *Repo) ClaimRefund(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, now uint64) (bool, error) {
	return changed(r.db.ExecContext(ctx, "UPDATE payment_txn SET refund_next_ms=?,refund_attempts=refund_attempts+1,updated_ms=? WHERE channel=? AND txn_hi=? AND txn_lo=? AND refund_state IN (?,?,?) AND refund_next_ms=?",
		lease, now, rec.Channel, rec.TxnHi, rec.TxnLo, RefundNone, RefundRequested, RefundSucceeded, rec.RefundNextMs))
}

// RefundProgress 是一次退款处理之后要写回的全部字段。
type RefundProgress struct {
	State       uint32
	OutRefundNo string
	RefundID    string
	RefundMs    uint64
	NextMs      uint64
	LastError   string
}

// SaveRefund 写回退款进度。只改自己领到的那一次(refund_next_ms 仍是自己的租约)。
func (r *Repo) SaveRefund(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease uint64, p RefundProgress, now uint64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE payment_txn SET refund_state=?,out_refund_no=?,refund_id=?,refund_ms=?,refund_next_ms=?,refund_last_error=?,updated_ms=? WHERE channel=? AND txn_hi=? AND txn_lo=? AND refund_next_ms=?",
		p.State, p.OutRefundNo, p.RefundID, p.RefundMs, p.NextMs, p.LastError, now, rec.Channel, rec.TxnHi, rec.TxnLo, lease)
	return err
}

// DueFinalize 列出到点该做渠道收尾的付款。走 (finalize_state, finalize_next_ms) 索引。
func (r *Repo) DueFinalize(ctx context.Context, now uint64, limit int) ([]*paymentpb.PaymentTxnRecord, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+txnColumns+" FROM payment_txn WHERE finalize_state=? AND finalize_next_ms<=? ORDER BY finalize_next_ms LIMIT ?",
		FinalizePending, now, limit)
	return scanAll(rows, err, scanTxn)
}

// ClaimFinalize 领一次渠道收尾。
func (r *Repo) ClaimFinalize(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease, now uint64) (bool, error) {
	return changed(r.db.ExecContext(ctx, "UPDATE payment_txn SET finalize_next_ms=?,updated_ms=? WHERE channel=? AND txn_hi=? AND txn_lo=? AND finalize_state=? AND finalize_next_ms=?",
		lease, now, rec.Channel, rec.TxnHi, rec.TxnLo, FinalizePending, rec.FinalizeNextMs))
}

// SaveFinalize 写回收尾结论。只改自己领到的那一次。
func (r *Repo) SaveFinalize(ctx context.Context, rec *paymentpb.PaymentTxnRecord, lease uint64, state uint32, next, now uint64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE payment_txn SET finalize_state=?,finalize_next_ms=?,updated_ms=? WHERE channel=? AND txn_hi=? AND txn_lo=? AND finalize_next_ms=?",
		state, next, now, rec.Channel, rec.TxnHi, rec.TxnLo, lease)
	return err
}

// ---------------------------------------------------------------------------
// payment_channel_cursor
// ---------------------------------------------------------------------------

// ClaimCursor 领一个渠道的补漏游标租约,领到返回当前游标。行不存在时先按 initialMs 建一行(并发建行只有一个成功)。
func (r *Repo) ClaimCursor(ctx context.Context, channel uint32, owner, initialMs, lease, now uint64) (uint64, bool, error) {
	if _, err := r.db.ExecContext(ctx, "INSERT INTO payment_channel_cursor (channel,cursor_ms,lease_until_ms,lease_owner,updated_ms) VALUES (?,?,0,0,?)",
		channel, initialMs, now); err != nil && !duplicateKey(err) {
		return 0, false, err
	}
	claimed, err := changed(r.db.ExecContext(ctx, "UPDATE payment_channel_cursor SET lease_until_ms=?,lease_owner=?,updated_ms=? WHERE channel=? AND lease_until_ms<?",
		lease, owner, now, channel, now))
	if err != nil || !claimed {
		return 0, false, err
	}
	var cursor uint64
	if err := r.db.QueryRowContext(ctx, "SELECT cursor_ms FROM payment_channel_cursor WHERE channel=? AND lease_owner=?", channel, owner).Scan(&cursor); err != nil {
		return 0, false, err
	}
	return cursor, true, nil
}

// AdvanceCursor 把游标推到 cursorMs 并交还租约。只有持租约的副本改得动;游标只进不退。
func (r *Repo) AdvanceCursor(ctx context.Context, channel uint32, owner, cursorMs, now uint64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE payment_channel_cursor SET cursor_ms=CASE WHEN cursor_ms<? THEN ? ELSE cursor_ms END,lease_until_ms=0,updated_ms=? WHERE channel=? AND lease_owner=?",
		cursorMs, cursorMs, now, channel, owner)
	return err
}
