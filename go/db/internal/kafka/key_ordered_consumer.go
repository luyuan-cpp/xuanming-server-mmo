package kafka

import (
	"context"
	"crypto/sha256"
	db_config "db/internal/config"
	"db/internal/dbguard"
	"db/internal/metrics"
	"db/internal/storeerr"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	playerpb "proto/common/database"
	db_proto "proto/db"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luyuancpp/proto2mysql"

	"db/internal/locker"

	"shared/drainproof"
	"shared/kafkautil"

	"github.com/IBM/sarama"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	expandStatusExpireDuration = 30 * time.Minute
	taskResultExpireDuration   = 5 * time.Minute

	// retryDrainBudgetPerTick 限定一次 tick 最多认领多少条重试任务。
	// 有预算是为了不让重试排空长期霸占单个 goroutine、把新写饿死;
	// 取 200 是让排空能力(200/s)明显高于稳态入流(压测记录 ~90/s),
	// 这样一次 MySQL 抖动产生的积压能在秒级追平,而不是像每 tick 一条
	// 那样只涨不落。真要长期调优应提到 KafkaConfig 里。
	retryDrainBudgetPerTick = 200
)

// buildCacheKey builds the same Redis key as login's cache.BuildRedisKey:
// "{MsgType}:{Key}" — e.g. "db.PlayerData:12345"
func buildCacheKey(task *db_proto.DBTask) string {
	return fmt.Sprintf("%s:%d", task.MsgType, task.Key)
}

func cacheTTL() time.Duration {
	sec := db_config.AppConfig.ServerConfig.RedisClient.DefaultTTLSeconds
	if sec <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(sec) * time.Second
}

type KeyOrderedKafkaConsumer struct {
	consumer         sarama.ConsumerGroup
	redisClient      redis.Cmdable
	ownerEpochRedis  redis.Cmdable
	ownerEpochClient *redis.Client
	topic            string
	groupID          string
	partitionCount   int32
	subShardCount    int
	isOfflineExpand  bool

	// workers 由 workersMu 保护:ConsumeClaim(会话 goroutine)、重试消费
	// goroutine、以及按需建 worker 的写路径会并发访问。旧实现按配置
	// PartitionCnt 固定建表且无锁 —— topic 实际分区数超过配置时,新分区的
	// 消息查不到 worker,被「提交 offset 后丢弃」,静默丢玩家存盘。
	workersMu sync.RWMutex
	workers   map[int32]*worker
	started   bool // Start() 之后按需创建的 worker 要立即拉起

	wg                   *sync.WaitGroup
	ctx                  context.Context
	cancel               context.CancelFunc
	locker               *locker.RedisLocker
	retryQueueKey        string
	retryProcessingKey   string
	retryDeadQueueKey    string
	retryConsumeInterval time.Duration
	retryMaxTimes        int

	// retryOwner 管本实例对重试收据的归属(认领进自己的 processing 列表、续租、回收孤儿、停机归还),
	// 见 retry_ownership.go。生产路径恒非 nil;只有直接拼结构体的单测是 nil,那时认领退回单纯的 RPOPLPUSH。
	// retryProcessingKey 就是 retryOwner.processingKey —— 收据的 ack / 搬回 / 进死信都作用在本实例的列表上。
	retryOwner *retryOwnership
	// retryOwnerDone 在续租循环退出后关闭;Stop 等它退出之后才归还收据,否则循环会把刚摘掉的租约又登记回去。
	retryOwnerDone chan struct{}

	// placement 是按玩家落点选库的依赖(player-storage-placement.md §6),所有 worker 共用。
	placement *placementRouter

	// handoff 是消费组交接闸(group_handoff.go);只有交接形状(配置了 Kafka.GroupHandoffFrom)时非 nil,
	// 最终形状下为 nil,行为与引入之前相同。
	handoff *groupHandoff
	// handoffDone 在守望 goroutine 退出后关闭;在 Start 里创建,没 Start 过(或没有 handoff)时为 nil。
	handoffDone chan struct{}
	// gap 量再平衡空窗(db_consumer_group_rebalance_gap_seconds)。
	gap rebalanceGapTracker
	// now 缺省 time.Now(nil 时由 nowTime 回落),单测可注入。
	now func() time.Time

	// 停机(Stop)用的状态,见 Stop 的注释。
	// pullStop 在 Stop 第①步关闭(只关一次,pullStopOnce):消费循环、各 claim、重试循环据此停止取新任务。
	// 直接拼结构体的单测里为 nil,此时 pullStopped 恒为 false、select 里的这一路永不就绪,行为与引入之前相同。
	pullStop     chan struct{}
	pullStopOnce sync.Once
	// consumeDone 在消费循环退出时关闭(最后一个 session 已释放、位点已最后一次提交),Stop 第②步等它。
	consumeDone chan struct{}
	stopOnce    sync.Once
	stopMu      sync.Mutex
	stopDone    chan struct{}
	stopErr     error
	relayWork   *drainproof.Session
	ready       chan struct{}
	readyOnce   sync.Once
	readyActive bool
	readyDigest []byte
	// shutdownDrainTimeout 是停机时每个 claim 等在途任务做完的上限(≤0 时取 claimDrainTimeout);
	// Stop 第②步最多等它加 consumerShutdownGrace。
	shutdownDrainTimeout time.Duration
	// drainTimeout 是再平衡 / 失败收尾(drainAfterStop、消息通道关闭后的排空)时 claim 等在途任务与在途 owner 重试的上限;
	// ≤0 时取 claimDrainTimeout(只给单测缩短)。
	drainTimeout time.Duration

	// 重试路由(retry_routing.go,docs/design/db-retry-owner-routing.md)。零值 = shared、没有 p 键、没有租约表,
	// 行为与引入之前相同(直接拼结构体的单测)。
	//   - retryRouting:"" 视同 shared;owner 只在有 p 键时生效(ownerRouting)。
	//   - retryPartitionKeys:按原始分区的 ready kafka:retry:queue:{topic}:p{N},下标 = 分区号;shared 模式也要用它兜底排空 p*。
	//   - owned:本进程的 claim 租约表,ConsumeClaim 进出时登记 / 摘除;全部方法对 nil 接收者安全,调用点不判空。
	//   - retryTick:owner 模式轮转认领起点,只在重试协程里读写。
	//   - stranded:待重搬的收据表(retry_stranded.go),worker 与重试协程共用;nil 时(直接拼结构体的单测)搬不动的收据
	//     与引入之前一样留在本实例 processing。
	retryRouting       string
	retryPartitionKeys []string
	owned              *ownedPartitions
	retryTick          uint64
	stranded           *strandedRetries

	// durable 是 Kafka 源三类入队点(失败入重试、隔离进死信、毒消息)的入队层(durable_enqueue.go),生命周期独立于 c.ctx,
	// Stop 的最后一步才关。nil(直接拼结构体的单测)时 enqueuer() 退回 directEnqueue:同步 LPUSH,行为与引入之前相同。
	durable durableEnqueue
	// durableBackoff 记「claim 因入队未确认落盘而退出」的连续次数,消费循环据此在重新入组之前退避(rejoin_backoff.go)。
	// nil(直接拼结构体的单测)时不退避。
	durableBackoff *durableFailureBackoff

	// routeGate 是读方就绪闸(route_ready_gate.go,D1 第 3 批):只在 Placement.RequireRouteReady=true 时非 nil。
	// 闸关着时 ConsumeClaim 不取新消息、重试循环不分拣也不认领;nil 时恒放行,行为与引入之前相同。
	routeGate *routeReadyGate
}

const (
	// consumerShutdownGrace 是停机时在 claim 排空上限之外,留给 sarama 释放会话、最后一次提交位点的余量。
	consumerShutdownGrace = 5 * time.Second
	// consumeRetryBackoff 是 Consume 返回错误后、重新入组之前的等待。
	consumeRetryBackoff = 1 * time.Second
)

// nowTime 返回当前时间;now 未注入(直接拼结构体的单测)时用 time.Now。
func (c *KeyOrderedKafkaConsumer) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// enqueuer 返回 Kafka 源入队层;durable 为 nil(直接拼结构体的单测)时退回 directEnqueue(同步 LPUSH,done 在 submit
// 返回前调用),行为与引入入队层之前相同。
func (c *KeyOrderedKafkaConsumer) enqueuer() durableEnqueue {
	if c.durable != nil {
		return c.durable
	}
	return directEnqueue{rc: c.redisClient}
}

// claimDrainLimit 是再平衡 / 失败收尾时 claim 排空的上限(drainTimeout ≤0 时取 claimDrainTimeout)。
func (c *KeyOrderedKafkaConsumer) claimDrainLimit() time.Duration {
	if c.drainTimeout > 0 {
		return c.drainTimeout
	}
	return claimDrainTimeout
}

type worker struct {
	partition          int32
	taskCh             chan *workerTask
	ctx                context.Context
	redisClient        redis.Cmdable
	ownerEpochRedis    redis.Cmdable
	locker             *locker.RedisLocker
	topic              string
	retryQueueKey      string
	retryProcessingKey string
	retryDeadQueueKey  string
	wg                 *sync.WaitGroup

	// retryRouting / retryPartitionKeys 由 buildWorker 从消费者透传,决定重试写回目的地(retryReadyKeyFor);
	// 零值 = shared,一律写共享收件箱。
	retryRouting       string
	retryPartitionKeys []string
	// stranded 是消费者的待重搬收据表(由 buildWorker 透传,retry_stranded.go);nil 时搬不动的收据只打 ERROR。
	stranded *strandedRetries
	// durable 是 Kafka 源入队层(由 buildWorker 透传);nil 时 enqueuer() 退回 directEnqueue。
	durable durableEnqueue

	// appliedEpochs 记录每个 key 已成功落库的最大归属 epoch,是落库前 epoch 守卫的
	// 数据源(见 guardOwnerEpoch)。生产由 buildWorker 装成 redisAppliedEpochStore;测试
	// 注入 map 实现。nil 时守卫对带 epoch 的任务 fail-closed 成可重试错误,而不是放行。
	appliedEpochs appliedEpochStore

	// placement 为每条 read / write 任务选落点库并在落库后复核(routeTask / placementRoute.confirm)。
	// 生产由 buildWorker 装配;nil 时任务 fail-closed 成可重试错误,绝不回落到本 zone 库。
	placement *placementRouter

	// subShardCount is the number of intra-partition parallel goroutines
	// that share the work for this partition. With subShardCount=1 the
	// worker is the legacy single-goroutine behaviour. With N>1, the
	// `start` goroutine becomes a router that hashes by task.Key into N
	// sub-channels, each consumed by its own goroutine.
	//
	// Why this exists: MySQL pool capacity (MaxOpenConn=30) was hugely
	// underused because partition=10 worker.start was strictly serial —
	// only 10 MySQL connections in flight, 20 idle. With subShardCount=4
	// effective parallelism becomes 10×4=40, saturating the pool.
	//
	// Per-key ordering is preserved: same key.hash → same sub-channel →
	// same goroutine → same coalesce window. The batch coalescer only
	// looks at the *contiguous* tasks the sub-worker drained, so it
	// works identically to the legacy path within each shard.
	//
	// See 2026-05-28 stress §5 ("反转:db worker 串行才是真天花板").
	subShardCount int
	subShardChans []chan *workerTask
}

// enqueuer 返回本 worker 的 Kafka 源入队层;durable 为 nil 时退回 directEnqueue(同 KeyOrderedKafkaConsumer.enqueuer)。
func (w *worker) enqueuer() durableEnqueue {
	if w.durable != nil {
		return w.durable
	}
	return directEnqueue{rc: w.redisClient}
}

// workerTask wraps either a Kafka message or a pre-parsed retry task.
//
// seq is Kafka offset+1 (0 remains the legacy "unknown" sentinel). It is only
// comparable when originPartition is also known and matches the previously
// applied partition. Kafka offsets from different partitions are unrelated.
type workerTask struct {
	kafkaMsg *sarama.ConsumerMessage
	session  sarama.ConsumerGroupSession
	acker    *claimAcker
	dbTask   *db_proto.DBTask // non-nil for retry tasks
	seq      uint64

	originPartition    int32
	hasOriginPartition bool
	fromRetry          bool
	// claimCtx is canceled as soon as the owning Sarama claim is revoked or one
	// offset becomes non-durable. Queued old-claim work checks it before DB I/O.
	claimCtx context.Context
	// retryReceipt is the exact payload atomically moved from ready -> processing.
	// It is removed only after success/stale-drop, or atomically moved back to
	// ready/dead on failure. Empty for Kafka-origin tasks.
	retryReceipt []byte
	// enqueuedAt 是任务交给 worker 通道的时刻(ConsumeClaim / consumeOneRetryFrom / consumeOneOwnedRetry 赋值),用于
	// db_task_stage_seconds{stage="queue_wait"};零值(单测直接拼的任务)不记。
	enqueuedAt time.Time
	// retryDone 是 owner 重试的在途回执(claim 租约的 exit,retry_routing.go):任务走到终态(执行完、搬回、进死信、
	// 停机丢弃)时恰好调一次,由 finishRetry 调用。Kafka 任务与 shared 重试为 nil。
	retryDone func()
	drainDone func()
}

// finishRetry 交还 owner 重试的在途回执:先取出、置 nil 再调用,所以多次调用只生效一次。
// 任务任一时刻只归一个 goroutine(经 channel 交接:重试协程 → worker 路由 → 子 shard),无需加锁。
func (t *workerTask) finishRetry() {
	if t == nil {
		return
	}
	if t.drainDone != nil {
		done := t.drainDone
		t.drainDone = nil
		done()
	}
	if t.retryDone == nil {
		return
	}
	done := t.retryDone
	t.retryDone = nil
	done()
}

// claimAcker prevents Sarama's high-water MarkMessage semantics from skipping
// over a failed lower offset when sub-shards finish out of order. It only marks
// the largest contiguous completed prefix of one partition claim.
type claimAcker struct {
	mu        sync.Mutex
	session   sarama.ConsumerGroupSession
	order     []int64
	head      int
	tracked   map[int64]struct{}
	completed map[int64]*sarama.ConsumerMessage
	failed    bool
	cancel    context.CancelFunc
	failureCh chan error
	pending   sync.WaitGroup
}

func newClaimAcker(session sarama.ConsumerGroupSession, _ int64, cancel context.CancelFunc) *claimAcker {
	return &claimAcker{
		session:   session,
		tracked:   make(map[int64]struct{}),
		completed: make(map[int64]*sarama.ConsumerMessage),
		cancel:    cancel,
		failureCh: make(chan error, 1),
	}
}

func (a *claimAcker) track(msg *sarama.ConsumerMessage) {
	if msg == nil {
		return
	}
	a.mu.Lock()
	if _, exists := a.tracked[msg.Offset]; exists {
		a.mu.Unlock()
		return
	}
	a.tracked[msg.Offset] = struct{}{}
	a.order = append(a.order, msg.Offset)
	a.pending.Add(1)
	a.mu.Unlock()
}

// untrack 撤销一条已经 track、但还没交给 worker 的消息(停机时 ConsumeClaim 的派发 select 选中了撤拉取):
// 从 tracked 删掉、从 order 末尾摘掉、pending 减一。之后的连续前缀推进与 wait 都当它没来过;它没被标记,
// 由下一任属主从已提交位点重放。返回 false 表示没有摘(调用方打 ERROR,由停机排空的时限兜底)。
//
// 为什么它一定在 order 末尾:track 只在 ConsumeClaim 自己的 goroutine 里按 offset 递增追加,这条消息 track 之后、
// 派发之前不会再 track 别的;ack 只推进 head,压缩(append(order[:0], order[head:]...))保留尾部;fail / stop
// 不动 order;它从没完成过,head 不会越过它。三项校验(仍在 tracked、末尾下标不小于 head、末尾就是它)
// 任一不满足就什么都不做。
//
// 为什么不用 fail:fail 会取消 claimCtx、清空 completed,把已经派发出去的在途任务连同它们的位点一起作废,
// 违背停机时「已派发的做完并标记」的语义;这条消息根本没派发,只摘它一条就够了。
//
// 与 ack / fail / stop 并发安全:四者都在 a.mu 下先查后删 tracked,同一 offset 的 pending.Done 恰好一次;
// 这条消息没交给任何 worker,不会有针对它的 ack / fail;stop 不碰 tracked 与 pending,之后再 untrack 照样成立。
// pending 的 Add 在 track 里做过,这里只做对应的一次 Done;acker.wait 在 untrack 之后才创建,不违反 WaitGroup 的
// Add / Wait 规则。
func (a *claimAcker) untrack(msg *sarama.ConsumerMessage) bool {
	if msg == nil {
		return false
	}
	a.mu.Lock()
	last := len(a.order) - 1
	if _, ok := a.tracked[msg.Offset]; !ok || last < a.head || a.order[last] != msg.Offset {
		a.mu.Unlock()
		return false
	}
	delete(a.tracked, msg.Offset)
	a.order = a.order[:last]
	a.mu.Unlock()
	a.pending.Done()
	return true
}

func (a *claimAcker) ack(msg *sarama.ConsumerMessage) {
	if msg == nil {
		return
	}
	a.mu.Lock()
	if _, tracked := a.tracked[msg.Offset]; !tracked {
		a.mu.Unlock()
		return
	}
	delete(a.tracked, msg.Offset)
	if !a.failed {
		a.completed[msg.Offset] = msg
		for a.head < len(a.order) {
			offset := a.order[a.head]
			next, ok := a.completed[offset]
			if !ok {
				break
			}
			a.session.MarkMessage(next, "")
			delete(a.completed, offset)
			a.head++
		}
		if a.head >= 1024 && a.head*2 >= len(a.order) {
			a.order = append(a.order[:0], a.order[a.head:]...)
			a.head = 0
		}
	}
	a.mu.Unlock()
	a.pending.Done()
}

func (a *claimAcker) fail(msg *sarama.ConsumerMessage, err error) {
	if msg == nil {
		return
	}
	a.mu.Lock()
	if _, tracked := a.tracked[msg.Offset]; !tracked {
		a.mu.Unlock()
		return
	}
	delete(a.tracked, msg.Offset)
	a.failed = true
	// Once one offset cannot be made durable, do not touch the session again.
	// Already-marked lower offsets are safe; all remaining offsets will replay.
	a.completed = make(map[int64]*sarama.ConsumerMessage)
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	select {
	case a.failureCh <- err:
	default:
	}
	a.pending.Done()
}

func (a *claimAcker) stop() {
	a.mu.Lock()
	if !a.failed {
		a.failed = true
		a.completed = make(map[int64]*sarama.ConsumerMessage)
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
}

func (a *claimAcker) wait() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		a.pending.Wait()
		close(done)
	}()
	return done
}

const claimDrainTimeout = 10 * time.Second

// claimDrained 在 claim 的全部在途工作结束时关闭:先等 acker 的在途 Kafka 任务(含异步入队的确认,
// acker.pending 包含它们),再等租约的在途 owner 重试归零(lease.idle)。lease 为 nil 时只等 acker。
//
// 调用方必须先 lease.close() 再调本函数:close 之后 enter 一律失败,idle 拿到的 channel 不会再被换掉,
// 「排空开始之后又加一」不会发生(claimLease 的注释)。等待不随任何 ctx 结束,上限由调用方的计时器给出。
func claimDrained(acker *claimAcker, lease *claimLease) <-chan struct{} {
	ackerDone := acker.wait()
	done := make(chan struct{})
	go func() {
		<-ackerDone
		<-lease.idle()
		close(done)
	}()
	return done
}

// waitClaimDrained 在 timeout 内等 claimDrained;超时返回 false。
func waitClaimDrained(acker *claimAcker, lease *claimLease, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-claimDrained(acker, lease):
		return true
	case <-timer.C:
		return false
	}
}

func ackKafkaTask(task *workerTask) {
	if task == nil || task.kafkaMsg == nil {
		return
	}
	if task.acker != nil {
		task.acker.ack(task.kafkaMsg)
		return
	}
	if task.session != nil {
		task.session.MarkMessage(task.kafkaMsg, "")
	}
}

func failKafkaTask(task *workerTask, err error) {
	if task == nil || task.kafkaMsg == nil || task.acker == nil {
		return
	}
	task.acker.fail(task.kafkaMsg, err)
}

// dbOpHandler 在选好的落点库上执行一条任务的 SQL 半程(读 = SELECT,写 = 大字段闸 + Save),
// 不碰任何共享 Redis。
//
// 共享缓存回写与读结果发布是另一半,由 runDBOp 在落库后复核(§6.3)通过之后
// 才做。拆成两半是为了让复核卡在「SQL 已执行、别人还没看到结果」这一个点上 —— 复核写在公共路径里,
// 任何 handler(包括测试替换进来的)都绕不过去。
//
// 返回 error 而不是文本(L-04):驱动错误经 storeerr.Tag 打上来源标记、%w 包住原始错误,上层才能区分
// 「存储此刻不可用」(延后、不耗重试预算)与任务自身的问题。
type dbOpHandler func(
	ctx context.Context,
	store *proto2mysql.DB,
	task *db_proto.DBTask,
	msg proto.Message,
) (writeOutcome, error)

var dbOpHandlers = map[db_proto.DBOp]dbOpHandler{
	db_proto.DBOp_DB_OP_READ:  handleDBReadOp,
	db_proto.DBOp_DB_OP_WRITE: handleDBWriteOp,
}

// handleDBReadOp 的 ctx 是调用方的 ctx(Kafka 来源即 claimCtx):claim 被收回、停机超时时读随之取消,排空更快;
// 读不改数据,取消没有回档风险。取消导致的失败不报注册表(见 runDBOp)。
func handleDBReadOp(
	ctx context.Context,
	store *proto2mysql.DB,
	task *db_proto.DBTask,
	msg proto.Message,
) (writeOutcome, error) {
	if err := store.WithContext(ctx).FindOneByWhereClause(msg, task.WhereCase); err != nil {
		if !errors.Is(err, proto2mysql.ErrNoRowsFound) {
			return writeOutcome{}, fmt.Errorf("db read failed: %w", storeerr.Tag(err))
		}
		// 玩家双表已由正式 Create 原子建立；缺行代表权威历史缺口，不能把带 id 的读请求原样发布为存档。
		switch msg.ProtoReflect().Descriptor().FullName() {
		case (&playerpb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName(),
			(&playerpb.PlayerDatabase_1{}).ProtoReflect().Descriptor().FullName():
			return writeOutcome{}, errMissingPlayerSQLRow
		}
	}
	// C2(persist_schema.go):本实例的 SELECT 只列自己认识的列,行世代更高时读出的是截断副本,不能回填缓存、不能交给 login。
	if err := checkReadSchema(task, msg); err != nil {
		return writeOutcome{}, err
	}
	return writeOutcome{kind: writeApplied}, nil
}

// publishDBReadResult 用 NX 填冷缓存，并独立发布本次 SELECT 的结果。
// 只能在落库后复核通过之后调用(见 runDBOp)。
func publishDBReadResult(ctx context.Context, redisClient redis.Cmdable, task *db_proto.DBTask, msg proto.Message) error {
	resultData, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal read result failed: %w", err)
	}

	// Write-back: update Redis cache so login can hit cache next time
	cacheKey := buildCacheKey(task)
	cacheStart := time.Now()
	written, setErr := redisClient.SetNX(ctx, cacheKey, resultData, cacheTTL()).Result()
	if setErr != nil {
		err := setErr
		logx.Errorf("cache write-back failed on read: key=%s, taskID=%s, err=%v", cacheKey, task.TaskId, err)
	}
	cacheCost := time.Since(cacheStart)
	metrics.ObserveStage(metrics.StageCacheWrite, "read", cacheCost)
	metrics.ObserveRedisStep("read", metrics.RedisStepCacheSet, cacheCost)
	if setErr == nil {
		if written {
			metrics.CountFenceCache("read", "written")
		} else {
			metrics.CountFenceCache("read", "exists")
		}
	}

	// Write result to Redis (read ops only)
	if task.TaskId != "" {
		publishStart := time.Now()
		result := &db_proto.TaskResult{
			Success: true,
			Data:    resultData,
			Error:   "",
		}
		if err := publishTaskResult(ctx, redisClient, task, result); err != nil {
			return err
		}
		metrics.ObserveStage(metrics.StageResultPublish, "read", time.Since(publishStart))
	}

	return nil
}

// publishTaskResult 把读任务的回执交给等待方:LPUSH task:result:{taskID} + EXPIRE,再 PUBLISH 一条只带 task_id 的通知。
// 成功回执与落点失败回执(failReadTask)共用这一条路径。
//
// LPUSH / EXPIRE 必须在 PUBLISH 之前:login 一收到通知就 LPOP,TTL 到期时的兜底 LPOP 也依赖列表已经存在。
// 通知按 task.reply_instance 只发一次(taskResultNotifyTarget):合法只发发起方 login 的实例频道,为空或不合法回落
// 全服旧频道;不双发,因为新版 login 两条频道都订着。通知失败或没人收只拉长时延 —— 结果已在列表里,由 login 的
// TTL 兜底取回 —— 所以仍返回 nil;只有结果本身没写进去才返回错误,调用方据此延后重试。
func publishTaskResult(ctx context.Context, redisClient redis.Cmdable, task *db_proto.DBTask, result *db_proto.TaskResult) error {
	taskID := task.GetTaskId()
	resBytes, err := proto.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal result failed: %w", err)
	}
	resultKey := fmt.Sprintf("task:result:%s", taskID)
	if err := redisClient.LPush(ctx, resultKey, resBytes).Err(); err != nil {
		return fmt.Errorf("save read result failed: %w", err)
	}
	if err := redisClient.Expire(ctx, resultKey, taskResultExpireDuration).Err(); err != nil {
		return fmt.Errorf("set expire for result key failed: %w", err)
	}
	replyInstance := task.GetReplyInstance()
	channel, target := taskResultNotifyTarget(replyInstance)
	receivers, err := redisClient.Publish(ctx, channel, taskID).Result()
	outcome := taskResultNotifyResult(receivers, err)
	metrics.CountTaskResultNotify(target, outcome)
	switch {
	case target == metrics.TaskResultNotifyLegacyInvalid:
		// 正常的 login 不会填出不合法的回复目标,出现即说明有生产者写错了字段;发布本身的结果一并带上。
		logTaskResultNotify(taskResultNotifyLogKey(target, outcome),
			"task result notify fell back to the legacy channel, invalid reply_instance: taskID=%s len=%d reply_instance=%q result=%s err=%v",
			taskID, len(replyInstance), replyInstanceForLog(replyInstance), outcome, err)
	case err != nil:
		logTaskResultNotify(taskResultNotifyLogKey(target, outcome),
			"publish task result notify failed: taskID=%s channel=%s target=%s err=%v", taskID, channel, target, err)
	}
	return nil
}

// appliedSeqKey stores the last successfully applied Kafka cursor for a given
// (topic, player key, msg type). The key name is retained for rolling upgrade
// compatibility; new values are "v2:<partition>:<offset+1>", while old values
// were a bare offset+1 with no partition identity.
func appliedSeqKey(topic string, key uint64, msgType string) string {
	return fmt.Sprintf("consumer:applied:%s:%d:%s", topic, key, msgType)
}

type appliedCursor struct {
	partition    int32
	seq          uint64
	hasPartition bool
}

type orderingDecision uint8

const (
	orderingApply orderingDecision = iota
	orderingStale
	orderingIncomparable
)

func encodeAppliedCursor(cursor appliedCursor) string {
	if !cursor.hasPartition {
		return strconv.FormatUint(cursor.seq, 10)
	}
	return fmt.Sprintf("v2:%d:%d", cursor.partition, cursor.seq)
}

func parseAppliedCursor(raw string) (appliedCursor, error) {
	if strings.HasPrefix(raw, "v2:") {
		parts := strings.Split(raw, ":")
		if len(parts) != 3 {
			return appliedCursor{}, fmt.Errorf("invalid v2 applied cursor %q", raw)
		}
		partition, err := strconv.ParseInt(parts[1], 10, 32)
		if err != nil || partition < 0 {
			return appliedCursor{}, fmt.Errorf("invalid applied partition in %q", raw)
		}
		seq, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil || seq == 0 {
			return appliedCursor{}, fmt.Errorf("invalid applied offset in %q", raw)
		}
		return appliedCursor{partition: int32(partition), seq: seq, hasPartition: true}, nil
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || seq == 0 {
		return appliedCursor{}, fmt.Errorf("invalid legacy applied cursor %q", raw)
	}
	return appliedCursor{seq: seq}, nil
}

// orderingForTask compares offsets only when both sides prove they belong to
// the same Kafka partition. Once a cursor exists, an unqualified task or a task
// from another partition is never applied automatically: Kafka offsets provide
// no cross-partition causal order, so either direction could overwrite the
// newer full player snapshot. The caller must durably quarantine that task.
//
// 每个判定都计入 db_ordering_guard_total(只计写任务)。apply_no_cursor 单独成类:游标被清空(FLUSH / 回档)之后
// 它会成片出现,说明这道防线此刻形同虚设。
func orderingForTask(ctx context.Context, rc redis.Cmdable, topic string, dbTask *db_proto.DBTask, task *workerTask) (orderingDecision, error) {
	// Legacy retry payloads do not prove their source partition. Quarantine them
	// even when this key has no marker: Key%N is not the producer's partitioner,
	// and executing the task could race a fresh write on another worker.
	if task.fromRetry && (task.seq == 0 || !task.hasOriginPartition) {
		if dbTask.Op == db_proto.DBOp_DB_OP_WRITE {
			metrics.CountOrderingGuard(metrics.OrderingIncomparable)
		}
		return orderingIncomparable, nil
	}
	if dbTask.Op != db_proto.DBOp_DB_OP_WRITE {
		return orderingApply, nil
	}
	getStart := time.Now()
	raw, err := rc.Get(ctx, appliedSeqKey(topic, dbTask.Key, dbTask.MsgType)).Result()
	metrics.ObserveRedisStep("write", metrics.RedisStepCursorGet, time.Since(getStart))
	if errors.Is(err, redis.Nil) {
		metrics.CountOrderingGuard(metrics.OrderingApplyNoCursor)
		return orderingApply, nil
	}
	if err != nil {
		metrics.CountOrderingGuard(metrics.OrderingError)
		return orderingIncomparable, fmt.Errorf("read applied cursor: %w", err)
	}
	previous, err := parseAppliedCursor(raw)
	if err != nil {
		metrics.CountOrderingGuard(metrics.OrderingError)
		return orderingIncomparable, err
	}

	if task.seq == 0 || !task.hasOriginPartition {
		metrics.CountOrderingGuard(metrics.OrderingIncomparable)
		return orderingIncomparable, nil
	}
	if !previous.hasPartition {
		metrics.CountOrderingGuard(metrics.OrderingIncomparable)
		return orderingIncomparable, nil
	}
	if previous.partition != task.originPartition {
		metrics.CountOrderingGuard(metrics.OrderingIncomparable)
		return orderingIncomparable, nil
	}
	if task.seq <= previous.seq {
		metrics.CountOrderingGuard(metrics.OrderingStale)
		return orderingStale, nil
	}
	metrics.CountOrderingGuard(metrics.OrderingApply)
	return orderingApply, nil
}

// markAppliedSeq records a partition-qualified cursor only after the business
// write succeeds. Ordering metadata is deliberately persistent (expiration=0):
// tying it to the 24h business-cache TTL would let a delayed retry resurrect
// after the guard expired and overwrite a newer full snapshot.
func markAppliedSeq(ctx context.Context, rc redis.Cmdable, topic string, dbTask *db_proto.DBTask, task *workerTask) error {
	if task.seq == 0 || dbTask.Op != db_proto.DBOp_DB_OP_WRITE {
		return nil
	}
	cursor := appliedCursor{seq: task.seq, partition: task.originPartition, hasPartition: task.hasOriginPartition}
	setStart := time.Now()
	err := rc.Set(ctx, appliedSeqKey(topic, dbTask.Key, dbTask.MsgType), encodeAppliedCursor(cursor), 0).Err()
	_ = setStart
	return err
}

// ───────────────────────── 归属 epoch 守卫(reentry-barrier §6.3)─────────────────────────
//
// 为什么 applied-seq 挡不住这类写:seq 是 Kafka offset,只表达「同一分区内谁先到」。
// 被废黜的老节点(etcd 分区期的僵尸、屏障估算失手的疏散源)照样能在新主之后
// 产出更大的 offset,seq 守卫会把它当成更新的版本放行,MySQL 回档。
// epoch 与时间无关:归属每变一次 scene_manager INCR 一次,老节点手里的值永远追不上。
// Redis 侧 PlayerAllData 由 C++ Lua CAS 拦;这里拦的是 Kafka→MySQL 这条通道,
// 只关 Redis 的门不关这扇窗等于没关(§6.3 原话)。
//
// 比对对象是「该 (topic,key,msgType) **已成功落库**的最大 epoch」,不是 Redis 里
// scene_manager 铸造的当前值。两者的区别正好是要不要误伤合法写:
//
//	源节点以 epoch=E 存盘 → 同时发 DBTask(E) → Redis 落地 → 写交接标记 →
//	scene_manager 放行并 INCR 到 E+1,整段只有几十毫秒;而 DBTask(E) 经 Kafka 到这里
//	常常晚几百毫秒到数秒。拿「当前值 E+1」去比,这笔**传送前的最终态**每次都会被当成
//	僵尸写丢掉 —— MySQL 少一版,stale 计数在没有任何双主的情况下持续上涨,「压测期
//	恒 0」的红线(cross-zone-scene-travel.md §6 不变量 3)失去判别力。
//
// 要拦的是「新主**已经落库**之后,老 epoch 的写才到」—— 只有这种才会回档。所以:
// task.epoch < 已落库的最大 epoch → 拒;>= → 放行并在落库成功后推进。新主落库之前到达
// 的老主最后一笔按 offset 顺序正常落库,之后新主的写再盖上去,与 Redis 侧的最终状态一致。

// appliedEpochKey 记录某个 (topic, key, msgType) 已成功落库的最大归属 epoch。
// 与 appliedSeqKey 分开存而不是往 cursor 里加一段:cursor 的值格式是滚动升级契约,
// 旧版 db 读到不认识的格式会把任务判成 orderingIncomparable 送去隔离。
func appliedEpochKey(topic string, key uint64, msgType string) string {
	return fmt.Sprintf("consumer:applied_epoch:%s:%d:%s", topic, key, msgType)
}

// luaDecimalGreaterFn 是 Lua 里比较两个无符号十进制串大小的函数定义,拼在需要它的脚本前面用。
//
// 为什么不用 tonumber:Redis Lua 的数字是 double,大于 2^53 的相邻整数会舍入成同一个数,更大的值推不进去;
// owner_epoch 等 uint64 计数一旦越过 2^53(或被写成雪花一类的大数),只升不降的比较就会静默失效。
// 规范十进制串(无前导 0)先比长度、再逐字节比,与数值大小一致;用 string.byte 而不是 < 比较字符串,
// 不受 strcoll / locale 影响。
//
// is_dec(s):s 是 "0" 或不以 0 开头的纯数字串。dec_gt(a, b):a、b 都是规范十进制串时 a > b。
// 复用约定:条件写等后续脚本要比较 uint64 时直接拼这一段,不要再写 tonumber。
const luaDecimalGreaterFn = `
local function is_dec(s)
    return type(s) == 'string' and (s == '0' or string.match(s, '^[1-9]%d*$') ~= nil)
end
local function dec_gt(a, b)
    if #a ~= #b then
        return #a > #b
    end
    for i = 1, #a do
        local x, y = string.byte(a, i), string.byte(b, i)
        if x ~= y then
            return x > y
        end
    end
    return false
end
`

// luaMarkAppliedEpoch 只升不降地记录已落库的 epoch。同一 key 的写在单个 worker 内
// 串行,但重试队列可以让更老的任务晚于更新的任务成功,所以必须取 max 而不是直接 SET。
// 比较按十进制串做(luaDecimalGreaterFn),uint64 全范围精确;Redis 里的值格式不变,仍是规范十进制。
// 参数或存量值不是规范十进制时返回错误(调用方 markAppliedOwnerEpoch 只记日志),不改存量值;
// 返回值只有整数 1 或错误,不向客户端返回 Lua 布尔。
const luaMarkAppliedEpoch = luaDecimalGreaterFn + `
local new = ARGV[1]
if not is_dec(new) then
    return redis.error_reply('applied owner epoch argument is not a canonical decimal: ' .. tostring(new))
end
local cur = redis.call('GET', KEYS[1])
if not cur then
    if new ~= '0' then
        redis.call('SET', KEYS[1], new)
    end
    return 1
end
if not is_dec(cur) then
    return redis.error_reply('stored applied owner epoch is not a canonical decimal: ' .. cur)
end
if dec_gt(new, cur) then
    redis.call('SET', KEYS[1], new)
end
return 1
`

// appliedEpochStore 读写「已落库的最大归属 epoch」。抽成接口只为测试替身能覆盖
// 「读失败」分支 —— miniredis 一关连 applied-seq 的 GET 也会先失败,隔离不出这一支。
type appliedEpochStore interface {
	// ReadAppliedEpoch 返回 (epoch, found, err)。found=false 表示还没有任何带 epoch 的
	// 写落过库(不是错误);err 非 nil 表示读失败或值不是十进制整数,一律按可重试处理。
	ReadAppliedEpoch(ctx context.Context, topic string, key uint64, msgType string) (uint64, bool, error)
	// MarkAppliedEpoch 在业务写成功之后调用,只升不降。
	MarkAppliedEpoch(ctx context.Context, topic string, key uint64, msgType string, epoch uint64) error
}

// redisAppliedEpochStore 是生产实现,与 applied cursor 用同一个 Redis。
// 不设过期(与 markAppliedSeq 同理):跟业务缓存的 TTL 走,守卫过期后迟到的重试就能
// 复活并盖掉新主的整份快照。
type redisAppliedEpochStore struct {
	rc redis.Cmdable
}

func (r redisAppliedEpochStore) ReadAppliedEpoch(ctx context.Context, topic string, key uint64, msgType string) (uint64, bool, error) {
	getStart := time.Now()
	raw, err := r.rc.Get(ctx, appliedEpochKey(topic, key, msgType)).Result()
	metrics.ObserveRedisStep("write", metrics.RedisStepEpochGet, time.Since(getStart))
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read applied owner epoch: %w", err)
	}
	epoch, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		// 该键只由 luaMarkAppliedEpoch 写,出现非整数只可能是人手改坏。当错误(→ 重试 →
		// 死信)而不是当缺键放行:放行会让一个被改坏的键静默关掉整条防线。
		return 0, false, fmt.Errorf("applied owner epoch %q for key %d is not a decimal integer: %w", raw, key, err)
	}
	return epoch, true, nil
}

func (r redisAppliedEpochStore) MarkAppliedEpoch(ctx context.Context, topic string, key uint64, msgType string, epoch uint64) error {
	markStart := time.Now()
	err := r.rc.Eval(ctx, luaMarkAppliedEpoch, []string{appliedEpochKey(topic, key, msgType)},
		strconv.FormatUint(epoch, 10)).Err()
	_ = markStart
	return err
}

// ownerEpochVerdict 是一次守卫判定。outcome 直接就是 metrics 的 label 取值。
type ownerEpochVerdict struct {
	outcome      string
	appliedEpoch uint64 // 已落库的最大 epoch;没有记录 / 未读时为 0
	reject       bool
}

// checkOwnerEpoch 只做比对,不碰 metrics / 日志 / ACK,让判定规则能被单独单测。
//
// 规则(任务侧 taskEpoch 对已落库的最大 epoch applied):
//
//	taskEpoch == 0        → legacy_zero,放行,不读 Redis(旧版生产者兼容窗口)
//	还没有 applied 记录   → first,放行(该 key 第一笔带 epoch 的写)
//	taskEpoch <  applied  → stale,拒绝(新主已落库之后才到的老 epoch 写)
//	taskEpoch == applied  → match,放行(同一持有者的后续写,绝大多数流量)
//	taskEpoch >  applied  → advance,放行(归属变更后新主的第一笔写)
//	读失败                → err,由调用方按可重试处理
func checkOwnerEpoch(ctx context.Context, store appliedEpochStore, topic string, dbTask *db_proto.DBTask) (ownerEpochVerdict, error) {
	if dbTask.OwnerEpoch == 0 {
		return ownerEpochVerdict{outcome: metrics.OwnerEpochLegacyZero}, nil
	}
	if store == nil {
		// 装配缺失是编程错误。fail-closed 成可重试错误而不是放行:放行会让漏装配
		// 变成一条永远不响的假防线。
		return ownerEpochVerdict{}, errors.New("applied owner epoch store not configured")
	}
	applied, found, err := store.ReadAppliedEpoch(ctx, topic, dbTask.Key, dbTask.MsgType)
	if err != nil {
		return ownerEpochVerdict{}, err
	}
	if !found {
		return ownerEpochVerdict{outcome: metrics.OwnerEpochFirst}, nil
	}
	switch {
	case dbTask.OwnerEpoch < applied:
		return ownerEpochVerdict{outcome: metrics.OwnerEpochStale, appliedEpoch: applied, reject: true}, nil
	case dbTask.OwnerEpoch > applied:
		return ownerEpochVerdict{outcome: metrics.OwnerEpochAdvance, appliedEpoch: applied}, nil
	default:
		return ownerEpochVerdict{outcome: metrics.OwnerEpochMatch, appliedEpoch: applied}, nil
	}
}

// ownerEpochsDiverge 判定两条写是否来自不同的归属期。0 是「未知」,与任何值都不算分歧,
// 与 checkOwnerEpoch 对 0 的兼容口径一致。
func ownerEpochsDiverge(a, b uint64) bool {
	return a != 0 && b != 0 && a != b
}

// guardOwnerEpoch 在 handleTask 里跑守卫,返回 false 表示任务已被终态处理(丢弃或转重试),
// 调用方直接 return。只对 write 任务调用;read 不改数据,不需要归属。
//
// 位置约束:必须在 applied-seq 判定**之后**、执行 SQL **之前**。放在 seq 之后是因为
// seq 已经过滤掉了纯粹的重复投递,这里只剩「按 offset 看是新的、按归属看是老的」这一种。
func (w *worker) guardOwnerEpoch(task *workerTask, dbTask *db_proto.DBTask) bool {
	verdict, err := checkOwnerEpoch(w.ctx, w.appliedEpochs, w.topic, dbTask)
	if err != nil {
		// 与 ordering guard unavailable 同一条路:Kafka 源 → 持久化进重试队列后 ACK,
		// 重试源 → 收据原子搬回 ready;retryMaxTimes 之后进死信队列由人处理。
		// 绝不静默放行(那是把 Redis 抖动变成串档),也绝不原地丢(那是丢盘)。
		metrics.CountOwnerEpochGuard(metrics.OwnerEpochReadError)
		w.deferFailedTask(task, dbTask, fmt.Errorf("owner epoch guard unavailable: %w", err))
		return false
	}
	metrics.CountOwnerEpochGuard(verdict.outcome)

	switch verdict.outcome {
	case metrics.OwnerEpochStale:
		if fencingDual.Load() {
			regressed, probeErr := w.ownerCounterRegressed(dbTask.Key, verdict.appliedEpoch)
			if probeErr != nil {
				w.deferFailedTask(task, dbTask, probeErr)
				return false
			}
			if regressed {
				metrics.CountOwnerEpochGuard(metrics.OwnerEpochRegressed)
				logx.Errorf("OWNER-EPOCH-REGRESSED taskID=%s", dbTask.TaskId)
				w.quarantineOrderingConflict(task, dbTask, metrics.DeadLetterOwnerEpochRegressed, "owner epoch counter is below committed epoch")
				return false
			}
		}
		metrics.CountStaleOwnerWriteRejected()
		logx.Errorf("STALE-OWNER-WRITE rejected: key=%d taskID=%s msgType=%s taskEpoch=%d appliedEpoch=%d workerPartition=%d originPartition=%d seq=%d fromRetry=%v",
			dbTask.Key, dbTask.TaskId, dbTask.MsgType, dbTask.OwnerEpoch, verdict.appliedEpoch,
			w.partition, task.originPartition, task.seq, task.fromRetry)
		// 丢弃 = 终态,口径与 orderingStale 完全一致:不落库、不重试、不推进 applied cursor。
		// 不推进的理由:cursor 只挡「更小的 seq」,同 key 后续更高 seq 的任务不会被卡住;
		// 这条任务的重复投递再来时会再撞一次只升不降的 applied epoch,结论不变。推进反而把
		// 「已落库」写假 —— cursor 的定义是最后一次**成功应用**的位置。
		// 同一版本重投结论不变,补投器据终态停止(save_outbox_note.go)。
		w.markSaveOutboxTerminal(dbTask, "stale_owner_redis")
		if task.fromRetry {
			if ackErr := ackRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, task.retryReceipt); ackErr != nil {
				logx.Errorf("ack stale-owner retry receipt failed; it remains recoverable: taskID=%s err=%v", dbTask.TaskId, ackErr)
			}
		}
		ackKafkaTask(task)
		return false
	case metrics.OwnerEpochAdvance:
		// 归属变更后新主的第一笔写:正常事件,留一条 INFO 方便对账交接时间线。
		logx.Infof("owner-epoch advance: key=%d taskID=%s msgType=%s taskEpoch=%d appliedEpoch=%d",
			dbTask.Key, dbTask.TaskId, dbTask.MsgType, dbTask.OwnerEpoch, verdict.appliedEpoch)
	case metrics.OwnerEpochLegacyZero:
		// 兼容窗口内每条旧版写都会走到这里,只计数不刷日志;窗口该收口时看 counter 归零即可。
		logx.Debugf("owner-epoch guard skipped for legacy producer: key=%d taskID=%s msgType=%s",
			dbTask.Key, dbTask.TaskId, dbTask.MsgType)
	}
	return true
}

// markAppliedOwnerEpoch 在业务写与 applied cursor 都已持久化之后推进已落库的 epoch。
// 失败不回滚业务写、也不让任务重试:代价只是守卫暂时少看到一格(之后同一持有者的
// 任何一次成功写都会补上),而重试一笔已经落库的写没有任何收益。
func (w *worker) markAppliedOwnerEpoch(dbTask *db_proto.DBTask) {
	if dbTask.Op != db_proto.DBOp_DB_OP_WRITE || dbTask.OwnerEpoch == 0 || w.appliedEpochs == nil {
		return
	}
	if err := w.appliedEpochs.MarkAppliedEpoch(w.ctx, w.topic, dbTask.Key, dbTask.MsgType, dbTask.OwnerEpoch); err != nil {
		logx.Errorf("persist applied owner epoch failed (guard lags by one step until the next write): key=%d taskID=%s msgType=%s epoch=%d err=%v",
			dbTask.Key, dbTask.TaskId, dbTask.MsgType, dbTask.OwnerEpoch, err)
	}
}

// retryPayloadMagic identifies the new retry-queue payload format that
// carries an explicit seq prefix. The byte 0x01 is unambiguous because raw
// proto3-marshaled DBTask bytes always start with a field tag (≥ 0x08).
const retryPayloadMagic byte = 0x01

// retryPayloadMagicV2 adds the origin Kafka partition after seq, so retries
// can be routed back to the exact worker that owns the key's serialization.
const retryPayloadMagicV2 byte = 0x02

// wrapRetryPayload encodes (seq, origin partition, dbTask bytes) as the
// on-wire retry payload. Format: [magic 1B][seq 8B BE][partition 4B BE][dbTask bytes].
//
// partition 必须随载荷携带:重试任务只有回到**原始 Kafka 分区**的 worker,
// 才能和同一 key 的新消息保持串行。生产端分区按 kafkautil.DBTaskPartition
// (CRC-32(key) mod N)决定,消费端拿 Key%N 重算得到的通常是另一个分区 —— 旧实现因此把同一
// 玩家的重试与新写并发跑在两个 goroutine 里,旧 seq 可以覆盖新 seq。
// 原始分区一律以载荷里记的为准,不用契约函数重算:载荷记的是消息实际所在的分区,
// 契约被破坏时两者会不一样。
func wrapRetryPayload(seq uint64, partition int32, taskBytes []byte) []byte {
	buf := make([]byte, 1+8+4+len(taskBytes))
	buf[0] = retryPayloadMagicV2
	binary.BigEndian.PutUint64(buf[1:9], seq)
	binary.BigEndian.PutUint32(buf[9:13], uint32(partition))
	copy(buf[13:], taskBytes)
	return buf
}

// unwrapRetryPayload reads (seq, partition, hasPartition, dbTask bytes).
// v1 payloads(无分区)与 legacy payloads(无 magic)仍可解出任务,
// 只是拿不到原始分区(hasPartition=false),由调用方持久化隔离并报警。
func unwrapRetryPayload(payload []byte) (uint64, int32, bool, []byte) {
	if len(payload) >= 13 && payload[0] == retryPayloadMagicV2 {
		return binary.BigEndian.Uint64(payload[1:9]),
			int32(binary.BigEndian.Uint32(payload[9:13])), true, payload[13:]
	}
	if len(payload) >= 9 && payload[0] == retryPayloadMagic {
		return binary.BigEndian.Uint64(payload[1:9]), 0, false, payload[9:]
	}
	return 0, 0, false, payload
}

const poisonPayloadMagic byte = 0x03

type poisonMessageRecord struct {
	Topic      string `json:"topic"`
	Partition  int32  `json:"partition"`
	Offset     int64  `json:"offset"`
	Key        []byte `json:"key"`
	Value      []byte `json:"value"`
	Error      string `json:"error"`
	CapturedAt int64  `json:"captured_at_unix_ms"`
}

func encodePoisonMessage(msg *sarama.ConsumerMessage, decodeErr error) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("cannot encode nil poison message")
	}
	record := poisonMessageRecord{
		Topic:      msg.Topic,
		Partition:  msg.Partition,
		Offset:     msg.Offset,
		Key:        append([]byte(nil), msg.Key...),
		Value:      append([]byte(nil), msg.Value...),
		Error:      decodeErr.Error(),
		CapturedAt: time.Now().UnixMilli(),
	}
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 1+len(body))
	payload[0] = poisonPayloadMagic
	copy(payload[1:], body)
	return payload, nil
}

// blobGuard 是落库前的大字段三档闸,由 InitBlobGuard 在启动时装配。
//
// 用包级变量而不是塞进 worker:handleDBWriteOp 是 dbOpHandlers 里的函数值,
// 签名被 dbOpHandler 钉死,而这道闸必须卡在**每一条**写路径上(包括测试替换
// handler 之外的真实路径)。默认值保证即使忘了 InitBlobGuard 也有闸在。
var blobGuard = newBlobGuard(db_config.BlobGuardConfig{})

// 默认保护闸与启动装配共用配置默认入口，缺 YAML 不能退回另一份列预算。
func newBlobGuard(cfg db_config.BlobGuardConfig) *dbguard.Guard {
	cfg = cfg.WithDefaults()
	perTable := make(map[string]dbguard.TableLimit, len(cfg.PerTable))
	for table, limit := range cfg.PerTable {
		perTable[table] = dbguard.TableLimit{
			MaxColumnBytes: limit.MaxColumnBytes,
			MaxRowBytes:    limit.MaxRowBytes,
			ColumnBytes:    limit.ColumnBytes,
		}
	}
	return dbguard.NewGuard(dbguard.Limits{
		MaxColumnBytes: cfg.MaxColumnBytes,
		MaxRowBytes:    cfg.MaxRowBytes,
		WarnRatio:      cfg.WarnRatio,
		ReportOnly:     cfg.ReportOnly,
		PerTable:       perTable,
	}, metrics.BlobObserver{})
}

// InitBlobGuard 按配置装配大字段闸。启动时调用一次。
func InitBlobGuard(cfg db_config.BlobGuardConfig) {
	blobGuard = newBlobGuard(cfg)
	limits := blobGuard.Limits()
	logx.Infof("blob size gate: maxColumn=%dB maxRow=%dB warnRatio=%.2f reportOnly=%v perTable=%d",
		limits.MaxColumnBytes, limits.MaxRowBytes, limits.WarnRatio, limits.ReportOnly, len(limits.PerTable))
}

// handleDBWriteOp 的 ctx 来自排序锁租约(runDBOp → orderingLease.beginSQL):只在失租时取消,claim 收回、停机都不取消
// 正在跑的写 SQL。取消之后锁随任务一起释放,而服务端可能还在执行这条语句,可能在别的实例写入新版本之后才提交 ——
// 所以只有锁已经(或快要)不归本实例时才取消,且取消后锁留给 TTL(ordering_lease.go)。这不是栅栏:驱动取消只关
// 客户端连接、不发 KILL QUERY,已经到了 MySQL 的语句可能照样提交。
func handleDBWriteOp(
	ctx context.Context,
	store *proto2mysql.DB,
	task *db_proto.DBTask,
	msg proto.Message,
) (writeOutcome, error) {
	// 大字段闸必须卡在 Save **之前**:一旦写进 MEDIUMBLOB,后面 Redis 写回、
	// login 侧读取、下一次全量存盘都要原样搬运同一坨字节,拒不拒已经不重要了。
	// 超限的任务照常走 deferFailedTask → 重试队列 → 死信,payload 不会丢,
	// 由人工决定是裁剪数据还是调高上限。
	guardResult, guardErr := blobGuard.Check(task.MsgType, msg)
	if warnings := guardResult.Warnings(); len(warnings) > 0 {
		logx.Errorf("BLOB-SIZE %s key=%d taskID=%s: %s",
			guardResult.Level, task.Key, task.TaskId, strings.Join(warnings, "; "))
	}
	if guardErr != nil {
		return writeOutcome{}, fmt.Errorf("db write rejected by blob size gate: %w", guardErr)
	}

	// Save 的实际语句:按主键 UPDATE → 0 行则 INSERT → 撞 1062 再 UPDATE → 仍 0 行用 FOR UPDATE 归类(proto2mysql v0.2.0)。
	// 玩家两张表走行内围栏（player_row_fence.go）；存储侧错误同样打 storeerr 标记，让 L-04 的延后 / 冷却 / 预算豁免生效。
	// SQL 拒写是正常结局，只有驱动错误进入 storeerr。
	if handled, outcome, err := savePlayerRow(ctx, store, task, msg); handled {
		return outcome, storeerr.Tag(err)
	}
	if err := store.WithContext(ctx).Save(msg); err != nil {
		metrics.CountFenceWrite("unfenced", "error")
		return writeOutcome{}, fmt.Errorf("db write failed: %w", storeerr.Tag(err))
	}
	metrics.CountFenceWrite("unfenced", "updated")
	return writeOutcome{kind: writeApplied}, nil
}

func NewKeyOrderedKafkaConsumer(
	cfg db_config.Config,
	redisClient redis.Cmdable,
	placementRedis redis.Cmdable,
	stores StoreResolver,
) (*KeyOrderedKafkaConsumer, error) {
	if placementRedis == nil || stores == nil {
		return nil, errors.New("placement routing needs both the placement Redis client and the store registry")
	}
	if cfg.ZoneId == 0 {
		return nil, errors.New("placement routing needs ZoneId > 0")
	}
	// 主机名取不到时由 resolveRetryInstanceID 报错(除非显式配置了 Kafka.RetryInstanceId)。
	hostname, _ := os.Hostname()
	retryInstanceID, err := resolveRetryInstanceID(cfg.ServerConfig.Kafka.RetryInstanceId, hostname, cfg.ListenOn)
	if err != nil {
		return nil, err
	}

	// 对本地副本再 Normalize 一次(幂等):防止调用方漏调时消费组超时键为 0、校验必然失败。
	cfg.Normalize()
	kcfg := cfg.ServerConfig.Kafka
	if err := kcfg.ValidateConsumerGroup(); err != nil {
		return nil, fmt.Errorf("invalid Kafka consumer group config: %w", err)
	}
	// 组名一律来自配置字面量(最终形状 GroupID / 交接形状 ConsumerGroup),运行时不拼 _z 后缀。
	groupID := kcfg.EffectiveGroupID()

	// 重试路由(retry_routing.go):db.go 已经用 ResolveRetryRouting 合并配置与环境变量并回写,这里再兜一道
	// (owner 要求 PartitionCnt > 0)。放在建消费组之前:拒绝时还没有要收拾的连接。
	retryRouting, err := resolveConsumerRetryRouting(kcfg.RetryRouting, kcfg.PartitionCnt)
	if err != nil {
		return nil, fmt.Errorf("retry routing: %w", err)
	}

	// 排序锁失租后被取消的 SQL 在服务端最多还能活两个会话锁等待之和;租约的本地截止时间余量必须盖得住它
	// (ordering_lease.go)。不满足只打 ERROR 不拒启:削弱的是失租之后的保护,拒启等于停掉整个 zone 的存盘。
	if err := leaseMarginCoversLockWaits(cfg.ServerConfig.Database.SessionLockWaitSeconds,
		cfg.ServerConfig.Database.SessionInnodbLockWaitSeconds, orderingLeaseSafetyMargin); err != nil {
		logx.Errorf("ordering lease safety margin check failed (startup allowed): %v", err)
	}

	// 读方门禁(route_ready_gate.go,D1 第 3 批):Placement.RequireRouteReady=false(缺省)时两者都是 nil。
	// 放在建消费组之前:装配被拒时还没有要收拾的连接。
	routeReader, routeGate, err := newRouteReadyGating(cfg.Placement, placementRedis)
	if err != nil {
		return nil, fmt.Errorf("route ready gating: %w", err)
	}

	config := sarama.NewConfig()
	config.Version = consumerKafkaVersion
	config.Metadata.AllowAutoTopicCreation = false
	config.ClientID = consumerClientID(cfg.ZoneId, hostname)
	config.Consumer.Return.Errors = true
	config.Consumer.Group.Session.Timeout = time.Duration(kcfg.SessionTimeoutMs) * time.Millisecond
	config.Consumer.Group.Heartbeat.Interval = time.Duration(kcfg.HeartbeatIntervalMs) * time.Millisecond
	config.Consumer.Group.Rebalance.Timeout = time.Duration(kcfg.RebalanceTimeoutMs) * time.Millisecond
	// 保持 OffsetOldest:按 zone 拆组时新组的起点由交接闸在 Setup 里用对方组的已提交位点播种(group_handoff.go),
	// 不靠改这里 —— 改成 OffsetNewest 会让真正的全新 topic 丢掉第一个 session 之前写入的消息。
	config.Consumer.Offsets.Initial = sarama.OffsetOldest

	consumerGroup, err := sarama.NewConsumerGroup(kcfg.Brokers, groupID, config)
	if err != nil {
		return nil, fmt.Errorf("create consumer group failed: groupID=%s, err=%w", groupID, err)
	}

	// 交接形状才建交接闸;最终形状下返回 nil,不连 admin、不依赖对方组。inspector 懒建,出错即重建。
	handoff, err := newGroupHandoffFromConfig(kcfg, groupID, func() (groupInspector, error) {
		return newSaramaGroupInspector(kcfg.Brokers, consumerKafkaVersion, config.ClientID)
	})
	if err != nil {
		_ = consumerGroup.Close()
		return nil, fmt.Errorf("group handoff config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}

	// 键名集中在 retry_routing.go。共享 ready 在 owner 模式下是「未分拣收件箱」:回收、停机归还、旧版实例都写这里,
	// 由 owner 实例分拣进 p{N}。
	retryQueueKey := retryReadyKey(cfg.ServerConfig.Kafka.Topic)
	// processing 列表按实例隔离:同一个 zone 可以有多个 db 实例,谁认领的收据归谁管(retry_ownership.go)。
	retryOwner := newRetryOwnership(redisClient, cfg.ServerConfig.Kafka.Topic, retryQueueKey, retryInstanceID,
		retryLeaseDuration(cfg.ServerConfig.Kafka.RetryLeaseSeconds))
	// 续租与认领时把生效模式登记进模式 HASH(retry_ownership.go 文件头),回滚工具据此拦「还有 owner 实例」;
	// 取值与 db_retry_routing_mode 相同(owner 只在有 p 键时生效)。
	retryOwner.routingMode = retryRouteModeOf(retryRouting, cfg.ServerConfig.Kafka.PartitionCnt > 0)
	retryProcessingKey := retryOwner.processingKey
	retryDeadQueueKey := retryDeadKey(cfg.ServerConfig.Kafka.Topic)
	lockerIns := locker.NewRedisLocker(redisClient)
	durableBackoff := &durableFailureBackoff{}

	subShardCount := cfg.ServerConfig.Kafka.SubShardCount
	if subShardCount <= 0 {
		subShardCount = 1 // 1 = legacy single-goroutine-per-partition behaviour
	}

	var ownerEpochClient *redis.Client
	var ownerEpochRedis redis.Cmdable = redisClient
	if pr := cfg.ServerConfig.OwnerEpochRedis; pr != nil {
		ownerEpochClient = redis.NewClient(&redis.Options{Addr: pr.Hosts, Password: pr.Password, DB: pr.DB, PoolSize: pr.PoolSize, DisableIndentity: true})
		ownerEpochRedis = ownerEpochClient
	}
	c := &KeyOrderedKafkaConsumer{
		consumer:             consumerGroup,
		redisClient:          redisClient,
		ownerEpochRedis:      ownerEpochRedis,
		ownerEpochClient:     ownerEpochClient,
		topic:                cfg.ServerConfig.Kafka.Topic,
		groupID:              groupID,
		partitionCount:       cfg.ServerConfig.Kafka.PartitionCnt,
		subShardCount:        subShardCount,
		isOfflineExpand:      cfg.ServerConfig.Kafka.IsOfflineExpand,
		workers:              make(map[int32]*worker),
		wg:                   wg,
		ctx:                  ctx,
		cancel:               cancel,
		locker:               lockerIns,
		retryQueueKey:        retryQueueKey,
		retryProcessingKey:   retryProcessingKey,
		retryDeadQueueKey:    retryDeadQueueKey,
		retryConsumeInterval: 1 * time.Second,
		retryMaxTimes:        3,
		retryOwner:           retryOwner,
		placement: &placementRouter{
			rc:       placementRedis,
			zone:     cfg.ZoneId,
			required: cfg.Placement.Required,
			stores:   stores,
		},
		handoff: handoff,
		now:     time.Now,

		pullStop:             make(chan struct{}),
		consumeDone:          make(chan struct{}),
		shutdownDrainTimeout: claimDrainTimeout,
		relayWork:            drainproof.New(),
		ready:                make(chan struct{}),

		retryRouting: retryRouting,
		// shared 模式也建 p 键:每拍兜底排空 p*(从 owner 回滚的路径)。
		retryPartitionKeys: retryPartitionReadyKeys(cfg.ServerConfig.Kafka.Topic, cfg.ServerConfig.Kafka.PartitionCnt),
		owned:              newOwnedPartitions(),
		stranded:           newStrandedRetries(),
		// 开启 WAITAOF 时建专用小客户端做组提交(不占主池);两项都写 0 时退回 directEnqueue 并打一行 ERROR。
		// 必须先 Start 里的 enqueuer().start(探针)才能用;Stop 的最后一步关闭。放在所有会失败返回的步骤之后,
		// 构造失败时不会留下没人关的专用客户端。外面包一层 confirmingEnqueue:任一请求确认成功就清零重新入组的退避
		// (rejoin_backoff.go)。
		durable:        &drainEnqueue{inner: confirmingEnqueue{durableEnqueue: newDurableEnqueue(cfg.ServerConfig.RedisClient, redisClient), backoff: durableBackoff}, pending: drainproof.New()},
		durableBackoff: durableBackoff,
	}
	// 读方门禁的两半:逐条选库 / 复核用的读(所有 worker 共用同一个 placementRouter),与进程级就绪闸。
	c.placement.ready = routeReader
	c.routeGate = routeGate
	// 落点选库与按需开库的计数器以 0 建出全部序列(封闭 label 集合):「出现一次就报」的告警用 increase(),序列从无到 1
	// 时会漏掉第一次(deploy/k8s/player-placement-alerts.yaml)。go/db 的消费者一律按落点选库,装配到这里即启用。
	metrics.PrimePlacement()
	for i := int32(0); i < cfg.ServerConfig.Kafka.PartitionCnt; i++ {
		c.workers[i] = c.buildWorker(i)
	}
	metrics.SetRetryRoutingMode(c.retryRouteMode())
	return c, nil
}

// buildWorker constructs (but does not start) the worker for one partition.
func (c *KeyOrderedKafkaConsumer) buildWorker(partition int32) *worker {
	shardChans := make([]chan *workerTask, c.subShardCount)
	for s := 0; s < c.subShardCount; s++ {
		// 256 buffer per sub-channel is enough: the router fans tasks
		// in, sub-workers drain via batch + processTaskBatch. If a
		// shard backs up, the router will block (which propagates
		// backpressure into Kafka — the desired behaviour).
		shardChans[s] = make(chan *workerTask, 256)
	}
	return &worker{
		partition:          partition,
		taskCh:             make(chan *workerTask, 1000),
		ctx:                c.ctx,
		redisClient:        c.redisClient,
		ownerEpochRedis:    c.ownerEpochRedis,
		locker:             c.locker,
		topic:              c.topic,
		retryQueueKey:      c.retryQueueKey,
		retryProcessingKey: c.retryProcessingKey,
		retryDeadQueueKey:  c.retryDeadQueueKey,
		wg:                 c.wg,
		retryRouting:       c.retryRouting,
		retryPartitionKeys: c.retryPartitionKeys,
		stranded:           c.stranded,
		durable:            c.durable,
		appliedEpochs:      redisAppliedEpochStore{rc: c.redisClient},
		placement:          c.placement,
		subShardCount:      c.subShardCount,
		subShardChans:      shardChans,
	}
}

// ensureWorker 返回分区契约内的 worker。合法分区理论上都在构造期创建；按需
// 创建只兜住内部生命周期竞态，绝不接受 broker 在线扩出来的新分区，因为那会
// 绕过 topic generation/cursor 的顺序边界。
func (c *KeyOrderedKafkaConsumer) ensureWorker(partition int32) (*worker, error) {
	if partition < 0 || partition >= c.partitionCount {
		return nil, fmt.Errorf("partition %d is outside immutable contract [0,%d) for topic %s",
			partition, c.partitionCount, c.topic)
	}
	c.workersMu.RLock()
	w, ok := c.workers[partition]
	c.workersMu.RUnlock()
	if ok {
		return w, nil
	}

	c.workersMu.Lock()
	defer c.workersMu.Unlock()
	if w, ok = c.workers[partition]; ok {
		return w, nil
	}
	w = c.buildWorker(partition)
	c.workers[partition] = w
	logx.Errorf("worker recreated on demand inside partition contract: topic=%s, partition=%d, configured=%d",
		c.topic, partition, c.partitionCount)
	if c.started {
		c.wg.Add(1)
		go w.start(c.isOfflineExpand)
	}
	return w, nil
}

func (c *KeyOrderedKafkaConsumer) Start() error {
	// 读方就绪闸(route_ready_gate.go):Placement.RequireRouteReady=true 时先试跑一次读脚本,执行出错即拒启;通过后按间隔
	// 刷新。开关关着时什么都不做。放在最前:拒启时还没有起任何 goroutine、没有占任何收据。
	if err := c.startRouteReadyGate(); err != nil {
		return fmt.Errorf("route ready gate: %w", err)
	}

	// 入队层先于一切消费:开启 WAITAOF 时这里发一次启动探针(appendonly no、Redis < 7.2、副本份数配错都是确定性的),
	// 失败即拒启,而不是留到运行期让每一次入队都 fail-closed、对应分区反复停滞;探针通过后才起组提交的写协程。
	// 写协程的生命周期不跟 c.ctx 走,只由 Stop 的最后一步关闭(durable_enqueue.go)。
	if err := c.enqueuer().start(c.ctx); err != nil {
		return fmt.Errorf("retry enqueue durability: %w", err)
	}

	// 同一个 zone 可以有多个 db 实例,所以启动时**不能**再把整个 processing 列表搬回 ready ——
	// 那会把别的实例正在处理的收据搬走重做。现在每个实例只管自己认领的收据:先收回同名实例(上一次的自己)
	// 遗留的、登记租约、回收一遍租约已过期的实例留下的孤儿,再开始消费(retry_ownership.go)。
	// Redis 不可用时与以前一样拒绝启动:重试队列不可用就不该开始消费。
	if c.retryOwner != nil {
		if err := c.retryOwner.start(c.ctx); err != nil {
			return fmt.Errorf("retry receipt ownership: %w", err)
		}
		c.retryOwnerDone = make(chan struct{})
		go func() {
			defer close(c.retryOwnerDone)
			// 续租与孤儿回收的周期取租约的三分之一:允许连续两拍失败而不丢租约。
			ticker := time.NewTicker(c.retryOwner.lease / 3)
			defer ticker.Stop()
			c.retryOwner.run(c.ctx, ticker.C)
		}()
	}

	c.workersMu.Lock()
	for _, w := range c.workers {
		c.wg.Add(1)
		go w.start(c.isOfflineExpand)
	}
	c.started = true
	c.workersMu.Unlock()

	c.StartRetryConsumer()

	// 交接形状下起守望:放行之后每 5s 看一次对方组,只凭正面证据让出(group_handoff.go)。
	if c.handoff != nil {
		c.handoffDone = make(chan struct{})
		go c.handoff.watch(c.ctx, c.handoffDone)
	}

	// 消费组错误通道(Consumer.Return.Errors=true):心跳、位点提交、claim 的错误计数并限速打 ERROR
	// (consumer_group_errors.go)。通道在 consumer.Close(Stop 第③步)时关闭,协程随之退出;替身返回 nil 通道时不起。
	if c.consumer != nil {
		c.startConsumerGroupErrorWatcher(c.consumer.Errors())
	}

	// 消费循环。退出时关闭 consumeDone:Stop 第②步等它,意味着最后一个 session 已经释放、位点已最后一次提交。
	consumeDone := c.consumeDone
	go func() {
		if consumeDone != nil {
			defer close(consumeDone)
		}
		// consumeCtx 随撤拉取(Stop 第①步)一起取消,交接闸等待与 Consume 都用它:
		//   - 交接闸前的等待不会把 Stop 卡住;
		//   - 没有分到分区的成员(副本数多于分区数)没有 claim 可返回,sarama 只在 ctx 取消或 Close 时结束它的
		//     session,不随 pullStop 取消的话 Stop 第②步要白等满超时。
		// 取消 Consume 的 ctx 会让 sarama 取消会话,但停机时 claim 的收尾不看会话 ctx(drainClaimForShutdown),
		// 已派发的任务照常做完;sarama 的心跳持续到会话释放、释放时最后一次提交位点,先提交、后离组的顺序不变。
		consumeCtx, cancelConsume := context.WithCancel(c.ctx)
		defer cancelConsume()
		go func() {
			select {
			case <-c.pullStop:
				cancelConsume()
			case <-consumeCtx.Done():
			}
		}()
		for !c.pullStopped() {
			// 交接闸在每次 Consume(入组)之前判定:被闸住时本进程根本不入组、不持有分配。已放行时立即返回,
			// 所以之后的再平衡不再依赖对方组;只有守望让出后才会重新等待。
			if c.handoff != nil {
				if err := c.handoff.await(consumeCtx); err != nil {
					logx.Infof("consumer group stopped while waiting for group handoff: groupID=%s peer=%s topic=%s",
						c.groupID, c.handoff.peer, c.topic)
					return
				}
			}
			if err := c.consumer.Consume(consumeCtx, []string{c.topic}, &consumerGroupHandler{consumer: c}); err != nil {
				logx.Errorf("consumer group consume failed: groupID=%s, topic=%s, err=%v", c.groupID, c.topic, err)
				select {
				case <-c.pullStop:
				case <-c.ctx.Done():
				case <-time.After(consumeRetryBackoff):
				}
			}
			if c.ctx.Err() != nil {
				break
			}
			// 本轮会话里有 claim 因入队未确认落盘而退出时,重新入组之前指数退避(rejoin_backoff.go):Consume 此时通常
			// 返回 nil,不走上面的 consumeRetryBackoff,否则毒消息之类确定性进死信的队头会每轮多推一份副本。撤拉取与
			// c.ctx 取消立即打断等待。
			c.waitRejoinBackoff()
		}
		logx.Infof("consumer group stopped: groupID=%s, topic=%s", c.groupID, c.topic)
	}()

	handoffFrom := ""
	if c.handoff != nil {
		handoffFrom = c.handoff.peer
	}
	logx.Infof("consumer started successfully: groupID=%s, handoffFrom=%s, topic=%s, partitionCount=%d, isOfflineExpand=%v, retryRouting=%s",
		c.groupID, handoffFrom, c.topic, c.partitionCount, c.isOfflineExpand, c.retryRouteMode())
	return nil
}

// StartRetryConsumer 起重试协程。协程纳入 c.wg(go 之前 Add):Stop 第④步的 wg.Wait 由构造保证它已经退出,
// 第⑤步的 releaseRetryOwnership 才归还收据 —— 否则它最后一次认领的脚本会先续租,把刚摘掉的实例重新登记回去,
// 收据留在一个没人回收的 processing 里。协程里每个阻塞点都带 c.ctx.Done 与 pullStop 两路(Redis 调用用 c.ctx;
// 派发 select 见 consumeOneRetryFrom / consumeOneOwnedRetry),所以 Wait 不会互等。
func (c *KeyOrderedKafkaConsumer) StartRetryConsumer() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(c.retryConsumeInterval)
		defer ticker.Stop()

		for {
			select {
			case <-c.ctx.Done():
				logx.Infof("retry consumer stopped: topic=%s, retryQueueKey=%s", c.topic, c.retryQueueKey)
				return
			case <-c.pullStop:
				// 停机撤拉取:不再认领新的重试收据;已派发的由 worker 做完,留在本实例 processing 里的由 Stop 第⑤步归还。
				logx.Infof("retry consumer stopped pulling for shutdown: topic=%s, retryQueueKey=%s", c.topic, c.retryQueueKey)
				return
			case <-ticker.C:
				c.consumeRetryQueue()
			}
		}
	}()
	logx.Infof("retry consumer started: topic=%s, interval=%v, maxRetryTimes=%d, mode=%s",
		c.topic, c.retryConsumeInterval, c.retryMaxTimes, c.retryRouteMode())
}

// claimRetryReceiptFrom 把一条重试收据从 readyKey(共享收件箱,或 owner 模式下的 p{P})原子地认领到**本实例的**
// processing 列表。ready 为空时返回 redis.Nil。认领之后进程崩溃,收据留在本实例的 processing 列表里:
// 同名实例重启时自己收回,否则等租约过期由别的实例回收进共享收件箱(retry_ownership.go)。
func (c *KeyOrderedKafkaConsumer) claimRetryReceiptFrom(readyKey string) ([]byte, error) {
	if c.retryOwner == nil {
		// 没有归属登记(只有直接拼结构体的单测走到这里):退回单纯的原子认领。
		return c.redisClient.RPopLPush(c.ctx, readyKey, c.retryProcessingKey).Bytes()
	}
	return c.retryOwner.claimFrom(c.ctx, readyKey)
}

// moveRetryReceiptScript atomically publishes the replacement payload before
// removing the exact processing receipt. If LPUSH fails (for example WRONGTYPE),
// the receipt remains in processing and is recoverable on restart.
const moveRetryReceiptScript = `
local found = false
local receipts = redis.call('LRANGE', KEYS[1], 0, -1)
for _, value in ipairs(receipts) do
	if value == ARGV[1] then
		found = true
		break
	end
end
if not found then
	return 0
end
redis.call('LPUSH', KEYS[2], ARGV[2])
redis.call('LREM', KEYS[1], 1, ARGV[1])
return 1`

// moveRetryReceipt 原子搬一条收据(moveRetryReceiptScript)。go-redis 主池缺省 MaxRetries=3,超时 / EOF 时会原样重发
// 这条 EVAL;重发不会丢收据:首发已执行时,重发要么找不到收据返回 0(调用方看到 moved=false),要么搬走 processing 里
// 另一份字节相同的副本、目的地多一份相同载荷 —— 只会多出重复执行,由排序锁与游标吸收。所以这里不加防重记录
// (对比 ackRetryReceipt 与 retryClaimScript)。
func moveRetryReceipt(ctx context.Context, rc redis.Cmdable, processingKey, destinationKey string, receipt, replacement []byte) (bool, error) {
	result, err := rc.Eval(ctx, moveRetryReceiptScript,
		[]string{processingKey, destinationKey}, receipt, replacement).Int64()
	return result == 1, err
}

// ackRetryReceiptScript 是按 nonce 防重的 LREM 1(R7 复核):KEYS[2] 不存在时执行 LREM 并把删掉的条数记进 KEYS[2]
// (ARGV[2] 毫秒),已存在时直接返回记下的条数、不再 LREM。
// 为什么要防重:主池的 go-redis 在超时 / EOF 时原样重发 LREM。processing 里有两份字节相同的副本(Kafka 重投、组提交
// 重推)而回包丢了时,重发会把另一份也删掉;processTaskBatch 合并掉同 seq 的重复副本、保留的那份随后失败时,
// deferFailedTask 已找不到任何一份收据,这笔存盘就丢了。残余:Redis 卡顿超过 retryGuardTTL,旧连接里迟到执行的副本
// 会再删一次。
// KEYS[1] = processing,KEYS[2] = 本次 ack 的防重记录;ARGV[1] = 收据,ARGV[2] = 记录毫秒数。
const ackRetryReceiptScript = `
local prev = redis.call('GET', KEYS[2])
if prev then
	return tonumber(prev)
end
local n = redis.call('LREM', KEYS[1], 1, ARGV[1])
redis.call('SET', KEYS[2], n, 'PX', ARGV[2])
return n`

// ackRetryReceipt 从 processing 里删掉一份收据(成功、判 stale、被合并、读用完次数等终态之后调用);没删到(收据已不在)
// 返回 error。每次调用一个新 nonce,go-redis 的内部重发因此幂等(见 ackRetryReceiptScript)。
func ackRetryReceipt(ctx context.Context, rc redis.Cmdable, processingKey string, receipt []byte) error {
	if len(receipt) == 0 {
		return nil
	}
	nonce, err := newRetryGuardNonce()
	if err != nil {
		return err
	}
	removed, err := rc.Eval(ctx, ackRetryReceiptScript,
		[]string{processingKey, retryGuardKey("ack", processingKey, nonce)}, receipt, retryGuardTTL.Milliseconds()).Int64()
	if err != nil {
		return err
	}
	if removed != 1 {
		return fmt.Errorf("retry receipt not found in processing queue")
	}
	return nil
}

// moveClaimedRetryToDead 把认领到的收据原子搬进死信,返回是否搬成。label 是 db_task_dead_letter_total 的 reason
// (metrics.DeadLetter*),reason 是写进日志的详细原因;onMoved(可为 nil)在搬成之后调用一次,调用方在这里补自己的计数。
// Redis 出错时登记进待重搬表(retry_stranded.go),之后重搬成功时照样计死信与 onMoved;此时返回 false。
func (c *KeyOrderedKafkaConsumer) moveClaimedRetryToDead(receipt, payload []byte, label, reason string, onMoved func()) bool {
	counted := func() {
		metrics.CountTaskDeadLetter(label)
		if onMoved != nil {
			onMoved()
		}
	}
	moved, err := moveRetryReceipt(c.ctx, c.redisClient, c.retryProcessingKey, c.retryDeadQueueKey, receipt, payload)
	if err != nil || !moved {
		if err != nil && c.ctx.Err() == nil && c.stranded.add(strandedRetry{
			processingKey: c.retryProcessingKey, dest: c.retryDeadQueueKey, receipt: receipt, replacement: payload,
			what: "dead_letter " + label, onMoved: counted,
		}) {
			logx.Errorf("retry dead-letter failed; queued for a re-move on the next retry tick: topic=%s reason=%s err=%v",
				c.topic, reason, err)
			return false
		}
		logx.Errorf("retry dead-letter failed; receipt remains recoverable in processing: topic=%s reason=%s moved=%v err=%v",
			c.topic, reason, moved, err)
		return false
	}
	counted()
	logx.Errorf("retry task moved to dead queue: topic=%s reason=%s deadQueue=%s", c.topic, reason, c.retryDeadQueueKey)
	return true
}

// consumeRetryQueue 每个 tick 排空重试队列,直到队列空、达到本轮预算或进程退出。
//
// 旧实现每个 tick 只 RPopLPush 一条,而 ticker 是 1 秒 —— 重试队列的排空上限
// 被硬钉在 1 条/秒。稳态入流是 PartitionCnt × SubShardCount 量级(压测记录 ~90/s),
// 于是 MySQL 抖一下产生的失败批次根本追不平:队列只涨不落,玩家存盘卡在里面,
// 而 retryMaxTimes=3 又会把超限的任务推进 kafka:dead:queue:* —— 那个 key 全仓
// 没有任何消费者,等于静默丢盘。
// 单条 claim 的原子性语义不变,只是把它放进有界循环里。
//
// L-04(docs/design/db-task-store-unavailable-deferral.md §3.5)加了两条:
//   - 本拍最多认领「拍开始时 ready 的长度」与 200 中较小的那个数。搬回 ready 是 LPUSH 到队头、认领从队尾取,
//     原来一条任务可以在同一拍里被反复认领,3 次重试最短可以在 1 秒内用完,被豁免的任务则没有任何最小间隔。
//     现在每实例每拍对每条至多认领一次;R 个实例共用 ready 时,3 次预算最短约 3/R 秒。
//   - 本 zone 主库已被探活确认不可达(冷却中)时整拍跳过:此时每条任务都只会在选库处被挡回,认领只是白花
//     Redis 往返。代价是主库整个不可用期间,落到其它库的重试任务也不排空。
//
// S1 ④(retry_routing.go)按 RetryRouting 分两路,都先采样一次各列表长度(sampleRetryQueues):
//   - owner:consumeRetryQueueOwner —— 先把共享收件箱分拣进 p{N}(冷却期照做),再只对本进程存活的 claim 租约认领;
//   - shared(含零值):consumeRetryQueueShared —— 上面的旧语义,另外兜底排空 p*。
//
// 两路之前各有一步(R7 复核):先重搬待重搬表里的收据(retry_stranded.go),再处理结果未知的认领
// (retryOwnership.resolveUnresolvedClaims);后者没处理完(Redis 仍出错)时本拍到此为止,不再分拣、认领。
func (c *KeyOrderedKafkaConsumer) consumeRetryQueue() {
	c.retryStrandedReceipts()
	if !c.retryOwner.resolveUnresolvedClaims(c.ctx) {
		return
	}
	s := c.sampleRetryQueues()
	// 读方就绪闸关着(route_ready_gate.go,D1 第 3 批):本拍不分拣、不认领 —— 认领出来的任务逐条判也只会不可信、再搬回去,
	// 读还要白耗预算。收据留在原列表,下一拍(retryConsumeInterval)再看。上面两步只是收尾已认领的收据,照做;
	// 长度采样照做,队列指标不断档。闸为 nil(开关关着)时恒放行。闸在本拍中途关上时,两路的认领循环每认领一条之前
	// 再看一次(opened,不记等待次数),当拍至多多认领正在认领的那一条,而不是把预算用完。
	if c.retryClaimsPaused() {
		return
	}
	if c.ownerRouting() {
		c.consumeRetryQueueOwner(s)
		return
	}
	c.consumeRetryQueueShared(s)
}

// consumeRetryQueueShared 是 shared 模式的一拍:主库冷却中整拍跳过;否则依次从共享收件箱与拍开始时非空的 p{N}
// 认领,每拍总预算 retryDrainBudgetPerTick。每个来源的额度 = 拍开始时它的长度(读失败取总预算),所以同一条收据
// 一拍里至多被本实例认领一次(L-04 §3.5 的口径不变)。某个来源认领为空或出错就换下一个来源。
//
// 兜底排空 p*(从 owner 回滚的路径):shared 实例也认领 p{N} 里的收据。与 owner 实例混跑也安全 —— 执行仍在排序锁、
// 游标与 epoch 守卫之下;本实例没有原始分区的 claim 时计 executed_nonowner(见 consumeOneRetryFrom)。
func (c *KeyOrderedKafkaConsumer) consumeRetryQueueShared(s retryQueueSample) {
	if c.homeStoreCoolingDown() {
		return
	}
	type retrySource struct {
		readyKey string
		quota    int64
	}
	sources := []retrySource{{readyKey: c.retryQueueKey, quota: s.shared}}
	for i, key := range c.retryPartitionKeys {
		if i < len(s.partitioned) && s.partitioned[i] != 0 {
			sources = append(sources, retrySource{readyKey: key, quota: s.partitioned[i]})
		}
	}

	budget := retryDrainBudgetPerTick
	for _, src := range sources {
		quota := src.quota
		if quota < 0 || quota > int64(budget) {
			quota = int64(budget) // 长度读失败(-1)时退回固定上限
		}
		for ; quota > 0 && budget > 0; quota-- {
			if c.ctx.Err() != nil || c.pullStopped() {
				return // 停机撤拉取之后不再认领(Stop 第①步)
			}
			if !c.routeGate.opened() {
				return // 读方就绪闸在本拍中途关上:剩下的收据留在原列表(nil 闸恒放行,开关关着时不走这里)
			}
			if !c.consumeOneRetryFrom(src.readyKey) {
				break
			}
			budget--
		}
		if budget == 0 {
			logx.Infof("retry drain budget reached this tick: topic=%s budget=%d readyQueue=%s",
				c.topic, retryDrainBudgetPerTick, src.readyKey)
			return
		}
	}
}

// consumeOneRetryTask 认领并派发共享收件箱里的一条重试(shared 模式的原入口,单测直接调用)。
func (c *KeyOrderedKafkaConsumer) consumeOneRetryTask() bool {
	return c.consumeOneRetryFrom(c.retryQueueKey)
}

// consumeOneRetryFrom 从 readyKey(共享收件箱或某个 p{N})认领一条重试并派给原始分区的 worker(shared 模式)。
// 返回 false 表示这个来源本拍不再认领(列表已空、认领失败、停机);认领后已终态处理(死信 / 读丢弃)的返回 true。
//
// shared 重试不带 claimCtx、没有在途回执(今天的语义):执行与否由排序锁、游标与 epoch 守卫兜住。本实例没有原始分区的
// claim 时计 executed_nonowner —— zone 里已有 owner 实例时还出现它,说明有 shared 实例混跑
// (告警 DbRetryNonOwnerExecutionInOwnerZone)。
//
// 派发 select 与 ConsumeClaim、consumeOneOwnedRetry 同一写法:c.ctx 取消与撤拉取都不再派发、不按失败处理,
// 收据已认领进本实例的 processing 列表,由 Stop 第⑤步(releaseRetryOwnership)原样还回共享收件箱,不丢、不耗重试次数。
func (c *KeyOrderedKafkaConsumer) consumeOneRetryFrom(readyKey string) bool {
	// Atomic ready -> processing claim (see claimRetryReceiptFrom for what happens to the receipt after a crash).
	msgBytes, err := c.claimRetryReceiptFrom(readyKey)
	if err != nil {
		if !errors.Is(err, redis.Nil) && c.ctx.Err() == nil {
			logx.Errorf("claim retry queue failed: ready=%s processing=%s err=%v",
				readyKey, c.retryProcessingKey, err)
		}
		return false
	}

	mode := c.retryRouteMode()
	task, seq, partition, w, ok := c.parseClaimedRetry(msgBytes, mode)
	if !ok {
		return true
	}
	task.RetryCount++

	action := metrics.RetryRouteExecuted
	if c.owned.lookup(partition) == nil {
		action = metrics.RetryRouteExecutedNonOwner
	}
	retryTask := &workerTask{
		dbTask:             task,
		seq:                seq,
		originPartition:    partition,
		hasOriginPartition: true,
		fromRetry:          true,
		retryReceipt:       append([]byte(nil), msgBytes...),
		enqueuedAt:         time.Now(),
	}
	c.trackTask(retryTask)
	select {
	case w.taskCh <- retryTask:
		metrics.CountRetryRoute(mode, action)
		logx.Debugf("retry task routed to worker: taskID=%s, partition=%d, retryCount=%d, action=%s",
			task.TaskId, partition, task.RetryCount, action)
	case <-c.ctx.Done():
		retryTask.finishRetry()
		return false
	case <-c.pullStop:
		retryTask.finishRetry()
		// 停机撤拉取时 worker 通道还满着:不再派发,收据留在本实例 processing,由 Stop 第⑤步归还。
		return false
	}
	return true
}

// parseClaimedRetry 解析一条刚认领进本实例 processing 的收据,决定它能不能派发(shared 与 owner 两条路径共用)。
//
// ok=false 表示已经终态处理,调用方不得再碰这条收据:畸形载荷、写用完次数、没有可信的原始分区、分区越界 → 原字节进死信;
// 读用完次数 → 丢弃(读不进死信,L-04 §3.6)。终态搬移成功时计 db_retry_route_total{mode, action=dead}。
// ok=true 时返回解出的任务(**RetryCount 未加**,由调用方在派发前加一)、seq、原始分区与该分区的 worker。
func (c *KeyOrderedKafkaConsumer) parseClaimedRetry(msg []byte, mode string) (*db_proto.DBTask, uint64, int32, *worker, bool) {
	deadLetter := func(label, reason string) {
		// 路由计数放进 onMoved:当场搬成与之后从待重搬表搬成都计一次。
		c.moveClaimedRetryToDead(msg, msg, label, reason, func() { metrics.CountRetryRoute(mode, metrics.RetryRouteDead) })
	}

	seq, partition, hasPartition, taskBytes := unwrapRetryPayload(msg)
	var task db_proto.DBTask
	if err := proto.Unmarshal(taskBytes, &task); err != nil {
		deadLetter(metrics.DeadLetterMalformedPayload, fmt.Sprintf("malformed payload: %v", err))
		return nil, 0, 0, nil, false
	}
	if _, ok := dbOpHandlers[task.Op]; !ok {
		deadLetter(metrics.DeadLetterMalformedPayload, fmt.Sprintf("unsupported op: %d", task.Op))
		return nil, 0, 0, nil, false
	}

	if task.RetryCount >= int32(c.retryMaxTimes) {
		if task.Op == db_proto.DBOp_DB_OP_READ {
			// 读不改数据,进死信没有可保全的东西,只会让死信告警失去判别力(L-04 §3.6)。
			if c.abandonClaimedReadRetry(msg, &task) {
				metrics.CountRetryRoute(mode, metrics.RetryRouteDead)
			}
			return nil, 0, 0, nil, false
		}
		deadLetter(metrics.DeadLetterBudgetExhausted,
			fmt.Sprintf("max retries exceeded taskID=%s retryCount=%d", task.TaskId, task.RetryCount))
		return nil, 0, 0, nil, false
	}
	if !hasPartition {
		// v1/raw 遗留载荷没有可信的来源分区。Key%N 不是生产端的分区函数;按契约重算也只能得到
		// 应在的分区,证明不了原消息实际在的分区。按猜测派发会与新写竞争或回退新写,保留载荷交运维修复。
		deadLetter(metrics.DeadLetterLegacyPayload,
			fmt.Sprintf("ORDERING: legacy retry has no proven origin partition taskID=%s key=%d", task.TaskId, task.Key))
		return nil, 0, 0, nil, false
	}

	// 重试必须路由回**原始 Kafka 分区**的 worker:同 key 的新消息在生产端按分区契约
	// (kafkautil.DBTaskPartition)落到的分区上串行,重试只有回到同一个 worker 才不会和新写
	// 并发。旧格式没有原始分区,已在上方 fail-closed 隔离,不再用 Key%N 猜测。
	if partition < 0 {
		deadLetter(metrics.DeadLetterInvalidPartition,
			fmt.Sprintf("invalid origin partition=%d taskID=%s", partition, task.TaskId))
		return nil, 0, 0, nil, false
	}
	w, err := c.ensureWorker(partition)
	if err != nil {
		deadLetter(metrics.DeadLetterInvalidPartition,
			fmt.Sprintf("ORDERING: retry partition outside immutable contract: %v taskID=%s", err, task.TaskId))
		return nil, 0, 0, nil, false
	}
	return &task, seq, partition, w, true
}

func (w *worker) start(isOfflineExpand bool) {
	defer func() {
		w.wg.Done()
		logx.Infof("worker stopped: partition=%d, topic=%s", w.partition, w.topic)
	}()

	logx.Infof("worker started: partition=%d, topic=%s, isOfflineExpand=%v, subShards=%d",
		w.partition, w.topic, isOfflineExpand, w.subShardCount)

	// Launch one sub-worker goroutine per sub-shard. Each sub-worker
	// drains its own channel and does its own batch coalesce, so per-key
	// ordering is preserved (same key.hash → same shard → same goroutine).
	//
	// We use an inner WaitGroup so this function only returns once every
	// sub-worker has fully drained on shutdown; otherwise the outer wg
	// would mark the worker done while sub-workers are still touching
	// MySQL/Redis.
	var subWg sync.WaitGroup
	for s := 0; s < w.subShardCount; s++ {
		subWg.Add(1)
		go w.runSubShard(s, isOfflineExpand, &subWg)
	}

	// 路由循环:从 w.taskCh 取任务,按 spreadKey(task.Key) % subShardCount 分发到子 shard(见 subShardIndex)。
	// 停机时关闭所有子 channel,让子 worker 退出;子 worker 全部停下之后,通道里没做的任务交还 owner 重试的在途回执
	// (discardQueuedRetries),否则 claim 排空要等满时限。
	for {
		select {
		case <-w.ctx.Done():
			logx.Infof("worker received stop signal: partition=%d", w.partition)
			for _, ch := range w.subShardChans {
				close(ch)
			}
			subWg.Wait()
			w.discardQueuedRetries()
			return
		case task, ok := <-w.taskCh:
			if !ok {
				logx.Infof("worker task channel closed: partition=%d", w.partition)
				for _, ch := range w.subShardChans {
					close(ch)
				}
				subWg.Wait()
				w.discardQueuedRetries()
				return
			}
			w.routeToSubShard(task)
		}
	}
}

// routeToSubShard picks the destination sub-shard for one task.
//
// We pre-extract the routing key (dbTask.Key for retry tasks, or the
// Key/MsgType pair encoded into the kafka payload for fresh tasks).
// Same routing key always lands in the same sub-shard, so two tasks
// for the same player can never run concurrently in different goroutines,
// preserving read-after-write semantics.
//
// 这只覆盖进了同一个 worker 的任务:同 key 的读只有与写落在同一分区时,读屏障才成立;前提与剩余缺口见
// processTaskBatch 的「读屏障的前提」与 db-task-kafka-partition-contract.md。
func (w *worker) routeToSubShard(task *workerTask) {
	if w.subShardCount <= 1 {
		// Legacy: only one sub-shard, no routing needed.
		select {
		case w.subShardChans[0] <- task:
		case <-w.ctx.Done():
			task.finishRetry()
		}
		return
	}

	routingKey := uint64(0)
	switch {
	case task.dbTask != nil:
		routingKey = task.dbTask.Key
	case task.kafkaMsg != nil:
		// Decode just enough to read Key. We pay the cost twice (here +
		// in handleTask) for fresh kafka tasks, but only once for retry
		// tasks. Acceptable; the alternative (storing parsed dbTask on
		// workerTask) churns more allocator pressure.
		var t db_proto.DBTask
		if err := proto.Unmarshal(task.kafkaMsg.Value, &t); err == nil {
			routingKey = t.Key
		} else {
			logx.Errorf("routeToSubShard: failed to peek Key, partition=%d, offset=%d: %v",
				w.partition, task.kafkaMsg.Offset, err)
			// Fall through with routingKey=0; the handleTask path will
			// durably quarantine the malformed message before ACK.
		}
	}

	shard := subShardIndex(routingKey, w.subShardCount)
	select {
	case w.subShardChans[shard] <- task:
	case <-w.ctx.Done():
		// Don't deadlock on shutdown if a shard is already gone.
		// 丢弃这条:owner 重试交还在途回执(收据留在本实例 processing,由 Stop 第⑤步归还);Kafka 任务没有回执,
		// 不标记的位点由下一任属主重放。
		task.finishRetry()
	}
}

// subShardIndex 把路由 key 映射到子 shard,取模前先打散。
//
// 存量 PlayerId 是 bwmarrin 布局(41 位毫秒 | 13 位 node | 9 位 step),低 9 位是毫秒内序号、
// 每个新毫秒归零,低频建角时几乎全为 0。直接 key % 8 会让这批玩家全挤进 0 号子 shard,
// 分区内并行形同虚设(2026-09-08 改号段之后新建的角色不受影响)。
//
// 同一 key 恒落同一子 shard 的保证不变。子 shard 只是进程内划分,跨实例的同 key 串行靠
// 分区独占 + 排序锁,所以滚动发布时新旧实例打散方式不同不影响正确性。
func subShardIndex(routingKey uint64, shardCount int) int {
	return int(spreadKey(routingKey) % uint64(shardCount))
}

// spreadKey 是 splitmix64 的终混步,算法与常量同 C++ 的 SpreadPlayerId
// (cpp/libs/engine/core/network/node_utils.cpp),两边要改一起改。
func spreadKey(key uint64) uint64 {
	z := key + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// runSubShard is the per-sub-channel drain + batch-coalesce loop. This
// is what used to be the body of worker.start before the sub-shard
// refactor; logic inside is unchanged so per-key ordering and the
// processTaskBatch coalescer keep working identically.
func (w *worker) runSubShard(shardIdx int, isOfflineExpand bool, subWg *sync.WaitGroup) {
	defer subWg.Done()
	ch := w.subShardChans[shardIdx]

	for {
		select {
		case <-w.ctx.Done():
			return
		case task, ok := <-ch:
			if !ok {
				return
			}

			batch := []*workerTask{task}
		drainLoop:
			for {
				select {
				case t, ok := <-ch:
					if !ok {
						break drainLoop
					}
					batch = append(batch, t)
				default:
					break drainLoop
				}
			}

			metrics.ObserveSubShardBatch(len(batch))
			w.processTaskBatch(batch, isOfflineExpand)
		}
	}
}

// writeCoalesceKey identifies a write that can be coalesced: same player + same table.
type writeCoalesceKey struct {
	key     uint64
	msgType string
}

// processTaskBatch coalesces consecutive writes for the same (key, msg_type),
// keeping only the latest body of each "write segment". A read task acts as
// a barrier: writes that precede a read for the same (key, msg_type) are NOT
// coalesced into writes that follow it, because the read must observe the
// post-write state in MySQL (read-after-write consistency within the same
// per-key Kafka partition).
//
// Every safely completed Kafka message (including superseded ones) is handed
// to claimAcker; Sarama only receives the contiguous completed prefix.
//
// 读屏障的前提(docs/design/db-task-kafka-partition-contract.md「屏障覆盖范围与剩余缺口」):
//   - 两侧生产者都守分区契约(kafkautil.DBTaskPartition:C++ scene 的写与 login 的读同一个函数);
//   - 两侧 topic 名相同(login 的 DbTaskTopicForGeneration(登录 zone) == C++ 的 GetDbTaskTopic(home_zone));
//   - 读发出时,本玩家的写已经进了 Kafka(旧会话还在断线租约内未存盘、或写还滞留在 librdkafka 本地队列时,
//     读会排在写之前)。
//
// 满足前提时,同一玩家的读与写进同一 worker、同一子 shard,按 offset 串行。以下情形不在屏障覆盖范围内:
//  1. 访客或合服后跨 topic:读走登录 zone 的 topic,写走 home_zone 的 topic。
//  2. 写已转进 Redis 重试队列,读不会等它。
//  3. 重试被非属主实例执行。
//  4. 父键缺失但子表缓存还在(login ensure_player_all_data_async):命中的子表直接用缓存,不发读;
//     部分命中时拼出来的是新旧混合档。
//  5. 再平衡排空超时后旧实例仍在执行的写(G1)。
//
// 放大效应:publishDBReadResult 会把读结果回写子表缓存({MsgType}:{key},TTL 为 cacheTTL,默认 24h),
// 上面 1/2/3/5 里读到的旧值因此会延续到缓存过期,并经情形 4 在后续冷登录里被直接复用。
// 触发条件:父键全量丢失、FLUSH、运维删键会走冷读,屏障有效;AOF 部分回退时父键回到旧值、
// 走 EXISTS 快路径不发读,与屏障无关,属于 Redis 持久化的范畴。
func (w *worker) processTaskBatch(batch []*workerTask, isOfflineExpand bool) {
	defer func() {
		for _, task := range batch {
			task.finishRetry()
		}
	}()
	if len(batch) == 1 {
		w.handleTask(batch[0], isOfflineExpand)
		return
	}

	type parsedTask struct {
		wt     *workerTask
		dbTask *db_proto.DBTask
	}
	parsed := make([]parsedTask, 0, len(batch))

	// Pass 1 (forward): unmarshal kafka payloads once per task.
	for _, wt := range batch {
		var task *db_proto.DBTask
		if wt.dbTask != nil {
			task = wt.dbTask
		} else if wt.kafkaMsg != nil {
			var t db_proto.DBTask
			if err := proto.Unmarshal(wt.kafkaMsg.Value, &t); err != nil {
				logx.Errorf("coalesce: unmarshal failed, partition=%d, offset=%d, err=%v",
					w.partition, wt.kafkaMsg.Offset, err)
				w.quarantinePoisonTask(wt, err)
				continue
			}
			task = &t
			wt.dbTask = task
		}
		parsed = append(parsed, parsedTask{wt: wt, dbTask: task})
	}

	// Pass 2 (reverse): mark writes that are SUPERSEDED by a causally newer
	// write in the SAME segment (no intervening read for the same
	// key+msgType).
	//
	// Arrival order is not a version order here: a delayed retry can enter the
	// sub-shard after a fresh Kafka delivery. We may coalesce only tasks whose
	// origin partition is known and equal, then keep the greatest offset+1.
	// Different partitions are incomparable without a migration epoch, and
	// seq=0 is the legacy "unknown" sentinel, so those writes must reach the
	// normal ordering guard rather than being discarded by this optimization.
	superseded := make([]bool, len(parsed))
	liveBestWrite := make(map[writeCoalesceKey]map[int32]int)
	for i := len(parsed) - 1; i >= 0; i-- {
		pt := parsed[i]
		if pt.dbTask == nil {
			continue
		}
		ck := writeCoalesceKey{key: pt.dbTask.Key, msgType: pt.dbTask.MsgType}
		switch pt.dbTask.Op {
		case db_proto.DBOp_DB_OP_WRITE:
			if pt.wt.seq == 0 || !pt.wt.hasOriginPartition {
				continue
			}
			byPartition := liveBestWrite[ck]
			if byPartition == nil {
				byPartition = make(map[int32]int)
				liveBestWrite[ck] = byPartition
			}
			if bestIndex, exists := byPartition[pt.wt.originPartition]; exists {
				if ownerEpochsDiverge(pt.dbTask.OwnerEpoch, parsed[bestIndex].dbTask.OwnerEpoch) {
					// 归属期不同的写不能按 offset 互相替代:被废黜节点的迟到写可以
					// 有更大的 offset,按 offset 合并会把新主的写标成 superseded 并
					// ACK 掉,随后老写又被 guardOwnerEpoch 丢弃 —— 新主这一版就从
					// MySQL 消失了。把 epoch 变化当成与 read 同级的段边界:两边各自
					// 的最新写都保留,都走 handleTask 的归属守卫,由它决定谁落库、
					// 谁被计数拒绝。代价只是这一批少合并一次,而这种批次本身就是
					// 归属变更的异常窗口。
					byPartition[pt.wt.originPartition] = i
					continue
				}
				best := parsed[bestIndex].wt
				preferBest := pt.wt.seq <= best.seq
				if fencingDual.Load() && pt.dbTask.OwnerEpoch != 0 && pt.dbTask.SaveSeq != 0 && parsed[bestIndex].dbTask.SaveSeq != 0 && pt.dbTask.OwnerEpoch == parsed[bestIndex].dbTask.OwnerEpoch {
					mt, findErr := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(pt.dbTask.MsgType))
					if findErr == nil && isFenceTable(mt.Descriptor()) {
						preferBest = pt.dbTask.SaveSeq <= parsed[bestIndex].dbTask.SaveSeq
					}
				}
				if preferBest {
					// Equal versions are duplicate deliveries; retain the later
					// arrival so the forward execution order stays stable.
					superseded[i] = true
				} else {
					// A higher version happened to arrive earlier in this batch.
					// Replace the delayed lower-version candidate.
					superseded[bestIndex] = true
					byPartition[pt.wt.originPartition] = i
				}
			} else {
				byPartition[pt.wt.originPartition] = i
			}
		case db_proto.DBOp_DB_OP_READ:
			// Read acts as a barrier: any earlier write for ck is in a
			// different segment and must execute so the read sees post-write
			// state.
			delete(liveBestWrite, ck)
		default:
			// 未知操作由 handleTask 持久隔离；不能跨过它合并写，保留原始执行边界。
			delete(liveBestWrite, ck)
		}
	}

	// Pass 3 (forward): execute or skip.
	skipped := 0
	for i, pt := range parsed {
		if superseded[i] {
			skipped++
			logx.Debugf("coalesce: skipping superseded write: workerPartition=%d, originPartition=%d, seq=%d, key=%d, msgType=%s, taskID=%s",
				w.partition, pt.wt.originPartition, pt.wt.seq, pt.dbTask.Key, pt.dbTask.MsgType, pt.dbTask.TaskId)
			if pt.wt.fromRetry {
				if err := ackRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, pt.wt.retryReceipt); err != nil {
					// 保留执行的那个更高(或相同)版本才是权威。收据留在 processing 里可以恢复:启动时被收回重放,
					// 前提是游标没有随 Redis 回退,才会被游标判 stale 丢弃。游标与收据 ack 都是主池上的普通写,
					// 不受 WAITAOF 覆盖;AOF 尾巴丢失时这份旧写可能被重放、把 MySQL 覆盖回旧档,这一残余只能由
					// MySQL 条件写封死(durable_enqueue.go 文件头)。
					logx.Errorf("ack superseded retry receipt failed; it remains recoverable: taskID=%s err=%v",
						pt.dbTask.TaskId, err)
				}
			}
			// 被合并掉的 owner 重试不进 handleTask:在这里交还在途回执(handleTask 的 defer 覆盖不到它)。
			pt.wt.finishRetry()
			ackKafkaTask(pt.wt)
			continue
		}
		w.handleTask(pt.wt, isOfflineExpand)
	}

	if skipped > 0 {
		logx.Infof("coalesce: partition=%d, batch=%d, skipped=%d writes", w.partition, len(batch), skipped)
	}
}

func (w *worker) handleTask(task *workerTask, isOfflineExpand bool) {
	// owner 重试的在途回执(claim 租约的 exit)在任务走到终态时交还。最先注册、最后执行:panic 恢复里的
	// deferFailedTask 把收据搬回 p{origin} 之后才归零 inflight,claim 排空等到的是真正的终态。Kafka 任务是空操作。
	if task != nil {
		defer task.finishRetry()
	}
	taskStart := time.Now()
	// Per-message panic recovery: a single bad message won't kill the partition worker
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("worker panic: partition=%d panic=%v", w.partition, r)
			logx.Errorf("%v, stack=%s", err, string(debug.Stack()))
			// Preserve a durable copy just like any other processing failure. For
			// retry-origin work this atomically returns the processing receipt to
			// ready; leaving it in processing would stall forever because this
			// per-message recover keeps the process alive (startup recovery would
			// never run). Kafka ACK still happens only if enqueue succeeds.
			if task != nil && task.dbTask != nil {
				w.deferFailedTask(task, task.dbTask, err)
				return
			}
			failKafkaTask(task, err)
		}
	}()
	if task == nil {
		return
	}
	// 注册在 recover 之后:先于 recover 执行,panic 的任务也记一次整段耗时。
	defer observeTaskTiming(task, taskStart)
	if !task.enqueuedAt.IsZero() {
		metrics.ObserveStage(metrics.StageQueueWait, taskOpLabel(task.dbTask), taskStart.Sub(task.enqueuedAt))
	}
	if task.claimCtx != nil {
		select {
		case <-task.claimCtx.Done():
			if task.fromRetry {
				// owner 重试的 claim 已撤销(第一次复核):不执行、不耗预算,收据原字节搬回 p{origin} 交新属主。
				// failKafkaTask 对重试是空操作,只调它会让收据永远卡在本实例 processing。
				w.requeueRevokedRetry(task, "claim revoked before execution")
				return
			}
			failKafkaTask(task, fmt.Errorf("claim canceled before task execution: %w", task.claimCtx.Err()))
			return
		default:
		}
	}

	// Resolve dbTask up front (handles both Kafka-origin and retry-origin
	// tasks). We need dbTask available for both the seq guard AND the retry
	// queue save path on failure — historically Kafka-origin failures were
	// silently dropped here (data loss).
	var dbTask *db_proto.DBTask
	if task.dbTask != nil {
		dbTask = task.dbTask
	} else if task.kafkaMsg != nil {
		var t db_proto.DBTask
		if err := proto.Unmarshal(task.kafkaMsg.Value, &t); err != nil {
			logx.Errorf("worker unmarshal kafka task failed: partition=%d, offset=%d, err=%v",
				w.partition, task.kafkaMsg.Offset, err)
			w.quarantinePoisonTask(task, err)
			return
		}
		dbTask = &t
	} else {
		return
	}
	// Keep the decoded task on the envelope so panic recovery can persist the
	// exact operation instead of leaving a retry receipt stranded.
	task.dbTask = dbTask
	if _, ok := dbOpHandlers[dbTask.Op]; !ok {
		cause := fmt.Errorf("unsupported op: %d", dbTask.Op)
		// 旧字符串载荷按新协议解码后 Op 为零，也必须先保全原始载荷再确认。
		if task.fromRetry {
			w.quarantineOrderingConflict(task, dbTask, metrics.DeadLetterMalformedPayload, cause.Error())
		} else {
			w.quarantinePoisonTask(task, cause)
		}
		return
	}

	var orderingLock *orderingLease
	if dbTask.Op == db_proto.DBOp_DB_OP_WRITE {
		var lockErr error
		orderingLock, lockErr = acquireOrderingLock(w.ctx, w, dbTask)
		if lockErr != nil {
			w.deferFailedTask(task, dbTask, lockErr)
			return
		}
		defer releaseOrderingLock(w.ctx, orderingLock, dbTask)
	}

	decision, orderingErr := orderingForTask(w.ctx, w.redisClient, w.topic, dbTask, task)
	if orderingErr != nil {
		w.deferFailedTask(task, dbTask, fmt.Errorf("ordering guard unavailable: %w", orderingErr))
		return
	}
	epochOverride := false
	switch decision {
	case orderingStale:
		if fencingDual.Load() && fencingEpochOverride.Load() && dbTask.OwnerEpoch != 0 && dbTask.SaveSeq != 0 {
			mt, findErr := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(dbTask.MsgType))
			if findErr == nil && isFenceTable(mt.Descriptor()) {
				verdict, epochErr := checkOwnerEpoch(w.ctx, w.appliedEpochs, w.topic, dbTask)
				if epochErr != nil {
					w.deferFailedTask(task, dbTask, fmt.Errorf("%w: epoch override: %w", errFenceRedis, epochErr))
					return
				}
				if verdict.outcome == metrics.OwnerEpochAdvance {
					epochOverride = true
					metrics.CountOrderingGuard(metrics.OrderingEpochOverride)
				}
				if verdict.outcome == metrics.OwnerEpochFirst {
					metrics.CountOrderingGuard(metrics.OrderingStaleNoEpoch)
				}
			}
		}
		if epochOverride {
			break
		}
		logx.Infof("cursor guard: dropping stale write: workerPartition=%d originPartition=%d key=%d msgType=%s seq=%d taskID=%s",
			w.partition, task.originPartition, dbTask.Key, dbTask.MsgType, task.seq, dbTask.TaskId)
		if task.fromRetry {
			if err := ackRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, task.retryReceipt); err != nil {
				logx.Errorf("ack stale retry receipt failed; it remains recoverable: taskID=%s err=%v", dbTask.TaskId, err)
			}
		}
		ackKafkaTask(task)
		return
	case orderingIncomparable:
		w.quarantineOrderingConflict(task, dbTask, metrics.DeadLetterOrderingIncomparable,
			"origin partition cannot be safely compared with the existing applied cursor")
		return
	}

	// 归属 epoch 守卫:seq 只证明「同分区内更晚到」,不证明「来自当前归属节点」。
	// 只对 write 生效;read 不改数据。判定与终态处理都在 guardOwnerEpoch 里。
	if dbTask.Op == db_proto.DBOp_DB_OP_WRITE && !w.guardOwnerEpoch(task, dbTask) {
		return
	}

	// 按玩家落点选库(player-storage-placement.md §6.2):写在上面三道守卫全部通过之后才选,
	// 选中的库连同选库时的快照交给执行路径,SQL 之后由 runDBOp 复核(§6.3)。
	var route *placementRoute
	if dbTask.Op == db_proto.DBOp_DB_OP_WRITE || dbTask.Op == db_proto.DBOp_DB_OP_READ {
		var routed bool
		if route, routed = w.routeTask(task, dbTask); !routed {
			return
		}
	}

	// owner 重试在 SQL 之前第二次复核 claim(排队、等排序锁、选库期间可能已再平衡):撤销则原样搬回 p{origin}、不执行;
	// 已拿到的排序锁由上面的 defer 释放。过了这一点就不再看 claim:见下面 processCtx 的注释。
	if task.fromRetry && task.claimCtx != nil && task.claimCtx.Err() != nil {
		w.requeueRevokedRetry(task, "claim revoked before SQL")
		return
	}

	startTime := time.Now()
	// Kafka 任务的 SQL 半程随 claim 撤销取消(排空更快,位点不提交,由新属主重放)。重试不从 claimCtx 派生:SQL 与其后的
	// Redis 步骤(复核、缓存发布、游标)用 w.ctx —— 已经开始的重试持锁跑完,比半途撤掉、留一份「SQL 已提交而游标没推进」
	// 的状态更安全;重叠窗口由排序锁兜底。handler 已接 ctx(写的 SQL 用排序锁租约派生的 ctx),今后改动也不得让重试改用 claimCtx。
	processCtx := w.ctx
	if task.claimCtx != nil && !task.fromRetry {
		processCtx = task.claimCtx
	}
	// 只把非 nil 的租约装进接口:nil 指针装进接口后接口本身不为 nil,runDBOp 的「写必须持锁」检查会被绕过。
	var lease writeLease
	if orderingLock != nil {
		lease = orderingLock
	}
	outcome, err := processDBTask(processCtx, w, dbTask, route, lease, w.partition, task.seq, isOfflineExpand)
	if epochOverride && (!outcome.publishes() || outcome.kind == writeEqual) && err == nil {
		metrics.CountFenceRejectedBytes("epoch_override", len(dbTask.Body))
	}
	// C2(persist_schema.go):行世代高于本实例的读立即回失败回执,不进重试 —— 读按 L-04 会耗预算,排队只会让登录等满超时。
	if err != nil && dbTask.Op == db_proto.DBOp_DB_OP_READ && errors.Is(err, errSchemaTooNew) {
		w.failReadSchemaTooNew(task, dbTask, err)
		return
	}
	if err != nil && dbTask.Op == db_proto.DBOp_DB_OP_READ && errors.Is(err, errMissingPlayerSQLRow) {
		w.failReadAuthority(task, dbTask, metrics.ReadAbandonedMissingPlayerRow, err)
		return
	}

	if err == nil && !outcome.publishes() {
		switch outcome.kind {
		case writeOlder: // runDBOp 已复核落点并核锁；只确认此消息，绝不推进游标。
			logx.Debugf("FENCE-OLDER-SEQ taskID=%s ownerEpoch=%d saveSeq=%d", dbTask.TaskId, dbTask.OwnerEpoch, dbTask.SaveSeq)
			if task.fromRetry {
				if ackErr := ackRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, task.retryReceipt); ackErr != nil {
					logx.Errorf("ack older-seq receipt failed: %v", ackErr)
				}
			}
			ackKafkaTask(task)
		case writeStaleOwner:
			regressed, probeErr := w.ownerCounterRegressed(dbTask.Key, outcome.storedEpoch)
			if probeErr != nil {
				w.deferFailedTask(task, dbTask, probeErr)
				return
			}
			reason := metrics.DeadLetterFenceStaleOwner
			if regressed {
				reason = metrics.DeadLetterOwnerEpochRegressed
				metrics.CountOwnerEpochGuard(metrics.OwnerEpochRegressed)
			}
			label := writeStaleOwner
			if regressed {
				label = "owner_epoch_regressed"
			} else {
				metrics.CountStaleOwnerWriteRejected()
				logx.Errorf("STALE-OWNER-WRITE source=sql taskID=%s taskEpoch=%d committedEpoch=%d", dbTask.TaskId, dbTask.OwnerEpoch, outcome.storedEpoch)
			}
			metrics.CountFenceWrite(fenceClass(dbTask.OwnerEpoch, dbTask.SaveSeq), label)
			metrics.CountFenceRejectedBytes(label, len(dbTask.Body))
			w.quarantineOrderingConflict(task, dbTask, reason, "SQL player row rejected owner epoch")
		case writeLegacyAfterSeq:
			logx.Errorf("FENCE-LEGACY-AFTER-SEQ taskID=%s committedEpoch=%d", dbTask.TaskId, outcome.storedEpoch)
			w.quarantineOrderingConflict(task, dbTask, metrics.DeadLetterFenceLegacyAfterSeq, "unversioned write cannot overwrite fenced player row")
		case writePersistUnstamped:
			// C0:引导期关闭后仍有机制前的写方。它可能已经截断了新字段,不能落库;死信保留原件,确认写方下线后可人工重放。
			logx.Errorf("PERSIST-UNSTAMPED taskID=%s msgType=%s: unstamped player write after the persist schema bootstrap period was closed",
				dbTask.TaskId, dbTask.MsgType)
			w.quarantineOrderingConflict(task, dbTask, metrics.DeadLetterPersistUnstamped, "unstamped player write after persist schema bootstrap closed")
		case writeSchemaRegression:
			// C1b:body 世代低于行世代,说明链路上有人截断过数据。整行写会盖掉新世代的字段,只能拒写并告警排查。
			logx.Errorf("PERSIST-SCHEMA-REGRESSION taskID=%s msgType=%s: write persist schema generation is below the stored row",
				dbTask.TaskId, dbTask.MsgType)
			w.quarantineOrderingConflict(task, dbTask, metrics.DeadLetterSchemaRegression, "write persist schema generation is below the stored row")
		}
		return
	}

	if err != nil {
		if orderingLock != nil && orderingLock.lost() != nil && !errors.Is(err, errOrderingLeaseLost) {
			// 这次失败期间看门狗已经判了失租(例如复核读失败的同时续租也放弃了):按失租记延后原因。
			err = fmt.Errorf("%w: %w", errOrderingLeaseLost, err)
		}
		// 存储不可用(L-04)时每条任务都会失败一次,逐条 ERROR 会刷屏;由 deferFailedTask 按原因限速记录。
		// 世代超前(C1)同理:go/db 回滚或违反发布顺序时同一 key 的每次存盘都会走到这里。
		// 复核时副本不可信(errRouteNotReady,包在 errPlacementMoved 里)同理:Redis 重启 / 切主 / 标记被撤期间已派进 worker
		// 的每条任务都会走到这里;逐条不打 ERROR(重试来源的在 deferFailedTask 打 DEBUG),汇总由 confirm 里的 noteRouteNotReady
		// 每分钟一条 INFO。
		if !logsAsStoreOutage(err) && !errors.Is(err, errSchemaTooNew) && !errors.Is(err, errRouteNotReady) {
			logx.Errorf("worker process task failed: partition=%d, cost=%v, err=%v",
				w.partition, time.Since(startTime), err)
		}
		w.deferFailedTask(task, dbTask, err)
		return
	} else {
		logx.Debugf("worker process task success: partition=%d, cost=%v",
			w.partition, time.Since(startTime))
		// 核锁已在 runDBOp 内、共享缓存回写之前完成(ordering_lease.go 的 verify):走到这里说明持锁写完、缓存已发布。
		// Update applied seq ONLY after a successful write so that a failed
		// write does not falsely block its own retry.
		postCtx, postCancel := context.WithTimeout(context.WithoutCancel(w.ctx), orderingLockReleaseTimeout)
		markErr := advanceApplied(postCtx, lease, w.topic, dbTask, task)
		postCancel()
		if markErr != nil {
			w.deferFailedTask(task, dbTask, fmt.Errorf("persist applied cursor and epoch: %w", markErr))
			return
		}

	}

	if task.fromRetry {
		if err := ackRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, task.retryReceipt); err != nil {
			// 业务写与游标都已写入,但游标和收据 ack 是主池上的普通写,不受 WAITAOF 覆盖。只有游标没有随 Redis 回退时,
			// 留下的收据重放才会被判 stale 丢弃;AOF 尾巴丢失时可能发生旧写重放、覆盖 MySQL,这一残余只能由 MySQL
			// 条件写封死(durable_enqueue.go 文件头)。
			logx.Errorf("ack retry receipt failed; stale replay will recover it: taskID=%s err=%v", dbTask.TaskId, err)
		}
	}
	// Mark Kafka offset AFTER processing and cursor persistence complete.
	ackKafkaTask(task)
}

// observeTaskTiming 在 handleTask 结束时记 task_total,以及 Kafka 来源任务的 kafka_e2e。
//
// kafka_e2e 用的是生产者写进消息的 CreateTime(Kafka 0.10+ 的 Timestamp),跨机有时钟偏差:差值为负时丢弃;
// 重试源没有 kafkaMsg,不计。它只用来看趋势,不能拿来和本进程内的阶段耗时逐项相加。
func observeTaskTiming(task *workerTask, start time.Time) {
	end := time.Now()
	op := taskOpLabel(task.dbTask)
	metrics.ObserveStage(metrics.StageTaskTotal, op, end.Sub(start))
	if task.kafkaMsg == nil || task.kafkaMsg.Timestamp.IsZero() {
		return
	}
	if e2e := end.Sub(task.kafkaMsg.Timestamp); e2e >= 0 {
		metrics.ObserveStage(metrics.StageKafkaE2E, op, e2e)
	}
}

// consumerGroupHandler 每次 Consume 新建一个,对应一个 session。yield* 只在 Setup 里写、Cleanup 里清,
// ConsumeClaim goroutine 在 Setup 返回之后才启动、在 Cleanup 之前全部返回,所以读写不需要额外加锁。
type consumerGroupHandler struct {
	consumer *KeyOrderedKafkaConsumer

	// yieldCtx 派生自 session.Context(),交接闸的守望让出时被取消(只在 handoff 非 nil 时创建)。
	yieldCtx    context.Context
	yieldCancel context.CancelFunc
	// setupDone 只有 Setup 成功才置位:Setup 失败后的 Cleanup 不算一次 session 结束(再平衡空窗从上一个真实 session 算起)。
	setupDone bool
}

// sessionScope 返回本 session 派发任务用的父 ctx 与让出信号:有交接闸时是 yieldCtx 与它的 Done();
// 否则是 session.Context() 与 nil channel(select 里永不就绪,行为与引入之前相同)。
func (h *consumerGroupHandler) sessionScope(session sarama.ConsumerGroupSession) (context.Context, <-chan struct{}) {
	if h.yieldCtx != nil {
		return h.yieldCtx, h.yieldCtx.Done()
	}
	return session.Context(), nil
}

// yielded 报告本 session 是否已被交接闸让出(或 session 本身已结束;yieldCtx 派生自 session ctx)。
func (h *consumerGroupHandler) yielded() bool {
	return h.yieldCtx != nil && h.yieldCtx.Err() != nil
}

func (h *consumerGroupHandler) Setup(session sarama.ConsumerGroupSession) error {
	claims := session.Claims()
	partitions, assigned := claims[h.consumer.topic]
	if assigned {
		partitionIDs := make([]int32, 0, len(partitions))
		for _, partition := range partitions {
			partitionIDs = append(partitionIDs, partition)
			if partition < 0 || partition >= h.consumer.partitionCount {
				return fmt.Errorf("DATA-ORDERING: consumer claim contains partition %d outside immutable contract [0,%d) for topic %s; refusing rebalance",
					partition, h.consumer.partitionCount, h.consumer.topic)
			}
		}
		logx.Infof("consumer group assigned partitions: groupID=%s, topic=%s, partitions=%v",
			h.consumer.groupID, h.consumer.topic, partitionIDs)
	} else if h.consumer.ownerRouting() {
		// owner 模式下副本数多于分区数时这是热备的常态(retry_routing.go):不执行重试,只分拣收件箱、回收孤儿收据,
		// 属主崩溃后在 session 超时内接手。全 zone 都拿不到分区由告警 DbZoneNoPartitionClaims 兜底,不在这里报 ERROR。
		logx.Infof("no partitions assigned (hot standby: retries are only forwarded and orphan receipts reclaimed): groupID=%s topic=%s",
			h.consumer.groupID, h.consumer.topic)
	} else {
		logx.Errorf("no partitions assigned: groupID=%s, topic=%s", h.consumer.groupID, h.consumer.topic)
	}

	// 交接闸(group_handoff.go):先给本 session 分到的未落定分区播种,再登记让出函数。
	// 播种失败让 Setup 报错:sarama 释放本 session、Consume 返回错误,消费循环 1s 后重新入组;
	// 此时已经放行(released),不会再过闸,只会用重建的 inspector 再播种一次。
	if hf := h.consumer.handoff; hf != nil {
		if err := hf.seed(session, partitions); err != nil {
			return fmt.Errorf("group handoff seeding failed: %w", err)
		}
		if h.yieldCancel != nil {
			h.yieldCancel()
		}
		h.yieldCtx, h.yieldCancel = context.WithCancel(session.Context())
		if !hf.setSessionYield(h.yieldCancel) {
			// 守望在播种与登记之间已经让出:本 session 立即停止派发。
			h.yieldCancel()
		}
	}

	raw, err := json.Marshal(struct {
		Topic, Group, Member string
		Generation           int32
		Claims               map[string][]int32
	}{h.consumer.topic, h.consumer.groupID, session.MemberID(), session.GenerationID(), session.Claims()})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	h.consumer.stopMu.Lock()
	h.consumer.readyActive = true
	h.consumer.readyDigest = append([]byte(nil), digest[:]...)
	h.consumer.stopMu.Unlock()
	h.consumer.readyOnce.Do(func() {
		if h.consumer.ready != nil {
			close(h.consumer.ready)
		}
	})
	h.setupDone = true
	if d, ok := h.consumer.gap.sessionStarted(h.consumer.nowTime()); ok {
		metrics.ObserveRebalanceGap(d)
	}
	return nil
}

func (h *consumerGroupHandler) Cleanup(session sarama.ConsumerGroupSession) error {
	h.consumer.stopMu.Lock()
	h.consumer.readyActive = false
	h.consumer.stopMu.Unlock()
	claims := session.Claims()
	if partitions, ok := claims[h.consumer.topic]; ok {
		partitionIDs := make([]int32, 0, len(partitions))
		for _, partition := range partitions {
			partitionIDs = append(partitionIDs, partition)
		}
		logx.Infof("consumer group releasing partitions: groupID=%s, topic=%s, partitions=%v",
			h.consumer.groupID, h.consumer.topic, partitionIDs)
	}
	// 也要容忍 Setup 失败之后的 Cleanup(sarama 会调):此时 yieldCancel 可能为 nil、setupDone 为 false。
	if h.yieldCancel != nil {
		h.yieldCancel()
	}
	if hf := h.consumer.handoff; hf != nil {
		hf.clearSessionYield()
	}
	if h.setupDone {
		h.consumer.gap.sessionEnded(h.consumer.nowTime())
	}
	return nil
}

func (h *consumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) (retErr error) {
	// 返回的错误由 sarama 投进消费组错误通道:包一层 claimHandlerError 标明来源(consumer_group_errors.go,errors.Is / As
	// 照常穿透);因入队未确认落盘而退出时,让消费循环在重新入组之前退避(rejoin_backoff.go)。
	defer func() {
		if retErr == nil {
			return
		}
		if isDurabilityFailure(retErr) {
			h.consumer.durableBackoff.claimFailed()
		}
		retErr = &claimHandlerError{err: retErr}
	}()
	logx.Infof("ConsumeClaim invoked: partition=%d, initialOffset=%d",
		claim.Partition(), claim.InitialOffset())
	if claim.Partition() < 0 || claim.Partition() >= h.consumer.partitionCount {
		return fmt.Errorf("DATA-ORDERING: refusing claim partition %d outside immutable contract [0,%d) for topic %s",
			claim.Partition(), h.consumer.partitionCount, h.consumer.topic)
	}

	// 有交接闸时父 ctx 是 yieldCtx(守望让出即取消,在途任务随 claimCtx 一起被撤);否则就是 session ctx。
	//
	// claimCtx 不直接继承父 ctx(停机顺序,docs/design/db-task-pipeline-bottlenecks.md G6):sarama 在任一
	// ConsumeClaim 返回时就取消整个会话,停机排空时先排空完的兄弟 claim 一返回,其余 claim 的在途任务就会随会话
	// 一起被撤,只能等新属主从已提交位点重放。所以 claimCtx 由 WithoutCancel 派生,另起一个观察 goroutine:
	// 只在没有撤拉取(不是停机)时随父 ctx 取消 —— 再平衡与交接闸让出的语义与之前相同;停机时由
	// drainClaimForShutdown 按自己的时限收尾。
	parentCtx, yieldDone := h.sessionScope(session)
	claimCtx, cancelClaim := context.WithCancel(context.WithoutCancel(parentCtx))
	defer cancelClaim()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-parentCtx.Done():
			if !h.consumer.pullStopped() {
				cancelClaim()
			}
		case <-stopWatch:
		}
	}()
	// claim 租约(retry_routing.go):用最终定稿的 claimCtx 登记(交接闸的 yieldCtx、停机用的 WithoutCancel 与观察
	// goroutine 都已就位),放在主循环之前。owner 模式的重试协程只对存活的租约认领;属主资格 = 租约未关闭且 claimCtx
	// 未取消,以 claim 自身的生命周期为准。acquire 只是进程内登记、不阻塞。返回时摘除(只摘自己这一代)。
	lease := h.consumer.owned.acquire(claim.Partition(), claimCtx)
	defer h.consumer.owned.release(lease)
	acker := newClaimAcker(session, claim.InitialOffset(), cancelClaim)
	drainLimit := h.consumer.claimDrainLimit()

	// 再平衡 / 失败收尾:acker.stop 作废未标记的位点并取消 claimCtx,然后先关租约(不再接新的 owner 重试认领)再等:
	// 在途 Kafka 任务与在途 owner 重试在同一个截止内归零(claimDrained)。
	// 等的过程中到达的异步入队确认(acker.pending 含它们)不再标记位点:等确认的 offset 及其后已完成的 offset 一律由新属主
	// 重投,存储故障期还会多出重复的重试副本,靠游标与 seq 吸收。只有停机排空(drainClaimForShutdown)与消息通道关闭的
	// 排空会等确认到了再标记、在会话释放时提交。量级与「为什么不改成先等再 stop」见 durable_enqueue.go 文件头
	// 「claim 收尾时」一节(R7 复核维持这一实现,规格与设计文档按它改写)。
	drainAfterStop := func(reason string) {
		acker.stop()
		lease.close()
		if !waitClaimDrained(acker, lease, drainLimit) {
			logx.Errorf("claim drain timed out; remaining tasks are canceled and protected by cross-instance ordering locks: topic=%s partition=%d reason=%s timeout=%v inflightRetries=%d",
				h.consumer.topic, claim.Partition(), reason, drainLimit, lease.inflightCount())
		}
	}

	for {
		// 停机撤拉取之后不再取新消息:已派发的任务做完并标记位点,再返回(drainClaimForShutdown)。
		if h.consumer.pullStopped() {
			return h.drainClaimForShutdown(claim, acker, lease)
		}
		// 读方就绪闸(route_ready_gate.go,D1 第 3 批):闸关着时 messages 为 nil(这一路永不就绪 = 不取新消息、不 track、
		// 不派发,位点停在已完成的前缀,消息留在 Kafka),等 recheck(下一次刷新完成)醒来再判;其余各路照常,停机、再平衡、
		// 交接让出与失败收尾都不受影响。闸放行时 recheck 也是刷新通道:刷新把闸关上时 select 立即醒来、回到循环顶停在闸前,
		// 空闲分区上等消息的 claim 不会在闸关之后漏取下一条;代价是每个 claim 每个刷新间隔空转一圈(见 gatedMessages)。
		// 闸为 nil(开关关着)时 messages 恒为 claim.Messages()、recheck 恒为 nil。
		messages, recheck := h.consumer.gatedMessages(claim)
		select {
		case <-recheck:
			continue
		case err := <-acker.failureCh:
			drainAfterStop("offset durability failure")
			return err
		case <-h.consumer.pullStop:
			return h.drainClaimForShutdown(claim, acker, lease)
		case <-session.Context().Done():
			// 停机时会话被取消多半是兄弟 claim 先排空返回了,不能按再平衡撤掉本 claim 的在途任务。
			if h.consumer.pullStopped() {
				return h.drainClaimForShutdown(claim, acker, lease)
			}
			drainAfterStop("session canceled")
			return nil
		case <-yieldDone:
			if h.consumer.pullStopped() {
				return h.drainClaimForShutdown(claim, acker, lease)
			}
			// 交接闸守望发现对方组正在消费本 topic:停止派发、按原有收尾路径排空,返回 nil 让 sarama 结束本 session;
			// 消费循环下一轮重新过闸。yieldCtx 派生自 session ctx,两者同时结束时按 session 结束记。
			if session.Context().Err() != nil {
				drainAfterStop("session canceled")
			} else {
				drainAfterStop("handoff peer reclaimed partition")
			}
			return nil
		case msg, ok := <-messages:
			if !ok {
				// 不会再有新消息:先关租约(不再接新的 owner 重试认领),再等在途工作。
				lease.close()
				if h.consumer.pullStopped() {
					return h.drainClaimForShutdown(claim, acker, lease)
				}
				// 消息通道关了而会话还在(sarama v1.43.1 的分区消费者遇到 ErrOffsetOutOfRange 会自行关闭):等在途任务
				// 与在途 owner 重试排空(claimDrained)。
				// 等待期间撤拉取(停机)时与顶层 select 一样转去 drainClaimForShutdown:已派发的做完并标记,按
				// shutdownDrainTimeout 收尾。不转的话,Stop ① 取消 consumeCtx 带出的会话取消会被当成再平衡:acker.stop
				// 作废已完成的位点,再白等 claimDrainTimeout。这一路没有再 track 新消息,drainClaimForShutdown 再建一次
				// acker.wait 不违反 WaitGroup 的 Add / Wait 规则。
				done := claimDrained(acker, lease)
				timer := time.NewTimer(drainLimit)
				defer timer.Stop()
				select {
				case <-done:
					return nil
				case err := <-acker.failureCh:
					drainAfterStop("offset durability failure while draining")
					return err
				case <-h.consumer.pullStop:
					return h.drainClaimForShutdown(claim, acker, lease)
				case <-session.Context().Done():
					if h.consumer.pullStopped() {
						return h.drainClaimForShutdown(claim, acker, lease)
					}
					drainAfterStop("session canceled while draining")
					return nil
				case <-yieldDone:
					if h.consumer.pullStopped() {
						return h.drainClaimForShutdown(claim, acker, lease)
					}
					drainAfterStop("handoff peer reclaimed partition while draining")
					return nil
				case <-timer.C:
					drainAfterStop("messages closed with pending work")
					return fmt.Errorf("claim pending work did not drain within %v: topic=%s partition=%d inflightRetries=%d",
						drainLimit, h.consumer.topic, claim.Partition(), lease.inflightCount())
				}
			}

			if h.consumer.pullStopped() {
				// 撤拉取之后才取到的消息:不 track、不派发,已标记的位点不会越过它,由下一任属主从已提交位点重放。
				return h.drainClaimForShutdown(claim, acker, lease)
			}
			logx.Debugf("fetched message: topic=%s, partition=%d, offset=%d, key=%s",
				msg.Topic, msg.Partition, msg.Offset, string(msg.Key))
			acker.track(msg)

			var dbTask db_proto.DBTask
			if decodeErr := proto.Unmarshal(msg.Value, &dbTask); decodeErr != nil {
				// 毒消息的唯一副本交给入队层(WAITAOF 组提交时异步确认),本 claim 接着取下一条:确认之前 acker 不会让
				// 位点越过这一条;确认失败时 acker.fail 让本 claim 收尾、由新属主从这一条重投。回调可能在入队层的写协程里
				// 执行:acker.ack / fail 带互斥锁,failureCh 非阻塞发送,都可以跨 goroutine 调用。
				deadQueueKey := h.consumer.retryDeadQueueKey
				persistPoisonMessage(h.consumer.ctx, h.consumer.enqueuer(), deadQueueKey, msg, decodeErr, func(dlqErr error) {
					if dlqErr == nil {
						logx.Errorf("poison Kafka payload durably quarantined: topic=%s partition=%d offset=%d err=%v deadQueue=%s",
							msg.Topic, msg.Partition, msg.Offset, decodeErr, deadQueueKey)
						acker.ack(msg)
						return
					}
					fatalErr := fmt.Errorf("poison Kafka DLQ write failed: topic=%s partition=%d offset=%d decodeErr=%v dlqErr=%w",
						msg.Topic, msg.Partition, msg.Offset, decodeErr, dlqErr)
					if errors.Is(dlqErr, errDurableEnqueueCanceled) {
						logx.Infof("shutdown: poison Kafka payload not enqueued; offset left uncommitted for replay: topic=%s partition=%d offset=%d err=%v",
							msg.Topic, msg.Partition, msg.Offset, dlqErr)
					} else {
						logx.Errorf("DATA-LOSS PREVENTED: %v", fatalErr)
					}
					acker.fail(msg, fatalErr)
				})
				continue
			}

			// 分区契约观测(db-task-kafka-partition-contract.md「分区函数契约」):只计数、限量打日志,
			// 不拒绝消息,不改 ack、游标、重试。写不符说明有生产者破坏契约(critical 告警);读不符说明还有旧版 login 在跑。
			observeProducerPartition(msg, &dbTask, h.consumer.partitionCount)

			// 绝不允许「查不到 worker → MarkMessage 丢弃」:那是把 offset 提交掉
			// 之后静默丢玩家写入。分区超出配置时按需建 worker(见 ensureWorker)。
			partition := msg.Partition
			w, err := h.consumer.ensureWorker(partition)
			if err != nil {
				acker.fail(msg, err)
				return err
			}

			// seq = offset + 1 so that seq=0 sentinel ("no version info") is
			// reserved and never collides with a real Kafka offset.
			task := &workerTask{
				kafkaMsg:           msg,
				session:            session,
				acker:              acker,
				dbTask:             &dbTask,
				seq:                uint64(msg.Offset) + 1,
				originPartition:    msg.Partition,
				hasOriginPartition: true,
				claimCtx:           claimCtx,
				enqueuedAt:         time.Now(),
			}

			h.consumer.trackTask(task)
			// Block until worker accepts the task or context is canceled.
			// Never drop messages — the worker will mark the offset after processing.
			select {
			case w.taskCh <- task:
				logx.Debugf("message dispatched to worker: partition=%d, offset=%d", partition, msg.Offset)
			case <-h.consumer.pullStop:
				task.finishRetry()
				// 停机撤拉取时这条还没交出去(worker 通道满):摘掉它,走停机收尾。
				//   - 不加这一路,通道满时只能等 worker 腾出槽位或 Stop 第④步取消 w.ctx;而 sarama 的 Close→leave
				//     要等全部 ConsumeClaim 返回、会话释放完才拿得到锁(第③步),第③步等这里、这里等第④步,
				//     MySQL 卡住时两步互等到 SIGKILL。
				//   - 不能用 acker.fail:它会取消 claimCtx、清空 completed,把已派发的在途任务连同位点一起作废;
				//     untrack 只摘这一条(它一定在 order 末尾,见 untrack)。
				//   - 摘掉的这条与它之后的消息都没标记,由下一任属主从已提交位点重放,与「撤拉取之后取到的消息不 track」一致。
				// 与重试派发(consumeOneRetryFrom、consumeOneOwnedRetry)的 pullStop 分支同一写法:撤拉取即不再派发,
				// 不按失败处理。drainClaimForShutdown 的第一步 lease.close() 也覆盖这里的返回。
				// pullStop 为 nil(直接拼结构体的单测)时这一路永不就绪。
				if !acker.untrack(msg) {
					logx.Errorf("untrack undispatched message failed; shutdown drain waits for its timeout: topic=%s partition=%d offset=%d",
						h.consumer.topic, claim.Partition(), msg.Offset)
				}
				return h.drainClaimForShutdown(claim, acker, lease)
			case err := <-acker.failureCh:
				task.finishRetry()
				acker.fail(msg, fmt.Errorf("claim stopped before dispatch after prior failure: %w", err))
				drainAfterStop("failure before dispatch")
				return err
			case <-claimCtx.Done():
				task.finishRetry()
				acker.fail(msg, fmt.Errorf("claim canceled before dispatch: %w", claimCtx.Err()))
				drainAfterStop("claim canceled before dispatch")
				// session 结束或交接闸让出都是正常收尾,返回 nil;其它原因的取消照旧作为错误返回。
				if session.Context().Err() != nil || h.yielded() {
					return nil
				}
				return claimCtx.Err()
			case <-w.ctx.Done():
				task.finishRetry()
				acker.fail(msg, fmt.Errorf("worker context canceled before dispatch"))
				drainAfterStop("worker canceled before dispatch")
				logx.Infof("worker context canceled, stop dispatching: topic=%s, partition=%d",
					h.consumer.topic, partition)
				return nil
			}
		}
	}
}

// drainClaimForShutdown 是停机(Stop 第①步撤拉取之后)时 claim 的收尾:不再取消息,等已派发的任务全部做完、
// 由 claimAcker 标记连续完成的前缀,然后返回 nil,让 sarama 在会话释放时把这些位点最后一次提交(Stop 第②步),
// 之后才离组(第③步)—— 先提交、后离组。
//
// 不看会话 ctx:兄弟 claim 先排空返回会取消整个会话,这不代表本 claim 该放弃在途任务。等待有上限
// (shutdownDrainTimeout):超时后作废剩余任务(acker.stop 取消 claimCtx,排队中的任务不再碰库),它们由下一任属主
// 从已提交位点重放;已经在跑的写 SQL 不被取消,由排序锁兜底。只等一次,不二次等待。
// worker 被取消(c.ctx,Stop 第②步超时后在离组之前取消)时同样作废剩余任务、立即返回:此时再等也等不到 ack。
//
// 在途 owner 重试(租约,retry_routing.go)一并等:第一步先 lease.close(),不再接新的 owner 重试认领,再等 claimDrained
// (在途 Kafka 任务含异步入队的确认 + 租约 inflight 归零)。停机排空期间 claimCtx 不随会话取消,已派发的重试照常跑完。
// lease.idle() 与入队层的异步确认都不随 c.ctx 取消(入队层到 Stop 第⑥步才关),所以 consumerDone 与计时器两路必须都在:
// 计时器管正常上限,consumerDone 管第②步超时之后立即放手;少任一路,Stop 的 ③④ 会重新互等(R1 的接缝)。
func (h *consumerGroupHandler) drainClaimForShutdown(claim sarama.ConsumerGroupClaim, acker *claimAcker, lease *claimLease) error {
	lease.close()
	timeout := h.consumer.shutdownDrainTimeout
	if timeout <= 0 {
		timeout = claimDrainTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var consumerDone <-chan struct{}
	if h.consumer.ctx != nil {
		consumerDone = h.consumer.ctx.Done()
	}
	select {
	case <-claimDrained(acker, lease):
		logx.Infof("claim drained for shutdown: topic=%s partition=%d", h.consumer.topic, claim.Partition())
		return nil
	case err := <-acker.failureCh:
		h.consumer.holdStop(err)
		acker.stop()
		return err
	case <-consumerDone:
		// Stop 第②步已经超时、在离组之前先取消了 worker(见 Stop):不再等,已标记的前缀照常在会话释放时提交。
		acker.stop()
		return nil
	case <-timer.C:
		h.consumer.holdStop(errors.New("claim未在预算内实际排空"))
		acker.stop()
		logx.Errorf("shutdown drain timed out; remaining tasks replay from the committed offset: topic=%s partition=%d timeout=%v inflightRetries=%d",
			h.consumer.topic, claim.Partition(), timeout, lease.inflightCount())
		return nil
	}
}

func processDBTask(ctx context.Context, w *worker, task *db_proto.DBTask, route *placementRoute, lease writeLease, partition int32, seq uint64, isOfflineExpand bool) (writeOutcome, error) {
	if _, ok := dbOpHandlers[task.GetOp()]; !ok {
		return writeOutcome{}, fmt.Errorf("unsupported op: %d", task.GetOp())
	}
	key := strconv.FormatUint(task.Key, 10)
	logx.Debugf("received db task: taskID=%s, key=%s, partition=%d, seq=%d, isOfflineExpand=%v",
		task.TaskId, key, partition, seq, isOfflineExpand)

	if isOfflineExpand {
		logx.Debugf("offline expand mode: skip lock/status check, taskID=%s", task.TaskId)
		return processTaskWithoutLock(ctx, w.redisClient, task, route, lease)
	}

	expandStart := time.Now()
	expandStatus, err := kafkautil.GetExpandStatus(ctx, w.redisClient, w.topic)
	metrics.ObserveRedisStep(taskOpLabel(task), metrics.RedisStepExpandGet, time.Since(expandStart))
	if err != nil {
		logx.Errorf("get expand status failed: key=%s, taskID=%s, err=%v", key, task.TaskId, err)
		return tryLockAndProcess(ctx, w, key, task, seq, route, lease)
	}

	currentTime := time.Now().UnixMilli()
	if expandStatus.Status == kafkautil.ExpandStatusExpanding &&
		(expandStatus.UpdateTime == 0 || currentTime-expandStatus.UpdateTime > expandStatusExpireDuration.Milliseconds()) {
		logx.Errorf("expand status expired: topic=%s, key=%s, lastUpdate=%d",
			w.topic, key, expandStatus.UpdateTime)
		expandStatus.Status = kafkautil.ExpandStatusNormal
		_ = kafkautil.SetExpandStatus(ctx, w.redisClient, w.topic, kafkautil.ExpandStatusNormal, expandStatus.PartitionCount)
	}

	if expandStatus.Status == kafkautil.ExpandStatusExpanding {
		logx.Debugf("expanding mode: try lock for task: taskID=%s, key=%s", task.TaskId, key)
		return tryLockAndProcess(ctx, w, key, task, seq, route, lease)
	}

	return processTaskWithoutLock(ctx, w.redisClient, task, route, lease)
}

// 排序锁的租约(取锁、续租、核锁、释放)在 ordering_lease.go。
var (
	errTaskDeferred     = errors.New("task deferred because per-key expansion lock is occupied")
	errOrderingLockBusy = errors.New("task deferred because cross-instance ordering lock is occupied")
)

// writeLease 是 runDBOp 对排序锁租约的全部要求,生产实现是 *orderingLease(ordering_lease.go)。
// 抽成接口只为单测能注入替身,钉住 runDBOp 的执行顺序与上报口径。
type writeLease interface {
	// beginSQL 在发出写 SQL 之前调用一次:已失租或已过本地截止时间时返回错误(一条 SQL 都不发);
	// 否则交出只随失租取消的 ctx,以及 SQL 结束后必须调用一次的 finish。
	beginSQL() (ctx context.Context, finish func(handlerErr error), err error)
	// verify 在共享缓存回写之前做一次带 token 的核锁;失租时返回包着 errOrderingLeaseLost 的错误。
	verify(ctx context.Context) error
	runTokenScript(context.Context, *redis.Script, []string, ...interface{}) (int64, error)
}

func tryLockAndProcess(ctx context.Context, worker *worker, key string, task *db_proto.DBTask, seq uint64, route *placementRoute, lease writeLease) (writeOutcome, error) {
	lockKey := fmt.Sprintf("kafka:consumer:lock:%s", key)
	lockTTL := 5 * time.Second

	tryLock, err := worker.locker.TryLock(ctx, lockKey, lockTTL)
	if err != nil {
		return writeOutcome{}, fmt.Errorf("try lock failed: key=%s, taskID=%s, err=%w", key, task.TaskId, err)
	}
	if !tryLock.IsLocked() {
		// 这里只报告 deferred，统一由 handleTask 把 Kafka 原消息持久化到
		// ready queue，或把 processing receipt 原子搬回 ready。返回 nil 会被
		// 误判为已执行成功并 markAppliedSeq，这是已确认的数据丢失根因。
		logx.Debugf("lock occupied: task deferred without marking applied: taskID=%s key=%s seq=%d",
			task.TaskId, key, seq)
		return writeOutcome{}, errTaskDeferred
	}

	defer func() {
		if _, err := tryLock.Release(ctx); err != nil {
			logx.Errorf("release lock failed: key=%s, taskID=%s, err=%v", key, task.TaskId, err)
		}
	}()

	return processTaskWithoutLock(ctx, worker.redisClient, task, route, lease)
}

func processTaskWithoutLock(ctx context.Context, redisClient redis.Cmdable, task *db_proto.DBTask, route *placementRoute, lease writeLease) (writeOutcome, error) {
	handler, ok := dbOpHandlers[task.GetOp()]
	if !ok {
		return writeOutcome{}, fmt.Errorf("unsupported op: %d", task.GetOp())
	}
	totalStart := time.Now()
	opLabel := taskOpLabel(task)
	defer func() {
		metrics.ObserveStage(metrics.StageOpTotal, opLabel, time.Since(totalStart))
	}()

	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(task.MsgType))
	if err != nil {
		metrics.ObserveResult(opLabel, "error")
		return writeOutcome{}, fmt.Errorf("find message type failed: type=%s, taskID=%s, err=%w", task.MsgType, task.TaskId, err)
	}

	msg := dynamicpb.NewMessage(mt.Descriptor())
	if err := proto.Unmarshal(task.Body, msg); err != nil {
		metrics.ObserveResult(opLabel, "error")
		return writeOutcome{}, fmt.Errorf("unmarshal task body failed: taskID=%s, err=%w", task.TaskId, err)
	}

	// 持久 schema 世代 C0 / C1(persist_schema.go):进 SQL 之前判定,不经过 runDBOp,也就不会把一次没发 SQL 的结局报给落点库注册表。
	if kind, schemaErr := checkWriteSchema(task, msg); schemaErr != nil {
		metrics.ObserveResult(opLabel, "error")
		return writeOutcome{}, schemaErr
	} else if kind != "" {
		metrics.ObserveResult(opLabel, "ok")
		return writeOutcome{kind: kind}, nil
	}

	// op_handler(写)= SQL + 落库后复核 + 核锁 + 缓存回写;(读)= SQL + 复核 + 缓存回写 + 结果发布。
	// 写比 S4 之前多了一次核锁往返(原来在 handleTask 里、缓存回写之后),与 R19 对比时按这个口径。
	handlerStart := time.Now()
	outcome, opErr := runDBOp(ctx, redisClient, task, msg, handler, route, lease)
	metrics.ObserveStage(metrics.StageOpHandler, opLabel, time.Since(handlerStart))

	errText := ""
	if opErr != nil {
		errText = opErr.Error()
	}
	logx.Infof("task processed: taskID=%s, op=%s, success=%v, err=%s",
		task.TaskId, opLabel, opErr == nil, errText)

	if opErr != nil {
		metrics.ObserveResult(opLabel, "error")
		return outcome, opErr
	}
	metrics.ObserveResult(opLabel, "ok")
	return outcome, nil
}

// runDBOp 按固定顺序执行一条任务。
//
//	写:beginSQL → 在选中的落点库上跑 SQL(ctx 来自租约,只随失租取消)→ finish → 落库后复核(§6.3)
//	    → 核锁(verify)→ 共享缓存回写
//	读:在选中的落点库上跑 SQL(ctx 即调用方的 ctx)→ 落库后复核 → 缓存回写 + 结果发布
//
// 复核不通过时返回包着 errPlacementMoved 的错误;核锁失败时返回包着 errOrderingLeaseLost 的错误(或 ctx 被取消时的
// 普通错误)。两者都发生在共享缓存回写之前:缓存不写、读结果不发,调用方也就不会推进游标与 applied epoch ——
// 这正是「可重试,不标记游标」。核锁原来在 handleTask 里、缓存回写之后,失租的写会先把缓存写出去(G1 ③)。
//
// route 缺失是装配错误,写任务缺租约也是:都 fail-closed,一条 SQL 都不发,绝不回落到某个默认库或无锁执行。
// lease 只对写生效;handleTask 只在真的持锁时才把租约装进接口(nil 指针装进接口后接口本身不为 nil)。
func runDBOp(ctx context.Context, redisClient redis.Cmdable, task *db_proto.DBTask, msg proto.Message,
	handler dbOpHandler, route *placementRoute, lease writeLease) (writeOutcome, error) {
	if _, ok := dbOpHandlers[task.GetOp()]; !ok {
		return writeOutcome{}, fmt.Errorf("unsupported op: %d", task.GetOp())
	}
	if route == nil || route.store == nil {
		return writeOutcome{}, fmt.Errorf("no placement route for op=%s taskID=%s; refusing to guess a database", taskOpLabel(task), task.TaskId)
	}
	isWrite := task.Op == db_proto.DBOp_DB_OP_WRITE
	sqlCtx := ctx
	finish := func(error) {}
	if isWrite {
		if lease == nil {
			return writeOutcome{}, fmt.Errorf("no ordering lease for write taskID=%s; refusing to run SQL", task.TaskId)
		}
		var err error
		if sqlCtx, finish, err = lease.beginSQL(); err != nil {
			// 已失租或已过本地截止时间:一条 SQL 都没发。
			return writeOutcome{}, err
		}
	}

	sqlStart := time.Now()
	outcome, handlerErr := handler(sqlCtx, route.store, task, msg)
	metrics.ObserveStage(metrics.StageSQL, taskOpLabel(task), time.Since(sqlStart))
	finish(handlerErr)

	if sqlCtx.Err() != nil {
		// SQL 的 ctx 被本进程取消了(写:失租;读:claim 收回或停机),失败不是库的问题,成功也说明不了什么,
		// 都不报注册表。失败时按取消原因返回(写即包着 errOrderingLeaseLost 的失租错误):驱动这时常给出
		// 「连接已关」一类错误,带着 storeerr 标记原样返回会被记成 conn_lost、写还会豁免重试预算,
		// 所以原错误只作文本附上,不进错误链。
		return outcome, fmt.Errorf("%w (SQL error: %v)", context.Cause(sqlCtx), handlerErr)
	} else {
		// 只把 SQL 这一步的结果报给注册表(L-04 §3.4):复核、核锁与缓存回写的错误来自 Redis,不能拿来判断库通不通。
		route.reportStoreResult(handlerErr)
	}
	if handlerErr != nil {
		return outcome, handlerErr
	}
	if isWrite {
		switch outcome.kind {
		case writeApplied, writeEqual, writeOlder, writeStaleOwner, writeLegacyAfterSeq, writeSchemaRegression:
		default:
			return outcome, fmt.Errorf("SQL handler returned unknown write outcome %q", outcome.kind)
		}
	}
	if isWrite && (outcome.kind == writeStaleOwner || outcome.kind == writeLegacyAfterSeq || outcome.kind == writeSchemaRegression) {
		return outcome, nil
	}
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orderingLockReleaseTimeout)
	defer cancel()
	if err := route.confirm(postCtx); err != nil {
		return writeOutcome{}, err
	}
	if isWrite {
		if outcome.kind == writeOlder {
			return outcome, lease.verify(postCtx)
		}
		return outcome, publishDBWriteResult(postCtx, lease, task, msg)
	}
	return outcome, publishDBReadResult(ctx, redisClient, task, msg)
}

func encodeRetryPayload(task *db_proto.DBTask, seq uint64, partition int32, hasPartition bool) ([]byte, error) {
	taskBytes, err := proto.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("marshal task for retry failed: taskID=%s, err=%w", task.TaskId, err)
	}
	if hasPartition {
		return wrapRetryPayload(seq, partition, taskBytes), nil
	}
	if seq > 0 {
		// Preserve v1's unknown-partition semantics. Deriving Key%N and upgrading
		// it to v2 would invent provenance that the legacy payload never carried.
		payload := make([]byte, 1+8+len(taskBytes))
		payload[0] = retryPayloadMagic
		binary.BigEndian.PutUint64(payload[1:9], seq)
		copy(payload[9:], taskBytes)
		return payload, nil
	}
	return taskBytes, nil
}

func (w *worker) deferFailedTask(task *workerTask, dbTask *db_proto.DBTask, cause error) {
	// 每一次延后都按原因计数(L-04 §3.7):一眼能看出任务为什么没落库,也是「存储延后持续不退」告警的数据源。
	metrics.CountTaskDeferred(taskOpLabel(dbTask), deferReason(cause))
	// 给存盘补投器的提示(save_outbox_note.go):确定性拒写打终态,其余记「本 key 已有在途重试」。
	w.noteSaveOutboxDeferral(dbTask, cause)
	if errors.Is(cause, errOrderingLockBusy) {
		// 排序锁忙按来源分开计:owner 模式下重试与该分区新消息同一个 worker 串行,retry 来源应接近 0;
		// 明显不为 0 说明还有非属主实例在执行重试(或重叠窗口里的旧 claim)。
		origin := metrics.OriginKafka
		if task.fromRetry {
			origin = metrics.OriginRetry
		}
		metrics.CountOrderingLockBusy(origin)
	}
	if task.fromRetry {
		if retryBudgetExempt(dbTask, cause) && dbTask.RetryCount > 0 {
			// Lock contention is serialization backpressure, not a failed DB
			// attempt. Do not consume the finite business-retry budget while a
			// prior owner (possibly an old rebalance claim) is still draining.
			// 落点冻结 / 复核发现落点刚变 / 落点库暂不可用(写)同理,见 retryBudgetExempt。
			// 认领时(consumeOneRetryFrom / consumeOneOwnedRetry)先 +1,这里减回去,重试次数就停在原地。
			dbTask.RetryCount--
		}
		// 重试源的搬移是单脚本原子操作、前一状态本身在 Redis 里,同步完成,不走入队层、不加 WAITAOF(durable_enqueue.go)。
		// 目的地:owner 模式写回 p{origin}(由该分区的属主再认领),shared 模式写回共享收件箱。
		// 搬移因 Redis 出错没做成(且不是停机)时登记进待重搬表,由重试协程下一拍重搬(retry_stranded.go);
		// 收据已不在 processing(moved=false)或没有这张表时只打 ERROR,收据留在 processing 等恢复。
		payload, err := encodeRetryPayload(dbTask, task.seq, task.originPartition, task.hasOriginPartition)
		if err != nil {
			logx.Errorf("retry reschedule failed; receipt remains in processing for recovery: taskID=%s cause=%v err=%v",
				dbTask.TaskId, cause, err)
			return
		}
		dest := w.retryReadyKeyFor(task.originPartition)
		moved, err := moveRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, dest, task.retryReceipt, payload)
		if err != nil {
			if w.ctx.Err() == nil && w.stranded.add(strandedRetry{
				processingKey: w.retryProcessingKey, dest: dest, receipt: task.retryReceipt, replacement: payload,
				what: "reschedule taskID=" + dbTask.TaskId,
			}) {
				logx.Errorf("retry reschedule failed; queued for a re-move on the next retry tick: taskID=%s cause=%v err=%v",
					dbTask.TaskId, cause, err)
				return
			}
			logx.Errorf("retry reschedule failed; receipt remains in processing for recovery: taskID=%s cause=%v err=%v",
				dbTask.TaskId, cause, err)
			return
		}
		if !moved {
			logx.Errorf("retry reschedule failed; receipt remains in processing for recovery: taskID=%s cause=%v err=processing receipt not found",
				dbTask.TaskId, cause)
			return
		}
		if errors.Is(cause, errPlacementFrozen) {
			// 搬库冻结期间每条写每轮重试都会走到这里(不耗预算、直到解冻),与 routeTask 的冻结分支同口径只打
			// DEBUG,否则批量搬库时每个 topic 每秒上百条 ERROR 会淹没真故障;冻结长时间不解除看
			// frozen_deferred 计数告警。落点库不可用(errPlacementStoreUnavailable)是真实基础设施故障,
			// 仍打 ERROR。
			logx.Debugf("retry task rescheduled (placement frozen): taskID=%s retryCount=%d cause=%v",
				dbTask.TaskId, dbTask.RetryCount, cause)
			return
		}
		if errors.Is(cause, errRouteNotReady) {
			// mapping Redis 的副本不可信(D1 第 3 批):与冻结同口径只打 DEBUG,每分钟一条 INFO 汇总由 noteRouteNotReady 打;
			// 持续多久看 db_route_ready_gate 与 db_placement_guard_total{outcome="route_not_ready"}。
			logx.Debugf("retry task rescheduled (route not ready): taskID=%s retryCount=%d cause=%v",
				dbTask.TaskId, dbTask.RetryCount, cause)
			return
		}
		if logsAsStoreOutage(cause) {
			// 存储不可用(L-04):每条每轮都会走到这里,按原因限速。落点库打开失败不在此列,下面照旧逐条 ERROR。
			logStoreDeferral(dbTask, deferReason(cause), cause)
			return
		}
		if errors.Is(cause, errSchemaTooNew) {
			logSchemaTooNew(dbTask, cause)
			return
		}
		logx.Errorf("retry task rescheduled: taskID=%s retryCount=%d cause=%v",
			dbTask.TaskId, dbTask.RetryCount, cause)
		return
	}
	if logsAsStoreOutage(cause) {
		logStoreDeferral(dbTask, deferReason(cause), cause)
	} else if errors.Is(cause, errSchemaTooNew) {
		logSchemaTooNew(dbTask, cause)
	}

	partition := w.partition
	if task.hasOriginPartition {
		partition = task.originPartition
	}
	payload, err := encodeRetryPayload(dbTask, task.seq, partition, true)
	if err != nil {
		fatalErr := fmt.Errorf("task failed and retry payload encoding failed: taskID=%s partition=%d seq=%d cause=%v encodeErr=%w",
			dbTask.TaskId, partition, task.seq, cause, err)
		logx.Errorf("DATA-LOSS PREVENTED: %v", fatalErr)
		failKafkaTask(task, fatalErr)
		return
	}
	// Kafka 源失败:这一份是任务唯一的副本(位点提交之后 Kafka 不再有它),交给入队层(WAITAOF 组提交)异步确认,
	// worker 不等 fsync、接着处理下一条;ack 推迟到确认之后(persistBeforeAck)。
	w.persistBeforeAck(task, metrics.DurableTargetRetry, w.retryReadyKeyFor(partition), payload, func(enqueueErr error) {
		if enqueueErr == nil {
			// The ready queue is now the durable copy, so this Kafka offset is complete.
			ackKafkaTask(task)
			return
		}
		// claimAcker keeps this offset hole open and stops the claim. Higher
		// sub-shard completions cannot advance Sarama's high-water past it.
		fatalErr := fmt.Errorf("task failed and retry enqueue failed: taskID=%s partition=%d seq=%d cause=%v enqueueErr=%w",
			dbTask.TaskId, partition, task.seq, cause, enqueueErr)
		if errors.Is(enqueueErr, errDurableEnqueueCanceled) {
			// 停机或 ctx 取消:不是故障(不计 error、不打 DATA-LOSS),位点照样不提交,由下一任属主重放。
			logx.Infof("shutdown: retry enqueue canceled; offset left uncommitted for replay: taskID=%s partition=%d seq=%d err=%v",
				dbTask.TaskId, partition, task.seq, enqueueErr)
		} else {
			logx.Errorf("DATA-LOSS PREVENTED: %v", fatalErr)
		}
		failKafkaTask(task, fatalErr)
	})
}

// persistBeforeAck 把 Kafka 源任务的唯一副本交给入队层,onDone 负责 ack 或 failKafkaTask(恰好调用一次)。
//
// 位点语义(durable_enqueue.go 文件头):claimAcker 只标记连续完成的前缀,未确认的 offset 把本分区已提交位点停在它前面,
// 后面完成的任务越不过它;确认后前缀一次推进,确认失败则 failKafkaTask,本 claim 不再标记任何位点、由新属主重投。
// 收尾时只有停机排空(drainClaimForShutdown)与消息通道关闭的排空会等确认到了再标记、在会话释放时提交;再平衡、交接
// 让出、失败收尾(drainAfterStop)先 acker.stop,之后到的确认不再标记,未标记的位点(含等确认的这一条及其后已完成的)
// 由新属主重投,靠游标与 seq 吸收;任一 claim 失败会让 sarama 取消整个会话,作废范围是本实例全部 claim。
// onDone 可能在入队层的写协程里执行:ackKafkaTask / failKafkaTask 经 claimAcker 的互斥锁,跨 goroutine 安全;
// onDone 必须快、不得阻塞、不得再提交入队。
//
// task.acker==nil 时(只有直接拼结构体的单测)同步等 done:Sarama 的 MarkMessage 是高水位语义,
// 异步乱序 ack 会越过未确认的 offset。
func (w *worker) persistBeforeAck(task *workerTask, target, queueKey string, payload []byte, onDone func(error)) {
	submitBeforeAck(task, func(done func(error)) {
		w.enqueuer().submit(w.ctx, target, queueKey, payload, done)
	}, onDone)
}

// submitBeforeAck 是 persistBeforeAck 与 quarantinePoisonTask 共用的等待规则:task 有 claimAcker 时把 onDone 直接交给
// submit(异步确认);没有时用容量为 1 的 channel 同步等结果再调 onDone(理由见 persistBeforeAck)。
// 入队层的契约保证 done 恰好调用一次(close 也会以 canceled 完成所有已交出的请求),所以同步等待一定会返回。
func submitBeforeAck(task *workerTask, submit func(done func(error)), onDone func(error)) {
	if task != nil && task.acker != nil {
		submit(onDone)
		return
	}
	result := make(chan error, 1)
	submit(func(err error) { result <- err })
	onDone(<-result)
}

func (w *worker) deadLetterRetry(task *workerTask, dbTask *db_proto.DBTask, label, reason string) error {
	payload, err := encodeRetryPayload(dbTask, task.seq, task.originPartition, task.hasOriginPartition)
	if err != nil {
		return err
	}
	moved, err := moveRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey,
		w.retryDeadQueueKey, task.retryReceipt, payload)
	if err != nil {
		// Redis 出错(且不是停机):登记进待重搬表,重试协程下一拍重搬,搬成时再计死信(retry_stranded.go)。
		if w.ctx.Err() == nil && w.stranded.add(strandedRetry{
			processingKey: w.retryProcessingKey, dest: w.retryDeadQueueKey, receipt: task.retryReceipt, replacement: payload,
			what:    "dead_letter " + label + " taskID=" + dbTask.TaskId,
			onMoved: func() { metrics.CountTaskDeadLetter(label) },
		}) {
			return fmt.Errorf("queued for a re-move on the next retry tick: %w", err)
		}
		return err
	}
	if !moved {
		return fmt.Errorf("processing receipt not found")
	}
	metrics.CountTaskDeadLetter(label)
	logx.Errorf("DATA-LOSS/ORDERING: retry quarantined in dead queue: taskID=%s key=%d msgType=%s originKnown=%v originPartition=%d seq=%d reason=%s",
		dbTask.TaskId, dbTask.Key, dbTask.MsgType, task.hasOriginPartition, task.originPartition, task.seq, reason)
	return nil
}

// quarantineOrderingConflict 把一条不能落库的写持久化进死信后 ack。label 是 db_task_dead_letter_total 的 reason
// (metrics.DeadLetter*),reason 是写进日志的详细原因。
func (w *worker) quarantineOrderingConflict(task *workerTask, dbTask *db_proto.DBTask, label, reason string) {
	// 进死信的写都是这一版本的确定性结局:补投器停止重投同一版本,免得每轮再多一条死信(save_outbox_note.go)。
	w.markSaveOutboxTerminal(dbTask, label)
	if task.fromRetry {
		if err := w.deadLetterRetry(task, dbTask, label, reason); err != nil {
			logx.Errorf("DATA-LOSS/ORDERING: retry quarantine failed; receipt remains recoverable: taskID=%s err=%v",
				dbTask.TaskId, err)
		}
		return
	}

	payload, err := encodeRetryPayload(dbTask, task.seq, task.originPartition, task.hasOriginPartition)
	if err != nil {
		fatalErr := fmt.Errorf("DATA-LOSS/ORDERING: fresh Kafka quarantine failed: taskID=%s originPartition=%d seq=%d reason=%s err=%w",
			dbTask.TaskId, task.originPartition, task.seq, reason, err)
		logx.Errorf("%v", fatalErr)
		failKafkaTask(task, fatalErr)
		return
	}

	// Kafka 源隔离进死信:这一份同样是唯一副本,走入队层异步确认;确认之前不计死信、不 ack。
	w.persistBeforeAck(task, metrics.DurableTargetDead, w.retryDeadQueueKey, payload, func(enqueueErr error) {
		if enqueueErr == nil {
			metrics.CountTaskDeadLetter(label)
			logx.Errorf("DATA-LOSS/ORDERING: fresh Kafka task durably quarantined: taskID=%s key=%d msgType=%s originPartition=%d seq=%d reason=%s deadQueue=%s",
				dbTask.TaskId, dbTask.Key, dbTask.MsgType, task.originPartition, task.seq, reason, w.retryDeadQueueKey)
			// The offset is safe to acknowledge only after the dead queue owns a copy.
			ackKafkaTask(task)
			return
		}
		fatalErr := fmt.Errorf("DATA-LOSS/ORDERING: fresh Kafka quarantine failed: taskID=%s originPartition=%d seq=%d reason=%s err=%w",
			dbTask.TaskId, task.originPartition, task.seq, reason, enqueueErr)
		if errors.Is(enqueueErr, errDurableEnqueueCanceled) {
			logx.Infof("shutdown: quarantine enqueue canceled; offset left uncommitted for replay: taskID=%s originPartition=%d seq=%d reason=%s err=%v",
				dbTask.TaskId, task.originPartition, task.seq, reason, enqueueErr)
		} else {
			logx.Errorf("%v", fatalErr)
		}
		failKafkaTask(task, fatalErr)
	})
}

// persistPoisonMessage 把一条解不出来的 Kafka 消息原样(含原始 key / value 与解码错误)交给入队层写进死信,
// done 恰好调用一次:nil = 副本已按配置确认,调用方可以 ack;非 nil = 未确认,调用方不得 ack
// (errors.Is(err, errDurableEnqueueCanceled) 表示停机)。编码失败时同步 done(err)。
// 死信计数放在确认之后、而且放在这里而不是调用方:ConsumeClaim 在分发前解码失败时直接调用本函数(L-04 评审 F10)。
func persistPoisonMessage(ctx context.Context, q durableEnqueue, deadQueueKey string, msg *sarama.ConsumerMessage, decodeErr error, done func(error)) {
	payload, err := encodePoisonMessage(msg, decodeErr)
	if err != nil {
		done(err)
		return
	}
	q.submit(ctx, metrics.DurableTargetDead, deadQueueKey, payload, func(err error) {
		if err == nil {
			metrics.CountTaskDeadLetter(metrics.DeadLetterPoison)
		}
		done(err)
	})
}

// quarantinePoisonTask 是 handleTask / processTaskBatch 解码失败时的毒消息隔离;acker==nil 时同步等确认
// (规则同 persistBeforeAck)。
func (w *worker) quarantinePoisonTask(task *workerTask, decodeErr error) {
	if task == nil || task.kafkaMsg == nil {
		return
	}
	msg := task.kafkaMsg
	submitBeforeAck(task, func(done func(error)) {
		persistPoisonMessage(w.ctx, w.enqueuer(), w.retryDeadQueueKey, msg, decodeErr, done)
	}, func(dlqErr error) {
		if dlqErr == nil {
			logx.Errorf("poison Kafka payload durably quarantined: topic=%s partition=%d offset=%d err=%v deadQueue=%s",
				msg.Topic, msg.Partition, msg.Offset, decodeErr, w.retryDeadQueueKey)
			ackKafkaTask(task)
			return
		}
		fatalErr := fmt.Errorf("poison Kafka DLQ write failed: topic=%s partition=%d offset=%d decodeErr=%v dlqErr=%w",
			msg.Topic, msg.Partition, msg.Offset, decodeErr, dlqErr)
		if errors.Is(dlqErr, errDurableEnqueueCanceled) {
			logx.Infof("shutdown: poison Kafka payload not enqueued; offset left uncommitted for replay: topic=%s partition=%d offset=%d err=%v",
				msg.Topic, msg.Partition, msg.Offset, dlqErr)
		} else {
			logx.Errorf("DATA-LOSS PREVENTED: %v", fatalErr)
		}
		failKafkaTask(task, fatalErr)
	})
}

// Stop 优雅停机,可重复调用(只执行一次)。顺序与各步上限(docs/design/db-task-pipeline-bottlenecks.md G6):
//
//	① 撤拉取:关闭 pullStop,不阻塞。消费循环、各 claim、重试循环不再取新任务;派发 select 遇到撤拉取即不再派发
//	   (ConsumeClaim 摘掉手里那条,重试循环把收据留在本实例 processing),worker 通道满也卡不住 claim;
//	② 等消费循环退出:各 claim 把已派发的任务做完并标记位点(drainClaimForShutdown,每个 claim 最多等
//	   shutdownDrainTimeout;顶层 select、派发 select、消息通道关闭后的排空等待在撤拉取时都转到这里;撤拉取之前
//	   已经按再平衡 / 失败收尾的 claim 走原路径,最多 claimDrainTimeout),sarama 等全部 claim 返回后释放会话,
//	   释放时最后一次提交位点。本步最多等
//	   shutdownDrainTimeout + consumerShutdownGrace(默认 10s + 5s);没 Start 过的不等;超时的处理见下;
//	③ 离组(consumer.Close → LeaveGroup):正常路径下会话已释放,只剩 LeaveGroup 与关 client 的网络调用;
//	   ② 超时时还要等 sarama 把会话释放完(它的 leave 与 Consume 争同一把锁)。本步上限不由我们控制,
//	   只受 sarama 的 Net 超时与 Pod 的 terminationGracePeriodSeconds(45s)兜底;
//	④ 取消 worker 并等它们与重试协程退出(c.cancel 幂等;重试协程在 c.wg 里,见 StartRetryConsumer):ConsumeClaim
//	   返回时 claimCtx 已取消,排队中的任务不再碰库,worker 退出时交还通道里 owner 重试的在途回执(discardQueuedRetries);
//	   在途的写 SQL 不随之取消(租约只在失租时取消它),最坏等业务池读超时(每条语句 30s)加 SQL 后各 Redis 步骤的独立上限之和;
//	⑤ 归还重试收据:先等续租 goroutine 随 c.ctx 退出,再在 retryReleaseTimeout(5s)内把本实例 processing 还回
//	   共享收件箱(重试协程已在 ④ 退出,不会再认领、再续租);最后收掉交接闸:等守望 goroutine 退出(最多等一次在途的
//	   watchOnce:至多建一次 admin 连接、发两次查询,各受 handoffAdminTimeout 的网络超时约束),再关 inspector;
//	⑥ 关入队层(c.enqueuer().close(),durable_enqueue.go),必须是最后一步:入队层生命周期独立于 c.ctx,② 的排空
//	   还在等已提交请求的异步确认;close 返回时所有已提交请求的 done 都已调用过(没确认完的以 canceled 完成,
//	   位点不提交、之后重投)。组提交时最多等一批在途确认(WaitAOFTimeoutMs + 1s 的读超时)。
//
// ②③④ 的上限相加可能超过 45s 宽限期,超出部分由 SIGKILL 截断:没标记的位点照样由新属主从已提交位点重放,
// 被截断的写 SQL 由排序锁(TTL)与游标兜底。
//
// 先提交、后离组:离组会立刻触发其余成员的再平衡,新属主从已提交位点开始;位点在离组之前提交完,新属主就不必
// 重放本实例已经落库的任务。旧顺序是先取消 worker:在途任务既不 ack 也不 fail,每个 claim 都要等满 10s 排空超时,
// 已完成的位点也随之作废。
//
// ② 超时后的处理:打 ERROR,先 c.cancel() 取消 worker,再走 ③。sarama v1.43.1 的 Close→leave 要拿
// consumerGroup.lock,Consume 从入口持有它,直到会话 release 返回(等全部 ConsumeClaim → Cleanup → offsets.Close
// 最后一次提交 → 停心跳),所以超时之后离组照样排在最后一次提交之后;不先取消的话,会话里只认 c.ctx 的等待要到 ④
// 才解开,而 ④ 排在 ③ 之后,③④ 互等到 SIGKILL。正常路径(② 按时结束)不提前取消:离组时 worker 还在跑。
//
// 重试只交给分区属主与异步入队(S1,retry_routing.go / durable_enqueue.go)对停机的约束:
//   - claim 租约的 acquire 是非阻塞登记,不在 ② 超时分支提前取消要解开的等待之列。drainClaimForShutdown 等的是
//     claimDrained(acker, lease):lease.idle() 与入队层的异步确认都不随 c.ctx 取消(入队层第⑥步才关),所以那里的
//     consumerDone 与 shutdownDrainTimeout 计时器两路必须都在 —— 计时器管 ② 的正常上限,consumerDone 管 ② 超时之后
//     立即放手;少任一路,③④ 会重新互等。
//   - 第⑥步必须是最后一行;改停机顺序时保留 worker 退出的 discardQueuedRetries 与 routeToSubShard 丢弃分支的
//     finishRetry,否则每次停机都要等满排空时限。
func (c *KeyOrderedKafkaConsumer) Stop() {
	if err := c.StopAndReport(context.Background()); err != nil {
		logx.Errorf("DB消费者排空HOLD: %v", err)
	}
}

// StopAndReport的成功要求全部owner/worker/异步命令已经join；ctx截止只给调用方HOLD，不杀掉实际收尾。
func (c *KeyOrderedKafkaConsumer) StopAndReport(ctx context.Context) error {
	c.stopMu.Lock()
	if c.stopDone == nil {
		c.stopDone = make(chan struct{})
	}
	done := c.stopDone
	c.stopMu.Unlock()
	c.stopOnce.Do(func() { go func() { c.stop(); close(done) }() })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	return c.stopErr
}
func (c *KeyOrderedKafkaConsumer) holdStop(err error) {
	c.stopMu.Lock()
	c.stopErr = errors.Join(c.stopErr, err)
	c.stopMu.Unlock()
}

func (c *KeyOrderedKafkaConsumer) stop() {
	started := time.Now()

	// ① 撤拉取。
	c.stopPulling()

	// ② 等消费循环退出(上限 = claim 排空上限 + 会话释放与提交的余量)。超时说明会话里还有 claim 没返回或 sarama
	// 迟迟不释放会话:先取消 worker 再往下走(见超时分支),没提交的位点由新属主从已提交位点重放,写 SQL 由排序锁兜底。
	// 没 Start 过的不等。
	if c.isStarted() && c.consumeDone != nil {
		drain := c.shutdownDrainTimeout
		if drain <= 0 {
			drain = claimDrainTimeout
		}
		timer := time.NewTimer(drain + consumerShutdownGrace)
		select {
		case <-c.consumeDone:
		case <-timer.C:
			c.holdStop(errors.New("消费session未在预算内实际排空"))
			// 超时:在离组(③)之前先取消 worker。
			//   - 仍是先提交、后离组:sarama v1.43.1 的 Close→leave 要拿 consumerGroup.lock,Consume 从入口持有它,
			//     直到会话 release 返回(等全部 ConsumeClaim → Cleanup → offsets.Close 最后一次提交 → 停心跳)。
			//     不先取消的话,会话里只认 c.ctx 的等待要到 ④ 才解开,而 ④ 排在 ③ 之后,③④ 互等到 SIGKILL。
			//   - 提前取消只解开会话里还在等 c.ctx 的地方:毒消息交给入队层时的背压等待(persistPoisonMessage(c.ctx))与
			//     drainClaimForShutdown 的 consumerDone 一路。租约 acquire 是非阻塞登记,不在此列;排空等的
			//     lease.idle() 与入队层异步确认不随 c.ctx 取消,只能靠 consumerDone 与排空计时器兜底(见 Stop 注释)。
			//   - 写 SQL 的 ctx 来自排序锁租约(只随失租取消),不受影响;SQL 之后的复核、令牌发布与 applied 原子推进
			//     各用有界的脱离取消上下文继续收敛；重试源的读仍随 worker 取消。
			//   - sarama 自身慢(提交、心跳、LeaveGroup 的网络调用)时 Close 照样要等,只受它的 Net 超时与
			//     terminationGracePeriodSeconds 兜底。
			// 正常路径(② 按时结束)不提前取消:离组时 worker 还在跑(TestStop_OrderPullDrainLeaveCancelRelease 钉住)。
			logx.Errorf("consumer loop did not exit within %v after pulling stopped; canceling workers before leaving the group (uncommitted offsets replay from the last commit): groupID=%s topic=%s",
				drain+consumerShutdownGrace, c.groupID, c.topic)
			if c.cancel != nil {
				c.cancel()
			}
		}
		timer.Stop()
	}

	// ③ 离组:位点已在会话释放时提交(② 超时时 Close 内部仍要等 sarama 释放完会话,见上)。
	if c.consumer != nil {
		if err := c.consumer.Close(); err != nil {
			c.holdStop(err)
			logx.Errorf("close consumer group failed: groupID=%s, err=%v", c.groupID, err)
		} else {
			logx.Infof("consumer group closed: groupID=%s, topic=%s", c.groupID, c.topic)
		}
	}

	// ④ 取消 worker 并等它们与重试协程退出(② 超时时已取消过,cancel 幂等)。在途的写 SQL 不随之取消(租约只在失租时
	// 取消它),受业务池读超时约束；最坏等待还包括 SQL 后各 Redis 步骤的独立上限之和。
	if c.cancel != nil {
		c.cancel()
	}
	if c.wg != nil {
		c.wg.Wait()
	}

	// ⑤ 归还重试收据(worker 与重试协程已全部停下,不会再认领、再续租);再收掉交接闸(守望 goroutine 随 c.ctx 退出)。
	// 所有durable done先真实返回，避免归还processing后迟到callback再次改队列。
	c.enqueuer().close()
	if status, ok := c.enqueuer().(interface{ drainError() error }); ok {
		if err := status.drainError(); err != nil {
			c.holdStop(err)
		}
	}
	c.releaseRetryOwnership()
	c.closeHandoff()
	if c.stranded.size() != 0 {
		c.holdStop(errors.New("stranded retry收据未逐条确认"))
	}
	if c.retryOwner.unresolvedClaimCount() != 0 {
		c.holdStop(errors.New("retry仍有结果未知认领"))
	}
	if c.ownerEpochClient != nil {
		if err := c.ownerEpochClient.Close(); err != nil {
			logx.Errorf("close owner epoch reader: %v", err)
		}
	}
	// ⑥ 最后关入队层:生命周期独立于 c.ctx,不能早于 ⑤;close 返回时所有已提交请求的 done 都已调用过。
	if c.relayWork != nil {
		c.relayWork.CloseAdmission()
		if err := c.relayWork.Wait(context.Background()); err != nil {
			c.holdStop(err)
		}
	}
	logx.Infof("db consumer stopped: groupID=%s topic=%s took=%v", c.groupID, c.topic, time.Since(started))
}

// stopPulling 撤拉取(Stop 第①步):关闭 pullStop,只关一次;没有 pullStop 的(直接拼结构体的单测)什么都不做。
func (c *KeyOrderedKafkaConsumer) stopPulling() {
	c.pullStopOnce.Do(func() {
		if c.pullStop != nil {
			close(c.pullStop)
		}
	})
}

// pullStopped 报告是否已经撤拉取;pullStop 为 nil 时恒为 false。
func (c *KeyOrderedKafkaConsumer) pullStopped() bool {
	if c.pullStop == nil {
		return false
	}
	select {
	case <-c.pullStop:
		return true
	default:
		return false
	}
}

// isStarted 报告 Start 是否已经跑过(拉起 worker 与消费循环)。
func (c *KeyOrderedKafkaConsumer) isStarted() bool {
	c.workersMu.RLock()
	defer c.workersMu.RUnlock()
	return c.started
}

// closeHandoff 在消费组关闭之后收掉交接闸:先等守望 goroutine 退出(c.ctx 已取消,最多等一次在途 admin 调用),
// 再关 inspector。失败只记日志:admin 连接是观测用的,关不掉不影响已提交的位点。
func (c *KeyOrderedKafkaConsumer) closeHandoff() {
	if c.handoff == nil {
		return
	}
	if c.handoffDone != nil {
		<-c.handoffDone
	}
	if err := c.handoff.Close(); err != nil {
		logx.Errorf("close group handoff inspector failed: groupID=%s peer=%s err=%v", c.groupID, c.handoff.peer, err)
	}
}

// releaseRetryOwnership 在 worker 全部停下之后,把本实例没做完的重试收据还回 ready 并摘掉自己的租约,
// 让别的实例立刻接手,而不是等租约过期。c.ctx 此时已取消,所以用一个新的、有时限的 context。
// 失败只记日志:租约过期后别的实例照样会回收(最多晚一个租约周期)。
func (c *KeyOrderedKafkaConsumer) releaseRetryOwnership() {
	if c.retryOwner == nil {
		return
	}
	if c.retryOwnerDone != nil {
		<-c.retryOwnerDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), retryReleaseTimeout)
	defer cancel()
	if err := c.drainStranded(ctx); err != nil {
		c.holdStop(err)
	}
	if !c.retryOwner.resolveUnresolvedClaims(ctx) {
		c.holdStop(errors.New("结果未知的retry认领未解决"))
	}
	if err := c.retryOwner.release(ctx); err != nil {
		c.holdStop(err)
		logx.Errorf("release retry receipt ownership failed (receipts stay recoverable until the lease expires): topic=%s err=%v",
			c.topic, err)
	}
}

func (c *KeyOrderedKafkaConsumer) trackTask(task *workerTask) {
	c.stopMu.Lock()
	if c.relayWork == nil {
		c.relayWork = drainproof.New()
	}
	gate := c.relayWork
	c.stopMu.Unlock()
	done, err := gate.Enter()
	if err != nil {
		c.holdStop(err)
		return
	}
	task.drainDone = done
}

// FrontierDigest只在StopAndReport真实join后返回本boot全部实际接收/完成集合的前沿。
func (c *KeyOrderedKafkaConsumer) FrontierDigest(ctx context.Context) ([]byte, error) {
	if err := c.StopAndReport(ctx); err != nil {
		return nil, err
	}
	c.stopMu.Lock()
	gate := c.relayWork
	c.stopMu.Unlock()
	if gate == nil {
		gate = drainproof.New()
		gate.CloseAdmission()
	}
	return gate.SettledDigest(ctx)
}

func (c *KeyOrderedKafkaConsumer) ReadyFrontier(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ready:
	}
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	if !c.readyActive || len(c.readyDigest) != 32 {
		return nil, drainproof.ErrHold
	}
	return append([]byte(nil), c.readyDigest...), nil
}
