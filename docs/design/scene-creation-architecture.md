# Scene Creation Architecture

**Updated:** 2026-08-03 (rev 4 — zone-scoped node identity + safe EnterScene commit)

## Overview

The scene system supports two scene types managed by Go SceneManager and hosted on C++ SceneNodes:
- **Main World** scenes: persistent, one per config ID per zone, created at startup.
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
| Main World | `SceneTypeMainWorld=1` | Persistent. Created at startup via `InitWorldScenes`. Idempotent (returns existing if already created). Never auto-destroyed. |
| Instance | `SceneTypeInstance=2` | On-demand. Unique per request. Tracked in Redis sorted set. Auto-destroyed after `InstanceIdleTimeoutSeconds` (default 300s) with 0 players. Mirror instances (`MirrorConfigId > 0` or `SourceSceneId > 0`) use the shorter `MirrorIdleTimeoutSeconds` (default 30s) — see [Mirror vs. instance idle timeouts](#mirror-vs-instance-idle-timeouts). |

Auto-detection: if `scene_type` is unset in the request, the system checks `WorldConfIds` config. If the `scene_conf_id` is in that list → main world; otherwise → instance.

## Create Flow

```
Client/System → Go SceneManager.CreateScene(scene_conf_id)
│
├─ resolveSceneType (auto-detect or explicit)
│
├─ Main World path:
│  ├─ Check Redis hash → return existing (idempotent)
│  ├─ getNodesForPurpose(World)  # filters by scene_node_type
│  ├─ allocateScene → SceneIDGen.Generate(Snowflake), SET scene:{id}:node
│  ├─ HSET world_channels:zone:{zoneId} confId → sceneId
│  └─ RequestNodeCreateScene → gRPC → C++ Scene.CreateScene(config_id)
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

The lookup is exposed as `Config.ChannelCountFor(confId)` and consulted
per confId in `initWorldScenesForZone`.

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
   player entity, require the player to currently be in a non-mirror source
   scene (refuse to mirror a mirror), require `PlayerSessionSnapshotComp`
   for gate routing, and require an available SceneManager node entity.
2. **Build CreateSceneRequest** with `scene_conf_id = source.scene_config_id`,
   `scene_type = SCENE_TYPE_INSTANCE`, `zone_id = GetZoneId()`,
   `source_scene_id = source.scene_id` (drives co-location),
   `mirror_config_id = mirrorConfigId`, and `creator_ids = [player_id]`.
3. **Fire-and-forget** via `scene_manager::SendSceneManagerCreateScene` — no
   per-call state retained on the SceneNode.
4. **Async response** lands in `AsyncSceneManagerCreateSceneHandler` in
   `cpp/nodes/scene/rpc_replies/scene_manager_response_handler.cpp`. On
   success, the handler iterates `resp.creator_ids()` and dispatches a
   `SceneManagerEnterScene` for each creator that is still resident on this
   node. Players that disconnected between fire and reply are skipped — when
   they reconnect, normal `EnterScene` flows handle re-entry.

This pattern keeps the SceneNode stateless across the round trip; the echoed
`creator_ids` is the entire correlation key. It also means a SceneManager
that legitimately reuses an existing mirror via `MirrorDedupBySource` still
auto-routes the requesting player(s) into that mirror.

### C++ Side (Scene.CreateScene handler)

1. **Idempotency check**: scans `sceneRegistry.view<SceneInfoComp>()` for existing entity with same `config_id`. If found, returns existing info without creating — safe to call repeatedly.
2. `tlsEcs.sceneRegistry.create()` — creates entt entity (only if no duplicate).
3. Emplaces `SceneInfoComp` (config_id, guid) and `ScenePlayers` (empty set).
4. Fires `OnSceneCreated` event for downstream handlers (AOI, etc.).

## Destroy Flow

```
Go SceneManager.DestroyScene(scene_id) or InstanceLifecycleManager
│
├─ [cascade] SMEMBERS scene:{id}:mirrors → destroyInstanceForce(each)
├─ GET scene:{id}:node → resolve nodeId
├─ RequestNodeDestroyScene → gRPC → C++ Scene.DestroyScene(scene_id)
├─ [atomic] EVAL destroy-if-idle (see "Destroy-while-entering race")
│    └─ DEL scene:{id}:{node, player_count, mirror, source, zone}
│       + ZREM instances:zone:{zoneId}:active
├─ SREM node:zone:{zoneId}:{nodeId}:scenes
├─ [if mirror] SREM scene:{sourceId}:mirrors
└─ INCRBY node:zone:{zoneId}:{nodeId}:scene_count -1 + drain residual from player_count
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
- **`luaAtomicDestroyInstance`** (lifecycle + DestroyScene path): if
  `player_count > 0`, returns `""` without touching anything; otherwise
  wipes every scene-scoped key, `ZREM`s from the active set, and returns
  the hosted nodeId so the caller can fire `DestroyScene` RPC and drain
  counters. A force path (`destroyInstanceForce`) bypasses the idle check
  for admin teardowns and node-death reconciliation.

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
`SMEMBERS scene:{id}:mirrors` BEFORE the atomic wipe and force-destroy
every child. This prevents mirrors from living on a dead source (their
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

`load_reporter.go::removeNodeFromRedis` now finishes with
`reconcileDeadNodeScenes`:

1. `SMEMBERS node:zone:{zoneId}:{deadNodeId}:scenes`.
2. For each sceneId: if `scene:{id}:node` still points at the dead node
   AND the scene is in `instances:zone:{z}:active` (i.e. it's an
   instance, not a world channel) → `destroyInstanceForce(reason="node_death")`.
3. Reassigned scenes (mapping already points at a live node) and world
   channels are skipped — the rebalance pipeline handles them.
4. `DEL node:zone:{zoneId}:{nodeId}:scenes`.

World channels are intentionally excluded here; destroying them would
drop every player in that shard. The existing rebalance path already
moves them to a new node and re-issues CreateScene on the C++ side.

Regression: `TestReconcileDeadNode_{DestroysOrphanInstances,PreservesWorldChannels,SkipsReassignedScenes}`.

### Observability (Prometheus)

Added counters alongside the existing gauges in
`scene_manager/internal/metrics/metrics.go`:

| Metric | Labels | Emitted by |
|--------|--------|-----------|
| `scene_manager_mirror_colocate_total` | `zone_id`, `outcome` (hit\|fallback), `reason` (ok\|no_mapping\|zone_mismatch\|node_dead\|overloaded) | `pickInstanceNode` |
| `scene_manager_instance_destroyed_total` | `zone_id`, `kind` (instance\|mirror), `reason` (idle\|explicit\|cascade\|node_death\|source_migrated) | `destroyInstance*`, `DestroyScene`, `migrateWorldChannel` |
| `scene_manager_enter_scene_rejected_total` | `zone_id`, `reason` (scene_gone) | `EnterScene` on CAS reject |
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

Alert on sustained `enter_scene_rejected_total{reason="scene_gone"}` > 0
(sign of an aggressive idle timeout or a misbehaving reconciliation
pass) and on nonzero `mirror_source_missing_total` (a C++ caller
requested a source-clone mirror — currently refused server-side; each
event indicates a client-side contract bug).

### C++ Side (Scene.DestroyScene handler)

1. Linear scan `sceneRegistry` for entity matching `scene_id`.
2. If not found, log `"DestroyScene: scene_id=N not found, idempotent OK"` and return success.
3. Otherwise: fire `OnSceneDestroyed` event, then `sceneRegistry.destroy(entity)` — removes all components.

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

#### 2. Reconciliation Loop（K8s Controller 模式）

**K8s 原理**：Controller 不记录"我做过什么操作"的步骤状态机，而是：
- 声明 **期望状态**（Desired State）：例如 Deployment spec 声明要 3 个 Pod。
- 观察 **当前状态**（Current State）：查看实际 Pod 数量。
- **收敛**（Reconcile）：计算 diff，执行动作让 current → desired。
- 关键特性：**幂等**。执行多次结果一样。不管中间崩溃多少次，下次运行时重新 observe + reconcile 即可自愈。

**本项目映射**：

| K8s Controller 概念 | 本项目实现 |
|--------------------|-----------|
| Desired State | `WorldConfIds` 配置："每个 zone 应该有这些主场景" |
| Current State | Redis `world_channels:zone:{zoneId}` 哈希 + C++ ECS registry |
| Reconcile 函数 | `initWorldScenesForZone()` — 对比 + 收敛 |
| Reconcile 触发方式 | Watch PUT event（增量）或 fullSync（全量） |
| 幂等保证 | Redis 层跳过已有 ID + C++ CreateScene 按 config_id 去重 |

```
Reconcile (initWorldScenesForZone):

  for each confId in WorldConfIds:
      ┌─ Redis Hget(world_channels:zone:X, confId)
      │
      ├─ 不存在 → 分配 scene ID (INCR), 写 Redis (HSET)
      │           └─ 调 C++ CreateScene RPC → 创建 ECS entity
      │
      └─ 已存在 → 跳过 ID 分配
                  └─ 仍然调 C++ CreateScene RPC → C++ 幂等返回已有 entity
                     (覆盖 C++ 重启后 ECS 为空的情况)
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

1. **C++ CreateScene idempotent by `config_id`**: Before creating a new entity, scans `sceneRegistry.view<SceneInfoComp>()` for an existing entity with the same `scene_confid`. If found, returns the existing scene info. This makes the RPC safe to call any number of times.

2. **Go `initWorldScenesForZone` dual-layer idempotency**:
   - Redis layer: `Hget` checks if `confId` already has a scene ID → skips ID allocation.
   - C++ layer: **always sends `CreateScene` RPC** regardless of Redis state (C++ deduplicates).
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
3. `initWorldScenesForZone`: iterates `WorldConfIds`, performs Redis idempotent check (skip ID allocation if exists), always sends C++ CreateScene RPC, assigns nodes via FNV-1a consistent hashing.

## Key Redis Keys

| Key | Type | Purpose |
|-----|------|---------|
| `scene:{id}:node` | String | Scene → node mapping |
| `scene:{id}:zone` | String (int) | Scene → zone mapping for cross-zone lookups |
| `scene:{id}:mirror` | String | `"1"` marker for mirror scenes (shorter idle timeout) |
| `scene:{id}:source` | String (int) | A mirror's source scene id, for reverse SREM on destroy |
| `scene:{sourceId}:mirrors` | Set | Mirror scene ids whose source is this scene (cascade destroy) |
| `world_channels:zone:{zoneId}` | Hash | confId → sceneId for main worlds |
| `instances:zone:{zoneId}:active` | ZSet | Active instances (score = create/last-active timestamp) |
| `instance:{id}:player_count` | String (int) | Player count per instance |
| `scene_nodes:zone:{zoneId}:load` | ZSet | Node load balancing (score = α·scene_count + β·player_count) |
| `node:zone:{zoneId}:{nodeId}:scene_count` | String (int) | Node's hosted scene count |
| `node:zone:{zoneId}:{nodeId}:player_count` | String (int) | Node's aggregate player count (sum across hosted scenes) |
| `node:zone:{zoneId}:{nodeId}:scene_node_type` | String (int) | Mirrored `eSceneNodeType` value for purpose filtering |
| `node:zone:{zoneId}:{nodeId}:scenes` | Set | Scene ids currently hosted by this node (node-death reconciliation) |

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
AllowUnsafeCrossNodeHandoff: false  # production invariant until handoff epoch exists
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
| `go/scene_manager/internal/logic/instance_lifecycle.go` | Auto-destroy idle instances + player count helpers. Per-type timeout (mirror vs. regular) via `resolveMirrorTimeout` + `scene:{id}:mirror` flag. Also: atomic CAS destroy, cascade to mirrors, node-death force path |
| `go/scene_manager/internal/logic/scene_atomic.go` | Redis Lua scripts for the race-safe enter / destroy CAS paths |
| `go/scene_manager/internal/logic/scene_node_client.go` | gRPC client cache + RequestNodeCreateScene/DestroyScene |
| `go/scene_manager/internal/logic/enterscenelogic.go` | EnterScene: ownership gate, exact-value location rollback, broker-ACKed Gate routing, player count |
| `go/scene_manager/internal/logic/leavescenelogic.go` | LeaveScene: location cleanup, player count decrement |
| `go/scene_manager/internal/logic/destroyscenelogic.go` | Admin DestroyScene RPC handler |
| `go/scene_manager/internal/logic/load_reporter.go` | etcd → Redis node discovery, load sync, grace period |
| `go/scene_manager/internal/logic/logic_test.go` | Unit tests incl. `TestCreateScene_Mirror_*` co-location suite |
| `go/scene_manager/internal/logic/world_rebalance.go` | World channel migration. `migrateWorldChannel` uses `reassignSceneNode` to keep `node:zone:{zoneId}:{nodeId}:scenes` in sync with `scene:{id}:node` |
| `go/scene_manager/internal/logic/orphan_cleanup.go` | Drops world-channel sets whose confId is no longer in the World table; cleans all scene-scoped keys + reverse indexes |
| `go/scene_manager/internal/metrics/metrics.go` | Prometheus gauges + counters for nodes, mirrors, lifecycle, dedup, source-missing, orphans |
| `proto/scene/scene.proto` | C++ Scene service (CreateScene, DestroyScene) |
| `proto/scene_manager/scene_manager_service.proto` | Go SceneManager service definition |
| `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` | C++ CreateScene/DestroyScene ECS handlers (writes `mirror_config_id` onto `SceneInfoComp`); DestroyScene is idempotent (missing scene → success) |
| `deploy/k8s/scene-manager-alerts.yaml` | PrometheusRule for pool / load / rebalance / mirror lifecycle alerts |
| `deploy/k8s/scene-manager-dashboard.json` | Grafana dashboard mirroring the alert metric set |
