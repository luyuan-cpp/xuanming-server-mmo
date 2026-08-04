# 玩家登录/断线/重连/替换登录/超时清理 — 最终态事件流

> **架构前提**: Centre 节点已删除。Login + player_locator 是会话权威源；Login 统一调用 SceneManager.EnterScene 完成场景路由。

---

## 1. 首次登录 (LOGIN_FIRST)

```
Client → Gate(TCP) → Login(gRPC EnterGame)
                           │
                           ├─ 1. player_locator.GetSession → 无记录
                           ├─ 2. DecideEnterGame → FirstLogin
                           ├─ 3. player_locator.SetSession(state=Online, version=N)
                           ├─ 4. Kafka → gate-{gateId}: GateCommand{BindSession, enter_gs_type=1}
                           │         │
                           │         └─ Gate 收到 BindSessionEvent:
                           │              ├─ 创建 SessionInfo(playerId, sessionVersion)
                           │              └─ scene 未分配 → 存 pendingEnterGsType=1
                           │
                           ├─ 5. SceneManager.EnterScene(scene_id=0)
                           │         │
                           │         ├─ scene_id=0 → 负载均衡选 Scene 节点
                           │         └─ Kafka → gate-{gateId}: RoutePlayer(target_node_id)
                           │              │
                           │              └─ Gate 收到 RoutePlayerEvent:
                           │                   ├─ 设置 sceneNodeId
                           │                   └─ 发现 pendingEnterGsType=1 → ForwardLoginToScene()
                           │                        │
                           │                        └─ Scene 收到 GateLoginNotify(enter_gs_type=1)
                           │                             └─ OnPlayerLogin(LOGIN_FIRST): 初始化玩家实体, 加入默认场景
                           │
                           └─ 6. 返回 EnterGameResponse(playerId) 给 Client
```

**涉及组件**:
| 组件 | 文件 | 关键函数 |
|------|------|---------|
| Go Login | `go/login/internal/logic/clientplayerlogin/entergamelogic.go` | `applyLoadedPlayerSession()` |
| Go Login | `go/login/internal/svc/servicecontext.go` | `SendBindSessionToGate()` |
| Go Login | `go/login/internal/logic/pkg/sessionmanager/session_manager.go` | `DecideEnterGame()`, `DecisionToEnterGsType()` |
| Go SceneManager | `go/scene_manager/internal/logic/enterscenelogic.go` | `EnterScene()` |
| C++ Gate | `cpp/nodes/gate/handler/event/gate_event_handler.cpp` | `BindSessionEventHandler()`, `RoutePlayerEventHandler()` |
| C++ Gate | `cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp` | `DispatchGateEventFallback()` |
| C++ Scene | `cpp/nodes/scene/handler/rpc/player/game_player_handler.cpp` | `GateLoginNotify()` |
| C++ Scene | `cpp/libs/services/scene/player/system/player_lifecycle.cpp` | `OnPlayerLogin()` |

---

## 2. 断线处理 (Disconnect)

```
Client TCP 断开
    │
    └─ Gate 检测到连接断开:
         ├─ 保持 SessionInfo 不清除（等待重连窗口）
         └─ Kafka → player_locator topic: PlayerDisconnected
              │
              └─ player_locator.SetDisconnecting(playerId, sessionId):
                   ├─ state = Disconnecting
                   ├─ 写入 Redis ZSET (lease 超时时间戳)
                   └─ 开始 30s 重连窗口

Scene 侧: 不立即清理玩家实体，保持在场景中等待重连
```

**涉及组件**:
| 组件 | 文件 | 关键函数 |
|------|------|---------|
| C++ Gate | `cpp/nodes/gate/handler/event/gate_event_handler.cpp` | 连接断开回调 |
| Go player_locator | `go/player_locator/` | `SetDisconnecting()` |

---

## 3. 短时重连 (LOGIN_RECONNECT, 30s 窗口内)

### 3a. 同 Gate 重连

```
Client → 同一 Gate(TCP) → Login(gRPC EnterGame)
                           │
                           ├─ 1. player_locator.GetSession → state=Disconnecting, 同account
                           ├─ 2. DecideEnterGame → ShortReconnect
                           ├─ 3. player_locator.Reconnect(state=Online, version=N+1)
                           ├─ 4. Kafka → gate-{gateId}: GateCommand{BindSession, enter_gs_type=3}
                           │         │
                           │         └─ Gate 收到 BindSessionEvent:
                           │              ├─ 更新 SessionInfo(sessionVersion), 保留 conn 和 nodeIds
                           │              └─ sceneNodeId 已有 → 立即 ForwardLoginToScene()
                           │                   │
                           │                   └─ Scene 收到 GateLoginNotify(enter_gs_type=3)
                           │                        └─ OnPlayerLogin(LOGIN_RECONNECT):
                           │                             恢复消息推送, 同步最新状态给客户端
                           │
                           ├─ 5. SceneManager.EnterScene(scene_id=existing.SceneID)
                           │         └─ 幂等命中 → 无操作（玩家已在此场景）
                           │
                           └─ 6. 返回 EnterGameResponse(playerId) 给 Client
```

**关键**: 同 Gate 重连时 SessionInfo 保留了 sceneNodeId，BindSession 立即触发 ForwardLoginToScene。
SceneManager.EnterScene 仍然调用但幂等无副作用。

### 3b. 不同 Gate 重连

```
Client → Gate B(TCP) → Login(gRPC EnterGame)
                           │
                           ├─ 1. player_locator.GetSession → state=Disconnecting, 同account
                           ├─ 2. DecideEnterGame → ShortReconnect
                           ├─ 3. player_locator.Reconnect(newGateId=B, newSessionId, state=Online, version=N+1)
                           ├─ 4. Kafka → gate-{newGateId}: GateCommand{BindSession, enter_gs_type=3}
                           │         │
                           │         └─ Gate B 收到 BindSessionEvent:
                           │              ├─ 创建 SessionInfo (新 Gate 无旧 session)
                           │              └─ scene 未分配 → 存 pendingEnterGsType=3
                           │
                           ├─ 5. SceneManager.EnterScene(scene_id=existing.SceneID)
                           │         │
                           │         ├─ 查 Redis 定位 scene 所在节点
                           │         └─ Kafka → gate-{newGateId}: RoutePlayer(target_node_id)
                           │              │
                           │              └─ Gate B 收到 RoutePlayerEvent:
                           │                   ├─ 设置 sceneNodeId
                           │                   └─ 发现 pendingEnterGsType=3 → ForwardLoginToScene()
                           │                        │
                           │                        └─ Scene 收到 GateLoginNotify(enter_gs_type=3)
                           │                             └─ OnPlayerLogin(LOGIN_RECONNECT):
                           │                                  恢复消息推送, 同步最新状态给客户端
                           │
                           └─ 6. 返回 EnterGameResponse(playerId) 给 Client
```

**关键区别**: 不同 Gate 重连时 Gate B 没有旧的 SessionInfo，需要 SceneManager.EnterScene 重新路由。
session_id 也会变（session_id = gateNodeId << 32 | seq），player_locator.Reconnect 负责更新。

---

## 4. 替换登录 (LOGIN_REPLACE, 不同连接登录同一角色)

```
Client B → Gate B(TCP) → Login(gRPC EnterGame)
                              │
                              ├─ 1. player_locator.GetSession → state=Online, 不同 gate/session
                              ├─ 2. DecideEnterGame → ReplaceLogin
                              ├─ 3. Kafka → gate-{oldGateId}: GateCommand{KickPlayer, sessionId=old}
                              │         │
                              │         └─ Gate A 收到 KickPlayerEvent:
                              │              └─ 断开旧连接 conn.shutdown()
                              │
                              ├─ 4. player_locator.SetSession(新 gate/session, state=Online, version=N+1)
                              ├─ 5. Kafka → gate-{newGateId}: GateCommand{BindSession, enter_gs_type=2}
                              │         │
                              │         └─ Gate B 收到 BindSessionEvent:
                              │              ├─ 创建 SessionInfo
                              │              └─ scene 未分配 → 存 pendingEnterGsType=2
                              │
                              ├─ 6. SceneManager.EnterScene(scene_id=existing.SceneID)
                              │         │
                              │         ├─ 查 Redis 定位 scene 所在节点
                              │         └─ Kafka → gate-{newGateId}: RoutePlayer(target_node_id)
                              │         │
                              │         └─ Gate B 收到 RoutePlayerEvent:
                              │              ├─ 设置 sceneNodeId
                              │              └─ ForwardLoginToScene(enter_gs_type=2)
                              │                   │
                              │                   └─ Scene 收到 GateLoginNotify(enter_gs_type=2)
                              │                        └─ OnPlayerLogin(LOGIN_REPLACE):
                              │                             迁移会话到新连接, 清理旧连接状态
                              │
                              └─ 7. 返回 EnterGameResponse(playerId) 给 Client B
```

---

## 5. 超时清理 (Lease 过期, 30s 无重连)

```
player_locator LeaseMonitor 轮询 Redis ZSET:
    │
    ├─ 发现 lease 过期的 playerId
    ├─ ready → processing token claim（30s deadline，10s heartbeat）
    ├─ 完整 PlayerSession protobuf CAS：只删除 claim 对应的 DISCONNECTING 版本
    ├─ Kafka → gate-{gateId}: GateCommand{PlayerLeaseExpired, sessionId, playerId}
    ├─ gRPC → SceneManager.LeaveScene(playerId, authoritative sceneId)
    └─ 两个外部副作用都成功后才 ack processing
         ├─ 失败/进程退出：保留 payload，deadline 后换 token 重试
         └─ commit 后尚未 ack：SetSession fail-closed，禁止新会话抢走旧清理权

Scene 侧:
    └─ 收到 ExitGame 或超时检测:
         └─ PlayerLifecycleSystem::HandleExitGameNode()
              ├─ SavePlayerToRedis (持久化)
              └─ 销毁玩家实体
```

正常 `LeaveGame` 调用 `MarkOffline{player_id, expected_session_id,
expected_session_version}`；Locator 只在 session id/version 仍匹配时以 Lua CAS
删除。迟到的旧连接请求是成功 no-op，不能删掉替换登录后的新会话。

---

## 数据流总结

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

## Kafka Topic 与消息映射

| Topic | 方向 | 消息 | 用途 |
|-------|------|------|------|
| `gate-{gateId}` | Login → Gate | `GateCommand{BindSession}` | 绑定会话 + 携带 enter_gs_type |
| `gate-{gateId}` | Login → Gate | `GateCommand{KickPlayer}` | 踢掉旧连接（替换登录） |
| `gate-{gateId}` | SceneManager → Gate | `GateCommand{RoutePlayer}` | 分配 sceneNodeId |
| `gate-{gateId}` | player_locator → Gate | `GateCommand{PlayerLeaseExpired}` | 超时清理 |

## enter_gs_type 枚举值

| 值 | 名称 | 含义 |
|----|------|------|
| 0 | LOGIN_NONE | 无操作 |
| 1 | LOGIN_FIRST | 首次登录 |
| 2 | LOGIN_REPLACE | 替换登录（顶号） |
| 3 | LOGIN_RECONNECT | 短时重连 |

## 关键设计决策

1. **Login + player_locator 是会话唯一权威源** — 不再依赖 Centre 节点。
2. **Login 统一调用 SceneManager.EnterScene** — 首次登录 scene_id=0（负载均衡选节点），重连/替换使用 existing.SceneID。
3. **Gate 是纯转发层** — BindSession 绑定会话；RoutePlayer 绑定场景；ForwardLoginToScene 通知 Scene。
4. **Kafka 解耦** — Gate/Login/SceneManager/player_locator 全部通过 Kafka 异步通信，无 full-mesh gRPC 流。
5. **pendingEnterGsType 延迟机制** — 首次登录/跨 Gate 场景时 BindSession 先于 RoutePlayer 到达，Gate 暂存 enter_gs_type 等待 scene 分配后再转发。
6. **同 Gate 重连快速路径** — SessionInfo 保留 sceneNodeId，BindSession 直接触发 ForwardLoginToScene，SceneManager.EnterScene 幂等无副作用。
7. **GateLoginNotify 替换 Centre2GsLogin** — 原由 Centre 发送，现由 Gate Kafka 事件驱动转发。

## 场景切换时 Gate 绑定机制

### 核心结论

- 普通切换场景不依赖旧的 `Gate::PlayerEnterGameNode` 回调。
- 当前绑定入口是 `SceneManager -> Kafka gate-{gateId} -> RoutePlayerEvent -> GateEventHandler::RoutePlayerEventHandler`。
- Gate 更新的是同一条 TCP 会话上的 `SessionInfo(SceneNodeService -> targetNodeId)` 映射，而不是重建 Gate 连接。

### 详细流程

1. 上游业务触发 `SceneManager.EnterScene`。
2. SceneManager 选择目标 Scene 节点并更新玩家位置（Redis 作为位置事实源）。
3. SceneManager 向 `gate-{gateId}` 发送 `GateCommand{RoutePlayer}`（payload 为 `RoutePlayerEvent{session_id,target_node_id}`）。
4. Gate 消费 Kafka 命令并分发 `RoutePlayerEvent`。
5. `RoutePlayerEventHandler` 通过 `session_id` 找到会话，执行 `SetNodeId(SceneNodeService, target_node_id)`。
6. 后续该玩家客户端消息在 Gate 转发时通过 `SessionInfo` 读取新绑定，路由到新 Scene 节点。

### 与 pendingEnterGsType 的关系

- `pendingEnterGsType` 主要用于登录/重连竞态（`BindSessionEvent` 先于 `RoutePlayerEvent` 到达）。
- 普通切场景通常只有 `RoutePlayerEvent`，不会触发这套延迟登录通知机制。
