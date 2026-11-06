package metrics

import "github.com/prometheus/client_golang/prometheus"

// 存活集合理性闸门的指标(internal/logic/node_view_gate.go,决策清单 B1-1)。注册在 metrics.go 的 register() 里。
// 告警见 deploy/k8s/scene-manager-alerts.yaml SceneManagerNodeViewHeld / SceneManagerNodeViewReleased。
// 不带 label:压住的是整轮 fullSync(一个 zone 压住,本副本全部 zone 的同步都停),哪个 zone、缺几个看同一时刻的日志
// "[LoadReporter] full sync failed: scene node view held by the liveness gate ..."。不带 label 也让两条序列在注册时就以 0 出现,
// increase() 抓得到第一次放行。
var (
	// nodeViewHolding:1 = 最近一次 fullSync 被闸门压住(快照为空,或某个 zone 缺席过半且缺席数还没稳定满 60s),0 = 放行。
	// 每个副本一份(跟随者也比自己的旧镜像)。压住期间 fullSync 每 3s 重试一次,每次都重写它。
	nodeViewHolding = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "node_view_holding",
		Help:      "1 while this replica's last full sync was held by the etcd liveness gate (empty snapshot, or more than half of a zone's known scene nodes missing and the missing count not yet stable for the hold time), 0 once a full sync is accepted.",
	})

	// nodeViewReleasedTotal:闸门放行骤降的次数(每个 zone 每次压住记一次)。放行之后缺席的节点按消失处理:
	// 领导者摘负载集、屏障之后拆它们名下的实例、改派世界频道。
	nodeViewReleasedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "node_view_released_total",
		Help:      "Times the etcd liveness gate released a held drop for a zone (the missing node count stayed unchanged for the hold time); the missing nodes are then treated as gone.",
	})
)

// SetNodeViewHolding 记最近一次 fullSync 有没有被闸门压住。
func SetNodeViewHolding(holding bool) {
	register()
	if holding {
		nodeViewHolding.Set(1)
		return
	}
	nodeViewHolding.Set(0)
}

// ObserveNodeViewReleased 记一次放行。
func ObserveNodeViewReleased() {
	register()
	nodeViewReleasedTotal.Inc()
}
