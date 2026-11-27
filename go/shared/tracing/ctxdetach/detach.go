// Package ctxdetach provides an allow-list request context detacher with zero
// broker/transport dependencies, so non-sarama services (e.g. chat, mail, friend)
// can detach background goroutines without pulling in github.com/IBM/sarama.
//
// Ported from xuanming-server/pkg/log/log.go Detach.
package ctxdetach

import (
	"context"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
)

// Detach derives a clean background context from ctx, keeping ONLY allow-listed
// OpenTelemetry trace SpanContext and Baggage while stripping cancellation,
// deadline, gRPC incoming/outgoing metadata, and request-scoped values.
func Detach(ctx context.Context) context.Context {
	out := context.Background()
	if ctx == nil {
		return out
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		out = trace.ContextWithSpanContext(out, sc)
	}
	if b := baggage.FromContext(ctx); b.Len() > 0 {
		out = baggage.ContextWithBaggage(out, b)
	}
	return out
}
