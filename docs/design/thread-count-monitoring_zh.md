# 每进程线程数监控与控制

## 概述

每个 C++ 节点自动运行一个 **ThreadMonitor**，定期采样进程线程数，并在检测到持续增长时输出警告日志。Go 服务依赖 goroutine 调度和按分区的 worker 池进行并发控制。

单个 muduo EventLoop 表示**业务状态由单线程持有**，不表示整个进程只有一条
线程。Kafka、日志和 lifecycle 组件都有后台线程。gRPC 没有：gRPC 迁到 muduo
驱动（第 3、4 批，`docs/design/grpc-muduo-transport.md` §14–§15）之后，生成的
gRPC 客户端与 scene / battle 的 gRPC 服务端都经 `muduo_grpc` 跑在节点自己的
EventLoop 上，不建线程。2026-10-26 起，C++ 工程不再链接官方 gRPC SDK；
Protobuf、Abseil、GoogleTest 与必要的 BoringSSL 各自独立管理，协议与 Go 服务保持兼容。

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

## C++ 运行时线程构成

仍然不能使用“进程预期 5–14 条线程”一类固定区间：librdkafka、日志和 lifecycle
worker 的线程数由实现决定，生产基线必须来自 ThreadMonitor 实测。第 3、4 批改变
的是 gRPC 不再贡献任何线程。此前仅默认 gRPC EventEngine 就会 eager 启动
`Clamp(gpr_cpu_num_cores(), 4, 16)` 条 worker 加 1 条 Lifeguard（16 核及以上主机
即 17 条），积压时还能扩展；带 gRPC 服务端的节点另有最多 8 条 sync poller；关机
还要起一条临时 worker。

| 线程类别 | 数量/上限 | 配置方式 | 备注 |
|---|---|---|---|
| muduo EventLoop（业务主线程） | 1 | — | 持有 ECS，并串行执行所有业务状态变更；同时驱动 `muduo_grpc` 客户端通道与服务端、muduo_h2 etcd 传输 |
| gRPC 客户端与服务端（`muduo_grpc`，基于 nghttp2 的 h2c） | 0（不另起） | — | 跑在节点 EventLoop 上；服务端上限（每连接 256 条流、全节点 1024 个在途调用、每轮 loop 至多派发 64 个 / 2ms）见 `grpc-muduo-transport.md` §15 |
| 官方 gRPC C++ SDK | 0（不链接） | — | 官方运行时与桩生成插件已移除；依赖边界由源码及工程静态检查把关 |
| Kafka consumer / librdkafka | 由实现决定 | librdkafka 内部管理 | 后台线程；应实测，不能假定固定数量 |
| 异步日志和 lifecycle worker | 由组件决定 | 组件各自配置 | 包括启用时 scene / battle 的 Agones lifecycle 工作线程 |
| 操作系统 / 运行库线程 | 由平台决定 | — | 例如 Windows 加载器线程池的 worker，可能自行退出；应实测，不能假定 |

已退役或无效的配置：

- `GRPC_SERVER_MAX_POLLERS` 第 4 批起退役：已经没有 sync poller。Node 忽略它，
  设了只打一次 `WARN`（部署脚本的处理见 `grpc-muduo-transport.md` §15.11）。
- `GRPC_MAX_THREADS` 配的是 `GrpcChannelCache` 里官方 client channel 的
  `ResourceQuota`，第 3 批已删掉 `GrpcChannelCache`，现在没有任何代码读它。
- `GRPC_THREAD_POOL_RESERVE_THREADS` 和 `GRPC_THREAD_POOL_MAX_THREADS` 从来不是
  gRPC v1.83 内置 EventEngine 的公开控制项；官方 SDK 移除后节点没有对应配置入口。
  以上变量都不能用于容量规划。

### Gate 节点

Gate 的业务状态归属与 transport 工作相互分离。第 3 批起，它的 gRPC 客户端
通道（路由转发及其他生成客户端）都是 gate EventLoop 上的 `muduo_grpc::Channel`，
创建通道不依赖官方 gRPC SDK；Kafka 与日志组件仍会增加自己的后台线程。

### Scene 与 Battle 节点

Scene 的 ECS 和 `World::Update` 由一条 muduo EventLoop 持有。Scene 与 battle
还托管 control-plane gRPC 服务端，第 4 批起它是同一条 EventLoop 上的
`muduo_grpc::Server`：接入连接、解 HTTP/2、把调用派发给生成的 handler，全部
直接在 loop 上完成。没有 sync poller，也没有 `runInLoop` + promise/future 的
交接；Agones 分配许可这类等待以异步方式回到 loop 继续
（`AcquireAllocationPermitAsync`）。即便如此，“Scene 逻辑单线程”仍绝不能解释为
“Scene 进程只有一条线程”：Kafka、日志和 Agones lifecycle worker 都有自己的线程。

### gRPC 服务端关机

关机同样全部在 EventLoop 上执行（`grpc-muduo-transport.md` §15.10），没有关机
worker 线程，`~Node` 也不再 join 任何线程。`BeginShutdown` 停止监听、发两段式
GOAWAY，并继续处理已经收到的调用。2s 排空到期时，业务尚未开始的调用被拒
（`REFUSED_STREAM`），已开始的以 `UNAVAILABLE "server shutting down"` 完成；剩余
连接再给 1s 关闭宽限，所以排空最多约 3s 结束。日志依次出现
`gRPC server draining` 与 `gRPC server drained. drain_ms=`。

handler 在 loop 上同步执行，已经在跑的 handler 一定先于任何排空定时器结束；
排空上限约束的是排队中或异步等待中（例如等 Agones 许可）的调用。

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
3. **gRPC 协议收发不新增线程** — `muduo_grpc` 客户端与服务端跑在节点 EventLoop 上，官方 gRPC C++ SDK 已退出构建。
4. **依赖边界由源码与工程检查把关** — 检查全部工程配置、生成产物与 Linux 导出入口；ThreadMonitor 继续负责实际线程观测。
5. **`GRPC_SERVER_MAX_POLLERS` 与 `GRPC_MAX_THREADS` 已退役** — 它们配置的 sync server poller 与 client `ResourceQuota` 都已不存在。
6. **Go 服务没有 goroutine 硬上限** — 依赖 `GOMAXPROCS` 调度；`runtime.SetMaxThreads()` 管的是 OS 线程，不是 goroutine 数量。
7. **Kafka 内部线程由实现管理** — 生产基线必须来自 ThreadMonitor 实测。
8. **K8s cgroup CPU/内存限制不会直接限制线程数** — 应使用监控和组件公开支持的控制项，不能把间接资源限制当成线程硬上限。

---

## 文件索引

| 文件 | 用途 |
|------|---------|
| `cpp/libs/engine/core/node/system/node/thread_observability.h` | ThreadMonitor 类、环境变量解析、`RegisterThreadObservability()` |
| `cpp/libs/engine/core/node/system/node/node.cpp` | 注册 ThreadMonitor；启动 `muduo_grpc` 服务端并在 loop 内执行关机排空 |
| `cpp/libs/engine/muduo_grpc/` | muduo 驱动的 gRPC 客户端通道与服务端（不建线程） |
| `tools/scripts/tests/test_cpp_grpc_dependency_boundary.py` | 官方 SDK 的源码、生成产物与全配置工程依赖边界检查 |
| `cpp/tests/rpc_controller_test/grpc_client_local_reject_test.cpp` | 生成客户端本地拒绝与异步完成回归 |
| `cpp/tests/rpc_controller_test/grpc_client_loopback_test.cpp` | 生成客户端经 muduo 通道的真实回环回归 |
| `go/db/internal/kafka/key_ordered_consumer.go` | Kafka 分区 worker 池（结构化 goroutine 控制） |
