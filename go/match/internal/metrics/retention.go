package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// 保留期清理(到期删除)的失败与逾期指标。match 有两个清理循环:观战意图(logic/global_spectator.go)与剧本审核流水
// (logic/battle_script_review_purge.go,docs/design/battle-script.md §6.4);删除行数各记各的(spectator_intent_purged_total、
// battle_script_review_purged_total)。告警在 deploy/k8s/match-alerts.yaml 的 match.retention 组。

// retention_* 指标的 purge 取值:代码里的常量,不放实体 id。
const (
	RetentionPurgeSpectatorIntent    = "spectator_intent"
	RetentionPurgeBattleScriptReview = "battle_script_review"
)

var (
	// retentionPurgeFailuresTotal 清理循环失败的轮数。失败的一轮不动游标,下一轮重试;持续上涨 = 到期的行一直没删。
	// 搬库冻结窗口里被写者闸门拒绝的轮次不计(logic.observeRetentionRound):那一轮确定没写,不是清理坏了。
	retentionPurgeFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "retention_purge_failures_total",
		Help: "Failed rounds of a retention purge loop by purge (spectator_intent|battle_script_review). A failed round is retried one interval later. Rounds rejected only because the writer gate is frozen (database move) are not counted.",
	}, []string{"purge"})
	// retentionOldestUnixtime 表里现存最老一行的时刻。记时刻、不记年龄:年龄由告警规则用 time() 减出来,这个副本读不动库时
	// 它照样随时间变老,不会停在最后一次读到的值上。只有剧本审核流水报:观战意图的请求头行有意一直留着,最老一行没有意义。
	retentionOldestUnixtime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "retention_oldest_unixtime",
		Help: "Time (Unix seconds) of the oldest row still in the table as last read by this replica, by purge; the read time when the table is empty.",
	}, []string{"purge"})
	// retentionMaxAgeSeconds 本副本生效的保留期,逾期告警拿它作门槛。
	retentionMaxAgeSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "retention_max_age_seconds",
		Help: "Retention period in effect on this replica, in seconds, by purge.",
	}, []string{"purge"})
)

func retentionCollectors() []prometheus.Collector {
	return []prometheus.Collector{retentionPurgeFailuresTotal, retentionOldestUnixtime, retentionMaxAgeSeconds}
}

// ObserveRetentionPurgeFailure 记一轮保留期清理失败(purge 取 RetentionPurge* 之一)。
func ObserveRetentionPurgeFailure(purge string) {
	retentionPurgeFailuresTotal.WithLabelValues(purge).Inc()
}

// RetentionPurgeFailureValue 读回某个清理的失败轮数,只给单测断言用。
func RetentionPurgeFailureValue(purge string) float64 {
	return counterVecValue(retentionPurgeFailuresTotal, purge)
}

// SetRetentionMaxAge 记某个清理生效的保留期,清理循环起来时调一次。
func SetRetentionMaxAge(purge string, maxAge time.Duration) {
	retentionMaxAgeSeconds.WithLabelValues(purge).Set(maxAge.Seconds())
}

// SetRetentionOldest 记表里现存最老一行的时刻(Unix 毫秒,库时间)。
func SetRetentionOldest(purge string, oldestMs uint64) {
	retentionOldestUnixtime.WithLabelValues(purge).Set(float64(oldestMs) / 1000)
}

// RetentionOldestValue 读回某个清理最近报的最老一行时刻(Unix 秒),只给单测断言用;读失败返回 0。
func RetentionOldestValue(purge string) float64 {
	var m dto.Metric
	if err := retentionOldestUnixtime.WithLabelValues(purge).Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}
