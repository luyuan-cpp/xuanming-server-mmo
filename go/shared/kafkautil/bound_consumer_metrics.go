package kafkautil

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

// BoundConsumerGroup 的低基数指标：label 只有消费组名（配置常量）、固定阶段名与固定原因名，不含分区、offset、玩家或对局号。
// 它们回答「owner 在原地重试还是已经停了」：retry 涨 = 暂时失败、消费者还活着；released 涨 = 再均衡或排空离组时有在途记录交还重投；
// dead_lettered 涨 = 有记录转了死信，要按手册重放；retention_loss 涨 = 保留期删掉了本组还没处理的记录（已 HOLD）；
// hold 涨 = owner 记了 HOLD，维护排空之外出现就是消费者永久停止取新，要人看。告警见 deploy/k8s/kafka-bound-consumer-alerts.yaml。
// 惰性注册，理由同 shared/safego：本包被几乎所有服务 import，init 期 MustRegister 撞名会让进程起不来。
const (
	boundStageJoin       = "join"        // 等下一代次（入组 / 同步 / 读位点）失败
	boundStageOpen       = "open"        // 打开分到的分区失败
	boundStageFetch      = "fetch"       // 分区拉取失败，或取到的记录前面有缺口而读日志起点失败
	boundStageHandle     = "handle"      // 回调失败（返回错误、超时或 panic）
	boundStageDeadLetter = "dead_letter" // 写死信失败（记录留在原处，退避后重写）
	boundStageCommit     = "commit"      // 提交失败
)

var boundStages = []string{boundStageJoin, boundStageOpen, boundStageFetch, boundStageHandle, boundStageDeadLetter, boundStageCommit}

var (
	boundRetryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_bound_consumer_retry_total",
		Help: "Transient failures a BoundConsumerGroup retried without stopping, by consumer group and stage (join|open|fetch|handle|commit).",
	}, []string{"group", "stage"})
	boundReleasedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_bound_consumer_released_total",
		Help: "Accepted records returned to Kafka uncommitted for redelivery (generation ended or commit rejected while the owner kept running), by consumer group.",
	}, []string{"group"})
	boundHoldTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_bound_consumer_hold_total",
		Help: "HOLD (unknown outcome) events recorded by a BoundConsumerGroup, by consumer group. Outside a maintenance drain any increase means the owner stopped fetching for good.",
	}, []string{"group"})
	boundDeadLetteredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_bound_consumer_dead_lettered_total",
		Help: "Accepted records written to the consumer group's dead-letter topic before the offset was committed past them, by consumer group and cause (unprocessable|exhausted). Each one needs a manual replay after the cause is fixed.",
	}, []string{"group", "cause"})
	boundRetentionLossTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_bound_consumer_retention_loss_total",
		Help: "Times a BoundConsumerGroup found records deleted by topic retention before the group processed them; the owner HOLDs and stops fetching instead of skipping them. By consumer group.",
	}, []string{"group"})

	boundMetricsOnce sync.Once
)

func registerBoundConsumerMetrics() {
	boundMetricsOnce.Do(func() {
		for _, collector := range []prometheus.Collector{boundRetryTotal, boundReleasedTotal, boundHoldTotal, boundDeadLetteredTotal, boundRetentionLossTotal} {
			if err := prometheus.Register(collector); err != nil {
				var already prometheus.AlreadyRegisteredError
				if !errors.As(err, &already) {
					logx.Errorf("[kafka-bound] failed to register metrics: %v", err)
				}
			}
		}
	})
}

// primeBoundConsumerMetrics 在构造 owner 时把本组的全部序列预置为 0：
// 否则第一次重试 / HOLD 之前序列不存在，`increase(...) > 0` 恰好漏掉第一次。死信序列只给打开了死信的组预置
// （没打开的组永远不会有，预置出恒为 0 的序列只会让面板看着像「在跑」）。
func primeBoundConsumerMetrics(group string, deadLetter bool) {
	registerBoundConsumerMetrics()
	for _, stage := range boundStages {
		if stage == boundStageDeadLetter && !deadLetter {
			continue
		}
		boundRetryTotal.WithLabelValues(group, stage).Add(0)
	}
	boundReleasedTotal.WithLabelValues(group).Add(0)
	boundHoldTotal.WithLabelValues(group).Add(0)
	boundRetentionLossTotal.WithLabelValues(group).Add(0)
	if deadLetter {
		for _, cause := range deadLetterCauses {
			boundDeadLetteredTotal.WithLabelValues(group, cause).Add(0)
		}
	}
}

func observeBoundRetry(group, stage string) {
	registerBoundConsumerMetrics()
	boundRetryTotal.WithLabelValues(group, stage).Inc()
}

func observeBoundReleased(group string, records int) {
	if records <= 0 {
		return
	}
	registerBoundConsumerMetrics()
	boundReleasedTotal.WithLabelValues(group).Add(float64(records))
}

func observeBoundHold(group string) {
	registerBoundConsumerMetrics()
	boundHoldTotal.WithLabelValues(group).Inc()
}

func observeBoundDeadLettered(group, cause string) {
	registerBoundConsumerMetrics()
	boundDeadLetteredTotal.WithLabelValues(group, cause).Inc()
}

func observeBoundRetentionLoss(group string) {
	registerBoundConsumerMetrics()
	boundRetentionLossTotal.WithLabelValues(group).Inc()
}
