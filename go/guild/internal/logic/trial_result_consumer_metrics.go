package logic

// 历练结果消费组(topic Activity.TrialResult.Topic、组 Activity.TrialResult.GroupID,缺省 match-results / guild-trial)
// 在 broker 上的积压(2026-11-12,排空框架残余第 4 条;以前只能靠 GuildTrialResultOverdue 间接发现)。
//
// 由结果消费者的 lag 探针每 30s 写一次(kafkautil.RunGroupLagProbe:只读本组的已提交位点与日志起止,不碰消费与提交):
// 所有副本读同一个组、报同一个数,告警一律取 max;某副本探针失败时它的 gauge 保留上一次的值(陈值),
// 由按实例触发的 GuildTrialResultLagProbeBlind 点名。告警规则见 deploy/k8s/guild-trial-result-consumer-alerts.yaml。
// 没有业务 label:只有一个组、一个 topic。注册方式同 activity_metrics.go(go-zero core/metric)。

import (
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"

	"shared/kafkautil"
)

// guild_trial_result_consumer_lag_probe_total 的 result 取值。
const (
	trialLagProbeOK    = "ok"
	trialLagProbeError = "error"
)

var (
	guildTrialResultConsumerLag = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "result_consumer_lag_messages",
		Help:      "Trial result consumer group lag summed over all partitions, as seen by the broker (every replica reports the same group value; alert on max).",
	})

	guildTrialResultConsumerLagMaxPartition = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "result_consumer_lag_max_partition_messages",
		Help:      "Largest single-partition lag of the trial result consumer group, as seen by the broker (retention is enforced per partition).",
	})

	guildTrialResultConsumerRetentionGap = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "result_consumer_retention_gap_messages",
		Help:      "Battle results deleted by topic retention before the trial result consumer group committed past them (sampled; a lower bound of lost results).",
	})

	// 有积压的分区里,组内已提交位点最久多少秒没动(kafkautil.CommitStall,探针跨轮按分区算;位点一动或积压清零即从头计)。
	// GuildTrialResultConsumerLagging 按它判,判据与 match 结果消费组的 MatchResultConsumerLagging 相同:
	// 不看合计积压(高负载下几乎从不为 0 而位点一直在前进),也不要求新消息到达(安静分区上卡在一条上也报)。进程重启从 0 重计。
	guildTrialResultConsumerCommitStall = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "result_consumer_commit_stall_seconds",
		Help:      "Longest time any trial result partition with a backlog has gone without its committed offset advancing, as seen by this replica's lag probe (0 = every backlogged partition is advancing).",
	})

	guildTrialResultConsumerLagProbeTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "trial",
		Name:      "result_consumer_lag_probe_total",
		Help:      "Trial result consumer group lag probe outcomes by result (ok|error).",
		Labels:    []string{"result"},
	})
)

// TrialResultLagObserver 返回结果消费组 lag 探针的观察者,同时把探针结局的两条序列预置为 0:否则「探针从未失败」和
// 「指标不存在」在告警规则里长得一样。要在 go-zero 指标开关打开之后调用(MustNewServer 之后,同本文件其它指标)。
func TrialResultLagObserver() kafkautil.GroupLagObserver {
	guildTrialResultConsumerLagProbeTotal.Add(0, trialLagProbeOK)
	guildTrialResultConsumerLagProbeTotal.Add(0, trialLagProbeError)
	return observeTrialResultLag
}

// observeTrialResultLag 把一轮探测结果写进指标。失败时 gauge 保留上一次的值,只记失败计数与日志。
func observeTrialResultLag(target kafkautil.GroupLagTarget, lag kafkautil.GroupLag, err error) {
	if err != nil {
		guildTrialResultConsumerLagProbeTotal.Inc(trialLagProbeError)
		logx.Errorf("[GuildTrial] Trial result consumer lag probe failed group=%s topic=%s; lag gauges keep their last value: %v",
			target.Group, target.Topic, err)
		return
	}
	guildTrialResultConsumerLag.Set(float64(lag.Lag))
	guildTrialResultConsumerLagMaxPartition.Set(float64(lag.MaxPartitionLag))
	guildTrialResultConsumerRetentionGap.Set(float64(lag.RetentionGap))
	guildTrialResultConsumerCommitStall.Set(lag.CommitStall.Seconds())
	guildTrialResultConsumerLagProbeTotal.Inc(trialLagProbeOK)
	if lag.RetentionGap > 0 {
		// 结果已丢:保留期删掉了这个组还没提交到的对局结果(采样值,只是下界)。已登记的历练由巡检器从 match 的 SQL 终局兜底结算。
		logx.Errorf("[GuildTrial] TRIAL RESULTS LOST: %d message(s) of topic %s were deleted by retention before consumer group %s committed past them (lag=%d)",
			lag.RetentionGap, target.Topic, target.Group, lag.Lag)
	}
}
