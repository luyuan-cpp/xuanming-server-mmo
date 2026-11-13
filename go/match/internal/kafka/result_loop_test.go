package kafka

import (
	"context"
	"errors"
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

// 测试用的最小消费循环:按消费组 owner(kafkautil.BoundConsumerGroup)对回调结局的口径驱动 processResultRecord ——
// 回调返回 nil 才提交该条位点,返回 error 就不提交、本轮结束(owner 会退避后原地重调同一条)。
// 生产不再有自建 reader 的循环(2026-11-12 起 StartResultConsumer 只走 owner,见其注释);这里保留它,
// 让单测与真 Kafka + TiDB 的归档集成测试(result_archive_integration_test.go)照旧核「先归档、后评分、最后提交」与
// 「没确认就不推进位点」,不用每条都起一个消费组 owner。

type resultReader interface {
	FetchMessage(context.Context) (kafkago.Message, error)
	CommitMessages(context.Context, ...kafkago.Message) error
}

// testResultFetchBackoff 是拉取失败后的退避(经 wait 接缝,单测替身立即返回)。
const testResultFetchBackoff = time.Second

func runResultConsumer(ctx context.Context, reader resultReader, handle ResultHandler, wait func(context.Context, time.Duration) bool, record ResultRecordHandler) {
	for {
		message, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			if !wait(ctx, testResultFetchBackoff) {
				return
			}
			continue
		}
		if err = processResultRecord(ctx, message, handle, wait, record); err != nil {
			return
		}
		_ = reader.CommitMessages(ctx, message)
	}
}
