# timertask —— 与 C++ 定时器组件的对照与合并说明

本包是 C++ 端定时器组件的**行为移植**，不是独立设计。上游修 bug 时必须同步过来。
本文件是合并时的唯一依据：**先查「刻意差异」表，再动手改。**

## 上游是哪一份

```
cpp/libs/engine/core/time/comp/timer_task_comp.h        TimerTaskComp
cpp/libs/engine/core/time/comp/timer_task_comp.cpp
cpp/libs/engine/muduo_windows/src/muduo/net/TimerQueue.h    TimerQueue
cpp/libs/engine/muduo_windows/src/muduo/net/TimerQueue.cc
cpp/tests/timer_queue_unit_test/timer_queue_unit_test.cpp   （行为基准）
cpp/tests/timer_destroy_test/timer_destroy_test.cpp         （行为基准）
```

**muduo 取 `muduo_windows` 那一份，不是 `third_party/muduo`，也不是
`third_party/muduo-linux`。** 只有 `muduo_windows` 带着本仓自己的补丁：
`TimerQueue::handleRead()` 里那段 `cancelingTimers_.contains(...)` 判断，
上游原版没有。（截至移植时，CMake / Linux 构建引的是无补丁的 `muduo-linux`，
所以「同批内取消」在 Linux 上全靠 `TimerTaskComp::generation` 兜着 —— 这是
C++ 侧的事，与本包无关，但说明 generation 那一层是承重的，不是冗余。）

代码里的锚点统一写成 `对照 C++：<路径> <符号>`。**只认符号名，不写行号**，
行号会漂。

## 结构对照

| Go | C++ | 备注 |
|---|---|---|
| `Loop` | `muduo::net::EventLoop` | Go 无对应设施，必须自带 |
| `Loop.Run` | `EventLoop::loop()` | epoll_wait 等 fd+timerfd ⟷ select 等 tasks+wake.C |
| `Loop.Post` | `EventLoop::runInLoop/queueInLoop` | 返回 false ⟷ `GetThreadEventLoop()==nullptr` 分支 |
| `Scheduler` | `TimerQueue` | 一个 loop 一个，恒定持有 |
| `Scheduler.wake` | `TimerQueue::timerfd_` + `timerfdChannel_` | 全 loop 唯一的定时器 |
| `Scheduler.h` (taskHeap) | `TimerQueue::timers_` (`TimerList`) | 二叉堆 ⟷ 红黑树，见下 |
| `Scheduler.onWake` | `TimerQueue::handleRead()` | **对应最密，优先看这里** |
| ├ 整批摘出循环 | `TimerQueue::getExpired()` | |
| ├ 逐个执行循环 | `handleRead()` 里的 `it.second->run()` | |
| └ 续期 + `resetWake()` | `TimerQueue::reset()` | |
| `Scheduler.resetWake` | `insert()` 的 `earliestChanged` + `resetTimerfd()` | |
| `Task` | `TimerTaskComp` | 逐方法对应 |
| `Task.RunAt/RunAfter/RunEvery` | 同名方法 | 同样都走共用的 schedule/`ScheduleTimer` |
| `Task.schedule` | `TimerTaskComp::ScheduleTimer()` | 同样"先 Cancel 再武装" |
| `Task.Cancel` | `TimerTaskComp::Cancel()` + `TimerQueue::cancelInLoop()` | 两边职责在 Go 侧合并 |
| `Task.generation` / `firing.gen` | `TimerTaskComp::generation` + `cancelingTimers_` | 两处合成一处，见下 |
| `Task.IsActive/EndTime/SetCallback/Run` | 同名方法 | |
| `noCopy` | 拷贝构造 + 移动构造 | 见下 |

## 刻意差异 —— 合并时**不要**改回去

这些不是漏掉的，是 Go 语言/生态与 C++ 不同导致的结构性选择。

1. **没有析构函数。** C++ 的 `~TimerTaskComp()` 自动 `Cancel()`；Go 没有对应物。
   实体销毁 / 组件移除路径**必须显式调用 `Cancel()`**。
   → 这是移植后最大的风险点，对应 C++ 侧 `registration_manager.cpp` 里
   `registry.remove<TimerTaskComp>(entity)` 那几处，Go 侧要有等价的显式清理。
   上游若新增了"某某路径也要移除组件"，Go 侧同样要加显式 Cancel。

2. **没有拷贝/移动构造，改为 `noCopy` 禁止值拷贝。** C++ 用"拷贝出空对象 +
   移动时取消源"防止两个对象共享同一个捕获了 `this` 的回调；Go 无移动语义，
   等价手段是一律用 `*Task`，由 `go vet` 的 copylocks 拦截。

3. **容器：二叉堆，不是红黑树。** Go 标准库没有有序容器。用 `container/heap`
   并在 `Task.index` 回写堆内下标，于是 `Cancel` 能 `heap.Remove` 做到 O(log n)。
   **因此本包没有 `activeTimers_` 的对应物** —— 上游那个集合的唯一职责是按
   `Timer*` 反查，本包用下标解决了。上游若改动 `activeTimers_` 相关逻辑，
   先判断那是不是只服务于反查；是的话 Go 侧无需跟进。

4. **陈旧触发只保留 `generation` 一层。** 上游有两层：`TimerTaskComp::generation`
   和 `muduo_windows` 补丁里的 `cancelingTimers_`，二者功能重叠（都在拦"同批内
   已被取消/重排的陈旧触发"）。Go 侧只实现 generation。
   **关键实现细节**：世代必须在**入批那一刻**快照进 `firing{task, gen}`，
   不能等执行时再读 `t.generation` —— 那时它已经被同批前序回调改过，守卫形同虚设。
   （移植时第一版就写错在这里，被 `TestSched_StaleFiringInSameBatchIsDropped` 抓出。）

5. **`IsActive`/`EndTime` 读自存的 `deadline`，不问底层定时器。** C++ 那边要读
   `timerId.GetTimer()->expiration()`，所以才有 `OwningLoopAlive()` 上方那一大段
   防悬垂指针的注释。Go 无此问题，**那段注释在本包没有对应物**，上游若继续加固
   那条路径，Go 侧不需要跟。

6. **`Loop.Post` 有界会阻塞（背压），且没有 `TryPost`。** 无界队列只会把积压问题
   推迟成 OOM。若某条链路宁可丢弃也不能阻塞，届时再加 `TryPost`——丢弃与否是
   业务决定，不由本包替业务定。

7. **`afterfunc_baseline_test.go` 不参与生产构建，也不需要跟上游同步。** 它是给
   基准做对照的第二实现（每任务一个 `time.AfterFunc`），存在的唯一理由是量化
   证明生产实现为什么值得那点复杂度。

## 刻意对齐 —— 改动时**必须**保持一致

1. **周期任务的续期时刻**取 `onWake()` 入口处的 `now`，不是回调跑完之后的时刻。
   对应 muduo `reset(expired, now)` 里的 `Timer::restart(now)`，那个 `now` 同样
   来自 `handleRead()` 入口。因此回调耗时不会累积漂移到周期上。

2. **先整批摘出、再逐个执行**（不是边摘边跑）。对应 `getExpired()` 先 `std::copy`
   到 vector 再执行。改成边摘边跑会改变同批内的可见性语义，`generation` 那层的
   前提也就没了。

3. **`Cancel` 不触发 `resetWake`。** 唤醒早到只是空跑一次 `onWake`，比每次取消
   都去 `Reset` 定时器便宜。与 muduo 一致 —— `cancelInLoop()` 同样不调
   `resetTimerfd()`。

4. **线程契约**：除 `Loop.Post` / `Loop.Stop` 外，`Task`/`Scheduler` 的所有方法
   只能在 `Loop.Run` 所在 goroutine 上调用。对应 muduo 每个入口的
   `loop_->assertInLoopThread()`。满足这条才不需要任何锁 —— 同步由 channel 的
   happens-before 提供。**想跨 goroutine 取消，走 `loop.Post(func(){ t.Cancel() })`，
   不要加锁**，加锁会破坏"回调执行期间状态不被并发修改"这个前提。

## 上游改了怎么办

1. 看改动落在哪个符号上，用上表反查 Go 侧位置（代码里搜 `对照 C++：`）。
2. 先对「刻意差异」表：命中其中任一条 → 大概率**不需要跟**，在 PR 里写明理由。
3. 需要跟的，**先加测试再改**。行为基准在 `cpp/tests/timer_queue_unit_test/` 和
   `cpp/tests/timer_destroy_test/`，Go 侧对应的场景测试在 `scheduler_test.go`
   （`TestSched_StaleFiringInSameBatchIsDropped` / `TestSched_CancelInSameBatchIsHonoured`
   / `TestSched_OrderingAndRemoval`）。上游的新用例应当先在 Go 侧复现为失败测试。
4. 跑 `go vet ./... && go test -race -count=5 ./...`。

## 已知缺口

- **`-race` 尚未跑过**（移植环境无 gcc，`CGO_ENABLED=1` 起不来）。线程契约是本包
  的地基，接入前必须补跑。
- **线程契约无断言**。muduo 有 `assertInLoopThread()`，本包只有文档约定。Go 没有
  官方 goroutine ID；可加一个 debug 断言（loop 执行任务期间置 atomic 标志，
  `Task` 方法入口断言其为真）能抓住多数越线调用。尚未实现。
- **唤醒延迟只在 Windows 测过**：5ms 定时器超出量 p50=352µs / p99=927µs / max=1.02ms，
  受 Windows 系统定时器粒度支配。若对精度有要求，须在 Linux 重测。
- **`Loop.Stop` 后队列内剩余任务直接丢弃**，没有 drain 策略。关服若有必须落地的
  定时任务，需另行处理。
