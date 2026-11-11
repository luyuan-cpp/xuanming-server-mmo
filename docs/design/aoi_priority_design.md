# AOI Priority, Capacity & Visibility Design

**Reference**: [如何实现一个强大的MMO技能系统——AOI (kasan)](https://zhuanlan.zhihu.com/p/148077453)

## Core Principle

> 网格只是手段，关注者列表才是结果。
> (The grid is just a means; the interest list is the result.)

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
| `kNormal`   | 0 | Default spatial grid discovery |
| `kTeammate` | 1 | Same `TeamId` component |
| `kCombat`   | 2 | Combat partner: whoever I hit, hit me, healed or was healed by. Registered by `CombatEngagementSystem` as an explicit `kCombat` source, revoked after 10 s without interaction or on death (replaces the never-set `kAttacker`, 2026-11-10) |
| `kQuestNpc` | 3 | Quest/mission system marks NPC (TODO: integrate) |
| `kBoss`     | 4 | Boss / elite monster |
| `kPinned`   | 5 | Highest weight (255) for explicit follows that must never be evicted, e.g. skill tracking |

### Built-in Policies

The tables are static: an entry's weight depends only on its sources, never on the observer's state.
"Combat partners first while fighting, quest NPCs first afterwards" comes from the combat source's
lifetime. Switching tables by observer state was considered and rejected; see
[aoi-directed-interest.md](aoi-directed-interest.md) (Chinese) for the reasons.

| Policy | Weight order (low → high) |
|--------|--------------------------|
| `kPolicyOpenWorld` | normal(0) < teammate(1) < questNpc(3) < boss(4) < combat(5) < pinned(255) |
| `kPolicyDungeon`   | normal(0) < questNpc(1) < teammate(2) < boss(4) < combat(5) < pinned(255) |
| `kPolicyPvpArena`  | normal(0) < questNpc(1) < teammate(2) < boss(3) < combat(4) < pinned(255) |

Scenes attach `ScenePriorityPolicyComp` at creation to pick a policy.
`kPinned` always gets weight 255 across all policies — it can never be evicted
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

Pressure comes from simulation step time (wired 2026-11-11): `AoiLoadMonitor` smooths
step time / fixed step, maps 0.5 → 0 and 0.9 → 1 in 10 levels with hysteresis, and decides at
most once per second. `AoiSystem::ApplyServerPressure` writes it into every scene on the thread;
when capacity shrinks, entries are trimmed lowest weight first, farthest first among equals, and
restored when it grows back. Thresholds still need calibration from stress tests.

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
  state. Moving beyond the leave radius removes only the spatial source; the client gets one removal
  when the last source is gone.

## Spatial Re-evaluation (2026-11-10)

Spatial interest is decided by distance and re-evaluated whenever a position changes, not only when an
entity crosses a hex cell (that left people who approached within the same cells invisible forever).
Enter: 3D distance ≤ view radius (10 m, inclusive), candidates from the current hex and its six
neighbours. Leave: 3D distance > leave radius (view radius × 2 = 20 m), independent of the grid; the
band in between is the hysteresis. A stationary entity is re-evaluated as soon as its position changes;
a moving one after every 1 m of accumulated displacement. Each re-evaluation handles both directions.
Same rules as the Java version. Details: [aoi-directed-interest.md](aoi-directed-interest.md).

## Stealth Visibility

`ViewSystem::CanSee(observer, target)` is the spatial enter check (distance ≤ view radius, then stealth);
`ViewSystem::CanKeepSeeing(observer, target)` keeps an existing spatial source (distance ≤ leave radius,
then stealth). A stealthed target is visible only when one of the observer's explicit sources for it sets
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

## Files Changed

| File | Change |
|------|--------|
| `spatial/constants/aoi_priority.h` | Priority tags, `AoiPriorityPolicy` struct, built-in policies, dynamic capacity constants |
| `spatial/comp/scene_node_scene_comp.h` | `AoiListComp` (entries map), `AoiClientCapacityComp`, `ScenePressureComp`, `ScenePriorityPolicyComp` |
| `spatial/system/interest.h/.cpp` | Policy-aware `AddAoiEntity`, `GetEffectiveCapacity`, `GetPriorityPolicy`, `UpgradePriority` |
| `spatial/constants/view.h` | View radius, leave radius factor, re-evaluation step, hex size |
| `spatial/system/view.h/.cpp` | `CanSee()`, `CanKeepSeeing()`, `IsStealthed()` |
| `spatial/system/aoi.cpp` | Grid membership, distance re-evaluation with `CanSee` / `CanKeepSeeing` / `DetermineAoiPriority`, explicit follow entry points, netted visibility delivery |
| `actor/attribute/system/actor_state_attribute_sync.cpp` | Updated for `entries` API |
| `tests/aoi_test/*` | Priority, capacity, pin/team, dynamic capacity, policy tests |

## Future Work

- **Threat values for AI**: NPC threat rosters (2026-11-11) only record members, so fighters of the
  same NPC see each other as allies. Add threat values when AI target selection needs them. The
  server does not spawn NPCs yet, so rosters take effect once spawning lands.
- **Quest NPC priority**: Hook mission accept to call
  `InterestSystem::UpgradePriority(player, npc, kQuestNpc)`.
- **Boss tagging**: Scene creation marks boss entities; grid entry detects and
  assigns `kBoss` tag.
- **Pressure calibration**: the pressure monitor and weight-ordered trimming are wired
  (2026-11-11); calibrate the thresholds with stress tests and decide whether RPC handling time
  should feed the signal too.
- **Client settings RPC**: Wire client-reported display count to
  `AoiClientCapacityComp`.
- **Custom policies**: Data-driven policy loading from configuration tables.
