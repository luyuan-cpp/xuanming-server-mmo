# 发布轨道：灰度与金丝雀发布设计（2026-11-05）

> **状态**：方案已定。批 1（轨道身份与会话轨道）、批 2（金丝雀 zone 与全局服务的后台开关）11-05 已落码；批 2b（全局服务的金丝雀副本）的 guild、match 代码 11-05 落码，`infra-up` 生成 `<svc>-canary` 与 match 的注册键校验 11-06 落码；批 3a（准入名单带金丝雀、金丝雀 Pod 标签、按轨道的转发计数、告警、[操作手册](../ops/release-track-canary-runbook.md)）11-06 落码；批 3b（战斗按整局、金丝雀 battle 池）11-10 落码，同日修完它的四条残余（观战按成局原件记的轨道判、随机观战与观战列表按观众轨道过滤、补观战用例、切换编排形态时一起删金丝雀 battle 池）。服务器代码已按本文全部落码，分层验证结果见下行与 §7。下行验证之后，11-11 又按通行做法定了金丝雀判定（§2.8），路由服补了按会话轨道的耗时直方图与延迟告警，这部分未编译。负责人 11-05 要求「按你想法看看灰度和金丝雀对不对，没做完就按最标准的方式做：数据强一致、代码易懂、可水平扩展，Java 留待做」，方案由 Claude 选定。
> **验证（2026-11-11）**：no-unity 协议生成及后处理完成；Go 核心指定用例、六个注册包的 11 条轨道用例、C++ `client_endpoint_test` 79 条（含 9 条发布轨道用例）和告警契约 5 条通过。Windows 整解曾退出 0；随后最新复核被并发新增邮件 outbox 组件缺少生成头阻塞，不能据此宣称最新整解通过。trade 两条 SQL 序列测试及完整部署契约仍未全绿，部分长套执行期间输入发生变化；真库门控用例、集群冒烟、Linux 与 promtool 未验证。完整命令、快照范围与失败原件见 `docs/PROGRESS.md` 2026-11-11「发布轨道五批验证」条目。
> **需求来源**：[全类型节点平滑停机、水平扩展与灰度／金丝雀需求](node-lifecycle-elasticity-release-20261023.md) §2、§4.2、§5。该文把灰度粒度留给「后续设计」，本文把它定下来。
> **核对基线**：HEAD `993b6da9d` 加 11-05 工作区；批 2b 的 `infra-up` 部分按 HEAD `f6fddb466` 加 11-06 工作区。同时有十几个会话在改，行号会漂，按函数名找。
> **上线状态**：未上线。本文要求的新旧版本兼容是灰度本身的运行条件，不是给旧部署补兼容。

## 1. 现在的灰度和金丝雀对不对

10-23 需求文档对两个词的定义是对的：

- **灰度**：按稳定、说得清的业务单元，分批扩大新版本的覆盖面；
- **金丝雀**：第一批只放少量真实业务，指标过了门槛才扩大，出问题能暂停、能收回。

两者靠同一套机制：每个进程知道自己属于哪条发布轨道；分配点按业务单元把流量送到对应的轨道，一段有状态流程中途不换轨；按轨道观察，并且能单独停掉新版本的新流量。

11-05 核对时代码里这三样都还没有，之后按 §7 分批补齐。当时逐条核对源码的结果：

| # | 现状 | 后果 |
|---|---|---|
| 1 | `NodeInfo`（`proto/common/base/common.proto`）没有发布轨道字段，所有选点函数都分不清新旧实例 | 新版实例和稳定实例一起被选中，比例不受控 |
| 2 | 有状态层按负载选点：login 快路径 `PickGate` 随机抽两台取人少的，排队 dispatcher 取负载最少；scene_manager `GetBestNodeForPurpose` 取负载分最低的节点 | 新起的金丝雀实例是空的，每次比较都赢。快路径下它拿到约 2/N 的新连接而不是 1/N，排队路径会先把它填平；新建的场景和副本在负载分追平之前全落到它上面。所以对 gate、scene 来说，「加一个新版实例」不是受控的金丝雀 |
| 3 | scene_manager 的 `LeaderEligible=false` 只让副本不参选。数据面流量有两个来源：C++ scene 经 `SelectSceneManagerEntity` 按 `SpreadPlayerId(playerId) % 实例数` 选，login 经 `zrpc.MustNewClient`（go-zero 的 etcd 解析加 p2c）选 | 前一半是「1/N 的玩家」，实例数一变就几乎全部换绑；后一半按延迟和在途数分，与玩家无关。合起来不是稳定的玩家分组，同一玩家的换图和登录可能落在不同版本上 |
| 4 | 全局服务之间的内部调用走共享的 go-zero etcd key（例如 guild 的 `MatchRpc` 连 `matchservice.rpc`）；C++ 节点按 `NodeInfo` 前缀发现全局服务；router 对全局类型 `PickRandom()` | 全局服务的金丝雀副本会收到稳定 zone 的内部调用和客户端请求，还会照常参加 Kafka 消费、租约和后台清理，影响面不受控 |
| 5 | 没有按轨道的观测，也没有固定的晋升和中止步骤；`ForwardRequest` 只带 `zone_id` | 新旧版本没法比较，也没法只停新版本的新流量 |

已有的基础：`k8s_deploy.ps1 zone-up -ZoneId <z> -NodeImage <ref>` 只换一个 zone 的工作负载（zone 内 Go 服务的 tag 跟随 NodeImage）；gate 是 `OnDelete`，有逐台排空脚本；Agones battle 有准入排空。这些是按 zone 换版本的执行手段，缺的是轨道身份、全局服务的分流和隔离、观察和中止的闭环。

## 2. 选定的方案

### 2.1 只有两条轨道

轨道只有 `stable` 和 `canary`。轨道是进程的部署属性：

- 由环境变量 `MMORPG_RELEASE_TRACK` 注入；不设等于 `stable`，其他值启动失败；
- 进程存续期间不变，写进 `NodeInfo.release_track`；
- 观测靠 `NodeInfo.release_track`、C++ 的 `cpp_node_info{release_track}`、路由服按会话轨道分的转发计数 `client_rpc_router_forward_by_track_total`，以及金丝雀 Pod 上的标签 `mmorpg.io/release-track: canary`（批 3a）。Pod 标签和 env 同一判据，stable 不写；要按轨道比较 Go 服务自己的指标时，由 Prometheus 把这个标签映射成目标标签，缺标签的算 stable（写法见[操作手册](../ops/release-track-canary-runbook.md)第 1 节）。不在每个指标里加 label。

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

battle 是全局池。一局战斗的成员全部来自 canary 会话时，放到 canary battle 节点；只要有一个成员来自 stable，就放 stable。选定的机房里没有可用的 canary 节点时退回 stable，和 §2.4 的退回方向一致。

成员的轨道记在 match 的玩家网络档案（`MatchNetworkProfile`）上，不记在排队票上：成局的每条路径（排队成局、切磋、活动、整队直接开战）本来就要读每个成员的档案，档案由玩家自己的会话经 router 写入、带有效期，换 zone 之后下一次探测就会更新。票只属于发起请求的那个人，整队直接开战时其余队员的轨道从票上拿不到。排队票入队时复制一份档案（等待中随回执刷新），所以排队成局读票上的那份。

机房始终按 stable battle 节点选：探测目标、可放置的机房集合都只看 stable 镜像，canary 局在选定的机房里先找 canary 节点。所以 canary battle 池要和 stable 部署在同一批机房（`infra-up` 起的金丝雀池与 stable 用同一个机房号），不会出现「只有 canary 节点的机房」。

观战：stable 会话不看 canary 对局，否则会被连到 canary battle 节点上；canary 会话两条轨道都能看。一局在哪条轨道由成局原件记下（选点时按选中节点写，之后不变），点名观战、随机观战和观战列表（含赛事局）都按它判，不看实时节点镜像。做法见 §7「批 3b 的细节」（11-10 已落码，未编译）。

### 2.7 发布步骤

| 步骤 | 操作 | 通过条件 |
|---|---|---|
| 1 开金丝雀 | 选定 zone：`zone-up -ReleaseTrack canary -NodeImage <新>`，gate 用排空脚本逐台换。要验证全局服务时，再 `infra-up -CanaryServices <svc,…> -CanaryGoSvcTag <新>`；要验证战斗时加 `-CanaryBattleReplicas <N> -CanaryNodeImage <新>`。其余参数与线上 stable 相同（§5.3） | 新版实例全部就绪；各实例的 `NodeInfo.release_track` 与部署一致（金丝雀 zone、`<svc>-canary`、`battle-canary` 是 canary，其余是 stable） |
| 2 观察 | 按轨道和 zone 比较错误率、延迟、登录恢复、场景迁移、Kafka lag、存盘拒绝与死信、对局作废、结算完整性 | 按 §2.8 判定：窗口与流量下限满足、五条判据全部通过；不通过按「中止」处理 |
| 3 扩大（灰度） | 按 §2.8 的批次对更多 zone 重复第 1 步：第 2 批 10%、第 3 批 50% | 每批按 §2.8 重新判定，上一批通过才开下一批 |
| 4 晋升 | 第 3 批判定通过后，逐 zone `zone-up -ReleaseTrack stable -NodeImage <新>`；全部 zone 换完后跑一次不带 `-CanaryServices`、`-CanaryBattleReplicas` 的 `infra-up -GoSvcTag <新> -NodeImage <新>`：稳定版（含 battle）换成新版本，随后删掉全部 `<svc>-canary` 与 `battle-canary` | 新版本成为 stable，金丝雀轨道清空 |
| 中止 | 先停新流量：跑一次不带 `-CanaryServices`（或只列要留下的服务）、不带 `-CanaryBattleReplicas` 的 `infra-up`，其余参数照旧，删掉 `<svc>-canary` 与 `battle-canary`；来不及跑部署时直接删这几个工作负载。金丝雀会话随即退回 stable，还没开局的金丝雀局改放 stable battle；`battle-canary` 上正在打的局随 Pod 结束，能等就等这些局打完再删。zone 级回退：`zone-up -ReleaseTrack stable -NodeImage <旧>` | zone 回退受 [混版本持久字段保护](mixed-version-persist-field-protection.md) 的回退门禁约束，不满足就向前修复，不承诺一键任意回退 |

持久 schema 升世代的版本不能按 zone 金丝雀 go/db，按混版本文档 §8.2 的顺序：先对每个 zone 跑 `zone-up -ZoneId <z> -NodeImage <线上版本的节点镜像> -GoSvcTag <新>`，把全部 zone 的 Go 服务（含 go/db）升上去、C++ 节点不动；全部滚完之后再按上表第 1 步开金丝雀 zone、逐批换 scene。加字段不改表，没有 DDL 这一步；login 不读玩家存档，不参与。

### 2.8 金丝雀判定

需求文档 §2 没给观察窗口和门槛的数。负责人 11-11 要求不等压测、先按通行做法定，这里按金丝雀分析的通行结构定下：

- **只和同一时段的 stable 比**，不和历史比。流量起伏、外部依赖抖动这类与版本无关的变化，两条轨道同时受影响，比较时自然抵消。
- **每项判据都是「相对倍数 + 绝对下限 + 最低样本量」三件套**：只看倍数，两边都接近 0 时一次偶发就翻倍；只看绝对值，又和流量大小绑死；样本太少时任何比较都是噪声。
- **一项不过就判不通过**，按 §2.7「中止」处理；修好后发新版本，从第 1 批重来，窗口重新计。
- **能机械化的判据做成告警**（`deploy/k8s/release-track-alerts.yaml`），窗口内响过一次就算不过，不靠人盯曲线。

**批次与窗口**。最后一批停在 50%：再往上 stable 只剩很少几个 zone，同时段的基线就不可信了；全量是第 4 步「晋升」，不是金丝雀。

| 批次 | 金丝雀覆盖 | 最短观察窗口 |
|---|---|---|
| 第 1 批 | 1 个 zone，选在线人数最少、但不是空服的那个；要验证全局服务与战斗时，同批开 `<svc>-canary` 与 `battle-canary` | 24 小时，并且包含一次晚高峰和一次跨天（按天刷新的逻辑只在跨天那一刻跑） |
| 第 2 批 | zone 总数的 10%，向上取整，且比上一批至少多 1 个 | 2 小时 |
| 第 3 批 | zone 总数的 50%，向上取整 | 2 小时，并且包含一次晚高峰（一半玩家在新版本上，容量问题只在满载时出现） |

zone 少于 2 个时没有同时段的 stable 可比，不能按本节做金丝雀，只能整体发布、按普通告警观察。

**流量下限**。达不到的时段不计入窗口，窗口顺延：

- 金丝雀会话经路由服的请求，每秒至少 1 次，和耗时判据的最低样本量相同；
- 开了 `battle-canary` 时，窗口内至少 300 局在金丝雀 battle 上打完（终局判定为 COMPLETE 或 ABORT）。300 局时 1 个百分点是 3 局，与下面作废率的绝对下限对得上。

**判据**（全部满足才通过）：

| # | 判据 | 门槛 | 怎么判 |
|---|---|---|---|
| 1 | 全局服务请求失败率 | canary 会话 ≤ stable × 2 + 1 个百分点；canary 每秒 0.1 次以上才比 | 告警 `ReleaseTrackCanaryForwardErrorsHigh`，连续 10 分钟超过就响 |
| 2 | 全局服务请求耗时 p99 | canary 会话 ≤ max(stable × 1.5, stable + 50 毫秒)；canary 每秒 1 次以上才比 | 告警 `ReleaseTrackCanaryForwardLatencyHigh`，同上 |
| 3 | 其余告警 | 金丝雀 zone 的 namespace、`<svc>-canary`、`battle-canary` 涉及的规则在窗口内没有响过 | 查 Prometheus 的 `ALERTS`。同一时段 stable 也在响的是共同原因：先暂停扩大，查清再判 |
| 4 | 进程稳定 | 金丝雀 Pod 没有非计划重启 | `kubectl get pods -A -l mmorpg.io/release-track=canary` 的 RESTARTS 全是 0；部署动作换掉的 Pod 是新 Pod，不算 |
| 5 | 对局作废率 | canary 局 ≤ stable 局 × 2 + 1 个百分点 | 按成局原件记的轨道（`match_formation.release_track`）连终局表 `match_battle_completion` 统计 ABORT 占比，SQL 在手册里 |

**数据完整性零容忍**：存盘被拒（`ScenePlayerStoreSaveRefused`）、个人结算积压等数据类告警，本身就是「出现一次就响」的规则，归在判据 3 里；在金丝雀上响而 stable 同期没响，直接判不通过，不看倍数。

**门槛为什么是这几个数**：失败率与作废率用「2 倍 + 1 个百分点」，失败是离散事件，基线常在 1% 以下，翻倍才区分得出版本问题与抖动，1 个百分点挡住两边都接近 0 时的偶发。耗时用 1.5 倍：耗时退化多是渐进的（多一次查询、多一段锁等待），等到翻倍才响太迟。50 毫秒约是路由服耗时分桶 0.05～0.1 秒的一格，差距小于一格多半是插值与抖动。窗口长度见上表：第 1 批要覆盖一个完整的日周期，后面几批验证的是规模，2 小时够出现容量与连接数问题。这些数都写在告警规则的 description 里，改动要同时改本节、规则与契约测试。

**判定记录**：每批结束时把批次、zone 列表、窗口起止、流量、五条判据的值与结论写进当次发布单，不通过时写明按哪一条中止。

## 3. 数据一致性

- 轨道只决定由哪个版本的二进制处理请求，不参与任何提交判定。提交点仍由各服务的事务、owner epoch、围栏和幂等保证，混轨不放宽一致性。前提是两个版本的在线协议和持久数据双向兼容，按混版本持久字段保护文档的规则执行。
- 不会出现「一段有状态流程跨两个版本」：zone 内状态整 zone 一条轨道；会话的轨道跟着 TCP 连接；全局服务的金丝雀只处理请求，状态在 SQL 权威；战斗整局一条轨道。
  - 跨 zone 传送是例外：玩家从金丝雀 zone 去 stable zone 时，玩家存档要被另一个版本的 scene 认领。
    - 金丝雀版本升过存档世代时，stable scene 照常认领在金丝雀版本上存过盘的存档：不认识的内容原样带回，存盘世代取 max(自己的, 加载到的)，金丝雀版本才有的功能在 stable 上不可用（混版本设计 §4.2、§5.2，2026-11-12）。金丝雀 zone 回退 scene 之后也一样。
    - 前提是全部 zone 的 go/db 已经认识这个世代：玩家会带着新世代的存档跨到别的 zone。所以升存档世代的版本先把全部 zone 的 go/db 升上去，scene 再按 zone 金丝雀（混版本设计 §8.2）。顺序错了，认领回 `SCHEMA_TOO_NEW`、玩家进场失败，不丢数据。
    - 2026-11-10 到 11-12 之间，stable scene 的认领会因为「存档世代比 scene 新」被 go/db 挡住（`decideClaim`），所以那时写的是「升存档世代的版本不按 zone 金丝雀」。
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
| `MatchNetworkProfile.release_track = 6`（批 3b） | string | 写探测计划时由服务端按会话轨道记下（取值同上，空串是本字段之前的档案，按 stable），成局时用来判这局的轨道（§2.6）。存在档案的 payload 里，不改表结构 |
| `MatchFormationRecord.release_track = 23`（批 3b） | string | 这局所在 battle 节点的轨道，选点时按选中节点记下（canary 局退回 stable 节点时记 stable），之后不变。观战的三个入口按它判。`match_formation` 按列存，迁移补列；补列前的行读出 NULL，按 stable；认不出的值观战不放行 |
| `SpectateBattleRecord.release_track = 7`（批 3b） | string | 由成局原件组观战记录时照抄，观战列表按它过滤。服务端内部消息，不下发客户端 |
| gRPC metadata `x-release-track`（批 3b） | string | 客户端请求链路上把会话轨道带给目标服务：router 按 `ForwardRequest.release_track` 写，gate 直连写自己的轨道。读不到按 stable，写了多份或认不出就拒绝 |

客户端协议不变，客户端不需要升级。

### 5.2 代码

- Go `go/shared/releasetrack`：`Track`（`uint8`，零值是 stable）、`Parse`（空串算 stable）、`FromEnv`（只认 `MMORPG_RELEASE_TRACK`）、`Select`（§2.4 的规则，泛型过滤候选）。只依赖标准库。`Track` 刻意不是 string：etcd 里读出的字符串不能直接转换成 `Track`，只能经 `Parse`，认不出的新取值就不会被当成 stable。
- C++ `cpp/libs/engine/core/node/system/node/release_track.h`：纯头文件，同名同义的 `Parse`、`Name`、`FromEnv`、`Select`。节点在 `Node::InitRpcServer` 里解析一次，非法就 `FatalStartupConfig` 拒启。
- Go 服务经 `shared/noderegistry` 注册时，轨道由该包统一写进 `releaseTrack`：`Register` 读一次环境变量，非法就拒注册；各服务的 `BuildValue` 不用管。这样 canary 进程不会因为漏填被当成 stable。
- 两边用同一组固定样例做单测对拍，做法同发号器的 `MixID` 与 `MixGuid`：`go/shared/releasetrack/releasetrack_test.go` 与 `cpp/tests/client_endpoint_test/release_track_test.cpp`。
- 观测：C++ 节点的 `cpp_node_info` 多一个 `release_track` 标签（只有两个取值）；router 的 `client_rpc_router_forward_total` 多一个 `outcome="invalid_release_track"`。批 3a 起 router 另有 `client_rpc_router_forward_by_track_total{release_track, outcome}`：同一批终态按 gate 自报的会话轨道再记一份，认不出的轨道记 `unknown`，不带 message_id，基数固定。会话轨道在 `Forward` 开头只解析一次，所以早于轨道检查的拒绝（如未知消息号）也记在会话自己的轨道上，两条轨道的失败率可以直接比。11-11 起另有 `client_rpc_router_forward_by_track_seconds{release_track}`：同一段耗时按会话轨道再记一份，分桶与 `forward_seconds` 共用，两条轨道的 p99 可以直接比（§2.8 判据 2）。

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
  - `-NetworkPolicies on` 时照样能用（批 3a）：生成 NetworkPolicy 时，两侧名单里能起金丝雀的服务各自动补一个 `<app>-canary`（`Get-ReleaseTrackAppLabels`），guild、match 的策略同时保护它们的金丝雀副本，放行一侧的金丝雀调用方照样能拨。名单表只写真实清单的标签，补上的标签不看这次起没起金丝雀，没有金丝雀时选不中任何 Pod。scene 的名单在 `zone-up` 时生成，各 zone 要先用同一版脚本部署过。
- 金丝雀 Pod 带标签 `mmorpg.io/release-track: canary`（批 3a）：zone-up 的 gate、scene（Deployment、Agones Fleet 的 GameServer 与 Pod 两层、external gate StatefulSet）和 login、scene-manager、db，以及全局服务的 `<svc>-canary`。只加在 Pod 模板，selector 不动；stable 不写，已有 zone 不会因此滚动。`zone-status`、`infra-status` 最后列出各 Pod 的轨道标签。
- `k8s_deploy.ps1 infra-up -CanaryBattleReplicas <N> -CanaryNodeImage <ref>`（批 3b，11-10 已落码，未编译）另起 `battle-canary` 池：Deployment 形态是 `battle-canary` Deployment，Agones 形态是第二个 Fleet；镜像、拉取策略、副本数是金丝雀的，写 `MMORPG_RELEASE_TRACK=canary` 与 Pod 标签，`app: battle-canary`，其余和 stable battle 相同。只用于 infra-up；要求 stable 池在；不支持 hostPort 形态；staging、prod 同样要求固定 tag。不粘滞：没起时删掉以前留下的 `battle-canary`，只删本次编排形态的那种 kind。切换 `-BattleOrchestrator` 时，另一种 kind 的 `battle-canary` 和 stable battle 一起列为旧形态资源，过同一道 `-AllowDisruptiveSwitch` 闸删除，不用先手动撤池。
- 告警 `deploy/k8s/release-track-alerts.yaml`（批 3a，11-11 补第三条）：`ReleaseTrackZoneMixedTooLong`（一个 zone 的 gate / scene 混轨超过 3 小时，按 `cpp_node_info`）、`ReleaseTrackCanaryForwardErrorsHigh`（canary 会话经 router 的失败率超过 stable 的 2 倍再加 1 个百分点，canary 每秒 0.1 次请求以上，持续 10 分钟）、`ReleaseTrackCanaryForwardLatencyHigh`（canary 会话经 router 的 p99 超过 stable 的 1.5 倍且至少慢 50 毫秒，canary 每秒 1 次请求以上，持续 10 分钟）。门槛按 §2.8 定，依据写在规则里。不做 namespace 标签：金丝雀 zone 里 data-service、player-locator 仍是 stable，按 namespace 打标签会标错。

## 6. 不做的事

- 不做 zone 内按玩家比例的金丝雀，理由见 §2.2。
- 不做动态权重，也不热切换百分比；扩大和中止都靠部署动作。
- 轨道不进玩家存档（`persist_schema_gen`），玩家数据里没有轨道。唯一落库的是成局原件上「这局放在哪条轨道的 battle 上」（§5.1 `MatchFormationRecord.release_track`）：它是这局的放置事实，随成局写入、不再修改，观战与对局作废率统计（§2.8 判据 5）按它判。
- 金丝雀副本不承担服务间调用和全局后台工作，见 §2.5。

## 7. 实施分批

AGENTS.md §10.2 规定预计改动 30 个以上文件要先停下请示，本方案合计 70 个左右文件，所以分批，每批不超过 30 个。批 3 原估约 12 个文件，11-06 核对代码后约 35 个，拆成 3a（部署与观测）和 3b（战斗按整局）。

| 批 | 内容 | 主要文件 | 落码后的行为 |
|---|---|---|---|
| 1 轨道身份与会话轨道（11-05 已落码，未编译） | 两个 proto 字段；Go `releasetrack` 包及单测；`shared/noderegistry` 统一写轨道；router 自己的注册写轨道，watcher 解析轨道，`Forward` 按 §2.4 选全局实例；C++ `release_track.h` 及对拍单测；`node.cpp` 解析并注册轨道、`cpp_node_info` 加标签；C++ 发现对认不出的轨道记错误日志；gate 写 `ForwardRequest.release_track`，选不分 zone 的类型时按自身轨道过滤 | `proto/common/base/common.proto`、`proto/client_rpc_router/client_rpc_router.proto`、`go/shared/releasetrack/*`、`go/shared/noderegistry/registry{,_test}.go`、`go/client_rpc_router/{client_rpc_router_service.go,internal/noderegistry/registry.go,internal/discovery/*,internal/logic/*,internal/metrics/metrics.go}`、`cpp/libs/engine/core/node/system/node/{release_track.h,node.cpp}`、`cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp`、`cpp/libs/engine/core/core.vcxproj{,.filters}`、`cpp/nodes/gate/handler/rpc/client_session_handler.cpp`、`cpp/tests/client_endpoint_test/{release_track_test.cpp,client_endpoint_test.vcxproj}`，共 21 个代码文件 | 没部署金丝雀时全部是 stable，行为与现在相同 |
| 2 金丝雀 zone 与全局服务的后台开关（11-05 已落码，未编译） | 其余 Go 注册副本写出轨道（另一会话）；C++ 除 gate 外不收 canary 的全局类型节点；`releasetrack.RunsGlobalBackgroundWork`；friend、trade、mail、rank 的 canary 副本不跑全局后台（chat、router 没有这类后台，不用改）；`k8s_deploy.ps1 zone-up -ReleaseTrack` 与 4 条契约测试 | `go/{guild,player_locator,scene_manager,match,data_service,login}` 的注册副本与单测、`cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp`、`go/shared/releasetrack/*`、`go/friend/friend.go`、`go/trade/{trade.go,internal/svc/assetchannel.go,internal/svc/servicecontext.go}`、`go/mail/mail.go`、`go/rank/rank.go`、`tools/scripts/{k8s_deploy.ps1,lib/k8s_client_entry.ps1,tests/k8s_deploy_contract.tests.ps1}` | 能部署金丝雀 zone；全局服务还不能部署金丝雀副本 |
| 2b 全局服务的金丝雀副本（11-05 guild、match 代码已落码；11-06 `infra-up` 生成与 match 注册键校验已落码；都未编译） | guild 的后台开关；match 的发现镜像只收 stable、canary 后台开关、canary 注册键校验；`infra-up -CanaryServices` 生成 `<svc>-canary`（细节见下） | `go/guild/{guild.go,internal/svc/asset_op.go,internal/svc/asset_op_test.go}`、`go/match/{match_service.go,release_track_registration_test.go,internal/svc/*,internal/discovery/node_watcher{,_parse_test}.go,internal/logic/matcher.go}`、`tools/scripts/{k8s_deploy.ps1,tests/k8s_deploy_contract.tests.ps1,tests/k8s_migrate_gate.tests.ps1}`，共 14 个 | 能部署 router、chat、friend、trade、mail、rank、guild 的金丝雀副本（3a 起开着 `-NetworkPolicies on` 也行）；match 在过滤版 stable 上线后才能 |
| 3a 部署与观测（11-06 已落码，未编译） | `<svc>-canary` 进 NetworkPolicy 准入名单；金丝雀 Pod 标签与 status 显示；router 按会话轨道的转发计数；混轨 zone 与金丝雀失败率告警；晋升与中止的操作手册（细节见下） | `tools/scripts/{k8s_deploy.ps1,lib/k8s_client_entry.ps1,tests/k8s_deploy_contract.tests.ps1,tests/release_track_alerts_contract.tests.ps1}`、`go/client_rpc_router/internal/{metrics/metrics.go,logic/forwardlogic.go,logic/forwardlogic_test.go}`、`deploy/k8s/{release-track-alerts.yaml,README.md}`、`docs/ops/release-track-canary-runbook.md`、`docs/README.md`，共 12 个 | 生产形态（开着准入策略）也能做全局服务金丝雀；能按轨道观察、告警，晋升和中止有手册 |
| 3b 战斗按整局（11-10 已落码，未编译） | 会话轨道经 metadata 传到 match；玩家网络档案记轨道；全员 canary 的新成局先放 canary battle；stable 会话不看 canary 对局；`infra-up -CanaryBattleReplicas` 起 battle-canary 池（细节见下） | `go/shared/releasetrack/{releasetrack.go,crosslang_test.go}`、`cpp/libs/engine/core/node/system/node/release_track.h`、`cpp/nodes/gate/handler/rpc/client_session_handler.cpp`、`go/client_rpc_router/internal/logic/{forwardlogic.go,forwardlogic_test.go}`、`proto/match/match_authority.proto`、`go/match/{match_service.go,internal/svc/servicecontext.go,internal/discovery/node_watcher{,_parse_test}.go,internal/logic/{release_track.go,release_track_test.go,network_probe.go,global_match.go,global_pool_matcher.go,spectate.go,global_spectator.go}}`、`tools/scripts/{k8s_deploy.ps1,lib/k8s_client_entry.ps1,tests/k8s_deploy_contract.tests.ps1}`，共 22 个 | 战斗也能做金丝雀，闭环完整 |

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
  - NetworkPolicy：批 2b 先拒绝与 `-NetworkPolicies on` 同用。只改 `infra-up` 不够，理由有两条：scene 的准入名单由 `zone-up` 生成，金丝雀副本会连不上 scene 的 gRPC 口，资产与同步投递整片失败；guild、match 自己的准入策略按 `app: <svc>` 选 Pod，选不中金丝雀副本，它的 gRPC 口反而不设防。批 3a 把两侧名单一起补上，拒绝已去掉。
  - 顺带修了批 2 留下的测试缺口：`k8s_migrate_gate.tests.ps1` 按 AST 只抽函数跑 `Apply-OneGoSvc`，批 2 加的 `Add-GoSvcReleaseTrackEnv` 没恢复，这套用例会报「命令未找到」。现在夹具恢复真函数、按 stable 跑，并照抄 `$script:SqlWriterGoSvcs`。

**批 3a 的细节**（11-06 已落码，未编译；参数与行为见 §5.3）：

- **按轨道的转发计数**单独一个计数器，不在 `client_rpc_router_forward_total` 上加 label：已有的标签集合不变，看板和单测不受影响；新计数器不带 message_id，3 个轨道值乘 11 个终态，基数固定。
- **Pod 标签只写 canary**，和 env 同一判据。stable 写 `stable` 也说得通，但那样每个 Pod 模板都会变，已有 zone 全部滚动一次；缺标签当 stable 由 Prometheus 的 relabel 补，写法在手册里。仓库不部署 Prometheus，relabel 规则也不在仓库里；§2.8 的判据都不依赖它（路由服按会话轨道分、C++ 有 `cpp_node_info`、zone 按 namespace 比、对局按成局原件），它只在按轨道聚合 Go 服务自己的指标时用得上。
- **准入名单在生成时补 `<app>-canary`**，名单表不写：表里每个标签都要对应一个真实清单（契约测试逐个核对），金丝雀副本是 `infra-up -CanaryServices` 按需生成的。补上的标签固定存在，起停金丝雀不用重新生成 zone 里的策略。
- **告警的门槛**：混轨 3 小时按每台 gate 的排空预算约 35 分钟、5 台顺序排空估算；失败率比较用「超过基线 2 倍再加 1 个百分点」，两边都接近 0 时不会响。11-11 起门槛按 §2.8 的金丝雀判定定案，不再是待负责人给数的起步值。
- **手册**：[灰度与金丝雀发布手册](../ops/release-track-canary-runbook.md)，把 §2.7 的步骤展开成命令和核对项。

**批 3b 的细节**（11-10 已落码，未编译）。11-06 定方案时在改 match 和 battle 的几个会话 11-10 都已结束；动手前和「数据覆盖审计交接文档」会话分了工，它在改 `go/match/internal/authority` 的终局与恢复文件和 `global_match.go` 的终局函数，这些本批都没碰。

1. **会话轨道传到 match**。router 转发时在 metadata 里写 `x-release-track`（`releasetrack.MetadataKey`），取 `ForwardRequest.release_track` 解析后的值，覆盖 gate 带来的同名键；写的是会话的轨道，canary 会话退回 stable 实例时目标照样知道。gate 直连模式（`-GateRouterMode 0`）在通用的直连分发处写 gate 自己的轨道（`client_session_handler.cpp`）。C++ `release_track.h` 的 `kMetadataKey` 与 Go 同名，`go/shared/releasetrack/crosslang_test.go` 读头文件对拍。match 读不到这个键按 stable 算，写了多份或认不出就拒绝，和 router 同口径。它只决定这局放在哪条轨道的 battle 上，不是安全边界，所以不进 gate 的身份签名。
2. **档案记轨道**。`GetNetworkProbePlan` 存计划时把会话轨道写进 `MatchNetworkProfile.release_track`。`CommitProfile` 本来就读出计划里那份档案、只换回执再写回，所以轨道原样保留，不接受回执里自带的值。排队票入队时复制档案，刷新回执时一起刷新。
3. **成局判轨道**。`buildGlobalDirect`（点名成局）和 `claimGroup`（排队成局）读完全员档案后算这局的轨道（`battleTrackOf` / `poolClaimTrack`）：全员都是 canary 才是 canary，缺档案、空串、认不出一律按 stable。选点用 `pickBattleNode`：机房仍按 stable 节点选定，canary 局在这个机房里先在 canary 镜像里选，选不到退回 stable；stable 局只在 stable 节点里选。
4. **两份 battle 镜像**。原方案是让 `BattleNodes` 收两条轨道、在各调用点按轨道过滤；落码时改成保留 `BattleNodes` 只收 stable，另起 `CanaryBattleNodes`（`NodeWatcher.CanaryOnly()`）。这样探测目标、机房集合、节点计数这些已有调用点一处不用改，仍然只看 stable；只有新成局选点和按节点号确认已有对局（`battleInstanceRegistered` / `battleInstanceReplaced`，经 `battleEntryOfNode` 先查 stable 再查 canary）用到 canary 镜像。探测只给 stable 节点：机房按 stable 选，canary 会话不需要 canary 节点的延迟。
5. **观战**。选点时把选中节点的轨道写进成局原件 `MatchFormationRecord.release_track`（`buildGlobalDirect` 与 `claimGroup` 两处；canary 局退回 stable 节点时记 stable），之后不变。观战只按原件判（`viewerMayWatch`）：stable 观众只看记着 stable 或空串（补列前的行）的对局，canary 观众都能看，认不出的值不放行。三个入口用同一条判据：
   - **点名观战**：`locateGlobalWatch` 在原有判定之后回 `ErrBattleNotWatchable`（16018），意图按此结案；不发 battle RPC、不落 L0 原件、不摘观战索引（canary 观众仍能看）。
   - **随机观战**：stable 观众走 `pickRandomBattleFor`，每抽一场读一次原件，抽到 canary 局就重抽，最多 8 次，抽不到按「没有可看的场」结案；跳过的场不摘索引。代价是 stable 观众每次随机多一次按主键读。
   - **观战列表（含赛事局）**：`watchableSummary` 按观众轨道过滤，滤掉的场不摘索引。canary 镜像里有节点时 stable 观众多读一倍候选；镜像为空时不多读，残留的 canary 场照样滤掉，只是这一页可能短一点（与懒剔除不回填同口径）。

   最初的落码按实时 canary 镜像判，match 刚启动、镜像还没同步时会放过，随机与列表也没过滤；11-10 改成按原件判，结论只取决于原件，与镜像何时同步无关。`match_formation` 按列存，加列要改 `store.go` 的列清单、插入与读取三处（读取用 `sql.NullString` 接补列前的 NULL），动手前与数据覆盖审计会话分过工。
6. **部署**。`infra-up -CanaryBattleReplicas N -CanaryNodeImage <ref>` 另起一个 battle 池：同一条渲染路径（`New-BattleWorkloadManifest -Canary`），换镜像、拉取策略、轨道与副本数，渲染完由 `Rename-CanaryWorkload` 改名成 `battle-canary`、全部 `app: battle` 换成 `app: battle-canary`（容器名不变）；写 canary env 与 Pod 标签（Agones Fleet 两层）。和 stable 共用 battle-node-config、机房号、探测与终局密钥、Agones SDK 身份。不粘滞，没起就删以前留下的 `battle-canary`（只删本次编排形态的那种 kind）。切换编排形态时，`Get-BattleObsoleteResources` 每个方向列两项（另一种 kind 的 stable battle 与 `battle-canary`），由 `Apply-BattlePool` 开头的旧形态删除一起删，写路径入口的 `Assert-ClientEntryClusterState` 也按这份清单先判，过同一道 `-AllowDisruptiveSwitch` 闸；集群里没有 Agones CRD 时 Fleet 项按 ApiGroup 跳过。要求 stable 池在（`-BattleReplicas > 0`）；不支持 hostPort 形态，两个池会抢 hostPort 20000。
7. **测试**。router 的 metadata 用例（`TestForwardWritesSessionTrackMetadata`）；Go 与 C++ 键名对拍；`NodeWatcher.CanaryOnly()`；match 的会话轨道读取、成局判轨道（全员 canary、混合、旧档案、解不开）、选点（canary 局用 canary、选不到退回、stable 局不进 canary）、两份镜像的查找（`internal/logic/release_track_test.go`）；部署契约 4 条，旧形态清单每个方向两项（`k8s_client_entry_contract.tests.ps1`）。观战：判据 `viewerMayWatch`（六种组合，认不出的轨道不放行）、选中节点的轨道就是原件记的轨道、列表按观众轨道过滤（含赛事局、不摘索引、多读候选、认不出的会话轨道报错）是单测；点名观战被拦（16018、不发 RPC、不落原件、不摘索引，canary 观众同一场直连）和随机观战跳过 canary 局是两条真库用例（`global_spectate_lobby_native_test.go`，要 `MATCH_IT_TIDB`）。

**收尾（11-11，未编译）：金丝雀判定定案**。负责人 11-11 要求查出的问题当场修、等压测再定的先按通行做法定。本轮定下 §2.8（批次、窗口、流量下限、五条判据与门槛的依据），并补齐能机械化的部分：

- 路由服新增 `client_rpc_router_forward_by_track_seconds{release_track}`（`go/client_rpc_router/internal/metrics/metrics.go`），与 `forward_seconds` 共用分桶 `forwardLatencyBuckets`，在 `ObserveForward` 里一起记。单独一个直方图，不给 `forward_seconds` 加 label：已有的序列与看板不变，新直方图基数是 3 条轨道乘 13 个桶。
- 告警新增 `ReleaseTrackCanaryForwardLatencyHigh`；`release_track_alerts_contract.tests.ps1` 改成按组钉死规则名单，并新增延迟规则的用例：指标注册与共用分桶、两边都取 p99 并按 le 聚合、只比较 stable 与 canary。
- 对局作废率不加指标，用 SQL 按成局原件的轨道统计：终局提交在 `authority/completion.go`，这一块数据覆盖审计会话正在改，而原件和终局表本来就在 TiDB 里，是强一致的权威数据。
- §6「轨道不进任何持久数据」改正为只不进玩家存档；批 3b 起成局原件记了这局 battle 的轨道。

**与其他会话的冲突**：11-05 工作区里，login（`loginqueue`、`node`）、scene_manager（`world_init`、`createscenelogic`）、gate 的 `main.cpp`、九个全局服务的入口文件、`k8s_deploy.ps1`、`common.proto` 都有其他会话未提交的改动。批 1 只碰当时没人在改的文件，`common.proto` 只在 `NodeInfo` 末尾追加字段；批 2 的服务入口和部署脚本，要等那些改动入库后再动。

**验证分工**（AGENTS.md §10.1）：Claude 只写服务器代码、测试和文档；协议生成、编译、单测、冒烟由 Codex 执行，每批落码时给出命令清单。

## 8. Java 版

待做。Java 版（0.1.0-SNAPSHOT）用本版的 Netty、Dubbo 和部署形态实现同一套规则：两条轨道、按 zone 灰度、会话轨道跟随入口、全局金丝雀只接客户端请求、战斗按整局。已在 Java `PARITY.md` 登记待做。

## 9. 参考

- Kubernetes 的 [金丝雀部署](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#canary-deployments)：不同版本用不同的 Deployment 和标签区分。本文的 `<svc>-canary` 与 Pod 标签沿用这一做法，再补上业务归属、调用隔离和后台工作的作用域。
- 独立 Go 仓 `D:\luyuan\xuanming-server` 的 `pkg/releasetrack` 与 `docs/design/zero-downtime-update.md` §6.3 也用 stable／canary 两条轨道和「canary 没容量退回 stable、stable 不进 canary」的规则。那边按玩家哈希百分比分配 Hub DS，这里按 zone 和会话分配，原因见 §2.2；两仓代码不共用。
