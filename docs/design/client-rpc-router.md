# 客户端 RPC 路由服(client_rpc_router):gate 唯一的 gRPC 目标

**Created:** 2026-09-05
**状态:** 决策已定,实施中(D29–D34);验证清单见 §7
**关联:** [ARCH.md](./ARCH.md)(总拓扑)、[turn-based-battle-server.md §18](./turn-based-battle-server.md)(战斗直连;本文是它的"收缩阶段")、[player_login_flow.md](./player_login_flow.md)、[gate-scene-relay-architecture.md](./gate-scene-relay-architecture.md)、[client-access-band-routing.md](./client-access-band-routing.md)(续篇:gate 按号段整体转发、路由服为唯一白名单,D51–D64,修订本文 D30/D31/D33/D34 的部分表述,见其 §20)
**2026-09-29 更新:** 战斗直连收缩(turn-based §22 D65–D75)已落码。本文 §1、D33、D34、§3、§4、§5、§6、§7.4、§7.5 里涉及 battle 中继、BindBattle/UnbindBattle、`skip_direct_connect` 回落、「默认旧模式」、「部署链待补」的表述,都在原文旁加了更正标注,原文未删。这些改动**未编译、未测试,待 Codex 验证**。

## 0. 一句话

gate 不再认识任何业务 Go 服务:客户端可见的 gRPC 类消息一律**原样**转给一个无状态的 Go 路由服,由它按**生成的路由表**以原始字节转发到 login / match / chat / …;gate 的 gRPC 白名单从"每个业务服务一类"收成**只有路由服一类**,连接数 = 路由服副本数,与业务服务数量无关。

## 1. 问题

gate 是 C++、有状态(持有全部客户端会话)、对公网的唯一入口。今天它对每个客户端可见的 Go 服务都要:

- 白名单加一类节点类型、`service_discovery_prefixes` 加一行;
- 编进该服务生成的 typed gRPC sender 与回包类型反查表;
- 对每个实例各持一条 gRPC channel(login / scene_manager / match / battle 四类 × 实例数);〔2026-09-29 更正:battle 一类已随收缩移出 gate 白名单,两种模式都不连(turn-based §22 D66,`cpp/nodes/gate/main.cpp:218-219`),旧模式剩 login / scene_manager / match 三类〕
- **重编 7 分钟 + 滚动重启踢在线玩家**。

加 chat / friend / guild / team 每个都要来一遍。"连接多"不是 socket 的问题,是**业务变化必须碰有状态的公网入口**。

## 2. 决策

| # | 决策 | 理由 |
|---|------|------|
| D29 | 新增无状态 Go 服务 `client_rpc_router`(节点类型 `ClientRpcRouterNodeService = 29`,`NODE_CLIENT_RPC_ROUTER = 31`),全局池不分 zone,照 match 用 `noderegistry` 以 `PROTOCOL_GRPC` 注册到 etcd 前缀 `ClientRpcRouterNodeService.rpc/` | gate 只需发现一类节点;路由服无会话、无权威态,可水平扩、可随时重启 |
| D30 | **gate 不透明转发**:`ClientRpcRouter.Forward(ForwardRequest{ClientRequest 原包, zone_id}) returns MessageContent`;会话身份照旧走 gRPC metadata `x-session-detail-bin`,路由服透传给目标并在响应头回写;响应类型直接是 `MessageContent`,gate 现有回包桥接(对 MessageContent 原样下发)零改动 | gate 不解析 body、不挑业务实例、不持业务 stub;加业务服务只改 proto + 重生成 + 重部署路由服 |
| D31 | **路由表是生成物**:proto-gen 在 `go/client_rpc_router/generated/pb/game/route_table.go` 输出 `message_id → {gRPC 全限定方法, 目标节点类型, 是否客户端协议}`,与该服务的 `message_id.go` 同包、键用生成常量(编译期校验) | 路由知识只有 proto 一处事实源;手抄表必漂移(tip 码轴刚踩过) |
| D32 | **原始字节转发**:路由服用自定义 codec 把 `ClientRequest.body` 原样作为请求载荷 `Invoke` 目标方法,响应字节原样进 `MessageContent.serialized_message`;不为任何业务 proto 生成 stub | gRPC 载荷本来就是 protobuf 字节;路由服对业务协议零依赖 |
| D33 | **目标实例选择**:节点类型 ∈ 配置 `ZoneScopedNodeTypes`(默认 `[LoginNodeService]`)只挑与 `ForwardRequest.zone_id` 同 zone 的实例;其余(match / scene_manager / …)全局随机。目标为 `BattleNodeService` 的消息**不转发**(回 `kServiceUnavailable`):战斗流量走直连(§18),票据补签改由 `MatchService.RequestBattleTicket → BattleNode.IssueBattleTicket` | 与 gate 今天的 `IsZoneScopedNodeType / IsGlobalPoolNodeType` 语义一致;battle 的会话绑定只有 gate 知道,路由服不该复制这份状态〔2026-09-29 更正:后半句理由已失效,改为「gate 不再持有 battle 绑定」,见表后修订注〕 |
| D34 | **gate 双模式、默认旧模式**:环境变量 `GATE_CLIENT_RPC_ROUTER=1` 时白名单 = `{Scene(TCP), ClientRpcRouter}`,gRPC 类消息、断线通知(`ClientPlayerLogin.Disconnect`)全部经路由服,BindBattle/UnbindBattle 事件忽略;未设或 0 时行为与改前完全一致。默认值在冒烟通过后翻转,再删旧路径 | AGENTS §11.3 expand → migrate → contract;默认不坏(§14.2)〔2026-09-29 修订,见表后修订注〕 |

> **D33 更正(2026-09-29)**:决策本身不变,路由服仍然不转发目标为 `BattleNodeService` 的消息(`go/client_rpc_router/internal/logic/forwardlogic.go:133-136`)。变的是理由。原理由是「battle 的会话绑定只有 gate 知道,路由服不该复制」,现改为:**gate 也不再持有 battle 绑定**。
> - 收缩后 gate 两种模式都不连 battle、不维护战斗会话绑定(turn-based §22 D66)。
> - Kafka `BindBattleEvent` / `UnbindBattleEvent` 契约已删(D67)。
> - 战斗上行与战斗帧只走客户端直连,没有谁需要替路由服记绑定。
>
> gate 已在 `DispatchClientRpcMessage` 分派之前统一拒绝 battle 消息(`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:936-950`),这类消息到不了路由服。路由服这条拒绝分支因此是纵深防御。
>
> **D34 修订(2026-09-29)**:
> 1. **Bind/Unbind 契约已删**。「BindBattle/UnbindBattle 事件忽略」不再适用:两个事件从 `proto/contracts/kafka/gate_event.proto` 删除,事件号 43/44 用 `N=reserved:<原名>` 墓碑永久保留(D67,生成器重生后写入 `proto/event_id.txt`)。gate 侧 `battle_binding_helper.{h,cpp}` 也已删除。两种模式下 gate 都不中继战斗,战斗消息一律回 `kServiceUnavailable`(D66)。
> 2. **本地和 K8s 部署默认都是路由模式**:
>    - 本地一键启动:`tools/scripts/start_game.ps1:22`,`-GateRouterMode` 默认 `'1'`。
>    - K8s:`tools/scripts/k8s_deploy.ps1:164`,`-GateRouterMode` 默认 `"1"`(D75)。
>    - `dev_tools.ps1` / `k8s_image.ps1` 的同名参数留空时不覆盖,由 k8s_deploy 的默认值接管;非空时透传。
>    - 翻转前置的「K8s 路由模式 battle-smoke 先跑通」由用户豁免,事后补验(D65)。
> 3. **C++ 默认值按 D-12 不改**:`gate_router_mode.h` 的解析函数与默认值(未设或非 1/true/on 即旧模式)一字不改,`gate_security_test` 的相关断言也不改(D-12 及其 2026-09-29 修订,见 [xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md))。所以直接起 C++ 进程、或经 `dev_tools.ps1 dev-start` 继承未设变量的父 shell 时,仍是旧模式。
> 4. 「默认值在冒烟通过后翻转,再删旧路径」的后半句仍未执行,旧模式和开关本身都保留,`"0"` 只作回退。client-access-band-routing.md D61 计划删除直连模式和开关本身,并修订 D-12,该文档状态为「尚未实施」;在它落地之前,以本修订注为准。

## 3. 转发契约

```
客户端 ── ClientRequest{id, message_id, body} ──▶ gate
gate:    会话已验证 → 体积 ≤1KB → 每消息号限速 → IsClientMessageId 白名单
         → messageInfo.protocol == PROTOCOL_GRPC
         → ForwardRequest{request=原包, zone_id=本 gate zone}
         → metadata x-session-detail-bin = base64(SessionDetails{session_id, player_id, gate_node_id, gate_instance_id})
         → PickRandomNode(ClientRpcRouterNodeService) → SendClientRpcRouterForward(生成)
路由服:  route.Table[message_id] → {FullMethod, NodeType, ClientProtocol}
         → 未知 / 非客户端协议 → MessageContent{id, message_id, error=kInvalidParameter}
         → NodeType==Battle → error=kServiceUnavailable
         → 选实例(zone-scoped 按 zone_id;否则全局随机)→ 无实例 → error=kServiceUnavailable
         → conn.Invoke(FullMethod, body 原始字节, 透传全部 `x-` 前缀 metadata(含 x-session-detail-bin;目标看到的 metadata 与 gate 直连时相同), 超时 ForwardTimeoutMs)
         → gRPC 错 → error=kServiceUnavailable(日志记 code / 目标)
         → MessageContent{id, message_id, serialized_message=响应原始字节};响应头回写 x-session-detail-bin
gate:    回包桥接 → 按响应头找会话 → MessageContent 原样写回客户端 TCP
```

不变的部分:Scene 消息仍走 gate→scene 的 muduo TCP(`HandleTcpNodeMessage`);服务端推送仍是 Go 服务 → Kafka `gate-{id}` → 客户端;Go 服务侧的 `SessionInterceptor` 一行不改(它看到的 metadata 与 gate 直连时完全相同)。

> (2026-09-29 更正)`NodeType==Battle → error=kServiceUnavailable` 这一步在路由服里仍然存在。但收缩后,gate 在 `IsClientMessageId` / 体积 / 限速 / GM 几道闸之后、按协议分派之前,已经对 `BattleNodeService` 消息统一回 `kServiceUnavailable`(turn-based §22 D66,`client_message_processor.cpp:936-950`)。这类消息实际到不了路由服,路由服这一步只是纵深防御。

## 4. 改动集

| 位置 | 改动 |
|---|---|
| `proto/common/base/node.proto` / `proto/db/proto_option.proto` | `ClientRpcRouterNodeService = 29` / `NODE_CLIENT_RPC_ROUTER = 31` |
| `proto/client_rpc_router/client_rpc_router.proto`(新) | `ClientRpcRouter.Forward` + `ForwardRequest` |
| `proto/match/match_service.proto` / `proto/battle/battle_node.proto` / `proto/battle/player_battle.proto` | 补签改道:`MatchService.RequestBattleTicket`(客户端协议)+ `BattleNode.IssueBattleTicket`(内部);`BattleClientPlayer.RequestBattleTicket` 删除,请求/响应消息保留复用 |
| `tools/proto_generator/protogen`:`internal/route_table.go`(新)、`cmd/pipeline.go`、`internal/config/config.go`、`etc/proto_gen.yaml` | 路由表生成器 + `client_rpc_router` domain |
| `go/client_rpc_router/`(新) | go-zero 服务:`Forward` 实现、原始字节 codec、按节点类型的 etcd list-watch 与连接缓存(照 match 的 `discovery`)、zone 过滤、metrics、`noderegistry` 注册 |
| `go/match` | `RequestBattleTicket` logic:`spectate:battle:{battle_id}` 取 `battle_node_id` → `BattleNodes.EndpointOfNode` → `BattleNode.IssueBattleTicket` |
| `cpp/nodes/battle` | `BattleNodeImpl::IssueBattleTicket` → `BattleRoomManager::HandleIssueBattleTicket(battle_id, player_id)`(原 `HandleRequestBattleTicket` 去掉会话入参);删 `BattleClientPlayerGrpcImpl::RequestBattleTicket`〔2026-09-29 更正:收缩后整个 `BattleClientPlayerGrpcImpl`(gate→battle 中继的 gRPC 服务端)都已删除,battle 不再实现 `BattleClientPlayer` 的 gRPC 服务(turn-based §22 D66,`cpp/nodes/battle/main.cpp:92-93`)〕 |
| `cpp/nodes/gate` | `GATE_CLIENT_RPC_ROUTER` 模式:白名单 / 依赖门 / `HandleGrpcNodeMessage` 转发分支 / 断线通知走路由服 / Bind-Unbind 事件忽略〔2026-09-29 更正:「Bind-Unbind 事件忽略」已不存在。Bind/Unbind 契约与 gate 侧 battle 绑定已整体删除(D66/D67);旧模式白名单去掉了 battle,剩 `{Scene, Login, SceneManager, Match}`(`main.cpp:218-219`);战斗消息在两种模式下都于分派前拒绝〕 |
| `cpp/libs/engine/core/node/system/node/node_util.cpp` | 名字表 + `IsGlobalPoolNodeType` 加路由服 |
| `bin/etc/base_deploy_config.yaml` | `service_discovery_prefixes` 加 `ClientRpcRouterNodeService.rpc` |
| `tools/scripts/go_services.ps1` | 注册 `client_rpc_router`(Tier 1) |
| `robot/logic/handler` | `RequestBattleTicket` 响应 handler 随消息号迁到 MatchService。注意生成的 `message_body_handler.go` 只收服务名含 `ClientPlayer`/`GamePlayer` 的服务(`robot_case.go` 的 `isRelevantService`),MatchService 的应答从来不在里面 —— 现有 JoinQueue / WatchBattle handler 一直是死代码;本次在手写的 `match_service_responses.go` 用 `init()` 把三条 MatchService 应答登记进分发表,不改生成物 |
| 文档 | 本文、ARCH.md 决策行与拓扑、turn-based §18.7 收缩阶段、PROGRESS |

## 5. 不做 / 后续

- 不把 Scene 的 TCP 路径搬进路由服:那是 MMO 场景中继,会话绑定在 gate,与本文无关。
- 不让路由服转发 battle 消息:见 D33;直连是唯一的战斗通路,gate 中继在路由模式下即为收缩。〔2026-09-29 更正:收缩已在**两种模式**下完成,旧模式下 gate 也不再中继战斗(turn-based §22 D66)〕
- 客户端零改动:线协议、消息号、S2C 通道都不变。
- 后续:默认翻转到路由模式后删除 gate 的 typed sender 路径与四类白名单;`go/match/internal/discovery` 与路由服的发现代码收成 `go/shared` 一份。〔2026-09-29 现状:翻转只做在部署层,本地与 K8s 默认都已是路由模式(D34 修订注)。C++ 默认值按 D-12 不改,所以 typed sender 路径与旧模式白名单都保留。白名单已剩三类 Go 目标:login / scene_manager / match,battle 已移出。删除旧路径按 client-access-band-routing.md D61 另行排期,尚未实施〕

## 6. 运维

- 路由服配置:`ListenOn 127.0.0.1:50600` / `Timeout 6000`(zrpc 整体超时必须 > `ForwardTimeoutMs`,否则慢目标会被服务端先掐成故障,`config.Validate` 强制)/ `Etcd` / `ZoneId`(仅注册路径用)/ `LeaseTTL` / `ForwardTimeoutMs`(默认 5000)/ `ZoneScopedNodeTypes`(默认 `[LoginNodeService]`,枚举名解析失败 fail-fast)/ `MetricsListenAddr`(默认空;开启建议 `:9200` —— 9180 已被 friend 占用,guild 与 match 都写 9170 是既有撞号)。
- 副本数 ≥ 2;gate 的 gRPC 连接数 = 副本数。
- 观测:`client_rpc_router_forward_total{message_id, outcome}`(outcome ∈ ok / missing_session / invalid_request / unknown_message / not_client_protocol / battle_rejected / no_target / dial_error / upstream_timeout / upstream_error)、`client_rpc_router_forward_seconds{message_id}`(不在路由表的消息号归 `unknown`,基数有界)、`client_rpc_router_targets{node_type}`。
- 部署链待补(与 battle 同批):`tools/scripts/k8s_deploy.ps1` 第 307 行附近的部署服务表与 ConfigMap 模板没有路由服;`cpp/generated/{proto,grpc_client}/CMakeLists.txt`(Linux)本次只加了 client_rpc_router,battle / match 的条目原本就缺,是既有缺口。
  - **(2026-09-29 更正)已补:**
    - `k8s_deploy.ps1` 的 `$GoSvcCatalogue` 有 `client-rpc-router` 条目(`:716`,infra namespace 全局一份,端口 50600)。
    - ConfigMap 模板带上了 `IgnoreContentMethods: [/client_rpc_router.ClientRpcRouter/Forward]`(`:3550-3553`)。
    - 清单文件是 `deploy/k8s/manifests/go-svc/client-rpc-router.yaml`。
    - Linux 的 `cpp/generated/grpc_client/CMakeLists.txt` 已含 battle / match 条目(`:55-57`)。
  - **新的前置约束**:K8s 默认路由模式下,gate 的依赖门等的是 ClientRpcRouter + Scene(`cpp/nodes/gate/main.cpp:403`)。所以 infra-up 必须已部署路由服:不带 `-SkipGoSvc`,且给了 `-GoSvcRegistry`(`k8s_deploy.ps1:150`)。否则 gate 卡在依赖门,登录和匹配全部 no_target。
  - **已知缺口**:`k8s_image.ps1` 的发布路径从不部署 Go 服务,在全新集群上会踩到上面这条;目前只有注释说明,没有运行时拦截,待负责发布链的会话决定。
- **必配 `Middlewares.StatConf.IgnoreContentMethods: [/client_rpc_router.ClientRpcRouter/Forward]`**:go-zero 的 Stat 拦截器默认开启,会把 `ForwardRequest` 整包 JSON 打进 INFO —— 里面的 `body` 是客户端原包,base64 一解就是目标请求原文,登录消息即**明文密码**;而且路由服汇聚全网 gRPC 类客户端消息,逐请求 INFO 等于把消息速率放大成日志速率。`config.Validate()` 对此 fail-fast(Stat 开着又没屏蔽就拒绝启动),k8s ConfigMap 模板补路由服时必须带上这一段。
- 失败分支日志按 outcome 分别采样(每 1024 条一行),与 gate 的拒绝日志同口径;精确计数看指标,不看日志。
- 灰度:先起路由服(旧模式 gate 不会连它),再给 gate 设 `GATE_CLIENT_RPC_ROUTER=1` 滚动;回退 = 去掉环境变量再滚动。
  - **(2026-09-29 更正)** K8s 默认已是 `"1"`(D75),「去掉环境变量」不再等于回退:k8s_deploy 会按默认值重新写出 `"1"`。回退要显式传 `-GateRouterMode 0`,包装入口(dev_tools / k8s_image)也要显式再传一次,否则静默落回 `"1"`(`k8s_deploy.ps1:159`)。
  - 回退的代价:chat / guild / friend / trade 等只承诺路由模式的服务立刻不可达;战斗也没有了 gate 中继兜底,两种模式下都只走直连。回退前按 D-12 先用 killswitch 关掉这些方法并发公告。

## 7. 验证清单(按序;任一步红即停)

1. proto-gen:`proto-gen-build`(二进制必须重建,见 §18.8)→ `proto-gen-run`(`protoc` 在 PATH)。**2026-09-05 已跑**:`message_id.txt` 176=ClientRpcRouterForward、177=BattleClientPlayerNotifyBattleAssigned、178=BattleNodeIssueBattleTicket、179=MatchServiceRequestBattleTicket(开发期复用了被删 RPC 的 176,AGENTS §4.3 允许,须完整重编);`go/client_rpc_router/generated/pb/game/{message_id,route_table}.go` 出现(95 条,gofmt 过);`cpp/generated/grpc_client/client_rpc_router/` 出现;`message_body_handler.go` 不含 MatchService 条目(见 §4 robot 行)。
2. Go:`go/proto`、`go/client_rpc_router`(build + test:路由表覆盖、zone 过滤、原始字节 codec 往返、metadata 透传/回写、未知消息号 / battle 拒绝)、`go/match`(build + test)、`robot`(vendor 刷新后 build + vet)。
3. C++:`msbuild game.sln -m:1 Debug x64` 0 error(gate / battle / node_util / 生成物)。
4. 冒烟(旧模式回归):不设 `GATE_CLIENT_RPC_ROUTER`,`battle-smoke` 与直连断言照过 —— 证明 D34 默认不坏。
   - **(2026-09-29 更正)** 收缩后旧模式下的战斗同样只走直连(turn-based §22 D66),本步的含义改为:C++ 默认的旧模式下,直连战斗和非战斗的 typed sender 路径都不坏。
   - robot 的直连计数现在恒 ≥1,因为直连没有开关(D73)。
   - K8s 路由模式下的 battle-smoke 是 D65 / D75 豁免后的事后补验项,由谁跑、在哪个 kind namespace 跑,待用户决定。
5. 冒烟(路由模式):起 `client_rpc_router`,gate 设 `GATE_CLIENT_RPC_ROUTER=1`;登录 / 进场 / JoinQueue / 观战 / 直连战斗全过;gate 日志里 gRPC 目标只剩路由服;`netstat` 上 gate 到 login / match 端口零连接;`skip_direct_connect: true` 的回落路径在路由模式下**预期失败**(战斗消息经 gate 被拒),这是设计而非缺陷。
   - **(2026-09-29 更正)** 「`skip_direct_connect` 回落路径」这条判据作废。robot 的 `skip_direct_connect` 开关已删除(D73,`robot/config/config.go:161`)。经 gate 的战斗回落路径在**两种模式**下都不存在:gate 对战斗上行回 `kServiceUnavailable`,battle 不再经 Kafka 回落战斗帧(D66 / D68)。
   - 其余判据不变。team-smoke / features-smoke 的战斗段已改走直连(D73),不能再拿「经 gate 的 SetAutoBattle」当证据。
6. 负向:**路由服已发现但连接不可用**(进程刚被 kill、gRPC 传输层失败)时,生成的 gate 侧异步回调只打日志、不回包 —— 该窗口内的请求**没有回执**,靠客户端超时重试兜底(生成模板的 `!status.ok()` 分支也未采样,路由模式下全部 gRPC 类流量走这一条路径,滚动重启窗口会放大日志;要改得动 `tools/proto_generator` 的 grpc_client 模板,影响所有服务,单独排期)。路由服**全部下线**(发现镜像为空)则走 `PickRandomNode` 落空分支,客户端收到 `kServiceUnavailable`。
7. **认证档**:路由模式冒烟须在 login `Mode=pro`(强制 callerauth 验签)下再跑一次 —— 当前 C++ gate 并不签 `x-caller-*`,dev 宽松档会把这个既有缺口遮住。
