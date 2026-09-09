package kafka

import (
	"context"
	"fmt"
	"time"

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

// SnapshotSink 是消费者需要的唯一落库能力,由 *store.SnapshotStore 实现。
type SnapshotSink interface {
	InsertSnapshotIfGuidAbsent(ctx context.Context, row *store.SnapshotRow) (id uint64, inserted bool, err error)
}

// SnapshotConsumerConfig 重试参数;零值取默认。不攒批:单条几百 KB,逐条落、逐条提交。
type SnapshotConsumerConfig struct {
	DBMaxAttempts  int           // 默认 5
	DBRetryBackoff time.Duration // 默认 1s
	FetchBackoff   time.Duration // 默认 1s
	MaxBlobBytes   int           // 默认 MaxSnapshotBlobBytes
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
}

// SnapshotConsumer 逐条落 player_snapshot(source=1)。
type SnapshotConsumer struct {
	cfg    SnapshotConsumerConfig
	reader MessageReader
	sink   SnapshotSink
	health *Health
}

// NewSnapshotConsumer 组装消费者;Run 之前不做任何 IO。
func NewSnapshotConsumer(cfg SnapshotConsumerConfig, reader MessageReader, sink SnapshotSink) *SnapshotConsumer {
	cfg.withDefaults()
	return &SnapshotConsumer{cfg: cfg, reader: reader, sink: sink, health: newHealth(ConsumerSnapshot)}
}

// Health 返回健康标志。
func (c *SnapshotConsumer) Health() *Health { return c.health }

// Run 阻塞消费直到 ctx 结束(返回 nil)或 DB 重试用尽(返回 ErrConsumerStoppedOnDB,
// 当前消息 offset 未提交)。
func (c *SnapshotConsumer) Run(ctx context.Context) error {
	c.health.markRunning()
	defer c.health.markStopped()

	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				logx.Infof("[%s] consumer exiting on shutdown", ConsumerSnapshot)
				return nil
			}
			logx.Errorf("[%s] fetch failed, retry in %s: %v", ConsumerSnapshot, c.cfg.FetchBackoff, err)
			if !sleepCtx(ctx, c.cfg.FetchBackoff) {
				return nil
			}
			continue
		}

		row, ok := c.decode(msg)
		if !ok {
			c.commit(ctx, msg)
			continue
		}

		id, inserted, err := c.insertWithRetry(ctx, row)
		if err != nil {
			metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "db_error", 1)
			c.health.markFailed(err)
			logx.Errorf("[%s] STOPPED: snapshot guid=%d player=%d could not be inserted after %d attempts, offset NOT committed (lag is preferred over loss): %v",
				ConsumerSnapshot, row.SnapshotGuid, row.PlayerID, c.cfg.DBMaxAttempts, err)
			return fmt.Errorf("%w: %v", ErrConsumerStoppedOnDB, err)
		}
		if inserted {
			metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "inserted", 1)
		} else {
			metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "duplicate", 1)
			logx.Infof("[%s] duplicate snapshot guid=%d player=%d already stored as id=%d, skipped",
				ConsumerSnapshot, row.SnapshotGuid, row.PlayerID, id)
		}
		c.commit(ctx, msg)
	}
}

// decode 把一条消息解成 source=1 的行;坏消息返回 ok=false,调用方只提交不入库。
// data 列存**收到的原始字节**(整条 PlayerSnapshotEntry,含两个 player_database blob 与
// schema_version),而不是拆开的 blob:source=1 的恢复路径是下一阶段,存原样最不丢信息。
func (c *SnapshotConsumer) decode(msg kafkago.Message) (*store.SnapshotRow, bool) {
	entry := &rollbackpb.PlayerSnapshotEntry{}
	if err := proto.Unmarshal(msg.Value, entry); err != nil {
		logx.Errorf("[%s] undecodable message skipped partition=%d offset=%d key=%s: %v",
			ConsumerSnapshot, msg.Partition, msg.Offset, string(msg.Key), err)
		metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "decode_error", 1)
		return nil, false
	}
	if entry.GetSnapshotId() == 0 || entry.GetPlayerId() == 0 {
		// 生产者对 snapshot_id=0 fail-closed;player_id=0 的快照无主。两者都无法按 guid 去重 / 按玩家检索。
		logx.Errorf("[%s] invalid snapshot skipped partition=%d offset=%d snapshot_id=%d player_id=%d",
			ConsumerSnapshot, msg.Partition, msg.Offset, entry.GetSnapshotId(), entry.GetPlayerId())
		metrics.ObserveKafkaConsumerMessages(ConsumerSnapshot, "invalid", 1)
		return nil, false
	}
	if len(msg.Value) > c.cfg.MaxBlobBytes {
		logx.Errorf("[%s] oversize snapshot skipped partition=%d offset=%d snapshot_id=%d player_id=%d bytes=%d limit=%d",
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

func (c *SnapshotConsumer) insertWithRetry(ctx context.Context, row *store.SnapshotRow) (uint64, bool, error) {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.DBMaxAttempts; attempt++ {
		id, inserted, err := c.sink.InsertSnapshotIfGuidAbsent(ctx, row)
		if err == nil {
			return id, inserted, nil
		}
		lastErr = err
		logx.Errorf("[%s] insert snapshot guid=%d player=%d failed (attempt %d/%d): %v",
			ConsumerSnapshot, row.SnapshotGuid, row.PlayerID, attempt, c.cfg.DBMaxAttempts, err)
		if attempt < c.cfg.DBMaxAttempts && !sleepCtx(ctx, c.cfg.DBRetryBackoff) {
			return 0, false, fmt.Errorf("interrupted during db retry: %w", lastErr)
		}
	}
	return 0, false, lastErr
}

// commit 同步提交一条;失败只记日志(行已落库,重放会被 guid 去重)。
func (c *SnapshotConsumer) commit(ctx context.Context, msg kafkago.Message) {
	if err := c.reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
		logx.Errorf("[%s] commit offset failed partition=%d offset=%d (row is persisted; replay is deduped by guid): %v",
			ConsumerSnapshot, msg.Partition, msg.Offset, err)
	}
}
