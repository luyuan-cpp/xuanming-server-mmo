package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"shared/looplive"
)

// 审计发件箱的指标(docs/design/player-audit-outbox.md §9)。label 都是封闭集合,不含 player_id、存档库编号(AGENTS.md §9)。

// kind 标签的取值。
const (
	AuditKindTransactionLog = "transaction_log"
	AuditKindPlayerSnapshot = "player_snapshot"
)

// relay_errors_total 的 stage 取值。
const (
	AuditRelayStageRead    = "read"
	AuditRelayStageProduce = "produce"
	AuditRelayStageDelete  = "delete"
)

var (
	auditOutboxWritten = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_records_written_total",
		Help:      "Audit records written to player_audit_outbox together with a committed save. kind: transaction_log | player_snapshot.",
	}, []string{"kind"})

	auditOutboxSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_records_skipped_total",
		Help:      "Audit records skipped while the save itself committed. reason: invalid | unknown_row_kind | too_large | undecodable.",
	}, []string{"reason"})

	auditOutboxForwarded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_forwarded_total",
		Help:      "Audit outbox rows acknowledged by Kafka (acks=all) and deleted. kind: transaction_log | player_snapshot.",
	}, []string{"kind"})

	auditOutboxRelayErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_relay_errors_total",
		Help:      "Audit outbox relay failures. stage: read | produce | delete. Rows stay in the outbox and are retried.",
	}, []string{"stage"})

	// 本副本负责的 (存档库, 片) 里,最近一轮读到的最老未投记录的年龄;没有积压为 0。各副本只报自己负责的那部分,告警取 max。
	auditOutboxOldestAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_oldest_pending_age_seconds",
		Help:      "Age of the oldest audit outbox row seen by this replica's latest relay pass, over the shards it owns; 0 when nothing is pending.",
	}, []string{"kind"})

	auditOutboxRelayEnabled = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_relay_enabled",
		Help:      "1 when this replica runs the audit outbox relay; 0 when it is not configured (no Kafka brokers or no player store list) or this is a canary.",
	})

	// 分工覆盖(口径同 assetop_owned_shards):各副本负责的单位数加起来应等于单位总数。一个 stable 副本登记在成员表里却没在转发
	// (没配 Kafka、版本太旧),它名下的单位没人投,这些单位的积压年龄也没人上报 —— 上面的最老年龄看不出来,只有这两个数对不上。
	auditOutboxOwnedUnits = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_relay_owned_units",
		Help:      "Number of (player store, shard) units this replica owned in its latest relay pass; the stable replicas of a region should add up to db_audit_outbox_relay_units.",
	})
	auditOutboxUnits = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_relay_units",
		Help:      "Total number of (player store, shard) units known to this replica's relay: player stores of the region x shards per store; 0 while the relay is not running.",
	})

	// auditOutboxRelayLive 是转发循环的活性,指标 db_audit_outbox_relay_last_run_unixtime(口径在 shared/looplive 的包注释)。
	// 循环停住的副本还登记在成员表里、占着自己那些单位,别的副本不会接手;告警按副本判 time() - 它 > 阈值。
	// 没起转发的副本(canary、没配 Kafka)与已经有意停掉循环的副本没有序列。
	auditOutboxRelayLive = looplive.New(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "audit_outbox_relay_last_run_unixtime",
		Help:      "Unix time at which the audit outbox relay loop last finished a pass (success or failure both count, a pass that panicked does not; also written once when the loop starts). The series exists only while the loop runs; a value that stops advancing means the loop is stuck.",
	})

	auditRegisterOnce sync.Once
)

func registerAuditOutbox() {
	auditRegisterOnce.Do(func() {
		prometheus.MustRegister(auditOutboxWritten, auditOutboxSkipped, auditOutboxForwarded, auditOutboxRelayErrors,
			auditOutboxOldestAge, auditOutboxRelayEnabled, auditOutboxOwnedUnits, auditOutboxUnits, auditOutboxRelayLive.Collector())
		for _, kind := range []string{AuditKindTransactionLog, AuditKindPlayerSnapshot} {
			auditOutboxWritten.WithLabelValues(kind).Add(0)
			auditOutboxForwarded.WithLabelValues(kind).Add(0)
			auditOutboxOldestAge.WithLabelValues(kind).Set(0)
		}
		for _, stage := range []string{AuditRelayStageRead, AuditRelayStageProduce, AuditRelayStageDelete} {
			auditOutboxRelayErrors.WithLabelValues(stage).Add(0)
		}
	})
}

// PrimeAuditOutbox 把跳过原因以 0 预置,告警的 increase 才抓得到第一次。原因由 playerstore 定义(AuditSkipReasons)。
func PrimeAuditOutbox(skipReasons []string) {
	registerAuditOutbox()
	for _, reason := range skipReasons {
		auditOutboxSkipped.WithLabelValues(reason).Add(0)
	}
}

// ObserveAuditOutboxWritten 记随一笔已提交的存盘写进发件箱的条数。
func ObserveAuditOutboxWritten(kind string, n int) {
	registerAuditOutbox()
	if n > 0 {
		auditOutboxWritten.WithLabelValues(kind).Add(float64(n))
	}
}

// ObserveAuditOutboxSkipped 记跳过的条数。
func ObserveAuditOutboxSkipped(reason string, n int) {
	registerAuditOutbox()
	if n > 0 {
		auditOutboxSkipped.WithLabelValues(reason).Add(float64(n))
	}
}

// ObserveAuditOutboxForwarded 记投出并删除的条数。
func ObserveAuditOutboxForwarded(kind string, n int) {
	registerAuditOutbox()
	if n > 0 {
		auditOutboxForwarded.WithLabelValues(kind).Add(float64(n))
	}
}

// CountAuditOutboxRelayError 记一次转发失败。
func CountAuditOutboxRelayError(stage string) {
	registerAuditOutbox()
	auditOutboxRelayErrors.WithLabelValues(stage).Inc()
}

// SetAuditOutboxOldestPendingAge 设本副本最老未投记录的年龄(秒)。
func SetAuditOutboxOldestPendingAge(kind string, seconds float64) {
	registerAuditOutbox()
	auditOutboxOldestAge.WithLabelValues(kind).Set(seconds)
}

// SetAuditOutboxRelayEnabled 报本副本的转发循环是否在跑。
func SetAuditOutboxRelayEnabled(enabled bool) {
	registerAuditOutbox()
	value := 0.0
	if enabled {
		value = 1
	}
	auditOutboxRelayEnabled.Set(value)
}

// SetAuditOutboxRelayUnits 报本副本最近一轮负责的单位数与它知道的单位总数(存档库数 × 每库片数)。每轮写一次。
func SetAuditOutboxRelayUnits(owned, total int) {
	registerAuditOutbox()
	auditOutboxOwnedUnits.Set(float64(owned))
	auditOutboxUnits.Set(float64(total))
}

// AuditOutboxRelayLive 返回转发循环的活性 gauge。只由 internal/auditoutbox 的 Relay.Run 写。
func AuditOutboxRelayLive() *looplive.Gauge {
	registerAuditOutbox()
	return auditOutboxRelayLive
}
