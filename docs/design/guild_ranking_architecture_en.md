# Guild Ranking Architecture

> Current implementation. Rewritten on 2026-11-10 against the code after the global ranking was removed; on 2026-11-11 score writes became versioned conditional writes and stopped taking the global ranking maintenance lock (item 23 of the [gap list](../handoff/server-gap-audit-20261101.md), C3 in §9 of the [no-single-node plan](no-single-node-horizontal-scaling-20261001.md)). Neither code change is compiled yet: verification of the global-ranking removal is section 8 of the [Codex checklist](../handoff/gate-online-identity-simplify-20261105/CODEX.md); verification of the versioned writes is the 2026-11-11 [PROGRESS](../PROGRESS.md) entry on guild score writes no longer taking the global lock. The 2025-03-25 draft had two rankings, global plus per-zone; the owner decided on 2026-11-10 to remove the global one, see §6.
> Same content as the Chinese [guild_ranking_architecture_zh.md](guild_ranking_architecture_zh.md) and [guild_ranking_architecture.md](guild_ranking_architecture.md); keep all three in sync.

## 1. Summary

- Guild rankings are per zone only: one ZSET per final zone, and players only see the ranking of their home zone. There is no global ranking and no aggregation service.
- The authoritative score is MySQL `guild.score`; every score update increments `guild.score_version` in the same transaction. The ZSETs are a derived read projection; `RebuildRanks` realigns them with MySQL when they are lost or wrong.
- Every ranking entry records its version, and every ranking write is conditional: it is applied only if its version is not older than the one recorded. Score writes therefore need no global lock: when several replicas update scores concurrently and their writes reach Redis out of order, the ranking always ends up with the last committed score.
- Which ranking a guild is on depends only on the guild row's `zone_id` in MySQL, resolved to its final zone through the merge mapping. `zone_id` is the zone the guild was created in and is not rewritten by a merge. A zone passed by a caller never decides the ranking.

## 2. Data and Redis keys

| Name | Type | Content |
|---|---|---|
| MySQL `guild.score` | column | Authoritative score |
| MySQL `guild.score_version` | column | Score version. 0 at creation; `UpdateGuildScore` increments it in the same transaction that writes `score`. Score updates of one guild are serialized on the guild row lock, so versions grow strictly in commit order and each version stands for exactly one committed score |
| MySQL `guild.zone_id` | column | Zone the guild was created in. Guilds are only created in the requester's home zone, so it is never 0; a merge does not rewrite it |
| `guild_rank:zone:<final zone>` | ZSET | member = guild_id, score = ranking score |
| `guild_rank:version:<final zone>` | HASH | field = guild_id, value = `score_version` of the score on the ranking. Changed only together with the zone's ZSET, by scripts, so both always hold the same members |
| `guild_rank:tombstone:<zone>` | ZSET | The zone's disband tombstones: member = guild_id, score = expiry time (Redis server time in milliseconds, 24 hours after the tombstone was written). Blocks writes committed before a disband that arrive after it. Every new tombstone extends the whole key's TTL to its own expiry |
| `guild_rank:maintenance_lock` | string | Ranking maintenance lock; only makes rebuilds and merge folds mutually exclusive. SETNX, TTL 5 minutes, waits at most 5 seconds, released only by the matching token. Score updates, ranking a new guild and removal on disband do not take it |

The keys carry no hash tag. guild uses its own `RedisClient` (`go/guild/etc/guild.yaml`, DB 0), a single-instance client. Every key starting with `guild_rank` belongs to the guild ranking: `RebuildRanks` scans `guild_rank:zone:*` and removes members that do not match MySQL, so other services must not use this prefix ([leaderboard design](leaderboard-system.md) §3.1). The version HASH deliberately does not start with `guild_rank:zone:`, so code that scans rankings by that prefix (rebuild, removal, `tools/data_consistency_check`) never mistakes it for a ranking.

## 3. Writes

Every ranking write goes through one conditional write script (`rankUpsertScript`). It touches only one zone's three keys (ZSET, version HASH, tombstones):

1. The zone's tombstones hold this guild and the tombstone has not expired: the guild was disbanded; nothing is written.
2. The version recorded in the HASH is newer than this one: nothing is written.
3. Otherwise the ZSET and the version are written. An equal version is written too: a version stands for one score, so rewriting it is idempotent and also repairs a score that was changed by hand.

Writers:

- **Score update**: `UpdateGuildScore` in the data layer locks the guild row in one READ COMMITTED transaction with `SELECT zone_id, score_version ... FOR UPDATE` and runs `UPDATE guild SET score = ?, score_version = ?` (old version plus 1). Deadlocks and write conflicts are retried a bounded number of times, and an unknown commit outcome is reported as `ErrWriteConflict`. After the commit it invalidates the guild cache, then writes the final zone's ranking conditionally on that version. It does not take the maintenance lock.
  - MySQL first, Redis second, on purpose: a lost ZSET can be rebuilt; writing the ZSET first would lose the score for good on a Redis failure.
  - The ranking is chosen only by the `zone_id` in MySQL; a caller's zone is used only for a mismatch log.
  - A row with `zone_id=0` is corrupt data: the score is still persisted, but the guild goes on no ranking and an ERROR is logged.
  - A conditional write that is not applied (the ranking already holds a newer version, or the guild was disbanded) is a normal outcome under concurrency; it is logged at INFO and is not a failure.
- **Score RPC**: the RPC of the same name, `UpdateGuildScore`, is marked `CLIENT_RPC_SERVER_ONLY`; a client sending it is dropped by the gate as an illegal packet. Nothing in the repository calls it in normal operation; the robot only uses it to check that clients cannot send it.
- **Guild creation**: after a guild is created, the logic layer calls `AddNewGuildToRank`, which puts the guild on its ranking with the initial score 0 and version 0 written by the creation transaction. It is one Redis write, without a second transaction. Version 0 loses to every later score update, so arriving late cannot overwrite a newer score. A failure is only logged: the authoritative score is in MySQL, and the missing entry is filled by the next score update or by the startup rebuild.
- **Disband and removal**: `RemoveGuildFromRank` first picks the zone. While the guild row still exists, the MySQL `zone_id` wins; once the row is deleted (the normal disband path), it uses the zone read with `FOR UPDATE` inside the disband transaction. It then removes the entry, together with its version, from every ranking found by `SCAN guild_rank:zone:*` and from every zone of the guild's merge group (all original zones that resolve to the same final zone, the final zone included). On a disband, the zones of the merge group get a tombstone in the same script that removes the entry (`rankBuryScript`). It does not take the maintenance lock.
  - Why not just one ranking: before a merge is folded, the entry may still sit on the source zone's old key; after a merge is undone, it may still sit on the target zone's key. Removing it from only one ranking leaves a ghost entry with no name.
  - Why it needs no mutual exclusion with score writes: in each zone, writing the tombstone and removing the entry are one script. A write that ran before it left an entry that the script removes; every write after it is rejected by the tombstone.
  - Why the merge group is enough: ranking writes only go to `Final(zone_id)`, and a replica with a stale mapping cache still lands inside the merge group: one that does not know about the merge writes the original zone, one that does writes the final zone. One window remains: right after a merge, if the replica removing the guild has not refreshed its mapping yet while the replica updating the score has, a late score update goes to a new final zone the remover did not know about. It leaves one ghost entry until the next rebuild. Only what the ranking shows is affected.
  - How long a tombstone lives: a late write has only three sources: a score update committed before the disband that has not reached the ranking yet (same request, seconds), a rebuild between reading its snapshot and merging it (minutes), and a fold between reading a source entry and merging it into the target (seconds). 24 hours is far longer than any of them. Expiry uses the Redis server clock, so clock differences between replicas do not matter.
  - How tombstones are cleaned up: there is no cleanup job. Writing a tombstone first deletes the zone's expired ones, then extends the whole key's TTL to the new tombstone's expiry; a zone with no disband for a day loses the key to Redis expiry.
  - While the guild row still exists (degraded mode, manual cleanup) the entry is removed without a tombstone, so the next score update puts the guild back.
  - A failed removal after a disband is only logged; the startup rebuild repairs it.

## 4. Reads

- **Entry**: the client methods `GetGuildRank` (a page) and `GetGuildRankByGuild` (one guild's position) only accept requests with a session; a call without a session gets `PermissionDenied`.
- **Zone**: always the home zone from data_service. An unknown home zone answers `kGuildHomeZoneUnknown`; a lookup failure is returned as an error. The server does not read a zone from the request body; removal of those fields is item 2 of §9.6 in [Gate online identity](gate-online-identity.md).
- **Paging**: pages are 1-based, default 1; the page size defaults to 20 and is clamped to `MaxRankPageSize` (50). Each entry is filled with the guild name, level and member count from the guild cache; leader names for a whole page are fetched in one batch.
- **Position**: `GetGuildRankByGuild` returns the position on the requester's home-zone ranking. A guild from another zone is not on that ranking and answers `kGuildNotRanked`.
- **Data layer**: `GetGuildRankPage` / `GetGuildRank` require a non-zero zoneID; 0 returns `ErrRankZoneRequired`. A 0 can only be a caller bug, so it is not answered with an empty ranking that would pass for "nobody is ranked". A merged-away zone reads its final zone's ranking.
  - Consistency: total plus page (`ZCARD` + `ZREVRANGE`) and position plus score (`ZREVRANK` + `ZSCORE`) are each read in one Lua script, from one snapshot. Reads only touch the ZSET, never the version HASH.
  - Out of range: the page start is computed in 64 bits; an out-of-range start returns an empty page and never wraps around to the top.

## 5. Rebuild and self-repair

`RebuildRanks` runs at guild startup, on stable replicas only; canary replicas skip it ([release track design](release-track-gray-canary.md) §2.5). If the rebuild fails, the process refuses to start rather than serve an incomplete ranking.

The rebuild no longer builds temporary keys and RENAMEs them over the live keys: that put the old snapshot values back over scores written during the rebuild, which is why score updates used to take the same lock as the rebuild. It now writes entry by entry, conditionally. Steps:

1. Take the maintenance lock, only to exclude folds and rebuilds on other replicas.
2. Read everything with `SELECT guild_id, zone_id, score, score_version FROM guild`.
3. Merge each row by version into its final zone under the current merge mapping. If the ranking already holds a newer version (a score update after the snapshot), it is left alone; if the guild was disbanded (tombstone present), nothing is written.
4. Scan every `guild_rank:zone:*` key for entries that are not in the snapshot, or that the snapshot places in another zone. Each of them is first checked against the current MySQL rows; nothing is decided from the snapshot alone:
   - No longer in MySQL: a ghost left by a disband whose removal failed. Write a tombstone in this zone and remove the entry;
   - In MySQL and in this zone: a guild created after the snapshot. Merge the current row; do not remove it;
   - In MySQL but in another zone (the mapping changed), or with `zone_id=0`: merge the current row into the zone it belongs to, then remove it here only if its version is still the one that was read, so a version written in the meantime is never removed by mistake.

This repairs missing entries, stale scores, a lost ranking, ghost entries left by disbands and entries in the wrong zone. After a merge or an undone merge, one restart brings the rankings back in line with the mapping. Rows with `zone_id=0` go on no ranking; they are counted and an ERROR is logged. Members that are not guild ids (only possible if written by hand) are removed.

Correctness does not depend on the maintenance lock: every write is conditional, and every move removes an entry only after comparing its version. The lock has a 5-minute TTL and is not renewed; if a slow rebuild outlives it and a fold runs at the same time, the rankings still cannot be corrupted.

## 6. Why there is no global ranking

The 2025-03-25 draft asked for two rankings: every score update wrote both `guild_rank` and the per-zone ranking, and `zone_id=0` read the global one. On 2026-11-10 the owner decided that neither operations nor GMs need a global ranking, and it was removed. Reasons:

- **No reader**: clients only see their zone's ranking. The only reader of the global ranking was the no-session branch of the guild read methods; the entry gate already rejected such calls in production, and the branch was deleted on 2026-11-06 (item 1 of §9.6 in [Gate online identity](gate-online-identity.md)). The merge tool, the data consistency check, the merge runbook, the robot and the gateway admin all use per-zone rankings only.
- **Only cost**: one extra ZADD per score update, and every guild on a single key, which becomes a single-slot hotspot on Redis Cluster.
- **Both versions agree**: the Java version never had a global ranking.

A `guild_rank` key left over in an old environment is no longer read or written; `RebuildRanks` only scans `guild_rank:zone:*` and does not delete it. The project has not launched, so there is no migration code. Two ways to clean it:

- Wiping before a stress run removes it: `pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`; on K8s use `-Command k8s-redis-wipe`.
- To delete only that key: `redis-cli -h <guild RedisClient address> -n 0 DEL guild_rank`.

## 7. The merge tool and guild rankings

The merge tool `tools/merge_zone` only records one merge mapping, "source zone → target zone", in the global `zone_merged_into` table. It does not change guild rows and does not touch ranking keys (see the [merge runbook](../ops/merge-zone-runbook.md)). Guild rankings follow the mapping:

- **New scores**: written by `Final(guild.zone_id)` into the final zone's ranking; reading the source zone also reads the final zone's ranking.
- **Old entries**: entries written to the source zone's old key before the merge are folded into the final zone by `FoldMergedZoneRanks`.
  - Scheduling: runs on every replica, on the guild trial's owed-reward loop, every 10 seconds by default. In steady state it costs one `EXISTS` per source zone.
  - Folding: when an old key exists it takes the maintenance lock and repeatedly reads one page from the head of the source ranking atomically (scores paired with versions), writing each entry into the final zone conditionally on its version. If the target already holds a newer version, it is left alone; if the source holds a newer version (a replica with a stale mapping cache wrote to the source after the merge), it replaces the target's older score; if the guild was disbanded, nothing is written. After a page is merged, its entries are removed from the source only if their version is still the one that was read: a newer version written to the source in the meantime is never removed by mistake and is merged on the next page or round. When the source ranking is empty, Redis deletes the key.
  - No `ZUNIONSTORE`: the two keys are in different slots on Redis Cluster. No `ZADD NX`: it would let an older score in the target block a newer one from the source.
  - Failure: a fold that fails halfway starts over in the next round; the conditional writes are idempotent.
- **Undoing a merge**: the tool asks for one guild restart, and the startup rebuild puts the rankings back under the current mapping.

## 8. Scaling constraints

- **Score updates run concurrently**: score updates, ranking a new guild and removal on disband do not take the maintenance lock, so updates of different guilds and from different replicas never wait for each other. Updates of one guild are serialized on its MySQL guild row lock: that is where version order comes from, and it bounds the update rate of a single guild. The maintenance lock only makes rebuilds and folds mutually exclusive, and neither is on the request path.
- **Not Cluster-ready yet**: guild uses a single-instance Redis client and its keys carry no hash tag. Every ranking script touches only one zone's three keys, so moving to Redis Cluster only needs one tag on the three keys of a zone (the `gr` family in §3.2 of the [all-Redis-on-Cluster design](redis-cluster-all-domains-20261110.md)); the protocol stays as it is. The same move must also change the cache generation scripts in `guild_repo.go` (`guild:v2:cache_generation:<id>` and `guild:v2:<id>` are in different slots) and the places that find rankings with `SCAN guild_rank:zone:*` (rebuild, removal, `tools/data_consistency_check`).
- **Key size**: with one key per zone, a key holds one zone's guilds and does not grow with the number of guilds across all zones. A tombstone key only holds guilds of the zone's merge group disbanded in the last 24 hours.

## 9. Code

- **Data layer**: `go/guild/internal/data/guild_repo.go` — the scripts in the ranking write protocol section, `UpdateGuildScore`, `AddNewGuildToRank`, `RebuildRanks`, `RemoveGuildFromRank`, `FoldMergedZoneRanks`, `GetGuildRankPage`, `GetGuildRank`, `zoneRankKey`.
- **Schema**: `GuildRecord` in `proto/guild/guild_db.proto`; `score_version` is field 12. Tables and columns are created by `go/schemamigrate`.
- **Logic layer**: `go/guild/internal/logic/guild_logic.go` — `GetGuildRank`, `GetGuildRankByGuild`, and ranking upkeep in guild creation and disband; the fold scheduling is in `trial_background.go`.
- **Startup rebuild**: `go/guild/guild.go`.
- **Protocol**: `proto/guild/guild.proto` — `UpdateGuildScore`, `GetGuildRank`, `GetGuildRankByGuild`.
- **Tests**: `go/guild/internal/data/rank_version_test.go` (no database: late older writes, score writes not waiting for the lock, rebuild against concurrent writes, guilds created after the snapshot, late writes after a disband), `merged_zone_rank_test.go` (folds merge by version), and `rank_zone_integration_test.go` (`integration` tag, real database).
