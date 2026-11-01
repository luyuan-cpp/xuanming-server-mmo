package metrics

import (
	"sync"

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
	})
}

// ObserveRecallDispatch 记一次派发结果。
func ObserveRecallDispatch(result string) {
	registerRecallDispatch()
	recallDispatchTotal.WithLabelValues(result).Inc()
}
