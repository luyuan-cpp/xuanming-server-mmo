# 场景所有权：再入屏障与 owner_epoch

状态：再入屏障、epoch 传递及 Redis / DBTask 守卫已在 `6281e4d27` 落码。2026-10-15 本次仅按源码
修正文档，未执行构建、测试或故障演练；代码存在不等于运行期验收完成。2026-11-05：存盘直接提交 TiDB 之后，
Redis 存盘的 CAS 与补种、DBTask 的 epoch 守卫随旧路径删除，存盘守卫改为 go/db 事务里的写权行判定；§0、§5、§6
按现在的代码改写（新路径未编译、未运行）。

关联：[跨 zone 传送](cross-zone-scene-travel.md)、[玩家异步存盘窗口](player-async-save-loss-windows.md)、
[场景线程与关机](scene-node-threading-model.md)。

## 0. 目标与边界

C++ scene 是在线玩家数据的持有者；SceneManager 维护场景映射、玩家位置和负载，并负责改派。
两个进程分别观察 etcd 租约，不能依靠“收到了同一条下线信号”推导它们同时停写。

当前实现用两层约束控制旧节点与新节点重叠的窗口：Go 判死后等待再入屏障；归属变化时推进
`owner_epoch`，新持有者认领时把 TiDB 写权行抬到新代次，go/db 的存盘事务拒绝旧代次（§6.2）。
屏障只靠时间预算，进程暂停与跨主机时钟下证明不了老节点已停写；写权行不靠时间，是放行的安全性所在。
它挡得住旧写覆盖新档，挡不住老节点上没来得及提交的进度丢失（只丢不盖）；C++ 在身份不确认期间每 5 秒补存一次，
把这段丢失压到约 5 秒（[一致性保证图](data-consistency-guarantee-map-20261020.md) §6.2 第 4 条）。

## 1. 原始缺陷与当前收尾

原始缺陷是：etcd DELETE 让 Go 立即改派，C++ 老节点同时才开始存盘 drain；新节点可能先读到
旧快照，随后双方写入互相覆盖。网络只隔离 etcd、仍能访问 Redis 时尤其危险。

当前 C++ `Node::OnNodeIdConflictShutdown` 停健康检查与身份注册重试，调用 scene 的
`BeginEmergencyRelocateAll`，按“存盘完成后再请求改派”收尾。`StartConflictDrainWatchdog`
使用 steady 截止时间，最多等待 15 秒；到期会记录未完成项后退出。永久 GUID 号段与路由 node_id
分离，丢路由身份不再等价于立刻 fence 所有发号器。

Go 不再在看到 DELETE 后无条件立即接管，具体门禁见 §3.1–§3.2。

## 2. 安全目标与证据限制

目标是避免老节点可能仍在写时，新节点直接接管并从旧快照产生新状态。再入屏障只能覆盖
配置预算内的收尾与余量；极端暂停、异步在途写入和时钟差异仍须由 epoch 守卫及故障演练核实。

[网络故障与进程暂停调研](../notes/2026-09-14-network-failures-and-process-pauses.md) 与
[独立复核](../notes/2026-09-14-network-failures-and-process-pauses-verification.md) 不能为本项目
的 drain 或调度延迟提供硬上界。本次修订没有新增运行证据。

## 3. 已落地的两层门禁

### 3.1 前置：节点活性未知时不触发破坏性改派

`go/scene_manager/internal/logic/load_reporter.go::IsNodeAlive` 区分负载集命中、明确缺失和
Redis 读取失败：命中返回存活，`redis.Nil` 返回不存在，其余错误记录后按存活处理，防止
Redis 抖动触发改派、销毁或清理。歧义节点身份另有 fail-closed 检查，不能把它当作明确死亡。

测试入口包含 `TestIsNodeAliveTreatsRedisOutageAsAlive` 与
`TestIsNodeAliveReportsDeadWhenMemberMissing`；本文不新增它们的通过声明。

### 3.2 第一层：Go 再入屏障

实现位于 `internal/logic/reentry_barrier.go`，常量在 `internal/constants/reentry_barrier.go`。

| 项 | 当前默认值与约束 |
|---|---|
| C++ 身份冲突 drain 预算 | 15 秒，`Node::StartConflictDrainWatchdog` |
| Go 镜像 `CppNodeDrainBudget` | 15 秒；跨语言依赖，调整 C++ 时必须同步 |
| `ReentryBarrierClockSkewMargin` | 5 秒，预算余量，不是已证明的暂停上界 |
| `SceneReentryBarrier` | 两者相加为 20 秒；配置只能调高，低值钳回下限并报错 |
| `NodeDeathMarkTTL` | 10 分钟，须大于屏障；重新注册时显式清理 |

`markNodeDeath` 把观察到的死亡时刻写入 `node:zone:{zone}:{node}:death_at`。改派、rebalance、
孤儿收尾、缩容排空收尾的死属主分支（2026-11-05 补上，点位 `autoscale_drain`）及玩家接管等入口通过 `CanReclaimDeadNode` / `reentryBarrierBlocks` 检查：标记还在
屏障内、读失败、格式错误或时间在未来都拒绝本轮接管；合法标记已过屏障则放行。没有近期死亡
标记且不在推迟摘除队列时允许接管，不把“无标记”当永久禁止。

死亡标记写入或负载集摘除失败时，当前实现会进入推迟摘除队列（GO-6，进程内，节点留在负载集里按
存活处理）；需结合活性与领导者队列防止遗漏屏障，不能只在 `resolveScene` 一处加 sleep。常量关系由
启动检查核对，Go 与 C++ 目前仍各自持有对应预算，尚未改成读取同一配置值。

屏障压着的死节点收尾（孤儿实例强制销毁 + 计数清理）2026-10-30 起落 Redis：摘负载集与写收尾任务是
同一次 Lua（任务表 `scene_manager:dead_node_reconcile` + 判死时刻快照 `node:zone:{z}:{n}:reconcile_scenes`），
屏障过后由任何一任领导者执行；drain 有领导者闸门，跟随者不读不写；有场景没结论时只重试剩下的，计数
等收尾范围清空才与出队一起删，本进程满 10 分钟仍做不完才放弃。换领导者、进程重启都不再丢任务。
不变量 R1-I1～I4 见 `reentry_barrier.go`「被屏障推迟的死节点收尾」与
[scene-creation-architecture.md](scene-creation-architecture.md)「Node-death reconciliation」。

### 3.3 第二层：owner_epoch 与换手门

`player:{id}:owner_epoch` 是 sidecar 整数键，不放进 PlayerAllData。SceneManager 在 EnterScene
归属闸口推进它，并以同一 epoch 对 location 做 CAS；不是每次 EnterScene 都 INCR：同节点换图、
同落点重连等通常复用当前值，首次落点和满足换手门的持有者变更才铸造。

已有位置的跨节点交接要求源 scene 先冻结、存盘，再写当前代的 `player:{id}:handoff` 标记。
没有合法标记时回可重试的 `ErrHandoffPending`，不改落点。路由投递失败的回滚也保持 epoch
单调，必要时再推进并带回滚回执；源 scene 只能按专门的归属取证流程采纳，不能随意 GET 后
更新自己的缓存。细节统一见 [cross-zone-scene-travel.md](cross-zone-scene-travel.md) §12–§13。

铸出的 epoch 随 `RoutePlayerEvent` 和入场请求传给目标，目标缓存该次路由的值。若让旧路由
在加载时自行读最新 epoch，就可能与新路由同时自认持有者，因此禁止以普通读取代替路由凭证。

## 4. 文档基线与并行修改

本页记录 HEAD 已存在的实现，不再把这些项当作待拍板方案。源码定位使用路径和函数名，避免
把旧提案的行号和当时“文件干净”的判断当成当前编辑授权。实施变更前仍须检查最新工作区。
工作区 RPC channel 改造与本页的 epoch 守卫是不同事项，不据其未提交状态提升验证结论。

## 5. 当前状态与剩余验证

活性未知保护、death_at 屏障、Go 铸造/传递、C++ 缓存 epoch，以及 go/db 认领与存盘的写权行判定
（§6.2）均已有代码；剩余工作按跨区设计与故障注入手册执行，不再重复首次加字段或新增组件。

须验证的情形包括：仅隔离 etcd、旧进程暂停后恢复、屏障前后接管、路由失败回滚、迟到存盘、
新持有者认领与旧持有者存盘交错、Redis 回退后铸出更小的 epoch（应认领被拒、进场失败），以及新旧实例
并存。测试通过情况以 [PROGRESS](../PROGRESS.md) 的具体条目为准，本次没有运行这些命令。

## 6. C++ 与持久化实现

### 6.1 入场时缓存 epoch

`PlayerOwnerEpochComp` 位于 `player/comp/player_ownership_comp.h`；`player_lifecycle.cpp`
从待入场上下文接收路由 epoch，新建实体时缓存，已有实体按入场栅栏处理。普通入场路径不能
把缓存倒退；回滚后的合法采纳由专用回执和标记取证约束。

### 6.2 存盘：TiDB 写权行判定（取代 Redis CAS 与缺键补种）

2026-10-30 起存盘直接提交 TiDB（[存盘直接提交](player-save-direct-commit.md)，开关已删，没有第二条路径）。
原来的 `SavePlayerToRedisImpl`、`redis_client.h` 的 `kSaveIfGuardLuaScript` 和其中的缺键补种都已删除，
守卫改在 go/db 的 `Save` 事务里：`SubmitPlayerSave` 经 `PlayerStoreClient` 带上入场时缓存的 epoch（E）、
整笔序号和本实例号；go/db 先 `FOR UPDATE` 锁住 `player_store` 的写权行（`kind=0`），按
`go/db/internal/playerstore/decide.go` 的 `decideSave` 判定，判过才在同一事务里写数据行。与归属有关的几格：

| 库里的写权行 | 结果 | scene 处置 |
|---|---|---|
| 不存在 | `NO_OWNER` | 这笔不写；保留实体，和结果未知一样退避重试，ERROR 节流 |
| 代次大于 E | `STALE_OWNER` | `HandlePlayerSaveRejected` |
| 持有者不是本实例 | 同代次回 `NOT_HOLDER`，E 更大回 `UNCLAIMED` | 同上 |
| 其余 | 再按序号、世代判，见规格 §4 | — |

新持有者进场时，`LoadAndClaim` 在同样锁住写权行的事务里把代次抬到路由带来的 E、持有者改成自己，所以旧持有者
在新持有者第一次存盘之前就被挡住。以前这段窗口只靠 Redis Lua 的 owner 比对，外加靠时间的再入屏障。

**为什么不再补种**：补种是给 Redis 被清空、guard 键丢失时留的例外，不补就会把在线玩家全部判成旧主；代价是被废黜的
僵尸节点若先补种了旧值，合法持有者反而被拒。写权行和存档在同一个库、同一笔事务里，不会单独丢失；它在建角时就由
data_service 写好（E=1、`released=1`、持有者为空），缺行只说明角色不存在或还没建完，存盘不造行，这条竞争也就不存在了。
`player:{id}:owner_epoch` 仍在 Redis，由 scene_manager 铸造，只是候选值：Redis 回退之后新铸的值取墙钟，通常仍比库里大；
万一更小，认领被拒、进场失败，不会覆盖库里更新的档（规格 §4「Redis 回退」）。

被拒之后的处置不变：`HandlePlayerSaveRejected` 按被拒那一笔的代次判断。实体缓存的代次已经变了，就用新代次重存，
迟到的拒绝不会误删合法实体；没变就说明本节点已被废黜，`DestroyDeposedPlayer` 丢掉本地状态，不重试、不改派。
E=0 的无守卫存盘也已删除，go/db 在进事务之前就回 `INVALID`。

旧路径的 Lua 原文和 2026-10-16 的 `dirty_keys_set` 更正，见本文 2026-11-05 改写之前的 git 历史。

### 6.3 只有一个提交点

原来的 DBTask → Kafka → go/db 写 MySQL 那条链（`key_ordered_consumer.go` 的 epoch 与 applied-seq 守卫）随旧存盘
路径一起删除，`go/db/internal/kafka` 已不存在。epoch 现在只在 §6.2 那笔 TiDB 事务里比对一次，存盘的提交点就是
这笔事务，没有「Redis 写成功、SQL 还没落地」的中间态。交接标记、资产通道与收件箱的确认都以它的结果为准
（规格 §6）。

### 6.4 维护与验证入口

- C++：`cpp/libs/services/scene/player/system/player_lifecycle.cpp`、`player_store_client.{h,cpp}`、
  `player_ownership_comp.h`。
- Go：`go/scene_manager/internal/logic/reentry_barrier.go`、`owner_epoch.go`、
  `go/db/internal/playerstore/decide.go`、`store.go`。
- 真实环境：[跨区故障注入手册](../ops/cross-zone-failure-test-runbook.md)。

本页不替代上述模块的单测、串行 C++ 构建及真实故障验收记录。


### 6.5 入场授权的单次协程（2026-11-05）

`RoleAdmissionSystem` 将一次登记表达为 SQL Register → 按需读取旧 handoff 资格 → 原条件写入 → 激活。Scene 持久任务作用域负责运行；等待期间保存玩家身份、代次和自有资格字节，恢复后复核原授权状态。Seal、Close、Ack、交易封存、原 Tick 重试与截止时间继续由原状态机裁决。

本地停止与传输终态分开记账。停止不冒充 SQL/Redis 完成；已接纳操作的真实终态通过原作用域弱身份及单次完成标志归还登记/围栏在途数，晚结果不得重新激活或发 Ack。读取完成但尚未发 CAS 时取消，可归还该读尝试。这样最终拆除的取消不会制造“已经排空”的假象，也不会让重复回包重复减账。线程与停止顺序见[线程模型](scene-node-threading-model.md)。本批不改变 Java 可观察协议。
