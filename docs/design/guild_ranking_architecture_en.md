# Guild Ranking Architecture

> Current implementation. Rewritten on 2026-11-10 against the code after the global ranking was removed; the code change is not compiled yet, verification is section 8 of the [Codex checklist](../handoff/gate-online-identity-simplify-20261105/CODEX.md). The 2025-03-25 draft had two rankings, global plus per-zone; the owner decided on 2026-11-10 to remove the global one, see §6.
> Same content as the Chinese [guild_ranking_architecture_zh.md](guild_ranking_architecture_zh.md) and [guild_ranking_architecture.md](guild_ranking_architecture.md); keep all three in sync.

## 1. Summary

- Guild rankings are per zone only: one ZSET per final zone, and players only see the ranking of their home zone. There is no global ranking and no aggregation service.
- The authoritative score is MySQL `guild.score`. The ZSETs are a derived read projection; `RebuildRanks` rebuilds them from MySQL when they are lost or wrong.
- Which ranking a guild is on depends only on the guild row's `zone_id` in MySQL, resolved to its final zone through the merge mapping. `zone_id` is the zone the guild was created in and is not rewritten by a merge. A zone passed by a caller never decides the ranking.

## 2. Data and Redis keys

| Name | Type | Content |
|---|---|---|
| MySQL `guild.score` | column | Authoritative score |
| MySQL `guild.zone_id` | column | Zone the guild was created in. Guilds are only created in the requester's home zone, so it is never 0; a merge does not rewrite it |
| `guild_rank:zone:<final zone>` | ZSET | member = guild_id, score = ranking score |
| `guild_rank:maintenance_lock` | string | Ranking maintenance lock; score updates, removals, rebuilds and folds are mutually exclusive. SETNX, TTL 5 minutes, waits at most 5 seconds, released only by the matching token |
| `guild_rank:rebuild:<token>:zone:<zone>` | ZSET | Temporary key during a rebuild. Renamed onto the live key at the swap, deleted on exit if the swap did not happen |

The keys carry no hash tag. guild uses its own `RedisClient` (`go/guild/etc/guild.yaml`, DB 0). Every key starting with `guild_rank` belongs to the guild ranking: `RebuildRanks` deletes all existing `guild_rank:zone:*` keys, so other services must not use this prefix ([leaderboard design](leaderboard-system.md) §3.1).

## 3. Writes

- **Score update**: `UpdateGuildScore` in the data layer takes the maintenance lock, then in one READ COMMITTED transaction locks the guild row with `SELECT zone_id ... FOR UPDATE` and runs `UPDATE guild SET score`. Deadlocks and write conflicts are retried a bounded number of times, and an unknown commit outcome is reported as `ErrWriteConflict`. After the commit it invalidates the guild cache and runs `ZADD` on the final zone's ranking.
  - MySQL first, Redis second, on purpose: a lost ZSET can be rebuilt; writing the ZSET first would lose the score for good on a Redis failure.
  - The ranking is chosen only by the `zone_id` in MySQL; a caller's zone is used only for a mismatch log.
  - A row with `zone_id=0` is corrupt data: the score is still persisted, but the guild goes on no ranking and an ERROR is logged.
- **Score RPC**: the RPC of the same name, `UpdateGuildScore`, is marked `CLIENT_RPC_SERVER_ONLY`; a client sending it is dropped by the gate as an illegal packet. Nothing in the repository calls it in normal operation; the robot only uses it to check that clients cannot send it.
- **Guild creation**: after a guild is created, the logic layer writes a score of 0 into its ranking. A failure is only logged: the authoritative score is in MySQL, and the missing entry is filled by the next score update or by the startup rebuild.
- **Disband and removal**: `RemoveGuildFromRank` takes the maintenance lock, then picks the zone. While the guild row still exists, the MySQL `zone_id` wins; once the row is deleted (the normal disband path), it uses the zone read with `FOR UPDATE` inside the disband transaction. It then runs `ZREM` on every ranking found by `SCAN guild_rank:zone:*` plus the final zone's ranking.
  - Why not just one ranking: before a merge is folded, the entry may still sit on the source zone's old key; after a merge is undone, it may still sit on the target zone's key. Removing it from only one ranking leaves a ghost entry with no name.
  - A failed removal after a disband is only logged; the startup rebuild repairs it.

## 4. Reads

- **Entry**: the client methods `GetGuildRank` (a page) and `GetGuildRankByGuild` (one guild's position) only accept requests with a session; a call without a session gets `PermissionDenied`.
- **Zone**: always the home zone from data_service. An unknown home zone answers `kGuildHomeZoneUnknown`; a lookup failure is returned as an error. The server does not read a zone from the request body; removal of those fields is item 2 of §9.6 in [Gate online identity](gate-online-identity.md).
- **Paging**: pages are 1-based, default 1; the page size defaults to 20 and is clamped to `MaxRankPageSize` (50). Each entry is filled with the guild name, level and member count from the guild cache; leader names for a whole page are fetched in one batch.
- **Position**: `GetGuildRankByGuild` returns the position on the requester's home-zone ranking. A guild from another zone is not on that ranking and answers `kGuildNotRanked`.
- **Data layer**: `GetGuildRankPage` / `GetGuildRank` require a non-zero zoneID; 0 returns `ErrRankZoneRequired`. A 0 can only be a caller bug, so it is not answered with an empty ranking that would pass for "nobody is ranked". A merged-away zone reads its final zone's ranking.
  - Consistency: total plus page (`ZCARD` + `ZREVRANGE`) and position plus score (`ZREVRANK` + `ZSCORE`) are each read in one Lua script, from one snapshot.
  - Out of range: the page start is computed in 64 bits; an out-of-range start returns an empty page and never wraps around to the top.

## 5. Rebuild and self-repair

`RebuildRanks` runs at guild startup, on stable replicas only; canary replicas skip it ([release track design](release-track-gray-canary.md) §2.5). If the rebuild fails, the process refuses to start rather than serve an incomplete ranking. Steps:

1. Take the maintenance lock.
2. Read everything with `SELECT guild_id, zone_id, score FROM guild`.
3. Write into temporary keys by final zone under the current merge mapping.
4. In one MULTI/EXEC, delete every existing `guild_rank:zone:*` key and rename the temporary keys onto the live keys.

This repairs missing entries, stale scores, a lost ranking and ghost entries left by disbands. After a merge or an undone merge, one restart brings the rankings back in line with the mapping. Rows with `zone_id=0` go on no ranking; they are counted and an ERROR is logged.

## 6. Why there is no global ranking

The 2025-03-25 draft asked for two rankings: every score update wrote both `guild_rank` and the per-zone ranking, and `zone_id=0` read the global one. On 2026-11-10 the owner decided that neither operations nor GMs need a global ranking, and it was removed. Reasons:

- **No reader**: clients only see their zone's ranking. The only reader of the global ranking was the no-session branch of the guild read methods; the entry gate already rejected such calls in production, and the branch was deleted on 2026-11-06 (item 1 of §9.6 in [Gate online identity](gate-online-identity.md)). The merge tool, the data consistency check, the merge runbook, the robot and the gateway admin all use per-zone rankings only.
- **Only cost**: one extra ZADD per score update, and every guild on a single key, which becomes a single-slot hotspot on Redis Cluster.
- **Both versions agree**: the Java version never had a global ranking.

A `guild_rank` key left over in an old environment is no longer read or written, and `RebuildRanks` does not delete it. The project has not launched, so there is no migration code. Two ways to clean it:

- Wiping before a stress run removes it: `pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`; on K8s use `-Command k8s-redis-wipe`.
- To delete only that key: `redis-cli -h <guild RedisClient address> -n 0 DEL guild_rank`.

## 7. The merge tool and guild rankings

The merge tool `tools/merge_zone` only records one merge mapping, "source zone → target zone", in the global `zone_merged_into` table. It does not change guild rows and does not touch ranking keys (see the [merge runbook](../ops/merge-zone-runbook.md)). Guild rankings follow the mapping:

- **New scores**: written by `Final(guild.zone_id)` into the final zone's ranking; reading the source zone also reads the final zone's ranking.
- **Old entries**: entries written to the source zone's old key before the merge are folded into the final zone by `FoldMergedZoneRanks`.
  - Scheduling: runs on every replica, on the guild trial's owed-reward loop, every 10 seconds by default. In steady state it costs one `EXISTS` per source zone.
  - Folding: when an old key exists it takes the maintenance lock, walks the old key with `ZSCAN` and runs `ZADD NX` member by member into the final zone, and deletes the old key only after every member is written. `NX` keeps scores written after the merge; `ZUNIONSTORE` is not used because the two keys are in different slots on Redis Cluster.
  - Failure: a fold that fails halfway starts over in the next round; `NX` makes that idempotent.
- **Undoing a merge**: the tool asks for one guild restart, and the startup rebuild puts the rankings back under the current mapping.

## 8. Scaling constraints

- **Serialized score updates**: score updates, removals, rebuilds and folds all take the same maintenance lock, so score updates are serialized. Today the only update is the score of 0 written at guild creation, so it is not a bottleneck; this must change before a high-rate score producer is connected, see item 23 of the [gap list](../handoff/server-gap-audit-20261101.md).
- **Not Cluster-ready yet**: guild uses a single-instance Redis client. The `RebuildRanks` swap touches several keys in one MULTI/EXEC (deleting old zone rankings, renaming temporary keys), and a temporary key and its live key are in different slots, so it must change before guild moves to Redis Cluster.
- **Key size**: with one key per zone, a key holds one zone's guilds and does not grow with the number of guilds across all zones.

## 9. Code

- **Data layer**: `go/guild/internal/data/guild_repo.go` — `UpdateGuildScore`, `RebuildRanks`, `RemoveGuildFromRank`, `FoldMergedZoneRanks`, `GetGuildRankPage`, `GetGuildRank`, `zoneRankKey`.
- **Logic layer**: `go/guild/internal/logic/guild_logic.go` — `GetGuildRank`, `GetGuildRankByGuild`, and ranking upkeep in guild creation and disband; the fold scheduling is in `trial_background.go`.
- **Startup rebuild**: `go/guild/guild.go`.
- **Protocol**: `proto/guild/guild.proto` — `UpdateGuildScore`, `GetGuildRank`, `GetGuildRankByGuild`.
