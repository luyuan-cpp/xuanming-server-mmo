package kafka

import (
	"context"
	"crypto/sha256"
	"fmt"
	"login/internal/config"
	"sync"

	"shared/drainproof"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/core/logx"
)

// CommandProducer 发控制面命令(gate-cmd、scene-cmd)。login 只生产不消费,这是它唯一的 Kafka 写路径。
//
// topic 与分区号是一对不可分的东西(见 shared/kafkacmd):调用方必须由 kafkacmd 一次性把两者算出来再传进来。
type CommandProducer struct {
	producer  sarama.SyncProducer
	stateMu   sync.Mutex
	sends     *drainproof.Session
	closeOnce sync.Once
	closeErr  error
	closed    bool
	draining  bool
}

// NewCommandProducer 建同步生产者。分区器是 NewCommandAwarePartitioner:命令必须落在 node_id % P 算出来的那一个分区上,
// sarama 默认的哈希分区器会覆盖 ProducerMessage.Partition。
func NewCommandProducer(cfg config.KafkaConfig) (*CommandProducer, error) {
	saramaCfg := sarama.NewConfig()
	saramaCfg.Metadata.AllowAutoTopicCreation = false
	saramaCfg.Version = sarama.V3_6_0_0
	saramaCfg.Net.DialTimeout = cfg.DialTimeout
	saramaCfg.Net.ReadTimeout = cfg.ReadTimeout
	saramaCfg.Net.WriteTimeout = cfg.WriteTimeout
	saramaCfg.Producer.Return.Successes = true
	saramaCfg.Producer.Return.Errors = true
	saramaCfg.Producer.RequiredAcks = sarama.WaitForAll
	saramaCfg.Producer.Partitioner = NewCommandAwarePartitioner
	if err := saramaCfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid Kafka config: %w", err)
	}
	producer, err := sarama.NewSyncProducer(cfg.Brokers, saramaCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kafka producer: %w", err)
	}
	return &CommandProducer{producer: producer, sends: drainproof.New()}, nil
}

// SendToTopicPartition 把一条命令发到显式的 (topic, partition)。
// key 仍然填(玩家 id),它不决定落点,只用于日志与 broker 侧的可读性。
func (p *CommandProducer) SendToTopicPartition(topic string, partition int32, data []byte, key string) error {
	done, err := p.beginSend()
	if err != nil {
		return err
	}
	defer done()
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
	_, _, err = p.producer.SendMessage(msg)
	p.noteSendError(err)
	return err
}

func (p *CommandProducer) beginSend() (func(), error) {
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

// StopAndReport 先拒绝新发送,等同步发送的真实回执,再关闭生产者,最后返回真实关闭错误。
func (p *CommandProducer) StopAndReport(ctx context.Context) error {
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

func (p *CommandProducer) closeResources() error {
	p.stateMu.Lock()
	p.closed = true
	p.stateMu.Unlock()
	if p.producer == nil {
		return nil
	}
	if err := p.producer.Close(); err != nil {
		logx.Errorf("failed to close Kafka producer: %v", err)
		return fmt.Errorf("failed to close Kafka producer: %w", err)
	}
	return nil
}

// Close 等同 StopAndReport(context.Background())。
func (p *CommandProducer) Close() error { return p.StopAndReport(context.Background()) }

// FrontierDigest 关闭生产者并给出已结算发送的前沿。
func (p *CommandProducer) FrontierDigest(ctx context.Context) ([]byte, error) {
	if err := p.StopAndReport(ctx); err != nil {
		return nil, err
	}
	p.stateMu.Lock()
	sends := p.sends
	p.stateMu.Unlock()
	return sends.SettledDigest(ctx)
}

// ReadyFrontier 给维护新 boot 开放前的就绪证明:生产者没关,以及此刻已接收发送的快照。
func (p *CommandProducer) ReadyFrontier(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.stateMu.Lock()
	if p.sends == nil {
		p.sends = drainproof.New()
	}
	gate, closed := p.sends, p.closed
	p.stateMu.Unlock()
	if closed {
		return nil, drainproof.ErrClosed
	}
	h := sha256.New()
	h.Write([]byte("LOGIN_COMMAND_PRODUCER_READY_V1\x00"))
	h.Write(gate.SnapshotDigest())
	return h.Sum(nil), nil
}

// BeginDrain 之后发送失败会让排空保持 HOLD:命令是否已到 broker 不确定。
func (p *CommandProducer) BeginDrain() {
	p.stateMu.Lock()
	p.draining = true
	p.stateMu.Unlock()
}

func (p *CommandProducer) noteSendError(err error) {
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
