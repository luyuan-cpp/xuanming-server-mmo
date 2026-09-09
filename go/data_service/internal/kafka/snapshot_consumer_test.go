package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/store"

	rollbackpb "proto/common/rollback"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func runSnapshot(t *testing.T, ctx context.Context, c *SnapshotConsumer) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return done
}

func TestSnapshotConsumer_InsertsSource1RowThenCommits(t *testing.T) {
	log := &eventLog{}
	reader := newFakeReader(log)
	sink := &fakeSnapSink{log: log}
	c := NewSnapshotConsumer(SnapshotConsumerConfig{}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSnapshot(t, ctx, c)

	entry := validSnap(777)
	msg := snapMsg(t, 1, 40, entry)
	reader.push(msg)

	require.Eventually(t, func() bool { return sink.rowCount() == 1 }, eventually, 10*time.Millisecond)
	row := sink.row(0)
	assert.Equal(t, store.SnapshotSourceSceneKafka, row.Source)
	assert.Equal(t, uint64(777), row.SnapshotGuid)
	assert.Equal(t, uint64(100), row.PlayerID)
	assert.Equal(t, uint32(4), row.ZoneID, "zone_id comes from the message, not the router")
	assert.Equal(t, uint64(1700000123), row.CreatedAt, "created_at = snapshot_time")
	assert.Equal(t, uint32(rollbackpb.SnapshotTrigger_SNAPSHOT_LOGIN), row.SnapshotType, "snapshot_type holds the raw SnapshotTrigger")
	assert.Equal(t, "cpp:SNAPSHOT_LOGIN", row.Reason)
	assert.Equal(t, store.SnapshotOperatorSceneNode, row.Operator)
	assert.Equal(t, msg.Value, row.Data, "data is the serialized PlayerSnapshotEntry as received")

	// data 列能原样解回 PlayerSnapshotEntry(将来的 source=1 恢复路径依赖这一点)。
	decoded := &rollbackpb.PlayerSnapshotEntry{}
	require.NoError(t, proto.Unmarshal(row.Data, decoded))
	assert.Equal(t, []byte("blob-a"), decoded.GetPlayerDatabaseBlob())
	assert.Equal(t, "v1", decoded.GetSchemaVersion())

	require.Eventually(t, func() bool { return reader.committedMax()[1] == 40 }, eventually, 10*time.Millisecond)
	assert.Equal(t, []string{"insert", "commit"}, log.snapshot())

	cancel()
	assert.NoError(t, waitDone(t, done))
}

func TestSnapshotConsumer_DuplicateGuidStillCommits(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeSnapSink{duplicate: true}
	c := NewSnapshotConsumer(SnapshotConsumerConfig{}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSnapshot(t, ctx, c)

	reader.push(snapMsg(t, 0, 1, validSnap(5)), snapMsg(t, 0, 2, validSnap(5)))

	require.Eventually(t, func() bool { return reader.committedMax()[0] == 2 }, eventually, 10*time.Millisecond)
	assert.Equal(t, 2, sink.callCount(), "dedup is the sink's check-then-insert; consumer always asks")

	cancel()
	assert.NoError(t, waitDone(t, done))
}

func TestSnapshotConsumer_SkipsBadMessagesAndCommitsThem(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeSnapSink{}
	c := NewSnapshotConsumer(SnapshotConsumerConfig{MaxBlobBytes: 64}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSnapshot(t, ctx, c)

	noGuid := validSnap(0)
	noPlayer := validSnap(8)
	noPlayer.PlayerId = 0
	oversize := validSnap(9)
	oversize.PlayerDatabaseBlob = make([]byte, 200)

	reader.push(
		badMsg(0, 1),
		snapMsg(t, 0, 2, noGuid),
		snapMsg(t, 0, 3, noPlayer),
		snapMsg(t, 0, 4, oversize),
		snapMsg(t, 0, 5, validSnap(10)),
	)

	require.Eventually(t, func() bool { return reader.committedMax()[0] == 5 }, eventually, 10*time.Millisecond)
	assert.Equal(t, 1, sink.callCount(), "only the valid message reaches the sink")
	assert.Equal(t, uint64(10), sink.row(0).SnapshotGuid)
	assert.Len(t, reader.commitCalls(), 5, "every message, good or bad, is committed individually")

	cancel()
	assert.NoError(t, waitDone(t, done))
}

func TestSnapshotConsumer_StopsWithoutCommitWhenDBKeepsFailing(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeSnapSink{alwaysFail: true}
	c := NewSnapshotConsumer(SnapshotConsumerConfig{DBMaxAttempts: 2, DBRetryBackoff: time.Millisecond}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSnapshot(t, ctx, c)

	reader.push(snapMsg(t, 0, 1, validSnap(1)))

	err := waitDone(t, done)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConsumerStoppedOnDB))
	assert.Equal(t, 2, sink.callCount())
	assert.Empty(t, reader.commitCalls())
	assert.True(t, c.Health().StoppedOnError())
	assert.ErrorIs(t, c.Health().LastError(), errDBDown)
}

func TestSnapshotConsumer_TransientDBErrorRecovers(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeSnapSink{failFirst: 1}
	c := NewSnapshotConsumer(SnapshotConsumerConfig{DBMaxAttempts: 3, DBRetryBackoff: time.Millisecond}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSnapshot(t, ctx, c)

	reader.push(snapMsg(t, 0, 1, validSnap(1)))

	require.Eventually(t, func() bool { return reader.committedMax()[0] == 1 }, eventually, 10*time.Millisecond)
	assert.Equal(t, 2, sink.callCount())
	assert.Equal(t, 1, sink.rowCount())
	assert.True(t, c.Health().Healthy())

	cancel()
	assert.NoError(t, waitDone(t, done))
}

func TestStartConfig_Validate(t *testing.T) {
	good := StartConfig{
		Brokers:    []string{"127.0.0.1:9092"},
		TxLogTopic: "t", TxLogPartitions: 6, TxLogGroup: "g",
		SnapshotTopic: "s", SnapshotPartitions: 3, SnapshotGroup: "sg",
	}
	assert.NoError(t, good.validate())

	noBrokers := good
	noBrokers.Brokers = nil
	assert.Error(t, noBrokers.validate())

	noGroup := good
	noGroup.SnapshotGroup = ""
	assert.Error(t, noGroup.validate())

	zeroPartitions := good
	zeroPartitions.TxLogPartitions = 0
	assert.Error(t, zeroPartitions.validate())

	// 没有 store 就不能起消费者(fail-closed),且不碰 Kafka。
	_, err := Start(context.Background(), good, nil, nil)
	assert.Error(t, err)
}
