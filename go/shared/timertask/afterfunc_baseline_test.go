package timertask

import "time"

// 本文件是**基准对照用的第二实现，不参与生产构建**（文件名带 _test）。
//
// 它按"每个任务一个 time.AfterFunc + 投递回 loop"的写法实现同一套语义，
// 存在的唯一理由是给 scheduler_test.go 的基准提供对照项 —— 正是它证明了
// 生产实现（Scheduler，单 time.NewTimer + 到期堆）为什么值得那点复杂度：
// 2 万个定时器同刻到期时，本实现瞬时派生 3455 个 goroutine，Scheduler 是 0。
//
// 顺带一提，这也是 name5566/leaf 的 skeleton.AfterFunc 采用的结构。
//
// 不要在业务里用它，也不需要让它跟上游 C++ 保持同步。

// TimerTask 对应 TimerTaskComp。
//
// 线程契约（与 muduo 一致，且 C++ 版其实也隐含依赖）：除 Loop.Post 外，
// 本类型所有方法都必须在 Loop.Run 所在的 goroutine 上调用。因此
// generation / timer / cb 全部是普通字段，不需要原子量或锁。
// 若确实要跨 goroutine 取消，走 loop.Post(func(){ t.Cancel() })。
//
// 与 C++ 版的一处硬差异：Go 没有析构函数。实体销毁 / 组件移除路径必须
// 显式调用 Cancel()，否则定时器会在其属主消失后仍然触发。
type TimerTask struct {
	_ noCopy

	loop *Loop

	timer    *time.Timer
	cb       func()
	deadline time.Time

	repeating bool
	interval  time.Duration

	// 每次 Cancel() 自增 —— 因而每次重新 arm 也会自增（schedule 先 Cancel）。
	// 只做相等比较，回绕无害。
	generation uint64
}

func New(loop *Loop) *TimerTask {
	return &TimerTask{loop: loop}
}

// ---- 调度 -------------------------------------------------------------------

func (t *TimerTask) RunAt(at time.Time, cb func()) {
	t.schedule(time.Until(at), false, cb)
}

func (t *TimerTask) RunAfter(d time.Duration, cb func()) {
	t.schedule(d, false, cb)
}

func (t *TimerTask) RunEvery(interval time.Duration, cb func()) {
	t.schedule(interval, true, cb)
}

func (t *TimerTask) schedule(d time.Duration, repeating bool, cb func()) {
	t.Cancel() // 同时自增 generation，作废任何已在途的触发

	if cb == nil || t.loop == nil {
		return
	}
	if repeating && d <= 0 {
		return // 周期必须为正，否则是忙循环
	}

	t.cb = cb
	t.repeating = repeating
	t.interval = d
	t.deadline = time.Now().Add(d)

	gen := t.generation
	t.timer = time.AfterFunc(d, func() {
		// 这里跑在 runtime 的定时器 goroutine 上：不得触碰 t 的任何字段，
		// 只允许投递回 loop。所有读写 t 的动作都发生在 onTimer 里。
		t.loop.Post(func() { t.onTimer(gen) })
	})
}

// ---- 执行与取消 -------------------------------------------------------------

// Run 同步执行当前回调（对应 C++ 的 Run()）。
func (t *TimerTask) Run() {
	if t.cb != nil {
		t.cb()
	}
}

func (t *TimerTask) Cancel() {
	if t.timer != nil {
		// Stop 返回 false 说明回调已经派发出去了；文档明确 Stop 不等待 f
		// 结束。再叠加"任务可能已排在 loop 队列里"，取消后仍会到达 onTimer
		// 的窗口是必然存在的 —— 所以下面必须自增 generation，
		// 和 C++ 版对 muduo cancelingTimers_ 的处理是同一个道理。
		t.timer.Stop()
		t.timer = nil
	}
	t.cb = nil
	t.deadline = time.Time{}
	t.repeating = false
	t.interval = 0
	t.generation++
}

// ---- 查询 -------------------------------------------------------------------

func (t *TimerTask) IsActive() bool {
	return t.timer != nil && time.Now().Before(t.deadline)
}

// Deadline 返回下次触发时刻，未武装时为零值。
// 不像 C++ 版要从 muduo 的 Timer* 里读 expiration()（那个指针会悬垂，
// 对应源文件里那一大段注释）——这里自己记，问题从根上不存在。
func (t *TimerTask) Deadline() time.Time { return t.deadline }

// EndTime 对应 C++ 的 GetEndTime()：epoch 秒，未武装时为 0。
func (t *TimerTask) EndTime() int64 {
	if t.deadline.IsZero() {
		return 0
	}
	return t.deadline.Unix()
}

func (t *TimerTask) SetCallback(cb func()) { t.cb = cb }

// ---- 内部 -------------------------------------------------------------------

func (t *TimerTask) onTimer(firedGeneration uint64) {
	// 已被取消或已被重新 arm 的旧触发在这里被拦下。
	//
	// C++ 版拦的是 use-after-free：实体 1 和 3 落在同一批到期里，1 的回调
	// 重新 arm 了 3，此时 3 已持有新 TimerId，旧触发若在此清空 timerId，
	// 新定时器就失去了唯一句柄，会活得比属主久。
	//
	// Go 没有 use-after-free，但逻辑错误一模一样：旧触发会跑一次本该被
	// 取消的回调，或者把新 arm 的状态覆盖掉。守卫照留。
	if firedGeneration != t.generation {
		return
	}

	now := time.Now()

	if !t.repeating {
		t.timer = nil
		t.deadline = time.Time{}
	}

	// 先拷贝：回调可能重新调度或取消本定时器，从而覆写 t.cb。
	cb := t.cb
	if cb != nil {
		cb()
	}

	// 仅当回调没有取消、也没有重新调度我们时才续期。
	// 语义对齐 muduo 的 runEvery：从本次触发时刻起算下一次，不是从回调
	// 结束起算，因此回调耗时不会累积漂移到周期上。
	if t.repeating && firedGeneration == t.generation && t.timer != nil {
		t.deadline = now.Add(t.interval)
		t.timer.Reset(time.Until(t.deadline))
	}
}
