package kafkautil

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

// BoundConsumerGroup 的低基数指标（r2-kc#1）：label 只有消费组名（配置常量）与固定阶段名，不含分区、offset、玩家或对局号。
// 它们回答「owner 在原地重试还是已经停了」：retry 涨 = 暂时失败、消费者还活着；released 涨 = 再均衡时有在途记录交还重投；
// hold 涨 = owner 记了 HOLD，维护排空之外出现就是消费者永久停止取新（不变量破坏 / 提交点越过保留期），要人看。
// 惰性注册，理由同 shared/safego：本包被几乎所有服务 import，init 期 MustRegister 撞名会让进程起不来。
const (
	boundStageJoin   = "join"   // 等下一代次（入组 / 同步 / 读位点）失败
	boundStageOpen   = "open"   // 打开分到的分区失败
	boundStageFetch  = "fetch"  // 分区拉取失败
	boundStageHandle = "handle" // 回调失败（返回错误、超时或 panic）
	boundStageCommit = "commit" // 提交失败
)

var boundStages = []string{boundStageJoin, boundStageOpen, boundStageFetch, boundStageHandle, boundStageCommit}

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

	boundMetricsOnce sync.Once
)

func registerBoundConsumerMetrics() {
	boundMetricsOnce.Do(func() {
		for _, collector := range []prometheus.Collector{boundRetryTotal, boundReleasedTotal, boundHoldTotal} {
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
// 否则第一次重试 / HOLD 之前序列不存在，`increase(...) > 0` 恰好漏掉第一次。
func primeBoundConsumerMetrics(group string) {
	registerBoundConsumerMetrics()
	for _, stage := range boundStages {
		boundRetryTotal.WithLabelValues(group, stage).Add(0)
	}
	boundReleasedTotal.WithLabelValues(group).Add(0)
	boundHoldTotal.WithLabelValues(group).Add(0)
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
