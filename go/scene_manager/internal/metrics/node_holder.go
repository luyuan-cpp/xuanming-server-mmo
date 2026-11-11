package metrics

import "github.com/prometheus/client_golang/prometheus"

// 节点号持有者全量同步的指标(internal/logic/node_holder.go,docs/design/scene-node-ownership-reconcile.md §4.6)。
// 注册在 metrics.go 的 register() 里。告警见 deploy/k8s/scene-manager-alerts.yaml SceneManagerEtcdRevisionRegressed。
// 不带 label:一个副本只编排一个 zone(leader_gate.go isLeader),zone 看同一时刻的 ERROR 日志
// "[NodeHolder] etcd revision went back in zone ...";不带 label 也让序列在注册时就以 0 出现,increase() 抓得到第一次。
var (
	// etcdRevisionRegressionsTotal:领导者认出 etcd 修订号倒退、换了一次纪元的次数(etcd 换成新集群,或从备份恢复)。
	// 只在换纪元写成时加一;同一次倒退写失败重试时不重复计。
	etcdRevisionRegressionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "etcd_revision_regressions_total",
		Help:      "Times the zone leader found the current etcd revision below the highest revision it had recorded (etcd replaced by a new cluster or restored from a backup) and started a new etcd epoch, so node holder records written before give way to new registrations.",
	})
)

// ObserveEtcdRevisionRegression 记一次换纪元。
func ObserveEtcdRevisionRegression() {
	register()
	etcdRevisionRegressionsTotal.Inc()
}
