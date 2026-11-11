// Package tracing 收口 Go 侧的链路追踪传播(docs/design/distributed-tracing.md §6)。
//
// go-zero 的 zrpc 服务端和客户端已经按 W3C traceparent 传播,并把 trace / span 写进带 ctx 的日志;
// 这里只补 zrpc 之外的路径:
//   - 裸 grpc.NewClient 的出站拦截(Go 调 C++ 节点等,UnaryClientInterceptor);
//   - 给 handler 日志加 player_id 的服务端拦截器(PlayerFieldInterceptor)。
//
// Kafka 的载体在该设计 §10.1 第 5 批加。
package tracing

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
	ztrace "github.com/zeromicro/go-zero/core/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// UnaryClientInterceptor 给不经 zrpc 的裸 grpc.NewClient 用:开一个 client span,把 traceparent 写进出站 metadata。
//
// 做法与 go-zero zrpc 客户端的 tracing 拦截器一致:先取出已有的出站 metadata(含 callerauth 的签名头)再追加,
// 已有的键原样保留。拦截器在调用方建好 ctx 之后才执行,所以不会被签名那一步的整体替换冲掉。
// 调用方 ctx 里没有 span 时开新根。传播器与 TracerProvider 用全局的那份:传播器由 go-zero 的 core/trace 在
// init 里设成 W3C TraceContext + Baggage,TracerProvider 由服务的 ServiceConf.SetUp 设置。
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, span := startClientSpan(ctx, method, targetOf(cc))
		defer span.End()
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			if s, ok := status.FromError(err); ok {
				span.SetStatus(codes.Error, s.Message())
			} else {
				span.SetStatus(codes.Error, err.Error())
			}
		}
		return err
	}
}

func startClientSpan(ctx context.Context, method, target string) (context.Context, trace.Span) {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.MD{}
	}
	name, attrs := ztrace.SpanInfo(method, target)
	ctx, span := otel.Tracer(ztrace.TraceName).Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	ztrace.Inject(ctx, otel.GetTextMapPropagator(), &md)
	return metadata.NewOutgoingContext(ctx, md), span
}

func targetOf(cc *grpc.ClientConn) string {
	if cc == nil {
		return ""
	}
	return cc.Target()
}

// playerIDGetter 是带 uint64 player_id 字段的 proto 消息自动生成的取值方法。
type playerIDGetter interface {
	GetPlayerId() uint64
}

// PlayerFieldInterceptor 让 handler 里经 logx.WithContext(ctx) 打的日志带上 player_id(设计 §3.2、§6.2):
// 请求实现了 GetPlayerId() uint64 且值非零时,用 logx.ContextWithFields 加字段。
// 只进日志,不做 Prometheus label(AGENTS §9)。player_id 不在请求顶层的方法不受影响。
func PlayerFieldInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if getter, ok := req.(playerIDGetter); ok {
			if playerID := getter.GetPlayerId(); playerID != 0 {
				ctx = logx.ContextWithFields(ctx, logx.Field("player_id", playerID))
			}
		}
		return handler(ctx, req)
	}
}
