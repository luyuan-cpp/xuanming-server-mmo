package kafkagotrace

import (
	"context"
	"strings"
	"testing"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// C++ 的 kafka_trace_header_test.cpp 读同一个字面值,两边对同一组 id 写出的值必须逐字相同。
const crossLanguageTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// 用 W3C 传播器跑用例,结束时恢复全局状态(服务进程里由 go-zero 的 core/trace 在 init 里设置)。
func useW3CPropagator(t *testing.T) {
	t.Helper()
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previous) })
}

// callerContext 返回带固定 trace 的 ctx,不需要 SDK 的 TracerProvider。
func callerContext(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
	caller := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), caller), caller
}

func TestHeadersWriteTheSameValueAsCpp(t *testing.T) {
	useW3CPropagator(t)
	ctx, _ := callerContext(t)

	headers := Headers(ctx)
	if len(headers) != 1 || headers[0].Key != "traceparent" || string(headers[0].Value) != crossLanguageTraceparent {
		t.Fatalf("want exactly traceparent=%s, got %v", crossLanguageTraceparent, headers)
	}
}

func TestHeadersRoundTripTheCallerTrace(t *testing.T) {
	useW3CPropagator(t)
	ctx, caller := callerContext(t)

	consumed := trace.SpanContextFromContext(ContextFrom(context.Background(), Headers(ctx)))
	if !consumed.IsValid() || !consumed.IsRemote() {
		t.Fatalf("consumer side must get a valid remote span context, got %v", consumed)
	}
	if consumed.TraceID() != caller.TraceID() || consumed.SpanID() != caller.SpanID() {
		t.Fatalf("consumer side got %v, want %v", consumed, caller)
	}
}

func TestHeadersAreEmptyWithoutTrace(t *testing.T) {
	useW3CPropagator(t)
	if headers := Headers(context.Background()); len(headers) != 0 {
		t.Fatalf("no trace in ctx must add no header, got %v", headers)
	}
}

func TestContextFromIgnoresMissingOrMalformedHeaders(t *testing.T) {
	useW3CPropagator(t)
	for _, headers := range [][]kafkago.Header{
		nil,
		{{Key: "traceparent", Value: []byte("garbage")}},
		{{Key: "traceparent", Value: []byte("00-00000000000000000000000000000000-00f067aa0ba902b7-01")}},
		{{Key: "other", Value: []byte(crossLanguageTraceparent)}},
	} {
		if got := trace.SpanContextFromContext(ContextFrom(context.Background(), headers)); got.IsValid() {
			t.Fatalf("headers %v must not yield a trace, got %v", headers, got)
		}
	}
}

func TestContextFromTakesTheLastTraceparent(t *testing.T) {
	useW3CPropagator(t)
	headers := []kafkago.Header{
		{Key: "traceparent", Value: []byte("00-11111111111111111111111111111111-1111111111111111-01")},
		{Key: "traceparent", Value: []byte(crossLanguageTraceparent)},
	}
	got := trace.SpanContextFromContext(ContextFrom(context.Background(), headers))
	if got.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("want the last traceparent, got %v", got)
	}
}

// 消费端拿到的 ctx 交给 logx.WithContext,日志就带上生产端的 trace_id(data_service 的逐条日志就这样用)。
func TestContextFromLetsContextLogsCarryTheTrace(t *testing.T) {
	useW3CPropagator(t)
	collector := logtest.NewCollector(t)
	headers := []kafkago.Header{{Key: "traceparent", Value: []byte(crossLanguageTraceparent)}}

	logx.WithContext(ContextFrom(context.Background(), headers)).Info("consumed")
	if !strings.Contains(collector.String(), "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("log line misses the producer trace: %s", collector.String())
	}
}
