package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// SnapshotRow represents a row in the player_snapshot table.
type SnapshotRow struct {
	ID           uint64
	PlayerID     uint64
	ZoneID       uint32
	SnapshotType uint32
	CreatedAt    uint64
	Reason       string
	Operator     string
	Data         []byte // serialized SnapshotData proto
}

// AuditLogRow represents a row in the rollback_audit_log table.
type AuditLogRow struct {
	PlayerID              uint64
	ZoneID                uint32
	RollbackType          uint32 // 1=player, 2=zone, 3=server
	SnapshotIDUsed        uint64
	PreRollbackSnapshotID uint64
	TargetTime            uint64
	PlayersAffected       uint32
	PlayersFailed         uint32
	OrphansCleaned        uint32
	Reason                string
	Operator              string
	CreatedAt             uint64
}

// MySQLConfig holds MySQL connection settings for the snapshot store.
type MySQLConfig struct {
	Host        string
	User        string
	Password    string
	DBName      string
	MaxOpenConn int
	MaxIdleConn int
}

// SnapshotStore provides CRUD operations for player snapshots and audit logs.
type SnapshotStore struct {
	db *sql.DB
}

// NewSnapshotStore creates a new SnapshotStore and ensures tables exist.
func NewSnapshotStore(cfg MySQLConfig) (*SnapshotStore, error) {
	// sql_mode=%27STRICT_TRANS_TABLES%27 强制会话级严格模式(%27 是被 URL 转义的单引号,
	// 驱动会还原成 SET sql_mode = 'STRICT_TRANS_TABLES')。玩家快照是 LONGBLOB 里的序列化
	// 数据,非严格模式下超长写入会静默截断且无报错;这里在连接层兜底,与 go/db 同口径。
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&sql_mode=%%27STRICT_TRANS_TABLES%%27",
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

	s := &SnapshotStore{db: db}
	if err := s.ensureTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure tables: %w", err)
	}

	logx.Infof("[SnapshotStore] connected to %s/%s", cfg.Host, cfg.DBName)
	return s, nil
}

func (s *SnapshotStore) ensureTables() error {
	// 单调递增主键(自增)在 TiDB 默认聚簇表下是写热点,按全局数据层决策 §D3 处方
	// 用 /*T!*/ 双方言注释声明 NONCLUSTERED + SHARD_ROW_ID_BITS 打散;
	// MySQL 把注释当普通注释忽略,同一份 DDL 两种后端都能跑。
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS player_snapshot (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			player_id BIGINT UNSIGNED NOT NULL,
			zone_id INT UNSIGNED NOT NULL DEFAULT 0,
			snapshot_type INT UNSIGNED NOT NULL DEFAULT 0,
			created_at BIGINT UNSIGNED NOT NULL,
			reason VARCHAR(512) NOT NULL DEFAULT '',
			operator VARCHAR(128) NOT NULL DEFAULT '',
			data LONGBLOB,
			PRIMARY KEY (id) /*T![clustered_index] NONCLUSTERED */,
			INDEX idx_player_created (player_id, created_at),
			INDEX idx_zone_created (zone_id, created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */`,
		`CREATE TABLE IF NOT EXISTS rollback_audit_log (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			player_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
			zone_id INT UNSIGNED NOT NULL DEFAULT 0,
			rollback_type INT UNSIGNED NOT NULL DEFAULT 0,
			snapshot_id_used BIGINT UNSIGNED NOT NULL DEFAULT 0,
			pre_rollback_snapshot_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
			target_time BIGINT UNSIGNED NOT NULL DEFAULT 0,
			players_affected INT UNSIGNED NOT NULL DEFAULT 0,
			players_failed INT UNSIGNED NOT NULL DEFAULT 0,
			orphans_cleaned INT UNSIGNED NOT NULL DEFAULT 0,
			reason VARCHAR(512) NOT NULL DEFAULT '',
			operator VARCHAR(128) NOT NULL DEFAULT '',
			created_at BIGINT UNSIGNED NOT NULL,
			PRIMARY KEY (id) /*T![clustered_index] NONCLUSTERED */
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */`,
	}

	for _, stmt := range ddl {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the database connection.
func (s *SnapshotStore) Close() error {
	return s.db.Close()
}

// InsertSnapshot saves a snapshot and returns the auto-increment ID.
func (s *SnapshotStore) InsertSnapshot(ctx context.Context, row *SnapshotRow) (uint64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO player_snapshot (player_id, zone_id, snapshot_type, created_at, reason, operator, data)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		row.PlayerID, row.ZoneID, row.SnapshotType, row.CreatedAt, row.Reason, row.Operator, row.Data,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return uint64(id), nil
}

// GetSnapshotByID loads a snapshot by primary key.
func (s *SnapshotStore) GetSnapshotByID(ctx context.Context, id uint64) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, data
		 FROM player_snapshot WHERE id = ?`, id,
	).Scan(&row.ID, &row.PlayerID, &row.ZoneID, &row.SnapshotType, &row.CreatedAt,
		&row.Reason, &row.Operator, &row.Data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// GetLatestSnapshotBefore returns the most recent snapshot for a player before the given time.
func (s *SnapshotStore) GetLatestSnapshotBefore(ctx context.Context, playerID, beforeTime uint64) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, data
		 FROM player_snapshot
		 WHERE player_id = ? AND created_at <= ?
		 ORDER BY created_at DESC LIMIT 1`, playerID, beforeTime,
	).Scan(&row.ID, &row.PlayerID, &row.ZoneID, &row.SnapshotType, &row.CreatedAt,
		&row.Reason, &row.Operator, &row.Data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// ListSnapshots returns metadata (no data blob) for a player's snapshots.
func (s *SnapshotStore) ListSnapshots(ctx context.Context, playerID, beforeTime uint64, limit uint32) ([]*SnapshotRow, error) {
	if limit == 0 {
		limit = 20
	}

	var (
		rows *sql.Rows
		err  error
	)
	if beforeTime > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, LENGTH(data)
			 FROM player_snapshot
			 WHERE player_id = ? AND created_at <= ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, beforeTime, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, LENGTH(data)
			 FROM player_snapshot
			 WHERE player_id = ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*SnapshotRow
	for rows.Next() {
		r := &SnapshotRow{}
		var dataLen uint32
		if err := rows.Scan(&r.ID, &r.PlayerID, &r.ZoneID, &r.SnapshotType, &r.CreatedAt,
			&r.Reason, &r.Operator, &dataLen); err != nil {
			return nil, err
		}
		// Store data size in ZoneID field temporarily — we'll handle this in the logic layer
		// Actually, we just pass dataLen separately via a wrapper if needed.
		// For simplicity, the caller reads DataSizeBytes from the SnapshotInfo.
		result = append(result, r)
	}
	return result, rows.Err()
}

// ListSnapshotsWithSize is like ListSnapshots but also returns data size.
type SnapshotMeta struct {
	SnapshotRow
	DataSizeBytes uint32
}

func (s *SnapshotStore) ListSnapshotsMeta(ctx context.Context, playerID, beforeTime uint64, limit uint32) ([]*SnapshotMeta, error) {
	if limit == 0 {
		limit = 20
	}

	var (
		rows *sql.Rows
		err  error
	)
	if beforeTime > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, COALESCE(LENGTH(data),0)
			 FROM player_snapshot
			 WHERE player_id = ? AND created_at <= ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, beforeTime, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, COALESCE(LENGTH(data),0)
			 FROM player_snapshot
			 WHERE player_id = ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*SnapshotMeta
	for rows.Next() {
		m := &SnapshotMeta{}
		if err := rows.Scan(&m.ID, &m.PlayerID, &m.ZoneID, &m.SnapshotType, &m.CreatedAt,
			&m.Reason, &m.Operator, &m.DataSizeBytes); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// GetPlayerIDsByZone returns all player IDs whose latest snapshot belongs to a given zone.
// Uses the zone_id stored at snapshot time. For accurate zone membership, prefer Router.
func (s *SnapshotStore) GetSnapshotPlayerIDsByZone(ctx context.Context, zoneID uint32, beforeTime uint64) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT player_id FROM player_snapshot
		 WHERE zone_id = ? AND created_at <= ?`, zoneID, beforeTime)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var pid uint64
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		ids = append(ids, pid)
	}
	return ids, rows.Err()
}

// InsertAuditLog writes a rollback audit record.
func (s *SnapshotStore) InsertAuditLog(ctx context.Context, row *AuditLogRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rollback_audit_log
		 (player_id, zone_id, rollback_type, snapshot_id_used, pre_rollback_snapshot_id,
		  target_time, players_affected, players_failed, orphans_cleaned, reason, operator, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.PlayerID, row.ZoneID, row.RollbackType, row.SnapshotIDUsed,
		row.PreRollbackSnapshotID, row.TargetTime, row.PlayersAffected,
		row.PlayersFailed, row.OrphansCleaned, row.Reason, row.Operator, row.CreatedAt,
	)
	return err
}

// DeleteOldSnapshots removes snapshots older than the given timestamp.
// Used for retention policies.
func (s *SnapshotStore) DeleteOldSnapshots(ctx context.Context, olderThan uint64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM player_snapshot WHERE created_at < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
