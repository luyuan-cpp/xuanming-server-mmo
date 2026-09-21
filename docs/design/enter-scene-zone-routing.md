# EnterScene Zone Routing Design (updated 2026-08-03; handoff-gate, login-side and merge sections re-verified against code 2026-09-20)

## Problem
`EnterSceneRequest` had `zone_id` and `gate_zone_id` fields but:
1. Login never passed `zone_id` → `resolveScene` looked up `world_channels:zone:0:xxx` → miss
2. `PlayerLocation` in Redis had no `zone_id` → no way to know what zone a player was last in
3. Scene creation didn't store `scene:{id}:zone` → no way to reverse-lookup a scene's zone

## Solution: Zone Resolution Chain
SceneManager determines `targetZoneId` via a priority-ordered fallback chain:
1. `in.ZoneId` — caller explicitly says "go to this zone" (cross-zone teleport, login)
2. `scene:{id}:zone` Redis lookup — when `sceneId != 0` (reconnect, follow-leader)
3. `PlayerLocation.zone_id` — player's last known zone (offline fallback). Skipped when the record is ignored as stale (owner zone / node gone, see "Cross-Zone Flow") or is an awaiting-placement record older than the redirect ticket TTL (300s)
4. `in.GateZoneId` — gate's zone (first-ever login, everything else is 0)

Cross-zone check: `GateZoneId != 0 && targetZoneId != 0 && GateZoneId != targetZoneId` → redirect. It is decided **before** the target scene is resolved (`enterscenelogic.go` step 2), and the redirect path takes no scene reservation.

## Changes Made
| Component | Change |
|-----------|--------|
| `storage.proto` / `PlayerLocation` | Added `zone_id` field 4 |
| `changesceneutil.go` | `UpdatePlayerLocation` takes `zoneId`; added `GetSceneZone()` |
| `world_init.go` | Writes `scene:{id}:zone` on world scene creation |
| `createscenelogic.go` | `allocateScene` takes `zoneId`, writes `scene:{id}:zone` |
| `enterscenelogic.go` | Zone resolution chain; `handleCrossZoneRedirect` takes `targetZoneId` |
| Login `entergamelogic.go` | Passes `ZoneId = config.Node.ZoneId` (or `home_zone`, see "Login-side zone resolution") |
| C++ scene `EnterScene` callers | `player_scene_handler.cpp` / `player_lifecycle.cpp` (relocate) / `scene_manager_response_handler.cpp` pass `zone_id = GetZoneId()`; follow-leader (`player_team.cpp`) passes the leader's `zone_id`; zone travel (`player_lifecycle.cpp`) passes the target zone |

## Redis Keys
- `scene:{id}:zone` — uint32 zone_id for each scene instance
- `player:{id}:location` — protobuf `PlayerLocation` (scene_id, node_id, update_time, zone_id, owner_epoch, pending_scene_conf_id). `node_id == ""` with `owner_epoch != 0` means "cross-zone handoff granted, awaiting placement in `zone_id`": no node holds the player
- `player:{id}:owner_epoch` — decimal ownership generation. Minted (`INCR`) only by `EnterScene`, in the same Lua step that writes the location; **never deleted**, not even by `LeaveScene`
- `player:{id}:handoff` — `"{epoch}:{saved_at_ms}"`, EX 300, written by the source Scene after its Redis save landed; SceneManager only compares it
- `world_channels:zone:{zoneId}:{confId}` — SET of sceneId strings per zone/conf

The three `player:{id}:*` keys are read/written by multi-key Lua and must live in the same Redis instance (no hash-tag, no Redis Cluster); see `cross-zone-scene-travel.md` §10.3.

## Cross-Zone Flow and Current Safety Gate

The redirect transport is now backed by a persistence handoff protocol: a
per-player `owner_epoch` plus a "source has saved" marker, both judged in
`EnterScene` (the only ownership gate). Design and rationale:
`cross-zone-scene-travel.md` CZ-4 / CZ-5, §10.2, §11.2, §11.5. All not-yet-safe
handoffs are rejected with the retryable `ErrHandoffPending` (18);
`ErrUnsafeCrossNodeHandoff` (14) is a retired code that `EnterScene` no longer
emits.

- **First landing (no `PlayerLocation`)**: a request received on a zone B Gate
  for zone C may return `RedirectToGateInfo`; no old Scene ownership exists to
  transfer, so this is a connection-only redirect (no handoff check, no location
  write, no epoch mint). The second leg in zone C places the player and mints the
  epoch. The redirect is successful only after its Gate Kafka command gets a
  broker ACK.
- **Existing location, target zone == the zone the player is already in**: also
  connection-only; ownership does not change.
- **Existing location, leaving its zone or moving to another node**: granted
  only if the source Scene has written `player:{id}:handoff` for the **current**
  `player:{id}:owner_epoch` (it does so after its Redis save landed). Otherwise
  SceneManager answers 18, releases any target reservation and
  does not release the old entity, update location, or report a successful
  route/redirect. `ReleasePlayer` alone still only queues an asynchronous save and cannot
  prove the old snapshot is durable; the marker is that proof. On grant, one Lua
  step re-checks epoch and marker, mints `owner_epoch + 1` and writes the
  location — for a cross-zone move as an awaiting-placement record
  (`zone_id = target`, `node_id = ""`, `pending_scene_conf_id`), which the
  second leg places without a handoff check because nobody holds the player.
- **Existing location whose owner is gone**: the record is treated as absent
  (so placement is a first landing and always mints a new epoch) when either
  the whole zone of the record has an empty `scene_nodes:zone:{z}:load`
  (`playerLocationOwnerGone`: merged / decommissioned zone) or that one node is
  deregistered from etcd (`playerLocationOwnerDead`, §11.5) — in both cases
  only after the re-entry barrier. Anything uncertain (zone unknown, ambiguous
  node identity, Redis error, barrier not elapsed) stays fail-closed on 18.
- `AllowUnsafeCrossNodeHandoff=true` is a development bypass only: without a
  marker it lets the move through **without minting** (legacy race semantics).
  It is not a production capability claim.

## Offline-Return Behavior
Player belongs to zone A and previously had a location in zone C:

- A clean logout removes the location (player_locator's `MarkOffline`, or its
  lease monitor after a disconnect lease expires, calls `LeaveScene`, which
  deletes `player:{id}:location` and keeps `player:{id}:owner_epoch`); the next
  login is a first landing and can enter zone A normally.
- A clean **disconnect** (not a logout) keeps the zone C location for the
  disconnect lease (30s; AFK passes extend it). A login inside that window is
  `ShortReconnect` (another live device: `ReplaceLogin`), and since 2026-09-21
  login sends `ZoneId=0, SceneId=0` for both (GO-5,
  `cross-zone-scene-travel.md` §12.7): scene_manager follows the location, so
  the player goes back to zone C — through a cross-zone redirect when they
  logged in from zone A's entry. The session record is in the shared Redis
  (`cross-zone-matchmaking.md` D12), so Login(A) does see zone C's
  disconnecting session. Once the exit save has converged, the zone C Scene
  writes a release marker (A1′, `"E:ms"`, EX 300), so a reconnect placed on a
  different node passes the handoff gate instead of waiting out the lease; the
  node that loads the player clears it again under an owner_epoch check (A2′).
- If a crash left the zone C location behind, Login(A)'s explicit `ZoneId=A`
  resolves the desired target and the outcome depends on the recorded owner:
  - owner confirmed dead (node deregistered from etcd, or all of zone C down)
    and re-entry barrier elapsed → the record is ignored and the player lands
    in zone A under a new epoch; progress the dead node never saved is lost
    (crash-inherent);
  - owner alive or not provably dead → retryable `ErrHandoffPending` (18),
    nothing modified. It does **not** silently return the player home. It
    clears once the source Scene writes the marker (for moves it initiates
    itself: scene change, zone travel, evacuation / drain; and, since
    2026-09-21, after a clean disconnect's exit save converges — A1′),
    `LeaveScene` removes the record, or the node is confirmed dead;
  - awaiting-placement record (zone travel granted, second leg never landed) →
    no holder, so any zone may place the player. A `ZoneId=0` reconnect is
    **not** pulled toward the travel target (the former pull, R8, was replaced
    by GO-5 so that a failing second leg cannot bounce the player back and
    forth); the record only decides whether a landing in the travel target
    zone reuses the recorded map, until the 300s ticket TTL.
- Since GO-5 login sends `SceneId=0` for reconnect / replace, so a zone C
  `SceneId` no longer reaches scene_manager from login. Any other caller that
  sends a zone C `SceneId` with `ZoneId=A` is still rejected by `resolveScene`
  ("scene belongs to zone C", `ErrNoAvailableNode`) before the handoff check.

## Merge-Server (合服)
`zone_id` is a **runtime routing identifier**, not a permanent identity:
- **Home zone (authoritative for data routing)**: run `tools/merge_zone` (or `DataService/RemapHomeZoneForMerge`) to rewrite `player:zone:*` in mapping Redis; see `docs/design/guild_ranking_architecture_zh.md` §合服工具.
- **Guild / 本服榜**: same tool updates MySQL `guild.zone_id` and merges `guild_rank:zone:{id}`.
- **SceneManager hot state** (`scene:{id}:zone`, `player:{id}:location`, world channel sets): do **not** hand-delete it. Once every source-zone scene node has deregistered and `scene_nodes:zone:{src}:load` is empty, `EnterScene` ignores source-zone locations on its own; housekeeping goes through `tools/merge_zone -clear-source-hot-state` (`-MergeClearSourceHotState`), which refuses to run while that load set is non-empty. Preconditions, what the player gets and what must never be deleted: see "Stale source-zone `player:{id}:location` after a merge" below.

### Login-side zone resolution (added 2026-09-08)

Three notions of "zone" coexist and only one is authoritative:

| notion | where | who writes it | role after merge |
|---|---|---|---|
| physical zone | k8s namespace (gate/scene/login/scene_manager), picked by the client via server list + `/api/assign-gate` | deploy | may be decommissioned |
| **home_zone** | data_service `player:zone:{player_id}` | `RegisterPlayerZone` at create; `tools/merge_zone` / `RemapHomeZoneForMerge` on merge | **single source of truth** for data routing, guild, leaderboard |
| creation-time `zone_id` | account blob `AccountSimplePlayer.zone_id` (`proto/common/.../user_accounts.proto`) | `createplayerlogic.go`, once | hint only |

Rules (implemented in `go/login/internal/logic/pkg/homezone`, wired via `ServiceContext.HomeZone`):

1. **Role list zone = current home_zone resolved at login.** `Login` resolves every role's `player_id` with one `BatchGetPlayerHomeZone` (bounded, `HomeZone.RoleListLookupTimeout`, default 500ms) and overwrites `zone_id` in the *response copy* when the mapping has a non-zero zone. Ids absent from the mapping or a failed RPC keep the stored creation-time value; login never fails because of it. The refreshed zone is **not** written back into the account blob — the mapping stays the only truth, and a second copy would fight it on the next merge/rollback.
2. **EnterGame routes by home_zone via the existing cross-zone redirect — opt-in.** Only when `HomeZone.RedirectOnEnterEnabled=true` and the request is a first login with no in-scene session in player_locator (`homeZoneOverrideAllowed`; reconnect / replace login send `ZoneId = 0` since 2026-09-21 and let scene_manager follow the player's own location — GO-5, `cross-zone-scene-travel.md` §12.7; priority in `resolveEnterSceneRoute`: pinning redirect ticket > reconnect / replace > this rule > own zone), `EnterGame` resolves `GetPlayerHomeZone(player_id)` before `SceneManager.EnterScene` (bounded, `HomeZone.EnterLookupTimeout`, default 1.5s). If it is non-zero and differs from `Node.ZoneId`, the request carries `ZoneId = home_zone`, `GateZoneId = login's zone`, `SceneId = 0`, so `enterscenelogic.go`'s `crossZoneRedirect` fires and the gate pushes `RedirectToGateEvent`. Lookup failure / zero → `ZoneId = own zone`, logged (ERROR for a transport failure, INFO for "unmapped"), never blocking. A connection that holds a redirect ticket whose `target_zone_id` is this zone (second leg of a zone travel, `cross-zone-scene-travel.md` CZ-8) skips the lookup and is never bounced home. `SceneId` is still sent as 0; scene_manager now decides the redirect before scene resolution, so an old-zone `SceneId` can no longer block the first leg.
3. **Account blob `zone_id` is a creation-time hint only.** Nothing should route on it; merge tooling does not need to touch the account system.

Switches in `go/login/etc/login.yaml`: `HomeZone.RefreshRoleListDisabled` (reverse-named; an absent block means enabled, same rationale as `KillSwitch`) and `HomeZone.RedirectOnEnterEnabled` (forward-named; absent / default means **disabled** — enabling it requires a client that follows msg 124 `RedirectToGate`, see `go/login/internal/config/config.go` `HomeZoneConf`). The role-list refresh is therefore the primary post-merge correction; the EnterGame redirect is the opt-in second line.

Known limits on the scene_manager side (rewritten 2026-09-20; the earlier text predated the CZ-4 handoff gate and told ops to clear `player:{id}:location`):

- The redirect is decided **before** `resolveSceneForEnter`, so the first leg does not need a live world channel in the target zone. It needs a registered Gate there (`AssignGateForZone`, otherwise `ErrNoAvailableNode`). Only a scene-initiated zone travel that names a map (`SceneConfId != 0`) is pre-checked, read-only, against `world_channels:zone:{z}:{conf}`.
- A location that still blocks returns the retryable `ErrHandoffPending` (18) and modifies nothing; `ErrUnsafeCrossNodeHandoff` (14) is retired.

#### Stale source-zone `player:{id}:location` after a merge

What the player gets without any operator action:

- **Source zone fully down** (every source scene node deregistered from etcd, the scene_manager leader has emptied `scene_nodes:zone:{src}:load` — on the etcd DELETE event, or on `fullSync`, which also sweeps zones that no longer have any node — and the re-entry barrier, default 20s, has elapsed): `playerLocationOwnerGone` treats the record as absent. Through a target-zone Gate the player gets a first landing with a freshly minted `owner_epoch`; through any other Gate a connection-only redirect first, then the same first landing. Nothing needs to be cleared.
- **Source zone still has a node in its load set** and the recorded node is alive or not provably dead: 18 on every retry. For a login-initiated request nothing prompts the source Scene to write a marker, so this lasts until `LeaveScene` removes the record, the source node drains (evacuation writes markers), or the node is confirmed dead (`playerLocationOwnerDead`, `cross-zone-scene-travel.md` §11.5). This is the gate doing its job: finish zone-down instead of deleting the key.
- **Zone of the record cannot be determined** (legacy record with `zone_id = 0` whose `scene:{id}:zone` is already gone): fail-closed, 18 forever. `tools/merge_zone` keeps such records too ("undecided"). This is the only case that needs a manual delete, and the reason `scene:{id}:zone` must not be removed ahead of the locations that depend on it.

Is deleting `player:{id}:location` safe?

- It does **not** skip epoch minting. With no location the placement is a first landing (`mint = true`), and the Lua `INCR`s the existing `player:{id}:owner_epoch`. `LeaveScene` performs exactly this delete on every clean logout.
- It **does** skip the handoff-marker gate, which only runs when a location exists. It is therefore safe only when no live process can still hold or write the player: all source scene nodes deregistered from etcd, load set empty, re-entry barrier elapsed (the barrier covers the 15s emergency-drain save window). Under exactly these conditions scene_manager already ignores the record, so the delete is housekeeping, not a fix. (Whether those final saves also reached MySQL is the merge runbook's Kafka-drain gate, `docs/ops/merge-zone-runbook.md`; the location key does not control it.)
- Deleting it while the recorded node is still alive is unsafe: the new node loads the last persisted snapshot while the old holder has newer state in memory. The freshly minted epoch then dethrones the old holder (its saves are rejected by the C++ Lua CAS and the db applied-epoch guard, and the entity is destroyed without saving), so there is no dual write, but progress since the last save is lost and the old scene's `instance:{id}:player_count` is never returned.
- **Never delete `player:{id}:owner_epoch`** (and never `consumer:applied_epoch:*` on its own). Re-minting from 1 makes a stale holder's cached value comparable again, and the db guard only ratchets upward, so a new owner's lower-epoch `DBTask`s on the same topic are dropped as stale.
- Preferred procedure: finish zone-down, then `tools/merge_zone -clear-source-hot-state`. It refuses while `scene_nodes:zone:{src}:load` is non-empty, deletes only locations with `zone_id == src`, and does not touch `owner_epoch` / `handoff`. Its gate checks the load set only, and its node-key step also removes `node:zone:{src}:*:death_at`; run it at least one re-entry barrier after the last source node deregistered (the runbook's zone-down step is well before it).

Open questions (not decidable from code; need the project owner / merge-tool owner):

1. Whether to ship an audited admin path for the "zone cannot be determined" records instead of a manual `DEL`.
2. Whether merge preflight must also require that no `home_zone == src` player is online as a visitor in another, still-live zone (their location has `zone_id != src`, so neither scene_manager nor the tool touches it, and the holding node keeps the pre-merge `home_zone_id` it received in `RoutePlayerEvent`). Not verified here.
