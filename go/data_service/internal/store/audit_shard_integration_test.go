//go:build integration

package store_test

// 审计两张表的分桶列 shard(2026-11-06,缺口清单第 15 条,docs/design/audit-pipeline-horizontal-scale-20261106.md §2.C):
// 写入时按 store.AuditShard 落值;按时间 / 按 zone 的索引都在时间列前插了 shard;没有玩家条件的查询带
// shard IN (0..15) 仍然查全。

import (
	"context"
	"testing"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// indexColumnsOf 读一张表的全部索引(含 PRIMARY),值是按 SEQ_IN_INDEX 排好的列序。
func indexColumnsOf(t *testing.T, db *storetest.DB, table string) map[string][]string {
	t.Helper()
	rows, err := db.Raw.Query(
		`SELECT INDEX_NAME, COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		  ORDER BY INDEX_NAME, SEQ_IN_INDEX`, db.Name, table)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var idx, col string
		require.NoError(t, rows.Scan(&idx, &col))
		out[idx] = append(out[idx], col)
	}
	require.NoError(t, rows.Err())
	return out
}

func hasIndexExactly(indexes map[string][]string, cols ...string) bool {
	for _, got := range indexes {
		if len(got) != len(cols) {
			continue
		}
		match := true
		for i := range cols {
			if got[i] != cols[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestAuditShard_IndexesLeadTimeWithShard:按时间写的索引都在时间列前有 shard;不分桶的单调形态一条都不剩
// (新库按 proto 建,schema.go 也不再兜底补 (zone_id, created_at))。按玩家、按物品 uuid 的索引不分桶。
func TestAuditShard_IndexesLeadTimeWithShard(t *testing.T) {
	db := storetest.NewMigratedDB(t)

	tx := indexColumnsOf(t, db, "transaction_log")
	for _, want := range [][]string{
		{"from_player", "timestamp_sec"},
		{"to_player", "timestamp_sec"},
		{"item_config_id", "shard", "timestamp_sec"},
		{"tx_type", "shard", "timestamp_sec"},
		{"shard", "timestamp_sec"},
		{"item_uuid"},
	} {
		assert.Truef(t, hasIndexExactly(tx, want...), "transaction_log 缺索引 %v(有 %v)", want, tx)
	}
	for _, unwanted := range [][]string{{"timestamp_sec"}, {"tx_type", "timestamp_sec"}, {"item_config_id", "timestamp_sec"}} {
		assert.Falsef(t, hasIndexExactly(tx, unwanted...), "transaction_log 不该再有单调索引 %v", unwanted)
	}

	snap := indexColumnsOf(t, db, "player_snapshot")
	for _, want := range [][]string{{"player_id"}, {"snapshot_guid"}, {"zone_id", "shard", "created_at"}, {"shard", "created_at"}} {
		assert.Truef(t, hasIndexExactly(snap, want...), "player_snapshot 缺索引 %v(有 %v)", want, snap)
	}
	for _, unwanted := range [][]string{{"created_at"}, {"zone_id", "created_at"}} {
		assert.Falsef(t, hasIndexExactly(snap, unwanted...), "player_snapshot 不该再有单调索引 %v", unwanted)
	}
}

// TestAuditShard_TransactionLogCarriesShardAndUnfilteredQueriesSeeAllShards:每行 shard = AuditShard(tx_id),
// 16 个桶都有行;没有玩家条件的按时间窗 / tx_type / item_config_id 查询数得全、按时间倒序取得全。
func TestAuditShard_TransactionLogCarriesShardAndUnfilteredQueriesSeeAllShards(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ts := newTxLogStore(t, db)
	ctx := context.Background()

	const n = 2 * store.AuditShardCount // 每个桶两行(tx_id 连续,取模后均匀)
	var batch []*store.TransactionLogRow
	for id := uint64(1); id <= n; id++ {
		batch = append(batch, txRow(id))
	}
	inserted, err := ts.InsertBatchIgnore(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, int64(n), inserted)

	rows, err := db.Raw.QueryContext(ctx, `SELECT tx_id, shard FROM transaction_log`)
	require.NoError(t, err)
	defer rows.Close()
	perShard := map[uint32]int{}
	for rows.Next() {
		var txID uint64
		var shard uint32
		require.NoError(t, rows.Scan(&txID, &shard))
		assert.Equalf(t, store.AuditShard(txID), shard, "tx_id=%d", txID)
		perShard[shard]++
	}
	require.NoError(t, rows.Err())
	assert.Len(t, perShard, store.AuditShardCount, "16 个桶都要有行")

	got, total, err := ts.QueryLog(ctx, &store.TransactionLogQuery{TimeStart: 1700000001, TimeEnd: 1700000000 + n, Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, uint32(n), total)
	require.Len(t, got, n)
	assert.Equal(t, uint64(n), got[0].TxID, "按 timestamp_sec 倒序,跨桶合并后最新的在前")
	assert.Equal(t, uint64(1), got[n-1].TxID)

	_, total, err = ts.QueryLog(ctx, &store.TransactionLogQuery{TxTypes: []uint32{3}, ItemConfigID: 7, Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, uint32(n), total, "tx_type + item_config_id 过滤同样跨 16 个桶查全")

	// 带玩家条件的不加分桶谓词,结果不变。
	_, total, err = ts.QueryLog(ctx, &store.TransactionLogQuery{PlayerID: 200, Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, uint32(n), total)
}

// TestAuditShard_SnapshotRowsCarryShardAndZoneQueriesSeeAllShards:快照的 shard = AuditShard(行号 id),
// GM 路径与按 guid 去重的路径都写;按 zone 扫的两条回档查询跨 16 个桶查全。
func TestAuditShard_SnapshotRowsCarryShardAndZoneQueriesSeeAllShards(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	ctx := context.Background()

	const players = 2 * store.AuditShardCount
	for i := uint64(1); i <= players; i++ {
		_, err := ss.InsertSnapshot(ctx, &store.SnapshotRow{
			PlayerID: 5000 + i, ZoneID: 9, CreatedAt: 1700000000 + i, Reason: "it", Operator: "it", Data: []byte("{}"),
		})
		require.NoError(t, err)
	}
	_, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, &store.SnapshotRow{
		PlayerID: 6000, ZoneID: 9, CreatedAt: 1700000100, Data: []byte("x"),
		SnapshotGuid: 4242, Source: store.SnapshotSourceSceneKafka,
	})
	require.NoError(t, err)
	require.True(t, inserted)

	rows, err := db.Raw.QueryContext(ctx, `SELECT id, shard FROM player_snapshot`)
	require.NoError(t, err)
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id uint64
		var shard uint32
		require.NoError(t, rows.Scan(&id, &shard))
		assert.Equalf(t, store.AuditShard(id), shard, "id=%d", id)
		count++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, players+1, count)

	ids, err := ss.GetSnapshotPlayerIDsByZone(ctx, 9, 1700000000+players)
	require.NoError(t, err)
	assert.Len(t, ids, players, "只数 source=0 的 GM 快照,跨 16 个桶查全")
	times, err := ss.GetSnapshotPlayerTimesByZone(ctx, 9, 1700000000+players)
	require.NoError(t, err)
	assert.Len(t, times, players)
	assert.Equal(t, uint64(1700000000+players), times[5000+players])
}
