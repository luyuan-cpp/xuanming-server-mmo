package kafka

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"login/internal/config"
	"sync"
	"sync/atomic"
	"time"

	db_proto "proto/db"
	"shared/drainproof"
	"shared/kafkacmd"
	"shared/kafkautil"
	"shared/safego"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// ProducerMeta holds message metadata for payload recycling.
type ProducerMeta struct {
	producer *KeyOrderedKafkaProducer
	payload  []byte
}

// KeyOrderedKafkaProducer 是 db_task 读请求的生产者(SyncProducer,幂等)。
// 分区由 kafkautil.NewDBTaskPartitioner 决定:CRC-32(key) mod N,与 C++ scene 写存盘用的
// librdkafka consistent_random 是同一个函数;同一玩家的读与写落在同一分区,go/db 的读后写屏障才成立
// (docs/design/db-task-kafka-partition-contract.md)。
type KeyOrderedKafkaProducer struct {
	producer     sarama.SyncProducer
	client       sarama.Client
	topic        string
	partitionCnt int
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
	payloadPool  sync.Pool // reuses []byte to reduce GC

	successCount int64
	errorCount   int64
	// routingFenced 本进程一旦置位不可逆。broker 分区数漂移会改变 CRC-32(key) mod N 的映射,
	// 继续发送就违反 go/db 按分区记录的 applied cursor 顺序。
	routingFenced atomic.Bool

	// Separate non-transactional producer for cross-topic fire-and-forget sends (e.g. gate commands).
	plainProducer sarama.SyncProducer
	stateMu       sync.Mutex
	sends         *drainproof.Session
	background    sync.WaitGroup
	closeOnce     sync.Once
	closeErr      error
	draining      bool
}

// newDBTaskProducerConfig 建 db_task 主生产者的 sarama 配置。
//
// Partitioner 必须在 sarama.NewClient 之前设:NewSyncProducerFromClient 用的是 client 上的配置。
// sarama 默认的 NewHashPartitioner 是 FNV-1a,与 C++ 不是同一个函数,而且它会覆盖
// ProducerMessage.Partition —— 这就是旧的一致性哈希环与「不可用分区重映射」从来没生效过的原因。
func newDBTaskProducerConfig(cfg config.KafkaConfig) *sarama.Config {
	saramaCfg := sarama.NewConfig()
	saramaCfg.Metadata.AllowAutoTopicCreation = false
	saramaCfg.Version = sarama.V3_6_0_0 // requires Kafka >= 0.11.0.0
	saramaCfg.Net.DialTimeout = cfg.DialTimeout
	saramaCfg.Net.ReadTimeout = cfg.ReadTimeout
	saramaCfg.Net.WriteTimeout = cfg.WriteTimeout
	saramaCfg.Producer.Return.Successes = true
	saramaCfg.Producer.Return.Errors = true
	saramaCfg.Producer.Retry.Max = cfg.RetryMax
	saramaCfg.Producer.Retry.Backoff = cfg.RetryBackoff
	saramaCfg.Producer.RequiredAcks = sarama.WaitForAll
	saramaCfg.ChannelBufferSize = cfg.ChannelBuffer
	saramaCfg.Producer.Compression = cfg.CompressionType
	saramaCfg.Producer.Idempotent = cfg.Idempotent
	saramaCfg.Net.MaxOpenRequests = 1 // required by idempotency
	// db_task 分区契约的唯一来源(go/shared/kafkautil/dbtask_partition.go)。
	saramaCfg.Producer.Partitioner = kafkautil.NewDBTaskPartitioner
	return saramaCfg
}

// NewKeyOrderedKafkaProducer creates an idempotent producer using SyncProducer.
func NewKeyOrderedKafkaProducer(cfg config.KafkaConfig) (*KeyOrderedKafkaProducer, error) {
	saramaCfg := newDBTaskProducerConfig(cfg)
	if err := saramaCfg.Validate(); err != nil {
		logx.Errorf("invalid Kafka config: %v", err)
		return nil, err
	}

	client, err := sarama.NewClient(cfg.Brokers, saramaCfg)
	if err != nil {
		logx.Errorf("failed to create Kafka client: %v", err)
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	initialPartition := cfg.InitialPartition
	if initialPartition <= 0 {
		initialPartition = int(cfg.PartitionCnt)
	}
	if cfg.PartitionCnt <= 0 || initialPartition != int(cfg.PartitionCnt) {
		_ = client.Close()
		return nil, fmt.Errorf("Kafka partition contract mismatch in config: PartitionCnt=%d InitialPartition=%d",
			cfg.PartitionCnt, initialPartition)
	}
	actualPartitions, err := client.Partitions(cfg.Topic)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("read Kafka partition contract for %s: %w", cfg.Topic, err)
	}
	if !partitionsMatchContract(actualPartitions, initialPartition) {
		_ = client.Close()
		return nil, fmt.Errorf("Kafka partition contract drift at startup: topic=%s configured=%d actual=%v; use an offline-drained new TopicGeneration",
			cfg.Topic, initialPartition, actualPartitions)
	}

	producer, err := sarama.NewSyncProducerFromClient(client)
	if err != nil {
		client.Close()
		logx.Errorf("failed to create Kafka sync producer: %v", err)
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	// Create a separate non-transactional producer for cross-topic fire-and-forget sends.
	plainCfg := sarama.NewConfig()
	plainCfg.Metadata.AllowAutoTopicCreation = false
	plainCfg.Version = sarama.V3_6_0_0
	plainCfg.Net.DialTimeout = cfg.DialTimeout
	plainCfg.Net.ReadTimeout = cfg.ReadTimeout
	plainCfg.Net.WriteTimeout = cfg.WriteTimeout
	plainCfg.Producer.Return.Successes = true
	plainCfg.Producer.Return.Errors = true
	plainCfg.Producer.RequiredAcks = sarama.WaitForAll
	// 控制面命令必须落在 node_id % P 算出来的那一个分区上(消费端只 assign 那一个),
	// 而 sarama 默认的哈希分区器会覆盖 ProducerMessage.Partition,所以 plainCfg 装 NewCommandAwarePartitioner。
	// db_task 主生产者(上面的 newDBTaskProducerConfig)装的是分区契约分区器 kafkautil.NewDBTaskPartitioner;
	// plainCfg 对 db_task_zone_ 前缀的 topic 也装契约分区器,而且 SendToTopic / SendToTopicPartition
	// 都拒绝这类 topic。其余非命令 topic 仍用 sarama 默认哈希分区器。
	plainCfg.Producer.Partitioner = NewCommandAwarePartitioner

	plainProducer, err := sarama.NewSyncProducer(cfg.Brokers, plainCfg)
	if err != nil {
		producer.Close()
		client.Close()
		return nil, fmt.Errorf("failed to create plain producer: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	kp := &KeyOrderedKafkaProducer{
		producer:      producer,
		client:        client,
		topic:         cfg.Topic,
		partitionCnt:  initialPartition,
		ctx:           ctx,
		cancel:        cancel,
		closed:        false,
		sends:         drainproof.New(),
		plainProducer: plainProducer,
	}
	kp.payloadPool.New = func() interface{} {
		return make([]byte, 0, 1024)
	}

	// safego:这两条后台循环任意一条 panic,旧写法都会直接打死 login 进程,
	// 而日志里只剩一段 runtime 栈。换成命名点位后,
	// safego_panic_total{point="login.kafka_*"} 能直接指出是哪条。
	kp.background.Add(2)
	safego.Go("login.kafka_sync_partitions", func() { defer kp.background.Done(); kp.syncPartitions(cfg.SyncInterval) })
	safego.Go("login.kafka_monitor_stats", func() { defer kp.background.Done(); kp.monitorStats(cfg.StatsInterval) })

	return kp, nil
}

// SendTasks sends a batch of tasks using SyncProducer.SendMessages.
func (p *KeyOrderedKafkaProducer) SendTasks(ctx context.Context, tasks []*db_proto.DBTask, key string) error {
	done, admissionErr := p.beginSend()
	if admissionErr != nil {
		return admissionErr
	}
	defer func() {
		if fault := recover(); fault != nil {
			p.stateMu.Lock()
			gate := p.sends
			p.stateMu.Unlock()
			gate.Hold()
			done()
			panic(fault)
		}
		done()
	}()
	if p.routingFenced.Load() {
		return fmt.Errorf("producer routing fenced: Kafka partition contract drifted for topic=%s", p.topic)
	}
	if p.isClosed() {
		return fmt.Errorf("producer closed: batch send failed")
	}
	if len(tasks) == 0 || key == "" {
		return fmt.Errorf("invalid params: taskCount=%d, key=%s", len(tasks), key)
	}

	var msgs []*sarama.ProducerMessage
	for _, task := range tasks {
		if task.TaskId == "" {
			p.recycleMessages(msgs)
			return fmt.Errorf("empty task ID: %+v", task)
		}
		// 分区契约:Kafka key 必须是 DBTaskKey(task.Key),否则这条读会落到别的分区,
		// 排不到本玩家待落库的写后面(go/db 的读后写屏障失效)。不符直接拒发。
		if want := kafkautil.DBTaskKey(task.Key); key != want {
			p.recycleMessages(msgs)
			return fmt.Errorf("db_task kafka key %q does not match task key %d (want %q)", key, task.Key, want)
		}
		if task.Op != db_proto.DBOp_DB_OP_READ && task.Op != db_proto.DBOp_DB_OP_WRITE {
			p.recycleMessages(msgs)
			return fmt.Errorf("unsupported db_task op: %d", task.Op)
		}

		payload := p.payloadPool.Get().([]byte)
		payload, err := proto.MarshalOptions{}.MarshalAppend(payload[:0], task)
		if err != nil {
			p.payloadPool.Put(payload)
			p.recycleMessages(msgs)
			return fmt.Errorf("failed to marshal task: %s, %w", task.TaskId, err)
		}

		// 不填 Partition:分区由装在配置上的 kafkautil.NewDBTaskPartitioner 按 key 算,sarama 会回填。
		msgs = append(msgs, &sarama.ProducerMessage{
			Topic:     p.topic,
			Key:       sarama.StringEncoder(key),
			Value:     sarama.ByteEncoder(payload),
			Timestamp: time.Now(),
			Metadata: &ProducerMeta{
				producer: p,
				payload:  payload,
			},
		})
	}

	select {
	case <-ctx.Done():
		p.recycleMessages(msgs)
		return fmt.Errorf("batch send timeout: %v", ctx.Err())
	default:
		p.mu.Lock()
		err := p.producer.SendMessages(msgs)
		if err != nil {
			p.noteSendError(err)
			atomic.AddInt64(&p.errorCount, int64(len(msgs)))
			logx.Errorf("batch send failed: topic=%s, partition=%d, key=%s, count=%d, err=%v",
				p.topic, msgs[0].Partition, key, len(msgs), err)

			if p.producer.IsTransactional() {
				_ = p.producer.AbortTxn()
				if err := p.producer.BeginTxn(); err != nil {
					logx.Errorf("failed to restart transaction: %v", err)
				}
			}
			p.mu.Unlock()
			p.recycleMessages(msgs)
			return err
		}

		if p.producer.IsTransactional() {
			if err := p.producer.CommitTxn(); err != nil {
				_ = p.producer.AbortTxn()
				if err := p.producer.BeginTxn(); err != nil {
					logx.Errorf("failed to restart transaction after commit failure: %v", err)
				}
				p.mu.Unlock()
				return fmt.Errorf("failed to commit transaction: %w", err)
			}
			if err := p.producer.BeginTxn(); err != nil {
				p.mu.Unlock()
				return fmt.Errorf("failed to begin new transaction: %w", err)
			}
		}
		p.mu.Unlock()

		logx.Debugf("batch sent: topic=%s, partition=%d, firstOffset=%d, lastOffset=%d, count=%d",
			p.topic, msgs[0].Partition, msgs[0].Offset, msgs[len(msgs)-1].Offset, len(msgs))
		p.recycleMessages(msgs)
		atomic.AddInt64(&p.successCount, int64(len(msgs)))
		return nil
	}
}

// SendTask sends a single task using SyncProducer.SendMessage.
func (p *KeyOrderedKafkaProducer) SendTask(ctx context.Context, task *db_proto.DBTask, key string) error {
	done, admissionErr := p.beginSend()
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	if p.routingFenced.Load() {
		return fmt.Errorf("producer routing fenced: Kafka partition contract drifted for topic=%s", p.topic)
	}
	if p.isClosed() {
		return fmt.Errorf("producer closed: task=%s", task.TaskId)
	}
	if task.TaskId == "" || key == "" {
		return fmt.Errorf("invalid params: taskID=%s, key=%s", task.TaskId, key)
	}
	// 分区契约:Kafka key 必须是 DBTaskKey(task.Key),理由同 SendTasks。
	if want := kafkautil.DBTaskKey(task.Key); key != want {
		return fmt.Errorf("db_task kafka key %q does not match task key %d (want %q)", key, task.Key, want)
	}
	if task.Op != db_proto.DBOp_DB_OP_READ && task.Op != db_proto.DBOp_DB_OP_WRITE {
		return fmt.Errorf("unsupported db_task op: %d", task.Op)
	}

	payload := p.payloadPool.Get().([]byte)
	payload, err := proto.MarshalOptions{}.MarshalAppend(payload[:0], task)
	if err != nil {
		p.payloadPool.Put(payload)
		return fmt.Errorf("failed to marshal task: %s, %w", task.TaskId, err)
	}

	// 不填 Partition:分区由装在配置上的 kafkautil.NewDBTaskPartitioner 按 key 算,sarama 会回填。
	msg := &sarama.ProducerMessage{
		Topic:     p.topic,
		Key:       sarama.StringEncoder(key),
		Value:     sarama.ByteEncoder(payload),
		Timestamp: time.Now(),
		Metadata: &ProducerMeta{
			producer: p,
			payload:  payload,
		},
	}

	select {
	case <-ctx.Done():
		p.payloadPool.Put(payload)
		return fmt.Errorf("send timeout: task=%s, %v", task.TaskId, ctx.Err())
	default:
		p.mu.Lock()
		partition, offset, err := p.producer.SendMessage(msg)
		if err != nil {
			p.noteSendError(err)
			atomic.AddInt64(&p.errorCount, 1)
			logx.Errorf("send failed: task=%s, topic=%s, partition=%d, err=%v", task.TaskId, p.topic, msg.Partition, err)

			if p.producer.IsTransactional() {
				_ = p.producer.AbortTxn()
				if err := p.producer.BeginTxn(); err != nil {
					logx.Errorf("failed to restart transaction: %v", err)
				}
			}
			p.mu.Unlock()
			p.payloadPool.Put(payload)
			return err
		}

		if p.producer.IsTransactional() {
			if err := p.producer.CommitTxn(); err != nil {
				_ = p.producer.AbortTxn()
				if err := p.producer.BeginTxn(); err != nil {
					logx.Errorf("failed to restart transaction after commit failure: %v", err)
				}
				p.mu.Unlock()
				return fmt.Errorf("failed to commit transaction: %w", err)
			}
			if err := p.producer.BeginTxn(); err != nil {
				p.mu.Unlock()
				return fmt.Errorf("failed to begin new transaction: %w", err)
			}
		}
		p.mu.Unlock()

		p.payloadPool.Put(payload)
		atomic.AddInt64(&p.successCount, 1)
		logx.Debugf("message sent: task=%s, partition=%d, offset=%d", task.TaskId, partition, offset)
		return nil
	}
}

func partitionsMatchContract(partitions []int32, expected int) bool {
	if expected <= 0 || len(partitions) != expected {
		return false
	}
	seen := make([]bool, expected)
	for _, partition := range partitions {
		if partition < 0 || int(partition) >= expected || seen[partition] {
			return false
		}
		seen[partition] = true
	}
	return true
}

func (p *KeyOrderedKafkaProducer) verifyPartitionContract(partitions []int32) error {
	if partitionsMatchContract(partitions, p.partitionCnt) {
		return nil
	}
	p.routingFenced.Store(true)
	return fmt.Errorf("DATA-ORDERING: Kafka partition contract drifted: topic=%s configured=%d actual=%v; producer permanently fenced until an offline-drained TopicGeneration switch",
		p.topic, p.partitionCnt, partitions)
}

// SendToTopic sends raw bytes to an arbitrary Kafka topic using the non-transactional producer.
// When key is non-empty, messages with the same key are routed to the same partition.
//
// 控制面命令 topic 走不了这条路:它的分区号必须是 node_id % P 显式算出来的,
// 从这里发会带着零值 Partition 落到 0 号分区 —— 0 号分区上坐着 node_id 是 P 的
// 倍数的那些节点,它们读到后按 target_instance_id 丢弃,等于静默丢命令。
// 所以这里 fail-closed,逼调用方走 SendToTopicPartition。
//
// db_task topic 同样拒绝:db_task 只能经 SendTask / SendTasks 发,那里校验 key 必须是
// DBTaskKey(task.Key),分区由契约分区器决定;从这个通用口子发出去的 key 不受校验,
// 同一玩家的读可能落到别的分区,go/db 的读后写屏障随之失效。
func (p *KeyOrderedKafkaProducer) SendToTopic(topic string, data []byte, key string) error {
	done, admissionErr := p.beginSend()
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	if kafkacmd.IsCommandTopic(topic) {
		return fmt.Errorf("refusing to send to command topic %s without an explicit partition: use SendToTopicPartition", topic)
	}
	if kafkautil.IsDBTaskTopic(topic) {
		return fmt.Errorf("refusing to send db_task topic %s through the plain producer: use SendTask/SendTasks (partition contract)", topic)
	}
	msg := &sarama.ProducerMessage{
		Topic: topic,
		Value: sarama.ByteEncoder(data),
	}
	if key != "" {
		msg.Key = sarama.StringEncoder(key)
	}
	_, _, err := p.plainProducer.SendMessage(msg)
	p.noteSendError(err)
	return err
}

// SendToTopicPartition sends raw bytes to an explicit (topic, partition) using the
// non-transactional producer. This is the control-plane command path.
//
// topic 与分区号是一对不可分的东西(见 shared/kafkacmd):调用方必须由
// kafkacmd 一次性把两者算出来再传进来,不许在这里或别处单独拼其中一个。
// key 仍然填(玩家 id),它不再决定落点,只用于日志与 broker 侧的可读性。
//
// db_task topic 在这里也拒绝:plainProducer 对 db_task_zone_ 前缀装的是契约分区器,
// 会按 key 覆盖调用方给的显式分区,"指定分区"在这类 topic 上本来就不成立;db_task 只能经 SendTask / SendTasks 发。
func (p *KeyOrderedKafkaProducer) SendToTopicPartition(topic string, partition int32, data []byte, key string) error {
	done, admissionErr := p.beginSend()
	if admissionErr != nil {
		return admissionErr
	}
	defer done()
	if kafkautil.IsDBTaskTopic(topic) {
		return fmt.Errorf("refusing to send db_task topic %s through the plain producer: use SendTask/SendTasks (partition contract)", topic)
	}
	if partition < 0 {
		return fmt.Errorf("send to %s rejected: negative partition %d", topic, partition)
	}
	msg := &sarama.ProducerMessage{
		Topic:     topic,
		Partition: partition,
		Value:     sarama.ByteEncoder(data),
	}
	if key != "" {
		msg.Key = sarama.StringEncoder(key)
	}
	_, _, err := p.plainProducer.SendMessage(msg)
	return err
}

// Close gracefully shuts down the producer with transaction commit.
func (p *KeyOrderedKafkaProducer) closeResources() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isClosed() {
		logx.Errorf("producer already closed: topic=%s", p.topic)
		return nil
	}

	p.stateMu.Lock()
	p.closed = true
	p.stateMu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	p.background.Wait()

	var txnErr error
	if p.producer != nil && p.producer.IsTransactional() {
		txnErr = p.producer.CommitTxn()
	}

	var producerErr, plainErr, clientErr error
	if p.producer != nil {
		producerErr = p.producer.Close()
	}
	if p.plainProducer != nil {
		plainErr = p.plainProducer.Close()
	}
	if p.client != nil {
		clientErr = p.client.Close()
	}

	var finalErr error
	if txnErr != nil {
		finalErr = fmt.Errorf("failed to commit transaction: %w", txnErr)
	}
	if producerErr != nil {
		if finalErr == nil {
			finalErr = fmt.Errorf("failed to close producer: %w", producerErr)
		} else {
			finalErr = fmt.Errorf("%v; failed to close producer: %w", finalErr, producerErr)
		}
	}
	if plainErr != nil {
		if finalErr == nil {
			finalErr = fmt.Errorf("failed to close plain producer: %w", plainErr)
		} else {
			finalErr = fmt.Errorf("%v; failed to close plain producer: %w", finalErr, plainErr)
		}
	}
	if clientErr != nil {
		if finalErr == nil {
			finalErr = fmt.Errorf("failed to close client: %w", clientErr)
		} else {
			finalErr = fmt.Errorf("%v; failed to close client: %w", finalErr, clientErr)
		}
	}

	if finalErr != nil {
		logx.Errorf("failed to close resources: topic=%s, err=%v", p.topic, finalErr)
		return finalErr
	}
	logx.Infof("producer closed: topic=%s", p.topic)
	return nil
}

// syncPartitions periodically syncs actual Kafka partitions.
func (p *KeyOrderedKafkaProducer) syncPartitions(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			partitions, err := p.client.Partitions(p.topic)
			if err != nil {
				logx.Errorf("failed to get Kafka partitions: topic=%s, err=%v", p.topic, err)
				continue
			}
			if err := p.verifyPartitionContract(partitions); err != nil {
				logx.Errorf("%v", err)
			}
		case <-p.ctx.Done():
			logx.Debug("context done: exiting partition sync")
			return
		}
	}
}

// monitorStats periodically logs message statistics.
func (p *KeyOrderedKafkaProducer) monitorStats(interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			success := atomic.SwapInt64(&p.successCount, 0)
			errs := atomic.SwapInt64(&p.errorCount, 0)
			logx.Infof("message stats: topic=%s, interval=%v, success=%d, errors=%d",
				p.topic, interval, success, errs)
		case <-p.ctx.Done():
			logx.Debug("context done: exiting stats monitor")
			return
		}
	}
}

// recycleMessages returns message payloads to the pool.
func (p *KeyOrderedKafkaProducer) recycleMessages(msgs []*sarama.ProducerMessage) {
	for _, msg := range msgs {
		if meta, ok := msg.Metadata.(*ProducerMeta); ok {
			meta.producer.payloadPool.Put(meta.payload)
		}
	}
}

func (p *KeyOrderedKafkaProducer) beginSend() (func(), error) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.sends == nil {
		p.sends = drainproof.New()
	}
	if p.closed {
		return nil, drainproof.ErrClosed
	}
	return p.sends.Enter()
}
func (p *KeyOrderedKafkaProducer) isClosed() bool {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.closed
}

// StopAndReport先拒绝新send，等同步发送的真实回执，再flush/close两个producer及客户端，最后返回真实关闭错误。
func (p *KeyOrderedKafkaProducer) StopAndReport(ctx context.Context) error {
	p.stateMu.Lock()
	if p.sends == nil {
		p.sends = drainproof.New()
	}
	sends := p.sends
	sends.CloseAdmission()
	p.stateMu.Unlock()
	if err := sends.Wait(ctx); err != nil {
		return err
	}
	p.closeOnce.Do(func() { p.closeErr = p.closeResources() })
	return p.closeErr
}
func (p *KeyOrderedKafkaProducer) Close() error { return p.StopAndReport(context.Background()) }

func (p *KeyOrderedKafkaProducer) FrontierDigest(ctx context.Context) ([]byte, error) {
	if err := p.StopAndReport(ctx); err != nil {
		return nil, err
	}
	p.stateMu.Lock()
	sends := p.sends
	p.stateMu.Unlock()
	return sends.SettledDigest(ctx)
}

func (p *KeyOrderedKafkaProducer) ReadyFrontier(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.isClosed() {
		return nil, drainproof.ErrClosed
	}
	if err := p.client.RefreshMetadata(p.topic); err != nil {
		return nil, err
	}
	partitions, err := p.client.Partitions(p.topic)
	if err != nil {
		return nil, err
	}
	if err = p.verifyPartitionContract(partitions); err != nil {
		return nil, err
	}
	p.stateMu.Lock()
	if p.sends == nil {
		p.sends = drainproof.New()
	}
	gate := p.sends
	p.stateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(struct {
		Topic      string
		Partitions []int32
		Accepted   []byte
	}{p.topic, partitions, gate.SnapshotDigest()})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func (p *KeyOrderedKafkaProducer) BeginDrain() {
	p.stateMu.Lock()
	p.draining = true
	p.stateMu.Unlock()
}
func (p *KeyOrderedKafkaProducer) noteSendError(err error) {
	if err == nil {
		return
	}
	p.stateMu.Lock()
	draining, gate := p.draining, p.sends
	p.stateMu.Unlock()
	if draining && gate != nil {
		gate.Hold()
	}
}

// RunJoined只改变循环的owner，不复制分区校验；ServiceContext在调用前Add，返回后Done。
// 原Start无可join句柄，维护启动改由该显式调用面拥有goroutine。
func (m *ExpandMonitor) RunJoined(ctx context.Context) {
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			safego.Run("login.kafka_expand_monitor", m.checkAndHandleExpand)
		}
	}
}
