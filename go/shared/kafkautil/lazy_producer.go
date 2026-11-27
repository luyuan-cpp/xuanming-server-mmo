// lazy_producer.go —— 双检锁惰性 Kafka 同步生产者
// （移植自 xuanming-server/pkg/kafkax/lazy.go）。
//
// 解决的问题：启动期直接调 sarama.NewSyncProducer 在 broker 瞬时不可达时会让整个服务装配失败，
// 或使弱依赖发布器保持 nil 永久停摆直至重启进程。
//
// LazySyncProducer 将拨号推迟到首次发送，且：
//   - 状态锁 mu 仅保护指针与 closed 标志，绝不持锁做网络拨号；
//   - 拨号锁 dialMu 串行化首次建连，防止并发请求打出大量重复 sarama 客户端；
//   - 拨号失败不缓存失败态，Kafka 恢复后下一次发送自动建连自愈；
//   - 拨号期间被 Close 时立刻关闭新创建的底层生产者，零连接泄漏。
package kafkautil

import (
	"errors"
	"fmt"
	"sync"

	"github.com/IBM/sarama"
)

// ErrLazyProducerClosed 表示惰性生产者已关闭，不再接受任何发送或拨号。
var ErrLazyProducerClosed = errors.New("kafkautil: lazy sync producer is closed")

// SyncProducerDialer 是构造底层 sarama.SyncProducer 的工厂函数。
type SyncProducerDialer func() (sarama.SyncProducer, error)

// LazySyncProducer 并发安全地按需拨号底层 sarama.SyncProducer。
type LazySyncProducer struct {
	dial SyncProducerDialer

	mu     sync.Mutex
	p      sarama.SyncProducer
	closed bool

	dialMu sync.Mutex
}

// NewLazySyncProducer 构造惰性同步生产者；构造期不做任何网络 IO。
func NewLazySyncProducer(dial SyncProducerDialer) (*LazySyncProducer, error) {
	if dial == nil {
		return nil, errors.New("kafkautil: nil sync producer dialer")
	}
	return &LazySyncProducer{dial: dial}, nil
}

// SendMessage 在首次发送时按需拨号底层生产者并发送消息；拨号失败不缓存，下次发送自动重试。
func (l *LazySyncProducer) SendMessage(msg *sarama.ProducerMessage) (partition int32, offset int64, err error) {
	p, err := l.producer()
	if err != nil {
		return -1, -1, err
	}
	return p.SendMessage(msg)
}

func (l *LazySyncProducer) producer() (sarama.SyncProducer, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, ErrLazyProducerClosed
	}
	if l.p != nil {
		p := l.p
		l.mu.Unlock()
		return p, nil
	}
	l.mu.Unlock()

	l.dialMu.Lock()
	defer l.dialMu.Unlock()

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, ErrLazyProducerClosed
	}
	if l.p != nil {
		p := l.p
		l.mu.Unlock()
		return p, nil
	}
	l.mu.Unlock()

	p, err := l.dial()
	if err != nil {
		return nil, fmt.Errorf("kafkautil: lazy producer dial failed: %w", err)
	}
	if p == nil {
		return nil, errors.New("kafkautil: lazy producer dial returned nil producer")
	}

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = p.Close()
		return nil, ErrLazyProducerClosed
	}
	l.p = p
	l.mu.Unlock()
	return p, nil
}

// Close 关闭底层生产者（如已拨号），并阻止后续任何发送或重新拨号。重复调用安全。
func (l *LazySyncProducer) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	p := l.p
	l.p = nil
	l.mu.Unlock()

	if p == nil {
		return nil
	}
	return p.Close()
}
