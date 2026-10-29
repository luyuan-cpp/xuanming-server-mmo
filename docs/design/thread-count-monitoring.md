# Thread Count Monitoring & Control per Process

## Overview

Every C++ node automatically runs a **ThreadMonitor** that periodically samples the process thread count and logs warnings when sustained growth is detected. Go services rely on goroutine scheduling and per-partition worker pools for concurrency control.

A single muduo EventLoop means **single-threaded ownership of business state**,
not a single-threaded process. Kafka, logging, and lifecycle components have
background threads. gRPC does not: since the muduo-driven gRPC migration
(batches 3 and 4, `docs/design/grpc-muduo-transport.md` §14–§15) the generated
gRPC clients and the scene / battle gRPC servers run on the node's own EventLoop
through `muduo_grpc` and create no threads.
2026-10-26 起，C++ 工程不再链接官方 gRPC SDK；Protobuf、Abseil、GoogleTest 与必要的
BoringSSL 各自独立管理，协议与 Go 服务保持兼容。

---

## C++ Nodes — ThreadMonitor (Automatic, All Nodes)

### Implementation
- **File**: `cpp/libs/engine/core/node/system/node/thread_observability.h`
- **Registration**: Node initialization calls `RegisterThreadObservability()` — enabled by default for every node
- **Mechanism**: Timer on main EventLoop samples thread count every N seconds
  - Windows: `CreateToolhelp32Snapshot` + `Thread32First/Next`
  - Linux: `readdir("/proc/self/task")`
- **Behavior**: Passive monitoring only — logs warnings, does **NOT** enforce hard limits or kill threads

### Environment Variables

| Env Var | Default | Description |
|---------|---------|-------------|
| `NODE_THREAD_MONITOR_ENABLED` | `1` (on) | Set `0` / `false` / `off` to disable |
| `NODE_THREAD_MONITOR_SAMPLE_INTERVAL_SECONDS` | `10` | How often to sample (seconds) |
| `NODE_THREAD_MONITOR_STABLE_WINDOW_SECONDS` | `60` | Wait this long before setting baseline |
| `NODE_THREAD_MONITOR_GROWTH_WARN_CONSECUTIVE_SAMPLES` | `6` | Consecutive growth samples before warning |
| `NODE_THREAD_MONITOR_GROWTH_WARN_ABSOLUTE_INCREASE` | `16` | Min absolute increase from baseline to trigger warning |

### Log Output Examples

```
[INFO]  Gate thread metrics: total_threads=12, delta_threads=0, peak_threads=14, uptime_sec=120
[INFO]  Gate thread baseline (stable window reached): baseline_threads=12, ...
[WARN]  Gate thread growth warning: total_threads=30, increase_from_baseline=+18, consecutive_growth_samples=7, ...
```

`thread metrics` 行在第一次采样时输出，此后最多每小时一次；持续增长告警继续使用原有基线与阈值。

### C++ SDK 依赖边界（2026-10-26）

节点使用 `muduo_grpc` 与 nghttp2 实现标准 gRPC 协议，消息序列化由独立 Protobuf 提供。
官方 gRPC C++ SDK、官方桩生成插件及其运行时链接项已经退出 C++ 构建。原运行时探针、
配套守卫配置、日志字段与仅检查该探针的旧测试同步移除；ThreadMonitor 的采样、基线、
增长告警和启停配置继续生效。

依赖边界由 `tools/scripts/tests/test_cpp_grpc_dependency_boundary.py` 检查：覆盖全部
Windows 工程配置及继承 targets，拒绝官方 SDK 路径、专用库、头文件、运行时调用和官方
生成桩，核对 Linux CMake 不再查找官方 SDK，同时允许自有 `grpc_client` 与 `muduo_grpc`。
生成客户端的本地拒绝与真实回环行为分别保留在 `grpc_client_local_reject_test.cpp` 和
`grpc_client_loopback_test.cpp`；静态边界检查不能代替这些行为测试、实际构建与互通验证。

核对节点线程时，继续比较同等条件下的 `thread baseline (stable window reached)` 和
`thread metrics`；Linux 可通过
`for t in /proc/<pid>/task/*; do cat $t/comm; done | sort | uniq -c`
核实实际线程来源。移除官方 SDK 不给 Kafka、日志或其它组件增加固定线程数保证。

---

## C++ Runtime Thread Composition

A fixed process-wide range such as “5–14 threads” is still not valid:
librdkafka, logging, and lifecycle workers are implementation-dependent, so
production baselines must come from ThreadMonitor measurements. What changed
with batches 3–4 is that gRPC no longer contributes any thread. Before, the
default gRPC EventEngine alone eagerly started `Clamp(gpr_cpu_num_cores(), 4, 16)`
workers plus a Lifeguard (17 threads on a host with 16 or more logical CPUs)
and could grow under backlog, nodes with a gRPC server added up to 8 sync
pollers, and shutdown used a temporary worker thread.

| Thread Category | Count / Limit | Configuration | Notes |
|---|---|---|---|
| muduo EventLoop (business main thread) | 1 | — | Owns ECS and serialized business-state changes; also drives the `muduo_grpc` client channels and server and the muduo_h2 etcd transport |
| gRPC clients and servers (`muduo_grpc`, h2c over nghttp2) | 0 extra | — | Run on the node's EventLoop; server limits (256 streams per connection, 1024 in-flight calls per node, at most 64 dispatches / 2ms per loop turn) are in `grpc-muduo-transport.md` §15 |
| 官方 gRPC C++ SDK | 0（不链接） | — | 官方运行时与桩生成插件已移除；依赖边界由源码及工程静态检查把关 |
| Kafka consumer / librdkafka | implementation-dependent | librdkafka internal | Background threads; measure rather than assume a fixed count |
| Async logging and lifecycle workers | component-dependent | component-specific | Includes the Agones lifecycle worker on scene / battle where enabled |
| OS / runtime threads | platform-dependent | — | For example Windows loader thread-pool workers, which may exit on their own; measure, do not assume |

Retired or inert settings:

- `GRPC_SERVER_MAX_POLLERS` is retired since batch 4: there are no sync
  pollers. Node ignores it and logs one `WARN` if it is set (deployment-script
  handling: `grpc-muduo-transport.md` §15.11).
- `GRPC_MAX_THREADS` configured the `ResourceQuota` of the official client
  channels in `GrpcChannelCache`, which batch 3 deleted; nothing reads it now.
- `GRPC_THREAD_POOL_RESERVE_THREADS` 和 `GRPC_THREAD_POOL_MAX_THREADS` 从来不是
  gRPC v1.83 内置 EventEngine 的公开控制项；官方 SDK 移除后节点没有对应配置入口。
  以上变量都不能用于容量规划。

### Gate Node

Gate keeps business-state ownership separate from transport work. Since batch 3
its gRPC client channels (router forwarding and the other generated clients)
are `muduo_grpc::Channel` instances on the gate EventLoop.
创建通道不依赖官方 gRPC SDK；Kafka 与日志组件仍会增加自己的后台线程。

### Scene and Battle Nodes

Scene owns ECS and `World::Update` on one muduo EventLoop. Scene and battle also
host the control-plane gRPC server, which since batch 4 is a
`muduo_grpc::Server` on that same EventLoop: connections are accepted, HTTP/2
is decoded, and calls are dispatched to the generated handlers directly on the
loop. There are no sync pollers and no `runInLoop` plus promise/future hand-off;
waits such as the Agones allocation permit continue asynchronously on the loop
(`AcquireAllocationPermitAsync`). Still, “Scene logic is single-threaded” must
never be read as “the Scene process has only one thread”: Kafka, logging, and
the Agones lifecycle worker have their own threads.

### gRPC Server Shutdown

Shutdown also runs entirely on the EventLoop (`grpc-muduo-transport.md` §15.10).
There is no shutdown worker thread, and `~Node` no longer joins one.
`BeginShutdown` stops listening, sends a two-phase GOAWAY, and keeps serving the
calls already received. When the 2s drain timeout expires, calls whose business
part has not started are refused (`REFUSED_STREAM`) and started calls complete
with `UNAVAILABLE "server shutting down"`; the remaining connections get a 1s
close grace, so the drain ends after about 3s at most. The log shows
`gRPC server draining` and then `gRPC server drained. drain_ms=`.

Handlers are synchronous on the loop, so a handler that is already running
finishes before any drain timer can fire; the drain bound applies to calls that
are queued or waiting asynchronously (for example on an Agones permit).

---

## Go Services — Goroutine-Based

### General Pattern

- All Go services use go-zero framework; each gRPC/HTTP request runs in its own goroutine
- Goroutine scheduling bound by `GOMAXPROCS` (defaults to `runtime.NumCPU()`)
- No explicit goroutine pool limits configured currently

### db Service — Kafka Partition Workers

- **File**: `go/db/internal/kafka/key_ordered_consumer.go`
- 1 worker goroutine per Kafka partition (`Config.Kafka.PartitionCnt`)
- Each worker has task channel buffer of 1000
- This is the only Go service with structured concurrency control

### login Service

- etcd KeepAlive goroutine (1 per registered node)
- NodeWatcher goroutine (1) for etcd-based service discovery
- TaskManager goroutine (1) for expired batch cleanup

### Other Go Services (scene_manager, data_service, player_locator)

- Standard go-zero request-per-goroutine model
- Kafka producers use fire-and-forget (segmentio/kafka-go); minimal extra goroutines

---

## Key Design Points

1. **C++ monitoring is passive** — logs and warns but does not kill or cap threads.
2. **Business single-threading is an ownership rule** — it does not describe the total process thread count.
3. **gRPC 协议收发不新增线程** — `muduo_grpc` 客户端与服务端跑在节点 EventLoop 上，官方 gRPC C++ SDK 已退出构建。
4. **依赖边界由源码与工程检查把关** — 检查全部工程配置、生成产物与 Linux 导出入口；ThreadMonitor 继续负责实际线程观测。
5. **`GRPC_SERVER_MAX_POLLERS` and `GRPC_MAX_THREADS` are retired** — the sync server pollers and client `ResourceQuota` they configured no longer exist.
6. **Go services have no goroutine hard limit** — they rely on scheduling via `GOMAXPROCS`; `runtime.SetMaxThreads()` concerns OS threads, not goroutine count.
7. **Kafka internal threads are implementation-managed** — production baselines must come from ThreadMonitor measurements.
8. **K8s cgroup CPU/memory limits do not directly cap thread count** — use monitoring and component-specific supported controls rather than assuming an indirect hard limit.

---

## File Reference

| File | Purpose |
|------|---------|
| `cpp/libs/engine/core/node/system/node/thread_observability.h` | ThreadMonitor 类、环境变量解析、`RegisterThreadObservability()` |
| `cpp/libs/engine/core/node/system/node/node.cpp` | Registers ThreadMonitor; starts the `muduo_grpc` server and runs its in-loop shutdown drain |
| `cpp/libs/engine/muduo_grpc/` | muduo-driven gRPC client channels and server (no threads) |
| `tools/scripts/tests/test_cpp_grpc_dependency_boundary.py` | 官方 SDK 的源码、生成产物与全配置工程依赖边界检查 |
| `cpp/tests/rpc_controller_test/grpc_client_local_reject_test.cpp` | 生成客户端本地拒绝与异步完成回归 |
| `cpp/tests/rpc_controller_test/grpc_client_loopback_test.cpp` | 生成客户端经 muduo 通道的真实回环回归 |
| `go/db/internal/kafka/key_ordered_consumer.go` | Kafka partition worker pool (structured goroutine control) |
