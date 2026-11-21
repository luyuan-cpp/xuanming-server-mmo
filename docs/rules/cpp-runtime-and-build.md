# C++ 运行时规范、通信细则与构建命令

> 对应根索引 [AGENTS.md §13](../../AGENTS.md#13-架构构建与编码细则)。本文件承接原 `.github/copilot-instructions.md` 中的 C++ 编码门禁、运行时机制、通信优化与构建测试命令。

## 1. 基础设施与项目细则

- **尽可能避免使用 `static` 存储**：像 gRPC channel 缓存这类有状态组件，应由 `Node` 持有，而不是用静态存储实现（新起线程约束见 [naming-and-boundaries.md](naming-and-boundaries.md#硬禁令)）。
- **Kubernetes 外部 Gate 暴露建议**：托管云 K8s 通常优先使用 `LoadBalancer`；自建 / 裸金属 K8s 通常优先在外部四层负载均衡器后使用 `NodePort`。当集群缺乏成熟的 LB 实现时，不要将 `LoadBalancer` 作为一刀切的默认方案推荐。

## 2. C++ 源文件编码（MSVC `/utf-8`）

- **权威机制是 `/utf-8` 编译开关，而不是 BOM**。`/utf-8` 会让 MSVC 无论系统代码页是什么都按 UTF-8 读取源文件，因此设置了该开关的项目完全不需要 BOM。
- 每个 `.vcxproj` 都必须在每个 `ItemDefinitionGroup/ClCompile` 的 `<AdditionalOptions>` 中传入 `/utf-8`。不要手动给由脚本生成 `CMakeLists.txt` 的文件添加 BOM。
- **字符串字面量**优先使用 ASCII（用 `--` 代替 `—`，用 `->` 代替 `→`）：这些内容会进入运行时输出和日志流水线，避免运行期代码页问题。
- Linux / CMake 构建下 GCC 默认假定源码为 UTF-8。

## 3. C++ 语法门禁与生命周期细则

### MSVC Lambda 捕获 `entt::entity`（C3495）
- MSVC（`/std:c++latest`）在 lambda 中通过简单捕获 `[entity]` 捕获 `entt::entity`（`enum class`）或结构化绑定变量时会触发 C3495 报错或不稳定。
- **规避方法**：一律使用初始化捕获语法：`[entity = entity]()`，不要写 `[entity]()`。

### C++ 类成员不放裸指针（no-raw-pointer-member 门禁）
- MSBuild 编译每个 C++ 工程前先跑 `tools/scripts/check_no_raw_pointer_member.ps1`（`Directory.Build.targets` 的 `NoRawPointerMemberCheck`），规则在 `cpp/plugin/no_raw_ptr_matcher.cq`：类 / 结构体 / 联合体的非静态成员不得是指针类型。`const char *` 算，函数里的局部结构体（含测试夹具）也算；静态成员、指针数组、`std::vector<T*>` 这类容器、函数参数与局部变量不算。`third_party`、`generated`、`cpp/libs/engine/muduo_windows` 不查。
- 门禁逐个翻译单元检查，首个违例就让整个工程失败：后面的单元不再检查，这个工程的用例一条都跑不到。
- 写法：名字用 `std::string_view`；非空借用用引用成员；可空借用用 `std::optional<std::reference_wrapper<T>>`；拥有用智能指针。`std::string + std::string_view` 要到 C++26 才有，拼接时包一层 `std::string(...)`。
- 交付前的文本预检：仓库根 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`（只读、几秒、Linux 也能跑，CI 的 `test_cpp*` 一步会跑它）。它靠文本识别，结论以门禁为准；误报或漏报时改识别规则并补它的自测用例。

### `TimerTaskComp` 生命周期与 `EventLoop` 析构顺序
- `TimerTaskComp::Cancel()` 在调用 `loop->cancel()` 之前必须判空 `GetThreadEventLoop()`。`EventLoop` 析构函数会在 entt registry 清理并销毁实体组件**之前**将 `t_loopInThisThread` 置为 `NULL`。
- **规则**：当 `loop` 已经销毁时，`Cancel()` 必须跳过 `loop->cancel()` 以防止空指针崩溃；构造函数不应调用 `Cancel()`——`timerId` 已经默认初始化。
- 定时器闭包只捕获内部 State 的 `weak_ptr`；移动转移调度状态，复制得到空定时器。包装组件的 `in_place_delete` 仅为指针稳定性选择，不再是定时器正确性的前提。业务回调若捕获外层 owner 的 `this`，该 owner 必须显式禁止移动；详情见 [ecs-component-access-rules.md](../design/ecs-component-access-rules.md)。
- **测试模式**：在 `loop.loop()` 返回后、且 `loop` 仍存活时调用 `tlsEcs.actorRegistry.clear()`，以干净地取消所有实体定时器。使用 `EventLoop` 的测试必须使用 `loop.runAfter(duration, quit)`，不要使用无限 `while(true)` 循环。

## 4. 通信、广播与流量统计

### 流量统计系统
- **C++**：`TrafficStatsCollector` 单例位于 `cpp/libs/engine/core/network/traffic_statistics.h/.cpp`。按消息维护原子计数器（收/发次数、字节数、最大包大小）。在 `Node` 构造函数中通过 `RegisterTrafficStatsReporter()` 自动注册。`GameChannel` 中旧的逐消息 `LogMessageStatistics()` 已随 `RpcChannel` 重写移除；RPC0 调用结果由 `RpcChannelStats`（`network/rpc_channel_stats.h`，每 30 秒输出 `[RpcChannel] summary` 行）统计。
- **Go**：`grpcstats.Collector` 位于 `go/shared/grpcstats/collector.go`。通过 gRPC `UnaryServerInterceptor` 统计每个方法的调用次数、请求/响应字节数、平均/最大延迟。已接入所有 Go 服务。
- **C++ 开关**：环境变量 `NODE_TRAFFIC_STATS_ENABLED=1`，可选 `NODE_TRAFFIC_STATS_AUTO_DISABLE_MINUTES`、`NODE_TRAFFIC_STATS_INTERVAL_SECONDS`（默认 30 秒）。
- **Go 开关**：环境变量 `GRPC_TRAFFIC_STATS_ENABLED=1`，可选 `GRPC_TRAFFIC_STATS_AUTO_DISABLE_MINUTES`、`GRPC_TRAFFIC_STATS_INTERVAL_SECONDS`（默认 30 秒）。
- **生产环境安全**：仅使用原子计数器、定期汇总输出（非逐消息打日志）、支持超时自动关闭。

### 广播消息优化（三级体系）
- **三级体系**：`BroadcastToPlayers`（AOI 子集）、`BroadcastToScene`（全场景）、`BroadcastToAll`（全服）。
- **Bitmap 编码**：当 `count >= 32 && bitmapBytes + 6 < count * 3` 时，`BroadcastToPlayers` 自动选用 bitmap。Session ID 编码为 `session_bitmap_base`（`uint32`）+ `session_bitmap`（`bytes`）。750 名玩家：约 2250 字节 → 约 155 字节（减少 93%）。
- **Session ID 位布局**：`uint32`，高 15 位 = gate `node_id`，低 17 位 = 序列号。同一 Gate 上的所有会话共享高位 → bitmap 跨度受限。
- **C++ API**（`client_message_sender.h/cpp`）：`BroadcastToPlayerClients()`、`BroadcastToSceneClients()`、`BroadcastToKnownGateClients()` 均经 `RpcSession::SendRequest` 走 muduo TCP RPC；玩家子集自动编码 bitmap，场景广播按 `scene_id` 筛选，后者只覆盖当前线程注册表中拥有 `RpcSession` 的 Gate 上的全部客户端会话。
- **Go API**（`go/shared/kafkautil/gate_push.go`）：`BroadcastToPlayers()`、`BroadcastToScene()`、`BroadcastToGateClients()` — 均经 Kafka 投递；最后一个入口只覆盖调用方传入的 Gate 集合。`EncodeBroadcastTargets()` 将每个会话与目标玩家配对，并自动选择 bitmap；每个生产者必须填写目标玩家（`PushToPlayerEvent.player_id`、`BroadcastToPlayersEvent.player_list`）。
- **Gate 处理端**：muduo RPC 在 `gate_service_handler.cpp`，Kafka 在 `gate_event_handler.cpp`；两者都委托共享投递模块 `gate_push`（`cpp/nodes/gate/handler/gate_push_delivery.{h,cpp}`：身份栅栏、广播编码一次）。
- **Proto**：`proto/gate/gate_service.proto`（C++ 节点 RPC）、`proto/contracts/kafka/gate_event.proto`（Kafka 命令）。
- **设计文档**：[broadcast-message-size-analysis.md](../design/broadcast-message-size-analysis.md)。

### gRPC 服务端与线程（C++ 节点，muduo 驱动）
- scene / battle 内嵌 gRPC 服务端承接控制面 RPC（`CreateScene`、`DestroyScene`、`ReleasePlayer`、资产 RPC、`CreateBattle` 等）。第 4 批起它是 `muduo_grpc::Server`（muduo `Acceptor` + nghttp2，设计见 [grpc-muduo-transport.md](../design/grpc-muduo-transport.md) §15）：接入、HTTP/2、派发、截止、排空全部跑在节点 EventLoop 上，**不新增任何线程**；不再构造 `grpc::Server` / `ServerBuilder` / `ServerContext` / `CompletionQueue`。每个节点一个 gRPC 端口（TCP+30000），scene 只注册 `SceneNodeGrpc`（9 个方法），battle 只注册 `BattleNode`（6 个方法）。
- 生成的 `<Svc>Impl : muduo_grpc::Service` 在 loop 线程上被调用：`grpc.<M>.guard` 守护段要么同步 `Reject`，要么发起异步步骤（如 `AcquireAllocationPermitAsync`）后再 `Proceed()`；`Handle<M>` 同步执行。handler 里**不得阻塞**（不用 `promise/future`，不调 `AcquireAllocationPermitBlocking`），否则卡住整个逻辑帧。异步续跑只捕获调用句柄（`shared_ptr<GrpcUnaryCall>`）或 weak，不捕获 `this`。「业务逻辑单线程」仍然不等于进程只有一个线程（Kafka、Redis、日志等仍有自己的线程）。
- 第 3 批起生成的 C++ gRPC 客户端也走 `muduo_grpc`（节点实体上的 `MuduoGrpcChannelPtr`），节点不再拉起官方 gRPC 运行时（`grpc_init`、EventEngine、poller、Lifeguard）。保持这一点：进程里任何路径再构造官方 grpc 对象，这些线程就会回来。2026-10-26 起继续移除官方 SDK：不保留初始化探针、官方链接项、`grpc_cpp_plugin` 或 `*.grpc.pb.*`；Protobuf、Abseil、GoogleTest、BoringSSL 独立固定提交，版本与迁移前相同。依赖边界及验证以主传输设计的后续决策为准。
- `GRPC_SERVER_MAX_POLLERS` **已退役**：进程忽略它，设了只打一行 WARN；部署脚本不再注入。并发上限由服务端内建：每连接 `MAX_CONCURRENT_STREAMS=256`、全节点在途 1024（超限在业务之前回 `RESOURCE_EXHAUSTED`）、派发泵每轮 ≤64 个且 ≤2ms。
- `GRPC_THREAD_POOL_RESERVE_THREADS` / `GRPC_THREAD_POOL_MAX_THREADS` / `GRPC_MAX_THREADS` 对节点线程数都没有作用（`GrpcChannelCache` 已随第 3 批删除），不要再用它们「调线程」。线程数核对按 `grpc-muduo-transport.md` §15.14 第 8 步（线程基线 + Linux `/proc/<pid>/task/*/comm`）。

### gRPC 服务端关机
- 关机全部在 loop 内完成，没有关机线程、没有 join（旧的 `grpcShutdownThread_` 已删）：`ShutdownInLoop` 依次 StopConsumers → beforeShutdown hook → 看门狗 → `grpcServer_->BeginShutdown({2s, 1s, 1s}, onDrained)` → MaybeFinalize（`grpc-muduo-transport.md` §15.10）。
- `BeginShutdown`：置排空标志（此后 `ServerDraining()` 为真）→ 停监听（Acceptor 经 `queueInLoop` 销毁）→ 每条连接两段 GOAWAY（shutdown notice + PING，收到 PING ACK 或满 1s 再发最终 GOAWAY）。最终 GOAWAY 之前收到的调用照常处理；2s 排空到期时，未开始的回 `RST_STREAM(REFUSED_STREAM)`，已开始的回 `UNAVAILABLE "server shutting down"`；之后写半关，宽限 1s 再 forceClose，总上限约 3s。
- `onDrained` 恰好一次、经 `queueInLoop` 投递；`Node::OnGrpcServerDrained` 在这个排队任务里 reset Server，早于两跳 `quit()`。`muduo_grpc::Server` 与 `Acceptor` 只能在 `queueInLoop` 任务里或 loop 外析构（I/O 事件处理期间 `removeChannel` 有断言）。
- handler 不能同步等待排空：排空本身要靠 loop 推进。停机期间要拒绝的变更型 RPC 在守护段里查 `ServerDraining()`（scene `CreateScene` 回 `UNAVAILABLE "scene_node_shutting_down"`）或用自己的准入闸（battle `CreateBattle` 回 `battle_not_allocatable`）。
- 截止由服务端执行：投递前或等许可时到期回 `DEADLINE_EXCEEDED`、不进业务；业务一旦开始不会被打断，迟到的完成被丢弃并计数。恰好一次的唯一裁决点是 `Server::CompleteCall`。

### Scene Manager 与 Gate 通信
- SceneManager 为 Go/go-zero 服务，配置入口 `go/scene_manager/etc/scene_manager_service.yaml`；Gate 的 Kafka 消费链已实现，不是待接线功能。
- `RoutePlayer`、`BindSession`、`KickPlayer` 等控制命令经 Kafka 共享分区节点命令 topic 投递。Gate 与本 zone Scene 的玩家热路径走 muduo TCP RPC；Go 控制面仍走 gRPC。
- 节点类型事实源在 `proto/common/base/node.proto`，`SceneManagerNodeService = 25`；新改动统一使用 Scene Node 名称。
- 完整架构与路由模式默认值见 [ARCH.md](../design/ARCH.md)、[client-rpc-router.md](../design/client-rpc-router.md)，共享分区寻址见 [control-plane-topic-partitioning-20260908.md](../design/control-plane-topic-partitioning-20260908.md)。

## 5. 构建、测试与本地开发命令

### C++（核心游戏服务端）
- **构建（Windows 解决方案）**：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`（按根规范串行）
- **使用 CMake 构建单个节点（以 scene 为例）**：`cd cpp\nodes\scene && cmake -S . -B build && cmake --build build --config Debug`
- **运行 C++ 测试**：测试工程位于 `cpp\tests\` 下（可在 Visual Studio 测试资源管理器中运行，或直接执行构建出的测试可执行文件）。
- **运行单个 C++ 测试**：`build\cpp\tests\bag_test.exe --gtest_filter=BagTest.AddNewGridItem`
- **C++ 代码检查（Lint）**：配合 `cpp\.clang-tidy` 使用 `clang-tidy`（示例：`clang-tidy <file.cpp> --config-file=cpp\.clang-tidy`）

### Go（微服务）
- **协议重生成**：在仓库根按 [proto-and-tables.md](proto-and-tables.md) 运行 `dev_tools.ps1 -Command proto-gen-run`；不写客户端仓时使用 `proto_gen.no-unity.yaml`。`cd go && build.bat` 仅处理 db/login 包装与导入修正。
- **运行 login 服务**：`cd go\login && go run login.go`
- **运行全部 login 测试**：`cd go\login && go test ./...`
- **运行单个 Go 测试**：`cd go\login && go test ./internal/logic/pkg/sessionmanager -run '^TestMintLoginGeneration_KeyName$'`
- **整理依赖**：`cd go\login && go mod tidy`

### Java（网关 / Web）
- **构建**：`cd java\gateway_node && mvn clean install`
- **运行测试**：`cd java\gateway_node && mvn test`
- **运行单个 Java 测试**：`cd java\gateway_node && mvn -Dtest=GatewayNodeApplicationTests#contextLoads test`

### Go 服务（本地开发与镜像）
- **构建全部二进制**：`pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build`
- **构建指定二进制**：`pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build -GoServices login,db`
- **全部启动 / 停止 / 状态 / 列表**：`pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start`（或 `go-svc-stop` / `go-svc-status` / `go-svc-list`）
- **构建 / 推送全部镜像**：`pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build-images -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1`（推送为 `go-svc-push-images`）

### C++ 节点与统一本地开发
- **C++ 节点启动 / 停止 / 状态**：`pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-start`（按数量加 `-GateCount 2 -SceneCount 4`；停止为 `cpp-node-stop`；状态为 `cpp-node-status`）
- **统一本地开发（C++ + Go 一键操作）**：`pwsh -File tools/scripts/dev_tools.ps1 -Command dev-start`（停止为 `dev-stop`；状态为 `dev-status`）
