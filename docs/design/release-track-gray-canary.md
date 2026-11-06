# 发布轨道：灰度与金丝雀发布设计（2026-11-05）

> **状态**：方案已定。批 1（轨道身份与会话轨道）、批 2（金丝雀 zone 与全局服务的后台开关）11-05 已落码；批 2b（全局服务的金丝雀副本）的 guild、match 代码 11-05 落码，`infra-up` 生成 `<svc>-canary` 与 match 的注册键校验 11-06 落码。以上都未编译，待 Codex 验证。批 3 待做，见 §7。负责人 11-05 要求「按你想法看看灰度和金丝雀对不对，没做完就按最标准的方式做：数据强一致、代码易懂、可水平扩展，Java 留待做」，方案由 Claude 选定。
> **需求来源**：[全类型节点平滑停机、水平扩展与灰度／金丝雀需求](node-lifecycle-elasticity-release-20261023.md) §2、§4.2、§5。该文把灰度粒度留给「后续设计」，本文把它定下来。
> **核对基线**：HEAD `993b6da9d` 加 11-05 工作区；批 2b 的 `infra-up` 部分按 HEAD `f6fddb466` 加 11-06 工作区。同时有十几个会话在改，行号会漂，按函数名找。
> **上线状态**：未上线。本文要求的新旧版本兼容是灰度本身的运行条件，不是给旧部署补兼容。

## 1. 现在的灰度和金丝雀对不对

10-23 需求文档对两个词的定义是对的：

- **灰度**：按稳定、说得清的业务单元，分批扩大新版本的覆盖面；
- **金丝雀**：第一批只放少量真实业务，指标过了门槛才扩大，出问题能暂停、能收回。

两者靠同一套机制：每个进程知道自己属于哪条发布轨道；分配点按业务单元把流量送到对应的轨道，一段有状态流程中途不换轨；按轨道观察，并且能单独停掉新版本的新流量。

代码里这三样都还没有。逐条核对源码的结果：

| # | 现状 | 后果 |
|---|---|---|
| 1 | `NodeInfo`（`proto/common/base/common.proto`）没有发布轨道字段，所有选点函数都分不清新旧实例 | 新版实例和稳定实例一起被选中，比例不受控 |
| 2 | 有状态层按负载选点：login 快路径 `PickGate` 随机抽两台取人少的，排队 dispatcher 取负载最少；scene_manager `GetBestNodeForPurpose` 取负载分最低的节点 | 新起的金丝雀实例是空的，每次比较都赢。快路径下它拿到约 2/N 的新连接而不是 1/N，排队路径会先把它填平；新建的场景和副本在负载分追平之前全落到它上面。所以对 gate、scene 来说，「加一个新版实例」不是受控的金丝雀 |
| 3 | scene_manager 的 `LeaderEligible=false` 只让副本不参选。数据面流量有两个来源：C++ scene 经 `GetSceneManagerEntity` 按 `SpreadPlayerId(playerId) % 实例数` 选，login 经 `zrpc.MustNewClient`（go-zero 的 etcd 解析加 p2c）选 | 前一半是「1/N 的玩家」，实例数一变就几乎全部换绑；后一半按延迟和在途数分，与玩家无关。合起来不是稳定的玩家分组，同一玩家的换图和登录可能落在不同版本上 |
| 4 | 全局服务之间的内部调用走共享的 go-zero etcd key（例如 guild 的 `MatchRpc` 连 `matchservice.rpc`）；C++ 节点按 `NodeInfo` 前缀发现全局服务；router 对全局类型 `PickRandom()` | 全局服务的金丝雀副本会收到稳定 zone 的内部调用和客户端请求，还会照常参加 Kafka 消费、租约和后台清理，影响面不受控 |
| 5 | 没有按轨道的观测，也没有固定的晋升和中止步骤；`ForwardRequest` 只带 `zone_id` | 新旧版本没法比较，也没法只停新版本的新流量 |

已有的基础：`k8s_deploy.ps1 zone-up -ZoneId <z> -NodeImage <ref>` 只换一个 zone 的工作负载（zone 内 Go 服务的 tag 跟随 NodeImage）；gate 是 `OnDelete`，有逐台排空脚本；Agones battle 有准入排空。这些是按 zone 换版本的执行手段，缺的是轨道身份、全局服务的分流和隔离、观察和中止的闭环。

## 2. 选定的方案

### 2.1 只有两条轨道

轨道只有 `stable` 和 `canary`。轨道是进程的部署属性：

- 由环境变量 `MMORPG_RELEASE_TRACK` 注入；不设等于 `stable`，其他值启动失败；
- 进程存续期间不变，写进 `NodeInfo.release_track`；
- 观测先靠 `NodeInfo.release_track`、C++ 的 `cpp_node_info{release_track}` 和启动日志；Pod 标签 `mmorpg.io/release-track` 及 Prometheus 目标标签映射留到批 3，和告警一起做。不在每个指标里加 label。

不做两条以上的轨道，也不做运行中可调的百分比权重。同一时刻在线的版本只有「当前稳定版」和「正在验证的版本」，兼容性检查只需要覆盖这一对。

### 2.2 zone 内的有状态层按 zone 灰度

zone 内的工作负载（gate、scene 各角色、login、scene_manager、go/db）一个 zone 只用一条轨道。金丝雀是第一批 zone，灰度是一批批扩大 zone，回退也按 zone。

同样按 zone 部署的 data_service 和 player_locator 例外：data_service 注册进全局池，player_locator 的死 gate 清扫按全服 slot 分段、对账 leader 是全服一把锁，两者的后台都是全局作用域。金丝雀 zone 里它们保持 stable（部署时不重新下发），理由见 §2.5。

选 zone、不选「zone 内按玩家比例」的理由：

1. zone 是完整的状态所有权域。玩家的 home zone、世界频道、go/db 分区、排队和会话都在 zone 内闭合，整 zone 换轨不会把一段有状态流程拆到两个版本上。
2. zone 内混轨有两种做法，都不可取。一种是直接加新版实例，会被 §1 第 2 条的负载选点放大到失控；另一种是让世界频道按轨道分组，同一张地图的玩家会被分成互相看不见的两组线，频道补建、扩缩容、均衡和选线面板都要按轨道重写，代价和风险都远大于收益。
3. 执行手段已经有了：`zone-up` 按 zone 换镜像，gate 逐台排空。

代价是最小粒度为一个 zone 的玩家。想让第一批更小，就拿一个人少的 zone 或专门的先锋 zone 当金丝雀。

### 2.3 会话的轨道等于它所在 gate 的轨道

gate 把自己的轨道写进每次发给 router 的 `ForwardRequest.release_track`。这和已有的 `zone_id` 一样，是 gate 自报的路由依据。

会话连在哪台 gate 上就属于哪条轨道，连接不断就不换轨。同一 zone 只有一条轨道，所以会话的轨道就是 zone 的轨道。zone 切换期间 gate 逐台排空，新旧 gate 短时并存，各自的会话走各自的轨道。

### 2.4 全局服务跟着会话的轨道走

接客户端请求的全局服务（router、chat、trade、friend、guild、mail、rank，以及 match 和同进程的 team）的金丝雀是独立的一组副本，部署名 `<svc>-canary`。player_inbox 在路由表里没有客户端路由，金丝雀副本只会被服务间调用误选，所以和 data_service 一样不提供金丝雀。battle 按整局，见 §2.6。

选点规则只有一条，Go 和 C++ 用同一份：

> 调用方是 canary：有 canary 实例就只在 canary 里选，没有就退回 stable。
> 调用方是 stable：只在 stable 里选，**永远不进 canary**。
> 节点没写轨道（空串）按 stable 算；写了无法识别的值，跳过该节点并记错误日志。

这条规则只用在客户端请求链路上，「调用方」就是会话：

- router 给全局类型选实例，会话轨道取 `ForwardRequest.release_track`；
- gate 给不分 zone 的类型选实例（router，以及直连模式下的全局业务服务），会话轨道就是 gate 自己的轨道。

zone 内类型不用这条规则：zone 只有一条轨道，按 zone 选就够了。服务间调用也不用它，只连 stable，见 §2.5。

**按发现作用域分类，不按部署 namespace。** zone 内类型以 `NodeUtils::IsZoneScopedNodeType` 为准：gate、scene、login、player_locator、go/db（`PlayerStoreNodeService`），以及 11-05 起按 zone 化的 scene_manager（[控制面按 zone 化](control-plane-zone-scoping-20261105.md) §3.1：选主锁 `scene_manager:leader:lock:zone:<z>`，scene 和 gate 只连本 zone 的实例）。data_service 虽然按 zone 部署，却注册进全局池（`bin/etc/base_deploy_config.yaml` 写着「全局池不分 zone」），别的 zone 也会发现它，所以按全局服务处理，见 §2.5。

### 2.5 全局作用域的后台工作只由 stable 承担

全局服务的金丝雀副本只接金丝雀会话经 router 来的客户端请求：

- 内部调用方连不到它。K8s 上的全局服务里只有 match 注册 go-zero 服务键（`matchservice.rpc`），guild 的 `MatchRpc` 按这个键发现它。go-zero 按「键 + `/`」前缀发现，所以 match 的金丝雀副本改登记在 `matchservice.rpc.canary` 下，guild 就发现不到它；canary 进程登记了 go-zero 键而键名不以 `.canary` 结尾时拒启（`go/match/match_service.go` 的 `validateReleaseTrackRegistration`），配错不会悄悄把内部调用送进新版本。其余全局服务不注册 go-zero 键（chat、friend、trade、player_inbox 的配置校验拒绝非空 `Etcd.Key`，mail、rank 由 `domainhost` 清空，guild 没有这一段），内部调用方走 K8s Service（`selector: {app: <svc>}`）或 `NodeInfo` 前缀。所以金丝雀副本的 Pod 不能带 `app: <svc>` 标签；前缀的消费方要按轨道过滤：router 和 C++ 在批 1、批 2 做了，match 的 `NodeWatcher` 在批 2b 加了 `StableOnly()`。
- 不参加任何全局作用域的单例选主、Kafka 消费组、后台租约和清理任务。

C++ 的服务间调用同样只连 stable：除 gate 以外的 C++ 节点（scene、battle）发现不分 zone 的类型时，canary 和轨道认不出的节点一律不收，在进发现快照之前就拦下，所以 `NodeConnector::ConnectAllNodes` 也连不到它们（批 2，`service_discovery_manager.cpp` 的 `AddServiceNode`）。gate 不受这条限制：它选 router 和直连模式下的业务服务，都是客户端请求链路，由 `PickRandomNode` 按 gate 自身轨道选。

同样的规则也适用于 zone 内的进程：**全局作用域的工作只由 stable 承担，zone 作用域的工作由该 zone 的进程承担。** 例子：

- scene_manager 的选主锁按 zone 分（`scene_manager:leader:lock:zone:<z>`），属于 zone 作用域，金丝雀 zone 的 scene_manager 照常参选，否则这个 zone 没有人编排；
- login 的排队出队 leader 按 zone 分，go/db 消费本 zone 的 topic，两者都属于 zone 作用域，金丝雀 zone 里照常跑；
- player_locator 的死 gate 清扫按全服 16384 个 slot 分段领租约，对账 leader 是全服一把锁 `player_locator:session_reconciler:leader`，属于全局作用域。

后台属于全局作用域、又只被服务间调用的服务（data_service、player_locator）不提供金丝雀：金丝雀 zone 里它们保持 stable，等金丝雀 zone 验证通过后随 stable 滚动升级，靠混版本兼容兜住。scene_manager 的 `LeaderEligible=false` 保留原义，只是一个「本副本不参选」的部署开关，与轨道无关。

这样金丝雀的影响面可以说清：金丝雀 zone 内 zone 作用域的全部状态，加上金丝雀会话发出的客户端请求。服务间调用和全局后台工作留在 stable，晋升时随 stable 一起换版本。

### 2.6 战斗按整局

battle 是全局池。一局战斗的成员全部来自 canary 会话时，放到 canary battle 节点；只要有一个成员来自 stable，就放 stable。match 成局时按工单上记录的轨道判断，工单经 router 进来时带着会话的轨道。

### 2.7 发布步骤

| 步骤 | 操作 | 通过条件 |
|---|---|---|
| 1 开金丝雀 | 选定 zone：`zone-up -ReleaseTrack canary -NodeImage <新>`，gate 用排空脚本逐台换。要验证全局服务时，再 `infra-up -CanaryServices <svc,…> -CanaryGoSvcTag <新>`，其余参数与线上 stable 相同（§5.3） | 新版实例全部就绪；各实例的 `NodeInfo.release_track` 与部署一致（金丝雀 zone 与 `<svc>-canary` 是 canary，其余是 stable） |
| 2 观察 | 按轨道和 zone 比较错误率、延迟、登录恢复、场景迁移、Kafka lag、存盘拒绝与死信、对局作废、结算完整性 | 观察窗口和阈值由负责人定。需求文档 §2 没给数值，本文不代填 |
| 3 扩大（灰度） | 对更多 zone 重复第 1 步 | 每批重复第 2 步 |
| 4 晋升 | 逐 zone `zone-up -ReleaseTrack stable -NodeImage <新>`；全部 zone 换完后跑一次不带 `-CanaryServices` 的 `infra-up -GoSvcTag <新>`：稳定版换成新 tag，随后删掉全部 `<svc>-canary` | 新版本成为 stable，金丝雀轨道清空 |
| 中止 | 先停新流量：跑一次不带 `-CanaryServices`（或只列要留下的服务）的 `infra-up`，其余参数照旧，删掉 `<svc>-canary`；来不及跑部署时直接删这个 Deployment。金丝雀会话随即退回 stable。zone 级回退：`zone-up -ReleaseTrack stable -NodeImage <旧>` | zone 回退受 [混版本持久字段保护](mixed-version-persist-field-protection.md) 的回退门禁约束，不满足就向前修复，不承诺一键任意回退 |

持久 schema 升世代的版本不能按 zone 金丝雀 go/db，按混版本文档的顺序：先 DDL，再 go/db 全部 zone，再 login，最后 scene。

## 3. 数据一致性

- 轨道只决定由哪个版本的二进制处理请求，不参与任何提交判定。提交点仍由各服务的事务、owner epoch、围栏和幂等保证，混轨不放宽一致性。前提是两个版本的在线协议和持久数据双向兼容，按混版本持久字段保护文档的规则执行。
- 不会出现「一段有状态流程跨两个版本」：zone 内状态整 zone 一条轨道；会话的轨道跟着 TCP 连接；全局服务的金丝雀只处理请求，状态在 SQL 权威；战斗整局一条轨道。
- 退回只有一个方向，canary 会话退到 stable。stable 会话永远不会被送进新版本。

## 4. 水平扩展

- 轨道和 zone 正交。stable 和 canary 各自按现有方式多副本、按 zone 扩容；选点只多一次按轨道过滤，开销与候选数成正比。
- 不引入新的中心组件，不加跨服务的状态存储：轨道是每个进程的部署属性，会话的轨道随请求带。

## 5. 契约

### 5.1 协议

| 字段 | 类型 | 语义 |
|---|---|---|
| `NodeInfo.release_track = 15` | string | 生产者只写 `"stable"` 或 `"canary"`。消费方把空串当 stable，其他值当无效节点跳过并记错误日志。13、14 号已被 `max_connections`、`admission` 占用 |
| `ForwardRequest.release_track = 3` | string | gate 自报的会话轨道，取值同上，与 `zone_id` 同形。空串按 stable；其他认不出的值是 gate 的契约违例，router 回 `kInvalidParameter` 并计 `outcome="invalid_release_track"` |
| match 工单的轨道 | 批 3 定 | §2.6 |

客户端协议不变，客户端不需要升级。

### 5.2 代码

- Go `go/shared/releasetrack`：`Track`（`uint8`，零值是 stable）、`Parse`（空串算 stable）、`FromEnv`（只认 `MMORPG_RELEASE_TRACK`）、`Select`（§2.4 的规则，泛型过滤候选）。只依赖标准库。`Track` 刻意不是 string：etcd 里读出的字符串不能直接转换成 `Track`，只能经 `Parse`，认不出的新取值就不会被当成 stable。
- C++ `cpp/libs/engine/core/node/system/node/release_track.h`：纯头文件，同名同义的 `Parse`、`Name`、`FromEnv`、`Select`。节点在 `Node::InitRpcServer` 里解析一次，非法就 `FatalStartupConfig` 拒启。
- Go 服务经 `shared/noderegistry` 注册时，轨道由该包统一写进 `releaseTrack`：`Register` 读一次环境变量，非法就拒注册；各服务的 `BuildValue` 不用管。这样 canary 进程不会因为漏填被当成 stable。
- 两边用同一组固定样例做单测对拍，做法同发号器的 `MixID` 与 `MixGuid`：`go/shared/releasetrack/releasetrack_test.go` 与 `cpp/tests/client_endpoint_test/release_track_test.cpp`。
- 观测：C++ 节点的 `cpp_node_info` 多一个 `release_track` 标签（只有两个取值）；router 的 `client_rpc_router_forward_total` 多一个 `outcome="invalid_release_track"`。

### 5.3 部署

- `k8s_deploy.ps1 zone-up -ReleaseTrack stable|canary`（批 2，已落码），默认 stable，区分大小写：
  - canary 给 gate（Deployment 与 external StatefulSet）、scene（Deployment 与 Agones Fleet）和 login、scene-manager、db 写 `MMORPG_RELEASE_TRACK=canary`；stable 一律不写，生成物与引入参数之前逐字相同；
  - 同在 zone namespace 的 data-service、player-locator 不重新下发，输出 `[keep-stable]`，线上 stable 那份照跑；前提是这个 zone 已按 stable 部署过；
  - infra-up、all-up 及其他命令带 canary 直接拒绝。
- `k8s_deploy.ps1 infra-up -CanaryServices <svc,…> -CanaryGoSvcTag <tag> [-CanaryReplicas N]`（批 2b，11-06 已落码，未编译）为点名的全局服务生成金丝雀副本：
  - 只 apply 一个 Deployment，名字 `<svc>-canary`；全部 `app: <svc>` 换成 `app: <svc>-canary`（metadata、selector、Pod 模板、反亲和），稳定版的 Service 选不中它，两个 Deployment 的 selector 也不重叠。Service、PDB、ConfigMap、迁移 Job 和共享 Secret 都归稳定版，不另起。镜像用 `-CanaryGoSvcTag`，staging、prod 同样要求固定 tag；副本数取 `-CanaryReplicas`，缺省 1；写 `MMORPG_RELEASE_TRACK=canary`。
  - 只收后台开关已接好的服务：router、chat、friend、trade、mail、rank、guild、match。只用于部署全局 Go 服务的 `infra-up`（带 `-SkipGoSvc` 或没给 `-GoSvcRegistry` 时拒绝）；服务重复、缺 tag 也拒绝。
  - 不粘滞：每次 `infra-up` 都按这次的 `-CanaryServices` 收敛，能起金丝雀却没列出的服务，删掉以前留下的 `<svc>-canary`。中止和晋升收尾都靠它。
  - 写 SQL 的服务（router 以外都是）在 RPC 里照样写库，boot 目录要另有 `<ns>/<svc>-canary` 条目：自己的注册计划、registrar 与运行配置 Secret 和名册，`replicas` 等于 `-CanaryReplicas`。两个 Deployment 共用一份名册会领到同一条 boot，所以不借稳定版那份。写路径入口先核（`Assert-GlobalGoSvcCanaryPreflight`），缺条目或副本数对不上就在任何写操作之前拒绝。match 的运行配置里 `Etcd.Key` 写 `matchservice.rpc.canary`（§2.5）。
  - 暂不能与 `-NetworkPolicies on` 同用，直接拒绝。准入名单按 `app` 标签放行，scene 那份名单在 `zone-up` 里生成，要让 `<svc>-canary` 进名单得同时改 zone 侧，留到批 3。
- Pod 标签 `mmorpg.io/release-track`、namespace 标签、`zone-status` 显示轨道，以及同一 zone 混轨超过排空窗口的告警，留到批 3。

## 6. 不做的事

- 不做 zone 内按玩家比例的金丝雀，理由见 §2.2。
- 不做动态权重，也不热切换百分比；扩大和中止都靠部署动作。
- 轨道不进 `persist_schema_gen`，也不进任何持久数据。
- 金丝雀副本不承担服务间调用和全局后台工作，见 §2.5。

## 7. 实施分批

AGENTS.md §10.2 规定预计改动 30 个以上文件要先停下请示，本方案合计约 45 个文件，所以分三批，每批不超过 30 个。

| 批 | 内容 | 主要文件 | 落码后的行为 |
|---|---|---|---|
| 1 轨道身份与会话轨道（11-05 已落码，未编译） | 两个 proto 字段；Go `releasetrack` 包及单测；`shared/noderegistry` 统一写轨道；router 自己的注册写轨道，watcher 解析轨道，`Forward` 按 §2.4 选全局实例；C++ `release_track.h` 及对拍单测；`node.cpp` 解析并注册轨道、`cpp_node_info` 加标签；C++ 发现对认不出的轨道记错误日志；gate 写 `ForwardRequest.release_track`，选不分 zone 的类型时按自身轨道过滤 | `proto/common/base/common.proto`、`proto/client_rpc_router/client_rpc_router.proto`、`go/shared/releasetrack/*`、`go/shared/noderegistry/registry{,_test}.go`、`go/client_rpc_router/{client_rpc_router_service.go,internal/noderegistry/registry.go,internal/discovery/*,internal/logic/*,internal/metrics/metrics.go}`、`cpp/libs/engine/core/node/system/node/{release_track.h,node.cpp}`、`cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp`、`cpp/libs/engine/core/core.vcxproj{,.filters}`、`cpp/nodes/gate/handler/rpc/client_session_handler.cpp`、`cpp/tests/client_endpoint_test/{release_track_test.cpp,client_endpoint_test.vcxproj}`，共 21 个代码文件 | 没部署金丝雀时全部是 stable，行为与现在相同 |
| 2 金丝雀 zone 与全局服务的后台开关（11-05 已落码，未编译） | 其余 Go 注册副本写出轨道（另一会话）；C++ 除 gate 外不收 canary 的全局类型节点；`releasetrack.RunsGlobalBackgroundWork`；friend、trade、mail、rank 的 canary 副本不跑全局后台（chat、router 没有这类后台，不用改）；`k8s_deploy.ps1 zone-up -ReleaseTrack` 与 4 条契约测试 | `go/{guild,player_locator,scene_manager,match,data_service,login}` 的注册副本与单测、`cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp`、`go/shared/releasetrack/*`、`go/friend/friend.go`、`go/trade/{trade.go,internal/svc/assetchannel.go,internal/svc/servicecontext.go}`、`go/mail/mail.go`、`go/rank/rank.go`、`tools/scripts/{k8s_deploy.ps1,lib/k8s_client_entry.ps1,tests/k8s_deploy_contract.tests.ps1}` | 能部署金丝雀 zone；全局服务还不能部署金丝雀副本 |
| 2b 全局服务的金丝雀副本（11-05 guild、match 代码已落码；11-06 `infra-up` 生成与 match 注册键校验已落码；都未编译） | guild 的后台开关；match 的发现镜像只收 stable、canary 后台开关、canary 注册键校验；`infra-up -CanaryServices` 生成 `<svc>-canary`（细节见下） | `go/guild/{guild.go,internal/svc/asset_op.go,internal/svc/asset_op_test.go}`、`go/match/{match_service.go,release_track_registration_test.go,internal/svc/*,internal/discovery/node_watcher{,_parse_test}.go,internal/logic/matcher.go}`、`tools/scripts/{k8s_deploy.ps1,tests/k8s_deploy_contract.tests.ps1,tests/k8s_migrate_gate.tests.ps1}`，共 14 个 | 不带 `-NetworkPolicies on` 时，能部署 router、chat、friend、trade、mail、rank、guild 的金丝雀副本；match 在过滤版 stable 上线后才能 |
| 3 战斗、观测与手册 | match 工单记录轨道、battle 按整局选轨道；`<svc>-canary` 进 NetworkPolicy 准入名单；混轨 zone 与金丝雀错误率告警；晋升与中止的运维手册 | `go/match/internal/*`、`tools/scripts/k8s_deploy.ps1`、`deploy/k8s/*-alerts.yaml`、`docs/ops/` 新手册，约 12 个 | 闭环完整 |

**注册路径已全部写出轨道（批 2，未编译）**：`shared/noderegistry` 覆盖 chat、db、friend、player_inbox、trade、mail、rank 和 match 里的 Team；router 用自己的副本（批 1）；guild、player_locator、scene_manager、match（MatchNodeService）、data_service、login 的六份副本由「灰度发布、金丝雀发布和水平扩展」会话补上，各带单测。以后新增任何注册路径都必须写出轨道：没写的服务会被当成 stable，它的金丝雀副本就会漏进 stable 流量。

**批 2b 的细节**（11-05 只读审计的结论）：

- **guild**：canary 副本要跳过 `RebuildRanks`（持维护锁整表重建榜）、资产 outbox 重投、维护选主 `guild:assetop:maintenance:leader`、唤醒订阅、终态清理、历练的两条后台循环和结果消费组 `guild-trial`；要保留 scene 节点镜像，捐献、兑换、活动奖励提交后的同步投递靠它定位 scene，不起的话金丝雀会话的同步投递全部失败。SQL 排空的 prepared 条件同时改成只在 stable 等结果消费者。不能靠关配置来关：关掉 `Activity.TrialResult.Enabled` 会让金丝雀会话的历练直接关闭。
- **match（含 team）**：只在进程内加开关不够。stable 的 `PoolMembers` 把所有 MatchNodeService 注册都算进 HRW 成员表，落到金丝雀 UUID 名下的匹配池，stable 既不撮合也不恢复，这些池会停摆。所以要先发一版 stable：`internal/discovery/node_watcher.go` 解析轨道（认不出就丢弃），`PoolMembers` 只算 stable，`PickRandom` 选 player_inbox 等也按「服务间调用只连 stable」过滤。等这版上线，再给 canary 加进程内开关：排名发布、终局发件箱、结果消费组 `match-rating`、team 的 5 个 SQL 维护循环、观战恢复、撮合循环、`SetPoolMember`；`GlobalGatherSlots` 要单独初始化，否则 RPC 里会同步跑最长 110 秒的备战。
- **11-05 已落码（未编译）**：
  - guild：`AssetPipeline.Start` 加 `globalBackground` 参数，canary 只起 scene 节点镜像；榜单重建、终态清理、历练后台、结果消费组只在 stable 起；排空 prepared 在 canary 上按无消费者发布。
  - match：`discovery.NodeWatcher` 解析轨道，`StableOnly()` 用在 battle、match、player_inbox 三个镜像上，canary 在解析时就丢掉，所以 `PoolMembers`、放战斗和找收件箱都只看 stable。`NewServiceContextWithAdmission` 加 `globalBackground` 参数，canary 不起排名发布与终局发件箱。入口里结果消费组、team 维护、观战恢复、撮合循环、`SetPoolMember` 只在 stable 起；canary 单独调 `InitGatherSlots`；排空 prepared 在 canary 上按无消费者发布。
- **发布顺序**：stable 一侧的保护（router 按会话轨道选、C++ 发现过滤、match 镜像只收 stable）必须先作为 stable 上线，之后的发布才能做金丝雀。因为金丝雀期间 stable 跑的是旧二进制，旧版本里没有的保护起不了作用。首次上线整批带上即可；以后再加这类保护，也要先随一次 stable 发布上线。
- **infra-up 生成 `<svc>-canary`**（11-06 已落码，未编译；参数与行为见 §5.3）：
  - 走稳定版同一条渲染路径 `Apply-OneGoSvc -Canary`，只在 Workload 阶段用，所以 SQL 写者 boot、控制面命令 topic、topic 副本数、内部签名与玩家域口令这些注入和稳定版逐项相同。改写分两步：读清单后立刻只留 Deployment、换副本数（`Select-GoSvcCanaryDeployment`，SQL 写者 boot 要拿副本数和 `<ns>/<svc>-canary` 条目核对）；全部注入完、apply 之前改名和换 `app` 标签（`Rename-GoSvcCanaryDeployment`）。容器名不改，写者注入按容器名找主容器。形状不对一律报错，不猜。
  - 起的时机跟稳定版：router 的进程在 zone 之前起，它的金丝雀副本也在 `Apply-GlobalGoSvcManifests` 里起；其余服务依赖 data_service，金丝雀副本在 `Apply-GlobalGoSvcWorkloads` 里、同批稳定版之后起。
  - `Add-SqlWriterBootManifest` 多了 `-CatalogSvcName`（缺省就是服务名），金丝雀副本传 `<svc>-canary`；`Add-GoSvcReleaseTrackEnv` 多了 `-Track`。写 SQL 的服务表收成一处 `$script:SqlWriterGoSvcs`，`Apply-OneGoSvc` 与预检共用。
  - NetworkPolicy 原计划同步名单，落码时改成拒绝同用，理由有两条：scene 的准入名单由 `zone-up` 生成，只改 `infra-up` 会让金丝雀副本连不上 scene 的 gRPC 口，资产与同步投递整片失败；guild、match 自己的准入策略按 `app: <svc>` 选 Pod，选不中金丝雀副本，它的 gRPC 口反而不设防。两边一起改放进批 3。
  - 顺带修了批 2 留下的测试缺口：`k8s_migrate_gate.tests.ps1` 按 AST 只抽函数跑 `Apply-OneGoSvc`，批 2 加的 `Add-GoSvcReleaseTrackEnv` 没恢复，这套用例会报「命令未找到」。现在夹具恢复真函数、按 stable 跑，并照抄 `$script:SqlWriterGoSvcs`。

**与其他会话的冲突**：11-05 工作区里，login（`loginqueue`、`node`）、scene_manager（`world_init`、`createscenelogic`）、gate 的 `main.cpp`、九个全局服务的入口文件、`k8s_deploy.ps1`、`common.proto` 都有其他会话未提交的改动。批 1 只碰当时没人在改的文件，`common.proto` 只在 `NodeInfo` 末尾追加字段；批 2 的服务入口和部署脚本，要等那些改动入库后再动。

**验证分工**（AGENTS.md §10.1）：Claude 只写服务器代码、测试和文档；协议生成、编译、单测、冒烟由 Codex 执行，每批落码时给出命令清单。

## 8. Java 版

待做。Java 版（0.1.0-SNAPSHOT）用本版的 Netty、Dubbo 和部署形态实现同一套规则：两条轨道、按 zone 灰度、会话轨道跟随入口、全局金丝雀只接客户端请求、战斗按整局。已在 Java `PARITY.md` 登记待做。

## 9. 参考

- Kubernetes 的 [金丝雀部署](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#canary-deployments)：不同版本用不同的 Deployment 和标签区分。本文的 `<svc>-canary` 与 Pod 标签沿用这一做法，再补上业务归属、调用隔离和后台工作的作用域。
- 独立 Go 仓 `D:\luyuan\xuanming-server` 的 `pkg/releasetrack` 与 `docs/design/zero-downtime-update.md` §6.3 也用 stable／canary 两条轨道和「canary 没容量退回 stable、stable 不进 canary」的规则。那边按玩家哈希百分比分配 Hub DS，这里按 zone 和会话分配，原因见 §2.2；两仓代码不共用。
