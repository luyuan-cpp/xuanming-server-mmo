# 玩家数据只存 TiDB 与全链路水平扩展:总体方案

> **状态**:v1.6 — 2026-10-30:阶段 A 的代码全部写完(§13),**未编译、未测试、未压测**,验证清单见 [落码进度](../handoff/player-save-direct-commit-20261030/progress.md)。v1.5(发号器落码时修正 §6,见文中注明之处)。总体方案,汇总「玩家数据存储方案」会话(2026-10-23 ~ 10-30)的讨论、用户拍板与推荐做法。**未落码、未编译、未测试、未压测**;容量数字除注明外都是推算 [I]。v1.1 补了 §2.1「Redis 该用的地方」、§2.2 判据,以及附录 A(会话里的关键发现、比较过的方案、更正过的说法)。v1.2 按用户 10-30 的决定重写 §6(64 位、向发号器申请、崩溃也不重复),新增 §7.1 别名合服。v1.3 加入 Lua 里 ID 用字符串、别名合服定稿、Kafka 最终用途清单(§3.5)。v1.4 按正确性评审重写发号器(§6.1~§6.3):内存发段 + 两份独立预留记录。
> **证据**:读代码、读文档,加两轮多代理只读盘点与对抗评审。第二轮的正确性评审因额度未跑成,发号(§6)与多集群(§7、§8)两部分**还没有过正确性评审**,落码前补(§12)。标注:[C] 读代码或配置确认,[D] 文档记载,[I] 推断,[W] 外部官方资料。
> **关系**:
> - [TiDB 决策 §9](./global-data-layer-tidb-decision.md)「玩家数据不再用 Redis 存储」:方向与现行键族分类以它为准,本文回答它 §9.4 的待定问题,不重复它的键族表。
> - [映射持久化提案 §15](./player-route-mapping-durability-proposal.md)「映射改直读 TiDB」:由「D1 剩余收尾交接」会话落码;本文涉及 go/db 选库、`player_route` 的改动排在 §15 之后,开工前与该会话对齐。
> - [玩家态 Redis 集群化](./redis-player-store-cluster.md):存档相关批次作废,只保留协调键的分片(§8.4)。
> - [TiDB Phase 1 与开服准备](./tidb-phase1-and-launch-readiness.md)、[容量模型](./capacity-model-hok-scale.md)、[会话制抽离判据](./session-extractability-mmo-slg.md)、[存盘围栏](./db-write-fencing-mysql.md)、[跨 zone 场景传送](./cross-zone-scene-travel.md)。
> **本文不是**:落码规格。每一批开工前另写规格(追加到本文或独立文档),评审通过后再落码。

---

## 0. 结论(先读)

1. **玩家数据只放 SQL。** 本 MMO 用 TiDB。Redis 不撤,只做协调:会话、位置、交接标记、锁与租约、排队限流、Pub/Sub、能从 TiDB 重建的缓存。(用户 10-24 定,TiDB 决策 §9)
2. **存盘直接提交 TiDB**,不经过 Redis,也不经过 Kafka。一次存盘是一个 TiDB 事务,写权代次和整笔存盘序号在同一个事务里核对(§3)。
3. **缩小崩溃丢失窗口:关键变化立即存 + 拆出热行短间隔存。**(用户 10-30 定,§4)
4. **正常运行不丢、不回档。** 只有进程崩溃、强杀、存储损毁、人为失误才会丢,且丢的是「回到上一次成功存盘」,不会混版本、不会被旧写覆盖(§5)。
5. **发号不依赖 TiDB / MySQL**:照旧向发号器申请号段,发号器的计数器换成 etcd;ID 是 64 位,不带分区、地域、代号、时间、节点等任何含义,崩不崩溃都不重复(用户 10-24、10-30 定,§6)。
6. **路由用落点目录**(玩家 → 集群 + 库),不把分区写进 ID(用户 10-30 质疑后改定,§7)。合服改成别名合服、去掉围栏(用户 10-30 同意,§7.1)。
7. **所有层都靠加机器扩。** 数据面全部可水平扩展;控制面(etcd、TiDB 的 PD)是单 leader,只让它们承担低频写,并按地域、集群各起一套(§8)。
8. **同一套代码按预算分三档部署**:MySQL 主从 / 单个 TiDB 集群 / 多个 TiDB 集群。后端档位已落码,部署参数 `-DatabaseBackend`(§9.1,未编译)。
9. **MMO、SLG、MOBA 都适用。** SLG 也默认用 TiDB;单张大地图超过一个进程的容量时要拆图(§10)。
10. **先用一个 TiDB 集群上线**,把以后改不了的东西(ID 位宽、打散常量、表结构)一次定对;压测之后再拆多集群(§13)。

## 1. 用户要求与拍板(按时间)

| 日期 | 用户原话(节选) | 落到本文 |
|---|---|---|
| 10-24 | 「我不是撤掉 redis 啊,就是玩家数据不要 redis 存储而已,其他改用的地方还是要用的哦……该用还是用」 | §2(已记入 TiDB 决策 §9) |
| 10-24 | 「我就要水平扩展,我不想自增 id 依赖 tidb 和 Mysql」 | §6、§8 |
| 10-29 ~ 10-30 | 架构以后可能用于率土之滨类 SLG;「就算 SLG 都想水平扩展」 | §10 |
| 10-30 | 「看情况加机器,预算不够就 mysql 最省钱,预算够所有的都可以水平扩展」 | §9 |
| 10-30 | 「所有的都要水平扩展」 | §8 |
| 10-30 | 「为什么一定要带分区……player_id 不要有什么依赖……加了分区以后真不好扩展」 | §6(ID 不带分区)、§7(目录路由) |
| 10-30 | 「玩家数据不回档,正常不崩溃不延迟情况下不会丢就行」 | §3、§5 |
| 10-30 | 选「关键立存加拆热行」 | §4 |
| 10-30 | 「开始做……跨服也不用 kafka 了对吧……总结整个对话,写进文档开始开工」 | 本文;§3.5 |
| 10-30 | 「就要 64 位,不考虑 lua java script json 先……不能用发号器申请吗……一定要不重复,全局唯一,不管崩溃不崩溃」 | §6 |
| 10-30 | 问合服围栏排队的问题「这个怎么做最好呢」 | §7.1 |
| 10-30 | 「etcd 整体丢数据,或被人从旧备份恢复,这个你也应该有解决办法吧……一起写进文档一起做,像 lua 这种 uint64 直接用 string 存储就行,存盘不能再按归属区选 Kafka topic 和数据库可以的,能水平扩展就行,那最后的 kafka 还只用在哪些地方呢」 | §6.1、§6.3、§7.1、§3.5 |

存盘路径:会话里对比过「直接提交 TiDB」与「保留 Kafka → go/db」(§3.6),用户随后说「开始做」并确认跨服不用 Kafka,本文按直接提交 TiDB 推进。若要保留 Kafka 存盘,§3 需要重写。

## 2. 数据放在哪

| 数据 | 放在哪 | 说明 |
|---|---|---|
| 玩家存档(核心状态、资产、成长进度) | TiDB | 唯一真源 |
| 写权代次、整笔存盘序号 | TiDB,与该玩家存档同一个集群 | 与存档在同一个事务里核对(§3.2) |
| 落点目录(玩家 → 集群 + 库) | TiDB | 映射提案 §15 改为直读 TiDB(§7) |
| 会话、位置、交接标记、战斗锁、排队限流、选主锁、Pub/Sub | Redis | 协调态:丢了能按协议重建,或按 TTL 自然失效 |
| 展示投影(好友档案摘要等) | Redis,可选 | 必须能从 TiDB 重算,不得成为唯一来源 |
| 节点间命令与事件(跳转通知、路由事件、快照、审计流水等) | Kafka | 传输通道,不承载存档(§3.5) |
| 发号计数、分区与集群登记、服务发现 | etcd | 控制面,低频写(§6、§8.3) |

从 Redis 移出的键族(父快照、子表缓存、存盘意图、资产证据、结算原件等)以 TiDB 决策 §9.3 表 A / 表 B 为准。

### 2.1 Redis 该用的地方(该用的一定要用)

用户 10-24 / 10-30 的口径:「该用 redis 的地方用 redis」。下表是改造后 Redis 继续承担的协调用途。键名与读写方以[玩家态 Redis 集群化](./redis-player-store-cluster.md) §5.1、§5.2 为准 [D],这里只按用途归类。

| 用途 | 键族(举例) | 写方 → 读方 | 为什么放 Redis |
|---|---|---|---|
| 在线位置与换场景 | `player:<pid>:location` 及修订号 / 下限、离场等待、换线冷却、进场去重 | scene_manager → C++、player_locator、login、guild、trade、mail、match、player_inbox | 高频读写、需要 Lua 原子比较;丢了玩家重新进场即重建 |
| 交接标记 | `player:<pid>:handoff` | C++ ↔ scene_manager | 交接过程中的短期协调;正确性由 TiDB 写权行兜底(§3.2) |
| 写权代次的投影 | `player:<pid>:owner_epoch` | scene_manager → C++、login、player_inbox | 路由与快速拒绝用;**权威在 TiDB**(§3.2),这一点与 TiDB 决策 §9.3 表 C「铸造照旧在 Redis」不同,以本文为准推进,落码前在第 1 批规格里定稿 |
| 会话与登录 | `player:session:*`、会话租约、gate 索引、AFK 通行、`player_locker:*`、`login_generation`、登录幂等 | player_locator、login → friend、guild、mail、match、login | 有 TTL 的短期状态,丢了重新登录即可 |
| 登录排队、准入与限流 | `queue:*`、`admit:*`、gate 排空标记;Java 网关 Bucket4j 令牌桶;sa-token 会话 | login、Java 网关 | 高频计数与过期,天然适合 Redis |
| 战斗互斥 | `battle:lock:*`、`battle:ctx:*` | C++ → guild、match 只读 | 短期互斥,有 TTL 与 reaper 兜底 |
| 场景路由与节点负载 | 场景路由、人数计数 | scene_manager、C++ | 可从运行中的节点重算 |
| 选主锁与短租约 | 排队调度、对账、补偿等单例任务的锁 | 各服务 | 租约天然要 TTL |
| 跨进程通知 | Pub/Sub:`assetop:wake`、`task:result:notify*` 等 | 各服务 | 只是唤醒信号,丢了靠轮询补 |
| 同步摘要 | `player:<pid>:sync_digest` | friend、guild、match → C++ | 只是提示,丢失或过期时客户端全量拉一次 |
| 匹配排队 | MatchRedis 排队池与匹配锁 | match | 排队状态短命,可重建 |
| 各服务私有的协调用途 | chat、friend、guild、match 的限流、配额、排队 | 各服务 | 同上 |
| 能从 TiDB 重建的展示缓存 | 好友档案摘要、在线目录、公会展示缓存(cache-aside) | friend、guild | 只做加速;必须能从 TiDB 重算,不得成为唯一来源 |

- **排行榜**:本仓 rank 已全部走 SQL,不用 Redis(TiDB 决策 §9.3 表 C [D])。以后若用 ZSET 做实时榜,榜单必须能从 SQL 重算。
- **Redis 自身也要水平扩展**:玩家相关的协调键按 `{player_id}` 分片成 Cluster,控制域按用途和 zone 拆(§8.4)。去掉存档后内存从 TB 级降到几十 GB 级(推算 [I]),但会话、位置、排队的读写量仍然很大,所以仍要分片。
- **改造时要一起改的读方**:match 的组队在线判定、friend 的在线推荐和好友档案、login 的选角外观补齐,今天读的是父快照(TiDB 决策 §9.3 表 B),要改成读位置 / 会话,或读 TiDB / 展示投影。

### 2.2 以后新功能怎么判断放 Redis 还是 TiDB

沿用 TiDB 决策 §9.2 的判据,看值,不看键名里有没有 player_id:

- 值是玩家持久数据的内容、它的副本或中转载荷,丢了只能从 TiDB 找回 → **放 TiDB**。
- 值是代次、租约、锁、位置、标记、计数、队列成员这类协调信息,丢了能按协议重建或按 TTL 失效 → **放 Redis**。
- 只做加速、真源在 TiDB、正确性从不依赖它的读缓存 → 逐项裁定,默认不新增。
- 资产、充值、交易、结算这类「丢了就是事故」的东西,一律不以 Redis 为准。

## 3. 存盘路径:直接提交 TiDB

### 3.1 流程

1. scene 在定时或事件触发时,把**有变化的行**序列化(§4),通过异步 RPC 交给存盘服务。scene 的 tick 不等回包([scene 线程模型](./scene-node-threading-model.md))。
2. 存盘服务开一个 TiDB 事务:锁该玩家的写权行 → 核对写权代次与整笔存盘序号 → 写入脏行 → 推进序号 → 提交。
3. 回包到 scene:成功就记下「已确认的版本」;被拒(代次过期)就销毁这个玩家的实体;结果未知(超时)就用**同样的内容和同一个序号**重试。
4. 存盘服务由 go/db 改造而来(用户 10-30 定),无状态多副本,复用现有行围栏判定(`go/db/internal/kafka/player_row_fence.go` 的 `savePlayerRow`:代次小判 stale、同代次序号小判 older、相同判 equal [C])。

### 3.2 写权:任何时刻只有一个写者

- **写权代次以 TiDB 为准。** 进场登记时在 TiDB 事务里铸造或抬升,存盘时在同一个事务里比对。今天入场登记已经在 TiDB 悲观事务里把 owner_epoch 写进 `account_role_admission.identity_proto`,但它不能比较,也不抬玩家行围栏(TiDB 决策 §9.4 第 3 条 [C])。要把它改成可比较的列,或像 Java 版那样放在玩家行里。
- **Redis 里的 `owner_epoch` 降为路由投影**,丢了能重建,不再是写权依据。这样「Redis 回退导致代次倒退、旧档覆盖新档」这一整类问题(G1、G3、R-G1-1,见[Redis 回退守卫](./zone-redis-regression-guard-20261022.md))不复存在。
- **整笔存盘序号单调递增**,放在写权行上。一次存盘整笔比较、整笔拒绝。评审发现:如果逐行判围栏、又跳过未变化的行,超时晚到的旧事务可能让两行落成内存里从未同时存在过的版本组合;整笔序号可以避免这个问题。
- **没有旁路。** 所有写玩家行的入口都经过写权行核对:存盘、建角初始行、GM 回档、合服工具、`save_barrier_restore` 的替代物、导入工具。

### 3.3 交接与加载

- **下线、换场景、跨节点**:最终存盘和「释放写权」在同一个事务里提交;提交成功后才放人。目标 scene 在事务里认领(代次加一)之后再读档。移交请求带令牌,回包丢失时重试也不会重复移交。
- **加载**:同一玩家的所有行要在同一个一致性视图里读出——一条语句读出全部行,或者先锁写权行再读。读已提交隔离下,同一事务里的两条 SELECT 不保证读到同一个快照。
- **热加载代价**:断线重连、同节点重登今天走 Redis 父快照,改为读 TiDB 后的代价要压测(TiDB 决策 §9.4 第 2 条)。
- **停机、发版**:先排空,所有在线玩家的最终存盘提交成功后进程才退出;K8s 的停机宽限时间要配够。

### 3.4 资产与跨服务

- 资产变化和它的幂等记录在同一个事务里提交,TiDB 决策 §8.4 的资产原子范围不变。
- 对方服务(公会、交易、邮件、收件箱)只认 TiDB 里的提交记录(强读),不认 scene 内存或 Redis 投影。今天 guild 的 `AssetOp.RequireSQLCommit` 缺省是 false,在线时按 scene 回包里的 Redis 快照终结(`go/guild/etc/guild.yaml`、`go/shared/assetop/reconcile.go` 的 `ProcessOne` [C]),新路径下这一档不再存在。
- 自动放弃(对方等太久就退款)判断玩家是否在线时,读 TiDB 的写权行,不读 Redis 位置。评审发现:如果只按 Redis 位置判断,Redis 丢键时可能一边退款、一边又把这笔资产存进档,造成一物两得。
- 战斗结算原件移出 Redis,走 match 写 SQL 原件加收件箱(TiDB)的新路径(TiDB 决策 §9.3 表 A)。
- 玩家之间转移资产(例如 PvP 抢夺)要走带操作号的资产通道,不能靠两次独立存盘;或者设计成系统掉落,不从对方身上拿。代码里目前没有 PvP 抢夺玩法 [C]。

### 3.5 Kafka 还用不用

存档不走 Kafka;跨 zone 本来就不搬数据。Kafka 只做一件事:**发出去就不用等结果的异步通知和事件流**。原则和 Redis 一样:该用的用。下表按 2026-10-30 的代码盘点 [C]。

**保留:**

| Topic | 谁发 → 谁收 | 干什么 |
|---|---|---|
| `gate-cmd_g<N>`、`scene-cmd_g<N>` | login、scene_manager、player_locator、mail、match、friend、player_inbox 等 → C++ gate、scene | 控制面命令与推送:绑定会话、进世界、踢人、跨 zone 跳转通知、玩家路由事件、组队刷新、收件箱唤醒、给玩家推消息。按节点分区,不随玩家数增加 topic |
| `match-results` | battle → match(评分)、guild(帮会历练结算) | 对局结果事件 |
| `transaction_log_topic_g<N>` | C++ scene → data_service | 审计流水,异步批量落库 |
| `player_snapshot_topic_g<N>` | C++ scene → data_service | 回档用的玩家快照。它带玩家数据的历史副本,但只是归档,不是真源;丢了只少回档点,不影响存档 |

**退役或清理:**

| Topic | 处理 |
|---|---|
| `db_task_zone_*` | 存盘和登录读任务的管线,随新存盘路径退役;连带 go/db 的存盘消费者、saveoutbox 补投、重试与死信队列里承载玩家行的部分。逐项裁定,裁定之前照旧(TiDB 决策 §9.1 第 3 条) |
| `gate-<id>`、`scene-<id>` 旧单节点 topic | 生产者已切走,K8s 已关,本机开关 `DisableLegacyPerNodeTopic` 还是 false,确认后关闭并删除旧路径 |
| `match-rating-snapshot` | 发布开关缺省关闭(`RatingSnapshotPublishEnabled: false`),排行走 SQL;确认没有读者后删除 |
| `game-events` | 配置里的默认值,代码里没有使用,删配置 |

存盘退出后,Kafka 流量的大头就没了(2000 万在线推算 0.35~2.8 GB/s [I]),分区数和磁盘预算要按剩下的 topic 重算。

### 3.6 备选:保留 Kafka → go/db

| | 直接提交 TiDB(采用) | 保留 Kafka |
|---|---|---|
| 提交点 | 一个事务;只有「提交了」和「没提交」两种状态 | 多一个「已进 Kafka、还没进 TiDB」的中间状态 |
| 登录、交接、资产到账 | 看事务结果 | 要等 Kafka 里的存盘落进 TiDB,需要另外记录「在途存盘」 |
| 唯一多出的好处 | — | TiDB 故障同时 scene 崩溃时少丢一点 |
| 额外部件 | 无 | 消费组、再平衡、重试与死信队列、分区数契约 |

用户的底线是「正常运行不丢、不回档」,Kafka 的好处只在双重故障时体现,所以不采用。

## 4. 崩溃丢失窗口:关键变化立即存 + 拆出热行(用户 10-30 定)

### 4.1 现状 [C]

- 周期存盘:每名玩家每 300 秒整份存一次,环境变量 `SCENE_PLAYER_SAVE_INTERVAL_SECONDS`(缺省 300,0 为关闭),按 `playerId % 间隔` 摊到每一秒(`cpp/libs/services/scene/core/system/redis.cpp`)。整份序列化后做脏比较,相等就跳过。
- 立即存盘:资产通道、战斗结算、收件箱会立即触发,带 500 毫秒节流(`asset_op_system`、`player_battle`、`player_inbox_system`)。
- 结果:scene 崩溃最多丢 300 秒的非资产进度(位置、打怪经验、任务进度等)。

### 4.2 决定

1. **关键变化立即存。** 保留现有立即存盘触发,并逐个系统补齐「丢了玩家会明显感知」的事件:升级、任务完成与领奖、稀有掉落、副本通关、充值到账等。立即存盘仍有节流,同一玩家在节流窗口内的多次变化合并成一次。具体事件清单在落码前逐系统确定。
2. **拆出热行,定时存盘只写有变化的行。** 玩家存档从「一行多个 BLOB 列」改成同一玩家的多行:
   - **热行**:小而常变的数据,如位置 `transform`、等级经验 `level_component`。
   - **其余按 TiDB 决策 §8.4 的职责分组**:资产组(货币、背包、资产流水、结算流水、收件箱流水)、成长组(任务、技能、加点、宠物)、档案组等。
   - 定时存盘缩到 30~60 秒量级(初值,压测后定),每次只写有变化的行:热行几乎每次都写,背包等大行只在变了时写。
   - 为什么必须拆行:TiDB 一条 SQL 行就是一个 KV,只改一列也要重写整行(TiDB 决策 §8.1),「只写变化的列」省不了写入量。

### 4.3 必须守住的约束

- 一次存盘的所有脏行在**同一个事务**里提交,带**同一个整笔序号**(§3.2)。
- 脏判断以「上一次**确认提交**」为基准,不以「上一次发出」为基准;结果未知的存盘重试时带同样的内容和同一个序号,否则会漏写某一行。
- 加载按 §3.3 在同一个一致性视图里读出全部行。
- 拆行是玩家表结构变更,按 TiDB 决策 §8.6 覆盖 proto 源与生成器、建表、装卸代码、快照与回档、合服与搬迁工具;新表名按职责命名,不再用 `_1`、`_2` 编号。项目未上线,直接以新结构起步,不做存量兼容。
- 本条拆分的理由是存盘频率,不是行大小。§8.3「测量支持才拆」的口径针对的是大小;本条按 AGENTS §6 压测事务数与写入量后定间隔。

### 4.4 效果与代价(推算 [I],未实测)

- 资产、升级、任务完成等关键进度基本不丢,只差节流窗口;位置、打怪经验最多丢一个短间隔(30~60 秒)。
- 2000 万在线时:整行每 300 秒存一次,约每秒 3.3 万笔事务,每笔 11~33 KB(写入 0.37~1.1 GB/s);热行每 60 秒存一次,按一半玩家无变化计,约每秒 17 万笔小事务,按热行 0.5 KB 估约 85 MB/s。位置走动时几乎每次都在变,跳过率会比整行低,实际笔数可能更多。
- 写入字节可能下降,事务数上升。事务数是 TiDB 最敏感的指标(每笔都要取时间戳,§8.1 第 3 条),间隔按压测结果定,扛不住靠多集群。
- TiDB 整体故障期间所有存盘都提交不了,丢失量取决于故障时长,与本条无关,靠降级模式控制(§5)。

### 4.5 待定

热行字段清单与大小;各组的表或行划分与命名(同一张表按 `(player_id, 组)` 分行,还是按职责分表);立即存盘事件清单与节流;短间隔初值;`player_database_1`(现在只有 `player_id` 与压测探针,TiDB 决策 §8.2)的去留;背包变化很频繁时,是否进一步把道具拆成多行(TiDB 决策 §8.4)。

## 5. 什么情况下会丢,什么情况下保证不丢

### 5.1 正常运行保证不丢、不回档

正常下线、换场景、跨节点、发版和停机(先排空)、TiDB 变慢(存盘排队重试,下线和换节点等提交完才放人)都不丢;新档不会被旧档覆盖。

### 5.2 会丢的情况

| 情况 | 丢什么 | 怎么防 |
|---|---|---|
| scene 进程崩溃(`kill -9`、内存耗尽、程序崩溃、所在机器宕机) | 上次成功存盘之后的进度:关键进度只差节流窗口,其余最多一个短间隔 | §4 |
| 发版或停机时还没排空就被强杀 | 没排空的玩家回到上次存盘 | 停机宽限时间配够,排空完成才退出 |
| TiDB 或存盘服务整体不可用,期间 scene 又崩溃 | 故障开始之后的进度 | TiDB 跨三个可用区;故障时进入降级,暂停会产出资产的玩法和新登录 |
| 同一份数据的多数副本同时损坏(三副本坏两个盘) | 已提交的数据也可能丢,只能从备份恢复 | 三副本分散在不同可用区;全量备份加日志备份,可恢复到任意时间点 |
| 整个地域的机房不可用 | 取决于异地容灾:TiCDC 异地同步官方 RPO 小于 10 秒,只用 BR 是分钟级([TiDB Phase 1](./tidb-phase1-and-launch-readiness.md) §8 [W]) | 做异地容灾 |
| 人为失误或代码 bug(GM 误操作、跑错脚本、字段忘了存) | 不确定 | 时间点恢复、应用级回档、测试 |

崩溃时回到上次存盘,玩家看来也是回档。要做到崩溃也不回档,只能每次改动都同步写库,本方案不这样做。

### 5.3 「一定」需要的前提与证明

前提就是 §3.2 到 §3.4 的规则:没有旁路;写权代次只增不减、在 TiDB 事务里核对;整笔序号整笔比较;同一次存盘的多行同事务;资产与幂等记录同事务、对方只认 TiDB 提交;自动放弃读写权行;移交带令牌。

证明要用真 TiDB 加故障注入(由 Codex 执行,AGENTS §10.1):存盘中途 `kill -9` scene;请求超时后重试;旧存盘晚到;被顶号的 scene 继续写;TiDB 切主;两地同时登录;跨节点交接时回包丢失;停机排空被打断。仓库已有的 L1–L4 一致性测试与混沌测试([数据一致性压测](./data-consistency-stress-testing.md))要在新路径上重跑通过。没有这些运行证据之前,不宣称「已保证」。

## 6. 发号:不依赖 TiDB / MySQL

### 6.1 机制

用户 10-30 定:**ID 是 64 位;用发号器申请;一定不重复、全局唯一,崩不崩溃都一样;Lua 里 ID 当字符串;先不考虑 JavaScript、JSON 的精度问题。**

v1.3 的「etcd 计数器 + 时间下限」经正确性评审被否决:时钟跑快过的发号器会把计数器抬到未来,之后 etcd 回退、时钟恢复,等真实时间追上来就会重号,而那时时钟完全正确;时钟门禁还会让一次坏时钟停掉全服发号。下面是修正后的设计,评审记录见 `docs/handoff/player-save-direct-commit-20261030/id-issuer-review.md`。

- **调用方不改。** C++ 的 `GuidSegmentClient`、Go 的 `go/shared/idsegment` 仍向 `data_service.AllocateIdSegment` 申请 `[lo, hi)` [C]。双缓冲预取、动态步长、发现号段重叠即拒绝都保留。「号段必须递增」这条要放宽:多个发号器副本各发各的块,同一个客户端先后领到的段不保证递增(落码时发现,2026-10-30)。
- **发号器从内存发号段,不是每次都写存储。** 发号器进程先预留一大块号,记录落盘后,再从这块里切号段给调用方。一块用完(或用掉一半时后台预取)才碰一次存储。所以 `id_segment` 表的单行热点和 etcd 单键争用都不存在。
- **预留记录写两份,两份互相独立。** 主记录和副记录分别放在两套独立的 etcd 集群里,都只增不减。一块号只有在两份都写成功之后才能发出去。任何一份丢失或回退,另一份仍然盖得住所有发过的号。详见 §6.3。
- **所有种类共用一个计数器。** 今天每种 `biz_tag` 一个计数器,不同种类的数值可能相同。共用之后,系统里任意两个 ID 都不相同。`biz_tag` 只用来校验和记指标,未登记的种类照旧拒绝。
- **打散不决定唯一性,只解决索引热点。** 发号器发出的是递增的计数器值,直接用的话新号都挤在索引最右边一个 Region。客户端发号时过一遍 63 位内的公开混合函数:`x ^= x>>30; x = x*C1 mod 2^63; x ^= x>>27; x = x*C2 mod 2^63; x ^= x>>31`,`C1`、`C2` 用 SplitMix64 的两个奇数常量。每一步都可逆,所以一一对应;没有循环;0 只对应 0,而计数器恒大于 0。Go 与 C++ 用同一组固定样例对拍,并测逆函数往返。
- **ID 不带业务含义**:不带分区、地域、时间、节点的语义,没有任何代码解析它。注意打散是公开可逆的,从 ID 能反算出计数器,所以这是「不依赖」,不是「保密」;权限判断不得依赖 ID 猜不到。
- **位宽**:类型是 64 位(`uint64`、`BIGINT UNSIGNED`)。**值只用低 63 位,最高位恒为 0(默认按此写,用户未否决;发第一个正式号之前还能改)**:2^63 约 922 亿亿,用不完;而代码里有依赖最高位的地方——Java 网关的 `playerId` 是有符号 `long`(`LoginResponse`、`LoginRpcClient` [C]);player_locator 的 `locationKey(int64(playerID))` 会把最高位为 1 的 ID 拼成负数键,和 scene_manager 写的位置键对不上,而且不报错(`session_cas.go`、`keys.go` [C]);trade 的守卫表靠 `| 1<<63` 区分哈希键和 ID 键(`market.go`、`auction.go` [C])。若要用满 64 位,这些地方要先全部改掉。
- **Lua 里 ID 一律当字符串**(用户 10-30 定):Redis Lua 的数字是浮点数,超过 2^53 会丢精度。脚本里 ID 只做字符串比较和拼键,不用 `tonumber` 计算或比较;有序集合的分数也是浮点数,不拿 ID 当分数。现有 `tonumber(` 约 96 处(30 个文件 [C])。2026-10-30 逐处看过:全部是计数、时间戳、版本号、代次和 TTL,没有处理 ID 的,不用改;有序集合的分数也全是时间。打散之后所有 ID 都超过 2^53,以后新写脚本要守这条。
- **唯一性只依赖四点**:预留记录只增不减;一块号两份记录都落盘后才发;CAS 只有一个预留者成功;打散一一对应。正常路径不依赖时钟和租约。

**计数器结构**(63 位,只有发号器知道,客户端不配、不解析):

| 字段 | 位数 | 说明 |
|---|---|---|
| 地域 | 6 | 放在最高位。64 个地域,现在全是 0;由发号器配置,并和存储里记下的地域核对,不一致拒绝启动。客户端拿到的 `[lo, hi)` 已经包含它,不需要、也不允许配置 |
| 时间 | 33 | 秒,从 2026 年起,约 272 年 |
| 序号 | 24 | 每秒约 1677 万个,是 2000 万在线推算峰值(约每秒 116 万条流水 [I])的 14 倍以上 |

计数器值连续,号段 `[lo, hi)` 就是这个 63 位上的连续区间,所以客户端的范围校验把上限改成 2^63,「必须递增」改成「不能和最近接受过的段重叠」。发号器发段时不要求 `hi-lo` 等于请求的步长,可以截短到块尾(客户端已支持)。

### 6.2 崩溃也不重复

| 情况 | 结果 |
|---|---|
| 调用方进程拿到号段后崩溃 | 没用完的号作废,重启后申请新段;只有空洞,没有重号。今天的 C++ 客户端就是这个语义:不持久化游标,优雅关服也不把尾巴还回去(`guid_segment_client.h` [C]) |
| 申请请求超时,不知道发号器发了没有 | 重新申请,上一段作废;不重号 |
| 发号器进程崩溃 | 手里这块剩下的号作废。重启后预留新块,新块一定在两份记录之上 |
| 多个发号器副本 | 每个副本独占自己预留的块,互不重叠;预留时靠 CAS 决定谁成功 |
| 进程卡死很久才恢复 | 手里的段或块是它独占的,接着用也不重号。这是号段比 snowflake 强的地方:不依赖租约和时钟 |
| etcd 切 leader、单节点宕机 | 多数派确认才算提交;没确认的预留不会被使用 |
| **一套 etcd 整体丢数据,或被人从旧备份恢复** | 另一份记录盖得住所有发过的号,下一次预留自动跳到它之上,不用人工处理(§6.3) |
| 时钟跑快、跑慢、回拨 | 不会重号。时钟只作为第三道防线抬高块的起点;跑快只浪费值域 |

### 6.3 etcd 丢数据或回退:两份预留记录 + 时间下限(与发号器同批做)

任何发号方案都有「记录不能回退」这个前提,放 TiDB 表也一样。解决办法是**先把上限写进两份独立的记录,再发号**,和 TiDB 的 TSO 先持久化时间窗口上限是同一思路:

```
预留一块(很少发生):
  P = 主记录当前值,S = 副记录当前值        // 两套独立 etcd,线性读
  start = max(P, S, now_floor)            // now_floor = 地域 | 当前秒 << 24
  end   = start + 块大小
  CAS 主记录 P → end                      // 冲突就从头再来,这是多副本之间的仲裁点
  CAS 副记录 S → end                      // 只增不减;失败则这块作废,不发
  两份都成功后,才可以从 [start, end) 发号段
```

- **为什么不重号**:发出去的每个号都小于当时两份记录的值。任何一份丢失或回退,另一份还在,下一块的 `start` 自动跳到它之上。手里已有块的发号器不受影响,因为块在内存里、从不回读。
- **时钟跑快也没事**:被抬高的 `end` 已经写进两份记录,之后时钟恢复、一份记录回退,`start` 仍然取到另一份的高值。v1.3 的漏洞就此消失。
- **没有门禁,不会一次坏时钟停全服**:发号路径上没有时钟判断。预留时时间下限永远生效:它只会把起点往上抬,不可能造成重号,而两份记录同时回退时它是最后一道防线;记录落后时钟很多也是常态,用量小的时候一块号能用几个月。`now` 不在可表示范围内(早于纪元,或超出 33 位)时这次不采用时间下限、告警,按 `max(P, S)` 继续。计数器比时钟超前 7 天以上只告警:说明时钟曾跳到未来,唯一性不受影响,只是时间下限在追上来之前不起作用。(v1.4 写的是「时钟跳变 7 天以上就不采用时间下限」,落码时改掉:那样两份记录都被旧备份覆盖时,最后一道防线反而不生效。)
- **两份记录都不存在**:可能是首次启动,也可能是两套 etcd 同时丢了。生产环境**拒绝发号**(fail-closed),由部署步骤显式初始化,与今天 `AllowAutoSeed=false` 同一语义 [C]。初始化入口是 `data_service -id-issuer-init`,不并进 `-migrate`(部署每次都跑 `-migrate`,不能让一次普通发布盖过这个事故),它在当前时间之上留一天余量。事故恢复见 [发号器运维手册](../ops/id-issuer-runbook.md)。dev 环境(`AllowAutoSeed=true`)自动初始化。
- **一份记录不存在**:另一份有值,按 `max` 继续,并把缺的那份补上,同时告警。不用人工。
- **硬校验**:`end` 不得超过本地域的值域上限,时间字段不得溢出;否则拒绝发号。
- **块大小**:2^32 个号(约 43 亿)。发号器每块至多作废一次,时间字段每 256 秒前进一块的量,所以正常用量下值域由时间决定,不会因为重启被耗尽。用掉一半时后台预取下一块,留出处理第二套 etcd 故障的时间。
- **第二套 etcd 的代价**:多一个 3 节点的小集群,写入极少。它不可用时,新块预留失败,但发号器手里的块还能用很久(按峰值也有一小时以上 [I]),其间告警处理;都用完才 fail-closed。生产环境启动时校验两套 endpoints 没有交集,dev 可以指向同一套、用不同的键。发号器启动不连 etcd,etcd 暂时不可达不影响进程启动,只影响预留新块。

**还会不会重号**:只有两套 etcd 同时回退,并且时钟不准(或曾把计数器抬到未来)才可能。这要写进 etcd 恢复 runbook 并演练:恢复其中一套时另一套不许动;两套都要恢复时按 ID 安全事件处理。

已有的防线保留:

- **客户端校验**:拿到的段不能和最近接受过的 16 段重叠,否则整段拒收(`client.go`、`guid_segment_client.h` [C]);上限校验放宽到 2^63。这只是兜底,窗口之外的重复查不到。
- **部分唯一键兜底**:建角初始行是严格 INSERT,撞号报错回滚(`account_role_initial_commit.go` [C]),邮件、挂单等表的主键同样报错。但这不是全部:`transaction_log_store.go`、`snapshot_store.go` 的写入撞号不报错,存档 BLOB 里的物品号也不报错,所以前面的设计必须成立,不能靠唯一键兜底。

**落码要带的测试**(由 Codex 执行):主记录回退、副记录回退、某一份被删、两份都被删、时钟回拨、时钟跳到未来再恢复后主记录回退(v1.3 的反例)、并发预留、副记录写失败后块不被使用、发号器崩溃重启、调用方崩溃重领,都要断言发出的 ID 不重复;打散函数 Go 与 C++ 用同一组固定样例对拍,并测逆函数往返。

**etcd 运维现状也要补**:已有每 5 分钟一次的 MVCC 压缩 CronJob(`deploy/k8s/manifests/infra/etcd-maintenance.yaml` [C]),但镜像仍是 `:latest`、配额是默认 2 GB、碎片整理只有手工流程。

### 6.4 适用范围(盘点 [C])

- 现有 17 个号段种类:player、guild、item、txlog、snapshot、trade_listing、trade_asset_op、guild_asset_op、friend_request、mail、mail_asset_op、rank_board、rank_settlement、rank_reward、battle、snapshot_row、rollback_audit。其中 trade_listing 段兼发交易与商城单号,guild 段兼发历练大厅号,item 段兼发宠物号,battle 段兼发 challenge_id、team_id。
- 自增主键改由应用发号(A7c,已写未编译):
  - 全局库 `player_snapshot.id`、`rollback_audit_log.id`:data_service 进程内直接调发号器(biz_tag = snapshot_row、rollback_audit),不走 RPC。发号器不可用时插入失败,快照消费者停在当前 offset,不自造 id。撞主键只有发号器发重才会发生,这时 ODKU 会吸收成 affected=0,写入路径认得出来并报错重试,不把这条快照当成已存在丢掉。
  - Java 网关 `zone_whitelist.id`、`announcement.id`:运营手工维护的小表,行号只在本表内用,不走发号器(网关不依赖 data_service)。用 `RowIds`:毫秒时间 × 1024 + 随机低 10 位,同进程严格递增,小于 2^53。**不能打散**:zone_whitelist 的 ODKU 只排队不成环,前提是新行的唯一键项排在删除标记项之后(见 `ZoneWhitelistRepository`)。这个前提要用 `ZoneWhitelistUpsertLockOrderMySqlTest` 在真 MySQL 上复验。`mall_game`、`springboot_satoken_auth_starter` 不在部署里,没有改。
  - **已有的库要清库重建**:`-migrate` 的漂移校验会比对 auto_increment 和默认值,旧表 `id` 带 AUTO_INCREMENT,协议重新生成后迁移报 `ErrSchemaDrift`。不伤数据,但发布卡在 migrate Job。项目未上线,和翻 `PlayerStorePath` 一样清库;新旧二进制也不能混跑(旧二进制不写 id)。
  - `mysql_database_table.proto` 三张旧表的 `OptionAutoIncrementKey = "player_id"` 没动:值一直由应用给,B8 随旧表删除。
- battle_id、challenge_id、team_id 从 snowflake 迁到号段(biz_tag = battle,A7c,已写未编译),match 不再申领 snowflake 槽位,也没有失租强退了。match 不在启动时预领第一段:K8s 上它先于 data-service 部署,预领必失败,而领段结局未知会让这一轮启动的维护排空出不了收据。运行中领段失败过一次也一样,要重启 Pod,与 guild、trade 同口径。依赖单调的写法逐处核过:SQL 都是时间列在前、id 只作并列决胜;ZADD 分数不是 id;Go 里没有解析这些 id 的,C++ `ParseGuid` 只在测试里用。scene_id 不入 SQL 表,保留 snowflake。
- 依赖单调性的代码(2026-10-30 逐处核实):邮件收件箱、卖家货架、默认浏览、玩家自己的合同与挂单列表、帮会资产唤醒顺序,改成按时间列排序。不用改的:`snapshot_store.go` 的 `id` 只作同秒决胜,打散后同秒次序不代表先后,本来也不靠它;`schema.go` 去重留最小 `id` 只针对唯一键建起来之前的历史自增行;各资产表的 `op_id > ?` 只是稳定翻页,判龄靠时间列;`BrowseCommerceOffers` 按 `commerce_id` 只是稳定翻页,打散后顺序随机,不影响公平;好友推荐按 `player_id` 区间随机取样,打散后分布更均匀。
- **回退 snowflake 已删**:`Minter` 的回退依赖「两套值域不相交」,打散后不再成立。`FallbackToSnowflake`、`raiseIdSegmentFloor`、2^55 硬编码、`id_segment` 表都已随发号器批删除。login、guild 的 snowflake 节点申领代码还在(`sf.Fence()` 仍在用),可以单独清理。
- 用大号跑全链路:dev 现在是小号,打散后所有 ID 都超过 2^53,要验证 Lua、Redis 键、Java 网关都正常。
- AGENTS §7 第 1 条(SnowFlake 节点隔离、位布局)要同步修订。

### 6.5 退路

只想消掉索引热点、不在乎发号依赖谁时,最省事的是保留 data_service 号段、只加一层打散,不到 10 个文件。它不满足「不依赖 TiDB / MySQL」,只作退路。

## 7. 路由:落点目录

- **目录**:「玩家 → 集群 + 库」。现有的 `player_route` 表与落点记录里的 `storage_id` 就是它,映射提案 §15 正在改成直读 TiDB;以后把 `storage_id` 从「选库」扩成「选集群 + 库」([玩家存储落点](./player-storage-placement.md) §0 第 7 条:今天只选库名、不选机器)。
- **建号时选集群**:按地域和各集群的余量挑一个写进目录,亚洲玩家落在亚洲集群。
- **目录不是单点**:读多写少,各服务缓存;目录表按 player_id 哈希分散到各个集群。
- **到了目标集群再核一次**:用那边的路由行在同一个事务里核对;搬走的玩家在旧集群留一条「已搬到某集群、版本 v」,旧写一律拒绝并转发。
- **扩容**:加集群 → 新玩家落新集群 → 需要时按批搬迁老玩家。搬迁沿用现有 relocate 的「冻结 → 拷贝 → 切换」,把它从同一个库实例扩成跨集群(今天只支持同实例,[TiDB Phase 1](./tidb-phase1-and-launch-readiness.md) §8)。
- **为什么不把分区写进 ID**:分区位一旦写进 ID 就永远改不了——分区数定死、热分区拆不开、单个玩家搬不走、建号那一刻就决定了住处。目录路由的代价只是多一次可缓存的查询,加上要把搬迁协议做扎实。
- **合服围栏**:见 §7.1。
- **SLG**:路由单位是世界,「世界 → 集群」同样是一张目录表。

### 7.1 合服:去掉围栏,改成「别名合服」(用户 10-30 同意)

用户 10-30 回复:「存盘不能再按归属区选 Kafka topic 和数据库可以的,能水平扩展就行」。本文按此推进:存盘不再按归属区选 topic 和库,合服改成别名合服、去掉围栏。这改变了映射提案 §15「保留合服围栏落库」的结论,但切换排在新存盘路径之后,在那之前 §15 照旧。

**围栏的问题 [C]**:`zone_merge_fence` 每个 zone 64 行。`LockRoleRoutes`(`go/shared/placement/role_route_sql.go`)每次都对其中一行 `FOR UPDATE`,调用方是聊天、公会、匹配组队、收件箱、角色首次提交。TiDB 默认不支持共享锁,所以只是想看一眼牌子的读者也要拿排他锁,互相排队。按每人每 20 秒一次写、持锁 20 毫秒推算,单 zone 在线约 6.4 万到顶 [I];拆多集群时还要把围栏写满每个集群。

**根因**:今天合服要列出玩家清单、改每个人的归属,清单列完之后不能再有人登记或改动,所以才需要牌子。不改每个人的归属,牌子就不需要了。

**做法**:合服只写一条「5 区 → 6 区」的映射,用现有的 `zone_merged_into` 表(已是永久记录 [D])。

1. 玩家行、路由行都不动,`home_zone` 还是 5。
2. 所有按区分组的地方(榜单、频道、市场、公会、进场判断)先把 `home_zone` 解析成最终区再用。解析收口到一个函数;映射表各服务缓存,变更走通知加版本号;支持连续合服(5 → 6 → 7)。
3. 合服过程中有人在 5 区建号也不丢:他的 `home_zone` 是 5,解析出来就是 6 区。不需要清单,也不需要牌子。
4. `LockRoleRoutes` 去掉围栏这一步,只保留每个玩家自己路由行的锁和 home / rev / frozen 复核。每个玩家锁自己的行,互不排队,加机器就能扩。
5. 榜单、公会、市场这些按区存的业务数据,仍由各业务的合并任务处理(`merge_zone` 现有的公会 zone_id、聚宝斋 market_zone、公会榜等步骤),在维护窗口里进行,只影响这些业务自己。
6. 撤销合服:删掉映射;合服后新建的号按它们自己的 `home_zone` 留在各自的区。

**前提**:存盘不再按归属区选 Kafka topic 和库(C++ 存盘今天按 home_zone 选 `db_task_zone_<z>` [C])。否则别名合服之后,5 区的 topic、go/db、库都下不了线。这一条等新存盘路径(§13 A3~A6)落地后才成立。

**时机**:项目未上线,上线前不会发生合服,所以不做中间方案,等 A6 之后直接切(§13 A6b)。在那之前围栏原样保留。

**代价与待验证**:所有按区分组的入口都要走解析函数,漏一处就会分错组,要用契约测试钉住;映射缓存滞后窗口内的写,由业务合并任务兜底;拆多集群时映射放在控制面,不再需要逐集群写围栏。

**落码口径(2026-10-30,A6b)**:

- **解析收口**:`placement.ResolveMergedZone(merges, zone)` 是唯一的解析函数,沿链解析(5→6→7),跳数上限 16,成环报错。按区分组的调用方几乎都经 `data_service.GetPlayerHomeZone` / `BatchGetPlayerHomeZone` 取归属,所以在这两个 RPC 的出口解析:它们返回的 `home_zone_id` 就是最终区。少数直接读 `player_route.home_zone` 做分组的(`go/match/internal/authority`),读完调同一个函数。
- **原始归属只有三处用**:go/db 按 `home_zone` 选库(玩家数据不搬,原值才找得到库)、`RegisterPlayerZone` 写入、路由工具。这三处不解析。
- **缓存**:data_service 启动时读一次 `zone_merged_into`,读不到拒绝启动;之后定时重读(默认 5 秒)。方案里说的「变更通知加版本号」先不做:合服频率极低,只靠定时重读,滞后上限是一个周期,`merge_zone` 写完映射后等 2 个周期再跑业务合并任务。
- **合服顺序**:前置检查 → 映射冲突检查 → 公会重名检查 → 清单落盘 → **写映射** → 等缓存 → 公会合并 → 交易合并 → 公会榜。映射先写,新建的号、新建的公会从这一刻起都归到目标区,不会有漏网的行,所以不需要任何闸门。清单仍在第一次写之前落盘,撤销靠它。
- **删除**:`zone_merge_fence` 表、`merge:in_progress:{zone}` 键及其镜像、`GetPlayerHomeZoneResponse.home_zone_merging`、scene_manager 的可重试码 21、guild 的 `MergeFence`、`RemapHomeZoneForMerge`、`merge_zone` 的玩家映射改写、玩家行拷贝、落点钉住、回填、搬库。`LockRoleRoutes` 只锁每个玩家自己的路由行并复核 home / rev / frozen,`route_meta` 一并移出这条路径。
- **没做的**:跨集群搬迁(§7)要在 player_store 上重写,旧 relocate 随旧表删除。

## 8. 水平扩展全景

| 层 | 怎么扩 | 前提 |
|---|---|---|
| 无状态服务(login、data_service、go/db、好友、公会、聊天、邮件、排行、交易、匹配、收件箱、Java 网关) | 加副本 | 选主的单例任务按分区拆成多把锁 |
| gate | 加节点 | — |
| scene / battle | MMO 加分线、加场景实例;战斗按对局装箱 | 一个场景里很多人(万人攻城)的瓶颈在场景计算与视野广播,不在存储 |
| SLG 地图服 | 加世界;单张大图按州拆分片 | §10 |
| TiDB | 单集群加 TiKV、TiDB 节点;到顶后多集群;每个地域各一套 | §8.1、§8.2 |
| Redis | 玩家相关协调键按 `{player_id}` 分片成 Cluster;控制域按用途和 zone 拆 | §8.4 |
| Kafka | 加 broker | topic 分区数第一代给足(D5 契约规定分区数不可变) |
| etcd、PD | 不靠加机器扩写入 | §8.3 |

### 8.1 单个 TiDB 集群加机器解决不了的四类

1. **单调递增索引尾部**(开服建角潮):号段单调发号时,新建角色的 INSERT 只写主键索引最右边一个 Region;`PRE_SPLIT_REGIONS` 只预切行数据,不切索引([TiDB Phase 1](./tidb-phase1-and-launch-readiness.md) §5 [D])。→ §6 可逆打散。
2. **单行热点**(一行只在一个 Region leader 上,悲观锁排队)。清单:
   - 聊天每发一条消息,都在同一个事务里锁角色路由,再对 `account_role` 加 `FOR UPDATE`(`go/chat/internal/authority/store.go` 的 `lockActor` [C]),是事务量最大的一项 → 改成会话级令牌,在聊天自己的库里复核。**已落码,未编译**:校验挪到发送事务之外,普通读 `player_route` 和 `account_role`,结果按 (actor, account, owner_revision) 在进程内缓存(`ActorVerifyTTLSeconds`,生产 30 秒,0 = 每次都校验),发送和拉取事务里只剩聊天自己的表;代价:角色被挂牌或过户后旧会话最多还能发 TTL 秒的聊天,限流从精确变成近似(超额上限是这名玩家同时在途的请求数,gate 还有第一道限频),同一 request_id 的并发首发改由主键裁决、输家撞 1062 转读原件;
   - 评级游标 `match_rank_source` 每局 `FOR UPDATE` 一次 → 拆片。**已落码,未编译**:来源表和 outbox 主键加 `shard`(`battle_id % 64`),每局只锁一片,发布循环按片推进,停写逐片取锁;榜的幂等和连续改成按 (shard, cursor),`rank_board.committed_cursor` 改存各片批次总数,封榜按总数核齐。拆片破坏了「批次全局有序」,而批次是绝对分,旧批次晚到会把榜分退回旧值,所以批次带 `score_version`(这名玩家第几局,在玩家行锁下递增),榜上旧版本迟到直接跳过;代价:榜多一列、每批多一条取本片游标的读,榜分在投递途中不保证与全局提交顺序一致,只保证每名玩家最终落在最新版本;
   - guild、trade、mail 的哨兵守卫行 → 撤掉,状态行在首次提交时预建。**已落码,未编译**:`guild_player_state(0)`、`trade_player_op_seq(0,0)`、`mail_box(0)` 都撤了,状态行在业务事务之前的短事务里 `INSERT IGNORE` 预建,查重靠唯一索引撞 1062。它们防的都是 MySQL 上的 1213(唯一索引查重的 S next-key、首插者回滚的间隙继承),不管对错;TiDB 没有这些环。MySQL 档上写者闸门把 1213 变成不可重试的结果未知(§9.3),`retryOnDeadlock` / `WithTxRetry` 认不出,这一笔直接失败,事务已回滚,玩家重试。代价:MySQL 档这几条路径从没有死锁变成偶发失败;mail 的 `MaxActiveSystemMails` 变成软上限,并发发件可能略超(读侧有 LIMIT,发件方只有运维和帮会公告)。trade 的 `trade_commerce_guard` 没动:它按拍品、按盘口各一行,串行的是撮合本身,不是全服排队;
   - 登录读账号目录 → 命中即只读。**已落码,未编译**:`Read` 先无锁读,命中只读返回,只有 create=true 且没命中才进加锁建行路径(锁内复核);代价:Read 不再排在同账号在途的建角后面,提交前读到的是提交前的列表,调用方都不依赖那次排队(改归属的操作按 owner_revision 复核);
   - `route_meta` → 移出提交路径。**已落码,未编译**:建角的 `lockInitialPlayerRoute` 改普通读 `writable`,`FOR UPDATE` 只留在修库、解冻、建池探针这些低频路径;读到旧值不是问题,提交点的保证靠写者闸门:事务持着 `writer_slot` 行锁并在持锁后复核过 `route_meta`,`Catalog.Freeze` 拿到全部 slot 锁才能推进 epoch,冻结只能排在在途事务提交之后,`route_tool` 的冻结流程不用改。写者闸门每笔事务还要普通读这一行两次,读负载靠缓存表(TiDB 运维步骤,S2 档起必选);
   - 合服围栏 `zone_merge_fence`(聊天、公会、匹配组队、收件箱、建角每次都锁一行)→ 改别名合服后删除(§7.1);
   - 自增主键 → 应用发号;号段领段行 → 随 §6 消失。
3. **时间戳(TSO)**:每个事务都要向唯一的 PD leader 取时间戳;按官方文档,读已提交隔离下每条语句默认都要取 [W]。→ 先减少每笔事务的语句数(上面的单行热点清理同时起这个作用),压测出单集群上限,再按 §8.2 拆多集群。
4. **跨地域**:单集群跨地域要求延迟小于 30 ms([TiDB Phase 1](./tidb-phase1-and-launch-readiness.md) §8 [W])。→ 每个地域一套集群,不把一个集群拉到多个大洲。

### 8.2 拆成多集群的前提

- **拆开跨服务的同事务耦合**:聊天、公会、匹配、收件箱等在同一个事务里同时锁角色目录和路由。不拆开就只能一个集群——拆早了,这些事务会跨集群,原子性保证不了。先拆聊天(§8.1 第 2 条)。
- **同集群放置**:玩家行、写权行、路由行、角色目录、入场登记放在同一个集群,保证 §3、§15.3 的同事务核验成立。
- **名字与账号目录按哈希分到各集群**:开服建角峰值在 S3 档推算为每秒 1~5 万([容量模型](./capacity-model-hok-scale.md) `C_p` [I]),放在单一全局库会成为单集群瓶颈和全球单点。

### 8.3 控制面

etcd 和 TiDB 的 PD 都是单 leader,加机器提升不了写能力。只让它们承担低频写:发号器从内存发号段,只在预留新块时写 etcd,频率极低;分区与集群登记很少变。再按地域、按用途(发号与服务发现分开)、按集群各起一套,不让一套承担全部。

### 8.4 其他「加机器没用」的单主(不在上面四类里)

- Redis 控制域:登录令牌、会话、排队现在都在一个主节点上 → 按用途和 zone 拆([玩家态 Redis 集群化](./redis-player-store-cluster.md) §10.2)。
- data_service 写门禁用的 MappingRedis(映射提案 §15.4 保留)。
- 玩家态 Redis 集群化:存档相关批次(父快照、补投索引与补投器、子表缓存与 go/db 句柄)作废;位置、会话、战斗锁等协调键的分片(第 3、5、6、7 批)仍要做,规模按剩下的键重算(TiDB 决策 §9.5)。

## 9. 按预算分三档部署

| 预算 | 部署 | 怎么扩 |
|---|---|---|
| 紧 | MySQL 主从,一个实例放多个服或多个分区 | 加实例,把分区或服搬过去,搬数据要人工做 |
| 中 | 一个 TiDB 集群,从最小规模起步 | 加 TiKV、TiDB 节点,数据自动均衡 |
| 足 | 多个 TiDB 集群,按目录分配;多地域各一套 | 加集群,调整目录与分区登记 |

- 三档的代码和表结构相同,只是配置不同。MySQL 上生产也至少要主从加备份。
- **发号不依赖数据库**(§6)是切换后端的前提。

### 9.1 后端档位(2026-10-30 落码,未编译,待 Codex 验证)

`RequireTiDB` 已删,换成档位 `Backend: tidb | mysql`。不写就是 tidb,其余值启动时拒绝;**`tidb` 和 `mysql` 都能过门禁,和 `Mode` 无关**,原来「生产必须 TiDB」的门禁只剩「档位合法」[C]。

| 入口 | 档位从哪来 |
|---|---|
| chat、mail、rank、friend | yaml 顶层 `Backend` |
| data_service、route_tool | `SnapshotMySQL.Backend` |
| db | `ServerConfig.Database.Backend` |
| guild(含 assetopfix)、trade、player_inbox | `MySQL.Backend` |
| match | `GlobalMatch.Backend` |
| writer_authority、admission_recover、save_barrier_restore、password_admin、merge_zone | 环境变量 `MMORPG_SQL_BACKEND`(这些工具不读服务 yaml;`-local-mysql` 与 `LOGIN_PASSWORD_MIGRATION_ALLOW_LOCAL_MYSQL` 已删) |

`go/shared/authoritysql` 是唯一判定点:`ParseBackend`(空串 = tidb),`Open`、`OpenForMigration`、`PinCurrentSession`、`AssertCurrentSession` 收 `Backend`,零值按 tidb,非法值在碰服务器之前就拒绝。

| 档位 | 每条物理连接(含回池复用) | 业务池投入使用前 |
|---|---|---|
| tidb | 固定 RC、严格 sql_mode、锁等待,加 `tidbSettings`(`tidb_snapshot`、`tidb_read_consistency`、`tidb_txn_mode`、`tidb_read_staleness`、`tidb_low_resolution_tso`、`tidb_replica_read`、`tidb_constraint_check_in_place_pessimistic`,最后一项要求 TiDB ≥ 6.3);接到 MySQL 时 SET 失败 | `SELECT tidb_version()` + 读回全部强读变量。行为未变 |
| mysql | 先读 `VERSION()`、`@@version_comment`、`@@read_only`、`@@super_read_only`,不是 TiDB 且两个开关都是 0 才往下走;再固定 RC、严格 sql_mode、锁等待。不发任何 `tidb_*` | 再读一次上述四项加 `@@transaction_isolation`,必须 READ-COMMITTED |

- 两个版本串都看:TiDB 配了 `server-version` 时 `VERSION()` 里没有 TiDB 字样,`@@version_comment` 不受影响(friend 的 sqldialect 2026-10-13 实测)。MariaDB 和 5.7.8 之前的 MySQL 没有 `super_read_only`,读不到就拒绝。
- 只读开关逐连接核,不只启动时核一次:主从切换后同一地址可能已经是从库。复用时发现降级,这条连接被隔离,下一条新连接被拒绝,业务看到报错而不是写进从库。
- 旧 `requireTiDB=false` 分支不是「什么都不核对」:它照样固定 RC 和严格模式,只是不核对引擎。依赖这一点的只有 dev 本地 MySQL 和两组测试夹具(`storetest.NewEmptyDB`、guild 的 `openEconomyFlowDB`,`AllowMySQL=true`,MySQL 和 TiDB 端点都可能连)。已并入 mysql 档,没有保留第三种连接:夹具改为先问真实引擎再选档位,所以 `AllowMySQL` 夹具连 TiDB 时比以前多了强读固定。本地 MySQL 只要不是只读实例就不受影响。

### 9.2 TiDB 专属依赖盘点

按 `grep -E "tidb_|@@tidb|SHARD_ROW_ID|PRE_SPLIT|NONCLUSTERED|CLUSTERED|AUTO_RANDOM|FLASHBACK|cached|ALTER TABLE .* CACHE"` 逐处看过(`go`、`tools/merge_zone`,不含 `go/proto` 与测试),另查了优化器提示和共享锁语法 [C]。`AUTO_RANDOM`、`FLASHBACK` 没有命中;`cached` 命中的都是普通变量名。

| 位置 | 内容 | mysql 档 |
|---|---|---|
| `authoritysql/pool.go` | `tidbSettings`、`tidb_version()`、`@@tidb_*` 读回 | 按 Backend 不发 |
| `authoritysql/pool.go` `open` | 从 DSN 删 `tidb_*` 参数 | 两档都删,DSN 带了也发不出去 |
| `friend/internal/svc/servicecontext.go` | DSN 的 `tidb_*`、`@@tidb_*` 读回 | 按 Backend 不发 |
| `match/internal/authority/schema.go` | DSN 的 `tidb_*` | 按 Backend 不发 |
| `merge_zone/current_tidb.go` | `pinCurrentSession`、`requireCurrentSession`(原 `pinCurrentTiDB` 等) | 按 Backend;mysql 档 DSN 里任何 `tidb_*` 都删掉,读回走 `AssertCurrentSession` |
| `data_service/internal/store/route_store.go` | `@@tidb_txn_mode`、`@@GLOBAL.tidb_enable_check_constraint` | 运行时 `serverIsTiDB` 分支,MySQL 上不发 |
| `data_service/internal/store/route_tidb.go` | `route_tool tidb-prepare`:`ALTER … ATTRIBUTES`、`SPLIT TABLE`、`TIDB_PK_TYPE` | 同上,打印 SKIP 后退出 0 |
| `data_service/internal/store/schema.go` | 不改 `player_name` 主键、`snapshot_guid_nz` 生成列表达式 | 同上,走 MySQL 分支 |
| `friend/internal/data/recommend_sql.go` | 优化器提示 `INL_JOIN`、`QB_NAME` | 按 sqldialect 方言渲染,MySQL 用 `SEMIJOIN` |
| DDL(data_service、authoritysql、proto2mysql 生成的建表) | `/*T![clustered_index] …*/`、`/*T! SHARD_ROW_ID_BITS=… PRE_SPLIT_REGIONS=… */` | MySQL 当普通注释,无需处理 |
| `asset_store.go`、`asset_op_repo.go`、`route_store.go`、`cli.go` 等 | `tidb_lock_unchanged_keys`、`ALTER TABLE route_meta CACHE`、`tidb_table_cache_lease` | 只在注释和帮助文本里,不发语句 |

`serverIsTiDB` 这几处没有改成读配置:`Open` 已保证档位和真实引擎一致,运行时判引擎结果相同,而且这些函数只拿到 `*sql.DB`,改成读配置要穿透整个 store 层。遗留一点:`serverIsTiDB` 只看 `VERSION()`,TiDB 配了 `server-version` 时会判成 MySQL(`tidb-prepare` 跳过、强读核对跳过),和 `Open` 的判据不同。这是原有问题,未改。

落码时顺带修掉一处真问题:match 的 `TeamLaunchTickets`、`sqlTeamStore.read`、`readMembers` 原来在非 TiDB 上开 `READ ONLY` 事务,而写者资格门要在同一事务里锁 `writer_slot`(`FOR UPDATE` 或 `UPDATE lock_touch`),MySQL 的只读事务里这两种都报 1792 [I:读 MySQL 行为推断,未实测]。旧代码里这条路径走不到(非 TiDB 时 `ConfigureRoleAuthority` 之前就 panic),改成档位后会走到,所以三处都不再设 `ReadOnly`;tidb 档行为不变。

### 9.3 mysql 档已知差异(只记录,不改业务)

- **间隙锁**:TiDB 没有间隙锁,MySQL 有。全部池子固定 RC,RC 下基本只剩记录锁,但 match 的三处读事务显式用 RR,里面的 `FOR UPDATE` 在 MySQL 上可能带 next-key 锁;`snapshot_store` 的 `NOT EXISTS` 退路本来就靠 RR 间隙锁,只在唯一键缺失时走,mysql 档上线前必须跑过 `-migrate`。
- **共享锁**:TiDB 默认把 `FOR SHARE` 当空操作(Java 版真库测试踩过,PROGRESS 有记录),MySQL 是真锁。Go 版业务代码没有显式共享锁读,但 MySQL 撞唯一键时隐式取 S 锁,S → X 升级在 8.0.29 之前会成环,所以 guild 启动时要求 MySQL ≥ 8.0.29(`CheckServerVersion`)。
- **死锁与锁等待**:MySQL 上 1213、1205 会比 TiDB 常见。写者池的 `guardedConn` 只把 1062 当确定失败,其余一律变成不可重试的 `ErrOutcomeUnknown` 并隔离连接(`mysqlStatementPolicy`,TiDB 上同样如此),所以 `WithTxRetry` 看不到它们。这在 TiDB 上影响小,在 MySQL 上可能放大成错误率和连接抖动,要在回归里量。
- **锁权限**:MySQL 的 `SELECT … FOR UPDATE` 除 SELECT 外还要 UPDATE、DELETE 或 LOCK TABLES 之一。写者门的 `SELECT_ONLY` 锁原语在只授 SELECT 的账号上过不了,mysql 档应选 `LOCK_TOUCH`;跨 schema 的 `account_role … FOR UPDATE` 也要按 MySQL 重核授权。
- **丢数据窗口**:TiDB 多数派提交,MySQL 异步复制切主可能丢已提交事务。本批只保证连到主库,不保证切主不丢;mysql 档上生产要半同步或等价方案,切主后旧主要置只读。发号不依赖 MySQL(§6),但写权代次和玩家存档都在 SQL 里,异步复制切主丢事务时都可能回退,§5 的「不回档」承诺不成立 [I]。
- **DDL**:TiDB 在线 DDL,MySQL 的部分 ALTER 会锁表或重建表,迁移时长另算。
- **排序规则**:MySQL 默认 `utf8mb4_0900_ai_ci`,TiDB 默认 `utf8mb4_bin`。抽查建表语句,字符串键都显式写了排序规则或用 `VARBINARY`,两档比较语义相同 [C,抽查]。
- **`binlog_format`**:RC 要求 ROW,MySQL 8 默认就是。没有加启动核对,错了第一条写就会报错。

### 9.4 mysql 档要跑的真库回归

本批没有跑过任何一项,全部待 Codex。

1. 启动门禁:连主库能起;连 `read_only=1`、`super_read_only=1` 的从库拒绝;`Backend: mysql` 连 TiDB、`Backend: tidb` 连 MySQL 都拒绝;非法值拒绝。运行中把主库置只读,确认复用连接被隔离、新连接被拒绝。
2. 各服务全套迁移在 MySQL 8.0.29+ 上跑通:data_service `-migrate`、db `cmd/migrate`、guild、trade、friend、mail、rank、chat、match、player_inbox,确认 `/*T! */` 表结构和 CHECK 约束 `chk_route_shard` 生效。
3. 写者注册与排空:`writer_authority` 配 `MMORPG_SQL_BACKEND=mysql` 走注册、退休、冻结、释放;`LOCK_TOUCH` 用最小权限账号核对。
4. 既有 `AllowMySQL` 集成测试(`storetest`、guild 经济流)在 MySQL 和 TiDB 端点各跑一遍;`_tidb_test` 里锁语义相关的用例挑出来在 MySQL 上补一份:组队三处读事务(含 RR 下的 next-key 影响)、帮会成员 S → X 升级、公会并发、资产通道。
5. 全链路压测对比两档的 1213、1205、`ErrOutcomeUnknown` 比例和连接重建次数,判定 §9.3 第 3 条要不要改写者门的错误分类。
6. `merge_zone` 配 `MMORPG_SQL_BACKEND=mysql` 跑合服、撤销、审计。
7. 主从切换演练:切主期间业务报错而不写从库,切主后恢复;确认异步复制下的丢失窗口。

## 10. MMO、SLG、MOBA

| | 在线状态放在哪 | 什么时候存盘 | 怎么扩 |
|---|---|---|---|
| MMO | 场景服内存 | 关键变化立即存 + 短间隔只存脏行(§4) | 加分线、加场景实例 |
| SLG | 地图服内存 | 资产操作随资产原子落库;脏地块定时批量落库,接受有界回档 | 加世界;单张大图按州拆分片 |
| MOBA | 战斗节点内存,一局一份,崩了可以丢 | 主要在对局结算与商城购买时,幂等落库 | 战斗节点按对局装箱 |

SLG 补充:

- **也默认用 TiDB。** 世界号是天然的分区键,世界之间几乎没有跨世界事务,多集群比 MMO 好分;合服在同一集群内只改世界标签,跨集群就把整个世界作为一个单元搬。攻城热点在地图服内存里串行处理,数据库只收到定时批量落盘的脏地块。
- **单张大图要扩就拆图**,用空间分片而不是分布式事务:按州或固定区块切分,每个地块只有所在分片能写;行军越界时交接(沿用写权代次加交接);相邻分片互相同步边界一圈的只读影子,拦截与碰撞在本分片判定;客户端按视口订阅多个分片;同盟领土连通等全图规则由同盟服汇总、关键判定在提交时复核;战斗交给无状态 worker,结果回到地块所在分片后先核对状态再生效。这会推翻[会话制抽离判据](./session-extractability-mmo-slg.md) §3.2「一张图在一个赛季内不拆」,立项时先定单张图要容纳多少人,一个进程扛得住就不拆。
- **本仓 SLG 笔记里「行军与地块以 Redis 为准」**要改成「地图服内存为准,落 TiDB 或 MySQL」,Redis 只做协调。SLG 目前只有设计稿、零实现。

## 11. 跨 zone 对战

**所有 zone 之间都能互相打。** 玩家不搬家:任何 zone 的 scene 都能按目录找到玩家所在的集群并加载;同一时刻只有一个 scene 持有写权,换 zone 就是一次交接(§3.3)。三种打法都有路径:

1. 传送到对方 zone 的地图,同场景 PvP(跨 zone 场景传送已落码,交接时源端等 SQL 确认 [D])。
2. 跨 zone 匹配进 battle 节点打竞技场、战场(全局排队池、全局 battle 池已有 [D])。
3. 大型跨服战场:单独起战场实例,每局结束后结果结算进各自存档。

注意:

- 玩家之间转移资产走资产通道(§3.4)。
- 跨 zone 传送今天依赖所有 zone 共享同一个 Redis 里的父快照(CZ-2);存档移出 Redis 后改成目标 scene 从 TiDB 加载,不再要求共享这个 Redis(TiDB 决策 §9.5)。
- 跨地域对打:数据没问题,存盘与加载跨洋只是慢一点;实时 PvP 受物理延迟限制,建议放在专门的战场实例,选双方延迟都能接受的位置。

## 12. 待拍板与待补

| # | 事项 | 状态 |
|---|---|---|
| 1 | 存盘路径:直接提交 TiDB | 按用户 10-30「开始做」推进;要保留 Kafka 需重写 §3 |
| 2 | ID 值是否保留最高位为 0 | 用户 10-30 定 64 位;默认只用低 63 位写(§6.1),用户未否决,发第一个正式号之前还能改。打散用公开标准算法,不需要拍板常量 |
| 3 | 合服围栏去留 | 已定:去掉,改别名合服(§7.1)。2026-10-30 已落码,未编译 |
| 4 | 存盘服务放 go/db 还是 data_service | 已定:放 go/db(用户 10-30) |
| 5 | 热行字段与分组、立即存盘事件清单、短间隔初值 | 第 1 批规格里定,间隔压测后定 |
| 6 | 战斗结算原件新路径的覆盖范围 | 读代码 [I]:每局结果都带格式版本 1 提交 match SQL 并进收件箱,同时仍写 `battle:settlement:pending`,scene 按 battle_id 幂等,两条路径并存。真库核实之前旧路径不删(TiDB 决策 §9.3),本轮没动,是「玩家数据不进 Redis」剩下的一项 |
| 7 | MySQL 档何时实现 | 2026-10-30 已落码,未编译(§9.1~§9.4);生产 mysql 档要 3 个 Kafka broker(历练结果 topic 的 RF3 门禁不放开),与省钱档有冲突,待用户定 |
| 8 | 发号与多集群的正确性评审 | 发号部分 10-30 已补,否决了 v1.3,按 v1.4 重写(§6);多集群部分落 §7、§8.2 的代码前补做 |
| 9 | 第二套 etcd | 按 §6.3 推荐,多一个 3 节点小集群;待用户确认 |

## 13. 分批计划

每批不超过 30 个手写文件;每批开工前写落码规格并评审;每批落码后由 Codex 编译、测试(AGENTS §10.1);所有批次合计粗估 250~350 个手写文件 [I]。

**阶段 A:上线前,单个 TiDB 集群**

| 批 | 内容 | 依赖 |
|---|---|---|
| A0 | 本文;第 1 批规格:存盘 RPC 协议、写权行与整笔序号、拆行后的表结构、加载与交接 | — |
| A1 | 写权进 TiDB + 存盘 RPC 服务端(Go)。**代码已写,未编译,待 Codex 验证** | A0;映射提案 §15 先落码(同批文件相交,与「D1 剩余收尾交接」会话对齐) |
| A2 | 玩家表拆行:proto 源、生成器、建表(生成由 Codex 执行,AGENTS §4)。**代码已写,未编译,待 Codex 验证** | A0 |
| A3 | C++ 存盘改走新 RPC;关键变化立即存;定时只存脏行。**代码已写,未编译,待 Codex 验证** | A1、A2 |
| A4 | 加载与交接改 TiDB(login、scene、跨 zone 传送)。**代码已写,未编译,待 Codex 验证** | A1、A2 |
| A5 | 资产通道、战斗结算、收件箱的判据改 SQL;父快照读方改造(friend、match、login 外观补齐等,TiDB 决策 §9.4 第 7 条)。**代码已写,未编译,待 Codex 验证** | A3、A4 |
| A6 | 删除旧路径:父快照、子表缓存、存盘意图与补投、Kafka 存盘 topic 与消费者等,按 TiDB 决策 §9.3 逐项。**代码已写,未编译,待 Codex 验证** | A5 |
| A6b | 别名合服:按区解析函数收口、`LockRoleRoutes` 去掉围栏、删除 `zone_merge_fence`、改造 `merge_zone`。**代码已写,未编译,待 Codex 验证** | A6 |
| A7 | 发号器改成内存发段 + 两份预留记录(§6.3);发号时打散(调用方接口不变);删除 snowflake 回退;Lua 里 ID 改字符串的审计;单调依赖修正;第二套 etcd 部署与恢复 runbook。**代码已写,未编译,待 Codex 验证** | 无 |
| A7c | 自增主键改由应用发号;battle、challenge、team 的主键从 snowflake 迁到号段。**代码已写,未编译,待 Codex 验证**。etcd 运维补齐(钉镜像、配额、碎片整理)这次没做:[etcd 维护手册](../ops/etcd-maintenance-runbook.md) §8 要求 C++ etcd 流重建(P0-D)实跑通过、负责人许可后才能进 P0-C 阶段 2,P0-D 实跑还缺,排在它之后 | A7 |
| A7b | Kafka 清理:关闭并删除旧单节点 topic,确认后删除 `match-rating-snapshot`、`game-events`,按剩余 topic 重算分区与磁盘预算(§3.5)。**代码已写,未编译,待 Codex 验证** | A6 |
| A8 | 单行热点清理(§8.1 第 2 条)。**代码已写,未编译,待 Codex 验证** | 合服围栏一项等 §12 第 3 项 |
| A9 | 后端档位化(§9)。**代码已写,未编译,待 Codex 验证** | — |

**阶段 B:压测**——TiDB 写入与事务数、PD 时间戳吞吐、冷读与热加载 P99、Redis、Kafka 的实际上限,按 AGENTS §6 出对比表,据此定短间隔与集群数。

**阶段 C:上线后**——拆跨服务同事务耦合 → 目录跨集群 → 多集群 → 分区搬迁工具 → 多地域;SLG 立项时再做拆图。

## 14. Java 版(AGENTS §12)

- Java 版的存档与写权代次在 MySQL:`PlayerStore` 的 `claimOwnership`、`handOffOwnership` 在一个 SQL 事务里完成认领与移交(`D:\luyuan\xuanming-server-mmo\game-player-store\src\main\java\com\game\player\store\PlayerStore.java` [C]),与本方案 §3.2、§3.3 的形状一致;存盘是进程内有界队列加重试的异步执行([D] [玩家态 Redis 集群化](./redis-player-store-cluster.md) §15)。
- 两版共享的客户端契约里,本方案会动到的是 ID 位宽与精度(§6),届时两版同批改。
- 本文只改文档,Java 版无对应改动(对应 Java 版本 0.1.0-SNAPSHOT);各批落码时逐项核对 Java 是否已有等价实现,结论为不适用或已有等价时只写回本仓文档。

---

## 附录 A:会话讨论记录(关键发现、比较过的方案、更正过的说法)

本附录记录「玩家数据存储方案」会话里正文没有展开的讨论,供以后追溯「为什么这么定」。数字除注明外都是推算 [I]。

### A.1 最初的问题与前提

- 用户的问题(10-23):「玩家数据的存储读取不是已经上了 TiDB 吗,还要不要用 Redis 存玩家数据?」
- 核对结果(10-23 当时):「上了 TiDB」只是 SQL 落地层要求连 TiDB——go/db 等服务的配置要求 TiDB,本机全球同服验收环境用的是 TiDB;K8s 部署脚本要求传入 TiDB 地址,但仓库里没有 TiDB 集群清单 [C]。存盘链本身没变:scene 先写 Redis 父快照,再经 Kafka 由 go/db 写进 SQL。

### A.2 旧存盘链里 Redis 的真实角色(为什么不能直接删)

- **Redis 是存盘链上第一个持久点,不只是缓存。** C++ 一次存盘是一段 4 键 Lua:校验 `owner_epoch`、写父快照(首次存盘后不带 TTL)、写存盘屏障的 pending、写两条完整的 DBTask 意图和补投索引;Lua 成功之后才发 Kafka。scene 崩溃或 Kafka 丢消息时,go/db 的 saveoutbox 从 Redis 取出意图补投(`redis_client.h` 的 `kSaveWithCommitBarrierLuaScript`、`go/db/internal/saveoutbox` [C])。
- **go/db 每条写任务约 11 次 Redis 往返**:排序锁、游标、已落库代次、落点读取、令牌发布脚本(子表缓存 + 资产证据 + 两表确认)等([存盘管线瓶颈](./db-task-pipeline-bottlenecks.md) §6 [D])。
- **10-17 起加载只在 SQL 已追平时才用父快照**(`parent_commit_barrier.go` 的 `route()` [C])。所以离线玩家的父快照只是已提交 SQL 的副本,却是 Redis 常驻内存里最大的一项。
- 所以「不要 Redis 存玩家数据」不是删一层缓存,而是要给提交点、写权核对、在途判断找新的承载——本文 §3 的答案是全部放进一个 TiDB 事务。

### A.3 当时比较过的三种方案与评审结论

| 方案 | 内容 | 两位评审(正确性 / 容量运维)的结论 |
|---|---|---|
| A 保持 | 父快照常驻 + 子表缓存,Redis 上 Cluster | 不推荐:内存随累计角色数无上限增长(30 天 2~6 TB,主节点数由内存决定,约 50~250 个,而按读写量只需 18~56 个);它声称的「游戏不受 TiDB 时延影响」大半已不成立(见 A.4) |
| B 收窄 | TiDB 为真源,Redis 只留在线玩家的父快照与控制态,离线卸载,子表缓存停写 | 当时两位评审都推荐的开服前路线;收益主要在内存,时延和耦合几乎没改善 |
| C 移出 | 存档只在 TiDB,存盘同步提交,写权进 TiDB | 终态一致性最强(Redis 回退导致代次倒退这一整类问题消失,Java 版已是这个形状);当时的稿子有缺陷、缺实测,评审建议作为上线后候选 |

用户 10-24 定「玩家数据不进 Redis」,方向就是 C。本文按评审意见补上了 C 的缺陷:整笔序号整笔比较(§3.2)、自动放弃读写权行(§3.4)、移交带令牌(§3.3)、没有旁路(§3.2);实测要求见 §13 阶段 B。

### A.4 评审中的其他发现

- **子表缓存没有生产读者**,只剩调试工具在读,却占 0.6~1.2 TB、每秒 8.9~22.2 万次写([共享 Redis 按用途拆分](./shared-redis-split-by-purpose.md) §19)。
- **战斗后排下一局要等两表 SQL 确认**:战斗锁 `battle:lock` 保留到双表确认后销账才删,最长 180 秒(`player_battle.h` 的 `kSettlementLockHoldSec` [C]);match 见到锁就拒绝排队。`player_battle.cpp` 里「快路径把它压到一次 Redis 往返」的注释,评审认为已过时。收件箱确认、跨节点交接同样要等 SQL 确认。
- **guild 资产通道缺省按 Redis 快照终结**:`AssetOp.RequireSQLCommit: false` 是有意的灰度缺省(`go/guild/etc/guild.yaml` 注释写明了打开的前提 [C]);新路径下这一档不再存在(§3.4)。评审另指出 [存盘围栏](./db-write-fencing-mysql.md) 里「资产成功只由 TiDB 强读证据决定」的说法与代码不符,证据其实是 go/db 发布到 Redis 的投影。
- **SQL 行内围栏只挡一类旧写**:代次更小的写判 stale、同代次序号更小判 older;代次更大就整行覆盖,行不存在直接插入,不核对内容(`player_row_fence.go` [C])。所以单靠旧的行围栏挡不住所有迟到写,本文改为写权行加整笔序号。

### A.5 「Redis 不也会丢吗」

- Redis 默认每秒刷一次盘,崩溃丢约 1 秒;主从是异步复制,切主时可能丢掉已经确认过的写(K8s 的 `redis` / `redis-ha` 只配了 `--appendonly yes` [C])。
- TiDB 三副本多数派提交,单台机器坏了不丢已提交的数据。
- 旧方案只在「TiDB 故障、Redis 正常、scene 又崩溃」这种双重故障下多保住一点进度,代价是平时多出 Redis 丢数据和回退的问题。所以「存储挂了 + 进程崩」是任何存储都有的问题,直接提交 TiDB 不比原来差。

### A.6 SLG 为什么不直接用 Redis 当主存储

- **快靠的是地图服内存,不是 Redis**:Redis 在另一台机器上,每次读写多一次网络往返;它在 SLG 里唯一的作用是崩溃后从哪里恢复。
- **放 Redis 的问题**:会丢、会回退(同 A.5);查不了(同盟全部领地、州城归属、战报历史、客服流水都要按条件查);跨对象改动没有跨 slot 原子(攻城易主、同盟仓库、交易);备份只有整实例快照,不能按时间点恢复,赛季归档不顺手。
- **成本对 SLG 不是主要理由**:单服几千到一万人,整服数据几 GB 量级 [I]。「Redis 太贵」在 MMO(全量常驻 TB 级)成立,在 SLG 不成立。
- **想要 KV 形态时**:落盘的 Redis 兼容存储(Apache Kvrocks、Pika、Tendis)数据在磁盘上,成本和持久性好很多,但查询和跨分片事务的问题仍在。
- 本仓 SLG 笔记的分工本来就是「MySQL 持久化,Redis 放在线状态、排行榜、热缓存」(`docs/notes/slg/slg-interview-qa.md` [D]),只有行军与地块的检查点放了 Redis,按本文改为落 TiDB 或 MySQL。

### A.7 TiDB 与业界实践的外部资料([W],子代理检索,主会话未逐条复核)

- TiDB v8.5 官方 Sysbench(小行):主键点查 P95 约 1.9~2.6 ms,单行 UPDATE P95 约 10~13 ms(https://docs.pingcap.com/tidbcloud/v8.5-performance-benchmarking-with-sysbench)。官方没有几十 KB 大行的公开数据,要自测。
- 非聚簇主键的点查要先读索引再读行,多一次 KV 读(https://docs.pingcap.com/tidb/stable/clustered-indexes/)。两张玩家表同一事务写,预计不能走 1PC,可以走 async commit(https://docs.pingcap.com/tidb/stable/system-variables/)。
- 大于 32 KiB 的值进 Titan(v7.6 起新集群默认开),写放大更低、约 2 倍空间(https://docs.pingcap.com/tidb/stable/titan-overview/)。
- 缓存表、Stale Read、Follower Read 都不适合「每名玩家一行、在线时频繁覆盖写」的存档(https://docs.pingcap.com/tidb/stable/cached-tables/、https://docs.pingcap.com/tidb/stable/stale-read/、https://docs.pingcap.com/tidb/stable/follower-read/)。官方文档没有要求、也没有禁止在 TiDB 前加 Redis。
- 业界实践:Roblox 官方建议会话期间以服务器内存为准、定期与离开时写回,并用会话锁防旧写(https://create.roblox.com/docs/cloud-services/data-stores/player-data-purchasing);Halo 4/5 的 Orleans 以内存对象为准、由对象方法持久化(https://www.odbms.org/blog/2016/02/orleans-the-technology-behind-xbox-halo4-and-halo5-interview-with-phil-bernstein/);腾讯 TcaplusDB 把缓存与持久化合一(https://www.tencentcloud.com/document/product/1016/30267);Pokémon GO 以 Spanner 作主事务库(https://cloud.google.com/blog/topics/developers-practitioners/how-pok%C3%A9mon-go-scales-millions-requests)。没有查到以 TiDB 作大型 MMO 唯一存档库的具名公开案例。

### A.8 讨论中更正或推翻过的说法

| 原来的说法 | 更正 |
|---|---|
| Redis 在存盘链里只是缓存 | 旧链里它是第一持久点(A.2) |
| write-behind 让游戏不受 TiDB、go/db、Kafka 时延影响 | 战斗再排队、收件箱确认、交接都要等 SQL 确认,解耦大半已不存在(A.4) |
| player_id 带分区位 [分区 16][代号 3][打散 34] | 用户 10-30 否决:ID 不带任何含义,路由用落点目录(§6、§7) |
| SLG 默认用 MySQL | 用户要求所有都水平扩展,改为默认 TiDB,MySQL 作省钱档(§9、§10) |
| owner_epoch 和 handoff 标记都搬进 TiDB | 写权以 TiDB 为准;handoff 标记和 owner_epoch 投影仍在 Redis 做协调(§2.1、§3.2) |
| 8 个服务写死 `RequireTiDB: true` | 7 个写了 true,trade 缺省要求 TiDB,friend 本地配置为 false(§9) |
| 开服前先按收窄方案,彻底移出留到上线后 | 用户 10-24 定玩家数据不进 Redis、10-30 开工,改为上线前直接按「存档只在 TiDB」落地(§3、§13) |
| 玩家与可见 ID 小于 2^53、内部小于 2^63;后来又建议统一 52 位 | 用户 10-30 定 64 位,先不考虑 Lua、JavaScript、JSON(§6.1) |
| 打散要自定 Feistel 常量,定了不能改,需要拍板 | 用公开标准混合算法,没有要我们定的常量;唯一性来自发号器,打散只解决热点(§6.1) |
| C++ 号段客户端改接 etcd | 调用方不改,只换发号器背后的计数器(§6.1) |
| 合服围栏先从高频路径拿掉,只在建角时检查 | 上线前不会合服,不做中间方案,新存盘路径落地后直接改别名合服(§7.1) |
| 防 etcd 回档靠领块水位备份,回档后丢整块(v1) | 否决 |
| 每次发段带时间下限,水位只做探测(v1.3) | 评审否决:时钟跑快过加 etcd 回退会重号,时钟门禁会停全服发号。改为两份预留记录(§6.3) |
| 地域之间靠全局下发不重叠的大块 | 改为计数器最高 6 位地域码,由发号器配置并与存储核对,客户端不配(§6.1) |
