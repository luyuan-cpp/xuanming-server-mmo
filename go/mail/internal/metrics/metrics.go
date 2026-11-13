// Package metrics 暴露 mail 服务的 Prometheus 指标(设计 docs/design/mail-system.md §2.5、§4.3、§4.5、§4.6、§4.7)。
// /metrics 端口是 :9240(etc/mail.yaml 的 MetricsListenAddr)。
//
// 两条硬约束(AGENTS.md §9):
//  1. label 全是**有限枚举**,取值集合就是本文件的常量。player_id / mail_id / request_id 一律只进日志。
//  2. 指标**懒注册**:Vec 在包级变量就建好,MustRegister 只在 Register 的 sync.Once 里做(Start 会调它)。没调的
//     `-migrate` 进程照样可以安全 Inc,不会重复注册 panic;单测要读数时先调 Register,再从 prometheus.DefaultGatherer 读。
//
// 所有 label 组合都在 register 里**预建 0 值序列**:Prometheus 里"从未发生过"的序列根本不存在,
// `increase(...[1h]) > 0` 一类告警在序列缺失时既不报警也不报错,与"一切正常"长得一样(设计 §4.7)。
// 唯一的例外是活性 gauge sweep_last_run_unixtime(SweepLive):它不是事件计数,只在清理循环运行时有序列。
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"shared/looplive"
)

// subsystem 决定指标名前缀:mail_admin_send_total / mail_push_total / …改它等于改全部指标名。
const subsystem = "mail"

// MailAdmin 发件的 method label。
const (
	MethodSendPersonal = "send_personal"
	MethodSendSystem   = "send_system"
	MethodSendGuild    = "send_guild"
)

// MailAdmin 发件的 sender_kind label(与 mail_admin.proto 的 MailSenderKind 对应;未知值一律记 unknown,不拿数字当 label)。
const (
	SenderOpsTool = "ops_tool"
	SenderRobot   = "robot"
	SenderUnknown = "unknown"
	SenderRank    = "rank"
	SenderGuild   = "guild"
	SenderTrade   = "trade"
	SenderBattle  = "battle"
)

// MailAdmin 发件的 outcome label(设计 §2.5):
//   - ok:首次发出;duplicated:同键同摘要的重放,回首次结果;rejected:业务拒绝(满箱、冲突、附件不合法……);
//   - fault:存储 / 号段故障(kServiceUnavailable);denied:令牌缺失或错误(gRPC PermissionDenied)。
const (
	OutcomeOK         = "ok"
	OutcomeDuplicated = "duplicated"
	OutcomeRejected   = "rejected"
	OutcomeFault      = "fault"
	OutcomeDenied     = "denied"
)

// 推送的 outcome label(设计 §4.5)。offline 与 session_error、error 必须分开:读会话失败若也记成 offline,
// 一次协议事故会表现为"推送全都不推"而没有任何异常指标。
const (
	PushOK           = "ok"            // 已写入 Kafka
	PushOffline      = "offline"       // 收件人无会话或非 ONLINE:最常见的正常分支
	PushSessionError = "session_error" // 读 / 解码 player:session 失败
	PushError        = "error"         // 推送通道未配置、Kafka 写失败、组包失败
)

// sweep沿用告警label：personal/system分别对应M2 mail_message中的个人/纯文本广播，system_state对应mail_state；不是M1物理表名。
const (
	TablePersonal    = "mail_personal"
	TableSystem      = "mail_system"
	TableSystemState = "mail_system_state"
)

// sweep 的 mode label。与 config.SweepMode* 同值(本包保持叶子包,不 import config;三份字面量由测试钉一致)。
const (
	SweepModeReportOnly = "report_only"
	SweepModeDelete     = "delete"
)

var (
	adminSendTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "admin_send_total",
		Help:      "MailAdmin send calls by method (send_personal|send_system|send_guild), sender_kind (ops_tool|robot|unknown|rank|guild|trade|battle) and outcome (ok|duplicated|rejected|fault|denied).",
	}, []string{"method", "sender_kind", "outcome"})

	pushTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "push_total",
		Help:      "NotifyMailEvent pushes for new personal mail by outcome (ok|offline|session_error|error).",
	}, []string{"outcome"})

	guildLookupFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "guild_lookup_failures_total",
		Help:      "Reads of a player's guild from the guild service that failed; guild text mails stay hidden for that request.",
	})

	sweepCandidateRows = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "sweep_candidate_rows",
		Help:      "Rows the last sweep pass counted as eligible for physical deletion (bounded count), by table and sweep mode.",
	}, []string{"table", "mode"})

	sweepDeletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "sweep_deleted_total",
		Help:      "Rows physically deleted by sweep, by table.",
	}, []string{"table"})

	// 设计 §4.6 写的名字是 mail_sweep_invalid_rows;它是 counter,按 Prometheus 命名约定带 _total 后缀(§12.2 登记)。
	sweepInvalidRows = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "sweep_invalid_rows_total",
		Help:      "Rows with expire_ms = 0 seen by sweep (counted, never deleted; send-side validation was bypassed), by table.",
	}, []string{"table"})

	// SweepLive 是清理循环的活性,指标 mail_sweep_last_run_unixtime(缺口清单第 25 条第四批,
	// docs/design/assetop-redelivery-sharding-20261110.md §14.3;口径在 shared/looplive 的包注释)。canary(不跑清理)
	// 和已经有意停掉清理的副本没有序列,告警 MailSweepStalled 按副本逐条判,不必另滤 0。只由 maintenance.Run 写。
	SweepLive = looplive.New(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "sweep_last_run_unixtime",
		Help: "Unix time at the end of the last finished sweep pass (failed passes and passes with 0 rows included; a panicking pass is not), " +
			"also written when the loop starts; the series exists only while the sweep loop runs and stops advancing when the loop is stalled.",
	})

	registerOnce sync.Once
)

// ObserveAdminSend 记一次 MailAdmin 发件的终态。三个参数都取本包常量,别传数字或拼出来的串。
func ObserveAdminSend(method, senderKind, outcome string) {
	adminSendTotal.WithLabelValues(method, senderKind, outcome).Inc()
}

// ObservePush 记一次推送的终态,outcome 取 Push* 之一。
func ObservePush(outcome string) {
	pushTotal.WithLabelValues(outcome).Inc()
}

// ObserveGuildLookupFailure 记一次读所在帮会失败:这一次按没有帮算,帮会文本邮件看不到。
func ObserveGuildLookupFailure() {
	guildLookupFailuresTotal.Inc()
}

// SetSweepCandidateRows 刷新某张表在某个 sweep 模式下本轮的候选行数。每轮都要写,**包括 0**。
func SetSweepCandidateRows(table, mode string, rows float64) {
	sweepCandidateRows.WithLabelValues(table, mode).Set(rows)
}

// AddSweepDeleted 累加某张表本轮实删的行数(0 也可以调,无副作用)。
func AddSweepDeleted(table string, rows float64) {
	sweepDeletedTotal.WithLabelValues(table).Add(rows)
}

// AddSweepInvalidRows 累加 expire_ms = 0 的保险分支命中数(有告警 MailSweepInvalidRows)。
func AddSweepInvalidRows(table string, rows float64) {
	sweepInvalidRows.WithLabelValues(table).Add(rows)
}

// Register 注册全部指标并预建 0 值序列。幂等:重复调用不会 panic。
func Register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			adminSendTotal,
			pushTotal,
			guildLookupFailuresTotal,
			sweepCandidateRows,
			sweepDeletedTotal,
			sweepInvalidRows,
			SweepLive.Collector(),
		)
		for _, method := range []string{MethodSendPersonal, MethodSendSystem, MethodSendGuild} {
			for _, sender := range []string{SenderOpsTool, SenderRobot, SenderUnknown, SenderRank, SenderGuild, SenderTrade, SenderBattle} {
				for _, outcome := range []string{OutcomeOK, OutcomeDuplicated, OutcomeRejected, OutcomeFault, OutcomeDenied} {
					adminSendTotal.WithLabelValues(method, sender, outcome)
				}
			}
		}
		for _, outcome := range []string{PushOK, PushOffline, PushSessionError, PushError} {
			pushTotal.WithLabelValues(outcome)
		}
		for _, table := range []string{TablePersonal, TableSystem, TableSystemState} {
			for _, mode := range []string{SweepModeReportOnly, SweepModeDelete} {
				sweepCandidateRows.WithLabelValues(table, mode)
			}
			sweepDeletedTotal.WithLabelValues(table)
			sweepInvalidRows.WithLabelValues(table)
		}
	})
}

// HTTP端点与有界关闭只由shared/domainhost提供，不再启动第二套metrics服务器。
