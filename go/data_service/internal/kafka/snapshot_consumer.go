package kafka

import (
	"context"
	"fmt"
	"sync"
	"time"

	"data_service/internal/config"
	"data_service/internal/metrics"
	"data_service/internal/store"

	rollbackpb "proto/common/rollback"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// ConsumerSnapshot 是 player_snapshot 消费者的名字 / 指标 label。
const ConsumerSnapshot = "player_snapshot"

// MaxSnapshotBlobBytes 是 player_snapshot.data(proto2mysql 把 bytes 渲染成 MEDIUMBLOB)
// 的硬上限 2^24-1。严格模式下超长写入会报错而不是截断,这样的消息永远插不进去,
// 只能当坏消息跳过(打 Error + 计数),否则会把分区卡死。
const MaxSnapshotBlobBytes = 16*1024*1024 - 1

// SnapshotSink 是消费者需要的落库能力,由 *store.SnapshotStore 实现。
type SnapshotSink interface {
	InsertSnapshotIfGuidAbsent(ctx context.Context, row *store.SnapshotRow) (id uint64, inserted bool, err error)
	// InsertSnapshotsIfGuidAbsent 批量去重落库,返回新插入行数;批内与库内重复都不计。
	// 只在一批有 >= 2 行时调用(BatchMaxRows > 1 才会出现),1 行一律走上面的单行接口。
	InsertSnapshotsIfGuidAbsent(ctx context.Context, rows []*store.SnapshotRow) (int64, error)
}

// SnapshotConsumerConfig 重试与批量参数;零值取默认。
type SnapshotConsumerConfig struct {
	DBMaxAttempts  int           // 默认 5
	DBRetryBackoff time.Duration // 默认 1s
	FetchBackoff   time.Duration // 默认 1s
	MaxBlobBytes   int           // 默认 MaxSnapshotBlobBytes

	// 以下四项零值 = 2026-10-12 之前的逐条串行行为(装配见 data_service.go,yaml 取值与校验见 config.KafkaConfig)。
	BatchMaxRows       int           // <=1:逐条落、逐条提交;上限钳到 store.MaxSnapshotBatchRows
	BatchMaxBytes      int           // <=0:config.DefaultSnapshotBatchMaxBytes(软上限:加入一条后检查)
	FlushInterval      time.Duration // <=0:不等待,只把已排队的消息拼进来
	MaxInflightFlushes int           // <=0:1(本实例同时落库的分区数)
}

func (c *SnapshotConsumerConfig) withDefaults() {
	if c.DBMaxAttempts <= 0 {
		c.DBMaxAttempts = 5
	}
	if c.DBRetryBackoff <= 0 {
		c.DBRetryBackoff = time.Second
	}
	if c.FetchBackoff <= 0 {
		c.FetchBackoff = time.Second
	}
	if c.MaxBlobBytes <= 0 {
		c.MaxBlobBytes = MaxSnapshotBlobBytes
	}
	if c.BatchMaxRows < 1 {
		c.BatchMaxRows = 1
	}
	if c.BatchMaxRows > store.MaxSnapshotBatchRows {
		// yaml 路径由 config.ValidateConsumers 拒启;这里兜的是直接构造配置的调用方(单测、将来的工具)。
		// 不钳的话 store 每批都拒绝 → 每批都退回逐条兜底,静默变慢且只有 info 级告警。
		logx.Errorf("[%s] BatchMaxRows=%d exceeds store.MaxSnapshotBatchRows=%d; clamped (a larger batch would be rejected by the store and every batch would fall back to row-by-row)",
			ConsumerSnapshot, c.BatchMaxRows, store.MaxSnapshotBatchRows)
		c.BatchMaxRows = store.MaxSnapshotBatchRows
	}
	if c.BatchMaxBytes <= 0 {
		c.BatchMaxBytes = config.DefaultSnapshotBatchMaxBytes
	}
	if c.FlushInterval < 0 {
		c.FlushInterval = 0
	}
	if c.MaxInflightFlushes <= 0 {
		c.MaxInflightFlushes = 1
	}
}

// SnapshotConsumer 落 player_snapshot(source=1):每个分区一个 worker(分区内 FIFO、先落库后提交),
// 可配批量(默认逐条),实例级落库并发上限。结构见 snapshot_worker.go。
type SnapshotConsumer struct {
	cfg    SnapshotConsumerConfig
	reader MessageReader
	sink   SnapshotSink
	health *Health

	// sem 是实例级落库并发上限(容量 MaxInflightFlushes):每次 DB 尝试前取一个名额、尝试后归还,重试退避期间不占。
	// 有它,「本实例同时写几个分区」才有界,快照池(SnapshotMySQL.MaxOpenConn)总留得出连接给 GM 快照 / 回档读。
	sem chan struct{}
	// commitMu 串行化 CommitMessages:各分区 worker 会并发提交,这里不依赖 kafka-go Reader 对并发提交的未写明承诺。
	commitMu sync.Mutex
}

// NewSnapshotConsumer 组装消费者;Run 之前不做任何 IO。
func NewSnapshotConsumer(cfg SnapshotConsumerConfig, reader MessageReader, sink SnapshotSink) *SnapshotConsumer {
	cfg.withDefaults()
	return &SnapshotConsumer{
		cfg:    cfg,
		reader: reader,
		sink:   sink,
		health: newHealth(ConsumerSnapshot),
		sem:    make(chan struct{}, cfg.MaxInflightFlushes),
	}
}

// Health 返回健康标志。
func (c *SnapshotConsumer) Health() *Health { return c.health }

// decode 把一条消息解成 source=1 的行;坏消息返回 ok=false,调用方只提交不入库。
// data 列存**收到的原始字节**(整条 PlayerSnapshotEntry,含两个 player_database blob 与
// schema_version),而不是拆开的 blob:source=1 的恢复路径是下一阶段,存原样最不丢信息。
func (c *SnapshotConsumer) decode(msg kafkago.Message) (*store.SnapshotRow, bool) {
	entry := &rollbackpb.PlayerSnapshotEntry{}
	if err := proto.Unmarshal(msg.Value, entry); err != nil {
		messageLogger(msg).Errorf("[%s] undecodable message skipped partition=%d offset=%d key=%s: %v",
			ConsumerSnapshot, msg.Partition, msg.Offset, string(msg.Key), err)
		metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "decode_error", 1)
		return nil, false
	}
	if entry.GetSnapshotId() == 0 || entry.GetPlayerId() == 0 {
		// 生产者对 snapshot_id=0 fail-closed;player_id=0 的快照无主。两者都无法按 guid 去重 / 按玩家检索。
		messageLogger(msg).Errorf("[%s] invalid snapshot skipped partition=%d offset=%d snapshot_id=%d player_id=%d",
			ConsumerSnapshot, msg.Partition, msg.Offset, entry.GetSnapshotId(), entry.GetPlayerId())
		metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "invalid", 1)
		return nil, false
	}
	if len(msg.Value) > c.cfg.MaxBlobBytes {
		messageLogger(msg).Errorf("[%s] oversize snapshot skipped partition=%d offset=%d snapshot_id=%d player_id=%d bytes=%d limit=%d",
			ConsumerSnapshot, msg.Partition, msg.Offset, entry.GetSnapshotId(), entry.GetPlayerId(), len(msg.Value), c.cfg.MaxBlobBytes)
		metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "oversize", 1)
		return nil, false
	}

	// 拷贝一份:reader 复用的缓冲不能被存进行里。
	data := make([]byte, len(msg.Value))
	copy(data, msg.Value)

	return &store.SnapshotRow{
		PlayerID:     entry.GetPlayerId(),
		ZoneID:       entry.GetZoneId(), // 捕获时刻的 zone,不回查 Router
		SnapshotType: uint32(entry.GetTrigger()),
		CreatedAt:    entry.GetSnapshotTime(),
		Reason:       "cpp:" + entry.GetTrigger().String(),
		Operator:     store.SnapshotOperatorSceneNode,
		Data:         data,
		SnapshotGuid: entry.GetSnapshotId(),
		Source:       store.SnapshotSourceSceneKafka,
	}, true
}

// runAttempt 在本实例的快照落库名额下跑一次 DB 尝试,口径见 runFlushAttempt。
func (c *SnapshotConsumer) runAttempt(ctx context.Context, op func() error) error {
	return runFlushAttempt(ctx, c.sem, ConsumerSnapshot, op)
}

// insertWithRetry 单行落库 + 有界重试(DBMaxAttempts 次、间隔 DBRetryBackoff),与 2026-10-12 之前逐条消费的容错逐字相同。
// ctx 结束立即返回(不记 attempt 失败、不再重试),调用方据 ctx.Err() 判定为安静退出、不算停机。
func (c *SnapshotConsumer) insertWithRetry(ctx context.Context, row *store.SnapshotRow) (uint64, bool, error) {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.DBMaxAttempts; attempt++ {
		var (
			id       uint64
			inserted bool
		)
		err := c.runAttempt(ctx, func() error {
			var insertErr error
			id, inserted, insertErr = c.sink.InsertSnapshotIfGuidAbsent(ctx, row)
			return insertErr
		})
		if err == nil {
			return id, inserted, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, false, fmt.Errorf("interrupted during db retry: %w", lastErr)
		}
		logx.Errorf("[%s] insert snapshot guid=%d player=%d failed (attempt %d/%d): %v",
			ConsumerSnapshot, row.SnapshotGuid, row.PlayerID, attempt, c.cfg.DBMaxAttempts, err)
		if attempt < c.cfg.DBMaxAttempts && !sleepCtx(ctx, c.cfg.DBRetryBackoff) {
			return 0, false, fmt.Errorf("interrupted during db retry: %w", lastErr)
		}
	}
	return 0, false, lastErr
}

// insertBatchWithRetry 批量落库 + 与单行同样的有界重试。「批本身太大」的确定性错误(store.IsSnapshotBatchTooLarge)
// 不再退避重试、立即返回,由调用方逐条兜底 —— 同一批原样再试只会撞同一堵墙,每批白等约 DBMaxAttempts 秒。
func (c *SnapshotConsumer) insertBatchWithRetry(ctx context.Context, rows []*store.SnapshotRow) (int64, error) {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.DBMaxAttempts; attempt++ {
		var inserted int64
		err := c.runAttempt(ctx, func() error {
			var insertErr error
			inserted, insertErr = c.sink.InsertSnapshotsIfGuidAbsent(ctx, rows)
			return insertErr
		})
		if err == nil {
			return inserted, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, fmt.Errorf("interrupted during db retry: %w", lastErr)
		}
		logx.Errorf("[%s] insert snapshot batch of %d (first guid=%d) failed (attempt %d/%d): %v",
			ConsumerSnapshot, len(rows), rows[0].SnapshotGuid, attempt, c.cfg.DBMaxAttempts, err)
		if store.IsSnapshotBatchTooLarge(err) {
			return 0, lastErr
		}
		if attempt < c.cfg.DBMaxAttempts && !sleepCtx(ctx, c.cfg.DBRetryBackoff) {
			return 0, fmt.Errorf("interrupted during db retry: %w", lastErr)
		}
	}
	return 0, lastErr
}

// observeOne 记一行单行落库的结局(原 inserted / duplicate 计数与日志,逐条模式下与 2026-10-12 之前逐字相同)。
func observeOne(id uint64, inserted bool, row *store.SnapshotRow) {
	if inserted {
		metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "inserted", 1)
		return
	}
	metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "duplicate", 1)
	logx.Infof("[%s] duplicate snapshot guid=%d player=%d already stored as id=%d, skipped",
		ConsumerSnapshot, row.SnapshotGuid, row.PlayerID, id)
}

// commit 同步提交 msgs 里每个分区的最高 offset(本消费者每次只交一个分区的一段前缀);
// 失败只记日志(行已落库,重放会被 guid 去重)。
// 空切片不发提交:flush / isolate 在第一行就失败时会传 b.msgs[:0],白发一次空提交既浪费,也会让
// "停机不提交"的断言失真(TxLogConsumer.commit 同一个守卫)。
func (c *SnapshotConsumer) commit(ctx context.Context, msgs []kafkago.Message) {
	top := highestPerPartition(msgs)
	if len(top) == 0 {
		return
	}
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	if err := c.reader.CommitMessages(ctx, top...); err != nil && ctx.Err() == nil {
		logx.Errorf("[%s] commit offset failed partition=%d offset=%d (rows are persisted; replay is deduped by guid): %v",
			ConsumerSnapshot, top[0].Partition, top[0].Offset, err)
	}
}
