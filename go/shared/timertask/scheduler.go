package timertask

import (
	"container/heap"
	"time"
)

// noCopy 让 go vet 的 copylocks 检查拦下值拷贝。
//
// 对照 C++：TimerTaskComp 的拷贝构造（刻意拷成空对象）与移动构造（取消源），
// 二者都是为了防止"两个对象共享同一个捕获了 this 的回调"。Go 没有移动语义，
// 等价手段就是根本不许值拷贝，一律传 *Task。
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// Scheduler 复刻 muduo TimerQueue 的**行为结构**：整个 loop 只持有一个
// runtime 定时器（对应 muduo 的单个 timerfd），所有任务按到期时间有序组织，
// 唤醒后成批摘出、先拷贝再逐个执行，周期任务在批末续期。
//
// 对照 C++：muduo_windows/src/muduo/net/TimerQueue.{h,cc}  TimerQueue
//
// 容器不同，别混为一谈（这是刻意差异，合并上游时不要"改回去"）：
// muduo 用 std::set<pair<Timestamp, Timer*>>，即红黑树（TimerQueue.h 的
// TimerList），不是堆；它另配一个 activeTimers_ 集合供 cancel 反查 Timer*。
// Go 标准库没有有序容器，这里用 container/heap 的二叉堆，并在 Task 里回写
// 堆内下标 —— 于是 Cancel 可以直接 heap.Remove，同样 O(log n)，
// **还省掉了 muduo 那个 activeTimers_**（本包没有它的对应物）。
// muduo 之所以不用 std::priority_queue，正是因为它无法从中间摘除。
//
// 相对"每个任务一个 time.AfterFunc"的区别：
//   - **全程零 goroutine 派生**。唤醒定时器用 time.NewTimer，它到期只是往
//     channel 送一个值；Loop.Run 直接 select 这个 channel。而 time.AfterFunc
//     每次到期都会 `go f()` 派生一个（见 src/time/sleep.go 的 goFunc）——
//     挂着不动时不占 goroutine，但同一刻大批到期就是一场 goroutine 风暴。
//   - 武装/取消是纯堆操作，不碰 runtime 的定时器堆，也不分配闭包。
//
// 所有方法都必须在 Loop.Run 所在 goroutine 上调用。
type Scheduler struct {
	loop *Loop
	h    taskHeap
	wake *time.Timer
	// wake 当前设定的到期时刻；零值表示未武装。用于避免无谓的 Reset。
	wakeAt time.Time
	// onWake 的批次缓冲，复用以避免每次唤醒都分配。
	batch []firing
}

// firing 记录"入批那一刻"的世代。必须在摘出时抓取：等到执行时再读
// t.generation，读到的已经是被同批前序回调改过的值，守卫就形同虚设。
//
// 对照 C++：这一层对应上游的**两处**，合并时两处都要看 ——
//   - timer_task_comp.cpp  TimerTaskComp::OnTimer() 的 firedGeneration 判断
//     （以及 TimerTaskComp::Cancel() 里的 ++generation）
//   - muduo_windows/src/muduo/net/TimerQueue.cc  TimerQueue::handleRead()
//     里 cancelingTimers_.contains(...) 那段 —— **本仓自己打的补丁**，
//     third_party/muduo 与 third_party/muduo-linux 都没有。
//
// 两者功能重叠（都在拦"同批内已被取消/重排的陈旧触发"），Go 侧只实现
// generation 一层，不再复刻 cancelingTimers_。
type firing struct {
	task *Task
	gen  uint64
}

// newScheduler 由 NewLoop 调用；每个 Loop 恒定持有一个。
func newScheduler(loop *Loop) *Scheduler {
	s := &Scheduler{loop: loop}

	// 整个 loop 唯一的 runtime 定时器，对应 muduo 的单个 timerfd。
	// 先 Stop 置于未武装态 —— Go 1.23 起 Stop/Reset 保证调用后不会再从
	// channel 收到陈旧值，所以不需要老写法里那套"Stop 后手动 drain channel"
	// 的模板代码。
	s.wake = time.NewTimer(time.Hour)
	s.wake.Stop()
	return s
}

// Task 是本包的主类型，逐方法对应 C++ 的 TimerTaskComp。
//
// 对照 C++：cpp/libs/engine/core/time/comp/timer_task_comp.{h,cpp}  TimerTaskComp
//
// 刻意差异（详见 PORTING.md，合并上游时不要"补回来"）：
//   - **没有析构函数**。C++ 的 ~TimerTaskComp() 自动 Cancel；Go 没有对应物，
//     实体销毁 / 组件移除路径必须显式调用 Cancel()。这是移植后最大的风险点。
//   - 没有拷贝/移动构造。C++ 用"拷贝出空对象 + 移动时取消源"来防止两个对象
//     共享同一个捕获了 this 的回调；Go 无移动语义，改为 noCopy 禁止值拷贝，
//     一律用 *Task。
//   - IsActive/EndTime 读本结构体自存的 deadline，不去问底层定时器。
//     C++ 那边要读 timerId.GetTimer()->expiration()，所以才有 OwningLoopAlive()
//     那一大段防悬垂指针的注释；Go 无此问题，那段没有对应物。
type Task struct {
	_ noCopy

	sched     *Scheduler
	cb        func()
	deadline  time.Time
	interval  time.Duration
	repeating bool

	// 堆内下标；-1 表示不在堆里。维护它才能让 Cancel 做到 O(log n) 摘除，
	// 而不是留墓碑等它自然到期。
	index int

	// 与 TimerTask 中同名字段作用完全一致：拦下"同一批到期中，前一个回调
	// 取消 / 重新调度了后一个任务"造成的陈旧触发。批次是先从堆里拷出来再
	// 逐个执行的（muduo handleRead 也是这么做的），所以这个窗口必然存在。
	generation uint64
}

func (s *Scheduler) NewTask() *Task {
	return &Task{sched: s, index: -1}
}

// ---- 调度 -------------------------------------------------------------------
// 对照 C++：TimerTaskComp::RunAt / RunAfter / RunEvery，三者同样都走一个
// 共用的 ScheduleTimer()，且同样"先 Cancel 再武装"。

func (t *Task) RunAt(at time.Time, cb func()) {
	t.schedule(time.Until(at), false, cb)
}

func (t *Task) RunAfter(d time.Duration, cb func()) {
	t.schedule(d, false, cb)
}

func (t *Task) RunEvery(interval time.Duration, cb func()) {
	t.schedule(interval, true, cb)
}

// 对照 C++：TimerTaskComp::ScheduleTimer()
func (t *Task) schedule(d time.Duration, repeating bool, cb func()) {
	t.Cancel() // 同时自增 generation，作废任何已入批的陈旧触发

	if cb == nil || t.sched == nil {
		return
	}
	if repeating && d <= 0 {
		return
	}

	t.cb = cb
	t.repeating = repeating
	t.interval = d
	t.deadline = time.Now().Add(d)

	s := t.sched
	heap.Push(&s.h, t)
	s.resetWake()
}

// Cancel 对照 C++：TimerTaskComp::Cancel() + TimerQueue::cancelInLoop()。
// 两边职责在这里合并了：C++ 是组件调 loop->cancel() 再由 TimerQueue 从
// timers_/activeTimers_ 里摘除，Go 侧直接 heap.Remove。
func (t *Task) Cancel() {
	if t.index >= 0 {
		heap.Remove(&t.sched.h, t.index)
		t.index = -1
	}
	t.cb = nil
	t.deadline = time.Time{}
	t.repeating = false
	t.interval = 0
	t.generation++
	// 刻意不在这里 resetWake：唤醒早到只是空跑一次 onWake，
	// 而每次取消都去 Reset runtime 定时器反而更贵。
	// 与 muduo 一致 —— cancelInLoop() 同样不调 resetTimerfd()。
}

// Run 对照 C++：TimerTaskComp::Run()，同步执行当前回调。
func (t *Task) Run() {
	if t.cb != nil {
		t.cb()
	}
}

// 对照 C++：TimerTaskComp::IsActive / GetEndTime / SetCallBack。
// EndTime 取 epoch 秒并在未武装时返回 0，与 C++ 侧保持一致；Deadline 是
// Go 侧新增的、无损的形式，新代码优先用它。
func (t *Task) IsActive() bool        { return t.index >= 0 && time.Now().Before(t.deadline) }
func (t *Task) Deadline() time.Time   { return t.deadline }
func (t *Task) SetCallback(cb func()) { t.cb = cb }
func (t *Task) EndTime() int64 {
	if t.deadline.IsZero() {
		return 0
	}
	return t.deadline.Unix()
}

// ---- 唤醒与派发 -------------------------------------------------------------

// resetWake 对照 C++：TimerQueue::insert() 里的 earliestChanged 判断 +
// resetTimerfd()。只有"最早到期时刻提前了"才去动那个唯一的定时器。
func (s *Scheduler) resetWake() {
	if len(s.h) == 0 {
		s.wake.Stop()
		s.wakeAt = time.Time{}
		return
	}

	earliest := s.h[0].deadline
	if !s.wakeAt.IsZero() && !earliest.Before(s.wakeAt) {
		return // 已有的唤醒足够早，不必动 runtime 定时器
	}
	s.wakeAt = earliest

	d := time.Until(earliest)
	if d < 0 {
		d = 0
	}
	// 唤醒早到是无害的：onWake 发现没有到期任务，就只是重新武装一次。
	s.wake.Reset(d)
}

// onWake 是本包与上游对应最密的一处，合并时**优先看这里**。
//
// 对照 C++：TimerQueue::handleRead() —— 三段一一对应：
//
//	getExpired(now)      → 下面的"整批摘出"循环
//	for(...) it->run()   → 下面的"逐个执行"循环
//	reset(expired, now)  → 循环末尾的周期任务续期 + resetWake()
//
// 语义上刻意对齐的一点：muduo 的 reset() 用的是 handleRead() 入口处取的
// now（Timer::restart(now)），不是回调跑完之后的时刻；这里同样用入口的
// now.Add(interval)。因此回调耗时不会累积漂移到周期上。
func (s *Scheduler) onWake() {
	s.wakeAt = time.Time{}
	now := time.Now()

	// 先整批摘出再执行 —— 对应 TimerQueue::getExpired()，那边是
	// lower_bound(sentry) 一刀切 + std::copy 到 vector。也正因为"先拷再跑"，
	// 批内前一个回调取消/重排后一个任务时，后者的陈旧触发才需要 generation
	// 拦。世代必须在这里抓，不能等到执行时再读（见 firing 的注释）。
	s.batch = s.batch[:0]
	for len(s.h) > 0 && !s.h[0].deadline.After(now) {
		t := heap.Pop(&s.h).(*Task)
		t.index = -1
		s.batch = append(s.batch, firing{task: t, gen: t.generation})
	}

	for _, f := range s.batch {
		t := f.task
		// 已被同批前序回调取消或重新调度：这次触发是陈旧的，丢弃。
		// 对应 muduo_windows fork 在 handleRead 里加的 cancelingTimers_ 判断。
		if t.generation != f.gen {
			continue
		}

		cb := t.cb
		if !t.repeating {
			t.cb = nil
			t.deadline = time.Time{}
		}
		if cb != nil {
			cb()
		}
		// 回调可能已经取消（generation 变了）或重新调度（index >= 0）本任务
		if t.repeating && t.generation == f.gen && t.index < 0 {
			t.deadline = now.Add(t.interval)
			heap.Push(&s.h, t)
		}
	}
	clear(s.batch) // 不留悬挂的 *Task 引用妨碍 GC

	s.resetWake()
}

// Len 返回当前挂起的任务数，仅用于观测/测试。
func (s *Scheduler) Len() int { return len(s.h) }

// ---- 最小堆 -----------------------------------------------------------------
// 对照 C++：TimerQueue::TimerList（std::set<pair<Timestamp, Timer*>>）。
// 上游那边还有个 ActiveTimerSet activeTimers_ 专供 cancel 反查，本包**没有
// 对应物** —— Swap 里回写 index 已经让 Cancel 能 O(log n) 直接摘除。
// 若上游动了 activeTimers_ 相关逻辑，先判断那是不是只服务于反查；是的话
// Go 侧无需跟进。

type taskHeap []*Task

func (h taskHeap) Len() int           { return len(h) }
func (h taskHeap) Less(i, j int) bool { return h[i].deadline.Before(h[j].deadline) }
func (h taskHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *taskHeap) Push(x any)        { t := x.(*Task); t.index = len(*h); *h = append(*h, t) }
func (h *taskHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return t
}
