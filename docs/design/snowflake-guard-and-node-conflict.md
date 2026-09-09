# SnowFlake Guard & Node ID Conflict Handling

> **2026-09-08 起本文大部分已被 [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md) 取代。**
> 落码后的现状(与下文旧描述不同的地方):
> - 发号 worker 与路由 node_id **已解耦**:scene 的 item/tx/snapshot 发号器从 `SnowflakeSlotClient` 拿槽位
>   (`/snowflake/scene-item/c<cluster>/slots/<slot>`),worker = `(cluster<<12)|slot`;路由 node_id 只做 Kafka topic / 会话反查。
> - **Redis guard(下文"Layer 3")已删除**:水位改写 etcd `/snowflake/<kind>/c<cluster>/watermark/<slot>`(无 lease),
>   值 = `max(墙钟, 逻辑高水位)+2s`,与槽位申领在同一个 txn 里原子落第一笔。过渡期仍读旧 Redis guard 取 max,一个版本后删。
> - 申领算法从"最小空闲位"改为"最久未用 + 隔离期 Q=4h";持有者按水位年龄自 fence(F=2h,判定在 `Generate()` 内)。
> - **keepalive TTL<=0 / 本地 lease 超时不再 fence、不再退出**:走重注册 + 重挂槽位;只有 alloc key 被别的 uuid 抢走才走
>   `OnNodeIdConflictShutdown`,只有 `slots/<slot>` 被别的 uuid 持有才永久 `Fence()`。`NodeIdConflictReason` 的前两个枚举值保留但不再触发。
> - 验收里 "etcdctl lease revoke" 的预期从"fence + 改派 + 退出"改为"重注册 + 重挂,无 fence";日志多了 `worker=c<cluster>:<slot> inc=<rev>`。
> 下文保留作历史与四层防护的推导过程。

## Problem Statement

When a node experiences network partition:
1. Node A (node_id=5) loses connectivity → etcd lease expires → key deleted
2. Node B starts, CAS succeeds → also gets node_id=5
3. Danger window: both A and B hold same node_id → SnowFlake ID collision

SnowFlake ID layout: `[time:32][node_id:17][step:15]` — same node_id in same second = duplicate IDs.

## Solution: Redis SnowFlake Guard (Zero-Delay Startup)

### Design Principle
**Node restart must be instant.** No delayed startup. Player experience should feel like "reconnecting a cable."

### Write Path (Heartbeat)
Every keepalive heartbeat writes a Redis guard key:
```
SETEX snowflake_guard:{node_type}:{node_id} 600 {now_utc_seconds}
```
- TTL = 600 seconds (10 minutes), fixed — long enough for any restart scenario
- Value = UTC timestamp of last heartbeat
- When node dies, no one renews → key auto-expires

> **2026-07-29 修正 — key 不再带 zone_id。** node_id 的唯一域是
> `MakeNodeAllocationKey` 用的全局 `(node_type, node_id)`（跨 zone 抢同一个 id 只有一个赢）。
> guard key 原来带了 `zone_id`，于是"zone 1 的 node_id=3 退出、zone 2 抢到 node_id=3"这种
> **正常的跨 zone 回收**会读到一把不存在的 guard，新持有者直接从 step=0 发号。

### Read Path (New Node Startup)
After CAS success in etcd:
1. `GET snowflake_guard:{node_type}:{node_id}`
2. **无条件** `SetGuardTime(max(now_utc, lastTs))` → SnowFlake 跳过当前秒
3. Immediately start RPC server — zero delay

> **2026-07-29 修正 — guard 从"条件生效"改成"无条件生效"。** 原实现有两条完全不 guard 的路径：
> ① Redis 没连上时直接 `OnNodeStart` 就开发号；② guard key 不存在时（跨 zone 回收、
> Redis 被清、guard 写入失败）也直接开发号。这两条路径下只要上一任在同一日历秒里发过号，
> 新持有者从 step=0 重发的号就与它**逐位相同**。现在的规则是：
> - **底线永远是本机当前秒**，Redis 挂了也一样 guard；
> - 读到上一任写的 `lastTs` 时取 `max(lastTs, now)`，覆盖"旧持有者所在机器时钟比本机快"。
>
> 代价是本进程第一个 ID 最多晚 1 秒发出。原文档"lastTs 在过去所以没用"的判断只在
> 同机同步时钟的前提下成立；跨机时钟偏斜时 `lastTs > now` 是可能的，所以取 max 而不是丢弃。

> **guard 仍有一个已知边界**：guard 值写在**分 zone 的 Redis 实例**里，所以跨 zone 回收
> node_id 时新持有者依然读不到旧值。这条路径由上面"无条件按本机当前秒兜底"覆盖 ——
> 它挡得住"旧进程已退出"的重启窗口，挡不住"旧进程还活着且时钟超前"。后者由
> `OnNodeIdConflictShutdown` 的 fence（见下）在**旧进程侧**关掉。

### Why This Is Safe
- etcd CAS success = old node's lease has expired = old node is network-isolated
- Network-isolated node: can't receive RPC, can't write to DB → its SnowFlake IDs exist only in local memory, cannot persist
- SetGuardTime(now) skips current second → even if old node survives HCI more seconds, its IDs can't escape

### Code Locations
| File | What |
|------|------|
| `cpp/libs/engine/core/utils/id/snow_flake.h` | `SetGuardTime()` method |
| `cpp/libs/engine/threading/snow_flake_manager.h` | `SetGuardTime()` pass-through |
| `cpp/libs/engine/core/node/system/etcd/etcd_manager.cpp` | `WriteSnowFlakeGuard()` — heartbeat writes |
| `cpp/libs/engine/core/node/system/etcd/etcd_service.cpp` | `ActivateSnowFlakeAfterGuard()` — read + activate |

## OnNodeIdConflictShutdown：fence → 存盘 → 改派 → 有界退出

```cpp
enum class NodeIdConflictReason {
    kLeaseExpiredByEtcd,     // keepalive returned TTL=0
    kLeaseDeadlineExceeded,  // local health check: no ACK within TTL
    kReRegistrationFailed,   // re-register CAS 失败 / Watch 发现 node_id 被抢
};

virtual void OnNodeIdConflictShutdown(NodeIdConflictReason reason);
```

### 2026-07-29 前的行为（错的，已改）

四个触发点都是「跑一次业务 hook，然后 `LOG_FATAL`」。muduo 的 `LOG_FATAL` 在语句结束时
`abort()`，而 hook 里的存盘是**异步**的（Redis 命令还在发送缓冲、DBTask 还在 Kafka producer
队列），于是**每一次失约都会丢掉这一批玩家的存盘**。同时玩家的 gate 会话还连着，
指向一具尸体，只能等自己发现掉线再重登。

### 现在的顺序（`Node::OnNodeIdConflictShutdown` + `StartConflictDrainWatchdog`）

1. **幂等门**：四个触发点会重复触发（keepalive 定时器还在跑），只走一次。
2. **立刻 fence 发号器**（`SnowFlakeManager::Fence()`）。etcd 已经可以把 node_id 交给别人，
   再发一个号就是确定性撞号。fence 必须在业务收尾**之前** —— 收尾只需要存盘和路由，不需要新 ID。
   fence 之后 `GenerateItemGuid()` 返回 `kInvalidGuid`，`SnapshotSystem` / `TransactionLogSystem`
   随之 fail-closed（宁可少一条快照，也不写 `snapshot_id=0` 去和别人的 0 号互相覆盖）。
3. 停掉 health monitor / allocator 重试定时器，避免收尾期间又跑一遍注册流。
4. **业务收尾 hook**：
   - **SceneNode** → `PlayerLifecycleSystem::BeginEmergencyRelocateAll()`
     （抄会话票据 → 存盘 → **存盘落地后**请求 `SceneManager.EnterScene(scene_id=0, scene_conf_id=0)`
     把玩家改派到存活节点上的大世界频道 → 摘会话 → 销毁本地实体）。
     顺序不能反：先改派会让新节点从 Redis 读到存盘前的旧数据（回档）。
     副本节点挂和大世界节点挂走**同一条路径**，落点都是大世界。
   - **GateNode** → 断开全部客户端 TCP 连接（同步，无需 drain）。
5. **有界 drain**：`conflictDrainTimer` 每 100ms 轮询 `SetConflictDrainComplete` 谓词，
   全部落地或 **15s 预算**到期就走正常 `Shutdown()`（跑 before-shutdown hook、关 gRPC、
   flush Kafka producer、释放 etcd 租约、quit loop）—— 这些正是 `abort()` 会跳过的东西。
   到期未落地会打 ERROR，不会伪装成成功。

## Four-Layer Protection Summary

| Layer | Mechanism | What It Prevents |
|-------|----------|-----------------|
| 1. etcd CAS | 全局 `(node_type,node_id)` PutIfAbsent | 两个健康节点抢到同一个 id |
| 2. Lease TTL | keepalive TTL=0 / 本地 deadline → **fence + 有界 drain** | 失约的旧进程继续发号 |
| 3. Redis Guard | 启动时**无条件** `SetGuardTime(max(now,lastTs))` | 同秒重启窗口的重号 |
| 4. Generator | high-water 只进不退；时钟回拨不自旋、不 abort | 时钟异常导致的重放 |

## Design Iteration History

| Version | Approach | Outcome |
|---------|----------|---------|
| v1 | Delay startup by safety window | **Rejected** — node restart must be instant |
| v2 | SetGuardTime(lastTs) from Redis | **Wrong** — 只看 lastTs，同机时钟下它在过去 |
| v3 | SetGuardTime(now)，且仅在 guard key 存在时 | **不够** — Redis 挂 / 跨 zone 回收时完全不 guard |
| v4 (2026-07-29) | 无条件 `SetGuardTime(max(now,lastTs))`，key 去掉 zone 段 | **Final** — zero delay，无 guard 缺口 |

## 验收（故障注入，必须真集群跑一次）

静态编译和单测只覆盖了发号器本身。**失约 drain 这条链目前零运行时证据**，上线前必须跑：

前置：起 etcd / redis / kafka / mysql + 1 个 scene_manager + 1 个 gate + **2 个 scene 节点**
（一个大世界 purpose、一个准备被杀），用 robot 或客户端进 3~5 个玩家到被杀的那个节点。

注入：对被杀节点的 lease 直接 revoke（不要 kill 进程 —— kill 测不到本改动）：
```
etcdctl lease list
etcdctl lease revoke <被杀节点的 leaseID>
```

判定（五条全过才算通过）：
1. 日志出现 `Fencing ID generation, persisting and relocating players`，且**之后不再出现任何新发的 item/snapshot guid**；出现 `GenerateItemGuid after fence` 属预期。
2. 进程在 **15s 内**走完 `Conflict drain complete, shutting down`（或到期的 `budget exceeded` ERROR）后**正常退出**，日志里**没有** abort/崩溃栈。
3. 每个玩家日志成对出现 `[SavePlayerToRedis] ... saved to Redis` → `[EmergencyRelocate] requested main-world re-home`，顺序不能反。
4. 玩家在另一台 scene 节点上重新出现在大世界频道，`player:{id}:location` 指向新 node，且**背包 / 等级 / 坐标与被杀前一致**（回档即失败）。
5. 用第二个玩家重复一次，但让它在被杀前处于**完全静止**状态（触发 dirty-save 快路径）：必须同样被改派，不能卡在原地。

回归对照：修复前跑同一脚本，第 2、3、4 条必然失败（进程 abort、无改派日志、玩家掉线）。
