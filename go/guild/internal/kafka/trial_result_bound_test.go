package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	"shared/drainproof"
	"shared/kafkautil"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"

	kafkapb "proto/contracts/kafka"
)

// boundTrialHarness 是生产路径(RelayAdmission 非 nil → kafkautil.BoundConsumerGroup)的回调夹具:
// 构造不连 Kafka,直接调交给 owner 的回调 handleBound。
type boundTrialHarness struct {
	consumer     *TrialResultConsumer
	relay        *drainproof.Session
	handleCalls  int
	handleErr    error
	retries      int
	decodeErrors int
}

func newBoundTrialHarness(t *testing.T) *boundTrialHarness {
	t.Helper()
	h := &boundTrialHarness{relay: drainproof.New()}
	cfg := trialConsumerConfig()
	cfg.RelayAdmission = h.relay
	consumer, err := NewTrialResultConsumer(cfg, func(context.Context, *kafkapb.BattleResultEvent) error {
		h.handleCalls++
		return h.handleErr
	}, TrialResultCounters{
		DecodeError:  func() { h.decodeErrors++ },
		HandlerRetry: func() { h.retries++ },
	})
	require.NoError(t, err)
	require.NotNil(t, consumer.Owner(), "生产路径必须装配消费组 owner")
	require.NotNil(t, consumer.deadLetter, "生产路径必须装配死信写入端(有界重试后转死信)")
	h.consumer = consumer
	return h
}

// r2-kc#1:生产路径每次回调只试一次,失败返回 error 交给 owner 退避重调(不在回调里循环到 HandlerTimeout,
// 否则一次再均衡要 5 分钟才放手);暂时失败不记 HOLD。
func TestTrialConsumer_BoundHandlerMakesOneAttemptAndLeavesRetryToOwner(t *testing.T) {
	h := newBoundTrialHarness(t)
	h.handleErr = errTransient
	err := h.consumer.handleBound(context.Background(), trialMessage(t, 5, 9001).msg)
	require.ErrorIs(t, err, errTransient)
	require.Equal(t, 1, h.handleCalls, "回调里不能自己循环重试")
	require.Equal(t, 1, h.retries, "每次失败计一次 handler_retry")

	h.handleErr = nil
	require.NoError(t, h.consumer.handleBound(context.Background(), trialMessage(t, 5, 9001).msg))
	require.Equal(t, 2, h.handleCalls)

	h.relay.CloseAdmission()
	require.NoError(t, h.relay.Wait(context.Background()), "暂时失败不是未知结局,不能记 HOLD")
}

// 生产路径:解不出的消息回包着 ErrUnprocessable 的错误(owner 不重试、直接转死信 guild-trial-dlq,写成功才提交越过),
// 不再被当成已结清直接提交(原件会随 topic 的 7 天保留期消失);非历练结果照旧直接结清。两种都不调 handler。
func TestTrialConsumer_BoundHandlerDeadLettersUndecodableAndSkipsNonTrialResults(t *testing.T) {
	h := newBoundTrialHarness(t)
	err := h.consumer.handleBound(context.Background(), kafkago.Message{Topic: "match-results", Offset: 1, Value: []byte{0xff, 0xff, 0xff}})
	require.ErrorIs(t, err, kafkautil.ErrUnprocessable, "坏消息要交给 owner 转死信,不能当成已结清")
	require.NoError(t, h.consumer.handleBound(context.Background(), resultMessage(t, 2, &kafkapb.BattleResultEvent{BattleId: 77}).msg))
	require.Zero(t, h.handleCalls)
	require.Equal(t, 1, h.decodeErrors)
	require.Zero(t, h.retries, "解不出不是 handler 重调")
}

// 第 5 条:handler panic 只是这一条的一次失败,回错误交给 owner 退避重调(次数到上限转死信),不记排空 HOLD。
// 修复前:panic 时 attempt 直接 relay.Hold,消费不停,但之后的维护排空永远不成立。
func TestTrialConsumer_BoundHandlerPanicIsAFailedAttemptWithoutHold(t *testing.T) {
	h := newBoundTrialHarness(t)
	h.consumer.handle = func(context.Context, *kafkapb.BattleResultEvent) error { panic("nil map write in settlement") }
	err := h.consumer.handleBound(context.Background(), trialMessage(t, 5, 9001).msg)
	require.ErrorIs(t, err, errTrialHandlerPanicked)
	require.NotErrorIs(t, err, kafkautil.ErrUnprocessable, "panic 按暂时失败计次,不是确定无法处理")
	require.Equal(t, 1, h.retries)
	h.relay.CloseAdmission()
	require.NoError(t, h.relay.Wait(context.Background()), "handler panic 不能把排空收据记成 HOLD")
}

// 死信 topic 契约:<GroupID>-dlq、1 个分区、保留 90 天,耐久要求跟结果 topic 走;启动门禁与后台消费者一起确保两个 topic。
func TestTrialConsumer_DeadLetterTopicFollowsResultTopicDurability(t *testing.T) {
	cfg := trialConsumerConfig()
	spec := cfg.DeadLetterTopicSpec()
	require.Equal(t, "guild-trial-dlq", spec.Name)
	require.Equal(t, int32(1), spec.Partitions)
	require.Equal(t, int64(90*24*3600*1000), spec.RetentionMs)
	require.False(t, spec.RequireDurability)
	cfg.RequireDurability = true
	durable := cfg.DeadLetterTopicSpec()
	require.True(t, durable.RequireDurability)
	require.Equal(t, int16(3), durable.ReplicaFactor)
	require.Equal(t, []kafkautil.TopicSpec{cfg.TopicSpec(), durable}, cfg.TopicSpecs())
}

// r2-kc#1:生产路径启动时确保 topic 失败(Kafka 暂不可达)按 trialResultStartRetryInterval 重试、不记 HOLD;
// ctx 先结束就不启动 owner,排空给零接纳收据。修复前:第一次失败就 relay.Hold 并返回,消费者再也不启动。
func TestTrialConsumer_BoundRunRetriesEnsureTopicWithoutHold(t *testing.T) {
	h := newBoundTrialHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ensures := 0
	h.consumer.ensureTopics = func(_ []string, specs []kafkautil.TopicSpec) error {
		ensures++
		require.Equal(t, h.consumer.cfg.TopicSpecs(), specs, "生产路径要同时确保结果 topic 与本组死信 topic")
		return errors.New("kafka: client has run out of available brokers to talk to")
	}
	var sleeps []time.Duration
	h.consumer.sleep = func(sleepCtx context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		if len(sleeps) == 2 {
			cancel() // 第二次退避期间进程退出
		}
		return sleepCtx.Err() == nil
	}
	h.consumer.Run(ctx)
	require.Equal(t, 2, ensures, "确保 topic 失败要重试,不能一次就放弃")
	require.Equal(t, []time.Duration{trialResultStartRetryInterval, trialResultStartRetryInterval}, sleeps)

	raw, err := h.consumer.Owner().CloseAndReport(context.Background())
	require.NoError(t, err, "owner 没启动、没接纳过记录,排空收据必须成立")
	require.Contains(t, string(raw), `"Started":false`)
	h.relay.CloseAdmission()
	require.NoError(t, h.relay.Wait(context.Background()), "确保 topic 失败不是未知结局,不能记 HOLD")
}
