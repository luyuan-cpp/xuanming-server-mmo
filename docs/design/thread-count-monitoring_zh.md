# 每进程线程数监控与控制

## 概述

每个 C++ 节点自动运行一个 **ThreadMonitor**，定期采样进程线程数，并在检测到持续增长时输出警告日志。Go 服务依赖 goroutine 调度和按分区的 worker 池进行并发控制。

单个 muduo EventLoop 表示**业务状态由单线程持有**，不表示整个进程只有一条
线程。gRPC、Kafka、日志和 lifecycle 组件都有后台线程。

---

## C++ 节点 — ThreadMonitor（自动启用，所有节点）

### 实现
- **文件**: `cpp/libs/engine/core/node/system/node/thread_observability.h`
- **注册**: Node 初始化时调用 `RegisterThreadObservability()` — 所有节点默认启用
- **机制**: 在主 EventLoop 上设置定时器，每 N 秒采样线程数
  - Windows: `CreateToolhelp32Snapshot` + `Thread32First/Next`
  - Linux: `readdir("/proc/self/task")`
- **行为**: 仅被动监控 — 输出警告日志，**不会**强制限制或终止线程

### 环境变量

| 环境变量 | 默认值 | 说明 |
|---------|---------|-------------|
| `NODE_THREAD_MONITOR_ENABLED` | `1`（开启） | 设为 `0` / `false` / `off` 以禁用 |
| `NODE_THREAD_MONITOR_SAMPLE_INTERVAL_SECONDS` | `10` | 采样间隔（秒） |
| `NODE_THREAD_MONITOR_STABLE_WINDOW_SECONDS` | `60` | 等待此时长后设定基线值 |
| `NODE_THREAD_MONITOR_GROWTH_WARN_CONSECUTIVE_SAMPLES` | `6` | 连续增长采样次数达到此值后触发警告 |
| `NODE_THREAD_MONITOR_GROWTH_WARN_ABSOLUTE_INCREASE` | `16` | 基线以上的最小绝对增量才触发警告 |

### 日志输出示例

```
[INFO]  Gate thread metrics: total_threads=12, delta_threads=0, peak_threads=14, uptime_sec=120
[INFO]  Gate thread baseline (stable window reached): baseline_threads=12, ...
[WARN]  Gate thread growth warning: total_threads=30, increase_from_baseline=+18, consecutive_growth_samples=7, ...
```

---

## C++ 运行时线程构成

不能再使用“进程预期 5–14 条线程”一类固定区间。仅默认 gRPC EventEngine
在 16 核及以上主机上就会 eager 启动 17 条线程，而且任务积压时还可以扩展。

| 线程类别 | 数量/上限 | 配置方式 | 备注 |
|---|---|---|---|
| muduo EventLoop（业务主线程） | 1 | — | 持有 ECS，并串行执行所有业务状态变更 |
| gRPC v1.83 默认 EventEngine reserve | `Clamp(gpr_cpu_num_cores(), 4, 16)` 条 worker + 1 条 Lifeguard | 无公开 reserve/max 环境变量 | eager 启动；积压时可扩展；16 不是硬上限 |
| gRPC sync server poller（注册了服务端的节点） | 最少 1，当前默认最大 8 | `GRPC_SERVER_MAX_POLLERS` | 与 EventEngine 线程池相互独立 |
| client channel 的 `ResourceQuota` 线程设置 | 项目默认 2 | `GRPC_MAX_THREADS` | 只作用于 `GrpcChannelCache` 附加的 quota；不限制 EventEngine、服务端 poller 或进程总线程数 |
| Kafka consumer / librdkafka | 由实现决定 | librdkafka 内部管理 | 后台线程；应实测，不能假定固定数量 |
| 异步日志和 lifecycle worker | 由组件决定 | 组件各自配置 | 包括启用时的 Scene Agones lifecycle 工作线程 |
| gRPC 关机 worker | 1，仅关机期间 | — | 临时且必须可 join；稳态不存在 |

`GRPC_THREAD_POOL_RESERVE_THREADS` 和 `GRPC_THREAD_POOL_MAX_THREADS` 不是
gRPC v1.83 内置 EventEngine 的公开控制项。项目或 debug 配置中遗留的这些
同名变量目前必须视为无效，不能用于容量规划。

### Gate 节点

Gate 的业务状态归属与 transport 工作相互分离。创建 gRPC client channel
后，进程还会初始化 gRPC runtime 和默认 EventEngine；Kafka 与日志组件也会
增加自己的后台线程。

### Scene 节点

Scene 的 ECS 和 `World::Update` 由一条 muduo EventLoop 持有，但 Scene 同时
托管 control-plane gRPC sync server。handler 先进入 sync poller，再通过
`runInLoop` + promise/future 把业务部分交给 EventLoop。因此“Scene 逻辑单线程”
绝不能解释为“Scene 进程只有一条线程”。

### gRPC 关机

当 sync handler 正在等待 `runInLoop` 任务时，EventLoop 不得同步调用
`grpc::Server::Shutdown()`。Shutdown 应由临时 worker 执行，EventLoop 继续
完成已经排队的 handler；`Shutdown()` 返回后，再把收尾投递回 EventLoop，
由 EventLoop join 已结束的 worker 并拆除其余资源。

关机 deadline 是 transport grace deadline。它可以取消 pending call，但不会
强制终止已经开始运行的 C++ sync handler，因此不是 handler 时长或整个进程
关机时长的硬上限。

---

## Go 服务 — 基于 Goroutine

### 通用模式

- 所有 Go 服务使用 go-zero 框架；每个 gRPC/HTTP 请求在独立的 goroutine 中运行
- Goroutine 调度受 `GOMAXPROCS` 限制（默认为 `runtime.NumCPU()`）
- 当前未配置显式的 goroutine 池上限

### db 服务 — Kafka 分区 Worker

- **文件**: `go/db/internal/kafka/key_ordered_consumer.go`
- 每个 Kafka 分区 1 个 worker goroutine（`Config.Kafka.PartitionCnt`）
- 每个 worker 的任务通道缓冲区大小为 1000
- 这是唯一具有结构化并发控制的 Go 服务

### login 服务

- etcd KeepAlive goroutine（每个注册节点 1 个）
- NodeWatcher goroutine（1 个）用于基于 etcd 的服务发现
- TaskManager goroutine（1 个）用于过期批次清理

### 其他 Go 服务（scene_manager、data_service、player_locator）

- 标准的 go-zero 每请求一个 goroutine 模型
- Kafka 生产者使用 fire-and-forget（segmentio/kafka-go）；额外 goroutine 极少

---

## 关键设计要点

1. **C++ 监控是被动的** — 仅记录日志和发出警告，不会终止或限制线程。
2. **业务单线程是状态所有权规则** — 它不描述进程总线程数。
3. **EventEngine 没有受支持的 reserve/max 环境变量** — eager reserve 由 CPU 数决定，线程池还能扩展。
4. **`GRPC_SERVER_MAX_POLLERS` 当前默认 8** — 它限制 sync server poller，不限制 EventEngine worker。
5. **`GRPC_MAX_THREADS` 是局部 ResourceQuota 配置** — 它不是进程级或 EventEngine 的硬上限。
6. **Go 服务没有 goroutine 硬上限** — 依赖 `GOMAXPROCS` 调度；`runtime.SetMaxThreads()` 管的是 OS 线程，不是 goroutine 数量。
7. **Kafka 内部线程由实现管理** — 生产基线必须来自 ThreadMonitor 实测。
8. **K8s cgroup CPU/内存限制不会直接限制线程数** — 应使用监控和组件公开支持的控制项，不能把间接资源限制当成线程硬上限。

---

## 文件索引

| 文件 | 用途 |
|------|---------|
| `cpp/libs/engine/core/node/system/node/thread_observability.h` | ThreadMonitor 类、环境变量解析、`RegisterThreadObservability()` |
| `cpp/libs/engine/core/node/system/node/node.cpp` | 注册 ThreadMonitor；配置 sync server poller 和关机生命周期 |
| `cpp/libs/engine/core/node/system/grpc_channel_cache.h` | client channel ResourceQuota 配置（`GRPC_MAX_THREADS`） |
| `cpp/nodes/gate/gate.vcxproj` | 含遗留的 EventEngine 风格 debug 变量；它们不会配置 gRPC v1.83 内置线程池 |
| `go/db/internal/kafka/key_ordered_consumer.go` | Kafka 分区 worker 池（结构化 goroutine 控制） |
