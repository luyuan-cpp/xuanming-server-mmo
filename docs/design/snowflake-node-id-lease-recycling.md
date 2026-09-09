# SnowFlake Node ID Lease-Based Recycling

> **2026-09-08:本文已作废,仅存历史。** 两侧都不再用"hostname 双 key / 最小空闲位 / lease 回收"这套:
> 现状(最久未用 + 隔离期、持久水位墓碑、按水位年龄自 fence、lease 只做活性)见
> [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md) 与 [snowflake-id-allocation.md](./snowflake-id-allocation.md)。
> 下文"Correctness: Equal"那张表在 2026-07-29 就被证伪,现在整套比较对象都不存在了。

**Date:** 2026-04-15

## Problem

The original `mustAllocNodeID` in the Go scene_manager used a monotonically incrementing etcd counter and permanent hostname keys. Node IDs were never released:

- Counter only went up; dead hostname keys stayed in etcd forever.
- K8s Deployments with random hostnames (e.g. `sm-7f8b9c-xk2jf`) would consume a new ID on every pod recreation.
- Eventually exhausts the 17-bit node ID pool (131072 IDs).

StatefulSets with stable hostnames were safe (restart = reuse), but Deployments were not.

## Solution: Lease-Based Dual-Key Recycling

### Key Layout

Both keys share a single etcd lease (60s TTL, background KeepAlive):

| Key | Value | Purpose |
|-----|-------|---------|
| `/scene_manager/snowflake_nodes/{hostname}` | id | Hostname -> ID (restart idempotency) |
| `/scene_manager/snowflake_ids/{id}` | hostname | ID -> Hostname (uniqueness index, recycling) |

### Allocation Flow

1. **Reclaim**: If hostname key already exists, CAS-refresh both keys with new lease (restart-safe).
2. **Scan**: Collect all in-use IDs from both `snowflake_ids/` (new keys) and `snowflake_nodes/` (old permanent keys for backward compat).
3. **Pick**: Find smallest free ID in [0, 131071].
4. **Claim**: CAS create both keys atomically. If another instance raced, retry with fresh scan.

### Recycling

Pod dies -> KeepAlive stops -> lease expires after 60s -> both keys auto-deleted -> ID is free.

### Backward Compatibility

- Old permanent hostname keys (from before this change) are scanned into `usedIDs` to avoid collision.
- Old `snowflake_counter` key remains in etcd but is harmless (no longer read).

## C++ Node ID Allocation — Comparison

The C++ side (`node_allocator.cpp` → `AcquireNode()`) already supports natural recycling:

1. Reads a **local snapshot** of live nodes (populated by etcd watch).
2. Picks `maxUsedId + 1` or the smallest gap.
3. CAS `PutIfAbsent` to etcd with the **node's lease**.
4. Node dies -> lease expires -> key auto-deleted -> ID is free in next snapshot.

**Key difference:** C++ doesn't use hostname-based identity. It picks from a live snapshot every time.
Both approaches are now safe for ephemeral K8s pods.

### Why C++ doesn't need the hostname pattern

The C++ node's etcd key includes `node_type` and `node_id` (not hostname), and the etcd watch gives every node a real-time view of which IDs are alive. Since the key is lease-bound, dead nodes' keys vanish automatically, and a restarting node just grabs the next free slot. There is no "stale key" problem because the lease is the garbage collector.

## Safety Analysis

| Scenario | Go (new) | C++ |
|----------|----------|-----|
| Same hostname restart | Reuses same ID (idempotent) | Reuses if slot is still free (lease may not have expired yet — SnowFlake guard protects) |
| New random hostname | Picks smallest free recycled ID | Picks smallest free from live snapshot |
| Pod crash (no graceful shutdown) | Lease expires in 60s, ID freed | Lease expires per config TTL, ID freed |
| K8s rolling update | Old pod's lease expires, new pod may get same or different ID | Same behavior |
| Pool exhaustion | Impossible in practice (IDs recycle) | Impossible in practice (IDs recycle) |

## Design Decision: Keep Both Approaches

Considered unifying Go to use C++ pure-scan approach. Decided to **keep both as-is**.

### Trade-off Comparison

| | C++ (pure scan slot) | Go (hostname + ID dual key) |
|---|---|---|
| Code | Simpler (one key) | Slightly more (two keys) |
| Restart identity | Not guaranteed same ID | Same hostname -> same ID |
| Log correlation | ID may drift across restarts | Stable ID, easier to grep |
| Extra protection | Has `SnowFlakeGuard` (Redis SETEX 600s) | ~~Not needed -- hostname key is the guard~~ **(错，见下)** → 生成器内置启动 guard |
| Correctness | Equal | Equal |

> ### ⚠️ 2026-07-29 更正：「hostname key 就是 guard」是反的
>
> hostname 亲和保证的是"同一台机器重启**必然**拿回同一个 worker id"。它不但不构成 guard，
> 反而把撞号从偶然变成必然 —— 叠加另外三件事：
> - `Handle.Close()` / `internal/node.Close()` 退出时立刻 `Delete` + `Revoke`，worker id 秒级可复用；
> - `shared/snowflake` 是**秒级**时间戳；
> - 新进程的 `Node` 从 `lastTime=0, step=0` 重新开始。
>
> 结果：进程在同一日历秒内重启，新老两个进程发出的号**逐位相同**。已用探针实测确认
> （同秒构造两个 `Node(7)`，第一个 ID 都是 `50960585831186432`）。
>
> **修法**：`snowflake.NewNode` 内置启动 guard —— 构造时把 `lastTime` 置为当前秒、`step` 置满，
> 于是第一个 ID 一定落在下一秒。语义与 C++ 的 `SetGuardTime(now)` 完全一致，
> 代价是第一个 ID 最多晚 1 秒。回归用例见 `go/shared/snowflake/snowflake_test.go`
> 的 `TestNewNode_BootGuardSkipsRestartSecond`。
>
> **另一个缺口(同日修复)**：`snowflakealloc` 的 KeepAlive goroutine 原来只 drain 响应，
> 租约真丢了也只打一条 ERROR，进程**继续用那个 worker id 发号**，而 etcd 已经可以把它
> 分给别人 —— 启动 guard 挡不住这种情况（它只覆盖"旧进程已退出"的窗口）。
> 现在 `Handle.Lost()` 会在失租时关闭，`guild` / `scene_manager` 收到后 `s.Stop()` 停服，
> 由编排重拉（重启拿新租约 + 启动 guard 兜底）。C++ 侧对应的是
> `OnNodeIdConflictShutdown` 里的 `SnowFlakeManager::Fence()`。

### Why not change Go to match C++

- Go has no etcd watch snapshot -- `mustAllocNodeID` runs once at startup, must self-scan.
- Losing hostname idempotency degrades observability with no offsetting benefit.
- Dual-key code is already written, compiled, and clear.
- ID 复用窗口的防护现在由**生成器内置的启动 guard**承担（见上），与用哪种 key scheme 无关。

### Why not change C++ to match Go

- C++ already has watch-maintained live snapshot -- hostname mapping is redundant.
- Adding hostname key + dual-key CAS would increase code complexity with zero benefit.
- `SnowFlakeGuard` already covers the restart-reuse safety window.

### Conclusion

Each language uses the approach that best fits its runtime model. Both are safe for ephemeral K8s pods. No unification needed.
