package kafka

// L-04(存储不可用时的延后,docs/design/db-task-store-unavailable-deferral.md)在 kafka 包里待补的单测:
// docs/ops/godb-pipeline-launch-todo.md §2.3 的第 1-7 条(第 8 条在 run_db_op_test.go)。
//
// claimCounter 是挂在 go-redis 上的测试 Hook:统计 retryClaimScript 的 EVAL 次数;pushBack 非 nil 时把认领到的
// 载荷用另一个 client LPUSH 回 ready,模拟「认领后立即失败、搬回 ready」—— 旧实现一拍会对同一批任务反复认领 200 次。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	"db/internal/storeerr"
	db_proto "proto/db"
	"shared/placement"

	"github.com/IBM/sarama"
	"github.com/alicebob/miniredis/v2"
	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type claimCounter struct {
	mu       sync.Mutex
	claims   int
	pushBack redis.Cmdable
	readyKey string
}

func (c *claimCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *claimCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (c *claimCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if cmd.Name() != "eval" || len(args) < 2 {
			return err
		}
		if script, ok := args[1].(string); !ok || script != retryClaimScript {
			return err
		}
		c.mu.Lock()
		c.claims++
		push := c.pushBack
		c.mu.Unlock()
		if push == nil || err != nil {
			return err
		}
		if evalCmd, ok := cmd.(*redis.Cmd); ok {
			if payload, ok := evalCmd.Val().(string); ok {
				_ = push.LPush(ctx, c.readyKey, payload).Err()
			}
		}
		return err
	}
}

func (c *claimCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims
}

// newRetryTestConsumer 建一个只用于重试循环的消费者:独立的、挂了 counter 的 client,按实例登记认领
// (retryClaimScript),派发到 w 的 taskCh(没有 goroutine 在消费,任务就留在通道里供断言)。
func newRetryTestConsumer(t *testing.T, w *worker, mr *miniredis.Miniredis, instanceID string, counter *claimCounter) *KeyOrderedKafkaConsumer {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	rc.AddHook(counter)
	owner := newRetryOwnership(rc, w.topic, w.retryQueueKey, instanceID, 30*time.Second)
	return &KeyOrderedKafkaConsumer{
		redisClient:        rc,
		topic:              w.topic,
		partitionCount:     1,
		workers:            map[int32]*worker{0: w},
		ctx:                w.ctx,
		retryQueueKey:      w.retryQueueKey,
		retryProcessingKey: owner.processingKey,
		retryDeadQueueKey:  w.retryDeadQueueKey,
		retryMaxTimes:      3,
		retryOwner:         owner,
	}
}

// seedReadyWrites 往 ready 里放 n 条带原始分区 0 的写任务(key 各不相同)。
func seedReadyWrites(t *testing.T, w *worker, firstKey uint64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		task := makeWriteTask(t, firstKey+uint64(i), placementTestMsgType, 10).dbTask
		require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))
	}
}

func drainDispatched(w *worker) []*workerTask {
	var out []*workerTask
	for {
		select {
		case task := <-w.taskCh:
			out = append(out, task)
		default:
			return out
		}
	}
}

// fakeTimeoutNetErr 是一个未打存储侧标记的超时类 net.Error(Redis 侧的超时长这样)。
type fakeTimeoutNetErr struct{}

func (fakeTimeoutNetErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutNetErr) Timeout() bool   { return true }
func (fakeTimeoutNetErr) Temporary() bool { return true }

var _ net.Error = fakeTimeoutNetErr{}

func storeTagged(kind storeerr.Kind) error {
	return fmt.Errorf("db write failed: %w", &storeerr.Error{Kind: kind, Err: errors.New("driver error")})
}

// 第 1 条:四类存储侧标记对写豁免、对读不豁免;原有四种豁免不变;未打标记的网络错误(只可能来自 Redis)不豁免。
func TestRetryBudgetExempt_StoreTaggedErrors(t *testing.T) {
	cases := []struct {
		name      string
		cause     error
		wantWrite bool
		wantRead  bool
	}{
		{name: "conn_refused", cause: storeTagged(storeerr.KindConnRefused), wantWrite: true},
		{name: "conn_lost", cause: storeTagged(storeerr.KindConnLost), wantWrite: true},
		{name: "store_readonly", cause: storeTagged(storeerr.KindReadOnly), wantWrite: true},
		{name: "store_busy", cause: storeTagged(storeerr.KindBusy), wantWrite: true},
		{name: "ordering lock busy", cause: fmt.Errorf("%w: k", errOrderingLockBusy), wantWrite: true, wantRead: true},
		{name: "placement frozen", cause: fmt.Errorf("%w: k", errPlacementFrozen), wantWrite: true, wantRead: true},
		{name: "placement moved", cause: fmt.Errorf("%w: k", errPlacementMoved), wantWrite: true, wantRead: true},
		{name: "placement store unavailable", cause: fmt.Errorf("%w: k", errPlacementStoreUnavailable), wantWrite: true},
		{name: "untagged net.OpError", cause: fmt.Errorf("placement lookup failed: %w",
			&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")})},
		{name: "untagged timeout net.Error", cause: fmt.Errorf("cache: %w", fakeTimeoutNetErr{})},
		{name: "untagged deadline exceeded", cause: fmt.Errorf("cache: %w", context.DeadlineExceeded)},
		{name: "lease lost still consumes the budget", cause: fmt.Errorf("%w: owner changed", errOrderingLeaseLost)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantWrite, retryBudgetExempt(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, tc.cause), "write")
			assert.Equal(t, tc.wantRead, retryBudgetExempt(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_READ}, tc.cause), "read")
		})
	}
}

// 第 2 条:deferReason 的 13 个取值逐一覆盖;storeerr 标记优先于 errPlacementStoreUnavailable;
// 未打标记的 DeadlineExceeded 与 Redis 网络错误归 redis_unavailable。
func TestDeferReason_EveryReason(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  string
	}{
		{name: "conn refused", cause: storeTagged(storeerr.KindConnRefused), want: metrics.DeferConnRefused},
		{name: "conn lost", cause: storeTagged(storeerr.KindConnLost), want: metrics.DeferConnLost},
		{name: "read only", cause: storeTagged(storeerr.KindReadOnly), want: metrics.DeferStoreReadOnly},
		{name: "busy", cause: storeTagged(storeerr.KindBusy), want: metrics.DeferStoreBusy},
		{name: "cooling down", cause: fmt.Errorf("%w: %w", errPlacementStoreUnavailable, proto_sql.ErrStoreCoolingDown),
			want: metrics.DeferStoreCooldown},
		{name: "store open failed", cause: fmt.Errorf("%w: storage=7", errPlacementStoreUnavailable), want: metrics.DeferStoreOpenFailed},
		{name: "ordering lock busy", cause: fmt.Errorf("%w: k", errOrderingLockBusy), want: metrics.DeferOrderingLockBusy},
		{name: "lease lost", cause: fmt.Errorf("%w: owner changed", errOrderingLeaseLost), want: metrics.DeferLeaseLost},
		{name: "placement frozen", cause: fmt.Errorf("%w: k", errPlacementFrozen), want: metrics.DeferPlacementFrozen},
		{name: "placement moved", cause: fmt.Errorf("%w: k", errPlacementMoved), want: metrics.DeferPlacementMoved},
		{name: "expand lock busy", cause: errTaskDeferred, want: metrics.DeferExpandLockBusy},
		{name: "redis net error", cause: fmt.Errorf("placement lookup failed: %w",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), want: metrics.DeferRedisUnavailable},
		{name: "other", cause: errors.New("unsupported op: frobnicate"), want: metrics.DeferOther},
		// 优先级与边界。
		{name: "store tag wins over store unavailable", cause: fmt.Errorf("%w: %w", errPlacementStoreUnavailable,
			&storeerr.Error{Kind: storeerr.KindConnLost, Err: errors.New("dial tcp: connection refused")}), want: metrics.DeferConnLost},
		{name: "untagged deadline exceeded is redis", cause: fmt.Errorf("cache: %w", context.DeadlineExceeded),
			want: metrics.DeferRedisUnavailable},
		{name: "untagged redis timeout is redis", cause: fmt.Errorf("verify: %w", fakeTimeoutNetErr{}),
			want: metrics.DeferRedisUnavailable},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, deferReason(tc.cause))
		})
		seen[tc.want] = true
	}
	all := []string{
		metrics.DeferConnRefused, metrics.DeferConnLost, metrics.DeferStoreReadOnly, metrics.DeferStoreBusy,
		metrics.DeferStoreCooldown, metrics.DeferStoreOpenFailed, metrics.DeferOrderingLockBusy, metrics.DeferLeaseLost,
		metrics.DeferPlacementFrozen, metrics.DeferPlacementMoved, metrics.DeferExpandLockBusy,
		metrics.DeferRedisUnavailable, metrics.DeferOther,
	}
	require.Len(t, all, 13)
	for _, reason := range all {
		assert.Truef(t, seen[reason], "reason %s is not covered", reason)
	}
}

// 第 3 条:ready 里 3 条时一拍只认领 3 次(认领后立即搬回 ready 也不会被同一拍再认领);旧实现会认领 200 次。
func TestConsumeRetryQueue_ClaimsAtMostReadyLengthPerTick(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	seedReadyWrites(t, w, 9401, 3)
	counter := &claimCounter{pushBack: w.redisClient, readyKey: w.retryQueueKey}
	c := newRetryTestConsumer(t, w, mr, "db-a:6000", counter)

	c.consumeRetryQueue()

	assert.Equal(t, 3, counter.count(), "one tick claims at most the ready length sampled at its start")
	dispatched := drainDispatched(w)
	require.Len(t, dispatched, 3)
	ids := map[string]bool{}
	for _, task := range dispatched {
		assert.Equal(t, int32(1), task.dbTask.RetryCount, "each claim consumes exactly one attempt")
		ids[task.dbTask.TaskId] = true
	}
	assert.Len(t, ids, 3, "three distinct tasks are claimed, none twice")
}

// 第 3 条(续):两个实例共用一个 Redis 时各自至多认领 ready 的长度。
func TestConsumeRetryQueue_TwoConsumersEachCapped(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	seedReadyWrites(t, w, 9411, 3)
	counterA := &claimCounter{pushBack: w.redisClient, readyKey: w.retryQueueKey}
	counterB := &claimCounter{pushBack: w.redisClient, readyKey: w.retryQueueKey}
	a := newRetryTestConsumer(t, w, mr, "db-a:6000", counterA)
	b := newRetryTestConsumer(t, w, mr, "db-b:6000", counterB)

	a.consumeRetryQueue()
	b.consumeRetryQueue()

	assert.Equal(t, 3, counterA.count())
	assert.Equal(t, 3, counterB.count())
}

// coolingStores 让本 zone 主库恒处于冷却期。
type coolingStores struct {
	*fakeStores
}

func (coolingStores) ReportConnLost(uint32, error) {}
func (coolingStores) ReportHealthy(uint32)         {}
func (coolingStores) CoolingDown(uint32) bool      { return true }

var _ storeHealth = coolingStores{}

func retryQueueGauge(t *testing.T, queue string) float64 {
	t.Helper()
	return metricCount(t, "db_retry_queue_length", map[string]string{"queue": queue})
}

// 第 4 条:主库冷却中整拍跳过(一次都不认领),三个队列长度照常采样。
func TestConsumeRetryQueue_SkipsTickWhileHomeStoreCoolingDown(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	seedReadyWrites(t, w, 9421, 3)
	counter := &claimCounter{}
	c := newRetryTestConsumer(t, w, mr, "db-a:6000", counter)
	c.placement = &placementRouter{rc: w.redisClient, zone: testZoneID, stores: coolingStores{fakeStores: newFakeStores()}}

	c.consumeRetryQueue()

	assert.Zero(t, counter.count())
	assert.Equal(t, float64(3), retryQueueGauge(t, metrics.RetryQueueReady))
	assert.Equal(t, float64(0), retryQueueGauge(t, metrics.RetryQueueProcessing))
	assert.Equal(t, float64(0), retryQueueGauge(t, metrics.RetryQueueDead))
	assert.Empty(t, drainDispatched(w))
}

// 第 5 条:读任务用完预算:不进死信,收据被摘掉,read_abandoned{budget_exhausted}+1。
func TestConsumeOneRetryTask_ExhaustedReadIsDroppedNotDeadLettered(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	read := makeReadTask(t, 9431, placementTestMsgType, "exhausted").dbTask
	read.RetryCount = 3
	require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, read, 5, 0))
	c := newRetryTestConsumer(t, w, mr, "db-a:6000", &claimCounter{})
	before := metricCount(t, "db_task_read_abandoned_total", map[string]string{"reason": metrics.ReadAbandonedBudgetExhausted})

	require.True(t, c.consumeOneRetryTask())

	assert.Zero(t, redisListLen(t, w, w.retryDeadQueueKey), "reads are never dead-lettered")
	assert.Zero(t, redisListLen(t, w, c.retryProcessingKey), "the receipt is acked")
	assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
	assert.Empty(t, drainDispatched(w))
	assert.Equal(t, before+1, metricCount(t, "db_task_read_abandoned_total",
		map[string]string{"reason": metrics.ReadAbandonedBudgetExhausted}))
}

// 第 6 条:写任务遇到存储侧故障(conn_lost):连续 4 轮重试后重试次数仍为 0、仍在 ready、不进死信,
// 载荷逐字节不变;deferred{write,conn_lost} 每轮 +1。
func TestStoreDeferral_RetryWriteKeepsBudgetAndPayload(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	dbOpHandlers[db_proto.DBOp_DB_OP_WRITE] = func(context.Context, *proto2mysql.DB, *db_proto.DBTask, proto.Message) (writeOutcome, error) {
		return writeOutcome{kind: writeApplied}, storeTagged(storeerr.KindConnLost)
	}
	task := makeWriteTask(t, 9441, placementTestMsgType, 10).dbTask
	require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))
	initial, err := w.redisClient.LIndex(w.ctx, w.retryQueueKey, -1).Bytes()
	require.NoError(t, err)
	before := metricCount(t, "db_task_deferred_total", map[string]string{"op": "write", "reason": metrics.DeferConnLost})

	for round := 0; round < 4; round++ {
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed, "round %d: a store outage must never dead-letter the write", round)
		w.handleTask(claimed, true)
		assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey), "round %d", round)
		assert.Zero(t, redisListLen(t, w, w.retryDeadQueueKey), "round %d", round)
		assert.Zero(t, redisListLen(t, w, w.retryProcessingKey), "round %d", round)
		assert.Zero(t, oldestReadyTask(t, w).RetryCount, "round %d: a store outage is not a failed attempt", round)
	}

	final, err := w.redisClient.LIndex(w.ctx, w.retryQueueKey, -1).Bytes()
	require.NoError(t, err)
	assert.Equal(t, initial, final, "the retry payload must be byte-identical after the deferrals")
	assert.Equal(t, before+4, metricCount(t, "db_task_deferred_total", map[string]string{"op": "write", "reason": metrics.DeferConnLost}))
}

// 第 7 条:延后与死信按原因计数;毒消息无论走 persistPoisonMessage 还是 ConsumeClaim 的解码失败路径都计一次。
func TestDeferAndDeadLetterCounters(t *testing.T) {
	deferred := func(reason string) float64 {
		return metricCount(t, "db_task_deferred_total", map[string]string{"op": "write", "reason": reason})
	}
	deadLetter := func(reason string) float64 {
		return metricCount(t, "db_task_dead_letter_total", map[string]string{"reason": reason})
	}

	t.Run("placement frozen defers", func(t *testing.T) {
		w, mr, _ := newTestWorker(t, 0)
		const key uint64 = 9451
		setRedisValue(t, mr, placement.Key(key), "102:3:frozen:run-a")
		before := deferred(metrics.DeferPlacementFrozen)
		w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)
		assert.Equal(t, before+1, deferred(metrics.DeferPlacementFrozen))
	})
	t.Run("ordering incomparable dead-letters", func(t *testing.T) {
		w, _, _ := newTestWorker(t, 0)
		wt := makeWriteTask(t, 9452, placementTestMsgType, 10)
		before := deadLetter(metrics.DeadLetterOrderingIncomparable)
		w.quarantineOrderingConflict(wt, wt.dbTask, metrics.DeadLetterOrderingIncomparable, "test conflict")
		assert.Equal(t, before+1, deadLetter(metrics.DeadLetterOrderingIncomparable))
		assert.Equal(t, int64(1), redisListLen(t, w, w.retryDeadQueueKey))
	})
	t.Run("poison via persistPoisonMessage", func(t *testing.T) {
		w, _, _ := newTestWorker(t, 0)
		before := deadLetter(metrics.DeadLetterPoison)
		msg := &sarama.ConsumerMessage{Topic: w.topic, Partition: 0, Offset: 80, Value: []byte{0xff, 0x00, 0xff}}
		// 入队层换成 submit + done(S1);directEnqueue 在 submit 返回前就调 done,channel 只为接结果。
		dlqErr := make(chan error, 1)
		persistPoisonMessage(w.ctx, directEnqueue{rc: w.redisClient}, w.retryDeadQueueKey, msg, errors.New("bad payload"),
			func(err error) { dlqErr <- err })
		require.NoError(t, <-dlqErr)
		assert.Equal(t, before+1, deadLetter(metrics.DeadLetterPoison))
	})
	t.Run("poison via ConsumeClaim", func(t *testing.T) {
		w, _, _ := newTestWorker(t, 0)
		c := &KeyOrderedKafkaConsumer{
			topic: w.topic, partitionCount: 1, workers: map[int32]*worker{0: w}, ctx: w.ctx,
			redisClient: w.redisClient, retryDeadQueueKey: w.retryDeadQueueKey,
		}
		sessionCtx, cancelSession := context.WithCancel(context.Background())
		defer cancelSession()
		session := &recordingSession{ctx: sessionCtx, claims: map[string][]int32{w.topic: {0}}}
		claim := &fakeClaim{topic: w.topic, partition: 0, messages: make(chan *sarama.ConsumerMessage, 1)}
		claim.messages <- &sarama.ConsumerMessage{Topic: w.topic, Partition: 0, Offset: 70, Value: []byte{0xff, 0x00, 0xff}}
		close(claim.messages)
		before := deadLetter(metrics.DeadLetterPoison)

		require.NoError(t, (&consumerGroupHandler{consumer: c}).ConsumeClaim(session, claim))

		assert.Equal(t, before+1, deadLetter(metrics.DeadLetterPoison))
		assert.Equal(t, []int64{70}, session.marked(), "the poison offset ACKs after the dead-letter copy is durable")
	})
}
