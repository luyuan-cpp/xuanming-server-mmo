package timertask

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// peakGoroutines 高频采样 runtime.NumGoroutine()，返回 during 执行期间的峰值。
// 采样器自身也是一个 goroutine，已计入。
func peakGoroutines(during func()) int {
	stop := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		peak := 0
		for {
			if n := runtime.NumGoroutine(); n > peak {
				peak = n
			}
			select {
			case <-stop:
				result <- peak
				return
			default:
			}
			runtime.Gosched()
		}
	}()
	during()
	close(stop)
	return <-result
}

func mkSync(loop *Loop) func(func()) {
	return func(f func()) {
		done := make(chan struct{})
		loop.Post(func() { f(); close(done) })
		<-done
	}
}

// 挂起中的定时器不占 goroutine —— 两种方案都一样。这条先钉死，
// 因为"每个实体一个 goroutine"的担心首先指向这里。
func TestPendingTimersCostNoGoroutines(t *testing.T) {
	const N = 50_000

	loop := NewLoop(1024)
	go loop.Run()
	defer loop.Stop()
	sync := mkSync(loop)

	runtime.GC()
	base := runtime.NumGoroutine()

	// 堆版：N 个任务全部武装
	s := loop.Sched()
	sync(func() {
		for i := 0; i < N; i++ {
			s.NewTask().RunAfter(time.Hour, func() {})
		}
	})
	afterHeap := runtime.NumGoroutine()

	// AfterFunc 版：另外 N 个任务全部武装
	sync(func() {
		for i := 0; i < N; i++ {
			New(loop).RunAfter(time.Hour, func() {})
		}
	})
	afterAfterFunc := runtime.NumGoroutine()

	t.Logf("goroutines: base=%d  堆版+%d挂起=%d  再+%d个AfterFunc挂起=%d",
		base, N, afterHeap, N, afterAfterFunc)

	if afterAfterFunc-base > 5 {
		t.Fatalf("挂起定时器不该占 goroutine：base=%d final=%d", base, afterAfterFunc)
	}
}

// 触发风暴：N 个定时器同一刻到期。这才是两种方案分道扬镳的地方。
func TestFireStormGoroutinePeak(t *testing.T) {
	const N = 20_000
	const due = 300 * time.Millisecond

	// --- 堆版 ---
	loopH := NewLoop(1024)
	go loopH.Run()
	syncH := mkSync(loopH)
	sH := loopH.Sched()

	tasksH := make([]*Task, N)
	syncH(func() {
		for i := range tasksH {
			tasksH[i] = sH.NewTask()
		}
	})
	var nH atomic.Int64
	doneH := make(chan struct{})
	cbH := func() {
		if nH.Add(1) == N {
			close(doneH)
		}
	}

	runtime.GC()
	baseH := runtime.NumGoroutine()
	peakH := peakGoroutines(func() {
		at := time.Now().Add(due)
		syncH(func() {
			for _, t := range tasksH {
				t.RunAt(at, cbH)
			}
		})
		<-doneH
	})
	loopH.Stop()

	// --- 每任务一个 AfterFunc ---
	loopA := NewLoop(1 << 20)
	go loopA.Run()
	syncA := mkSync(loopA)

	tasksA := make([]*TimerTask, N)
	for i := range tasksA {
		tasksA[i] = New(loopA)
	}
	var nA atomic.Int64
	doneA := make(chan struct{})
	cbA := func() {
		if nA.Add(1) == N {
			close(doneA)
		}
	}

	runtime.GC()
	baseA := runtime.NumGoroutine()
	peakA := peakGoroutines(func() {
		at := time.Now().Add(due)
		syncA(func() {
			for _, t := range tasksA {
				t.RunAt(at, cbA)
			}
		})
		<-doneA
	})
	loopA.Stop()

	t.Logf("%d 个定时器同刻到期时的 goroutine 峰值：", N)
	t.Logf("  堆版(NewTimer+select)   base=%d peak=%d  净增 %d", baseH, peakH, peakH-baseH)
	t.Logf("  每任务一个 AfterFunc     base=%d peak=%d  净增 %d", baseA, peakA, peakA-baseA)

	// 堆版的净增应当只有采样器那一个 goroutine，与 N 无关。
	if peakH-baseH > 5 {
		t.Fatalf("堆版不该随到期数量派生 goroutine：净增 %d", peakH-baseH)
	}
}
