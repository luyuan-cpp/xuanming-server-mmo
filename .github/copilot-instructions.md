# Copilot Instructions

## Project Guidelines
- Prefer clearer, verb-based RPC handler names and thin wrapper pattern: keep generated RPC method names but delegate work to well-named internal functions (e.g., ProcessClientPlayerMessage).
- Avoid static usage in code where possible. Prefer stateful components like gRPC channel cache to be owned by Node rather than implemented with static storage.
- Kubernetes external gate exposure guidance: managed cloud K8s should generally prefer `LoadBalancer`; self-hosted / bare metal K8s should generally prefer `NodePort` behind an external L4 load balancer. Do not recommend `LoadBalancer` as a one-size-fits-all default when the cluster lacks a mature LB implementation.

## C++ Source File Encoding (MSVC / Code Page 936)

**The authoritative mechanism is the `/utf-8` compiler switch, not the BOM.**
`/utf-8` makes MSVC read sources as UTF-8 regardless of the system codepage, so
a project that sets it needs no BOM at all. Measured state (2026-07-29): 27
`.cpp`/`.h` files under `cpp/` contain Chinese and have no BOM, and the full
`game.sln` Debug|x64 build is green — because the projects that compile them
pass `/utf-8`.

Rules, in priority order:

1. **Every `.vcxproj` MUST pass `/utf-8`** in `<AdditionalOptions>` for each
   `ItemDefinitionGroup/ClCompile`. This is the fix that scales; per-file BOMs
   are not.
   - 检查当前工程时逐个核对编译配置，不使用历史的“17/54 个工程”统计作为现状。
     PowerShell 可用 `rg --files-without-match '/utf-8' cpp -g '*.vcxproj' -g '!**/muduo_windows/**'` 初筛；文件中出现一次选项仍不代表每个配置都已覆盖。
     A non-ASCII source compiled by one of those projects **is** at risk on a
     CP936 machine: multi-byte UTF-8 (e.g. `→` = `E2 86 92`) is misread as GBK
     and corrupts the parser, with cascading errors on later lines.
2. **BOM is a belt-and-braces fallback**, useful for a brand-new standalone file
   or when you cannot confirm the owning project's flags. Adding one is harmless;
   relying on it *instead* of `/utf-8` is not.
   - Do NOT hand-add a BOM to a file whose `CMakeLists.txt` is generated — see
     the generated-output rule below; the same applies to encoding.
3. Prefer ASCII in **string literals** regardless (`--` over `—`, `->` over `→`):
   those cross into runtime output and log pipelines, where the codepage story
   starts over.
4. Linux/CMake builds are unaffected: GCC assumes UTF-8.

History: this section previously said "always add a UTF-8 BOM" and did not
mention `/utf-8` at all, which caused at least one agent to rewrite Chinese
comments as English to dodge a hazard the build already handled.

## ECS Component Access Rules (`get` vs `try_get` vs `get_or_emplace`)
- 修改或评审组件创建、读取、存盘与快照前，先读 [组件访问规则](../docs/design/ecs-component-access-rules.md)：持久数据在加载边界创建，按需状态在唯一写入口延迟创建，派生数据在实体创建时计算；读路径只读，缺失处理按类别执行。

## MSVC Lambda Capture with entt::entity
- MSVC (`/std:c++latest`) has a known issue (C3495) capturing `entt::entity` (enum class) variables in lambdas via simple capture `[entity]`.
- **Workaround**: Use init-capture syntax: `[entity = entity]()` instead of `[entity]()`.

## TimerTaskComp Lifecycle & EventLoop Destruction Order
- `TimerTaskComp::Cancel()` must null-check `GetThreadEventLoop()` before calling `loop->cancel()`. The EventLoop destructor sets `t_loopInThisThread = NULL` **before** entt registry cleanup destroys entity components.
- **Rule**: `Cancel()` must skip `loop->cancel()` when the loop is already gone to prevent nullptr crashes. 
- **Rule**: Constructors should not call `Cancel()` — `timerId` is already default-initialized.
- 定时器闭包只捕获内部 State 的 weak_ptr；移动转移调度状态，复制得到空定时器。包装组件的 `in_place_delete` 仅为指针稳定性选择，不再是定时器正确性的前提。业务回调若捕获外层 owner 的 `this`，该 owner 必须显式禁止移动；详情见 [组件访问规则](../docs/design/ecs-component-access-rules.md)。
- **Test pattern**: After `loop.loop()` returns, call `tlsEcs.actorRegistry.clear()` while the loop is still alive to cancel all entity timers cleanly. Tests using `EventLoop` must use `loop.runAfter(duration, quit)` instead of infinite `while(true)` loops.

## Reward System Design (Claimed-Status Ownership)
1. **Problem**: `RewardBitset` bit index was keyed by `reward_table_id`. Changing/deleting reward table entries would corrupt saved player claim data.
2. **Solution**: Each business system (quest, achievement, daily-login, etc.) owns its own bitset for claimed status. The bit index comes from that system's own table (e.g., `quest_table_id_bit_index.h`), **not** from the reward table.
   - Reward table ID becomes a pure data reference ("what items/currency to give"), not a persistence key.
   - Changing a quest's reward ID no longer affects claimed status.
3. **Bitset type**: Prefer `std::bitset` per system (compile-time size from table generation, cache-friendly, zero allocation). Switch to `boost::dynamic_bitset` or `std::vector<bool>` only if runtime hot-reload of tables without recompilation is required.
4. **`RewardClaimSystem` role**: Becomes a pure "dispatch reward items" utility. It no longer manages claimed state — each business system manages its own.

## Proto Field Type Constraints
- **Coordinates/Transform (double, NOT float)**: `Location`, `Rotation`, `Scale`, `Vector3`, `Velocity`, `Acceleration` x/y/z fields MUST stay `double` to match UE4 client-side precision. `float` loses accuracy at large world coordinates.
- **BaseAttributesComp (uint64, NOT uint32)**: `strength`, `stamina`, `health`, `mana`, `critchance`, `armor`, `resistance` MUST stay `uint64`. C++ combat code casts these to `double` for damage formulas; uint64 avoids narrowing and keeps headroom for late-game stat scaling.
- **身份字段保持 uint64**：`player_id`、`entity`、`scene_id`、`item_id`、`buff_id`、`skill_id`（运行时实例）、`tx_id`、`snapshot_id` 等保持 64 位。具体号源按契约判断：部分身份已用号段，客户端 actor 号是不透明运行时编号，不能按字段名推断 SnowFlake 布局或转换成 EnTT 句柄。
- **Unix timestamps (uint64/int64)**: `created_at`, `expires_at`, `castTime`, `previous_time`, `last_time`, `start` (TimeMeter/Cooldown) etc. MUST stay 64-bit (Unix ms exceeds uint32).
- **Currency (uint64)**: `CurrencyComp.values`, `owed`, `paid`, `balance_before/after` — keep uint64 for economy headroom.
- **Safe to use uint32**: `session_id`, `session_version`, `attack_power`, `defense_power`, `max_health`, `skill_table_id` (table lookup key), `current_frame` (tick counter).
- **Safe to use float**: `ViewRadius.radius` (AOI range).

## SnowFlake Node ID Isolation Constraint
- SnowFlake uses a 17-bit node field (`kNodeBits=17`, max 131071). The node ID comes from etcd-allocated `node_id`.
- Node IDs are allocated **per (zone, node_type)**: different node types in the same zone can have the same `node_id` value.
- **Invariant**: Each SnowFlake-generated ID type must be produced by only ONE node type. If Scene nodes generate item GUIDs and Gate nodes generate session IDs, those are separate ID spaces and safe.
- **NEVER** introduce a SnowFlake-generated ID that is produced by multiple node types — the 17-bit worker field will collide since different node types share the same `node_id` range.
- If a globally-unique cross-node-type ID is ever needed, either encode `node_type` into the worker field (e.g. 5-bit type + 12-bit node) or switch node ID allocation to a single global namespace.

## Kafka Zombie Message Protection
- 节点命令现用共享分区 topic：`gate-cmd_g<N>`、`scene-cmd_g<N>`，按目标 node_id 定分区。`{type}-{node_id}` 仅是迁移期兼容路径，是否继续消费由 `disable_legacy_per_node_topic` 控制；Centre 已退役。
- 契约在 `proto/contracts/kafka/`，GateCommand 位于 `gate_event.proto`。发送方通过共享寻址工具选择 topic 和分区，不自行拼旧 topic 名。
- 每条定向节点命令必须携带目标节点的 `target_instance_id`；共享消费者会丢弃实例 UUID 不匹配的消息，空值会关闭这层过滤。共享分区也不能省略此字段。
- topic 保留、分区数、换代和节点号复用约束见 `docs/design/control-plane-topic-partitioning-20260908.md` 与 `docs/design/kafka-topic-retention-strategy.md`，以实际配置为准。

## Traffic Statistics System
- **C++**: `TrafficStatsCollector` singleton in `cpp/libs/engine/core/network/traffic_statistics.h/.cpp`. Per-message atomic counters (send/recv count, bytes, max size). Registered automatically in `Node` constructor via `RegisterTrafficStatsReporter()`. The old per-message `LogMessageStatistics()` in `GameChannel` has been removed with the `RpcChannel` rewrite; RPC0 call outcomes are counted by `RpcChannelStats` (`network/rpc_channel_stats.h`, 30s `[RpcChannel] summary` line).
- **Go**: `grpcstats.Collector` in `go/shared/grpcstats/collector.go`. gRPC `UnaryServerInterceptor` tracking per-method call count, request/response bytes, latency avg/max. Wired into all Go services.
- **Toggle C++**: env `NODE_TRAFFIC_STATS_ENABLED=1`, optional `NODE_TRAFFIC_STATS_AUTO_DISABLE_MINUTES`, `NODE_TRAFFIC_STATS_INTERVAL_SECONDS` (default 30s).
- **Toggle Go**: env `GRPC_TRAFFIC_STATS_ENABLED=1`, optional `GRPC_TRAFFIC_STATS_AUTO_DISABLE_MINUTES`, `GRPC_TRAFFIC_STATS_INTERVAL_SECONDS` (default 30s).
- **Production safe**: atomic-only counters, periodic summary (not per-message), auto-disable timeout.

## Broadcast Message Optimization (Three-Level System)
- **Three levels**: `BroadcastToPlayers` (AOI subset), `BroadcastToScene` (scene-wide), `BroadcastToAll` (server-wide).
- **Bitmap encoding**: `BroadcastToPlayers` auto-selects bitmap when `count >= 32 && bitmapBytes + 6 < count * 3`. Session IDs encoded as `session_bitmap_base` (uint32) + `session_bitmap` (bytes). 750 players: ~2250 → ~155 bytes (-93%).
- **Session ID layout**: `uint32`, upper 15 bits = gate node_id, lower 17 bits = sequence. All sessions on one gate share upper bits → bitmap span bounded.
- **C++ API**（`client_message_sender.h/cpp`）：`BroadcastToPlayerClients()`、`BroadcastToSceneClients()`、`BroadcastToAllClientsOnKnownGates()` 均经 `RpcSession::SendRequest` 走 muduo TCP RPC；玩家子集自动编码 bitmap，场景广播按 scene_id 筛选，后者只覆盖当前线程注册表中拥有 `RpcSession` 的 Gate 上的全部客户端会话。
- **Go API** (`go/shared/kafkautil/gate_push.go`): `BroadcastToPlayers()`, `BroadcastToScene()`, `BroadcastToAll()` — Kafka-based, same three levels. `EncodeBroadcastTargets()` pairs each session with its target player and picks the bitmap form automatically; every producer must fill the target player (`PushToPlayerEvent.player_id`, `BroadcastToPlayersEvent.player_list`).
- **Gate handlers**：muduo RPC 在 `gate_service_handler.cpp`，Kafka 在 `gate_event_handler.cpp`；两者都委托共享投递模块 `gate_push`（`cpp/nodes/gate/handler/gate_push_delivery.{h,cpp}`：身份栅栏、广播编码一次）。
- **Proto**：`proto/gate/gate_service.proto`（C++ 节点 RPC）、`proto/contracts/kafka/gate_event.proto`（Kafka 命令）。
- **Design doc**: `docs/design/broadcast-message-size-analysis.md`.

## gRPC 服务端与线程(C++ 节点,muduo 驱动)
- scene / battle 内嵌 gRPC 服务端承接控制面 RPC(CreateScene、DestroyScene、ReleasePlayer、资产 RPC、CreateBattle 等)。第 4 批起它是 `muduo_grpc::Server`(muduo `Acceptor` + nghttp2,设计见 `docs/design/grpc-muduo-transport.md` §15):接入、HTTP/2、派发、截止、排空全部跑在节点 EventLoop 上,**不新增任何线程**;不再构造 `grpc::Server` / `ServerBuilder` / `ServerContext` / `CompletionQueue`。每个节点一个 gRPC 端口(TCP+30000),scene 只注册 `SceneNodeGrpc`(9 个方法),battle 只注册 `BattleNode`(6 个方法)。
- 生成的 `<Svc>Impl : muduo_grpc::Service` 在 loop 线程上被调用:`grpc.<M>.guard` 守护段要么同步 `Reject`,要么发起异步步骤(如 `AcquireAllocationPermitAsync`)后再 `Proceed()`;`Handle<M>` 同步执行。handler 里**不得阻塞**(不用 `promise/future`,不调 `AcquireAllocationPermitBlocking`),否则卡住整个逻辑帧。异步续跑只捕获调用句柄(`shared_ptr<GrpcUnaryCall>`)或 weak,不捕获 `this`。「业务逻辑单线程」仍然不等于进程只有一个线程(Kafka、Redis、日志等仍有自己的线程)。
- 第 3 批起生成的 C++ gRPC 客户端也走 muduo_grpc(节点实体上的 `MuduoGrpcChannelPtr`),节点不再拉起官方 gRPC 运行时(`grpc_init`、EventEngine、poller、Lifeguard)。保持这一点:进程里任何路径再构造官方 grpc 对象,这些线程就会回来。2026-10-26 起继续移除官方 SDK：不保留初始化探针、官方链接项、`grpc_cpp_plugin` 或 `*.grpc.pb.*`；Protobuf、Abseil、GoogleTest、BoringSSL 独立固定提交，版本与迁移前相同。依赖边界及验证以主传输设计的后续决策为准。

- `GRPC_SERVER_MAX_POLLERS` **已退役**:进程忽略它,设了只打一行 WARN;部署脚本不再注入。并发上限由服务端内建:每连接 `MAX_CONCURRENT_STREAMS=256`、全节点在途 1024(超限在业务之前回 `RESOURCE_EXHAUSTED`)、派发泵每轮 ≤64 个且 ≤2ms。
- `GRPC_THREAD_POOL_RESERVE_THREADS` / `GRPC_THREAD_POOL_MAX_THREADS` / `GRPC_MAX_THREADS` 对节点线程数都没有作用(`GrpcChannelCache` 已随第 3 批删除),不要再用它们「调线程」。线程数核对按 `grpc-muduo-transport.md` §15.14 第 8 步(thread baseline + Linux `/proc/<pid>/task/*/comm`)。

### gRPC 服务端关机
- 关机全部在 loop 内完成,没有关机线程、没有 join(旧的 `grpcShutdownThread_` 已删):`ShutdownInLoop` 依次 StopConsumers → beforeShutdown hook → 看门狗 → `grpcServer_->BeginShutdown({2s, 1s, 1s}, onDrained)` → MaybeFinalize(`grpc-muduo-transport.md` §15.10)。
- `BeginShutdown`:置排空标志(此后 `ServerDraining()` 为真)→ 停监听(Acceptor 经 `queueInLoop` 销毁)→ 每条连接两段 GOAWAY(shutdown notice + PING,收到 PING ACK 或满 1s 再发最终 GOAWAY)。最终 GOAWAY 之前收到的调用照常处理;2s 排空到期时,未开始的回 `RST_STREAM(REFUSED_STREAM)`,已开始的回 `UNAVAILABLE "server shutting down"`;之后写半关,宽限 1s 再 forceClose,总上限约 3s。
- `onDrained` 恰好一次、经 `queueInLoop` 投递;`Node::OnGrpcServerDrained` 在这个排队任务里 reset Server,早于两跳 `quit()`。`muduo_grpc::Server` 与 `Acceptor` 只能在 `queueInLoop` 任务里或 loop 外析构(I/O 事件处理期间 `removeChannel` 有断言)。
- handler 不能同步等待排空:排空本身要靠 loop 推进。停机期间要拒绝的变更型 RPC 在守护段里查 `ServerDraining()`(scene CreateScene 回 `UNAVAILABLE "scene_node_shutting_down"`)或用自己的准入闸(battle CreateBattle 回 `battle_not_allocatable`)。
- 截止由服务端执行:投递前或等许可时到期回 `DEADLINE_EXCEEDED`、不进业务;业务一旦开始不会被打断,迟到的完成被丢弃并计数。恰好一次的唯一裁决点是 `Server::CompleteCall`。

## Scene Manager 与 Gate 当前通信

- SceneManager 为 Go/go-zero 服务，配置入口 `go/scene_manager/etc/scene_manager_service.yaml`；Gate 的 Kafka 消费链已实现，不是待接线功能。
- `RoutePlayer`、`BindSession`、`KickPlayer` 等控制命令经上述 Kafka 节点命令 topic 投递。Gate 与本 zone Scene 的玩家热路径走 muduo TCP RPC；Go 控制面仍走 gRPC。
- 节点类型事实源在 `proto/common/base/node.proto`，`SceneManagerNodeService = 25`；新改动统一使用 Scene Node 名称。
- 完整架构与路由模式默认值见 `docs/design/ARCH.md`、`docs/design/client-rpc-router.md`，共享分区寻址见 `docs/design/control-plane-topic-partitioning-20260908.md`。

## Build, Test, and Lint Commands

### C++ (Core Game Server)
- **Build (Windows solution)**: `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`（按根规范串行）
- **Build a node with CMake (example: scene)**: `cd cpp\nodes\scene && cmake -S . -B build && cmake --build build --config Debug`
- **Run C++ tests**: test projects are under `cpp\tests\` (run in Visual Studio Test Explorer or execute the built test binary directly).
- **Run a single C++ test**: `build\cpp\tests\bag_test.exe --gtest_filter=BagTest.AddNewGridItem`
- **Lint C++**: use `clang-tidy` with `cpp\.clang-tidy` (example: `clang-tidy <file.cpp> --config-file=cpp\.clang-tidy`)

### Go (Microservices)
- **协议重生成**：在仓库根按 `AGENTS.md` §4 运行 `dev_tools.ps1 -Command proto-gen-run`；无客户端任务授权时使用 `proto_gen.no-unity.yaml`。`cd go && build.bat` 仅处理 db/login 包装与导入修正。
- **Run login service**: `cd go\login && go run login.go`
- **Run all login tests**: `cd go\login && go test ./...`
- **Run a single Go test**: `cd go\login && go test ./internal/logic/pkg/sessionmanager -run '^TestMintLoginGeneration_KeyName$'`
- **Dependencies**: `cd go\login && go mod tidy`

### Java (Gateway/Web)
- **Build**: `cd java\gateway_node && mvn clean install`
- **Run tests**: `cd java\gateway_node && mvn test`
- **Run a single Java test**: `cd java\gateway_node && mvn -Dtest=GatewayNodeApplicationTests#contextLoads test`

### Go Services (Local Dev)
- **Build all binaries**: `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build`
- **Build selected binaries**: `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build -GoServices login,db`
- **Start all**: `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start`
- **Stop all**: `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-stop`
- **Status**: `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-status`
- **List**: `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-list`

### Go Service Docker Images
- **Build all**: `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build-images -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1`
- **Push all**: `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-push-images -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1`

### C++ Nodes (Local Dev)
- **Start all**: `pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-start`
- **Start with counts**: `pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-start -GateCount 2 -SceneCount 4`
- **Stop all**: `pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-stop`
- **Status**: `pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-status`

### Unified Dev (C++ + Go, one-click)
- **Start all**: `pwsh -File tools/scripts/dev_tools.ps1 -Command dev-start`
- **Stop all**: `pwsh -File tools/scripts/dev_tools.ps1 -Command dev-stop`
- **Status**: `pwsh -File tools/scripts/dev_tools.ps1 -Command dev-status`

## High-Level Architecture

- **Polyglot backend split by responsibility**:
  - **C++ nodes** (`cpp/nodes/*`): runtime node processes (scene/gate/battle) and RPC handlers.
  - **C++ shared game logic** (`cpp/libs/services/scene/*`): ECS-heavy domain logic, with `system` and `comp` subtrees.
  - **Go services** (`go/login` and peers): go-zero based microservices and grpc servers.
  - **Java gateway/web** (`java/gateway_node`): Spring Boot gateway service (zone directory, gate assignment, admin APIs).
- **Proto-first contract flow**:
  - Source contracts live in `proto/`.
  - 共享 Go 绑定在 `go/proto/`，C++ 绑定在 `cpp/generated/`；`generated/proto/` 是生成流水线的暂存树，不能当作业务源码修改。
  - 完整绑定由 protogen 生成；`go/build.bat` 只重生成 db/login 的 go-zero 包装、修正导入并清理服务内重复绑定。
- **Service discovery/runtime wiring**:
  - Go 服务通过自己的启动入口注册到 etcd，使用共享注册与发现组件；Centre 已退役，login 的会话编排依赖 player_locator 与 scene_manager。
  - C++ scene request handlers delegate gameplay work to systems such as `PlayerSceneSystem`/`PlayerLifecycleSystem` instead of embedding heavy logic in transport layer handlers.

## Key Conventions

- **Generated/editable boundary**:
  - Do not edit generated protocol outputs under `generated/` or language `proto` output trees.
  - In generated-style C++ handler files that contain `///<<< BEGIN WRITING YOUR CODE`, keep custom logic inside those guarded sections.
- **C++ naming and layering**:
  - RPC request handlers are `*Handler` classes in `cpp/nodes/scene/handler/rpc/...` (example: `SceneScenePlayerHandler`).
  - Inter-node muduo RPC (RPC0) replies are handled at the call site: each `CallMethod` takes its own lambda (`RpcStatus` + response); there is no global per-message_id reply table any more. A server handler that must wait for a downstream call adopts its `done` with `RpcDoneCallback::Adopt(done)` (see `docs/design/muduo-rpc-channel.md`).
  - `rpc_replies/` only holds gRPC async reply adapters (pattern: `On<Domain><Method>Reply`) and the player reply chain.
  - ECS/domain logic stays in `*System` classes and `*Comp` structs under `cpp/libs/services/scene/...`.
- **Cross-service change workflow**:
  - `.proto` 变化后按根 `AGENTS.md` §4 重生成，再构建受影响的 C++/Go/Java 服务；不能用包装生成代替共享协议生成。
