# Redis 异步客户端重试、重连与生命周期

> 现状核对：2026-11-10，只读对照源码；本次只改文档与注释，没有新增构建或运行证据。
> 2026-10-30 起玩家存档、读档不再经过 Redis：原 `MessageAsyncClient`（`redis_client.{h,cpp}`，负责 protobuf 加载、快照写入与存盘重试队列）
> 随存档改为直接提交 TiDB 删除（`fc504300b`），存盘契约见[存盘直接提交 TiDB](./player-save-direct-commit.md)。
> 本文现在只讲 C++ 进程里的 Redis 连接这一层。下文 10-20、10-21、10-30 三节是当时的设计与验收记录，原文保留：
> 其中提到 MessageAsyncClient 的地方按当时的代码理解，历史通过只代表当时受测输入。

## 1. 职责与入口

| 层 | 职责 | 入口 |
|---|---|---|
| Hiredis 适配器 | muduo 事件循环上的 hiredis 异步连接：Channel 注册、命令回调恰好一次、连接对象与 socket 的寿命，以及进程级命令账 `Hiredis::MaintenanceState`（维护排空用） | [Hiredis.h](../../cpp/libs/engine/muduo_windows/src/contrib/hiredis/Hiredis.h)；三份副本见 10-30 一节 |
| RedisManager | 一条连接的建连、认证、重连与存活探测 | [redis_manager.h](../../cpp/libs/engine/thread_context/redis_manager.h) |
| RedisClusterClient | 玩家域路由层：单实例与 Cluster 两种模式，每个主节点一个 RedisManager；命令守卫、同 slot 停等、确定没有执行的错误在预算内重试 | [redis_cluster_client.h](../../cpp/libs/engine/infra/storage/redis_cluster/redis_cluster_client.h)；设计见[玩家态 Redis 集群化](./redis-player-store-cluster.md) §5.6 |

每个 loop 线程有三个句柄：`tlsRedis`（核心共享实例）、`tlsSceneControlRedis`（本 zone 的场景控制域）和 `tlsPlayerRedis`（玩家域路由层），
后两个只有 scene 会连。Scene 的装配与定时驱动见 [redis.cpp](../../cpp/libs/services/scene/core/system/redis.cpp)。

管理器入口为 `Connect(loop, addr, RedisConnectOptions)` 与 `Shutdown()`，内部 StartHandshake 先安装回调再 connect，
避免冷启动连接失败发生在回调安装之前。`SetupReconnect`、`InstallCallbacks` 已退役。
Hiredis 只能经 `Hiredis::Create` 创建，由 shared_ptr 持有。从 Connect 到 Shutdown 之间 `GetConnection()` 保持非空：
握手或断线期间是 `connected()` 为假的占位对象或已断开的旧连接，握手完成后才换成新连接。不能按旧 unique_ptr 临时置空的模型理解。
调用仍要区分已建连、已认证与业务可用状态。

## 2. 重连、存活探测与业务重试

- **重连**：建连失败、断线、AUTH 被拒或无应答，都由重连定时器（默认 3 秒，玩家域每个节点再加随机抖动）在 pending 阶段重建握手连接。
  定时器与排队任务带代次：同批 AUTH 成功、建连成功后重挂 AUTH 截止或显式取消，都会作废旧任务（见 10-20 一节）。
- **认证**：配了口令时 AUTH 是握手连接上唯一的命令，回 `+OK` 才移交给业务；被拒、发不出去或截止前无应答都不放行（fail-closed），
  日志带 `metric=zone_redis_auth_failed`。
- **存活探测**：默认每 5 秒检查一次对业务可见的连接，同一时刻至多一条探测 PING；15 秒没有应答就摘下、释放并走重连，
  日志带 `metric=zone_redis_liveness_timeout`。摘下排到本轮 pending 阶段执行。
- **连接层不做业务重试**：连接不可用时 `command()` 当场返回 `REDIS_ERR`，不缓存命令；在途命令在断线或释放时以空应答恰好回调一次，
  空应答表示结果未知，不是没有执行。重试与放弃由各调用点负责：玩家域路由层只在预算内重试 MOVED、ASK、LOADING 这类确定没有执行的错误，
  从不重放结果未知的命令；scene 的协调写（继承标记清理、handoff 标记撤回、改派核实）由 `RedisSystem` 的 1 秒节拍按各自的重试间隔与截止重发。
  重连成功不做业务补发（[断线重连总览](./infra-reconnect-overview.md) §1.1 与 §4.1 的 R14）。
- 原 MessageAsyncClient 的队列语义（加载 NIL 重试、存盘 `kMaxSaveRetries` 封顶退避、`RetryDuePending`、`OnReconnected`）已随它删除，不再适用于任何现行调用。

## 3. 命令回调契约

- `command()` / `formattedCommand()` 返回 `REDIS_OK` 时回调恰好调用一次；返回 `REDIS_ERR` 时回调不会被调用，回调副本当场释放。
  玩家域路由层的 `command()` / `commandForKey()` 是同一形状。
- 回调在 loop 线程运行。连接对象析构时，挂起命令收到的空应答回调里 `Hiredis*` 参数指向正在析构的对象：
  只能判断状态或发新命令（会被 hiredis 的 FREEING 状态拒绝），不能保存。
- `formattedCommand()` 可带 `NotExecutedPredicate`，只影响维护账：判为确定没有执行的错误或空应答不记 UNKNOWN。
  AUTH、探测 PING、CLUSTER SLOTS、ASKING 用 `Hiredis::hasNoSideEffect`。
- 适配器只保证自身与 C context 的寿命；业务回调捕获的其它对象仍由业务用 weak 或稳定 ID 管理。

## 4. 生命周期与观测

- 所有操作与最后一个引用的释放都在所属 loop 线程，EventLoop 必须活过连接对象与待销毁任务；Node 停机先排空 pending 再 quit。
  I/O 分发期间（含 Windows 在同一轮里跑定时器的阶段）释放最后一个引用时，整个对象、context 与 fd 推迟到本轮 pending 阶段销毁；
  同批里尚未分发的旧事件经 weak 回调与 Channel tie 失效。机制与验收见 10-21、10-30 两节。
- `disconnect()` 只在 pending 阶段或该连接自己的回调里调用。在别的 Channel 的回调里（或 Windows 定时器阶段）同步调用、
  目标又没有挂起应答而 socket 还在同一批时，hiredis 会当场注销 Channel，撞 Debug 下 `EventLoop::removeChannel` 的断言（Release 下无内存错误）。
  生产代码不调用它，释放连接一律放掉 shared_ptr。
- 维护账只按命令记：命令在业务回调返回后计完成；空应答、错误应答（`NotExecutedPredicate` 判为未执行的除外）与回调异常记 UNKNOWN，
  进程内不会清除。建连失败与断线本身不记（2026-11-11 起）：建连完成前 `command()` 一律拒收，断线时 hiredis 先以空应答回调完全部在途命令，
  各条已按自己的结局记账；连接、断线回调抛异常仍记 UNKNOWN。理由与回归用例见 10-30 一节末的「2026-11-11」。
- 观测：建连失败与断线日志带 hiredis 的 errstr（遮挡口令）；玩家域路由层每 30 秒在计数有变化时打一行 `[PlayerRedis]`，
  拒绝类或预算耗尽有增长时升 WARN。定位重连还要结合认证、代次与连接释放日志，不能把「connected」当作业务提交证明。

## 5. 验证与环境边界

本次没有检查当前监听器、Redis 数据或运行容器，不保留“Windows 服务才是真实目标、Docker 空闲”的历史环境推断。
具体连接目标以当前配置和进程身份为准。

语言服务无报错不等于编译通过。下文的专项日志、XML 与源码哈希保留原验证范围；
它们不能代替当前工作区的全量节点构建、真 Redis 资产用例或运行产物已更新的证明。

## 2026-10-20：握手截止与 socket 同批事件的生命周期修复

本节记录 2026-10-20 的 `RedisManager` 生命周期修复；现状以文首 §1–§4（2026-11-10 核对）为准。

Linux 的 timerfd 与 Hiredis socket 会进入同一批 `activeChannels_`。旧代码从重连定时器直接执行
`DoReconnect()`，可能先释放握手连接，再分发同批中仍引用它的 socket Channel。Debug 构建会在
`EventLoop::removeChannel` 断言中止，Release 则存在释放后使用风险。只有一个 loop 线程也能触发。

重连定时器现在只投递重建任务，连接的释放和重建统一在 `queueInLoop` 的 pending 阶段执行，
此时本轮 I/O 分发已经结束。定时器与排队任务均先锁定 `LifetimeToken`，失效后不再访问管理器。
每次安排或取消重连都更新代次，排队任务在执行前复核代次和 active 状态：

- 同批 AUTH 成功会作废旧任务，保持刚认证成功的连接。
- 同批建连成功可能先取消旧定时器，再安排新 AUTH 截止；此时 active 又为真，必须由代次区分旧任务。
- Shutdown 或重复 Connect 会作废旧生命周期的任务，不能在之后重新建连。

这是管理器内部的事件阶段约束，不依赖固定延时，也不改变 Hiredis 的独占所有权和业务调用接口。
AUTH 被拒或无应答时仍保持不可用、按原间隔重新认证；同批到达的 AUTH 成功可在 pending 重建前取消旧任务。
这一步只解决管理器定时器重建的分发顺序。当时 Hiredis 自己的连接/命令回调内仍不得同步销毁连接；
下节进一步收口底层所有权，使回调内撤销 owner 和同批其他 Channel 撤销 owner 也能安全清理。

Linux 回归入口为 `cpp/tests/redis_lifecycle_test/CMakeLists.txt`，使用真实 EventLoop、timerfd、
本地假 Redis 和公开管理器接口。专项用例选择 PollPoller 的稳定注册顺序，先确认截止与 socket 同时可读，
再用 loop iteration 确认 timerfd 先于同批 AUTH 分发；无需暴露生产私有接口。
构建保留断言并支持 ASan/UBSan，重新编译 muduo 源码，不复用镜像中带 NDEBUG 的归档。

## 2026-10-21：Hiredis 底层回调与最终释放

### 为什么只有 pending 重连还不够

定时器和 socket 都属于一轮事件分发。Linux `poll/epoll` 先得到一批 `activeChannels_`，再逐个调用
`Channel::handleEvent`。旧 Hiredis 虽然继承 `enable_shared_from_this`，真实调用方却使用 `unique_ptr` 或栈对象；
读、写和关闭回调仍捕获裸 `this`。`removeChannel` 中的 dummy 任务只保住 Channel，没有保住 Hiredis 和 C context。

有两种独立的失败顺序：

1. 同批较早的 timerfd 或其他 Channel 释放 Hiredis，较晚的 socket Channel 仍在待分发列表里。
   Debug 的 `removeChannel` 可能先断言，Release 可能调用已析构对象。
2. Hiredis 自己的连接、命令或断线回调撤掉最后一个业务 owner。`redisAsyncFree` 在
   `REDIS_IN_CALLBACK` 期间只能标记稍后释放；Hiredis 却已析构，hiredis 随后的 `ev.cleanup`
   仍会拿 `ac->ev.data` 回调这个已释放对象。用户回调若正在执行成员 `std::function`，清空它还可能销毁自己的捕获。

这些顺序都不需要两个线程竞争。修复必须覆盖对象、context、Channel、fd 和用户回调捕获的完整寿命。

### 统一创建与所有权

`Hiredis::Create(loop, address)` 是唯一创建入口，构造函数私有。返回 `shared_ptr<Hiredis>`，
RedisManager 和 MessageAsyncClient 的 `HiredisPtr` 同步改为这个类型；示例、直接测试和临时客户端也一起迁移。
`Create(nullptr, address)` 只允许构造永不连接的不可用占位对象，保留旧的未绑定管理器语义。

所有操作、最后一个强引用释放都在所属 loop 线程；EventLoop 必须活过客户端和待销毁任务。
Channel 仍由 Hiredis 管理，外部不取得 muduo `TcpConnection` 的强所有权，不改变 `AGENTS.md §11.7` 的连接所有权规则。

读、写、关闭回调都只捕获 `weak_ptr<Hiredis>`，触发后先 lock；失败直接返回。
Channel 同时 `tie(shared_from_this())`，使一整个 `handleEvent` 中 HUP、READ、WRITE 的组合分发都保住同一对象。
用户在回调内撤掉外部 owner 时，当前分发栈还持有对象，C 库可以安全返回；回调结束后不额外保留业务对象。

### 最后释放必须保住 socket，不能只延迟 Channel

工厂的自定义删除器在 `eventHandling()==true` 时把**整个 Hiredis 的 delete** 投递到本轮 pending 阶段，
其余场合立即删除。投递任务接管最终释放责任；此时原 shared 控制块已经归零，weak 回调不能重新锁回客户端。
所以同批尚未分发的旧 Channel 即使仍在列表中，也只会跳过业务处理。

不能只把 `Channel::remove()` 排队、却立即 `redisAsyncFree`：后者会关闭 fd；较早回调里马上创建的新连接可能复用相同 fd，
而 Poller 还保留旧 Channel 的注册，导致重复注册或错误注销。保住整个对象直到 pending 后再释放，才能同时保住 context 和 fd。
这不是固定毫秒延时，不依赖调度“通常够快”；安全点是本轮全部 I/O 已完成。

真正析构时继续同步调用 `redisAsyncFree`，并保留原有契约：成功登记但尚无应答的命令，以 `reply == nullptr`
恰好回调一次。此时 `me` 仍是析构体内的有效对象，调用方可判定连接状态或尝试新命令；hiredis 的 FREEING 状态拒绝新命令，
拒绝分支立即释放没有登记的回调副本。C hook 在这个同步析构期间直接使用有效的 `ac->ev.data`，不尝试在强引用归零后 lock。

`removeChannel` 仍幂等，并排队保留已注销的 Channel 到当前分发栈退出；Node 仍必须排空 pending 后再 quit，
不能以修复后共享所有权为理由把 EventLoop 提前析构。

### 回调重入与管理器换代

- Hiredis 连接/断线回调以及 RedisManager 重连回调先复制到局部再调用，清空成员不销毁正在执行的捕获。
- 命令回调由局部 `unique_ptr<CommandCallback>` 接管，业务返回后恰好释放一次；不再访问可能被外部撤销的业务 owner。
- 主动 `disconnect()` 自身也保活 wrapper，覆盖 pending 阶段调用 C API 时同步触发断线回调的路径。
- RedisManager 的连接、断线、AUTH、PING 回调先检查弱生命周期令牌。Shutdown 或新 Connect 使旧令牌失效，
  即使旧 Hiredis 的最终释放被排到 pending，旧内部回调也不会访问已销毁或换代的管理器。
- 重连定时器和已排队的重建任务仍检查 `reconnectGeneration_`。同批 AUTH 成功，或建连成功后重新安排 AUTH 截止，
  都会使旧重建失效；共享所有权不能代替业务代次判断。
- 释放业务连接会同步触发用户的空应答回调，用户可能在这里销毁管理器或再次 Connect。
  `Connect` 先把旧连接移到局部，初始化新周期后才释放旧连接；`Shutdown` 先清完全部状态，再释放局部旧连接。
  最后一次业务释放后不再访问管理器成员。存活超时摘除在释放后额外用弱令牌确认原周期仍存在，才安排重连；
  不能只在 pending 任务入口检查一次令牌，就假定用户回调返回后管理器仍然活着。
- `ping()` 不再绑定裸 `this`，并处理空应答，避免断线释放时解引用空 reply。

认证仍然 fail-closed：非空口令只在 AUTH 成功后移交；连接释放时不重发、不伪造业务成功，不改变业务重试或资产提交语义。
本节保证适配器与管理器自身的生命周期，业务传入的 CommandCallback 若捕获其他对象，仍由业务用 weak 或稳定 ID 管理。

### 回归与平台覆盖

Windows 与 `third_party/muduo` 的 Hiredis 公共头及实现同步；Linux 的持久补丁在
`tools/archived/muduo_linux_overlay/contrib/hiredis/`，包括新的工厂头与示例，不手改 gitlink 中的上游。
构造契约改变后库和所有使用方必须配套重编，不能拿旧 Hiredis 布局的库链接新头。

`cpp/tests/rpc_controller_test/hiredis_command_lifecycle_test.cpp` 保留命令格式拒绝、FREEING、DISCONNECTING 和正常释放测试，
并增加连接/应答/断线回调内撤掉 owner 的测试。`cpp/tests/redis_lifecycle_test/hiredis_event_batch_test.cpp`
使用 loopback 假服务端、真实 PollPoller 和 socket：在同批较早 Channel 中释放已可读客户端并马上创建替代连接，
验证旧应答不再分发、旧 fd 在分发结束前仍占用、pending 恰好空回复一次、捕获最终释放、新连接还能完成命令。
原有管理器握手同批测试继续覆盖截止与 AUTH 同轮、Shutdown、新 Connect、迟到代次等路径。

独立入口 `cpp/tests/redis_lifecycle_test/CMakeLists.txt` 必须 Debug、保留断言、默认启用 ASan/UBSan，
当前 overlay 源码重新编译。测试使用本地假服务端，不读取真实 Redis 凭据，不修改系统时钟。
具体执行结果、源码绑定和未验证项见下面的验收记录，不能沿用文首旧的语言服务检查当作运行证明。

### 2026-10-21 实际验收

修复前，真实假服务端上的 `ReplyCallbackCanReleaseLastOwnerAndCompletesPendingCommandOnce`
在回调内撤掉 owner 后命中 `~Hiredis` 的 Channel 断言，退出 134；日志保留在 `hiredis-red-test.log`。
与同目录的三条定时器红态一起，证明问题不是仅凭静态推断。

| 验证范围 | 结果 |
|---|---|
| Windows：Hiredis 命令/回调生命周期、RedisManager 认证/重连/存活及重入 | 27/27，零失败、禁用、跳过 |
| Linux：相同 27 项，加 3 项管理器 timerfd 同批测试与 1 项底层 socket 同批测试 | 31/31，零失败、禁用、跳过 |
| Linux 底层 socket 同批撤销 owner、立即建替代连接 | 连续 20 轮通过 |
| `redis_client.cpp` 与迁移后的 `currency_test.cpp` 消费方编译 | 独立 Windows Debug 对象编译通过，原有裸指针成员检查通过 |

Linux 全部启用 ASan/UBSan 和泄漏检查，最终运行没有报告错误。Windows 库与测试使用本轮源码独立编译，
不与其他聊天的 `game.sln` 产物共用输出。两端完整专项连同定时器合计 Windows **58 项**、Linux **61 项**。
消费方验证只代表编译，**没有运行 currency 真 Redis/资产用例**，也没有运行整个 RPC 工程或重编全部节点。

原件目录 `build/verification/timer-hiredis-complete-20261020/` 包含：
`windows-redis.xml`、`linux-redis.xml`、`linux-batch-repeat.log`、`windows-consumers-final-build.log`、各构建/退出记录、
源码哈希和 `result.json`。Windows 的实际独立工程源在该目录的 `windows-source/CMakeLists.txt`；
Linux 的永久回归入口是本节前述 CMake 工程，运行其 `redis_lifecycle_test` 即可重现完整专项。

本轮还遇到并修复测试工程的增量构建陷阱：`file(COPY)` 保留源时间戳，已覆盖的新 Hiredis 内容可能比旧 `.o` 更早，
链接因此报找不到 `Hiredis::Create`。现在基线与覆盖件用 `configure_file(COPYONLY)` 按内容更新，不重复覆盖仍在使用的 overlay；
删除覆盖件时还原基线或清除仅覆盖件的残留文件。首次链接失败日志保留，最终构建重新编译了 Hiredis 后退出 0。
另保留 Windows 独立消费方工程首次缺 include 的失败原件，补齐与仓库工程一致的 include 后再编译通过，未绕过检查。

三条真 Redis 资产测试的旧“故意泄漏连接”改为明确的退出顺序：用局部 RAII 守卫在业务客户端及其回调捕获还活着时先释放连接，
失败或超时提前返回也执行。不能把底层 Hiredis 自身保活扩大成“所有业务回调捕获都自动安全”。

本轮没有修改真实 Redis 数据、读取业务凭据、调整系统时间或部署节点；没有提交或推送。
现有运行进程仍需按正常构建/发布流程替换，不能用本轮专项通过代替运行产物已经更新的证明。

### 本轮验证回执

- 修复前，首条同批 AUTH 成功回归在 Debug 的 `EventLoop::removeChannel` 断言中止，进程退出 134；
  日志先出现 timerfd，再出现重建及 Hiredis 析构，同批 socket 尚未分发。没有关闭断言来绕过问题。
- 修复后同一用例退出 0；最终完整工程 22/22 通过、0 失败、0 跳过：新生命周期 3 项、命令释放 4 项、
  AUTH/重连 10 项、存活探测 5 项。新用例覆盖同批 AUTH 成功、管理器先析构后旧任务失效、建连成功重挂 AUTH 截止后旧代次失效。
- 新生命周期 3 项再重复 20 轮，stdout 实记 60/60 通过、进程退出 0；重复运行的 XML 只记录最后一轮，不能把它当成 60 项 XML。
- 使用固定 Linux SDK 镜像 `fc6fad59c914...`，串行构建；42 条编译命令均包含 `-UNDEBUG`、
  `-fsanitize=address,undefined`。ASan/UBSan 和泄漏检查未报错。外部 hiredis 1.2.0 使用 SDK 的预编译库，未对它自身重编 sanitizer。
- 最终受测的 6 份源码/构建入口前后 SHA256 全部相同。原始失败、成功 XML、日志、编译命令及
  汇总保存在 `build/verification/hiredis-handshake-lifetime-20261021/summary.json` 同目录。
- Windows 构建因其它 MSBuild/cl 正在运行而未启动；本轮没有重建或重启运行容器，没有修改防火墙、
  端口、TLS、密码或权限。Java 版没有对应 muduo 适配层，本次未改变客户端契约，无 Java 对齐代码或构建。

在准备好固定 SDK 的 Linux 环境中，回归入口如下；`MMORPG_SOURCE_ROOT` 是本轮源码，
`MMORPG_SDK_ROOT` 是已锁定并安装依赖的 SDK 源码根目录：

```sh
cmake -S cpp/tests/redis_lifecycle_test -B build/redis-lifecycle \
  -DCMAKE_BUILD_TYPE=Debug -DMMORPG_SOURCE_ROOT="$PWD" -DMMORPG_SDK_ROOT="$PWD"
cmake --build build/redis-lifecycle --parallel 1 --target redis_lifecycle_test
ASAN_OPTIONS=detect_leaks=1:halt_on_error=1 UBSAN_OPTIONS=halt_on_error=1 \
  timeout 60s build/redis-lifecycle/redis_lifecycle_test
```

## 2026-10-30：三份 Hiredis 适配器恢复逐字节一致，补 Windows 同批与半关闭回归（已合入并验证）

### 三份适配器

Hiredis 适配器有三份副本：`cpp/libs/engine/muduo_windows/src/contrib/hiredis/`（Windows 编译）、
`tools/archived/muduo_linux_overlay/contrib/hiredis/`（Linux 镜像构建时覆盖进 muduo-linux）、
`third_party/muduo/contrib/hiredis/`（使用方 include 的头文件镜像，`.cc` 不参与编译）。
主干 `72017963` 上三份 `Hiredis.h` 一致，`Hiredis.cc` 有三处分叉：

- `redisAsyncConnect` 的端口参数：Windows 用 `port()`，overlay 用 `toPort()`。上游 muduo 的端口访问器随版本改名：
  v2.0.2（`b55b0ac`）叫 `toPort()`，v2.0.3 起叫 `port()`（如 `f1fc77e`，v2.0.3-1）。muduo-linux 的 gitlink 在两者之间翻转过不止一次，
  例如 `a310a18b9` 时 gitlink 是 `f1fc77e`、overlay 却是 `toPort()`，按锁定版本编不过。`72017963` 上 gitlink 是 `b55b0ac`，与 overlay 一致。
- `CallbackPhase` 的构造参数：Windows 叫 `active`，与成员同名；overlay 改名为 `activeState`，以通过 muduo 的 `-Wshadow -Werror`。
- `third_party/muduo` 的 `.cc` 停在加命令账（`MaintenanceState`）之前的旧版。

现在三份统一为：端口参数直接写两版共有的 `sockets::networkToHost16(serverAddr_.portNetEndian())`（补 `muduo/net/Endian.h`），
参数一律叫 `activeState`，镜像 `.cc` 跟上当前实现。语义不变；子模块检出哪一版都能编，三份 `.cc` 与三份 `.h` 逐字节一致，
任意两份 `cmp` 应无输出（2026-11-11 起由 `tools/scripts/tests/hiredis_adapter_copies_contract.tests.ps1` 机械守住）。命令账与生命周期逻辑没有改动。

### Windows 回归

`cpp/tests/redis_lifecycle_test/hiredis_event_batch_test.cpp` 依赖 eventfd 与 `/proc/self/fd`，只在 Linux 编。新增
`cpp/tests/rpc_controller_test/hiredis_event_batch_windows_test.cpp`，整文件包在 `#ifdef _WIN32` 里，只登记进
`rpc_controller_test.vcxproj`。Windows 的 PollPoller 按注册顺序收集 activeChannels_，因此可以确定地构造「前置通道先于受害连接派发」。

- `HiredisEventBatchWindows.EarlierChannelReleasesLastOwnerOfReadyConnectionInSameBatch`：回环 UDP 前置通道先于受害 Hiredis 注册，
  受害连接上有 `+PONG` 可读、挂起的 PING 可写。前置通道的回调撤掉受害连接唯一的 owner，并当场新建替代连接。断言：
  受害连接已就绪的应答不再分发；挂起命令恰好空应答一次，而且发生在同一 iteration 的 pending 阶段（`eventHandling()` 为假）；
  替代连接能完成 PING。
- `HiredisEventBatchWindows.PeerHalfCloseDisconnectsAndOwnerMayReleaseInDisconnectCallback`：对端只 `shutdown(SD_SEND)`，即纯 FIN，
  WSAPoll 可能只报 POLLHUP。断言：断线回调恰好一次且为 `REDIS_ERR`；挂起命令恰好空应答一次；所有者在断线回调里放掉最后一个引用后，
  对象活到本次事件返回，排空后对象与保存的回调都已释放。

每条用例给自己的连接传独立的 `MaintenanceState`（同 `hiredis_command_lifecycle_test.cpp`），不碰进程级账本。
末尾 `Close()` 后断言 `Joined()`：所有连接对象、`removeChannel` 排队保活的 Channel 引用都已释放，命令都已结束。

回调里放掉 owner 后对象仍存活，主要靠 Channel 回调里 `weakSelf.lock()` 拿到的局部强引用。`Channel::tie` 是纵深防御，
去掉它这两条用例照样是绿的，目前没有单独锁住它的回归用例。
用例的回调以引用捕获测试栈上的计数，所以每条用例都在全部被捕获的变量之后声明收尾守卫（`ScopeExit`）：
停前置通道、清回调、释放连接、排空 pending。ASSERT 中途失败提前返回时，连接析构触发的回调也只会写到还活着的变量。
loop 跑在测试线程上，等待都由 2 秒看门狗或 WSAPoll 1.5 秒超时兜底。排空时加零延时定时器：在 loop 线程上
`queueInLoop` 不会唤醒 poll，Windows 没有定时器时一次 poll 要等满 `kPollTimeMs`（100ms，10 秒是 Linux 分支的值）。
这两条用例依赖 `Hiredis::Create`，修复前的代码编不过，红态以 10-21 Linux 专项为准（退出 134）。

### 2026-11-05 验证收尾

本节代码与文档于 11-01 随 `e33b99351` 并入主干。Codex 核对历史构建、运行日志及源码哈希，并补完 Windows 两条用例的 20 轮重复检查：

| 范围 | 实际结果 | 证据 |
|---|---|---|
| 三份 Hiredis 副本 | 11-02 `fc.exe /b` 两两比较六组全部一致；11-05 再核对 SHA256 一致 | `build/player-store-verify-20261101-r2/15-hiredis-copy-results.json`；本轮 `inputs-before.json` |
| Linux `redis_lifecycle_test`，muduo-linux `b55b0ac` | 11-02 实跑 122/122，ASan/UBSan 无报告 | `build/player-store-verify-20261101-r2/15-linux-b55b0ac-test.log` |
| 同上，muduo-linux `f1fc77e` | 11-02 实跑 122/122，ASan/UBSan 无报告；确认端口写法两版都能编 | `build/player-store-verify-20261101-r2/15-linux-f1fc77e-test.log` |
| Windows `muduo_grpc_test` | 11-02 实跑 200 通过、0 失败；5 条 Go 互通用例缺外部端点而跳过；`HiredisPeerFin` 41ms 通过 | `build/player-store-verify-20261101-r2/15-hiredis-grpc-after-buildfix/muduo_grpc_test.stdout.log` |
| Windows `rpc_controller_test` 全量 | 11-05 实跑 298/298，另 2 条既有 DISABLED 基准；`HiredisEventBatchWindows` 两条都通过 | `build/verification/team-follow-cobalt-20261105T145854735Z/windows-tests/rpc_controller_test.stdout.log`（两条用例见 207–210 行） |
| Windows `HiredisEventBatchWindows.*` 重复 20 轮 | 本次实跑 40/40，退出 0，零失败、零跳过、stderr 为空 | `build/verification/hiredis-event-batch-repeat-20261105-033750494/stdout.log`、`result.json` |

三份当前 `Hiredis.cc` 的 SHA256 均为 `8C8AF27C6F03B7536CC18B7C6CAA87968D88CBA5B8606A224DDEE4D0D79B55F4`，
三份 `Hiredis.h` 均为 `B8BB1AC73AA86313A65CEB5B1C4687BC33CC24F57AAB203BDB64B201026E4F21`，
与 11-02 的记录及两版 Linux 实际构建目录内的源码一致。Windows 测试文件自 `e33b99351` 后未改，
SHA256 为 `2489095421E90DA40178E7C68A4BD64CC1224ADDE66609068C321BCDF9542B6C`。

本轮重复检查使用 11-05 构建的 `rpc_controller_test.exe` 的逐字节副本及同目录 DLL，存入上述证据目录的 `bin/`，
在仓库根目录执行 `--gtest_filter=HiredisEventBatchWindows.* --gtest_repeat=20`；本轮未重新编译。
exe 晚于 `muduo.lib`，该库 SHA256 与 11-02 验证记录相同。源码、库和原 exe 的运行前后哈希均未变化，见
`inputs-before.json`、`inputs-after.json`；副本哈希见 `binary-snapshot.json`。
40/40 按 stdout 中每条用例各 20 次 RUN / OK 统计，`last-iteration.xml` 只保存最后一轮。

上述源码一致性结论限于本节 Hiredis 适配器及 Windows 测试；历史 298/298 不代表当前工作区其它在途改动已重新全量验证。
Java 版使用 Redisson，没有对应 muduo 适配层，本节无 Java 待办。

### 2026-11-10 独立复核（Claude，只读核对源码与原始证据，未编译、未运行）

复核对象是 `e33b99351` 中的 Hiredis 部分与收尾提交 `dc1595b85`。结论：没有发现需要改代码的缺陷；11-05 一节记录的运行事实与原始文件一致，
本节标题的「已合入并验证」有依据。顺带把文首 §1–§4 改成现状（原文按 10-23 描述已删除的 MessageAsyncClient），
并修正 `redis_manager.{h,cpp}` 与 Windows muduo `Channel.cc` 里描述 Hiredis 旧行为的注释。下面是补充、订正与残余。

**证据补充与订正**

- 11-05 一节说哈希「与 11-02 的记录一致」：11-02 只留了 `fc.exe /b` 的退出码，能直接对上哈希的是两版 Linux 构建目录里的副本
  （`build/player-store-verify-20261101-r2/15-linux-<sha>/build/muduo-source/contrib/hiredis/`），11-10 核对与当前一致。
  两个构建目录里的 `InetAddress.h` 分别是 `toPort()` 与 `port()`，确认两轮各用了自己那版 muduo；52 条编译命令都带 sanitizer 与 `-UNDEBUG`。
- 11-02 两轮 Linux 构建的输出目录在 Windows 绑定挂载上，大小写不敏感，上游 `muduo/net/boilerplate.cc`（包含 `BoilerPlate.h`，实际文件名是小写）
  因此编过。11-05 改到容器 `/tmp` 构建后，首轮就在这里退出 2（`build/verification/cobalt-redis-lifecycle-20261105T151615998Z/`），
  随后 `993b6da9d` 在回归入口的 CMakeLists 里排除了它。当前入口由 11-05 15:20Z 那一轮实证：122/122，
  CMakeLists 与 Hiredis 的哈希同 HEAD（`build/verification/cobalt-redis-lifecycle-final-20261105T152044053Z/`）。
- `74b097888`（11-08）改了 `redis_manager.{h,cpp}` 和同一个 Linux 二进制里的 `redis_manager_event_batch_test.cpp`，之后 Linux 套件没有再跑。
- Windows：`lib/muduo.lib`（Debug）生成于 `Hiredis.cc` 最后一次修改之后；11-02 的两次 Debug 整解里 muduo 都判为最新、没有重编，库就是由当前源码编出的。
  11-05 重复检查的 XML 里，用例行号 226、348 与当前测试文件一致。11-08 命名审查整解后 `rpc_controller_test` 366/366 通过，
  两条 `HiredisEventBatchWindows` 仍在其中（`build/verification/naming-audit-20261108/cpp-tests/rpc_controller_test/`）。
- 现行 SDK 钉住清单（`build/docker-debug-current/cpp-toolchain.json` 的守卫文件）记录的 overlay `Hiredis.cc` / `Hiredis.h` 哈希与当前一致。

**用例强度与残余**

- `EarlierChannelReleasesLastOwnerOfReadyConnectionInSameBatch` 发出 `+PONG` 后只等前置 UDP 通道可读，没有确认受害连接已经可读（Linux 版会等）。
  回环投递晚于那次 WSAPoll 时，受害连接只靠挂起 PING 的可写事件进入同批，「已就绪的应答不再分发」这条断言就恒为真；
  删除器不再延迟这类回归仍会被抓到（Debug 断言或 `nullReplyInPending`）。
- Windows 两条用例没有见过红态（见上文）；weak 回调与 tie 这一层在 Windows 上没有单独锁住它的用例。
- Linux 用例里的 `replacementUsesDifferentFd` 只是重复 `oldFdHeldThroughBatch` 的结论，没有独立证明新连接拿到了不同的 fd。
- 三份副本逐字节一致只靠手工 `cmp`，没有自动守卫；SDK 守卫只记 overlay 的哈希，不与 Windows 副本、头文件镜像比较。11-11 已补契约测试，见下节。
- `disconnect()` 的调用时机约束已写进 §4。
- 待维护排空负责人确认：建连失败、没有在途命令的出错断线也记 UNKNOWN（`Hiredis.cc` 的 `connect()`、`connectCallback`、`disconnectCallback`）。
  hiredis 在建连完成前不置 `REDIS_CONNECTED`，`command()` 在那之前一律拒收，所以这两种情况其实没有结果未知的命令；
  但进程里只要发生过一次 Redis 建连失败（例如启动时 Redis 尚未就绪），维护排空的 Redis 前沿就再也过不了，只能 hold。
  `hiredis_command_lifecycle_test.cpp` 的 `FailedConnectCallbackCannotReenterNativeConnect` 断言了这个行为，所以先按有意设计记录，不改代码。
  11-11 负责人定改掉，见下节。

**待 Codex 验证**

1. Linux HEAD 两版重跑。新建 `build/verification/hiredis-linux-head-<时间戳>/`，以
   `build/verification/cobalt-redis-lifecycle-final-20261105T152044053Z/runner.py` 为模板，只改两处：`SDK_ID` 换成
   `build/docker-debug-current/cpp-toolchain.json` 的 `imageId`；删掉对 `FINAL_NODES` 的断言（那是 cobalt 会话的前置条件）。
   其余照旧：只读封存上下文、在容器 `/tmp` 构建、`--parallel 1`、`ASAN_OPTIONS=detect_leaks=1:halt_on_error=1`、
   `UBSAN_OPTIONS=halt_on_error=1`、`timeout 60s`。先按 gitlink（`b55b0ac`）跑一轮；再把 `git -C third_party/muduo-linux archive f1fc77e`
   解到运行目录、只读挂到 `/src/third_party/muduo-linux`，跑第二轮。
   通过标准：两轮的配置、构建、测试都退出 0；XML 0 失败、0 跳过，总数不是 122 时列出多出或缺少的用例名；
   日志里没有 `AddressSanitizer`、`LeakSanitizer`、`runtime error:`；`changedLiveSources` 为空；两轮构建目录的 `InetAddress.h`
   分别是 `toPort()` 与 `port()`。失败时保留完整日志、`result.json`、首个错误行、最后一个 `[ RUN ]`、镜像 ID 与 muduo 的 SHA，不重试。
2. 可选：Windows 加量重复。工作区有其它会话在改 `rpc_controller_test` 工程，不要重编；把现有 exe 与同目录 DLL 复制到
   `build/verification/hiredis-repeat500/bin/`，运行前后各记一次 exe 与三份 Hiredis 源码的哈希，在仓库根目录运行
   `build/verification/hiredis-repeat500/bin/rpc_controller_test.exe --gtest_filter='HiredisEventBatchWindows.*:HiredisCommandLifecycle.*:HiredisMaintenanceState.*' --gtest_repeat=500 --gtest_color=no`。
   通过标准：退出 0，每条用例的 RUN 与 OK 各 500 次，没有 FAILED，stderr 为空。
3. 本次只改注释的 `cpp/libs/engine/thread_context/redis_manager.{h,cpp}` 与 `cpp/libs/engine/muduo_windows/src/muduo/net/Channel.cc`，
   随下一次 muduo 与 thread_context 的串行 Debug 构建（`/m:1`）确认即可。thread_context 目录里的 `trace_context_tls.h` 有其它会话的在途改动，
   单独构建前先确认归属。

Java 版使用 Redisson，没有对应适配层，本次无 Java 待办。

### 2026-11-11：维护账只按命令记，补三份副本守卫（Claude，未编译、未测试，待 Codex 验证）

负责人 11-11 定：上节两条残余直接改代码。

**维护账**

- 三份 `Hiredis.cc` 去掉连接事件上的 `MarkUnknown()`：`connect()` 的同步失败、`connectCallback` 的失败分支、`disconnectCallback` 的出错断线。
  连接、断线回调抛异常仍记 UNKNOWN：业务处理没有走完，无法证明它发起的写有确定结局。三份 `Hiredis.h` 在 `MaintenanceState` 上补了这条契约。
- 为什么不会漏记：hiredis 在 `redisAsyncInitialize` 里清掉 `REDIS_CONNECTED`，`__redisAsyncHandleConnect` 建连成功后才置位
  （`third_party/redis/deps/hiredis/async.c`），`command()` / `formattedCommand()` 在那之前一律拒收，失败的建连回调一定发生在置位之前；
  断线回调只由 `__redisAsyncFree` 调用，它先用空应答回调完 `replies` 里全部在途命令，各条已经按自己的结局 `Complete(known)`。
  所以连接事件本身不带额外的结果未知。旧实现再记一次，只会让一次没有在途命令的断线、或启动时 Redis 尚未就绪的一次建连失败，永久卡住维护排空。
- 口径与另外两本账一致：`RpcChannel`、`muduo_grpc::Channel` 的维护账也只在命令或回调层面不确定时记 UNKNOWN，不因连接事件记。
- 用例都在 `cpp/tests/rpc_controller_test/hiredis_command_lifecycle_test.cpp`（Windows 的 `rpc_controller_test` 与 Linux 的 `redis_lifecycle_test` 都编它）：
  - `FailedConnectCallbackCannotReenterNativeConnect` 末尾的前沿断言由「不可证明」改为「可证明」，这正是本次改的语义，旧实现在这里红；
  - 新增 `ErrorDisconnectWithOnlyNoSideEffectCommandKeepsFrontierKnown`：假服务端收到命令就关连接，在途只有 `hasNoSideEffect` 的 PING，
    出错断线后前沿可证明。旧实现红，新实现绿；
  - 新增对照 `ErrorDisconnectWithSideEffectCommandInFlightStaysUnknown`：在途是 `SET`，出错断线后前沿仍不可证明。新旧实现都应为绿，防止放宽过头。
- 改后三份仍逐字节一致：`Hiredis.cc` 为 `4035bfa16884fa19341c8ed3fd141c68c356e24e7bf49fb5bec5b7273e95a1b3`，
  `Hiredis.h` 为 `92f40466510c87a3a2a5e8ea05d3fa4b1bdf910cf928dc9667aa399966350f1b`。

**三份副本守卫**

- 新增 `tools/scripts/tests/hiredis_adapter_copies_contract.tests.ps1`：三处都有 `Hiredis.h` 与 `Hiredis.cc`（H1）；三份逐字节一致（H2）；
  不调用随 muduo 版本改名的 `toPort()` / `port()`（H3）；比较函数用临时文件自检，差一个字节、只差换行都判不一致（H4），保证测试真能红。
- `.github/workflows/deploy-config-tests.yml` 的 push 与 pull_request 触发路径加上三处 `contrib/hiredis/**`，只改其中一份也会触发；
  该 job 自动执行 `tools/scripts/tests/` 一级目录下的全部脚本，不需要另行登记。

**待 Codex 验证（取代上节 1–3 项，串行执行）**

1. 契约测试：在仓库根目录执行 `pwsh -NoProfile -File tools/scripts/tests/hiredis_adapter_copies_contract.tests.ps1`，通过标准为 `fail=0`、退出 0。
2. Windows 构建：`msbuild cpp\libs\engine\muduo_windows\muduo.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`，退出 0。
   `Hiredis.h` 只改了注释，引用它的其它库不必为本次改动重编；测试工程只经 AdditionalDependencies 链接 `muduo.lib`，不会顺带重编它，所以必须先做这一步。
3. Windows 测试：`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller -LogDir <日志目录>/rpc`，
   再 `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter muduo_grpc -LogDir <日志目录>/grpc`。
   通过标准：两个 exe 的修改时间都晚于第 2 步的 `lib/muduo.lib`；全量 0 失败；
   `ErrorDisconnectWithOnlyNoSideEffectCommandKeepsFrontierKnown`、`ErrorDisconnectWithSideEffectCommandInFlightStaysUnknown`、
   `FailedConnectCallbackCannotReenterNativeConnect`、两条 `HiredisEventBatchWindows` 与 `HiredisPeerFin` 都实际运行。
   工作区里 `rpc_controller_test.vcxproj` 等文件有其它会话的在途改动，构建失败落在它们身上时如实记录，不替它们修。
4. 可选，取红态：只在隔离副本与独立输出目录里，把三处 `MarkUnknown()` 恢复后编 muduo 与 `rpc_controller_test`，
   `ErrorDisconnectWithOnlyNoSideEffectCommandKeepsFrontierKnown` 与 `FailedConnectCallbackCannotReenterNativeConnect` 应失败，对照用例应通过。
   不要为取红在主工作区重编共享的 `lib/muduo.lib`。
5. Linux：按上节第 1 项的做法（11-05 runner 模板、容器 `/tmp` 构建、ASan/UBSan、`timeout 60s`），在 `b55b0ac` 与 `f1fc77e` 各跑一轮
   `redis_lifecycle_test`。总数应比 11-05 的 122 多 2 条，其余通过标准同上节。
6. overlay 的 `Hiredis.{h,cc}` 变了，现行 SDK 钉住清单的守卫会判 SDK 过期（`build_docker_cpp_debug.py` 报「SDK 依赖输入已变化」属预期）。
   Docker 调试栈重编 C++ 镜像之前，要先按既有流程重建并登记 Linux SDK；SDK 按 muduo 自己的编译选项（含 `-Werror`）编 overlay，顺带确认新代码没有告警。

Java 版使用 Redisson，没有对应的维护账与适配层，本次无 Java 待办。
