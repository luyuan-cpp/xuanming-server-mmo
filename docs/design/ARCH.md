# MMO 服务端架构总览 (ARCH.md)

**Date:** 2026-05-08 · **Updated:** 2026-09-29(§1 拓扑补 battle 与客户端第二条连接;§2 / §6 更正 gate↔scene 传输与路由模式默认值;§10 / §11 补 battle 直连收缩与集群外入口)
**Status:** Living document — 总入口,串联各专题文档

> 本文是**架构索引 + 关键决策记录**,不重复造轮子。
> 每个子模块有独立深度文档,本文负责告诉你"为什么这么做、去哪儿看细节"。

---

## 目录

1. [总体拓扑](#1-总体拓扑)
2. [关键约束与设计原则](#2-关键约束与设计原则)
3. [登录链路:从客户端到 Gate](#3-登录链路从客户端到-gate)
4. [Token 体系(三层)](#4-token-体系三层)
5. [Gate:有状态长连接层](#5-gate有状态长连接层)
6. [游戏内通信:Kafka + gRPC 解耦](#6-游戏内通信kafka--grpc-解耦)
7. [第三方登录(QQ/微信/SaToken/...)](#7-第三方登录qq微信satoken)
8. [开服削峰与容量保护](#8-开服削峰与容量保护)
9. [运维:内核调优与压测基线](#9-运维内核调优与压测基线)
10. [文档索引(按主题)](#10-文档索引按主题)
11. [关键决策摘要](#11-关键决策摘要)

---

## 1. 总体拓扑

```
                     ┌─────────────────────────────────────────────────────────┐
                     │                  客户端 (Unity / etc.)                   │
                     └─────────────────────────────────────────────────────────┘
                          │ HTTPS                  │ ① TCP 大厅长连接       │ ② TCP 战斗直连
                          │ (短请求)               │ (登录到登出,1 条)      │ (参战/观战期间,每局 1 条;
                          ▼                        │                         │  首包出示 battle 自签票据)
┌─────────────────────────────────────────────┐    │                         │
│  Java Gateway  (Spring Boot + Sa-Token)     │    │                         │
│                                             │    │                         │
│  · /api/server-list  (zone 列表 + 推荐)     │    │                         │
│  · /api/assign-gate  (选 gate + HMAC token) │    │                         │
│  · /api/announce/*   /admin/*               │    │                         │
│  · /api/cdn/sign     /api/hotfix/check      │    │                         │
│                                             │    │                         │
│  Filter Chain:                              │    │                         │
│    RateLimiter (Bucket4j+Redis)             │    │                         │
│    Sa-Token Auth Interceptor                │    │                         │
└─────────────────────────────────────────────┘    │                         │
        │ gRPC AssignGate                          │                         │
        ▼                                          │                         │
┌─────────────────────────────────────────────┐    │                         │
│  go-zero login (gRPC)                       │    │                         │
│  · AssignGate (HMAC 签 GateTokenPayload)    │    │                         │
│  · Login (Auth Provider 校验)               │◀───┼─── 第三方登录链路       │
│  · EnterGame (loginstep + RedisLocker)      │    │                         │
│  · RefreshToken (双 token 续签)             │    │                         │
└─────────────────────────────────────────────┘    │                         │
   │ gRPC                       │ Kafka 命令       │                         │
   ▼                            ▼                  ▼                         │
┌──────────┐              ┌─────────────┐  ┌──────────────────┐              │
│player_   │              │ scene_      │  │  C++ gate(muduo) │              │
│locator   │◀────权威─────│ manager     │  │  · HMAC 验 token  │              │
│(Redis)   │   会话源     │ (gRPC)      │  │  · 纯转发层       │              │
└──────────┘              └─────────────┘  │  · session 状态   │◀──────┐      │
                                            └──────────────────┘       │      │
                                               │  Kafka 大厅公告       │      │
                          muduo TCP RPC(同区)  │  (battle→gate)        │      │
                                               ▼                       │      ▼
                                            ┌──────────────────┐   ┌───┴───────────────────────┐
                                            │  C++ scene(muduo)│   │  C++ battle(muduo)        │
                                            │  ECS 玩家实体     │◀──│  · 回合制对局房间,全局池   │
                                            └──────────────────┘   │  · 本地验签票据,不连玩家库 │
                                              Kafka scene 命令     └───────────────────────────┘
                                              topic(battle 结算回流)       ▲ gRPC CreateBattle /
                                                                           │ AddObserver / IssueBattleTicket …
                                                                    ┌──────────────┐
                                                                    │ Go match     │
                                                                    │ (全局池)     │
                                                                    └──────────────┘
```

> **(2026-09-29 更正)** 原图 gate → scene 的边标的是「gRPC / Kafka」,不对:gate 按 muduo TCP RPC 连本 zone 的 scene(scene 以 `PROTOCOL_TCP` 注册,`node.cpp:534-535`;gate 出站说明 `gate/main.cpp:183`),详见 §6.1 更正。原图也没有 battle 节点和客户端第二条连接,本次补上。图中「Kafka 大厅公告」一线从 battle 框顶边引到 gate 框(battle → gate);scene ↔ gate 之间只有 muduo TCP RPC 一条边。图中 topic 按现行口径写成「命令 topic」:battle 经 `node::kafka::ResolveCommandRoute` 发往共享分区 topic `gate-cmd_g<N>` / `scene-cmd_g<N>`,分区 = 目标 node_id % P(`node_command_route.h:49-68`;`battle_room_manager.cpp:91-94`、`:169-171`;[control-plane-topic-partitioning-20260908.md](./control-plane-topic-partitioning-20260908.md))。本文其余处的 `gate-{id}` / `scene-{id}` 是沿用的旧记法(迁移窗口里的一节点一 topic 旧名,`node_command_route.h:56-60`)。
>
> 补图说明(均未编译、未上集群验证,待 Codex 验证):
> - **客户端连接数**:平时 1 条 gate 长连接;参战或观战期间 +1 条 battle 直连(每局一条,重连替换旧连接)。直连是战斗**唯一**通路:目标为 `BattleNodeService` 的客户端消息经 gate 发来时,gate 在分派前统一回 `kServiceUnavailable`(`client_message_processor.cpp:937-950`,两种路由模式相同,turn-based §22 D66);下行战斗帧无活直连即丢弃(`battle_push_policy.h:21-48`,D68)。跨 zone 传送是**替换** gate 连接,连接数不变。HTTP 短请求(server-list / assign-gate / queue-status / 可选 `/api/login` / refresh-token)不算长连接。
> - **gate 与 battle 之间没有连接**(gate 出站白名单两种模式都不含 `BattleNodeService`:白名单代码 `gate/main.cpp:216-219`,说明注释 `:199-203`)。battle → gate 只剩 Kafka gate 命令 topic(`gate-cmd_g<N>`,按 gate node_id 定分区;`battle_room_manager.cpp:91-94`)上的大厅公告:`NotifyBattleAssigned`(带票据)、`NotifyBattleStart`,且只在该玩家没有活直连时才回落到这条路(`PushLobbyAnnouncement`,`battle_room_manager.cpp:1410`)。scene 发的 `NotifyBattleReconnect` 与结算后的 `NotifyBattleEnd` 是 scene 自己的下行,同样经大厅。
> - **battle 的客户端入口地址**由 `client_endpoint::ClientFacing` 选取(`client_endpoint.h:100-109`,`battle_room_manager.cpp:1452-1464`,集群外入口 D78):`NodeInfo.client_endpoint`(=11)可用就用它;否则 `CLIENT_ENDPOINT_REQUIRED` 未开(required=false)时回落 `NodeInfo.endpoint`——默认 podip 形态(`CLIENT_ENDPOINT_SOURCE=none`)根本不自报 client_endpoint,走的就是这条回落(`client_endpoint.h:16-17`、`:90-91`);required=true 或连 endpoint 都不可用时拒签(D78 / D79,调用方按 D70 fail-closed)。static / agones 形态下进程在发布 etcd 前自报 client_endpoint(D76 / D79,`node.cpp` `Node::InitClientEndpoint`);K8s 集群外形态为 Agones Fleet `portPolicy: Dynamic`(D81),见 [k8s-client-entry.md](./k8s-client-entry.md)。

**三层语言分工**

| 层 | 技术栈 | 职责 |
|---|---|---|
| HTTP 门户 / 运维 | **Java Spring Boot + Sa-Token** | 公告、热更、CDN 签名、服务器列表、运维后台、SDK 鉴权 |
| 游戏内 RPC | **Go (go-zero)** | 登录、场景管理、数据服务、玩家定位、好友、公会 |
| 运行时节点 | **C++ (muduo)** | gate(长连接)、scene(战斗逻辑) |
| 对局节点(2026-09-29 补) | **C++ (muduo)**:客户端面 muduo TCP,控制面 gRPC | battle:回合制对局房间,全局池、纯内存、不连玩家库、可随时被杀;客户端凭 battle 自签票据直连(第二条连接);match 经 gRPC 建房 / 加观众 / 补签票据;结算经 Kafka 回原 scene。传输选型见 [battle-transport-decision.md](./battle-transport-decision.md),实现见 [turn-based-battle-server.md](./turn-based-battle-server.md) |

> 注(2026-09-29):上表「scene(战斗逻辑)」指 scene 内的实时技能 / 战斗系统;回合制对局已由独立的 battle 节点承载,scene 只负责备战冻结、抽快照与应用结算(turn-based D1)。

详见: [java-gateway-portal-decision.md](./java-gateway-portal-decision.md)

---

## 2. 关键约束与设计原则

### 硬约束
1. **muduo 框架代码不动** — gate 的优化只在业务层 + 配置 + 内核层
2. **MMO/MOBA 延迟敏感** — 游戏 TCP 链路**零中间代理**(不挂 LVS / Nginx / HAProxy)
3. **gate 是有状态的** — 不能用 L4 LB 做 gate 后向均衡(原因见 [gate-load-balancing-design.md](./gate-load-balancing-design.md))

### 设计原则
1. **登录的"重活"在 Gateway / login,不在 gate** — gate 只做 HMAC 验签和会话转发
2. **会话权威源:`Login + player_locator`** — Centre 节点已删除([player_login_flow.md](./player_login_flow.md))
3. **Gate 是纯转发层** — `BindSession` 绑定会话,`RoutePlayer` 绑定场景,`ForwardLoginToScene` 通知 scene
4. **Kafka 解耦** — Gate / Login / SceneManager / player_locator 全部通过 Kafka 异步通信,无 full-mesh gRPC 流
5. **gate 只连一类 gRPC 目标**(2026-09-05,[client-rpc-router.md](./client-rpc-router.md))— 客户端可见的 gRPC 类消息由 gate **原样**转给无状态 Go 路由服 `client_rpc_router`,路由服按生成的路由表以原始字节转发到 login / match / chat / …;gate 的 gRPC 连接数 = 路由服副本数,与业务服务数量无关,加业务服务不再改 gate、不重编、不重启。`GATE_CLIENT_RPC_ROUTER=1` 开启,默认仍是旧的逐服务直连(expand→migrate→contract)。战斗流量不经 gate、不经路由服:客户端凭票据直连 battle 节点([turn-based-battle-server.md §18](./turn-based-battle-server.md))
   - **(2026-09-29 更正)默认值分两层,「默认仍是旧的逐服务直连」只对 C++ 进程成立**([xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md) D-12 及其 2026-09-29 修订):
     - **C++ 进程默认直连**:`gate_router_mode.h:35-55` 未设变量或非 `1/true/on` 一律直连,`gate_security_test.cpp:160` 钉死;这是灰度开关拼错时的安全兜底,不改。
     - **部署默认路由模式**:本地 `start_game.ps1 -GateRouterMode` 默认 `'1'`(`start_game.ps1:22`);K8s `k8s_deploy.ps1 -GateRouterMode` 默认 `"1"`(`k8s_deploy.ps1:163-164`,turn-based §22 D75,2026-09-29 起;此前为 `"0"`)。`dev_tools.ps1` / `k8s_image.ps1` / `k8s_zone_rollback.ps1` 的同名参数留空即不覆盖。
     - chat / guild / trade 等新服务只承诺路由模式可达;以 `"0"` 回退运行的 zone 每次重新部署都要显式再传 `-GateRouterMode 0`(回退态不粘滞)。
     - 战斗与本开关无关:两种模式下 gate 都不中继战斗(turn-based §22 D66)。K8s 路由模式 battle-smoke 至今未跑(D65 豁免、事后补验),**未编译、未测试,待 Codex 验证**。
5. **失败语义统一** — 所有锁走 `RedisLocker`(UUID + Lua CAS,见 [login-simplification-2026-04.md](./login-simplification-2026-04.md))

---

## 3. 登录链路:从客户端到 Gate

### 3.1 完整 8 步

```
1. Client → GET  /api/server-list                 → Java Gateway
2. Client → POST /api/assign-gate {zone_id}       → Java Gateway
3.                  Java Gateway → gRPC AssignGate → go-zero login
4.                  login: 选最空 gate + HMAC 签名 GateTokenPayload
5. Client ← {gate_ip, gate_port, gate_token, deadline}
6. Client → TCP connect Gate
7. Client → ClientTokenVerifyRequest{payload, signature}    → Gate 验 HMAC
8. Client → Login(auth_type, auth_token, ...)               → go-zero login (EnterGame)
                                                             ↓
                              player_locator + scene_manager + Kafka 编排
                                                             ↓
                              Gate 收到 BindSessionEvent / RoutePlayerEvent
                                                             ↓
                              Scene 收到 GateLoginNotify(enter_gs_type)
                                                             ↓
                              玩家进游戏循环
```

### 3.2 EnterGame 决策(LOGIN_FIRST / RECONNECT / REPLACE)

由 `DecideEnterGame()` 根据 `player_locator.GetSession` 判定四种 `enter_gs_type`:

| 值 | 名称 | 触发条件 |
|---|---|---|
| 1 | LOGIN_FIRST | 无历史会话 |
| 2 | LOGIN_REPLACE | 已在线但不同 gate/session(顶号) |
| 3 | LOGIN_RECONNECT | 30s 内 Disconnecting |
| 0 | LOGIN_NONE | 异常,拒绝 |

**详细事件流(含同 gate / 跨 gate 重连差异、超时清理):** [player_login_flow.md](./player_login_flow.md)

### 3.3 并发安全

两层保证同玩家串行:
1. `player_locker:{playerId}` 分布式锁(`RedisLocker`,UUID+Lua CAS)
2. Kafka `KeyOrderedKafkaProducer`(playerId 一致性哈希到同一分区,FIFO)

详见: [login-simplification-2026-04.md](./login-simplification-2026-04.md)

---

## 4. Token 体系(三层)

```
Layer 1: 首次鉴权 (WeChat OAuth / QQ Connect / Sa-Token；或显式启用的 MySQL Argon2id password)
              │
              │ 仅首次登录,验过一次
              ▼
Layer 2: Access Token (2h) + Refresh Token (30d)
              │
              │ 玩家断线/换设备,凭 access 静默续会话
              ▼
Layer 3: Gate Token (HMAC-SHA256, 300s)
              │
              │ 一次性 TCP 握手凭证
              ▼
         TCP 长连接 (Gate)
```

**三个 token 的职责完全正交**:
- 第三方 token:解决"你是谁"(账号体系)
- Access/Refresh:解决"无需重新走 OAuth"(类似 WeChat 的 session_key + wx.login)
- Gate token:解决"防止伪造 TCP 连接"(HMAC,gate 本地验签,**不查 Redis 不查 DB**)

**完整字段、Redis Key、TTL、安全属性:** [dual-token-authentication.md](./dual-token-authentication.md)

**Gate token 的 HMAC 协议、enforcement、dev mode:** [gate-load-balancing-design.md](./gate-load-balancing-design.md) §Token Details

---

## 5. Gate:有状态长连接层

### 5.1 为什么 gate 不能用 L4 LB 做后向均衡

| 维度 | 我们的 Gate | L4 LB 要求 |
|---|---|---|
| 连接状态 | `SessionInfo`(playerId + 节点绑定 + sessionVersion) | 无状态 |
| 定向推送 | Kafka `gate-{gateId}` 路由 | 不需要 |
| 断线感知 | session_id 必须由原 Gate 发出 | LB 或后端自感知 |
| 重连 | **必须回到同一 Gate**(SessionInfo 在那) | 任意 Gate 均可 |

→ **所以选 "Login 分配 + HMAC token"**,而不是 LVS DR / Nginx。详见 [gate-load-balancing-design.md](./gate-load-balancing-design.md)

### 5.2 gate 上的会话状态

```
SessionInfo {
  playerId        : 玩家 ID
  sessionVersion  : 单调递增,顶号判定
  verified        : HMAC 验过
  nodeIds[]       : 绑定的下游节点(scene/login/...)
  pendingEnterGsType : 暂存 enter_gs_type,等 RoutePlayer 到达再 forward
}
```

### 5.3 端口 / 性能调优(已实测)

```
压测对比 (扩 ephemeral port + tcp_tw_reuse 后)

login_fail   3%   →  0       (端口耗尽消除)
avg_login    414ms→  78ms    (5.5x)
max_login    25s  →  1.2s    (~20x,长尾消失)
SynSent      1540 →  0       (内核排队清零)
Bound        15470→  2704    (5.7x 缓解)
```

完整 sysctl 配置 + runbook: 见 [docs/ops/](../ops/)(待写,任务 #9)

---

## 6. 游戏内通信:Kafka + gRPC 解耦

### 6.1 核心原则:控制面走消息总线,数据面走直连 RPC

整个游戏内通信按"是否延迟敏感"分两个平面,各用最合适的传输:

| 平面 | 路径 | 传输 | 理由 |
|---|---|---|---|
| **游戏热路径**(客户端包 ↔ scene) | C++ Gate ↔ C++ Scene | ~~**直连 gRPC**(lazy 建连 + channel pool + 同区)~~ **(2026-09-29 更正)muduo TCP RPC 直连**(RpcCodec,同区;scene 以 `PROTOCOL_TCP` 注册,`node.cpp:534-535`;gate 出站说明 `gate/main.cpp:183`、两种路由模式白名单都含 `SceneNodeService` `:216-219`) | 延迟敏感,要常驻有状态低延迟通道 |
| **控制面**(RoutePlayer / Kick / Bind / LeaseExpired / Redirect) | Go 服务 → `gate-{gateId}` topic → Gate | **Kafka** | 低频、fire-and-forget,要解耦防连接爆炸 |
| **业务反向推送**(好友/公会 server→client) | Go 服务 → Kafka → Gate → client TCP | **Kafka** | 异步通知,非实时 |
| **客户端业务请求**(login / match / chat / friend / guild …) | client TCP → Gate → `client_rpc_router` → 目标 Go 服务 | **gRPC unary,原始字节透传**(gate 只连路由服一类;`GATE_CLIENT_RPC_ROUTER=1`,见 [client-rpc-router.md](./client-rpc-router.md))。**(2026-09-29)** 部署层默认路由模式(本地 `start_game.ps1`、K8s `k8s_deploy.ps1` 的 `-GateRouterMode` 均默认 `"1"`),C++ 进程默认直连,见 §2 原则 5 更正与 [xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md) D-12 修订 | 请求/应答语义,一跳亚毫秒;gate 不持任何业务 stub,加服务不碰 gate |
| **对局战斗**(回合制 battle) | client TCP **直连** battle 节点(票据入场) | **TCP,零字节经 gate**([turn-based-battle-server.md §18](./turn-based-battle-server.md));**(2026-09-29 精确)** 战斗上下行(四条战斗 RPC 及应答、握手后的全部战斗帧)零字节经 gate,battle 与大厅之间只剩 Kafka 大厅公告(见下一行),gate 两种路由模式都不中继战斗(turn-based §22 D66);match 的大厅 RPC(补签 `MatchService.RequestBattleTicket`,应答带完整 `BattleAssignedS2C`;观战 `WatchBattle`)按设计走「客户端业务请求」一行的 gate 路径,不算战斗流量(turn-based §22.1) | 战斗服可随时 kill;gate 与 battle 之间无连接 |
| **对局大厅公告**(2026-09-29 补) | battle → Kafka gate 命令 topic(`PushToPlayerEvent`;现行名 `gate-cmd_g<N>`、按 gate node_id 定分区,`battle_room_manager.cpp:91-94`,[control-plane-topic-partitioning-20260908.md](./control-plane-topic-partitioning-20260908.md);`gate-{gateId}` 为本文沿用的旧记法)→ Gate → 大厅连接 | **Kafka,battle → gate 只剩这一条** | 只承载 `NotifyBattleAssigned`(带票据)与 `NotifyBattleStart`,且仅在该玩家没有活直连时回落(turn-based §22 D68,`battle_push_policy.h:46-48`);战斗帧、观战帧、终局无直连即丢弃。scene 发的 `NotifyBattleReconnect` 与结算后 `NotifyBattleEnd` 属 scene 自己的下行(scene → gate → 大厅连接)。Kafka `BindBattleEvent` / `UnbindBattleEvent` 已从 `proto/contracts/kafka/gate_event.proto` 删除(:98-100,D67);事件号 43/44 永不复用,重生时由生成器改写成 `N=reserved:<原名>` 墓碑(`event_id.go:59`;截至本次 `proto/event_id.txt:44-45` 尚未重生)。未编译、未测试,待 Codex 验证 |

### 6.2 Kafka topic 路由(控制面)

> **(2026-09-29 注)** 下表与本节的 `gate-{gateId}` 是一节点一 topic 的旧记法,原文保留。现行生产端(C++ 经 `node::kafka::ResolveCommandRoute`,Go 经 `go/shared/kafkacmd`)发往共享分区 topic `gate-cmd_g<N>` / `scene-cmd_g<N>`,分区 = 目标 node_id % P;旧名只在迁移窗口里仍被消费(`node_command_route.h:42-68`,[control-plane-topic-partitioning-20260908.md](./control-plane-topic-partitioning-20260908.md))。

| Topic | 方向 | 消息 | 用途 |
|---|---|---|---|
| `gate-{gateId}` | Login → Gate | `GateCommand{BindSession}` | 绑定会话 + enter_gs_type |
| `gate-{gateId}` | Login → Gate | `GateCommand{KickPlayer}` | 顶号踢人 |
| `gate-{gateId}` | SceneManager → Gate | `GateCommand{RoutePlayer}` | 分配 scene 节点 |
| `gate-{gateId}` | SceneManager → Gate | `GateCommand{RedirectToGate}` | 跨区切 Gate 重定向 |
| `gate-{gateId}` | player_locator → Gate | `GateCommand{PlayerLeaseExpired}` | 30s 重连超时清理 |
| `gate-{gateId}` | Friend/Guild → Gate | `PushToPlayerEvent` / `BroadcastToPlayersEvent` | 业务反向推送 |

### 6.3 为什么控制面用 Kafka 而不是 gRPC stream(决策定论)

**这些控制/推送消息本就不该用 stream,正因为要避免网状长连接。**

如果把 RoutePlayer/Kick/Push 改成 gRPC stream,意味着每个 Go 服务(login、scene_manager、player_locator、friend、guild)都要对**每一个 Gate** 维持一条常驻流 → 这正是要避免的 N×M 长连接爆炸(40,000 Gate 规模下不可接受),严格比 Kafka 更差。Kafka 在这里给出:

- **异步解耦** — Go 服务不需要知道/连接具体哪个 Gate 实例,只发 `gate-{gateId}`
- **顺序保证** — 同 playerId 走同一 partition(`KeyOrderedKafkaProducer`)
- **重启韧性** — Gate 重启后从 offset 继续消费,消息不丢;配合 `target_instance_id` 做防僵尸过滤
- **扇出** — `gate-{gateId}` 命名模式天然支持横向扩 Gate

而 bidi-stream 真正该用的地方——常驻、有状态、有序、低延迟的点对点通道——已经用在 C++ Gate↔Scene 的直连 gRPC 上,与 go-zero/kratos 框架选型无关。

> **(2026-09-29 更正)** 上一句的事实部分不对:C++ Gate↔Scene 走的是 **muduo TCP RPC**(RpcCodec 长连接,同区),不是 gRPC(`gate/main.cpp:183`;scene 以 `PROTOCOL_TCP` 注册,`node.cpp:534-535`;[battle-transport-decision.md](./battle-transport-decision.md) §8.6 已登记这处漂移)。结论不变:需要「常驻、有状态、有序、低延迟点对点通道」的地方用的是这条自有 TCP 长连接,Go 控制面仍无需 stream;对局战斗则走客户端 ↔ battle 直连,也不经 gRPC stream。

### 6.4 不要为 stream 迁移到 kratos(否决)

- **前提是误解**:go-zero 跑在标准 `grpc-go` 之上,**运行时完全支持** server/client/bidi streaming;只是 `goctl` 脚手架面向 unary,流式需在底层 `grpc.Server` 上自行注册(`zrpc` 提供 `AddOptions` / 自定义 register 钩子)。
- **代价极大收益≈0**:迁移 kratos 需重写所有服务启动 / 配置 / 中间件 / 服务发现 / etcd 集成,而要解决的"拿不到 stream"问题并不存在。
- **结论**:维持 go-zero。控制面继续 Kafka,游戏热路径继续直连 gRPC。
  - **(2026-09-29 更正)**「游戏热路径继续直连 gRPC」应为「游戏热路径继续 **muduo TCP RPC 直连**」(gate ↔ scene,见 §6.1 更正);对局战斗是客户端 ↔ battle 的 muduo TCP 直连。结论「维持 go-zero、不为 stream 迁 kratos」不受影响。

### 6.5 后续可选演进(非对错问题)

- 若嫌 Kafka 对"一条路由信令"偏重(延迟几 ms~几十 ms、运维偏重),控制面可评估更轻量的 **NATS**(或 Redis pub/sub):解耦相同、延迟更低、运维更简单。**代价**:当前用 Kafka retention 做的 `target_instance_id` 防僵尸需自行补齐。
- 若某些控制消息需要同步 ack(如"踢人是否成功"),让 Gate 回一个 `gate-event` 事件即可,**不需要 stream**。

详见 [player_login_flow.md](./player_login_flow.md) §数据流总结、[gate-scene-relay-architecture](连接爆炸与解法)。

---

## 7. 第三方登录(QQ/微信/SaToken/...)

### 7.1 已实现的 Provider

| auth_type | 实现 | 状态 | 账号映射 |
|---|---|---|---|
| `password` | `ProductionPasswordProvider`（MySQL Argon2id） | ✅ 已实现、默认关闭 | 每次查询权威 `user_accounts`；禁止直接采信 account。存量库须先跑 schema 与哈希迁移；开发机器人另走前缀+环境密钥门 |
| `satoken` | `SaTokenProvider` | ✅ | 查 SaToken Redis key |
| `wechat` | `WeChatProvider` | ✅ | `wx_<unionid\|openid>` |
| `qq` | `QQProvider` | ✅ | `qq_<unionid\|openid>` |
| `netease` | `NeteaseProvider` | 🚧 stub |  |

→ 框架 + 配置 + 新增 provider 的步骤: [auth-provider-framework.md](./auth-provider-framework.md)

### 7.2 微信/QQ 端到端流程

```
Client(SDK) → WeChat/QQ OAuth → 拿到 code/access_token
            │
            ▼
Client → POST /api/login
        { auth_type: "wechat", auth_token: <code> }
            │
            ▼
   Java Gateway → gRPC Login → go-zero login
            │
            ▼
   WeChatProvider.Validate
       (调 https://api.weixin.qq.com/sns/oauth2/access_token
        → 拿 unionid/openid)
            │
            ▼
   account = "wx_<unionid>" → 走通用 EnterGame 流程
            │
            ▼
   返回 access_token + refresh_token + 角色列表
```

### 7.3 Sa-Token 在体系中的角色

Sa-Token **不直接服务 C++ gate**,只在 Java Gateway 这一层发挥作用:
- 运维后台(`/admin/*`)用 `@SaCheckLogin` / `@SaCheckRole` 注解保护
- HTTP API 限流/拦截走 Sa-Token Filter
- `SaTokenProvider` 复用 SaToken 已签发的 token 给游戏侧账号系统(用于 Java 内部其他业务带 token 调用 login)

**Sa-Token 颁发的 token 不下发给 gate**,gate 只认第 3 层 HMAC token。两套体系互不耦合。

---

## 8. 开服削峰与容量保护

### 现状(已落地)
- `RedisLocker` 防同账号并发登录冲突
- `KeyOrderedKafkaProducer` 防同玩家消息乱序
- `db_task_zone_*` 的 partition 数在同一 `TopicGeneration` 内不可变；扩容必须停写排空并切新 topic，见 [DB Task Kafka 分区不可变契约](./db-task-kafka-partition-contract.md)
- `loginstep` 状态机防非法状态转移

### 缺口(任务 #10)
开服 5 万人同时点登录时,**需要 Java Gateway 层做削峰**:

```
1. /api/assign-gate 加 Bucket4j+Redis 全局令牌桶 (e.g. 2000 token/s)
2. 拿不到令牌 → 返回 { code: QUEUEING, queue_pos, retry_after }
3. 客户端 UI 显示"前面还有 N 人",定时重试
4. 按 zone 分批放人(T+0s zone1/2 / T+30s zone3/4 ...)
```

为什么放在 Gateway 而不是 login: Gateway 是 HTTP 短连接、Java 限流生态成熟、削峰失败也不影响游戏中玩家。

### 容量预估

| 组件 | 单实例承载 | 横向扩 | 备注 |
|---|---|---|---|
| Java Gateway | 1万 RPS | 2-3 实例 | 限流在这层 |
| go-zero login | 1.5万 QPS | 2-3 实例 | etcd 注册 |
| Redis | 10万 QPS | 主从 | token / locker / locator |
| gate | 5万长连接 | 按玩家数 | muduo 单进程 |

---

## 9. 运维:内核调优与压测基线

### sysctl 基线(待固化到 `/etc/sysctl.d/99-gate.conf`)

```ini
net.ipv4.ip_local_port_range     = 1024 65535
net.ipv4.tcp_tw_reuse            = 1
net.ipv4.tcp_max_tw_buckets      = 1048576
net.ipv4.tcp_fin_timeout         = 15
net.core.somaxconn               = 65535
net.ipv4.tcp_max_syn_backlog     = 65535
```

⚠️ 不要开 `tcp_tw_recycle`(NAT 环境会出问题,4.12+ 内核已删除)。

### 压测拐点排查 SOP

撞墙先看哪个先到顶:
1. 客户端: `ulimit -n` / `nf_conntrack_max` / 软中断 `%si`
2. 网络: NAT/LB 会话表 / SLB 单实例 CPS
3. 服务端: accept queue (`ListenOverflows`) / worker 池 / GC

**Bound + SynSent 同时高 = 端口耗尽**(已通过扩 port range 验证)。

详见任务 #9 / #2 待出文档。

---

## 10. 文档索引(按主题)

### 登录与会话
- [player_login_flow.md](./player_login_flow.md) — 登录/重连/顶号/超时清理事件流(权威)
- [auth-provider-framework.md](./auth-provider-framework.md) — 第三方登录扩展框架
- [dual-token-authentication.md](./dual-token-authentication.md) — Access + Refresh 双 token
- [login-simplification-2026-04.md](./login-simplification-2026-04.md) — FSM→loginstep / RedisLocker
- [login-gate-assignment-migration.md](./login-gate-assignment-migration.md) — Gateway 演进
- [login-test-anti-stuck-system.md](./login-test-anti-stuck-system.md) — 测试卡死防护
- [async-load-disconnect-reconnect-race.md](./async-load-disconnect-reconnect-race.md) — 加载/断线竞态
- [login-node-stateless-no-affinity.md](./login-node-stateless-no-affinity.md) — login 无状态化
- [login-queue-2026-05.md](./login-queue-2026-05.md) — AssignGate 真排队(Redis ZSET + dispatcher leader)

### Gate 与连接
- [gate-load-balancing-design.md](./gate-load-balancing-design.md) — gate 负载均衡设计 + L4 LB 排除
- [gate-scene-relay-architecture.md](./gate-scene-relay-architecture.md) — Gate↔Scene 中继
- [gate_client_high_water_mark.md](./gate_client_high_water_mark.md) — 高水位反压
- [gate-entity-id-truncation-fix.md](./gate-entity-id-truncation-fix.md) — entity id 截断修复
- [k8s_gate_exposure_guidance.md](./k8s_gate_exposure_guidance.md) — K8s 暴露
- [java-gateway-portal-decision.md](./java-gateway-portal-decision.md) — Java Gateway 选型决策
- [client-rpc-router.md](./client-rpc-router.md) — 客户端 RPC 路由服:gate 唯一的 gRPC 目标(2026-09-05)
- [battle-transport-decision.md](./battle-transport-decision.md) — battle 传输选型定谳(客户端面 muduo TCP 直连、控制面 gRPC);§8 回答客户端几条连接、battle 为什么不经 gate、scene 为什么仍经 gate(2026-09-29)
- [k8s-client-entry.md](./k8s-client-entry.md) — gate / battle 集群外客户端入口(D76–D93:进程自报 `client_endpoint`、battle Agones Fleet、gate StatefulSet + 每序号 Service、Ingress、kind 端到端验证;2026-09-29,同批文档新建)

### 对局 / 战斗
- [turn-based-battle-server.md](./turn-based-battle-server.md) — 回合制 battle 节点设计与决策(§18 票据直连;§22 直连收缩 D65–D75)
- [moba-battle-target-architecture.md](../notes/slg-moba/moba-battle-target-architecture.md) — 会话制对局目标形态与本仓现状映射、验收判据
- [cross-zone-matchmaking.md](./cross-zone-matchmaking.md) — 全服跨 zone 匹配、match 全局池与票据自愈

### 跨服与场景
- [cross_server_architecture_principle.md](./cross_server_architecture_principle.md)
- [mmo_cross_server_architecture.md](./mmo_cross_server_architecture.md)
- [cross_scene_player_messaging.md](./cross_scene_player_messaging.md)
- [scene-creation-architecture.md](./scene-creation-architecture.md)
- [scene-grpc-server-design.md](./scene-grpc-server-design.md)
- [scene-node-threading-model.md](./scene-node-threading-model.md)
- [enter-scene-zone-routing.md](./enter-scene-zone-routing.md)

### 数据与持久化
- [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md)
- [data_service_role_and_scope.md](./data_service_role_and_scope.md)
- [db_write_behind_dirty_flag_race.md](./db_write_behind_dirty_flag_race.md)
- [db_zone_isolation.md](./db_zone_isolation.md)
- [data-consistency-stress-testing.md](./data-consistency-stress-testing.md)

### 节点 / Snowflake / 服务发现
- [snowflake-id-allocation.md](./snowflake-id-allocation.md)
- [snowflake-guard-and-node-conflict.md](./snowflake-guard-and-node-conflict.md)
- [snowflake-node-id-lease-recycling.md](./snowflake-node-id-lease-recycling.md)
- [node_id_conflict_design.md](./node_id_conflict_design.md)
- [node-removal-grace-period.md](./node-removal-grace-period.md)

### 部署与运维
- [docker_k8s_build_deploy.md](./docker_k8s_build_deploy.md)
- [gateway-k8s-deployment.md](./gateway-k8s-deployment.md)
- [rolling-update-restart-resilience-tests.md](./rolling-update-restart-resilience-tests.md)
- [docs/ops/k8s-open-server-runbook.md](../ops/k8s-open-server-runbook.md)
- [docs/ops/k8s-docker-desktop-troubleshooting.md](../ops/k8s-docker-desktop-troubleshooting.md)

### 玩法 / ECS / SLG
- [ecs.md](./ecs.md) / [ecs-component-access-rules.md](./ecs-component-access-rules.md)
- [slg-server-architecture-design.md](../notes/slg-moba/slg-server-architecture-design.md)
- [aoi_priority_design.md](./aoi_priority_design.md)

### 压测 / 调优
- [stress-test-progress.md](../stress/stress-test-progress.md)
- [cpp_image_optimization.md](./cpp_image_optimization.md)
- [hashed-timing-wheel.md](./hashed-timing-wheel.md)

---

## 11. 关键决策摘要

| # | 决策 | 何时定的 | 文档 |
|---|---|---|---|
| 1 | Centre 节点删除,Login + player_locator 是会话权威源 | 2026-04 | [player_login_flow.md](./player_login_flow.md) |
| 2 | Java(Spring Boot) 做统一 HTTP Gateway | 2026-04-14 | [java-gateway-portal-decision.md](./java-gateway-portal-decision.md) |
| 3 | Gate 不能挂 L4 LB,改"Login 分配 + HMAC token" | 2026-03 | [gate-load-balancing-design.md](./gate-load-balancing-design.md) |
| 4 | 双 token (Access 2h + Refresh 30d) 对齐微信/QQ 标准 | 2026-04-20 | [dual-token-authentication.md](./dual-token-authentication.md) |
| 5 | 第三方登录 Provider 化,新增渠道仅改配置 | 2026-04 | [auth-provider-framework.md](./auth-provider-framework.md) |
| 6 | login 服务从 FSM 简化为 `loginstep` + `RedisLocker` | 2026-04 | [login-simplification-2026-04.md](./login-simplification-2026-04.md) |
| 7 | Kafka `gate-{gateId}` topic 解耦控制流 | 2026-04 | [player_login_flow.md](./player_login_flow.md) |
| 8 | muduo 框架代码不动,优化只在业务层 + 内核 | 持续 | 本文 §2 |
| 9 | **首次 Login 上移到 Java Gateway `/api/login`**,OAuth 校验、限流、排队都在 HTTP 层;cpp gate 的 `ClientPlayerLogin.Login` 保留兼容(带 deprecation 日志) | **2026-05-08** | [open-server-rate-limit-design.md](./open-server-rate-limit-design.md), [third-party-login-end-to-end-design.md](./third-party-login-end-to-end-design.md) |
| 10 | **`/api/refresh-token` 独立 HTTP 通道**,token 续签不再占用 gate TCP | **2026-05-08** | [dual-token-authentication.md](./dual-token-authentication.md) |
| 11 | **cpp gate→login gRPC 加 HTTP/2 keepalive**(30s/10s/permit-without-calls=1),防 NAT/LB 空闲踢连接 | **2026-05-08** | [gate-grpc-long-connection-audit-2026-05.md](./gate-grpc-long-connection-audit-2026-05.md) |
| 12 | **gate 的 `HandleGrpcNodeMessage` 保留为"协议路由器"**,登录只是它转发的 RPC 之一(EnterGame/LeaveGame/CreatePlayer/Disconnect 必须经它)| **2026-05-08** | [gate-login-rpc-boundary.md](./gate-login-rpc-boundary.md) |
| 13 | **Gateway 限流**:Bucket4j + Redis,三层叠加(zone/ip/account cooldown)+ 开服波次 | **2026-05-08** | [open-server-rate-limit-design.md](./open-server-rate-limit-design.md) |
| 14 | **AssignGate 真排队**:权威源放 go-zero login(不是 Java Gateway),Redis ZSET 做有序 FIFO + 单 leader dispatcher;Java AssignGateService 删本地签 HMAC,改成 gRPC 转发;Bucket4j 保留作为前置闸,两层互补 | **2026-05-14** | [login-queue-2026-05.md](./login-queue-2026-05.md) |
| 15 | **Server-list 走 OSS + CDN 静态发布**:玩家读路径不经过 Java Gateway / MySQL;运维改 zone 后异步生成 json 推 OSS + purge CDN;Gateway `/api/server-list` 仅作降级兜底(Caffeine 30s + Bucket4j);客户端三档:CDN 主 → CDN 备 → Gateway。zone 数据仍存 MySQL `zone_config`,**不进 Excel** | **2026-05-23** | [serverlist-static-publish-2026-05.md](./serverlist-static-publish-2026-05.md) |
| 16 | **`db_task_zone_{ZoneId}` partition 5→10**:解掉 2026-05-25 §N 基线的 db worker 池容量瓶颈;实测拐点从"开服打开就崩"上移到 **25k conn 之内 100% 干净 / 25k–45k 逐步降级**(单 zone smoke);AUTHORITY 在 `login.yaml`(EnsureTopics),`db.yaml` 是 MIRROR,两处必须同步改否则 worker/partition skew;下一瓶颈不再是 partition 而是 `entergamelogic.go` 里 `player_locker:{playerId}` 的 120s TTL 滞留(异步链没归还锁 + robot 6s 重连节奏命中)— 不是 PreloadPool 池满(实测 dropped=0)| **2026-05-27** | [stress-1zone-45k-2026-05-partition-10.md](../stress/stress-1zone-45k-2026-05-partition-10.md) |
| 17 | **dispatcher GC tick + dispatcherTaskTTL 联动修复**:用 prometheus histogram 把 EnterGame 异步链全段拆开做实验,定位真凶在 `dataloader_preload_callback_wait`(平均 3 秒,失败 35 秒);三连改动 `dispatcherTaskTTL 30s→5s` + 仪表化 12 个子阶段 + dispatcher GC tick `defaultTTL/2→1s`(原值让 5s TTL 实测变成 12s,因为 sweep 间隔被 ttl 整除而非按 entry 算);单 zone 25k smoke 从 "T+5m 拐点 → T+21m 雪崩冻死" 变成 **23 分钟 client 端打满 125k conn 上限 / robot 视角 0 失败**。**警示**:robot 视角"全成功"有水分,login 后台仍 46% preload_failed,靠 scene-side Redis NIL retry 兜底;下一步要在 db_rpc consumer 端继续打点 | **2026-05-28** | [stress-1zone-25k-2026-05-28-callback-wait.md](../stress/stress-1zone-25k-2026-05-28-callback-wait.md) |
| 18 | **46% 后台 preload_failed 深挖**:发现真凶不在 login 而在 db 端 — `MaxOpenConn=10` 配 partition=10 = MySQL 连接池满载,稳态 200/s 输入下单 partition 排队 ~9k task / 实测 Kafka lag 91k;scene-side `kMaxLoadRetries=6` + 指数退避 `2/4/8/16/32/60s` = 122s 兜底窗口让 robot 看不见失败,但 robot enter_ok 是 RPC 同步成功不等 scene-ready;失败 preload 时锁正常释放、session 保留、Gate/Scene 不知道玩家来过;审了 6 处其它陷阱(lock heartbeat��saveToRedis 失败、同 playerId in-flight、sub_cache 部分命中、TaskResult LPop、batch coalesce)。**下次先动 `MaxOpenConn 10→30`**,加 db_rpc 子阶段打点验证 | **2026-05-28** | [stress-1zone-25k-2026-05-28-deep-dive.md](../stress/stress-1zone-25k-2026-05-28-deep-dive.md) |
| 19 | **MaxOpenConn 10→30 实测解一半**:`preload{success} avg` 从 5.14s 暴跌到 **34.6ms**(降 99.3%),fail% 从 46% 降到 29%。但 Kafka backlog 仍 80k —— 反转:db worker 串行才是真天花板,**不是 MySQL 连接池**。深挖 §1 诊断对一半:连接池是 latency 瓶颈,但 throughput 瓶颈在 `worker.start` 单 goroutine 处理 batch 的循环里(`partition=10 × 1 worker × 1 MySQL conn = 10 在用,20 个永远闲着`)。下次试 partition 10→20(简单可逆),A 方案是 worker 内 sub-shard 并行(30 行+单测) | **2026-05-28** | [stress-1zone-25k-2026-05-28-maxopenconn.md](../stress/stress-1zone-25k-2026-05-28-maxopenconn.md) |
| 20 | **Worker sub-shard(方案 A)**:`worker.start` 改造成 router goroutine + `SubShardCount=4` 个并行 `runSubShard` goroutine,按 `hash(task.Key) % N` 路由,保 per-key 顺序。10×4=40 路实际并发。**实测:robot 23 分钟全跑 0 失败 + max_login 209ms(Round 6: 571ms,-64%) + Kafka final lag 4,233(Round 6: 80,055,-95%) + db consumer throughput ~190/s(Round 6: 73/s,+160%) + entergame fail% 17.8%(Round 6: 29%)**。同时把 stress 复盘脚本化:`tools/scripts/stress_summarize.ps1` 直接吃 RunDir + prom snapshots 出 2KB 二维表,以后压测复盘只读它输出 | **2026-05-28** | [stress-1zone-25k-2026-05-28-subshard.md](../stress/stress-1zone-25k-2026-05-28-subshard.md) |
| 21 | **全区全服数据层 + TiDB**:玩家数据层收敛为单一 TiDB 集群(v8.5 LTS),按 player_id 组织,home_zone 表达逻辑归属;跨区 = 客户端 redirect 重连 + 全局层直读(废弃 player_migrate 的数据搬运职责);合服 = RemapHomeZoneForMerge 零迁移。硬前提:snowflake 主键建表必须 NONCLUSTERED + SHARD_ROW_ID_BITS(写热点)、`txn-entry-size-limit` ≥32MB(16MB 存档默认必炸)、proto2mysql 升新版须逐表锁表名。partition 契约与 L1-L4 验收体系不变 | **2026-08-15** | [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) |
| 22 | **battle 直连收缩 + 集群外入口一次落地**(用户拍板「一次全做,事后验」,豁免「K8s 路由模式 battle-smoke 先跑通」前提):① gate 两种路由模式都不中继战斗,直连是战斗唯一通路,battle → gate 只剩 Kafka 大厅公告(Assigned / Start,无直连才回落),删 Kafka Bind/Unbind 契约(事件号墓碑不复用),D26 改 fail-closed(签不出票不建房),scene 换会话只推重连提示(D65–D74);② K8s `-GateRouterMode` 默认翻 `"1"`,C++ 进程默认仍直连(D75 / D-12 修订);③ 集群外入口:进程自报 `NodeInfo.client_endpoint`(=11;仅 static / agones 形态,默认 podip 不自报、回落 endpoint),login / scene_manager / battle 三出口同一选择规则,battle = Agones Fleet `portPolicy: Dynamic` + 分配许可与排空标签,gate = StatefulSet + 每序号 Service(D76–D93)。**全部未编译、未测试、未上集群,待 Codex / 用户验证** | **2026-09-29** | [turn-based-battle-server.md](./turn-based-battle-server.md) §22, [k8s-client-entry.md](./k8s-client-entry.md), [battle-transport-decision.md](./battle-transport-decision.md), [xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md) D-12 修订 |

---

## 12. 老 Login 路径下线计划

**当前状态**(2026-05-09):
- 新路径(`POST /api/login` → Gateway → gRPC login)已上线并通过 34 个单元+集成测试
- `POST /api/refresh-token` 独立 HTTP 通道同样上线,robot `runTokenRefresher` 默认走 HTTP(`cfg.GatewayAddr` 非空时)
- 老路径(客户端 → gate TCP → `ClientPlayerLogin.Login` RPC)继续工作,每次命中记录计数 + 每 60s 打一条 throttled warn
- robot 新增 `use_http_login` 开关(默认 false,灰度打开)
- 全栈端到端压测通过(50/100/200/500 bots × 30s,0 fail / 0 stuck,avg 69-101 ms)—— 详见 [stress-test-2026-05-http-login.md](../stress/stress-test-2026-05-http-login.md)
- GateWatcher 过滤 `allocated/*` etcd key,消除每 5s 一次的 NodeInfo JSON 解析告警

**下线步骤**:

| 阶段 | 目标日期 | 动作 | 退出条件 |
|---|---|---|---|
| **T+0 灰度** | 2026-05 | robot / 内测客户端 `use_http_login: true`,Gateway 限流默认关,观察日志 | 新路径登录成功率 ≥ 老路径;`[DEPRECATION]` 每小时计数稳定 |
| **T+1 全量切换** | 2026-06(下个版本) | 线上客户端默认走 `/api/login`;老路径保留但 Gateway 在 zone 层面可灰度关闭 | `[DEPRECATION]` 计数降到 <1%(只有老客户端零星兜底) |
| **T+2 下线** | 2026-07(下下个版本) | 老 gate Login RPC 加 feature flag `legacy-gate-login-enabled`,默认关闭;所有计数归 0 后考虑删除 | 两个星期无老路径调用 |
| **T+3 代码移除** | 2026-08 | `HandleGrpcNodeMessage` 里删除对 `ClientPlayerLoginLoginMessageId` 的路由;`deprecation.go` 删除;`message_id.txt` 保留但不再注册 handler | — |

**不会删的**(必须保留的 gate→login 转发):
- `CreatePlayer` / `EnterGame` / `LeaveGame` / `Disconnect` / `RefreshToken`(参见 [gate-login-rpc-boundary.md](./gate-login-rpc-boundary.md))

**监控**:
- go-zero login 的 `legacyLoginCount` / `newLoginCount` 原子计数器
- Gateway `/api/login` 的 Prometheus metric `gate_assign_total{result}`
- robot 压测里 `LoginFail` / `AccessReconnectFallback` 比率

---

## 当前已知缺口(待补)

按任务清单跟踪:
- **#10**: Java Gateway 加 Bucket4j 限流 + 排队(防开服风暴)— ✅ Bucket4j 2026-05-08 + 真排队 2026-05-14 已落地
- **#11**: QQ/微信 provider 端到端联调 + 账号映射 — 🚧 文档完成,待真跑
- **#13**: 核查 gate→login gRPC 是否长连接复用(压测 Bound 残留)— ✅ 2026-05-08 已核查
- **#9**: 内核调优 runbook + 压测 SOP 落地 — ✅ 文档完成,待 sysctl 固化到线上
- **#2**: 压测报告 45k→120k 阶梯加压拐点分析 — 🚧 待真跑
- **#8**: CLAUDE.md 项目记忆刷新 — ✅ 已更新

---

**维护者注**:
- 任何架构变更请先更新本文 §11 表,然后在对应专题文档展开
- 新增文档请挂到 §10 索引
- 决策有反复时,旧决策**保留**并标 `Superseded by #N`
- 新人 / AI 第一天先读 [onboarding.md](./onboarding.md)(跑起来)→ 本文(看架构)→ [player_login_flow.md](./player_login_flow.md)(看登录细节)
