# 断线重连：处理原则与各服务现状

> 更新：2026-11-06。证据：当前工作区静态核对（HEAD `f6fddb466` 加当时其他会话的未提交改动）。审计结论由三路只读代理给出，第 4 节标「已复核」的条目已回源码逐条核对。没有编译，没有跑断网、切主或故障转移演练，不能当运行验收。行号按当天工作区，漂移时以函数名为准。
>
> 本文替换 2026-04-05 的英文旧版。旧版里的 centre 已退役；`MessageAsyncClient`（10-30 `fc504300b` 删除）、`ScheduleWatchReconnect`、go/db 的 Kafka 重试队列都已不存在；「etcd 续约失败即 `LOG_FATAL`」也早已改成重新注册。
>
> 范围：服务端进程对外部依赖的断线与重连，包括 Redis、MySQL / TiDB、etcd、Kafka、服务间 gRPC、节点间 muduo 链路、Agones。玩家客户端的断线重连见 [断线重连恢复审计](disconnect-reconnect-audit-20261020.md) 与 [断线保留](player-connection-lost-hold.md)。Java 版服务器（独立仓库）的核对见 §7，本文原则同样适用。
>
> 2026-11-06 第二轮（负责人：「全部按优先级修」）：R1、R2、R3、R5、R8、R9、R10、R11、R12、R14、R16 和新发现的 R20 已落码，R4 复核后降为低危，未编译、未测试，待 Codex 验证；R6、R7、R13 等要先定设计的留到下一批。逐条状态见 §4.1。下面 §0~§4 的现状描述保留第一轮核对时的原文。
>
> 2026-11-06 Codex 续验仅更新 R11：负责人确认全球 Match 坏结果应保留位点并告警，取代历史“跳过并提交”建议，见 §4.1 和 `cross-zone-matchmaking.md` §13.4。位点保护定向 4 条由红转绿；Match build、vet、完整普通测试通过（438 条顶层通过、178 条外部环境用例跳过）。不代表本文其余重连修复、真实故障演练或真 TiDB 专项已经通过。

## 0. 结论

**连接都会自己恢复。** 所有服务、所有依赖断开后都能重新连上，不需要重启进程：

| 依赖 | 谁负责重连 | 状态 |
|---|---|---|
| Redis | C++：自写 `RedisManager`（单实例）与 `RedisClusterClient`（玩家域）；Go：go-redis / go-zero；Java：Lettuce | 都能重连。C++ 有一处例外：主机名在启动时解析失败，之后就一直连不对（§4 R9） |
| MySQL / TiDB | Go：database/sql 连接池 + `authoritysql` 守卫；Java：Hikari；C++ 不直连 | 能重连，但守卫让连接池没法悄悄换掉失效的空闲连接（§4 R2） |
| etcd | C++：自写 muduo_h2 传输；Go：clientv3 + 9 份注册实现；Java：jetcd 轮询 | 都能重连，失租后都会重新注册。Go 的 watch 有静默停住的窗口（R1），注册实现之间行为不一致（R3、R4） |
| Kafka | librdkafka / kafka-go / sarama | 都由库重连；Go 消费处理完才提交 |
| 服务间 gRPC | C++ muduo_grpc；Go zrpc；Java grpc-java | 都按需重连；每次调用带截止 |
| 节点链路（gate↔scene RPC0） | `RpcClient` / `RpcChannel` | 握手核对实例身份，心跳 5s、判死 15s（11-05 落码；11-08 Codex 编译通过、17 个心跳单测全过；冒烟未跑，见 [muduo-rpc-channel.md](muduo-rpc-channel.md) §21.11） |

**没做好的是另外两件事**：一是断线那一刻在途的调用怎么收场（结局未知怎么判、由哪一层重试）；二是「持有类状态」（选主锁、注册身份、watch 视图）在失联期间还可不可信。§4 列出 19 个缺口：高 2 个、中 11 个、低 6 个。已核实的条目里，没有会写坏权威数据（资产、存档、ID）的，这些都有权威库一侧的围栏兜底；但 R7 的下游是否全部兜住待确认，R13 会丢审计流水。两个高危影响的都是成片的可用性：一个是看不到节点上下线，一个是数据库重启后业务成批失败、排空被卡住。

**服务逻辑该怎么处理**，一句话：连接坏了由连接层修，业务只回答「这一次调用没成功怎么办」；重连成功本身不是业务事件（负责人 2026-11-05 定）。展开见 §1。

## 1. 处理原则（所有服务、所有依赖通用）

### 1.1 分层：连接层修连接，业务层处理这一次调用

| 层 | 负责 | 不做 |
|---|---|---|
| 连接层：驱动与客户端库，以及本仓的 `RedisManager`、`RedisClusterClient`、`RpcClient` / `RpcChannel`、muduo_grpc 通道、各注册与发现组件、`kafkautil` | 建连；判死（心跳、keepalive、读写超时）；退避重连（指数、封顶、带抖动）；握手（AUTH、核对实例身份、会话初始化）；维护就绪状态；在新连接上重建连接级状态（订阅、watch 续看、脚本缓存、会话变量）；连接状态的指标与日志 | 补发业务消息、业务全量同步、遍历玩家、改业务待办 |
| 业务层 | 每次调用带截止时间；按调用结局分别处理（§1.2）；只对能重放的操作做有界重试（§1.3）；结局未知交给幂等键、账本、有主协议收口；资产与身份路径 fail-closed | 关心「什么时候重连好了」；在 OnConnected、握手成功、重连回调里做事 |

为什么这样分：

- 服务一多，「连上某类服务就做 X」会随链路数成倍增长，还会带出启动和恢复的先后依赖。负责人以前在这上面踩过坑。
- 两端进程都活着时，各自的内存状态仍然有效，断线只会丢掉在路上的消息。在路上的消息本来就该由业务协议兜底：进场待办、断线租约、owner_epoch、outbox、对账循环。
- 实例是否消失只看权威发现（etcd 租约摘除）。进程重启一定换 uuid，按新实例处理。

允许的「重连后动作」只有连接层自己的事：重新 AUTH、重放会话变量、重新订阅、watch 续看或全量重拉以重建视图、刷新集群拓扑、发出排队中的调用。

### 1.2 一次调用只有三种结局

1. **成功**。
2. **确定没执行**：可以在预算内重试。包括：
   - 连接未就绪、本地拒发（`kNotSent`）、建连失败；
   - 对端在执行前拒绝，例如 gRPC `REFUSED_STREAM`，或驱动返回 `ErrBadConn`（database/sql 的约定：只在服务端不可能执行过时才返回它）；
   - 显式事务里 COMMIT 发出之前的任何失败（库会在会话结束时回滚未提交的事务）。
3. **结局未知**：请求已写出后断线或超时、COMMIT 已发出后断线、Kafka 投递超时。非幂等操作不能原样重发，三选一：带幂等键重发；回读核对；把「结局未知」交给业务对账（客户端看到 tip 1003）。

断线瞬间在途的调用，一律按第 3 类处理。连接层只保证每个调用**恰好收到一次结局**，不替业务猜它执行没有。对应的实现：C++ `RpcChannel` 的 `kDisconnected` / `kChannelClosed`、hiredis 的空应答、muduo_grpc 的 `UNAVAILABLE`。

反过来，也不要把第 2 类误判成第 3 类。确定没执行的失败如果被标成未知，本来可以自动重试的操作就会被挂起，等人工处理（§4 R2、R6）。

### 1.3 重试：只有一层负责，按截止时间算

- **只有一层重试**。驱动自带的重试和业务重试叠起来会成倍放大，还会重发结局未知的命令。各驱动的缺省值：
  - go-redis、go-zero：`MaxRetries` 缺省 3；
  - sarama：`Retry.Max` 缺省 3；
  - kafka-go：缺省 10 次。

  go-redis v9 在读超时或 EOF 后，会把**已经写出的命令**原样重发（见模块缓存 `go-redis/v9@v9.17.3/redis.go` 的 `_process` 和 `error.go` 的 `shouldRetry`）。所以非幂等命令所在的句柄有两种做法：设 `MaxRetries: -1`（`shared/playerredis` 已这样做）；或者让脚本自己识别重放（`scene_manager/internal/logic/owner_epoch.go` 的「重放识别」）。
- **预算按截止时间算**，不按「次数 × sleep」。退避要指数增长、封顶、带抖动，避免依赖恢复的那一刻所有实例同时打过去。
- **只重试可重试的错误**：一是确定没执行的失败；二是服务端明确表示可以整笔重试的码，例如 MySQL 1213（死锁）、1205（锁等待超时），TiDB 9007（写冲突）。
- **不阻塞事件循环**：
  - C++ loop 线程上不等重连：未就绪就当场失败，或用定时器排下一次；
  - Go 用 ctx 截止；
  - Java 每一层都要有超时：建连、取连接、socket 读、命令。

### 1.4 持有类状态：租约、锁、选主、注册身份

分两种情况：

- **正确性靠持有的**（选主锁、写者身份、发号预留）：
  - 续不上约时，按**本地单调时钟**在租约到期之前（TTL 减安全余量）停止以持有者身份行事。不能等重连成功才发现「租约早过期了，身份已被别人拿走」。
  - 重新获得持有就是新的一代：新租约、新 epoch。
  - 权威存储一侧必须有围栏，例如 `owner_epoch`、`role_fence` 写者闸门、发号器的三份 CAS 记录。这样即使短暂双持有，也写不坏数据。
- **只用于路由与可见性的**（节点注册）：续不上约时可以继续服务，前提是下面三条都成立：
  - 数据写入有权威围栏（存档会被判 `STALE_OWNER`）；
  - ID 唯一性不靠租约（永久 ID 走号段与发号器）；
  - 重新注册用 CAS 判定身份归属，发现被别人占了就排空退出。

  为什么不自停：etcd 整体不可用时，所有节点一起自停就是全服停机，而那时根本没有人能接管。代价是被接管时丢一小段进度，用「身份不确认就加密存盘」来缩短（C++ 已做，见 §3.1 etcd）。

Redis 里的锁和占位只能当协调提示：异步复制的主从切换会丢掉已确认的写。凭 Redis 锁做的变更，要么本身幂等，要么带 fencing token 到权威库做条件写。

### 1.5 订阅与 watch 是提示，不是真相

- Redis Pub/Sub、etcd watch 事件、Kafka 提醒类消息，断线期间都会丢。依赖它们的业务必须同时具备可回源的权威数据和周期对账，例如资产待办的后台重投、收件箱的在线周期拉取。
- **watch 要防静默停住**，下面三件一起做（login 的 gate 目录 `pkg/node/watcher.go` 是现成写法）：
  - 用 `clientv3.WithRequireLeader`：所连成员失去 leader 时流会报错，而不是一直不给事件；
  - etcd 客户端开 gRPC keepalive（`DialKeepAliveTime`），用来发现半开连接；
  - 周期全量对账，或者「静默太久就重新同步」。
- 断流后从 revision 续看、遇到 compaction 就全量重拉，这是发现层维持自身视图的职责，不算业务的重连钩子。
- 视图重建只更新视图。由视图变化引出的业务动作（摘除、收尾、rebalance）由各自的定时节拍驱动并保持幂等，不挂在「watch 刚重建」上。
- 消费方要不怕重复和乱序：处理幂等、按实体 key 有序、比较版本号或 epoch。

### 1.6 启动时依赖不可用

- **身份与发号类硬依赖**（etcd 注册、node_id 分配、发号器）：拿不到就不对外服务。要么进程不就绪，要么退出，交给编排带退避重启。不带着未确认的身份启动。
- **数据与消息类依赖**（TiDB、Redis、Kafka、对端 gRPC）：每个服务只选一种写法，并在配置注释里写明。要么有界等待后退出；要么照常启动，让依赖它的请求按截止失败。下面两种中间状态都不允许：
  - Ping 失败只打一行日志就照常接单，到第一笔业务才出错；
  - 对端服务没起就退出：服务之间互相等，就成了冷启动时的连锁 CrashLoop（§4 R5）。
- **地址要能重新解析**：重连时重新解析主机名或重新查发现，不要只在启动时解析一次（§4 R9）。

### 1.7 探针

- **liveness 不含任何外部依赖**：依赖坏了，重启自己修不好，只会制造重启风暴。
- **readiness 只反映本实例能不能接新流量**：所有副本共用的依赖挂了，把副本全摘掉不会让任何请求成功。上游反而拿不到明确的错误，依赖恢复后还要等探针。所以共用依赖不进 readiness。Java 网关已按此配置（见 `java/gateway_node/src/main/resources/application.yaml` 的 management 段注释）。

### 1.8 可观测性

- 每种依赖至少有三项：连接状态、重连次数、按原因分的失败次数。
- 日志按状态变化打（断开、重连成功、握手失败附连续失败次数），不按每次失败的调用打；高频失败要采样。
- 指标 label 不含 `player_id` 等高基数值（AGENTS §9）。

### 1.9 断线不等于数据回退

重连到一个刚完成故障转移的 Redis 副本，或者一个从备份恢复的库，看到的可能是旧数据。连接层分辨不出这一点，正确性靠权威存储的围栏与回档规则（[存盘围栏](db-write-fencing-mysql.md)、[整 zone 回档清理](zone_data_rollback_discard_tool.md)），不靠重连逻辑。

### 1.10 评审清单

新写或评审任何连接外部依赖的代码，逐条问：

1. 断线时，在途调用会不会**恰好一次**收到结局？结局分清了「确定没执行」和「未知」吗？
2. 谁负责重试？是不是只有一层？非幂等操作的句柄关掉驱动的自动重发了吗？
3. 每次调用都有截止时间吗？截止从哪里来？
4. 租约、锁、身份续不上时，什么时候停止以持有者身份行事？权威库一侧有围栏吗？
5. 订阅或 watch 断流期间丢的事件，靠什么补？会不会静默停住？
6. 有没有 OnConnected、握手成功、重连后补发 / 全量同步一类的业务钩子？有就删掉，改成各自的定时节拍或有主协议。
7. 依赖在启动时不可用，进程是退出、不就绪，还是带病接单？和 §1.6 的规则一致吗？
8. 重连时会重新解析地址吗？退避带抖动吗？连接状态有指标吗？

## 2. 各依赖的统一做法

| 依赖 | 连接层负责 | 业务层负责 |
|---|---|---|
| Redis | 重连；AUTH；EVALSHA 遇 NOSCRIPT 时回退 EVAL；Pub/Sub 重新订阅；集群的 MOVED/ASK 处理与拓扑刷新 | 非幂等命令要么关掉驱动重试，要么让脚本识别重放；锁和占位只当提示，正确性落在权威库；不可用时，资产、登录、身份路径 fail-closed，限流、配额这类保护手段可以 fail-open，但要计数 |
| MySQL / TiDB | 连接池换掉坏连接；配建连、读、写三个超时；`ConnMaxLifetime` / `ConnMaxIdleTime` 短于链路上断开空闲连接的时间；新物理连接上重放会话初始化 | 只对可重试的错误整笔重试事务，且事务要幂等；COMMIT 结局未知时按幂等键或回读核对；提交点锁本库的 `role_fence` |
| etcd | 续约；续不上约按 §1.4 处理，作为新的一代重新注册；watch 带 RequireLeader、keepalive 和周期对账；compaction 后全量重拉 | 只凭 etcd 摘除判定实例消失；不把「还连着 etcd」当成仍持有 |
| Kafka | 消费组重新入组、rebalance、断线续读；生产者库内重试 | 处理幂等，处理完再提交 offset；坏消息记录后跳过，不卡住分区；关键消息不 fire-and-forget，失败要么返回给调用方，要么落 outbox |
| 服务间 gRPC | 通道重连；按发现结果换实例；keepalive 参数与对端的强制策略一致 | 每次调用带截止；只对幂等方法重试，非幂等方法带幂等键 |
| 节点链路 RPC0 | 建连、握手双向核对实例、置就绪、心跳判死 | 用链路前先查就绪；在途调用的结局按 §1.2 处理；丢掉的消息由进场待办、断线租约等业务协议兜底（[muduo-rpc-channel.md](muduo-rpc-channel.md) §8.3、§21） |

## 3. 现状（2026-11-06 工作区）

### 3.1 C++ 节点（scene / gate / battle）

**Redis**

C++ 有三个 Redis 句柄：
- `tlsRedis`：核心实例。gate、scene、battle 都会连，但业务上只有 scene 在用。
- `tlsSceneControlRedis`：只有 scene 连。
- `tlsPlayerRedis`：玩家域的集群客户端。

- **单实例**（`engine/thread_context/redis_manager.cpp` 的 `StartHandshake`、`ScheduleReconnect` 和存活探测）：
  - 先装好回调再建连；
  - 断线后固定每 3s 重建一次，不带抖动，在 pending 阶段执行并按代次复核；
  - 配了口令时，AUTH 回 `+OK` 才把连接交给业务；
  - 每 5s 发一次 PING，15s 没有应答就摘掉这条连接。
- **集群**（`engine/infra/storage/redis_cluster/redis_cluster_client.cpp`）：
  - 每个主节点一条连接，重连间隔 3s，加至多 1s 抖动；
  - MOVED/ASK 立即重发，最多 5 次；
  - 确定未执行的错误按 50ms→1s 退避，单条命令预算 10s；
  - 拓扑在遇到重定向、空应答或节点就绪时刷新，另有 30s 一次的周期刷新；
  - 全部节点断开 5s 后，回到种子节点重新开始。
- **断线时的命令**：
  - 新命令当场返回 `REDIS_ERR`，不调回调；
  - 在途命令以空应答结束，表示结局未知，不重放。这些命令可能晚于新连接上的命令执行，靠 Lua 里的围栏兜底；
  - 生产代码一律用 EVAL 带脚本原文，所以不会出现 NOSCRIPT；
  - C++ 没有 SUBSCRIBE，只 PUBLISH `assetop:wake`。
- **Redis 不可用时各业务的选择**：

  | 业务 | 选择 |
  |---|---|
  | 进场归属登记 | 有界重试，之后拒绝进场 |
  | handoff 标记撤回 | 确认撤回之前，拒绝该玩家换图 |
  | 同步摘要 | 返回 `UNAVAILABLE`，让客户端全量拉 |
  | 资产唤醒 | 直接丢弃，回落到资产通道自己的重投 |
  | 分线目录、神秘商人 | 降级 |
  | 玩家存盘 | 走 gRPC，不依赖 Redis |
- 缺口：R17（连接状态只有日志）、R18（重连没有抖动）未补；R9（主机名只解析一次）、R14（重连回调里做业务）已修，状态见 §4.1。

**etcd**（muduo_h2 传输，官方 gRPC 实现已删除）

- **watch**（`engine/core/node/system/etcd/etcd_service.cpp`、`muduo_etcd_transport.cpp`；验收条件见 [grpc-muduo-transport.md](grpc-muduo-transport.md) §10.2，没有运行证据）：
  - 断开后按 0.2s→5s 无限重建；
  - 重建后逐个前缀重新 Range 对账，再从快照 revision+1 建 watch；
  - 遇到 compaction 立即重拉；
  - 有 HTTP/2 PING（30s / 10s）和每 60s 一次的兜底全量 Range，所以 watch 静默停住最多持续约 60s。
- **续约**（`etcd_service.cpp` 的 `OnKeepAliveResponse`，`node.cpp` 的冲突关停）：
  - 已不再 `LOG_FATAL`；
  - 应答 TTL≤0，或本地超过 TTL 收不到应答，就拿新租约重新注册，node_id 不变；
  - 重新注册时发现分配键已属于别的 uuid，排空至多 15s 后正常关停。
- **失联超过 TTL**：继续服务，按 §1.4 的第二种情况处理（`services/scene/player/system/player_lifecycle.cpp` 的 `RunIdentityAtRiskSaves`，取舍见 [数据一致性等级核对](data-consistency-guarantee-map-20261020.md) §6）：
  - 超过半个租约没收到应答，就视为身份不确认；
  - 这时停写 A1′ 凭证，并每 5s 给所有可改的玩家补存一次，被接管时最多丢这一段进度。
- **启动**：etcd 不通就一直等。node_id 取最小的空闲号并走 CAS，没有可用的号时 FATAL。
- 缺口：R19（只用 Hosts 第一项）。

**Kafka**（librdkafka）

- **消费**：
  - 全部 assign 固定分区，不加入消费组，所以没有 rebalance；
  - 不提交 offset，每次启动从分区末尾读。理由：节点命令按实例投递（`target_instance_id`），进程重启后就是新实例，旧命令本来就不该再执行；
  - 断线由库重连，按内存里的位点续读。
- **生产**：
  - 幂等生产者，库内重试到 `message.timeout.ms`（缺省 300s）；
  - 队列满时 poll 一次，再试一次；
  - 遇到 fatal 错误重建实例；
  - 最终失败只打 ERROR，没有死信，也没有 outbox；
  - battle→scene 的开局确认有业务层补发（每 10s 一次，窗口 180s）。
- 缺口：R13（审计流水与玩家快照会永久丢）、R19（启动时拿不到元数据会阻塞 loop）。

**gRPC 客户端**（muduo_grpc，用来调 Go 服务；`engine/muduo_grpc/channel.cpp`、`http2_transport.cpp`）

- 地址只来自 etcd。对端从 etcd 摘除时就删掉通道，在途调用以 `UNAVAILABLE` 结束。
- 断开后按需重连。`TRANSIENT_FAILURE` 期间新调用当场返回 `UNAVAILABLE`，退避 1s×1.6，封顶 120s。
- 每次调用必带截止（缺省 10s）。只在能证明对端没处理时，透明重试一次。
- 截止到期而连接没有动静时发 PING，20s 收不到 ACK 就判连接已死。
- 主要调用方：
  - 存盘只挑就绪节点；结局未知时用同一个序号重试（0.5→5s），靠 save_seq 幂等；
  - 领号段按 500ms→5s 重试；
  - 战斗终局重放同一份原件。

**节点链路 RPC0**：代码与 [muduo-rpc-channel.md](muduo-rpc-channel.md) §8.3、§21 一致：握手 3s，失败后关闭连接、退避 0.5→8s，入站核对 `expected_peer_uuid`，心跳 5s、判死 15s。旧的 `SetHandshakeSucceededHook` 全仓已无命中。

**Agones**：`/health` 每 2s 发一次，失败只打 WARN；事件循环卡死时停发 `/health`，让 Agones 判为不健康。缺口：R10（`/ready` 放弃后不再重试）。

### 3.2 Go 服务

**Redis**

| 句柄 | 客户端与重试 | 启动 | 用在哪 |
|---|---|---|---|
| `shared/playerredis`（玩家域；login、guild、trade、match、friend、mail、scene_manager、player_locator 等都有一份） | 单机或 Cluster；拨号、读、写、取连接各 1s，认 ctx；`MaxRetries: -1`；集群节点挂钩子，管道里拒绝会被重发的命令 | 各服务自定 | 会话、位置、在线态等 |
| login 的核心、排队、SaToken 句柄 | go-redis 单机，用配置的超时，不认 ctx，`MaxRetries` 缺省 3 | Ping 失败就 panic | 排队脚本能识别重发 |
| guild、trade、player_locator、data_service（召回）的共享句柄 | go-redis 单机，用库的缺省值（拨号 5s、读写 3s、重发 3 次） | Ping 失败就 panic 或拒绝启动 | `assetopwake` 订阅、`shared/leader` 选主 |
| player_inbox 的核心句柄 | go-redis 单机，认 ctx，重发 3 次 | 带超时 Ping，失败拒绝启动 | 维护任务选主 |
| scene_manager | go-zero（重发 3 次、带熔断）；铸造 owner_epoch 用专用的 pinned 客户端，自管重发并带 WAITAOF | go-zero 不探活，带病启动 | 场景认领脚本用 token 识别重发 |
| match、friend、chat、data_service（NameCache） | go-zero，没配集群时回落到共享单机 | 不探活，带病启动 | 配额 Lua、旧缓存 |

- **Pub/Sub**：只有 `shared/assetopwake` 一处用。go-redis 的订阅每 3s 做一次健康检查，断线后自动重新订阅；断线期间丢的唤醒消息不补，由资产重投循环的退避兜底。只重新订阅、不补发，符合规则。
- **Lua**：全仓没有裸 `EvalSha`，只用 EVAL 原文或 `Script.Run`（自带 NOSCRIPT 回退）。
- **Redis 不可用时**：登录与会话、口令失败配额 fail-closed；资产投递按 outbox 退避重投；帮会申请配额、好友申请配额 fail-open 并计数。
- 缺口：R7、R8、R19（超时口径不一、部分句柄带病启动）。

**MySQL / TiDB**

- **建池**：除 login 的口令库外，所有连接池都经过 `shared/authoritysql`：
  - 强制 `parseTime`，拨号 1s，读写 5s；
  - `MaxOpen = MaxIdle`，`ConnMaxLifetime` 15min，不设 `ConnMaxIdleTime`；
  - 每次建连、每次从池里复用连接，都重放会话变量（RC、pessimistic、`tidb_replica_read=leader`、`innodb_lock_wait_timeout=1`），并核验主体、库、世代与 boot 登记，所以重连后会话状态不会悄悄丢掉；
  - go/db 另配 5/30/30s 的超时和 5min 的 `ConnMaxLifetime`，外层套同一个守卫。
- **COMMIT 出错**：守卫对 COMMIT 的任何错误都标「结局未知」并隔离这条连接（`driver.go` 的 `endLocked`）。业务靠这些收口：
  - 存档按 `save_seq` 幂等；
  - 资产按 `op_id` 加 outbox；
  - 帮会用请求去重表；
  - rank 用 `RequestId`。

  凡是不能证明「确定拒绝」的错误，`serverbase.PublicationAdmission` 都会调 `Hold()`。之后这个实例的排空证明一直过不去，要人工处理（`shared/serverbase/interceptor.go`、`shared/drainproof/report.go`）。
- **可重试错误没有统一封装**：
  - `assetop.WithTxRetry`（trade、guild、player_inbox）认 1213、1205、9007，有次数上限、指数退避加抖动、受截止约束；
  - guild、rank、data_service 各有一份口径不同的判定；
  - 8022、8028、9001、9005 全仓都没有处理。
- **连接级状态**：业务路径不用 `GET_LOCK`、临时表、服务端预编译。`GET_LOCK` 只在迁移路径上用，固定在一条连接上；断线后后续 DDL 直接报错，不会在没有锁的情况下继续。
- **写者闸门与角色护栏**：都是事务内的行锁，断线时随回滚释放。闸门冻结时返回 `ErrWriterFrozen`，表示确定没写。
- 缺口：R2、R6。

**etcd**

注册有 9 份实现，行为不一致：

| 实现 | 使用方 | 续约通道关闭后 | 停机删键 |
|---|---|---|---|
| `shared/noderegistry` | chat、friend、trade、player_inbox、db（PlayerStore）、match（TeamNode）、domainhost（rank、mail） | 一个循环处理：申请新租约，用一笔 Txn 重新夺回原号，被占就换号；1s→30s 退避，可打断 | 按 uuid 条件删 ✔ |
| `internal/noderegistry` ×4 | client_rpc_router、data_service、scene_manager、match（MatchNode） | 首次或重启续约时报错就 return，之后不再续约（R4）；只比较 `Version==0` | 无条件删（R3） |
| `guild/internal/node`、`player_locator/internal/node` | guild、player_locator | 同上 | 无条件删，且没有超时（R3） |
| login 的 `pkg/etcd` + `pkg/node` | login | 拿到新租约后逐个 key 重新挂上；原号被占就退出进程 | 无条件删（R3） |
| go-zero Publisher（`*.rpc` 键） | 所有 zrpc 服务 | 每 1s 重新注册一次 | — |

- **失租期间**：各实现都继续服务，按 §1.4 的第二种情况处理。永久 ID 的唯一性不靠租约：
  - `idissuer` 每次预留都按主、副、登记处的顺序对三份记录做 CAS，任何一步失败，整块作废；只发已经预留到手的块。fail-closed ✔
  - `snowflakealloc` 失租时不停发，先用水位 revision 证明这段时间没人用过，再收回；确认被别人占用时才围栏并退出。
- **watch**：
  - clientv3 遇到 `Unavailable` 会在内部从上次的 revision 续看，断流期间的事件不会丢；遇到 compaction 时，各处都会全量重新同步。
  - 防静默停住的三件措施（§1.5）：
    - 只有 login 的 gate 目录和 `shared/dbplacement` 开了 `WithRequireLeader`；
    - 只有 login 的 etcd 客户端开了 keepalive；
    - 只有这两处有周期全量或静默重同步。
  - 其余几处都可能静默停住（R1、R12）：scene_manager 的 LoadReporter、match 与 client_rpc_router 的发现、`shared/scenenode`、`shared/killswitch`。
- 缺口：R1、R3、R4、R12、R15、R18。

**Kafka**

- **消费**：全部走 `shared/kafkautil` 的 `BoundConsumerGroup`：
  - 入组、拉取、提交失败时，1s→30s 退避加抖动，原地重试；
  - 处理成功才提交；
  - 一代结束时，没提交的记录交还 Kafka 重投；
  - 已提交的位点越过保留期时 HOLD。

  各消费者：data_service（流水、快照）和 guild（历练结果）幂等，坏消息跳过并提交；match 的对局结果遇到坏消息会原地无限重试（R11）。
- **生产**：

  | 服务 | 发送方式 | 失败时 |
  |---|---|---|
  | login 控制命令 | 同步，`WaitForAll` | 返回错误 |
  | scene_manager 路由命令 | 同步 | 回滚 EnterScene，返回可重试的错误 |
  | player_locator 租约清理 | — | 不 ack claim |
  | match 对局结果 | 走 SQL outbox | 由 outbox 重投 |
  | 在线提醒类 | 至多一次 | 由 SQL 补发 |

  另外：没有服务开幂等生产者；定向命令在目标实例 uuid 为空时拒发。
- **broker 全部不可用时**：login、match、guild（开启历练时）拒绝启动；其余服务照常启动，运行中按上面各自的方式失败。

**服务间 gRPC**

- **发现**：多数走 etcd 发现（go-zero resolver：遇到 compaction 重新 load，连接恢复就 reload）。mail、rank 的本地配置是直连地址。
- **NonBlock**：go-zero v1.10.0 的 `NonBlock` 缺省为 true，v1.9.2 缺省为 false。login 和 player_locator 用的是 v1.9.2。login 的 `DataServiceRpc` 已显式写了 `NonBlock: true`，`PlayerLocatorRpc`、`SceneManagerRpc` 没写（R5）。
- **重试**：全仓没有 gRPC 层的重试，熔断器缺省打开。业务重试带幂等键：
  - EnterScene 按 request_id 去重；
  - ReleasePlayer 带 expected_owner_epoch；
  - 资产走 outbox 加 seq。
- **keepalive**：全仓没有配 zrpc 客户端 keepalive。遇到半开连接时，每次调用都要耗满 Timeout 才失败。

### 3.3 Java 网关（`java/gateway_node`）

- **MySQL**（`config/AuthorityDatabaseConfiguration.java`）：
  - Hikari 池最大 5 条、最小空闲 2 条，取连接 3s；
  - JDBC URL 带 `connectTimeout=3000&socketTimeout=10000`；
  - 每条物理连接都经 `AuthorityDataSource` 核验 TiDB 强会话。
- **Redis**：只给限流用，`gate.rate-limit.enabled` 缺省关（`ratelimit/RateLimitConfig.java`、`RedisRateLimitStore.java`、`AssignGateRateLimiter.java`）。用一个专用的 Lettuce 客户端：
  - 命令超时 300ms；
  - 断线期间命令立即失败（`REJECT_COMMANDS`）；
  - 未完成命令上限 1024；
  - EVALSHA 遇到 NOSCRIPT 就带全文重试一次；
  - 出错时 fail-open 并计数。
- **etcd**（`etcd/GateWatcher.java`、`grpc/LoginNodeDiscovery.java`、`service/ZoneHealthProbeService.java`）：
  - 不 watch，也不持租约，每 5s 带超时轮询一次；
  - 查询失败时不清表，保留最近一次成功的路由；
  - 健康快照超过 15s 就返回 UNKNOWN，而不是「全区维护」；
  - 没有需要重建的连接级状态，天然合规。
- **gRPC 调 login**：每次尝试单独计算截止（3s）。Login、AssignGate、QueryQueueStatus、RefreshToken 都有副作用，都不自动重试，失败直接上抛。
- **探针**：liveness 和 readiness 都不含外部依赖；K8s 上探针走独立的管理端口。
- 缺口：R16（动态通道的空闲 ping）、R19（`etcd.dial-timeout-ms` 没有生效）。

## 4. 缺口与修法

- 「已复核」表示已回源码核对过；其余是审计结论，修之前先复核。
- 严重度按 AGENTS §11 的取舍顺序定：会写坏数据的才算高。本表的两个高，都是成片的可用性问题。

| 编号 | 严重度 | 位置 | 场景 → 后果 | 修法 |
|---|---|---|---|---|
| R1 | 高 · 已复核 | Go 的 etcd watch：[scene_manager LoadReporter](../../go/scene_manager/internal/logic/load_reporter.go#L890)（etcd 客户端 [servicecontext.go](../../go/scene_manager/internal/svc/servicecontext.go#L103) 也没开 keepalive）、[match 发现](../../go/match/internal/discovery/node_watcher.go#L138)、[client_rpc_router 发现](../../go/client_rpc_router/internal/discovery/node_watcher.go#L182)、[shared/scenenode](../../go/shared/scenenode/watcher.go#L175) | 所连的 etcd 成员和 leader 分区，或者那台成员掉电、留下半开连接 → watch 流既不报错也不来事件 → scene_manager 看不到 scene 节点上下线：死节点继续被选为落点、不做收尾，新节点不被使用；match 死实例名下的匹配池没人撮合。一直持续到分区恢复或 TCP 超时 | 三件一起加：`clientv3.WithRequireLeader`；etcd 客户端 `DialKeepAliveTime: 10s`、`DialKeepAliveTimeout: 5s`（与 login 同值）；周期全量 Range 对账，或静默太久就重新同步。现成写法见 [login watcher.go](../../go/login/internal/logic/pkg/node/watcher.go#L179) 和同文件 L219。最好收成一个公共 watcher 给各处复用 |
| R2 | 高 · 已复核 | [authoritysql ResetSession](../../go/shared/authoritysql/driver.go#L416) | TiDB / MySQL 重启或切主，或者链路上的 LB 断掉空闲连接之后：<br>① 驱动在复用连接前的存活检查里报 `ErrBadConn`，守卫把它改写成「结局未知」；<br>② database/sql 只有收到 `ErrBadConn` 才换连接（Go `database/sql/sql.go` 的 `conn()`），于是把这条坏连接照常交给业务；<br>③ 每条失效的空闲连接让一次业务调用失败，每池最多 `MaxIdle` 条，最长闲置 15min；<br>④ 这些失败又被 `PublicationAdmission` 记成结局未知并调 `Hold()`，排空证明永久计数，要人工处理。<br>每次例行的 TiDB 滚动升级都会触发 | `ResetSession` 里，底层返回 `ErrBadConn` 时原样返回 `ErrBadConn`：这时一条语句都还没发。这与同一函数 stale 分支的注释、[逻辑库自治](horizontal-scale-logical-db.md) 里版本落后时的口径一致。`session.pin` 失败同理。另外配 `ConnMaxIdleTime`，让它短于链路断开空闲连接的时间。现有用例 `TestWriterDriverResetFailurePoisonsWithoutBadConnReplay` 钉住的正是现行行为，说明守卫是有意这样写的，改之前要守卫作者确认 |
| R3 | 中 · 已复核（scene_manager 一份） | 7 份旧注册实现的 Close 无条件删键：[scene_manager](../../go/scene_manager/internal/noderegistry/registry.go#L382)（client_rpc_router、data_service、match 的同名副本同此）、[guild](../../go/guild/internal/node/node.go)、[player_locator](../../go/player_locator/internal/node/node.go)、[login](../../go/login/internal/logic/pkg/node/node.go) | 失租后原号已被实例 B 接管，本进程还没换号就停机 → 删掉 B 的发现键；而这些实现不 watch 自己的键，B 会一直不可见 | 统一到 [shared/noderegistry](../../go/shared/noderegistry/registry.go#L329)（按 uuid 条件删、一个循环重新夺回原号、退避可打断），把 9 份实现收成 1 份 |
| R4 | 中 | 同上：4 份 `internal/noderegistry` 及 guild、player_locator 的续约循环 | 首次或重启续约时报错就 return，此后再也不续约 → 进程活着，却从发现里消失，而且没有告警 | 同 R3 |
| R5 | 中 · 已复核 | login、player_locator 的 zrpc 客户端（go-zero v1.9.2，`NonBlock` 缺省 false） | 启动时拨号阻塞 3s：player_locator 或 scene_manager 3s 内连不上，login 就退出；scene_manager 没起，player_locator 就退出 → 冷启动和恢复时连锁 CrashLoop | 在 [login.yaml](../../go/login/etc/login.yaml#L175) 的 `PlayerLocatorRpc`、`SceneManagerRpc` 和 player_locator 的 `SceneManagerRpc` 上显式写 `NonBlock: true`（同文件的 `DataServiceRpc` 已这样写），或升级到 v1.10.0；调用时按截止失败 |
| R6 | 中 | [authoritysql 错误分类](../../go/shared/authoritysql/driver.go#L91) | 显式事务里 COMMIT 发出前的传输错误，以及 COMMIT 返回「已回滚」的码，都被标成结局未知 → 本来可以整笔重试的事务被 Hold；guild 在 COMMIT 处的 9007 重试分支，在守卫下根本走不到；8022、8028、9001、9005 全仓没有处理 | 显式事务里 COMMIT 发出之前的任何错误，判「事务已中止」；COMMIT 的错误，只对 TiDB 文档注明「已回滚」的码判确定，其余仍判未知。需要守卫作者评审 |
| R7 | 中 | [shared/leader](../../go/shared/leader/leader.go)（用于 scene_manager 编排、guild / trade 资产维护、player_inbox 维护、player_locator 对账） | 选主锁放在 Redis，没有 fencing token：异步复制的主从切换丢了锁 → 另一个副本当选，旧领导要到下一次续期才降级。双领导窗口约为一个心跳间隔（TTL/3，缺省 10s）加一次调用的耗时。下游是否都有 CAS 还没逐个核实（guild 的自动放弃有） | 当选时铸一个单调递增的 epoch（对 Redis 计数键 INCR，或改用 etcd 选主、取 revision），变更带着 epoch 到权威库做条件写 |
| R8 | 中 · 已复核 | 不走 `playerredis` 的 go-redis / go-zero 句柄（`MaxRetries` 缺省 3）；[leader 竞选](../../go/shared/leader/leader.go#L157) | 读超时或 EOF 之后，会重发已经写出的命令：<br>• 没逐个证明「重放安全」的非幂等脚本会多执行一次，例如 friend 的申请配额计数；<br>• 竞选用的 SETNX 首发已经写入、但回包丢了时，重发会返回「已存在」，本实例误判自己落选；这把锁空占到 TTL（缺省 30s）过期，期间没有领导者 | 协调用途的句柄统一设 `MaxRetries: -1`，由业务按 §1.2 决定怎么处理；竞选返回 `!ok` 时，先 GET 一下，看锁里的值是不是自己的 |
| R9 | 中 · 已复核 | C++ 的 [ResolveRedisAddress](../../cpp/libs/engine/thread_context/redis_manager.cpp#L107)（核心与场景控制域 Redis） | 启动时 DNS 解析失败，就把主机名当成 IP 字面量，之后重连也不再解析。K8s 配的是主机名 `redis.<namespace>` → 一次启动时的 DNS 抖动，就让资产唤醒、分线目录、神秘商人一直降级，直到进程重启 | 启动时解析失败，和玩家域一样 FATAL；重连时重新解析 |
| R10 | 中 · 已复核 | C++ 的 [Agones /ready](../../cpp/libs/engine/infra/agones/agones_gameserver_lifecycle.cpp#L717) | sidecar 晚于约 1 分钟才就绪 → 进程不再发 `/ready`，这个实例永远不可分配 | 封顶退避、无限重试；或者放弃时退出进程，交给编排重启 |
| R11 | 中 · 已复核 | [match 对局结果消费](../../go/match/internal/kafka/result_consumer.go#L246) | 一条解码失败或原字节冲突的消息 → 原地无限重试 → 这个分区后续的评分与结算归档全部停住 | 与 data_service、guild 一致：记死信或指标后提交 |
| R12 | 中 · 已复核 | [shared/killswitch](../../go/shared/killswitch/watch.go#L85) | 设计是「与 etcd 失联超过 StaleAfter 后规则作废、退回全放行」。但断线期间 watch 通道不会关闭，代码又每 3s 续一次 syncedAt，所以作废永远不会触发。再叠加 R1，运维撤掉的 deny 规则也可能一直传不过来 | 按 R1 修好 watch；只在真正收到 etcd 回包（事件或 progress notify）时才续 syncedAt |
| R13 | 中 · 已复核 | C++ 的 [审计流水](../../cpp/libs/modules/transaction_log/transaction_log_system.cpp#L169)、[玩家快照](../../cpp/libs/modules/snapshot/snapshot_system.cpp#L115) | Kafka 不可用超过 `message.timeout.ms`（300s），或者发送当场被拒 → 消息永久丢失，只剩一行 ERROR 日志 | 先确定审计流水是否要求不丢；要求不丢，就加本地落盘队列，Kafka 恢复后补发 |
| R14 | 低 · 已复核 | C++ 的 [玩家域 Redis 重连回调](../../cpp/libs/services/scene/core/system/redis.cpp#L133) | 任一主节点握手完成，就补发 handoff 撤回、A2′、改派核实，并且忽略重试间隔。这正是 11-05 规则禁止的写法；注释自己也写明，正确性靠的是 1s 定时器 | 删掉这个回调和 `reconnected` 参数，只留 1s 定时器 |
| R15 | 低 | scene_manager LoadReporter 的 fullSync | 每次 watch 中断后，领导者会顺带跑一整轮变更（清理陈旧成员、推迟摘除、死节点收尾、补频道、rebalance）；scene 节点重新注册时的 PUT，会按 (zone,node) 清掉死亡标记，不看 uuid | 视图重建只更新视图，变更由已有的 5s / rebalance 节拍驱动。按 uuid 清死亡标记有什么影响，待核实 |
| R16 | 低 · 已复核 | Java 的 [LoginRpcClient 动态通道](../../java/gateway_node/src/main/java/com/game/gateway/grpc/LoginRpcClient.java#L283) | 8-10 已把静态通道改成 `keepAliveWithoutCalls(false)`。原因见同文件 L174 的注释：grpc-go 缺省的强制策略会以 `too_many_pings` 踢断发空闲 ping 的连接。8-17 新增的动态通道又写回了 `true`，而 etcd 发现开着时，走的正是动态通道 → 空闲连接被反复踢断、探活间隔被翻倍，偶尔调用失败 | 改成 `false` |
| R17 | 低 | C++ 的全部依赖；Go 的注册层 | 连接状态只有日志、没有指标，掉线和重连风暴没法告警 | 按 §1.8 补指标 |
| R18 | 低 | C++ 单实例 Redis；Go 的旧注册实现与 login 的重新注册 | 重连间隔固定，不带抖动；`time.Sleep` 不可打断；调用没有超时 | 退避加抖动，改用可取消的等待 |
| R19 | 低 | 杂项 | • C++ etcd 只用 Hosts 的第一项；<br>• CLUSTER SLOTS 在途时没有自己的超时；<br>• C++ Kafka 启动拿不到元数据时，每个消费者要在 loop 线程上阻塞约 30s；<br>• Go 控制命令大多 acks=1；<br>• scene_manager、client_rpc_router 在开始监听之前就注册；<br>• Go 各句柄超时口径不一，部分带病启动；<br>• login 口令库不强制超时；<br>• Java 的 `etcd.dial-timeout-ms` 没传给客户端；<br>• 有过时注释：`node.cpp` 冲突关停处、`authoritysql/driver.go:14`、go/db `store_health.go` 指向已删的消费者 | 顺手修 |

**已知残余（有意取舍，不算缺口）**

- C++ 节点与 etcd 单边失联超过租约 TTL 时继续服务，被接管时最多丢掉最后一次补存（≤5s）之后的进度。要做到零丢失，只能冻结全部玩家，代价是一次 etcd 抖动就让整个节点停摆。
- Go 注册实现失租期间继续服务：ID 唯一性不靠租约，数据有写权行兜底。
- Pub/Sub 唤醒、Kafka 在线提醒丢了不补，都由 SQL 侧的后台循环兜底。
- 节点链路从对端最后一帧到判死有 15–16s，这期间的单向消息照样会丢，由业务协议兜底（[muduo-rpc-channel.md](muduo-rpc-channel.md) §21.9）。

**待确认**

- gate→login 的断线通知只发一次，丢了是否由 login 的会话租约兜底。
- 同一个 node_id 有两个进程同时在线时，gate 的 session_id 等由 node_id 派生的值会不会冲突。
- `shared/leader` 降级的瞬间，在途的变更是否都被下游的 CAS 或 owner_epoch 兜住。
- match 失联实例与接管实例同时撮合同一个池时，是否靠 SQL 认领防止重复开局。
- mail、rank 在生产环境是否仍用直连地址。

### 4.1 第二轮修复状态（2026-11-06，已落码，未编译、未测试，待 Codex 验证）

| 编号 | 状态 | 改了什么 |
|---|---|---|
| R1 | 已修 | 新增公共包 `go/shared/etcdwatch`：`Open` 打开的 watch 带 RequireLeader，返回时取消子 ctx（顺带修掉三份发现镜像每轮泄漏一条 watch 子流），每 5 分钟（`ResyncInterval`）强制回外层全量对账；`WithKeepAlive` 给 etcd 客户端补 gRPC keepalive（10s / 5s，与 login 同值）。用到它的 watch：scene_manager LoadReporter、match 与 client_rpc_router 的发现、`shared/scenenode`、`shared/killswitch`。补 keepalive 的客户端：scene_manager、match、client_rpc_router、trade、player_inbox、data_service（召回、killswitch、注册）、guild、db（killswitch）、`shared/dbplacement`。周期对账只打 Debug 日志 |
| R2 | 已修 | `authoritysql` 的 `ResetSession`：底层存活检查报 `ErrBadConn`、或重放会话 / 核验身份失败时，回 `ErrBadConn`（这时还没发任何业务语句），database/sql 随之换连接；新增 `ConnMaxIdleTime` 3 分钟。旧用例 `TestWriterDriverResetFailurePoisonsWithoutBadConnReplay` 改名为 `...DiscardsConnectionWithBadConn` 并改期望，`TestMySQLResetSessionRecheckAfterDemotion` 同步改期望，新增连接池级回归 `TestPooledConnectionFailingLivenessCheckIsReplacedTransparently`。这改了守卫原先有意钉住的行为，**负责人 2026-11-10 确认保留**。验证：Codex 11-08 跑过 `shared/authoritysql` 整包，上面三条用例都 PASS（`build/verification/naming-audit-20261108/go-shared.test-r4.log`），之后 `driver.go` 没再改；TiDB 滚动重启的演练（§5）还没做 |
| R3 | 已修（就地） | 7 份旧注册实现的 `Close` 改为「分配键的值仍是本实例 uuid 才删」，guild、player_locator、login 三处顺带给删键加 5s 超时。收成 `shared/noderegistry` 一份仍是正解（共享包注释写明「迁移时以本包语义为准」），这轮没迁：client_rpc_router 那份的签名已与其他副本不同，data_service 本地 ZoneId 可为 0 而共享包拒绝 0，迁移要逐个改装配，留作后续。<br>**2026-11-10 续（未编译）**：scene_manager、client_rpc_router、data_service、match 四份逐字相同的 `internal/noderegistry` 改成 `shared/noderegistry` 上的薄包装。对外的 `Register` / `KeepAlive` / `Close` / `Info` 不变，调用方一行没改；共享包与包装加起来净减约 800 行。`Info` 只是注册时的快照，换号后不再被续约 goroutine 改写（顺带去掉了一处数据竞争）。<br>与原副本的行为差异：<br>• ZoneId 为 0、端口为 0、client_rpc_router 传入的轨道与环境变量不符时拒绝注册；<br>• 分配时 CAS 出传输错误直接失败；<br>• etcd 不通时 Close 最长约 10s（原 5s），可能让 data_service 的停机返回 HOLD；<br>• 日志前缀变为 `[NodeRegistry <Prefix>]`。<br>guild、player_locator、login 三份形状不同（租约绑定标记、冲突即退出进程），仍是 11-06 的就地修 |
| R4 | 降为低；四份迁移副本已随 R3 收口（2026-11-10） | 共享包里续约启动失败按失租处理，不再静默停止续约。复核 clientv3 v3.5：租约续约器只在客户端被关闭时才停，「续约报错就 return」实际只在停机时走到。随 R3 的迁移一并收口 |
| R5 | 已修 | `go/login/etc/login.yaml` 的 `PlayerLocatorRpc`、`SceneManagerRpc`，`go/player_locator/etc/player_locator.yaml` 的 `SceneManagerRpc` 显式写 `NonBlock: true`；K8s login ConfigMap 同步 |
| R6 | 已修（负责人 2026-11-10 拍板；2026-11-11 落码，未编译） | 守卫：事务里只执行过纯 DML（`tx_outcome.go` 的 `plainDMLStatement`，可执行注释、SHOW、EXPLAIN 都按非纯 DML 算），COMMIT 发出前的任何失败判「确定中止」：<br>• 连接层失败回 `ErrTxAbortedBeforeCommit` 并隔离连接；<br>• 服务端回包原样返回，连接不隔离；<br>• 回滚失败也判确定中止。<br>COMMIT 自身的错误仍是结局未知。请求级不调 `NoteNoWriteRetry`：一个请求里可能先提交过别的事务。<br>测试与调用方：旧断言按新口径改了 9 处（其中 3 个改名，没删用例），新增 `tx_outcome_test.go`。trade、player_inbox、guild 后台、data_service 召回的事务重试判定加了 `IsAbortedBeforeCommit`。已核对：player_store 对冻结以外的错误一律回 `Unavailable`，scene 照旧按同一序号重试，存档路径不受影响。<br>未改：guild 的 `isRetryableTxError`（它同时供死锁计数和 COMMIT 分类）、各服务自写的重试循环 |
| R7 | 核对后降为低，未改（2026-11-10） | 只读核对了全部由 `shared/leader` 和 login `pkg/locker` 选主门控的动作。凡是动资产、场景归属、拆建的，提交点都已有按对象的条件写，双领导写不坏数据，**不需要 fencing token**：<br>• 场景认领比对期望属主和 token，改派按期望属主 CAS 带回执和代次；<br>• 资产自动放弃按租约 `UPDATE … WHERE lease_token=?`；<br>• 收件箱按 next_seq，会话对账按代次。<br>剩下 4 处是在脚本外先读、再判、再写，最坏多建一条频道、多缩一条、多放一批人：自动扩缩容（判冷却与写期望数分三步，`world_autoscale.go`）、登记世界频道（只 SADD，不比期望数，`scene_registration.go`）、排队出队放行（名额在脚本外按快照算，`login/.../loginqueue/dispatcher.go`）、负载分刷新（无条件 ZADD，`load_refresh.go`）。修法是把判断收进同一段 Lua，留作低危后续。<br>真要统一任期号，不能用 Redis INCR：双领导本来就是异步复制丢写造成的，计数键和锁在同一条复制流里会一起丢。应改用 etcd 选主，取当选键的 CreateRevision |
| R8 | 部分修 | `shared/leader` 与 login 的 `pkg/locker`：SETNX 回「已存在」时核对锁里是不是自己这次的值，是就当选或拿锁；各加两个单测。剩余：没逐个证明重放安全的非幂等脚本（如 friend 申请配额计数）仍在 `MaxRetries` 缺省 3 的句柄上。2026-11-10 与「Redis 分布式水平扩展」会话分工：这些非玩家域句柄由它整批换成 `shared/playerredis` 同一套构造器（挂键守卫和「结局未知不重放」钩子），换句柄和键改名同一批做，剩余部分随那批收掉，本清单不再单独改。<br>11-10 只读核对出两处中危，**光换句柄不够**，调用方自己的重试也会触发同样的问题，要在调用方修：<br>• 帮会榜维护锁 SETNX（`guild/internal/data/guild_repo.go` 的 `acquireRankLock`）：重发回「已存在」后，循环按同一个 token 重试却从不 GET 比对，锁空占 5 分钟，期间积分落库、重建榜、摘榜全部报错。修法同选主锁。负责人 11-10 已定删全服帮会榜，这把锁随删除处理；不删的话按此修；<br>• login 排队轮询取放行槽的 GETDEL（`loginqueue/queue.go`）：首次已取走、回包丢了时，重试回 admit_window_passed，排到号的玩家拿不到票。修法是取走后在 meta 里记下这个槽，同一排队令牌在短宽限期内重查时返回同一个槽。<br>低危的另有：补建锁、观战升档租约、申请推送冷却、唤醒去重的 SETNX，friend 的配额 INCR，`ReserveBestWorldChannel`，locator 的 `endPassScript`，开始排空脚本。清单与修法见 PROGRESS 2026-11-10 条目 |
| R9 | 已修 | C++ `ResolveRedisAddress`：DNS 失败按 1s 间隔重试 10 次（与 etcd 端点同口径），仍失败就致命退出，不再把主机名当 IP 字面量 |
| R10 | 已修 | C++ Agones `RunInitialReady`：快速重试用完之后不放弃，按 `readyMaxBackoff` 一直重试到成功或 Stop()，期间照旧 fail-closed；日志在第 `readyMaxAttempts` 次和此后每 30 次打一行 ERROR。旧用例 `ReadyGivesUpAfterMaxAttemptsAndKeepsFailingClosed` 改为 `ReadyKeepsRetryingPastMaxAttemptsAndStaysFailClosedUntilSidecarRecovers` |
| R11 | 按负责人 2026-11-06 决定修正 | match 对局结果消费：解码失败、同局原字节冲突（`authority.ErrCompletionOriginalConflict`）保留位点并告警，等待人工处置。单轮处理立即返回错误，不做三次业务重试；consumer owner 仍按既有退避恢复原消息。日志、`decode_error` / `original_conflict` 指标及提交停滞告警保留。两个永久错误单测改为拒绝，新增冲突不提交位点的消费者用例，原损坏消息不提交断言不变。取代上方历史 R11 的跳过方案，契约与处置边界以 `cross-zone-matchmaking.md` §13.4 为准 |
| R12 | 已修 | killswitch 不再在 watch 开着时每 3s 无条件续期：watch 按 StaleAfter 的三分之一定期返回，只在全量读成功或收到改变规则的事件时续 `syncedAt`；新增 `recheckInterval` 单测 |
| R13 | 未修，方案已定（2026-11-10） | 按「不丢数据」的标准，审计流水与玩家快照要求不丢，做法定为**事务性发件箱**。scene 存盘时把待发的审计条目随存档一起交给 player_store；player_store 在同一笔 TiDB 事务里写进本逻辑库的发件箱表；再由转发循环把发件箱投到 Kafka，确认后删除，data_service 照旧消费。<br>不选另外两种：<br>• 本地落盘队列：K8s 上 Pod 一重建就丢；<br>• 把 `message.timeout.ms` 调成无限：只撑得住进程活着时的 Kafka 中断。<br>发件箱放在 player_store 自己的逻辑库，不跨库写（AGENTS §7 第 6 条）。这是跨 C++、proto、player_store、转发循环的一个功能，单独开会话做 |
| R14 | 已修；2026-11-11 收尾（未编译） | 2026-11-06：删掉 `redis.cpp` 里玩家域 Redis 的重连回调，改为注释说明为什么不挂。<br>2026-11-11 收尾：删掉 `RetryPendingHandoffWithdrawals`、`RetryInheritedMarkClears`、`SweepRelocateConfirms` 的 `reconnected` 参数，连同往下传的「重连时忽略重试间隔」开关一起删：`FlushDueHandoffWithdrawals` 与 `handoff_mark_withdraw::Queue::CollectDue` 的 `force`，`relocate_confirm::Table::CollectDue` 的 `reconnected`。到期只看各条目自己的重试时刻。`RetryInheritedMarkClears` 改为由调用方传入 `now`（单调时钟，同 `SweepRelocateConfirms`），1s 定时器传 `Clock::now()`。<br>`cross_zone_test.cpp`：两条 A2′ 重发耗尽用例原来靠重连忽略退避来连发，改为注入合成时刻（现在 + 第 i 次失败后的退避），推进到条目的重试时刻之后再调；两张表的单测把「重连时提前」的断言改为「到了重试时刻才发」。没删断言。<br>原因同上：负责人 2026-11-05 定「重连成功不做业务逻辑」。待 Codex 编译并跑 `cross_zone_test` |
| R15 | 已修（2026-11-10，未编译） | ① R1 让 fullSync 每 5 分钟固定跑一次，领导者的那轮变更变成按时对账。<br>② 清死亡标记改为按实例：scene_manager 会话核实过「按 (zone,node) 清、不看 uuid」确实削弱屏障——新进程按最小空闲号接走同一个 node_id，它的 PUT 会删掉老实例的 `death_at`，老实例 20 秒的再入屏障对收尾、改派、rebalance 全部失效。现在由 `clearNodeDeathForInstance`（`reentry_barrier.go`）用一段 Lua 比对 `reconcile_instance` 里判死那一代的 uuid：相同才删；不同或没有记录就留着，让屏障走完。新增单测 `TestClearNodeDeathForInstance`、`TestClearNodeDeathForInstanceWithoutDeathIsNoop`。<br>残余（2026-11-11 读代码核实成立，没有运行；未修）：清 `death_at` 在领导者闸门之前、每个副本都会跑（`load_reporter.go` watch PUT 分支）；死节点收尾判断「判死的那一代回来了」，看的却是本副本的镜像（`reentry_barrier.go` 的 drain）；`CanReclaimDeadNode` 见不到 `death_at` 就放行；逐个销毁的 `destroyOrphanOfDeadNode` 只比对属主仍是这个 node_id，同一代回来时属主没变，照样拆。所以别的副本先清掉标记、而本领导者的镜像还没收到 PUT 时，收尾会拆掉刚回来的活实例，还向它发 DestroyScene。窗口平时是毫秒级，领导者的 watch 断开重连期间可到秒级以上。领导者自己的镜像在一轮收尾中途收到 PUT 时同样拦不住（撤销判断每个任务只做一次，在逐个销毁之前），这一条与 R15 无关、早就有。修法：清标记与撤销收尾任务合进同一段脚本，由先收到 PUT 的副本执行；死节点收尾的逐个销毁脚本在同一步核对收尾任务仍在、且入队时刻没变，任务被撤或换了一次判死就一个键都不动。归 scene_manager 死节点收尾的维护者 |
| R16 | 已修 | Java 网关动态 login 通道改为 `keepAliveWithoutCalls(false)` |
| R17 | Go 注册层已补（2026-11-10，未编译）；C++ 未补 | `shared/noderegistry/metrics.go` 新增 5 个指标，label 只有 prefix，失败另带固定的 reason：<br>• `noderegistry_lease_lost_total`<br>• `noderegistry_reregister_success_total`<br>• `noderegistry_reregister_failure_total{reason=grant\|reclaim\|allocate\|taken_over}`<br>• `noderegistry_node_id_reallocated_total`<br>• gauge `noderegistry_registered`<br>告警规则还没接。C++ 节点的连接状态仍然只有日志 |
| R18 | Go 部分随 R3 迁移处理；C++ 部分暂不改（2026-11-10） | C++ 单实例 Redis 的重连定时器（`RedisManager::ScheduleReconnect` 的 `runEvery`）同时兼作 AUTH 握手截止：`reconnectIntervalSec` 到点时握手还没完成，就拆掉重连。`redis_lifecycle_test`、`redis_manager_auth_test` 的时序断言都按这个间隔写。给它加抖动要同时改握手截止，并重新验证这两组生命周期用例。低危条目不值得这个风险，留到下次专门改 `RedisManager` 时一并处理 |
| R19 | 部分修 | 已修：Java `etcd.dial-timeout-ms` 现在传给 jetcd 的 `connectTimeout`；`authoritysql/driver.go` 第 14 行的过时注释已改。其余照旧 |
| R20 | 新发现，已修 | **中**。K8s 的 player-locator ConfigMap 没有 `SceneManagerRpc`，`SceneManagerClient` 为空，`notifySceneManagerLeave` 每次都回「SceneManager client is not configured」，清理任务永远重试不成。于是 muduo-rpc-channel.md §8.3 写的兜底（断线时没送到 scene 的 ExitGame，由断线租约 → LeaseMonitor → `SceneManager.LeaveScene` 收口）在 K8s 上不成立。已在 `tools/scripts/k8s_deploy.ps1` 的 player-locator 模板补上（键按 zone 加后缀，`NonBlock: true`，Timeout 3000 镜像仓库 yaml） |

## 5. 演练清单（尚未执行）

按 [AGENTS.md](../../AGENTS.md) §10.1 交给 Codex 在隔离环境执行；每条都要记录 Redis、TiDB、etcd 的版本与拓扑。

| 场景 | 应观察的结果 | 对应 |
|---|---|---|
| 三成员 etcd，把 scene_manager 当前所连的成员与 leader 隔离 60s（iptables），期间杀掉一个 scene | 修复前：LoadReporter 看不到 DELETE；修复后：数秒内流报错、重新同步、判死 | R1 |
| TiDB 滚动重启（或 `KILL` 掉所有空闲连接）后，立即发一批 guild / trade 写 | 修复前：每个池的首批调用失败，Hold 计数上涨；修复后：零失败、零 Hold | R2 |
| 先起 login，30s 后再起 player_locator 和 scene_manager | login 不退出，依赖起来后正常服务 | R5 |
| 持续竞选和续期的同时，让 Redis 主从切换（Sentinel 或手动 failover） | 记录双领导窗口和无领导窗口 | R7、R8 |
| scene 启动时让 `redis.<namespace>` 解析失败 10s，再恢复 | 修复后：进程退出、由编排重启，或者恢复后连上 | R9 |
| Agones sidecar 推迟 2 分钟就绪 | 实例最终变成 Ready | R10 |
| 往对局结果 topic 投一条坏消息 | 保留原位点，不评分、不提交后续消息；单轮处理不重试，日志和指标记录错误，提交停滞告警触发，人工在保留期内保全并核对原件 | R11 |
| 断开 etcd，持续时间超过 StaleAfter | killswitch 规则按设计作废 | R12 |
| Kafka 全停 6 分钟，期间产生交易流水 | 修复前记录丢失条数；修复后确认恢复后补齐 | R13 |
| gate↔scene 网络断开 20s 后恢复 | 15–16s 内判死，然后重连、握手、Ready；不触发任何业务补发 | 节点链路 |
| 单实例 Redis 重启 | C++ 在 3s 内重连，AUTH 成功后才放行业务；在途命令只收到一次空应答 | §3.1 |

## 6. 相关文档

- [muduo-rpc-channel.md](muduo-rpc-channel.md) §6、§8.3、§21：节点链路的断连、握手与心跳
- [grpc-muduo-transport.md](grpc-muduo-transport.md)：C++ 的 gRPC 与 etcd 传输
- [redis-async-client-retry-fix.md](redis-async-client-retry-fix.md)：C++ Redis 的连接生命周期。其中 `MessageAsyncClient` 各节描述的代码已在 10-30 随 `fc504300b` 删除，只作历史参考
- [horizontal-scale-logical-db.md](horizontal-scale-logical-db.md)：`authoritysql` 守卫、写者闸门、排空证明
- [data-consistency-guarantee-map-20261020.md](data-consistency-guarantee-map-20261020.md) §6：etcd 失联期间的补存取舍
- [disconnect-reconnect-audit-20261020.md](disconnect-reconnect-audit-20261020.md)、[player-connection-lost-hold.md](player-connection-lost-hold.md)：玩家客户端的断线重连

## 7. Java 版现状（独立仓库 `xuanming-server-mmo`）

> 核对范围：2026-11-06，Java 仓 `D:\luyuan\xuanming-server-mmo`，HEAD `173dec41`，工作区里只有他人未提交的 `PARITY.md` 改动。只读静态核对。Redisson 3.50.0、Druid 1.2.28 的库缺省值，是按本机 Maven 缓存里的 jar 反汇编核对的。没有编译，也没有跑断网、重启或切主演练。
>
> 路径写法：路径相对 Java 仓根目录，`src/main/java/com/game/` 一段写成 `…/`；行号按当天工作区。
>
> 注意：§0、§3.3 里的「Java」指本仓的 `java/gateway_node`（Lettuce、Hikari、jetcd、grpc-java），与本节的 Java 版服务器不是同一套代码。

### 7.1 结论

- **依赖面和 C++ / Go 版不同**：
  - Java 版不连 etcd。注册发现、节点号租约、选主锁、热关停规则都放在 Redis 上；
  - 服务间 RPC 是 Dubbo 3.3.6 的 Triple 协议，不用 gRPC；
  - Nacos 只在 `nacos` profile 下做 Dubbo 注册中心（`docs/design/architecture.md:1284-1307`）。
- **大部分条目合规**：
  - 节点号与选主租约按单调时钟，在 TTL 的 2/3 处自停；
  - 没有 watch，也就没有 watch 静默停住的问题；
  - 共用依赖不进 readiness；
  - 没有「重连成功后做业务」的钩子；
  - 改状态的 RPC 不让 Dubbo 重发；Redis 的客户端写路径按次关掉驱动重发。
- **缺口 5 个**：中 3 个（J1、J2、J5），低 2 个（J3、J4）。都不会写坏权威数据，影响的是依赖重启后的可用性：
  - J1：后端重启后约 60s 不可用；
  - J2：数据库重启后，每个连接池成批报「结局未知」；
  - J5：链路一断，就踢掉其上全部玩家。

  登记情况（都在 Java 仓 `PARITY.md`）：
  - J1～J3 登记为「Java 待做」（2026-11-06「依赖断线与重连核对」）；
  - J4 的口径负责人 11-06 已定为「保留位点并告警」，Java 现有行为与之一致，剩下的核对并入同日「Match 损坏结果与原件冲突保留位点（Java 待核对）」；
  - J5 在 11-05「节点链路恢复与业务状态解耦（Java 范围核对）」里登记过。

### 7.2 各依赖

| 依赖 | 库与建连位置 | 重连与退避 | 启动时不可用 |
|---|---|---|---|
| Redis | Redisson 3.50.0 单机。各进程共用一个懒建的客户端（`game-discovery/…/discovery/RedisAutoConfiguration.java:29-58`）；login 另为 Sa-Token 建一个，超时与业务 Redis 同口径（`game-login/…/login/LoginConfiguration.java:217-230`） | 都是库缺省，项目没有覆盖：<br>• 库内看门狗重连，100ms→10s 等抖动退避；<br>• 每 30s PING 一次，命令连接和订阅连接都装；<br>• 每 5s 重新解析一次主机名 | 常驻依赖 Redis 的进程在启动装配时就连，连不上就拒启；game-data 经 `ObjectProvider` 取用，不挡启动（`RedisAutoConfiguration.java:17-21`） |
| MySQL / TiDB | Druid 1.2.28 + mysql-connector-j。所有 DataSource 都在初始化之前装上 `DruidAuthorityDriver`（`game-discovery/…/discovery/sql/AuthorityDataSourcePolicy.java:38-52`） | • 由池换掉坏连接；借出时只校验闲置满 60s 的连接（Druid 缺省 `testWhileIdle`）；<br>• 每条新物理连接都重放 TiDB 强会话变量并核验（`DruidAuthorityDriver.java:18-29`、`StrongTiDbConnectionPolicy.java:17-20`）；<br>• 各服务的 JDBC URL 都带超时：建连 2～3s，读写 4～30s（例如 `game-login/src/main/resources/application.yaml:23`；game-chat 写在 Druid 属性里） | 启动期取一条连接核对库名，连不上就拒启（`game-discovery/…/discovery/namespace/NamespaceDatabaseVerifier.java:25-26`） |
| 节点目录、节点号、选主、热关停 | 全在 Redis：<br>• 节点目录 `NodeDirectory`：RMapCache，每 5s 发布、TTL 15s；<br>• 节点号租约 `NodeIdLease`；<br>• 选主锁：`WorldLeaderLock`、收件箱 `LeaderLease`、gateway 排队放行的 `RLock`；<br>• 热关停：`RedisKillSwitchSync` | 读方按需拉取，续期按 TTL/3 周期执行，没有长连接维护的视图 | 拿不到节点号就启动失败（`NodeIdLease.java:142`） |
| Kafka | kafka-clients 3.9.2。<br>• 生产：scene 的审计流水与快照（`game-scene/…/scene/audit/AuditPipeline.java:107-115`）、battle 的对局结果（`game-battle/…/battle/port/kafka/KafkaBattleResultSink.java:195-202`）；<br>• 消费：game-data 审计、match 评分、guild 历练 | • 由库重连；<br>• 消费循环遇可恢复故障时暂停分区，1s→30s 退避；<br>• 循环异常退出时，监督线程隔 5s 换一个新消费者（`game-match/…/match/rating/BattleResultIngest.java:55-61`） | 不拦启动：生产者在 topic 核对通过之前，把记录写进兜底日志；消费者后台每 30s 再核一次（`AuditPipeline.java:133-135`、`BattleResultIngest.java:214-227`、`docs/design/architecture.md:1364`） |
| 服务间 RPC | Dubbo 3.3.6 Triple，两类客户端：<br>• Spring 注入的 `@DubboReference`，例如 gate 的 9 个后端引用；<br>• 按节点直连的 `NodeRpcClients` 与 `SceneAssetOpClients` | • 直连客户端把重连间隔和空闲心跳都压到 1s（`game-api/…/api/rpc/NodeRpcClients.java:46-47`、`:203-227`）；<br>• 其余引用用库缺省：断线 1s 后重连一次，落空就要再等 60s；心跳 60s，判死 180s（J1） | 引用都是 `check = false`：对端不在也照常启动，不会出现冷启动连锁退出（`game-gate/src/main/resources/application.yaml:102-105`） |
| 节点链路 gate→scene | Netty。gate 侧：`game-gate/…/gate/link/SceneLinkManager.java`、`NettyLinkConnector.java`；scene 侧：`game-scene/…/scene/link/` | • 按需建链：有帧要发、又没有链路时才建；每个 scene 节点同时只有一条在建；<br>• 握手 5s；心跳 5s，读空闲 15s 判死（`SceneLink.java:191-216`、`:318-333`）；<br>• 建链失败后没有退避（J3） | 不涉及 |

### 7.3 逐条核对（§1.10）

**1. 在途调用的结局**

- **Redis**：
  - 命令响应超时（2s）后，库会隔 200ms 把已写出的命令原样重发一次；
  - INCR、SETNX、LPUSH 这类命令在库的不重发名单里，EVAL 不在（Redisson 的 `RedisExecutor.isResendAllowed` 与 `RedisCommands.NO_RETRY_COMMANDS`）；
  - 项目据此约定：可变的 Lua 必须能被重放（`docs/design/architecture.md:1329-1333`）。
- **MySQL / TiDB**：
  - `AuthorityConnection` 遇到连接类错误（SQLState 08xxx 等）或 COMMIT 出错时，一律中止这条物理连接，抛「结局未知」08007，要求调用方按原业务键复查（`game-common/…/common/sql/AuthorityConnection.java:119`、`:121-131`、`:145-157`）；
  - 这样一来，显式事务里 COMMIT 之前的失败也被算成了未知（J2）。
- **Kafka 生产**：
  - 幂等生产者，acks=all，库内重试到 `delivery.timeout.ms`（120s）；
  - 没被确认的记录逐条写进兜底日志，之后用 `AuditFallbackReplay` 工具补发（`AuditPipeline.java:47-49`）；
  - 不会静默丢失，但补发要人工执行。
- **Dubbo**：调用失败或超时直接以异常结束，gate 回带请求 id 的信封 1003。
- **节点链路**：两类结局分得清：
  - 只进了待发队列、没写出去的进场帧，判「确定没送到」，由会话在预算内补发；
  - 一旦调用过 `write`，这一帧就失去补发资格（`game-gate/…/gate/link/SceneLinkListener.java:42-43`、`SceneLink.java:136-169`）。

**2. 重试只有一层**

- **Redis**：
  - 全局句柄 `retryAttempts = 1`，靠脚本自己识别重放：选主与租约脚本都是「已是本令牌就续期」（`game-discovery/…/discovery/world/RedissonWorldChannelStore.java:103-113`）；匹配票据脚本逐段写明了重放语义（`game-match/…/match/ticket/TicketScripts.java:18-21`）；
  - 客户端请求路径上的计数写按次关掉重发：好友申请配额、聊天限速与写历史都用 `retryAttempts(0)`（`game-friend/…/friend/quota/FriendRequestQuota.java:84-120`、`game-chat/…/chat/store/RedissonChatStore.java:32-41`）；
  - 例外是帮会申请配额：它仍走带重发的句柄，注释写明重放可能多计一次（`game-guild/…/guild/quota/GuildApplyQuota.java:26`）。这是防刷用的 fail-open 背压，影响可以接受。
- **Dubbo**：
  - 改状态的方法都设了 `retries = 0`；
  - gate 的后端引用只对 `handle` 关了重试。`sessionClosed`、`abandonEnter` 保留 Dubbo 缺省的 2 次重试，这两个方法在接口上注明了幂等（`game-gate/…/gate/GateConfiguration.java:100-106`、`game-api/…/api/ClientMessageService.java:38-46`）。
- **SQL**：
  - `AuthorityJdbc`（mail、rank 用）只在 COMMIT 之前对 1213、1205、9007 整笔重试，最多 3 次，受截止约束；COMMIT 结局未知时抛给调用方（`game-discovery/…/discovery/sql/AuthorityJdbc.java:29-53`）；
  - 其余服务各有一份错误分类（如 `GuildSqlErrors`、`RatingSqlErrors`），没有统一封装。这一点与 Go 版现状相同。
- **节点链路**：进场补发是会话级重试，250ms→2s，最多 16 次，预算 20s，没有抖动（`game-gate/…/gate/session/PendingSceneEntry.java:10-40`）。

**3. 截止时间**

- Redis：单条命令最坏阻塞约 4.2s，低于客户端的 5s（`game-discovery/…/discovery/RedisProperties.java:13-16`）。
- SQL：每次借连接，都按剩余预算收紧网络超时和语句超时（`AuthorityJdbc.java:34`、`:62`）。
- Dubbo：gate 调后端 5s；其他引用逐个配置，多为 2～3s。

**4. 持有类状态**

- **节点号租约**（`game-discovery/…/discovery/NodeIdLease.java`）：
  - 有效的条件：未丢失，且距最近一次成功续期不足 TTL 的 2/3。时刻按单调时钟，取命令发出之前的读数（`:28-34`、`:183-186`）；
  - 续不上约时自动挂起。原号空出来就复占，而且必须先领到新代次才恢复；发现被别人占了就永久失效（`:190-255`）；
  - 发号方每次发号前都检查租约是否有效（`LeaseGatedSnowflake`，覆盖 login、team、trade、match、scene-manager 与 scene 的 GUID）。
- **失租后的动作**：
  - gate：停止接客，不再新建链路，关闭全部会话（`game-gate/…/gate/GateNode.java:337-341`）；
  - scene：身份挂起时，暂停发布目录、停止接收进场（`game-scene/…/scene/SceneNode.java:1192-1198`）；
  - battle：关准入闸，进程不退出；
  - match：健康检查报 DOWN，由编排重启（`docs/design/architecture.md:1293`、`:1362`）。
- **选主锁**：
  - 频道计划领导锁、收件箱维护锁的有效期判法同上；写入脚本还会再核一次令牌（`game-discovery/…/discovery/world/WorldLeaderLock.java:13-21`、`game-inbox/…/inbox/maintenance/InboxMaintenance.java:156`）；
  - gateway 排队放行用 Redisson 的 `RLock` 加看门狗，每拍先远程确认仍持有，再干活（`game-gateway/…/gateway/queue/QueueDispatcherRunner.java:44-49`）。
- **玩家归属**：租约在 SQL 里。库不可达时，这个节点继续服务内存里的玩家；写回带 `owner_epoch` 围栏；被接管的实例要等续约恢复后才移除（`game-scene/…/scene/ownership/OwnerLeaseRenewer.java:18-27`）。这与 §1.4 的第二种情况一致。
- **残余**：
  - 租约和选主锁都放在单机 Redis 上（AOF everysec）。Redis 崩溃丢掉最后约 1s 的写时，可能出现两个持有者；
  - 永久 ID 的唯一性不靠这个租约保证，见 Java 仓 `PARITY.md` 2026-11-05「永久 ID 发号」；
  - 上主从之前要做的事，列在 `docs/design/architecture.md:1343-1347`。

**5. 订阅与 watch**

- **没有 watch**：节点目录、gate 目录都是每次现读，视图最多滞后一个 TTL（15s）；读方把连接失败当成不可用。
- **Redis Pub/Sub 共四类**：gate 推送、归属夺权通知、产出封禁变更、观战帧。Redisson 断线后会自动重新订阅。断线期间丢掉的消息，各有兜底：
  - gate 推送明确是至多一次，由客户端拉取兜底（`game-discovery/…/discovery/presence/PlayerPushes.java:29-30`）；
  - 夺权通知丢了，由周期续约发现归属已失（`OwnerLeaseRenewer.java:104-118`）；
  - 产出封禁变更丢了，由周期全量刷新补上（`game-scene/…/scene/gainblock/GainBlockSync.java:63-72`）。
- **热关停**：每秒全量读一次 Redis 哈希。只有读成功才刷新快照时间，失联超过 60s 快照作废、整体放行（`game-common/…/common/killswitch/KillSwitch.java:20`、`:111-115`）。本仓 R12 的问题在这里不存在。
- Nacos 客户端的断线重连与重新订阅，这次没有核（见 §7.5 待确认）。

**6. 重连后的业务钩子：没有**

- 没有 Redisson 连接监听器，也没有 Dubbo 状态监听器。
- 节点链路握手成功后，只发出排队中的帧、启动心跳（`SceneLink.java:290-316`），与 11-05 那次 Java 范围核对的结论相同。
- scene 节点号身份恢复时，会触发一次频道计划重拉，拉完才恢复接客（`SceneNode.java:1200-1205`）。这属于身份层的置就绪：频道计划本来就按周期拉取。
- 要注意的是断开那一侧：链路一断，gate 就关掉其上的全部会话，scene 把这些玩家移出并写回（`game-gate/…/gate/session/ClientDispatcher.java:1432`、`game-scene/…/scene/link/SceneLinkService.java:57-63`）。这等于把「链路断了」当成「对端实例没了」，与 §1.1「实例是否消失只看权威发现」不一致（J5）。

**7. 启动时依赖不可用**

- Redis、MySQL、节点号：拿不到就拒启；
- Kafka、Dubbo 对端：不在也照常启动。

每种依赖只有一种写法，与 §1.6 一致。Nacos 不可用时 Dubbo 注册的启动行为，这次没有核。

**8. 地址与探针**

- **地址能重新解析**：Redisson 每 5s 重新解析主机名；节点链路每次建链都重查目录；JDBC 每条新连接各自解析。本仓 R9 的问题在这里不存在。
- **探针**：
  - 编排的健康检查走 `/actuator/health/readiness`，里面只有 `readinessState`，scene 另加本实例的 `sceneIdentity`；共用依赖不进 readiness（`game-scene/src/main/resources/application.yaml:59-63`、`deploy/compose/stack.yaml:72`）；
  - Java 仓目前没有 K8s 清单。

### 7.4 缺口与修法

| 编号 | 严重度 | 位置 | 场景 → 后果 | 修法 |
|---|---|---|---|---|
| J1 | 中 · 已复核 | 除 `NodeRpcClients`、`SceneAssetOpClients` 外的全部 Dubbo 引用：<br>• gate 的 9 个后端；<br>• gateway→login、login→scene-manager、team→match、battle→match、guild→match、rank→mail、trade→login、match→inbox、scene→scene-manager、game-data→guild。<br>例如 `game-gate/…/gate/GateConfiguration.java:105-106`、`game-battle/…/battle/BattleConfiguration.java:283` | ① 后端重启：Triple 客户端断线 1s 后重连一次，那一次落空就要等 60s（`dubbo.application.least-reconnect-duration` 的缺省值）。后端回来之后约一分钟内，gate 对它的号仍回信封 1003，211 回 4030。<br>② 后端主机掉电、留下半开连接：心跳 60s、判死 180s，最长约 4 分钟里，每次调用都要耗满超时才失败。<br>机制见 `game-api/…/api/asset/SceneAssetOpClients.java:52-66`。Java 设计文档把它记为遗留，调参待定（`docs/design/architecture.md:1357-1361`） | • 统一给消费端配与 `NodeRpcClients.reconnectParameters()` 相同的一组参数（重连间隔与空闲心跳）；<br>• 取值要权衡：1s 的代价是空闲连接每秒一次 PING，也可以取与节点链路同档的 5s（判死约 15s）；<br>• 改完后，把钉住现状的 `BackendReconnectTest` 改成钉新值 |
| J2 | 中 · 已复核 | `game-common/…/common/sql/AuthorityConnection.java:119`、`:130`、`:152-157`；Druid 的借出校验（各服务 `application.yaml` 里的 `test-while-idle`，阈值用缺省的 60s） | TiDB 滚动重启或切主之后：<br>① 60s 内用过的空闲连接，借出时不校验，首次往返就报连接错误（08S01）；<br>② 守卫把它判成「结局未知」08007。可这时事务还没有 COMMIT，库会回滚，实际是「确定没执行」；<br>③ 业务因此不能透明重试，调用方只能按结局未知处理，客户端多半收到 1003，要带原键重发。<br>每个池最多 `max-active` 条连接，各失败一次。COMMIT 返回「已回滚」的码，同样被判成未知。<br>这与本仓 R6 是同一处设计，但 Java 没有 Hold，不会卡住排空 | • 显式事务里 COMMIT 之前的连接错误，改判「事务已中止」，用一个区别于 08007 的状态码，让调用方能在预算内整笔重试；<br>• COMMIT 的错误，只对文档注明「已回滚」的码判确定，其余仍判未知；<br>• 把 Druid 的 `time-between-eviction-runs-millis` 调到秒级（借出时闲置超过它就校验），或者开 `test-on-borrow`。<br>需要守卫作者评审，与 R6 两版一起定 |
| J3 | 低 · 已复核 | `game-gate/…/gate/link/SceneLinkManager.java:184-203`、`SceneLink.java:335-380` | 链路建立失败（连接被拒、握手被拒或超时）时，只把这条链路摘掉；下一帧一到，就立刻建新的一代。<br>链路层没有冷却，重连节奏全看业务发帧：scene 刚死、目录条目还在的 15s 里，或者握手一直被拒时，每个进场帧都会触发一次建连。<br>会话级补发也没有抖动 | • 每个 scene 节点建链失败后进入冷却：0.5s 起翻倍，8s 封顶，加抖动（同 C++ `muduo-rpc-channel.md` §8.3）；<br>• 冷却期间 `send` 直接回 0，进场按「暂时不可用」走会话补发；<br>• `PendingSceneEntry` 的退避也加上抖动 |
| J4 | 低 · 已复核，口径已定 | `game-match/…/match/rating/BattleResultConsumer.java:30-34`、`:266-270`；开关 `game.match.global.enabled` 缺省关闭（`RatingConfiguration.java:148`） | 开启 global 模式后，遇到解不出的结果，或者被库按数据错误拒绝的结果（SQLState 22 / 23），都会保留原位点原地重试，这个分区后续的评分全部停住。<br>负责人 2026-11-06 已确认这就是要的口径：保留位点并告警，等人工处置，不跳过，也不写毒丸表（§4.1 R11，契约见 `cross-zone-matchmaking.md` §13.4）。所以 §2「坏消息记录后跳过」不适用于全球对局结果。<br>global 关闭（当前缺省）时走旧兼容模式，跳过并提交 | 停住的行为不用改。剩下的事并入 Java 仓 `PARITY.md` 2026-11-06「Match 损坏结果与原件冲突保留位点（Java 待核对）」：<br>• 永久错误在单轮处理里不做无效重试；<br>• 核对错误日志、指标和提交停滞告警；<br>• 补用例：解码失败、同局原件冲突都不评分、不推进位点，重放成功后只入账一次；<br>• 旧兼容模式不能成为全球生产语义 |
| J5 | 中 · 已复核、已登记 | gate：`ClientDispatcher.java:1432`；scene：`SceneLinkService.java:57-63`、`NodeLinkHandler.java:194-201` | 链路一断（对端读不动、15s 收不到帧、TCP 被重置），gate 就关掉其上全部会话，scene 把这些玩家移出并写回。<br>结果是一次网络抖动，就让这个 gate 在这个 scene 上的玩家全部掉线 | 按 C++ 版的做法：短暂断链时保留内存状态，恢复后只换通信绑定；实例是否消失，只看节点目录或租约。<br>Java 仓 `PARITY.md` 2026-11-05「节点链路恢复与业务状态解耦（Java 范围核对）」已登记，本次不重复登记 |

11-05 登记的「节点握手双向身份核对与失败退避（Java 待做）」第 2 项，这次一并核过：

- **旧连接顶替回推通道：不适用**。
  - gate 每个 scene 节点只有一条当前链路；握手回包要与目录里的实例和代次一致，不一致就断开（`SceneLink.java:262-289`）；
  - scene 按 gate 节点号租约的代次拒绝旧 gate（`game-scene/…/scene/link/GateLinks.java:79-90`）；
  - scene 原地重启后，指向旧实例的链路随旧进程一起断开，不会连到新进程上去顶替。
- **握手失败后的退避：确有缺口**，就是 J3。

### 7.5 已知残余与待确认

**已知残余**（有意取舍，不算缺口）

- Redisson 全局句柄会重发已写出的 EVAL。可变脚本能不能被重放，靠每个脚本自己证明。本次抽查的 6 处都处理了，但没有机械守卫保证新脚本也这样写。
- gate 推送是至多一次；审计兜底日志要人工跑工具补发。
- 玩家归属租约过期之后，节点仍服务内存里的玩家，靠 `owner_epoch` 围栏保证只丢进度、不覆盖。

**待确认**

- Nacos 2.x 客户端断线后的重连与重新订阅；Nacos 不可用时 Dubbo 注册的启动行为（`dubbo.registry.check`）。
- Redis、Dubbo 的连接状态有没有指标（§1.8）。节点链路已有建链与断链的事件指标（`GateMetrics.linkEvent`）。

### 7.6 演练清单（尚未执行，按 Java 仓 AGENTS.md 执行）

| 场景 | 应观察的结果 | 对应 |
|---|---|---|
| 运行中重启 game-friend，等它重新监听后，立刻发好友请求 | 修复前：约 60s 内回 1003；修复后：按新的重连间隔恢复 | J1 |
| TiDB 滚动重启后，立即发一批帮会与邮件写 | 修复前：每个池的首批调用报结局未知；修复后：零失败 | J2 |
| scene 端口拒绝连接，或握手一直被拒时，持续登录进这个 scene | 记录建连频率；修复后受冷却限制 | J3 |
| 开启 global 模式后，往对局结果 topic 投一条坏消息 | 按负责人 2026-11-06 确认口径：保留位点并告警，等待人工处置；单轮处理不做永久错误的无效重试。Java 对齐情况仍待验证，见主仓 §4.1 R11 与 Java PARITY 待做 | J4 |
| gate↔scene 断网 20s 后恢复 | 修复前：其上玩家全部掉线；修复后：会话保留 | J5 |
