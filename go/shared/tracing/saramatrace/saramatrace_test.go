package saramatrace

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// 与 C++ 的 kafka_trace_header_test.cpp、kafkagotrace 的用例读同一个字面值。
const crossLanguageTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func useW3CPropagator(t *testing.T) {
	t.Helper()
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previous) })
}

func TestHeadersCarryTheCallerTrace(t *testing.T) {
	useW3CPropagator(t)
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}))

	headers := Headers(ctx)
	if len(headers) != 1 || string(headers[0].Key) != "traceparent" || string(headers[0].Value) != crossLanguageTraceparent {
		t.Fatalf("want exactly traceparent=%s, got %v", crossLanguageTraceparent, headers)
	}
}

func TestHeadersAreEmptyWithoutTrace(t *testing.T) {
	useW3CPropagator(t)
	if headers := Headers(context.Background()); len(headers) != 0 {
		t.Fatalf("no trace in ctx must add no header, got %v", headers)
	}
}
