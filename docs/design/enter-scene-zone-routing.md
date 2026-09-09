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

### Login-side zone resolution (added 2026-09-08)

Three notions of "zone" coexist and only one is authoritative:

| notion | where | who writes it | role after merge |
|---|---|---|---|
| physical zone | k8s namespace (gate/scene/login/scene_manager), picked by the client via server list + `/api/assign-gate` | deploy | may be decommissioned |
| **home_zone** | data_service `player:zone:{player_id}` | `RegisterPlayerZone` at create; `tools/merge_zone` / `RemapHomeZoneForMerge` on merge | **single source of truth** for data routing, guild, leaderboard |
| creation-time `zone_id` | account blob `AccountSimplePlayer.zone_id` (`proto/common/.../user_accounts.proto`) | `createplayerlogic.go`, once | hint only |

Rules (implemented in `go/login/internal/logic/pkg/homezone`, wired via `ServiceContext.HomeZone`):

1. **Role list zone = current home_zone resolved at login.** `Login` resolves every role's `player_id` with one `BatchGetPlayerHomeZone` (bounded, `HomeZone.LookupTimeout`, default 1.5s) and overwrites `zone_id` in the *response copy* when the mapping has a non-zero zone. Ids absent from the mapping or a failed RPC keep the stored creation-time value; login never fails because of it. The refreshed zone is **not** written back into the account blob — the mapping stays the only truth, and a second copy would fight it on the next merge/rollback.
2. **EnterGame routes by home_zone via the existing cross-zone redirect.** Before `SceneManager.EnterScene`, `EnterGame` resolves `GetPlayerHomeZone(player_id)`. If it is non-zero and differs from `Node.ZoneId`, the request carries `ZoneId = home_zone`, `GateZoneId = login's zone`, `SceneId = 0`, so `enterscenelogic.go`'s `crossZoneRedirect` fires and the gate pushes `RedirectToGateEvent`. Lookup failure / zero → today's behaviour (`ZoneId = own zone`) with a WARN. `SceneId` is forced to 0 on redirect because `resolveScene` rejects a scene whose `scene:{id}:zone` is the old zone before the redirect check is reached.
3. **Account blob `zone_id` is a creation-time hint only.** Nothing should route on it; merge tooling does not need to touch the account system.

Switches: `HomeZone.RefreshRoleListDisabled` / `HomeZone.RedirectOnEnterDisabled` in `go/login/etc/login.yaml` (reverse-named; an absent block means both enabled, same rationale as `KillSwitch`).

Known limits on the scene_manager side (not changed here): the redirect is evaluated **after** `resolveSceneForEnter`, so the target zone must have at least one live world channel, and a stale `player:{id}:location` from the old zone combined with `AllowUnsafeCrossNodeHandoff=false` returns `ErrUnsafeCrossNodeHandoff` instead of redirecting — merge maintenance must clear that hot state (see bullet above).
