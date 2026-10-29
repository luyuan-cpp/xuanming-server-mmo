# muduo 定时器取消语义与野引用

## Windows 适配的现役接口边界（2026-10-20）

- Windows 移植来源为负责人维护的 `luyuan-cpp/muduo-windows`，本次同步起点 master 为
  `66de677f95f65d5f09076d485d6a3b2b150e779c`，接口收口与必要正确性修复同步到独立库；本仓编译源码在
  `cpp/libs/engine/muduo_windows/src`，使用方现有头文件镜像在 `third_party/muduo`。
  修改公开类时必须同步镜像并重编译库与使用方，不能混用旧布局的库和新头。
- `Condition` 保留原版 `Condition(MutexLock&)`、`wait()`、`waitForSeconds(double)`、
  `notify()` / `notifyAll()`。Windows 内部用 `condition_variable_any` 临时释放并重新获取构造时的锁；
  外层守卫继续拥有同一把锁。小数秒不截断，只有超时返回 true，单个通知与广播分别使用 notify_one / notify_all。
- `Thread` 保留原版启动同步、tid、默认名、异常处理和 started 语义；Windows 内部用 std::thread，
  join 成功返回 0，未 join 的析构 detach。工作线程独立拥有回调和名称，启动同步后不再访问 Thread 对象；
  回调捕获对象的寿命仍由调用者保证。需要等待完成的调用者必须显式 join。
- `Atomic` 标准库实现保留 getAndAdd 的旧值、addAndGet / incrementAndGet / decrementAndGet 的新值语义；
  getAndSet 必须是单次 exchange。否则恢复原版 Thread 默认命名时会出现 Thread0 与创建数 1 不一致，
  Timer 的序号也偏离原契约。新增固定值回归直接守住这些返回值，不调整 Thread 命名预期掩盖基础差异。
- `Timestamp` 恢复原版 Boost 比较基类，六种关系在两个平台具有相同契约；不再删除 Windows 的部分比较能力。
  MSVC 仅增加 `empty_bases` 属性，保持八字节值布局及原对齐。固定整数边界回归验证全部关系、大小和对齐，
  不依赖真实时钟；主仓现役头、使用方镜像和独立库头同步。
- Windows UDP 唤醒的创建、发送、排空集中在内部 `muduo/net/WindowsWakeup.{h,cc}`；
  EventLoop 只保留平台调用，不增加公开接口或成员。保留自连回环、非阻塞、不可继承、通知合并和排空到 EWOULDBLOCK 的语义。
- 单调时钟、动态 poll 超时、同批取消守卫、连接失败回调、Connector 生命周期及 HUP 处理是独立的正确性修复，
  本次接口收口不撤销。Linux 继续使用固定上游及既有 overlay，不套用 Windows 实现。
- 2026-10-22 复核发现独立库尚未同步下文三处闭包清理重入修复，已补齐双索引摘除、析构清理与取消窗口顺序。
  独立库另删除全树无调用的旧唤醒、HUP 返回及 poller 选择兼容入口，Windows 唤醒只由 WindowsWakeup 承担。
  Condition、Thread、Mutex 及现有连接修复继续保留，不为减少差异行数新增透传层或改回不兼容语义。

下文“只能在 muduo 里拦、组件层无能为力”描述的是裸 this 回调的旧方案，不能推广到当前全部调用者。
现役 TimerTaskComp 的闭包先锁独立 weak State，再核对 generation，已能在外层阻止失效的业务回调；
裸 muduo 定时器的同批取消契约和回归测试仍存在，因此底层守卫继续保留。

## 2026-10-20 修复范围与取消契约

这里有两项独立修复：**取消是否生效**，以及**定时器用哪只钟计时**。Hiredis 的 socket 生命周期是第三个接缝，详见
[Redis 生命周期说明](redis-async-client-retry-fix.md)。修改钟源不会修复释放后使用；修复定时器取消也不会自动保护 socket 回调。

以下“原版”“只能在 muduo 里拦”和旧平台对照描述历史缺陷；现役组件的独立 weak State 能在宿主销毁后安全拒绝回调，
底层仍必须履行裸定时器的取消契约。Windows 实现和 Linux overlay 都保留回调执行前的取消检查。

### 同批取消的完整顺序

1. `getExpired(now)` 把 A、B 整批移出 `timers_` 和 `activeTimers_`，放进局部 `expired`。
2. A 先执行，销毁 B 的宿主；宿主析构调用 `cancel(B)`。
3. `cancelInLoop` 在活动队列找不到 B，但本轮仍在处理到期批次，因此把 `(Timer*, sequence)` 记进 `cancelingTimers_`。
4. 遍历到 B 时先查取消记录，已取消就跳过 `run()`。
5. `reset(expired, now)` 统一销毁一次性或已取消的 Timer；未取消的重复 Timer 才重新入队。

这完全可以发生在一个线程上。若闭包捕获裸 `this`，不检查第 4 步就会调用已释放对象；崩溃不是必然表现，
也可能访问被其他对象重新占用的内存。跳过回调不等于提前释放 Timer，释放仍由第 5 步完成，因此不会泄漏。
身份用指针和递增序号共同判断，旧 `TimerId` 即使碰上地址复用，也不能取消新 Timer。

### 清理闭包时也必须接受取消

仅保护 `run()` 循环还不完整。`reset()` 中 `delete Timer` 会析构其 `std::function`，闭包捕获的最后一个
`shared_ptr<Owner>` 也可能在这里释放，Owner 的析构再调用 `cancel(B)`。如果这时就已把
`callingExpiredTimers_` 置 false，且 B 仍在本批待清理列表里，活动队列找不到 B，取消会丢失，重复 B 随后重新入队。

因此“本批仍在处理”的范围必须覆盖回调和统一清理两个阶段：

```cpp
callingExpiredTimers_ = true;
cancelingTimers_.clear();
for (const Entry& entry : expired) {
    if (cancelingTimers_.contains(ActiveTimer{entry.second, entry.second->sequence()})) {
        continue;
    }
    entry.second->run();
}
reset(expired, now);                // 闭包析构也可以重入 cancel()
callingExpiredTimers_ = false;
```

此处不增加对象布局、定时等待或新调度队列。已开始的回调不会被中断；在本轮清理时取消重复 Timer，只能阻止后续轮次。

同样的重入还存在于主动取消和整个队列析构：

- 旧 `cancelInLoop` 先从 `timers_` 摘除、接着 delete、最后才从 `activeTimers_` 摘除。
  delete 闭包时若再取消自己，会再次找到同一个待释放指针；若取消另一个 Timer，会看到两集合大小不一致。
  现役实现先保存指针、从两个索引都摘除，再 delete，任何析构重入看到的都是一致状态。
- 旧 `~TimerQueue` 遍历成员集合时直接 delete。闭包析构中的 `cancel()` 可以修改正在遍历的集合，甚至自删两次。
  现役实现每次先从两个索引摘下一项，再销毁；其他 Timer 被重入取消时也会从剩余队列移除，不再被析构循环重复释放。
  EventLoop 析构期间重新创建 Timer 不在支持契约内。

跨线程 `cancel()` 经 `runInLoop` 投递，调用返回不是取消完成屏障，不能据此立即销毁另一个线程回调捕获的裸对象。
跨线程拥有者仍须通过 weak 生命周期或 loop 内串行销毁保证安全。

### 墙钟与单调时钟的区别

墙钟是日期时间，会被 NTP 或人工校时调整；单调时钟用于比较进程内经过的时长。以“10 秒后超时”为例：

| 事件 | 实际经过 | 墙钟 |
|---|---:|---|
| 挂定时器 | 0 秒 | 12:00:00 |
| 记录墙钟到期点 | — | 12:00:10 |
| 系统时间回拨 5 分钟 | 3 秒 | 11:55:03 |
| 实际已等够 10 秒 | 10 秒 | 11:55:10 |

若还用 `wallNow >= 12:00:10` 判断，到实际第 10 秒仍认为未到期，回调会被拖延约 5 分钟。墙钟前跳则可能提前到期。
Linux 即使由 `CLOCK_MONOTONIC` timerfd 唤醒，上层继续用墙钟判定、计算下次等待，仍会产生这个问题。

当前整条链统一为单调域：`runAfter/runEvery` 生成截止点，`getExpired` 判断到期，`Timer::restart` 生成重复截止，
Windows `nextTimeoutMs` 与 Linux timerfd 的相对上弦都使用同一钟源。`runAt(墙钟时刻)` 在调用时只换算一次：
`monoWhen = monoNow + (wallWhen - wallNow)`，以后等待这一段时长。墙钟用于日志与对外日期，不能拿单调读数格式化日历。
过期墙钟换算后的单调值允许小于等于零，队列非空不能用 `Timestamp::valid()` 代替。

系统校时不会改变已经安排的相对超时；loop 忙、操作系统调度与计时精度仍可使回调晚执行。
这不承诺实时调度，也不承诺休眠时间的跨平台计入方式。需要随日历调整的整点任务必须自行在触发时复核墙钟。

### 平台生效与验证入口

- Windows 编译实现：`cpp/libs/engine/muduo_windows/src/muduo/net/`；公开头镜像为 `third_party/muduo/muduo/net/`。
- Linux 必须经 `tools/archived/setup_dependencies.sh` 应用 `tools/archived/muduo_linux_overlay`；
  `TimerQueue.{h,cc}` 与 `EventLoop.cc` 一起覆盖。脚本把 overlay 哈希纳入依赖 stamp，变化后重建归档。
  单独编译未覆盖的 `third_party/muduo-linux` 或链接旧 SDK 归档，不能证明修复生效。
- Windows 回归源为 `cpp/tests/timer_queue_unit_test/timer_queue_unit_test.cpp`；Linux 的独立 Debug/ASan/UBSan 入口为
  `cpp/tests/redis_lifecycle_test/CMakeLists.txt` 中的 `timer_queue_lifecycle_test`，直接编译相同测试和当前 overlay。
- 同批取消、宿主销毁、重复/重入取消、清理闭包导致取消分别使用外置计数器观察，不靠解引用已释放对象判定。
  时钟换算、过去时刻、负单调值、`runAt/runAfter` 同域排序由 `TimerClockTest.*` 覆盖；不修改系统时间。

本次实际执行结果和未验证边界在文末登记；下方原有“未编译”标签是历史批次状态。

## 历史缺陷的一句话

`EventLoop::cancel()` **拦不住一个已经进入本轮到期批次的定时器**。如果取消的原因是"宿主对象要销毁了"，那么轮到它时回调会打进已释放内存。

## 根因

`TimerQueue::handleRead()` 的结构（上游原样）：

```cpp
std::vector<Entry> expired = getExpired(now);   // ① 先把整批拷出来

callingExpiredTimers_ = true;
cancelingTimers_.clear();
for (const Entry& it : expired)
{
  it.second->run();                             // ② 挨个执行
}
callingExpiredTimers_ = false;

reset(expired, now);                            // ③ 一次性/已取消的在这里 delete
```

① 之后这批定时器已经从 `activeTimers_` 里摘走了。此时若在 ② 的某个回调里
`cancel()` 掉同批次里靠后的另一个定时器，`cancelInLoop()` 会走到：

```cpp
ActiveTimerSet::iterator it = activeTimers_.find(timer);
if (it != activeTimers_.end()) { ... }          // 找不到
else if (callingExpiredTimers_)
{
  cancelingTimers_.insert(timer);               // 只是记一笔
}
```

它**只能记一笔**。上游对 `cancelingTimers_` 的唯一用途在 ③ ——阻止一个
**重复定时器**被重新插回队列。它完全不阻止 ② 里那次 `run()`。

## 崩溃是怎么发生的

1. 实体 A、B 的定时器落在同一批 `expired` 里，A 在前
2. A 的回调销毁了 B —— 移除组件、销毁实体、关闭会话，都算
3. B 的析构里 `Cancel()`，但如上所述拦不住
4. 循环走到 B 的定时器，`it.second->run()` 调用那个捕获了裸 `this` 的
   `std::function` —— **`this` 已经是已释放内存**

这就是当年在 Windows 上撞到的崩溃。

## 修法：只能在 muduo 里拦

`cpp/libs/engine/muduo_windows/src/muduo/net/TimerQueue.cc:193`（提交 `e137bd658`）：

```cpp
for (const Entry& it : expired)
{
    if (cancelingTimers_.contains(ActiveTimer{ it.second, it.second->sequence() }))
    {
        continue;                               // 本轮已被取消，别跑
    }
  it.second->run();
}
```

**必须在这一层。** 组件层的任何"我过期了就返回"式守卫都救不了这个场景：宿主对象
已经被释放，回调为了读那个守卫标志，本身就得先解引用已释放内存 —— UAF 已经发生了。
组件层的守卫只能覆盖"对象还活着但定时器被重设/取消"，覆盖不了"对象没了"。

## 各 muduo 树的现状

仓库里有四份 muduo，守卫只在其中一份：

| 树 | 有守卫 | 谁在用 |
|---|---|---|
| `cpp/libs/engine/muduo_windows/src` | ✅ | Windows 编译进 `muduo.lib` 的实现 |
| `third_party/muduo` | ❌ | Windows 的头文件来源 |
| `third_party/muduo-linux` | ❌ | **Linux 全部** |
| `third_party/grpc/third_party/...` | — | 无关 |

*（2026-10-17 批注：上表与下一句是写作时的现状，已过时。Linux 的守卫已于 2026-08-02 以
`tools/archived/muduo_linux_overlay/muduo/net/TimerQueue.cc` 的 LOCAL DELTA (1) 补上，由
`setup_dependencies.sh` 覆盖到 `third_party/muduo-linux`；`third_party/muduo` 只作头文件来源，守卫在 `.cc` 里，与它无关。）*

**所以 Linux 侧目前没有这个保护**，同一个崩溃在 Linux 节点上是敞开的。
`third_party/muduo-linux` 是子模块，只有指针会被提交，直接改它的工作区活不过一次
clone —— 要补必须走 `tools/archived/muduo_linux_overlay/`（见
`tools/archived/setup_dependencies.sh` 里的 overlay 段落）。

## 相关但不同的一个坑：同批次「重设」

同一批次里，前面的回调**重新调度**了后面那个实体的定时器（而不是销毁它）：

- 旧定时器仍会触发
- 此时宿主还活着，`timerId` 已经换成新的那个
- 若回调无条件把 `timerId` 清空，就会丢掉**新**定时器的唯一句柄
- 宿主销毁时 `Cancel()` 无从取消 → 新定时器活过了宿主 → 到点野引用

这一个组件层能修，也**应该**在组件层修（宿主还活着，读自己的成员是安全的）。
`TimerTaskComp` 用一个每次布防都递增的 `generation` 解决：调度时把当时的代际号绑进
回调，触发时对不上就直接返回。

两个坑要一起堵才完整：

| 场景 | 宿主状态 | 谁来修 |
|---|---|---|
| 同批次被取消 + 宿主销毁 | 已释放 | **muduo 守卫**（组件层无能为力） |
| 同批次被取消/重设，宿主还在 | 存活 | `TimerTaskComp::generation` |

## 另一处两端不一致：定时器精度差一个数量级

> 2026-10-17 订正:本节描述的是旧状态。Windows 的 `EventLoop::loop()` 早已改用 `TimerQueue::nextTimeoutMs` 计算 poll 超时(最近一个定时器的剩余时间,上限 100ms),
> 第 3 批又修好了 Windows 的 `wakeup()`,短定时器不再被 100ms 粒度拖住;定时器到期判断也已改为单调时钟(见上文「定时器时钟」一节)。下文保留作历史。

`cpp/libs/engine/muduo_windows/src/muduo/net/EventLoop.cc`：

```cpp
const int kPollTimeMs = 10000;   // 第 32 行，Linux
const int kPollTimeMs = 100;     // 第 36 行，Windows
```

```cpp
pollReturnTime_ = poller_->poll(kPollTimeMs, &activeChannels_);
...
#ifdef WIN32
	timerQueue_->loop();          // 每轮 poll 之后才主动查一次定时器
#endif
```

Linux 上 timerfd 是 poller 里的一个真实 fd，poll 精确在到期时刻返回，定时器是
**毫秒级**的。Windows 上没有 timerfd，定时器不由 poll 唤醒，而是每轮循环结束后
查一次，而每轮 poll 最多阻塞 100ms。

**结论：Windows 上任何短于 100ms 的定时器，实际粒度都是 ~100ms。**

实测（`timer_queue_unit_test`，2026-07-31）：

| 用例 | 行为 | 实测 |
|---|---|---|
| `RunEvery(0.01)` 触发 3 次 | 期望 ~30ms | **326ms**（≈108ms/次） |
| `RunAfter(0.01)` 自重设，共 2 次 | 期望 ~20ms | **215ms**（≈107ms/次） |

对战斗服的影响是实打实的：同一个 `RunEvery(0.01)` 的战斗 tick，Linux 10ms、
Windows ~108ms。技能吟唱 / 后摇 / 引导间隔全部受影响。在 Windows 上调出来的
手感，上 Linux 会完全不同；反过来，某些因为 Windows 定时器粗糙而没能暴露的
竞态，到 Linux 上定时器精确触发时才会现形。

**写测试时的直接后果**：不要用固定墙上时间窗口去卡触发次数。窗口若短于一个
poll 周期，在 Windows 上会拿到"定时器只触发了一次"的假象。改成在回调里数够次数
就 `loop.quit()`，另配一个明显偏大的 `runAfter` 兜底。本文件的两条重复/自重设
用例最初就栽在这上面。

## 定时器时钟：改为单调时钟（2026-10-16，未编译）

**问题。** 上游 muduo 的 `TimerQueue` 按墙钟到期：到期判断、上弦时长、重复定时器的下一次
到期全用 `Timestamp::now()`（Windows 上是 `gettimeofday` → `system_clock`）。Linux 的 timerfd
虽然建成 `CLOCK_MONOTONIC`，上弦时长仍按墙钟差算。墙钟回拨 N 秒（NTP 步进、人工改时间），
所有定时器——调用截止、建连、探测、业务看门狗——都晚 N 秒；前跳则提前。业务侧"到点按
steady_clock 复核、未到就重挂"只兜得住前跳，兜不住回拨（`grpc-muduo-transport.md` §6 登记过的已知限制）。

**做法（与 POSIX timerfd `CLOCK_MONOTONIC`、`asio::steady_timer` 同一口径）。**

- `TimerQueue::monotonicNow()`：`steady_clock` 的微秒读数，借 `Timestamp` 承载（复用 `addTime` /
  `operator<` 与 `Timer` 字段）。**它不是 epoch 时间**：原点约为开机时刻，只能与同一时钟的读数
  相减、比较，不能格式化成日期、不能与 `Timestamp::now()` 混算。
- 队列里 `Timer::expiration()` 一律在单调域；`handleRead` / `getExpired` / `reset` / `Timer::restart`、
  Windows 的 `nextTimeoutMs`、Linux 的 `howMuchTimeFromNow` 全部取单调 now。
- `EventLoop::runAfter` / `runEvery`：单调 now 加时长。
- `EventLoop::runAt(墙钟时刻)`：调用时一次性换算，`monoWhen = monoNow + (wallTime − wallNow)`
  （`TimerQueue::monotonicFromWall`，纯函数）。公开 API 不变；语义变为"等价于
  `runAfter(time − now)`"，之后墙钟再跳也不跟随。需要"墙钟到某一刻才触发"的整点类任务，
  到点后自己按墙钟复核。现在生产代码没有 `runAt` 调用方（只有 `TimerTaskComp::RunAt` 转发与测试）。
  两只表显式先读墙钟、后读单调钟（不靠实参求值顺序）：两次读数之间被抢占时，截止时刻只会偏晚、不会偏早。
- `reset()` 不再用 `nextExpire.valid()`（> 0）判断"还有没有下一个"：由很久以前的墙钟时刻换算来的
  单调值可以 ≤ 0，改为直接看队列是否为空。
- 墙钟只用于对外显示：`pollReturnTime()`、日志时间戳不变。TRACE 日志里 `TimerQueue::handleRead()`
  打印的 now 改标 `at monotonic`。

**各树落点。**

| 树 | 改动 |
|---|---|
| `cpp/libs/engine/muduo_windows/src` | `TimerQueue.{h,cc}`、`EventLoop.cc`；`EventLoop.h` / `Timer.h` 只加注释 |
| `third_party/muduo`（Windows 使用方 include 的头副本） | `TimerQueue.h` / `EventLoop.h` / `Timer.h` 与上一行逐字一致（`TimerQueue.h` 顺带补齐原先缺的 `nextTimeoutMs` 声明） |
| `tools/archived/muduo_linux_overlay` | `TimerQueue.cc`（LOCAL DELTA (2)）、新增 `TimerQueue.h` / `EventLoop.cc` 覆盖件；三者必须一起生效 |

类布局都不变（只加静态成员函数），`EventLoop.h` 公开 API 不变。

**业务侧的 steady 复核保留。** `muduo_grpc` 的截止 / 建连 / 探测 / 排空定时器、`RunAtMonotonic`
（跨区换图看门狗）、agones 等待者都另记一份 `steady_clock` 截止点，到点复核、未到就按剩余时间重挂。
改为单调时钟后它们不再需要兜墙钟跳变，但仍兜得住"秒数转微秒的截断让定时器早到一点"，且无害，
所以保留，只把注释改准。

**测试。** `timer_queue_unit_test` 的 `TimerClockTest.*`：换算纯函数的偏移与保序、`monotonicNow`
不回退、过去墙钟时刻乱序挂仍按墙钟先后触发、1970 年的墙钟时刻（单调域为负）照常触发且后续定时器
照常上弦、`runAt` 与 `runAfter` 同域比较、Windows `nextTimeoutMs` 按单调时钟倒数。不改系统时钟；
"墙钟回拨后定时器不再晚到"的端到端效果需在可改系统时间的环境里手工验证。

## 写定时器的规约

- **单例 / `thread_local` 对象**：捕获列表留空，回调里用全局句柄取。
  `cpp/libs/services/scene/core/system/redis.cpp:89` 就是这个写法。
  这样"生命周期"这个问题从存在变成不存在，不需要论证。
- **per-instance 对象**（每实体、每连接、每会话）：走 `TimerTaskComp`，
  不要裸捕获 `this` 去挂 `loop->runAfter`。它析构自动 `Cancel()`，
  有代际校验，两个平台行为一致。
- **回调里要碰的对象可能被销毁**：捕获**稳定 ID**（entity id / player id）
  而不是指针，触发时再查一次，查不到就返回。这条同时免疫"对象被移动"。

## 相关

- 回归测试在 `cpp/tests/timer_queue_unit_test/timer_queue_unit_test.cpp`，两条：

  - `CancelledTimerInSameBatchMustNotRun` —— 最小语义：同批次被 `cancel()`
    的定时器不得执行。
  - `TimerOfObjectDestroyedInSameBatchMustNotRun` —— **本文描述的那次崩溃的
    原形**：宿主对象在同一批次里先被销毁（析构里规规矩矩 `cancel()` 了），
    定时器随后到点。

  两条都用**裸 muduo 定时器**而不是 `TimerTaskComp` —— 后者 `Cancel()` 会顺手
  清掉 `callback`，那样即使 muduo 真的跑了这次触发也看不出来，测不到守卫在不在。

  第二条里的回调**故意不解引用 `this`**，只给一个外部计数器加一。真实代码当然
  会碰 `this`，那正是崩溃点；但测试若也去碰，它自己就成了未定义行为，而不是
  一个能稳定判定的探测器。计数器活在测试栈上、lambda 活在尚未销毁的 Timer 里，
  所以"回调跑了没有"这个观测本身是良定义的。
- `TimerTaskComp` 作为 ECS 组件成员时，还有一个独立的坑（entt 删除组件走
  swap-and-pop 会移动元素，`TimerTaskComp` 的移动赋值会把源也 `Cancel()`，
  导致误杀另一个实体的定时器）。属于 entt 存储策略问题，不在本文范围。

## 2026-10-21 验收记录

本轮先在保留旧实现的独立构建树运行回归，再修复并验证 Windows 实现与 Linux overlay。

| 修复前回归 | 实际观测 |
|---|---|
| `ExpiredCallbackCleanupCanCancelRepeatingTimer` | 重复 B 被错误重新入队，外置计数断言失败，退出 1 |
| `CancellationCallbackCleanupCanCancelAnotherActiveTimer` | 重入 `cancelInLoop` 命中双索引大小不一致断言，退出 134 |
| `EventLoopDestructionCanCancelOwnAndAnotherTimer` | ASan 报 `attempting double-free`，退出 1 |

修复后的完整定时器专项：Windows MSVC Debug **31/31**；Linux GCC 13.4 Debug、ASan/UBSan **30/30**，均零失败、零禁用、零跳过。
Windows 多一条 `nextTimeoutMs` 专项，因为 Linux 使用 timerfd。Linux 上述三个清理重入用例另连续运行 20 轮全部通过，
无 ASan/UBSan/泄漏报告。回归中发现固定 20ms 退出可能掩盖误重入队，最终版本改为本批 reset 后的 pending 阶段安排退出，
确保旧重复 Timer 的下一次开火排在退出之前；不依赖测试线程恰好及时获得调度。

原件在 `build/verification/timer-hiredis-complete-20261020/`：
`red-*.log`、`red-artifact-hashes.json`、`windows-timer.xml`、`linux-timer.xml`、`linux-timer-repeat.log` 与 `result.json`。
目录日期是本轮开始日期，运行与收尾跨入 10 月 21 日。Windows 使用独立 CMake/MSBuild 工程与独立库目录，串行 `/m:1`；
Linux 使用仓库正式的 `cpp/tests/redis_lifecycle_test/CMakeLists.txt`，从锁定 SDK 源码加当前 overlay 重编，不链接旧 muduo 归档。

Linux 可复用命令（各路径指向本地已锁定源码，不下载依赖）：

```sh
cmake -S <仓库>/cpp/tests/redis_lifecycle_test -B <独立构建目录> \
  -DMMORPG_SOURCE_ROOT=<仓库> -DMMORPG_SDK_ROOT=<SDK> \
  -DMMORPG_GTEST_ROOT=<锁定的googletest目录> -DMMORPG_ENABLE_SANITIZERS=ON
cmake --build <独立构建目录> -j1
ASAN_OPTIONS=detect_leaks=1:halt_on_error=1 UBSAN_OPTIONS=halt_on_error=1 \
  <独立构建目录>/timer_queue_lifecycle_test
```

时钟验证包括纯函数、真实 EventLoop 和平台等待链；**没有实际修改系统日期时间**。
没有宣称实时精度、休眠语义或生产负载下性能结论，也没有部署替换运行节点。
上方旧的组件移动风险记载只作历史：当前组件移动转移独立 State，现有专项已覆盖 swap-and-pop 幸存者与移动后宿主销毁。

## 2026-10-22：TimerTaskComp 的取消与替换重入

继续保留独立 weak State、generation、RAII 与移动转交结构。底层前述三处闭包清理修复已在主干，
本轮直接复用并重新验证，不重复重写 Windows 或 Linux overlay。

组件此前先调用 `loop->cancel()`、后清句柄和代际。取消会同步析构旧回调捕获物；捕获物的析构若再次调度、
移动或销毁宿主，外层随后读写组件就会覆盖新状态，或者访问已释放内存。只把 `Cancel` 的清状态操作前移还不够，
因为旧的调度替换流程仍在 `Cancel()` 返回后继续访问宿主。

现行顺序与接口契约：

- `CancelState` 按值保活独立状态，先清句柄、清 armed、推进 generation，最后取消底层 Timer；释放闭包后不再回写。
- 替换调度先安装新 Timer 并完成状态更新，最后取消旧 Timer。旧闭包清理期间发生的后续取消、调度或移动决定最终状态；
  外层不再访问宿主。移动赋值同样先转交源状态，再清理目标旧状态，清理后不再访问源对象。
- `RunAt/RunAfter/RunEvery` 返回是否接受调度；返回 true 不保证稍后重入操作没有覆盖它。
  空回调或无当前 EventLoop 返回 false；非法参数也返回 false，均不改变现有计时。
- `RunEvery` 必须使用正间隔；相对时长必须有限，绝对值严格小于 `INT64_MAX / 2` 微秒对应的秒数，
  为浮点转整数、截止点相加和队列求差留出余量。`RunAfter` 的零和负延迟仍按到期任务处理。
- 调度、移动、取消和销毁继续仅在所属 EventLoop 线程执行；回调不得阻塞。
  正在释放旧闭包时可以销毁一个仍存活的宿主；**组件自身已进入析构后，不支持再次武装该对象**。
  改为 bool 返回值后，库与使用方必须配套重新编译，不支持混用旧目标文件。

回归增加 `TimerTaskRegressionTest`：非法周期/时长、空回调/无 loop、零与负单次延迟、取消中重入取消/重挂/自毁、
替换中取消/重挂/移动/销毁宿主、移动赋值清理中重挂目标/销毁源，以及周期回调自行取消/销毁/替换。
未修改组件布局、协议、配置、客户端或运行节点。Java 版没有对应的 muduo 层，本轮没有共享客户端契约变更。

本轮验证入口与原始日志位于 `build/verification/timer-task-fix-20261022/`。
未修复组件的实际红态中，前五条回归均出现业务断言失败，第六条替换中销毁宿主由 ASan 报告 heap-use-after-free；
进程退出 1，后续用例未执行，不能把该轮称为九条完整测试结果。

修复后 Windows MSVC 独立 Debug 专项 **54/54**、Linux GCC 13.4 Debug/ASan/UBSan 专项 **53/53**，
零失败、错误、跳过或禁用。新增 16 条回归在 Linux 另以独立进程连续运行 20 轮，320/320 通过，
每轮均有独立 XML、日志及退出码；没有 ASan、UBSan 或泄漏报告。Linux 46 条实际编译命令均含 `-UNDEBUG`
和 `-fsanitize=address,undefined`，受核对的七个源码/构建入口首终 SHA 不变。

原件包括 `linux-red.log`、两端 `*-green.xml`/日志、`linux-green-repeat-*.xml`/日志、
`linux-green-build/compile_commands.json` 和 `result.json`。Windows 以 `/m:1`、禁用编译并发重新编译真 muduo、
组件及测试；Linux 从固定本地 SDK 镜像源码加当前 overlay 重编。未构建完整 `game.sln`，未替换服务产物或重启节点，
也不把专项结果当作已部署 SDK 归档或生产负载的验证。
