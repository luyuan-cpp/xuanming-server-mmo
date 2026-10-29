# Scene Creation Architecture

**Updated:** 2026-08-03 (rev 4 — zone-scoped node identity + safe EnterScene commit)

> 2026-10-15 修订：世界频道按 `(zoneId, confId)` 的 Redis SET 保存，多频道各有独立 scene_id；同步创建、收敛与幂等示意，依据 `world_init.go`、`world_autoscale.go` 与 C++ `SceneHandler::CreateScene`。

## Overview

The scene system supports two scene types managed by Go SceneManager and hosted on C++ SceneNodes:
- **Main World** scenes：每个 zone 的每个配置可有多个持久频道，启动时按期望频道数补建；每个频道有独立 scene_id。
- **Instance** scenes: on-demand, lifecycle-managed, auto-destroyed when idle.

### Node Role Separation (WoW-style)

Each C++ scene node declares a **role** at startup via
`GameConfig.scene_node_type` (proto `eSceneNodeType`, mirrored by the Go
constants `SceneNodeType*`):

| Value | Constant | Role |
|------|----------|------|
| 0 | `kMainSceneNode` | hosts persistent main-world channels |
| 1 | `kSceneNode` | hosts on-demand instances / dungeons / mirrors |
| 2 | `kMainSceneCrossNode` | cross-server main-world |
| 3 | `kSceneSceneCrossNode` | cross-server instance |

The role is published into etcd as part of `NodeInfo`, mirrored by
`LoadReporter` into `node:zone:{zoneId}:{nodeId}:scene_node_type`, and consumed by
`getNodesForPurpose` (Go) to route world vs. instance creation to the
correct pool. This matches the classic industry split — WoW has dedicated
"world server" vs. "instance server" processes, FFXIV / TW3 do the same.

部署侧怎么落成两个池(`scene-world` / `scene-instance` 两个 Deployment、
`SCENE_NODE_TYPE` env 覆盖、legacy `scene: N` 的兼容规则)见
`docs/ops/scene-node-role-split.md` §3.2 / §3.2.1,实现在
`tools/scripts/k8s_deploy.ps1::Resolve-SceneDeploymentPlan`。

**StrictNodeTypeSeparation** (config flag):
- `true` (production default) — purposes must match. No instance-hosting
  node in a zone? Instance creation returns `ErrNoNodeForPurpose`.
- `false` (dev / single-node) — the selector falls back to the whole
  zone pool when the preferred pool is empty.

**Mirror co-location intentionally bypasses the filter.** The whole
point of mirrors is reusing the source scene's resident map/AI/spawn
data; forcing a mirror of a main-world scene onto a fresh instance node
defeats the optimization. Operators still have an escape hatch via
`TargetNodeId`.

### Zone-Agnostic Design

SceneManager is **stateless and horizontally scalable**. It does not bind to any specific zone:
- Zone information flows through **RPC requests** (`zone_id` field), not config.
- `LoadReporter` uses **list-watch** (etcd Watch) to reactively discover scene node changes. No polling.
- On Watch PUT (node appeared) → triggers `initWorldScenesForZone`. On Watch DELETE → grace period then cleanup.
- `fullSync` on startup / watch reconnect does full etcd Get, rebuilds `knownNodes` baseline.
- `InstanceLifecycleManager` iterates over `GetActiveZones()` when cleaning up idle instances.
- Multiple SceneManager instances can run behind a load balancer, each serving any zone.

## Scene Types

| Type | Constant | Behavior |
|------|----------|----------|
| Main World | `SceneTypeMainWorld=1` | 持久频道；`initWorldScenesForZone` 按期望数补建，CreateScene 返回可用频道。频道自动扩缩容见 [world-channel-autoscale.md](./world-channel-autoscale.md)，不走普通副本的空闲超时销毁。 |
| Instance | `SceneTypeInstance=2` | On-demand. Unique per request. Tracked in Redis sorted set. Auto-destroyed after `InstanceIdleTimeoutSeconds` (default 300s) with 0 players. Mirror instances (`MirrorConfigId > 0` or `SourceSceneId > 0`) use the shorter `MirrorIdleTimeoutSeconds` (default 30s) — see [Mirror vs. instance idle timeouts](#mirror-vs-instance-idle-timeouts). |

Auto-detection: if `scene_type` is unset in the request, the system checks `WorldConfIds` config. If the `scene_conf_id` is in that list → main world; otherwise → instance.

## Create Flow

```
Client/System → Go SceneManager.CreateScene(scene_conf_id)
│
├─ resolveSceneType (auto-detect or explicit)
│
├─ Main World path:
│  ├─ GetBestWorldChannel: SMEMBERS world_channels:zone:{zoneId}:{confId}
│  │    └─ 已有可用频道 → 返回当前人数最少的频道
│  ├─ 无可用频道 → initWorldScenesForZone（持 zone 补建锁）
│  │    ├─ DesiredWorldChannelCount + SMEMBERS → 只分配缺少的频道
│  │    ├─ getNodesForPurpose(World) → SceneIDGen.Generate
│  │    ├─ SET scene:{id}:node / zone，SADD world_channels:zone:{zoneId}:{confId} sceneId
│  │    └─ 对全部频道发 CreateScene(scene_id, config_id)，C++ 按 scene_id 幂等
│  └─ 再次 GetBestWorldChannel → 返回可用频道，否则回错误
│
└─ Instance path:
   ├─ pickInstanceNode:
   │    1. TargetNodeId (explicit override)
   │    2. Mirror co-location (bypasses purpose filter)
   │    3. GetBestNodeForPurpose(Instance)  # filters by scene_node_type
   ├─ allocateScene → SceneIDGen.Generate(Snowflake), SET scene:{id}:node
   ├─ ZADD instances:zone:{zoneId}:active (score = timestamp)
   ├─ SET instance:{id}:player_count = 0
   └─ RequestNodeCreateSceneWithOptions → gRPC → C++ Scene.CreateScene(config_id, mirror_config_id, creator_ids)
```

### Channel count overrides

The default channel count for a main-world confId comes from
`WorldChannelCount`. Hot maps can be bumped via `WorldChannelCountByConfId`:

```yaml
WorldChannelCount: 1
WorldChannelCountByConfId:
  1001: 4     # capital city — 4 parallel copies
  1010: 1     # tutorial — only one
```

`Config.ChannelCountFor(confId)` 是首次播种的频道数。`DesiredWorldChannelCount` 将其种入 `world_channels:desired:zone:{zoneId}`；之后补建以该 Redis Hash 中的期望数为准，自动扩缩容也修改它，避免缩容后又被静态配置补回。活动频道本身保存在另一族 SET：`world_channels:zone:{zoneId}:{confId}`。

### Composite load score

The per-node load score in `scene_nodes:zone:{zone}:load` is now a
weighted sum of two counters:

```
score = α · scene_count + β · player_count
α = NodeLoadWeightSceneCount  (default 1.0)
β = NodeLoadWeightPlayerCount (default 0.01)
```

Defaults treat one scene ≈ 100 players — an empty scene still burns CPU
on tick/AOI scaffolding, so it dominates. Shift β up if your scenes are
cheap but players are expensive (heavy physics, per-player AI).

Counters are maintained in Redis as `node:zone:{zoneId}:{nodeId}:scene_count` and
`node:zone:{zoneId}:{nodeId}:player_count`. Node IDs are only unique inside a
zone, so no node-scoped key or connection cache may omit `zoneId`. The player aggregate is piggy-backed on the
existing `IncrInstancePlayerCount` / `DecrInstancePlayerCount` hooks:
each scene enter/leave also touches the per-node counter, and
`destroyInstance` drains the residual so force-destroys don't leak
ghost players.

谁、多久把这两个计数折成负载分写进负载集:见下文「负载刷新与 zone 登记集合(2026-10-16)」(只有领导者按 zone
批量刷新;计数读失败不按 0 写)。

### Rebalance on node-set change

When a world-hosting node joins, leaves, or role-flips, SceneManager runs
`RebalanceWorldChannelsForZone` to move channels toward the new ideal
hash distribution (`assignNodeByHash(sceneId, sortedWorldNodes)`).

Two safety rules keep this cheap:

1. **Urgent recovery** (`reasonNodeGone`) — a channel whose old process is
   confirmed absent from the zone load set is recreated on a live node. This
   restores the scene mapping; it is **not** live player handoff. Players whose
   old process died reconnect through the normal login path. If the old process
   is still registered but merely changed to a non-world role, a non-empty
   channel is left in place until it drains; moving it hot would require the
   cross-node state-transfer protocol that is not implemented.
2. **Opportunistic migration** (`reasonBetterHome`) — channels on a live
   node but mapped to a non-ideal hash target are moved only when empty
   (`player_count == 0`). Hot channels stay put until they naturally
   drain; live player migration across nodes is out of scope and would
   need cross-node state transfer the codebase does not have.

`MaxRebalanceMigrationsPerTick` (default 10, 0 disables) caps moves per
event so a four-pod scale-up does not trigger N·channels of simultaneous
CreateScene/DestroyScene RPCs. Startup also calls Rebalance from
`fullSync` so drift accumulated during SceneManager downtime converges.

The planner (`PlanWorldChannelRebalance`) is split from the executor so
dashboards / admin RPCs can expose "pending migrations" without
triggering them, and so unit tests cover selection logic without a real
gRPC scene-node fleet.

End-to-end coverage of the executor (dial → `CreateScene` / `DestroyScene`
→ Redis mapping updates) lives in
`go/scene_manager/internal/logic/integration_test.go` (`TestIntegration_*`),
using in-process bufconn fake scene nodes and miniredis. Operational
context, alerts, and the CI checklist are in `docs/ops/scene-node-role-split.md`
(§8 "Verifying the pipeline in CI").

### EnterScene ownership and Gate-route commit

`EnterScene` treats the existing `player:{id}:location` as ownership state, not
as a best-effort hint:

- It reads and decodes the existing location before reserving a target. A Redis
  read or protobuf decode failure rejects the request without changing counts.
- Scene-node identity is `(zoneId,nodeId)`. Redis node keys and the gRPC cache
  include both fields; multiple live registrations for the same identity are
  ambiguous and fail closed before RPC or Kafka routing. An ambiguous mapping
  is not treated as a dead node and must not trigger automatic ownership
  reassignment.
- First landing and same-physical-node scene switches remain enabled. An
  existing player's cross-node/cross-zone handoff is rejected by default with
  `ErrUnsafeCrossNodeHandoff` until a per-player persistence epoch is available.
  `AllowUnsafeCrossNodeHandoff` is a development-only compatibility switch.
  > **Superseded (2026-09-20)**: the sentence above is the rev 4 (2026-08-03)
  > state. The per-player epoch now exists (`owner_epoch`, landed 2026-09-18,
  > not yet compiled or tested) and the unconditional gate was replaced by the
  > handoff gate: an existing player's cross-node/cross-zone handoff is granted
  > when the source scene has written `player:{id}:handoff` for the current
  > `owner_epoch` (freeze → save → mark); otherwise EnterScene returns the
  > retryable `ErrHandoffPending` (18) without changing any state.
  > `ErrUnsafeCrossNodeHandoff` (14) is a retired code that is no longer
  > emitted. `AllowUnsafeCrossNodeHandoff` is now a dev-only bypass of the
  > marker gate that does **not** mint a new epoch. Authoritative description:
  > [cross-zone-scene-travel.md](./cross-zone-scene-travel.md) CZ-4, §10.2
  > (R1/R4/R6) and §11.2.
- A Gate route is committed only after the synchronous Kafka writer receives a
  `RequireOne` broker ACK. On terminal write failure, location is restored with
  an exact-value Lua compare-and-set and this request's counters are compensated;
  a concurrent newer location is never overwritten.

### Mirror co-location

A *mirror* is a runtime copy of an existing world scene used for phasing, parallel
instances, or personal instances. Because a mirror is structurally identical to
its source (same map, same AI/spawn tables, same static data), the instance path
prefers to host the mirror on the **same scene node** as the source.

```
pickInstanceNode(in):
  if in.TargetNodeId != "":           return TargetNodeId   # explicit override
  if in.SourceSceneId != 0:
      node = Redis.Get("scene:{SourceSceneId}:node")
      if node alive in zone AND (cap == 0 OR node.scene_count < cap):
          return node                 # co-locate
      # else: log + fall through
  return GetBestNode(zone)            # default load balancing
```

**Why**:
- *Memory/storage reuse*: map statics, AOI grid, AI behaviour trees and spawn
  rules are already resident on the source node; co-locating avoids loading a
  second copy on another node.
- *Client scene-switch efficiency*: the player keeps the same `gate → scene
  node` path. No cross-node handoff, no `ReleasePlayer` RPC, no fresh Redis
  reload — the C++ node reuses the in-memory entity directly (see the same-node
  branch in `enterscenelogic.go`).
- *Cross-scene query locality*: mirror ↔ source interactions (ranking, shared
  broadcast, exit teleport) become local ECS lookups instead of Kafka/gRPC hops.

**Guardrails**:
- Soft cap: `MirrorSourceNodeLoadCap` in scene_manager config. When the source
  node's `node:zone:{zoneId}:{nodeId}:scene_count` is at or above the cap, the mirror falls back
  to `GetBestNode` so a popular world can't hotspot one node. Default 0 disables
  the cap (always co-locate).
- Zone isolation: if `scene:{sourceSceneId}:zone` differs from the request's
  `zone_id`, co-location is refused (reason=`zone_mismatch`). Allowing it would
  land the mirror on the source's zone node, which the requesting zone's
  reconciliation loop can't see — orphan on every restart.
- Source-node death: if `scene:{sourceSceneId}:node` maps to a node absent from
  `scene_nodes:zone:{zoneId}:load`, `IsNodeAlive` returns false and the mirror
  falls back.
- Source migrated by rebalancer: when `migrateWorldChannel` moves a source
  channel from oldNode → newNode, every mirror in `scene:{srcId}:mirrors` is
  force-destroyed (reason=`source_migrated`) so the old node can drain and
  the next mirror request lands co-located on newNode. Without this, mirrors
  would silently lose the locality benefit and pin oldNode's memory until
  idle timeout. See `TestIntegration_Rebalance_CascadesDependentMirrors`.
- Explicit override: `TargetNodeId` still wins, so ops can pin mirrors when
  required (e.g. diagnostic sessions, canary deploys).
- Load accounting: mirrors increment `node:zone:{zoneId}:{nodeId}:scene_count` like any other
  instance, so subsequent `GetBestNode` decisions correctly reflect the mirror
  weight on the source node.

**C++ side**: `CreateSceneRequest` already carries `mirror_config_id`. The scene
node sets it on `SceneInfoComp` during creation — downstream systems filter
mirror vs. source entities via `scene_info.mirror_config_id() > 0`.

**Regression coverage**: `go/scene_manager/internal/logic/logic_test.go`
`TestCreateScene_Mirror_*` suite pins every branch of `pickInstanceNode`:
happy co-location, dead source node, overloaded source (`>= cap`), under-cap
(still co-locate), missing `scene:{id}:node` mapping, cross-zone source
falls back (`TestCreateScene_Mirror_CrossZoneSourceFallsBack`) with the
positive control `TestCreateScene_Mirror_SameZoneSourceColocates`,
single-node deployment, and explicit `TargetNodeId` override. Plus
`TestCreateScene_Instance_NonMirror_UsesGetBestNode` guards against the
mirror path leaking into plain instance creation. Cascade behaviour on
source-node death is covered by `TestReconcileDeadNode_DestroysCoLocatedMirrors`,
and on source migration by `TestIntegration_Rebalance_CascadesDependentMirrors`.

**Proto surface** (`proto/scene_manager/scene_manager_service.proto`):
```proto
message CreateSceneRequest {
  uint64 scene_conf_id   = 1;
  string target_node_id  = 2;
  SceneType scene_type   = 3;
  repeated uint64 creator_ids = 4;
  uint32 zone_id         = 5;
  uint64 source_scene_id = 6;   // triggers mirror co-location
  uint32 mirror_config_id = 7;  // forwarded to C++ SceneInfoComp
}

message CreateSceneResponse {
  uint64 scene_id      = 1;
  string node_id       = 2;
  uint32 error_code    = 3;
  string error_message = 4;
  repeated uint64 creator_ids = 5; // echo of request — used by C++ caller to
                                   // dispatch the follow-up EnterScene without
                                   // keeping per-call state.
}
```

### C++ caller (mirror flow end-to-end)

**Client-facing entry point.** `EnterSceneC2S` (see
`cpp/nodes/scene/handler/rpc/player/player_scene_handler.cpp`) already
carries `SceneInfoComp`, which has a `mirror_config_id` field. The
handler dispatches on three cases:

| `scene_id` | `mirror_config_id` | Behaviour |
|------------|--------------------|-----------|
| `> 0`      | any                | Plain EnterScene — join an existing scene (mirror or not) by id. |
| `0`        | `> 0`              | **Mirror-entry** — allocate a new mirror of the player's current scene, then enter it. Calls `PlayerSceneSystem::RequestEnterMirrorScene`. |
| `0`        | `0` (and `scene_config_id == 0`) | Rejected (`kEnterSceneParamError`). |

The mirror-entry branch returns success to the client once the
`CreateScene` RPC is queued — the async reply drives the follow-up
`EnterScene`, and the client eventually receives a normal `EnterSceneS2C`
when the pipeline completes. Clients don't need a second round-trip.

**The helper.** `PlayerSceneSystem::RequestEnterMirrorScene(player, mirrorConfigId)`
in `cpp/libs/services/scene/player/system/player_scene.cpp` drives the full
mirror lifecycle from the gameplay side:

1. **Validate prerequisites** — refuse `mirrorConfigId == 0`, validate the
   player entity, refuse while in battle or while another scene change is in
   flight (`PlayerLifecycleSystem::IsSceneChangeBusy`), require the player to
   currently be in a non-mirror source scene (refuse to mirror a mirror),
   require `PlayerSessionSnapshotComp` for gate routing, and require an
   available SceneManager node entity.
2. **Build CreateSceneRequest** with `scene_conf_id = source.scene_config_id`,
   `scene_type = SCENE_TYPE_INSTANCE`, `zone_id = GetZoneId()`,
   `source_scene_id = source.scene_id` (drives co-location),
   `mirror_config_id = mirrorConfigId`, and `creator_ids = [player_id]`.
3. **Register, then send**(2026-10-15 起):先挂 `PlayerMirrorCreateInFlightComp`(令牌 + 发送时刻),
   再用生成客户端的每次调用回调重载 `SendSceneManagerCreateScene(..., GrpcDone, ...)` 发出,回调只按值捕获
   `player_id` 与令牌。登记期间 `IsSceneChangeBusy` 为真,客户端换图、跨 zone 传送、队伍跟随一律被挡
   ([client-request-ordering.md](client-request-ordering.md) §7.2 第 7 条)。
4. **Completion** lands in `PlayerSceneSystem::HandleMirrorCreateDone`(成功 / 业务失败 / 传输失败恰好一次,
   不再经过全局 `AsyncSceneManagerCreateSceneHandler`,scene 节点也不再安装它)。按令牌认领在途组件:
   实体已不在或令牌对不上就丢弃,镜像由 scene_manager 按空场景回收。认领后:传输失败回 `kEnterSceneServerBusy`;
   scene_manager 拒绝或 scene_id 为 0 回 `kEnterSceneFailed`;成功时复核(战斗在途、别的换图 / 交接在途、
   scene_manager 可达)后经 `PlayerLifecycleSystem::RequestSceneChange` 发自动进场的 `EnterScene`,之后的
   18 / 失败照普通换图处理。

The correlation key is now the per-call callback, not the echoed
`creator_ids` (business-failure replies never echoed them, so the creator
used to get no tip at all). A SceneManager that legitimately reuses an
existing mirror via `MirrorDedupBySource` still auto-routes the requesting
player into that mirror.

> **2026-10-17 契约勘误**：以上第 3 / 4 步和“creator_ids 是唯一关联键”保留为旧流程。镜像建场现在先登记 `PlayerMirrorCreateInFlightComp`，完成闭包按值捕获 playerId / token 并委托 `PlayerSceneSystem::HandleMirrorCreateDone`；全局 CreateScene 处理器与 `scene_manager_response_handler.cpp` 已删除，不依赖响应回显创建者。普通换图与镜像共用当前生效的 SceneManager deadline + 1000ms 时效政策，时间回拨按失效处理。完成入口先核对 token，再要求槽未过 TTL；token 不符不动新槽，匹配 token 但已过期或回拨时，仅摘对应当前 token 后静默丢弃，不推 tip、不自动进场。有效完成才释放本次槽，成功时复核玩家仍在、未进入战斗及可用 SceneManager，再经 `RequestSceneChange` 自动进场。有效槽内业务拒绝、零 scene_id 或建好时已在战斗回 `kEnterSceneFailed`，无可用 SceneManager 或传输失败回 `kEnterSceneServerBusy`（3028）。建场在途也占换图互斥闸，客户端换图、队伍跟随与跨 zone 传送不能绕过它。后来的普通换图登记、交接全部预检通过、退出新挂 `UnregisterPlayer`、权威落点成功这四个提交点永久摘掉旧镜像令牌；UTC 回拨后恢复到原 TTL 内也不能使旧提示或自动进场复活。忙碌检查仍只读，未通过的交接预检不取消旧镜像。验证状态另据实际结果；Java 对照见 `grpc-muduo-transport.md` §16.6。

### C++ Side (Scene.CreateScene handler)

1. **幂等检查**：按请求 `scene_id` 查找已有 `SceneInfoComp`，命中返回已有场景。同一 `config_id` 的不同频道有不同 scene_id，不能按配置号去重。
2. `tlsEcs.sceneRegistry.create()` — creates entt entity (only if no duplicate).
3. Emplaces `SceneInfoComp` (config_id, guid). 场景成员不另存反向集合：玩家只挂 `SceneEntityComp`，需要成员时扫描 `view<SceneEntityComp, PlayerGuidComp>`（见 [组件访问规则](ecs-component-access-rules.md)“实体类型与生命周期边界”）。
4. 场景生命周期入口直接调用 Agones `OnUnitCreated` 记账，不依赖 dispatcher 订阅；最终由共享 `SceneLifecycle::CreateScene` 收口。

## Destroy Flow

> 2026-10-23 更正:下图按 `instance_lifecycle.go::destroyInstanceInternal` 的现行顺序重画。旧图把级联与
> RPC 画在原子认领之前;实际级联在认领成功之后,空闲 CAS 放弃时镜像原样保留。

```
destroyInstanceInternal(scene_id)
  ← destroyInstance (idle) / destroyInstanceForce (explicit DestroyScene RPC,
    source_migrated, source_scaled_in) / destroyOrphanOfDeadNode (node_death)
│
├─ snapshot GET scene:{id}:{node, agones_gs, player_count, source, mirror}  (reference only)
├─ [atomic claim] EVAL luaAtomicDestroyInstance (idle: aborts if player_count > 0)
│                 or luaAtomicDestroyInstanceForce (others; node_death passes the
│                 dead node as expected owner, aborts if scene:{id}:node differs)
│    └─ DEL scene:{id}:{node, player_count, mirror, source, zone, agones_gs}
│       + ZREM instances:zone:{zoneId}:active
│    claim lost → return, no side effects at all
├─ [cascade] SMEMBERS scene:{id}:mirrors → destroyInstanceInternal(child, force, reason=cascade)
│            then DEL scene:{id}:mirrors
├─ RequestNodeDestroyScene → gRPC → C++ Scene.DestroyScene(scene_id)   (only if owner alive)
├─ SREM node:zone:{zoneId}:{nodeId}:scenes
├─ [if mirror] SREM scene:{sourceId}:mirrors
├─ INCRBY node:zone:{zoneId}:{nodeId}:scene_count -1 + drain residual from player_count
└─ ReleaseAgonesRoomForScene (if agones_gs was set)
```

### Destroy-while-entering race (fixed 2026-04-23)

Without synchronisation, there is a window between the lifecycle tick's
`player_count==0` check and its `DEL scene:{id}:node` call where an
`EnterScene` RPC can land. That would route a player into a scene that
is about to be wiped, and also create an orphan `instance:{id}:player_count`
key that lives forever (nothing cleans it up after the owning scene is
gone).

The fix moves both sides into single-keyspace Lua scripts so Redis
serialises them:

- **`luaAtomicIncrPlayerCount`** (EnterScene path): checks
  `EXISTS scene:{id}:node`; if missing returns `-1` without creating
  `instance:{id}:player_count`. If present INCRs and returns the new count.
  EnterScene treats `-1` as "scene disappeared, reject the request" and
  rolls back the `PlayerLocation` write.
- **`luaAtomicDestroyInstance`** (idle lifecycle path, via `destroyInstance`): if
  `player_count > 0`, returns `""` without touching anything; otherwise
  wipes every scene-scoped key, `ZREM`s from the active set, and returns
  the hosted nodeId so the caller can fire `DestroyScene` RPC and drain
  counters. A force path (`luaAtomicDestroyInstanceForce`) bypasses the idle
  check: `destroyInstanceForce` for explicit / source_migrated /
  source_scaled_in, and the mirror cascade (which recurses inside
  `destroyInstanceInternal`); since 2026-10-23 the explicit `DestroyScene`
  RPC delegates to it instead of its own snapshot-then-DEL copy.
  Node-death reconciliation uses `destroyOrphanOfDeadNode`, which passes
  the dead node as the expected owner, so the script only wipes scenes
  still owned by it (see below).

Failure mode coverage:

| Scenario | Behaviour |
|----------|-----------|
| Destroy wins the race | Enter's INCR returns `-1` → EnterScene 400s, client retries |
| Enter wins the race   | Destroy's script returns `""` → lifecycle reseeds the idle clock via `ZADD` and re-evaluates next tick |
| Both sides racing under retries | Bounded: every retry goes through the same Lua, Redis serialises |

Regression: `TestAtomicIncrPlayerCount_{SceneExists,SceneGone}`,
`TestAtomicDestroyIfIdle_{AbortsOnPlayers,DestroysWhenIdle}`,
`TestDestroyInstance_CAS_AbortsWhenPlayersPresent`,
`TestEnterScene_AtomicIncr_NoOrphanPlayerCount`.

### Reverse indexes (added 2026-04-23)

Two Redis SETs make the destroy paths O(1) lookups instead of keyspace scans:

| Key | Populated at | Used by |
|-----|--------------|---------|
| `node:zone:{zoneId}:{nodeId}:scenes` | every `allocateScene` / reassign | node-death reconciliation (`reconcileDeadNodeScenes`) |
| `scene:{sourceId}:mirrors` | mirror create with `source_scene_id > 0` | cascade destroy in lifecycle + `DestroyScene` RPC |

Companion scalar `scene:{mirrorId}:source` lets a dying mirror `SREM`
itself out of its source's mirror set without a full scan. The atomic
destroy script wipes it alongside the rest of the scene-scoped keys.

### Cascade destroy

When a scene with mirrors is destroyed (explicit RPC or lifecycle), we
`SMEMBERS scene:{id}:mirrors` only after the atomic claim succeeds and
force-destroy every child; if the idle CAS aborts, the mirrors are left
untouched (the destroy scripts never touch that set). This prevents mirrors from living on a dead source (their
NPCs reference the source's spawn data; leaving them behind means
nothing can enter them and their state ticks uselessly until idle
timeout).

Covered by `TestDestroyScene_CascadesToMirrors`,
`TestDestroyMirror_UnlinksFromSourceSet`. The
`TestReconcileDeadNode_DestroysCoLocatedMirrors` test pins that mirrors
co-located on a dying instance node are reaped by the node-death
reconciliation loop (each mirror's `node:zone:{zoneId}:{nodeId}:scenes` membership is
walked alongside the source's). The
`TestIntegration_Rebalance_CascadesDependentMirrors` integration test
pins the source-migration path: when the rebalancer moves a source
world channel from oldNode → newNode, every mirror in
`scene:{srcId}:mirrors` is force-destroyed (counter
`instance_destroyed_total{kind="mirror",reason="source_migrated"}`),
draining the old node and letting the next mirror request co-locate
correctly on newNode.

### Node-death reconciliation

> 2026-10-23 更正:本节原先描述的是早期实现(`removeNodeFromRedis` 末尾直接收尾、现读 SMEMBERS、
> 无条件 `destroyInstanceForce`、最后 DEL 整个集合),以下为现状。

The leader's `load_reporter.go::removeNodeFromRedis` writes `death_at`,
removes the node from the load set, snapshots
`node:zone:{zoneId}:{deadNodeId}:scenes` and enqueues a reconcile task
(when `death_at` cannot be written or the load-set removal fails, the
detach is deferred, GO-6). `reentry_barrier.go::drainPendingDeadNodeReconciles`
runs on every 5s loadTicker and in fullSync's leader branch; it has no
leader gate of its own (only the leader enqueues). Once the barrier has
elapsed it calls `reconcileDeadNodeScenes(entry, snapshot)`, which checks
`isLeader()` per member and aborts on lost leadership:

1. Fast filter: members whose `scene:{id}:node` already points at another
   node are counted `stale`.
2. Members not in `instances:zone:{z}:active` are counted `world_skipped`:
   world channels (left to the rebalance pipeline) and instances already
   destroyed before this read, e.g. mirrors cascaded by their source
   earlier in the same sweep — every destroy script `ZREM`s atomically with
   the mapping `DEL`. Only `redis.Nil` counts as "not in the set"; a read
   error counts `failed`.
3. The rest go to `destroyOrphanOfDeadNode(deadNode)`: no player_count
   check, but the Lua script only wipes the scene while `scene:{id}:node`
   still equals the dead node; otherwise it touches nothing and the member
   is counted `stale`. That covers a writer that moved the instance with
   `reassignSceneNode` after the GET in step 1 (at fix time, 2026-10-22,
   `resolveScene`'s dead-owner self-heal did; since 10-23 it only rebuilds
   world channels, so the compare is defence in depth), and a mapping that
   disappears between the Zscore and the script (a concurrent destroy, or
   `rollbackInstanceAllocation`, which DELs the mapping before the ZREM —
   the path that deleted it owns the rest of the cleanup). Redis errors
   count `failed`.
4. Only snapshot members are `SREM`ed (never `DEL` the set); `failed`
   members stay in the set, since their mapping may still point at the
   dead node.

After reconcile returns, drain runs `deleteNodeCounters` (must run after
the sweep) and drops the task either way. No retry is scheduled for
`failed` members, and members left by a sweep aborted on lost leadership
are not picked up by the new leader either (its fullSync only scans load
sets). They are only picked up if the same node_id is declared dead again,
or by idle reclaim once their player_count drains to 0 (LeaveScene from the
player_locator lease expiry, or the player entering elsewhere). Tracked as
residual R1 of the `docs/PROGRESS.md` 2026-10-23 entry 「scene_manager
死节点收尾与显式 DestroyScene 的销毁改为按认领时属主」.

World channels are intentionally excluded here; destroying them would
drop every player in that shard. The existing rebalance path already
moves them to a new node and re-issues CreateScene on the C++ side.

Regression: `TestReconcileDeadNode_{DestroysOrphanInstances,PreservesWorldChannels,SkipsReassignedScenes,DestroysCoLocatedMirrors,ReassignedAfterOwnerRead_LeavesNewOwnerIntact,StillOwnedByDeadNode_Destroys,DestroyScriptErrorKeepsMemberIndexed}`,
`TestAtomicDestroyForce_{ExpectedOwnerMismatchTouchesNothing,NoExpectedOwnerWipesWhoeverOwnsIt}`,
`TestOwnerReadInterleave_{ReconcileSkipsSceneReassignedAfterOwnerRead,ExplicitDestroyFollowsOwnerAtClaimTime}`.

### Observability (Prometheus)

Added counters alongside the existing gauges in
`scene_manager/internal/metrics/metrics.go`:

| Metric | Labels | Emitted by |
|--------|--------|-----------|
| `scene_manager_mirror_colocate_total` | `zone_id`, `outcome` (hit\|fallback), `reason` (ok\|no_mapping\|zone_mismatch\|node_dead\|overloaded) | `pickInstanceNode` |
| `scene_manager_instance_destroyed_total` | `zone_id`, `kind` (instance\|mirror), `reason` (idle\|explicit\|cascade\|node_death\|source_migrated\|source_scaled_in) | `destroyInstanceInternal` (via `destroyInstance` / `destroyInstanceForce` / `destroyOrphanOfDeadNode`), `DestroyScene`, `migrateWorldChannel`, `world_autoscale` |
| `scene_manager_enter_scene_rejected_total` | `zone_id`, `reason` (not a fixed list here — the authoritative set of values is the metric's `Help` string in `go/scene_manager/internal/metrics/metrics.go`) | `EnterScene` reject paths (`enterscenelogic.go` / `home_zone.go`) |
| `scene_manager_scene_orphans_reconciled_total` | `zone_id` | `reconcileDeadNodeScenes` |
| `scene_manager_mirror_source_missing_total` | `zone_id` | `createInstance` source-clone refusal |
| `scene_manager_mirror_dedup_total` | `zone_id`, `outcome` (hit\|miss\|stale) | `createInstance` when `MirrorDedupBySource=true` |

Dashboards should chart:

- `rate(mirror_colocate_total{outcome="hit"}[5m]) / rate(mirror_colocate_total[5m])`
  — co-location hit rate. A sustained drop flags either source-node
  death, load-cap pressure (`MirrorSourceNodeLoadCap`), or a cross-zone
  caller (`reason="zone_mismatch"`) — use the `reason` label to split.
- `rate(mirror_dedup_total{outcome="hit"}[5m]) / rate(mirror_dedup_total[5m])`
  — dedup hit rate, only meaningful when `MirrorDedupBySource=true`.
  A nonzero `outcome="stale"` means `scene:{source}:mirrors` drifted
  from the real scene state; a burst indicates a destroy path missed
  cleaning the source's mirrors set (cascade regression).
- `rate(instance_destroyed_total{kind="mirror",reason="source_migrated"}[5m])`
  — spikes align with rebalancer activity and confirm the cascade path
  is draining the old node.

Alerting on `enter_scene_rejected_total` is defined per `reason` in
`deploy/k8s/scene-manager-alerts.yaml` — that file is the source of truth for
which reasons page and at what rate, and the valid `reason` values are the
ones listed in the metric's `Help` string in
`go/scene_manager/internal/metrics/metrics.go`. Do not copy a reason list into
this document or hand-write a selector from memory: a selector on a reason
that has no call site evaluates to an empty vector and never fires, without
any error. Also alert on nonzero `mirror_source_missing_total` (a C++ caller
requested a source-clone mirror — currently refused server-side; each
event indicates a client-side contract bug).

### C++ Side (Scene.DestroyScene handler)

1. Linear scan `sceneRegistry` for entity matching `scene_id`.
2. If not found, log `"DestroyScene: scene_id=N not found, idempotent OK"` and return success.
3. 无住户时直接调用 Agones `OnUnitDestroyed` 后销毁场景实体；排空和销毁最终由共享 `SceneLifecycle::DestroyScene` 收口。

**Idempotency guarantee.** The `not found → success` branch is what makes the
Go-side cascade and reconciliation paths safe to run repeatedly. Cascade
destroy can issue `DestroyScene` against the same sceneId twice (parent
+ orphan-cleanup race); node-death reconciliation runs after the node is
already gone but a future restart may legitimately have nothing to clean.
Both cases collapse to a no-op on C++ instead of an error log spam.

### Mirror defensive checks (CreateScene)

Two checks run before allocation in `createInstance`:

1. **Source-clone refusal.** When `MirrorConfigId == 0 && SourceSceneId > 0`
   (the "clone everything from source" mode), `EXISTS scene:{sourceId}:node`
   is required. If the source is gone, refuse with `ErrSourceSceneGone`
   instead of silently producing an unplayable mirror with no template.
   Mirrors with their own `MirrorConfigId` are NOT subject to this check —
   the source there is just a co-location hint, and missing-source falls
   back through `pickInstanceNode` like any other no_mapping case.
   Counter: `scene_manager_mirror_source_missing_total{zone_id}`.

2. **Optional dedup (`MirrorDedupBySource` config, default false).** When
   on, a mirror request whose source already has at least one mirror in
   `scene:{sourceId}:mirrors` returns the existing mirror id instead of
   allocating a new one. Selection is arbitrary (first set member). The
   path also self-heals dangling mirror ids: if the chosen mirror's
   `scene:{id}:node` mapping is gone, the id is `SREM`'d from the set
   and the request falls through to a fresh allocate. Counter:
   `scene_manager_mirror_dedup_total{zone_id, outcome=hit|miss|stale}`.

Operators choose one mirror semantics regime per deployment:

- `MirrorDedupBySource: false` — independent copies (per-player phasing,
  unique loot rolls). Default; matches behaviour before this feature.
- `MirrorDedupBySource: true` — shared instance (raid lockouts, world
  bosses where the whole party should land in the same mirror).

## Player Count Tracking

- `EnterScene` → `IncrInstancePlayerCount(zoneId, sceneId)` — Redis INCR on `instance:{sceneId}:player_count` plus the zone-scoped node aggregate.
- `LeaveScene` → `DecrInstancePlayerCount(sceneId)` — Redis INCRBY -1, clamped to 0 (guards against double-leave or disconnect without leave).
- Instance lifecycle manager periodically scans active instances. If player count is 0 for longer than `InstanceIdleTimeoutSeconds`, the instance is destroyed.

## Instance Lifecycle Manager

- Runs as a goroutine started alongside the SceneManager.
- Polls every `InstanceCheckIntervalSeconds` (default 30s).
- For each active instance: if `player_count > 0`, resets idle clock (updates sorted set score to now). If `player_count == 0` and idle duration exceeds the **per-type** timeout, calls `destroyInstance`.
- `destroyInstance` notifies C++ node via `RequestNodeDestroyScene` before cleaning Redis state.

### Mirror vs. instance idle timeouts

Mirrors and regular instances use **separate** idle timeouts because their state semantics differ: a mirror re-initializes NPCs / spawn state on every entry, so keeping an empty mirror resident is pure waste; a regular instance (e.g. a dungeon run) may want to survive a disconnect + retry window.

| Config key | Default | Applies to |
|------------|---------|------------|
| `InstanceIdleTimeoutSeconds` | 300 | scenes WITHOUT `scene:{id}:mirror` flag |
| `MirrorIdleTimeoutSeconds`   | 30  | scenes WITH `scene:{id}:mirror` flag (set at allocate time when `MirrorConfigId > 0` or `SourceSceneId > 0`) |

- `0` on either key disables auto-destroy **for that kind only**, not both.
- `MirrorIdleTimeoutSeconds == 0` falls back to `InstanceIdleTimeoutSeconds` (treat mirrors like normal instances). This is the opt-out path.
- The mirror flag is a tiny Redis string key (`"1"`) set by `createscenelogic.go::createInstance` when the request carries mirror/source metadata. Both the lifecycle manager (`destroyInstance`) and the explicit `DestroyScene` RPC delete the flag — **every** scene-destroy path must stay in sync.
- Regression coverage: `TestCleanupIdleInstances_MirrorDestroyedFaster`, `TestCleanupIdleInstances_MirrorWithPlayers_NotDestroyed`, `TestCreateScene_Mirror_SetsMirrorFlag`, `TestCreateScene_NonMirror_NoMirrorFlag`, `TestDestroyScene_ClearsMirrorFlag`, `TestResolveMirrorTimeout_FallsBackToInstance`.

## gRPC Connection Cache (Go → C++ SceneNode)

- `scene_node_client.go` maintains a `nodeConnCache` (`sync.RWMutex`-guarded
  `map[string]*grpc.ClientConn`) keyed by `(zoneId,nodeId)`, not `nodeId` alone.
- Node endpoint discovered from etcd: `SceneNodeService.rpc/` prefix scan (all zones), JSON parsing of `sceneNodeRegistration.Endpoint`.
- Only `grpc_endpoint` is accepted; falling back to the raw RPC0 TCP endpoint
  would create a protocol mismatch.
- Both the watch cache and direct etcd fallback require exactly one registration
  for `(zoneId,nodeId)`. A duplicate identity is rejected even when a connection
  was already cached.
- `RemoveNodeConn(zoneId,nodeId)` is called by `load_reporter.go` when a node's
  grace period expires or its registration changes.

## Idempotent Convergence (Node Reconnect/Restart Safety)

Industry-standard pattern (same principle as K8s controller reconciliation loop). No per-node state machine required.

### K8s 设计模式在本项目中的映射

#### 1. List-Watch（K8s Informer 模式）

**K8s 原理**：K8s 的 Informer 不是每秒轮询 API Server，而是：
- **List**：启动时执行一次全量 GET，拉取所有资源，建立本地缓存（indexer），记录 `resourceVersion`。
- **Watch**：从 `resourceVersion+1` 开始监听变更事件流（ADDED / MODIFIED / DELETED），增量更新本地缓存。
- **Re-list**：Watch 连接断开时，重新 List 建立基线，再重新 Watch。
- 优势：全量 + 增量结合，不丢事件，不做无谓轮询，99% 的时间只处理增量。

**本项目映射**（`load_reporter.go`）：

| K8s Informer 概念 | 本项目实现 |
|-------------------|-----------|
| List（全量拉取） | `fullSync()` — `etcd.Get(prefix, WithPrefix())` 拉取所有 `SceneNodeService.rpc/*` |
| Local cache / indexer | `knownNodes map[string]nodeEntry` — 按 etcd key 索引的节点信息 |
| resourceVersion | `resp.Header.Revision` — etcd 全局递增版本号 |
| Watch（增量监听） | `watchAndRefresh()` — `etcd.Watch(prefix, WithRev(rev+1))` 接收 PUT/DELETE |
| Event handler | `handleWatchEvent()` — 分发 PUT（新增/更新）和 DELETE（删除）事件 |
| Re-list on watch error | Watch channel 关闭或报错 → 回到外层 for 循环 → 重新 `fullSync()` |

```
StartLoadReporter (外层 for 循环)
  │
  ├─ fullSync()                          ← K8s List
  │   ├─ etcd Get 全量拉取
  │   ├─ 建立 knownNodes (= indexer)
  │   ├─ 清理 Redis 中的陈旧节点
  │   ├─ 对所有 zone 执行 initWorldScenesForZone (= 全量 reconcile)
  │   └─ 返回 revision
  │
  └─ watchAndRefresh(rev)                ← K8s Watch
      ├─ etcd.Watch(prefix, WithRev(rev+1))
      ├─ SELECT:
      │   ├─ watchCh event
      │   │   └─ handleWatchEvent()      ← Event handler (增量 reconcile)
      │   │       ├─ PUT  → 更新 knownNodes, 更新 Redis load, 触发 initWorldScenesForZone
      │   │       └─ DELETE → 从 knownNodes 移除, 启动 grace period
      │   └─ 5s ticker
      │       ├─ refreshLoadScores()     ← 定期刷新负载分数
      │       └─ checkGracePeriodExpirations()
      └─ watch 断开 → return → 外层 for 自动 re-list
```

> 更正(2026-10-16):图里的 `refreshLoadScores()` 现在只在**领导者**上按 zone 批量刷新负载分(fullSync 里也走同一条
> 路径,PUT 事件的单节点刷新同样只由领导者做);跟随者一条 Redis 命令都不发,只发布本 zone 的 `nodes_by_role`。
> fullSync 里「清理 Redis 中的陈旧节点」与孤儿频道清理不再全库 SCAN,改读 zone 登记集合。详见下文
> 「负载刷新与 zone 登记集合(2026-10-16)」。

#### 2. Reconciliation Loop（K8s Controller 模式）

**K8s 原理**：Controller 不记录"我做过什么操作"的步骤状态机，而是：
- 声明 **期望状态**（Desired State）：例如 Deployment spec 声明要 3 个 Pod。
- 观察 **当前状态**（Current State）：查看实际 Pod 数量。
- **收敛**（Reconcile）：计算 diff，执行动作让 current → desired。
- 关键特性：**幂等**。执行多次结果一样。不管中间崩溃多少次，下次运行时重新 observe + reconcile 即可自愈。

**本项目映射**：

| K8s Controller 概念 | 本项目实现 |
|--------------------|-----------|
| Desired State | 世界配置集合 + `DesiredWorldChannelCount` 给出的每图期望频道数 |
| Current State | Redis `world_channels:zone:{zoneId}:{confId}` SET 的 sceneId 集合 + C++ ECS registry |
| Reconcile 函数 | `initWorldScenesForZone()` — 对比 + 收敛 |
| Reconcile 触发方式 | Watch PUT event（增量）或 fullSync（全量） |
| 幂等保证 | zone 补建锁内只分配缺失的频道 ID + C++ CreateScene 按 scene_id 去重 |

```
Reconcile (initWorldScenesForZone):

  持有 world_init:lock:zone:{zoneId} 的属主令牌:
    for each confId in 世界配置集合:
      desired = DesiredWorldChannelCount(zoneId, confId)
      existing = SMEMBERS world_channels:zone:{zoneId}:{confId}
      ├─ len(existing) < desired → SceneIDGen.Generate 分配缺失频道 ID
      │    └─ 写 scene:{id}:node / zone，SADD 频道 SET
      └─ 对频道 SET 中每个 sceneId 调 C++ CreateScene(scene_id, config_id)
           ├─ ECS 已有同 scene_id → 返回已有 entity
           └─ ECS 没有同 scene_id → 创建频道 entity（同配置可创建多个）
```

#### 3. 两者结合 = 事件驱动的收敛控制器

不轮询（Watch 推送），不搞状态机（幂等收敛），两个 K8s 核心模式的结合：

```
etcd Watch (事件驱动，不轮询)
  │
  ├─ PUT event (节点新增/重现)
  │   └─ handleWatchEvent → initWorldScenesForZone (reconcile)
  │
  ├─ DELETE event (节点消失)
  │   └─ handleWatchEvent → 启动 grace period → 超时后清理 Redis
  │
  └─ Watch 断开
      └─ fullSync (re-list) → 对所有 zone 执行 initWorldScenesForZone (全量 reconcile)
```

旧方案（轮询）vs 新方案（list-watch）对比：

| 方面 | 轮询 (旧) | List-Watch (新) |
|------|-----------|-----------------|
| 发现节点变化 | 每 5s `etcd.Get` 全量扫描 | etcd Watch 事件推送，毫秒级 |
| 节点新增检测 | `prevSeenByZone` 前后帧 diff | Watch PUT 事件，`!existed` 判断 |
| 节点删除检测 | 遍历 Redis vs seenByZone diff | Watch DELETE 事件直接触发 |
| 负载分数刷新 | 每帧全量刷所有节点 | 5s ticker 仅更新已知节点 |
| etcd 压力 | 每 5s 一次全量 Get | 建连 + 增量流，几乎零轮询开销 |
| 容错 | 单帧失败跳过，下帧重试 | Watch 断开 → re-list + re-watch |
| 延迟 | 最差 5s（一个 tick 间隔） | 毫秒级（etcd 推送延迟） |

### Three-Layer Protection

1. **C++ CreateScene 按 `scene_id` 幂等**：已有同 scene_id 时返回原场景；相同 config_id 的另一个 scene_id 代表另一条频道，应独立创建。

2. **Go `initWorldScenesForZone` dual-layer idempotency**:
   - Redis 层：在 zone 补建锁内读取 `SMEMBERS world_channels:zone:{zoneId}:{confId}`，只分配低于当前期望数的差额。
   - C++ 层：为频道 SET 中的全部 scene_id 发送 `CreateScene`，由 C++ 按 scene_id 去重。
   - This handles the scenario where Redis has stale data (scene ID exists) but the C++ node restarted and its ECS registry is empty.

3. **Go LoadReporter list-watch**: etcd Watch 事件驱动。节点 PUT → 触发 reconcile。Watch 断开 → fullSync re-list + 全量 reconcile。

### Scenario Matrix

| Scenario | Redis State | C++ ECS State | Outcome |
|----------|-------------|---------------|----------|
| First startup | empty | empty | fullSync → allocate scene ID + create ECS entity |
| Node reconnect (data alive) | has data | has entity | Watch PUT → reconcile → RPC returns existing, zero side-effect |
| Node restart (data lost) | has data | empty | Watch PUT → reconcile → no re-allocation, RPC creates new entity |
| SceneManager restart | has data | has entity | fullSync → 全量 re-ensure → C++ dedup |
| Watch 断开重连 | has data | has entity | re-list → 全量 reconcile → C++ dedup |

## Main Scene Initialization

事件驱动，不阻塞启动：
1. `StartLoadReporter` → `fullSync()` 全量拉 etcd → 对所有发现的 zone 执行 `initWorldScenesForZone`（全量 reconcile）。
2. 之后 Watch 接管：每当有 C++ 场景节点注册（PUT 事件），`handleWatchEvent` 触发该 zone 的 `initWorldScenesForZone`（增量 reconcile）。
3. `initWorldScenesForZone` 在 zone 补建锁内遍历世界配置，按 Redis 期望数与频道 SET 的差额分配 ID，再为集合中的每个频道发送 C++ CreateScene RPC；节点选择保留哈希分布与存活检查。

## 负载刷新与 zone 登记集合(2026-10-16)

**状态**:以「领导者按 zone 批量刷新 + 登记集合替代 SCAN」缓解(未编译、未测试,待 Codex 验证)。字面的「每个副本只读
本 zone」、按 zone 选主与 etcd watch 收窄仍归 A1(`docs/design/capacity-model-hok-scale.md` §3)。容量数字均为公式推算,未压测。
代码:`go/scene_manager/internal/logic/load_refresh.go`、`zone_registry.go`、`load_reporter.go`、`orphan_cleanup.go`、
`world_autoscale.go`;`tools/merge_zone/scene_hot_state.go`。

### 问题

scene_manager 按 zone 部署(每 zone 2 副本),但默认用全局一把选主锁,全服只有一个领导者;数据面是全局的(C++ 按
playerId 在全服所有 SM 里挑一个),所以每个副本 watch 全服 scene 节点。改动前:

- 每个副本每 5s 对内存里**全部**节点逐个串行 `GET` 两个计数(读不分领导者,写只由领导者做),跟随者读出来只为发每节点
  指标。按容量模型 §0.3 的参数推算,2000 万在线时是 0.8·Z·N_s ≈ 28 万~316 万 GET/s,每个副本每拍 3,556~17,778 次往返,
  已超过 5s 的拍长;
- 领导者每次 fullSync(启动、watch 中断、领导权变化)跑两次全库 SCAN(`scene_nodes:zone:*:load`、
  `world_channels:zone:*`),SCAN 遍历的是整个共享库,开服规模下单次是小时级,期间 load reporter 的 goroutine 停摆。

### 为什么不按副本刷新本 zone

负载集与类型镜像只能有一个写者:跟随者的 watch 镜像可能落后于领导者的清理,会把领导者刚 `ZREM` 出负载集的死节点
重新 `ZADD` 回去,而 DELETE 事件领导者已经消费过,没有第二次删除(`load_reporter.go` `updateNodeLoad` 的注释)。
「每个副本刷新本 zone」需要每个 zone 各有一个写者,即按 zone 选主,还要为滚动升级另加兜底 —— 那是 A1 的范围。
按 AGENTS.md §11「正确性 > 简单」,本项保留单写者:**只有领导者刷新**,跟随者一条 Redis 命令都不发;领导者内部按
zone 分批,正是 A1 拆成每 zone 领导者时可以直接复用的形状。

### 一拍的形状

1. 一条 `SADD scene_manager:zones` 登记本拍涉及的全部 zone;
2. 读:pipeline 分批(每批 1024 条命令),每个节点两条 `GET`;
3. 写之前再判一次领导权,批与批之间也判;写:pipeline 分批,每个节点一条 `SET` 类型镜像(排在 `ZADD` 之前,缩短
   「未分类节点进所有池」的窗口),每个 zone 一条 `ZADD` 写读成功的节点,再加一条 `ZADD NX` 写读失败的节点;
4. 计数读成功、且所在 zone 的分数 `ZADD` 确认写成功的节点发每节点指标(导出的分数必须是 Redis 里生效的那个)。

推算(§7.2 口径,未实测):刷新读降为 0.4·N_s GET/s(2000 万在线约 711~3,556),跟随者为 0;领导者每拍约 7~29 次往返。
每节点序列只由领导者导出(3·N_s 条),`nodes_by_role` 约 12·Z 条。

### 读失败语义(fail-closed)

- **不按 0 写**:按 0 写会把满载节点伪装成最空闲的,下一批进场全压上去。旧代码 `readInt64` 出错就当 0。
- **已在负载集里的成员保留旧分**。
- **不在负载集里的节点以哨兵分 `1e12` 加入**(`ZADD NX`):负载集成员资格就是存活信号 —— `IsNodeAlive` 把「不在负载集」
  当作唯一的死亡证据,`playerLocationOwnerGone` 把 `ZCARD == 0` 当作整 zone 已死,`world_rebalance` 用 `ZSCORE` 判存活。
  Redis 丢了成员(或节点刚注册)时若因计数读失败就不加回去,活节点会被判死,在老属主仍在存盘时放行改派 / 接管。
  哨兵分让它在选点里排最后(选点按分数升序),下一拍读成功后自然纠正;它只写进 Redis,不进每节点指标。
- **登记写失败不扣成员资格**:`SADD` 失败只记 `registry_failed`,`ZADD` 照常写,不加 `XX`。
- **每节点指标与 Redis 的不一致是有意的、有告警的**:读不到计数或分数 `ZADD` 没写进去的节点,本拍不更新每节点 gauge,
  序列停在上一次成功的值、不删 —— 删掉的话单拍抖动会让序列闪断,把 `WorldNodeSaturated` / `LoadScoreDispersionLow`
  的 for 计时清零。代价是持续读不到的节点在看板与这两条告警里一直是旧值(非成员还一直挂着哨兵分、等于少了一份容量),
  由 `SceneManagerLoadRefreshNodeErrors` 盯 `load_refresh_node_errors_total`;写失败由 `SceneManagerLoadRefreshFailing` 盯。

### pipeline 结果按批判定

2026-10-16 对照模块缓存里的源码核实(go-zero v1.10.0、go-redis v9.17.3):

- go-zero 的熔断挂在 `ProcessPipelineHook` 上(`core/stores/redis/breakerhook.go`):熔断打开时直接返回
  `breaker.ErrServiceUnavailable`,队列里的命令一条都没执行、各自的 err 也不回填,`StringCmd.Result()` 是 `("", nil)`;
- 取连接 / 网络出错时 go-redis 把错误回填到每条命令(`redis.go` `generalProcessPipeline` 的 `setCmdsErr`);
- 正常执行时返回第一条带错命令的错误(`cmdsFirstErr`),`redis.Nil` 也算。

所以:`PipelinedCtx` 的返回值不是 `redis.Nil`,且是熔断拒绝、或本批没有任何一条命令自己带错时,整批按失败处理
(`pipelineBatchRejected`);单条 `GET` 只有 `redis.Nil` 视为 0,`("", nil)` 一律视为读失败(`classifyCounterReply`)。
写批同理,失败记 `write_failed`。否则熔断按比例丢请求时会出现「读批被拒 → 全读成 0 → 写批被放行 → 全服分数写成 0」,
而且 outcome 还记成 ok。

outcome 上 `read_failed` 只记**传输层失败**:批被拒,或有命令带着非 Redis 回复的错误(取连接 / 网络 / 超时;go-redis 会把
它回填到当前及后续命令),判定是 `pipelineHitTransportError`,按错误类型(有没有 `RedisError()` 方法)判。个别键的逐条回复
错误(WRONGTYPE)与坏值(空串 / 非数字)不论批多大、节点多少都只记 `partial`。不用「整批全错」判:同一个坏键独占尾批、或
部署里只有 1 个节点时会记成 `read_failed`,与别的节点同批时记 `partial`,告警随节点增减时响时不响。也不拿命令上的错误与
`PipelinedCtx` 的返回值比较:重试用尽时 go-redis 直接返回最后一轮的错误,命令身上留的可能是更早一轮的错误实例。
写失败按批聚合成一行日志(失败条数、zone 范围、第一条失败命令),网络错误被回填到整批时不至于每拍放大成 N_s+Z 行。

### zone 登记集合 `scene_manager:zones`

- **语义**:SET,成员是 zone 十进制;「可能留有 scene_manager 热状态的 zone」。只是发现索引,不是存活判据,不参与路由。
- **写入方**(全部尽力而为,失败只打 Error):领导者刷新(每拍一条 `SADD`,先于负载集写入);`DesiredWorldChannelCount`
  播种分支与 `setDesiredWorldChannelCount`(先于写期望数 Hash;可能跑在任意副本上,CreateScene 兜底会拿请求里的 zone
  走补建,所以集合里可能有没有活节点的 zone,无害);每个副本启动时登记自己的 `ZoneId`。只加不减,多写者安全。
- **读取方**:`sweepStaleLoadSetMembers`(扫描范围 = 快照 zone ∪ 登记集合)、`CleanupOrphanWorldChannels`(候选 zone =
  登记集合 ∪ 有活节点的 zone)。读登记集合失败时退回只用快照 / 活节点 zone。
- **删除方**:只有 `tools/merge_zone -clear-source-hot-state`,作为清热状态的最后一步(前面任何一步失败都保留登记)。
- **不变量**:尽力登记 + 每拍自愈。有活节点的 zone 一定在 etcd 快照里,领导者每拍重新登记。缺口只有「升级前就已整体
  下线、又从未登记」与「从没登记成功、又在领导者缺位时整体死掉」的 zone;它们复活时会重新进快照,一直不复活的按
  `docs/ops/scene-node-role-split.md` §6.1 的一次性命令补登记。
- **生产代码里不保留全库 SCAN**,也不设「打开就 SCAN」的开关:K8s ConfigMap 由 `k8s_deploy.ps1` 整份生成,手改的值下次部署
  就被覆盖;忘了关则每次领导者 fullSync 都会把小时级 SCAN 带回 load reporter 的 goroutine。
- 键名刻意不以 `scene_nodes:zone:` / `world_channels:zone:` / `world_channels:desired:zone:` / `node:zone:` 开头,避免被旧版本
  的 SCAN 与 merge_zone 按源 zone 前缀清理的模式误命中;与 merge_zone 的同名常量是跨 module 契约,两边测试各自钉死。

### 孤儿清理按期望数 Hash 发现

候选 `(zone, confId)` 取 `world_channels:desired:zone:{z}` 的字段(每个 zone 一条 `HKEYS`),confId 不在 World 表的组按
`(zone, confId)` 点名清理。生产代码里往频道集合 `SADD` 的唯一入口 `initWorldScenesForZone` 先经 `DesiredWorldChannelCount`
播种期望数字段,所以「有频道集合」基本蕴含「有期望数字段」。`deleteOrphanChannel` 把删期望数字段挪到最后一步:它是发现
索引,先删它、再删集合时中途失败,这组孤儿就再也找不到了。覆盖不到的只有 `HSETNX` 出错兜底路径建出的「有集合、没有
期望数字段」的组与更早的遗留,后果是残留垃圾键,不影响路由;反过来「集合已空、字段还在」的残留以前找不到,现在能清。

### 指标与告警

- `scene_manager_load_refresh_total{outcome}`:`ok` / `partial` / `read_failed` / `registry_failed` / `write_failed` /
  `lost_leadership`,每拍一次、只有领导者发出,所有 outcome 在注册时预置为 0;
- `scene_manager_load_refresh_duration_seconds`(Histogram)、`scene_manager_load_refresh_node_errors_total`;
- 每节点 gauge 只由领导者导出,降级时 `ResetLeaderGauges` 与跟随者每拍的 `ResetNodeSeries` 清掉旧序列;换主时序列换 pod,
  空缺约一个锁 TTL 加一拍,`WorldNodeSaturated` / `LoadScoreDispersionLow` 的 for 计时会在换主时清零;
- `nodes_by_role`:领导者发布全部 zone,跟随者只发布本 `ZoneId`,都给本 `ZoneId` 补 0;看板改为 `max by`;
- 新告警(阈值待压测基线):`SceneManagerLoadRefreshStalled`(刷新停摆 / 没有领导者)、`SceneManagerLoadRefreshFailing`
  (`read_failed|registry_failed|write_failed`)、`SceneManagerLoadRefreshSlow`(p99 > 2s)、`SceneManagerLoadRefreshNodeErrors`
  (`load_refresh_node_errors_total` 持续增长 30 分钟,覆盖只记 `partial` 的个别节点长期读不到)。见 `deploy/k8s/scene-manager-alerts.yaml`
  与 `docs/ops/scene-node-role-split.md` §7。

### 兼容与升级

不涉及 proto、客户端可见契约、C++ 代码与 go.mod;Redis 键名格式不变,只新增 `scene_manager:zones`。滚动升级时新旧版本
并存:旧领导者照旧全量刷新与 SCAN、新跟随者不读,总负载只降;新领导者按 zone 刷新并登记、旧跟随者照旧全量读但不写,
写入仍只有一个来源;领导权来回切时旧版本继续 SCAN,新版本用登记集合 ∪ 快照 zone。回滚到旧二进制行为完全回到原样,
留下的 `scene_manager:zones` 无害。长期环境的一次性补登记步骤见 `docs/ops/scene-node-role-split.md` §6.1。
领导者缺位(最长约一个锁 TTL)时没人刷新,与改动前相同(改动前写入也只由领导者做)。

### A1 剩余缺口

- 仍是全服一个领导者:刷新代价降到 0.4·N_s,但由单点承担;rebalance、分线目录、闲置回收、自动伸缩、Agones 对账这些
  领导者循环仍遍历全部 zone;
- etcd watch 与 `knownNodes` 仍是全服范围(数据面需要),fullSync 的全前缀 Range 与 `resolveNodeEndpoint` 慢路径的全前缀
  Get 不在本项,要等 C++ 改成只连本 zone 的 SM 之后才能收窄;
- `handleWatchEvent` PUT 换 zone 分支在任意副本上 `ZREM` / `DEL`,没有按领导者门控(既有问题,与 reassignSceneNode 无 CAS
  同类,不在本项)。

## Key Redis Keys

| Key | Type | Purpose |
|-----|------|---------|
| `scene:{id}:node` | String | Scene → node mapping |
| `scene:{id}:zone` | String (int) | Scene → zone mapping for cross-zone lookups |
| `scene:{id}:mirror` | String | `"1"` marker for mirror scenes (shorter idle timeout) |
| `scene:{id}:source` | String (int) | A mirror's source scene id, for reverse SREM on destroy |
| `scene:{sourceId}:mirrors` | Set | Mirror scene ids whose source is this scene (cascade destroy) |
| `world_channels:zone:{zoneId}:{confId}` | Set | 该 zone、该世界配置的活动频道 sceneId 集合 |
| `world_channels:desired:zone:{zoneId}` | Hash | confId → 当前期望频道数；配置只作首次播种 |
| `instances:zone:{zoneId}:active` | ZSet | Active instances (score = create/last-active timestamp) |
| `instance:{id}:player_count` | String (int) | Player count per instance |
| `scene_nodes:zone:{zoneId}:load` | ZSet | Node load balancing (score = α·scene_count + β·player_count) |
| `node:zone:{zoneId}:{nodeId}:scene_count` | String (int) | Node's hosted scene count |
| `node:zone:{zoneId}:{nodeId}:player_count` | String (int) | Node's aggregate player count (sum across hosted scenes) |
| `node:zone:{zoneId}:{nodeId}:scene_node_type` | String (int) | Mirrored `eSceneNodeType` value for purpose filtering |
| `node:zone:{zoneId}:{nodeId}:scenes` | Set | Scene ids currently hosted by this node (node-death reconciliation) |
| `scene_manager:zones` | Set | (2026-10-16)出现过负载集成员或频道期望数的 zone(十进制);只加不减,`merge_zone -clear-source-hot-state` 摘除。领导者清扫与孤儿清理靠它发现 zone,替代全库 SCAN |

Scene IDs are generated by the etcd-leased Snowflake worker and therefore have
no Redis counter key. Legacy `node:{nodeId}:*` keys are no longer read: `nodeId`
can repeat across zones, so dual-reading them would reintroduce cross-zone state
aliasing. Rollouts must rebuild/expire those derived counters and reverse indexes
rather than copy an unscopeable legacy value into every zone.

## Configuration

In `etc/scene_manager_service.yaml`:

```yaml
# Main world scenes are discovered from the World table; channel count:
WorldChannelCount: 1                # default channels per world scene
WorldChannelCountByConfId:          # per-confId override
  1001: 4                           # capital city — 4 parallel copies
  1010: 1                           # tutorial — one is plenty

# Instance lifecycle
InstanceIdleTimeoutSeconds: 300     # Auto-destroy idle instances after this (0 = disabled)
MirrorIdleTimeoutSeconds: 30        # Mirrors die faster — every entry re-inits NPC state (0 = fall back)
InstanceCheckIntervalSeconds: 30
MirrorSourceNodeLoadCap: 0          # Soft cap on co-located mirrors per source node (0 = always co-locate)
MirrorDedupBySource: false          # When true, CreateScene with source_scene_id reuses an existing mirror instead of allocating a fresh one. OFF by default — typical mirrors are per-player phasing where independent copies are intentional. Turn ON for "shared instance" semantics (raid lockouts, world bosses).

# Player ownership / Gate route safety
AllowUnsafeCrossNodeHandoff: false  # dev-only bypass of the handoff-marker gate (no epoch minted); production must stay false — cross-zone-scene-travel.md CZ-4
KafkaWriteTimeoutSeconds: 5         # one synchronous RequireOne produce attempt, writer-side timeout

# Node role routing
StrictNodeTypeSeparation: true      # production default; set false in dev / single-node

# Composite load score: score = α·scene_count + β·player_count
NodeLoadWeightSceneCount: 1.0
NodeLoadWeightPlayerCount: 0.01

# World-channel rebalance: how many empty channels to migrate per join/leave
# event. 0 disables proactive rebalance (lazy reassign still fixes dead-node
# mappings on demand). 10 is safe for zones with up to a few hundred channels.
MaxRebalanceMigrationsPerTick: 10

# Prometheus /metrics endpoint (host:port). Empty = disabled.
MetricsListenAddr: ":9150"
```

## File Index

| File | Purpose |
|------|---------|
| `go/scene_manager/internal/logic/createscenelogic.go` | CreateScene routing: main world vs instance |
| `go/scene_manager/internal/logic/world_init.go` | Startup init of persistent world scenes |
| `go/scene_manager/internal/logic/instance_lifecycle.go` | Auto-destroy idle instances + player count helpers. Per-type timeout (mirror vs. regular) via `resolveMirrorTimeout` + `scene:{id}:mirror` flag. Also: atomic CAS destroy, cascade to mirrors, node-death owner-CAS force path (`destroyOrphanOfDeadNode`) |
| `go/scene_manager/internal/logic/scene_atomic.go` | Redis Lua scripts for the race-safe enter / destroy CAS paths |
| `go/scene_manager/internal/logic/scene_node_client.go` | gRPC client cache + RequestNodeCreateScene/DestroyScene |
| `go/scene_manager/internal/logic/enterscenelogic.go` | EnterScene: ownership gate, exact-value location rollback, broker-ACKed Gate routing, player count |
| `go/scene_manager/internal/logic/leavescenelogic.go` | LeaveScene: location cleanup, player count decrement |
| `go/scene_manager/internal/logic/destroyscenelogic.go` | Admin DestroyScene RPC handler |
| `go/scene_manager/internal/logic/load_reporter.go` | etcd → Redis node discovery, load sync, grace period |
| `go/scene_manager/internal/logic/load_refresh.go` | (2026-10-16)领导者按 zone 批量刷新负载分;pipeline 结果按批判定、读失败不按 0 写 |
| `go/scene_manager/internal/logic/zone_registry.go` | (2026-10-16)zone 登记集合 `scene_manager:zones`,替代 fullSync 里的全库 SCAN |
| `go/scene_manager/internal/logic/logic_test.go` | Unit tests incl. `TestCreateScene_Mirror_*` co-location suite |
| `go/scene_manager/internal/logic/world_rebalance.go` | World channel migration. `migrateWorldChannel` uses `reassignSceneNode` to keep `node:zone:{zoneId}:{nodeId}:scenes` in sync with `scene:{id}:node`。2026-10-20 起 `reassignSceneNode`(`world_init.go`)是 Lua CAS:只在属主仍为 oldNode 时同批改归属、两侧反向索引与 scene_count;迁移 CAS 落空时 `destroyUnownedMigrationTarget` 回收 newNode 上预建的实体。`resolveScene` 的死节点自愈仍是无条件 SET,未纳入(2026-10-22 更正:已纳入,`enterscenelogic.go` `reassignSceneOffDeadNode` 改走 `reassignSceneNode`,落空时重读属主;见 PROGRESS 同日「resolveScene 死节点自愈改走 reassignSceneNode CAS」)。2026-10-23 再更正:死节点自愈只重建大世界频道(以 `world_channels` 成员资格确认 conf,提交时经 `reassignWorldChannelNode` 在 CAS 脚本里再核对一次;副本 / 镜像留给死节点收尾销毁),顺序与本行的 rebalance 相同:先建实体、再 CAS、最后转名额;`TransferAgonesRoomForScene` 的记录改写也改为比较并交换,见 PROGRESS 2026-10-23「resolveScene 死节点自愈收窄为只重建频道」 |
| `go/scene_manager/internal/logic/orphan_cleanup.go` | Drops world-channel sets whose confId is no longer in the World table; cleans all scene-scoped keys + reverse indexes |
| `go/scene_manager/internal/metrics/metrics.go` | Prometheus gauges + counters for nodes, mirrors, lifecycle, dedup, source-missing, orphans |
| `proto/scene/scene.proto` | C++ Scene service (CreateScene, DestroyScene) |
| `proto/scene_manager/scene_manager_service.proto` | Go SceneManager service definition |
| `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` | C++ CreateScene/DestroyScene ECS handlers (writes `mirror_config_id` onto `SceneInfoComp`); DestroyScene is idempotent (missing scene → success) |
| `deploy/k8s/scene-manager-alerts.yaml` | PrometheusRule for pool / load / rebalance / mirror lifecycle alerts |
| `deploy/k8s/scene-manager-dashboard.json` | Grafana dashboard mirroring the alert metric set |
