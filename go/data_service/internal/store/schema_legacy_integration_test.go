//go:build integration

package store_test

// 存量库升级路径的集成测试:表**先由老的手写 DDL 建好并写入数据**,再跑 MigrateSchema。
// 这是线上/本地任何一台跑过旧版 data_service 的机器的真实形态,与
// schema_integration_test.go(空库从零建表)是两条完全不同的代码路径:
// proto2mysql 的 syncTableSchema 只在表不存在时执行 CREATE TABLE(索引在那条语句里),
// 表已存在时它一条索引都不建,只 ADD/MODIFY/CHANGE COLUMN。
//
// 跑法: go test -tags=integration ./internal/store/...(见 storetest 包注释)

import (
	"context"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyDDL 是收口到 proto 之前 store 自己建表用的那三段语句,逐字来自
// `git show HEAD:go/data_service/internal/store/{snapshot_store,transaction_log_store}.go`。
// 关键差异(升级时会被 proto2mysql 逐列 MODIFY):列上没有 pb:N 注释、data 是 LONGBLOB
// 而不是 MEDIUMBLOB、reason/operator/extra 是 VARCHAR 而不是 MEDIUMTEXT;
// player_snapshot 少 snapshot_guid / source 两列,transaction_log 少 zone_id。
var legacyDDL = []string{
	`CREATE TABLE IF NOT EXISTS player_snapshot (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		player_id BIGINT UNSIGNED NOT NULL,
		zone_id INT UNSIGNED NOT NULL DEFAULT 0,
		snapshot_type INT UNSIGNED NOT NULL DEFAULT 0,
		created_at BIGINT UNSIGNED NOT NULL,
		reason VARCHAR(512) NOT NULL DEFAULT '',
		operator VARCHAR(128) NOT NULL DEFAULT '',
		data LONGBLOB,
		PRIMARY KEY (id),
		INDEX idx_player_created (player_id, created_at),
		INDEX idx_zone_created (zone_id, created_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
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
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS transaction_log (
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
		PRIMARY KEY (tx_id),
		INDEX idx_player_time (from_player, timestamp_sec),
		INDEX idx_to_player_time (to_player, timestamp_sec),
		INDEX idx_item_config (item_config_id, timestamp_sec),
		INDEX idx_tx_type_time (tx_type, timestamp_sec)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// indexLeadingColumns 返回表上每条索引的**首列**(SEQ_IN_INDEX=1),用于"这条查询有没有索引可用"的断言。
func indexLeadingColumns(t *testing.T, db *storetest.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Raw.Query(
		`SELECT INDEX_NAME, COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND SEQ_IN_INDEX = 1`, db.Name, table)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var idx, col string
		require.NoError(t, rows.Scan(&idx, &col))
		out[idx] = col
	}
	require.NoError(t, rows.Err())
	return out
}

// hasIndexLeadingWith 表上是否存在首列为 col 的索引(名字无所谓:存量表的索引名是手写 DDL 起的)。
func hasIndexLeadingWith(t *testing.T, db *storetest.DB, table, col string) bool {
	t.Helper()
	for _, leading := range indexLeadingColumns(t, db, table) {
		if leading == col {
			return true
		}
	}
	return false
}

// TestMigrateSchema_UpgradesLegacyHandWrittenTables 锁死存量升级路径:
// 老表 + 老数据 → MigrateSchema → 数据一行不少、新列补上、读路径要的索引都在、再跑一次零变更。
func TestMigrateSchema_UpgradesLegacyHandWrittenTables(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for _, ddl := range legacyDDL {
		_, err := db.Raw.ExecContext(ctx, ddl)
		require.NoError(t, err, "create legacy table")
	}

	// 存量数据:一条 GM 快照(升级后必须仍是 source=0 且能被回滚路径读到)、
	// 一条审计、一条流水(升级后 zone_id 是新列,老行取默认值 0)。
	const (
		legacyPlayer = uint64(90001)
		legacyZone   = uint32(7)
		legacyTx     = uint64(770001)
	)
	legacyBlob := []byte(`{"fields":{"base":"bGVnYWN5"}}`)
	res, err := db.Raw.ExecContext(ctx,
		`INSERT INTO player_snapshot (player_id, zone_id, snapshot_type, created_at, reason, operator, data)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		legacyPlayer, legacyZone, 1, 1700000000, "legacy-reason", "gm-old", legacyBlob)
	require.NoError(t, err)
	legacySnapshotID, err := res.LastInsertId()
	require.NoError(t, err)

	_, err = db.Raw.ExecContext(ctx,
		`INSERT INTO rollback_audit_log (player_id, zone_id, rollback_type, snapshot_id_used, orphans_cleaned, reason, operator, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		legacyPlayer, legacyZone, 1, legacySnapshotID, 3, "legacy-audit", "gm-old", 1700000001)
	require.NoError(t, err)

	_, err = db.Raw.ExecContext(ctx,
		`INSERT INTO transaction_log (tx_id, timestamp_sec, tx_type, from_player, extra)
		 VALUES (?, ?, ?, ?, ?)`, legacyTx, 1700000002, 2, legacyPlayer, "legacy-extra")
	require.NoError(t, err)

	// ── 升级 ────────────────────────────────────────────────────────────
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}), "migrate a database whose tables came from the old hand-written DDL")

	// 1) 数据没被重写掉。
	assert.EqualValues(t, 1, db.Count(t, "player_snapshot", "id = ?", legacySnapshotID))
	assert.EqualValues(t, 1, db.Count(t, "rollback_audit_log", "player_id = ? AND orphans_cleaned = 3", legacyPlayer))
	assert.EqualValues(t, 1, db.Count(t, "transaction_log", "tx_id = ?", legacyTx))

	var (
		gotReason string
		gotBlob   []byte
	)
	require.NoError(t, db.Raw.QueryRow(
		`SELECT reason, data FROM player_snapshot WHERE id = ?`, legacySnapshotID).Scan(&gotReason, &gotBlob))
	assert.Equal(t, "legacy-reason", gotReason, "MODIFY COLUMN must not rewrite the payload")
	assert.Equal(t, legacyBlob, gotBlob, "LONGBLOB → MEDIUMBLOB must keep the bytes")

	// 2) 新列补上了。
	snapshotCols := db.Columns(t, "player_snapshot")
	assert.True(t, snapshotCols["snapshot_guid"], "legacy player_snapshot must gain snapshot_guid")
	assert.True(t, snapshotCols["source"], "legacy player_snapshot must gain source")
	assert.True(t, db.Columns(t, "transaction_log")["zone_id"], "legacy transaction_log must gain zone_id")

	// 3) 索引:消费者按 snapshot_guid 先查后插,GetSnapshotPlayerIDsByZone / RollbackZone
	//    按 (zone_id, created_at) 过滤。缺任何一条都是"能跑但每次全表扫"的静默退化。
	assert.True(t, hasIndexLeadingWith(t, db, "player_snapshot", "snapshot_guid"),
		"snapshot_guid index missing after upgrade: InsertSnapshotIfGuidAbsent would full-scan per consumed snapshot (indexes=%v)",
		indexLeadingColumns(t, db, "player_snapshot"))
	assert.True(t, hasIndexLeadingWith(t, db, "player_snapshot", "zone_id"),
		"(zone_id, created_at) index missing after upgrade: RollbackZone would full-scan (indexes=%v)",
		indexLeadingColumns(t, db, "player_snapshot"))
	// 存量表原有的索引不该被动:老名字仍在,不会重复建一条同前缀的新索引。
	legacyIdx := indexLeadingColumns(t, db, "player_snapshot")
	assert.Contains(t, legacyIdx, "idx_player_created", "existing indexes must survive the upgrade")
	assert.Contains(t, legacyIdx, "idx_zone_created")
	assert.NotContains(t, legacyIdx, "idx_player_snapshot_0", "a duplicate of an already-covered index must not be created")
	txIdx := indexLeadingColumns(t, db, "transaction_log")
	for _, name := range []string{"idx_player_time", "idx_to_player_time", "idx_item_config", "idx_tx_type_time"} {
		assert.Contains(t, txIdx, name, "existing transaction_log indexes must survive")
	}
	assert.NotContains(t, txIdx, "idx_transaction_log_0", "the four legacy indexes already cover the proto ones")

	// 4) 升级后的表能被 store 正常用:老行仍是 GM 行(source=0),新的 source=1 行按 guid 去重。
	ss, err := store.NewSnapshotStore(db.Cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })

	legacyRow, err := ss.GetSnapshotByID(ctx, uint64(legacySnapshotID))
	require.NoError(t, err)
	require.NotNil(t, legacyRow, "the legacy row must stay visible to the GM/rollback read path (source=0)")
	assert.Equal(t, legacyPlayer, legacyRow.PlayerID)
	assert.EqualValues(t, store.SnapshotSourceDataService, legacyRow.Source)

	sceneRow := &store.SnapshotRow{
		PlayerID: legacyPlayer, ZoneID: legacyZone, SnapshotType: 2, CreatedAt: 1700000100,
		Reason: "cpp:test", Operator: store.SnapshotOperatorSceneNode,
		Data: []byte("raw-entry"), SnapshotGuid: 5150, Source: store.SnapshotSourceSceneKafka,
	}
	id1, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow)
	require.NoError(t, err)
	assert.True(t, inserted)
	id2, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow)
	require.NoError(t, err)
	assert.False(t, inserted, "the same snapshot_guid must not produce a second row")
	assert.Equal(t, id1, id2)

	// 5) 再跑一次迁移零变更(部署脚本每次发布都会重复跑 -migrate)。
	before := map[string]string{}
	for _, table := range []string{"player_snapshot", "transaction_log", "rollback_audit_log"} {
		before[table] = db.ShowCreateTable(t, table)
	}
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}), "second migration on an upgraded database must be a no-op")
	for table, ddl := range before {
		assert.Equalf(t, ddl, db.ShowCreateTable(t, table), "table %s DDL changed on the second migration", table)
	}
}
