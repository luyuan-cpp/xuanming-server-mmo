package kafkautil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	kafkago "github.com/segmentio/kafka-go"

	"shared/drainproof"
)

// 排空框架与消费组残余收口(2026-11-12)的回归,设计见 docs/design/kafka-bound-consumer-group.md:
//   - 第 1 条:排空时逐个离组,在途 / 退避中的记录交给新属主,收尾按组内已提交位点核结局,不记 HOLD;
//   - 第 2 条:分区漂走、不再回来的确定性恢复票据,收尾核一次组内已提交位点;
//   - 第 3、5 条:有界重试后转死信(含回调 panic),死信写成功才提交越过;
//   - 第 7 条:reader 模式交还已落库的记录时结束本代次,安静分区不再挂着积压;
//   - 第 8 条:保留期删掉了本组还没处理的记录时 HOLD,不静默跳过。
// 全部用替身驱动真实状态机:不连 Kafka,不等墙钟(退避、收尾轮询都经 sleep 替身);等待只等状态信号,超时只是防挂死的护栏。

// waitOwnerState 等 owner 的状态满足 cond(cond 在持有 owner.mu 时调用,只读 owner 字段),靠 owner 每次改状态时的信号唤醒。
func waitOwnerState(t *testing.T, owner *BoundConsumerGroup, what string, cond func() bool) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		owner.mu.Lock()
		ok := cond()
		changed := owner.changed
		owner.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("等待超时: %s", what)
		}
	}
}

// partitionZero 返回本代次分区 0 的状态;调用方持有 owner.mu。
func partitionZero(owner *BoundConsumerGroup) *boundPartition {
	return owner.partitions[boundPartitionKey(boundTestTopic, 0)]
}

// closingSleep 在 closing 置位之前让退避等到被打断(signal 报一次「进入退避」),之后立即返回(收尾轮询不等墙钟)。
func closingSleep(closing *atomic.Bool, signal chan struct{}) func(context.Context) bool {
	return func(ctx context.Context) bool {
		if closing.Load() {
			return ctx.Err() == nil
		}
		select {
		case signal <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return false
	}
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var metric dto.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetCounter().GetValue()
}

// ── 第 1 条:排空时逐个离组 ──────────────────────────────────────

// 维护排空时这条记录还在退避(TiDB 抖动):不再重试,交给新属主。收尾第二轮核到组内位点被新属主提交越过它,
// 收据成立、计入同一次排空:已接纳 = 已结清 + 重放 + 交还 仍成立,交还的那条记作 HandedOff 并附核到的位点。
// 修复前:owner 终止时没结清 → HOLD,排空收据停住。
func TestBoundConsumerGroupDrainHandsOffRecordAndConfirmsFromGroupCommit(t *testing.T) {
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error { return errors.New("tidb: write conflict") })
	backingOff := make(chan struct{}, 1)
	var closing atomic.Bool
	h.sleepFn = closingSleep(&closing, backingOff)
	h.groupCommitted = func(read int) int64 {
		if read == 1 {
			return -1 // 第一轮:新属主还没提交过
		}
		return 1 // 第二轮:新属主处理并提交到 1
	}
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(0, "battle-0")
	waitSignal(t, backingOff, "处理失败后进入退避")
	closing.Store(true)
	raw, err := h.stop()
	if err != nil {
		t.Fatalf("新属主结清了交还的记录,排空收据必须成立: %v", err)
	}
	if reads := h.committedReadCount(); reads != 2 {
		t.Fatalf("应在第二轮核到新属主提交越过: reads=%d", reads)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); err != nil {
		t.Fatalf("交还给新属主不是未知结局: %v", err)
	}
	for _, want := range []string{
		`"Accepted":1,"Settled":0,"Replayed":0,"Released":1,"HandedOff":1,"DeadLettered":0`,
		`"ResolvedHandoffs":[{"Topic":"results","Partition":0,"Offset":0,"Generation":1,"GroupCommitted":1}]`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("收据缺 %s: %s", want, raw)
		}
	}
	if commits := h.commitsOf(1); len(commits) != 0 {
		t.Fatalf("没处理成功的记录被提交: %+v", commits)
	}
}

// 逐个离组:本副本收到 StopFetching 时回调正在处理;先走的副本离组结束了本副本的代次,回调成功后用旧代次提交被拒
// (本副本已被踢出)。这一条交给新属主,不重试提交、不 HOLD;新属主提交越过之后收据成立。
func TestBoundConsumerGroupLeavingOneByOneHandsOffRejectedOldGenerationCommit(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error {
		entered <- struct{}{}
		<-release
		return nil
	})
	h.commitErr = func(int32, int64) error { return errors.New("[25] Unknown Member Id") }
	h.groupCommitted = func(int) int64 { return 1 }
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(0, "battle-0")
	waitSignal(t, entered, "回调开始处理 offset 0")

	type closed struct {
		raw []byte
		err error
	}
	result := make(chan closed, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, err := h.owner.CloseAndReport(ctx)
		result <- closed{raw: raw, err: err}
	}()
	waitOwnerState(t, h.owner, "本副本进入维护排空", func() bool { return h.owner.stopping })
	h.endGeneration(1) // 先走的副本离组:本副本的代次结束
	close(release)
	got := waitSignal(t, result, "排空收据")
	h.waitRun()
	if got.err != nil {
		t.Fatalf("逐个离组时旧代次提交被拒不能让收据 HOLD: %v", got.err)
	}
	if commits := h.commitsOf(1); len(commits) != 1 || commits[0].err == nil {
		t.Fatalf("终止时提交被拒只试一次、不重试: %+v", commits)
	}
	if !bytes.Contains(got.raw, []byte(`"HandedOff":1`)) || !bytes.Contains(got.raw, []byte(`"GroupCommitted":1`)) {
		t.Fatalf("收据没有记下交给新属主、由新属主结清: %s", got.raw)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); err != nil {
		t.Fatalf("relay 被记了未知结局: %v", err)
	}
}

// ── 第 2 条:漂走的确定性恢复票据 ─────────────────────────────────

// reader 模式:确定未提交的记录交回同 owner 重放后,分区漂到别的副本、不再回来。收尾读组内已提交位点:
// 越过票据、且组在该分区提交过(>= 0)才结清;没越过,或组从没提交过(越过它的只可能是保留期)都 HOLD。
func TestBoundMessageReaderResolvesDriftedReplayTicketAtClose(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed int64
		hold      bool
	}{
		{name: "组内位点越过且组提交过:结清", committed: 11},
		{name: "组内位点没越过:HOLD", committed: 10, hold: true},
		{name: "组在该分区没提交过:HOLD", committed: -1, hold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBoundReaderHarness(t)
			h.groupCommitted = func(int) int64 { return tc.committed }
			first := h.addReader()
			h.start()
			h.deliver(1, 10)
			first.push(10, "snapshot-a")
			message := h.fetch()
			if err := h.owner.ReportProcessingFailure(context.Background(), ProcessingNotCommitted, metadata(message.Offset)); err != nil {
				t.Fatal(err)
			}
			h.waitGeneration(1) // 之后本 owner 再也分不到这个分区
			raw, err := h.stop()
			h.relay.CloseAdmission()
			waitErr := h.relay.Wait(context.Background())
			if tc.hold {
				if !errors.Is(err, drainproof.ErrHold) || !errors.Is(err, errBoundTicketUnconfirmed) || raw != nil {
					t.Fatalf("核不到组内位点越过票据,必须 HOLD: %v %s", err, raw)
				}
				if !errors.Is(waitErr, drainproof.ErrHold) {
					t.Fatalf("relay 没有保留未知结局(或票据没结束): %v", waitErr)
				}
				return
			}
			if err != nil || waitErr != nil {
				t.Fatalf("别的副本提交越过了票据,收据必须成立: %v / %v", err, waitErr)
			}
			if !bytes.Contains(raw, []byte(`"Resolution":"`+boundReplayGroupCommittedAtClose+`"`)) {
				t.Fatalf("收据没有记下收尾核对的结清方式: %s", raw)
			}
		})
	}
}

// ── 第 7 条:reader 模式交还之后结束本代次 ─────────────────────────

// 提交失败交还已落库的记录时结束本代次:安静分区上不用等新消息,下一代次立即从组内已提交位点重投并提交,积压不再挂着。
// 修复前:本代次内不再重投,安静分区一直挂着这段积压,提交停滞告警只好要求「有新消息到达」才计时。
func TestBoundMessageReaderEndsGenerationAfterReleasedCommitSoQuietPartitionIsRedelivered(t *testing.T) {
	h := newBoundReaderHarness(t)
	h.commitErr = func(generation int32, _ int64) error {
		if generation == 1 {
			return errors.New("[16] Not Coordinator For Group")
		}
		return nil
	}
	first := h.addReader()
	second := h.addReader()
	h.start()
	h.deliver(1, 10)
	first.push(10, "snapshot-a")
	message := h.fetch()
	if err := h.owner.CommitMessages(context.Background(), metadata(message.Offset)); !errors.Is(err, ErrUncommitted) {
		t.Fatalf("代次还在、提交重试用尽应交还重投: %v", err)
	}
	h.waitGeneration(1) // 没有新消息到达,代次 1 也自己结束了
	h.deliver(2, 10)    // 组内位点没动:下一代次从 10 重投
	second.push(10, "snapshot-a")
	again := h.fetch()
	if again.Offset != 10 {
		t.Fatalf("下一代次应重投交还的那条: %d", again.Offset)
	}
	if err := h.owner.CommitMessages(context.Background(), metadata(again.Offset)); err != nil {
		t.Fatalf("重投之后提交应成功: %v", err)
	}
	raw, err := h.stop()
	if err != nil {
		t.Fatalf("排空收据不成立: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"Accepted":1,"Settled":0,"Replayed":0,"Released":1`)) || !bytes.Contains(raw, []byte(`"Accepted":1,"Settled":1,"Replayed":0,"Released":0`)) {
		t.Fatalf("收据没有如实记录交还与重投后的结清: %s", raw)
	}
	if h.committedNext(2) != 11 {
		t.Fatalf("重投后应提交到 11: %d", h.committedNext(2))
	}
}

// ── 第 3、5 条:有界重试后转死信 ───────────────────────────────────

type fakeDeadLetterSink struct {
	mu       sync.Mutex
	failures []error // 前几次写的结局(nil = 成功),用完之后一律成功
	writes   int
	records  []DeadLetterRecord
}

func (s *fakeDeadLetterSink) WriteDeadLetter(_ context.Context, record DeadLetterRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if len(s.failures) > 0 {
		err := s.failures[0]
		s.failures = s.failures[1:]
		if err != nil {
			return err
		}
	}
	s.records = append(s.records, record)
	return nil
}

func (s *fakeDeadLetterSink) written() ([]DeadLetterRecord, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DeadLetterRecord(nil), s.records...), s.writes
}

func newBoundHarnessWithDeadLetter(t *testing.T, sink DeadLetterSink, maxAttempts int, handler func(context.Context, kafkago.Message) error) *boundHarness {
	t.Helper()
	h := newBoundHarnessBase()
	config := boundTestConfig()
	config.DeadLetter = &DeadLetterConfig{Sink: sink, MaxAttempts: maxAttempts}
	owner, err := NewBoundConsumerGroup(config, h.relay, handler)
	if err != nil {
		t.Fatal(err)
	}
	h.attach(t, owner)
	return h
}

// 回调恒失败的毒丸:失败到预算(3 次)后写死信 —— 原件(topic、分区、offset、时间、key、value、headers)、原因与次数都在 ——
// 写成功才提交越过,之后照常消费下一条。修复前:分区永远停在这一条上。
func TestBoundConsumerGroupDeadLettersAfterRetryBudgetAndMovesOn(t *testing.T) {
	sink := &fakeDeadLetterSink{}
	h := newBoundHarnessWithDeadLetter(t, sink, 3, func(_ context.Context, message kafkago.Message) error {
		if message.Offset == 0 {
			return errors.New("tidb: deadlock found when trying to get lock")
		}
		return nil
	})
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	original := kafkago.Message{Topic: boundTestTopic, Partition: 0, Offset: 0, Time: time.UnixMilli(1_699_999_000_000),
		Key: []byte("battle-7"), Value: []byte("poison"), Headers: []kafkago.Header{{Key: "traceparent", Value: []byte("00-abc-def-01")}}}
	reader.items <- fakeFetch{message: original}
	reader.push(1, "ok")
	waitOwnerState(t, h.owner, "毒丸转死信后继续消费到 offset 2", func() bool { p := partitionZero(h.owner); return p != nil && p.committed == 2 })
	raw, err := h.stop()
	if err != nil {
		t.Fatalf("转死信之后排空收据必须成立: %v", err)
	}
	records, writes := sink.written()
	if writes != 1 || len(records) != 1 {
		t.Fatalf("应恰好写一条死信: writes=%d records=%d", writes, len(records))
	}
	record := records[0]
	if record.Group != "results-group" || record.Cause != DeadLetterCauseExhausted || record.Attempts != 3 ||
		record.Reason != "tidb: deadlock found when trying to get lock" || !record.At.Equal(time.UnixMilli(1_700_000_000_000)) {
		t.Fatalf("死信原因或次数不对: %+v", record)
	}
	if record.Original.Topic != original.Topic || record.Original.Partition != 0 || record.Original.Offset != 0 || !record.Original.Time.Equal(original.Time) ||
		!bytes.Equal(record.Original.Key, original.Key) || !bytes.Equal(record.Original.Value, original.Value) ||
		len(record.Original.Headers) != 1 || record.Original.Headers[0].Key != "traceparent" {
		t.Fatalf("死信原件不完整: %+v", record.Original)
	}
	if sleeps := h.recordedSleeps(); !slices.Equal(sleeps, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("预算用完立即写死信,不再多等一轮: %v", sleeps)
	}
	commits := h.commitsOf(1)
	if len(commits) != 2 || commits[0].next != 1 || commits[1].next != 2 || commits[0].err != nil || commits[1].err != nil {
		t.Fatalf("死信写成功后才提交越过,之后照常提交: %+v", commits)
	}
	if !bytes.Contains(raw, []byte(`"Accepted":2,"Settled":2,"Replayed":0,"Released":0,"HandedOff":0,"DeadLettered":1`)) {
		t.Fatalf("收据没有记下转死信的那条: %s", raw)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); err != nil {
		t.Fatalf("转死信是确定结局,不能记 HOLD: %v", err)
	}
}

// 回调判成无法处理(解不出):不重试,第一次就写死信。
func TestBoundConsumerGroupDeadLettersUnprocessableRecordWithoutRetry(t *testing.T) {
	sink := &fakeDeadLetterSink{}
	var calls atomic.Int32
	h := newBoundHarnessWithDeadLetter(t, sink, 20, func(context.Context, kafkago.Message) error {
		calls.Add(1)
		return fmt.Errorf("%w: battle result cannot be decoded", ErrUnprocessable)
	})
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(0, "\xff\xff")
	waitOwnerState(t, h.owner, "坏消息转死信后提交越过", func() bool { p := partitionZero(h.owner); return p != nil && p.committed == 1 })
	if _, err := h.stop(); err != nil {
		t.Fatalf("排空收据不成立: %v", err)
	}
	records, _ := sink.written()
	if len(records) != 1 || records[0].Cause != DeadLetterCauseUnprocessable || records[0].Attempts != 1 || calls.Load() != 1 {
		t.Fatalf("无法处理的记录不重试、直接转死信: records=%+v calls=%d", records, calls.Load())
	}
	if sleeps := h.recordedSleeps(); len(sleeps) != 0 {
		t.Fatalf("无法处理的记录不该退避: %v", sleeps)
	}
}

// 死信写不进去(死信 topic 暂不可用):记录留在原处,退避后重写,写成功之前不提交、不跳过;回调不再被调用(预算已用完)。
func TestBoundConsumerGroupKeepsRecordInPlaceWhileDeadLetterWriteFails(t *testing.T) {
	sink := &fakeDeadLetterSink{failures: []error{errors.New("[6] Not Leader For Partition"), errors.New("context deadline exceeded")}}
	var calls atomic.Int32
	h := newBoundHarnessWithDeadLetter(t, sink, 20, func(context.Context, kafkago.Message) error {
		calls.Add(1)
		return fmt.Errorf("%w: schema violation", ErrUnprocessable)
	})
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(0, "bad")
	waitOwnerState(t, h.owner, "死信第三次写成功后提交", func() bool { p := partitionZero(h.owner); return p != nil && p.committed == 1 })
	if _, err := h.stop(); err != nil {
		t.Fatalf("排空收据不成立: %v", err)
	}
	if records, writes := sink.written(); writes != 3 || len(records) != 1 {
		t.Fatalf("写失败要原地重写直到成功: writes=%d records=%d", writes, len(records))
	}
	if calls.Load() != 1 {
		t.Fatalf("预算用完后只重写死信,不再调用回调: %d", calls.Load())
	}
	if commits := h.commitsOf(1); len(commits) != 1 || commits[0].next != 1 {
		t.Fatalf("死信写成功之前不能提交越过: %+v", commits)
	}
	if sleeps := h.recordedSleeps(); !slices.Equal(sleeps, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("死信写失败按退避重写: %v", sleeps)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); err != nil {
		t.Fatalf("死信写失败只是暂时失败,不能记 HOLD: %v", err)
	}
}

// 第 5 条:回调 panic 只是这一条的一次失败:退避重调,次数到上限转死信,不记 HOLD、不污染排空收据。
func TestBoundConsumerGroupDeadLettersPanickingHandlerWithoutHold(t *testing.T) {
	sink := &fakeDeadLetterSink{}
	h := newBoundHarnessWithDeadLetter(t, sink, 2, func(context.Context, kafkago.Message) error { panic("nil map write in settlement") })
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(0, "battle-0")
	waitOwnerState(t, h.owner, "panic 两次后转死信并提交", func() bool { p := partitionZero(h.owner); return p != nil && p.committed == 1 })
	raw, err := h.stop()
	if err != nil {
		t.Fatalf("回调 panic 不能让排空收据 HOLD: %v", err)
	}
	records, _ := sink.written()
	if len(records) != 1 || records[0].Cause != DeadLetterCauseExhausted || records[0].Attempts != 2 || records[0].Reason != errBoundHandlerPanicked.Error() {
		t.Fatalf("panic 按失败计次、到上限转死信: %+v", records)
	}
	if !bytes.Contains(raw, []byte(`"DeadLettered":1`)) {
		t.Fatalf("收据没有记下转死信的那条: %s", raw)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); err != nil {
		t.Fatalf("relay 被记了未知结局: %v", err)
	}
}

// 没打开死信(match 结果消费,负责人 2026-11-06 定:保留位点、告警、人工处置):判成无法处理也照常原地重试,不跳过。
func TestBoundConsumerGroupWithoutDeadLetterRetriesUnprocessableInPlace(t *testing.T) {
	var calls atomic.Int32
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error {
		if calls.Add(1) <= 2 {
			return fmt.Errorf("%w: original conflict", ErrUnprocessable)
		}
		return nil
	})
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(0, "battle-0")
	waitOwnerState(t, h.owner, "原地重试后提交", func() bool { p := partitionZero(h.owner); return p != nil && p.committed == 1 })
	if _, err := h.stop(); err != nil {
		t.Fatalf("排空收据不成立: %v", err)
	}
	if sleeps := h.recordedSleeps(); !slices.Equal(sleeps, []time.Duration{time.Second, 2 * time.Second}) || calls.Load() != 3 {
		t.Fatalf("没打开死信时原地退避重试同一条: sleeps=%v calls=%d", sleeps, calls.Load())
	}
}

func TestBoundConsumerGroupDeadLetterConfigValidation(t *testing.T) {
	handler := func(context.Context, kafkago.Message) error { return nil }
	config := boundTestConfig()
	config.DeadLetter = &DeadLetterConfig{}
	if _, err := NewBoundConsumerGroup(config, drainproof.New(), handler); err == nil {
		t.Fatal("打开死信却没有死信存储必须拒绝")
	}
	config.DeadLetter = &DeadLetterConfig{Sink: &fakeDeadLetterSink{}, MaxAttempts: maxDeadLetterMaxAttempts + 1}
	if _, err := NewBoundConsumerGroup(config, drainproof.New(), handler); err == nil {
		t.Fatal("死信重试预算超过上限必须拒绝")
	}
	config.DeadLetter = &DeadLetterConfig{Sink: &fakeDeadLetterSink{}}
	owner, err := NewBoundConsumerGroup(config, drainproof.New(), handler)
	if err != nil || owner.deadLetter == nil || owner.deadLetter.MaxAttempts != defaultDeadLetterMaxAttempts {
		t.Fatalf("缺省预算应为 %d: owner=%+v err=%v", defaultDeadLetterMaxAttempts, owner, err)
	}
	if _, err := NewBoundMessageReader(config, drainproof.New()); err == nil {
		t.Fatal("reader 模式不支持死信,必须拒绝")
	}
}

// 死信消息:key、value、原 headers 原样,旧的 dlq.* 头去掉;解回来就是原件。原因截到 1024 字节且不切开 UTF-8 字符。
func TestDeadLetterMessageRoundTripsTheOriginal(t *testing.T) {
	original := kafkago.Message{Topic: "match-results", Partition: 2, Offset: 41, Time: time.UnixMilli(1_700_000_000_123),
		Key: []byte("9001"), Value: []byte{0xff, 0x01, 0x02},
		Headers: []kafkago.Header{{Key: "traceparent", Value: []byte("00-a-b-01")}, {Key: DeadLetterHeaderCause, Value: []byte("stale")}}}
	reason := strings.Repeat("原因", 600)
	record := DeadLetterRecord{Group: "guild-trial", Original: original, Cause: DeadLetterCauseExhausted, Attempts: 20, Reason: reason, At: time.UnixMilli(1_700_000_100_000)}
	message := DeadLetterMessage(record)
	if message.Topic != "" || !bytes.Equal(message.Key, original.Key) || !bytes.Equal(message.Value, original.Value) || !message.Time.Equal(record.At) {
		t.Fatalf("死信消息的 key / value / 时间不对: %+v", message)
	}
	parsed, err := ParseDeadLetter(message)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Group != "guild-trial" || parsed.Cause != DeadLetterCauseExhausted || parsed.Attempts != 20 || !parsed.At.Equal(record.At) {
		t.Fatalf("死信元数据解不回来: %+v", parsed)
	}
	got := parsed.Original
	if got.Topic != original.Topic || got.Partition != 2 || got.Offset != 41 || !got.Time.Equal(original.Time) ||
		!bytes.Equal(got.Key, original.Key) || !bytes.Equal(got.Value, original.Value) || len(got.Headers) != 1 || got.Headers[0].Key != "traceparent" {
		t.Fatalf("原件解不回来(旧 dlq.* 头要去掉): %+v", got)
	}
	if len(parsed.Reason) > maxDeadLetterReasonBytes || !utf8.ValidString(parsed.Reason) || !strings.HasPrefix(reason, parsed.Reason) || parsed.Reason == "" {
		t.Fatalf("原因截断不对: %d bytes", len(parsed.Reason))
	}
	if _, err := ParseDeadLetter(kafkago.Message{Value: []byte("x")}); err == nil {
		t.Fatal("不是本包写的死信必须解析失败")
	}
	spec := DeadLetterTopicSpec("guild-trial", true)
	if spec.Name != "guild-trial-dlq" || spec.Partitions != 1 || spec.RetentionMs != deadLetterRetentionMs || spec.ReplicaFactor != 3 || !spec.RequireDurability {
		t.Fatalf("死信 topic 契约不对: %+v", spec)
	}
}

// ── 第 8 条:保留期删掉了本组还没处理的记录 ───────────────────────

// 组在该分区提交到 10;kafka-go 遇 OffsetOutOfRange 跳到了日志起点之后,下一条是 15,日志起点已是 13:
// 10..12 没被本组处理就删了。HOLD、停止取新、计数,不接纳、不提交。修复前:照常接纳 15,静默跳过 10..12。
func TestBoundConsumerGroupHoldsWhenRetentionDeletedRecordsTheGroupHadNotProcessed(t *testing.T) {
	var calls atomic.Int32
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error { calls.Add(1); return nil })
	h.logStart = func() (int64, error) { return 13, nil }
	before := counterValue(t, boundRetentionLossTotal.WithLabelValues("results-group"))
	reader := h.addReader()
	h.start()
	h.deliver(1, 10)
	reader.push(15, "after-gap")
	if err := h.waitRun(); !errors.Is(err, errBoundRetentionSkipped) {
		t.Fatalf("保留期删掉了没处理的记录必须 HOLD 并停止取新: %v", err)
	}
	if calls.Load() != 0 || len(h.commitsOf(1)) != 0 {
		t.Fatalf("缺口之后的记录不能接纳或提交: calls=%d commits=%+v", calls.Load(), h.commitsOf(1))
	}
	if _, err := h.owner.CloseAndReport(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("排空收据必须 HOLD: %v", err)
	}
	if after := counterValue(t, boundRetentionLossTotal.WithLabelValues("results-group")); after-before != 1 {
		t.Fatalf("保留期丢失要计数(告警用): %v -> %v", before, after)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("relay 没有保留未知结局: %v", err)
	}
}

// 有缺口但日志起点没越过应到位点(事务控制记录、压缩占的位点):不是保留期删的,照常接纳。
func TestBoundConsumerGroupAcceptsGapNotCausedByRetention(t *testing.T) {
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error { return nil })
	h.logStart = func() (int64, error) { return 10, nil }
	reader := h.addReader()
	h.start()
	h.deliver(1, 10)
	reader.push(12, "after-marker")
	waitOwnerState(t, h.owner, "缺口之后的记录照常结清", func() bool { p := partitionZero(h.owner); return p != nil && p.committed == 13 })
	if _, err := h.stop(); err != nil {
		t.Fatalf("不是保留期造成的缺口不能 HOLD: %v", err)
	}
}

// 组在该分区从没提交过、本 owner 也还没取到过一条:组从日志起点读,起点在打开之后才被推过去,没有越过本组的已提交位点,照常接纳。
func TestBoundConsumerGroupAcceptsLogStartMovedBeforeUncommittedPartitionWasRead(t *testing.T) {
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error { return nil })
	h.logStart = func() (int64, error) { return 5, nil }
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(5, "first-kept")
	waitOwnerState(t, h.owner, "照常结清", func() bool { p := partitionZero(h.owner); return p != nil && p.committed == 6 })
	if _, err := h.stop(); err != nil {
		t.Fatalf("新组从日志起点读,不能 HOLD: %v", err)
	}
}

// reader 回退(取到的 offset 小于应到位点):接纳会重复处理或把提交位点往回拨,HOLD。
func TestBoundConsumerGroupHoldsWhenReaderRewinds(t *testing.T) {
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error { return nil })
	reader := h.addReader()
	h.start()
	h.deliver(1, 10)
	reader.push(9, "rewound")
	if err := h.waitRun(); err == nil || !strings.Contains(err.Error(), "回退") {
		t.Fatalf("reader 回退必须 HOLD: %v", err)
	}
}
