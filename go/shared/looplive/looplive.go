// Package looplive 给周期后台循环报活性:最近一次跑完一轮的时刻。
//
// # 为什么需要它
//
// 按片分工的后台循环(go/shared/workshard)里,一个副本的循环停住之后(卡在某一轮里、每一轮都 panic、goroutine 异常退出),
// 它还登记在成员表里、占着自己那几片,这几片从此没人做;而各副本合计的指标(负责片数之和、删除总数、最老年龄)
// 看不出是哪个副本。每个循环报一个「最近一次跑完一轮的时刻」,告警按副本判 time() - 这个时刻 > 阈值,
// 直接点出停住的那个副本。
//
// # 口径(用它的循环都一样;设计见 docs/design/assetop-redelivery-sharding-20261110.md §14.3)
//
//		live.Mark(time.Now()) // 循环起来时写一次:首轮之前可能有随机延迟,不能等第一轮
//		for {
//			select {
//			case <-ctx.Done():
//				live.Stop() // 有意停止才删序列
//				return
//			case <-ticker.C:
//			}
//			live.Round(point, round) // 这一轮正常返回(成败都算)才写时刻;panic 的那一轮不写,循环接着跑
//		}
//
//	  - 一轮出错(读库失败、单行失败)照样算跑完:出错有各自的计数与日志,一次库故障不该同时报成每个副本的循环卡死。
//	  - 一轮 panic 不算跑完:Round 里的 safego 兜住它(记点位、栈与 safego_panic_total),循环不死;每一轮都 panic 时
//	    时刻不再前进,由停摆告警报出来。一个坏行不该把整个进程打死 —— 按片分工时它的片会换到别的副本,把别的副本也挨个打死。
//	  - Stop 只在循环有意停止的 return 之前调,不要 defer:defer 在 panic 展开时也会执行。循环因为意料之外的原因退出时,
//	    旧值要留在 /metrics 上,停摆告警才报得出来。
//
// 序列只在循环运行期间存在:没起这个循环的副本(canary、功能开关关着、离线工具)与已经有意停掉循环的副本都没有序列,
// 所以告警规则不用滤掉 0,也不会对正在排空的副本误报。
//
// 一个循环一个指标名时用 New / NewRegistered,gauge 不带可变 label。一个服务用同一个包装函数起很多循环时(match 的
// StartSourceLoop / StartRelayLoop 起了十来个),用 NewFamily:一个指标名、一个固定的 label 区分循环,label 值只取代码里
// 写死的点位名,For(点位) 给每个循环一个 Gauge,口径同上,Stop 只删自己那一条。label 一律不放 player_id 之类的实体 id(AGENTS §9)。
package looplive

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"shared/safego"
)

// Gauge 是一个循环的活性 gauge(Unix 秒)。用 New / NewRegistered 构造,或用 Family.For 从一族里取,零值不可用。
//
// nil 接收者安全:Mark、Stop 是空操作,Round 照常在 safego 里跑这一轮,Value 回「没有序列」。给不接指标的调用方
// (单测里 metrics 为 nil 的循环)。方法都可并发调用。
type Gauge struct {
	// GaugeVec 而不是 Gauge:Gauge 一注册就报 0;GaugeVec 在第一次 Set 之前、删掉之后都没有序列。
	vec *prometheus.GaugeVec
	// label、value 是这个循环在一族里的那一条(Family.For);单独构造的 gauge 两个都为空,vec 不带可变 label。
	label, value string
}

// New 构造一个还没注册的活性 gauge,由调用方把 Collector() 注册进自己的注册表。opts 里可以带常量 label,不要带可变 label。
func New(opts prometheus.GaugeOpts) *Gauge {
	return &Gauge{vec: prometheus.NewGaugeVec(opts, nil)}
}

// Family 是一族循环共用的活性 gauge:一个指标名,用一个固定 label 区分循环。用 NewFamily 构造,零值不可用。
type Family struct {
	vec   *prometheus.GaugeVec
	label string
}

// NewFamily 构造一个还没注册的活性 gauge 族,label 是区分循环的 label 名(如 "point"),由调用方把 Collector() 注册进注册表。
func NewFamily(opts prometheus.GaugeOpts, label string) *Family {
	return &Family{vec: prometheus.NewGaugeVec(opts, []string{label}), label: label}
}

// Collector 返回底层 collector,给手工注册用。
func (f *Family) Collector() prometheus.Collector { return f.vec }

// For 返回 label 值为 value 的那个循环的活性 gauge。value 只取代码里写死的点位名。nil 族回 nil(见 Gauge 的 nil 约定)。
func (f *Family) For(value string) *Gauge {
	if f == nil {
		return nil
	}
	return &Gauge{vec: f.vec, label: f.label, value: value}
}

// labelValues 是这个 gauge 在 vec 里的 label 值:单独的 gauge 没有,族里的是自己那一个。
func (g *Gauge) labelValues() []string {
	if g.label == "" {
		return nil
	}
	return []string{g.value}
}

// NewRegistered 构造并注册到 reg。reg 为 nil 时只构造不注册(与 promauto.With(nil) 同一约定,单测可以放心用);重名时 panic。
func NewRegistered(reg prometheus.Registerer, opts prometheus.GaugeOpts) *Gauge {
	g := New(opts)
	if reg != nil {
		reg.MustRegister(g.vec)
	}
	return g
}

// Collector 返回底层 collector,给手工注册(prometheus.MustRegister)用。
func (g *Gauge) Collector() prometheus.Collector { return g.vec }

// Mark 记下循环起来、或跑完一轮的时刻。now 取真实时钟:告警拿它和 Prometheus 的 time() 相减。
func (g *Gauge) Mark(now time.Time) {
	if g == nil {
		return
	}
	g.vec.WithLabelValues(g.labelValues()...).Set(float64(now.Unix()))
}

// Round 跑循环的一轮,返回这一轮是否正常跑完。
//
// round 在 safego.Run 里执行:panic 时记点位、栈与 safego_panic_total{point},不打死进程,也不写时刻;
// round 正常返回(它自己处理过的失败也算)才写这一刻。point 是代码里写死的常量点位名(它会变成指标 label)。
func (g *Gauge) Round(point string, round func()) bool {
	if !safego.Run(point, round) {
		return false
	}
	g.Mark(time.Now())
	return true
}

// Stop 删掉这个循环的序列(族里的只删自己那一条)。只在循环有意停止(ctx 结束、排空入口关闭)的 return 之前调,
// 不要 defer(包注释)。
func (g *Gauge) Stop() {
	if g == nil {
		return
	}
	if g.label == "" {
		g.vec.Reset()
		return
	}
	g.vec.DeleteLabelValues(g.value)
}

// Value 读当前值(Unix 秒);没有序列时 ok=false。只读、不创建序列,给单测与自检用。
func (g *Gauge) Value() (unix float64, ok bool) {
	if g == nil {
		return 0, false
	}
	// 一族里可能有很多条序列:另起 goroutine 把 Collect 的输出全部读完,只认自己那一条。
	series := make(chan prometheus.Metric)
	go func() {
		g.vec.Collect(series)
		close(series)
	}()
	for metric := range series {
		var out dto.Metric
		if err := metric.Write(&out); err != nil || !g.owns(&out) {
			continue
		}
		unix, ok = out.GetGauge().GetValue(), true
	}
	return unix, ok
}

// owns 报告 Collect 出来的一条序列是不是这个 gauge 的:单独的 gauge 只有一条;族里按 label 值认。
func (g *Gauge) owns(m *dto.Metric) bool {
	if g.label == "" {
		return true
	}
	for _, pair := range m.GetLabel() {
		if pair.GetName() == g.label {
			return pair.GetValue() == g.value
		}
	}
	return false
}
