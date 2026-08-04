package svc

import (
	"context"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
)

type closeTrackingKafkaWriter struct {
	closeCalls int
}

func (w *closeTrackingKafkaWriter) WriteMessages(context.Context, ...kafka.Message) error {
	return nil
}

func (w *closeTrackingKafkaWriter) Close() error {
	w.closeCalls++
	return nil
}

func TestStopClosesKafkaWriterExactlyOnce(t *testing.T) {
	writer := &closeTrackingKafkaWriter{}
	sc := &ServiceContext{Kafka: writer}

	sc.Stop()
	sc.Stop()

	if writer.closeCalls != 1 {
		t.Fatalf("Kafka Close calls = %d, want 1", writer.closeCalls)
	}
}

func TestKafkaCompletionCountsOnlyTerminalFailures(t *testing.T) {
	sc := &ServiceContext{}
	messages := []kafka.Message{{Topic: "gate-1", Key: []byte("42")}}

	sc.handleKafkaCompletion(messages, nil)
	if got := sc.KafkaDeliveryFailures(); got != 0 {
		t.Fatalf("failures after ack = %d, want 0", got)
	}

	sc.handleKafkaCompletion(messages, errors.New("broker unavailable"))
	if got := sc.KafkaDeliveryFailures(); got != 1 {
		t.Fatalf("failures after terminal error = %d, want 1", got)
	}
}
