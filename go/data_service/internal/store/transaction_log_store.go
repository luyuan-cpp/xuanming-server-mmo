package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	dbpb "proto/common/database"

	"github.com/zeromicro/go-zero/core/logx"
)

// TransactionLogRow matches the schema consumed from Kafka and persisted in MySQL.
type TransactionLogRow struct {
	TxID          uint64
	Timestamp     uint64 // column timestamp_sec (Unix seconds)
	TxType        uint32
	FromPlayer    uint64
	ToPlayer      uint64
	ItemUUID      uint64
	ItemConfigID  uint32
	ItemQuantity  uint32
	CurrencyType  uint32
	CurrencyDelta int64
	BalanceBefore uint64
	BalanceAfter  uint64
	CorrelationID uint64
	Extra         string
	// ZoneID 是捕获时刻所在 zone(TransactionLogEntry.zone_id)。只在表 proto 声明了
	// zone_id 列时写入,见 txLogHasZoneColumn。
	ZoneID uint32
}

// TransactionLogQuery holds filter criteria for QueryTransactionLog.
type TransactionLogQuery struct {
	PlayerID     uint64   // 0 = all players
	TimeStart    uint64   // 0 = no lower bound
	TimeEnd      uint64   // 0 = no upper bound
	TxTypes      []uint32 // empty = all types
	ItemConfigID uint32   // 0 = no filter
	CurrencyType uint32   // 0 = no filter
	// ExactCurrency:只要这个币种的货币流水,CurrencyType 为 0 也按银两过滤。币种 0 就是银两,上面的 0 = 不过滤表达不了它。
	ExactCurrency bool
	// CorrelationIDs 非空时只要这些指令号的流水(correlation_id IN ...);空 = 不过滤。
	// GM 回收用它取一条指令不限时间窗的全部流水:同一条指令重投再应用时,第二条流水可能落在时间窗外。
	CorrelationIDs []uint64
	Limit          uint32 // default 100
	Offset         uint64
}

// txLogHasZoneColumn:表结构真源是 proto(rollback_database_table.proto 的 transaction_log),
// 而 Kafka 载荷 TransactionLogEntry 已经带 zone_id。表消息一旦补上 zone_id 字段,
// INSERT 自动多写这一列;还没补上时只能丢掉(启动时打一条 Error 提醒),不能靠手写 DDL
// 加列 —— 那正是本次收口要消灭的漂移源。
var txLogHasZoneColumn = (&dbpb.TransactionLog{}).ProtoReflect().Descriptor().Fields().ByName("zone_id") != nil

// txLogInsertColumns 是 InsertBatchIgnore 的列序;与那里的实参一一对应。
// shard 恒在最后(AuditShard(tx_id),audit_shard.go):表 proto 2026-11-06 起有这一列,缺列的库插入即报错,不静默丢。
var txLogInsertColumns = func() []string {
	cols := []string{
		"tx_id", "timestamp_sec", "tx_type", "from_player", "to_player",
		"item_uuid", "item_config_id", "item_quantity",
		"currency_type", "currency_delta", "balance_before", "balance_after",
		"correlation_id", "extra",
	}
	if txLogHasZoneColumn {
		cols = append(cols, "zone_id")
	}
	return append(cols, "shard")
}()

// TransactionLogStore provides query access to the transaction_log table.
type TransactionLogStore struct {
	db *sql.DB
}

// NewTransactionLogStore opens the pool. It does NOT create tables — see schema.go.
func NewTransactionLogStore(cfg MySQLConfig) (*TransactionLogStore, error) {
	db, err := openMySQL(cfg)
	if err != nil {
		return nil, err
	}
	if !txLogHasZoneColumn {
		logx.Errorf("[TransactionLogStore] table proto transaction_log has no zone_id column; " +
			"TransactionLogEntry.zone_id from Kafka will be dropped until the table proto gains the field")
	}
	logx.Infof("[TransactionLogStore] connected to %s/%s", cfg.Host, cfg.DBName)
	return &TransactionLogStore{db: db}, nil
}

// DeleteOldLogs 删一批早于 olderThan(Unix 秒)的流水,回实际删除行数。由 RunAuditRetention 循环调用,走 timestamp_sec 索引。
func (s *TransactionLogStore) DeleteOldLogs(ctx context.Context, olderThan uint64, limit int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM transaction_log WHERE timestamp_sec < ? LIMIT ?`, olderThan, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Close releases the database connection.
func (s *TransactionLogStore) Close() error {
	return s.db.Close()
}

// InsertBatchIgnore 一条多行 `INSERT IGNORE` 落一批流水,返回真正插入的行数。
// tx_id 是主键,重放同一批(消费者提交 offset 前崩溃)只会被 IGNORE,天然幂等。
// 200 行 × 16 列 = 3200 个占位符,远低于 MySQL 65535 的上限,不分块。
//
// 行按 tx_id 升序拼(txLogRowsInLockOrder),不按调用方的到达序。两条语句碰到同一组行时(rebalance 重叠:旧属主手里
// 没落的半批被新属主从已提交 offset 重读;2026-11-06 起同一实例的几个分区也会同时落库),到达序不同就会各自先插了
// 对方后面要碰的那一行,主键查重对未提交记录请求 S,REC_NOT_GAP 互等 → 1213(RC 下同样成立,与间隙锁无关)。
// 2026-10-16 真库实测(MySQL 26.7.0,两条 200 行语句同时放行):到达序全重叠 4/10、半重叠 3/10 成环,
// 两边都按 tx_id 升序 0/10。tx_id 不是自增列、流水没有插入序语义,排序不改变任何结果。
func (s *TransactionLogStore) InsertBatchIgnore(ctx context.Context, rows []*TransactionLogRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	rows = txLogRowsInLockOrder(rows)
	colCount := len(txLogInsertColumns)
	placeholder := "(" + strings.TrimSuffix(strings.Repeat("?,", colCount), ",") + ")"

	var sb strings.Builder
	sb.WriteString("INSERT IGNORE INTO transaction_log (")
	sb.WriteString(strings.Join(txLogInsertColumns, ", "))
	sb.WriteString(") VALUES ")
	args := make([]interface{}, 0, len(rows)*colCount)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(placeholder)
		args = append(args,
			r.TxID, r.Timestamp, r.TxType, r.FromPlayer, r.ToPlayer,
			r.ItemUUID, r.ItemConfigID, r.ItemQuantity,
			r.CurrencyType, r.CurrencyDelta, r.BalanceBefore, r.BalanceAfter,
			r.CorrelationID, r.Extra,
		)
		if txLogHasZoneColumn {
			args = append(args, r.ZoneID)
		}
		args = append(args, AuditShard(r.TxID))
	}

	res, err := s.db.ExecContext(ctx, sb.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("insert ignore transaction_log (%d rows): %w", len(rows), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("insert ignore transaction_log rows affected: %w", err)
	}
	return n, nil
}

// txLogRowsInLockOrder 返回按 tx_id 升序稳定排序的**副本**,即多行 INSERT 在主键上的取锁顺序。
// 不就地排序:调用方(消费者)手里的批还要按原样参与 offset 提交与日志。
func txLogRowsInLockOrder(rows []*TransactionLogRow) []*TransactionLogRow {
	sorted := slices.Clone(rows)
	slices.SortStableFunc(sorted, func(a, b *TransactionLogRow) int { return cmp.Compare(a.TxID, b.TxID) })
	return sorted
}

// QueryLog retrieves transaction log entries matching the filter criteria.
func (s *TransactionLogStore) QueryLog(ctx context.Context, q *TransactionLogQuery) ([]*TransactionLogRow, uint32, error) {
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit > 10000 {
		q.Limit = 10000
	}

	var conditions []string
	var args []interface{}

	if q.PlayerID > 0 {
		// 按玩家走 (from_player, timestamp_sec) / (to_player, timestamp_sec):玩家号打散,不需要分桶,也不带分桶谓词,
		// 免得优化器改走按段扫的时间索引。
		conditions = append(conditions, "(from_player = ? OR to_player = ?)")
		args = append(args, q.PlayerID, q.PlayerID)
	} else {
		// 没有玩家条件时按时间 / tx_type / item_config_id 扫:这些索引在时间列前有 shard(audit_shard.go)。
		conditions = append(conditions, auditShardInSQL)
	}
	if q.TimeStart > 0 {
		conditions = append(conditions, "timestamp_sec >= ?")
		args = append(args, q.TimeStart)
	}
	if q.TimeEnd > 0 {
		conditions = append(conditions, "timestamp_sec <= ?")
		args = append(args, q.TimeEnd)
	}
	if len(q.TxTypes) > 0 {
		placeholders := make([]string, len(q.TxTypes))
		for i, t := range q.TxTypes {
			placeholders[i] = "?"
			args = append(args, t)
		}
		conditions = append(conditions, "tx_type IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(q.CorrelationIDs) > 0 {
		placeholders := make([]string, len(q.CorrelationIDs))
		for i, id := range q.CorrelationIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		conditions = append(conditions, "correlation_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if q.ItemConfigID > 0 {
		conditions = append(conditions, "item_config_id = ?")
		args = append(args, q.ItemConfigID)
	}
	if q.ExactCurrency {
		conditions = append(conditions, "item_config_id = 0", "currency_type = ?")
		args = append(args, q.CurrencyType)
	} else if q.CurrencyType > 0 {
		conditions = append(conditions, "currency_type = ?")
		args = append(args, q.CurrencyType)
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	// Count total matches
	countSQL := "SELECT COUNT(*) FROM transaction_log" + where
	var totalCount uint32
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&totalCount); err != nil {
		return nil, 0, fmt.Errorf("count transaction_log: %w", err)
	}

	// Fetch rows
	querySQL := `SELECT tx_id, timestamp_sec, tx_type, from_player, to_player,
	              item_uuid, item_config_id, item_quantity,
	              currency_type, currency_delta, balance_before, balance_after,
	              correlation_id, extra
	             FROM transaction_log` + where +
		` ORDER BY timestamp_sec DESC LIMIT ? OFFSET ?`

	queryArgs := append(args, q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, querySQL, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query transaction_log: %w", err)
	}
	defer rows.Close()

	var result []*TransactionLogRow
	for rows.Next() {
		r := &TransactionLogRow{}
		if err := rows.Scan(
			&r.TxID, &r.Timestamp, &r.TxType, &r.FromPlayer, &r.ToPlayer,
			&r.ItemUUID, &r.ItemConfigID, &r.ItemQuantity,
			&r.CurrencyType, &r.CurrencyDelta, &r.BalanceBefore, &r.BalanceAfter,
			&r.CorrelationID, &r.Extra,
		); err != nil {
			return nil, 0, fmt.Errorf("scan transaction_log: %w", err)
		}
		result = append(result, r)
	}
	return result, totalCount, rows.Err()
}

// QueryByItemUUID retrieves all transaction log entries for a specific item UUID.
// Used for tracing item pollution chains (A→B→C).
func (s *TransactionLogStore) QueryByItemUUID(ctx context.Context, itemUUID uint64) ([]*TransactionLogRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tx_id, timestamp_sec, tx_type, from_player, to_player,
		        item_uuid, item_config_id, item_quantity,
		        currency_type, currency_delta, balance_before, balance_after,
		        correlation_id, extra
		 FROM transaction_log WHERE item_uuid = ?
		 ORDER BY timestamp_sec ASC`, itemUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*TransactionLogRow
	for rows.Next() {
		r := &TransactionLogRow{}
		if err := rows.Scan(
			&r.TxID, &r.Timestamp, &r.TxType, &r.FromPlayer, &r.ToPlayer,
			&r.ItemUUID, &r.ItemConfigID, &r.ItemQuantity,
			&r.CurrencyType, &r.CurrencyDelta, &r.BalanceBefore, &r.BalanceAfter,
			&r.CorrelationID, &r.Extra,
		); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
