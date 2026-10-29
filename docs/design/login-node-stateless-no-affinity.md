# Login Node: Stateless, No Session Affinity

> **2026-10-23 阅读边界**：本文是 2026-04-11 的实例亲和讨论，旧表的服务状态与 Gate 实现不能直接作当前事实。“不需要绑定 login 实例”不等于进程没有预加载、异步等待、排队等在途工作，也不能从本文推导所有 Go 服务都已支持无感维护。当前 db／login／guild／friend 的多实例与停机差异见 [节点生命周期与发布需求 §3.2](node-lifecycle-elasticity-release-20261023.md#32-dbloginguildfriend-的单独结论同日追问补充)。

**Date:** 2026-04-11

## Q&A

### Q: ResolveSessionTargetNode 这么赋值登录消息有问题吗？第一条消息是登录，但这时候并没有赋值对应的登录节点怎么办？

**A:** `ResolveSessionTargetNode` 是懒绑定的。首次调用时 `HasNodeId` 为 false，会走 `PickRandomNode` 从当前 zone 的可用 login 节点中随机选一个，绑到 session 上，后续消息复用这个绑定。所以首次登录消息不需要额外处理。

### Q: login 不是可水平扩展吗？就是玩家的登录消息到哪个登录服上都没事？

**A:** 对。login 节点是完全无状态的，所有状态都外推了：

| 状态 | 存储位置 | 是否依赖特定 login 节点 |
|------|----------|------------------------|
| 登录流程步骤 | Redis (`login_session:{sessionID}`) | 否 |
| 在线/断线状态 | player_locator gRPC 服务 | 否 |
| session 详情 | gRPC 请求元数据 (`x-session-detail-bin`) | 否，每次请求自带 |

disconnect 处理做的两件事：
1. `loginsession.Cleanup()` — 删 Redis key（任何 login 节点都能做）
2. `player_locator.SetDisconnecting()` — 调外部服务设 30s 租约（任何 login 节点都能做）

两步都不依赖 login 节点内存，所以 disconnect 发给任意 login 节点结果完全一样。

### Q: 那能不能不绑定了？

**A:** 可以。既然 login 完全无状态，gate 侧对 `LoginNodeService` 每次都重新 pick 一个即可，不用缓存绑定。这样还有个好处：如果某个 login 节点下线，不需要等 `OnNodeRemovePbEvent` 清除绑定再 re-bind，直接就能负载均衡到存活节点。

### Q: 除了 scene，其他 Go 微服务（公会、好友等）是不是也可以不绑定？

**A:** 对。所有 Go 微服务都是无状态的（状态全在 Redis/MySQL/外部服务），都可以用 `PickRandomNode`：

| 服务 | 节点类型 | 状态存储 | 需要绑定？ |
|------|----------|----------|-----------|
| Login | LoginNodeService (5) | Redis + player_locator | 不需要 |
| Guild | GuildNodeService (15) | Redis + MySQL | 不需要 |
| Friend | FriendNodeService (27) | Redis + MySQL | 不需要 |
| Chat | ChatNodeService (9) | 未实现 | 不需要 |
| Data Service | DataServiceNodeService (26) | Redis | 不需要 |
| Scene Manager | SceneManagerNodeService (25) | Kafka + Redis | 不需要 |
| Player Locator | PlayerLocatorNodeService (24) | Redis | 不需要 |
| **Scene (C++)** | **SceneNodeService (3)** | **内存 ECS 实体** | **需要** |

gate 代码逻辑改为：只有 `SceneNodeService` 走 `ResolveSessionTargetNode`（亲和绑定），其余全部 `PickRandomNode`。

## Decision
Gate does NOT bind login nodes to sessions. Every login/disconnect message picks a random available login node via `PickRandomNode`.

## Gate-Side Implementation
- `HandleConnectionDisconnection`: calls `PickRandomNode(LoginNodeService)` directly
- `HandleGrpcNodeMessage`: checks `targetNodeType == LoginNodeService` → `PickRandomNode`; other types → `ResolveSessionTargetNode` (affinity binding)
- `ResolveSessionTargetNode` is only used for stateful node types (e.g. scene nodes)

## Contrast with Scene Nodes
Scene nodes hold player entities in memory → **must** use session affinity (`ResolveSessionTargetNode` binds on first access, reuses thereafter).
