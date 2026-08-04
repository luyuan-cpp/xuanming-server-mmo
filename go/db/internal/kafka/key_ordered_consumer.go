package kafka

import (
	"context"
	db_config "db/internal/config"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	db_proto "proto/db"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luyuancpp/proto2mysql"

	"db/internal/locker"

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
	// taskResultNotifyChannel is the Redis Pub/Sub channel that downstream
	// services (e.g. login) subscribe to for event-driven task-result delivery.
	// Payload: the task_id string. Subscriber LPOPs the per-task list to fetch
	// the actual TaskResult bytes.
	taskResultNotifyChannel = "task:result:notify"
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
	consumer        sarama.ConsumerGroup
	redisClient     redis.Cmdable
	topic           string
	groupID         string
	partitionCount  int32
	subShardCount   int
	isOfflineExpand bool

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
}

type worker struct {
	partition          int32
	taskCh             chan *workerTask
	ctx                context.Context
	redisClient        redis.Cmdable
	locker             *locker.RedisLocker
	topic              string
	retryQueueKey      string
	retryProcessingKey string
	retryDeadQueueKey  string
	wg                 *sync.WaitGroup

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

func waitClaimPending(a *claimAcker, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-a.wait():
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

// dbOpHandler is the handler signature for DB operations.
type dbOpHandler func(
	ctx context.Context,
	redisClient redis.Cmdable,
	task *db_proto.DBTask,
	msg proto.Message,
) string

var dbOpHandlers = map[string]dbOpHandler{
	"read":  handleDBReadOp,
	"write": handleDBWriteOp,
}

func handleDBReadOp(
	ctx context.Context,
	redisClient redis.Cmdable,
	task *db_proto.DBTask,
	msg proto.Message,
) string {
	if err := proto_sql.DB.SqlModel.FindOneByWhereClause(msg, task.WhereCase); err != nil && !errors.Is(err, proto2mysql.ErrNoRowsFound) {
		return fmt.Sprintf("db read failed: %v", err)
	}

	resultData, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Sprintf("marshal read result failed: %v", err)
	}

	// Write-back: update Redis cache so login can hit cache next time
	cacheKey := buildCacheKey(task)
	cacheStart := time.Now()
	if err := redisClient.Set(ctx, cacheKey, resultData, cacheTTL()).Err(); err != nil {
		logx.Errorf("cache write-back failed on read: key=%s, taskID=%s, err=%v", cacheKey, task.TaskId, err)
	}
	metrics.ObserveStage(metrics.StageCacheWrite, "read", time.Since(cacheStart))

	// Write result to Redis (read ops only)
	if task.TaskId != "" {
		publishStart := time.Now()
		result := &db_proto.TaskResult{
			Success: true,
			Data:    resultData,
			Error:   "",
		}
		resBytes, err := proto.Marshal(result)
		if err != nil {
			return fmt.Sprintf("marshal result failed: %v", err)
		}
		resultKey := fmt.Sprintf("task:result:%s", task.TaskId)
		if err := redisClient.LPush(ctx, resultKey, resBytes).Err(); err != nil {
			return fmt.Sprintf("save read result failed: %v", err)
		}
		if err := redisClient.Expire(ctx, resultKey, taskResultExpireDuration).Err(); err != nil {
			return fmt.Sprintf("set expire for result key failed: %v", err)
		}
		// Notify event-driven subscribers (e.g. login dispatcher) so they don't
		// have to BLPOP-wait. Existing LPush + BLPOP consumers still work; this
		// is purely additive. Failure to publish only degrades latency for
		// notification-based consumers (their stale-cleanup will catch it).
		if err := redisClient.Publish(ctx, taskResultNotifyChannel, task.TaskId).Err(); err != nil {
			logx.Errorf("publish task result notify failed: taskID=%s, err=%v", task.TaskId, err)
		}
		metrics.ObserveStage(metrics.StageResultPublish, "read", time.Since(publishStart))
	}

	return ""
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
func orderingForTask(ctx context.Context, rc redis.Cmdable, topic string, dbTask *db_proto.DBTask, task *workerTask) (orderingDecision, error) {
	// Legacy retry payloads do not prove their source partition. Quarantine them
	// even when this key has no marker: Key%N is not the producer's partitioner,
	// and executing the task could race a fresh write on another worker.
	if task.fromRetry && (task.seq == 0 || !task.hasOriginPartition) {
		return orderingIncomparable, nil
	}
	if dbTask.Op != "write" {
		return orderingApply, nil
	}
	raw, err := rc.Get(ctx, appliedSeqKey(topic, dbTask.Key, dbTask.MsgType)).Result()
	if errors.Is(err, redis.Nil) {
		return orderingApply, nil
	}
	if err != nil {
		return orderingIncomparable, fmt.Errorf("read applied cursor: %w", err)
	}
	previous, err := parseAppliedCursor(raw)
	if err != nil {
		return orderingIncomparable, err
	}

	if task.seq == 0 || !task.hasOriginPartition {
		return orderingIncomparable, nil
	}
	if !previous.hasPartition {
		return orderingIncomparable, nil
	}
	if previous.partition != task.originPartition {
		return orderingIncomparable, nil
	}
	if task.seq <= previous.seq {
		return orderingStale, nil
	}
	return orderingApply, nil
}

// markAppliedSeq records a partition-qualified cursor only after the business
// write succeeds. Ordering metadata is deliberately persistent (expiration=0):
// tying it to the 24h business-cache TTL would let a delayed retry resurrect
// after the guard expired and overwrite a newer full snapshot.
func markAppliedSeq(ctx context.Context, rc redis.Cmdable, topic string, dbTask *db_proto.DBTask, task *workerTask) error {
	if task.seq == 0 || dbTask.Op != "write" {
		return nil
	}
	cursor := appliedCursor{seq: task.seq, partition: task.originPartition, hasPartition: task.hasOriginPartition}
	return rc.Set(ctx, appliedSeqKey(topic, dbTask.Key, dbTask.MsgType), encodeAppliedCursor(cursor), 0).Err()
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
// 才能和同一 key 的新消息保持串行。生产端分区是一致性哈希(分区不可用时还会
// 临时重映射),消费端拿 Key%N 重算通常得到另一个分区 —— 旧实现因此把同一
// 玩家的重试与新写并发跑在两个 goroutine 里,旧 seq 可以覆盖新 seq。
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

func handleDBWriteOp(
	ctx context.Context,
	redisClient redis.Cmdable,
	task *db_proto.DBTask,
	msg proto.Message,
) string {
	if err := proto_sql.DB.SqlModel.Save(msg); err != nil {
		return fmt.Sprintf("db write failed: %v", err)
	}
	if err := writeBackDBCache(ctx, redisClient, task, msg); err != nil {
		// MySQL is already durable, but ACKing here would leave login reading an
		// older Redis snapshot indefinitely. Return failure so the same
		// idempotent full-row write remains in Kafka/retry until cache publication
		// succeeds; the applied cursor is intentionally not advanced yet.
		return fmt.Sprintf("db write durable but cache publication failed: %v", err)
	}
	return ""
}

func writeBackDBCache(ctx context.Context, redisClient redis.Cmdable, task *db_proto.DBTask, msg proto.Message) error {
	// Write-back: update Redis cache after MySQL write (serial per key via Kafka partition)
	cacheKey := buildCacheKey(task)
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal key=%s taskID=%s: %w", cacheKey, task.TaskId, err)
	}
	cacheStart := time.Now()
	if err := redisClient.Set(ctx, cacheKey, data, cacheTTL()).Err(); err != nil {
		metrics.ObserveStage(metrics.StageCacheWrite, "write", time.Since(cacheStart))
		return fmt.Errorf("set key=%s taskID=%s: %w", cacheKey, task.TaskId, err)
	}
	metrics.ObserveStage(metrics.StageCacheWrite, "write", time.Since(cacheStart))
	return nil
}

func NewKeyOrderedKafkaConsumer(
	cfg db_config.Config,
	redisClient redis.Cmdable,
) (*KeyOrderedKafkaConsumer, error) {
	config := sarama.NewConfig()
	config.Version = sarama.V3_5_0_0
	config.Consumer.Return.Errors = true
	config.Consumer.Offsets.Initial = sarama.OffsetOldest

	consumerGroup, err := sarama.NewConsumerGroup(
		cfg.ServerConfig.Kafka.Brokers,
		cfg.ServerConfig.Kafka.GroupID,
		config,
	)
	if err != nil {
		return nil, fmt.Errorf("create consumer group failed: groupID=%s, err=%w", cfg.ServerConfig.Kafka.GroupID, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}

	retryQueueKey := fmt.Sprintf("kafka:retry:queue:%s", cfg.ServerConfig.Kafka.Topic)
	retryProcessingKey := fmt.Sprintf("kafka:retry:processing:%s", cfg.ServerConfig.Kafka.Topic)
	retryDeadQueueKey := fmt.Sprintf("kafka:dead:queue:%s", cfg.ServerConfig.Kafka.Topic)
	lockerIns := locker.NewRedisLocker(redisClient)

	subShardCount := cfg.ServerConfig.Kafka.SubShardCount
	if subShardCount <= 0 {
		subShardCount = 1 // 1 = legacy single-goroutine-per-partition behaviour
	}

	c := &KeyOrderedKafkaConsumer{
		consumer:             consumerGroup,
		redisClient:          redisClient,
		topic:                cfg.ServerConfig.Kafka.Topic,
		groupID:              cfg.ServerConfig.Kafka.GroupID,
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
	}
	for i := int32(0); i < cfg.ServerConfig.Kafka.PartitionCnt; i++ {
		c.workers[i] = c.buildWorker(i)
	}
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
		locker:             c.locker,
		topic:              c.topic,
		retryQueueKey:      c.retryQueueKey,
		retryProcessingKey: c.retryProcessingKey,
		retryDeadQueueKey:  c.retryDeadQueueKey,
		wg:                 c.wg,
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
	// db 服务在部署与本地脚本中均被强制为单实例。启动时先把上次进程崩溃
	// 遗留的 processing 收据全部搬回 ready，恢复 at-least-once 交付。
	if err := c.recoverRetryProcessing(); err != nil {
		return fmt.Errorf("recover retry processing queue: %w", err)
	}

	c.workersMu.Lock()
	for _, w := range c.workers {
		c.wg.Add(1)
		go w.start(c.isOfflineExpand)
	}
	c.started = true
	c.workersMu.Unlock()

	c.StartRetryConsumer()

	go func() {
		for {
			if err := c.consumer.Consume(c.ctx, []string{c.topic}, &consumerGroupHandler{consumer: c}); err != nil {
				logx.Errorf("consumer group consume failed: groupID=%s, topic=%s, err=%v", c.groupID, c.topic, err)
				time.Sleep(1 * time.Second)
			}
			if c.ctx.Err() != nil {
				logx.Infof("consumer group stopped: groupID=%s, topic=%s", c.groupID, c.topic)
				return
			}
		}
	}()

	logx.Infof("consumer started successfully: groupID=%s, topic=%s, partitionCount=%d, isOfflineExpand=%v",
		c.groupID, c.topic, c.partitionCount, c.isOfflineExpand)
	return nil
}

func (c *KeyOrderedKafkaConsumer) StartRetryConsumer() {
	go func() {
		ticker := time.NewTicker(c.retryConsumeInterval)
		defer ticker.Stop()

		for {
			select {
			case <-c.ctx.Done():
				logx.Infof("retry consumer stopped: topic=%s, retryQueueKey=%s", c.topic, c.retryQueueKey)
				return
			case <-ticker.C:
				c.consumeRetryQueue()
			}
		}
	}()
	logx.Infof("retry consumer started: topic=%s, interval=%v, maxRetryTimes=%d",
		c.topic, c.retryConsumeInterval, c.retryMaxTimes)
}

// recoverRetryProcessing restores receipts left behind by a crashed singleton
// db consumer. RPOPLPUSH keeps every payload present in at least one durable
// list throughout recovery.
func (c *KeyOrderedKafkaConsumer) recoverRetryProcessing() error {
	var recovered int
	for {
		_, err := c.redisClient.RPopLPush(c.ctx, c.retryProcessingKey, c.retryQueueKey).Result()
		if errors.Is(err, redis.Nil) {
			if recovered > 0 {
				logx.Errorf("recovered %d abandoned retry receipts: processing=%s ready=%s",
					recovered, c.retryProcessingKey, c.retryQueueKey)
			}
			return nil
		}
		if err != nil {
			return err
		}
		recovered++
	}
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

func moveRetryReceipt(ctx context.Context, rc redis.Cmdable, processingKey, destinationKey string, receipt, replacement []byte) (bool, error) {
	result, err := rc.Eval(ctx, moveRetryReceiptScript,
		[]string{processingKey, destinationKey}, receipt, replacement).Int64()
	return result == 1, err
}

func ackRetryReceipt(ctx context.Context, rc redis.Cmdable, processingKey string, receipt []byte) error {
	if len(receipt) == 0 {
		return nil
	}
	removed, err := rc.LRem(ctx, processingKey, 1, receipt).Result()
	if err != nil {
		return err
	}
	if removed != 1 {
		return fmt.Errorf("retry receipt not found in processing queue")
	}
	return nil
}

func (c *KeyOrderedKafkaConsumer) moveClaimedRetryToDead(receipt, payload []byte, reason string) {
	moved, err := moveRetryReceipt(c.ctx, c.redisClient, c.retryProcessingKey, c.retryDeadQueueKey, receipt, payload)
	if err != nil || !moved {
		logx.Errorf("retry dead-letter failed; receipt remains recoverable in processing: topic=%s reason=%s moved=%v err=%v",
			c.topic, reason, moved, err)
		return
	}
	logx.Errorf("retry task moved to dead queue: topic=%s reason=%s deadQueue=%s", c.topic, reason, c.retryDeadQueueKey)
}

func (c *KeyOrderedKafkaConsumer) consumeRetryQueue() {
	// Atomic ready -> processing claim. A process crash after this point leaves
	// the only receipt in processing; Start() restores it before consuming again.
	msgBytes, err := c.redisClient.RPopLPush(c.ctx, c.retryQueueKey, c.retryProcessingKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return
		}
		logx.Errorf("claim retry queue failed: ready=%s processing=%s err=%v",
			c.retryQueueKey, c.retryProcessingKey, err)
		return
	}

	seq, storedPartition, hasPartition, taskBytes := unwrapRetryPayload(msgBytes)
	var task db_proto.DBTask
	if err := proto.Unmarshal(taskBytes, &task); err != nil {
		c.moveClaimedRetryToDead(msgBytes, msgBytes, fmt.Sprintf("malformed payload: %v", err))
		return
	}

	if task.RetryCount >= int32(c.retryMaxTimes) {
		c.moveClaimedRetryToDead(msgBytes, msgBytes,
			fmt.Sprintf("max retries exceeded taskID=%s retryCount=%d", task.TaskId, task.RetryCount))
		return
	}
	if !hasPartition {
		// v1/raw legacy payloads have no trustworthy producer partition. Key%N
		// is not equivalent to Sarama's hash partitioner, so dispatching them can
		// race or regress fresh writes. Preserve the payload for operator repair.
		c.moveClaimedRetryToDead(msgBytes, msgBytes,
			fmt.Sprintf("ORDERING: legacy retry has no proven origin partition taskID=%s key=%d", task.TaskId, task.Key))
		return
	}

	task.RetryCount++

	// 重试必须路由回**原始 Kafka 分区**的 worker:同 key 的新消息在生产端
	// 一致性哈希指定的分区上串行,重试只有回到同一个 worker 才不会和新写
	// 并发。旧格式没有原始分区,已在上方 fail-closed 隔离,不再用 Key%N 猜测。
	partition := storedPartition
	if partition < 0 {
		c.moveClaimedRetryToDead(msgBytes, msgBytes,
			fmt.Sprintf("invalid origin partition=%d taskID=%s", partition, task.TaskId))
		return
	}
	w, err := c.ensureWorker(partition)
	if err != nil {
		c.moveClaimedRetryToDead(msgBytes, msgBytes,
			fmt.Sprintf("ORDERING: retry partition outside immutable contract: %v taskID=%s", err, task.TaskId))
		return
	}

	retryTask := &workerTask{
		dbTask:             &task,
		seq:                seq,
		originPartition:    partition,
		hasOriginPartition: hasPartition,
		fromRetry:          true,
		retryReceipt:       append([]byte(nil), msgBytes...),
	}
	select {
	case w.taskCh <- retryTask:
		logx.Debugf("retry task routed to worker: taskID=%s, partition=%d, retryCount=%d", task.TaskId, partition, task.RetryCount)
	case <-c.ctx.Done():
		return
	}
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

	// Router loop: drain w.taskCh and fan each task out to the right
	// sub-shard by hash(task.Key). On shutdown, close all sub-channels
	// so the sub-workers exit.
	for {
		select {
		case <-w.ctx.Done():
			logx.Infof("worker received stop signal: partition=%d", w.partition)
			for _, ch := range w.subShardChans {
				close(ch)
			}
			subWg.Wait()
			return
		case task, ok := <-w.taskCh:
			if !ok {
				logx.Infof("worker task channel closed: partition=%d", w.partition)
				for _, ch := range w.subShardChans {
					close(ch)
				}
				subWg.Wait()
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
func (w *worker) routeToSubShard(task *workerTask) {
	if w.subShardCount <= 1 {
		// Legacy: only one sub-shard, no routing needed.
		w.subShardChans[0] <- task
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

	shard := int(routingKey % uint64(w.subShardCount))
	select {
	case w.subShardChans[shard] <- task:
	case <-w.ctx.Done():
		// Don't deadlock on shutdown if a shard is already gone.
	}
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
func (w *worker) processTaskBatch(batch []*workerTask, isOfflineExpand bool) {
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
		case "write":
			if pt.wt.seq == 0 || !pt.wt.hasOriginPartition {
				continue
			}
			byPartition := liveBestWrite[ck]
			if byPartition == nil {
				byPartition = make(map[int32]int)
				liveBestWrite[ck] = byPartition
			}
			if bestIndex, exists := byPartition[pt.wt.originPartition]; exists {
				best := parsed[bestIndex].wt
				if pt.wt.seq <= best.seq {
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
		case "read":
			// Read acts as a barrier: any earlier write for ck is in a
			// different segment and must execute so the read sees post-write
			// state.
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
					// The retained higher version remains authoritative. Keep the
					// receipt recoverable in processing; startup recovery will replay
					// it and the durable cursor guard will drop it as stale.
					logx.Errorf("ack superseded retry receipt failed; it remains recoverable: taskID=%s err=%v",
						pt.dbTask.TaskId, err)
				}
			}
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
	if task.claimCtx != nil {
		select {
		case <-task.claimCtx.Done():
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

	var orderingLock *orderingLease
	if dbTask.Op == "write" {
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
	switch decision {
	case orderingStale:
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
		w.quarantineOrderingConflict(task, dbTask,
			"origin partition cannot be safely compared with the existing applied cursor")
		return
	}

	startTime := time.Now()
	processCtx := w.ctx
	if task.claimCtx != nil {
		processCtx = task.claimCtx
	}
	err := processDBTask(processCtx, w, dbTask, w.partition, task.seq, isOfflineExpand)

	if err != nil {
		logx.Errorf("worker process task failed: partition=%d, cost=%v, err=%v",
			w.partition, time.Since(startTime), err)
		w.deferFailedTask(task, dbTask, err)
		return
	} else {
		logx.Debugf("worker process task success: partition=%d, cost=%v",
			w.partition, time.Since(startTime))
		// A long DB call must still own the cross-instance guard before it can
		// publish its cursor. The watchdog renews the lease while MySQL is busy;
		// verify() performs one final token-checked Lua extension before SET.
		if orderingLock != nil {
			if verifyErr := orderingLock.verify(w.ctx); verifyErr != nil {
				w.deferFailedTask(task, dbTask, fmt.Errorf("ordering lock ownership lost after DB write: %w", verifyErr))
				return
			}
		}
		// Update applied seq ONLY after a successful write so that a failed
		// write does not falsely block its own retry.
		if markErr := markAppliedSeq(w.ctx, w.redisClient, w.topic, dbTask, task); markErr != nil {
			w.deferFailedTask(task, dbTask, fmt.Errorf("persist applied cursor: %w", markErr))
			return
		}
	}

	if task.fromRetry {
		if err := ackRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, task.retryReceipt); err != nil {
			// The business write and cursor are already durable. Leaving the
			// receipt causes a harmless stale replay rather than at-most-once loss.
			logx.Errorf("ack retry receipt failed; stale replay will recover it: taskID=%s err=%v", dbTask.TaskId, err)
		}
	}
	// Mark Kafka offset AFTER processing and cursor persistence complete.
	ackKafkaTask(task)
}

type consumerGroupHandler struct {
	consumer *KeyOrderedKafkaConsumer
}

func (h *consumerGroupHandler) Setup(session sarama.ConsumerGroupSession) error {
	claims := session.Claims()
	if partitions, ok := claims[h.consumer.topic]; ok {
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
	} else {
		logx.Errorf("no partitions assigned: groupID=%s, topic=%s", h.consumer.groupID, h.consumer.topic)
	}
	return nil
}

func (h *consumerGroupHandler) Cleanup(session sarama.ConsumerGroupSession) error {
	claims := session.Claims()
	if partitions, ok := claims[h.consumer.topic]; ok {
		partitionIDs := make([]int32, 0, len(partitions))
		for p := range partitions {
			partitionIDs = append(partitionIDs, int32(p))
		}
		logx.Infof("consumer group releasing partitions: groupID=%s, topic=%s, partitions=%v",
			h.consumer.groupID, h.consumer.topic, partitionIDs)
	}
	return nil
}

func (h *consumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	logx.Infof("ConsumeClaim invoked: partition=%d, initialOffset=%d",
		claim.Partition(), claim.InitialOffset())
	if claim.Partition() < 0 || claim.Partition() >= h.consumer.partitionCount {
		return fmt.Errorf("DATA-ORDERING: refusing claim partition %d outside immutable contract [0,%d) for topic %s",
			claim.Partition(), h.consumer.partitionCount, h.consumer.topic)
	}

	claimCtx, cancelClaim := context.WithCancel(session.Context())
	defer cancelClaim()
	acker := newClaimAcker(session, claim.InitialOffset(), cancelClaim)

	drainAfterStop := func(reason string) {
		acker.stop()
		if !waitClaimPending(acker, claimDrainTimeout) {
			logx.Errorf("claim drain timed out; remaining tasks are canceled and protected by cross-instance ordering locks: topic=%s partition=%d reason=%s timeout=%v",
				h.consumer.topic, claim.Partition(), reason, claimDrainTimeout)
		}
	}

	for {
		select {
		case err := <-acker.failureCh:
			drainAfterStop("offset durability failure")
			return err
		case <-session.Context().Done():
			drainAfterStop("session canceled")
			return nil
		case msg, ok := <-claim.Messages():
			if !ok {
				done := acker.wait()
				timer := time.NewTimer(claimDrainTimeout)
				defer timer.Stop()
				select {
				case <-done:
					return nil
				case err := <-acker.failureCh:
					drainAfterStop("offset durability failure while draining")
					return err
				case <-session.Context().Done():
					drainAfterStop("session canceled while draining")
					return nil
				case <-timer.C:
					drainAfterStop("messages closed with pending work")
					return fmt.Errorf("claim pending work did not drain within %v: topic=%s partition=%d",
						claimDrainTimeout, h.consumer.topic, claim.Partition())
				}
			}

			logx.Debugf("fetched message: topic=%s, partition=%d, offset=%d, key=%s",
				msg.Topic, msg.Partition, msg.Offset, string(msg.Key))
			acker.track(msg)

			var dbTask db_proto.DBTask
			if err := proto.Unmarshal(msg.Value, &dbTask); err != nil {
				if dlqErr := persistPoisonMessage(h.consumer.ctx, h.consumer.redisClient,
					h.consumer.retryDeadQueueKey, msg, err); dlqErr != nil {
					fatalErr := fmt.Errorf("poison Kafka DLQ write failed: topic=%s partition=%d offset=%d decodeErr=%v dlqErr=%w",
						msg.Topic, msg.Partition, msg.Offset, err, dlqErr)
					logx.Errorf("DATA-LOSS PREVENTED: %v", fatalErr)
					acker.fail(msg, fatalErr)
				} else {
					logx.Errorf("poison Kafka payload durably quarantined: topic=%s partition=%d offset=%d err=%v deadQueue=%s",
						msg.Topic, msg.Partition, msg.Offset, err, h.consumer.retryDeadQueueKey)
					acker.ack(msg)
				}
				continue
			}

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
			}

			// Block until worker accepts the task or context is canceled.
			// Never drop messages — the worker will mark the offset after processing.
			select {
			case w.taskCh <- task:
				logx.Debugf("message dispatched to worker: partition=%d, offset=%d", partition, msg.Offset)
			case err := <-acker.failureCh:
				acker.fail(msg, fmt.Errorf("claim stopped before dispatch after prior failure: %w", err))
				drainAfterStop("failure before dispatch")
				return err
			case <-claimCtx.Done():
				acker.fail(msg, fmt.Errorf("claim canceled before dispatch: %w", claimCtx.Err()))
				drainAfterStop("claim canceled before dispatch")
				if session.Context().Err() != nil {
					return nil
				}
				return claimCtx.Err()
			case <-w.ctx.Done():
				acker.fail(msg, fmt.Errorf("worker context canceled before dispatch"))
				drainAfterStop("worker canceled before dispatch")
				logx.Infof("worker context canceled, stop dispatching: topic=%s, partition=%d",
					h.consumer.topic, partition)
				return nil
			}
		}
	}
}

func processDBTask(ctx context.Context, w *worker, task *db_proto.DBTask, partition int32, seq uint64, isOfflineExpand bool) error {
	key := strconv.FormatUint(task.Key, 10)
	logx.Debugf("received db task: taskID=%s, key=%s, partition=%d, seq=%d, isOfflineExpand=%v",
		task.TaskId, key, partition, seq, isOfflineExpand)

	if isOfflineExpand {
		logx.Debugf("offline expand mode: skip lock/status check, taskID=%s", task.TaskId)
		return processTaskWithoutLock(ctx, w.redisClient, task)
	}

	expandStatus, err := kafkautil.GetExpandStatus(ctx, w.redisClient, w.topic)
	if err != nil {
		logx.Errorf("get expand status failed: key=%s, taskID=%s, err=%v", key, task.TaskId, err)
		return tryLockAndProcess(ctx, w, key, task, seq)
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
		return tryLockAndProcess(ctx, w, key, task, seq)
	}

	return processTaskWithoutLock(ctx, w.redisClient, task)
}

const orderingLockTTL = 2 * time.Minute
const orderingLockRenewInterval = 30 * time.Second

var (
	errTaskDeferred     = errors.New("task deferred because per-key expansion lock is occupied")
	errOrderingLockBusy = errors.New("task deferred because cross-instance ordering lock is occupied")
)

func orderingLockKey(topic string, task *db_proto.DBTask) string {
	return fmt.Sprintf("kafka:ordering:%s:%d:%s", topic, task.Key, task.MsgType)
}

type orderingLease struct {
	result *locker.TryLockResult
	stop   chan struct{}
	done   chan struct{}

	mu      sync.Mutex
	lostErr error
}

func (l *orderingLease) setLost(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lostErr == nil {
		l.lostErr = err
	}
}

func (l *orderingLease) lost() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lostErr
}

func (l *orderingLease) verify(ctx context.Context) error {
	if err := l.lost(); err != nil {
		return err
	}
	owned, err := l.result.Extend(ctx, orderingLockTTL)
	if err != nil {
		l.setLost(err)
		return err
	}
	if !owned {
		err = errors.New("ordering lease token is no longer the Redis lock owner")
		l.setLost(err)
		return err
	}
	return nil
}

func acquireOrderingLock(ctx context.Context, w *worker, task *db_proto.DBTask) (*orderingLease, error) {
	result, err := w.locker.TryLock(ctx, orderingLockKey(w.topic, task), orderingLockTTL)
	if err != nil {
		return nil, fmt.Errorf("acquire cross-instance ordering lock: taskID=%s: %w", task.TaskId, err)
	}
	if !result.IsLocked() {
		return nil, fmt.Errorf("%w: taskID=%s key=%d msgType=%s", errOrderingLockBusy, task.TaskId, task.Key, task.MsgType)
	}

	lease := &orderingLease{
		result: result,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(lease.done)
		ticker := time.NewTicker(orderingLockRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-lease.stop:
				return
			case <-w.ctx.Done():
				lease.setLost(w.ctx.Err())
				return
			case <-ticker.C:
				owned, extendErr := result.Extend(w.ctx, orderingLockTTL)
				if extendErr != nil {
					lease.setLost(fmt.Errorf("ordering lease renewal failed: %w", extendErr))
					return
				}
				if !owned {
					lease.setLost(errors.New("ordering lease renewal found a different owner"))
					return
				}
			}
		}
	}()
	return lease, nil
}

func releaseOrderingLock(ctx context.Context, lease *orderingLease, task *db_proto.DBTask) {
	if lease == nil {
		return
	}
	close(lease.stop)
	<-lease.done
	// Release should still run if a Sarama claim context was canceled. The
	// worker context remains alive across rebalances; token-checked Lua prevents
	// this owner from deleting a successor's lock after TTL expiry.
	if _, err := lease.result.Release(ctx); err != nil {
		logx.Errorf("release cross-instance ordering lock failed: taskID=%s key=%d msgType=%s err=%v",
			task.TaskId, task.Key, task.MsgType, err)
	}
}

func tryLockAndProcess(ctx context.Context, worker *worker, key string, task *db_proto.DBTask, seq uint64) error {
	lockKey := fmt.Sprintf("kafka:consumer:lock:%s", key)
	lockTTL := 5 * time.Second

	tryLock, err := worker.locker.TryLock(ctx, lockKey, lockTTL)
	if err != nil {
		return fmt.Errorf("try lock failed: key=%s, taskID=%s, err=%w", key, task.TaskId, err)
	}
	if !tryLock.IsLocked() {
		// 这里只报告 deferred，统一由 handleTask 把 Kafka 原消息持久化到
		// ready queue，或把 processing receipt 原子搬回 ready。返回 nil 会被
		// 误判为已执行成功并 markAppliedSeq，这是已确认的数据丢失根因。
		logx.Debugf("lock occupied: task deferred without marking applied: taskID=%s key=%s seq=%d",
			task.TaskId, key, seq)
		return errTaskDeferred
	}

	defer func() {
		if _, err := tryLock.Release(ctx); err != nil {
			logx.Errorf("release lock failed: key=%s, taskID=%s, err=%v", key, task.TaskId, err)
		}
	}()

	return processTaskWithoutLock(ctx, worker.redisClient, task)
}

func processTaskWithoutLock(ctx context.Context, redisClient redis.Cmdable, task *db_proto.DBTask) error {
	totalStart := time.Now()
	opLabel := task.Op
	if opLabel != "read" && opLabel != "write" {
		opLabel = "unknown"
	}
	defer func() {
		metrics.ObserveStage(metrics.StageOpTotal, opLabel, time.Since(totalStart))
	}()

	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(task.MsgType))
	if err != nil {
		metrics.ObserveResult(opLabel, "error")
		return fmt.Errorf("find message type failed: type=%s, taskID=%s, err=%w", task.MsgType, task.TaskId, err)
	}

	msg := dynamicpb.NewMessage(mt.Descriptor())
	if err := proto.Unmarshal(task.Body, msg); err != nil {
		metrics.ObserveResult(opLabel, "error")
		return fmt.Errorf("unmarshal task body failed: taskID=%s, err=%w", task.TaskId, err)
	}

	handler, ok := dbOpHandlers[task.Op]
	var resultErr string
	if !ok {
		resultErr = fmt.Sprintf("unsupported op: %s", task.Op)
	} else {
		handlerStart := time.Now()
		resultErr = handler(ctx, redisClient, task, msg)
		metrics.ObserveStage(metrics.StageOpHandler, opLabel, time.Since(handlerStart))
	}

	logx.Infof("task processed: taskID=%s, op=%s, success=%v, err=%s",
		task.TaskId, task.Op, resultErr == "", resultErr)

	if resultErr != "" {
		metrics.ObserveResult(opLabel, "error")
		return fmt.Errorf("%s", resultErr)
	}
	metrics.ObserveResult(opLabel, "ok")
	return nil
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
	if task.fromRetry {
		if errors.Is(cause, errOrderingLockBusy) && dbTask.RetryCount > 0 {
			// Lock contention is serialization backpressure, not a failed DB
			// attempt. Do not consume the finite business-retry budget while a
			// prior owner (possibly an old rebalance claim) is still draining.
			dbTask.RetryCount--
		}
		payload, err := encodeRetryPayload(dbTask, task.seq, task.originPartition, task.hasOriginPartition)
		if err == nil {
			var moved bool
			moved, err = moveRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey,
				w.retryQueueKey, task.retryReceipt, payload)
			if err == nil && !moved {
				err = fmt.Errorf("processing receipt not found")
			}
		}
		if err != nil {
			logx.Errorf("retry reschedule failed; receipt remains in processing for recovery: taskID=%s cause=%v err=%v",
				dbTask.TaskId, cause, err)
			return
		}
		logx.Errorf("retry task rescheduled: taskID=%s retryCount=%d cause=%v",
			dbTask.TaskId, dbTask.RetryCount, cause)
		return
	}

	partition := w.partition
	if task.hasOriginPartition {
		partition = task.originPartition
	}
	if err := saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, dbTask, task.seq, partition); err != nil {
		// claimAcker keeps this offset hole open and stops the claim. Higher
		// sub-shard completions cannot advance Sarama's high-water past it.
		fatalErr := fmt.Errorf("task failed and retry enqueue failed: taskID=%s partition=%d seq=%d cause=%v enqueueErr=%w",
			dbTask.TaskId, partition, task.seq, cause, err)
		logx.Errorf("DATA-LOSS PREVENTED: %v", fatalErr)
		failKafkaTask(task, fatalErr)
		return
	}
	// The ready queue is now the durable copy, so this Kafka offset is complete.
	ackKafkaTask(task)
}

func (w *worker) deadLetterRetry(task *workerTask, dbTask *db_proto.DBTask, reason string) error {
	payload, err := encodeRetryPayload(dbTask, task.seq, task.originPartition, task.hasOriginPartition)
	if err != nil {
		return err
	}
	moved, err := moveRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey,
		w.retryDeadQueueKey, task.retryReceipt, payload)
	if err != nil {
		return err
	}
	if !moved {
		return fmt.Errorf("processing receipt not found")
	}
	logx.Errorf("DATA-LOSS/ORDERING: retry quarantined in dead queue: taskID=%s key=%d msgType=%s originKnown=%v originPartition=%d seq=%d reason=%s",
		dbTask.TaskId, dbTask.Key, dbTask.MsgType, task.hasOriginPartition, task.originPartition, task.seq, reason)
	return nil
}

func (w *worker) quarantineOrderingConflict(task *workerTask, dbTask *db_proto.DBTask, reason string) {
	if task.fromRetry {
		if err := w.deadLetterRetry(task, dbTask, reason); err != nil {
			logx.Errorf("DATA-LOSS/ORDERING: retry quarantine failed; receipt remains recoverable: taskID=%s err=%v",
				dbTask.TaskId, err)
		}
		return
	}

	payload, err := encodeRetryPayload(dbTask, task.seq, task.originPartition, task.hasOriginPartition)
	if err == nil {
		err = pushPayloadDurably(w.ctx, w.redisClient, w.retryDeadQueueKey, payload)
	}
	if err != nil {
		fatalErr := fmt.Errorf("DATA-LOSS/ORDERING: fresh Kafka quarantine failed: taskID=%s originPartition=%d seq=%d reason=%s err=%w",
			dbTask.TaskId, task.originPartition, task.seq, reason, err)
		logx.Errorf("%v", fatalErr)
		failKafkaTask(task, fatalErr)
		return
	}

	logx.Errorf("DATA-LOSS/ORDERING: fresh Kafka task durably quarantined: taskID=%s key=%d msgType=%s originPartition=%d seq=%d reason=%s deadQueue=%s",
		dbTask.TaskId, dbTask.Key, dbTask.MsgType, task.originPartition, task.seq, reason, w.retryDeadQueueKey)
	// The offset is safe to acknowledge only after the dead queue owns a copy.
	ackKafkaTask(task)
}

func persistPoisonMessage(ctx context.Context, rc redis.Cmdable, deadQueueKey string, msg *sarama.ConsumerMessage, decodeErr error) error {
	payload, err := encodePoisonMessage(msg, decodeErr)
	if err != nil {
		return err
	}
	return pushPayloadDurably(ctx, rc, deadQueueKey, payload)
}

func (w *worker) quarantinePoisonTask(task *workerTask, decodeErr error) {
	if task == nil || task.kafkaMsg == nil {
		return
	}
	if err := persistPoisonMessage(w.ctx, w.redisClient, w.retryDeadQueueKey, task.kafkaMsg, decodeErr); err != nil {
		fatalErr := fmt.Errorf("poison Kafka DLQ write failed: topic=%s partition=%d offset=%d decodeErr=%v dlqErr=%w",
			task.kafkaMsg.Topic, task.kafkaMsg.Partition, task.kafkaMsg.Offset, decodeErr, err)
		logx.Errorf("DATA-LOSS PREVENTED: %v", fatalErr)
		failKafkaTask(task, fatalErr)
		return
	}
	logx.Errorf("poison Kafka payload durably quarantined: topic=%s partition=%d offset=%d err=%v deadQueue=%s",
		task.kafkaMsg.Topic, task.kafkaMsg.Partition, task.kafkaMsg.Offset, decodeErr, w.retryDeadQueueKey)
	ackKafkaTask(task)
}

func pushPayloadDurably(ctx context.Context, rc redis.Cmdable, queueKey string, payload []byte) error {
	// A DLQ/retry queue is the only durable copy after Kafka ACK. Use bounded
	// backoff; if all writes fail, the claim keeps the offset hole open.
	var lastErr error
	for _, delay := range []time.Duration{0, 100 * time.Millisecond, 500 * time.Millisecond, time.Second} {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		if lastErr = rc.LPush(ctx, queueKey, payload).Err(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func saveToRetryQueue(ctx context.Context, redisClient redis.Cmdable, retryQueueKey string, task *db_proto.DBTask, seq uint64, partition int32) error {
	payload, err := encodeRetryPayload(task, seq, partition, true)
	if err != nil {
		return err
	}
	return pushPayloadDurably(ctx, redisClient, retryQueueKey, payload)
}

func (c *KeyOrderedKafkaConsumer) Stop() {
	c.cancel()
	c.wg.Wait()
	if err := c.consumer.Close(); err != nil {
		logx.Errorf("close consumer group failed: groupID=%s, err=%v", c.groupID, err)
	} else {
		logx.Infof("consumer group closed: groupID=%s, topic=%s", c.groupID, c.topic)
	}
}
