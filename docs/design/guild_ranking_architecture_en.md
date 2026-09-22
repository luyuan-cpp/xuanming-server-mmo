# Guild Ranking Architecture (2025-03-25)

## Problem
Need to support both **global (cross-server) rankings** and **per-zone rankings** simultaneously, working correctly under both zone-locked and zone-unlocked scenarios.

## Solution: Dual Redis ZSET Strategy

### Redis Keys
- **Global ranking**: `guild_rank` — all guilds, single ZSET
- **Per-zone ranking**: `guild_rank:zone:{zoneId}` — one ZSET per zone

### Writes (Pipeline dual-write)
- `UpdateGuildScore`: Pipeline simultaneously `ZADD guild_rank` + `ZADD guild_rank:zone:{zoneId}`
- `RemoveGuildFromRank`: Pipeline simultaneously `ZREM` both keys
- `CreateGuild`: Initialize with score=0, write to both ZSETs
- `DisbandGuild`: Remove from both ZSETs

### Reads (zone_id routing)
- `zone_id=0` → read `guild_rank` (global)
- `zone_id>0` → read `guild_rank:zone:{zoneId}` (per-zone)
- Pagination: `ZCARD` for total, `ZREVRANGE start stop` for current page, client uses `ceil(total/page_size)` to compute total pages

### Zone-lock / Zone-unlock Has No Impact
- Ranking data isolation relies on the `zone_id` field, not on the zone-lock mechanism
- Global ranking is read-only display; no cross-zone operations involved
- When zones are unlocked, a guild's `zone_id` is fixed at creation time, so per-zone rankings remain accurate

### Redis Architecture
- All guild instances connect to the same **global Redis** (DB:2), not per-zone Redis
- Under Redis Cluster, a single ZSET key lands on one slot/node — no cross-shard issues
- No additional aggregation/ranking service needed

## Data Model Changes
- `GuildData` adds `ZoneID uint32`
- MySQL `guild` table adds `zone_id INT UNSIGNED` + `KEY idx_zone`
- Proto: `CreateGuildRequest`, `GuildInfo`, `UpdateGuildScoreRequest`, `GetGuildRankRequest`, `GetGuildRankByGuildRequest` all add `zone_id`

## When Would You Need a Separate Ranking Service?
- Only when guild data is scattered across per-zone independent Redis instances (not shared) would an aggregation service be needed
- Current architecture uses global Redis — **not needed**

## Merge-Zone Tool

### Location
- Go module: `tools/merge_zone/` (see `go.mod`)
- PowerShell wrapper: `tools/scripts/merge_zone.ps1` (runs `go build` and executes the binary)
- **RPC equivalent**: `DataService/RemapHomeZoneForMerge` (same mapping semantics as the player-mapping step in the CLI)

### What It Does
1. **Name conflict detection**: SQL JOIN to find guilds with same name in source and target zones
2. **MySQL migration**: primary-key point updates over the manifest written before any write — `UPDATE guild SET zone_id = <target> WHERE guild_id = ? AND zone_id = <source>`, ascending `guild_id`, one autocommit statement per guild — then invalidate `guild:v2` caches and re-count the source zone (still > 0 → abort, step not marked done). The session runs at READ COMMITTED; a row hitting 1213 / 1205 is retried a bounded number of times (capped exponential backoff, cancellable); any failure aborts with the merge fences **left in place** and logs the resume steps (GET to check run_id → DEL both fences → re-run). Why not a zone-wide `UPDATE ... WHERE zone_id = <source>`: it walks `idx_guild_0` (secondary entry first, then the primary key), the reverse of the online `DisbandGuild` (primary key `FOR UPDATE` → DELETE removes the secondary entry), so the two deadlock (1213) — same lock-order class as [`docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md`](../ops/incident-friend-lock-order-deadlock-2026-09-21.md)
3. **Redis per-zone ZSET merge**: `ZRANGE` source `guild_rank:zone:*` → `ZADD` target → `DEL` source key
4. **(Multi-cluster required) Player string-key copy**: copies all `player:{playerId}:*` string keys from the **source zone data Redis** to the **target zone data Redis**, **before** rewriting `player:zone` — otherwise `ClientForPlayer` would read the wrong (empty) cluster. If source/target endpoints and DB are identical, the tool skips this step (single-Redis dev).
5. **Player home-zone mapping**: rewrites `player:zone:{playerId}` on **mapping Redis** (`-mapping-redis-*`)

### Usage
```powershell
go run -C tools/merge_zone . -source-zone=102 -target-zone=101 -dry-run
go run -C tools/merge_zone . -source-zone=102 -target-zone=101 -apply

pwsh -File tools/scripts/merge_zone.ps1 -SourceZone 102 -TargetZone 101 -DryRun
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone 102 -TargetZone 101 -Apply
```

Optional skip flags: `-skip-guild-mysql`, `-skip-guild-rank`, `-skip-player-mapping`. Non-dry runs require `-apply` to avoid accidents.

### Properties
- **Idempotent**: safe to re-run (MySQL UPDATE is no-op for already-migrated rows, Redis ZADD overwrites)
- **Conflict-safe**: name-conflict guilds are logged and skipped (resolve manually)
- **Global ZSET untouched**: only per-zone ZSETs are merged; `guild_rank` (global) already has all guilds
