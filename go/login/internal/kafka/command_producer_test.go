package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	"login/internal/config"
	"shared/drainproof"
)

func TestNewLazyCommandProducer_NoDialOnConstructAndCleanClose(t *testing.T) {
	p, err := NewLazyCommandProducer(config.KafkaConfig{
		Brokers:      []string{"127.0.0.1:1"},
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLazyCommandProducer must not dial on construct: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close without dial must succeed: %v", err)
	}
	err = p.SendToTopicPartitionContext(context.Background(), "gate-cmd_g1", 0, []byte("x"), "1")
	if !errors.Is(err, drainproof.ErrClosed) {
		t.Fatalf("send after Close = %v, want drainproof.ErrClosed", err)
	}
}
