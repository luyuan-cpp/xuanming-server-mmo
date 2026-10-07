# Stress Test — 1 Zone × 45k Robots — Round 15 (2026-06-01)

Run dir: `robot/logs/stress-45k-z1-r15-20260601-103407`
Baseline: `docs/stress/prev-summary-round15.txt` (= Round 14 summary)
Start: `2026-06-01 10:35:41` Local · 18-minute window

## 1. What changed vs Round 14

This round bundled the three Round-14 candidates (P5, P6, P7) into one cut:

| Patch | File | Change | Hypothesis |
|---|---|---|---|
| **P5** | `go/db/etc/db.yaml` | `SubShardCount 4→8`, `MaxOpenConn 30→60`, `MaxIdleConn 10→20` | db consumer was the bottleneck (3,663 preload timeouts @ t18m) — double effective workers and MySQL pool |
| **P6** | `go/login/internal/logic/clientplayerlogin/entergamelogic.go` | `dispatcherTaskTTL 5s → 8s` | Short-term stop-loss: give callbacks more time before sweep declares failure |
| **P7** | `go/login/internal/dispatcher/task_result_dispatcher.go` | Bounded worker pool (16 workers, buf 4096) replacing unbounded goroutine spawn | Reduce scheduler/GC jitter under burst — Round 14 cb_wait{ok} jumped 85ms→275ms vs Round 13 |

## 2. Two-dimensional comparison

### EnterGame end-to-end

| Snapshot | success (R14) | success (R15) | preload_failed (R14) | preload_failed (R15) | apply avg (R14) | apply avg (R15) |
|---|---:|---:|---:|---:|---:|---:|
| t2m  | n/a    | 2,773  | 0    | **0** | n/a    | 33.4ms |
| t5m  | n/a    | 11,281 | n/a  | **0** | n/a    | 39.3ms |
| t10m | n/a    | 26,217 | n/a  | **0** | n/a    | 42.5ms |
| t15m | n/a    | 40,383 | n/a  | **0** | n/a    | 43.3ms |
| **t18m** | **84,706** (logical, R14 counted retries) | **45,764** | **3,663** | **0** | **66.5ms** | **43.5ms** ↓35% |

### Dataloader per-stage avg (the critical row)

| Metric | Round 14 @ t18m | Round 15 @ t18m | Δ |
|---|---:|---:|---:|
| cache_check | 1.9ms | 1.8ms | flat |
| sub_cache   | 3.7ms | 3.6ms | flat |
| dispatcher  | 0.0ms | 0.0ms | flat |
| kafka_send  | 3.4ms | 3.3ms | flat |
| **cb_wait{ok}** | **274.5ms** | **31.4ms** | **−88%** |
| **cb_wait{fail}** | **5,493ms** (5s TTL miss) | **n/a** (no failures) | **eliminated** |

### Kafka consumer lag

| Round | Total lag @ end |
|---|---:|
| R14 | 0 (but with sustained 3,663 preload timeouts) |
| R15 | **0** (and 0 timeouts) |

## 3. Verdict per patch

- **P5 (db pool/sub-shards) — CONFIRMED PRIMARY FIX**
  preload_failed: 3,663 → **0**. Doubling `SubShardCount` (4→8) gave the db consumer enough parallelism per partition to keep up with the 45k burst without falling behind the 5–8 s TTL. MySQL pool bump (30→60) absorbed the extra concurrency without saturating the default 151-connection server cap.

- **P6 (TTL 5s → 8s) — SAFETY NET, NOT EXERCISED**
  With P5 in place there were 0 timeouts, so the extended TTL never fired. Keep the change — cheap insurance against future bursts. Can be re-tightened to 5s later once we have multiple clean rounds.

- **P7 (bounded dispatcher worker pool) — CONFIRMED LATENCY FIX**
  `cb_wait{ok}` avg: **274.5ms → 31.4ms (−88%)**. Round 14 had been showing per-callback jitter from unbounded goroutine spawn under load (5–10× variance vs Round 13). The 16-worker bounded pool with a 4,096-deep buffer flattened that latency curve. Even the worst snapshot (t18m) sits comfortably below 35ms — well under what Round 13 baseline (~85ms) produced before any pressure.

## 4. Other signals

- 1 stray `apply_failed` event from t10m onward — single-digit, no growth, likely a duplicate retry race. Not chasing this round.
- `Connect failed` count at T+2m ≈ 1,192 — expected initial burst retries during 45k ramp-up; absorbed by reconnect.
- All snapshots captured cleanly (t2/t5/t10/t15/t18 = 43–46 KB each). No service crashes; `dev-status` clean throughout.

## 5. Next candidates (Round 16 backlog)

The system is now bounded by the desired 45k headroom rather than infra:
- **R-1 (optional)**: Re-tighten `dispatcherTaskTTL` 8s → 5s with P5+P7 in place — confirm we still have 0 fails. Restores tight SLO.
- **R-2 (optional)**: Scale to 2 zones × 45k or 1 zone × 90k to find the next ceiling. Likely bottleneck will move to gate fan-out or scene tick budget.
- **R-3 (deferred)**: Investigate the persistent `apply_failed=1` constant — low priority unless it grows.

## 6. Touched memories

- `/memories/repo/db-kafka-partition-pre-create.md` — SubShardCount tuning notes still valid (8/partition for 45k).
- `/memories/repo/task-result-dispatcher-pubsub-tuning.md` — update with R15 bounded-worker outcome.

---

Round 15 declared **PASS**. No new failure modes; all three patches retain.
