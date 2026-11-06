# 世界频道的放置代次

> 2026-11-06 落码,未编译、未测试,待 Codex 验证(命令见 `docs/PROGRESS.md` 同日条目「世界频道放置代次」)。
> Java 版不适用,见 §9。

## 0. 结论

- scene_manager 每次把一个世界频道放到某个节点上(换节点的四条路径),先从本 zone 的计数器分一个**放置代次**。
  这次尝试的建场景、改派提交、落空回收、拆老实体都带这一个值。
- scene 节点按场景记下见过的代次:**拆场景只拆不比请求新的实体,建场景不复活已经按同代或更新的代次拆掉的放置**。
  网络分区恢复后才送到的旧请求,不论晚多久,都只作用在它自己那一次放置上。副本、镜像与世界频道的退役拆除不带代次(0),
  行为不变。
- scene_manager 侧每个频道有一个代次高水位 `scene:{id}:placement_gen`:改派提交要求自己的代次比它新,并把它写成自己;
  回收预建实体之前先把它抬到这次尝试的代次。由此保证:之后合法落地的放置,代次一定比之前发出的任何回收都新。
- 取代 2026-11-06 上午加的 60 秒回收栅栏 `scene:{id}:reclaim_fence:{node}`:栅栏只盖住 60 秒内送到的旧 DestroyScene,
  代次不依赖时间。

## 1. 要解决的问题

世界频道换节点是「先建后改」:先在新节点 CreateScene(按 scene_id 幂等),返回后 CAS 属主;CAS 落空就回收预建的实体
(DestroyScene)。回收的 DestroyScene 可能晚到:本端已经超时,请求还卡在连接里,分区恢复后才送达。晚到时,这个频道可能
已经被合法地改派到同一个节点:

1. 写者 A 想把频道 S 从 O 搬到 N:在 N 上 CreateScene,CAS 落空(属主已被别人改走),向 N 发 DestroyScene(S)。请求卡住。
2. 过一段时间,写者 B 把 S 合法地搬到 N:CreateScene(复用或新建实体),CAS 落地,属主 = N。
3. A 的 DestroyScene 送达 N,按 scene_id 拆掉了 B 的实体。属主指向 N,N 上没有实体。要等下一次
   initWorldScenesForZone 才补建,期间进这个频道的玩家被派到没有场景的节点。

rebalance 迁移成功之后拆老节点实体的 DestroyScene 同理:频道之后迁回老节点,晚到的那次拆除会拆掉迁回来的实体。

11-06 上午的回收栅栏让「回收之后 60 秒内」改派到 N 的 CAS 落空,只盖住 60 秒以内的晚到。要不依赖时间,节点必须能分辨
一次拆除针对的是哪一次放置。

## 2. 代次与两把键

| 键 | 值 | 写入 | 删除 |
|---|---|---|---|
| `world_channels:placement_seq:zone:{z}` | 本 zone 的代次计数器 | 每次换节点的尝试 INCR 一次 | 不删,每个 zone 一把 |
| `scene:{id}:placement_gen` | 频道的代次高水位(十进制) | 改派 CAS 落地时写成自己的代次;回收之前抬到回收的代次 | 世界频道的拆除任务(排空收尾、孤儿清理) |

- 代次 ≥ 1,同一 zone 内严格递增、不重复(INCR;go-redis 重发 INCR 只会跳号,不会重号)。比较只在同一个场景内进行。
  计数器按 zone 而不是按场景,省掉每个场景一把要清理的计数键。
- 高水位不存在 = 0:频道登记之后从没换过节点。
- 两把键都是控制域键,不带 hash tag(`scene_keys_contract_test.go` 守着)。
- Lua 里比较代次按十进制串「先比长度再比字典序」(`luaPlacementGenerationAfter`),不经 `tonumber`:Lua 的数是双精度,
  大于 2^53 会丢精度。

## 3. scene_manager 的规则

### 3.1 换节点

四条路径:rebalance(`migrateWorldChannel`)、懒改派(`lazyReassignStaleWorldChannel`)、init 的死节点改派
(`moveWorldChannelOffDeadNode`)、resolveScene 的死节点自愈(`reassignSceneOffDeadNode`)。

1. 前置判断不变(重读属主、存活、身份歧义、再入屏障)。
2. `allocatePlacementGeneration` 分配代次 p。出错就放弃这次尝试,什么都还没发。
3. 在新节点 `CreateScene(scene, p)`。节点按代次拒收时回空应答,按建场景失败处理(与原来的失败处理一样,不回收)。
4. 改派 CAS(`luaReassignSceneNode`)。在原有的回执、属主比对与在役核对之后多一条:p 必须比高水位新,否则回 3
   (`reassignStaleGeneration`),一个键不动。落地时同一个脚本把高水位写成 p。
5. 落地:转 Agones 名额;rebalance 还向老节点发 `DestroyScene(scene, p)`。
6. 落空:按 p 回收新节点上预建的实体(§3.2)。resolveScene 的死节点自愈把「重读属主」并进了这一步:回收脚本读到的属主
   就是重读结果(`adoptOwnerAfterLostReassign`)。

### 3.2 回收预建实体

`reclaimUnownedWorldChannelEntity` 先跑 `luaFenceReclaim`,一个脚本里:

- 读属主;
- 频道仍存在(有属主,或仍在在役集合里)时,把高水位抬到 max(高水位, p);
- 属主就是这个节点:不拆,那是属主的活实体;否则调用方发 `DestroyScene(scene, p)`。

两处取舍:

- 属主就是这个节点时也抬高水位。节点上的实体可能正是本次按 p 建的(别的写者以更旧的代次把频道派到了同一个节点,
  它的 CreateScene 复用了本次的实体)。高水位要不低于节点上这份实体的代次,之后迁出时的拆除(代次比高水位新)才拆得到它。
- 频道已经被拆除(没有属主,也不在在役集合)时不写高水位:拆除任务已经删过这把键,再写就留下没人删的垃圾;此后也不会再有
  改派落地(四条路径的 CAS 都要比属主,懒改派还核对在役)。

脚本出错时不发 DestroyScene,结局未知,调用方只记日志(resolveScene 的死节点自愈回错误,让客户端重试)。

### 3.3 init 的补建

initWorldScenesForZone 对每个在役频道向属主补发 CreateScene。属主与高水位用一条 MGET 读,建场景带高水位。高水位的值
损坏时本轮跳过这个频道(记 read_unknown),不按 0 去建:节点若已记下更新的拆除,代次 0 的建场景会被拒。

### 3.4 不带代次的请求

代次 0 = 不带代次:

- 副本、镜像的建场景与销毁:它们不换节点。
- 世界频道的退役拆除:排空期间每拍的 DestroyScene、拆除任务里的 DestroyScene(排空收尾、孤儿清理)。退役开始时频道已
  摘出在役集合,拆除认领再删映射,之后不会再被放置;拆除无条件执行。

### 3.5 拆除任务删高水位

`runWorldDrainTeardown`、`runWorldOrphanTeardown` 删 `scene:{id}:placement_gen`,删不掉记为未完成、留给扫描重做。
认领脚本已经删了映射、摘出了集合,之后没有写者会再写这把键(§3.2 的条件,CAS 的属主比对)。

## 4. scene 节点的规则

纯规则在 `cpp/libs/services/scene/world/scene_placement_fence.h`(`ScenePlacementFence`),实例由 `SceneLifecycle::PlacementFence()`
持有(节点 EventLoop 线程,thread_local)。每个场景一条记录:

- `newestCreate`:放行过的最新建场景代次;
- `destroyedThrough`:放行过的最新拆场景代次;0 = 没按代次拆过。

只有带过代次(≥ 1)的场景才有记录。

| 请求 | 过期条件 | 过期时 | 放行后记下 |
|---|---|---|---|
| CreateScene(g) | 有记录,且 destroyedThrough ≠ 0 且 g ≤ destroyedThrough | 回空应答,不建、不复用 | g ≥ 1 时 newestCreate = max(newestCreate, g) |
| DestroyScene(g ≥ 1) | 有记录,且 newestCreate > g | 忽略,回 OK | destroyedThrough = max(destroyedThrough, g);节点上没有这个实体也记 |
| DestroyScene(0) | 从不过期 | — | 不改记录 |

- 代次 0 的建场景按最旧的代次判(受「已按代次拆过」挡);代次 0 的拆场景与没有代次时完全一样。
- 退役拆除(代次 0)不留「已退役」墓碑。显式 DestroyScene 走副本的销毁路径,拆世界频道时不把它摘出在役集合,
  之后会被懒改派以新的代次复活(既有行为,见 §7);墓碑会让它永远建不回这个节点。代价是退役之后晚到的旧建场景
  可能建出孤儿,由归属对账收。

- 两个入口都判定:gRPC `SceneNodeGrpcImpl::HandleCreateScene` / `HandleDestroyScene`,muduo `SceneHandler::CreateScene` /
  `DestroyScene`。建场景在按 scene_id 去重之前、参数与 Agones 许可的拒绝之后判定,所以「放行」就等于建成或复用;
  过期的建场景不走 `ReuseExistingScene`,不会重算归属对账器的宽限期。拆场景在查找实体之前判定。
- gRPC 建场景先在协程里等 Agones 许可,判定在许可之后的 `HandleCreateScene` 里做:等许可期间到达的拆场景已经记下,
  这次建场景按那时的记录判定。
- 「没有实体也记」:回收的 DestroyScene 可能先于同一次尝试的 CreateScene 被处理(gRPC 建场景要等许可),记下之后
  那次晚到的建场景按 destroyedThrough ≥ g 拒收。
- 记录活到进程退出,不清理。条数 = 本进程生命期内带代次放置到本节点的世界频道数,每条两个 uint64 加哈希表开销。
- 节点自己的归属对账器(docs/design/scene-node-ownership-reconcile.md)在进程内直接调 `SceneLifecycle::DestroyScene`,
  不经这里、也不改记录。

## 5. 为什么这样够了

设频道 S 在节点 N 上按代次 p 合法落地:CAS 在 t_C 提交,高水位 = p。改派者等到建场景应答才提交,所以 N 上 Create(p)
在 t_C 之前已经放行。

**N 仍是属主期间,任何发给 N 的拆场景都拆不到这份实体。** 发给 N 的拆场景只有三种:

1. 回收 Destroy(g)。`luaFenceReclaim` 在 t_R 读到属主 ≠ N,并把高水位抬到 ≥ g。t_R 在 t_C 之后不可能(那时属主是 N,
   读到就不拆);t_R 在 t_C 之前,则 CAS 要求 p > 高水位 ≥ g。所以 g < p。Destroy(g) 若在 Create(p) 之后被处理:
   newestCreate ≥ p > g,忽略;若在之前:拆掉的是更早的那份,随后 Create(p) 因为 p > g ≥ destroyedThrough 放行、重建。
2. 迁出 Destroy(q):只在频道已从 N 迁走(CAS 提交 q)之后发,此后 N 不是属主。频道再迁回 N 要用比高水位新的代次 r > q,
   按第 1 条同样的推理,晚到的 Destroy(q) 拆不到迁回来的实体。
3. 退役 Destroy(0):频道已退役,不会再有放置。

**init 对属主的补建总能放行。** 补建带高水位 h ≥ p。N 上的 destroyedThrough < p ≤ h:代次 ≥ p 的拆场景要么是 N 不是属主时
的回收(那次回收先把高水位抬过它,N 要成为属主必须用更新的代次),要么是迁出(之后 N 不再是属主)。

**晚到的建场景不复活被回收的放置。** 回收 Destroy(g) 处理之后,同一次尝试晚到的 Create(g) 遇到 destroyedThrough ≥ g,拒收。

**退役拆除(代次 0)拆不到合法放置。** 排空与孤儿清理开始时频道已摘出在役集合,拆除认领再删映射,之后四条改派路径的 CAS
都落不了地(要比属主,懒改派还核对在役),频道不会再被放置。例外见 §7 显式销毁。

## 6. 取代 60 秒回收栅栏

11-06 上午的栅栏改动整体去掉:

- `luaReassignSceneNode` 的 KEYS[7] 从 `scene:{id}:reclaim_fence:{newNode}` 改为高水位;返回 3 的含义从「栅栏在」改为「代次过期」;
- `luaFenceReclaim` 不再立带 TTL 的栅栏,改为抬高水位;
- `worldChannelReclaimFenceTTLSeconds`、`sceneReclaimFenceKey` 删除。

行为上,回收之后不再有「60 秒内改派到这个节点一律落空」:新的改派用更新的代次,立刻就能落地。

## 7. 剩余与边界

- **孤儿实体**:建场景超时(本端放弃、请求晚到后又建成)、改派者预建之后崩溃、退役之后晚到的旧建场景,都会在节点上
  留下没有映射指向的实体。它们不影响路由。代次只保证「不拆合法的实体」,不负责收孤儿;孤儿由节点的归属对账器过宽限期后拆掉。
- **显式 DestroyScene 拆世界频道**(既有行为,不在本次范围):它走副本的销毁路径(`destroyInstanceForce`),删映射但不把
  频道摘出在役集合,之后懒改派会以空属主把频道复活到某个节点。那次拆除不带代次;它若晚到、而频道恰好复活在同一个节点上,
  会拆掉复活后的实体。根治要让显式销毁按退役处理(同一步摘出在役集合),或拒绝拆世界频道。
- **控制域 Redis 丢数据**:计数器或高水位回退时,新分配的代次可能不比节点记录的新:建场景被拒、拆场景被忽略
  (fail-closed,不会拆错),计数器涨过去之后恢复。属主映射在同一个实例里,丢数据时本来就一起不可信。
- **混跑**:项目尚未上线(AGENTS.md「上线状态」),不考虑新旧版本混跑。新字段对旧版本是未知字段,按 0 处理,即原来的行为。
- **节点重启**:记录在进程内存里,重启后清空。旧进程连接上的请求不会送到新进程(gRPC 请求绑定连接),不需要跨进程保留。

## 8. 改动位置

| 位置 | 内容 |
|---|---|
| `proto/scene/scene.proto` | `CreateSceneRequest.placement_generation = 6`、`DestroySceneRequest.placement_generation = 2` |
| `go/scene_manager/internal/logic/world_init.go` | 两把键、`allocatePlacementGeneration`、`luaPlacementGenerationAfter`、CAS 与回收脚本、init 补建、init 死节点改派与懒改派 |
| `go/scene_manager/internal/logic/world_rebalance.go` | `migrateWorldChannel` 分配代次,建场景、CAS、拆老实体、回收都带它 |
| `go/scene_manager/internal/logic/enterscenelogic.go` | resolveScene 死节点自愈分配代次;落空后的重读属主并进回收脚本 |
| `go/scene_manager/internal/logic/scene_node_client.go` | `RequestNodeCreateSceneAtGeneration`、`RequestNodeDestroySceneAtGeneration` |
| `go/scene_manager/internal/logic/world_autoscale.go`、`orphan_cleanup.go` | 拆除任务删高水位 |
| `cpp/libs/services/scene/world/scene_placement_fence.h` | 纯规则 |
| `cpp/libs/services/scene/world/scene_lifecycle.{h,cpp}` | `PlacementFence()` |
| `cpp/nodes/scene/handler/grpc/scene_node_service.cpp`、`cpp/nodes/scene/handler/rpc/scene_handler.cpp` | 两个入口的判定 |
| 用例 | `go/.../world_channel_placement_generation_test.go`;`cpp/tests/cross_zone_test/scene_placement_fence_test.cpp` |

## 9. Java 版

不适用,不写 Java 代码,也不登记待办:Java 版的频道换节点是新号新建、旧的排空(`PARITY.md`「场景实例与主世界频道」行 D4),
scene-manager 不向 scene 发 Create / Destroy(D3),没有「按 scene_id 幂等预建、落空回收」这一步,也就没有晚到的回收拆错实体。
