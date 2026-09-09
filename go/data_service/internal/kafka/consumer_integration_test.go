//go:build integration

package kafka

// 集成测试:两条消费者接**真实** MySQL store(reader 仍是 fake,Kafka 本身不在测试范围;
// EnsureTopics / kafka-go 的行为由 shared/kafkautil 与 match 服务各自覆盖)。
// 跑法:  go test -tags=integration ./internal/kafka/...
// 需要本地 MySQL,见 internal/store/storetest 包注释(建库权限不够时用 root)。

import (
	"context"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	rollbackpb "proto/common/rollback"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// waitCommitted 轮询到 partition 的已提交最高 offset >= want。
func waitCommitted(t *testing.T, r *fakeReader, partition int, want int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := r.committedMax()[partition]; ok && got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("partition %d: offset %d not committed within deadline (committed=%v)", partition, want, r.committedMax())
}

// runConsumer 起 Run,返回"取消并等退出"的函数;Run 必须以 nil 退出(不是 DB 故障停机)。
func runConsumer(t *testing.T, c runner) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err, "consumer must exit cleanly on shutdown")
		case <-time.After(10 * time.Second):
			t.Fatal("consumer did not exit after cancel")
		}
	}
}

func TestSnapshotConsumer_RealStore_DedupsBySnapshotGuidAndStaysInvisibleToGm(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss, err := store.NewSnapshotStore(db.Cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })

	reader := newFakeReader(nil)
	c := NewSnapshotConsumer(SnapshotConsumerConfig{DBRetryBackoff: 10 * time.Millisecond}, reader, ss)
	stop := runConsumer(t, c)

	entry777 := validSnap(777)
	entry778 := validSnap(778)
	entry778.PlayerId = 200
	raw777, err := proto.Marshal(entry777)
	require.NoError(t, err)

	// 同一 guid 在两个 offset 上出现(提交前崩溃后的重放),外加另一玩家的一条。
	reader.push(
		snapMsg(t, 0, 0, entry777),
		snapMsg(t, 0, 1, entry777),
		snapMsg(t, 1, 0, entry778),
	)
	waitCommitted(t, reader, 0, 1)
	waitCommitted(t, reader, 1, 0)
	stop()

	// 落库:guid 去重后恰好两行,全部 source=1。
	assert.Equal(t, int64(1), db.Count(t, "player_snapshot", "snapshot_guid = ?", 777))
	assert.Equal(t, int64(2), db.Count(t, "player_snapshot", "source = ?", store.SnapshotSourceSceneKafka))
	assert.Equal(t, int64(0), db.Count(t, "player_snapshot", "source = ?", store.SnapshotSourceDataService))

	// 列契约:snapshot_type = SnapshotTrigger 原值;data = 收到的整条 PlayerSnapshotEntry 字节;
	// zone_id / created_at 取自载荷,operator 固定。
	var (
		gotType, gotZone uint32
		gotCreated       uint64
		gotData          []byte
		gotOp            string
	)
	require.NoError(t, db.Raw.QueryRow(
		`SELECT snapshot_type, zone_id, created_at, data, operator FROM player_snapshot WHERE snapshot_guid = 777`).
		Scan(&gotType, &gotZone, &gotCreated, &gotData, &gotOp))
	assert.Equal(t, uint32(rollbackpb.SnapshotTrigger_SNAPSHOT_LOGIN), gotType)
	assert.Equal(t, uint32(4), gotZone)
	assert.Equal(t, uint64(1700000123), gotCreated)
	assert.Equal(t, raw777, gotData)
	assert.Equal(t, store.SnapshotOperatorSceneNode, gotOp)
	decoded := &rollbackpb.PlayerSnapshotEntry{}
	require.NoError(t, proto.Unmarshal(gotData, decoded))
	assert.Equal(t, []byte("blob-a"), decoded.GetPlayerDatabaseBlob())
	assert.Equal(t, "v1", decoded.GetSchemaVersion())

	// GM 读路径看不到这些行;scene 读路径看得到。
	metas, err := ss.ListSnapshotsMeta(context.Background(), 100, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, metas)
	latest, err := ss.GetLatestSnapshotBefore(context.Background(), 100, 1800000000)
	require.NoError(t, err)
	assert.Nil(t, latest)
	scene, err := ss.ListSceneSnapshotsByPlayer(context.Background(), 100, 10)
	require.NoError(t, err)
	require.Len(t, scene, 1)
	assert.Equal(t, uint64(777), scene[0].SnapshotGuid)
}

func TestTxLogConsumer_RealStore_ReplayAfterCommitIsNoop(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ts, err := store.NewTransactionLogStore(db.Cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ts.Close() })

	reader := newFakeReader(nil)
	c := NewTxLogConsumer(TxLogConsumerConfig{
		BatchSize:      3,
		FlushInterval:  50 * time.Millisecond,
		DBRetryBackoff: 10 * time.Millisecond,
	}, reader, ts)
	stop := runConsumer(t, c)

	// 第一批:凑满 3 条触发落库。
	reader.push(txMsg(t, 0, 0, validTx(1)), txMsg(t, 0, 1, validTx(2)), txMsg(t, 0, 2, validTx(3)))
	waitCommitted(t, reader, 0, 2)
	assert.Equal(t, int64(3), db.Count(t, "transaction_log", ""))

	// 重放同三条(新 offset)+ 一条新的,外加一条坏消息:坏消息只提交不入库,
	// 重放被 IGNORE,只有 tx 4 新增;这批 5 条按时间驱动落库。
	reader.push(
		txMsg(t, 0, 3, validTx(1)), txMsg(t, 0, 4, validTx(2)), txMsg(t, 0, 5, validTx(3)),
		badMsg(0, 6),
		txMsg(t, 0, 7, validTx(4)),
	)
	waitCommitted(t, reader, 0, 7)
	stop()

	assert.Equal(t, int64(4), db.Count(t, "transaction_log", ""))
	rows, total, err := ts.QueryLog(context.Background(), &store.TransactionLogQuery{PlayerID: 100})
	require.NoError(t, err)
	assert.Equal(t, uint32(4), total)
	require.Len(t, rows, 4)
	assert.Equal(t, uint64(4), rows[0].TxID, "newest timestamp_sec first")
	assert.Equal(t, uint64(1700000004), rows[0].Timestamp)

	var tsSec uint64
	require.NoError(t, db.Raw.QueryRow(`SELECT timestamp_sec FROM transaction_log WHERE tx_id = 1`).Scan(&tsSec))
	assert.Equal(t, uint64(1700000001), tsSec)
}
