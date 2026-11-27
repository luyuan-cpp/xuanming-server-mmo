// detach.go —— 白名单安全脱离请求上下文（移植自 xuanming-server/pkg/log/log.go Detach）。
//
// 为什么 fire-and-forget 后台协程不能直接用 context.WithoutCancel(ctx)：
//   - context.WithoutCancel 只剥离取消与截止时间，原样保留全部 ctx Value，
//     包括入站 gRPC metadata（如 x-session-detail-bin 鉴权头）、出站 metadata 与
//     grpc.ServerTransportStream；后台协程再发起下游 RPC 时可能污染下游头或与原请求响应并发读写。
//   - Detach 从干净的 context.Background() 起步，仅白名单复制不可变观测上下文
//     （OTel trace.SpanContext 与 baggage），既保留跨进程链路串联与日志 trace_id，
//     又彻底阻断请求级 transport 与鉴权头逃逸。
package tracing

import (
	"context"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
)

// Detach 返回脱离请求取消/截止、且不携带任何 gRPC metadata / server transport 的新背景上下文，
// 仅白名单保留有效的 OpenTelemetry SpanContext 与 Baggage。
func Detach(ctx context.Context) context.Context {
	out := context.Background()
	if ctx == nil {
		return out
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		out = trace.ContextWithSpanContext(out, sc)
	}
	if bg := baggage.FromContext(ctx); bg.Len() > 0 {
		out = baggage.ContextWithBaggage(out, bg)
	}
	return out
}
