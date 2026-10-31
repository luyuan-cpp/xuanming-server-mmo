# Zone Data Rollback Plan

> **Obsolete (2026-10-30)**: player saves now commit directly to TiDB `player_store` ([player-save-direct-commit.md](./player-save-direct-commit.md)); the rollback RPCs were removed. Kept for history only.

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
# (corrected 2026-10-16, the two lines below are kept as written: Option A does not apply to the current pipeline. The topic is
#  db_task_zone_<id>[_g<gen>], and its partition count is part of the data-ordering protocol: take ServerConfig.Kafka.PartitionCnt
#  from go/db/etc/db.yaml (currently 10, not 5). Recreating a topic under the same name restarts offsets at 0 while go/db's permanent
#  applied cursor (consumer:applied:<topic>:<key>:<msgType>) still holds the old offsets, so orderingForTask judges new writes stale (same partition) or incomparable (partition count changed). Delete-and-recreate
#  is a generation change and must follow the offline procedure in db-task-kafka-partition-contract.md ("离线扩容流程");
#  the one-click k8s_zone_rollback.ps1 Step 5 only uses Option B (-ToDatetime).)
# (corrected 2026-10-17: kafka_offset_reset.ps1 -DeleteAndRecreateTopic now refuses every db_task topic (db_task_zone_<id>[_g<gen>],
#  matched by lib Test-DbTaskTopicName); the only way to get an empty topic is to bump ServerConfig.Kafka.TopicGeneration, and there is no
#  same-name escape hatch. The first line ("prevent ... from being replayed") does not hold on K8s either; see "Correction (2026-10-17)" below.)
# Option A: Delete and recreate the topic
kafka-topics.sh --delete --topic db_task_topic --bootstrap-server <broker>
kafka-topics.sh --create --topic db_task_topic --partitions 5 --bootstrap-server <broker>

# Option B: Reset consumer group offset to the specified timestamp
# (corrected 2026-10-15: the group is the zone's own db_rpc_consumer_group_z<id> and the topic is db_task_zone_<id>,
#  with a _g<gen> suffix when TopicGeneration >= 2; the legacy group's offsets on this topic must be deleted afterwards)
kafka-consumer-groups.sh --reset-offsets --to-datetime 2026-03-26T10:00:00.000 \
  --group db_rpc_consumer_group_z<id> --topic db_task_zone_<id> --bootstrap-server <broker> --execute

Step 5: Bring the zone back up
──────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up \
  -ZoneName <zone> -ZoneId <id> -NodeImage <image> -WaitReady
```

> **Correction (2026-10-15; implemented, not compiled or tested, pending Codex verification)**: go/db's db_task consumer group is moving, zone by zone,
> from the shared `db_rpc_consumer_group` (the legacy group) to a per-zone `db_rpc_consumer_group_z<ZoneId>` (the zone group). The manual steps above are
> superseded by the one-shot script `tools/scripts/k8s_zone_rollback.ps1` (`dev_tools.ps1 -Command k8s-zone-rollback`, dry-run unless `-Apply`). Its Kafka
> handling now: the old Step 2 drain wait is removed (`-KafkaDrainTimeoutSec` is gone); step 0 c takes a read-only LAG snapshot of both groups on this topic
> (warnings only) and step 0 d rehearses Step 5 with a dry-run reset before shutdown when the zone group holds no offsets yet; Step 5 resets the zone group to
> the target time, verifies every partition, then deletes the legacy group's offsets on this topic (`kafka_offset_reset.ps1 -RetireGroup … -Partitions <N>`);
> Step 6 passes `-DbConsumerGroup zone`, so `k8s_deploy.ps1` renders the final shape `GroupID: "db_rpc_consumer_group_z<id>"` without `GroupHandoffFrom`.
> After a rollback the zone always runs on its zone group. Group base name and partition count are read from `go/db/etc/db.yaml`
> (`ServerConfig.Kafka.GroupID`, `ServerConfig.Kafka.PartitionCnt`). Full details, residual risks and unverified Kafka CLI behaviour are in the
> "2026-10-15 修订" section of [zone_data_rollback.md](zone_data_rollback.md) (Chinese, authoritative) and in
> [db-consumer-group-per-zone.md](db-consumer-group-per-zone.md).

> **Correction (2026-10-17; implemented, not compiled or tested, pending Codex verification)**: script fixes after the R7 review. Details are in the
> "2026-10-17 修订" section of [zone_data_rollback.md](zone_data_rollback.md) (Chinese, authoritative) and in §5 / §6 of
> [db-consumer-group-per-zone.md](db-consumer-group-per-zone.md).
> - **Step 0 e** (`Assert-Step6KafkaFactsReadable`, `-Apply` only, after step 0 d and before Step 1): reads both groups' offsets through the same path Step 6 uses,
>   i.e. `kubectl exec` into `kafka-0` of the infra namespace (lib `Get-DbGroupDescribeExecArgs` / `Resolve-DbGroupDescribeExecResult`, the same `-KubeContext` /
>   `-KubeConfig`, and the `-InfraNamespace` default read from `dev_tools.ps1`'s parameter block). An exec failure (missing `pods/exec`, kafka-0 not ready) or an
>   unparseable result refuses before shutdown, and so does a disagreement with the local CLI (`-KafkaBootstrap`) on whether each group holds committed offsets
>   on this topic. The earlier residual risk "Kafka offsets are first read in Step 6, after shutdown" is therefore covered before shutdown; only the window between
>   step 0 e and Step 6 (the MySQL PITR in between) remains.
> - **Topic name**: derived from `ServerConfig.Kafka.TopicGeneration` in `go/db/etc/db.yaml` via lib `Get-DbTaskTopicName` (`db_task_zone_<id>` for generation ≤ 1,
>   otherwise `db_task_zone_<id>_g<gen>`); an explicit `-KafkaTopic` that differs from the derived name is refused at entry.
> - **`kafka_offset_reset.ps1` constraints**: `-DeleteAndRecreateTopic` refuses every db_task topic (bump `TopicGeneration` instead); for other topics
>   `-ReplicationFactor` is mandatory and must equal the existing one, the topic is described before deletion (no delete if unreadable or mismatched), the wait for
>   deletion times out after 30 s and throws without recreating. Reset modes now end with `exit 0`, and `Invoke-KafkaToolCaptured` resets `LASTEXITCODE`, so a
>   non-zero "group does not exist" describe no longer turns a successful Step 5 into a failure.
> - **Rollback semantics (pending at the time; the owner chose (A) "discard" on 2026-10-17, implemented as described in "Correction (2026-10-18)" below; kept for history)** (comments only, behaviour unchanged): on K8s go/db's applied cursors
>   (`consumer:applied:<topic>:<key>:<msgType>`, `consumer:applied_epoch:<topic>:<key>:<msgType>`) live in the shared Redis DB 0, which the Step 4 FLUSHDB guard
>   refuses to flush, so Step 4 is skipped and the cursors stay. After Step 5 rewinds the zone group, writes already applied after the target time are absorbed as
>   stale, while the backlog not yet consumed at zone-down and this topic's retry receipts in DB 0 still land in the restored MySQL. "Replay from the target time"
>   therefore does not hold on K8s, and "discard everything after the target time" is incomplete. The two mutually exclusive fixes are (A) discard: Step 5 uses
>   `-ToLatest` and this topic's retry receipts are inventoried and intercepted between Step 4 and Step 5; (B) disaster-recovery replay: before Step 5 delete only this
>   topic's cursors (SCAN `consumer:applied:<topic>:*` and `consumer:applied_epoch:<topic>:*`, UNLINK page by page) and add a step 0 check that the target time is
>   not earlier than each partition's log start (no such check exists today). Costs are compared in zone_data_rollback.md.

> **Correction (2026-10-18; rollback semantics decided; implemented, not compiled or tested, pending Codex verification)**: on 2026-10-17 the owner chose
> "discard every write after TargetTime": a rollback returns the zone to what MySQL had committed at T. The full procedure, the handling of every leftover state
> family, the cost, the residual risks and the items still awaiting the owner are in the "2026-10-18 修订" section of [zone_data_rollback.md](zone_data_rollback.md)
> (Chinese, authoritative). In short:
> - **Accepted cost**: legitimate saves after T are discarded too, including saves produced before T that were still unconsumed in Kafka at T; cross-service state is
>   still not rolled back.
> - **Kafka**: Step 5 moves the zone group to the log end (`kafka_offset_reset.ps1 -ToLatest`, no more `--to-datetime`), verifies every partition, retires the legacy
>   group's offsets on this topic and records the cut in `kafka-cut.json`. go/db's applied cursors and owner_epoch are kept (after the move every new seq exceeds the old
>   cursor). Any later `-ToDatetime` / `-ToEarliest` on a db_task topic must not cross that cut.
> - **Redis**: no FLUSHDB any more (shared DB 0). All MySQL reads and Redis writes go through the discard tool `go/db/cmd/zone_rollback_discard`, which **has not been
>   written yet**, so `-Apply` stops at the entry checks until it lands. In Step 4b it renames this topic's retry receipts and dead letters into a per-pass quarantine
>   and deletes the affected players' parent snapshots, child caches, sync_digest, asset-commit evidence and barriers according to the restored MySQL; mappings, cursors
>   and the account directory are never touched.
> - **Order**: entry checks → step 0 a–f (entry state and `entry-state.json`, static preflight, LAG snapshot, Step 5 rehearsal, Step 6 Kafka path, deploy facts + Redis
>   run_id + tool build commit + tool preflight) → Step 1 → 1b (Kafka consumers, two log-end reads, then the tool's stopped mode writes the stopped-phase row-fingerprint
>   baseline) → Step 3 manual PITR → 4a inventory against that baseline, then a re-check that the zone is still stopped → Step 5 → 4b discard → 5b (cut + tool verify)
>   → Step 6 `-DbConsumerGroup zone` → Step 7. The abort path before Step 5 is a zone-up **without** `-DbConsumerGroup` (keep).
> - **Still awaiting the owner**: #1 zone maintenance fence (until it lands the tool's preflight always refuses), #3 strict barrier protocol, #4 blocked players,
>   #6 players merged into this zone after T. The script fails closed on all of them.

### Key Safety Points

| Point | Explanation |
|-------|-------------|
| Redis can be safely flushed | It is a cache layer; nodes rebuild from MySQL after startup |
| Kafka must be handled | Not purging the topic/offset causes old messages to replay and overwrite rolled-back data |
| MySQL must be restored first | It is the sole persistent source of truth |
| Serial guarantee | Kafka partition key = `player_id`; writes for the same player execute serially |

> **Correction (2026-10-17)**: the "Kafka must be handled" row is incomplete for the current pipeline. Which writes replay after the offsets are rewound depends on
> whether go/db's per-topic applied cursors survive (on K8s they do, in the shared Redis DB 0); see "Correction (2026-10-17)" above.
>
> **Correction (2026-10-18)**: with the decided "discard" semantics the first two rows no longer hold: Redis must not be flushed (the discard tool drops only this
> zone's post-T state), and Kafka is neither purged nor rewound to TargetTime; the zone group is moved to the log end and the cursors are kept. See "Correction (2026-10-18)" above.

---

## 3. Current Infrastructure Gaps

| Gap | Description | Priority |
|-----|-------------|----------|
| MySQL scheduled backup + binlog archiving | K8s MySQL pod has no automatic backup configured; needs a CronJob or cloud RDS | **High** |
| Redis RDB/AOF periodic snapshot archiving | Current `--appendonly yes` ensures persistence but has no point-in-time recovery capability | Low (can FLUSHDB) |
| Kafka offset rollback tool | Need a script to reset consumer group offset to a specified timestamp (implemented since 2026-05-15 as `tools/scripts/kafka_offset_reset.ps1`, `dev_tools.ps1 -Command kafka-offset-reset`, per [zone_data_rollback.md](zone_data_rollback.md)) | ~~Medium~~ |
| One-click zone rollback script | Encapsulate the above steps as `dev_tools.ps1 -Command k8s-zone-rollback` (implemented since 2026-05-15 as `tools/scripts/k8s_zone_rollback.ps1`, per the authoritative table in [zone_data_rollback.md](zone_data_rollback.md); its 2026-10-15 Kafka-step revision is in the Correction above and is not compiled or tested, pending Codex verification; since 2026-10-18 the semantics are decided, see "Correction (2026-10-18)" above, and `-Apply` cannot run until the discard tool lands) | ~~Medium~~ |

---

## 4. Production Environment Recommendations

- **Cloud MySQL (RDS)**: Built-in automatic backup and PITR — rollback is just a few clicks in the console, **strongly recommended**
- **Self-hosted MySQL**: Must configure `log-bin`, periodic `mysqldump` CronJob, and binlog archiving to object storage
- **Redis**: Simply FLUSHDB during zone rollback — no additional backup needed
  - Corrected 2026-10-18: no longer true. Redis is shared, a rollback never flushes it; see "Correction (2026-10-18)" above.
- **Kafka**: The safest approach after rollback is to purge `db_task_topic` to prevent old message replay
  - Corrected 2026-10-17: the current pipeline cannot purge or recreate a db_task topic under the same name (go/db's applied cursors are keyed by topic name and never
    expire, so new saves on a recreated topic would be judged stale and dropped; `kafka_offset_reset.ps1 -DeleteAndRecreateTopic` refuses it). Rollback only rewinds
    offsets; see "Correction (2026-10-17)" above for which writes replay.
  - Corrected 2026-10-18: a rollback moves the zone group to the log end (`-ToLatest`) and keeps the cursors; see "Correction (2026-10-18)" above.
