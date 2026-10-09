package kafka

// 历练结果消费者的单测(设计 docs/design/guild-phase2/06-activities.md §6.40)。
// 全部用假 reader、假 sleep、假 ensureTopics:不连 Kafka、不依赖墙钟。
// 断言的都是可观察行为:handler 被调了几次、位点在什么时候提交、退避等了多久、循环何时退出。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/protobuf/proto"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	"shared/kafkautil"
)

// ── 替身 ──────────────────────────────────────────────────────

// fetchStep 是假 reader 一次 FetchMessage 的预设结果:err 非 nil 时返回 err,否则返回 msg。
type fetchStep struct {
	msg kafkago.Message
	err error
}

// fakeTrialReader 按预设顺序吐消息;吐完后取消 ctx 并返回 ctx 的错误(相当于"进程退出")。
// 只被消费循环那一个 goroutine 访问(单测里就是测试 goroutine 自己),不需要同步。
type fakeTrialReader struct {
	steps     []fetchStep
	cancel    context.CancelFunc
	events    *[]string
	commitErr error
	committed []int64
	closed    int
}

func (r *fakeTrialReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	if err := ctx.Err(); err != nil {
		return kafkago.Message{}, err
	}
	if len(r.steps) == 0 {
		r.cancel()
		return kafkago.Message{}, ctx.Err()
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	return step.msg, step.err
}

func (r *fakeTrialReader) CommitMessages(_ context.Context, msgs ...kafkago.Message) error {
	for _, msg := range msgs {
		*r.events = append(*r.events, fmt.Sprintf("commit:%d", msg.Offset))
		r.committed = append(r.committed, msg.Offset)
	}
	return r.commitErr
}

func (r *fakeTrialReader) Close() error {
	r.closed++
	return nil
}

// consumerHarness 把消费者与它的全部替身装在一起。events 是按发生顺序记下的流水:
// "handle:<battle_id>"、"sleep:<时长>"、"commit:<offset>"。
type consumerHarness struct {
	ctx      context.Context
	cancel   context.CancelFunc
	events   []string
	reader   *fakeTrialReader
	consumer *TrialResultConsumer

	// handle 是测试给的 handler 行为;call 从 1 起计。
	handle       func(ctx context.Context, call int, ev *kafkapb.BattleResultEvent) error
	handleCalls  int
	sleeps       []time.Duration
	decodeErrors int
	retries      int
}

func trialConsumerConfig() TrialResultConsumerConfig {
	return TrialResultConsumerConfig{
		Brokers:    []string{"127.0.0.1:9092"},
		Topic:      "match-results",
		GroupID:    "guild-trial",
		Partitions: 3,
	}
}

func newConsumerHarness(t *testing.T, steps ...fetchStep) *consumerHarness {
	t.Helper()
	h := &consumerHarness{}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	h.reader = &fakeTrialReader{steps: steps, cancel: h.cancel, events: &h.events}

	consumer, err := NewTrialResultConsumer(trialConsumerConfig(),
		func(ctx context.Context, ev *kafkapb.BattleResultEvent) error {
			h.handleCalls++
			h.events = append(h.events, fmt.Sprintf("handle:%d", ev.GetBattleId()))
			if h.handle == nil {
				return nil
			}
			return h.handle(ctx, h.handleCalls, ev)
		},
		TrialResultCounters{
			DecodeError:  func() { h.decodeErrors++ },
			HandlerRetry: func() { h.retries++ },
		})
	require.NoError(t, err)
	consumer.sleep = func(ctx context.Context, d time.Duration) bool {
		h.sleeps = append(h.sleeps, d)
		h.events = append(h.events, fmt.Sprintf("sleep:%v", d))
		return ctx.Err() == nil
	}
	consumer.ensureTopics = func([]string, []kafkautil.TopicSpec) error { return nil }
	consumer.openReader = func(TrialResultConsumerConfig) trialReader { return h.reader }
	h.consumer = consumer
	return h
}

// run 跑消费主循环直到假 reader 吐完(或被测行为让它提前退出)。
func (h *consumerHarness) run() {
	h.consumer.consume(h.ctx, h.reader)
}

func trialContext(guildID uint64) *battlepb.BattleActivityContext {
	return &battlepb.BattleActivityContext{
		Kind:              battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL,
		GuildId:           guildID,
		ActivityId:        3,
		PeriodKey:         20261008,
		InitiatorPlayerId: 1001,
		GuildPeriodKey:    20261008,
	}
}

func resultMessage(t *testing.T, offset int64, ev *kafkapb.BattleResultEvent) fetchStep {
	t.Helper()
	value, err := proto.Marshal(ev)
	require.NoError(t, err)
	return fetchStep{msg: kafkago.Message{
		Topic:     "match-results",
		Partition: 1,
		Offset:    offset,
		Key:       []byte(strconv.FormatUint(ev.GetBattleId(), 10)),
		Value:     value,
	}}
}

// trialMessage 是一条字段齐全的帮会历练结果。
func trialMessage(t *testing.T, offset int64, battleID uint64) fetchStep {
	t.Helper()
	return resultMessage(t, offset, &kafkapb.BattleResultEvent{BattleId: battleID, ActivityContext: trialContext(7)})
}

var errTransient = errors.New("mysql: connection refused")

// ── 过滤与提交 ────────────────────────────────────────────────

// §6.40 用例 1:坏字节 → 计 decode_error、提交、不调 handler(留在 topic 里只会每次重启都撞一遍)。
func TestTrialConsumer_UndecodableMessageIsCommittedAndSkipped(t *testing.T) {
	h := newConsumerHarness(t, fetchStep{msg: kafkago.Message{Offset: 10, Key: []byte("x"), Value: []byte{0xff, 0xff, 0xff}}})
	h.run()

	assert.Zero(t, h.handleCalls)
	assert.Equal(t, 1, h.decodeErrors)
	assert.Zero(t, h.retries)
	assert.Equal(t, []string{"commit:10"}, h.events)
}

// §6.40 用例 2:与帮会历练无关的结果(没有活动上下文、kind=NONE)→ 直接提交,不调 handler,也不计任何数。
// match-results 里绝大多数消息走这条路。
func TestTrialConsumer_NonTrialResultsAreCommittedWithoutHandling(t *testing.T) {
	h := newConsumerHarness(t,
		resultMessage(t, 20, &kafkapb.BattleResultEvent{BattleId: 501}),
		resultMessage(t, 21, &kafkapb.BattleResultEvent{BattleId: 502,
			ActivityContext: &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_NONE, GuildId: 7}}),
		// 空 payload 能解成一条全零事件:同样与历练无关。
		fetchStep{msg: kafkago.Message{Offset: 22}},
	)
	h.run()

	assert.Zero(t, h.handleCalls)
	assert.Zero(t, h.decodeErrors)
	assert.Zero(t, h.retries)
	assert.Equal(t, []string{"commit:20", "commit:21", "commit:22"}, h.events)
}

// 上下文残缺但 kind 是历练的结果仍交给 handler(由它判成"上下文不可用"并销账),消费者不悄悄吞掉。
func TestTrialConsumer_TrialWithBrokenContextStillReachesHandler(t *testing.T) {
	h := newConsumerHarness(t, resultMessage(t, 30, &kafkapb.BattleResultEvent{BattleId: 503,
		ActivityContext: &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL}}))
	h.run()

	assert.Equal(t, []string{"handle:503", "commit:30"}, h.events)
}

// handler 回 nil 就提交。§6.40 用例 4 的"毒消息"也是这条路:确定性失败由 handler 自己标成 POISON 后回 nil,
// 消费者看不出也不需要看出区别 —— 消费不会被一条坏结果卡住。
func TestTrialConsumer_HandlerSuccessCommitsEachMessageInOrder(t *testing.T) {
	h := newConsumerHarness(t,
		trialMessage(t, 40, 601),
		resultMessage(t, 41, &kafkapb.BattleResultEvent{BattleId: 602}),
		trialMessage(t, 42, 603),
	)
	h.run()

	assert.Equal(t, []string{"handle:601", "commit:40", "commit:41", "handle:603", "commit:42"}, h.events)
	assert.Zero(t, h.retries)
	assert.Empty(t, h.sleeps)
}

// handler 拿到的事件就是 payload 里的那一条(含活动上下文);key 与 battle_id 不一致只记日志,以 payload 为准。
func TestTrialConsumer_HandlerReceivesDecodedEventAndKeyMismatchIsTolerated(t *testing.T) {
	step := trialMessage(t, 50, 604)
	step.msg.Key = []byte("999")
	h := newConsumerHarness(t, step)
	var got *kafkapb.BattleResultEvent
	h.handle = func(_ context.Context, _ int, ev *kafkapb.BattleResultEvent) error {
		got = ev
		return nil
	}
	h.run()

	require.NotNil(t, got)
	assert.Equal(t, uint64(604), got.GetBattleId())
	assert.True(t, proto.Equal(trialContext(7), got.GetActivityContext()))
	assert.Equal(t, []int64{50}, h.reader.committed)
}

// 提交失败不终止循环:下一次提交会覆盖它,在那之前崩溃则重放一条,由 handler 的幂等闸门兜住。
func TestTrialConsumer_CommitFailureDoesNotStopTheLoop(t *testing.T) {
	h := newConsumerHarness(t, trialMessage(t, 60, 605), trialMessage(t, 61, 606))
	h.reader.commitErr = errors.New("kafka: rebalance in progress")
	h.run()

	assert.Equal(t, []string{"handle:605", "commit:60", "handle:606", "commit:61"}, h.events)
}

// ── 暂时性失败:不提交、退避、重调 ─────────────────────────────

// §6.40 用例 3:handler 前两次回暂时性错误、第三次成功 → 调 3 次、期间一次都不提交、成功后提交 1 次;
// 退避 1s、2s;每次重调计一次 handler_retry。
func TestTrialConsumer_TransientFailureRetriesWithoutCommitting(t *testing.T) {
	h := newConsumerHarness(t, trialMessage(t, 70, 701))
	h.handle = func(_ context.Context, call int, _ *kafkapb.BattleResultEvent) error {
		if call <= 2 {
			return errTransient
		}
		return nil
	}
	h.run()

	assert.Equal(t, []string{"handle:701", "sleep:1s", "handle:701", "sleep:2s", "handle:701", "commit:70"}, h.events)
	assert.Equal(t, 2, h.retries)
	assert.Equal(t, []int64{70}, h.reader.committed)
}

// 退避逐次翻倍、封顶 30s,且没有次数上限:跳过 = 这一局的奖励永久不发。
func TestTrialConsumer_BackoffDoublesAndIsCappedAtThirtySeconds(t *testing.T) {
	const failures = 9
	h := newConsumerHarness(t, trialMessage(t, 80, 702))
	h.handle = func(_ context.Context, call int, _ *kafkapb.BattleResultEvent) error {
		if call <= failures {
			return errTransient
		}
		return nil
	}
	h.run()

	assert.Equal(t, []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
	}, h.sleeps)
	assert.Equal(t, failures+1, h.handleCalls)
	assert.Equal(t, failures, h.retries)
	assert.Equal(t, []int64{80}, h.reader.committed)
}

// 一条消息的退避不带到下一条:每条消息都从 1s 起。
func TestTrialConsumer_BackoffResetsForTheNextMessage(t *testing.T) {
	h := newConsumerHarness(t, trialMessage(t, 90, 703), trialMessage(t, 91, 704))
	failed := map[uint64]int{}
	h.handle = func(_ context.Context, _ int, ev *kafkapb.BattleResultEvent) error {
		if failed[ev.GetBattleId()] < 2 {
			failed[ev.GetBattleId()]++
			return errTransient
		}
		return nil
	}
	h.run()

	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, time.Second, 2 * time.Second}, h.sleeps)
	assert.Equal(t, []int64{90, 91}, h.reader.committed)
}

// 纪律 2 的影响面:消费是单协程串行的,一条消息在重调期间,**别的分区**的消息同样不被处理 ——
// reader 把本进程分到的全部分区汇成一路,卡住的不只是它所在的那个分区(06 §6.35a 场景一按这个影响面写)。
// 这条用例钉住的是现状:要改成按分区隔离,先改文件头纪律 2 与 §6.35a,再改这里。
func TestTrialConsumer_RetryingMessageHoldsBackOtherPartitions(t *testing.T) {
	stuck := trialMessage(t, 95, 713)
	stuck.msg.Partition = 0
	other := trialMessage(t, 7, 714)
	other.msg.Partition = 2
	h := newConsumerHarness(t, stuck, other)
	h.handle = func(_ context.Context, call int, ev *kafkapb.BattleResultEvent) error {
		if ev.GetBattleId() == 713 && call <= 3 {
			return errTransient
		}
		return nil
	}
	h.run()

	assert.Equal(t, []string{
		"handle:713", "sleep:1s", "handle:713", "sleep:2s", "handle:713", "sleep:4s", "handle:713", "commit:95",
		"handle:714", "commit:7",
	}, h.events, "713 成功之前,另一个分区的 714 一次都不该被处理")
	assert.Equal(t, 3, h.retries)
}

// §6.40 用例 5:退避等待中 ctx 结束 → 退出,**不提交**(重启后这条消息重放),也不再拉下一条。
func TestTrialConsumer_CancelDuringBackoffExitsWithoutCommit(t *testing.T) {
	h := newConsumerHarness(t, trialMessage(t, 100, 705), trialMessage(t, 101, 706))
	h.handle = func(context.Context, int, *kafkapb.BattleResultEvent) error { return errTransient }
	h.consumer.sleep = func(context.Context, time.Duration) bool {
		h.events = append(h.events, "sleep")
		h.cancel()
		return false
	}
	h.run()

	assert.Equal(t, []string{"handle:705", "sleep"}, h.events)
	assert.Empty(t, h.reader.committed)
	assert.Len(t, h.reader.steps, 1, "第二条消息不该被拉走")
}

// handler 因为 ctx 结束而回错误:这不是一次"重调",不计数、不等待、不提交。
func TestTrialConsumer_CancelDuringHandlerExitsWithoutCommitOrRetryCount(t *testing.T) {
	h := newConsumerHarness(t, trialMessage(t, 110, 707))
	h.handle = func(ctx context.Context, _ int, _ *kafkapb.BattleResultEvent) error {
		h.cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	h.run()

	assert.Equal(t, []string{"handle:707"}, h.events)
	assert.Zero(t, h.retries)
	assert.Empty(t, h.sleeps)
	assert.Empty(t, h.reader.committed)
}

// handler panic 只算一次暂时性失败:消费循环不死,退避后重调同一条消息。
func TestTrialConsumer_HandlerPanicIsRetriedNotFatal(t *testing.T) {
	logtest.Discard(t)
	h := newConsumerHarness(t, trialMessage(t, 120, 708), trialMessage(t, 121, 709))
	h.handle = func(_ context.Context, call int, _ *kafkapb.BattleResultEvent) error {
		if call == 1 {
			panic("nil map write in settlement")
		}
		return nil
	}
	h.run()

	assert.Equal(t, []string{"handle:708", "sleep:1s", "handle:708", "commit:120", "handle:709", "commit:121"}, h.events)
	assert.Equal(t, 1, h.retries)
}

// 每次调 handler 的 ctx 都带单次上限(某个依赖把调用挂住时,到点按暂时性失败处理,而不是不出声地停住整个消费)。
func TestTrialConsumer_HandlerContextCarriesPerAttemptDeadline(t *testing.T) {
	h := newConsumerHarness(t, trialMessage(t, 130, 710))
	var remaining time.Duration
	var hasDeadline bool
	h.handle = func(ctx context.Context, _ int, _ *kafkapb.BattleResultEvent) error {
		deadline, ok := ctx.Deadline()
		hasDeadline = ok
		remaining = time.Until(deadline)
		return nil
	}
	h.run()

	require.True(t, hasDeadline)
	assert.Greater(t, remaining, time.Duration(0))
	assert.LessOrEqual(t, remaining, trialResultHandleTimeout)
}

// ── 拉取失败 ──────────────────────────────────────────────────

// 拉取失败(broker 抖动、重平衡)固定退避 1s 后接着拉,不退出、不丢消息。
// ctx 没结束而错误恰好是 context.Canceled 的,同样按拉取失败处理 —— 据此退出会让消费者不出声地停掉。
func TestTrialConsumer_FetchErrorBacksOffAndContinues(t *testing.T) {
	h := newConsumerHarness(t,
		fetchStep{err: errors.New("kafka: broker not available")},
		fetchStep{err: context.Canceled},
		trialMessage(t, 140, 711),
	)
	h.run()

	assert.Equal(t, []string{"sleep:1s", "sleep:1s", "handle:711", "commit:140"}, h.events)
	assert.Zero(t, h.retries, "拉取失败不是 handler 重调")
}

// reader 被关闭(FetchMessage 回 io.EOF)→ 循环退出;继续拉只会空转。
func TestTrialConsumer_ClosedReaderEndsTheLoop(t *testing.T) {
	logtest.Discard(t)
	h := newConsumerHarness(t, fetchStep{err: io.EOF}, trialMessage(t, 150, 712))
	h.run()

	assert.Empty(t, h.events)
	assert.Len(t, h.reader.steps, 1)
	assert.NoError(t, h.ctx.Err(), "退出是因为 reader 关闭,不是因为 ctx 结束")
}

// ── Run:确保 topic、建 reader、退出 ───────────────────────────

// Run 先确保 topic(名字、分区数、7 天保留期照配置),成功后才建 reader 并打启动日志;退出前关闭 reader。
// 启动日志是部署验收的判据(06 §6.46 B6b-srv2):topic 与 group 都不带 _zN 后缀。
func TestTrialConsumer_RunEnsuresTopicThenConsumesAndCloses(t *testing.T) {
	logs := logtest.NewCollector(t)
	h := newConsumerHarness(t, trialMessage(t, 160, 801))
	var gotBrokers []string
	var gotSpecs []kafkautil.TopicSpec
	opened := 0
	h.consumer.ensureTopics = func(brokers []string, specs []kafkautil.TopicSpec) error {
		assert.Zero(t, opened, "确保 topic 必须先于建 reader")
		gotBrokers, gotSpecs = brokers, specs
		return nil
	}
	h.consumer.openReader = func(cfg TrialResultConsumerConfig) trialReader {
		opened++
		assert.Equal(t, trialConsumerConfig(), cfg)
		return h.reader
	}

	h.consumer.Run(h.ctx)

	assert.Equal(t, []string{"127.0.0.1:9092"}, gotBrokers)
	assert.Equal(t, []kafkautil.TopicSpec{{Name: "match-results", Partitions: 3, RetentionMs: 7 * 24 * 3600 * 1000}}, gotSpecs)
	assert.Equal(t, 1, opened)
	assert.Equal(t, []string{"handle:801", "commit:160"}, h.events)
	assert.Equal(t, 1, h.reader.closed)
	assert.Contains(t, logs.String(), "历练结果消费者启动 topic=match-results group=guild-trial")
}

// Kafka 未就绪 / 分区数不符:每 30s 重试,不放弃;成功之前不建 reader。确保 topic 时 panic 同样只算一次失败。
func TestTrialConsumer_RunRetriesTopicSetupEveryThirtySeconds(t *testing.T) {
	logtest.Discard(t)
	h := newConsumerHarness(t)
	attempts, opened := 0, 0
	h.consumer.ensureTopics = func([]string, []kafkautil.TopicSpec) error {
		attempts++
		switch attempts {
		case 1:
			return errors.New("kafka admin connect: dial tcp 127.0.0.1:9092: connection refused")
		case 2:
			panic("sarama blew up")
		default:
			return nil
		}
	}
	h.consumer.openReader = func(TrialResultConsumerConfig) trialReader {
		opened++
		assert.Equal(t, 3, attempts, "topic 就绪之前不该建 reader")
		return h.reader
	}

	h.consumer.Run(h.ctx)

	assert.Equal(t, 3, attempts)
	assert.Equal(t, []time.Duration{30 * time.Second, 30 * time.Second}, h.sleeps)
	assert.Equal(t, 1, opened)
	assert.Equal(t, 1, h.reader.closed)
}

// 等待重试期间 ctx 结束 → Run 返回,从不建 reader。
func TestTrialConsumer_RunStopsWhileWaitingForTopic(t *testing.T) {
	logtest.Discard(t)
	h := newConsumerHarness(t)
	h.consumer.ensureTopics = func([]string, []kafkautil.TopicSpec) error { return errors.New("kafka down") }
	h.consumer.sleep = func(context.Context, time.Duration) bool {
		h.cancel()
		return false
	}
	h.consumer.openReader = func(TrialResultConsumerConfig) trialReader {
		t.Error("topic 从未就绪,不该建 reader")
		return h.reader
	}

	h.consumer.Run(h.ctx)

	assert.Zero(t, h.reader.closed)
}

// 确保 topic 的调用卡在管理连接上(它不接受 ctx)时,ctx 结束后 Run 仍立即返回:卡住的调用被丢下,不拖住进程退出。
func TestTrialConsumer_RunDoesNotWaitForStuckTopicSetup(t *testing.T) {
	logtest.Discard(t)
	h := newConsumerHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.consumer.ensureTopics = func([]string, []kafkautil.TopicSpec) error {
		<-release
		return nil
	}
	// 用 t.Error 而不是 t.Fatal:Run 在下面单独的协程里跑,FailNow 只许在测试协程里调。
	h.consumer.openReader = func(TrialResultConsumerConfig) trialReader {
		t.Error("ctx 已结束,不该建 reader")
		return h.reader
	}
	h.cancel()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		h.consumer.Run(h.ctx)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 在 ctx 结束后没有返回:被卡住的确保 topic 调用拖住了")
	}
	assert.Empty(t, h.sleeps)
}

// ── 构造参数 ──────────────────────────────────────────────────

func TestNewTrialResultConsumer_RejectsBadArguments(t *testing.T) {
	handler := func(context.Context, *kafkapb.BattleResultEvent) error { return nil }
	cases := []struct {
		name   string
		mutate func(*TrialResultConsumerConfig)
		handle TrialResultHandler
	}{
		{name: "nil handler", mutate: func(*TrialResultConsumerConfig) {}, handle: nil},
		{name: "no brokers", mutate: func(c *TrialResultConsumerConfig) { c.Brokers = nil }, handle: handler},
		{name: "blank brokers", mutate: func(c *TrialResultConsumerConfig) { c.Brokers = []string{"", "  "} }, handle: handler},
		{name: "empty topic", mutate: func(c *TrialResultConsumerConfig) { c.Topic = " " }, handle: handler},
		{name: "empty group", mutate: func(c *TrialResultConsumerConfig) { c.GroupID = "" }, handle: handler},
		{name: "zero partitions", mutate: func(c *TrialResultConsumerConfig) { c.Partitions = 0 }, handle: handler},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := trialConsumerConfig()
			tc.mutate(&cfg)
			consumer, err := NewTrialResultConsumer(cfg, tc.handle, TrialResultCounters{})
			assert.Error(t, err)
			assert.Nil(t, consumer)
		})
	}
}

// 计数回调可以不给(nil):坏消息与重调照常处理,只是不计数。空白的 broker 项被丢掉,不带进 reader。
func TestNewTrialResultConsumer_NilCountersAndBlankBrokersAreTolerated(t *testing.T) {
	cfg := trialConsumerConfig()
	cfg.Brokers = []string{"", "127.0.0.1:9092"}
	calls := 0
	consumer, err := NewTrialResultConsumer(cfg, func(context.Context, *kafkapb.BattleResultEvent) error {
		calls++
		if calls == 1 {
			return errTransient
		}
		return nil
	}, TrialResultCounters{})
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.1:9092"}, consumer.cfg.Brokers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	reader := &fakeTrialReader{cancel: cancel, events: &events, steps: []fetchStep{
		{msg: kafkago.Message{Offset: 1, Value: []byte{0xff}}},
		trialMessage(t, 2, 901),
	}}
	consumer.sleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	consumer.consume(ctx, reader)

	assert.Equal(t, 2, calls)
	assert.Equal(t, []int64{1, 2}, reader.committed)
}
