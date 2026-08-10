// Package safego 是 go-zero core/threading 的一层薄封装:
// 在它的 recover 之上补两样东西 —— **命名点位** 与 **可观测计数**。
//
// # 为什么需要它
//
// 全仓有 40+ 处裸 `go func(...)`。其中任何一处 panic,整个进程当场退出;
// 日志里只剩一段 runtime 栈,看不出是哪条后台链路炸的。
// go-zero 的 threading.GoSafe 能兜住 panic,但兜住之后你只知道
// "某个 goroutine 炸了",不知道是哪个,也没有指标可以配告警。
//
// 本包的两条原语给出这两样:
//
//	safego.Go("login.session_sweeper", fn)
//	safego.Loop(ctx, "scene.load_report", 5*time.Second, round)
//
// panic 时打一条稳定事件名 EventGoroutinePanic 的 Error 日志(带点位名 +
// 完整 goroutine 栈),并 Inc 指标 safego_panic_total{point="..."}。
// **压测期该指标恒 0** 可以直接当健康判据。
//
// # 不重复造轮子
//
// 派生 goroutine 一律走 threading.GoSafe,不自己 `go`。
// 本包自己的 recover 在**内层**:先由它记录点位与栈,再由 GoSafe 的
// rescue 作为外层兜底 —— 万一记录逻辑自身炸了,进程仍然不会死。
//
// # 边界(recover 兜不住的东西,本包一样兜不住)
//
// 下面这些是 Go runtime 的 **throw**,不是 panic,没有任何 recover 能拦:
//
//   - 并发读写 map(fatal error: concurrent map read and map write /
//     concurrent map writes)
//   - 栈溢出(stack overflow)
//   - 内存耗尽(out of memory)
//   - 全体 goroutine 休眠(all goroutines are asleep - deadlock)
//   - 从 cgo / 系统线程侧过来的致命信号
//
// 把共享 map 包进 safego.Go **不会**让它变安全,该加锁还得加锁、
// 该换 sync.Map / atomic.Pointer 快照还得换。本包不提供那种假象。
//
// 另外:本包只保证"panic 不打死进程",不保证业务正确性。
// 一轮 panic 意味着这一轮的副作用做了一半,是否要回滚/重试由调用方决定。
package safego

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/threading"
)

// EventGoroutinePanic 是 goroutine / 循环单轮 panic 的**稳定事件名**。
// 日志检索与告警规则认这个字符串,改它等于改对外契约。
const EventGoroutinePanic = "goroutine_panic"

// panicTotal 的 point label 必须是**代码里写死的常量点位名**,
// 绝不能拼进 player_id / scene_id 之类运行期值(仓库 CLAUDE.md §9)。
var panicTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "safego_panic_total",
	Help: "safego 兜住的 panic 次数,按点位。压测期应恒 0。",
}, []string{"point"})

var registerOnce sync.Once

// register 惰性注册,理由同 serverbase/metrics.go:
// 本包被所有服务 import,init 期 MustRegister 撞名会直接让进程起不来。
func register() {
	registerOnce.Do(func() {
		if err := prometheus.Register(panicTotal); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				logx.Errorf("[safego] 指标注册失败: %v", err)
			}
		}
	})
}

// Run 同步执行 fn 并兜住 panic,返回 fn 是否**正常跑完**。
//
// 它是本包的最小单元:Go 与 Loop 都建立在它之上。
// 自己管着循环/select 的调用方也可以直接用它把单次回调包起来。
func Run(point string, fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			report(point, r)
		}
	}()

	fn()
	return true
}

// Go 在新 goroutine 里跑 fn,panic 时记点位 + 完整栈 + 计数,不打死进程。
//
// point 是点位名,取**常量**(如 "login.session_sweeper"),
// 它会直接变成 Prometheus label。
func Go(point string, fn func()) {
	// 外层 threading.GoSafe 负责派生 goroutine 与最终兜底;
	// 内层 Run 负责把点位名与栈记下来。两层不重复,各司其职。
	threading.GoSafe(func() {
		Run(point, fn)
	})
}

// Loop 起一个后台循环:每隔 interval 跑一轮 round,直到 ctx 结束。
//
// **recover 的作用域精确到"一轮"**:某一轮 panic 只丢掉那一轮,
// 循环本身继续按节拍跑下一轮。这正是裸 `go func(){ for { ... } }()`
// 做不到的地方 —— 那种写法一轮炸掉,要么整个进程死,要么(套了 GoSafe)
// 循环从此静默停摆,而外面没人知道。
//
// 立即返回,不阻塞调用方。要等它退出请自行用 ctx + WaitGroup。
//
// interval <= 0 视为配置错误,记一条 Error 日志后直接不启动
// —— 用 0 间隔的 ticker 会 panic,而在这里 panic 掉一个后台循环
// 比什么都不做更糟。
func Loop(ctx context.Context, point string, interval time.Duration, round func(ctx context.Context)) {
	if interval <= 0 {
		logx.Errorf("[safego] 循环 %s 的 interval=%v 非法,不启动", point, interval)
		return
	}

	Go(point, func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 每一轮各自 recover:单轮炸掉不影响下一轮。
				Run(point, func() { round(ctx) })
			}
		}
	})
}

// report 记录一次被兜住的 panic。
//
// 它自己也可能炸(比如 logx 的 writer 出问题),那时由外层
// threading.GoSafe 的 rescue 兜底 —— 这就是"两层 recover"的意义。
func report(point string, recovered any) {
	register()
	panicTotal.WithLabelValues(point).Inc()

	logx.Errorw(EventGoroutinePanic,
		logx.Field("point", point),
		logx.Field("panic", recovered),
		logx.Field("stack", string(debug.Stack())),
	)
}
