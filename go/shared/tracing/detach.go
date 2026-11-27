// detach.go —— 白名单异步上下文剥离（移植自 xuanming-server/pkg/log/log.go Detach）。
//
// 委托给零 sarama 依赖的子包 shared/tracing/ctxdetach，保证所有服务统一使用同一份白名单实现。
package tracing

import (
	"context"

	"shared/tracing/ctxdetach"
)

// Detach 返回脱离原请求取消、截止时间、gRPC metadata 与请求级值的新后台上下文，
// 仅按白名单保留 OpenTelemetry SpanContext 与 Baggage。
func Detach(ctx context.Context) context.Context {
	return ctxdetach.Detach(ctx)
}
