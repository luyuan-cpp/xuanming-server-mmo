package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
)

// player_snapshot.source 列的取值契约。两种来源的 data 列格式**不兼容**:
//   - SnapshotSourceDataService(0):GM / data_service 自己写入,data 是 JSON
//     {fields: map[string][]byte}(Redis 字段图),snapshot_type 是 data_service 的 SnapshotType。
//   - SnapshotSourceSceneKafka(1):C++ scene 经 player_snapshot_topic 落库,data 是收到的
//     PlayerSnapshotEntry 原始 proto 字节(内含两个 player_database blob + schema_version),
//     snapshot_type 是 C++ 的 SnapshotTrigger 原值。统一两套枚举是以后的 proto 改动。
//
// 回滚 / diff / 列表等所有现有读路径在学会解 proto blob 之前必须只看 source=0,
// 否则 rollback_logic 的 json.Unmarshal 会在第一条 C++ 快照上炸掉今天能用的 GM 回滚。
const (
	SnapshotSourceDataService uint32 = 0
	SnapshotSourceSceneKafka  uint32 = 1

	// SnapshotOperatorSceneNode 是 source=1 行的 operator 固定值。
	SnapshotOperatorSceneNode = "scene-node"
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
	Data         []byte // source=0: JSON snapshotData; source=1: raw PlayerSnapshotEntry bytes
	SnapshotGuid uint64 // source=1: C++ SnapshotId (SnowFlake); 0 for GM rows
	Source       uint32 // SnapshotSourceDataService | SnapshotSourceSceneKafka
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

// SnapshotStore provides CRUD operations for player snapshots and audit logs.
// 表结构由 MigrateSchema 按 proto 建,本 store 不再碰 DDL。
type SnapshotStore struct {
	db *sql.DB
}

// NewSnapshotStore opens the pool. It does NOT create tables — see schema.go.
func NewSnapshotStore(cfg MySQLConfig) (*SnapshotStore, error) {
	db, err := openMySQL(cfg)
	if err != nil {
		return nil, err
	}
	logx.Infof("[SnapshotStore] connected to %s/%s", cfg.Host, cfg.DBName)
	return &SnapshotStore{db: db}, nil
}

// Close releases the database connection.
func (s *SnapshotStore) Close() error {
	return s.db.Close()
}

// InsertSnapshot saves a snapshot and returns the auto-increment ID.
// GM 路径调用方不设 Source / SnapshotGuid,即落成 source=0、guid=0。
func (s *SnapshotStore) InsertSnapshot(ctx context.Context, row *SnapshotRow) (uint64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO player_snapshot
		 (player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.PlayerID, row.ZoneID, row.SnapshotType, row.CreatedAt, row.Reason, row.Operator, row.Data,
		row.SnapshotGuid, row.Source,
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

// InsertSnapshotIfGuidAbsent 按 snapshot_guid 去重落库,返回 inserted=false 时
// id 是已存在那行的自增 id。
//
// 去重由**一条语句**完成:`INSERT ... SELECT ... WHERE NOT EXISTS (...)`。存在性判断
// 与插入在同一条语句、同一个隐式事务里,InnoDB 会在 snapshot_guid 索引上对那个不存在的
// 值加间隙锁,两个并发实例写同一个 guid 只会有一个成功、另一个看到 0 行受影响。
// 拆成"先 SELECT 再 INSERT"的老写法在两次往返之间没有任何锁:滚动更新的窗口里
// (Deployment 默认 RollingUpdate 会让新旧两个 pod 同时在线)两个消费者实例足以把同一条
// 快照写成两行 source=1。
//
// 为什么不是 UNIQUE 键:GM 行的 guid 恒为 0,proto2mysql 又做不出可空唯一列,UNIQUE 会让
// 第二条 GM 行就撞键。所以 DB 侧的保证止步于"同一条语句 + 间隙锁",要真正杜绝重复
// 还需要 snapshot 消费者单实例(deploy 侧把 data-service 的 Deployment 策略设成 Recreate)。
// 索引本身由 store.MigrateSchema 的 ensureIndexes 保证存在 —— 没有它这条语句会退化成
// 每次全表扫,并且间隙锁的范围会放大到整表。
func (s *SnapshotStore) InsertSnapshotIfGuidAbsent(ctx context.Context, row *SnapshotRow) (id uint64, inserted bool, err error) {
	if row.SnapshotGuid == 0 {
		return 0, false, fmt.Errorf("snapshot_guid must be non-zero for check-then-insert")
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO player_snapshot
		 (player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?
		 FROM DUAL
		 WHERE NOT EXISTS (SELECT 1 FROM player_snapshot AS existing WHERE existing.snapshot_guid = ?)`,
		row.PlayerID, row.ZoneID, row.SnapshotType, row.CreatedAt, row.Reason, row.Operator, row.Data,
		row.SnapshotGuid, row.Source, row.SnapshotGuid,
	)
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot guid %d: %w", row.SnapshotGuid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot guid %d rows affected: %w", row.SnapshotGuid, err)
	}
	if affected > 0 {
		newID, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("insert snapshot guid %d last insert id: %w", row.SnapshotGuid, err)
		}
		return uint64(newID), true, nil
	}

	// 0 行受影响 = 这个 guid 已经在库里;把已存在那行的 id 回给调用方(日志/审计要用)。
	var existing uint64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM player_snapshot WHERE snapshot_guid = ? LIMIT 1`, row.SnapshotGuid,
	).Scan(&existing); err != nil {
		if err == sql.ErrNoRows {
			// 只可能是别人刚把它删了(retention 清理)。不当故障:重放会再落一次。
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("lookup snapshot_guid %d: %w", row.SnapshotGuid, err)
	}
	return existing, false, nil
}

// GetSnapshotByID loads a snapshot by primary key. source=1 行对回滚读路径不可见(见文件头)。
func (s *SnapshotStore) GetSnapshotByID(ctx context.Context, id uint64) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source
		 FROM player_snapshot WHERE id = ? AND source = ?`, id, SnapshotSourceDataService,
	).Scan(&row.ID, &row.PlayerID, &row.ZoneID, &row.SnapshotType, &row.CreatedAt,
		&row.Reason, &row.Operator, &row.Data, &row.SnapshotGuid, &row.Source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// GetLatestSnapshotBefore returns the most recent source=0 snapshot for a player before the given time.
func (s *SnapshotStore) GetLatestSnapshotBefore(ctx context.Context, playerID, beforeTime uint64) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source
		 FROM player_snapshot
		 WHERE player_id = ? AND created_at <= ? AND source = ?
		 ORDER BY created_at DESC LIMIT 1`, playerID, beforeTime, SnapshotSourceDataService,
	).Scan(&row.ID, &row.PlayerID, &row.ZoneID, &row.SnapshotType, &row.CreatedAt,
		&row.Reason, &row.Operator, &row.Data, &row.SnapshotGuid, &row.Source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// ListSnapshots returns metadata (no data blob) for a player's source=0 snapshots.
func (s *SnapshotStore) ListSnapshots(ctx context.Context, playerID, beforeTime uint64, limit uint32) ([]*SnapshotRow, error) {
	metas, err := s.ListSnapshotsMeta(ctx, playerID, beforeTime, limit)
	if err != nil {
		return nil, err
	}
	result := make([]*SnapshotRow, 0, len(metas))
	for _, m := range metas {
		r := m.SnapshotRow
		result = append(result, &r)
	}
	return result, nil
}

// SnapshotMeta is ListSnapshotsMeta's row: metadata plus data size, no blob.
type SnapshotMeta struct {
	SnapshotRow
	DataSizeBytes uint32
}

// ListSnapshotsMeta lists a player's source=0 snapshots newest first.
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
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, snapshot_guid, source, COALESCE(LENGTH(data),0)
			 FROM player_snapshot
			 WHERE player_id = ? AND created_at <= ? AND source = ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, beforeTime, SnapshotSourceDataService, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, snapshot_guid, source, COALESCE(LENGTH(data),0)
			 FROM player_snapshot
			 WHERE player_id = ? AND source = ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, SnapshotSourceDataService, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*SnapshotMeta
	for rows.Next() {
		m := &SnapshotMeta{}
		if err := rows.Scan(&m.ID, &m.PlayerID, &m.ZoneID, &m.SnapshotType, &m.CreatedAt,
			&m.Reason, &m.Operator, &m.SnapshotGuid, &m.Source, &m.DataSizeBytes); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// GetSnapshotPlayerIDsByZone returns all player IDs with a source=0 snapshot in a zone
// at or before beforeTime. Uses the zone_id stored at snapshot time.
func (s *SnapshotStore) GetSnapshotPlayerIDsByZone(ctx context.Context, zoneID uint32, beforeTime uint64) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT player_id FROM player_snapshot
		 WHERE zone_id = ? AND created_at <= ? AND source = ?`, zoneID, beforeTime, SnapshotSourceDataService)
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

// SceneSnapshotMeta 是 source=1(C++ scene)快照的元数据,给将来的 GM 列表 / 恢复路径用。
type SceneSnapshotMeta struct {
	ID            uint64 // player_snapshot.id(自增)
	SnapshotGuid  uint64 // C++ SnapshotId
	ZoneID        uint32
	Trigger       uint32 // C++ SnapshotTrigger 原值(snapshot_type 列)
	CreatedAt     uint64 // = PlayerSnapshotEntry.snapshot_time
	DataSizeBytes uint32
}

// ListSceneSnapshotsByPlayer lists a player's source=1 rows newest first (no blob).
// 尚未接 RPC:source=1 的恢复路径是下一阶段。
func (s *SnapshotStore) ListSceneSnapshotsByPlayer(ctx context.Context, playerID uint64, limit uint32) ([]*SceneSnapshotMeta, error) {
	if limit == 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, snapshot_guid, zone_id, snapshot_type, created_at, COALESCE(LENGTH(data),0)
		 FROM player_snapshot
		 WHERE player_id = ? AND source = ?
		 ORDER BY created_at DESC, id DESC LIMIT ?`, playerID, SnapshotSourceSceneKafka, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*SceneSnapshotMeta
	for rows.Next() {
		m := &SceneSnapshotMeta{}
		if err := rows.Scan(&m.ID, &m.SnapshotGuid, &m.ZoneID, &m.Trigger, &m.CreatedAt, &m.DataSizeBytes); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
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

// DeleteOldSnapshots removes snapshots (both sources) older than the given timestamp.
// Used for retention policies.
func (s *SnapshotStore) DeleteOldSnapshots(ctx context.Context, olderThan uint64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM player_snapshot WHERE created_at < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
