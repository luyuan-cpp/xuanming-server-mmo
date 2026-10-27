package kafka

// player_snapshot 消费者的「按分区并发 + 可配批量」(G2a 快照审计管道扩容)。
//
//	kafka-go group Reader(每实例一个)
//	        │ FetchMessage
//	   dispatcher(Run 所在 goroutine):map[partition]*worker,懒创建
//	        │
//	   worker p0 / p1 / …   每个 worker:FIFO 通道 → 攒批 → flush(先落库、后提交本分区本批最高 offset)
//	        │
//	   sem(MaxInflightFlushes,实例级)→ SnapshotSink
//
// 不变量(「不丢」的全部依据):任何分区的任何一次提交 V,都满足「该分区 offset < V 的有效消息都已落库」。
//   - 同一分区只由一个 worker 处理,通道 FIFO 是分区内顺序的唯一来源;批内行按 offset 序拼进一条语句;
//   - 只在落库成功后提交、只提交已落库前缀;落库没成功(含 ctx 取消导致的安静退出)的批一律不提交;
//   - 一批因 ctx 取消没提交之后,这个 worker 立刻退出,**绝不**再处理后面的消息 —— 后面的批一旦提交,
//     就越过了没落库的这一批(run 循环顶部的 ctx 检查就是为它写的)。
// 默认配置(批 1 条、并发 1、不等待)下每条消息单独落库、单独提交,与 2026-10-12 之前语义等价。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"data_service/internal/metrics"
	"data_service/internal/store"

	"shared/safego"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
)

// snapshotWorkerPoint 是分区 worker 的 safego 点位名。常量,**不拼分区号**(point 是 Prometheus label)。
const snapshotWorkerPoint = "data_service.kafka.player_snapshot.partition"

// errSnapshotWorkerAborted 是某个分区 worker panic 时 Run 返回的错误(safego 已记点位与栈)。
var errSnapshotWorkerAborted = errors.New("player_snapshot partition worker aborted (panic); offsets of its in-flight batch NOT committed")

// Run 阻塞消费直到 ctx 结束(返回 nil)或任一分区停下(返回 errors.Is(err, ErrConsumerStoppedOnDB)
// 或 errSnapshotWorkerAborted;该分区停在最后一次成功提交处)。返回前一定等所有分区 worker 退出 ——
// supervisor 随后才 Close reader,不会出现「reader 已关、worker 还在提交」。
//
// 一个分区停下就停整个消费者(与 2026-10-12 之前「一条循环停下 = 整个消费者停下」一致),
// supervisor 冷却 ResumeAfter 后从各分区的断点重建。
func (c *SnapshotConsumer) Run(ctx context.Context) error {
	c.health.markRunning()
	defer c.health.markStopped()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stop := &runStop{cancel: cancelRun}
	var wg sync.WaitGroup
	// dispatcher panic 时也先取消并 join 分区 worker，再让 supervisor 关闭 reader。
	defer func() { cancelRun(); wg.Wait() }()
	workers := make(map[int]*snapshotPartitionWorker)
	graceful := false

	for runCtx.Err() == nil {
		msg, err := c.reader.FetchMessage(runCtx)
		if err != nil {
			if errors.Is(err, errFetchDrained) {
				graceful = true
				break
			}
			if runCtx.Err() != nil {
				break
			}
			logx.Errorf("[%s] fetch failed, retry in %s: %v", ConsumerSnapshot, c.cfg.FetchBackoff, err)
			sleepCtx(runCtx, c.cfg.FetchBackoff)
			continue
		}
		w := workers[msg.Partition]
		if w == nil {
			w = c.startWorker(runCtx, msg.Partition, &wg, stop)
			workers[msg.Partition] = w
		}
		select {
		case w.in <- msg:
		case <-runCtx.Done():
			// 某个分区已停(或进程关停):这条不投递、不提交,重建 / 重启后从断点重放。
		}
	}
	if graceful {
		for _, worker := range workers {
			close(worker.in)
		}
	} else {
		cancelRun()
	}
	wg.Wait()

	if err := stop.firstErr(); err != nil && ctx.Err() == nil {
		c.health.markFailed(err)
		return err
	}
	logx.Infof("[%s] consumer exiting on shutdown", ConsumerSnapshot)
	return nil
}

// runStop 记第一次失败并取消整个 Run。err 在 wg.Wait 之后读:worker 的 fail 发生在它自己的 wg.Done 之前
// (两个 defer 的注册顺序保证),wg.Wait 给出 happens-before。
type runStop struct {
	once   sync.Once
	err    error
	cancel context.CancelFunc
}

func (s *runStop) fail(err error) {
	s.once.Do(func() { s.err = err })
	s.cancel()
}

func (s *runStop) firstErr() error { return s.err }

// snapshotPartitionWorker 处理一个分区。in 的容量 = BatchMaxRows,FIFO 是分区内顺序的唯一来源。
type snapshotPartitionWorker struct {
	c         *SnapshotConsumer
	partition int
	in        chan kafkago.Message
}

func (c *SnapshotConsumer) startWorker(runCtx context.Context, partition int, wg *sync.WaitGroup, stop *runStop) *snapshotPartitionWorker {
	w := &snapshotPartitionWorker{c: c, partition: partition, in: make(chan kafkago.Message, c.cfg.BatchMaxRows)}
	wg.Add(1)
	safego.Go(snapshotWorkerPoint, func() {
		defer wg.Done()
		completed := false
		defer func() {
			// panic 展开时也会先跑到这里(早于 safego 的 recover):不让一个死掉的分区在其它分区继续跑时静默停摆。
			if !completed {
				stop.fail(errSnapshotWorkerAborted)
			}
		}()
		err := w.run(runCtx)
		completed = true
		if err != nil {
			stop.fail(err)
		}
	})
	return w
}

// snapshotBatch 是一个分区正在攒的一批。msgs 是要提交的全部消息(含被跳过的坏消息),
// rows[i] 来自 msgs[rowMsgIdx[i]] —— 逐条兜底时据此只提交失败行之前的前缀。
type snapshotBatch struct {
	msgs      []kafkago.Message
	rows      []*store.SnapshotRow
	rowMsgIdx []int
	bytes     int
}

// reset 清空批、保留底层数组。先 clear 再截断:只截断时底层数组仍引用上一批的行(Data 最大约 1MB/条),
// 空闲分区会一直把它们钉在内存里。
func (b *snapshotBatch) reset() {
	clear(b.msgs)
	clear(b.rows)
	b.msgs, b.rows, b.rowMsgIdx, b.bytes = b.msgs[:0], b.rows[:0], b.rowMsgIdx[:0], 0
}

// run 处理一个分区直到 ctx 结束(返回 nil)或停机(返回带 ErrConsumerStoppedOnDB 的错误)。
func (w *snapshotPartitionWorker) run(ctx context.Context) error {
	cfg := w.c.cfg
	b := &snapshotBatch{}
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
		deadline := time.Now().Add(cfg.FlushInterval) // 单调时钟
	fill:
		for !w.full(b) {
			select { // 先无等待地拿已排队的(FlushInterval=0 时的「自然攒批」)
			case msg, ok := <-w.in:
				if !ok {
					draining = true
					break fill
				}
				w.add(b, msg)
				continue
			default:
			}
			wait := time.Until(deadline)
			if wait <= 0 {
				break fill
			}
			timer := time.NewTimer(wait)
			select {
			case msg, ok := <-w.in:
				timer.Stop()
				if !ok {
					draining = true
					break fill
				}
				w.add(b, msg)
			case <-timer.C:
				break fill
			case <-ctx.Done():
				timer.Stop()
				return nil // 手里这批不落不提交,重建 / 重启后重放(与 2026-10-12 之前关停时丢弃在途一条同口径)
			}
		}
		if err := w.flush(ctx, b); err != nil {
			return err
		}
		b.reset()
		if draining {
			return nil
		}
	}
}

func (w *snapshotPartitionWorker) full(b *snapshotBatch) bool {
	return len(b.msgs) >= w.c.cfg.BatchMaxRows || b.bytes >= w.c.cfg.BatchMaxBytes
}

func (w *snapshotPartitionWorker) add(b *snapshotBatch, msg kafkago.Message) {
	// 含坏消息:单条大小分布是核对生产端约 1MB 消息上限要的数据。
	metrics.ObserveKafkaConsumerMessageBytes(ConsumerSnapshot, len(msg.Value))
	// 坏消息在 decode 里记日志 + outcome 计数;好消息的 Value 已拷进 row.Data。
	// 计数与日志在进批时记,不等提交:批量模式下同批前面有行失败时这条坏消息不会被提交,重建重放会重复记
	// (与 TxLogConsumer 同口径,见告警 DataServiceAuditMessagesSkipped 的说明)。默认配置(批 1 条)下坏消息
	// 独占一批并立即提交,排在失败行后面的消息留在通道里不会被 decode,不受影响。
	row, ok := w.c.decode(msg)
	// 批里留着的 msgs 只用于提交与日志:kafka-go 提交只读 Topic / Partition / Offset(Topic 必须保留)。
	// 不剥掉 Value,每条快照在批里就有两份(msg.Value + row.Data),内存翻倍。
	msg.Value, msg.Key, msg.Headers = nil, nil, nil
	b.msgs = append(b.msgs, msg)
	if ok {
		b.rows = append(b.rows, row)
		b.rowMsgIdx = append(b.rowMsgIdx, len(b.msgs)-1)
		b.bytes += len(row.Data)
	}
}

// flush 落库 → 成功后才提交本批本分区的最高 offset。
//   - 0 行(全是坏消息):只提交;
//   - 1 行:走单行接口 + 单行重试(逐条模式下与 2026-10-12 之前逐字相同,含 "already stored as id=%d" 日志);
//   - >=2 行:批量接口 + 同样的有界重试;用尽后逐条兜底(isolate);
//   - 失败时 ctx 已取消(关停 / 兄弟分区停机):不提交、不计 db_error、不算停机,返回 nil 让 run 退出。
func (w *snapshotPartitionWorker) flush(ctx context.Context, b *snapshotBatch) error {
	c := w.c
	switch len(b.rows) {
	case 0:
	case 1:
		row := b.rows[0]
		id, inserted, err := c.insertWithRetry(ctx, row)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "db_error", 1)
			logx.Errorf("[%s] STOPPED: partition=%d snapshot guid=%d player=%d could not be inserted after %d attempts, offset %d NOT committed (lag is preferred over loss): %v",
				ConsumerSnapshot, w.partition, row.SnapshotGuid, row.PlayerID, c.cfg.DBMaxAttempts, b.msgs[b.rowMsgIdx[0]].Offset, err)
			// 夹在它前面的坏消息照常提交(逐条消费时它们也是先于它各自提交的);它自己与之后的一律不提交。
			c.commit(ctx, b.msgs[:b.rowMsgIdx[0]])
			return fmt.Errorf("%w: %w", ErrConsumerStoppedOnDB, err)
		}
		metrics.ObserveKafkaConsumerBatchRows(ConsumerSnapshot, 1)
		observeOne(id, inserted, row)
	default:
		inserted, err := c.insertBatchWithRetry(ctx, b.rows)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return w.isolate(ctx, b, err)
		}
		metrics.ObserveKafkaConsumerBatchRows(ConsumerSnapshot, len(b.rows))
		dup := len(b.rows) - int(inserted)
		metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "inserted", int(inserted))
		metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "duplicate", dup)
		if dup > 0 {
			// 批量路径(预读 + 普通多行 INSERT)不回带每行 id,每批一条汇总(逐条模式仍是逐 guid 的 already stored 日志)。
			logx.Infof("[%s] partition=%d batch of %d: %d duplicate snapshot(s) already stored, skipped (offsets %d..%d)",
				ConsumerSnapshot, w.partition, len(b.rows), dup, b.msgs[0].Offset, b.msgs[len(b.msgs)-1].Offset)
		}
	}
	c.commit(ctx, b.msgs)
	return nil
}

// isolate 是批量失败后的逐条兜底。它让批量**永远不会**比逐条更容易卡住:
//   - 批本身的问题(包过大、事务过大、某种只在多行语句上出现的错误):逐条全部成功 → 整批提交,继续消费;
//   - 真有一行插不进去(毒丸行 / DB 真挂了):停在那一行,只提交它之前的前缀 ——
//     与逐条消费「落到它为止、之前的都已提交」的结局逐字相同,并且日志点名是哪个 guid。
//
// 每行走与单行路径**同一个** insertWithRetry(DBMaxAttempts 次有界重试),单行的容错与逐条消费逐字相同;
// 代价是 DB 真挂时停机前多耗约 DBMaxAttempts×DBRetryBackoff。
func (w *snapshotPartitionWorker) isolate(ctx context.Context, b *snapshotBatch, batchErr error) error {
	c := w.c
	logx.Errorf("[%s] partition=%d batch of %d rows failed, retrying row by row to isolate the failing row: %v",
		ConsumerSnapshot, w.partition, len(b.rows), batchErr)
	for i, row := range b.rows {
		id, inserted, err := c.insertWithRetry(ctx, row)
		if err != nil {
			// 前缀(之前的行已逐条落库,夹在中间的坏消息本就要提交)照常提交;这一行与之后的不提交。
			c.commit(ctx, b.msgs[:b.rowMsgIdx[i]])
			if ctx.Err() != nil {
				return nil
			}
			metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "db_error", 1)
			metrics.ObserveKafkaConsumerIsolation(ConsumerSnapshot, metrics.KafkaIsolationStopped)
			logx.Errorf("[%s] STOPPED: partition=%d snapshot guid=%d player=%d could not be inserted (batch failed, then row by row with %d attempts), offset %d NOT committed (lag is preferred over loss): %v",
				ConsumerSnapshot, w.partition, row.SnapshotGuid, row.PlayerID, c.cfg.DBMaxAttempts, b.msgs[b.rowMsgIdx[i]].Offset, err)
			return fmt.Errorf("%w: %w", ErrConsumerStoppedOnDB, err)
		}
		observeOne(id, inserted, row)
	}
	metrics.ObserveKafkaConsumerIsolation(ConsumerSnapshot, metrics.KafkaIsolationRecovered)
	metrics.ObserveKafkaConsumerBatchRows(ConsumerSnapshot, len(b.rows))
	c.commit(ctx, b.msgs)
	return nil
}
