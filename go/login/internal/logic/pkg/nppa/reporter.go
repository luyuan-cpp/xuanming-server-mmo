package nppa

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	reportInterval = time.Second
	// reportMaxAge 留出余量:规范要求一批里最早的事件与上报时刻相差小于 180 秒。
	reportMaxAge    = 150 * time.Second
	reportQueueSize = 8192
	// maxPending 是 maxAge 之内最多能发出去的条数,多出来的反正会过期。
	maxPending = MaxBatch * int(reportMaxAge/reportInterval)
)

var reportTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "login_nppa_report_total",
	Help: "Login/logout behaviors handed to the NPPA reporter, by result (sent / rejected / dropped_age / dropped_full).",
}, []string{"result"})

// Reporter 在一个 login 副本里攒批上报上下线(docs/design/real-name-anti-addiction.md §5.5):每批至多 MaxBatch 条,
// 两次请求至少隔 1 秒、同一时刻只有一个请求在途,所以每个副本至多 1 QPS(系统限 10 QPS,副本数不能超过 10)。
// 失败的批留着重试,直到其中的事件过了 150 秒,丢弃并计数。
//
// shortcut: 事件只在内存里,进程崩溃或退出会丢还没发出的几秒;180 秒的时限让持久化队列收益很小。login 副本要超过 10 个时改成集中上报。
type Reporter struct {
	send     func(context.Context, []Behavior) ([]int, error)
	queue    chan Behavior
	interval time.Duration
	maxAge   time.Duration
	now      func() time.Time
}

// NewReporter 用 client 上报。
func NewReporter(client *Client) *Reporter {
	return &Reporter{
		send:     client.Loginout,
		queue:    make(chan Behavior, reportQueueSize),
		interval: reportInterval,
		maxAge:   reportMaxAge,
		now:      time.Now,
	}
}

// Report 排一条记录,不阻塞;队列满就丢弃并计数。
func (r *Reporter) Report(b Behavior) {
	select {
	case r.queue <- b:
	default:
		reportTotal.WithLabelValues("dropped_full").Inc()
	}
}

// Run 一直跑到 ctx 结束。
func (r *Reporter) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	var pending []Behavior
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-r.queue:
			if len(pending) >= maxPending {
				reportTotal.WithLabelValues("dropped_full").Inc()
				continue
			}
			pending = append(pending, b)
		case <-ticker.C:
			pending = r.flush(ctx, pending)
		}
	}
}

// flush 丢掉过期的,发出最早的一批,返回剩下的。
func (r *Reporter) flush(ctx context.Context, pending []Behavior) []Behavior {
	cutoff := r.now().Add(-r.maxAge).Unix()
	kept := pending[:0]
	for _, b := range pending {
		if b.OT < cutoff {
			reportTotal.WithLabelValues("dropped_age").Inc()
			continue
		}
		kept = append(kept, b)
	}
	if len(kept) == 0 {
		return kept
	}
	n := min(len(kept), MaxBatch)
	callCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	rejected, err := r.send(callCtx, kept[:n])
	cancel()
	if err != nil {
		logx.Errorf("[nppa] Loginout report failed, retrying next tick: batch=%d pending=%d err=%v", n, len(kept), err)
		return kept
	}
	if len(rejected) > 0 {
		logx.Errorf("[nppa] Loginout report: %d of %d behaviors rejected by the system: numbers=%v", len(rejected), n, rejected)
	}
	reportTotal.WithLabelValues("rejected").Add(float64(len(rejected)))
	reportTotal.WithLabelValues("sent").Add(float64(n - len(rejected)))
	return append(kept[:0], kept[n:]...)
}
