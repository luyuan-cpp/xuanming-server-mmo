# Gate Load Balancing Design / Gate 负载均衡设计

**Date / 日期:** 2026-03-29

---

## Q&A 记录

### Q1: 怎么让客户端负载均衡地选 Gate？游戏中选择服务器的时候连 Gate 负载均衡一般怎么做？

游戏行业里"选服→连 Gate"的负载均衡常见三种模式：

| 模式 | 流程 | 适用场景 |
|------|------|----------|
| **Login 分配单个 Gate**（最常见） | Client → Login "我选了XX区" → Login 返回**一个** gate 地址+token → Client TCP 连 gate | 大多数 MMO |
| Client 从列表自选 | Client → Login → 返回 gate list → Client 自己挑 | 仅限受信客户端/测试 |
| L4 LB 透传 | Client → NLB/HAProxy → 转发到后端 gate | 所有 gate 无状态、可互换时 |

**推荐做法（Login 分配模式）：** 生产环境应该让 Login 做决策，只返回一个 gate 地址，而不是让客户端拿到列表自己选。原因：

1. **防作弊** — 客户端拿到列表可以故意挤满某个 gate 或绕过限流
2. **连接令牌** — Login 分配 gate 的同时签发一个一次性 token，gate 验证后才允许连接，防伪造/重放
3. **Zone 感知** — 玩家选的是"区服"，一个 zone 可能对应多个 gate，Login 按负载选

### Q2: L4 LB 透传模式能用在这个架构里吗？

**不能。** 我们的 Gate 是**有状态的**：

- `SessionInfo` 持有 `playerId`、节点绑定（`SetNodeId`）、`sessionVersion`
- Session 绑定了具体的 Scene/Login 节点 — 换一个 Gate 没有这个 session 上下文
- 断线通知依赖 `session_id`，必须由同一个 Gate 发出
- Kafka 控制命令路由到 `gate-{gate_id}` — Login/SceneManager 用 gate_id 定向推送

如果 HAProxy 把同一个玩家的重连分到另一个 Gate，那个 Gate 上没有这个 session，所有后续消息都会被丢弃。

| 对比 | 我们的 Gate | L4 LB 要求 |
|------|-----------|-----------|
| 连接状态 | SessionInfo + 节点绑定 | 无 |
| 定向推送 | Kafka `gate-{id}` | 不需要 |
| 断线处理 | Gate 通知 Login | 由 LB 或后端自行感知 |
| 重连 | 必须回到同一 Gate | 任意 Gate 均可 |

L4 LB 只适合无状态网关（如 REST API gateway），不适合 MMO 长连接 Gate。

**结论：Login 分配 + HMAC token 是正确选择。**

---

## Pattern Comparison

| Pattern | Flow | When to Use |
|---------|------|-------------|
| **Login assigns single Gate** (chosen) | Client → Login `AssignGate(zone_id)` → Login returns 1 gate addr + HMAC token → Client TCP connects | Stateful gate (MMO standard) |
| Client picks from list | Client → Login → gate list → Client self-selects | Trusted client / test only |
| L4 LB transparent proxy | Client → NLB/HAProxy → any backend gate | All gates stateless & interchangeable |

## Why L4 LB Doesn't Fit This Architecture

Our Gate is **stateful**:
- `SessionInfo` holds `playerId`, node bindings (`SetNodeId`), `sessionVersion`
- Session is bound to specific Scene/Login nodes — another Gate has no session context
- Disconnect notification depends on `session_id` from the originating Gate
- Kafka control commands route to `gate-{gate_id}` — Login/SceneManager push to a specific gate

If an L4 LB routes a reconnect to a different Gate, that Gate has no session → all messages silently dropped.

L4 LB only works when gates are fully stateless (e.g., REST API gateway doing pure protocol translation with no per-connection state). Incompatible with MMO long-connection architecture.

## Chosen Design: Login Assigns + HMAC Token

### Requirements Satisfied
1. **Anti-cheat**: Client never sees the gate list. Login decides which gate.
2. **Connection token**: Login signs `GateTokenPayload` (HMAC-SHA256). Gate verifies before allowing any game messages.
3. **Zone-aware**: `AssignGate(zone_id)` filters gates by zone, picks least-loaded.

### Flow
```
Client                    Login                    Gate
  │  gRPC AssignGate(zone_id)  │                      │
  │──────────────────────────>│                      │
  │                           │ pick least-loaded gate│
  │                           │ sign HMAC token       │
  │  {ip, port, token, deadline}                      │
  │<──────────────────────────│                      │
  │                                                   │
  │  TCP connect                                      │
  │──────────────────────────────────────────────────>│
  │  ClientTokenVerifyRequest{payload, signature}     │
  │──────────────────────────────────────────────────>│
  │                           verify HMAC + gate_node_id + expiry
  │  ClientTokenVerifyResponse{success=true}          │
  │<──────────────────────────────────────────────────│
  │  ClientRequest (game messages now allowed)        │
  │──────────────────────────────────────────────────>│
```

### Token Details
- **Payload**: `GateTokenPayload { gate_node_id, zone_id, expire_timestamp }` (protobuf)
- **Signature**: HMAC-SHA256(shared_secret, serialized_payload) → hex string
- **TTL**: 60 seconds (configurable in Login)
- **Shared secret**: `GateTokenSecret` in Login YAML / `gate_token_secret` in C++ `BaseDeployConfig`
- **Dev mode**: Empty secret → Gate auto-verifies all connections (no token required)

### Gate Enforcement
- `SessionInfo.verified` flag (default `false`)
- `DispatchTokenVerify`: validates HMAC, gate_node_id, expiry → sets `verified = true`
- `DispatchClientRpcMessage`: rejects + disconnects unverified sessions
- Dev mode: `HandleConnectionEstablished` auto-sets `verified = true` when secret is empty

### Files Touched
- `proto/login/login.proto` — `AssignGate` RPC, `AssignGateRequest/Response`
- `proto/common/base/message.proto` — `GateTokenPayload`, `ClientTokenVerifyRequest/Response`
- `proto/common/base/config.proto` — `gate_token_secret` field
- `go/login/internal/logic/pregate/` — `AssignGate` logic with HMAC signing
- `go/login/internal/config/config.go` — `GateTokenSecret` field
- `go/login/etc/login.yaml` — `GateTokenSecret` config
- `cpp/nodes/gate/handler/rpc/client_message_processor.cpp` — token verify handler + enforcement
- `cpp/libs/services/gate/session/comp/session_info_comp.h` — `verified` flag
- `bin/etc/base_deploy_config.yaml` — `GateTokenSecret` config
- `robot/main.go` — `assignGate()` + token handshake
- `robot/pkg/client.go` — `VerifyGateToken()` method

---

## 2026-09-29 更正(按磁盘代码核对;原文保留在上方,不删)

### 1. 客户端入口是 Java gateway 的 HTTP,不是直连 login 的 gRPC;gateway 只做透传

- 上面 Flow 图里 `Client → Login: gRPC AssignGate(zone_id)` 已过时。现状:客户端发 HTTP `POST /api/assign-gate` 到 Java gateway
  (`java/gateway_node/.../controller/AssignGateController.java`),gateway 的 `AssignGateService` 在限流与 zone 准入(维护 / 开关区)之后,
  **原样透传**给 go-zero login 的 `LoginPreGate.AssignGate`,把回包 1:1 映射成 HTTP DTO
  (`java/gateway_node/src/main/java/com/game/gateway/service/AssignGateService.java:20-48` 类注释、`:86-115` `assignGate`)。
  gateway **不选 gate、不签票**:它原先那份"选最低负载 gate + HMAC 签名"的 Java 副本已在 2026-05 登录排队改造时删除,只剩 login 一处权威实现。
- 选 gate 与签票的唯一实现在 login(`go/login/internal/logic/pkg/loginqueue/gatetoken.go`):选 gate 是 `PickGate`(`:84`,最低负载),
  签票是 `SignGateToken`(`:113`),`PickAndSignGateToken`(`:68`)只是两者合一的便捷包装。三条调用路径
  (入口均为 `go/login/internal/logic/loginpregate/assigngatelogic.go:60` `AssignGate`,排队轮询另有 `QueryQueueStatus`):
  1. 快路径(不排队):`signFastPath`(`assigngatelogic.go:191`)在 `:207` 调 `PickAndSignGateToken`,选与签一次完成;
  2. 排队路径:zone 满员时回 `QUEUEING`,dispatcher 在 admit 时只 `PickGate`(`pkg/loginqueue/dispatcher.go:262`,不签),
     客户端轮询 `/api/queue-status`(经 gateway 透传给 `LoginPreGate.QueryQueueStatus`,`proto/login/login.proto:218-228`),
     消费 admit 时才 `SignGateToken`(`loginpregate/querystatuslogic.go:68`),让票据 TTL 从兑现时起算;
  3. 排队重入:客户端带 queue token 再调 `AssignGate` 时走 `handleReentry`(`assigngatelogic.go:151`),
     `Lookup` 到已 admit 的槽位同样在消费时 `SignGateToken`(`:160`),gate 沿用 dispatcher 选定的那台、不重新选。
  (2026-09-29 更正:此处原写"`AssignGate` → `gatetoken.go:52` `PickAndSignGateToken`",只覆盖了快路径,且 `:52` 是函数注释起点。)
- 上方「Files Touched」里的 `go/login/internal/logic/pregate/` 路径已不存在,现为 `loginpregate/`。
- Java 侧 `GateWatcher` 不参与选 gate,有两个用途:① gate / scene 查询(`fetchAllGateNodes` / `fetchAllSceneNodes`,
  `java/gateway_node/src/main/java/com/game/gateway/etcd/GateWatcher.java:53`、`:63`)只供 `ZoneHealthProbeService` 判断 zone 健康与在线数
  (`service/ZoneHealthProbeService.java:76-77`);② 它同时是 `LoginNodeDiscovery` 读 login 节点的 etcd 客户端
  (`fetchAllLoginNodes`,`GateWatcher.java:76`;`grpc/LoginNodeDiscovery.java:37`、`:44`、`:58`,只用 `endpoint`)。
  两处都不读 `client_endpoint`;`NodeInfoRecord` 标了 `@JsonIgnoreProperties(ignoreUnknown = true)`,NodeInfo 新增字段不影响它。
  集群外入口因此不需要改 Java 代码(ingress D91 只加 Ingress 与 trusted proxies)。

### 2. login 下发给客户端的 gate 地址(集群外入口 D76–D78,未上集群)

- login 从 etcd 读 gate 的 NodeInfo,候选集由纯函数 `buildGateCandidates` 生成(`go/login/internal/svc/servicecontext.go:342`):
  1. 按 zone 过滤;2. `clientendpoint.Select`(`go/shared/clientendpoint/select.go:41`)选客户端地址 —— `client_endpoint` 可用(ip 非空且 port 在 1..65535)就用它,
  否则 `RequireClientEndpoint=false` 时回落 `endpoint`(podip 形态,即 PodIP),`true` 时跳过该 gate;3. 按选中的地址 `DedupeNewest`,同一地址只留
  `launch_time` 最大的一条(gate 崩溃后旧记录残留到 NodeTTL 时,客户端会被新 gate 以 `token_gate_node_mismatch` 拒绝);4. 过滤正在排空的 gate
  (`servicecontext.go:309-314`,`FilterDrainingGates`,配合 `tools/scripts/k8s_gate_drain.ps1`)。
- scene_manager 的跨 zone `RedirectToGate` 用同一规则(`go/scene_manager/internal/logic/gate_redirect.go:125` `selectGateTargets`)。
- `RequireClientEndpoint` 由部署脚本写进 login / scene_manager 的 ConfigMap,`auto` 跟随 `-ClientEntryMode`(external → true)。

### 3. Q2 的结论在 K8s 上的具体化

- 「L4 LB 透传不适用」在 K8s 上意味着:**gate 入口必须每实例一个**。票据绑 `gate_node_id`(`proto/common/base/message.proto:219-220`),
  gate 不符即断开(`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:1052-1057`)。单一 `gate-entry` Service 只在 podip 且单副本时生成(D90);
  集群外部署用 gate StatefulSet + 每序号 Service `gate-<i>`,见 `docs/design/k8s_gate_exposure_guidance.md` 与 `docs/design/k8s-client-entry.md`(D87–D90)。
- 以上为静态核对结论,相关 Go / Java 单测与 kind 端到端均**未由本文档批运行**,待 Codex 验证。
