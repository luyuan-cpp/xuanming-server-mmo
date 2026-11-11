package kafka

// transaction_log 消费者:按分区并行攒批落库(2026-11-06,缺口清单第 15 条,
// docs/design/audit-pipeline-horizontal-scale-20261106.md §2.B)。
// 派发骨架、不变量与排空语义与 player_snapshot 共用,见 partition_workers.go;这里只有流水自己的攒批与落库:
//   - 每个分区各攒一批(BatchSize 行或 FlushInterval 到点),按 tx_id 升序拼一条 INSERT IGNORE(store.InsertBatchIgnore),
//     重放与乱序都被主键忽略,所以落库不依赖消息顺序,不同分区的批同时落也只是各自按 tx_id 升序取锁;
//   - 落库名额是流水池上的实例级信号量(Kafka.TransactionLogMaxInflightFlushes,默认 1:同一时刻只有一条 INSERT,
//     与改动前一条循环串行落库相同),上限 = 池连接数 − 1,留一条给 GM 流水查询 / 召回。
// 改动前是一条循环把所有分到的分区攒成一批串行落库;一个副本分到几个分区吞吐也只有一条线。

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// txLogWorkerPoint 是分区 worker 的 safego 点位名。常量,**不拼分区号**(point 是 Prometheus label)。
const txLogWorkerPoint = "data_service.kafka.transaction_log.partition"

// errTxLogWorkerAborted 是某个分区 worker panic 时 Run 返回的错误(safego 已记点位与栈)。
var errTxLogWorkerAborted = fmt.Errorf("transaction_log partition worker aborted (panic); offsets of its in-flight batch NOT committed: %w", errPartitionWorkerAborted)

// TxLogSink 是消费者需要的唯一落库能力,由 *store.TransactionLogStore 实现。
// 会被多个分区 worker 并发调用(并发数 ≤ MaxInflightFlushes)。
type TxLogSink interface {
	InsertBatchIgnore(ctx context.Context, rows []*store.TransactionLogRow) (int64, error)
}

// TxLogConsumerConfig 攒批、重试与并发参数;零值取默认。
type TxLogConsumerConfig struct {
	// BatchSize 每个分区攒够多少行就落一次(默认 200)。
	BatchSize int
	// FlushInterval 一批的第一条进来之后最多等多久就落(默认 200ms):低流量时延迟有上限。
	FlushInterval time.Duration
	// DBMaxAttempts 一批落库的最大尝试次数(默认 5),用尽即停下不提交。
	DBMaxAttempts int
	// DBRetryBackoff 两次落库尝试之间的间隔(默认 1s)。
	DBRetryBackoff time.Duration
	// FetchBackoff 拉取失败后的退避(默认 1s)。
	FetchBackoff time.Duration
	// MaxInflightFlushes 本实例同时落库的分区数(默认 1)。yaml 取值与越界拒启见 config.KafkaConfig
	// 的 TransactionLogMaxInflightFlushes。
	MaxInflightFlushes int
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
	if c.MaxInflightFlushes <= 0 {
		c.MaxInflightFlushes = 1
	}
}

// ErrConsumerStoppedOnDB 是 Run 因 DB 重试用尽而停下时返回的错误(可 errors.Is)。
var ErrConsumerStoppedOnDB = errors.New("kafka consumer stopped: database unavailable, offsets not committed")

// TxLogConsumer 按分区攒批落 transaction_log。
type TxLogConsumer struct {
	cfg    TxLogConsumerConfig
	reader MessageReader
	sink   TxLogSink
	health *Health

	// sem 是实例级落库并发上限(容量 MaxInflightFlushes):每次 DB 尝试前取一个名额、尝试后归还,重试退避期间不占。
	sem chan struct{}
	// commitMu 串行化 CommitMessages:各分区 worker 会并发提交,这里不依赖 kafka-go Reader 对并发提交的未写明承诺。
	commitMu sync.Mutex
}

// NewTxLogConsumer 组装消费者;Run 之前不做任何 IO。
func NewTxLogConsumer(cfg TxLogConsumerConfig, reader MessageReader, sink TxLogSink) *TxLogConsumer {
	cfg.withDefaults()
	return &TxLogConsumer{
		cfg:    cfg,
		reader: reader,
		sink:   sink,
		health: newHealth(ConsumerTxLog),
		sem:    make(chan struct{}, cfg.MaxInflightFlushes),
	}
}

// Health 返回健康标志(给 /metrics 与将来的 readiness 用)。
func (c *TxLogConsumer) Health() *Health { return c.health }

// Run 阻塞消费直到 ctx 结束 / 排空完成(返回 nil)或任一分区停下(返回 errors.Is(err, ErrConsumerStoppedOnDB)
// 或 errTxLogWorkerAborted;该分区停在最后一次成功提交处,其余分区手里没落库的批不提交)。
// 返回前一定等所有分区 worker 退出;不 Close reader,由调用方负责。
//
// ctx 结束(进程关停、兄弟分区停机)时手里没落库的批**不落不提交**,重建 / 重启后重放,由 tx_id 主键忽略。
// 正常停服走维护排空:ownedReader 返回 errFetchDrained,各分区把通道里剩下的落完、提交后退出(partition_workers.go)。
func (c *TxLogConsumer) Run(ctx context.Context) error {
	c.health.markRunning()
	defer c.health.markStopped()

	if err := runPartitionWorkers(ctx, ConsumerTxLog, c.reader, c.cfg.FetchBackoff, c.startWorker); err != nil && ctx.Err() == nil {
		c.health.markFailed(err)
		return err
	}
	logx.Infof("[%s] consumer exiting on shutdown", ConsumerTxLog)
	return nil
}

// txLogPartitionWorker 处理一个分区。in 的容量 = BatchSize,FIFO 是分区内顺序的唯一来源。
type txLogPartitionWorker struct {
	c         *TxLogConsumer
	partition int
	in        chan kafkago.Message
}

func (c *TxLogConsumer) startWorker(runCtx context.Context, partition int, wg *sync.WaitGroup, stop *runStop) chan<- kafkago.Message {
	w := &txLogPartitionWorker{c: c, partition: partition, in: make(chan kafkago.Message, c.cfg.BatchSize)}
	goPartitionWorker(runCtx, txLogWorkerPoint, errTxLogWorkerAborted, wg, stop, w.run)
	return w.in
}

// txBatch 是一个分区正在攒的一批:msgs 是要提交的全部消息(含被跳过的坏消息),rows 是要插的行。
type txBatch struct {
	msgs []kafkago.Message
	rows []*store.TransactionLogRow
}

// reset 清空批、保留底层数组。先 clear 再截断:只截断时底层数组仍引用上一批的行,空闲分区会一直把它们钉在内存里。
func (b *txBatch) reset() {
	clear(b.msgs)
	clear(b.rows)
	b.msgs, b.rows = b.msgs[:0], b.rows[:0]
}

// run 处理一个分区直到 ctx 结束 / 通道关闭(返回 nil)或停机(返回带 ErrConsumerStoppedOnDB 的错误)。
func (w *txLogPartitionWorker) run(ctx context.Context) error {
	cfg := w.c.cfg
	b := &txBatch{}
	draining := false
	for {
		// 先单独看一次 ctx:上一批若因 ctx 取消而安静返回(没提交),绝不能再取后面的消息去落库、提交 ——
		// 下面那个 select 在 ctx.Done 与通道都就绪时是随机选的,不能只靠它。
		if ctx.Err() != nil {
			return nil
		}
		select { // 阻塞等这一批的第一条
		case <-ctx.Done():
			return nil
		case msg, ok := <-w.in:
			if !ok {
				return nil
			}
			w.add(b, msg)
		}
		timer := time.NewTimer(cfg.FlushInterval) // 单调时钟;FlushInterval 恒 > 0(withDefaults)
	fill:
		for len(b.rows) < cfg.BatchSize {
			select {
			case msg, ok := <-w.in:
				if !ok {
					draining = true
					break fill
				}
				w.add(b, msg)
			case <-timer.C:
				break fill
			case <-ctx.Done():
				timer.Stop()
				return nil // 手里这批不落不提交,重建 / 重启后重放(被 tx_id 主键忽略)
			}
		}
		timer.Stop()
		if err := w.flush(ctx, b); err != nil {
			return err
		}
		b.reset()
		if draining {
			return nil
		}
	}
}

func (w *txLogPartitionWorker) add(b *txBatch, msg kafkago.Message) {
	row, ok := decodeTxLog(msg)
	// 批里留着的 msgs 只用于提交与日志:kafka-go 提交只读 Topic / Partition / Offset(Topic 必须保留)。
	// 好消息的字段已拷进 row,不剥掉 Value 的话每条流水在批里就有两份。
	msg.Value, msg.Key, msg.Headers = nil, nil, nil
	b.msgs = append(b.msgs, msg)
	if ok {
		b.rows = append(b.rows, row)
	}
}

// decodeTxLog 把一条消息解成行;坏消息(解不出 / tx_id=0)返回 ok=false,调用方只提交不入库。
func decodeTxLog(msg kafkago.Message) (*store.TransactionLogRow, bool) {
	entry := &rollbackpb.TransactionLogEntry{}
	if err := proto.Unmarshal(msg.Value, entry); err != nil {
		messageLogger(msg).Errorf("[%s] undecodable message skipped partition=%d offset=%d key=%s: %v",
			ConsumerTxLog, msg.Partition, msg.Offset, string(msg.Key), err)
		metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "decode_error", 1)
		return nil, false
	}
	if entry.GetTxId() == 0 {
		// 生产者对 tx_id=0 是 fail-closed 的,走到这里说明上游坏了;0 做主键会让所有坏行互撞。
		messageLogger(msg).Errorf("[%s] tx_id=0 message skipped partition=%d offset=%d key=%s",
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

// flush 落库(有界重试)→ 成功后才提交本批本分区的最高 offset。
//   - 0 行(全是坏消息):只提交;
//   - 落库失败时 ctx 已取消(关停 / 兄弟分区停机):不提交、不计 db_error、不算停机,返回 nil 让 run 退出;
//   - 重试用尽:不提交,返回带 ErrConsumerStoppedOnDB 的错误,整个 Run 停下,supervisor 冷却后从本分区上次提交处续。
func (w *txLogPartitionWorker) flush(ctx context.Context, b *txBatch) error {
	c := w.c
	if len(b.rows) > 0 {
		inserted, err := c.insertWithRetry(ctx, b.rows)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "db_error", len(b.rows))
			logx.Errorf("[%s] STOPPED: partition=%d %d rows (offsets %d..%d) could not be inserted after %d attempts, offsets NOT committed (lag is preferred over loss; resumes from the last commit after the DB recovers): %v",
				ConsumerTxLog, w.partition, len(b.rows), b.msgs[0].Offset, b.msgs[len(b.msgs)-1].Offset, c.cfg.DBMaxAttempts, err)
			return fmt.Errorf("%w: %w", ErrConsumerStoppedOnDB, err)
		}
		// 只在有行且落成时记批大小:全是坏消息的批(rows=0)也走 flush,记进去会拉低均值。
		metrics.ObserveKafkaConsumerBatchRows(ConsumerTxLog, len(b.rows))
		metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "inserted", int(inserted))
		metrics.ObserveKafkaConsumerMessages(ConsumerTxLog, "duplicate", len(b.rows)-int(inserted))
	}
	c.commit(ctx, b.msgs)
	return nil
}

// insertWithRetry 一批落库 + 有界重试(DBMaxAttempts 次、间隔 DBRetryBackoff),每次尝试占一个落库名额。
// ctx 结束立即返回(不再重试),调用方据 ctx.Err() 判定为安静退出、不算停机。
func (c *TxLogConsumer) insertWithRetry(ctx context.Context, rows []*store.TransactionLogRow) (int64, error) {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.DBMaxAttempts; attempt++ {
		var inserted int64
		err := runFlushAttempt(ctx, c.sem, ConsumerTxLog, func() error {
			var insertErr error
			inserted, insertErr = c.sink.InsertBatchIgnore(ctx, rows)
			return insertErr
		})
		if err == nil {
			return inserted, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, fmt.Errorf("interrupted during db retry: %w", lastErr)
		}
		logx.Errorf("[%s] insert batch of %d failed (attempt %d/%d): %v",
			ConsumerTxLog, len(rows), attempt, c.cfg.DBMaxAttempts, err)
		if attempt < c.cfg.DBMaxAttempts && !sleepCtx(ctx, c.cfg.DBRetryBackoff) {
			return 0, fmt.Errorf("interrupted during db retry: %w", lastErr)
		}
	}
	return 0, lastErr
}

// commit 同步提交 msgs 里的最高 offset(每次只交一个分区的一段前缀)。提交失败只记日志:行已经在库里,重放会被主键忽略。
// 空切片不发提交。ctx 已取消也照样提交:调用点已确认这是落库前缀(ownedReader.CommitMessages 会换一个有界 ctx)。
func (c *TxLogConsumer) commit(ctx context.Context, msgs []kafkago.Message) {
	top := highestPerPartition(msgs)
	if len(top) == 0 {
		return
	}
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	if err := c.reader.CommitMessages(ctx, top...); err != nil && ctx.Err() == nil {
		logx.Errorf("[%s] commit offset failed partition=%d offset=%d (rows are persisted; replay will be ignored by PK): %v",
			ConsumerTxLog, top[0].Partition, top[0].Offset, err)
	}
}
