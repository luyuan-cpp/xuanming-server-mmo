# EnterScene Zone Routing Design (updated 2026-08-03)

## Problem
`EnterSceneRequest` had `zone_id` and `gate_zone_id` fields but:
1. Login never passed `zone_id` → `resolveScene` looked up `world_channels:zone:0:xxx` → miss
2. `PlayerLocation` in Redis had no `zone_id` → no way to know what zone a player was last in
3. Scene creation didn't store `scene:{id}:zone` → no way to reverse-lookup a scene's zone

## Solution: Zone Resolution Chain
SceneManager determines `targetZoneId` via a priority-ordered fallback chain:
1. `in.ZoneId` — caller explicitly says "go to this zone" (cross-zone teleport, login)
2. `scene:{id}:zone` Redis lookup — when `sceneId != 0` (reconnect, follow-leader)
3. `PlayerLocation.zone_id` — player's last known zone (offline fallback)
4. `in.GateZoneId` — gate's zone (first-ever login, everything else is 0)

Cross-zone check: `GateZoneId != 0 && targetZoneId != 0 && GateZoneId != targetZoneId` → redirect.

## Changes Made
| Component | Change |
|-----------|--------|
| `storage.proto` / `PlayerLocation` | Added `zone_id` field 4 |
| `changesceneutil.go` | `UpdatePlayerLocation` takes `zoneId`; added `GetSceneZone()` |
| `world_init.go` | Writes `scene:{id}:zone` on world scene creation |
| `createscenelogic.go` | `allocateScene` takes `zoneId`, writes `scene:{id}:zone` |
| `enterscenelogic.go` | Zone resolution chain; `handleCrossZoneRedirect` takes `targetZoneId` |
| Login `entergamelogic.go` | Passes `ZoneId = config.Node.ZoneId` |
| C++ `player_scene.cpp` | Passes `zone_id = GetZoneId()` for follow-leader |

## Redis Keys
- `scene:{id}:zone` — uint32 zone_id for each scene instance
- `player:{id}:location` — protobuf `PlayerLocation` (scene_id, node_id, update_time, zone_id)
- `world_channels:zone:{zoneId}:{confId}` — SET of sceneId strings per zone/conf

## Cross-Zone Flow and Current Safety Gate

The redirect transport exists, but it is not a persistence handoff protocol.

- **First landing (no `PlayerLocation`)**: a request received on a zone B Gate
  for zone C may return `RedirectToGateInfo`; no old Scene ownership exists to
  transfer. The redirect is successful only after its Gate Kafka command gets a
  broker ACK.
- **Existing location**: production defaults to fail-closed with
  `ErrUnsafeCrossNodeHandoff`. SceneManager releases any target reservation and
  does not release the old entity, update location, or report a successful
  route/redirect. `ReleasePlayer` only queues an asynchronous save and cannot
  prove the old snapshot is durable before another node loads it.
- `AllowUnsafeCrossNodeHandoff=true` restores the legacy flow for development
  protocol exercises only. It is not a production capability claim.

Re-enabling existing-player cross-zone travel requires a per-player handoff
epoch: the old node publishes the epoch only after durable save, and the target
node verifies that epoch before loading.

## Offline-Return Behavior
Player belongs to zone A and previously had a location in zone C:

- A clean logout removes the location; the next login is a first landing and can
  enter zone A normally.
- If a crash left the zone C location behind, Login(A)'s explicit `ZoneId=A`
  resolves the desired target but the ownership gate rejects the cross-node
  transition. It does **not** silently return the player home. Recovery must
  clear/reconcile stale ownership or complete the handoff-epoch protocol.

## Merge-Server (合服)
`zone_id` is a **runtime routing identifier**, not a permanent identity:
- **Home zone (authoritative for data routing)**: run `tools/merge_zone` (or `DataService/RemapHomeZoneForMerge`) to rewrite `player:zone:*` in mapping Redis; see `docs/design/guild_ranking_architecture_zh.md` §合服工具.
- **Guild / 本服榜**: same tool updates MySQL `guild.zone_id` and merges `guild_rank:zone:{id}`.
- **SceneManager hot state** (`scene:{id}:zone`, `player:{id}:location`, world channel sets): normally cleared or left to refresh on next login after a maintenance window; coordinate with ops (stale keys can cause wrong routing until overwritten).
