// Package auditoutbox 是审计发件箱的转发循环(docs/design/player-audit-outbox.md §7,断线重连 R13)。
//
// 发件箱 player_audit_outbox 建在每个地域存档库里,存盘事务提交存档时同事务写入(internal/playerstore/audit_outbox.go),
// 所以「发件箱里有这条」等于「它描述的改动已提交」。这里把它投到两个审计 topic,broker 全副本确认之后按主键删除:
//   - 工作单位是 (存档库, 片),片号 = player_id % playerstore.AuditOutboxShards;
//   - 分工:各单位按最高随机权重哈希分给本地域的 stable go/db(shared/workshard),只为省重复劳动;
//   - 互斥:处理一个单位之前取这一片的会话级命名锁(sql.go;MySQL 不等待,TiDB 最多等 1 秒),拿不到 = 别的实例正在投,这一轮跳过。
//     分工表变化的几秒里两个副本都认为自己负责同一片,也只有拿到锁的那个在投;
//   - 至少一次:投出去之后删除失败、进程崩溃、持锁的连接断开后被另一个实例接手,都会重投,
//     由 data_service 的消费端按 tx_id / snapshot_guid 去重(设计 §8)。删除只按投成功的那些行的主键,
//     不会删掉没投出去的行,所以不漏。
//
// 线程:Run 在一个 goroutine 里跑,Relay 的状态只在这个 goroutine 里读写;Pass 不能与 Run 并发调用。
package auditoutbox

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"db/internal/metrics"
	"db/internal/playerstore"

	storepb "proto/player_store"
	"shared/authoritysql"
	"shared/drainproof"
)

const (
	// txlogBatch、snapshotBatch 是一次读、一次投的行数。流水一行一两百字节;快照一行可到 4 MiB(playerstore.MaxAuditSnapshotBytes)。
	txlogBatch    = 500
	snapshotBatch = 8
	// maxBatchesPerUnit 限制一个单位一轮最多投几批:积压多的单位不独占一轮,别的单位照样每轮轮到。
	maxBatchesPerUnit = 20
	// visibilityMargin:created_ms 是写入方在提交事务里取的墙钟,提交之后行才可见。游标永远停在「现在 − 余量」之前,
	// 盖住提交延迟(存盘事务受 30 秒读超时约束)与 go/db 之间的时钟偏差(设计 §7.2)。
	visibilityMargin = 2 * time.Minute
	// fullScanEvery:每个单位每这么久从头扫一次,接住余量盖不住的情形(比如时钟跳变)。刚接手的单位也从头扫。
	fullScanEvery = 10 * time.Minute
	// statementTimeout 是转发的一条 SQL 的上限:一个慢库不能把整轮拖住。
	statementTimeout = 5 * time.Second
	// produceTimeout 是投一批的上限(acks=all,含 kafka-go 内部的重试)。投的时候还钉着存档库池的一条连接(持锁),
	// 而 K 个库时每库的池可能只有 2 条(k8s_deploy.ps1 Get-PlayerStorePoolLimit),所以取短:Kafka 不可用时每次只占这条连接 5 秒,
	// 随后整轮退避。健康时 acks=all 一批是几十毫秒。
	produceTimeout = 5 * time.Second
	// passBudget 是一轮的时间上限:到点就不再开始新的批和新的单位,剩下的下一轮从断点接着做、不等下一拍。
	// 一轮最长 = passBudget + 在途的一批(读、投、删各一个上限)+ 这个单位的探测、取锁、放锁,约 65 秒;
	// 活性告警的阈值按它和两轮之间的最长等待(退避上限、IntervalMs 上限)算(设计 §9)。
	passBudget = 30 * time.Second
	// relayPoint 是转发一轮的 safego 点位名:一轮 panic 时记在 safego_panic_total{point} 上,循环不死,活性不前进。
	relayPoint = "db.audit_outbox_relay"
	// backoffMin、backoffMax:一批一条都没投出去(Kafka 不可用)时的退避,每次翻倍,再乘 0.5~1 的随机系数。
	backoffMin = time.Second
	backoffMax = 30 * time.Second
	// frozenPause:存档库的写者闸门冻结(搬库切换窗口)时,这个库暂停这么久再试,不在窗口里反复投同一批。
	frozenPause = 5 * time.Second
	// failureLogEvery:同一阶段连续失败时,第一次和此后每这么多次打一条 ERROR。
	failureLogEvery = 30
)

// relayKinds 是转发的两种记录。下标就是 unitState 里游标的下标。
var relayKinds = [...]storepb.PlayerAuditKind{
	storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_TRANSACTION_LOG,
	storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_PLAYER_SNAPSHOT,
}

const kindCount = len(relayKinds)

// batchSize 是 kind 一次读、一次投的行数。
func batchSize(kind storepb.PlayerAuditKind) int {
	if kind == storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_PLAYER_SNAPSHOT {
		return snapshotBatch
	}
	return txlogBatch
}

// UnitKey 是 (存档库, 片) 在分工表里的归属键(设计 §7.1)。
func UnitKey(storageID, shard uint32) string {
	return fmt.Sprintf("audit-outbox:%d:%d", storageID, shard)
}

// Row 是发件箱的一行。片号和 kind 由读它的调用方给出,这里不重复存。
type Row struct {
	CreatedMs uint64
	PlayerID  uint64
	RecordID  uint64
	Payload   []byte
}

// Store 是一个存档库上的发件箱。生产实现是 SQLStore(sql.go),走这个库的写者闸门连接池。
type Store interface {
	// Oldest 返回 (shard, kind) 里 created_ms >= from 的第一行的 created_ms;没有这样的行时 found=false。
	// 不加锁,只用来探测有没有活:绝大多数单位绝大多数时候是空的,探测为空就不取锁。
	Oldest(ctx context.Context, shard uint32, kind storepb.PlayerAuditKind, from uint64) (createdMs uint64, found bool, err error)
	// LockUnit 取这一片的转发锁,不排队等(TiDB 上命名锁的超时下限是 1 秒)。held=false 表示另一个实例正持有它。拿到的锁必须 Release。
	LockUnit(ctx context.Context, shard uint32) (lock UnitLock, held bool, err error)
}

// UnitLock 是持有中的一片转发锁。读和删都在持锁的同一条连接上做,锁随这条连接的会话存亡。
type UnitLock interface {
	// Read 按主键序返回 (片, kind) 里 created_ms >= from 的至多 limit 行。
	Read(ctx context.Context, kind storepb.PlayerAuditKind, from uint64, limit int) ([]Row, error)
	// Delete 按主键删掉这些行。删不存在的行不算错(重投之后另一个实例已经删过)。
	Delete(ctx context.Context, kind storepb.PlayerAuditKind, rows []Row) error
	// Release 放锁并归还连接;放不掉时丢掉这条物理连接,服务端随会话结束放锁。恰好调一次。
	Release()
}

// Producer 把一批行投到 kind 对应的 topic,返回与 rows 下标对齐的结果:nil = broker 全副本确认(acks=all)。
// 生产实现是 KafkaProducer(kafka.go)。
type Producer interface {
	Produce(ctx context.Context, kind storepb.PlayerAuditKind, rows []Row) []error
}

// Options 是 New 的全部依赖。
type Options struct {
	// StorageIDs 返回要转发的存档库编号,每轮调一次(生产:proto_sql.StoreRegistry.RelayStorageIDs)。
	StorageIDs func() []uint32
	// Open 取一个存档库的发件箱(必要时按需打开这个库)。出错时本轮跳过这个库。
	Open func(ctx context.Context, storageID uint32) (Store, error)
	// Owns 判本副本负不负责一个单位(生产:workshard.Membership.Owns)。nil = 全部归本副本。
	Owns func(key string) bool
	// Relay 是维护排空的 RELAY 会话(internal/drain):每个单位处理之前 Enter,关闭之后不再开始新的单位,
	// 排空等在途的单位做完。nil = 不接排空(单测、工具)。
	Relay *drainproof.Session
	// Producer 投 Kafka。
	Producer Producer
	// Interval 是空闲时两轮之间的间隔。
	Interval time.Duration
	// Now、Jitter 为 nil 时用墙钟与 [0.5, 1) 的均匀随机系数。
	Now    func() time.Time
	Jitter func() float64
}

// Relay 是转发循环。用 New 构造。
type Relay struct {
	opts    Options
	now     func() time.Time
	jitter  func() float64
	units   map[unitID]*unitState
	paused  map[uint32]time.Time // 写者闸门冻结的库 → 暂停到这一刻
	backoff time.Duration
	// resume 是下一轮从本副本负责的单位清单里的第几个开始:一轮到点或中途停下时记下断点,下一轮接着做,
	// 清单靠后的单位不会因为前面的单位慢而一直轮不到。清单变了取模,落到哪里都只是换个起点。
	resume int
	// failures 记各阶段连续失败的次数,只给日志节流用。
	failures map[string]int
}

// New 构造转发循环。依赖缺失返回错误:装配错了宁可起不来,也不悄悄不投。
func New(opts Options) (*Relay, error) {
	switch {
	case opts.StorageIDs == nil:
		return nil, errors.New("auditoutbox: StorageIDs is nil")
	case opts.Open == nil:
		return nil, errors.New("auditoutbox: Open is nil")
	case opts.Producer == nil:
		return nil, errors.New("auditoutbox: Producer is nil")
	case opts.Interval <= 0:
		return nil, fmt.Errorf("auditoutbox: Interval must be positive, got %v", opts.Interval)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	jitter := opts.Jitter
	if jitter == nil {
		jitter = func() float64 { return 0.5 + rand.Float64()/2 }
	}
	return &Relay{
		opts:     opts,
		now:      now,
		jitter:   jitter,
		units:    map[unitID]*unitState{},
		paused:   map[uint32]time.Time{},
		failures: map[string]int{},
	}, nil
}

// Run 阻塞式地一轮接一轮转发,ctx 取消即返回。空闲时按 Interval 一拍;有单位一轮没投完就不等;
// 有一批一条都没投出去就退避。db_audit_outbox_relay_enabled 在 Run 期间报 1。
//
// 活性(db_audit_outbox_relay_last_run_unixtime,口径在 shared/looplive):起来时写一次;每一轮经 Round 跑,正常返回才写时刻;
// 一轮 panic 被兜住(safego_panic_total{point="db.audit_outbox_relay"}),按投递失败退避,循环接着跑;
// ctx 结束(停机)才删序列,删完立刻返回。
func (r *Relay) Run(ctx context.Context) {
	metrics.SetAuditOutboxRelayEnabled(true)
	defer metrics.SetAuditOutboxRelayEnabled(false)
	live := metrics.AuditOutboxRelayLive()
	live.Mark(time.Now())
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			live.Stop()
			return
		case <-timer.C:
		}
		// panic 的一轮不会给 result 赋值:按投递失败处理,退避之后再试,不在同一个坏行上空转。
		result := PassResult{ProduceFailed: true}
		live.Round(relayPoint, func() { result = r.Pass(ctx) })
		timer.Reset(r.delayAfter(result))
	}
}

// delayAfter 决定下一轮什么时候开始。
func (r *Relay) delayAfter(result PassResult) time.Duration {
	if result.ProduceFailed {
		r.backoff = min(max(r.backoff*2, backoffMin), backoffMax)
		// 几个副本同时撞上 Kafka 故障时不在同一刻一起重试。
		return time.Duration(float64(r.backoff) * r.jitter())
	}
	r.backoff = 0
	if result.More {
		return 0
	}
	return r.opts.Interval
}

// PassResult 是一轮的结局。
type PassResult struct {
	// More:有单位投满了本轮的批数还有剩,或这一轮到点(passBudget)还有单位没做,下一轮不等。
	More bool
	// ProduceFailed:有一批一条都没投出去(Kafka 不可用或拒收),本轮就此停下,之后退避。
	ProduceFailed bool
	// Forwarded:本轮投出并删除的行数。
	Forwarded int
}

// Pass 把本副本负责的单位各转发一轮,至多做到 passBudget。只在 Run 的 goroutine(或单测)里调用。
func (r *Relay) Pass(ctx context.Context) PassResult {
	deadline := r.now().Add(passBudget)
	var result PassResult
	// oldest[i]:本轮在本副本负责的单位里看到的第 i 种记录最老的 created_ms;0 = 没看到。
	var oldest [kindCount]uint64
	storageIDs := r.opts.StorageIDs()
	r.forgetUnlisted(storageIDs)
	units := r.ownedUnits(storageIDs)
	metrics.SetAuditOutboxRelayUnits(len(units), len(storageIDs)*playerstore.AuditOutboxShards)
	begin := 0
	if len(units) > 0 {
		begin = r.resume % len(units)
	}
	r.resume = 0
	pass := passState{stores: map[uint32]Store{}, skipped: map[uint32]bool{}}
	for i := range units {
		at := (begin + i) % len(units)
		if ctx.Err() != nil {
			r.resume = at
			break
		}
		if !r.now().Before(deadline) {
			// 这一轮的时间用完:剩下的单位下一轮从这里接着做,不等下一拍。
			r.resume = at
			result.More = true
			break
		}
		outcome := r.relayOne(ctx, &pass, units[at], deadline, &oldest)
		result.More = result.More || outcome.more
		result.ProduceFailed = result.ProduceFailed || outcome.produceFailed
		result.Forwarded += outcome.forwarded
		if outcome.stopPass {
			r.resume = at
			break
		}
	}
	r.publishOldest(oldest)
	return result
}

// ownedUnits 列出本副本此刻负责的单位,按 (存档库, 片) 升序;不负责的单位的游标丢掉(再接手时从头扫)。
func (r *Relay) ownedUnits(storageIDs []uint32) []unitID {
	units := make([]unitID, 0, len(storageIDs)*playerstore.AuditOutboxShards)
	for _, storageID := range storageIDs {
		for shard := uint32(0); shard < playerstore.AuditOutboxShards; shard++ {
			id := unitID{storageID: storageID, shard: shard}
			if r.opts.Owns != nil && !r.opts.Owns(UnitKey(storageID, shard)) {
				delete(r.units, id)
				continue
			}
			units = append(units, id)
		}
	}
	return units
}

// passState 是一轮之内按存档库记的状态:打开过的发件箱,和这一轮不再碰的库(打不开、读删失败、闸门冻结)。
type passState struct {
	stores  map[uint32]Store
	skipped map[uint32]bool
}

// unitID 是一个工作单位。
type unitID struct {
	storageID uint32
	shard     uint32
}

// unitState 是本副本对一个单位的游标(设计 §7.2)。只在本副本负责这个单位期间保留;不负责了就丢,
// 再接手时从头扫:那时它可能被别的副本推进过,也可能还有别的副本留下的行。
type unitState struct {
	id unitID
	// scanFrom[i]:第 i 种记录从 created_ms >= scanFrom 开始读,永远不超过「现在 − 余量」。
	scanFrom     [kindCount]uint64
	nextFullScan time.Time
}

// idle:读到空,游标推到「现在 − 余量」。
func (u *unitState) idle(i int, now time.Time) {
	u.scanFrom[i] = max(u.scanFrom[i], visibleFloorMs(now))
}

// advance:created_ms 小于 createdMs 的行都处理过了,游标推到 min(createdMs, 现在 − 余量)。
// 等于 createdMs 的行下一次照样读(同一笔存盘的记录 created_ms 相同,已删的读不到)。
func (u *unitState) advance(i int, createdMs uint64, now time.Time) {
	u.scanFrom[i] = max(u.scanFrom[i], min(createdMs, visibleFloorMs(now)))
}

// visibleFloorMs 是「现在 − 余量」的毫秒数,不早于 0。
func visibleFloorMs(now time.Time) uint64 {
	ms := now.Add(-visibilityMargin).UnixMilli()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}

// unit 取(或新建)一个单位的游标。新建的游标从 0 开始,即先全扫一次。
func (r *Relay) unit(id unitID, now time.Time) *unitState {
	if u, ok := r.units[id]; ok {
		return u
	}
	u := &unitState{id: id, nextFullScan: now.Add(fullScanEvery)}
	r.units[id] = u
	return u
}

// forgetUnlisted 丢掉不在库清单里的库的游标与暂停记录。
func (r *Relay) forgetUnlisted(storageIDs []uint32) {
	for id := range r.units {
		if !slices.Contains(storageIDs, id.storageID) {
			delete(r.units, id)
		}
	}
	for storageID := range r.paused {
		if !slices.Contains(storageIDs, storageID) {
			delete(r.paused, storageID)
		}
	}
}

// outcome 是一个单位、一个库或一种记录这一轮的结局,逐层往上并。
type outcome struct {
	more          bool
	produceFailed bool
	forwarded     int
	// stopStore:这个库这一轮不再往下做(读、删失败,或写者闸门冻结)。
	stopStore bool
	// stopPass:整轮停下(维护排空关了 RELAY 入口,或 Kafka 不可用)。
	stopPass bool
}

func (o *outcome) merge(other outcome) {
	o.more = o.more || other.more
	o.produceFailed = o.produceFailed || other.produceFailed
	o.forwarded += other.forwarded
	o.stopStore = o.stopStore || other.stopStore
	o.stopPass = o.stopPass || other.stopPass
}

// relayOne 把一个单位做一轮。所在的库这一轮已经停下(打不开、读删失败、闸门冻结暂停中)就跳过;
// 库只在本副本负责它的某一片、这一轮第一次轮到时才打开:不负责的库不占连接池。
func (r *Relay) relayOne(ctx context.Context, pass *passState, id unitID, deadline time.Time, oldest *[kindCount]uint64) outcome {
	if pass.skipped[id.storageID] {
		return outcome{}
	}
	if until, ok := r.paused[id.storageID]; ok {
		if r.now().Before(until) {
			pass.skipped[id.storageID] = true
			return outcome{}
		}
		delete(r.paused, id.storageID)
	}
	store, opened := pass.stores[id.storageID]
	if !opened {
		var err error
		if store, err = r.opts.Open(ctx, id.storageID); err != nil {
			r.fail(metrics.AuditRelayStageRead, id.storageID, err)
			pass.skipped[id.storageID] = true
			return outcome{}
		}
		pass.stores[id.storageID] = store
	}
	result := r.relayUnit(ctx, store, r.unit(id, r.now()), deadline, oldest)
	if result.stopStore {
		pass.skipped[id.storageID] = true
	}
	return result
}

// relayUnit 把一个单位做一轮:探测 → 取锁 → 每种有活的记录读、投、删,到 deadline 不再开始新的一批。
func (r *Relay) relayUnit(ctx context.Context, store Store, u *unitState, deadline time.Time, oldest *[kindCount]uint64) outcome {
	if r.opts.Relay != nil {
		finish, err := r.opts.Relay.Enter()
		if err != nil {
			// 维护排空关了 RELAY 入口:本 boot 不再开始新的单位。已经投出、还没删的行留给接手的 boot 重投,由消费端去重。
			return outcome{stopPass: true}
		}
		defer finish()
		defer func() {
			// 同 assetop:半路 panic 说不清写到哪一步,排空不能把这个 boot 当成干净收尾。
			if recovered := recover(); recovered != nil {
				r.opts.Relay.Hold()
				panic(recovered)
			}
		}()
	}
	now := r.now()
	if !now.Before(u.nextFullScan) {
		u.scanFrom = [kindCount]uint64{}
		u.nextFullScan = now.Add(fullScanEvery)
	}

	// 1. 探测:不加锁,每种记录只读最老的一行的 created_ms(不读 payload)。
	var due [kindCount]bool
	anyDue := false
	for i, kind := range relayKinds {
		probeCtx, cancel := context.WithTimeout(ctx, statementTimeout)
		createdMs, found, err := store.Oldest(probeCtx, u.id.shard, kind, u.scanFrom[i])
		cancel()
		if err != nil {
			return r.storeFailure(metrics.AuditRelayStageRead, u.id.storageID, err)
		}
		if !found {
			u.idle(i, now)
			continue
		}
		due[i], anyDue = true, true
		if oldest[i] == 0 || createdMs < oldest[i] {
			oldest[i] = createdMs
		}
	}
	r.recovered(metrics.AuditRelayStageRead)
	if !anyDue {
		return outcome{}
	}

	// 2. 互斥:拿不到 = 另一个实例正在投这一片(分工表变化的窗口里会出现),这一轮让给它。
	lockCtx, cancel := context.WithTimeout(ctx, statementTimeout)
	lock, held, err := store.LockUnit(lockCtx, u.id.shard)
	cancel()
	if err != nil {
		return r.storeFailure(metrics.AuditRelayStageRead, u.id.storageID, err)
	}
	if !held {
		return outcome{}
	}
	defer lock.Release()

	// 3. 每种有活的记录:读一批 → 投 → 只删投成功的。
	var result outcome
	for i, kind := range relayKinds {
		if !due[i] {
			continue
		}
		result.merge(r.forwardKind(ctx, lock, u, i, kind, deadline))
		if result.stopStore || result.stopPass {
			break
		}
	}
	return result
}

// forwardKind 把一个单位的一种记录投至多 maxBatchesPerUnit 批,到 deadline 不再开始新的一批(第一批照投:单位已经开始了)。持锁调用。
func (r *Relay) forwardKind(ctx context.Context, lock UnitLock, u *unitState, i int, kind storepb.PlayerAuditKind, deadline time.Time) outcome {
	var result outcome
	limit := batchSize(kind)
	kindName := playerstore.AuditKindName(kind)
	for batch := 0; batch < maxBatchesPerUnit; batch++ {
		if batch > 0 && !r.now().Before(deadline) {
			// 这一轮的时间用完:剩下的行下一轮接着投(游标已推到这一批之后),不等下一拍。
			result.more = true
			return result
		}
		readCtx, cancel := context.WithTimeout(ctx, statementTimeout)
		rows, err := lock.Read(readCtx, kind, u.scanFrom[i], limit)
		cancel()
		if err != nil {
			result.merge(r.storeFailure(metrics.AuditRelayStageRead, u.id.storageID, err))
			return result
		}
		if len(rows) == 0 {
			u.idle(i, r.now())
			return result
		}

		produceCtx, cancel := context.WithTimeout(ctx, produceTimeout)
		errs := r.opts.Producer.Produce(produceCtx, kind, rows)
		cancel()
		acked := make([]Row, 0, len(rows))
		firstFailed := -1
		var firstErr error
		for j := range rows {
			if j < len(errs) && errs[j] == nil {
				acked = append(acked, rows[j])
				continue
			}
			if firstFailed < 0 {
				firstFailed = j
				firstErr = errAt(errs, j)
			}
		}

		if len(acked) > 0 {
			deleteCtx, cancel := context.WithTimeout(ctx, statementTimeout)
			err := lock.Delete(deleteCtx, kind, acked)
			cancel()
			if err != nil {
				// 投出去了、没删掉:行留着,下一轮重投,消费端去重。游标不动。
				result.merge(r.storeFailure(metrics.AuditRelayStageDelete, u.id.storageID, err))
				return result
			}
			r.recovered(metrics.AuditRelayStageDelete)
			metrics.ObserveAuditOutboxForwarded(kindName, len(acked))
			result.forwarded += len(acked)
		}

		now := r.now()
		if firstFailed >= 0 {
			// 游标停在第一条没投成的行上,它和它之后的行下一轮再投。
			u.advance(i, rows[firstFailed].CreatedMs, now)
			r.fail(metrics.AuditRelayStageProduce, u.id.storageID,
				fmt.Errorf("%d of %d %s row(s) not acknowledged: %w", len(rows)-len(acked), len(rows), kindName, firstErr))
			if len(acked) == 0 {
				// 一条都没投出去:多半是 Kafka 不可用,整轮停下退避,不在每个单位上各撞一次超时。
				// 部分失败(个别行被拒)不退避:别的行照常投,被拒的那行留在发件箱,积压年龄告警会响。
				result.produceFailed, result.stopPass = true, true
			}
			return result
		}
		r.recovered(metrics.AuditRelayStageProduce)
		u.advance(i, rows[len(rows)-1].CreatedMs, now)
		if len(rows) < limit {
			return result
		}
	}
	// 投满了本轮的批数还有剩。
	result.more = true
	return result
}

func errAt(errs []error, j int) error {
	if j < len(errs) && errs[j] != nil {
		return errs[j]
	}
	return errors.New("producer returned no result for this row")
}

// storeFailure 处理一次读、删失败:写者闸门冻结时这个库暂停一会儿(不算错误),其余计数、打日志。都停下这个库这一轮。
func (r *Relay) storeFailure(stage string, storageID uint32, err error) outcome {
	if authoritysql.IsWriterFrozen(err) {
		// 搬库切换窗口:这次确定没写。行都还在,窗口过了接着投。
		r.paused[storageID] = r.now().Add(frozenPause)
		logx.Infof("[audit_outbox] Writer gate of storage %d is frozen during relay %s; pausing this storage for %v", storageID, stage, frozenPause)
		return outcome{stopStore: true}
	}
	r.fail(stage, storageID, err)
	return outcome{stopStore: true}
}

// fail 计一次失败;连续失败时第一次和此后每 failureLogEvery 次打 ERROR。日志不带玩家数据。
func (r *Relay) fail(stage string, storageID uint32, err error) {
	metrics.CountAuditOutboxRelayError(stage)
	n := r.failures[stage] + 1
	r.failures[stage] = n
	if n == 1 || n%failureLogEvery == 0 {
		logx.Errorf("[audit_outbox] Relay %s failed on storage %d (consecutive=%d); the rows stay in the outbox and are retried: %v",
			stage, storageID, n, err)
	}
}

// recovered 在一个阶段连续失败之后第一次成功时打一行 INFO。
func (r *Relay) recovered(stage string) {
	if n := r.failures[stage]; n > 0 {
		logx.Infof("[audit_outbox] Relay %s recovered after %d consecutive failure(s)", stage, n)
		delete(r.failures, stage)
	}
}

// publishOldest 把本轮看到的最老未投记录的年龄报给 db_audit_outbox_oldest_pending_age_seconds。
func (r *Relay) publishOldest(oldest [kindCount]uint64) {
	nowMs := r.now().UnixMilli()
	for i, kind := range relayKinds {
		age := 0.0
		if oldest[i] != 0 && nowMs > int64(oldest[i]) {
			age = float64(nowMs-int64(oldest[i])) / 1000
		}
		metrics.SetAuditOutboxOldestPendingAge(playerstore.AuditKindName(kind), age)
	}
}
