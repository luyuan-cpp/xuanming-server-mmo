# 按人数自动扩缩容:大世界频道 / gate / Scene Node 进程

**日期**: 2026-07-29
**状态**: 已落码。Go 侧 `build` / `vet` / 120 个单测全绿;C++ 侧未编译(需 Codex)。
**没有上过集群。**

## 0. 三个层次,别混成一件事

| 层 | 伸缩什么 | 谁决定 | 依据 |
|---|---|---|---|
| 频道 | 一张大世界地图开几个 Scene(频道) | Go SceneManager `world_autoscale.go` | 频道人数 `instance:{sceneId}:player_count` |
| 进程 | Scene Node Pod 数(Fleet replicas) | Agones FleetAutoscaler(Counter 策略) | 剩余空闲**房间名额**(rooms Counter) |
| 接入 | gate Pod 数 | 运维 + drain 标记 | 在线连接数 |

混成一件事是常见错误:频道多不等于进程多(一个进程能装 N 个频道),
进程多也不等于该开新频道(可能只是副本多)。

## 1. 频道扩缩容

### 1.1 口径

- 人数**按频道**算(`instance:{sceneId}:player_count`),不是按进程。
- **扩容**:该地图**所有**频道人数都 `>= ScaleOutPlayerThreshold`(默认 2000)
  → 期望频道数 +1,并立刻建出来。
  - 要求"所有"而不是"任一":负载不均时(刚扩的新频道是空的、老频道还满着),
    按"任一"会连续触发扩容,扩出一堆空频道。
- **缩容**:某频道人数 `< ScaleInPlayerThreshold`(默认 100)→ 把它排空
  (玩家强制改派到同图其它频道)→ 销毁。
- **每个大世界地图至少保留 1 个频道**。`MinChannelsPerMap` 配成 0 也会被钳回 1 ——
  缩到 0 会让这张图直接进不去。

### 1.2 期望频道数为什么必须落 Redis

`initWorldScenesForZone` 会把"缺的频道"补齐,它在 fullSync 和每次节点 PUT
事件时都跑。如果期望值继续直接读 `ChannelCountFor(confId)` 这个静态配置,
自动缩容刚摘掉的频道**会在几秒内被它重新建出来** —— 缩容根本不成立。

所以期望值下沉到 Redis `world_channels:desired:zone:{z}`(hash confId→N),
配置只作为**首次播种**的种子,扩缩容改的是 Redis 那份,两边共用一个权威。
改配置不会覆盖已经伸缩过的值(否则一次重启就把伸缩结果抹掉);运维想强制
回到配置值,删掉对应 hash field 即可。

### 1.3 缩容的顺序

```
1. SREM world_channels:zone:{z}:{conf}   ← 先摘路由,新玩家不再被分进来
2. 期望频道数 -1                          ← 否则 init 立刻补回来
3. SADD world_channels:draining:zone:...  ← 摘掉之后只有这个索引还知道它存在
4. DestroyScene RPC                       ← C++ 侧:还有人就改派、没人才销毁
5. 下一拍重复 4,直到人走光               ← 收敛,不是重试 hack
6. 清 Redis 状态 + 归还 Agones 房间名额
```

顺序不能反。先排空再摘路由的话,刚被改派出去的玩家有可能又被分回这个正在
销毁的频道。

### 1.4 防抖

两条:

- **冷却窗口**(`CooldownSeconds`,默认 120s):一次伸缩之后该 (zone, map)
  静默。伸缩的效果(玩家重新分布)需要时间体现,不等就会连续误判。
- **容量校验**:把 victim 的人并进其余频道后,不能把任何频道推过扩容线。
  少了这条,缩容把人挤过去立刻触发扩容,扩容又让某个频道掉到线下 ——
  自激振荡,玩家被反复强制改派。

2000 / 100 之间的宽带本身也是防抖的一部分。

## 2. 强制迁移(C++ 侧)

复用整节点疏散那条链路,不新写一套:

```
BeginSceneDrain(sceneEntity)
  → 对场景里每个残留玩家 EnqueueRelocateTicket:
      抄 gate/session 票据(实体销毁前)→ HandleExitGameNode
  → 存盘落地回调 → DispatchEmergencyRelocate
      → SceneManager.EnterScene(scene_id=0, scene_conf_id=0)
      → SceneManager 按世界频道表挑一个**存活节点**上的大世界频道
  → 摘会话 → 销毁本地实体
```

与 `BeginEmergencyRelocateAll` 的区别只有两点:范围是一个场景;**不设**
`tlsEmergencyRelocating`(节点自己不退出,还要继续服务别的场景,标成疏散中
会让 Node 的 drain 看门狗误判)。

### 2.1 DestroyScene 改成 drain-then-destroy

`HandleDestroyScene`(gRPC 与 legacy 两条路径)现在:**场景里还有人就先改派、
并且不销毁实体**,直接返回。

这既是频道缩容需要的原语,也补上了一个真实缺口:在此之前"销毁一个还有人的
场景"行为是未定义的 —— 玩家会留着一个指向已销毁实体的 gate 会话。副本走
不到这个分支(Lua CAS 保证 `player_count==0` 才销毁),但世界频道缩容和镜像
级联销毁都会走到。

返回而不销毁是有意的:改派是异步的(存盘回调才完成),此刻场景不可能是空的。
SceneManager 的 DestroyScene 幂等,autoscaler 下一拍会再调 —— 那时人已经走光,
走正常分支。这是**收敛**,不是重试 hack:每一拍都重新观察真实状态,而不是
记"我做到第几步了"。

## 3. gate 扩缩容

gate 持长连接,直接缩 Pod 会让那台上的玩家全部断线。三步:

1. **标记 draining** —— `gate:{nodeId}:draining`,新玩家不再被分配过来。
   实现:`loginqueue.MarkGateDraining` + `FilterDrainingGates`,接在
   `gateWatcherCapacityProvider.CandidatesForZone`,这是**所有** gate 选择
   路径的唯一收口(队列 dispatcher 与非队列快路径都经过它)。
2. **等在线自然掉**到阈值以下,或等到超时。
3. **改派剩余玩家**到别的 gate,然后才允许缩容。

本轮只实现第 1 步与状态读写。第 2/3 步刻意**没有**做成"到期自动强踢":
什么时候可以牺牲最后那批玩家的连接是运营决策,不该由一个后台循环替人做。

两个刻意的失败方向:

- 查排空标记失败 → **放行**(当作没人在排空)。Redis 抖动时宁可把玩家分到
  一台待缩容的 gate,也不能让所有 gate 被判成不可用、玩家全部进不来。
- 候选集**全部**被标记排空 → 返回原集合并打 ERROR。那是明显的误操作
  (把整个 zone 都标了),拒绝所有登录比分到待缩容 gate 更糟。

排空标记带 TTL,标记的人中途挂了也不会让这部分容量永久蒸发。

## 4. Scene Node 进程扩缩容

`-AgonesAutoscale` 生成基于 rooms Counter 的 `FleetAutoscaler`:

```yaml
policy:
  type: Counter
  counter:
    key: rooms
    bufferSize: <始终保持的空闲房间名额>
    minCapacity / maxCapacity
```

用 Counter 而不是 Buffer 策略:高密度模型下"还剩几个 Ready 的 GameServer"
没有意义(一个进程能装 N 个房间),真正该看的是"还剩几个空闲**房间名额**"。

`-AgonesAutoscale` 必须配 `-AgonesHighDensity`(Counter 策略依赖 rooms
Counter),否则直接报错。

缩容安全性:Agones 缩 Fleet 时挑 Ready(未分配)的 GameServer 下手,
Allocated 的不动。一个进程只要还有 1 个房间就是 Allocated,所以缩容不会踢掉
在玩的房间;空进程被回收是预期行为。

## 5. 配置

```yaml
WorldAutoscale:
  Enabled: false              # 默认关
  CheckIntervalSeconds: 30
  ScaleOutPlayerThreshold: 2000
  ScaleInPlayerThreshold: 100
  MinChannelsPerMap: 1        # 硬下限 1,配 0 也会被钳回
  MaxChannelsPerMap: 16       # 0 = 不限(不推荐)
  CooldownSeconds: 120
  DrainTimeoutSeconds: 300
```

部署侧:`-AgonesAutoscale -AgonesBufferRooms N -AgonesMinReplicas N -AgonesMaxReplicas N`。

## 6. 指标

```
scene_manager_world_autoscale_total{zone_id,action,outcome}
    action  = scale_out | scale_in
    outcome = ok | drained | error | max_reached
```

`max_reached` 持续出现 = 该地图已经顶到频道上限还在满员,需要人工介入。

## 7. 明确不成立的说法

- **强制迁移不是无感的**。玩家会经历一次"退出场景 → 重新进场"(存盘 + 重新
  加载),客户端会看到一次加载。这与身份冲突疏散的体验完全一致 —— 复用同一条
  链路是有意的,不是凑合。
- **本轮没有做 gate 的自动缩容执行器**。只有"不再分配新玩家"这一步。
- **单测覆盖的是 SceneManager 的决策与状态机**,不是"玩家真的被改派到了另一个
  频道"。后者必须由 dev 集群 E2E 验证。
- 频道扩容依赖 `initWorldScenesForZone` 能选到 world-hosting 节点;
  `StrictNodeTypeSeparation=true` 且 world 池为空时扩容会失败(记 ERROR)。

## 8. dev 集群验收(由人执行)

1. 单频道人数推到 2000 以上 → 观察 `world_channels:desired:zone:{z}` +1,
   新频道被建出来。
2. 只让一个频道满、另一个留空 → **不应**扩容。
3. 把某频道人数降到 100 以下 → 观察它被 SREM、进 draining 集合、
   里面的玩家出现在别的频道。
4. 缩到只剩 1 个频道 → 再空也不再缩。
5. 标记一台 gate draining → 新登录不再落到它;清标记后恢复。
6. 开 FleetAutoscaler,把房间填满 → Fleet replicas 增长;房间释放 → 回落,
   且**没有**在玩的房间被踢。

## 9. 文件索引

| 文件 | 作用 |
|---|---|
| `go/scene_manager/internal/logic/world_autoscale.go` | 频道扩缩容决策 + 排空收敛 |
| `go/scene_manager/internal/logic/world_init.go` | 期望频道数改读 Redis;`SetWorldConfIdsForTest` 测试缝 |
| `go/scene_manager/internal/logic/world_autoscale_test.go` | 13 个单测 |
| `cpp/libs/services/scene/player/system/player_lifecycle.{h,cpp}` | `BeginSceneDrain` / `EnqueueRelocateTicket` |
| `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` | DestroyScene 改 drain-then-destroy |
| `cpp/nodes/scene/handler/rpc/scene_handler.cpp` | 同上(legacy 路径) |
| `go/login/internal/logic/pkg/loginqueue/gatedrain.go` | gate 排空标记 + 候选过滤 |
| `go/login/internal/svc/servicecontext.go` | 过滤接在候选集唯一收口 |
| `tools/scripts/k8s_deploy.ps1` | `New-SceneFleetAutoscalerYaml` / `-AgonesAutoscale` |
