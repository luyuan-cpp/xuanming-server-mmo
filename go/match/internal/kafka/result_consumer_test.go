package kafka

import (
	"context"
	"errors"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	kafkapb "proto/contracts/kafka"
	"testing"
	"time"
)

type resultReaderFixture struct {
	message            kafkago.Message
	fetched, committed int
	cancel             context.CancelFunc
}

func (r *resultReaderFixture) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	if ctx.Err() != nil {
		return kafkago.Message{}, ctx.Err()
	}
	r.fetched++
	return r.message, nil
}
func (r *resultReaderFixture) CommitMessages(ctx context.Context, _ ...kafkago.Message) error {
	r.committed++
	r.cancel()
	return nil
}

// noArchive 是不关心来源归档的用例用的空操作;生产的归档处理器必需。
func noArchive(context.Context, *kafkapb.BattleResultEvent, []byte, string, int, int64) error {
	return nil
}

func TestGlobalResultsNeverCommitAfterSqlRetryBudgetIsExhausted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, e := proto.Marshal(&kafkapb.BattleResultEvent{BattleId: 7})
	require.NoError(t, e)
	r := &resultReaderFixture{message: kafkago.Message{Value: body}, cancel: cancel}
	attempts := 0
	runResultConsumer(ctx, r, func(context.Context, *kafkapb.BattleResultEvent) error {
		attempts++
		return errors.New("SQL结果未知")
	}, func(context.Context, time.Duration) bool { return true }, noArchive)
	require.Equal(t, 3, attempts)
	require.Equal(t, 1, r.fetched)
	require.Zero(t, r.committed)
}
func TestGlobalResultsRetrySameMessageAndCommitOnlyAfterSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, e := proto.Marshal(&kafkapb.BattleResultEvent{BattleId: 7})
	require.NoError(t, e)
	r := &resultReaderFixture{message: kafkago.Message{Value: body}, cancel: cancel}
	attempts := 0
	runResultConsumer(ctx, r, func(_ context.Context, event *kafkapb.BattleResultEvent) error {
		require.Equal(t, uint64(7), event.BattleId)
		attempts++
		if attempts < 3 {
			return errors.New("SQL暂不可用")
		}
		return nil
	}, func(context.Context, time.Duration) bool { return true }, noArchive)
	require.Equal(t, 3, attempts)
	require.Equal(t, 1, r.committed)
}
func TestGlobalMalformedResultCannotAdvanceOffset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &resultReaderFixture{message: kafkago.Message{Value: []byte{255}}, cancel: cancel}
	runResultConsumer(ctx, r, func(context.Context, *kafkapb.BattleResultEvent) error { t.Fatal("坏消息不能入账"); return nil }, func(context.Context, time.Duration) bool { return true }, noArchive)
	require.Zero(t, r.committed)
}
func TestGlobalCompletionArchiveFailureDoesNotRateOrCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	event := &kafkapb.BattleResultEvent{BattleId: 7, SettlementFormatVersion: 1}
	r := &resultReaderFixture{message: kafkago.Message{Value: mustResultBytes(t, event), Topic: "match-results-g1", Partition: 2, Offset: 17}, cancel: cancel}
	archived := 0
	runResultConsumer(ctx, r, func(context.Context, *kafkapb.BattleResultEvent) error {
		t.Fatal("来源归档失败时不能先评分")
		return nil
	}, func(context.Context, time.Duration) bool { return true }, func(_ context.Context, got *kafkapb.BattleResultEvent, raw []byte, topic string, partition int, offset int64) error {
		archived++
		require.True(t, proto.Equal(event, got))
		require.Equal(t, r.message.Value, raw)
		require.Equal(t, "match-results-g1", topic)
		require.Equal(t, 2, partition)
		require.Equal(t, int64(17), offset)
		return errors.New("来源SQL提交未知")
	})
	require.Equal(t, 3, archived)
	require.Zero(t, r.committed)
}

func TestCompletionArchiveRetryUsesSameRawBeforeRatingAndCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	event := &kafkapb.BattleResultEvent{BattleId: 7, SettlementFormatVersion: 1}
	// 相同标量字段重复出现，合法但不是重序列化后的字节。
	raw := append(mustResultBytes(t, event), byte(8), byte(7))
	r := &resultReaderFixture{message: kafkago.Message{Value: raw, Topic: "results", Partition: 0, Offset: 11}, cancel: cancel}
	archived, rated := 0, 0
	runResultConsumer(ctx, r, func(context.Context, *kafkapb.BattleResultEvent) error {
		require.Equal(t, 3, archived)
		require.Zero(t, r.committed)
		rated++
		return nil
	}, func(context.Context, time.Duration) bool { return true }, func(_ context.Context, _ *kafkapb.BattleResultEvent, got []byte, _ string, _ int, _ int64) error {
		archived++
		require.Equal(t, raw, got)
		if archived < 3 {
			return errors.New("来源暂不可用")
		}
		return nil
	})
	require.Equal(t, 3, archived)
	require.Equal(t, 1, rated)
	require.Equal(t, 1, r.committed)
}

func TestCompletionArchiveReplaysBeforeRetryingRating(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &resultReaderFixture{message: kafkago.Message{Value: mustResultBytes(t, &kafkapb.BattleResultEvent{BattleId: 7})}, cancel: cancel}
	archived, rated := 0, 0
	runResultConsumer(ctx, r, func(context.Context, *kafkapb.BattleResultEvent) error {
		rated++
		require.Equal(t, rated, archived)
		if rated < 3 {
			return errors.New("评分提交未知")
		}
		return nil
	}, func(context.Context, time.Duration) bool { return true }, func(context.Context, *kafkapb.BattleResultEvent, []byte, string, int, int64) error {
		archived++
		return nil
	})
	require.Equal(t, 3, archived)
	require.Equal(t, 1, r.committed)
}

func TestGlobalResultStartupRequiresCompletionRecorderBeforeKafka(t *testing.T) {
	handle := func(context.Context, *kafkapb.BattleResultEvent) error { return nil }
	recorder := func(context.Context, *kafkapb.BattleResultEvent, []byte, string, int, int64) error { return nil }
	config := ResultConsumerConfig{RequireDurability: true}
	require.ErrorContains(t, StartResultConsumer(context.Background(), config, handle, nil), "完整来源归档")
	require.ErrorContains(t, StartResultConsumer(context.Background(), config, nil, recorder), "完整来源归档")
}

func mustResultBytes(t *testing.T, event *kafkapb.BattleResultEvent) []byte {
	t.Helper()
	raw, err := proto.Marshal(event)
	require.NoError(t, err)
	return raw
}
