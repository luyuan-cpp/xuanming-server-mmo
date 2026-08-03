package timertask

import (
	"sync/atomic"
	"testing"
	"time"
)

// startLoop 起一个 loop goroutine，并返回一个"在 loop 上同步执行 f"的助手。
func startLoop(t *testing.T) (*Loop, func(f func())) {
	t.Helper()
	loop := NewLoop(64)
	go loop.Run()
	t.Cleanup(loop.Stop)

	sync := func(f func()) {
		done := make(chan struct{})
		if !loop.Post(func() { f(); close(done) }) {
			t.Fatal("loop already stopped")
		}
		<-done
	}
	return loop, sync
}

func TestRunAfterFires(t *testing.T) {
	loop, sync := startLoop(t)

	var n atomic.Int32
	task := New(loop)
	sync(func() { task.RunAfter(10*time.Millisecond, func() { n.Add(1) }) })

	time.Sleep(60 * time.Millisecond)
	sync(func() {}) // 排空队列，确保 onTimer 已执行完

	if got := n.Load(); got != 1 {
		t.Fatalf("callback fired %d times, want 1", got)
	}
	sync(func() {
		if task.IsActive() {
			t.Error("one-shot should be inactive after firing")
		}
		if task.EndTime() != 0 {
			t.Error("EndTime should be 0 after firing")
		}
	})
}

// 复刻 C++ timer_queue_unit_test 的核心场景：victim 的触发已经在途（已投递
// 进 loop 队列），此时它被重新 arm。那次陈旧触发必须被 generation 守卫拦下，
// 旧回调一次都不能跑。
//
// 顺序完全由 loop 队列的 FIFO 决定 —— 先堵住 loop，把"重新 arm"排在队首，
// victim 的触发投递只能排在其后。刻意不依赖"两个定时器谁先响"：Windows 的
// 系统定时器粒度是 15.6ms，几毫秒之差的两个定时器同拍触发、投递顺序随机。
func TestStaleFiringAfterRearmIsDropped(t *testing.T) {
	loop, sync := startLoop(t)

	var oldVictimCB, newVictimCB atomic.Int32
	victim := New(loop)
	release := make(chan struct{})

	sync(func() { victim.RunAfter(10*time.Millisecond, func() { oldVictimCB.Add(1) }) })

	loop.Post(func() { <-release })
	loop.Post(func() {
		// 重新 arm：schedule 内部先 Cancel，generation 自增
		victim.RunAfter(time.Hour, func() { newVictimCB.Add(1) })
	})

	time.Sleep(120 * time.Millisecond) // victim 必已触发并投递到队尾
	close(release)
	sync(func() {}) // 排空：重新 arm 与 victim 的陈旧 onTimer 都已处理

	if got := oldVictimCB.Load(); got != 0 {
		t.Fatalf("stale victim callback ran %d times, want 0", got)
	}
	if got := newVictimCB.Load(); got != 0 {
		t.Fatalf("new victim callback ran %d times, want 0 (armed for 1h)", got)
	}
	sync(func() {
		if !victim.IsActive() {
			t.Error("victim should still hold its new arming")
		}
		victim.Cancel()
	})
}

// Cancel 说到做到：已经排进 loop 队列的那次触发不得执行回调。
func TestCancelDropsQueuedFiring(t *testing.T) {
	loop, sync := startLoop(t)

	var fired atomic.Int32
	victim := New(loop)
	release := make(chan struct{})

	sync(func() { victim.RunAfter(10*time.Millisecond, func() { fired.Add(1) }) })

	loop.Post(func() { <-release })
	loop.Post(func() { victim.Cancel() })

	time.Sleep(120 * time.Millisecond)
	close(release)
	sync(func() {})

	if got := fired.Load(); got != 0 {
		t.Fatalf("cancelled callback ran %d times, want 0", got)
	}
}

func TestRunEveryRepeatsAndCancels(t *testing.T) {
	loop, sync := startLoop(t)

	var n atomic.Int32
	task := New(loop)
	sync(func() { task.RunEvery(10*time.Millisecond, func() { n.Add(1) }) })

	time.Sleep(105 * time.Millisecond)
	sync(func() { task.Cancel() })

	got := n.Load()
	if got < 5 {
		t.Fatalf("repeating timer fired %d times in ~100ms, want >=5", got)
	}

	time.Sleep(60 * time.Millisecond)
	sync(func() {})
	if after := n.Load(); after != got {
		t.Fatalf("timer kept firing after Cancel: %d -> %d", got, after)
	}
}

// 回调内部重新调度自己：不得出现旧触发与新触发并存。
func TestCallbackReschedulesSelf(t *testing.T) {
	loop, sync := startLoop(t)

	var n atomic.Int32
	task := New(loop)
	var arm func()
	sync(func() {
		arm = func() {
			task.RunAfter(10*time.Millisecond, func() {
				if n.Add(1) < 3 {
					arm()
				}
			})
		}
		arm()
	})

	time.Sleep(120 * time.Millisecond)
	sync(func() {})
	if got := n.Load(); got != 3 {
		t.Fatalf("self-rescheduling chain ran %d times, want exactly 3", got)
	}
}

// loop 停掉之后，定时器触发只是被丢弃，不应 panic 或阻塞。
func TestFiringAfterLoopStopIsDropped(t *testing.T) {
	loop, sync := startLoop(t)

	var fired atomic.Int32
	task := New(loop)
	sync(func() { task.RunAfter(20*time.Millisecond, func() { fired.Add(1) }) })

	loop.Stop()
	time.Sleep(60 * time.Millisecond)

	if got := fired.Load(); got != 0 {
		t.Fatalf("callback ran %d times after loop stop, want 0", got)
	}
}
