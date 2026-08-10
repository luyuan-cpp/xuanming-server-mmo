// dispatcher.go — single-leader goroutine that drains zone queues into
// admit:{queueId} entries.
//
// Why leader-elect (instead of every login replica draining):
//
//	N replicas all running ZPOPMIN every second would race on the same
//	queue and produce unfair ordering (member popped by replica A but
//	signed-and-stored by replica B who lost the race) — plus 3-5x the
//	Redis QPS for no benefit. A single leader is simpler, fairer, and
//	cheap because the work itself is trivial (a few ZPOPMINs + SETs per
//	tick).
//
// Leader election uses pkg/locker.RedisLocker:
//   - TryLock with TTL=DispatcherLockTTL (e.g. 30s)
//   - StartHeartbeat at TTL/3 keeps the lease alive while we're draining
//   - On lost-lock: dispatcher cancels its loop ctx, the next replica's
//     periodic TryLock takes over within ~LockTTL.
//
// Dispatcher lifecycle:
//
//	Start() (called from svc.ServiceContext.Start)
//	  └─ goroutine: leader election loop
//	      ├─ try acquire lock
//	      ├─ on success: drain loop until lost
//	      └─ on failure or lost: sleep ~LockTTL/3, retry
//	Stop()
//	  └─ cancel ctx, wait for goroutine to exit, release lock if held.
//
// The drain loop walks every "active" zone — defined as any zone with a
// non-empty queue (ZCARD > 0) or any admitted entries. Zones without
// either are skipped without touching their gate watcher; this keeps the
// hot path cheap when the system is idle.
package loginqueue

import (
	"context"
	"sync"
	"time"

	"login/internal/logic/pkg/locker"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

// Dispatcher coordinates queue draining across login replicas.
type Dispatcher struct {
	queue        *Queue
	provider     CapacityProvider
	rdb          *redis.Client
	hmacSecret   []byte
	gateTokenTTL time.Duration

	interval          time.Duration
	softCapMultiplier float64
	lockTTL           time.Duration
	lockKey           string

	// podID is the metric label for login_queue_dispatcher_is_leader.
	// Empty falls back to "unknown" so the gauge still publishes — used by
	// tests that don't bother setting an identity.
	podID string

	cancel  context.CancelFunc
	stopped chan struct{}

	// activeZonesProvider returns the set of zone IDs that should be
	// considered for draining each tick. Tests inject a static list; the
	// production wiring derives this from the GateWatcher (zones we know
	// about) plus any zone with a non-empty queue:zone:* key.
	activeZonesProvider func(ctx context.Context) []uint32
	mu                  sync.Mutex
}

// NewDispatcher builds a Dispatcher. Callers must invoke Start exactly once.
func NewDispatcher(
	queue *Queue,
	provider CapacityProvider,
	rdb *redis.Client,
	hmacSecret []byte,
	gateTokenTTL time.Duration,
	interval time.Duration,
	softCapMultiplier float64,
	lockTTL time.Duration,
	lockKey string,
	activeZones func(ctx context.Context) []uint32,
) *Dispatcher {
	return &Dispatcher{
		queue:               queue,
		provider:            provider,
		rdb:                 rdb,
		hmacSecret:          hmacSecret,
		gateTokenTTL:        gateTokenTTL,
		interval:            interval,
		softCapMultiplier:   softCapMultiplier,
		lockTTL:             lockTTL,
		lockKey:             lockKey,
		activeZonesProvider: activeZones,
		stopped:             make(chan struct{}),
	}
}

// Start launches the leader-election + drain goroutine. Returns immediately.
func (d *Dispatcher) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()

	// Initialize the leader gauge to 0 so the metric appears on /metrics
	// before the first election round completes. Without this, a fresh
	// pod looks "missing" in Grafana for ~lockTTL/3 seconds.
	dispatcherIsLeaderGauge.Set(0, d.podLabel())

	// safego:选主循环 panic 会让这个副本永远不再参选,而队列没有 leader 时
	// 所有排队玩家都晾在那里。裸 go func 的旧写法是"炸了就整进程死",
	// 现在换成"记点位 + 计数",故障可见且不牵连其它链路。
	safego.Go("login.queue_dispatcher", func() { d.electAndRun(ctx) })
}

// SetPodID supplies the metric label for dispatcher_is_leader. Optional;
// when unset the gauge labels as "unknown" but still publishes (so the
// presence/absence of a leader is still observable).
func (d *Dispatcher) SetPodID(id string) {
	d.mu.Lock()
	d.podID = id
	d.mu.Unlock()
}

func (d *Dispatcher) podLabel() string {
	d.mu.Lock()
	id := d.podID
	d.mu.Unlock()
	if id == "" {
		return "unknown"
	}
	return id
}

// Stop signals the dispatcher to exit and waits for the goroutine to finish.
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-d.stopped
}

// electAndRun is the outer loop: keep trying to become leader. When elected,
// run drainLoop until we lose the lease or shutdown is requested.
func (d *Dispatcher) electAndRun(ctx context.Context) {
	defer close(d.stopped)

	rl := locker.NewRedisLocker(d.rdb)
	backoff := max(d.lockTTL/3, time.Second)

	for {
		if ctx.Err() != nil {
			return
		}

		lock, err := rl.TryLock(ctx, d.lockKey, d.lockTTL)
		if err != nil || !lock.IsLocked() {
			// Another login replica is leader (or Redis blip). Wait and retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}

		logx.Infof("[loginqueue] elected dispatcher leader key=%s", d.lockKey)
		dispatcherIsLeaderGauge.Set(1, d.podLabel())

		// Cancel drain when we lose the lease (e.g. Redis flap → key
		// expired and another replica took over). drainLoop respects this
		// ctx on every tick.
		drainCtx, drainCancel := context.WithCancel(ctx)
		stopHB := lock.StartHeartbeat(d.lockTTL/3, d.lockTTL, func(err error) {
			logx.Errorf("[loginqueue] dispatcher lost leadership: %v", err)
			dispatcherIsLeaderGauge.Set(0, d.podLabel())
			drainCancel()
		})

		d.drainLoop(drainCtx)

		stopHB()
		dispatcherIsLeaderGauge.Set(0, d.podLabel())
		// Best-effort release; on ctx cancel or lost lease this may no-op,
		// which is fine — the next replica's TryLock will succeed once the
		// TTL expires.
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = lock.Release(releaseCtx)
		releaseCancel()
		drainCancel()
	}
}

// drainLoop runs one drain pass per d.interval until ctx is cancelled.
func (d *Dispatcher) drainLoop(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	// Run one immediate tick so a fresh leader doesn't sit idle for
	// `interval` while a queue is already backed up.
	d.dispatchOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.dispatchOnce(ctx)
		}
	}
}

// dispatchOnce walks every active zone and admits as many entries as
// freeSlots allows. Errors per zone are logged and skipped — one bad zone
// (e.g. its gates went offline) shouldn't starve the others.
func (d *Dispatcher) dispatchOnce(ctx context.Context) {
	zones := d.activeZonesProvider(ctx)
	for _, zoneID := range zones {
		if ctx.Err() != nil {
			return
		}
		// Publish gauge snapshot first so even zero-admit zones keep their
		// gauges fresh — otherwise a zone that just finished draining
		// would show stale "size=N" in Grafana until the next entry shows up.
		publishGaugeSnapshot(ctx, d.queue, zoneID)

		free, _, _, _, err := FreeSlots(ctx, d.provider, d.queue, zoneID, d.softCapMultiplier)
		if err != nil {
			logx.Errorf("[loginqueue] freeSlots zone=%d err=%v", zoneID, err)
			continue
		}
		if free == 0 {
			continue
		}
		queueLen, err := d.queue.QueueLen(ctx, zoneID)
		if err != nil || queueLen == 0 {
			continue
		}

		batch := min(free, uint32(queueLen))

		// Pick a gate per entry but DO NOT sign the token here — the HMAC
		// + expire happen at consume time inside consumeAdmitAndSign so
		// the 5-min TTL window starts when the client redeems the admit,
		// not when we picked the gate (R17 R1 fix). The dispatcher's job
		// reduces to "decide which gate is least loaded right now".
		pickFn := func(zoneID uint32, _ string) (*AdmitSlot, error) {
			candidates, err := d.provider.CandidatesForZone(ctx, zoneID)
			if err != nil {
				return nil, err
			}
			gate, err := PickGate(candidates)
			if err != nil {
				return nil, err
			}
			return &AdmitSlot{
				NodeID: gate.NodeID,
				IP:     gate.IP,
				Port:   gate.Port,
				ZoneID: gate.ZoneID,
			}, nil
		}

		admitted, err := d.queue.PopAdmit(ctx, zoneID, int(batch), pickFn)
		if err != nil {
			logx.Errorf("[loginqueue] PopAdmit zone=%d err=%v", zoneID, err)
			continue
		}
		recordAdmit(zoneID, admitted)
		if admitted > 0 {
			logx.Infof("[loginqueue] zone=%d admitted=%d freeBefore=%d queueLen=%d",
				zoneID, admitted, free, queueLen)
		}
	}
}
