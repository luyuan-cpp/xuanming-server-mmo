package kafka

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
// 提交前崩溃)由 handler 侧的 match:rating:applied:{battle_id} SETNX 幂等兜底。
// 本文件不 import internal/logic(svc → kafka → logic → svc 会成环),
// 入账逻辑由 main 以回调注入。

// ResultConsumerConfig 消费者参数(来自 config.Config,由 main 装配)。
type ResultConsumerConfig struct {
	Brokers    []string
	Topic      string
	GroupID    string
	Partitions int32
}

// ResultHandler 处理一条对局结果:返回 error 表示可重试的失败(Redis 抖动),
// 消费者会有限次重试后跳过;nil 即终态(含"不计分"/"重复")。
type ResultHandler func(ctx context.Context, event *kafkapb.BattleResultEvent) error

// 消费者常量:handler 失败重试上限与退避;拉取失败的退避;topic 保留 7 天
// (与入账标记 TTL 同口径,重复投递不会跨这么久)。
const (
	resultHandleMaxAttempts = 3
	resultHandleBackoff     = time.Second
	resultFetchBackoff      = time.Second
	resultTopicRetentionMs  = 7 * 24 * 3600 * 1000
)

// StartResultConsumer 确保 topic 存在后起消费 goroutine(ctx 结束即退出)。
// 立即返回;handler 在消费 goroutine 里串行调用(同一实例内有序,足够:
// 不同局互不相关,同一局只有一条结果)。
func StartResultConsumer(ctx context.Context, cfg ResultConsumerConfig, handle ResultHandler) error {
	if len(cfg.Brokers) == 0 {
		return fmt.Errorf("Kafka.Brokers 为空,无法消费对局结果")
	}
	if cfg.Topic == "" || cfg.GroupID == "" {
		return fmt.Errorf("ResultTopic / ResultConsumerGroup 不能为空")
	}
	partitions := cfg.Partitions
	if partitions <= 0 {
		partitions = 3
	}
	// 启动时确保 topic 存在(照 login / db 的 kafkautil.EnsureTopics):分区数是
	// 不可变契约,与 broker 现状不一致会直接报错,让部署问题在启动时暴露。
	if err := kafkautil.EnsureTopics(cfg.Brokers, []kafkautil.TopicSpec{{
		Name:        cfg.Topic,
		Partitions:  partitions,
		RetentionMs: resultTopicRetentionMs,
	}}); err != nil {
		return fmt.Errorf("确保对局结果 topic %s 存在失败: %w", cfg.Topic, err)
	}

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
	logx.Infof("[rating] 对局结果消费者启动 topic=%s group=%s brokers=%v", cfg.Topic, cfg.GroupID, cfg.Brokers)
	safego.Go("match.kafka.results", func() {
		defer func() {
			if err := reader.Close(); err != nil {
				logx.Errorf("[rating] 关闭对局结果 reader 失败: %v", err)
			}
		}()
		runResultConsumer(ctx, reader, handle)
	})
	return nil
}

// runResultConsumer 拉取 → 反序列化 → handler(有限重试)→ 提交,直到 ctx 结束。
func runResultConsumer(ctx context.Context, reader *kafkago.Reader, handle ResultHandler) {
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				logx.Info("[rating] 对局结果消费者退出")
				return
			}
			logx.Errorf("[rating] 拉取对局结果失败,%s 后重试: %v", resultFetchBackoff, err)
			if !sleepCtx(ctx, resultFetchBackoff) {
				return
			}
			continue
		}

		event := &kafkapb.BattleResultEvent{}
		if err := proto.Unmarshal(msg.Value, event); err != nil {
			// 坏消息跳过并提交:留在 topic 里只会每次重启都撞一遍。
			logx.Errorf("[rating] 对局结果反序列化失败,跳过 partition=%d offset=%d key=%s: %v",
				msg.Partition, msg.Offset, string(msg.Key), err)
			metrics.ObserveRatingUpdate("unknown", "decode_error")
			commitResult(ctx, reader, msg)
			continue
		}
		if key := string(msg.Key); key != "" && key != strconv.FormatUint(event.GetBattleId(), 10) {
			// key 契约是 battle_id;不一致只是日志,payload 里的 battle_id 为准。
			logx.Errorf("[rating] 对局结果 key=%s 与 battle_id=%d 不一致", key, event.GetBattleId())
		}

		handled := false
		for attempt := 1; attempt <= resultHandleMaxAttempts; attempt++ {
			if err := handle(ctx, event); err == nil {
				handled = true
				break
			} else {
				logx.Errorf("[rating] 对局结果处理失败(第 %d/%d 次) battle=%d mode=%s: %v",
					attempt, resultHandleMaxAttempts, event.GetBattleId(),
					matchpb.MatchMode(event.GetMatchMode()).String(), err)
			}
			if attempt < resultHandleMaxAttempts && !sleepCtx(ctx, resultHandleBackoff) {
				return
			}
		}
		if !handled {
			// 重试用尽:跳过这一局(评分是软数据,不能让一条坏结果卡住整个分区)。
			logx.Errorf("[rating] 对局结果重试用尽,跳过 battle=%d", event.GetBattleId())
		}
		commitResult(ctx, reader, msg)
	}
}

// commitResult 同步提交一条消息的 offset;失败只记日志(下次提交会覆盖,
// 崩溃最多重放到上次成功提交处,幂等标记兜底)。
func commitResult(ctx context.Context, reader *kafkago.Reader, msg kafkago.Message) {
	if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
		logx.Errorf("[rating] 提交对局结果 offset 失败 partition=%d offset=%d: %v", msg.Partition, msg.Offset, err)
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
