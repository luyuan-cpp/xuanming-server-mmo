package kafka

import (
	"context"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/tracing/kafkagotrace"
)

// messageLogger 返回一条消息自己的日志器:消息头带了 traceparent 就挂上(docs/design/distributed-tracing.md §6.2),
// 坏消息的日志能顺着 trace_id 找到发出它的那一端(scene 写存档、写流水的那次请求)。
// 只给逐条的日志用;整批级别的日志(落库、提交)不挂 trace,一批里的消息各属不同请求。
func messageLogger(msg kafkago.Message) logx.Logger {
	return logx.WithContext(kafkagotrace.ContextFrom(context.Background(), msg.Headers))
}
