# Gate ↔ Login RPC 边界 (2026-05)

**Date:** 2026-05-08
**起因:** 加完 Java Gateway `/api/login` 后,有人会问"那 cpp gate 是不是不用再连 login 了?`HandleGrpcNodeMessage` 是不是可以砍?"——本文回答**不可以**,并梳理清楚每条 RPC 走哪条路。

---

## TL;DR

```
登录前(玩家在登录界面)        登录后(玩家在游戏 TCP 上)
─────────────────────────     ──────────────────────────────
Client → Java Gateway          Client → cpp gate
       → gRPC login                   → gRPC login   ← HandleGrpcNodeMessage
                                      → TCP scene    ← HandleTcpNodeMessage
                                      → gRPC friend/chat/guild/... ← HandleGrpcNodeMessage
```

`HandleGrpcNodeMessage` 不只服务登录,**所有玩家在游戏 TCP 上需要打到 Go 微服务的 RPC 都走这里**。砍掉等于游戏内一切跨服务调用都没了。

---

## 一、cpp gate 的两条转发路径

```cpp
// cpp/nodes/gate/handler/rpc/client_message_processor.cpp
void RpcClientSessionHandler::DispatchClientRpcMessage(...) {
    ...
    auto &messageInfo = gRpcMethodRegistry[request->message_id()];
    if (messageInfo.protocol == PROTOCOL_TCP) {
        HandleTcpNodeMessage(session, request, sessionId, conn);   // → scene (有状态,session affinity)
    } else if (messageInfo.protocol == PROTOCOL_GRPC) {
        HandleGrpcNodeMessage(sessionId, request, conn);           // → 任意 Go 微服务 (无状态,随机节点)
    }
}
```

`protocol` 字段从 proto 文件的 service annotation 派生(`OptionIsClientProtocolService = true`)。当前标了这个 annotation 的 service:

| service | 文件 | 协议 | gate 转发到 |
|---|---|---|---|
| `ClientPlayerLogin` | `proto/login/login.proto` | gRPC | go-zero login |
| `ClientPlayerCommon` | `proto/scene/client_player_common.proto` | TCP | C++ scene |
| `ClientPlayerMovement` | `proto/scene/player_movement.proto` | TCP | C++ scene |
| `ClientPlayerScene` | `proto/scene/player_scene.proto` | TCP | C++ scene |
| `ClientPlayerSkill` | `proto/scene/player_skill.proto` | TCP | C++ scene |
| `Client*Chat`(实际名以 proto 为准) | `proto/chat/chat.proto` | gRPC | Go chat |

→ **登录类 RPC 全部从客户端经 gate 走 gRPC 路径打到 go-zero login**,这是设计本意。

---

## 二、`ClientPlayerLogin` 6 个 RPC 各自走哪条路

```protobuf
// proto/login/login.proto
service ClientPlayerLogin {
  option (OptionIsClientProtocolService) = true;
  rpc Login        (LoginRequest)               returns (LoginResponse);          // ① 首次登录
  rpc CreatePlayer (CreatePlayerRequest)        returns (CreatePlayerResponse);   // ② 创角
  rpc EnterGame    (EnterGameRequest)           returns (EnterGameResponse);      // ③ 选角进游戏
  rpc LeaveGame    (LeaveGameRequest)           returns (LoginEmptyResponse);     // ④ 退到角色选择
  rpc Disconnect   (LoginNodeDisconnectRequest) returns (LoginEmptyResponse);     // ⑤ 内部用
  rpc RefreshToken (RefreshTokenRequest)        returns (RefreshTokenResponse);   // ⑥ token 续签
}
```

注册到 message_id.txt:

```
14=ClientPlayerLoginCreatePlayer        ← gate 转发
17=ClientPlayerLoginLeaveGame           ← gate 转发
26=ClientPlayerLoginEnterGame           ← gate 转发
48=ClientPlayerLoginLogin               ← gate 转发(auth_type=access_token:新流程绑定会话,必经;口令 / 第三方:老路径,兼容)
58=ClientPlayerLoginDisconnect          ← gate 内部触发(玩家 TCP 断开时)
127=ClientPlayerLoginRefreshToken       ← 应走 Java Gateway,见下文
```

### ① `Login` (message_id=48):**三种入口**
| 路径 | 何时用 | 谁调用 | 写 `login_session` | 受 `LegacyGateLoginEnabled` 管 |
|---|---|---|---|---|
| **新路径(推荐):Java Gateway `/api/login`** | 玩家在登录界面首次登录 / 静默重登 | Java Gateway → gRPC login(不带会话) | 否 | 否 |
| **gate 上 `Login{auth_type:"access_token"}`** | 新流程第二步:连上 gate 后绑定会话;也用于 access_token 重连 | client → gate → gRPC login | **是(唯一写入点)** | **否** |
| 老路径:gate 上用口令 / 第三方凭据首认证 | 兼容老客户端 / robot 测试用 | client → gate → gRPC login | 是 | 是 |

新客户端**应该走 Java Gateway** 做认证,因为:
- 登录还没拿到 gate 地址,逻辑上不该已经连上 gate
- HTTP 层好做限流 / 排队 / 风控(见 [open-server-rate-limit-design.md](./open-server-rate-limit-design.md))
- OAuth code 失败可在 HTTP 401 阶段返回,不用先建 TCP

但 HTTP 入口不带会话、**不写 `login_session`、不登记设备集**。客户端连上 gate 后仍要在 TCP 上发一次
`Login{auth_type:"access_token", auth_token:<HTTP 拿到的 access_token>}`:login 只验自家 token(不再回源第三方、
不轮换 token),写 `login_session:{session_id}` 与账号设备集。CreatePlayer / EnterGame 第一步就是按会话读
`login_session` 取账号,少了这一步回 `kLoginSessionNotFound`。参考调用顺序见 `robot/main.go` `loginAndEnterViaHttpGateway`。

老客户端走 gate 凭据登录这条路**继续保留**,作为过渡期兼容。ARCH §12 T+2 由 login 的
`LegacyGateLoginEnabled=false` 关掉它(回 `kLoginUnknownError`),**只关凭据首认证**;入口分类在
`go/login/internal/logic/clientplayerlogin/legacy_gate_killswitch.go` `classifyLoginPath`,
指标 `login_auth_path_total` 的 `path` 标签按同一分类取 `new` / `gate_bind` / `legacy`。
2026-10-18 前的判定只看「带不带会话」,把 access_token 绑定也算作老路径,开关一关所有客户端都进不了游戏,已修正。

**拒绝点在认证之前**(2026-10-18 起;此前在认证与账号锁之后)。分类只看会话头与 `auth_type`,认证前就能确定,
所以开关关闭时被拒的 gate 凭据登录不调用认证器(第三方 code 不被消费,客户端改走 `/api/login` 时还能用)、
不抢 `account_lock:login:{account}`、不建账号目录、不写 `login_session` / 设备集。代价:凭据真假都回
`kLoginUnknownError`,不再先回 `kLoginAccountNotFound`;拒绝日志只记收敛后的 `auth_type` 与 `session_id`,
不记未认证的账号。

弃用遥测随之前移到认证之前,计的是**请求数**(含认证失败、抢锁失败、被开关拒绝的请求;开关关闭后运维靠
`path="legacy"` 看还剩多少尾巴)。此时 `auth_type` 是未认证的客户端输入,先收敛再做 label
(`deprecation.go` `authTypeMetricLabel`):空串记 `password`;`password` / `access_token` 与 auth 注册表里
已注册的名字保留原值;其余一律记 `unknown`(大小写不符也算)。取值集合在进程生命周期内有界。

### ② `CreatePlayer` (message_id=14):**必走 gate**
玩家此时**已经在游戏 TCP 上**(角色选择界面),客户端 UI 在 gate 的连接上发包,走 `HandleGrpcNodeMessage` 转给 login。

### ③ `EnterGame` (message_id=26):**必走 gate**
角色选择 → 进入世界,触发完整 `EnterGame` 决策(LOGIN_FIRST / RECONNECT / REPLACE)。流程详见 [player_login_flow.md](./player_login_flow.md)。

### ④ `LeaveGame` (message_id=17):**必走 gate**
游戏中 → 角色选择,这是 MMO 标准的"退到选服界面"操作。**TCP 不断**,gate 上 SessionInfo 保留。

### ⑤ `Disconnect` (message_id=58):**gate 内部触发**
玩家 TCP 断开时 gate 主动调用 login,通知"会话开始 Disconnecting"。**不是客户端发的**,但走的也是 gate→login gRPC 通道。

### ⑥ `RefreshToken` (message_id=127):**应走 Java Gateway**
Refresh token 续签是"无状态"操作(只看 refresh token 本身,不需要 session 上下文),应该走 HTTP `/api/refresh-token`(待补)→ go-zero login.RefreshToken,**不该走 gate**。

> 当前 message_id=127 在客户端协议表里,意味着客户端今天可能在 gate 上调它。属于**历史遗留可优化项**,不阻塞主路径。

### 速查表

| RPC | 玩家所处状态 | 走哪 | 备注 |
|---|---|---|---|
| Login(首次) | 登录界面 | Java Gateway → login | 新路径 |
| Login(access_token 绑定会话) | 刚连上 gate | **gate → login** | 新流程必经,`login_session` 唯一写入点;不受 `LegacyGateLoginEnabled` 管 |
| Login(老兼容,凭据首认证) | 登录界面 | gate → login | 兼容期保留;`LegacyGateLoginEnabled=false` 时拒绝 |
| CreatePlayer | 角色选择界面 | **gate → login** | 必走 gate |
| EnterGame | 角色选择 → 世界 | **gate → login** | 必走 gate |
| LeaveGame | 世界 → 角色选择 | **gate → login** | 必走 gate |
| Disconnect | 玩家断线 | gate 内部 → login | 不是客户端发的 |
| RefreshToken | 任何时候 | Java Gateway → login(目标) | 当前在 gate,可迁 |
| AssignGate | 登录界面 | **Java Gateway → login** | gate 不参与 |

---

## 三、玩家 UI 状态机 ↔ 协议路径

```
                         ┌────────────────────┐
       (启动游戏)         │  StateLogin         │  HTTP 短连
   ─────────────────────►│  登录界面           │
                         │                    │
                         │  POST /api/login   │ ──► Java Gateway → gRPC login.Login
                         │  (拿 access_token + │
                         │   refresh_token +  │
                         │   player list)     │
                         │                    │
                         │  POST /api/        │
                         │  assign-gate       │ ──► Java Gateway → gRPC login.AssignGate
                         │  (拿 gate_ip +     │
                         │   HMAC token)      │
                         └─────────┬──────────┘
                                   │ 拿到 gate_ip 后
                                   ▼ 建 TCP 长连接
                         ┌────────────────────┐
                         │  TCP 已连 gate      │
                         │                    │
                         │  ClientToken-      │ ──► gate 本地 HMAC 验签
                         │  VerifyRequest     │     (不查 Redis,不打 RPC)
                         │                    │
                         │  Login{auth_type:  │ ──► gate.HandleGrpcNodeMessage
                         │  "access_token"}   │      → gRPC login.Login
                         │                    │     (写 login_session + 设备集,
                         │                    │      CreatePlayer / EnterGame 靠它)
                         └─────────┬──────────┘
                                   ▼
                         ┌────────────────────┐
                         │  StateCharSelect    │  TCP 长连
                         │  角色选择界面       │
                         │                    │
                         │  CreatePlayer      │ ──► gate.HandleGrpcNodeMessage
                         │                    │      → gRPC login.CreatePlayer
                         │                    │
                         │  EnterGame         │ ──► gate.HandleGrpcNodeMessage
                         │                    │      → gRPC login.EnterGame
                         └─────────┬──────────┘
                                   ▼
                         ┌────────────────────┐
                         │  StateInGame        │  TCP 长连
                         │  世界 / 战斗        │
                         │                    │
                         │  Movement/Skill/   │ ──► gate.HandleTcpNodeMessage
                         │  Scene 系列         │      → TCP scene
                         │                    │
                         │  Chat / Friend /   │ ──► gate.HandleGrpcNodeMessage
                         │  Guild ...         │      → gRPC 对应 Go 服务
                         │                    │
                         │  LeaveGame ────────┼─┐  gate.HandleGrpcNodeMessage
                         │                    │ │   → gRPC login.LeaveGame
                         └────────────────────┘ │
                                   ▲            │
                                   └────────────┘  TCP **不断**,回 StateCharSelect
```

**关键观察:**
1. 玩家从登录界面到角色选择 = HTTP 阶段结束 + TCP 阶段开始,**只换一次链路**
2. 角色选择 ↔ 世界来回切,**TCP 不断**,只是 gate 上的 `SessionInfo` 标志变
3. 玩家"退到登录"才会断 TCP(主动 `Logout` 或客户端关闭)

---

## 四、为什么 LeaveGame / EnterGame 必须走 gate(不走 HTTP)

有人会想"为啥 LeaveGame 不能学 Login 改走 HTTP `/api/leave-game`"。**不能**:

1. **Session 绑定**:LeaveGame 需要 `session_id`,只有同一条 TCP 上的 gate 知道。HTTP 短连无 session 上下文。
2. **结果要推回同一条 TCP**:玩家退到角色选择后,gate 还要在**这条** TCP 上继续推角色列表 / 后续 `EnterGame` 流程。HTTP 拿到响应后没法把它"喂回去"给已建立的 TCP。
3. **Kafka 通知 Scene 销毁实体也要 session_id**:这条命令最终要回到 `gate-{gateId}` topic 上,session_id 是路由 key。
4. **延迟敏感**:玩家点"退出"期望马上回选服,HTTP 多一次握手不值得。

**结论:** 一旦玩家"在游戏 TCP 上",所有命运相关的 RPC 都得走那条 TCP,这是 stateful gate 的本质约束(详见 [gate-load-balancing-design.md](./gate-load-balancing-design.md))。

---

## 五、不要做 / 容易做错的事

### ❌ 不要砍 `HandleGrpcNodeMessage`
它是**所有玩家在游戏 TCP 上调用 Go 微服务的唯一通道**。砍掉等于:
- 玩家不能创角(CreatePlayer)
- 不能进游戏(EnterGame)
- 不能退到选服(LeaveGame)
- 不能聊天 / 加好友 / 进公会 / 看排行榜

### ❌ 不要把 LeaveGame / EnterGame 改成走 Java Gateway
见上节 §四。

### ❌ 不要在 gate 上做"重登录"
- gate 不该校验 OAuth code(那是 HTTP 阶段干的)
- gate 不该签发 access_token(那是 login 干的,HMAC token 是另一回事)
- gate 唯一的"鉴权"是 ClientTokenVerify(HMAC,本地验,~微秒)

### ✅ 应该做的
- 把 `Login`(首次)从 gate 路径迁到 Java Gateway 路径(已在做)
- 把 `RefreshToken` 从 gate 路径迁到 Java Gateway 路径(目标态)
- 给 `Login`(老路径)在 Java Gateway 灰度跑稳后,加 deprecation 日志,再下个版本删
  ——删的只是 gate 上的**凭据首认证**;gate 上的 `Login{access_token}` 是新流程绑定会话的必经一步,
  在另有会话绑定入口取代它之前不能删,也不能被 `LegacyGateLoginEnabled` 拦

---

## 六、gate↔login gRPC 通道的运维特性

| 维度 | 现状 | 文档 |
|---|---|---|
| Channel 长连接复用 | ✅ 每个发现节点持有 `MuduoGrpcChannelPtr`，发现即预连，调用复用现有通道；GOAWAY 时可与排空连接并存 | [grpc-muduo-transport.md](./grpc-muduo-transport.md) §3、§14 |
| HTTP/2 keepalive | ✅ 业务通道默认关闭周期 PING，使用截止触发探测；旧 30s 空闲 PING 曾触发 `too_many_pings` | [grpc-muduo-transport.md](./grpc-muduo-transport.md) §4.4 |
| 服务发现 | ✅ etcd `login.rpc` key | go-zero 标配 |
| 负载均衡 | ✅ `PickRandomNode`(login 无状态) | client_message_processor.cpp:426 |
| Session affinity | scene 才需要(`SceneNodeService`) | client_message_processor.cpp:420 |

---

## 七、推论:`HandleGrpcNodeMessage` 的真实作用

```
gate 业务层有两件事:
  1. 维护玩家 TCP 长连接 + SessionInfo 状态
  2. 当作"客户端协议路由器",按 message_id 把请求转到对应后端

第 2 件事就是 HandleGrpcNodeMessage / HandleTcpNodeMessage 干的。
登录只是它转发的众多 RPC 之一,不是它存在的目的。
```

把它定位成"**协议路由器**"——比"登录转发器"更准确,也更不容易被误删。

---

## 八、对应 mental model 一句话

> **Java Gateway 管"还没进游戏"的玩家;cpp gate 管"已经在游戏 TCP 上"的玩家。两者都会调 go-zero login,职责不重叠。**

---

## 关联文档
- [docs/design/ARCH.md](./ARCH.md) §3 登录链路 / §5 gate
- [docs/design/player_login_flow.md](./player_login_flow.md) — 完整登录/重连/顶号事件流(权威)
- [docs/design/gate-load-balancing-design.md](./gate-load-balancing-design.md) — 为什么 gate 是有状态的
- [docs/design/auth-provider-framework.md](./auth-provider-framework.md) — 第三方登录 provider
- [docs/design/dual-token-authentication.md](./dual-token-authentication.md) — Access + Refresh token
- [docs/design/open-server-rate-limit-design.md](./open-server-rate-limit-design.md) — `/api/login` 限流
- [docs/design/grpc-muduo-transport.md](./grpc-muduo-transport.md) — 现行 gRPC 通道复用、保活与 GOAWAY 语义
- [docs/design/architecture-current-state-vs-gaps-2026-05.md](./architecture-current-state-vs-gaps-2026-05.md) — 现状盘点
- 代码: `cpp/nodes/gate/handler/rpc/client_message_processor.cpp` (`HandleGrpcNodeMessage` / `HandleTcpNodeMessage`)
- 代码: `proto/login/login.proto` (`ClientPlayerLogin` 6 个 RPC)
- 代码: `proto/message_id.txt` (协议号注册表)
