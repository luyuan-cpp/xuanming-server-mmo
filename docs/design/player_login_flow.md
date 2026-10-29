# 玩家登录/断线/重连/替换登录/超时清理 — 最终态事件流

> 2026-10-15 按当前代码校正：EnterGame 应答仅表示受理；断线删除 Gate 会话并通知 Login；同 Gate/跨 Gate 重连统一重新路由。30s 为默认 Locator 租约，不是 Scene 实体保留承诺。

> **架构前提**: Centre 节点已删除。Login + player_locator 是会话权威源；Login 统一调用 SceneManager.EnterScene 完成场景路由。

---

## 1. 首次登录 (LOGIN_FIRST)

```
Client → Gate(TCP) → Login(gRPC EnterGame)
                           │
                           ├─ 1. player_locator.GetSession → 无记录
                           ├─ 2. DecideEnterGame → FirstLogin
                           ├─ 3. player_locator.SetSession(state=Online, version=N)
                           ├─ 4. Kafka gate 命令（按 gateId 分区）: GateCommand{BindSession, enter_gs_type=1}
                           │         │
                           │         └─ Gate 收到 BindSessionEvent:
                           │              ├─ 绑定当前 TCP 会话的玩家与登录代次
                           │              └─ scene 未分配 → 存 pendingEnterGsType=1
                           │
                           ├─ 5. SceneManager.EnterScene(scene_id=0)
                           │         │
                           │         ├─ scene_id=0 → 负载均衡选 Scene 节点
                           │         └─ Kafka gate 命令（按 gateId 分区）: RoutePlayer(target_node_id)
                           │              │
                           │              └─ Gate 收到 RoutePlayerEvent:
                           │                   ├─ 设置 sceneNodeId
                           │                   └─ 发现 pendingEnterGsType=1 → ForwardPlayerToScene()
                           │                        │
                           │                        └─ Scene 收到 PlayerEnterGameNode(enter_gs_type=1)
                           │                             └─ HandleEnterGameNode（首登）: 初始化玩家实体, 加入默认场景
                           │
                           └─ 6. Scene 发 NotifyEnterScene 确认进场（EnterGame 应答已提前返回）
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
| C++ Scene | `cpp/nodes/scene/handler/rpc/scene_handler.cpp` | `PlayerEnterGameNode()` |
| C++ Scene | `cpp/libs/services/scene/player/system/player_lifecycle.cpp` | `HandleEnterGameNode()` |

---

## 2. 断线处理 (Disconnect)

```text
Client TCP 断开
    └─ Gate::HandleConnectionDisconnection
         ├─ 已绑定 Scene 且链路可用：经 muduo TCP RPC 代发 ExitGame(session_id, player_id)
         │    └─ Scene 按会话身份校验后进入退出/存盘流程
         ├─ 已发起 Login 或已绑定玩家：发 Login.Disconnect
         │    ├─ 路由模式：Gate → client_rpc_router → Login（gRPC）
         │    └─ 旧模式：Gate → 同 zone 可用 Login（gRPC）
         ├─ 注销在途调用、丢弃未发出的请求队列
         └─ 删除旧 SessionInfo

Login.Disconnect
    ├─ 关闭该连接的会话栅栏，清理临时登录态
    └─ 对仍匹配这条连接的玩家会话调用 player_locator.SetDisconnecting
         └─ 设置 DISCONNECTING，登记默认 30s 断线租约
```

30s 窗口保留的是 player_locator 的会话/重连资格，不是 Gate 的旧 TCP 会话，也不承诺 Scene 实体驻留 30s。正常断线会立即代发 ExitGame；退出存盘在途与重连相遇时，由 Scene 的会话与属主栅栏决定复用或重新载入，不能假设实体一定还在内存。尚未绑定 Scene 或链路不可用时，不能把“尝试通知”视为 Scene 已完成退出。

| 组件 | 文件 | 入口 |
|------|------|------|
| Gate | `cpp/nodes/gate/handler/rpc/client_message_processor.cpp` | `HandleConnectionDisconnection()` |
| Login | `go/login/internal/logic/clientplayerlogin/disconnectlogic.go` | `Disconnect()` |
| Locator 调用封装 | `go/login/internal/logic/pkg/sessionmanager/session_manager.go` | `SetSessionDisconnecting()` |
| Scene | `cpp/nodes/scene/handler/rpc/player/player_lifecycle_handler.cpp` | `ExitGame()` |

## 3. 短时重连 (LOGIN_RECONNECT, 默认 30s 租约内)

同 Gate 与跨 Gate 都建立新 TCP 会话，旧 SessionInfo 已在断线时删除；它们走同一条入场路径。

```text
Client → 原 Gate 或 Gate B：建立新 TCP 会话
    └─ Login.EnterGame（立即返回受理结果，后续链异步执行）
         ├─ GetSession → 同账号且仍可重连 → ShortReconnect
         ├─ player_locator.Reconnect：更新新 gate/session、版本与登录代次，取消断线租约
         ├─ Kafka gate 命令：BindSession(enter_gs_type=3)
         │    └─ Gate 绑定当前连接；路由未就绪时保留待入场状态
         ├─ SceneManager.EnterScene(zone_id=0, scene_id=0, gate_zone_id=当前接入区)
         │    ├─ 按 player:{id}:location 恢复权威位置与属主
         │    └─ Kafka gate 命令：RoutePlayer(target_node_id, scene_id, owner_epoch, ...)
         └─ Gate 汇合 BindSession 与 RoutePlayer
              └─ 链路/握手就绪后 ForwardPlayerToScene → PlayerEnterGameNode
                   └─ Scene 校验会话与属主，复用或载入实体，发 NotifyEnterScene
```

没有“同 Gate 保留 sceneNodeId、SceneManager 无操作”的重连特例。BindSession 与 RoutePlayer 的处理需要兼容到达顺序；Scene 链路尚未就绪时走有界补发，具体契约见 `cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp`。`zone_id=0` 让 SceneManager 依据 location 决定目标；若仍在另一物理 zone，可能先重定向连接。带有效目标区票据的第二条腿优先固定目标区，见 [cross-zone-scene-travel.md](./cross-zone-scene-travel.md)。

`session_id` 是 uint32 临时标识，由 `SessionIdGenerator = TransientNodeCompositeIdGenerator<uint32_t, kNodeBits>` 生成；普通构建中 `kNodeBits=17` 用作低位序列宽度，布局为 `[gate node_id:15][seq:17]`，即 `(gateNodeId << 17) | seq`。高位节点号与低位序列都受生成器边界约束；序列会回绕，Gate 自查活跃会话去重，它不是永久 GUID。新连接重新分配 session_id，允许以后复用旧数值；player_locator.Reconnect 更新这次分配的值。定义见 `cpp/libs/engine/core/network/network_utils.h`、`cpp/libs/engine/core/utils/id/node_id_generator.h` 和 `proto/common/base/session.proto`。

---

## 4. 替换登录 (LOGIN_REPLACE, 不同连接登录同一角色)

```
Client B → Gate B(TCP) → Login(gRPC EnterGame)
                              │
                              ├─ 1. player_locator.GetSession → state=Online, 不同 gate/session
                              ├─ 2. DecideEnterGame → ReplaceLogin
                              ├─ 3. Kafka gate 命令（按 oldGateId 分区）: GateCommand{KickPlayer, sessionId=old}
                              │         │
                              │         └─ Gate A 收到 KickPlayerEvent:
                              │              └─ 断开旧连接 conn.shutdown()
                              │
                              ├─ 4. player_locator.SetSession(新 gate/session, state=Online, version=N+1)
                              ├─ 5. Kafka gate 命令（按 newGateId 分区）: GateCommand{BindSession, enter_gs_type=2}
                              │         │
                              │         └─ Gate B 收到 BindSessionEvent:
                              │              ├─ 绑定新 TCP 会话
                              │              └─ scene 未分配 → 存 pendingEnterGsType=2
                              │
                              ├─ 6. SceneManager.EnterScene(zone_id=0, scene_id=0)
                              │         │
                              │         ├─ 按 player:{id}:location 解析当前场景与属主
                              │         └─ Kafka gate 命令（按 newGateId 分区）: RoutePlayer(target_node_id)
                              │         │
                              │         └─ Gate B 收到 RoutePlayerEvent:
                              │              ├─ 设置 sceneNodeId
                              │              └─ ForwardPlayerToScene(enter_gs_type=2)
                              │                   │
                              │                   └─ Scene 收到 PlayerEnterGameNode(enter_gs_type=2)
                              │                        └─ HandleEnterGameNode（顶号）:
                              │                             迁移会话到新连接, 清理旧连接状态
                              │
                              └─ 7. Scene 发 NotifyEnterScene 确认进场（EnterGame 应答已提前返回）
```

---

## 5. 超时清理 (Lease 过期, 30s 无重连)

```
player_locator LeaseMonitor 轮询 Redis ZSET:
    │
    ├─ 发现 lease 过期的 playerId
    ├─ ready → processing token claim（30s deadline，10s heartbeat）
    ├─ 完整 PlayerSession protobuf CAS：只删除 claim 对应的 DISCONNECTING 版本
    ├─ Kafka gate 命令（按 gateId 分区）: GateCommand{PlayerLeaseExpired, sessionId, playerId}
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

## 6. 登录超时配置待办（2026-10-23，未完成）

[TimeoutConf](../../go/login/internal/config/config.go) 中的 `TaskWaitTimeout`、`LoginTotalTimeout` 当前只有字段声明和配置，没有运行时读取；修改这两个值不会改变 DB 任务等待或登录总超时。下表数值仅指当前[本地配置](../../go/login/etc/login.yaml)，不代表所有部署配置。

| 配置 | 本地配置值 | 待办（未完成） |
|------|------------|----------------|
| `Timeouts.TaskWaitTimeout` | `30s` | 明确并实现与 DB 任务结果等待预算的关联；若确认废弃，则移除字段及各配置入口。 |
| `Timeouts.LoginTotalTimeout` | `10s` | 明确同步受理与异步入场各自覆盖的流程边界，再接入相应总预算；若确认废弃，则移除字段及各配置入口。 |

**这不表示登录没有超时。** 当前本地配置的同步 RPC `Timeout: 4000` 由 zrpc 执行；[EnterGame](../../go/login/internal/logic/clientplayerlogin/entergamelogic.go) 后台入场链另建独立 context，使用 `enterGameChainBudget = 5min`，预加载回调登记使用 `dispatcherTaskTTL = 8s`。[TaskResultDispatcher](../../go/login/internal/dispatcher/task_result_dispatcher.go) 按登记 TTL 记录过期时间并每秒扫描，不保证严格在第 8 秒触发回调。这些预算均不是由上述两个配置字段驱动的。

接线后需验证配置确实改变相应截止时间、子预算不延长父截止时间，并覆盖到期与取消后的回调、锁等资源清理。在完成接线及验证前，不得把调整这两个 YAML 值视为超时已经生效。

---

## 数据流总结

```text
Client ── TCP ──▶ Gate ── gRPC（路由模式经 client_rpc_router）──▶ Login
                   │                                           │
                   │                                           ├─ gRPC → player_locator
                   │                                           └─ gRPC → SceneManager
                   │
Login / SceneManager / player_locator ── Kafka gate 命令 ──────▶ Gate
                   │
                   └─ muduo TCP RPC ──▶ Scene ── NotifyEnterScene / 状态推送 ──▶ Gate
```

## Kafka Topic 与消息映射

下表 `P` 取当前 `command_topic_partitions` 配置，默认值为 256；路由计算使用配置值，不能把 256 视为所有部署的固定分区数。

| Topic | 方向 | 消息 | 用途 |
|-------|------|------|------|
| `gate-cmd_g{generation}`（partition = gateId % P） | Login → Gate | `GateCommand{BindSession}` | 绑定会话 + 携带 enter_gs_type |
| `gate-cmd_g{generation}`（partition = gateId % P） | Login → Gate | `GateCommand{KickPlayer}` | 踢掉旧连接（替换登录） |
| `gate-cmd_g{generation}`（partition = gateId % P） | SceneManager → Gate | `GateCommand{RoutePlayer}` | 分配 sceneNodeId |
| `gate-cmd_g{generation}`（partition = gateId % P） | player_locator → Gate | `GateCommand{PlayerLeaseExpired}` | 超时清理 |

## enter_gs_type 枚举值

| 值 | 名称 | 含义 |
|----|------|------|
| 0 | LOGIN_NONE | 无操作 |
| 1 | LOGIN_FIRST | 首次登录 |
| 2 | LOGIN_REPLACE | 替换登录（顶号） |
| 3 | LOGIN_RECONNECT | 短时重连 |

## 关键设计决策

1. **Login 与 player_locator 管理会话，SceneManager 管理位置/属主**：重连和顶号默认不指定目标区/场景，由 `player:{id}:location` 决定去向；有效重定向票据优先。
2. **EnterGame 成功应答表示已受理**：预加载与入场链继续异步执行，客户端以 NotifyEnterScene 确认入场；链失败会推送进场失败 tip。
3. **Gate 汇合身份与路由**：BindSession 绑定当前 TCP 会话，RoutePlayer 提供场景与属主；通过 `ForwardPlayerToScene` 转发 `PlayerEnterGameNode`。`GateLoginNotify` 是已废弃兼容入口。
4. **RPC 与 Kafka 分工**：Gate 到 Login、Login 到 Locator/SceneManager 走 gRPC，Gate 到 Scene 走 muduo TCP RPC；绑定、路由、踢线等下行命令走 Kafka。
5. **新连接重建 Gate 会话**：同 Gate、跨 Gate 均重新绑定和路由；30s 断线租约不保留旧 SessionInfo，也不保证旧 Scene 实体还在。
6. **入场允许乱序与短暂链路未就绪**：BindSession 与 RoutePlayer 汇合后才转发；Scene 链路/握手未就绪时有界补发，超限向客户端报告失败并收口。
7. **身份栅栏贯穿链路**：登录代次约束迟到入场，owner_epoch 约束跨节点属主；详见 [scene-enter-session-fence.md](./scene-enter-session-fence.md) 与 [cross-zone-scene-travel.md](./cross-zone-scene-travel.md)。

## 场景切换时 Gate 绑定机制

### 核心结论

- 普通切换场景不依赖旧的 `Gate::PlayerEnterGameNode` 回调。
- 当前绑定入口是 `SceneManager -> Kafka gate-cmd_g{generation} -> RoutePlayerEvent -> GateEventHandler::RoutePlayerEventHandler`。
- Gate 更新的是同一条 TCP 会话上的 `SessionInfo(SceneNodeService -> targetNodeId)` 映射，而不是重建 Gate 连接。

### 详细流程

1. 上游业务触发 `SceneManager.EnterScene`。
2. SceneManager 选择目标 Scene 节点并更新玩家位置（Redis 作为位置事实源）。
3. SceneManager 向 `gate-cmd_g{generation}`（partition = gateId % P，P 取 `command_topic_partitions` 配置，默认 256）发送 `GateCommand{RoutePlayer}`（payload 为 `RoutePlayerEvent{session_id,target_node_id}`）。
4. Gate 消费 Kafka 命令并分发 `RoutePlayerEvent`。
5. `RoutePlayerEventHandler` 通过 `session_id` 找到会话，委托 `OnRouteDecision` 汇合身份与路由；向 Scene 成功转发后才整体提交 sceneId 和节点指向，未交出去则进入有界补发。
6. 后续该玩家客户端消息在 Gate 转发时通过 `SessionInfo` 读取新绑定，路由到新 Scene 节点。

### 与 pendingEnterGsType 的关系

- 待入场状态用于汇合 `BindSessionEvent` 与 `RoutePlayerEvent`；处理需要兼容二者到达顺序，不能以某条必先到为前提。
- 普通切场景也由 `OnRouteDecision` / `OnBindSession` 共用的入场派发逻辑决定是否转发，链路未就绪时同样可能进入有界补发。
