package auditoutbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"

	"db/internal/playerstore"

	storepb "proto/player_store"
)

const (
	// txlogBatchBytes 是流水 topic 一次请求的上限,等于 broker 缺省的 max.message.bytes(约 1 MiB):
	// 流水 topic 不设 topic 级上限(go/data_service/internal/kafka/start.go),批再大 broker 会整批拒收。
	txlogBatchBytes = 1 << 20
	// snapshotBatchBytes 等于快照 topic 的 max.message.bytes(data_service 的 snapshotTopicMaxMessageBytes)。
	snapshotBatchBytes = 16 << 20
	// messageOverheadBytes 给 key、消息头和记录格式留的余量。单条超过「批上限 − 余量」的行不发,
	// 否则 kafka-go 会把整次调用都拒掉(MessageTooLargeError),一条坏行拖住整批。
	messageOverheadBytes = 1 << 10
)

// errRowTooLarge:这一行装不进一次请求,永远投不出去。行留在发件箱,积压年龄告警会响,由人处置。
// 正常写入不会产生这样的行:流水单条上限 64 KiB、快照 4 MiB(playerstore 的 MaxAuditTransactionLogBytes、MaxAuditSnapshotBytes)。
var errRowTooLarge = errors.New("audit outbox row is larger than one Kafka request")

// KafkaProducer 是 Producer 的生产实现(设计 §7.3):两个审计 topic 各一个同步 kafka-go Writer。
//   - acks=all:broker 全部同步副本确认才算投成,确认之后转发循环才删行;
//   - key = player_id 的十进制串、分区算法 CRC32(与 C++ 生产者 librdkafka 的 consistent_random 相同):
//     同一玩家的流水不管由谁投都落同一个分区(AGENTS.md §7 第 3 条);
//   - 不自动建 topic:topic 由 data_service 的 EnsureTopics 与 kafka-topic-init 按分区契约预建,
//     裸名字被自动建成 1 分区会让消费端的分区契约永久失配。
//
// 并发安全(kafka-go Writer 本身并发安全),但转发循环只在一个 goroutine 里用它。
type KafkaProducer struct {
	writers [kindCount]*kafka.Writer
	limits  [kindCount]int
}

// NewKafkaProducer 构造生产者。topic 名是有效名(带世代后缀,config.AuditOutboxConfig.EffectiveTransactionLogTopic 等)。
func NewKafkaProducer(brokers []string, transactionLogTopic, snapshotTopic string) (*KafkaProducer, error) {
	if len(brokers) == 0 {
		return nil, errors.New("auditoutbox: no Kafka brokers")
	}
	if transactionLogTopic == "" || snapshotTopic == "" || transactionLogTopic == snapshotTopic {
		return nil, fmt.Errorf("auditoutbox: invalid topics %q / %q", transactionLogTopic, snapshotTopic)
	}
	p := &KafkaProducer{}
	for i, kind := range relayKinds {
		topic, batchBytes := transactionLogTopic, int64(txlogBatchBytes)
		if kind == storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_PLAYER_SNAPSHOT {
			topic, batchBytes = snapshotTopic, snapshotBatchBytes
		}
		p.writers[i] = newWriter(brokers, topic, batchSize(kind), batchBytes)
		p.limits[i] = int(batchBytes) - messageOverheadBytes
	}
	return p, nil
}

func newWriter(brokers []string, topic string, batch int, batchBytes int64) *kafka.Writer {
	return &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Topic:        topic,
		Balancer:     &kafka.CRC32Balancer{},
		RequiredAcks: kafka.RequireAll,
		Async:        false,
		// 投一批的总时限由调用方的 ctx 给(relay.go 的 produceTimeout);这里只限 kafka-go 内部重试几次。
		MaxAttempts: 3,
		BatchSize:   batch,
		BatchBytes:  batchBytes,
		// 同步写:一批不满也立刻发。kafka-go 缺省会等 1 秒凑批,每批都多等 1 秒。
		BatchTimeout:           5 * time.Millisecond,
		WriteTimeout:           10 * time.Second,
		ReadTimeout:            10 * time.Second,
		Compression:            kafka.Lz4,
		AllowAutoTopicCreation: false,
	}
}

// Produce 实现 Producer:返回与 rows 下标对齐的结果。
func (p *KafkaProducer) Produce(ctx context.Context, kind storepb.PlayerAuditKind, rows []Row) []error {
	errs := make([]error, len(rows))
	index := kindIndex(kind)
	if index < 0 {
		fill(errs, fmt.Errorf("auditoutbox: unknown audit kind %d", kind))
		return errs
	}
	writer, limit := p.writers[index], p.limits[index]
	messages := make([]kafka.Message, 0, len(rows))
	sent := make([]int, 0, len(rows)) // messages[k] 对应 rows[sent[k]]
	for i, row := range rows {
		if len(row.Payload) > limit {
			errs[i] = fmt.Errorf("%w: %s record_id=%d is %d bytes, limit %d", errRowTooLarge, playerstore.AuditKindName(kind), row.RecordID, len(row.Payload), limit)
			continue
		}
		messages = append(messages, kafka.Message{Key: []byte(strconv.FormatUint(row.PlayerID, 10)), Value: row.Payload})
		sent = append(sent, i)
	}
	if len(messages) == 0 {
		return errs
	}
	perMessage := splitWriteError(writer.WriteMessages(ctx, messages...), len(messages))
	for k, i := range sent {
		errs[i] = perMessage[k]
	}
	return errs
}

// splitWriteError 把 WriteMessages 的返回值摊到每条消息:kafka.WriteErrors 按下标对齐,别的错误(拿不到元数据、ctx 到期)
// 算整批失败。ctx 到期时 kafka-go 不保证消息没写出去,按失败处理只会重投,不会漏。
func splitWriteError(err error, n int) []error {
	errs := make([]error, n)
	if err == nil {
		return errs
	}
	var perMessage kafka.WriteErrors
	if errors.As(err, &perMessage) && len(perMessage) == n {
		copy(errs, perMessage)
		return errs
	}
	fill(errs, err)
	return errs
}

func fill(errs []error, err error) {
	for i := range errs {
		errs[i] = err
	}
}

func kindIndex(kind storepb.PlayerAuditKind) int {
	for i, k := range relayKinds {
		if k == kind {
			return i
		}
	}
	return -1
}

// Close 关掉两个 Writer。转发循环退出之后调用。
func (p *KafkaProducer) Close() error {
	var errs []error
	for _, writer := range p.writers {
		if writer != nil {
			errs = append(errs, writer.Close())
		}
	}
	return errors.Join(errs...)
}
