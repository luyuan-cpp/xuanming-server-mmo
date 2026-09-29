# battle 节点传输选型定谳(gRPC vs muduo TCP RPC vs Go)

**Created:** 2026-09-05
**状态:** 架构定谳(结论已拍板;实现进度以 [turn-based-battle-server.md](./turn-based-battle-server.md) 为准,本文不记录完成度)
**关联:** [turn-based-battle-server.md](./turn-based-battle-server.md)(D2/D6 原始决策)、[moba-battle-target-architecture.md](./moba-battle-target-architecture.md)(会话制目标形态)、[scene-grpc-server-design.md](./scene-grpc-server-design.md)(gRPC vs Kafka 取舍表)、[ARCH.md](./ARCH.md) §6

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

## 5. 什么情况下改主意

- RPC0 补了 request_id 且有人维护 Go 客户端 → 方案 2 升为首选,C++ 节点网统一 RPC0,gRPC 只留在 Go 边界。
- battle 变成实时 MOBA → 客户端面换 KCP/UDP(见 moba-ds-server-interview-qa.md Q3),控制面仍 gRPC。**先实测 muduo TCP 边的单房 tick 成本再决定**(moba 目标文档 §四)。
- 回合制被确认为独立长期产品且归 Go 团队 → 重开 Go 重写,以 C++ 录制的事件流 golden 为跨语言基线。

## 6. 对既有决策的修订与过渡路径

- **D6 修订**:「上行走 gate→gRPC,下行走 Kafka」定为**过渡形态**,只为 Unity 第二连接未落地前保通。第二连接落地后,同批删除:gate 中继的 `BattleClientPlayer` gRPC 路径、Kafka `gate-{id}` S2C 回落、gate 里的 battle 专属代码(白名单项、绑定优先规则、`battle_binding_helper.cpp`、`ClearBattleRecord`、OnNodeRemove 分支)。最终 battle 只有两条客户端路径:直连 TCP 上行 + 直连 TCP 下行。
- **D2 保留**:独立节点类型、全局池、gRPC 注册 `PROTOCOL_GRPC`。

## 7. 顺手记录的文档/代码漂移(与选型无关,但应清)

- `cpp/generated/proto_helpers/proto_util.cpp:33` 的 `IsTcpNodeType` **含** BattleNodeService,而 `battle_handler.h/.cpp`、`node_util.h:123-133`、turn-based 文档 §5.5 都说「不加」。后果:TCP 握手能过 `registration_manager.cpp:118`;gate 不误拨只是因为 etcd 里 protocol_type 是 GRPC。
- `ARCH.md` §6.1 / §6.3 把 gate↔scene 写成「直连 gRPC」,注册表里是 `PROTOCOL_TCP`(`rpc_event_registry.cpp:786-793`)。
- `battle_client_edge.h:3`、`battle_security.h:11` 引用 turn-based 文档「§18 D23-D28」,该文档止于 §17。
- turn-based 文档 §8 承诺 K8s Deployment,`deploy/k8s/manifests` 无 battle;`Dockerfile.cpp` 只出 gate + scene。
- `table_expression.h` 在 exprtk 里注册了基于 `rand()` 的 `random` 函数,策划在伤害表达式里一用就绕过引擎的种子 RNG,破坏确定性。

## 8. 问答:客户端有几条连接,battle 为什么不经 gate,scene 为什么经 gate(2026-09-29)

> 起因:用户 2026-09-29 提问「为什么我的 battle 节点不经过 gate,现在客户端有几条连接,为什么这么设计」。
> 依据:四路静态调研,关键结论逐条对过代码。没有读客户端仓(AGENTS §9),Unity 侧只转述本仓文档;没有编译,没有运行。
> 行号以 2026-09-29 工作树为准(HEAD `766cb037c`)。
> 本节只回答「是什么、为什么」。**各环境实际走到哪一步**,见 [turn-based-battle-server.md §21](./turn-based-battle-server.md)。

### 8.1 客户端有几条连接

**平时 1 条长连接(gate);参战或观战期间 2 条(gate + battle)。** 登录前后另有打给 Java 网关的 HTTP 短请求,不算长连接。

| 阶段 | 对端 | 传输 | 生命周期 | 承载 | 鉴权 |
|---|---|---|---|---|---|
| 登录前后 | Java 网关(本地 `:8081`) | HTTP 短请求 | 按请求 | `GET /api/server-list`(只在未指定 zone 时调)、`POST /api/assign-gate`(排队时轮询 `/api/queue-status`)、会话期间 `POST /api/refresh-token` | 无 / access token |
| ① 大厅 | gate(本地 zone1 `:10000`) | muduo TCP,ProtobufCodec | 登录到登出 | Login / CreatePlayer / EnterGame;scene 的全部客户端消息;chat / guild / friend / team / trade / match 等 Go 服务经 `client_rpc_router`;战斗的分配包、开战包、重连提示 | 首包 `ClientTokenVerifyRequest`(gate token,login 签发,有效期 10 分钟) |
| ② 战斗 | battle 节点自己的 TCP 端口(`NodeInfo.endpoint`,本地 `RPC_PORT=20010`) | muduo TCP,**与 gate 同一套 codec** | 每局一条;终局、作废或观众被清退时,服务端推完终局包再关 | 上行只放 4 个消息号(`SubmitBattleAction` / `GetBattleState` / `StopWatchBattle` / `SetAutoBattle`);下行是回合结果、终局、观战帧 | 首包 `BattleTokenVerifyRequest`(battle 自签的票据,寿命等于房间 deadline,重连可重复使用) |

- **第 ② 条连接怎么来的**:
  - battle 在 CreateBattle / AddObserver 时自签票据,组成 `BattleAssignedS2C{host, port, token_payload, token_signature, expire_at_ms, role}`,经 Kafka → gate → 大厅连接推给客户端(`battle_room_manager.cpp:562-577, 1291-1360`)。
  - 客户端据此建第二条连接(`robot/battle_direct_conn.go:1-10`)。
  - 票据丢了,客户端经大厅调 `MatchService.RequestBattleTicket`,match 再调 `BattleNode.IssueBattleTicket`,由 battle 补签。
- **两条连接共用一张 handler 表**:客户端不关心 S2C 来自哪条连接(turn-based §18.2 契约)。
- **同一时刻最多一条有效的 battle 直连**:
  - 同一房间内:重连即替换旧连接(`BattleRoomManager::AttachDirectConnection`)。
  - 跨房间:由 match 的互斥保证。持有 `battle:lock` 或匹配票时不许观战;`spectate:watching` 用 SETNX 保证同时只观一场。
  - 重连替换或换场观战的瞬间,旧连接可能还没关完,会短暂重叠。
- **跨 zone 传送是替换 gate 连接**:先连新 gate,再关旧 gate(`robot/pkg/redirect.go`),连接数不增加。
- 没有单独的聊天、世界频道或语音连接。

### 8.2 battle 为什么不经 gate(按分量排序)

1. **gate 是全体玩家共用的漏斗。** 战斗是最高频的流量,而 gate 的客户端面只有一个 IO 线程。如果中继:
   - gate 每条消息都要 parse 进 prototype、组 `SessionDetails`,再走 gRPC;
   - battle 收到后再解一次;
   - 应答回来,gate 还要按 response 类型全名反查 message_id。

   证据见 `client_message_processor.cpp:726-798`、`gate/main.cpp:268-313`。参照项目实测,网关这个漏斗在 69 房时卡死([moba-battle-target-architecture.md](./moba-battle-target-architecture.md) §四)。
2. **battle 是全局池,经 gate 中继必然 N×M。**
   - scene 按 zone 划分,gate 只连本 zone 的 scene,连接数有上限。
   - battle 不分 zone(`node_util.cpp` `IsGlobalPoolNodeType`)。中继就要求每个 gate 对每个 battle 建 channel + CompletionQueue + stub(`node_connector.cpp:39-82`),应答还要等 5ms 的 CQ 排空定时器。
   - 旧模式下,这张 N×M 的连接网至今还在。
3. **一局只活在一个进程的内存里。** 客户端必须连到持有房间的那一台。票据签给具体实例(`battle_node_id` + 实例 UUID `battle_instance_id`),battle 在本地验签,不回头问大厅(§4)。
4. **battle 要能随时被杀。** gate 与 battle 之间没有连接,battle 崩溃、扩缩容、换版本都不会牵动 gate,gate 也不用再保存战斗绑定状态(`boundBattleIdBySession`)。这是会话制对局的判据(moba 目标文档)。
5. **改造成本低。** gate 客户端面的整套能力原样搬进了 battle(turn-based D27):ProtobufCodec、HMAC 验签、握手期限、体积上限、限速、消息号白名单、非法包阈值、输出高水位。客户端的第二条连接复用同一套 codec 和 handler 表,不需要第二套协议栈。
6. **按平面拆传输。** match → battle 的控制面每局只有一两次调用,留在 gRPC;客户端面是热路径,不进 gRPC(§2、§3.1);结算走 Kafka。
7. **票据由 battle 自签,不由 match 签**(turn-based D24):
   - 只有 battle 知道房间名单;
   - 分配包和开战包由同一个生产者按同一个 key 发出,才能保证先后顺序;
   - 少一个持有密钥的服务。

### 8.3 scene 为什么仍经 gate

- **scene 不能随时被杀。** 它常驻、按 zone 划分,是玩家权威数据的唯一写入方。按 battle 的方式把它抽出来是范畴错误([session-extractability-mmo-slg.md](./session-extractability-mmo-slg.md))。
- **换场景对客户端无感。** 玩家频繁换场景,gate 按 Kafka `RoutePlayer` 改写会话绑定即可,客户端连接不变。如果直连 scene,每次换图都要重建连接。
- **连接数有上限。** gate 只连本 zone 的 scene;跨 zone 由客户端换 gate(RedirectToGate)。
- **安全层只做一份。** gate 是 scene 面的安全层(逐个消息号白名单、限速、非法包计数)。直连 scene,就要把这层复制到每个 scene。

### 8.4 这个例外在什么条件下成立

业界标准是「客户端只连一个接入层」(见 [client-access-band-routing.md](./client-access-band-routing.md) 第一部分)。battle 直连属于这个标准允许的唯一一类例外:**权威实时服,凭短期票据**。xuanming 的第二条连接同样连的是 DS。

新服务想让客户端直连,必须同时满足以下 6 条:

1. 会话形状:有始有终,入口一份快照,出口一份结果,中间状态外界不需要;
2. 不写玩家库,进程可以随时被杀;
3. 票据本地验签,不回头问大厅;
4. 直连面按 gate 的标准同等设防;
5. 每个落点都有客户端能访问的入口;
6. 流量高到值得绕开共享漏斗。

chat / guild / friend / trade 等业务服不满足,一律经 gate + `client_rpc_router`。2026-09-28 已否决「客户端直连各业务服」。

### 8.5 代价

- 客户端要分别管两条连接的生命周期:断线重连、票据过期补签,两条连接互不干扰。
- 每个 battle 落点都需要客户端能访问的入口(hostPort / NodePort / 外部 L4),K8s 上还没解决。
- 多了一个对公网开放的端口。票据的风险:
  - 走明文 TCP,只有 adler32 校验;
  - 不绑定客户端 IP,整局有效;
  - 一旦泄露,别人就能冒名接入,并借重连替换把正主挤掉;
  - 要吊销只能销毁房间(turn-based §18.7 已列为不做)。
- 收缩到位后(D39),连不上 battle 端口的客户端就不能战斗。

### 8.6 §7 漂移复核(2026-09-29)

| §7 条目 | 现状 |
|---|---|
| `IsTcpNodeType` 含 BattleNodeService | 仍在(`proto_util.cpp`)。根因是生成模板对全部 `eNodeType` 一律输出,要修得改 `tools/proto_generator` 模板,不能手改生成物 |
| ARCH §6.1 / §6.3 把 gate↔scene 写成 gRPC | 仍在(`ARCH.md:225`)。`gate-scene-relay-architecture.md` 全文同样按 gRPC 叙述。实际上 scene 客户端方法是 `protocol=0`(muduo TCP) |
| 引用「§18 D23-D28」而文档止于 §17 | 已解决:turn-based 文档已写到 §21 |
| 没有 battle 的 K8s Deployment / 镜像 | 部分解决:`Dockerfile.cpp` 已构建 battle,`k8s_deploy.ps1` 有 `Apply-BattlePool` 和密钥注入。集群外入口仍缺(对外通告的是 POD_IP),turn-based §18.6 的文字已过时 |
| exprtk 的 `random` 基于 `rand()` | 仍在(`table_expression.h`) |

新增漂移:

- **本文 §2.1「battle → gate 没有这条边」写的是目标形态,不是现状。** 当前:
  - battle 仍经 Kafka 给 gate 发 `BindBattleEvent`;
  - 没有直连时,所有 S2C 都回落到 Kafka → gate;
  - 旧模式下 gate 仍持有 battle 的 stub。
- **本文 §3.1、§3.3 引用的行号已经漂移:**
  - `client_message_processor.cpp:634-707` 现为 `:726-798`;
  - `gate/main.cpp:198` 现为 `:199-217`;
  - `grpc_init_client.cpp:213-221` 现为 `:436-444`。
- `proto/battle/player_battle.proto` 的头注释仍写「客户端协议经 gate 以 gRPC 直达、下行走 Kafka」。
- ARCH §1 总体拓扑图里没有 battle 节点,也没有客户端的第二条连接。
