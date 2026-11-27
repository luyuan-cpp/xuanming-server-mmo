package tracing

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

func TestDetachPreservesTraceAndStripsCancelAndMetadata(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Attach incoming/outgoing gRPC metadata (must be stripped).
	parent = metadata.NewIncomingContext(parent, metadata.Pairs("x-session-detail-bin", "secret"))
	parent = metadata.NewOutgoingContext(parent, metadata.Pairs("authorization", "bearer-token"))

	// Attach valid OTel SpanContext and baggage (must be preserved).
	tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	sid, _ := trace.SpanIDFromHex("0123456789abcdef")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
	})
	parent = trace.ContextWithSpanContext(parent, sc)
	member, err := baggage.NewMember("zone", "1")
	if err != nil {
		t.Fatalf("baggage.NewMember: %v", err)
	}
	bg, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage.New: %v", err)
	}
	parent = baggage.ContextWithBaggage(parent, bg)

	detached := Detach(parent)
	cancel()

	if detached.Err() != nil {
		t.Fatalf("detached context must ignore parent cancellation, got %v", detached.Err())
	}
	if _, hasDeadline := detached.Deadline(); hasDeadline {
		t.Fatal("detached context must not inherit parent deadline")
	}
	if _, ok := metadata.FromIncomingContext(detached); ok {
		t.Fatal("detached context leaked incoming gRPC metadata")
	}
	if _, ok := metadata.FromOutgoingContext(detached); ok {
		t.Fatal("detached context leaked outgoing gRPC metadata")
	}
	gotSC := trace.SpanContextFromContext(detached)
	if gotSC.TraceID() != tid || gotSC.SpanID() != sid || !gotSC.IsSampled() {
		t.Fatalf("detached SpanContext = %+v, want tid=%s sid=%s", gotSC, tid, sid)
	}
	if got := baggage.FromContext(detached).Member("zone").Value(); got != "1" {
		t.Fatalf("detached baggage zone = %q, want 1", got)
	}
}

func TestDetachNilContext(t *testing.T) {
	if Detach(nil) == nil {
		t.Fatal("Detach(nil) must return non-nil background context")
	}
}
