package metrics

import "github.com/prometheus/client_golang/prometheus"

// 全球终局恢复与终局发件箱的指标(2026-10-30,评审 B-4 / B-6)。label 只用 kind(result / first / waiting)与
// 投递结局的封闭集合(取值见 authority 包 OutboxKind* / OutboxPublished 等),battle_id / player_id 一律只进日志。
// 存量与积压 gauge 由每个副本每分钟的巡检各写一份(读的是同一个全局 TiDB),看板与告警按 max() 聚合;
// 投递计数每个副本各报一份,按 sum() 聚合。
var (
	globalExpiredStranded = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "global_expired_stranded_formations",
		Help: "全球成局里过了权威截止、却因候选形状不合格进不了到期终局恢复的存量(每分钟巡检;票据停在 state 2/3,按 cross-zone-matchmaking.md 13.3 处置)。",
	})
	outcomeOutboxBacklogRows = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "outcome_outbox_backlog_rows",
		Help: "终局发件箱待投行数(每分钟 COUNT):result = 已 COMPLETE 未发 Kafka;first = 个人结算未入箱;waiting = 已入箱等 APPLIED(离线玩家会长期停留)。",
	}, []string{"kind"})
	outcomeOutboxOldestAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "outcome_outbox_oldest_pending_age_seconds",
		Help: "终局发件箱每类最老一行自归档起的秒数(每分钟 MIN;没有积压为 0)。",
	}, []string{"kind"})
	outcomeOutboxDeliveryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "outcome_outbox_delivery_total",
		Help: "终局发件箱逐行处理结局:published / pending / applied / failed / corrupt / claim_lost / deferred / store_error。",
	}, []string{"kind", "outcome"})
	globalDeadlineAbortOverdue = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "global_deadline_abort_overdue_formations",
		Help: "全球成局里截止之后超过最晚落定时刻(约 16 分钟)仍没有终局的可裁决在途存量(每分钟巡检,2026-11-11)。大于 0 说明到期恢复跟不上积压或 match / 库长期不可用,scene 可能已按续冻上限解冻;按 cross-zone-matchmaking.md 13.3 处置(加副本或查库)。",
	})
	globalDeadlineAbortOverdueOldest = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "global_deadline_abort_overdue_oldest_seconds",
		Help: "上一项里最早一局已超期多少秒(每分钟巡检;没有超期为 0)。",
	})
)

func outcomeCollectors() []prometheus.Collector {
	return []prometheus.Collector{globalExpiredStranded, outcomeOutboxBacklogRows, outcomeOutboxOldestAge, outcomeOutboxDeliveryTotal,
		globalDeadlineAbortOverdue, globalDeadlineAbortOverdueOldest}
}

// SetGlobalDeadlineAbortOverdue 写「超期未作废」的存量与最早一局超期的秒数(0 也写:恢复跟上之后读数要能回落)。
func SetGlobalDeadlineAbortOverdue(count uint64, oldestOverdueSeconds float64) {
	globalDeadlineAbortOverdue.Set(float64(count))
	globalDeadlineAbortOverdueOldest.Set(oldestOverdueSeconds)
}

// SetGlobalExpiredStranded 写到期滞留成局的存量(0 也写:处置之后读数要能回落)。
func SetGlobalExpiredStranded(count uint64) { globalExpiredStranded.Set(float64(count)) }

// SetOutcomeOutboxBacklog 写终局发件箱一类的积压条数与最老待投年龄。
func SetOutcomeOutboxBacklog(kind string, rows uint64, oldestAgeSeconds float64) {
	outcomeOutboxBacklogRows.WithLabelValues(kind).Set(float64(rows))
	outcomeOutboxOldestAge.WithLabelValues(kind).Set(oldestAgeSeconds)
}

// ObserveOutcomeOutboxDelivery 记终局发件箱一行的处理结局;并发投递时会被多个 goroutine 同时调用(计数器并发安全)。
func ObserveOutcomeOutboxDelivery(kind, outcome string) {
	outcomeOutboxDeliveryTotal.WithLabelValues(kind, outcome).Inc()
}
