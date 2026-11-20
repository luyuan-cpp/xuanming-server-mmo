# 控制面按 zone 化与控制域 Redis 按 zone 分片(2026-11-05)

缺口清单 [server-gap-audit-20261101.md](../handoff/server-gap-audit-20261101.md) 第 4、5 条的设计与落码记录。容量依据是 [容量模型](capacity-model-hok-scale.md) §2.2 与 A1。控制域 Redis 的键形、句柄与部署以[全部 Redis 上 Cluster](redis-cluster-all-domains-20261110.md) 为准(§4.1)。

状态:设计已定,第 4a、4b、4c、5a、5b、5c 批 2026-11-05 已落码,验证状态见 §8。5a~5c 里按 zone 组分的控制域实例、`CoreRedis` 句柄与 C++ `scene_control_redis` 句柄,后来由全部 Redis 上 Cluster 第 2b、3、6b-1 批换成控制集群(§7 末尾)。Java 版只在 PARITY 记待做(§9)。

## 1. 结论

**第 4 条:控制面按 zone 化。**

1. C++ 节点对 zone-scoped 类型只 Range、只 watch `<Svc>.rpc/zone/<本 zone>/`。SceneManager 归入 zone-scoped,scene 与 gate 只连本 zone 的 scene_manager。
2. 每 2 秒一次的全前缀 Range 改为 60 秒兜底、±20% 抖动。正常靠 watch,断流、压缩和 watch 被取消时逐前缀重拉。
3. gate 每 10 秒重写一次 etcd 记录的做法不变,它是 login 与 scene_manager 判断 gate 停写的心跳(缺口第 7 条)。收窄监听方之后,这些写只扇出到本 zone 的监听者。这一点偏离容量模型 A1「人数不再写 etcd」,理由见 §3.2。
4. scene_manager 按 zone 选主,锁是 `scene_manager:leader:lock:{ctl:<z>}`。每个进程只服务配置里的 `ZoneId`,只 watch 这个 zone 的场景节点,`knownNodes` 只放这个 zone。
5. 判断别的 zone 的节点时,按需直读 etcd 的那个 zone 前缀,并用本副本「第一次看到它不在」的时刻做再入屏障(§3.5)。
6. K8s 上 go-zero 的发现键按 zone 加后缀 `.z<zone>`,login、player_locator 只连本 zone 的 scene_manager。本机多 zone 脚本 `go_services.ps1` 早就这样做。
   唯一的例外(2026-11-12):player_locator 的托管归位([player-connection-lost-hold.md](player-connection-lost-hold.md) §4.5)把 EnterScene
   发给角色所在 zone 的 scene_manager —— 租约监视按 slot 分片、不按 zone 分,认领到的托管角色可能在别的 zone,而 scene_manager 只放本 zone
   的落点。别的 zone 的发现键由本 zone 的键换后缀得到,按需建客户端(`go/player_locator/internal/svc/zone_scene_manager.go`);
   键不带后缀的单 zone 部署只连本 zone。离场通知(LeaveScene)仍发给本 zone 的 scene_manager。

**第 5 条:控制域 Redis 进控制集群,每个 zone 的键共用一个 hash tag。**

2026-11-05 原稿按 zone 组分单主实例(`redis-ctl-g<N>`)、不上 Cluster;2026-11-10 负责人改口径(「我要分布式水平扩展」,按 zone 组分实例不算完成),改成下面的做法(§4.1)。

1. 控制域全部在控制集群 `redis-control` 上(K8s staging / prod;dev 档与本机缺省是单实例 `redis`,客户端走与集群相同的路径和守卫)。每个 zone 的控制域键共用 `{ctl:<z>}`,扩容 = 加主节点 + 原子迁 slot,zone 随 slot 散到各主节点。
2. 键名带本 zone 的 tag,例如 `scene:{ctl:<z>}:<id>:node`、`node:{ctl:<z>}:<n>:*`。原来全服唯一的三把键:选主锁、死节点收尾任务表改按 zone 起名;zone 登记集合 `scene_manager:zones` 删除。
3. **不变量**:zone z 的控制域键只由 zone z 的 scene_manager 读写。别的 zone 的事实只从与 zone 无关的介质取:etcd、玩家域 Redis、Kafka、重定向票据(§4.3)。它是按 zone 打 tag 的前提。
4. 所有脚本和 MULTI 的键本来就只属于一个 zone(§4.4),同 tag 即同槽,不需要拆。
5. C++ scene 只有一个控制集群句柄 `tlsControlRedis`(配置段 `control_redis`):读分线目录、神秘商人,做归属对账,并把落点唤醒 `SPUBLISH` 到 64 个分片频道;订阅方 guild、trade、data_service 连同一个控制集群。gate、battle 不连 Redis(§4.5)。

## 2. 现状(2026-11-05 重新核对)

缺口清单的证据行号是 11-01 的,这里按当天工作区重新核对。

| 项 | 位置 | 现状 |
|---|---|---|
| C++ 全前缀 Range | `service_discovery_manager.cpp:16-18`、`:27-33` | 每 2 秒对 `service_discovery_prefixes` 的 13 个前缀各 Range 一次,前缀不带 zone |
| C++ watch | `etcd_service.cpp:158-167`、`:331-345` | 每个前缀一个 watch,前缀不带 zone;断流后逐前缀重拉;非压缩原因的取消等下一次 2 秒 Range 重建 |
| zone-scoped 判定 | `node_util.cpp:120-136` | Gate、Scene、Login、PlayerLocator、PlayerStore;别的 zone 的这几类节点拉回来后在 `AddServiceNode` 丢掉,白拉 |
| gate 人数写 etcd | `cpp/nodes/gate/main.cpp:557-562` | 每 10 秒 `UpdateNodeInfo`;watch 的人全服都收 |
| scene / gate 连 scene_manager | `cpp/nodes/scene/main.cpp:107-109`、`cpp/nodes/gate/main.cpp:376-378`、`node_utils.cpp` 的 `SelectSceneManagerEntity` | SceneManager 不是 zone-scoped,连全服全部实例,按 player_id 取模挑一个 |
| scene_manager 选主 | `scene_manager_service.go:94-97` | 一把全服锁 `scene_manager:leader:lock` |
| scene_manager watch | `load_reporter.go:31`、`:708`、`:899` | `SceneNodeService.rpc/` 全服前缀,`knownNodes` 是全服镜像 |
| 领导者循环 | `load_reporter.go:810-815`、`:974`;各循环的 `GetActiveZones()` | 对镜像里出现的全部 zone 跑 |
| 跨 zone 的注册表判定 | `load_reporter.go:192-211` | `isZoneGoneFromRegistry` 依赖全服镜像,文件注释写明收窄前要先改 |
| go-zero 发现键 | `k8s_deploy.ps1:10275`、`:10395` | K8s 上 `scenemanagerservice.rpc` 不带 zone,login 与 player_locator 连全服 scene_manager |
| 控制域 Redis | `k8s_deploy.ps1:10396-10400` | 每个 zone 的 scene-manager 都连核心 `redis.<infra>` |
| 场景键 | `scene_atomic.go:279-288`、`createscenelogic.go:20-74` | `scene:<id>:node` 等只按场景号,不带 zone、没有 hash tag(2026-11-12 第 2a 批起改为 `scene:{ctl:<z>}:<id>:node`) |
| 全服键 | `reentry_barrier.go:238`、`zone_registry.go` | `scene_manager:dead_node_reconcile`、`scene_manager:zones` |
| C++ 读控制域 | `player_scene.cpp:59`、`player_mystery_merchant.cpp:52`、`player_landing.cpp:255` | scene 经 `zoneredis`(核心)读分线目录、神秘商人,并 PUBLISH 落点唤醒 |

## 3. 第 4 条设计

### 3.1 C++ 发现收窄

- 新增 `NodeUtils::DiscoveryPrefixFor(prefix, zoneId)`:zone-scoped 类型返回 `<prefix>/zone/<zoneId>/`,其余原样返回。`FetchServiceNodes` 与 `OnWatchStreamRestarted` 都经它算前缀,两处不会不一致。
  - 前缀收窄之后,分配键 `<Svc>.rpc/allocated/...` 自然不在 zone-scoped 类型的发现前缀里(A1「分配键移出发现前缀」);全局池类型的分配键仍在,它们个数很少。
  - 自检不受影响:劫持检测比对的是本节点自己的发现键,它在本 zone 前缀下;分配键的删除在发现层本来就跳过。
- `IsZoneScopedNodeType` 加 SceneManagerNodeService。scene_manager 早已按 `ZoneId` 注册在 `SceneManagerNodeService.rpc/zone/<z>/` 下;收窄后 scene / gate 只连本 zone 的实例。前提是**每个有 scene / gate 的 zone 都部署了 ZoneId 等于它的 scene_manager**(§3.7 的部署闸)。
- 兜底周期:`kDiscoveryResyncIntervalSeconds = 60`,每次排下一拍时乘 [0.8, 1.2] 的随机系数(`RunAfter` 链,不是 `RunEvery`),多个进程不会同时打 etcd。启动时那一次 Range 照旧立即发出,它是建 watch 的前提。
- watch 被取消:
  - 有 `compact_revision` 时立即重拉(现有逻辑);
  - 其余原因以前等 2 秒 Range,现在改成按前缀退避重拉:1 秒起、翻倍、封顶 30 秒,成功建 watch 后清零。对端持续拒绝时不空转。
- 量级:etcd Range 次数从 13·N_cpp/2 s 降到 13·N_cpp/60 s;每次 zone-scoped 前缀只回本 zone 的键,总 KV 读从约按 N_cpp² 增长降到 Σ_z N_z²/60。watch 扇出从 (N_g/10)·N_cpp 降到 Σ_z (N_g,z/10)·N_z。

### 3.2 gate 人数仍写 etcd

容量模型 A1 建议「player_count 不再写 etcd」。2026-11-05 缺口第 7 条落码后,这次写同时是停写判定的心跳:login 镜像与 scene_manager 跨 zone 重定向都按 ModRevision 判断 gate 有没有停写,权威常量是 `go/shared/gatepick.ReportPeriod`。拿掉它要另造心跳,不会更简单。

平方项来自监听方是全服,不来自写本身。收窄之后每次写只送给本 zone 的 gate、scene、login 和 player_locator,再加少量全局服务。写入量 N_g/10 每秒本来就是线性的。**不改上报周期**,要改必须与 `gatepick.ReportPeriod` 同批。

### 3.3 scene_manager 按 zone 选主与收窄

- 选主锁 `scene_manager:leader:lock:{ctl:<z>}`,`z` 是配置的 `ZoneId`,锁与 z 的控制域键同 tag、同槽。`LeaderLockKey` 配置项删除(只有一处读取,没有任何配置写它):锁名由 zone 决定,配置写错就会让两个 zone 抢一把锁,或一个 zone 没人编排。
- 一个进程只编排一个 zone。领导者的全部循环(补频道、rebalance、死节点清扫与收尾、空闲回收、自动扩缩容、分线目录、神秘商人、成员对账、孤儿频道清理、Agones 对账)只处理 `ZoneId`。`GetActiveZones()` 改名为 `orchestratedZones(svcCtx)`,恒返回 `[ZoneId]`;本 zone 一个节点都没有时照样扫,能清掉最后一个节点死后留下的负载集与孤儿实例,原来靠 zone 登记集合补的那个口子随之消失。
- watch 与全量同步的前缀改为 `SceneNodeService.rpc/zone/<ZoneId>/`,`knownNodes` 只有本 zone;快照或 watch 里自称别的 zone 的注册按部署错配丢弃(watch PUT 按删除收尾)。存活集闸门(第 9 条,`node_view_gate.go` 的 `sceneNodeViewGate.check`)按 zone 计算,读基线与清扫的 zone 范围 `nodeViewZones(svcCtx)` 恒为本 zone。
- 每节点的 Prometheus 序列只有本 zone 的领导者导出;`nodes_by_role` 只发本 zone。
- 数据面:一个进程只收本 zone 的请求(C++、login、player_locator 都只连本 zone)。请求可能涉及别的 zone:跨 zone 重定向的目标,或玩家位置落在别的 zone。这些情况按 §3.5、§4.3 处理。

### 3.4 knownNodes 的读者

| 读者 | 收窄后 |
|---|---|
| `isNodeGoneFromRegistry` / `isZoneGoneFromRegistry` / `isKnownNodeIdentityAmbiguous` / `knownNodeIdentityMatchCount` | 本 zone 读镜像;别的 zone 走 §3.5 |
| `knownNodeRegisteredInstance` / `knownInstanceRegistered`(GO-3) | 同上 |
| `resolveFromKnownNodes` 与 etcd 慢路径 | 慢路径的 Get 改成那个 zone 的前缀,不再拉全服 |
| `FindNodeByPodIP` / `FindPodIPByNodeID`(Agones) | 只服务本 zone 的分配,镜像只有本 zone,正好 |
| 负载刷新、`nodes_by_role` | 只有本 zone |

### 3.5 判断别的 zone 的节点

收窄之后,本副本对别的 zone 没有 watch 观察。原来用的负载集与 death_at 都是别的 zone 的控制域键(`{ctl:<z>}` 是那个 zone 的),按 §4.3 的不变量不能读。改为两样与 zone 无关的证据:

1. **etcd 线性一致读**那个 zone 的前缀 `SceneNodeService.rpc/zone/<z>/`(超时 2 秒)。读失败一律按「拿不到证据」处理,fail-closed。
2. **本副本第一次看到它不在的时刻**。节点级记在 `nodeGoneObservedAt`,zone 级记一张新表;只在「不在」时首次写入,看到它在册就清掉。满一个再入屏障才算死亡证据。

为什么这和原来一样安全:death_at 是 z 的领导者第一次观察到节点消失的时刻;本副本的首次观察只会更晚,屏障结束点也跟着更晚。负载集只是 etcd 注册的派生物,注册已消失时它不提供更强的证据。被接管之后僵尸写入由 owner_epoch 挡住,这一点与本 zone 相同。

代价:首次观察时一定回可重试的拒绝,玩家要多等一个屏障(默认约 20 秒)。只发生在「位置落在别的 zone、而那个节点死了」的少见路径上。

对应的函数:

- `playerLocationOwnerDead`、`playerLocationOwnerGone`、LeaveScene 的 `judgeHolderRelease`:别的 zone 走新的 `foreignOwnerDead` / `foreignZoneGone`,不读负载集、不读 death_at;
- `playerLocationOwnerReplaced`:在册实例从那个 zone 的 etcd 快照取。它原来就以「本次复核」为首次观察,做法不变。

### 3.6 go-zero 发现键

K8s 上 scene_manager 服务端的 `Etcd.Key` 和 login、player_locator 的 `SceneManagerRpc.Etcd.Key` 都渲染成 `scenemanagerservice.rpc.z<zone>`,与 `go_services.ps1` 本机多 zone 的后缀规则一致。单 zone 本机配置不带后缀,三方相同。data_service 等全局服务的键不加后缀。

### 3.7 部署闸

C++ 只连本 zone 的 scene_manager 之后,一个 zone 没有部署 scene_manager 就进不了场景。K8s 每个 zone namespace 都部署 scene-manager(`k8s_deploy.ps1` 的 Scope = zone),本机多 zone 脚本每个 zone 各起一个。zone-up 渲染时核对:scene-manager 的 `ZoneId` 与 node-config 的 `zone_id` 相同,不同就拒绝。

## 4. 第 5 条设计

### 4.1 选型

两个候选:按 zone 组分单主实例;或者上 Redis Cluster,给控制域键加按 zone 的 hash tag。现在是后者。2026-11-05 原稿取前者(理由是 §4.3 的不变量之后一个实例就够、键名不用改),2026-11-10 负责人改口径:按 zone 组分实例不算水平扩展,控制域也要能加主节点扩写入。

1. **一个控制集群,按 zone 打 tag**。控制域全部键在 `redis-control` 上,每个 zone 的键共用 `{ctl:<z>}`(族前缀登记在 `go/shared/redisslot/control_keys.go`,C++ `redis_slot.h` 是镜像,金样测试逐字核对)。脚本、MGET、pipeline 本来就只碰一个 zone 的键(§4.4),改名之后天然同槽,脚本逻辑不动。
2. **§4.3 的不变量照用**。它保证一个 zone 的控制域只读写自己的 tag,是按 zone 打 tag 的前提,也是不需要任何跨 tag 原子操作的原因。
3. **扩容在线做**:加主节点 + 原子迁 slot(Redis 8.10.x 的 `CLUSTER MIGRATION`),数据和读写一起搬过去,不停写。原稿「zone 换组只能停写迁移」的代价随之消失。
4. **句柄**:scene_manager 的控制域句柄是 `shared/clusterredis` 上的 `controlredis.Client`(控制集群键策略:多键命令同 tag 且是登记过的族,玩家域的 tag 一律拒绝;结果未知不重放),go-zero 的 `Redis` 段与 `CoreRedis` 段退役;C++ scene 一个 `tlsControlRedis`(§4.5)。本机单实例走同一条路径和守卫。

代价:一个 zone 的控制域键落在一个槽上,单 zone 的上限是一台主节点。到顶时把这个 zone 的槽迁到独占的主节点,再大就拆 zone,不拆脚本里的键([全部 Redis 上 Cluster](redis-cluster-all-domains-20261110.md) §6.2、§6.3)。

### 4.2 键的归属

| 键族 | 放在哪 | 谁读写 |
|---|---|---|
| 场景路由 `scene:{ctl:<z>}:<id>:node|source|mirror|mirrors|draining|agones_gs|teardown|registration|placement_gen`、成员 `instance:{ctl:<z>}:<id>:members`、`node:{ctl:<z>}:<n>:*`、`instances:{ctl:<z>}:active`、`scene_nodes:{ctl:<z>}:load`、世界频道 `world_channels:{ctl:<z>}:<conf>` 与 `world_channels:desired|cooldown|draining:{ctl:<z>}…`、分线目录与线号、神秘商人、`world_init:lock:{ctl:<z>}` | 控制集群,本 zone 的 `{ctl:<z>}` | 本 zone 的 scene_manager;C++ scene 只读分线目录、神秘商人,以及归属对账读的本地场景 `scene:{ctl:<z>}:<id>:node` 与本节点号的 `node:{ctl:<z>}:<n>:holder`(2026-11-06 / 11-10,scene-node-ownership-reconcile.md) |
| 选主锁 `scene_manager:leader:lock:{ctl:<z>}`(原 `scene_manager:leader:lock`) | 同上 | 本 zone 的 scene_manager |
| 死节点收尾任务 `scene_manager:dead_node_reconcile:{ctl:<z>}`(原全服一张) | 同上 | 本 zone 的领导者 |
| etcd 修订号水位 `scene_manager:etcd_revision_seen:{ctl:<z>}` 与 etcd 纪元 `scene_manager:etcd_epoch:{ctl:<z>}`(2026-11-10,节点号持有者认 etcd 倒退用,scene-node-ownership-reconcile.md §4.6) | 同上 | 本 zone 的领导者写;纪元由各副本写持有者时在同一段 Lua 里读 |
| `scene_manager:zones` | **删除** | — |
| gate 排空标记 `gate:{gm:<zone>}:<id>:draining|drained` | 控制集群,按 zone 的 `{gm:<zone>}` | `k8s_gate_drain.ps1` 与 login 写;login、scene_manager 只读,scene_manager 经控制集群句柄(第 2b 批起,`CoreRedis` 已删) |
| gate 停写标记 `gate:{gm:<zone>}:<id>:stale`(2026-11-10,键名在 `go/shared/gatepick/marks.go`) | 控制集群,与排空标记同 tag | login 各副本写(本 zone 判为停写的 gate,过期 30s、不主动删,经 login 的 `ControlRedis` 句柄);scene_manager 只读,与排空标记同一次 MGET(gate-load-balancing-design.md「满载与停写的 gate」) |
| 落点唤醒频道 `assetop:wake:{aw:<pid % 64>}`(64 个分片频道) | 控制集群 | C++ scene `SPUBLISH`;guild、trade、data_service 按分工只订本副本负责的片(全部 Redis 上 Cluster §4.6) |
| 玩家域 `player:{<id>}:*`、进场去重 | 玩家域集群 `redis-player`(不动) | 经 `PlayerRedis`。scene_manager 的 `PlayerRedis` 没写地址时回落到 `ControlRedis` 那一个实例,只给本机单实例开发;控制域是 Cluster 时不写 `PlayerRedis.Addrs` 拒启(`ValidateRedisSections`),K8s 上开控制集群必须同时开玩家域集群(`Assert-RedisClusterPairing`),否则 location / owner_epoch 会与别的服务读写的那一份分家 |

场景键带本 zone 的 tag(2026-11-12 第 2a 批起)。只给场景号的入口一律按本进程的 zone 拼键,别的 zone 的场景读成「不存在」,`scene:<id>:zone` 已删(全部 Redis 上 Cluster §3.3)。按 §4.3,只读写本 zone 的场景。

### 4.3 不变量:zone z 的控制域键只由 zone z 的 scene_manager 读写

全部 zone 的控制域键在同一个控制集群里、又各自选主之后,有一类缺陷:领导者「从 Redis 里发现有哪些 zone」,结果把别的 zone 也扫了。最危险的是 `sweepStaleLoadSetMembers`:本副本的镜像里没有那个 zone 的节点,它会把那个 zone 的活节点全当成陈旧成员,写 death_at 并摘出负载集。为此改了这些地方:

1. zone 登记集合删除。`sweepStaleLoadSetMembers`、`CleanupOrphanWorldChannels`、成员对账只处理 `ZoneId`。
2. 数据面上凡是可能带别的 zone 的入口,都显式判一次 `ownsControlZone(zone)`:
   - 跨 zone 传送的预检 `rejectTravelToUnopenedMap`:目标 zone 恒不是本 zone,原来 SCARD 目标 zone 的频道集合。改为与 zone 无关的判定:地图在 World 表里,且 etcd 里那个 zone 至少有一个能用来落地的大世界节点,判据与选点相同(没在计划排空、身份不歧义、类型对,`hasWorldHostingRegistration`)。它原本就是 fail-open 的提前拒绝,不是安全门(etcd 读失败照旧放行)。与原判定的差别在「目标 zone 刚起、节点已注册但领导者还没铺频道」的几秒:原来拒,现在放行;第二条腿在一个频道都没有时当场补铺,领导者还没把节点放进负载集时在请求期限内等它(§6 第 1 条)。
   - 成员集合的增减(`addSceneMember` / `removeSceneMember` / `removeScenePlayer`):别的 zone 一律不动,只记 `scene_manager_foreign_zone_skip_total{site}`。成员集合是派生索引,权威是玩家域的 location;别的 zone 留下的陈旧成员由那个 zone 的成员对账在 2 分钟宽限后摘掉(`scene_member_reconcile.go`,redis-player-store-cluster.md §5.10)。没摘过的成员,回滚时也不需要加回。
   - `removeScenePlayer` 只给场景号、不给 zone 时(LeaveScene 的两处清理),按本进程的 zone 看 `scene:{ctl:<z>}:<id>:node` 在不在:不在(场景在别的 zone,或已销毁)就不动。(2026-11-12 起:控制域键带 zone 的 tag,`scene:<id>:zone` 删除,redis-cluster-all-domains-20261110.md §3.3。)
   - 别的 zone 的节点判活见 §3.5,不读那个 zone 的负载集和 death_at。
   - 建镜像时源场景只按本 zone 的键查(2026-11-12 起,redis-cluster-all-domains-20261110.md §3.3):源在别的 zone 时本 zone 的键里没有它,与源已销毁同一个结局——源克隆镜像回 `ErrSourceSceneGone`;带自己配置的镜像照建,共置按 `no_mapping` 回落,子集合与反向索引写在本 zone 的键里(`scene:{ctl:<z>}:<源>:mirrors`、`scene:{ctl:<z>}:<镜像>:source`),源所在 zone 的级联读不到它们,随镜像销毁摘掉。(原来按 `scene:{源}:zone` 认出别的 zone、不登记子镜像,是 2026-11-06「场景销毁认领」会话报的漏口;那把键已删。)
   - CreateScene 与 EnterScene 一样,请求的 zone 不是本进程的 zone 就在任何写入之前拒绝。
   - 销毁路径:认领只读本 zone 的键(2026-11-12 起场景键带 zone 的 tag),别的 zone 的场景读不到映射,按映射不存在处理、一个键都不动;镜像级联与源迁移级联对不在本 zone 的子镜像认领落空、不拆;镜像拆除从本 zone 的源子集合摘自己。(2026-11-06「场景销毁认领」会话加的 `scene:{id}:zone` 比对与 `foreign` 结局随那把键删除;显式 DestroyScene 拿到只登记在别的 zone 的场景,从回错误改为与「映射已不存在」同样幂等成功。)显式 DestroyScene 的 `zone_id` 不是本进程的 zone 就在任何读写之前拒绝,没填按本进程的 zone。详见 scene-creation-architecture.md「销毁只改本 zone 的键」。
3. 别的 zone 的键在同一个集群里,读得到也不读:哪些 zone 的槽落在同一个主节点上是部署细节,行为不能随它变。

为什么不会丢数据、不破坏一致性:

- **编排单写者**:每个 zone 只有一把锁、一个领导者,与原来全服一把锁的单写者语义相同,只是范围变成一个 zone。
- **原子范围不变**:每段脚本的键都属于同一个 zone、共用 `{ctl:<z>}`,在同一个槽上(§4.4)。
- **权威数据不在控制域**:位置、owner_epoch、交接标记都在玩家域。控制域里的成员集合与负载集是派生索引,由对账收敛。
- **跨 zone 的动作原来就不是原子的**:跨 zone 传送的两条腿由两个 EnterScene 完成,中间靠玩家域的「等待落点」与重定向票据衔接(cross-zone-scene-travel.md CZ-4、CZ-5),本设计不改这一点。

### 4.4 脚本逐个核对(2026-11-05)

| 脚本 | 位置 | 键 | 结论 |
|---|---|---|---|
| 三段认领 / 空闲回收 / force 销毁 | `scene_atomic.go` `sceneClaimKeys` 及 `:367`、`:385` | 场景键 + `instances:{ctl:<z>}:active` + `node:{ctl:<z>}:<n>:*` | 同一 zone |
| 加成员 / 选最空频道 | `scene_atomic.go:398-454` | 场景键 + `node:{ctl:<z>}:<n>:members` | 同一 zone(候选都是本 zone 频道) |
| 成员增减 | `scene_membership.go` `sceneMemberKeys` | 场景成员 + 节点成员 | 同一 zone |
| 场景登记 | `scene_registration.go:182-191` | 场景键 + 节点计数 / 反向索引 + 频道集合 | 同一 zone |
| 改派 | `world_init.go` `casReassignSceneNode` | 场景键 + 频道集合 | 同一 zone |
| 排空 | `world_autoscale.go:425-431` | 频道集合 / 排空集合 / 期望数 + 场景键 | 同一 zone |
| 线号 | `world_channel_directory.go:173` | 一个键 | — |
| 神秘商人 | `mystery_merchant.go:421` | 一个键 | — |
| Agones 名额转移 | `agones_binding.go:377` | 场景键 | 同一 zone |
| 死节点收尾入队 / 出队 | `reentry_barrier.go:337`、`:507-512` | 任务表 + `node:{ctl:<z>}:<n>:*` + 负载集 | 原任务表是全服键;改成按 zone 之后同一 zone |
| 选主锁 | `shared/leader` | 一个键 | — |

没有跨 zone 的原子操作,不需要拆。以后新增脚本,键必须属于同一个 zone(共用 `{ctl:<z>}`,在控制集群上同槽);契约测试 `control_zone_keys_contract_test.go` 钉住两个改名键的形状,以及「没有全服键」。

### 4.5 C++ scene 的控制域句柄

2026-11-12 全部 Redis 上 Cluster 第 3 批起(该稿 §4.2、§10.5):

- `game_config.yaml` 的 `control_redis`(`GameConfig.control_redis = 12`,类型 `RedisClusterConfig`:`mode` standalone | cluster、`seeds`、`password`)。整段或单键缺失取 standalone、`127.0.0.1:6379`,开发环境不用改。口令可被环境变量 `CONTROL_REDIS_PASSWORD` 覆盖,值不进日志。11-05 加的 `scene_control_redis = 11` 与原来的 `zone_redis = 7` 已改 `reserved`;yaml 里还写着旧段只打一条 WARN,不再读。
- scene 进程一个控制集群句柄 `tlsControlRedis`(`RedisClusterClient`,键策略 `kMultiKeyOnly` + `KeyDomain::kControl`)。用它的有 `player_scene.cpp` 的分线目录 GET、`player_mystery_merchant.cpp` 的神秘商人 HGET、`world/scene_ownership_reconciler.cpp` 的归属对账 MGET(本地场景的 `scene:{ctl:<z>}:<id>:node` 与本节点号持有者,只读,tag 取 `GetZoneId()`,scene-node-ownership-reconcile.md),以及落点唤醒 `SPUBLISH assetop:wake:{aw:<pid % 64>}`。
- 对账只能按本节点的 zone 打 tag,前提是「一个 scene 节点只承载本 zone 的场景」:CreateScene 请求带 `zone_id`,节点发现不是本节点的 zone 就拒绝创建(全部 Redis 上 Cluster §3.4)。
- gate、battle 不连 Redis。

### 4.6 K8s

现状以全部 Redis 上 Cluster §7、§10.6、§10.7 为准(第 6a、6b-1 批,代码写完、未运行),这里只列与本稿相关的几项:

- **集群**:控制集群 `redis-control` 与玩家域集群 `redis-player` 共用一份模板 `deploy/k8s/manifests/infra/redis-cluster.yaml`,两套的差别只在 `k8s_deploy.ps1` 的 `Get-RedisCluster -Kind control`。参数 `-ControlRedisMasters`(缺省 0 不建,只有 dev 可以;staging / prod 必须显式、至少 3)、`-ControlRedisReplicasPerMaster`(缺省 1)、`-ControlRedisMaxMemory`、`-ControlRedisStorage`(dev 留空取 256mb、2Gi,staging / prod apply 时必须给);口令 `MMORPG_CONTROL_REDIS_PASSWORD` → Secret `redis-control-auth`。开控制集群必须同时开玩家域集群(`Assert-RedisClusterPairing`)。
- **控制端点**:全部控制域客户端从 `Get-ControlRedisEndpoint` 取地址与口令:开了控制集群是 `redis-control` 的种子,没开(只有 dev)是核心 `redis.<infra>:6379`。
- **scene-manager ConfigMap**:渲染 `ControlRedis` 段(`New-ControlRedisConfigYaml`,池上限与超时镜像服务 yaml,口令恒写空串,由 `SharedRedisPasswordEnv` 指名的 `SHARED_REDIS_PASSWORD` 填);旧的 `Redis` / `CoreRedis` 段不再渲染,配置里还写着 `Redis.Host` 就拒启;`Etcd.Key` 加 zone 后缀(§3.6)。
- **C++ node-config**:scene 写 `control_redis`(种子同上)与 `CONTROL_REDIS_PASSWORD`,Pod 模板带配置指纹 `mmorpg.io/control-redis-config-hash`;gate、battle 不写 Redis 段、不注入口令。
- **连接账**:控制集群单独一份每主节点的账(`Get-RedisControlClientRows -Cluster $true`,打印 `Redis control connection ledger`),唤醒订阅每副本按 64 条记;dev 档控制端点是核心时,控制域的行并进核心那份。
- **清库与状态**:`redis-wipe` 逐主节点清 `redis-control`;`redis-split-status` 列出两套集群的就绪与 `CLUSTER INFO`,有问题退出码 1。
- **告警**:控制集群本身的规则在 `deploy/k8s/redis-control-alerts.yaml`(与 `redis-player-alerts.yaml` 一条对一条)。scene-manager 运维文本里「`sum(scene_manager_is_leader)` 应恒为 1」改为「每个 zone 恒为 1」;负载刷新的 `registry_failed` 随 zone 登记集合删除。
- **退役**:11-05 加的 `redis-ctl-g<N>` 实例、`-RedisCtlMaxMemory` / `-ConfirmRedisCtlSwitch` 与切换闸、按组的连接账与状态检查,第 6b-1 批随 `-RedisSplit` 一起删除;`dev_tools.ps1`、`k8s_image.ps1` 改为透传四个 `-ControlRedis*` 参数。

## 5. 被否决或推迟的做法

- **按 zone 组分单主实例**(2026-11-05 原稿的做法,§4.1):换组只能停写迁移,一个组的写入上限是一个主节点。负责人 2026-11-10 定为不算水平扩展,第 6b-1 批退役。
- **控制域按场景号分片**:脚本把场景键和 zone 级集合绑在一起(§4.4),拆开就丢原子性([全部 Redis 上 Cluster](redis-cluster-all-domains-20261110.md) §0 第 7 条)。
- **scene_manager 之间转发跨 zone 请求**:要新增 RPC 和跨 zone 发现。§3.5 与 §4.3 用 etcd 和玩家域已经够了。
- **读别的 zone 的控制域键**:控制集群里读得到,§4.3 不许读。那些键只由那个 zone 的领导者维护;合服下线的 zone 没有领导者,键只剩没人维护的残留(§6 第 10 条),不能拿来判断节点死活。§3.5 的 etcd 判定在 zone 下线后照样能用。
- **gate 人数不写 etcd**:见 §3.2。

## 6. 残余与风险

1. 跨 zone 传送预检与目标 zone 刚起的那几秒(2026-11-12 修掉)。预检只读 etcd,看「目标 zone 有没有能用来落地的大世界节点」(§4.3 第 2 条);原来读频道集合时会拒掉的那几秒(节点已注册、频道还没铺)现在放行,第二条腿过去以前会落不了地。这时源 scene 已经销毁了实体,客户端只提示「进入场景失败,请稍后再试」,玩家要自己重进。修了三处:
   - 第二条腿与所有进场一样,一张大世界图一个频道都没有时当场补铺再挑(`world_channel_on_demand.go`),与 CreateScene 早有的慢路径同一个做法。补铺只用负载集里的节点:负载集由领导者维护,体现它对节点存活的裁决。负载集里还没有可用节点、本副本的 etcd 镜像里却已经有可用注册,说明领导者还没完成第一次同步,在请求期限内按退避(100 ms 起、封顶 1 s、±20% 抖动,最多 5 s)等它;镜像里也没有就立刻失败。同一进程同一 (zone, 图) 同一时刻只补一次,多副本之间靠 zone 锁串行。
   - 预检改用与选点同一个判据(`hasWorldHostingRegistration`):计划排空中(admission CLOSED)的节点、身份歧义的节点不算。原来只看节点类型:目标 zone 的大世界节点全在排空时预检放行,第二条腿却选不到点,排空持续多久就失败多久。
   - 「有节点、频道暂时全不可用(满员、属主刚死)」照旧放行:第二条腿懒改派,指定的地图落不进去时回落默认大世界。
   剩下的只有目标 zone 的 scene_manager 进程自己还没起、或领导者超过 5 秒还没同步完,这种情况同 zone 的首登同样失败,属于 zone 整体不可用,不是本设计的口子。
2. 位置落在别的 zone、持有节点已死时,玩家要多等一个再入屏障(§3.5,默认约 20 秒)。这是有意的安全余量,不是待修项:屏障防的是失租后还在跑的老进程最后几秒的写入,只能从「已知它不在了」的时刻起算。本 zone 用领导者写的 `death_at`;别的 zone 的 `death_at` 在那个 zone 的控制域里,按 §4.3 不读,本副本能证明的最早时刻就是自己第一次看到它不在 etcd 里。要更早只能去问那个 zone 的 scene_manager,§5 否决了跨 zone 转发,理由不变。这条路径只在「玩家离线很久、回来时位置还落在别的 zone 的死节点上」才会多等,回可重试拒绝,不丢数据。
3. 只给场景号、不给 zone 的 EnterScene 只能解析本 zone 的场景(场景键带 zone 的 tag,只给场景号的入口按本进程的 zone 拼键,全部 Redis 上 Cluster §3.3)。2026-11-11 核对过全部调用方,没有人这样调:
   - `SceneManager.EnterScene` 的 gRPC 调用方只有 C++ scene 与 login,go 其余服务、`tools/`、`robot/`、Java 网关里都没有。
   - C++ 的请求都填 `zone_id`(例如 `player_scene_handler.cpp` 的 `req.set_zone_id(GetZoneId())`)。
   - login 的 `SceneId` 恒为 0(`resolveEnterSceneRoute`):要么给本 zone 或归属 zone,要么 `ZoneId` 也为 0,由 scene_manager 按 location 决定去向。
   - robot 走客户端协议,经 gate 到 scene,最后是上面 C++ 那条路径。
   以后新增调用方必须同时给 zone。
4. 单个 zone 的控制域落在一个槽上,上限是一台主节点。到顶时把这个 zone 的槽迁到独占的主节点,再大就拆 zone,不拆脚本里的键([全部 Redis 上 Cluster](redis-cluster-all-domains-20261110.md) §6.2、§6.3,2026-11-19 定案)。原稿「zone 换组只能停写迁移」的代价随按组分实例退役消失:zone 随 slot 原子迁移,不停写。
5. 跨 zone 重定向挑 gate 每次直读一次 etcd,这是有意的,不改。读量等于跨 zone 重定向的速率,每次只读目标 zone 的 gate 前缀(几十条),随规模线性增长,没有平方项。第 7 条的停写判定(`staleGateIDs`)靠的就是每次直读拿到的 ModRevision 序列,改成缓存或 watch 要另造一套判定,不会更简单。
6. 全局池服务(data_service、match、battle、client-rpc-router、player_inbox、team)仍是全服 watch。它们实例少、键很少变,读量随 N_cpp 线性增长,不是平方项。
7. scene_manager 的进场去重键 `enter_scene:dedup:` 在玩家域,本设计没有动它。
8. Redis 连接总账:控制集群单独一份每主节点的账(`Get-RedisControlClientRows -Cluster $true`,写路径打一行 `Redis control connection ledger:`)。每个 scene-manager 副本按 `ControlRedis.PoolSizePerNode` 记,每个 scene 进程每个主节点 1 条 `tlsControlRedis`,唤醒订阅每副本按 64 条记;超水位只告警。加主节点不增加单节点的连接数。11-10 补齐的按组总账(`Get-RedisCtlConnectionLedgerInputs` / `Write-RedisCtlConnectionLedger`)随控制域实例在第 6b-1 批删除(全部 Redis 上 Cluster §10.7)。
9. `redis-split-status` 只剩两套集群(`redis-player`、`redis-control`)一段:StatefulSet 就绪数、`cluster_state`、`cluster_slots_ok`、`cluster_known_nodes`,没部署打 `<absent>`,有问题退出码 1;`redis-wipe` 逐主节点清 `redis-control`。11-10 补的每 zone 控制域一段(核对 scene-manager 的 `Redis.Host` 与 node-config 的 `scene_control_redis.host`)随控制域实例删除。
10. 合服:源 zone 下线后没有领导者再处理 `scene_manager:dead_node_reconcile:{ctl:<S>}`。合服流程不清源区的 Redis 热状态(`tools/merge_zone` 只改映射,不连 Redis),这张表与 `scene_nodes:{ctl:<S>}:*` 一样留作没人读的垃圾键,不删:撤销合服、把源区节点拉回来时,源区的领导者照表里剩下的接着收尾,与进程重启后接手同一条路径,删了反而丢掉这些死节点的收尾。想让表里不留任务,收源区 namespace 前看 `scene_manager_dead_node_reconcile_pending{zone_id="<S>"}` 回到 0,写在 [合服手册](../ops/merge-zone-runbook.md) §8 末尾(2026-11-10)。
11. K8s 的 player-locator ConfigMap 已有 `SceneManagerRpc` 段,键是 `scenemanagerservice.rpc.z<zone>`,只连本 zone 的 scene_manager。这一段由断线重连那条线补上(`infra-reconnect-overview.md` §4 R20),键的规则同 §3.6。
12. §3.1 收窄带来的选号问题:node_id 的分配键 `<Svc>.rpc/allocated/...` 全服唯一,`NodeAllocator::SelectNodeId` 却只看发现快照里的同类节点。收窄之后 gate、scene 只看得到本 zone 的同类,新节点会逐个去撞别的 zone 已占的号,每撞一次等 1 秒,zone 越多启动越慢。已由缺口第 30 条批 3 修掉(已落码,未编译):分配键 CAS 失败时用 keys_only 列出全服已分配的号,下次选号排除(`etcd_helper.cpp`、`etcd_manager.cpp`、`node_allocator`),见 [连接扇出子集化](connection-fanout-subsetting-20261106.md) §2 末段与 §3.4。

## 7. 分批与文件

| 批 | 内容 | 主要文件 |
|---|---|---|
| 4a | C++ 发现收窄、兜底周期、取消退避 | `node_util.{h,cpp}`、`service_discovery_manager.{h,cpp}`、`etcd_service.{h,cpp}`、`cpp/tests/` 新用例 |
| 4b | scene_manager 按 zone 选主、只 watch 本 zone、编排只本 zone、别的 zone 判定走 etcd | `scene_manager_service.go`、`config.go`、`load_reporter.go`(watch 前缀与 zone 判定,checkNodeView 段除外)、新 `foreign_zone_registry.go`、`owner_replaced.go`、`scene_node_client.go`、`enterscenelogic.go`、`leavescenelogic.go`、`zone_registry.go`(删)与各循环 |
| 4c | go-zero 发现键按 zone | `k8s_deploy.ps1` 三处 Key、契约测试 |
| 5a | scene_manager 控制域句柄、全服键改名或删除、`ownsControlZone` 守卫、跨 zone 预检改 etcd | `servicecontext.go`、`config.go`、`reentry_barrier.go`(任务表键)、`scene_membership.go`、`gate_redirect.go`(排空标记改读 `CoreRedis`)、新 `control_zone_keys_contract_test.go` |
| 5b | C++ scene 控制域句柄 | `config.proto`、`config.cpp`、`game_config.yaml`、scene 启动、`player_scene.cpp`、`player_mystery_merchant.cpp` |
| 5c | K8s 控制域实例、ConfigMap、切换闸、连接账、清库与状态 | `k8s_deploy.ps1`、`redis-purpose(-ha).yaml` 头注释、`tests/k8s_redis_split_contract.tests.ps1`、`deploy/k8s/README.md` |

各批的落码状态与 Codex 命令记在 §8 与 `docs/PROGRESS.md` 2026-11-05「控制面按 zone 化与控制域 Redis 按 zone 组分实例」条目。

实际改动(2026-11-05,未编译):

- 4a:`node_util.{h,cpp}`(SceneManager 归入 zone-scoped、`DiscoveryPrefixFor`)、新 `discovery/discovery_resync_policy.h`、`service_discovery_manager.{h,cpp}`(`EffectiveDiscoveryPrefixes`、60s±20% 兜底)、`etcd_service.{h,cpp}`(取消后按前缀退避重拉、流重建用收窄后的前缀)、`core.vcxproj(.filters)`、新用例 `cpp/tests/cross_zone_test/discovery_zone_scope_test.cpp`。
- 4b:`scene_manager_service.go`(`logic.LeaderLockKey(c.ZoneId)`)、`config.go`(删 `LeaderLockKey`、`ZoneId` 语义)、`leader_gate.go`、新 `orchestrated_zones.go`、新 `foreign_zone_registry.go`、`load_reporter.go`、`node_view_gate.go`(`nodeViewZones`)、`enterscenelogic.go`(别的 zone 的持有者判定、跨 zone 地图预检、路由不变量 `wrong_zone`)、`owner_replaced.go`、`scene_node_client.go`、`agones_reconcile.go` / `mystery_merchant.go` / `world_channel_directory.go` / `world_rebalance_debug.go` / `world_autoscale.go` / `instance_lifecycle.go` / `reentry_barrier.go` 的循环 zone 范围、删 `zone_registry.go`、`load_refresh.go`、`orphan_cleanup.go`、`scene_member_reconcile.go`、`metrics.go`(删 `registry_failed`,加两个计数器)。
- 4c:`k8s_deploy.ps1` 的 scene-manager 服务端与 login 客户端 `Etcd.Key` 加 `.z<zone>`。
- 5a:`config.go` / `svc/servicecontext.go` 的 `CoreRedis`、`gate_redirect.go` 排空标记改读 `CoreRedis`、`reentry_barrier.go` 任务表按 zone、`scene_membership.go` 的别 zone 守卫、`etc/scene_manager_service.yaml`。
- 5b:`config.proto` 的 `scene_control_redis = 11`、`config.cpp`、`redis_manager.{h,cpp}`(`tlsSceneControlRedis`、`ResolveRedisAddress`)、`node.cpp` / `node_entry.h` / `scene/main.cpp` 的关停、`scene/core/system/redis.cpp` 建连、`player_scene.cpp`、`player_mystery_merchant.cpp`、`bin/etc/game_config.yaml`。
- 5c:`k8s_deploy.ps1`(`-RedisCtlMaxMemory`、`-ConfirmRedisCtlSwitch`、控制域实例段、scene-manager ConfigMap 的 `Redis` / `CoreRedis`、node-config 的 `scene_control_redis`、`redis-wipe` 覆盖)、`dev_tools.ps1`、`k8s_image.ps1`、`scene-manager-alerts.yaml`、`docs/ops/scene-node-role-split.md`、新契约测试 `tools/scripts/tests/k8s_redis_ctl_contract.tests.ps1`、`k8s_client_entry_contract.tests.ps1` 的透传键。
- 测试迁移:Go 单测的 svcCtx 设 `ZoneId = testZoneId` 并装别的 zone 的替身(`installTestZoneFakes`);模拟 zone 2 gate 的用例把进程设成 zone 2;依赖 zone 登记集合的用例改写为「只处理本 zone」。新用例:`foreign_zone_registry_test.go`、`control_zone_keys_contract_test.go`。

2026-11-10 订正(复查 5c 时发现两处缺陷,并补齐 §6 第 8、9 条;未运行):

- **玩家域回落到了控制域实例(数据一致性缺陷)**:K8s 没建玩家域集群时,`PlayerRedis` 段只写池上限、不写地址,scene_manager 的 `EffectivePlayerRedis` 回落到 `Redis` 段。5c 把 `Redis` 段改成 `redis-ctl-g<N>` 之后,scene_manager 的 location、owner_epoch、handoff 落在控制域实例,login、player_locator、data_service 等服务与 C++ scene 的玩家域仍在核心,两份各写各的,跨服务的 CAS 全部失效。改为回落到 `EffectiveCoreRedis()`(写了 `CoreRedis` 就是核心,没写就是 `Redis` 段,开发环境三者同一个实例,行为不变)。回归用例 `TestEffectivePlayerRedisFallsBackToCoreNotControlInstance`;`k8s_redis_ctl_contract` 钉住「不写 Addrs、`CoreRedis` 是核心、C++ 用 zoneredis」这层跨语言依赖。
- **首次部署会被入口闸拒绝**:`ConvertFrom-RedisCtlLiveYaml` 的参数标了 `[string]`,PowerShell 把传进来的 `$null`(ConfigMap 不存在)转成空串,判成「在而读不出地址」→ `refuse`。不带 `-ConfirmRedisCtlSwitch` 的首次 zone-up / all-up 全部会被拦。参数改为不标类型(同 `ConvertFrom-RedisPurposeLiveYaml`)。契约里原有的 absent 用例本来就会红,只是这套契约还没人跑过。
- 连接总账与 `redis-split-status` 见 §6 第 8、9 条:`k8s_deploy.ps1` 新增 `Get-RedisCtlConnectionLedgerInputs`、`Write-RedisCtlConnectionLedger`、`Read-LiveConfigMapData`(入口闸与状态命令共用)、`ConvertFrom-RedisCtlNodeConfigYaml`、`Get-RedisCtlZoneProblems`、`Get-RedisCtlZoneStatus`。
- 诊断文案按 AGENTS §2 改英文:控制域一段的 throw / 告警 / 问题文本,以及 `ConvertFrom-RedisQueueInstanceLive` 的 7 条问题文本。后者去掉了写死的「login / gateway 指向的实例」,因为控制域实例也用它;`k8s_redis_split_contract` 的 9 条匹配同步改了。
- `deploy/k8s/README.md`:总账与 `redis-split-status` 各加一段,`redis-wipe` 的实例清单补 `redis-ctl-g<N>`。

2026-11-12 订正(§6 第 1 条改为修掉;第 2、5 条写成有意的取舍;未运行):

- 新文件 `go/scene_manager/internal/logic/world_channel_on_demand.go`:`reserveWorldChannelCreatingOnDemand` 是进场挑大世界频道的新入口,`resolveSceneForEnter` 改调它(`enterscenelogic.go` 只改这一处调用和 `rejectTravelToUnopenedMap` 的注释)。
  - 只在「World 表里的图、频道集合 SCARD 为 0」时补铺,挑回 0 的其他原因(都满、属主都死且没有活节点)不补。
  - 补铺前先等负载集里有可用节点(只在本副本镜像里已有可用注册时等),等完再查一次集合,领导者已经铺好就不重复发 CreateScene。
  - 同进程按 (zone, conf) 合并,写法同 `channel_move_flight.go`。
- `foreign_zone_registry.go`:新增 `hasWorldHostingRegistration`,预检与补铺前的「镜像里有没有可用注册」共用它,与 `getNodesForPurpose` 同口径(不算排空中、身份歧义的节点)。
- 新测试 `world_channel_on_demand_test.go`,共 9 条:首登与第二条腿当场补铺、等领导者、三种不等的情形、频道在却不可用时不补、等待期间领导者铺好了就用它的、同进程合并与等待者按自己期限放弃、预检不算排空与歧义节点。
- 与 [全部 Redis 上 Cluster](redis-cluster-all-domains-20261110.md) 第 2a 批对过文件:新文件只调现有的键构造函数,跟着它的改名走。

2026-11-10 之后由全部 Redis 上 Cluster(缺口第 34 条)换掉的部分:5a 的 `CoreRedis` 句柄与 go-zero `Redis` 段在第 2b 批删除;5b 的 `scene_control_redis` / `tlsSceneControlRedis` 在第 3 批合成 `control_redis` / `tlsControlRedis`;5c 的 `redis-ctl-g<N>` 实例、`-RedisCtlMaxMemory` / `-ConfirmRedisCtlSwitch`、切换闸、按组总账与状态检查、`k8s_redis_ctl_contract.tests.ps1` 在第 6b-1 批删除。

## 8. 交给 Codex 的验证

全部在仓库根目录执行,按顺序。C++ MSBuild 必须串行 `/m:1`。

已有的运行证据(2026-11-10 核对原件):第 2 步 Go 由 Codex 11-06 跑过,scene_manager `go vet ./...` 通过,`go test ./... -count=1` 788 个顶层用例 PASS、0 FAIL(PROGRESS 11-06「修正 scene_manager 旧夹具」两条);第 3 步 C++ 在 11-08 命名统一那轮整解后跑过,`cross_zone_test` 458 条全 PASS,其中 `DiscoveryZoneScopeTest.*` 5 条(`build/verification/naming-audit-20261108/cpp-tests/cross_zone_test/cross_zone_test.stdout.log`)。第 4、5 步至今没有运行记录。11-10 的订正(§7 末尾)只碰 `go/scene_manager/internal/config` 与部署脚本,第 2 步要重跑 `internal/config` 包,第 4 步整套首次跑。

1. **协议生成**(第 5b 批改了 `config.proto`):
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
2. **Go**:
   - `cd go/scene_manager && go vet ./... && go test ./... -count=1`
   - 通过标准:全部通过;新用例 `TestForeignOwnerDead*`、`TestForeignZoneGone*`、`TestForeignRegisteredInstance*`、`TestOrchestratedZonesIsOwnZoneOnly`、`TestLeaderLockKeyPerZone`、`TestSceneNodeZonePrefixDoesNotMatchOtherZones`、`TestOwnsControlZoneSkipsForeignMembership`、`TestTravelMapPrecheckUsesEtcd*`、`TestControlZoneKeysContract*`、`TestSweepStaleLoadSetMembers_OnlyOwnZoneNoScan`、`TestCleanupOrphanWorldChannels_OwnZoneOnlyWithoutScan`、`TestDeadNodeReconcile_DropsMalformedTask`(含别的 zone 的成员)都在列表里且通过。
   - 2026-11-12 新增:`go test ./internal/logic/ -count=1 -v -run 'TestEnterScene_FirstLoginCreatesDefaultWorldChannelOnDemand|TestEnterScene_TravelSecondLegCreatesDefaultWorldChannelOnDemand|TestEnterScene_OnDemand|TestJoinWorldChannelOnDemand_|TestTravelMapPrecheckUsesEtcd_'` 全部 PASS(新文件 9 条 + 原有预检 3 条),再跑全量。修复前红的取法写在 `world_channel_on_demand_test.go` 文件头。
   - 失败时保留 `go test -v` 的失败用例输出;本批改了约 60 个用例的夹具(进程 zone、别的 zone 替身),先看失败是不是夹具没跟上。
3. **C++**:先编库,再编测试(库改了不重编,测试会链接旧的 lib):
   - `msbuild cpp/libs/engine/engine.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`
   - `msbuild cpp/libs/services/scene/scene.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`
   - `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build`
   - 测试工程:`msbuild cpp/tests/cross_zone_test/cross_zone_test.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`,运行 `--gtest_filter=DiscoveryZoneScopeTest.*`。
   - 通过标准:`DiscoveryZoneScopeTest.*` 全绿,既有 etcd / 发现 / 跨 zone 用例不回归。
4. **部署脚本契约**:`pwsh -File tools/scripts/tests/k8s_redis_split_contract.tests.ps1`、`pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`pwsh -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`,全部通过。
   - 11-05 新建的 `k8s_redis_ctl_contract.tests.ps1` 随控制域实例在全部 Redis 上 Cluster 第 6b-1 批删除,其中仍成立的一条(scene-manager 的发现键 `scenemanagerservice.rpc.z<zone>`)挪进 `k8s_deploy_contract`;`k8s_redis_split_contract` 改成只钉运维命令与退役。控制集群那一组契约与命令见 PROGRESS 2026-11-19「全部 Redis 上 Cluster 第 6b-1 批」。
   - 11-10 的两条回归核对(`CoreRedis` 回落、`ConvertFrom-RedisCtlLiveYaml` 的参数类型)随对应代码删除,不再适用;scene_manager 玩家域回落到控制域实例的用例现名 `TestEffectivePlayerRedisFallsBackToTheControlInstance`。
5. **冒烟**(kind,dev 档,两个 zone,全新集群):`pwsh -File tools/scripts/k8s_deploy.ps1 -Command all-up`(dev 档不建控制集群,控制域在核心单实例 `redis`;`-RedisSplit` 与 `-ConfirmRedisCtlSwitch` 已在第 6b-1 批删除)。然后:
   - `-Command redis-split-status` 退出码 0;
   - 两个 zone 各自 `scene_manager_is_leader` 之和为 1;
   - 每个 zone 的 scene 日志里只有本 zone 的 `Watching prefix: SceneNodeService.rpc/zone/<z>/`;
   - robot 跑一次跨 zone 传送,两条腿都成功。
   - 失败时保留:两个 zone 的 scene-manager 日志、scene 日志里 `etcd watch` 相关行、`redis-split-status` 输出。

## 9. Java 版

Java 版的注册发现、场景编排是自有实现,没有共享 Redis 的同构部署(G4a §26)。本次只在 Java 仓 `PARITY.md` 登记待做:「控制面按 zone 化(发现只看本 zone、编排按 zone 选主)」与「控制域按 zone 组分实例」。后一项的目标随 11-10 改口径变成上 Cluster,Java 仓 `PARITY.md` 另有「Redis Cluster」待做行(全部 Redis 上 Cluster §11)。客户端契约不变。
