# battle 节点传输选型定谳(gRPC vs muduo TCP RPC vs Go)

**Created:** 2026-09-05
**状态:** 架构定谳(结论已拍板;实现进度以 [turn-based-battle-server.md](./turn-based-battle-server.md) 为准,本文不记录完成度)
**关联:** [turn-based-battle-server.md](./turn-based-battle-server.md)(D2/D6 原始决策)、[moba-battle-target-architecture.md](../notes/slg-moba/moba-battle-target-architecture.md)(会话制目标形态)、[scene-grpc-server-design.md](./scene-grpc-server-design.md)(gRPC vs Kafka 取舍表)、[ARCH.md](./ARCH.md) §6
**2026-09-29 更新:** 直连收缩(turn-based §22 D65–D75)与 K8s 集群外入口(D76–D93,全文见 [k8s-client-entry.md](./k8s-client-entry.md))已落码。§2.1、§3.1、§3.3、§4、§6 在原文旁加了现状标注,原文未删;§8 按 5 路对抗核验逐条订正,并改写为落码后的口径,订正明细见 §8.7(含改写稿的二次核验)。以上改动均**未编译、未测试、未上集群,待 Codex 验证**。

> **一句话:按平面拆传输,不按节点选传输。客户端面 muduo TCP 直连,Go→battle 控制面 gRPC,battle 保持 C++。**
> 当前是回合制,这个结论对回合制成立;将来做实时对局,只换客户端面(KCP),控制面不变。

---

## 1. 问题

三个问题一起回答:

1. battle 为什么用 gRPC service?
2. 既然用 gRPC,是不是做成 Go 更合适?
3. 能不能像 gate / scene 一样,直接用 muduo TCP RPC(RPC0)?

## 2. 结论

### 2.1 四条边,各用各的传输

| 边 | 传输 | 理由 |
|---|---|---|
| 客户端 ↔ battle | **直连**,票据入场,ProtobufCodec 帧(照搬 gate 的客户端面) | 战斗流量最高频,不能过 gate 这个共享漏斗;gRPC 不是游戏客户端协议(HTTP/2 头开销、无 UDP、移动端客户端重) |
| match(Go)→ battle | **gRPC unary**(CreateBattle / DestroyBattle / AddObserver / RemoveObserver / IssueBattleTicket) | 要应答的命令:需要请求关联、deadline、状态码;跨语言;每局一两次,gRPC 成本可忽略。「谁下命令谁当 client」与 scene_manager→scene 同款 |
| battle → 结算 | Kafka 事件,**result 先落库再发奖**,幂等键 battle_id | battle 随时可死,回写不能依赖发送方活着 |
| battle → gate | **没有这条边** | 客户端直连之后 battle 不需要向 gate 推任何东西;「战斗已分配」走既有 Kafka 推送 |

> **现状标注(2026-09-29)**:「没有这条边」指 gate 与 battle 之间没有网络连接、没有 gRPC、没有会话绑定。收缩后这一点已成立(turn-based §22 D66/D67):
> - gate 两种模式的出站白名单都不含 `BattleNodeService`(`cpp/nodes/gate/main.cpp:199-219`);
> - Kafka `BindBattleEvent` / `UnbindBattleEvent` 契约已从 proto 删除(`proto/contracts/kafka/gate_event.proto:98-101` 为删除说明)。事件号 43/44 将在 proto 重生后改写为 `N=reserved:<原名>` 墓碑(D67);当前 `proto/event_id.txt:44-45` 仍是原名,gate 的生成物 `gate_event_handler.cpp:342`、`:347` 的 Bind/Unbind handler 和 `gate_kafka_command_router.cpp:94`、`:101` 的分派也还在,重生后才消失。
>
> 剩下的只有一条 **Kafka 大厅公告**:玩家还没有活直连时,battle 把 `NotifyBattleAssigned` / `NotifyBattleStart` 经 `PushMessageViaGate` → Kafka `PushToPlayerEvent` → gate → 大厅连接下发(D68,`cpp/nodes/battle/logic/battle_room_manager.cpp:103-118`、`:1410-1430`)。战斗帧不走这条路。scene 发的 `NotifyBattleReconnect` 和结算后的 `NotifyBattleEnd` 是 scene 自己的大厅下行,不属于 battle→gate。
>
> match → battle 这一行也有补充:`CreateBattle` 的节点级准入拒绝走 gRPC `UNAVAILABLE "battle_not_allocatable"`,match 遇到后不发 DestroyBattle,换一个节点重试一次(D82,`cpp/nodes/battle/handler/grpc/battle_node.cpp:15-19`)。这偏离了「错误走 tip、status 恒 OK」的惯例,理由见 D82。

### 2.2 三个方案的排序

| 排序 | 方案 | 判定 |
|---|---|---|
| 1 | **客户端面 muduo TCP 直连 + 控制面 gRPC** | 标准形态,回合制当下就选它 |
| 2 | 全 muduo(控制面也走 RPC0) | 第二好。前提:先给 `GameRpcMessage` 加 request_id、写一个带超时/重连/按 message_id 派发的 Go 客户端。做完后 C++ 节点网只剩一种协议,值得,但不是现在 |
| 3 | 全 gRPC(客户端经 gate 中继 gRPC 到 battle) | 最差。HTTP/2 + 线程池进热路径;gate 每条消息 parse + SessionDetails 序列化 + Base64,battle 再解一次,gate 收应答再解一次。回合制勉强能扛,实时对局撑不住 |

### 2.3 语言:battle 保持 C++

传输不决定语言。battle 的语言由「战斗规则代码在哪」决定:技能、buff、伤害公式、六张战斗表的加载与 scene 同源,就是 C++。Go 的位置是 match、allocator、结算这类无状态编排。**gRPC ≠ Go**:C++ 说 gRPC 没有任何障碍,本仓 scene 已经这么做。

## 3. 核实过的事实(2026-09-05,逐条对过代码)

### 3.1 gRPC 在本仓 C++ 节点上的真实成本

这些是 gRPC 不该进热路径的依据,不是不用 gRPC 的依据:

> (2026-09-29 更正)本节行号按 2026-09-05 的代码。现行位置见 §8.6 行号漂移表。其中 `battle_client_player_service.cpp` 已随收缩删除(D66);gate 中继路径的成本描述的是收缩前旧模式的做法,现在两种模式都不再中继战斗。

- **线程**:EventEngine 池 `Clamp(cores, 4, 16)` 起手(20 核 = 17 条),仅是下限,backlog 时 Lifeguard 还会加;sync server 另有 poller 1..8 条(`node.cpp:625-645`,env `GRPC_SERVER_MAX_POLLERS`);再加 iomgr timer 线程。
- **阻塞桥**:每个 gRPC handler 在 poller 线程上 `runInLoop` + `promise/future` 等 muduo loop(`battle_client_player_service.cpp:71-79`、`battle_node.cpp:29-37`)。上限 8 个在途调用;scene 在 2 poller 时撞过 500ms 悬崖(`node.cpp:627-633` 注释)。
- **应答轮询**:客户端侧 CompletionQueue 由 5ms 定时器统一排空(`etcd_service.cpp:36-40` → `grpc_init_client.cpp:151-158`),每个应答等下一拍 0~5ms,且 gate 每秒扫 200 次所有已发现节点的队列。
- **端口派生**:gRPC 端口 = TCP + 30000(`node_allocator.cpp:302-324`),两个端口一个节点;2026-09-04 曾 fail-open(空 grpc_endpoint 发布到 etcd,match 报 battle 池为空)。
- **gate 中继路径的额外成本**:gate 单 IO 线程上每条消息 parse 进共享 prototype + 组 SessionDetails + Base64(`client_message_processor.cpp:634-707`);应答按 response 类型全名反查 message_id(`gate/main.cpp:241, 260-268`),两个方法共用同一 response 类型会串号。
- **版本锁步**:生成头硬校验 `PROTOBUF_VERSION != 7035001`,gRPC 升级必须整套重生成(与 battle 无关,任何链 gRPC 的节点都付,含 etcd 客户端)。

### 3.2 自家 RPC0 缺什么(全 muduo 方案的前置)

- `GameRpcMessage` 只有 type / request / response / error / message_id(`proto/common/base/rpc_message.proto`),**没有请求关联 id**。
- C++ 侧收应答按 message_id 派发给固定 handler(`game_channel.cpp:372` `HandleResponseMessage` → `gRpcResponseDispatcher`),不是按请求回调。
- 不改信封只能靠业务键关联:`CreateBattleResponse` 有 battle_id 能配;`AddObserverResponse` 没有 battle_id / observer_player_id,`DestroyBattle` / `RemoveObserver` 回 Empty,配不上。
- Go 侧现有的 RPC0 实现只有 robot 里 vendored 的 `luyuancpp/muduoclient`(Send / Recv,无超时、deadline、重连、派发),是压测机器人用的,不是服务端库。
- RPC0 连接要先过 NodeHandshake,注册侧校验对端 node_type 在 `IsTcpNodeType`(`registration_manager.cpp:118`);Go match 要以节点身份握手。

### 3.3 「不用管 battle↔gate 连接拓扑」这条 D6 论据不成立

gate 的出站白名单已含 BattleNodeService(`gate/main.cpp:198`),每个 gate 对全局池里**每个** battle 实例建 channel + stub + 独立 CompletionQueue(`node_connector.cpp:39-80`、`grpc_init_client.cpp:213-221`)。N×M 已经存在,只是跑在 HTTP/2 上。D6「battle 不做节点间 muduo RPC」的结论仍成立,理由改为:**没有任何 C++ 节点需要同步调 battle**(唯一 C++ 调用方 gate 在直连落地后也不调了),不是「省连接」。

> (2026-09-29 更正)上面描述的是收缩前的旧模式。收缩后(D66),gate 两种模式的白名单都不含 `BattleNodeService`(`gate/main.cpp:218-219`)。gate 仍发现并缓存 battle 的节点记录:`bin/etc/base_deploy_config.yaml:46` 保留了 `BattleNodeService.rpc` 发现前缀,`service_discovery_manager.cpp:86-113` 照常把 NodeInfo 写进 `ServiceNodeList` 并打「Node added」日志;但随后在 `:125-127` 按白名单返回,不建 channel / stub / CQ(D66)。所以这张 N×M 连接网已经没有了。「唯一 C++ 调用方 gate 也不调了」现在是事实:节点间控制面只剩 Go match 经 `grpc_endpoint` 调 battle(`cpp/libs/engine/core/node/system/node/node_util.h:132-145` 注释)。生成器为 `BattleClientPlayer` 产出的 gate 侧 gRPC 客户端桩仍编进 gate,但不会被调用,是有意保留的死代码(D66,KISS)。

### 3.4 关于 Go 重写的事实

- 引擎 `cpp/libs/services/battle/` 约 2.1k 行,刻意做成纯逻辑库:表访问只经 `BattleDataProvider` 接口,RNG 用裸 mt19937_64 高 53 位(为跨平台确定性)。
- Go 侧已有:六张战斗表生成代码并在 8 个服务加载、全部 tip 码、Kafka GateCommand 生产者、match-results 消费者、两份 gRPC 桩、etcd 注册形态。缺:MT19937-64、三列表达式求值器(生产表里只有 `k*level`、`k*level*health`、字面量)、指纹 sha256、HMAC 票据校验。
- **公式漂移不是「Go 才有」的问题**:`CalculateFinalDamage` 已经在 scene `skill.cpp:521-540` 与引擎 `turn_battle_engine.cpp:1019-1045` 各一份手抄,`turn_battle_constants.h:6-8` 写着「两侧改动必须手动同步」,`kMaxBattleTeamSize` 在 Go match 里第三份。配表指纹(D14)守的是**表版本**跨 zone 漂移,不守代码漂移。
- 所以 Go 重写在工程上是可做的(约 4k 行的有界移植),不做的理由只有一条:**将来的实时对局节点要复用 gate 的 muduo 客户端面栈(C++)**,两套 battle 两种语言不值。若回合制是独立长期产品且归 Go 团队,可以重开这个决定。

## 4. battle 是有状态游戏服务器,不是微服务

客户端 ↔ battle 这条边上的 battle 与 gate / scene 同类:一局只活在一个进程内存里,客户端必须连持有房间的那一台;票据签给具体实例(`BattleTicketPayload.battle_node_id` + `battle_instance_id`,验签侧必须等于自身);客户端拿到的是 ip:port 不是服务名。

| 组件 | 性质 | 暴露方式 |
|---|---|---|
| login / match / 结算 | 微服务 | k8s Service,副本随便挑 |
| gate | 有状态,会话钉定 | NodePort + 外部 L4 |
| scene | 有状态,房间钉定 | Agones GameServer,一 Pod 一节点 N 房 |
| **battle** | 有状态,房间钉定 | **同 scene**:Agones GameServer 或 hostPort 端口段 |

含义:不套 ClusterIP / Ingress / Mesh;`battle_id → 落点` 路由表进 Redis/etcd;换版只能排空不能滚动;扩缩容按房间数装箱不按 CPU。

> **暴露方式已定案并落码(2026-09-29,D76–D93;未编译、未测试、未上集群,待 Codex 验证)。** 上表 gate、battle 两行的原写法保留,现行口径如下:
>
> 下表「现行」一列是**集群外形态**,不是 K8s 默认形态:gate 行只在 `-ClientEntryMode external` 时成立,battle 行只在 `external` + `-BattleOrchestrator agones`(生产推荐)时成立。默认形态见表后第 1 条。
>
> | 组件 | 原写法 | 现行(集群外形态) |
> |---|---|---|
> | gate | NodePort + 外部 L4 | **(external 形态)StatefulSet + 每序号 Service `gate-<i>`**(D87):每个 Service 的类型是 NodePort(端口 = `gateNodePortBase + i`)或 LoadBalancer + 主机模板(D88);`podManagementPolicy: Parallel`、`updateStrategy: OnDelete`、PDB `maxUnavailable: 0`;不进 Agones;换版一律走 `tools/scripts/k8s_gate_drain.ps1` 逐个排空再删 Pod;`externalTrafficPolicy` 默认 `Local`,保留玩家真实源 IP(D89);单一 `gate-entry` Service 只在 podip 模式且 gate 单副本时生成(D90) |
> | battle | 同 scene:Agones GameServer 或 hostPort 端口段 | **(external + agones 形态,生产推荐)Agones Fleet,`portPolicy: Dynamic`**(D81):`spec.ports` 只有一个 `name: client`(containerPort 20000),gRPC 端口永不对外;排空靠标签 `mmorpg.io/drain`(D83);建房有许可与准入闸(D82);Deployment + hostPort 20000 只用于验证和回退(D86) |
> | scene | Agones GameServer | 不变;scene 不对客户端暴露(`portPolicy: None`,`tools/scripts/k8s_deploy.ps1:2211`) |
>
> - **K8s 默认形态不是上表**:默认 `podip` + `deployment`(`k8s_deploy.ps1:179`、`:210`)下,gate 仍是 Deployment(`k8s_deploy.ps1:4951` 的 `New-NodeDeploymentYaml`;StatefulSet 只在 external 时生成,`:4902`、`:4913-4914`),gate 单副本时另生成 `gate-entry`(D90);battle 是没有 Service 的普通 Deployment(`New-BattleWorkloadManifest` `:5256-5273`,Fleet 只在 agones 时生成)。四种组合见 [k8s-client-entry.md](./k8s-client-entry.md)「模式矩阵」。
> - **地址模型**:进程在发布到 etcd 之前自报 `NodeInfo.client_endpoint`(字段 11,D76)。login、scene_manager、battle 三个出口按同一规则选地址(D78);login 与 scene_manager 另按客户端地址 `net.JoinHostPort(host, port)` 去重,同地址只留 launch_time 最新的一条(D78)。battle 的 `BuildAssignment` 只对本节点取址,不涉及去重。`endpoint` 永远是集群内身份。
> - **两个开关**:`-ClientEntryMode podip|external` × `-BattleOrchestrator deployment|agones`(D80)。K8s 默认是 `podip` + `deployment`(`k8s_deploy.ps1:179,210`),这时 gate 和 battle 都通告 POD_IP,只有集群内 robot 连得上。生产推荐 `external` + `agones`。这两个参数不粘滞,切换形态要加 `-AllowDisruptiveSwitch`。
> - **清单生成位置**:`tools/scripts/lib/k8s_client_entry.ps1`(gate StatefulSet `:808`、PDB `:951-957`、battle Fleet `:1031-1064`);`k8s_deploy.ps1` 的 `New-BattleWorkloadManifest`(`:5254`)与 `Apply-BattlePool`(`:5276`)。
> - **路由表**:「`battle_id → 落点` 进 Redis」目前靠 match 的观战索引 `spectate:battle:{battle_id}` 兜着,补签也依赖它;独立路由键另起任务,本批不做。

## 5. 什么情况下改主意

- RPC0 补了 request_id 且有人维护 Go 客户端 → 方案 2 升为首选,C++ 节点网统一 RPC0,gRPC 只留在 Go 边界。
- battle 变成实时 MOBA → 客户端面换 KCP/UDP(见 moba-ds-server-interview-qa.md Q3),控制面仍 gRPC。**先实测 muduo TCP 边的单房 tick 成本再决定**(moba 目标文档 §四)。
- 回合制被确认为独立长期产品且归 Go 团队 → 重开 Go 重写,以 C++ 录制的事件流 golden 为跨语言基线。

## 6. 对既有决策的修订与过渡路径

- **D6 修订**:「上行走 gate→gRPC,下行走 Kafka」定为**过渡形态**,只为 Unity 第二连接未落地前保通。第二连接落地后,同批删除:gate 中继的 `BattleClientPlayer` gRPC 路径、Kafka `gate-{id}` S2C 回落、gate 里的 battle 专属代码(白名单项、绑定优先规则、`battle_binding_helper.cpp`、`ClearBattleRecord`、OnNodeRemove 分支)。最终 battle 只有两条客户端路径:直连 TCP 上行 + 直连 TCP 下行。
  - **(2026-09-29 标注)已执行**,见 turn-based §22 D65–D68、D72、D73(未编译、未测试,待 Codex 验证)。
    - 删码门槛「K8s 路由模式 battle-smoke 先跑通」由用户 2026-09-29 豁免,改为事后补验(D65)。
    - 已删:gate 中继的 `BattleClientPlayer` gRPC 路径;battle 侧的 `BattleClientPlayerGrpcImpl`;gate 的白名单项、`battle_binding_helper.{h,cpp}`、`ClearBattleRecord` 的两个调用点、`OnNodeRemove` 的 battle 分支(D66);Kafka Bind/Unbind 契约(D67);scene 重连时重发 Bind 的逻辑,现在只推重连提示(D72)。
    - 与原文的出入 ①:「Kafka `gate-{id}` S2C 回落」**只删了战斗帧这一类**。`NotifyBattleAssigned` / `NotifyBattleStart` 两条大厅公告在没有活直连时仍经 Kafka→gate 下发(D68 `PushLobbyAnnouncement`)。scene 的 `NotifyBattleReconnect` 和结算 `NotifyBattleEnd` 本来就是 scene 的大厅下行。所以「最终只有两条客户端路径」应读作「战斗上行与战斗帧只走直连;开局前的两条公告可以经大厅」。
    - 与原文的出入 ②:「绑定优先规则」只删了 battle 分支,Scene 的会话绑定是通用规则,保留。gate 的 `sResponseTypeToMsgId` 反查桥是所有 gRPC 回包的通用出口,也保留(D66)。
    - 与原文的出入 ③:生成器为 `BattleClientPlayer` 产出的 gate 侧 gRPC 客户端桩保留为死代码,不改生成器过滤(D66)。
    - 与原文的出入 ④:gate 已生成的 `BindBattleEventHandler` / `UnbindBattleEventHandler` 和各服务 `event_id.go` 里的相关常量,要等 proto 重生后才会消失。当前 `proto/event_id.txt:44-45` 仍是原名,重生后改写成 `N=reserved:<原名>` 墓碑(D67)。
- **D2 保留**:独立节点类型、全局池、gRPC 注册 `PROTOCOL_GRPC`。

## 7. 顺手记录的文档/代码漂移(与选型无关,但应清)

- `cpp/generated/proto_helpers/proto_util.cpp:33` 的 `IsTcpNodeType` **含** BattleNodeService,而 `battle_handler.h/.cpp`、`node_util.h:123-133`、turn-based 文档 §5.5 都说「不加」。后果:TCP 握手能过 `registration_manager.cpp:118`;gate 不误拨只是因为 etcd 里 protocol_type 是 GRPC。
- `ARCH.md` §6.1 / §6.3 把 gate↔scene 写成「直连 gRPC」,注册表里是 `PROTOCOL_TCP`(`rpc_event_registry.cpp:786-793`)。
- `battle_client_edge.h:3`、`battle_security.h:11` 引用 turn-based 文档「§18 D23-D28」,该文档止于 §17。
- turn-based 文档 §8 承诺 K8s Deployment,`deploy/k8s/manifests` 无 battle;`Dockerfile.cpp` 只出 gate + scene。
- `table_expression.h` 在 exprtk 里注册了基于 `rand()` 的 `random` 函数,策划在伤害表达式里一用就绕过引擎的种子 RNG,破坏确定性。

## 8. 问答:客户端有几条连接,battle 为什么不经 gate,scene 为什么经 gate(2026-09-29)

> 起因:用户 2026-09-29 提问「为什么我的 battle 节点不经过 gate,现在客户端有几条连接,为什么这么设计」。
> 依据:四路静态调研,再经 5 路对抗核验逐条订正(订正明细见 §8.7)。没有读客户端仓(AGENTS §9),Unity 侧只转述 turn-based §22 D74;客户端行为一律以 robot 为准。
> **口径(2026-09-29 改写)**:本节按收缩(turn-based §22 D65–D75)与集群外入口(D76–D93)落码后的代码写。行号以 2026-09-29 工作树为准(HEAD `9c9c012b7` + 未提交改动)。这些改动**未编译、未测试、未上集群,待 Codex 验证**。
> 本节只回答「是什么、为什么」。各环境实际走到哪一步,见 [turn-based-battle-server.md](./turn-based-battle-server.md) §21(收缩前快照)与 §22(收缩落码)。

### 8.1 客户端有几条连接

**平时 1 条长连接(gate);参战或观战期间多 1 条 battle 直连(每局一条)。直连是战斗的唯一通路。** 收缩后(D66/D73),gate 两种模式都一样:gate 不中继战斗,直连建不起来就不能出手。这时 Unity 显示「无法连接战斗服务器」并提供重连入口(D74)。登录前后另有打给 Java 网关的 HTTP 短请求,不算长连接。

| 阶段 | 对端 | 传输 | 生命周期 | 承载 | 鉴权 |
|---|---|---|---|---|---|
| 登录前后 | Java 网关(本地 `:8081`;K8s 经 Ingress,只路由 `/api`,D91) | HTTP 短请求 | 按请求 | `GET /api/server-list`(robot 只在 `zone_id=0` 时调;Unity 选区行为未核);可选 `POST /api/login`(robot 开关 `use_http_login`,默认关;关着时登录走 gate TCP 上的 `ClientPlayerLogin.Login`);`POST /api/assign-gate`(排队时轮询 `POST /api/queue-status`);会话期间 `POST /api/refresh-token`。另有 `/api/announcement`、`/api/hotfix-check`、`/api/cdn-sign`,客户端是否调用未核 | **都不校验 access token**:Java 网关的 `/api/*` 没有鉴权 filter,`AdminApiKeyFilter` 只管 `/admin/`(`AdminApiKeyFilter.java:29`)。server-list 无鉴权、也无限流(`ServerListController.java` 只有一个 `@GetMapping`,`:19`,不注入限流器);assign-gate 只有限流(`AssignGateRateLimiter`,`AssignGateController.java:38`、`:59`);queue-status 凭 `queue_token`,且有意绕过该限流器;refresh-token 凭 refresh token(一次性轮换);`/api/login` 凭密码或第三方 token,另经同一个 `AssignGateRateLimiter` 限流(`LoginController.java:37`、`:54`)。access token 不在 HTTP 上用,而是在 gate TCP 上 `Login(auth_type=access_token)` 时出示 |
| ① 大厅 | gate(本地 zone1 `:10000`;K8s external 形态下是每序号 Service `gate-<i>` 的地址,D87/D88) | muduo TCP,ProtobufCodec | 登录到登出 | Login / CreatePlayer / EnterGame;scene 的全部客户端消息;chat / guild / friend / team / trade / match 等 Go 服务经 `client_rpc_router`(包括补签 `MatchService.RequestBattleTicket` 和 `WatchBattle`)。战斗相关只剩 4 条下行:battle 的 `NotifyBattleAssigned` / `NotifyBattleStart`(没有活直连时经 Kafka→gate 回落,D68);scene 的 `NotifyBattleReconnect`(D72)和结算后的 `NotifyBattleEnd`。战斗上行经 gate 一律回 `kServiceUnavailable`(`client_message_processor.cpp:936-950`) | 首包 `ClientTokenVerifyRequest`,携带 gate token。首次登录的 token 由 login `LoginPreGate.AssignGate` 签发,有效期 10 分钟(`assigngatelogic.go:27`);跨 zone 重定向的 token 由 scene_manager `AssignGateForZone` 签发,有效期 5 分钟(`gate_redirect.go:27`)。有效期只约束握手那一刻 |
| ② 战斗 | battle 的客户端地址,即 `BattleAssignedS2C.host/port`。取址规则见 `client_endpoint::ClientFacing`(`battle_room_manager.cpp:1452-1467`):有 `NodeInfo.client_endpoint` 就用它,podip 形态回落 `endpoint`。各环境取值:本地 `start_game.ps1` 固定 `RPC_PORT=20010`;`dev_tools.ps1 cpp-node-start` 不设时在 20000–35535 自动分配。K8s 按形态分三种:podip(含 podip + agones)为 `POD_IP:20000`,podip + agones 时 Fleet 照常注入 Dynamic 端口,但 env 是 `CLIENT_ENDPOINT_SOURCE=none`,battle 不自报(`tools/scripts/lib/k8s_client_entry.ps1:1019`);external + agones 为 Fleet 的 Dynamic 端口(D81);external + deployment 为 hostPort 20000,host 取 `-ClientPublicHost`,留空取 `HOST_IP`(D86,`k8s_client_entry.ps1:1104`) | muduo TCP,**与 gate 同一套 codec** | 每局一条。关闭顺序:正常终局先推 `BattleEnd` 再关;观众被清退先推 `SpectateEnd` 再关;观众主动 `StopWatchBattle` 时回包后关;作废(`DestroyBattle`,或停机时的 `AbortAllRooms`)时参战者**没有**终局包,直接关,只有观众收到 `SpectateEnd(ABORTED)`。关法是本轮 loop 之后 `shutdown`,1s 后强关兜底。另有两条强关路径:握手 10s 超时、输出缓冲超过 2MB 高水位 | 上行只放 4 个消息号:`SubmitBattleAction` / `GetBattleState` / `StopWatchBattle` / `SetAutoBattle`(`battle_client_edge.cpp:459-509`)。下行包括握手应答、这 4 条请求的应答、回合结果、终局、观战帧。观众握手成功后,battle 立刻推一份观战快照(D69) | 首包 `BattleTokenVerifyRequest`,携带 battle 自签的票据。寿命等于房间作废期限,重连可以重复使用(D25) |

- **第 ② 条连接怎么来的**:
  - battle 在 CreateBattle / AddObserver 时**先预签**票据,签不出就拒绝开局或拒绝观战,回 `kServiceUnavailable`,不产生任何副作用(D70,`battle_room_manager.cpp:568-584`)。
  - 票据组成 `BattleAssignedS2C{battle_id, host, port, token_payload, token_signature, expire_at_ms, role}`,签发函数是 `BuildAssignment`(`:1432-1498`)。
  - 下发经 `PushLobbyAnnouncement`:有活直连就直发,否则经 Kafka → gate → 大厅连接(D68,`:1410-1430`)。调用点:CreateBattle 在 `:576-621`,AddObserver 在 `:827-890`。
  - 客户端据此建第二条连接。robot 用 `robot/battle_direct_conn.go` 的 `dialBattleDirect`,握手超时 10s;Unity 用 `IBattleChannel` / `BattleDirectLink`(D74)。
  - 票据丢了或直连断了,客户端经大厅调 `MatchService.RequestBattleTicket`。match 凭观战索引 `spectate:battle:{battle_id}` 找到落点,再调 `BattleNode.IssueBattleTicket` 由 battle 补签。Unity 每局最多自动补签 3 次,间隔 1s/2s/4s,±20% 抖动(D74)。
  - 首帧怎么拿:参战者在直连就绪后自己 `GetBattleState` 补拉;观众由握手时的快照拿到首帧(D69,`OnDirectConnectionVerified`,`:1576-1592`)。
- **两条连接共用一张 handler 表**,客户端不关心 S2C 来自哪条连接(turn-based §18.2 契约)。`NotifyBattleEnd` 可能从两条连接各到一次:一次是 battle 直连上的终局帧,一次是 scene 结算后经大厅推送。客户端按 battle_id 幂等处理(D68)。
- **同一时刻最多一条有效的 battle 直连**:
  - 同一房间内:重连即替换旧连接(`BattleRoomManager::AttachDirectConnection`)。
  - 跨房间,客户端侧:单一的 `BattleDirectLink` 收到新局的 Assigned 就关掉旧连接。
  - 跨房间,服务端侧:靠 match 的**尽力**互斥,不是强保证。
    - `WatchBattle` 见到匹配票或 `battle:lock` 就拒绝。`battle:lock` 只是咨询性的,权威在 scene 的 `InBattleComp`。
    - 已在观战时不拒绝,先尽力 `RemoveObserver` 旧场,再用 `spectate:watching:{player_id}` 的 SETNX 抢占新场。SETNX 只裁决并发的 WatchBattle。
    - 进匹配凑单时也会清退观战(`go/match/internal/logic/gather.go:233`)。
    - `RemoveObserver` 失败只记日志,旧场的观众身份和它的直连会一直留到该场结束。
  - 重连替换或换场观战的瞬间,旧连接可能还没关完,会短暂重叠。
- **跨 zone 传送是替换 gate 连接**:收到 `RedirectToGateNotify`(msg 124)后,先探测新 gate 可达并发起新连接,再关旧 gate(`robot/pkg/redirect.go` → `GameClient.SwapConn`,`robot/pkg/client.go:118`)。muduo 拨号是异步的,关旧连接时新 TCP 不保证已经建好。连接数不增加。服务端与 robot 不会因换 gate 断开 battle 直连;但 Unity 大厅断线仍会拆直连(D74,本批不改,见 turn-based §22.2 D74、§22.5 风险第 10 条),跨 zone 换 gate 时 Unity 是否按大厅断线处理,本仓没核。
- **K8s 上连不连得上,取决于入口形态**(D80):
  - 默认 `-ClientEntryMode podip`:gate 和 battle 都通告 POD_IP,只有集群内的 robot 连得上。
  - 集群外客户端必须用 `-ClientEntryMode external`,见 §4 的现行口径。
  - 早先推断「K8s 上直连失败会回落 gate 中继、只剩 1 条连接」不成立:gate 同样通告 POD_IP,集群外客户端 gate 和 battle 都连不上,集群内 robot 两者都连得上。而且回落路径现在已经删了。
- **dev/test 空密钥降级**:gate 的 `GateTokenSecret` 为空且 `GATE_RUN_MODE=dev` 时,跳过握手签名校验;battle 的密钥为空时,只跳过签名比对。本地 `bin/etc/base_deploy_config.yaml` 两把密钥都已配置(`:110`、`:132`),所以本地握手照常校验。
- 没有单独的聊天、世界频道或语音连接。

### 8.2 battle 为什么不经 gate(按分量排序)

1. **gate 是全体玩家共用的漏斗。** gate 的客户端面只有一个 IO 线程:全仓没有 `setThreadNum`,muduo 默认 0 个 IO 线程(`client_message_processor.cpp:542-544`)。回合制当下每人每回合只有一条上行,量不大;真正吃紧的是将来的实时对局,每人每 tick 一个输入、每 tick 一份快照。如果中继(收缩前旧模式就是这么做的,现已删除):
   - gate 每条消息都要 parse 进共享 prototype、组 `SessionDetails`,再走 gRPC。这条通用路径现在是 `HandleGrpcNodeMessage`(`client_message_processor.cpp:723-795`),已经不再接收战斗消息;
   - battle 收到后再解一次;
   - 应答回来,gate 还要按 response 类型全名反查 message_id(`gate/main.cpp:270-279` 建表,`:306-312` 查表)。反查表以类型全名为键,后写覆盖先写,所以两个方法共用同一个 response 类型会串号。

   隔壁 proj_base 项目实测过:房间进程随便加,网关这个共享序列化漏斗在 69 房时卡死。这是转述,见 [moba-battle-target-architecture.md](../notes/slg-moba/moba-battle-target-architecture.md) §五;本仓未复现,实测条件也没有记录。

   **现状**:收缩后 gate 两种模式都在 `DispatchClientRpcMessage` 里、按协议分派之前拒绝战斗消息(`client_message_processor.cpp:936-950`,D66),上面这些成本不再发生。路由服同样拒绝目标为 battle 的消息(`go/client_rpc_router/internal/logic/forwardlogic.go:133-136`,D33)。
2. **battle 是全局池,经 gate 中继必然 N×M。**
   - scene 按 zone 划分,gate 只连本 zone 的 scene,连接数有上限。
   - battle 不分 zone(`node_util.cpp` `IsGlobalPoolNodeType`)。中继就要求每个 gate 对每个 battle 建 channel、CompletionQueue 和 stub:`node_connector.cpp:39-82` 的 `ConnectToGrpcNode` 建 channel,再由 `InitGrpcNode` 挂上 CQ 与两个 stub(生成物 `grpc_init_client.cpp:438-443`)。应答还要等 5ms 一次的 CQ 排空定时器(`etcd_service.cpp:33`)。
   - **现状**:收缩前的旧模式(C++ 默认,以及当时的 K8s 默认)确实有这张网。收缩后 gate 两种模式的白名单都不含 `BattleNodeService`(`gate/main.cpp:218-219`),N×M 已经没有了。
3. **一局只活在一个进程的内存里。** 客户端必须连到持有房间的那一台。票据签给具体实例(`battle_node_id` + 实例 UUID `battle_instance_id`),battle 在本地验签,不回头问大厅(§4)。
4. **battle 要能随时被杀。** gate 与 battle 之间没有连接,所以 battle 崩溃、扩缩容、换版本都不会牵动 gate,gate 也不保存战斗绑定状态。
   - 收缩后两种模式都已如此:`battle_binding_helper.{h,cpp}`、`boundBattleIdBySession`、`OnNodeRemove` 的 battle 分支都已删除(D66)。
   - 「能随时被杀」是会话制对局的判据(moba 目标文档开头的一句话判据)。
   - 光「不连 gate」不够。battle 还要满足:结果先落库再发奖、超时作废有补偿(session-extractability §1.1)。
   - K8s agones 形态下,「杀」按排空走:打上 `mmorpg.io/drain` 标签后拒绝新房间,现有房间打完再回收(D83)。
5. **改造成本低。** gate 客户端面的整套能力逐条镜像进了 battle(turn-based §18.2 / D24 / D27),客户端的第二条连接因此复用同一套 codec 和 handler 表,不需要第二套协议栈(§18.2)。逐项如下:
   - ProtobufCodec、HMAC 验签;
   - 并发上限(`battle_max_connections`,`battle_client_edge.cpp:159`);
   - 握手期限 10s、体积上限 1KB(`battle_client_edge.h:51-53`);
   - 限速,与 gate 同一张 MessageLimiter 表(`battle_client_edge.h:87-88`);
   - 消息号白名单,是 battle 自己的四条,不是 gate 的 `IsClientMessageId` 表(`battle_client_edge.cpp:459-509`);
   - 非法包阈值,与 gate 同源;
   - 输出高水位 2MB(`battle_client_edge.cpp:23`、`:180`)。
6. **按平面拆传输。** match → battle 的控制面每局只有一两次调用,留在 gRPC;节点级准入拒绝用 gRPC `UNAVAILABLE "battle_not_allocatable"` 表达(D82)。客户端面是热路径,不进 gRPC(§2、§3.1)。结算走 Kafka。
7. **票据由 battle 自签,不由 match 签**(turn-based D24):
   - 只有 battle 知道房间名单;
   - 分配包和开战包由同一个生产者按同一个 key 发出,才能保证先后顺序;
   - 少一个持有密钥的服务。

### 8.3 scene 为什么仍经 gate

- **scene 的状态不能丢。**
  - 主世界 scene 常驻、按 zone 划分,是玩家权威数据的唯一写入方。
  - 硬 kill 会回档到上次周期存盘(默认 300s)。它的可达目标是「随时可杀但不回档」,不是 battle 式的「可丢」。
  - 按 battle 的方式把它抽出来是范畴错误([session-extractability-mmo-slg.md](./session-extractability-mmo-slg.md) §2.1)。
  - 副本 scene 在 0 人 300s 后回收,并不常驻。
- **同 zone 内换场景对客户端无感。**
  - 玩家频繁换场景。gate 收到 Kafka `RoutePlayerEvent` 后改写会话的 scene 绑定,转发成功才提交(`gate_event_handler.cpp:56-106`),客户端连接不变。
  - 如果直连 scene,每次换到另一个 scene 节点都要重建连接;同一节点内换场景则不用。
  - 跨 zone 仍要换 gate,见下条。
- **连接数有上限。** gate 只连本 zone 的 scene;跨 zone 由客户端换 gate(RedirectToGate)。
- **安全层只做一份。** gate 是 scene 面的安全层(逐个消息号白名单、限速、非法包计数)。直连 scene,就要把这层复制到每个 scene。

### 8.4 这个例外在什么条件下成立

业界标准是「客户端只连一个接入层」(见 [client-access-band-routing.md](./client-access-band-routing.md) 第一部分)。battle 直连属于这个标准允许的唯一一类例外:**权威对局服,凭票据入场**。xuanming 的第二条连接同样连的是 DS。

措辞说明:client-access-band-routing.md §4 的原文是「权威实时服,凭短期票据」。本仓 battle 是回合制,票据寿命等于房间作废期限,可重复用于重连,这是 D25 有意偏离「短期一次性」。

新服务想让客户端直连,必须同时满足以下 6 条。这是本节的归纳,没有文档把它列成清单,各条出处见括注:

1. 会话形状:有始有终,入口一份快照,出口一份结果,中间状态外界不需要(session-extractability §1.3);
2. 进程可以随时被杀:不写玩家库,结果在进程外先落地并幂等应用,超时有作废补偿。这四条契约缺一不可,零写权只是必要条件(session-extractability §1.1);
3. 票据本地验签,不回头问大厅(moba 目标文档 §二①);
4. 直连面按 gate 的标准同等设防(turn-based D27);
5. 每个落点都有客户端能访问的入口(moba 目标文档 §五;K8s 上按 D76–D93 落地,见 §4);
6. 流量高到值得绕开共享漏斗(moba 目标文档 §五)。

chat / guild / friend / trade 等业务服不满足这 6 条,目标形态一律经 gate + `client_rpc_router`。各入口的现状:
- 本地一键启动(`start_game.ps1:22`,默认 `GateRouterMode=1`)和 K8s(`k8s_deploy.ps1:164`,默认 `"1"`,D75)都已是路由模式,这些服务可达。
- C++ 进程未设 `GATE_CLIENT_RPC_ROUTER` 时仍是旧模式,C++ 默认值按 D-12 不改。`dev_tools.ps1 dev-start` 继承父 shell,未设即旧模式。旧模式的白名单(`gate/main.cpp:219`)里没有这些服务,玩家到不了。

2026-09-28 的评估结论是不照搬 xuanming 的「客户端直连各服务」。这不是带 D 编号的决策,出处是 client-access-band-routing.md §0 第 1 条,§5 把方案 A、C 都判为「不推荐」。

### 8.5 代价

- 客户端要分别管两条连接的生命周期:断线重连、票据过期补签,两条连接互不干扰。Unity 侧的补签预算和 UI 横幅见 D74。
- 每个 battle 落点都需要客户端能访问的入口。K8s 上已按 D76–D93 定案并落码(见 §4),但还没上集群验证。默认 podip 形态下,集群外客户端仍然连不上。防火墙要额外放行 Agones 端口段(kind 验证用 7100–7109)和 gate 的 nodePort 段。
- 多了一个对公网开放的端口。票据的风险:
  - 走明文 TCP,只有 adler32 校验;
  - 泄露面有两条链路:一条是 battle 直连握手;另一条是 `BattleAssignedS2C` 经 Kafka → gate → 大厅连接下发,大厅连接同样是明文 TCP + adler32(组包在 `battle_room_manager.cpp:1432-1498`);
  - 不绑定客户端 IP,整局有效;
  - 一旦泄露,别人就能冒名接入,并借重连替换把正主挤掉;
  - 参战者的票据要中途吊销,只能销毁房间。观众的票据随 `RemoveObserver` / `StopWatchBattle` 出名单即失效(`HandleRemoveObserver` `:897`、`HandleStopWatchBattle` `:931`)。按玩家吊销的 `RevokeTicket` 在 turn-based §18.7 列为「明确不做 / 后续」,需要时走 match→battle gRPC 补上。
- 收缩已落码(D39 由 D68 落地):连不上 battle 端口的客户端就不能战斗,不再有经 gate 的回落。D70 同时把「签不出票」改成拒绝开局,不再让玩家只带 1 条连接进局挂机。

### 8.6 §7 漂移复核(2026-09-29)

| §7 条目 | 现状(2026-09-29 核验后更新) |
|---|---|
| `IsTcpNodeType` 含 BattleNodeService | **仍在**(`cpp/generated/proto_helpers/proto_util.cpp:33`)。实际上 `eNodeType` 的 28 个值全在表里,ClientRpcRouter 等 Go gRPC 服务也在。<br>**根因不在模板**:`node_util.cpp.tmpl` 只遍历 NodeList。过滤写在生成器 Go 代码里:`node_util.go:46` → `IsTcpNodeByEnum` → `GetProtocolByEnum`(`service_register_info.go:223-236`)。它只认 `proto_gen.yaml` 的 `proto_directories` 里同时含节点名和 `grpc` 的目录,而该列表(`:169-186`)没有任何一项含 `grpc`,所以过滤恒为真。<br>修法:改这段判定逻辑,或给它真实的协议来源,再重生。不能手改生成物。<br>握手校验点现为 `registration_manager.cpp:134`。<br>收缩后 gate 仍发现并缓存 battle 节点记录(发现前缀保留),但白名单不含 battle,不建 channel / stub / CQ(`service_discovery_manager.cpp:125-127`,D66),影响面更小。battle 以 `PROTOCOL_GRPC` 注册,由 `IsGrpcOnlyNodeType`(`node_util.h:132-145`)保证 |
| ARCH §6.1 / §6.3 把 gate↔scene 写成 gRPC | **ARCH 已处理**(ARCH 2026-09-29 更正,工作树未提交,提交前按最终 ARCH 再核行号):§1 拓扑已改标 muduo TCP RPC,更正注在 `ARCH.md:81`;§6.1 在 `:249`;§6.3 原句 `:278` 保留,更正在 `:280`;§6.4 原句 `:286` 保留,更正在 `:287`。早先引的 `:64` / `:225` / `:253` / `:259` 是 HEAD 版行号,已失效(现 `:225` 是「5.3 端口 / 性能调优」)。<br>**仍在**:`gate-scene-relay-architecture.md` 全文仍按 gRPC 叙述(如 `:5`、`:16`、`:50`),不在本包范围。<br>实际情况:scene 客户端方法是 `protocol=0`,即 `PROTOCOL_TCP`。依据是 `rpc_event_registry.cpp` 的 `// --- SceneClientPlayerCommon ---` 段(工作树 `:913`)。该文件有他人未提交改动,行号不稳定,所以改引段标记。gate 据此走 `HandleTcpNodeMessage`(`client_message_processor.cpp:670`),经 muduo `CallRemoteMethod` 转给 scene |
| 引用「§18 D23-D28」而文档止于 §17 | **已解决**:turn-based 文档已写到 §22。另:§7 原说 `battle_security.h:11` 引的是「§18 D23-D28」,不准,它实际引的是「§18 D24/D25」;引 D23-D28 的只有 `battle_client_edge.h:3` |
| 没有 battle 的 K8s Deployment / 镜像 | **基本解决**,剩一项:<br>- 镜像:`Dockerfile.cpp` 已构建 battle。<br>- 部署:`k8s_deploy.ps1` 有 `Apply-BattlePool`(`:5276`)。Deployment 由脚本内联生成,`deploy/k8s/manifests` 里没有静态文件,这是设计如此。<br>- 密钥:`MMORPG_BATTLE_TOKEN_SECRET` 在 `New-NodeConfigMapYaml -IncludeBattleSettings` 里注入,不是 §18.6 设想的 `Initialize-InjectedSecrets`。<br>- 集群外入口:已按 D76–D93 落码(Fleet / hostPort 两种形态,见 §4)。<br>- **仍缺**:`release_preflight.ps1` 的 battle 目标 |
| exprtk 的 `random` 基于 `rand()` | **仍在**(`cpp/libs/engine/config/table_expression.h:10`) |

新增漂移(09-29 早先列出,收缩落码后逐项更新状态):

- **§2.1「battle → gate 没有这条边」** —— 已处理,§2.1 已加现状标注。初稿列的三项逐条状态如下:
  - battle 经 Kafka 给 gate 发 `BindBattleEvent`:**已删**,`SendBindBattle` / `SendUnbindBattle` 已移除(D67)。
  - 没有直连时所有 S2C 回落到 Kafka → gate:**收窄,不是删除**。只剩 `NotifyBattleAssigned` / `NotifyBattleStart` 两条大厅公告在无活直连时回落,战斗帧、观战帧、终局无直连即丢弃(D68)。
  - 旧模式下 gate 持有 battle 的 stub:**gate 不再建 battle stub 实例**(白名单不含 battle,见 §3.3 更正);生成器为 `BattleClientPlayer` 产出的 gate 侧桩代码仍编进 gate,是有意保留的死代码(D66)。

  核验补充项(不在初稿三项内):scene 在重连链路上重发 `BindBattleEvent` 的逻辑**已删**。原函数 `RebindBattleOnReconnect` 已改为 `NotifyBattleReconnectToClient`(`player_battle.cpp:1912-1933`),只推重连提示(D72)。

  生成物里的 Bind/Unbind(gate 事件 handler、`gate_kafka_command_router.cpp`、各服务 `event_id.go`)要等重生后才消失。
- **行号漂移**:下表 §3.1 / §3.2 / §3.3 / §7 列的是 2026-09-05 的行号,现行位置如下(2026-09-29 工作树):

  | 原引用 | 现行 |
  |---|---|
  | `client_message_processor.cpp:634-707`(gate 中继 parse + SessionDetails) | `HandleGrpcNodeMessage` `:723-795`,不再接收战斗消息;战斗拒绝在 `:936-950` |
  | `gate/main.cpp:198`(白名单含 battle) | 白名单 `:218-219`,两种模式都不含 battle;说明注释 `:199-203` |
  | `gate/main.cpp:241, 260-268`(按 response 全名反查 message_id) | 建表 `:270-279`,查表 `:306-312`,整个桥接回调 `:290-315` |
  | `grpc_init_client.cpp:213-221`(battle 的 CQ + stub) | `:438-443` |
  | `grpc_init_client.cpp:151-158`(CQ 排空) | `HandleCompletedQueueMessage` `:358-434` |
  | `etcd_service.cpp:36-40`(5ms `RunEvery`) | `:33` |
  | `node.cpp:625-645` / `:627-633`(poller 与 500ms 悬崖注释) | `:730-750` / `:731-737` |
  | `node_connector.cpp:39-80` | `ConnectToGrpcNode` `:39-82` |
  | `battle_client_player_service.cpp:71-79`(阻塞桥) | 文件已随收缩删除(D66);阻塞桥仍见 `battle_node.cpp`,CreateBattle 先过准入与许可(D82)再 `runInLoop`,`:60-117` |
  | `node_allocator.cpp:302-324`(gRPC 端口 = TCP + 30000) | `:302-326` |
  | `game_channel.cpp:372`(`HandleResponseMessage`) | `:386` |
  | `registration_manager.cpp:118` | `:134` |
  | `node_util.h:123-133` | `IsGrpcOnlyNodeType` 的注释与声明在 `:132-145` |
  | `rpc_event_registry.cpp:786-793` | 现在指向 Guild 条目;scene 段改引段标记 `// --- SceneClientPlayerCommon ---` |
- **`proto/battle/player_battle.proto` 头注释** —— 已解决。头注释已按收缩口径改写(`:9-21`):上行只走直连,战斗帧只走直连,只有两条大厅公告可以经 Kafka 回落。`:118` 起的「客户端直连 battle 节点」段与头注释不再矛盾。
- **ARCH §1 总体拓扑图里没有 battle 节点,也没有客户端的第二条连接** —— 已处理(ARCH 2026-09-29 更正,工作树未提交,提交前按最终 ARCH 再核行号):§1 拓扑已补 battle 节点与第二条连接,更正注在 `ARCH.md:81`,连接说明在 `:84-86`;「默认仍是旧的逐服务直连」原句保留在 `:115`,`:116-120` 更正为默认值分两层;§6.1 新增「对局大厅公告」行 `:254`,写明 battle → gate 只剩 Kafka 大厅公告。早先引的 `:64` / `:96` / `:229` 是 HEAD 版行号,已失效。
- **client-access-band-routing.md 链接** —— 已解决:早先它是未跟踪文件,现已纳入 git,链接不再悬空。
- **k8s-client-entry.md 链接** —— 仍有风险:该文件在 git 里仍是未跟踪文件(`git status` 显示 `?? docs/design/k8s-client-entry.md`)。本文文件头、§4、§8 都链接它,ARCH.md 也链接它(`:86`、`:428`)。**它必须和本文、ARCH.md、turn-based-battle-server.md 在同一次提交里纳入 git**,否则这些链接会悬空。

### 8.7 订正记录(2026-09-29)

§8 初稿写于 2026-09-29 上午,依据是四路静态调研。随后 5 路对抗核验逐条对照了代码。下面列出初稿中被推翻(refuted)或只部分成立(partial)的表述,以及订正结果。上面的正文已经改好,这里保留原说法供追溯。

**被推翻**

1. 表 8.1「登录前后」的鉴权一栏原写「无 / access token」。**这是错的。** Java 网关的 `/api/*` 没有任何鉴权 filter 或 interceptor,这几条 HTTP 接口都不校验 access token。access token 是在 gate TCP 上 `Login(auth_type=access_token)` 时出示的。

**部分成立**

2. 原写「平时 1 条,参战 2 条」。初稿没有区分 gate 两种模式:收缩前的旧模式下,直连失败会回落 gate 中继,仍然只有 1 条。收缩后两种模式都没有回落,正文已按「直连是战斗唯一通路」改写。
3. HTTP 接口清单漏了可选的 `POST /api/login`。「只在未指定 zone 时调 server-list」只是 robot 的行为。`queue-status` 是 POST。
4. 大厅连接的承载项漏了观战首帧、补签和 WatchBattle,也漏了旧模式下 gate 的上行中继。收缩后首帧改为随直连握手下发(D69),中继已删除,正文按现状改写。
5. gate token 原写「login 签发,10 分钟」。跨 zone 重定向的 token 由 scene_manager 签发,有效期 5 分钟;有效期只约束握手那一刻。
6. `RPC_PORT=20010` 只有 `start_game.ps1` 会设。
7. 原写「终局、作废、清退时都推完终局包再关」。实际上作废路径下参战者没有终局包,观众主动 StopWatch 也不推 SpectateEnd;另有握手超时和高水位两条强关路径。
8. 下行项漏了握手应答和请求应答。
9. `BattleAssignedS2C` 字段表漏了 `battle_id`,也漏了 AddObserver 的调用点。
10. 原写跨房间「由 match 的互斥保证」。这只是尽力互斥,RemoveObserver 失败时旧场观众和它的直连会一直留着。
11. 跨 zone 传送:关旧连接时,新 TCP 不保证已经建好。
12. §8.2 第 1 条原写「战斗是最高频的流量」。这是实时对局的前提,不是回合制的现状。「69 房」出自 moba §五 而不是 §四,而且是转述,本仓没有复现。另外漏了反查表串号的风险。
13. §8.2 第 2 条缺出处(CQ 和 stub 挂在 `InitGrpcNode` 上,5ms 定时器在 `etcd_service.cpp:33`)。初稿写了「旧模式下这张 N×M 网至今还在」,但没说明旧模式只是 C++ 与当时 K8s 的默认,本地一键启动默认是路由模式。
14. §8.2 第 4 条写的是目标形态,当时的旧模式下 gate 仍持有 battle 绑定。收缩后已成立。
15. §8.2 第 5 条原写「原样搬进(D27)」。出处其实分散在 §18.2 / D24 / D27;白名单是 battle 自己的四条,称不上「原样」;还漏了并发上限。
16. §8.3 原写「scene 不能随时被杀」。所引文档的口径其实是「状态不能丢」,可达目标是「随时可杀但不回档」;「常驻」只适用于主世界。「换场景无感」只在同 zone 内成立。
17. §8.4 原写「权威实时服,凭短期票据」。本仓 battle 是回合制,票据也不短期、可重用(D25)。6 条准入是本节自己的归纳,第 2 条表述不足。「业务服一律经路由服」原本是目标形态,当时 K8s 默认还是旧模式,现已由 D75 翻转。「否决直连各业务服」不是带编号的决策。
18. §8.5 原写「吊销只能销毁房间(已列为不做)」。观众票据出名单即失效;§18.7 的原文是「明确不做 / 后续」,留有余地。另外票据泄露面漏了大厅那条链路。
19. §8.6 原说 `IsTcpNodeType` 的根因在生成模板。实际在生成器的 Go 判定逻辑里。ARCH 那一行的证据也不全;K8s 部署一行没点出 `release_preflight` 的剩余项;行号漂移只列了 3 处;`player_battle.proto` 头注释的引号内文字是把两段拼起来的。
20. 另外,turn-based §21.1 曾推断「K8s 上直连失败回落 gate 中继、只剩 1 条连接」,这不成立:K8s 上 gate 同样通告 POD_IP,集群外客户端 gate 和 battle 都连不上。现状已被 D76–D93 取代。

**改写稿的二次核验(2026-09-29 更正)**

改写稿写完后又经一轮核查。以下表述已在原位改正,这里记下改前的说法:

21. §8.1 鉴权一栏原写「server-list / assign-gate 只有限流」。实际上 server-list 无鉴权也无限流;`AssignGateRateLimiter` 用在 assign-gate 和 `/api/login` 上。verify.txt 的核验原文(T)本来是分开写的,是替换文本(R)合并错了。
22. §8.1 ② 行原写「K8s agones 形态是 Fleet 的 Dynamic 端口」。podip + agones 下 battle 不自报,票据仍是 `POD_IP:20000`;原文还漏了 external + deployment 的 hostPort 20000 形态(D86)。
23. §8.1「跨 zone 传送」原写「换 gate 不会断开 battle 直连」,没加限定,与 D74「Unity 大厅断线仍拆直连」冲突。
24. §3.3 更正与 §8.6 首行原写「gate 不再发现 battle」。实际上 gate 仍发现并缓存 battle 节点记录,只是按白名单不建连接(D66)。
25. §8.6 的 ARCH 两处原判「仍在」,引的是 HEAD 版 ARCH 行号。ARCH 已在工作树里按 2026-09-29 口径改好,现改判「已处理」;只有 `gate-scene-relay-architecture.md` 仍在。
26. §8.6「新增漂移」首条原写「早先列的三项都已删除」,但列出的并不是初稿那三项:「所有 S2C 回落」其实是收窄成两条大厅公告,「gate 持 battle stub」只删了实例,桩代码仍作为死代码保留。现已按初稿三项逐条写状态。
27. §2.1 标注原写「事件号 43/44 用墓碑保留」,读起来像已生效。墓碑要等 proto 重生后才写入。
28. §4 原写「三个出口按同一规则选地址并去重」。去重只作用于 login 与 scene_manager 下发的 gate 候选。§4「现行」表也没写明它只是集群外形态,K8s 默认形态见表后新增的第 1 条。
29. 本条目 13 原说初稿「没说明 N×M 只在旧模式下存在」,与初稿原文不符:初稿写了旧模式,漏的是各环境默认值不同。
