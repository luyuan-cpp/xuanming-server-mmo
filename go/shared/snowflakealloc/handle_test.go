package snowflakealloc

import (
	"testing"
	"time"
)

func newTestHandle(workerID uint64) *Handle {
	return &Handle{WorkerID: workerID, lost: make(chan struct{})}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// 租约真的没了(KeepAlive 流在没有 Close 的情况下结束)必须通知调用方:
// 此刻 etcd 已经可以把同一个 worker id 分给别的进程,继续发号就是确定性撞号。
func TestHandle_LeaseLossSignalsLost(t *testing.T) {
	h := newTestHandle(7)

	h.onKeepAliveEnded("/test", "host-a")

	if !isClosed(h.Lost()) {
		t.Fatal("lease loss must close Lost()")
	}
}

// 正常 Close() 引起的 KeepAlive 结束不是身份丢失,不能误报 ——
// 否则每次优雅停机都会触发调用方的"停服"分支。
func TestHandle_CloseIsNotReportedAsLost(t *testing.T) {
	h := newTestHandle(7)
	h.closing.Store(true) // Close() 做的第一件事

	h.onKeepAliveEnded("/test", "host-a")

	if isClosed(h.Lost()) {
		t.Fatal("graceful Close() must not be reported as a lost lease")
	}
}

// 重复触发必须幂等(close 一个已关闭的 channel 会 panic)。
func TestHandle_LostIsIdempotent(t *testing.T) {
	h := newTestHandle(7)

	h.onKeepAliveEnded("/test", "host-a")
	h.onKeepAliveEnded("/test", "host-a")

	if !isClosed(h.Lost()) {
		t.Fatal("Lost() should stay closed")
	}
}

// nil Handle 上取信号不应崩(调用方可能在分配失败后仍然监听)。
func TestHandle_NilLostIsSafe(t *testing.T) {
	var h *Handle
	if h.Lost() != nil {
		t.Fatal("nil handle should report a nil channel")
	}
}

// 只靠"KeepAlive channel 关闭"感知失租**恒晚于服务端过期点**:clientv3 把 ka.deadline
// 设成"收到响应的时刻 + TTL",而服务端过期点是"它处理续租的时刻 + TTL",前者恒晚一个 RTT,
// 再叠加 deadlineLoop 每 1s 才扫一轮。等 channel 关闭时,worker id 可能已被分给别的进程。
// 而本包的消费方收到 Lost() 后是**优雅停**,排空期间仍在发号 —— 所以必须在服务端过期点
// **之前**主动 fence,把剩余租约留给排空。
func TestHandle_SelfFencesBeforeChannelCloses(t *testing.T) {
	h := newTestHandle(11)

	// 距上次成功续租已超预算 ⇒ 无法再证明自己持有 worker id,必须立刻报信,
	// 不等 etcd 把 channel 关掉。
	h.onSelfFenced("/test", "host-a", 41*time.Second, 40*time.Second)

	if !isClosed(h.Lost()) {
		t.Fatal("超过自 fencing 预算必须关闭 Lost(),不能干等 channel 关闭")
	}
}

// 正常 Close() 期间不得被自 fencing 误报成失租,否则每次优雅停机都会触发调用方的停服分支。
func TestHandle_SelfFenceSuppressedWhileClosing(t *testing.T) {
	h := newTestHandle(11)
	h.closing.Store(true)

	h.onSelfFenced("/test", "host-a", 41*time.Second, 40*time.Second)

	if isClosed(h.Lost()) {
		t.Fatal("Close() 期间的自 fencing 检查不得报成失租")
	}
}

// 自 fencing 预算必须显著早于服务端过期点(留出排空余量),又显著晚于 clientv3 的
// 续租间隔 TTL/3(否则健康时也会误报)。这两条边界一起把取值钉死在 TTL 的 2/3。
func TestSelfFenceAfter_SitsBetweenRenewIntervalAndExpiry(t *testing.T) {
	for _, ttlSec := range []int64{15, 30, 60, 120} {
		ttl := time.Duration(ttlSec) * time.Second
		renewInterval := ttl / 3 // clientv3 的续租节奏
		got := selfFenceAfter(ttlSec)

		if got <= renewInterval {
			t.Fatalf("ttl=%ds: fenceAfter=%v <= 续租间隔 %v,健康时会误报", ttlSec, got, renewInterval)
		}
		if got >= ttl {
			t.Fatalf("ttl=%ds: fenceAfter=%v >= TTL %v,晚于服务端过期点就失去意义", ttlSec, got, ttl)
		}
	}
}

// TTL 极小时 fenceAfter 不能退化到与续租间隔同量级(会误报),必须被下界钳住。
func TestSelfFenceAfter_ClampsTinyTTL(t *testing.T) {
	if got := selfFenceAfter(1); got < 2*time.Second {
		t.Fatalf("ttl=1s 时 fenceAfter=%v 未被下界钳住", got)
	}
}
