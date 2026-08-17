package kafka

import (
	"context"
	"fmt"
	"time"

	"shared/kafkautil"
	"shared/safego"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/core/logx"
)

// ExpandMonitor detects external partition drift and fences the producer.
// It deliberately never mutates the hash ring: online expansion cannot
// preserve the db service's per-player partition cursor.
type ExpandMonitor struct {
	client            sarama.Client
	topic             string
	producer          *KeyOrderedKafkaProducer
	checkInterval     time.Duration
	oldPartitionCount int32
	ctx               context.Context
	cancel            context.CancelFunc
}

// NewExpandMonitor creates an ExpandMonitor.
func NewExpandMonitor(
	brokers []string, topic string,
	producer *KeyOrderedKafkaProducer,
	checkInterval time.Duration,
) (*ExpandMonitor, error) {
	config := sarama.NewConfig()
	config.Version = sarama.V3_5_0_0
	client, err := sarama.NewClient(brokers, config)
	if err != nil {
		return nil, fmt.Errorf("create sarama client failed: %w", err)
	}

	oldPartitionCount, err := kafkautil.GetCurrentPartitionCount(client, topic)
	if err != nil {
		return nil, fmt.Errorf("get initial partition count failed: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if checkInterval <= 0 {
		checkInterval = 5 * time.Second
	}

	return &ExpandMonitor{
		client:            client,
		topic:             topic,
		producer:          producer,
		checkInterval:     checkInterval,
		oldPartitionCount: oldPartitionCount,
		ctx:               ctx,
		cancel:            cancel,
	}, nil
}

// Start begins periodic partition monitoring.
func (m *ExpandMonitor) Start() {
	logx.Infof("expand monitor started: topic=%s, checkInterval=%v, initialPartitionCount=%d",
		m.topic, m.checkInterval, m.oldPartitionCount)

	// 裸 go func 换成 safego:panic 不再打死整个 login 进程,而且能从
	// safego_panic_total{point="login.kafka_expand_monitor"} 直接看到是哪条
	// 后台链路炸的。内层 Run 把 recover 收窄到"一轮":某次分区探测炸掉
	// 不影响下一次节拍。
	safego.Go("login.kafka_expand_monitor", func() {
		ticker := time.NewTicker(m.checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-m.ctx.Done():
				logx.Infof("expand monitor stopped: topic=%s", m.topic)
				return
			case <-ticker.C:
				safego.Run("login.kafka_expand_monitor", m.checkAndHandleExpand)
			}
		}
	})
}

// checkAndHandleExpand checks for partition changes and handles expansion.
func (m *ExpandMonitor) checkAndHandleExpand() {
	currentPartitionCount, err := kafkautil.GetCurrentPartitionCount(m.client, m.topic)
	if err != nil {
		logx.Errorf("check partition count failed: topic=%s, err=%v", m.topic, err)
		return
	}

	if currentPartitionCount == m.oldPartitionCount {
		return
	}

	if currentPartitionCount < m.oldPartitionCount {
		logx.Errorf("partition count decreased: topic=%s, old=%d, current=%d",
			m.topic, m.oldPartitionCount, currentPartitionCount)
		return
	}

	logx.Errorf("DATA-ORDERING: detected forbidden live partition change: topic=%s, configured-at-start=%d, current=%d",
		m.topic, m.oldPartitionCount, currentPartitionCount)
	partitions, err := m.client.Partitions(m.topic)
	if err != nil {
		logx.Errorf("read drifted partition IDs failed: topic=%s, err=%v", m.topic, err)
		return
	}
	if err := m.producer.verifyPartitionContract(partitions); err != nil {
		logx.Errorf("%v", err)
	}
	// Avoid logging the same immutable drift every second. The producer fence
	// is irreversible; a process restart still fails the startup contract.
	m.oldPartitionCount = currentPartitionCount
}

// Stop stops the monitor.
func (m *ExpandMonitor) Stop() {
	m.cancel()
	if err := m.client.Close(); err != nil {
		logx.Errorf("close sarama client failed: err=%v", err)
	} else {
		logx.Info("sarama client closed success")
	}
}
