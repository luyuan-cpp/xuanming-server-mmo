package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// 对局结果消费组(topic ResultTopic、组 ResultConsumerGroup,缺省 match-results / match-rating)在 broker 上的积压(r2-kc#1)。
// 由 kafka 包的 lag 探针每 30s 写一次(kafkautil.RunGroupLagProbe):所有副本读同一个组,每个副本报同一个数,看板与告警一律取 max;
// 某个副本探针失败时它的 gauge 保留上一次的值(陈值),由按实例触发的 MatchResultLagProbeBlind 点名。
// 告警规则见 deploy/k8s/match-result-consumer-alerts.yaml。没有 label:只有一个组、一个 topic。
const (
	ResultLagProbeOK    = "ok"
	ResultLagProbeError = "error"
)

var (
	resultConsumerLagMessages = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "result_consumer_lag_messages",
		Help: "Battle result consumer group lag summed over all partitions, as seen by the broker (every replica reports the same group value; alert on max).",
	})
	resultConsumerLagMaxPartitionMessages = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "result_consumer_lag_max_partition_messages",
		Help: "Largest single-partition lag of the battle result consumer group, as seen by the broker (retention is enforced per partition).",
	})
	resultConsumerRetentionGapMessages = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "result_consumer_retention_gap_messages",
		Help: "Battle result messages deleted by topic retention before the consumer group committed past them (sampled; a lower bound of lost results).",
	})
	resultConsumerLagProbeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "result_consumer_lag_probe_total",
		Help: "Battle result consumer group lag probe outcomes by result (ok|error).",
	}, []string{"result"})
	// 有积压的分区里,组内已提交位点最久多少秒没动(kafkautil.CommitStall,探针跨轮按分区算;位点一动或积压清零即从头计)。
	// MatchResultConsumerLagging 按它判:高负载下合计积压几乎从不为 0 而位点一直在前进,「积压 > 0 持续 15 分钟」会误报;
	// 按分区算,一个分区卡在毒丸上、其余分区照常前进时照样报。进程重启从 0 重计。
	resultConsumerCommitStallSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "result_consumer_commit_stall_seconds",
		Help: "Longest time any battle result partition with a backlog has gone without its committed offset advancing, as seen by this replica's lag probe (0 = every backlogged partition is advancing).",
	})
)

func resultConsumerCollectors() []prometheus.Collector {
	return []prometheus.Collector{resultConsumerLagMessages, resultConsumerLagMaxPartitionMessages, resultConsumerRetentionGapMessages, resultConsumerLagProbeTotal, resultConsumerCommitStallSeconds}
}

// PrimeResultConsumerLagProbe 预置 ok / error 两条序列为 0:否则「探针从未失败」和「指标不存在」在告警规则里长得一样。
func PrimeResultConsumerLagProbe() {
	resultConsumerLagProbeTotal.WithLabelValues(ResultLagProbeOK).Add(0)
	resultConsumerLagProbeTotal.WithLabelValues(ResultLagProbeError).Add(0)
}

// SetResultConsumerLag 写一轮探针的结果:全部分区合计积压、单分区最大积压、保留期已删掉的未消费条数。
func SetResultConsumerLag(lag, maxPartitionLag, retentionGap int64) {
	resultConsumerLagMessages.Set(float64(lag))
	resultConsumerLagMaxPartitionMessages.Set(float64(maxPartitionLag))
	resultConsumerRetentionGapMessages.Set(float64(retentionGap))
}

// SetResultConsumerCommitStall 写一轮探针算出的停滞时长(kafkautil.GroupLag.CommitStall)。
func SetResultConsumerCommitStall(stall time.Duration) {
	resultConsumerCommitStallSeconds.Set(stall.Seconds())
}

// ResultConsumerCommitStallValue 读回停滞时长 gauge(单测断言用)。
func ResultConsumerCommitStallValue() float64 {
	var m dto.Metric
	if err := resultConsumerCommitStallSeconds.Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

// ObserveResultConsumerLagProbe 记一次探测的结局(ResultLagProbeOK / ResultLagProbeError)。
func ObserveResultConsumerLagProbe(result string) {
	resultConsumerLagProbeTotal.WithLabelValues(result).Inc()
}

// ResultConsumerLagProbeValue 读回某个结局的探测计数(单测断言用)。
func ResultConsumerLagProbeValue(result string) float64 {
	return counterVecValue(resultConsumerLagProbeTotal, result)
}

// ResultConsumerLagValue 读回合计积压 gauge(单测断言用)。
func ResultConsumerLagValue() float64 {
	var m dto.Metric
	if err := resultConsumerLagMessages.Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}
