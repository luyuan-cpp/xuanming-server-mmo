# Centre Decommission + HA Checklist (2026-03-15)

## Goal
- Remove Centre from hot path while avoiding any single point of failure.

## Control Plane Split
- Service discovery and liveness: etcd cluster (3/5 nodes).
- Player location truth: player_locator (multi-instance + durable store).
- Command fanout: Kafka (RF>=3, min.insync.replicas configured).
- Scene switch orchestration: scene_manager cluster (>=3 instances recommended).

## Ownership and Fencing
- Partition ownership by player_id (or zone_id) using consistent hash.
- Exactly one owner per partition via lease/epoch.
- Every control command carries target_instance_id and epoch.
- Consumers reject stale epoch or mismatched target_instance_id.

## Invariants (Must Hold)
- Single writer per player at any time.
- Scene handoff is serialized: old scene release+ack before new scene load.
- RoutePlayer/KickPlayer must be idempotent.

## Failure Handling
- Owner loss: partition re-election via lease expiry.
- Duplicate/stale messages: fenced by epoch + instance id.
- Player moved during routing:
  - important message: re-resolve locator and retry.
  - best-effort message: drop allowed.

## Migration Plan
1. Keep Centre only as observability/fallback, not routing authority.
2. Grey rollout by zone/player cohort to scene_manager+kafka path.
3. Run dual-path verification (old vs new route results).
4. Remove Centre from all hot-path traffic after stability window.
5. Keep emergency rollback toggle for one release cycle.

## Required Metrics Before Full Cutover
- Route command latency p95/p99.
- Scene switch success rate and timeout rate.
- Stale-command reject count (epoch/instance mismatch).
- Duplicate command rate and idempotent-hit rate.
- Player reconnect/kick rate during failover.

## Implementation Status (2026-03-15)

### Completed
- **Proto changes**: SessionDetails extended with gate_node_id+gate_instance_id; SceneManager RPCs renamed (EnterSceneByCentre→EnterScene, LeaveSceneByCentre→LeaveScene); gate_event.proto + gate_command.proto extended (BindSession=3, LeaseExpired=4, event_id field); gate_event.proto new messages (PlayerDisconnectedEvent, PlayerLeaseExpiredEvent, BindSessionEvent); player_event.proto created
- **Go Login**: sessionmanager (Redis-backed, account-based reconnect), gatenotifier (Kafka), entergamelogic/leavegamelogic/disconnectlogic rewritten without Centre, ServiceContext.KickOldSession via Kafka, connectToCentreNodes removed from login.go, CentreClient interface removed from session_cleaner
- **Go SceneManager**: logic files renamed + content updated (EnterScene/LeaveScene), server + interface updated
- **C++ Gate**: CentreNodeService removed from CanConnectNodeTypeList, Centre disconnect RPC removed, SessionDetails now carries gate_node_id+gate_instance_id, CentreNodeService removed from network_utils singleton, centre_service_service_metadata.h include removed from gate_service_handler.cpp
- **C++ Gate Kafka events**: gate_kafka_command_router handles all 5 command types (RoutePlayer, KickPlayer, Broadcast, BindSession, LeaseExpired); gate_event_handler has handlers for all 4 events with session/version checking
- **C++ Generated Proto**: session.pb.h/cc, gate_command.pb.h/cc, gate_event.pb.h/cc, scene_manager_service.pb.h/cc and .grpc.pb.h/cc regenerated via protoc; service_metadata.h/cpp updated
- **Go Generated Proto**: login/contracts/kafka/gate_command.pb.go, login/proto/common/session.pb.go regenerated with correct source paths

### Needs Full pbgen Run
- `cpp/generated/grpc_client/scene_manager/` still has ByCentre names in Send functions
- `tools/scene_manager/` (redundant copy) still has old names
- All Go generated proto under `go/generated/` needs regeneration

### Completed (Phase 2 — player_locator)
- **player_locator Go service**: Full implementation with 7 RPCs (SetLocation, GetLocation, MarkOffline, SetSession, GetSession, SetDisconnecting, Reconnect)
- **Proto regenerated**: player_locator_grpc.pb.go now includes all 7 session mgmt RPCs
- **Kafka contracts generated**: gate_command.pb.go, gate_event.pb.go, player_event.pb.go for player_locator module
- **Redis session store**: `player:session:{player_id}` (protobuf binary, TTL for disconnecting)
- **Lease monitor**: Background goroutine polls `player:leases` ZSET, atomic Lua ZPOPMIN, publishes GateCommand{LeaseExpired} to gate-{gate_id} Kafka topic
- **Node registration**: etcd lease + NodeAllocator pattern (same as Login), NodeType=24 (PlayerLocatorNodeService)
- **Config**: go-zero RPC server, Redis, Kafka brokers, etcd registry, lease settings (default 30s TTL, 1s poll, batch 100)

### Verified Ready to Decommission (2026-03-15)
- **CentreEnterGsSucceed**: All callback stubs are empty (fire-and-forget no-ops) — NOT blocking
- **centre_node_id in session**: Already removed from session snapshot init — NOT populated
- **Login → player_locator**: sessionmanager already delegates to player_locator gRPC — DONE
- **Scene CanConnectNodeTypeList**: Removed CentreNodeService (whitelist, not required) — DONE
- **Team follower crash fix**: Replaced unsafe `.at(CentreNodeService)` with LOG_WARN + skip (was only crash path)
- **TryFallbackToCentreForImportantRoute**: Does NOT exist in codebase
- **RegisterAllSceneToCentre**: Does NOT exist in codebase
- **SendToCentrePlayerById**: Dead code, no callers in production

### Remaining Dead Code (harmless, clean up later)
- centre_service_response_handler.cpp/h in both Gate and Scene rpc_replies (empty stubs, never triggered)
- InitCentreReply() calls in register_response_handler.cpp (registers no-op callbacks)
- enterscenebycentrelogic.go (unreferenced after pbgen)
- KickSessionByCentre handler (logs "decommissioned" and returns)
- Generated ByCentre naming (needs pbgen run)

### Not Yet Started
- Team follower scene-change via SceneManager (currently skipped with LOG_WARN)
- Cross-node messaging via Kafka (replacing Centre's RoutePlayerStringMsg/RouteNodeStringMsg)
- Session lease expiry handling test coverage
- Dual-path verification and grey rollout

## Rule
- Keep location transparency: logic layers must not branch on cross-server/local.
