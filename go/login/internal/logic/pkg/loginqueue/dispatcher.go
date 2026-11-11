// dispatcher.go — 每个 zone 一个 leader 的出队 goroutine:把本 zone 队列里的条目
// 放行成 admit:{queueId}。
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
// (2026-10-18 补注)出队已不是 ZPOPMIN,而是「ZRANGE + 每条一段先判在队的放行脚本」(Queue.PopAdmit),多副本同时
// 出队也不会重复放行,上文「member popped by replica A but signed-and-stored by replica B」的问题已不存在。仍要选主,
// 是为了省掉重复的 Redis 往返,并让批内的 gate 分配在一份候选上进行。
//
// 选主范围是「一个 zone」,不是全服(G1b,2026-10-12):
//
//	login 按 zone 部署,GateWatcher 的 etcd 前缀带本进程的 Node.ZoneId,
//	CapacityProvider 只看得见本 zone 的 gate。出队要先算 FreeSlots、再挑 gate,
//	所以一个 dispatcher 只能出它看得见 gate 的那个 zone。旧实现的锁键全服一把、
//	出队范围却只有本 zone:多个 zone 共用一个 Redis 时,只有抢到锁的那个 zone
//	出队,其它 zone 一个人都放不进来。现在锁键 = ZoneDispatcherLockKey(配置前缀, zoneID),
//	锁的范围与出队范围由同一个 zoneID 决定,不会再分家。
//
// Leader election uses pkg/locker.RedisLocker:
//   - TryLock with TTL=DispatcherLockTTL (e.g. 30s)
//   - StartHeartbeat at TTL/3 keeps the lease alive while we're draining
//   - On lost-lock: dispatcher cancels its loop ctx, the next replica's
//     periodic TryLock takes over within ~LockTTL + LockTTL/3.
//
// Dispatcher lifecycle:
//
//	Start() (called from svc.ServiceContext.Start)
//	  └─ goroutine: leader election loop
//	      ├─ try acquire lock
//	      ├─ on success: one term (leadOneTerm) = heartbeat + drain loop until lost;
//	      │  the term's cleanup (stop heartbeat, gauge 0, release lock) is deferred,
//	      │  so a panicking term still releases the lock
//	      └─ on failure, lost or panic: sleep ~LockTTL/3, retry
//	Stop()
//	  └─ cancel ctx, wait for goroutine to exit, release lock if held.
//
// 每拍只看本 zone:队列为空直接跳过(不碰 gate 目录);非空才取一次 gate 候选,算容量与批内挑 gate
// 共用这一份,批内按「PlayerCount + 本拍已分给它的人数」轮转,一拍的放行摊到各 gate 上。
package loginqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"login/internal/logic/pkg/locker"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

// Dispatcher drains ONE zone's queue. Login replicas of the same zone compete
// for the same zone-scoped lock; replicas of other zones never contend with it.
type Dispatcher struct {
	queue        *Queue
	provider     CapacityProvider
	rdb          *redis.Client
	hmacSecret   []byte
	gateTokenTTL time.Duration

	interval          time.Duration
	softCapMultiplier float64
	lockTTL           time.Duration
	// zoneID 是本 dispatcher 唯一负责出队的 zone(本 login 的 Node.ZoneId);
	// lockKey 由它派生,锁的范围与出队范围不会分家。
	zoneID  uint32
	lockKey string

	// podID is the metric label for login_queue_dispatcher_is_leader.
	// Empty falls back to "unknown" so the gauge still publishes — used by
	// tests that don't bother setting an identity.
	podID string

	// inflight 记本进程挑定的 gate(在途票),下一拍取候选时由 provider 算进负载,跨拍不再从同一份滞后人数起算。
	// nil = 不记(单测与没接线的调用方)。只在 Start 之前经 SetGateInflight 设一次,之后只读。
	inflight *GateInflight

	cancel  context.CancelFunc
	stopped chan struct{}
	mu      sync.Mutex
}

// NewDispatcher builds a Dispatcher that drains exactly one zone. Callers must
// invoke Start exactly once.
//
// lockKeyBase 是配置里的 Queue.DispatcherLockKey,实际抢的锁键是
// ZoneDispatcherLockKey(lockKeyBase, zoneID)。zoneID 必须是本 login 的 Node.ZoneId,
// 也就是 provider 看得见 gate 的那个 zone;否则抢到了锁也出不了队。
//
// zoneID 为 0 或 lockKeyBase 为空时返回错误,并且不起任何 goroutine:zone 0 的队列条目
// 在 Poll 里被当成损坏数据(queue.go 的 Poll),空前缀会让锁键和别的业务键挤在一起。
// interval 或 lockTTL 非正同样返回错误:interval <= 0 时 drainLoop 的 time.NewTicker 在拿到锁之后
// 才 panic,起服零报错;lockTTL <= 0 时 TryLock 走不带过期的 SETNX、心跳不启动,leader 进程异常
// 退出后锁键永不过期,该 zone 永久没有 leader。两者与 zone 0 同级:开了排队却出不了队。
func NewDispatcher(
	queue *Queue,
	provider CapacityProvider,
	rdb *redis.Client,
	hmacSecret []byte,
	gateTokenTTL time.Duration,
	interval time.Duration,
	softCapMultiplier float64,
	lockTTL time.Duration,
	lockKeyBase string,
	zoneID uint32,
) (*Dispatcher, error) {
	if zoneID == 0 {
		return nil, errors.New("loginqueue: dispatcher zone id must be non-zero")
	}
	if lockKeyBase == "" {
		return nil, errors.New("loginqueue: dispatcher lock key base must be non-empty")
	}
	if interval <= 0 {
		return nil, errors.New("loginqueue: dispatch interval must be positive")
	}
	if lockTTL <= 0 {
		return nil, errors.New("loginqueue: dispatcher lock ttl must be positive")
	}
	return &Dispatcher{
		queue:             queue,
		provider:          provider,
		rdb:               rdb,
		hmacSecret:        hmacSecret,
		gateTokenTTL:      gateTokenTTL,
		interval:          interval,
		softCapMultiplier: softCapMultiplier,
		lockTTL:           lockTTL,
		zoneID:            zoneID,
		lockKey:           ZoneDispatcherLockKey(lockKeyBase, zoneID),
		stopped:           make(chan struct{}),
	}, nil
}

// ZoneDispatcherLockKey 返回 zone 级出队 leader 锁键 <base>:zone:<zoneID>。
// 同一 zone 的 login 副本抢同一把锁,不同 zone 互不相干。
// 排查时用:redis-cli GET dispatcher:lock:login_queue:zone:1
func ZoneDispatcherLockKey(base string, zoneID uint32) string {
	return fmt.Sprintf("%s:zone:%d", base, zoneID)
}

// Start launches the leader-election + drain goroutine. Returns immediately.
func (d *Dispatcher) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()

	// 先发一次 0,指标开关已开时这条序列立刻出现在 /metrics 上。但生产上此刻开关多半还没开:
	// login.go 先 ctx.Start(),之后 zrpc.MustNewServer 里的 SetUp → StartAgent 才打开 go-zero
	// 指标的全局开关,在那之前写的样本被直接丢弃。所以 leader 指标按电平重发,不按边沿写一次:
	// leader 每拍重发 1(drainLoop),备机每轮抢锁失败重发 0(electAndRun),开关一开就补上。
	d.setLeaderGauge(0)

	// safego:选主循环 panic 会让这个副本永远不再参选,而队列没有 leader 时
	// 所有排队玩家都晾在那里。裸 go func 的旧写法是"炸了就整进程死",
	// 现在换成"记点位 + 计数",故障可见且不牵连其它链路。
	// 任期内(出队路径)的 panic 不走到这一层:electAndRun 按任期接住,放锁后退避再参选。
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

// SetGateInflight 接上本进程的在途记账(与快速通道、CandidatesForZone 用同一份),必须在 Start 之前调用。
// 每挑定一台 gate 记一张在途票,见 gate_inflight.go。
func (d *Dispatcher) SetGateInflight(inflight *GateInflight) {
	d.inflight = inflight
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

// setLeaderGauge 发布本副本在本 zone 的 leader 状态。zone 是低基数标签(zone 数),
// 让「每个 zone 恰好一个 leader」能直接写成 sum by (zone) (...) == 1。
func (d *Dispatcher) setLeaderGauge(v float64) {
	dispatcherIsLeaderGauge.Set(v, d.podLabel(), labelZone(d.zoneID))
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
// run one term (leadOneTerm) until we lose the lease or shutdown is requested.
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
			// 备机每轮重发 0:指标开关打开之前写的 0 会被丢,见 Start。
			d.setLeaderGauge(0)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}

		logx.Infof("[loginqueue] elected dispatcher leader zone=%d key=%s", d.zoneID, d.lockKey)
		d.setLeaderGauge(1)

		// 一个任期在 safego.Run 里跑:任期内 panic(出队路径上的代码缺陷)只结束这一个任期。
		// 收尾都在 leadOneTerm 的 defer 里,panic 展开时照样停心跳、指标归 0、放锁,同 zone 的
		// 备机下一轮就能接手;本副本退避一轮再参选,不永久退出选举。任期收尾若不走 defer,
		// panic 后心跳 goroutine 还在续期,本副本就成了「持锁不出队」的僵尸 leader。
		if !safego.Run("login.queue_dispatcher_term", func() { d.leadOneTerm(ctx, lock) }) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}
}

// leadOneTerm 是拿到锁之后的一个任期:起心跳、出队直到丢租约或停服。收尾(停心跳 →
// leader 指标归 0 → 放锁 → 取消 drain ctx)全部走 defer,正常返回与 panic 展开走同一条路。
func (d *Dispatcher) leadOneTerm(ctx context.Context, lock *locker.Lock) {
	// Cancel drain when we lose the lease (e.g. Redis flap → key
	// expired and another replica took over). drainLoop respects this
	// ctx on every tick.
	drainCtx, drainCancel := context.WithCancel(ctx)
	defer drainCancel()
	// onLost 只取消 drain ctx、不调 stopHB:它在心跳 goroutine 里同步执行,stopHB 会等它返回。
	stopHB := lock.StartHeartbeat(d.lockTTL/3, d.lockTTL, func(err error) {
		logx.Errorf("[loginqueue] dispatcher lost leadership zone=%d: %v", d.zoneID, err)
		d.setLeaderGauge(0)
		drainCancel()
	})
	defer func() {
		stopHB()
		d.setLeaderGauge(0)
		// Best-effort release; on ctx cancel or lost lease this may no-op,
		// which is fine — the next replica's TryLock will succeed once the
		// TTL expires.
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer releaseCancel()
		_, _ = lock.Release(releaseCtx)
	}()

	d.drainLoop(drainCtx)
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
			// ticker 与 ctx 同时就绪时 select 随机选:丢租约后不再重发 1。
			if ctx.Err() != nil {
				return
			}
			// 每拍重发 1:当选那一刻写的 1 可能落在指标开关打开之前被丢,见 Start。
			d.setLeaderGauge(1)
			d.dispatchOnce(ctx)
		}
	}
}

// dispatchOnce runs one drain pass over this dispatcher's zone. 出错只记日志,
// 条目留在队列里等下一拍,不丢人。
func (d *Dispatcher) dispatchOnce(ctx context.Context) {
	zoneID := d.zoneID
	// Publish gauge snapshot first so even zero-admit zones keep their
	// gauges fresh — otherwise a zone that just finished draining
	// would show stale "size=N" in Grafana until the next entry shows up.
	publishGaugeSnapshot(ctx, d.queue, zoneID)

	// 先看队列再算容量。以前 activeZones 来自 GateWatcher,本 zone 一台 gate 都没有时
	// 根本不会走到这里;现在本 zone 总在出队范围内,不先判空的话,空闲又没 gate 的 zone
	// 会每拍刷一条 freeSlots ERROR。队列非空却没有 gate,说明有人在排而放不进去,
	// 下面照常报 ERROR。
	queueLen, err := d.queue.QueueLen(ctx, zoneID)
	if err != nil {
		// 读不出队列长度就不出队,但不能静默:只有队列键读不了(WRONGTYPE / ACL 等)而锁照常续期时,
		// 现象是「leader=1、admit 不涨」。频率与下面的 freeSlots ERROR 相同,每拍至多一条、只有 leader 打。
		// ctx 已取消(丢租约或停服那一拍)时的 context canceled 没有信息量,不报。
		if ctx.Err() == nil {
			logx.Errorf("[loginqueue] queueLen zone=%d err=%v", zoneID, err)
		}
		return
	}
	if queueLen == 0 {
		return
	}

	// 每个非空拍只取一次 gate 候选,算容量与批内挑 gate 共用这一份。生产实现每次都是一次 etcd 前缀读
	// 加一次排空标记 MGET;以前每放一个人重取一次,一拍放 N 人就是 N 次串行读,而这批条目已被 ZPOPMIN
	// 弹出、既不在队列也不在占位里,拍越长,进程中途退出时丢的越多。gate 人数一拍之内不变,重取拿到的
	// 也是同一份数据。
	// (2026-10-18 更正)「这批条目已被 ZPOPMIN 弹出、既不在队列也不在占位里……丢的越多」已不成立:PopAdmit 先 ZRANGE
	// 读候选,条目在放行脚本写成之前一直在队列里,进程中途退出不丢人。一拍只取一次候选的理由剩下往返数与批内均衡。
	// (2026-10-18 再更正)「生产实现每次都是一次 etcd 前缀读」也已不成立:改读 login 进程内的 gate 目录 list-watch 镜像,
	// 镜像不新鲜时才回落一次 etcd 前缀读;排空标记 MGET 仍是每次一次。
	candidates, err := d.provider.CandidatesForZone(ctx, zoneID)
	if err != nil {
		// 与 FreeSlots 的错误同形(candidates: ...),日志检索口径不变。
		logx.Errorf("[loginqueue] freeSlots zone=%d queueLen=%d err=candidates: %v", zoneID, queueLen, err)
		return
	}
	// 没有候选时这里回「no gates for zone」:报 ERROR、不出队,条目原样留在队列里。
	free, capacity, online, _, err := freeSlotsFromCandidates(ctx, candidates, d.provider, d.queue, zoneID, d.softCapMultiplier)
	if err != nil {
		logx.Errorf("[loginqueue] freeSlots zone=%d queueLen=%d err=%v", zoneID, queueLen, err)
		return
	}
	if free == 0 {
		return
	}

	// free 只是快照,决定这一拍最多试几条;名额本身(容量 − 在线,与快速通道同一口径)交给放行脚本,每条写放行之前在
	// 同一段里数一次未到期的占位(2026-11-11,infra-reconnect-overview.md §4.1 R7)。重叠 leader 各按同一份快照放满时,
	// 在途上界因此是 capacity − online,不再是它的两倍。free > 0 意味着 capacity > online + 占位,减法不会下溢。
	budget := capacity - online
	batch := min(free, queueLen)

	admitted, err := d.queue.PopAdmit(ctx, zoneID, int(batch), budget, recordPicks(newBatchPickFn(candidates), d.inflight))
	// PopAdmit 出错时已放行数仍然有效(2026-10-18 起):先记账,再判错误。此前出错直接返回,本拍已放行的人不计入。
	recordAdmit(zoneID, admitted)
	if admitted > 0 {
		logx.Infof("[loginqueue] zone=%d admitted=%d freeBefore=%d queueLen=%d",
			zoneID, admitted, free, queueLen)
	}
	// 每拍至多一行 ERROR,上下文(queueId、已放行、跳过、未尝试,或 zrange)在 err 里;写满时 err 含 OOM,条目原样
	// 留在队列,下一拍再试。与上面 queueLen 分支同口径:ctx 已取消(停服 / 丢租约)时不报。
	if err != nil && ctx.Err() == nil {
		logx.Errorf("[loginqueue] PopAdmit zone=%d err=%v", zoneID, err)
	}
}

// newBatchPickFn 返回一拍的挑 gate 函数:在这一拍的候选副本上逐条挑,负载 = PlayerCount + 本拍已分给它的
// 人数,取最少、同数取 NodeID 最小,选中后本地计数 +1(gatePicker)。只按 PlayerCount 挑的话,这个数一拍
// 之内不变,整批都会落到同一台 gate 上。返回的函数只能用于一次 PopAdmit(它逐条串行调用),不能跨拍复用。
//
// Pick a gate per entry but DO NOT sign the token here — the HMAC
// + expire happen at consume time inside consumeAdmitAndSign so
// the 5-min TTL window starts when the client redeems the admit,
// not when we picked the gate (R17 R1 fix). The dispatcher's job
// reduces to "decide which gate is least loaded right now".
//
// 没有候选时回错误,PopAdmit 把条目以分数 0 放回队首、不写放行槽和占位,与逐条重取候选时取到空集的
// 处理相同。dispatchOnce 传进来的候选已由 freeSlotsFromCandidates 判过非空,正常走不到这条。
// (2026-10-18 更正)「以分数 0 放回队首」改读:PopAdmit 跳过该条,条目从未离开队列、分数不变,不写放行槽和占位,
// 首个错误随 PopAdmit 的返回值交给 dispatchOnce。
//
// 记账之前先剔除停写与满载的 gate(eligibleGates,与快速通道同一份规则),全部不合格时回落全集。
// 只影响这一拍挑哪台;本拍放多少人仍由 freeSlotsFromCandidates 按全部候选的人数算。
func newBatchPickFn(candidates []GateCandidate) PickAdmitSlotFn {
	picker := newGatePicker(eligibleGates(candidates))
	return func(uint32, string) (*AdmitSlot, error) {
		gate, err := picker.pick()
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
}
