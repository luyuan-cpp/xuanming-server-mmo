package kafka

import (
	"context"
	"time"

	"data_service/internal/metrics"

	"shared/kafkautil"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// lagProbePoint 是 lag 探针 goroutine 与每一轮的 safego 点位名(常量,不拼任何运行期值)。
const lagProbePoint = "data_service.kafka.lag_probe"

// lagTarget 是一条消费者在 broker 上要探的对象:哪个组、哪个 topic、几个分区。
type lagTarget struct {
	consumer, group, topic string
	partitions             int32
}

// runLagProbe 每 interval 探一次各消费组的 lag,直到 ctx 结束。只读 broker 元数据,不碰消费与落库。
//
// 为什么不用 kafka-go 的 Reader.Stats().Lag、也不按消息的 HighWaterMark 算:组模式下前者只反映最后读过的分区;
// 后者是各实例各自看到的局部值,rebalance 后旧主人的数留成陈值。要的是 broker 上「这个组在这个 topic 上还差多少」,
// 与哪个实例拿着哪个分区无关 —— 每个实例报同一个数,告警取 max。
//
// 探不到只记 lag_probe_total{result="error"} 并在下一轮重建连接,gauge 保留上一次的值
// (陈值会参与告警的 max;由按实例触发的 LagProbeBlind 点名)。
// 每一轮用 safego.Run 包住,一轮 panic 不杀循环;panic 的那一轮持有的连接(含这一轮刚建的)关掉,下一轮重建。第一轮在 interval 之后。
func runLagProbe(ctx context.Context, brokers []string, interval time.Duration, targets []lagTarget) {
	// prober 只在本 goroutine 里读写。一轮里新建的连接一建好就写回这里(probeLagOnce 收指针),那一轮半途 panic 时
	// 下面照样关得到它;以前新连接只在一轮正常返回时才赋给外层变量,panic 一次就漏一条 broker 连接(同 kafkautil.RunGroupLagProbe)。
	var prober *kafkautil.GroupLagProber
	defer closeLagProber(&prober, "close")

	// 每条消费者上一轮看到的「没提交过的分区数」:只在它变化时打 Info,低流量环境里永远收不到消息的分区
	// 不至于每 30s 刷一行。
	lastUncommitted := make(map[string]int, len(targets))
	// 各消费者的停滞计时,跨轮保留、不随连接重建清零。判据是「有积压的分区上组内已提交位点停着」,不依赖新消息到达:
	// 收不到新消息的分区卡住也看得见(2026-11-12)。reader 模式提交失败交还已落库的记录时,消费组 owner 结束本代次、
	// 由下一代次立即重投并提交,不会再有「已落库却一直挂着」的正常积压(kafkautil.BoundConsumerGroup.releaseUncommitted)。
	stalls := make(map[string]*kafkautil.CommitStall, len(targets))
	for _, t := range targets {
		stalls[t.consumer] = &kafkautil.CommitStall{}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var now time.Time
		select {
		case <-ctx.Done():
			return
		case now = <-ticker.C:
		}
		if !safego.Run(lagProbePoint, func() { probeLagOnce(&prober, brokers, targets, lastUncommitted, stalls, now) }) {
			closeLagProber(&prober, "close after panic")
		}
	}
}

// closeLagProber 关掉 *prober 并置 nil;*prober 为 nil 时什么都不做。
func closeLagProber(prober **kafkautil.GroupLagProber, why string) {
	if *prober == nil {
		return
	}
	if err := (*prober).Close(); err != nil {
		logx.Errorf("[kafka] lag probe %s: %v", why, err)
	}
	*prober = nil
}

// probeLagOnce 探一轮。*prober 为 nil 时先建连接,建好立即写回;任何一个目标探失败都关掉连接、*prober 置 nil,下一轮重建
// (sarama 的连接坏了没有可靠的就地自愈判据,重建最简单)。now 是这一轮的 ticker 时刻,只用来给停滞计时。
func probeLagOnce(prober **kafkautil.GroupLagProber, brokers []string, targets []lagTarget, lastUncommitted map[string]int, stalls map[string]*kafkautil.CommitStall, now time.Time) {
	if *prober == nil {
		p, err := kafkautil.NewGroupLagProber(brokers)
		if err != nil {
			for _, t := range targets {
				metrics.ObserveKafkaConsumerLagProbe(t.consumer, metrics.KafkaResultError)
			}
			logx.Errorf("[kafka] lag probe cannot connect to brokers %v; lag / retention-gap gauges keep their last value until it recovers: %v", brokers, err)
			return
		}
		*prober = p
	}

	healthy := true
	for _, t := range targets {
		lag, offsets, err := (*prober).ProbeOffsets(t.group, t.topic, t.partitions)
		if err != nil {
			healthy = false
			metrics.ObserveKafkaConsumerLagProbe(t.consumer, metrics.KafkaResultError)
			logx.Errorf("[%s] lag probe failed group=%s topic=%s: %v", t.consumer, t.group, t.topic, err)
			continue
		}
		metrics.SetKafkaConsumerLag(t.consumer, lag.Lag, lag.MaxPartitionLag, lag.RetentionGap)
		metrics.SetKafkaConsumerCommitStall(t.consumer, stalls[t.consumer].Observe(now, offsets))
		metrics.ObserveKafkaConsumerLagProbe(t.consumer, metrics.KafkaResultOK)
		if lag.RetentionGap > 0 {
			// 审计数据已经丢了:保留期(retention.ms 或每分区 retention.bytes)删掉了这个组还没消费到的消息。
			// 本行是采样值,只是下界:消费者越过缺口并提交之后本值自行归零。精确条数看 [kafka-go] 的日志(口径见 newReader):
			// 运行中被保留期追上是 `skipping from offset` 行;reader (重新)初始化时 kafka-go 静默跳到日志起点,
			// 看 `starting at offset` 与 `seeking to offset` 两行之差。
			logx.Errorf("[%s] AUDIT DATA LOST: %d message(s) of topic %s were deleted by retention before consumer group %s consumed them (lag=%d); see docs/ops/audit-topic-generation-bump-runbook.md",
				t.consumer, lag.RetentionGap, t.topic, t.group, lag.Lag)
		}
		if prev, seen := lastUncommitted[t.consumer]; lag.Uncommitted > 0 && (!seen || prev != lag.Uncommitted) {
			logx.Infof("[%s] lag probe: %d of %d partition(s) of topic %s have no committed offset for group %s yet (new group / new topic generation); counted from the log start",
				t.consumer, lag.Uncommitted, t.partitions, t.topic, t.group)
		}
		lastUncommitted[t.consumer] = lag.Uncommitted
	}
	if !healthy {
		closeLagProber(prober, "close after failure")
	}
}
