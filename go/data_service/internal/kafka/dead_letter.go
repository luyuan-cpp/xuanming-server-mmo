package kafka

import (
	"context"
	"errors"
	"slices"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/kafkautil"
)

// 审计消费者的坏消息转死信(2026-11-26;设计 docs/design/kafka-bound-consumer-group.md §5.5、§15.6)。
//
// 解不出、内容违反契约(主键为 0)或超过列宽的审计消息不会落库。以前只记日志与计数就提交越过,原件只在原 topic 的保留期内取得到;
// 现在提交之前先把原件写进本消费组的死信 topic「消费组名-dlq」(与 guild-trial 同一种格式、同一个重放工具 cmd/kafka_dlq),
// 写成功才提交。写不进(Kafka 故障、死信 topic 没建成)有界重试,仍不行就整批不提交、消费者停下,与落库失败同一个口径:宁可积压,
// 不丢原件。一批里先写死信、再落库、再提交:之后不管提交的是整批还是前缀,提交越过的坏消息都已经在死信里。

const (
	// deadLetterWriteAttempts / deadLetterRetryBackoff:一条坏消息写死信的有界重试(含第一次)。
	deadLetterWriteAttempts = 5
	deadLetterRetryBackoff  = time.Second
)

// ErrConsumerStoppedOnDeadLetter 是坏消息写不进死信、重试用尽而停下时返回的错误(可 errors.Is):这一批一条都没提交,
// 与 ErrConsumerStoppedOnDB 一样按「确定未提交」交还重投,supervisor 冷却后从上次提交处续。
var ErrConsumerStoppedOnDeadLetter = errors.New("kafka consumer stopped: dead-letter topic unavailable, offsets not committed")

// badMessage 是一条要转死信的坏消息:原件(Key / Value / Headers 深拷贝,reader 会复用缓冲)与原因。
type badMessage struct {
	original kafkago.Message
	reason   string
}

func newBadMessage(msg kafkago.Message, reason string) badMessage {
	msg.Key = slices.Clone(msg.Key)
	msg.Value = slices.Clone(msg.Value)
	headers := make([]kafkago.Header, len(msg.Headers))
	for i, header := range msg.Headers {
		headers[i] = kafkago.Header{Key: header.Key, Value: slices.Clone(header.Value)}
	}
	msg.Headers = headers
	return badMessage{original: msg, reason: reason}
}

// deadLetterTarget 是一条审计消费者的死信出口。sink 为 nil = 不转死信,只有单测这样构造;生产装配(Start)总会给。
type deadLetterTarget struct {
	sink  kafkautil.DeadLetterSink
	group string
	// attempts / backoff 为 0 取 deadLetterWriteAttempts / deadLetterRetryBackoff(单测设小值,不等墙钟)。
	attempts int
	backoff  time.Duration
}

// write 把一批里的坏消息按顺序逐条写进死信,每条有界重试。ctx 结束立即返回 ctx 的错误(调用方据此安静退出,不算停机)。
func (t deadLetterTarget) write(ctx context.Context, consumer string, bad []badMessage) error {
	if t.sink == nil || len(bad) == 0 {
		return nil
	}
	attempts, backoff := t.attempts, t.backoff
	if attempts <= 0 {
		attempts = deadLetterWriteAttempts
	}
	if backoff <= 0 {
		backoff = deadLetterRetryBackoff
	}
	for _, message := range bad {
		record := kafkautil.DeadLetterRecord{Group: t.group, Original: message.original, Cause: kafkautil.DeadLetterCauseUnprocessable,
			Attempts: 1, Reason: message.reason, At: time.Now()}
		var err error
		for attempt := 1; attempt <= attempts; attempt++ {
			if err = t.sink.WriteDeadLetter(ctx, record); err == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logx.Errorf("[%s] writing a bad message to the dead-letter topic %s failed (attempt %d/%d) partition=%d offset=%d: %v",
				consumer, kafkautil.DeadLetterTopic(t.group), attempt, attempts, message.original.Partition, message.original.Offset, err)
			if attempt < attempts && !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
		}
		if err != nil {
			return err
		}
		logx.Errorf("[%s] bad message written to the dead-letter topic %s partition=%d offset=%d (%s); committing past it, replay it with cmd/kafka_dlq after fixing the cause",
			consumer, kafkautil.DeadLetterTopic(t.group), message.original.Partition, message.original.Offset, message.reason)
	}
	return nil
}
