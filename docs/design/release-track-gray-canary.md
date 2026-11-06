# 发布轨道：灰度与金丝雀发布设计（2026-11-05）

> **状态**：方案已定。批 1（轨道身份与会话轨道）11-05 已落码，未编译，待 Codex 验证；批 2、批 3 待做，见 §7。负责人 11-05 要求「按你想法看看灰度和金丝雀对不对，没做完就按最标准的方式做：数据强一致、代码易懂、可水平扩展，Java 留待做」，方案由 Claude 选定。
> **需求来源**：[全类型节点平滑停机、水平扩展与灰度／金丝雀需求](node-lifecycle-elasticity-release-20261023.md) §2、§4.2、§5。该文把灰度粒度留给「后续设计」，本文把它定下来。
> **核对基线**：HEAD `993b6da9d` 加 11-05 工作区。当时有 12 个会话同时在改，行号会漂，按函数名找。
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
- 部署给 Pod 打标签 `mmorpg.io/release-track`，Prometheus 抓取时映射成 `release_track` 目标标签，不在每个指标里加 label。

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

全局服务（router，match 及同进程的 team，chat、trade、friend、player_inbox、guild、mail、rank，battle 池）的金丝雀是独立的一组副本，部署名 `<svc>-canary`。

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

- 不注册共享的 go-zero etcd key，部署时改成 `<key>.canary`。Go 的内部调用方用的是共享 key，永远连不到它。
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
| 1 开金丝雀 | 选定 zone：`zone-up -ReleaseTrack canary -NodeImage <新>`，gate 用排空脚本逐台换。要验证全局服务时，再部署对应的 `<svc>-canary` | 新版实例全部就绪；`NodeInfo.release_track` 与 Pod 标签一致 |
| 2 观察 | 按轨道和 zone 比较错误率、延迟、登录恢复、场景迁移、Kafka lag、存盘拒绝与死信、对局作废、结算完整性 | 观察窗口和阈值由负责人定。需求文档 §2 没给数值，本文不代填 |
| 3 扩大（灰度） | 对更多 zone 重复第 1 步 | 每批重复第 2 步 |
| 4 晋升 | 逐 zone `zone-up -ReleaseTrack stable -NodeImage <新>`，infra 的 stable 部署换成新 tag，再把全局 `<svc>-canary` 缩到 0 | 新版本成为 stable，金丝雀轨道清空 |
| 中止 | 先停新流量：全局 `<svc>-canary` 缩到 0，金丝雀会话自动退回 stable。zone 级回退：`zone-up -ReleaseTrack stable -NodeImage <旧>` | zone 回退受 [混版本持久字段保护](mixed-version-persist-field-protection.md) 的回退门禁约束，不满足就向前修复，不承诺一键任意回退 |

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

- `k8s_deploy.ps1 zone-up -ReleaseTrack stable|canary`，默认 stable：给 zone 内全部工作负载注入环境变量和 Pod 标签，namespace 打同一个标签；`zone-status` 打印每个工作负载的轨道和镜像。
- `infra-up` 为点名的全局服务生成 `<svc>-canary` 部署：独立副本数、`<key>.canary`、关掉全局后台工作的开关。
- 同一 zone 出现两条轨道并超过排空窗口时告警（批 3）。

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
| 2 全局服务的金丝雀副本与部署 | 逐个服务按作用域分清选主、消费、租约、清理和 go-zero key，金丝雀副本关掉全局作用域的部分；C++ 发现跳过 canary 轨道的全局类型节点（gate 选 router 除外）；scene_manager 的 `LeaderEligible` 改由轨道推出；`k8s_deploy.ps1` 的 `-ReleaseTrack`、`<svc>-canary` 生成与契约测试；`zone-status` 显示轨道 | 九个全局服务入口与装配、`go/scene_manager`、C++ 发现、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`deploy/k8s/README.md`，约 15 个 | 能部署金丝雀 zone 和全局金丝雀副本 |
| 3 战斗、观测与手册 | match 工单记录轨道、battle 按整局选轨道；混轨 zone 与金丝雀错误率告警；晋升与中止的运维手册 | `go/match/internal/*`、`deploy/k8s/*-alerts.yaml`、`docs/ops/` 新手册，约 10 个 | 闭环完整 |

**批 2 的前置条件**：批 1 只让经 `shared/noderegistry` 注册的 Go 服务、router 和全部 C++ 节点写出轨道。其余还在用各自注册副本的 Go 服务（guild、friend、player_locator、scene_manager、match、data_service 的 internal 副本，以及 login 自己的实现）写出的是空串，会被当成 stable。所以部署任何服务的金丝雀副本之前，必须先让它的注册路径写出轨道，否则金丝雀副本和 stable 分不清，stable 流量会漏进去；批 2 的部署脚本对没接好的服务要拒绝 `-ReleaseTrack canary`。

**与其他会话的冲突**：11-05 工作区里，login（`loginqueue`、`node`）、scene_manager（`world_init`、`createscenelogic`）、gate 的 `main.cpp`、九个全局服务的入口文件、`k8s_deploy.ps1`、`common.proto` 都有其他会话未提交的改动。批 1 只碰当时没人在改的文件，`common.proto` 只在 `NodeInfo` 末尾追加字段；批 2 的服务入口和部署脚本，要等那些改动入库后再动。

**验证分工**（AGENTS.md §10.1）：Claude 只写服务器代码、测试和文档；协议生成、编译、单测、冒烟由 Codex 执行，每批落码时给出命令清单。

## 8. Java 版

待做。Java 版（0.1.0-SNAPSHOT）用本版的 Netty、Dubbo 和部署形态实现同一套规则：两条轨道、按 zone 灰度、会话轨道跟随入口、全局金丝雀只接客户端请求、战斗按整局。已在 Java `PARITY.md` 登记待做。

## 9. 参考

- Kubernetes 的 [金丝雀部署](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#canary-deployments)：不同版本用不同的 Deployment 和标签区分。本文的 `<svc>-canary` 与 Pod 标签沿用这一做法，再补上业务归属、调用隔离和后台工作的作用域。
- 独立 Go 仓 `D:\luyuan\xuanming-server` 的 `pkg/releasetrack` 与 `docs/design/zero-downtime-update.md` §6.3 也用 stable／canary 两条轨道和「canary 没容量退回 stable、stable 不进 canary」的规则。那边按玩家哈希百分比分配 Hub DS，这里按 zone 和会话分配，原因见 §2.2；两仓代码不共用。
