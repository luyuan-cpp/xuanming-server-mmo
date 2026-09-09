//go:build integration

package store_test

import (
	"context"
	"testing"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	dbpb "proto/common/database"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transaction_log 落库(设计 §2.0c 步骤 4):tx_id 主键 + INSERT IGNORE,重放同一批是 no-op;
// Kafka 载荷的 timestamp 落到 timestamp_sec 列;zone_id 只在表 proto 声明了该列时才落。

func newTxLogStore(t *testing.T, db *storetest.DB) *store.TransactionLogStore {
	t.Helper()
	ts, err := store.NewTransactionLogStore(db.Cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ts.Close() })
	return ts
}

func txRow(txID uint64) *store.TransactionLogRow {
	return &store.TransactionLogRow{
		TxID:          txID,
		Timestamp:     1700000000 + txID,
		TxType:        3,
		FromPlayer:    100,
		ToPlayer:      200,
		ItemUUID:      9000 + txID,
		ItemConfigID:  7,
		ItemQuantity:  3,
		CurrencyType:  1,
		CurrencyDelta: -50,
		BalanceBefore: 1000,
		BalanceAfter:  950,
		CorrelationID: 555,
		Extra:         `{"k":"v"}`,
		ZoneID:        4,
	}
}

// tableProtoHasZoneID 与 store 内部 txLogHasZoneColumn 同一判据(外部测试包看不到那个变量)。
func tableProtoHasZoneID() bool {
	return (&dbpb.TransactionLog{}).ProtoReflect().Descriptor().Fields().ByName("zone_id") != nil
}

func TestTransactionLogStore_InsertIgnoreReplayIsNoop(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ts := newTxLogStore(t, db)
	ctx := context.Background()

	batch := []*store.TransactionLogRow{txRow(1), txRow(2), txRow(3)}
	n, err := ts.InsertBatchIgnore(ctx, batch)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)

	// 整批重放:0 行插入,不报错,表里仍是 3 行。
	n, err = ts.InsertBatchIgnore(ctx, batch)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "replay of an already-persisted batch must be ignored")
	assert.Equal(t, int64(3), db.Count(t, "transaction_log", ""))

	// 部分重叠:只有新 tx_id 进表。
	n, err = ts.InsertBatchIgnore(ctx, []*store.TransactionLogRow{txRow(2), txRow(3), txRow(4)})
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Equal(t, int64(4), db.Count(t, "transaction_log", ""))

	// 空批是 no-op。
	n, err = ts.InsertBatchIgnore(ctx, nil)
	require.NoError(t, err)
	assert.Zero(t, n)

	// 重放不能改写已有行(INSERT IGNORE 不是 REPLACE):改个 extra 再重放,库里仍是旧值。
	mutated := txRow(1)
	mutated.Extra = "mutated"
	_, err = ts.InsertBatchIgnore(ctx, []*store.TransactionLogRow{mutated})
	require.NoError(t, err)
	var extra string
	require.NoError(t, db.Raw.QueryRow(`SELECT extra FROM transaction_log WHERE tx_id = 1`).Scan(&extra))
	assert.Equal(t, `{"k":"v"}`, extra)
}

func TestTransactionLogStore_TimestampSecAndZoneLand(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ts := newTxLogStore(t, db)
	ctx := context.Background()

	_, err := ts.InsertBatchIgnore(ctx, []*store.TransactionLogRow{txRow(1), txRow(2)})
	require.NoError(t, err)

	// timestamp_sec 列(proto 与手写 DDL 统一后的列名)承载 Kafka 的 timestamp。
	var tsSec uint64
	require.NoError(t, db.Raw.QueryRow(`SELECT timestamp_sec FROM transaction_log WHERE tx_id = 2`).Scan(&tsSec))
	assert.Equal(t, uint64(1700000002), tsSec)

	// 读路径按 timestamp_sec 过滤/排序,且能读回全部字段。
	rows, total, err := ts.QueryLog(ctx, &store.TransactionLogQuery{PlayerID: 100, TimeStart: 1700000002})
	require.NoError(t, err)
	assert.Equal(t, uint32(1), total)
	require.Len(t, rows, 1)
	assert.Equal(t, uint64(2), rows[0].TxID)
	assert.Equal(t, uint64(1700000002), rows[0].Timestamp)
	assert.Equal(t, `{"k":"v"}`, rows[0].Extra)
	assert.Equal(t, int64(-50), rows[0].CurrencyDelta)

	byItem, err := ts.QueryByItemUUID(ctx, 9001)
	require.NoError(t, err)
	require.Len(t, byItem, 1)
	assert.Equal(t, uint64(1), byItem[0].TxID)

	// zone_id:载荷 TransactionLogEntry 带 zone_id,但落库与否取决于表 proto 是否声明了该列。
	cols := db.Columns(t, "transaction_log")
	if tableProtoHasZoneID() {
		require.True(t, cols["zone_id"], "table proto declares zone_id, migration must create it")
		var zone uint32
		require.NoError(t, db.Raw.QueryRow(`SELECT zone_id FROM transaction_log WHERE tx_id = 1`).Scan(&zone))
		assert.Equal(t, uint32(4), zone, "capture-time zone must land")
	} else {
		// 现状:rollback_database_table.proto 的 transaction_log 没有 zone_id 字段,
		// 消费者收到的 zone_id 被丢弃(store 启动时打 Error)。表 proto 补上字段后本分支自动失效。
		assert.False(t, cols["zone_id"], "column must not exist unless the table proto declares it")
		t.Logf("transaction_log table proto has no zone_id field; TransactionLogEntry.zone_id is dropped until proto/common/database/rollback_database_table.proto gains it")
	}
}
