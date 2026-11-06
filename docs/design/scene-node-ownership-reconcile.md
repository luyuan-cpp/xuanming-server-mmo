# scene 节点回来之后的场景归属对账(2026-11-06,缺口清单第 33 条)

状态:**已落码,未编译、未测试,待 Codex 验证(§10)。** Java 版不适用(§11)。

关联:[缺口清单](../handoff/server-gap-audit-20261101.md) 第 32、33 条;[再入屏障](scene-owner-reentry-barrier.md) §3.2;
[场景创建架构](scene-creation-architecture.md)「死节点收尾」R1-I5;[控制面按 zone 化](control-plane-zone-scoping-20261105.md) §4.3、§4.5;
[世界频道放置代次](world-channel-placement-generation.md)(同日另一会话,scene_manager 侧);[ECS 组件访问规则](ecs-component-access-rules.md)。

## 1. 结论

scene 节点在自己的 etcd 身份确认有效时,每 10 秒用 MGET 读一次本地全部场景的 `scene:{id}:node`(经本 zone 的场景控制域句柄
`tlsSceneControlRedis`,只读)。某个实体连续两次读到「不是本节点」(映射指向别的节点或不存在),而且距它第一次被核对、
或最近一次 CreateScene 按号命中它已满 60 秒,就走与 scene_manager 发来的 DestroyScene 完全相同的路径:
`SceneLifecycle::DestroyScene` → `PlayerLifecycleSystem::BeginSceneDrain` 把住户存盘后改派,没有绑定了才销毁实体。

这是 Kubernetes kubelet 的做法:kubelet 回到集群后,把 API server 上已删除的 Pod 在本地停掉。期望状态只有一份(scene_manager
写的映射),节点按它收敛,不自己判断谁该死。只在节点侧做,不加协议、不改 scene_manager。

## 2. 问题

时间线(租约 TTL 180s,再入屏障 20s = C++ 冲突疏散预算 15s + 时钟余量 5s):

| 时刻 | 节点 A(与 etcd 单边失联,仍连着 gate 与 Redis,玩家还在玩) | scene_manager |
|---|---|---|
| T0 | keepalive 发不出去 | — |
| T0+90s | 身份转为不确认(半个租约没收到 ACK),每 5s 给玩家补存一次(既有) | — |
| T0+180s | — | 租约到期,watch DELETE:写 death_at、摘负载集、入死节点收尾任务 |
| T0+200s | — | 屏障放行:拆掉 A 名下实例的映射(A 不在负载集,不发 DestroyScene);世界频道「先建后改」改派到别的节点 |
| T0+X | 连上 etcd:重新拿租约,按原 node_id、同一个 node_uuid 重新注册 | PUT 清 death_at;第 32 条的撤销已经晚了(任务早执行完) |

此后 A 的进程里留着两类没有映射的实体:

- **实例(副本、镜像)**:新玩家进不去,scene_manager 的空闲回收看不到它(已不在 active 集合);
- **世界频道**:留在旧实体里的玩家,与进了新频道(别的节点上同一个 scene_id)的玩家互相看不见。

玩家数据不受影响:被接管过的玩家,A 的存盘由 owner epoch / 写权行拒绝;没被接管的照常存盘。这是场景归属的一致性问题。

## 3. 不选「失租就停服」

etcd 整体故障时所有节点同时失租,节点分不清「etcd 挂了」和「只有我连不上」,失租自停等于 etcd 一挂全服掉线。
`go/scene_manager/internal/constants/reentry_barrier.go` 文件头(2026-11-05 更正)定的也是:老节点继续服务,靠写权行挡回档。
所以失联期间节点照常服务,回来之后再对账。

## 4. 做法

### 4.1 期望状态的事实源

- 唯一事实源:`scene:{id}:node`,值是属主 node_id 的十进制串(Go 侧 `strconv.FormatUint`,`createscenelogic.go` 的 `SceneNodeKeyFmt`)。
  C++ 侧 `scene_ownership::SceneNodeKey` 与它是同一个键,跨语言契约,改名必须同批改(用例 `SceneNodeKeyMatchesTheSceneManagerKey` 钉住)。
- 只读。节点不写控制域任何键:§4.3 的不变量「zone z 的控制域键只由 zone z 的 scene_manager 读写」不变,
  `tlsSceneControlRedis` 原来也只读分线目录与神秘商人。
- 只看 `scene:{id}:node`,不读 `scene:{id}:zone`:节点只托管本 zone scene_manager 建的场景(发现按 zone 收窄,scene_manager 只认得本 zone
  的节点),场景号全局唯一,别的 zone 不会改写本 zone 场景的映射(认领脚本比 zone、CreateScene 拒别的 zone)。同组别的 zone 有同号节点,
  但它们的场景不会出现在本节点上。真出现「映射等于本节点号、却是别的 zone 写的」这种违例,结果是留着不拆,方向安全。
- 不读反向索引 `node:zone:{z}:{n}:scenes`:那是派生索引,路由与收尾的判据都是映射本身。

### 4.2 判定(`world/scene_ownership_rules.h`,纯函数)

每个本地实体一份核对状态 `SceneOwnershipCheckComp`(按需状态,只在内存):宽限起点、连续「不归本节点」次数、核对状态号。

| 读数 | 归类 | 对状态的作用 |
|---|---|---|
| 等于本节点号 | `self` | 连击清零,留着 |
| 别的节点号 | `other_node` | 连击 +1(封顶 2) |
| 键不存在、空串 | `no_mapping` | 连击 +1(封顶 2) |
| 整条命令失败、断线空应答、元素类型不对、本节点号未知 | `unknown` | 不改连击,不动手 |

释放条件:连击达到 `kNotMineConfirmations = 2`,并且这次读的**发出时刻**距宽限起点已满 `kCreateGrace = 60s`。
判定释放之后,只要后续读数仍不归本节点,每一轮都再走一次销毁(幂等,住户走完之前实体留着)。排空途中读到映射又回到本节点
(scene_manager 把它迁了回来),就停止排空、不再算「排空中」;已经改派走的住户由 scene_manager 重新落点。

- **为什么要两次**:一次读可能落在 scene_manager 两步写入的中间。两次相隔一个周期都说不是,才当成稳定的期望状态。代价只是晚 10 秒。
- **读不到不算证据,也不推翻证据**:控制域抖动时永远不会因此拆场景;「不归、读不到、不归」仍算两次。
- **为什么按发出时刻判宽限**:读到的是发出之后某一刻的映射,发出时刻早于真正被读的时刻,判得更保守。

### 4.3 宽限期从哪里算起

宽限起点 = 本节点第一次核对到这个实体,或最近一次 CreateScene 按 scene_id 命中它。后者由 `SceneLifecycle::ReuseExistingScene` 负责:
两个 CreateScene 入口(`scene_node_service.cpp` 的 `HandleCreateScene`、`scene_handler.cpp` 的 `SceneHandler::CreateScene`)的去重分支
都改调它,它把核对状态整个摘掉,下一轮重新登记。摘掉时核对状态号随之作废,命中之前发出的读,应答再晚到也对不上号,作废。

这是为了 scene_manager 的「先建后改」。改派世界频道的四条路径(rebalance、懒改派、init 的死节点改派、resolveScene 的死节点自愈)
都是先在新节点 CreateScene、返回后立刻 CAS 映射;CAS 落空时按那次尝试的放置代次回收([放置代次](world-channel-placement-generation.md))。
这段窗口里新节点上的实体已建好,映射还指着旧节点。窗口上界 = CreateScene 时限(init 5s,其余是请求 ctx)+ CAS + 落空回收 3s,
远小于 60s(2026-11-06 与「世界频道改派与排空收尾的属主一致性」会话核对)。迁回本节点的预建会按号命中留在本节点的旧实体,
`ReuseExistingScene` 让宽限期从这一刻重算,预建与 CAS 之间它不会被当成孤儿拆掉。

被放置代次判为陈旧的 CreateScene 在去重分支之前就被拒,不复用实体,也不重算宽限期。

宽限期只拖慢「刚建好就成了孤儿」的实体的回收。第 33 条的实体在节点失联前就建好了,早已过了宽限期。

### 4.4 动作

调 `SceneLifecycle::DestroyScene(scene)`,与 scene_manager 发来的 DestroyScene 走同一个函数:

1. `BeginSceneDrain`:给场景里每个有 gate 会话的玩家登记改派票据,走既有退出流程(存盘落地 → 写交接标记 → 发 EnterScene,
   `scene_id` / `scene_conf_id` 都留 0,由 scene_manager 按它自己的规则落点)。已断线的玩家(含断线保留中的)只存盘、不改派,
   与频道缩容排空同一口径。
2. 按玩家绑定重新数一遍;还有绑定就留着实体,下一轮仍不归本节点再调;没有了才销毁(同时记 Agones 单元账)。

改派之后玩家的去向由 scene_manager 定,节点不复制规则:世界频道的住户回到同号频道(已在新属主上),实例与镜像的住户回大世界。

对账器在进程内直接调 `SceneLifecycle::DestroyScene`,不经 DestroyScene RPC,也就不经放置代次的栅栏。这是有意的:栅栏挡的是迟到的陈旧
请求,对账器的输入是刚读到的期望状态本身。

### 4.5 门禁

| 门禁 | 不满足时 |
|---|---|
| 本节点 etcd 身份确认有效(`EtcdService::IsIdentityConfirmedFresh`:租约已授予、不在重注册中、注册流未停、最近一次 keepalive ACK 在 TTL/2 内),与断线释放标记 A1′ 的探针是同一个函数 | 本轮不核对(`identity_unconfirmed`)。失联期间分不清 etcd 整体故障与单边失联,排空要走的改派链路也被身份闸挡着。整节点疏散(身份冲突)开始前对账器先 Stop |
| 控制域 Redis 已连上 | 本轮不核对(`redis_unavailable`) |
| 上一轮的应答都回来了 | 本轮不发(`previous_in_flight`)。Redis 卡住时 RedisManager 的存活探测最迟约 20 秒摘掉连接,在途命令回空应答,计数归零 |
| 应答回来时身份仍确认有效、本节点号与发读时相同 | 这批读数作废,计 `unknown` |
| 应答回来时实体仍在、核对状态号与 scene_id 都没变 | 这个实体的读数作废 |

启停:`SetAfterStart` 里 Start(此时 node_id 已是终值);普通停机(`exitAllPlayers`)、身份冲突关停(`SetOnConflictShutdown`,整节点疏散接管全部玩家)、
拆除前(`SetBeforeTeardown`)各 Stop 一次,Node 作用域结束后再 Stop 一次兜底。`exitAllPlayers` 在 Node 析构的兜底 drain 循环之前执行,
所以探针捕获的 node 在它析构之前就解开了。定时器与 Redis 回调只捕获内部 Session 的 `weak_ptr`(AGENTS.md §11.7),Stop 之后到达的应答直接丢弃。

### 4.6 回来之后多久收敛

节点重新注册、身份转为确认有效之后,第一轮读到不归本节点(连击 1),下一轮(+10s)连击 2、宽限早已过,开始排空。
即重新注册后约 10~20 秒开始改派住户,住户全部离开后实体销毁。失联期间(身份不确认)不核对,分裂一直持续到 etcd 恢复。

## 5. 各类场景的口径

| 场景 | 映射怎么写 | 对账结果 |
|---|---|---|
| 世界频道(MainWorld / MainWorldCross 节点) | 登记脚本先写映射、同一轮 init 再补 CreateScene;改派先建后改 | 被改派走:`other_node`,排空,住户回同号频道的新属主 |
| 副本实例(Instance / InstanceCross 节点) | 登记脚本先写映射,再 CreateScene;失败回滚删映射、发 DestroyScene | 死节点收尾拆掉:`no_mapping`,排空,住户回大世界 |
| 镜像 | 按实例登记,属主 = 与镜像源共置的节点(`resolveMirrorSourceNode`),写的是承载它的节点 | 同实例。源频道被迁走时 scene_manager 级联拆镜像;DestroyScene 丢了也由这里收掉 |
| 正在缩容排空的频道(`scene:{id}:draining`) | 映射仍是本节点 | `self`,不动,由 scene_manager 的排空负责 |
| 正在迁往本节点的预建 | 映射还指旧属主 | 宽限期内不动;命中已有实体时宽限期重算 |
| 迁走之后旧属主上的实体 | 映射已是新属主 | `other_node`,与 scene_manager 随后发来的 DestroyScene 同一个动作,谁先到都一样 |

跨 zone 类型(MainWorldCross = 2、InstanceCross = 3)只是本 zone 节点的用途,不是跨 zone 托管:它们在本 zone 注册,只被本 zone 的
scene_manager 认得和派活,映射在本 zone 的控制域里。跨 zone 传送是客户端重连到目标 zone,目标 zone 的场景由目标 zone 的节点承载
(cross-zone-scene-travel.md)。部署脚本目前只渲染 0、1 两种。口径与普通节点相同,不用单独处理。

## 6. 为什么不会误拆

本地实体只会因为下面这件事被拆:两次相隔一个周期的读(都在身份确认有效、本节点号不变时发出)都说映射不是本节点,
而且后一次发出时距最近一次 CreateScene 命中(或第一次核对)已满 60 秒。逐条对照 scene_manager 的写法:

1. **节点上的场景只来自本 zone scene_manager 的 CreateScene**(gRPC `SceneNodeGrpc.CreateScene`;muduo 版 `Scene.CreateScene` 全仓只有用例在调)。
2. **新场景先写映射再 CreateScene**(`scene_registration.go`):实体建好时映射已经指向本节点。之后读到不是本节点,只能是映射被拆或被改走了。
3. **改派都是先建后改,CAS 紧跟 CreateScene**,上界远小于宽限期;CAS 落空的预建由 scene_manager 按放置代次回收。
4. **生产代码里没有「CAS 到本节点却不先 CreateScene」的写法**(不预建的 `reassignSceneNode` 只剩用例在用,2026-11-06 核对)。
   所以任何把场景重新放回本节点的动作,都会先有一次 CreateScene 命中本地实体,把宽限期与在途读数清掉。
5. 读是只读的 MGET,不参与 scene_manager 的任何原子范围,不会让它的脚本看到中间状态。

剩下的误差来源是控制域本身丢数据(§9 第 2 条)。

## 7. 顺带收掉的孤儿

对账的输入只有「映射是不是本节点」,所以凡是「有实体没映射」都会在宽限期后收掉,不限于第 33 条:

- 回滚补发的 DestroyScene 早于节点处理完在途的 CreateScene,实体留下(11-05「场景拆除任务」条目的残余,原写「要 C++ 侧墓碑」);
- rebalance 迁移之后发往旧属主的 DestroyScene 失败(它是尽力而为、失败只记日志),旧实体与住户留在旧节点上;
- 频道缩容的最后一次 DestroyScene 丢失,空实体留在节点上;
- 放置代次落地之后,迟到的旧建请求若仍建出孤儿(放置代次设计里的兜底)。

## 8. 开销与可观测性

**开销**:每轮每个节点发 ceil(本地场景数 / 256) 条 MGET,一个控制域实例每秒读的键数 = 同组全部场景数 / 10。
例:一组 2 万个场景约 2000 键/s,单条 MGET 最多 256 个键。上一轮没回来不发下一轮,不叠加。

**日志**(前缀 `[SceneOwnership]`):开始排空时一条 WARN(scene_id、读数归类、读到的属主、本节点号、连击、已核对秒数);
有绑定留着时一条 WARN;销毁时一条 INFO;启停各一条 INFO。之后每轮重试只有 `BeginSceneDrain` 自己的那行 WARN。

**指标**(`RegisterSceneOwnershipMetrics`,设了 `MMORPG_METRICS_PORT` 才有出口,标签都是低基数):

| 指标 | 类型 | 说明 |
|---|---|---|
| `scene_ownership_sweeps_total{outcome}` | counter | sent / no_scenes / identity_unconfirmed / redis_unavailable / previous_in_flight / send_failed |
| `scene_ownership_observations_total{result}` | counter | self / other_node / no_mapping / unknown |
| `scene_ownership_releases_total{stage}` | counter | drain_started / destroyed。有增长就说明本节点托管过控制面已改派或已拆掉的场景 |
| `scene_ownership_releasing_scenes` | gauge | 已判定不归本节点、还在等住户离开的本地实体数 |

## 9. 残余与风险

1. **一直连不上 etcd 的节点不对账**:身份不确认期间不核对,分裂持续到 etcd 恢复。这是 §3 的取舍:分不清 etcd 整体故障与单边失联时,
   宁可继续服务。
2. **控制域丢数据时会大面积排空**:控制域实例被清空、换成空实例、或故障切换丢了最近的写入,本节点的场景会读成 `no_mapping`,
   过宽限期后被排空,住户改派到 scene_manager 重新铺好的频道。这与 scene_manager 自己的口径一致(它也按丢后的状态铺频道、找不到这些实例),
   收敛方向正确,但同一时刻全 zone 的玩家一起改派。部署手册要求切换控制域实例前先停本 zone 的 scene 节点(control-plane-zone-scoping §4.6),
   这条仍要遵守。没有加「大面积缺失就压住」的闸门:第 33 条的实例节点本来就是全部缺失,压住只会推迟收敛。
3. **宽限期依赖 scene_manager「先建后改」窗口的上界**。以后新增改派路径,若预建之后隔很久才 CAS,或者 CAS 到某节点而不先对它 CreateScene,
   要同时调大 `kCreateGrace` 或改这里的判据。`scene_ownership_rules.h` 与本节都写明了这条跨模块依赖。
4. **控制域换成 Redis Cluster 时 MGET 会跨槽报错**:报错整批按读不到处理,只会不拆、不会误拆;真要上 Cluster(control-plane-zone-scoping §5)
   时改成按槽分批。
5. **排空期间每轮重调一次 DestroyScene**,`BeginSceneDrain` 每次打一行 WARN;与 scene_manager 重发 DestroyScene 的频率同量级。
6. **断线保留中的玩家会被直接退出**(只存盘不改派):与频道缩容排空同一口径。短重连时 scene_manager 按 location 回到同号场景的新属主。
7. **还没有告警规则**。C++ scene 的指标出口按 `MMORPG_METRICS_PORT` 开,接告警时建议 `increase(scene_ownership_releases_total{stage="drain_started"}[15m]) > 0`
   报 warning,`scene_ownership_releasing_scenes` 持续大于 0 超过 10 分钟报 warning(住户迟迟不走)。

## 10. 文件与验证

**改动**:

- 新增 `cpp/libs/services/scene/world/scene_ownership_rules.h`(纯规则)、`world/scene_ownership_reconciler.{h,cpp}`(核对状态组件、
  读数适配、ECS 级应用、定时宿主、指标)。登记进 `cpp/libs/services/scene/scene.vcxproj` 与 `.filters`(CMakeLists 由 vcxproj 生成)。
- `world/scene_lifecycle.{h,cpp}`:新增 `SceneLifecycle::ReuseExistingScene`;`CreateScene` / `DestroyScene` 不变。
- `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` `HandleCreateScene`、`cpp/nodes/scene/handler/rpc/scene_handler.cpp`
  `SceneHandler::CreateScene`:去重分支改调 `ReuseExistingScene`。
- `cpp/nodes/scene/main.cpp`:`SceneRuntimeContext` 持有 `SceneOwnershipReconciler`;登记指标;`SetAfterStart` 里 Start;三条停机路径与 Node 析构后 Stop。
- 新增用例 `cpp/tests/cross_zone_test/scene_ownership_reconcile_test.cpp`,登记进 `cross_zone_test.vcxproj` 与 `.filters`。

**给 Codex**:仓库根,MSBuild 串行 `/m:1`,失败不连续重试,保留第一条 `error C` / `LNK` 原文。同一工作区里「世界频道改派与排空收尾的属主一致性」
会话在同几个文件上叠加放置代次(`scene_lifecycle`、两个 CreateScene / DestroyScene 入口、`scene_placement_fence.h`),编译错误先按文件归属分清。

1. `msbuild cpp/libs/services/scene/scene.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`
2. `msbuild cpp/nodes/scene/scene.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`
3. `msbuild cpp/tests/cross_zone_test/cross_zone_test.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`(测试工程链接预编好的库,第 1 步不能省)
4. `build\cpp\tests\cross_zone_test.exe --gtest_filter=SceneOwnership*`:`SceneOwnershipRules.*` 7 条、`SceneOwnershipReply.*` 2 条、
   `SceneOwnershipEcs.*` 11 条,共 20 条全部 PASS;再跑 `--gtest_filter=RelocateConfirmEcs.*:ExitPersistEcs.*` 与 `cross_zone_test.exe` 全量,原有用例不回归。
5. Linux:`build_linux.sh` 按 vcxproj 重新生成 CMakeLists 后编 scene 库与 scene 节点,0 error。
6. **变异取红**:在工作树里逐个改,只改所列的那一处,重编第 1、3 步再跑第 4 步的过滤器,跑完立即手工改回,用 `git diff -- <文件>` 核对与改前一致。
   这几个文件别的会话也在改,不要用 `git checkout` / `git stash` 恢复。
   - M1:`scene_ownership_rules.h` 的 `Advance` 里把 `notMineStreak >= kNotMineConfirmations` 改成 `notMineStreak >= 1`。期望
     `OneNotMineReadingPastTheGraceIsNotEnough`、`MineResetsTheStreak`、`SceneMovedToAnotherNodeWhileThisNodeWasAwayIsReleased` 红。
   - M2:`Advance` 里把 `confirmed && graceOver` 改成 `confirmed`。期望 `ReleasesOnlyAfterTwoConsecutiveNotMineReadingsPastTheGrace`、
     `UnmappedSceneIsDestroyedOnlyAfterTheGrace`、`ReusingTheSceneRestartsTheGraceAndVoidsReadingsInFlight` 红。
   - M3:`scene_lifecycle.cpp` 的 `ReuseExistingScene` 里删掉 `scene_ownership::RestartCheck(scene);`。期望 `ReusingTheSceneRestartsTheGraceAndVoidsReadingsInFlight` 红。
   - M4:`scene_ownership_reconciler.cpp` 的 `ApplyReadings` 第一遍里删掉 `tracked->trackingId != check.trackingId ||`。期望
     `ReusingTheSceneRestartsTheGraceAndVoidsReadingsInFlight` 红(作废的旧批次读数被算进了新登记的核对状态)。
   - M5:`Advance` 里把 `case Observation::kUnknown:` 挪到 `kOtherNode` / `kNoMapping` 那一组。期望 `UnreadableNeitherCountsNorClearsTheEvidence`、
     `MalformedBatchOrUnknownSelfNeverReleases` 红。
   - M6:`scene_ownership_reconciler.cpp` 的 `Sweep` 里删掉 `if (!session->IdentityConfirmed())` 那一段。期望 `ReconcilerDoesNotSweepWithoutAConfirmedIdentityOrControlRedis` 红。
7. **冒烟**(本机 `dev-start`,一个 zone、两个 scene 节点,`MMORPG_METRICS_PORT` 打开):
   - 让节点 A 的 etcd 租约过期超过 200 秒,而 A 的进程、A 到 gate 与 Redis 的连接都保持(例如用现成的 TCP 代理只切断 A 到 etcd 的链路)。
     期间至少一个机器人留在 A 承载的世界频道、一个留在 A 上的副本。本机没有不改系统环境就能做到的手段时,这一步报告「未冒烟」,
     不要为此改防火墙或装工具(AGENTS.md §10.2);
   - 恢复后 30 秒内 A 的日志出现 `[SceneOwnership] scene <id> is no longer mapped to this node: observed=other_node` 与 `observed=no_mapping` 各至少一条,
     随后 `destroyed unmapped scene`;机器人分别回到同号频道(新属主)与大世界;`scene_ownership_releases_total{stage="destroyed"}` 增长;
   - `scene_ownership_observations_total{result="unknown"}` 不持续增长;
   - 对照组:断开不到 180 秒就恢复,之后没有任何 `[SceneOwnership]` 排空日志。

**通过标准**:第 1~5 步 0 error;第 4 步 20 条全绿、原有用例不回归;第 6 步每个变异的红绿与上面一致;第 7 步日志与指标符合描述(或明确报告未冒烟)。
失败时保留:第一条编译错误原文;失败用例用 `--gtest_filter=<用例全名>` 单跑的完整输出;冒烟时 A 的 scene 日志与 scene_manager 日志中 T0 到恢复后 60 秒的片段。

## 11. Java 版

不适用。Java 版的 scene 节点本来就按计划拉取场景,不在计划里的频道自己排空(Java 仓 `PARITY.md`「场景实例与主世界频道」行 D3),
没有「scene_manager 推送建场景、节点只认推送」的结构,也就没有本文的孤儿。不往 `PARITY.md` 加行。
