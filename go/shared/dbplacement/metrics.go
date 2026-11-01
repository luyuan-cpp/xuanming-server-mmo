package dbplacement

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

var (
	// versionGauge 是这个进程知道的映射版本。配了映射的进程上报 etcd 里每个有键的逻辑库,没有键的库连过才上报(版本恒为 0)。
	// dbmove audit 靠它核各进程都跟上了翻转。
	versionGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_placement_version",
		Help: "这个进程看到的逻辑库映射版本。",
	}, []string{"logical_db"})

	// knownClusterGauge 是这个进程配置里认识的物理集群,恒为 1。dbmove cutover 冻结之前核各进程都认识目标集群:
	// 不认识的进程翻转之后这个库读写全停,而且翻转之后回不去。
	knownClusterGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_placement_known_cluster",
		Help: "这个进程的 SqlClusters 里有这个物理集群时恒为 1。",
	}, []string{"cluster"})

	syncFailTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "db_placement_sync_fail_total",
		Help: "逻辑库映射读取或 Watch 失败的次数。失败时继续用最后读到的映射。",
	})

	rejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "db_placement_rejected_total",
		Help: "没有接受的映射变化次数:值解析不了、版本回退、搬过的库的键被删、etcd 的 revision 回退。工具不会写出这些,出现就要查。",
	}, []string{"reason"})

	registerOnce sync.Once
)

// register 惰性注册,不用映射的进程不多出这几个序列。
func register() {
	registerOnce.Do(func() {
		for _, c := range []prometheus.Collector{versionGauge, knownClusterGauge, syncFailTotal, rejectedTotal} {
			if err := prometheus.Register(c); err != nil {
				var already prometheus.AlreadyRegisteredError
				if !errors.As(err, &already) {
					logx.Errorf("[dbplacement] register metrics: %v", err)
				}
			}
		}
	})
}

func setVersion(logicalDB string, version uint64) {
	register()
	versionGauge.WithLabelValues(logicalDB).Set(float64(version))
}

func setKnownClusters(clusters map[string]SqlCluster) {
	register()
	for name := range clusters {
		knownClusterGauge.WithLabelValues(name).Set(1)
	}
}

func countSyncFail() {
	register()
	syncFailTotal.Inc()
}

func countRejected(reason string) {
	register()
	rejectedTotal.WithLabelValues(reason).Inc()
}
