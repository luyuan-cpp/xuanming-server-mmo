# muduo 定时器取消语义与野引用

## 一句话

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
