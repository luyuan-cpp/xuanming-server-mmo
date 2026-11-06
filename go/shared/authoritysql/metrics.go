package authoritysql

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// frozenObservationTTL:超过这么久没再撞到冻结,就当闸门已经解冻。
// 冻结时长只有写者撞到闸门时才看得到。解冻之后进程可能一直没有新事务,没有这条的话指标会一直涨下去,冻结早就结束了还在告警。
// 代价是完全没有写流量的进程上报 0:这种进程也不受冻结影响。
const frozenObservationTTL = 15 * time.Second

// frozenGate 记一个逻辑库的闸门从什么时候开始被这个进程观察到冻结。
type frozenGate struct {
	active   atomic.Bool // 只给成功路径做一次无锁判断:每笔写事务都要过这里
	mu       sync.Mutex
	since    time.Time
	last     time.Time
	register sync.Once
}

func (g *frozenGate) observe(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.since.IsZero() || now.Sub(g.last) > frozenObservationTTL {
		g.since = now
	}
	g.last = now
	g.active.Store(true)
}

func (g *frozenGate) clear() {
	if !g.active.Load() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.since, g.last = time.Time{}, time.Time{}
	g.active.Store(false)
}

// seconds 是到 now 为止连续冻结了多久,没有在冻结时是 0。
func (g *frozenGate) seconds(now time.Time) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.since.IsZero() || now.Sub(g.last) > frozenObservationTTL {
		return 0
	}
	return now.Sub(g.since).Seconds()
}

// noteFrozen 在闸门把写者挡回去时调。第一次撞到才注册序列:没撞过冻结的库不多出一条恒为 0 的序列。
// 同一进程同一逻辑库只有一份写资格,重复注册只出现在测试夹具换 boot 时,新的那份不上报不影响判定,错误不用管。
func (b *writerBoot) noteFrozen() {
	b.frozen.observe(time.Now())
	b.frozen.register.Do(func() {
		gate := &b.frozen
		_ = prometheus.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "writer_frozen_seconds",
			Help:        "这个进程观察到逻辑库的写者闸门连续冻结了多少秒。搬库窗口是秒级,长时间不归零要查谁留下的冻结。",
			ConstLabels: prometheus.Labels{"logical_db": b.grant.AuthoritySchema},
		}, func() float64 { return gate.seconds(time.Now()) }))
	})
}

// noteAdmitted 在一笔事务过了闸门时调。
func (b *writerBoot) noteAdmitted() {
	b.frozen.clear()
	if !b.admitted.Load() {
		b.admitted.Store(true)
	}
}

// noteReadable 在事务外纯读看到闸门可写时调:冻结已经结束,清掉冻结计时。不算过过闸门(admitted 只由写事务置上),
// 启动阶段「读在冻结时放行」的规则因此不受只读流量影响。
func (b *writerBoot) noteReadable() { b.frozen.clear() }
