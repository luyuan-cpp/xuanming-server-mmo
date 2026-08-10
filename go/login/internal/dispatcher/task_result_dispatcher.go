// Package dispatcher hosts process-wide event-driven dispatchers. The
// TaskResultDispatcher converts DB-task result delivery from a polling/BLPOP
// model into a callback registry driven by a single Redis Pub/Sub subscriber
// goroutine. Hot paths (gRPC handlers, preload pool tasks) register a callback
// for a taskID and return immediately — no goroutine is left waiting on Redis.
package dispatcher

import (
	"context"
	"fmt"
	"sync"
	"time"

	db_proto "proto/db"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"shared/safego"
)

// safego 点位名。它们会直接变成 Prometheus label,必须是常量
// —— 绝不能把 taskID 之类的运行期值拼进去(仓库 CLAUDE.md §9)。
const (
	pointSubscriber = "login.task_result_subscriber"
	pointWorker     = "login.task_result_worker"
	pointCallback   = "login.task_result_callback"
)

// TaskResultNotifyChannel must match the channel published by the DB consumer.
const TaskResultNotifyChannel = "task:result:notify"

// ResultCallback receives the decoded TaskResult, or an error if the result
// could not be fetched (timeout, decode failure, etc.).
type ResultCallback func(res *db_proto.TaskResult, err error)

type pendingEntry struct {
	cb        ResultCallback
	expiresAt time.Time
}

// TaskResultDispatcher is a singleton-style component owned by the service
// context. Lifecycle: NewTaskResultDispatcher → Start → (Register many times)
// → Stop on shutdown.
type TaskResultDispatcher struct {
	rc         redis.UniversalClient
	defaultTTL time.Duration

	mu      sync.Mutex
	pending map[string]pendingEntry

	stopCh chan struct{}
	doneCh chan struct{}

	// 2026-06-01 Round 14 §P7: replace the per-message `go func()` in
	// dispatch() with a bounded worker pool. Round 14 showed
	// cb_wait{ok} avg climbing 85ms → 275ms as EnterGame throughput
	// rose post-P0; spawning an unbounded number of goroutines per
	// pub/sub message added scheduler pressure + GC churn that surfaced
	// as callback-arrival jitter. A fixed pool (workers drain a buffered
	// channel) bounds goroutine count and gives the runtime warm Gs to
	// reuse. On saturation we still fall back to an ad-hoc goroutine so
	// the subscriber loop never blocks — graceful degradation, not
	// back-pressure into pub/sub.
	dispatchJobs chan dispatchJob
	workersWg    sync.WaitGroup
}

// dispatchJob bundles a taken callback with its taskID for the worker pool.
// The callback has already been removed from d.pending in dispatch() (via
// take()), so the only remaining work is the LPop+Unmarshal+invoke trio.
type dispatchJob struct {
	taskID string
	cb     ResultCallback
}

// dispatchWorkerCount sizes the per-process pool. 16 comfortably absorbs the
// observed Round 14 peak (~4k notifies/s burst → each worker handles ~250/s
// of LPop+Unmarshal, well within a single Redis round-trip budget). Bump if
// future stress shows pool saturation in `pubsub_dispatch_overflow_total`.
const dispatchWorkerCount = 16

// dispatchJobsBuffer absorbs short bursts before the workers catch up.
// 4096 = 2x the worst observed burst size; full = overflow path kicks in.
const dispatchJobsBuffer = 4096

// NewTaskResultDispatcher constructs the dispatcher. defaultTTL bounds how long
// a registration may sit unfulfilled before its callback is fired with a
// timeout error — protects against DB drops / pub/sub loss / etc.
func NewTaskResultDispatcher(rc redis.UniversalClient, defaultTTL time.Duration) *TaskResultDispatcher {
	if defaultTTL <= 0 {
		defaultTTL = 30 * time.Second
	}
	return &TaskResultDispatcher{
		rc:           rc,
		defaultTTL:   defaultTTL,
		pending:      make(map[string]pendingEntry),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		dispatchJobs: make(chan dispatchJob, dispatchJobsBuffer),
	}
}

// Start launches the subscriber goroutine, the periodic GC sweep, and the
// bounded dispatch worker pool.
func (d *TaskResultDispatcher) Start() {
	for i := 0; i < dispatchWorkerCount; i++ {
		d.workersWg.Add(1)
		safego.Go(pointWorker, d.dispatchWorker)
	}
	safego.Go(pointSubscriber, d.run)
}

// Stop terminates background goroutines and fires every pending callback with
// a shutdown error so EnterGame chains can unwind cleanly.
func (d *TaskResultDispatcher) Stop() {
	close(d.stopCh)
	<-d.doneCh
	d.workersWg.Wait()
	// Drain any dispatch jobs the workers had not yet picked up — their
	// callbacks were already taken out of d.pending so the cleanup loop
	// below would miss them otherwise.
	for {
		select {
		case job := <-d.dispatchJobs:
			cb := job.cb
			safego.Go(pointCallback, func() { cb(nil, fmt.Errorf("dispatcher stopped")) })
		default:
			goto drained
		}
	}
drained:
	d.mu.Lock()
	for _, e := range d.pending {
		cb := e.cb
		safego.Go(pointCallback, func() { cb(nil, fmt.Errorf("dispatcher stopped")) })
	}
	d.pending = nil
	d.mu.Unlock()
}

// Register associates `cb` with `taskID`. The callback is invoked exactly once
// when a notification arrives, when the registration TTL elapses, or on Stop.
// `ttl<=0` uses the dispatcher default.
func (d *TaskResultDispatcher) Register(taskID string, ttl time.Duration, cb ResultCallback) {
	if ttl <= 0 {
		ttl = d.defaultTTL
	}
	d.mu.Lock()
	if d.pending == nil {
		d.mu.Unlock()
		safego.Go(pointCallback, func() { cb(nil, fmt.Errorf("dispatcher stopped")) })
		return
	}
	d.pending[taskID] = pendingEntry{cb: cb, expiresAt: time.Now().Add(ttl)}
	d.mu.Unlock()
}

// take atomically removes and returns the callback for taskID (or nil if none).
func (d *TaskResultDispatcher) take(taskID string) ResultCallback {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending == nil {
		return nil
	}
	e, ok := d.pending[taskID]
	if !ok {
		return nil
	}
	delete(d.pending, taskID)
	return e.cb
}

// run subscribes to the notify channel and dispatches incoming taskIDs. A
// secondary ticker sweeps expired registrations.
func (d *TaskResultDispatcher) run() {
	defer close(d.doneCh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	safego.Go(pointSubscriber+".cancel_watch", func() {
		<-d.stopCh
		cancel()
	})

	// GC tick interval. Originally `defaultTTL / 2` (so 15s when defaultTTL
	// is the default 30s). That made per-entry TTL effectively rounded UP to
	// the next 15s tick — registering with ttl=5s still meant timeout
	// callbacks fired at most every 15s, hiding the real 5s SLO.
	//
	// 2026-05-28 stress §(1)+(3): we lowered dispatcherTaskTTL from 30s to 5s
	// in entergamelogic.go to bound how long player_locker stays held when
	// the async chain stalls. But entergame_preload_seconds{preload_failed}
	// observed 12s avg afterwards (not the expected 5s), and prom data
	// showed dataloader_preload_callback_wait_seconds buckets all between
	// 5s and 20s — exactly the "next 15s tick" rounding behaviour.
	//
	// Switch to a fixed 1s tick: cheap (one map scan per second, bounded
	// by pending size which is in the hundreds at most), and makes the
	// per-entry TTL the actual SLO with at most ~1s of slop.
	const gcInterval = 1 * time.Second
	gc := time.NewTicker(gcInterval)
	defer gc.Stop()

	for {
		// Restart the subscription on any error (Redis disconnect, etc.) with
		// a small backoff to avoid hot-loops.
		if err := d.subscribeLoop(ctx, gc); err != nil {
			if ctx.Err() != nil {
				return
			}
			logx.Errorf("TaskResultDispatcher subscribeLoop error: %v", err)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
		} else {
			return
		}
	}
}

func (d *TaskResultDispatcher) subscribeLoop(ctx context.Context, gc *time.Ticker) error {
	psub := d.rc.Subscribe(ctx, TaskResultNotifyChannel)
	defer psub.Close()

	// Confirm subscription is live before processing.
	if _, err := psub.Receive(ctx); err != nil {
		return fmt.Errorf("subscribe receive: %w", err)
	}

	// 2026-06-01 Round 13 postmortem: under 45k-z1 burst we saw ~4% of
	// preload registrations time out at the 5s SLO while the corresponding
	// task:result:{id} data was already present in Redis — classic "notify
	// dropped, data present" signature. go-redis defaults psub.Channel() to
	// a 100-msg buffer with a 60s send timeout; when the subscriber loop
	// blinks (GC tick, scheduler hiccup, take()+goroutine spawn) more than
	// 100 notifies queue up and the oldest blocks for up to a minute,
	// exhausting the 5s registration TTL on the affected taskIDs.
	//
	// Sized the buffer to 8192 (enough to absorb the worst observed burst:
	// ~2000 enter/s × 4 sub-table requests each = 8k notifies/s peak) and
	// dropped the send timeout to 5s so a genuinely stuck subscriber surfaces
	// as a channel drop / log warning, not silent 60s back-pressure.
	ch := psub.Channel(
		redis.WithChannelSize(8192),
		redis.WithChannelSendTimeout(5*time.Second),
	)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-gc.C:
			d.sweepExpired(ctx)
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("pubsub channel closed")
			}
			d.dispatch(ctx, msg.Payload)
		}
	}
}

// dispatch handles one notification for taskID.
//
// 2026-05-29 stress §Round 7 postmortem: previously this method ran the LPop
// + Unmarshal SYNCHRONOUSLY on the subscriber goroutine. Each Redis LPop is
// 5-10ms; under 200 task/s steady-state we saw the go-redis Pub/Sub msgCh
// (default buffer 100, send timeout 60s) back up — messages that did make it
// in arrived 1-10s late and missed the 5s registration TTL, surfacing as
// 95% callback_wait{failed} despite the data already being in Redis.
//
// Fix: take() under the mutex is microseconds (map delete). LPop +
// Unmarshal + user callback are all moved to a per-message goroutine so the
// subscriber loop dequeues msgCh at full speed. Callback fan-out remains
// goroutine-per-message — same as before, just hoisted earlier.
func (d *TaskResultDispatcher) dispatch(ctx context.Context, taskID string) {
	cb := d.take(taskID)
	if cb == nil {
		// Either belongs to a different process (pub/sub broadcasts to all
		// subscribers) or already expired — both safe to ignore.
		return
	}

	job := dispatchJob{taskID: taskID, cb: cb}
	select {
	case d.dispatchJobs <- job:
		// Picked up by the worker pool — fast, no allocation.
	default:
		// Pool saturated: spawn an ad-hoc goroutine so the subscriber loop
		// never blocks. We deliberately do NOT block-send into dispatchJobs:
		// any back-pressure here would compound into the pub/sub channel
		// and replay the Round 13 "notify dropped, data present" failure
		// mode. Better to occasionally pay a goroutine-spawn at the tail
		// of a burst than stall the subscriber.
		safego.Go(pointWorker, func() { d.runDispatchJob(ctx, job) })
	}
}

// dispatchWorker drains the bounded job channel until shutdown.
//
// 每个 job 各自 recover(safego.Run):一个业务回调 panic 只丢掉那一次投递,
// 不会让这个 worker 退出把池子缩小 —— 池子悄悄从 16 缩到 0 的话,
// 所有 EnterGame 都会卡到 TTL 超时才醒,故障现象和根因隔得极远。
func (d *TaskResultDispatcher) dispatchWorker() {
	defer d.workersWg.Done()
	ctx := context.Background()
	for {
		select {
		case <-d.stopCh:
			return
		case job := <-d.dispatchJobs:
			safego.Run(pointWorker, func() { d.runDispatchJob(ctx, job) })
		}
	}
}

// runDispatchJob does the actual LPop + Unmarshal + invoke. Extracted so
// both pool workers and the saturation fallback share one code path.
func (d *TaskResultDispatcher) runDispatchJob(ctx context.Context, job dispatchJob) {
	resultKey := fmt.Sprintf("task:result:%s", job.taskID)
	bytesVal, err := d.rc.LPop(ctx, resultKey).Bytes()
	if err != nil {
		job.cb(nil, fmt.Errorf("lpop %s: %w", resultKey, err))
		return
	}
	res := &db_proto.TaskResult{}
	if err := proto.Unmarshal(bytesVal, res); err != nil {
		job.cb(nil, fmt.Errorf("unmarshal task result %s: %w", job.taskID, err))
		return
	}
	job.cb(res, nil)
}

// sweepExpired fires timeout callbacks for entries whose TTL has elapsed. This
// catches lost notifications (DB crash between LPush and Publish, network
// blip, subscriber miss during reconnect, etc.).
//
// 2026-06-01 Round 13 salvage: before firing the timeout callback, attempt
// one last-chance synchronous LPop on task:result:{id}. The DB consumer
// always LPushes BEFORE Publish, so if a notify was dropped (channel
// overflow, subscriber transiently slow) the data is still sitting in Redis
// and we can recover instead of failing the player's enter-game flow.
func (d *TaskResultDispatcher) sweepExpired(ctx context.Context) {
	now := time.Now()
	type expiredItem struct {
		taskID string
		cb     ResultCallback
	}
	var expired []expiredItem

	d.mu.Lock()
	for id, e := range d.pending {
		if now.After(e.expiresAt) {
			expired = append(expired, expiredItem{taskID: id, cb: e.cb})
			delete(d.pending, id)
		}
	}
	d.mu.Unlock()

	for _, item := range expired {
		it := item
		safego.Go(pointCallback, func() {
			// Last-chance LPop: notify may have been dropped but data is
			// already in Redis. Bounded by Redis client timeout (typically
			// ~3s) so worst-case the player waits ttl + ~3s.
			resultKey := fmt.Sprintf("task:result:%s", it.taskID)
			bytesVal, err := d.rc.LPop(ctx, resultKey).Bytes()
			if err == nil {
				res := &db_proto.TaskResult{}
				if uerr := proto.Unmarshal(bytesVal, res); uerr == nil {
					logx.Infof("TaskResultDispatcher salvaged taskID=%s after notify drop", it.taskID)
					it.cb(res, nil)
					return
				}
			}
			it.cb(nil, fmt.Errorf("task result wait timed out"))
		})
	}
}
