//go:build integration

package store_test

// 旧手写表的集成测试:表**先由老的手写 DDL 建好并写入数据**,再跑 MigrateSchema。
// 和 schema_integration_test.go(空库从零建表)是两条不同的代码路径。
//
// 跑法: go test -tags=integration ./internal/store/...(见 storetest 包注释)

import (
	"context"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/luyuancpp/proto2mysql"
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

// TestMigrateSchema_RejectsLegacyHandWrittenTables 钉住旧手写表的去向:迁移拒绝,一行不动。
//
// 旧手写 DDL 和 proto 对不上的地方:transaction_log 的列默认值(2026-09-29 起),player_snapshot、rollback_audit_log 的
// `id` 自增和默认值(2026-10-30 起,docs/design/player-data-tidb-scaling-plan.md §6.4)。项目没有上线,没有存量库要升级,
// 这种库清库重建。这条用例原来证明「能升级」,那条路从 09-29 起就走不通了(一直是红的),现在证明走不通时不伤数据。
func TestMigrateSchema_RejectsLegacyHandWrittenTables(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for _, ddl := range legacyDDL {
		_, err := db.Raw.ExecContext(ctx, ddl)
		require.NoError(t, err, "create legacy table")
	}

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

	require.ErrorIs(t, db.Migrate(ctx, store.MigrateOptions{}), proto2mysql.ErrSchemaDrift,
		"表形状和 proto 对不上时迁移必须拒绝,不能在旧表上继续跑")

	assert.EqualValues(t, 1, db.Count(t, "player_snapshot", "id = ?", legacySnapshotID))
	assert.EqualValues(t, 1, db.Count(t, "rollback_audit_log", "player_id = ? AND orphans_cleaned = 3", legacyPlayer))
	assert.EqualValues(t, 1, db.Count(t, "transaction_log", "tx_id = ?", legacyTx))
	var (
		gotReason string
		gotBlob   []byte
	)
	require.NoError(t, db.Raw.QueryRow(
		`SELECT reason, data FROM player_snapshot WHERE id = ?`, legacySnapshotID).Scan(&gotReason, &gotBlob))
	assert.Equal(t, "legacy-reason", gotReason)
	assert.Equal(t, legacyBlob, gotBlob)
}
