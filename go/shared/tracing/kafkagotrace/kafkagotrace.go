// Package kafkagotrace 在 kafka-go 的消息头上读写链路追踪(docs/design/distributed-tracing.md §5.4、§6.2)。
//
// 键 traceparent,值是 W3C traceparent,与 C++ 的 cpp/libs/engine/infra/messaging/kafka/kafka_trace_header.h
// 同形,三种语言互认。注入与提取都走 otel 全局传播器,与 go-zero 的 gRPC 拦截器同一个口径;服务进程里
// 由 go-zero 的 core/trace 在 init 里设成 W3C TraceContext + Baggage。
//
// 单独成包、不放进 shared/tracing:本包只依赖 kafka-go 与 otel,只做 gRPC 的模块(经 shared/tracing 或
// shared/scenenode)不会因此依赖 Kafka 客户端。sarama 那一份在 shared/tracing/saramatrace,理由相同。
package kafkagotrace

import (
	"context"

	kafkago "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
)

// Headers 返回要写进消息的链路追踪头;ctx 里没有 trace 时返回 nil,消息与改动前相同。
func Headers(ctx context.Context) []kafkago.Header {
	var headers []kafkago.Header
	otel.GetTextMapPropagator().Inject(ctx, carrier{headers: &headers})
	return headers
}

// ContextFrom 返回带上消息头里 trace 的 ctx:合法就作为远端父 span 挂上,之后经 logx.WithContext(ctx)
// 打的日志带同一个 trace_id;没有或不合法时原样返回 ctx,绝不据此丢弃消息。批量消费时每条用自己的 ctx。
func ContextFrom(ctx context.Context, headers []kafkago.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, carrier{headers: &headers})
}

// carrier 把消息头当作 otel 的 TextMapCarrier。同名键取最后一个(与 C++ 侧 get_last 同口径)。
type carrier struct {
	headers *[]kafkago.Header
}

func (c carrier) Get(key string) string {
	headers := *c.headers
	for i := len(headers) - 1; i >= 0; i-- {
		if headers[i].Key == key {
			return string(headers[i].Value)
		}
	}
	return ""
}

func (c carrier) Set(key, value string) {
	*c.headers = append(*c.headers, kafkago.Header{Key: key, Value: []byte(value)})
}

func (c carrier) Keys() []string {
	keys := make([]string, 0, len(*c.headers))
	for _, header := range *c.headers {
		keys = append(keys, header.Key)
	}
	return keys
}
