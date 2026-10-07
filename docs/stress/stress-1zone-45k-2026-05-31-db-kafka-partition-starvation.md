# Stress Test Round 10/11 Post-Mortem — db Kafka Partition Starvation

**Date**: 2026-05-31
**Scale**: 45,000 robots, 1 zone, 2 gates × 4 scenes
**Outcome**: Round 10 = 78% scene_ready timeout. Round 11 (after fix) = ~10.7× chain throughput, 24% timeout (new bottleneck).

## Symptoms (Round 10)

- Robot log: `1491 timed out waiting for scene ready`, `427 entered game` (vs config 45000 — but robot ramp didn't complete; treat ratios not absolutes).
- Login log: `[preload-pool] submitted_total=9333 running=0 free=256` — login submitted 9333 db tasks but had 0 outstanding workers, meaning db replied to only ~427 chains.
- scene_manager: only 642 `entered scene` over 2.5 min.

## Root Cause

Topic `db_task_zone_1` ended up with 10 partitions. `kafka-get-offsets.sh` showed each partition holding ~2000 messages (total 20,244). Yet `kafka-consumer-groups.sh --describe db_rpc_consumer_group` showed only **partition 0** with consumer assignment (offset 764/764). Partitions 1–9 had **no consumer**.

Timeline:
1. **22:38:21** db service starts. Calls `consumer.Start(ctx)` against `db_task_zone_1`.
   - Topic doesn't yet exist; broker auto-creates with `num.partitions=1` (default).
   - Sarama joins consumer group, gets partition 0.
2. **22:38:40** login service starts, calls `kafkautil.EnsureTopics(...)` which calls `admin.CreatePartitions` to grow `db_task_zone_1` from 1 → 10 partitions.
3. Kafka metadata propagates the new partition layout. Sarama in db notes the metadata but **does not trigger a rebalance**:
   - `Metadata.RefreshFrequency` default = 10 min.
   - Even on refresh, sarama's `Range`/`RoundRobin` strategies trigger rebalance on **member join/leave**, not **partition count change** (Kafka KIP-429 / cooperative-sticky has partial support, but `Range` does not).
4. Partitions 1–9 sit forever with no consumer. Login keeps producing to all 10 partitions via key-hash routing → 90% of tasks stranded.

## Fix

3 files modified:

### `go/db/db.go`
Added `kafkautil.EnsureTopics(brokers, []TopicSpec{...})` call BEFORE `kafka.NewKeyOrderedConsumer(...).Start(ctx)`. Same spec as login (partitions=10, retention=5min, segment=16MB). Guarantees the topic has full partition count when sarama joins.

### `go/db/internal/config/config.go`
Added `RetentionMs int64 \`json:"RetentionMs,default=300000"\`` to KafkaConfig — matches login's setting so both EnsureTopics calls are idempotent.

### `go/db/internal/kafka/key_ordered_consumer.go`
In `consumerGroupHandler.Setup()`, after partitions are assigned, log `logx.Errorf("partition under-claim: assigned=%d configured=%d ...")` when `len(claimed) < h.consumer.partitionCount`. Acts as alarm bell for future regressions (e.g. broker partition count diverges from local config).

## Round 11 Results (validation)

After force-killing all stale processes, restarting Kafka container (recovered from `__consumer_offsets` delete corruption), full state reset and dev-start:

| Metric                               | Round 10 (baseline) | Round 11 (after fix) | Delta   |
| ------------------------------------ | -------------------:| --------------------:| -------:|
| db consumer partition claim count    |                   1 |                   10 | +10×    |
| db consumer max lag (any partition)  |              ~2400  |                  13  | -180×   |
| login `EnterGame complete`           |                 427 |                4596  | +10.7×  |
| scene_manager `entered scene`        |                 642 |                7407  | +11.5×  |
| Robot `entered game`                 |                  ~  |                4818  | —       |
| Robot `timed out waiting for scene`  |                  ~  |                1491  | (new btl) |
| Login p99 RPC latency                |              619ms  |                15ms  | -40×    |
| scene_manager p99 RPC latency        |              421ms  |               543ms  | similar |

db consumer fully healthy. Login bottleneck cleared. **New bottleneck**: scene_manager EnterScene at 53 QPS with p90=534ms — likely C++ scene node CreateScene RPC backlog or scene_manager `world_init` migration cascading during ramp.

## Operational Notes

- **Kafka topic deletion** of `__consumer_offsets` corrupts brokers; recover by `docker restart kafka`. Avoid deleting that topic in dev resets — delete only user topics.
- **dev_tools.ps1 dev-stop** uses pidfiles which can go stale; surviving processes from prior `dev-start` runs may bind the same ports and cause new processes to crash. Periodically verify with `Get-Process | Where-Object Name -match "^(data_service|scene_manager_service|player_locator|login|db|robot|gate|scene)$"`.

## Generalized Rule

Any Go service that consumes a Kafka topic AND that topic is produced to by a different service MUST:
1. Either be guaranteed to start strictly AFTER the producer's `EnsureTopics` call has succeeded, OR
2. Call `EnsureTopics` itself with the SAME `TopicSpec` (partition count, retention) before subscribing.

Otherwise sarama can latch onto a stale 1-partition (or N-partition) view and silently drop messages on the missing partitions. The `partition under-claim` Errorf added in `Setup()` will catch this in CI/QA.
