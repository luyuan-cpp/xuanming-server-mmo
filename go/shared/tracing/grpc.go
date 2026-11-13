// Package tracing 收口 Go 侧的链路追踪传播(docs/design/distributed-tracing.md §6)。
//
// go-zero 的 zrpc 服务端和客户端已经按 W3C traceparent 传播,并把 trace / span 写进带 ctx 的日志;
// 这里只补 zrpc 之外的路径:
//   - 裸 grpc.NewClient 的出站拦截(Go 调 C++ 节点等,UnaryClientInterceptor);
//   - 给 handler 日志加 player_id 的服务端拦截器(PlayerFieldInterceptor);
//   - 调我们自己服务的裸 http.Client 出站请求(trade 调商城,DoHTTP,在 http.go)。
//
// Kafka 消息头的载体按客户端库分在子包 kafkagotrace、saramatrace(第 5 批),理由见各子包的包注释。
package tracing

import (
	"context"
	"strconv"

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
//
// 值写成十进制字符串("player_id":"123"),不写 JSON 数字:发号器的 id 可到 63 位,超过 2^53 的数字会被
// 按 float64 解析的下游(Alloy 的 stage.json、jq、浏览器)悄悄改掉末几位;与 AGENTS §7 第 1 条「id 只当字符串」同口径。
// Alloy 只从这种字符串形态取 player_id 进结构化元数据(设计 §9.1)。
func PlayerFieldInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if getter, ok := req.(playerIDGetter); ok {
			if playerID := getter.GetPlayerId(); playerID != 0 {
				ctx = logx.ContextWithFields(ctx, logx.Field("player_id", strconv.FormatUint(playerID, 10)))
			}
		}
		return handler(ctx, req)
	}
}
