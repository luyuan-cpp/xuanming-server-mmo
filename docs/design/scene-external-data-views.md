# 跨服务数据副本：按需缓存、版本视图与 C++ 协程边界

> 2026-11-05 讨论记录。以 Scene 读取 Guild 数据为例，记录通用设计选择与伪代码。
> 本文不声称当前仓库已实现这些缓存、变更流或协程运行时，也不授权迁移现有业务。
> 本次仅记录文档，不修改业务代码；两种服务器版本都尚未据此实施。

## 1. 数据所有权与连接生命周期

Guild 拥有公会数据的权威状态。Scene 可以保存自己需要的只读副本，但这份副本必须能从权威端重新构建。
副本的生命周期由数据读取与同步模块管理，不挂在 `OnGuildConnected`、节点握手成功或 TCP 重连回调里。

增加一个服务，只有真正消费它的数据的业务需要增加读取契约；其他服务不应新增一套连接恢复分支。
真实的数据依赖仍然存在，不能通过通用框架消除。如果大量服务互相保存完整副本并互相等待启动，应重新检查职责和数据归属。

优先选择如下：

| 需求 | 默认选择 | Scene 重启后的行为 |
|---|---|---|
| 偶尔读取，能接受明确范围内的旧值 | 按需查询，可丢弃的本地缓存 | 缓存为空，后续按正常读取路径重新获取 |
| 必须高频本地读取并持续接收变化 | 限定范围的快照与变更流，维护本地视图 | 重新获取快照，再从其恢复位置接收变更 |
| 权限、成员变更或资产提交等强一致操作 | 由数据权威端执行并在提交点校验 | 调用权威业务接口，不凭本地缓存作最终提交判断 |

这是一组可复用模式，不是“所有大厂使用同一套实现”的断言。跨服务访问通过服务契约，不据此引入跨库 SQL。

## 2. 默认方案：按需读取与可丢弃缓存

以下是接近 C++ 的伪代码，不可直接编译。`Task`、`AwaitUntil`、`RunOncePerKey` 与异步 RPC 都是能力占位名，不是标准库 API，也不表示仓库已有这些接口。

```text
// 通用缓存模块：成员和完成回调由同一个事件循环访问。
async Get(key, callerDeadline, callerCancellation):
    if entries[key] 存在且未超过本地缓存有效期:
        return entries[key].不可变快照

    // 同 key 的并发缺失合并成一次读取；所有完成路径都清理在途记录。
    task = flights.RunOncePerKey(key, async:
        再检查一次缓存，已有有效结果则返回

        // 底层是真异步 I/O。共享读取有自己的有限期限与模块取消令牌。
        value = await loader.Fetch(key, operationDeadline, moduleCancellation)
        // 失败直接传播；不把网络错误缓存成“公会不存在”或空对象。
        entries[key] = { value.不可变快照, expireAt = 单调时钟.now() + maxAge }
        return value
    )

    // 调用者超时只退出自己的等待，不随意取消其他等待者共享的读取。
    return await AwaitUntil(task, callerDeadline, callerCancellation)
```

公会数据只提供自己的读取适配，业务不感知连接恢复步骤：

```text
guildViews = ReadCache(
    loader = GuildApi.FetchGuildView,
    maxAge = 此类公会资料允许的缓存时间
)

async BuildGuildBanner(guildId, deadline, cancellation):
    info = await guildViews.Get(guildId, deadline, cancellation)
    return Banner(info.name, info.icon)
```

首次读取、淘汰后重读、Scene 重启后的加载共用同一条路径。通信暂时不可用时，这次读取按期限失败；后续真正需要数据时仍走同一接口，不在重连成功时遍历整个业务状态。

缓存模块还须限定容量与在途数量。缓存有效期不是强一致保证；若允许错误期间使用旧数据，应由该类数据的业务策略明确允许，不能默认静默返回旧值。
本例只描述读取与到期淘汰；如再增加主动失效或推送更新，还须校验版本/代次，避免晚到的旧读取覆盖较新的更新。
普通帧内计算先使用已经准备好的不可变视图，不在每个实体的每一帧都发起远端读取。

## 3. 必要时才使用快照与增量视图

前提是源端提供完整协议：快照包含作用域、数据与恢复令牌；变更流能从这个令牌之后回放，并明确报告历史过期、源代次改变等无法续接的情况。
令牌可以是不透明的 cursor，过滤后的流不必按数字连续递增，不能自行假设 `cursor + 1`。

```text
async MaintainView(source, scope, cancellation):
    needSnapshot = true
    while 未取消:
        try:
            if needSnapshot:
                view.MarkUnavailable()
                snapshot = await source.ReadSnapshot(scope, 有限期限, cancellation)
                // 源端保证分页或整批结果属于同一快照，并携带可续接的令牌。
                在所属事件循环一次替换 {数据, scope, 源代次, cursor}
                needSnapshot = false

            stream = await source.WatchAfter(scope, view.源代次, view.cursor, cancellation)
            for await item in stream:
                if item 是变更:
                    // 校验作用域、代次、顺序与删除语义；协议保证可续接，不猜测编号。
                    在所属事件循环一次提交 {应用变更, 更新 cursor}
                    // 两者之间没有 await；未应用的变更不能推进 cursor。
                if item 是源端确认的追赶进度标记:
                    按进度与新鲜度要求更新视图可用状态

            // 正常流结束也进入有退避的续接，不忙循环。
            view.MarkUncertain()
            await 异步退避等待(cancellation)
        catch 历史过期或源代次失效:
            view.MarkUnavailable()
            needSnapshot = true
            await 异步退避等待(cancellation)
        catch 可重试通信失败:
            view.MarkUncertain()
            // 保存当前数据与位置；下一轮仍从这个位置续接。
            await 带抖动且有上限的异步退避等待(cancellation)
        catch 取消:
            return
        catch 协议错误或永久拒绝:
            view.MarkUnavailable()
            报告明确错误并停止该同步任务
```

这里是一个长期受管理的订阅任务。重试次数不能代替单次 RPC 的截止时间；每次 I/O 有期限或存活检测，整个任务可取消并排空。通用同步机制处理断线续接，各业务只定义所需数据、源接口和允许的新鲜度。

必须维持以下不变量：

- 快照与变更流的衔接不能漏窗口。没有恢复令牌与历史回放契约，就不能使用“先拉全量，再随便订阅”的写法。
- 内存模式下，数据与 cursor 同寿命、同一次提交。进程重启后两者一起丢弃并重新取快照；不能只恢复 cursor，跳过本地已经丢失的数据。
- 若本地视图要持久化，则数据、源代次与 cursor 在同一事务或等价的原子提交中落盘。
- 增量缺失或历史过期时重新加载，不能跳到最新位置后宣称已经同步。
- 作用域变化由订阅协议明确处理；令牌不能在不同过滤条件、租户或数据源之间直接复用。
- 通信 Ready 只表示能通信。视图是否可用于某项业务，按其版本、追赶进度和新鲜度判断；刚收到快照不等于已经追到最新变化。

这套模式的公开参照是 Kubernetes 的 List/Watch 与 Reflector；借用的是一致快照、恢复令牌、续接与失效后重建的契约，不是要求游戏接入 Kubernetes 来保存业务数据。

## 4. C++ 协程到底会不会阻塞

### 4.1 暂停协程与阻塞线程是两回事

C++ 标准协程提供保存执行状态、暂停和恢复的机制，不自动提供网络 I/O、任务调度器、线程池或抢占式执行。
`co_await` 真正交还控制权后，调用者/恢复者可以继续运行；若调用者是事件循环，它便可以处理其他网络事件与 tick。
如果 awaiter 已就绪，或选择不持续挂起，则可以直接继续执行，不保证每次 `co_await` 都让出线程。

| 写法或情况 | 对当前执行线程的影响 |
|---|---|
| 等待接入事件循环的异步 RPC | 挂起协程并交还控制权，线程可处理其他工作 |
| 协程中调用同步 RPC、阻塞 socket、`future.get()` | 条件未满足时仍阻塞线程 |
| 协程中执行 `std::this_thread::sleep_for` | 阻塞当前线程 |
| 等待注册到事件循环的异步定时器 | 挂起协程，定时器到期后恢复 |
| 协程中执行长时间计算 | 持续占用线程；协程机制不会抢占它 |
| 等待把同步操作放进工作池的任务 | 调用线程可以不被阻塞，但工作线程仍被该操作占用 |

### 4.2 把同步函数包成协程不会自动异步化

以下为 C++ 风格示意；`Task` 与 RPC 接口是假设类型。

```cpp
Task<GuildView> ReadGuildBad(GuildId id)
{
    auto data = BlockingGuildRpc(id); // 同步等待期间仍占住执行这段代码的线程
    co_return data;
}

Task<void> UseGuildBad(GuildId id)
{
    auto data = co_await ReadGuildBad(id);
    // 外层 co_await 无法改变 BlockingGuildRpc 的同步实现。
}
```

如果 `Task` 采用惰性启动，调用时可以暂不运行函数体，但开始执行它后仍会遇到同一个阻塞调用。惰性启动仅改变执行时点。

```cpp
Task<GuildView> ReadGuildAsync(GuildId id)
{
    // 前提：发起操作不阻塞；awaiter 在完成通知到来后安排恢复。
    auto data = co_await AsyncGuildRpc(id, deadline, cancellation);
    co_return data;
}
```

“Async” 命名不是保证，要检查底层实现。awaiter 的准备、注册和取结果过程若执行同步 I/O、等锁或长计算，同样会占住线程。

### 4.3 一次真正异步等待的执行过程

```text
Scene 线程：执行业务 → 发起异步请求 → 协程保存状态并挂起 → 返回事件循环
Scene 线程：处理其他 RPC、网络事件和 tick
I/O 完成：保存结果 → 调度到约定的事件循环 → 恢复协程 → 继续后续业务
```

编译器把跨暂停点需要的参数、局部变量、promise 与恢复位置等保存到协程状态中。
这些状态可能涉及动态分配，也可能被优化消除；并不是给每个协程分配一条 OS 线程。

`co_await expr` 大致经历：

1. 先求值 `expr` 并取得 awaiter。这里就可能运行普通同步代码。
2. 调用 `await_ready()`；已就绪时可直接取结果继续。
3. 未就绪时进入挂起协议，调用 `await_suspend(handle)` 安排后续恢复。该函数本身也不能偷偷阻塞线程；它的返回方式还可以决定立即继续或转移到另一个协程。
4. 恢复后调用 `await_resume()` 取得结果或抛出错误，再执行下一句。

恢复线程由 awaiter/执行器的契约决定。标准不保证恢复到原线程；修改 Scene 的业务状态前必须回到其所属事件循环。

### 4.4 挂起不等于业务操作已经完成

```cpp
auto guild = co_await ReadGuildAsync(id);
UseGuild(guild); // 仍然要等这次读取完成，才能执行到这里
```

协程写法保持了本次业务的先后关系，同时允许执行线程在等待期间服务其他工作。
若缓存命中或请求已经完成，`co_await` 可以直接继续，这通常不需要额外切换。

### 4.5 生命周期与取消仍需显式设计

- `Task`、异步 RPC、异步定时器与任务组必须由选定运行时提供；标准关键字不自动保证超时、取消或关闭排空。普通 `std::future` 也不能仅凭名字就成为标准 awaitable。
- 不跨暂停点持有普通互斥锁、ECS 组件引用、可被替换的 Protobuf 子对象引用或不受保护的裸 `this`。
- 跨等待优先携带 key、版本、代次等值；回到所属 loop 后重新获取对象并复核。弱引用防止误用已析构业务对象，但不能替代协程帧的生命周期管理。
- awaiter 必须保证完成/超时/取消只恢复一次，任务销毁后不能再恢复已销毁的 handle；loop 退出前取消并排空其任务。
- 同键请求合并时，一名等待者取消不应随意取消其他等待者共享的操作。任务组负责共享请求、闭包和等待者的生命周期。

本项目当前 Scene 线程约束见 [Scene 线程模型](scene-node-threading-model.md)。本文没有选择或引入新的 C++ 协程库，也没有把已有回调式 RPC 改成协程。

## 5. 未来落地时的验收范围

- 冷启动、缓存淘汰和重启均通过同一读取路径恢复；未出现按对端服务器类型新增的重连业务钩子。
- 同键并发读取合并，失败不伪装为空数据；取消、期限、缓存容量与在途上限可验证。
- 快照期间源端继续写入不会漏变更；续接、删除、历史过期、源代次和作用域变化均有确定行为。
- 进程崩溃不会出现“cursor 已前进但数据未应用”；未准备好的视图不能冒充业务 Ready。
- 延迟 RPC 的等待期间，Scene 的其他事件与 tick 仍推进；把同步 I/O 包进协程的反例应能被识别。
- 恢复线程、任务关闭、对象销毁以及取消与完成竞争均符合实际运行时契约。

这些是设计验收条件，当前没有对应的新增运行证据。文档与伪代码不能视为实现完成，也不改变已有服务恢复策略。

## 6. 参考来源

- [Microsoft：Cache-Aside](https://learn.microsoft.com/en-us/azure/architecture/patterns/cache-aside)：按需加载、缓存失效与一致性边界。
- [Kubernetes：高效检测变化](https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes)：List/Watch、恢复版本、历史过期与客户端通用同步机制。
- [C++ 当前工作草案：await 表达式](https://eel.is/c++draft/expr.await)：挂起、交还控制权与 awaiter 协议。
- [C++ 当前工作草案：协程定义](https://eel.is/c++draft/dcl.fct.def.coroutine)：协程状态、启动、恢复和生命周期。工作草案与具体编译器支持程度应分别核对。
