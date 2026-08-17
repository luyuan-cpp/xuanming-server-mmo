package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// TransactionLogRow matches the schema consumed from Kafka and persisted in MySQL.
type TransactionLogRow struct {
	TxID          uint64
	Timestamp     uint64
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

// TransactionLogStore provides query access to the transaction_log table.
type TransactionLogStore struct {
	db *sql.DB
}

// NewTransactionLogStore creates a store sharing the same MySQL database.
func NewTransactionLogStore(cfg MySQLConfig) (*TransactionLogStore, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4",
		cfg.User, cfg.Password, cfg.Host, cfg.DBName)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	if cfg.MaxOpenConn > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConn)
	} else {
		db.SetMaxOpenConns(5)
	}
	if cfg.MaxIdleConn > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConn)
	} else {
		db.SetMaxIdleConns(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}

	s := &TransactionLogStore{db: db}
	if err := s.ensureTable(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure transaction_log table: %w", err)
	}

	logx.Infof("[TransactionLogStore] connected to %s/%s", cfg.Host, cfg.DBName)
	return s, nil
}

func (s *TransactionLogStore) ensureTable() error {
	// tx_id 是雪花 ID(时间戳高位、单调递增),TiDB 默认聚簇表下是写热点——
	// 交易日志是全服最高频的追加写路径之一,按全局数据层决策 §D3 处方
	// 用 /*T!*/ 双方言注释声明 NONCLUSTERED + 打散;MySQL 忽略注释,行为不变。
	ddl := `CREATE TABLE IF NOT EXISTS transaction_log (
		tx_id BIGINT UNSIGNED NOT NULL,
		timestamp_sec BIGINT UNSIGNED NOT NULL,
		tx_type INT UNSIGNED NOT NULL DEFAULT 0,
		from_player BIGINT UNSIGNED NOT NULL DEFAULT 0,
		to_player BIGINT UNSIGNED NOT NULL DEFAULT 0,
		item_uuid BIGINT UNSIGNED NOT NULL DEFAULT 0,
		item_config_id INT UNSIGNED NOT NULL DEFAULT 0,
		item_quantity INT UNSIGNED NOT NULL DEFAULT 0,
		currency_type INT UNSIGNED NOT NULL DEFAULT 0,
		currency_delta BIGINT NOT NULL DEFAULT 0,
		balance_before BIGINT UNSIGNED NOT NULL DEFAULT 0,
		balance_after BIGINT UNSIGNED NOT NULL DEFAULT 0,
		correlation_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
		extra VARCHAR(1024) NOT NULL DEFAULT '',
		PRIMARY KEY (tx_id) /*T![clustered_index] NONCLUSTERED */,
		INDEX idx_player_time (from_player, timestamp_sec),
		INDEX idx_to_player_time (to_player, timestamp_sec),
		INDEX idx_item_config (item_config_id, timestamp_sec),
		INDEX idx_tx_type_time (tx_type, timestamp_sec)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */`
	_, err := s.db.Exec(ddl)
	return err
}

// Close releases the database connection.
func (s *TransactionLogStore) Close() error {
	return s.db.Close()
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
