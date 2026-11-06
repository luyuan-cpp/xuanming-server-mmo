package kafka

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"

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

// TestTxLogConsumer_PartitionsBatchAndCommitIndependently:每个分区各攒各的批(2026-11-06 起按分区并行落库)。
// 分区 0 凑满就落库、只提交分区 0 的前缀;分区 1 的批没满、没到点,既不落库也不提交,也不被分区 0 带着提交。
func TestTxLogConsumer_PartitionsBatchAndCommitIndependently(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 2, FlushInterval: time.Hour}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(
		txMsg(t, 0, 7, validTx(1)),
		txMsg(t, 1, 3, validTx(2)),
		txMsg(t, 0, 8, validTx(3)), // 分区 0 凑满 2 行
	)

	require.Eventually(t, func() bool { return len(reader.commitCalls()) == 1 }, eventually, 10*time.Millisecond)
	call := reader.commitCalls()[0]
	require.Len(t, call, 1, "一次提交只交一个分区的一段前缀")
	assert.Equal(t, 0, call[0].Partition)
	assert.Equal(t, int64(8), call[0].Offset)
	require.Equal(t, 1, sink.batchCount())
	assert.Equal(t, []uint64{1, 3}, []uint64{sink.batch(0)[0].TxID, sink.batch(0)[1].TxID}, "分区 0 的批只含分区 0 的行")

	// 给分区 1 足够的机会越位;它的批没满、没到点,不许落库、不许提交。
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, map[int]int64{0: 8}, reader.committedMax())
	assert.Equal(t, 1, sink.batchCount())

	reader.push(txMsg(t, 1, 4, validTx(4)))
	require.Eventually(t, func() bool { return reader.committedMax()[1] == 4 }, eventually, 10*time.Millisecond)
	assert.Equal(t, 2, sink.batchCount())

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

// TestTxLogConsumer_CancelLeavesPendingBatchUncommitted:ctx 结束(进程关停、兄弟分区停机)时手里没落库的批不落不提交,
// Run 返回 nil、不算故障;重建 / 重启后从上次提交处重放,由 tx_id 主键忽略。(2026-11-06 之前这里会用一条独立的 5s ctx
// 尽力落掉最后一批;正常停服现在走维护排空,见下一条用例。)
func TestTxLogConsumer_CancelLeavesPendingBatchUncommitted(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 100, FlushInterval: time.Hour}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(txMsg(t, 0, 1, validTx(1)))
	// 第 2 次 FetchMessage 说明第一条已投进分区 worker 的通道。
	require.Eventually(t, func() bool { return reader.fetches.Load() >= 2 }, eventually, time.Millisecond)
	cancel()

	assert.NoError(t, waitDone(t, done))
	assert.Equal(t, 0, sink.callCount(), "关停不落手里的批")
	assert.Empty(t, reader.commitCalls())
	assert.False(t, c.Health().StoppedOnError(), "关停不是 DB 故障")
}

// drainReader 模拟维护排空:drain 关闭且已排队的消息取完之后,FetchMessage 返回 errFetchDrained(同 ownedReader.drainFetch)。
type drainReader struct {
	*fakeReader
	drain chan struct{}
}

func (r *drainReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	r.fetches.Add(1)
	select {
	case m := <-r.msgs:
		return m, nil
	default:
	}
	select {
	case m := <-r.msgs:
		return m, nil
	case <-r.drain:
		return kafkago.Message{}, errFetchDrained
	case <-ctx.Done():
		return kafkago.Message{}, ctx.Err()
	}
}

// TestTxLogConsumer_DrainFlushesPendingBatchesThenExits:排空时各分区把通道里已接收的消息落完、提交后退出,Run 返回 nil。
// 不等 FlushInterval 到点,也不靠 ctx 取消;坏消息照常随本分区一起提交。
func TestTxLogConsumer_DrainFlushesPendingBatchesThenExits(t *testing.T) {
	reader := &drainReader{fakeReader: newFakeReader(nil), drain: make(chan struct{})}
	sink := &fakeTxSink{}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 100, FlushInterval: time.Hour, MaxInflightFlushes: 2}, reader, sink)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(txMsg(t, 0, 5, validTx(1)), txMsg(t, 1, 9, validTx(2)), badMsg(1, 10))
	// 第 4 次 FetchMessage 说明三条都已投进各自分区的通道。
	require.Eventually(t, func() bool { return reader.fetches.Load() >= 4 }, eventually, time.Millisecond)
	close(reader.drain)

	assert.NoError(t, waitDone(t, done))
	assert.Equal(t, 2, sink.batchCount(), "两个分区各落一批")
	assert.Equal(t, map[int]int64{0: 5, 1: 10}, reader.committedMax())
	assert.False(t, c.Health().StoppedOnError())
}

// gatedTxSink 是并发用例的落库替身:gate 关闭前挡住落库(gateTx 非空时只挡含这些 tx_id 的批),记录同时在途的调用数与峰值;
// 含 failTx 里 tx_id 的批永远失败;含 panicTx 的批 panic。被挡住期间 ctx 结束就返回 ctx 的原因。
type gatedTxSink struct {
	gate    chan struct{}
	gateTx  map[uint64]bool
	failTx  map[uint64]bool
	panicTx uint64

	mu       sync.Mutex
	inflight int
	peakN    int
	entered  int
}

func (s *gatedTxSink) InsertBatchIgnore(ctx context.Context, rows []*store.TransactionLogRow) (int64, error) {
	gated := s.gate != nil && len(s.gateTx) == 0
	failed := false
	for _, r := range rows {
		if s.panicTx != 0 && r.TxID == s.panicTx {
			panic("gatedTxSink: injected panic")
		}
		gated = gated || s.gateTx[r.TxID]
		failed = failed || s.failTx[r.TxID]
	}
	s.mu.Lock()
	s.entered++
	s.inflight++
	s.peakN = max(s.peakN, s.inflight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()
	if gated {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if failed {
		return 0, errDBDown
	}
	return int64(len(rows)), nil
}

func (s *gatedTxSink) inflightNow() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.inflight }
func (s *gatedTxSink) peak() int         { s.mu.Lock(); defer s.mu.Unlock(); return s.peakN }
func (s *gatedTxSink) enteredCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.entered }

// TestTxLogConsumer_InflightFlushesBoundedBySemaphore:MaxInflightFlushes 限住本实例同时落库的分区数;
// =2 时两个分区真的同时在落(按分区并行),=1 时同一时刻只有一个(与改动前一条循环串行落库相同)。
func TestTxLogConsumer_InflightFlushesBoundedBySemaphore(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inflight int
	}{{"inflight=1", 1}, {"inflight=2", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			reader := newFakeReader(nil)
			sink := &gatedTxSink{gate: make(chan struct{})}
			c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 1, FlushInterval: time.Hour, MaxInflightFlushes: tc.inflight}, reader, sink)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runTxLog(t, ctx, c)

			reader.push(txMsg(t, 0, 0, validTx(61)), txMsg(t, 1, 0, validTx(62)))
			require.Eventually(t, func() bool { return reader.fetches.Load() >= 3 }, eventually, 5*time.Millisecond,
				"两条都已投递给各自的分区 worker")
			require.Eventually(t, func() bool { return sink.inflightNow() == tc.inflight }, eventually, 5*time.Millisecond)
			if tc.inflight == 1 {
				// 给第二个分区足够的机会越过信号量;它若越过,峰值就会变成 2。
				time.Sleep(50 * time.Millisecond)
				assert.Equal(t, 1, sink.peak(), "MaxInflightFlushes=1 时同一时刻只能有一个分区在落库")
			}
			close(sink.gate)

			require.Eventually(t, func() bool {
				m := reader.committedMax()
				return m[0] == 0 && m[1] == 0 && len(m) == 2
			}, eventually, 10*time.Millisecond)
			assert.Equal(t, tc.inflight, sink.peak())

			cancel()
			assert.NoError(t, waitDone(t, done))
		})
	}
}

// TestTxLogConsumer_OnePartitionStopStopsAllWithoutCommittingInFlight:一个分区重试用尽 → 整个 Run 停下;
// 另一个分区正在落的批被取消、不提交;Run 返回后不再有 worker 碰库。
func TestTxLogConsumer_OnePartitionStopStopsAllWithoutCommittingInFlight(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &gatedTxSink{
		gate:   make(chan struct{}),
		gateTx: map[uint64]bool{2001: true},
		failTx: map[uint64]bool{1001: true},
	}
	c := NewTxLogConsumer(TxLogConsumerConfig{
		BatchSize: 1, FlushInterval: time.Hour, MaxInflightFlushes: 2,
		DBMaxAttempts: 2, DBRetryBackoff: time.Millisecond,
	}, reader, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(txMsg(t, 1, 0, validTx(2001)))
	require.Eventually(t, func() bool { return sink.inflightNow() == 1 }, eventually, 5*time.Millisecond, "分区 1 的批应已在途")
	reader.push(txMsg(t, 0, 0, validTx(1001)))

	err := waitDone(t, done)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConsumerStoppedOnDB)
	assert.ErrorIs(t, err, errDBDown)
	assert.True(t, c.Health().StoppedOnError())
	assert.Empty(t, reader.commitCalls(), "失败分区与在途分区都不许提交")

	entered := sink.enteredCount()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, entered, sink.enteredCount(), "Run 返回后不许再有 worker 调 sink")
	assert.Equal(t, 0, sink.inflightNow())
}

// TestTxLogConsumer_WorkerPanicStopsRunInsteadOfHanging:sink panic → Run 在时限内返回 errTxLogWorkerAborted
// (supervisor 按 errPartitionWorkerAborted hold 排空框架),该批不提交,落库名额已归还。
func TestTxLogConsumer_WorkerPanicStopsRunInsteadOfHanging(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &gatedTxSink{panicTx: 77}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 1}, reader, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runTxLog(t, ctx, c)

	reader.push(txMsg(t, 0, 0, validTx(77)))

	err := waitDone(t, done)
	assert.ErrorIs(t, err, errTxLogWorkerAborted)
	assert.ErrorIs(t, err, errPartitionWorkerAborted)
	assert.True(t, c.Health().StoppedOnError())
	assert.Empty(t, reader.commitCalls())
	assert.Equal(t, 0, len(c.sem), "panic 展开时落库名额必须归还,否则同一实例重建后会卡在取名额上")
}

func TestTxLogConsumerConfig_DefaultsInflightToOne(t *testing.T) {
	c := NewTxLogConsumer(TxLogConsumerConfig{}, newFakeReader(nil), &fakeTxSink{})
	assert.Equal(t, 1, cap(c.sem), "MaxInflightFlushes 零值 = 1")
	assert.Equal(t, 200, c.cfg.BatchSize)
	assert.Equal(t, 200*time.Millisecond, c.cfg.FlushInterval)
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
