//go:build integration

package store_test

// 集成测试:需要本地 MySQL(默认 etc/data_service.yaml 的 SnapshotMySQL,127.0.0.1:3306)。
// 每个用例建一个一次性库 data_service_it_<pid>_<n>,跑完 DROP;yaml 指向的库不被触碰。
// 跑法:  go test -tags=integration ./internal/store/...
// 建库权限不够时(本地 docker 的 appuser)用 root 跑:
//   DATA_SERVICE_IT_MYSQL_USER=root DATA_SERVICE_IT_MYSQL_PASSWORD=<deploy/docker-compose.yml> \
//     go test -tags=integration ./internal/store/...

import (
	"context"
	"strings"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateSchema_CreatesGlobalTablesAndIsIdempotent 验证设计 §2.0c 步骤 3 的落地:
// 四张表全部由 proto 驱动建出(id_segment 走唯一的 bootstrap 例外),列齐,
// 且第二次迁移对已建好的表零变更(生产 -migrate 会在每次部署重复跑)。
func TestMigrateSchema_CreatesGlobalTablesAndIsIdempotent(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}), "first migration")

	// 表与列:每张表的关键列都得在(以前手写 DDL 与 proto 漂移的正是这些列)。
	wantColumns := map[string][]string{
		"transaction_log": {
			"tx_id", "timestamp_sec", "tx_type", "from_player", "to_player",
			"item_uuid", "item_config_id", "item_quantity",
			"currency_type", "currency_delta", "balance_before", "balance_after",
			"correlation_id", "extra",
		},
		"player_snapshot": {
			"id", "player_id", "zone_id", "snapshot_type", "created_at", "reason", "operator", "data",
			"snapshot_guid", "source",
		},
		"rollback_audit_log": {
			"id", "player_id", "zone_id", "rollback_type", "snapshot_id_used", "pre_rollback_snapshot_id",
			"target_time", "players_affected", "players_failed", "reason", "operator", "created_at",
			"orphans_cleaned",
		},
		store.IdSegmentTableName: {"biz_tag", "max_id", "step", "version"},
	}
	for table, cols := range wantColumns {
		have := db.Columns(t, table)
		require.NotEmpty(t, have, "table %s must exist after migration", table)
		for _, c := range cols {
			assert.Truef(t, have[c], "table %s lacks column %s (have %v)", table, c, keys(have))
		}
	}

	// id_segment 的 bootstrap 例外:biz_tag 必须是 VARCHAR(64) 主键,不是 proto2mysql 的 MEDIUMTEXT。
	var colType, colKey string
	require.NoError(t, db.Raw.QueryRow(
		`SELECT COLUMN_TYPE, COLUMN_KEY FROM INFORMATION_SCHEMA.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'biz_tag'`,
		db.Name, store.IdSegmentTableName).Scan(&colType, &colKey))
	assert.Equal(t, "varchar(64)", strings.ToLower(colType), "id_segment.biz_tag column type")
	assert.Equal(t, "PRI", colKey, "id_segment.biz_tag must be the primary key")

	// player_snapshot.snapshot_guid 要有普通索引(消费者按 guid 先查后插),且**不是** UNIQUE
	// (GM 行 guid 恒 0,UNIQUE 会让第二条 GM 行撞键)。
	var nonUnique int
	require.NoError(t, db.Raw.QueryRow(
		`SELECT NON_UNIQUE FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'player_snapshot' AND COLUMN_NAME = 'snapshot_guid' AND SEQ_IN_INDEX = 1
		  LIMIT 1`, db.Name).Scan(&nonUnique), "player_snapshot.snapshot_guid must be indexed")
	assert.Equal(t, 1, nonUnique, "snapshot_guid index must be non-unique")

	// (zone_id, created_at):GetSnapshotPlayerIDsByZone / RollbackZone 的过滤条件。
	// proto 的 OptionIndex 目前只有 "player_id;snapshot_guid",这条由 schema.go 的
	// extraIndexes 补(见那里的注释:proto 补上之后这条会自动被判为已覆盖)。
	// 少了它,回滚一个 zone 要扫完全服快照表,而且没有任何报错。
	var zoneIdxCols int
	require.NoError(t, db.Raw.QueryRow(
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'player_snapshot'
		    AND INDEX_NAME IN (
		      SELECT INDEX_NAME FROM INFORMATION_SCHEMA.STATISTICS
		       WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'player_snapshot'
		         AND COLUMN_NAME = 'zone_id' AND SEQ_IN_INDEX = 1)
		    AND COLUMN_NAME = 'created_at' AND SEQ_IN_INDEX = 2`,
		db.Name, db.Name).Scan(&zoneIdxCols))
	assert.Positive(t, zoneIdxCols, "player_snapshot must have a (zone_id, created_at) index")

	// transaction_log 的主键是 tx_id(INSERT IGNORE 幂等的根基)。
	var txPK string
	require.NoError(t, db.Raw.QueryRow(
		`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'transaction_log' AND CONSTRAINT_NAME = 'PRIMARY'`,
		db.Name).Scan(&txPK))
	assert.Equal(t, "tx_id", txPK)

	// 幂等:第二次迁移不报错,且四张表的 DDL 一字不变。
	before := map[string]string{}
	for table := range wantColumns {
		before[table] = db.ShowCreateTable(t, table)
	}
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}), "second migration must be a no-op")
	for table := range wantColumns {
		assert.Equalf(t, before[table], db.ShowCreateTable(t, table), "table %s DDL changed on second migration", table)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
