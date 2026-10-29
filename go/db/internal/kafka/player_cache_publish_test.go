package kafka

import (
	"context"
	"db/internal/metrics"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	componentpb "proto/common/component"
	dbpb "proto/db"
	"shared/assetop"
	"shared/savebarrier"
	"testing"
)

func fencePublishFixture(t *testing.T) (*worker, *orderingLease, *dbpb.DBTask) {
	t.Helper()
	w, _, _ := newTestWorker(t, 0)
	task := &dbpb.DBTask{Key: 42, Op: dbpb.DBOp_DB_OP_WRITE, MsgType: "player_database", OwnerEpoch: 7, SaveSeq: 9}
	lease, err := acquireOrderingLock(context.Background(), w, task)
	require.NoError(t, err)
	t.Cleanup(func() { releaseOrderingLock(context.Background(), lease, task) })
	return w, lease, task
}

func TestPlayerCachePublicationCannotOverwriteNewerVersion(t *testing.T) {
	w, lease, task := fencePublishFixture(t)
	rc := w.redisClient
	require.NoError(t, rc.Set(context.Background(), buildCacheKey(task), "newer", 0).Err())
	require.NoError(t, rc.Set(context.Background(), "distributed:lock:"+orderingLockKey(w.topic, task), "successor", 0).Err())
	before := metricCount(t, "db_ordering_lease_lost_total", map[string]string{"reason": metrics.LeaseLostOwnerChanged})
	require.ErrorIs(t, publishDBWriteResult(context.Background(), lease, task, fenceRow(3)), errOrderingLeaseLost)
	raw, err := rc.Get(context.Background(), buildCacheKey(task)).Result()
	require.NoError(t, err)
	require.Equal(t, "newer", raw)
	require.Equal(t, before+1, metricCount(t, "db_ordering_lease_lost_total", map[string]string{"reason": metrics.LeaseLostOwnerChanged}))
}

func TestPlayerCachePublicationEvidenceAndBarrierAreAtomic(t *testing.T) {
	require.NoError(t, SetFencingMode("dual"))
	t.Cleanup(func() { _ = SetFencingMode("legacy") })
	w, lease, task := fencePublishFixture(t)
	row := fenceRow(3)
	row.AssetOpLedger = &componentpb.PlayerAssetOpLedgerComp{}
	require.NoError(t, publishDBWriteResult(context.Background(), lease, task, row))
	proof := assetop.SQLCommitKey(task.Key)
	first, err := w.redisClient.HGet(context.Background(), proof, "data").Bytes()
	require.NoError(t, err)
	require.NoError(t, publishDBWriteResult(context.Background(), lease, task, row), "同版本同证据幂等")
	row.AssetOpLedger.Streams = []*componentpb.AssetOpStreamLedger{{Watermark: 3}}
	rawBefore, err := w.redisClient.Get(context.Background(), buildCacheKey(task)).Bytes()
	require.NoError(t, err)
	require.ErrorIs(t, publishDBWriteResult(context.Background(), lease, task, row), errFenceEvidenceConflict)
	rawAfter, err := w.redisClient.Get(context.Background(), buildCacheKey(task)).Bytes()
	require.NoError(t, err)
	require.Equal(t, rawBefore, rawAfter)
	after, err := w.redisClient.HGet(context.Background(), proof, "data").Bytes()
	require.NoError(t, err)
	require.Equal(t, first, after)
	version, err := w.redisClient.HGet(context.Background(), savebarrier.Key(task.Key), "player_database").Result()
	require.NoError(t, err)
	require.Equal(t, assetop.SQLCommitVersion(7, 9), version)
}

func TestAppliedAdvanceRejectsMalformedEpochBeforeCursorWrite(t *testing.T) {
	w, lease, task := fencePublishFixture(t)
	key := appliedEpochKey(w.topic, 42, task.MsgType)
	require.NoError(t, w.redisClient.Set(context.Background(), key, "01", 0).Err())
	wt := &workerTask{seq: 5, originPartition: 0, hasOriginPartition: true}
	require.Error(t, advanceApplied(context.Background(), lease, w.topic, task, wt))
	require.Zero(t, w.redisClient.Exists(context.Background(), appliedSeqKey(w.topic, 42, task.MsgType)).Val())
}

func TestReadCachePublicationNXPreservesConcurrentWrite(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	task := &dbpb.DBTask{Key: 42, Op: dbpb.DBOp_DB_OP_READ, MsgType: "player_database", TaskId: "read-nx"}
	row := fenceRow(9)
	raw, err := proto.Marshal(row)
	require.NoError(t, err)
	require.NoError(t, w.redisClient.Set(context.Background(), buildCacheKey(task), raw, 0).Err())
	require.NoError(t, publishDBReadResult(context.Background(), w.redisClient, task, fenceRow(1)))
	actual, err := w.redisClient.Get(context.Background(), buildCacheKey(task)).Bytes()
	require.NoError(t, err)
	require.Equal(t, raw, actual)
	result, ok := readTaskResult(t, w, task.TaskId)
	require.True(t, ok)
	read := fenceRow(0)
	require.NoError(t, proto.Unmarshal(result.Data, read))
	require.Equal(t, uint64(1), read.Currency.Values[1], "SELECT应答不得换成NX赢家的缓存")
}

func TestPlayerCachePublicationAheadKeepsProofAndMonotoneBarrier(t *testing.T) {
	require.NoError(t, SetFencingMode("dual"))
	t.Cleanup(func() { _ = SetFencingMode("legacy") })
	w, lease, task := fencePublishFixture(t)
	proof := assetop.SQLCommitKey(task.Key)
	require.NoError(t, w.redisClient.HSet(w.ctx, proof, "version", savebarrier.Version(7, 10), "data", "newer-proof").Err())
	require.NoError(t, w.redisClient.HSet(w.ctx, savebarrier.Key(task.Key), "pending", savebarrier.Version(7, 10), "player_database", savebarrier.Version(7, 1)).Err())
	require.NoError(t, publishDBWriteResult(w.ctx, lease, task, fenceRow(3)))
	require.Equal(t, "newer-proof", w.redisClient.HGet(w.ctx, proof, "data").Val())
	require.Equal(t, savebarrier.Version(7, 10), w.redisClient.HGet(w.ctx, proof, "version").Val())
	require.Equal(t, savebarrier.Version(7, 9), w.redisClient.HGet(w.ctx, savebarrier.Key(task.Key), "player_database").Val())
	require.Equal(t, savebarrier.Version(7, 10), w.redisClient.HGet(w.ctx, savebarrier.Key(task.Key), "pending").Val())
	require.Equal(t, int64(1), w.redisClient.Exists(w.ctx, buildCacheKey(task)).Val())
}

func TestPlayerCachePublicationMalformedBarrierCannotPartiallyWrite(t *testing.T) {
	require.NoError(t, SetFencingMode("dual"))
	t.Cleanup(func() { _ = SetFencingMode("legacy") })
	w, lease, task := fencePublishFixture(t)
	require.NoError(t, w.redisClient.HSet(w.ctx, savebarrier.Key(task.Key), task.MsgType, "18446744073709551616:00000000000000000001").Err())
	require.Error(t, publishDBWriteResult(w.ctx, lease, task, fenceRow(3)))
	require.Zero(t, w.redisClient.Exists(w.ctx, buildCacheKey(task)).Val())
	require.Zero(t, w.redisClient.Exists(w.ctx, assetop.SQLCommitKey(task.Key)).Val())
	require.NoError(t, lease.lost(), "数据格式错误不能伪判令牌易主")
}

// ackAheadCounts 读 db_save_barrier_ack_ahead_total 的两个 kind。计数器是进程级的,用例只比较前后差值。
func ackAheadCounts(t *testing.T) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, kind := range []string{metrics.SaveBarrierAckAbovePending, metrics.SaveBarrierAckPendingMissing} {
		out[kind] = metricCount(t, "db_save_barrier_ack_ahead_total", map[string]string{"kind": kind})
	}
	return out
}

// G2(docs/design/zone-redis-regression-guard-20261022.md §4.2):ACK 是已提交的 SQL 事实,领先 pending 也照写;
// 只有本次真正写下的 ACK 领先 pending(或 pending 缺失)才带回标志并计数。发布 ACK 从不改动、也不补种 pending。
func TestPlayerCachePublicationFlagsAckAheadOfPending(t *testing.T) {
	published := savebarrier.Version(7, 9) // fencePublishFixture 任务的版本
	cases := []struct {
		name      string
		pending   string // 空串 = 屏障里没有 pending
		storedAck string // 空串 = 该表还没有 ACK
		wantAck   string
		wantKind  string // 空串 = 不应带回标志
	}{
		{name: "pending equals published", pending: published, storedAck: savebarrier.Version(7, 1), wantAck: published},
		{name: "pending above published", pending: savebarrier.Version(7, 10), storedAck: savebarrier.Version(7, 1), wantAck: published},
		{name: "published seq above pending", pending: savebarrier.Version(7, 8), storedAck: savebarrier.Version(7, 1), wantAck: published, wantKind: metrics.SaveBarrierAckAbovePending},
		{name: "published epoch above pending", pending: savebarrier.Version(6, 99), wantAck: published, wantKind: metrics.SaveBarrierAckAbovePending},
		{name: "pending missing", storedAck: savebarrier.Version(7, 1), wantAck: published, wantKind: metrics.SaveBarrierAckPendingMissing},
		{name: "pending and ack both missing", wantAck: published, wantKind: metrics.SaveBarrierAckPendingMissing},
		{name: "newer stored ack is kept and not flagged", storedAck: savebarrier.Version(7, 10), wantAck: savebarrier.Version(7, 10)},
		{name: "equal stored ack is not rewritten or flagged", pending: savebarrier.Version(7, 8), storedAck: published, wantAck: published},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useDualFencing(t)
			w, lease, task := fencePublishFixture(t)
			barrier := savebarrier.Key(task.Key)
			if tc.pending != "" {
				require.NoError(t, w.redisClient.HSet(w.ctx, barrier, "pending", tc.pending).Err())
			}
			if tc.storedAck != "" {
				require.NoError(t, w.redisClient.HSet(w.ctx, barrier, task.MsgType, tc.storedAck).Err())
			}
			want := ackAheadCounts(t)
			if tc.wantKind != "" {
				want[tc.wantKind]++
			}

			require.NoError(t, publishDBWriteResult(w.ctx, lease, task, fenceRow(3)))

			require.Equal(t, want, ackAheadCounts(t))
			require.Equal(t, tc.wantAck, w.redisClient.HGet(w.ctx, barrier, task.MsgType).Val())
			if tc.pending == "" {
				require.False(t, w.redisClient.HExists(w.ctx, barrier, "pending").Val(), "发布 ACK 不得补种 pending")
			} else {
				require.Equal(t, tc.pending, w.redisClient.HGet(w.ctx, barrier, "pending").Val(), "发布 ACK 不得改动 pending")
			}
			require.Equal(t, int64(1), w.redisClient.Exists(w.ctx, buildCacheKey(task)).Val())
			require.NoError(t, lease.lost())
		})
	}
}

// 证据领先与 ACK 领先互相独立:同一次发布两件都报、各自计数,证据不回退,ACK 照写。
func TestPlayerCachePublicationReportsEvidenceAheadAndAckAheadTogether(t *testing.T) {
	useDualFencing(t)
	w, lease, task := fencePublishFixture(t)
	proof := assetop.SQLCommitKey(task.Key)
	require.NoError(t, w.redisClient.HSet(w.ctx, proof, "version", savebarrier.Version(7, 10), "data", "newer-proof").Err())
	evidenceAheadLabels := map[string]string{"path": "write", "result": "evidence_ahead"}
	evidenceAhead := metricCount(t, "db_fence_cache_publish_total", evidenceAheadLabels)
	want := ackAheadCounts(t)
	want[metrics.SaveBarrierAckPendingMissing]++

	require.NoError(t, publishDBWriteResult(w.ctx, lease, task, fenceRow(3)))

	require.Equal(t, evidenceAhead+1, metricCount(t, "db_fence_cache_publish_total", evidenceAheadLabels))
	require.Equal(t, want, ackAheadCounts(t))
	require.Equal(t, "newer-proof", w.redisClient.HGet(w.ctx, proof, "data").Val())
	require.Equal(t, savebarrier.Version(7, 9), w.redisClient.HGet(w.ctx, savebarrier.Key(task.Key), task.MsgType).Val())
}

// pending 畸形与已存 ACK 畸形同口径:在任何写之前报错,缓存、证据、ACK 一个都不写,不计任何结局,也不伪判失租。
// 即使本次 ACK 不需要写(已存 ACK 更高),畸形的 pending 也要报出来。
func TestPlayerCachePublicationMalformedPendingCannotPartiallyWrite(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pending   string
		storedAck string
	}{
		{name: "uint64 overflow", pending: "18446744073709551616:00000000000000000001", storedAck: savebarrier.Version(7, 1)},
		{name: "not fixed width", pending: "7:9", storedAck: savebarrier.Version(7, 1)},
		{name: "empty string", pending: "", storedAck: savebarrier.Version(7, 1)},
		{name: "checked even when the ack is kept", pending: "7:9", storedAck: savebarrier.Version(7, 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useDualFencing(t)
			w, lease, task := fencePublishFixture(t)
			barrier := savebarrier.Key(task.Key)
			require.NoError(t, w.redisClient.HSet(w.ctx, barrier, "pending", tc.pending, task.MsgType, tc.storedAck).Err())
			acks := ackAheadCounts(t)
			writtenLabels := map[string]string{"path": "write", "result": "written"}
			written := metricCount(t, "db_fence_cache_publish_total", writtenLabels)

			err := publishDBWriteResult(w.ctx, lease, task, fenceRow(3))

			require.ErrorIs(t, err, errFenceRedis)
			require.ErrorContains(t, err, "stored save barrier pending is malformed")
			require.Zero(t, w.redisClient.Exists(w.ctx, buildCacheKey(task)).Val())
			require.Zero(t, w.redisClient.Exists(w.ctx, assetop.SQLCommitKey(task.Key)).Val())
			require.Equal(t, tc.storedAck, w.redisClient.HGet(w.ctx, barrier, task.MsgType).Val())
			require.Equal(t, tc.pending, w.redisClient.HGet(w.ctx, barrier, "pending").Val())
			require.Equal(t, acks, ackAheadCounts(t))
			require.Equal(t, written, metricCount(t, "db_fence_cache_publish_total", writtenLabels))
			require.NoError(t, lease.lost(), "数据格式错误不能伪判令牌易主")
		})
	}
}

// 发布脚本与 Go 解码之间的整数约定:Lua 里的字面量 10 / 20 与 ackAhead* 一一对应;越出约定的取值一律报错,不猜。
func TestDecodeCachePublishReply(t *testing.T) {
	for _, tc := range []struct{ reply, outcome, ackAhead int64 }{
		{0, cachePublishNotOwner, ackAheadNone},
		{-1, cachePublishEvidenceConflict, ackAheadNone},
		{1, cachePublishWritten, ackAheadNone},
		{2, cachePublishEvidenceAhead, ackAheadNone},
		{11, cachePublishWritten, ackAheadAbovePending},
		{12, cachePublishEvidenceAhead, ackAheadAbovePending},
		{21, cachePublishWritten, ackAheadPendingMissing},
		{22, cachePublishEvidenceAhead, ackAheadPendingMissing},
	} {
		outcome, ackAhead, err := decodeCachePublishReply(tc.reply)
		require.NoError(t, err, "reply %d", tc.reply)
		require.Equal(t, tc.outcome, outcome, "reply %d", tc.reply)
		require.Equal(t, tc.ackAhead, ackAhead, "reply %d", tc.reply)
	}
	for _, reply := range []int64{-11, -2, 3, 9, 10, 13, 20, 31, 32} {
		_, _, err := decodeCachePublishReply(reply)
		require.Error(t, err, "reply %d", reply)
	}
}

func TestAppliedAdvanceUsesExactUint64MaxWithoutEpochRegression(t *testing.T) {
	w, lease, task := fencePublishFixture(t)
	task.OwnerEpoch = ^uint64(0)
	require.NoError(t, advanceApplied(w.ctx, lease, w.topic, task, &workerTask{seq: 12, hasOriginPartition: true, originPartition: 0}))
	require.Equal(t, "18446744073709551615", w.redisClient.Get(w.ctx, appliedEpochKey(w.topic, task.Key, task.MsgType)).Val())
	task.OwnerEpoch--
	require.NoError(t, advanceApplied(w.ctx, lease, w.topic, task, &workerTask{seq: 13, hasOriginPartition: true, originPartition: 0}))
	require.Equal(t, "18446744073709551615", w.redisClient.Get(w.ctx, appliedEpochKey(w.topic, task.Key, task.MsgType)).Val())
}
