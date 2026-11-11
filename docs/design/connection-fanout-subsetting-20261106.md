# 连接与发现扇出:路由服确定性子集、每类节点只发现自己用到的(2026-11-06)

> 状态:**设计稿,未落码**。对应[服务器代码缺口清单](../handoff/server-gap-audit-20261101.md)第 30 条与第 28 条后半(「连全服全部目标」)。
> 接在[控制面按 zone 化](control-plane-zone-scoping-20261105.md)之后:那份已经把 zone-scoped 类型的发现收窄到本 zone、把兜底 Range 拉长到约 60 秒。本文处理剩下的、随全服规模平方增长的连接与 watch。
> 挑 gate 的规则以「玩家切换zone和gate链接」会话 11-06 的改动为准(`go/shared/gatepick/eligibility.go` 一次分档判定),本文不另定挑 gate 规则。

## 1. 结论

1. **gate → 路由服:按 zone 取确定性子集。** zone z 的路由服集合 = 对 `(z, 路由服 node_uuid)` 做 HRW(rendezvous)打分取前 k 台,k 缺省 3,在同一发布轨道内算。gate 只连、只挑这 k 台。gate 到路由服的连接从 G_全服 × R 降到 G_全服 × k。路由服增减时,只有前 k 名变了的 zone 换路由服,其余 zone 不动。
2. **路由服 → 目标:zone 跟着子集走,全服池再取一次子集。** 稳态下一台路由服只收到分给它的那些 zone 的请求,zone-scoped 目标(login、scene_manager)只会拨这些 zone 的实例。全服池目标(match、chat、friend、guild 等)按 HRW 对 `(路由服 node_uuid, 目标 node_uuid)` 取前 k_t 台,k_t 缺省 4。连接按空闲回收,子集变了之后旧连接自然消失。
3. **gate ↔ scene 的 zone 内全连接保持不变。** 它的上界是 G_z × S_z,随单 zone 规模增长,不随全服 zone 数增长;单 zone 规模由开服容量限定。重新评估的触发条件:单 zone 的 G_z × S_z 超过 10 000 条连接(经验值,待压测),或者做万人同屏的时候(那时整体重做,现在不预留)。
4. **每类 C++ 节点只发现自己用到的类型。** 每类节点显式声明自己的发现集合,与 `CanConnectNodeTypeList` 并列。共享的前缀列表(本机 `bin/etc/base_deploy_config.yaml` 13 个,K8s 模板 11 个)只回答「部署里有哪些前缀」,不再决定每个节点 watch 什么。battle 的集合是 {Match},自身类型只 watch 自己那一条键,不再 watch 全部 battle。
5. **控制面命令 topic 的分区数先不改。** 每个节点只 assign `node_id % P` 那一个分区,读放大等于同分区的节点数。按该 topic 的设计口径(控制面总量每秒几百条),P = 256 时每个节点每秒只解码几条。先加读放大计数,超过阈值再按既有规则换代号、换更大的 P,绝不原地扩分区。
6. **场景亲和先测再定。** 场景广播已经按 gate 分组,每台 gate 一份。亲和只能减少一个场景的玩家分散在几台 gate 上。先在 scene 侧数每次广播涉及几台 gate,压测证明它是瓶颈再做。要做时作为 gatepick 判定的一维(同一档里偏好已有该场景玩家的 gate),不在调用方另加一层过滤。

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

### 3.1 HRW 子集

- 打分:`score = FNV-1a-64(key + "/" + member_uuid)`,取分数最高的前 k 个;分数相同按 uuid 字典序。不用 `std::hash`:它的结果随编译器和平台变,Windows 与 Linux 构建的 gate 会算出不同的子集。这不影响正确性,但会让问题难查。gate 侧(C++)在 FNV-1a-64 之后再过一遍 splitmix64 的终混:HRW 比的是整个 64 位分数,FNV-1a 末尾几个字节对高位的扩散偏弱,加一遍终混是这类用法的常见做法,代价是几次乘法。
- 只有算子集的一方需要这个函数:gate 算 zone → 路由服,路由服算自己 → 全服池目标。两边各写各的,不需要跨语言对拍。路由服无状态,同一 zone 的 gate 在成员变化的几秒内看法不一致也没有正确性问题。
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
  - `DisconnectNode(type, uuid)`:只拆本节点到它的网络实体(先摘 gRPC 通道,再发 `OnNodeRemoveEvent`、销毁实体,与节点摘除共用一段代码),不动发现快照;排到下一轮执行,停机开始后不做,由停机流程统一关。
- gate 的子集模块(`handler/rpc/router_subset.{h,cpp}`,纯计算在 `router_subset_policy.h`)装在这三处:
  - 准入 = 「在当前子集里」。子集按发现快照的代数缓存,快照没变不重算。
  - 观察者里对账:在子集里、已在快照、还没建连的立即建连(服务还没启动时不建,启动时 `ConnectAllNodes` 按准入建);已建连却掉出子集的立即不再被挑,并排一次延后关闭;关闭到期前回到子集的撤销关闭。
  - 延后关闭的时长 = 路由服调用期限(`GrpcClient.CallDeadlineMs.ClientRpcRouterNodeService`)加 lane 的在途过期余量 `kInflightExpiryMargin`(1 秒)。到那时 lane 里发往它的请求都已收到回包或被过期认领;万一还有在途调用,`~Channel` 以 UNAVAILABLE 完成它,lane 按「每个请求恰好一个回执」认领,已过期的直接丢弃。它再进子集时走普通发现路径重新建连。
- 选点:`PickRouterChannel` 改调子集模块的 `PickRouter()`。候选 = 已建连(挂着未关闭的通道)∩ 当前子集 ∩ 轨道合法,再按 `release_track::Select` 取轨道,最后按通道状态分级:READY 优先,其次 IDLE / CONNECTING,最后其余;级内随机。分级是子集的配套:子集只有 k 台,一台进程挂了、租约到期之前它的通道处在 TRANSIENT_FAILURE,不分级就有 1/k 的请求失败,全连接时只有 1/R。
- 依赖门不改:它等的是「发现到路由服」,子集非空当且仅当快照里有本 gate 能用的轨道的路由服。
- 子集里的路由服都连不上(登记还在、进程不响应):照旧回 `kServiceUnavailable`,不临时扩大子集。登记失效(租约过期)后它自然掉出子集,下一名补上。

### 3.3 路由服侧

- zone-scoped 目标的挑选不变。gate 只把本 zone 的流量送给本 zone 的 k 台路由服,每台路由服只会拨约 Z × k / R 个 zone 的实例。
- 全服池目标:`PickRandomForTrack` 的候选改成本路由服在该类型、该轨道里的 HRW 前 k_t 台。每个目标实例约有 R × k_t / T 台路由服连它,R 台路由服负载相近时请求仍然均匀。配置键 `GlobalTargetSubsetSize`,缺省 4。
- 连接缓存加最后使用时间,后台每分钟关掉空闲超过 10 分钟的连接。子集或 zone 分配变了之后,不再被用的连接靠它回收,不会只增不减。节点下线时的立即关闭保持不变。

### 3.4 C++ 每类节点的发现集合

- `Node` 构造多一个参数:发现集合 `DiscoveryNodeTypeList`,与建连白名单并列,由各节点的 `main.cpp` 显式给出。构造时校验发现集合包含白名单(不能连没发现的类型),不满足就 `LOG_FATAL`。
- 实际 Range / watch 的范围 = 配置前缀里属于发现集合的那些(zone-scoped 照旧收窄到本 zone);配置里有、集合里没有的前缀不再 Range、不再 watch。集合里有、配置里没有的类型启动时打一行 WARN:部署里没有这个服务,调用方照旧报找不到。配置前缀表因此只回答「部署里有哪些服务」。
- 自身类型:
  - 在集合里(gate、scene,都是 zone-scoped,只看本 zone 的同类)照旧按前缀;
  - 不在集合里(battle):拿到 node_id 之后(首次 `StartRpcServer`)开始 Range 并 watch 本节点自己那一条发现键。用精确键(range_end 为空),不用前缀:`.../node_id/1` 当前缀会把 10、11、100 号也套进来。劫持检测、注册自检、battle 的可分配状态回读都只看这一条。C++ 节点的 node_id 在进程内不变(重注册沿用原号,CAS 失败即冲突停机),这条键一旦确定就不再变;watch 流重建、兜底重拉、watch 被取消后的重拉都照常带上它。
- 选号不再依赖同类快照:分配键 CAS 的同一个 txn 在失败分支里只取键(keys_only)列出全服这一类型已分配的号,分配器记下这份清单(以最新一份为准),下一次选号把它们排除,规则仍是最小空闲号。第一次选号照旧用快照加本次启动输掉的号;多 zone 部署里第一次多半撞号,多一次 1 秒的重试,之后不再逐个撞。撞号后仍等 1 秒再试:同一时刻启动的一批同类节点每轮只有一个赢家,不等会在几毫秒里打出 N² 个 txn。这同时修掉 §2 记的「收窄后逐个撞号」。
- 选端口不改:K8s 用预设端口、每个 Pod 一个 IP;本机多进程靠绑端口检查与端口键 CAS 兜底,自身类型不在快照里只是少一道预排除。
- 守卫:
  - 构造时发现集合必须包含白名单;
  - `DependencyGate` 要等的类型必须在发现集合里,否则启动即 `LOG_FATAL`(不然永远等不到);
  - 发现快照按类型读(`NodesOfType` / `HasNodesOfType`)读到集合之外的类型(自身类型除外)时,每个类型打一次 ERROR:那份快照恒空,说明集合漏了。
  - 不在 `GetRegistry` 上加断言:直连模式下客户端消息的目标类型来自路由表,可以是本 gate 不连的服务,这是合法的「这里不服务」,Debug 构建里断言会让客户端的一条消息打死 gate。注册表只经建连(白名单,包含于发现集合)和入站握手(scene 的 Gate)写入,别的类型的注册表恒空,调用方照旧报找不到。
- 初始集合(2026-11-10 按代码逐处核对,以代码为准):
  - scene:建连白名单 SceneManager、DataService、PlayerInbox、PlayerStore、Team、Match;Gate(gate 主动连进来,入站登记 `registration_manager.cpp` 查快照,广播用 `ResolveLocalZoneGateEntity`);自身 Scene(本 zone)。
  - gate:路由模式 Scene、ClientRpcRouter;直连模式 Scene、Login、SceneManager、Match;两种都带自身 Gate(本 zone)。
  - battle:Match;自身只看自己的键。
- 效果:所有 C++ 节点不再 Range / watch Friend、Guild;scene 不再看 Battle、ClientRpcRouter、Login;gate 不再看 DataService、PlayerInbox、PlayerStore、Team、Battle,路由模式另不看 Login、SceneManager、Match;battle 之间的 B² 项消失。gate、scene 仍看本 zone 的同类,随单 zone 规模增长,与 §1 第 3 条同一口径。

### 3.5 命令 topic 的读放大

- 消费端加计数 `node_command_consumed_total{node_type, target}`,target 取 own(目标是本节点)或 foreign(同分区的别的节点),标签只有这两个值。
- 告警条件(经验值,待压测):单节点 foreign 每秒超过 50 条,且 foreign 与 own 之比超过 50,持续 10 分钟。
- 处理办法沿用 `node_command_topic.h` 的契约:换代号、换更大的 P(2 的幂),生产端与消费端按同一配置一起切换,不在同名 topic 上扩分区。

### 3.6 场景亲和(先测)

- scene 侧加直方图 `scene_broadcast_gate_fanout`(每次场景广播涉及几台 gate)。按场景类型分桶,不按场景号打标签。
- 压测中如果它长期接近 G_z,而且 gate 的入站广播是瓶颈,再做亲和:作为 gatepick 判定的一维,在同一档里偏好已经有该玩家目标场景玩家的 gate。前提是 login 在 AssignGate 时知道玩家要去的场景(落点或上次位置),那时另写设计。

## 4. 失败与一致性

- 子集变化只会让路由服开始收到新 zone 的请求,第一批请求多付一次建连(毫秒级)。路由服不持有会话状态,请求之间的顺序由 gate 的会话 lane 保证(`client-request-ordering.md`),与挑哪台路由服无关。
- 滚动重启路由服:每台重启使成员变两次。HRW 只移动前 k 名变动的 zone,其余 zone 的连接不动。
- 全服池子集:目标实例重启或增减,只影响把它排在前 k_t 的那些路由服。
- 发现集合漏了某个类型:建连类型漏了构造即拒启,依赖门要等的类型漏了启动即拒启;别的读者读到的快照恒空并打一次 ERROR,调用方照旧报「Node not found」。所以落码时仍要逐处核对(§3.4)。
- gate 子集变化:掉出子集的路由服先停止被挑,过调用期限加 1 秒再拆连接,在途请求照常收尾;拆连接不动发现快照,它再进子集就重新建连。同 zone 的 gate 在成员变化后的几秒内看法可能不同,只会让某台路由服短暂多收一个 zone 的请求。
- battle 自身改看精确键:本节点登记被删(租约过期)仍由这条 watch 的 DELETE 进快照,注册自检照常发现「快照里没有自己」并重注册;watch 流断开期间的变化由流重建后的重拉对账。
- 选号清单是那次 txn 时刻的快照,之后被别人拿走的号照旧靠 CAS 失败发现;清单只用来排除,不用来认定「这个号空着」。

## 5. 被否决的做法

- **按 zone 部署路由服**(路由服进 zone namespace):Pod 数随 zone 数增长,小 zone 也要至少 2 台。HRW 在共享池上得到同样的局部性,一台挂了自动由下一名接手。
- **带虚拟节点的一致性哈希环**:k 小、成员在百台以内时,HRW 更简单,O(成员数)重算,不用维护环。
- **etcd gRPC proxy 合并 watch**:发现集合收窄之后用不上;进程数到 10⁵ 量级时再评估。
- **gate 不 watch 路由服、改由配置给地址**:失去自动扩缩与故障摘除。

## 6. 分批与文件

| 批 | 内容 | 文件 | 依赖 |
|---|---|---|---|
| 1 | 路由服全服池子集与空闲连接回收 | `go/client_rpc_router/internal/discovery/node_watcher.go` 及测试、配置 | 无 |
| 2 | gate 的路由服子集:HRW、准入 / 观察 / 拆连三处接缝、延后关闭、选点分级 | 新增 `core/node/system/discovery/rendezvous_subset.h`、gate `handler/rpc/router_subset{_policy}.h`、`router_subset.cpp`;改 `node.h/.cpp`、`service_node_snapshot.h`、`service_discovery_manager.cpp`、`node_connector.cpp`,gate `main.cpp` 与 `client_session_handler.cpp` 各一两处,gate 与测试工程的 vcxproj;用例 `cpp/nodes/gate/tests/router_subset_test.cpp` | 无 |
| 3 | 每类节点的发现集合;选号读全服分配清单;battle 只看自己的键 | `node.h/.cpp`、`node_entry.h`、`service_discovery_manager.h/.cpp`、`service_node_snapshot.h`、`etcd_service.h/.cpp`、`etcd_helper.h/.cpp`、`etcd_manager.cpp`、`node_allocator.h/.cpp`、三类节点的 `main.cpp`、gtest | 改引擎公共代码,所有 C++ 节点受影响 |

- 2026-11-10 负责人改派:批 2、3 由「缺口第 30 条:gate 连接与解码扇出」会话做;批 1、4 仍归「服务器代码缺口补完」。
| 4 | 计数:命令 topic 读放大、场景广播 gate 扇出 | 命令消费处、`client_message_sender.cpp`、Prometheus 规则 | 无 |

- `node_command_topic.h` 另有会话的未提交改动(观战 topic),本设计不改这个文件。
- 新增 C++ 源文件只登记 `.vcxproj` 与 `.filters`,CMakeLists 由它生成。

## 7. 验证(交 Codex)

- 批 1:`cd go/client_rpc_router && go build ./... && go vet ./... && go test -race ./...`。HRW 用例覆盖:分布均匀(1 000 个 key、R = 10、k = 3,每台承担的 key 数在均值 ±20% 内);增减一台时,只有前 k 名变了的 key 移动;空闲回收用注入时钟。
- 批 2、3:`msbuild game.sln /m:1`;gtest 覆盖子集计算、准入钩子、延后关闭、发现集合断言。
- 冒烟:本机 3 个 zone、3 台路由服、k = 2,核对每台 gate 只连 2 台路由服,每台路由服只拨分给它的 zone 的 login 与 scene_manager。
- 压测按 AGENTS §6,对比连接数、etcd watch 事件量与登录延迟,不预先宣称结论。

## 8. Java 版

未核对 Java 版 game-gate 与路由层的连接方式。落码时核对,有同类问题再登记 Java `PARITY.md`;不适用或已有等价就不登记。
