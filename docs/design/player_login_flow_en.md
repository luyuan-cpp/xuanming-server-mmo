# Player Login / Disconnect / Reconnect / Replace Login / Timeout Cleanup — Final Event Flow

> **Architecture Premise**: The Centre node has been removed. Login + player_locator are the authoritative session sources; Login uniformly calls SceneManager.EnterScene to complete scene routing.

---

## 1. First Login (LOGIN_FIRST)

```
Client → Gate(TCP) → Login(gRPC EnterGame)
                           │
                           ├─ 1. player_locator.GetSession → no record
                           ├─ 2. DecideEnterGame → FirstLogin
                           ├─ 3. player_locator.SetSession(state=Online, version=N)
                           ├─ 4. Kafka → gate-{gateId}: GateCommand{BindSession, enter_gs_type=1}
                           │         │
                           │         └─ Gate receives BindSessionEvent:
                           │              ├─ Create SessionInfo(playerId, sessionVersion)
                           │              └─ scene not assigned → store pendingEnterGsType=1
                           │
                           ├─ 5. SceneManager.EnterScene(scene_id=0)
                           │         │
                           │         ├─ scene_id=0 → load-balance to select Scene node
                           │         └─ Kafka → gate-{gateId}: RoutePlayer(target_node_id)
                           │              │
                           │              └─ Gate receives RoutePlayerEvent:
                           │                   ├─ Set sceneNodeId
                           │                   └─ Found pendingEnterGsType=1 → ForwardLoginToScene()
                           │                        │
                           │                        └─ Scene receives GateLoginNotify(enter_gs_type=1)
                           │                             └─ OnPlayerLogin(LOGIN_FIRST): initialize player entity, join default scene
                           │
                           └─ 6. Return EnterGameResponse(playerId) to Client
```

**Involved Components**:
| Component | File | Key Function |
|-----------|------|-------------|
| Go Login | `go/login/internal/logic/clientplayerlogin/entergamelogic.go` | `applyLoadedPlayerSession()` |
| Go Login | `go/login/internal/svc/servicecontext.go` | `SendBindSessionToGate()` |
| Go Login | `go/login/internal/logic/pkg/sessionmanager/session_manager.go` | `DecideEnterGame()`, `DecisionToEnterGsType()` |
| Go SceneManager | `go/scene_manager/internal/logic/enterscenelogic.go` | `EnterScene()` |
| C++ Gate | `cpp/nodes/gate/handler/event/gate_event_handler.cpp` | `BindSessionEventHandler()`, `RoutePlayerEventHandler()` |
| C++ Gate | `cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp` | `DispatchGateEventFallback()` |
| C++ Scene | `cpp/nodes/scene/handler/rpc/player/game_player_handler.cpp` | `GateLoginNotify()` |
| C++ Scene | `cpp/libs/services/scene/player/system/player_lifecycle.cpp` | `OnPlayerLogin()` |

---

## 2. Disconnect Handling (Disconnect)

```
Client TCP disconnected
    │
    └─ Gate detects connection drop:
         ├─ Keep SessionInfo (do not clear; wait for reconnect window)
         └─ Kafka → player_locator topic: PlayerDisconnected
              │
              └─ player_locator.SetDisconnecting(playerId, sessionId):
                   ├─ state = Disconnecting
                   ├─ Write to Redis ZSET (lease expiry timestamp)
                   └─ Begin 30s reconnect window

Scene side: do not immediately clean up player entity; keep in scene awaiting reconnect
```

**Involved Components**:
| Component | File | Key Function |
|-----------|------|-------------|
| C++ Gate | `cpp/nodes/gate/handler/event/gate_event_handler.cpp` | Connection drop callback |
| Go player_locator | `go/player_locator/` | `SetDisconnecting()` |

---

## 3. Short Reconnect (LOGIN_RECONNECT, within 30s window)

### 3a. Same-Gate Reconnect

```
Client → Same Gate(TCP) → Login(gRPC EnterGame)
                           │
                           ├─ 1. player_locator.GetSession → state=Disconnecting, same account
                           ├─ 2. DecideEnterGame → ShortReconnect
                           ├─ 3. player_locator.Reconnect(state=Online, version=N+1)
                           ├─ 4. Kafka → gate-{gateId}: GateCommand{BindSession, enter_gs_type=3}
                           │         │
                           │         └─ Gate receives BindSessionEvent:
                           │              ├─ Update SessionInfo(sessionVersion), retain conn and nodeIds
                           │              └─ sceneNodeId already present → immediately ForwardLoginToScene()
                           │                   │
                           │                   └─ Scene receives GateLoginNotify(enter_gs_type=3)
                           │                        └─ OnPlayerLogin(LOGIN_RECONNECT):
                           │                             Resume message pushing, sync latest state to client
                           │
                           ├─ 5. SceneManager.EnterScene(scene_id=existing.SceneID)
                           │         └─ Idempotent hit → no-op (player already in this scene)
                           │
                           └─ 6. Return EnterGameResponse(playerId) to Client
```

**Key Point**: On same-Gate reconnect, SessionInfo retains sceneNodeId, so BindSession immediately triggers ForwardLoginToScene.
SceneManager.EnterScene is still called but is idempotent with no side effects.

### 3b. Different-Gate Reconnect

```
Client → Gate B(TCP) → Login(gRPC EnterGame)
                           │
                           ├─ 1. player_locator.GetSession → state=Disconnecting, same account
                           ├─ 2. DecideEnterGame → ShortReconnect
                           ├─ 3. player_locator.Reconnect(newGateId=B, newSessionId, state=Online, version=N+1)
                           ├─ 4. Kafka → gate-{newGateId}: GateCommand{BindSession, enter_gs_type=3}
                           │         │
                           │         └─ Gate B receives BindSessionEvent:
                           │              ├─ Create SessionInfo (new Gate has no old session)
                           │              └─ scene not assigned → store pendingEnterGsType=3
                           │
                           ├─ 5. SceneManager.EnterScene(scene_id=existing.SceneID)
                           │         │
                           │         ├─ Query Redis to locate the scene's node
                           │         └─ Kafka → gate-{newGateId}: RoutePlayer(target_node_id)
                           │              │
                           │              └─ Gate B receives RoutePlayerEvent:
                           │                   ├─ Set sceneNodeId
                           │                   └─ Found pendingEnterGsType=3 → ForwardLoginToScene()
                           │                        │
                           │                        └─ Scene receives GateLoginNotify(enter_gs_type=3)
                           │                             └─ OnPlayerLogin(LOGIN_RECONNECT):
                           │                                  Resume message pushing, sync latest state to client
                           │
                           └─ 6. Return EnterGameResponse(playerId) to Client
```

**Key Difference**: On different-Gate reconnect, Gate B has no old SessionInfo and needs SceneManager.EnterScene to re-route.
session_id also changes (session_id = gateNodeId << 32 | seq); player_locator.Reconnect is responsible for the update.

---

## 4. Replace Login (LOGIN_REPLACE, different connection logs in as the same character)

```
Client B → Gate B(TCP) → Login(gRPC EnterGame)
                              │
                              ├─ 1. player_locator.GetSession → state=Online, different gate/session
                              ├─ 2. DecideEnterGame → ReplaceLogin
                              ├─ 3. Kafka → gate-{oldGateId}: GateCommand{KickPlayer, sessionId=old}
                              │         │
                              │         └─ Gate A receives KickPlayerEvent:
                              │              └─ Disconnect old connection conn.shutdown()
                              │
                              ├─ 4. player_locator.SetSession(new gate/session, state=Online, version=N+1)
                              ├─ 5. Kafka → gate-{newGateId}: GateCommand{BindSession, enter_gs_type=2}
                              │         │
                              │         └─ Gate B receives BindSessionEvent:
                              │              ├─ Create SessionInfo
                              │              └─ scene not assigned → store pendingEnterGsType=2
                              │
                              ├─ 6. SceneManager.EnterScene(scene_id=existing.SceneID)
                              │         │
                              │         ├─ Query Redis to locate the scene's node
                              │         └─ Kafka → gate-{newGateId}: RoutePlayer(target_node_id)
                              │         │
                              │         └─ Gate B receives RoutePlayerEvent:
                              │              ├─ Set sceneNodeId
                              │              └─ ForwardLoginToScene(enter_gs_type=2)
                              │                   │
                              │                   └─ Scene receives GateLoginNotify(enter_gs_type=2)
                              │                        └─ OnPlayerLogin(LOGIN_REPLACE):
                              │                             Migrate session to new connection, clean up old connection state
                              │
                              └─ 7. Return EnterGameResponse(playerId) to Client B
```

---

## 5. Timeout Cleanup (Lease Expired, no reconnect within 30s)

```
player_locator LeaseMonitor polls Redis ZSET:
    │
    ├─ Discover playerId with expired lease
    ├─ ready → processing token claim (30s deadline, 10s heartbeat)
    ├─ CAS the complete PlayerSession protobuf; delete only the claimed DISCONNECTING version
    ├─ Kafka → gate-{gateId}: GateCommand{PlayerLeaseExpired, sessionId, playerId}
    ├─ gRPC → SceneManager.LeaveScene(playerId, authoritative sceneId)
    └─ Ack processing only after both external side effects succeed
         ├─ Failure/process exit: retain payload and retry with a new token after the deadline
         └─ Committed but not acked: SetSession fails closed so a new login cannot steal cleanup ownership

Scene side:
    └─ Receives ExitGame or timeout detection:
         └─ PlayerLifecycleSystem::HandleExitGameNode()
              ├─ SavePlayerToRedis (persist)
              └─ Destroy player entity
```

A graceful `LeaveGame` calls `MarkOffline{player_id, expected_session_id,
expected_session_version}`. Locator deletes with a Lua CAS only while both values
still match; a delayed request from an old connection is a successful no-op.

---

## Data Flow Summary

```
┌─────────┐    TCP     ┌──────┐   Kafka    ┌───────┐   gRPC    ┌───────┐
│  Client  │◄─────────►│ Gate │◄──────────►│ Login │◄────────►│player │
└─────────┘            │      │            │       │          │locator│
                       │      │   Kafka    │       │          └───────┘
                       │      │◄──────────►│       │
                       │      │            └───────┘
                       │      │   gRPC
                       │      │──────────►┌───────┐
                       └──────┘           │ Scene │
                                          └───────┘
```

## Kafka Topic and Message Mapping

| Topic | Direction | Message | Purpose |
|-------|-----------|---------|---------|
| `gate-{gateId}` | Login → Gate | `GateCommand{BindSession}` | Bind session + carry enter_gs_type |
| `gate-{gateId}` | Login → Gate | `GateCommand{KickPlayer}` | Kick old connection (replace login) |
| `gate-{gateId}` | SceneManager → Gate | `GateCommand{RoutePlayer}` | Assign sceneNodeId |
| `gate-{gateId}` | player_locator → Gate | `GateCommand{PlayerLeaseExpired}` | Timeout cleanup |

## enter_gs_type Enum Values

| Value | Name | Meaning |
|-------|------|---------|
| 0 | LOGIN_NONE | No-op |
| 1 | LOGIN_FIRST | First login |
| 2 | LOGIN_REPLACE | Replace login (kick existing session) |
| 3 | LOGIN_RECONNECT | Short reconnect |

## Key Design Decisions

1. **Login + player_locator are the sole authoritative session source** — No longer depends on the Centre node.
2. **Login uniformly calls SceneManager.EnterScene** — First login uses scene_id=0 (load-balance to select node); reconnect/replace uses existing.SceneID.
3. **Gate is a pure forwarding layer** — BindSession binds the session; RoutePlayer binds the scene; ForwardLoginToScene notifies Scene.
4. **Kafka decoupling** — Gate/Login/SceneManager/player_locator all communicate asynchronously via Kafka; no full-mesh gRPC streams.
5. **pendingEnterGsType deferred mechanism** — On first login / cross-Gate scenarios, BindSession arrives before RoutePlayer; Gate stores enter_gs_type and waits for scene assignment before forwarding.
6. **Same-Gate reconnect fast path** — SessionInfo retains sceneNodeId; BindSession directly triggers ForwardLoginToScene; SceneManager.EnterScene is idempotent with no side effects.
7. **GateLoginNotify replaces Centre2GsLogin** — Previously sent by Centre; now driven by Gate Kafka event forwarding.
