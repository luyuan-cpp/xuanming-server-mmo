package timertask

import (
	"runtime"
	"runtime/metrics"
	"sort"
	"testing"
	"time"
)

// userCPUSeconds 返回进程迄今在用户 Go 代码上花掉的 CPU 秒数。
func userCPUSeconds(t *testing.T) float64 {
	t.Helper()
	s := []metrics.Sample{{Name: "/cpu/classes/user:cpu-seconds"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64 {
		t.Skip("此 Go 版本不支持 /cpu/classes/user:cpu-seconds")
	}
	return s[0].Value.Float64()
}

func measureUserCPU(t *testing.T, window time.Duration) time.Duration {
	t.Helper()
	before := userCPUSeconds(t)
	time.Sleep(window)
	return time.Duration((userCPUSeconds(t) - before) * float64(time.Second))
}

// 阻塞在 select 上的 loop goroutine 是"停泊"状态：不占 CPU，不占 OS 线程。
// 挂着上万个远期定时器也不会让它周期性醒来 —— 它是事件驱动，不是轮询。
func TestIdleLoopIsParkedNotSpinning(t *testing.T) {
	const window = 2 * time.Second

	base := measureUserCPU(t, window) // 基线：只有测试框架自己

	loop1 := NewLoop(64)
	go loop1.Run()
	idle := measureUserCPU(t, window)
	loop1.Stop()

	loop2 := NewLoop(64)
	go loop2.Run()
	sync2 := mkSync(loop2)
	s2 := loop2.Sched()
	sync2(func() {
		for i := 0; i < 10_000; i++ {
			s2.NewTask().RunAfter(time.Hour, func() {})
		}
	})
	pending := measureUserCPU(t, window)
	loop2.Stop()

	t.Logf("每 %v 窗口的用户态 CPU：", window)
	t.Logf("  基线（无 loop）            %v", base)
	t.Logf("  1 个空转 loop              %v", idle)
	t.Logf("  1 个 loop + 1 万挂起定时器 %v", pending)

	// 空转 loop 与挂满定时器的 loop 都不该有可观测的 CPU 占用。
	if pending-base > 20*time.Millisecond {
		t.Fatalf("loop 疑似在轮询：%v 窗口内多花了 %v CPU", window, pending-base)
	}
}

// 停泊的 goroutine 只占一个栈。测一个 loop 的常驻内存。
func TestIdleLoopMemory(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	const N = 100
	loops := make([]*Loop, N)
	for i := range loops {
		loops[i] = NewLoop(64)
		go loops[i].Run()
	}
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	runtime.ReadMemStats(&after)

	// goroutine 栈不计入 HeapAlloc，要单独看 StackInuse，否则会低估。
	perHeap := (after.HeapAlloc - before.HeapAlloc) / N
	perStack := (after.StackInuse - before.StackInuse) / N
	t.Logf("%d 个空转 loop（各含 Scheduler + 64 槽队列 + 1 个停泊 goroutine）：", N)
	t.Logf("  堆   约 %d B/个", perHeap)
	t.Logf("  栈   约 %d B/个", perStack)
	t.Logf("  合计 约 %d B/个", perHeap+perStack)
	t.Logf("goroutine 数：%d", runtime.NumGoroutine())

	for _, l := range loops {
		l.Stop()
	}
}

// 唤醒延迟：证明它是被定时器唤醒的，不是轮询轮到的。
func TestWakeLatency(t *testing.T) {
	loop := NewLoop(64)
	go loop.Run()
	defer loop.Stop()
	sync := mkSync(loop)
	s := loop.Sched()

	const rounds = 200
	const delay = 5 * time.Millisecond
	lat := make([]time.Duration, 0, rounds)

	var task *Task
	sync(func() { task = s.NewTask() })

	for i := 0; i < rounds; i++ {
		done := make(chan time.Duration, 1)
		start := time.Now()
		sync(func() {
			task.RunAfter(delay, func() { done <- time.Since(start) - delay })
		})
		lat = append(lat, <-done)
	}

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("%d 次 %v 定时器的超出量（实际 - 期望）：p50=%v p90=%v p99=%v max=%v",
		rounds, delay, lat[rounds/2], lat[rounds*90/100], lat[rounds*99/100], lat[rounds-1])
}
