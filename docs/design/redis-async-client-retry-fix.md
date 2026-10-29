# Redis 异步客户端重试、重连与生命周期

> 当前接口与重试行为核对：2026-10-23。本次只修订文档，没有新增构建或运行证据。
> 下文 2026-10-20／21 的生命周期设计、红绿记录与验收边界保留；历史通过只代表当时受测输入。

## 1. 职责与入口

[MessageAsyncClient](../../cpp/libs/engine/infra/storage/redis_client/redis_client.h)负责 protobuf 加载、快照写入与业务重试；
[RedisManager](../../cpp/libs/engine/thread_context/redis_manager.cpp)负责连接、认证、重连与存活探测。
Scene 的定时驱动与装配见 [redis.cpp](../../cpp/libs/services/scene/core/system/redis.cpp)，
业务持久化提交契约见[存盘围栏设计](./db-write-fencing-mysql.md)。

管理器入口为 `Connect(loop, addr, RedisConnectOptions)`，内部 StartHandshake 先安装回调再 connect，
避免冷启动连接失败发生在回调安装之前。`SetupReconnect`、`InstallCallbacks` 已退役。
Hiredis 通过工厂创建并使用 shared_ptr；从 Connect 到 Shutdown 之间 GetZoneRedis 保持非空，
握手或断线期间为不可用对象，成功后移交已完成握手的连接。不能继续按旧 unique_ptr 临时置空的模型理解。
调用仍要区分已建连、已认证与业务可用状态。

## 2. 重连与周期重试分开

- `OnReconnected()` 只用于重连通知，回收在途状态并安排待处理工作的重新投递。
- `RetryDuePending()` 由 Scene 每秒驱动，只处理到期队列，不按周期清空在途加载表。
- 队列的 `next_retry_at` 使用 steady_clock。退避依次为 500ms、1s、2s、4s、8s、16s，之后封顶 16s。
  定时器调度与连接恢复还会影响实际时间，不能把这些间隔相加当作整条操作的严格超时预算。

### 加载

NIL 的重试上限为 6 次；耗尽后的 DataNotFound 只描述该读取路径的结果，
不证明 SQL 没有角色数据，也不构成新角色创建资格。
断线造成的空 reply 会重新排队，不消耗 NIL 次数；Redis ERROR 或 protobuf 解析失败直接按失败处理。
同 key 存盘尚未落定时，加载还可能先挂起等待写入结果，不能只看 pending load 数判断卡点。

### 存盘

`kMaxSaveRetries=6` 是一次故障通知阈值，**不是丢弃阈值**。
达到阈值通知一次后仍保留最新 payload，以封顶退避继续重试，直到写入落定、被后继值取代或进入停机流程。
属主或覆盖世代护栏的终态拒绝另走业务拒绝路径，不将已作废的同一快照当作连接故障原样重试。
这些待重试快照在进程内，不能把队列保留当成已落 Redis 或 SQL。

同 key 最多一份在途写、一份更新的排队快照。新快照替代尚待重试的旧值时，继承重试次数、
下次重试时间与已告警状态，避免新调用绕过退避。旧写成功时，每份实际成功快照的提交钩子先执行一次；
若已有后继快照，先发送后继并抑制旧值的全局成功回调，不能误认为“旧快照完全不再回调”。

## 3. 快照与提交回调契约

Save 在调用时固定 serialized_payload，需要持久化的字段必须提前填写；后续修改不会进入本次重试字节。
成功回调仍可能引用同一个 shared message，调用方应将其作为不可变快照使用；这是接口约定，不是类型系统自动禁止修改。

当前 Scene 存盘提前固定两表 DBTask 意图与字节，严格路径使用提交屏障，Redis 成功后的提交钩子才发送 Kafka。
不能再把流程写成“Save 返回后立即把当前可变消息发 Kafka”，也不能用 db 的分表回填承诺修复 Scene 父快照键；
两者键空间与提交职责不同。详见[DBTask 投递设计](./db-task-delivery-and-retention.md)与[存盘围栏](./db-write-fencing-mysql.md)。

写入按脚本缓存使用 EVALSHA；脚本尚未加载或收到 NOSCRIPT 时，可重载并回退 EVAL。
脚本重发仍携带本次快照固定的保护条件，不回头读取调用方的新状态。

## 4. 观测与生命周期

队列观测包括在途 Load、在途 Save、pending Load、pending Save、等待 Save 的 Load；
LogQueueSnapshot 在这些数量均为零时不输出，Scene 每 30 秒检查。
定位重连还要结合认证、代次与连接释放日志，不能把“connected”或队列短暂为零当作业务提交证明。

Hiredis → MessageAsyncClient → EventLoop 的释放顺序、pending 阶段销毁与回调重入约束见下面两节。
底层对象自身保活不自动保护业务回调捕获的其它 owner；业务仍须用 weak 或稳定身份管理寿命。

## 5. 验证与环境边界

本次没有检查当前监听器、Redis 数据或运行容器，不保留“Windows 服务才是真实目标、Docker 空闲”的历史环境推断。
具体连接目标以当前配置和进程身份为准。

语言服务无报错不等于编译通过。下文的专项日志、XML 与源码哈希保留原验证范围；
它们不能代替当前工作区的全量节点构建、真 Redis 资产用例或运行产物已更新的证明。

## 2026-10-20：握手截止与 socket 同批事件的生命周期修复

本节记录 2026-10-20 的 `RedisManager` 生命周期修复；文首已按 2026-10-23 源码更新接口与重试说明。

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
