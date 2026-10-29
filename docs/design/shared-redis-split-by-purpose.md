# 共享 Redis 按用途拆实例(G4a 设计稿)

> **状态**:设计稿,2026-10-12 起草。**未落码、未编译、未测试、未压测**。本稿只读代码、grep 和 git 得出(AGENTS.md §10.1),
> 行号以 HEAD `73b852f32` 加当前工作区(D1 映射持久化第 0、1a 批的在途改动)为准。
> **起因**:用户 2026-10-12 列出开服首日的五个瓶颈,第 3 条是「共享 Redis 只有一个主节点,所有 zone 共用」,并追问「万一归属全服库成瓶颈了呢」。
> 对应开服准备清单 L-06(`docs/design/tidb-phase1-and-launch-readiness.md:817`)和
> `docs/design/no-single-node-horizontal-scaling-20261001.md` §5(`:234-237`,当时写「方向待定」)。
> **范围**:只按用途拆实例。不改任何键名格式,不改任何 Lua,不上 Redis Cluster,不做哨兵 HA。HA 与集群化是后续项(§13)。

> **2026-10-18 修订注(先读;上面状态块原文保留)**:
> - **目标改为 DAU 1 亿、峰值同时在线 PCU 2000 万**(负责人 2026-10-15 确认,[capacity-model-hok-scale.md](capacity-model-hok-scale.md) §7.2)。按在线人数增长的项不能把旧 S3(1000 万)的数字乘二,口径与重算见 §16。本稿原文的容量判断是按全服 10 万~S1 写的,只作历史参考。
> - **本稿合并全局单点交接**([handoff-global-singletons-10m-ccu-20261013.md](../handoff/handoff-global-singletons-10m-ccu-20261013.md))§5.1 与 §8 第 1、3、5 条(D12 / CZ-2 改写、冷玩家卸载阈值、账号权威落库),不另起文档。合并要求的出处是开服线交接 §4 第 5 项([handoff-launch-readiness-and-d1-20261013.md](../handoff/handoff-launch-readiness-and-d1-20261013.md)),不是全局单点交接 10-16 的订正。
> - **范围扩大**:从「只按用途拆实例、不上 Redis Cluster」扩为「按用途拆 + 单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」。路线已由负责人 2026-10-17 拍板(PROGRESS「2026-10-17 开服线:负责人拍板四项」第 1 条):单元化与分片是叠加,不是二选一。但分片、改名、按组分实例本身仍是**设计,第 1~4 批都未落码**(`tools/scripts/k8s_deploy.ps1` 里没有 `-RedisSplit`),落码前按 §24 的门禁逐批立项、逐批授权。
> - **第 0 批五项已落码**,全部未编译、未测试、未验证,见 §17。
> - §1~§15 里被取代的结论已逐处加了带日期的更正注,**冲突时以 §16~§26 为准**。最要紧的三处:控制面不再并入 redis-mapping,改为按 zone 组分实例(§18);redis-cache 改为必做(§19);核心不再以全服单主为终态(§20)。另有一条围栏硬依赖必须在第 2、3 批之前解决:令牌发布脚本把排序锁、子表缓存、资产证据与 `save_barrier` 绑在同一段 Lua 里(§18.7)。
> - 证据口径:[C] 读代码确认,行号以 2026-10-18 的主工作区为准(多个会话在改,引用时以函数名为主);[D] 文档记载;[I] 推断。**§16~§26 的容量数字全部是推算 [I],未实测**,不据此宣称任何性能结论(AGENTS.md §6)。
>
> 新章节索引:
> - §16 修订依据与 2000 万口径重算
> - §17 第 0 批五项的定位与现状
> - §18 控制面按 zone 组分实例(取代 §3.2.3;§18.7 是 go/db Redis 句柄承载的键全集与围栏硬依赖)
> - §19 redis-cache 改为必做
> - §20 核心路线:单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片(D12 / CZ-2 改写)
> - §21 mapping 三族 hash tag 的裁决材料
> - §22 账号目录落库
> - §23 核心内存上界与冷卸载(S1 前拍板)
> - §24 新分批表
> - §25 待负责人拍板清单
> - §26 Java 版

> **2026-10-23 注(上面原文保留)**:负责人 10-23 拍板,玩家归属 / 落点映射改为直读 TiDB,撤掉 Redis 映射副本(经「账号登录逻辑/物理服瓶颈」会话转述;设计见 [player-route-mapping-durability-proposal.md](player-route-mapping-durability-proposal.md) §15「2026-10-23 负责人拍板:映射改为直读 TiDB,撤掉 Redis 副本」,设计已定、未落码)。本稿以下内容**被取代,以该 §15 为准**:redis-mapping 实例及其迁移(§3.2.2、§5.4)、mapping 三族 hash tag(§21、§25.2 第 7 条及子项)、redis-mapping 的 HA 优先级(§25.2 第 18 条第一项)。句柄去留按该稿 §15.4:go/db `Placement.Redis` 删除;guild `MergeMarkerRedis` 删除,改读 SQL 围栏;data_service `MappingRedis` 保留,只剩玩家写锁一个用途;`player:zone` / `placement` / `maprev`、`mapping:route:*`、`merge:in_progress:<z>` 围栏镜像(以 TiDB `zone_merge_fence` 为准)、`db:capability:zone:<z>` 都移出 Redis(2026-10-23 映射线定稿后补)。控制面按 zone 组分实例(§18)与核心路线(§20)不受影响。

> **2026-10-24 注(上面原文保留)**:用户 10-24 明确「玩家数据不要 redis 存储,其他改用的地方还是要用」,记录与键族分类见 [TiDB 决策 §9](global-data-layer-tidb-decision.md)。本稿受影响的有三处:
> - §19 redis-cache:它要装的子表缓存与玩家读结果都移出 Redis,「必做」结论待重评。
> - go/db 控制面按组分实例(§18、§24 第 2 批)是否还要,取决于新存盘路径是否保留 Kafka → go/db(TiDB 决策 §9.3 B、§9.4 第 1 条);§18.7 令牌发布脚本的硬依赖与 A / B 二选一随之重评。
> - §16 里按父快照、补投意图、子表缓存、资产证据推算的核心与缓存的 ops、内存、写带宽要按剩下的键重算;§23 的冷卸载以父键为对象,父快照移出后失去对象,核心内存上界按剩下的键重算。
>
> 排队、登录会话等按用途拆实例的部分不受本条影响。只定了方向,未设计、未落码。

## 1. 结论

1. **拆成「1 个核心 + 2 个新实例 + 1 个可选实例」**:

   | 实例(K8s Service) | 用途 | 批次 |
   |---|---|---|
   | `redis`(原实例,不改名、不重建) | 玩家归属与运行时核心:会话、锁、`PlayerAllData` 父键、归属 epoch / 位置 / 交接标记、场景路由、战斗与组队契约、账号目录 | 现状 |
   | `redis-queue` | 排队与限流:login 排队 / 准入 / gate 排空标记,Java gateway 的 Bucket4j 令牌桶 | 第 1 批 |
   | `redis-mapping` | 映射:归属、落点、合服围栏、能力标记;第 2 批起再并入 go/db 存盘控制面(已落库游标、applied epoch、排序锁、重试收据) | 第 1 / 2 批 |
   | `redis-cache`(可选) | 子表缓存与读结果:`PlayerDatabase*:{id}` 子表缓存、`task:result:*` | 第 3 批 |

   - **(2026-10-18 更正,上表原文保留)** 2000 万口径下:redis-mapping 行「第 2 批起再并入 go/db 存盘控制面」作废,控制面按 zone 组分实例 `redis-dbctl-g<N>`(§18);redis-cache 由「可选」改为必做(§19);核心 `redis` 不再以全服单主为终态,改为单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片(§20)。

2. **任务提出的四类里,有两类不能照字面拆**:
   - 「会话与锁」不能单独成实例。login 的身份补齐脚本在一段 Lua 里同时读写 `PlayerAllData` 父键、玩家登录锁、`player:session`、`player:{id}:location`(§2.3)。
   - 「PlayerAllData 缓存」只有**子表缓存和读结果**能挪走。父键 `PlayerAllData:{id}` 被 C++ 存盘 Lua 与 `player:{id}:owner_epoch` 原子绑定;C++ 每个线程只有一条 Redis 连接,还依赖这条连接的先后顺序(§2.4)。
     - **(2026-10-18 更正,上句原文保留)** C++ 是**每进程一条** zone Redis 连接,不是每线程:`tlsRedis` 虽声明为 thread_local(`cpp/libs/engine/thread_context/redis_manager.h:122`),全仓唯一建连点是 `node.cpp` 的 `tlsRedis.Connect`(当前 :710)。依赖同连接先后顺序的点已远不止 §2.4 列的两处,完整清单见 §20.6。另:「子表缓存能挪走」仍成立,但子表缓存写入已与排序锁、资产证据、`save_barrier` 绑进同一段令牌发布脚本,挪走前要先过 §18.7 的硬前置。
   - 所以这些键只能一起留在核心实例。
3. **拆完后核心仍是全服单主**。拆分只做两件事:把最吵的流量(排队轮询、限流)挪走;把要求最严的状态(映射、游标:丢一个键就静默写错库)挪到可以单独配 noeviction、WAITAOF、回档守卫和容量告警的实例上。核心的容量和单点问题,下一步只能靠哨兵 HA(只解决可用性)或区域集群(`tidb-phase1-and-launch-readiness.md` §8)。
   - **(2026-10-18 更正,上条原文保留)** 2000 万口径下,第 0 批与按用途拆分都做完之后,核心(不含子表缓存、令牌发布脚本、friend 登录 MGET、排队)推算约 119 万~127 万 ops/s,在线热集约 200~420 GB,常驻至少 1~2 TB(§16,推算 [I],未实测)。所以「拆完后核心仍是全服单主」只能是过渡态:路线已拍板为单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片(§20)。哨兵 HA 只解决可用性(redis-ha 已由 `k8s_deploy.ps1 -RedisReplicas` 接入:origin `43284eb0b`(10-16)接入,本地 10-19 补齐;非 dev 档缺省 3 副本,已落码、未验证,见 §2.5 第 6 条的 10-19 更正与 §17。本括注 10-19 改写,10-22 订正接入日期)。
4. **默认全关**:部署参数 `-RedisSplit` 缺省为空时,拆分相关的渲染结果与今天逐字节相同。本机 docker-compose 继续单实例(`no-single-node-horizontal-scaling-20261001.md:27`)。
   第 1a 批只有两处**有意的缺省即生效**改动,都不属于拆分本身,单独声明:
   - release 档 gateway ConfigMap 补 `spring.data.redis.password`(修 §2.5 第 4 条)。只在注入的口令非空时写,所以 dev 档渲染逐字节不变,release 档会多一行;
   - 回档 FLUSHDB 守卫补核心族(§4.6)。今天核心上已经有映射族,守卫本来就会拒绝,所以现在看不到行为变化;拆分后它防的是「核心上没有映射族了,FLUSHDB 被放行」。
   login 的 Dial / Read / WriteTimeout 没传进 client(§2.5 第 8 条)**不在本稿修**:传进去会让核心 client 的 DialTimeout 从 go-redis 缺省 5s 变成 3s,这是与拆分无关的行为变化,另行登记。
   - **(2026-10-18 更正,上面第二处有意改动原文保留)** 回档里已经没有 FLUSHDB 了:存盘管线 R8(PROGRESS「2026-10-18 存盘管线剩余代码落码收尾：R1–R8」)按负责人 10-17「丢弃 TargetTime 之后的写」的裁决重写了 `tools/scripts/k8s_zone_rollback.ps1`,FLUSHDB 连同 `Get-SharedRedisGuardPatterns` 等防护函数一并删除(该脚本 :19-20、:392-394;回归 `tools/scripts/tests/k8s_zone_rollback_discard.tests.ps1` :89-95 断言这些函数不存在),`-ConfirmFlushSharedRedis` 改为入口拒绝。「回档 FLUSHDB 守卫补核心族」这一条随之失效,第 1a 批不再有这项改动。拆分后的回档改由尚未落地的 `go/db/cmd/zone_rollback_discard` 按 zone 精确丢弃 T 之后的 Redis 状态,它要覆盖每个用途 / 组实例(§18.6)。
5. **迁移顺序固定为**:停写 → 拷贝 → 校验 → 切换 → 解冻 → 清理旧实例。双写、双读都不采用(§5.1)。回退走同一顺序,但用「先清目标再拷」的 `restore`,不能用只补缺的 `delta`(§5.8)。
6. **所有新实例都设 `noeviction`,并显式写 `maxmemory`**。release 档启用了某个用途、却没给它的 `maxmemory`,部署直接拒绝。核心的 `maxmemory` 由 §13 第 4 条拍板,不给就保持现状(Redis 缺省即 noeviction,只是没有上限)。
   - **(2026-10-18 补充,上条原文保留)** 核心的 maxmemory 与冷玩家卸载改为 S1 开服前必须拍板,材料见 §23、§25。
7. 关于「归属全服库会不会成瓶颈」,Redis 这一侧的回答见 §9:归属目录本身的读写是 O(1) 小键,先到的是第 2 批并进来的存盘控制面。届时把控制面再拆成第四个实例只需改配置。
   - **(2026-10-18 更正,上条原文保留)** 2000 万口径下,mapping 读推算约 20 万~47 万 ops/s、内存约 100~300 GB(§16,[I]),单实例放不下,必须按 `{player_id}` 分片(§20、§21)。控制面也不再并进 redis-mapping(§18),「先到的是第 2 批并进来的存盘控制面」作废。

## 2. 盘点

### 2.1 谁在连共享 Redis

| 运行时 / 服务 | 句柄(配置键) | 客户端与连接方式 | K8s 现在指向 | 备注 |
|---|---|---|---|---|
| C++ gate / scene / battle | `zoneredis`(`cpp/libs/engine/config/config.cpp:283-306`) | muduo hiredis。每个线程一条连接 `tlsRedis.GetZoneRedis()`(`cpp/libs/engine/thread_context/redis_manager.h:25-28`、`:56`);启动时只解析一次地址(`cpp/libs/engine/core/node/system/node/node.cpp:541-557`) | `redis.<infra>`,password 写空串(`tools/scripts/k8s_deploy.ps1:1866-1874`) | **不发 AUTH,也不发 SELECT**:password、db 读进了配置,但没有任何代码使用(§2.5 第 3 条) |
| go/login | `Node.RedisClient`(`go/login/internal/config/config.go:405-424`) | go-redis v9,单个 client(`go/login/internal/svc/servicecontext.go:107-120`) | `:3583-3590` | 还有 `AuthProviders.SaToken.Redis`(`internal/svc/auth_init.go:67-80`),K8s 不配 |
| go/db | `ServerConfig.RedisClient`(`go/db/internal/config/config.go:69-75`、`:181-186`)+ `Placement.Redis`(`:47-60`,缺省复用前者) | go-redis(`go/db/internal/svc/servicecontext.go:21-55`) | `:3456-3465`,不写 Placement 段 | 落点句柄与排序锁 / 游标 / 缓存句柄「在部署上可以是不同实例」(`go/db/internal/kafka/placement_route.go:53-55`) |
| go/data_service | `MappingRedis`(go-zero,`go/data_service/internal/config/config.go:15`);`DevRedis` / `Regions`(`:16-17`、`:505-514`) | go-zero + go-redis | MappingRedis `:3480-3483`;Regions `:3484-3490`;DevRedis `:3491-3495` | DevRedis 一配就短路 Regions(`internal/routing/router.go:433-450`) |
| go/player_locator | `RedisClient`(`go/player_locator/internal/config/config.go:11`) | go-redis | `:3699-3702` | |
| go/scene_manager | `Redis`(go-zero RedisKeyConf) | go-zero | `:3731-3735` | |
| go/match | `Redis`(共享)+ `MatchRedis`(`go/match/internal/config/config.go:21`,match 自己的 Redis Cluster) | go-zero / go-redis | `:3792-3800` | |
| go/chat | `Redis`(共享;v1 不读任何契约键,`go/chat/internal/svc/servicecontext.go:29-33`)+ `ChatRedis`(`internal/config/config.go:36`) | go-zero | `:3869-3882` | ChatRedis 复用 match 的集群 |
| go/trade | `SharedRedis`(`go/trade/internal/config/config.go:95-109`) | go-redis | `:4008-4011` | 只读 `player:{id}:location` |
| go/friend | `Redis`(共享)+ `FriendRedis`(`go/friend/internal/config/config.go:93-95`) | go-zero | `:4056-4064`;FriendRedis 刻意不配,回落共享库 | |
| go/guild | `RedisClient`(DB 2)、`PlayerLocatorRedis`(DB 0)、`MergeMarkerRedis`(`go/guild/internal/config/config.go:41-42`、`:60`;`go/guild/etc/guild.yaml:14-38`) | | **K8s 上没有清单**(no-single-node §7) | |
| Java gateway | `spring.data.redis.*`(`java/gateway_node/src/main/resources/application.yaml:20-23`);Bucket4j 用专门建的 Lettuce 连接(`ratelimit/RateLimitConfig.java:40-57`) | Lettuce | `:4935-4938`,**没有 password**。**按 zone 部署**:ConfigMap、Deployment、Ingress 都在 zone namespace,随 zone-up 走(`k8s_deploy.ps1:217`、`Apply-JavaSvcManifests`),不是全局服务 | `ZoneHealthProbeService` 注入了 `StringRedisTemplate`,但没用到(`service/ZoneHealthProbeService.java:31`) |
| Java auth(sa-token-auth) | `spring.data.redis.host`(`k8s_deploy.ps1:4914-4916`) | Spring Data Redis | `redis.<infra>`,没有 password | sa-token 会话。本稿不动它,留在核心;契约测试一并钉住 |
| 运维工具 | merge_zone 有五组句柄(`tools/merge_zone/main.go:252-281`);`k8s_gate_drain.ps1` 读 login 的 `Node.RedisClient`(`:312-330`、`:368`);`k8s_zone_rollback.ps1 -RedisHost`(`:844-853`);data_consistency_check(`main.go:101-103`);`route_tool` 读 data_service yaml;go/db `cmd/verifier`、`cmd/data_stress`;robot `data_stress.go:71`;data_service `cmd/debug_fetch` | | | |

> **(2026-10-18 更正,上表原文保留)**
> - C++ 行「每个线程一条连接」改为**每进程一条**:`tlsRedis` 是 thread_local(`redis_manager.h:122`),但全仓唯一建连点是 `node.cpp` 的 `tlsRedis.Connect`(当前 :710)。§4.3 的 P0-E 注订正过一次,这里补正源头。上 Cluster 后,每个 C++ 进程要连本单元每个主节点,连接数变为「进程数 × 主节点数」(§20.6)。
> - go/db 行:2026-10-17 起另有可选的只读句柄 `ServerConfig.OwnerEpochRedis`(`go/db/internal/config/config.go:107-108`),缺省复用 `RedisClient`。当前有两道闸阻止它与 `RedisClient` 分家:K8s db ConfigMap 生成器遇到显式配置就拒绝(`k8s_deploy.ps1` 约 :1919,理由是独立连接池没进 Redis 连接总账);`saveoutbox.Start` 在 owner 实例与父缓存实例不同时拒启(`go/db/internal/saveoutbox/service.go`)。go/db 的 `RedisClient` 今天必须就是核心实例,理由见 §18.7。
> - `k8s_zone_rollback.ps1 -RedisHost` 一格:回档已不再 FLUSHDB(§1 第 4 条的 2026-10-18 更正)。

### 2.2 键族清单

「写 / 读」一栏里,C++ 指 gate / scene / battle。频率的量化放在 §6.1,这里只给定性说明。

> **(2026-10-18 注)** §2.2 各表里的 `{id}` / `{z}` 等多数是占位符,不是 hash tag:例如 `player:{id}:owner_epoch` 的实际键名是 `player:<id>:owner_epoch`,不带花括号(`go/shared/ownerepoch/ownerepoch.go` 的 `OwnerEpochKey`)。真带花括号的只有 `player:{<id>}:save_barrier`(`go/shared/savebarrier/version.go` 的 `Key`)、`player:{<id>}:asset_sql_commit`(`go/shared/assetop/sql_commit.go`)与 data_service 的 `player:{%d}:*`(`data_logic.go`、`router.go` 的 `__lock`)。上 Cluster 时以 §20.3 为准。

**A. 排队与准入(login)**

| 键 | 写 | 读 | 频率 | TTL | 多键原子 | 证据 |
|---|---|---|---|---|---|---|
| `queue:zone:{z}` ZSET、`queue:meta:{qid}` HASH、`queue:token:{token}` | login Enqueue | login Lookup(每次轮询 ≥4–5 条命令) | 排队开启时最高频 | `QueueEntryTTL` 1h | Enqueue 用 MULTI 写三键 | `go/login/internal/logic/pkg/loginqueue/queue.go:138-142`、`:170-183`、`:226-284` |
| `admit:{qid}`、`admitted:zset:zone:{z}` ZSET(成员逐个到期;2026-10-13 G4b-G 起,此前是 `admitted:zone:{z}` SET) | login 放行循环、快速通道 | login | 中 | `admit:{qid}` 为 `AdmitTTL` 60s;占位成员按分数到期,整键 TTL `AdmitTTL + 1min` 只做垃圾回收 | 放行 MULTI(`:465-469`);计数与快速通道是单键 Lua(`:495-498`、`:521-527`);取走后续期是单键 MULTI(`:387-389`) | 同上 |
| `dispatcher:lock:login_queue` | login 选主 | login | 1/s | 30s | 单键 | `go/login/internal/config/config.go:244-258`;`go/login/etc/login.yaml:436`。键是全局的,L-01 缺陷相关 |
| `gate:{id}:draining` / `gate:{id}:drained` | `k8s_gate_drain.ps1` 的多键 Lua;login 排空循环 | login AssignGate 一次 MGET(失败时放行);scene_manager 跨 zone 重定向 `drainingGateIDs` 一次 MGET(2026-10-18 起,失败时放行,经 scene_manager 自己的 Redis 连接读,只能读 DB 0) | 低 | 有 | MarkScript 同时碰本台的 draining、drained 和同 zone 其它候选的 draining | `tools/scripts/k8s_gate_drain.ps1:150-158`;`loginqueue/gatedrain.go:30`、`:47-81`;`gatedrain_monitor.go:37`、`:129-181` |

> **(2026-10-18 更正,上表「A. 排队与准入」前两行;原文保留;未编译、未测试)** 登录排队改造后(详见 `login-queue-2026-05.md` §5 决策树后的 2026-10-18 更正):排队中的每次轮询 = GET token 索引 + GETDEL 放行槽 + 一次读状态脚本(EVALSHA,内含 HMGET / ZRANK / ZCARD,剩余 TTL 不足 `QueueEntryTTL` 时才续期),共 3 次往返;放行 = 每拍一次 ZRANGE + 每放行一人一次 EVALSHA(不再 ZPOPMIN);`queue:zone:{z}`、`queue:meta:{qid}`、`queue:token:{token}` 改为空闲 TTL,至多约 1.25×`QueueEntryTTL`。脚本里只访问同一实例的键,redis-queue 仍可单独拆出。

排队默认关闭:`login.yaml:428-429`;K8s 的 login ConfigMap 不写 Queue 段,取缺省值 false。

> **(2026-10-15 更正,上表与上一句原文保留)** A 表的原子性说法与行号已过时。以下按当前 `go/login/internal/logic/pkg/loginqueue/queue.go` 核对,行号均指该文件(另注明的除外):
>
> - **第 2 行「多键原子」一格作废**,现状是:
>   - **放行**:每个出队条目一个 MULTI,里面是 `SET admit:{qid} <AdmitSlot JSON>`(TTL = `AdmitTTL`)加一段 `EVAL admittedHoldScript`(`:465-473`),两键都在排队族内。MULTI 只保证这两条连着执行、中间不插入别的命令,**不回滚**:EXEC 里一条运行期出错,另一条照样生效;`Exec` 回 error,该条目不计入放行数(`:467-469` 注释)。`ZPOPMIN`(`:433`)在 MULTI 之外:弹出之后、写入之前进程退出,这些条目既不在队列里,也不在占位里。
>   - **取走**:`GETDEL admit:{qid}`(`:335`)→ 现签 gate 票 → `HMGET queue:meta`(`:368`)→ 单段 `EVAL admittedHoldScript`(`:389`),把占位改为「现在 + 20s」到期。这几步各是独立命令,不在一个事务里,也不是 MULTI。最后那段 EVAL 只碰 `admitted:zset:zone:{z}` 一个键;它失败只记日志,占位保持放行时的分数,最迟 `AdmitTTL` 到期。
>   - **计数**:`admittedCountScript` 一段 Lua(`:531-534`),先 `ZREMRANGEBYSCORE` 清掉过期成员再 `ZCARD`。单键,而且是写命令。
>   - **快速通道**:`reserveFastPathSlotScript` 一段 Lua(`:560-568`),清过期、判 `ZCARD < budget`、`ZADD`、续整键 TTL 都在这一段里。单键。
>   - `admittedHoldScript` 定义在 `:504-510`,由 `holdAdmitted`(`:519-524`)带脚本全文以 `EVAL` 调用:排在 MULTI 里的 EVALSHA 撞上 NOSCRIPT 时来不及回退(`:515-518`)。计数与快速通道用 `Script.Run`。
> - **整键 TTL**:是 `W = max(AdmitTTL, 20s) + 1min`(`admittedKeyTTL`,`:493-495`;默认仍是 2 分钟),不是 `AdmitTTL + 1min`。放行、取走、快速通道三条写路径都先 `ZADD`,剩余 `PTTL < W` 时才 `PEXPIRE W`,**只延长不缩短**;计数不碰 TTL。20s 是交票后宽限,代码常量 `defaultHandoffGrace`(`:130`),不是配置。成员是否有效只看分数:放行时为 `now + AdmitTTL`,取走后与快速通道为 `now + 20s`。§5.8 回退里「等 `admitted:*` 过期(AdmitTTL + 1min)」同样按此读:有副本配过更大的 `AdmitTTL` 时,按其中最大的 `W` 等。
> - **第 1 行行号**:键名函数 `:171-175`;Enqueue 的 MULTI `:204-216`(写 `queue:zone`、`queue:meta`、`queue:token` 三键);Lookup `:258-317`。
> - **第 3 行(`dispatcher:lock:login_queue`)作废**:G1b(2026-10-12,对应 L-01,见 `docs/handoff/launch-readiness-20261013/spec_G1b.md`)起锁按 zone 分。实际锁键是 `dispatcher:lock:login_queue:zone:{z}`,配置项 `Queue.DispatcherLockKey` 只是前缀(`loginqueue/dispatcher.go:137-142`;`go/login/internal/config/config.go:265-271`、`:282`;`go/login/etc/login.yaml:481-484`)。同一 zone 的 login 副本抢同一把,不同 zone 互不相干,不再是全服一把键。表中「1/s」是出队拍的频率;锁键本身只在抢锁退避与心跳时读写,每个副本约 `DispatcherLockTTL/3`(默认 10s)一次(`dispatcher.go:206`、`:252`)。
> - 上一句「排队默认关闭」引的 `login.yaml:428-429`,现为 `:473-474`。

**B. 开服限流(Java gateway)**

| 键 | 写 / 读 | TTL | 证据 |
|---|---|---|---|
| `rl:zone:{z}`、`rl:ip:{ip}` | Bucket4j 做 CAS,每个请求碰 2 个桶 | 桶重填满的时间 + 1h | `AssignGateRateLimiter.java:150-154`;`RateLimitConfig.java:71-91`。默认关(`application.yaml:36-38`) |

**C. 映射(data_service `MappingRedis`)**

| 键 | 写 | 读 | TTL | 多键原子 | 证据 |
|---|---|---|---|---|---|
| `player:zone:{id}` | data_service W1(建角时一次);merge_zone | data_service 单查 / 批查、go/db MGET、merge_zone、data_consistency_check | 无 | W1 的 Lua 里复核 `merge:in_progress`;「读 home 带围栏」那段 Lua 在脚本内拼出围栏键 | `go/data_service/internal/routing/router.go:86`、`:152-237`、`:288-324`、`:366-390` |
| `player:placement:{id}` | merge_zone 钉落点 / 搬库 CAS | go/db 每个任务 MGET 两次(选库一次,复核一次) | 无 | 映射族内部 | `go/shared/placement/placement.go:30-41`;`go/db/internal/kafka/placement_route.go:90`、`:242-254` |
| `player:maprev:{id}`、`mapping:route:ready` | D1 第 1a 批只定义了这两个键 | — | 无 | 发布脚本碰 zone / placement / maprev / 围栏四键 | `go/shared/placement/route.go:21`、`:30` |
| `merge:in_progress:{z}`、`merge:merged_into:{src}` | merge_zone | data_service、guild | 前者有 TTL(D1 计划改成无 TTL),后者永久 | 映射族内部 | `router.go:110`;`tools/merge_zone/fence.go:53`;`merged_into.go:35`;`go/guild/internal/logic/merge_fence.go:62-73` |
| `db:capability:zone:{z}` | go/db 每 30s 续写 | merge_zone | 90s | 单键 | `placement.go:106-112`;`placement_route.go:384-386`、`:404`;`go/db/db.go:143-157` |
| `lock:player:{id}` | data_service | data_service | `PlayerLockTTLSec` = 3s | 与 DevRedis 上的数据锁组成**跨实例双锁**(设计如此,本来就不是原子操作) | `router.go:525-533`、`:545`;`k8s_deploy.ps1:3496` |
| `player:name:{id}` | data_service 名字缓存(含负缓存) | data_service | 缓存 | 单键 | `go/data_service/internal/routing/player_name_cache.go:28-29` |

**D. go/db 存盘控制面(go/db `ServerConfig.RedisClient`)**

| 键 | 读写 | TTL | 多键原子 | 证据 |
|---|---|---|---|---|
| `consumer:applied:{topic}:{id}:{msgType}` | 每个写任务读一次、写一次 | **无** | 单键 | `go/db/internal/kafka/key_ordered_consumer.go:420`、`:482`、`:518` |
| `consumer:applied_epoch:{topic}:{id}:{msgType}` | 每个写任务读一次,单键 Lua 推进 | **无** | 单键 | `:547-596` |
| `distributed:lock:kafka:ordering:{topic}:{id}:{msgType}` | 每个写任务 SETNX / 续期 / 释放 | 有(租约) | 单键 | `go/db/internal/locker/locker.go:29`;`key_ordered_consumer.go:1852` |
| `kafka:retry:queue\|processing:{topic}[:{inst}]`、`kafka:retry:instances:{topic}`、`kafka:dead:queue:{topic}` | 重试路径 | **无** | 多键 Lua,只在本族内 | `key_ordered_consumer.go:903-908`、`:1079`、`:1089-1104`;`retry_ownership.go:63-66`、`:107-146` |
| `kafka:retry:instance_mode:{topic}`(HASH,field = 实例名,值 = 生效模式 `owner` / `shared`)、`kafka:retry:guard:{claim\|ack}:<本实例 processing 键>:<nonce>`(STRING,认领 / 收据 ack 的防重记录)。2026-10-17 登记(R7 复核),已落码,未编译、未测试,待 Codex 验证 | 模式 HASH:续租与认领脚本在 ZADD 登记表的同一个原子操作里 HSET,回收脚本摘掉登记时 HDEL;`go/db/cmd/retry_requeue` 经 `RetryQueueDepths` 的 HMGET 读,登记着 owner 实例就以退出码 5 拒绝。防重记录:每次认领 / ack 带随机 nonce,第一次执行写入,go-redis 原样重发同一个 EVAL 时据此不再 POP、不再多删一份 | 模式 HASH **无**(与登记表同生共死);防重记录 **5 分钟**(`retryGuardTTL`) | 多键 Lua,只在本族内(认领脚本同时碰 ready、本实例 processing、登记表、防重记录与模式 HASH) | `go/db/internal/kafka/retry_ownership.go` 的 `retryInstanceModesKey`、`retryGuardKey`、`retryGuardTTL`、`retryLeaseScript`、`retryClaimScript`;`retry_routing.go` 的 `claimFrom`;`key_ordered_consumer.go` 的 `ackRetryReceiptScript` |
| `distributed:lock:kafka:consumer:lock:{playerID}`、`kafka:expand:status:{topic}` | 锁只在在线扩分区期间(或读扩分区状态出错时)用;状态每个任务读一次 | 有(5s)/ 无 | 单键 | `key_ordered_consumer.go:1820-1832`、`:1959`;`go/shared/kafkautil/expand_utils.go:104-105`。**锁键经 locker 加了 `distributed:lock:` 前缀(`locker.go:29`),而且只含 player_id、不含 topic**:它是全服共享的一把键,不能按 zone 过滤(§3.2.3、§5.2) |

只读方:merge_zone 的 preflight 与审计检查重试 / 死信队列(`tools/merge_zone/preflight.go:164-199`、`:260-270`;`audit_checks.go:153-165`);relocate 的 R2 步骤等排序锁释放(`relocate.go:367-372`)。

**E. 子表缓存与读结果**

| 键 | 写 | 读 | TTL | 证据 |
|---|---|---|---|---|
| `player_database:{id}`、`player_database_1:{id}`(键名 = proto 全名;proto 无 package、message 名小写。2026-10-19 更正:原写 `PlayerDatabase:{id}` 是错的) | go/db:写任务落库后回写;读任务回写 | 只读排障:data_service `debug_fetch -mode player-bundle`。2026-10-17 起 login 冷加载父键缺失时强制读 SQL、不采信本缓存(`ensure_player_all_data_async.go`) | `DefaultTTLSeconds`:K8s 3600s(`k8s_deploy.ps1:3458`),缺省 24h | `key_ordered_consumer.go:54-64`、`:369`、`:846-857`;`go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go:83-84` |
| `task:result:{taskID}` LIST + 频道 `task:result:notify` | go/db 写 LIST,再 PUBLISH | login 结果分发器 | 5min | `key_ordered_consumer.go:37`、`:42`、`:393-411`;`go/login/internal/dispatcher/task_result_dispatcher.go:32`、`:324`、`:370`。**单频道,PUBLISH 会扇出到所有 login 实例** |
| `player:{id}:asset_sql_commit`(2026-10-16 登记;hash:`version` = `%020d:%020d` 的 (owner_epoch, save_seq),`data` = `PlayerAssetSQLCommit`)**不是缓存,见表下说明** | go/db `RedisClient`:玩家行 SQL 提交之后发布,单键 Lua 只升不降(`assetop.PublishSQLCommit`);计划并入写路径令牌发布脚本,与排序锁、子表缓存同一次 EVAL | data_service `GetPlayerAssetOpLedger(require_sql_commit=true)`,经 `Router.ClientForPlayer`(home_zone 集群或 DevRedis);guild / trade / mail 的资产通道据此终结 | **无**,与父键同寿命:设了 TTL,已离线玩家的资产待办会永远等不到证据。回收只靠合服 / 撤销删除与回档清理;每玩家常驻一份账本(含托管收据里的完整实例字节),计入核心容量 | `go/shared/assetop/sql_commit.go`;`go/data_service/internal/logic/asset_sql_commit.go`;设计见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) §6.6 |

> **2026-10-18 更正(B0-3 第 1 批)**:task:result 的键与频道清单改为:
> - `task:result:{taskID}` LIST:不变。
> - 旧频道 `task:result:notify`:全服广播,兼容旧版 go/db、供回滚用,login 永久保留订阅。
> - 实例频道 `task:result:notify:<login 进程 UUID>`:定向投递,每个 login 进程一条,与旧频道共用同一条 Pub/Sub 连接。
>
> 频道名的唯一来源是 `go/shared/taskresult`。login 侧已落码(未编译);go/db 按 DBTask.reply_instance 定向发布是第 2 批,落地之前 go/db 仍只发旧频道。表中引用的 `task_result_dispatcher.go:32` 常量已删,改为引用 taskresult.LegacyNotifyChannel。
>
> **2026-10-18 更正(B0-3 第 2 批)**:go/db 按 DBTask.reply_instance 定向发布已落码(未编译、未验证)。`publishTaskResult` 先 LPUSH + EXPIRE,再只 PUBLISH 一次:合法发实例频道,为空或不合法回落旧频道。上一段「落地之前 go/db 仍只发旧频道」只在第 2 批上线前成立。计数用 `db_task_result_notify_total{target,result}`,告警文件是 `deploy/k8s/task-result-notify-alerts.yaml`。表中 `key_ordered_consumer.go:42` 的常量 taskResultNotifyChannel 已删。

`player:{id}:save_barrier`（2026-10-17 登记）与父快照/SQL 证明同处核心 Redis，不设 TTL；hash 的 `pending` 由 C++ 与父快照原子写入，`player_database` / `player_database_1` 由 go/db 在 SQL applied/equal 后与核令牌、子缓存和证明同一脚本 ACK。三字段均为 41 字节定宽版本，不转 Lua double。login 冷加载只在两 ACK 与 pending 同版时重建父键，不能降级为读取旧子缓存。现有 owner/父键与新屏障未同槽，只支持同一 Redis 实例；不可宣称 Cluster 原子协议已就绪。每名玩家新增一个常驻 hash，容量需按实际玩家基数计入核心预算。

merge_zone 合服与撤销时会按玩家删缓存（`playerCacheKeys`）；清单同时含资产 SQL 提交证据和 `save_barrier`。删除屏障后必须维持维护闸，从权威 SQL 核对同版两行并恢复归属/屏障才能放流，历史数据不能当新号补种。PITR 同规，见 `db-write-fencing-mysql.md` 的追加恢复门禁。

> **(2026-10-20 更正,r2-mz#1,上段原文保留)** 前向 copy 合服改用 `playerCacheKeysKeepBarrier`,**不再删 `save_barrier`**:屏障与父快照、owner_epoch 同在本节的核心实例,目标库两行是源库的逐字节拷贝,原 pending / ACK 对它们仍成立;`-verify-merged` 的 `verify:save_barrier` 逐人核对。撤销与 PITR 仍删屏障并按上段重建(合服手册 §10.1「屏障重建」)。见 `db-write-fencing-mysql.md` 文末「上线状态」之前的同日附注。

`save_barrier` 的 `outbox_*` 字段暂存两条完整 DBTask，`player:save_pending:<home_zone>:<player_id % 64>` 为同实例 ZSET 索引。每 zone 固定64桶；每个未完成玩家会额外占一份任务字节，容量按实际两表快照大小和最大积压计算，不能只按 hash 字段数估算。DB补投器专用连接池启用context截止，父Redis和mappingRedis同指针时最多2条连接，否则各最多2条；每副本均计入连接预算。只在两表 SQL ACK 与 pending 同版时原子回收字段/member；合服 P4 要求全部桶空，PITR 不得直接丢弃未提交意图。当前单实例 Redis/AOF 仍是父提交与补投恢复的前提。

**存盘协议世代与补投状态(2026-10-18 登记,`db-write-fencing-mysql.md` 文末同日附注)**:以下键与字段都和父快照、`save_barrier`、补投桶在同一核心 Redis 实例上,判定与 C++ 写入看到的是同一份事实。
- `db:save_protocol:zone:<z>`:STRING,值为 `strict`,无 TTL。`Fencing.Mode=dual` 的 go/db 启动时写入,`legacy` 实例启动前与运行中读它。只能由运维按降级流程删除:先回退写者,再排空补投桶。合服 / 回档不动它,拆分实例时必须随父快照一起迁。
- `db:save_protocol:legacy_alive:zone:<z>`:STRING,PX 30s,由 `legacy` 实例每 5s 续写,`dual` 补投器每拍 `EXISTS`。丢失的代价只是补投早几秒恢复。
- `save_barrier` 新增五个字段,全部只由 go/db 写,两表 ACK 齐全时与 4 个 `outbox_*` 意图字段一并删除:
  - `outbox_attempt_version`、`outbox_attempts`;
  - `outbox_terminal`、`outbox_terminal_reason`;
  - `outbox_retry_ms`。

  容量上每名有未完成意图的玩家多出不到 200 字节,可忽略。merge_zone 与 PITR 按玩家删 `save_barrier` 时,这些字段随之消失,不需要单独处理。

**资产 SQL 提交证据键的拆分约束(2026-10-16 登记)**:
- 它是资产通道「SQL 已提交」的证据,不是可丢的缓存:**不能**随第 3 批当缓存拆到 `redis-cache`,也不能被清缓存类操作顺手删掉而不知会(删了方向安全,但资产待办要等玩家下一次存盘才能终结)。
- **G4a 拆分时必须与写方同实例**:证据键只能放在 go/db 发布它所用的那个 Redis 上,同时读方 data_service 的 `Router.ClientForPlayer` 必须指向同一实例。今天本地与 K8s 两边都落在共享实例 DB 0;启用 Regions 集群或拆分后两边可能分家,分家即读不到证据(终结卡住)或读到旧证据。
- 证据发布并入令牌发布脚本之后,它还要与排序锁(存盘控制面,第 2 批)、子表缓存(第 3 批)同实例、同 `{player_id}` hash tag(db-write-fencing-mysql.md §6.4、§14 第 19 条);在那之前第 2 批、第 3 批不得把它们拆开。
- 回档 / PITR 必须删除受影响玩家的这把键(db-write-fencing-mysql.md §9.2;`docs/ops/mysql-backup-pitr-runbook.md` §4.1),否则证据指向已被回滚的提交。

> **(2026-10-18 更正 / 增补,上面 E 表与两段说明原文保留)**
> - **令牌发布脚本已落码**(验证口径见 §18.7),上面第 3 条的「证据发布并入令牌发布脚本之后」已成为现状:`go/db/internal/kafka/player_cache_publish.go` 的 `playerCachePublishLua`(:26)一段 Lua 的 KEYS 依次是排序锁、子表缓存、资产证据(不发布时用缓存键占位)、`player:{id}:save_barrier`(不 ACK 时占位),由 `publishDBWriteResult`(:75)经 `lease.runTokenScript` 执行。所以证据、屏障 ACK、子表缓存与排序锁今天已在同一段 Lua 里,第 2、3 批的硬前置见 §18.7。
> - **E 表「`PlayerDatabase*`」行的读者「login 进场(父键缺失时)」已不成立**:2026-10-17 起父键缺失一律经 Kafka 读任务读 SQL 两行,不再用子表缓存拼父键(`go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go` :89-96 的注释与循环;设计见 db-write-fencing-mysql.md 文末「2026-10-17 登录冷恢复与恢复运维门禁」)。`sync_loader.go` 的 `TriggerPlayerDataPreload`(:43)仍会 GET 子表缓存,但全仓没有调用方。现存读者只剩运维工具 go/db `cmd/verifier` 与 data_service `cmd/debug_fetch`。对拆分与缓存形态的含义见 §19。
> - **新增键族登记**(都在核心实例,与父缓存同一 Redis;验证口径见 §18.7,其中存盘协议世代是 10-18 之后的增量,未编译、未测试):
>   - `player:save_pending:<home_zone>:<player_id % 64>`:已有登记见上段(10-17),此处只补两处拼键点与 merge_zone P4 读者。拼键点是 `go/db/internal/saveoutbox/contract.go:40` 与 C++ `player_lifecycle.cpp` 的 `SavePlayerToRedisImpl`(约 :4413);P4 读者是 `tools/merge_zone/preflight.go` 的 `assertSaveOutboxDrained`(约 :699),逐桶 ZCARD。另有三处手拼桶键,见 §20.7 的镜像清单。
>   - `db:save_protocol:zone:<z>` 与 `db:save_protocol:legacy_alive:zone:<z>` 必须与父缓存、`save_barrier`、补投桶同一实例(`go/db/internal/saveoutbox/generation.go` 的 `GenerationKey`、`legacyAliveKey`);详见同节 2026-10-18「存盘协议世代与补投状态」登记。
>   - `player:save_barrier:maintenance:<zone>`:`go/db/internal/barrierrestore/restore.go` 的 `MaintenanceKey`(约 :442),只在停写维护窗口由 `cmd/save_barrier_restore` 使用。

**F. 核心:玩家归属与运行时(全部留在 `redis`)**

| 键 | 写 | 读 | TTL | 证据 |
|---|---|---|---|---|
| `PlayerAllData:{id}` 父键 | C++ 存盘 Lua(写入后**无 TTL**);login 组装父键(24h);login 身份补齐 Lua(KEEPTTL) | C++ 载入;login;data_service 资产账本;match 组队在线判定 | 见左 | `cpp/libs/engine/infra/storage/redis_client/redis_client.h:42-52`、`:212`、`:491`(落码前行号;2026-10-16 起该文件行号整体后移 6 行,依次按 `kSaveIfGuardLuaScript`、`AsyncLoad`、`EnqueueSave` 的键拼法定位);`dataloader/data_loader.go:44-45`;`sync_loader.go:125`;`go/data_service/internal/logic/asset_op_ledger_logic.go:25`、`:79-89`;`go/match/internal/team/presence.go:50-52` |
| `dirty_keys_set` | C++ 每次存盘 SADD(2026-10-16:两段存盘脚本的写入已在代码中删除(已落码,未编译、未测试;全部 scene 换成新镜像前生产仍在写)) | **没有读者** | 无(存量待全部 scene 升级后按 [release-checklist.md](../ops/release-checklist.md) §6.1 一次性 UNLINK) | `redis_client.h:22`、`:50`(落码前行号) |
| `player:{id}:owner_epoch` / `:handoff` / `:location` | scene_manager(铸造、CAS、回滚);C++(交接标记、取证) | C++、scene_manager、player_locator、match、trade、login(Lua)、battle 节点、merge_zone | owner_epoch 永不删;handoff 300s | `go/shared/ownerepoch/ownerepoch.go:14-56`;`cpp/libs/services/scene/player/comp/player_ownership_comp.h:54-66`;`go/shared/scenenode/locator.go:18-36`(列了这个格式的 7 处实现) |
| `player:session:{id}`、`player:location:{id}`(旧)、`player:leases`、`player:leases:processing`、`player:leases:claim_tokens`、`player:leases:claim_payloads` | player_locator 的 9 段 Lua | friend、guild、match、merge_zone、login(Lua) | 断线后 session 带 PX | `go/player_locator/internal/logic/keys.go:5-27`;`session_cas.go:29-215` |
| `account:{account}` | login,写时带建角锁做围栏 | login | **无;权威数据只有这一份,没有库可以回源** | `go/login/internal/logic/clientplayerlogin/loginlogic.go:318-345`;`createplayerlogic.go:62-73` |
| `player_to_account:{id}` | login 建角 | 回档时的孤儿清理 | 无 | `createplayerlogic.go:304-305`;`go/login/internal/constants/login_constants.go:18-22` |
| `login_account_sessions:{account}`、`login_session:{sid}`、`login_idempotent:*`、`access_token:*`、`refresh_token:*`、`account_refresh:*` | login | login | 有 | `login_constants.go:5-16`;`pkg/loginsession/loginsession.go:38-49`;`pkg/sessionmanager/session_manager.go:47-48`;`pkg/token/token.go:31-33`、`:105-112`(令牌三键走 MULTI) |
| `account_lock:create\|login:{account}`、`player_locker:{id}` | login | login | 有;玩家锁 120s(`login.yaml:88`) | `createplayerlogic.go:118`;`loginlogic.go:55`;`entergamelogic.go:103` |
| `player:{id}:login_generation`(2026-10-14 新增) | login 登录链持锁铸造(一段 Lua) | **只有铸造脚本自己读**;校验方比的是会话记录里的值 | 7 天;**可丢**:重建以毫秒时间与会话记录代次为下限,不回退,不必进清库 / 回档 / 合服清单 | `go/login/internal/logic/pkg/sessionmanager/login_generation.go`;设计见 [scene-enter-session-fence.md](scene-enter-session-fence.md) §3 |
| `player_merge_notice:{id}`、`player_force_rename:{id}` | merge_zone | login | — | `tools/merge_zone/post_merge_stamp.go:65-66`;`entergamelogic.go:494-531` |
| 场景路由:`scene:{id}:node\|zone\|mirror\|source\|mirrors\|draining\|agones_gs`、`instances:zone:{z}:active`、`instance:{id}:player_count`、`node:zone:{z}:{node}:scenes\|death_at`、`scene_nodes:zone:{z}:load`、`world_channels:*`、`world_channel_directory:zone:*`、`world_channel_lineno:*`、`world_init:lock:zone:{z}`、`enter_scene:dedup:{id}:{req}`、`player:{id}:channel_switch_cooldown`、`scene_manager:leader:lock`、`scene_manager:zones`(2026-10-16 新增:zone 登记集合,只加不减,merge_zone 摘除;见 `go/scene_manager/internal/logic/zone_registry.go`) | scene_manager | scene_manager;C++ 读频道目录(`cpp/libs/services/scene/player/system/player_scene.cpp:39`);merge_zone | 部分有 | `go/scene_manager/internal/logic/createscenelogic.go:19-40`;`load_reporter.go:23`;`reentry_barrier.go:39`;`channel_pick.go:50`;`agones_binding.go:25`;`enterscenelogic.go:204`;`world_*.go`;`go/scene_manager/scene_manager_service.go:94-96` |
| 战斗:`battle:lock:{id}`、`battle:ctx:{id}`、`battle:settlement:pending[:id]:{id}`、`battle:activity_result:*` | C++ scene;battle 节点 | match、C++ | 有;**2026-10-15 起 `battle:settlement:pending[:id]:{id}` 不再过期**(去掉 7 天 TTL,保留到应用为止,每玩家单槽) | `cpp/libs/services/scene/battle/system/player_battle.cpp:94-95`;`cpp/libs/services/battle/settlement/settlement_outbox.h:98-111`;`battle/system/battle_result_activity.h:30`;保留期改动见 [landing-recovery-sync-digest.md](landing-recovery-sync-digest.md) §7 |
| `battle:settlement:pending:index`(2026-10-15 新增,全局一把 ZSET) | battle 节点、C++ scene(与写入 / 销账同一段 Lua 内 ZADD / ZREM) | battle 节点报数(积压数、最老年龄)与孤儿清理 | 无;**可丢**:只用于监控,丢了只影响报数,不影响结算 | `cpp/libs/services/battle/settlement/settlement_outbox.h`;设计见 [landing-recovery-sync-digest.md](landing-recovery-sync-digest.md) §7 |
| `battle:settlement:staged:{battle_id}`(2026-10-19 新增,SET,成员 = player_id;审计 r2-battle#1) | battle 节点:全球确认模式发结果之前的落库脚本 `kStagePendingSettlementOnceScript`,与 pending 两键同一段 Lua 内 SADD + EXPIRE | 只有同一段脚本自己 SISMEMBER(本局是否已为该成员写过) | 24 小时;全部成员落定后 battle 立即 DEL。落库轮次进行中**不可丢**:丢了下一轮可能把已销账的 pending 写回来;一局落库结束后即无意义,不必进清库 / 回档 / 合服清单。该脚本同时跨 `{pid}` 键、全局积压索引与本键,改 Cluster 时与下文「战斗结算写入 / 销账」同批改形 | `cpp/libs/services/battle/settlement/settlement_outbox.h`;设计见 [cross-zone-matchmaking.md](cross-zone-matchmaking.md) §13.6「2026-10-19 修复」 |
| `player:{id}:sync_digest`(2026-10-15 新增,hash) | friend / match(team)/ guild 在业务提交后 bump(一段单键 Lua) | C++ scene(`HGETALL`,`player_landing.cpp`) | 30 天,每次 bump 续期;**可丢**:丢失或过期即代次 `g` 改变,客户端全量拉一次;不必进清库 / 回档 / 合服清单,但回档 Go 域权威数据(friend / guild 库)的工具应删除受影响玩家的这把键 | `go/shared/playersync/digest.go`;`cpp/libs/services/scene/player/system/player_landing.h`;设计见 [landing-recovery-sync-digest.md](landing-recovery-sync-digest.md) §3 |
| trade 资产通道维护选主锁 `trade:assetop:maintenance:leader`(2026-10-15 新增;guild 的同类锁在它自己的全局 Redis,不在本实例) | trade(`go/shared/leader`,SET NX EX + 属主校验) | trade | 30 秒,续租;**可丢**:丢了只是重新选主,日对账 / 自动放弃本身靠 Claim CAS 防重 | `go/trade/internal/svc/assetchannel.go`;设计见 [landing-recovery-sync-digest.md](landing-recovery-sync-digest.md) §6.2 / §6.3 |
| 资产唤醒:频道 `assetop:wake`(Pub/Sub,不落键)、去重键 `assetop:wake:{service}:{id}`(2026-10-15 新增) | C++ scene `PUBLISH`;guild / trade 订阅后 `SET NX EX 5` | guild / trade | 去重键 5 秒;**可丢** | `cpp/libs/services/scene/player/system/player_landing.cpp`;`go/shared/assetopwake/`;设计见 [landing-recovery-sync-digest.md](landing-recovery-sync-digest.md) §5 |
| 组队:`team:rec:{tid}`、`team:{tid}`、`team:player:{pid}`、`team:invite:{pid}` | match 的 team 模块(S_COMMIT 一段 Lua 写四类) | C++(`player_team.cpp:43-45`、`:202`、`:291`、`:355`) | — | `go/match/internal/team/keys.go:9`、`:38-53` |
| data_service 玩家数据 `player:{%d}:*`(含 `__lock`、`__version`) | data_service | data_service | — | `go/data_service/internal/logic/data_logic.go:20-25`;`router.go:531-533`。Load / Save 这两个 RPC 在仓库里 grep 不到生产调用方 |

> **(2026-10-18 更正 / 增补,B0-1,上表原文保留)**
> - 会话族新增 `player:gate_index:sessions:<gate 实例 uuid>`(SET)、`player:gate_index:gates`(SET)、`player:gate_index:ready`(PX 3.5 拍)、`player:gate_index:gen`、`player:gate_index:last_audit`、`player:gate_index:audit_cooldown`(EX,审计失败后的冷却)。写者是 player_locator 的三段会话 Lua 与会话对账 leader 的守卫脚本,读者只有对账。它们**必须与 `player:session` 同实例**(同一段 Lua 内原子维护),属于核心闭包。选主锁 `player_locator:session_reconciler:leader`(TTL 30s,shared/leader)同在核心。设计见 [player-locator-gate-session-index.md](player-locator-gate-session-index.md)。
> - 更正上表会话族行的 TTL 列「断线后 session 带 PX」:会话键在任何路径下都没有 TTL。三段写 Lua 的调用方都传 0,rearm 还会 PERSIST,清理时机只由 `player:leases` 决定(见 `go/player_locator/internal/logic/setdisconnectinglogic.go` 的注释)。

> **(2026-10-19 增补,data_service 字段索引与保留名,上表原文保留)**
> - data_service 玩家数据族新增 `player:{id}:__fields`(HASH,data_service 字段索引):成员是字段名(值为空串),保留成员 `__version` 是戳;戳与 `player:{id}:__version` 逐字节相等时索引完整,全量读 / 删除按它一段 Lua 完成,否则退回 `SCAN player:{id}:*`。无 TTL,随 `DeletePlayerData` 删除,只由 `saveFieldsScript` 维护。设计见 [data_service_role_and_scope.md](data_service_role_and_scope.md)「字段索引与保留名」。
> - data_service 的字段名保留:`__` 前缀、`save_barrier`、`asset_sql_commit`。用这些名字的 Load / Save / SetField / GetField 一律拒绝(InvalidRequest);写入时字段键已存在且不是 string 也零副作用拒绝。`DeletePlayerData` 只删自有 string 键,不再连带删掉 C++ 的 `player:{id}:save_barrier` 与 go/db 的 `player:{id}:asset_sql_commit`(此前会删,覆盖 save_barrier 后 C++ 存盘脚本对该玩家永久回 error_reply)。
> - **规矩**:今后任何模块新增 `player:{%d}:` 花括号键,必须同时登记进 `go/data_service/internal/logic/data_logic.go` 的 `isReservedPlayerField`,否则 data_service 可能「抢注」同名字段把它写成 string。
> - `player:{id}:owner_epoch`、`player:{id}:handoff`、`player:{id}:location`、`player:{id}:sync_digest` 的真实键名不带花括号,不在这个命名空间内,SCAN `player:{id}:*` 匹配不到它们。

**G. 本稿不处理**:guild 私有键(DB 2,K8s 上未部署:`guild:v2:*`、`guild_rank:*`,见 `tools/merge_zone/guild_step.go:123-128`、`fence.go:239`);friend 私有键(FriendRedis 未配置时回落到共享库);match / chat 的集群键。

> **(2026-10-18 增补,上句原文保留)**
> - `redis-match-cluster`(K8s 上 match 与 chat 共用)配的是 `maxmemory 512mb` + `maxmemory-policy volatile-lru`(`deploy/k8s/manifests/infra/redis-match-cluster.yaml:27-28`),会优先淘汰带 TTL 的键。按容量模型 §4.2,match 的票据和锁不能被这样淘汰 [D]。chat 的历史 LIST 带 TTL(缺省 7 天、每次写入续期,每频道 LTRIM 只留 200 条:`go/chat/internal/config/config.go` :63-68,`chat_logic.go` 的 `appendLogScript` :88-92),在这个集群上同样会被优先淘汰。是否纳入本稿只进 §25 待拍板,不扩大本稿的实施范围(容量模型 A8 建议拆成两个集群)。
> - guild 用 DB 2(`go/guild/etc/guild.yaml:25`)。Redis Cluster 只有 DB 0,上 Cluster 前要迁出(容量模型 A6,§20.5)。

### 2.3 跨族原子性:哪些 Lua / MULTI 把几类键绑在一起

| 脚本 | 位置 | 碰到的族 | 对拆分的含义 |
|---|---|---|---|
| C++ `kSaveIfGuardLuaScript` | `redis_client.h:42-52`(落码前行号) | 父键 + `owner_epoch` + `dirty_keys_set`(2026-10-16:对 `dirty_keys_set` 的写入已在代码中删除,只剩父键 + `owner_epoch`;已落码,未编译、未测试;全部 scene 换成新镜像前生产仍在写) | 父键必须与归属 epoch 同实例 |
| C++ `kSaveLuaScript`(2026-10-16 更正:此处原写改名前的旧常量名,旧名带 AndMark 后缀;同日删 SADD 并改名) | `redis_client.h:20-24`(落码前行号) | 父键 + `dirty_keys_set`(2026-10-16:对 `dirty_keys_set` 的写入已在代码中删除,只剩父键;已落码,未编译、未测试;全部 scene 换成新镜像前生产仍在写) | 同上 |
| scene_manager 落点 CAS | `go/scene_manager/internal/logic/changesceneutil.go:153-155` | `owner_epoch` + `location` + `handoff` | 位置与 epoch 同实例 |
| scene_manager 路由失败回滚 | `owner_epoch.go:480-482` | `location` + `owner_epoch` + `handoff` | 同上 |
| scene_manager 离场 | `leavescenelogic.go:65-70` | `location` + 场景人数 + 节点人数 | 位置与场景路由同实例 |
| scene_manager 场景生命周期(多段) | `scene_atomic.go:179-303` | `scene:*` + `instances:*` + `node:*`(最多 3N 键) | 场景路由族内部 |
| C++ 交接标记 | `cpp/libs/services/scene/player/system/exit_release_mark.h:173-182`、`:253-291` | `owner_epoch` + `handoff` | |
| C++ 改派核实 | `relocate_confirm.h:134-148` | `owner_epoch` + `location` + `handoff` | |
| C++ 换图结果取证 | `player_lifecycle.h:717-742` | `handoff` + `owner_epoch` + `location` | |
| **login 身份补齐** | `go/login/internal/logic/clientplayerlogin/player_class_backfill.go:91-95`、`:179-191` | **父键 + `player_locker` + `player:session` + `location`** | **会话、锁、父键、位置必须同实例** |
| login 外观修复 | `player_class_backfill.go:157-158`、`:168-174` | `account` + `player_locker` + `account_lock:create` | 账号目录与锁同实例 |
| login 写账号 blob | `createplayerlogic.go:62-73`、`:624` | `account` + `account_lock:create` | 同上 |
| player_locator 会话生命周期(9 段) | `go/player_locator/internal/logic/session_cas.go:29-215` | `player:session` + `player:location` + `player:leases*` | 会话族内部 |
| C++ 战斗锁与结算 | `player_battle.cpp:170-226`;`settlement_outbox.h:101-111` | `battle:lock` + `battle:ctx` + 结算待销账记录 | 战斗族内部 |
| match 组队 S_COMMIT 等 | `go/match/internal/team/keys.go:9`;`scripts.go` | `team:*` 四类 | 组队族内部(C++ 读) |
| login 排队 | `queue.go:170-183`(MULTI)、`:428-431`(MULTI)、`:456-463` | 排队 / 准入 | 排队族内部 |
| gate 排空标记与撤回 | `k8s_gate_drain.ps1:150-158` | `gate:{id}:draining` + `drained` + 同 zone 其它台的 draining | 排队族内部 |
| login 签令牌 | `token.go:105-112`(MULTI) | `access_token` + `refresh_token` + `account_refresh` | 会话族内部 |
| data_service W1 注册 | `router.go:152-237` | `player:zone` + `merge:in_progress` | 映射族内部 |
| data_service 读 home 带围栏 | `router.go:299-324` | `player:zone` + 脚本内拼出的围栏键 | 映射族内部(脚本内拼键,单实例才成立) |
| D1 发布脚本(db_first 模式) | `go/shared/placement/route.go` 的 `PublishRouteScript` | zone / placement / maprev / 围栏 | 映射族内部 |
| merge_zone 改映射、relocate CAS、merged_into | `tools/merge_zone/merge_run.go:839-951`;`relocate.go`;`merged_into.go:93-119` | 映射族内部 | |
| go/db 重试收据 | `retry_ownership.go:107-146`;`key_ordered_consumer.go:1089-1104` | `kafka:retry*` | 控制面内部 |
| data_service 删除时的双锁 | `router.go:545` 起(`AcquirePlayerLock`) | `lock:player`(映射)+ `player:{id}:__lock`(DevRedis) | **本来就按跨实例设计**,不依赖原子性 |

> **(2026-10-15 更正,上表原文保留)**「login 排队」一行的行号已漂移。当前 `queue.go`:入队 MULTI `:204-216`;放行 MULTI `:465-473`(`SET` 放行槽 + `EVAL admittedHoldScript`);取走后的单段 `EVAL` `:389`;计数与快速通道各一段 Lua,`:531-534`、`:560-568`。这些都只碰排队族的键,「排队族内部」的结论不变。原子性细节见 §2.2 A 表后的更正注。

> **(2026-10-18 增补,B0-1,上表原文保留)**「player_locator 会话生命周期」一行:replace / setDisconnecting / deleteIfUnchanged 三段会话 Lua 现在还会碰 `player:gate_index:sessions:<uuid>` 与 `player:gate_index:gates`;对账新增的守卫脚本(摘成员、补成员、释放空索引)碰 `player:session`、索引与登记集。这些仍在会话族内部,不改变拆分结论。

**结论**:
- 父键、`owner_epoch`、`handoff`、`location`、`player:session`、`player_locker`、`account`、`account_lock`、场景路由这一组键,通过上表的脚本两两相连,构成一个闭包。它们只能整体留在同一个实例。
- 排队、映射、控制面、子表缓存与读结果这几族,多键操作都只在自己族内。它们各自可以独立成实例。

> **(2026-10-18 更正,上面两条结论原文保留)** 第二条对**控制面与子表缓存**已不成立。2026-10-16~17 落码的脚本把它们绑进了核心闭包(验证口径见 §18.7),上表没有列:
>
> | 脚本 | 位置 | 碰到的族 | 含义 |
> |---|---|---|---|
> | go/db 令牌发布脚本 `playerCachePublishLua` | `go/db/internal/kafka/player_cache_publish.go:26`;调用方 `publishDBWriteResult`(:75) | 排序锁(控制面)+ 子表缓存 + `player:{id}:asset_sql_commit` + `player:{id}:save_barrier`(核心闭包) | 控制面、缓存与核心在同一段 Lua 里;拆到不同实例后无法原子执行(§18.7) |
> | go/db 推进脚本 `appliedAdvanceLua` | 同文件 :57;调用方 `advanceApplied`(:129) | 排序锁 + 游标 + 已落库 epoch | 控制面内部;分片时三键须同 tag(db-write-fencing-mysql.md §5.5) |
> | C++ `kSaveWithCommitBarrierLuaScript` | `cpp/libs/engine/infra/storage/redis_client/redis_client.h:66`;调用方 `SaveWithCommitBarrier`(:344),由 `player_lifecycle.cpp` 的 `SavePlayerToRedisImpl`(:4443 一带)发起 | 父键 + 旧格式 `player:<id>:owner_epoch` + `player:{<id>}:save_barrier` + `player:save_pending:<home_zone>:<bucket>` | 父键、归属、屏障与按 zone 分桶的待投索引同一段 Lua;取代 `kSaveIfGuardLuaScript` 成为 owner_epoch≠0 的主路径 |
> | C++ `BeginTravelHandoff` 三键 Lua | `player_lifecycle.cpp:5330` 起(`handoff_sql_barrier::kPublishMarkLua`,:5475 一带) | 旧 owner 键 + `save_barrier` + 旧 handoff 键 | 交接标记以两表 SQL ACK 为前置 |
> | login 冷加载 `beginParentLoadScript` / `publishParentLoadScript` | `go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go`,键表 `parentBarrierKeys`(:67-68) | 父键 + 旧格式 owner_epoch + `save_barrier` | 核心闭包内部 |
> | go/db 补投 `claimScript` / `migrateScript` 等 | `go/db/internal/saveoutbox/scripts.go`(:22 起、:104 起) | `save_barrier` + 一个或两个 `player:save_pending:*` 桶 | 屏障与按 zone 分桶的索引同一段 Lua |
> | go/db 消费侧终态 / 延后标记 | `go/db/internal/kafka/save_outbox_note.go` :56、:73 → `saveoutbox.MarkTerminal` / `NoteRetryDeferred` | 单键写 `save_barrier`,但走 go/db 的 `RedisClient` | go/db `RedisClient` 必须是核心实例(§18.7) |
> | D1 读方 `RouteReadyReadScript` | `go/shared/placement/route_ready.go`(约 :186)。已接入:go/db `route_ready_gate.go`(`RequireRouteReady` 开时 lookup 与复核两步都走,每条写任务多 4 次 INFO)、data_service `route_read.go`(批量就绪读;`getHomeZoneFenceReadyScript` 用 ARGV 前缀在脚本内拼围栏键)、guild `merge_fence.go` | `mapping:route:ready` + 映射键,并在脚本内读本实例身份 | 映射族内部,但全局标记与按玩家的键同一段 Lua,分片时要改形(§21) |
>
> 结论改为:**排队、映射可以独立成实例;控制面与子表缓存今天经令牌发布脚本与核心闭包相连,要先解决 §18.7 的二选一才能拆。**
>
> 例外:`save_barrier_restore` 的 `restoreScript`(`go/db/internal/barrierrestore/scripts.go`,键表由 `restore.go` 的 `recoveryKeys` 拼出;已支持 `-routing-mode redis-split -mapping-redis`,拆 mapping 时映射改走 mapping 句柄(`inspectMapping`),脚本只带前 72 把键,不会拒绝;但子表缓存 EXISTS 在父实例上执行,缓存拆走后恒为 0,「缓存不存在」前置被空过,即 fail-open。第 3 批前要加缓存句柄,在缓存实例上核验。Cluster:`-redis` 参数说明写明不支持,代码里没有显式的 cluster 探测,靠 72~75 键的 Lua 跨槽被 Redis 以 CROSSSLOT 拒绝),以及 `zone_rollback_discard` 契约(`tools/scripts/k8s_zone_rollback.ps1` 头注的契约段与第 0 步核对:每玩家一段 Lua 跨映射、核心、缓存,并要求 `cluster_enabled=0`、单一 run_id,OwnerEpochRedis / Placement.Redis / MappingRedis 与 RedisClient 同实例同 DB;1c 之后回档 -Apply 必然拒绝)。

### 2.4 Lua 之外的跨族约束

1. **C++ 依赖单连接的先后顺序**:
   - 存盘客户端用的就是 zone 连接本身(`cpp/libs/services/scene/core/system/redis.cpp:19`)。
   - 结算时「存盘后紧跟一条 PING,PING 回来就判定已落盘」(`player_battle.cpp:385-393`)。
   - 「两条命令必须走同一条不重放的连接」(`player_lifecycle.h:734-735`)。
   - 所以 C++ 的键即使不在同一段 Lua 里,也依赖「同一实例、同一连接按顺序执行」。
   - 只要动 C++ 的键,就得先给 `RedisManager` 加第二条连接,再逐条重新论证顺序。**本稿不改 C++**。
   - **(2026-10-18 更正,上面几行原文保留)** 引用的行号已漂移:PING 快路径现在在 `player_battle.cpp` :397-409;`player_lifecycle.h` 那处「同连接先 DEL 后 MGET」已被一段 Lua 取代(`kLuaJudgeTravelOutcome` 上方注释,:740-746 一带)。依赖同连接 FIFO 的点远不止这三处,完整清单与处置见 §20.6。路线已拍板上 Cluster(§20),C++ 要接集群客户端,所以「本稿不改 C++」只对第 1~3 批成立。
2. **go/db 持锁选库与搬库 R2**:
   - 「选库结果必须在持锁期间用掉 —— 锁挡着搬库工具的等在途写完成(R2)」(`placement_route.go:258-261`)。
   - 锁属于控制面,落点记录属于映射,两者今天已经分属两个句柄。
   - 拆到两个实例之后,「go/db 先拿锁再读落点」与「工具先写冻结再查锁」仍然成立:每个实例各自线性一致,先后由单个进程的执行顺序保证。
   - 所以第 1 批只挪映射、把控制面暂时留在核心,正确性不受影响。
3. **D1 §5.2 的论证**(`docs/design/player-route-mapping-durability-proposal.md:239`):原文是「Redis 整体丢失时,游标、排序锁、重试队列也在同一实例一起丢,回源换不来可用性」。
   - 第 2 批把控制面并进 `redis-mapping` 之后,这条论证原样成立。
   - 第 1 批和第 2 批之间暂时不成立,见 §11 第 3 条。
   - **(2026-10-18 更正,上面两行原文保留)** 控制面改为按 zone 组分实例(§18),不再并进 `redis-mapping`,这条论证**永久不成立**:只丢某个 dbctl 组实例时,游标与已落库 epoch 缺失,顺序守卫按缺键放行(瓶颈文档 §4 缺口表的 G5:游标与已落库 epoch 缺键放行;处置见 db-write-fencing-mysql.md §5.6)。所以第 2 批改以条件写落地为前置(§18.3),不再依赖「一起丢」。
4. **既有契约**:
   - CZ-2:`PlayerAllData` 所在的 Redis 物理共享(`docs/design/cross-zone-scene-travel.md:31`)。
   - D12:SharedRedis 必须是全服单一实例(`docs/design/cross-zone-matchmaking.md:103`)。
   - 核心实例保持全服一份,所以这两条契约都不受影响。映射、排队、控制面不在这两条契约列举的键里。
   - **(2026-10-18 更正,上句原文保留)** 2000 万口径下「核心全服一份」不是终态。负责人 2026-10-17 拍板单元化 + 单元内 Cluster,连带把 D12 / CZ-2 改写为「逻辑单一、物理按 `{player_id}` 分片」,改写后的不变量与迁移见 §20。在 §20 的第 4 批落码之前,两份契约文档(`cross-zone-scene-travel.md`、`cross-zone-matchmaking.md`)的原文照旧有效;给它们加指向注的事归各自文档的维护线(§24)。
5. **data_service 的发现键不分 zone**:login 经 etcd 键 `dataservice.rpc` 找 data_service(`k8s_deploy.ps1:3469-3474`;`go/login/etc/login.yaml:166-170`)。
   - 也就是说,任何 zone 的 login 都可能调到任何 zone 的 data_service。
   - 所以切换映射地址时,**所有 zone 的 data_service 必须同一拍切**。

### 2.5 顺带发现的既有问题(本稿不修,只登记)

1. **`dirty_keys_set` 只写不读**。C++ 每次存盘都 SADD,全仓 grep 找不到任何读、SREM 或 DEL(只有 `redis_client.h:22`、`:50` 和 `cpp/tests/currency_test`)。结果是一个随「累计存过盘的玩家数」无限增长的大 SET。
   - **(2026-10-16 更正,上面一行原文保留)写入已在代码中删除(已落码,未编译、未测试;全部 scene 换成新镜像前生产仍在写)**:
     - 两段存盘脚本(`kSaveLuaScript`、`kSaveIfGuardLuaScript`)都删了 SADD,只访问 KEYS[] 声明的键。
     - 回归用例在 currency_test:
       - `RedisSaveScriptsTouchOnlyDeclaredKeys` 不连 Redis,断言三点:每条 redis.call 的首参整串恰好是 KEYS[n];除命令名外没有字符串字面量;脚本不含旧集合名。
       - `RedisSaveLeavesNoGlobalDirtySetAgainstRealRedis` 连真 Redis。
     - 写频口径:周期存盘 + 进退场约 7.8 万/s(推算),另加战斗结算、资产提交、收件箱、跨节点交接触发的事件驱动存盘(未量化)。
     - 存量键的一次性清理写在 [release-checklist.md](../ops/release-checklist.md) §6.1:门禁是版本盘点,只核对业务容器,快照镜像按 /app/BUILD_INFO 的 commit 判定;IDLETIME 只作辅助;先 ROLE 确认是主;UNLINK 不用 DEL;24 小时后 EXISTS 复查;从备份恢复后重做。
     - 删掉它不等于核心可上 Redis Cluster:守卫脚本的父键与 owner_epoch 键仍无共享 hash tag,C++ 也没有 MOVED/ASK 处理。
2. **核心内存无上界**:
   - `PlayerAllData:{id}` 一经 C++ 存盘就没有 TTL,全仓也 grep 不到删除点。
   - `owner_epoch` 按设计永不删除(`redis_client.h:31-37` 的注释;2026-10-16:落码前行号,现见 `kSaveIfGuardLuaScript` 上方注释)。
   - 所以核心的内存随「累计进过场景的玩家数」线性增长。
   - redis-oom 问答记录里规划的「冷玩家主动卸载」没有实现(`docs/notes/redis-oom-data-safety-qa-record.md:314-316`)。
   - **(2026-10-18 更正,第 1、2 条原文保留)** 第 1 条已作为第 0 批 B0-2 落码(§17);第 2 条改为 S1 开服前必须拍板,材料见 §23。2026-10-16~17 又加了两类不设 TTL、按累计玩家常驻的键:`player:{<id>}:save_barrier`、`player:{<id>}:asset_sql_commit`;另有 `player:gate_index:*`,它的成员随会话增删,与在线人数成正比,不按累计玩家常驻。容量见 §16。
3. **C++ 不发 AUTH / SELECT**:
   - 现状:`node.cpp:541-557` 建连时不发;`config.cpp:295-301` 读了 password / db,但没有任何代码使用;K8s 写的是空串(`k8s_deploy.ps1:1869`)。
   - 冲突:release 档注入 ≥12 位密码(`:452-453`),`redis.yaml:93-97` 随之开 requirepass。这时 C++ 节点的所有命令都会 NOAUTH。
   - `docs/PROGRESS.md:5440` 的 release 实测清单也没有列 C++ 和 Java。
   - **(2026-10-15 更正,上面三行原文保留)AUTH 已落码,两批都未经 Codex 编译 / 测试**(P0-A,`docs/handoff/global-singletons-p0-findings-20261014.md`):
     - 第一批(C++):`RedisManager` 每条新连接先 AUTH,回 `+OK` 才对业务可见;认证被拒 / 超时 fail-closed(zone Redis 保持不可用,按重连间隔重试)。口令来自 `zoneredis.password`,环境变量 `ZONE_REDIS_PASSWORD` 可覆盖;空口令不发 AUTH。SELECT 仍不发,库号钉在 0。
     - 第二批(K8s,方案 A):`k8s_deploy.ps1` 的 `New-NodeConfigMapYaml` 把 `MMORPG_REDIS_PASSWORD` 明文写进 `node-config` / `battle-node-config` 的 `zoneredis.password`(与 Go 服务 / gateway 的 ConfigMap 同一做法);C++ 工作负载 Pod 模板带只哈希口令的指纹注解 `mmorpg.io/zone-redis-auth-hash`,换口令即滚动。上线顺序见 `deploy/k8s/README.md`「C++ 节点的 zone Redis 口令」。
4. **Java gateway 的 K8s ConfigMap 没写 `spring.data.redis.password`**(`k8s_deploy.ps1:4935-4938`)。release 档一开限流就 NOAUTH,限流按设计 fail-open。
5. 9 处代码注释仍写「共享库是 allkeys-lfu」(D1 §11.2 已登记)。
   - **(2026-10-18 更正,上句原文保留)** 实际是 12 处:go 下 10 处,另有 `tools/scripts/k8s_deploy.ps1` 渲染进 friend ConfigMap 的一处注释、`deploy/k8s/README.md` 一处。10-16 已改 9 处(PROGRESS「2026-10-16 共享 Redis 淘汰策略过时注释更正」),改法不再写那个策略名,历史值交给 git 历史。2026-10-18 在主工作区只读复核(`git grep` 范围 go / cpp / java / tools / deploy),余下的提及都已不是错误断言:
     - `deploy/k8s/README.md`(约 :809)与 `tools/scripts/k8s_deploy.ps1`(约 :9338,friend ConfigMap 的 here-string 内)原句仍在,但其他会话已就地追加 2026-10-18 更正:compose 与 K8s 现均为 noeviction,不再淘汰、写满时拒写。
     - `go/match/etc/match_service.yaml`(约 :130)与 `deploy/k8s/manifests/infra/redis-rank.yaml`(约 :5)是条件句(「若配了…之类的淘汰策略」「在…的库上会被驱逐」),字面不错。
     - `deploy/docker-compose.yml` :240、:242 是历史说明,保留。
   - **(2026-10-22 更正,上面几行原文保留)** README friend 段与 `k8s_deploy.ps1` friend ConfigMap here-string 两处的原句连同 10-18 括注,已于 2026-10-19 改写为现状口径(共享库 noeviction、不设 maxmemory、回落只是过渡,附 10-19 更正说明);`match_service.yaml` 评分段改写为限定条件(GlobalMatch 关闭、回落共享库、共享库被配成 allkeys-* 三者同时成立)。随 `c35e1133d` 入库。`redis-rank.yaml` :4-5「字面不错」的判定只对 allkeys-lfu 那半句的条件读法成立:volatile-lru 不淘汰无 TTL 键,那半句有误,已在该文件追加 2026-10-22 更正注。行号会漂移,以锚定文字为准。
6. `redis-ha.yaml:3` 提到的 `-RedisReplicas` 参数在部署脚本里不存在(`tidb-phase1-and-launch-readiness.md:727`)。
   - **(2026-10-19 更正,上面一行原文保留)已不成立**:`k8s_deploy.ps1 -RedisReplicas`(0 / 1 / 3,0 = 按档位)已接入,核心实例取 3 时 apply `redis-ha.yaml` 代替 `redis.yaml`;
     origin `43284eb0b`(10-16)接入,本地 10-19 补齐。`redis-ha.yaml` 文件头已改写为接入说明。已落码,契约测试未运行、未在 kind 复测,待 Codex 验证;
     详见 `no-single-node-horizontal-scaling-20261001.md` §5.1。
7. merge_zone 的 `-notice-redis-*` 一个句柄混了四类:合服提示、`player:session`、go/db 重试队列、go/db 排序锁(`tools/merge_zone/preflight.go:211`;`relocate_run.go:339`)。
8. K8s 给 login `Node.RedisClient` 写了 Dial / Read / WriteTimeout(`k8s_deploy.ps1:3587-3590`),但构造 client 时没传(`go/login/internal/svc/servicecontext.go:111-116`),实际用的是 go-redis 缺省值。

## 3. 拆分方案

### 3.1 目标拓扑

| 实例 | 键族(§2.2) | 客户端 | 持久化 | 淘汰策略 |
|---|---|---|---|---|
| `redis` | F 全部;第 3 批之前还有 E | C++ 全部;login、player_locator、scene_manager、match、trade、friend、chat(回落);data_service 的 DevRedis | AOF everysec(现状) | **noeviction(显式写)** |
| `redis-queue` | A + B | login 新句柄 `Node.QueueRedis`、Java gateway、`k8s_gate_drain.ps1` | AOF everysec | noeviction |
| `redis-mapping` | C(第 1 批)+ D(第 2 批) | data_service MappingRedis;go/db 的 `Placement.Redis`(第 1 批)/ `RedisClient`(第 2 批);guild MergeMarkerRedis;merge_zone;data_consistency_check;route_tool | AOF everysec + WAITAOF(D1) | **noeviction(D1 §3.3 硬前提)** |
| `redis-cache`(可选) | E | go/db 新句柄 `CacheRedis`;login 新句柄 `Node.CacheRedis` | AOF everysec | noeviction(理由见 §6.3) |

> **(2026-10-18 更正,上表原文保留)**
> - `redis-mapping` 行去掉「+ D(第 2 批)」,只承载 C 族;2000 万下还要按 `{player_id}` 分片(§21)。
> - 新增 `redis-dbctl-g<N>`(按 zone 组分的控制面实例,第 2 批):承载 D 族,客户端是该组各 zone 的 go/db 与 merge_zone 的 dbctl 句柄(§18)。
> - `redis-cache` 由「可选」改为必做,形态(Cluster 或按 zone 组分实例)待拍板(§19)。
> - `redis`(核心)改为单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片(§20);在第 4 批之前它仍是单实例。
> - 「核心」的客户端还要加上 go/db `RedisClient`:它写 `save_barrier` ACK、跑补投工作器,今天必须就是核心实例(§18.7)。

### 3.2 逐类说明与跨类约束

#### 3.2.1 排队与限流 → `redis-queue`(第 1 批)

- **能拆的理由**:A、B 两族的多键原子操作都在族内(§2.3);没有 C++ 读写者;键都带 TTL。
- **值得拆的理由**:
  - 排队一开,轮询就是开服首日最吵的流量:每次 Lookup 至少 4–5 条命令(`queue.go:226-284`)。
  - 限流对每个 HTTP 请求做 2 个桶的 CAS。
  - 拆走之后,这股峰值不会再挤压核心上的锁续期。2026-05-25 的 45k 浪涌压测里,Redis QPS 飙升导致丢锁 6.5 万次(`login.yaml:89-93`)。
- **故障隔离**:`redis-queue` 挂掉时:
  - 排空判定放行(`gatedrain.go:60-81`,原有设计);
  - Bucket4j fail-open(`application.yaml:120`);
  - **排队关闭时**:AssignGate 走快速通道,不碰 `redis-queue` 上的排队键,新登录不受影响;
  - **排队打开时**:`FreeSlots` 读 SCARD、快速通道占位、入队都在 `redis-queue` 上,任何一步出错都返回 `StatusError`;占位失败按设计不放行快速通道(fail-closed,`assigngatelogic.go:98-135`)。结果是**新登录全部失败**,直到实例恢复;
    - **(2026-10-19 更正,上句原文保留)** 「`FreeSlots` 读 SCARD」已过时:计数现为 `admitted:zset:zone:{z}` 上的 `admittedCountScript`(先 `ZREMRANGEBYSCORE` 清过期、再 `ZCARD`),快速通道占位是 `reserveFastPathSlotScript`,两者都是单键 Lua,见 §2.2 A 表后的 2026-10-15 更正。本条「排队实例挂了新登录全部失败」的结论不变,但「任一步出错都返回 `StatusError`」不准(10-22 订正):实例不可用时 `FreeSlots` 先失败并返回 `StatusError`;`QueueLen` 出错按 0 处理;占位出错不放行快速通道而转入队,入队也失败才返回 `StatusError`(`go/login/internal/logic/loginpregate/assigngatelogic.go` 的 AssignGate)。
  - **已在游戏里的玩家、断线重连的会话判定(读核心)和存盘都不受影响。** 拆分换来的是「排队实例挂了只停新登录」,不是「排队实例挂了还能登录」。
- **约束**:
  1. 同一个 zone 的 login 与 `k8s_gate_drain.ps1` 必须看同一个实例:MarkScript 会同时检查本台和同 zone 其它台的 draining(`k8s_gate_drain.ps1:150-158`)。(2026-10-18 补:scene_manager 跨 zone 重定向也读这组标记,`go/scene_manager/internal/logic/gate_redirect.go` 的 `drainingGateIDs`;它的读要与 login 同批指向排队实例,部署闸校验 queue 地址时也要把 scene_manager 算进去,否则拆分后它静默读不到标记、退回不过滤。)
  2. **切换期间所有 zone 的排队必须关着**。zone-up 是逐 zone 执行的,做不到「同一拍」;放行循环的锁是全局键(`config.go:253`),排队开着时,切换窗口里两个实例上会各有一个 leader、各有一套队列,准入翻倍。所以部署闸要读线上 login ConfigMap 的 `Queue.Enabled`,为 true 就拒绝切换(§4.4)。
     - **(2026-10-15 更正,上句原文保留)**「放行循环的锁是全局键(`config.go:253`)」已过时:G1b(2026-10-12)起每个 zone 一把锁,锁键 `<Queue.DispatcherLockKey>:zone:<Node.ZoneId>`(`loginqueue/dispatcher.go:137-142`;`config.go:265-271`、`:282`),不同 zone 不再抢同一把。结论与部署闸不变,只是理由换了:同一 zone 的 login 滚动重启时新旧副本并存、各连一个实例(§5.3),两边各在自己的实例上抢 `…:zone:{z}`,这个 zone 就有两个 leader、两套队列、两份在途占位,准入最多翻倍。
  3. 从第一个 zone 的 login 开始滚动,到最后一个 zone 滚完为止,不许执行 `k8s_gate_drain.ps1`,也不许滚动 gate:脚本按新 ConfigMap 往 `redis-queue` 写 draining,还没滚到的旧 login 读核心,看不到标记,会继续往正在排空的 gate 分人。(2026-10-18 补:scene_manager 同理,要与 login 同批切换。)
- **与 L-01 / L-02 的关系**:
  - L-01(排队 leader 锁)的修复与本拆分无关。
  - **拆分应排在 L-02(打开排队与限流)之前**。今天排队和限流都关着(`login.yaml:429`;`application.yaml:38`),切换时没有在途数据要搬。
- **不并入**:
  - friend 私有键:`FriendRedis` 要求 `Type: cluster`(`go/friend/internal/config/config.go:93-95`)。
  - chat 私有键:已经在 match 的集群里。

#### 3.2.2 映射 → `redis-mapping`(第 1 批)

> **(2026-10-23 注,本节原文保留)被取代**:映射改为直读 TiDB、撤掉 Redis 映射副本,见 [player-route-mapping-durability-proposal.md](player-route-mapping-durability-proposal.md) §15「2026-10-23 负责人拍板:映射改为直读 TiDB,撤掉 Redis 副本」。

- **能拆的理由**:
  - C 族的所有 Lua 都在族内;C++ 不读写映射键。
  - go/db 已经有独立句柄 `Placement.Redis`(`go/db/internal/config/config.go:47-60`),data_service 已经有独立的 `MappingRedis`。
  - 所以第 1 批对 go/db、data_service、guild 三方都是**纯配置改动**。
- **值得拆的理由**:
  - D1 §12 已把「mapping Redis 独立成实例」列为开服前事项(`player-route-mapping-durability-proposal.md:775-781`)。
  - 映射是唯一要求「缺键 = 权威缺席」的族。它对 noeviction、AOF、WAITAOF、回档守卫的要求,和核心上的缓存类数据不同。
- **同址契约**:go/db 读落点记录的 Redis 必须与 data_service 的 `MappingRedis` 同址,且是 DB 0(`tools/scripts/tests/k8s_deploy_contract.tests.ps1:839-860`;`k8s_deploy.ps1:3460-3465`)。
  - 第 1 批在 go/db ConfigMap 里显式写 `Placement.Redis.Hosts = redis-mapping`。
  - 契约测试里「显式写了 Placement.Redis 就以它为准」的分支(`:850-853`)原样成立。
- **全局性**:映射全服只有一份,所有 zone 必须同一拍切(§2.4 第 5 条)。
- **随之迁走的键**:`lock:player:{id}`、`player:name:{id}` 走的也是 `MappingRedis` 句柄,一起迁过去。名字缓存可以丢,迁移时不拷。
- **与 D1 的衔接**:
  - 就绪标记 `mapping:route:ready` 记着 `redis_run_id` 和 `master_replid`(proposal `:142`)。换实例等于身份变化,D1 第 1b 批的对账 leader 会撤标记、修补、再挂回(proposal `:262-270`)。
  - leader 挂标记前会自检 noeviction(proposal `:145`)。
  - RouteStore 的 WAITAOF 按 `redis-mapping` 的拓扑取值:单实例用 `NumLocal=1, NumReplicas=0`,也就是配置缺省值。

#### 3.2.3 go/db 存盘控制面 → 并入 `redis-mapping`(第 2 批)

- **为什么并进映射实例,而不是留在核心或单独开实例**:
  1. 两者都是「无 TTL、丢一个键就静默出错」的状态。游标被淘汰,顺序守卫就静默失效(`deploy/docker-compose.yml:228-231` 的注释)。
  2. D1 §5.2「一起丢」的论证恢复成立。
  3. 回档守卫本来就把这两族放在一起检查(`tools/scripts/k8s_zone_rollback.ps1:633-642`)。relocate 的 R1(写冻结,属映射)和 R2(等排序锁,属控制面)也能用一个句柄完成。
  4. 少开一个实例。
- **代价**:
  - 负载:`redis-mapping` 从「每个任务 2 次 MGET」变成「每个任务约 9 条命令」(§6.1)。
  - 内存:除了 O(累计玩家数 × 映射),还要加上 O(累计存过盘的玩家数 × 表数 × 2 个游标)。
  - 如果压测显示扛不住:第 2 批的代码已经让 go/db 的三个句柄互相独立,把控制面拆到第四个实例只需改配置(§13 第 2 条)。
- **为什么要写代码**:go/db 的 `RedisClient` 同时承担子表缓存回写和读结果发布(`key_ordered_consumer.go:353-411`、`:846-857`),这两样必须和 login 在同一个实例。
  - 所以要先加 `CacheRedis` 句柄,缺省等于 `RedisClient`。
  - 有了它,才能把 `RedisClient` 指到 `redis-mapping`,同时让缓存留在核心。
- **可以逐 zone 切,但有一个例外**:游标、epoch、排序锁、重试收据、扩分区状态都带 topic,也就是带 zone(`:420`、`:547`、`:903-908`、`:1852`),只有本 zone 的 go/db 会碰。
  - 例外是在线扩分区用的 `distributed:lock:kafka:consumer:lock:{playerID}`(`:1959`):它只含 player_id,是全服共享的一把键。逐 zone 切换期间,已切和未切的 zone 会在两个实例上各拿各的锁。
  - 所以**第 2 批从第一个 zone 开始、到最后一个 zone 切完为止,全服禁止在线扩分区**。只有扩分区中(`:1835-1838`)或读扩分区状态出错时(`:1821-1823`)才走加锁路径,平时不存在这把锁,所以禁扩分区就够了,不需要改键。读状态出错的那条路径,锁也落在同一个出错的实例上,大概率同样失败、任务延后,不构成跨实例并发。
  - 满足这一条后,可以逐个 zone 停 go/db、拷贝、切换(§5.5)。
  - `db:capability:zone:{z}` 本来就走 `Placement.Redis`,第 1 批已经在 `redis-mapping` 上了。

> **(2026-10-18 更正,本节原文保留)**
> - **总注:全文凡是写「dbctl 并入 redis-mapping」「go/db 的 `RedisClient` 指向 redis-mapping」「dbctl 必须与 mapping 同实例」的地方,一律以 §18 为准。** 控制面改为按 zone 组分实例 `redis-dbctl-g<N>`。下文 §4.1、§4.2、§4.4、§5.5、§5.8、§5.9、§6.1、§6.2、§7、§8、§9、§10、§11、§12 的相关处各加了一行短注;§7 / §8 的「dbctl 不带 mapping 就拒绝」那道闸显式作废,落码时不要实现。
> - 上面「为什么并进映射实例」四条理由里,第 2 条(D1 §5.2「一起丢」)永久不成立(§2.4 第 3 条的更正),第 4 条(少开一个实例)在 2000 万下不再是考量(控制面推算 6·DBTask 约 53 万~133 万 ops/s,含令牌发布脚本约 62 万~155 万,§16)。
> - 「为什么要写代码」一段的前提已经变了:写路径的缓存发布已并入令牌发布脚本,与排序锁同一段 Lua(§2.3 后的更正注),不能只靠加一个 `CacheRedis` 句柄把缓存留在核心、把锁挪走。硬前置见 §18.7。
> - 扩分区锁一段的分析保留(§18.2 沿用)。负责人 2026-10-13 定「`kafka:expand:status` 死代码保留」(PROGRESS「2026-10-14 存盘管线接手」§5 拍板第 5 条),所以不能照容量模型 §4.2「先删 expand 死代码再拆」执行,§18.2 改用「同一玩家只在其 home topic 上处理」来论证,并补上 home_zone 未知时的残余。

#### 3.2.4 子表缓存与读结果 → `redis-cache`(第 3 批,可选)

- **能挪的范围**:只有子表缓存和 `task:result`。父键被 §2.3 的两段 Lua 绑在核心上。
- **什么时候做**:压测(L-14)显示核心的内存或 CPU 主要花在下面两处时再做:
  - 子表缓存回写:每个写任务一次大 value 的 SET;
  - `task:result` 的 PUBLISH 扇出:单频道发给全部 login(`task_result_dispatcher.go:32`)。
  它与「`task:result` 按 login 实例分频道」(L-06 后半句)互相独立,**建议先做分频道**。
  - **2026-10-18 状态**:分频道第 1 批(proto + go/shared/taskresult + login 双订)已落码,未编译、未验证;go/db 侧是第 2 批。
  - **2026-10-18 第 2 批状态**:go/db 侧已落码,未编译、未验证。实例频道仍跟着 publishTaskResult 的 Redis 句柄(go/db 的 RedisClient)走,下面「同一实例、全服同一拍」的约束不变。发起方 login 不在,或两端频道名不一致时,由 `db_task_result_notify_total{target="instance",result="no_receiver"}` 暴露。注意 PUBLISH 的订阅者数包含模式订阅,这台 Redis 上若有人开着 `PSUBSCRIBE task:result:notify:*`,no_receiver 会被压成 0,所以 `PUBSUB NUMPAT` 应为 0。
  - 约束不变,同样适用于实例频道:实例频道跟着 publishTaskResult 的 Redis 句柄走,必须和结果 LIST、login 的 SUBSCRIBE 与 LPOP 在同一个实例上。第 3 批挪 `redis-cache` 时,go/db 与 login 仍要全服同一拍切换。
  - LIST 与频道不在同一实例的配置错误,由 login 的 `login_task_result_delivery_total{outcome="result_missing"}` 暴露。它是附加信号,不进送达率分母。
- **约束**:go/db 与 login 必须**全服同一拍**切。玩家可以在 home 以外的 zone 进场,任何 zone 的 login 都可能在等任何 home zone 的 go/db 发布结果(CZ-2)。
- **为什么可以不拷**:目标实例是空的,缺键就回源读库,结果一定是对的。迁移时不拷,切换后父键缺失的玩家首次进场多一次冷读。
- **反过来,目标上有旧缓存就不安全**:login 父键缺失时直接用子表缓存拼父键(`ensure_player_all_data_async.go:80-93`),拼出来的父键随后被 C++ 载入。子表缓存一旦比库旧,就等于把玩家进度回档。
  - 正向切换时,目标必须没有 `PlayerDatabase*:*`(§5.2 把它列为「目标必须为空」)。
  - 回退时,核心上还留着切换前的子表缓存(TTL:K8s 3600s,缺省 24h),它们没有收到切换期间的回写,比库旧。所以回退必须先在核心上删掉这些键,或者等满一个 TTL 再切回(§5.8)。

> **(2026-10-18 更正,本节原文保留)**
> - 「可选」作废,改为必做,见 §19(2000 万下子表缓存推算 0.6~1.2 TB,[I])。
> - 「反过来,目标上有旧缓存就不安全:login 父键缺失时直接用子表缓存拼父键」这条风险对 login 已不成立:2026-10-17 起父键缺失一律读 SQL 两行(§2.2 E 表后的更正注)。它仍适用于没有调用方的 `TriggerPlayerDataPreload`(`sync_loader.go:43`);若有人重新接上它,风险回来。
> - 「建议先做分频道」已落地为 B0-3 两批(§17)。分频道之后频道仍跟着 go/db 发布结果用的 Redis 句柄走,「go/db 与 login 全服同一拍切」的约束不变。
> - 挪缓存之前先过 §18.7:写路径的缓存发布与排序锁、资产证据、`save_barrier` 是同一段 Lua。

#### 3.2.5 留在核心 `redis` 的部分

- **留下的理由**:§2.3 的闭包、§2.4 的 C++ 单连接顺序、CZ-2 和 D12 两条契约。
- **核心不是缓存**。上面有两类只存在于 Redis 的权威数据:
  - 账号目录 `account:{account}`(`loginlogic.go:318-320`:「不是可重新加载的缓存」)和 `player_to_account`;
  - 写回式的 `PlayerAllData` 最新态:Kafka 落库之前,只有这一份。
  所以 noeviction 和 AOF 都不能降。
- **(2026-10-18 补充,上文原文保留)** 账号目录的落库方案见 §22;2026-10-17 起核心上又多了 `save_barrier`(pending 与两表 SQL ACK)与补投意图,它们同样是只在 Redis 的状态,丢了会让冷登录与交接 fail-closed(db-write-fencing-mysql.md 文末 2026-10-17 附注),noeviction 与 AOF 更不能降。

### 3.3 被否决的切法

| 切法 | 否决理由 |
|---|---|
| 「会话与锁」单独成实例 | 会被 login 身份补齐、外观修复、scene_manager 落点 CAS、C++ guard 这几段脚本切开(§2.3)。要拆,只能把它们改成跨实例两阶段,原子性就没了 |
| 「PlayerAllData 父键」单独成实例 | C++ guard Lua 把父键与 `owner_epoch` 原子绑定,这正是 scene-owner-reentry-barrier §6.2 方案 a 的核心;C++ 还只有一条连接 |
| 核心上 Redis Cluster | `redis-ha.yaml:10-13`;`cross-zone-matchmaking.md` §10.0 列的阻塞表 |
| 核心按 zone 拆 | 违反 CZ-2 / D12:访客数据、跨 zone 匹配读位置,都要求物理共享 |
| 映射用只读副本扩读 | go/db 必须读主节点(`go/db/internal/config/config.go:57-58`) |
| 控制面单独成第四个实例 | 备选,见 §13 第 2 条 |

> **(2026-10-18 更正,上表原文保留)**
> - 「核心上 Redis Cluster」重开,并已由负责人 2026-10-17 拍板为路线的一部分:单元内 Cluster 按 `{player_id}` 分片(§20)。表中引用的阻塞项(C++ 无集群客户端、多键 Lua 跨槽等)改为第 4 批的前置清单(§20.4~§20.6)。
> - 「核心按 zone 拆」仍否决;已拍板的是**区域单元**:每个单元若干 zone,单元内物理共享,跨单元按转区处理(§20),不是按 zone 拆。
> - 「控制面单独成第四个实例」由 §18 的「按 zone 组分实例」取代。

### 3.4 分批

| 批 | 内容 | 代码 | 数据迁移 | 停服 |
|---|---|---|---|---|
| 1a | 部署能力:`redis-purpose.yaml` 模板、`-RedisSplit`(含 dev_tools 与回档脚本的转发)、拓扑函数与安全闸、契约测试;login `QueueRedis`;`k8s_gate_drain.ps1` 读 QueueRedis;回档守卫补核心族;Java gateway 补密码 | login、PowerShell | 无 | 无(参数缺省为空;§1 第 4 条的两处有意改动除外,行为不变) |
| 1b | 迁移工具 `tools/redis_split`(含 `restore`、`unmark`、墓碑)+ 集群内 Job + 运维手册;merge_zone / data_consistency_check / route_tool 的墓碑检查 | 新 Go module、三个工具 | — | — |
| 1c(运维执行) | 切 queue、mapping;核心清单显式写 noeviction / maxmemory | 无 | 映射拷贝 | queue 无;mapping 冻结建角与合服工具 10–30 分钟(推算);核心显式化要一次核心重启(维护窗口) |
| 2 | go/db `CacheRedis`;merge_zone `-dbctl-redis-*`;dev_tools 转发参数;部署 dbctl | go/db、merge_zone、PowerShell | 控制面拷贝 | 逐 zone 停 go/db(存盘暂停,由 Kafka 缓冲) |
| 3(可选) | login `CacheRedis`;部署 cache;工具改参数 | login、merge_zone、工具 | 无(缓存不拷) | 全服一拍:停入口 + 停 go/db |

> **(2026-10-18 更正,上表原文保留)** 分批以 §24 为准:前面加第 0 批(已落码,§17),第 2 批改为 dbctl 按 zone 组分实例,第 3 批改为必做,最后加第 4 批(单元化 + 单元内 Cluster)。第 2、3 批的文件清单由各自的落码规格重列,§15 的清单不再适用。

## 4. 配置改动点

### 4.1 逐服务

| 服务 / 工具 | 句柄 | 现在 | 第 1 批后 | 第 2 批后 | 第 3 批后 | 改动类型 |
|---|---|---|---|---|---|---|
| C++ gate / scene / battle | `zoneredis` | redis | redis | redis | redis | 不变;契约测试钉住它永不指向拆出去的实例 |
| login | `Node.RedisClient` | redis | redis | redis | redis | — |
| login | `Node.QueueRedis`(新) | —(= RedisClient) | redis-queue | | | 代码 + 配置 |
| login | `Node.CacheRedis`(新) | — | — | — | redis-cache | 代码 + 配置 |
| go/db | `ServerConfig.RedisClient` | redis | redis | **redis-mapping** | | 配置 |
| go/db | `Placement.Redis` | (= RedisClient) | **redis-mapping**(显式写) | 不写(= RedisClient = redis-mapping) | | 配置 |
| go/db | `ServerConfig.CacheRedis`(新) | — | — | redis(显式写) | redis-cache | 代码 + 配置 |
| data_service | `MappingRedis` | redis | redis-mapping | | | 配置 |
| data_service | `DevRedis` / `Regions` | redis | redis | | | 不变(父键在核心) |
| guild | `MergeMarkerRedis` | (K8s 无清单) | 写清单时指向 redis-mapping | | | 配置(随 guild 清单) |
| player_locator、scene_manager、match、trade、friend、chat 的共享段 | — | redis | 不变 | | | — |
| Java gateway(按 zone 部署) | `spring.data.redis` | redis(无密码) | redis-queue + password(口令非空时才写) | | | 配置 |
| Java auth | `spring.data.redis.host` | redis | redis | redis | redis | 不变;契约测试钉住 |
| merge_zone | `-mapping-redis-*` | redis | redis-mapping | | | 运维参数 |
| merge_zone | `-dbctl-redis-*`(新) | — | — | redis-mapping | | 代码 |
| merge_zone | `-notice-redis-*` | redis | redis | redis(只剩合服提示、session、缓存失效) | 子表缓存失效改走新参数 `-cache-redis-*` | 代码 |
| `k8s_gate_drain.ps1` | 读 login ConfigMap | `Node.RedisClient` | 改读 `Node.QueueRedis`,缺省回落 | | | 代码 |
| `k8s_zone_rollback.ps1` | `-RedisHost`;新增 `-RedisSplit` | | 守卫模式补上核心族;转发 `-RedisSplit`;停服前真跑拓扑闸 | | | 代码 |
| data_consistency_check、route_tool、merge_zone | 映射句柄 | | 连到带墓碑的实例即拒绝(第 1b 批) | | | 代码 |
| data_consistency_check | `-redis-addr` | redis | redis-mapping | | | 运维参数 |
| route_tool | 读 data_service yaml | | 跟随 MappingRedis | | | — |
| go/db `cmd/verifier`、`cmd/data_stress`;robot `data_stress` | 读缓存 | redis | | | redis-cache | 运维参数 |
| data_service `cmd/debug_fetch -mode player-bundle`、`cmd/debug_import`(2026-10-19 更正) | 读写核心族(父快照、屏障、owner_epoch、会话)+ 读缓存;映射族走 db.yaml `Placement.Redis` | db.yaml `ServerConfig.RedisClient` | | | 拆分后键清单 `debugutil.PlayerRedisKeySpecs` 的实例归属要随之改 | 运维参数 |

> **(2026-10-18 更正,上表原文保留)**
> - go/db `ServerConfig.RedisClient` 的「第 2 批后 redis-mapping」作废。按 §18,控制面改指本 zone 所属组的 `redis-dbctl-g<N>`;而且今天这个句柄同时承载 `save_barrier` ACK、补投工作器与证据发布(§18.7),第 2 批不能只改它的地址,要先把句柄拆成「控制面」与「父缓存所在核心」两个。
> - go/db `Placement.Redis` 的「第 2 批后不写(= RedisClient = redis-mapping)」作废:控制面与映射分属不同实例,`Placement.Redis` 在任何拓扑下都要显式写。
> - merge_zone `-dbctl-redis-*` 的「第 2 批后 redis-mapping」作废:改为按 zone 解析到所属组实例,跨组合服怎么处理待拍板(§18.4、§25)。
> - go/db 新增的 `ServerConfig.OwnerEpochRedis`(2026-10-17)不在上表:它只读核心实例的 owner_epoch,缺省复用 `RedisClient`,当前部署不支持显式配置(§2.1 表后的更正注)。
> - C++ `zoneredis`、data_service `DevRedis` / `Regions` 一直指核心;第 4 批核心改 Cluster 后,这些句柄都要换成集群客户端(§20.6)。

### 4.2 新配置键、关键类型与函数

**login(第 1a 批)**

```go
// go/login/internal/config/config.go(NodeConfig,:405-414)
// QueueRedis 是排队 / 准入 / gate 排空标记所在的 Redis(shared-redis-split-by-purpose.md §3.2.1)。
// 整段不写 = 复用 RedisClient,而且是同一个 *redis.Client(不多开连接池),行为与拆分前逐字相同。
// 用指针 + optional 的原因:go-zero 不会为没出现的 optional 嵌套结构回填内层字段
// (同 go/db/internal/config/config.go:24-30 的说明)。段一旦出现,内层字段就按 RedisConf 的规则全部必填,
// 所以 ConfigMap 必须整段写出。
QueueRedis *RedisConf `json:"QueueRedis,optional"`
```

```go
// go/login/internal/svc/servicecontext.go
type ServiceContext struct {
    RedisClient *redis.Client
    // QueueRedis:没配置时与 RedisClient 是同一个指针。只给 loginqueue 和 gate 排空使用。
    QueueRedis *redis.Client
    // ...
}

// newRedisClient 从 :107-120 抽出来,给两个句柄共用。Ping 失败就 panic,与现状同口径;
// 错误信息带句柄名和 Host,不带口令。
// redis.Options 只填 Addr / Password / DB / DisableIndentity,与今天逐字相同。
// **不**顺带传 Dial / Read / WriteTimeout(§2.5 第 8 条):传进去会让核心 client 的 DialTimeout 从 5s 变 3s,
// 那是与拆分无关的行为变化,另行登记,不夹带(AGENTS §11.2)。
func newRedisClient(ctx context.Context, name string, c config.RedisConf) *redis.Client

// newQueueRedis:c == nil 时原样返回 core(同一个指针,不多开连接池);否则 newRedisClient(ctx, "queue", *c)。
// 单测 TestNewQueueRedis_NilConfigSharesPointer 钉的就是这个函数。
func newQueueRedis(ctx context.Context, core *redis.Client, c *config.RedisConf) *redis.Client
```

- **注入点**(loginqueue 包本来就接收 `*redis.Client` 参数,所以**包本身不改**):
  - `initLoginQueue` 里的 `gateWatcherCapacityProvider.rdb`(`servicecontext.go:430-435`);
  - `loginqueue.New`(`:442-447`);
  - `loginqueue.NewDispatcher`(`:468-480`);
  - `startGateDrainMonitor` 里的 `StartGateDrainMonitor`(`:524`)。
- **启动日志**:`newQueueRedis` 之后打一行 logx `[Redis] core=<host>/db<n> queue=<host>/db<n>|shared`;启动横幅(`go/login/login.go:466` 的 `redis:` 行)后面补一行 `queue:`,值同上。§5.3、§10 的「横幅」指的是这两处之一,所以 `login.go` 进第 1a 批文件清单。
- **关闭**:与 RedisClient 同口径,不显式 Close(现在两个服务都没有显式关闭 Redis)。

**login(第 3 批)**

- 新键:`` CacheRedis *RedisConf `json:"CacheRedis,optional"` ``。
- 用处:
  - `dispatcher.NewTaskResultDispatcher(s.CacheRedis, …)`(`servicecontext.go:202`);
  - `dataloader.EnsurePlayerAllDataInRedisAsync(ctx, parentRC, cacheRC, …)`:签名加一个参数,父键的 EXISTS / SET 仍走核心(`ensure_player_all_data_async.go:57`、`:103`、`:148`),子表的 GET 走缓存(`:84`);
  - `sync_loader.go` 同口径;
  - 调用点 `entergamelogic.go:380` 附近。

**go/db(第 2 批)**

```go
// go/db/internal/config/config.go(ServerConfig,:69-75)
// CacheRedis 是玩家子表缓存回写与读结果(task:result)所在的 Redis,必须与 login 读它们的实例相同。
// nil = 复用 RedisClient(与拆分前逐字相同)。
// 缓存 TTL 仍取 RedisClient.DefaultTTLSeconds,不新增第二个 TTL 来源。
CacheRedis *CacheRedisConfig `json:"CacheRedis,optional"`

type CacheRedisConfig = PlacementRedisConfig // 同一形状:Hosts 必填,Password / DB 可选
```

- **svc**:新增 `CacheRedis *redis.Client`,没配置时等于 `RedisClient`。写法同 `svc/servicecontext.go:35-48`:连不上只记 ERROR、不拒启。这与 `RedisClient` 同口径:回写失败的写任务不推进游标,留在重试里(`key_ordered_consumer.go:836-843`)。
- **kafka 包**:
  - 构造函数改为 `NewKeyOrderedKafkaConsumer(cfg, redisClient, cacheRedis, placementRedis, stores)`(`:867`;调用点 `go/db/db.go:116-121`)。
    - `cacheRedis == nil` 与 `placementRedis == nil` 同口径返回错误(fail-closed),缺省值只在 svc 层回填,构造函数不替调用方猜。
    - 既有用例 `placement_route_test.go:891-895` 按四参数调用,要同步改成五参数,并补一条「cacheRedis 为 nil 时报错」。
  - consumer 结构(`:68`)和 worker 结构(`:108`)各加字段 `cacheClient redis.Cmdable`;consumer 在 `:918` 赋值,建 worker 时(`:962`)传下去。
  - **共享缓存回写和读结果发布的唯一入口是自由函数 `processTaskWithoutLock(ctx, redisClient, task, route)`(`:1984`)**,它把参数原样传给 `runDBOp`(`:2012`)。`runDBOp` 里没有 `w`,不能写成 `runDBOp(ctx, w.cacheClient, …)`。正确改法:
    - 把 `processTaskWithoutLock` 的第二个参数改名为 `cacheClient`(函数体只把它交给 `runDBOp`,不做别的事);
    - 三个调用点都改传缓存句柄:`processDBTask` 的离线扩分区分支(`:1817`)和正常分支(`:1840`)传 `w.cacheClient`,`tryLockAndProcess`(`:1981`)传 `worker.cacheClient`;
    - 同一函数里读写扩分区状态(`:1820`、`:1832`)和拿锁的句柄仍是 `w.redisClient` / `worker.locker`,不动。
  - `failReadTask` 里改为 `publishTaskResult(w.ctx, w.cacheClient, …)`(`placement_route.go:334-335`);它后面的 `ackRetryReceipt`(`:342`)属于控制面,仍走 `w.redisClient`。
  - 游标、epoch、排序锁、重试、扩分区状态仍走 `redisClient`。
  - 改完后在 go/db 里 grep `publishTaskResult(`、`publishDBOpResult(`、`writeBackDBCache(` 的所有调用点,确认每个都拿到缓存句柄;单测按三条路径各覆盖一次(§8)。
- **启动横幅**:`go/db/db.go:226-231` 补一项 cache 句柄。
- **(2026-10-18 更正,上面「go/db(第 2 批)」一段原文保留)** 这段改法以令牌发布脚本落码之前的代码为前提,已不能照做:
  - 写路径的缓存发布现在是 `publishDBWriteResult` → `playerCachePublishScript`,经 `lease.runTokenScript` 与排序锁令牌同一次 EVAL 执行(`player_cache_publish.go` :75-127)。缓存键与锁键必须在同一实例,不能单独换成 `cacheClient`。
  - 读路径缓存发布(`publishDBReadResult`,`key_ordered_consumer.go:523`)与读结果发布(`publishTaskResult`,:572)仍是单独的命令,可以换句柄。
  - 构造函数现在是 `NewKeyOrderedKafkaConsumer(cfg, redisClient, placementRedis, stores)`(:1125;调用点 `go/db/db.go` :195 一带),内部另按 `ServerConfig.OwnerEpochRedis` 建只读句柄(:1221-1225)。上文引用的 `processTaskWithoutLock`、`runDBOp` 等行号都已漂移(当前分别约 :2840、:2901),文件仍有多个会话在改。
  - 第 2 批要先完成 §18.7 的二选一,并把 go/db 的 Redis 句柄按承载的键重新划分(§18.7 表),再按新的句柄划分重写本段。

**merge_zone(第 2 批)**

- 新参数 `-dbctl-redis-addr` / `-dbctl-redis-password` / `-dbctl-redis-db`,缺省取 `-notice-redis-*` 的值(向后兼容)。
- 走新句柄的:
  - preflight 的重试 / 死信队列检查(`preflight.go:260-270`,改用 `d.dbctlRdb`);
  - 审计 `kafka_db_task_queues`(`audit_checks.go:153-165`;`audit_resources.go:228-251`);
  - relocate R2 等排序锁(`relocate_run.go:339`、`:478`)。
- 仍走 notice 句柄的:`player:session` 与组队检查(`preflight.go:296`、`:308`;`audit_checks.go:82-85`),合服提示(`unmerge.go:231`)。
- 第 3 批:`invalidatePlayerCaches`(`player_rows.go:718-745`)拆成两半:父键走 notice 句柄,子表走 `-cache-redis-*`。

**`k8s_deploy.ps1` 参数**

| 参数 | 类型 / 缺省 | 说明 |
|---|---|---|
| `-RedisSplit` | string[];ValidateSet `queue`、`mapping`、`dbctl`、`cache`;所有档位缺省 `@()` | 空 = 生成的 ConfigMap 与 infra 与今天逐字节相同 |
| `-RedisQueueMaxMemory` / `-RedisMappingMaxMemory` / `-RedisCacheMaxMemory` | string,形如 `512mb`;dev 档缺省 `128mb` / `256mb` / `256mb` | release 档(ReleaseProfile 不是 dev,或 OpsProfile 不是 custom)启用了对应用途却没给 → 拒绝部署 |
| `-RedisCoreMaxMemory` | string;缺省空 = 核心清单不变 | 给了就改核心 StatefulSet 的命令行,会重启 redis-0,必须同时给 `-ConfirmRedisCoreRestart`(**2026-10-15:这条「必须同时给」已由 P0-E 的重启闸按更宽语义覆盖,本参数本身仍未实现**) |
| `-ConfirmRedisSplitSwitch` | string[] | 线上已有 ConfigMap 的用途要改地址时必须列出,并通过 §4.4 的迁移标记检查 |
| `-ConfirmRedisCoreRestart` | switch | 见上。**2026-10-15 已由 P0-E 按更宽语义实现**(`tools/scripts/k8s_deploy.ps1` 的 `Assert-RedisCoreRestartConfirmed`):不论原因,本次 apply 会改变线上 `sts/redis` 的 Pod 模板(或线上 StatefulSet 还没有模板摘要注解)就必须给;`dev_tools.ps1` / `k8s_image.ps1` 已透传。本稿以后接 `-RedisCoreMaxMemory` 时直接复用这道闸,不另做一套。见 `deploy/k8s/README.md`「Redis 连接上限与总账」 |

**`-RedisSplit` 是每次调用都要带的参数,漏带 = 想回到共享拓扑**。部署闸(§4.4)会把这种「地址要变」拦下来,不会静默切回;但下面这些包装入口必须原样转发,否则会在半路被闸拒绝:
- `dev_tools.ps1` 的 `k8s-infra-up`、`k8s-zone-up`、`k8s-all-up`、`k8s-release-zone`、`k8s-release-all`;
- `k8s_zone_rollback.ps1`:Step 6 用固定的 `$upArgs` 调 zone-up(`:771-790`),今天没有 `-RedisSplit`。不补的话,拆分后的回档会在**已经停服、MySQL 已经回档之后**的 Step 6 被闸拒绝。它停服之前的静态预检是 `-DryRun`(`:843-848`),DryRun 不查集群,拦不住。所以回档脚本要:(1) 新增 `-RedisSplit` 并转发;(2) 在停服之前、只读地跑一次 `Assert-RedisTopologyChangeIsSafe`(真查集群),不一致就在停服前拒绝。

拓扑只在一个函数里算,写法仿照 `Get-KafkaTopologyFor`(`k8s_deploy.ps1:1049`):

```powershell
# 返回 @{ Core; Queue; Mapping; DbCtl; Cache },每项是 @{ Service; Host; Port; HostPort },
# 例如 @{ Service='redis-queue'; Host='redis-queue.<ns>'; Port=6379; HostPort='redis-queue.<ns>:6379' }。
# 分开给 Host 与 Port,是因为 gateway(host / port 两个键)和 C++ zoneredis(host / port)不吃 "host:port"。
# 规则:dbctl 必须同时启用 mapping,否则 throw "dbctl 必须与 mapping 同实例(§3.2.3)";
# DbCtl = Mapping;没拆的用途 = Core。
function Get-RedisTopologyFor { param([string[]]$Split, [string]$InfraNamespace) ... }
```

所有 ConfigMap 生成器只从这个函数取地址:
- login(`:3583-3590`):`RedisClient` = Core;Queue ≠ Core 时整段写 `QueueRedis`;Cache ≠ Core 时写 `CacheRedis`。
- go/db(`:3456-3465`):`RedisClient` = DbCtl;Mapping ≠ DbCtl 时显式写 `Placement.Redis`;Cache ≠ DbCtl 时写 `CacheRedis`。
- data-service:`MappingRedis` = Mapping;`DevRedis` / `Regions` = Core。
- gateway:`spring.data.redis` = Queue;注入的口令非空时才写 `password`(dev 档渲染逐字节不变)。
- auth、C++ 的 `zoneredis`:恒为 Core。
- **(2026-10-18 更正,上面拓扑规则与生成器取址原文保留)**
  - 拓扑函数注释里的「dbctl 必须同时启用 mapping,否则 throw」与「DbCtl = Mapping」**作废**。改为:按用途拆的拓扑函数按 zone 返回 DbCtl,取值是该 zone 所属组的 `redis-dbctl-g<N>`;组来自 zones 配置的新字段(§18.4)。不启用 dbctl 时 DbCtl = Core。(2026-10-19 补:原稿规划的函数名 `Get-RedisTopologyFor` / `Assert-RedisTopologyChangeIsSafe` 已被核心高可用形态占用,落码时换名,见 §4.3 的 2026-10-19 更正。)
  - go/db 生成器的「`RedisClient` = DbCtl;Mapping ≠ DbCtl 时显式写 `Placement.Redis`」改为:`Placement.Redis` 一律显式写 Mapping;控制面句柄 = DbCtl;父缓存 / 屏障所在的句柄恒为 Core(§18.7)。今天这三者都是 `RedisClient` 一个句柄,第 2 批要先拆句柄。

### 4.3 K8s 清单

- **新增模板 `deploy/k8s/manifests/infra/redis-purpose.yaml`**,结构照 `redis.yaml`:
  - 对象:Service `__REDIS_NAME__`、headless `__REDIS_NAME__-headless`、PDB `__REDIS_NAME__-pdb`(minAvailable 1)、StatefulSet `__REDIS_NAME__`(replicas 1)。
  - 标签与 selector 一律用 `app: __REDIS_NAME__`。核心 Service `redis` 的 selector 是 `app: redis`(`redis.yaml:22-30`),新 Pod 不能带这个标签,否则会被挂到核心 Service 后面。契约测试检查渲染结果里没有 `app: redis` 的新对象。
  - 镜像 `redis:7.2`,与 `redis.yaml:83` 一致;WAITAOF 要求 ≥ 7.2。
  - 命令:`--appendonly yes --appendfsync everysec --maxmemory __REDIS_MAXMEMORY__ --maxmemory-policy noeviction`;口令可选,做法同 `redis.yaml:90-104`。
  - **环境变量名必须叫 `REDIS_PASSWORD`**,来自 Secret `redis-auth`(optional: true)。`k8s_gate_drain.ps1:630-640` 和 §4.4 的标记检查都是 exec 进 Pod 后读这个变量。
  - 探针照搬 `redis.yaml:112-135`。
  - requests 与 limits 的内存都取 `__REDIS_MEM__` = 2 × maxmemory(AOF 重写时 fork,写时复制最坏会翻倍);CPU `__REDIS_CPU__`;PVC `__REDIS_STORAGE__`。
- **怎么 apply**:
  - 在 `Apply-Infra` 里 `redis.yaml` 之后,按 `Get-RedisTopologyFor` 启用的用途循环渲染(占位替换写法同 `:5835-5851`)。
  - 复用 `:5854-5859` 的「有未替换占位就拒绝」检查。
  - Secret `redis-auth` 已经在 `redis.yaml` 那个分支里先建好(`:5789-5812`)。
  - `-WaitReady` 时逐个 `Wait-ForStatefulSetReady`(同 `:5877-5879`)。
- **不删已有对象**:没启用的用途,脚本不会删掉已存在的 `redis-queue` 等对象。回退时数据还在里面,删不删由人决定。
- **核心 `redis.yaml`**:
  - 第 1c 批显式写 `--maxmemory-policy noeviction`、`--maxmemory` 和 limits。`:136-143` 的注释原本就写明要等压测数据再定。
  - 改 Pod 模板 = redis-0 重启 = 全服登录中断(`redis.yaml:17-20`)。
  - 所以命令行加占位 `__REDIS_CORE_ARGS__`,resources 段加占位 `__REDIS_CORE_LIMITS__`。不给 `-RedisCoreMaxMemory` 时两者都渲染成空串,渲染结果必须与今天的清单逐字节相同(契约测试用 golden 文件钉住)。给了却没给 `-ConfirmRedisCoreRestart` 就拒绝。
    - 占位必须**行内**放置:命令行里两处 `exec redis-server --appendonly yes`(有口令、无口令两个分支,`redis.yaml:93-97`)都要加,写成 `--appendonly yes__REDIS_CORE_ARGS__`,替换值以空格开头;`__REDIS_CORE_LIMITS__` 接在 `memory: 512Mi` 行尾,替换值以换行开头。单独占一行的占位替换成空串后会留下空行,golden 比对就不再逐字节相同。
  - 不重启的过渡做法(供 §13 第 4 条参考):Redis 缺省就是 noeviction(核心命令行没传 `--maxmemory-policy`),`maxmemory` 可以先用 `CONFIG SET` 在线设上,不动 Pod 模板。代价是重启后丢失,要等下一次计划内维护窗口再把清单改成显式值。
- **`redis-ha.yaml` 不动**:它的 apply 路径在脚本里不存在(`tidb-phase1-and-launch-readiness.md` §9.5)。
  **(2026-10-19 更正,上句原文保留)** 前提已变:核心实例已有高可用形态与 `-RedisReplicas` 的 apply 路径(见 §2.5 第 6 条的更正),staging / prod 档缺省就是高可用。
  本节的拆分做法不因此改变;落码时要核对两点:
  - 本稿 §4.2 / §4.4 规划的函数名 `Get-RedisTopologyFor`、`Assert-RedisTopologyChangeIsSafe` 已被高可用形态占用(语义是核心的副本数 1 / 3 与 1 → 3、3 → 1 的变更判定),按用途拆的那一套要换名;
  - 上面核心显式化的占位(`__REDIS_CORE_ARGS__` / `__REDIS_CORE_LIMITS__`)只写了 `redis.yaml`;核心取高可用形态时 apply 的是 `redis-ha.yaml`,两份清单怎么同步待定。
- **连接上限(2026-10-15 补,P0-E)**:每个拆出的实例(`redis-purpose.yaml` 的模板)都要像核心 `redis.yaml` 那样显式 `--maxclients` 并在启动时打印 fd 限值,按用途在 `k8s_deploy.ps1` 的「Redis 连接总账」里各自重算(每个服务对每个实例各开一个池,单实例连接数下降、进程总连接数上升;C++ 是每进程一条,不是 §2.1 写的「每个线程一条」)。核心 `redis.yaml` 已经显式 `MAXCLIENTS=40000`,§4.3「缺省渲染与今天逐字节相同」的基准要以 P0-E 之后的清单为准,`__REDIS_CORE_ARGS__` 接在 `--maxclients "$MAXCLIENTS"` 之后。
- **(2026-10-18 增补,上条原文保留)** `redis-dbctl-g<N>` 的每个组实例、`redis-cache`(若为 Cluster 则每个主节点)同样要显式 `--maxclients`,并在连接总账里按实例各算一行(§18.6、§19)。第 4 批核心上 Cluster 后,集群客户端会连每个主节点,C++ 的「每进程一条」变成「每进程每主节点一条」,总账要按单元重算(§20.6)。

### 4.4 部署脚本的安全闸

`Assert-RedisTopologyChangeIsSafe`,写法仿照 `Assert-KafkaTopologyChangeIsSafe`(`k8s_deploy.ps1:1130`)。DryRun 时不查集群。

为了能单测,读集群的部分以 scriptblock 注入(同 `k8s_zone_rollback.ps1` 的 `Assert-RedisFlushTargetIsZoneLocal -Probe` 写法):`-ReadLiveConfigMap`、`-ReadMarker`、`-ServiceExists`、`-ScanCount`。

1. 读线上 ConfigMap,求出各用途的现地址:
   - queue:login 的 `Node.QueueRedis.Host`,没有就取 `RedisClient`;gateway 的 `spring.data.redis.host`。两者不一致就拒绝(说明上次切换没做完);
   - mapping:data-service 的 `MappingRedis.Host`;
   - dbctl:db 的 `ServerConfig.RedisClient.Hosts`;
   - cache:db 的 `ServerConfig.CacheRedis.Hosts`,没有就取 `RedisClient`;login 的 `Node.CacheRedis.Host`,没有就取 `Node.RedisClient.Host`。两者不一致就拒绝。
   ConfigMap 不存在(新 zone、新集群)时不检查地址变化,但第 4 条照查。
2. 地址要变的用途,必须出现在 `-ConfirmRedisSplitSwitch` 里。漏带 `-RedisSplit` 的例行部署会在这一步被拦下,报错里写明「线上已拆分为 X,请带 -RedisSplit X」。
3. mapping 和 dbctl 还要求**目标实例**上已有迁移标记 `redis-split:migrated:<purpose>`(dbctl 按 zone:`…:dbctl:zone:<z>`),标记里的 `source` 等于现地址、`verified` 为 true。
   - 读法:`kubectl exec svc/<target> -- redis-cli GET`,口令取容器内的 `REDIS_PASSWORD`,做法同 `k8s_gate_drain.ps1:630-640`。
   - 没有标记就拒绝,提示先跑 `tools/redis_split`。
   - **标记必须比源上的同名标记新**:回退时源(比如核心)上也会被 `mark` 写一份。目标标记的 `verified_at_ms` 不大于源标记的,说明目标上是回退之前的旧数据,拒绝。否则「正向迁移 → 回退 → 再正向但忘了重新拷贝」会凭第一次留下的标记通过,把读方切到一份旧映射上。
   - cache 用途要求目标上有 `-skip-copy` 标记,且标记写入时目标上没有 `PlayerDatabase*:*`(§5.2)。
4. 每个启用的用途,目标 Service 必须存在(`kubectl get svc <name> -n <infra>`)。queue 没有迁移标记,不查这条的话 zone-up 会把 login 指向一个不存在的实例,login Ping 失败 panic、CrashLoop。
5. queue 还要求:
   - 核心上 `SCAN MATCH gate:*:draining` 结果为空,即没有正在进行的排空;
   - 线上所有 zone 的 login ConfigMap 里 `Queue.Enabled` 都不是 true(§3.2.1 约束 2)。
6. C++ 的 `zoneredis` 和 auth 永远等于 Core:生成器里写死取 Core,契约测试再钉一次。

DryRun 不调这个函数(同 Kafka 闸)。所以凡是「先 DryRun 预检、后真执行」的流程(典型是 `k8s_zone_rollback.ps1`),要在产生任何改动之前单独真跑一次本函数(只读),见 §4.2。

另新增 `dev_tools.ps1 -Command k8s-redis-split-status`:
- zone 列表用 `Resolve-ZonesConfigPath`(支持 `-ZonesConfigPath`、json 与 yaml,`k8s_deploy.ps1:5449-5451`、`:5916`),不写死 `zones.json`;
- 对每个 zone 打印各用途的现地址:ConfigMap 里的值,**以及**每个相关 Deployment(login、gateway、data-service、db)的 rollout 是否完成(带短超时的 `kubectl rollout status` 的结果,以及是否还有旧 ReplicaSet 的 Pod)。只看 ConfigMap 不够:ConfigMap 改了不等于旧 Pod 都退场了,旧 Pod 还在往旧实例写;
- queue、mapping、cache 在 zone 之间不一致,或任一 rollout 没完成,以退出码 1 结束;dbctl 按 zone 迁移(§5.5),各 zone 不一致只打印、不算失败。
- mapping、cache 的切换步骤和所有 purge 之前都以它为闸(§5.4、§5.6、§5.8)。
- **(2026-10-18 更正,本节原文保留)** dbctl 一项的「现地址」改为按 zone 读,目标是该 zone 所属组的 `redis-dbctl-g<N>`,迁移标记 `redis-split:migrated:dbctl:zone:<z>` 写在组实例上。部署闸另加一条:zone → 组的映射与线上 ConfigMap 不一致(例如 zones 配置改了组而该 zone 没走迁移)就拒绝。`k8s-redis-split-status` 同样按组打印。

### 4.5 契约测试

新用例都加在 `tools/scripts/tests/k8s_deploy_contract.tests.ps1`,清单见 §8。
既有的 `:839` 用例今天只检查缺省渲染(`$devOut`)。要让它「在所有拓扑下都通过」,把用例体抽成一个函数(入参是渲染输出),原用例照旧对 `$devOut` 调一次,新用例再对 `-RedisSplit queue,mapping`、`mapping,dbctl`、`mapping,dbctl,cache` 的渲染各调一次。断言本身一字不改。

### 4.6 运维脚本

- **`k8s_gate_drain.ps1`**:`Node.QueueRedis.Host` / `DB` 存在就用它,否则用 `Node.RedisClient.*`(改 `:312-330`、`:368`)。新句柄 DB 非法时与现状同口径,拒绝。
- **`k8s_zone_rollback.ps1`**:
  - `Get-SharedRedisGuardPatterns`(`:633-642`)补上核心族:`account:*`、`player_to_account:*`、`player:session:*`、`player:*:owner_epoch`。
    - 原因:第 1 批 purge 之后核心上不再有映射族,第 2 批 purge 之后也不再有游标,原有的守卫模式在核心上就不再命中。不补的话,对核心 FLUSHDB 会被放行,而那会清掉全服未落库的最新存档和账号目录。
    - **不加 `PlayerAllData:*`**:Step 4 的用途写明是「清 zone 的数据缓存、从 MySQL 重建」(`:5`、`:32`、`:922`),zone 私有缓存里有 `PlayerAllData:*` 是正常的,把它列进守卫会拦掉本来合法的目标。核心上一定有 `account:*`,所以不加它也拦得住核心。
    - 今天核心上已有映射族,守卫本来就拒绝,补上之后看不到行为变化;所以随第 1a 批一起补。
    - 参数注释(`:211-213`)和拒绝文案(`:695`)里列举的键类要同步补上「账号目录、会话、归属 epoch」。
    - (2026-10-18 增补,B0-1)`player:gate_index:*` 与 `player:session:*` 同族、必须同实例,守卫模式补核心族时一并加上。代码事实源仍是 `tools/scripts/k8s_zone_rollback.ps1` 的 `Get-SharedRedisGuardPatterns`,B0-1 没有改它。
    - **(2026-10-18 更正,上面几条原文保留)** `Get-SharedRedisGuardPatterns` 已不存在:R8 删掉了回档的 FLUSHDB,连同这组防护函数(§1 第 4 条的 2026-10-18 更正;`tests/k8s_zone_rollback_discard.tests.ps1` :89-95 断言它们不存在;`k8s_zone_rollback_flush_guard.tests.ps1` 也已不在)。所以本条「守卫补核心族」与上一行的 B0-1 增补都失效,第 1a 批不做。拆分后回档要清的是各用途 / 各组实例上 T 之后的状态,由尚未落地的 `go/db/cmd/zone_rollback_discard` 负责(契约在 `k8s_zone_rollback.ps1` :150 起),它的输入要包含 §18.4 的 zone → 组映射。
  - 新增 `-RedisSplit` 参数并转发给 Step 6 的 zone-up;停服之前真跑一次 `Assert-RedisTopologyChangeIsSafe`(§4.2、§4.4)。
- **`dev_tools.ps1`**:新增 `k8s-redis-split-status` 和 `k8s-redis-wipe`(§5.9);`-RedisSplit`、`-ConfirmRedisSplitSwitch`、各 `-Redis*MaxMemory`、`-ConfirmRedisCoreRestart` 原样转发给 §4.2 列出的部署入口和 `k8s-zone-rollback`。第 2 批再转发 merge_zone 的 `-dbctl-redis-*`。
- **`stress_snap.ps1`**:加 `-RedisTargets` 参数(§6.4)。

## 5. 迁移

### 5.1 原则

- **固定顺序**:停写 → 拷贝 → 校验并打迁移标记 → 切换读写方 → 补齐冻结期漏网的写(delta)→ 解冻 → 浸泡 → 清理旧实例上的键(purge)。
- **不做双写**:
  - 映射和控制面的写都是带复核的 Lua(围栏检查 + 写入、set-if-newer、CAS),拆成两个实例各写一次,就不再原子。
  - 一边成功一边失败时,没有任何仲裁者能判定哪份对。
- **不做双读**(先读新实例,读不到再读旧实例):
  - 对映射来说,这正是 D1 要堵的「缺键 ≠ 无记录」漏洞。旧实例上过期但看起来合理的值会被当成权威。
  - 对缓存来说可以接受,但只为一次性的切换写代码不值得(YAGNI),冷读就够了。

### 5.2 迁移工具 `tools/redis_split`(第 1b 批)

- **形态**:独立 Go module,与 merge_zone 同口径,只依赖 go-redis v9.7.3(`tools/merge_zone/go.mod`),不引入 miniredis。
  - 单测通过接口注入假实现。
  - 真 Redis 测试打 `integration` tag。
- **族表 `families.go` 是唯一出处**:

```go
type Policy int

const (
    PolicyCopy        Policy = iota // 拷贝,保留 PTTL
    PolicySkip                      // 不拷:缓存、心跳、会自动重建的键
    PolicyMustBeEmpty               // 迁移时必须不存在(锁);存在说明写者没停干净 → 拒绝
)

type Family struct {
    Purpose  string   // queue | mapping | dbctl | cache
    Name     string
    Patterns []string
    Policy   Policy
}
```

- **执行位置**:Service 地址只在集群内可解析。merge_zone 今天是在运维机上 `go run`、靠参数传地址(`dev_tools.ps1:945-1011`)。本工具同样可以经 `kubectl port-forward` 跑,但百万级键过 port-forward 的吞吐没测过,直接决定 §5.4 的冻结时长。所以:
  - 第 1b 批同时给出一个一次性 Job 清单(镜像同 go-svc 的构建方式),在 infra namespace 里跑,口令从 Secret `redis-auth` 注入环境变量;
  - kind 演练(§10)分别记录 port-forward 与 Job 两种方式的拷贝耗时,冻结窗口按实测定。
- **扫描方式**:一次 SCAN 遍历(COUNT 1000)、客户端按族分类,不按族各扫一遍。核心上的键绝大多数是 `PlayerAllData`、会话这类不相关的键,MATCH 是在服务端遍历之后才过滤的,按族各扫一遍等于把整个键空间扫 N 次。
- **各用途的族**:

  | 用途 | 拷贝 | 不拷 | 必须为空 |
  |---|---|---|---|
  | mapping | `player:zone:*`、`player:placement:*`、`player:maprev:*`、`merge:in_progress:*`、`merge:merged_into:*` | `db:capability:zone:*`(go/db 30s 内会重写)、`player:name:*`(缓存)、`mapping:route:ready`(见下) | 源与目标上的 `lock:player:*` |
  | dbctl | 按 `-topics` 给出的**完整 topic 名**逐个拼出精确前缀:`consumer:applied:<topic>:*`、`consumer:applied_epoch:<topic>:*`、`kafka:retry:queue:<topic>`,以及按 SCAN 列出的 `kafka:retry:queue:<topic>:p*`(2026-10-16 补,④ 重试只交给分区属主后按原始分区拆开的 ready 列表,键由 `go/db/internal/kafka/retry_routing.go::retryPartitionReadyKey` 拼成 `kafka:retry:queue:<topic>:p<N>`;topic 先做 glob 转义,同 `tools/merge_zone/preflight.go::dbRetryPartitionQueuePattern`;p* 不能按精确键拼,否则搬迁时这些列表里的存盘滞留在旧实例)、`kafka:retry:processing:<topic>` 与 `kafka:retry:processing:<topic>:*`、`kafka:retry:instances:<topic>`、`kafka:retry:instance_mode:<topic>`(2026-10-17 补:实例模式 HASH,与登记表同生共死,只拷登记表不拷它,`retry_requeue` 会把崩溃实例留下的登记看成「模式未知」,退出码 5 那道门就漏了)、`kafka:dead:queue:<topic>`、`kafka:expand:status:<topic>` | `kafka:retry:guard:*`(2026-10-17 补:认领 / ack 的防重记录,TTL 5 分钟,只防 go-redis 重发与迟到执行的旧副本;nonce 每次调用新取,本 zone go/db 缩到 0 之后无用,留在旧实例上自行过期。键以 `kafka:retry:guard:` 开头,上面的精确前缀本来也拼不到它) | `distributed:lock:kafka:ordering:<topic>:*`;全服的 `distributed:lock:kafka:consumer:lock:*`(不含 topic,§3.2.3) |
  | queue | — | `rl:*` | `gate:*:draining`、`queue:*`、`admit:*`、`admitted:*`(排队开着时要带 `-allow-drop-queue`) |
  | cache | — | 全部 | **目标上**的 `PlayerDatabase*:*`、`task:result:*`(§3.2.4) |

  - **dbctl 不接受通配的 topic 过滤**。`db_task_zone_1` 当子串或前缀用,会同时命中 `db_task_zone_10`…`_19`、`_100`… 和换代后的 `db_task_zone_1_g2`(`go/db/internal/config/config.go:299-310`)。拷错了还不算最糟:后面 purge 会把这些仍在核心上工作的 zone 的游标删掉,顺序守卫和 epoch 守卫随之失效。所以参数叫 `-topics`,值是逗号分隔的完整 topic 名,工具对每个名字校验 `^db_task_zone_\d+(_g\d+)?$`,并且只拼上表的精确前缀。一个 zone 有多代 topic 时要全部列出。
  - **`mapping:route:ready`(D1 第 1b 批之后才存在)**:不拷。切换读方之前,在**源**上 DEL 它,purge 也把它列进去。源不再是权威之后还挂着「副本完整可信」的标记,等于给回退埋雷:回退时源的 `run_id` 没变,对账 leader 会把这份缺了切换期间新写的旧副本当成可信(proposal `:260-272` 只在身份变化时撤标记)。
  - 迁移标记 `redis-split:*` 本身不属于任何族,verify 和 purge 都跳过它。

- **子命令**:
  - **`plan -src A -dst B -purpose P`**:只读。对每个族做 SCAN 计数;「必须为空」的族不为空就退出码 1;报告目标实例上已有的本用途键。
  - **`copy`**:
    - 前置条件是 `plan` 通过。目标上已有本用途的键就拒绝,除非带 `-resume`(此时只补缺)。
    - 逐批 SCAN(COUNT 1000),按类型读:`GET + PTTL` / `LRANGE` / `ZRANGE WITHSCORES` / `HGETALL` / `SMEMBERS`;用 pipeline 写:`SET PX` / `RPUSH` / `ZADD` / `HSET` / `SADD + PEXPIRE`。
    - 开始和结束各取一次源实例 `INFO server` 的 `run_id`。拷贝期间源实例重启过就以退出码 3 结束。
  - **`delta`**:只补「源有、目标没有」的键。两边都有但值不同时,记为冲突并以退出码 1 结束。目标已经是权威,绝不覆盖。用于收住冻结失效窗口里写进旧实例的建角(§5.4 第 6 步)。
  - **`verify`**:双向比较三种差异:源有目标无、目标有源无(只看本用途的族)、值不同。
    - 值按类型做逻辑比较,不比 DUMP 字节:同一个值在两边的内部编码可能不同。
    - PTTL 差超过容差(缺省 5s)只告警。
  - **`mark`**:`verify` 通过后,在目标上写 `redis-split:migrated:<purpose>[:zone:<z>]`,无 TTL,值为 JSON `{purpose, source, source_run_id, keys, verified, verified_at_ms, tool_version}`。cache 用途用 `-skip-copy` 写一个「已确认不拷」的标记,写之前复核目标上没有 `PlayerDatabase*:*`。部署闸只认这个标记,并要求它比源上的同名标记新(§4.4 第 3 条)。
    - `mark` 同时在**源**上写墓碑 `redis-split:moved:<purpose>[:zone:<z>]`,值为 `{to, at_ms}`,并删掉**目标**上的同名墓碑(目标从这一刻起是权威;回退之后再正向迁移时,目标上会留着上一次回退写下的墓碑)。用途见下面「误配防线」。
  - **`unmark`**:删除指定实例上的迁移标记与墓碑。回退完成后,对被放弃的那个实例执行(§5.8)。
  - **`restore -confirm-overwrite`**:回退专用,方向是「新实例 → 旧实例」。
    - 先 UNLINK 目标(旧实例)上本用途「拷贝」类的键(dbctl 按 `-topics` 精确过滤),再做一次 `copy`,然后双向 `verify`。
    - 为什么不能用 `delta`:旧实例上留着切换前的旧值。映射在浸泡期间可能被改写(合服工具解冻后照常运行),只补缺会把这些旧值原样留下;控制面更糟,核心上的旧游标与旧 epoch 比真实值小,go/db 切回之后,重试队列里已经落过库的任务会再落一次,顺序守卫与 epoch 守卫都被绕过,旧数据盖掉新数据。
    - 前置条件与 `copy` 相同:写者已停(冻结 / go/db 缩到 0),「必须为空」的族为空。
- **误配防线(墓碑)**:切换之后、purge 之前,旧实例上的映射与控制面仍在,而且「看起来正常」。运维给 merge_zone 漏传 `-mapping-redis-addr`(它缺省取 `-redis-addr`,`tools/merge_zone/main.go:256`),合服围栏、改映射就会写进没人读的核心:data_service 看不到围栏,合服中的 zone 继续建角,合服报成功却零改动。所以第 1b 批同时改三个读写映射的工具:merge_zone、`tools/data_consistency_check`、`route_tool`,连上映射句柄后先 `EXISTS redis-split:moved:mapping`,存在就拒绝并打印墓碑里的 `to`。第 2 批 merge_zone 新增的 dbctl 句柄(缺省等于 notice 句柄)同理,对本次涉及的每个 zone 检查 `redis-split:moved:dbctl:zone:<z>`;notice 句柄本身合法地指向核心,不做这项检查。
  - data_service 与 go/db 启动时对自己的映射句柄做同样的检查(命中即拒启),能再兜住「ConfigMap 被手工改回」这种绕过部署闸的情形。这要改两个服务的代码,第 1 批就不再是纯配置改动,所以列为 §13 第 15 条待拍板,不默认纳入。
  - **`purge -confirm-purge`**:
    - 只在源实例上删除本用途「拷贝」类的键。
    - 要求目标上的标记存在,且 `verified_at_ms` 早于 `-min-soak`(缺省 24h)。
    - 分批 UNLINK,只打印数量,不打印键名。
    - **这是永久删除,由运维执行。**
- **退出码**:0 成功;1 校验或前置不通过;2 参数错误;3 源在拷贝期间变化,需要重跑。与 `route_tool` 同口径。
- **口令**:只收环境变量名 `-src-password-env` / `-dst-password-env`,做法同 trade 的 `AssetOp.SecretEnv`(`k8s_deploy.ps1:4022-4024`)。口令不进命令行,也不进日志。

### 5.3 排队与限流

前提:L-02 还没打开(排队、限流都关着)。

1. 第 1a 批已部署(参数缺省为空)。
2. `infra-up -RedisSplit queue`:建出 `redis-queue`,不改任何服务。
3. `redis_split plan -purpose queue`:`gate:*:draining` 和 `queue:*` 都为空;所有 zone 的 login `Queue.Enabled` 为 false(部署闸也会查)。
4. 从这一步起到第 5 步结束,**禁止** `k8s_gate_drain.ps1` 和 gate 滚动(§3.2.1 约束 3)。
5. 所有 zone 执行 `zone-up -RedisSplit queue -ConfirmRedisSplitSwitch queue`:login ConfigMap 写入 `QueueRedis`,滚动重启;gateway 也是按 zone 部署的(§2.1),随同一次 zone-up 切到 `redis-queue`。之后跑 `k8s-redis-split-status`,确认各 zone 的 ConfigMap 一致、rollout 都已完成。
   - 逐 zone 执行期间,已切和未切的 zone 的 gateway 各用一个实例上的 `rl:ip:*` 桶,同一 IP 的限额暂时翻倍。限流关着(L-02 之前)时没有影响,这也是「先拆再开 L-02」的另一个理由。
6. 验证:
   - login 启动横幅出现 `queue: redis-queue…`;
   - `k8s_gate_drain.ps1` dry-run 读到的是 `redis-queue`;
   - 核心上 `SCAN MATCH queue:*` 不再增长。

不需要停服。滚动期间同一 zone 的新旧 login 各看一个实例的排空标记,但第 3 步已经保证两边都没有标记,第 4 步保证窗口里不会产生新标记。

### 5.4 映射

**路径 A**(今天就能做,`RouteStore.Mode=redis`):

1. `infra-up -RedisSplit queue,mapping`:建出 `redis-mapping`。
2. **冻结**:
   - 禁止 merge_zone 的一切写模式、backfill、relocate、pin。这与 D1 P1 的冻结窗口是同一份规矩(proposal §7)。
   - 用 killswitch 关掉 `DataService/RegisterPlayerZone`;登录侧表现为建角失败、可重试。
   - killswitch 是 fail-open 的(`go/shared/killswitch/killswitch.go:10-22`),**不能当唯一的闸**,所以后面有第 6 步。
3. `redis_split plan / copy / verify / mark -purpose mapping`:源是核心,目标是 `redis-mapping`。`mark` 同时在核心上留下墓碑 `redis-split:moved:mapping`(§5.2)。如果核心上有 `mapping:route:ready`(D1 第 1b 批之后),在这一步 DEL 掉。
4. 所有 zone 执行 `zone-up -RedisSplit queue,mapping -ConfirmRedisSplitSwitch mapping`。
   - data-service 的 `MappingRedis` 和 go/db 的 `Placement.Redis` 同一批改。
   - 部署闸会检查第 3 步写的标记。
   - 滚动期间新旧副本读的是两个内容相同的实例(冻结中),安全。
5. `k8s-redis-split-status`:所有 zone 的 data-service 和 db 的 ConfigMap 都指向 `redis-mapping`,**并且 rollout 全部完成、没有旧 ReplicaSet 的 Pod**。只看 ConfigMap 不够:没退场的旧 data-service Pod 仍会把漏网的建角写进核心,而它可能写在第 6 步之后。
6. `redis_split delta`:把冻结失效窗口里落到核心的建角补进 `redis-mapping`。出现冲突就停在这一步,由人处理。
7. 再跑一次 `verify`,然后 `mark`(刷新 `verified_at_ms`)。
8. **解冻**:删掉 killswitch 规则。然后观察:
   - 建角成功率和 data_service 的路由计数;
   - go/db 启动日志里的落点行(`go/db/db.go:226-231`);
   - 核心上 `player:zone:*` 不再增长。
9. 浸泡至少 24h 后,先再跑一次 `k8s-redis-split-status`,再 `purge` 核心上的映射族(运维执行,永久删除)。墓碑与迁移标记不删。
   - 这一步之后,任何仍然读核心的组件都会把所有玩家判成「无映射」。表现是大面积、看得见的失败,而不是静默写错库。
   - purge 之前的浸泡期里,误配到核心的 merge_zone / data_consistency_check / route_tool 由墓碑拦下(§5.2「误配防线」)。

停服范围:只有建角冻结,即第 2–8 步,推算 10–30 分钟;百万级键的拷贝在分钟级(取决于执行位置,§5.2,以 kind 实测为准)。已在线的玩家、登录和进场都不受影响,因为读映射的两份副本内容一致。

> **(2026-10-18 更正,上段原文保留)** 冻结窗口要按键数复算(容量模型 §4.2),映射三族约 3×N_reg 个键,按 5 万键/s 拷贝(推算 [I],拷贝速率未实测):
> - 开服前做:键很少,窗口取决于执行位置,上段的 10–30 分钟可作上限参考。
> - S1 口径(N_reg = 1 亿):约 3 亿个键,约 100 分钟。
> - 规模后(N_reg = 10 亿,DAU 1 亿按 10×DAU 推):约 30 亿个键,约 16.7 小时,不可行。
> 
> 所以路径 A 只适用于开服前或 S1 以内;规模后只能走路径 B(从 `player_route` 预热)。mapping 三族要不要借这个窗口改 hash tag,见 §21。

**路径 B**(D1 第 1b 批和第 3 批的读方开关上线之后):

- 不拷映射。data_service 直接指向空的 `redis-mapping`。
- 对账 leader 从 `player_route` 全量预热,然后挂就绪标记。预热期间 go/db 开着 `RequireRouteReady`,写任务延后,不消耗重试预算(proposal `:239`)。围栏和 merged_into 从库里补。
- 只需要第 2、4、5、8 步。
- **推荐 D1 到位后走路径 B;开服前 D1 没到位就走路径 A**(§13 第 11 条)。

### 5.5 存盘控制面(第 2 批,逐 zone 执行)

前提:
- 从第一个 zone 开始到最后一个 zone 切完,**全服禁止在线扩分区**(§3.2.3),也禁止运行 merge_zone(它的 `-dbctl-redis` 句柄只能指向一个实例)。
- **与 L-09 的关系**:K8s 上所有 zone 的 go/db 共用一个消费组 `db_rpc_consumer_group`(`tidb-phase1-and-launch-readiness.md` L-09,`k8s_deploy.ps1:3437`)。缩容和扩回各触发一次全组 rebalance,**全服**存盘各停几秒,Z 个 zone 一共 2Z 次。建议先做 L-09(消费组按 zone 命名)再做第 2 批;做不到就把每个 zone 的两次 rebalance 都排进维护窗口,并在 robot / 监控里确认每次之后各 zone 的 lag 都追平。
  - (2026-10-16 更新,③;上文不删)L-09 已落码,未编译、未测试,待 Codex 验证(设计见 [db-consumer-group-per-zone.md](db-consumer-group-per-zone.md))。各 zone 逐个从共享组 `db_rpc_consumer_group` 交接到本 zone 组 `db_rpc_consumer_group_z<ZoneId>`:`k8s_deploy.ps1 -Command zone-up -DbConsumerGroup zone` → `kafka_offset_reset.ps1 -FinalizeHandoff` → 以缺省 keep 再跑一次 zone-up(去掉 `GroupHandoffFrom`)。切组并收尾之后,该 zone 第 2 步的缩容与扩回只让本 zone 组再平衡,不再让全服存盘停顿。线上 db ConfigMap 仍是 `GroupID: "db_rpc_consumer_group"` 的 zone 照旧会触发全组 rebalance。所以「先做 L-09」现在的含义是:先把要做第 2 批的 zone 切组并收尾;交接没收尾的 zone,go/db 每个新进程入组前都要过交接闸并读 legacy 组,不要在这种 zone 上做第 2 批。

步骤:
1. 部署第 2 批代码。此时 `CacheRedis` 缺省等于 `RedisClient`,行为不变。
2. 把 zone Z 的 go/db 缩到 0。
   - 存盘由 Kafka 缓冲。
   - 父键缺失的玩家(新号首次进场必然如此)进场要冷读,go/db 停着就会撞上 8s 超时(`entergamelogic.go:380`),所以选低峰期,或者配合维护窗口。
3. `redis_split plan / copy / verify / mark -purpose dbctl -topics db_task_zone_Z[,db_task_zone_Z_g<N>…]`(完整 topic 名,§5.2)。
   - 本 zone 的排序锁、全服的扩分区锁必须为空。go/db 正常停机会释放排序锁(`:1945-1951` 用脱离取消的 ctx 释放);被强杀的话要等租约 2 分钟过期。
4. `zone-up -RedisSplit queue,mapping,dbctl -ConfirmRedisSplitSwitch dbctl`:go/db 的 `RedisClient` 指向 `redis-mapping`,`CacheRedis` 指向 `redis`。
5. go/db 起来后检查:
   - 横幅显示 cursor / lock 在 `redis-mapping`、cache 在 `redis`;
   - 消费恢复,本 zone lag 追平;
   - `redis-mapping` 上 `SCAN MATCH consumer:applied:db_task_zone_Z:*` 的数量在增长,核心上同一 pattern 的数量不再增长。**不要写成 `*db_task_zone_Z*`**:Z=1 时它还会命中 zone 10–19 和换代 topic。
6. 所有 zone 做完并浸泡后,先跑 `k8s-redis-split-status`,再逐 zone `purge -topics …` 核心上的控制面键。

> **(2026-10-18 更正,本节原文保留)**
> - **目标实例**:第 3~5 步的「`redis-mapping`」一律改为本 zone 所属组的 `redis-dbctl-g<N>`(§18);第 4 步不是把 `RedisClient` 指过去,而是把拆出来的控制面句柄指过去,父缓存 / 屏障句柄仍指核心(§18.7)。第 5 步的 SCAN 核对在组实例上做。§5.8 的 dbctl 回退同此。
> - **前提补三条**(都不满足就不开工):
>   1. §18.7 的围栏硬依赖已按拍板方案解决并落码验证(令牌发布脚本今天把排序锁、子表缓存、证据、`save_barrier` 绑在一段 Lua 里);
>   2. 条件写已落地:db-write-fencing-mysql.md 当前版本(v0.4 + 2026-10-17 实施附注)上线,该组全部 zone 已切 `Fencing.Mode=dual`,且全部有写生产者的 DBTask 目标表都是围栏表(§18.3);
>   3. 全部 zone 的 `home_zone_unknown` 与 class-2 / class-3 计数稳定为 0(§18.2,与围栏 §8.5 同口径)。
> - **与 L-09 的关系**:③ 消费组按 zone 的代码已入库(`go/db/internal/kafka/group_handoff.go`,随 `de4960bee` 入库;`k8s_deploy.ps1` 的 `-DbConsumerGroup keep|zone|legacy`;`go/db/etc/db.yaml` :21-24 的 `ConsumerGroup` / `GroupHandoffFrom`),但未验证。上面 2026-10-16 的 L-09 注照旧适用:各 zone 切到按 zone 的组并收尾之后,L-09 的全组 rebalance 问题才消失。负责人 2026-10-13 定 ③ 是开服门槛(PROGRESS「2026-10-14 存盘管线接手」§5 拍板第 15 条)。

### 5.6 子表缓存与读结果(第 3 批)

需要全服一拍完成:
1. 停入口:gate 缩到 0,或发维护公告。
2. 等 db_task 的 lag 和在途的 `task:result` 都清零。
3. `redis_split plan -purpose cache`:目标 `redis-cache` 上没有 `PlayerDatabase*:*` 与 `task:result:*`(§3.2.4;实例是新建的就天然满足,曾经用过、回退过的实例必须先清空)。然后 `mark -skip-copy`。
4. 所有 zone 的 go/db 和 login 同一批执行 `-RedisSplit …,cache -ConfirmRedisSplitSwitch cache`。缓存不拷。
5. `k8s-redis-split-status` 通过(含 rollout 完成)后开入口。

### 5.7 核心清单显式化(随第 1c 批的维护窗口)

- 执行 `-RedisCoreMaxMemory X -ConfirmRedisCoreRestart`,redis-0 会重启。
- 重启时重放 AOF;startupProbe 最多给 5 分钟(`redis.yaml:111-120`)。这段时间全服登录中断。
- 在线会话和位置随 AOF 保留,不会丢。这一点与 2026-09-19 从 emptyDir 切到 PVC 的那次不同。

### 5.8 回退

- **原则**:切换之后,新写只落在新实例上,旧实例上留着的是切换那一刻的旧值。**回退必须用新实例的数据整体覆盖旧实例,不能只改地址,也不能只补缺。**所以回退一律用 `redis_split restore -confirm-overwrite`(先清目标上的本用途键再拷,§5.2),不用 `delta`。purge 前后做法相同。
- **queue**:
  - 排队关着时:去掉 `queue` 重新部署即可,没有数据要搬。约束同 §5.3(禁排空与 gate 滚动,直到所有 zone rollout 完成)。
  - 排队已经打开(L-02 之后)时:逐 zone 回退会重演 §3.2.1 约束 2 的双 leader 与双队列;排位与在途准入随实例丢失。必须先把各 zone 的 `Queue.Enabled` 关掉、等 `admitted:*` 过期(AdmitTTL + 1min,默认 2 分钟),再回退。部署闸的 `Queue.Enabled` 检查对回退方向同样生效。
- **mapping**:
  1. 冻结(同 §5.4 第 2 步)。
  2. 如果 `redis-mapping` 上有 `mapping:route:ready`,先记下;核心上的同名键一律 DEL。
  3. `redis_split restore -purpose mapping`:源是 `redis-mapping`,目标是核心。
  4. 双向 `verify`,然后在核心上 `mark`(这一步会在 `redis-mapping` 上写墓碑)。
  5. 部署时去掉 `mapping`,带 `-ConfirmRedisSplitSwitch mapping`;部署闸会检查核心上的标记,并确认它比 `redis-mapping` 上的标记新。
  6. `k8s-redis-split-status` 通过(含 rollout 完成)后,对 `redis-mapping` 执行 `unmark`(删掉第一次正向迁移留下的标记,墓碑保留)。
  7. 解冻。
- **dbctl**:按 §5.5 逐 zone 反向执行,把第 3 步换成 `restore -purpose dbctl -topics …`。前提相同:该 zone 的 go/db 缩到 0、全服不扩分区、不跑 merge_zone。**禁止用 `delta`**:核心上的旧游标和旧 epoch 比真实值小,只补缺会让重试队列里已经落过库的任务再落一次,旧数据盖掉新数据(§5.2 `restore`)。
- **cache**:按 §5.6 反向执行,不拷;但切回之前必须在核心上 UNLINK 全部 `PlayerDatabase*:*`(或者等满一个 `DefaultTTLSeconds`,K8s 为 3600s)。核心上的这些键停在切换那一刻,比库旧,login 会用它们拼父键,等于回档(§3.2.4)。
- **时限**:没有硬时限,数据都还在 Redis 里。
- **再次正向迁移**:被放弃的实例上还留着旧数据。`copy` 遇到目标有本用途的键会拒绝,所以要先对它 `purge`(此时它是源侧),再按 §5.4 / §5.5 从头做;部署闸的「目标标记必须比源标记新」也会拦住漏做拷贝的情形。
- **代码回退**:第 1a、2、3 批的代码在 `-RedisSplit` 为空时与今天逐字相同(§1 第 4 条列出的两处有意改动除外)。回退代码之前,先把拓扑回退到共享。
- (2026-10-18 更正,上文原文保留)dbctl 回退的目标改为组实例(§18);cache 一条「login 会用它们拼父键」自 10-17 起不成立(父键缺失一律冷读 SQL,§2.2 E 表后的更正),旧缓存只影响工具读者。

### 5.9 与 AGENTS §6.2 压测清库纪律的关系

- §6.2 第 2 步「redis-cli FLUSHALL」和第 6 步「每次登录压测前把 redis 数据全部删除」,在拆分之后指的是**拓扑里的每一个实例**,再加 `redis-match-cluster`。
- **漏掉 `redis-mapping` 的后果最严重**:
  - 如果清了 broker 数据目录、topic 重建,offset 会从 0 重新开始。旧的 `consumer:applied:*` 游标比新 offset 大,go/db 就会把新写判成「已落库」直接跳过(`key_ordered_consumer.go:482-518`)。压测结果里会出现假的「存盘丢失」。
  - 旧映射还会把新号路由到错误的库。
- **本机**:docker-compose 不拆,`stress_round19.ps1:171-191`、`currency_crash_window.ps1:175-186` 都不用改。
- **K8s 压测环境**(`tidb-phase1-and-launch-readiness.md` §3.5 说的专用环境):新增 `dev_tools.ps1 -Command k8s-redis-wipe -ConfirmWipeAllRedis`。
  - 按 `Get-RedisTopologyFor` 列出的实例逐个 FLUSHALL,然后逐个检查 `DBSIZE`,不为 0 就以退出码 1 结束。拓扑以线上 ConfigMap 读出的现地址为准(同 `k8s-redis-split-status`),不以本次命令行参数为准:参数漏写一个用途,就会漏清那个实例。
  - `redis-match-cluster` 是集群,要对每个主节点各发一次 FLUSHALL,再逐个核对。
  - 白名单不能只靠本次命令行参数(调用者想清哪个就能写哪个)。infra namespace 必须符合固定的压测命名规则(例如 `^mmorpg-stress-`),规则写死在脚本里,不符合就拒绝。
- **压测快照**:`stress_snap.ps1` 增加各实例的 `INFO` 快照,`stress_summarize.ps1` 增加一张「Redis 实例」表(§6.4)。这会改 §6.1 规定的输出口径,需要负责人点头(§13 第 9 条)。
- **AGENTS.md 本身**由负责人改,本稿不动。建议改法:把「redis-cli FLUSHALL」改成「对部署拓扑里的每个 Redis 实例 FLUSHALL,并核对 DBSIZE=0(K8s 用 dev_tools k8s-redis-wipe)」。
- **对拆分本身的要求**:拆分有没有效果,必须用 L-14 尖峰压测的对比表来说明,拆分前一轮要先存 prev-summary。没有对比表,不能宣称「Redis 不再是瓶颈」。
- **(2026-10-18 更正,本节原文保留)**
  - 「漏掉 `redis-mapping` 的后果最严重」一条里的游标后果,在 §18 之后落在 **各 `redis-dbctl-g<N>` 组实例**上:漏清任一组实例,该组各 zone 的旧 `consumer:applied:*` 游标都会把新写判成已落库。`k8s-redis-wipe` 要遍历拓扑里的每个组实例;`redis-cache` 若是 Cluster,要对每个主节点 FLUSHALL 再逐个核对 DBSIZE(做法同 `redis-match-cluster`)。第 4 批核心上 Cluster 后,核心的每个主节点同样要逐个清、逐个核对。
  - 清库时还要清掉 2026-10-16~17 新增的核心键:`save_barrier`、补投桶、`db:save_protocol:*`、`asset_sql_commit`、`player:gate_index:*`。整库 FLUSHALL 会一并清掉。`tools/scripts/deploy-staging.sh`(:126 起)与 `stress-linux-tier.sh`(:57 起)是按前缀的部分清理:只清会话 / 登录类残留(B0-1 已补 `player:gate_index:*`),不清父键、`owner_epoch` 与屏障,这本身前后一致;将来若改成按前缀全清,要把上面几类一并列入,只清一半会让冷登录被屏障拒绝。
  - 只清 Redis、不清 MySQL 且会复用 player_id 时,要先按 db-write-fencing-mysql.md §9.1 抬计数器(负责人 10-13 定「条件写落码时再改 AGENTS §6.2」)。

## 6. 容量、maxmemory 与淘汰策略

本节数字**全部是推算**,没有压测。

### 6.1 负载模型

变量:
- N:在线人数;
- C:每秒建角数;E:每秒进场数;R:每秒读任务数;
- Q:排队人数;P:客户端轮询间隔(秒);
- L:login 实例数,每 zone 2 个副本,即 L = 2Z;
- W:每秒写任务数,约 2N/300。每次存盘最多涉及 2 张子表,存盘周期 300s(`cpp/libs/services/scene/core/system/redis.cpp:121`)。

| 实例 | 主要命令来源 | 推算 ops/s |
|---|---|---|
| redis-queue | 排队 Lookup 每次 ≥4–5 条;放行循环每秒每 zone 一次 ZPOPMIN,每放行一人 3 条;快速通道 1 次 EVAL;限流每个请求 2 个桶 | ≈ 5Q/P + 3 × 放行数/s + 2 × 请求数/s |
| redis-mapping(第 1 批) | go/db 每个任务 2 次 MGET;每次进场 1 次 EVAL;每次角色列表 1 次 MGET;每次建角 2–3 条(db_first 模式再加 WAITAOF);心跳 | ≈ 2W + E + 列表数/s + 3C |
| redis-mapping(第 2 批后) | 再加每个写任务约 7 条控制面命令:锁 SETNX、游标 GET / SET、epoch GET / EVAL、解锁 EVAL | ≈ 9W + E + … |
| redis-cache | 每个任务 1 次大 value 的 SET;每个读任务 LPUSH / EXPIRE / PUBLISH 各一次;PUBLISH 扇出到 L 个 login | ≈ W + 3R,另有推送 R × L 条 |
| redis(核心) | C++ 存盘每玩家每 300s 一次 EVALSHA;进场链路(锁、父键、会话、位置 / epoch 的 Lua、场景计数);离场;战斗与组队;会话续租 | 待压测 |

> **(2026-10-18 更正,上表 redis-queue 一行;原文保留)** 排队 Lookup 改为 3 次往返(其中 1 次是脚本),放行改为每人 1 次 EVALSHA,不再 ZPOPMIN;稳态续期写约 100 PEXPIRE/s(4.5 万人在排、2s 轮询、`QueueEntryTTL`=1h 的估算,未实测)。见 `login-queue-2026-05.md` §5 的 2026-10-18 更正。

> **(2026-10-22 更正,上表 redis-queue 一行的「快速通道 1 次 EVAL」;原文保留)** 快速通道现在是三条命令:`FreeSlots` 里 `AdmittedCount` 一次 EVAL(`admittedCountScript`)、`QueueLen` 一次 ZCARD、占位一次 EVAL(`reserveFastPathSlotScript`),见 `go/login/internal/logic/loginpregate/assigngatelogic.go` 的 AssignGate 与 `loginqueue/capacity.go`。这是 `spec_G4b-G.md:1523` 推给本稿的另一半(另一半「读 SCARD」见 §3.2.1 的 2026-10-19 更正)。

例:N = 10 万时 W ≈ 667/s。第 1 批 `redis-mapping` 约 1.3k 次 MGET/s;第 2 批后约 6k ops/s。单实例 Redis 处理简单命令的量级是每秒几万到十万,稳态余量很大。**首日真正的风险是尖峰**,也就是 E、C、Q 三项,而不是稳态。

> **(2026-10-18 更正,上表与上例原文保留)** 上例是全服 10 万在线的口径,2000 万下按 §16 重算(全部推算 [I],未实测):
> - 「redis-mapping(第 2 批后)≈ 9W + E + …」一行作废:控制面不并入 mapping(§18)。mapping 读约 2·DBTask + 2E + 3C_n ≈ 20 万~47 万 ops/s;控制面在各 `redis-dbctl-g<N>` 上合计 6·DBTask 约 53 万~133 万 ops/s(含令牌发布脚本 7·DBTask 约 62 万~155 万),每 zone 约 0.27 万~0.35 万(区间按 §16.1 配对)。
> - 写任务的 Redis 命令数以 go/db 当前的步骤标签为准(`go/db/internal/metrics/pipeline.go` :110-121):Kafka 来源、owner_epoch>0 的写约 9 次往返——取锁、游标 GET、epoch GET、选库 MGET、扩分区状态 GET、复核 MGET、令牌发布脚本、推进脚本、释锁。「约 7 条控制面命令」改为 6 条控制面命令加 1 次令牌发布脚本。
> - 「首日真正的风险是尖峰」仍成立,但稳态本身也已超出单实例:核心约 119 万~127 万 ops/s,排队轮询约 300 万往返/s(Q = 0.1×PCU = 200 万),见 §16。

### 6.2 内存模型

| 实例 | 内存主项 | 增长口径 | 推算 |
|---|---|---|---|
| redis-queue | 每个排队玩家:ZSET 成员 + meta HASH + token 键,约 0.5–1KB;每个 IP 桶约 0.2KB | 与在途排队人数、活跃 IP 数成正比,带 TTL | 10 万人排队约 100MB |
| redis-mapping | 每个玩家的 `player:zone`(加上 placement、maprev)约 0.1–0.3KB;第 2 批起,每个存过盘的玩家再加「表数(2)× 2 个游标键」约 0.5KB | **随累计注册 / 存过盘的玩家数增长,无 TTL,不会回落** | 1000 万玩家约 3–8GB |
| redis-cache | 最近一个 TTL 内(K8s 为 1h)写过的玩家 × 子表大小 | 与近 1h 活跃玩家数成正比 | 待压测 |
| redis(核心) | `PlayerAllData` 父键(2026-06 实测每玩家约 21KB / 17 个键,`docs/notes/redis-oom-data-safety-qa-record.md:21`)+ `owner_epoch`(永不删)+ 账号 + `dirty_keys_set` 成员(2026-10-16:写入已在代码中删除(已落码,未编译、未测试;全部 scene 换成新镜像前生产仍在写),存量待按 release-checklist §6.1 清理) | **随累计进过场景的玩家数增长**(§2.5 第 1、2 条) | 累计 100 万玩家约 20GB |

**核心内存没有上界,这是比拆分更急的容量问题**:
- 核心一旦设了 maxmemory + noeviction,满了就拒写,全服存盘和登录都会失败。
- 不设 maxmemory,就是今天的状态:最终被 OOMKill。
- 要么先有冷玩家卸载,要么按累计玩家规模配内存(§13 第 5 条)。

> **(2026-10-18 更正,上表与上段原文保留)** 按 2000 万口径重算见 §16(全部推算 [I]):
> - redis-mapping 行:「1000 万玩家约 3–8GB」改为按 N_reg = 10 亿(10×DAU)算,约 100~300 GB;「第 2 批起再加游标」作废(游标在 dbctl 组实例上)。
> - redis-cache 行:约 0.6~1.2 TB(3·PCU·V)。
> - 核心行:常驻至少 1~2 TB(P_cum ≥ 1 亿),其中在线热集约 200~420 GB;另加 `save_barrier`、`asset_sql_commit`、gate 索引、账号目录(205~360 GB,未落库前)与补投意图(正常约 11~83 GB;go/db 停摆 1 h 约 0.48~1.5 TB,公式见 §16.2)。
> - 「核心内存没有上界」改为 S1 前必须拍板,材料见 §23。

### 6.3 maxmemory 与淘汰策略

**所有实例都设 `maxmemory-policy noeviction`,并且显式写出,不依赖 Redis 的缺省值。** 各族被淘汰的后果:

| 族 | 被淘汰的后果 |
|---|---|
| 映射 | 缺键被当成「无记录」,写进错误的库(`docker-compose.yml:228-231`;proposal §3.3) |
| 游标 / applied_epoch | 顺序守卫静默失效(同上) |
| 排序锁 / 重试收据 | 并发写;收据丢失,存盘随之丢失 |
| `PlayerAllData` 父键 | 写回式的最新态丢失,等于回档 |
| 账号目录 | 角色从账号里消失,没有库可以回源 |
| 锁 / 会话 / epoch | 出现双持有者;顶号判断出错 |
| 排队 / 准入 | 玩家丢失排位;gate 令牌超发 |
| 限流桶 | 限流静默失效(friend 的注释 `k8s_deploy.ps1:4062-4064` 说的是同一件事) |
| 子表缓存 | 安全,只是多一次冷读 |
| `task:result` | 读等待 8s 超时,进场失败 |

只有子表缓存能容忍淘汰,但它和 `task:result` 放在同一个 `redis-cache` 里,所以这个实例也用 noeviction。

- **取值**:`maxmemory` = 压测峰值 × 1.5;`limits.memory` = 2 × maxmemory。release 档不给值就拒绝部署;dev 档用很小的缺省值。D1 的 noeviction 自检需要 `CONFIG GET` 可用,所以不能禁用这个命令。
  - `redis-queue` 的峰值要按最坏情况算 `rl:ip:*`:每个出现过的 IP 一个桶,TTL 是重填时间加 1h(`RateLimitConfig.java:71-91`)。trusted-proxies 配错时 IP 可被 X-Forwarded-For 伪造(`ClientIpResolver.java:20`),桶数没有上界。它满了之后,排队开着时新登录全部失败(§7),所以这个实例的内存告警要比别的实例更早。
- **持久化**:全部用 AOF everysec;核心维持现状(`redis.yaml:95-97`)。`redis-mapping` 上的 W1 用 `WAITAOF 1 0`(D1)。要不要改成 `appendfsync always`,按 D1 §11.1 第 5 条,压测后再定。

### 6.4 监控

- **现状**:没有 redis_exporter(`deploy/`、`tools/scripts/` 里 grep 不到)。
- **第 1a 批先不引入新镜像**:
  - `stress_snap.ps1` 增加 `-RedisTargets` 参数。每个快照时刻,对每个实例执行 `kubectl exec … redis-cli INFO stats|memory|cpu|commandstats|persistence`,存成 `t<N>m_redis_<name>.txt`。
  - `stress_summarize.ps1` 出一张表,包含:ops/s、`used_memory`、`used_memory` 占 maxmemory 的比例、OOM 拒绝次数(`errorstat_OOM`)、`rejected_connections`、`aof_delayed_fsync`、各类命令的 calls / usec_per_call。
- **长期**:每个实例挂 redis_exporter sidecar,并配告警:内存超过 maxmemory 的 80%、出现 `errorstat_OOM`、AOF 写错误(§13 第 6 条)。
- **指标 label** 只用实例名(低基数),不用 player_id(AGENTS §9)。

## 7. 错误语义

- **部署期拒绝**:非 0 退出,并给出中文原因。拒绝的情形有:
  - dbctl 没带 mapping;
  - release 档没给 maxmemory;
  - 线上改地址时没给 Confirm(包括例行部署漏带 `-RedisSplit`),或目标实例上没有迁移标记,或目标标记不比源标记新;
  - 启用的用途对应的 Service 不存在;
  - 同一用途在一个 zone 内的两份 ConfigMap 不一致(login 与 gateway、login 与 db 的 cache 句柄);
  - 改核心模板时没给 `-ConfirmRedisCoreRestart`;
  - 切 queue 时有正在进行的排空,或任一 zone 的排队开着。
  - **(2026-10-18 更正,上面清单原文保留)** 第一项「dbctl 没带 mapping」**作废,落码时不要实现这道拒绝闸**:按 §18,dbctl 是按 zone 组分的独立实例,与 mapping 无关。替换为:启用 dbctl 的 zone 在 zones 配置里没有组字段、组字段与线上 ConfigMap 不一致,或 §18.7 的硬前置没有满足(以部署参数显式确认),都拒绝。
- **工具期拒绝**:merge_zone、data_consistency_check、route_tool 连到带墓碑的实例(§5.2「误配防线」)。
- **login 启动**:QueueRedis / CacheRedis Ping 失败就 panic,与 RedisClient 同口径(`servicecontext.go:118-120`)。
- **login 运行期**:
  - 排队句柄出错:AssignGate 走既有的错误返回,客户端重试;
  - 排空查询失败:放行(`gatedrain.go:60-81`,不变)。
- **go/db**:
  - CacheRedis 连不上:只记 ERROR,不拒启,与 `svc/servicecontext.go:31-47` 同口径。
  - 写任务的缓存回写失败:任务重试,游标不推进(`key_ordered_consumer.go:836-843`,不变)。
  - `redis-mapping` 上的控制面不可用:行为与今天核心不可用时相同,拿不到锁、读不到游标就重试。
- **各实例满了(noeviction 拒写)时**:
  - `redis-mapping` 拒写:W1 建角失败,返回 Unavailable;login 按 fail-closed 处理并释放名字(`createplayerlogic.go:257-275`)。go/db 游标写失败 → 重试耗尽预算 → 进死信。这会放大 L-04 的问题(§12 第 8 条)。
  - `redis-queue` 拒写或不可用:排队开着时 AssignGate 对新登录一律返回 `StatusError`(fail-closed,§3.2.1);排队关着时不受影响。限流 fail-open,排空过滤放行。
  - 核心拒写:C++ 存盘失败并无限重试(`player_lifecycle.cpp:5401` 的注释),登录失败。与今天不设 maxmemory 时会被 OOMKill 相比,变成了「拒写,但进程活着」。
- **`redis_split`**:退出码 0 / 1 / 2 / 3,见 §5.2。

## 8. 测试清单

| 用例 | 位置 | 外部依赖 | 批 |
|---|---|---|---|
| `TestNodeConfig_QueueRedisAbsentReusesRedisClient` | `go/login/internal/config/queue_redis_conf_test.go`(新) | 无 | 1a |
| `TestNodeConfig_QueueRedisFullBlockLoads` | 同上 | 无 | 1a |
| `TestNodeConfig_QueueRedisPartialBlockRejected`(钉住 go-zero 的语义:段出现时内层字段必填) | 同上 | 无 | 1a |
| `TestRepoLoginYaml_HasNoQueueRedis`(本地 yaml 保持共享) | 同上 | 无 | 1a |
| `TestNewQueueRedis_NilConfigSharesPointer` | `go/login/internal/svc/queue_redis_test.go`(新) | 无(miniredis,login 已依赖) | 1a |
| `TestInitLoginQueue_WritesToQueueRedisOnly`(两个 miniredis:Enqueue 只落在 queue 实例) | 同上 | 无 | 1a |
| `TestCapacityProvider_DrainingReadFromQueueRedis` / `TestGateDrainMonitor_UsesQueueRedis` | 同上 | 无 | 1a |
| 契约:缺省拓扑下所有 ConfigMap 的 Redis 地址都是 `redis.<ns>`,且不渲染 `redis-purpose` | `k8s_deploy_contract.tests.ps1` | 无 | 1a |
| 契约:缺省拓扑的渲染结果与改造前逐字节相同(golden) | 同上 | 无 | 1a |
| 契约:`-RedisSplit queue,mapping` 时,login QueueRedis、gateway、data-service MappingRedis、db Placement.Redis 都指向对应实例;db RedisClient 仍是核心 | 同上 | 无 | 1a |
| 契约:任何拓扑下 C++ `zoneredis` 都是 `redis.<ns>` | 同上 | 无 | 1a |
| 契约:任何拓扑下 scene-manager / player-locator / match / chat / trade / friend 的共享段都是核心;data-service 的 DevRedis / Regions 也是核心 | 同上 | 无 | 1a |
| 契约:dbctl 不带 mapping 时拒绝;release 档启用用途却不给 maxmemory 时拒绝 | 同上 | 无 | 1a |
| 契约:`redis-purpose` 渲染结果含 noeviction、appendonly、maxmemory、limits = 2 × maxmemory、`REDIS_PASSWORD` 环境变量,且没有残留占位 | 同上 | 无 | 1a |
| 契约:release 档的 gateway 写了 `spring.data.redis.password`;dev 档(口令为空)不写,渲染逐字节不变 | 同上 | 无 | 1a |
| 契约:`redis-purpose` 渲染出的对象标签都是 `app: redis-<purpose>`,没有 `app: redis` | 同上 | 无 | 1a |
| 契约:任何拓扑下 auth 的 `spring.data.redis.host` 是 `redis.<ns>` | 同上 | 无 | 1a |
| 契约:`:839` 的断言抽成函数,对 `queue,mapping`、`mapping,dbctl`、`mapping,dbctl,cache` 三种渲染各调一次 | 同上 | 无 | 1a / 2 |
| `Assert-RedisTopologyChangeIsSafe`(注入假的 `-ReadLiveConfigMap` / `-ReadMarker` / `-ServiceExists` / `-ScanCount`):地址要变但没给 Confirm → 拒;漏带 `-RedisSplit`(线上已拆、本次为空)→ 拒且报错写明现拓扑;mapping 目标无标记 → 拒;目标标记不比源标记新 → 拒;目标 Service 不存在 → 拒;queue 切换时有 draining 或某 zone `Queue.Enabled=true` → 拒;login 与 gateway 的 queue 地址不一致 → 拒;一切齐全 → 放行;ConfigMap 不存在 → 不查地址变化 | 同上 | 无 | 1a |
| `k8s_zone_rollback.ps1`:给了 `-RedisSplit` 时 Step 6 的 zone-up 参数里带上它;拓扑闸在停服之前执行,不一致时在 Step 1 之前拒绝 | `k8s_zone_rollback_gate_router_mode.tests.ps1` 同款写法(新文件 `k8s_zone_rollback_redis_split.tests.ps1`) | 无 | 1a |
| 守卫新增的四个核心 pattern 各自命中都拒绝;只有 `PlayerAllData:*` 的 zone 缓存放行 | `k8s_zone_rollback_flush_guard.tests.ps1` | 无 | 1a |
| 契约:不给 `-RedisCoreMaxMemory` 时 `redis.yaml` 渲染逐字节不变;给了却没给 Confirm 时拒绝 | 同上 | 无 | 1a |
| 契约:既有的 `:839` 用例在所有拓扑下都通过 | 同上 | 无 | 1a / 2 |
| `QueueRedis 段存在时 exec 目标是 redis-queue` / `缺省回落 Node.RedisClient` / `QueueRedis.DB 非法时拒绝` | `k8s_gate_drain.tests.ps1` | 无 | 1a |
| `核心族命中时拒绝 FLUSHDB` / `只有排队族的实例放行` | `k8s_zone_rollback_flush_guard.tests.ps1` | 无 | 1a |
| `k8s-redis-split-status`:queue / mapping / cache 在各 zone 地址不一致时退出码为 1;任一 rollout 未完成时退出码为 1;dbctl 各 zone 不一致只打印、退出码 0;zone 列表走 `Resolve-ZonesConfigPath`(yaml 也能读)。`k8s-redis-wipe` 没有确认开关或 namespace 不符合压测命名规则时拒绝 | `tools/scripts/tests/dev_tools_redis_split.tests.ps1`(新) | 无 | 1a |
| `dev_tools.ps1` 把 `-RedisSplit` 等参数原样转给 `k8s-zone-up` / `k8s-all-up` / `k8s-zone-rollback`(DryRun 输出里能看到) | 同上 | 无 | 1a |
| `TestFamilies_NoOverlapAcrossPurposes`、`TestFamilies_PatternsCoverRollbackGuard`、`TestFamilies_ConsumerLockHasLockerPrefix`(必须为空的扩分区锁 pattern 是 `distributed:lock:kafka:consumer:lock:*`。redis_split 是独立 module,引用不了 go/db 的 internal 包,所以照 `tools/merge_zone/relocate.go:367-372` 的做法镜像前缀常量,注释写明改一处必须同步另一处) | `tools/redis_split/families_test.go` | 无 | 1b |
| `TestTopics_ExactMatchOnly`:`-topics db_task_zone_1` 不命中 `db_task_zone_10`、`db_task_zone_1_g2` 的任何键;非法 topic 名(含 `*`、空串、不匹配正则)退出码 2 | `families_test.go` | 无 | 1b |
| `TestPlan_MustBeEmptyFamilyNonEmptyFails`、`TestPlan_CacheTargetMustBeEmpty`、`TestCopy_RefusesExistingTargetKeys`、`TestCopy_PreservesTTLAndTypes`、`TestCopy_SourceRunIDChangedExits3`、`TestScan_SinglePassClassifiesAllFamilies` | `copy_test.go` | 无(接口 + 假实现) | 1b |
| `TestDelta_FillsMissingOnly`、`TestDelta_ConflictStopsWithoutOverwrite` | `delta_test.go` | 无 | 1b |
| `TestRestore_OverwritesStaleTargetValues`(目标上有更小的游标,restore 后等于源)、`TestRestore_RemovesTargetOnlyKeys`、`TestRestore_RequiresConfirmOverwrite`、`TestRestore_TopicFilterLeavesOtherZonesUntouched` | `restore_test.go` | 无 | 1b |
| `TestVerify_ValueMismatch`、`TestVerify_ExtraKeyInTarget`、`TestMark_RequiresVerifyPass`、`TestMark_WritesTombstoneOnSourceAndClearsTargetTombstone`、`TestUnmark_RemovesMarker`、`TestPurge_RequiresMarkerSoakAndConfirm`、`TestPurge_IncludesRouteReadyMarker`、`TestPasswordOnlyFromEnv` | `verify_test.go`、`marker_test.go` | 无 | 1b |
| `TestMappingHandleRefusesTombstone` | `tools/merge_zone`、`tools/data_consistency_check`、`go/data_service/cmd/route_tool`(各一条,假实现) | 无 | 1b |
| `TestIT_CopyVerifyMarkRoundTrip`、`TestIT_DeltaAfterLateWrite`、`TestIT_RestoreRoundTrip`、`TestIT_PurgeDeletesOnlyPurposeFamilies` | `integration_test.go`(`integration` tag;用 DB 14 / 15 当源和目标,只 FLUSHDB 这两个库) | **需要本机 Redis** | 1b |
| `TestCacheRedisNilSharesRedisClient`、`TestConfig_CacheRedisOptional` | `go/db/internal/svc`、`internal/config` | 无 | 2 |
| `TestProcessDBTask_NormalPathPublishesToCacheRedis`、`TestProcessDBTask_OfflineExpandPublishesToCacheRedis`、`TestTryLockAndProcess_PublishesToCacheRedis`、`TestFailReadTask_PublishesToCacheRedis`、`TestCursorEpochLockStayOnRedisClient`、`TestNewKeyOrderedKafkaConsumer_WiresThreeHandles`、`TestNewKeyOrderedKafkaConsumer_NilCacheRedisRejected` | `go/db/internal/kafka/cache_redis_test.go`(新,两个 miniredis);`placement_route_test.go:891-895` 同步改成五参数 | 无 | 2 |
| `TestDbCtlRedisDefaultsToNotice`、`TestPreflight_RetryQueuesReadFromDbCtl`、`TestRelocate_OrderingLocksFromDbCtl`、`TestDbCtlHandleRefusesTombstone` | `tools/merge_zone`(假实现) | 无 | 2 |
| 契约:`-RedisSplit mapping,dbctl` 时 db 的 RedisClient = redis-mapping、CacheRedis = redis,且不写 Placement 段 | `k8s_deploy_contract.tests.ps1` | 无 | 2 |
| `TestEnsurePlayerAllData_ParentOnCoreSubOnCache`、`TestTaskResultDispatcher_SubscribesCacheRedis` | `go/login/internal/logic/pkg/dataloader`、`internal/dispatcher` | 无 | 3 |
| `TestInvalidatePlayerCaches_SplitsParentAndSubKeys` | `tools/merge_zone` | 无 | 3 |
| kind 演练:新 namespace 下拆分 + robot login-test + 排空 + 杀 Pod | §10 | **需要 kind** | 1c / 2 / 3 |

> **(2026-10-18 更正,上表原文保留)**
> - 「契约:dbctl 不带 mapping 时拒绝」**作废**(§7 的更正),改为「dbctl 的 zone 缺组字段或组与线上不一致时拒绝」。
> - 「契约:`-RedisSplit mapping,dbctl` 时 db 的 RedisClient = redis-mapping、CacheRedis = redis,且不写 Placement 段」**作废**,改为:控制面句柄 = 该 zone 的 `redis-dbctl-g<N>`,父缓存 / 屏障句柄 = 核心,`Placement.Redis` 显式 = mapping。
> - 「`:839` 的断言抽成函数,对 `queue,mapping`、`mapping,dbctl`、`mapping,dbctl,cache` 三种渲染各调一次」与「既有的 `:839` 用例在所有拓扑下都通过」两行:`mapping,dbctl` 两种渲染里 go/db 的期望值按上一条改。
> - 两条「守卫新增的四个核心 pattern」与「核心族命中时拒绝 FLUSHDB」用例**作废**:回档已不再 FLUSHDB,`k8s_zone_rollback_flush_guard.tests.ps1` 已删除(§4.6 的更正)。
> - 补充用例(落码批次时再写,本次不写代码):第 2 批 zone → 组映射的渲染与拒绝;§18.7 的硬依赖用例(选值封装:新旧两种缓存格式并存,全部缓存读者——今天只剩运维工具——都能读两种;选同实例 / 同 slot:CRC16 slot 断言);第 4 批每段多键 Lua 的 KEYS 同 slot 断言,C++ 与 Go 键格式用 golden 向量对拍;「旧二进制按旧键 CAS、新二进制按新键 CAS 并存时必须被门禁拒绝」(§20.7)。

## 9. 归属目录在 Redis 侧会不会成瓶颈(回应负责人追问)

- **读写模型**(§6.1):
  - 写:每个玩家只在建角时写一次,另有合服运维时的批量改写。
  - 读:go/db 每个任务 2 次 MGET;每次进场 1 次 Lua;每次角色列表 1 次 MGET。
  - 全都是 O(1) 的小键,单实例的余量远大于首日推算值。这一点仍要用 L-14 压测证实。
- **先到瓶颈的不是映射本身**,而是第 2 批并进来的存盘控制面:每个写任务约 7 条命令。压测如果显示 `redis-mapping` 的 CPU 吃紧,第一步是把控制面拆到第四个实例,只改配置(§13 第 2 条)。
- **再往后**:
  - 映射不能靠只读副本扩容,因为 go/db 必须读主节点。
  - 要集群化,得把 `player:zone`、`placement`、`maprev` 改成同一个 hash tag 的 `player:{id}:…`,并把围栏复核从 Redis 发布脚本挪到库里。D1 的 db_first 提交点已经在库里(proposal §4),所以这是 D1 P2 之后的事,本稿不做。
- **全球多地域**:目录留在单一写入地域(proposal `:783-786`;`tidb-phase1-and-launch-readiness.md` §8)。
- **与库侧的关系**:D1 §12 已经论证,全局库上的映射写不是一种新的瓶颈。Redis 侧拆出 `redis-mapping` 之后,目录的热路径不再与会话、存盘、排队抢同一个 CPU。
- **本节只回答 Redis 侧**。负责人问的「归属全服库」如果指全局库里的 `player_route` 表(D1 db_first 的提交点),那是库侧的问题,答案在 `player-route-mapping-durability-proposal.md` §12(吞吐上限约为「每 zone 分片数 ÷ 事务时长」、TiDB 上的尾部 Region 与围栏整表热点)和 `tidb-phase1-and-launch-readiness.md` §5,本稿不重复。

> **(2026-10-18 更正,本节原文保留)**
> - 「单实例的余量远大于首日推算值」**作废**:2000 万口径下 mapping 读约 20 万~47 万 ops/s,内存约 100~300 GB(§16,推算 [I]),单实例放不下,必须按 `{player_id}` 分片。
> - 「先到瓶颈的不是映射本身,而是第 2 批并进来的存盘控制面」作废:控制面不并入 mapping(§18)。
> - 「要集群化,得把三族改成同一个 hash tag……D1 P2 之后的事,本稿不做」改为:原则已由负责人 2026-10-13 定为「以按 player_id 分片为准」,剩下的时机、形态与键名见 §21。

## 10. 交给 Codex 的验证命令与通过标准

工作目录都是仓库根。C++ 不涉及,不需要 MSBuild。任何一步失败都保留输出摘要并停下,不要连续自动重试。

**第 1a 批**

```
cd go/login && gofmt -l ./internal
cd go/login && go build ./... && go vet ./... && go test -count=1 ./internal/config/... ./internal/svc/... ./internal/logic/pkg/loginqueue/...
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_gate_drain.tests.ps1
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_zone_rollback_flush_guard.tests.ps1
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_zone_rollback_gate_router_mode.tests.ps1
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_zone_rollback_redis_split.tests.ps1
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/dev_tools_redis_split.tests.ps1
python tools/scripts/gen_docs_index.py --check
```

通过标准:
- gofmt 没有输出;
- 所有用例 PASS,或 fail=0;
- 契约测试的新用例全部出现在输出里;
- `gen_docs_index --check` 退出码为 0。

如果 Java 只改了注释,`mvn` 可以不跑;改了 Java 代码就要在 `java/gateway_node` 跑 `mvn -q test`。

**第 1b 批**

```
cd tools/redis_split && gofmt -l . && go build ./... && go vet ./... && go test -count=1 ./...
cd tools/merge_zone && gofmt -l . && go build ./... && go vet ./... && go test -count=1 ./...
cd tools/data_consistency_check && gofmt -l . && go build ./... && go vet ./... && go test -count=1 ./...
cd go/data_service && gofmt -l ./cmd/route_tool ./internal/routetool && go build ./... && go vet ./cmd/route_tool/... ./internal/routetool/... && go test -count=1 ./cmd/route_tool/... ./internal/routetool/...
# 需要本机 Redis(docker compose -f deploy/docker-compose.yml up -d redis)。PowerShell:
$env:REDIS_SPLIT_IT_ADDR='127.0.0.1:6379'; cd tools/redis_split; go test -tags=integration -count=1 ./...
```

通过标准:全部 PASS;集成测试只动 DB 14 / 15(跑完后用 `redis-cli -n 0 DBSIZE` 核对,DB 0 不变)。墓碑检查只读一个 `EXISTS`,不会改动既有 merge_zone 用例的行为,既有用例必须全部原样通过。

**kind 演练(第 1c 批之前,有外部依赖)**

1. 在新 namespace 执行 `dev_tools.ps1 -Command infra-up -RedisSplit queue,mapping -WaitReady`。期望:
   - `redis-queue-0` 和 `redis-mapping-0` 都 Ready;
   - `kubectl exec redis-mapping-0 -- redis-cli CONFIG GET maxmemory-policy` 返回 `noeviction`。
2. 两个 zone 执行 `zone-up`。期望:
   - login 横幅里有 `queue: redis-queue…`;
   - go/db 横幅的落点行是 `redis-mapping…`;
   - robot login-test 通过;
   - 用 `k8s_gate_drain.ps1` 标记一台 gate 排空,login 日志里按排空剔除了它;
   - 核心上 `SCAN MATCH player:zone:*` 为 0,`redis-mapping` 上的映射数随建角增长。
3. 演练一次 §5.4 路径 A 的完整流程:先在共享拓扑下建出一批角色,再迁移。期望:
   - verify 退出码为 0;
   - 冻结期间用 killswitch 关掉 RegisterPlayerZone,并人为在核心上写一个映射键,之后 delta 能把它补进 `redis-mapping`;
   - 部署闸在没有标记时拒绝;
   - 记录 copy / verify 的耗时,port-forward 与集群内 Job 各一次(§5.2「执行位置」),据此修正 §5.4 的冻结时长;
   - 切换后不带 `-mapping-redis-addr` 跑一次 merge_zone 只读审计,被墓碑拒绝。
4. 演练一次 §5.8 的 mapping 回退:解冻后再建一批角色,并在 `redis-mapping` 上人为改一个已有玩家的 `player:zone` 值;回退用 `restore`。期望:核心上新建角色齐全、被改的值以 `redis-mapping` 为准;之后不重新拷贝就尝试再次正向切换,部署闸以「目标标记不比源标记新」拒绝。
5. 不带 `-RedisSplit` 对已拆分的 zone 跑一次 `zone-up`(非 DryRun):部署闸拒绝,报错写明现拓扑;带 `-RedisSplit` 跑 `k8s-zone-rollback` 的 dry-run,输出里 Step 6 带着它。
6. 默认档回归:不带 `-RedisSplit` 跑 `k8s_deploy.ps1 -DryRun`,dev 档产物与改造前逐字节相同;release 档只多出 gateway 的 `password` 一行。

**第 2 批**

```
cd go/db && gofmt -l ./internal && go build ./... && go vet ./... && go test -count=1 ./internal/kafka/... ./internal/config/... ./internal/svc/...
cd tools/merge_zone && go build ./... && go vet ./... && go test -count=1 ./...
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1
```

kind 上两个 zone 按 §5.5 逐个 zone 迁移。zone 号要选成一个是另一个的前缀(例如 1 和 10),专门验证 `-topics` 不串 zone。期望:
- robot 的存盘没有中断(Kafka 缓冲之后能追平);记录每次缩容 / 扩回时其它 zone 的 lag 尖峰(L-09 的全组 rebalance);
- 游标在 `redis-mapping` 上增长;迁移 zone 1 之后,zone 10 的游标在核心上照常增长、在 `redis-mapping` 上一条都没有;
- 对 zone 1 演练一次 `restore` 回退:回退前在 `redis-mapping` 上让 zone 1 的游标继续前进,回退后核心上的值与之相等,不是切换前的旧值;
- `go test ./internal/kafka/...` 里 retry_ownership 的既有用例仍然 PASS。
- **(2026-10-18 更正,上面第 2 批的命令与期望原文保留)** 期望里的「`redis-mapping`」一律改为该 zone 所属组的 `redis-dbctl-g<N>`;kind 上至少建两个组,zone 1 与 zone 10 分到不同组,再多核对一条:迁移后 zone 1 的 `save_barrier` ACK、补投桶与资产证据仍在核心上增长,组实例上一条都没有(§18.7)。命令清单要按第 2 批的落码规格重列(go/db 句柄拆分会改 `internal/saveoutbox`、`internal/config` 等包)。

**第 3 批**:login 与 go/db 的单测、merge_zone 单测、契约测试,另在 kind 上全服一拍切换一次。

## 11. 与现有设计的偏离与代价

1. **任务的四类变成三类加核心**:「会话与锁」「PlayerAllData 缓存」不能按字面拆(§3.3)。代价:核心仍是最大的单点。
2. **no-single-node §5 原本在「哨兵」与「按用途拆库后分别集群化」之间待定**。本稿只拆,不集群化,也不做哨兵。代价:每个实例仍是单点,可用性没有提高,只是缩小了爆炸半径。
3. **D1 §5.2 的论证在第 1 批与第 2 批之间暂时不成立**:映射已经在 `redis-mapping`,游标还在核心。
   - 只丢 `redis-mapping` 时,D1 第 3 批之前 go/db 会走 legacy 分支,按本 zone 选库(`placement_route.go:171-172`)。
   - 今天所有环境都是 `PinOnCreate=false`,也从没执行过 pin / relocate(`docs/design/player-storage-placement.md:5`),所以 legacy 分支的结果就是对的。
   - D1 第 3 批读方开关之后,行为变成延后写。
   - 建议第 1 批和第 2 批之间不要跨过「打开 PinOnCreate」或「执行 pin / relocate」。
4. **merge_zone 的句柄从 5 组变成 6 组**(加 `-dbctl`),dev_tools 的参数面随之变大。
5. **同址契约从隐式变显式**:契约测试 `:839` 不改,但「go/db 读落点的 Redis 与 MappingRedis 同址」从「都在核心,隐式同址」变成「都显式指向 redis-mapping」。
6. **CZ-2 / D12 不变**:核心仍是物理共享的单实例。
7. **Java gateway 改指 `redis-queue`**:`RateLimitConfig.java:84` 注释里「与 login 的 token 是同一实例」要更正。
8. **新增一个 Go module `tools/redis_split`**,不引入 miniredis。单测靠接口,真 Redis 走 integration tag,与 merge_zone 同口径。
9. **第 1a 批有两处缺省即生效的改动**(§1 第 4 条):release 档 gateway 补口令、回档守卫补核心族。代价:release 档的 gateway ConfigMap 多一行;守卫今天看不到变化。login 超时参数的修复因为会改核心 client 的行为,刻意不纳入。
10. **`-RedisSplit` 成了每次部署都要带的参数**。代价:例行部署、回档、发布入口都要转发;漏带由部署闸拦下而不是静默回退,但会让一次例行部署失败。备选是把拓扑存进 infra namespace 的 ConfigMap、zone-up 缺省读它;那样 DryRun 读不到集群,与 Kafka 拓扑的现有做法也不一致,所以本稿不采用,列入 §13 第 16 条。

> **(2026-10-18 更正,上面各条原文保留)**
> - 第 2 条「本稿只拆,不集群化」作废:单元内 Cluster 已拍板(§20)。
> - 第 3 条「D1 §5.2 的论证在第 1 批与第 2 批之间暂时不成立」改为**永久不成立**:控制面按 zone 组分实例,不会与映射同实例(§2.4 第 3 条的更正);第 2 批改以条件写落地为前置(§18.3)。「第 1 批和第 2 批之间不要打开 PinOnCreate 或执行 pin / relocate」的建议仍有效。
> - 第 5 条「都显式指向 redis-mapping」只对 `Placement.Redis` 成立;go/db 控制面指组实例(§18)。
> - 第 6 条「CZ-2 / D12 不变」作废,改写为「逻辑单一、物理按 `{player_id}` 分片」,见 §20。

## 12. 风险

1. **配错地址就会让映射静默写错库**。防线:迁移标记(且必须比源上的新)、契约测试、`k8s-redis-split-status`(含 rollout 完成)、墓碑(工具连到旧实例即拒绝)、purge 让配错的组件大面积失败而不是静默出错。
   - 残余:purge 之前的浸泡期里,绕过部署闸手工改回 ConfigMap 的服务读到的是冻结时的旧映射,只缺冻结之后新建的号。§13 第 15 条的服务启动期墓碑检查能关掉这个窗口;D1 就绪标记上线后也能关掉。
2. **killswitch 是 fail-open 的**:由 delta 兜住;delta 发现冲突就交给人处理。
3. **核心内存没有上界**(§2.5 第 1、2 条)。这与拆分无关,但决定了 maxmemory 能不能设:设了 noeviction + maxmemory 之后,满了就拒写,全服存盘和登录都会失败。
4. **C++ 不发 AUTH**:release 档核心一开 requirepass,C++ 全线 NOAUTH。本稿的新实例都没有 C++ 客户端,可以开密码;核心怎么处理待拍板。
   **(2026-10-15 结论,上句原文保留)** 已拍板为「给 `RedisManager` 加 AUTH」(§13 第 7 条),C++ 与 K8s 两批都已落码、未经 Codex 编译 / 测试,详见 §2.5 第 3 条的更正。剩余风险:口令配错时 C++ 节点 fail-closed(zone Redis 不可用、Pod 仍 Ready),「zone Redis 已认证」的就绪条件另起一项,不在本次最小修复内。
5. **实例变多,运维面变大**:PVC、备份都要多管几份。Redis 今天没有任何备份,AOF 只在 PVC 上。映射在 D1 之后有库里的真源,控制面在库侧没有备份。
6. **第 2 批逐 zone 停 go/db**:存盘暂停期间,新号首次进场冷读会失败(8s 超时)。而且在 L-09 之前,每次缩容与扩回都会让**全服**存盘停几秒(共用消费组的 rebalance),Z 个 zone 一共 2Z 次。
7. **迁移期间必须禁用 merge_zone 和 relocate,第 2 批期间还要全服禁止在线扩分区**,目前只靠运维纪律和手册约束;墓碑只能拦住「连错实例」,拦不住「在窗口里运行」。
8. **`redis-mapping` 满了时**,go/db 写游标失败 → 重试耗尽预算 → 进入没有消费者的死信(L-04)。拆分把这个风险集中到了 `redis-mapping` 的容量上,必须配告警。
9. **每个新实例都是单点**:
   - `redis-queue` 挂掉:排队关着时只影响排空过滤与限流(都放行);排队开着时新登录全部失败(fail-closed,§3.2.1),已在线玩家不受影响。
   - `redis-mapping` 挂掉:建角、进场(要读 home)、存盘全部停止,影响面与今天核心挂掉相同,只是故障源多了一个。
   - 所以 `redis-mapping` 是 HA 的第一优先级。
10. **WAITAOF 在 `redis-mapping` 上的延迟没测过**(D1 L-12)。
11. **回退不能只补缺**。旧实例上的旧值会被当成权威:映射是旧归属,控制面是更小的游标与 epoch(重放已落库的任务),子表缓存比库旧(拼父键等于回档)。所以映射与控制面回退一律 `restore`,缓存回退先清核心上的子表缓存(§5.8)。
12. **`-RedisSplit` 漏带**:由部署闸拦下,不会静默回退,但会让例行部署或回档失败。回档脚本在停服前真查一次拓扑(§4.2),把失败挪到停服之前。

> **(2026-10-18 更正,上面各条原文保留)**
> - 第 3 条「核心内存没有上界」改为 S1 前拍板(§23);2026-10-16~17 又加了几类常驻键,补投意图在 go/db 停摆时还有一个随在线人数与停摆时长增长的上界(§16)。
> - 第 6 条「第 2 批逐 zone 停 go/db」:③ 代码已入库但未验证;各 zone 切到按 zone 的消费组并收尾之后,全服 rebalance 停顿才消失(§5.5 的更正)。
> - 第 8 条「`redis-mapping` 满了时 go/db 写游标失败」在 §18 之后落在各 `redis-dbctl-g<N>` 组实例上:组实例满了,该组各 zone 的写任务重试耗尽预算进死信。每个组实例都要配容量告警。
> - 第 9 条单点分析补一个组实例:某个 `redis-dbctl-g<N>` 挂掉,只影响该组各 zone 的存盘(拿不到锁、读不到游标就延后);核心挂掉时 go/db 的屏障 ACK 与补投也一起停(§18.7)。
> - 新增风险:**围栏硬依赖**。令牌发布脚本、C++ 父快照提交屏障、登录冷恢复、补投工作器都要求「父键、owner_epoch、`save_barrier`、补投桶(以及今天的排序锁与子表缓存)在同一个 Redis 实例上」(db-write-fencing-mysql.md 文末 2026-10-17 各附注;`go/db/internal/saveoutbox/service.go` 的拒启)。任何拆分或分片都要先过 §18.7。

## 13. 待负责人拍板

1. 是否采纳「核心 + 2 个新实例 + 1 个可选实例,分 3 批」,以及「会话与锁 / PlayerAllData 父键不拆」的结论。
2. 控制面是并入 `redis-mapping`(本稿推荐),还是单独开第四个实例。
3. 第 3 批(`redis-cache`)做不做、什么时候做;是否先做 `task:result` 按 login 实例分频道。
   - **2026-10-18 已答复后半句**:负责人 10-17 定为先做分频道,分两批,先用 PUBLISH。第 1 批(proto + go/shared + login)已落码,未编译;go/db 侧是第 2 批。前半句(第 3 批做不做)仍待定。
   - **2026-10-18 第 2 批**:go/db 侧(定向发布、计数、独立告警)也已落码,未编译。前半句仍待定。
4. 各实例 maxmemory 与 limits 的初值(等 L-14);核心是否在第 1c 批的维护窗口里显式化(需要一次全服重启)。
5. 核心内存无上界怎么处理:另写一份冷玩家卸载设计,还是按累计玩家规模配内存。
6. Redis 监控:先做 INFO 快照(不引入新镜像),还是直接上 redis_exporter sidecar。
7. C++ AUTH:给 `RedisManager` 加 AUTH,还是核心不开 requirepass、改用 NetworkPolicy 限制访问。
   **(2026-10-15 已拍板,原文保留)结论:选 AUTH,已落码**(P0-A,`docs/handoff/global-singletons-p0-findings-20261014.md`)。理由:release 档已强制 Redis 口令,Go 与 Java 也已接线;不开 requirepass 等于把核心交给同网段任何 Pod。
   K8s 侧选方案 A(口令明文进 `node-config`,与 Go 服务 / gateway 的 ConfigMap 同一做法;C++ 工作负载带只哈希口令的指纹注解)。两批都未经 Codex 编译 / 测试;取值与上线步骤待实测复核,见 `deploy/k8s/README.md`「C++ 节点的 zone Redis 口令」。
8. AGENTS §6.2 中 Redis 清库口径的改写(由负责人改 AGENTS.md)。
9. `stress_summarize` 增加 Redis 表(会改 §6.1 规定的输出口径)。
10. 拆分与 L-02 的先后。建议先拆 queue,再打开排队与限流。
11. 映射迁移走路径 A(冻结拷贝,今天就能做),还是等 D1 第 1b、3 批到位后走路径 B。
12. purge 前的浸泡时长(缺省 24h),以及由谁执行。
13. 新实例是否也用 redis-ha 形态(`redis-mapping` 优先),以及补上 `-RedisReplicas` 的 apply 路径(`tidb-phase1-and-launch-readiness.md` §9.5)。
   - **(2026-10-19 更正,上句原文保留)后半句已完成**:核心实例 `redis` 已有高可用形态与 `-RedisReplicas` 的 apply 路径(origin `43284eb0b` 10-16 接入;
     本地 10-19 补齐 WAITAOF 副本份数按拓扑渲染、zone 写路径跟随线上 `sts/redis` 等;已落码,契约测试未运行、未在 kind 复测,待 Codex 验证;
     见 `no-single-node-horizontal-scaling-20261001.md` §5.1)。它只管核心一个实例。
   - **前半句仍待定**:按用途拆出的新实例是否也用高可用形态。若用,§4.4 第 3 条的 `kubectl exec svc/<target>` 要改为 exec 节点 StatefulSet 的 redis 容器
     (高可用形态下 Service 指向 haproxy,代理镜像没有 redis-cli;`k8s_gate_drain.ps1` 10-19 已这样改)。
14. `deploy/k8s/AGENTS.md:24`、`:82` 对 Redis 拓扑的描述要随落码更新。它是 AGENTS 文件,改不改由负责人确认。
15. data_service 与 go/db 启动时是否检查自己映射句柄上的墓碑(命中拒启,§5.2「误配防线」)。做了能兜住手工改 ConfigMap 的绕过,代价是第 1 批不再是纯配置改动。
16. 拓扑是继续用「每次部署带 `-RedisSplit`」(本稿),还是存进 infra namespace 的 ConfigMap、由 zone-up 缺省读取(§11 第 10 条)。
17. 第 2 批是否等 L-09(消费组按 zone 命名)做完再开始(§5.5)。
18. login Redis 超时参数没传进 client(§2.5 第 8 条)单独修不修;修的话核心 client 的 DialTimeout 从 5s 变 3s,要单独评审。

> **(2026-10-18 更正,上面各条原文保留)** 待拍板项以 §25 为准:
> - 第 1 条:「核心 + 2 个新实例 + 1 个可选实例,分 3 批」由 §24 的分批取代;「会话与锁 / PlayerAllData 父键不拆」在单元内 Cluster 下改为「同 `{player_id}` tag、同 slot」(§20)。
> - 第 2 条:由 §18「按 zone 组分实例」取代,剩下的组大小与跨组合服进 §25。
> - 第 3 条:后半句(先做分频道)已答复并落码(B0-3,§17);前半句改为必做(§19),形态进 §25。
> - 第 5 条:改为 S1 前必须拍板(§23、§25)。
> - 第 11 条:规模后只能走路径 B(§5.4 的更正),与 mapping 改 tag 的时机一起进 §25(§21)。
> - 第 13 条:后半句「补上 `-RedisReplicas` 的 apply 路径」已于 2026-10-19 落码:非 dev 档缺省 3 副本,即 1 主 2 从、3 个哨兵、2 个入口代理。契约测试还没跑,也没在 kind 复测,待 Codex 验证,见 §2.5 第 6 条的 2026-10-19 更正。核实之前,§12 第 9 条的单点风险不能视为已消除;HA 也只解决可用性,不解决容量(§16)。前半句「新实例是否也用 HA」进 §25.2 第 18 条。
> - 第 14 条:`deploy/k8s/AGENTS.md` 的 Redis 拓扑描述还要加上「单元化 + 单元内 Cluster」的方向,仍由负责人确认是否改。

## 14. Java 版(AGENTS §12)

- **现状**:
  - Java 版只有一个 `game.redis.*`,用 Redisson,缺省 DB 12(`D:\luyuan\xuanming-server-mmo\game-discovery\src\main\java\com\game\discovery\RedisProperties.java`)。
  - 排队与限流在 game-gateway 里(`queue/LoginQueue.java`、`queue/QueueDispatcherRunner.java`、`ratelimit/RedisRateLimitStore.java`)。
  - 账号与角色在 MySQL(`PARITY.md:41`)。
  - 没有 K8s 部署,只有 compose,单个 Redis(`deploy/compose/infra.yaml:59-64`)。
- **判断**:本拆分是服务端内部的部署形态,不涉及客户端契约(AGENTS §12「两版共享的只有客户端契约」)。Java 版状态 = **待做**。
  - 原因 1:Java 还没有生产部署形态。
  - 原因 2:它的键族与本仓不同(账号在库里),拆分要按 Java 自己的键重新盘点。
- **建议做法**:在 game-discovery 的 `RedisAutoConfiguration` 里按用途提供多个 RedissonClient bean,例如 `game.redis.queue.*`,缺省回落到 `game.redis.*`;先拆排队与限流。
- **PARITY.md 登记**:第 1 批落码时登记一行「共享 Redis 按用途拆实例」:mmorpg commit ↔ Java「待做」+ 上述原因。
- **(2026-10-18 补充,本节原文保留)** 账号目录 Java 已在 MySQL(`game-player-store/src/main/resources/db/game-player-schema.sql` 的 `account` 表),本仓 §22 的落库与之「实现不同」;单元化、分片与 friend 在线目录游标的两版同批要求见 §26。

## 15. 文件清单

**第 1a 批**(约 22 个文件)

新增:

| 文件 | 要点 |
|---|---|
| `deploy/k8s/manifests/infra/redis-purpose.yaml` | 模板,占位见 §4.3 |
| `go/login/internal/config/queue_redis_conf_test.go` | §8 |
| `go/login/internal/svc/queue_redis_test.go` | §8 |
| `tools/scripts/tests/dev_tools_redis_split.tests.ps1` | §8 |
| `tools/scripts/tests/k8s_zone_rollback_redis_split.tests.ps1` | §8 |

修改:

| 文件 | 要点 |
|---|---|
| `go/login/internal/config/config.go` | NodeConfig 加 `QueueRedis` |
| `go/login/internal/svc/servicecontext.go` | `newRedisClient`、`newQueueRedis`、`QueueRedis`,四个注入点;client 选项与今天逐字相同 |
| `go/login/login.go` | 启动横幅补 `queue:` 一行(`:466` 之后) |
| `go/login/etc/login.yaml` | 只加注释:不写 QueueRedis = 共享 |
| `tools/scripts/k8s_deploy.ps1` | 新参数、`Get-RedisTopologyFor`、`Assert-RedisTopologyChangeIsSafe`、渲染 redis-purpose、各 ConfigMap 取拓扑、gateway 补密码、核心占位 |
| `deploy/k8s/manifests/infra/redis.yaml` | 加 `__REDIS_CORE_ARGS__`、`__REDIS_CORE_LIMITS__`;缺省渲染逐字节不变 |
| `tools/scripts/tests/k8s_deploy_contract.tests.ps1` | §8 |
| `tools/scripts/k8s_gate_drain.ps1`、`tests/k8s_gate_drain.tests.ps1` | 读 QueueRedis |
| `tools/scripts/k8s_zone_rollback.ps1`、`tests/k8s_zone_rollback_flush_guard.tests.ps1` | 守卫补四个核心 pattern(不含 `PlayerAllData:*`);新增 `-RedisSplit` 并转发给 Step 6;停服前真跑拓扑闸 |
| `tools/scripts/dev_tools.ps1` | `k8s-redis-split-status`、`k8s-redis-wipe`;转发 `-RedisSplit` 等参数 |
| `tools/scripts/stress_snap.ps1` | `-RedisTargets` |
| `java/gateway_node/src/main/java/com/game/gateway/ratelimit/RateLimitConfig.java` | 只改注释 |

文档:本文状态更新;`deploy/k8s/README.md`;`docs/design/no-single-node-horizontal-scaling-20261001.md` §5 状态改为指向本文;`docs/design/tidb-phase1-and-launch-readiness.md` L-06 行加注。

**第 1b 批**:

| 文件 | 要点 |
|---|---|
| `tools/redis_split/{go.mod, go.sum, main.go, families.go, scan.go, copy.go, delta.go, restore.go, verify.go, marker.go, purge.go}` | §5.2(`marker.go` 含 `mark` / `unmark` / 墓碑) |
| `tools/redis_split/{families,copy,delta,restore,verify,marker}_test.go`、`integration_test.go` | §8 |
| 集群内一次性 Job 清单(新,位置随 go-svc 清单目录约定) | §5.2「执行位置」 |
| `tools/merge_zone`(新文件放墓碑检查,`main.go` 接线)+ 测试;`tools/data_consistency_check/main.go` + 新测试;`go/data_service/internal/routetool/redisread.go` + 测试 | 映射句柄的墓碑检查(§5.2「误配防线」) |
| `docs/ops/redis-split-runbook.md` | §5 的运维步骤 |
| `docs/README.md` | 登记 runbook;文件名命中 `*runbook*` 规则,会自动归入「运维手册」 |

**第 2 批**:

| 文件 | 要点 |
|---|---|
| `go/db/internal/config/config.go`、`config_test.go` | `CacheRedis` |
| `go/db/internal/svc/servicecontext.go` | `CacheRedis` 句柄 |
| `go/db/internal/kafka/key_ordered_consumer.go`、`placement_route.go`、`placement_route_test.go`(构造函数改五参数)、`cache_redis_test.go`(新) | `cacheClient`;`processTaskWithoutLock` 的三个调用点 |
| `go/db/db.go` | 构造参数、横幅 |
| `tools/merge_zone/{main.go, preflight.go, audit_resources.go, audit_checks.go, merge_run.go, relocate_run.go, unmerge.go}` + 测试 | `-dbctl-redis-*` |
| `tools/scripts/dev_tools.ps1`、`tests/dev_tools_merge_zone_contract.tests.ps1` | 转发 `-MergeDbCtlRedis*` |
| `tools/scripts/k8s_deploy.ps1`、契约测试 | dbctl 渲染 |
| `docs/ops/merge-zone-runbook.md`、`docs/design/player-storage-placement.md` §6.2、`docs/design/player-route-mapping-durability-proposal.md` §3.3 | 加注 |

**第 3 批**:

| 文件 | 要点 |
|---|---|
| `go/login/internal/config/config.go`、`internal/svc/servicecontext.go`、`internal/logic/pkg/dataloader/{ensure_player_all_data_async.go, sync_loader.go}`、`internal/logic/clientplayerlogin/entergamelogic.go` 的调用点 + 测试 | `CacheRedis`、父键与子表分句柄 |
| `tools/merge_zone/{player_rows.go, main.go, merge_run.go, unmerge.go}` + 测试 | 缓存失效拆分 |
| go/db `cmd/verifier`、`cmd/data_stress`;robot `data_stress.go`;data_service `cmd/debug_fetch` | 参数或 yaml |
| `tools/scripts/k8s_deploy.ps1`、契约测试 | cache 渲染 |

**改动面**:第 1a 批约 22 个文件,第 1b 批约 20 个文件,合计超过 AGENTS §10.2 的 30 个文件门槛。落码前需要负责人授权;授权后也按 1a、1b 严格分两次提交,每次单独由 Codex 验证。

> **(2026-10-18 更正,本节原文保留)** 本节只对第 1a、1b 批还有参考价值,而且要去掉已失效的两项:`k8s_zone_rollback.ps1` 的守卫补核心族,以及 `k8s_zone_rollback_flush_guard.tests.ps1`(回档已不再 FLUSHDB,该测试文件已删除,§4.6 的更正)。第 2、3 批的文件清单作废,由各自的落码规格按 §18、§19 重列。第 0 批已落码的文件见 §17 引的 PROGRESS 条目,第 4 批见 §24。

## 16. 修订依据与 2000 万口径重算(2026-10-18 新增)

**状态**:本节所有数字都是「公式 + 参数」推出来的 [I],没有任何压测证据,不能当验收预算,也不据此宣称性能结论(AGENTS.md §6)。单实例上限一律是量级参照,本仓没有测过:单主简单命令按 5 万 ops/s(全局单点交接 §5 的保守值)到 10 万 ops/s(容量模型 §0.5);单实例内存宜在几十 GB 以内(容量模型 §2.6 按 25 GB 参照)。

### 16.1 规则

- 按在线人数增长的项,用容量模型 §7.2 的 2000 万口径:E = PCU/1800 ≈ 11,111/s,λ_sw = PCU/300 ≈ 66,667/s。§7.2 也说明了旧 S3 的峰均比 2.4 为什么不能沿用。
- 按日活增长的项(C_n、N_reg、N_acc、P_cum),DAU 仍是 1 亿,沿用容量模型 §0.3 / §0.4 的 S3 值。
- **不把 S3 的表直接乘二**。有的项随 PCU 线性增长;有的随 Z×N_s 或 R×L 近似平方增长(SM 刷新与会话对账的改前形态、task:result 广播);有的只随 DAU 增长。
- 下列派生量**不在 §7.2 里**,是本稿按容量模型 §0.3 的参数与 PCU = 2000 万推出的,标 [I]:
  - Z = PCU / C_z:C_z = 10 万时 200,C_z = 4.5 万时约 445;
  - N_s = PCU / c_scene:c_scene = 11,250 时约 1,778,2,250 时约 8,889;
  - N_g = PCU / c_gate:c_gate = 2 万时 1,000,4,500 时约 4,444;
  - λ_x = f_x·λ_sw:f_x 取 0~0.5,得 0~33,333/s;
  - L(login 实例数)= 2Z = 400~890;
  - Q(开服排队人数)= 0.1×PCU = 200 万,出处是 §0.3 的假设「Q = 0.1×PCU」。
- 区间配对与容量模型相同:下沿取 C_z = 10 万、c_scene = 11,250、1−s = 0.5、f_x = 0;上沿取 C_z = 4.5 万、c_scene = 2,250、1−s = 1、f_x = 0.5。V(父快照)取 10~21 KB,B(`player_database` 序列化)取 8~25 KB(§0.3)。

### 16.2 推算表(全部 [I])

| 量 | 公式与出处 | 2000 万推算 | 备注 |
|---|---|---|---|
| N_save(每秒真存盘) | PCU(1−s)/300 + E + λ_x(1−s)(容量模型 §2.3) | 4.4 万~11.1 万/s | 不含战斗结算、资产提交、收件箱、交接触发的事件存盘 |
| DBTask | 2·N_save | 8.9 万~22.2 万/s | `player_database_1` 那条几乎为空 |
| 核心:C++ 存盘 EVALSHA | N_save | 4.4 万~11.1 万/s | 2026-10-17 起 owner_epoch≠0 的主路径是 `SaveWithCommitBarrier`(四键 Lua),次数不变,每次多写一份完整意图 |
| 核心:进退场链路 | ≈ 43E(容量模型 §2.5,静态盘点,未逐条计数) | 约 47.8 万/s | B0-1 的索引维护在会话 Lua 内部,不增加往返 |
| 核心:切场景 | ≈ 10λ_sw | 约 66.7 万/s | |
| 核心:SM 负载刷新 | 改前 0.8·Z·N_s;改后(B0-5)读 0.4·N_s,写按每拍每节点 1 条、每 zone 1 条估,本表取上界 0.8·N_s | 改前 28 万~316 万 GET/s;改后约 0.14 万~0.71 万/s | 跟随者改后为 0 |
| 核心:建角常态 | ≈ 29·C_n,C_n ≈ 69/s(按 DAU) | 约 0.2 万/s | |
| **核心合计** | 上五项之和:第 0 批 + 按用途拆分之后,不含子表缓存、令牌发布脚本、friend 登录 MGET、排队 | **约 119 万~127 万 ops/s** | 对 10 万参照 12~13 倍。不能与容量模型 §2.5 的 1000 万口径 71 万~153 万直接比:那一行含子表缓存 SET 与改前的 SM 刷新 |
| friend 登录 MGET | E×F,F = 50~200(容量模型 §2.8) | 键 55.6 万~222 万/s;命令:单实例约 1.1 万/s(F 不超过 `ListReadHardLimit` 1000 时每次登录一条 MGET,`SessionReader.readChunk`,`session_reader.go:168`);Cluster 下按 slot 拆开,命令数趋近键数 | 计入核心预算,键/s 与命令/s 分开看 |
| 核心合计(含 friend,单实例口径) | | 命令约 120 万~128 万/s,另加 55.6 万~222 万键/s 的批量读 | |
| 核心写带宽 | N_save×(V + B):父快照加一份完整 `player_database` 意图 | 约 0.8~5.1 GB/s | 2026-10-17 起父快照与意图在同一段 Lua 里写入 |
| 令牌发布脚本 | 每个写任务一次 EVAL;写入的值约 N_save×B | 8.9 万~22.2 万 EVAL/s;约 0.36~2.8 GB/s | 落在哪个实例取决于 §18.7 的拍板 |
| mapping 读 | 2·DBTask(选库、复核各一次 MGET)+ 2E + 3C_n | 约 20 万~47 万 ops/s | 每次 MGET 两键,键读数约加倍;`RequireRouteReady` 开时次数不变,每次多两段 INFO(改走 `RouteReadyReadScript`,§18.7) |
| 控制面 | 6·DBTask:取锁、游标 GET、epoch GET、扩分区状态 GET、推进脚本、释锁(步骤标签见 `go/db/internal/metrics/pipeline.go` :110-121);含令牌发布脚本为 7·DBTask | 6·DBTask 约 53 万~133 万 ops/s;含令牌脚本约 62 万~155 万。÷Z 按区间配对(下沿 Z = 200、上沿 Z = 445):每 zone 约 0.27 万~0.35 万(只算 6·DBTask 时约 0.27 万~0.30 万) | 续租看门狗、重试认领另计 |
| 子表缓存内存 | ≈ 3·PCU·V,TTL 1 h(容量模型 §2.5) | 约 0.6~1.2 TB | 2026-10-17 起已无生产读者(§19) |
| 核心常驻 | ≥ P_cum×(V + 0.3 KB),P_cum ≥ 1 亿(30 天 2~3 亿) | 至少 1~2 TB;30 天 2~6 TB | 按 DAU,沿用 S3 |
| 其中在线热集 | PCU·V | 约 200~420 GB | 即使冷卸载也放不下单实例(§23) |
| 核心新增常驻项 | `save_barrier`:pending 与两个 ACK 各 41 字节,约 0.2~0.3 KB/玩家 × P_cum;`asset_sql_commit`:每玩家一份账本,含托管收据里的完整实例字节,大小未测;gate 索引约 1~1.6 GB(B0-1 设计 §7) | `save_barrier` 约 20~90 GB(P_cum 1 亿~3 亿) | 都不设 TTL;gate 索引成员随会话增删,与在线人数成正比 |
| 补投意图 | 每个「父快照已写、两表 SQL 未双 ACK」的玩家一份,约 B | 正常时 ≈ N_save×T_res×B,T_res 是工作器扫到的延迟加上至多 30 s(`FirstReplayAfter`,另有至多 5 s 抖动),按 T_res ≈ 30 s 约 11~83 GB [I];go/db 停摆 T 秒时上界 ≈ (PCU+E·T)×B,1 h 约 3·PCU×B ≈ 0.48~1.5 TB [I] | 只有补投工作器 `claimScript` 的 complete 分支会 HDEL 意图(合服 / 撤销与回档契约是整键删屏障);补投只在 dual 下运行(`saveoutbox/service.go` 的 `Start`);新的尖峰内存项,要告警(§23、§25);工作器容量见 §25.2 第 21 条 |
| mapping 内存 | N_reg×0.1~0.3 KB,N_reg = 10 亿 | 约 100~300 GB | 按 DAU |
| 账号目录 | N_acc×0.15~0.3 KB + N_reg×0.1~0.15 KB | 约 205~360 GB | 落库之前在核心(§22) |
| 会话对账 | 改前 2Z·N/60;改后(B0-1)每拍约 3 条 + N_g 条 SCARD,另有限速的补建 / 审计 | 改前 1.33 亿~2.97 亿 GET/s;改后约 17~74 SCARD/s,加约 380 条/s 的补建 / 审计(B0-1 设计 §7 的推算) | 改后补建一遍约 9 小时 |
| 排队轮询 | 每 2 s 一次,每次 3 次往返,其中一次是脚本,内含 3 条命令(`login-queue-2026-05.md` §5 的 2026-10-18 更正) | 往返约 300 万/s(1.5·Q);服务端命令约 500 万/s(2.5·Q) | redis-queue 也要按 zone 组分或用 `{zone}` tag(§24) |
| 全服重连浪涌 | PCU/120×35(容量模型 §2.5,只算进场链路) | 约 583 万/s | |
| task:result 扇出 | R×L(容量模型 §7.2 的 2026-10-18 行) | 稳态 R ≈ 139/s:B0-3 两批全量上线前 5.6 万~12.4 万/s,之后约 139/s。f_miss→1(冷卸载上线或核心切主)时 R ≈ 2.2 万/s:上线前 889 万~1,976 万/s,之后约 2.2 万/s | B0-3 已落码,未验证(§17) |
| 映射拷贝冻结窗口 | 3×N_reg 个键 ÷ 5 万键/s | S1 口径(N_reg = 1 亿)约 3 亿键、约 100 分钟;规模后(N_reg = 10 亿)约 30 亿键、约 16.7 小时 | 规模后只能走路径 B(§5.4 的更正) |

### 16.3 结论

- 拆实例只是第一步。对单实例参照(5 万~10 万 ops/s、25 GB),核心 ops 约 12~25 倍、子表缓存内存约 24~48 倍、排队约 30~100 倍、mapping ops 约 2~9 倍 / 内存约 4~12 倍(推算 [I]);控制面合计也超出,只是它天然按 zone 切得开。
- 2000 万口径下,单元化 + 单元内分片是必要条件,不是优化项。每单元的预算见 §20.2。
- 单元规模、组大小、分片数都要等容量模型 §5 的 V2 实测单主上限之后再定。

## 17. 第 0 批五项的定位与现状(2026-10-18 新增)

**状态**:五项都已落码,**全部未编译、未测试、未验证**,交 Codex 的命令在各自的 PROGRESS 条目里。它们不改键族归属,不需要 `-RedisSplit`。规格都在 `docs/handoff/launch-readiness-20261013/`,每份后半是核实意见。下表最后一列写的是它们对 §20 Cluster 化的接缝。

| 项 | 做了什么 | 规格 / PROGRESS 条目 | 新键、频道与跨槽绑定 | 上 Cluster 时怎么改形 |
|---|---|---|---|---|
| B0-1 会话对账按 gate 索引 | replace / setDisconnecting / deleteIfUnchanged 三段会话 Lua 原子维护 gate 索引;对账改为全服单 leader,每拍只处理死 gate 的索引,不再 SCAN 全键空间。设计见 [player-locator-gate-session-index.md](player-locator-gate-session-index.md) | `spec_B0-1_reconcile_by_gate.md`;PROGRESS「2026-10-18 B0-1:player_locator 会话对账按 gate 实例建索引、去掉全键 SCAN」 | 键在 `go/player_locator/internal/logic/keys.go` :25-41:`player:gate_index:sessions:<gate 实例 uuid>`(SET)、`player:gate_index:gates`、`…:ready`(PX 3.5 拍)、`…:gen`、`…:last_audit`、`…:audit_cooldown`,选主锁 `player_locator:session_reconciler:leader`。会话 Lua 里索引与 `player:session:<id>`、全局 `player:leases*` 同一段脚本,跨槽。补建 / 审计仍 SCAN `player:session:*`(`session_reconciler.go:1092`),并靠 INFO 的 run_id 判一遍是否有效 | 负责人 10-17 已定:索引在会话 Lua 里原子维护,上 Cluster 时按 slot 分桶改形。按设计 §11:以会话 `{player_id}` 所在 slot 的预计算 slot-tag 分桶为 `player:gate_index:{<slottag>}:sessions:<uuid>` 与 `player:gate_index:{<slottag>}:gates`,与 `player:leases*` 的 slot 分桶同批设计;补建 SCAN 与 run_id 核对改为逐主节点。调研规格 §17「索引不得与会话原子绑定」的硬约束作废 |
| B0-2 删 `dirty_keys_set` | 两段存盘 Lua 删掉 SADD,只访问 KEYS[] 声明的键;回归 `RedisSaveScriptsTouchOnlyDeclaredKeys` | `spec_B0-2_dirty_keys_set.md`;PROGRESS「2026-10-16 删除 C++ 存盘 Lua 的全服键 dirty_keys_set 写入」 | 去掉一个脚本里未声明的全局键。全部 scene 换成新镜像之前生产仍在写;存量清理口径在 `docs/ops/release-checklist.md` §6.1,负责人 10-17 定暂不执行、等指定环境 | 是 Cluster 的必要条件之一,不是充分条件:守卫脚本的父键与 `owner_epoch` 仍无共享 tag,10-17 的四键屏障脚本又引入了按 zone 分桶的索引(§20.4) |
| B0-3 task:result 分频道 | 第 1 批:DBTask `reply_instance = 10`、`go/shared/taskresult`、login 一条 SUBSCRIBE 同订旧频道与本实例频道;第 2 批:go/db `publishTaskResult` 先 LPUSH + EXPIRE,再只 PUBLISH 一次 | `spec_B0-3_task_result_channel.md`(文末两批交付);PROGRESS「2026-10-18 B0-3 … 第 1 批」「… 第 2 批」 | 实例频道 `task:result:notify:<login 进程 UUID>`(`go/shared/taskresult/notify.go` 的 `InstanceNotifyChannel`,:49);回复目标为空或不合法时回落旧频道 `task:result:notify`(`LegacyNotifyChannel`,:19)。负责人 10-17 定先用 PUBLISH | Cluster 下 PUBLISH 经集群总线发到所有节点;到 Cluster 那批改 SPUBLISH / SSUBSCRIBE(容量模型 A0-5 的 2026-10-18 更正)。结果 LIST `task:result:<taskID>`(不带 hash tag)是单键,订阅方用集群客户端 LPOP 会路由到对的节点,不要求 LIST 与频道同节点。go/db 与 login 仍要同一拍改命令,做不到就沿用「login 两边都订、发布方只发一次」的过渡 |
| B0-4 nofile | gate / scene / battle 在 `Node::Initialize` 把 RLIMIT_NOFILE 软限抬到 min(硬限, 1048576),需求 = 准入上限 + 4096,不够缺省拒启;`NODE_OPEN_FILE_LIMIT_MODE=warn` 豁免(负责人 10-17 拍板) | `spec_B0-4_nofile.md`;PROGRESS「2026-10-18 B0-4:gate / scene / battle 启动时抬 RLIMIT_NOFILE 软限并做门槛检查」 | 不碰 Redis 键 | Cluster 下每个 C++ 进程要连本单元每个主节点,fd 需求多出「主节点数」一项;余量 4096(`kNonClientReserve`)要随单元规模复核 |
| B0-5 scene_manager 负载刷新 | 领导者按 zone 批量 pipeline 刷新,跟随者不发命令;两处全库 SCAN 换成登记集合 | `spec_B0-5_scene_manager_zone_scan.md`;PROGRESS「2026-10-16 scene_manager 负载刷新:领导者按 zone 批量刷新 + zone 登记集合替代全库 SCAN」 | 新键 `scene_manager:zones`(`go/scene_manager/internal/logic/zone_registry.go:54`,全局 SET,只加不减,merge_zone 摘除);全局 `scene_manager:leader:lock` 不变 | 登记集合与选主锁都是单键操作,Cluster 下合法;go-redis ClusterClient 会按节点自动拆非事务 pipeline,不必改代码;但 `pipelineBatchRejected`(`go/scene_manager/internal/logic/load_refresh.go`)的「整批」判定会遇到逐节点部分失败,要复核;场景路由族按 `{zone}` 打 tag(§20.3)。按 zone 选主与 watch 收窄仍归容量模型 A1 |

另外两项现状:
- **P0-E** 已落码,未验证:`redis.yaml` 显式 `MAXCLIENTS=40000`;`k8s_deploy.ps1`「Redis 连接总账」(`Get-RedisConnectionLedgerInputs` / `Get-RedisConnectionLedger`);改动核心 Pod 模板必须带 `-ConfirmRedisCoreRestart`(`Assert-RedisCoreRestartConfirmed`)。见 `docs/handoff/global-singletons-p0-findings-20261014.md`「## P0-E」、PROGRESS「2026-10-15 补充:全局单点 P0 部署侧四批落码与交接新账号」,以及 2026-10-16 的 P0-E 第 3 批各条。
- **redis-ha 已接入部署脚本**:由 `k8s_deploy.ps1 -RedisReplicas` 接入(origin `43284eb0b` 10-16 接入,本地 10-19 补齐;10-22 订正,原写「2026-10-19 接入」),可取 0 / 1 / 3,其中 0 表示按档位,非 dev 档按 3 部署 `redis-ha.yaml`。已落码,契约测试未跑,也没在 kind 复测,待 Codex 验证。详见 §2.5 第 6 条的 2026-10-19 更正与 `no-single-node-horizontal-scaling-20261001.md` §5.1。本稿 10-18 初稿写的「尚无部署路径」同此更正。

## 18. 控制面按 zone 组分实例(2026-10-18 新增,取代 §3.2.3)

本节是第 2 批的设计,**未落码**。

### 18.1 理由

- 负载正比于 DBTask:2000 万下控制面 6·DBTask 合计约 53 万~133 万 ops/s,含令牌发布脚本(7·DBTask)约 62 万~155 万(§16,[I]),单实例放不下。
- 键都带 topic,也就是带 zone [C]:
  - 游标 `consumer:applied:<topic>:<id>:<msgType>`(`go/db/internal/kafka/key_ordered_consumer.go` 的 `appliedSeqKey`,:603);
  - 已落库 epoch `consumer:applied_epoch:<topic>:<id>:<msgType>`(`appliedEpochKey`,:749);
  - 排序锁 `distributed:lock:kafka:ordering:<topic>:<id>:<msgType>`(`ordering_lease.go` 的 `orderingLockKey` :71-72,加 `locker.go:28` 的 `distributed:lock:` 前缀);
  - 重试 ready、按分区 ready、processing、登记表、模式 HASH、死信(`retry_routing.go` :83-98;`retry_ownership.go`);
  - 扩分区状态 `kafka:expand:status:<topic>`(`go/shared/kafkautil/expand_utils.go:105`)。
- DBTask 按 home_zone 选 topic:`player_lifecycle.cpp` 的 `SavePlayerToRedisImpl`(:4181 起)在 :4321-4356 先取 `PlayerHomeZoneComp`,再调 `GetDbTaskTopic(homeZoneId, generation)`;注释写明「选哪个 zone 的 topic 仍只看 home_zone」。所以一名玩家的控制面键正常只落在其 home zone 的 topic 下。
- 按 zone 组分实例:组内若干 zone 共用一个实例,负载随 zone 数线性摊开,组与组之间互不影响。

### 18.2 扩分区锁与 home_zone 未知的残余

- 扩分区锁 `distributed:lock:kafka:consumer:lock:<player_id>`(`tryLockAndProcess`,`key_ordered_consumer.go` 当前约 :2794)只含 player_id,不含 topic。
- expand 是死代码,但保留(负责人 2026-10-13,PROGRESS「2026-10-14 存盘管线接手」§5 拍板第 5 条)。生产代码只读状态,唯一的写入是 `processDBTask`(当前约 :2741 起)把过期的 expanding 改回 normal(约 :2765);只有状态为 expanding、或读状态出错时才拿这把锁。
- 互斥论证:同一玩家只在 home topic 上处理,竞争者都在同一组实例上,按组分实例不破坏互斥。
- **残余一:home_zone 未知时的回落**。2026-10-17 起分两条路径(`player_lifecycle.cpp` :4321-4345):
  - 严格路径(owner_epoch ≠ 0):home_zone 为 0 时计 `home_zone_unknown` 并**拒绝存盘**,不回落进程 zone(围栏文档 2026-10-17「收据身份与父快照投递恢复补充」);
  - epoch = 0 的旧兼容路径:仍回落为进程 zone 并计数,滚动升级窗口里每次存盘都可能走到。这时同一玩家的 DBTask 可能进非 home 的 topic,从而落到另一组实例。
- 今天正常态不取扩分区锁(只在 expanding 或读状态出错时取),排序锁、游标、epoch 都带 topic,跨 topic 本来就没有 Redis 侧互斥;回落路径只靠 MySQL 围栏,而围栏对 class-3(epoch=0)不生效(围栏 §5.6)。按组拆分不改变这一点。门禁保留,改为「`home_zone_unknown` 与 class-2 / class-3 计数在全部 zone 稳定为 0」(与围栏 §8.5 的 sql 进入条件同口径)。上线前没有旧 gate / 旧 scene 的滚动窗口,不阻塞。
  - 读数:class-2 / class-3 看 go/db 的 `db_fence_write_total{class="legacy_epoch"|"legacy_zero"}`(`go/db/internal/metrics/pipeline.go` 的 `fenceClasses`)。C++ 节点没有指标出口,`home_zone_unknown` 的读数是 `cpp/libs/services/scene/core/system/redis.cpp`(约 :129)每 30 s 一行的 `[OwnerEpoch] home_zone_unknown=N` WARN 汇总,只能按日志判读;要不要补指标进 §25。
- **残余二:合服跨组**。合服改了 home_zone 之后,同一玩家旧 topic 下的游标、锁与重试在源 zone 的组,新 topic 在目标 zone 的组。源与目标跨组时,merge_zone 的 R2(等排序锁消失)与 P4 检查要两边都查,或者规定合服只在组内进行(待拍板,§25)。

### 18.3 前置:条件写落地

- D1 §5.2「映射与控制面一起丢」不成立之后,只丢某个组实例时游标缺失,顺序守卫按缺键放行(瓶颈文档 §4 缺口表的 G5:游标与已落库 epoch 缺键放行;处置见 db-write-fencing-mysql.md §5.6)。正确性要由 MySQL 行内围栏兜底,但 G5 在 dual 下只对 class-1 关闭(围栏 §5.6),所以还要求 class-2 / class-3 计数为 0(围栏 §8.5)。
- 丢一个组实例,还会丢 offset 已提交的重试 ready / processing 原消息:offset 提交之后,重试队列里的那一份就是唯一副本(`key_ordered_consumer.go` 的 `persistBeforeAck`)。两张玩家表靠核心上的补投兜底(pending≠ACK 即补投),补投只在 dual 下运行;门禁要求补投在跑、积压告警正常。
- 前置改为:[db-write-fencing-mysql.md](db-write-fencing-mysql.md) 当前版本(v0.4 + 2026-10-17 实施附注)已上线;该组全部 zone 已切 `Fencing.Mode=dual`(条件写只在 dual 下、只对围栏表生效,legacy 下丢了组实例仍会静默放行);全部有写生产者的 DBTask 目标表都是围栏表。今天只有 `player_database`、`player_database_1` 有写生产者,go/db 启动时断言这两张是围栏表;另外 7 张注册表没有围栏列(围栏文档 §4.1)。
- 更正调研规格的「组数取 1 就退化为原方案」:只有那唯一一组就是 `redis-mapping` 时才是原方案;否则就是原稿 §13 第 2 条的「第四个实例」。
- ③ 消费组按 zone 的代码已入库,未验证(§5.5 的 2026-10-16 L-09 注与 2026-10-18 更正)。

### 18.4 配置

- zones 配置每个 zone 加组字段(例如 `redisDbCtlGroup`,名字落码时定),不填 = 不拆。
- 按用途拆的拓扑函数按 zone 返回 DbCtl(§4.2 的更正)。原稿给它取名 `Get-RedisTopologyFor`,但这个名字已被核心高可用形态占用,落码时换名(§4.3 的 2026-10-19 更正)。
- 实例名 `redis-dbctl-g<N>`,清单可复用 `redis-purpose.yaml` 模板。
- 迁移标记仍按 zone:`redis-split:migrated:dbctl:zone:<z>` 写在组实例上,墓碑 `redis-split:moved:dbctl:zone:<z>` 写在源上。
- merge_zone 的 `-dbctl-redis-*` 按 zone 解析到所属组,或规定合服只在组内(待拍板)。merge_zone P4 的 64 桶 ZCARD(`tools/merge_zone/preflight.go` 的 `assertSaveOutboxDrained`,:699)读的是补投桶,它们在核心,不在组实例。

### 18.5 组大小(推算 [I])

- 每 zone 控制面约 0.27 万~0.35 万 ops/s(含令牌发布脚本;只算 6·DBTask 时约 0.27 万~0.30 万;区间按 §16.1 配对,§16)。按每实例 5 万 ops/s 的保守参照,一组约 14~18 个 zone,Z = 200~445 时共约 11~32 组;按 10 万参照,一组约 28~37 个 zone,共约 6~16 组。
- 组大小只有在 V2 实测单主上限之后才能定,还要与单元划分对齐:一个组不跨单元(§20)。

### 18.6 运维面

- 清库(AGENTS.md §6.2):`k8s-redis-wipe` 遍历每个组实例,逐个 FLUSHALL 再核对 DBSIZE(§5.9 的更正)。漏清任一组,该组各 zone 的旧游标会把新写判成已落库。
- 回档:不再 FLUSHDB(10-17 拍板)。尚未落地的 `zone_rollback_discard` 在本 zone 所属组实例上,把本 topic 的重试收据与死信改名进 `rollback:quarantine:*`;在核心(及 redis-cache)上按玩家删父快照、子表缓存、sync_digest、证据、屏障,并 ZREM 补投索引;游标(`consumer:applied*`)与 owner_epoch 不动(`tools/scripts/k8s_zone_rollback.ps1` 头注与 purge 段)。strict 协议在裁决 #3 前入口拒绝。上线前没有可回档的 zone,不阻塞。
- 连接与 maxclients:每个组实例显式 `--maxclients`,`k8s_deploy.ps1`「Redis 连接总账」按实例各算一行(§4.3 的 P0-E 注与 2026-10-18 增补)。go/db 拆句柄之后,每个 go/db 副本多一个连接池。
- 监控与告警:每个组实例一份内存、OOM 拒绝、AOF 错误与连接数告警,label 只用实例名(§6.4)。
- 对瓶颈文档 3b(往返合并)的影响:锁、游标、epoch 在组实例,落点在 mapping,缓存与屏障在核心(或 redis-cache),写路径至少跨三个实例。3b 的「独立实例 11→4」是按两个实例算的(瓶颈文档 §6.3),**本稿沿用这个说法但没有核实,待 3b 按三实例重算**。

### 18.7 go/db 的 Redis 句柄承载的键全集,与 db-write-fencing 的硬依赖

**现状**(v0.4 + 2026-10-17 之后,主工作区代码 [C]):go/db 有三个句柄,K8s 上三者都是核心实例。

**验证口径**(本稿其他处写「验证口径见 §18.7」即指本段):状态以 db-write-fencing-mysql.md 文首与 PROGRESS 为准:10-17 Codex 完成定向 C++ 构建、受影响单测,以及隔离 zone 990017/990018 的真实邮件 / 交易到账、严格交接往返、屏障与补投收尾(PROGRESS「2026-10-17 附件/交易到账与 SQL 围栏双区闭环补验」);10-18 补了 DB 真实 Redis / MySQL 门控用例和一次增量整解(PROGRESS「2026-10-18 Codex：landing-fencing 续验补充，Redis 真测与 C++ 修复后增量整解通过」)。10-18 之后的增量(R1–R8、存盘协议世代门禁与缺省 dual、补投有界)未编译、未测试。全部只在单实例同 DB 下验证过,没有在拆分 / Cluster 形态下验证,也没有生产切换。

| 句柄 | 承载的键与脚本 | 出处 |
|---|---|---|
| `ServerConfig.RedisClient` | 控制面:排序锁、游标、已落库 epoch、扩分区状态与扩分区锁,重试 ready / 分区 ready / processing / 登记表 / 模式 HASH / 防重记录,死信 | §2.2 D 表;`key_ordered_consumer.go`、`ordering_lease.go`、`retry_*.go` |
| 同上 | 令牌发布脚本 `playerCachePublishLua`:一段 Lua 原子访问排序锁、子表缓存 `<MsgType>:<id>`、资产证据 `player:{%d}:asset_sql_commit`、`player:{%d}:save_barrier`(写 ACK 字段) | `player_cache_publish.go` :26、:75-127 |
| 同上 | 围栏 §5.5 推进脚本 `appliedAdvanceLua`:排序锁、游标、已落库 epoch | 同文件 :57、:129-147 |
| 同上 | 读路径缓存 `SET NX`;读结果 `task:result:<taskID>` 与通知频道 | `publishDBReadResult`(:523)、`publishTaskResult`(:572) |
| 同上 | 消费侧写 `save_barrier` 的终态 / 延后标记 | `save_outbox_note.go` :56、:73 |
| 同上(作为「父缓存实例」) | 补投工作器:读写 `save_barrier`、`player:save_pending:<zone>:<bucket>`;存盘协议世代 `db:save_protocol:zone:<z>`、`db:save_protocol:legacy_alive:zone:<z>` | `go/db/db.go:213` `saveoutbox.Start(..., ctx.RedisClient, ctx.PlacementRedis, ...)`;`saveoutbox/scripts.go`、`generation.go` |
| `ServerConfig.OwnerEpochRedis`(可选,缺省 = `RedisClient`) | 只读 `player:<id>:owner_epoch`:dual 下判 stale 时的计数器倒退探测 `ownerCounterRegressed` | `player_row_fence.go:201`;`key_ordered_consumer.go` :1221-1225 |
| `Placement.Redis` | `player:placement:<id>`、`player:zone:<id>`、`db:capability:zone:<z>`;补投工作器读 home_zone;`Placement.RequireRouteReady` 开时,选库与复核改走 `RouteReadyReadScript`,同一段 Lua 另读 `mapping:route:ready` 与本实例身份(INFO server / replication 的 run_id、role、master_replid) | `placement_route.go`;`route_ready_gate.go` 的 `lookupReady`;`saveoutbox/replay.go:270` |

**硬依赖**:
1. **令牌发布脚本**在一段 Lua 里原子访问排序锁(控制面)、子表缓存(缓存)、资产证据与 `save_barrier`。后两者与父键同属核心闭包:`save_barrier` 的 `pending` 由 C++ `SaveWithCommitBarrier` 与父键在同一段 Lua 里写。按 §18 把控制面拆到组实例、按 §19 把缓存拆到 redis-cache,或者上 Cluster 而这些键不在同一 slot,这段脚本都无法原子执行。db-write-fencing-mysql.md §6.4「与 G4a 按用途拆分的冲突」与 §14 第 19 条要求本稿登记这条依赖,即本节。
2. **go/db 的 `RedisClient` 今天必须就是核心实例**:它写 `save_barrier` ACK(login 冷加载、C++ 交接都在核心上读它),补投工作器也把它当父缓存实例。整体指到别的实例,ACK 写错实例,冷登录与交接全部 fail-closed。现有的闸只拦住一种误配:`saveoutbox.Start` 在 `OwnerEpochRedis` 与 `RedisClient` 不同实例时拒启(`go/db/internal/saveoutbox/service.go` 的 `Start`),`k8s_deploy.ps1` 拒绝显式配置 `OwnerEpochRedis`(约 :1919);只改 `RedisClient` 地址的误配今天拦不住。所以第 2 批必须先把 `RedisClient` 拆成「控制面」与「父缓存所在核心」两个句柄,不能只改地址。
3. 资产证据的读方 data_service 经 `Router.ClientForPlayer`(`go/data_service/internal/logic/asset_sql_commit.go:29`)读,必须与写方同实例(§2.2 E 表后的拆分约束)。
4. 回档 / PITR 要按 db-write-fencing-mysql.md §9.1(计数器倒退:定真源、抬计数器、不归零、不回灌)与 §9.2(删证据键、跳过位点、清重试)处理;本稿后续的回档 runbook 引用这两节(围栏 §14 第 19 条)。

**第 2、3 批的硬前置:二选一,进 §25 待拍板。**
- **A. 缓存围栏换成值封装**(围栏 §6.4:缓存值前加定长头,单键 `GETRANGE` 比较后写,不依赖排序锁)。缓存一支解除依赖;排序锁回到控制面单独校验。
- **B. 这几把键同实例 / 同 slot**:排序锁、子表缓存、资产证据、`save_barrier` 落在同一个实例,Cluster 下同一个 `{player_id}` slot。实际含义是:这些按玩家的控制面键与缓存跟着核心闭包走,进核心 Cluster 并改名带 tag;§18 的按组分实例只剩按 topic 的键(重试、死信、扩分区状态),§19 的独立 redis-cache 也不再成立。
- **(2026-10-18 新增)选 A 是否足够,要由围栏线裁决**。2026-10-17 把 `save_barrier` ACK 加进了令牌发布脚本;值封装只解开了缓存,资产证据与屏障 ACK 是否还要在同一原子步骤里核排序锁令牌,本稿不下结论。可供裁决的事实:两者都按 41 字节版本只升不降(`version_gt`),内容取自同一事务里锁定读到的 SQL 行(围栏文档 2026-10-17「统一接手落码附注」),看起来可以不依赖令牌;但「失租的旧实例在新主之后发布」这个时序是否安全,还没有人论证过。如果仍需令牌,A 也要把锁与证据 / 屏障放在同一 slot,A 与 B 的差别就只剩缓存。
- 无论选哪个,`appliedAdvanceLua` 的三把键(排序锁、游标、已落库 epoch)分片后都必须同 tag(围栏 §5.5 末条、§14 第 3 条);排序锁改名要与 `tools/merge_zone/relocate.go` :367-372 的锁键镜像同批改。

## 19. redis-cache 改为必做(2026-10-18 新增,取代 §3.2.4 的「可选」)

本节是第 3 批的设计,**未落码**。

- **为什么必做**:子表缓存推算约 0.6~1.2 TB(3·PCU·V,1 h TTL),写入约 8.9 万~22.2 万次/s、约 0.36~2.8 GB/s(§16,[I]),不能留在核心,单实例也放不下。
- **形态待拍板**(§25):
  - **Cluster**:键 `<MsgType>:<id>` 今天不带 tag。就缓存自身的 GET / SET 而言是单键操作,但**不能据此断言「Cluster 安全」**:写路径的缓存发布在令牌发布脚本里,与排序锁、证据、屏障是同一段 Lua(§18.7)。调研规格里「`PlayerDatabase*:<id>` 都是单键操作,不改名也是 Cluster 安全的」一句删去,改为受 §18.7 的硬依赖约束:选 A(值封装)之后缓存才是真正的单键(例外是两段工具脚本:`save_barrier_restore` 的 `restoreScript` 在父实例上 EXISTS 子表缓存,缓存拆走后恒为 0、前置被空过,即 fail-open,第 3 批前要加缓存句柄;`zone_rollback_discard` 契约要求每玩家一段 Lua 跨映射、核心、缓存同实例。详见 §2.3 结论注后的例外);选 B 则缓存跟核心 Cluster 走,不单独成 redis-cache。login / go/db 的缓存句柄换成集群客户端(go-redis `UniversalClient` / `ClusterClient`)。
  - **按 zone 组分实例**:读缓存的一方要先知道玩家的 home 组。
- **(2026-10-18 新增)子表缓存今天没有生产读者**。2026-10-17 起 login 父键缺失一律经 Kafka 读任务读 SQL 两行,不再用子表缓存拼父键(§2.2 E 表后的更正);`TriggerPlayerDataPreload` 没有调用方;剩下的读者只有 `cmd/verifier` 与 `cmd/debug_fetch`。所以「子表缓存还要不要写、redis-cache 还要不要单独部署」本身成了待拍板项,本稿不自行决定;决定之前仍按必做设计,容量按写入方算。
- **降内存的评估项**:
  - 「写任务只 DEL 不 SET」(容量模型 A2 第 4 条):与围栏的缓存语义冲突。v0.4 的缓存「只前进不后退」靠令牌脚本写入保证(围栏 §6.2、§6.5),改成只 DEL 要改这条保证,还会动证据与屏障所在的那段脚本,**需围栏线同意**。
  - 缩短 1 h TTL:没有生产读者时,代价只是工具多一次冷读,同样由围栏线确认。
- **task:result**:结果 LIST 与通知跟着 go/db 发布读结果所用的句柄走。读结果发布是单独的命令(`publishTaskResult`,§4.2 的更正),可以换到 redis-cache;到 Cluster 时频道改 SPUBLISH / SSUBSCRIBE(依赖 B0-3 已落码的分频道,§17)。go/db 与 login 仍要全服同一拍切换(§3.2.4 原约束)。
- **清单**:Cluster 形态参照 `redis-match-cluster.yaml`,不复用 `redis-purpose.yaml`;按 zone 组分实例则复用后者。
- **运维面**(同 §18.6):清库对每个主节点 FLUSHALL 并逐个核对(§5.9 的更正);每个主节点显式 `--maxclients` 并进连接总账(§4.3 的增补);按实例或按主节点配内存、OOM 告警(§6.4)。回档不再 FLUSHDB(10-17 拍板);`zone_rollback_discard` 契约按玩家删子表缓存(§18.6),而今天它要求每玩家一段 Lua 跨映射、核心、缓存同实例,缓存拆到 redis-cache 后契约要加缓存句柄、按新拓扑重写(§2.3 的例外、§25.2 第 17 条)。上线前没有可回档的 zone,不阻塞。

## 20. 核心路线:单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片(2026-10-18 新增;D12 / CZ-2 改写)

负责人 2026-10-17 拍板:核心走「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」,两者叠加;连带把 D12 / CZ-2 改写为「逻辑单一、物理按 `{player_id}` 分片」,C++ 要接集群客户端或支持 MOVED / ASK。本节是这条路线的设计材料,**第 4 批未落码**。

### 20.1 不变量改写

- 原文:D12(`cross-zone-matchmaking.md:103`)要求 SharedRedis 是全服单一实例;CZ-2(`cross-zone-scene-travel.md` §3 决策表 CZ-2 行;原写 :31,10-22 订正)要求 `PlayerAllData` 所在的 Redis 物理共享。
- 改写为(两份契约文档的同步由各自的维护线做):
  1. **同一玩家的契约键只有一个权威存储**:所在单元的核心 Cluster 里、该玩家 `{player_id}` 所在的 slot。单元内任何 zone 都能寻址到它(全局单点交接 §5.1 第 1 条;「逻辑单一」的作用域是全服还是单元,见 §25.2 第 20 条)。
  2. **逻辑单一,物理分片**:同一玩家的多键原子操作全部落在 `{player_id}` 一个 slot;跨玩家、跨场景的原子操作要拆开(§20.4)。
  3. **一名玩家同一时刻只属于一个单元**。跨单元按「转区」处理(停写、排空、迁移),不做实时跨单元 CAS(容量模型 §3.0)。转区协议仓库里没有,另行设计;它改变跨单元旅行的体验,属于客户端可见行为(§26)(待拍板,§25.2 第 20 条)。
- 第 4 批落码之前,两份契约照旧有效。

### 20.2 每单元内存与 ops 预算(推算 [I],未实测)

单元规模未定(§25)。下面按每单元 PCU_c 给公式,并假设玩家按在线份额均匀分到各单元:
- 核心 ops ≈ (0.0596~0.0632)×PCU_c:§16 的核心合计除以 PCU,即每个在线玩家约 0.06 次/s;
- friend 登录 MGET 的键 ≈ PCU_c/1800×F(F = 50~200);Cluster 下按 slot 拆开,命令数趋近键数;
- 令牌发布脚本 ≈ 2·N_save,c = DBTask_c = (0.0044~0.0111)×PCU_c,只有 §18.7 选 B 时才落在核心;选 B 时核心还另加按玩家的控制面 5·DBTask_c(取锁、游标 GET、epoch GET、推进脚本、释锁;扩分区状态 GET 按 topic 留在组实例)与子表缓存常驻 ≈ 3·PCU_c·V(1 h TTL);
- 在线热集 ≈ PCU_c×V;常驻 ≈ P_cum×(PCU_c/PCU)×(V + 0.3 KB),另加屏障、证据、账号目录(落库前)的份额。

| 每单元 PCU | 单元数 | 核心 ops | friend MGET 键 | 选 B 时核心另加 | 在线热集 | 常驻(P_cum ≥ 1 亿 / 30 天 2~3 亿) | 主节点数(ops ÷ 5 万与内存 ÷ 25 GB 取大,不含从库) |
|---|---|---|---|---|---|---|---|
| 50 万 | 40 | 3.0 万~3.2 万 | 1.4 万~5.6 万 | 令牌脚本 0.22 万~0.56 万;控制面 5·DBTask_c 1.1 万~2.8 万;子表缓存 15~32 GB | 5~10.5 GB | 26~53 GB / 52~160 GB | 不冷卸载 2~7;冷卸载后按 ops 1~2 |
| 100 万 | 20 | 6.0 万~6.3 万 | 2.8 万~11.1 万 | 令牌脚本 0.44 万~1.1 万;控制面 5·DBTask_c 2.2 万~5.6 万;子表缓存 30~63 GB | 10~21 GB | 52~107 GB / 103~320 GB | 不冷卸载 3~13;冷卸载后 2~4 |
| 250 万 | 8 | 14.9 万~15.8 万 | 6.9 万~27.8 万 | 令牌脚本 1.1 万~2.8 万;控制面 5·DBTask_c 5.6 万~13.9 万;子表缓存 75~158 GB | 25~53 GB | 129~266 GB / 258~800 GB | 不冷卸载 6~32;冷卸载后 5~9 |

(上下沿都已计入 friend MGET 按 slot 拆开后的命令数;选 B 时主节点数按加总重算;主节点数未计屏障 / owner_epoch 保留份额与落库前的账号目录,内存一项偏低。)

**与其他口径的差异**:
- 容量模型 A6 / §3.0「每单元不超过 50 万 PCU 时,拆分后核心约 3.3 万~4.1 万 ops/s」:那是 S1 拆分后核心(6.5 万~8.2 万)减半。差别正好两项:A6 把子表缓存 SET(第 3 批不做时在核心)与 B0-5 之前的 SM 刷新(0.8·Z·N_s)算在核心里;本节按 2000 万口径,缓存移出核心(§19),SM 刷新已改为线性。把这两项加回 50 万单元(子表缓存 SET 2·N_save,c ≈ 0.22 万~0.56 万;S1 的 SM 刷新减半约 0.04 万~0.41 万),就回到约 3.2 万~4.1 万。两者都不含 friend MGET。
- 全局单点交接 §5.1 第 3 条「100 万在线的 cell 约 1.85e5 ops/s、约 0.5 TB」:它按 1000 万口径,一个实例承载全部(每在线约 0.17 条命令,其中约 60% 是 go/db 写路径每任务 11.5 条控制面命令);常驻是上沿 4.9 TB(累计约 3 亿账号)的 1/10。本节 100 万单元只算核心(控制面在组实例、映射在 mapping、缓存在 redis-cache),所以是 6.0 万~6.3 万;把控制面(7·DBTask_c ≈ 3.1 万~7.8 万)、映射读(≈ 1.0 万~2.3 万)与缓存发布加回去,才回到 10 万以上的量级。内存方面,2000 万口径下一个 100 万单元只占 1/20,所以是 0.05~0.32 TB。两份结论一致之处在于:即使单元化,100 万的单元也超出一个主节点,单元内必须分片,这正是「叠加」的理由。

### 20.3 键族的 hash tag 归属

Cluster 只按键里第一对花括号内的子串算 slot。今天同一段 Lua 里已经混用了两种形状,上 Cluster 会报 CROSSSLOT:
- C++ `kSaveWithCommitBarrierLuaScript`:`PlayerAllData:<id>`(无 tag)、`player:<id>:owner_epoch`(无 tag)、`player:{<id>}:save_barrier`(有 tag)、`player:save_pending:<zone>:<bucket>`(按 zone 分桶);
- go/db `playerCachePublishLua`:`distributed:lock:kafka:ordering:<topic>:<id>:<msgType>`(无 tag)、`<MsgType>:<id>`(无 tag)、`player:{<id>}:asset_sql_commit`、`player:{<id>}:save_barrier`(有 tag);
- login `parentBarrierKeys` 与 C++ `BeginTravelHandoff`:父键、旧格式 owner_epoch、handoff(无 tag)与 `save_barrier`(有 tag)。

(围栏文档 2026-10-17 附注同样写明「现有 owner / 父键与新屏障未同槽,只支持同一 Redis 实例」。)

| tag | 键族(今天的键名,`<id>` 为十进制 player_id) | 备注 |
|---|---|---|
| `{pid}` | 父键 `PlayerAllData:<id>`;`player:<id>:owner_epoch` / `:handoff` / `:location` / `:login_generation` / `:sync_digest` / `:channel_switch_cooldown` / `:leave_release_wait`;`player:{<id>}:save_barrier`、`player:{<id>}:asset_sql_commit`(已带 tag);`player:session:<id>`、`player:location:<id>`(旧);`player_locker:<id>`;`battle:lock:<id>`、`battle:ctx:<id>`、`battle:settlement:pending:<id>`、`battle:settlement:pending:id:<id>`;`team:player:<pid>`、`team:invite:<pid>`;`enter_scene:dedup:<id>:<req>`;`player_merge_notice:<id>`、`player_force_rename:<id>`、`player_to_account:<id>`;新号种子(父键 + owner_epoch,`new_player_seed.go:74`);data_service 的 `player:{%d}:*`(`data_logic.go:19`,已带 tag) | C++ 与 Go 的键格式逐字镜像(`go/shared/scenenode/locator.go` :15-37 列了 location 键的 7 处实现;`ownerepoch.go` :49-55),改名要全部同批 |
| `{pid}`(§18.7 选 B 时) | 子表缓存 `<MsgType>:<id>`;go/db 每玩家的控制面键:排序锁、游标、已落库 epoch | 选 A 时缓存是单键,控制面三键在组实例内同 tag 即可 |
| `{account}` | `account:<account>`、`account_lock:create:<account>`、`account_lock:login:<account>`、`login_account_sessions:<account>`、`account_refresh:<account>` | 落库后 Redis 只是缓存(§22) |
| `{account}` 或另定(2026-10-18 补) | login 会话栅栏 `login:sessfence:<gate uuid>:<incarnation>`、`login_session:<gate uuid>:<incarnation>`(旧版 gate 为 `login_session:<session_id>`) | 两族按 gate 连接取键,却与按账号取键的设备集 `login_account_sessions:<a>` 同在 `commitLoginScript` 里(§20.4);归 `{account}` 要把账号写进这两族的键名,也可以另定 tag |
| `{team}` | `team:rec:<tid>`、`team:<tid>` | 只让脚本合法,不分散负载 |
| `{zone}` | 场景路由族:`scene:<id>:*`、`instances:zone:<z>:active`、`instance:<id>:player_count`、`node:zone:<z>:<node>:*`、`scene_nodes:zone:<z>:load`、`world_channels:*`、`world_channel_directory:zone:<z>:<conf>`、`world_init:lock:zone:<z>`;排队族(随 redis-queue 的形态,§24) | 只含 scene_id 的键,打 tag 时要把 zone 写进键名 |
| 不打 tag 的全局键 | `scene_manager:leader:lock`、`scene_manager:zones`、`player_locator:session_reconciler:leader`、`player:gate_index:audit_cooldown`、`trade:assetop:maintenance:leader`、`db:save_protocol:*`;mapping 上的 `mapping:route:*` | 单键操作合法,但不能出现在按玩家的多键脚本里 |

`player:gate_index:ready` / `:gen` / `:last_audit` 三键不能当作互不相干的全局键:它们被 `markLegacyWritersScript`(gen + ready)、`setGateIndexReadyIfGenScript`(gen + ready + last_audit)同脚本访问(`go/player_locator/internal/logic/gate_session_index.go`),要共用一个 tag(例如 `{gidx}`)。

### 20.4 必须拆开或改形的跨 tag 脚本(2026-10-18 核实)

| 脚本 | 位置 | 跨了什么 | 改形方向 |
|---|---|---|---|
| player_locator 9 段会话 Lua | `go/player_locator/internal/logic/session_cas.go`(:39~:226 的 9 个 `redis.NewScript`) | `player:session:<id>` + 全局 `player:leases*`(+ B0-1 的 gate 索引) | `player:leases*` 与 gate 索引同批按 slot 分桶(B0-1 设计 §11),保持 CAS 与索引同槽原子。对账的 prune / repair / release 三段守卫脚本(`gate_session_index.go` 的 `pruneGateIndexMemberScript`、`repairGateIndexMemberScript`、`releaseGateIndexIfEmptyScript`)跨会话键、索引键与登记集,对账 MGET 索引成员的会话(`mgetSessions`),都随分桶同槽。会话 Lua 用 `""` 作 KEYS 哨兵(`session_cas.go` 文件头注第 3 条),Cluster 下 `""` 落 slot 0 会 CROSSSLOT,哨兵要挪进 ARGV |
| C++ 父快照提交屏障 | `redis_client.h:66`(`kSaveWithCommitBarrierLuaScript`) | `{pid}` 三键 + `player:save_pending:<zone>:<bucket>` | **待拍板(2026-10-18 新增)**:补投桶怎么改形。可选:照 B0-1 按 slot 分桶(桶键带 `{<slottag>}`,补投工作器与 merge_zone P4 按 slot 桶遍历);或把桶移出脚本,改为尽力而为的索引加全量兜底(要重新论证「意图不丢」) |
| go/db 补投 `claimScript` / `migrateScript` | `go/db/internal/saveoutbox/scripts.go`(:22、:104) | `save_barrier` + 桶 | 随上一项 |
| go/db 令牌发布脚本 | `player_cache_publish.go:26` | 控制面 + 缓存 + `{pid}` 证据 / 屏障 | §18.7 二选一 |
| 战斗结算写入 / 销账 | `cpp/libs/services/scene/battle/system/player_battle.cpp` :309、:519、:2144;`cpp/nodes/battle/logic/battle_room_manager.cpp` :303、:315(孤儿清理);battle 节点 `kSetPendingSettlementScript` 的调用点(`battle_room_manager.cpp` 约 :1494、:2106,脚本定义在 `settlement_outbox.h`) | `{pid}` 键 + 全局 `battle:settlement:pending:index` | 索引是只用于报数的可丢键:移出脚本改成单独的尽力而为命令,或按 slot 分桶 |
| scene_manager 离场 | `go/scene_manager/internal/logic/leavescenelogic.go`(KEYS 注释 :22-25) | `{pid}` 的 location / owner_epoch / handoff + `{zone}` 的 instance / node 计数 | 拆成「玩家 CAS + 计数幂等递减」,偏向多计,靠上报对账回收(全局单点交接 §4.2) |
| 组队 S_COMMIT | `go/match/internal/team/keys.go` :40-55、`scripts.go` | `{team}` + 两名以上玩家的 `team:player` / `team:invite` | 统一 `{team}` 只能让 team 键合法;`team:player:<pid>` 按 pid 查找,要拆成两阶段,或改为以 team 为主键的索引 |
| login 外观修复 / 入场自愈 `accountRecordCASScript` | `go/login/internal/logic/clientplayerlogin/player_class_backfill.go:174` | `account:<a>` + `player_locker:<pid>` + `account_lock:create:<a>` | 拆开:先在 `{pid}` 上核玩家锁,再在 `{account}` 上 CAS;两步之间的竞态要重新论证 |
| login 身份补齐 `backfillPlayerClassScript` | 同文件 :185 | 父键 + `player_locker` + `player:session` + location | 全部 `{pid}`,改名后合法 |
| login 入场自愈 `healAccountRoleScript`、admin 摘孤儿 `removeOrphansScript` | `clientplayerlogin/account_self_heal.go`(约 :120);`go/login/internal/logic/admin/remove_players_from_accounts.go`(约 :205) | 前者 `account:<a>` + `player_locker:<pid>` + `account_lock:create:<a>` + `player_to_account:<pid>`;后者 `account:<a>` + 账号锁 + 多名玩家的 `player_to_account:<pid>` | 跨 `{account}` 与 `{pid}`,做法同外观修复:拆开并重新论证竞态,或把反向索引并进 `{account}` |
| login 登录提交 `commitLoginScript` | `go/login/internal/logic/pkg/sessionfence/fence.go`(约 :103) | `login:sessfence:<gate>:<inc>` + 设备集 `login_account_sessions:<a>` + 一个或多个 `login_session:*` | 两族按 gate 连接取键、设备集按账号取键,tag 归属见 §20.3 |
| data_service W1 与「读 home 带围栏」 | `go/data_service/internal/routing/router.go` 的 `setPlayerZoneIfAbsentScript`(:286)、`getHomeZoneAndMergeFenceScript`(:387) | `player:zone:<id>` + `merge:in_progress:<zone>`(读 home 的脚本在脚本内拼围栏键) | 围栏键不能在脚本里拼;围栏复核挪进库(D1 db_first),或按 slot 复制广播围栏(§21) |
| D1 发布脚本 | `go/shared/placement/route.go` 的 `PublishRouteScript`(:316,KEYS 注释 :317) | 三族映射键 + `merge:in_progress:<home>` | 同上 |
| D1 读方就绪脚本 | `go/shared/placement/route_ready.go`(约 :186,`RouteReadyReadScript`)。已接入:go/db `route_ready_gate.go`(`RequireRouteReady` 开时 lookup 与复核两步都走,每条写任务多 4 次 INFO)、data_service `route_read.go`(批量就绪读;`getHomeZoneFenceReadyScript` 用 ARGV 前缀在脚本内拼围栏键)、guild `merge_fence.go` | 全局 `mapping:route:ready` + 映射键 + 本实例身份 | 就绪标记与身份核对要按主节点各一份(§21);`getHomeZoneFenceReadyScript` 的脚本内拼键同 W1 的处理 |
| merge_zone 落点 CAS | `tools/merge_zone/placement_ops.go`(KEYS 注释 :496) | placement + zone + 可选围栏 | 同 W1 |
| scene_manager 场景生命周期 | `go/scene_manager/internal/logic/scene_atomic.go` | `scene:*` + `instances:*` + `node:*` | 统一 `{zone}` |

### 20.5 全局键、SCAN 依赖与跨玩家多键操作的改造表

Cluster 下:跨 slot 的 MGET / MULTI 报 CROSSSLOT;SCAN 只扫当前连接的那个节点。行号以 2026-10-18 主工作区为准。

本表是示例清单,非穷举;第 4 批立项前按 `Scan(ctx|SScan|MGet(|TxPipeline|NewScript` 重新 grep。SCAN 必须逐主节点扫完,扫不全就 fail-closed。

| 类别 | 位置(函数,当前行号) | 键 | Cluster 下的改法 |
|---|---|---|---|
| 跨玩家 MGET | friend `SessionReader.readChunk`(`go/friend/internal/data/session_reader.go:168`) | `player:session:<id>` × F | 按 slot 分组后 pipeline;命令数趋近键数,计入核心预算 |
| 同上 | friend `OnlineDirectory.FillFriendProfiles`(`friend_profiles.go:37`) | 好友的父键 `PlayerAllData:<id>`,每批 64 个 | 同上;冷卸载后父键可能不在(§23) |
| 同上 | friend `OnlineDirectory.readEntries`(`online_directory.go:178`) | SCAN 扫到的 `player:session:*` | 同上 |
| 同上 | guild `OnlineStatusResolver.BatchResolve` / `mgetWithin`(`go/guild/internal/logic/online_status_resolver.go` :80、:221);`loadGateInfos`(`push.go:174`) | `player:session:<id>` | 同上 |
| 同上 | login `fillMissingRoleAppearances`(`role_appearance_backfill.go:36`) | 账号下各角色的键 | 同上 |
| 同上 | login `DrainingGates`(`loginqueue/gatedrain.go:77`);scene_manager 跨 zone 重定向 `drainingGateIDs`(`gate_redirect.go`,2026-10-18 起,见 §2.2 A 表) | `gate:<id>:draining` | 排队族,随 redis-queue 的形态。重定向读的是目标 zone 的 gate,与 login 的读法一样,同 zone 的 gate 键打 `{zone}` tag 后就能一次 MGET;scene_manager 要与 login 同批指向排队实例 |
| 同上 | match `presenceReader.mget`(`go/match/internal/team/presence.go:116`) | 队员的会话 / 父键 | 按 slot 分组 |
| 同上 | data_service `Router.BatchGetPlayerHomeZone`(`router.go:463`)、`Router.MGetPlayerNames`(`player_name_cache.go:87`)、`Router.registerPlayerZoneDBFirst`(`route_publish.go:147`) | 映射三族、`player:name:<id>` | 三族同 `{pid}` 后,单玩家的 MGET 合法;跨玩家的按 slot 分组(§21) |
| 同上 | data_service `mget`(`data_logic.go:364`);routetool `redisread.go` :123、:256、:384;merge_zone `readPlacements`(`placement_ops.go:52`);data_consistency_check `redisRouteSource.LiveZones`(`route_source.go:128`) | 映射与玩家数据 | 按 slot 分组 |
| 同上(2026-10-18 补) | merge_zone `readPlayerZones`(`merge_run.go` 约 :851)、`restoreMappingForIDs`(`unmerge.go` 约 :433);`placement_ops.go` 另两处:`readCapabilityMarkers`(约 :435)、`collectStoragePlayers`(约 :877) | 映射三族;`db:capability:zone:<z>`(按 zone 的全局键) | 映射按 slot 分组;能力标记跨 zone 的 MGET 改逐键 GET 或打同一 tag |
| 跨键 MULTI | login `Manager.Issue`(`go/login/internal/logic/pkg/token/token.go:104` 的 `TxPipeline`)、`trimRefreshSet`(:268) | `access_token:*`、`refresh_token:*`、`account_refresh:<a>` | 令牌键名带上 `{account}`,或拆成可重放的独立命令;改令牌键名会让旧令牌失效,要设计过渡 |
| 同上 | login `Queue.Enqueue`(`loginqueue/queue.go:324`) | `queue:zone:<z>`、`queue:meta:<qid>`、`queue:token:<token>` | 三键都带 `{z}`,或 redis-queue 按 zone 组分实例 |
| SCAN | friend `OnlineDirectory.List`(`online_directory.go:86`,SCAN 在 :118) | `player:session:*` | 逐主节点扫;**游标格式要变**(见下) |
| 同上 | data_service `Router.GetAllPlayerIDsInZone`(`router.go:545`);routetool importer(`importer.go:199`);data_consistency_check(`route_source.go:112`) | `player:zone:*`、`player:placement:*` | 逐主节点扫,或改从 `player_route` 读 |
| 同上 | player_locator 补建 / 审计(`session_reconciler.go:1092`) | `player:session:*` | 逐主节点扫,run_id 核对改为每节点 |
| 同上 | match `migrateLegacyQueues`(`go/match/internal/logic/matcher.go:156`) | 旧队列键 | 一次性迁移代码,上 Cluster 前删除 |
| 同上(2026-10-18 补) | merge_zone `scanLocationKeys` 与 `clearSourceZoneHotState`(`scene_hot_state.go` 约 :614、:418、:472) | `player:*:location`、源 zone 场景热态 | 他区在线判定靠扫全 `location`:只扫一个节点就是 fail-open,必须逐主节点扫完 |
| 同上(2026-10-18 补) | go/db `scanRetryPartitionKeys`(`go/db/internal/kafka/retry_routing.go` 约 :924);merge_zone `scanAllKeys`(`preflight.go` 约 :780,按分区 ready 列表 `kafka:retry:queue:<topic>:p*`) | 本 topic 的按分区重试 ready 列表 | 按 topic 的键在 dbctl 组实例;组实例若上 Cluster,逐主节点扫或给本 topic 的重试族打 `{topic}` tag |
| 同上(2026-10-18 补) | guild `GuildRepo.RebuildRanks` 与 `allZoneRankKeys`(`go/guild/internal/data/guild_repo.go` 约 :678、:781);data_consistency_check `checkGuildRankZoneOrphans`(`tools/data_consistency_check/main.go` 约 :384) | `guild_rank:zone:*` | 随 guild 迁出 DB 2 一起改,逐主节点扫 |
| 同上(2026-10-18 补) | merge_zone `verifySourceHotStateGone`(`audit_checks.go` 约 :524,源 zone 的 `world_*`、`node:zone:<z>:*` 与负载键,模式见 `scene_hot_state.go` 的 `sourceZoneScanPatterns`);`collectPlayerIDsWithHomeZone`(`player_blob_migrate.go` 约 :37,`player:zone:*`) | 场景热态、`player:zone:*` | 逐主节点扫,或映射改从 `player_route` 读。同文件的 `scanPlayerKeys` 已按 `ForEachMaster` 逐主节点扫,是现成写法 |
| PUBLISH(2026-10-18 补) | C++ `PlayerLandingSystem::PublishAssetWake`(`player_landing.cpp` 约 :268,频道 `assetop:wake`) | 频道 | Cluster 下 PUBLISH 经集群总线广播到所有节点;订阅方(guild / trade)用集群客户端订阅即可,量大时改 SPUBLISH |
| SELECT DB | guild `RedisClient` 用 DB 2(`go/guild/etc/guild.yaml:25`) | `guild:v2:*`、`guild_rank:*` | Cluster 只有 DB 0,要迁出 |

- **friend 在线目录的游标**:`RecommendFriendsResponse.next_cursor`(`proto/friend/friend.proto` 约 :219)把服务端的 `v1:<scan>:<offset>` 原样交给客户端(`online_directory.go` 的 `parseDirectoryCursor` :46、`encodeDirectoryCursor` :68,长度上限 48 字节)。proto 写明是不透明分页游标(`friend.proto`),字段与类型不变;风险限于滚动升级时在途的 v1 游标会被拒(`ErrInvalidDirectoryInput`),以及服务端 48 字节上限。服务端兼容读 v1、按重新开始处理即可;上线前没有在途游标。客户端是否解析或限长未核实(未读客户端仓),**仍列为客户端与 Java 同批评审项**;Java 只需语义一致,不要求同格式(AGENTS.md §12,见 §26)。

### 20.6 C++ 前置

- **集群客户端**:今天是 muduo hiredis 单连接(`redis_manager.h`),不处理 MOVED / ASK,地址只在启动时解析一次(`node.cpp` 的 `tlsRedis.Connect`,:710)。候选是 hiredis-cluster 或 redis-plus-plus(容量模型 A6)。新依赖要按 AGENTS.md §11.2 写理由,初步理由是:现有依赖做不到 slot 路由与重定向,自写一套 slot 表与 MOVED / ASK 处理的维护成本与风险更高。这只是待评审的理由,不是结论。
- **MOVED / ASK 与地址重解析**:slot 表刷新,主从切换后重新解析 DNS;AUTH 要对每条节点连接做(P0-A 的 AUTH 已在 `RedisManager` 的新连接上做)。
- **连接数**:每个 C++ 进程连本单元每个主节点,连接数 = 进程数 × 主节点数,要进 maxclients 总账与 nofile 需求(§17 的 B0-4 行)。
- **脚本只访问 KEYS[] 声明的键,且全部 KEYS 同 slot**:B0-2 已对两段存盘脚本守住。其余 C++ 脚本要同样核对:`exit_release_mark.h` :180 / :260 / :281、`handoff_mark_withdraw.h:54`、`handoff_sql_barrier.h:30`、`player_lifecycle.h:763`、`relocate_confirm.h:148`、`redis_client.h:66`,以及 `player_battle.cpp` 的结算脚本。
- **依赖同连接 FIFO 的完整清单**:2026-10-18 在 `cpp/libs/services`、`cpp/libs/engine`、`cpp/nodes` 下重新 grep「同连接」「同一条连接」「FIFO」「PING」得到。Cluster 下不同 slot 的命令走不同节点的连接,MOVED / ASK 重发还会打乱顺序,下表每一处都要改成「脚本内原子」,或显式确认正确性不依赖顺序。
- **(2026-10-23 补充,第二轮审计 r2-lc#1 / r2-lc#2)同连接 FIFO 在今天的单连接部署里也会跨连接失效**:存活探测(`redis_manager.cpp` 的 `DropUnresponsive`,PING 15s 无应答)摘下旧连接、`redisAsyncFree` 正常 close 之后,内核仍在重传旧连接上未确认的命令,它们可能晚于重连后新连接上的命令执行;经四层代理 / Sentinel 也一样。所以「同一条连接」只能保证一条连接内的先后,任何「旧写不得晚于新持有者」的要求都必须在 Redis 端用围栏令牌原子判定。handoff 标记的全部写入口已改用 `save_barrier.handoff_fence`(cross-zone-scene-travel.md 文末两节),下表相应各行已更正(会话结束凭证是"先 HGET 原件、scene 线程重核仍不持有、再以原件 CAS",读与写之间、写与之后的登记之间的顺序都不影响正确性,location-release-binding.md §3.3);表中其余「依赖」项(战斗锁、A2′ 与 GET 的先后等)在摘连接重连时同样要按这条重新核对。

| 位置(当前行号) | 依赖什么 | 现状 |
|---|---|---|
| `player_battle.cpp` :397-409 | 结算存盘之后紧跟一条 PING,PING 回来就当场判落盘、销账放锁 | 只是快路径:判落盘按 `IsSettlementDurable` 比字节,失败由发件箱 10 s 重投、reaper、登录补销账兜底。Cluster 下 PING 可能落到别的节点,快路径命中率下降;正确性按注释不受影响,待核实 |
| `player_battle.cpp` :1996、:2107、:2254 | 销账 / 续锁命令要排在随后的读锁之前;读命令排在条件删锁之后 | 依赖 FIFO。同一玩家的键同 `{pid}` slot 时仍在同一节点连接上,但 MOVED / ASK 重发会打乱,要改为脚本内原子或回调链串行 |
| `cpp/libs/services/scene/core/system/redis.cpp` :35-51 | 重连后先补发 handoff 撤回与 A2′,再重发玩家加载;改派核实读与随后的凭证补写「必须走这条不重放的连接」 | 撤回与 A2′ 的注释写明正确性不靠这条顺序(靠 `IsSceneChangeBusy` 与 A2′ 应答闸);改派核实读一项依赖。*2026-10-23 起改派核实读一项也不依赖(见下方 :1751-1757 行)* |
| `player_inbox_system.cpp` :454-483 | 收件箱压写后,同连接紧跟一条 PING 走快路径 | 快路径,tick 每秒复查兜底 |
| `player_lifecycle.cpp` :1044-1046(`SendConditionalMarkWrite`) | 条件写 handoff 标记必然排在退出存盘落地之后 | 依赖。*2026-10-23 更正(r2-lc#2):"排在存盘之后"是回调链因果(落地回调之后才发),不靠 FIFO;防"晚于下一任持有者"改由 `handoff_fence` 原 H→F 的 CAS,不依赖顺序* |
| `player_lifecycle.cpp` :1283 | 放弃补写排在任何更后一轮的 A2′ 之前 | 依赖;注释称两者都是 owner_epoch 条件写、取哪个都只在归属未变时生效,待复核。*2026-10-23 复核:只比 owner_epoch 不够(同节点重载同 E),已改为只消费被放弃那一轮的原 H,更后一轮登记之后迟到即拒,不再依赖顺序* |
| `player_lifecycle.cpp` :1751-1757(核实读) | 核实读与随后的凭证补写必须走不重放的同一 FIFO 连接 | 依赖。*2026-10-23 更正:补写在 Lua 里原子复核 owner_epoch 与条目缓存的原 H,核实读与补写之间的顺序不再影响正确性* |
| `player_lifecycle.cpp` :2606-2610;`cpp/nodes/scene/handler/rpc/scene_handler.cpp:145` | A2′ 的 EVAL 必须排在随后的 AsyncLoad GET 之前 | 依赖 |
| `player_lifecycle.cpp` :5704-5706;`player_lifecycle.h` :740-746 | 交接取证原为「同一不重放连接上先 DEL 后 MGET」 | **已被 Lua 取代**(`kLuaJudgeTravelOutcome`);注释要求以后若拆回两条命令,必须走同一连接 |
| `player_lifecycle.h` :1294-1299 | 旧一代 SET 的 ERROR 应答先于新实体的 A2′ 应答到达 | 注释写明这「不是可以依赖的契约」,已改由代际比较 |
| `cpp/nodes/battle/logic/battle_room_manager.cpp` :264-270 | 孤儿清理三步发在同一连接,第 3 步的回调晚于全部清理回调 | 依赖;只影响计数是否准确,待核实 |
| `SaveWithCommitBarrier` 与提交钩子(`player_lifecycle.cpp` :4443 一带;围栏 10-17「C++ 父快照提交屏障与 SQL 任务准入」) | 每份实际写入 Redis 的快照,回调之后才投递 SQL 任务、再发下一份 | 依赖 `redis_client.h` 按键的在途合并与回调顺序,不直接依赖连接 FIFO;换集群客户端时要保住「同一玩家同一时刻只有一份在途」 |

(`cpp/nodes/scene/id_segment_bootstrap.cpp` 里的「FIFO」说的是 gRPC 响应归属,与 Redis 无关。)

### 20.7 迁移:核心闭包的键改名不做双读

- **首次上线前完成改名的,直接以新键名起步,没有存量与混跑窗口,本节的迁移方式只适用于上线后**(项目尚未上线,见 AGENTS.md 上线状态;§25.2 第 22 条)。
- 调研规格写的「核心闭包走双读窗口加回滚」**作废**。owner_epoch 守卫存盘、location / handoff 的 CAS、scene_manager 的 INCR 铸造、`save_barrier` 的 pending / ACK、会话 CAS、战斗锁都是围栏原语。滚动期间旧二进制按旧键名 CAS、新二进制按新键名 CAS,两边各自都会「成功」,围栏被绕过(双持有者、回档)。本稿 §5.1 也以同样的理由否决了对 CAS 键双写、对映射双读。
- 改为二选一(待拍板,§25):
  1. **停写切换**:全部写者(scene、battle、scene_manager、player_locator、login、go/db、merge_zone 等)停下或排空,一次改名(或从旧实例按新键名拷到新集群),再放流;
  2. **版本门禁**:版本门禁只保证翻转时全部写者都认识新键名,翻转本身仍要停写排空;或逐玩家迁移:先在旧键写一个让旧名 CAS 必然失败的墓碑,再写新键(无守卫 SET、SET NX 两类路径要逐一证明会被挡住)。`db:save_protocol` 只是近似先例:legacy 只在启动时与每 5 s 复查(`generation.go` 的 `LegacyWatchInterval`),有窗口。
- 回滚同样整体切回,不留新旧键名并存的窗口。
- Go 与 C++ 的键格式镜像要同批改(`scenenode/locator.go` 列的 7 处、`ownerepoch.go`、C++ `player_ownership` 的键函数、merge_zone 的镜像常量等)。契约测试用 golden 向量对拍,并对每段多键 Lua 计算 CRC16 slot,断言同槽。
  - (2026-10-18 补)Lua 体内的键形校验:C++ `kSaveWithCommitBarrierLuaScript` 逐字比对 owner / 屏障键名,要求父键以 `:<id>` 结尾,桶键按正则校验且桶号 = id%64;父键改成 `PlayerAllData:{id}` 后结尾校验必然失败。手拼桶键还有三处:merge_zone `preflight.go`(`assertSaveOutboxDrained`)、barrierrestore `restore.go`(`recoveryKeys`)、data_service `debugutil/bundle.go`。
  - (2026-10-19 补)**新键名不能落进 data_service 的玩家字段命名空间**。data_service 以 `player:{<id>}:<字段名>` 存自己的玩家字段,并按 `player:{<id>}:*` 做 SCAN、整批删除与字段索引 `player:{<id>}:__fields`。主工作区 `data_logic.go` 的 `isReservedPlayerField` 写着规矩:任何模块新增 `player:{%d}:` 花括号键,必须登记为保留名,否则 data_service 可能把它当字段名「抢注」写成 string。见 §2.2 的 2026-10-19 增补。
    - 所以核心闭包如果照字面改成 `player:{<id>}:owner_epoch` / `:handoff` / `:location`,就会进入这个命名空间,被 data_service 的 SCAN 与删除扫到。
    - 两条出路:换一个前缀,例如 `pcore:{<id>}:owner_epoch`;或者逐个登记为保留名,并核对 data_service 的 SCAN 与删除路径都会跳过它们。
    - 键名格式与 §21 的 mapping 新键名一起定(§25.2 第 22 条)。
- 测试补一条:**「旧二进制按旧键 CAS、新二进制按新键 CAS 并存时,必须被门禁拒绝」**。
- mapping 不能双读(D1「缺键 ≠ 无记录」),见 §21。
- 单元化本身的配套(服务发现、scene_manager、消费组按 zone 或单元限域,单元划分与跨单元迁移协议)不在 Redis 范围内,见全局单点交接 §7 阶段 6 与容量模型 A1、§3.0。

## 21. mapping 三族 hash tag 的裁决材料(2026-10-18 新增)

> **(2026-10-23 注,本节原文保留)被取代**:映射改为直读 TiDB、撤掉 Redis 映射副本,mapping 三族不再有 Redis 键名可定,见 [player-route-mapping-durability-proposal.md](player-route-mapping-durability-proposal.md) §15「2026-10-23 负责人拍板:映射改为直读 TiDB,撤掉 Redis 副本」。§21.6 给瓶颈文档的同 tag 指引里,与映射无关的两条(令牌发布脚本、围栏 §5.5 推进脚本)仍有效。

### 21.1 现状键名与镜像点(2026-10-18 grep)

- 键:`player:zone:<id>`、`player:placement:<id>`、`player:maprev:<id>`,都不带 tag。同族还有 `merge:in_progress:<zone>`、`merge:merged_into:<src>`、`mapping:route:ready` / `:warm` / `:watermark`、`db:capability:zone:<z>`、`lock:player:<id>`、`player:name:<id>`。
- 定义与镜像点(改名要全部同批):
  - `go/shared/placement/placement.go`:`KeyPrefix` :30、`HomeZoneKey` :39-40、`CapabilityKey` :111;`route.go`:`MapRevKeyPrefix` :22、`MapRevKey` :25、`RouteReadyKey` :36;`route_ready.go`:`RouteWarmKey` :28、`RouteWatermarkKey` :33、`MergedIntoKey` :507;
  - data_service:`routing/router.go` 的 `mappingKeyPrefix` :118、`BatchGetPlayerHomeZone` 的 MGET :463、`GetAllPlayerIDsInZone` 的 SCAN :545;`routing/route_publish.go:147`;`routetool/redisread.go` 的 `homeZoneKeyPrefix` :33;`routetool/importer.go:199`(按两族前缀 SCAN);
  - `tools/merge_zone`:`main.go` 的 `playerZoneKeyPrefix` :96、`playerZoneKey` :99;`placement_codec.go` 的 `placementKeyPrefix` :28、`placementKey` :33;`placement_ops.go` :52(MGET)、:496 / :529(CAS 脚本键);`fence.go:53`;`merged_into.go`;
  - `tools/data_consistency_check/route_source.go` 的 `homeZoneKeyPattern` :112 与 :115;
  - guild `go/guild/internal/logic/merge_fence.go` 的 `mergeFenceKeyPrefix` :50(另经 `placement.MergeFenceKey` 取键,:159);
  - go/db 经 `go/shared/placement` 取键(`placement_route.go`、`saveoutbox/replay.go:270`);
  - `tools/redis_split` 的族表(未落码);
  - 调研规格列的 `k8s_zone_rollback.ps1` 守卫模式与 `k8s_zone_rollback_flush_guard.tests.ps1:58` 已随 R8 删除(§4.6 的更正),不再是镜像点。

### 21.2 两边的理由

- **主张改名**(容量模型 §4.2):第 1c 批的映射拷贝是唯一一次全量搬键的停写窗口;不改,将来集群化还要再停写搬一次;规模后停写搬键约 16.7 小时(§16)。
- **反对**(瓶颈文档 §6.5 :366「为 Redis Cluster 改 hash tag 键名:破坏滚动升级兼容与 merge_zone 的锁键镜像」,以及 go/db 交接附录提到的运维守卫模式):滚动升级时新旧二进制读写不同的键,缺键会被当成无记录;多个独立 module 各自镜像前缀。注意 §6.5 的原意针对 go/db 写路径的锁、游标、epoch、缓存键,mapping 键是在 3b 合并往返时被卷进去的。

### 21.3 已定原则

负责人 2026-10-13 定「以按 player_id 分片为准」(PROGRESS「2026-10-14 存盘管线接手」§5 拍板第 16 条)。§6.5 的三点(滚动兼容、镜像、守卫模式)不再是否决理由,改为分片迁移必须满足的约束。

### 21.4 剩下的三件事

- (a) **时机**:开服前做 1c 时同窗口改名;还是 1c 晚于开服时走路径 B,在新集群上按新键名从 `player_route` 预热。
- (b) **形态**:停写窗口内一次改名,全部读写方(21.1 的清单)同窗口切换,不做双读;用契约测试把镜像清单钉住,漏一处就会静默写错库。
- (c) **键名**:例如 `pmap:{<id>}:zone` / `:placement` / `:maprev`(调研者的建议,待拍板)。
- 改名只能让三族同槽。W1、发布脚本、读 home 时对 `merge:in_progress:<zone>` 的复核仍会 CROSSSLOT,要挪进库(D1 db_first 的提交点在库里),或按 slot 复制广播围栏。
- **(2026-10-18 新增约束)** D1 读方就绪脚本 `RouteReadyReadScript`(`route_ready.go` 约 :186)在一段只读 Lua 里取全局 `mapping:route:ready`、映射键与本实例身份(INFO 的 run_id / role / master_replid)。已接入:go/db `route_ready_gate.go`(`RequireRouteReady` 开时 lookup 与复核两步都走,每条写任务多 4 次 INFO)、data_service `route_read.go`(批量就绪读;`getHomeZoneFenceReadyScript` 用 ARGV 前缀在脚本内拼围栏键)、guild `merge_fence.go`。Cluster 下就绪标记与身份核对要按主节点各一份,读方按键所在节点核对;交 D1 线设计。

### 21.5 推荐(供拍板,不自行决定)

- 键名与 tag 现在就定,避免 D1 后续批次再引入新的镜像点。
- 开服前做 1c:同窗口改名,键还少,窗口可控。1c 晚于开服:走路径 B,在新集群上以新键名从 `player_route` 预热,不在旧实例上原地改名。
- 围栏复核挪进库的工作归 D1 线,在 mapping 上 Cluster 之前完成。

### 21.6 给瓶颈文档 §6.5 的指引(本稿不改瓶颈文档)

[db-task-pipeline-bottlenecks.md](db-task-pipeline-bottlenecks.md) §6.5 的分片修订由 go/db 存盘管线线写,要把 db-write-fencing-mysql.md §6.4 的同 tag 约束算进去:
- 令牌发布脚本里的排序锁、子表缓存、资产证据,以及 2026-10-17 起的 `save_barrier`,同 tag;若 §18.7 选 A,缓存可以脱离,锁与证据 / 屏障的关系由围栏线另定;
- 围栏 §5.5 推进脚本里的排序锁、游标、已落库 epoch 同 tag;
- 排序锁改名与 `tools/merge_zone/relocate.go` :367-372 的镜像同批改。

按组分实例并不免除这些 tag 约束:组实例内也可能上 Cluster,同一段 Lua 的键仍必须同 slot。

## 22. 账号目录落库(2026-10-18 新增)

### 22.1 现状 [C]

- `account:<account>` 是「账号 → 角色」目录的唯一权威,只在核心 Redis,没有库可以回源:`go/login/internal/logic/clientplayerlogin/loginlogic.go` 的 `getOrInitUserAccountScript`(约 :345;命中时还会 PERSIST,把旧 TTL 迁掉)注释写明「不是可重新加载的缓存」;`GetOrInitUserAccount`(约 :355)在缺键时就地 SET 空账号。建角写账号 blob 走 `createplayerlogic.go` 的 `writeAccountBlobScript`(:74),与 `account_lock:create:<account>` 同一段 Lua。
- 后果:FLUSHALL / FLUSHDB 会把所有 zone 的账号目录一起清掉,之后登录看到的是新建的空账号,角色从账号里「消失」(db-write-fencing-mysql.md §9.1 表的「`redis-cli FLUSHALL`」一行)。
- 容量约 205~360 GB(§16,[I]),落库之前叠加在核心上。

### 22.2 方向

- 在全局薄层建账号目录表,主键用 account 或它的哈希(随机键,没有尾部热点;容量模型 §4.2),与密码表的位置一并定(全局单点交接 §8 第 5 条)。
- 库是提交点,Redis 降为缓存:`GetOrInitUserAccount` 改为「缺键先回源库,库里也没有才初始化」;建角先在库里提交,再写 Redis。
- 集群化时 `account:<a>` 与 `account_lock:create:<a>` 同一个 `{account}` tag(§20.3)。
- 存量 `account:*` 在冻结建角的窗口内回填,回填幂等。

### 22.3 滚动升级顺序(必须按序)

**项目尚未上线**:若账号目录表在首次上线前落库,第 1 步回填与新旧混跑窗口都不存在,直接以库为提交点起步;下面的顺序只适用于上线后。

1. **先回填库**:冻结建角,把存量 `account:*` 写进账号目录表,并核对条数与内容。冻结范围是全部账号 blob 写者,不只建角:admin 摘角色(`go/login/internal/logic/admin/remove_players_from_accounts.go`,缺键时只打日志、removed=0,缓存化后会静默跳过)、入场自愈(`account_self_heal.go`)、身份补齐与外观修复(`player_class_backfill.go`)、debug_import 等。
2. **全部 login 换成新版**:新版遇到缺键**或空 blob** 都回源库核对,库里也没有才按新号初始化。只认缺键不够:新旧并存的窗口里,旧版 login 遇到缺键会写入空 blob;新版如果把空 blob 当成命中,角色就丢了。新版还要去掉 `getOrInitUserAccountScript` 命中时的 PERSIST,否则第 3 步加不上 TTL。
3. **Redis 才降为缓存**:之后才允许给 `account:*` 加 TTL,不再把它当权威。

- 部署闸拒绝新旧 login 混跑超过约定的窗口。两版并存时旧版的建角只写 Redis,窗口结束后要再对账一次。
- 回退:回到旧版之前,先确认 Redis 里的账号目录完整;Redis 已降为缓存、键已经过期之后不能直接回退。

### 22.4 Java

Java 版账号与角色已在 MySQL(§26),本项在 Java 是「已做,实现不同」。

## 23. 核心内存上界与冷卸载(2026-10-18 新增;S1 前拍板)

### 23.1 现状 [C]

- 父键在 C++ 首次存盘之后不带 TTL:存盘脚本用不带过期的 SET 覆盖父键(如 `kSaveWithCommitBarrierLuaScript`,`redis_client.h:66` 起),login 种子或冷加载写入时带的 TTL(`publishParentLoadScript` 的 `PX ... NX`,`parent_commit_barrier.go`)随之消失。删除点只有 merge_zone `player_rows.go`(合服 / 撤销,`playerCacheKeys`)与 `zone_rollback_discard` 契约(未落地)。
- `owner_epoch` 永不删(`kSaveIfGuardLuaScript` 上方注释)。
- `save_barrier` 与 `asset_sql_commit` 不设 TTL(§2.2 E 表后的说明;围栏 §6.6)。
- K8s 核心不设 maxmemory:`deploy/k8s/manifests/infra/redis.yaml` 的两条 exec 只带 `--appendonly`、`--maxclients` 与口令。
- 规模(§16,[I]):常驻至少 1~2 TB,在线热集约 200~420 GB。

### 23.2 卸载条件(候选,要与围栏线、全域到账线一起评审)

同时满足下列全部条件,才允许给父键加 TTL 或删除:
1. 玩家离线:没有 `player:<id>:location`,会话不是 ONLINE。
2. 最后一次存盘已落库:`save_barrier` 的 `pending` 与两表 ACK 精确相等。2026-10-17 起这是比「go/db 游标越过最后一次存盘」更直接的证据。
3. 没有未双 ACK 的补投意图:`save_barrier` 的 `outbox_*` 字段为空,玩家不在任何 `player:save_pending:*` 桶里。
4. 没有未终结的资产待办:assetop outbox 与托管(escrow)里都没有这名玩家的条目。
5. **资产账本判定遇到父键缺失时 fail-closed**:data_service `GetPlayerAssetOpLedger` 读父键 blob(`asset_op_ledger_logic.go`);自动放弃的条件之一是「父键 blob 与 SQL 证据都判未见」(围栏 §6.6)。父键被卸载后,读方必须把缺失当成「未知」而不是「未见」,不放弃、不终结,否则会一物两得。
6. 卸载后保留 `owner_epoch` 与 `save_barrier`:冷登录要靠屏障证明 SQL 两行同版,才肯重建父键。`asset_sql_commit` 是否保留随第 4 条定。

另需评估:
- 冷读放大:卸载后 task:result 的冷读约放大 20 倍(全局单点交接 §4.5)。B0-3 已落码(未验证),两件事同批排期。
- 父键的其他读者:friend `FillFriendProfiles` 读好友父键做展示(§20.5),match 组队在线判定读父键(`presence.go`),收件箱对账(data_service `GetPlayerInboxLedger`:缺键按 unknown 处理,是降级而非错误),login `fillMissingRoleAppearances`(`role_appearance_backfill.go`),以及 `save_barrier_restore`(`restoreScript` 以父键不存在为前置之一),都要有缺键时的降级,或确认缺键语义不变。
- 与新号种子(NewPlayerSeed)、合服回流(`tools/merge_zone/player_rows.go` 会删父键)一起评估。
- maxmemory:设了 noeviction + maxmemory 之后,满了就拒写,全服存盘与登录失败,所以要先有卸载与告警,再设上限。补投意图的常驻也要计入:正常约 11~83 GB;go/db 停摆 T 秒时上界 ≈ (PCU+E·T)×B,1 h 约 0.48~1.5 TB(§16,[I])。
- 提交点:卸载在同一 `{pid}` 的一段 Lua 里原子复核 player_locker、会话、location、handoff、屏障,预检不能代替(AGENTS.md §11.3)。

### 23.3 结论

即使做了冷卸载,在线热集约 200~420 GB 也放不下单实例。所以本节与 §20 的单元化 + 分片绑定,不能单独作为容量方案。

## 24. 新分批表(2026-10-18 新增,取代 §3.4)

| 批 | 内容 | 前置门禁 | 要改的运维脚本 | Java |
|---|---|---|---|---|
| 0(已落码,见 §17) | B0-1~B0-5;另有 P0-E | 交 Codex 编译 / 测试;B0-4 发布前读各节点池的 nofile 硬限;B0-1 的 player-locator ConfigMap Prometheus 段与 manifest 9190 端口同批;B0-3 若 login 已是第 1 批,按 zone 金丝雀 go/db;`dirty_keys_set` 存量清理等负责人指定环境 | release-checklist §6.1;`deploy-staging.sh`、`stress-linux-tier.sh` 的清理清单;player-locator 告警与 manifest;task-result-notify 告警;scene-manager 告警 | B0-4 Java 待做(PARITY「C++ 节点 RLIMIT_NOFILE」行);B0-3 第 1 批在 PARITY 有一行,状态是不适用,作为 db_task.proto 的 ContractSync 同步项保留;P0-E 见 PARITY「共享 Redis 连接上限」两行;B0-1、B0-2、B0-5 不适用,按开服线 10-19 交接 §4 的约定不进 PARITY(见 §26)。(10-22 订正) |
| 1a | 原 §3.4 第 1a 批,去掉「回档守卫补核心族」(回档已不再 FLUSHDB);拓扑函数按 zone 返回 DbCtl 的骨架;每个新实例显式 `--maxclients` 并进总账 | 与 L-02(打开排队与限流)的先后(时机待原 §13 第 10 条拍板);超过 30 文件需授权 | `k8s_deploy.ps1`、`dev_tools.ps1`(`k8s-redis-split-status`、`k8s-redis-wipe`)、`k8s_gate_drain.ps1`、`stress_snap.ps1` / `stress_summarize.ps1` | 待做(Java 没有 K8s 生产形态,§26) |
| 1b | 迁移工具 `tools/redis_split` 与墓碑检查;族表按 §2.2 的更正补齐新键 | 1a | merge_zone、data_consistency_check、route_tool;redis-split runbook | 待做 |
| 1c | 切 queue、mapping;mapping 三族是否同窗口改名按 §21 拍板;核心显式 maxmemory 按 §23 拍板 | 路径 A 只在开服前或 S1 以内可行,规模后走路径 B(§5.4 的更正);回档契约按拆分拓扑重写(映射复核改走 mapping 句柄加维护围栏;今天的契约要求 MappingRedis 与 go/db RedisClient 同实例同 DB,1c 之后回档 -Apply 必然拒绝,§2.3 的例外)。首次上线前做 1c 不阻塞,但契约要在上线前按新拓扑定稿 | 同 1a;核心重启走 `-ConfirmRedisCoreRestart`;`k8s_zone_rollback.ps1` 的部署事实核对与 `zone_rollback_discard` 契约 | 不适用 |
| 2 | dbctl 按 zone 组分实例(§18) | §18.7 二选一落码并验证;go/db 句柄拆成控制面与核心两个;db-write-fencing v0.4 + 10-17 附注上线,该组全部 zone `Fencing.Mode=dual`;`home_zone_unknown` 与 class-2 / class-3 计数在全部 zone 稳定为 0(与围栏 §8.5 同口径,§18.2);补投在跑、积压告警正常(§18.3);③ 消费组按 zone 已验证并收尾;禁止在线扩分区与 merge_zone 的窗口 | zones 配置加组字段;`k8s_deploy.ps1` 的拓扑与闸;`k8s-redis-wipe` 遍历组实例;连接总账按实例;merge_zone 的 dbctl 句柄;`zone_rollback_discard`(未落地)的组映射 | 不适用(Java 不经 Kafka 落库) |
| 3 | redis-cache 必做(§19),形态按拍板 | §18.7;是否还写子表缓存先拍板(§19);restore 工具的缓存句柄(`save_barrier_restore` 在缓存实例上核验「子表缓存不存在」,§2.3 的例外) | 同上;Cluster 形态下清库与告警按主节点;`save_barrier_restore` 与 `zone_rollback_discard` 加缓存句柄 | 不适用 |
| 4 | 单元化 + 单元内 Cluster 按 `{player_id}` 分片(§20) | 单元规模与分片数拍板;C++ 集群客户端与 FIFO 清单处置(§20.6);跨 tag 脚本改形(§20.4);全局键与 SCAN 改造(§20.5);键名迁移方式拍板(§20.7);账号目录落库(§22);冷卸载(§23);guild 迁出 DB 2;redis-match-cluster 的去留;friend 在线目录游标两版同批评审 | 全部清库 / 回档 / 合服 / 监控脚本按单元与主节点重写;各处「共享库不许集群化」的注释随落码更新:`deploy/k8s/README.md:792`、`k8s_deploy.ps1` :7554 / :7738、`go/friend/etc/friend.yaml:67`、`go/friend/internal/data/session_reader.go:34`、`go/friend/internal/svc/servicecontext.go:81`、`go/chat/internal/svc/servicecontext.go:32`、`go/match/etc/match_service.yaml:34`、`go/match/internal/config/config.go:22`、`go/match/internal/team/keys.go:11` | 待做;单元化与游标改格式要两版同批(§26) |

- **redis-queue 在 2000 万下**:排队轮询约 300 万往返/s(§16),一个实例放不下,要按 zone 组分实例,或上 Cluster 并给排队族打 `{zone}` tag(`Queue.Enqueue` 的 MULTI 才合法,§20.5)。轮询间隔与推送化归登录排队设计。
- 其他文档的指向注(本稿不改,归各自的维护线):`cross-zone-scene-travel.md`(CZ-2)、`cross-zone-matchmaking.md`(D12)、`tidb-phase1-and-launch-readiness.md` L-06、`no-single-node-horizontal-scaling-20261001.md` §5(redis-ha 已由该文 §5.1 记为 2026-10-16 接入、10-19 本地补齐(10-22 订正,原写「2026-10-19 接入」);「拆分方向待定」一句要指向本稿)、容量模型(§4.2 的指向注随本稿 2026-10-18 修订加上;A6 与 §6 第 6 条的指向注 2026-10-19 已补,三处都不再待做)、`docs/handoff/launch-readiness-20261013/spec_G4a.md`;db-write-fencing-mysql.md 三处:§5.1「applied_epoch 迁到 redis-mapping」、§6.4「与 G4a 按用途拆分的冲突」(RedisClient → redis-mapping / CacheRedis、推进脚本三把键都去 redis-mapping)、§9.1 表「G4a 拆分后核心实例单独回档」行。§18 之后都应指向所属组 redis-dbctl-g<N> 与 §18.7,由围栏线加注。
  - **(2026-10-19 补)上列指向注已全部补上**,都是只追加的带日期注、原文不动:CZ-2 表后、D12 表后、L-06 行尾、no-single-node §5 文首注、`spec_G4a.md` 文首、db-write-fencing-mysql.md §5.1 / §6.4 / §9.1 三处、`db-task-pipeline-bottlenecks.md` §6.5(按 §21.6 的指引),另补 `player-route-mapping-durability-proposal.md` 的 12 处过时注释结案注与 `gate-load-balancing-design.md` 的 scene_manager 读者注。见 PROGRESS「2026-10-19 G4a 收尾」条目。

## 25. 待负责人拍板清单(2026-10-18 新增,取代 §13 的相应条目)

### 25.1 已拍板(不再讨论)

| 日期 | 事项 | 结论 | 出处 |
|---|---|---|---|
| 10-13 | `kafka:expand:status` 死代码 | 保留 | PROGRESS「2026-10-14 存盘管线接手」§5 拍板第 5 条 |
| 10-13 | DBTask 字段号 | 9 给 `save_seq`,10 给 `reply_instance` | 同上第 13 条 |
| 10-13 | db 消费组按 zone(③) | 开服门槛 | 同上第 15 条 |
| 10-13 | Redis 分片的 hash tag | 以按 player_id 分片为准(瓶颈文档 §6.5 待修订) | 同上第 16 条 |
| 10-16 | mail 发奖与商城到账 | 对应 zone 的 go/db 切 dual 之前保持关闭 | PROGRESS「2026-10-16 补充:mail 发奖与商城到账的上线门禁」 |
| 10-17 | 核心 Redis 路线 | 单元化 + 单元内 Cluster 分片,两者叠加;D12 / CZ-2 改写为「逻辑单一、物理按 `{player_id}` 分片」;C++ 接集群客户端 | PROGRESS「2026-10-17 开服线:负责人拍板四项」第 1 条 |
| 10-17 | B0-3 | 授权超过 30 文件;先用 PUBLISH,SPUBLISH 推迟到上 Cluster 那批;分两批 | 同上第 2 条 |
| 10-17 | B0-1 | gate 索引在会话 Lua 里原子维护;上 Cluster 时按 slot 分桶改形 | 同上第 3 条;B0-1 设计 §11 |
| 10-17 | B0-4 / D1 1b-2 / `dirty_keys_set` | B0-4 缺省拒启并加豁免开关;1b-2 单标记、容差 0;存量清理暂不执行 | 同上第 4 条 |
| 10-17 | K8s 回档语义 | 丢弃 TargetTime 之后的写;回档不再 FLUSHDB | PROGRESS「2026-10-18 存盘管线剩余代码落码收尾：R1–R8」 |
| 10-13 | 原 §13 第 17 条(第 2 批是否等 L-09) | 由第 15 条「③ 是开服门槛」覆盖:上线后做第 2 批时 ③ 必然已完成;首次上线前没有在线流量,rebalance 停顿不阻塞。这是推论,不是新拍板 | PROGRESS「2026-10-14 存盘管线接手」§5 第 15 条 |

### 25.2 待拍板

1. **围栏硬依赖二选一**(§18.7):A 缓存围栏换成值封装;还是 B 排序锁、子表缓存、资产证据、`save_barrier` 同实例 / 同 slot。围栏文档 10-16 已登记,未拍板。
   - **(2026-10-18 新增)** 选 A 时,资产证据与 `save_barrier` ACK 是否仍需在同一原子步骤里核排序锁令牌(10-17 把屏障 ACK 加进了令牌发布脚本),由围栏线裁决。
2. **(2026-10-18 新增)go/db Redis 句柄的拆分**:`RedisClient` 今天必须是核心实例(屏障 ACK、补投、证据)。第 2 批要先把它拆成「控制面」与「父缓存所在核心」两个句柄,并补一道防止两者指错的启动闸。
3. **(2026-10-18 新增)补投分桶索引 `player:save_pending:<zone>:<bucket>` 在 Cluster 下怎么改形**:照 B0-1「按 slot 分桶」,还是移出父快照脚本另行设计(§20.4)。
4. 单元规模与单元内主节点数(§20.2;建议值等 V2 实测)。
5. dbctl 组大小与跨组合服:merge_zone 用源、目标两个 dbctl 句柄,还是规定合服只在组内(§18.2、§18.4、§18.5)。
6. redis-cache 的形态:Cluster,还是按 zone 组分实例(§19)。
   - **(2026-10-18 新增)** 子表缓存 10-17 起已无生产读者:是否还写子表缓存,redis-cache 是否还要单独部署;「写任务只 DEL 不 SET」与缩短 TTL 需要围栏线同意。
7. mapping 三族 hash tag 的时机、形态与键名(§21);瓶颈文档 §6.5 由 go/db 存盘管线线按 §21.6 的指引加注。**(2026-10-23 注:本条连同子项被取代,映射改为直读 TiDB,见 [player-route-mapping-durability-proposal.md](player-route-mapping-durability-proposal.md) §15「2026-10-23 负责人拍板:映射改为直读 TiDB,撤掉 Redis 副本」。)**
   - **(2026-10-18 新增)** D1 读方就绪脚本 `RouteReadyReadScript` 在 Cluster 下的改形,交 D1 线。已接入:go/db `route_ready_gate.go`(`RequireRouteReady` 开时 lookup 与复核两步都走,每条写任务多 4 次 INFO)、data_service `route_read.go`(批量就绪读;`getHomeZoneFenceReadyScript` 用 ARGV 前缀在脚本内拼围栏键)、guild `merge_fence.go`。
8. 账号目录落库的位置与主键(account 还是其哈希),与密码表的位置一并定;单独立项授权(§22)。
9. 核心 maxmemory 初值与冷卸载阈值、触发条件(§23;S1 开服前)。
   - **(2026-10-18 新增)** 补投意图的常驻要不要单独设上限与告警:正常 ≈ N_save×T_res×B,约 11~83 GB;go/db 停摆 T 秒时上界 ≈ (PCU+E·T)×B,1 h 约 0.48~1.5 TB([I],§16.2)。
10. redis-queue 在 2000 万下按 zone 组分实例,还是用 `{zone}` tag(§24)。
11. `redis-match-cluster` 是否纳入本稿:volatile-lru 会淘汰 match 的票据与锁,也会优先淘汰带 TTL 的 chat 历史;容量模型 A8 建议拆成两个集群(§2.2 G 的增补)。
12. friend 在线目录的游标格式:逐节点 SCAN 后游标格式要变。proto 写明是不透明分页游标,字段与类型不变;风险限于滚动升级时在途的 v1 游标被拒与服务端 48 字节上限,服务端兼容读 v1、按重新开始处理即可,上线前没有在途游标。客户端是否解析或限长未核实,仍列为客户端与 Java 同批评审项;Java 只需语义一致,不要求同格式(§20.5、§26)。
13. 核心闭包围栏键的迁移方式(§20.7):首次上线前改名,直接以新键名起步(见第 22 条);或上线后停写切换;或上线后版本门禁(门禁只保证翻转时全部写者认识新键名,翻转本身仍要停写排空,或逐玩家先写让旧名 CAS 必然失败的墓碑再写新键)。
14. 第 2 批的门禁「`home_zone_unknown` 与 class-2 / class-3 计数在全部 zone 稳定为 0」(与围栏 §8.5 同口径;上线前不阻塞):`home_zone_unknown` 给 C++ 补指标出口,还是按 `redis.cpp` 的 30 s WARN 汇总判读;class-2 / class-3 读 `db_fence_write_total{class}`(§18.2)。
15. **(2026-10-18 新增)** C++ 集群客户端的选型(hiredis-cluster、redis-plus-plus 或其他)与新依赖评审;连接数「进程数 × 主节点数」进 maxclients 与 nofile 预算(§20.6)。
16. **(2026-10-18 新增)** guild 从 DB 2 迁到 DB 0 的时机(§2.2 G 的增补)。
17. **(2026-10-18 新增)** `zone_rollback_discard` 与 `save_barrier_restore` 在拆分 / Cluster 拓扑下的契约:多句柄、映射复核方式、zone → 组映射(§2.3 的例外、§18.6)。
18. redis-ha(原 §13 第 13 条):核心的接入已落码(10-16 `43284eb0b` 接入,10-19 本地补齐;10-22 订正日期),未验证(§17)。还剩两件事要定:
    - 拆出的新实例与组实例是否也走 HA,`redis-mapping` 与核心哪个优先;
    - 第 4 批上单元内 Cluster 后,HA 形态的入口代理与哨兵怎样过渡到 Cluster 自带的故障转移,还有它对 C++ 集群客户端(§20.6)的影响。
19. 原 §13 仍待拍板:第 4 条(各实例 maxmemory 初值;核心部分并入本节第 9 条)、第 6、8、9、10、12、14、15、16、18 条。第 8 条只剩按实例清库的 AGENTS 改写;围栏归零那一半已由 10-13 第 14 条答复(条件写落码时再加)。
20. **(2026-10-18 新增)单元边界语义**:(a) 跨单元旅行改为转区的体验能否接受(容量模型 §6 第 5 条、全局单点交接 §8 第 2 条);(b) D12 改写后「逻辑单一」的作用域是全服还是单元——若全服,跨单元读(friend / guild / match / chat 对其他单元玩家的会话、位置)需要 player→单元寻址层与多集群客户端;若单元,跨单元匹配与好友 / 公会在线判定另行设计。客户端与 Java 同批评审(§26)。
21. **(2026-10-18 新增)补投工作器容量**:缺省 Batch 16、Interval 100 ms(`go/db/internal/saveoutbox/replay.go` 的 `normalized`;`service.go` 的 `Start` 只传 zone 与世代,取缺省值),约 160 名/s/副本,K8s 每 zone 2 副本(`deploy/k8s/manifests/go-svc/db.yaml` 的 `replicas: 2`);2000 万口径下每 zone 每秒约 222~250 份新意图 [I],每份 1~2 次 claim,正常态就可能积压,意图常驻随之上升,ZRANGEBYSCORE / claim / 读 home_zone 的往返也未计入 §16。选项:按实测调 Batch / Interval / 副本数;或在 ACK 齐时同段回收意图(要围栏线评审,Cluster 下与桶改形一起定)。
22. **(2026-10-18 新增)核心闭包键名是否在首次上线前改带 `{pid}`**(仍是单实例):项目尚未上线时这是最省的路径,以后上 Cluster 只做重分片,§20.7 的迁移问题随之消失;与 §21.5 对 mapping 的推荐同理。代价是现在就要动 C++ / Go 全部镜像点与 Lua 体内键形校验(§20.7)。新键名还不能直接用 `player:{<id>}:…`,那会进入 data_service 的玩家字段命名空间(§20.7 的 2026-10-19 补)。所以前缀与格式要一并定。

### 25.3 拍板记录(2026-10-23,负责人授权「按最标准的方式做」)

**依据与前提**:
- 负责人 2026-10-22 说 §25.2 的待拍板项「按最标准的做」,第 1a 批也要落码。本会话记为授权,包括第 1a 批改动超过 30 个文件(AGENTS.md §10.2)。
- 负责人 2026-10-23 补充三条:项目没上线,**玩家数据可以完全清空**;标准是数据强一致、代码可读易懂、可水平扩展。
- 两条外部决定以对方文档为准,本节只引用:
  - 映射改为直读 TiDB、撤掉 Redis 映射副本。负责人 10-23 拍板,经「账号登录逻辑/物理服瓶颈」会话转述,设计以 [player-route-mapping-durability-proposal.md](player-route-mapping-durability-proposal.md) 的新章节为准;
  - **核心(玩家域)分片路线**归「Redis 物理水平扩展设计」会话,设计稿 [redis-player-store-cluster.md](redis-player-store-cluster.md)。负责人 10-22 把「redis 物理水平无限扩展」交给它定方案,10-23 两会话对齐边界:玩家域单独上 Redis Cluster 按 `{player_id}` 分片,控制域留原共享实例。
- 下表的「标准答案」来自两轮独立的只读分析(各 3 名分析者,合计覆盖 1~22 条全部子项),由本会话合并。两轮结论不同的只有第 10 条,取舍理由写在该行。

**归属与结论**:

| 条目 | 结论 | 归属 / 状态 |
|---|---|---|
| 1、1-子项(围栏硬依赖二选一) | 本稿两轮分析都推荐 A(值封装,证据与 ACK 按版本自守)。玩家域稿选了 B(锁、缓存、证据、ACK 同 `{pid}` slot,不拆脚本),**以玩家域稿为准** | 玩家域线 |
| 2(go/db 句柄拆分)、5(dbctl 组与跨组合服)、14(第 2 批门禁) | 控制域留原共享实例,go/db 句柄拆分列在玩家域稿 §13 的落码顺序里 | 玩家域线 |
| 3(补投桶改形)、13(核心闭包迁移方式)、22(上线前改名) | 上线前直接改名、按 slot 分桶,不迁移、不双读(数据可清空)。键名用玩家域稿的 `player:{<id>}:*` + 保留后缀表,不用本稿分析推荐的 `pcore:` | 玩家域线,第 2 批已在落码 |
| 4(单元规模)、15(C++ 集群客户端)、18-子项2(Cluster 故障转移)、20(a)(b)(单元边界语义) | 玩家域稿自研 C++ 路由层,不用 hiredis-cluster。第 20 条原推荐「跨单元 = 转区」**作废**:负责人 10-22 的需求写明保留跨区位置透明,不改成转区体验(`docs/handoff/redis-horizontal-scaling-requirements-20261022.md` §1) | 玩家域线 |
| 6、6-子项(redis-cache / 子表缓存) | 随第 1 条的选择,以玩家域稿为准 | 玩家域线 |
| 7、7-子项(mapping 三族 hash tag) | 被取代:映射改为直读 TiDB | 映射线 |
| 8(账号目录落库) | 追认已落码的方案(`7d9208237`,`account-role-authority.md`):全局 TiDB 薄层,主键是 account 原文,密码表不并入 | 已落码,待 Codex 验证 |
| 9(核心 maxmemory 与冷卸载) | 核心即玩家域,以玩家域稿为准。控制域实例按第 19 条第 4 项 | 玩家域线 |
| 9-子项(补投意图常驻) | 补投在玩家域核心上,以玩家域稿为准 | 玩家域线 |
| 10(redis-queue 形态) | **按 zone 组分实例**(`redis-queue-g<N>`),不改排队键名,不上 Cluster。zones 配置加组字段,首发 1 组;非 dev 档的高可用形态与核心同一套 `-RedisReplicas` 档位规则。**两轮分析的分歧与取舍**:另一轮推荐 Cluster + `{zone}` tag,水平扩展同样成立,但要改 login、scene_manager、Java gateway 三处客户端和全部排队键名;排队键天然按 zone,按组分实例不改任何键、loginqueue 包不改,与 dbctl 组同一机制,取后者(KISS) | 本会话,第 1a 批 |
| 11(redis-match-cluster) | 纳入本稿:淘汰策略改为 noeviction,limits 调为 2 × maxmemory(1Gi);chat 已改 SQL 原件,不再拆 chat 集群 | **已落码 2026-10-23**,未验证 |
| 12(friend 在线目录游标) | 随逐主节点 SCAN 一起改,以玩家域稿为准 | 玩家域线 |
| 16(guild DB 2 → 0) | 上线前改为 DB 0,不搬数据;merge_zone `-redis-db` 与 dev_tools `-MergeRedisDB` 缺省同步改 0 | **已落码 2026-10-23**,未验证 |
| 17(回档 / 恢复工具在拆分拓扑下的契约) | 多句柄 + 维护闸作原子边界;玩家域部分以玩家域稿为准 | 玩家域线 |
| 18-子项1(新实例是否走 HA) | 控制域新实例(redis-queue 组)与核心同一套 `-RedisReplicas` 档位规则:dev 单实例,非 dev 高可用。redis-mapping 被取代,其「HA 第一优先级」作废 | 本会话,第 1a 批 |
| 19-原 §13 第 4 条(maxmemory 初值) | 控制域实例全部显式 noeviction + maxmemory,limits = 2 × maxmemory;release 档缺值拒绝部署。初值:redis-queue 每组 1gb,redis-match-cluster 每主 512mb | 本会话,第 1a 批(queue) |
| 19-原 §13 第 6 条(Redis 监控) | 直接上 redis_exporter sidecar(核心、redis-ha、redis-match-cluster),告警 `deploy/k8s/redis-alerts.yaml`;玩家域集群的告警归玩家域线 | **已落码 2026-10-23**,未验证 |
| 19-原 §13 第 8 条(AGENTS §6.2 清库口径) | 改为「对部署拓扑里每个 Redis 实例 FLUSHALL 并核对 DBSIZE=0,Cluster 逐主节点」。AGENTS.md 由负责人改,本会话不改 | **待负责人改 AGENTS.md** |
| 19-原 §13 第 9 条(stress_summarize 加 Redis 表) | 已由 L-18 会话做成 INFO 快照(`stress_snap.ps1 -RedisTargets`) | 已落码(别的会话) |
| 19-原 §13 第 10 条(拆分与 L-02 的先后) | 先拆 queue,再打开排队。上线前直接以拆出 queue 的拓扑首次部署,不存在切换窗口;打开排队的门槛另加「queue 已拆分」一条 | 本会话,第 1a 批 |
| 19-原 §13 第 12 条(purge 浸泡) | 不适用:数据可清空,控制域与 queue 都不做迁移,没有 purge | 结案 |
| 19-原 §13 第 14 条(deploy/k8s/AGENTS.md 拓扑描述) | 随批更新事实描述;规则条款仍由负责人改 | 本会话,随第 1a 批 |
| 19-原 §13 第 15 条(启动时查墓碑) | 不适用:不做迁移工具就没有墓碑;mapping 已改直读 TiDB | 结案 |
| 19-原 §13 第 16 条(`-RedisSplit` 怎么给) | 照 `-RedisReplicas` 的既有惯例:缺省按档位;zone 写路径不给就跟随线上,给了而与线上不同即拒绝;DryRun 按参数与档位渲染 | 本会话,第 1a 批 |
| 19-原 §13 第 18 条(login Redis 超时没传进 client) | 修:抽出 `redisOptions`,Dial / Read / WriteTimeout 原样传入;拨号超时由 go-redis 缺省 5s 变为配置的 3s | **已落码 2026-10-23**,未验证 |
| 21(补投工作器容量) | 玩家域稿把「ACK 齐时同段回收意图」列为后续项 | 玩家域线 |

**由此对第 1a 批的修订**(取代 §24 第 1a 行与 §4 的相应描述):
- 只做 queue 一个用途,mapping 被取代,dbctl 归玩家域线;
- 不做迁移工具(第 1b 批)、迁移标记、墓碑与 purge,第 1c 批简化为「清空后按新拓扑首次部署」;
- 部署闸只保留两条:地址要变必须显式确认;任一 zone 排队开着时拒绝切换 queue;
- scene_manager 读排空标记的句柄随 queue 一起指过去(§3.2.1 约束 1 的 10-18 补)。
- **(2026-10-23 落码时订正,上面几条原文保留)**:
  - **gate 排空标记 `gate:<id>:draining` / `drained` 留在核心,不随 queue 拆走**,上一条作废。排空标记是运维置位的低频标志,读方有 login 的 AssignGate 过滤与排空监视器、`k8s_gate_drain.ps1`、scene_manager 的跨 zone 重定向,与排队键之间没有原子操作。留在核心后:scene_manager 与 `k8s_gate_drain.ps1` 不用改;也不需要「按目标 zone 找它所属的 queue 组」这种跨组寻址;§4.4 第 5 条「核心上没有正在进行的排空」的闸也随之不需要。queue 实例只放 `queue:*` / `admit:*` / `admitted:*`、放行锁和 gateway 的 `rl:*` 限流桶。
  - **`-RedisSplit` 在所有档位缺省都解析为不拆**,第 19 条原 §13 第 16 条「非 dev 缺省拆 queue」改为:首次上线显式 `-RedisSplit queue` 部署;不带参数时渲染逐字节不变;zone 写路径跟随线上,与线上不同须 `-ConfirmRedisSplitSwitch`。
  - **(2026-10-24)第 1a 批已落码,未编译、未测试**:login 的 `Node.QueueRedis`、`k8s_deploy.ps1 -RedisSplit queue`、模板 `redis-purpose.yaml`、`redis-split-status` / `redis-wipe`,以及 dev_tools / k8s_image / 回档脚本的转发;文件清单与 Codex 验证命令见 PROGRESS「2026-10-24 G4a 第 1a 批落码」。
  - **queue 实例本批在所有档位都是单实例**(第 18 条子项 1 的「非 dev 高可用」推迟)。理由:`redis-ha.yaml` 的对象名(redis、redis-proxy、mymaster、redis-ha-scripts)写死,要先参数化才能复用;排队数据都带 TTL、可丢,单实例重启期间只影响该组 zone 的新登录。高可用变体列为后续项。

## 26. Java 版(AGENTS.md §12)(2026-10-18 新增)

- **本次是设计文档**,不改代码,也不改客户端契约,文档本身 Java 不适用;按用途拆实例与分片 Java 待做,账号目录 Java 已做、实现不同(见下两条;10-22 订正)。对应 Java 版本号 0.1.0-SNAPSHOT,与 PROGRESS 条目、Java 仓 PARITY.md 那一行一致。
- **账号目录**:Java 已在 MySQL(`D:\luyuan\xuanming-server-mmo\game-player-store\src\main\resources\db\game-player-schema.sql` 的 `account` 表 :4、`player` 表 :12)。本仓 §22 落库之后,两版是「已做,实现不同」。
- **按用途拆实例与按玩家分片**:Java 待做。Java 是 Redisson 单实例(`game-discovery\src\main\java\com\game\discovery\RedisAutoConfiguration.java:34` 的 `useSingleServer`),没有 K8s 生产形态;键族与本仓不同(账号在库里),要按 Java 自己的键重新盘点。2026-10-18 只读核对,Java 仓 HEAD `80be462`,核对时没有改 Java 仓代码;之后只在 PARITY.md 登记了一行(见本节末条)。
- **客户端契约**(两版共享的只有它):
  - friend 在线目录游标(`RecommendFriendsResponse.next_cursor`)若因逐节点 SCAN 改格式:proto 写明是不透明分页游标,字段与类型不变,风险限于滚动升级时在途的 v1 游标被拒与服务端 48 字节上限,服务端兼容读 v1 即可,上线前没有在途游标;客户端是否解析或限长未核实(未读客户端仓),仍与客户端、Java 版同批评审;Java 只需语义一致,不要求同格式(§20.5、§25.2 第 12 条);
  - 单元化把跨单元旅行改成「转区」,改变客户端可见的体验(容量模型 §3.0「契约 / Java」),与「逻辑单一」的作用域一起拍板、同批评审(§25.2 第 20 条)。
- **PARITY.md**:按开服线 2026-10-19 交接 §4 的约定,PARITY 只记 Java 待做项。所以 Java 仓 PARITY.md 只登记了一行「Java 待做」:按用途拆实例、单元化与单元内分片,以及游标、转区两项的两版同批评审。「已做,实现不同」(账号目录)与「不适用」的结论写在本节,不进 PARITY。各批落码时更新那一行。
