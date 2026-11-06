package kafkautil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"shared/drainproof"
)

// r2-kc#1 的回归:BoundConsumerGroup 遇再均衡、暂时失败不能永久停止消费;真正的维护排空(StopFetching)仍按收据 HOLD。
// 用例只用替身(消费组、代次、分区 reader、提交、退避)驱动真实状态机:不连 Kafka,不等墙钟 —— 退避替身立即返回或等到被打断,
// waitUntil / time.After 只是防挂死的护栏,不参与判定。

const boundTestTopic = "results"

func boundTestConfig() BoundConsumerGroupConfig {
	return BoundConsumerGroupConfig{
		Group:  kafkago.ConsumerGroupConfig{ID: "results-group", Brokers: []string{"localhost:9092"}, Topics: []string{boundTestTopic}},
		Reader: kafkago.ReaderConfig{MinBytes: 1, MaxBytes: 1024},
		// 远大于用例耗时:单次处理预算不能参与判定。
		HandlerTimeout: 5 * time.Minute,
	}
}

type fakeNext struct {
	generation *kafkago.Generation
	err        error
}

// fakeBoundGroup 按测试交出的顺序吐出代次或错误。真实 kafka-go 在上一代次结束前不给下一代次,用例自己等上一代次退出后再交。
type fakeBoundGroup struct{ next chan fakeNext }

func (g *fakeBoundGroup) Next(ctx context.Context) (*kafkago.Generation, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case item := <-g.next:
		return item.generation, item.err
	}
}

func (g *fakeBoundGroup) Close() error { return nil }

type fakeFetch struct {
	message kafkago.Message
	err     error
}

// fakePartitionReader 吐出测试推入的记录或错误;没有就等到 ctx 结束。
type fakePartitionReader struct{ items chan fakeFetch }

func newFakePartitionReader() *fakePartitionReader {
	return &fakePartitionReader{items: make(chan fakeFetch, 16)}
}

func (r *fakePartitionReader) push(offset int64, value string) {
	r.items <- fakeFetch{message: kafkago.Message{Topic: boundTestTopic, Partition: 0, Offset: offset, Value: []byte(value)}}
}

func (r *fakePartitionReader) fail(err error) { r.items <- fakeFetch{err: err} }

func (r *fakePartitionReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	select {
	case <-ctx.Done():
		return kafkago.Message{}, ctx.Err()
	case item := <-r.items:
		return item.message, item.err
	}
}

func (r *fakePartitionReader) Close() error { return nil }

type fakeOpen struct {
	reader *fakePartitionReader
	err    error
	// earliest 是日志起点:assignment 没有组内已提交位点(FirstOffset)时从这里起读,零值 = 0。
	earliest int64
}

type fakeCommit struct {
	generation int32
	next       int64
	err        error
}

// boundHarness 把替身接到一个真实 owner 上(单 topic、单分区 0)。
type boundHarness struct {
	t      *testing.T
	owner  *BoundConsumerGroup
	relay  *drainproof.Session
	group  *fakeBoundGroup
	opens  chan fakeOpen
	runErr chan error

	mu        sync.Mutex
	genCancel map[int32]context.CancelFunc
	genDone   map[int32]chan struct{}
	commits   []fakeCommit
	sleeps    []time.Duration
	// commitErr 决定一次提交的结局(nil = 成功);sleepFn 决定一次退避(nil = 立即返回 ctx.Err()==nil)。
	commitErr func(generation int32, next int64) error
	sleepFn   func(ctx context.Context) bool
}

func newBoundHarness(t *testing.T, handler func(context.Context, kafkago.Message) error) *boundHarness {
	t.Helper()
	h := newBoundHarnessBase()
	owner, err := NewBoundConsumerGroup(boundTestConfig(), h.relay, handler)
	if err != nil {
		t.Fatal(err)
	}
	h.attach(t, owner)
	return h
}

func newBoundReaderHarness(t *testing.T) *boundHarness {
	t.Helper()
	h := newBoundHarnessBase()
	owner, err := NewBoundMessageReader(boundTestConfig(), h.relay)
	if err != nil {
		t.Fatal(err)
	}
	h.attach(t, owner)
	return h
}

func newBoundHarnessBase() *boundHarness {
	return &boundHarness{
		relay:     drainproof.New(),
		group:     &fakeBoundGroup{next: make(chan fakeNext, 8)},
		opens:     make(chan fakeOpen, 8),
		runErr:    make(chan error, 1),
		genCancel: map[int32]context.CancelFunc{},
		genDone:   map[int32]chan struct{}{},
	}
}

func (h *boundHarness) attach(t *testing.T, owner *BoundConsumerGroup) {
	h.t = t
	h.owner = owner
	owner.newGroup = func(kafkago.ConsumerGroupConfig) (boundGroupClient, error) { return h.group, nil }
	owner.startGeneration = h.startGeneration
	owner.openReader = h.openReader
	owner.commitOffsets = h.commitOffsets
	owner.sleep = h.sleep
}

func (h *boundHarness) start() {
	go func() { h.runErr <- h.owner.Run(context.Background()) }()
}

// deliver 交出一个只分到 results/0 的代次;offset 是组内已提交位点(FirstOffset = 没提交过)。
func (h *boundHarness) deliver(id int32, offset int64) {
	h.group.next <- fakeNext{generation: &kafkago.Generation{ID: id, GroupID: "results-group", MemberID: "member",
		Assignments: map[string][]kafkago.PartitionAssignment{boundTestTopic: {{ID: 0, Offset: offset}}}}}
}

func (h *boundHarness) deliverErr(err error) { h.group.next <- fakeNext{err: err} }

func (h *boundHarness) addReader() *fakePartitionReader {
	reader := newFakePartitionReader()
	h.opens <- fakeOpen{reader: reader}
	return reader
}

func (h *boundHarness) startGeneration(generation *kafkago.Generation, run func(context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	h.mu.Lock()
	h.genCancel[generation.ID] = cancel
	h.genDone[generation.ID] = done
	h.mu.Unlock()
	go func() {
		defer close(done)
		run(ctx)
	}()
}

// endGeneration 模拟心跳发现再均衡:代次的 ctx 结束(kafka-go 的 genCtx)。
func (h *boundHarness) endGeneration(id int32) {
	h.mu.Lock()
	cancel := h.genCancel[id]
	h.mu.Unlock()
	cancel()
}

// waitGeneration 等某代次的 Start 回调返回(真实 kafka-go 此后才会给下一代次)。
func (h *boundHarness) waitGeneration(id int32) {
	h.t.Helper()
	var done chan struct{}
	waitUntil(h.t, fmt.Sprintf("代次 %d 已启动", id), func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		done = h.genDone[id]
		return done != nil
	})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		h.t.Fatalf("代次 %d 的 worker 没有退出", id)
	}
}

func (h *boundHarness) openReader(ctx context.Context, _ string, assigned kafkago.PartitionAssignment) (boundPartitionReader, int64, error) {
	select {
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case open := <-h.opens:
		if open.err != nil {
			return nil, 0, open.err
		}
		offset := assigned.Offset
		if offset < 0 {
			offset = open.earliest
		}
		return open.reader, offset, nil
	}
}

func (h *boundHarness) commitOffsets(generation *kafkago.Generation, offsets map[string]map[int]int64) error {
	next := offsets[boundTestTopic][0]
	var err error
	if h.commitErr != nil {
		err = h.commitErr(generation.ID, next)
	}
	h.mu.Lock()
	h.commits = append(h.commits, fakeCommit{generation: generation.ID, next: next, err: err})
	h.mu.Unlock()
	return err
}

func (h *boundHarness) sleep(ctx context.Context, d time.Duration) bool {
	h.mu.Lock()
	h.sleeps = append(h.sleeps, d)
	fn := h.sleepFn
	h.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return ctx.Err() == nil
}

// committedNext 是某代次成功提交过的最大 exclusive offset。
func (h *boundHarness) committedNext(generation int32) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var best int64
	for _, commit := range h.commits {
		if commit.generation == generation && commit.err == nil && commit.next > best {
			best = commit.next
		}
	}
	return best
}

func (h *boundHarness) commitsOf(generation int32) []fakeCommit {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []fakeCommit
	for _, commit := range h.commits {
		if commit.generation == generation {
			out = append(out, commit)
		}
	}
	return out
}

func (h *boundHarness) recordedSleeps() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.sleeps...)
}

func (h *boundHarness) failure() error {
	h.owner.mu.Lock()
	defer h.owner.mu.Unlock()
	return h.owner.failure
}

// stop 走真实排空入口 CloseAndReport(内部 StopFetching),并等 Run 返回。
func (h *boundHarness) stop() ([]byte, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := h.owner.CloseAndReport(ctx)
	h.waitRun()
	return raw, err
}

func (h *boundHarness) waitRun() error {
	h.t.Helper()
	select {
	case err := <-h.runErr:
		h.runErr <- err
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("Run 没有返回")
		return nil
	}
}

func (h *boundHarness) fetch() kafkago.Message {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	message, err := h.owner.FetchMessage(ctx)
	if err != nil {
		h.t.Fatalf("reader 模式取不到已交付记录: %v", err)
	}
	return message
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitSignal[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("等待超时: %s", what)
		var zero T
		return zero
	}
}

func metadata(offset int64) kafkago.Message {
	return kafkago.Message{Topic: boundTestTopic, Partition: 0, Offset: offset}
}

// 在途记录遇再均衡:旧代次提交被拒 → 不提交、不 HOLD、交还重投;下一代次重投同一条并提交,之后照常消费,排空收据成立。
// 修复前:handler 返回时代次已失效 → 「提交结局未知」→ fail + StopFetching,该副本永久停止取消息。
func TestBoundConsumerGroupKeepsConsumingAfterRebalanceWithRecordInFlight(t *testing.T) {
	entered := make(chan int64, 8)
	release := make(chan struct{})
	var callsMu sync.Mutex
	calls := map[int64]int{}
	h := newBoundHarness(t, func(_ context.Context, message kafkago.Message) error {
		callsMu.Lock()
		calls[message.Offset]++
		first := message.Offset == 0 && calls[0] == 1
		callsMu.Unlock()
		entered <- message.Offset
		if first {
			<-release
		}
		return nil
	})
	h.commitErr = func(generation int32, _ int64) error {
		if generation == 1 {
			return errors.New("[27] Rebalance In Progress")
		}
		return nil
	}
	// 退避只等到被打断:代次结束后不能靠「立即返回」空转。
	h.sleepFn = func(ctx context.Context) bool {
		<-ctx.Done()
		return false
	}
	first := h.addReader()
	second := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	first.push(0, "battle-0")
	waitSignal(t, entered, "第一代次开始处理 offset 0")
	h.endGeneration(1)
	close(release)
	h.waitGeneration(1)
	if err := h.failure(); err != nil {
		t.Fatalf("再均衡把 owner 记成 HOLD: %v", err)
	}
	if h.relay.Pending() != 0 {
		t.Fatal("交还重投的记录仍占着票据")
	}
	if len(h.commitsOf(1)) == 0 {
		t.Fatal("代次结束后没有尝试用旧代次提交(重平衡准备期 broker 仍可能接受)")
	}
	for _, commit := range h.commitsOf(1) {
		if commit.err == nil {
			t.Fatalf("被拒的旧代次提交被当成成功: %+v", commit)
		}
	}

	h.deliver(2, kafkago.FirstOffset) // 位点没推进:下一代次从 0 重投
	second.push(0, "battle-0")
	second.push(1, "battle-1")
	waitUntil(t, "第二代次提交到 offset 2", func() bool { return h.committedNext(2) == 2 })
	raw, err := h.stop()
	if err != nil {
		t.Fatalf("在途再均衡之后排空收据不成立: %v", err)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); err != nil {
		t.Fatalf("relay 被记了未知结局: %v", err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls[0] != 2 || calls[1] != 1 {
		t.Fatalf("重投次数不对(offset 0 应在两代次各处理一次): %v", calls)
	}
	if !bytes.Contains(raw, []byte(`"Accepted":1,"Settled":0,"Replayed":0,"Released":1`)) || !bytes.Contains(raw, []byte(`"Accepted":2,"Settled":2,"Replayed":0,"Released":0`)) {
		t.Fatalf("收据没有如实记录交还与结清: %s", raw)
	}
}

// 暂时失败(TiDB 抖动、提交失败)只是不提交:代次还在就按 1s、2s… 退避原地重试同一条;回调成功过就只重试提交。
// 恢复后照常提交、继续消费,不 HOLD。修复前:第一次失败就 fail + StopFetching。
func TestBoundConsumerGroupRetriesTransientFailureInPlaceAndRecovers(t *testing.T) {
	var callsMu sync.Mutex
	calls := map[int64]int{}
	h := newBoundHarness(t, func(_ context.Context, message kafkago.Message) error {
		callsMu.Lock()
		defer callsMu.Unlock()
		calls[message.Offset]++
		if message.Offset == 0 && calls[0] <= 2 {
			return errors.New("tidb: connection refused")
		}
		return nil
	})
	commitFailed := false
	h.commitErr = func(_ int32, next int64) error {
		if next == 2 && !commitFailed {
			commitFailed = true
			return errors.New("[15] Group Coordinator Not Available")
		}
		return nil
	}
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(0, "a")
	reader.push(1, "b")
	waitUntil(t, "恢复后提交到 offset 2", func() bool { return h.committedNext(1) == 2 })
	raw, err := h.stop()
	if err != nil {
		t.Fatalf("暂时失败恢复之后排空收据不成立: %v", err)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); err != nil {
		t.Fatalf("暂时失败被记成未知结局: %v", err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls[0] != 3 || calls[1] != 1 {
		t.Fatalf("重试对象不对(提交失败不应重复调用回调): %v", calls)
	}
	if sleeps := h.recordedSleeps(); fmt.Sprint(sleeps) != fmt.Sprint([]time.Duration{time.Second, 2 * time.Second, time.Second}) {
		t.Fatalf("退避序列应为 1s、2s(回调)与 1s(提交): %v", sleeps)
	}
	if !bytes.Contains(raw, []byte(`"Accepted":2,"Settled":2`)) {
		t.Fatalf("收据没有记下两条结清: %s", raw)
	}
}

// 真正的维护排空(StopFetching)时,没结清的已接纳记录仍按收据记 HOLD:排空语义不变,不能把「没提交」洗成已结清。
func TestBoundConsumerGroupDrainHoldsRecordThatCannotSettle(t *testing.T) {
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error { return errors.New("tidb: write conflict") })
	backingOff := make(chan struct{}, 1)
	h.sleepFn = func(ctx context.Context) bool {
		select {
		case backingOff <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return false
	}
	reader := h.addReader()
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	reader.push(0, "a")
	waitSignal(t, backingOff, "处理失败后进入退避")
	raw, err := h.stop()
	if !errors.Is(err, drainproof.ErrHold) || raw != nil {
		t.Fatalf("排空时没结清的记录必须 HOLD: %v %s", err, raw)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("relay 没有保留未知结局: %v", err)
	}
	if commits := h.commitsOf(1); len(commits) != 0 {
		t.Fatalf("没处理成功的记录被提交: %+v", commits)
	}
}

// 等代次失败(协调者不可达)与分区拉取失败都不停 owner:退避后接着等代次 / 接着取。
func TestBoundConsumerGroupJoinAndFetchErrorsDoNotStopTheOwner(t *testing.T) {
	h := newBoundHarness(t, func(context.Context, kafkago.Message) error { return nil })
	reader := h.addReader()
	h.start()
	h.deliverErr(errors.New("[15] Group Coordinator Not Available"))
	h.deliver(1, kafkago.FirstOffset)
	reader.fail(errors.New("[5] Leader Not Available"))
	reader.push(0, "a")
	waitUntil(t, "拉取恢复后提交到 offset 1", func() bool { return h.committedNext(1) == 1 })
	if _, err := h.stop(); err != nil {
		t.Fatalf("暂时失败之后排空收据不成立: %v", err)
	}
	if sleeps := h.recordedSleeps(); fmt.Sprint(sleeps) != fmt.Sprint([]time.Duration{time.Second, time.Second}) {
		t.Fatalf("入组与拉取各应退避一次 1s: %v", sleeps)
	}
}

// 打开分区的暂时失败(leader 不可达)退避后重试;提交点越过保留期不是暂时的,HOLD 并停止取新(继续读等于静默跳过)。
func TestBoundConsumerGroupOpenRetriesTransientButHoldsOutOfRetention(t *testing.T) {
	t.Run("暂时失败", func(t *testing.T) {
		h := newBoundHarness(t, func(context.Context, kafkago.Message) error { return nil })
		h.opens <- fakeOpen{err: errors.New("dial leader: connection refused")}
		reader := h.addReader()
		h.start()
		h.deliver(1, kafkago.FirstOffset)
		reader.push(0, "a")
		waitUntil(t, "打开重试后提交到 offset 1", func() bool { return h.committedNext(1) == 1 })
		if _, err := h.stop(); err != nil {
			t.Fatalf("打开分区重试之后排空收据不成立: %v", err)
		}
		if sleeps := h.recordedSleeps(); fmt.Sprint(sleeps) != fmt.Sprint([]time.Duration{time.Second}) {
			t.Fatalf("打开失败应退避一次 1s: %v", sleeps)
		}
	})
	t.Run("越过保留期", func(t *testing.T) {
		h := newBoundHarness(t, func(context.Context, kafkago.Message) error { return nil })
		h.opens <- fakeOpen{err: fmt.Errorf("%w: results[0] offset=5 earliest=9 end=20", errBoundAssignmentOutOfRange)}
		h.start()
		h.deliver(1, 5)
		if err := h.waitRun(); !errors.Is(err, errBoundAssignmentOutOfRange) {
			t.Fatalf("提交点越过保留期必须 HOLD 并停止: %v", err)
		}
		if _, err := h.owner.CloseAndReport(context.Background()); !errors.Is(err, drainproof.ErrHold) {
			t.Fatalf("排空收据必须 HOLD: %v", err)
		}
	})
}

// reader 模式:代次已结束(再均衡)时批提交照常用旧代次提交;broker 接受就结清,被拒就交还重投并返回 *UncommittedError,都不 HOLD。
// 修复前:代次 ctx 一结束,CommitMessages 就 fail + StopFetching,data_service 两条落库消费者一起停。
func TestBoundMessageReaderCommitAfterGenerationEndSettlesOrReleases(t *testing.T) {
	relay := drainproof.New()
	owner, err := NewBoundMessageReader(boundTestConfig(), relay)
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	owner.sleep = func(ctx context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return ctx.Err() == nil }
	generation := &kafkago.Generation{ID: 3, GroupID: "results-group", MemberID: "member"}
	generationContext, endGeneration := context.WithCancel(context.Background())
	partition := &boundPartition{topic: boundTestTopic, partition: 0, initial: 10, committed: 10, pending: map[int64]*boundDelivery{}}
	owner.generation = generation
	owner.generationContext = generationContext
	owner.generationCancel = func() {}
	owner.partitions = map[string]*boundPartition{boundPartitionKey(boundTestTopic, 0): partition}
	owner.initialized = true
	for _, offset := range []int64{10, 11} {
		owner.messages <- &boundDelivery{generation: generation, partition: partition, message: kafkago.Message{Topic: boundTestTopic, Partition: 0, Offset: offset, Value: []byte("row")}}
		if _, err := owner.FetchMessage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if relay.Pending() != 2 {
		t.Fatal("两条交付没有登记 accepted")
	}
	endGeneration() // 再均衡:代次 ctx 结束,这两条还在途(waitDelivered 会等它们结清)
	var commits []int64
	owner.commitOffsets = func(g *kafkago.Generation, offsets map[string]map[int]int64) error {
		if g != generation {
			t.Fatalf("没有用交付它们的代次提交: %d", g.ID)
		}
		commits = append(commits, offsets[boundTestTopic][0])
		return nil
	}
	if err := owner.CommitMessages(context.Background(), metadata(10)); err != nil {
		t.Fatalf("重平衡准备期旧代次提交成功却没结清: %v", err)
	}
	if relay.Pending() != 1 || partition.committed != 11 || partition.settled != 1 {
		t.Fatalf("旧代次提交成功后前沿不对: pending=%d committed=%d settled=%d", relay.Pending(), partition.committed, partition.settled)
	}
	owner.commitOffsets = func(_ *kafkago.Generation, offsets map[string]map[int]int64) error {
		commits = append(commits, offsets[boundTestTopic][0])
		return errors.New("[22] Illegal Generation")
	}
	err = owner.CommitMessages(context.Background(), metadata(11))
	var released *UncommittedError
	if !errors.As(err, &released) || !errors.Is(err, ErrUncommitted) || len(released.Released) != 1 || released.Released[0].Offset != 11 {
		t.Fatalf("被拒的提交应交还重投并列出原位置: %v", err)
	}
	if owner.failure != nil || owner.stopping {
		t.Fatalf("代次结束后的提交被拒把 owner 停了: %v", owner.failure)
	}
	if relay.Pending() != 0 || partition.released != 1 || len(partition.pending) != 0 || partition.committed != 11 {
		t.Fatalf("交还重投后票据或前沿不对: pending=%d released=%d inflight=%d committed=%d", relay.Pending(), partition.released, len(partition.pending), partition.committed)
	}
	if fmt.Sprint(commits) != "[11 12]" || len(sleeps) != 0 {
		t.Fatalf("代次已结束的被拒提交不该重试: commits=%v sleeps=%v", commits, sleeps)
	}
}

// reader 模式:代次还在时提交失败先有界重试(共 3 次,200ms 起翻倍);仍失败就交还重投、不 HOLD。
// owner 已终止(StopFetching)时同样的失败才记 HOLD,排空语义不变。
func TestBoundMessageReaderCommitRetriesWhileAliveAndHoldsOnlyWhenTerminating(t *testing.T) {
	relay := drainproof.New()
	owner, err := NewBoundMessageReader(boundTestConfig(), relay)
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	owner.sleep = func(ctx context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return ctx.Err() == nil }
	generation := &kafkago.Generation{ID: 4, GroupID: "results-group", MemberID: "member"}
	partition := &boundPartition{topic: boundTestTopic, partition: 0, initial: 20, committed: 20, pending: map[int64]*boundDelivery{}}
	owner.generation = generation
	owner.generationContext = context.Background()
	owner.generationCancel = func() {}
	owner.partitions = map[string]*boundPartition{boundPartitionKey(boundTestTopic, 0): partition}
	owner.initialized = true
	for _, offset := range []int64{20, 21} {
		owner.messages <- &boundDelivery{generation: generation, partition: partition, message: kafkago.Message{Topic: boundTestTopic, Partition: 0, Offset: offset, Value: []byte("row")}}
		if _, err := owner.FetchMessage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	attempts := 0
	owner.commitOffsets = func(*kafkago.Generation, map[string]map[int]int64) error {
		attempts++
		return errors.New("[15] Group Coordinator Not Available")
	}
	if err := owner.CommitMessages(context.Background(), metadata(20)); !errors.Is(err, ErrUncommitted) {
		t.Fatalf("代次还在、重试用尽应交还重投: %v", err)
	}
	if attempts != boundCommitAttempts || fmt.Sprint(sleeps) != fmt.Sprint([]time.Duration{200 * time.Millisecond, 400 * time.Millisecond}) {
		t.Fatalf("提交重试次数或间隔不对: attempts=%d sleeps=%v", attempts, sleeps)
	}
	if owner.failure != nil || relay.Pending() != 1 {
		t.Fatalf("运行中的提交失败不该 HOLD: failure=%v pending=%d", owner.failure, relay.Pending())
	}
	owner.StopFetching()
	if err := owner.CommitMessages(context.Background(), metadata(21)); err == nil || errors.Is(err, ErrUncommitted) {
		t.Fatalf("终止后提交失败不能当成交还重投: %v", err)
	}
	if owner.failure == nil {
		t.Fatal("终止后没结清的记录必须 HOLD")
	}
	owner.waitDelivered(context.Background(), generation, owner.partitions)
	relay.CloseAdmission()
	if err := relay.Wait(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("relay 没有保留未知结局: %v", err)
	}
}

// reader 模式:确定未提交的记录交回同 owner 重放后,分区漂到别的副本、组内位点被提交越过;分区回来时票据按组内位点结清,
// 之后同分区更高位点的提交照常成功。修复前:票据永久悬着,下一次提交被「越过尚未恢复的已交付前缀」拒绝并 HOLD、停止取新。
func TestBoundMessageReaderResolvesReplayPassedByGroupCommit(t *testing.T) {
	h := newBoundReaderHarness(t)
	first := h.addReader()
	second := h.addReader()
	h.start()
	h.deliver(1, 10)
	first.push(10, "snapshot-a")
	message := h.fetch()
	if err := h.owner.ReportProcessingFailure(context.Background(), ProcessingNotCommitted, metadata(message.Offset)); err != nil {
		t.Fatal(err)
	}
	h.waitGeneration(1)
	if h.relay.Pending() != 1 {
		t.Fatal("确定未提交的票据应保留到重放或被组内位点越过")
	}
	h.deliver(2, 50) // 分区回来时组内已提交到 50(别的副本处理并提交了 10..49)
	waitUntil(t, "重放票据按组内位点结清", func() bool { return h.relay.Pending() == 0 })
	second.push(50, "snapshot-b")
	next := h.fetch()
	if err := h.owner.CommitMessages(context.Background(), metadata(next.Offset)); err != nil {
		t.Fatalf("组内位点越过重放票据之后,更高位点的提交被拒: %v", err)
	}
	raw, err := h.stop()
	if err != nil {
		t.Fatalf("排空收据不成立: %v", err)
	}
	if !bytes.Contains(raw, []byte(boundReplayGroupCommittedPast)) {
		t.Fatalf("收据没有记下按组内位点结清的重放票据: %s", raw)
	}
}

// 评审修正:组在该分区从没提交过(assignment 是 FirstOffset)时 initial 只是日志起点。起点越过重放票据 = 这条没被任何属主
// 提交就被保留期删掉,不能按 GROUP_COMMITTED_PAST 记成已结清:HOLD 并停止取新,票据释放等待资源、未知结局保留。
func TestBoundMessageReaderHoldsReplayDeletedBeforeAnyGroupCommit(t *testing.T) {
	h := newBoundReaderHarness(t)
	first := h.addReader()
	h.opens <- fakeOpen{reader: newFakePartitionReader(), earliest: 5} // 分区回来时组仍没提交过,保留期已把起点推到 5
	h.start()
	h.deliver(1, kafkago.FirstOffset)
	first.push(0, "snapshot-a")
	message := h.fetch()
	if err := h.owner.ReportProcessingFailure(context.Background(), ProcessingNotCommitted, metadata(message.Offset)); err != nil {
		t.Fatal(err)
	}
	h.waitGeneration(1)
	h.deliver(2, kafkago.FirstOffset)
	if err := h.waitRun(); !errors.Is(err, errBoundReplayLost) {
		t.Fatalf("没被提交就被删掉的重放票据必须 HOLD 并停止取新: %v", err)
	}
	if raw, err := h.owner.CloseAndReport(context.Background()); !errors.Is(err, drainproof.ErrHold) || raw != nil {
		t.Fatalf("排空收据必须 HOLD,不能把丢失记成结清: %v %s", err, raw)
	}
	h.relay.CloseAdmission()
	if err := h.relay.Wait(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("relay 没有保留未知结局: %v", err)
	}
}

// 长寿 owner 跨上千次再均衡:代次原件只保留最近 boundHistoryWindow 代,更早的折叠进汇总,不再因代次数超限 HOLD;
// 结局不平的代次滚出窗口时仍 HOLD,不把不平藏进合计。已结清的重放原件同样是滚动窗口。
func TestBoundConsumerGroupHistoryStaysBoundedAcrossManyGenerations(t *testing.T) {
	owner, err := NewBoundConsumerGroup(boundTestConfig(), drainproof.New(), func(context.Context, kafkago.Message) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	balanced := func(id int32) *boundGroupObservation {
		return &boundGroupObservation{Generation: id, Partitions: []boundPartitionObservation{{Topic: boundTestTopic, Accepted: 3, Settled: 2, Released: 1}}}
	}
	owner.mu.Lock()
	for id := int32(1); id <= boundHistoryWindow+6; id++ {
		owner.appendHistoryLocked(balanced(id))
	}
	for i := 0; i < boundReplayWindow+2; i++ {
		owner.appendResolvedReplayLocked(boundReplayObservation{Topic: boundTestTopic, Offset: int64(i), Resolution: boundReplayCommitted})
	}
	failure, kept, compacted := owner.failure, len(owner.history), owner.compacted
	replays, replayCompacted := len(owner.resolvedReplay), owner.replayCompacted
	owner.mu.Unlock()
	if failure != nil || kept != boundHistoryWindow || compacted.Generations != 6 || compacted.FirstGeneration != 1 || compacted.LastGeneration != 6 || compacted.Accepted != 18 || compacted.Released != 6 {
		t.Fatalf("代次窗口不对: failure=%v kept=%d compacted=%+v", failure, kept, compacted)
	}
	if replays != boundReplayWindow || replayCompacted.Count != 2 || len(replayCompacted.Digest) != 32 {
		t.Fatalf("重放原件窗口不对: kept=%d compacted=%+v", replays, replayCompacted)
	}
	owner.StopFetching()
	raw, err := owner.CloseAndReport(context.Background())
	if err != nil {
		t.Fatalf("上千次再均衡之后排空收据不成立: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"Compacted":{"Generations":6,"FirstGeneration":1,"LastGeneration":6`)) {
		t.Fatalf("收据没有带上折叠汇总: %.200s", raw)
	}

	unbalanced, err := NewBoundConsumerGroup(boundTestConfig(), drainproof.New(), func(context.Context, kafkago.Message) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	unbalanced.mu.Lock()
	unbalanced.appendHistoryLocked(&boundGroupObservation{Generation: 1, Partitions: []boundPartitionObservation{{Topic: boundTestTopic, Accepted: 2, Settled: 1}}})
	for id := int32(2); id <= boundHistoryWindow+1; id++ {
		unbalanced.appendHistoryLocked(balanced(id))
	}
	failure = unbalanced.failure
	unbalanced.mu.Unlock()
	if !errors.Is(failure, drainproof.ErrHold) {
		t.Fatalf("结局不平的代次被折叠进合计却没有 HOLD: %v", failure)
	}
}

type fakeLagConn struct {
	lag     GroupLag
	offsets []PartitionOffsets
	err     error
	closed  int
}

func (f *fakeLagConn) ProbeOffsets(string, string, int32) (GroupLag, []PartitionOffsets, error) {
	return f.lag, f.offsets, f.err
}

func (f *fakeLagConn) Close() error { f.closed++; return nil }

// lag 探针:连不上、探失败都按轮报 error 并在下一轮重建连接;探成功的连接沿用到下一轮,退出时关闭。
func TestRunGroupLagProbeReportsEveryRoundAndReconnectsAfterFailure(t *testing.T) {
	broken := &fakeLagConn{err: errors.New("[16] Coordinator Not Available")}
	healthy := &fakeLagConn{lag: GroupLag{Lag: 7, MaxPartitionLag: 5}}
	queue := []*fakeLagConn{broken, healthy}
	connects := 0
	connect := func() (groupLagProbeConn, error) {
		connects++
		if connects == 1 {
			return nil, errors.New("dial tcp: connection refused")
		}
		next := queue[0]
		queue = queue[1:]
		return next, nil
	}
	type observed struct {
		target GroupLagTarget
		lag    GroupLag
		err    error
	}
	results := make(chan observed, 8)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	target := GroupLagTarget{Group: "match-rating", Topic: "match-results", Partitions: 3}
	go func() {
		defer close(done)
		runGroupLagProbe(ctx, ticks, "kafkautil.test.lag_probe", []GroupLagTarget{target}, func(got GroupLagTarget, lag GroupLag, err error) {
			results <- observed{target: got, lag: lag, err: err}
		}, connect)
	}()
	round := func() observed {
		ticks <- time.Time{}
		return waitSignal(t, results, "一轮探测的结局")
	}
	if got := round(); got.err == nil || got.target != target {
		t.Fatalf("连不上必须按目标报错: %+v", got)
	}
	if got := round(); got.err == nil {
		t.Fatalf("探失败必须报错: %+v", got)
	}
	if got := round(); got.err != nil || got.lag.Lag != 7 || got.lag.MaxPartitionLag != 5 {
		t.Fatalf("重建连接后应报出积压: %+v", got)
	}
	if got := round(); got.err != nil || got.lag.Lag != 7 {
		t.Fatalf("健康连接应沿用: %+v", got)
	}
	cancel()
	<-done
	if connects != 3 || broken.closed != 1 || healthy.closed != 1 {
		t.Fatalf("连接重建 / 关闭次数不对: connects=%d broken.closed=%d healthy.closed=%d", connects, broken.closed, healthy.closed)
	}
}

// 一轮 panic(这里在 observe 里)时,这一轮刚建的连接也要关掉,下一轮重建。以前新连接只在一轮正常返回时才赋给外层变量,
// panic 一次就漏一条 broker 连接,下一轮又新建一条。计数在探针 goroutine 退出(done 关闭)之后才读,不与它竞争。
func TestRunGroupLagProbeClosesTheConnectionOfAPanickedRound(t *testing.T) {
	first := &fakeLagConn{lag: GroupLag{Lag: 1}}
	second := &fakeLagConn{lag: GroupLag{Lag: 2}}
	queue := []*fakeLagConn{first, second}
	connects := 0
	connect := func() (groupLagProbeConn, error) {
		next := queue[connects]
		connects++
		return next, nil
	}
	results := make(chan GroupLag, 4)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	target := GroupLagTarget{Group: "match-rating", Topic: "match-results", Partitions: 3}
	go func() {
		defer close(done)
		runGroupLagProbe(ctx, ticks, "kafkautil.test.lag_probe_panic", []GroupLagTarget{target}, func(_ GroupLagTarget, lag GroupLag, _ error) {
			results <- lag
			if lag.Lag == 1 {
				panic("observer bug")
			}
		}, connect)
	}()
	ticks <- time.Unix(1_000_000, 0)
	if got := waitSignal(t, results, "panic 那一轮的结局"); got.Lag != 1 {
		t.Fatalf("第一轮应探到第一条连接: %+v", got)
	}
	ticks <- time.Unix(1_000_030, 0)
	if got := waitSignal(t, results, "重建连接后的一轮"); got.Lag != 2 {
		t.Fatalf("panic 之后下一轮应重建连接: %+v", got)
	}
	cancel()
	<-done
	if connects != 2 || first.closed != 1 || second.closed != 1 {
		t.Fatalf("panic 那一轮的连接必须关掉并重建: connects=%d first.closed=%d second.closed=%d", connects, first.closed, second.closed)
	}
}

// 探针循环跨轮给出 lag.CommitStall:有积压的分区上已提交位点不动才计时,位点一动从头计;探失败的那一轮不推进,
// 连接重建也不清零(状态在循环里,不在连接上)。替身的字段只在两轮之间改:改完才发下一拍,探针收到这一拍之后才读,不竞争。
func TestRunGroupLagProbeReportsCommitStallAcrossRoundsAndReconnects(t *testing.T) {
	stuck := []PartitionOffsets{{Partition: 0, Committed: 10, End: 15}, {Partition: 1, Committed: 7, End: 7}}
	first := &fakeLagConn{offsets: stuck}
	second := &fakeLagConn{offsets: stuck}
	queue := []*fakeLagConn{first, second}
	connects := 0
	connect := func() (groupLagProbeConn, error) {
		next := queue[connects]
		connects++
		return next, nil
	}
	results := make(chan GroupLag, 8)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runGroupLagProbe(ctx, ticks, "kafkautil.test.lag_probe_stall", []GroupLagTarget{{Group: "g", Topic: "t", Partitions: 2}}, func(_ GroupLagTarget, lag GroupLag, err error) {
			if err != nil {
				lag.CommitStall = -1
			}
			results <- lag
		}, connect)
	}()
	base := time.Unix(1_000_000, 0)
	step := func(round int) time.Duration {
		ticks <- base.Add(time.Duration(round) * 30 * time.Second)
		return waitSignal(t, results, "一轮探测的结局").CommitStall
	}
	stalls := []time.Duration{step(0), step(1)}
	first.err = errors.New("[16] Coordinator Not Available")
	stalls = append(stalls, step(2), step(3))
	second.offsets = []PartitionOffsets{{Partition: 0, Committed: 15, End: 16}, {Partition: 1, Committed: 7, End: 7}}
	stalls = append(stalls, step(4))
	cancel()
	<-done
	// 第 0 轮开始计;第 1 轮 30s;第 2 轮探失败(-1,不推进,连接作废);第 3 轮新连接上仍是同一位点,计到 90s;第 4 轮位点前进,从头计。
	if want := []time.Duration{0, 30 * time.Second, -1, 90 * time.Second, 0}; !slices.Equal(stalls, want) {
		t.Fatalf("CommitStall 各轮 %v, want %v", stalls, want)
	}
	if connects != 2 || first.closed != 1 || second.closed != 1 {
		t.Fatalf("探失败的连接要关掉并重建: connects=%d first.closed=%d second.closed=%d", connects, first.closed, second.closed)
	}
}

// CommitStall(handler 模式口径):有积压的分区上已提交位点不动才计时;位点一动或积压清零从头计;按分区算。
func TestCommitStallTracksBacklogWhoseCommittedOffsetDoesNotMove(t *testing.T) {
	at := func(seconds int64) time.Time { return time.Unix(1_000_000+seconds, 0) }
	part := func(partition int32, committed, end int64) PartitionOffsets {
		return PartitionOffsets{Partition: partition, Committed: committed, End: end}
	}
	var stall CommitStall
	// 高负载:每轮都有积压(合计积压从不为 0),但位点每轮都在前进 —— 不算停滞。旧判据「积压 > 0 持续 15 分钟」在这里误报。
	for round := int64(0); round < 40; round++ {
		if got := stall.Observe(at(round*30), []PartitionOffsets{part(0, 100*round, 100*round+7), part(1, 50*round, 50*round+3)}); got != 0 {
			t.Fatalf("第 %d 轮位点在前进, 不算停滞: %v", round, got)
		}
	}
	// 分区 1 卡住(位点停在 2000、积压在涨),分区 0 照常前进:组的合计位点在动,按分区照样报出分区 1 停了多久。
	const base = 40 * 30
	stall.Observe(at(base), []PartitionOffsets{part(0, 4000, 4005), part(1, 2000, 2003)})
	if got := stall.Observe(at(base+900), []PartitionOffsets{part(0, 5000, 5002), part(1, 2000, 2400)}); got != 900*time.Second {
		t.Fatalf("分区 1 的位点 900 秒没动, 应报 900s: %v", got)
	}
	if got := stall.Observe(at(base+930), []PartitionOffsets{part(0, 5100, 5100), part(1, 2001, 2400)}); got != 0 {
		t.Fatalf("位点动了应从头计: %v", got)
	}
	if got := stall.Observe(at(base+960), []PartitionOffsets{part(0, 5100, 5100), part(1, 2001, 2400)}); got != 30*time.Second {
		t.Fatalf("位点又停了 30 秒: %v", got)
	}
	if got := stall.Observe(at(base+990), []PartitionOffsets{part(0, 5100, 5100), part(1, 2400, 2400)}); got != 0 {
		t.Fatalf("积压清零不算停滞: %v", got)
	}
	// 安静分区卡在一条上(积压不涨、位点不动)也算停滞:handler 模式没有「已落库却挂着积压」的情形,这正是毒丸。
	stall.Observe(at(base+1000), []PartitionOffsets{part(1, 2400, 2401)})
	if got := stall.Observe(at(base+1900), []PartitionOffsets{part(1, 2400, 2401)}); got != 900*time.Second {
		t.Fatalf("安静分区上卡住的那一条也要报: %v", got)
	}
	// 没提交过的分区(committed = -1)从日志起点算积压,同样计时。
	var fresh CommitStall
	fresh.Observe(at(0), []PartitionOffsets{{Partition: 0, Committed: -1, Start: 5, End: 9}})
	if got := fresh.Observe(at(600), []PartitionOffsets{{Partition: 0, Committed: -1, Start: 5, End: 9}}); got != 600*time.Second {
		t.Fatalf("从没提交过、积压不动的分区应计时: %v", got)
	}
}

// RequireArrivals(reader 模式口径):位点停下之后没有新消息时(提交失败交还的已落库记录挂着积压)不计时,
// 来了新消息而位点不动才计;消费者把新消息连同交还的几条一并提交后,位点动了就从头计。
func TestCommitStallRequiringArrivalsIgnoresReleasedBacklogOnQuietPartition(t *testing.T) {
	at := func(seconds int64) time.Time { return time.Unix(1_000_000+seconds, 0) }
	stall := CommitStall{RequireArrivals: true}
	for _, seconds := range []int64{0, 900, 3600, 86400} {
		if got := stall.Observe(at(seconds), []PartitionOffsets{{Partition: 0, Committed: 10, End: 12}}); got != 0 {
			t.Fatalf("没有新消息的积压不算卡住(第 %d 秒): %v", seconds, got)
		}
	}
	stall.Observe(at(90000), []PartitionOffsets{{Partition: 0, Committed: 10, End: 13}})
	if got := stall.Observe(at(90900), []PartitionOffsets{{Partition: 0, Committed: 10, End: 20}}); got != 900*time.Second {
		t.Fatalf("新消息到了 900 秒位点仍不动, 应报 900s: %v", got)
	}
	if got := stall.Observe(at(90930), []PartitionOffsets{{Partition: 0, Committed: 20, End: 21}}); got != 0 {
		t.Fatalf("位点动了应从头计: %v", got)
	}
	if got := stall.Observe(at(91830), []PartitionOffsets{{Partition: 0, Committed: 20, End: 21}}); got != 0 {
		t.Fatalf("位点停下之后没有新消息, 仍不计时: %v", got)
	}
}

// computeGroupOffsets 按分区号升序给出读到的位点,lag 与 computeGroupLag 完全相同(后者只是丢掉位点)。
func TestComputeGroupOffsetsReturnsPartitionOffsetsInOrder(t *testing.T) {
	f := &groupOffsetFake{
		committed: map[int32]int64{0: 10, 1: -1, 2: 30},
		start:     map[int32]int64{0: 0, 1: 4, 2: 0},
		end:       map[int32]int64{0: 15, 1: 9, 2: 30},
	}
	lag, offsets, err := computeGroupOffsets(f, "g", "t", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []PartitionOffsets{{Partition: 0, Committed: 10, Start: 0, End: 15}, {Partition: 1, Committed: -1, Start: 4, End: 9}, {Partition: 2, Committed: 30, Start: 0, End: 30}}
	if !slices.Equal(offsets, want) {
		t.Fatalf("位点 %+v, want %+v", offsets, want)
	}
	if lag != (GroupLag{Lag: 10, MaxPartitionLag: 5, Uncommitted: 1}) {
		t.Fatalf("lag %+v", lag)
	}
	for i, p := range want {
		if backlog := p.Backlog(); backlog != []int64{5, 5, 0}[i] {
			t.Fatalf("分区 %d 积压 %d", p.Partition, backlog)
		}
	}
}
