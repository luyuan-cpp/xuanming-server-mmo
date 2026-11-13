package kafkautil

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

// 已接纳记录的死信(handler 模式;设计见 docs/design/kafka-bound-consumer-group.md §5,人工重放见 docs/ops/kafka-bound-consumer-runbook.md)。
//
// 毒丸(解码失败、回调恒失败或恒 panic)以前会让所在分区永远停在那一条上。标准做法是有界重试后转死信:
//   - 回调失败(返回错误、超过 HandlerTimeout、panic)在本 owner 上累计到 MaxAttempts 次,或回调用 %w 返回 ErrUnprocessable
//     (记录本身就处理不了,重试没有意义),就把原件写进死信存储;
//   - 死信写成功才以本代次提交越过它(收据计 Settled,另记 DeadLettered);写失败就留在原处退避重写,不越过、不跳过;
//   - 死信按消费组隔离:一个 topic 可以被几个组读(match-results 有 match-rating 与 guild-trial),某条可能只对其中一个组是毒丸。
//
// 存储选新增的死信 topic `<消费组>-dlq`(DeadLetterTopic),不用各服务自己的库表,理由:
//   - 死信原件本来就是一条 Kafka 记录:key、value、headers 原样保存,重放就是原样写回原 topic 的原分区(cmd/kafka_dlq),
//     不用给每个服务各设计一张表、各写一份迁移与写者闸门(逻辑库自治,AGENTS §7 第 6 条);
//   - 与原 topic 同一套耐久契约(调用方按原 topic 的要求 EnsureTopics:严格耐久时三副本、minISR 2),不依赖消费者自己那个
//     多半正在出故障的库;
//   - 保留 90 天(deadLetterRetentionMs),远长于原 topic 的 7 天:人工处置按周算。
//
// 代价:依赖(库、Redis)长时间不可用时,在途记录各自用完预算后也会转死信(1s 起翻倍、30s 封顶,20 次约 7.5 分钟一条),
// 恢复之后要按手册重放;告警 KafkaBoundConsumerDeadLettered 对第一条就响。

// ErrUnprocessable 由回调用 %w 包住返回:这条记录本身就处理不了(解不出、内容违反契约),重试不会变好。
// 打开了死信的 owner 不再重试、直接转死信;没打开死信的 owner 照常原地重试(match 结果消费:保留位点等人工,负责人 2026-11-06 定)。
var ErrUnprocessable = errors.New("Kafka 记录本身无法处理")

// 死信的原因(kafka_bound_consumer_dead_lettered_total 的 cause label,也写进死信头 dlq.cause)。
const (
	DeadLetterCauseUnprocessable = "unprocessable" // 回调返回 ErrUnprocessable
	DeadLetterCauseExhausted     = "exhausted"     // 回调失败次数用完 MaxAttempts
)

var deadLetterCauses = []string{DeadLetterCauseUnprocessable, DeadLetterCauseExhausted}

// 死信消息的头。key、value 与原 headers 原样保留,本包只追加 dlq.* 这几个头;原件里已有的 dlq.* 头(重放过又进了死信)先去掉。
const (
	DeadLetterHeaderTopic     = "dlq.original.topic"
	DeadLetterHeaderPartition = "dlq.original.partition"
	DeadLetterHeaderOffset    = "dlq.original.offset"
	DeadLetterHeaderTimestamp = "dlq.original.timestamp.ms" // 原件没有时间戳时不写
	DeadLetterHeaderGroup     = "dlq.group"
	DeadLetterHeaderCause     = "dlq.cause"
	DeadLetterHeaderAttempts  = "dlq.attempts"
	DeadLetterHeaderReason    = "dlq.reason"
	DeadLetterHeaderAt        = "dlq.dead.lettered.at.ms"
	deadLetterHeaderPrefix    = "dlq."
)

const (
	// defaultDeadLetterMaxAttempts:回调失败 20 次转死信。按 1s 起翻倍、30s 封顶的退避,前后约 7.5 分钟,
	// 盖得住 TiDB 换主、Redis 重启这类分钟级抖动。
	defaultDeadLetterMaxAttempts = 20
	maxDeadLetterMaxAttempts     = 1000
	// deadLetterRetentionMs:死信 topic 保留 90 天。
	deadLetterRetentionMs = int64(90 * 24 * time.Hour / time.Millisecond)
	// deadLetterWriteTimeout:单次写死信的上限;超时按一次写失败,退避后重写。
	deadLetterWriteTimeout = 30 * time.Second
	// maxDeadLetterReasonBytes:死信头里原因文本的上限(按 UTF-8 字符边界截断)。
	maxDeadLetterReasonBytes = 1024
	deadLetterTopicSuffix    = "-dlq"
	// boundDeadLetterPanicPoint 是写死信 panic 的 safego 点位名(直接成为 label,必须是常量)。
	boundDeadLetterPanicPoint = "kafkautil.bound_consumer.dead_letter"
)

var errBoundDeadLetterPanicked = errors.New("Kafka 写死信 panic(栈见 goroutine_panic 日志),记录留在原处")

// DeadLetterConfig 打开 handler 模式的「有界重试后转死信」。
type DeadLetterConfig struct {
	// Sink 持久化死信原件;WriteDeadLetter 返回 nil 才算写成功(生产用 NewKafkaDeadLetterSink:同步、RequiredAcks=all)。
	Sink DeadLetterSink
	// MaxAttempts 是同一条记录在本 owner 上回调失败多少次(含第一次)后转死信;<= 0 取 20,上限 1000。
	// 只在本 owner 的这一次交付里累计:代次结束交还重投后,新属主从 0 重计(不做跨副本的持久计数)。
	MaxAttempts int
}

// DeadLetterSink 是 owner 对死信存储的全部要求。实现必须在返回 nil 之前把原件持久化(Kafka:acks=all 写成功)。
type DeadLetterSink interface {
	WriteDeadLetter(ctx context.Context, record DeadLetterRecord) error
}

// DeadLetterRecord 是一条死信:原件加失败原因与次数。
type DeadLetterRecord struct {
	Group    string          // 原消费组
	Original kafkago.Message // 原件:Topic / Partition / Offset / Time / Key / Value / Headers
	Cause    string          // DeadLetterCauseUnprocessable | DeadLetterCauseExhausted
	Attempts int             // 回调失败的次数
	Reason   string          // 最后一次失败的原因(写进头时截到 1024 字节)
	At       time.Time       // 写死信的时刻
}

// DeadLetterTopic 是消费组 group 的死信 topic 名。
func DeadLetterTopic(group string) string { return group + deadLetterTopicSuffix }

// DeadLetterTopicSpec 是消费组 group 的死信 topic 契约:1 个分区、保留 90 天;原 topic 要求严格耐久时死信同样三副本、minISR 2。
// 调用方在启动 owner 之前与原 topic 一起 EnsureTopics(写死信的 writer 不自动建 topic)。
func DeadLetterTopicSpec(group string, requireDurability bool) TopicSpec {
	spec := TopicSpec{Name: DeadLetterTopic(group), Partitions: 1, RetentionMs: deadLetterRetentionMs}
	if requireDurability {
		spec.ReplicaFactor = 3
		spec.RequireDurability = true
	}
	return spec
}

// normalizeDeadLetter 校验并补齐死信配置;nil = 不转死信。
func normalizeDeadLetter(config *DeadLetterConfig) (*DeadLetterConfig, error) {
	if config == nil {
		return nil, nil
	}
	if config.Sink == nil {
		return nil, errors.New("Kafka owner 打开了死信却没有死信存储")
	}
	normalized := *config
	if normalized.MaxAttempts <= 0 {
		normalized.MaxAttempts = defaultDeadLetterMaxAttempts
	}
	if normalized.MaxAttempts > maxDeadLetterMaxAttempts {
		return nil, fmt.Errorf("Kafka 死信重试预算 %d 超过上限 %d", normalized.MaxAttempts, maxDeadLetterMaxAttempts)
	}
	return &normalized, nil
}

// deadLetterDue 判这一条是否该转死信:打开了死信、回调已失败过,且判成无法处理或失败次数到了预算。
func (c *BoundConsumerGroup) deadLetterDue(attempts int, last error) bool {
	if c.deadLetter == nil || attempts == 0 {
		return false
	}
	return errors.Is(last, ErrUnprocessable) || attempts >= c.deadLetter.MaxAttempts
}

// writeDeadLetter 写一次死信:单次有界(deadLetterWriteTimeout),不继承代次结束、StopFetching 与 Run ctx 的取消。
// panic 折成本次写失败(记录留在原处)。写成功就记日志与计数:死信里已经有这一条,哪怕随后提交失败、交还重投(重投又转死信时
// 死信里会有两份同一 T/P/O 的原件,重放前按原位置去重,见手册)。
func (c *BoundConsumerGroup) writeDeadLetter(fetchContext context.Context, partition *boundPartition, message kafkago.Message, attempts int, last error) error {
	cause := DeadLetterCauseExhausted
	if errors.Is(last, ErrUnprocessable) {
		cause = DeadLetterCauseUnprocessable
	}
	reason := ""
	if last != nil {
		reason = last.Error()
	}
	record := DeadLetterRecord{Group: c.config.Group.ID, Original: message, Cause: cause, Attempts: attempts, Reason: reason, At: c.now()}
	work, cancel := context.WithTimeout(context.WithoutCancel(fetchContext), deadLetterWriteTimeout)
	defer cancel()
	var err error
	if !safego.Run(boundDeadLetterPanicPoint, func() { err = c.deadLetter.Sink.WriteDeadLetter(work, record) }) {
		return errBoundDeadLetterPanicked
	}
	if err != nil {
		return err
	}
	observeBoundDeadLettered(c.config.Group.ID, cause)
	logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d offset=%d: record written to the group's dead-letter topic after %d failed attempt(s) (cause=%s); committing past it, replay it after fixing the cause (docs/ops/kafka-bound-consumer-runbook.md): %v",
		c.config.Group.ID, partition.topic, partition.partition, message.Offset, attempts, cause, last)
	return nil
}

// DeadLetterMessage 把死信记录编成写进死信 topic 的消息:key、value 原样,原 headers 去掉旧的 dlq.* 之后原样保留,
// 再追加本次的 dlq.* 头。消息时间戳取写死信的时刻(死信 topic 的保留期从它算)。Topic 留空,由写死信的 writer 决定。
func DeadLetterMessage(record DeadLetterRecord) kafkago.Message {
	original := record.Original
	headers := make([]kafkago.Header, 0, len(original.Headers)+9)
	for _, header := range original.Headers {
		if !strings.HasPrefix(header.Key, deadLetterHeaderPrefix) {
			headers = append(headers, header)
		}
	}
	add := func(key, value string) { headers = append(headers, kafkago.Header{Key: key, Value: []byte(value)}) }
	add(DeadLetterHeaderTopic, original.Topic)
	add(DeadLetterHeaderPartition, strconv.Itoa(original.Partition))
	add(DeadLetterHeaderOffset, strconv.FormatInt(original.Offset, 10))
	if !original.Time.IsZero() {
		add(DeadLetterHeaderTimestamp, strconv.FormatInt(original.Time.UnixMilli(), 10))
	}
	add(DeadLetterHeaderGroup, record.Group)
	add(DeadLetterHeaderCause, record.Cause)
	add(DeadLetterHeaderAttempts, strconv.Itoa(record.Attempts))
	add(DeadLetterHeaderReason, truncateDeadLetterReason(record.Reason))
	add(DeadLetterHeaderAt, strconv.FormatInt(record.At.UnixMilli(), 10))
	return kafkago.Message{Key: original.Key, Value: original.Value, Headers: headers, Time: record.At}
}

// ParseDeadLetter 从死信 topic 读到的一条消息还原死信记录:Original 带原 topic / 分区 / offset / 时间 / key / value,
// headers 去掉 dlq.* 之后就是原 headers。缺头、头重复或取值不合法返回 error(不是本包写的死信,不能重放)。
func ParseDeadLetter(message kafkago.Message) (DeadLetterRecord, error) {
	values := map[string]string{}
	var original []kafkago.Header
	for _, header := range message.Headers {
		if !strings.HasPrefix(header.Key, deadLetterHeaderPrefix) {
			original = append(original, header)
			continue
		}
		if _, duplicate := values[header.Key]; duplicate {
			return DeadLetterRecord{}, fmt.Errorf("dead letter header %s repeated", header.Key)
		}
		values[header.Key] = string(header.Value)
	}
	for _, key := range []string{DeadLetterHeaderTopic, DeadLetterHeaderPartition, DeadLetterHeaderOffset, DeadLetterHeaderGroup, DeadLetterHeaderCause, DeadLetterHeaderAttempts, DeadLetterHeaderAt} {
		if _, ok := values[key]; !ok {
			return DeadLetterRecord{}, fmt.Errorf("dead letter header %s missing", key)
		}
	}
	partition, err := strconv.Atoi(values[DeadLetterHeaderPartition])
	if err != nil || partition < 0 {
		return DeadLetterRecord{}, fmt.Errorf("dead letter header %s invalid: %q", DeadLetterHeaderPartition, values[DeadLetterHeaderPartition])
	}
	offset, err := strconv.ParseInt(values[DeadLetterHeaderOffset], 10, 64)
	if err != nil || offset < 0 {
		return DeadLetterRecord{}, fmt.Errorf("dead letter header %s invalid: %q", DeadLetterHeaderOffset, values[DeadLetterHeaderOffset])
	}
	attempts, err := strconv.Atoi(values[DeadLetterHeaderAttempts])
	if err != nil || attempts < 1 {
		return DeadLetterRecord{}, fmt.Errorf("dead letter header %s invalid: %q", DeadLetterHeaderAttempts, values[DeadLetterHeaderAttempts])
	}
	at, err := strconv.ParseInt(values[DeadLetterHeaderAt], 10, 64)
	if err != nil {
		return DeadLetterRecord{}, fmt.Errorf("dead letter header %s invalid: %q", DeadLetterHeaderAt, values[DeadLetterHeaderAt])
	}
	topic, group, cause := values[DeadLetterHeaderTopic], values[DeadLetterHeaderGroup], values[DeadLetterHeaderCause]
	if topic == "" || group == "" || (cause != DeadLetterCauseUnprocessable && cause != DeadLetterCauseExhausted) {
		return DeadLetterRecord{}, fmt.Errorf("dead letter headers invalid: topic=%q group=%q cause=%q", topic, group, cause)
	}
	record := DeadLetterRecord{
		Group:    group,
		Original: kafkago.Message{Topic: topic, Partition: partition, Offset: offset, Key: message.Key, Value: message.Value, Headers: original},
		Cause:    cause,
		Attempts: attempts,
		Reason:   values[DeadLetterHeaderReason],
		At:       time.UnixMilli(at),
	}
	if raw, ok := values[DeadLetterHeaderTimestamp]; ok {
		millis, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return DeadLetterRecord{}, fmt.Errorf("dead letter header %s invalid: %q", DeadLetterHeaderTimestamp, raw)
		}
		record.Original.Time = time.UnixMilli(millis)
	}
	return record, nil
}

// truncateDeadLetterReason 把原因截到 maxDeadLetterReasonBytes 字节以内,不切开 UTF-8 字符。
func truncateDeadLetterReason(reason string) string {
	if len(reason) <= maxDeadLetterReasonBytes {
		return reason
	}
	cut := maxDeadLetterReasonBytes
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

// KafkaDeadLetterSink 把死信写进死信 topic:同步、RequiredAcks=all、不自动建 topic(由调用方 EnsureTopics 预建),
// 单次只尝试一次(重试由 owner 按退避做,失败就留在原处)。
type KafkaDeadLetterSink struct {
	writer *kafkago.Writer
}

// NewKafkaDeadLetterSink 建死信写入端,不拨号(第一次写时才连)。
func NewKafkaDeadLetterSink(brokers []string, topic string) (*KafkaDeadLetterSink, error) {
	if len(brokers) == 0 || strings.TrimSpace(topic) == "" {
		return nil, errors.New("Kafka 死信写入端缺 broker 或 topic")
	}
	return &KafkaDeadLetterSink{writer: &kafkago.Writer{
		Addr:                   kafkago.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafkago.Hash{},
		RequiredAcks:           kafkago.RequireAll,
		Async:                  false,
		MaxAttempts:            1,
		BatchSize:              1,
		BatchTimeout:           10 * time.Millisecond,
		WriteTimeout:           10 * time.Second,
		AllowAutoTopicCreation: false,
	}}, nil
}

// WriteDeadLetter 实现 DeadLetterSink。
func (s *KafkaDeadLetterSink) WriteDeadLetter(ctx context.Context, record DeadLetterRecord) error {
	return s.writer.WriteMessages(ctx, DeadLetterMessage(record))
}

// Close 关掉写入端;owner 的 Run 返回之后再关(之后不会再写死信)。
func (s *KafkaDeadLetterSink) Close() error { return s.writer.Close() }
