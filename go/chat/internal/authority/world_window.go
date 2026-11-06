package authority

import (
	"context"
	"errors"
	"sync"
	"time"

	"shared/safego"

	"github.com/prometheus/client_golang/prometheus"
)

// 世界频道近窗快照(docs/design/chat-world-channel-fanout.md §3)。
//
// 世界频道全服只有一条流,直读时每次拉取都在 SQL 上读同一段索引尾部,读量随在线人数线性增长。
// 每个副本改为持有一份「最新若干行」的快照:过期才重读,同一时刻只有一次重读在途。
// SQL 仍是唯一真相:快照只放已提交的原始行,读失败不缓存,应答语义与直读共用 visibleHistory。

// worldWindowLoadTimeout 是一次重读的上限。重读不受触发它的请求取消影响(其他请求在等同一个结果),
// 所以要有自己的期限;它小于 chat 的服务端超时(Timeout 上限 4000ms),卡住的库不会让等待方一直挂着。
const worldWindowLoadTimeout = 3 * time.Second

// errWorldWindowLoadAborted:重读没有给出结果(读取函数 panic,或者返回了空快照)。按存储故障回。
var errWorldWindowLoadAborted = errors.New("聊天世界频道快照重读中断")

var worldWindowLoadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Subsystem: "chat",
	Name:      "world_window_load_total",
	Help:      "World channel history snapshot reloads by outcome (ok|error); hits = chat_pull_total{channel=\"world\",outcome=\"ok\"} minus ok reloads.",
}, []string{"outcome"})

// MetricCollectors 返回本包的指标,由 svc.StartMetrics 统一注册进默认 registry。
func MetricCollectors() []prometheus.Collector {
	return []prometheus.Collector{worldWindowLoadTotal}
}

// worldSnapshot 是一次 SQL 读取的结果:世界频道最新的若干行(新到旧),以及读取时的数据库时间。
type worldSnapshot struct {
	rows    []historyRow
	dbNowMs uint64
	takenAt time.Duration // 读取完成时的单调时刻
}

// nowMs 把读取时的数据库时间推进到 at:TTL 窗口必须用数据库时间,不能换成本机墙钟。
func (s *worldSnapshot) nowMs(at time.Duration) uint64 {
	age := at - s.takenAt
	if age < 0 {
		age = 0
	}
	return s.dbNowMs + uint64(age/time.Millisecond)
}

// worldLoad 是一次在途的重读。done 关闭之后 snap / err 只读。
type worldLoad struct {
	done chan struct{}
	snap *worldSnapshot
	err  error
}

// worldWindow 持有快照并让重读单飞。状态全部由 mu 保护;load 在锁外执行。
type worldWindow struct {
	maxAge time.Duration
	now    func() time.Duration // 单调时钟
	load   func(context.Context) (*worldSnapshot, error)

	mu       sync.Mutex
	snap     *worldSnapshot
	inflight *worldLoad
	// epoch 每次作废加一。重读开始时记下它,结束时不相等就说明期间有本副本的世界频道提交,
	// 这份结果可能没有那条消息,只交给已经在等的请求,不进缓存。
	epoch uint64
}

func newWorldWindow(maxAge time.Duration, now func() time.Duration, load func(context.Context) (*worldSnapshot, error)) *worldWindow {
	return &worldWindow{maxAge: maxAge, now: now, load: load}
}

// get 返回未过期的快照;过期就加入(或发起)一次重读,等到结果或请求自己的期限。
func (w *worldWindow) get(ctx context.Context) (*worldSnapshot, error) {
	w.mu.Lock()
	if s := w.snap; s != nil && w.now()-s.takenAt < w.maxAge {
		w.mu.Unlock()
		return s, nil
	}
	load := w.inflight
	if load == nil {
		load = &worldLoad{done: make(chan struct{})}
		w.inflight = load
		epoch := w.epoch
		// 重读在独立 goroutine 里跑:发起它的请求超时或取消时,等着同一结果的其他请求不受牵连。
		loadCtx := context.WithoutCancel(ctx)
		safego.Go("chat.world_window_load", func() { w.run(loadCtx, load, epoch) })
	}
	w.mu.Unlock()
	select {
	case <-load.done:
		return load.snap, load.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (w *worldWindow) run(ctx context.Context, load *worldLoad, epoch uint64) {
	// done 必须关:load 若 panic(safego 兜住),等待方也要拿到错误而不是挂到各自超时。
	snap, err := (*worldSnapshot)(nil), errWorldWindowLoadAborted
	// 快照时刻取读之前:load 先读数据库时间再读行,这样 nowMs 推算的数据库时间不会偏早,年龄也只会偏大(保守)。
	started := w.now()
	defer func() {
		w.mu.Lock()
		if w.inflight == load {
			w.inflight = nil
		}
		if err == nil && w.epoch == epoch {
			w.snap = snap
		}
		w.mu.Unlock()
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		worldWindowLoadTotal.WithLabelValues(outcome).Inc()
		load.snap, load.err = snap, err
		close(load.done)
	}()
	loadCtx, cancel := context.WithTimeout(ctx, worldWindowLoadTimeout)
	defer cancel()
	snap, err = w.load(loadCtx)
	if err == nil && snap == nil {
		err = errWorldWindowLoadAborted
	}
	if err == nil {
		snap.takenAt = started
	}
}

// invalidate 在本副本世界频道提交成功后调用:丢掉快照,在途重读的结果也不再进缓存,
// 之后到来的请求一定发起新的重读,所以本副本上的读己之写成立。
func (w *worldWindow) invalidate() {
	w.mu.Lock()
	w.snap = nil
	w.inflight = nil
	w.epoch++
	w.mu.Unlock()
}
