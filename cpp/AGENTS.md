# CPP KNOWLEDGE BASE

## OVERVIEW
`cpp/` holds runtime node entrypoints, transport adapters, shared scene-domain services, plugins, tests, and checked-in/generated C++ artifacts.

## STRUCTURE
```text
cpp/
├── nodes/       # Runnable node entrypoints and node-local handlers/replies
├── libs/        # Shared C++ libraries; scene domain logic lives here
├── tests/       # Native test projects/binaries
├── plugin/      # Custom clang-tidy / plugin work
└── generated/   # Generated C++ artifacts
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Scene process bootstrap | `nodes/scene/main.cpp` | Registers node + Kafka command handlers |
| Gate process bootstrap | `nodes/gate/main.cpp` | TCP client bridge + session wiring |
| New node `main.cpp` templates | `nodes/_template/` | Use `main.simple.cpp.example` or `main.with_context.cpp.example` |
| Node-local RPC handlers | `nodes/*/handler/rpc/` | Thin transport adapters |
| Node-local event handlers | `nodes/*/handler/event/` | Event-driven edge logic |
| Async/reply handling | `nodes/*/rpc_replies/` | `On<Domain><Method>Reply` style |
| Scene gameplay/domain | `libs/services/scene/` | actor/player/world/frame/team/etc. |
| Player save pipeline | `libs/services/scene/player/system/player_lifecycle.cpp` | `SavePlayerToRedis`: marshals → stamps probe → Redis cache → Kafka |
| Data-consistency stress probe | `libs/services/scene/player/system/stress_test_probe.{h,cpp}` | Gated by `STRESS_TEST_PROBE=1` env var. Go mirror: `go/db/internal/stresstest/probe.go` |
| Scene node role config | `libs/engine/config/config.cpp` `readGameConfig` | Honours `SCENE_NODE_TYPE` / `ZONE_ID` / `GAME_CONFIG_PATH` / `ZONE_REDIS_PASSWORD` / `SCENE_CONTROL_REDIS_PASSWORD` / `PLAYER_REDIS_PASSWORD` env overrides (the last three are Redis AUTH passwords, never logged; on K8s they come from Secrets via `secretKeyRef`, the node ConfigMap carries empty passwords) |
| Agones lifecycle (high-density) | `libs/engine/infra/agones/` | One `agones::GameServerLifecycle` shared by scene and battle (a scene or a battle room is one "unit"). Ready/Allocated state machine + unit counting. HTTP only on the lifecycle worker thread, never on the EventLoop. Design: `docs/design/agones-scene-node-high-density.md` |
| Static analysis rule | `.clang-tidy` | Custom `myplugin-no-member-pointer` check only |

## CONVENTIONS
- Keep handlers thin; move gameplay/state transitions into `libs/services/scene/*` systems/components/world code.
- Respect `///<<< BEGIN WRITING YOUR CODE` regions in scaffolded/generated-style C++ files.
- Do not hand-edit `cpp/generated/` outputs.
- Use **Scene Node** naming, matching current project terminology.
- Scene/gate command consumers are wired in `main.cpp` via `topicPrefix` / `groupPrefix` options.
- New node entrypoints should start from `nodes/_template/README.md` and reuse `node::entry::RunSimpleNodeMain...` helpers.
- Reply handlers are adapters; they should not become business-logic owners.

### Node Main PR Checklist (Summary)
- Start from `nodes/_template/main.simple.cpp.example` or `nodes/_template/main.with_context.cpp.example`.
- Keep startup logic thin; do not move gameplay/business logic into `main.cpp`.
- Ensure node main uses `node::entry::RunSimpleNodeMain...` helpers.
- Ensure Kafka topic/group and routing fields match the node contract.
- Ensure thread observability is active by default (or intentionally disabled via `NODE_THREAD_MONITOR_ENABLED=0`).
- Do not hand-edit generated outputs while wiring new node startup.

### ECS component access rules
- 修改或评审组件访问前，先读 [组件访问规则](../docs/design/ecs-component-access-rules.md)：持久数据在加载边界创建，按需状态在唯一写入口延迟创建，派生数据在实体创建时计算；读路径只读，缺失处理按类别执行。

## ANTI-PATTERNS
- Embedding large gameplay branches inside node handlers.
- Rewiring Kafka command flow ad hoc from inside unrelated handlers.
- Editing generated protobuf/codegen outputs directly.
- Broad edits in vendored engine trees when a node/service-layer fix is enough.
- 组件访问中的反例与审查清单统一见上述权威规则，避免在此复制另一套判据。

## COMMANDS
```bash
msbuild game.sln /m /p:Configuration=Debug /p:Platform=x64
cd cpp\nodes\scene && cmake -S . -B build && cmake --build build --config Debug
clang-tidy <file.cpp> --config-file=cpp\.clang-tidy
```

## NOTES
- **Agones lifecycle** (`libs/engine/infra/agones/`; scene and battle share one `agones::GameServerLifecycle`):
  - 所有 HTTP 都在 lifecycle 专属 worker 线程上发。创建类 handler 用 RAII 许可（`GameServerLifecycle::AllocationPermit`）：EventLoop 上的 gRPC 守护段（scene `CreateScene`、battle `CreateBattle`）用 `AcquireAllocationPermitAsync(loop, cb)`，结果一律在该 loop 线程上、永不在调用栈内回调（当场判定与 worker 裁决经 `queueInLoop` 投递，等待超时由该 loop 的定时器裁决并就地回调）；RPC0 的 `SceneHandler::CreateScene` 用 `AcquireAllocationPermitNonBlocking`，拿不到即 fail-closed；`AcquireAllocationPermitBlocking` 会阻塞调用线程，只给非 EventLoop 线程和测试用（第 4 批起节点上没有 gRPC 线程，见 `docs/design/grpc-muduo-transport.md` §15.9）。许可必须活到请求结束，在途创建才能阻止过早回 Ready。
  - Scene 房间计数只由 `SceneLifecycle::CreateScene` / `DestroyScene`（`libs/services/scene/world/scene_lifecycle.cpp`）直接调用 Agones 完成；两个 RPC 入口统一委托，不再经 dispatcher 通知或另加计数。Battle 房间计数只经 `BattleRoomManager::SetRoomLifecycleHooks`（接线在 `nodes/battle/main.cpp`），每次插表 / 删表各调用一次，key 为 battle_id。
  - libcurl is Linux-only and gated on `MMORPG_AGONES_CURL`; Windows builds get a no-op transport (`MakeDefaultHttpTransport()` returns nullptr → `Disabled`). Do not add cpp-httplib / CPR / Boost.Beast, and do not vendor curl into `third_party/`.
  - Sources are registered in `libs/engine/infra/infra.vcxproj`, `infra.vcxproj.filters`, and `libs/engine/infra/CMakeLists.txt`; do NOT re-register them in `scene.vcxproj` (the old `nodes/scene/agones/` path is gone). On Linux, `tools/scripts/build_linux.sh` regenerates the CMakeLists from the vcxproj via `tools/archived/vcxproj2cmake.py`. Tests live in `cpp/tests/agones_lifecycle_test/` and compile the four sources (`agones_rest_client`, `agones_gameserver_lifecycle`, `agones_gameserver_status`, `agones_client_endpoint_source`) directly with an injected fake transport (no sidecar needed); they link libprotobufd + absl because `agones_gameserver_status.cpp` parses GET /gameserver via `google::protobuf::Struct`.
- `libs/services/scene/` is the highest-value subtree for durable domain fixes.
- Tests are native binaries/projects under `cpp/tests/`; run the relevant target rather than assuming a single root test runner.
- **Stress test probe** (`libs/services/scene/player/system/stress_test_probe.{h,cpp}`):
  - Off by default; enabled by `STRESS_TEST_PROBE=1` environment variable (cached at process start via `IsEnabled()`).
  - 只有 `StampPlayerDatabase`(`player_database_1` 和 `StampPlayerDatabase1` 已于 2026-11-05 删除)。它经 `player_lifecycle.cpp` 的 `MakePlayerStoreOptions` 装进 `PlayerStoreClient::Options::stampProfile`:比脏之后、确定要写这一笔时才打在 kProfile 行的字节上,比脏不看探针(否则玩家恒脏)。不要挪到比脏之前。
  - `NextSeq` uses `max(prior+1, now_us)` to guarantee strict monotonicity across Scene restarts without persistence. The `now_us` floor is epoch microseconds.
  - `ComputeSig` implements dual-seeded FNV-1a-64 (128-bit total). It MUST produce byte-identical output to `go/db/internal/stresstest/probe.go:ComputeSig`. Golden vectors in `go/db/internal/stresstest/probe_test.go` lock the algorithm; if you change either side, update both AND both tests.
  - Added to `scene.vcxproj`, `scene.vcxproj.filters`, and `CMakeLists.txt`. Linux builds auto-pick it up via `vcxproj2cmake.py`.
  - Design doc: `docs/design/data-consistency-stress-testing.md`.
