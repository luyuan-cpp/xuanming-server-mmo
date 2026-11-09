# 控制面按 zone 化与控制域 Redis 按 zone 组分实例(2026-11-05)

缺口清单 [server-gap-audit-20261101.md](../handoff/server-gap-audit-20261101.md) 第 4、5 条的设计与落码记录。容量依据是 [容量模型](capacity-model-hok-scale.md) §2.2 与 A1;排队实例的先例是 [G4a 设计稿](shared-redis-split-by-purpose.md) §25.3 第 10、18 条。

状态:设计已定,第 4a、4b、4c、5a、5b、5c 批 2026-11-05 已落码,**都没有编译、没有测试**,验证命令见 §8。Java 版只在 PARITY 记待做(§9)。

## 1. 结论

**第 4 条:控制面按 zone 化。**

1. C++ 节点对 zone-scoped 类型只 Range、只 watch `<Svc>.rpc/zone/<本 zone>/`。SceneManager 归入 zone-scoped,scene 与 gate 只连本 zone 的 scene_manager。
2. 每 2 秒一次的全前缀 Range 改为 60 秒兜底、±20% 抖动。正常靠 watch,断流、压缩和 watch 被取消时逐前缀重拉。
3. gate 每 10 秒重写一次 etcd 记录的做法不变,它是 login 与 scene_manager 判断 gate 停写的心跳(缺口第 7 条)。收窄监听方之后,这些写只扇出到本 zone 的监听者。这一点偏离容量模型 A1「人数不再写 etcd」,理由见 §3.2。
4. scene_manager 按 zone 选主,锁是 `scene_manager:leader:lock:zone:<z>`。每个进程只服务配置里的 `ZoneId`,只 watch 这个 zone 的场景节点,`knownNodes` 只放这个 zone。
5. 判断别的 zone 的节点时,按需直读 etcd 的那个 zone 前缀,并用本副本「第一次看到它不在」的时刻做再入屏障(§3.5)。
6. K8s 上 go-zero 的发现键按 zone 加后缀 `.z<zone>`,login、player_locator 只连本 zone 的 scene_manager。本机多 zone 脚本 `go_services.ps1` 早就这样做。

**第 5 条:控制域 Redis 按 zone 组分实例,不上 Cluster。**

1. 每个 zone 组一个控制域实例 `redis-ctl-g<N>`。组号沿用 zones 配置的 `redisQueueGroup`,一个组是一组 zone 共用的 Redis 单元,排队实例和控制域实例各一个。
2. 键名一个都不改。原来全服唯一的三把键:选主锁、死节点收尾任务表改按 zone 起名;zone 登记集合 `scene_manager:zones` 删除。
3. **新不变量**:zone z 的控制域键只由 zone z 的 scene_manager 读写。别的 zone 的事实只从与 zone 无关的介质取:etcd、玩家域 Redis、Kafka、重定向票据(§4.3)。
4. 所有脚本和 MULTI 的键本来就只属于一个 zone(§4.4),不需要拆。
5. C++ scene 读的两个读模型(分线目录、神秘商人)跟着控制域走,scene 加一个连本组控制域实例的句柄 `scene_control_redis`。落点唤醒的 PUBLISH 留在核心,订阅者 guild、trade 只连核心。

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
| 场景键 | `scene_atomic.go:279-288`、`createscenelogic.go:20-74` | `scene:<id>:node` 等只按场景号,不带 zone、没有 hash tag |
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

- 选主锁 `scene_manager:leader:lock:zone:<z>`,`z` 是配置的 `ZoneId`,锁在 z 所属控制域实例上。`LeaderLockKey` 配置项删除(只有一处读取,没有任何配置写它):锁名由 zone 决定,配置写错就会让两个 zone 抢一把锁,或一个 zone 没人编排。
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

收窄之后,本副本对别的 zone 没有 watch 观察。原来用的负载集与 death_at 都在别的 zone 的控制域实例上,按 §4.3 的不变量也不能读。改为两样与 zone 无关的证据:

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

两个候选:按 zone 组分实例;或者上 Redis Cluster,并给控制域键加 `{zone}` hash tag。取前者,理由:

1. **§4.3 的不变量让 Cluster 的长处用不上**。Cluster 的主要好处是一个地址能到任何键;不变量之后,一个进程只碰本 zone 的键,一个实例就够。
2. **改动小,读起来直**。所有脚本本来就只碰一个 zone(§4.4),键名一个不改。上 Cluster 要给约 130 处调用和全部脚本的键改名,SCAN 要逐主节点执行,go-zero 客户端还要换成集群模式。
3. **与排队实例同一机制**(G4a §25.3 第 10 条):模板 `redis-purpose.yaml` / `redis-purpose-ha.yaml`、形态规则、连接账、清库、状态检查都复用。
4. **故障隔离一样**:一个组的实例挂了只影响这个组的 zone,高可用形态按 `-RedisReplicas` 走哨兵。

代价:

- 一个 zone 要换组时只能停写迁移。上线前不存在这个问题;上线后要等那个 zone 停服维护,把它的控制域键搬过去。场景路由和成员可以从零重建:停服后 scene 节点全部重启,领导者重新铺频道。Cluster 可以在线 reshard,这是放弃的唯一能力。
- 一个组的写入上限是一个主节点。组太热时把组拆开,多分几组。

### 4.2 键的归属

| 键族 | 放在哪 | 谁读写 |
|---|---|---|
| 场景路由 `scene:<id>:node|zone|source|mirror|mirrors|draining|agones_gs|claim`、成员 `instance:<id>:members`、`node:zone:<z>:<n>:*`、`instances:zone:<z>:active`、`scene_nodes:zone:<z>:load`、世界频道 `world_channels:*:zone:<z>:*`、分线目录与线号、神秘商人、`world_init:lock:zone:<z>` | zone 所属组的 `redis-ctl-g<N>` | 本 zone 的 scene_manager;C++ scene 只读分线目录与神秘商人 |
| 选主锁 `scene_manager:leader:lock:zone:<z>`(原 `scene_manager:leader:lock`) | 同上 | 本 zone 的 scene_manager |
| 死节点收尾任务 `scene_manager:dead_node_reconcile:zone:<z>`(原全服一张) | 同上 | 本 zone 的领导者 |
| `scene_manager:zones` | **删除** | — |
| gate 排空标记 `gate:<id>:draining|drained` | 核心(G4a §25.3 的 10-23 订正) | scene_manager 只读,经新句柄 `CoreRedis` |
| 落点唤醒频道 | 核心 | C++ scene PUBLISH,guild、trade 订阅 |
| 玩家域 `player:{<id>}:*`、进场去重 | 玩家域(不动) | 经 `PlayerRedis` |

场景号不带 zone。同一组里几个 zone 的场景键放在一起,这没关系:场景号全局唯一,不会撞。按 §4.3,只读写本 zone 的场景。

### 4.3 不变量:zone z 的控制域键只由 zone z 的 scene_manager 读写

几个 zone 共用一个实例、又各自选主之后,有一类缺陷:领导者「从 Redis 里发现有哪些 zone」,结果把同组别的 zone 也扫了。最危险的是 `sweepStaleLoadSetMembers`:本副本的镜像里没有那个 zone 的节点,它会把那个 zone 的活节点全当成陈旧成员,写 death_at 并摘出负载集。为此改了这些地方:

1. zone 登记集合删除。`sweepStaleLoadSetMembers`、`CleanupOrphanWorldChannels`、成员对账只处理 `ZoneId`。
2. 数据面上凡是可能带别的 zone 的入口,都显式判一次 `ownsControlZone(zone)`:
   - 跨 zone 传送的预检 `rejectTravelToUnopenedMap`:目标 zone 恒不是本 zone,原来 SCARD 目标 zone 的频道集合。改为与 zone 无关的判定:地图在 World 表里,且 etcd 里那个 zone 至少有一个能承载大世界的场景节点。它原本就是 fail-open 的提前拒绝,不是安全门(etcd 读失败照旧放行)。与原判定的差别只在「目标 zone 刚起、节点已注册但领导者还没铺频道」的几秒:原来拒,现在放行,第二条腿解析不到场景时按原有失败处理。
   - 成员集合的增减(`addSceneMember` / `removeSceneMember` / `removeScenePlayer`):别的 zone 一律不动,只记 `scene_manager_foreign_zone_skip_total{site}`。成员集合是派生索引,权威是玩家域的 location;别的 zone 留下的陈旧成员由那个 zone 的成员对账在 2 分钟宽限后摘掉(`scene_member_reconcile.go`,redis-player-store-cluster.md §5.10)。没摘过的成员,回滚时也不需要加回。
   - `removeScenePlayer` 只给场景号、不给 zone 时(LeaveScene 的两处清理),先读本组的 `scene:<id>:zone`:读不到,或者不是本 zone,就不动。
   - 别的 zone 的节点判活见 §3.5,不读那个 zone 的负载集和 death_at。
   - 建镜像时源场景记录在别的 zone(`scene:{源}:zone` 不是本 zone,或读失败):不按源去重,也不把新镜像登记成源的子镜像(不写 `scene:{源}:mirrors` 与 `scene:{镜像}:source`)。新镜像照样带镜像标记、按镜像的短超时空闲回收,只是源销毁时不级联到它。源没有 zone 记录(不在本组实例里)时照旧登记:那份子集合在本组实例里,只有本 zone 读写,随镜像销毁摘掉。(2026-11-06,「场景销毁认领」会话报的漏口。)
   - CreateScene 与 EnterScene 一样,请求的 zone 不是本进程的 zone 就在任何写入之前拒绝。
   - 销毁路径(2026-11-06,「场景销毁认领」会话):认领脚本在同一步比对 `scene:{id}:zone`,不是认领方的 zone(或记录损坏)就回 `foreign`、一个键都不动;镜像级联与源迁移级联跳过登记在别的 zone 的子镜像,留给那个 zone 的空闲回收;镜像拆除时只从本 zone 或本实例里没有 zone 记录的源的子集合摘自己;显式 DestroyScene 的 `zone_id` 不是本进程的 zone 就在任何读写之前拒绝,没填按本进程的 zone。详见 scene-creation-architecture.md「销毁只改本 zone 的键」。
3. 同组的键在同一个实例上,读得到也不读:组的划分是部署细节,行为不能随组的划分而变。

为什么不会丢数据、不破坏一致性:

- **编排单写者**:每个 zone 只有一把锁、一个领导者,与原来全服一把锁的单写者语义相同,只是范围变成一个 zone。
- **原子范围不变**:每段脚本的键都在同一个 zone 的同一个实例上(§4.4)。
- **权威数据不在控制域**:位置、owner_epoch、交接标记都在玩家域。控制域里的成员集合与负载集是派生索引,由对账收敛。
- **跨 zone 的动作原来就不是原子的**:跨 zone 传送的两条腿由两个 EnterScene 完成,中间靠玩家域的「等待落点」与重定向票据衔接(cross-zone-scene-travel.md CZ-4、CZ-5),本设计不改这一点。

### 4.4 脚本逐个核对(2026-11-05)

| 脚本 | 位置 | 键 | 结论 |
|---|---|---|---|
| 三段认领 / 空闲回收 / force 销毁 | `scene_atomic.go` `sceneClaimKeys` 及 `:367`、`:385` | 场景键 + `instances:zone:<z>:active` + `node:zone:<z>:<n>:*` | 同一 zone |
| 加成员 / 选最空频道 | `scene_atomic.go:398-454` | 场景键 + `node:zone:<z>:<n>:members` | 同一 zone(候选都是本 zone 频道) |
| 成员增减 | `scene_membership.go` `sceneMemberKeys` | 场景成员 + 节点成员 | 同一 zone |
| 场景登记 | `scene_registration.go:182-191` | 场景键 + 节点计数 / 反向索引 + 频道集合 | 同一 zone |
| 改派 | `world_init.go` `casReassignSceneNode` | 场景键 + 频道集合 | 同一 zone |
| 排空 | `world_autoscale.go:425-431` | 频道集合 / 排空集合 / 期望数 + 场景键 | 同一 zone |
| 线号 | `world_channel_directory.go:173` | 一个键 | — |
| 神秘商人 | `mystery_merchant.go:421` | 一个键 | — |
| Agones 名额转移 | `agones_binding.go:377` | 场景键 | 同一 zone |
| 死节点收尾入队 / 出队 | `reentry_barrier.go:337`、`:507-512` | 任务表 + `node:zone:<z>:<n>:*` + 负载集 | 原任务表是全服键;改成按 zone 之后同一 zone |
| 选主锁 | `shared/leader` | 一个键 | — |

没有跨 zone 的原子操作,不需要拆。以后新增脚本,键必须属于同一个 zone;契约测试 `control_zone_keys_contract_test.go` 钉住两个改名键的形状,以及「没有全服键」。

### 4.5 C++ scene 的控制域句柄

- `game_config.yaml` 新段 `scene_control_redis`(host、port、password、db),对应 `GameConfig.scene_control_redis = 11`,类型复用 `ZoneRedisConfig`。整段缺失时沿用 `zoneredis`,开发环境不用改。口令可被环境变量 `SCENE_CONTROL_REDIS_PASSWORD` 覆盖,值不进日志。
- scene 进程多一个 `RedisManager`(`tlsSceneControlRedis`),认证与重连契约与 `zoneredis` 相同。只有 `player_scene.cpp` 的分线目录与 `player_mystery_merchant.cpp` 的神秘商人改读它。
- gate、battle 不连它。落点唤醒的 PUBLISH 仍走 `zoneredis`(核心)。

### 4.6 K8s

- **实例**:infra-up 对 zones 配置里出现过的每个组 apply 一份 `redis-ctl-g<N>`,模板与排队实例相同。实例名、maxmemory 由参数决定,形态与核心同一套 `-RedisReplicas` 规则。**不受 `-RedisSplit` 控制**:控制域实例从首次部署起就是这个形态,不做迁移、不做灰度。
- **容量**:`-RedisCtlMaxMemory`,dev 档缺省 128mb,release 档缺省 1gb(G4a §25.3 第 19 条给排队实例定的每组初值,待压测复核)。与排队实例「release 档必须显式给」不同:控制域实例不受开关控制、每次 infra 部署都 apply,缺省就拒会让每次 release 部署都多带一个参数;压测定下容量之后改缺省。noeviction,写满拒写。
- **scene-manager ConfigMap**:`Redis.Host` 改为 `redis-ctl-g<N>.<infra>:6379`,新增 `CoreRedis` 段指向核心;`Etcd.Key` 加 zone 后缀。
- **C++ node-config**:scene 的 `scene_control_redis.host` 写本 zone 组的实例。
- **切换闸**:非 DryRun 的 zone 写路径读线上 scene-manager ConfigMap 的 `Redis.Host`。线上存在且与本次渲染不同,就拒绝,除非带 `-ConfirmRedisCtlSwitch`。切过去之后新实例是空的,本 zone 的场景路由要从零重建:先停本 zone 的 scene 节点再切。上线前的环境数据可以清空,带确认开关即可。
- **连接账**:scene-manager 的控制域连接从核心那一行移到 `redis-ctl-g<N>` 一行,并加上每个 scene 进程一条 `scene_control_redis` 连接。
- **清库与状态**:`k8s-redis-wipe` 逐个 FLUSHALL 控制域实例并核对 DBSIZE;`redis-split-status` 列出每个 zone 指向的控制域实例。
- **告警**:运维文本里「`sum(scene_manager_is_leader)` 应恒为 1」改为「每个 zone 恒为 1」;负载刷新的 `registry_failed` 随 zone 登记集合删除。
- **包装脚本**:`dev_tools.ps1`、`k8s_image.ps1` 透传 `-RedisCtlMaxMemory` 与 `-ConfirmRedisCtlSwitch`(追加在参数表末尾,不改位置参数绑定)。

## 5. 被否决或推迟的做法

- **控制域上 Cluster 并打 `{zone}` tag**:见 §4.1。如果以后一个组装不下一个 zone 的控制域写入,再按 zone 改 tag 上 Cluster。那时 §4.3 的不变量照样成立,改的只是寻址。
- **scene_manager 之间转发跨 zone 请求**:要新增 RPC 和跨 zone 发现。§3.5 与 §4.3 用 etcd 和玩家域已经够了。
- **给 scene_manager 一张全部 zone 到组的地址表,让它读别的 zone 的控制域**:每加一个 zone 就要重新渲染所有 zone 的 ConfigMap;合服下线的 zone,控制域实例可能已经没了,位置落在那里的玩家就永远进不来。§3.5 的 etcd 判定在 zone 下线后照样能用。
- **gate 人数不写 etcd**:见 §3.2。

## 6. 残余与风险

1. 跨 zone 传送预检在目标 zone 刚起的几秒内会放过(§4.3 第 2 条)。
2. 位置落在别的 zone、持有节点已死时,玩家要多等一个再入屏障(§3.5)。
3. 只给场景号、不给 zone 的 EnterScene 只能解析本组的场景。生产调用方都同时给了 zone(C++ 五处都填 `zone_id`,login 恒给 0 号场景),robot 与 GM 工具的写法未逐个核对。
4. zone 换组只能停写迁移(§4.1)。
5. 跨 zone 重定向挑 gate 仍是每次直读 etcd(第 7 条会话加了停写过滤),不在本设计范围。
6. 全局池服务(data_service、match、battle、client-rpc-router、player_inbox、team)仍是全服 watch。它们实例少、键很少变,读量随 N_cpp 线性增长,不是平方项。
7. scene_manager 的进场去重键 `enter_scene:dedup:` 不在控制域,本设计没有动它。
8. Redis 连接总账没有为 `redis-ctl-g<N>` 单列一行:scene-manager 的控制域连接仍记在核心那一行(多算核心、少算控制域),每个 scene 进程多出的一条 `scene_control_redis` 连接也没计入。控制域实例 `MAXCLIENTS` 10000,同组 zone 的 scene-manager 池加 scene 进程数远低于它,但要在压测前补上。
9. `redis-split-status` 不列控制域实例;`redis-wipe` 已覆盖 `redis-ctl-g<N>`。
10. 合服:源 zone 下线后没有领导者再处理 `scene_manager:dead_node_reconcile:zone:<S>`(不以 `node:zone:` 开头,不在合服按前缀清的范围里),要由合服流程显式删掉;`tools/merge_zone` 没有改。
11. 顺带发现:K8s 的 player-locator ConfigMap 没有 `SceneManagerRpc` 段(本机 `player_locator.yaml` 有),断线租约到期时的 LeaveScene 在 K8s 上可能发不出去。不在本设计范围,未核实影响,另行处理。

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

## 8. 交给 Codex 的验证

全部在仓库根目录执行,按顺序。C++ MSBuild 必须串行 `/m:1`。

1. **协议生成**(第 5b 批改了 `config.proto`):
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
2. **Go**:
   - `cd go/scene_manager && go vet ./... && go test ./... -count=1`
   - 通过标准:全部通过;新用例 `TestForeignOwnerDead*`、`TestForeignZoneGone*`、`TestForeignRegisteredInstance*`、`TestOrchestratedZonesIsOwnZoneOnly`、`TestLeaderLockKeyPerZone`、`TestSceneNodeZonePrefixDoesNotMatchOtherZones`、`TestOwnsControlZoneSkipsForeignMembership`、`TestTravelMapPrecheckUsesEtcd*`、`TestControlZoneKeysContract*`、`TestSweepStaleLoadSetMembers_OnlyOwnZoneNoScan`、`TestCleanupOrphanWorldChannels_OwnZoneOnlyWithoutScan`、`TestDeadNodeReconcile_DropsMalformedTask`(含别的 zone 的成员)都在列表里且通过。
   - 失败时保留 `go test -v` 的失败用例输出;本批改了约 60 个用例的夹具(进程 zone、别的 zone 替身),先看失败是不是夹具没跟上。
3. **C++**:先编库,再编测试(库改了不重编,测试会链接旧的 lib):
   - `msbuild cpp/libs/engine/engine.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`
   - `msbuild cpp/libs/services/scene/scene.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`
   - `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build`
   - 测试工程:`msbuild cpp/tests/cross_zone_test/cross_zone_test.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`,运行 `--gtest_filter=DiscoveryZoneScopeTest.*`。
   - 通过标准:`DiscoveryZoneScopeTest.*` 全绿,既有 etcd / 发现 / 跨 zone 用例不回归。
4. **部署脚本契约**:`pwsh -File tools/scripts/tests/k8s_redis_ctl_contract.tests.ps1`、`pwsh -File tools/scripts/tests/k8s_redis_split_contract.tests.ps1`、`pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`pwsh -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`,全部通过。
5. **冒烟**(kind,dev 档,两个 zone 同组):`pwsh -File tools/scripts/k8s_deploy.ps1 -Command all-up -RedisSplit queue -ConfirmRedisCtlSwitch`,然后:
   - `kubectl get sts -n <infra> | grep redis-ctl-g0` 就绪;
   - 两个 zone 各自 `scene_manager_is_leader` 之和为 1;
   - 每个 zone 的 scene 日志里只有本 zone 的 `Watching prefix: SceneNodeService.rpc/zone/<z>/`;
   - robot 跑一次跨 zone 传送,两条腿都成功。
   - 失败时保留:两个 zone 的 scene-manager 日志、scene 日志里 `etcd watch` 相关行、`redis-split-status` 输出。

## 9. Java 版

Java 版的注册发现、场景编排是自有实现,没有共享 Redis 的同构部署(G4a §26)。本次只在 Java 仓 `PARITY.md` 登记待做:「控制面按 zone 化(发现只看本 zone、编排按 zone 选主)」与「控制域按 zone 组分实例」。客户端契约不变。
