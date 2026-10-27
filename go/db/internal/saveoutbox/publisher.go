package saveoutbox

import (
	"context"
	"errors"
	"shared/drainproof"
	"sync"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"shared/kafkautil"
)

// dbTaskBalancer 仅适配Writer接口，分区知识仍由shared/kafkautil独占。
type dbTaskBalancer struct{}

func (dbTaskBalancer) Balance(msg kafkago.Message, partitions ...int) int {
	p, err := kafkautil.DBTaskPartition(msg.Key, int32(len(partitions)))
	if err != nil {
		// Publisher在入队前拒绝空key；Writer只在取到非空分区元数据后调用。
		return -1 // 非法分区让broker拒绝，不能误投0号分区。
	}
	return partitions[int(p)]
}

type kafkaPublisher struct {
	writer     *kafkago.Writer
	transport  *kafkago.Transport
	topic      string
	mu         sync.RWMutex
	historical map[string]bool
	sends      *drainproof.Session
}

func newKafkaPublisher(brokers []string, topic string) (*kafkaPublisher, error) {
	if len(brokers) == 0 || topic == "" {
		return nil, errors.New("save outbox Kafka配置缺失")
	}
	transport := &kafkago.Transport{DialTimeout: time.Second, IdleTimeout: 30 * time.Second,
		MetadataTopics: []string{topic}, ClientID: "db-save-outbox"}
	return &kafkaPublisher{topic: topic, sends: drainproof.New(), transport: transport, writer: &kafkago.Writer{
		Addr: kafkago.TCP(brokers...), Balancer: dbTaskBalancer{}, RequiredAcks: kafkago.RequireAll,
		MaxAttempts: 1, BatchSize: 1, BatchTimeout: time.Millisecond,
		ReadTimeout: time.Second, WriteTimeout: time.Second,
		Transport: transport, AllowAutoTopicCreation: false,
	}}, nil
}

func (p *kafkaPublisher) Publish(ctx context.Context, topic, key string, payload []byte) error {
	p.mu.Lock()
	if p.sends == nil {
		p.sends = drainproof.New()
	}
	sends := p.sends
	allowed := topic == p.topic || p.historical[topic]
	p.mu.Unlock()
	if !allowed || key == "" || len(payload) == 0 {
		return errors.New("save outbox Kafka投递超出当前topic或缺身份")
	}
	done, err := sends.Enter()
	if err != nil {
		return err
	}
	defer done()
	// WriteMessages取消后仍可能晚到；payload不再修改，原意图等SQL双ACK后才清理。
	return p.writer.WriteMessages(ctx, kafkago.Message{Topic: topic, Key: []byte(key), Value: payload})
}

func (p *kafkaPublisher) close() error {
	p.sends.CloseAdmission()
	if err := p.sends.Wait(context.Background()); err != nil {
		return err
	}
	err := p.writer.Close() // 单次尝试、网络超时有界；先由服务取消Run，阻止新入队。
	p.transport.CloseIdleConnections()
	return err
}

// allowHistorical只扩展审定的原topic全集，不改写payload中的身份或世代，也不创建topic。
func (p *kafkaPublisher) allowHistorical(topics []string) error {
	if len(topics) == 0 || len(topics) > 128 {
		return errors.New("历史topic目录为空或超限")
	}
	allowed := map[string]bool{}
	for _, topic := range topics {
		if _, err := topicZone(topic); err != nil || allowed[topic] {
			return errors.New("历史topic目录非法或重复")
		}
		allowed[topic] = true
	}
	p.mu.Lock()
	p.historical = allowed
	p.mu.Unlock()
	return nil
}
