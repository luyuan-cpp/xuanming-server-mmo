package killswitch

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/grpcstats"
)

var (
	// blockedTotal 统计被关停短路掉的请求数。method 用短名,低基数。
	blockedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "killswitch",
		Name:      "blocked_total",
		Help:      "被热关停规则短路掉的 RPC 次数。",
	}, []string{"method"})

	// ruleCount 是当前生效的规则条数。
	// 正常应恒 0;非 0 说明有止血阀开着,别忘了关。
	ruleCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "killswitch",
		Name:      "rules",
		Help:      "当前生效的热关停规则条数(正常应为 0)。",
	})

	// etcdSyncFailTotal 统计与 etcd 的同步失败次数。
	// 它只是可观测信号,不影响放行判定 —— fail-open。
	etcdSyncFailTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "killswitch",
		Name:      "etcd_sync_fail_total",
		Help:      "killswitch 与 etcd 全量同步 / watch 失败次数(不影响放行)。",
	})

	registerOnce sync.Once
)

// register 惰性注册,理由同 serverbase/metrics.go。
func register() {
	registerOnce.Do(func() {
		collectors := []prometheus.Collector{blockedTotal, ruleCount, etcdSyncFailTotal}
		for _, c := range collectors {
			if err := prometheus.Register(c); err != nil {
				var already prometheus.AlreadyRegisteredError
				if !errors.As(err, &already) {
					logx.Errorf("[killswitch] 指标注册失败: %v", err)
				}
			}
		}
	})
}

func incBlocked(fullMethod string) {
	register()
	blockedTotal.WithLabelValues(grpcstats.ShortMethod(fullMethod)).Inc()
}

func setRuleCount(n int) {
	register()
	ruleCount.Set(float64(n))
}

func incSyncFail() {
	register()
	etcdSyncFailTotal.Inc()
}
