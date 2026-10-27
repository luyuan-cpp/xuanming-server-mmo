package kafka

import (
	"context"
	"errors"
	"fmt"
	"shared/drainproof"
	"sync"
	"time"

	"shared/kafkautil"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// StartConfig 由 main 从 config.KafkaConfig 装配。
type StartConfig struct {
	// Owner 来自同一 boot；Start 重试及 supervisor 世代共用，不得替换来清除 UNKNOWN。
	Owner   *ConsumerOwner
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

	// Snapshot 是 player_snapshot 消费者的批量 / 并发参数(G2a);零值 = 逐条串行(2026-10-12 之前的行为)。
	Snapshot SnapshotConsumerConfig
	// LagProbeInterval 是消费组 lag / 保留期丢失量探针的周期;<= 0 = 不探。
	LagProbeInterval time.Duration
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
	wg          sync.WaitGroup
	owner       *ConsumerOwner
	cancelProbe context.CancelFunc
	waitOnce    sync.Once
	done        chan struct{}
}

// Wait 等两条消费者 goroutine 都退出,最多等 timeout;返回是否等到。
//
// 存在的理由是关停顺序:txlog 的 flushOnShutdown 会用一条独立的 5s ctx 去插最后一批,
// 而 main 若同时把 store 的连接池关掉，那次 flush 必然拿到 "sql: database is closed"。
// Wait 只提供等待结果；超时不构成排空证明，main 必须保留仍被 owner 使用的 SQL 池。
func (c *Consumers) waitDone() <-chan struct{} {
	c.waitOnce.Do(func() { c.done = make(chan struct{}); go func() { c.wg.Wait(); close(c.done) }() })
	return c.done
}
func (c *Consumers) Wait(timeout time.Duration) bool {
	if c == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return false
	case <-c.waitDone():
		return true
	}
}

// StopAndReport 只停 Fetch 与新 supervisor，已接收消息继续沿原 consumer 完成 SQL 与 Commit。
func (c *Consumers) StopAndReport(ctx context.Context) ([]byte, error) {
	if c == nil {
		return nil, drainproof.ErrHold
	}
	c.owner.BeginDrain()
	if c.cancelProbe != nil {
		c.cancelProbe()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.waitDone():
	}
	return c.owner.SettledDigest(ctx)
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
	if cfg.Owner == nil {
		return nil, errors.New("Kafka consumers 缺同 boot 生命周期 owner")
	}
	if cfg.Owner.Draining() {
		return nil, drainproof.ErrClosed
	}
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

	if err := cfg.Owner.attach(); err != nil {
		return nil, err
	}
	probeCtx, cancelProbe := context.WithCancel(ctx)
	consumers := &Consumers{
		owner:       cfg.Owner,
		cancelProbe: cancelProbe,
		TxLog:       newHealth(ConsumerTxLog),
		Snapshot:    newHealth(ConsumerSnapshot),
	}

	consumers.wg.Add(2)
	safego.Go("data_service.kafka.transaction_log", func() {
		defer consumers.wg.Done()
		var reader *ownedReader
		superviseOwned(ctx, ConsumerTxLog, cfg.ResumeAfter, func() (runner, MessageReader, error) {
			businessCtx, cancel := context.WithCancel(ctx)
			if reader == nil {
				group, err := newReader(cfg.Brokers, cfg.TxLogTopic, cfg.TxLogGroup, cfg.Owner.relay)
				if err != nil {
					cancel()
					return nil, nil, err
				}
				reader = &ownedReader{reader: group, owner: cfg.Owner, txlog: true}
				reader.startGroup(ctx, group)
				cfg.Owner.bindReader(ConsumerTxLog, cfg.TxLogGroup, cfg.TxLogTopic, group)
			}
			reader.stopRun = cancel
			c := NewTxLogConsumer(TxLogConsumerConfig{}, reader, &ownedTxLogSink{owner: cfg.Owner, sink: txSink})
			c.health = consumers.TxLog
			return &ownedRunner{runner: c, ctx: businessCtx, cancel: cancel}, reader, nil
		}, cfg.Owner)
	})
	safego.Go("data_service.kafka.player_snapshot", func() {
		defer consumers.wg.Done()
		var reader *ownedReader
		superviseOwned(ctx, ConsumerSnapshot, cfg.ResumeAfter, func() (runner, MessageReader, error) {
			businessCtx, cancel := context.WithCancel(ctx)
			if reader == nil {
				group, err := newReader(cfg.Brokers, cfg.SnapshotTopic, cfg.SnapshotGroup, cfg.Owner.relay)
				if err != nil {
					cancel()
					return nil, nil, err
				}
				reader = &ownedReader{reader: group, owner: cfg.Owner}
				reader.startGroup(ctx, group)
				cfg.Owner.bindReader(ConsumerSnapshot, cfg.SnapshotGroup, cfg.SnapshotTopic, group)
			}
			reader.stopRun = cancel
			c := NewSnapshotConsumer(cfg.Snapshot, reader, &ownedSnapshotSink{owner: cfg.Owner, sink: snapSink})
			c.health = consumers.Snapshot
			return &ownedRunner{runner: c, ctx: businessCtx, cancel: cancel}, reader, nil
		}, cfg.Owner)
	})

	if cfg.LagProbeInterval > 0 {
		targets := []lagTarget{
			{consumer: ConsumerTxLog, group: cfg.TxLogGroup, topic: cfg.TxLogTopic, partitions: cfg.TxLogPartitions},
			{consumer: ConsumerSnapshot, group: cfg.SnapshotGroup, topic: cfg.SnapshotTopic, partitions: cfg.SnapshotPartitions},
		}
		// 即使只读，探针也必须实际退出并关闭自己的连接，不能从生命周期 join 中漏掉。
		consumers.wg.Add(1)
		safego.Go(lagProbePoint, func() {
			defer consumers.wg.Done()
			runLagProbe(probeCtx, cfg.Brokers, cfg.LagProbeInterval, targets)
		})
	}

	// 生效的批量参数打出来:排障时要先确认这个实例到底是逐条还是批量(零值已按 withDefaults 的口径解释)。
	snap := cfg.Snapshot
	snap.withDefaults()
	logx.Infof("[kafka] consumers started: %s(topic=%s group=%s p=%d) %s(topic=%s group=%s p=%d batch_rows=%d batch_bytes=%d flush_interval=%s inflight=%d) lag_probe=%s brokers=%v",
		ConsumerTxLog, cfg.TxLogTopic, cfg.TxLogGroup, cfg.TxLogPartitions,
		ConsumerSnapshot, cfg.SnapshotTopic, cfg.SnapshotGroup, cfg.SnapshotPartitions,
		snap.BatchMaxRows, snap.BatchMaxBytes, snap.FlushInterval, snap.MaxInflightFlushes,
		cfg.LagProbeInterval, cfg.Brokers)
	return consumers, nil
}

type runner interface {
	Run(ctx context.Context) error
}

// supervise 保留既有测试接缝；生产用同一个 ownedReader 的消费组和票据。
// 确定性 DB 失败只重建批处理业务栈，实际重投并 Commit 后才完成原票据；UNKNOWN 不恢复。
// resumeAfter < 0 时不恢复，只留下健康标志与日志。
func supervise(ctx context.Context, name string, resumeAfter time.Duration, build func() (runner, MessageReader), owners ...*ConsumerOwner) {
	superviseOwned(ctx, name, resumeAfter, func() (runner, MessageReader, error) { c, r := build(); return c, r, nil }, owners...)
}
func superviseOwned(ctx context.Context, name string, resumeAfter time.Duration, build func() (runner, MessageReader, error), owners ...*ConsumerOwner) {
	var owner *ConsumerOwner
	if len(owners) == 1 {
		owner = owners[0]
	}
	var activeReader MessageReader
	defer func() {
		if reader, ok := activeReader.(*ownedReader); ok {
			if err := reader.Close(); err != nil {
				logx.Errorf("[%s] reader owner 关闭 HOLD: %v", name, err)
			}
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			if owner != nil {
				owner.hold(errors.New("Kafka supervisor panic，接收/关闭结局未知"))
			}
			panic(recovered)
		}
	}()
	for {
		if ctx.Err() != nil || owner != nil && owner.Draining() {
			return
		}
		c, reader, buildErr := build()
		if buildErr != nil {
			if owner != nil {
				owner.RecordUnknown(buildErr)
			}
			logx.Errorf("[%s] consumer 构造失败: %v", name, buildErr)
			return
		}
		activeReader = reader
		err := runAndClose(ctx, name, c, reader)
		if owner != nil && errors.Is(err, errSnapshotWorkerAborted) {
			owner.hold(err)
		}
		if ctx.Err() != nil || err == nil || owner != nil && owner.Draining() {
			return
		}
		if resumeAfter < 0 {
			logx.Errorf("[%s] consumer stopped and auto-resume is disabled; restart data_service after the database recovers: %v", name, err)
			return
		}
		logx.Errorf("[%s] consumer stopped; will rebuild the reader and resume from the last committed offset in %s: %v", name, resumeAfter, err)
		retryCtx, cancelRetry := context.WithCancel(ctx)
		stopRetry := func() bool { return false }
		if owner != nil {
			stopRetry = context.AfterFunc(owner.fetchCtx, cancelRetry)
		}
		keepRunning := sleepCtx(retryCtx, resumeAfter)
		stopRetry()
		cancelRetry()
		if !keepRunning {
			return
		}
	}
}

func runAndClose(ctx context.Context, name string, c runner, reader MessageReader) (err error) {
	if owned, ok := reader.(*ownedReader); ok {
		defer func() {
			if recovered := recover(); recovered != nil {
				owned.owner.RecordUnknown(errors.New("Kafka 业务 consumer panic，批次结果未知"))
				_ = owned.finishAttempt(drainproof.ErrHold)
				panic(recovered)
			}
		}()
		err = c.Run(ctx)
		return errors.Join(err, owned.finishAttempt(err))
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			logx.Errorf("[%s] close reader: %v", name, closeErr)
		}
	}()
	return c.Run(ctx)
}

func (c *Consumers) PreparedFrontier(ctx context.Context, runtime *drainproof.Runtime, role string) ([]byte, []byte, error) {
	if c == nil || c.owner == nil {
		return nil, nil, drainproof.ErrHold
	}
	return c.owner.PreparedFrontier(ctx, runtime, role)
}

func (c *Consumers) WithReady(ctx context.Context, raw []byte, commit func() ([]byte, error)) ([]byte, error) {
	if c == nil || c.owner == nil {
		return nil, drainproof.ErrHold
	}
	return c.owner.WithReady(ctx, raw, commit)
}
