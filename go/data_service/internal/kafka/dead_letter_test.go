package kafka

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"shared/kafkautil"
)

// fakeDeadLetterSink 记下写进来的死信;fail 非 nil 时第 n 次写(从 1 起)按它的返回决定成败。
type fakeDeadLetterSink struct {
	mu      sync.Mutex
	records []kafkautil.DeadLetterRecord
	writes  int
	fail    func(n int) error
}

func (s *fakeDeadLetterSink) WriteDeadLetter(_ context.Context, record kafkautil.DeadLetterRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.fail != nil {
		if err := s.fail(s.writes); err != nil {
			return err
		}
	}
	s.records = append(s.records, record)
	return nil
}

func (s *fakeDeadLetterSink) written() []kafkautil.DeadLetterRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]kafkautil.DeadLetterRecord(nil), s.records...)
}

func (s *fakeDeadLetterSink) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// 死信一直写不进(Kafka 故障、死信 topic 没建成):有界重试之后整批不提交、消费者停下,好消息也不落库(死信写在最前面),
// 与落库失败同一个口径;重建之后从上次提交处重放。改之前坏消息只记日志就提交越过,原件只在原 topic 保留期内取得到。
func TestTxLogConsumer_StopsWithoutCommitWhenDeadLetterKeepsFailing(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeTxSink{}
	c := NewTxLogConsumer(TxLogConsumerConfig{BatchSize: 2, FlushInterval: time.Hour}, reader, sink)
	dlq := &fakeDeadLetterSink{fail: func(int) error { return errors.New("kafka: broker unreachable") }}
	c.deadLetter = deadLetterTarget{sink: dlq, group: "txlog-group", attempts: 3, backoff: time.Millisecond}

	done := runTxLog(t, context.Background(), c)
	reader.push(badMsg(0, 1), txMsg(t, 0, 2, validTx(9)))

	err := waitDone(t, done)
	require.ErrorIs(t, err, ErrConsumerStoppedOnDeadLetter)
	assert.Equal(t, 3, dlq.writeCount(), "一条坏消息有界重试 3 次")
	assert.Empty(t, reader.commitCalls(), "写不进死信:这一批什么都不提交")
	assert.Equal(t, 0, sink.callCount(), "死信写在落库之前:好消息也先不落")
}

// 快照同样:批量模式下坏消息先写死信,之后按前缀提交的任何一段里,坏消息都已经在死信里;写不进就整批不提交。
func TestSnapshotConsumer_StopsWithoutCommitWhenDeadLetterKeepsFailing(t *testing.T) {
	reader := newFakeReader(nil)
	sink := &fakeSnapSink{}
	c := NewSnapshotConsumer(SnapshotConsumerConfig{BatchMaxRows: 3, FlushInterval: time.Hour}, reader, sink)
	dlq := &fakeDeadLetterSink{fail: func(int) error { return errors.New("kafka: not enough replicas") }}
	c.deadLetter = deadLetterTarget{sink: dlq, group: "snapshot-group", attempts: 2, backoff: time.Millisecond}

	done := runSnapshot(t, context.Background(), c)
	reader.push(snapMsg(t, 0, 1, validSnap(10)), badMsg(0, 2), snapMsg(t, 0, 3, validSnap(11)))

	err := waitDone(t, done)
	require.ErrorIs(t, err, ErrConsumerStoppedOnDeadLetter)
	assert.Equal(t, 2, dlq.writeCount())
	assert.Empty(t, reader.commitCalls())
	assert.Equal(t, 0, sink.callCount())
}

// 死信里存的是原件的副本:reader 复用缓冲,批里的消息随后还会被剥掉 Value / Key / Headers。
func TestNewBadMessageCopiesTheOriginal(t *testing.T) {
	value := []byte{1, 2, 3}
	msg := kafkago.Message{Topic: "audit", Partition: 2, Offset: 41, Key: []byte("k"), Value: value,
		Headers: []kafkago.Header{{Key: "traceparent", Value: []byte("00-a-b-01")}}}
	bad := newBadMessage(msg, "decode_error: x")
	value[0] = 9
	msg.Headers[0].Value[0] = 'x'
	assert.Equal(t, []byte{1, 2, 3}, bad.original.Value)
	assert.Equal(t, "00-a-b-01", string(bad.original.Headers[0].Value))
	assert.Equal(t, int64(41), bad.original.Offset)
	assert.Equal(t, "decode_error: x", bad.reason)
}
