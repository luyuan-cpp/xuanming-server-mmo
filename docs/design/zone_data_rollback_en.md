# Zone Data Rollback Plan

## Overview

Zone data rollback operates at two levels:
1. **Application-level rollback** (implemented): Restore player data from snapshots via the `RollbackZone` RPC
2. **Disaster recovery level** (Ops procedure): MySQL PITR + Redis flush + Kafka offset reset

---

## 1. Single Player Rollback (Application-Level)

See [single_player_rollback.md](single_player_rollback.md) for details.

- Kick the player offline → find the closest snapshot before the target time → call each service's `ImportPlayerData()` to restore
- Automatically validates trade logs to prevent item duplication (items with legitimate transfer records are not restored)
- This is essentially a **compensatory** rollback, not a time reversal

---

## 2. Full Zone Application-Level Rollback (`RollbackZone` RPC)

### Design Decision: Guild/Friend Data Is Not Rolled Back

**Conclusion: Guild/Friend data does not need to be rolled back.**

**Reasons**:
- The Player Redis blob (`player:{id}:player_database`) **does not contain** guild_id or friend references
- Guild data is queried independently via `GuildService` gRPC; Friend data is queried independently via `FriendService` gRPC
- The two systems' data is fully decoupled — rolling back player data will not cause inconsistencies with guild/friend data
- Even if temporal discrepancies exist (e.g., a player is rolled back to before joining a guild, but the guild_member table still has the record),
  such inconsistencies self-heal through normal guild/friend operations (leave and rejoin, reapply, etc.)

### Players Without Snapshots (Fail Closed)

`RollbackZone` no longer deletes any player merely because no snapshot exists at
or before the target time. Snapshots are event/GM/rollback driven; the repository
has no periodic snapshot job. Snapshot absence therefore does not prove that a
character was created after the target time.

| Scenario | Handling |
|----------|----------|
| Characters created before T | Restored from player_snapshot ✅ |
| No snapshot at or before T | Report in `orphan_player_ids` as a review candidate; do not restore or delete |
| Candidate Redis data / zone mapping | Left unchanged |
| Candidate account / guild / friend data | Left unchanged |

The field name `orphan_player_ids` is retained for wire compatibility. Its
current meaning is “players without a qualifying snapshot that require manual
review”, not “confirmed post-T characters” and not “players already cleaned”.
`orphans_cleaned` is always zero. Automated deletion requires an authoritative
character creation timestamp plus approval, a pre-delete snapshot, and a
recoverable deletion workflow.

### Execution Flow (2 Phases)

```
Phase 1: Restore player data
────────────────────────────
Iterate over all players in the zone that have snapshots
→ For each player: find the closest player_snapshot before target_time
→ Create a pre-rollback safety snapshot
→ Overwrite Redis with snapshot data

Phase 2: Report no-snapshot candidates
──────────────────────────────────────
SCAN mapping Redis to find all players with zone mapping in this zone
→ Compare against the player list from snapshots to get the candidate set
→ Return candidates while leaving every data record and mapping unchanged
```

### Code Locations

| File | Purpose |
|------|---------|
| `go/data_service/internal/logic/rollback_logic.go` | RollbackZone (player rollback + no-snapshot candidate report) |
| `go/data_service/internal/routing/router.go` | GetAllPlayerIDsInZone (SCAN mapping Redis) |
| `go/data_service/internal/store/snapshot_store.go` | player_snapshot + rollback_audit_log CRUD |
| `proto/data_service/data_service.proto` | RollbackZone RPC definition |

### Cross-Service Call Chain

```
RollbackZone (data_service)
  ├── Phase 1: Restore each player's Redis data (from snapshot)
  └── Phase 2: Return no-snapshot candidates (no deletes or account changes)
```

> **Safety boundary**: callers may use the candidate list for manual review only;
> they must not interpret it as a completed cleanup result.

---

## 3. Full Zone Disaster Recovery Rollback (Ops Procedure)

### Architecture Prerequisites

Each zone is an independent namespace in K8s (`mmorpg-zone-{zoneName}`), with its own:
- MySQL instance (database name `game`)
- Redis instance (`--appendonly yes`)
- Kafka broker / topic (`db_task_topic`)

Write path: `C++ node → Kafka(db_task_topic) → Go db service → MySQL`. Redis serves only as a cache acceleration layer.

### Procedure Steps

```
Step 1: Shut down the zone (stop all business writes)
─────────────────────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <zone>

Step 2: MySQL Point-in-Time Recovery (PITR)
───────────────────────────────────────────
# First restore to a shadow database for verification
mysqlbinlog --stop-datetime="2026-03-26 10:00:00" binlog.000xxx | mysql -h shadow-host -u root -p game

# After verification, replace the production database
# Option A: Cloud RDS console one-click PITR
# Option B: Self-hosted MySQL manual recovery via binlog

Step 3: Redis handling
──────────────────────
# Redis is a cache layer; C++ nodes rebuild from MySQL on startup, so it can be safely flushed
redis-cli -h <redis-host> FLUSHDB

# Or restore from AOF/RDB backup to the corresponding time point (if archives exist)

Step 4: Kafka handling
──────────────────────
# Purge db_task_topic to prevent write messages after the rollback point from being replayed
# Option A: Delete and recreate the topic
kafka-topics.sh --delete --topic db_task_topic --bootstrap-server <broker>
kafka-topics.sh --create --topic db_task_topic --partitions 5 --bootstrap-server <broker>

# Option B: Reset consumer group offset to the specified timestamp
kafka-consumer-groups.sh --reset-offsets --to-datetime 2026-03-26T10:00:00.000 \
  --group db_rpc_consumer_group --topic db_task_topic --bootstrap-server <broker> --execute

Step 5: Bring the zone back up
──────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up \
  -ZoneName <zone> -ZoneId <id> -NodeImage <image> -WaitReady
```

### Key Safety Points

| Point | Explanation |
|-------|-------------|
| Redis can be safely flushed | It is a cache layer; nodes rebuild from MySQL after startup |
| Kafka must be handled | Not purging the topic/offset causes old messages to replay and overwrite rolled-back data |
| MySQL must be restored first | It is the sole persistent source of truth |
| Serial guarantee | Kafka partition key = `player_id`; writes for the same player execute serially |

---

## 3. Current Infrastructure Gaps

| Gap | Description | Priority |
|-----|-------------|----------|
| MySQL scheduled backup + binlog archiving | K8s MySQL pod has no automatic backup configured; needs a CronJob or cloud RDS | **High** |
| Redis RDB/AOF periodic snapshot archiving | Current `--appendonly yes` ensures persistence but has no point-in-time recovery capability | Low (can FLUSHDB) |
| Kafka offset rollback tool | Need a script to reset consumer group offset to a specified timestamp | Medium |
| One-click zone rollback script | Encapsulate the above steps as `dev_tools.ps1 -Command k8s-zone-rollback` | Medium |

---

## 4. Production Environment Recommendations

- **Cloud MySQL (RDS)**: Built-in automatic backup and PITR — rollback is just a few clicks in the console, **strongly recommended**
- **Self-hosted MySQL**: Must configure `log-bin`, periodic `mysqldump` CronJob, and binlog archiving to object storage
- **Redis**: Simply FLUSHDB during zone rollback — no additional backup needed
- **Kafka**: The safest approach after rollback is to purge `db_task_topic` to prevent old message replay
