# Error Aggregation & Reporting — Design (todo.md #250)

> **Status:** 2026-05-14 — in-process buffer landed
> (`error_reporter.h`); shipping to a central server (Sentry-style)
> is not done. This doc explains the split and what comes next.
>
> **2026-11-12 核对现状：** 切片 A 接了四处（`RpcChannel` 的 `rpc_error_in` / `rpc_error_out`，gate 的
> `illegal_packet` 与 `router_lane_expired`），`HandleFatalSignal` 那一处没接；切片 D（SIGUSR2 转储）已在
> `Node` 里接好；切片 B、C、E 没做。同日事件带上了日志上下文（见下文「日志上下文」），未编译。

---

## Problem

Today errors land in muduo logs and that's it. Triage requires SSHing
into the right node, finding the right log file, grepping for the
right time window. When 50 nodes are running this is unworkable; when
an error pattern crosses nodes (e.g. a bad config knob deployed to
zone 2 causes a spike on every zone-2 gate) it's invisible.

todo #250 calls out the gap. Sentry / GlitchTip / Bugsnag / self-host
Loki-style aggregation are all reasonable answers; the choice is a
deployment-level decision, not an engine-level one.

---

## Decision

### Split into "collect" (engine) and "ship" (deployment)

The engine ships **only** the in-process aggregator: a bounded ring
buffer that any error site pushes into, with three consumer paths
(Drain, Snapshot, counter exports). Downstream pickup is deployment-
controlled:

| Layer | Component | Responsibility |
|---|---|---|
| Engine | `error_reporter::Buffer` | Collect events from every error site into a single bounded queue. |
| Engine | `error_reporter::Record(code, tag, msg)` | One-liner call-site API. |
| Engine | Prometheus exporter (slice B) | Export `error_total / dropped_total / current_size` as counters. |
| Deployment | HTTP push loop (slice C) | Periodically Drain() the buffer and POST events to whatever aggregator the deployment chose. |
| Deployment | SIGUSR2 dump (slice D, optional) | On a diagnostic signal, write Snapshot() to a file for the on-call to read directly. |

This separation matters because **engine code shouldn't have an
opinion on whether the deployment uses Sentry**. Multiple deployments
(on-prem behind firewall vs SaaS-OK environments) end up with
different shippers; the buffer stays one piece.

### Bounded ring, drop-oldest

1024-event capacity by default. When full, the OLDEST event drops and
`dropped_count_` increments. Rationale:

- Recent events are more interesting for triage. Old ones are usually
  the noise from a prior bug that's already known.
- Drop-on-full is non-blocking so the recorder is safe to call from
  signal handlers (#105 / #216) and hot paths (#236 illegal-packet
  rejection).
- The dropped counter makes "I'm losing events" visible to monitoring
  without requiring backpressure plumbing.

If a deployment finds 1024 too tight, bump via constructor arg. We
don't expose the cap as an env knob because the right answer is
usually "ship more often, not buffer more events" — fix it in slice C.

### Threading: one mutex over a `std::deque`

The buffer is shared across:
- Every gate / scene / login event loop (muduo)
- The Kafka consumer pool
- The proto-gen workers
- Signal handlers (best-effort; technically not async-signal-safe due
  to allocation, but the same caveat applies to muduo's LOG_ERROR
  that the project already uses from signal handlers — see #105)

A single `std::mutex` protects the deque. The contention point is one
push per ERROR event (rare by design) versus the legal-message
throughput path, so the lock is well outside any hot path. We could
move to `boost::lockfree::queue` later if profiling ever showed it
mattered — currently overkill.

### Singleton via Meyers pattern

`error_reporter::Instance()` returns a `static` local Buffer. Leak-on-
exit by construction — avoids "an error is recorded during static
destructor teardown" lifetime races.

---

## What landed in this commit (2026-05-14)

| File | Change |
|---|---|
| `cpp/libs/engine/core/error_reporter/error_reporter.h` (new) | `Buffer { Record / Drain / Snapshot / TotalCount / DroppedCount / CurrentSize }` + namespace-level `Record(code, tag, msg)` convenience + `Instance()` singleton. Header-only, depends on `<mutex>` / `<deque>` / `<atomic>` only. |
| `docs/design/error-reporting.md` (this file) | Decision record. |

现在接了哪些调用点见文首的核对；还没接的列在下面的切片 A 里。

### 日志上下文（2026-11-12）

每条事件记下录入那一刻线程上的 trace 与玩家（`docs/design/distributed-tracing.md` §13 第 4 条）：

| 字段 | 来源 | 没有时 |
|---|---|---|
| `trace` | `tracing::Current()` | `IsValid()` 为 false |
| `player_id` | `tracing::CurrentPlayerId()` | 0 |

- 业务照旧调 `error_reporter::Record(code, tag, msg)`，由它取当前线程的值；`Buffer::Record` 改为显式接收这两项，本身不读线程局部状态，单测可以直接构造。
- JSONL 转储按日志行尾后缀的口径「有值才写」：有 trace 写 `trace_id`（32 位）与 `span_id`（16 位），有玩家写 `player_id`。`player_id` 写成十进制字符串，jq 这类按双精度解析数字的工具读超过 2^53 的 id 也不丢精度。
- 用途：拿转储里的 `trace_id` 到 Loki 查同一请求在各服务的全部日志。
- 用例：`cpp/tests/rpc_controller_test/error_reporter_test.cpp`。

---

## What's NOT done (slices to track)

### A. Wire the existing error sites into the buffer — **S, ~1 day**

Add a `Record()` call alongside every existing LOG_ERROR-on-error site:

- `error_handling_system.h::SendErrorToClient` — already logs context
  via #70/#125; mirror that into the buffer with `tag="rpc_reject"`
  and `error_code=err`. (2026-10-16: removed. Its only caller was the
  retired `Scene.InvokePlayerService`, see `muduo-rpc-channel.md` §20.3.)
- `game_channel.cpp::HandleResponseMessage` RPC_ERROR branch +
  `SendErrorResponse` — `tag="rpc_error_in"` / `"rpc_error_out"`.
- `return_define.h` macros — too noisy on every call site; SKIP. The
  RPC boundary catches everything that escapes.
- `client_message_processor.cpp::CheckMessageLimit` (#236 hook) —
  `tag="illegal_packet"`, message includes the running count.
- `HandleFatalSignal` (#105) — `tag="fatal_signal"`, error_code=signum.
  Note: this MUST drain to file before re-raise, since the process is
  about to die.

### B. Prometheus exporter — **S, ~half day**

Three gauges in scrape_endpoint:

```
error_reporter_total{node_type}        // monotonic since process start
error_reporter_dropped_total{node_type}
error_reporter_current_size{node_type}
```

Same wiring pattern as `login_auth_path_total` (see
`docs/ops/grafana-login-path-deprecation.json` for the dashboard
template).

### C. HTTP push loop — **M, 1–2 days; deployment-specific**

Pick the shipping target — recommendation: a thin self-hosted Loki +
Grafana stack to start (no SaaS vendor lock-in, runs on the same K8s
the rest does). Sentry SDK is fine if ops prefers managed.

Pseudocode for the loop:

```cpp
loop.runEvery(10.0, [] {
    auto events = error_reporter::Instance().Drain();
    if (events.empty()) return;
    SerializeAndPost(events);  // deployment-defined endpoint
});
```

Failure mode: if the shipper can't reach its endpoint, events go to
the FLOOR — they were already drained from the buffer. Options:
- Drain into an on-disk WAL and let the shipper re-read on retry.
- Accept the loss and bump the per-node alert (the shipper itself
  should emit a failure metric).

Don't pre-decide here; pick when slice C lands.

### D. SIGUSR2 dump-to-file — **S, ~half day; optional**

Useful for "the central shipper is down, but the on-call needs to see
what's been failing on this node":

```
kill -USR2 <pid>
# writes /tmp/error_reporter_<pid>_<timestamp>.jsonl
```

Same install pattern as the SIGUSR1 diagnostic handler from #216.

### E. Cross-language coverage — **L, when needed**

`error_reporter.h` is cpp-only. The Go nodes (login / scene_manager
/ data_service / etc.) have their own logging chain; an equivalent
`error_reporter` package + Prometheus counters is symmetric but
non-trivial. Defer until a deployment actually needs unified
cross-language error visibility — Prometheus + `go_login_errors_total`
gauges may be enough for most ops needs.

---

## Open questions

1. **Where do errors from Go services land?** The Go side already
   has `goerrors` returned out of every logic function but no
   aggregator. Slice E above. Probably fine to keep them in
   Prometheus counters and skip the per-event aggregator entirely
   on the Go side.

2. **PII in error messages.** A `ShortDebugString` of a proto
   payload may contain account names, tokens, IP addresses. Before
   slice C ships events off-node, decide:
   - Strip known PII fields (account, token, ip) before recording?
   - Or trust the aggregator to redact?
   - Or only ship aggregate counts, not bodies?

   Recommendation: strip at the recorder (`error_reporter::Record`
   does the strip), so the buffer itself never holds PII. Conservative
   default.

3. **Sampling.** If error rate explodes (e.g. a config rollout
   breaks every login), the buffer fills in milliseconds and
   slice C might struggle. Add a per-tag rate cap in slice A?

---

## References

- `cpp/libs/engine/core/error_reporter/error_reporter.h` — the
  primitive (this slice)
- `cpp/libs/engine/core/error_handling/error_handling.h` (#70/#125)
  — likely first caller in slice A
- `cpp/libs/engine/core/node/system/node/node.cpp::HandleFatalSignal`
  (#105) — likely SIGUSR2 dump trigger in slice D
- todo.md #250 — origin
