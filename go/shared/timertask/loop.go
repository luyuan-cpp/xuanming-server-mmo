// Package timertask 是 C++ 端定时器组件的 Go 移植。
//
// # 上游（fork 源）
//
// 本包是下列 C++ 代码的行为移植，**上游发生修复时必须同步过来**：
//
//	cpp/libs/engine/core/time/comp/timer_task_comp.h    TimerTaskComp
//	cpp/libs/engine/core/time/comp/timer_task_comp.cpp  同上
//	cpp/libs/engine/muduo_windows/src/muduo/net/TimerQueue.h   TimerQueue
//	cpp/libs/engine/muduo_windows/src/muduo/net/TimerQueue.cc  同上
//
// 注意上游取 muduo_windows 那一份，不是 third_party/muduo 或
// third_party/muduo-linux —— 只有 muduo_windows 带着本仓自己的补丁
// （TimerQueue::handleRead() 里的 cancelingTimers_ 判断，上游原版没有）。
//
// 对照表、刻意保留的差异、以及"上游改了怎么合"的流程，见同目录 PORTING.md。
// 代码里的锚点一律写成 `对照 C++：<路径> <符号>`，改动时请一并维护。
// 行号会漂，锚点只认符号名。
//
// # 本文件
//
// C++ 版把 muduo::net::EventLoop 当作既有设施；Go 里没有对应物，所以必须
// 自带。Loop 就是 muduo EventLoop 在"定时器派发"这一面的最小等价物：
// 一个独立 goroutine，独占所有回调会碰到的状态。
//
// 对照 C++：muduo_windows/src/muduo/net/EventLoop.{h,cc}  muduo::net::EventLoop
package timertask

import "sync"

// Loop 是单 goroutine 执行器。所有 TimerTask 的方法、以及所有定时器回调，
// 都只在 Run() 所在的那个 goroutine 上执行 —— 这正是 C++ 版"回调在 loop
// 线程跑，所以可以裸访问 entt registry"这一前提的复刻。
//
// 与 muduo 一致，Loop 恒定持有一个 Scheduler（对应 EventLoop 的成员
// timerQueue_），其唤醒 channel 是 Run 的 select 里的一个 case —— 对应
// muduo 把 timerfd_ 经 timerfdChannel_ 挂进 epoll（TimerQueue.h 的
// timerfd_ / timerfdChannel_ 两个成员）。
type Loop struct {
	tasks    chan func()
	done     chan struct{}
	stopOnce sync.Once
	sched    *Scheduler
}

func NewLoop(queueSize int) *Loop {
	l := &Loop{
		tasks: make(chan func(), queueSize),
		done:  make(chan struct{}),
	}
	l.sched = newScheduler(l)
	return l
}

// Sched 返回本 loop 的定时器调度器。只能在 Run 所在 goroutine 上使用。
func (l *Loop) Sched() *Scheduler { return l.sched }

// Post 把 f 排入 loop，可从任意 goroutine 调用。
// 返回 false 表示 loop 已停，任务被丢弃。
//
// 对照 C++：EventLoop::runInLoop() / queueInLoop()。
// "loop 已停就丢弃"对应 timer_task_comp.cpp 里每处都要判的
// GetThreadEventLoop() == nullptr（见该文件 OwningLoopAlive() 上方那段
// 关于 ~EventLoop 把 t_loopInThisThread 置 NULL 的注释）。
//
// tasks 有界，队列满时 Post 会阻塞发起方。这是刻意的背压：无界队列在积压时
// 只会把问题推迟到 OOM。若某条调用链宁可丢弃也不能阻塞，需要另加 TryPost
// （非阻塞 select + default），本包刻意没有提供，因为丢弃与否是业务决定。
//
// 注意：Scheduler 的定时器唤醒**不走**这个队列（它是 Run 的一个 select
// case），所以 tasks 只承载外部投递，定时器风暴不会把它顶满。
func (l *Loop) Post(f func()) bool {
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.tasks <- f:
		return true
	case <-l.done:
		return false
	}
}

// Run 在调用方 goroutine 上排空任务队列并派发到期定时器，直到 Stop。
//
// 对照 C++：EventLoop::loop()。那边是 epoll_wait 同时等 fd 事件与 timerfd，
// 这边是 select 同时等 tasks 与 wake.C —— 结构相同，wake.C 就是 timerfd。
//
// 单线程约束与 muduo 完全一致：任何一个回调里干重活或做阻塞调用，都会拖住
// 全部定时器和全部排队任务。Go 里这条比 C++ 更容易踩，因为阻塞调用
// （DB、文件、同步 gRPC）长得和普通函数没区别。
func (l *Loop) Run() {
	for {
		select {
		case <-l.done:
			return
		case f := <-l.tasks:
			f()
		case <-l.sched.wake.C:
			l.sched.onWake()
		}
	}
}

// Stop 幂等。刻意不 close(tasks)：生产者是定时器 goroutine，
// close 会让它们 panic。
func (l *Loop) Stop() {
	l.stopOnce.Do(func() { close(l.done) })
}
