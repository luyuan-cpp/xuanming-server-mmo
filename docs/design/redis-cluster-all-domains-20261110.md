# 全部 Redis 上 Cluster:控制集群与各用途的分片设计(2026-11-10)

状态:**第 1、2a、2b、3、4 批已落码(未编译、未测试,待 Codex 验证);第 5、6 批由别的会话在做;第 7 批未落码。** 负责人 2026-11-10 定:「我要分布式水平扩展」。服务器里每一套 Redis 都要能靠加主节点扩写入。按 zone 组分单主实例(排队 `redis-queue-g<N>`、控制域 `redis-ctl-g<N>`)和核心单主(`redis` / `redis-ha`)都不算完成。登记为[缺口清单](../handoff/server-gap-audit-20261101.md)第 34 条。

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

- **一律用本进程的 zone**(`svcCtx.Config.ZoneId`),与 §4.3 的不变量一致:同组的键「读得到也不读」,行为不随组的划分而变。
- **别的 zone 的场景读成「不存在」。** 原来同组外 zone 的场景能解析,这本来就是不变量禁止依赖的部署巧合。每个入口都核对过:「不存在」与原来「是别的 zone」走同一个结局,不销毁、不拆除任何东西。第 2a 批逐个入口补了用例(§10 表第 2a 行列出)。
- **`scene:<id>:zone` 删除**,读它的 `GetSceneZone` / `lookupSceneZone` 一起删。zone 已经是键地址的一部分,「只给场景号,它在哪个 zone」只有一种答法 `ownZoneOfScene`:本 zone 的 `scene:{ctl:<z>}:<id>:node` 在就回本 zone,否则(别的 zone 的、已销毁、读失败)回 0,与原来「没有 zone 记录」同一个结局。EnterScene 第 1 步与没有 zone_id 的历史位置记录、LeaveScene 的释放判定用它。
- **`foreign` 分支删掉。** 认领脚本(原 `luaSceneClaimZoneCheck`)与 Agones 条目判定(`luaJudgeAgonesRoomEntry`)里的 `foreign`,连同 `sceneClaim.Foreign`、`errSceneInOtherZone`。各入口的结局:
  - 销毁认领:别的 zone 的场景在本 zone 读不到映射,force 认领落空、按期望属主的认领 stale,一个键都不动。显式 DestroyScene 拿到只登记在别的 zone 的场景,从回错误改为与「映射已不存在」一样幂等成功;请求的 zone 不是本进程的 zone 仍在任何读写之前拒绝。
  - Agones 条目:场景不在本 zone 的键里的条目按孤儿处理,只从本 zone 的 GameServer 上删掉这一条,不销毁场景、不碰别的 zone 的键(§10.1)。按 zone 标签列出的 GameServer 只承载本 zone 的场景,别的 zone 的场景出现在这里本身是错账。
  - 镜像级联(源销毁、源迁移):子集合在本 zone 的键里;列着的子镜像不在本 zone 时认领落空、不拆,按已处理从子集合摘掉。
  - 镜像拆除摘源子集合(`unlinkFromSourceMirrors`):只 SREM 本 zone 的 `scene:{ctl:<z>}:<源>:mirrors`,不再先读源的 zone。
- **`removeScenePlayer` 只给场景号时**,按本进程的 zone 读 `scene:{ctl:<z>}:<id>:node`:不在就不动(计一次 `foreign_zone_skip`),在就按属主摘;读失败回错误,由上游重试(LeaveScene 的错误语义)。先判再摘是为了不在本 zone 凭空建出别的 zone 场景的成员集合(摘完之后「location 又指回这里就加回去」那一步)。
- **镜像源在别的 zone**:源克隆镜像(没有自己的配置)回 `ErrSourceSceneGone`;带自己配置的镜像照建,共置按 `no_mapping` 回落(原来单列的 `zone_mismatch` 标签删除),子集合与反向索引写在本 zone 的键里——与源已销毁同一个结局,源所在 zone 的级联读不到它们,随镜像销毁摘掉。
- **CreateScene 的 `zone_id = 0` 按本进程的 zone 处理**,与 DestroyScene 的口径相同。原来以 zone 0 登记(`createscenelogic.go:91`),改后不存在 zone 0 的键。
- **resolveScene 指定场景号**:只读请求 zone 的属主键,别的 zone 的场景按「查无此场景」拒绝(原来靠 `scene:<id>:zone` 守卫拦),不判活、不改派。

### 3.4 C++ 归属对账的前提

`scene_ownership_reconciler.cpp` 每 10 秒 MGET 一批 `scene:<id>:node`,每批最多 256 个,C++ 只能用 `GetZoneId()` 打 tag。这要求「一个 scene 节点只承载本 zone 的场景」。

今天这条只由 Go 保证 [C]:
- CreateScene 拒绝外 zone;
- 按 (zone, node) 拨号;
- Agones 分配校验 zone;
- 镜像共置遇到 zone 不符会回落;
- 世界频道只铺本 zone。

一旦被打破,C++ 读到 kNoMapping,确认两次后会拆掉活场景。第 3 批在节点侧的 CreateScene 请求里加上 zone 字段(内部协议,客户端不可见),C++ 发现 zone 不是本节点的就拒绝创建。这样把前提变成两边都核,在创建时就失败关闭。

第 3 批的做法(2026-11-12,未编译):
- 协议:`proto/scene/scene.proto` 的 `CreateSceneRequest` 加 `uint32 zone_id = 7`(muduo `Scene` 服务与 gRPC `SceneNodeGrpc` 共用这一份)。
- scene_manager:唯一的出口 `createSceneOnNode` 填 `svcCtx.Config.ZoneId`(场景的控制域键写在本进程 zone 下),不填拨号用的 zoneId:调用方拨错 zone 时由节点拒掉,而不是在别的 zone 的节点上建出一个之后会被对账拆掉的场景。
- C++:纯函数 `SceneLifecycle::AdmitsCreateZone(request, node)`,zone 不等或为 0 都拒。gRPC 入口判在等 Agones 创建许可之前(别的 zone 的建场景不能让进程被 Allocate),回 `FAILED_PRECONDITION`;muduo 入口判在取许可之前,回空应答。两处都打 `metric=scene_create_zone_mismatch` 的 ERROR。scene_manager 对任何错误都回滚(`createSceneOnNode` 同时核对应答里的 scene_id)。

## 4. 逐用途改法

### 4.1 scene_manager 控制域

- **键**:12 种 `scene:<id>:*`、`instance:<id>:members`、9 种 `node:zone:<z>:<n>:*`、世界频道 6 种、分线目录与线号、神秘商人、Agones 名额、拆除任务、死节点收尾表、`world_init` 锁、选主锁、etcd 修订号水位与纪元,全部改 `{ctl:<z>}`。
  - 9 种节点级键按 11-11 工作区的代码是 `scenes`、`scene_count`、`scene_node_type`、`members`、`death_at`、`reconcile_scenes`、`reconcile_instance`、`holder`、`holder_epoch`;其中 `holder`、`holder_epoch` 在 `node_holder.go`,写本稿时还没提交。
  - 修订号水位 `scene_manager:etcd_revision_seen:zone:<z>`、纪元 `scene_manager:etcd_epoch:zone:<z>` 是 11-11 为节点号持有者加的(缺口第 33 条,`node_holder.go`)。它们和 `holder`、`holder_epoch` 出现在同一段脚本里,必须同 tag。
  - 拼键约 185 处:177 次私有构造函数调用,加 8 处裸串或裸 Sprintf,散在约 14 个文件里 [C]。测试里另有约 460 处(38 个文件)。
  - 第 2a 批的做法(2026-11-12):构造函数留在原文件,tag 只经一个函数 `sceneControlTag(zone)` 生成,8 处裸串全部换成构造函数;
    `TestControlZoneKeysContract_NoHandBuiltControlKeysInSource` 扫生产代码,拦住 `zone:%d`、`"scene:%d`、`"node:zone` 这类手拼。
    没有另建键包:那要搬约 40 个构造函数、再改一遍全部调用点,功能上没有差别,「只经构造函数拼键」由这条用例守住。
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
  2026-11-12 第 2b 批已核对:断言是 `TestLoadRefreshBatchesStayOnOneZoneTag`(读批、写批与选主锁键同一个 `{ctl:<z>}`);带围栏的 ZADD(R7 的 `luaFencedLeaderZadd`)是 EVAL,控制集群句柄不许它进管道,改为逐条单发,其余命令照旧一条管道;go-zero 的熔断钩子随句柄退役,判定里的熔断分支删掉(§10.4)。
- **键前缀约定**:合服的 `node:zone:{S}:*`(`reentry_barrier.go:250-251`),以及 `orphanKeyPrefix` / `parseWorldChannelsKey`(`orphan_cleanup.go:48`、`:109`),随键形改。
- **句柄**:go-zero → `UniversalClient`(§5.1)。选主 `leader.NewGoZeroStore` 换成 UniversalClient 版的 store(§4.7)。
  2026-11-12 第 2b 批的做法:新包 `go/scene_manager/internal/controlredis`,在 `clusterredis.New(…, ControlClusterKeys)` 之上做一层形状适配,方法名与返回形状沿用 go-zero(Get 缺键回空串,Hget / Zscore 缺键回 `redis.Nil`,分数截成 int64),约 170 处调用不改写;守卫、不重放都来自 clusterredis。原计划「约 26 个文件的调用改写」不做:逐处改写不增加正确性,还要把每处「缺键怎么判」重新核一遍(§10.4)。
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
- **第 3 批的实际做法**(2026-11-12,未编译;协调与偏差见 §10.5):
  - `tlsControlRedis` 声明在 `redis_cluster_client.h`,与 `tlsPlayerRedis` 并列;`redis_manager.h` 里的 `tlsRedis`、`tlsSceneControlRedis` 删掉,`RedisManager` 只剩集群客户端的每节点连接一种用法。scene 在 `RedisSystem::Initialize` 建连,键策略 `kMultiKeyOnly + KeyDomain::kControl`;不挂重连回调(四处用法都是一次性的,丢了等下一拍或退回原退避)。
  - 唤醒用 `command` 发 `SPUBLISH <频道> <pid>`,不用 `commandForKey`:命令表按 SPUBLISH 的频道参数路由(第 1 批已登记)。
  - 归属对账的 MGET 一次几百个键,格式串写不出,集群客户端新加 `commandArgv(cb, argc, argv, argvlen, SideEffects)`,与 `command` 同一套守卫、停等与回调契约。`SideEffects::kNone` 保住原来 `hasNoSideEffect` 的口径:只读命令任何结局都不在维护前沿留下 UNKNOWN。
  - 整个 zone 的控制域键同一个 slot,集群客户端在同 slot 上停等:一个 scene 进程的分线目录 GET、神秘商人 HGET 与对账 MGET 排成一列,吞吐约 1 / RTT(往返 0.3~0.5ms 时每进程每秒两三千条)[I]。分线目录按进场次数发,单进程每秒几十到一两百次,余量约 10 倍;排队满时 GET 回 REDIS_ERR,照旧推不带目录的场景信息。
  - 配置:`GameConfig` 的 `zone_redis = 7`、`scene_control_redis = 11` 改 `reserved`,新加 `control_redis = 12`;与 `player_redis` 共用一个消息 `RedisClusterConfig`(mode、seeds、password)。`control_redis` 整段或单键缺失取 standalone、127.0.0.1:6379,口令变量 `CONTROL_REDIS_PASSWORD`;`player_redis` 整段缺失时整体沿用 `control_redis`(同 Go 各服务 PlayerRedis 回落到 ControlRedis),写了这一段就必须带种子。两段的种子都经 `ResolveRedisAddress` 解析(R9 的有限次重试)。旧段还在时打一条 WARN。
  - gate、battle:`Node::InitRpcServer` 不再建连,`ReleaseLocalOwnersInLoop` 与入口兜底改为关两个集群句柄(gate、battle 上是空操作)。K8s 模板里的口令环境变量与 node-config 的渲染归第 6b 批(§10.5)。
  - 契约测试实际改的:`PlayerRedisRouteContract.ControlDomainStaysOffThePlayerHandle`(唤醒频道在玩家域句柄上按域拒绝)、新增 `ControlRedisRouteContract.*` 三条、`RedisClusterClientTest.ArgvCommandGoesThroughTheSameGuardAndRoute`、`SceneOwnershipRules.CreateSceneAdmitsOnlyTheNodeZone`;`RedisCommandRouteTest.*` 与 `AllKeysPolicyKeepsUntaggedKeysOffTheWire` 不用改。

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
  - 发布:`SPUBLISH assetop:wake:{aw:<pid % 64>} <pid>`,64 与资产通道的工作分片(`player_id % 64`)取同一规则(`wake_test.go` 核对 `ShardCount == workshard.Shards`)。只用一个分片频道会全部落在一个槽上,成为热分片。
  - 订阅(2026-11-12 第 3 批的实际做法,go/shared/assetopwake):
    - 集群客户端按 slot 分组,一组一个 `SSubscribe`:SSUBSCRIBE 一次订的多个频道必须同槽,go-redis 一个 PubSub 只连一个节点。64 片落在 64 个 slot,每个副本 64 条订阅连接;单机客户端一组、一条连接订全部 64 个频道(部署脚本的连接总账按每副本 1 条记)。
    - **不用** `Channel` / `ChannelWithSubscriptions`,订阅确认、收包、空闲 PING(3 秒)与重订都自己做:收到 `sunsubscribe`(迁槽)、`-MOVED`、断线或健康检查无回音,就关掉整条订阅、重读拓扑,按退避(1 秒起翻倍、封顶 30 秒、取 [d/2, d))重新 SSubscribe。原因:go-redis v9.17.3 在集群上自己重订分片频道时选节点只看普通频道,频道表为空就连随机节点,那里回 `-MOVED`,收包循环把错误吞掉,订阅就此静默失聪(PROGRESS 2026-11-12「第 3 批 Go 部分」有源码位置)。
  - 跨语言常量:C++ `player_landing.h` 的 `kAssetWakeChannelPrefix`、`kAssetWakeShardCount` 与 `AssetWakeChannel(pid)`(用 `redis_slot::ControlFamily::kAssetWake` 拼 tag),Go `wake.go` 的 `ChannelPrefix`、`ShardCount`、`ShardChannel`;`TestChannelMatchesCpp` 逐字解析 C++ 的两个声明,两侧金样同为 130 → `{aw:2}`、64 → `{aw:0}`、uint64 上限 → `{aw:63}`。
- **扇出是另一个问题,改了频道也解决不了。**
  - 在缺口第 25 条(资产重投按分片分工)落地之前,每个副本都得订全部 64 个分片,每条消息仍然投给 4 + 2Z 个副本,只是摊到了多个主节点上。
  - Z 取 200~445 时,每秒投递 1000 万~7000 万条 [I],摊开也扛不住。
  - 第 25 条落地后,每个副本只订自己负责的分片,每条消息只投给各服务的那一个负责副本(3 个服务),与 zone 数无关。
  - data_service 的召回不按 zone 过滤(`go/data_service/internal/svc/recall_channel.go`)[C]:每个 zone 的副本都在重投、都在订阅。第 25 条要按全体 data_service 副本分片,不能按 zone 内分片。
  - 这条依赖记在缺口清单第 34 条。今天 K8s 上 trade 缺省关、guild 在非 dev 档强制关、data_service 跟随 `-AssetOps`,所以开服时不一定全开。

### 4.7 选主锁与 shared/leader

- **集群下可用**:`go/shared/leader` 只有单键 `SET NX EX` 和单键 Lua(`leader.go:41-62`),没有 WAITAOF,也没有同连接假设 [C]。
- **缺口**:`NewGoRedisStore` 只收 `*redis.Client`(`store_goredis.go:19`),要补一个 UniversalClient 版本(`UniversalMaxCallDuration` 已有)。shared/leader 的内部改动归断线重连会话(R7),这一处谁先动谁做,动手前互发文件清单(§13)。**2026-11-12 已补(未编译)**:第 5a 批 guild 是第一个调用方,新增 `go/shared/leader/store_universal.go` 的 `NewUniversalStore(redis.UniversalClient) Store`,只新增文件;guild、trade、player_locator 已改用它,scene_manager 在第 2b 批接。
- **锁键 tag**:
  - scene_manager 的锁改成 `scene_manager:leader:lock:{ctl:<z>}`,与本 zone 的控制域键同槽;
  - login 的放行锁改成 `dispatcher:lock:login_queue:{q:<z>}`。

  这样带围栏的写能在同一段脚本里核对锁键并写数据。R7 2026-11-12 就是这样落的:负载分刷新的 ZADD 改成 `luaFencedLeaderZadd`,KEYS = [选主锁键, 负载集],脚本先核对锁键的值(本任期写进锁键的值,`leaderFence`)再写,没有另加任期号。
- **已知风险**:异步复制下切主可能丢锁,出现短暂双主。四个全局调用方都声明靠 CAS 兜底 [C];scene_manager 的负载分刷新由锁值围栏兜住(R7,`leaderFence`),不加任期号。

### 4.8 各服务私有键

- **guild**:
  - 缓存填充 / 失效 Lua 的 KEYS 是 [generation, data],两键不同槽(`guild_repo.go:215-227`)。改成 `{g:<id>}` / `{pg:<pid>}`,`guild/cmd/assetopfix` 复用同一套 Lua,同批改。
  - 帮会榜的脚本(条件写入、立墓碑、按版本摘、读榜头)每段只碰同一区的三个键:`guild_rank:zone:<z>`、`guild_rank:version:<z>`、`guild_rank:tombstone:<z>`,改成同一个 `{gr:<z>}` 即可,协议不用改。重建已不再用 MULTI 包多键 DEL 加 RENAME 临时键(缺口第 23 条,11-11 落码未编译,见 PROGRESS 2026-11-11「帮会积分写入不再抢全服锁」)。
  - SCAN `guild_rank:zone:*`(`guild_repo.go` 的 `rankZones`,重建与摘榜都用它)改成按 zone 清单逐个处理,zone 清单从 SQL 取,不扫 Redis。
  - `tools/data_consistency_check` 的榜孤儿检查例外:它要找的正是不在区清单里的榜键,只能枚举整个库。2026-11-12 已提前改完(未编译):客户端换成 `clusterredis.New(…, ControlClusterKeys)`,`-redis-cluster` 时 `-redis-addr` 是种子列表;扫描走 `clusterredis.ScanEachMaster` 逐主节点扫完整个库,任一节点出错或扫描前后拓扑变化报 fail「NOT CHECKED」,不报半截计数;没写 `-redis-cluster` 却连到 Cluster 节点也报 fail。键按第 5a 批的 `guild_rank:zone:{gr:<z>}` 解析(与 guild 同用 `redisslot.ZoneControlTag` 构造、逐字复核),改形之前的旧键与手写的键计数报 warn,不静默跳过。
  - 全服一把 `guild_rank:maintenance_lock`(11-11 起只让重建与合服折叠互斥,写分不拿)改成每 zone 一把 `guild_rank:maintenance_lock:{gr:<z>}`。
  - 全服帮会榜已由另一会话删掉(PROGRESS 2026-11-10,未编译),每 zone 的榜还在。
- **friend**:键已经是 `{f:<p>}` 等形状,两段缓存 Lua 是两键同 tag,集群安全 [C]。只换句柄:go-zero → UniversalClient,申请配额的 INCR Lua 从此不会被自动重发(R8)。
- **player_locator**:7 把控制键统一加 `{pl:gix}`,5 段 Lua(`gate_session_index.go:140`、`:161`,`index_pass.go:118`、`:134`、`:145`)就同槽了。访问是 10 秒级的低频。契约测试 `script_slots_contract_test.go` 的 `TestControlDomainKeysCarryNoHashTag` 要重写。`deploy-staging.sh`、`stress-linux-tier.sh` 的 `--scan 'player:gate_index:*'` 改成逐主节点。
- **data_service**:
  - 名字缓存的批量读改成单键 GET 的管道(ClusterClient 按节点拆),句柄由 go-zero 换成 UniversalClient,配置并进 `ControlRedis`;
    - **(2026-11-13 改正,原写「MGET 改用 `MGetBySlot`」)**:键 `player:name:<id>` 不带 tag,控制集群守卫允许单键不带 tag,但多键命令的每个键必须带同一个 tag。`MGetBySlot` 按槽分组,两个 id 落同槽时那一组就是不带 tag 的两键 MGET,守卫拒绝、整批读失败退化成全部回源;一批 500 个 id 在 16384 个槽里至少两个同槽的概率约 99.9%(生日问题)。给每个玩家加 tag 也不行:不同 tag 同槽照样是混 tag 的多键命令。单键 GET 管道没有这个问题,往返数与按槽分组相同(500 个 id 本来就几乎各占一槽)。回归用例 `TestMGetPlayerNamesWorksWhenKeysShareASlotUnderTheControlGuard`;
  - `SharedRedis` 只剩唤醒订阅与去重键;
  - `player_name_logic.go:599` 的 mget 是死代码,随批删。
- **trade、player_inbox**:只有选主锁、唤醒去重这类单键,换句柄即可。
- **第 5 批落码状态(2026-11-12,未编译,见 PROGRESS 同日「第 5a / 5b / 5c 批」三条)**:
  - guild:缓存 `{g:<id>}` / `{pg:<pid>}`、榜三键与维护锁 `{gr:<z>}`、zone 清单从 SQL(`SELECT DISTINCT zone_id FROM guild` 加合服映射两端)、锁按区升序拿、句柄与 assetopfix、申请配额专用池都换成控制集群句柄,配置段 `ControlRedis` 取代 `RedisClient` 与 `PlayerLocatorRedis`。**设计没写到的一处**:写榜脚本原来成批进 pipeline,控制集群句柄的守卫拒绝 pipeline 里的 EVAL,改成逐条发(重建、折叠每项一个往返;解散摘榜每个清单里的区一条)。不扫 Redis 的代价:某区帮会全部解散、摘榜又失败时,那个区的幽灵条目要等它再有帮会才被重建摘掉。
  - friend:句柄换成控制集群句柄,`ControlRedis` 取代 go-zero 的 `Redis` 与 `FriendRedis`,键形不变。
  - player_locator:7 把控制键改成 `player:gate_index:<名>:{pl:gix}`(tag 放在末尾),5 段 Lua 同槽,契约测试改为 `TestControlDomainKeysShareThePlayerLocatorTag`;`ControlRedis` 取代 `RedisClient` 段(`ServiceContext.RedisClient` 字段名暂留,只改类型);两份脚本加 `REDIS_CONTROL_NODES` 逐主节点清。
  - data_service:唤醒订阅句柄换成控制集群句柄,`ControlRedis` 取代 `SharedRedis`;`player_name_logic.go` 的死 mget 已删。**名字缓存没换**(`NameCacheRedis` 仍是 go-zero,MGET 未改 `MGetBySlot`):另一会话正在改 `player_name_cache.go` 与 `cmd/create_residual` 的修名字删缓存。名字缓存没有 Lua,R8 对它不成立。**2026-11-13 补做(未编译)**:`NameCacheRedis` 段删除、并进 `ControlRedis`,句柄是 `clusterredis.New(EffectiveControlRedis(), ControlClusterKeys)`(常驻启动必校验 `ValidateControlRedis`,不再只在 Recall 开着时要求),批量读改单键 GET 管道(理由见上);`cmd/create_residual` 参数改为 `-name-cache-addrs`(逗号分隔种子)加 `-name-cache-cluster`。K8s 渲染里 data_service 的 `NameCacheRedis` 段随 6b 删掉(go-zero 忽略多余的键,删之前不会拒启)。
  - trade:句柄换成控制集群句柄,`ControlRedis` 取代 `SharedRedis`,维护选主改 `NewUniversalStore`。
  - player_inbox:**没换**,配置与装配文件正被另一会话改;它只有选主锁,R8 由 `shared/leader` 的锁值核对兜住。
  - 渲染(5d):没做,`k8s_deploy.ps1` 与部署契约测试正被另一会话改,要改的点列在 PROGRESS「第 5c 批」条目末尾。
  - **R8 收口状态**:guild、friend(申请配额与目录配额的 INCR Lua 不再被自动重发)、player_locator、data_service 唤醒、trade 换成 `shared/clusterredis` 之后都没有 go-zero 句柄,也没有 MaxRetries 缺省 3 的控制域 go-redis 句柄。本批范围里剩两处:data_service 名字缓存(go-zero,只有 MGET / SET / SETNX / DEL,没有 Lua,R8 不成立)与 player_inbox 的 `CoreRedis`(go-redis,只有选主锁,`shared/leader` 已按锁值核对)。scene_manager(第 2b 批)、login(第 4 批)不在本批。
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

- **集群清单**(2026-11-12 第 6a 批落码,未运行):
  - 一份模板 `deploy/k8s/manifests/infra/redis-cluster.yaml`(由 `redis-player-cluster.yaml` 通用化而来,旧文件删除),集群名与口令 Secret 名做成占位,渲染两次(`redis-player`、`redis-control`),全部对象名带集群名前缀,两套在同一个 namespace 里不重名;
  - 两套的差别只写在 `k8s_deploy.ps1` 的 `Get-RedisCluster -Kind player|control` 一处(名字、Secret、口令环境变量、参数前缀、打印与告警前缀),拓扑、容量、变更闸、渲染、apply、就绪等待、连接总账、清库、状态都是同一组函数;
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
| 2a | scene_manager 控制域键全部改 `{ctl:<z>}`(§4.1):以 zone 为键的族只改构造函数体,以场景号为键的族加 zone 参数并改全部调用点;四个跨语言键同批改 C++(`SceneNodeKey`、`NodeHolderKey`、`PlayerSceneSystem::WorldChannelDirectoryKey`、`PlayerMysteryMerchantSystem::RecordKey`,句柄不换);删 `scene:<id>:zone` 与 `foreign` 分支;只给场景号的入口收敛(§3.3);CreateScene 的 zone 0;契约测试改写,公用夹具加「每段脚本的 KEYS 同 tag」守卫;运维手册、告警注释与几份设计稿里的键名 | 约 100(生产 Go 34、Go 测试 43、C++ 8、文档与清单 17) | 1;与「场景销毁认领」「Returning scene node」「服务器代码缺口补完」(R15)「owner_epoch 铸造持久化独立审核」「控制面按 zone 化」「分布式锁安全性与一致性」会话对过段落(§10.1) | 已落码,未编译(2026-11-12,PROGRESS 同日「全部 Redis 上 Cluster 第 2a 批」)。超过每批 30 个文件:场景键改签名必须全部调用点同批改,拆不成可编译的小批;负责人 11-12 同意一批做完(原话「同意一批做完」,在「Turn battle gap closure handoff」会话里答「同意第 2a 批一次改约 60 个文件吗?」;实际约 100 个,多出来的是测试里的调用点与文档里的键名),当作一批机械改动做,评审按「签名加 zone、调用点照改」看,§3.3 的语义改动单列 |
| 2b | scene_manager 换控制集群句柄:新包 `internal/controlredis`(`clusterredis.New(…, ControlClusterKeys)` 上的形状适配层,方法与返回形状沿用 go-zero,调用点不改写);配置 `ControlRedis` 取代嵌入的 go-zero `Redis` 段与 `CoreRedis` 段(配置里写着 `Redis.Host` 拒启,控制集群是 Cluster 时 `PlayerRedis.Addrs` 必填);gate 标记改从控制集群句柄读;选主换 `leader.NewUniversalStore`,删 `shared/leader/store_gozero.go`;`load_refresh` 带围栏的 ZADD 单发、删熔断分支、加「一批只在一个 zone tag 上」断言;测试夹具走生产构造器,测试里经控制句柄读写玩家域键与跑玩家域脚本的 190 处改走玩家域句柄;守卫用例:模块里不许再导入 go-zero 的 Redis | 52(生产 Go 16、shared 2、测试 31、文档 3) | 2a;「分布式锁安全性与一致性」(R7 已落)、第 4 批(`gate_redirect.go`)、第 3 批(`createSceneOnNode`)对过段落(§10.4) | 已落码,未编译(2026-11-12,PROGRESS 同日「全部 Redis 上 Cluster 第 2b 批」)。超过每批 30 个文件:句柄类型一换,logic 包的全部测试必须同批改才能编译;负责人 11-12 在做 2a、2b 的会话里同意一批做完(问的时候估约 45 个,实际 52 个:多出 4 个经控制句柄读写玩家键的测试文件、5 个只改注释的生产文件与 1 条守卫用例) |
| 3 | C++ `tlsControlRedis`;三处读与归属对账;SPUBLISH 与 Go `assetopwake` 的 64 路 SSUBSCRIBE、`ChannelWithSubscriptions`(跨语言同批);CreateScene 校验 zone(内部协议加字段,先跑协议生成);gate、battle 去掉 Redis 连接 | 20–25 | 1、2a;「Hiredis 生命周期收尾审核」会话 | 已落码,未编译(2026-11-12,PROGRESS 同日「全部 Redis 上 Cluster 第 3 批」与「第 3 批 Go 部分」两条)。拆 3a(C++ 句柄、配置、唤醒发布、gate / battle 不连 Redis,加 Go 订阅端的真 Redis 用例,26 个文件)与 3b(CreateScene 校验 zone,8 个文件),Go 订阅由同题会话 [0cc556] 做(4 个文件),文档 4 份共用,每份都在 30 个以内。与原计划的偏差:订阅不用 `ChannelWithSubscriptions`(§4.6);集群客户端新加 `commandArgv`;K8s 渲染留给第 6b 批(§10.5) |
| 4 | login 排队与 gate 标记(键改形、UniversalClient、放行改节点客户端 pipeline、删 `QueueShardCount`;按名次拉长轮询间隔已由第 14 条 11-11 提前落码);scene_manager 读 gate 标记;`k8s_gate_drain.ps1`;`gatepick`;Java 网关限流两段单键脚本与 Lettuce 集群 | 25–30 | 1;gate 排空迁移相关会话、「Zone 人数满的处理」会话 | 已落码,未编译(2026-11-12,「服务器代码缺口补完」会话,PROGRESS 同日「全部 Redis 上 Cluster 第 4 批」)。拆 4a gate 标记、4b 排队、4c 网关限流;协调与偏差见 §10.3 |
| 5 | guild(缓存 tag、榜按 zone、去 SCAN、锁按 zone、assetopfix);friend 换句柄;player_locator 控制键 tag;data_service(唤醒、名字缓存单键 GET 管道,11-13 补做);trade、player_inbox 换句柄;chat 删无用句柄(11-11 已做,见 §4.8);R8 收口;`tools/data_consistency_check`(逐主节点扫描与 `{gr:<z>}` 键形解析 11-12 已改,未编译,见 §4.8) | 25–30 | 1;唤醒只订本副本分片依赖第 25 条 | 11-12 由「按缺口清单做完服务器代码(11-05 额度重置后)」会话认领。5a guild 已落码;5b friend、player_locator 已落码;5c data_service 唤醒句柄与 trade 已落码,名字缓存 `MGetBySlot` 与 player_inbox 因别的会话在改同一批文件未做;5d 渲染未做(同因)。全部未编译,见 §4.8 末条与 PROGRESS 同日三条;`tools/data_consistency_check` 由另一会话做 |
| 6a | 控制集群本身的部署(§7),不依赖各服务的配置结构:集群清单通用化成一份模板,渲染 `redis-player`(产物不变)与 `redis-control`;`k8s_deploy.ps1` 的 `-ControlRedisMasters` 与副本、内存、存储参数,口令 Secret `redis-control-auth`(只经 `secretKeyRef`),apply、建群、就绪等待;`redis-wipe`、`redis-split-status`、连接总账补控制集群;告警与手册通用化;match 集群升 8.10.2;删 `redis-rank.yaml`;部署契约测试。match 集群的显式 maxclients、单独一份总账与 `-WaitReady` 逐节点核对 11-11 已由「Redis 登录总账审核」会话补上,通用化时并入 | 15–25 | 1;「Redis 登录总账审核」会话 | 代码写完,未运行(2026-11-12,「控制面按 zone 化」会话;「Redis 分布式水平扩展」会话分派,负责人 11-12 对剩余批次原话「Start them」)。28 个文件;落码与偏差见 §10.6,验证命令见 PROGRESS 2026-11-13「全部 Redis 上 Cluster 第 6a 批」 |
| 6b | 各服务接控制集群:ConfigMap 渲染 `ControlRedis`、C++ node-config 的 `control_redis`、gate / battle 去掉 Redis 口令环境变量、Java 网关集群节点;退役核心、queue、ctl 实例与 `-RedisSplit` 等参数;compose | 25–30 | 2b~5 批定下各服务的配置段;6a | 未开工(「控制面按 zone 化」会话认领) |
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

### 10.2 第 2a 批落码时的协调与偏差(2026-11-12)

- **协调**:动手前逐个对了文件与段落,都没有同时改同一段:
  - 「场景销毁认领」已收尾(会话已结束),`world_init.go`、`enterscenelogic.go`、`channel_move_flight.go` 不再改;
  - 「服务器代码缺口补完」的 R15 残余(死节点收尾撤销、认领核对任务表)11-12 落完才动 `reentry_barrier.go`、`scene_atomic.go`、
    `instance_lifecycle.go`、`load_reporter.go`;它新加的认领脚本同时碰场景键与本 zone 的任务表,改形后同 tag;
  - 「owner_epoch 铸造持久化独立审核」改完 `enterscenelogic.go` 的 owner_epoch 四处之后,才改那里的场景键调用点;
  - 「控制面按 zone 化」11-12 在 `enterscenelogic.go` 的 `resolveSceneForEnter`、新文件 `world_channel_on_demand.go` 与
    `foreign_zone_registry.go` 改动,与本批不同段落;
  - 「分布式锁安全性与一致性」(R7 任期号)排在本批之后改 `load_refresh.go`、`scene_registration.go`、`leader_gate.go`、
    `scene_manager_service.go`;新任期键按选主锁同 tag(`{ctl:<z>}`)经构造函数拼;
  - 第 33 条会话(Returning scene node)已结束,PROGRESS 11-11「缺口第 33 条残余收尾」之后,本批顺带改了它文档里的键名。
- **与原计划的偏差**:
  - 2a 不拆 2a-1 / 2a-2(理由见 §10 表);
  - 构造没有集中到键包(理由见 §4.1);
  - 认领脚本与登记脚本去掉 `scene:<id>:zone` 之后,KEYS / ARGV 的序号顺移:认领 KEYS[1..8]、ARGV[8..10] 是收尾任务的核对
    参数(原 [9..11]);登记 KEYS[1..7]、ARGV[1..6](实例另有 KEYS[8..11]、ARGV[7..9]);Agones 条目判定 KEYS[1..3]、ARGV[1..3]。
  - 镜像源在别的 zone、带自己配置的镜像照样登记进本 zone 的子集合(与源已销毁同一个结局),没有保留原来「不登记」的特例(§3.3)。
- **没做(按分批留给后面)**:句柄仍是 go-zero / `tlsSceneControlRedis`(第 2b、3 批);gate 标记仍是旧键形(第 4 批;`TestControlDomainKeysCarryTheZoneTag` 暂未列它们,第 4 批改完补上 `{gm:<z>}` 的断言);
  login 排队、guild 榜、网关限流的键名在运维文档里原样留着(第 4、5 批)。

### 10.3 第 4 批的落码、协调与偏差(2026-11-12)

- **协调**:动手前对过文件,都没有同时改同一段:
  - 「Redis 上 Cluster 第 2a 批」会话(接着做 2b):`gate_redirect.go` 只由本批改键形(`gateRedisMarks` 加 zone 参数),它随后换句柄
    (`svc.CoreRedis` → `svc.Redis`)与那段「CoreRedis 必须指向 login 同一实例」的注释;`gate_redirect_draining_test.go` 的 `rds` 夹具由它换。
  - 「无感升级和重启方案」:它在 `gatetoken.go`(`GateCandidate.Build`)、`gatedrain_migrate.go`(`outgoingBuilds` / `preferIncomingBuild`)、
    `servicecontext.go` 的 `buildGateCandidates` 与 `GateDrain.Migrate.Enabled` 缺省值上的改动,本批按精确替换绕开。
  - 「IP端口变量在灰度升级中的处理」/ gate 排空迁移:`k8s_gate_drain.ps1` 与它的测试里有同区容量估算的改动,本批只改键字面量、
    `Get-GateDrainKeys`、redis-cli 参数与地址来源;`robot/gate_migrate_smoke_scenario.go` 已按新签名 `gatepick.DrainingKey(zone, node)` 调用。
  - 「Zone 人数满的处理」:`querystatuslogic.go` 已改名 `queryqueuestatuslogic.go`,本批不动它;`capacity.go` 的语义不动(只改两处注释里的键名)。
- **键形**(金样钉在 `go/shared/gatepick/marks_test.go`、`loginqueue/cluster_guard_test.go`、`k8s_gate_drain.tests.ps1`):
  - gate 标记 `gate:{gm:<zone>}:<id>:draining|drained|stale`。zone 是 gate 登记所在的 etcd 前缀里的 zone:login 用自己的 `Node.ZoneId`,
    排空脚本用它读到的 login ConfigMap 的 `Node.ZoneId`,scene_manager 跨 zone 重定向用目标 zone,三方是同一个数。
    §10.2 末尾说的 `{gm:<z>}` 断言放在 `gatepick/marks_test.go`(形状、登记过的族、同槽)与 scene_manager 的
    `gate_redirect_draining_test.go`(按目标 zone 拼键、别的 zone 的标记读不到),不并进 `TestControlDomainKeysCarryTheZoneTag`(它只管 ctl 族)。
  - 排队 `queue:{q:<z>}`、`admit:{q:<z>}:<queueId>`、`admitted:zset:{q:<z>}`、`queue:meta:{q:<z>}:<queueId>`、`queue:token:{q:<z>}:<token>`;
    放行锁 `<DispatcherLockKey>:{q:<z>}`。轮询与取走用验过签的令牌里的 zone 拼键;取走之后改占位改按这个 zone 拼 meta 键并核对 meta 的 `zone_id`。
- **句柄**:login 顶层新段 `ControlRedis`(`clusterredis.Config`,没写地址时回落 `Node.RedisClient` 那一个实例),gate 标记与(没配
  `Node.QueueRedis` 时的)排队都走它,经 `clusterredis.New(…, ControlClusterKeys)` 建;`Node.QueueRedis` 照旧可单独指排队实例,同一个构造器。
  放行流水线另建一个不挂守卫的句柄(`clusterredis.NewPinned`,每节点一条连接),`runAdmitScripts` 发之前自己按控制集群键策略核键,
  再取队列键所在槽的主节点发 pipeline;整块收到 MOVED / ASK / TRYAGAIN 时请求刷新路由表、这一拍停批,条目原样留在队列。
- **网关限流**:每个桶一段单键脚本(`TAKE_SCRIPT`),区桶空时另一段退还 IP 令牌(`REFUND_SCRIPT`);退还失败只打 WARN、照样回区桶空
  (偏严,不让一次退还失败把请求当成 Redis 故障放行并熔断)。客户端读 Spring 的 `RedisProperties`:写了 `spring.data.redis.cluster.nodes`
  就建 `RedisClusterClient`(开拓扑自适应刷新与 60 秒定期刷新),否则单实例;自动配置的连接(健康检查)读的是同一个属性,不另写代码。
  `ZoneHealthProbeService` 已不再注入 `StringRedisTemplate`(此前已删),本批不用动。
- **与原计划的偏差**:
  - 排空脚本的 redis-cli 保留 `-n <db>`(原计划去掉):集群只有 DB 0,传 0 合法,传别的值集群直接拒绝(fail-closed);单实例开发库号照样与
    login 对得上。另加 `-c`。脚本的 Redis 地址改为与 login 的 `EffectiveControlRedis` 同一取法(先 `ControlRedis.Addrs[0]`,再 `Node.RedisClient`);
    上了控制集群之后 exec 进哪个 Pod 跑 redis-cli 由第 6 批定,在那之前 ConfigMap 不渲染 `ControlRedis`。
  - K8s 上 login ConfigMap 写的 `DispatcherLockKey` 仍带 `:zone:<z>`(G1b 的纵深防御),实际锁键是 `dispatcher:lock:login_queue:zone:<z>:{q:<z>}`,
    tag 仍是本 zone 的 `{q:<z>}`,与排队键同槽。项目未上线、没有 G1b 之前的镜像可回退,这段纵深防御第 6 批可以去掉。
  - 新增一条起服校验:`DispatcherLockKey` 前缀含花括号拒启(花括号会顶掉本 zone 的 tag,控制集群句柄拒绝这把锁的每一条命令,本 zone 选不出 leader)。
- **没做(按分批留给后面)**:`-RedisSplit queue`、`Node.QueueRedis` 与 queue 实例的退役,`ControlRedis` 段、`spring.data.redis.cluster.nodes`
  与 `redis-control` 的渲染(第 6 批);scene_manager 读标记换句柄(第 2b 批);集群形态的运维手册(第 7 批)。

### 10.4 第 2b 批的落码、协调与偏差(2026-11-12)

- **协调**:动手前对过文件与段落,都没有同时改同一段:
  - 「分布式锁安全性与一致性」(R7):`leader_gate.go`(`SetLeaderElection`、`leaderFence`)、`load_refresh.go` 的带围栏 ZADD、
    `scene_manager_service.go` 的接线行都已落完;本批在 `load_refresh.go` 只改发送方式与判定,不动 `loadRefreshWireArgs` 与脚本。
    它请本批把 §4.7 里「R7 会补任期号」两句改成实际做法,已改。
  - 第 4 批(「服务器代码缺口补完」):`gate_redirect.go` 的键形与 `gate_redirect_draining_test.go` 的 zone 参数它先落完,本批随后只换句柄
    (`svc.CoreRedis` → `svc.Redis`)、形参类型与那段「CoreRedis 必须指向 login 同一实例」的注释,测试里的 `rds` 换成 `newTestControlRedis`。
  - 第 3 批(「Redis 上 Cluster 第 3 批:C++ 控制集群句柄与分片唤醒」):它改 `scene_node_client.go` 的 `createSceneOnNode` 并新建
    `scene_node_create_zone_test.go`,本批不碰这两处;已告诉它新测试文件不要导入 go-zero 的 Redis(守卫用例会拦)、玩家域的键走玩家域句柄。
  - 「所有服务中 redis mysql tidb etcd…」(断线重连):它在 `scene_registration.go` 世界频道登记加 KEYS[9](期望频道数),本批不动这个文件。
- **改法**:
  - **句柄**:`svc.ServiceContext.Redis` 从 go-zero `*redis.Redis` 换成 `*controlredis.Client`;`CoreRedis` 字段删掉,gate 标记(`gm` 族)
    与控制域键同在控制集群上,读它走同一个句柄。启动先 Ping,连不上拒启(原来 go-zero 的 `MustNewRedis` 同样先探一次);`Stop` 关它。
  - **形状适配**:`controlredis.Client` 只做形状适配,方法与 go-zero v1.10.0 同名同形;另加 `Do`(单发任意命令)、`AddHook`(单测在守卫之后
    注入钩子)、`UniversalClient`(给选主锁存储)。守卫、不重放、管道规则全部来自 `clusterredis`。
  - **配置**:`ControlRedis`(`clusterredis.Config`)取代嵌入的 go-zero `Redis` 段与 `CoreRedis` 段。`ValidateRedisSections` 在建任何客户端之前拒启:
    配置里还写着 `Redis.Host`(旧形状;照它起服控制域会落到回落地址 127.0.0.1:6379)、`ControlRedis.PasswordEnv`(变量缺失时静默落成空口令,
    口令变量名仍写 `SharedRedisPasswordEnv`)、控制集群是 Cluster 却没写 `PlayerRedis.Addrs`(玩家域回落过去会与别的服务分家)。
    没写 `ControlRedis.Addrs` 时回落本机 127.0.0.1:6379,池 20、读写 3s、取连接 4s、拨号 5s(换句柄之前 go-zero 句柄的口径);
    没写 `PlayerRedis.Addrs` 时回落到控制集群那一个实例(只给开发环境的单实例)。etc yaml 显式写出 `ControlRedis.Addrs` 与池、超时。
  - **选主**:锁存储换成 `leader.NewUniversalStore(svcCtx.Redis.UniversalClient())`。控制集群句柄按 ctx 截止时间约束 socket 读写,
    `maxCall` = 续期 ctx 超时 min(2s, TTL/8),`LeaderLockTTLSeconds` 的「TTL > 24s」下限随之取消(注释已改)。`store_gozero.go` 没有调用方了,删掉。
  - **负载分写批**:带围栏的 ZADD 是 EVAL,控制集群句柄的守卫不许它进管道(`ErrNonIdempotentInPipeline`),改成先发管道(类型镜像 SET、
    不带围栏的 ZADD)、再逐条单发 EVAL;「整批没执行」只对管道那部分判。go-zero 的熔断分支(`breaker.ErrServiceUnavailable`)删掉,
    `classifyCounterReply` / `classifyMemberCountReply` 去掉 batchErr 参数(整批被拒由调用方先按 `pipelineBatchRejected` 跳过)。
  - **测试**:三个夹具(`newTestSvcCtx`、`newTestSvcCtxWithWorldScenes`、`newIntegrationSvcCtxWithRedisOptions`)用生产同一个构造器
    (`newTestControlRedis`),钩子从 `redis.WithHook(h)` 改成直接传 `h`(go-zero 的钩子类型本来就是 go-redis 的别名);铸造替身改走玩家域句柄。
    测试里经 `sc.Redis` 读写玩家域键(location、owner_epoch、handoff、进场去重)与跑玩家域脚本(铸造、回滚、C++ 栅栏脚本)的 190 处,
    改走玩家域助手(`playerRedisGet` / `playerRedisEval` / 新增的 `setPlayerKeyForTest` 等):它们以前能过,只是因为两个域在测试里是同一个
    miniredis,换上控制集群守卫之后全部会报 `ErrWrongDomainTag`。
- **新增用例**:`controlredis` 包 7 条(缺键读法、有序集合形状、单键可以不带 tag、拒绝玩家域的键、多键混 tag、管道里的 EVAL 整批不发而单发照常、
  钩子排在守卫之后);`config` 包 `TestEffectiveControlRedisFallsBackToLocalInstance`、`TestEffectivePlayerRedisFallsBackToTheControlInstance`、
  `TestValidateRedisSections`;logic 包 `TestControlRedisHandleContract_NoGoZeroRedisInModule`(整个模块不许再导入 go-zero 的 Redis)、
  `TestLoadRefreshBatchesStayOnOneZoneTag`。
- **给第 6b 批的渲染要求**(本批之后,K8s 上旧渲染的 scene-manager ConfigMap 会拒启,第 6b 批落地之前不要部署 scene-manager):
  - 渲染 `ControlRedis:`:`Addrs` 是 `redis-control` 的种子(staging / prod `Cluster: true`;dev 档单实例 `redis.yaml`),
    `PoolSizePerNode`、`DialTimeoutMs`、`ReadTimeoutMs`、`WriteTimeoutMs`、`PoolTimeoutMs` 镜像 `go/scene_manager/etc/scene_manager_service.yaml`,
    `Password: ""` 加 `SharedRedisPasswordEnv`;不再渲染 `Redis:`(Host / Type / Pass / Key)与 `CoreRedis:`。
  - 控制集群是 Cluster 时必须渲染 `PlayerRedis.Addrs`(玩家域集群的种子)。
  - 跟着要改的脚本与契约:`ConvertFrom-RedisCtlLiveYaml`(按 `^Redis:` 读线上地址)、`redis-split-status` 的控制域一段、`-ConfirmRedisCtlSwitch`
    的线上核对、Redis 连接总账的 scene-manager 行(go-zero 缺省池 → `ControlRedis.PoolSizePerNode` 按每主节点记)、`k8s_deploy_contract.tests.ps1`
    里 scene-manager 的 `PasswordKeys`(`Redis.Pass`、`CoreRedis.Pass` → `ControlRedis.Password`)、`k8s_redis_ctl_contract.tests.ps1`、
    `k8s_redis_split_contract.tests.ps1` 的 scene-manager 断言。
- **没做(按分批留给后面)**:上面的渲染与契约(第 6b 批);运维手册与 `deploy/k8s/README.md` 里 scene-manager 的段名(第 7 批)。

### 10.5 第 3 批的落码、协调与偏差(2026-11-12)

- **分工**:第 3 批同时派给了两个会话,按先开工的一方提的分工落码:「Redis 上 Cluster 第 3 批:C++ 控制集群句柄与分片唤醒」做 C++、proto、
  CreateScene 校验 zone 与文档;同题会话 [0cc556] 只做 `go/shared/assetopwake`(签名不变,guild / trade / data_service 的调用点不动)。
  两边先对好频道写法与 C++ 声明的写法(§4.6),Go 用例逐字解析 C++ 头。
- **协调**:
  - 第 2a / 2b 批会话:`createSceneOnNode` 与假节点夹具只由本批改一处(填 zone、夹具记下 zone),它不动这两处;新测试文件不导入 go-zero 的 Redis。
  - 「控制面按 zone 化与控制域 Redis 分片」(第 6b 批):K8s 的 node-config 渲染、gate / battle 的口令环境变量、battle-node-config 归它;C++ 新配置段的
    契约已发给它(下一条)。
  - 第 5 批会话:订阅方三个服务的句柄与注释归它,本批不碰;频道新形状由 [0cc556] 告诉它。
  - 「Hiredis 生命周期收尾审核」:本批没碰三份 Hiredis 与 `RedisManager` 的连接逻辑,只删了两个 thread_local 句柄与注释,不用跑副本契约测试。
- **给第 6b 批的渲染要求**(本批之后、第 6b 批落地之前,K8s 上旧渲染的 node-config 让 scene 连到缺省地址 127.0.0.1:6379,不要部署 scene):
  - node-config 的 game_config.yaml:不再写 `zoneredis`、`scene_control_redis`;写 `control_redis: {mode, seeds, password: ""}`,种子是控制集群
    (staging / prod 是 `redis-control`,dev 档单实例),scene 的 Pod 经 Secret 拿 `CONTROL_REDIS_PASSWORD`。
  - gate、battle 不读任何 Redis 段、不要 Redis 口令环境变量;battle-node-config 不写 Redis 段。
  - 玩家域与控制域不在同一实例时(没开玩家域集群而玩家域在别处),必须显式渲染 `player_redis`:整段缺失时它整体沿用 `control_redis`。
  - 唤醒的发布与订阅必须在同一套 Redis 上:scene 发在 `control_redis`,guild / trade / data_service 的订阅句柄(第 5 批)指向同一个集群。
    `-RedisSplit ctl` 那种「控制域在 `redis-ctl-g<N>`、订阅方在核心」的形态下唤醒会静默丢失(退回原退避,不丢数据),第 6b 批退役它时一并消失。
  - 连接总账:scene 每个 loop 线程对每个控制集群主节点一条连接(玩家域另算);订阅方在集群上每副本 64 条订阅连接、散在各主节点,单机每副本 1 条。
- **与原计划的偏差**:
  - 订阅不用 `ChannelWithSubscriptions`(§4.6,理由在 PROGRESS「第 3 批 Go 部分」)。
  - 集群客户端加 `commandArgv` 与 `SideEffects`(§4.2):对账的 MGET 键数不定、格式串写不出;只读命令保住原来「不在维护前沿留 UNKNOWN」的口径。
  - 唤醒用 `command` 而不是原稿写的 `commandForKey`:SPUBLISH 自带路由键(频道)。
  - `player_redis` 写了这一段却没写种子时不再回落(原来 standalone 会回落到 `zoneredis`),由 scene 启动时拒绝:写了这一段就是另一个实例,
    悄悄回落会把玩家域落到控制集群上。
  - 玩家域种子的主机名解析改走 `ResolveRedisAddress`(R9 的有限次重试),与控制集群一致;原来解析失败一次就拒启。
  - 原稿 §4.2「gate、battle:删掉 K8s 模板里的口令环境变量」与「node-config 合成 `control_redis`」的渲染部分,按分批移到第 6b 批。
- **没做(按分批留给后面)**:K8s 渲染与部署契约(第 6b 批);订阅方注释(第 5 批);`deploy/k8s/AGENTS.md`、`deploy/k8s/README.md`、
  控制面设计稿里 `zoneredis` / `scene_control_redis` / `ZONE_REDIS_PASSWORD` 的说法(第 7 批);C++ 在真集群上的冒烟要等第 2b、5、6b 批都落地,
  本批只有 Go 订阅端的真 Redis 用例(PROGRESS 第 3 批条目的验证第 6 步)。

### 10.6 第 6a 批的落码与偏差(2026-11-12~13,「控制面按 zone 化」会话)

- **一份模板,一处差别**:`redis-player-cluster.yaml` 通用化成 `deploy/k8s/manifests/infra/redis-cluster.yaml`(占位 `__REDIS_CLUSTER_NAME__`、`__REDIS_CLUSTER_AUTH_SECRET__`、`__REDIS_CLUSTER_ORDINAL__` 等),旧文件删除。`k8s_deploy.ps1` 原来的 12 个玩家域专用函数整段换成按集群参数化的同一组函数(`Get-RedisCluster` / `Get-RedisClusters`、拓扑、容量、变更判定、预检、渲染、口令 Secret、apply、就绪等待、`Read-RedisClusterLiveStatus`);集群描述写成函数而不是脚本变量,因为不少契约测试只按 AST 抽函数定义。玩家域的产物不变。
- **参数**:`-ControlRedisMasters`(dev 缺省 0 不建,staging / prod 必须显式、≥ 3)、`-ControlRedisReplicasPerMaster`、`-ControlRedisMaxMemory`、`-ControlRedisStorage`,口令 `MMORPG_CONTROL_REDIS_PASSWORD` → Secret `redis-control-auth`,只进 infra namespace;zone namespace 的那份随第 6b 批接入服务时加(现在没有读者)。`dev_tools.ps1`、`k8s_image.ps1` 同名透传,追加在参数块末尾。
- **运维命令**:`redis-wipe` 的目标名单取自 `Get-RedisClusters`(以后加集群自动进名单);`redis-split-status` 末尾逐集群读 StatefulSet 与 `CLUSTER INFO`,没部署打 `<absent>`、不算失败,没就绪、`cluster_state` 不是 ok、`cluster_slots_ok` ≠ 16384、`cluster_known_nodes` ≠ 副本数都以退出码 1 结束。
- **连接总账**:`Get-RedisClusterConnectionLedgerInputs -Cluster` / `Write-RedisClusterConnectionLedger -Cluster`,玩家域的客户端表原样;控制集群在 6b 之前是空表,只打一行 `no clients wired yet`(总账函数对空表 fail-closed,不能拿 0 条冒充结论)。
- **告警**:`deploy/k8s/redis-control-alerts.yaml` 与 `redis-player-alerts.yaml` 一条对一条,表达式只差 pod 选择器,for 与 severity 相同,契约测试逐条比对;改阈值两边一起改。
- **手册**:`docs/ops/redis-player-cluster-runbook.md` 改成两套共用(命令里的集群名是 `$CLUSTER`,文件名不变)。
- **顺带**:match 私有集群镜像从 `redis:7.2` 升到同一个 8.10.2 摘要;遗留 `redis-rank.yaml` 删除;`redis_cluster_capacity.ps1` 的口令 Secret 缺省改成 `<statefulset>-auth`,反亲和标签取目标 StatefulSet 名,两套集群都能压。
- **偏差**:设计 §7 写的「Java 网关的集群节点」「各服务的 `ControlRedis` 段」「C++ node-config 的 `control_redis`」都在第 6b 批;compose 本机开发集群仍只有玩家域一套(开发时控制域用单实例 `redis`),要加第二套时另起。
- **给第 6b 批的已知事项**:第 3 批(C++)已不读 `zoneredis` / `scene_control_redis`,第 2b、5 批(Go)已删掉旧配置段,所以现在的 K8s 渲染在 6b 落地前起不来(scene 连不上、scene-manager 等服务拒启),相关部署契约测试在 6b 落地前是红的。

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
8. **改写量**:go-zero 句柄退役的改写量大(scene_manager 约 26 个文件),第 2b 批要单独评审。2026-11-12 第 2b 批用形状适配层(`controlredis.Client`)避开了逐处改写,改动落在配置、选主接线、负载分写批与测试夹具上;测试里经控制句柄读写玩家域键的 190 处改走玩家域句柄(§10.4)。

## 13. 与断线重连会话的分工(2026-11-10 约定)

- **R8**(MaxRetries 缺省 3 的句柄上自动重发非幂等 Lua,例如 friend 的申请配额)并入本稿的句柄替换批(第 2b、4、5 批)。断线重连会话不改这些建句柄的代码,它的清单里记成「随 Redis 分布式水平扩展的句柄替换批收掉」。
- **R7**(shared/leader 选主之后的写带任期号)归断线重连会话。约束:
  - 新增的键由锁键派生,与锁键同 tag,同一段脚本里两把键都写进 KEYS;
  - 只依赖 `UniversalClient`,不假设多条命令走同一条连接;
  - 任期号放在 Redis 里时,切主可能丢掉最后一次 INCR,任期会倒退。
  - **2026-11-12 落地结果**:没有加任期键。负载分刷新改为在脚本里核对锁键的值(`leader_gate.go` 的 `leaderFence`、`load_refresh.go` 的 `luaFencedLeaderZadd`),KEYS 同 `{ctl:<z>}`;世界频道登记的在役数判断归断线重连会话(`scene_registration.go`)。
- **文件清单**:双方动 5 个锁键调用点(`go/scene_manager/scene_manager_service.go`、`go/trade/internal/svc/assetchannel.go`、`go/guild/internal/svc/asset_op.go`、`go/player_locator/internal/logic/session_reconciler.go`、`go/player_inbox/internal/svc/servicecontext.go`)或任何建句柄文件之前,先把文件清单发给对方。
- **2026-11-12 shared/leader 的 UniversalClient 版 store(第 5 批)**:新增文件 `go/shared/leader/store_universal.go` 与测试 `store_universal_test.go`,导出 `NewUniversalStore(redis.UniversalClient) Store`,按现有 `Store` / `CallBounder` 接口写,锁语义与 `goRedisStore` 逐字一致。**只新增文件,`leader.go` 与现有两个 store 一行未改**(R7 会话同日给 `Elector` 加的 `Token()` 不受影响)。调用方:guild 资产维护选主、trade 资产维护选主、player_locator 对账选主。trade、player_inbox、guild 的选主锁下游都有按对象的条件写,**不需要围栏**;唯一要核对锁的是 scene_manager 的负载分刷新,由「分布式锁安全性与一致性」会话在写脚本里核对锁键仍是 `Elector.Token()` 的值(不是 etcd 任期号),scene_manager 的选主接线归它和第 2b 批。

## 修订记录

- 2026-11-10:初稿。依据三路只读盘点与断线重连会话的分工约定。
- 2026-11-11:第 1 批落码(未编译)。§5.2 补 C++ `KeyDomain` 的取值;shared/leader 的 UniversalClient store 从第 1 批挪到第 2b 批(第 1 批没有调用方)。
- 2026-11-12:第 2a 批落码(未编译)。§3.3 按实际做法改正文(DestroyScene 对别的 zone 的场景改为幂等成功、`ownZoneOfScene`、`removeScenePlayer` 先判属主、镜像源在别的 zone 的结局);§4.1 记下没建键包的理由;§10 表第 2a 行改状态,新增 §10.2。
- 2026-11-12:第 2b 批落码(未编译)。§4.1 的「句柄」「pipeline」两条按实际做法改正文;§4.7 与 §13 的 R7 改成实际落地的锁值围栏(不加任期号);§10 表第 2b 行改状态,新增 §10.4(含给第 6b 批的渲染要求);§12 第 8 条补结果。
- 2026-11-12:第 4 批落码(未编译)。§10 表第 4 行改状态,新增 §10.3(协调、键形、句柄、网关限流、偏差);§4.3 与 §4.4 的做法照原稿,偏差只在 §10.3 记。
- 2026-11-12:第 3 批落码(未编译)。§3.4 补 CreateScene 校验 zone 的实际做法;§4.2 补实际做法(`commandArgv`、配置段、停等的容量说明);§4.6 的订阅改成实际做法(不用 `ChannelWithSubscriptions`)与跨语言常量;§10 表第 3 行改状态,新增 §10.5(分工、协调、给第 6b 批的渲染要求、偏差)。
- 2026-11-13:第 6a 批落码(未运行)。§7「集群清单」按实际做法改正文;§10 表第 6a 行改状态,新增 §10.6。
