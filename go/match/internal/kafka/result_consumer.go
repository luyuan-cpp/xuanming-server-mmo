package kafka

import (
	"context"
	"errors"
	"fmt"
	"shared/drainproof"
	"strconv"
	"sync"
	"time"

	"match/internal/metrics"

	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"

	"shared/kafkautil"
	"shared/safego"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 对局结果消费者(设计文档 cross-zone-matchmaking.md §11):battle 节点打完一局
// 把 contracts.kafka.BattleResultEvent 发到 ResultTopic(默认 match-results,
// 全局无 zone 段,key=battle_id,payload 直接是事件、不套 *Command 信封),
// match 用消费组 ResultConsumerGroup(默认 match-rating)消费并更新 Elo。
//
// 多实例:同组分摊分区,每条结果只被一个实例消费;重复投递(rebalance /
// 提交前崩溃)由 handler 的 SQL 入账收据幂等兜底。
// 只在入账确认后提交位点;失败关闭并重建 reader 恢复原消息。
// 本文件不 import internal/logic(svc → kafka → logic → svc 会成环),
// 入账逻辑由 main 以回调注入。

// ResultConsumerConfig 消费者参数(来自 config.Config,由 main 装配)。
type ResultConsumerConfig struct {
	RelayAdmission *drainproof.Session
	BindOwner      func(*kafkautil.BoundConsumerGroup) error
	Brokers        []string
	Topic          string
	GroupID        string
	Partitions     int32
	// RequireDurability 要求结果 topic 严格三副本、minISR>=2,拒绝现存单副本主题(config.ResultTopicRequiresDurability)。
	// 只有 dev / test 档放行:本地 Kafka 通常单 broker。
	RequireDurability bool
}

// ResultHandler 处理一条对局结果：error 表示入账未确认，保留原位点恢复。
// nil 即确认终态（含“不计分”/“重复”）。
type ResultHandler func(ctx context.Context, event *kafkapb.BattleResultEvent) error

// ResultRecordHandler 在评分与推进位点前保存Kafka完整原件；不得用重序列化代替raw。
// 来源归档不代表个人资产已到账，也不驱动R07重投或销账。
type ResultRecordHandler func(ctx context.Context, event *kafkapb.BattleResultEvent, raw []byte, topic string, partition int, offset int64) error

// 消费者常量:handler 失败重试上限与退避;拉取失败的退避;topic 保留 7 天
// (与入账标记 TTL 同口径,重复投递不会跨这么久)。
const (
	resultHandleMaxAttempts = 3
	resultHandleBackoff     = time.Second
	resultFetchBackoff      = time.Second
	resultTopicRetentionMs  = 7 * 24 * 3600 * 1000
)

// EnsureResultTopic 核验结果主题，独立于是否启用评分。
// 必须在启动 matcher 与开放任何成局 RPC 之前同步通过此门禁。
func EnsureResultTopic(cfg ResultConsumerConfig) error {
	if len(cfg.Brokers) == 0 {
		return fmt.Errorf("Kafka.Brokers 为空,无法消费对局结果")
	}
	if cfg.Topic == "" {
		return fmt.Errorf("ResultTopic 不能为空")
	}
	partitions := cfg.Partitions
	if partitions <= 0 {
		partitions = 3
	}
	// 启动时确保 topic 存在(照 login / db 的 kafkautil.EnsureTopics):分区数是
	// 不可变契约,与 broker 现状不一致会直接报错,让部署问题在启动时暴露。
	topic := kafkautil.TopicSpec{
		Name:        cfg.Topic,
		Partitions:  partitions,
		RetentionMs: resultTopicRetentionMs,
	}
	if cfg.RequireDurability {
		// 不能丢失已确认结果：严格三副本、minISR>=2，拒绝现存单副本主题。
		topic.ReplicaFactor = 3
		topic.RequireDurability = true
	}
	if err := kafkautil.EnsureTopics(cfg.Brokers, []kafkautil.TopicSpec{topic}); err != nil {
		return fmt.Errorf("确保对局结果 topic %s 存在失败: %w", cfg.Topic, err)
	}
	return nil
}

// StartResultConsumer 确保 topic 存在后起消费 goroutine(ctx 结束即退出)。
// handler 在本实例内串行调用，结果只有 SQL 确认后才提交位点。
func StartResultConsumer(ctx context.Context, cfg ResultConsumerConfig, handle ResultHandler, record ResultRecordHandler) error {
	if handle == nil || record == nil {
		return fmt.Errorf("结果消费者必须装配完整来源归档，处理器不能缺失")
	}
	if cfg.GroupID == "" {
		return fmt.Errorf("ResultConsumerGroup 不能为空")
	}
	if err := EnsureResultTopic(cfg); err != nil {
		return err
	}

	if cfg.RelayAdmission != nil {
		if cfg.BindOwner == nil {
			return fmt.Errorf("结果消费者缺少实际生命周期 owner")
		}
		var serial sync.Mutex
		owner, err := kafkautil.NewBoundConsumerGroup(kafkautil.BoundConsumerGroupConfig{Group: kafkago.ConsumerGroupConfig{ID: cfg.GroupID, Brokers: cfg.Brokers, Topics: []string{cfg.Topic}, StartOffset: kafkago.FirstOffset}, Reader: kafkago.ReaderConfig{MinBytes: 1, MaxBytes: 1 << 20, MaxWait: 500 * time.Millisecond}, HandlerTimeout: 2 * time.Minute}, cfg.RelayAdmission, func(call context.Context, message kafkago.Message) error {
			serial.Lock()
			defer serial.Unlock()
			if err := call.Err(); err != nil {
				return err
			}
			return processResultRecord(call, message, handle, sleepCtx, record)
		})
		if err != nil {
			return err
		}
		if err = cfg.BindOwner(owner); err != nil {
			return err
		}
		safego.Go("match.kafka.results", func() {
			if err := owner.Run(ctx); err != nil {
				cfg.RelayAdmission.Hold()
				logx.Errorf("结果消费者 HOLD: %v", err)
			}
		})
		return nil
	}

	logx.Infof("[rating] Battle result consumer started topic=%s group=%s", cfg.Topic, cfg.GroupID)
	safego.Go("match.kafka.results", func() {
		delay := resultFetchBackoff
		for ctx.Err() == nil {
			reader := kafkago.NewReader(kafkago.ReaderConfig{
				Brokers: cfg.Brokers,
				GroupID: cfg.GroupID,
				Topic:   cfg.Topic,
				// 新消费组从最早开始:match 晚于 battle 启动时,先发的结果不能丢。
				StartOffset: kafkago.FirstOffset,
				MinBytes:    1,
				MaxBytes:    1 << 20,
				MaxWait:     500 * time.Millisecond,
				// CommitInterval=0:每条处理完同步提交,崩溃最多重放一条(幂等兜底)。
				CommitInterval: 0,
			})
			func() {
				defer func() {
					if err := reader.Close(); err != nil {
						logx.Error("关闭评级结果消费者失败")
					}
				}()
				runResultConsumer(ctx, reader, handle, sleepCtx, record)
			}()
			if ctx.Err() != nil {
				return
			}
			// 未确认的位点绝不提交；换 reader 后从 broker 的原位点恢复。每轮最多三次业务调用。
			logx.Error("评级 SQL 尚未确认，保留 Kafka 原位点并退避恢复")
			if !sleepCtx(ctx, delay) {
				return
			}
			delay = min(delay*2, 30*time.Second)
		}
	})
	return nil
}

// runResultConsumer 拉取 → 反序列化 → handler(有限重试)→ 提交,直到 ctx 结束。
type resultReader interface {
	FetchMessage(context.Context) (kafkago.Message, error)
	CommitMessages(context.Context, ...kafkago.Message) error
}

func runResultConsumer(ctx context.Context, reader resultReader, handle ResultHandler, wait func(context.Context, time.Duration) bool, record ResultRecordHandler) {
	for {
		message, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			logx.Errorf("结果拉取失败，等待原有退避: %v", err)
			if !wait(ctx, resultFetchBackoff) {
				return
			}
			continue
		}
		if err = processResultRecord(ctx, message, handle, wait, record); err != nil {
			return
		}
		commitResult(ctx, reader, message)
	}
}

// processResultRecord 只返回业务结局；实际 offset 提交由唯一 consumer owner 完成。
func processResultRecord(ctx context.Context, msg kafkago.Message, handle ResultHandler, wait func(context.Context, time.Duration) bool, record ResultRecordHandler) error {
	event := &kafkapb.BattleResultEvent{}
	if err := proto.Unmarshal(msg.Value, event); err != nil {
		metrics.ObserveRatingUpdate("unknown", "decode_error")
		logx.Errorf("结果原件解码失败 partition=%d offset=%d: %v", msg.Partition, msg.Offset, err)
		return err
	}
	if key := string(msg.Key); key != "" && key != strconv.FormatUint(event.GetBattleId(), 10) {
		logx.Errorf("结果 key 与 battle_id 不符 partition=%d offset=%d", msg.Partition, msg.Offset)
	}
	var last error
	for attempt := 1; attempt <= resultHandleMaxAttempts; attempt++ {
		last = record(ctx, event, msg.Value, msg.Topic, msg.Partition, msg.Offset)
		if last == nil {
			last = handle(ctx, event)
		}
		if last == nil {
			return nil
		}
		logx.Errorf("结果处理未确认 attempt=%d/%d battle=%d mode=%s: %v", attempt, resultHandleMaxAttempts, event.GetBattleId(), matchpb.MatchMode(event.GetMatchMode()).String(), last)
		if attempt < resultHandleMaxAttempts && !wait(ctx, resultHandleBackoff) {
			return errors.Join(last, ctx.Err())
		}
	}
	return last
}

// commitResult 同步提交一条消息的 offset;失败只记日志(下次提交会覆盖,
// 崩溃最多重放到上次成功提交处,幂等标记兜底)。
func commitResult(ctx context.Context, reader resultReader, msg kafkago.Message) {
	if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
		logx.Errorf("[rating] Failed to commit battle result offset partition=%d offset=%d: %v", msg.Partition, msg.Offset, err)
	}
}

// sleepCtx 可中断的 sleep;ctx 结束返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
