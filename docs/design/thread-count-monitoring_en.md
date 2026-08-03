# Thread Count Monitoring & Control per Process

## Overview

Every C++ node automatically runs a **ThreadMonitor** that periodically samples the process thread count and logs warnings when sustained growth is detected. Go services rely on goroutine scheduling and per-partition worker pools for concurrency control.

A single muduo EventLoop means **single-threaded ownership of business state**,
not a single-threaded process. gRPC, Kafka, logging, and lifecycle components
have background threads.

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

---

## C++ Runtime Thread Composition

A fixed process-wide range such as “5–14 threads” is not valid. The default
gRPC EventEngine alone can eagerly start 17 threads on a host with 16 or more
logical CPUs, and its pool may grow when work backs up.

| Thread Category | Count / Limit | Configuration | Notes |
|---|---|---|---|
| muduo EventLoop (business main thread) | 1 | — | Owns ECS and serialized business-state changes |
| gRPC v1.83 default EventEngine reserve | `Clamp(gpr_cpu_num_cores(), 4, 16)` workers + 1 Lifeguard | No public reserve/max env | Eagerly started; elastic under backlog; 16 is not a hard maximum |
| gRPC sync server pollers (nodes that register a server) | minimum 1, configured maximum 8 by default | `GRPC_SERVER_MAX_POLLERS` | Separate from the EventEngine pool |
| Client channel `ResourceQuota` thread setting | 2 by project default | `GRPC_MAX_THREADS` | Applies to the quota attached by `GrpcChannelCache`; does not cap EventEngine, server pollers, or total process threads |
| Kafka consumer / librdkafka | implementation-dependent | librdkafka internal | Background threads; measure rather than assume a fixed count |
| Async logging and lifecycle workers | component-dependent | component-specific | Includes Scene Agones lifecycle work where enabled |
| gRPC shutdown worker | 1, shutdown only | — | Temporary joinable worker; absent during steady state |

`GRPC_THREAD_POOL_RESERVE_THREADS` and `GRPC_THREAD_POOL_MAX_THREADS` are not
public controls for gRPC v1.83's built-in EventEngine. Legacy project/debug
settings with those names must be treated as inert and must not be used for
capacity planning.

### Gate Node

Gate keeps business-state ownership separate from transport work. When it
creates gRPC client channels, the process also initializes gRPC runtime and the
default EventEngine. Kafka and logging add their own background threads.

### Scene Node

Scene owns ECS and `World::Update` on one muduo EventLoop, but it also hosts the
Scene control-plane gRPC sync server. Its handlers enter on sync pollers and use
`runInLoop` plus promise/future to execute their business portion on the
EventLoop. Therefore “Scene logic is single-threaded” must never be interpreted
as “the Scene process has only one thread.”

### gRPC Shutdown

The EventLoop must not synchronously call `grpc::Server::Shutdown()` while a
sync handler is waiting for `runInLoop` work. Shutdown runs on the temporary
worker while the EventLoop continues servicing queued handlers. Once
`Shutdown()` returns, finalization is queued back to the EventLoop, which joins
the completed worker and tears down the remaining resources.

The shutdown deadline is a transport grace deadline. It may cancel pending
calls, but it does not forcibly terminate an already-running C++ sync handler,
so it is not a hard handler-duration or process-shutdown bound.

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
3. **The EventEngine has no supported reserve/max env control** — its eager reserve is CPU-based and its pool can expand.
4. **`GRPC_SERVER_MAX_POLLERS` defaults to 8** — it controls sync server pollers, not EventEngine workers.
5. **`GRPC_MAX_THREADS` is scoped ResourceQuota configuration** — it is not a process-wide or EventEngine hard cap.
6. **Go services have no goroutine hard limit** — they rely on scheduling via `GOMAXPROCS`; `runtime.SetMaxThreads()` concerns OS threads, not goroutine count.
7. **Kafka internal threads are implementation-managed** — production baselines must come from ThreadMonitor measurements.
8. **K8s cgroup CPU/memory limits do not directly cap thread count** — use monitoring and component-specific supported controls rather than assuming an indirect hard limit.

---

## File Reference

| File | Purpose |
|------|---------|
| `cpp/libs/engine/core/node/system/node/thread_observability.h` | ThreadMonitor class, env var parsing, `RegisterThreadObservability()` |
| `cpp/libs/engine/core/node/system/node/node.cpp` | Registers ThreadMonitor; configures sync server pollers and shutdown lifecycle |
| `cpp/libs/engine/core/node/system/grpc_channel_cache.h` | Client channel ResourceQuota configuration (`GRPC_MAX_THREADS`) |
| `cpp/nodes/gate/gate.vcxproj` | Contains legacy EventEngine-like debug env names; they do not configure gRPC v1.83's built-in pool |
| `go/db/internal/kafka/key_ordered_consumer.go` | Kafka partition worker pool (structured goroutine control) |
