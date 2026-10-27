// Package dispatcher hosts process-wide event-driven dispatchers. The
// TaskResultDispatcher converts DB-task result delivery from a polling/BLPOP
// model into a callback registry driven by a single Redis Pub/Sub subscriber
// goroutine. Hot paths (gRPC handlers, preload pool tasks) register a callback
// for a taskID and return immediately — no goroutine is left waiting on Redis.
package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	db_proto "proto/db"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"shared/drainproof"
	"shared/safego"
	"shared/taskresult"
)

// safego 点位名。它们会直接变成 Prometheus label,必须是常量
// —— 绝不能把 taskID 之类的运行期值拼进去(仓库 CLAUDE.md §9)。
const (
	pointSubscriber = "login.task_result_subscriber"
	pointWorker     = "login.task_result_worker"
	pointCallback   = "login.task_result_callback"
)

// ResultCallback receives the decoded TaskResult, or an error if the result
// could not be fetched (timeout, decode failure, etc.).
type ResultCallback func(res *db_proto.TaskResult, err error)

type pendingEntry struct {
	cb        ResultCallback
	cancel    func()
	expiresAt time.Time
}

// TaskResultDispatcher is a singleton-style component owned by the service
// context. Lifecycle: NewTaskResultDispatcher → Start → (Register many times)
// → Stop on shutdown.
type TaskResultDispatcher struct {
	rc         redis.UniversalClient
	defaultTTL time.Duration

	// replyInstance 是本进程的读回执回复目标(DBTask.reply_instance),instanceChannel 是它
	// 对应的实例频道。构造时定死、此后只读,无需加锁。
	replyInstance   string
	instanceChannel string

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
	dispatchJobs    chan dispatchJob
	workersWg       sync.WaitGroup
	callbacks       *drainproof.Session
	stopOnce        sync.Once
	ready           chan struct{}
	readyOnce       sync.Once
	subscriberReady bool
	draining        bool
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
// future stress shows pool saturation in `login_task_result_dispatch_overflow_total`
// (metrics.go; counted on the saturation fallback in dispatch).
const dispatchWorkerCount = 16

// dispatchJobsBuffer absorbs short bursts before the workers catch up.
// 4096 = 2x the worst observed burst size; full = overflow path kicks in.
const dispatchJobsBuffer = 4096

// NewTaskResultDispatcher constructs the dispatcher. defaultTTL bounds how long
// a registration may sit unfulfilled before its callback is fired with a
// timeout error — protects against DB drops / pub/sub loss / etc.
//
// 回复目标按「进程」每次启动取一个新 UUID:回调表只活在本进程内存里,进程换了旧回调也就
// 没了,新进程接住旧进程的通知毫无用处。不用主机名 / Pod 名:容器原地重启后它们不变,
// 会让新进程收到旧进程任务的通知。签名保持不变,装配处(svc.ServiceContext)无需改动。
func NewTaskResultDispatcher(rc redis.UniversalClient, defaultTTL time.Duration) *TaskResultDispatcher {
	return newTaskResultDispatcher(rc, defaultTTL, uuid.NewString())
}

// newTaskResultDispatcher 由调用方指定回复目标。replyInstance 不合法属于编程错误,直接 panic
// 让进程启动即失败,而不是带着一个 go/db 会回落到全服广播的目标默默运行。
func newTaskResultDispatcher(rc redis.UniversalClient, defaultTTL time.Duration, replyInstance string) *TaskResultDispatcher {
	instanceChannel, ok := taskresult.InstanceNotifyChannel(replyInstance)
	if !ok {
		panic(fmt.Sprintf("dispatcher: invalid task result reply instance %q", replyInstance))
	}
	if defaultTTL <= 0 {
		defaultTTL = 30 * time.Second
	}
	registerDeliveryMetrics()
	return &TaskResultDispatcher{
		rc:              rc,
		defaultTTL:      defaultTTL,
		replyInstance:   replyInstance,
		instanceChannel: instanceChannel,
		pending:         make(map[string]pendingEntry),
		callbacks:       drainproof.New(),
		ready:           make(chan struct{}),
		stopCh:          make(chan struct{}),
		doneCh:          make(chan struct{}),
		dispatchJobs:    make(chan dispatchJob, dispatchJobsBuffer),
	}
}

// ReplyInstance 返回本进程的读回执回复目标。发读任务时必须把它写进 DBTask.reply_instance,
// 而且必须取自登记了这批回调的同一个 dispatcher:取错了,通知会投到别的进程或无人订阅的
// 频道,等待方只能等满 TTL 靠兜底 LPOP 取回。
func (d *TaskResultDispatcher) ReplyInstance() string {
	return d.replyInstance
}

// Start launches the subscriber goroutine, the periodic GC sweep, and the
// bounded dispatch worker pool.
func (d *TaskResultDispatcher) Start() {
	// 运维据此把实例频道对到 Pod(PUBSUB CHANNELS task:result:notify:*)。
	logx.Infof("TaskResultDispatcher starting: reply_instance=%s channels=[%s %s]",
		d.replyInstance, taskresult.LegacyNotifyChannel, d.instanceChannel)
	for i := 0; i < dispatchWorkerCount; i++ {
		d.workersWg.Add(1)
		safego.Go(pointWorker, d.dispatchWorker)
	}
	safego.Go(pointSubscriber, d.run)
}

// Stop terminates background goroutines and fires every pending callback with
// a shutdown error so EnterGame chains can unwind cleanly.
func (d *TaskResultDispatcher) Stop() {
	if err := d.StopAndReport(context.Background()); err != nil {
		logx.Errorf("结果分发排空未完成: %v", err)
	}
}

// StopAndReport保留订阅和TTL兜底直至实际回调全部返回；截止只返回HOLD，不清空pending伪造收敛。
// 登记与关闭都在mu下；callback账先于pending移除建立，包含池溢出和GC回调。
func (d *TaskResultDispatcher) StopAndReport(ctx context.Context) error {
	d.mu.Lock()
	if d.callbacks == nil {
		d.callbacks = drainproof.New()
	}
	callbacks := d.callbacks
	d.mu.Unlock()
	if err := callbacks.WaitIdle(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	callbacks.CloseAdmission()
	d.mu.Unlock()
	if err := callbacks.Wait(ctx); err != nil {
		return err
	}
	d.stopOnce.Do(func() { close(d.stopCh) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.doneCh:
	}
	d.workersWg.Wait()
	return nil
}

// Register associates `cb` with `taskID`. The callback is invoked exactly once
// when a notification arrives, when the registration TTL elapses, or on Stop
// — unless the registration is withdrawn first with Cancel.
// `ttl<=0` uses the dispatcher default.
func (d *TaskResultDispatcher) Register(taskID string, ttl time.Duration, cb ResultCallback) {
	if ttl <= 0 {
		ttl = d.defaultTTL
	}
	d.mu.Lock()
	if d.callbacks == nil {
		d.callbacks = drainproof.New()
	}
	done, err := d.callbacks.Enter()
	if err != nil || d.pending == nil {
		d.mu.Unlock()
		if done != nil {
			done()
		}
		cb(nil, drainproof.ErrClosed)
		return
	}
	if _, exists := d.pending[taskID]; exists {
		d.mu.Unlock()
		done()
		cb(nil, fmt.Errorf("重复结果登记"))
		return
	}
	var once sync.Once
	wrapped := func(result *db_proto.TaskResult, err error) {
		once.Do(func() {
			defer func() {
				if fault := recover(); fault != nil {
					d.callbacks.Hold()
					done()
					panic(fault)
				}
				done()
			}()
			d.mu.Lock()
			draining := d.draining
			d.mu.Unlock()
			if draining && err != nil {
				d.callbacks.Hold()
			}
			cb(result, err)
		})
	}
	d.pending[taskID] = pendingEntry{cb: wrapped, cancel: func() { once.Do(done) }, expiresAt: time.Now().Add(ttl)}
	d.mu.Unlock()
}

// Cancel 撤销登记:删掉这些 taskID 的回调,不回调、不计数。只给「登记后任务没能发出去、调用方
// 已自行以错误收尾」的路径用。留着这种登记只会在 TTL 到期时多做一次无用的 LPOP,并把发送失败
// 计成 timed_out,混进「通知丢了且结果也没取到」的口径,让值班把 Kafka 故障误判成 Pub/Sub 或
// go/db 的送达问题。
//
// 约束:已被 dispatch / sweep 取走的回调不受影响,照常执行一次;不存在的 taskID 忽略;Stop 之后
// pending 为 nil,delete 是空操作。被撤销的任务若实际已发出,结果只在 Redis 里按 TTL 过期;
// 它的通知到达时已不在回调表,实例频道上会计 late_notify。
func (d *TaskResultDispatcher) Cancel(taskIDs ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range taskIDs {
		if e, ok := d.pending[id]; ok && e.cancel != nil {
			if d.draining && d.callbacks != nil {
				d.callbacks.Hold()
			}
			e.cancel()
		}
		delete(d.pending, id)
	}
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

// run subscribes to the notify channels (legacy + per-instance) and dispatches incoming taskIDs. A
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
	// 一条 SUBSCRIBE 同时订旧频道与本实例频道,共用这一条 Pub/Sub 连接(Redis 连接总账里
	// login 的 PubSub 仍是 1);go-redis 断线重连时按记下的频道集合整体重订。旧频道永久保留:
	// 混跑期旧版 go/db 只发它,go/db 回滚后也只发它,发布方对缺目标的任务也回落到它。
	psub := d.rc.Subscribe(ctx, taskresult.LegacyNotifyChannel, d.instanceChannel)
	defer psub.Close()

	// Confirm subscription is live before processing.
	// 只等第一条确认:Redis 把整条 SUBSCRIBE 执行完(两个频道都已登记)才把确认写回连接;
	// 第二条确认随后由 psub.Channel() 的读循环当作 *redis.Subscription 忽略。
	if _, err := psub.Receive(ctx); err != nil {
		return fmt.Errorf("subscribe receive: %w", err)
	}

	d.mu.Lock()
	d.subscriberReady = true
	d.mu.Unlock()
	d.readyOnce.Do(func() {
		if d.ready != nil {
			close(d.ready)
		}
	})
	defer func() { d.mu.Lock(); d.subscriberReady = false; d.mu.Unlock() }()

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
			d.dispatch(ctx, msg.Channel, msg.Payload)
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
//
// channel 是通知到达的频道,只用于区分计数,两条频道上本进程的任务处理方式完全相同。
func (d *TaskResultDispatcher) dispatch(ctx context.Context, channel, taskID string) {
	fromInstance := channel == d.instanceChannel
	cb := d.take(taskID)
	if cb == nil {
		// 旧频道是全服广播:混跑期(旧版 go/db、未填回复目标的生产者)别的 login 的任务也送到
		// 这里,查不到是常态,不计数。实例频道只投本进程发起的任务,查不到说明通知迟到(任务已被
		// 兜底计过 salvaged / timed_out)、重复(go/db 重投重执行后再发一次,任务早已送达)或登记
		// 已被 Cancel,都可安全忽略,但值得计数;口径见 metrics.go。
		if fromInstance {
			countDelivery(deliveryLateNotify)
		}
		return
	}
	if fromInstance {
		countDelivery(deliveryInstanceNotify)
	} else {
		countDelivery(deliveryLegacyNotify)
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
		// 计数让这条退路可观测(login_task_result_dispatch_overflow_total):压测与告警据此判断池子是否偏小。
		countDispatchOverflow()
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
		// 有通知却没有结果:go/db 总是先 LPUSH 再 PUBLISH,正常不会发生。单独计数是为了暴露
		// 结果 LIST 与通知频道不在同一个 Redis 实例这类配置错误;Redis 故障不算在内。
		if errors.Is(err, redis.Nil) {
			countDelivery(deliveryResultMissing)
		}
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
					countDelivery(deliverySalvaged)
					it.cb(res, nil)
					return
				}
			}
			countDelivery(deliveryTimedOut)
			it.cb(nil, fmt.Errorf("task result wait timed out"))
		})
	}
}

// WaitIdle在上游闭合后保留已接收回调的派生能力；它不把TTL视作服务端撤销。
func (d *TaskResultDispatcher) WaitIdle(ctx context.Context) error {
	d.mu.Lock()
	if d.callbacks == nil {
		d.callbacks = drainproof.New()
	}
	callbacks := d.callbacks
	d.mu.Unlock()
	return callbacks.WaitIdle(ctx)
}

func (d *TaskResultDispatcher) FrontierDigest(ctx context.Context) ([]byte, error) {
	if err := d.StopAndReport(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	callbacks := d.callbacks
	d.mu.Unlock()
	return callbacks.SettledDigest(ctx)
}

// NewTaskResultDispatcherForBoot让受控维护启动的真实回复UUID与已固定同boot身份一致。
func NewTaskResultDispatcherForBoot(rc redis.UniversalClient, ttl time.Duration, instance []byte) (*TaskResultDispatcher, error) {
	id, err := uuid.FromBytes(instance)
	if err != nil {
		return nil, err
	}
	return newTaskResultDispatcher(rc, ttl, id.String()), nil
}

func (d *TaskResultDispatcher) ReadyFrontier(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.ready:
	}
	d.mu.Lock()
	ready, callbacks := d.subscriberReady, d.callbacks
	d.mu.Unlock()
	if !ready || callbacks == nil {
		return nil, drainproof.ErrHold
	}
	return callbacks.SnapshotDigest(), nil
}

// BeginDrain保留回调派生能力，但TTL/取消/Redis错误在此后不能算作server undo。
func (d *TaskResultDispatcher) BeginDrain() { d.mu.Lock(); d.draining = true; d.mu.Unlock() }
