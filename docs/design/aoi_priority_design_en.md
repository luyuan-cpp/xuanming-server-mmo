# AOI Priority, Capacity & Visibility Design

**Reference**: [如何实现一个强大的MMO技能系统——AOI (kasan)](https://zhuanlan.zhihu.com/p/148077453)

## Core Principle

> The grid is just a means; the interest list is the result.

AOI spatial queries (hex grid neighbors) populate the **interest list**, but the
interest list can also be manipulated directly by skills, buffs, and other
game systems. The client displays whatever is in the interest list — not
the raw grid contents.

## Priority Tags & Policy-Based Weighting

Priority is split into two layers:
1. **Semantic tags** (`AoiPriority` enum) — what category an entity belongs to.
2. **Priority policy** (`AoiPriorityPolicy`) — scene-specific weight table
   that assigns an eviction weight to each tag.

### Tags

| Tag          | Value | Source |
|-------------|-------|--------|
| `kNormal`   | 0 | Default: entities discovered via spatial grid query |
| `kTeammate` | 1 | Same `TeamId` component |
| `kAttacker` | 2 | Combat/threat system (TODO: integrate) |
| `kQuestNpc` | 3 | Quest/mission system marks NPC (TODO: integrate) |
| `kBoss`     | 4 | Boss / elite monster |
| `kPinned`   | 5 | Highest weight (255) for explicit follows that must never be evicted, e.g. skill tracking |

### Built-in Policies

| Policy | Weight order (low → high) |
|--------|--------------------------|
| `kPolicyOpenWorld` | normal(0) < teammate(1) < attacker(2) < questNpc(3) < boss(4) < pinned(255) |
| `kPolicyDungeon`   | normal(0) < questNpc(1) < teammate(2) < attacker(3) < boss(4) < pinned(255) |
| `kPolicyPvpArena`  | normal(0) < questNpc(1) < teammate(2) < boss(3) < attacker(4) < pinned(255) |

Scenes attach `ScenePriorityPolicyComp` at creation to pick a policy.
`kPinned` always gets weight 255 across all policies — it can **never** be evicted
by spatial logic.

### Eviction Rule

When the list is full, `InterestSystem::AddAoiEntity` compares the **policy
weight** of the new entity against the lowest-weight existing entry:
- New entity's weight > lowest entry's weight → evict lowest, insert new.
- Otherwise → reject the new entity.

## Dynamic AOI Capacity

Capacity is no longer a compile-time constant. It is computed per-entity
per-frame:

```
effectiveCapacity = min(clientReportedCapacity, serverPressureCapacity)
```

### Components

| Component | Scope | Purpose |
|-----------|-------|---------|
| `AoiClientCapacityComp` | Per-entity (actor) | Client-reported desired display count |
| `ScenePressureComp` | Per-scene | Server pressure factor → derived capacity ceiling |

### Constants

| Constant | Value | Purpose |
|----------|-------|---------|
| `kAoiListCapacityDefault` | 100 | Fallback when no capacity components exist |
| `kAoiListCapacityMin` | 20 | Floor — never go below this even under max pressure |
| `kAoiListCapacityMax` | 200 | Ceiling — client can never request more |

### Server Pressure

`ScenePressureComp.pressureFactor` ∈ [0, 1]:
- 0 = idle → server capacity = `kAoiListCapacityMax` (200)
- 1 = max pressure → server capacity = `kAoiListCapacityMin` (20)
- Linear interpolation between the two.

The scene's tick/load monitor sets `pressureFactor` each frame (integration
TODO).

### Client Reported Count

The client sends a desired display count via a settings RPC. The server
stores it in `AoiClientCapacityComp.clientDesiredCount` and clamps it to
[`kAoiListCapacityMin`, `kAoiListCapacityMax`].

### Resolution

`InterestSystem::GetEffectiveCapacity(watcher)` returns
`min(clamped_client, server_pressure_cap)`.

## Explicit Follows

Requirements and acceptance: [aoi-directed-interest.md](aoi-directed-interest.md). The reserved Pin/Unpin
idea is replaced by explicit follows; the Chinese version of this document has the full rules.

- Gameplay calls `AoiSystem::Follow(watcher, target, grant)` / `AoiSystem::Unfollow(watcher, target, source)`.
  The target must be in the same scene and may be outside the hex grid range.
- `AoiFollowGrant` carries the source identity (`AoiFollowSource`: kind + instance id), an eviction
  priority tag and an optional stealth-detection grant. Re-adding a source updates it in place;
  unfollow revokes only that source.
- One relation may have a spatial source and several explicit sources but only one client-visible
  state. Leaving the grid removes only the spatial source; the client gets one removal when the last
  source is gone.

## Stealth Visibility

`ViewSystem::CanSee(observer, target)` is the spatial enter check: distance, then stealth.
A stealthed target is visible only when one of the observer's explicit sources for it sets
`revealsStealth`. When the stealth tag appears or disappears, `BuffSystem` calls
`AoiSystem::RefreshTargetVisibility`: watchers without detection get one removal (explicit
sources are kept), and on reveal paused follows plus nearby observers get a fresh create with the
current position immediately.

## Directionality and Delivery

Interest is **unidirectional**. Two indexes describe the same directed relations and are written only
by `InterestSystem`: `AoiListComp` on the watcher (forward) and `AoiWatchersComp` on the target
(reverse). Movement, attribute sync and skill broadcasts go to the target's visible watchers that have
a gateway session, including explicit followers outside the grid. Leaving the scene or being destroyed
goes through `AoiSystem::OnLeaveScene`, which removes every in/out edge through the two indexes without
scanning the node.

## Key APIs

| Method | Description |
|--------|-------------|
| `InterestSystem::AddAoiEntity(watcher, target, priority)` | Uses policy weights for comparison, dynamic capacity for limit |
| `InterestSystem::RemoveAoiEntity(watcher, target)` | Remove target from watcher's interest list |
| `InterestSystem::UpgradePriority(watcher, target, priority)` | Upgrade priority (compares policy weights, not raw enum values) |
| `InterestSystem::GetEffectiveCapacity(watcher)` | Dynamically resolve effective capacity |
| `InterestSystem::GetPriorityPolicy(watcher)` | Get scene's policy (default: open-world) |
| `ViewSystem::CanSee(observer, target)` | Full visibility check: distance + stealth state |
| `ViewSystem::IsStealthed(entity)` | Check for active stealth buff |

## Files Changed

| File | Change |
|------|--------|
| `spatial/constants/aoi_priority.h` | Priority tags, `AoiPriorityPolicy` struct, built-in policies, dynamic capacity constants |
| `spatial/comp/scene_node_scene_comp.h` | `AoiListComp` (entries map), `AoiClientCapacityComp`, `ScenePressureComp`, `ScenePriorityPolicyComp` |
| `spatial/system/interest.h/.cpp` | Policy-aware `AddAoiEntity`, `GetEffectiveCapacity`, `GetPriorityPolicy`, `UpgradePriority` |
| `spatial/system/view.h/.cpp` | `CanSee()`, `IsStealthed()` |
| `spatial/system/aoi.cpp` | Grid evaluation with `CanSee` / `DetermineAoiPriority`, explicit follow entry points, netted visibility delivery |
| `actor/attribute/system/actor_state_attribute_sync.cpp` | Updated for `entries` API |
| `tests/aoi_test/*` | Priority, capacity, pin/team, dynamic capacity, policy tests |

## Future Work

- **Attacker priority**: Hook combat/damage system to call
  `InterestSystem::UpgradePriority(target, attacker, kAttacker)` on hit.
- **Quest NPC priority**: Hook mission accept to call
  `InterestSystem::UpgradePriority(player, npc, kQuestNpc)`.
- **Boss tagging**: Scene creation marks boss entities; grid entry detects and
  assigns `kBoss` tag.
- **Pressure integration**: Scene tick monitors entity count / CPU load and
  writes `ScenePressureComp.pressureFactor`.
- **Client settings RPC**: Wire client-reported display count to
  `AoiClientCapacityComp`.
- **Hysteresis / grace period**: Add a grace period before removing entities
  that oscillate across grid boundaries.
- **Custom policies**: Data-driven policy loading from configuration tables.
