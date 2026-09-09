package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"shared/kafkautil"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// StartConfig 由 main 从 config.KafkaConfig 装配。
type StartConfig struct {
	Brokers []string

	TxLogTopic      string
	TxLogPartitions int32
	TxLogGroup      string

	SnapshotTopic      string
	SnapshotPartitions int32
	SnapshotGroup      string

	// RetentionMs 两个 topic 的保留期(<=0 用 broker 默认)。
	RetentionMs int64

	// ResumeAfter:消费者因 DB 故障停下(offset 未提交)之后,多久重建 reader 再试。
	// 停下本身是为了"宁可积压不许丢";但 data_service 同时承载 Load/Save 热路径,
	// 不能靠重启进程来恢复消费,所以由本包的 supervisor 在冷却后从断点续消费。
	// 0 取默认 60s;负数 = 永不自动恢复(只能重启进程)。
	ResumeAfter time.Duration
}

const defaultResumeAfter = 60 * time.Second

func (c StartConfig) validate() error {
	if len(c.Brokers) == 0 {
		return errors.New("Kafka.Brokers is empty")
	}
	if c.TxLogTopic == "" || c.TxLogGroup == "" {
		return errors.New("Kafka.TransactionLogTopic / TransactionLogConsumerGroup must be set")
	}
	if c.SnapshotTopic == "" || c.SnapshotGroup == "" {
		return errors.New("Kafka.SnapshotTopic / SnapshotConsumerGroup must be set")
	}
	if c.TxLogPartitions <= 0 || c.SnapshotPartitions <= 0 {
		return errors.New("Kafka partition counts must be > 0")
	}
	return nil
}

// Consumers 是已启动的两条消费者的句柄:健康标志 + 一个"都退出了"的等待点。
type Consumers struct {
	TxLog    *Health
	Snapshot *Health

	// wg 覆盖两条 supervisor goroutine 的整个生命周期(含关停时的 flushOnShutdown)。
	wg sync.WaitGroup
}

// Wait 等两条消费者 goroutine 都退出,最多等 timeout;返回是否等到。
//
// 存在的理由是关停顺序:txlog 的 flushOnShutdown 会用一条独立的 5s ctx 去插最后一批,
// 而 main 的 defer 若同时把 store 的连接池关掉,那次 flush 必然拿到 "sql: database is
// closed" —— 一段永远跑不成的代码。main 必须先 cancel、再 Wait(有界)、最后才
// svcCtx.Close()。超时返回 false:关停不能被一个连不上的 DB 无限拖住。
func (c *Consumers) Wait(timeout time.Duration) bool {
	if c == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Start 确保两个 topic 存在(分区数是不可变契约)后,各起一条消费 goroutine,立即返回。
// EnsureTopics 是唯一的启动失败点(sarama admin 连不上 broker);reader 本身不拨号,
// broker 后来才可达也能正常开始消费。调用方对返回的 error 只该做"记日志 + 稍后重试",
// 绝不能让它拖死 data_service。
//
// **本函数会阻塞**:半死不活的 broker 上 EnsureTopics 要走完 sarama 的
// Net.DialTimeout(默认 30s)加若干次元数据重试。所以连第一次尝试也必须在后台
// goroutine 里做,不能挡在 gRPC 服务器起来之前 —— 否则 LoadPlayerData / SavePlayerData
// 这条玩家数据热路径会被一个纯审计用途的组件按住几十秒(见 data_service.go
// startKafkaConsumers)。
func Start(ctx context.Context, cfg StartConfig, txSink TxLogSink, snapSink SnapshotSink) (*Consumers, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if txSink == nil || snapSink == nil {
		return nil, errors.New("kafka consumers need both the transaction_log and snapshot stores")
	}
	if cfg.ResumeAfter == 0 {
		cfg.ResumeAfter = defaultResumeAfter
	}

	if err := kafkautil.EnsureTopics(cfg.Brokers, []kafkautil.TopicSpec{
		{Name: cfg.TxLogTopic, Partitions: cfg.TxLogPartitions, RetentionMs: cfg.RetentionMs},
		{Name: cfg.SnapshotTopic, Partitions: cfg.SnapshotPartitions, RetentionMs: cfg.RetentionMs},
	}); err != nil {
		return nil, fmt.Errorf("ensure kafka topics: %w", err)
	}

	consumers := &Consumers{
		TxLog:    newHealth(ConsumerTxLog),
		Snapshot: newHealth(ConsumerSnapshot),
	}

	consumers.wg.Add(2)
	safego.Go("data_service.kafka.transaction_log", func() {
		defer consumers.wg.Done()
		supervise(ctx, ConsumerTxLog, cfg.ResumeAfter, func() (runner, MessageReader) {
			reader := newReader(cfg.Brokers, cfg.TxLogTopic, cfg.TxLogGroup)
			c := NewTxLogConsumer(TxLogConsumerConfig{}, reader, txSink)
			c.health = consumers.TxLog
			return c, reader
		})
	})
	safego.Go("data_service.kafka.player_snapshot", func() {
		defer consumers.wg.Done()
		supervise(ctx, ConsumerSnapshot, cfg.ResumeAfter, func() (runner, MessageReader) {
			reader := newReader(cfg.Brokers, cfg.SnapshotTopic, cfg.SnapshotGroup)
			c := NewSnapshotConsumer(SnapshotConsumerConfig{}, reader, snapSink)
			c.health = consumers.Snapshot
			return c, reader
		})
	})

	logx.Infof("[kafka] consumers started: %s(topic=%s group=%s p=%d) %s(topic=%s group=%s p=%d) brokers=%v",
		ConsumerTxLog, cfg.TxLogTopic, cfg.TxLogGroup, cfg.TxLogPartitions,
		ConsumerSnapshot, cfg.SnapshotTopic, cfg.SnapshotGroup, cfg.SnapshotPartitions, cfg.Brokers)
	return consumers, nil
}

type runner interface {
	Run(ctx context.Context) error
}

// supervise 跑一条消费者直到 ctx 结束。Run 因 DB 故障返回时:关 reader(未提交的 offset
// 留在 broker 上)、冷却 resumeAfter、重建 reader 从上次提交处续消费。
// resumeAfter < 0 时不恢复,只留下健康标志与日志。
func supervise(ctx context.Context, name string, resumeAfter time.Duration, build func() (runner, MessageReader)) {
	for {
		c, reader := build()
		err := c.Run(ctx)
		if closeErr := reader.Close(); closeErr != nil {
			logx.Errorf("[%s] close reader: %v", name, closeErr)
		}
		if ctx.Err() != nil || err == nil {
			return
		}
		if resumeAfter < 0 {
			logx.Errorf("[%s] consumer stopped and auto-resume is disabled; restart data_service after the database recovers: %v", name, err)
			return
		}
		logx.Errorf("[%s] consumer stopped; will rebuild the reader and resume from the last committed offset in %s: %v", name, resumeAfter, err)
		if !sleepCtx(ctx, resumeAfter) {
			return
		}
	}
}
