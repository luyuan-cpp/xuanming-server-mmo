# C++ 生成 gRPC 客户端:每次调用设 deadline + 失败也回调(2026-09-28)

> 状态(2026-09-28):两批均已落码,第一批 regen 已完成(897ac8241)。**全部未编译、未测试**,编译与单测交 Codex
> (AGENTS.md §10.1),步骤见 §9.2。
> 相关:`docs/design/microservice-zone-contract-20260914.md` §3「超时预算」、`docs/design/cross-zone-scene-travel.md` §12.2、
> `tools/proto_generator/protogen/internal/template/grpc_async_client.{h,cpp}.tmpl`、`grpc_init_total.{h,cpp}.tmpl`。

## 1. 缺陷(按 HEAD 4e409c5c3 / 24dd3e6c5 核实)

| # | 缺陷 | 位置 | 后果 |
|---|---|---|---|
| D1 | 调用失败不回调:只有 `status.ok()` 才调 `Async<Svc><Method>Handler`,否则只打一行 `LOG_ERROR << error_message()` | 模板 `grpc_async_client.cpp.tmpl:92-97`;生成物如 `scene_manager_service_grpc_client.cpp:26-31` | 业务层收不到失败,每个调用点各写看门狗兜底:`player_lifecycle.cpp` 的 5s 在途标记(`kSceneChangeInFlightTtlMs`)与 30s 交接应答看门狗、`id_segment_bootstrap.cpp` / `GuidSegmentClient` 的 `fetchTimeoutSec`;镜像副本 CreateScene、玩家换图、gate 转发在 gRPC 失败时客户端什么也收不到 |
| D2 | 不设 deadline:`cpp/`、`tools/` 下 `set_deadline(` 为 0 | 模板 `:116`、`:141` 附近 | 服务端不回、连接半死时调用永远挂着;"上游比下游宽"无从谈起 |
| D3 | 应答处理器靠手装,生成器不知道谁装、也不报漏装 | `cpp/nodes/scene/main.cpp:96-110` 手装 `InitSceneManagerReply()` | 2026-04-16 `f1b110bcc` regen 抹掉注册调用后,`if (handler)` 静默跳过每条换图应答约 5 个月 |

附带确认的既有事实(不在本次修复范围,见 §10):
- `HandleCompletedQueueMessage` 的 `if (!ok) { LOG_ERROR; return; }`:unary 的 Finish 标签 gRPC 保证 `ok == true`,本方案不受影响;对流(etcd Watch / LeaseKeepAlive)会泄漏 tag、提前 return、流永久失效。
- gate 通用路径发出的请求是 `gRpcMethodRegistry` 里的**共享原型** `*requestProto`,下一条客户端消息就会覆盖它 —— 失败回调若要交还请求,必须在调用对象里存副本。

## 2. 目标与非目标

目标:
1. 每次 **unary** 调用设 deadline,数值按**目标节点类型**可配,且遵守「上游比下游宽」。
2. 非 OK 状态通知业务层,且业务层能认出**是哪一次请求**失败(关联号 / 会话 / 请求编号)。
3. 漏装应答处理器不再静默。
4. 9 个现有调用点逐个核对是否需要接失败回调(§5)。

非目标:
- 双向流(etcd Watch / LeaseKeepAlive)**不设 deadline、不接失败回调**:两条流在 `InitEtcdGrpcNode` 里建一次、进程内不重建,设 deadline 等于让它们到期即永久死亡。流的失败处理是另一件事(§10)。
- 不改任何 Go 服务的超时值(login 的疑似笔误只报告,§4.3)。
- 不做生成器自动注册(评估见 §6)。

## 3. 接口设计

### 3.1 选型:另提供失败处理器,不改现有应答处理器签名

两种候选:

| | A. 应答处理器带上 status:`(ctx, status, reply)` | B. 另加失败处理器 `Async<Svc><Method>FailedHandler` |
|---|---|---|
| 与现有注册方式 | 改掉所有既有签名(etcd 8 处、scene 3 处、gate 通用桥) | 与 `Async…Handler` / `Set<File>Handler` / `SetIfEmptyHandler` 一一平行 |
| 失败时能认出是哪次请求吗 | **不能**:失败时 `reply` 是空的,`correlation_id` / `creator_ids` 都在应答里,拿不到 | 能:失败处理器拿到**发出的请求**与**发出的 metadata** |
| 并行会话冲突面 | 大 | 只增不改 |

选 **B**。决定性理由是第二行:应答类回调按应答里的回显字段对号(`EnterSceneResponse.correlation_id`、`CreateSceneResponse.creator_ids`),失败时这些字段不存在;gate 按服务端回写的 initial metadata 找会话(`GetSessionDetailsByClientContext`),失败时服务端没有回写。能认出"哪一次"的只有请求端自己的数据。

### 3.2 生成的形状(每个 unary 方法)

```cpp
// grpc_client/grpc_call_tag.h(手工维护的共享支撑头,不是生成物)
using GrpcSentMetadata = std::vector<std::pair<std::string, std::string>>;
struct GrpcCallFailure {
    uint32_t messageId;                  // 与 gRpcMethodRegistry 同一编号
    const char *method;                  // "SceneManager.EnterScene"
    const grpc::ClientContext &context;
    const grpc::Status &status;          // 非 OK
    const GrpcSentMetadata &sentMetadata;// Send 传入的原值(Base64 之前)
    const std::string *FindSentMetadata(std::string_view key) const;
};
inline constexpr uint32_t kDefaultGrpcCallDeadlineMs = 10000;

// <file>_grpc_client.h
struct Async<Svc><Method>GrpcClient {
    ... context / status / reply / response_reader(不变)
    Req request;                 // 新增:请求副本(失败时交还;gate 发的是共享原型,必须拷)
    GrpcSentMetadata sentMetadata; // 新增
};
using Async<Svc><Method>FailedHandlerFunctionType = std::function<void(const GrpcCallFailure &, const Req &)>;
extern Async<Svc><Method>FailedHandlerFunctionType Async<Svc><Method>FailedHandler;
void Set<File>FailedHandler(const std::function<void(const GrpcCallFailure &, const google::protobuf::Message &request)> &);
void Set<File>IfEmptyFailedHandler(同上);
void Set<File>CallDeadline(std::chrono::milliseconds);

// grpc_init_client.h(总表)
void SetFailedHandler(...);  void SetIfEmptyFailedHandler(...);           // 与 SetHandler / SetIfEmptyHandler 平行
void SetGrpcCallDeadline(uint32_t nodeType, std::chrono::milliseconds);   // 按目标节点类型分发到各文件,同 InitGrpcNode
```

完成路径(`AsyncCompleteGrpc<Svc><Method>`):

```
status OK  且装了应答处理器   → 调它(不变)
status OK  且没装             → 每方法每线程 LOG_ERROR 一次「应答被丢弃」(§6 的绊线),不再静默
status 非 OK 且装了失败处理器 → 调它(GrpcCallFailure + 请求副本)
status 非 OK 且没装           → LOG_ERROR,带方法名与状态码(与今天等价,信息更全)
```

发送路径:`Send` 两个重载先把请求拷进调用对象、记下 metadata 原值、`context.set_deadline(now + 本文件当前 deadline)`,再 `PrepareAsync`。不带 metadata 的重载改为转调带 metadata 的重载(空 metadata),模板里少一份重复。

成本:每次调用多一次请求拷贝(gate 转发的 ClientRequest 受 1024B 上限约束)、一个 `vector` 与一次 `atomic load`,相对一次 gRPC 往返可忽略;不在 per-tick 路径。

### 3.3 失败的语义:**结果未知**

`DEADLINE_EXCEEDED` / `UNAVAILABLE` / `CANCELLED` 不代表对端没执行:go-zero 的超时拦截器在服务端超时后先回 DeadlineExceeded,handler goroutine 仍可能继续跑完;连接在请求发出后断开也报 UNAVAILABLE。因此失败处理器只做**不依赖对端是否执行**的事:

- 释放本地在途记录;提示客户端"服务不可用";按退避重试**幂等**请求。
- **不得**把传输失败当作"被拒"的证据去改权威状态。交接(归属转移)的去留仍只由 owner_epoch 核实与既有看门狗裁决(§5 #1)。

## 4. Deadline

### 4.1 配置

`bin/etc/base_deploy_config.yaml` 新增顶层块(`config.proto` `BaseDeployConfig.grpc_client = 21`,`GrpcClientConfig.call_deadline_ms` 为 `map<string, uint32>`,键是 `ENodeType` 枚举名):

```yaml
GrpcClient:
  CallDeadlineMs:
    SceneManagerNodeService: 10000
    ...
```

- 没列出的目标类型用 `kDefaultGrpcCallDeadlineMs`(10000)。整块缺席(旧 ConfigMap)同样落到它。
- 键不认识 / 值为 0:`LOG_ERROR` 后忽略,该类型保持默认 —— 0 = 不限时,违反预算(与 chat `Validate` 拒 0 同一口径);未知键照 IdSegments 的先例不 FATAL(滚动升级时 yaml 可能先于二进制认识新类型)。
- 应用时机:`Node::Initialize` 里 `LoadConfigs()` 之后、`InitEtcdService()` 之前,由 `grpc_call_deadline::Apply` 写进生成的客户端并留一份供业务查询(`grpc_call_deadline::Get(nodeType)`);启动日志打一行生效值。
- 存储:每个生成文件一个 `std::atomic<uint32_t>`,与应答处理器同为进程级全局;原子量避免"通常不会并发"的假设(AGENTS §11.3)。

### 4.2 预算表(上游比下游宽)

两条不等式,缺一不可:
1. **C++ deadline > 目标服务的服务端超时**:下游先超时,C++ 才能收到带真实错误码的应答,而不是自己的 DeadlineExceeded(scene_manager 把 Timeout 调到 8000 正是为此)。余量取 ≥ 2000ms。
2. **依赖该调用结果的 C++ 业务预算 > deadline**:否则业务先放弃、槽位被下一次请求覆盖,随后到的应答 / 失败回调对不上号。做法是**从 deadline 派生**,不再各写一个常数。

| 目标节点类型 | C++ deadline | 下游服务端超时 | 从它派生的 C++ 上游预算 |
|---|---|---|---|
| SceneManagerNodeService | 10000 | `scene_manager_service.yaml` Timeout 8000(= 归属查询 1500 + Kafka 写 5000 + 1500) | 在途换图 TTL = deadline + 1000(第二批);交接应答看门狗 30s(不变,> deadline) |
| DataServiceNodeService | 4000 | `data_service.yaml` 未写 Timeout = go-zero 默认 2000 | 号段 `fetchTimeoutSec` = deadline + 1s = 5s(第一批) |
| ClientRpcRouterNodeService | 8000 | 路由服 Timeout 6000(> ForwardTimeoutMs 5000 > 业务 4000) | 无(客户端自己等) |
| MatchNodeService(gate 直连模式) | 7000 | `match_service.yaml` Timeout 5000 | 无 |
| BattleNodeService(gate 直连模式) | 5000 | C++ battle 服务端同步处理,无服务端超时 | 无 |
| LoginNodeService(gate 直连模式) | 102000 | `login.yaml` Timeout **100000**(行尾注释写 10s) | 无 |
| EtcdNodeService | 5000 | etcd 无服务端超时 | etcd Txn 看门狗 10s、LeaseGrant 重试 10s(`etcd_service.cpp`,不变,> deadline) |
| 其余(Chat / Friend / Guild / Team / Trade) | 默认 10000 | 4000 | C++ 不直连(节点白名单里没有),只经路由服 |

### 4.3 需要拍板:login 的 Timeout

`go/login/etc/login.yaml:7` 与 K8s ConfigMap(`k8s_deploy.ps1` 约 2360 行)都是 `Timeout: 100000`,注释写"(10s)"。按规则 gate 直连 login 的 deadline 只能取 > 100s,本方案取 102000,"守规则"但几乎等于不设。若 100000 是笔误,改成 10000 后本表 login 行改 12000。路由模式下 login 实际受路由服 `ForwardTimeoutMs 5000` 约束,不受此影响。

### 4.4 K8s

node ConfigMap 以 readOnly 整目录挂载,**完全遮蔽**镜像里的 `base_deploy_config.yaml`。第二批在 `k8s_deploy.ps1` 用 `Get-AuthoritativeYamlBlock -Key 'GrpcClient'` 原样搬运(与 IdSegments 同法),并加一条契约断言:每个 C++ deadline ≥ 对应服务 yaml 的 Timeout + 2000。第一批落地后、第二批之前,K8s 上全部目标取默认 10000:scene_manager / 路由服 / match 仍满足不等式 1;etcd 10000 与 Txn 看门狗 10s 相等(比今天的"无限"好,但没有余量);号段 fetchTimeout 从 deadline 派生为 11s,仍自洽。

## 5. 调用点逐个结论

行号按 24dd3e6c5(任务描述里的行号已随 4e409c5c3 漂移,括号内是任务给的旧号)。

| # | 调用点 | 发出的 RPC | 失败时业务要什么 | 结论 | 批次 |
|---|---|---|---|---|---|
| 1 | `player_lifecycle.cpp:2211` `SendEmergencyRelocateEnterScene`(旧 :2163)与 `RequestTravelEnterScene`(交接) | EnterScene,经唯一出口 `SendCorrelatedEnterScene`(:297) | 交接:**结果未知**,不能当失败证据 | 失败处理器按**请求里的 correlation_id** 走 `enter_scene_reply::Classify`;命中交接只记日志,继续由 30s 看门狗 / owner_epoch 裁决;疏散无等待者,只记日志 | 二 |
| 2 | `player_lifecycle.cpp:2842` `RequestSceneChange`(旧 :3040) | EnterScene | 普通换图:释放单槽在途记录;玩家发起的给提示 | 同上 Classify;命中换图:先抄后摘在途组件;`playerRequested` 回 `kServiceUnavailable`(「服务不可用」,不断言"换图失败",见下),队伍跟随只记日志;TTL 改为 SceneManager deadline + 1000 | 二 |
| 3 | `player_scene_handler.cpp:163`(旧 :164)玩家换图 | 经 #2 | 同 #2 | 调用点不改 | — |
| 4 | `player_team.cpp:490`(旧 :493)队伍跟随 | 经 #2,`playerRequested=false` | 只记日志、释放槽、不回 tip(组队线约定) | 调用点不改 | — |
| 5 | `scene_manager_response_handler.cpp:150`(旧 :152)镜像自动进场 | 经 #2 | 同 #2 | 调用点不改 | — |
| 6 | `player_scene.cpp:196` 镜像副本 CreateScene | CreateScene | 自动进场由**应答**驱动,传输失败 = 这次一定不会自动进场 | 装 `AsyncSceneManagerCreateSceneFailedHandler`:按请求 `creator_ids` 找本节点上的玩家,回 `kServiceUnavailable`;镜像若其实已建,由 scene_manager 按空场景回收 | 二 |
| 7 | `id_segment_bootstrap.cpp:156` 领号段 | AllocateIdSegment | 立即按退避重试,不必等 fetchTimeout | 失败处理器按发出的 `x-idseg-seq` 精确归属:命中在途 → `GuidSegmentClient::OnTransportFailure`;命中已放弃的那次 → 只清归属(客户端已由 fetchTimeout 判过);`fetchTimeoutSec` 从 deadline 派生,降为兜底 | 一 |
| 8 | `client_message_processor.cpp:163` 路由模式转发 | ClientRpcRouter.Forward | 告诉客户端"服务不可用" | gate 装通用 `SetIfEmptyFailedHandler`:从发出的 `x-session-detail-bin` 找回会话 → `SendTipToClient(kServiceUnavailable)`;日志按 1024 条采样(同 `SendViaRouter`) | 一 |
| 9 | `client_message_processor.cpp:480` 直连断线通知 | Login.Disconnect | 客户端已断,无处可回 | 同一通用处理器,会话已不在 → 只记日志;login 侧会话状态由其自身 TTL 收口 | 一 |
| + | gate 直连模式通用路径 `client_message_processor.cpp:792` `rpcHandlerMeta.sender` | 任意客户端 gRPC 消息 | 同 #8 | 同 #8,调用点不改 | 一 |
| + | etcd 一元调用(`etcd_helper.cpp`) | Range / Put / Txn / LeaseGrant / LeaseRevoke | 见下 | **不装失败处理器**,保持"只打日志 + 既有 10s 看门狗 / 2s 周期重发";deadline 5000 < 看门狗 10000,反而消除了"旧调用的迟到应答被记到新 pending key 上"的错位 | 一(只 deadline) |

**#2 为什么回「服务不可用」而不是「换图失败」**:传输失败时 scene_manager 可能已执行、路由事件随后到达。回 `kEnterSceneFailed` 会出现"先报失败、后被搬走"。但常见的失败形态是 scene_manager 整个不可达(UNAVAILABLE 立即返回),此时不回任何提示,客户端就会一直等一个不会来的 `EnterSceneS2C`(用户明确提出的缺陷)。取中:回措辞不断言结果的 `kServiceUnavailable`,并释放槽位允许重试。"先提示、后成功"只在 scene_manager 服务端超时(已调到 8s 盖住全部内部预算)或请求发出后断连时出现,属罕见路径。跨 zone 传送线建议过"只释放槽不提示",此处取舍记录在案,若需改为不提示,只动 `DispatchEnterSceneTransportFailure` 一行。

**#1 的约束(与跨 zone 传送线对齐)**:按请求的 correlation_id 分发,**不按 player_id**(否则会把 4e409c5c3 刚修掉的"吃掉别的请求的应答"带回来);交接的传输失败**不**作为失败证据解冻 —— 那会在其实已被放行时解冻并带新 epoch 存盘,等于回档;也不提前触发核实:UNAVAILABLE 可能发生在 scene_manager 的 Kafka 写 / 回滚窗口中间,`kTravelReplyBudgetSec` 注释里的下限约束正是为此。

## 6. 应答处理器能否由生成器自动注册

结论:**这次不做自动注册,只做"漏装即报"的绊线**;自动注册作为独立任务。

现状:
- `WriteRepliedRegisterFile`(`internal/register_gen.go:82`)只为 `IsSceneNodeReceivedProtocolResponseHandler`(`generator_scene_node_util.go:73`)选中的服务生成 `Init<Svc>Reply()`,该谓词要求 `cc_generic_services`,gRPC 服务**永远**选不进来;整份 `register_response_handler.cpp` 每次 regen 全量重写,没有守护段。
- 生成器没有任何"某节点作为客户端调用哪些 gRPC 服务"的声明:`proto_gen.yaml` 的 `domain_meta.<域>.rpc.type` 只说服务端是 grpc 还是 muduo;`InitGrpcNode` 对所有 gRPC 文件按目标节点类型建 client,不按调用方过滤。

要做自动注册需要:
1. 新增调用方声明(如 `proto_gen.yaml` 加 `grpc_clients: { scene: [scene_manager, data_service] }`),否则会给每个节点生成全部十几个 gRPC 域的桩。
2. 新生成函数:为声明的方法生成带守护段的自由函数 `On<Svc><Method>GrpcReply(const ClientContext&, const Resp&)` / `On<Svc><Method>GrpcFailed(...)`,`Init<File>GrpcReply()` 负责赋值;不能复用 `GetServiceRepliedHandlerCppStr`(签名绑了 `TcpConnectionPtr`)。守护段解析器只在命中函数签名后收集,自由函数比 lambda 稳。
3. `WriteRepliedRegisterFile` 接受第二类回调,让生成的 `InitReply()` 也调用 gRPC 的 Init。
4. 迁移 `InitSceneManagerReply` / `InitDataServiceReply` 进守护段,删 `main.cpp:110`。
5. 生成器 Go 源码改动要**重编 proto-gen.exe**(这些是编进二进制的字符串模板,与 `.tmpl` 不同),属于构建,按 §10.1 交 Codex;并补"守护段保留 + 幂等"的 Go 单测。

估算中等(Go 150–250 行 + 测试 + 两处迁移)。它能消除的风险——"注册调用被 regen 抹掉"——今天已不存在:两处注册都在 regen 不碰的手写文件里(`scene/main.cpp`、`id_segment_bootstrap.cpp`)。剩下的风险是"有人删了注册调用或新调用方忘了装",本次的绊线(应答到达而处理器为空时每方法每线程 `LOG_ERROR` 一次)在任何冒烟的第一条应答上就会暴露,成本只在模板里。

## 7. 风险与兼容

- 行为变化 1:原来"永远挂着"的调用,现在到 deadline 以失败结束。没装失败处理器的方法表现为多一行 ERROR 日志,状态与今天收到 UNAVAILABLE 时相同。
- 行为变化 2:gate 客户端请求在下游失败时收到 `kServiceUnavailable`(fault=1)。有副作用的 C2S 靠 Go 侧幂等键兜重试(契约 §3)。
- 行为变化 3(第二批):scene_manager 卡住时,玩家换图最长被挡 deadline + 1s(11s,原 5s),但到 deadline 一定收到提示;原来 5s 后放行、第一条的迟到应答被丢弃的残余随之消失。
- 协议 / 存储:无。`config.proto` 只新增字段 21,旧 yaml 不写该块照常启动。
- CreateScene 的**业务**失败(`error_code != 0`)应答不回显 `creator_ids`(`go/scene_manager/internal/logic/createscenelogic.go` 各错误返回),C++ 仍无法告诉创建者;要修需 scene_manager 在错误应答里也回显 `CreatorIds`(Go 侧一行,另立项)。

## 8. 分批落地(多会话协作)

跨 zone 传送线正在改 `player_lifecycle.{h,cpp}`、`scene_manager_response_handler.cpp`、`k8s_deploy.ps1` 及其契约测试(冻结上限 → CPP-3 → GO-2 串行),约定这三处排在它之后。

- **第一批(本次)**:生成器模板 4 个、`grpc_call_tag.h`、生成器 Go 单测断言;`config.proto` 字段 21 + `config.cpp` + `base_deploy_config.yaml`;`core/node/system/grpc_call_deadline.{h,cpp}`(+ core 工程登记)与 `node.cpp` 应用;gate 通用失败桥(#8/#9/直连);号段(#7)+ `GuidSegmentClient::OnTransportFailure` + 回归单测;全量 regen。
  第一批单独可编译:失败处理器与 deadline 都是"可选装",没装的方法行为与今天一致(只多 deadline)。
  分工会话的复核发现首版 DataService 取 2500 只有 500ms 余量,违反本块自己的 +2000 规则,改为 4000(yaml 由该会话改)。
- **第二批(跨 zone 线提交后)**:`PlayerLifecycleSystem::DispatchEnterSceneTransportFailure` + TTL 派生(#1/#2);`scene_manager_response_handler.cpp` 装 EnterScene / CreateScene 失败处理器(#6);`k8s_deploy.ps1` 镜像 GrpcClient 块 + 契约断言(§4.4);更新 `cross-zone-scene-travel.md` §12.2 与 `scene_manager_service.yaml` 注释里"生成客户端非 OK 不回调"的表述。

## 9. regen 与验证

### 9.1 regen(已完成)

由会话「C++ 完全不连接 Go 服务的设计」在本会话撞额度期间代跑(用户授权),产物随每小时自动保存 897ac8241 进 main:
- 用 `enable_unity_client: false` 的配置副本跑现成的 `proto-gen.exe`(09-16 构建;`.tmpl` 运行时从磁盘读,本次不改生成器 Go 源码,**不需要重编**),不写 `../mmorpg-client`。
- 在隔离的临时 worktree 里跑:当时 HEAD 上帮会会话的 guild.proto / guild_db.proto 多了 5 个尚未 regen 的 RPC(会分到消息号 239–243),跑之前把这两份 proto 退回上次 regen 时的内容,不占帮会的号。
- 拷回主仓库的只有 40 个文件:`cpp/generated/grpc_client/**`(34)、`cpp/generated/proto/common/base/config.pb.{h,cc}`、`generated/proto/{_unified,db,login}/proto/common/base/config.proto`、`go/proto/common/base/config.pb.go` 与逐字节同步的 `robot/vendor/proto/common/base/config.pb.go`。
- 已核:`proto/message_id.txt`、`rpc_event_registry.*`、`cpp/nodes/scene/handler/grpc/scene_node_service.cpp` 的 Agones 块都没动。
- 本会话复核生成物:34 个客户端文件与当前模板一致;etcd 的 Watch / LeaseKeepAlive 两个流式方法没有失败处理器、不设 deadline;`SetGrpcCallDeadline` 对同一节点类型的多个文件用独立的 if。

### 9.2 交给 Codex 的编译 / 测试(串行)

工作目录均为仓库根 `E:\work\xuanming-server-mmo`。MSBuild 路径 `D:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe`,**一律 `/m:1`**(并发会报假的 C1041 / LNK1104)。

1. 生成器模板单测(Go 1.26.5,`GOTOOLCHAIN=local`,`GOPROXY=https://goproxy.cn,https://mirrors.aliyun.com/goproxy/,direct`):
   `cd tools/proto_generator/protogen && go test ./internal/generator/cpp/ -run TestGrpcInitTemplateSupportsMultipleServicesForSameNodeType -count=1 -v`
   通过标准:PASS(新增断言:`SetGrpcCallDeadline` 两个 Battle 分支互相独立、两个文件都收到 deadline)。
2. 全量编译:`MSBuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。
   本次改动面:`proto`(config.pb 新增字段 21)→ `grpc_client`(34 个生成文件)→ `core`(新增 `node/system/grpc_call_deadline.cpp`,已登记 vcxproj / filters / CMakeLists)→ `modules`(`guid_segment_client`)→ `scene` 库(`player_lifecycle`)→ gate / scene / battle 三个节点 → 测试工程。
   通过标准:exit 0;`bin` 下 gate.exe / scene.exe / battle.exe 的 mtime 晚于 `lib` 下所有 .lib(防"新 grpc_client.lib + 旧 core.lib"混装)。
   失败时保留:第一条 `error C` / `LNK` 及其前后 20 行。重点怀疑点:`GrpcCallFailure` 聚合初始化(含引用成员)、`Send…(registry, node, request, {}, {})` 的重载决议、`google::protobuf::Map` 的 `operator[]`(`config.cpp`)。
3. 单测:`pwsh tools/scripts/run_cpp_tests.ps1 -Filter bag_test`、`pwsh tools/scripts/run_cpp_tests.ps1 -Filter cross_zone_test`。
   通过标准:新增 `GuidSegmentClientTest.TransportFailureRetriesWithoutWaitingForFetchTimeout` 与 `EnterSceneTransportFailureEcs.*`(5 条)全部 PASS;其余用例与本次改动前的基线一致(bag_test 09-28 基线里有 8 条他人在途区域的既有失败,见 PROGRESS.md 09-28 防御 / 法力条目)。
4. 联机冒烟(有本地栈时):起 gate / scene 后,
   - 日志里应有一行 `[GrpcClient] unary call deadlines (ms): default=10000 ...`(`LogLevel` 为 0 / 1 时可见;本地默认 2 = WARN 看不到,可临时调低),没有 `CallDeadlineMs entry ignored`;
   - 跑任意一条换图冒烟,不得出现 `reply dropped: Async...Handler is not installed`;
   - 停掉 scene_manager 后在客户端发一次换图:10s 内收到 tip 1003(服务不可用),再发一次不应被"切换中"(3014)挡住;
   - 路由模式下停掉路由服后发任意 gRPC 类消息:客户端收到 tip 1003,gate 日志有采样的 `gRPC client call failed (sampled)`。

## 10. 残留与后续

1. 流(etcd Watch / LeaseKeepAlive)的 `!ok` 分支:泄漏 tag、提前 return 跳过本轮其余事件、流不重建(KeepAlive 死后约每个 TTL 重注册一轮)。另立项。
2. etcd 一元调用若要接失败处理器:传输失败必须走与 `OnTxnTimeout` 等价的"取出 pending key 重发",**不能**走 `OnTxnFailed`(在 `kReRegisterExisting` 下会让节点因一次 etcd 抖动自杀),且要用请求里的 key 核对 `pendingTxnKey`。
3. 纯服务端流方法会被模板误生成成 unary(今天 proto 里没有此类方法)。
4. `SendMessageToPlayerOnGrpcNode`(`player_message_utils.cpp:270-315`)无调用方,且发的是 `*requestProto` 而非入参。
5. login Timeout 待拍板(§4.3);CreateScene 错误应答回显 `creator_ids`(§7)。
6. 生成器自动注册(§6)。
