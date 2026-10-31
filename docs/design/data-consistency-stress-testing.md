# Player-Data Consistency: Stress Testing & Verification

> **已过期(2026-10-30)**:玩家存盘改成 scene 直接提交 TiDB 的 `player_store`([player-save-direct-commit.md](./player-save-direct-commit.md)),go/db 的 verifier、data_stress 和机器人 data-stress 模式都已删除。本文只作历史保留,不再描述现行行为。

> Status: shipped. L1 unit tests, L2 standalone driver + verifier, L3 robot
> mode, L4 chaos harness (`tools/scripts/chaos_test.ps1`). The three consumer
> bugs originally tracked as "known-red" (TC3 / TC5a / TC5b) are fixed and
> their tests now run as regressions in CI (`go/test.bat`). The seq carrier
> is the dedicated `PlayerStressTestProbe` proto field, with a 16-byte
> FNV-1a-64 `test_sig` mirrored between go and cpp.

## 1. The invariant being tested

For any single player the data path is:

```
Scene (cpp) ──► Kafka (key = player_id) ──► DB consumer (go) ──► MySQL + Redis
```

The contract is **per-key monotonic last-write-wins**:

> If Scene emits writes `W1, W2, …, Wn` for the same `(player_id, msg_type)`,
> the final state in **MySQL must equal `Wn`**, the **Redis cache must equal
> `Wn`**, and **all messages must be processed** (no Kafka backlog left over,
> no silent drops).

Concretely, five formal invariants (see also `docs/design/zone_data_rollback_*.md`):

| ID  | Name                                | Reads from                | Pass criterion                                                                  |
| --- | ----------------------------------- | ------------------------- | ------------------------------------------------------------------------------- |
| I1  | Persistence convergence             | MySQL                     | row's seq field == expected_seq                                                 |
| I2  | Cache convergence                   | Redis `<msg_type>:<id>`   | decoded seq field == expected_seq (or absent → soft-pass)                       |
| I3  | Kafka no backlog                    | `kafka-consumer-groups`   | consumer-group lag == 0 within deadline                                         |
| I4  | Read-after-write within partition   | login read path           | login read sees post-write state for the same player_id                         |
| I5  | Crash recovery consistency          | post-crash MySQL/Redis    | restart converges to the highest seq that was acked to producer pre-crash       |

## 2. The `PlayerStressTestProbe` carrier

To verify **I1** and **I2** in O(1) per row, every produced write embeds a
monotonic per-key version into the payload, so we can compare without diffing
two protobufs.

We use a dedicated proto sub-message rather than repurposing an existing
field — repurposing would silently couple the test to whatever production
semantic that field happens to carry, and would make it impossible to detect
a *contaminated* row (an unrelated mutator overwriting one of the verifier's
checks).

```proto
// proto/common/component/player_comp.proto
message PlayerStressTestProbe {
  uint64 test_seq = 1;
  bytes  test_sig = 2;   // 16 bytes
}

// proto/common/database/mysql_database_table.proto
message player_database   { ... PlayerStressTestProbe stress_test_probe = 9; }
message player_database_1 { ... PlayerStressTestProbe stress_test_probe = 2; }
```

* `test_seq` is a per-`(player_id, msg_type)` monotonically-increasing
  uint64 — the L2 driver bumps it once per Kafka send (small 1..N values,
  used with `-mode strict`); the cpp Scene-side stamper uses
  `max(prior+1, now_us)` so that the seq is monotonic both within a Scene
  process AND **across Scene restarts**. Without that floor, a freshly
  restarted Scene would stamp `test_seq=1` over an existing high MySQL
  value (the Go consumer's monotonic guard is gated on Kafka offset, not
  on the proto field), and the verifier in `-mode atleast` would
  false-positive `MYSQL_BEHIND_EXPECTED` after every restart.
* `test_sig` is `FNV-1a-64(player_id ‖ msg_type ‖ test_seq)` computed twice
  with two seeded variants and concatenated big-endian into 16 bytes (see
  `go/db/internal/stresstest/probe.go` and
  `cpp/libs/services/scene/player/system/stress_test_probe.cpp`). The
  algorithm is intentionally non-cryptographic: it just has to be stable
  across two languages with no extra dependency. Golden vectors in
  `probe_test.go` lock the bit pattern in.

Verifier-side: `stresstest.VerifyProbe` returns the embedded seq and a bool
that is *only* true when `test_sig` matches what the seq+player+msgType would
have produced. A mismatch means either (a) some other writer touched this
row (contamination), or (b) the row is torn / mis-routed. Both are reported
as `*_SIG_INVALID` divergence kinds, separately from "wrong seq" kinds.

## 3. The four-layer test pyramid

```
┌──────────────────────────────────────────────────────────────────┐
│ L4  Chaos (kill nodes, partition Kafka with toxiproxy, etc.)     │  manual
├──────────────────────────────────────────────────────────────────┤
│ L3  E2E robot ─► Gate ─► Scene ─► Kafka ─► DB ─► MySQL+Redis     │  robot mode
├──────────────────────────────────────────────────────────────────┤
│ L2  Standalone Kafka driver ─► real DB consumer ─► MySQL+Redis   │  data_stress + verifier
├──────────────────────────────────────────────────────────────────┤
│ L1  Unit tests on processTaskBatch + dbOpHandlers                │  go test
└──────────────────────────────────────────────────────────────────┘
        ↑                   |
   sharper failures     fewer spurious flakes / faster
```

### L1 — Unit tests on the consumer

File: `go/db/internal/kafka/key_ordered_consumer_test.go`

Replaces `dbOpHandlers` with recording stubs; exercises `worker.processTaskBatch`
in isolation against miniredis. Six test cases:

| ID  | Name                                                     | Status      | What it asserts                                                                  |
| --- | -------------------------------------------------------- | ----------- | -------------------------------------------------------------------------------- |
| TC1 | `_SingleBatchCoalesces`                                  | GREEN       | A batch of N consecutive writes for the same key collapses to 1 effective write. |
| TC2 | `_CrossBatchOrdering`                                    | GREEN       | Each batch contributes its own latest write; final state = global max seq.       |
| TC3 | `_ReadActsAsBarrier`                                     | GREEN (fix) | Read between two writes must NOT skip the earlier write (regression for Bug 1).  |
| TC4 | `_PerKeyCoalesce`                                        | GREEN       | Multi-player interleaved batch — each key's latest write survives independently. |
| TC5a| `_KafkaFailure_MustEnterRetryQueue`                      | GREEN (fix) | Kafka-origin write failure enters retry queue with embedded seq (regression Bug 2). |
| TC5b| `_RetryAfterNewerWrite_MustNotRegress`                   | GREEN (fix) | Stale retry is dropped via `appliedSeqKey` guard (regression for Bug 3).         |
| TC6 | `TestConcurrentWorkers_FinalStateIsGlobalMax`            | GREEN       | Soak: many keys × many writes × 4 workers — every key converges to its max.      |
| TCS | `stresstest.TestComputeSig_KnownVectors`                 | GREEN       | Locked golden vectors for `ComputeSig`; cpp `stress_test_probe.cpp` mirrors them.|

CI runs the L1 set on every build via `go/test.bat`
(wraps `go/test.ps1`, which calls `go test -count=1 ./internal/kafka/...
./internal/stresstest/...` from `go/db`). Add `-Race` for the race detector.

Manual one-liner from the same dir:

```bash
cd go/db
go test -v -run "TestProcessTaskBatch_|TestConcurrentWorkers_" ./internal/kafka/...
go test -v ./internal/stresstest/...
```

### L2 — Standalone Kafka driver + verifier

Two binaries that share `go/db`'s deps so no extra modules are needed.

#### Driver: `go/db/cmd/data_stress`

Synthesizes `taskpb.DBTask` messages with the same Kafka key encoding the
production cpp Scene uses (`SavePlayerToRedis` writes `playerIdStr` as the
Kafka key). Each player gets writes 1..N where the seq is stamped into
`player_database.stress_test_probe.{test_seq,test_sig}` via
`stresstest.MakeProbe(playerID, msgType, seq)`.

```bash
cd go/db
go build ./cmd/data_stress
./data_stress -kafka 127.0.0.1:9092 -zone 1 \
              -redis 127.0.0.1:6379 \
              -players 1000 -writes 100 \
              -concurrency 64
```

After the run, Redis contains:

- `verify:enrolled:player_database` — set of all enrolled `player_id`s.
- `verify:expected:player_database:<player_id>` — string `<N>`, the highest
  seq this driver wrote for that player.

#### Verifier: `go/db/cmd/verifier`

Reads the expected map and polls MySQL+Redis until convergence (or a
deadline). Exits non-zero on persistent divergence.

```bash
cd go/db
go build ./cmd/verifier
./verifier -config etc/db.yaml \
           -redis 127.0.0.1:6379 \
           -wait 60s -interval 2s \
           -mode strict \
           -metrics-addr :9091
```

Flags worth noting:

* `-mode strict` (default, for L2): MySQL/cache `test_seq` must equal the
  expected value exactly. `-mode atleast` (for L3 robot): `test_seq` must be
  ≥ expected, since cpp Scene may stamp multiple times per session.
* `-metrics-addr :9091`: enables the Prometheus scrape endpoint at
  `:9091/metrics` with `verify_enrolled_players`, `verify_checks_total`,
  `verify_round_total`, `verify_converged`, `verify_seq_lag{store=...}`,
  and `verify_divergent_players{kind=...}`. Useful during chaos / soak runs.
* `-json` emits a machine-parseable summary on exit.

A passing run looks like:

```
round 1: checked=1000 mismatches=0 converged=true

=== verifier summary: converged ===
rounds=1, mismatches=0
```

A failing run flags the kind of divergence per player:

```
=== verifier summary: DIVERGENT ===
rounds=30, mismatches=3
  player=1000005 expected=100 mysql=98 cache=98 kind=BOTH_DIVERGENT
  player=1000049 expected=100 mysql=99 cache=0  kind=CACHE_MISSING
  player=1000150 expected=100 mysql=0  cache=0  kind=MYSQL_ROW_MISSING
```

Convergence kinds:

| Kind                    | Meaning                                                                       |
| ----------------------- | ----------------------------------------------------------------------------- |
| `MYSQL_ROW_MISSING`     | DB consumer never persisted any row for this player. Was Bug 2 (TC5a).        |
| `CACHE_MISSING`         | Persistence ok but Redis write-back skipped/expired.                          |
| `MYSQL_BEHIND_EXPECTED` | Row exists but `test_seq < expected`. Backlog or pre-Bug-3 (TC5b) regression. |
| `CACHE_BEHIND_EXPECTED` | Cache `test_seq < expected`. Stale write-back.                                |
| `BOTH_BEHIND_EXPECTED`  | MySQL and cache both behind (often same root cause).                          |
| `MYSQL_DIVERGENT`       | Row `test_seq != expected` but not behind (only meaningful in `strict` mode). |
| `CACHE_DIVERGENT`       | Cache `test_seq != expected` but not behind.                                  |
| `BOTH_DIVERGENT`        | MySQL and cache both diverge from expected.                                   |
| `MYSQL_SIG_INVALID`     | MySQL row's `test_sig` doesn't match its `test_seq` — torn / contaminated.    |
| `CACHE_SIG_INVALID`     | Same, in Redis cache.                                                         |
| `BOTH_SIG_INVALID`      | Both stores have invalid sigs — almost always proto-schema drift.             |

Pair this with `kafka-consumer-groups.sh --describe --group <go/db consumer group>`
for I3 (no backlog). The group name depends on how go/db was started (corrected
2026-10-16; the code is in place, not compiled or tested, pending Codex verification):

| How go/db runs | Consumer group | Topic |
| --- | --- | --- |
| Local single-zone default start (`go_services.ps1` without `-Zone`, i.e. `-Zone 0`, e.g. `dev-start`) | bare `db_rpc_consumer_group` (`go/db/etc/db.yaml` `ServerConfig.Kafka.GroupID`, unchanged) | `db_task_zone_1` |
| Local `-Zone N` start (`dev-start-zones`, or `go-svc-start -Zone N`) | `db_rpc_consumer_group_zN` (`go_services.ps1` appends `_zN` to the `GroupID` line only when `-Zone` > 0) | `db_task_zone_N` |
| K8s, after the zone was switched to its own group | `db_rpc_consumer_group_z<ZoneId>` | `db_task_zone_<ZoneId>` |

A K8s zone that has not been switched yet still consumes with the legacy shared
`db_rpc_consumer_group`; the live `go-svc-db-config` ConfigMap is authoritative
(see `docs/design/db-consumer-group-per-zone.md`). Topics carry a `_g<gen>`
suffix when `TopicGeneration` >= 2.

`tools/scripts/stress_summarize.ps1` follows the same rule: `-Zone` defaults
to 0, which keeps the defaults `db_task_zone_1` / bare `db_rpc_consumer_group`;
with `-Zone N` (N > 0) it derives `db_task_zone_N` / `db_rpc_consumer_group_zN`,
and an explicitly passed `-KafkaTopic` / `-KafkaConsumerGroup` always wins.

### L3 — E2E robot mode

File: `robot/data_stress.go`. Adds a new `data-stress` mode to the existing
robot binary. Each robot performs `rounds` × (login → enter → play → logout)
cycles and publishes `verify:expected:player_database:<player_id> = round`
to Redis after each successful logout, alongside the same
`verify:enrolled:*` set the L2 driver populates. The verifier treats both
populations identically.

```bash
cd robot
./robot -c etc/robot.data_stress.yaml
```

Sample config: `robot/etc/robot.data_stress.yaml`.

In robot mode, MySQL's `test_seq` is stamped by the cpp Scene-side probe
(`cpp/libs/services/scene/player/system/stress_test_probe.{h,cpp}`), which
is invoked from `SavePlayerToRedis` immediately before the protobuf is
serialized to Kafka. The probe is **off by default** and gated by the
environment variable `STRESS_TEST_PROBE=1`, so production builds carry zero
overhead. With it enabled, every snapshot Scene emits gets a fresh per-key
monotonic seq + matching `test_sig`.

Use `verifier -mode atleast` here: the robot publishes
`verify:expected:player_database:<player_id> = <round>` after each logout,
but Scene may stamp several snapshots per session (movement, level-up,
heartbeats), so the converged seq is `>= round`, not `==`.

### L4 — Chaos

The "kill DB consumer mid-batch" matrix entry is automated:

```powershell
pwsh -File tools/scripts/chaos_test.ps1 `
    -Players 200 -Writes 50 -KillCount 3 -KillIntervalMs 1500
```

What it does:

1. Builds `db.exe`, `data_stress.exe`, `verifier.exe` into `bin/chaos_test/`.
2. Starts the db consumer.
3. Starts `data_stress` to push 200 × 50 writes into the real Kafka.
4. Kills the consumer with `Stop-Process -Force` 3 times at random points
   while the driver is still producing, restarting between each kill.
5. After the driver finishes, runs `verifier -mode strict` and exits
   non-zero if any player did not converge to the produced max seq.

Strict mode is correct here because every Kafka message carries a unique
`test_seq` — the post-chaos state must match exactly (no lost write, no
stale-retry overwrite). If the verifier fails, logs are in
`bin/chaos_test/logs/{db,stress}.{out,err}.log`.

Remaining (still manual) faults:

| Fault                          | Tool                  | Expected behavior                                  |
| ------------------------------ | --------------------- | -------------------------------------------------- |
| Kafka network blip (200ms)     | toxiproxy             | Producer retries; final state correct.             |
| Redis OOM evicts cache         | `MAXMEMORY` config    | I1 still passes; I2 may downgrade to soft-pass.    |
| MySQL primary failover         | RDS / Aurora failover | Consumer reconnects; no data loss.                 |
| Scene node hard kill           | `docker rm -f`        | Last in-flight snapshot may be lost (acceptable); reconnect produces a fresh snapshot. |

## 4. The bugs the test suite caught (now fixed)

The unit-test suite was written against the *spec*, not against the
existing implementation. Three real bugs surfaced immediately, were fixed
in `go/db/internal/kafka/key_ordered_consumer.go`, and are now locked in
as regression tests in CI.

### Bug 1: read-after-write violation (TC3)

**Where:** `worker.processTaskBatch` in `go/db/internal/kafka/key_ordered_consumer.go`,
the loop that fills `lastWriteIdx` and skips superseded writes.

**Symptom:** for a batch `[W1, R, W2]` (same key+msg_type), W1 is silently
skipped. R then issues a SELECT that observes the pre-W1 row.

**Fix:** `processTaskBatch` now does a reverse pass over the batch and
treats any read for `(key, msgType)` as a coalescing barrier. Writes that
precede a read remain eligible for execution; only writes that have been
superseded *between two reads* (or after the final read) are dropped.

### Bug 2: Kafka-origin failure data loss (TC5a)

**Where:** `worker.handleTask` in the same file. The failure branch:

```go
if err != nil {
    logx.Errorf(...)
    if task.dbTask != nil {        // ← guard
        if saveErr := saveToRetryQueue(...); saveErr != nil { ... }
    }
}
```

After this branch, `MarkMessage` is unconditionally called for `kafkaMsg`
tasks. Kafka-origin failures are dropped *and* their offset is committed,
so the message can never be re-read.

**Fix:** `handleTask` now resolves the `*DBTask` up-front from either
`task.dbTask` or `task.kafkaMsg.Value`, and any failed apply (Kafka-origin
or retry-origin) is unconditionally enqueued via `saveToRetryQueue` before
the offset is marked. The retry payload is wrapped with a small magic +
the original Kafka offset (`wrapRetryPayload` / `unwrapRetryPayload`) so
Bug 3's monotonic guard can read the seq back.

### Bug 3: stale retry overwrites newer state (TC5b)

**Where:** `consumeRetryQueue` and the worker dispatch. A retry task popped
from Redis is routed by `key % partitionCount` to a worker, with no check
of whether a newer write for that same key has already been processed.

**Fix:** every successful write calls `markAppliedSeq` to set
`consumer:applied:<topic>:<key>:<msgType> = seq` in Redis. Before applying
*any* task — Kafka-origin or retry-origin — `handleTask` calls
`shouldApplyBySeq`, which drops the task if its embedded seq is ≤ the stored
applied seq. The seq is sourced from `msg.Offset + 1` for Kafka messages and
from the wrapped retry payload for retries, so the guard is consistent
across both paths.

## 5. End-to-end one-shot script

```bash
# 1. Make sure infra is up: Kafka, Redis, MySQL.
mprocs -c tools/dev/dev_mprocs.yaml   # or your usual spin-up

# 2. L1 unit tests on every PR (CI uses the same script).
pwsh -File go/test.ps1                # or simply: go\test.bat

# 3. L2: drive 1k players × 100 writes through the real consumer, then verify.
cd go/db
go build ./cmd/data_stress ./cmd/verifier
./data_stress -kafka 127.0.0.1:9092 -zone 1 -redis 127.0.0.1:6379 \
              -players 1000 -writes 100 -concurrency 64
./verifier    -config etc/db.yaml -redis 127.0.0.1:6379 \
              -wait 90s -interval 2s -mode strict
cd -

# 4. L3: robot mode hitting the full client → server stack.
#    Make sure Scene was launched with STRESS_TEST_PROBE=1.
cd robot
./robot -c etc/robot.data_stress.yaml
cd ../go/db
./verifier -config etc/db.yaml -redis 127.0.0.1:6379 -wait 60s -mode atleast

# 5. L4: chaos (kill consumer mid-batch).
pwsh -File tools/scripts/chaos_test.ps1
```

## 6. Future work

- Add toxiproxy-driven Kafka-blip and Redis-OOM scenarios to
  `chaos_test.ps1`.
- Promote the Prometheus metrics into a Grafana dashboard JSON checked into
  `deploy/k8s/observability/`.
- Extend the cpp probe to additional saved sub-tables once they're added to
  `SavePlayerToRedis`.
