# 全类型节点：平滑停机、水平扩展与灰度／金丝雀发布需求

> **需求日期**：2026-10-23。负责人要求：“先别改代码……所有类型节点都可以无感关闭、水平扩展、灰度发布、金丝雀发布吗？这个需求也记录到文档。”
> **状态**：需求已记录；两版均未完成全类型验收。本次只读核对源码并修改文档，没有实现、构建、测试、压测或部署。
> **核对基线**：C++/Go 仓 `2c07f2c28`、独立 Java 仓 `e16d2f9c` 加核对时各自工作区。并行变更仍在进行，本文事实不代表运行中的部署已经具备对应保证。

## 1. 目标与适用范围

所有实际部署的应用节点都应支持：增加实例分担工作；计划关闭或缩容时先停止接新工作、完成或交接存量工作；兼容版本并存；灰度与金丝雀发布能够控制影响范围、观察结果、暂停和安全回退。

范围包含 C++ gate、scene 的主世界／副本／跨服角色、battle，Go login、client_rpc_router、player_locator、scene_manager、db、data_service、match、guild、friend、chat、mail、rank、trade、player_inbox，以及本仓 Java HTTP gateway；独立 Java 版按自己的模块承担同样的业务目标。组队能力按实际承载模块归类；当前 `go/team`、`go/battle` 仅有生成常量等辅助内容，不能据目录名算作已经部署的节点。

完整类型、实际入口、枚举外应用与两套类型编号的区别见 **§3.4–§3.6**。本需求覆盖所有实际承担业务的应用；预留类型将来启用时也必须逐类验收，不能只交付 Gate／Scene／Battle 或上一次点名的几个 Go 服务。

`eNodeType` 中仅预留枚举、没有实际模块的类型不算已实现；将来启用也须满足本需求。Redis、Kafka、etcd、SQL 数据层纳入可用性与容量依赖，但按各自集群协议升级、搬迁和扩缩容，不能把应用层玩家分流原样套到存储副本上。

项目仍未上线，按 [存盘围栏设计的上线状态](db-write-fencing-mysql.md) 判断当前引导范围；本需求是后续上线与持续运营的目标，不要求为不存在的线上旧部署补历史迁移。

## 2. “无感”与各项能力的含义

| 能力 | 本需求的目标 | 必须区分的边界 |
|---|---|---|
| 平滑关闭／缩容 | 无需玩家手动重新登录；资产、会话归属与结算不丢、不重；在线玩法连续性达到约定预算 | 进程正常退出、完成存盘或机器人 TCP 数量不变，均不能独自证明玩家无感 |
| 水平扩容 | 新实例就绪后承接实际工作，容量或吞吐可以随合理分片增加 | 新房间／新频道分配与搬迁已有活跃世界是两项能力；副本数增加不等于热点实体或底层写吞吐增加 |
| 灰度发布 | 按可解释、稳定的业务范围逐步启用新版或新功能 | 普通滚动替换只决定换实例顺序，不自动提供稳定的玩家分组 |
| 金丝雀发布 | 先用少量真实业务验证新版，满足观测门槛才扩大，异常可暂停或收回新流量 | 金丝雀是渐进发布的一种方式；只开一个新版实例或设置不参加选主还不构成完整闭环 |

这里的业务无感目标允许将来评估客户端自动恢复连接的方案，但**尚未认定一次自动重连就满足无感**。Gate 的 TCP 属于原进程，替换进程不会自动把连接搬到另一台；若要求物理连接完全不断，须另行评估接入架构与连接迁移，当前没有这项保证。本次未读取或修改独立客户端，后续涉及客户端实现须在对应任务中授权并验收。

发布前须确定自动恢复的允许中断时长、请求成功率与延迟、允许等待的排空时间、对局连续性、扩容目标负载与金丝雀观察窗口。本次没有给定数值，不能自行填入数字后宣称满足需求。

计划维护、强杀／宕机、网络隔离分别验收。计划下线应优先完成交接；崩溃路径只能依赖已经设计并验证的恢复机制，不能沿用计划排空的结论。

## 3. C++/Go 版当前做到哪里

下表是源码／配置核对，不是本次运行验收。原有多实例总纲见 [消除单节点](no-single-node-horizontal-scaling-20261001.md)，具体业务不变量继续由各专题负责。

| 节点／模块 | 已有基础 | 本需求仍须补齐或证明的部分 |
|---|---|---|
| gate | 多实例分配、排空标记；external 入口为每实例地址，StatefulSet 使用 `OnDelete`，有逐台排空脚本 | 普通停机逐条关闭连接；排空主要等人数自然减少，没有统一的透明会话搬迁。`drained=deadline` 可能仍有玩家，阈值也可以大于 0；脚本不负责缩容，不能把标记或退出码当作零连接证明 |
| scene（含主世界／副本／跨服角色） | 多实例与频道分配；单场景有 `BeginSceneDrain`、存盘和改派确认；存在可选 Agones 扩缩容实现 | 普通节点停机走玩家 `kNodeShutdown` 存盘退出，尚未串成全部存量场景的在线交接。均衡器只搬空频道；活跃世界状态搬迁未实现，回主世界也不等于原玩法连续 |
| battle | 多实例建房；Agones 排空标签能关闭新建房准入，存量房间可以继续运行 | 普通停机会 `AbortAllRooms`、断开直连；须形成“拒新局→原局结束→终态结果耐久接管／投递完成→零房间→退出”的维护流程，并单独验收 |
| login、client_rpc_router、Java HTTP gateway | 有多副本、发现／分配或转发基础，适合按请求分担工作 | 在途请求、业务就绪与退出预算须逐模块核实；router 当前随机选目标，尚无统一版本／玩家分组选择；Gateway 探针或副本数不能证明业务排空 |
| player_locator、scene_manager | 多副本承接请求，后台部分任务由 leader 执行；scene_manager 有 `LeaderEligible=false` 的金丝雀数据面模式 | leader 换主空窗与容量边界另算；已有选主开关未覆盖全链路版本分流，不能据此宣称所有控制面均可金丝雀 |
| db | 消费组分区所有权、顺序与归属围栏、重试收据归属和退出处理，多实例部署基础已具备 | 消费并行度受固定分区数约束；入退组可能暂停消费。须证明 rebalance／关停中的在途任务、ACK、补投与恢复；不能靠盲目增加副本或原地增加分区保证无感 |
| data_service | 多实例、共享存储及幂等／围栏基础，有装配就绪检查与后台选主 | 逐条证明消费者、生产者、leader 与存储连接的关停顺序；源码已记录 go-zero 退出时限与 Kafka flush 上限不匹配，不能保证每次完整排空 |
| match、team、guild、friend、chat、mail、rank、trade、player_inbox | 按各域已有存储、队列、锁／租约和幂等机制支持或推进多实例；mail／rank 已有独立入口与多副本清单 | 逐域核对队伍／公会／匹配任务等状态所有者、定时任务和消费者交接，不能一律当无状态。无统一的全类型维护与版本分流验收；金融／资产类失败语义不得为无感而放宽 |
| Redis、Kafka、etcd、SQL | 已有或正在推进高可用、分片和 TiDB 路线 | 高可用副本不等于物理写扩展；核心 Redis 仍有共享原子域约束。集群成员变更、数据搬迁、版本兼容与失去多数派的边界需独立验收 |

### 3.1 可以定位的事实入口

- Gate 普通退出：`cpp/nodes/gate/main.cpp` 的关闭钩子；排空原因在 `go/login/internal/logic/pkg/loginqueue/gatedrain_monitor.go`，操作脚本为 `tools/scripts/k8s_gate_drain.ps1`。契约与超时边界见 [集群外入口](k8s-client-entry.md) D83、D87 及 Gate 排空章节。
- Scene 普通退出：`cpp/nodes/scene/main.cpp`；单场景排空：`cpp/libs/services/scene/world/scene_lifecycle.cpp` 的 `DestroyScene`；空频道迁移限制：`go/scene_manager/internal/logic/world_rebalance.go` 的 `RebalanceWorldChannelsForZone`。
- Battle 普通退出：`cpp/nodes/battle/main.cpp` 的关闭钩子与 `logic/battle_room_manager.cpp` 的 `AbortAllRooms`；新建房准入见 `handler/grpc/battle_node.cpp`。
- 版本分流：`proto/common/base/common.proto` 的 `NodeInfo` 当前没有发布轨道；`go/client_rpc_router/internal/logic/forwardlogic.go` 的 `pickTarget` 只按全局／zone 随机挑选；`go/scene_manager/scene_manager_service.go` 的 `LeaderEligible` 仅约束参选。
- 退出预算：`go/data_service/data_service.go` 的关停顺序注释；mail／rank 使用的 `go/shared/domainhost/host.go` 明确有界退出，超时不能算完整排空。
- 存储扩展：见 [Redis 物理扩展需求](../handoff/redis-horizontal-scaling-requirements-20261022.md)、[全局数据层 TiDB 决策](global-data-layer-tidb-decision.md)。

2026-04-04 的 [滚动／重启实验](rolling-update-restart-resilience-tests.md) 只作历史材料。其“杀 Scene 无影响”主要观察连接与消息计数，不能替代当前资产、世界状态、对局和新旧版本端到端验收。

### 3.2 db、login、guild、friend 的单独结论（同日追问补充）

负责人继续追问“db、login、guid、friend 这种呢”。`guid` 尚未明确指公会 `guild` 还是 GUID 发号，因此两者都覆盖，不据拼写替负责人做最终解释。

| 服务 | 多实例与状态归属 | 已有的正常下线处理 | 当前能作的结论／发布边界 |
|---|---|---|---|
| **go/db（玩家存档消费者）** | 同 zone 多实例通过同一 Kafka consumer group 分担分区；重试 processing 按实例隔离并带租约，可归还／回收。持有分区的实例数受固定分区数限制，实例内部另有子分片 | 已有实际业务排空：撤拉取→等待已派发任务并提交连续完成位点→离组→停 worker→归还未完重试收据→最后关持久入队层；主进程专门处理 go-zero 重发的终止信号 | 可以多实例，也有计划停机基础。成员增减会再平衡，可能短时停消费；SQL／Redis／网络耗时总和仍可能超过 Pod 退出预算，不能承诺任意负载零停顿。新版消费者获得的是 Kafka 分区，不能由副本比例推导精确的玩家灰度比例 |
| **login** | 玩家登录／会话权威主要在 Redis、SQL 与 player_locator，不要求玩家固定黏在某个 login；进程内仍有预加载任务、异步结果等待、排队 dispatcher 与发号缓冲 | `ServiceContext.Stop()` 会停止后台监视、发号客户端、结果 dispatcher、排队和预加载池；入口仍以 go-zero 启停与 defer 为主，不能把 `RpcServer.Stop()` 当作整个业务链已排空 | 可以多实例；替换 login 不会直接关闭 Gate 上已进场玩家的 TCP。但正在登录、建角、排队、冷加载或续令牌的请求仍可能受影响，须验证撤流／在途完成／取消与安全重试。历史“无状态”只说明无需实例亲和，不等于没有待完成工作 |
| **guild（公会）** | 有共享 SQL 权威、版本／事务／租约及资产操作的幂等基础，可以多副本承接；并非要求把整个公会固定在一个常驻内存 actor 上 | 已有资产通道、清理、历练调度／结果消费者等 stop，入口通过 defer 编排；没有 friend 同等明确的注销→真实 RPC 排空→后台与资源关闭协调流程 | 可以多实例，不必搬迁 Gate 式玩家连接；仍须闭合存量 RPC、资产任务、活动开局／结果的持久接管及退出预算。资产幂等能防重复提交，不证明玩家请求没有失败。公会／活动灰度应保持同一业务归属的兼容性 |
| **friend（好友）** | 全服多副本使用同一 SQL 事务域；申请、双向边与容量在事务里提交，不依赖特定副本长期保存玩家权威状态 | 已有显式流程：注销尝试返回→真实 `grpc.Server.GracefulStop`（最多 5 秒）→关闭后台／依赖→框架收尾；可恢复的终态清理循环仅随信号取消、不等待退出。24 秒硬截止超时会报错／退出，明确不保证剩余在途完成 | 具备较完整的请求级平滑退出基础，可以作为同类服务的参考。仍须验收发现传播、超时长请求、提交结果不明与通知；不能由已有排空推导全链路灰度已完成 |

四类都纳入本需求，成熟度并不相同。没有玩家长连接要迁移，通常比 Gate／Scene 容易完成平滑维护；持久提交后响应丢失、后台任务接手和混版本语义仍须单独处理。客户端／调用方只能重试已明确幂等的操作，不能盲目重试建角、捐献等变更。

源码定位：db 的 `go/db/db.go`、`internal/kafka/key_ordered_consumer.go` 的 `Stop`／`claimAcker.ack`／`persistBeforeAck`、`retry_ownership.go`；login 的 `go/login/login.go`、`internal/svc/servicecontext.go` 的 `Start`／`Stop`；guild 的 `go/guild/guild.go` 与域内 repo／后台循环；friend 的 `go/friend/friend.go` 的 `runFriend`、`internal/lifecycle`。这些均为本次静态核对，未新增运行证据。

这四类均尚无统一的版本／稳定业务分组分流与金丝雀观测／中止闭环。尤其 db 必须遵守 [分区与任务契约](db-task-kafka-partition-contract.md) 和 [持久世代发布顺序](mixed-version-persist-field-protection.md)：兼容的版本可以安排滚动，破坏性协议变更不能直接混跑；安全守卫的拒绝／延后不算无感成功。guild 新实例加入同一结果消费组或竞选维护任务后也会处理全局工作，入口只分 1% 请求并不能把全部副作用限制在 1%。

### 3.3 如果 guid 指 GUID 发号

发号是能力，当前没有名为 `guid` 的独立部署节点。主仓的永久 ID 逐种走 `data_service.AllocateIdSegment`，调用方在本地消耗不重叠号段；SQL 事务锁定对应 `biz_tag` 行并推进水位，支持多个 data_service 实例分配。停掉一个调用方可能浪费剩余号码，这与重号不同；服务或调用方重启不能把剩余号段重新发给另一实例，也不能降低已分配水位。多实例可扩接入，同一 tag 的领段事务仍有串行点，不能据此承诺无上限吞吐。

login 当前建角优先号段，号段耗尽且续段不可用时按配置明确失败；Snowflake 回退须显式允许。仍走 Snowflake 的种类按独立 worker 租约、水位与失去所有权时的发号围栏保证安全，扩实例不能复制 worker 身份。源码在 `go/data_service/internal/store/id_segment_store.go` 的 `Allocate`／`tryAllocate`、`go/login/internal/svc/player_id_minter.go` 与发号装配；设计见 [ID 改造方案](node-id-overhaul-plan-20260908.md)。发号数据兼容、耗尽／退出与恢复路径同样要验收。

独立 Java 版没有主仓 go/db 存盘消费者，也明确没有移植 data_service 号段服务；本版 scene 直接受 owner epoch 保护写 SQL，永久 ID 走 Snowflake＋节点租约。Java login／friend／guild 已有各自工作池的有界排空，guild 资产任务另有停止领新任务与等待手中任务的处理；仍未证明全链路无感发布。上述机制不能跨版本仓库互相充作验收证据。

### 3.4 全部运行时 eNodeType 盘点（同日“所有其他节点”追问）

事实源为 `proto/common/base/node.proto`。目前共 **29 个枚举项：Unknown=0，加 28 个非 Unknown 类型**；其中 **17 类已有应用角色实现、2 类基础设施标识、9 类尚无独立常驻入口的预留角色**。这些计数是源码分类，不是正在运行的 Pod 数，也不是通过维护验收的数量。Team／Match 共进程，不能把 17 类直接当作 17 个独立进程。

下表逐项覆盖这 28 类。“可多实例”只记录实现或部署基础；所有应用类型的全链路灰度／金丝雀仍须遵守 §4 和 §5，不能由本表直接判为验收通过。

| 值 | 运行时类型 | 实际入口／当前角色 | 扩容与正常关闭的已有能力及缺口 |
|---|---|---|---|
| 3 | `SceneNodeService` | `cpp/nodes/scene`，主世界／副本／跨服场景 | 可多实例、频道分配、单场景排空；节点存量在线交接及活跃世界完整迁移未闭合 |
| 4 | `GateNodeService` | `cpp/nodes/gate`，客户端长连接 | 可多实例分配与自然排空；普通退出关闭连接，超时标记不是零连接证据 |
| 5 | `LoginNodeService` | `go/login/login.go` | 可多实例，无需玩家亲和；登录、建角、排队与异步预加载的在途流程须单独排空／恢复，详见 §3.2 |
| 6 | `RedisNodeService` | Redis 基础设施标识 | 按 Redis 拓扑做故障转移、分片与搬迁；副本高可用不等于写吞吐水平扩展，不能套玩家金丝雀 |
| 7 | `EtcdNodeService` | etcd 基础设施标识，也用于传输超时选择等内部契约 | 按成员、租约与多数派协议维护；应用多开仍依赖发现／发号租约安全，不是多开普通应用进程 |
| 8 | `MailNodeService` | `go/mail/mail.go` | 已有多副本及 `domainhost` 注销／RPC 有界排空；SQL 领取与资产 outbox 由租约接手，后台 4 秒等待超时不能宣称已排空 |
| 9 | `ChatNodeService` | `go/chat/chat.go` | 已有多副本、先注销后 RPC 排空；当前授权、请求原件及历史以 SQL 为权威，提交不明须沿用原请求 ID 恢复，仍需维护效果验收 |
| 10 | `TeamNodeService` | `go/match/match_service.go` 同进程注册的组队服务 | 随 match 多实例；与 Match 同端口、同启停并共同注销，尚不能独立扩缩容或独立发布 Team |
| 11 | `ActivityNodeService` | 无独立活动节点入口；已有部分活动协议／调度在 Scene 或 Guild 内 | 类型预留不表示活动功能全无；若以后独立部署，必须另验活动时序、唯一调度与状态交接 |
| 12 | `TradeNodeService` | `go/trade/trade.go`，交易与商城／资产适配 | 已有多副本、SQL 状态／outbox／租约及注销→RPC 排空→资产通道停止；硬截止不保证所有在途完成，待办须持久保留接手 |
| 13 | `RankNodeService` | `go/rank/rank.go` | 已有多副本与 `domainhost` 排空；现役为 SQL 榜代次、封榜快照、奖励意图，奖励租约与邮件幂等接手；后台退出预算仍需验收 |
| 14 | `TaskNodeService` | 无独立任务节点入口；玩家任务逻辑在 Scene 域内 | 预留角色，当前任务状态随实际承载 Scene 的存盘／维护保证判断，不能称已有独立 Task 节点发布能力 |
| 15 | `GuildNodeService` | `go/guild/guild.go` | 已有多副本、事务、任务租约与结果消费者；注销、RPC、资产／历练后台的退出编排仍需闭合，详见 §3.2 |
| 16 | `MatchNodeService` | `go/match/match_service.go` | 已有多副本、SQL 成局原件／工作租约、注销及 RPC 等待；执行中成局任务的取消／接手可能等租约，不保证立即无感恢复 |
| 17 | `AiNodeService` | 无独立 AI 常驻节点入口 | 预留；已有玩法内的 AI／NPC 能力不能据此算成已验独立 AI 进程 |
| 18 | `LogNodeService` | 无该类型的独立游戏节点入口 | 预留角色；现有 Alloy／Loki 日志采集走基础设施部署，不是此枚举服务已落地 |
| 19 | `PaymentNodeService` | 无 `go/payment` 常驻入口；[充值设计](payment-recharge.md) 已规划独立支付服务 | 处于设计／待实施范围；`java/mall_game` 订单应用不等于此支付角色。未来须验支付回执、幂等、退款与后台义务接手 |
| 20 | `SecurityNodeService` | 无独立安全节点入口 | 预留；现有鉴权、签名与限流在各模块，不代表此独立节点已实现或已支持发布 |
| 21 | `CrossServerNodeService` | 无该独立节点入口；跨区能力由 Scene 与现有控制面协作 | 预留角色，不表示跨区业务不存在；现有跨区维护按实际所有者和协议验收 |
| 22 | `AnalyticsNodeService` | 无独立分析常驻节点入口 | 预留；新增时再定义消费位点、聚合状态、重复与回放语义，不计为当前弹性能力 |
| 23 | `GmNodeService` | 无独立 GM 常驻节点入口；管理操作分布在 HTTP／各域管理 RPC | 预留角色，不等于没有 GM 功能；管理操作的资产与归属门禁仍归实际承载模块 |
| 24 | `PlayerLocatorNodeService` | `go/player_locator/player_locator.go` | 已有多副本、Redis 会话、断线租约认领与对账 leader 主动让位；尚缺统一有界的摘流／RPC／所有后台退出协调验收 |
| 25 | `SceneManagerNodeService` | `go/scene_manager/scene_manager_service.go` | 多副本处理 RPC，后台调度由 leader 执行并有停机让位；已有不参选的数据面金丝雀，尚未覆盖后台新逻辑与稳定业务分组 |
| 26 | `DataServiceNodeService` | `go/data_service/data_service.go` | 多副本 RPC、消费组与 SQL 租约对账；已有注册／注销／后台收尾，Kafka flush 与框架退出预算冲突仍是维护边界 |
| 27 | `FriendNodeService` | `go/friend/friend.go` | 多副本 SQL 权威与显式注销／RPC 排空，基础较完整；超时、提交不明与通知仍待维护演练，详见 §3.2 |
| 28 | `BattleNodeService` | `cpp/nodes/battle` | 多实例建房、Agones 准入排空；普通停机仍作废存量局，须闭合等局结束与结果接管流程 |
| 29 | `ClientRpcRouterNodeService` | `go/client_rpc_router/client_rpc_router_service.go` | 无业务权威状态，可多实例转发；有框架关停但应用层摘流／在途转发协调待验，现有选点没有版本／权重／稳定分组 |
| 30 | `PlayerInboxNodeService` | `go/player_inbox/player_inbox.go` | 多副本 SQL 收件箱、流顺序与消费围栏；有注销／RPC 排空和有界后台停止，对账 leader 接任可能等租约，不能声称零空窗 |

`UnknownNodeService=0` 是无效／未知默认值，不是要部署的第 29 类服务器。`eSceneNodeType` 中 4 种主世界／副本／跨服角色都由 Scene 类型承载；`PROTOCOL_TCP/GRPC/HTTP` 是通信协议，也不新增应用节点类型。

核对依据为各入口实际注册、`deploy/k8s/manifests/go-svc` 的有效服务清单及对应业务存储／退出代码。旧 `go/README.md` 尚未列出 mail／rank，泛用 `using`、生成绑定中的枚举定义或旧配置目录也不构成节点已实现证据。

### 3.5 枚举之外仍须纳入的应用与基础设施

| 入口／模块 | 实际身份 | 本需求如何覆盖 |
|---|---|---|
| `go/db/db.go` | 玩家存盘消费者，有独立常驻进程；不在运行时 `eNodeType` 中 | 按 §3.2 的消费分区、位点、SQL 提交与重试接手完整验收，不因枚举缺项而漏掉 |
| `java/gateway_node` | 正式 HTTP 门户；有效部署在 `manifests/java-svc/gateway.yaml`，已有双副本 | HTTP 在途、会话／管理操作与依赖就绪分别验收；端口探针或副本数不证明零失败滚动 |
| `java/mall_game` | 有实际 Spring Boot 入口的商城 HTTP 应用，持有订单权威 | SQL 并发／命令幂等与 nonce 去重已有基础，但统一镜像目录和 K8s 清单尚未接入；须补正式部署、撤流、在途订单／资产义务的关闭与版本兼容验收 |
| `java/config_node` | 可运行查表／热重载辅助应用，也是导表产物落点，日常不启动 | 不当作已部署游戏节点；若正式多副本启用，要证明相同表版本与一致的热重载，单进程 `loadVersion` 不提供跨副本一致性 |
| `java/springboot_satoken_auth_starter` | 有实际 Spring Boot main 的本地认证示例 | 可运行示例不等于正式登录节点或已经接入部署的认证服务；其本地 H2 等状态不提供正式多副本保证。当前产品登录按 Go login 的链路验收 |
| Kafka、SQL／TiDB／MySQL、Alloy／Loki，以及 Agones 等部署控制器 | 应用依赖与运维基础设施，未必出现在 `eNodeType` | 各自按集群协议、持久性、容量、故障转移与兼容升级验证；不能用应用请求 1% 分流替代基础设施变更验收 |
| `go/shared`、`go/proto`、`go/schemamigrate`、生成的辅助模块、`cpp/nodes/_template` | 公共库、生成物、迁移库或模板 | 不算独立部署节点；其变更随真实依赖它的进程构建、发布与验收 |

商城、配置查询与认证示例不因缺少类型值而被自动排除，也不因目录里有 main 就被认定已纳入正式部署。`k8s_deploy.ps1` 中残留的 auth 目录表条目与磁盘缺失的 `auth.yaml` 不能作为已完成部署的证据。本轮不修改这些实现或目录表。

### 3.6 两套“NodeType”不能混数或混用编号

仓库还有 `proto/db/proto_option.proto` 的 **`NodeType`，共 32 个选项标签（0–31）**，供 `OptionFileDefaultNode` 等协议生成选项使用。它与运行时 `eNodeType` 是两套编号，例如 `NODE_LOGIN=10`，但 `LoginNodeService=5`；不能互相强转，也不能把两个枚举相加当成实际节点总数。

其中 `NODE_CLIENT` 是客户端标签，`NODE_UNSPECIFIED` 是默认值；`NODE_CENTRE`、`NODE_CENTRE_SCENE`、`NODE_DEPLOY`、`NODE_DB_SERVICE` 等保留旧架构标签，当前没有据此对应的独立服务入口。Centre 已退役；`NODE_DB` 与实际 go/db 生成／任务职责相关，不代表运行时枚举存在同名一项。这些协议标签的存在不自动恢复旧节点，也不能据名字相近把 `NODE_DB_SERVICE` 认定为当前 data_service。

反过来，当前 Friend／DataService／PlayerInbox 等运行时角色不能仅靠该旧选项枚举完整盘点。Friend 与 PlayerInbox 的 proto 注释明确解释了缺少对应 option 值时由生成器按服务名／包／目录解析的路径；最终以实际生成映射、注册、监听与启动入口为准。本文只记录这一区别，不改类型契约。

后续能力台账应始终覆盖“全部实际应用入口＋每个已启用的运行时类型＋基础设施依赖”；新增类型、拆分或合并进程、启用预留角色时更新本表并逐类留下维护与发布证据。一个共享进程不能只验证其中某一种类型就声称全部通过。

## 4. 后续设计必须给出的契约

### 4.1 统一可观察的维护过程，各节点保留自己的排空方式

建议用“准备→服务→排空→已排空→停止”描述维护状态；这只是待实现的共同语义，不是宣称已有统一状态机，也不要求给所有节点套同一个实现框架。

1. **准备**：依赖、契约和容量检查完成才允许接新工作，新实例启动成功不等于业务就绪。
2. **排空**：先停止新的连接分配、场景／房间创建、任务认领和相应后台工作；原有状态所有者仍完成存量请求。新工作准入与已有玩家／任务路由要分开，不能一注销发现就让未交接的存量状态失联。
3. **交接**：按角色选择自然完成、持久恢复或在线迁移，明确提交点、owner epoch、幂等、迟到消息与旧实例复活的围栏。不得复制相同 node 身份，定向 Kafka 命令继续携带目标实例 UUID。
4. **已排空**：以业务事实确认，不以 sleep 或标记到期确认。Gate 要核实真实连接／交接完成；Scene 要核实玩家、场景状态与所需存盘／改派提交证据；Battle 要核实房间和终态投递义务；消费者要核实在途与可恢复的持久接管。
5. **停止**：业务、RPC、存盘／发件箱与传输关闭预算必须一致；K8s 宽限期覆盖整个过程。严格无感模式超时应暂停本轮替换、保留旧实例并告警；强杀属于另一个显式接受影响的操作，不能记为排空成功。

扩容应计入旧实例、新实例及退出中实例的资源和连接峰值。缩容前证明剩余容量与故障余量足够；只有 leader 的后台任务须说明接任、积压恢复与未来分片方式。多副本不能自动提升单个热玩家、公会、队伍或活跃世界的处理上限。

### 4.2 灰度与金丝雀按业务归属保持一致

- 明确发布版本／轨道、协议与配置版本、持久 schema 能力，以及允许混跑的版本组合。实例 UUID 只识别进程，`persist_schema_gen` 只描述持久数据覆盖世代，二者都不能代替发布版本。
- 按已确定的业务单元选择灰度范围：普通玩家会话可按玩家／账号分组，组队和对局按完整队伍／整局，Scene 按完整状态所有权域；不能把同一有状态流程中的请求随机拆到新旧所有者。具体粒度由后续设计确定。
- 分流须贯穿入口、路由服、调度、下游及异步任务；仅入口 1% 或副本比例不能证明实际业务暴露比例。共用全局任务、数据与副作用须说明影响范围，不能默认与稳定版本隔离。
- 先扩大兼容的读写能力，再启用新行为，最后收紧旧路径。停止给新版分配新工作后，仍须完成新版已有会话／对局／任务；回退必须重新满足同一交接契约。
- 按版本／轨道观察错误率、延迟、登录恢复、场景迁移、Kafka lag、存盘拒绝／死信、对局作废和结算完整性。只使用有界低基数标签，不把玩家 ID 作指标 label。定义扩大、暂停、中止与安全回退的判据。

### 4.3 数据与回滚门禁沿用既有权威设计

[混版本持久字段保护](mixed-version-persist-field-protection.md) 的发布顺序、未知字段携带、覆盖世代、配置值域保护及 scene 回滚限制继续适用。不能把“字段没被覆盖”推导成“旧版能正确解释新版业务”；fail-closed 可以保住数据，同时仍会拒绝业务请求，两项指标要分别验收。

涉及持久 schema 升世代时，按该文档约定先 DDL、go/db 全部 zone、login，再 scene；不能为了“所有节点都能金丝雀”打乱依赖顺序。guild／mail／trade 等独立持久域也要给出自己的兼容合同。回滚二进制不自动撤销 DDL、新配置或已经发生的资产副作用；不满足回退条件时应暂停或向前修复，不能承诺一键任意回退。

## 5. 验收清单与实施顺序

每类实际部署节点分别登记实现、单测、真实依赖验证、维护演练与容量证据；任何一项缺失都不能由其他节点的通过替代。

| 场景 | 必须保留的证据与通过条件 |
|---|---|
| 带存量业务扩容 | 新实例就绪后实际承接工作；原业务连续；固定总负载与逐级增长负载分别比较成功率、延迟和容量，资源／连接预算不超限 |
| 主动关闭一个实例／缩容 | 无新工作进入待退实例；存量完成或原子交接；没有手动重登、丢资产、重复资产、双所有者或非预期对局作废；退出前有业务排空证据 |
| 排空失败／超时／并发发布 | 依赖不可用、长对局、延迟存盘 ACK、旧实例复活、排空标记过期等情况下保持不变量；暂停与恢复可重入，不能假报完成 |
| 新旧版本并存与小流量扩大 | 实际流量归属可核实；同一业务单元不被随机拆散；同步／异步路径兼容；稳定版本影响范围和持续观察门槛明确 |
| 中止／回退 | 停止新版新流量后存量有去向；符合持久数据与配置值域回退门禁；拒绝不安全回退并保留数据及诊断证据 |
| 强杀／网络隔离 | 单独记录中断与恢复时间、RPO／RTO 和恢复正确性，不冒充计划维护无感 |

建议按四步推进：先完成逐类型能力盘点并约定体验预算；再闭合 Gate／Scene／Battle 及消费者的维护与排空；随后补版本分流、混版本兼容与金丝雀控制；最后按两版分别进行真实依赖、在线维护、故障恢复和容量验收。本次只记录需求，未启动这些实施步骤。

## 6. 独立 Java 版对齐

Java `0.1.0-SNAPSHOT` 同样纳入目标，按本版的 Netty、Dubbo 和存储协议实现，不与 C++/Go 混部，不复制内部协议。

核对时 `game-gate` 的 `GateNode.stop()` 关闭所有会话；`game-scene` 的 `SceneNode.release()` 关闭链路并做最终写回；`game-battle` 的 `BattleNode.release()` 关闭准入并作废房间。已有 Gate 排空也允许 deadline，不能当零连接证明。跨节点疏散及正式 K8s 发布仍有待做项（Java `docs/porting/roadmap.md` 5.5、7.6），没有全类型灰度／金丝雀闭环验收。

Java 架构文档追加本需求入口，`PARITY.md` 登记“需求已记录、实现与整体验收待完成”。两版共用业务目标和验收口径，各自保留实现与证据；仅完成本文不算功能对齐完成。

## 7. 平台行为参考

- Kubernetes 的 [Pod 终止流程](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination) 给应用发送终止信号并提供宽限期，超时可以强杀；会话与业务排空仍由应用完成。因此只配置 `preStop`／宽限期不能证明本需求成立。
- Kubernetes 的 [金丝雀部署说明](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#canary-deployments) 说明可为不同版本创建独立 Deployment；本项目还须完成上文的业务归属、版本路由、数据兼容与观测门槛。
