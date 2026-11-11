# scene 节点的场景归属对账(缺口清单第 33 条)

状态:**已落码,未编译、未测试,待 Codex 验证(§10)。** 2026-11-06 初版(节点回来之后对账);2026-11-10 补完三项残余:
与 etcd 失联期间也按映射收敛(§4.5)、节点号被新进程接走时全部排空(§4.6)、开始排空按住户预算限速(§4.7),加了告警(§8)。
Java 版只登记待做(§11)。

关联:[缺口清单](../handoff/server-gap-audit-20261101.md) 第 32、33 条;[再入屏障](scene-owner-reentry-barrier.md) §3.2;
[场景创建架构](scene-creation-architecture.md)「死节点收尾」R1-I5;[控制面按 zone 化](control-plane-zone-scoping-20261105.md) §4.2、§4.3、§4.5;
[世界频道放置代次](world-channel-placement-generation.md);[ECS 组件访问规则](ecs-component-access-rules.md)。

## 1. 结论

scene 节点每 10 秒用一条 MGET 读本节点号的持有者记录 `node:zone:{z}:{n}:holder` 和本地全部场景的 `scene:{id}:node`
(经本 zone 的场景控制域句柄 `tlsSceneControlRedis`,只读)。一个本地实体满足下面两条,就按本轮的住户预算走与 scene_manager
发来的 DestroyScene 完全相同的路径:`SceneLifecycle::DestroyScene` → `PlayerLifecycleSystem::BeginSceneDrain` 把住户存盘后改派,
没有绑定了才销毁实体。

1. 连续两次读到「不归本进程」:映射指向别的节点、映射不存在,或者本节点号已被更新的登记(别的进程)接走;
2. 距它第一次被核对、或最近一次 CreateScene 按号命中它已满 60 秒。

核对不看本节点的 etcd 身份:etcd 整体故障时 scene_manager 不判任何节点死亡、映射不变,读到的都是本节点;只有本节点单边失联、
被判死之后映射才会变。所以失联期间照样收敛,不用等 etcd 恢复。每轮每个节点最多新开始改派 200 名住户,控制域丢数据时
全 zone 也不会同一时刻一起改派。

这是 Kubernetes kubelet 的做法:按 API server 上的期望状态停掉本地多余的 Pod。期望状态只有一份(scene_manager 写的映射与持有者),
节点按它收敛,不自己判断谁该死。节点不写控制域任何键;scene_manager 侧只多写一把持有者键(§4.6),不加协议。

## 2. 问题

时间线(租约 TTL 180s,再入屏障 20s = C++ 冲突疏散预算 15s + 时钟余量 5s):

| 时刻 | 节点 A(与 etcd 单边失联,仍连着 gate 与 Redis,玩家还在玩) | scene_manager |
|---|---|---|
| T0 | keepalive 发不出去 | — |
| T0+90s | 身份转为不确认(半个租约没收到 ACK),每 5s 给玩家补存一次(既有) | — |
| T0+180s | — | 租约到期,watch DELETE:写 death_at、摘负载集、入死节点收尾任务 |
| T0+200s | — | 屏障放行:拆掉 A 名下实例的映射(A 不在负载集,不发 DestroyScene);世界频道「先建后改」改派到别的节点 |
| 之后某刻 | — | A 的节点号按「最小空闲 id」被新起的进程 B 拿走(可能发生),B 登记;还挂在这个号上的频道由 B 承载 |
| T0+X | 连上 etcd:号还空着就按原 node_id、同一个 node_uuid 重新注册;号已被 B 拿走就因分配键冲突疏散退出 | — |

A 的进程里因此留着三类实体,玩家与新位置上的玩家互相看不见,实例谁也回收不了:

- **实例(副本、镜像)**:映射已被拆掉,新玩家进不去,scene_manager 的空闲回收看不到它;
- **世界频道,改派到了别的节点号**:映射指向别的节点;
- **世界频道,留在原节点号上、由接走这个号的 B 承载**:映射仍是「A 的号」,只看映射分不清是不是自己。

2026-11-06 的初版只在 A 身份确认有效(重新注册成功)之后核对:A 一直连不上 etcd 时什么也不做,第三类在 A 重新注册失败、
疏散退出之前一直留着。玩家数据不受影响:被接管过的玩家,A 的存盘由 owner epoch / 写权行拒绝;没被接管的照常存盘。
这是场景归属的一致性问题。

## 3. 不选「失租就停服」

etcd 整体故障时所有节点同时失租,节点分不清「etcd 挂了」和「只有我连不上」,失租自停等于 etcd 一挂全服掉线。
`go/scene_manager/internal/constants/reentry_barrier.go` 文件头(2026-11-05 更正)定的也是:老节点继续服务,靠写权行挡回档。

本设计不靠节点自己猜:节点按控制面的结论行事。etcd 整体故障时控制面不会做出任何「拿走 A 的场景」的结论,A 照常服务;
单边失联时控制面判 A 死、把场景改派或拆掉、号可能给了别人,这些结论都写在控制域里,A 读到就收敛。

## 4. 做法

### 4.1 期望状态的事实源

- **场景属主** `scene:{id}:node`:值是属主 node_id 的十进制串(Go 侧 `strconv.FormatUint`,`createscenelogic.go` 的 `SceneNodeKeyFmt`)。
- **节点号持有者** `node:zone:{z}:{n}:holder`:值是 `"<etcd ModRevision>:<node_uuid>"`,这个节点号最近一次登记的进程(§4.6)。
- 两把键的 C++ 写法在 `scene_ownership::SceneNodeKey` / `NodeHolderKey`,与 Go 侧是跨语言契约,改名必须同批改
  (C++ 用例 `KeysMatchTheSceneManagerKeys`、Go 用例 `TestNodeHolderKeyShape` 钉住同一个串)。
- 只读。节点不写控制域任何键:不变量「zone z 的控制域键只由 zone z 的 scene_manager 读写」(control-plane-zone-scoping §4.3)不变。
- 只看 `scene:{id}:node`,不读 `scene:{id}:zone`:节点只托管本 zone scene_manager 建的场景(发现按 zone 收窄,scene_manager 只认得本 zone
  的节点),场景号全局唯一,别的 zone 不会改写本 zone 场景的映射(认领脚本比 zone、CreateScene 拒别的 zone)。真出现「映射等于本节点号、
  却是别的 zone 写的」这种违例,结果是留着不拆,方向安全。
- 不读反向索引 `node:zone:{z}:{n}:scenes`:那是派生索引,路由与收尾的判据都是映射本身。

### 4.2 判定(`world/scene_ownership_rules.h`,纯函数)

每个本地实体一份核对状态 `SceneOwnershipCheckComp`(按需状态,只在内存):宽限起点、连续「不归本进程」次数、核对状态号、
是否已开始排空、是否在等预算。

| 读数 | 归类 | 对状态的作用 |
|---|---|---|
| 本节点号已被更新的登记接走(§4.6),不看映射 | `superseded` | 连击 +1(封顶 2) |
| 映射等于本节点号 | `self` | 连击清零,留着;已在排空或在等预算的,停下 |
| 映射是别的节点号 | `other_node` | 连击 +1(封顶 2) |
| 映射不存在、空串 | `no_mapping` | 连击 +1(封顶 2) |
| 整条命令失败、断线空应答、元素类型不对、本节点号未知 | `unknown` | 不改连击,不动手 |

释放条件:连击达到 `kNotMineConfirmations = 2`,并且这次读的**发出时刻**距宽限起点已满 `kCreateGrace = 60s`。
判定释放之后,只要后续读数仍不归本进程,每一轮都再走一次销毁(幂等,住户走完之前实体留着)。排空途中读到映射又回到本节点
(scene_manager 把它迁了回来),就停止排空;已经改派走的住户由 scene_manager 重新落点。

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
2. 按玩家绑定重新数一遍;还有绑定就留着实体,下一轮仍不归本进程再调;没有了才销毁(同时记 Agones 单元账)。

改派之后玩家的去向由 scene_manager 定,节点不复制规则:世界频道的住户回到同号频道(已在新属主上),实例与镜像的住户回大世界。
第一次开始排空受住户预算约束(§4.7);已在排空的实体每轮照常重调,不占预算。

对账器在进程内直接调 `SceneLifecycle::DestroyScene`,不经 DestroyScene RPC,也就不经放置代次的栅栏。这是有意的:栅栏挡的是迟到的陈旧
请求,对账器的输入是刚读到的期望状态本身。

### 4.5 门禁:为什么不看 etcd 身份

| 门禁 | 不满足时 |
|---|---|
| 控制域 Redis 已连上 | 本轮不核对(`redis_unavailable`) |
| 上一轮的应答都回来了 | 本轮不发(`previous_in_flight`)。Redis 卡住时 RedisManager 的存活探测最迟约 20 秒摘掉连接,在途命令回空应答,计数归零 |
| 应答回来时本节点号与发读时相同 | 这批读数作废,计 `unknown` |
| 应答回来时实体仍在、核对状态号与 scene_id 都没变 | 这个实体的读数作废 |

初版另有一道「本节点 etcd 身份确认有效(`EtcdService::IsIdentityConfirmedFresh`)才核对」,2026-11-10 去掉。理由:

1. **「不归本进程」的三种读数都不依赖 etcd 身份就成立。** 映射指向别的节点号、映射不存在:不论本进程还持不持有这个号,scene_manager 都不要
   这个场景在这里。节点号被更新的登记接走:持有者记录本身就是证明(§4.6)。唯一依赖身份的是「映射等于本节点号」能不能当成「是我」,
   而这个读数只会让实体留着,方向安全;它的歧义由 §4.6 处理。
2. **etcd 整体故障不会触发排空。** scene_manager 判死只来自 watch DELETE 与全量同步的陈旧清扫;etcd 整体故障时 watch 收不到 DELETE,
   全量同步读不到快照或被存活集闸门(B1-1)压住,不会判任何节点死亡,映射不变。etcd 恢复时租约随新领导者续期,节点 1~2 秒内按同一
   node_uuid 重新注册,第 32 条在屏障内撤掉收尾任务,映射同样不变。
3. **改派链路不需要身份。** 存盘、交接标记、EnterScene 都不过身份闸;过身份闸的只有断线释放标记(A1′)与改派踢线前的凭证补写,
   身份不确认时跳过,与整节点疏散(身份冲突时)完全同一口径,只是范围是单个场景。

身份探针仍然注入(与 A1′ 同一个函数),只用来解读持有者记录:身份确认有效时本进程此刻持有分配键,别人的记录只能是旧值(§4.6)。

### 4.6 节点号被新进程接走

C++ 节点按「最小空闲 id」分配 node_id。A 的租约到期之后,分配键随之过期,新起的进程 B 可以拿走同一个号。A 的世界频道如果在
A 判死时没被改派(比如 B 在屏障走完之前就登记了,这个号看起来一直活着),scene_manager 之后会把频道补建到这个号上,也就是 B 上;
映射的值没变。A 只看映射会一直以为是自己的。

**scene_manager 写持有者**(`go/scene_manager/internal/logic/node_holder.go`):

- 每次 watch PUT(每个副本都写,与 `clearNodeDeath` 同一口径)与领导者的全量同步,把这次登记写成
  `node:zone:{z}:{n}:holder = "<etcd ModRevision>:<node_uuid>"`,TTL 24 小时(全量同步约每分钟给在册节点续一次)。
- Lua 只在新值的修订号不小于已存值时覆盖(十进制串先比长度再比字典序,与放置代次同一个比较函数,大于 2^53 不丢精度)。
  ModRevision 是 etcd 全局单调的修订号,B 只能在 A 的分配键过期之后登记,它那次登记的修订号一定更大;副本之间先后乱序也盖不回旧的。
- 不随 DELETE 删除:节点死了,它是这个号最后一次登记的进程仍是事实,新进程登记时自然覆盖。没有 node_uuid 或修订号的登记不写。
- 只发已过存活集闸门的快照:批量发布放在 `applyNodeSnapshot` 的领导者分支里,闸门(B1-1)拒掉的残缺快照在替换镜像之前就返回了。
- **etcd 换成新集群、修订号从 1 重计**(运维规定所有节点重启):控制域里旧集群的记录修订号比新集群的任何登记都大,单调写会把新登记
  全部拒掉。全量同步带上这次快照的应答头修订号,已存记录比它还新就直接覆盖:快照是线性一致读,同一个 etcd 里任何已观察到的登记都
  不晚于它,所以这只可能是换集群之前留下的。watch PUT 不知道 etcd 当前修订号,只做单调覆盖;换集群后最迟一次全量同步(约一分钟)修正。
- 键以 `node:zone:{z}:` 开头,与其他节点级控制域键一样只由本 zone 的 scene_manager 写,合服按前缀清得到;不带 hash tag。

**节点判定**(`scene_ownership::HolderTracker`,跨轮记着本进程自己那次登记的修订号 `ownRevision`):

| 持有者读数 | 判定 |
|---|---|
| 是本进程的 uuid | 记下修订号(最近一次见到的,不取历史最大:记录单调写,只有 etcd 换集群后被全量同步改写时才会变小,那时要跟着新集群比);按映射判 |
| 别人的 uuid,修订号 > ownRevision,本节点身份**不**确认有效 | **被接走**:本地实体一律 `superseded` |
| 别人的 uuid,但本节点身份确认有效 | 按映射判:本进程此刻持有分配键,那条只能是登记之前留下的旧值 |
| 别人的 uuid,但修订号不大于 ownRevision,或还没见过本进程自己的记录(ownRevision = 0) | 按映射判:证明不了别人更新 |
| 键不存在、读不到、记录损坏、不知道自己的 uuid | 按映射判:没有信息 |

判据里没有时间推断,只有「别人、比我那次登记更新」这一条证明,所以不会因为 scene_manager 迟迟没处理某次 PUT 而误判:新进程接走号
之后、scene_manager 写下它的记录之前,控制域里是更旧的记录,新进程自己的身份也确认有效,两道都不会让它把刚分到的场景排空。
代价是本进程登记之后 scene_manager 一次都没写过它的持有者记录(ownRevision 一直是 0)时认不出被接走,退回到 A 重连 etcd 时分配键冲突、
疏散退出那条老路(§9 第 1 条)。

本进程那次登记的修订号也可以从自己注册时 etcd 应答头里拿,不必等 scene_manager 写回来。没有这样做:要改 `etcd_service.cpp` 的注册路径
并多注入一个探针,而它能补的只是上面那一种「scene_manager 一次都没写过」的情形;从持有者记录学到的修订号与 scene_manager 用的是同一个
来源,换集群时一起被修正,不会出现节点拿新集群的小修订号、记录还是旧集群大修订号而误判被接走的窗口。

被接走之后 A 不主动退出进程:它的场景全部排空、没人再往它身上放东西;重连 etcd 时照常因分配键冲突疏散退出。

### 4.7 限速:按住户预算开始排空

每轮核对的全部批次共用一份 `ReleaseBudget`,缺省 `kMaxRelocationsPerSweep = 200` 名住户:

- 判定释放、还没开始排空的实体,按它绑定的玩家数(与 `BeginSceneDrain` 改派的范围同一口径:`SceneEntityComp` + `PlayerGuidComp`)扣预算;
  不够就标记「等预算」(`releaseDeferred`),留到下一轮,下一轮连击仍是 2、照样判释放、重新竞争预算。
- 没有玩家的实体不占预算,随时销毁。已经开始排空的实体每轮重调,不占预算(它的住户早已登记改派)。
- 本轮第一个有住户的实体超出预算也放行:一个满员频道的住户可能比预算还多,不能永远排不动。
- 应答按发出顺序回来,先回来的批次先用。一个 zone 的改派速率上限 ≈ 节点数 × 200 / 10 秒,单个节点的瞬时量最多一个场景的住户
  (整节点疏散一次就是全部住户,这个量系统本来就要扛)。

控制域丢数据(被清空、换成空实例、主从切换丢写入)时,全 zone 的场景都会读成 `no_mapping`,收敛方向仍是排空:scene_manager 也按丢后的
状态重新铺频道、找不到这些实例,不排空它们会一直孤立。限速让这件事分批发生,scene_manager 有时间重建频道,被改派的玩家落到新频道;
告警 `SceneOwnershipMassRelease` 让值班去查控制域为什么丢了状态。没有加「大面积缺失就压住」:第 33 条的实例节点本来就是全部缺失,
丢掉的状态也不会自己回来,压住只会推迟收敛。200 是保守的初值,没有压测基线,要按压测里 EnterScene 与 go/db 的余量复核
(`SceneOwnershipReleaseBacklog` 持续响说明偏小)。

### 4.8 启停与生命周期

`SetAfterStart` 里 Start(此时 node_id 与 node_uuid 已是终值);普通停机(`exitAllPlayers`)、身份冲突关停(`SetOnConflictShutdown`,
整节点疏散接管全部玩家)、拆除前(`SetBeforeTeardown`)各 Stop 一次,Node 作用域结束后再 Stop 一次兜底。`exitAllPlayers` 在 Node 析构的
兜底 drain 循环之前执行,所以探针捕获的 node 在它析构之前就解开了。定时器与 Redis 回调只捕获内部 Session 的 `weak_ptr` 与本轮预算的
`shared_ptr`(AGENTS.md §11.7),Stop 之后到达的应答直接丢弃。

### 4.9 多久收敛

- A 一直连不上 etcd:屏障放行(T0+200s)之后,控制域里的映射一变,A 下一轮读到连击 1,再下一轮(+10s)开始排空;节点号被 B 接走的,
  从 scene_manager 写下 B 的持有者记录算起同样两轮。
- A 连回 etcd、按原号重新注册:同上,两轮之内开始排空。
- 住户多于预算时按每轮 200 名分批。

## 5. 各类场景的口径

| 场景 | 映射怎么写 | 对账结果 |
|---|---|---|
| 世界频道(MainWorld / MainWorldCross 节点) | 登记脚本先写映射、同一轮 init 再补 CreateScene;改派先建后改 | 被改派走:`other_node`,排空,住户回同号频道的新属主 |
| 副本实例(Instance / InstanceCross 节点) | 登记脚本先写映射,再 CreateScene;失败回滚删映射、发 DestroyScene | 死节点收尾拆掉:`no_mapping`,排空,住户回大世界 |
| 镜像 | 按实例登记,属主 = 与镜像源共置的节点(`resolveMirrorSourceNode`),写的是承载它的节点 | 同实例。源频道被迁走时 scene_manager 级联拆镜像;DestroyScene 丢了也由这里收掉 |
| 本节点号已被新进程接走,频道留在这个号上 | 映射仍是这个号 | `superseded`,全部排空,住户回到 B 上的同号频道 |
| 正在缩容排空的频道(`scene:{id}:draining`) | 映射仍是本节点 | `self`,不动,由 scene_manager 的排空负责 |
| 正在迁往本节点的预建 | 映射还指旧属主 | 宽限期内不动;命中已有实体时宽限期重算 |
| 迁走之后旧属主上的实体 | 映射已是新属主 | `other_node`,与 scene_manager 随后发来的 DestroyScene 同一个动作,谁先到都一样 |

跨 zone 类型(MainWorldCross = 2、InstanceCross = 3)只是本 zone 节点的用途,不是跨 zone 托管:它们在本 zone 注册,只被本 zone 的
scene_manager 认得和派活,映射在本 zone 的控制域里。跨 zone 传送是客户端重连到目标 zone,目标 zone 的场景由目标 zone 的节点承载
(cross-zone-scene-travel.md)。部署脚本目前只渲染 0、1 两种。口径与普通节点相同,不用单独处理。

## 6. 为什么不会误拆

本地实体只会因为下面这件事被拆:两次相隔一个周期的读(本节点号不变时发出)都说它不归本进程,而且后一次发出时距最近一次
CreateScene 命中(或第一次核对)已满 60 秒。逐条对照:

1. **节点上的场景只来自本 zone scene_manager 的 CreateScene**(gRPC `SceneNodeGrpc.CreateScene`;muduo 版 `Scene.CreateScene` 全仓只有用例在调)。
2. **新场景先写映射再 CreateScene**(`scene_registration.go`):实体建好时映射已经指向本节点。之后读到不是本节点,只能是映射被拆或被改走了。
3. **改派都是先建后改,CAS 紧跟 CreateScene**,上界远小于宽限期;CAS 落空的预建由 scene_manager 按放置代次回收。
4. **生产代码里没有「CAS 到本节点却不先 CreateScene」的写法**(不预建的 `reassignSceneNode` 只剩用例在用,2026-11-06 核对)。
   所以任何把场景重新放回本节点的动作,都会先有一次 CreateScene 命中本地实体,把宽限期与在途读数清掉。
5. **`superseded` 只来自证明**:别人的登记、修订号比本进程那次登记更新、而且本进程此刻身份不确认有效(§4.6)。
6. **etcd 整体故障不改映射**(§4.5 第 2 条),所以去掉身份门禁不会让全服在 etcd 故障时排空。
7. 读是只读的 MGET,不参与 scene_manager 的任何原子范围,不会让它的脚本看到中间状态。

剩下的误差来源是控制域本身丢数据:那时排空是正确的收敛方向,按 §4.7 分批。

## 7. 顺带收掉的孤儿

对账的输入只有「是不是本进程的」,所以凡是「有实体没映射」都会在宽限期后收掉,不限于第 33 条:

- 回滚补发的 DestroyScene 早于节点处理完在途的 CreateScene,实体留下(11-05「场景拆除任务」条目的残余,原写「要 C++ 侧墓碑」);
- rebalance 迁移之后发往旧属主的 DestroyScene 失败(它是尽力而为、失败只记日志),旧实体与住户留在旧节点上;
- 频道缩容的最后一次 DestroyScene 丢失,空实体留在节点上;
- 放置代次落地之后,迟到的旧建请求若仍建出孤儿(放置代次设计里的兜底)。

## 8. 开销与可观测性

**开销**:每轮每个节点发 ceil(本地场景数 / 256) 条 MGET,每条多带一个持有者键;一个控制域实例每秒读的键数 ≈ 同组全部场景数 / 10。
例:一组 2 万个场景约 2000 键/s。上一轮没回来不发下一轮,不叠加。scene_manager 每次 PUT 与每次全量同步(领导者,每个在册节点一次)
多一次 EVAL。

**日志**(前缀 `[SceneOwnership]`,英文):开始排空时一条 WARN(scene_id、读数归类、读到的属主、本节点号、连击、已核对秒数、住户数);
有绑定留着时一条 WARN;销毁时一条 INFO;本批有实体在等预算时一条 INFO;判定本节点号被接走 / 不再被接走时各一条 ERROR / WARN;
启停各一条 INFO。之后每轮重试只有 `BeginSceneDrain` 自己的那行 WARN。scene_manager 写持有者失败时 `[NodeHolder]` 一条 ERROR。

**指标**(`RegisterSceneOwnershipMetrics`,设了 `MMORPG_METRICS_PORT` 才有出口,K8s 缺省 9400;标签都是低基数,启动即以 0 导出):

| 指标 | 类型 | 说明 |
|---|---|---|
| `scene_ownership_sweeps_total{outcome}` | counter | sent / no_scenes / redis_unavailable / previous_in_flight / send_failed |
| `scene_ownership_observations_total{result}` | counter | self / other_node / no_mapping / superseded / unknown |
| `scene_ownership_releases_total{stage}` | counter | drain_started / destroyed。有增长就说明本节点托管过已不归它的场景 |
| `scene_ownership_tracked_scenes` | gauge | 登记了核对状态的本地实体数(大面积释放告警的分母) |
| `scene_ownership_releasing_scenes` | gauge | 已开始排空、还在等住户离开的本地实体数 |
| `scene_ownership_release_deferred_scenes` | gauge | 已判定不归本进程、在等住户预算的本地实体数 |
| `scene_ownership_superseded` | gauge | 1 = 最近一次读到本节点号已被别的进程接走 |

**告警**(`deploy/k8s/scene-ownership-alerts.yaml`,PrometheusRule,与 `owner-epoch-alerts.yaml` 同法安装;处置见
`docs/ops/scene-node-role-split.md`「场景归属对账告警」;规则引用的指标名与取值由 `tools/scripts/tests/scene_ownership_alerts_contract.tests.ps1` 对照 C++ 源码钉住):

| 告警 | 级别 | 条件 |
|---|---|---|
| `SceneOwnershipUnownedScenesReleased` | warning | 某个 scene Pod 15 分钟内开始排空过任何场景 |
| `SceneOwnershipMassRelease` | critical | 一个 namespace 10 分钟内开始排空的场景数超过它场景总数的一半,且至少 10 个 |
| `SceneOwnershipNodeIdTakenOver` | critical | 某个 scene Pod 读到自己的节点号已被别的进程接走 |
| `SceneOwnershipReleaseBacklog` | warning | 某个 scene Pod 排空中与等预算的实体数持续 15 分钟大于 0 |
| `SceneOwnershipReconcileBlind` | warning | 某个 scene Pod 10 分钟里过半轮次读不到控制域,持续 5 分钟 |

## 9. 残余与风险

1. **认不出被接走的一种情形**:本进程登记之后 scene_manager 一次都没写过它的持有者记录(这段时间没有 scene_manager 在跑,或写入一直失败),
   ownRevision 一直是 0。这时号被接走、频道留在这个号上的那一类实体要等本进程重连 etcd、分配键冲突、疏散退出才收掉。
   只要 scene_manager 处理过一次本进程的 PUT 或做过一次全量同步就不会出现。
2. **不在任何场景里的玩家实体不归对账管**:节点号被接走时,对账只排空场景;在途换图等不绑定场景的玩家实体由它们自己的超时与
   重连 etcd 时的整节点疏散收掉。
3. **住户预算是没有压测基线的初值**(200 名 / 轮 / 节点):偏小时排空积压(`SceneOwnershipReleaseBacklog` 会响),偏大时控制域丢数据后
   EnterScene 与 go/db 的瞬时压力大。压测里补一轮「清空控制域」的故障演练再定。
4. **宽限期依赖 scene_manager「先建后改」窗口的上界**。以后新增改派路径,若预建之后隔很久才 CAS,或者 CAS 到某节点而不先对它 CreateScene,
   要同时调大 `kCreateGrace` 或改这里的判据。`scene_ownership_rules.h` 与本节都写明了这条跨模块依赖。
5. **控制域换成 Redis Cluster 时 MGET 会跨槽报错**:报错整批按读不到处理,只会不拆、不会误拆;真要上 Cluster(control-plane-zone-scoping §5)
   时改成按槽分批,持有者键单独读。
6. **排空期间每轮重调一次 DestroyScene**,`BeginSceneDrain` 每次打一行 WARN;与 scene_manager 重发 DestroyScene 的频率同量级。
7. **断线保留中的玩家会被直接退出**(只存盘不改派):与频道缩容排空同一口径。短重连时 scene_manager 按 location 回到同号场景的新属主。
8. **告警规则没有过 promtool,也没有装进任何集群**(§10 第 7 步)。

## 10. 文件与验证

**改动**:

- C++ 新增 `cpp/libs/services/scene/world/scene_ownership_rules.h`(纯规则:读数分类、连击与宽限、持有者判定、住户预算)、
  `world/scene_ownership_reconciler.{h,cpp}`(核对状态组件、MGET 应答适配、ECS 级应用、定时宿主、指标)。登记进
  `cpp/libs/services/scene/scene.vcxproj` 与 `.filters`(CMakeLists 由 vcxproj 生成)。
- `world/scene_lifecycle.{h,cpp}`:`SceneLifecycle::ReuseExistingScene`;`CreateScene` / `DestroyScene` 不变。
- `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` `HandleCreateScene`、`cpp/nodes/scene/handler/rpc/scene_handler.cpp`
  `SceneHandler::CreateScene`:去重分支改调 `ReuseExistingScene`。
- `cpp/nodes/scene/main.cpp`:`SceneRuntimeContext` 持有 `SceneOwnershipReconciler`;登记指标;`SetAfterStart` 里 Start;三条停机路径与 Node 析构后 Stop。
- Go 新增 `go/scene_manager/internal/logic/node_holder.go`;`load_reporter.go` 的 watch PUT 分支与 `applyNodeSnapshot` 领导者分支各一处调用;
  `scene_keys_contract_test.go` 的「控制域键不带 hash tag」名单加持有者键。
- 用例:C++ `cpp/tests/cross_zone_test/scene_ownership_reconcile_test.cpp`(登记进 `cross_zone_test.vcxproj` 与 `.filters`);
  Go `go/scene_manager/internal/logic/node_holder_test.go`;告警契约 `tools/scripts/tests/scene_ownership_alerts_contract.tests.ps1`。
- 告警 `deploy/k8s/scene-ownership-alerts.yaml`;安装说明 `deploy/k8s/README.md`;处置 `docs/ops/scene-node-role-split.md`。

**给 Codex**:仓库根执行,MSBuild 串行 `/m:1`,失败不连续重试,保留第一条 `error C` / `LNK` 原文。同一工作区里还有别的会话在改
`scene_lifecycle`、两个 CreateScene / DestroyScene 入口与 `go/scene_manager`,编译错误先按文件归属分清。

1. `msbuild cpp/libs/services/scene/scene.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`
2. `msbuild cpp/nodes/scene/scene.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`
3. `msbuild cpp/tests/cross_zone_test/cross_zone_test.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`(测试工程链接预编好的库,第 1 步不能省)
4. `build\cpp\tests\cross_zone_test.exe --gtest_filter=SceneOwnership*`:`SceneOwnershipRules.*` 11 条、`SceneOwnershipReply.*` 2 条、
   `SceneOwnershipEcs.*` 14 条,共 27 条全部 PASS;再跑 `--gtest_filter=RelocateConfirmEcs.*:ExitPersistEcs.*` 与 `cross_zone_test.exe` 全量,原有用例不回归。
5. Go,工作目录 `go/scene_manager`,`GOTOOLCHAIN=local`、`GOPROXY=off`:
   - `gofmt -l internal/logic/node_holder.go internal/logic/node_holder_test.go internal/logic/load_reporter.go internal/logic/scene_keys_contract_test.go` 无输出;
   - `go vet ./internal/logic/`、`go build ./...`;
   - `go test ./internal/logic/ -count=1 -v -run 'TestNodeHolderKeyShape|TestPublishNodeHolder_|TestWatchPutPublishesTheNodeHolder|TestApplyNodeSnapshot_|TestControlDomainKeysCarryNoHashTag|TestDeadNodeReregister_|TestNodeViewGate'`,
     其中本条新增 9 条全部 PASS;
   - `go test ./... -count=1`,既有失败逐条点名归因。
6. `pwsh -NoProfile -File tools/scripts/tests/scene_ownership_alerts_contract.tests.ps1`:4 个 Test-Case 全绿。
7. `promtool check rules deploy/k8s/scene-ownership-alerts.yaml`(先按 PrometheusRule 取出 `spec` 下的 `groups`,与其他告警文件的校验方法相同):0 error。
8. Linux:`build_linux.sh` 按 vcxproj 重新生成 CMakeLists 后编 scene 库与 scene 节点,0 error。
9. **变异取红**:在工作树里逐个改,只改所列的那一处,重编(C++ 第 1、3 步;Go 直接跑)再跑对应过滤器,跑完立即手工改回,用 `git diff -- <文件>`
   核对与改前一致。这几个文件别的会话也在改,不要用 `git checkout` / `git stash` 恢复。
   - M1:`scene_ownership_rules.h` 的 `Advance` 里把 `notMineStreak >= kNotMineConfirmations` 改成 `notMineStreak >= 1`。期望
     `OneNotMineReadingPastTheGraceIsNotEnough`、`MineResetsTheStreak`、`SceneMovedToAnotherNodeWhileThisNodeWasAwayIsReleased` 红。
   - M2:`Advance` 里把 `confirmed && graceOver` 改成 `confirmed`。期望 `ReleasesOnlyAfterTwoConsecutiveNotMineReadingsPastTheGrace`、
     `UnmappedSceneIsDestroyedOnlyAfterTheGrace`、`ReusingTheSceneRestartsTheGraceAndVoidsReadingsInFlight` 红。
   - M3:`scene_lifecycle.cpp` 的 `ReuseExistingScene` 里删掉 `scene_ownership::RestartCheck(scene);`。期望 `ReusingTheSceneRestartsTheGraceAndVoidsReadingsInFlight` 红。
   - M4:`scene_ownership_reconciler.cpp` 的 `ApplyReadings` 第一遍里删掉 `tracked->trackingId != check.trackingId ||`。期望
     `ReusingTheSceneRestartsTheGraceAndVoidsReadingsInFlight` 红(作废的旧批次读数被算进了新登记的核对状态)。
   - M5:`Advance` 里把 `case Observation::kUnknown:` 挪到 `kOtherNode` / `kNoMapping` 那一组。期望 `UnreadableNeitherCountsNorClearsTheEvidence`、
     `MalformedBatchOrUnknownSelfNeverReleases` 红。
   - M6:`HolderTracker::Observe` 里删掉 `identityConfirmed ||`。期望 `HolderTrackerNeedsANewerRegistrationOfAnotherProcess` 红。
   - M7:`HolderTracker::Observe` 里把 `record->revision <= ownRevision` 改成 `false`。期望 `HolderTrackerNeedsANewerRegistrationOfAnotherProcess` 红。
   - M8:`ReleaseBudget::TryStart` 开头直接 `return true;`。期望 `ReleaseBudgetAdmitsEmptyScenesAndTheFirstLargeScene`、
     `ReleaseBudgetDefersScenesWithResidentsToLaterSweeps`、`MetricsExposeTheCountersAndGauges` 红。
   - M9:`ApplyReadings` 里把 `slotHolder == SlotHolder::kAnotherProcess ? Observation::kSuperseded : ` 删掉(只留 `Classify(...)`)。期望
     `NodeIdTakenOverReleasesScenesStillMappedToIt` 红。
   - M10:`Sweep` 里在读控制域之前加回旧门禁 `if (!session->IdentityConfirmed()) { return finish(SweepOutcome::kNoScenes); }`。期望
     `ReconcilerSweepsWithoutEtcdIdentityButNeedsControlRedis` 红。
   - M11(Go):`luaPublishNodeHolder` 里删掉 `generation_after` 那一段判断(无条件 SET)。期望 `TestPublishNodeHolder_NewerRegistrationWins`、
     `TestPublishNodeHolder_ComparesRevisionsAsDecimalStrings`、`TestWatchPutPublishesTheNodeHolder` 红。
   - M12(Go):`handleWatchEvent` 里删掉 `publishNodeHolder(...)` 那一句。期望 `TestWatchPutPublishesTheNodeHolder` 红。
   - M13(Go):`applyNodeSnapshot` 里删掉 `publishNodeHolders(svcCtx, holders, revision)`。期望 `TestApplyNodeSnapshot_LeaderPublishesHoldersOfRegisteredNodes`、
     `TestApplyNodeSnapshot_RepairsHoldersLeftFromBeforeAnEtcdReset` 红。
   - M14(Go):`luaPublishNodeHolder` 里删掉 `and (ARGV[4] == '' or not generation_after(currentRevision, ARGV[4]))`。期望
     `TestApplyNodeSnapshot_RepairsHoldersLeftFromBeforeAnEtcdReset` 红(换集群之前的记录修不掉)。
   - M15:`HolderTracker::Observe` 里把 `ownRevision = record->revision;` 改成 `ownRevision = std::max(ownRevision, record->revision);`。期望
     `HolderTrackerNeedsANewerRegistrationOfAnotherProcess` 红(换集群之后认不出接走号的进程)。
10. **冒烟**(本机 `dev-start`,一个 zone、两个 scene 节点,`MMORPG_METRICS_PORT` 打开):
    - 让节点 A 的 etcd 租约过期超过 200 秒,而 A 的进程、A 到 gate 与 Redis 的连接都保持(例如用现成的 TCP 代理只切断 A 到 etcd 的链路)。
      期间至少一个机器人留在 A 承载的世界频道、一个留在 A 上的副本。本机没有不改系统环境就能做到的手段时,这一步报告「未冒烟」,
      不要为此改防火墙或装工具(AGENTS.md §10.2);
    - **不恢复 etcd 链路**,屏障放行后 30 秒内 A 的日志出现 `[SceneOwnership] scene <id> is no longer owned by this process: observed=other_node`
      与 `observed=no_mapping` 各至少一条,随后 `destroyed unowned scene`;机器人分别回到同号频道(新属主)与大世界;
      `scene_ownership_releases_total{stage="destroyed"}` 增长;
    - 再起第三个 scene 进程,让它拿到 A 的节点号(A 的分配键已过期),A 的日志出现 `is held by a newer registration of another process`,
      `scene_ownership_superseded` 变 1;
    - 对照组:断开不到 180 秒就恢复,之后没有任何 `[SceneOwnership]` 排空日志。

**通过标准**:第 1~3、8 步 0 error;第 4 步 27 条、第 5 步新增 9 条全绿,原有用例不回归;第 6 步全绿;第 7 步 0 error;第 9 步每个变异的红绿
与上面一致;第 10 步日志与指标符合描述(或明确报告未冒烟)。失败时保留:第一条编译错误原文;失败用例单跑的完整输出;冒烟时 A 的
scene 日志与 scene_manager 日志中 T0 到恢复后 60 秒的片段。

## 11. Java 版

只登记待做,不写 Java 代码(负责人 2026-11-10:Java 留到 todo)。Java 版的 scene 节点按计划拉取场景、不在计划里的频道自己排空
(Java 仓 `PARITY.md`「场景实例与主世界频道」行 D3),没有本文的「推送建场景、节点只认推送」结构,初版判断为不适用;但 2026-11-10 补的
三项要在 Java 侧逐项核对:拉计划是否受 etcd 身份门禁、计划里的节点标识会不会被新进程复用、按计划自己排空时有没有限速,以及对应告警。
登记在 Java 仓 `PARITY.md`「2026-11-10 scene 节点归属对账」一节。
