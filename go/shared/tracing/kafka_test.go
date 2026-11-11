package tracing

import (
	"context"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	kafkago "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func TestKafkaGoHeadersRoundTripTheCallerTrace(t *testing.T) {
	installTestTracing(t)
	ctx, producer := otel.Tracer("test").Start(context.Background(), "produce")
	defer producer.End()

	headers := KafkaGoHeaders(ctx)
	if len(headers) == 0 || headers[0].Key != "traceparent" {
		t.Fatalf("want a traceparent header, got %v", headers)
	}
	consumed := trace.SpanContextFromContext(ContextFromKafkaGo(context.Background(), headers))
	if !consumed.IsValid() || consumed.TraceID() != producer.SpanContext().TraceID() {
		t.Fatalf("consumer side lost the trace: got %v, want trace %s", consumed, producer.SpanContext().TraceID())
	}
}

func TestKafkaGoHeadersAreEmptyWithoutTrace(t *testing.T) {
	installTestTracing(t)
	if headers := KafkaGoHeaders(context.Background()); len(headers) != 0 {
		t.Fatalf("no trace in ctx must add no header, got %v", headers)
	}
}

func TestContextFromKafkaGoIgnoresMissingOrMalformedHeaders(t *testing.T) {
	installTestTracing(t)
	for _, headers := range [][]kafkago.Header{
		nil,
		{{Key: "traceparent", Value: []byte("garbage")}},
		{{Key: "other", Value: []byte("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")}},
	} {
		if got := trace.SpanContextFromContext(ContextFromKafkaGo(context.Background(), headers)); got.IsValid() {
			t.Fatalf("headers %v must not yield a trace, got %v", headers, got)
		}
	}
}

func TestContextFromKafkaGoTakesTheLastTraceparent(t *testing.T) {
	installTestTracing(t)
	headers := []kafkago.Header{
		{Key: "traceparent", Value: []byte("00-11111111111111111111111111111111-1111111111111111-01")},
		{Key: "traceparent", Value: []byte("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")},
	}
	got := trace.SpanContextFromContext(ContextFromKafkaGo(context.Background(), headers))
	if got.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("want the last traceparent, got %v", got)
	}
}

// 消费端拿到的 ctx 交给 logx.WithContext,日志就带上生产端的 trace_id。
func TestContextFromKafkaGoLetsContextLogsCarryTheTrace(t *testing.T) {
	installTestTracing(t)
	collector := logtest.NewCollector(t)
	headers := []kafkago.Header{{Key: "traceparent", Value: []byte("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")}}
	logx.WithContext(ContextFromKafkaGo(context.Background(), headers)).Info("consumed")
	if !strings.Contains(collector.String(), "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("log line misses the producer trace: %s", collector.String())
	}
}

func TestSaramaHeadersCarryTheCallerTrace(t *testing.T) {
	installTestTracing(t)
	ctx, producer := otel.Tracer("test").Start(context.Background(), "produce")
	defer producer.End()

	headers := SaramaHeaders(ctx)
	if len(headers) == 0 || string(headers[0].Key) != "traceparent" {
		t.Fatalf("want a traceparent header, got %v", headers)
	}
	if want := producer.SpanContext().TraceID().String(); !strings.Contains(string(headers[0].Value), want) {
		t.Fatalf("traceparent %q does not carry trace %s", headers[0].Value, want)
	}
	if headers := SaramaHeaders(context.Background()); len(headers) != 0 {
		t.Fatalf("no trace in ctx must add no header, got %v", headers)
	}
}
