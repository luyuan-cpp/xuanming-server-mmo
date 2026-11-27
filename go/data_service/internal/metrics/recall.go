package metrics

import (
	"sync"

	"shared/looplive"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// GM 批量回收派发的结果。投递和终结的指标在 assetop_* 里,stream 是 gm_recall。
const (
	RecallDispatchSent     = "sent"
	RecallDispatchDeferred = "deferred"
	RecallDispatchRoleGone = "role_gone"
	RecallDispatchLost     = "lost"
	RecallDispatchError    = "error"
)

var (
	recallDispatchOnce  sync.Once
	recallDispatchTotal *prometheus.CounterVec
	// recallDispatchListErrors 是派发读到期 PLANNED 行失败的轮数(这一轮什么都没派)。recall_dispatch_total 是逐行结果,
	// 读队头失败不是某一行的结果,单独计;与重投的 assetop_store_errors_total{op="list"} 对应。
	recallDispatchListErrors prometheus.Counter
	// recallDispatchLive 是派发循环的活性,指标 data_service_recall_dispatch_last_run_unixtime(缺口清单第 25 条第四批,
	// docs/design/assetop-redelivery-sharding-20261110.md §14.3;口径在 shared/looplive 的包注释)。没装配回收通道
	// (Recall.Enabled=false)或已经有意停掉循环的副本没有序列,告警 RecallDispatchLoopStalled 不用另滤 0。
	recallDispatchLive *looplive.Gauge
)

func registerRecallDispatch() {
	recallDispatchOnce.Do(func() {
		recallDispatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "recall_dispatch_total",
			Help:      "GM batch recall planned rows dispatched, by result.",
		}, []string{"result"})
		for _, result := range []string{RecallDispatchSent, RecallDispatchDeferred, RecallDispatchRoleGone, RecallDispatchLost, RecallDispatchError} {
			recallDispatchTotal.WithLabelValues(result)
		}
		recallDispatchListErrors = promauto.NewCounter(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "recall_dispatch_list_errors_total",
			Help:      "GM batch recall dispatch rounds whose read of due PLANNED rows failed (that round dispatched nothing).",
		})
		recallDispatchLive = looplive.NewRegistered(prometheus.DefaultRegisterer, prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "recall_dispatch_last_run_unixtime",
			Help: "Unix time at which the GM recall dispatch loop last finished a round, failed rounds included " +
				"(a panicking round is not); also written when the loop starts. The series exists only while the loop runs.",
		})
	})
}

// ObserveRecallDispatch 记一次派发结果。
func ObserveRecallDispatch(result string) {
	registerRecallDispatch()
	recallDispatchTotal.WithLabelValues(result).Inc()
}

// ObserveRecallDispatchListError 记一轮派发读到期 PLANNED 行失败。
func ObserveRecallDispatchListError() {
	registerRecallDispatch()
	recallDispatchListErrors.Inc()
}

// RecallDispatchLive 返回派发循环的活性 gauge(有告警 RecallDispatchLoopStalled:它长期不前进 = 这个副本的派发循环停住了)。
func RecallDispatchLive() *looplive.Gauge {
	registerRecallDispatch()
	return recallDispatchLive
}

// AdminMethod* 是 admin_unapproved_command_total 的 method 取值:要求双人复核批准记录的四个回收入口
// (docs/design/admin-identity-and-dual-control.md §3 甲级)。也是这三个入口日志与拒绝文本里的方法名。
const (
	AdminMethodBatchRecallItems  = "BatchRecallItems"
	AdminMethodCancelRecallBatch = "CancelRecallBatch"
	AdminMethodResolveRecallOp   = "ResolveRecallOp"
	// RegisterRecallReviewHandled 把流水永久登记成已人工处置,以后的回收都跳过它们,做完撤不回,同样是甲级。
	AdminMethodRegisterRecallReviewHandled = "RegisterRecallReviewHandled"
)

var (
	adminUnapprovedOnce sync.Once
	// adminUnapprovedCommandTotal 是不带批准记录而被受理的回收命令,只在 AdminAllowUnapproved 打开时增长,集群里应当恒为 0
	// (告警 DataServiceAdminCommandUnapproved,deploy/k8s/dual-control-alerts.yaml)。
	adminUnapprovedCommandTotal *prometheus.CounterVec
)

func registerAdminUnapproved() {
	adminUnapprovedOnce.Do(func() {
		adminUnapprovedCommandTotal = promauto.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "admin_unapproved_command_total",
			Help: "Admin recall commands (BatchRecallItems execute, CancelRecallBatch, ResolveRecallOp, RegisterRecallReviewHandled) accepted without a " +
				"dual-control approval record (only while AdminAllowUnapproved is on), by method.",
		}, []string{"method"})
		for _, method := range []string{AdminMethodBatchRecallItems, AdminMethodCancelRecallBatch, AdminMethodResolveRecallOp,
			AdminMethodRegisterRecallReviewHandled} {
			adminUnapprovedCommandTotal.WithLabelValues(method)
		}
	})
}

// PrimeAdminUnapprovedCommand 预建四个 method 的零值序列:第一次受理就从 1 起的序列,increase() 看不到这一次。启动时调,可重复调用。
func PrimeAdminUnapprovedCommand() { registerAdminUnapproved() }

// ObserveAdminUnapprovedCommand 记一次不带双人复核批准记录而被受理的回收命令。
func ObserveAdminUnapprovedCommand(method string) {
	registerAdminUnapproved()
	adminUnapprovedCommandTotal.WithLabelValues(method).Inc()
}
