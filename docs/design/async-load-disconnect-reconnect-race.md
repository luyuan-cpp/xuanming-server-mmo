# 异步加载竞态：Redis 加载期间的断线与重连

## 概述

玩家进入 Scene 节点时，其数据通过 `MessageAsyncClient::AsyncLoad` 从 Redis 异步加载。
在这段异步窗口内（通常为 1–20 毫秒），可能发生两种竞态：

1. **加载期间断线**：加载完成前收到 `ExitGame`。
2. **断线后立即重连**：第一次加载尚未完成时，又收到第二次 `PlayerEnterGameNode`。

这两种情况都必须处理，避免出现幽灵实体、玩家卡住以及 Gate 的 WARN 日志。

## 架构：PendingEnterMap

尚未完成的加载所需的会话和场景上下文，保存在由 `PlayerLifecycleSystem` 管理的
`thread_local PendingEnterMap`（`unordered_map<Guid, PlayerGameNodeEntryInfoComp>`）中。

此前，这些信息保存在 `MessageAsyncClient` 的 `std::any extra_data` 中。新设计如下：

- **通用 Redis 客户端不包含业务逻辑**：`MessageAsyncClient` 只负责按键异步加载值。
- **重连即覆盖**：`PendingEnterMap[player_id] = newInfo` 自然覆盖为重连后的上下文，
  无需提供 `UpdateExtraData()` 接口。
- **不再使用类型擦除**：移除 `std::any_cast` 分支和不再使用的 `PlayerSceneEnterContext` 结构。

Redis 客户端之外的两张映射表协同工作：

- `SessionMap`（`unordered_map<SessionId, Guid>`）：用于路由，并以映射是否存在表达取消状态。
- `PendingEnterMap`（`unordered_map<Guid, PlayerGameNodeEntryInfoComp>`）：保存加载上下文。

## 流程 1：正常异步加载（无竞态）

```
t0  PlayerEnterGameNode(session_A, player_id)
    -> RemovePlayerSessionSilently(player_id)         // 无操作（玩家尚未在线）
    -> SessionMap[session_A] = player_id              // 预先登记路由
    -> PendingEnterMap[player_id] = enterInfo_A       // 保存上下文
    -> AsyncLoad(player_id)                           // 发起 Redis GET

t1  Redis 返回结果
    -> HandlePlayerAsyncLoaded(player_id, data)
    -> PendingEnterMap.find(player_id) -> enterInfo_A // 取出上下文并从映射表中删除
    -> SessionMap.find(session_A) -> 找到             // 会话仍然有效
    -> InitPlayerFromAllData(data, enterInfo_A)
      -> EnterScene -> HandleEnterScene
        -> 给玩家挂 SceneEntityComp 绑定到场景，并加入 AOI 网格
        -> SendToPlayerClient(EnterSceneS2C)  // 客户端收到响应
```

## 流程 2：异步加载期间断线（取消）

```
t0  PlayerEnterGameNode(session_A)
    -> SessionMap[session_A] = player_id
    -> PendingEnterMap[player_id] = enterInfo_A
    -> AsyncLoad(player_id)

t1  客户端断线 -> Gate 发送 ExitGame(session_A)
    -> ProcessClientPlayerMessage
    -> SessionMap.find(session_A) -> 找到，但玩家实体 == null
    -> msg == ExitGame -> SessionMap.erase(session_A)  // 发出取消信号

t2  Redis 返回结果
    -> HandlePlayerAsyncLoaded(player_id, data)
    -> PendingEnterMap.find(player_id) -> enterInfo_A  // 找到上下文
    -> SessionMap.find(session_A) -> 未找到            // 已取消
    -> 记录日志并返回                                 // 不创建实体
```

**结果**：不会产生幽灵实体，也不会向已经删除的 Gate 会话发送消息。

## 流程 3：异步加载期间断线后立即重连

这是关键竞态。核心在于 `AsyncLoad` 按键去重：同一 `player_id` 的加载尚未完成时，
第二次调用会被静默忽略。而覆盖 `PendingEnterMap` 可以确保回调使用最新的会话信息。

```
t0  PlayerEnterGameNode(session_A, player_id)
    -> SessionMap[session_A] = player_id
    -> PendingEnterMap[player_id] = enterInfo_A
    -> AsyncLoad(player_id)                           // 开始 Redis GET

t1  客户端断线 -> ExitGame(session_A)
    -> SessionMap.erase(session_A)                    // 取消 session_A

t2  客户端重连 -> PlayerEnterGameNode(session_B, player_id)
    -> RemovePlayerSessionSilently(player_id)         // 无操作（尚未创建实体）
    -> SessionMap[session_B] = player_id              // 登记新会话
    -> PendingEnterMap[player_id] = enterInfo_B       // 覆盖 enterInfo_A
    -> AsyncLoad(player_id)                           // 去重：静默跳过
                                                      // （loading_queue_ 已包含该键）

t3  Redis 返回结果（对应 t0 发起的 GET；无论会话如何变化，加载的数据相同）
    -> HandlePlayerAsyncLoaded(player_id, data)
    -> PendingEnterMap.find(player_id) -> enterInfo_B // 最新信息（session_B）
    -> SessionMap.find(session_B) -> 找到             // 会话有效
    -> InitPlayerFromAllData(data, enterInfo_B)
      -> EnterScene(session_B) -> HandleEnterScene
        -> 使用 session_B 将玩家绑定到场景
        -> SendToPlayerClient(EnterSceneS2C)  // 客户端 B 收到响应
```

**结果**：重连后的客户端成功进入场景。无需再次发起 Redis GET，尚未完成的同一次 GET
即可服务这两次请求。覆盖 `PendingEnterMap` 正是将加载结果“转交”给新会话的机制。

## 为什么覆盖 PendingEnterMap 能生效

`AsyncLoad` 按 `player_id` 对应的 Redis 键去重。加载尚未完成时，
第二次 `PlayerEnterGameNode` 到达后的处理如下：

1. `PendingEnterMap[player_id] = enterInfo_B` 覆盖旧条目。
2. `AsyncLoad(player_id)` 发现 `loading_queue_` 已有该键，立即返回。
3. Redis GET 回调触发时，调用 `HandlePlayerAsyncLoaded(player_id, data)`。
4. 处理函数查询 `PendingEnterMap[player_id]`，得到 `enterInfo_B`。
5. `SessionMap.find(session_B)` 成功，因此使用 `session_B` 创建实体。

`PendingEnterMap` 与 `loading_queue_` 相互解耦：

- `loading_queue_` 控制 Redis 网络调用，同一玩家同时只发起一次 GET。
- `PendingEnterMap` 保存最新会话上下文，每次重连都会更新。

## 机器人测试

`robot/login_test_scenarios.go` 中的 `testRapidDisconnectReconnect`：

1. 准备：完成一次登录并正常退出，确保 Redis 中已有玩家数据。
2. 循环执行 3 次：
   - 登录并调用 `EnterGame`，触发 Scene 上的异步加载。
   - 直接断开连接，不发送 `LeaveGame`，触发服务端的 `ExitGame`。
   - 立即重连，不加延迟，可能命中异步加载窗口。
   - 再次登录并调用 `EnterGame`，必须成功，且客户端必须收到 `EnterSceneS2C`。
   - 正常退出。
3. 三轮循环都必须完成，不能出现 STUCK 超时。

## 修改文件

| 文件 | 改动 |
|------|--------|
| `redis_client.h` | 移除 `std::any extra_data`、`UpdateExtraData()`、`EventCallbackWithExtra`、`load_callback_with_extra_`。`AsyncLoad` 只接收 `key`。 |
| `player_lifecycle.h` | 新增 `PendingEnterMap` 类型别名和 `GetPendingEnterMap()`。`HandlePlayerAsyncLoaded` 只接收 `(Guid, PlayerAllData&)`。 |
| `player_lifecycle.cpp` | `HandlePlayerAsyncLoaded` 从 `PendingEnterMap` 读取上下文。移除不再使用的 `PlayerSceneEnterContext`。 |
| `redis.cpp` | 用 `SetLoadCallback` 替代 `SetLoadCallbackWithExtra`。 |
| `scene_handler.cpp` | `PlayerEnterGameNode`：覆盖 `PendingEnterMap` 前清理其中记录的旧会话，再调用 `AsyncLoad(player_id)`。`ProcessClientPlayerMessage`、`SendMessageToPlayer`、`InvokePlayerService` 三条消息分发路径均增加会话不匹配的防御检查。 |
| `login_test_scenarios.go` | 新增 `testRapidDisconnectReconnect` 测试场景。 |

## 流程 4：重连完成后旧 ExitGame 才到达

如果没有会话不匹配检查，旧连接迟到的 `ExitGame` 可能让已经重连的玩家被下线：

```
t0  PlayerEnterGameNode(session_A)
    -> SessionMap[A]=pid, PendingEnterMap[pid]=infoA, AsyncLoad(pid)

t2  PlayerEnterGameNode(session_B)  （重连，ExitGame 尚未到达）
    -> RemovePlayerSessionSilently(pid) -> 无操作（玩家尚不在 playerList 中）
    -> SessionMap[B]=pid              // 此时 SessionMap 同时包含 {A:pid, B:pid}
    -> PendingEnterMap: 从 SessionMap 删除旧 session_A  <-- 修复点（主动清理）
    -> PendingEnterMap[pid]=infoB     // 覆盖
    -> AsyncLoad(pid)                 // 去重

t3  Redis 返回结果 -> 使用 session_B 创建实体

t1  迟到的 ExitGame(session_A) 到达
    -> SessionMap.find(A) -> pid
    -> 玩家实体已存在
    -> snapshot.gate_session_id()==session_B != session_A  <-- 修复点（防御检查）
    -> 识别为旧会话，删除 SessionMap[A]，丢弃消息
    -> 玩家实体保留
```

两层防护：

1. **主动清理**（`PlayerEnterGameNode`）：覆盖 `PendingEnterMap` 前，检查旧条目的
   `session_id` 是否不同；若不同，则从 `SessionMap` 中删除旧会话。
2. **防御检查**（`scene_handler.cpp` 中的三条消息分发路径）：找到玩家实体后，校验
   `PlayerSessionSnapshotComp.gate_session_id() == sessionId`。不匹配说明消息来自已被替换的连接，
   删除过期的 `SessionMap` 条目并丢弃消息。覆盖以下入口：
   - `ProcessClientPlayerMessage`：经 Kafka 路由的客户端消息。
   - `SendMessageToPlayer`：经 gRPC 路由的 Gate 转发消息。
   - `InvokePlayerService`：gRPC 玩家服务调用。

> 2026-10-16 补（RpcChannel 批 7）：`SendMessageToPlayer`（Scene 1）与 `InvokePlayerService`（Scene 40）已退役并从 proto 删除
> （号留墓碑，见 `docs/design/muduo-rpc-channel.md` §20.3），不再派发任何消息，也就不存在旧会话消息经它们误投的问题；
> 现存的消息分发路径只剩 `ProcessClientPlayerMessage`。上面「修改文件」表与第 2 层防护里涉及这两个入口的描述只作留痕。
