package lexstore

import (
	"context"
	"errors"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/etcdwatch"
)

// 加载失败的原因名,给指标做标签,基数固定。
const (
	FailureEtcd    = "etcd"    // 读 etcd 失败
	FailureCorrupt = "corrupt" // 指针或分块与预期不符
	FailureApply   = "apply"   // 正文读到了,但解析或编译不过
	FailureWatch   = "watch"   // watch 中断
)

// defaultRetryBackoff 是读 etcd 失败后的重试间隔,与热关停(shared/killswitch)的全量同步同值。
const defaultRetryBackoff = 3 * time.Second

// Watcher 让本进程的词库跟着 etcd 里的指针走:启动时读一次,之后指针一变就重读。
//
// 只做连接层的事:什么时候读、读到的东西对不对。读到之后怎么用由 Apply 决定。
// 任何失败都保持现有词库不动(包括指针被删掉),只回调 OnFailure;宁可继续用旧词库,也不退回只有内置词。
type Watcher struct {
	Client *clientv3.Client
	Prefix string
	// Apply 解析、编译并换上一份词库。返回错误表示这一版不能用。同一个指针不会重复调用,除非上一次失败了。
	Apply func(pointer Pointer, text []byte) error
	// OnFailure 可选。reason 是上面的常量之一。
	OnFailure func(reason string, err error)
	// Resync 是 watch 连续跑多久就重新全量读一次,0 取 etcdwatch.ResyncInterval(5 分钟)。
	// 它兜住「watch 不报错地停住」的未知情况,也让上次 Apply 失败的版本隔一阵再试一次。
	Resync time.Duration
	// RetryBackoff 是读失败或 watch 中断之后的等待,0 取 3 秒。
	RetryBackoff time.Duration
}

// Run 阻塞到 ctx 结束。
func (w *Watcher) Run(ctx context.Context) {
	backoff := w.RetryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	var applied Pointer
	for ctx.Err() == nil {
		loaded, err := Load(ctx, w.Client, w.Prefix)
		if err != nil {
			reason := FailureEtcd
			if errors.Is(err, ErrCorrupt) {
				reason = FailureCorrupt
			}
			w.fail(reason, err)
			if !sleep(ctx, backoff) {
				return
			}
			continue
		}
		// Text 为 nil = etcd 里(还)没有词库:保持现状,等指针出现。
		if loaded.Text != nil && (loaded.Pointer.Version != applied.Version || loaded.Pointer.SHA256 != applied.SHA256) {
			if err = w.Apply(loaded.Pointer, loaded.Text); err != nil {
				// 同一份内容立刻再试结果一样,所以不退避重试:等指针再变,或者到 Resync 的点再试。
				w.fail(FailureApply, err)
			} else {
				applied = loaded.Pointer
			}
		}
		if !w.watch(ctx, loaded.Revision) && !sleep(ctx, backoff) {
			return
		}
	}
}

// watch 等到指针有变化或到了重读的点就返回 true;watch 自己出错返回 false(调用方退避后重读)。
func (w *Watcher) watch(ctx context.Context, revision int64) bool {
	ch, resync, stop := etcdwatch.Open(ctx, w.Client, w.Resync, PointerKey(w.Prefix), clientv3.WithRev(revision+1))
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return true
		case <-resync:
			return true
		case resp, ok := <-ch:
			if !ok {
				if ctx.Err() == nil {
					w.fail(FailureWatch, errors.New("lexstore: watch channel closed"))
				}
				return false
			}
			if err := resp.Err(); err != nil {
				w.fail(FailureWatch, err)
				return false
			}
			if len(resp.Events) > 0 {
				return true
			}
		}
	}
}

func (w *Watcher) fail(reason string, err error) {
	if w.OnFailure != nil {
		w.OnFailure(reason, err)
	}
}

// sleep 等 d 或 ctx 结束;ctx 结束返回 false。
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
