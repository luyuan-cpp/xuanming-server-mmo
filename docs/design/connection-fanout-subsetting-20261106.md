# 连接与发现扇出:路由服确定性子集、每类节点只发现自己用到的(2026-11-06)

> 状态:**批 1、批 4 已落码未编译(2026-11-10)**,批 2、3 由「缺口第 30 条:gate 连接与解码扇出」会话做。对应[服务器代码缺口清单](../handoff/server-gap-audit-20261101.md)第 30 条与第 28 条后半(「连全服全部目标」)。
> 2026-11-10 改:路由服挑全服池目标不用 HRW,改为确定性 aperture(§3.3,理由见 §5);gate 侧按 zone 取路由服仍用 HRW(§3.2)。
> 接在[控制面按 zone 化](control-plane-zone-scoping-20261105.md)之后:那份已经把 zone-scoped 类型的发现收窄到本 zone、把兜底 Range 拉长到约 60 秒。本文处理剩下的、随全服规模平方增长的连接与 watch。
> 挑 gate 的规则以「玩家切换zone和gate链接」会话 11-06 的改动为准(`go/shared/gatepick/eligibility.go` 一次分档判定),本文不另定挑 gate 规则。

## 1. 结论

1. **gate → 路由服:按 zone 取确定性子集。** zone z 的路由服集合 = 对 `(z, 路由服 node_uuid)` 做 HRW(rendezvous)打分取前 k 台,k 缺省 3,在同一发布轨道内算。gate 只连、只挑这 k 台。gate 到路由服的连接从 G_全服 × R 降到 G_全服 × k。路由服增减时,只有前 k 名变了的 zone 换路由服,其余 zone 不动。
2. **路由服 → 目标:zone 跟着子集走,全服池再取一次子集。** 稳态下一台路由服只收到分给它的那些 zone 的请求,zone-scoped 目标(login、scene_manager)只会拨这些 zone 的实例。全服池目标(match、chat、friend、guild 等)用确定性 aperture:同轨道路由服按 node_uuid 排名,在目标环上均匀各占一段,每台路由服约连 k_t 台(缺省 4),每台目标收到的请求一样多、一台不漏。连接按空闲回收,子集变了之后旧连接自然消失。
3. **gate ↔ scene 的 zone 内全连接保持不变。** 它的上界是 G_z × S_z,随单 zone 规模增长,不随全服 zone 数增长;单 zone 规模由开服容量限定。重新评估的触发条件:单 zone 的 G_z × S_z 超过 10 000 条连接(经验值,待压测),或者做万人同屏的时候(那时整体重做,现在不预留)。
4. **每类 C++ 节点只发现自己用到的类型。** 每类节点显式声明自己的发现集合,与 `CanConnectNodeTypeList` 并列。共享的前缀列表(本机 `bin/etc/base_deploy_config.yaml` 13 个,K8s 模板 11 个)只回答「部署里有哪些前缀」,不再决定每个节点 watch 什么。battle 的集合是 {Match},自身类型只 watch 自己那一条键,不再 watch 全部 battle。
5. **控制面命令 topic 的分区数先不改。** 每个节点只 assign `node_id % P` 那一个分区,读放大等于同分区的节点数。按该 topic 的设计口径(控制面总量每秒几百条),P = 256 时每个节点每秒只解码几条。读放大计数与告警已加(§3.5),超过阈值再按既有规则换代号、换更大的 P,绝不原地扩分区。
6. **场景亲和先测再定。** 场景广播已经按 gate 分组,每台 gate 一份。亲和只能减少一个场景的玩家分散在几台 gate 上。scene 侧已加每次广播涉及几台 gate 的直方图(§3.6),压测证明它是瓶颈再做。要做时作为 gatepick 判定的一维(同一档里偏好已有该场景玩家的 gate),不在调用方另加一层过滤。

## 2. 现状(2026-11-06 核对)

| 项 | 位置 | 现状 |
|---|---|---|
| gate 连路由服 | `cpp/nodes/gate/main.cpp:379-382` | 路由模式白名单是 {Scene, ClientRpcRouter};发现到的每台路由服都建连 |
| gate 挑路由服 | `cpp/nodes/gate/handler/rpc/client_session_handler.cpp` 的 `PickRandomNode` | 在全部已连上的路由服里随机;全服池类型按本 gate 的发布轨道过滤 |
| 路由服挑目标 | `go/client_rpc_router/internal/discovery/node_watcher.go` | 11-05 起按 zone、按轨道的只读索引,选点 O(1) |
| 路由服连目标 | 同文件 `DialEndpoint` | 懒拨号,按 endpoint 缓存,只在节点下线时关;路由服不分 zone,任何 zone 的请求都可能到任何一台,长期运行后每台都连上全服全部目标 |
| C++ 发现 | `cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp` 的 `EffectiveDiscoveryPrefixes` | 每个节点对部署配置里同一份前缀列表(本机 13 个,K8s 模板 11 个)Range 并 watch,zone-scoped 类型收窄到本 zone;全服池类型(battle、路由服、match、data_service、player_inbox、team)全量 |
| battle 看 battle | `tools/scripts/k8s_deploy.ps1` 的 base_deploy_config 模板注释、`cpp/nodes/battle/main.cpp` 的 `ObservedAdvertised` | battle 保留 BattleNodeService 前缀只为了看自己的键(劫持检测、注册自检),实际收到全部 battle 的变更 |
| 命令 topic | `cpp/libs/engine/core/node/system/node/node_command_topic.h` | partition = node_id % P,P 缺省 256,可配置,改 P 要换代号 |
| 场景广播 | `cpp/libs/engine/core/network/client_message_sender.cpp` | `BroadcastToPlayers` 按 gate 分组,每台 gate 一份,只带本 gate 上的收件人;`BroadcastToSceneClientsImpl` 给场景玩家所在的每台 gate 各发一份 |

**2026-11-10 批 2、3 动手前重新核对**(行号以这里为准):

- gate 白名单在 `cpp/nodes/gate/main.cpp:471`,依赖门在 `:659`。挑路由服的唯一入口是 `client_session_handler.cpp` 的 `PickRouterChannel`(`:232`,内部调 `PickRandomNode`),路由 lane、断线通知、排空迁移请求都经它。路由模式下 gate 读的节点类型只有 Scene、ClientRpcRouter 和自身;Login 只在直连模式里读。
- 本机配置的 13 个发现前缀里,Friend、Guild 没有任何 C++ 读者。scene 不读 Battle、ClientRpcRouter、Login;gate 不读 DataService、PlayerInbox、PlayerStore、Team、Battle;battle 只读 Match 和它自己的登记。
- 自身类型的发现快照有三个读者:选号 `NodeAllocator::SelectNodeId`(`node_allocator.cpp:33`)、选端口 `AcquireNodePort`(`:156`)、注册自检 `StartNodeRegistrationHealthMonitor`(`node.cpp:2379`);battle 另有可分配状态回读 `ObservedAdvertised`(`battle/main.cpp:135`)。后两个只找自己。
- **新问题(11-05 发现前缀按 zone 收窄带来的)**:node_id 的分配键 `<Svc>.rpc/allocated/node_type/<t>/node_id/<n>` 全服唯一(`etcd_manager.cpp:35`),选号却只看发现快照里的同类节点。收窄之后 gate、scene 的快照只剩本 zone 的同类,新节点从最小空闲号起逐个去撞别的 zone 已占的号,每撞一次等 1 秒(`etcd_service.cpp:605`)。zone 越多,后起的节点启动越慢,随全服规模线性增长。批 3 一并修(§3.4)。

## 3. 设计

### 3.1 HRW 子集(gate 侧)

- 只用于 gate 按 zone 取路由服(§3.2)。路由服挑全服池目标用确定性 aperture(§3.3):HRW 分得匀靠键多,zone 成百上千个,分得匀;路由服那一侧的键是路由服本身,只有几台到几十台,用 HRW 会严重不均(§5)。
- 打分:`score = FNV-1a-64(key + "/" + member_uuid)`,取分数最高的前 k 个;分数相同按 uuid 字典序。不用 `std::hash`:它的结果随编译器和平台变,Windows 与 Linux 构建的 gate 会算出不同的子集。这不影响正确性,但会让问题难查。gate 侧(C++)在 FNV-1a-64 之后再过一遍 splitmix64 的终混:HRW 比的是整个 64 位分数,FNV-1a 末尾几个字节对高位的扩散偏弱,加一遍终混是这类用法的常见做法,代价是几次乘法。
- 只有算子集的 gate 需要这个函数,不需要跨语言对拍。路由服无状态,同一 zone 的 gate 在成员变化的几秒内看法不一致也没有正确性问题。
- 成员来自发现:路由服与全服池目标的实例数都在百台以内,全量 watch 的代价是线性的(控制面按 zone 化 §6 第 6 条),不用改。重算是 O(成员数),只在成员变化时做一次。
- 发布轨道:先按轨道分组再取子集。canary 会话优先 canary 组,组里没有可用实例才退 stable;stable 永远不进 canary。规则与 `releasetrack.Select` 相同。
- 实例数不足 k 时取全部。

### 3.2 gate 侧

- 子集:key 是本 gate 的 zone 号(十进制串),成员是发现快照里的路由服,按发布轨道分组,各组各取 HRW 前 k 台。stable gate 只用 stable 组的前 k;canary gate 用 canary 组的前 k 加 stable 组的前 k,因为 `release_track::Select` 在 canary 组没有可用实例时退回 stable,退回的那几台也要连着。轨道认不出的路由服不进任何组(选点本来就跳过它们)。一个 zone 只有一条轨道,同 zone 的 gate 算出同一个子集。
- k 由环境变量 `GATE_ROUTER_SUBSET_SIZE` 给,缺省 3,合法范围 2~64,非法拒启。下限 2:一台路由服重启时,本 zone 至少还有一台可用。用环境变量、不进 proto 配置:它和路由模式开关 `GATE_CLIENT_RPC_ROUTER` 是同一类部署参数,缺省值适用于全部部署,不值得为它重新生成协议。
- 容量口径:一个 zone 的客户端请求只落在 k 台路由服上。k 要不小于「zone 峰值请求率 ÷ 单台路由服能力」再加一台冗余,缺省 3 待压测校准。只有一两个 zone 的部署里,多出来的路由服分不到流量,要扩吞吐就调大 k。
- 引擎加三处接缝(`Node`),都只在 loop 线程用:
  - `SetConnectionAdmission(type, fn)`:白名单类型在建连前再过一道准入。`ServiceDiscoveryManager::AddServiceNode` 与 `NodeConnector::ConnectAllNodes` 都改成问 `Node::AdmitsConnection`(在白名单里且准入通过)。没装准入的类型行为不变。
  - `SetDiscoveryObserver(type, fn)`:发现快照里这一类的成员变了(加入、更新、摘除)之后调用,调用时快照已是新状态。发现快照每类型带一个代数(generation),每变一次加一。
  - `DropUnadmittedConnection(type, uuid)`:拆掉一条已不被准入的连接,只拆本节点到它的网络实体(先摘 gRPC 通道,再发 `OnNodeRemoveEvent`、销毁实体,与节点摘除共用一段代码),不动发现快照。排到下一轮执行,执行时再判一次准入,那时又被准入(子集变回来)就保留;停机开始后不做,由停机流程统一关。
- gate 的子集模块(`handler/rpc/router_subset.{h,cpp}`,纯计算在 `router_subset_policy.h`)装在这三处:
  - 准入 = 「在当前子集里」。子集按发现快照的代数缓存,快照没变不重算。
  - 观察者里对账,规则在纯函数 `PlanReconcile`:在子集里、已在快照、还没建连的立即建连(服务还没启动时不建,启动时 `ConnectAllNodes` 按准入建);已建连、仍在快照、掉出子集的立即不再被挑,并排一次延后关闭(已排过的不顺延);已排关闭但回到子集、已不在快照(节点摘除会拆它)或连接已经没了的撤销。到期由一个只在有待关连接时运行的 1 秒扫描定时器处理,截止时刻用单调时钟。
  - 延后关闭的时长 = 路由服调用期限(`GrpcClient.CallDeadlineMs.ClientRpcRouterNodeService`)加 lane 的在途过期余量 `kInflightExpiryMargin`(1 秒)。到那时 lane 里发往它的请求都已收到回包或被过期认领;万一还有在途调用,`~Channel` 以 UNAVAILABLE 完成它,lane 按「每个请求恰好一个回执」认领,已过期的直接丢弃。它再进子集时走普通发现路径重新建连。
- 选点:`PickRouterChannel` 改调子集模块的 `PickRouter()`。候选 = 已建连(挂着未关闭的通道)∩ 当前子集 ∩ 轨道合法,再按 `release_track::Select` 取轨道,最后按通道状态分级:READY 优先,其次 IDLE / CONNECTING,最后其余;级内随机。分级是子集的配套:子集只有 k 台,一台进程挂了、租约到期之前它的通道处在 TRANSIENT_FAILURE,不分级就有 1/k 的请求失败,全连接时只有 1/R。
- 依赖门不改:它等的是「发现到路由服」,子集非空当且仅当快照里有本 gate 能用的轨道的路由服。
- 子集里的路由服都连不上(登记还在、进程不响应):照旧回 `kServiceUnavailable`,不临时扩大子集。登记失效(租约过期)后它自然掉出子集,下一名补上。

### 3.3 路由服侧(批 1,已落码未编译)

- zone-scoped 目标的挑选不变。gate 只把本 zone 的流量送给本 zone 的 k 台路由服,每台路由服只会拨约 Z × k / R 个 zone 的实例。
- 全服池目标用**确定性 aperture**(`go/client_rpc_router/internal/discovery/subset.go`;Finagle 的 deterministic aperture,Google SRE 书「数据中心内的负载均衡」的确定性子集是同一类做法):
  - 一条发布轨道的目标按 node_uuid 排成环,每台占一样长的一段;同轨道的 R 台路由服按 node_uuid 排名,第 i 台的弧从 i/R 圈处起、长 m/R 圈;路由服在自己的弧上均匀取点,交给占这一点的目标。
  - m = round(k_t × R / T),至少 1。环上每一点恰好落在 m 段弧里,所以各路由服负载相同时,每台目标收到的请求完全一样,而且一台都不漏。一段弧上约有 k_t 台目标,两端没对齐目标边界时各分一部分,最多再多一台;T 多于 R × k_t 时 m 取 1,每台路由服连 T/R 台,保证全覆盖。k_t ≥ 2 时每台路由服至少能挑两台。
  - 只用整数:环切成 R × T 格,目标 j 占第 [j×R, (j+1)×R) 格,路由服 i 的弧是第 [i×T, (i+m)×T) 格。任一格 c = q×T + r 恰好被 i ∈ {q, q−1, …, q−m+1}(模 R)这 m 台覆盖。单测逐格枚举 R ≤ 12、T ≤ 30、k_t ∈ {0, 2, 3, 4, 8} 的全部组合核对这一条。
  - 先按轨道分组再取子集,轨道选法仍只由 `releasetrack.Select` 决定:canary 会话有 canary 组就只在 canary 组的弧上挑,stable 永远不进 canary。
  - 名次只在**同轨道**路由服里算(`PeerRank`):gate 按自己的轨道挑路由服,同一条轨道的路由服一起承担同一批会话;把 canary 路由服算进 stable 的环,stable 流量会在 canary 路由服那段弧上缺一块。为此路由服多 watch 一个前缀 `ClientRpcRouterNodeService.rpc/`,只用来算名次,不拨号;镜像里还没有自己(注册事件未到)时把自己算进去,按 node_uuid 去重。
  - 以下情况在全部实例里挑:注册之前(还不知道自己的 node_uuid;服务注册后才开始接请求,生产上这段时间没有转发)、只有一台同轨道路由服、实例不多于 k_t、算出的 m ≥ R、`GlobalTargetSubsetSize: 0`。
  - 配置键 `GlobalTargetSubsetSize`,缺省 4;只许 0 或 ≥ 2(1 台时它一重启,本路由服对这类目标全部失败)。本地 yaml 写了这一行,K8s ConfigMap 逐字镜像(与 chat 的可选键同一写法,契约测试守着)。
- 连接缓存记最后一次取用的单调时刻,后台每分钟(`IdleConnSweepInterval`)关掉空闲满 10 分钟(`IdleConnTimeout`)的连接。子集或 zone 分配变了之后,不再被用的连接靠它回收,不会只增不减;节点下线时的立即关闭保持不变。
  - 取用在读锁里记时刻,回收在写锁里判空闲,两者互斥:刚取到的连接不会被当成空闲关掉。在途转发用的连接一定在 `ForwardTimeoutMs` 之内被取用过,所以启动时校验 `ForwardTimeoutMs` 必须短于 `IdleConnTimeout`。
- 指标:`client_rpc_router_target_conns`(每轮回收后剩下的缓存连接数)、`client_rpc_router_idle_target_conns_closed_total`,不带 label。名次变化时打一行 `[subset] Global target pick range updated`。

### 3.4 C++ 每类节点的发现集合

- `Node` 构造多一个参数:发现集合 `DiscoveryNodeTypeList`,与建连白名单并列,由各节点的 `main.cpp` 显式给出。构造时校验发现集合包含白名单(不能连没发现的类型),不满足就 `LOG_FATAL`。
- 实际 Range / watch 的范围 = 配置前缀里属于发现集合的那些(zone-scoped 照旧收窄到本 zone,纯函数 `NodeUtils::PlanDiscoveryScopes`);配置里有、集合里没有的前缀不再 Range、不再 watch,启动日志逐条列出。集合里有、配置里没有的类型启动时打一行 WARN:部署里没有这个服务,调用方照旧报找不到。配置前缀表因此只回答「部署里有哪些服务」。
- 自身类型:
  - 在集合里(gate、scene,都是 zone-scoped,只看本 zone 的同类)照旧按前缀;
  - 不在集合里(battle):拿到 node_id 之后(首次 `StartRpcServer`)开始 Range 并 watch 本节点自己那一条发现键。用精确键(range_end 为空),不用前缀:`.../node_id/1` 当前缀会把 10、11、100 号也套进来。劫持检测、注册自检、battle 的可分配状态回读都只看这一条。C++ 节点的 node_id 在进程内不变(重注册沿用原号,CAS 失败即冲突停机),这条键一旦确定就不再变;watch 流重建、兜底重拉、watch 被取消后的重拉都照常带上它。
- 选号不再依赖同类快照:分配键 CAS 的同一个 txn 在失败分支里只取键(keys_only)列出全服这一类型已分配的号,分配器记下这份清单(以最新一份为准),下一次选号把它们排除,规则仍是最小空闲号。第一次选号照旧用快照加本次启动输掉的号;多 zone 部署里第一次多半撞号,多一次 1 秒的重试,之后不再逐个撞。撞号后仍等 1 秒再试:同一时刻启动的一批同类节点每轮只有一个赢家,不等会在几毫秒里打出 N² 个 txn。这同时修掉 §2 记的「收窄后逐个撞号」。
- 选端口不改:K8s 用预设端口、每个 Pod 一个 IP;本机多进程靠绑端口检查与端口键 CAS 兜底,自身类型不在快照里只是少一道预排除。
- 守卫:
  - 构造时发现集合必须包含白名单;
  - `DependencyGate` 要等的类型必须在发现集合里,否则启动即 `LOG_FATAL`(不然永远等不到);
  - 发现范围为空(配置的前缀里没有一项属于发现集合)启动即 `LOG_FATAL`:注册链从第一个 watch 应答开始(`EtcdService::OnStreamWatchResponse`),一个范围都没有就永远不会注册;
  - 发现快照按类型读(`NodesOfType` / `HasNodesOfType`)读到集合之外的类型(自身类型除外)时,每个类型打一次 ERROR:那份快照恒空,说明集合漏了。
  - 不在 `GetRegistry` 上加断言:直连模式下客户端消息的目标类型来自路由表,可以是本 gate 不连的服务,这是合法的「这里不服务」,Debug 构建里断言会让客户端的一条消息打死 gate。注册表只经建连(白名单,包含于发现集合)和入站握手(scene 的 Gate)写入,别的类型的注册表恒空,调用方照旧报找不到。
- 初始集合(2026-11-10 按代码逐处核对,以代码为准):
  - scene:建连白名单 SceneManager、DataService、PlayerInbox、PlayerStore、Team、Match;Gate(gate 主动连进来,入站登记 `registration_manager.cpp` 查快照,广播用 `ResolveLocalZoneGateEntity`);自身 Scene(本 zone)。
  - gate:路由模式 Scene、ClientRpcRouter;直连模式 Scene、Login、SceneManager、Match;两种都带自身 Gate(本 zone)。
  - battle:Match;自身只看自己的键。
- 效果:所有 C++ 节点不再 Range / watch Friend、Guild;scene 不再看 Battle、ClientRpcRouter、Login;gate 不再看 DataService、PlayerInbox、PlayerStore、Team、Battle,路由模式另不看 Login、SceneManager、Match;battle 之间的 B² 项消失。gate、scene 仍看本 zone 的同类,随单 zone 规模增长,与 §1 第 3 条同一口径。

### 3.5 命令 topic 的读放大(批 4,已落码未编译)

- 计数 `node_command_consumed_total{node_type, topic, target}`(`cpp/libs/engine/core/node/system/node/node_command_consume_stats.h`)。计在 `RegisterKafkaCommandHandler` 的回调里(`node_kafka_command_handler.h`):过了目标过滤、已派发的记 own;目标过滤判定不是本节点的记 foreign,包括同分区的别的节点和同号前任实例的残留命令,维护模式的精确目标校验同样算。解码失败的不计。
- 比原设计多一个 `topic` 标签(topic 基名,不带代号):同一个节点还会消费广播 topic(如 chat-world-bcast,`assignAllPartitions`),它们只有 own,混在一起会把比值拉低。标签取值都是低基数。
- 计数表只在注册命令消费时追加(启动期),第一次注册时把采集回调登记进 `/metrics`,不改 node.h/.cpp。
- 告警 `NodeCommandTopicReadAmplificationHigh`(`deploy/k8s/node-command-topic-alerts.yaml`,经验值,待压测):单节点(按 namespace、pod、node_type、topic 聚合)foreign 每秒超过 50 条,且 foreign 超过 own 的 50 倍,持续 10 分钟,severity warning。比值用乘法写,own 为 0 时不除零。契约测试 `tools/scripts/tests/node_command_topic_alerts_contract.tests.ps1` 核对指标名、label 名与取值都是 C++ 写出的。
- 处理办法沿用 `node_command_topic.h` 的契约:换代号、换更大的 P(2 的幂),生产端与消费端按同一配置一起切换,不在同名 topic 上扩分区。

### 3.6 场景亲和(先测;直方图为批 4,已落码未编译)

- 直方图 `scene_broadcast_gate_fanout{kind}`(`cpp/libs/engine/core/network/broadcast_fanout_stats.h`):每次广播实际发给了几台 gate,桶上界 1、2、4、8、16、32、64。kind 取 players(按收件人集合广播 `BroadcastToPlayerClients`:AOI 进出视野、移动、视野内消息)或 scene(整场景广播 `BroadcastToSceneClients`,目前没有调用方)。原设计只数场景广播,但实际流量都走按收件人集合广播,两条一起记。只记至少发给一台 gate 的广播。
- 场景类型(主世界 / 副本)不进标签:引擎层只有 scene_id,要知道类型就得改每个调用方;主世界与副本跑在不同的 scene 部署上(`docs/ops/scene-node-role-split.md`),抓取目标自带的标签已经能分开。
- Prometheus 文本写出器(`metrics/prometheus_text.h`)为此加了 histogram 类型(`_bucket{le}` / `_sum` / `_count`)。scene 的 `main.cpp` 调 `RegisterBroadcastFanoutMetrics()` 登记。
- 压测中如果它长期接近 G_z,而且 gate 的入站广播是瓶颈,再做亲和:作为 gatepick 判定的一维,在同一档里偏好已经有该玩家目标场景玩家的 gate。前提是 login 在 AssignGate 时知道玩家要去的场景(落点或上次位置),那时另写设计。

## 4. 失败与一致性

- 子集变化只会让路由服开始收到新 zone 的请求,第一批请求多付一次建连(毫秒级)。路由服不持有会话状态,请求之间的顺序由 gate 的会话 lane 保证(`client-request-ordering.md`),与挑哪台路由服无关。
- 滚动重启路由服:每台重启使成员变两次。HRW 只移动前 k 名变动的 zone,其余 zone 的连接不动。
- 全服池子集:目标实例增减时,环上各目标的段整体平移一点,每台路由服的弧两端换掉一两台目标;路由服增减时同轨道的名次变化,各段弧同样小幅平移。代价是新进弧的目标第一次请求多付一次建连,掉出弧的连接 10 分钟后被空闲回收。各路由服看到的成员短时不一致只让这几秒分得不够匀,挑到的仍是镜像里的实例,不影响正确性。
- 发现集合漏了某个类型:建连类型漏了构造即拒启,依赖门要等的类型漏了启动即拒启;别的读者读到的快照恒空并打一次 ERROR,调用方照旧报「Node not found」。所以落码时仍要逐处核对(§3.4)。
- gate 子集变化:掉出子集的路由服先停止被挑,过调用期限加 1 秒再拆连接,在途请求照常收尾;拆连接不动发现快照,它再进子集就重新建连。同 zone 的 gate 在成员变化后的几秒内看法可能不同,只会让某台路由服短暂多收一个 zone 的请求。
- battle 自身改看精确键:本节点登记被删(租约过期)仍由这条 watch 的 DELETE 进快照,注册自检照常发现「快照里没有自己」并重注册;watch 流断开期间的变化由流重建后的重拉对账。
- 选号清单是那次 txn 时刻的快照,之后被别人拿走的号照旧靠 CAS 失败发现;清单只用来排除,不用来认定「这个号空着」。

## 5. 被否决的做法

- **按 zone 部署路由服**(路由服进 zone namespace):Pod 数随 zone 数增长,小 zone 也要至少 2 台。HRW 在共享池上得到同样的局部性,一台挂了自动由下一名接手。
- **带虚拟节点的一致性哈希环**:k 小、成员在百台以内时,HRW 更简单,O(成员数)重算,不用维护环。
- **etcd gRPC proxy 合并 watch**:发现集合收窄之后用不上;进程数到 10⁵ 量级时再评估。
- **gate 不 watch 路由服、改由配置给地址**:失去自动扩缩与故障摘除。
- **路由服挑全服池目标用 HRW 取前 k_t 台**(本文初稿的写法,2026-11-10 落码时否决):每台目标被几台路由服选中服从二项分布 B(R, k_t/T)。R = 10、T = 8、k_t = 4 时,少的目标只被 2 台路由服选中,多的被 8 台选中,负载差 4 倍;R × k_t 小于 T 时还会有目标一台路由服都不连,加实例不加容量,水平扩展失效。HRW 只在键多的那一侧(gate 侧的 zone)分得匀。
- **Google SRE 书的「按轮洗牌」确定性子集**:要求路由服编号连续;T 不是 k_t 的整数倍、路由服又只够一轮时,每轮剩下的目标没有路由服连,同样会漏。aperture 没有这个洞,而且只用排名,不要求编号连续。

## 6. 分批与文件

| 批 | 内容 | 文件 | 依赖 |
|---|---|---|---|
| 1 | 路由服全服池子集(确定性 aperture)与空闲连接回收。2026-11-10 已落码未编译 | `go/client_rpc_router` 的 `internal/discovery/subset.go`(新)、`node_watcher.go`(选点索引、连接池)、`internal/svc`、`internal/config`、`internal/metrics`、`client_rpc_router_service.go`、`etc/client_rpc_router.yaml` 及单测;`tools/scripts/k8s_deploy.ps1` 的 ConfigMap 镜像与契约测试 | 无 |
| 2 | gate 的路由服子集:HRW、准入 / 观察 / 拆连三处接缝、延后关闭、选点分级。2026-11-10 已落码未编译 | 新增 `core/node/system/discovery/rendezvous_subset.h`、gate `handler/rpc/router_subset_policy.h`、`router_subset.h/.cpp`;改 `node.h/.cpp`(`SetConnectionAdmission` / `SetDiscoveryObserver` / `AdmitsConnection` / `NotifyDiscoveryChanged` / `DropUnadmittedConnection`,节点摘除与拆连共用 `DestroyNodeEntityByUuid`)、`service_node_snapshot.h`(按类型的代数)、`service_discovery_manager.cpp`、`node_connector.cpp`,gate `main.cpp`(安装与 context 成员)、`client_session_handler.cpp`(`PickRouterChannel` 改调 `PickRouter`)、`client_request_lane.h`(注释),`gate.vcxproj/.filters`、`core.vcxproj/.filters`;用例 `cpp/nodes/gate/tests/router_subset_test.cpp`,登记在 `routing_identity_test.vcxproj` | 无 |
| 3 | 每类节点的发现集合;选号读全服分配清单;battle 只看自己的键。2026-11-10 已落码未编译 | 新增 `core/node/system/node/node_id_selection.h`;改 `node.h/.cpp`(构造参数、校验、首次 `StartRpcServer` 开始看自己的键)、`node_entry.h`、`node_util.h/.cpp`(`PlanDiscoveryScopes`、`NodeTypeOfDiscoveryPrefix`)、`service_discovery_manager.h/.cpp`、`service_node_snapshot.h`(读守卫)、`etcd_service.h/.cpp`(精确键)、`etcd_helper.h/.cpp`(`RangeKey`、`StartWatchingKey`、`PutIfAbsentListingOnConflict`、`TxnConflictListing`)、`etcd_manager.cpp`、`node_allocator.h/.cpp`,gate / scene / battle 的 `main.cpp`,`core.vcxproj/.filters`;用例 `cpp/tests/cross_zone_test/discovery_set_test.cpp`,登记在 `cross_zone_test.vcxproj/.filters` | 改引擎公共代码,所有 C++ 节点受影响 |
| 4 | 计数:命令 topic 读放大、广播 gate 扇出。2026-11-10 已落码未编译 | `metrics/prometheus_text.h/.cpp`(加 histogram)、`node/system/node/node_command_consume_stats.h`(新)、`node_kafka_command_handler.h`、`network/broadcast_fanout_stats.h`(新)、`client_message_sender.h/.cpp`、scene `main.cpp` 一行、`core.vcxproj/.filters`;`deploy/k8s/node-command-topic-alerts.yaml`(新)与契约测试;gtest 在 `cpp/tests/rpc_controller_test/prometheus_text_test.cpp` | 无 |

- 2026-11-10 负责人改派:批 2、3 由「缺口第 30 条:gate 连接与解码扇出」会话做;批 1、4 仍归「服务器代码缺口补完」。

- `node_command_topic.h` 另有会话的未提交改动(观战 topic),本设计不改这个文件。
- 新增 C++ 源文件只登记 `.vcxproj` 与 `.filters`,CMakeLists 由它生成。

## 7. 验证(交 Codex)

- 批 1:`cd go/client_rpc_router && go build ./... && go vet ./... && go test -race ./...`。用例:`TestApertureBalancesEveryTargetExactly`(逐格枚举,每台目标的份额完全相等且大于 0)、`TestApertureSubsetSize`(每台路由服至少两台、确实收窄、R × k < T 时全覆盖)、`TestApertureFallsBackToAllOnUnknownRank`、`TestPeerRank`、`TestPickRandomForTrackStaysInArc`、`TestConnPool*`(注入时钟的空闲回收)、`TestSubsetIdentityNarrowsGlobalTargetsAndFollowsPeers`、`TestBuildRejectsForwardTimeoutNotShorterThanIdleTimeout`;另跑 `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`(ConfigMap 镜像 `GlobalTargetSubsetSize`)。gate 侧 HRW 的分布用例归批 2。
- 批 4:`msbuild` 串行 `/m:1` 编 core 与 scene、gate、battle,再编 `rpc_controller_test` 并跑 `--gtest_filter=PrometheusText.*:BroadcastFanoutStats.*:CommandConsumeStats.*:MetricsRegistry.*`;`pwsh -NoProfile -File tools/scripts/tests/node_command_topic_alerts_contract.tests.ps1`;告警语法用 promtool 另验。
- 批 2、3:先 `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(测试工程链接预编好的 core / gate 库,改了库必须先编库),再编并跑 `routing_identity_test --gtest_filter=RouterSubsetTest.*` 与 `cross_zone_test --gtest_filter=DiscoverySetTest.*:DiscoveryZoneScopeTest.*:EtcdRegistrationWaitTest.*`,两个测试工程其余用例也要全绿(接缝改了节点摘除与 `AddServiceNode` 的公共路径)。用例覆盖:FNV-1a 官方向量与钉住的分数、HRW 取前 k 的顺序与上界、加减一台路由服时只有前 k 名变了的 zone 换人、1000 个 zone 在 10 台与 20 台路由服上的分布(均值 ±20%)、任意路由服规模下每台 gate 至多 k 台、轨道分组与 canary 的 stable 退回、`GATE_ROUTER_SUBSET_SIZE` 解析、对账计划的三种动作、选点分级;每类节点实际看的前缀(scene、两种模式的 gate、battle,本机 13 个与 K8s 11 个前缀两份配置)、配置重复与认不出的前缀、分配键清单解析、最小空闲号、冲突清单只从失败的清单申领里取、快照读守卫每类型只报一次、快照代数。没有单测覆盖、靠冒烟核对的:Node 构造与依赖门的拒启、精确键的 Range 与 watch、分配键 CAS 失败后第二次选号跳过全服已占的号。部署契约另跑 `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`:「发现前缀」用例改为断言 battle-node-config 含 `MatchNodeService.rpc`(battle 唯一的发现类型,没有它 battle 启动即拒启),原来断言 `BattleNodeService.rpc` 的理由已不成立。
- 批 2、3 冒烟:本机 3 个 zone 各 1 台 gate(路由模式)、3 台路由服、`GATE_ROUTER_SUBSET_SIZE=2`。通过标准:① 每台 gate 日志有一行 `Gate router subset installed` 与 `Router subset changed ... members=<2 个 uuid>`,对每台 gate 用 `netstat`/`Get-NetTCPConnection` 数到路由服 gRPC 端口的连接正好 2 条;② 停一台在某 zone 子集里的路由服,该 zone 的 gate 日志出现 `Connecting to a router that entered this zone's subset`,连接数仍为 2,登录与匹配照常;重启它,出现 `Router left this zone's subset ... closing in <期限+1000>ms`,到期后连接数回到 2;③ battle 日志里 watch 只有 `MatchNodeService.rpc` 一条前缀和一行 `Watching key: BattleNodeService.rpc/zone/.../node_id/<本节点号>`,没有 `Watching prefix: BattleNodeService.rpc`;scene 日志里没有 Battle、ClientRpcRouter、Login、Friend、Guild 的 watch;④ 先起 zone 1 的 2 台 scene,再起 zone 2 的 scene:zone 2 的 scene 日志里至多一行 `Node-id allocation conflict`,不再出现连续多次 `etcd registration CAS rejected`;⑤ 把 battle 的 `service_discovery_prefixes` 删到只剩 `GateNodeService.rpc`,battle 启动即以 `Service discovery has no scope to watch` 拒启(验完改回)。
- 冒烟:本机 3 个 zone、3 台路由服、k = 2,核对每台 gate 只连 2 台路由服,每台路由服只拨分给它的 zone 的 login 与 scene_manager。
- 压测按 AGENTS §6,对比连接数、etcd watch 事件量与登录延迟,不预先宣称结论。

## 8. Java 版

2026-11-10 核对(批 1、4 部分):
- Java 没有路由服,`game-gate` 用 `@DubboReference` 直接引用 login、friend、chat、team、guild、trade、mail、rank、match 九个服务(`GateConfiguration`)。Dubbo 消费端默认连每个服务的全部提供者,是同类问题(gate 数 × 实例数)。已登记 Java `PARITY.md`「2026-11-10 连接扇出」条,只登记待做,做法按 Java 惯用方式(自定义 `StateRouter` 取确定性子集加懒连接),不照抄 Go。
- 批 4 的读放大计数不适用:Java 已用 Dubbo 端口取代 gate-cmd / scene-cmd,没有按 `node_id % P` assign 的命令 topic。广播扇出直方图随 Java 自己的推送路径定,未登记。
2026-11-10 核对(批 2、3 部分),都不另登记 `PARITY.md`:
- 批 2(gate 只连本 zone 的 k 台路由服):Java 没有路由服,对应的是 gate 直连九类后端的全连接,已由上面「2026-11-10 连接扇出」条一并登记待做。
- 批 3 的发现集合与 battle 只看自己的键:Java 用 Dubbo 注册中心,消费端只订阅 `@DubboReference` 引用的接口,本来就只发现自己用到的服务;battle 是提供者,不订阅别的 battle,没有 B² 项。已有等价。
- 批 3 的选号:Java 的节点号是 Redis 里按 (节点类型, zone) 的租约(`game-discovery` 的 `NodeIdLease`),同一 zone 内从小到大逐个 SETNX,不是全服 CAS,也没有每次失败等 1 秒,不存在「快照按 zone 收窄后逐个撞别的 zone 的号」的问题。不适用。
