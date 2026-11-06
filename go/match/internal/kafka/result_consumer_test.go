package kafka

import (
	"context"
	"errors"
	"fmt"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	kafkapb "proto/contracts/kafka"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/kafkautil"
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

// 闸门冻结是搬库窗口:确定没写,不占重试次数,否则窗口稍长消费者就停了。
func TestProcessResultRecordFrozenGateDoesNotUseAttempts(t *testing.T) {
	body, e := proto.Marshal(&kafkapb.BattleResultEvent{BattleId: 7})
	require.NoError(t, e)
	attempts := 0
	err := processResultRecord(context.Background(), kafkago.Message{Value: body}, func(context.Context, *kafkapb.BattleResultEvent) error {
		attempts++
		if attempts <= resultHandleMaxAttempts+2 {
			return fmt.Errorf("commit: %w", authoritysql.ErrWriterFrozen)
		}
		return nil
	}, func(context.Context, time.Duration) bool { return true }, noArchive)
	require.NoError(t, err)
	require.Equal(t, resultHandleMaxAttempts+3, attempts)

	// 预算用完(等待返回 false)就停,错误带着冻结和超时,不算确定没写:消费者的 owner 按未知处理。
	err = processResultRecord(context.Background(), kafkago.Message{Value: body}, func(context.Context, *kafkapb.BattleResultEvent) error {
		return authoritysql.ErrWriterFrozen
	}, func(context.Context, time.Duration) bool { return false }, noArchive)
	require.ErrorIs(t, err, authoritysql.ErrWriterFrozen)
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
	// lag 探针的关停 join 缺失同样在碰 Kafka 之前拒绝(没配 broker,走到 EnsureResultTopic 会报另一条错)。
	config.GroupID = "match-rating"
	require.ErrorContains(t, StartResultConsumer(context.Background(), config, handle, recorder), "lag 探针")
}

// lag 探针纳入关停 join:起 goroutine 之前把退出信号交给 BindLagProbe;ctx 结束后探针退出、信号关闭(svc 的 StopAndReport 等的就是它)。
// 登记被拒就不起探针。首轮探测在 30s 之后,用例在那之前结束 ctx,不连 Kafka;time.After 只是防挂死的护栏。
func TestStartResultLagProbeIsJoinedAtShutdown(t *testing.T) {
	var bound <-chan struct{}
	cfg := ResultConsumerConfig{Brokers: []string{"127.0.0.1:1"}, Topic: "match-results", GroupID: "match-rating", BindLagProbe: func(done <-chan struct{}) error {
		bound = done
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, startResultLagProbe(ctx, cfg))
	require.NotNil(t, bound, "起探针之前必须登记退出信号")
	select {
	case <-bound:
		t.Fatal("ctx 未结束, 探针不能先退出")
	default:
	}
	cancel()
	select {
	case <-bound:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 结束后探针必须退出并关闭退出信号")
	}
	refused := cfg
	refused.BindLagProbe = func(<-chan struct{}) error { return drainproof.ErrHold }
	require.ErrorIs(t, startResultLagProbe(context.Background(), refused), drainproof.ErrHold)
}

// 探测成功的一轮把 CommitStall 写进 match_result_consumer_commit_stall_seconds(MatchResultConsumerLagging 按它判);
// 探失败的一轮保留上一次的值(陈值由 MatchResultLagProbeBlind 点名)。
func TestObserveResultLagWritesCommitStallAndKeepsItOnError(t *testing.T) {
	target := kafkautil.GroupLagTarget{Group: "match-rating", Topic: "match-results", Partitions: 3}
	observeResultLag(target, kafkautil.GroupLag{Lag: 4, CommitStall: 901 * time.Second}, nil)
	require.Equal(t, 901.0, metrics.ResultConsumerCommitStallValue())
	observeResultLag(target, kafkautil.GroupLag{}, errors.New("[16] Coordinator Not Available"))
	require.Equal(t, 901.0, metrics.ResultConsumerCommitStallValue(), "探失败的一轮不改 gauge")
	observeResultLag(target, kafkautil.GroupLag{Lag: 4}, nil)
	require.Zero(t, metrics.ResultConsumerCommitStallValue(), "位点在前进")
}

func mustResultBytes(t *testing.T, event *kafkapb.BattleResultEvent) []byte {
	t.Helper()
	raw, err := proto.Marshal(event)
	require.NoError(t, err)
	return raw
}
