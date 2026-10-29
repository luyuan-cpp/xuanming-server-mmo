package dispatcher

// TaskResultDispatcher 分频道送达与计数的确定性单测:不 Start、不起订阅与 worker,直接同步调用
// dispatch / runDispatchJob / sweepExpired。计数经 prometheus.DefaultGatherer 读取(不 import
// client_golang 的 testutil,避免改 go.sum);计数器是进程级累计值,一律比较前后差值。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	db_proto "proto/db"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"shared/taskresult"
)

const (
	unitReplyInstance   = "0b6c2f5e-6a3d-4c1b-9f2e-8d7a6b5c4d3e"
	deliveryMetricName  = "login_task_result_delivery_total"
	callbackWaitTimeout = 5 * time.Second
)

type deliveredResult struct {
	res *db_proto.TaskResult
	err error
}

func newUnitDispatcher(t *testing.T) (*TaskResultDispatcher, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return newTaskResultDispatcher(rc, time.Minute, unitReplyInstance), mr
}

// deliverySnapshot 读出全部 outcome 的当前值。任一序列不存在即失败:序列必须在构造时就以 0 建出。
func deliverySnapshot(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	got := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != deliveryMetricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" {
					got[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	for _, outcome := range deliveryOutcomes {
		if _, ok := got[outcome]; !ok {
			t.Fatalf("%s{outcome=%q} series missing", deliveryMetricName, outcome)
		}
	}
	return got
}

// assertDeliveryDelta 断言每个 outcome 的增量恰好等于 want(未列出的为 0)。
func assertDeliveryDelta(t *testing.T, before map[string]float64, want map[string]float64) {
	t.Helper()
	after := deliverySnapshot(t)
	for _, outcome := range deliveryOutcomes {
		if delta := after[outcome] - before[outcome]; delta != want[outcome] {
			t.Errorf("%s{outcome=%q} delta = %v, want %v", deliveryMetricName, outcome, delta, want[outcome])
		}
	}
}

func isPending(d *TaskResultDispatcher, taskID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.pending[taskID]
	return ok
}

func pushResult(t *testing.T, mr *miniredis.Miniredis, taskID string, res *db_proto.TaskResult) {
	t.Helper()
	raw, err := proto.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mr.Lpush("task:result:"+taskID, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func recordInto(ch chan<- deliveredResult) ResultCallback {
	return func(res *db_proto.TaskResult, err error) { ch <- deliveredResult{res: res, err: err} }
}

// 标签取值是告警与压测汇总按字面值引用的契约,改名要先让这里变红。
func TestDeliverySeriesPreCreated(t *testing.T) {
	newUnitDispatcher(t)
	got := deliverySnapshot(t)
	want := []string{"instance_notify", "legacy_notify", "late_notify", "result_missing", "salvaged", "timed_out"}
	if len(got) != len(want) {
		t.Fatalf("%s has %d series %v, want exactly %v", deliveryMetricName, len(got), got, want)
	}
	for _, outcome := range want {
		if _, ok := got[outcome]; !ok {
			t.Fatalf("%s{outcome=%q} series missing", deliveryMetricName, outcome)
		}
	}
}

func TestNewTaskResultDispatcher_ReplyInstanceValidAndUnique(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	a := NewTaskResultDispatcher(rc, time.Minute)
	b := NewTaskResultDispatcher(rc, time.Minute)
	for _, d := range []*TaskResultDispatcher{a, b} {
		if !taskresult.ValidReplyInstance(d.ReplyInstance()) {
			t.Fatalf("ReplyInstance %q is not a valid reply instance", d.ReplyInstance())
		}
		want, _ := taskresult.InstanceNotifyChannel(d.ReplyInstance())
		if d.instanceChannel != want {
			t.Fatalf("instanceChannel = %q, want %q", d.instanceChannel, want)
		}
	}
	// 每个进程(这里是每个 dispatcher)一个回复目标:相同就会接到别人的通知。
	if a.ReplyInstance() == b.ReplyInstance() {
		t.Fatalf("two dispatchers share reply instance %q", a.ReplyInstance())
	}
}

func TestNewTaskResultDispatcher_PanicsOnInvalidInstance(t *testing.T) {
	for _, bad := range []string{"", "bad:id", "a*", strings.Repeat("a", taskresult.MaxReplyInstanceLen+1)} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("newTaskResultDispatcher(%q) did not panic", bad)
				}
			}()
			newTaskResultDispatcher(nil, time.Minute, bad)
		})
	}
}

func TestDispatch_OwnTaskDeliveredOnEitherChannel(t *testing.T) {
	for _, c := range []struct {
		name    string
		channel func(*TaskResultDispatcher) string
		outcome string
	}{
		{"实例频道", func(d *TaskResultDispatcher) string { return d.instanceChannel }, deliveryInstanceNotify},
		{"旧频道", func(*TaskResultDispatcher) string { return taskresult.LegacyNotifyChannel }, deliveryLegacyNotify},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			d, mr := newUnitDispatcher(t)
			got := make(chan deliveredResult, 1)
			d.Register("t-1", time.Hour, recordInto(got))

			before := deliverySnapshot(t)
			d.dispatch(ctx, c.channel(d), "t-1")
			assertDeliveryDelta(t, before, map[string]float64{c.outcome: 1})
			if isPending(d, "t-1") {
				t.Fatal("task still pending after dispatch")
			}
			if n := len(d.dispatchJobs); n != 1 {
				t.Fatalf("dispatchJobs = %d, want 1", n)
			}

			pushResult(t, mr, "t-1", &db_proto.TaskResult{Success: true})
			d.runDispatchJob(ctx, <-d.dispatchJobs)
			select {
			case r := <-got:
				if r.err != nil || !r.res.GetSuccess() {
					t.Fatalf("callback got res=%v err=%v, want success", r.res, r.err)
				}
			default:
				t.Fatal("runDispatchJob did not invoke the callback synchronously")
			}
		})
	}
}

// 混跑期旧频道上是别的 login 的任务:不取、不排队、不计数,也不动本进程的登记。
func TestDispatch_LegacyForeignTaskIgnored(t *testing.T) {
	d, _ := newUnitDispatcher(t)
	d.Register("mine", time.Hour, func(*db_proto.TaskResult, error) {})

	before := deliverySnapshot(t)
	d.dispatch(context.Background(), taskresult.LegacyNotifyChannel, "foreign")
	assertDeliveryDelta(t, before, nil)
	if n := len(d.dispatchJobs); n != 0 {
		t.Fatalf("dispatchJobs = %d, want 0", n)
	}
	if !isPending(d, "mine") {
		t.Fatal("own registration lost after a foreign legacy notify")
	}
}

func TestDispatch_InstanceLateNotifyCounted(t *testing.T) {
	d, _ := newUnitDispatcher(t)

	before := deliverySnapshot(t)
	d.dispatch(context.Background(), d.instanceChannel, "already-gone")
	assertDeliveryDelta(t, before, map[string]float64{deliveryLateNotify: 1})
	if n := len(d.dispatchJobs); n != 0 {
		t.Fatalf("dispatchJobs = %d, want 0", n)
	}
}

// dispatchOverflowValue 读出 login_task_result_dispatch_overflow_total 的当前值。序列不存在即失败:
// 它在构造 dispatcher 时就注册(无 label 的 Counter 注册即有 0 序列),increase() 才不会漏掉第一次。
func dispatchOverflowValue(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == "login_task_result_dispatch_overflow_total" {
			if ms := mf.GetMetric(); len(ms) == 1 {
				return ms[0].GetCounter().GetValue()
			}
			t.Fatalf("login_task_result_dispatch_overflow_total has %d series, want exactly 1 (no labels)", len(mf.GetMetric()))
		}
	}
	t.Fatal("login_task_result_dispatch_overflow_total series missing")
	return 0
}

// 派发池队列满了(突发尾部):通知改由临时 goroutine 处理,回调照样拿到结果;计一次溢出,
// 送达计数与不溢出时相同(instance_notify +1),溢出不是送达结果、不进送达率分母。
// 不 Start(没有 worker 消费),先用占位任务把队列填满,让这次 dispatch 必然走 default 分支。
func TestDispatch_PoolSaturatedFallsBackAndCountsOverflow(t *testing.T) {
	ctx := context.Background()
	d, mr := newUnitDispatcher(t)
	for i := 0; i < cap(d.dispatchJobs); i++ {
		d.dispatchJobs <- dispatchJob{taskID: "filler"}
	}
	got := make(chan deliveredResult, 1)
	d.Register("t-overflow", time.Hour, recordInto(got))
	pushResult(t, mr, "t-overflow", &db_proto.TaskResult{Success: true})

	overflowBefore := dispatchOverflowValue(t)
	before := deliverySnapshot(t)
	d.dispatch(ctx, d.instanceChannel, "t-overflow")

	r := waitDelivered(t, got)
	if r.err != nil || !r.res.GetSuccess() {
		t.Fatalf("callback got res=%v err=%v, want success via the overflow goroutine", r.res, r.err)
	}
	if delta := dispatchOverflowValue(t) - overflowBefore; delta != 1 {
		t.Fatalf("login_task_result_dispatch_overflow_total delta = %v, want 1", delta)
	}
	assertDeliveryDelta(t, before, map[string]float64{deliveryInstanceNotify: 1})
	if n := len(d.dispatchJobs); n != cap(d.dispatchJobs) {
		t.Fatalf("dispatchJobs = %d, want the queue untouched at %d (the overflow job must not be queued)", n, cap(d.dispatchJobs))
	}
	if isPending(d, "t-overflow") {
		t.Fatal("task still pending after an overflow dispatch")
	}
}

// 队列没满时不计溢出:走 worker 池的常规路径。
func TestDispatch_QueuedJobDoesNotCountOverflow(t *testing.T) {
	d, _ := newUnitDispatcher(t)
	d.Register("t-queued", time.Hour, func(*db_proto.TaskResult, error) {})

	overflowBefore := dispatchOverflowValue(t)
	d.dispatch(context.Background(), d.instanceChannel, "t-queued")
	if delta := dispatchOverflowValue(t) - overflowBefore; delta != 0 {
		t.Fatalf("login_task_result_dispatch_overflow_total delta = %v, want 0 when the job fits in the queue", delta)
	}
	if n := len(d.dispatchJobs); n != 1 {
		t.Fatalf("dispatchJobs = %d, want 1", n)
	}
}

func TestRunDispatchJob_ResultMissingCounted(t *testing.T) {
	t.Run("结果 LIST 为空", func(t *testing.T) {
		d, _ := newUnitDispatcher(t)
		got := make(chan deliveredResult, 1)

		before := deliverySnapshot(t)
		d.runDispatchJob(context.Background(), dispatchJob{taskID: "t-missing", cb: recordInto(got)})
		assertDeliveryDelta(t, before, map[string]float64{deliveryResultMissing: 1})
		r := <-got
		if r.res != nil || !errors.Is(r.err, redis.Nil) {
			t.Fatalf("callback got res=%v err=%v, want redis.Nil", r.res, r.err)
		}
	})
	// Redis 故障不是「结果缺失」,不得混进这个计数。
	t.Run("Redis 故障", func(t *testing.T) {
		d, mr := newUnitDispatcher(t)
		mr.SetError("injected redis failure")
		got := make(chan deliveredResult, 1)

		before := deliverySnapshot(t)
		d.runDispatchJob(context.Background(), dispatchJob{taskID: "t-err", cb: recordInto(got)})
		assertDeliveryDelta(t, before, nil)
		r := <-got
		if r.res != nil || r.err == nil || errors.Is(r.err, redis.Nil) {
			t.Fatalf("callback got res=%v err=%v, want a non-Nil redis error", r.res, r.err)
		}
	})
}

// 兜底用例直接写入一个早已过期的登记(expiresAt 取零值时间),再同步调用 sweepExpired,
// 不依赖墙钟流逝。回调在 safego.Go 里执行,用带上限的 channel 等待。
func expireNow(d *TaskResultDispatcher, taskID string, cb ResultCallback) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[taskID] = pendingEntry{cb: cb, expiresAt: time.Time{}}
}

func waitDelivered(t *testing.T, got <-chan deliveredResult) deliveredResult {
	t.Helper()
	select {
	case r := <-got:
		return r
	case <-time.After(callbackWaitTimeout):
		t.Fatalf("sweep callback not invoked within %v", callbackWaitTimeout)
		return deliveredResult{}
	}
}

func TestSweepExpired_SalvagesResult(t *testing.T) {
	d, mr := newUnitDispatcher(t)
	got := make(chan deliveredResult, 1)
	expireNow(d, "t-salvage", recordInto(got))
	d.Register("t-live", 24*time.Hour, func(*db_proto.TaskResult, error) {})
	pushResult(t, mr, "t-salvage", &db_proto.TaskResult{Success: true})

	before := deliverySnapshot(t)
	d.sweepExpired(context.Background())
	r := waitDelivered(t, got)
	if r.err != nil || !r.res.GetSuccess() {
		t.Fatalf("salvage callback got res=%v err=%v, want success", r.res, r.err)
	}
	assertDeliveryDelta(t, before, map[string]float64{deliverySalvaged: 1})
	if isPending(d, "t-salvage") || !isPending(d, "t-live") {
		t.Fatal("sweep must remove only the expired registration")
	}
}

// 发送失败的调用方撤销登记后,兜底扫描既不回调也不计 timed_out;未撤销的登记照常兜底。
// 对照组 t-control 与被撤销的两条在同一次 sweep 里,用来确认这次 sweep 确实生效。
func TestCancel_WithdrawnRegistrationsSkipSweep(t *testing.T) {
	d, _ := newUnitDispatcher(t)
	cancelledGot := make(chan deliveredResult, 2)
	controlGot := make(chan deliveredResult, 1)
	expireNow(d, "t-cancel-a", recordInto(cancelledGot))
	expireNow(d, "t-cancel-b", recordInto(cancelledGot))
	expireNow(d, "t-control", recordInto(controlGot))

	d.Cancel("t-cancel-a", "t-cancel-b", "never-registered")
	if isPending(d, "t-cancel-a") || isPending(d, "t-cancel-b") {
		t.Fatal("cancelled registration still pending")
	}
	if !isPending(d, "t-control") {
		t.Fatal("Cancel removed a registration it was not asked to remove")
	}

	before := deliverySnapshot(t)
	d.sweepExpired(context.Background())
	if r := waitDelivered(t, controlGot); r.err == nil {
		t.Fatalf("control callback got res=%v err=nil, want a timed-out error", r.res)
	}
	assertDeliveryDelta(t, before, map[string]float64{deliveryTimedOut: 1})
	select {
	case r := <-cancelledGot:
		t.Fatalf("cancelled registration invoked with res=%v err=%v", r.res, r.err)
	default:
	}
}

func TestSweepExpired_TimesOutWithoutResult(t *testing.T) {
	d, _ := newUnitDispatcher(t)
	got := make(chan deliveredResult, 1)
	expireNow(d, "t-timeout", recordInto(got))

	before := deliverySnapshot(t)
	d.sweepExpired(context.Background())
	r := waitDelivered(t, got)
	if r.res != nil || r.err == nil || !strings.Contains(r.err.Error(), "timed out") {
		t.Fatalf("timeout callback got res=%v err=%v, want a timed-out error", r.res, r.err)
	}
	assertDeliveryDelta(t, before, map[string]float64{deliveryTimedOut: 1})
	if isPending(d, "t-timeout") {
		t.Fatal("expired registration still pending after sweep")
	}
}

func TestStopAndReportDoesNotDiscardPendingCallbacks(t *testing.T) {
	d, _ := newUnitDispatcher(t)
	delivered := false
	d.Register("drain-pending", time.Hour, func(*db_proto.TaskResult, error) { delivered = true })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.StopAndReport(ctx); err == nil {
		t.Fatal("pending未完成却成功停止")
	}
	if delivered || !isPending(d, "drain-pending") {
		t.Fatal("超时清掉了pending或伪造回调")
	}
	callback := d.take("drain-pending")
	callback(nil, nil)
	if err := d.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDrainTimeoutCallbackRemainsUnknown(t *testing.T) {
	d, _ := newUnitDispatcher(t)
	d.Register("unknown-server", time.Hour, func(*db_proto.TaskResult, error) {})
	d.BeginDrain()
	cb := d.take("unknown-server")
	cb(nil, context.DeadlineExceeded)
	d.callbacks.CloseAdmission()
	if err := d.callbacks.Wait(context.Background()); err == nil {
		t.Fatal("TTL/取消被当成server undo")
	}
}
