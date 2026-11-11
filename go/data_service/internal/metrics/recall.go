package metrics

import (
	"sync"
	"time"

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
	// recallDispatchLastRun 是派发循环最近一次跑完一轮的时刻(Unix 秒;缺口清单第 25 条第四批,
	// docs/design/assetop-redelivery-sharding-20261110.md §14.3)。用不带可变 label 的 GaugeVec:循环起来才有序列、
	// 退出就删,没装配回收通道(Recall.Enabled=false)或已经停掉循环的副本都没有序列,告警 RecallDispatchLoopStalled 不用另滤 0。
	recallDispatchLastRun *prometheus.GaugeVec
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
		recallDispatchLastRun = promauto.NewGaugeVec(prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "recall_dispatch_last_run_unixtime",
			Help: "Unix time at which the GM recall dispatch loop last finished a round, failed rounds included " +
				"(a panicking round is not); also written when the loop starts. The series exists only while the loop runs.",
		}, nil)
	})
}

// ObserveRecallDispatch 记一次派发结果。
func ObserveRecallDispatch(result string) {
	registerRecallDispatch()
	recallDispatchTotal.WithLabelValues(result).Inc()
}

// MarkRecallDispatchRun 记派发循环起来、或跑完一轮的时刻(有告警 RecallDispatchLoopStalled:它长期不前进 = 循环卡在某一轮里)。
func MarkRecallDispatchRun(now time.Time) {
	registerRecallDispatch()
	recallDispatchLastRun.WithLabelValues().Set(float64(now.Unix()))
}

// ClearRecallDispatchRun 在派发循环退出时删掉活性序列:停掉的循环不该被当成卡住。
func ClearRecallDispatchRun() {
	registerRecallDispatch()
	recallDispatchLastRun.Reset()
}
