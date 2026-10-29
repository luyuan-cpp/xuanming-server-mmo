package kafka

// 停机顺序(KeyOrderedKafkaConsumer.Stop 的五步、ConsumeClaim 的 drainClaimForShutdown、claimAcker.untrack)的单测。
//
// 替身:fakeClaim、fakeInspector(group_handoff_test.go)、recordingSession / recordingHarness(key_ordered_consumer_test.go);
// fakeConsumerGroup 按 sarama v1.43.1 的语义驱动一个 session:Consume 从入口持有 lock(模拟 consumerGroup.lock)直到会话
// 释放完;会话 ctx 派生自 Consume 的 ctx,任一 ConsumeClaim 返回即取消会话,全部返回后调 Cleanup、记一次「会话释放
// (最后一次提交)」再返回;Close 先关 closed,再像 leave() 一样拿 lock,拿到之后才记 "close" 事件与当时的状态。
// 所有等待都有上限;不靠墙钟 sleep 推进剧情。

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	db_locker "db/internal/locker"
	db_proto "proto/db"

	"github.com/IBM/sarama"
	"github.com/alicebob/miniredis/v2"
	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// kafkaMessageFor 把一条 DBTask 包成指定分区 / offset 的 Kafka 消息(key 为玩家 id 的十进制串)。
func kafkaMessageFor(t *testing.T, topic string, partition int32, offset int64, task *db_proto.DBTask) *sarama.ConsumerMessage {
	t.Helper()
	body, err := proto.Marshal(task)
	require.NoError(t, err)
	return &sarama.ConsumerMessage{
		Topic: topic, Partition: partition, Offset: offset,
		Key: []byte(strconv.FormatUint(task.Key, 10)), Value: body,
	}
}

// shutdownFixture:一个 worker(由测试 goroutine 逐条 handleTask)、一个直接拼出来的消费者、一个 session 与一个 claim。
// gate 关闭之前,blocked 里的任务在 SQL 之后停住(harness 的 afterCall),并把 taskID 送进 entered。
type shutdownFixture struct {
	w       *worker
	mr      *miniredis.Miniredis
	h       *recordingHarness
	c       *KeyOrderedKafkaConsumer
	handler *consumerGroupHandler
	session *recordingSession
	claim   *fakeClaim

	cancelSession context.CancelFunc

	gate     chan struct{}
	gateOnce sync.Once
	entered  chan string
	blocked  map[string]bool

	stopOnce   sync.Once
	stopWorker context.CancelFunc
	loopDone   chan struct{}

	// acceptLimit > 0 时 worker 循环处理这么多条之后不再收任务(worker 卡住、通道不再被读),只等 workerCtx 结束。
	acceptLimit int
}

// shutdownOption 在 worker 循环启动之前调整 fixture。
type shutdownOption func(*shutdownFixture)

// withTaskChCap 把 worker 通道换成容量 n(0 = 无缓冲:worker 正在处理一条时,下一条的派发就会阻塞)。
func withTaskChCap(n int) shutdownOption {
	return func(f *shutdownFixture) { f.w.taskCh = make(chan *workerTask, n) }
}

// withAcceptLimit 让 worker 循环处理 n 条之后不再收任务。
func withAcceptLimit(n int) shutdownOption {
	return func(f *shutdownFixture) { f.acceptLimit = n }
}

func newShutdownFixture(t *testing.T, opts ...shutdownOption) *shutdownFixture {
	t.Helper()
	w, mr, h := newTestWorker(t, 0)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	w.ctx = workerCtx
	c := &KeyOrderedKafkaConsumer{
		topic:                w.topic,
		partitionCount:       1,
		workers:              map[int32]*worker{0: w},
		ctx:                  workerCtx,
		cancel:               stopWorker,
		redisClient:          w.redisClient,
		retryDeadQueueKey:    w.retryDeadQueueKey,
		pullStop:             make(chan struct{}),
		consumeDone:          make(chan struct{}),
		shutdownDrainTimeout: claimDrainTimeout,
	}
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	f := &shutdownFixture{
		w: w, mr: mr, h: h, c: c,
		handler:       &consumerGroupHandler{consumer: c},
		session:       &recordingSession{ctx: sessionCtx, claims: map[string][]int32{w.topic: {0}}},
		claim:         &fakeClaim{topic: w.topic, partition: 0, messages: make(chan *sarama.ConsumerMessage, 16)},
		cancelSession: cancelSession,
		gate:          make(chan struct{}),
		entered:       make(chan string, 16),
		blocked:       map[string]bool{},
		stopWorker:    stopWorker,
		loopDone:      make(chan struct{}),
	}
	h.setAfterCall(func(call recordedCall) {
		if f.blocked[call.taskID] {
			f.entered <- call.taskID
			<-f.gate
		}
	})
	for _, opt := range opts {
		opt(f)
	}
	taskCh := w.taskCh
	go func() {
		defer close(f.loopDone)
		accepted := 0
		for {
			if f.acceptLimit > 0 && accepted >= f.acceptLimit {
				<-workerCtx.Done()
				return
			}
			select {
			case <-workerCtx.Done():
				return
			case task := <-taskCh:
				accepted++
				w.handleTask(task, true)
			}
		}
	}()
	t.Cleanup(func() {
		f.shutdownWorker()
		cancelSession()
	})
	return f
}

func (f *shutdownFixture) openGate() { f.gateOnce.Do(func() { close(f.gate) }) }

// shutdownWorker 放行被挡住的任务、停掉 worker 循环并等它退出(在途的那一条做完)。
func (f *shutdownFixture) shutdownWorker() {
	f.stopOnce.Do(func() {
		f.openGate()
		f.stopWorker()
		<-f.loopDone
	})
}

// addWrite 往 claim 里放一条写(每条一个玩家,避免合并);block 为 true 时它会在 SQL 之后停在 gate 上。
func (f *shutdownFixture) addWrite(t *testing.T, offset int64, block bool) *db_proto.DBTask {
	t.Helper()
	task := makeWriteTask(t, uint64(9300+offset), placementTestMsgType, uint64(offset)+1).dbTask
	if block {
		f.blocked[task.TaskId] = true
	}
	f.claim.messages <- kafkaMessageFor(t, f.w.topic, 0, offset, task)
	return task
}

func (f *shutdownFixture) startClaim() <-chan error {
	done := make(chan error, 1)
	go func() { done <- f.handler.ConsumeClaim(f.session, f.claim) }()
	return done
}

func waitClaimResult(t *testing.T, done <-chan error) error {
	t.Helper()
	return waitClaimResultWithin(t, done, 5*time.Second)
}

// waitClaimResultWithin 在 d 内等 ConsumeClaim 返回,超时判失败。
func waitClaimResultWithin(t *testing.T, done <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("ConsumeClaim did not return within %v", d)
		return nil
	}
}

// offsetMsg 是只带 offset 的 Kafka 消息(claimAcker 只看 offset)。
func offsetMsg(offset int64) *sarama.ConsumerMessage {
	return &sarama.ConsumerMessage{Topic: "test-db-task", Partition: 0, Offset: offset}
}

// closedWithin 报告 ch 是否在 d 内关闭。
func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

func waitEntered(t *testing.T, f *shutdownFixture, taskID string) {
	t.Helper()
	select {
	case got := <-f.entered:
		require.Equal(t, taskID, got)
	case <-time.After(5 * time.Second):
		t.Fatalf("task %s never reached its SQL", taskID)
	}
}

// 已派发的 3 条(其中第 2 条卡在 SQL 之后、第 3 条在 worker 通道里排队)在撤拉取之后全部做完并标记,
// ConsumeClaim 返回 nil。
func TestConsumeClaim_ShutdownDrainsDispatchedTasks(t *testing.T) {
	f := newShutdownFixture(t)
	f.addWrite(t, 0, false)
	blocked := f.addWrite(t, 1, true)
	f.addWrite(t, 2, false)
	done := f.startClaim()

	waitEntered(t, f, blocked.TaskId)
	require.Eventually(t, func() bool { return len(f.w.taskCh) == 1 }, 5*time.Second, 5*time.Millisecond,
		"the third task must be dispatched before shutdown starts")
	f.c.stopPulling()
	f.openGate()

	require.NoError(t, waitClaimResult(t, done))
	assert.Equal(t, []int64{0, 1, 2}, f.session.marked(), "every dispatched task completes and is marked before the claim returns")
}

// 撤拉取之后 claim 里还有的消息不 track、不派发、不执行。
func TestConsumeClaim_ShutdownStopsTakingMessages(t *testing.T) {
	f := newShutdownFixture(t)
	f.c.stopPulling()
	f.addWrite(t, 0, false)

	require.NoError(t, waitClaimResult(t, f.startClaim()))
	assert.Empty(t, f.h.snapshot(), "nothing may run after pulling stopped")
	assert.Zero(t, len(f.w.taskCh))
	assert.Empty(t, f.session.marked())
	assert.Equal(t, 1, len(f.claim.messages), "the message is left for the next owner to replay")
}

// 排空期间会话被取消(兄弟 claim 先排空返回,sarama 取消整个会话):本 claim 的 claimCtx 不随之取消,
// 在途任务照常走完复核、缓存与游标,并被标记。
func TestConsumeClaim_ShutdownIgnoresSiblingSessionCancel(t *testing.T) {
	f := newShutdownFixture(t)
	task := f.addWrite(t, 0, true)
	done := f.startClaim()

	waitEntered(t, f, task.TaskId)
	f.c.stopPulling()
	f.cancelSession()
	f.openGate()

	require.NoError(t, waitClaimResult(t, done))
	assert.Equal(t, []int64{0}, f.session.marked())
	_, hasCursor := appliedCursorOf(t, f.w, task.Key)
	assert.True(t, hasCursor, "the in-flight write must finish normally, not be deferred by a canceled claim")
	assert.Zero(t, redisListLen(t, f.w, f.w.retryQueueKey), "nothing is pushed to the retry queue")
}

// 排空超时:按时返回 nil,acker 作废,超时之后才完成的任务与排队中的任务都不再标记(由新属主从已提交位点重放)。
func TestConsumeClaim_ShutdownDrainTimeoutCancelsRest(t *testing.T) {
	f := newShutdownFixture(t)
	f.c.shutdownDrainTimeout = 50 * time.Millisecond
	f.addWrite(t, 0, false)
	blocked := f.addWrite(t, 1, true)
	f.addWrite(t, 2, false)
	done := f.startClaim()

	waitEntered(t, f, blocked.TaskId)
	require.Eventually(t, func() bool { return len(f.w.taskCh) == 1 }, 5*time.Second, 5*time.Millisecond)
	f.c.stopPulling()

	require.NoError(t, waitClaimResult(t, done))
	f.shutdownWorker() // 放行卡住的任务并等 worker 停下:之后不会再有标记
	assert.Equal(t, []int64{0}, f.session.marked(), "offsets after the drain timeout must not be marked")
}

// 不撤拉取、只取消会话(再平衡):claimCtx 照旧随会话取消,原有行为不变。
func TestConsumeClaim_RebalanceStillCancelsClaim(t *testing.T) {
	f := newShutdownFixture(t)
	ctxSeen := make(chan context.Context, 1)
	dbOpHandlers[db_proto.DBOp_DB_OP_READ] = func(ctx context.Context, _ *proto2mysql.DB, _ *db_proto.DBTask, _ proto.Message) (writeOutcome, error) {
		ctxSeen <- ctx
		<-f.gate
		return writeOutcome{kind: writeApplied}, nil
	}
	read := makeReadTask(t, 9390, placementTestMsgType, "rebalance").dbTask
	f.claim.messages <- kafkaMessageFor(t, f.w.topic, 0, 0, read)
	done := f.startClaim()

	var claimCtx context.Context
	select {
	case claimCtx = <-ctxSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never reached its SQL")
	}
	f.cancelSession()
	waitDone(t, claimCtx.Done(), "claim context cancellation on rebalance")
	f.openGate()

	require.NoError(t, waitClaimResult(t, done))
	assert.Empty(t, f.session.marked(), "a revoked claim does not mark anything")
}

// 派发被堵住时撤拉取:派发 select 走撤拉取一路,只把手里那条从 acker 摘掉(不 fail、不作废在途任务);
// 在途的 A 做完并标记后 ConsumeClaim 立即返回,不必等满排空时限(10s),也不等 w.ctx 取消。
// 修前派发 select 没有撤拉取一路,只能等 worker 腾出槽位或 w.ctx 取消,这里会超时。
func TestConsumeClaim_ShutdownWhileDispatchBlockedUntracks(t *testing.T) {
	f := newShutdownFixture(t, withTaskChCap(0), withAcceptLimit(1))
	a := f.addWrite(t, 0, true)
	b := f.addWrite(t, 1, false)
	done := f.startClaim()

	waitEntered(t, f, a.TaskId)
	// B 已经从 claim 取出:A 占着唯一的 worker,无缓冲通道送不进去,ConsumeClaim 停在派发 select 上。
	require.Eventually(t, func() bool { return len(f.claim.messages) == 0 }, 5*time.Second, 5*time.Millisecond)
	f.c.stopPulling()
	f.openGate()

	require.NoError(t, waitClaimResultWithin(t, done, 3*time.Second),
		"well under shutdownDrainTimeout: untrack must release the undispatched message's pending count")
	assert.Equal(t, []int64{0}, f.session.marked(), "the in-flight task completes and is marked; the undispatched one is not")
	assert.Empty(t, f.h.callsForKey(b.Key), "the undispatched message never runs; the next owner replays it")
}

// 同样堵在派发上,但在途的 A 一直不做完:按 shutdownDrainTimeout 准时返回 nil,什么都不标记,B 没执行。
func TestConsumeClaim_ShutdownDispatchBlockedReturnsWithinDrainTimeout(t *testing.T) {
	f := newShutdownFixture(t, withTaskChCap(0), withAcceptLimit(1))
	f.c.shutdownDrainTimeout = 200 * time.Millisecond
	a := f.addWrite(t, 0, true)
	b := f.addWrite(t, 1, false)
	done := f.startClaim()

	waitEntered(t, f, a.TaskId)
	require.Eventually(t, func() bool { return len(f.claim.messages) == 0 }, 5*time.Second, 5*time.Millisecond)
	f.c.stopPulling()

	require.NoError(t, waitClaimResultWithin(t, done, 2*time.Second))
	f.shutdownWorker() // 放行 A 并等 worker 停下:排空超时之后才做完的 A 不再标记
	assert.Empty(t, f.session.marked())
	assert.Empty(t, f.h.callsForKey(b.Key), "the undispatched message never runs")
}

// 排空中 worker 被取消(Stop 第②步超时后先取消 worker 再离组):drainClaimForShutdown 走 consumerDone 一路立即返回 nil,
// 之后才做完的任务不再标记。
func TestConsumeClaim_ShutdownDrainStopsWhenWorkersCanceled(t *testing.T) {
	f := newShutdownFixture(t)
	a := f.addWrite(t, 0, true)
	done := f.startClaim()

	waitEntered(t, f, a.TaskId)
	f.c.stopPulling()
	f.c.cancel()

	require.NoError(t, waitClaimResultWithin(t, done, 2*time.Second), "well under shutdownDrainTimeout")
	f.shutdownWorker()
	assert.Empty(t, f.session.marked(), "a task finishing after the drain gave up is not marked")
}

// 停机排空期间交接闸守望让出(yieldCtx 被取消):撤拉取之后 claimCtx 不随让出取消,在途任务照常做完、被标记;
// 让出只把下一次 Consume 送回闸前。
func TestConsumeClaim_ShutdownDrainIgnoresHandoffYield(t *testing.T) {
	f := newShutdownFixture(t)
	hh := newHandoffHarness(t, testZoneGroup, testLegacyGroup, 1)
	hh.forceRelease()
	hh.cluster.setGroup(testZoneGroup, groupStateEmpty, nil, map[int32]int64{0: 5})
	hh.cluster.setGroup(testLegacyGroup, groupStateStable, []int32{0}, map[int32]int64{0: 1})
	f.c.handoff = hh.h
	require.NoError(t, f.handler.Setup(f.session))
	_, yieldDone := f.handler.sessionScope(f.session)
	require.NotNil(t, yieldDone, "with a handoff the claim scope is the yield context")

	task := f.addWrite(t, 0, true)
	done := f.startClaim()
	waitEntered(t, f, task.TaskId)
	f.c.stopPulling()

	// 误部署的旧发布物拿走本 topic 的分区并推进位点:守望第 1 拍建基线,第 2 拍看到前进 → 让出。
	hh.h.watchOnce()
	hh.cluster.setCommitted(testLegacyGroup, map[int32]int64{0: 2})
	hh.h.watchOnce()
	waitDone(t, yieldDone, "handoff yield")
	f.openGate()

	require.NoError(t, waitClaimResult(t, done))
	assert.Equal(t, []int64{0}, f.session.marked(), "the in-flight task is marked although the session yielded during the drain")
	_, hasCursor := appliedCursorOf(t, f.w, task.Key)
	assert.True(t, hasCursor, "the in-flight write must finish normally, not be canceled by the yield")
	assert.Zero(t, redisListLen(t, f.w, f.w.retryQueueKey), "nothing is pushed to the retry queue")
	assert.False(t, hh.released(), "the yield still sends the next Consume back through the gate")
	require.NoError(t, f.handler.Cleanup(f.session))
}

// probedSession / probedClaim 记录 ConsumeClaim 对 session.Context() 与 claim.Messages() 的求值次数。进入 select 时
// 各 case 的通道表达式按源码顺序各求值一次,所以这两个计数能确定地指出 ConsumeClaim 停在哪个 select 上,不靠墙钟猜:
//   - claim.Messages() 是顶层 select 的最后一个 case,第 n 次求值之后 ConsumeClaim 就停在第 n 轮顶层 select 上;
//   - 消息通道关闭之后的下一次 session.Context() 求值来自关闭分支里的排空 select(worker 不调 session.Context())。
type probedSession struct {
	*recordingSession
	contextCalls atomic.Int32
}

func (s *probedSession) Context() context.Context {
	s.contextCalls.Add(1)
	return s.recordingSession.Context()
}

type probedClaim struct {
	*fakeClaim
	messagesCalls atomic.Int32
}

func (c *probedClaim) Messages() <-chan *sarama.ConsumerMessage {
	c.messagesCalls.Add(1)
	return c.fakeClaim.Messages()
}

// enterClosedMessagesDrain 派发一条卡在 SQL 之后的写,再关闭消息通道;返回时 ConsumeClaim 已经停在关闭分支的排空 select 上
// (sarama 的分区消费者遇到 ErrOffsetOutOfRange 自行关闭、会话还在的情形)。
func (f *shutdownFixture) enterClosedMessagesDrain(t *testing.T) (*db_proto.DBTask, <-chan error) {
	t.Helper()
	task := f.addWrite(t, 0, true)
	session := &probedSession{recordingSession: f.session}
	claim := &probedClaim{fakeClaim: f.claim}
	done := make(chan error, 1)
	go func() { done <- f.handler.ConsumeClaim(session, claim) }()

	waitEntered(t, f, task.TaskId)
	// 第 1 轮顶层 select 取到这条写并派发;第 2 次求值 Messages() 时通道已空,ConsumeClaim 停在第 2 轮顶层 select 上。
	require.Eventually(t, func() bool { return claim.messagesCalls.Load() >= 2 }, 5*time.Second, 5*time.Millisecond)
	before := session.contextCalls.Load()
	close(f.claim.messages)
	require.Eventually(t, func() bool { return session.contextCalls.Load() > before }, 5*time.Second, 5*time.Millisecond,
		"ConsumeClaim must reach the drain select of the closed-messages branch")
	return task, done
}

// 消息通道关闭、正在排空在途任务时停机(Stop ① 撤拉取,并取消 consumeCtx 带出会话取消):与顶层 select 一样转去
// drainClaimForShutdown,在途任务做完并标记。修前这个排空 select 没有撤拉取一路,会话取消被当成再平衡:
// acker.stop 作废已完成的位点,这里什么都没标记。
func TestConsumeClaim_ShutdownWhileDrainingClosedMessagesMarks(t *testing.T) {
	f := newShutdownFixture(t)
	task, done := f.enterClosedMessagesDrain(t)

	f.c.stopPulling()
	f.cancelSession()
	f.openGate()

	require.NoError(t, waitClaimResultWithin(t, done, 3*time.Second))
	assert.Equal(t, []int64{0}, f.session.marked(), "the in-flight task completes and is marked on the shutdown path")
	_, hasCursor := appliedCursorOf(t, f.w, task.Key)
	assert.True(t, hasCursor, "the in-flight write must finish normally, not be canceled as a rebalance")
}

// 同一位置停机、在途任务一直不做完:按 shutdownDrainTimeout 准时返回 nil,什么都不标记。修前走再平衡收尾,
// 要再等满 claimDrainTimeout(10s)才返回,这里超时。
func TestConsumeClaim_ShutdownWhileDrainingClosedMessagesReturnsWithinDrainTimeout(t *testing.T) {
	f := newShutdownFixture(t)
	f.c.shutdownDrainTimeout = 200 * time.Millisecond
	_, done := f.enterClosedMessagesDrain(t)

	f.c.stopPulling()
	f.cancelSession()

	require.NoError(t, waitClaimResultWithin(t, done, 2*time.Second))
	f.shutdownWorker() // 放行卡住的任务并等 worker 停下:排空超时之后才做完的任务不再标记
	assert.Empty(t, f.session.marked())
}

// ---------------------------------------------------------------------------
// claimAcker.untrack
// ---------------------------------------------------------------------------

// untrack 只摘「还没派发」的末尾那条:不在末尾、重复摘、没 track 过的都返回 false;摘掉之后连续前缀照常推进,
// wait 不再等它,迟到的 ack 也不会标记它。
func TestClaimAcker_UntrackRemovesOnlyUndispatchedTail(t *testing.T) {
	t.Run("只摘末尾", func(t *testing.T) {
		session := &recordingSession{}
		canceled := 0
		a := newClaimAcker(session, 0, func() { canceled++ })
		for _, o := range []int64{0, 1, 2} {
			a.track(offsetMsg(o))
		}
		assert.False(t, a.untrack(offsetMsg(1)), "a non-tail offset was handed to a worker and must stay tracked")
		assert.True(t, a.untrack(offsetMsg(2)))
		assert.False(t, a.untrack(offsetMsg(2)), "a second untrack must not release pending twice")
		assert.False(t, a.untrack(offsetMsg(7)), "an offset that was never tracked")
		assert.False(t, a.untrack(nil))

		a.ack(offsetMsg(1))
		assert.Empty(t, session.marked(), "offset 1 waits for offset 0")
		a.ack(offsetMsg(0))
		assert.Equal(t, []int64{0, 1}, session.marked())
		assert.True(t, closedWithin(a.wait(), time.Second), "the untracked offset must not keep wait open")

		assert.NotPanics(t, func() { a.ack(offsetMsg(2)) }, "a late ack for the untracked offset is a no-op")
		assert.Equal(t, []int64{0, 1}, session.marked(), "the untracked offset is never marked")
		assert.Zero(t, canceled, "untrack never cancels the claim")
	})
	t.Run("唯一一条", func(t *testing.T) {
		session := &recordingSession{}
		a := newClaimAcker(session, 0, nil)
		a.track(offsetMsg(0))
		require.True(t, a.untrack(offsetMsg(0)))
		a.mu.Lock()
		assert.Empty(t, a.order)
		assert.Empty(t, a.tracked)
		a.mu.Unlock()
		assert.True(t, closedWithin(a.wait(), time.Second))
		assert.Empty(t, session.marked())
	})
	t.Run("stop 之后", func(t *testing.T) {
		session := &recordingSession{}
		a := newClaimAcker(session, 0, nil)
		a.track(offsetMsg(0))
		a.track(offsetMsg(1))
		a.stop()
		require.True(t, a.untrack(offsetMsg(1)), "stop leaves tracked and pending alone, so the tail can still be untracked")
		a.ack(offsetMsg(0))
		assert.True(t, closedWithin(a.wait(), time.Second), "pending reaches zero exactly once")
		assert.Empty(t, session.marked(), "a stopped acker marks nothing")
	})
	t.Run("压缩之后", func(t *testing.T) {
		session := &recordingSession{}
		a := newClaimAcker(session, 0, nil)
		const n = 1100 // head 越过 1024 时 order 压缩,压缩保留尾部
		for o := int64(0); o <= n; o++ {
			a.track(offsetMsg(o))
		}
		for o := int64(0); o < n; o++ {
			a.ack(offsetMsg(o))
		}
		require.True(t, a.untrack(offsetMsg(n)), "compaction keeps the tail, so the undispatched offset is still last")
		assert.True(t, closedWithin(a.wait(), time.Second))
		assert.Len(t, session.marked(), n)
	})
}

// untrack 与别的 offset 的 ack 并发(配合 -race):连续前缀按序推进到被摘那条之前,pending 恰好归零。
func TestClaimAcker_UntrackConcurrentWithAck(t *testing.T) {
	session := &recordingSession{}
	a := newClaimAcker(session, 0, nil)
	const n = 200
	for o := int64(0); o < n; o++ {
		a.track(offsetMsg(o))
	}
	start := make(chan struct{})
	var acks sync.WaitGroup
	for o := int64(0); o < n-1; o++ {
		acks.Add(1)
		go func() {
			defer acks.Done()
			<-start
			a.ack(offsetMsg(o))
		}()
	}
	close(start)
	untracked := a.untrack(offsetMsg(n - 1))
	acks.Wait()

	assert.True(t, untracked)
	want := make([]int64, 0, n-1)
	for o := int64(0); o < n-1; o++ {
		want = append(want, o)
	}
	assert.Equal(t, want, session.marked(), "the contiguous prefix is marked in order, up to but excluding the untracked tail")
	assert.True(t, closedWithin(a.wait(), 2*time.Second))
}

// ---------------------------------------------------------------------------
// Stop 的五步
// ---------------------------------------------------------------------------

// fakeConsumerGroup 实现 sarama.ConsumerGroup,只驱动一个 session(一个 claim)。
//
// lock 模拟 sarama v1.43.1 的 consumerGroup.lock:Consume 从入口持有它,直到会话释放完(等全部 ConsumeClaim →
// Cleanup → 最后一次提交)才放;Close 先关 closed,再像 leave() 一样拿这把锁,拿到之后才算离组(记 "close"、采样状态)。
// 所以会话没释放完之前 Close 会一直等 —— Stop 第③步与会话里还在等的东西之间的隐式互等在这里看得见。
type fakeConsumerGroup struct {
	topic string
	claim *fakeClaim
	owner *KeyOrderedKafkaConsumer

	// noClaims 与 releaseNeedsOwnerCtx 在 Start 之前设置,onLeave 在 Stop 之前设置,之后只读。
	// noClaims:像没分到分区的成员一样,Setup 拿到空分配、不调 ConsumeClaim,只等会话 ctx 结束或 Close。
	noClaims bool
	// releaseNeedsOwnerCtx:会话释放(全部 ConsumeClaim 返回之后、Cleanup 之前)要等 owner.ctx 取消。它抽象模拟会话里
	// 任何只认 c.ctx 的等待(生产中是 ConsumeClaim 里的 persistPoisonMessage(c.ctx) 与 drainClaimForShutdown 的
	// consumerDone 一路);sarama 的 release 与本仓 Cleanup 并不等 c.ctx,放在 release 处只为确定性。
	releaseNeedsOwnerCtx bool
	// onLeave 在第一次 Close 离组之后调用(不持 mu、不持 lock)。
	onLeave func()

	lock sync.Mutex

	mu       sync.Mutex
	events   []string
	sessions int
	session  *recordingSession
	closed   chan struct{}

	closeCalls        int
	ctxErrAtClose     error
	processingAtClose int64
	markedAtClose     []int64
}

func newFakeConsumerGroup(topic string, claim *fakeClaim) *fakeConsumerGroup {
	return &fakeConsumerGroup{topic: topic, claim: claim, closed: make(chan struct{})}
}

func (g *fakeConsumerGroup) record(event string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, event)
}

func (g *fakeConsumerGroup) snapshotEvents() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.events...)
}

func (g *fakeConsumerGroup) sessionStarted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.session != nil
}

func (g *fakeConsumerGroup) Consume(ctx context.Context, _ []string, handler sarama.ConsumerGroupHandler) error {
	select {
	case <-g.closed:
		return sarama.ErrClosedConsumerGroup
	default:
	}
	// sarama:Consume 从入口持有 consumerGroup.lock,直到会话释放完才放。
	g.lock.Lock()
	defer g.lock.Unlock()

	g.mu.Lock()
	if g.sessions > 0 {
		g.mu.Unlock()
		// 只跑一个 session;之后的 Consume 像没有分到分区的成员一样,等到 ctx 结束或 Close。
		select {
		case <-ctx.Done():
		case <-g.closed:
		}
		return nil
	}
	g.sessions++
	sessCtx, cancelSess := context.WithCancel(ctx)
	defer cancelSess()
	claims := map[string][]int32{g.topic: {g.claim.partition}}
	if g.noClaims {
		claims = map[string][]int32{}
	}
	session := &recordingSession{ctx: sessCtx, claims: claims}
	g.session = session
	g.mu.Unlock()

	if err := handler.Setup(session); err != nil {
		return err
	}
	var wg sync.WaitGroup
	if !g.noClaims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancelSess() // sarama:任一 ConsumeClaim 返回即取消整个会话
			_ = handler.ConsumeClaim(session, g.claim)
		}()
	}
	select {
	case <-sessCtx.Done():
	case <-g.closed:
	}
	wg.Wait()
	if g.releaseNeedsOwnerCtx && g.owner != nil {
		<-g.owner.ctx.Done()
	}
	_ = handler.Cleanup(session)
	g.record("released") // sarama 在这里最后一次提交位点(offsets.Close)
	return nil
}

func (g *fakeConsumerGroup) Errors() <-chan error { return nil }

func (g *fakeConsumerGroup) Close() error {
	g.mu.Lock()
	g.closeCalls++
	if g.closeCalls > 1 {
		g.events = append(g.events, "close")
		g.mu.Unlock()
		return nil
	}
	close(g.closed)
	g.mu.Unlock()

	// sarama 的 leave():先拿 Consume 持有到会话释放完的那把锁,拿到之后才发 LeaveGroup。
	g.lock.Lock()
	g.mu.Lock()
	g.events = append(g.events, "close")
	if g.owner != nil {
		g.ctxErrAtClose = g.owner.ctx.Err()
		g.processingAtClose, _ = g.owner.redisClient.LLen(context.Background(), g.owner.retryProcessingKey).Result()
	}
	if g.session != nil {
		g.markedAtClose = g.session.marked()
	}
	onLeave := g.onLeave
	g.mu.Unlock()
	g.lock.Unlock()
	if onLeave != nil {
		onLeave()
	}
	return nil
}

func (g *fakeConsumerGroup) Pause(map[string][]int32)  {}
func (g *fakeConsumerGroup) Resume(map[string][]int32) {}
func (g *fakeConsumerGroup) PauseAll()                 {}
func (g *fakeConsumerGroup) ResumeAll()                {}

var _ sarama.ConsumerGroup = (*fakeConsumerGroup)(nil)

// newStopTestConsumer 拼一个可以真跑 Start / Stop 的消费者:Kafka 换成 fakeConsumerGroup,其余(worker、排序锁、
// 重试归属、落点)都是生产代码,跑在 miniredis 上;重试循环间隔 1h,不会自己认领。
func newStopTestConsumer(t *testing.T) (*KeyOrderedKafkaConsumer, *fakeConsumerGroup, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	const topic = "test-db-task-stop"
	readyKey := "kafka:retry:queue:" + topic
	owner := newRetryOwnership(rc, topic, readyKey, "db-stop-test:6000", 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	group := newFakeConsumerGroup(topic, &fakeClaim{topic: topic, partition: 0, messages: make(chan *sarama.ConsumerMessage, 16)})
	c := &KeyOrderedKafkaConsumer{
		consumer:             group,
		redisClient:          rc,
		topic:                topic,
		groupID:              "db_rpc_consumer_group_stop_test",
		partitionCount:       1,
		subShardCount:        1,
		isOfflineExpand:      true,
		workers:              map[int32]*worker{},
		wg:                   &sync.WaitGroup{},
		ctx:                  ctx,
		cancel:               cancel,
		locker:               db_locker.NewRedisLocker(rc),
		retryQueueKey:        readyKey,
		retryProcessingKey:   owner.processingKey,
		retryDeadQueueKey:    "kafka:dead:queue:" + topic,
		retryConsumeInterval: time.Hour,
		retryMaxTimes:        3,
		retryOwner:           owner,
		placement:            newTestPlacementRouter(rc),
		now:                  time.Now,
		pullStop:             make(chan struct{}),
		consumeDone:          make(chan struct{}),
		shutdownDrainTimeout: claimDrainTimeout,
	}
	c.workers[0] = c.buildWorker(0)
	group.owner = c
	return c, group, rc
}

func stopWithin(t *testing.T, c *KeyOrderedKafkaConsumer, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		c.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("Stop did not return within %v", d)
	}
}

// Stop 的顺序:撤拉取 → 在途任务做完、会话释放(最后一次提交)→ 离组(此时 worker 还没被取消、重试收据还在
// 本实例 processing 里)→ 取消 worker → 归还收据。
func TestStop_OrderPullDrainLeaveCancelRelease(t *testing.T) {
	h := newHarness(t)
	c, group, rc := newStopTestConsumer(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	h.setAfterCall(func(recordedCall) {
		entered <- struct{}{}
		<-gate
	})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(openGate)

	require.NoError(t, c.Start())
	require.Eventually(t, group.sessionStarted, 5*time.Second, 5*time.Millisecond)
	// 一条本实例认领了还没做完的重试收据(Start 之后放:Start 会先收回同名实例遗留的收据)。
	require.NoError(t, rc.LPush(context.Background(), c.retryProcessingKey, "receipt-in-flight").Err())
	task := makeWriteTask(t, 9395, placementTestMsgType, 1).dbTask
	group.claim.messages <- kafkaMessageFor(t, c.topic, 0, 0, task)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the write never reached its SQL")
	}

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()
	require.Eventually(t, c.pullStopped, 5*time.Second, 5*time.Millisecond)
	assert.NotContains(t, group.snapshotEvents(), "close", "the group must not be left while a dispatched task is in flight")
	openGate()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("Stop did not return")
	}

	assert.Equal(t, []string{"released", "close"}, group.snapshotEvents(), "the session is released (final commit) before leaving the group")
	assert.NoError(t, group.ctxErrAtClose, "workers are still running when the group is left")
	assert.Equal(t, int64(1), group.processingAtClose, "retry receipts are returned only after leaving the group")
	assert.Equal(t, []int64{0}, group.markedAtClose, "the in-flight task is marked before leaving the group")
	assert.Error(t, c.ctx.Err(), "workers are canceled by the end of Stop")
	ready, err := rc.LRange(context.Background(), c.retryQueueKey, 0, -1).Result()
	require.NoError(t, err)
	assert.Contains(t, ready, "receipt-in-flight", "the unfinished receipt is returned to ready")
	processing, err := rc.LLen(context.Background(), c.retryProcessingKey).Result()
	require.NoError(t, err)
	assert.Zero(t, processing)
	select {
	case <-c.consumeDone:
	default:
		t.Fatal("consumeDone must be closed after Stop")
	}
}

func TestStop_Idempotent(t *testing.T) {
	newHarness(t)
	c, group, _ := newStopTestConsumer(t)
	require.NoError(t, c.Start())
	stopWithin(t, c, 20*time.Second)
	stopWithin(t, c, time.Second)
	group.mu.Lock()
	defer group.mu.Unlock()
	assert.Equal(t, 1, group.closeCalls, "the group is left exactly once")
}

func TestStop_BeforeStartDoesNotWait(t *testing.T) {
	c, group, _ := newStopTestConsumer(t)
	stopWithin(t, c, 2*time.Second) // 远小于 shutdownDrainTimeout + consumerShutdownGrace
	group.mu.Lock()
	defer group.mu.Unlock()
	assert.Equal(t, 1, group.closeCalls)
	assert.Error(t, c.ctx.Err())
}

// sameSubShardKeys 从 base 起找 n 个与 base 落在同一子 shard 的玩家 id。
func sameSubShardKeys(base uint64, shardCount, n int) []uint64 {
	target := subShardIndex(base, shardCount)
	keys := make([]uint64, 0, n)
	for k := base; len(keys) < n; k++ {
		if subShardIndex(k, shardCount) == target {
			keys = append(keys, k)
		}
	}
	return keys
}

// Stop 时 worker 通道与子 shard 通道都满、ConsumeClaim 停在派发 select 上(MySQL 卡住时的常态):撤拉取后 claim 摘掉
// 手里那条、按排空时限收尾,会话随之释放,第②步按时结束,离组不必等 w.ctx 取消。
// 修前派发 select 没有撤拉取一路:第③步的 Close 拿不到 sarama 的锁(会话等 claim、claim 等第④步),onLeave 永远不放行,
// Stop 超时;只在第②步超时分支加 c.cancel() 也不够(claim 走 w.ctx 一路 fail 后还要等满 10s 排空),同样超时。
func TestStop_FullWorkerQueueDoesNotStallLeave(t *testing.T) {
	h := newHarness(t)
	c, group, _ := newStopTestConsumer(t)
	// 子 shard 必须 ≥2:subShardCount≤1 时 routeToSubShard 的阻塞发送没有 w.ctx 出口(另一处缺口,与本用例无关)。
	c.subShardCount = 2
	w := c.buildWorker(0)
	w.taskCh = make(chan *workerTask, 1)
	for i := range w.subShardChans {
		w.subShardChans[i] = make(chan *workerTask, 1)
	}
	c.workers[0] = w
	c.shutdownDrainTimeout = 200 * time.Millisecond

	// 6 个落在同一子 shard 的玩家:offset 0 正常写完;offset 1 卡在 SQL 之后;2 进子 shard 通道,3 在路由 goroutine
	// 手里,4 在 worker 通道里,5 停在 ConsumeClaim 的派发 select 上。
	keys := sameSubShardKeys(9500, c.subShardCount, 6)
	shard := subShardIndex(keys[0], c.subShardCount)
	tasks := make([]*db_proto.DBTask, len(keys))
	for i, k := range keys {
		tasks[i] = makeWriteTask(t, k, placementTestMsgType, uint64(i)+1).dbTask
	}
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	h.setAfterCall(func(call recordedCall) {
		if call.taskID == tasks[1].TaskId {
			entered <- struct{}{}
			<-gate
		}
	})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(openGate)

	require.NoError(t, c.Start())
	require.Eventually(t, group.sessionStarted, 5*time.Second, 5*time.Millisecond)
	group.claim.messages <- kafkaMessageFor(t, c.topic, 0, 0, tasks[0])
	group.claim.messages <- kafkaMessageFor(t, c.topic, 0, 1, tasks[1])
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("offset 1 never reached its SQL")
	}
	// offset 1 已在执行(它所在的批次已经成形)之后才放其余 4 条,堵塞位置就是确定的。
	for i := 2; i < len(tasks); i++ {
		group.claim.messages <- kafkaMessageFor(t, c.topic, 0, int64(i), tasks[i])
	}
	require.Eventually(t, func() bool {
		return len(group.claim.messages) == 0 && len(w.taskCh) == 1 && len(w.subShardChans[shard]) == 1
	}, 5*time.Second, 5*time.Millisecond, "offset 5 must be stuck in the dispatch select")

	group.onLeave = openGate // 离组之后才放行 offset 1:离组等不到,门就永远不开
	started := time.Now()
	stopWithin(t, c, c.shutdownDrainTimeout+consumerShutdownGrace+2*time.Second)
	took := time.Since(started)

	assert.Less(t, took, consumerShutdownGrace, "step ② must end on its own, not by its timeout")
	assert.Equal(t, []string{"released", "close"}, group.snapshotEvents(), "the session is released (final commit) before leaving the group")
	assert.NoError(t, group.ctxErrAtClose, "on the normal path workers are still running when the group is left")
	assert.Equal(t, []int64{0}, group.markedAtClose, "only the completed prefix is committed before leaving the group")
	for i := 2; i < len(tasks); i++ {
		assert.Empty(t, h.callsForKey(tasks[i].Key), "offset %d was queued or undispatched and must not run", i)
	}
	assert.Error(t, c.ctx.Err(), "workers are canceled by the end of Stop")
	select {
	case <-c.consumeDone:
	default:
		t.Fatal("consumeDone must be closed after Stop")
	}
}

// 第②步超时(会话释放还在等只认 c.ctx 的东西,例如毒消息落 DLQ;见 releaseNeedsOwnerCtx):先取消 worker 再离组,
// Stop 在 ② 的上限之后很快返回;离组照样排在会话释放(最后一次提交)之后。修前 ③ 的 Close 永远拿不到锁。
// 耗时约 consumerShutdownGrace(5s,常量)。
func TestStop_DrainTimeoutCancelsWorkersBeforeLeave(t *testing.T) {
	c, group, _ := newStopTestConsumer(t)
	group.releaseNeedsOwnerCtx = true
	c.shutdownDrainTimeout = 100 * time.Millisecond

	require.NoError(t, c.Start())
	require.Eventually(t, group.sessionStarted, 5*time.Second, 5*time.Millisecond)
	stopWithin(t, c, c.shutdownDrainTimeout+consumerShutdownGrace+2*time.Second)

	assert.Equal(t, []string{"released", "close"}, group.snapshotEvents(),
		"even after the timeout the session is released (final commit) before leaving the group")
	assert.Error(t, group.ctxErrAtClose, "on the timeout path workers are canceled before leaving the group, on purpose")
	assert.Error(t, c.ctx.Err())
	// ② 超时时没有等 consumeDone:消费循环在 Consume 返回之后才关它,可能略晚于 Stop 返回。
	waitDone(t, c.consumeDone, "consume loop exit")
}

// eventInspector 把 inspector 的 Close 记进消费组替身的事件序列,其余调用原样转发。
type eventInspector struct {
	groupInspector
	record func(string)
}

func (e *eventInspector) Close() error {
	e.record("inspector close")
	return e.groupInspector.Close()
}

// 交接闸还在等对方组让出时停机:撤拉取即取消闸前等待,本进程根本没入组;Stop 很快返回,交接闸的 inspector
// 在离组之后才关。
func TestStop_WhileHandoffGateWaitingReturnsPromptly(t *testing.T) {
	c, group, _ := newStopTestConsumer(t)
	hh := newHandoffHarness(t, testZoneGroup, testLegacyGroup, 1)
	// 对方 Stable 且持有本 topic 的分区:闸一直等(wait_peer_holding)。
	hh.cluster.setGroup(testLegacyGroup, groupStateStable, []int32{0}, map[int32]int64{0: 9})
	waiting := make(chan struct{}, 1)
	hh.h.sleep = func(ctx context.Context, _ time.Duration) bool {
		select {
		case waiting <- struct{}{}:
		default:
		}
		<-ctx.Done() // 不靠墙钟:只有 consumeCtx 取消(撤拉取)能结束这次等待
		return false
	}
	hh.h.newInspector = func() (groupInspector, error) {
		insp, err := hh.factory.build()
		if err != nil {
			return nil, err
		}
		return &eventInspector{groupInspector: insp, record: group.record}, nil
	}
	c.handoff = hh.h

	require.NoError(t, c.Start())
	waitDone(t, waiting, "handoff gate wait")
	stopWithin(t, c, 2*time.Second)

	assert.False(t, group.sessionStarted(), "a gated process never joins the group")
	assert.False(t, hh.released())
	assert.Equal(t, []string{"close", "inspector close"}, group.snapshotEvents(),
		"the handoff inspector is closed only after leaving the group")
	require.Equal(t, 1, hh.factory.builds())
	assert.Equal(t, 1, hh.factory.inspector(0).closeCount())
}

// 没分到分区的成员(副本数多于分区数):sarama 不调 ConsumeClaim,会话只在 ctx 取消或 Close 时结束。consumeCtx 随撤拉取
// 取消,Stop 不必等满第②步的上限,也仍是先释放会话、后离组。
func TestStop_MemberWithoutClaimsReturnsPromptly(t *testing.T) {
	c, group, _ := newStopTestConsumer(t)
	group.noClaims = true

	require.NoError(t, c.Start())
	require.Eventually(t, group.sessionStarted, 5*time.Second, 5*time.Millisecond)
	stopWithin(t, c, 2*time.Second)

	assert.Equal(t, []string{"released", "close"}, group.snapshotEvents())
	assert.NoError(t, group.ctxErrAtClose, "the session ends with consumeCtx, so the group is left before workers are canceled")
}

// 撤拉取之后重试循环一次都不认领。
func TestRetryConsumer_StopsClaimingAfterPullStop(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	seedReadyWrites(t, w, 9396, 2)
	counter := &claimCounter{}
	c := newRetryTestConsumer(t, w, mr, "db-a:6000", counter)
	c.pullStop = make(chan struct{})
	c.stopPulling()

	c.consumeRetryQueue()

	assert.Zero(t, counter.count())
	assert.Equal(t, int64(2), redisListLen(t, w, w.retryQueueKey))
}
