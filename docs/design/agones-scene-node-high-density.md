# Agones 管理 Scene Node(高密度 GameServer)

**日期**: 2026-07-29
**状态**: 阶段 A 已完成;阶段 B 已完成 Windows `Debug|x64` 全 solution 编译与
24 个 C++ 单测验证;阶段 C 已落码并通过 Go 侧 `build` / `vet` / 107 个单测
(其中 20 个为阶段 C 新增)。**三个阶段都没有上过集群**,也没有装过 Agones。

> **2026-09-29 更新(集群外入口 D81–D85)**:C++ 生命周期代码已从 `cpp/nodes/scene/agones/` 下沉到 `cpp/libs/engine/infra/agones/` 并泛化,
> 类名改为 `GameServerLifecycle` / `AllocationPermit`,battle 也接入(Fleet `portPolicy: Dynamic`、排空标签、EventLoop 心跳)。
> 下文凡提到旧路径 / 旧类名的地方均按 §12 的对照表理解;scene 只做了机械改名,行为不变。**本轮改动未编译、未测试、未上集群,待 Codex 验证。**

## 1. 为什么是 1 GameServer = 1 Scene Node,而不是 1 Scene

```
1 Agones GameServer
    = 1 Scene Node Pod / 1 个 C++ 进程
    = N 个动态创建的 ECS Scene 房间
```

不是「一个 Scene 一个 GameServer」。理由:

1. **Scene 是进程内的 entt 实体,不是进程**。`CreateScene` 只是
   `sceneRegistry.create()` + 挂 `SceneInfoComp`
   (`cpp/nodes/scene/handler/grpc/scene_node_service.cpp`)。让 Agones 为每个
   实体拉一个 Pod,等于把微秒级操作换成秒级调度。
2. **镜像 Scene 必须与源 Scene 共置**。`docs/design/scene-creation-architecture.md`
   的 "Mirror co-location" 明确要求镜像复用源节点上已驻留的地图静态数据 /
   AOI 网格 / AI 行为树。一 Scene 一 Pod 直接废掉这个优化。
3. **Scene 的编排规则是游戏规则,不是基础设施规则**。主世界频道数、副本闲置
   回收、按 zone 路由、负载分数都在 Go SceneManager 里,Agones 没有也不该有
   这些概念。
4. Agones 官方就为这种形态提供了 high-density GameServer 模式。

职责边界保持不变:

```
SceneManager (Go)                 Agones                       C++ Scene Node
├─ 按规则选 Scene Node            ├─ 管理 Pod 生命周期          ├─ 一个进程承载多个 ECS Scene
├─ 分配 scene_id                  ├─ Ready/Allocated/           ├─ CreateScene 建实体
├─ 创建/销毁 Scene                │  Unhealthy/Shutdown         ├─ DestroyScene 销毁实体
├─ 维护 Redis 路由状态            ├─ 替换故障进程                ├─ Agones Health 心跳
└─ (阶段 C) GSA 预占容量          └─ (阶段 C) rooms 容量        └─ 按实际 Scene 数切 Ready/Allocated
```

## 2. 两类 Fleet:world 与 instance

第一版**同时**把 `scene-world` 和 `scene-instance` 都换成 Fleet,不能只换
instance。原因是镜像 Scene 会**有意绕过**节点角色过滤与源 Scene 共置
(`pickInstanceNode` 的 mirror 分支)。如果 world 还是普通 Deployment,
镜像房间就会落到不受 Agones 管理的 world 进程上,容量统计出现两套语义 ——
阶段 C 的 `rooms` Counter 会从第一天就是错的。

| Fleet | SCENE_NODE_TYPE | 承载 | 副本策略(第一版) |
|---|---|---|---|
| `scene-world` | 0 | 常驻主世界频道 + 与之共置的镜像 | 固定副本,不自动缩容 |
| `scene-instance` | 1 | 按需副本 / 战场,一个进程多个实例 | 固定副本 |

Gate / Login / SceneManager 等继续用普通 Deployment。

副本数与 legacy `scene: N` 的兼容规则见
`docs/ops/scene-node-role-split.md` §3.2.1(唯一权威),实现在
`tools/scripts/k8s_deploy.ps1::Resolve-SceneDeploymentPlan`。Fleet 和
Deployment 共用同一份 plan,只是渲染成不同的 kind。

## 3. 为什么内部端口用 `portPolicy: None`

Scene Node 是**内部服务**:对端是 Gate 和 SceneManager,不是玩家客户端。
玩家的 UDP/TCP 入口在 Gate,不在 Scene。所以本阶段不需要 UDP LB、HostPort、
NodePort 或任何公网地址,GameServer 端口用:

```yaml
ports:
  - name: rpc
    portPolicy: None
    containerPort: 19000
    protocol: TCP
```

(2026-09-29 注:示例里的 `19000` 是旧值;scene 的 TCP 端口早已改为 20000(19000 落在 gate 的 10000–19999 区间),生成器写的是 `containerPort: $RpcPort`
(`tools/scripts/k8s_deploy.ps1:2209-2213`)。`portPolicy: None` 只适用于 scene 这类内部服务;battle 是客户端面,用 `portPolicy: Dynamic`,见 §12.3。)

SceneManager 继续走原有发现链路:C++ 节点把自己的 `Endpoint` 注册进 etcd
(`SceneNodeService.rpc/` 前缀),`scene_node_client.go` 从 etcd 拿 PodIP:port
建 gRPC 连接。Agones 不参与寻址。

## 4. Ready / Allocated 状态机

实现:`cpp/nodes/scene/agones/agones_scene_lifecycle.{h,cpp}`。
(2026-09-29 已被取代:现为 `cpp/libs/engine/infra/agones/agones_gameserver_lifecycle.{h,cpp}` 的 `agones::GameServerLifecycle`;
本节的 `CreatePermit` / `AcquireCreatePermit*` / `activeScenes` 分别对应 `AllocationPermit` / `AcquireAllocationPermit*` / 活跃单元,见 §12.1。)

```
Disabled          非 Agones 构建 / 环境,全程不发 HTTP,所有 gate 直接放行
   |
Starting          RPC server + 依赖初始化 + etcd 注册完成后才开始
   |  POST /ready 有上限退避重试(默认 30 次,200ms→2s)
   v
Ready             当前实际 Scene 数 == 0
   |  POST /allocate  —— 在第一个 Scene **创建之前**
   v
Allocating -> Allocated    当前实际 Scene 数 > 0 或仍有 CreateScene 在途 permit
   |  POST /ready     —— 在最后一个 Scene **销毁之后**
   v
ReturningToReady      /ready 在途;新创建不得复用旧的 Allocated 判断
   v
Ready
```

### 4.1 两条硬约束

**(a) 不能先建房间再异步 allocate。**
`SceneNodeGrpcImpl::CreateScene` 必须先拿到 Allocated 许可才进 `HandleCreateScene`,
拿不到就直接返回 `UNAVAILABLE "agones allocate not confirmed"`,一个实体都不创建。`CreateSceneResponse`
里没有错误字段(`proto/scene/scene.proto`),非 OK 的 gRPC 状态是唯一能让
SceneManager 知道"这次失败了、该回滚"的诚实手段。

> 2026-10-16(第 4 批,代码未编译,待 Codex 验证):gRPC 服务端改为跑在 EventLoop 上的 `muduo_grpc::Server`,
> 许可改在守护段 `grpc.CreateScene.guard` 里用 `AcquireAllocationPermitAsync(loop, cb)` 取,结果经 `queueInLoop`
> 回到 loop 后才 `Proceed()`;「先许可、后建实体」这条约束不变(见 [grpc-muduo-transport.md](grpc-muduo-transport.md)
> §15.8.4、§15.9)。第 4 批之前的写法是在 gRPC 线程上、`runInLoop` **之前**调 `AcquireCreatePermitBlocking()`
> (2026-09-29 改名为 `AcquireAllocationPermitBlocking`)。

**(b) HTTP 绝不进 muduo EventLoop。**
`docs/design/scene-node-threading-model.md` 的决策是单线程 EventLoop 跑
RPC + Kafka + `World::Update()`(33ms 帧)。一次 HTTP 抖动进去就是整帧卡住。
因此:

| 调用方 | 所在线程 | 用哪个 gate |
|---|---|---|
| `SceneNodeGrpcImpl::CreateScene`(gRPC 守护段) | **EventLoop**(第 4 批起;此前是 gRPC 线程池) | `AcquireAllocationPermitAsync(loop, cb)`,有界等待(默认 3s),结果一律在该 loop 线程上回调(当场判定与 worker 裁决经 `queueInLoop` 投递,等待超时由该 loop 的定时器就地裁决为拒绝);此前是 `AcquireCreatePermitBlocking()` |
| `SceneHandler::CreateScene`(legacy muduo RPC) | **EventLoop** | `AcquireAllocationPermitNonBlocking()`(旧名 `AcquireCreatePermitNonBlocking`),立刻返回 false + 踢一次异步 allocate |

所有 HTTP 都在 lifecycle worker 线程上跑。EventLoop 上的两条路径都不等待:gRPC 守护段登记异步等待者后立即返回,
RPC0 路径当场拒绝。(第 4 批之前 gRPC 线程阻塞在条件变量上,不是 socket。)`AcquireAllocationPermitBlocking`
保留,只给非 EventLoop 线程和测试用。

这不是"用定时器掩盖时序问题":到期后不假设成功,而是**拒绝这次创建**让调用方
重查权威后重试 —— 有界、有出口、fail-closed。

### 4.2 房间计数的唯一来源

计数只在场景域的 `SceneLifecycle::CreateScene` / `DestroyScene`
(`cpp/libs/services/scene/world/scene_lifecycle.cpp`) 中直接调用 `OnUnitCreated` / `OnUnitDestroyed`。

gRPC 与 muduo RPC 入口都先做各自校验和幂等查找，再委托同一场景生命周期函数；
只有实体实际创建、或完成住户排空并实际销毁时才记账。必需记账不依赖 dispatcher 订阅，因此满足：

- 重复 CreateScene 不重复计数
- Destroy 不存在的 Scene 不减计数
- 创建失败不增加计数

`GameServerLifecycle` 内部再用 `unordered_set<uint64_t>` 按 key 幂等一次,所以即使
将来有第三条路径重复调用计数接口,计数也不会错、更不会为负。所有计数操作在同一把
互斥锁下,可从任意线程调用。

### 4.3 在途创建 permit 与零房间收口

gate 成功后到 EventLoop 真正完成实体创建之间存在排队窗口。两个 CreateScene
入口都持有 RAII `CreatePermit`:最后一个旧 Scene 销毁时,只有
`activeScenes == 0 && pendingCreates == 0` 才允许回 Ready。permit 析构时如果
仍是 Allocated 且没有实体,会自动请求回 Ready,覆盖参数非法/幂等命中等
"allocate 成功但没建出实体"的情况。

worker 真正发 `/ready` 前先把本地状态切成 `ReturningToReady`;此时新创建不能
复用旧的 Allocated 判断。阻塞路径与异步路径(第 4 批起 gRPC 守护段所用)等待 `/ready -> /allocate` 完成,非阻塞路径
立即 fail-closed 并预挂一次 allocate。这样 sidecar 与本地状态不会因 HTTP
在途竞态而分叉。

## 5. libcurl:选型与边界

HTTP 客户端定为 **libcurl**,不再引入 cpp-httplib / CPR / Boost.Beast,也不把
curl 源码复制进 `third_party/`。

依据:同类库社区规模最大、生产成熟度与错误处理更可靠、构建镜像已经装了
`libcurl4-openssl-dev`、Agones REST 只需要少量 GET/POST、避免再增加 vendored
第三方依赖。

边界:

- Linux 构建链接**系统** `CURL::libcurl`(`cpp/nodes/scene/CMakeLists.txt`
  的 `find_package(CURL REQUIRED)`),并定义 `MMORPG_AGONES_CURL`。
- Windows / 本地开发构建不定义该宏,`MakeDefaultHttpTransport()` 返回
  `nullptr`,整套逻辑退化成 `Disabled` —— 本机开发不需要装 Agones。
- 所有 curl 调用封死在 `CurlAgonesHttpTransport` 内,业务代码里不出现
  `CURL*` / `curl_easy_setopt`。
- 用同步 easy API,且**只在 lifecycle worker 线程**跑。
- 连接超时与总超时都设(`CURLOPT_CONNECTTIMEOUT_MS` + `CURLOPT_TIMEOUT_MS`),
  只设总超时的话连接阶段挂死照样吃满预算。
- `curl_global_init` 用 `std::call_once`;刻意不调 `curl_global_cleanup`。
- **不使用** `third_party/curl` 子模块(当前未初始化)。
- `deploy/k8s/Dockerfile.runtime` 增加 `libcurl4` 运行时库。

## 6. 功能开关与回退

| 开关 | 位置 | 默认 | 作用 |
|---|---|---|---|
| `-SceneOrchestrator deployment\|agones` | `k8s_deploy.ps1` / `dev_tools.ps1` / `k8s_image.ps1` | `deployment` | 生成 Deployment 还是 Fleet |
| `AGONES_ENABLED` | Pod env | Fleet 模板里为 `1` | 设 `0` 可在不换镜像的前提下关掉整套生命周期 |
| `AGONES_SDK_HTTP_PORT` | Agones 注入 | 无 | 缺失即 Disabled(本地开发的默认情况) |

**刻意不做**"检测集群装没装 Agones 就自动切换":自动切换会让同一条命令在两个
集群上产出不同 kind 的工作负载,出事时无法从命令还原现场。

回退路径:把 `-SceneOrchestrator` 改回 `deployment` 重新 apply,然后删掉
Fleet。注意 `kubectl apply` **不会**跨 kind 回收同名对象,Deployment 与 Fleet
必须手动删掉另一个,否则同一个 zone 会有两套 scene 进程、两套容量语义 ——
脚本在 agones 模式下会打印该执行的删除命令。

## 7. 明确不成立的说法

- **world Fleet 的滚动更新不是无缝的**。没有实现在线世界迁移;
  `RebalanceWorldChannelsForZone` 的 opportunistic 迁移只搬
  `player_count == 0` 的频道。活动中的 world 节点更新必须走维护/排空/停服。
- **Agones 只替换进程,不恢复内存中的房间状态**。GameServer 被判 Unhealthy 后
  Agones 拉起的是一个空进程;Redis 里的 Scene 映射要靠 SceneManager 的
  `reconcileDeadNodeScenes` 收拾。玩家会被断开。
- 阶段 B 只做固定副本,**没有** FleetAutoscaler、**没有** Counters/Lists、
  SceneManager **没有**调用 GameServerAllocation。阶段 C 加上了后两者,
  但 **FleetAutoscaler 仍然没有** —— 副本数还是固定的,扩缩容靠人。
- 阶段 C 的所有代码路径都**没有在真集群上跑过**。Go 单测用的是 fake
  Allocator,证明的是"上层逻辑在给定 Agones 行为下是对的",不是"Agones
  真的这么行为"。见 §8.9。

## 8. 阶段 C:rooms Counter 与 GameServerAllocation(已落码,未上集群)

### 8.1 开关

| 配置 | 默认 | 含义 |
|---|---|---|
| `Agones.Enabled` | `false` | 关:选节点完全走原来的 Redis 负载分数,不碰 Agones |
| `Agones.HighDensityEnabled` | `false` | 关:走 GSA 但不用 Counters(一个进程一次一个房间) |
| `Agones.RoomCapacity` | `0` | 仅作配置留痕,运行时容量以 GameServer 上的 `capacity` 为准 |
| `-AgonesHighDensity` + `-AgonesRoomCapacity N` | 关 | 部署生成器给 Fleet 挂 `counters.rooms` |

Counters and Lists 在 Agones 里是 **beta**,需要集群侧显式打开
`CountsAndLists` FeatureGate,所以默认关。

`-AgonesHighDensity` 不给 `-AgonesRoomCapacity` 会**直接报错**,不替你猜一个
数字:C++ Scene Node 是单 EventLoop,每进程房间容量必须按帧耗时、AOI、
玩家数和内存压测确定(压测口径见 `CLAUDE.md` §6)。

### 8.2 创建 Scene 的原子边界

`createInstance` 在 Agones 模式下的顺序是刻意的:

```
1. GSA:选 GameServer + rooms 原子 +1        （选中与计数是同一个操作,不留超卖窗口）
2. status.state 必须是 Allocated
3. 取 gameServerName / PodIP / counter 结果
4. PodIP -> knownNodes 反查 nodeID
5. 校验 zone 与 role                        （Fleet label 与 C++ SCENE_NODE_TYPE 是两条独立链路）
6. 分配 scene_id
7. 写 Redis scene 状态 + scene:{id}:agones_gs
8. 调 C++ CreateScene
9. 只有 8 成功才向调用方返回成功
```

4/5 任一步失败都会把刚预占的名额还回去并拒绝请求。**不允许**"找不到就放行":
那等于绕过容量约束。

PodIP 反查必须唯一命中一条 `(zoneId,nodeId)` 注册；同一身份有重复 etcd
记录时也按映射失败处理，不能任取一个同号进程。gRPC 连接缓存使用同一复合身份。

`scene:{id}:agones_gs` 必须在第 8 步**之前**写。放到 RPC 成功之后写,进程正好
在中间崩溃就永远找不回该减哪个 GameServer 的计数。

### 8.3 修掉的既有 bug:phantom scene

改之前,`RequestNodeCreateSceneWithOptions` 失败只会打一条
`(Redis state committed)` 然后**照样返回成功**。结果是 Redis 里有映射、节点上
没有实体,玩家被路由进去后 `EnterScene` 永远成功不了。

现在失败一律回滚(Redis scene 全部键 + `node:zone:{zoneId}:{nodeId}:scene_count` + 反向索引 +
Agones 名额)并返回错误码。**这一条与 Agones 无关**,非 Agones 模式同样生效。

代价:本包里原先"创建一个场景再断言点什么"的单测都依赖 RPC 失败被忽略,
现在需要一个能应答的 fake 节点 —— 已在 `newTestSvcCtxWithWorldScenes` 里默认装上。

### 8.4 镜像共置在 Agones 下怎么保住

镜像必须与源 Scene 同进程(复用已驻留的地图/AI/spawn),这条业务约束优先于
"让 Agones 自由挑进程"。GSA 只能按标签选、不能点名,所以共置路径走的是
`AcquireRoomOnGameServer`:`nodeID -> PodIP -> GameServer 名字 -> 对该
GameServer 的 rooms 做带 CAS 的 +1`。

源节点满了 / 反查不到(老版本 Agones 的 GameServer status 里没有 Pod 地址)
时,回落到自由分配 —— 镜像失去共置优化但玩家不会卡死。

### 8.5 回滚与漂移

- 销毁:`scene:{id}:agones_gs` 与其余 scene 状态在**同一个 Lua 脚本**里读出并
  删除,脚本把它返回给调用方去精确归还名额。先删后读会永久丢失回滚依据。
- 重复 Destroy:第二次在脚本里读不到 `scene:{id}:node`,拿不到 gs 名字,
  不会重复减。
- 节点死亡:GameServer 可能还在(Agones 还没判 Unhealthy),名额照样要还。
  `ReleaseRoom` 对"GameServer 已消失"返回成功,所以无脑调用是安全的。
- 归还最终失败:记
  `scene_manager_agones_counter_rollback_total{outcome="failed"}` + ERROR 日志。
  **这类漂移不会自愈**,必须对这个指标配告警。

### 8.6 reconcile 只告警,不自动改

`StartAgonesReconcile` 周期性比对 Agones `rooms.count` 与 Redis
`node:zone:{zoneId}:{nodeId}:scene_count`,把差异写进 `scene_manager_agones_counter_drift{zone}`。

**第一版不自动改写任何一方。** 三方任意一方都可能是错的那个;在证据不完整时
自动"修正"很可能把对的一方改错,还会掩盖真正的 bug。先让漂移可见。

### 8.7 指标

```
scene_manager_agones_allocation_total{zone,role,outcome}      ok|no_capacity|error|mapping_failed
scene_manager_agones_allocation_latency_seconds{zone,role}
scene_manager_agones_counter_rollback_total{outcome}          ok|failed
scene_manager_agones_mapping_failure_total{zone,reason}        no_pod_ip|unknown_pod_ip|zone_mismatch|role_mismatch
scene_manager_agones_counter_drift{zone}
```

标签全是低基数维度。`scene_id` / `player_id` 绝不进 label(`CLAUDE.md §9`)。

### 8.8 依赖与权限

不引入 `agones.dev/agones` Go module(它会把 Agones 自己的 `k8s.io/*` 版本拖
进来,和本仓库的 client-go v0.29.3 打架)。只用已有的 `k8s.io/client-go`
dynamic/unstructured 客户端按 GVR 操作。

RBAC 见 `deploy/k8s/manifests/go-svc/scene-manager-agones-rbac.yaml`:
全部是 namespace 级 Role,没有 cluster-admin,没有 ClusterRole。

### 8.9 尚未验证的假设(上集群前必须确认)

这些是按 Agones 文档写的,**没有跑过真集群**:

1. `GameServerAllocation` 的 `spec.selectors[].counters.rooms.minAvailable`
   与 `spec.counters.rooms.{action:Increment, amount}` 字段形状。
2. `status.counters.rooms.{count,capacity}` 在 GSA 返回对象里的位置。
3. 通过 `gameservers/status` 子资源直接改 `counters.rooms.count` 是否被
   Agones 接受(还是必须走 SDK)。这一条影响回滚与共置预占两条路径。
4. `status.addresses` 里 `type: "Pod"` 条目的可用性(老版本没有,代码会退化
   成 GET 同名 Pod)。

任何一条对不上,改的都是 `internal/agones/k8s_allocator.go` 一个文件 ——
接口 `Allocator` 与上层逻辑不受影响,单测也不用改。

## 9. dev 集群验收流程(由人执行,脚本不连集群)

前提:dev 集群已安装 Agones,且版本与 `agones.dev/v1` Fleet + `portPolicy: None`
兼容(由人确认)。

1. 两个 Fleet 都能 Ready:`kubectl -n <ns> get fleet`
2. world/instance 注册的节点类型分别正确:
   `redis-cli GET node:zone:<zoneId>:<nodeId>:scene_node_type` 期望 `0` / `1`
3. 同一个 Scene Node 内能创建至少两个 Scene
4. 第一个 Scene:GameServer `Ready -> Allocated`
5. 第二个 Scene:仍是同一个 Allocated GameServer,**不新建 Pod**
6. 销毁其中一个:仍为 Allocated
7. 销毁最后一个:转回 Ready
8. 杀掉 Scene Pod:Agones 创建替代进程
9. etcd/Redis 节点死亡处理能清理孤儿实例
10. world 节点死亡只证明替换和重建,**不宣称**玩家无感恢复
11. 没有 HostPort / NodePort / LB:`kubectl -n <ns> get svc,gs -o wide`
12. 镜像 Scene 仍与源 Scene 共置
13. 镜像、Pod、二进制版本、日志可对应到确切构建产物
    (`mmorpg.io/build` label 取自镜像 tag)

## 10. 文件索引

| 文件 | 作用 |
|---|---|
| `cpp/nodes/scene/agones/agones_rest_client.{h,cpp}` | 可注入 HTTP 传输层 + libcurl 实现 + Agones REST 端点封装 + 环境变量解析 —— **2026-09-29 已迁到 `cpp/libs/engine/infra/agones/agones_rest_client.{h,cpp}`,旧文件已删** |
| `cpp/nodes/scene/agones/agones_scene_lifecycle.{h,cpp}` | 状态机、lifecycle worker、房间计数、allocate 前置门 —— **2026-09-29 已迁为 `cpp/libs/engine/infra/agones/agones_gameserver_lifecycle.{h,cpp}`(`GameServerLifecycle`),旧文件已删;新增文件见 §12.6** |
| `cpp/libs/services/scene/world/scene_lifecycle.cpp` | 场景创建 / 销毁与房间计数唯一接入点 |
| `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` | gRPC 路径的 allocate 门 + idle 收口(2026-10-16 第 4 批起改为守护段 `grpc.CreateScene.guard` 里的异步许可,不再阻塞) |
| `cpp/nodes/scene/handler/rpc/scene_handler.cpp` | legacy muduo RPC 路径的非阻塞 allocate 门 |
| `cpp/nodes/scene/main.cpp` | `SetAfterStart` 起生命周期,`SetBeforeShutdown` + loop 退出后停并 join |
| `cpp/tests/agones_lifecycle_test/` | 24 个状态机单测(含在途 permit/回 Ready/停止态竞态;注入 fake transport,不需要 sidecar) —— 2026-09-29 磁盘上 `TEST(` 共 51 个(工程名不变),见 §12.7 |
| `tools/scripts/k8s_deploy.ps1` | `New-SceneFleetYaml` / `Resolve-SceneDeploymentPlan` / `-SceneOrchestrator` / `-AgonesHighDensity` |
| `deploy/k8s/Dockerfile.runtime` | 运行时增加 `libcurl4` |
| `docs/ops/scene-node-role-split.md` | 角色拆分运维手册 + 兼容规则权威表 |
| `go/scene_manager/internal/agones/allocator.go` | `Allocator` 接口 + 类型(测试注入 fake 的唯一缝) |
| `go/scene_manager/internal/agones/k8s_allocator.go` | dynamic client 实现:GSA、counter CAS、PodIP 解析、list |
| `go/scene_manager/internal/logic/agones_binding.go` | 预占+映射+zone/role 校验+精确回滚;`scene:{id}:agones_gs` |
| `go/scene_manager/internal/logic/agones_reconcile.go` | 周期性漂移比对(只告警) |
| `go/scene_manager/internal/logic/createscenelogic.go` | 创建原子边界 + RPC 失败回滚(修 phantom scene) |
| `go/scene_manager/internal/logic/scene_atomic.go` | 销毁脚本一并读出并删除 `agones_gs` |
| `go/scene_manager/internal/logic/agones_test.go` | 20 个 Go 单测,fake allocator,不依赖 K8s |
| `deploy/k8s/manifests/go-svc/scene-manager-agones-rbac.yaml` | 最小权限 Role/RoleBinding/SA |

## 11. Windows Debug 构建闭环

`Debug|x64` 必须使用同一套 `/MDd` 第三方产物。2026-10-26 起官方 gRPC SDK
退出构建，BoringSSL 使用独立固定源码及 `third_party/boringssl/install_vs2026_dbg`，
Protobuf / Abseil / utf8 使用 `third_party/protobuf/install_vs2026_dbg`；头文件与库必须同批，
不能把系统 OpenSSL 的配置头与 BoringSSL 静态库混用。

依赖版本保持迁移前提交，具体库清单以当前独立构建的安装产物为准。
zlib、hiredis、yaml-cpp 也必须按 `/MDd` 构建；Debug 的 `/WHOLEARCHIVE` 必须指向
`hiredisd.lib` / `yaml-cppd.lib`。

2026-07-28 本地验证:

- `msbuild game.sln /m /p:Configuration=Debug /p:Platform=x64`:成功。
- `bin/gate.exe` 与 `bin/scene.exe`:成功生成并由各节点 PostBuildEvent 复制。
- `agones_lifecycle_test.exe --gtest_brief=1`:24/24 PASS。
- 未连接 dev 集群、未安装 Agones、未构建运行时镜像。

## 12. 2026-09-29:代码下沉 infra、泛化为 GameServerLifecycle,battle 接入(集群外入口 D81–D85)

决策原文见 `docs/design/k8s-client-entry.md`(D76–D93,battle 相关为 D81–D86 与「battle Fleet health 预算与就绪判据」)。本节只记与本文相关的代码事实。**全部未编译、未测试、未上集群,待 Codex 验证。**

### 12.1 位置与改名(D85,scene 只做机械改名,不复制第二份状态机)

| 旧(本文 §4–§10 的写法) | 新 |
|---|---|
| `cpp/nodes/scene/agones/agones_rest_client.{h,cpp}` | `cpp/libs/engine/infra/agones/agones_rest_client.{h,cpp}` |
| `cpp/nodes/scene/agones/agones_scene_lifecycle.{h,cpp}`,类 `SceneLifecycle` | `cpp/libs/engine/infra/agones/agones_gameserver_lifecycle.{h,cpp}`,类 `agones::GameServerLifecycle` |
| `CreatePermit` / `AcquireCreatePermitBlocking` / `AcquireCreatePermitNonBlocking` | `AllocationPermit` / `AcquireAllocationPermitBlocking` / `AcquireAllocationPermitNonBlocking`(`agones_gameserver_lifecycle.h:123-169`);2026-10-16 第 4 批新增 `AcquireAllocationPermitAsync`,EventLoop 上的 gRPC 守护段改用它,Blocking 只给非 EventLoop 线程与测试 |
| `OnSceneCreated` / `OnSceneDestroyed` / `SceneCount` | `OnUnitCreated` / `OnUnitDestroyed` / `UnitCount`(`:176-194`;单元 = scene 的房间实体或 battle 的 battle_id) |
| (无) | 新增 `agones_gameserver_status.{h,cpp}`(`ParseGameServer` / `ParseGameServerLabels`,用 protobuf `Struct` 解析,兼容 `object_meta` 与 `objectMeta`)、`agones_client_endpoint_source.{h,cpp}`(Agones 地址来源) |

- 旧目录与 4 个旧文件已删除(未用 `git mv`,新建 + 删除,构建清单同步);构建登记改在 `infra.vcxproj` / `infra.vcxproj.filters` / infra `CMakeLists.txt`,
  scene 的 vcxproj 与 CMake 移除了旧条目。`AllocationPermit` 持有 `GameServerLifecycle&` 引用(不再是裸指针成员)。
- scene 调用点只改名:`scene_node_service.cpp:251` 阻塞许可、`scene_handler.cpp:869` 非阻塞许可、单元计数现收口到 `world/scene_lifecycle.cpp` 的创建 / 销毁入口、
  `scene/main.cpp:286-291` 以默认 `LifecycleOptions{}` 启动。
- libcurl 边界不变(§5):infra 与 battle 的 Linux 构建定义 `MMORPG_AGONES_CURL` 并链接系统 curl;Windows 不定义,`MakeDefaultHttpTransport()` 返回 nullptr → `StartDisabled`。

### 12.2 `LifecycleOptions` 新增项(默认值即 scene 行为)

- `requireLoopHeartbeat=false`、`loopStaleAfter=10s`:开启后 EventLoop 须定时 `TouchLoopHeartbeat()`,超过阈值没更新 worker 就停发 `/health`,由 Agones 判 Unhealthy 替换(D84)。
- `drainLabelKey`(空 = 不启用)、`drainPollInterval=5s`:开启后 worker 每 5s `GET /gameserver` 读自己的 labels,有标签即排空(D83)。
  SDK 的 `SetLabel` 会自动加 `agones.dev/sdk-` 前缀,所以排空标签只能由 Fleet `allocationOverflow` 或运维 `kubectl label` 打上,进程只读。
- `healthInterval=2s`、`allocateWaitTimeout=3s` 沿用原值(`agones_gameserver_lifecycle.h:75-106`)。

### 12.3 battle 与 scene 的差异

| 项 | scene | battle |
|---|---|---|
| 端口 | `portPolicy: None`,内部服务,SceneManager 从 etcd 拿 PodIP | `name: client`、`portPolicy: Dynamic`、containerPort 20000(`tools/scripts/lib/k8s_client_entry.ps1:1054-1058`);gRPC 50000 只写容器 ports,永不进 Agones ports / hostPort |
| 客户端地址 | 不涉及 | external 时 `CLIENT_ENDPOINT_SOURCE=agones`:构造期经 THooks `ClientEndpointSourceFactory`(`battle/main.cpp:228-232`)读 sidecar `status.address` + `status.ports[client]`,在发布 etcd 之前自报 `client_endpoint`;podip 时 `none` |
| 排空标签 | 不开 | `mmorpg.io/drain`(`battle/main.cpp:45` `kAgonesDrainLabelKey`),Fleet `allocationOverflow.labels` 同值 |
| EventLoop 心跳 | 不开(D84 后续项) | 开:`requireLoopHeartbeat=true`,EventLoop 每 1s `TouchLoopHeartbeat()`(`battle/main.cpp:85-86`、`:319`) |
| 单元 | scene 实体（`SceneLifecycle` 直接记账） | battle_id:所有插表 / 删表路径收口到 `BattleRoomManager::EmplaceRoom` / `EraseRoom`(房间表 `battle_room_table.h`),回调转发 `OnUnitCreated/OnUnitDestroyed`(`battle/main.cpp:279-281`) |
| 许可拒绝的出口 | gRPC `UNAVAILABLE`(§4.1) | gRPC `UNAVAILABLE "battle_not_allocatable"`(`battle_node.cpp:15-19`);match 遇到它不发 DestroyBattle、换一个没试过的节点重试一次(D82) |
| 进程级准入闸 | 无 | `battle_admission_gate.h`:SetAfterStart 完成前、停机开始后的 CreateBattle 也回 `battle_not_allocatable`(启动窗口与停机窗口) |
| 停机 | Stop 并 join | CloseAdmission → BeginRoomShutdown → DisconnectAll → lifecycle `Stop()`,**不调** `/shutdown`(`battle/main.cpp:342-367`);单元归零可能让 worker 在 Stop 前发一次 `/ready`,Stop 经取消令牌中止它(以及在途的 `/allocate`、`/health`、`GET /gameserver` 等可放弃请求),不等满超时,通常毫秒级、最坏约 1s(libcurl 空闲期进度回调粒度);唯一例外是 Stop 之前已 `RequestShutdown()` 的 `/shutdown`:不中止,worker 退出前恰好发出一次(单次尝试、不重试),Stop 最多多等 `timeouts.total`(默认 2s)。Stop 之后不再签发任何许可(阻塞 / 异步等待方一律拒绝,即使 worker 已写入 Allocated)(2026-10-17) |
| Fleet health | `-AgonesHealth*` 默认 30 / 10 / 3 | 复用同三个参数;initialDelaySeconds 不够覆盖 battle 启动最坏耗时(地址来源 ≤60s + 初始化余量 20s + `/ready` 重试约 53s + 首次 health 2s,合计 135s;默认 period 10 × threshold 3 时下限 105s,`lib/k8s_client_entry.ps1:543`、`:560-564`)时由 `Resolve-BattleFleetHealth`(`lib/k8s_client_entry.ps1:553`)抬高并打说明 |
| 驱逐 | 按原模板 | `eviction.safe: Never`;Fleet `scheduling: Packed` |

### 12.4 修复后的时序语义(WP6 评审修复与 2c 批 late-allocate)

- **排空先于分配**:Ready 成功之后、进入主循环之前先同步读一次排空标签,再处理挂起的 allocate;不会"先接单元再发现自己在排空"。
- **读取失败保持上一次判定**:GET 失败、响应没有 `object_meta` / `objectMeta`、labels 形状不符,都算读取失败;status 段形状问题不影响排空判定(排空只用 `ParseGameServerLabels`)。
  首次读取失败时按未排空处理(可用性优先的 fail-open,有 WARN,最长一个 `drainPollInterval` 后纠正)。解除排空 = 删标签(只看键是否存在,不看值)。
- **Agones 地址来源有硬预算**:`ClientEndpointSourceOptions.totalBudget=60s`(单调时钟,`agones_client_endpoint_source.h:36-47`),单次 GET 超时截到剩余预算,
  最多 30 次、退避 200ms→2s;address 为空、没有 `client` 端口、sidecar 不可达都算失败并重试,耗尽即 `LOG_FATAL`(fail-closed,由 Agones 重建)。
- **allocate 晚到收口**:`DoAllocate` 最坏约 6.4s,而等待方只等 3s。等待方全部超时之后 allocate 才成功时,若零单元、零在途许可、无等待方与非阻塞重试方,
  同一临界区内自动请求回 Ready,并打 WARN `allocate confirmed after every waiter gave up`(`agones_gameserver_lifecycle.cpp:525`)。
  否则该实例会以零房间的 Allocated 永久占着 Fleet 容量。非阻塞调用方踢了 allocate 却再也不来重试时,仍会停在"Allocated 但零单元"(与改动前的 scene 一致)。
- **已知未做**:`state_` 默认 Disabled,gRPC server 起来到 `Start()` 之间许可恒放行;battle 由准入闸兜住,scene 仍有这个窗口。显式的 NotStarted(默认拒绝)状态已登记为独立任务。

### 12.5 部署侧

- `-BattleOrchestrator agones` 在 infra namespace 生成 battle Fleet(只对 `infra-up` / `all-up` 生效,与 `-SceneOrchestrator` 相互独立);`-ClientEntryMode external` 时
  battle 带 `CLIENT_ENDPOINT_SOURCE=agones` 与 `CLIENT_ENDPOINT_REQUIRED=1`。Fleet 就绪等待是有界轮询(`Wait-ForFleetReady`)。参数口径见 `deploy/k8s/README.md` Optional Flags。
- 节点维护前给 battle 打 `mmorpg.io/drain=true`;防火墙放行 Agones 端口段。kind 验证用段 7100–7109(helm `gameservers.minPort=7100 / maxPort=7109`,见 `deploy/k8s/kind-config.yaml`)。
- gate **不进 Agones**(D87,长连接恒为 Allocated、Fleet 滚动永远收敛不了),走 StatefulSet + 每序号 Service。

### 12.6 新增文件索引(补 §10)

| 文件 | 作用 |
|---|---|
| `cpp/libs/engine/infra/agones/agones_gameserver_lifecycle.{h,cpp}` | 泛化后的状态机、worker、单元计数、许可、排空、心跳 |
| `cpp/libs/engine/infra/agones/agones_gameserver_status.{h,cpp}` | `/gameserver` JSON 解析(完整解析 / 只解析元数据段) |
| `cpp/libs/engine/infra/agones/agones_client_endpoint_source.{h,cpp}` | Agones 地址来源(`MakeClientEndpointSourceFromEnv`,常量 `kClientPortName="client"`) |
| `cpp/nodes/battle/main.cpp` | battle 的 THooks 地址来源工厂、lifecycle 启动选项、心跳、停机顺序 |
| `cpp/nodes/battle/battle_room_table.h` / `battle_admission_gate.h` | 房间表(增删恰好回调一次)与进程级准入闸,各有独立 gtest(`cpp/nodes/battle/tests/`,不进 vcxproj,编译命令见文件头) |

### 12.7 验证交接(Codex,仓库根,MSBuild 必须串行 `/m:1`)

- 前置:WP2 的 proto 重生(`NodeInfo.client_endpoint`);命令见 `docs/design/k8s-client-entry.md`,不是 `cd go && build.bat`。
- `msbuild` 依次 core → infra → scene → battle → `cpp\tests\agones_lifecycle_test\agones_lifecycle_test.vcxproj`(`/p:Configuration=Debug /p:Platform=x64 /m:1`),
  再 `pwsh -File tools/scripts/run_cpp_tests.ps1` 与 `pwsh -File tools/scripts/check_no_raw_pointer_member.ps1`。
  通过标准:infra 0 warning / 0 error;`agones_lifecycle_test` 全绿(磁盘上 `TEST(` 共 51 个,以实际运行数为准)。Linux 按 `tools/scripts/build_linux.sh`,`ldd` 确认 scene / battle 链接 libcurl。
- 行为回归:scene 日志应为 `require_loop_heartbeat=false ... drain_label=<disabled>` 且无 `GET /gameserver`;battle 应为 `require_loop_heartbeat=true ... drain_label=mmorpg.io/drain`。
- `/gameserver` 的真实 JSON 形状仍待 kind 阶段 B 用 curl 实测;元数据键名若既不是 `object_meta` 也不是 `objectMeta`,排空会一直保持上一次判定并每 5s 打 WARN(不会静默解除)。


### 2026-11-05 创建许可的协程接续

Scene/Battle 的 gRPC 创建守护段由各自服务对象的 `LoopTaskScope` 持有单次任务，等待现有 `AcquireAllocationPermitAsync` 后复核停机/准入并 `Proceed`。许可按值移入自有 RAII 对象，覆盖同步建房全过程。关闸取消本地等待，晚到许可仍自行释放，不能再次创建房间。

这层接续不启动线程、不调用阻塞许可接口、不增加期限或重试。原 Agones worker、超时和投回 EventLoop 的顺序保持不变。正常许可回复在原 loop 回调栈续跑。Java 不使用这套 C++ Agones 适配，无客户端契约变化。


## 13. 2026-11-10:rooms 名额改为 List 条目,增删幂等,对账两个方向都收敛(缺口清单第 1 条「仍剩」;已落码,未编译)

本节先由「按缺口清单做完服务器代码(11-05 额度重置后)」会话写了初稿。同日改由「场景销毁认领:回滚原子化与重发幂等」会话落码,落码前改了两处,见 13.2 的条目与 13.3 的在途占用;不选的做法见 13.7。§8 里讲 rooms 的段落已按本节改写。

### 13.1 问题

rooms 原来是一个 Counter,只有数,没有「这一份是谁的」。下面几种情况会让它记错,而且收不回来:

1. 建场景时 Agones 预占在 Redis 登记之前,进程在两者之间崩溃(多记);
2. 拆除任务的归还是「至多一次」(先置位再调 K8s),置位之后、调用之前进程退出(多记);
3. 归还调用失败,或请求已生效、回包丢了;为了不多还,这一份不再重试(多记);
4. 改派转移名额时新 GameServer 满了:旧的不还,新的没记(旧的多记,新的少记,新节点会被超卖)。

对账(原 §8.6)分不清哪一份是漏的,只能告警,运维也没有安全的手工修法。

### 13.2 名额条目

- 高密度模式下每个 GameServer 挂一个 Agones List `rooms`(与 Counter 同属 `CountsAndLists` 特性),`capacity` 取 `-AgonesRoomCapacity`。占用数就是条目数。
- **每次占用一个条目**,值是 `<scene_id>.<32 位十六进制随机串>`(`newAgonesRoomEntry`)。建场景、改派转移、对账补转都各占自己的一条。
  - 不用裸 scene_id:同一个场景的两路操作可能落在同一台 GameServer 上,例如改派落空、发现属主恰好是自己挑的节点时补转,或 A→B→A 来回改派。两路共用一个值时,一路按「删掉自己刚加的」去删,删掉的是另一路正在用的那条,变成少记。每次占用各有一条,谁都只删自己的。
- Redis 记录 `scene:{id}:agones_gs` 写成 `<GameServer 名>/<条目>`(Go 里的 `agonesRoom`)。GameServer 名是 DNS 子域名,不含 `/`。
- 非高密度模式不记名额,记录只有 GameServer 名,归还是空操作,与原来相同。
- 认领脚本、拆除任务、转移回执照旧把记录当作不透明的串搬运,只在 Go 里解析。改派转移的 CAS 要比「记录是不是已经在新 GameServer 上」,只在 Lua 里取 `/` 之前的部分。

### 13.3 在途占用

- 本 zone 的有序集合 `agones_rooms:zone:{z}:holds`,成员是条目,分数是截止时刻:写入时刻 + 5 分钟(`agonesRoomHoldTTL`)。
- **向 Agones 要名额之前先写在途占用。** Redis 写不进就不去要,这次建场景或转移按没有容量处理。
- 三段脚本把名额写进记录,都在同一步 `ZREM` 自己的在途占用:实例登记、频道登记、改派转移。删到了才写记录;删不到说明已被对账取消,这一次一个键都不写,回 `fenced`。
- 对账只取消**已过截止时刻**的在途占用,取消与「判这个条目是不是孤儿」在同一段脚本里。取消之后晚到的提交一律被挡住,所以截止时刻只决定多久以后可以回收(活性),不影响正确性。
- 失败路径(决定不提交):先删自己的在途占用,再尽力删自己的条目。删条目失败也不用管:没有记录、也没有在途占用的条目就是孤儿,对账下一轮收。
- 截止时刻要长过从写在途占用到提交的最慢正常路径:分配(K8s 请求 5s,加可能的一次 Pod GET),加登记 EVAL 的 go-redis 重发(最坏约 33s),再加一次作废查询。5 分钟留足余量。慢过它的提交只会失败,不会记错。

### 13.4 各动作

| 动作 | 做法 | 失败时 |
|---|---|---|
| 自由分配 | 选择器 `lists.rooms.minAvailable: 1`,动作 `lists.rooms.addValues: [条目]`,选中与占位是一步。之后核对返回对象 `status.lists.rooms.values` 里确有这一条;返回对象里没有 `lists` 时 GET 这台 GameServer 再核对。没有这一条按没容量处理,防的是列表已满时 Agones 不加值却照样回 Allocated | 删在途占用,尽力删条目 |
| 指定 GameServer(镜像共置、改派转移、对账补加) | 读 GameServer:条目已在就是成功;条目数到了 capacity 回 `ErrNoCapacity`;否则追加后带 resourceVersion 更新,409 冲突有界重试 | 同上 |
| 删除 | 读 GameServer:条目不在、GameServer 已不存在都算成功;否则删掉后带 resourceVersion 更新,409 冲突有界重试 | 调用方可以放心重试 |
| 登记结局 | `registered`:在途占用已消费。`conflict` / `aborted` / `fenced`:记录没指向这一条,删自己的条目与在途占用。`unknown`:都不动,在途占用过期之后由对账按记录判 | — |
| 改派转移 | 写在途占用 → 在新 GameServer 加条目 → CAS:属主仍是新节点才改记录,同一步消费在途占用。改成了就删旧条目。落空就删新条目:属主已变、场景已销毁、记录已在新 GameServer、在途占用被取消 | CAS 结局未知:都不动,交给对账 |
| 拆除任务 | 按快照里的记录删条目;失败时任务留着,扫描重做到成功。去掉原来的「先置位、至多一次」 | 重做 |

由此改变的调用方语义:

- **建实例先铸 scene_id 再要名额**:条目里要有它。分配失败浪费一个号,无害。铺世界频道本来就先铸号。
- **conflict 也删自己的条目**:原来分不清 conflict 的两种来历,怕双还,都不还;现在删的是自己那一条,不会删到别人的。
- **新节点满了的改派**仍然先不动旧的(不能让场景一份名额都不占),由对账每轮重试转过来。

### 13.5 对账

领导者每 `ReconcileIntervalSeconds`(缺省 60s)对本 zone 做一轮,只碰本 zone 的 GameServer 与本 zone 的场景键。列不出 GameServer 时整轮跳过,不改任何一方。

- **条目 → 记录**:列表里的每个条目在 Redis 里用一段脚本判(`luaJudgeAgonesRoomEntry`):
  - 场景登记在别的 zone:不碰,计入未收敛;
  - 记录等于「这台/这条」:在用;
  - 场景的拆除任务快照里是「这台/这条」:等拆除任务删;
  - 在途占用还没到截止时刻:在途;
  - 在途占用已过截止时刻:同一步取消它,然后按孤儿删;
  - 都不是:孤儿,删。
- **过期的在途占用,条目不在任何一台 GameServer 上**:取消。来历是分配根本没发出去,或者条目已经删了、在途占用没删掉。
- **记录 → 条目**:按 PodIP 把 GameServer 对上本 zone 的节点,读节点的场景集合,只看属主仍是这个节点的场景:
  - 记录在这台、条目不在:补加这一条,满了记 `missing_no_capacity`;
  - 记录在别的 GameServer(改派转移时新 GameServer 满了):按改派转移的做法转过来,满了记 `rehome_no_capacity`,下一轮再试。
- 单个条目、单个场景读写失败只跳过它,计入本轮未收敛数。
- 指标:
  - `scene_manager_agones_rooms_reconciled_total{zone,action}`,action 取 `orphan_removed`、`hold_expired_removed`、`hold_canceled`、`missing_added`、`missing_no_capacity`、`rehomed`、`rehome_no_capacity`;
  - `scene_manager_agones_counter_drift{zone}` 改为「本轮结束时仍没收敛的条目与场景数」,降级的副本把自己发过的 zone 写回 0;
  - 告警 `SceneManagerAgonesRoomsNotConverging`:持续 15 分钟大于 0。

### 13.6 部署

- Fleet 模板:`counters.rooms` 换成 `lists.rooms: {capacity: N}`(N = `-AgonesRoomCapacity`)。
- FleetAutoscaler:策略从 `type: Counter` 换成 `type: List`(`list.key: rooms`)。`bufferSize`、`minCapacity`、`maxCapacity` 的含义与单位不变。
- RBAC 不变(`gameservers` 的 get / list / update)。
- 项目未上线,不做 Counter 到 List 的迁移:换版本时整批重建 Fleet。

### 13.7 不选的做法

- **保留 Counter、另写按场景号的注解**:身份与数量分在两处,每次更新都要保证两处一致,还要防别人只改了计数。List 把身份和数量放在同一个字段里,一次写完。
- **对账直接把 Counter 改成 Redis 里的数**:Counter 被 GameServerAllocation 和本进程并发修改,按数覆盖会把别人刚占的名额抹掉,超卖。
- **条目只用 scene_id**(初稿的做法):见 13.2。
- **用「连续两轮、满宽限期」判孤儿,代替在途占用**(初稿的做法):宽限期挡不住慢过它的登记,登记落地时条目已经被删,就是少记、会超卖。在途占用与提交在同一步消费,取消之后晚到的提交被挡住,不依赖时间。

### 13.8 上集群前要核对

1. GameServerAllocation 的 `selectors[].lists.rooms.minAvailable`、`lists.rooms.addValues` 字段形状,以及返回对象里 `status.lists.rooms.values` 的位置。拿不到时代码会 GET 这台 GameServer 核对,只是多一次请求。
2. 直接 Update GameServer 改 `status.lists.rooms.values` 能被 Agones 接受。现在改 `status.counters` 用的就是这个机制(§8.9 第 3 条)。
3. FleetAutoscaler 的 `type: List` 策略字段。
4. Agones SDK sidecar 不会拿自己的缓存覆盖 `status.lists`。C++ 节点不调列表相关的 SDK。

### 13.9 代码位置

- `go/scene_manager/internal/agones/{allocator.go,k8s_allocator.go}`:接口改为 `Allocate(条目)` / `AddRoomEntry` / `RemoveRoomEntry` / `ListGameServerRooms(含条目)`。
- `internal/logic/agones_room_ledger.go`(新):条目与记录格式、在途占用、`luaJudgeAgonesRoomEntry`。
- `internal/logic/agones_binding.go`:分配、指定节点、放弃、归还、改派转移。
- `internal/logic/agones_reconcile.go`:两个方向的对账。
- `internal/logic/scene_registration.go`:两段登记脚本消费在途占用,新结局 `fenced`。
- `internal/logic/createscenelogic.go`:先铸号,按登记结局放弃名额。
- `internal/logic/world_init.go`:按登记结局放弃名额。
- `internal/logic/scene_teardown_job.go`:归还做到为止。
- `internal/metrics/metrics.go`;`tools/scripts/k8s_deploy.ps1` 与部署合同测试;`deploy/k8s/scene-manager-alerts.yaml`。
- C++ 节点不碰 rooms,不改。Java 版不接 Agones(PARITY「场景实例与主世界频道」行有意差异 D14),不适用。
