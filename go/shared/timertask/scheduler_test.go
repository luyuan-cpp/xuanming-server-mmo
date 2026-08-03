package timertask

import (
	"sync/atomic"
	"testing"
	"time"
)

// ---- Scheduler 正确性：与 TimerTask 同样的场景 -------------------------------

func TestSched_RunAfterFires(t *testing.T) {
	loop, sync := startLoop(t)
	s := loop.Sched()

	var n atomic.Int32
	var task *Task
	sync(func() {
		task = s.NewTask()
		task.RunAfter(10*time.Millisecond, func() { n.Add(1) })
	})

	time.Sleep(60 * time.Millisecond)
	sync(func() {})

	if got := n.Load(); got != 1 {
		t.Fatalf("callback fired %d times, want 1", got)
	}
	sync(func() {
		if task.IsActive() || s.Len() != 0 {
			t.Errorf("one-shot should be gone: active=%v len=%d", task.IsActive(), s.Len())
		}
	})
}

// 批内前一个回调重新 arm 了后一个任务 —— 陈旧触发必须被 generation 拦下。
// 这里两个任务到期时刻只差 1ms，必定落在同一批。
func TestSched_StaleFiringInSameBatchIsDropped(t *testing.T) {
	loop, sync := startLoop(t)
	s := loop.Sched()

	var oldCB, newCB atomic.Int32
	var driver, victim *Task
	release := make(chan struct{})

	sync(func() {
		driver, victim = s.NewTask(), s.NewTask()
		victim.RunAfter(6*time.Millisecond, func() { oldCB.Add(1) })
		driver.RunAfter(5*time.Millisecond, func() {
			victim.RunAfter(time.Hour, func() { newCB.Add(1) })
		})
	})
	loop.Post(func() { <-release }) // 堵住 loop，保证两者进同一批

	time.Sleep(50 * time.Millisecond)
	close(release)
	sync(func() {})

	if got := oldCB.Load(); got != 0 {
		t.Fatalf("stale callback ran %d times, want 0", got)
	}
	if got := newCB.Load(); got != 0 {
		t.Fatalf("new callback ran %d times, want 0 (armed for 1h)", got)
	}
	sync(func() { victim.Cancel() })
}

func TestSched_CancelInSameBatchIsHonoured(t *testing.T) {
	loop, sync := startLoop(t)
	s := loop.Sched()

	var fired atomic.Int32
	release := make(chan struct{})
	sync(func() {
		driver, victim := s.NewTask(), s.NewTask()
		victim.RunAfter(6*time.Millisecond, func() { fired.Add(1) })
		driver.RunAfter(5*time.Millisecond, func() { victim.Cancel() })
	})
	loop.Post(func() { <-release })

	time.Sleep(50 * time.Millisecond)
	close(release)
	sync(func() {})

	if got := fired.Load(); got != 0 {
		t.Fatalf("cancelled callback ran %d times, want 0", got)
	}
}

func TestSched_RunEveryRepeatsAndCancels(t *testing.T) {
	loop, sync := startLoop(t)
	s := loop.Sched()

	var n atomic.Int32
	var task *Task
	sync(func() {
		task = s.NewTask()
		task.RunEvery(10*time.Millisecond, func() { n.Add(1) })
	})

	time.Sleep(105 * time.Millisecond)
	sync(func() { task.Cancel() })
	got := n.Load()
	if got < 5 {
		t.Fatalf("repeating timer fired %d times in ~100ms, want >=5", got)
	}

	time.Sleep(60 * time.Millisecond)
	sync(func() {})
	if after := n.Load(); after != got {
		t.Fatalf("kept firing after Cancel: %d -> %d", got, after)
	}
}

// 乱序武装 + 中途取消，必须严格按到期顺序触发。
func TestSched_OrderingAndRemoval(t *testing.T) {
	loop, sync := startLoop(t)
	s := loop.Sched()

	var order []int
	done := make(chan struct{})
	sync(func() {
		delays := []time.Duration{50, 10, 40, 20, 30}
		tasks := make([]*Task, len(delays))
		for i, d := range delays {
			i := i
			tasks[i] = s.NewTask()
			tasks[i].RunAfter(d*time.Millisecond, func() {
				order = append(order, i)
				if len(order) == 4 {
					close(done)
				}
			})
		}
		tasks[2].Cancel() // 摘掉中间那个（40ms）
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	sync(func() {
		want := []int{1, 3, 4, 0} // 10, 20, 30, 50
		if len(order) != len(want) {
			t.Fatalf("order = %v, want %v", order, want)
		}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("order = %v, want %v", order, want)
			}
		}
	})
}

// ---- 基准 -------------------------------------------------------------------

func benchLoop(b *testing.B, qsize int) (*Loop, func(func())) {
	b.Helper()
	loop := NewLoop(qsize)
	go loop.Run()
	b.Cleanup(loop.Stop)
	sync := func(f func()) {
		done := make(chan struct{})
		loop.Post(func() { f(); close(done) })
		<-done
	}
	return loop, sync
}

// 武装 + 取消，不触发。游戏里最常见的路径：buff 刷新、技能打断、心跳重排。
func BenchmarkArmCancel_AfterFuncPerTask(b *testing.B) {
	loop, sync := benchLoop(b, 1024)
	task := New(loop)
	cb := func() {}
	b.ReportAllocs()
	b.ResetTimer()
	sync(func() {
		for i := 0; i < b.N; i++ {
			task.RunAfter(time.Hour, cb)
			task.Cancel()
		}
	})
}

func BenchmarkArmCancel_SchedulerHeap(b *testing.B) {
	loop, sync := benchLoop(b, 1024)
	s := loop.Sched()
	var task *Task
	sync(func() { task = s.NewTask() })
	cb := func() {}
	b.ReportAllocs()
	b.ResetTimer()
	sync(func() {
		for i := 0; i < b.N; i++ {
			task.RunAfter(time.Hour, cb)
			task.Cancel()
		}
	})
}

// 堆里已有大量挂起任务时的武装/取消成本（O(log n) 的 n 有多疼）。
func BenchmarkArmCancel_SchedulerHeap_100kPending(b *testing.B) {
	loop, sync := benchLoop(b, 1024)
	s := loop.Sched()
	var task *Task
	sync(func() {
		for i := 0; i < 100_000; i++ {
			s.NewTask().RunAfter(time.Duration(i%3600)*time.Second+time.Hour, func() {})
		}
		task = s.NewTask()
	})
	cb := func() {}
	b.ReportAllocs()
	b.ResetTimer()
	sync(func() {
		for i := 0; i < b.N; i++ {
			task.RunAfter(30*time.Minute, cb)
			task.Cancel()
		}
	})
}

const fireDelay = 50 * time.Millisecond

// 触发吞吐：b.N 个任务几乎同时到期，测"从武装到全部回调执行完"的墙钟。
// 含一个固定的 fireDelay 偏移，b.N 大时可忽略。
func BenchmarkFire_AfterFuncPerTask(b *testing.B) {
	loop, sync := benchLoop(b, 1<<21)
	tasks := make([]*TimerTask, b.N)
	for i := range tasks {
		tasks[i] = New(loop)
	}
	done := make(chan struct{})
	n := 0
	cb := func() {
		n++
		if n == b.N {
			close(done)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	sync(func() {
		for _, t := range tasks {
			t.RunAfter(fireDelay, cb)
		}
	})
	<-done
	b.StopTimer()
}

func BenchmarkFire_SchedulerHeap(b *testing.B) {
	loop, sync := benchLoop(b, 1024)
	s := loop.Sched()
	tasks := make([]*Task, b.N)
	sync(func() {
		for i := range tasks {
			tasks[i] = s.NewTask()
		}
	})
	done := make(chan struct{})
	n := 0
	cb := func() {
		n++
		if n == b.N {
			close(done)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	sync(func() {
		for _, t := range tasks {
			t.RunAfter(fireDelay, cb)
		}
	})
	<-done
	b.StopTimer()
}

// ---- 基线：裸用官方 time.AfterFunc，不经 loop ---------------------------------
// 回调直接跑在 runtime 定时器 goroutine 上。对纯计数尚可，对游戏状态就是
// 并发写 —— 计数器这里必须用 atomic，本身就说明了问题。

func BenchmarkArmCancel_RawAfterFunc(b *testing.B) {
	cb := func() {}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t := time.AfterFunc(time.Hour, cb)
		t.Stop()
	}
}

func BenchmarkFire_RawAfterFunc(b *testing.B) {
	var n atomic.Int64
	target := int64(b.N)
	done := make(chan struct{})
	cb := func() {
		if n.Add(1) == target {
			close(done)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		time.AfterFunc(fireDelay, cb)
	}
	<-done
	b.StopTimer()
}

// 纯 loop 投递开销：一次 channel 送 + 一次收 + 调度唤醒。
func BenchmarkLoopPost(b *testing.B) {
	loop := NewLoop(1024)
	go loop.Run()
	defer loop.Stop()

	done := make(chan struct{})
	n := 0
	f := func() {
		n++
		if n == b.N {
			close(done)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loop.Post(f)
	}
	<-done
}
