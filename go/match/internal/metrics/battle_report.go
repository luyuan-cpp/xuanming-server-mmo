package metrics

import "github.com/prometheus/client_golang/prometheus"

// 战报回放的指标(docs/design/battle-report-replay.md §15)。label 只用封闭集合(丢弃原因 / 查询结局),
// battle_id / player_id 一律只进日志。
var (
	battleReportStoredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "battle_report_stored_total",
		Help: "终局事务里写入的战报正文行数(只算回放合格、存下来的局)。",
	})
	battleReportDroppedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "battle_report_dropped_total",
		Help: "终局请求带了回放却没存下:invalid_envelope / too_large / not_requested。终局照常提交。",
	}, []string{"reason"})
	battleReportEvictedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "battle_report_evicted_total",
		Help: "超出每人保留条数、被新局挤出的列表行数。",
	})
	battleReportReplayMismatchTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "battle_report_replay_mismatch_total",
		Help: "终局重放时请求里的回放与库里那份摘要不同。正常应恒为 0,非 0 告警;库里那份不改,回执照常。",
	})
	battleReportGetTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "battle_report_get_total",
		Help: "客户端战报查询:rpc = list / get;result = ok / not_found / no_session / error。",
	}, []string{"rpc", "result"})
)

func battleReportCollectors() []prometheus.Collector {
	return []prometheus.Collector{battleReportStoredTotal, battleReportDroppedTotal, battleReportEvictedTotal, battleReportReplayMismatchTotal, battleReportGetTotal}
}

// ObserveBattleReportStored 记一行写入的战报正文。
func ObserveBattleReportStored() { battleReportStoredTotal.Inc() }

// ObserveBattleReportDropped 记一次被丢弃的回放(终局照常提交)。
func ObserveBattleReportDropped(reason string) { battleReportDroppedTotal.WithLabelValues(reason).Inc() }

// ObserveBattleReportEvicted 记被挤出的列表行数。
func ObserveBattleReportEvicted(rows int) {
	if rows > 0 {
		battleReportEvictedTotal.Add(float64(rows))
	}
}

// ObserveBattleReportReplayMismatch 记一次终局重放时的回放摘要不一致。
func ObserveBattleReportReplayMismatch() { battleReportReplayMismatchTotal.Inc() }

// ObserveBattleReportGet 记一次客户端查询的结局。
func ObserveBattleReportGet(rpc, result string) { battleReportGetTotal.WithLabelValues(rpc, result).Inc() }
