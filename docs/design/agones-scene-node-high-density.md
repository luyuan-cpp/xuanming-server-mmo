# Agones 管理 Scene Node(高密度 GameServer)

**日期**: 2026-07-29
**状态**: 阶段 A 已完成;阶段 B 已完成 Windows `Debug|x64` 全 solution 编译与
24 个 C++ 单测验证;阶段 C 已落码并通过 Go 侧 `build` / `vet` / 107 个单测
(其中 20 个为阶段 C 新增)。**三个阶段都没有上过集群**,也没有装过 Agones。

## 1. 为什么是 1 GameServer = 1 Scene Node,而不是 1 Scene

```
1 Agones GameServer
    = 1 Scene Node Pod / 1 个 C++ 进程
    = N 个动态创建的 ECS Scene 房间
```

不是「一个 Scene 一个 GameServer」。理由:

1. **Scene 是进程内的 entt 实体,不是进程**。`CreateScene` 只是
   `sceneRegistry.create()` + 挂 `SceneInfoComp` / `ScenePlayers`
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

SceneManager 继续走原有发现链路:C++ 节点把自己的 `Endpoint` 注册进 etcd
(`SceneNodeService.rpc/` 前缀),`scene_node_client.go` 从 etcd 拿 PodIP:port
建 gRPC 连接。Agones 不参与寻址。

## 4. Ready / Allocated 状态机

实现:`cpp/nodes/scene/agones/agones_scene_lifecycle.{h,cpp}`。

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
`SceneNodeGrpcImpl::CreateScene` 在 `runInLoop` **之前**调
`AcquireCreatePermitBlocking()`,拿不到 Allocated 就直接返回
`grpc::StatusCode::UNAVAILABLE`,一个实体都不创建。`CreateSceneResponse`
里没有错误字段(`proto/scene/scene.proto`),非 OK 的 gRPC 状态是唯一能让
SceneManager 知道"这次失败了、该回滚"的诚实手段。

**(b) HTTP 绝不进 muduo EventLoop。**
`docs/design/scene-node-threading-model.md` 的决策是单线程 EventLoop 跑
RPC + Kafka + `World::Update()`(33ms 帧)。一次 HTTP 抖动进去就是整帧卡住。
因此:

| 调用方 | 所在线程 | 用哪个 gate |
|---|---|---|
| `SceneNodeGrpcImpl::CreateScene` | gRPC 线程池 | `AcquireCreatePermitBlocking()`,有界等待(默认 3s) |
| `SceneHandler::CreateScene`(legacy muduo RPC) | **EventLoop** | `AcquireCreatePermitNonBlocking()`,立刻返回 false + 踢一次异步 allocate |

所有 HTTP 都在 lifecycle worker 线程上跑。gRPC 线程阻塞的是条件变量,不是 socket。

这不是"用定时器掩盖时序问题":到期后不假设成功,而是**拒绝这次创建**让调用方
重查权威后重试 —— 有界、有出口、fail-closed。

### 4.2 房间计数的唯一来源

计数挂在 `SceneEventHandler::OnSceneCreatedHandler` / `OnSceneDestroyedHandler`
(`cpp/nodes/scene/handler/event/scene_event_handler.cpp`)。

选这里而不是两个 RPC handler,是因为 gRPC 路径和 legacy 路径**都**只在实体
真的建出来/真的存在时才 `dispatcher.trigger()`:幂等命中、参数校验失败、
「销毁一个不存在的 Scene」全部提前 return。于是天然满足:

- 重复 CreateScene 不重复计数
- Destroy 不存在的 Scene 不减计数
- 创建失败不增加计数

`SceneLifecycle` 内部再用 `unordered_set<entity>` 按 key 幂等一次,所以即使
将来有第三条路径重复触发事件,计数也不会错、更不会为负。所有计数操作在同一把
互斥锁下,可从任意线程调用。

### 4.3 在途创建 permit 与零房间收口

gate 成功后到 EventLoop 真正完成实体创建之间存在排队窗口。两个 CreateScene
入口都持有 RAII `CreatePermit`:最后一个旧 Scene 销毁时,只有
`activeScenes == 0 && pendingCreates == 0` 才允许回 Ready。permit 析构时如果
仍是 Allocated 且没有实体,会自动请求回 Ready,覆盖参数非法/幂等命中等
"allocate 成功但没建出实体"的情况。

worker 真正发 `/ready` 前先把本地状态切成 `ReturningToReady`;此时新创建不能
复用旧的 Allocated 判断。阻塞路径等待 `/ready -> /allocate` 完成,非阻塞路径
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

`scene:{id}:agones_gs` 必须在第 8 步**之前**写。放到 RPC 成功之后写,进程正好
在中间崩溃就永远找不回该减哪个 GameServer 的计数。

### 8.3 修掉的既有 bug:phantom scene

改之前,`RequestNodeCreateSceneWithOptions` 失败只会打一条
`(Redis state committed)` 然后**照样返回成功**。结果是 Redis 里有映射、节点上
没有实体,玩家被路由进去后 `EnterScene` 永远成功不了。

现在失败一律回滚(Redis scene 全部键 + `node:{id}:scene_count` + 反向索引 +
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
`node:{id}:scene_count`,把差异写进 `scene_manager_agones_counter_drift{zone}`。

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
   `redis-cli GET node:<id>:scene_node_type` 期望 `0` / `1`
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
| `cpp/nodes/scene/agones/agones_rest_client.{h,cpp}` | 可注入 HTTP 传输层 + libcurl 实现 + Agones REST 端点封装 + 环境变量解析 |
| `cpp/nodes/scene/agones/agones_scene_lifecycle.{h,cpp}` | 状态机、lifecycle worker、房间计数、allocate 前置门 |
| `cpp/nodes/scene/handler/event/scene_event_handler.cpp` | 房间计数唯一接入点 |
| `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` | gRPC 路径的阻塞式 allocate 门 + idle 收口 |
| `cpp/nodes/scene/handler/rpc/scene_handler.cpp` | legacy muduo RPC 路径的非阻塞 allocate 门 |
| `cpp/nodes/scene/main.cpp` | `SetAfterStart` 起生命周期,`SetBeforeShutdown` + loop 退出后停并 join |
| `cpp/tests/agones_lifecycle_test/` | 24 个状态机单测(含在途 permit/回 Ready/停止态竞态;注入 fake transport,不需要 sidecar) |
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

`Debug|x64` 必须使用同一套 `/MDd` 第三方产物。Gate 实际链接的是 gRPC 随附的
BoringSSL,所以 Debug include 必须优先使用
`third_party/grpc/third_party/boringssl-with-bazel/include`;不能生成独立 OpenSSL 3
的 `configuration.h` 后与 BoringSSL 库混用。

当前 Abseil 已把 `crc_cpu_detect` 迁为 `base_cpu_detect`,并删除了
`low_level_hash` / `string_view` 两个独立库产物。Gate/Scene Debug 链接优先读取
`third_party/grpc/install_vs2026_dbg/lib`,其中 gRPC、Abseil、Protobuf、BoringSSL、
zlib、hiredis、yaml-cpp 均按 `/MDd` 构建;Debug 的 `/WHOLEARCHIVE` 也必须指向
`hiredisd.lib` / `yaml-cppd.lib`。

2026-07-28 本地验证:

- `msbuild game.sln /m /p:Configuration=Debug /p:Platform=x64`:成功。
- `bin/gate.exe` 与 `bin/scene.exe`:成功生成并由各节点 PostBuildEvent 复制。
- `agones_lifecycle_test.exe --gtest_brief=1`:24/24 PASS。
- 未连接 dev 集群、未安装 Agones、未构建运行时镜像。
