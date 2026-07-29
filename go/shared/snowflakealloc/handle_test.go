package snowflakealloc

import "testing"

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
