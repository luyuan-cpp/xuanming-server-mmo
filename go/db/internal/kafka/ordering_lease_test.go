package kafka

// 排序锁租约(ordering_lease.go)的单测。排序锁跑在 miniredis 上;时间源换成 fakeLeaseClock:
// now 由用例推进,after 把请求的等待时长写进 requested 并返回一个由用例触发的通道,jitter 恒等 —— 不依赖墙钟。
// 用例替换包级变量 orderingLeaseTimingDefault,不能 t.Parallel。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"db/internal/metrics"
	db_proto "proto/db"

	"github.com/luyuancpp/proto2mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fakeLeaseClock struct {
	mu        sync.Mutex
	now       time.Time
	requested chan time.Duration
	fire      chan time.Time
}

// installFakeLeaseTiming 换上可控的时间源,用例结束时还原。
func installFakeLeaseTiming(t *testing.T) *fakeLeaseClock {
	t.Helper()
	fc := &fakeLeaseClock{
		now:       time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
		requested: make(chan time.Duration, 64),
		fire:      make(chan time.Time),
	}
	prev := orderingLeaseTimingDefault
	timing := prev
	timing.now = fc.Now
	timing.after = fc.after
	timing.jitter = func(d time.Duration) time.Duration { return d }
	orderingLeaseTimingDefault = timing
	t.Cleanup(func() { orderingLeaseTimingDefault = prev })
	return fc
}

func (c *fakeLeaseClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeLeaseClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeLeaseClock) after(d time.Duration) <-chan time.Time {
	c.requested <- d
	return c.fire
}

// nextWait 等看门狗发出下一次等待请求,返回请求的时长。
func (c *fakeLeaseClock) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.requested:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("ordering lease watchdog did not ask to wait")
		return 0
	}
}

// tick 让看门狗当前的等待到期。
func (c *fakeLeaseClock) tick(t *testing.T) {
	t.Helper()
	select {
	case c.fire <- time.Time{}:
	case <-time.After(5 * time.Second):
		t.Fatal("ordering lease watchdog is not waiting")
	}
}

func leaseTestTask(key uint64) *db_proto.DBTask {
	return &db_proto.DBTask{Key: key, Op: db_proto.DBOp_DB_OP_WRITE, MsgType: placementTestMsgType, TaskId: "lease-test"}
}

// orderingLockRedisKey 是排序锁在 Redis 里的完整键(locker 的默认前缀 + orderingLockKey)。
func orderingLockRedisKey(w *worker, task *db_proto.DBTask) string {
	return "distributed:lock:" + orderingLockKey(w.topic, task)
}

// acquireTestLease 取锁,并登记一次幂等的释放(用例自己先释放也没关系)。
func acquireTestLease(t *testing.T, w *worker, task *db_proto.DBTask) (*orderingLease, func()) {
	t.Helper()
	lease, err := acquireOrderingLock(w.ctx, w, task)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { releaseOrderingLock(context.Background(), lease, task) }) }
	t.Cleanup(release)
	return lease, release
}

func waitDone(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

func leaseLostCount(t *testing.T, reason string) float64 {
	t.Helper()
	return metricCount(t, "db_ordering_lease_lost_total", map[string]string{"reason": reason})
}

func lockReleaseCount(t *testing.T, outcome string) float64 {
	t.Helper()
	return metricCount(t, "db_ordering_lock_release_total", map[string]string{"outcome": outcome})
}

func renewRetryCount(t *testing.T) float64 {
	t.Helper()
	return metricCount(t, "db_ordering_lease_renew_retry_total", map[string]string{})
}

// 续租成功:本地截止时间前移到「续租发出的时刻 + TTL − margin」,没有失租,下一次按正常间隔续。
func TestOrderingLease_RenewSuccessExtendsDeadline(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	fc := installFakeLeaseTiming(t)
	start := fc.Now()
	lease, _ := acquireTestLease(t, w, leaseTestTask(9801))

	assert.Equal(t, start.Add(orderingLockTTL-orderingLeaseSafetyMargin), lease.localDeadline())
	require.Equal(t, orderingLockRenewInterval, fc.nextWait(t))
	fc.advance(10 * time.Second)
	fc.tick(t)
	require.Equal(t, orderingLockRenewInterval, fc.nextWait(t), "after a successful renewal the next one is a normal interval away")

	assert.Equal(t, start.Add(10*time.Second+orderingLockTTL-orderingLeaseSafetyMargin), lease.localDeadline())
	assert.NoError(t, lease.lost())
}

// 续租出错:按 500ms、1s 退避重试并计数;Redis 恢复后续上,不失租。
func TestOrderingLease_RenewErrorRetriesWithBackoffThenRecovers(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	fc := installFakeLeaseTiming(t)
	lease, _ := acquireTestLease(t, w, leaseTestTask(9802))
	retriesBefore := renewRetryCount(t)

	require.Equal(t, orderingLockRenewInterval, fc.nextWait(t))
	mr.SetError("ERR injected redis failure")
	fc.tick(t)
	assert.Equal(t, 500*time.Millisecond, fc.nextWait(t))
	fc.tick(t)
	assert.Equal(t, time.Second, fc.nextWait(t))
	mr.SetError("")
	fc.tick(t)
	assert.Equal(t, orderingLockRenewInterval, fc.nextWait(t), "a successful renewal resets the backoff")

	assert.Equal(t, retriesBefore+2, renewRetryCount(t))
	assert.NoError(t, lease.lost())
}

// 一直出错:第 10 次失败判 renew_exhausted,beginSQL 交出的 ctx 被取消,原因里包着 errOrderingLeaseLost。
func TestOrderingLease_RenewErrorsExhaustAttemptsCancelSQL(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	fc := installFakeLeaseTiming(t)
	lease, release := acquireTestLease(t, w, leaseTestTask(9803))
	lostBefore := leaseLostCount(t, metrics.LeaseLostRenewExhausted)
	retriesBefore := renewRetryCount(t)

	sqlCtx, finish, err := lease.beginSQL()
	require.NoError(t, err)

	require.Equal(t, orderingLockRenewInterval, fc.nextWait(t))
	mr.SetError("ERR injected redis failure")
	wantBackoffs := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
		5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, want := range wantBackoffs {
		fc.tick(t)
		assert.Equalf(t, want, fc.nextWait(t), "backoff after failure %d", i+1)
	}
	fc.tick(t) // 第 10 次失败

	waitDone(t, sqlCtx.Done(), "SQL context cancellation")
	assert.ErrorIs(t, context.Cause(sqlCtx), errOrderingLeaseLost)
	assert.ErrorIs(t, lease.lost(), errOrderingLeaseLost)
	assert.Equal(t, lostBefore+1, leaseLostCount(t, metrics.LeaseLostRenewExhausted))
	assert.Equal(t, retriesBefore+9, renewRetryCount(t))

	finish(nil) // SQL 已经结束、没有被取消在途
	mr.SetError("")
	release()
}

// 时钟越过本地截止时间仍没续上:renew_deadline,SQL ctx 被取消。
func TestOrderingLease_RenewDeadlineCancelsSQL(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	fc := installFakeLeaseTiming(t)
	lease, _ := acquireTestLease(t, w, leaseTestTask(9804))
	lostBefore := leaseLostCount(t, metrics.LeaseLostRenewDeadline)
	sqlCtx, _, err := lease.beginSQL()
	require.NoError(t, err)

	require.Equal(t, orderingLockRenewInterval, fc.nextWait(t))
	fc.advance(orderingLockTTL - orderingLeaseSafetyMargin + time.Second)
	fc.tick(t)

	waitDone(t, sqlCtx.Done(), "SQL context cancellation")
	assert.ErrorIs(t, context.Cause(sqlCtx), errOrderingLeaseLost)
	assert.Equal(t, lostBefore+1, leaseLostCount(t, metrics.LeaseLostRenewDeadline))
}

// 续租发现锁已易主:立即失租(owner_changed),不重试。
func TestOrderingLease_OwnerChangedIsLostImmediately(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	fc := installFakeLeaseTiming(t)
	task := leaseTestTask(9805)
	lease, _ := acquireTestLease(t, w, task)
	lostBefore := leaseLostCount(t, metrics.LeaseLostOwnerChanged)
	retriesBefore := renewRetryCount(t)

	require.Equal(t, orderingLockRenewInterval, fc.nextWait(t))
	require.NoError(t, mr.Set(orderingLockRedisKey(w, task), "another-instance-token"))
	fc.tick(t)

	waitDone(t, lease.done, "watchdog exit")
	assert.ErrorIs(t, lease.lost(), errOrderingLeaseLost)
	assert.Equal(t, lostBefore+1, leaseLostCount(t, metrics.LeaseLostOwnerChanged))
	assert.Equal(t, retriesBefore, renewRetryCount(t), "owner change is not retried")
}

// worker ctx 取消(停机)不是失租:SQL ctx 不受影响,看门狗照常续租。
//
// 与看门狗逐拍同步,不在 stopWorker 之后立刻断言(那样看门狗还没来得及反应,守不住回归):先等它进入等待,
// 再停 worker、让这次等待到期,确认它完成一轮续租、重新进入等待。若看门狗在 select 里监听了 worker ctx,
// 停机后它会退出(tick 等不到接收方而失败)或判失租(下一次 nextWait 等不到请求而失败)。
func TestOrderingLease_WorkerStopDoesNotCancelSQL(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	fc := installFakeLeaseTiming(t)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	w.ctx = workerCtx
	lease, _ := acquireTestLease(t, w, leaseTestTask(9806))
	sqlCtx, _, err := lease.beginSQL()
	require.NoError(t, err)
	require.Equal(t, orderingLockRenewInterval, fc.nextWait(t), "the watchdog must be waiting before the worker stops")

	stopWorker()
	fc.tick(t)
	require.Equal(t, orderingLockRenewInterval, fc.nextWait(t), "the watchdog must renew and wait again after the worker stops")

	assert.NoError(t, sqlCtx.Err(), "shutting down must not cancel an in-flight write SQL")
	assert.NoError(t, lease.lost())
}

// 已过本地截止时间才要发 SQL:beginSQL 直接判失租,runDBOp 一条 SQL 都不发。
func TestOrderingLease_BeginSQLPastLocalDeadline(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	fc := installFakeLeaseTiming(t)
	task := leaseTestTask(9807)
	lease, _ := acquireTestLease(t, w, task)
	lostBefore := leaseLostCount(t, metrics.LeaseLostRenewDeadline)

	fc.advance(orderingLockTTL - orderingLeaseSafetyMargin)

	calls := 0
	handler := func(context.Context, *proto2mysql.DB, *db_proto.DBTask, proto.Message) (writeOutcome, error) {
		calls++
		return writeOutcome{kind: writeApplied}, nil
	}
	route := &placementRoute{router: w.placement, op: "write", key: task.Key, storageID: testZoneID, store: proto2mysql.NewDB()}
	err := runDBOpError(context.Background(), w.redisClient, task, &db_proto.TaskResult{}, handler, route, lease)

	require.ErrorIs(t, err, errOrderingLeaseLost)
	assert.Zero(t, calls, "no SQL may be sent past the local deadline")
	assert.Equal(t, lostBefore+1, leaseLostCount(t, metrics.LeaseLostRenewDeadline))
}

// SQL 在途时被失租取消:释放时不删锁(留给 TTL),计 skipped_sql_abandoned。
func TestOrderingLease_ReleaseSkipsAfterAbandonedSQL(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	installFakeLeaseTiming(t)
	task := leaseTestTask(9808)
	lease, release := acquireTestLease(t, w, task)
	skippedBefore := lockReleaseCount(t, metrics.LockReleaseSkippedSQLAbandoned)

	_, finish, err := lease.beginSQL()
	require.NoError(t, err)
	lease.setLost(metrics.LeaseLostOwnerChanged, errors.New("injected lease loss"))
	finish(context.Canceled)
	require.True(t, lease.abandoned())
	release()

	assert.True(t, mr.Exists(orderingLockRedisKey(w, task)), "the lock must be left to its TTL")
	assert.Equal(t, skippedBefore+1, lockReleaseCount(t, metrics.LockReleaseSkippedSQLAbandoned))
}

// 释放的三种结果:正常删锁记 released;锁已被别人改写记 not_owner(不删别人的锁);Redis 出错记 error。
func TestOrderingLease_ReleaseOutcomes(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		w, mr, _ := newTestWorker(t, 0)
		installFakeLeaseTiming(t)
		task := leaseTestTask(9809)
		_, release := acquireTestLease(t, w, task)
		before := lockReleaseCount(t, metrics.LockReleaseReleased)
		release()
		assert.False(t, mr.Exists(orderingLockRedisKey(w, task)))
		assert.Equal(t, before+1, lockReleaseCount(t, metrics.LockReleaseReleased))
	})
	t.Run("not_owner", func(t *testing.T) {
		w, mr, _ := newTestWorker(t, 0)
		installFakeLeaseTiming(t)
		task := leaseTestTask(9810)
		_, release := acquireTestLease(t, w, task)
		require.NoError(t, mr.Set(orderingLockRedisKey(w, task), "successor-token"))
		before := lockReleaseCount(t, metrics.LockReleaseNotOwner)
		release()
		value, err := mr.Get(orderingLockRedisKey(w, task))
		require.NoError(t, err)
		assert.Equal(t, "successor-token", value, "a successor's lock must never be deleted")
		assert.Equal(t, before+1, lockReleaseCount(t, metrics.LockReleaseNotOwner))
	})
	t.Run("error", func(t *testing.T) {
		w, mr, _ := newTestWorker(t, 0)
		installFakeLeaseTiming(t)
		_, release := acquireTestLease(t, w, leaseTestTask(9811))
		before := lockReleaseCount(t, metrics.LockReleaseError)
		mr.SetError("ERR injected redis failure")
		release()
		mr.SetError("")
		assert.Equal(t, before+1, lockReleaseCount(t, metrics.LockReleaseError))
	})
}

// 释放之后 SQL ctx 被取消(不泄漏),原因是 errOrderingLeaseReleased 而不是失租。
func TestOrderingLease_ReleaseCancelsSQLContext(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	installFakeLeaseTiming(t)
	lease, release := acquireTestLease(t, w, leaseTestTask(9812))
	sqlCtx, finish, err := lease.beginSQL()
	require.NoError(t, err)
	finish(nil)
	release()

	assert.Error(t, sqlCtx.Err())
	assert.ErrorIs(t, context.Cause(sqlCtx), errOrderingLeaseReleased)
	assert.NoError(t, lease.lost())
}

// 用已取消的 ctx 核锁(claim 被收回 / 停机):返回错误,但不算失租。
func TestOrderingLease_VerifyCanceledContextIsNotLeaseLoss(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	installFakeLeaseTiming(t)
	lease, _ := acquireTestLease(t, w, leaseTestTask(9813))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.Error(t, lease.verify(ctx))
	assert.NoError(t, lease.lost())
}

// 核锁成功会把本地截止时间推到「核锁发出的时刻 + TTL − margin」。
func TestOrderingLease_VerifyExtendsDeadline(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	fc := installFakeLeaseTiming(t)
	start := fc.Now()
	lease, _ := acquireTestLease(t, w, leaseTestTask(9814))
	fc.advance(20 * time.Second)

	require.NoError(t, lease.verify(context.Background()))
	assert.Equal(t, start.Add(20*time.Second+orderingLockTTL-orderingLeaseSafetyMargin), lease.localDeadline())
}

func TestLeaseMarginCoversLockWaits(t *testing.T) {
	cases := []struct {
		name           string
		lockWait, inno int
		margin         time.Duration
		wantErr        bool
	}{
		{name: "defaults 10+10+5 fit in 30s", lockWait: 10, inno: 10, margin: orderingLeaseSafetyMargin},
		{name: "lock_wait_timeout disabled", lockWait: -1, inno: 10, margin: orderingLeaseSafetyMargin, wantErr: true},
		{name: "innodb_lock_wait_timeout disabled", lockWait: 10, inno: 0, margin: orderingLeaseSafetyMargin, wantErr: true},
		{name: "20+10+5 exceeds 30s", lockWait: 20, inno: 10, margin: orderingLeaseSafetyMargin, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := leaseMarginCoversLockWaits(tc.lockWait, tc.inno, tc.margin)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// 退避序列:首次 500ms、逐次翻倍、封顶 5s;次数再大也不溢出。
func TestOrderingLeaseTiming_Backoff(t *testing.T) {
	timing := orderingLeaseTimingDefault
	timing.jitter = func(d time.Duration) time.Duration { return d }
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second}
	for i, d := range want {
		assert.Equalf(t, d, timing.backoff(i+1), "attempt %d", i+1)
	}
	assert.Equal(t, 5*time.Second, timing.backoff(64))
	assert.Equal(t, 500*time.Millisecond, timing.backoff(0))
}
