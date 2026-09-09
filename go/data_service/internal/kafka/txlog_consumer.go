package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"data_service/internal/metrics"
	"data_service/internal/store"

	rollbackpb "proto/common/rollback"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// ConsumerTxLog 是 transaction_log 消费者的名字 / 指标 label。
const ConsumerTxLog = "transaction_log"

// TxLogSink 是消费者需要的唯一落库能力,由 *store.TransactionLogStore 实现。
type TxLogSink interface {
	InsertBatchIgnore(ctx context.Context, rows []*store.TransactionLogRow) (int64, error)
}

// TxLogConsumerConfig 攒批与重试参数;零值取默认。
type TxLogConsumerConfig struct {
	// BatchSize 攒够多少条就落一次(默认 200)。
	BatchSize int
	// FlushInterval 第一条进批之后最多等多久就落(默认 200ms):低流量时延迟有上限。
	FlushInterval time.Duration
	// DBMaxAttempts 一批落库的最大尝试次数(默认 5),用尽即停下不提交。
	DBMaxAttempts int
	// DBRetryBackoff 两次落库尝试之间的间隔(默认 1s)。
	DBRetryBackoff time.Duration
	// FetchBackoff 拉取失败后的退避(默认 1s)。
	FetchBackoff time.Duration
}

func (c *TxLogConsumerConfig) withDefaults() {
	if c.BatchSize <= 0 {
		c.BatchSize = 200
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 200 * time.Millisecond
	}
	if c.DBMaxAttempts <= 0 {
		c.DBMaxAttempts = 5
	}
	if c.DBRetryBackoff <= 0 {
		c.DBRetryBackoff = time.Second
	}
	if c.FetchBackoff <= 0 {
		c.FetchBackoff = time.Second
	}
}

// ErrConsumerStoppedOnDB 是 Run 因 DB 重试用尽而停下时返回的错误(可 errors.Is)。
var ErrConsumerStoppedOnDB = errors.New("kafka consumer stopped: database unavailable, offsets not committed")

// TxLogConsumer 攒批落 transaction_log。
type TxLogConsumer struct {
	cfg    TxLogConsumerConfig
	reader MessageReader
	sink   TxLogSink
	health *Health
}

// NewTxLogConsumer 组装消费者;Run 之前不做任何 IO。
func NewTxLogConsumer(cfg TxLogConsumerConfig, reader MessageReader, sink TxLogSink) *TxLogConsumer {
	cfg.withDefaults()
	return &TxLogConsumer{cfg: cfg, reader: reader, sink: sink, health: newHealth(ConsumerTxLog)}
}

// Health 返回健康标志(给 /metrics 与将来的 readiness 用)。
func (c *TxLogConsumer) Health() *Health { return c.health }

// txBatch 是正在攒的一批:msgs 是要提交的全部消息(含被跳过的坏消息),rows 是要插的行。
type txBatch struct {
	msgs     []kafkago.Message
	rows     []*store.TransactionLogRow
	deadline time.Time
}

func (b *txBatch) reset() {
	b.msgs = b.msgs[:0]
	b.rows = b.rows[:0]
	b.deadline = time.Time{}
}

// Run 阻塞消费直到 ctx 结束(返回 nil)或 DB 重试用尽(返回 ErrConsumerStoppedOnDB,
// 本批 offset 未提交)。不 Close reader,由调用方负责。
func (c *TxLogConsumer) Run(ctx context.Context) error {
	c.health.markRunning()
	defer c.health.markStopped()

	batch := &txBatch{
		msgs: make([]kafkago.Message, 0, c.cfg.BatchSize),
		rows: make([]*store.TransactionLogRow, 0, c.cfg.BatchSize),
	}

	for {
		// 批非空时用批的 deadline 限住 FetchMessage,到点即使没凑满也落库。
		fetchCtx, cancel := ctx, context.CancelFunc(func() {})
		if len(batch.msgs) > 0 {
			fetchCtx, cancel = context.WithDeadline(ctx, batch.deadline)
		}
		msg, err := c.reader.FetchMessage(fetchCtx)
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				// 关停:尽力把手里这批落掉并提交(失败也无妨,未提交的会在重启后重放并被 IGNORE)。
				c.flushOnShutdown(batch)
				logx.Infof("[%s] consumer exiting on shutdown", ConsumerTxLog)
				return nil
			}
			if fetchCtx.Err() != nil && len(batch.msgs) > 0 {
				// 批到时限:时间驱动的落库。
				if err := c.flush(ctx, batch); err != nil {
					return err
				}
				continue
			}
			logx.Errorf("[%s] fetch failed, retry in %s: %v", ConsumerTxLog, c.cfg.FetchBackoff, err)
			if !sleepCtx(ctx, c.cfg.FetchBackoff) {
				c.flushOnShutdown(batch)
				return nil
			}
			continue
		}

		if len(batch.msgs) == 0 {
			batch.deadline = time.Now().Add(c.cfg.FlushInterval)
		}
		batch.msgs = append(batch.msgs, msg)

		if row, ok := decodeTxLog(msg); ok {
			batch.rows = append(batch.rows, row)
		}

		if len(batch.rows) >= c.cfg.BatchSize {
			if err := c.flush(ctx, batch); err != nil {
				return err
			}
		}
	}
}

// decodeTxLog 把一条消息解成行;坏消息(解不出 / tx_id=0)返回 ok=false,调用方只提交不入库。
func decodeTxLog(msg kafkago.Message) (*store.TransactionLogRow, bool) {
	entry := &rollbackpb.TransactionLogEntry{}
	if err := proto.Unmarshal(msg.Value, entry); err != nil {
		logx.Errorf("[%s] undecodable message skipped partition=%d offset=%d key=%s: %v",
			ConsumerTxLog, msg.Partition, msg.Offset, string(msg.Key), err)
		metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "decode_error", 1)
		return nil, false
	}
	if entry.GetTxId() == 0 {
		// 生产者对 tx_id=0 是 fail-closed 的,走到这里说明上游坏了;0 做主键会让所有坏行互撞。
		logx.Errorf("[%s] tx_id=0 message skipped partition=%d offset=%d key=%s",
			ConsumerTxLog, msg.Partition, msg.Offset, string(msg.Key))
		metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "invalid", 1)
		return nil, false
	}
	return &store.TransactionLogRow{
		TxID:          entry.GetTxId(),
		Timestamp:     entry.GetTimestamp(), // 列名 timestamp_sec
		TxType:        uint32(entry.GetTxType()),
		FromPlayer:    entry.GetFromPlayer(),
		ToPlayer:      entry.GetToPlayer(),
		ItemUUID:      entry.GetItemUuid(),
		ItemConfigID:  entry.GetItemConfigId(),
		ItemQuantity:  entry.GetItemQuantity(),
		CurrencyType:  entry.GetCurrencyType(),
		CurrencyDelta: entry.GetCurrencyDelta(),
		BalanceBefore: entry.GetBalanceBefore(),
		BalanceAfter:  entry.GetBalanceAfter(),
		CorrelationID: entry.GetCorrelationId(),
		Extra:         entry.GetExtra(), // 列是 MEDIUMTEXT,不截断
		ZoneID:        entry.GetZoneId(),
	}, true
}

// flush 落库(有界重试)→ 成功后才提交本批每个分区的最高 offset → 清批。
// 落库重试用尽:标记故障、**不提交**、返回 ErrConsumerStoppedOnDB 让 Run 退出。
func (c *TxLogConsumer) flush(ctx context.Context, batch *txBatch) error {
	if len(batch.rows) > 0 {
		inserted, err := c.insertWithRetry(ctx, batch.rows)
		if err != nil {
			metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "db_error", len(batch.rows))
			c.health.markFailed(err)
			logx.Errorf("[%s] STOPPED: %d rows could not be inserted after %d attempts, offsets NOT committed (lag is preferred over loss; restart after DB recovers): %v",
				ConsumerTxLog, len(batch.rows), c.cfg.DBMaxAttempts, err)
			return fmt.Errorf("%w: %v", ErrConsumerStoppedOnDB, err)
		}
		dup := len(batch.rows) - int(inserted)
		metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "inserted", int(inserted))
		metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "duplicate", dup)
	}
	c.commit(ctx, batch.msgs)
	batch.reset()
	return nil
}

func (c *TxLogConsumer) insertWithRetry(ctx context.Context, rows []*store.TransactionLogRow) (int64, error) {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.DBMaxAttempts; attempt++ {
		inserted, err := c.sink.InsertBatchIgnore(ctx, rows)
		if err == nil {
			return inserted, nil
		}
		lastErr = err
		logx.Errorf("[%s] insert batch of %d failed (attempt %d/%d): %v",
			ConsumerTxLog, len(rows), attempt, c.cfg.DBMaxAttempts, err)
		if attempt < c.cfg.DBMaxAttempts && !sleepCtx(ctx, c.cfg.DBRetryBackoff) {
			return 0, fmt.Errorf("interrupted during db retry: %w", lastErr)
		}
	}
	return 0, lastErr
}

// commit 提交每个分区的最高 offset。提交失败只记日志:行已经在库里,重启后重放会被 IGNORE。
func (c *TxLogConsumer) commit(ctx context.Context, msgs []kafkago.Message) {
	top := highestPerPartition(msgs)
	if len(top) == 0 {
		return
	}
	if err := c.reader.CommitMessages(ctx, top...); err != nil && ctx.Err() == nil {
		logx.Errorf("[%s] commit offsets failed (rows are persisted; replay will be ignored by PK): %v", ConsumerTxLog, err)
	}
}

// flushOnShutdown 关停时尽力落掉手里这批。用独立的短超时 ctx:父 ctx 已经取消。
func (c *TxLogConsumer) flushOnShutdown(batch *txBatch) {
	if len(batch.msgs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if len(batch.rows) > 0 {
		if _, err := c.sink.InsertBatchIgnore(ctx, batch.rows); err != nil {
			logx.Errorf("[%s] shutdown flush of %d rows failed; they will be replayed on restart: %v",
				ConsumerTxLog, len(batch.rows), err)
			return
		}
	}
	c.commit(ctx, batch.msgs)
	batch.reset()
}
