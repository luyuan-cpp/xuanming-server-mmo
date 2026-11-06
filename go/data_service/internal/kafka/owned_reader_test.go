package kafka

import (
	"context"
	"errors"
	"testing"

	"shared/drainproof"
	"shared/kafkautil"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
)

// groupReaderStub 是消费组 owner 在 ownedReader 下面的替身:FetchMessage 吐出预置消息,CommitMessages 的结局由用例决定。
// 只被测试 goroutine 调用。
type groupReaderStub struct {
	msgs   chan kafkago.Message
	commit func(msgs []kafkago.Message) error
}

func (s *groupReaderStub) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	select {
	case <-ctx.Done():
		return kafkago.Message{}, ctx.Err()
	case msg := <-s.msgs:
		return msg, nil
	}
}

func (s *groupReaderStub) CommitMessages(_ context.Context, msgs ...kafkago.Message) error {
	return s.commit(msgs)
}

func (s *groupReaderStub) Close() error { return nil }

func snapshotPosition(offset int64) kafkago.Message {
	return kafkago.Message{Topic: "player_snapshot", Partition: 0, Offset: offset}
}

// r2-kc#1:消费组 owner 把一批交还重投(再均衡时提交被拒)不是未知结局:不 hold、不把同 boot 的兄弟消费者一起排空,
// 只把这些位置从在途集合与票据里摘掉;同 T/P/O 重投时新建票据、正常结清,排空收据成立。
// 修复前:任何提交错误都 hold → BeginDrain,transaction_log 与 player_snapshot 两条落库消费者一起永久停止。
func TestOwnedReader_ReleasedCommitKeepsOwnerRunning(t *testing.T) {
	relay, ingress := drainproof.New(), drainproof.New()
	owner, err := NewConsumerOwner(relay, ingress)
	require.NoError(t, err)
	stub := &groupReaderStub{msgs: make(chan kafkago.Message, 4)}
	reader := &ownedReader{reader: stub, owner: owner}
	ctx := context.Background()

	fetchBoth := func() {
		stub.msgs <- snapshotPosition(10)
		stub.msgs <- snapshotPosition(11)
		for i := 0; i < 2; i++ {
			_, err := reader.FetchMessage(ctx)
			require.NoError(t, err)
		}
	}
	fetchBoth()
	require.Equal(t, uint64(2), relay.Pending())
	require.Equal(t, int64(2), reader.pending.Load())

	stub.commit = func([]kafkago.Message) error {
		return &kafkautil.UncommittedError{Released: []kafkago.Message{snapshotPosition(10), snapshotPosition(11)}, Cause: errors.New("[22] Illegal Generation")}
	}
	err = reader.CommitMessages(ctx, snapshotPosition(11))
	require.ErrorIs(t, err, kafkautil.ErrUncommitted)
	require.False(t, owner.Draining(), "交还重投不能把兄弟消费者一起排空")
	require.Zero(t, relay.Pending(), "交还的位置不能再占着票据")
	require.Zero(t, reader.pending.Load())
	require.Empty(t, reader.active, "交还的位置不能留给 finishAttempt 当本代次票据上报")
	select {
	case held := <-owner.Errors():
		t.Fatalf("交还重投被当成未知结局: %v", held)
	default:
	}

	// 下一代次重投同 T/P/O:新建票据,正常提交结清。
	fetchBoth()
	stub.commit = func([]kafkago.Message) error { return nil }
	require.NoError(t, reader.CommitMessages(ctx, snapshotPosition(11)))
	require.Zero(t, relay.Pending())
	require.Empty(t, reader.active)

	ingress.CloseAdmission()
	digest, err := owner.SettledDigest(ctx)
	require.NoError(t, err, "交还重投之后排空收据必须成立")
	require.Len(t, digest, 32)
}

// 对照:除交还重投之外的提交错误仍是未知结局,hold 并排空(fail-closed 不变)。
func TestOwnedReader_OtherCommitErrorStillHolds(t *testing.T) {
	relay, ingress := drainproof.New(), drainproof.New()
	owner, err := NewConsumerOwner(relay, ingress)
	require.NoError(t, err)
	stub := &groupReaderStub{msgs: make(chan kafkago.Message, 1)}
	reader := &ownedReader{reader: stub, owner: owner}
	stub.msgs <- snapshotPosition(10)
	_, err = reader.FetchMessage(context.Background())
	require.NoError(t, err)

	stub.commit = func([]kafkago.Message) error { return drainproof.ErrHold }
	require.ErrorIs(t, reader.CommitMessages(context.Background(), snapshotPosition(10)), drainproof.ErrHold)
	require.True(t, owner.Draining())
	require.ErrorIs(t, <-owner.Errors(), drainproof.ErrHold)
}
