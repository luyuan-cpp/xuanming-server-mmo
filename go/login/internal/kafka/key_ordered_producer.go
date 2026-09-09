package kafka

import (
	"context"
	"fmt"
	"login/internal/config"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"login/internal/logic/pkg/consistent"
	db_proto "proto/db"
	"shared/kafkacmd"
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

// KeyOrderedKafkaProducer is a consistent-hash-based ordered Kafka producer using SyncProducer for idempotency.
type KeyOrderedKafkaProducer struct {
	producer     sarama.SyncProducer
	client       sarama.Client
	topic        string
	partitionCnt int
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	consistent   *consistent.Consistent
	closed       bool
	payloadPool  sync.Pool // reuses []byte to reduce GC

	successCount int64
	errorCount   int64
	// routingFenced is irreversible for this process. Broker partition drift
	// changes consistent-hash ownership, so continuing would violate the
	// per-player ordering cursor in db service.
	routingFenced atomic.Bool

	// fault-tolerance state
	unavailablePartitions map[int32]time.Time // partition → time marked unavailable
	retryInterval         time.Duration       // retry interval for unavailable partitions

	// Separate non-transactional producer for cross-topic fire-and-forget sends (e.g. gate commands).
	plainProducer sarama.SyncProducer
}

// NewKeyOrderedKafkaProducer creates an idempotent producer using SyncProducer.
func NewKeyOrderedKafkaProducer(cfg config.KafkaConfig) (*KeyOrderedKafkaProducer, error) {
	config := sarama.NewConfig()
	config.Version = sarama.V3_6_0_0 // requires Kafka >= 0.11.0.0
	config.Net.DialTimeout = cfg.DialTimeout
	config.Net.ReadTimeout = cfg.ReadTimeout
	config.Net.WriteTimeout = cfg.WriteTimeout
	config.Producer.Return.Successes = true
	config.Producer.Return.Errors = true
	config.Producer.Retry.Max = cfg.RetryMax
	config.Producer.Retry.Backoff = cfg.RetryBackoff
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.ChannelBufferSize = cfg.ChannelBuffer
	config.Producer.Compression = cfg.CompressionType
	config.Producer.Idempotent = cfg.Idempotent
	config.Net.MaxOpenRequests = 1 // required by idempotency

	if err := config.Validate(); err != nil {
		logx.Errorf("invalid Kafka config: %v", err)
		return nil, err
	}

	client, err := sarama.NewClient(cfg.Brokers, config)
	if err != nil {
		logx.Errorf("failed to create Kafka client: %v", err)
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	consistentHash := consistent.NewConsistent(20)
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
	for i := int32(0); i < int32(initialPartition); i++ {
		consistentHash.AddPartition(i)
	}

	// Create a separate non-transactional producer for cross-topic fire-and-forget sends.
	plainCfg := sarama.NewConfig()
	plainCfg.Version = sarama.V3_6_0_0
	plainCfg.Net.DialTimeout = cfg.DialTimeout
	plainCfg.Net.ReadTimeout = cfg.ReadTimeout
	plainCfg.Net.WriteTimeout = cfg.WriteTimeout
	plainCfg.Producer.Return.Successes = true
	plainCfg.Producer.Return.Errors = true
	plainCfg.Producer.RequiredAcks = sarama.WaitForAll
	// 控制面命令必须落在 node_id % P 算出来的那一个分区上(消费端只 assign 那一个),
	// 而 sarama 默认的哈希分区器会覆盖 ProducerMessage.Partition。只装在 plainCfg 上:
	// 上面那个 config(db_task 的事务/幂等生产者)一个字都没动,行为不变。
	// 非命令 topic 在 NewCommandAwarePartitioner 里仍然拿默认哈希分区器。
	plainCfg.Producer.Partitioner = NewCommandAwarePartitioner

	plainProducer, err := sarama.NewSyncProducer(cfg.Brokers, plainCfg)
	if err != nil {
		producer.Close()
		client.Close()
		return nil, fmt.Errorf("failed to create plain producer: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	unavailableParts := make(map[int32]time.Time)
	retryInterval := 10 * time.Second

	kp := &KeyOrderedKafkaProducer{
		producer:              producer,
		client:                client,
		topic:                 cfg.Topic,
		partitionCnt:          initialPartition,
		ctx:                   ctx,
		cancel:                cancel,
		consistent:            consistentHash,
		closed:                false,
		unavailablePartitions: unavailableParts,
		retryInterval:         retryInterval,
		plainProducer:         plainProducer,
	}
	kp.payloadPool.New = func() interface{} {
		return make([]byte, 0, 1024)
	}

	// safego:这三条后台循环任意一条 panic,旧写法都会直接打死 login 进程,
	// 而日志里只剩一段 runtime 栈。换成命名点位后,
	// safego_panic_total{point="login.kafka_*"} 能直接指出是哪条。
	safego.Go("login.kafka_sync_partitions", func() { kp.syncPartitions(cfg.SyncInterval) })
	safego.Go("login.kafka_monitor_stats", func() { kp.monitorStats(cfg.StatsInterval) })
	safego.Go("login.kafka_check_unavailable_partitions", kp.checkUnavailablePartitions)

	return kp, nil
}

// SendTasks sends a batch of tasks using SyncProducer.SendMessages.
func (p *KeyOrderedKafkaProducer) SendTasks(ctx context.Context, tasks []*db_proto.DBTask, key string) error {
	if p.routingFenced.Load() {
		return fmt.Errorf("producer routing fenced: Kafka partition contract drifted for topic=%s", p.topic)
	}
	if p.closed {
		return fmt.Errorf("producer closed: batch send failed")
	}
	if len(tasks) == 0 || key == "" {
		return fmt.Errorf("invalid params: taskCount=%d, key=%s", len(tasks), key)
	}

	partition, ok := p.getAvailablePartition(key)
	if !ok {
		return fmt.Errorf("no available partition: key=%s", key)
	}

	var msgs []*sarama.ProducerMessage
	for _, task := range tasks {
		if task.TaskId == "" {
			p.recycleMessages(msgs)
			return fmt.Errorf("empty task ID: %+v", task)
		}

		payload := p.payloadPool.Get().([]byte)
		payload, err := proto.MarshalOptions{}.MarshalAppend(payload[:0], task)
		if err != nil {
			p.payloadPool.Put(payload)
			p.recycleMessages(msgs)
			return fmt.Errorf("failed to marshal task: %s, %w", task.TaskId, err)
		}

		msgs = append(msgs, &sarama.ProducerMessage{
			Topic:     p.topic,
			Key:       sarama.StringEncoder(key),
			Value:     sarama.ByteEncoder(payload),
			Partition: partition,
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
		p.markPartitionUnavailable(partition)
		return fmt.Errorf("batch send timeout: %v", ctx.Err())
	default:
		p.mu.Lock()
		err := p.producer.SendMessages(msgs)
		if err != nil {
			atomic.AddInt64(&p.errorCount, int64(len(msgs)))
			logx.Errorf("batch send failed: partition=%d, err=%v", partition, err)

			if isPartitionUnavailableErr(err) {
				p.markPartitionUnavailable(partition)
			}

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

		p.recycleMessages(msgs)
		atomic.AddInt64(&p.successCount, int64(len(msgs)))
		return nil
	}
}

// SendTask sends a single task using SyncProducer.SendMessage.
func (p *KeyOrderedKafkaProducer) SendTask(ctx context.Context, task *db_proto.DBTask, key string) error {
	if p.routingFenced.Load() {
		return fmt.Errorf("producer routing fenced: Kafka partition contract drifted for topic=%s", p.topic)
	}
	if p.closed {
		return fmt.Errorf("producer closed: task=%s", task.TaskId)
	}
	if task.TaskId == "" || key == "" {
		return fmt.Errorf("invalid params: taskID=%s, key=%s", task.TaskId, key)
	}

	payload := p.payloadPool.Get().([]byte)
	payload, err := proto.MarshalOptions{}.MarshalAppend(payload[:0], task)
	if err != nil {
		p.payloadPool.Put(payload)
		return fmt.Errorf("failed to marshal task: %s, %w", task.TaskId, err)
	}

	partition, ok := p.getAvailablePartition(key)
	if !ok {
		p.payloadPool.Put(payload)
		return fmt.Errorf("no available partition: key=%s, task=%s", key, task.TaskId)
	}

	msg := &sarama.ProducerMessage{
		Topic:     p.topic,
		Key:       sarama.StringEncoder(key),
		Value:     sarama.ByteEncoder(payload),
		Partition: partition,
		Timestamp: time.Now(),
		Metadata: &ProducerMeta{
			producer: p,
			payload:  payload,
		},
	}

	select {
	case <-ctx.Done():
		p.payloadPool.Put(payload)
		p.markPartitionUnavailable(partition)
		return fmt.Errorf("send timeout: task=%s, %v", task.TaskId, ctx.Err())
	default:
		p.mu.Lock()
		_, _, err := p.producer.SendMessage(msg)
		if err != nil {
			atomic.AddInt64(&p.errorCount, 1)
			logx.Errorf("send failed: task=%s, partition=%d, err=%v", task.TaskId, partition, err)

			if isPartitionUnavailableErr(err) {
				p.markPartitionUnavailable(partition)
			}

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
		logx.Debugf("message sent: task=%s, partition=%d", task.TaskId, partition)
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
func (p *KeyOrderedKafkaProducer) SendToTopic(topic string, data []byte, key string) error {
	if kafkacmd.IsCommandTopic(topic) {
		return fmt.Errorf("refusing to send to command topic %s without an explicit partition: use SendToTopicPartition", topic)
	}
	msg := &sarama.ProducerMessage{
		Topic: topic,
		Value: sarama.ByteEncoder(data),
	}
	if key != "" {
		msg.Key = sarama.StringEncoder(key)
	}
	_, _, err := p.plainProducer.SendMessage(msg)
	return err
}

// SendToTopicPartition sends raw bytes to an explicit (topic, partition) using the
// non-transactional producer. This is the control-plane command path.
//
// topic 与分区号是一对不可分的东西(见 shared/kafkacmd):调用方必须由
// kafkacmd 一次性把两者算出来再传进来,不许在这里或别处单独拼其中一个。
// key 仍然填(玩家 id),它不再决定落点,只用于日志与 broker 侧的可读性。
func (p *KeyOrderedKafkaProducer) SendToTopicPartition(topic string, partition int32, data []byte, key string) error {
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
func (p *KeyOrderedKafkaProducer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		logx.Errorf("producer already closed: topic=%s", p.topic)
		return nil
	}

	p.closed = true
	p.cancel()

	var txnErr error
	if p.producer.IsTransactional() {
		txnErr = p.producer.CommitTxn()
	}

	producerErr := p.producer.Close()
	plainErr := p.plainProducer.Close()
	clientErr := p.client.Close()

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
			unavailableCnt := p.getUnavailablePartitionCount()
			logx.Infof("message stats: topic=%s, interval=%v, success=%d, errors=%d, unavailablePartitions=%d",
				p.topic, interval, success, errs, unavailableCnt)
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

// markPartitionUnavailable marks a partition as unavailable.
func (p *KeyOrderedKafkaProducer) markPartitionUnavailable(partition int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unavailablePartitions[partition] = time.Now()
	logx.Errorf("partition marked unavailable: topic=%s, partition=%d, retryInterval=%v",
		p.topic, partition, p.retryInterval)
}

// getAvailablePartition returns an available partition for the given key.
func (p *KeyOrderedKafkaProducer) getAvailablePartition(key string) (int32, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	partition, ok := p.consistent.GetPartition(key)
	if !ok {
		return 0, false
	}

	if _, isUnavailable := p.unavailablePartitions[partition]; !isUnavailable {
		return partition, true
	}

	allPartitions := p.consistent.GetPartitions()
	for _, part := range allPartitions {
		if _, isUnavailable := p.unavailablePartitions[part]; !isUnavailable {
			logx.Errorf("original partition unavailable, remapped: key=%s, from=%d, to=%d",
				key, partition, part)
			return part, true
		}
	}

	return 0, false
}

// checkUnavailablePartitions periodically re-enables recovered partitions.
func (p *KeyOrderedKafkaProducer) checkUnavailablePartitions() {
	ticker := time.NewTicker(p.retryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.mu.Lock()
			now := time.Now()
			for part, unavailableTime := range p.unavailablePartitions {
				if now.Sub(unavailableTime) >= p.retryInterval {
					delete(p.unavailablePartitions, part)
					logx.Infof("partition recovered: topic=%s, partition=%d, downtime=%v",
						p.topic, part, now.Sub(unavailableTime))
				}
			}
			p.mu.Unlock()
		case <-p.ctx.Done():
			logx.Debug("context done: exiting unavailable partition check")
			return
		}
	}
}

// isPartitionUnavailableErr checks if the error indicates a partition is unavailable.
func isPartitionUnavailableErr(err error) bool {
	if prodErr, ok := err.(*sarama.ProducerError); ok {
		errMsg := prodErr.Error()
		if strings.Contains(errMsg, "partition") {
			switch prodErr.Err {
			case sarama.ErrUnknownTopicOrPartition,
				sarama.ErrLeaderNotAvailable,
				sarama.ErrOffsetNotAvailable,
				sarama.ErrReplicaNotAvailable:
				return true
			}
			return strings.Contains(errMsg, "unavailable")
		}
	}

	errMsg := err.Error()
	return strings.Contains(errMsg, "partition") && strings.Contains(errMsg, "unavailable")
}

// getUnavailablePartitionCount returns the number of unavailable partitions.
func (p *KeyOrderedKafkaProducer) getUnavailablePartitionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.unavailablePartitions)
}
