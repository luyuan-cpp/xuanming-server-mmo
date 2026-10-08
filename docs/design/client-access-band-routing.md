# 客户端接入的标准形态，与 gate 按号段整体转发给路由服（client-rpc-router 续篇，D51–D64）

**Created:** 2026-09-28
**状态:** 设计定稿，**尚未实施、未编译、未验证**。本文经过三轮 agent 工作流：
- 接入形态评估：5 路调研，3 个方案各配 1 份设计和 1 份对抗评审；
- 号段设计：5 路代码级调研，安全、兼容、管线三个视角对抗评审，1 次定稿；
- 定稿核验：两路逐条核对引用，一路评审新机制，一路评估标准终态。

改动预计远超 30 个文件，还要改客户端仓。动手前须按 AGENTS §10.2 报告并取得授权，客户端仓另需 §9 授权。

**关联:** [client-rpc-router.md](./client-rpc-router.md)（本文修订其中 D30、D31、D33、D34 的部分表述）、[xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md)（D-9、D-12）、[tip-code-axis.md](./tip-code-axis.md)（号段先例）、[grpc-client-deadline-failure-callback.md](./grpc-client-deadline-failure-callback.md)、[ARCH.md](./ARCH.md)、AGENTS §4、§6、§7、§11.3、§11.7。

**证据标注:**
- 【码】代码或配置事实，写成 `路径:行号`，行号以 HEAD `ed75ad177` 为准。
- 【文】文档里的说法。
- 【推】静态阅读得出的推断，未运行验证。

## 0. 结论速览

1. **不照搬 xuanming 的"客户端直连各服务"。** xuanming 实际上也是"客户端 → Envoy 网关 → 各服务"；多出的一条连接是连 UE DS 的，不是连业务服的。业界标准是：**客户端只连一个接入层，网关按消息号路由、不解析业务 body、不认识具体业务，后端不暴露公网。** 本仓的 gate 加 `client_rpc_router` 已经是这个形态（第一部分）。
2. **要补的是网关的"数据化"：gate 按号段整体转发。**
   - 消息号数轴按大区切分，gate 只认几条大区边界常量；
   - 落在 ROUTER 大区的号，gate 不查表、整段不透明地交给路由服；
   - 路由服按生成的「仅客户端可调用」路由表做权威白名单。

   此后新增 Go 客户端方法，**C++ 零改动，gate 不重编、不重启**（第二部分）。
3. **推荐一步到位到标准终态：** 趁项目未上线，一次停服切换，把全部方法按号段重新发号。gate 不再认识任何 Go 号，也不再链接任何 Go 业务 pb。前置缺陷先分步修，号轴一步到位（§16）。

## 第一部分　接入形态：要不要照搬 xuanming 的"客户端直连各服务"

### 1. 起因

用户原话（2026-09-28）：「我想和 xuanming-server 一样，所有服务可以只有客户端连接吗？这样 C++ 侧就不用转发给 Go 服务器了。」追问：「我就问一下最标准做法是什么，不用管改动。」再追问：「那 gate 按号段整体转发给路由服怎么做？」

本部分回答前两问，第二部分回答第三问。

### 2. xuanming-server 实际怎么接入

以下路径前缀 `X = ../xuanming-server/`。【码】= 代码或配置事实，【文】= 文档说法。

- **不是"客户端直连所有服务"，而是"客户端 → Envoy 网关 → 各服务"。** 客户端固定 2 条连接【文】`X/docs/design/gateway-decision.md:69-102`：
  - ① UE NetDriver（UDP）连 Hub DS / Battle DS，只做局内同步；
  - ② gRPC-Web over HTTP/2+TLS 连 Envoy `:8443`，承载全部业务 unary 请求和推送 stream。Envoy 按 `/pandora.X.v1.Y/` 前缀路由到各 Go 服务【码】`X/deploy/envoy/envoy.yaml:34-74,396-417`。
- **之所以多一条连接，是因为 MOBA 的"场景"是 UE DS**，客户端本来就要直连 DS。否决"客户端只连 DS"的理由见【文】`X/docs/design/architecture-rejected-strict-ds-only.md:66-139`。
- **边缘鉴权**：login 签 HS256 SessionToken（24h）【码】`X/pkg/auth/jwt.go:413-433`。Envoy 依次做 local_ratelimit、无条件剥离客户端自带的身份头、jwt_authn 验签、把 `sub` 写成 `x-pandora-player-id`【码】`envoy.yaml:90-150`。业务服只读这个头，不再验签【码】`X/pkg/middleware/auth.go:74-82`。JWT 只证明"登录过"，所以客户端面 unary 还挂了会话代际校验【码】`X/pkg/middleware/session.go:46-117`。
- **推送**：独立的 push 服务，只有一个 `Subscribe` server stream【码】`X/services/runtime/push/internal/service/push.go:59-115`。业务服只发 Kafka（key = player_id），push 消费后写 Redis ZSET 分配每玩家游标，跨实例用 pub/sub 唤醒，重连按 `last_seen_ms` 补推，有缺口时下发 `pandora.push.resync` 让客户端回源【码】`push.go`、`internal/data/offline.go:209,342`。
- **内部方法防客户端直呼**：Envoy 按精确路径回 403，服务层再拒一次【码】`envoy.yaml:343-355,423-428,506-523`。
- **付出的代价与坑**：
  - Envoy 单副本，崩了业务和推送全断【码】`X/deploy/k8s/infra/edge-envoy.yaml:21`；
  - 被顶号的旧 JWT 在过期前仍能通过 Envoy（INC-20260722-004）【码】`X/pkg/middleware/session.go:3-10`；
  - matchmaker producer 永久为 nil 导致推送全丢，由此补了"推送不承担正确性"原则【文】`X/docs/design/protocol-ordering-rules.md:610-620`；
  - 客户端 gRPC-Web 要自研，UE 插件会让包体 +80MB 并带来 BoringSSL 冲突【文】`gateway-decision.md:769-800`；
  - 每新增一个方法，Envoy 路由、jwt 规则、403 列表都要同时改。

### 3. 本仓现状

- 客户端只有一条 muduo TCP 连 gate（战斗另有票据直连）。gate 按 `gRpcMethodRegistry[id].protocol` 分两路【码】`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:951-968`：
  - TCP 类发给会话绑定的 scene（`HandleTcpNodeMessage`）；
  - gRPC 类在路由模式下原包交给 `client_rpc_router`（`HandleRouterForward`），**不解析 body**（`:159` 原包 CopyFrom）；在直连模式下用 typed sender 解析再序列化（`:726-798`）。
- 路由模式开关：C++ 默认关【码】`cpp/nodes/gate/gate_router_mode.h:34-54`；本地脚本默认开，K8s 默认 `"0"`【码】`tools/scripts/k8s_deploy.ps1:153`。直连模式白名单里没有 chat/guild/friend/team/trade【码】`cpp/nodes/gate/main.cpp:207-209`，所以这些服务在 K8s 上玩家不可达。
- Go 服务 → 客户端推送：`kafkautil.PushToPlayer` → Kafka `gate-cmd` → gate 按 session 下发【码】`go/shared/kafkautil/gate_push.go:50-150`；gate 不在消费组里、从最新位置开始消费，gate 重启窗口内的推送会丢。
- scene 不替玩家把请求转给 chat/guild/friend 这类 Go 业务服；它只为自身业务调 scene_manager 与 data_service。所以"C++ 不转发给 Go"真正能去掉的只有 gate 的 gRPC 转发分支和路由服这一跳。

### 4. 业界标准做法

**客户端只连一个接入层（网关），不直连任何业务服务。**

- 微服务领域：API Gateway / BFF 模式。客户端直连各微服务被视为反模式：内部拆分暴露给客户端，鉴权和限流每个服务各做一遍，服务一重构客户端就得跟着改。
- MMO 领域：以 BigWorld 为例，客户端登录后只连一台 BaseApp 上的 Proxy，CellApp 等进程客户端连不到。国内自研 MMO 的 gate/access 层是同一形态。
- xuanming 也是这个形态，只是网关用现成的 Envoy；多出来的连接是连 DS 的，不是连业务服的。

标准网关的职责：

1. 连接与安全：TLS 或加密、握手时验一次 token、绑定会话、顶号、心跳；
2. 防护：限流、包体上限、消息号白名单、非法包踢线；
3. 路由：按消息号或服务号转发，**不解析业务 body、不认识具体业务**，路由表是数据不是代码；
4. 下行：持有连接，负责应答、推送、断线通知；
5. 身份传递：把"哪个玩家"注入给后端，内网用 mTLS 或签名防伪造；后端一律不暴露公网。

```
客户端 ──长连接(1条)──▶ 接入层/网关 ──内网──▶ 场景服(有状态,会话亲和)
   │                    │               ──内网──▶ 各业务服(无状态,按消息号路由)
   │                    ◀── 推送:服务 → MQ/定位 → 持有连接的网关 → 客户端
   └──票据直连──▶ 战斗/DS(只有权威实时服才允许直连,凭短期票据)
```

**结论：本仓 gate（连接层）+ `client_rpc_router`（L7 路由）已经是标准形态，"C++ 把消息转给 Go"是网关的本职，不是坏味道。** 与标准的差距只有三处：身份头无签名、白名单编译在 gate 里、K8s 默认仍走旧直连模式。第二部分解决第二处。

### 5. 三个方案的评估（2026-09-28，11 agent 调研 + 对抗评审，全部静态阅读）

| 方案 | 内容 | 对"C++ 不转发"的真实收益 | 评审结论 |
|---|---|---|---|
| A 完全照搬 xuanming | 客户端第二条连接（gRPC-Web）经 Envoy 直连 7 个 Go 服务；新建 push 服务；下线路由服 | 删约 200–300 行 C++ 透传代码和路由服；gate 仍要做 scene 中继、控制命令、推送下发、断线通知；大厅连接 1 条变 2 条 | **不推荐**。粗估 14–22 人周（客户端占 3–6 人周且可能翻倍）；评审找出 6 个致命缺陷，均可修但修完前比现状更不安全 |
| B 保持单连接 + 路由服 | 翻 K8s 默认、补签名、删直连旧路径 | ≈ 0（路由模式本来就只有一跳不透明转发） | **有条件推荐**：真实价值是安全收口与代码简化 |
| C 按域混合 | 社交域经新的 `client_edge` 直连，其余走 gate | C++ 一行不删；客户端 3 条连接 | **不推荐** |

方案 A 被评审挡下的致命点（均附证据，见附录 A.1）：

- 会话代际放在 PlayerSession 上不单调：登出 CAS 删键、租约到期 DEL 键后，`nextSessionVersion` 从 1 重来，旧 token 重新有效（ABA）【码】`go/player_locator/internal/logic/markofflinelogic.go:78-83`、`session_manager.go:262-266`；
- 短线重连（ShortReconnect）不经 SetSession，代际不递增，顶号围栏被绕过【码】`session_manager.go:180-185`；
- 以服务级 `OptionIsClientProtocolService` 生成的白名单会放出内部方法 `UpdateGuildScore`【码】`go/client_rpc_router/generated/pb/game/route_table.go:121`；
- 会话证明缺跨 zone 票据字段（CZ-8）、会产生永久 ONLINE 的幽灵会话、保留的 gate→login Disconnect 在 prod 会被拒。

### 6. 顺带发现的现存缺口（与接入形态无关，都要修）

| # | 缺口 | 证据 | 核实状态 |
|---|---|---|---|
| K1 | C++ 全目录不签 `x-caller-*`，而 login 在非 dev 档强制验签（`EnforceInternalAuth` 生产恒 true）；K8s staging/prod 不写 Mode 即 pro 档 | 【码】`cpp/` grep 零命中；`go/login/internal/config/secrets.go`；`tools/scripts/k8s_deploy.ps1:2173-2174` | 代码已核；"staging/prod 下 Login/EnterGame/Disconnect 被拒"是推断，待 Codex 以 pro 档实跑 |
| K2 | Disconnect 通知丢失 = 会话永久 ONLINE，没有对账路径 | 【码】`go/login/internal/logic/clientplayerlogin/session_lifecycle.go:45-52` | 已核（注释原文） |
| K3 | 发号器复用空号，挑哪个空号取决于 Go map 随机顺序；19 号已易主一次 | 【码】`tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go:189-202`、`service_register_info_test.go:57-58`、`PROGRESS.md:5124` | 已核 |
| K4 | login 无客户端方法白名单；`Disconnect`、`LeaveGame` 标为客户端协议 | 【码】`route_table.go:57,59` | 已核；危害以自身会话为限（player_id 取自 gate 注入的会话头） |
| K5 | 服务级可见性放出内部方法：`UpdateGuildScore` 为 `ClientProtocol:true`，只靠 guild 自己的 `ClientMethods` 拦住 | 【码】`route_table.go:121`、`go/guild/internal/session/session.go:94-106` | 已核 |
| K6 | guild/friend/trade 把"没带会话头"当内部调用放行；安全性完全依赖 Go 端口不对外 | 【码】`go/guild/internal/session/session.go:94-96` | 已核 |
| K7 | K8s 默认直连模式，chat/guild/friend/team/trade 玩家不可达 | 【码】`k8s_deploy.ps1:153`、`main.cpp:207-209` | 已核 |
| K8 | 下行专用号（29 个 Notify* 及 23/34/124）在上行白名单里，客户端可上发 | 调研【码】`cpp/generated/rpc/service_metadata/rpc_event_registry.cpp:1536-1679` | 调研已核，本文作者未逐条复核 |
| K9 | 未登录会话的 `player_id = kInvalidGuid (UINT64_MAX)` 被原样放进 SessionDetails，Go 侧只判 `==0` | 调研【码】`session_info_comp.h:54`、`client_message_processor.cpp:119-133` | 调研已核 |

K1–K2 已另开后台任务"gate 补 x-caller 签名与断线通知可靠性"，K3 已另开"发号器改为 message_id 只追加"。

## 第二部分　gate 按号段整体转发给路由服

### 7. 目标、非目标、终态

**目标**
- **G1** 新增 Go 客户端方法时不改 C++，gate 不重编、不重启。
- **G2** 路由服是 Go 客户端方法唯一的权威白名单，按方法判定，fail-closed。内部方法和下行推送号根本进不了它的表。
- **G3** gate 的攻击面不因号段化而扩大：每会话占用的内存、在途调用数都有上界，且与段宽无关。
- **G4** 不误踢正常玩家：版本错位、后端变慢都不能导致正常玩家累计非法包被踢；断线通知不丢。

**非目标**
- scene 客户端方法仍由 gate 逐号放行，因为 scene 按号分发时不看客户端可见性【码】`cpp/nodes/scene/handler/rpc/scene_handler.cpp:379-399`。新增 scene 客户端方法仍需重编 gate 和 scene。是否把 scene 段也整段转发、把可见性校验下沉到 scene，列为待决 Q1。
- battle 不进路由服（D33 不变）。
- 下行链路不改：Kafka 推送、scene 下发、回包本来就原样透传（F14）。
- 路由服 v1 不做按玩家、按方法的有状态限流（§13.5）。

**终态（本文推荐）**
- gate 只认识：大区常量、scene 客户端逐号表、GM 号集合、自己的服务方法。
- gate 只链接 `client_rpc_router` 和 etcd 两个 gRPC 客户端，不 include 任何 Go 业务 pb，没有任何 Go typed stub。
- `kMaxRpcMethodCount` 只覆盖 NODE 大区。
- 路由服按生成的 `ClientRouteTable` 做权威白名单，另用 `GateHookTable` 承接 gate 的会话关闭通知。

### 8. 现状事实（逐条核验后）

| # | 事实 | 证据 | 对设计的含义 |
|---|---|---|---|
| F1 | `kMaxRpcMethodCount=239` 是编译期常量，另有 static_assert 校验 | 【码】`cpp/generated/rpc/service_metadata/rpc_event_registry.h:28`、`rpc_event_registry.cpp:267-269` | 今天任何新方法都会改到 C++ 生成物 |
| F2 | gate 白名单 `IsClientMessageId` 共 136 个 case：29 个 Notify*，S2C 的 23、34、124，`UpdateGuildScore`、`LeaveGame`、`Disconnect`，6 个 GM。`route_table.go` 共 138 条，其中 `ClientProtocol:true` 83 条（battle 12、guild 23、team 15、friend 11、match 10、login 6、trade 4、chat 2） | 【码】`rpc_event_registry.cpp:1536-1679`、`go/client_rpc_router/generated/pb/game/route_table.go` | 下行号和内部号今天都能上发（K8） |
| F3 | 路由表的 ClientProtocol 是服务级布尔，路由服唯一的方法级闸是 `!route.ClientProtocol` | 【码】`tools/proto_generator/protogen/internal/route_table.go:66`、`go/client_rpc_router/internal/logic/forwardlogic.go:126` | 需要方法级可见性（D53） |
| F4 | gate 实际闸门顺序：会话存在（859-866）→ verified（874-879）→ 白名单（890）→ 体积（847→269）→ 限流（849→301）→ GM 闸（925）→ 按 protocol 分流（951-968）。`client-rpc-router.md:37` 写的是体积、限速在白名单之前，与代码不符 | 【码】`cpp/nodes/gate/handler/rpc/client_message_processor.cpp` | 新顺序见 §12.2 |
| F5 | 限流器每会话一份 `unordered_map<messageId, circular_buffer>`：先建桶再判断，桶从不回收；表值 uint32 被截成 uint8；秒级时钟；默认 3 次/秒 | 【码】`cpp/libs/engine/core/message_limiter/message_limiter.h:10,15-17`、`message_limiter.cpp:8,13-26,45-47` | 今天桶数有界，只因为白名单排在前面。ROUTER 段号**绝不能**进 `CanSend(id)` |
| F6 | 路由服拒绝时以 MessageContent 信封返回（未知号、非客户端协议号都是 `kInvalidParameter`），gate 原样下发，不计非法包 | 【码】`forwardlogic.go:119-132`、`cpp/nodes/gate/main.cpp:297-301` | — |
| F7 | 首次验票成功时非法包计数清零（1092）；验票没有一次性消费或 nonce（971-1102）。gate 票据由 Go 签发，TTL 10 分钟；Java 侧的 `token-ttl-seconds: 300` / 代码默认 60 是否仍然生效，待核实 | 【码】`client_message_processor.cpp:1004-1008,1092`、`go/login/internal/logic/loginpregate/assigngatelogic.go:27`、`java/gateway_node/src/main/resources/application.yaml:34` | 换一条新 TCP 重放同一张票，就能拿到全新的计数器。踢线只能当辅助手段 |
| F8 | 登录绑定前 `SessionInfo.playerId = kInvalidGuid (UINT64_MAX)`，路由和直连两条路径都原样放进 SessionDetails；Go 各服务只判 `==0` | 【码】`cpp/libs/services/gate/session/comp/session_info_comp.h:54`、`client_message_processor.cpp:119-133,743-744,831`；`go/chat/internal/session/session.go:47`、`go/friend/internal/session/session.go:78,121`、`go/guild/internal/session/session.go:89` 等 | 未登录会话会以玩家 18446744073709551615 的身份进入 Go 服务（K9） |
| F9 | scene 按号分发只查越界、服务是否存在、方法是否存在，不看客户端可见性 | 【码】`scene_handler.cpp:379-399` | scene 号不能撤掉 gate 的逐号白名单 |
| F10 | `RpcMethodMeta::serviceName` 默认 nullptr。**今天已有空槽 id=3**（`dbTest`，DB 域不进注册表），以下位置遇到空槽都会构造 `std::string(nullptr)`，属于 UB：`game_channel.cpp:356-358`、`455-456`、`511-513`（SendErrorResponse，即回 NO_SERVICE 的那条路径），以及 `scene_handler.cpp:76,314,386,623` | 【码】`rpc_event_registry.h:12`、`cpp/generated/rpc/service_metadata/db_service_metadata.h:6`、`cpp/libs/engine/core/network/game_channel.cpp`。另外 `game_channel.cpp:391-392` 与 `player_message_utils.cpp:285,298` 已判空，是安全的 | 空槽安全是**修现存缺陷**；只增不复用之后空槽会更多 |
| F11 | 三个 handler 都是先 assert 再取 `registry[id]`（675-676、728-729、809-810），951 行在分流前就取了 `registry[id]` | 【码】`client_message_processor.cpp` | Release 下越界就是 UB。分类必须在所有注册表访问之前 |
| F12 | 发号器把空号放进 map，按 Go map 的随机顺序复用；单测断言"复用 ID 1"；19 号已易主一次。旧读取器遇到「左侧非数字=右侧」的行会 Fatal，遇到不含 `=` 的行 Warn 并跳过。生成器自己的 `generateConstants` 也逐行读 `message_id.txt` 生成各 Go 服务和 robot 的 `message_id.go` | 【码】`tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go:69,119-135,189-202`、`service_register_info_test.go:57-58`、`tools/proto_generator/protogen/internal/message_id.go:45-55,183-214`、`PROGRESS.md:5124` | 发号器需要重做（§11） |
| F13 | `guild.proto` 有 28 个 rpc，`message_id.txt` 里 GuildService 只有 23 条：B6a 的 5 个新 rpc 还没发号 | 【码】`proto/guild/guild.proto:571-575`、`proto/message_id.txt` | 切换时一并发号（Q6） |
| F14 | 下行对号透明：MessageContent 原样下发。依赖号表的只有直连模式的反查表（268-278、303-313）和 gate 自造的 23、34、124 | 【码】`main.cpp:268-313`、`client_message_processor.cpp:263`、`cpp/nodes/gate/handler/event/gate_event_handler.cpp:132,265`、`scene_entry_dispatch.cpp:192` | 下行不用改 |
| F15 | gate 合成断线通知 `ClientRequest{id=0, message_id=58}`，走同一个 Forward，请求体是 `loginpb::LoginNodeDisconnectRequest`。login 清理 loginsession 用 metadata 的 session_id，只有标记断线（`markPlayerSessionDisconnecting`）用请求体的 `in.SessionId`。player_locator 以 session_id 是否一致作为唯一身份校验。robot 以客户端身份发 58 号，请求体为空，所以今天对租约不起作用 | 【码】`client_message_processor.cpp:19,26,452-481`、`go/login/internal/logic/clientplayerlogin/disconnectlogic.go:26-35`、`go/player_locator/internal/logic/setdisconnectinglogic.go:48-53`、`robot/login.go:278-287` | gate 为此必须链接 login pb；设计见 §14 |
| F16 | `ClientRequest` 里有客户端可控的 `service`、`method` 两个字符串，gate 用 CopyFrom 整包转给路由服 | 【码】`proto/common/base/message.proto:196-203`、`client_message_processor.cpp:159` | 转发前清掉（§12.5） |
| F17 | 路由服透传所有 `x-` 前缀 metadata；K8s Service 是 ClusterIP 50600；`deploy/` 下 NetworkPolicy 零命中 | 【码】`forwardlogic.go:41,215-223`、`deploy/k8s/manifests/go-svc/client-rpc-router.yaml:20-26` | 安全前置（§15） |
| F18 | 路由模式开关进程内只读一次；gate 关机时 forceClose 全部客户端 | 【码】`cpp/nodes/gate/gate_router_mode.h:50-54`、`main.cpp:250-262` | 切模式要滚动 gate、踢全部玩家，不能当回退手段 |
| F19 | 路由服先在 etcd 注册再 Listen；`s.Start()` 返回之后才由 defer 注销 | 【码】`go/client_rpc_router/client_rpc_router_service.go:84-95,118` | 每次滚动都有一段不可用窗口（§13.8） |
| F20 | 失败回执不带请求 id：`SendTipToClient` 用 23 号且不设 id；battle 拒绝（820）、无路由服（833）、gRPC 失败桥（`main.cpp:323-348`）都走这条路径 | 【码】`client_message_processor.cpp:257-264` | 阶段 0d 修 |
| F21 | 超时预算：本地 gate→路由服 deadline 8000（`bin/etc/base_deploy_config.yaml:215`）；K8s 的 ConfigMap 整目录遮蔽镜像配置、且不带 `GrpcClient` 段，所以 K8s 实际用内置默认 10000（`cpp/generated/grpc_client/grpc_call_tag.h:22`、`k8s_deploy.ps1:709-711`）。路由服 Timeout 6000 > ForwardTimeoutMs 5000 | 【码】如左 | 在途名额的过期阈值必须读实际生效的 deadline（§12.4） |
| F22 | 路由服 watch 哪些目标类型由服务级 ClientProtocol 推导；zone 作用域是配置项 `ZoneScopedNodeTypes`（默认只有 login） | 【码】`go/client_rpc_router/internal/svc/servicecontext.go:48-55`、`internal/config/config.go:31-44` | §13.7 |
| F23 | gate 的 GM 闸只认 6 个 scene GM 号；路由服没有运行模式概念 | 【码】`cpp/nodes/gate/gate_gm_client_messages.h:39-60` | GM_DEV 禁止进 ROUTER（§10.5） |
| F24 | session_id 是 uint32，序号段 17 位掩码后回绕，刚释放的号可以立即被新会话拿到；回包桥接只按 session_id 找会话 | 【码】`cpp/libs/engine/core/utils/id/node_id_generator.h:14-17,27,51`、`client_message_processor.cpp:621-630`、`main.cpp:290-293` | 需要身份栅栏（D60） |
| F25 | guild 有 fail-closed 的方法白名单，但每次拒绝都写一条不采样的 Errorf；chat、login、match、team 没有方法白名单 | 【码】`go/guild/internal/session/session.go:47-78,94-114` | 路由服成为权威后，业务侧白名单保留为纵深防御 |
| F26 | 现成 tip 码：1003 `kServiceUnavailable`（fault）、1005 `kInvalidParameter`、1008 `kRateLimitExceeded`、1013 `kMessageIdNotFound`；1011、1012 是 fault 码，friend 明确不用 1012 表示"客户端没带身份" | 【码】`generated/code/proto/tip/common_error_tip.proto:18-38`、`go/shared/generated/tip/faults.go:26-34`、`go/friend/internal/session/session.go:70-75` | **本设计不新增 tip 码**；未登录用 1005 |
| F27 | MethodOptions 已有扩展点（`OptionMessagePriority=410000`），服务级 option 用了 400000/400001；SessionDetails 下一个空闲字段号是 7 | 【码】`proto/db/proto_option.proto:63-73`、`proto/common/base/session.proto:6-18` | 新 option 与新字段有位置，实施前复核 |
| F28 | 生成的 C++ gRPC 客户端用 message_id 做完成队列 tag，在 switch 里分派，不索引注册表 | 【码】`cpp/generated/grpc_client/client_rpc_router/client_rpc_router_grpc_client.cpp:86,99` | ROUTER 段号不需要进 C++ 注册表 |
| F29 | 节点摘除默认宽限 0：`ExecuteNodeRemoval` 在 queueInLoop 里 DestroyEntity，实体上的 CompletionQueue、stub、Channel 一起析构；全仓没有 CQ 的 Shutdown 或排空。结果是在途调用永无回调，call 对象在 object_pool 里泄漏，客户端只能等超时 | 【码】`cpp/libs/engine/core/node/system/node/node.cpp:1310-1311,1327-1393`、`cpp/generated/grpc_client/grpc_init_client.cpp:358-365,438`、`client_rpc_router_grpc_client.cpp:28,58`、`bin/etc/base_deploy_config.yaml:12` | 阶段 0g 修，是在途记账正确的前提 |
| F30 | gate 链接的 `-lrpc` 和 `-lgrpc_client` 是全节点共享的；`InitGrpcNode` 按节点类型初始化全部 Go 服务客户端；`main.cpp:26` include 了 scene_manager 的 pb 却没使用；`SendMessageToPlayerOnGrpcNode` 全仓无调用方 | 【码】`cpp/nodes/gate/CMakeLists.txt:104,107`、`grpc_init_client.cpp:436-476`、`cpp/libs/engine/core/network/player_message_utils.cpp:270-300` | 按节点生成（D55） |
| F31 | MessageLimiter 表 68 行，主键是裸数字：47 行 Go 号（guild 21、team 12、friend 10、trade 4），21 行 scene 号；id=68 指向内部方法 `ScenePlayerSyncSyncAttribute30Frames`，id=119 已因发号漂移改绑过一次 | 【码】`data/schema/messagelimiter_table.proto:12-21`、`generated/tables/messagelimiter.json` | 表键改为方法名（D64） |
| F32 | `message_id.txt` 有跨仓消费者：客户端仓的 `tools/gen_messageids.ps1` 直接读它，为没有生成桩的服务取号 | 【文】`docs/design/team-system.md:2192`、`player-pet.md:247`、`friend-client-spec-20260920.md:150-159` | 切换批次要同步客户端仓（§9 授权） |

### 9. 决策

| # | 决策 | 理由 |
|---|---|---|
| D51 | 消息号数轴按大区划分（§10.1）。gate 只认生成的 `message_id_bands.h` 里的大区常量，ROUTER 大区整段不透明转发 | gate 不再需要知道每个 Go 客户端方法 |
| D52 | **一次停服切换，全部方法按 axis 的 extent 重新发号**（含 NODE），0 号保留不发。旧号→新号写进 state 的 `legacy_ids`，只供排查，**不是运行时翻译层**（照 tip 码轴先例） | 项目未上线、无老客户端（`PROGRESS.md:4691,5124`）；AGENTS §4.3 允许开发期改号但须完整重编。渐进路径要先建再拆 LegacyGrpc 分类、legacy 限流行、UNSPECIFIED 窗口和冻结号，总成本更高 |
| D53 | 可见性唯一事实源 = 方法级 option `OptionMethodVisibility`。标了 `OptionIsClientProtocolService` 的服务，其中每个方法缺这个 option 就在生成期报错；服务级开关降为派生用途 | 修掉 F3（UpdateGuildScore、Notify* 上行）；新方法 fail-closed；不维护第二份真相 |
| D54 | 发号器改为 `message_id_axis.yaml` 加 `message_id_state.json` 两个文件：只增不复用、墓碑、每个 extent 记一个 `next` 高水位、全局唯一自检、bootstrap 防重入、毒丸、state 缺失 fail-closed | F12；撞号要在 git 层显式冲突 |
| D55 | 各生成物按大区收录（§11.3）：GO_DOWNLINK、GO_INTERNAL 不进任何 C++ 生成物；注册表与 grpc_client 按节点生成 | gate 不再链接 Go 业务 pb（F30） |
| D56 | gate 用一个分类纯函数返回 4 类结果，管线顺序固定；分类之前不访问注册表；注册表空槽必须安全 | F10、F11 |
| D57 | gate 限流：ROUTER 大区每会话 1 个聚合令牌桶，软限回 tip 不计非法包，硬限才计；每会话和进程各有在途上限，超限只回 tip、不计；在途**按单次调用记账** | G3、G4；在途超限多半是后端变慢，不该惩罚客户端 |
| D58 | 路由服是 Go 客户端方法唯一的权威：只生成 `ClientRouteTable`（C2S、C2S_PRELOGIN）；表外号回 1013、未登录调 C2S 回 1005，都用信封返回、都不计非法包 | G2、G4 |
| D59 | 断线通知改走专用 RPC `ClientRpcRouter.NotifySessionClosed`：空请求体加会话 metadata 加 zone_id，路由服按生成的 `GateHookTable` 转给 login.Disconnect；login 用 metadata 的 session_id；gate 的 `loginStarted` 改为 `routerTouched`。**gate 不再认识任何 Go 号** | F15；Forward 永远只代表客户端来源，不需要 origin 字段和并存窗口 |
| D60 | SessionDetails 新增 `uint64 session_incarnation = 7`，作为回包身份栅栏；`player_id` 在 `BuildSessionDetails` 这个唯一构造点把 kInvalidGuid 归一成 0 | F8、F24 |
| D61 | 删除直连模式是切换的硬前置：K8s 先翻到路由模式并跑通 battle-smoke，再删直连路径和模式开关本身。同时修订 D-12「C++ 默认值和单测一字不改」 | Login 搬进 ROUTER 段后，直连模式连登录都做不了 |
| D62 | 安全前置顺序定死：NetworkPolicy → gate 到路由服的认证 → 路由服剥离并代签 `x-caller-*` → metadata 透传收窄为显式清单 | 否则路由服会变成签名预言机；也解决 K1 |
| D63 | 版本信号：gate 的 `NodeInfo.message_axis_version` 加上路由服的 `client_rpc_router_route_table_info{axis_version, table_hash}`，发布门禁机械核对 | 切换后能识别混进来的旧镜像 |
| D64 | MessageLimiter 表键改为方法全名（或者加 `message_ref` 选项，导表时对照生成的方法清单校验）；47 行 Go 号删除，Go 方法的逐方法限流归各业务服务 | F31；消息号引用也纳入号轴自检 |

### 10. 消息号数轴

#### 10.1 大区

| 大区 | 区间（初值） | 成员 | gate 上行行为 | C++ 注册表 | 路由表 |
|---|---|---|---|---|---|
| — | 0 | 保留，永不发号 | 拒绝，计非法包 | — | — |
| NODE | [1, 16384) | 由 C++ 节点提供、或被 C++ 节点调用的方法：scene（C2S、S2C、内部）、battle、gate 自己的服务方法、gate 与 scene 之间的内部方法、C++ 调用的 SceneManager / DataService 方法、etcd KV/Lease/Watch、路由服的 Forward 和 NotifySessionClosed | 只放行生成的 scene 客户端逐号表（C2S、GM_DEV）；battle C2S 回 tip；其余拒绝并计数 | 收录，`kMaxRpcMethodCount = NODE 最大号 + 1` | 不收录 |
| ROUTER | [16384, 32768) | Go 的 C2S、C2S_PRELOGIN | 整段处理：体积 → 聚合桶 → 在途上限 → Forward | 不收录 | `ClientRouteTable` |
| GO_DOWNLINK | [32768, 40960) | Go 的 S2C（Kafka 推送用） | 拒绝，计非法包 | 不收录 | 不收录 |
| GO_INTERNAL | [40960, 49152) | 只被 Go 调用的内部方法，以及 VIS_GATE_ORIGIN 方法（如 login.Disconnect） | 拒绝，计非法包 | 不收录 | 只收 `GateHookTable`（按 hook 索引，不按号） |
| 保留 | [49152, 2³²) | — | 拒绝，计非法包 | — | — |

- 16384 及以上的号 varint 编码要 3 字节，每包多 1 字节【推】，可以忽略。
- 大区边界写在 axis 文件里，由生成器输出到 `message_id_bands.h`。调整大区边界要重编 gate，这件事很少发生，属于设计内的代价。
- NODE 内可选再划一个 BATTLE 子区，gate 按区间判定 BattleViaGate，不用逐号列（Q7）。

#### 10.2 服务 extent

- 每个大区里，每个服务可以有**多个** extent，默认宽度 256。段满时追加新 extent，**绝不重排**。
- 服务第一次在某个大区发号时如果没有 extent，生成期直接报错，必须人工在 axis 文件里加一行，经过评审。
- gate **不认识** extent，只认大区。extent 只用于发号归属：多会话并行改不同服务时互不抢号，号也更好读。
- 同一个服务在两处（两个 worktree 或两台机器）并行追加方法时，靠 state 里的 extent `next` 高水位在 git 合并时产生冲突来暴露（§11.2），不靠文件锁。

#### 10.3 方法级可见性 option

写在 `proto/db/proto_option.proto` 的 MethodOptions 下，字段号 410001（实施前复核是否被占用）：

```proto
enum MethodVisibility {
  VIS_INTERNAL = 0;      // 默认:服务端内部
  VIS_C2S = 1;           // 客户端可调,要求已绑定玩家
  VIS_C2S_PRELOGIN = 2;  // 客户端可调,登录前即可
  VIS_S2C = 3;           // 下行推送占号,上行一律拒
  VIS_GATE_ORIGIN = 4;   // 只由 gate 经专用 hook 触发(login.Disconnect)
  VIS_GM_DEV = 5;        // 客户端可调,仅 dev/test 运行模式(只允许 scene)
}
```

规则：
- 服务标了 `OptionIsClientProtocolService` 时，每个方法**必须**显式写 visibility，否则生成期报错。存量方法在切换批次里一次性补齐。其他服务的方法默认 INTERNAL。
- VIS_GATE_ORIGIN 方法还要写 hook 名，例如 `OptionGateHook = HOOK_SESSION_CLOSED`。生成器保证每个 hook 恰好对应一个方法。
- C2S_PRELOGIN 的初始名单：Login、CreatePlayer、EnterGame。RefreshToken 等待定（Q3）。
- 生成器用 protowire 从 unknown fields 读 option，先例是 `option/value.go` 的 `ReadBoolExtension`，只需补一个 varint/enum 版本。
- 服务级再加 `OptionZoneScopedService`（ServiceOptions 400002，实施前复核），替代路由服配置项 `ZoneScopedNodeTypes`（§13.7）。

#### 10.4 大区归属规则（生成器按此自动决定，不手填）

| 方法 | 大区 |
|---|---|
| 由 C++ 节点提供（scene、battle、gate、路由服的 gate 入口），或被 C++ 节点调用 | NODE |
| Go 提供、VIS_C2S 或 VIS_C2S_PRELOGIN | ROUTER |
| Go 提供、VIS_S2C | GO_DOWNLINK |
| Go 提供、VIS_INTERNAL 或 VIS_GATE_ORIGIN，且没有 C++ 调用方 | GO_INTERNAL |

"被 C++ 调用"由生成器按"哪些节点的 grpc_client 需要它"判定，今天有 SceneManager 44/46、DataService 180【码】（核验记录）。

按 HEAD `ed75ad177` 的实测，切换时的搬迁清单如下：

| 去向 | 条目 |
|---|---|
| ROUTER | login 5（48、14、26、127、17）、chat 2（61、28）、friend 10（2、7、11、12、119、230、232、234、236、238）、guild 21（15、19、27、29、35、38、39、52、53、60、76、120、216–219、221–223、228、233）+ B6a 新增 5、match 8（148、151、152、153、157、163、164、179）、team 12（201、202、204–212、214）、trade 4（196、197、198、200） |
| GO_DOWNLINK | 235、220、154、156、203、213、215 |
| GO_INTERNAL | UpdateGuildScore（8）、login.Disconnect（58，VIS_GATE_ORIGIN），以及约 29 个只被 Go 调用的内部方法（DataService 除 180 外的 21 个、SceneManager 16/85、LoginAdmin 111、LoginPreGate 118/138、TradeAdmin 199 等） |
| NODE | scene 的 36 个 C2S（含 6 个 GM）和 17 个 S2C（含 gate 自造的 23/34/124）、battle 12 个、gate 服务方法、gate 与 scene 之间的 9/10、Forward（176）、C++ 调用的 SceneManager 44/46 和 DataService 180、etcd 的 KV/Lease/Watch |

NODE 大区也在同一批里按 extent 重新发号（D52）。这样"legacy"这一类彻底消失。额外成本只有 scene 的限流行（D64 改成按名字作键后不受影响）和 Unity 的数字字面量（本来就要在同一批重新生成）。

#### 10.5 生成期自检（任一失败就 fail-closed）

1. 大区互不重叠；每个 extent 落在所属大区内，同一大区内的 extent 互不重叠；每个号落在所属 extent 内。
2. **号全局唯一，墓碑号不得复用。**
3. 可见性与大区一致（§10.4）：ROUTER 只放 C2S、C2S_PRELOGIN；GO_DOWNLINK 只放 S2C；GATE_ORIGIN 只在 GO_INTERNAL；GM_DEV 只在 NODE，**不得进 ROUTER**；battle、scene 只在 NODE。
4. 0 号永不发出。
5. 与 `git show HEAD:proto/message_id_state.json` 比较，只增不减：已发出的号不改主人，删除的方法留墓碑。
6. 标了客户端服务的方法缺 visibility 就报错；每个 hook 恰好对应一个 GATE_ORIGIN 方法。
7. 生成的 GM 集合必须等于 VIS_GM_DEV 集合，单测断言，并与 `gate_gm_client_messages.h` 核对，或者直接改成由生成物提供。
8. 导表器校验 MessageLimiter 等表里引用的方法名都存在，且是客户端可调的 NODE 方法（D64）。

### 11. 生成器改造

#### 11.1 文件

| 文件 | 维护方 | 内容 |
|---|---|---|
| `proto/message_id_axis.yaml` | 人工，需评审 | 大区边界、各服务在各大区的 extent 列表、`axis_version` |
| `proto/message_id_state.json` | 生成器写入 | 全限定方法 → 号；墓碑；每个 extent 的 `next` 高水位；`legacy_ids`（切换前的旧号，只供排查）。是发号的唯一权威，照抄 tip 先例（`tools/data_table_exporter/core/generators/enum_gen.py` 的 `_assign_tip_ids`、`_check_tip_axis`，state 缺失即 fail-closed） |
| `proto/message_id.txt` | 生成器写入 | 降为派生视图，首行写毒丸 `!format=segmented-v2`，墓碑以不含 `=` 的注释行保留 |
| `cpp/.../message_id_bands.h` | 生成器写入 | 大区常量与 `axis_version` |

#### 11.2 防旧二进制、防撞号、防重入

- **毒丸必须恰好含一个 `=`，且左边不是数字。** 旧读取器切分后对左段 ParseUint，失败就 Fatal【码】`service_register_info.go:119-135`；不含 `=` 的行只会 Warn 跳过，防不住。毒丸在旧流水线的 InitMessageId 之前就会触发（`pipeline.go:26` 早于 `:42-44`）。
- 会跑到旧二进制的路径不止 `-UseBinary`：默认的 `proto-gen-run` 也优先运行预编译的 `proto-gen.exe` 或旧名 `pbgen.exe`（`tools/scripts/dev_tools.ps1:329-352`）；`proto-gen-build` 失败时会留下旧 exe（`:274-279`）。
- **生成器自己的 `generateConstants` 必须改读 state**，或者跳过以 `!` 开头的行。否则毒丸会生成 `const SegmentedV2MessageId = !format`，所有 Go 服务和 robot 的 `message_id.go` 都编译不过【码】`internal/message_id.go:45-55,183-214`。event_id 路径复用同一个解析器，改动时不要把它带坏。
- **撞号**：同一个 extent 的 `next` 高水位存成 state 里的独立一行。两处并行在同一 extent 追加方法时，这一行必然在 git 合并时冲突；再加上"号全局唯一"自检，重复号无法静默入库。文件锁（锁文件记录 pid，能判定过期锁）只能串行化同一工作树里的并发运行，**不是全局互斥**，文档和报错信息都要写明。
- **写入原子性**：先写临时文件再 rename。
- **bootstrap 防重入**：只在「HEAD 和 `git log --all` 里都从未出现过 state 文件」且 `message_id.txt` 首行不是毒丸时才允许；否则拒绝，并提示去恢复 state，而不是重新初始化。这是为了防止重建时丢掉墓碑、让被删方法的号又被发出去。

#### 11.3 各生成物收录哪些号

| 生成物 | 收录 |
|---|---|
| C++ `rpc_event_registry`、C++ `grpc_client` | 只收 NODE，**按节点生成**。gate 的 TU 只含 gate 自己的服务、scene 客户端逐号表与 GM 集合（不依赖 pb 的元数据）、路由服与 etcd 客户端 |
| `message_id_bands.h` | 大区常量 |
| 路由服 `route_table.go` | `ClientRouteTable`（C2S、C2S_PRELOGIN，带 visibility 和 zone_scoped）、`GateHookTable`（hook → FullMethod + NodeType + zone_scoped）、`AxisVersion` 与 `TableHash` 常量。**不收 INTERNAL** |
| 各 Go 服务的 `message_id.go`、robot、Unity 桩 | 全部号（Unity 只为 `ClientPlayer`、`GamePlayer` 服务生成，GATE_ORIGIN 不生成） |

#### 11.4 消费者清单

| 消费者 | 处理 |
|---|---|
| `service_register_info.go` 读取器 | 改读 state |
| `internal/message_id.go` 的 `generateConstants` | 改读 state，或跳过 `!` 行 |
| `tools/scripts/friend_xlsx_patch.py:159-170`、`guild_b5a_xlsx_patch.py:339-345` | 遇到毒丸会 fail-closed（正确）；message-limiter 子命令退役（D64） |
| 客户端仓 `tools/gen_messageids.ps1` | 改读 state JSON，或确认能容忍毒丸行；需 §9 客户端仓授权 |
| `service_register_info_test.go` | 保留"注册表大小跟着同步"（60-65），"复用 ID 1"（57-58）改为断言不复用；补空槽 id 走分发路径不崩的用例 |


### 12. gate 侧

#### 12.1 分类纯函数

`ClassifyClientMessage(id)` 是纯函数，只依赖 `message_id_bands.h` 和生成的 scene 客户端逐号表。它返回 4 类结果：

| 结果 | 条件 | 处理 | 计非法包 |
|---|---|---|---|
| SceneTcp | NODE 大区，在 scene 客户端逐号表里（C2S、GM_DEV） | 限流（逐号 MessageLimiter）→ GM 闸 → `HandleTcpNodeMessage` | 限流超限、prod 下的 GM 号照旧计 |
| BattleViaGate | NODE 大区，是 battle 的 C2S | 回 1003（带请求 id） | 不计。`skip_direct_connect` 回落路径上的正常客户端会发这些号 |
| RouterBand | 落在 ROUTER 大区 | 聚合桶 → 在途上限 → Forward | 见 §12.3、§12.4 |
| Reject | 其余情况：0 号、NODE 里不在表中的号、GO_DOWNLINK、GO_INTERNAL、保留区 | 丢弃 | **计** |

直连模式在阶段 1 就已删除（D61），分类里没有 LegacyGrpc 这一类。

#### 12.2 管线顺序

1. 会话存在；
2. verified；
3. **分类**：Reject 就计数并返回；
4. 体积：1KB，与号无关【码】`client_message_processor.cpp:269-299`；
5. 限流：RouterBand 走聚合桶，SceneTcp 走逐号 MessageLimiter；
6. GM 闸：只对 SceneTcp 里的 GM_DEV 号；
7. 在途上限：只对 RouterBand；
8. 分派。

**分类之前不得访问 `gRpcMethodRegistry`**，包括 675-676、728-729、809-810 这三个 handler，以及 951 行（F11）。

#### 12.3 ROUTER 大区限流（D57）

- 新写一个 `BandRateLimiter`：令牌桶，毫秒单调时钟，每会话只有 1 个桶，键固定。不复用 MessageLimiter，因为它用 `operator[]` 自动建桶、有 uint8 截断、用秒级时钟（F5）。
- **软限**：超出桶容量时回 1008（带请求 id），**不计**非法包。
- **硬限**：超过软限的 N 倍，或连续 T 秒持续超限，才计非法包。这样洪泛连接最终会被踢，合法的突发不会。
- 参数从 gate 配置读取，并做范围校验。gate 配置没有热加载（未穷尽搜索），改参数就要重启 gate，所以把「桶容量 ≥ 已发布方法的合法突发」写进发布门禁和 §17 的新增方法清单。初值由压测定：按真实客户端的突发量（如打开帮会面板时的并发请求数）的 2 倍以上定档。
- **每会话桶数上界** = scene 客户端逐号表大小（今天是 36 个，含 6 个 GM）+ 1。与 ROUTER 段宽无关。
- Go 方法的逐方法限流由业务服务自己负责，chat、friend 已有 Redis 先例（`go/chat/internal/logic/chat_logic.go:79-82`、`go/friend/internal/logic/rate_quota.go`）。

#### 12.4 在途上限（D57）

- **每会话上限**（初值 32）：超限回 1008（带请求 id），**不计**非法包。在途超限多半是后端变慢造成的，聚合桶已经限住了客户端速率。
- **进程上限**：超限回 1003（带请求 id），不计。初值按「峰值 Forward QPS × 实际 deadline 秒数 × 安全系数」推导，再由压测校准。
- **按单次调用记账**：
  - 键 = `(const grpc::ClientContext*, 单调 seq)`，值 = `{session_id, session_incarnation, 到期时刻}`。成功桥拿到的 ctx 与失败桥的 `failure.context` 是同一个 `call->context`；加 seq 是为了防 object_pool 复用地址。
  - 完成时（成功或失败）按键删除，**删到了才扣减**。进程计数无条件扣减；会话计数只在会话仍在、incarnation 一致时扣减。删不到说明已被过期回收，什么也不做，防止还两次。
  - 到期时刻 = 发出时读取的**实际生效** deadline（F21：本地 8000，K8s 10000），加 CQ 处理余量。过期在申请名额的路径上惰性清理（按到期时间排序的队列），不另挂定时器，免得违反 §11.7 的 this 绑定规则。
  - `NotifySessionClosed` 不登记。
- gate 只有一个 IO 线程，CQ 也在主 loop 里轮询，在途计数和桶都不需要加锁【码】`client_message_processor.cpp:545-549`、`grpc_init_client.cpp:358-365`。

#### 12.5 转发前清洗

- `ForwardRequest.request` 在 CopyFrom 之后清掉 `service` 和 `method`（F16）。
- SessionDetails 带上 `session_incarnation`，`player_id` 归一（D60）。

#### 12.6 回包桥接与身份栅栏（D60，D30 的「桥接零改动」作废）

- **session_incarnation 的定义**：在 `HandleConnectionEstablished` 里与 session_id 一起分配，用进程内单调递增的 uint64，从 1 开始；会话存续期间不变，重复验票、BindSession 都不换。`BuildSessionDetails` 的**所有**调用点都必须带上它，会话关闭通知在 erase 之前从 `sessionIt` 取。
- **栅栏**：Forward 回包里 echo 回来的 incarnation 与会话当前值不一致（**包括 0**）就丢弃，并采样计数。路由服按原值回写 `x-session-detail-bin`，不解析、不重新 Marshal【码】`forwardlogic.go:27-30,97-105`，所以 0 只可能是 gate 自己漏填，没有兼容豁免。
- 为什么不用 player_id 加 gate_instance_id：同一个 gate 上 gate_instance_id 恒定，未登录会话的 player_id 都是 0（F24）。
- **不需要裁决头**：终态里，非 ROUTER 段的号在 gate 按大区就拒掉了，路由服能给出的拒绝只有 1013（表外号，通常是版本错位）和 1005（未登录），两者都不计非法包，gate 不需要区分，直接原样下发信封。统计由路由服的指标承担。定稿稿里的 `x-router-verdict` 删除（KISS）。
- 路由服用 `conn.Invoke` 且不带 `grpc.Header`，所以下游 Go 服务的响应头不会透传到 gate【码】`forwardlogic.go:165-170`。这一点写进路由服契约。

#### 12.7 会话关闭通知（D59）

- gate 在会话关闭时，若 `routerTouched || hasBoundPlayer`，调用 `ClientRpcRouter.NotifySessionClosed{zone_id}`，附会话 metadata。
- `routerTouched` = 本会话有过任意一次成功发出的 ROUTER 段转发，在 `SendViaRouter` 成功后置位，取代 `loginStarted`（`client_message_processor.cpp:787,839-842`）。它是原条件的超集：今天发一个 body 是垃圾的 48 号包就能置位 `loginStarted`，所以攻击面不变；只做过验票的空闲连接仍然不通知 login，放大防线保留（`:441-446` 的注释）。
- 失败回调里有界重试（≤3 次，带退避），仍失败记 CRITICAL。对账 sweep 由 K2 的后续任务决定。
- gate 从此不构造 login 请求体，不再 include login pb。

#### 12.8 可观测性

- gate 没有找到 Prometheus 出口（grep 无命中，不确定）。先用每 N 秒一行的汇总日志加 error_reporter 计数，维度只有 `band × outcome`。
- outcome：forward、band_soft_limited、band_hard_limited、inflight_session_limited、inflight_process_limited、battle_tip、reject、incarnation_mismatch、router_unavailable。
- **禁止**用 player_id、session_id 或原始 message_id 作维度（AGENTS §9）。
- 切换后 `traffic_statistics` 不再按号统计 Go 流量（`traffic_statistics.cpp:45,62` 越界跳过），这部分由路由服的 forward 指标承担。

#### 12.9 gate 仍然必须知道的东西

- 大区常量（`message_id_bands.h`）；
- scene 客户端逐号表与 GM 集合（生成）；
- battle C2S 集合，或 BATTLE 子区（Q7）；
- 自己下发用的 NODE 号 23、34、124，以及和 scene 之间的 9、10（符号常量，重排后重新生成即可）；
- 自己的服务方法。

### 13. 路由服侧

#### 13.1 权威白名单（D58）

- 先查 `ClientRouteTable`，查不到回信封 1013，outcome 记 `unknown`（基数有界【码】`go/client_rpc_router/internal/metrics/metrics.go:73-78`）。
- 断言号落在 ROUTER 大区。不在的话同样回 1013，另记 `out_of_band`：gate 不该发这种号，出现就说明 gate 版本不对。
- 永远不按 `ClientRequest.service/method` 路由，也不把它们写进日志或指标。
- 保留现有的 battle 拒绝分支作为纵深防御。

#### 13.2 需要登录

- VIS_C2S 方法在 `player_id ∈ {0, UINT64_MAX}` 时拒绝，回信封 1005 `kInvalidParameter`（非 fault 码，与 friend 口径一致，F26），outcome 记 `rejected_not_logged_in`。
- 判 UINT64_MAX 是纵深防御：阶段 0b 之后 gate 已经归一。
- C2S_PRELOGIN 放行。
- 为此路由服要**只读地**解码 SessionDetails，回写时仍用原始字节。

#### 13.3 GateHookTable 与 NotifySessionClosed（D59）

- 新 RPC：`rpc NotifySessionClosed(NotifySessionClosedRequest) returns (Empty)`，请求体为 `{ uint32 zone_id = 1; }`，会话走 `x-session-detail-bin`。
- 路由服按 `GateHookTable[HOOK_SESSION_CLOSED]` 找到 login.Disconnect，按 zone_id 选 login 实例（zone-scoped），用**空请求体**调用。
- 不受限流、killswitch、舱壁约束。
- 可在总耗时 ≤ ForwardTimeoutMs 内换实例重试；login 自己有 200/400/800ms 退避（`session_lifecycle.go:53-71`）。
- 这个 RPC 只有 gate 会调；集群内的访问控制见 §15。

#### 13.4 下游契约

- 透传的 metadata 在 §15 第 4 步之前沿用 `x-` 前缀，之后收窄为显式清单。
- 下游响应头不透传。
- 回写 `x-session-detail-bin` 用原始字节。

#### 13.5 纵深防御

- `request.body` 设上限 `MaxForwardBodyBytes`（默认 4096，gate 整包上限是 1KB），超限记 `body_too_large`。go-zero 能否设 MaxRecvMsgSize，未核实。
- 按目标 node type 设在途舱壁（`bulkhead_rejected`）。
- 按方法的 killswitch，改配置要滚动路由服，但不踢玩家。
- v1 **不做**按玩家、按方法的有状态限流：路由服无状态，gate 随机挑实例（`client_message_processor.cpp:143`）。以后要做，前提是按 hash(session_id) 粘住实例（Q2）。

#### 13.6 GM

生成期就禁止 GM_DEV 进 ROUTER（§10.5-3）。路由服的表里没有 GM 方法，不需要运行模式判定。

#### 13.7 目标发现与 zone 作用域

- 要 watch 的目标类型改为从 `ClientRouteTable ∪ GateHookTable` 推导，替换 `servicecontext.go:48-55`。
- zone 作用域改由 `OptionZoneScopedService` 写进表。配置项 `ZoneScopedNodeTypes` 保留一个过渡期：如果配了，必须等于从表推导出的集合，否则 fail-fast；之后删除。

#### 13.8 启停顺序与版本信号

- 启动：先 Listen，再注册 etcd。
- 退出：先注销 → 等待（≥ gate 发现传播时间 + ForwardTimeoutMs）→ GracefulStop（修 F19）。
- 暴露 gauge `client_rpc_router_route_table_info{axis_version, table_hash}=1`（D63）。
- outcome 新增：rejected_not_logged_in、out_of_band、body_too_large、bulkhead_rejected、killswitch、session_closed_ok、session_closed_failed。

### 14. 断线通知的完整口径

- **今天**：gate 伪装成客户端的 58 号经 Forward 发给 login，请求体是 login pb。58 同时在客户端白名单里（K4），robot 以客户端身份发 58 号，但请求体为空，今天对租约不起作用（F15）。
- **终态**：
  - login.Disconnect 标 VIS_GATE_ORIGIN，落在 GO_INTERNAL，客户端上发会被 gate 按大区拒掉，路由服的 `ClientRouteTable` 里也没有它；
  - 只能经 `NotifySessionClosed` → `GateHookTable` 到达；
  - login.Disconnect 改用 metadata 里的 session_id，请求体字段 `session_id` 标 deprecated。
- **顺序约束（核验时发现的坑）**：login 改用 metadata 这一步**不能单独先上**。在 58 仍然客户端可达的窗口里，这一改会让伪造的 58 号**必然**命中自己的当前会话、挂上 30 秒租约，到期后清理掉一个仍然在线的玩家。所以它与"58 离开客户端可达集"必须在同一次切换里生效（阶段 2）。
- robot 的 `sendDisconnectBestEffort` 在阶段 0 删除：定义在 `robot/login.go:278`，调用点有 `robot/main.go:355` 和各 smoke 场景约 20 处，`robot/login_test.go:14-45` 的两个用例一并删除。
- Unity 是否发 58（Q4）：GATE_ORIGIN 方法不再生成 Unity 桩，客户端如果还引用 Disconnect 桩，重新生成后会直接编译失败，问题会被机械地暴露出来。

### 15. 安全前置（D62，顺序定死）

1. 加 NetworkPolicy：只允许 gate 的 Pod 访问路由服 50600（F17）。需要 CNI 真正执行；要跨 infra/zone namespace 放行，并列全内部调用方。
2. gate→路由服认证：mTLS，或者 gate 用一把**独立的** HMAC 密钥签名、路由服验签，不与 login 的 callerauth 共用密钥。
3. 1、2 完成之后，路由服才可以剥掉入站的 `x-caller-*`，按 `route.FullMethod` 用 login 的 callerauth 密钥代签。gate 在号段模式下不知道目标方法，本来就签不了。这一步解决 K1。
4. 透传从「`x-` 前缀」收窄为显式清单：`x-session-detail-bin`，加上路由服自己生成的 `x-caller-*`。实施前 grep 全仓，确认没有其他 `x-` 键。

先做 3 再做 1、2，路由服就会变成签名预言机，这是禁止的。第 3 步完成之前，login 在 pro 档（staging/prod）会拒掉经 gate 的 Login、EnterGame、Disconnect【推】，所以**任何非 dev 部署都以 §15 完成为前提**，与号段切换无关。

### 16. 实施路径：前置分步做，号轴一步到位

| 阶段 | 内容 | 依赖 |
|---|---|---|
| 0 | 与号段无关的现存缺陷，可以立即各自上线：<br>a 注册表空槽安全：生成 `const RpcMethodMeta* LookupMeta(id)`，改 `game_channel.cpp:356-358,455-456,511-513` 与 `scene_handler.cpp:76,314,386,623`（修今天 id=3 的 UB）<br>b `BuildSessionDetails` 把 player_id 归一<br>c MessageLimiter 的 uint8 截断（`message_limiter.cpp:8,17`、`.h:10,15`），CheckMessageLimit 的日志与 error_reporter 改采样<br>d 失败回执带请求 id：`main.cpp:323-348`、`client_message_processor.cpp:820,831-834`，连接仍按会话查找后 `lock()`，不在 call 对象里存 `TcpConnectionPtr`（§11.7）<br>e robot 删除 `sendDisconnectBestEffort`（§14）<br>f 路由服启停顺序（§13.8）<br>g 节点摘除：先移除 NodeInfo 使其不再被选中，保留 CQ、stub、Channel，等在途归零或超过实际 deadline 加余量后再销毁实体（F29）<br>h guild、friend、trade 的拒绝日志改采样 | 无 |
| 1 | K8s 翻到路由模式，路由服 ≥2 副本，在路由模式下跑通 battle-smoke → **删除直连路径**：`HandleGrpcNodeMessage`、直连白名单、回包反查表、battle 绑定与 match_event 处理、路由模式开关本身、`main.cpp:26` 的死 include、`SendMessageToPlayerOnGrpcNode`。修订 D-12，以及 `gate_security_test.cpp:160-168`「默认必须落在旧模式」的断言 | 0 |
| 2 | **一次停服切换**（清单见 §16.1） | 1 |
| 3 | 安全前置（§15） | 可与 1、2 并行，但内部顺序定死；任何非 dev 部署前必须完成 |
| 4 | 稳态：按 §17 新增 Go 客户端方法，gate 不动 | 2 |

#### 16.1 阶段 2 切换清单（同一批，停服执行）

1. 生成器：axis 与 state 一次性分配（含 NODE 重排与 0 号保留）、方法级 option 补齐、按节点生成注册表与 grpc_client、GO_DOWNLINK 与 GO_INTERNAL 移出 C++ 生成物、路由表拆分、`message_id_bands.h`。
2. proto：`OptionMethodVisibility`、`OptionGateHook`、`OptionZoneScopedService`、`NotifySessionClosed`、`SessionDetails.session_incarnation=7`、`NodeInfo.message_axis_version`（实施前复核字段号），以及约 15 个客户端服务文件的方法级 option。
3. gate G1：§12 全部。
4. 路由服 R1：§13 全部。
5. login：Disconnect 改用 metadata 的 session_id（§14）。
6. 全部 Go 服务重新生成、重新编译（推送常量随号变化）。robot 重新生成并刷新 vendor；路由服测试 `forwardlogic_test.go:231-253` 按新表结构重写。
7. 客户端仓（需 §9 授权）：Unity 桩的数字字面量、`MessageIds.cs`、`tools/gen_messageids.ps1` 改读 state。
8. MessageLimiter：schema 改为方法名作键（或加 `message_ref`），删掉 47 行 Go 号，第 68 行人工定夺，重新导出（D64）。
9. 数据：Kafka offset reset（队列里的 PushToPlayer 带着旧号）、Redis 与 etcd 清空（AGENTS §6.2 已有流程）。切换前先 grep 一遍，确认没有别的持久化存了消息号。
10. 文档：§20 的漂移清单。
11. 发布门禁：核对所有 gate 的 `message_axis_version` 与路由服的 `route_table_info` 一致。

#### 16.2 并存与回退

- 切换之后只剩「路由服表滚动」这一类并存：
  - R1 的表里已有该方法 → 正常转发；
  - R1 还没有该方法（滚动中）→ 回 1013，**不计**非法包，客户端稍后重试即可。
- 切换本身的回退 = 整套回到上一个发布集：全部镜像、客户端包，加上同样的数据清理。这是停服切换的固有性质，项目未上线时可以接受。
- 切换之后，功能级回退走路由服按方法的 killswitch 和客户端功能开关。路由服回退到没有某方法的旧表时回 1013，不踢线。
- 不再存在路由模式开关，F18 的问题随之消失。

### 17. 稳态：新增一个 Go 客户端方法（零 C++ 改动）

1. 在 proto 里写 rpc 与 `OptionMethodVisibility`（C2S 或 C2S_PRELOGIN）；如果该服务在 ROUTER 大区还没有 extent，先在 axis 文件里加一行，经评审。
2. 跑 proto-gen，号由 state 发出。
3. 业务服务：身份只从会话取（D-9）；需要逐方法限流就在服务内用 Redis 配额。
4. 核对 gate 聚合桶容量仍 ≥ 该功能的合法突发（§12.3）。
5. 部署该 Go 服务 → 全量部署路由服 → 核对 `route_table_info`。
6. 客户端重新生成桩后开始使用（客户端仓授权另计）。

gate 不重编、不重启。

### 18. 验证清单（交 Codex 执行；Claude 不执行，AGENTS §10.1）

工作目录都是仓库根目录，按顺序执行，任何一步红了就停，保留失败日志摘要。

1. **proto-gen**：
   - **先把现有的 `proto-gen.exe`（或 `pbgen.exe`）另存一份**，因为 `proto-gen-build` 会覆盖它；
   - 再跑 `pwsh tools/scripts/dev_tools.ps1 -Command proto-gen-build` 和 `-Command proto-gen-run`（`protoc` 要在 PATH 里）。期望：`message_id.txt` 首行是毒丸，state 已生成，各号落在所属大区与 extent，`legacy_ids` 完整；
   - 最后用另存的旧 exe 跑一次，期望 Fatal。
2. **生成器单测**：在 `tools/proto_generator/protogen` 下跑 `go test ./...`。覆盖：
   - 不复用、墓碑、号全局唯一；
   - 同一 extent 的 `next` 冲突；
   - state 缺失时 fail-closed，bootstrap 防重入；
   - extent 重叠、可见性与大区不匹配、缺 option、GM_DEV 进 ROUTER、battle 进 ROUTER、0 号被发出；
   - HEAD 只增不减、`generateConstants` 读 state、hook 唯一性。
3. **Go**：`go test ./...` 覆盖 `go/client_rpc_router`、`go/login`、`go/guild`、`go/friend`、`go/trade`、`go/match`。
   - 路由服：遍历全部非 ROUTER 号都回 1013，且 `out_of_band` 计数；player_id 为 {0, UINT64_MAX} 调 C2S 回 1005；PRELOGIN 放行；body 上限；会话头按原字节回写；忽略 `ClientRequest.service`；`NotifySessionClosed` 经 hook 到达 login，且按 zone 选实例；启停顺序。
   - login：Disconnect 用 metadata 的 session_id，空请求体可用。
   - robot：刷新 vendor 后 build 与 vet。
4. **C++**：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`，**必须串行**，期望 0 error。新增 gate 单测，照 `cpp/nodes/gate/tests/gate_security_test.cpp` 的独立编译方式，并登记进 `run_cpp_tests.ps1`：
   - 分类：各大区边界 lo-1、lo、hi-1、hi；0 号；GO_DOWNLINK、GO_INTERNAL 被 Reject 且计数；battle 回 tip 不计；
   - 聚合桶：喷洒 W 个不同的 ROUTER 号，桶数恒为 1；软限不计，硬限计；
   - 在途：成功、失败、过期、会话已断、incarnation 不一致、节点摘除，这 6 种完成路径各自只扣减一次，进程计数不泄漏；
   - 栅栏：incarnation 不一致或为 0 时丢弃；
   - `routerTouched` 触发 NotifySessionClosed；
   - `BuildSessionDetails` 归一；
   - `LookupMeta` 空槽（含 id=3）：GameChannel 回 NO_SERVICE 不崩，scene 拒绝；
   - 转发前 `service/method` 已清空。
5. **冒烟**（清库流程按 AGENTS §6.2）：
   - 登录、进场、帮会、好友、聊天、组队、匹配、聚宝斋全部通过；
   - `skip_direct_connect` 的 battle 回落路径收到 tip，不踢线；
   - 客户端发 S2C 号、内部号、login.Disconnect：计非法包，到阈值被踢，拿不到业务回包；
   - 滚动路由服期间持续有流量：没有人被踢，在途请求要么拿到应答、要么拿到带 id 的 tip，不能沉默；
   - 断线后 player_locator 进入 DISCONNECTING，30 秒后清理。
6. **阶段 3 之后**：在 login `Mode=pro` 下重跑第 5 步。
7. **压测**：给聚合桶、在途上限定档，观察 `PickRandomNode` 的开销（R2）。按 AGENTS §6 流程出对比表，没有对比表不宣称任何性能结论。

### 19. 待决、风险、相邻缺口

- **Q1** scene 段是否也整段转发，可见性校验下沉到 scene。做了之后，新增 scene 客户端方法也不用重编 gate。
- **Q2** Go 逐方法限流的归属：业务服务 Redis 配额（v1 默认），还是路由服加粘性路由。
- **Q3** RefreshToken 等方法是否 C2S_PRELOGIN，要看 login 的实现。
- **Q4** Unity 端是否发 58：切换后会机械暴露（§14）。
- **Q5** 聚合桶、在途上限的数值，由压测定。
- **Q6** guild B6a 的 5 个新 rpc 如果必须在切换前给客户端用：先用旧发号器临时发 239–243，切换时再搬（开发期允许，AGENTS §4.3）。需要和帮会会话协调。
- **Q7** 是否在 NODE 内划 BATTLE 子区。
- **Q8** 是否给 gate 加 ROUTER 段未知号的负缓存（16384 位的位图，与会话无关、上限固定）。unknown 不计数，只受聚合桶约束；借 F7 的票据重放，一个账号可以开多条连接去打路由服查表。
- **R1** 切换是一次跨仓停服发布，协调失败的代价是整批回退（§16.2）。
- **R2** `PickRandomNode` 每次调用都新建 `std::random_device`（`client_message_processor.cpp:78`），切换后所有 Go 客户端流量都经过这里。没有 profiling 数据，本期不改（§11.3），列入压测观察项。
- **相邻缺口（登记，不在本设计范围）**：
  - gate 票据在 TTL（10 分钟）内可重放，没有一次性消费（F7）；
  - event_id 发号器同样复用空号（`generator/cpp/event_id.go:186-217`）；
  - K1–K9 见第一部分 §6。

### 20. 文档漂移修订清单（实施时同批修）

| 文档 | 需要修订的内容 |
|---|---|
| `client-rpc-router.md` | :9「gate 不再认识任何业务 Go 服务」补前提；:27 D30「回包桥接零改动」作废；:28 D31 改为方法级可见性、路由表拆分；:30 D33 zone 作用域改为 option；:31 D34 直连路径删除；:37 闸门顺序更正（F4）；:52「Kafka gate-{id}」已过时，实为 `gate-cmd`；:76「客户端零改动」作废；:87 回退说法更正；:96 §7.6 失败桥现状 |
| `ARCH.md` | :96「gate 只连一类 gRPC 目标」、:228「加服务不碰 gate」加上号段前提 |
| `xuanming-port-decisions-20260910.md`、`friend-handoff-20260920.md` | D-12「C++ 默认值和单测一字不改」由 D61 取代 |
| `mail-system.md` | :604、:784-794 里的 MessageLimiter 与「gate 重编」步骤 |
| `leaderboard-system.md` | :332、:409、:506，同上 |
| `jubaozhai-market.md`、`guild-zone-client-access.md` | :342、:41 等写死的消息号 |
| `team-system.md`、`player-pet.md`、`friend-client-spec-20260920.md` | 「从 message_id.txt 现场读号」改为读 state |
| `dual-token-authentication.md` | :39 票据 TTL 写的是 300 秒，代码是 10 分钟 |
| `tools/scripts/go_svc_image.ps1` | :155「路由服先、gate 后」的注释 |
| AGENTS §4 | 类型清单登记 `session_incarnation` 为 uint64（规范变更，需用户同意） |

新决策从 D51 开始编号，仓内此前最大是 D50（`turn-battle-gap-closure.md`）。

### 21. 改动规模与授权

涉及以下范围：
- **生成器**：发号器、axis 与 state、route_table、按节点生成的 C++ 注册表与 grpc_client 模板、bands 头文件、导表器 message_ref、测试；
- **proto**：option、NotifySessionClosed、SessionDetails、NodeInfo，以及约 15 个服务文件；
- **C++**：gate 约 5 个文件加新测试；engine 的 `game_channel`、`message_limiter`、`node`（摘除排空）；scene 的 `scene_handler`；CMake 与 vcxproj；
- **Go**：路由服约 8 个文件、login，guild、friend、trade 的日志，全部服务重新生成；
- **其他**：robot（约 20 个调用点加 vendor）、脚本（k8s_deploy、两个 patch 脚本、dev_tools）、MessageLimiter 的 schema 与数据、全部生成物、文档；
- **客户端仓**：见 §16.1 第 7 条。

合计远超 30 个文件。按 AGENTS §10.2 先报告、取得授权后才能动手，客户端仓另需 §9 授权。阶段 0 的各项彼此独立，可以按小批次分别授权。

## 附录 A　评审与核验记录

### A.1 接入形态评估（第一部分 §5 的依据）

| 方案 | 评审找出的主要问题 |
|---|---|
| A 照搬 xuanming | 代际 ABA；短线重连绕过顶号围栏；服务级白名单放出 UpdateGuildScore；会话证明缺 CZ-8 票据字段；幽灵 ONLINE 会话；gate→login 的 Disconnect 在 prod 被拒。另外 robot 冒烟与 §6 压测在收缩阶段之后跑不了；LeaveGame、CreatePlayer 依赖 session_id；账号 JWT 不带 zone；共享 Redis 是 allkeys-lfu，会静默淘汰代际键 |
| B 单连接 + 路由服 | gate 签名需要先改生成模板（registry 缺 package 名，sender 会对 metadata 做 Base64）；断线通知丢失 = 永久 ONLINE；login 缺方法白名单；nonce 按进程存、路由服随机选实例，挡不住换实例重放；路由服是跨 zone 的全局单点 |
| C 按域混合 | K8s 默认直连模式下收益为零、回退路径不存在；打算复用的 Go muduo codec 是机器人客户端的实现，没有边界检查，一个 8 字节包就能让它 panic；发号器复用空号；双入口导致双倍限流配额 |

### A.2 号段设计：三视角对抗评审的主要结论（已在正文落实）

- **安全**：限流键无界、没有在途上限 → §12.3、§12.4；段内未知号不计数、踢线可被票据重放绕过 → 聚合桶加在途上限为主防线，踢线为辅；路由服代签会变成签名预言机 → §15 顺序；可见性过渡规则 fail-open → 缺 option 就报错；登录态判据要兼容 UINT64_MAX → §13.2、阶段 0b。
- **兼容**：空槽 `find(nullptr)` UB → 阶段 0a；断线通知新旧并存 → 改走专用 RPC，并存窗口消失；旧 gate 遇到段内号静默丢包并踢线 → 停服切换加版本信号；段满只能重排 → 多 extent；发号器并发与旧二进制 → §11.2。
- **管线**：battle 需要单独一种结果 → BattleViaGate；ROUTER 号不得进 `CanSend`；回包桥接要有身份栅栏 → session_incarnation；失败回执带 id → 阶段 0d。

### A.3 定稿核验时修正的问题

| 位置 | 核验发现 | 处理 |
|---|---|---|
| 旧稿 §9：「login 改用 metadata 的 session_id 能关闭伪造 Disconnect」 | 不成立。改完之后伪造的 58 号必然命中自己的会话 | 与 58 离开客户端可达集同批生效（§14） |
| 旧稿 §6.3 在途上限 | 归还没有按单次调用记账，会重复归还或永久泄漏；过期阈值写死 8000，而 K8s 实际是 10000 | 按调用记账，读实际 deadline（§12.4） |
| 旧稿 §6.2、§6.3 超限计非法包 | 后端变慢会让正常玩家被踢 | 在途超限不计；聚合桶分软限、硬限 |
| 旧稿 §6.5 nonce | 来源、类型、生命周期都没定义；「0 是旧版本」的前提不成立；断线通知会漏填 | §12.6 定死 session_incarnation |
| 旧稿 §7.2 origin 与 §9 的 58 号过渡 | 可见性单一事实源下，58 在阶段 1 只能二选一，两种选法都会丢断线通知或误踢；AllowUnspecifiedOrigin 的默认值也没写 | 改用专用 RPC `NotifySessionClosed`，删掉 origin 字段和并存窗口 |
| 旧稿 §5 发号器 | 同一服务在两处并行发号会撞号；bootstrap 可重入导致墓碑丢失；毒丸会打坏 `generateConstants` | §11.2 |
| 旧稿 D52、Q1（legacy 冻结区、渐进路径） | 用户要最标准的做法，项目也未上线；渐进路径要先建再拆多套过渡机制 | 改为一次停服切换（D52、§16） |
| 旧稿 Q1 的波及面 | 列了不需要搬的 C++ 号，漏了 Go 推送常量、客户端脚本、限流表 | §10.4 的搬迁清单 |
| 旧稿 §7.4 未登录用 1012 | 1012 是 fault 码 | 改用 1005 |
| F7、F10、F11、F15、F21 | TTL、今天已有空槽 id=3、第三个 handler、login 用 body 的范围、K8s deadline | 已在 §8 更正 |
| 旧稿 R1 节点摘除 | 在途调用会永久丢失，且 call 对象泄漏 | 改为阶段 0g 的必修项 |
