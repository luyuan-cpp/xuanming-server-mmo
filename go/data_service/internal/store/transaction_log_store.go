package store

import (
	"context"
	"database/sql"
	"fmt"
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
	Limit        uint32   // default 100
	Offset       uint64
}

// txLogHasZoneColumn:表结构真源是 proto(rollback_database_table.proto 的 transaction_log),
// 而 Kafka 载荷 TransactionLogEntry 已经带 zone_id。表消息一旦补上 zone_id 字段,
// INSERT 自动多写这一列;还没补上时只能丢掉(启动时打一条 Error 提醒),不能靠手写 DDL
// 加列 —— 那正是本次收口要消灭的漂移源。
var txLogHasZoneColumn = (&dbpb.TransactionLog{}).ProtoReflect().Descriptor().Fields().ByName("zone_id") != nil

// txLogInsertColumns 是 InsertBatchIgnore 的列序;与 rowValues 一一对应。
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
	return cols
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

// Close releases the database connection.
func (s *TransactionLogStore) Close() error {
	return s.db.Close()
}

// InsertBatchIgnore 一条多行 `INSERT IGNORE` 落一批流水,返回真正插入的行数。
// tx_id 是主键,重放同一批(消费者提交 offset 前崩溃)只会被 IGNORE,天然幂等。
// 200 行 × 15 列 = 3000 个占位符,远低于 MySQL 65535 的上限,不分块。
func (s *TransactionLogStore) InsertBatchIgnore(ctx context.Context, rows []*TransactionLogRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
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
		conditions = append(conditions, "(from_player = ? OR to_player = ?)")
		args = append(args, q.PlayerID, q.PlayerID)
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
	if q.ItemConfigID > 0 {
		conditions = append(conditions, "item_config_id = ?")
		args = append(args, q.ItemConfigID)
	}
	if q.CurrencyType > 0 {
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
