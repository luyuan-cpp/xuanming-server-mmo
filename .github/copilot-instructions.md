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
   - **Known gap**: only 17 of 54 `.vcxproj` currently set it. Find the rest with
     `grep -L "/utf-8" $(find cpp third_party -maxdepth 4 -name '*.vcxproj' -not -path '*muduo_windows*')`.
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
- `get<T>(entity)`: asserts existence — crashes if absent. Only safe inside `view<T,...>` iterations or after an `any_of<T>`/`all_of<T>` guard.
- `try_get<T>(entity)`: returns nullptr when absent. Use for cross-entity lookups, optional components, or any entity whose archetype is not guaranteed.
- `get_or_emplace<T>(entity)`: **only** during entity initialization/setup. **Never** in per-tick systems, combat, spatial, attribute sync, or any per-frame code path. It silently creates zero-valued default components that corrupt ECS state and mask missing initialization.
- Return `std::optional<T>` from functions that depend on optional components.
- ECS sparse-set: entities without a component use zero memory in that pool and skip `view<T>` — intentional design for dungeons/instances.
- **Guard macros** (`macros/return_define.h`): Use `ECS_GET_OR_RETURN(var, Type, entity, retval)`, `ECS_GET_OR_VOID(var, Type, entity)`, `ECS_GET_OR_CONTINUE(var, Type, entity)`, `ECS_GET_OR_FALSE(var, Type, entity)` to reduce `try_get` + null-check boilerplate. Same convention as generated `Lookup*` macros.

## MSVC Lambda Capture with entt::entity
- MSVC (`/std:c++latest`) has a known issue (C3495) capturing `entt::entity` (enum class) variables in lambdas via simple capture `[entity]`.
- **Workaround**: Use init-capture syntax: `[entity = entity]()` instead of `[entity]()`.

## TimerTaskComp Lifecycle & EventLoop Destruction Order
- `TimerTaskComp::Cancel()` must null-check `GetThreadEventLoop()` before calling `loop->cancel()`. The EventLoop destructor sets `t_loopInThisThread = NULL` **before** entt registry cleanup destroys entity components.
- **Rule**: `Cancel()` must skip `loop->cancel()` when the loop is already gone to prevent nullptr crashes. 
- **Rule**: Constructors should not call `Cancel()` — `timerId` is already default-initialized.
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
- **SnowFlake GUIDs (uint64)**: All `player_id`, `entity`, `scene_id`, `item_id`, `buff_id`, `skill_id` (runtime instance), `tx_id`, `snapshot_id` etc. MUST stay `uint64` (64-bit SnowFlake layout).
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
- All C++ node types (Gate, Scene, Centre) subscribe to Kafka topics named `{type}-{node_id}`. Node IDs are recycled via etcd after restart.
- Each `*_command.proto` (gate, scene, centre) includes a `target_instance_id` field. The shared C++ consumer in `node_kafka_command_handler.h` validates it: mismatched instance UUID -> message dropped.
- **Rule**: Every Kafka producer (Go or C++) sending to `{type}-{id}` topics MUST fill `target_instance_id` with the target node's UUID. Empty value disables zombie filtering.
- **Rule**: Any new `*_command.proto` MUST include a `target_instance_id` string field for the shared consumer validation to work.
- Three-layer defense: (1) instance UUID validation, (2) 60s topic retention, (3) node ID reuse cooldown (etcd 60s + SnowFlake guard 600s).

## Traffic Statistics System
- **C++**: `TrafficStatsCollector` singleton in `cpp/libs/engine/core/network/traffic_statistics.h/.cpp`. Per-message atomic counters (send/recv count, bytes, max size). Registered automatically in `Node` constructor via `RegisterTrafficStatsReporter()`. Old `LogMessageStatistics()` in `GameChannel` is legacy (per-message logging, not thread-safe); new system replaces it with periodic summary logging.
- **Go**: `grpcstats.Collector` in `go/shared/grpcstats/collector.go`. gRPC `UnaryServerInterceptor` tracking per-method call count, request/response bytes, latency avg/max. Wired into all Go services.
- **Toggle C++**: env `NODE_TRAFFIC_STATS_ENABLED=1`, optional `NODE_TRAFFIC_STATS_AUTO_DISABLE_MINUTES`, `NODE_TRAFFIC_STATS_INTERVAL_SECONDS` (default 30s).
- **Toggle Go**: env `GRPC_TRAFFIC_STATS_ENABLED=1`, optional `GRPC_TRAFFIC_STATS_AUTO_DISABLE_MINUTES`, `GRPC_TRAFFIC_STATS_INTERVAL_SECONDS` (default 30s).
- **Production safe**: atomic-only counters, periodic summary (not per-message), auto-disable timeout.

## Broadcast Message Optimization (Three-Level System)
- **Three levels**: `BroadcastToPlayers` (AOI subset), `BroadcastToScene` (scene-wide), `BroadcastToAll` (server-wide).
- **Bitmap encoding**: `BroadcastToPlayers` auto-selects bitmap when `count >= 32 && bitmapBytes + 6 < count * 3`. Session IDs encoded as `session_bitmap_base` (uint32) + `session_bitmap` (bytes). 750 players: ~2250 → ~155 bytes (-93%).
- **Session ID layout**: `uint32`, upper 15 bits = gate node_id, lower 17 bits = sequence. All sessions on one gate share upper bits → bitmap span bounded.
- **C++ API** (`player_message_utils.h/cpp`): `BroadcastMessageToPlayers()` (gRPC per gate, auto bitmap), `BroadcastMessageToScene()` (gRPC per gate, by scene_id), `BroadcastMessageToAll()` (gRPC to all gates).
- **Go API** (`go/shared/kafkautil/gate_push.go`): `BroadcastToPlayers()`, `BroadcastToScene()`, `BroadcastToAll()` — Kafka-based, same three levels. `EncodeBitmapFields()` for auto bitmap in Go.
- **Gate handlers**: gRPC in `gate_service_handler.cpp`, Kafka in `gate_event_handler.cpp`. Both implement bitmap decode, scene filter, and all-session dispatch.
- **Proto**: `proto/gate/gate_service.proto` (gRPC), `proto/contracts/kafka/gate_event.proto` (Kafka events).
- **Design doc**: `docs/design/broadcast-message-size-analysis.md`.

## gRPC Server Thread Pool (C++ Nodes)
- C++ nodes embed a gRPC sync server for control-plane RPCs (CreateScene, DestroyScene, etc.).
- All RPC handlers dispatch to the muduo EventLoop via `runInLoop` + `promise/future`; business logic stays single-threaded.
- **Toggle**: env `GRPC_SERVER_MAX_POLLERS` (default `2`). Controls the max gRPC server poller threads. 1-2 is enough for control-plane operations.
- Multiple gRPC services (scene, mail, guild, etc.) share the same port and thread pool.

## Recent Architectural Decisions (2025-03-09)

### Scene Manager & Gate Communication
1. **Switch from gRPC Stream to Kafka**:
   - **Problem**: A full mesh of 40,000 Gates * 2000 Scene Managers would exhaust file descriptors and network resources.
   - **Solution**: Use Kafka for control messages (`RoutePlayer`, `KickPlayer`) to decouple Gate and SceneManager.
   - **Protocol**: 
     - Messages are defined in `GateCommand` within `scene_manager_service.proto`.
     - Topics follow the pattern `gate-{gate_id}`.
     - Messages include `target_instance_id` to prevent "zombie" messages from affecting restarted Gate nodes.

2. **Scene Manager Implementation (Go-Zero)**:
   - `SceneManager` is implemented in Go using go-zero.
   - It handles `EnterScene` requests by looking up player location in Redis.
   - Instead of maintaining a gRPC stream to Gate, it now pushes `RoutePlayer` commands to the appropriate Kafka topic.
   - Configuration requires Kafka brokers in `etc/scene_manager_service.yaml`.

3. **Gate Node Implementation (C++)**:
   - **Naming**: Use **Scene Node** (not Game Node) for consistency.
   - **Communication**: Gate Node must implement a Kafka Consumer to listen on `gate-{gate_id}`.
   - **Service Type**: Added `SceneManagerNodeService` (ID 25) to `node.proto` and `node_util.cpp` to distinguish the Go-based manager from C++ services.

4. **Pending Work**:
   - C++ side needs to link `librdkafka` or `cppkafka` and implement the consumer loop in `GateNode`.

## Build, Test, and Lint Commands

### C++ (Core Game Server)
- **Build (Windows solution)**: `msbuild game.sln /m /p:Configuration=Debug /p:Platform=x64`
- **Build a node with CMake (example: scene)**: `cd cpp\nodes\scene && cmake -S . -B build && cmake --build build --config Debug`
- **Run C++ tests**: test projects are under `cpp\tests\` (run in Visual Studio Test Explorer or execute the built test binary directly).
- **Run a single C++ test**: `build\cpp\tests\bag_test.exe --gtest_filter=BagTest.AddNewGridItem`
- **Lint C++**: use `clang-tidy` with `cpp\.clang-tidy` (example: `clang-tidy <file.cpp> --config-file=cpp\.clang-tidy`)

### Go (Microservices)
- **Regenerate Go gRPC/proto code**: `cd go && build.bat`
- **Run login service**: `cd go\login && go run login.go`
- **Run all login tests**: `cd go\login && go test ./...`
- **Run a single Go test**: `cd go\login && go test ./data -run TestPlayerFSM_InitialState`
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
  - **C++ nodes** (`cpp/nodes/*`): runtime node processes (scene/gate/centre) and RPC handlers.
  - **C++ shared game logic** (`cpp/libs/services/scene/*`): ECS-heavy domain logic, with `system` and `comp` subtrees.
  - **Go services** (`go/login` and peers): go-zero based microservices and grpc servers.
  - **Java gateway/web** (`java/gateway_node`): Spring Boot gateway service (zone directory, gate assignment, admin APIs).
- **Proto-first contract flow**:
  - Source contracts live in `proto/`.
  - Generated protocol outputs are checked into `generated/proto/` and language-specific trees (e.g., `go/login/proto/...`).
  - `go/build.bat` regenerates Go-side rpc/proto artifacts for login/db services.
- **Service discovery/runtime wiring**:
  - Go login starts grpc via `zrpc.MustNewServer`, registers node metadata, and watches centre nodes through `NodeWatcher` (etcd-backed).
  - C++ scene request handlers delegate gameplay work to systems such as `PlayerSceneSystem`/`PlayerLifecycleSystem` instead of embedding heavy logic in transport layer handlers.

## Key Conventions

- **Generated/editable boundary**:
  - Do not edit generated protocol outputs under `generated/` or language `proto` output trees.
  - In generated-style C++ handler files that contain `///<<< BEGIN WRITING YOUR CODE`, keep custom logic inside those guarded sections.
- **C++ naming and layering**:
  - RPC request handlers are `*Handler` classes in `cpp/nodes/scene/handler/rpc/...` (example: `SceneScenePlayerHandler`).
  - Async/reply handling uses dedicated reply handler functions (example pattern: `On<Domain><Method>Reply` in `cpp/nodes/scene/rpc_replies`).
  - ECS/domain logic stays in `*System` classes and `*Comp` structs under `cpp/libs/services/scene/...`.
- **Cross-service change workflow**:
  - When `.proto` changes, regenerate Go artifacts (`go\build.bat`) and then rebuild affected C++/Go/Java services that consume the updated contract.
