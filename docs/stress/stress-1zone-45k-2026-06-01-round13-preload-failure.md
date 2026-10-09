# Stress Round 13 — 45k z1 — Preload TTL Bottleneck Postmortem

**Date**: 2026-06-01
**Run dir**: `robot/logs/stress-45k-z1-r13-20260601-021135/`
**Robot start**: 02:11:35 PDT
**Snapshots**: t2m / t3m / t13m / t18m at `prom-snapshots/`

## TL;DR
Round 12 fixed the C++ cold-start gRPC discovery race (`entergame_fail` 24% → 0%).
The residual ~4% bottleneck flagged at the end of Round 12 was a `dataloader_preload_cb_wait{fail}` timeout averaging ~5,495ms (5s TTL hits with overshoot).
Round 13 isolated the cause to **go-redis Pub/Sub channel back-pressure**: `psub.Channel()` defaults to a 100-msg buffer with 60s send timeout; under enter-game bursts of ~2000/s × 4 sub-table notifies, the buffer overflows and the dispatcher subscriber goroutine blocks, starving the 5s registration TTL.

## Fix
`go/login/internal/dispatcher/task_result_dispatcher.go`:

1. **Larger buffered Pub/Sub channel**:
   ```go
   ch := psub.Channel(
       redis.WithChannelSize(8192),
       redis.WithChannelSendTimeout(5*time.Second),
   )
   ```
   - 8192 absorbs the worst observed burst (8k notifies/s peak).
   - 5s `chanSendTimeout` (down from default 60s) fails fast if the subscriber loop is genuinely stuck — the channel surfaces a drop in logs instead of silently piling up multi-second back-pressure.

2. **Last-chance LPop in `sweepExpired`**:
   The DB consumer LPushes the `TaskResult` BEFORE Publish. If the notify is lost (channel overflow, transient subscriber pause), the data is still in `task:result:{id}`. Before firing the timeout callback, attempt one synchronous `LPop`; if data is present, decode and deliver success instead of failure.

## Setup Notes (Operational Lessons)
- **Robot/gateway death at t≈2min in earlier attempt**: Caused by launching child processes with `Start-Process -NoNewWindow` in PowerShell. When the parent terminal session was recycled/reaped, the inherited console was destroyed, killing the children silently (no panic, no exit log).
  - **Lesson**: All long-running stress processes (robot, java gateway, sa-token) must be started with `Start-Process -WindowStyle Hidden` (or via a detached scheduler) so they get their own console group and survive parent terminal lifecycle events.
- **MySQL `mmorpg` DB + `appuser` had to be created** before gateway boot (Round 13 was a first run after a fresh dev environment).
- **JDK 23 required** for the gateway jar (compiled with JDK 23 class version 67). JDK 21 fails with `UnsupportedClassVersionError`.

## Metrics

### t2m (02:13:35) — early ramp
- `entergame_total{success}=8603` `entergame_total{fail}=0`
- `dataloader_preload_callback_wait_seconds_sum/count{success} = 186.3 / 2543 ≈ 73ms avg`
- Robot stats: `conn=4834 enter_ok=4805 enter_fail=0 msg_recv=5526/s`

### t13m (02:24:35) — steady state, baseline (BEFORE fix)
- `entergame_total{success}=61,918` `{preload_failed}=1,325` `{apply_failed}=5`
- **preload_failed rate = 1325 / 63,248 = 2.10%**
- `dataloader_preload_callback_wait_seconds{failed}`: sum=7,261s / count=1,325 → **avg 5.48s** (classic 5s TTL with overshoot)
- `dataloader_preload_callback_wait_seconds{success}`: sum=3,386s / count=14,033 → avg 241ms (healthy)
- `entergame_preload_seconds{preload_failed}`: sum=7,278s / count=1,325 → avg 5.49s
- Robot stats: `conn=57,168 enter_ok=56,412 enter_fail=0 msg_sent=1,251/s`

**Diagnosis confirmed**: every failed preload waits ~5.5s (TTL + scheduling slop), then the registration GC fires the timeout callback. The TaskResult data is sitting in Redis already — only the notify is being dropped/delayed by the 100-msg default Pub/Sub buffer.

### t18m (02:29:35) — pre-fix final baseline
- `entergame_total{success}=88,795` `{preload_failed}=4,145` `{apply_failed}=5`
- **preload_failed rate = 4145 / 92,945 = 4.46%** (rising from 2.10% at t13m as backlog accumulates)
- `dataloader_preload_callback_wait_seconds{failed}`: sum=22,753s / count=4,145 → **avg 5.49s** (TTL pinned)
- `dataloader_preload_callback_wait_seconds{success}`: sum=5,410s / count=17,780 → avg 304ms
- `entergame_preload_seconds{preload_failed}`: sum=22,811s / count=4,145 → avg 5.50s
- Robot stats 18m10s: `conn=91,379 login_ok=89,779 enter_ok=89,779 enter_fail=0 msg_sent=1,210/s`

**Trend confirmed**: failure rate doubles between t13m (2.10%) and t18m (4.46%). With buffer-of-100 + 60s send-timeout, the dispatcher cannot drain notifies fast enough at ~2k enter/s × ~4 sub-tables; once the buffer fills, every additional notify blocks for up to 60s, causing the registration GC to fire timeouts even though Redis already holds the data. Robot-level enter success stays at 100% because the C++ side retries — the loss surfaces only as elevated p50/p99 entergame latency.

## Verification Plan
After applying the dispatcher fix:
1. Rebuild `login` service (`pwsh tools/scripts/dev_tools.ps1 -Command go-svc-build -GoServices login`).
2. Restart login service.
3. Re-run identical 45k-z1 stress.
4. Expected:
   - `dataloader_preload_cb_wait{fail}` count drops to ~0 (or any residual failures should be salvaged by the LPop fallback, registered as success).
   - `entergame_preload_failed` near-zero at steady state.
   - Salvage log line `TaskResultDispatcher salvaged taskID=...` appears at most in low-single-digit counts per minute under burst.

## Verification Results (POST-FIX, run `stress-45k-z1-r13fix-20260601-023118`)

### Post-fix snapshots (login PID 57776, fixed binary)
| Stage | success | preload_failed | failure rate | avg fail wait | avg success wait |
| --- | ---: | ---: | ---: | ---: | ---: |
| t13m PRE | 61,918 | 1,325 | **2.10 %** | 5.48 s | 0.24 s |
| t18m PRE | 88,795 | 4,145 | **4.46 %** | 5.49 s | 0.30 s |
| t13m POST | 51,779 | **0** | **0.00 %** | n/a | n/a |
| t18m POST | 73,507 | **0** | **0.00 %** | n/a | 0.234 s (count=6) |

### Interpretation
- `entergame_total{preload_failed}` series is absent at both t13m and t18m → **zero preload timeouts** across the entire run.
- `dataloader_preload_callback_wait_seconds_count{success}` collapsed from 17,780 (pre-fix t18m) to **6** (post-fix t18m). Meaning: with the larger channel buffer (8192) and 5-second send timeout, nearly all DB-task results are delivered through the Pub/Sub fast path while the request is still in-flight, so the registration's TTL `<-time.After` branch is effectively dead code under normal load.
- `entergame_preload_seconds{success}` mean dropped from 5,928 / 88,821 ≈ 66.7 ms (pre-fix, success branch only) to 140.7 / 73,533 ≈ **1.9 ms** end-to-end preload latency. The 35× speedup is the elimination of GC-driven re-checks.
- No `TaskResultDispatcher salvaged taskID=` lines were emitted — the LPop fallback never had to fire, which is the desired steady-state behaviour. The fallback remains as a safety net for true channel-drop scenarios.
- Robot-side at 18m25s: `enter_ok=75,284 enter_fail=1 login_fail=27 login_stuck=9` (the 27 login_fail / 9 stuck are unrelated to preload — they correlate with brief gate reconnect storms; investigate separately).

### Conclusion
The dispatcher channel-sizing fix (`WithChannelSize(8192)`, `WithChannelSendTimeout(5*time.Second)`) plus the last-chance LPop salvage in `sweepExpired` completely eliminates the Round 13 preload bottleneck under the 45k-z1 single-zone profile. Recommend merging and re-running the 4-zone × 45k profile to confirm scaling headroom.

## References
- `go/login/internal/dispatcher/task_result_dispatcher.go` (modified)
- go-redis Pub/Sub defaults: `vendor/github.com/redis/go-redis/v9@v9.16.0/pubsub.go` line 648-651 (`chanSize=100`, `chanSendTimeout=60s`).
- Round 12 postmortem: `docs/stress/stress-1zone-45k-2026-05-29-round12-cold-start.md` (previous race fix).
