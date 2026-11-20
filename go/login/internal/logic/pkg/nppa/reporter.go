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
	reportMaxAge = 150 * time.Second
)

var reportTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "login_nppa_report_total",
	Help: "Login/logout behaviors taken from the NPPA ledger, by result: sent, rejected (refused by the system), " +
		"dropped_age (older than 150s, cannot be reported any more), send_failed (batch will be retried), " +
		"swept (logouts filled in for sessions whose lease ran out).",
}, []string{"result"})

// Reporter 把账本里的上下线事件发给国家系统(docs/design/real-name-anti-addiction.md §5.5)。每个 login 副本跑一份,每秒一拍:
// 先清扫租约到期的会话,再一批批领事件、发、删。每发一批先从所有副本共用的额度(LoginoutQuota)里取一个名额,
// 所以副本加多少都不会超出系统的 10 QPS。事件都在账本里,这里不留状态:进程随时可以停,没发完的由别的副本或下一次启动接着发。
type Reporter struct {
	ledger *Ledger
	quota  *Quota // nil 只在单测里
	send   func(context.Context, []Behavior) ([]int, error)
	maxAge time.Duration
	now    func() time.Time
}

// NewReporter 用 client 上报 ledger 里的事件,用 quota 与别的副本分额度。
func NewReporter(client *Client, ledger *Ledger, quota *Quota) *Reporter {
	return &Reporter{ledger: ledger, quota: quota, send: client.Loginout, maxAge: reportMaxAge, now: time.Now}
}

// Run 一直跑到 ctx 结束。
func (r *Reporter) Run(ctx context.Context) {
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick 是一拍:清扫,然后发到没有到期的事件、没有名额或发送失败为止。
func (r *Reporter) tick(ctx context.Context) {
	swept, err := r.ledger.Sweep(ctx)
	if err != nil {
		logx.Errorf("[nppa] ledger sweep failed, retrying next tick: err=%v", err)
	} else if swept > 0 {
		// 正常收尾的会话不走这里:它计的是断线通知没送到 login 的连接(gate 进程没了等)。
		reportTotal.WithLabelValues("swept").Add(float64(swept))
		logx.Infof("[nppa] filled in logouts for %d sessions whose lease ran out", swept)
	}
	for r.sendBatch(ctx) {
	}
}

// sendBatch 领一批、发一批,返回还要不要接着领。
func (r *Reporter) sendBatch(ctx context.Context) bool {
	claimed, err := r.ledger.Claim(ctx, MaxBatch)
	if err != nil {
		logx.Errorf("[nppa] ledger claim failed, retrying next tick: err=%v", err)
		return false
	}
	if len(claimed) == 0 {
		return false
	}
	// 按本副本的时钟分三类:过期的(报了也会被拒)删掉;时刻不早于现在的(本副本的钟比 Redis 慢)留着,领取到期后再发;其余的发。
	now := r.now().Unix()
	var discard, members []string
	var batch []Behavior
	expired := 0
	for _, c := range claimed {
		switch {
		case !c.Valid:
			logx.Errorf("[nppa] unreadable ledger event dropped")
			discard = append(discard, c.Member)
		case c.Behavior.OT < now-int64(r.maxAge/time.Second):
			expired++
			discard = append(discard, c.Member)
		case c.Behavior.OT < now:
			members = append(members, c.Member)
			batch = append(batch, c.Behavior)
		}
	}
	if err := r.ledger.Ack(ctx, discard); err != nil {
		logx.Errorf("[nppa] ledger ack of dropped events failed: err=%v", err)
	}
	if expired > 0 {
		reportTotal.WithLabelValues("dropped_age").Add(float64(expired))
		logx.Errorf("[nppa] %d behaviors are older than %s and can no longer be reported", expired, r.maxAge)
	}
	if len(batch) == 0 {
		return len(discard) > 0
	}
	if !r.allow(ctx) {
		if err := r.ledger.Release(ctx, members); err != nil {
			logx.Errorf("[nppa] ledger release failed, the batch becomes visible again in %s: err=%v", claimVisibility, err)
		}
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	rejected, err := r.send(callCtx, batch)
	cancel()
	if err != nil {
		// 不放回去:留到领取到期(15 秒)再发,国家系统故障时不会每拍都打过去。
		reportTotal.WithLabelValues("send_failed").Inc()
		logx.Errorf("[nppa] Loginout report failed, the batch is retried in %s: batch=%d err=%v", claimVisibility, len(batch), err)
		return false
	}
	if err := r.ledger.Ack(ctx, members); err != nil {
		logx.Errorf("[nppa] ledger ack failed, the batch will be reported again: batch=%d err=%v", len(batch), err)
	}
	if len(rejected) > 0 {
		logx.Errorf("[nppa] Loginout report: %d of %d behaviors rejected by the system: numbers=%v", len(rejected), len(batch), rejected)
	}
	reportTotal.WithLabelValues("rejected").Add(float64(len(rejected)))
	reportTotal.WithLabelValues("sent").Add(float64(len(batch) - len(rejected)))
	return true
}

// allow 取一个上报名额;取不到或额度读不了都按没有名额处理。
func (r *Reporter) allow(ctx context.Context) bool {
	if r.quota == nil {
		return true
	}
	ok, err := r.quota.Allow(ctx, LoginoutQuota)
	if err != nil {
		logx.Errorf("[nppa] Loginout quota unavailable, retrying next tick: err=%v", err)
	}
	return ok
}
