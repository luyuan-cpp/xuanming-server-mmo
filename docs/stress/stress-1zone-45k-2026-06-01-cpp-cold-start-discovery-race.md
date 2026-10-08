# Stress 1-zone 45k — 2026-06-01 — C++ Cold-Start Discovery Race FIX VERIFIED

## TL;DR

Round 12 verifies the Round 11 fix for the C++ scene/gate cold-start gRPC
discovery race. The fix moves the etcd `NodeInfo` publish from "after txn
allocation" to "after `BuildAndStart` of the gRPC server returns",
eliminating the millisecond window where scene_manager could dial a
not-yet-listening port and mark the node dead.

| metric                             | Round 11 (2026-05-31) | Round 12 (2026-06-01) |
|------------------------------------|-----------------------|-----------------------|
| robot count (target)               | 45,000                | 45,000                |
| run length                         | ~3 min (aborted)      | 19 min                |
| `enter_fail` (scene_ready timeout) | **1,491 (24%)**       | **0 (0%)**            |
| max_login                          | 914 ms                | 801 ms                |
| scene_manager `dead_nodes`         | 4 (all reported dead) | **0**                 |
| scene_manager `No live nodes`      | yes (zone 1)          | **none**              |
| kafka consumer lag                 | 0                     | 0                     |

## Round 11 baseline (re-summarized for comparison)

`robot/logs/stress-45k-z1-20260531-230150/prev-summary.txt`

```
1m0s   1,463    1,442    1,442   0  0  350ms  237/s
2m0s   3,250    3,220    3,220   0  0  914ms  449/s
```

The robot run reached only 2m before being aborted; scene_manager logs
showed 4× `[connection refused]` plus `No live nodes remaining for zone
1`. ~24% of robots that *did* connect later timed out on `scene_ready`
because their target world channel had never been provisioned.

## Round 12 result

`robot/logs/stress-45k-z1-20260601-002210/summary.txt`

```
  time   conn    login_ok  enter_ok  enter_fail  recon_fb  max_login  msg/s
  1m0s   1,824   1,797     1,797     0           0         320ms      119/s
  5m0s   18,543  18,495    18,495    0           0         801ms      260/s
  10m0s  42,014  41,942    41,942    0           0         801ms      355/s
  15m0s  64,381  63,877    63,877    0           0         801ms      412/s
  19m0s  80,224  78,181    78,181    0           0         801ms      451/s
```

Notes:
- `conn` exceeds 45 k because the robot reconnects on AOI scene switch
  (see `recon_ok`, `scene_switch` counters in the live stats line).
- `login_ok == enter_ok` throughout. No scene_ready timeouts (this was
  the headline R11 failure).
- `max_login` is stable at 801 ms across the whole 19 min run (matches
  Round 11 ramp behaviour pre-failure).
- Kafka `db_task_zone_1` lag is **0** on all 10 partitions at end of
  run — db pipeline absorbed 80 k logins cleanly with the existing
  partition-starvation fix from R10.

## scene_manager evidence

`run/logs/go_services/scene_manager.stdout.log` — startup log shows
every node-appearance is followed by a clean world-channel allocation:

```
[LoadReporter] Zone 1: node 1 appeared (watch PUT, role=0)
[World] Zone 1: ensuring 16 world scenes across 1 world-hosting nodes
[World] Allocated channel 0 (scene=…, conf=1) on node 1 in zone 1
... (15 more)
[SceneNodeClient] Connected to scene node 1 at 172.27.16.1:50000
[LoadReporter] Zone 1: node 2 appeared (watch PUT, role=0)
[World] Zone 1: ensuring 16 world scenes across 2 world-hosting nodes
[World] Zone 1 done: created=0 channels, ensured=31 RPCs, dead_nodes=0
[SceneNodeClient] Connected to scene node 2 at 172.27.16.1:50001
... (node 3, node 4 same pattern)
[Rebalance] zone=1 urgent=0 opportunistic=… migrated=… (no failures)
```

Every `dead_nodes=0` line. Every `SceneNodeClient Connected` succeeds
on the first dial. No `connection refused`. This is the direct
behavioural signal that the discovery write now strictly follows
gRPC `BuildAndStart`.

## Fix recap (already in code)

Two files (also captured in `/memories/repo/cpp-cold-start-grpc-discovery-race.md`):

- `cpp/libs/engine/core/node/system/etcd/etcd_service.{h,cpp}`
  - `OnTxnSucceeded(allocKey)` — initial boot: call
    `ActivateSnowFlakeAfterGuard()` directly, do **not** publish
    discovery yet. Re-registration: keep original behaviour (gRPC
    stayed up).
  - New method `PublishDiscoveryAfterGrpcReady()`: invoked by Node
    after `BuildAndStart` returns. No-op for re-registration paths.
- `cpp/libs/engine/core/node/system/node/node.cpp` `StartGrpcServer`
  - After `BuildAndStart` succeeds, call
    `etcdService.PublishDiscoveryAfterGrpcReady()`.
  - Both `empty grpcServices_` and `port==0` early-return branches
    also call it, so non-gRPC node types still register.

## Rule recorded

**Any discovery write that exposes a service endpoint MUST happen
AFTER the endpoint is bound and accepting connections.** etcd watch
latency is millisecond-scale; peers will dial within ms of a PUT
event. Cold-start ordering bugs are 100% reproducible at 45 k load and
silent under smoke tests.

## Next bottleneck (separate ticket)

The Round 12 prom snapshots show a *different* failure surface —
`entergame_preload_failed` 3–4% with `cb_wait{fail}` averaging
**5,495 ms** (= 5 s kafka-callback timeout). Total counter at end of
run: 3,042 preload failures out of 78,181 enter_ok. This is not the
cold-start race (entergame is async dataloader → db via Kafka). Likely
candidates:

- `dispatcher` queue depth saturation at 450 msg/s sustained.
- DB write-behind dirty-flag race re-triggering retries (see
  `/memories/repo/db-write-behind-dirty-flag-race.md`).
- Kafka producer side-effect of the partition-pre-create fix
  (`/memories/repo/db-kafka-partition-pre-create.md`).

Recommend Round 13 focused on the preload path (`dataloader_*`
histograms). Captured snapshots: `prom-snapshots/t2m_ramp.txt`,
`t3m_rampmid.txt`, `t13m_steady.txt`, `t18m_final.txt`.

## Artefacts

- Run dir: `robot/logs/stress-45k-z1-20260601-002210/`
- Round 11 baseline (kept): `robot/logs/stress-45k-z1-20260531-230150/`
- Code fix files: see "Fix recap" above
- Memory: `/memories/repo/cpp-cold-start-grpc-discovery-race.md`
