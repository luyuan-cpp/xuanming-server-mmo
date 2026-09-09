package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const eventually = 3 * time.Second

// runTxLog 起 Run,返回 done channel(携带 Run 的返回值)。
func runTxLog(t *testing.T, ctx context.Context, c *TxLogConsumer) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return done
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(eventually):
		t.Fatal("consumer did not stop in time")
		return nil
	}
}

func TestTxLogConsumer_BatchBySize_InsertsThenCommitsHighestOffset(t *testing.T) {
	log := &eventLog{}
	reader := newFakeReader(log)
	sink := &fakeTxSink{log: log}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 3, FlushInterval: 10 * time.Second}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(
		txMsg(t, 0, 10, validTx(1)),
		txMsg(t, 0, 11, validTx(2)),
		txMsg(t, 0, 12, validTx(3)),
	)

	require.Eventually(t, func() bool { return sink.batchCount() == 1 }, eventually, 10*time.Millisecond)
	rows := sink.batch(0)
	require.Len(t, rows, 3)
	assert.Equal(t, uint64(1), rows[0].TxID)
	assert.Equal(t, uint64(3), rows[2].TxID)
	// 字段映射:proto timestamp → 列 timestamp_sec;zone_id 原样;extra 不截断。
	assert.Equal(t, uint64(1700000001), rows[0].Timestamp)
	assert.Equal(t, uint32(4), rows[0].ZoneID)
	assert.Equal(t, uint32(1), rows[0].TxType)
	assert.Equal(t, int64(-50), rows[0].CurrencyDelta)
	assert.Equal(t, `{"k":"v"}`, rows[0].Extra)

	require.Eventually(t, func() bool { return len(reader.commitCalls()) == 1 }, eventually, 10*time.Millisecond)
	calls := reader.commitCalls()
	require.Len(t, calls[0], 1, "only the highest offset per partition is committed")
	assert.Equal(t, int64(12), calls[0][0].Offset)

	// 落库先于提交。
	assert.Equal(t, []string{"insert", "commit"}, log.snapshot())
	assert.True(t, c.Health().Healthy())

	cancel()
	assert.NoError(t, waitDone(t, done))
	assert.False(t, c.Health().Healthy())
	assert.False(t, c.Health().StoppedOnError())
}

func TestTxLogConsumer_BatchByTime_FlushesPartialBatch(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 100, FlushInterval: 50 * time.Millisecond}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(txMsg(t, 2, 5, validTx(1)), txMsg(t, 2, 6, validTx(2)))

	require.Eventually(t, func() bool { return sink.batchCount() == 1 }, eventually, 10*time.Millisecond)
	require.Len(t, sink.batch(0), 2)
	require.Eventually(t, func() bool { return reader.committedMax()[2] == 6 }, eventually, 10*time.Millisecond)

	cancel()
	assert.NoError(t, waitDone(t, done))
}

func TestTxLogConsumer_HighestOffsetPerPartition(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 3, FlushInterval: 10 * time.Second}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(
		txMsg(t, 0, 7, validTx(1)),
		txMsg(t, 1, 3, validTx(2)),
		txMsg(t, 0, 5, validTx(3)), // 同分区更低 offset 排在后面也不该压低提交位
	)

	require.Eventually(t, func() bool { return len(reader.commitCalls()) == 1 }, eventually, 10*time.Millisecond)
	call := reader.commitCalls()[0]
	require.Len(t, call, 2)
	assert.Equal(t, map[int]int64{0: 7, 1: 3}, reader.committedMax())

	cancel()
	assert.NoError(t, waitDone(t, done))
}

func TestTxLogConsumer_SkipsUndecodableAndZeroTxIdButCommitsThem(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 1, FlushInterval: 50 * time.Millisecond}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	zeroID := validTx(0)
	reader.push(
		badMsg(0, 1),               // 解不出
		txMsg(t, 0, 2, validTx(9)), // 好消息,凑满 BatchSize=1 触发落库
		txMsg(t, 0, 3, zeroID),     // tx_id=0,只能靠时间刷新提交
	)

	require.Eventually(t, func() bool { return sink.batchCount() == 1 }, eventually, 10*time.Millisecond)
	require.Len(t, sink.batch(0), 1)
	assert.Equal(t, uint64(9), sink.batch(0)[0].TxID)

	// 坏消息也被提交(offset 3),且没有第二次落库调用。
	require.Eventually(t, func() bool { return reader.committedMax()[0] == 3 }, eventually, 10*time.Millisecond)
	assert.Equal(t, 1, sink.callCount())

	cancel()
	assert.NoError(t, waitDone(t, done))
}

func TestTxLogConsumer_StopsWithoutCommitWhenDBKeepsFailing(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{alwaysFail: true}
	c := NewTxLogConsumer(TxLogConsumerConfig{
		BatchSize: 1, FlushInterval: time.Second,
		DBMaxAttempts: 3, DBRetryBackoff: time.Millisecond,
	}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(txMsg(t, 0, 1, validTx(1)))

	err := waitDone(t, done)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConsumerStoppedOnDB))
	assert.Equal(t, 3, sink.callCount(), "bounded retries")
	assert.Empty(t, reader.commitCalls(), "nothing committed: lag over loss")
	assert.True(t, c.Health().StoppedOnError())
	assert.False(t, c.Health().Healthy())
	assert.ErrorIs(t, c.Health().LastError(), errDBDown)
}

func TestTxLogConsumer_TransientDBErrorIsRetriedThenCommitted(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{failFirst: 2}
	c := NewTxLogConsumer(TxLogConsumerConfig{
		BatchSize: 1, FlushInterval: time.Second,
		DBMaxAttempts: 5, DBRetryBackoff: time.Millisecond,
	}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(txMsg(t, 0, 1, validTx(1)))

	require.Eventually(t, func() bool { return reader.committedMax()[0] == 1 }, eventually, 10*time.Millisecond)
	assert.Equal(t, 3, sink.callCount())
	assert.Equal(t, 1, sink.batchCount())
	assert.True(t, c.Health().Healthy())

	cancel()
	assert.NoError(t, waitDone(t, done))
}

func TestTxLogConsumer_ShutdownFlushesPendingBatch(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 100, FlushInterval: time.Hour}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	done := runTxLog(t, ctx, c)

	reader.push(txMsg(t, 0, 1, validTx(1)))
	// 等消费者把消息取走(channel 清空)再关停。
	require.Eventually(t, func() bool { return len(reader.msgs) == 0 }, eventually, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	cancel()

	assert.NoError(t, waitDone(t, done))
	assert.Equal(t, 1, sink.batchCount())
	assert.Equal(t, int64(1), reader.committedMax()[0])
}

func TestHighestPerPartition(t *testing.T) {
	assert.Nil(t, highestPerPartition(nil))
	got := highestPerPartition([]kafkago.Message{
		{Partition: 0, Offset: 3}, {Partition: 0, Offset: 9}, {Partition: 1, Offset: 2}, {Partition: 0, Offset: 5},
	})
	require.Len(t, got, 2)
	m := map[int]int64{}
	for _, x := range got {
		m[x.Partition] = x.Offset
	}
	assert.Equal(t, map[int]int64{0: 9, 1: 2}, m)
}
