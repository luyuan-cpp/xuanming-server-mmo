# Scene Node gRPC Server Design

**Date:** 2026-04-15
**Last updated:** 2026-07-31
**Status:** Implemented

## Problem

Go `scene_manager` connects to C++ Scene Node via `grpc.NewClient()`, but Scene Node only exposes a TCP port with RPC0 (ProtobufCodecLite) codec. The gRPC HTTP/2 connection preface (`PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n`) is misinterpreted by the RPC0 codec as a 1.3-billion-byte message length, triggering `kInvalidLength` errors. The scene_manager retries every few seconds, flooding Scene logs.

```
ERROR ProtobufCodecLite::defaultErrorCallback - InvalidLength
  peer: 172.27.16.1:57211 local: 172.27.16.1:20000
```

### Root Cause

| Component | Behavior |
|-----------|----------|
| C++ Scene Node | Registers to etcd with `protocol_type = PROTOCOL_TCP`, exposes TCP(RPC0) port |
| Go scene_manager | Reads endpoint from etcd, dials with `grpc.NewClient()` regardless of `protocol_type` |
| Wire format mismatch | RPC0 = `[len:4]["RPC0":4][payload][checksum:4]`; gRPC = HTTP/2 framing |

## Decision: Dual-Port Architecture

Scene Node exposes **two ports**:

```
Scene Node (C++)
├── TCP  :N     ← Gate node (RPC0, node-to-node, ProtobufCodecLite)
└── gRPC :N+1   ← Go services (scene_manager, future cross-server services)
```

### Why scene_manager Connects TO Scene (Not the Reverse)

**Principle: whoever issues commands is the client.**

| System | Orchestrator → Worker | Pattern |
|--------|----------------------|---------|
| Kubernetes | API Server → kubelet | Master dials worker |
| Agones | Allocator → GameServer | Allocator dials game server |
| This project | scene_manager → Scene Node | Orchestrator dials executor |

Scene does not need to RPC-call scene_manager:
- Load reporting already uses Redis/etcd metadata
- Player enters scene via Gate → scene_manager gRPC, not Scene → scene_manager

### Why Not Bidirectional Streaming

| Aspect | Unary gRPC | Bidirectional Stream |
|--------|-----------|---------------------|
| Complexity | Simple request-response | Heartbeat, reconnect, stream multiplex |
| Connection management | gRPC handles it | Manual stream-to-node mapping |
| Cross-server | Any zone dials any scene directly | Need cross-zone stream management |
| Error handling | gRPC status codes, immediate | Async, slow failure detection |

### Why Not Kafka for Scene Commands

| Aspect | gRPC | Kafka |
|--------|------|-------|
| CreateScene/DestroyScene | Request-response, immediate result | Needs reply topic + correlation ID |
| Latency | Sub-millisecond | Consumer poll interval (~100ms) |
| Error handling | Status codes, deadline | Async, no built-in reply |
| Appropriate for | Commands needing response | Fire-and-forget notifications |

Kafka remains appropriate for Gate's `RoutePlayer`/`KickPlayer` (notifications, no reply needed).

## Architecture

```
                                    ┌─────────────────────────┐
                                    │    Scene Node (C++)     │
                                    │                         │
  Gate Node ──── TCP (RPC0) ──────► │  TCP  :N   (RPC0)      │
                                    │                         │
  scene_manager ── gRPC ──────────► │  gRPC :N+1 (HTTP/2)    │
                                    │                         │
  cross-zone    ── gRPC ──────────► │  (same gRPC port)       │
  scene_manager                     └─────────────────────────┘
```

## Implementation (Completed)

### 1. Proto: Added gRPC Endpoint to NodeInfo

**File:** `proto/common/base/common.proto`

```protobuf
message NodeInfo {
    // ... existing fields 1-9 ...
    EndpointComp grpc_endpoint = 10; // gRPC server endpoint (for Go services to connect)
}
```

### 2. Proto/codegen: Added a dedicated gRPC-only Scene control proto

**File:** `proto/scene_manager/scene_node_service.proto`

Added a separate gRPC-only service definition for Scene control:
- `SceneNodeGrpc.CreateScene`
- `SceneNodeGrpc.DestroyScene`

This imports the existing Scene request/response messages from the legacy Scene proto, but avoids the C++ gRPC plugin limitation with `cc_generic_services = true`.

### 3. C++ Node base class: Added optional gRPC server

**File:** `cpp/libs/engine/core/node/system/node/node.h`

- `RegisterGrpcService(grpc::Service*)` — register before `StartRpcServer()`
- `StartGrpcServer()` / `ShutdownGrpcServer()` — lifecycle methods
- `grpcServer_` (unique_ptr), `grpcServices_` (vector)
- No dedicated thread calls `grpcServer_->Wait()`; `Wait()` is only a passive
  join point and does not drive request processing

**File:** `cpp/libs/engine/core/node/system/node/node.cpp`

- `StartGrpcServer()` called in `StartRpcServer()` after TCP server, only if services registered
- The sync server owns its poller threads; `GRPC_SERVER_MAX_POLLERS` defaults to 8
- Shutdown temporarily runs `grpc::Server::Shutdown()` on a Node-owned worker so
  the muduo EventLoop remains available to complete queued handlers
- After `Shutdown()` returns, the worker queues finalization back to the
  EventLoop; the EventLoop joins the completed worker and tears down the
  remaining Node resources
- Banner updated to include gRPC endpoint

### 4. C++ Node allocator: gRPC port allocation

**File:** `cpp/libs/engine/core/node/system/node/node_allocator.cpp`

After TCP port allocation, if node has gRPC services, allocates gRPC port = TCP port + 1 (with availability check and fallback scan). Sets `grpc_endpoint` in NodeInfo, which is then published to etcd as JSON.

### 5. C++ Scene Node: gRPC service implementation

**File:** `cpp/nodes/scene/handler/grpc/scene_node_service.h/.cpp`

Implements `SceneNodeGrpc::Service` from generated
`scene_node_service.grpc.pb.h`:
- `CreateScene` — dispatches to muduo event loop via promise/future for thread safety
- `DestroyScene` — same dispatch pattern
- `ReleasePlayer` — same dispatch pattern
- Idempotent, matches existing muduo handler logic

**File:** `cpp/nodes/scene/main.cpp`

Registers `SceneNodeGrpcImpl` via `node.RegisterGrpcService()` before startup.
Changed from `RunSimpleNodeMainWithOwnedContext` to `RunNodeMain` to support constructing `SceneRuntimeContext` with `EventLoop*`.

## Threading and Shutdown Contract

“Scene gameplay logic is single-threaded” does not mean the Scene process has
only one thread:

- ECS and gameplay state are owned by the muduo EventLoop.
- gRPC sync handlers enter on gRPC poller threads, dispatch their `Handle*`
  business work with `runInLoop`, and wait on a promise/future.
- gRPC v1.83's built-in EventEngine eagerly reserves
  `Clamp(gpr_cpu_num_cores(), 4, 16)` workers plus one Lifeguard thread. The pool
  may grow under backlog; 16 is not a hard maximum.
- The built-in EventEngine exposes no public reserve/max environment variable.
  Project variables named `GRPC_THREAD_POOL_RESERVE_THREADS` or
  `GRPC_THREAD_POOL_MAX_THREADS` must not be documented as controlling it.

Because sync handlers may be waiting for EventLoop work, calling
`grpc::Server::Shutdown()` synchronously on that same EventLoop creates a
shutdown cycle. The required state flow is:

```text
Running -> GrpcDraining -> Finalizing -> Done
```

1. The EventLoop marks shutdown and runs the pre-shutdown business hook.
2. A temporary, joinable worker calls `grpc::Server::Shutdown(deadline)`.
3. The EventLoop continues servicing already queued handler work.
4. When `Shutdown()` returns, the worker queues finalization to the EventLoop.
5. The EventLoop joins the completed worker, releases the remaining resources,
   and quits.

The deadline is a transport grace deadline. gRPC can cancel pending calls when
it expires, but it does not forcibly terminate a C++ sync handler that has
already started. It therefore must not be treated as a hard handler-duration or
process-shutdown bound. A truly bounded handler must implement its own
cancellation-aware or bounded wait.

### 6. Go scene_manager: Uses gRPC endpoint

**File:** `go/scene_manager/internal/logic/scene_node_client.go`

`resolveNodeEndpoint()` only accepts `grpcEndpoint` from etcd JSON. It does not
fall back to `endpoint`: that field is the raw RPC0 TCP port and dialing it with
gRPC recreates the protocol mismatch this design fixes.

**File:** `go/scene_manager/internal/logic/load_reporter.go`

Added `GrpcEndpoint` field to `sceneNodeRegistration` struct.

## Cross-Server Implications

- Any zone's scene_manager can discover remote Scene nodes from shared etcd
- Direct gRPC dial to remote Scene nodes can execute control-plane
  CreateScene/DestroyScene commands
- No Kafka topic routing or cross-zone stream management needed
- Same `CreateScene` RPC works for local and cross-zone requests

This does **not** make existing-player cross-node handoff safe. The player data
path still needs a durable handoff epoch; SceneManager therefore rejects that
flow by default even though the control-plane socket is reachable.

## Files Changed

| File | Change |
|------|--------|
| `proto/common/base/common.proto` | Added `grpc_endpoint` field 10 to NodeInfo |
| `proto/scene_manager/scene_node_service.proto` | **New** — dedicated gRPC-only Scene control service |
| `cpp/libs/engine/core/node/system/node/node.h` | gRPC server members and methods |
| `cpp/libs/engine/core/node/system/node/node.cpp` | gRPC server lifecycle, banner update |
| `cpp/libs/engine/core/node/system/node/node_allocator.cpp` | gRPC port allocation |
| `cpp/nodes/scene/handler/grpc/scene_node_service.h` | **New** — gRPC service header |
| `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` | **New** — gRPC service implementation |
| `cpp/nodes/scene/main.cpp` | Register gRPC service, use RunNodeMain |
| `go/scene_manager/internal/logic/scene_node_client.go` | Use grpcEndpoint and SceneNodeGrpc client |
| `go/scene_manager/internal/logic/load_reporter.go` | Parse grpcEndpoint |

## Multi-Node Multi-Zone: No Extra Mapping Needed

Each Scene node registers its own gRPC endpoint in etcd. No additional port mapping or service mesh is required.

```
etcd keys (example):
  SceneNodeService.rpc/zone/1/node_type/3/node_id/1 → {ip: x.x.x.x, port: 20000, grpc_port: 20001, zone_id: 1}
  SceneNodeService.rpc/zone/1/node_type/3/node_id/2 → {ip: x.x.x.x, port: 20002, grpc_port: 20003, zone_id: 1}
  SceneNodeService.rpc/zone/2/node_type/3/node_id/1 → {ip: y.y.y.y, port: 20000, grpc_port: 20001, zone_id: 2}
```

| Environment | Extra mapping? | Reason |
|-------------|---------------|--------|
| Local dev | No | Go services in Docker reach host IP directly |
| K8s | No | Each Pod has unique IP, no port conflict |
| Multi-zone | No | Each zone's Scene nodes register independently, scene_manager filters by zone |

- gRPC port = TCP port + 1 (convention, unique per node via etcd CAS port allocation).
- scene_manager caches by `(zoneId,nodeId)`, because the same numeric node ID can
  exist in multiple zones. Endpoint resolution requires exactly one live etcd
  registration for that pair; duplicates fail closed, including cache hits.
- gRPC port is **internal cluster communication only** — no LoadBalancer/NodePort exposure needed.

## Connection Scale / Cost Assessment

This design does **not** recreate the old full-mesh explosion problem.

| Dimension | Reality |
|-----------|---------|
| Who dials Scene gRPC | Mainly scene_manager, plus occasional cross-zone scene_manager |
| Per-Scene steady-state connections | Usually single-digit to low double-digit |
| Per-request connection creation | No — connections are cached and reused |
| Concurrency model | HTTP/2 multiplexing over a small number of TCP connections |
| Process threads | Includes sync pollers and EventEngine workers; business state still stays on one EventLoop |
| High-fanout traffic | Still stays on Kafka or existing TCP(RPC0), not this gRPC path |

So the cost is acceptable because this path is for **low-caller-count, request/response orchestration**, not for Gate-scale fanout.

## Related

- Gate codec race fix (2026-04-15): moved Gate TcpServer callbacks before WaitAndRun to prevent `kUnknownMessageType` on early client connections.
- Scene creation architecture: `docs/design/scene-creation-architecture.md` (if exists), or see Go scene_manager source.
