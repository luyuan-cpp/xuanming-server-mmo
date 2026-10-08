package clientendpoint

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

// result label 的全部取值。只有这四个常量,基数恒为 4;
// 绝不能把 node_id / player_id / 地址拼进 label(AGENTS.md §9)。
const (
	resultClient   = "client"   // 用了节点自报的客户端地址
	resultFallback = "fallback" // 未要求且缺客户端地址,回落 endpoint(podip / 过渡期)
	resultRejected = "rejected" // require 下缺客户端地址,或回落时 endpoint 也不可用 —— 节点被跳过
	resultDeduped  = "deduped"  // 同一客户端地址上的陈旧记录被丢弃(每丢一条计一次)
)

var (
	// selectTotal 读法:
	//
	//   - external 模式稳态下 fallback 应恒 0,rejected 非 0 说明有 gate 没自报地址
	//     (看调用方的节流 ERROR 日志定位是哪台);
	//   - deduped 在 gate 崩溃后的 NodeTTL 窗口内短暂增长是预期,持续增长说明
	//     有两台活节点报了同一个客户端地址(模板或 nodePort 基数配错)。
	selectTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "mmorpg",
		Subsystem: "client_endpoint",
		Name:      "select_total",
		Help:      "下发给客户端的节点地址选择结果。result: client | fallback | rejected | deduped。",
	}, []string{"result"})

	// 预先取好子计数器:热路径上每节点一次,省掉 WithLabelValues 的加锁与哈希。
	// 子计数器挂在 selectTotal 上,注册前取也会随 selectTotal 一起暴露。
	clientCounter   = selectTotal.WithLabelValues(resultClient)
	fallbackCounter = selectTotal.WithLabelValues(resultFallback)
	rejectedCounter = selectTotal.WithLabelValues(resultRejected)
	dedupedCounter  = selectTotal.WithLabelValues(resultDeduped)

	registerOnce sync.Once
)

// register 惰性注册,理由同 serverbase/metrics.go:shared 被多个服务 import,
// init 期 MustRegister 撞名会让进程起不来;这里用 Register + AlreadyRegisteredError 容错,绝不 panic。
func register() {
	registerOnce.Do(func() {
		if err := prometheus.Register(selectTotal); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				logx.Errorf("[clientendpoint] 指标注册失败: %v", err)
			}
		}
	})
}

// countSelect 记一次 Select 结果。c 只能是本文件预取的 client / fallback / rejected 子计数器。
func countSelect(c prometheus.Counter) {
	register()
	c.Inc()
}

// countDeduped 记 n 条被去重丢弃的记录。
func countDeduped(n int) {
	if n <= 0 {
		return
	}
	register()
	dedupedCounter.Add(float64(n))
}
