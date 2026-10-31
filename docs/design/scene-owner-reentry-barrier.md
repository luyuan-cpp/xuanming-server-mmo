# 场景所有权：再入屏障与 owner_epoch

状态：再入屏障、epoch 传递及 Redis / DBTask 守卫已在 `6281e4d27` 落码。2026-10-15 本次仅按源码
修正文档，未执行构建、测试或故障演练；代码存在不等于运行期验收完成。

关联：[跨 zone 传送](cross-zone-scene-travel.md)、[玩家异步存盘窗口](player-async-save-loss-windows.md)、
[场景线程与关机](scene-node-threading-model.md)。

## 0. 目标与边界

C++ scene 是在线玩家数据的持有者；SceneManager 维护场景映射、玩家位置和负载，并负责改派。
两个进程分别观察 etcd 租约，不能依靠“收到了同一条下线信号”推导它们同时停写。

当前实现用两层约束控制旧节点与新节点重叠的窗口：Go 判死后等待再入屏障；归属变化时推进
`owner_epoch`，C++ Redis 存盘与 Go db 持久化路径拒绝已知旧 epoch。屏障的时间预算、进程暂停、
跨主机时钟及 guard 缺键例外仍有条件，不能把这两层描述为任意故障下的绝对保证。

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
孤儿收尾及玩家接管等入口通过 `CanReclaimDeadNode` / `reentryBarrierBlocks` 检查：标记还在
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

活性未知保护、death_at 屏障、Go 铸造/传递、C++ 缓存和 guarded Save、DBTask 携带 epoch 均已
有代码；剩余工作按跨区设计与故障注入手册执行，不再重复首次加字段或新增组件。

须验证的情形包括：仅隔离 etcd、旧进程暂停后恢复、屏障前后接管、路由失败回滚、迟到存盘与
DBTask、Redis 清空后的补种竞争、旧版本 epoch=0 兼容，以及新旧实例并存。测试通过情况以
[PROGRESS](../PROGRESS.md) 的具体条目为准，本次没有运行这些命令。

## 6. C++ 与持久化实现

### 6.1 入场时缓存 epoch

`PlayerOwnerEpochComp` 位于 `player/comp/player_ownership_comp.h`；`player_lifecycle.cpp`
从待入场上下文接收路由 epoch，新建实体时缓存，已有实体按入场栅栏处理。普通入场路径不能
把缓存倒退；回滚后的合法采纳由专用回执和标记取证约束。

### 6.2 Redis 存盘：原子 CAS 与缺键补种例外

`SavePlayerToRedisImpl` 对非零 epoch 调 `MessageAsyncClient::Save(message, key, guardKey,
guardExpected)`；`redis_client.h::kSaveIfGuardLuaScript` 的当前规则为：

```lua
local cur = redis.call('GET', KEYS[2])
if cur == false then
    redis.call('SET', KEYS[2], ARGV[2])
elseif cur ~= ARGV[2] then
    return 0
end
redis.call('SET', KEYS[1], ARGV[1])
return 1
```

**2026-10-16 更正**:上面代码块原先在 `SET KEYS[1]` 之后还有一行 `redis.call('SADD', 'dirty_keys_set', KEYS[1])`。
该全服集合没有任何读者、随累计存盘玩家数无界增长,且是脚本未在 KEYS[] 声明的键(挡 Redis Cluster),
已从源码删除;无 guard 的普通存盘脚本同步删去同一行并改名为 `kSaveLuaScript`。两段脚本现在只访问
KEYS[] 声明的键,返回值与 CAS / 补种语义不变。原因见 [capacity-model-hok-scale.md](./capacity-model-hok-scale.md) A0-4。

与最初“缺键也拒绝”的提案有意不同：guard 缺失时补种调用方的期望值，再在同一 Lua 内写入，
避免 Redis 清空后把在线玩家内存中剩余的状态全部当作旧主丢弃。**风险仍在**：清空时若被废黜
的僵尸节点先补种旧值，合法持有者随后会被拒；再入屏障和节点收尾只能降低这项竞争风险。
这条既有行为与风险已写在源码，本次只同步文档，未改变或重新批准其故障策略。

键存在且不相等时走 `HandlePlayerSaveRejected`，不继续旧主存盘/改派；回调还须按被拒那次的
期望值判断，不能因迟到回调误删已更新代次的合法实体。epoch=0 仍走无 guard 的兼容 Save，
并计入 unknown 统计，不能声称旧版本窗口也获得相同保护。

### 6.3 Kafka → MySQL 也携带 epoch

C++ 构造各分表 `DBTask` 时填写 `owner_epoch`；`go/db/internal/kafka/key_ordered_consumer.go`
包含 epoch 与 applied-seq 守卫，按已应用 epoch 拒绝已知旧代任务。Redis 写与 Kafka/MySQL 写
是不同原子范围，不能把 Redis CAS 成功、Kafka ACK 或 offset 提交等同于 MySQL 已落地；
完整持久化验收见跨区设计及存盘管线交接。

### 6.4 维护与验证入口

- C++：`cpp/libs/services/scene/player/system/player_lifecycle.cpp`、`player_ownership_comp.h`、
  `cpp/libs/engine/infra/storage/redis_client/redis_client.h`。
- Go：`go/scene_manager/internal/logic/reentry_barrier.go`、`owner_epoch.go`、
  `go/db/internal/kafka/key_ordered_consumer.go`。
- 真实环境：[跨区故障注入手册](../ops/cross-zone-failure-test-runbook.md)。

本页不替代上述模块的单测、串行 C++ 构建及真实故障验收记录。
