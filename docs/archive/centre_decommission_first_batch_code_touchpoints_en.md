# Centre Decommission First Batch - Code Touchpoints (2026-03-15)

## Scope of Batch 1
- Keep wire compatibility where needed, but remove Centre semantics from SceneManager entry points.
- Add fencing/ownership metadata for no-SPOF control-plane safety.
- Do not edit generated files under generated/; regenerate via existing pbgen pipeline.

## 1) Proto contract changes (source-of-truth)
- File: `proto/scene_manager/scene_manager_service.proto`
- Change RPC names (keep thin wrappers in server if needed):
  - `EnterSceneByCentre` -> `RoutePlayerToScene`
  - `LeaveSceneByCentre` -> `ReleasePlayerFromScene`
- Message rename and field migration:
  - `EnterSceneByCentreRequest` -> `RoutePlayerToSceneRequest`
  - `LeaveSceneByCentreRequest` -> `ReleasePlayerFromSceneRequest`
  - deprecate `centre_node_id` (field number 3) and replace semantics with `request_source_node_id` (same field number if wire compatibility required).
- Add HA/fencing fields to route request:
  - `uint64 orchestrator_epoch` (new)
  - `string orchestrator_instance_id` (new)
  - keep `gate_id`, `gate_instance_id`, `request_id`.

## 2) Kafka command contract extension
- File: `proto/contracts/kafka/gate_command.proto`
- Add optional control-plane fencing metadata:
  - `optional uint64 orchestrator_epoch` (new field number)
  - `optional string orchestrator_instance_id` (new field number)
- Keep existing `target_instance_id` as Gate-side stale guard.

## 3) Go SceneManager logic refactor (thin wrapper pattern)
- Files:
  - `go/scene_manager/internal/server/scenemanagerserver.go`
  - `go/scene_manager/internal/logic/enterscenebycentrelogic.go`
  - `go/scene_manager/internal/logic/leavescenebycentrelogic.go`
- Actions:
  - Keep generated method names callable, but delegate to verb-based internal funcs:
    - `ProcessRoutePlayerToScene(...)`
    - `ProcessReleasePlayerFromScene(...)`
  - Validate ownership/fencing before write-path:
    - reject stale `orchestrator_epoch`
    - reject mismatched orchestrator instance for same epoch
  - Keep idempotency by `request_id` and current-location check.

## 4) Gate consumer validation hardening
- Files:
  - `cpp/nodes/gate/main.cpp`
  - `cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp`
- Current state:
  - already validates `target_gate_id`
  - already validates `target_instance_id`
- Batch 1 add:
  - structured log fields include `player_id`, `session_id`, `event_id`, topic.
  - optional metric counters for stale/mismatch drops.

## 5) Regeneration / build order
1. Edit source proto only under `proto/`.
2. Run pbgen generation (`pbgen: run` task or project script).
3. Run `gofmt` for touched Go files.
4. Build Go scene_manager service.
5. Build C++ gate node target.

## 6) Known blast radius to watch
- Any `*ByCentre` references in Go generated grpc stubs and callsites.
- Existing C++ scene flow still contains `centre_node_id` in multiple proto domains; do not mass-rename in batch 1.
- Keep migration incremental: SceneManager contract first, then scene/player lifecycle contracts in batch 2.
