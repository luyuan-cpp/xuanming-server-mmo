# 全部 Redis 上 Cluster:控制集群与各用途的分片设计(2026-11-10)

状态:**设计稿,未落码。** 负责人 2026-11-10 定:「我要分布式水平扩展」。服务器里每一套 Redis 都要能靠加主节点扩写入。按 zone 组分单主实例(排队 `redis-queue-g<N>`、控制域 `redis-ctl-g<N>`)和核心单主(`redis` / `redis-ha`)都不算完成。登记为[缺口清单](../handoff/server-gap-audit-20261101.md)第 34 条。

依据:2026-11-10 三路只读盘点(控制域;排队、gate 标记与网关限流;核心其余用途),行号以当天工作区为准,落码时按函数名复核。证据等级:[C] 读代码或配置确认;[D] 文档记载;[I] 推算;[M] 实测。本稿没有 [M]。

关联文档:
- 玩家域与登录域的集群化见[玩家态 Redis 集群化](redis-player-store-cluster.md),本稿不重复,只复用它的客户端与部署能力。
- [控制面按 zone 化](control-plane-zone-scoping-20261105.md) §4.3 的不变量「zone z 的控制域键只由 zone z 的 scene_manager 读写」照用,它是按 zone 打 tag 的前提;该稿 §4.1 的选型与 §4.6 的部署形态由本稿取代。

## 0. 结论(先读)

1. **生产只剩三套 Redis Cluster。**
   - 玩家域集群 `redis-player`:玩家域加登录域,沿用已有设计;代码已落,还没有任何运行证据。
   - 新建控制集群 `redis-control`:除玩家域、登录域和 match 私有键之外的全部用途。
   - match 私有集群 `redis-match-cluster`:已经是 Cluster,只统一版本。
   - 核心 `redis` / `redis-ha`、`redis-queue-g<N>`、`redis-ctl-g<N>` 退役,遗留的 `redis-rank.yaml` 删除。本机开发仍用单实例,但客户端走与集群相同的路径和守卫(standalone 模式)。
2. **控制集群里,同一个多键操作的键共用一个 hash tag。** scene_manager 控制域、login 排队、gate 标记各自按 zone 打 tag,guild 缓存按帮或按玩家打,player_locator 的控制键共用一个 tag。盘点确认没有跨 zone、跨 tag 的原子操作需要拆(§3)。单键命令不强制带 tag。
3. **扩容 = 加主节点 + 原子迁 slot**(Redis 8.10.x 的 `CLUSTER MIGRATION`)。zone、帮、玩家与唤醒分片随 slot 散到各主节点,数据和读写一起搬过去。**单个 zone 的控制域与排队各自仍落在一个槽上**,一个 zone 的上限是一个主节点。这是按 zone 原子提交的代价,余量与到顶后的路线见 §6。
4. **唤醒频道改分片 Pub/Sub。** `assetop:wake` 按 `player_id % 64` 拆成 64 个频道,用 `SPUBLISH` 发布,分片规则与资产通道的工作分片相同。订阅侧的扇出今天随 zone 数增长(每个 zone 的 data_service 都订全部唤醒)。要等缺口第 25 条按分片分工、每个副本只订自己的分片,扇出才不随 zone 数增长(§4.6)。
5. **客户端统一。**
   - Go:非玩家域句柄全部改成 `redis.UniversalClient`,用与玩家域同一个构造器(键守卫 + 结果未知不重放)。go-zero 的 Redis 句柄全部退役:go-zero 写死 `MaxRetries = 3`,没有配置项(§5.1)。这同时收掉[断线重连清单](infra-reconnect-overview.md)的 R8。
   - C++:scene 的两个 `RedisManager` 句柄合成一个 `RedisClusterClient`。gate、battle 不再连 Redis,它们今天只发 AUTH 和存活 PING。
   - Java 网关:限流拆成两段单键脚本,Lettuce 改用集群客户端。
6. **迁移。** 项目未上线,键改名直接以新键起步,不做双读双写。上线后再改键形必须停写切换(§8)。
7. **不做。**
   - 控制域按场景号分片:脚本把场景键和 zone 级集合绑在一起,拆开就丢原子性。
   - 单 zone 排队再分片:代价见 §6.2,先用服务端按名次拉长轮询间隔。
   - 接入代理:理由同[玩家域设计](redis-player-store-cluster.md) §6.4。

## 1. 需求与范围

- 控制面设计稿 §4.1 当时选「按 zone 组分实例、不上 Cluster」,§5 写了「以后一个组装不下再按 zone 改 tag 上 Cluster」。本稿就是做这一步,时间提前到首次上线之前。
- 范围是服务端对 Redis 的全部使用:C++ 的 gate、scene、battle,各 Go 服务,本仓 Java 网关,运维与压测脚本,K8s 清单。
- 玩家域与登录域的键形不在本稿。match 私有键只统一版本;观战大厅的三把全局键单独协调(§4.9)。
- Java 版服务器在独立仓,本稿不写它的代码,只登记待做(§11)。

## 2. 现状(2026-11-10 盘点)

### 2.1 实例与内容

| 实例 | 形态 | 放什么 |
|---|---|---|
| 核心 `redis` | 全服一个主;非 dev 档 1 主 2 从 + 哨兵 + haproxy | gate 排空 / 停写标记;落点唤醒 `assetop:wake` 与各服务的去重键;trade、guild、player_inbox、player_locator 的选主锁;guild 私有键(缓存、帮会榜、维护锁);friend 私有键(K8s 刻意回落到这里,`k8s_deploy.ps1` 自己写着「回落只是过渡」);player_locator 的 gate 索引控制键;data_service 名字缓存 |
| `redis-queue-g<N>` | 每个 zone 组一个主 | login 排队、准入、放行锁;Java 网关限流桶。**缺省不拆**:`-RedisSplit` 缺省解析为 none,不带 `queue` 时这些键在核心 |
| `redis-ctl-g<N>` | 每个 zone 组一个主 | scene_manager 控制域全部键;C++ scene 读分线目录、神秘商人,并做场景归属对账 |
| `redis-player` | Redis Cluster 8.10.2 | 玩家域与登录域(已有设计) |
| `redis-match-cluster` | Redis Cluster 3 主 3 从,Redis 7.2 | match 私有键(观战索引、档位与配额) |

### 2.2 客户端

- login 的 `Node.RedisClient`、`QueueRedis`、SaToken 都是 `redis.NewClient`;`Queue`、`Dispatcher`、`DrainMarkCache` 与 gatedrain 各函数的签名写死 `*redis.Client`(`go/login/internal/svc/servicecontext.go:156`、`:282-296`)[C]。
- scene_manager 控制域与 `CoreRedis`、friend、data_service 名字缓存是 go-zero 句柄,`Type: node` [C]。
- trade、guild、player_inbox、player_locator、data_service `SharedRedis` 是 `redis.NewClient` [C]。
- Java 网关限流自建 Lettuce 单机 `RedisClient`,不读 Spring 的集群配置(`RateLimitConfig.java:75-126`)[C]。
- C++:`tlsRedis` 与 `tlsSceneControlRedis` 都是单地址 hiredis 的 `RedisManager`,不跟随 MOVED [C]。
  - `tlsRedis` 上唯一的业务命令是 scene 的 `PUBLISH assetop:wake`(`player_landing.cpp:255-271`)。
  - gate、battle 只建连,之后只有 `RedisManager` 自己发的 AUTH 和每 5 秒一次的存活 PING(`node.cpp:1149`、`:1161`,`redis_manager.cpp:269`)[C]。

### 2.3 只换地址会怎样

不改键形、只把地址指向集群,结果是报错或者静默失效 [C]:

| 用途 | 结果 |
|---|---|
| login 入队 | TxPipeline 跨 3 个槽,go-redis 直接回 `ErrCrossSlot`,一个人都入不了队 |
| login 轮询与放行 | `lookupScript`、`admitEntryScript` 各 4 个键,跨槽执行失败 |
| gate 标记 | login 与 scene_manager 的 MGET 跨槽,两边都 fail-open:不再过滤排空 / 停写的 gate,只打 ERROR |
| Java 网关限流 | 两桶脚本跨槽,fail-open:不再限流 |
| scene_manager 控制域 | 绝大多数脚本把场景键(不带 zone)与 zone 级集合绑在一起,跨槽失败 |
| C++ 归属对账 | MGET holder 加最多 256 个场景键,跨槽 |
| guild | 缓存两键 Lua、重建榜的 MULTI + RENAME 跨槽;SCAN 只扫一个节点 |
| player_locator | 5 段 Lua 跨 2~3 把不带 tag 的键 |
| data_service 名字缓存 | 一次 MGET 最多 500 个键,跨槽报 CROSSSLOT |

所以必须先改键形、换客户端,不能只换地址。

## 3. 键形:控制集群的 hash tag

### 3.1 规则

- **tag 形状**是 `{<族前缀>:<身份>}`,族前缀登记在一张表里(§3.2)。Go `shared/redisslot/control_keys.go` 与 C++ `redis_slot.h` 各一份,金样测试保证逐字一致。
- **域的接缝**可以机械检查:
  - 玩家域与登录域的 tag(`{<十进制 player_id>}`、`{<SlotTag>}`、`{<gate>:<inc>}`)不得出现在控制集群;
  - 控制集群登记过的族前缀不得出现在玩家域;
  - 两侧客户端的守卫在发送前检查(§5)。今天两种语言都用「有没有 tag」区分两个域,这条判据随本稿改成「tag 是什么形状」。
- **多键命令必须同 tag。** Lua 的 KEYS,多键 MGET / DEL / EXISTS,MULTI 里的命令,RENAME 的两端,都在此列。单键命令可以不带 tag。
- **脚本不得现拼 KEYS 之外的键。** 盘点里生产代码没有;guild 的 `trialLobbyCreateScript` 只服务旧测试夹具(`trial_lobby_repo.go:118`),随批删除或改形。
- **tag 写在原来 zone 或实体号的位置。** 例如:
  - `node:zone:<z>:<n>:scenes` → `node:{ctl:<z>}:<n>:scenes`;
  - 场景键补上 zone:`scene:<id>:node` → `scene:{ctl:<z>}:<id>:node`。

  每个族的新旧键形逐条列在各批的金样表里,跨语言的键在同一批里改两种语言。

### 3.2 族前缀

| 前缀 | 身份 | 键族 | 读写方 |
|---|---|---|---|
| `ctl` | zone | scene_manager 控制域全部键(§4.1) | scene_manager;C++ scene 只读分线目录、神秘商人,并做归属对账 |
| `q` | zone | login 排队 `queue`、`admitted:zset`、`admit:<qid>`、`queue:meta:<qid>`、`queue:token:<tok>`,放行锁 `dispatcher:lock:login_queue` | login |
| `gm` | zone | gate 标记 `draining` / `drained` / `stale` | `k8s_gate_drain.ps1` 与 login 写;login、scene_manager 读 |
| `aw` | `player_id % 64` | 唤醒频道 `assetop:wake:{aw:<k>}` | C++ scene SPUBLISH;guild、trade、data_service SSUBSCRIBE |
| `g` | guild_id | `guild:v2:<g>` 与它的 generation 键 | guild |
| `pg` | player_id | `player_guild:v2:<p>` 与它的 generation 键 | guild |
| `gr` | zone | 帮会榜 `guild_rank:zone:<z>` 与同区的版本 HASH `guild_rank:version:<z>`、墓碑 `guild_rank:tombstone:<z>`,维护锁(改按 zone) | guild |
| `pl` | 固定 `gix` | player_locator 的 gate 索引控制键 7 把(`player:gate_index:ready / gen / last_audit / audit_cooldown / pass / pass_seq / pass_progress`) | player_locator |
| `f`、`rq`、`directory`、`apply` | player_id | friend 私有键、guild 申请配额(今天已是这个形状,不改名) | friend、guild |

不带 tag 的单键族:
- 选主锁:trade、guild、player_inbox、player_locator;
- 唤醒去重键 `assetop:wake:<svc>:<pid>`;
- guild 的申请推送、巡检租约、清理水位;
- 名字缓存 `player:name:<id>`;
- 网关限流 `rl:ip:<ip>`、`rl:zone:<z>`。

同一个 zone 的 `ctl`、`q`、`gm` 三族分属三个 tag,可以落在不同主节点上。它们之间没有原子操作,不需要同槽。

### 3.3 只给场景号的入口

按 zone 打 tag 之后,拼场景键必须知道 zone。好几个入口只拿得到场景号(`enterscenelogic.go:333-335`、`:365-368`、`:1560-1567`;`leavescenelogic.go:381`;`scene_membership.go:161-171`;`createscenelogic.go:217-268`、`:554-577`;`instance_lifecycle.go:367`;`world_rebalance.go:393`;`scene_teardown_job.go:141`)[C]。

- **一律用本进程的 zone**,与 §4.3 的不变量一致:同组的键「读得到也不读」,行为不随组的划分而变。
- **别的 zone 的场景读成「不存在」。** 今天同组外 zone 的场景能解析,这本来就是不变量禁止依赖的部署巧合。每个入口要核对:「不存在」与今天「是别的 zone」走同一个结局,不得走销毁或拆除。第 2a 批逐个入口补用例。
- **`foreign` 分支删掉。** 认领脚本、Agones 判定里的 `foreign` 分支(`scene_atomic.go:159-164`、`agones_room_ledger.go:109-112`)不再可达。
- **`scene:<id>:zone` 删除。** zone 已经是键地址的一部分。`removeScenePlayer` 原来用它判断「是不是本 zone」,改为看本 zone 的 `scene:{ctl:<z>}:<id>:node` 在不在。
- **源场景在别的 zone 的镜像共置**(`source_scene_id`)改回 `ErrSourceSceneGone`。共置失败的原因标签从 `zone_mismatch` 变成 `no_mapping`,指标与用例同批改。
- **CreateScene 的 `zone_id = 0` 按本进程的 zone 处理**,与 DestroyScene 的口径相同。今天它会以 zone 0 登记(`createscenelogic.go:91`),改后不存在 zone 0 的键。

### 3.4 C++ 归属对账的前提

`scene_ownership_reconciler.cpp` 每 10 秒 MGET 一批 `scene:<id>:node`,每批最多 256 个,C++ 只能用 `GetZoneId()` 打 tag。这要求「一个 scene 节点只承载本 zone 的场景」。

今天这条只由 Go 保证 [C]:
- CreateScene 拒绝外 zone;
- 按 (zone, node) 拨号;
- Agones 分配校验 zone;
- 镜像共置遇到 zone 不符会回落;
- 世界频道只铺本 zone。

一旦被打破,C++ 读到 kNoMapping,确认两次后会拆掉活场景。第 3 批在节点侧的 CreateScene 请求里加上 zone 字段(内部协议,客户端不可见),C++ 发现 zone 不是本节点的就拒绝创建。这样把前提变成两边都核,在创建时就失败关闭。

## 4. 逐用途改法

### 4.1 scene_manager 控制域

- **键**:12 种 `scene:<id>:*`、`instance:<id>:members`、9 种 `node:zone:<z>:<n>:*`、世界频道 6 种、分线目录与线号、神秘商人、Agones 名额、拆除任务、死节点收尾表、`world_init` 锁、选主锁、etcd 修订号水位与纪元,全部改 `{ctl:<z>}`。
  - 9 种节点级键按 11-11 工作区的代码是 `scenes`、`scene_count`、`scene_node_type`、`members`、`death_at`、`reconcile_scenes`、`reconcile_instance`、`holder`、`holder_epoch`;其中 `holder`、`holder_epoch` 在 `node_holder.go`,写本稿时还没提交。
  - 修订号水位 `scene_manager:etcd_revision_seen:zone:<z>`、纪元 `scene_manager:etcd_epoch:zone:<z>` 是 11-11 为节点号持有者加的(缺口第 33 条,`node_holder.go`)。它们和 `holder`、`holder_epoch` 出现在同一段脚本里,必须同 tag。
  - 拼键约 185 处:177 次私有构造函数调用,加 8 处裸串或裸 Sprintf,散在约 14 个文件里 [C]。
  - 第 2a 批先把构造集中到一个键包,再统一改形。
  - 四个跨语言键(`node:…:holder`、`scene:…:node`、分线目录、神秘商人)在同一批里改两种语言。
- **原子操作**:25 段 Lua、3 处 MGET、2 处 pipeline,以及 Lua 里的 SUNIONSTORE、多键 DEL / EXISTS,全部只碰一个 zone 的键;没有 MULTI、WATCH、SCAN、阻塞命令或 Pub/Sub [C]。改名之后自然同槽,脚本逻辑不动。
  - 11-10 盘点之后新增或改成多键的脚本有 4 段,同样只碰一个 zone 的键:
    - `node_holder.go` 的持有者发布 `luaPublishNodeHolder`,三键:`holder`、`holder_epoch`、纪元;
    - 换纪元 `luaStartNewEtcdEpoch`,两键:水位、纪元;
    - 抬水位 `luaRaiseEtcdRevisionSeen`,单键;
    - `reentry_barrier.go` 的按实例清死亡标记 `clearNodeDeathForInstanceScript`,两键:`death_at`、`reconcile_instance`(断线重连 R15)。
  - 控制面设计稿 §4.4 漏了约 10 段脚本,§4.2 漏了 `teardown`、`registration`、`placement_gen`、两种 `*_receipt:<token>`、`agones_rooms:…:holds`、`scene_teardown:…:pending`、`placement_seq`,还列了已经不存在的 `scene:<id>:claim`。第 2a 批的金样表以代码为准。
- **跨键先后**:`death_at` 必须先于「摘出负载集」落地(`load_reporter.go:316-340`)。同槽才在同一条复制流里,切主时不会留下「已摘出、death_at 却丢了」的组合。按 zone 打 tag 正好满足。
- **pipeline**:`load_refresh.go` 的整批判定是按单节点 pipeline 写的(`:46-54`、`:312-356`)。改名后整批同槽、落在同一个节点,语义不变,第 2b 批核对并加断言。
- **键前缀约定**:合服的 `node:zone:{S}:*`(`reentry_barrier.go:250-251`),以及 `orphanKeyPrefix` / `parseWorldChannelsKey`(`orphan_cleanup.go:48`、`:109`),随键形改。
- **句柄**:go-zero → `UniversalClient`(§5.1),约 26 个文件的调用改写。选主 `leader.NewGoZeroStore` 换成 UniversalClient 版的 store(§4.7)。
- **要重写的契约测试**:
  - `TestControlDomainKeysCarryNoHashTag`;
  - `requireOneDomain` 及用它的 `TestEnterAndLeaveScriptsNeverMixDomains`、`TestPlayerDomainAndControlDomainCanLiveOnSeparateInstances`;
  - `TestControlZoneKeysContract_*`、`TestLeaderLockKeyPerZone`、`TestNodeHolderKeyShape`、两条跨语言键契约、`TestParseWorldChannelsKey`;
  - `go/data_service/internal/faultdrill/ownership_fault_fullstack_test.go` 里裸拼的 `node:zone:%d:%s:death_at`。

### 4.2 C++ scene、gate、battle

- **scene 的句柄**:`tlsRedis` 与 `tlsSceneControlRedis` 合成一个 `tlsControlRedis`(`RedisClusterClient`,控制集群键策略,§5.2)。用它的地方:
  - `player_scene.cpp` 的分线目录 GET;
  - `player_mystery_merchant.cpp` 的神秘商人 HGET;
  - 归属对账的 MGET,tag `{ctl:<GetZoneId()>}`;
  - 唤醒 SPUBLISH,经 `commandForKey` 按频道路由(§4.6)。
- **命令表**:`redis_command_route.cpp` 登记 `SPUBLISH`。今天 `PUBLISH` 也没登记,走的是 kUnknownCommand,集群客户端会当场拒绝。
- **配置**:node-config 的 `zoneredis` 与 `scene_control_redis` 合成 `control_redis`(种子写 IP 字面量,口令走环境变量)。
- **gate、battle**:删掉 Redis 连接、`zoneredis` 配置与 K8s 模板里的口令环境变量。
- **CreateScene 校验 zone**:见 §3.4。
- **要改的契约测试**:`SceneOwnershipRules.KeysMatchTheSceneManagerKeys`、`PlayerRedisRouteContract.ControlDomainStaysOffThePlayerHandle`、`RedisCommandRouteTest.*`、`RedisClusterClientTest.AllKeysPolicyKeepsUntaggedKeysOffTheWire`。

### 4.3 login 排队与准入

- **键**:改 `{q:<z>}`。改完之后:
  - 入队的 MULTI(3 键)、`lookupScript`(4 键)、`admitEntryScript`(4 键)都同槽(`go/login/internal/logic/pkg/loginqueue/queue.go:321-343`、`:502-532`、`:917-940`)[C];
  - 轮询从验过签的令牌里取 zone;
  - consume 的 HMGET 要把 zone 传进去。
- **类型**:`*redis.Client` 改 `redis.UniversalClient`。
- **放行的 pipeline**:今天 `PopAdmit` 把放行脚本放进普通 pipeline(`queue.go:992`)。集群客户端的 pipeline 在 EOF / 超时后会整批重发,这正是守卫拒绝「pipeline 里带 EVAL」的原因。改成先按槽取主节点,再用节点客户端发 pipeline,与 owner_epoch 铸造通道的做法相同(`playerredis.MasterForKey` + `NewNodeClient`)。
- **死配置 `QueueShardCount`**:Go 代码没有任何地方读它(`config.go:596`、`login.yaml:51`)[C]。连同 `k8s_deploy.ps1` 的镜像和契约用例一起删。
- **轮询间隔**:单 zone 的热度见 §6.2。服务端按名次拉长下发的 `RetryAfterMs`(`queryqueuestatuslogic.go`)。
  **2026-11-11 已由缺口清单第 14 条提前落码(未编译,`loginqueue.RetryAfterForRank`,规则见 `login-queue-2026-05.md` §14 第 10 条),第 4 批不用再做;下面几条是当时的设计口径。**
  - 前 2000 名每 2 秒一次;之后按预计等待时间线性增长,上限 30 秒。
  - 名次与总数的定义不变。
  - 前提是客户端遵守 `RetryAfterMs`。动手前先查 Java 仓 `docs/reference/mmorpg-client-contract-*.md`,没有客户端任务授权时不读客户端仓。
  - 与「Zone 人数满的处理」会话对一下排队上限。
- **已过时的注释**:账号锁已经在 `LoginRedis` 上(`loginlogic.go:99`、`createplayerlogic.go:97`),`login.yaml:72` 与 `locker/player_locker.go:17` 的注释随批改正;`login.yaml:65` 提到的 Pub/Sub 已没有对应代码。
- **要改的测试**:
  - `loginqueue` 的 `TestPoll_EqualScoresUseQueueIDLexicographicOrder`、`TestZonesAreIndependent`、`TestPopAdmit_HoldsPlaceholderInZSetUntilAdmitTTL`、`TestLegacyAdmittedSetKey_NeverWritten`、`TestZoneDispatcherLockKey_IsPerZone`;
  - `svc` 的 `TestInitLoginQueue_*`。

### 4.4 gate 标记

- **键**:`gate:{gm:<z>}:<id>:draining|drained|stale`,按 zone 打,不按 gate 打。
  - N 台 gate 的 MGET,以及 MarkScript 检查「同 zone 其他候选」,都要求同槽;
  - 按 gate 打只能保住 draining / drained 这一对。
- **写方**都知道 gate 的 zone:`tools/scripts/k8s_gate_drain.ps1` 的 MarkScript / ReleaseScript / DEL,login 的 `EvaluateDrainingGates`,`PublishStaleMarks`。
- **读方**都是一次 MGET 一个 zone 的全部 gate:login 的 `DrainingGates`、`DrainMigrationsDue`、`DrainingGateSet`,以及 scene_manager `gate_redirect.go:202-213`(跨 zone 重定向读目标 zone 的 2N 个键)。改名后同槽。
- **运维脚本**:`k8s_gate_drain.ps1` 的 redis-cli 加 `-c`,去掉 `-n <db>`(Cluster 只有 DB 0),「不涉及跨 slot」那句注释随之改写。
- **协调**:`go/shared/gatepick/marks.go`、`gatestale.go`、`gate_stale_publisher.go` 是另一个会话正在写的新文件,还没入库。第 4 批动手前先对一下段落。
- **要改的测试**:`TestMarkKeys_Golden`、scene_manager 的 `TestGateRedisMarks_*`、`tools/scripts/tests/k8s_gate_drain.tests.ps1` 的契约字面量两条。

### 4.5 Java 网关限流

- **脚本**:按 `RedisRateLimitStore.java:30-32` 已经写好的方案,拆成两段单键脚本。
  - 先扣 IP 桶,再判区桶;区桶空时另发一条退还 IP 令牌。
  - 退还那条超时没执行的话,IP 桶多扣一个令牌,偏严,与类注释里「超时多扣」的取舍相同。
  - 不按 zone 给 IP 桶打 tag:那样 IP 桶就变成每个 zone 一个,语义变了。
- **客户端**:限流自建的 Lettuce 单机客户端改成 `RedisClusterClient`,配置加集群种子。`spring.data.redis` 只有 host / port,健康指示器用的自动配置连接也要改成集群节点配置。`ZoneHealthProbeService` 注入了 `StringRedisTemplate` 但从不使用,顺带删掉。
- **热键**:`rl:zone:<z>` 每次 `/api/login`、`/api/assign-gate` 都打一次,单键、每 zone 一个,上限是一个主节点(§6.2)。
- **要改的测试**:`AssignGateRateLimiterTest`、`RedisRateLimitStoreTest.ipAndZone_sentAsTwoKeysAndFiveArgs`、`RedisRateLimitStoreIntegrationTest`。

### 4.6 唤醒频道(Pub/Sub)

- **今天** [C]:
  - 每个 scene 进程每次进场 PUBLISH 一次 `assetop:wake`(同一实体 5 秒内合并),每次战斗解冻也发(不合并),没有订阅者也照发。
  - 2000 万 PCU 口径下约 1.1 万~7.8 万次/秒 [I](G4a §16 口径,C++ 侧没有发布计数)。
  - 订阅方是 trade 2 副本、guild 2 副本、data_service 每 zone 2 副本。每个副本对每条消息各做一次 `SET assetop:wake:<svc>:<pid> NX EX 5`,所以扇出是 4 + 2Z。
- **为什么要改**:集群里普通 PUBLISH 经集群总线广播到全部节点,不随节点数扩展。
- **改法**:
  - 发布:`SPUBLISH assetop:wake:{aw:<pid % 64>} <pid>`,64 与资产通道的工作分片(`player_id % 64`)取同一规则。只用一个分片频道会全部落在一个槽上,成为热分片。
  - 订阅:
    - 每个分片频道一个 `SSubscribe`:SSUBSCRIBE 一次订的多个频道必须同槽,go-redis 一个 PubSub 只连一个节点。
    - 改用 `ChannelWithSubscriptions`:迁槽时服务端会推来 `sunsubscribe`,今天用的 `Channel()` 会忽略这类消息(go-redis `pubsub.go:724`),迁槽后订阅就静默失聪了。收到退订要在新节点上重订。
  - 同批改 C++ 常量(`player_landing.h:44` 改成格式串)与 Go 的频道常量(`go/shared/assetopwake/wake.go:34`)。跨语言契约 `TestChannelMatchesCpp` 逐字解析 C++ 常量,也要同步改。
- **扇出是另一个问题,改了频道也解决不了。**
  - 在缺口第 25 条(资产重投按分片分工)落地之前,每个副本都得订全部 64 个分片,每条消息仍然投给 4 + 2Z 个副本,只是摊到了多个主节点上。
  - Z 取 200~445 时,每秒投递 1000 万~7000 万条 [I],摊开也扛不住。
  - 第 25 条落地后,每个副本只订自己负责的分片,每条消息只投给各服务的那一个负责副本(3 个服务),与 zone 数无关。
  - data_service 的召回不按 zone 过滤(`go/data_service/internal/svc/recall_channel.go`)[C]:每个 zone 的副本都在重投、都在订阅。第 25 条要按全体 data_service 副本分片,不能按 zone 内分片。
  - 这条依赖记在缺口清单第 34 条。今天 K8s 上 trade 缺省关、guild 在非 dev 档强制关、data_service 跟随 `-AssetOps`,所以开服时不一定全开。

### 4.7 选主锁与 shared/leader

- **集群下可用**:`go/shared/leader` 只有单键 `SET NX EX` 和单键 Lua(`leader.go:41-62`),没有 WAITAOF,也没有同连接假设 [C]。
- **缺口**:`NewGoRedisStore` 只收 `*redis.Client`(`store_goredis.go:19`),要补一个 UniversalClient 版本(`UniversalMaxCallDuration` 已有)。放在第 2b 批做,那时 scene_manager 是它的第一个调用方;第 1 批没有调用方,不提前加。shared/leader 的内部改动归断线重连会话(R7),这一处谁先动谁做,动手前互发文件清单(§13)。
- **锁键 tag**:
  - scene_manager 的锁改成 `scene_manager:leader:lock:{ctl:<z>}`,与本 zone 的控制域键同槽;
  - login 的放行锁改成 `dispatcher:lock:login_queue:{q:<z>}`。

  这样 R7 加任期号之后,能在同一段脚本里核对任期并写数据。
- **已知风险**:异步复制下切主可能丢锁,出现短暂双主。四个全局调用方都声明靠 CAS 兜底 [C],R7 会补任期号。

### 4.8 各服务私有键

- **guild**:
  - 缓存填充 / 失效 Lua 的 KEYS 是 [generation, data],两键不同槽(`guild_repo.go:215-227`)。改成 `{g:<id>}` / `{pg:<pid>}`,`guild/cmd/assetopfix` 复用同一套 Lua,同批改。
  - 帮会榜的脚本(条件写入、立墓碑、按版本摘、读榜头)每段只碰同一区的三个键:`guild_rank:zone:<z>`、`guild_rank:version:<z>`、`guild_rank:tombstone:<z>`,改成同一个 `{gr:<z>}` 即可,协议不用改。重建已不再用 MULTI 包多键 DEL 加 RENAME 临时键(缺口第 23 条,11-11 落码未编译,见 PROGRESS 2026-11-11「帮会积分写入不再抢全服锁」)。
  - SCAN `guild_rank:zone:*`(`guild_repo.go` 的 `rankZones`,重建与摘榜都用它,以及 `tools/data_consistency_check`)改成按 zone 清单逐个处理,zone 清单从 SQL 取,不扫 Redis。
  - 全服一把 `guild_rank:maintenance_lock`(11-11 起只让重建与合服折叠互斥,写分不拿)改成每 zone 一把 `guild_rank:maintenance_lock:{gr:<z>}`。
  - 全服帮会榜已由另一会话删掉(PROGRESS 2026-11-10,未编译),每 zone 的榜还在。
- **friend**:键已经是 `{f:<p>}` 等形状,两段缓存 Lua 是两键同 tag,集群安全 [C]。只换句柄:go-zero → UniversalClient,申请配额的 INCR Lua 从此不会被自动重发(R8)。
- **player_locator**:7 把控制键统一加 `{pl:gix}`,5 段 Lua(`gate_session_index.go:140`、`:161`,`index_pass.go:118`、`:134`、`:145`)就同槽了。访问是 10 秒级的低频。契约测试 `script_slots_contract_test.go` 的 `TestControlDomainKeysCarryNoHashTag` 要重写。`deploy-staging.sh`、`stress-linux-tier.sh` 的 `--scan 'player:gate_index:*'` 改成逐主节点。
- **data_service**:
  - 名字缓存的 MGET 改用 `MGetBySlot`(按槽分组,每个节点一条 pipeline),句柄由 go-zero 换成 UniversalClient;
  - `SharedRedis` 只剩唤醒订阅与去重键;
  - `player_name_logic.go:599` 的 mget 是死代码,随批删。
- **trade、player_inbox**:只有选主锁、唤醒去重这类单键,换句柄即可。
- **mail、chat、match 的旧 `Redis` 段**:在 K8s 上不对核心发命令 [C]。
  - chat 两个 go-zero 句柄没有调用方,删掉。2026-11-11 已由「Redis 登录总账审核」会话删掉,连同 `ChatRedis` 段与 `Redis.Host` 必填校验,见 PROGRESS 同日条目。
  - mail 的 `SharedRedisAddr` 与 match 的 `Redis` 段只是玩家域没写地址时的回落,开发环境留着。

### 4.9 match 私有集群

- 每局的键已带 `{<battle_id>}`,多键 DEL 同槽 [C]。
- 镜像从 7.2 升到与另外两套相同的 8.10.2 摘要,扩缩容走原子迁移。
- 观战大厅三把全局 ZSET(`spectate:battles:active`、`spectate:bcast:active`、`spectate:featured:active`)各是一个单键热点,加主节点分不开。
  - 归「全服观战架构设计」会话处理。
  - 如果它不接,做法是按 `battle_id % K` 拆成 K 份,读时合并 K 份,列在第 5 批之后。

## 5. 客户端

### 5.1 Go

- **构造器**:沿用 `shared/playerredis` 的实现(UniversalClient、单机 / 集群同一套守卫、结果未知不重放、`MGetBySlot`、`ScanEachMaster`、`MasterForKey`)。
  - 包名在第 1 批改成 `shared/clusterredis`,`playerredis` 保留类型别名,现有导入不用改;
  - 新代码从 `clusterredis` 导入。
- **两种键策略**:
  - 玩家域策略不变:所有键带玩家域或登录域的 tag;
  - 控制集群策略:多键命令同 tag 且是登记过的族前缀,单键可以不带 tag,任何命令出现玩家域 tag 形状都拒绝。
- **go-zero 句柄全部退役**:scene_manager 的 `Redis` / `CoreRedis`、friend、data_service 名字缓存、chat。
  - 原因:go-zero v1.10.0 写死 `maxRetries = 3`(`core/stores/redis/redisclientmanager.go:14`,集群版 `redisclustermanager.go:33`),没有配置项 [C];
  - 「结果未知不重放」必须由我们的构造器把节点客户端的重试关掉;
  - 只给 go-zero 改 `Type: cluster` 收不掉 R8。
- **配置**:每个服务一个 `ControlRedis` 段(字段同玩家域 `Config`:Addrs、Cluster、PasswordEnv、PoolSizePerNode、超时),一个客户端。
  - 取代 `Node.RedisClient`、`QueueRedis`、`CoreRedis`、`SharedRedis`、`RedisClient`、`PlayerLocatorRedis`、`NameCacheRedis`、go-zero 的 `Redis`、`FriendRedis`;
  - 段名不叫 `Redis`,避开 `zrpc.RpcServerConf` 的同名字段;
  - 不写 Addrs 时指向本机 `127.0.0.1:6379` 单实例,守卫与不重放照样生效。
- **订阅**:SSUBSCRIBE 的连接不占池名额,另算(§7 总账)。

### 5.2 C++

- `RedisClusterClient` 已经支持单机与集群同一条路径,同 slot 停等保序,错误归类,只放行登记过的命令 [C]。
- 本稿只加两件事:控制集群键策略(登记过的族前缀、多键同 tag),以及 `SPUBLISH` 的登记。
  - 键策略是 `RedisClusterOptions::keyDomain`(`redis_cluster::KeyDomain`),与原有的 `hashTagPolicy` 正交:`hashTagPolicy` 管「哪些键必须带 tag」,`keyDomain` 管「带着的 tag 必须是哪一种」。
  - 玩家域句柄取 `kAllKeys` + `kPlayer`:控制集群的族 tag 发过来拒绝(`wrong_domain_tag`)。控制集群句柄取 `kMultiKeyOnly` + `kControl`:玩家域的 tag 拒绝,多键命令带不同 tag 拒绝(`mixed_tags`,碰巧同 slot 也拒绝)。
  - 缺省 `kUnrestricted` 不看 tag 形状,既有调用方与用例不变。
- 每个 scene loop 线程一个 `tlsControlRedis`,每个控制集群主节点一条连接;gate、battle 不连。

### 5.3 Java 网关

Lettuce 集群客户端(限流自建的那一个,加上 Spring 自动配置的那一个)。限流脚本改成单键,见 §4.5。

## 6. 容量与扩展边界 [I]

### 6.1 加主节点的效果

- zone、帮、玩家与唤醒分片按 slot 散在各主节点上,原子迁 slot 时连数据带读写一起搬过去,每个主节点的负载约为 1/M。
- 粗估控制集群:
  - 每 zone 的控制域在登录潮时每秒几千段 Lua;
  - 445 个物理 zone 合计约 200 万次/秒;
  - 每主节点 5 万~10 万次 Lua/秒,需要几十个主节点 [I]。
  - Redis Cluster 的设计目标约 1000 个节点 [D],远没到。
- 连接数:每主节点的连接 = scene 进程数 + Σ(Go 服务副本数 × 每节点池上限)+ 网关副本数 + 订阅连接。加主节点不增加单节点的连接数,只增加每个客户端进程的连接数。公式同[玩家域设计](redis-player-store-cluster.md) §6.1。

### 6.2 单 zone 的上限

- **控制域**:一个 zone 一个槽。
  - 最重的是登录潮:每次自动选线跑一段 3N 键的 Lua(`scene_atomic.go:480`),外加 2~3 段成员 Lua。
  - 选点、判活几乎每次都读 `scene_nodes:{ctl:<z>}:load`,领导者每一拍都写。
  - 按每 zone 每秒进场几百人估,是每秒几千段 Lua,单主节点约有 10 倍余量 [I],要压测核实。
- **排队**:
  - 10 万人排队、固定 2 秒轮询时,每秒 5 万次 `lookupScript`,每次内部 9 条命令,其中 3 条打在 `queue:{q:<z>}` 上;单主节点要吃掉 0.5~1 个核 [I],接近饱和。
  - 按 §4.3 拉长轮询间隔后,每秒约 2000 / 2 + (100000 − 2000) / 30 ≈ 4300 次 [I]。
  - 再往上(单 zone 百万级排队)才需要按 queueId 把排队拆成 S 份。代价是:
    - 每次轮询要对另外 S−1 份各做一次 ZCOUNT;
    - 名次和总数不再是同一个快照;
    - 放行要 S 路归并;
    - `admitted:zset` 留单键,放行脚本拆成「先占位、后放行」。

    等压测证明需要再做。
- **网关**:`rl:zone:<z>` 上限是该 zone 的请求速率。IP 桶先判,IP 桶按 IP 散开。

### 6.3 到顶之后

- **单个 zone 超过一个主节点**:只在互相没有原子操作的子族之间再拆 tag(例如世界频道按地图拆)。要逐段改脚本,不预留,等实测再定。
- **集群节点数接近上限**:按 zone 区间做多集群联邦,做法同[玩家域设计](redis-player-store-cluster.md) §10.3,键形不变。

## 7. 部署

- **集群清单**:
  - `redis-player-cluster.yaml` 通用化成一份集群模板,名字做成占位,渲染两次(player、control);
  - 镜像钉 8.10.2 同一个摘要;
  - 每个 Pod 一个 ClusterIP Service,用 `cluster-announce-ip` 宣告(C++ 只认 IP 字面量);
  - `updateStrategy: OnDelete` 加建群 Job;
  - `cluster-require-full-coverage no`;
  - noeviction + 显式 maxmemory,AOF everysec,PVC。
- **`k8s_deploy.ps1`**:
  - 参数 `-ControlRedisMasters`:dev 缺省 0,保留单实例 `redis.yaml` 只给 dev 用;staging、prod 必须显式、至少 3。
  - 另有 `-ControlRedisReplicasPerMaster`、`-ControlRedisMaxMemory`、`-ControlRedisStorage`,口令 Secret `redis-control-auth` / `MMORPG_CONTROL_REDIS_PASSWORD`。
  - 渲染:各服务的 `ControlRedis` 段,C++ node-config 的 `control_redis`,Java 网关的集群节点。
  - 运维命令:连接总账按每主节点记;`redis-wipe` 把新 StatefulSet 加进清单;`redis-split-status` 改成列出集群状态。
- **压测**:`stress_snap.ps1` 的 target 用 `k8s-cluster:<ns>/redis-control`(已支持集群 target)。
- **告警**:另起一组 PrometheusRule(slot 覆盖、内存、maxclients),照 `redis-player-alerts.yaml` 写。
- **手册**:`docs/ops/redis-player-cluster-runbook.md` 通用化成两套集群共用。
- **退役**:
  - 清单:`redis-ha.yaml`、`redis-purpose.yaml`、`redis-purpose-ha.yaml`、`redis-rank.yaml`(遗留,部署脚本早已不 apply);
  - 参数:`-RedisSplit`、`-RedisReplicas`、`-RedisQueueMaxMemory`、`-RedisCtlMaxMemory`、`-ConfirmRedisCtlSwitch`;
  - 相关的 queue / ctl 总账、切换闸与状态检查。`k8s_deploy.ps1` 里这套机制约 356 处引用 [C]。
- **本机**:
  - `deploy/docker-compose.yml` 的单实例 redis 不动;
  - 真集群验证复用 `deploy/compose.redis-cluster.yml`(8.10.2,7100-7105),玩家域与控制集群的句柄都指过去:两者是独立句柄,能不能分开在 K8s staging 上验;
  - `redis_local_flush.ps1` 已能逐主节点清库;`gateway-mock-stress.sh` 的 `--scan rl:*` 改成逐主节点。
- **AGENTS.md §6**:压测口径里的 `-RedisTargets` 缺省值与清库说明,在文档批更新。

## 8. 迁移

- **上线前**(负责人 2026-10-18 确认未上线,见[存盘围栏](db-write-fencing-mysql.md)文末):
  - 新键名直接起步,不做双读双写;
  - 开发环境数据按 AGENTS §6.2 清空;
  - 首次上线就是集群形态。
- **上线后**:再改任何键形都要停写切换(全部写者排空后一次切换),禁止双读双写。围栏类原语在新旧键名上会各自「成功」。

## 9. 验证(交 Codex,按 AGENTS §10.1)

1. **单元与契约**(每批):
   - 两种语言的键金样表逐字一致;
   - 守卫用例:多键同 tag、只接受登记的族前缀、玩家域 tag 形状在控制客户端被拒,反过来也一样;
   - 每段脚本做「全部 KEYS 同槽」断言;
   - §4 列出的旧契约测试改写后全部通过。
2. **真集群集成**(compose 3 主 3 从,8.10.2):
   - 覆盖:scene_manager 的建场景、进场、离场、拆除、世界频道铺设与排空,C++ 归属对账,login 入队、轮询、放行全流程,gate 排空与停写脚本,网关限流(Java 集成测试连集群),唤醒 SPUBLISH / SSUBSCRIBE,选主锁,guild 缓存与重建榜,player_locator 的 pass,名字缓存批量读;
   - 断言:CROSSSLOT 为 0,守卫拒绝为 0,各处 fail-open 计数为 0。
3. **迁移与故障**:
   - 用例运行中原子迁 slot;
   - 迁移中 kill 源主;kill 副本;kill 主节点且旧主不回来;
   - 迁槽后唤醒订阅自动重订、不丢后续唤醒;
   - 断言照[玩家域设计](redis-player-store-cluster.md) §14.2:不出现非重定向类错误。
4. **容量实证**:3 主对 6 主施加同一份过量负载(控制域脚本组合加排队轮询),预期吞吐接近 2 倍、每主内存近半;按 AGENTS §6 用 `stress_summarize.ps1` 出 R1 / R2 逐主节点对比表。没有这张表,不能宣称扩容有效。

以上一项都没有跑过。玩家域集群的代码同样还没有任何运行证据,本稿依赖它的构造器与 C++ 路由层。

## 10. 分批

每批不超过 30 个文件(含测试),批间做语义评审;构建、测试、压测由 Codex 执行;动手前在缺口清单第 34 条认领、核对工作区。

| 批 | 内容 | 约文件数 | 依赖与协调 | 状态 |
|---|---|---|---|---|
| 0 | 本设计稿;缺口清单第 34 条;索引与 PROGRESS | 6 | — | 已写(2026-11-10) |
| 1 | `shared/clusterredis`(从 playerredis 迁出,`New(c, KeyPolicy)` 两种键策略),`shared/playerredis` 只剩别名;控制集群族前缀表 `shared/redisslot/control_keys.go` 与 C++ `redis_slot.h` 镜像;C++ `KeyDomain` 与 `SPUBLISH` 登记,玩家域句柄设 `kPlayer`;金样与守卫测试 | 约 25(含 9 个挪走的旧文件) | — | 已落码,未编译(2026-11-11;「服务器代码缺口补完」写了 Go 拷贝与族表,本会话补完别名层、控制策略用例与 C++ 一侧,见 PROGRESS 2026-11-11) |
| 2a | scene_manager 键构造集中化与改形(§4.1),连同四个跨语言键的 C++ 常量;删 `scene:<id>:zone`;只给场景号的入口收敛(§3.3);CreateScene 的 zone 0 | 25–30 | 1;与「场景销毁认领」「Returning scene node」会话对段落 | 未开工 |
| 2b | scene_manager 句柄换 UniversalClient(go-zero 调用改写)、shared/leader 补 UniversalClient 版 store(§4.7)、`load_refresh` 核对、契约测试重写 | 25–30 | 2a;shared/leader 一处与断线重连会话对清单 | 未开工 |
| 3 | C++ `tlsControlRedis`;三处读与归属对账;SPUBLISH 与 Go `assetopwake` 的 64 路 SSUBSCRIBE、`ChannelWithSubscriptions`(跨语言同批);CreateScene 校验 zone(内部协议加字段,先跑协议生成);gate、battle 去掉 Redis 连接 | 20–25 | 1、2a;「Hiredis 生命周期收尾审核」会话 | 未开工 |
| 4 | login 排队与 gate 标记(键改形、UniversalClient、放行改节点客户端 pipeline、删 `QueueShardCount`;按名次拉长轮询间隔已由第 14 条 11-11 提前落码);scene_manager 读 gate 标记;`k8s_gate_drain.ps1`;`gatepick`;Java 网关限流两段单键脚本与 Lettuce 集群 | 25–30 | 1;gate 排空迁移相关会话、「Zone 人数满的处理」会话 | 未开工 |
| 5 | guild(缓存 tag、榜按 zone、去 SCAN、锁按 zone、assetopfix);friend 换句柄;player_locator 控制键 tag;data_service(唤醒、名字缓存 `MGetBySlot`);trade、player_inbox 换句柄;chat 删无用句柄(11-11 已做,见 §4.8);R8 收口;`tools/data_consistency_check` | 25–30 | 1;唤醒只订本副本分片依赖第 25 条 | 未开工 |
| 6 | 部署:集群模板通用化与 `redis-control`、`k8s_deploy.ps1` 参数与渲染、总账、wipe / status、告警、手册、compose;退役核心、queue、ctl、rank 清单与参数;match 集群升 8.10.2;部署契约测试。match 集群的显式 maxclients、单独一份总账与 `-WaitReady` 逐节点核对 11-11 已由「Redis 登录总账审核」会话补上,通用化时并入 | 25–30 | 2–5;「Redis 登录总账审核」会话 | 未开工 |
| 7 | 文档改写:控制面设计稿 §4 / §5,玩家域设计 §5.1 域表,TiDB 总体方案 §2.1 / §8.4,G4a 设计稿(标历史),AGENTS.md §6 压测口径,`deploy/k8s/README.md`,`tools/scripts/README.md` | ~10 | 6 | 未开工 |

合计约 170~200 个文件。

### 10.1 第 2a 批动手前的协调记录(2026-11-11)

11-11 凌晨有四个会话在改 scene_manager 与 C++ 的控制域文件,第 2a 批先逐个对了文件与键:

- **「场景销毁认领:回滚原子化与重发幂等」**:
  - 还在改的:`world_init.go` 的懒改派、`enterscenelogic.go` 里 `resolveScene` 的一处调用、新文件 `channel_move_flight.go` 与测试。约 02:30 收尾,收尾后通知。不新增控制域键。
  - 已交出、可以改:`scene_atomic.go`、`scene_teardown_job.go`、`agones_room_ledger.go`、`agones_binding.go`、`agones_reconcile.go`、`world_rebalance.go`。
  - 它近两天新增或改过形状的键,同批改形:`agones_rooms:zone:<z>:holds`(ZSET,成员是条目,分数是截止毫秒);`scene:<id>:agones_gs`(值改为「GameServer/条目」);两种回执键;`scene:<id>:teardown` 与 `scene_teardown:zone:<z>:pending`;`scene:<id>:placement_gen`。
  - `luaJudgeAgonesRoomEntry` 删掉 `foreign` 分支后,找不到场景的条目按孤儿处理:只删 Agones 列表里那一项,不销毁场景。
  - `scene_keys_contract_test.go` 登记了 `agonesRoomHoldsKey`,键模式要同批改。
- **「无感升级和重启方案」**(计划排空 / 疏散):
  - scene_manager 已写完,不再动。新文件 `node_drain.go`、`node_drain_test.go`、`enter_evacuation.go`、`metrics/planned_drain.go`;小改 `load_reporter.go`、`node_selection.go`、`world_rebalance.go`、`world_init.go`、`enterscenelogic.go`、`safe_points.go`、`metrics.go`。都未编译,要等协议生成(`NodeInfo.build=16`、`EnterSceneRequest.evacuation=15`)。
  - 不新增 Redis 键:`drainWorkerKey` 只是进程内 map 的键,不改形。
  - 改法提示:`enter_evacuation.go` 的 `evacuationOwnerOf` 手里有 zoneID,可以直接传;`node_drain_test.go` 里的 `sceneNodeKey(555)` 改用 `testZoneId`。
- **「Returning scene node」**(缺口第 33 条残余):
  - 代码已改完,不再动;Codex 报错要回改时先找第 2a 批。只剩文档在改:收尾以 PROGRESS 出现「缺口第 33 条残余收尾」条目为准,之前不要动它的文档。
  - 新增三把控制域键,同批改形:`node:zone:<z>:<n>:holder_epoch`(与 holder 同写同过期,只有 scene_manager 读写)、`scene_manager:etcd_revision_seen:zone:<z>`、`scene_manager:etcd_epoch:zone:<z>`。
  - 同槽约束:`luaPublishNodeHolder` 的 KEYS 是 [holder, holder_epoch, etcd_epoch],`luaStartNewEtcdEpoch` 是 [etcd_revision_seen, etcd_epoch],同一个 zone 都用 `{ctl:<z>}` 就同槽。
  - 要跟着改的用例:`TestNodeHolderKeyShape`、`TestControlDomainKeysCarryNoHashTag` 的名单、C++ 的 `SceneOwnershipRules.KeysMatchTheSceneManagerKeys`。
  - 写着这些键名、要跟着改的文档:`scene-node-ownership-reconcile.md` §4.6 / §9 / §10、`docs/ops/etcd-maintenance-runbook.md` §7a(含手工 `INCR scene_manager:etcd_epoch:zone:<z>` 的退路)、`docs/ops/scene-node-role-split.md` 告警表、控制面设计稿 §4.2 表、`deploy/k8s/README.md`。
- **「Scene manager zone selection and etcd threshold」**:
  - 只改了注释,已收尾,不再动 scene_manager 与 C++,没有新加键。改过的注释在 `reentry_barrier.go` 文件头与 `clearNodeDeathForInstance`、`load_reporter.go` 的 `nodeGoneObservedAt`。
  - 经负责人同意,在本稿 §4.1 补了两处:「键」下列出 9 种节点级键、修订号水位与纪元;「原子操作」下列出 11-10 盘点之后新增的 4 段脚本。
  - 另报:断线重连会话在 `reentry_barrier.go` 加了 `clearNodeDeathForInstanceScript`,两键 `death_at`、`reconcile_instance`,同样要进 `{ctl:<z>}`。
- **结论**:四个会话的 Go 代码都已交出,只剩「场景销毁认领」在 `world_init.go` 与 `enterscenelogic.go` 的一处,收尾后通知。第 33 条会话的 `node_holder.go` 与 C++ 持有者几处,动之前先找它对一下,它还在改指标、告警与文档。

## 11. Java 版(AGENTS.md §12)

- Java 版服务器(独立仓)是 Redisson 单实例。它的 `PARITY.md` 第 369 行已有「Redis Cluster」待做行 [C]。
- 按负责人 11-05 的口径,本轮只登记待做,不写 Java 代码。各批落码时更新那一行,写明对应的本仓 commit。
- 客户端契约只有一处可能变:排队轮询间隔的取值。`RetryAfterMs` 本来就由服务端下发,字段与语义不变;Java 版的排队是否同样按名次拉长,随第 4 批登记。

## 12. 风险与未决

1. **单个 zone 的上限是一个主节点**(§6.2)。排队靠拉长轮询间隔压住,控制域的余量要压测核实。
2. **只给场景号的入口改了语义**(§3.3)。逐个入口核对并补用例;「不存在」不得走销毁。
3. **唤醒扇出依赖缺口第 25 条**(§4.6)。第 25 条落地前,唤醒在 2000 万 PCU 口径下不可扩展。
4. **排队拉长轮询间隔的前提是客户端遵守 `RetryAfterMs`**,还没有核对(客户端仓需要授权)。
5. **多个会话正在改同一批文件**:gate 排空与停写、scene_manager 拆除与归属、`k8s_deploy.ps1` 的总账、Hiredis 生命周期、login 排队上限、观战、shared/leader(R7)。每批动手前核对工作区、互发文件清单。
6. **依赖的玩家域集群代码与 C++ 路由层都还没有运行证据。**
7. **版本**:集群统一 8.10.x 后,「真 Redis 验证跑 7.2 与 8.x 两版」的口径只剩本机单实例开发需要;K8s 上不再有 7.2。
8. **改写量**:go-zero 句柄退役的改写量大(scene_manager 约 26 个文件),第 2b 批要单独评审。

## 13. 与断线重连会话的分工(2026-11-10 约定)

- **R8**(MaxRetries 缺省 3 的句柄上自动重发非幂等 Lua,例如 friend 的申请配额)并入本稿的句柄替换批(第 2b、4、5 批)。断线重连会话不改这些建句柄的代码,它的清单里记成「随 Redis 分布式水平扩展的句柄替换批收掉」。
- **R7**(shared/leader 选主之后的写带任期号)归断线重连会话。约束:
  - 新增的键由锁键派生,与锁键同 tag,同一段脚本里两把键都写进 KEYS;
  - 只依赖 `UniversalClient`,不假设多条命令走同一条连接;
  - 任期号放在 Redis 里时,切主可能丢掉最后一次 INCR,任期会倒退。
- **文件清单**:双方动 5 个锁键调用点(`go/scene_manager/scene_manager_service.go`、`go/trade/internal/svc/assetchannel.go`、`go/guild/internal/svc/asset_op.go`、`go/player_locator/internal/logic/session_reconciler.go`、`go/player_inbox/internal/svc/servicecontext.go`)或任何建句柄文件之前,先把文件清单发给对方。

## 修订记录

- 2026-11-10:初稿。依据三路只读盘点与断线重连会话的分工约定。
- 2026-11-11:第 1 批落码(未编译)。§5.2 补 C++ `KeyDomain` 的取值;shared/leader 的 UniversalClient store 从第 1 批挪到第 2b 批(第 1 批没有调用方)。
