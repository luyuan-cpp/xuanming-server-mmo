package tracing

import (
	"context"

	"github.com/IBM/sarama"
	kafkago "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
)

// Kafka 消息头上的链路追踪(docs/design/distributed-tracing.md §5.4、§6.2):键 traceparent,值是 W3C traceparent,
// 与 C++ 的 cpp/libs/engine/infra/messaging/kafka/kafka_trace_header.h 同形,三种语言互认。
// 注入与提取都走全局传播器(go-zero 的 core/trace 在 init 里设成 W3C TraceContext + Baggage)。

// KafkaGoHeaders 返回要写进 kafka-go 消息的链路追踪头;ctx 里没有 trace 时返回 nil,消息与改动前相同。
func KafkaGoHeaders(ctx context.Context) []kafkago.Header {
	var headers []kafkago.Header
	otel.GetTextMapPropagator().Inject(ctx, kafkaGoCarrier{headers: &headers})
	return headers
}

// ContextFromKafkaGo 返回带上消息头里 trace 的 ctx:合法就作为远端父 span 挂上,之后经 logx.WithContext(ctx)
// 打的日志带同一个 trace_id;没有或不合法时原样返回 ctx,绝不据此丢弃消息。批量消费时每条用自己的 ctx。
func ContextFromKafkaGo(ctx context.Context, headers []kafkago.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, kafkaGoCarrier{headers: &headers})
}

// SaramaHeaders 返回要写进 sarama 生产消息的链路追踪头;ctx 里没有 trace 时返回 nil。
func SaramaHeaders(ctx context.Context) []sarama.RecordHeader {
	var headers []sarama.RecordHeader
	otel.GetTextMapPropagator().Inject(ctx, saramaCarrier{headers: &headers})
	return headers
}

// kafkaGoCarrier 把 kafka-go 的消息头当作 otel 的 TextMapCarrier。同名键取最后一个(与 C++ 侧 get_last 同口径)。
type kafkaGoCarrier struct {
	headers *[]kafkago.Header
}

func (c kafkaGoCarrier) Get(key string) string {
	headers := *c.headers
	for i := len(headers) - 1; i >= 0; i-- {
		if headers[i].Key == key {
			return string(headers[i].Value)
		}
	}
	return ""
}

func (c kafkaGoCarrier) Set(key, value string) {
	*c.headers = append(*c.headers, kafkago.Header{Key: key, Value: []byte(value)})
}

func (c kafkaGoCarrier) Keys() []string {
	keys := make([]string, 0, len(*c.headers))
	for _, header := range *c.headers {
		keys = append(keys, header.Key)
	}
	return keys
}

// saramaCarrier 把 sarama 的生产消息头当作 otel 的 TextMapCarrier。同名键取最后一个。
type saramaCarrier struct {
	headers *[]sarama.RecordHeader
}

func (c saramaCarrier) Get(key string) string {
	headers := *c.headers
	for i := len(headers) - 1; i >= 0; i-- {
		if string(headers[i].Key) == key {
			return string(headers[i].Value)
		}
	}
	return ""
}

func (c saramaCarrier) Set(key, value string) {
	*c.headers = append(*c.headers, sarama.RecordHeader{Key: []byte(key), Value: []byte(value)})
}

func (c saramaCarrier) Keys() []string {
	keys := make([]string, 0, len(*c.headers))
	for _, header := range *c.headers {
		keys = append(keys, string(header.Key))
	}
	return keys
}
