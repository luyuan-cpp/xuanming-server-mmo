//go:build integration

package store_test

import (
	"context"
	"testing"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// player_snapshot 的 source 列契约(设计 §2.0c 步骤 5)在真实表上的表现:
//   - snapshot_guid 去重是 check-then-insert,同一 guid 两次只落一行;
//   - GM 行(source=0,guid=0)可以有任意多条,与 source=1 行同表共存(索引非 UNIQUE);
//   - 现有 GM 读路径一律只看 source=0,source=1 行对它们不可见;
//   - source=1 行的 data 原样存取(消费者写的是 PlayerSnapshotEntry 原始字节)。

func newSnapshotStore(t *testing.T, db *storetest.DB) *store.SnapshotStore {
	t.Helper()
	ss, err := store.NewSnapshotStore(db.Cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	return ss
}

func sceneRow(playerID, guid, createdAt uint64, data []byte) *store.SnapshotRow {
	return &store.SnapshotRow{
		PlayerID:     playerID,
		ZoneID:       4,
		SnapshotType: 2, // C++ SnapshotTrigger 原值,不是 data_service 的 SnapshotType
		CreatedAt:    createdAt,
		Reason:       "cpp:SNAPSHOT_LOGIN",
		Operator:     store.SnapshotOperatorSceneNode,
		Data:         data,
		SnapshotGuid: guid,
		Source:       store.SnapshotSourceSceneKafka,
	}
}

func gmRow(playerID, createdAt uint64) *store.SnapshotRow {
	return &store.SnapshotRow{
		PlayerID:     playerID,
		ZoneID:       4,
		SnapshotType: 1,
		CreatedAt:    createdAt,
		Reason:       "gm manual",
		Operator:     "gm-1",
		Data:         []byte(`{"fields":{"player_database":"AQID"}}`),
		// SnapshotGuid / Source 不设:GM 路径落成 guid=0、source=0。
	}
}

func TestSnapshotStore_GuidDedupAndGmRowsCoexist(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	ctx := context.Background()

	raw := []byte{0x0a, 0x03, 'a', 'b', 'c', 0x12, 0x02, 0xff, 0x00} // 任意字节,含 NUL 与高位
	id1, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(100, 777, 1700000200, raw))
	require.NoError(t, err)
	require.True(t, inserted)
	require.NotZero(t, id1)

	// 同一 guid 重放(消费者提交 offset 前崩溃后的重放形态):不再插,返回已有行 id。
	id2, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(100, 777, 1700000200, raw))
	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Equal(t, id1, id2)
	assert.Equal(t, int64(1), db.Count(t, "player_snapshot", "snapshot_guid = ?", 777))

	// guid=0 不能走 check-then-insert:那是 GM 行的形态,按 0 去重会把所有 GM 行当成重复。
	_, _, err = ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(100, 0, 1700000200, raw))
	require.Error(t, err)

	// 两条 GM 行 guid 都是 0,必须都能落(索引非 UNIQUE)。
	gm1, err := ss.InsertSnapshot(ctx, gmRow(100, 1700000100))
	require.NoError(t, err)
	gm2, err := ss.InsertSnapshot(ctx, gmRow(100, 1700000150))
	require.NoError(t, err)
	assert.NotEqual(t, gm1, gm2)
	assert.Equal(t, int64(2), db.Count(t, "player_snapshot", "snapshot_guid = 0 AND source = 0"))
	assert.Equal(t, int64(3), db.Count(t, "player_snapshot", "player_id = 100"))

	// source=1 行原样存取:字节与列值都不能被改写。
	var (
		gotData   []byte
		gotType   uint32
		gotSource uint32
		gotOp     string
	)
	require.NoError(t, db.Raw.QueryRow(
		`SELECT data, snapshot_type, source, operator FROM player_snapshot WHERE id = ?`, id1).
		Scan(&gotData, &gotType, &gotSource, &gotOp))
	assert.Equal(t, raw, gotData)
	assert.Equal(t, uint32(2), gotType)
	assert.Equal(t, store.SnapshotSourceSceneKafka, gotSource)
	assert.Equal(t, store.SnapshotOperatorSceneNode, gotOp)
}

func TestSnapshotStore_GmReadersExcludeSceneRows(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	ctx := context.Background()

	// 玩家 100:两条 GM 行 + 一条**更新**的 scene 行(created_at 最大,故意诱导"最新"查询选中它)。
	gm1, err := ss.InsertSnapshot(ctx, gmRow(100, 1700000100))
	require.NoError(t, err)
	gm2, err := ss.InsertSnapshot(ctx, gmRow(100, 1700000150))
	require.NoError(t, err)
	sceneID, _, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(100, 777, 1700000999, []byte("raw")))
	require.NoError(t, err)
	// 玩家 200:只有 scene 行。
	_, _, err = ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(200, 778, 1700000300, []byte("raw2")))
	require.NoError(t, err)

	// GetSnapshotByID:scene 行按 id 也查不到(rollback 会对它 json.Unmarshal 失败)。
	got, err := ss.GetSnapshotByID(ctx, sceneID)
	require.NoError(t, err)
	assert.Nil(t, got, "source=1 row must be invisible to GetSnapshotByID")
	got, err = ss.GetSnapshotByID(ctx, gm1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, store.SnapshotSourceDataService, got.Source)
	assert.Equal(t, uint64(0), got.SnapshotGuid)

	// GetLatestSnapshotBefore:最新的是 scene 行,但必须返回最新的 GM 行。
	latest, err := ss.GetLatestSnapshotBefore(ctx, 100, 1800000000)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, gm2, latest.ID)
	assert.Equal(t, store.SnapshotSourceDataService, latest.Source)
	// 玩家 200 只有 scene 行 → GM 视角没有快照。
	latest, err = ss.GetLatestSnapshotBefore(ctx, 200, 1800000000)
	require.NoError(t, err)
	assert.Nil(t, latest)

	// ListSnapshotsMeta / ListSnapshots:只列 GM 行。
	metas, err := ss.ListSnapshotsMeta(ctx, 100, 0, 10)
	require.NoError(t, err)
	require.Len(t, metas, 2)
	for _, m := range metas {
		assert.Equal(t, store.SnapshotSourceDataService, m.Source)
	}
	assert.Equal(t, gm2, metas[0].ID, "newest first")
	list, err := ss.ListSnapshots(ctx, 200, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, list)

	// GetSnapshotPlayerIDsByZone:zone 4 里只有玩家 100 有 GM 快照;200 只有 scene 行,不算。
	ids, err := ss.GetSnapshotPlayerIDsByZone(ctx, 4, 1800000000)
	require.NoError(t, err)
	assert.Equal(t, []uint64{100}, ids)

	// scene 专用读路径看得到 source=1 行,且不带 blob 只带长度。
	scene, err := ss.ListSceneSnapshotsByPlayer(ctx, 100, 10)
	require.NoError(t, err)
	require.Len(t, scene, 1)
	assert.Equal(t, uint64(777), scene[0].SnapshotGuid)
	assert.Equal(t, sceneID, scene[0].ID)
	assert.Equal(t, uint32(2), scene[0].Trigger)
	assert.Equal(t, uint64(1700000999), scene[0].CreatedAt)
	assert.Equal(t, uint32(3), scene[0].DataSizeBytes)

	// rollback_audit_log 的 orphans_cleaned 列真的能写(以前 proto 缺这列)。
	require.NoError(t, ss.InsertAuditLog(ctx, &store.AuditLogRow{
		ZoneID: 4, RollbackType: 2, TargetTime: 1700000100, PlayersAffected: 1, OrphansCleaned: 3,
		Reason: "it", Operator: "gm-1", CreatedAt: 1700001000,
	}))
	var orphans uint32
	require.NoError(t, db.Raw.QueryRow(`SELECT orphans_cleaned FROM rollback_audit_log WHERE zone_id = 4`).Scan(&orphans))
	assert.Equal(t, uint32(3), orphans)
}
