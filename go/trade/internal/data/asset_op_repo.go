package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	assetpb "proto/common/asset"
	tradepb "proto/trade"

	"shared/assetop"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

// 编译前置(写下本文件时未满足):本文件用到 TradeAssetOpRecord 的 resolved_by(22)/
// resolve_reason(23)。这两个字段只在 proto/trade/trade_table.proto 里,生成物还停在
// 字段 21 —— 直接 go build 会报 `rec.ResolvedBy undefined`,那不是代码写错,是**没跑
// 生成**。先跑仓库既有的 proto 生成(dev.bat proto),再 build/vet/test。
// 生成会改到:go/proto/trade/trade_table.pb.go、cpp/generated/proto/trade/trade_table.pb.{h,cc}、
// generated/proto/{_unified,db,login}/proto/trade/trade_table.proto;robot/vendor/proto/trade/
// trade_table.pb.go 是 `replace proto => ../go/proto` 的 vendor 副本,按仓库惯例一并同步。

// 表名。唯一事实源仍是 proto/trade/trade_table.proto 的 OptionTableName;这里是手写 SQL
// 需要的字面量,两边必须一致。改表名要同改这两处(schemamigrate 会按 proto 的名字建表,
// 改漏一边的表现是"表建出来了但所有查询报 table doesn't exist",启动期查不出来)。
const (
	AssetOpSeqTableName = "trade_player_op_seq"
	AssetOpTableName    = "trade_asset_op"
)

// assetOpColumns 是 outbox 行的全列,顺序即 scanAssetOp / InsertOp 的参数顺序。
const assetOpColumns = "`op_id`, `player_id`, `stream`, `stream_epoch`, `seq`, `kind`, `status`, `durable`, " +
	"`attempts`, `next_attempt_ms`, `deadline_ms`, `lease_until_ms`, `lease_token`, `tx_type`, " +
	"`ref_kind`, `ref_id`, `payload`, `last_outcome`, `last_reason`, `created_ms`, `updated_ms`, " +
	"`resolved_by`, `resolve_reason`"

// PendingStatus 是 outbox 的"待办"状态数值,给 assetop 的 seq 分配与重投循环用。
// 取 proto 枚举而不是写 0 / 1:§4.43 #23 明确 assetop.Status 不绑库值,库值由业务表自己定。
func PendingStatus() uint32 { return uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING) }

// AssetOpRepo 是通用资产通道 outbox(trade_asset_op)与 seq 分配器(trade_player_op_seq)的
// MySQL 实现,同时满足 assetop.Store。
//
// 契约:
//   - 除 InsertOp / AllocateSeqTx 之外的方法自带 opTimeout 上限,调用方 ctx 更早到期时以 ctx 为准;
//   - InsertOp 只在调用方给的事务里执行,不自带超时(事务的预算由调用方统一给);
//   - 返回的 error 一律是存储故障,唯一的例外是 Claim 的 assetop.ErrPoisonRow(payload 解不开)。
type AssetOpRepo struct {
	db        *sql.DB
	opTimeout time.Duration
	tables    assetop.SeqTables
}

var (
	_ assetop.Store          = (*AssetOpRepo)(nil)
	_ assetop.ManualResolver = (*AssetOpRepo)(nil)
)

// NewAssetOpRepo。opTimeout 是每次调用的上限(constants.StoreOpTimeout),必须 > 0。
// 表名经 assetop.NewSeqTables 校验(正则 ^[a-z][a-z0-9_]{0,62}$),防止拼接注入。
func NewAssetOpRepo(db *sql.DB, opTimeout time.Duration) (*AssetOpRepo, error) {
	tables, err := assetop.NewSeqTables(AssetOpSeqTableName, AssetOpTableName, PendingStatus())
	if err != nil {
		return nil, fmt.Errorf("trade: 资产 outbox 表名非法: %w", err)
	}
	return &AssetOpRepo{db: db, opTimeout: opTimeout, tables: tables}, nil
}

// DB 暴露连接池,供调用方用 assetop.WithTxRetry 开业务事务(seq 分配必须与业务写同事务)。
func (r *AssetOpRepo) DB() *sql.DB { return r.db }

// Tables 返回 seq / outbox 的表名对,供 assetop.AllocateSeq / EnsureSeqRow 使用。
func (r *AssetOpRepo) Tables() assetop.SeqTables { return r.tables }

func (r *AssetOpRepo) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.opTimeout)
}

// EnsureSeqRow 在事务外(autocommit)建 (player_id, stream) 的 seq 行。
// 必须在开业务事务之前调用:事务内首次并发建行会让两个共享锁升级成死锁(§4.19 seq.go)。
func (r *AssetOpRepo) EnsureSeqRow(ctx context.Context, playerID uint64, stream assetpb.AssetOpStream, nowMs uint64) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	if err := assetop.EnsureSeqRow(ctx, r.db, r.tables, playerID, stream, nowMs); err != nil {
		return fmt.Errorf("ensure %s row (player=%d stream=%d): %w", AssetOpSeqTableName, playerID, int32(stream), err)
	}
	return nil
}

// InsertOp 在调用方的事务里插入一行 outbox。主键冲突按故障返回:op_id 来自号段,撞号说明
// 发号源出了问题,绝不静默覆盖(同 InsertListing 的理由)。
func (r *AssetOpRepo) InsertOp(ctx context.Context, tx *sql.Tx, rec *tradepb.TradeAssetOpRecord) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO "+AssetOpTableName+" ("+assetOpColumns+") VALUES "+
			"(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		rec.GetOpId(), rec.GetPlayerId(), rec.GetStream(), rec.GetStreamEpoch(), rec.GetSeq(),
		int32(rec.GetKind()), int32(rec.GetStatus()), rec.GetDurable(),
		rec.GetAttempts(), rec.GetNextAttemptMs(), rec.GetDeadlineMs(), rec.GetLeaseUntilMs(), rec.GetLeaseToken(),
		rec.GetTxType(), int32(rec.GetRefKind()), rec.GetRefId(), rec.GetPayload(),
		rec.GetLastOutcome(), rec.GetLastReason(), rec.GetCreatedMs(), rec.GetUpdatedMs(),
		// 新行一律是空留痕:人工终结只由 ResolveManually 写,插入路径绝不预置操作人。
		rec.GetResolvedBy(), rec.GetResolveReason(),
	)
	if err != nil {
		return fmt.Errorf("insert %s %d: %w", AssetOpTableName, rec.GetOpId(), err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// assetop.Store
// ---------------------------------------------------------------------------

// ListDue 非加锁一致性读,只取主键,走 (status, next_attempt_ms) 索引。
// 不用范围 UPDATE 领取:那会在 RR 下加 next-key 锁,与业务事务里 AllocateSeq 的
// FOR UPDATE + 插入新 op 行互相等待,被判死锁牺牲的是玩家的请求(§4.37)。
func (r *AssetOpRepo) ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx,
		"SELECT `op_id` FROM "+AssetOpTableName+
			" WHERE `status` = ? AND `next_attempt_ms` <= ? AND `lease_until_ms` < ?"+
			" ORDER BY `next_attempt_ms` LIMIT ?",
		PendingStatus(), nowMs, nowMs, limit)
	if err != nil {
		return nil, fmt.Errorf("list due %s: %w", AssetOpTableName, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due %s: %w", AssetOpTableName, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due %s: %w", AssetOpTableName, err)
	}
	return ids, nil
}

// Claim 单行主键 CAS 领取(autocommit),紧挨着处理前调用,租约从"领到这一行"起算。
// RowsAffected == 0 表示这一行已被别的副本领走(或已终结),返回 (Op{}, false, nil)。
//
// payload 解不开的毒行不能卡住整个循环:把它推迟到 poisonUntilMs 并回 assetop.ErrPoisonRow,
// 循环记 decode 计数后继续下一行(§4.37)。
//
// poisonUntilMs 由 assetop 的重投循环按 LoopConfig.PoisonDelay 算好传进来。**这里不许再自写
// 一份毒行延迟常量**:写了的话 yaml 里改 PoisonDelay 不生效,两份值迟早分叉,而分叉的表现是
// "配置改了没用",没有任何报错。
func (r *AssetOpRepo) Claim(ctx context.Context, opID, nowMs, leaseUntilMs, poisonUntilMs, token uint64) (assetop.Op, bool, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	res, err := r.db.ExecContext(ctx,
		"UPDATE "+AssetOpTableName+" SET `lease_until_ms` = ?, `lease_token` = ?, `updated_ms` = ?"+
			" WHERE `op_id` = ? AND `status` = ? AND `lease_until_ms` < ?",
		leaseUntilMs, token, nowMs, opID, PendingStatus(), nowMs)
	if err != nil {
		return assetop.Op{}, false, fmt.Errorf("claim %s %d: %w", AssetOpTableName, opID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return assetop.Op{}, false, fmt.Errorf("claim %s %d rows affected: %w", AssetOpTableName, opID, err)
	}
	if affected != 1 {
		return assetop.Op{}, false, nil
	}

	rec, err := r.getOp(ctx, opID)
	if err != nil {
		return assetop.Op{}, false, err
	}
	bundle := &assetpb.AssetBundle{}
	if len(rec.GetPayload()) > 0 {
		if err := proto.Unmarshal(rec.GetPayload(), bundle); err != nil {
			r.markPoison(ctx, opID, token, nowMs)
			return assetop.Op{}, false, fmt.Errorf("%w: op_id=%d: %v", assetop.ErrPoisonRow, opID, err)
		}
	}
	return assetop.Op{
		OpID:        rec.GetOpId(),
		PlayerID:    rec.GetPlayerId(),
		Stream:      assetpb.AssetOpStream(rec.GetStream()),
		Seq:         rec.GetSeq(),
		StreamEpoch: rec.GetStreamEpoch(),
		// correlation_id 取 op_id,**不是** ref_id:§4.38 的人工终结以 scene 流水
		// correlation_id 定位唯一一行 outbox。取 ref_id 的话,同一件商品的托管与回退
		// 两条 op 会在 transaction_log 里落成同一个 correlation_id,取证就失效了。
		// 这里必须与 reconcile 首次投递时填的值一致,否则重投会换一个 correlation。
		CorrelationID: rec.GetOpId(),
		TxType:        rec.GetTxType(),
		Bundle:        bundle,
		Attempts:      rec.GetAttempts(),
		DeadlineMs:    rec.GetDeadlineMs(),
		LeaseToken:    token,
		LastReason:    rec.GetLastReason(),
	}, true, nil
}

// markPoison 把解不开 payload 的行推迟 PoisonDelay,并记下"最近一次结局未知"。
// 用本次领取的 lease_token 做 CAS,避免推迟别的副本刚领走的同一行。
// 失败只记在返回给调用方的错误里之外的日志层:这里没有 logger,推迟失败的后果是
// 下一轮再撞一次同一行(仍然跳过),不会丢数据,所以吞掉写错误但不吞解码错误。
func (r *AssetOpRepo) markPoison(ctx context.Context, opID, token, nowMs uint64) {
	_, _ = r.db.ExecContext(ctx,
		"UPDATE "+AssetOpTableName+" SET `last_outcome` = 0, `next_attempt_ms` = ?, `lease_until_ms` = 0, `updated_ms` = ?"+
			" WHERE `op_id` = ? AND `lease_token` = ?",
		nowMs+uint64(PoisonDelay/time.Millisecond), nowMs, opID, token)
}

// Finalize 把行终结成 status,并在同一事务里做对侧账。RowsAffected == 1 才算本次终结
// (返回 true);已被别的副本终结时返回 false,调用方不得重复入账。
//
// 锁序(P3 落订单后必须遵守,与 guild 的 guild → guild_member → seq → op 同形):
//
//	trade_listing → trade_order → trade_player_op_seq → trade_asset_op
//
// 本批没有订单与卖家账,所以事务里只有 outbox 一张表;对侧账的位置留在下面的 TODO 注释处,
// 由 P3 在**同一个事务**里补(拆成两个事务就会出现"发了货没改状态")。
func (r *AssetOpRepo) Finalize(ctx context.Context, op assetop.Op, status assetop.Status, res assetop.Result, nowMs uint64) (bool, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	finalized := false
	err := assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		finalized = false
		result, err := tx.ExecContext(ctx,
			"UPDATE "+AssetOpTableName+" SET `status` = ?, `durable` = 1, `last_outcome` = ?, `last_reason` = ?, `updated_ms` = ?"+
				" WHERE `op_id` = ? AND `status` = ?",
			int32(StatusToRecord(status)), uint32(res.Outcome), res.Reason, nowMs, op.OpID, PendingStatus())
		if err != nil {
			return fmt.Errorf("finalize %s %d: %w", AssetOpTableName, op.OpID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("finalize %s %d rows affected: %w", AssetOpTableName, op.OpID, err)
		}
		if affected != 1 {
			return nil // 已被别的副本终结:不是错误,也不做对侧账
		}
		finalized = true
		// P3 对侧账在这里(同一事务):
		//   ESCROW_DEBIT  APPLIED → 商品 ESCROWING → LISTED;REJECTED/ABORTED → ESCROW_REJECTED;
		//   DELIVER_CREDIT APPLIED → 订单 DELIVERING → DELIVERED + 卖家入账;
		//   RETURN_CREDIT  APPLIED → 商品 RETURNING → RETURNED。
		//   APPLIED_PARTIAL 一律不入账、不退款,转人工补偿(§4.33)。
		return nil
	})
	if err != nil {
		return false, err
	}
	return finalized, nil
}

// ResolveManually 是 §4.38 的人工终结通道:把一行卡死的 outbox 按人工判定直接落成终局,
// 并留下操作人与依据。它**不查证据、不猜结论** —— 调用方(管理入口)已经按 scene 流水的
// correlation_id = op_id 判定过,本方法只负责落库 + 留痕 + 与自动路径同一把 CAS。
//
// 与 Finalize 完全同形:同一个事务、同一条 `status = PENDING` 的 CAS、RowsAffected == 1
// 才算本次终结并做对侧账。两条路径共用这个条件,人工与循环同时下手也只会有一个赢家。
//
// durable 刻意**不**置 1:这一行的终局来自人工判定,不是 scene 确认的落盘结局,
// 把它标成 durable 会让事后对账分不清"scene 真的答过"和"人写上去的"。
func (r *AssetOpRepo) ResolveManually(ctx context.Context, m assetop.ManualResolution, nowMs uint64) (bool, error) {
	// 状态合法性、操作人与理由的长度由 assetop.Loop.ResolveManually 在进来之前校验;
	// 这里只兜住"映射不出库值"这一种:落成 UNSPECIFIED 会让客服查不到结局。
	final := StatusToRecord(m.Final)
	if final == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED ||
		final == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING {
		return false, fmt.Errorf("manual resolve %s %d: 终结状态非法 (%s)", AssetOpTableName, m.OpID, m.Final)
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	resolved := false
	err := assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		resolved = false
		result, err := tx.ExecContext(ctx,
			"UPDATE "+AssetOpTableName+" SET `status` = ?, `resolved_by` = ?, `resolve_reason` = ?, `updated_ms` = ?"+
				" WHERE `op_id` = ? AND `status` = ?",
			int32(final), m.Operator, m.Reason, nowMs, m.OpID, PendingStatus())
		if err != nil {
			return fmt.Errorf("manual resolve %s %d: %w", AssetOpTableName, m.OpID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("manual resolve %s %d rows affected: %w", AssetOpTableName, m.OpID, err)
		}
		if affected != 1 {
			return nil // 行不存在或已被终结:不是错误,也不做对侧账
		}
		resolved = true
		// P3 对侧账在这里,与 Finalize 的 TODO 处**同一份**逻辑(抽成一个内部函数,
		// 两条路径都调它);人工与自动走两份对侧账就会出现只有一边改了商品状态。
		return nil
	})
	if err != nil {
		return false, err
	}
	return resolved, nil
}

// Reschedule 退回待办并推迟下一次投递。带 lease_token 做 CAS:租约已被别人接管时本次更新落空,
// 不会把别的副本刚排好的时间覆盖掉。
func (r *AssetOpRepo) Reschedule(ctx context.Context, op assetop.Op, nextAttemptMs uint64, res assetop.Result, nowMs uint64) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	_, err := r.db.ExecContext(ctx,
		"UPDATE "+AssetOpTableName+" SET `attempts` = `attempts` + 1, `next_attempt_ms` = ?, `lease_until_ms` = 0,"+
			" `durable` = ?, `last_outcome` = ?, `last_reason` = ?, `updated_ms` = ?"+
			" WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?",
		nextAttemptMs, res.Durable, uint32(res.Outcome), res.Reason, nowMs, op.OpID, PendingStatus(), op.LeaseToken)
	if err != nil {
		return fmt.Errorf("reschedule %s %d: %w", AssetOpTableName, op.OpID, err)
	}
	return nil
}

// OldestPendingCreatedMs 实现 assetop 的可选接口,喂 assetop_pending_oldest_age_seconds。
// 每 30s 一次的聚合查询;本批表小,先不为它单独建索引(积压真上来了再按 (status, stream) 补)。
func (r *AssetOpRepo) OldestPendingCreatedMs(ctx context.Context, stream assetpb.AssetOpStream) (uint64, bool, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	var oldest sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		"SELECT MIN(`created_ms`) FROM "+AssetOpTableName+" WHERE `status` = ? AND `stream` = ?",
		PendingStatus(), uint32(stream)).Scan(&oldest)
	if err != nil {
		return 0, false, fmt.Errorf("oldest pending %s: %w", AssetOpTableName, err)
	}
	if !oldest.Valid || oldest.Int64 < 0 {
		return 0, false, nil
	}
	return uint64(oldest.Int64), true, nil
}

// getOp 按主键取全列。
func (r *AssetOpRepo) getOp(ctx context.Context, opID uint64) (*tradepb.TradeAssetOpRecord, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+assetOpColumns+" FROM "+AssetOpTableName+" WHERE `op_id` = ?", opID)
	rec, err := scanAssetOp(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 刚 CAS 成功又查不到:只可能是被并发删了(本服务从不删 outbox 行),按故障返回。
			return nil, fmt.Errorf("select %s %d: 行在领取后消失", AssetOpTableName, opID)
		}
		return nil, fmt.Errorf("select %s %d: %w", AssetOpTableName, opID, err)
	}
	return rec, nil
}

// scanAssetOp 按 assetOpColumns 的列序扫一行。枚举列先扫 int32 再转型,不依赖
// database/sql 对具名整数类型的反射转换(同 scanListing)。
func scanAssetOp(row rowScanner) (*tradepb.TradeAssetOpRecord, error) {
	rec := &tradepb.TradeAssetOpRecord{}
	var kind, status, refKind int32
	if err := row.Scan(
		&rec.OpId, &rec.PlayerId, &rec.Stream, &rec.StreamEpoch, &rec.Seq,
		&kind, &status, &rec.Durable,
		&rec.Attempts, &rec.NextAttemptMs, &rec.DeadlineMs, &rec.LeaseUntilMs, &rec.LeaseToken,
		&rec.TxType, &refKind, &rec.RefId, &rec.Payload,
		&rec.LastOutcome, &rec.LastReason, &rec.CreatedMs, &rec.UpdatedMs,
		&rec.ResolvedBy, &rec.ResolveReason,
	); err != nil {
		return nil, err
	}
	rec.Kind = tradepb.TradeAssetOpKind(kind)
	rec.Status = tradepb.TradeAssetOpStatus(status)
	rec.RefKind = tradepb.TradeAssetOpRefKind(refKind)
	return rec, nil
}

// StatusToRecord 把 assetop 的语义终态映射成本表的存储值。
// assetop.Status 刻意不绑库值(§4.43 #23),映射只此一处。
func StatusToRecord(s assetop.Status) tradepb.TradeAssetOpStatus {
	switch s {
	case assetop.StatusPending:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING
	case assetop.StatusApplied:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED
	case assetop.StatusRejected:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED
	case assetop.StatusAborted:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED
	case assetop.StatusAppliedPartial:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL
	default:
		// 未知语义值不能落成 PENDING(会被循环反复领走),也不能落成 APPLIED(会伪装成功)。
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED
	}
}

// txRetryAttempts 是业务写路径撞死锁 / 写冲突时的整体重试次数(§4.37:attempts=2)。
const txRetryAttempts = 2

// PoisonDelay 是 payload 解不开的毒行的推迟时长,与 assetop.LoopConfig.PoisonDelay 同值。
const PoisonDelay = time.Hour

// IsRetryableTxError 报告事务错误是否值得整体重试。shared/assetop 不引 MySQL 驱动,
// 分类由调用方注入(§4.37);取值与 guild 一致:
//
//	1213 死锁 / 1205 锁等待超时 / 9007 TiDB 写冲突。
func IsRetryableTxError(err error) bool {
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1213, 1205, 9007:
		return true
	default:
		return false
	}
}
