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
- 不改任何 Go 服务的超时值。例外:login 的 `Timeout: 100000` 经用户确认是笔误,2026-09-29 改为 10000(§4.3)。
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

- 释放本地在途记录;提示客户端(gate 回「服务不可用」,换图回「服务器繁忙,请稍后再试」);按退避重试**幂等**请求。
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
| DataServiceNodeService | 4000 | `data_service.yaml` 未写 Timeout = go-zero 默认 2000 | 号段 `fetchTimeoutSec` = deadline + 1s = 5s(第一批;首版 deadline 2500 时为 3.5s) |
| ClientRpcRouterNodeService | 8000 | 路由服 Timeout 6000(> ForwardTimeoutMs 5000 > 业务 4000) | 无(客户端自己等) |
| MatchNodeService(gate 直连模式) | 7000 | `match_service.yaml` Timeout 5000 | 无 |
| BattleNodeService(gate 直连模式) | 5000 | C++ battle 服务端同步处理,无服务端超时 | 无 |
| LoginNodeService(gate 直连模式) | 12000 | `login.yaml` Timeout 10000(09-29 由笔误 100000 改正,原 deadline 102000) | 无 |
| EtcdNodeService | 5000 | etcd 无服务端超时 | etcd Txn 看门狗 10s、LeaseGrant 重试 10s(`etcd_service.cpp`,不变,> deadline) |
| 其余(Chat / Friend / Guild / Team / Trade) | 默认 10000 | 4000 | C++ 不直连(节点白名单里没有),只经路由服 |

### 4.3 已拍板:login 的 Timeout(2026-09-29)

`go/login/etc/login.yaml:7` 原为 `Timeout: 100000`,行尾注释却写"(10s)"。按规则 gate 直连 login 的 deadline 只能取 ≥ 100000 + 2000,首版取 102000,"守规则"但几乎等于不设。用户确认是笔误:
- `go/login/etc/login.yaml` 与 `deploy/login-stack.linux/login.yaml` 改为 `Timeout: 10000`;K8s 的 login ConfigMap 由 `New-GoSvcConfigMapYaml` 从前者镜像,自动跟随。
- `bin/etc/base_deploy_config.yaml` 的 `GrpcClient.CallDeadlineMs.LoginNodeService` 改为 12000(= 10000 + 2000,部署门禁 `Assert-GrpcClientDeadlineBudget` 恰好通过)。

10s 够不够(静态核对,未压测):
- 同步路径里最慢的 CreatePlayer:持锁后串行三次跨进程 gRPC,各有 3s 硬预算(发号 / 名字登记 / player:zone 登记)= 9s,常态 Redis 毫秒级,装得下。Redis 卡死时单条命令可重发到约 12.5s(`login.yaml` Locker 注释),这时服务端先超时、客户端收 1003;建角的正确性靠账号 blob 的围栏写 + 回读,不靠超时。另:服务端 ctx 截止时间从 100s 变成 10s 后,`createplayerlogic.go` 里 `hasBudget` 类判断会更早放弃重试 —— 慢的时候早失败,是预期效果。
- EnterGame:应答"已受理"后,预加载链在后台 `chainBudget = 5min` 里跑,`TaskWaitTimeout: 30s` 的浪涌余量不受本值影响。
- Login 快路径挑 gate 时的 etcd 探测(`NodeWatcher.FetchAllNodes`)自带 30s、不跟请求 ctx。etcd 卡住超过 10s 时,这次 Login 以 DeadlineExceeded 结束(客户端收 1003 重试);2026-05-24 压测记录的是"偶发 >5s",仍在 10s 内。
- 路由模式下 login 实际受路由服 `ForwardTimeoutMs 5000` 约束,不受此改动影响。

### 4.4 K8s

**已落地,未测**(分工会话,2026-09-29;大部分随 hourly save `ccfc40291` 进库,审查后的修正随其后的 hourly save,详见 PROGRESS.md「2026-09-29 C++↔Go 边界加固(V0+)分工部分」):
- node ConfigMap 以 readOnly 整目录挂载,**完全遮蔽**镜像里的 `base_deploy_config.yaml`。`k8s_deploy.ps1` 用 `Get-AuthoritativeYamlBlock -Key 'GrpcClient'` 把整块原样搬进 node-config 与 battle-node-config(与 IdSegments 同法),缺块则生成期 throw。
- 部署门禁 `Assert-GrpcClientDeadlineBudget`:挂在 zone-up / infra-up / all-up 写集群之前,核对 SceneManager / DataService / ClientRpcRouter / Match / Login 五个目标「C++ deadline ≥ 服务 yaml 的 zrpc Timeout + 2000」。五项必须在 `GrpcClient.CallDeadlineMs` 里显式写成正整数;服务 yaml 没写 Timeout 按 go-zero 默认 2000 计;Timeout 为 0 / 非数字,拒绝部署并逐项点名。纯函数 `Get-GrpcClientDeadlineBudgetViolations` 由契约测试喂构造值覆盖。*-down / *-status 不经过门禁。
- **`MethodTimeouts` 逐条核对(2026-10-09 订正,契约测试已实跑)**:go-zero 的 `MethodTimeouts` 能把个别方法的服务端超时单独放宽,而 C++ 的 deadline 是按目标节点类型配的一个值,对该目标的所有方法生效。所以表里的每一条都要满足「C++ deadline ≥ 该方法的超时 + 2000」。唯一的例外是 **C++ 节点从不调用的方法**:它再长也不存在「C++ 比服务端先放弃」,由 `k8s_deploy.ps1` 的 `Get-GrpcClientMethodsNotCalledFromCpp` 显式登记。今天只有 data_service 的 `RollbackPlayer` / `RollbackZone` / `RollbackAll`:它们由运维带 `x-admin-token` 发起,服务端在回档过程中要等帮会资产沉降并反向调用 guild 做检查与复查,超时是分钟到小时级。
  - 原口径是「出现 `MethodTimeouts` 就拒绝」。同一天(09-29)帮会线给 `data_service.yaml` 配了回档三个 RPC 的 `MethodTimeouts`(guild-phase2/07 §7.5.3-3),两边互不知情,结果是所有写路径在入口被拒、`k8s_deploy_contract` 与 `k8s_client_entry_contract` 两个契约测试从 09-29 起一直是红的,10-08 才在实跑时发现。
  - 解析:`Get-ZrpcMethodTimeouts` 专门读这张表(通用的拍平函数会把「映射的列表」里每一项的第二个键压到同一条路径上,方法名和超时对不上号)。它只认一种写法:键顶格、不带引号,键后换行,每项缩进并以 `- ` 开头。`ConvertTo-ZrpcDurationMs` 换算时长,认的是 Go 时长写法的一个子集:单位只认 ms / s / m / h,不认正负号与小数点一侧缺数字的写法(`+5s`、`.5s`、`5.s` Go 认,这里按解析不了拒绝)。
  - 拒绝而不是放行的情形:文件里别处提到了 `MethodTimeouts` 却不是上面那种键行(键带引号、整份文档统一缩进、流式 / JSON 写法、同一个键写两次 —— go-zero 走完整的 YAML 解析,这些写法在服务端照样生效;嵌在别的键下面的在服务端并不生效,但这里不去分辨,一并拒绝);文本里有单独的 CR 或 NEL / LS / PS 换行符(yaml.v2 把它们当换行,这里只按 LF / CRLF 切行,注释后面跟一个这样的字符,后续内容会被整段漏读);列表项顶格写;看不懂的行、未知键、重复键、缺键;有键却读不出条目而值又不是 `[]`;两套解析数出的条数不一致;任何一条(包括已登记的)的方法名不是 `/包.服务/方法` 的形状或超时解析不了。键名换大小写不会绕过核对(go-zero 的配置键不分大小写,`methodTimeouts:` 同样生效,这里照常读出来比)。
  - 登记表靠契约测试守住。C++ 调 Go 服务有两种形态,两种都查:① 直接调用 —— `cpp/` 手写代码(`cpp/generated` 除外)里出现生成客户端的符号 `Send<服务><方法>` / `Async<服务><方法>…` 或消息号常量 `<服务><方法>MessageId`;② gate 直连模式按消息号转发 —— 手写代码里不出现任何符号,所以消息号列在生成的 `IsClientMessageId` 里的方法一律算「C++ 会调用」。另外表里每一项都要对应服务 yaml 里的一条 `MethodTimeouts`,不留过期项。C++ 哪天开始调用其中某个方法,下一次跑契约测试时会红,届时必须把它移出登记表并让 deadline 盖住它的超时。触发时机要说清:部署脚本自身不扫 C++ 源码;CI 的 `deploy-config-tests` 只在 `tools/scripts`、各 `etc` 配置、k8s 清单等路径有改动时跑,发版流程(`release.yml`)必跑,所以只改 C++ 的提交不会当场变红。没有把 `cpp/**` 加进它的触发路径,是因为这个 workflow 逐个执行全部契约测试、任何一个失败就整体判红,而今天还有几套与此无关的既有失败(`k8s_client_entry_contract` 9 条等);先扩触发面只会给只改 C++ 的提交多一个修不了的红灯。等既有失败清掉,或把这条守护拆成独立的 job,再接上 C++ 路径。
  - 通过时的那一行会列出本次按「C++ 不调用」放行的条目,部署的人看得到。
  - 已知的边界(改前改后都一样,不是本次引入):键名用 YAML 转义拼出来(如 `"Method\x54imeouts"`,双引号里的 `\x54` 就是字母 T)时两套文本解析都认不出。普通的带引号键 `"MethodTimeouts"` 不在此列,它会被拒绝。另外,顶层 `Timeout` 仍由通用的拍平函数读取,它对其他顶层键下的顶格列表、合并键、多文档这几种写法读不准(五个目标文件今天都没有这些写法),这一点同样是改前就有的。这道门禁防的是配置之间的无心漂移,不防刻意绕过。
  - 没有顺带解决的事:K8s 的 data-service ConfigMap 仍不镜像这张表(`data_service.yaml` 的注释早有说明),集群里回档 RPC 取的是 go-zero 默认 2000ms;这属于帮会线「接上 guild 时一并处理」的范围,与本条预算无关。
- login 的 go-svc ConfigMap 改为从 `go/login/etc/login.yaml` 镜像 Timeout(原来写死)。

历史说明:第一批落地后、上述改动之前,K8s 上全部目标取内置默认 10000 —— scene_manager / 路由服 / match 仍满足不等式 1;etcd 10000 与 Txn 看门狗 10s 相等(比原来的"无限"好,但没有余量);号段 fetchTimeout 从 deadline 派生为 11s,仍自洽。

## 5. 调用点逐个结论

行号按 24dd3e6c5(任务描述里的行号已随 4e409c5c3 漂移,括号内是任务给的旧号)。

| # | 调用点 | 发出的 RPC | 失败时业务要什么 | 结论 | 批次 |
|---|---|---|---|---|---|
| 1 | `player_lifecycle.cpp:2211` `SendEmergencyRelocateEnterScene`(旧 :2163)与 `RequestTravelEnterScene`(交接) | EnterScene,经唯一出口 `SendCorrelatedEnterScene`(:297) | 交接:**结果未知**,不能当失败证据 | 失败处理器按**请求里的 correlation_id** 走 `enter_scene_reply::Classify`;命中交接只记日志,继续由 30s 看门狗 / owner_epoch 裁决;疏散无等待者,只记日志 | 二 |
| 2 | `player_lifecycle.cpp:2842` `RequestSceneChange`(旧 :3040) | EnterScene | 普通换图:释放单槽在途记录;玩家发起的给提示 | 同上 Classify;命中换图:先抄后摘在途组件;`playerRequested` 回专用码 `kEnterSceneServerBusy`(3028,「服务器繁忙,请稍后再试」,不断言"换图失败",见下;10-01 前为 `kServiceUnavailable`),队伍跟随只记日志;TTL 改为 SceneManager deadline + 1000 | 二 |
| 3 | `player_scene_handler.cpp:163`(旧 :164)玩家换图 | 经 #2 | 同 #2 | 调用点不改;同文件里"一个 SceneManager 都没注册"的分支原回 `kEnterSceneParamError`,改为与传输失败同一提示(09-29 `kServiceUnavailable`,10-01 起 `kEnterSceneServerBusy`) | 二 |
| 4 | `player_team.cpp:490`(旧 :493)队伍跟随 | 经 #2,`playerRequested=false` | 只记日志、释放槽、不回 tip(组队线约定) | 调用点不改 | — |
| 5 | `scene_manager_response_handler.cpp:150`(旧 :152)镜像自动进场 | 经 #2 | 同 #2 | 调用点不改 | — |
| 6 | `player_scene.cpp:196` 镜像副本 CreateScene | CreateScene | 自动进场由**应答**驱动,传输失败 = 这次一定不会自动进场 | 装 `AsyncSceneManagerCreateSceneFailedHandler`:按请求 `creator_ids` 找本节点上的玩家,回 `kEnterSceneServerBusy`(10-01 前为 `kServiceUnavailable`);镜像若其实已建,由 scene_manager 按空场景回收 | 二 |
| 7 | `id_segment_bootstrap.cpp:156` 领号段 | AllocateIdSegment | 立即按退避重试,不必等 fetchTimeout | 失败处理器按发出的 `x-idseg-seq` 精确归属:命中在途 → `GuidSegmentClient::OnTransportFailure`;命中已放弃的那次 → 只清归属(客户端已由 fetchTimeout 判过);`fetchTimeoutSec` 从 deadline 派生,降为兜底 | 一 |
| 8 | `client_message_processor.cpp:163` 路由模式转发 | ClientRpcRouter.Forward | 告诉客户端"服务不可用" | gate 装通用 `SetIfEmptyFailedHandler`:从发出的 `x-session-detail-bin` 找回会话 → `SendTipToClient(kServiceUnavailable)`;日志按 1024 条采样(同 `SendViaRouter`) | 一 |
| 9 | `client_message_processor.cpp:480` 直连断线通知 | Login.Disconnect | 客户端已断,无处可回 | 同一通用处理器,会话已不在 → 只记日志;login 侧会话状态由其自身 TTL 收口 | 一 |
| + | gate 直连模式通用路径 `client_message_processor.cpp:792` `rpcHandlerMeta.sender` | 任意客户端 gRPC 消息 | 同 #8 | 同 #8,调用点不改 | 一 |
| + | etcd 一元调用(`etcd_helper.cpp`) | Range / Put / Txn / LeaseGrant / LeaseRevoke | 见下 | **不装失败处理器**,保持"只打日志 + 既有 10s 看门狗 / 2s 周期重发";deadline 5000 < 看门狗 10000,反而消除了"旧调用的迟到应答被记到新 pending key 上"的错位 | 一(只 deadline) |

**#2 为什么回「服务器繁忙,请稍后再试」而不是「换图失败」**:传输失败时 scene_manager 可能已执行、路由事件随后到达。回 `kEnterSceneFailed` 会出现"先报失败、后被搬走"。但常见的失败形态是 scene_manager 整个不可达(UNAVAILABLE 立即返回),此时不回任何提示,客户端就会一直等一个不会来的 `EnterSceneS2C`(用户明确提出的缺陷)。取中:回措辞不断言结果的提示,并释放槽位允许重试。"先提示、后成功"只在 scene_manager 服务端超时(已调到 8s 盖住全部内部预算)或请求发出后断连时出现,属罕见路径。跨 zone 传送线建议过"只释放槽不提示",此处取舍记录在案,若需改为不提示,只动 `DispatchEnterSceneTransportFailure` 一行。

**用哪个码(2026-10-01 用户拍板:保留提示,换成更规范的文案)**:首版复用 `kServiceUnavailable`(「服务不可用」),有两个问题 —— 文案生硬、没告诉玩家该怎么办;而且它是通用码,客户端的换图界面不认识,会显示成"传送失败(tip=1003)"。改为在 `data/tip/Tip.xlsx` 的 scene_error 组新开 `EnterSceneServerBusy`(导表器发号 **3028**,fault=1),文案「服务器繁忙,请稍后再试」:
- 服务端三处用它:普通换图的传输失败(#2)、镜像 CreateScene 的传输失败(#6)、换图入口"一个 SceneManager 都没注册"(#3)。gate 的通用失败桥(#8 / #9)不是换图语义,仍回 `kServiceUnavailable`。
- 客户端(`../mmorpg-client`):`SceneErrorTip.cs` 重新生成;`GameClient.DescribeTravelTip` 增加这条文案。**不**加入 `IsTravelFailureTip`:那个判据的含义是"这次传送确定没成、玩家留在原地",用在跨区传送在途与踢线原因上;本码是结果未知,且交接(跨区 / 跨节点)的传输失败根本不发 tip。同区换图在途时界面收到任意 tip 都会结束等待并按码显示文案(`CityTravelUiRoot.HandleServerTip`),所以只补文案即可。
- 导表在隔离工作树里做:HEAD 上帮会有 10 行已提交未导表的 guild_error 码,导表前在工作树副本里去掉,只带回本码相关的 13 个产物(scene_error_tip 各语言产物、`tip_text.json`、`faults.go`、`segments.go`、`tip_enum_ids.json`)+ robot vendor 同名文件;guild 的码留给帮会整批导表(届时为 14032–14041,与本码无关)。

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
  scene 侧已提交 b85f13c07;09-29 补:换图入口"没有 SceneManager"改回 `kServiceUnavailable`(§5 #3);10-01 换图三处改用专用码 `kEnterSceneServerBusy`(3028,§5 "用哪个码")。
- **分工会话(「C++ 完全不连接 Go 服务的设计」)承担的部分**(09-29,未编译;大部分随 hourly save `ccfc40291` 进库,Codex 步骤见 PROGRESS.md「2026-09-29 C++↔Go 边界加固(V0+)分工部分」):
  - K8s:`GrpcClient` 块原样搬进 node / battle-node ConfigMap;写路径入口的部署门禁 `Assert-GrpcClientDeadlineBudget` 核对 SceneManager / DataService / 路由服 / Match / Login 五个目标「C++ deadline ≥ zrpc Timeout + 2000」,五项必须显式写成正整数,没写 Timeout 按 go-zero 默认 2000,出现 `MethodTimeouts` 即拒绝(2026-10-09 改为逐条核对,见 §4.4);login 的 ConfigMap 改为从 `login.yaml` 镜像 Timeout。本会话复核过口径:拍平函数会剥行尾注释,login 当时 102000 恰好等于 100000 + 2000,通过;09-29 改为 12000 / 10000 后同样恰好通过。
  - `GetSceneManagerEntity`(签名与调用点不变,规则抽成纯函数 `scene_manager_selector::Pick`,单测 `cpp/tests/routing_identity_test/scene_manager_selector_test.cpp`)分级挑选:① 通道 READY → ② IDLE / CONNECTING(`GetState(true)` 顺手触发连接)→ ③a 挂了通道但 TRANSIENT_FAILURE / SHUTDOWN → ③b 没挂通道的实体。③b 可以由 TCP 握手声明 `node_type = SceneManager` 造出来(`IsTcpNodeType` 含 SceneManagerNodeService),挑中后会在发送处断言,所以排最末,只为兼容旧行为与单测里的假节点。**只在注册表为空时返回 null**(与原来一致,不新增 null 窗口)。落到 ③ 时打 `[SceneManagerSelect]` LOG_WARN,每 10s 最多一行并带上被压掉的次数。取模前用 splitmix64 打散 playerId(bwmarrin PlayerId 低位多为 0,原来的 `% N` 在 2 / 4 个实例时几乎全落第 0 个)。SceneManager 全挂时换图因此走 UNAVAILABLE → 失败处理器 → tip 1003,与 §9.2 第 4 步的预期一致。
  - 删除无调用方的 `SendMessageToPlayerOnGrpcNode`(§10 第 4 条)。

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
   - 停掉 scene_manager 后在客户端发一次换图:10s 内收到 tip 3028(服务器繁忙,请稍后再试;10-01 前为 1003),再发一次不应被"切换中"(3014)挡住;
   - 路由模式下停掉路由服后发任意 gRPC 类消息:客户端收到 tip 1003,gate 日志有采样的 `gRPC client call failed (sampled)`。

## 10. 残留与后续

1. 流(etcd Watch / LeaseKeepAlive)的 `!ok` 分支:泄漏 tag、提前 return 跳过本轮其余事件、流不重建(KeepAlive 死后约每个 TTL 重注册一轮)。另立项。
2. etcd 一元调用若要接失败处理器:传输失败必须走与 `OnTxnTimeout` 等价的"取出 pending key 重发",**不能**走 `OnTxnFailed`(在 `kReRegisterExisting` 下会让节点因一次 etcd 抖动自杀),且要用请求里的 key 核对 `pendingTxnKey`。
3. 纯服务端流方法会被模板误生成成 unary(今天 proto 里没有此类方法)。
4. ~~`SendMessageToPlayerOnGrpcNode`(`player_message_utils.cpp:270-315`)无调用方,且发的是 `*requestProto` 而非入参。~~ 分工会话 09-29 删除(§8)。
5. ~~login Timeout 待拍板(§4.3)~~ 09-29 已改;CreateScene 错误应答回显 `creator_ids`(§7)。
6. 生成器自动注册(§6)。
