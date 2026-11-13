package metrics

import (
	"shared/looplive"

	"github.com/prometheus/client_golang/prometheus"
)

// LoopLive 是 match 后台循环的活性,指标 match_loop_last_run_unixtime{point}(缺口清单第 25 条第六批,
// docs/design/assetop-redelivery-sharding-20261110.md §14.10;口径在 shared/looplive 的包注释)。
//
// 经 svc.ServiceContext 的 StartSourceLoop / StartRelayLoop 起的循环(撮合、成局推进、到期终局恢复、整队维护、观战维护等)
// 与投榜循环(authority.RunRankPublisher)各占一条,point 就是循环的 safego 点位名:代码里的常量,十几个,不放实体 id。
// 撮合与投榜按片分给各副本:一个副本的循环停住(卡在某一轮里、每一轮都 panic、goroutine 意外退出),它名下的撮合段、
// 投榜片就没人做,别的副本照常推进,合计指标看不出来。告警 MatchBackgroundLoopStalled(deploy/k8s/match-alerts.yaml)
// 按副本、按点位判。终局发件箱另有 OutcomeOutboxLive(outcome.go),不在这一族里。
var LoopLive = looplive.NewFamily(prometheus.GaugeOpts{
	Subsystem: subsystem, Name: "loop_last_run_unixtime",
	Help: "match 后台循环最近一次跑完一轮的时刻(Unix 秒,这一轮成败都算,panic 的那一轮不算;循环起来时也写一次)," +
		"point 是循环的点位名。只在循环运行时有序列;值长期不前进 = 循环停住了。",
}, "point")

func loopCollectors() []prometheus.Collector {
	return []prometheus.Collector{LoopLive.Collector()}
}
