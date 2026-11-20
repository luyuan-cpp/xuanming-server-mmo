# Error Aggregation & Reporting — Design (todo.md #250)

> **Status:** 2026-05-14 — in-process buffer landed
> (`error_reporter.h`); shipping to a central server (Sentry-style)
> is not done. This doc explains the split and what comes next.
>
> **2026-11-19 现状：** 收集（切片 A 的四处接点）、计数（切片 B）、节点内转储（切片 D）都已接好，事件带日志上下文（2026-11-12）。
> 集中推送（切片 C）、致命信号录入、Go 侧的同款缓冲（切片 E）定为不做，理由见文末「切片状态与定案」。
> 切片 B 与日志上下文都未编译，待 Codex 验证。

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

现在接了哪些调用点、各切片做到哪，见文末「切片状态与定案」。

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

## 切片状态与定案（2026-11-19）

原计划的五片与三个开放问题，2026-11-19 按「不等以后、先按标准做法定」全部落码或定案（负责人要求）。

| 切片 | 状态 |
|---|---|
| A. 接上错误点 | 已接四处：`RpcChannel` 的 `rpc_error_in`（对端拒绝）与 `rpc_error_out`（本端拒绝请求），都按 `ShouldLogRpcEvent` 抽样录；gate 的 `illegal_packet`（带累计次数）与 `router_lane_expired`。原列表里的 `SendErrorToClient` 已随调用方删除（2026-10-16，`muduo-rpc-channel.md` §20.3）；`return_define.h` 的宏调用点太密，不接。`HandleFatalSignal` 定为不接，见下文。 |
| B. Prometheus 计数 | 2026-11-19 已做，未编译。见下文。 |
| C. 集中推送 | 定为不做，见下文。 |
| D. SIGUSR2 转储 | 已做：Linux 上 `Node` 装 SIGUSR2，信号处理只置标志，主循环里写 `logs/error_reporter_dump_<时间>.jsonl`（`DumpSnapshotToDefaultPath`），首行是计数头。 |
| E. Go 的同款缓冲 | 定为不做，见下文。 |

**B. 三个指标**（`error_reporter.h` 的 `WriteMetrics` / `RegisterMetrics`）：

- `error_reporter_events_total`（counter）：进程启动以来录入的事件数。
- `error_reporter_dropped_events_total`（counter）：缓冲满了被挤掉的最旧事件数；它在涨，就说明转储之前已经丢了。
- `error_reporter_buffered_events`（gauge）：缓冲里现有的条数。
- 原方案带 `node_type` 标签，现在不带：节点类型与实例由抓取目标的标签给出，与其它 C++ 指标一致。
- `Node::StartMetricsExporter` 起 `/metrics` 时调 `RegisterMetrics()`，重复调用无副作用。`WriteMetrics(buffer, writer)` 不读全局状态，单测直接构造缓冲；用例在 `error_reporter_test.cpp`。

**C. 集中推送不做。** 原方案要在节点里起一个推送循环，把事件发到 Sentry 一类的服务。现在已有统一的日志管道（Alloy → Loki → Grafana，`docs/design/distributed-tracing.md`）：四个接点录入的同时都打了 WARN / ERROR 日志，行尾带 `trace_id` / `player_id`，集中查看与按级别告警都在 Loki 上做。再加一条推送就是第二份管道：要另写重试、落盘与限流，丢了还要另外对账，和单一事实源的原则冲突。缓冲只服务节点本身：计数进 `/metrics`，现场用 SIGUSR2 转储。

**致命信号不录。** `HandleFatalSignal` 已经用 `LOG_ERROR` 写了信号号、线程数与调用栈，这几行带着崩溃线程的上下文进日志与 Loki，之后进程照常生成 core dump。录入缓冲要加锁、分配内存，在信号处理函数里不是异步信号安全的：崩溃若正好发生在持有缓冲锁或 malloc 锁的线程上，处理函数会死锁，连那几行日志都写不出去。缓冲也随进程一起消失，还得在处理函数里再做一次文件转储。处理函数里能少做就少做。

**E. Go 的同款缓冲不做。** Go 服务的错误已经有两条去处：带 ctx 打的日志进 Loki（带 trace 与 player_id），各服务的错误计数进 Prometheus。进程内事件缓冲在 Go 侧没有别的用处。

### 开放问题的定案

1. **Go 的错误去哪**：同切片 E，不另做缓冲。
2. **消息里的敏感信息**：
   - 录入的 `message` 只许写方法名、消息号、计数、内部 id 这类诊断字段；不写账号、token、IP，也不写 proto 正文（`ShortDebugString` 之类）。
   - 转储只留在节点本地，不外发。
   - 现有四处都符合；新增接点按同一口径评审。
   - 不在 `Record` 里做自动脱敏：它不知道字段的语义，按名字猜着删既删不干净，又会误删。
3. **抽样**：由接点负责，照 `RpcChannel` 的 `ShouldLogRpcEvent` 只录抽中的那些。缓冲本身有界，满了挤掉最旧的，丢了多少看 `error_reporter_dropped_events_total`。不加按 tag 的限速。

---

## References

- `cpp/libs/engine/core/error_reporter/error_reporter.h` — the
  primitive (this slice)
- `cpp/libs/engine/core/error_handling/error_handling.h` (#70/#125)
  — likely first caller in slice A
- `cpp/libs/engine/core/node/system/node/node.cpp::HandleFatalSignal`
  (#105) — likely SIGUSR2 dump trigger in slice D
- todo.md #250 — origin
