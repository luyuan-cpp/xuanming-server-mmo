// Package saramatrace 在 sarama 生产消息的消息头上写链路追踪(docs/design/distributed-tracing.md §5.4、§6.2)。
//
// 键 traceparent,值是 W3C traceparent,与 C++ 的 kafka_trace_header.h、kafka-go 的 shared/tracing/kafkagotrace
// 同形。注入走 otel 全局传播器(服务进程里由 go-zero 的 core/trace 在 init 里设成 W3C TraceContext + Baggage)。
//
// 单独成包:本包只依赖 sarama 与 otel,不用 sarama 的模块不会因此依赖它。本仓只有 login 用 sarama,而且只生产,
// 所以只提供注入;要消费 sarama 消息时再按 kafkagotrace.ContextFrom 的口径补提取。
package saramatrace

import (
	"context"

	"github.com/IBM/sarama"
	"go.opentelemetry.io/otel"
)

// Headers 返回要写进 ProducerMessage.Headers 的链路追踪头;ctx 里没有 trace 时返回 nil,消息与改动前相同。
func Headers(ctx context.Context) []sarama.RecordHeader {
	var headers []sarama.RecordHeader
	otel.GetTextMapPropagator().Inject(ctx, carrier{headers: &headers})
	return headers
}

// carrier 把生产消息头当作 otel 的 TextMapCarrier。同名键取最后一个。
type carrier struct {
	headers *[]sarama.RecordHeader
}

func (c carrier) Get(key string) string {
	headers := *c.headers
	for i := len(headers) - 1; i >= 0; i-- {
		if string(headers[i].Key) == key {
			return string(headers[i].Value)
		}
	}
	return ""
}

func (c carrier) Set(key, value string) {
	*c.headers = append(*c.headers, sarama.RecordHeader{Key: []byte(key), Value: []byte(value)})
}

func (c carrier) Keys() []string {
	keys := make([]string, 0, len(*c.headers))
	for _, header := range *c.headers {
		keys = append(keys, string(header.Key))
	}
	return keys
}
