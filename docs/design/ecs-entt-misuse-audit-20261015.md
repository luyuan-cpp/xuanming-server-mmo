# entt 误用审计（2026-10-15）

> 起因：用户问「我代码里面哪些乱用了 entt，我感觉有些地方很奇怪」。本文是那次只读审计的结论与修复状态，修 entt 相关问题前先看这里。
> 组件访问规则本身见 [ecs-component-access-rules.md](ecs-component-access-rules.md)。
> **2026-10-15 实施补充**：第 2–5 批源码和回归用例已由 Codex 按授权落盘，用户明确暂缓生成器构建、统一生成、编译和测试；当前生成树仍可包含旧接口。实现范围与保留项见 分批总回执 §7（原 `docs/handoff/ecs-refactor/README.md`，已清理，见 `ccf2840b2`）。
> **2026-10-16 审查方更新**：附录表的「HEAD 状态」列已按独立审查逐条核对当前源码后改写，结论见 审查报告（原 `docs/handoff/ecs-refactor/review-report-20261015.md`，已清理，见 `ccf2840b2`） §6，汇总见下文「2026-10-16 审查后的状态」。这些都是源码层面的结论，**运行证据待负责人编译与测试**，拿到运行证据前不关闭任何条目。单元格里的「审查 Wx」指向报告中与该条相关、仍需修的问题，修复任务见 第 6 批任务单（原 `docs/handoff/ecs-refactor/batch6-review-fixes.md`，已清理，见 `ccf2840b2`）。

## 范围与方法

- **范围**：`cpp/` 下全部用到 entt / `tlsEcs` / `tlsNodeContextManager` 的非测试代码，以及生成这些代码的模板 `tools/proto_generator/protogen/internal/template/**`。entt 版本 3.13.2，`ENTT_ID_TYPE=uint64_t`（低 32 位 index、高 32 位 version）。
- **查找**：12 个按目录分片的审查 + 7 个跨全仓专项（跨 registry、句柄当外部 ID、引用失效、访问规则、句柄生命周期、dispatcher、设计异味），之后又从 4 个补漏视角扫了一遍：同模式扩散、未覆盖文件与生成器模板、`tlsEcs` 线程亲和、细粒度 API 语义。
- **复核**：每条都交给独立的复核者，要求它尽力推翻，拿不出代码证据就判不成立；high 再加一位独立二审。
- **基线**：发现产生于 `2f897bd07`。复核期间 HEAD 前进到 `ebccaf04d`，修复会话的改动进了这个提交，下文的「HEAD 状态」以它为准。
- **结果**：共 110 条，**102 条成立**（1 high / 24 medium / 77 low），8 条被推翻。全部条目见文末附录。
- 全程只读，没有改代码，也没有跑构建或测试。

## 为什么「奇怪」：五个根源习惯

1. **把 `entt::entity` 当对外 ID 用。** 这一类条目最多。
   - 实体整数直接下发给客户端当 actor ID（`view.cpp:90`、`aoi.cpp`、`movement.cpp`），客户端再把它原样传回来当技能目标。
   - 写进 proto 事件，经 Kafka 跨进程派发（`node_kafka_command_handler.h:163` → 生成的 `DispatchProtoEvent`）。
   - 截成 uint32，丢掉 version（`node_event.proto:19` 的 `ConnectToNodeEvent.entity`，`client_message_processor.cpp:405/413` 的日志，`anomaly_detector.h:90` 的哈希）。
   - SnowFlake 的 scene_id 直接当实体用（`s2s_player_scene_handler.cpp:24`）。
   - 实体整数冒充玩家号写进日志（`player_lifecycle_handler.cpp:28`、`mission.cpp:82`）。
   - `game_registry.h:17` 在 `namespace entt` 里自定义了一个 `to_entity(uint64_t)`，和官方的 `entt::to_entity` 同名，语义却相反。
   - 「没有目标 / 没有施法者」有两套哨兵：0 和 `entt::null`。0 号实体问题已修（F048）；`entt::null` 那一套还在（C003）。
2. **不用 `registry.ctx()`，而是造一个全局实体，挂上组件当单例，再用 `get_or_emplace` 去取。** 全仓没有一处用 `ctx()`。
   - `TipInfoMessage` 单例曾把 handler 写好的拒绝码覆盖成「成功」（F003，已修）。
   - `Clear()` 之后再取，会悄悄造出空的 `NodeInfo`（F008，见下表）。
   - `ErrorEntity`、`OperatorEntity`、`itemRegistry` 都没人用；`nodeGlobalRegistry` 只为放一个单例。
3. **组件该由谁创建没有约定。** 于是到处用 `get_or_emplace` 懒建，或者用 `try_get` 拿不到就静默跳过，功能空转了也看不出来。
   - 技能冷却组件和上下文组件曾经没有任何创建点，整条技能链空转（已修）。
   - 存盘 Marshal 用 `get_or_emplace` 取全部持久化组件，组件一旦缺失就会把零值写进 DB（F059；2026-10-16 源码已修）。
   - `PlayerItemBlockList`、AOI 优先级策略组件在生产代码里也没有创建点（C004、C006）。
   - buff、AOI、属性脏标记、宠物 `EnsureComp` 等热路径上都有 `get_or_emplace`。
4. **生命周期与引用。**
   - 节点实体上挂着 gRPC CompletionQueue，销毁前没有先 Shutdown 再排空（F010）。
   - 生成的 `Send*` 直接 `get` CQ 和 Stub，而选点只按 `view<NodeInfo>` 挑（F100）。
   - dispatcher 的 sink 绑了 `*this`，从不断开（F009、F083）。
   - 遍历容器时经调用链改同一个容器（F040、F047，已修）。
   - 组件引用跨过同步的 `dispatcher.trigger` 继续使用（F067、F087、F096）。
5. **registry 的划分和实际用法对不上。**
   - 物品实体其实放在每个 Bag 私有的 registry 里（F024）。
   - `SceneEntityComp` 在 actor 组件里存 sceneRegistry 的裸句柄（F029）；`ECS_GET_OR_*` 宏写死查 `actorRegistry`（F005）。
   - `Guid = uint64_t` 这类基础类型直接当组件，导致所有 uint64 组件共用一个 pool（F032、F099）。
   - 背包扣除选哪一堆，取决于 pool 的内部迭代顺序（F023）。

## high / medium 条目与 HEAD 状态

### 2026-10-16 审查后的状态（源码层面）

110 条中：已修 90、部分修 7（F014、F043、F052、F085、F095、F099、C006）、不再存在 5（C007、F007、F010、F093、C008，相关代码已被 RPC 通道重构或 gRPC→muduo 迁移删除）、原审计已推翻 8，没有原样未修的。「已修」不等于没有问题：下表「审查」列出的条目是修法本身留下、仍需修的问题。

| ID | 当前状态（源码） | 说明 |
|---|---|---|
| F028 | 已修 | 已修（ebccaf04d：状态落定后同步 trigger）；审查 W2-cross_sem-02 |
| F001 | 已修 | 已修（第 4 批：OptionKafkaIngress 白名单）；审查 W1-16、W1-17、W1-19、W1-21 |
| F002 | 已修 | 已修（第 4 批：白名单+进程内事件改结构体）；审查 W1-16、W1-17 |
| F013 | 已修 | 已修（第 4 批：删默认全量派发，hook 显式指定）；审查 W1-16、W1-20 |
| F003 | 已修 | 已修（第 2 批：删全局 tip，handler 直写 response）；审查 W2-b2-02 |
| F090 | 已修 | 已修（第 2 批：同 F003，ReleaseSkill 直写应答）；审查 W2-b2-02 |
| F008 | 已修 | 已修（第 2 批：NodeInfo 归 Node，发现快照归管理器）；审查 W2-b2-03、W2-b2-08、W2-b2-06 |
| F010 | 不再存在 | 不再存在（muduo 迁移删 CQ，摘除时通道回 UNAVAILABLE） |
| F100 | 已修 | 已修（§8 收尾守卫，muduo 迁移模板 FindGrpcChannel） |
| F012 | 已修 | 已修（§8 收尾：teardown 内整体 Clear 节点池） |
| F022 | 已修 | 已修（§8 收尾：FinalizeTeardownInLoop 调 Clear）；审查 W2-b2-01 |
| F027 | 已修 | 已修（ebccaf04d 保留64位；第4批事件改为 entt::entity 结构体）；审查 W1-30 |
| F040 | 已修 | 已修（先收集 id 再逐个重查） |
| F041 | 已修 | 已修（改 try_get，缺属性组件跳过） |
| F044 | 已修 | 已修（第 1 批：StartCooldown/上下文写入口延迟创建）；审查 W2-b1_runtime-03、W2-cross_sem-11 |
| F050 | 已修 | 已修（同 F044：写入口延迟创建、缺数据收尾）；审查 W2-b1_runtime-01、W2-b1_runtime-03、W2-b1_runtime-04、W2-cross_sem-11 |
| F046 | 已修 | 已修（ActorNetIds 反查+同场景校验）；审查 W1-26 |
| F049 | 已修 | 已修（同 F046:ResolveSkillTarget）；审查 W1-26 |
| F047 | 已修 | 已修（第 5 批删目标镜像，只删施法者一次） |
| F048 | 已修 | 已修（EcsContext 构造时退役 0 号句柄） |
| F059 | 已修 | 已修（第1批：Marshal 改 try_get，缺组件整份拒存）；审查 W1-01、W1-02、W1-03 |
| F060 | 已修 | 已修（第1批：模板同步 FindPersistedComp 拒存）；审查 W1-01、W1-02、W1-03 |
| F080 | 已修 | 已修（第4批 4d：改用 ActorNetIds 不透明网络号）；审查 W1-25、W1-26、W1-31 |
| F091 | 已修 | 已修（第1批：按 SceneInfo 反查 scene_id，找不到即拒绝）；审查 W2-b5_types-02、W2-fit-02 |
| C003 | 已修 | 已修（第 1 批：ProcessedCasterOf，no_caster 记 0） |

下面两张表是 `ebccaf04d` 时的历史状态，保留原文。

### 仍未修（`ebccaf04d` 时，历史）

| ID | 位置 | 问题 |
|---|---|---|
| F001 / F002 / F013 / F098 | `node_kafka_command_handler.h:163`，生成器 `event_id.go:188` | Kafka 入口没有事件白名单。47 个进程内事件都能从外部打进来，其中很多事件的载荷是本进程的实体整数 |
| F059 / F060 | `player_database_loader.cpp:141`，模板 `player_data_loader.cpp.tmpl:25` | 存盘 Marshal 用 `get_or_emplace` 取持久化组件，组件缺失时会把零值货币和零值账本写进 Redis 和 DB |
| F010 | `node.cpp:1491` | 摘除节点时直接 destroy 挂着 CompletionQueue 的实体，在途 gRPC 调用的应答和失败回调永远不会投递 |
| F100 | `grpc_async_client.cpp.tmpl:150` | 生成的 `Send*` 对节点实体无守卫 `get<CompletionQueue>` / `get<StubPtr>`；registry 里混着 TCP 握手建的、缺这两个组件的实体 |
| F008 | `node.cpp:472` | 进程级单例靠 `get_or_emplace` 从全局实体上取。停机时 `Clear()` 之后再取，会造出一个空的 NodeInfo，gate 的断线通知随之丢失 |
| F012 / F022 | `node_entry.h:165`、`node_context_manager.h:56` | **部分修好**：`FinalizeShutdownInLoop` 已清 `RpcClientPtr`；`RunNodeMain` 的兜底路径和 CQ / stub 还没处理 |
| F091 | `s2s_player_scene_handler.cpp:24` | 把 SnowFlake 的 scene_id 直接 `to_entity` 当场景句柄（目前没有发送方，处于潜伏状态） |
| C003 | `buff.cpp:139/278` | no_caster 的 buff 把 `entt::null` 写进 `processed_caster`，叠层时再拿它和施法者整数比较，永远匹配不上。结果每次都新建一条，`max_layer` 失效，和战斗引擎的叠层口径分叉。目前表里没有 no_caster=1 的行 |
| F080 | `view.cpp:90` | 实体整数当客户端 actor ID 下发。**入站已防住**（`ResolveSkillTarget`），根因仍在 |

### 已在 `ebccaf04d` 修好（历史）

| ID | 问题 |
|---|---|
| F028（high） | 任务完成事件用 `dispatcher.enqueue` 入队，生产代码却从不 `update`：自动发奖、任务链、「完成任务」条件全部失效。已改为状态落定后同步 `trigger` |
| F027 | 任务事件里的句柄被截成 uint32 |
| F048 | 0 号实体和协议里的「没有目标」撞在一起。现在线程启动时让 0 号句柄退役 |
| F044 / F050 | 技能冷却组件和上下文组件没有创建点。现在 actor 初始化时挂上（第 1 批起改为按需：StartCooldown 与技能上下文写入口首次写入时创建，actor 初始化不再预挂，见 [组件访问规则](ecs-component-access-rules.md)「第 1 批核对后的具体创建入口」） |
| F046 / F049 | 客户端 `target_id` 能命中别的场景的玩家。现在统一走 `ResolveSkillTarget`：要求是玩家或 NPC、同场景、未冻结 |
| F003 / F090 | `TRANSFER_ERROR_MESSAGE` 用空 tip 覆盖拒绝码 |
| F040 / F047 | 遍历中修改同一容器；对自己放技能时同一条上下文被删两次 |
| F041 | 每帧 buff tick 里无守卫 `get` |

## 修这些问题时的注意事项

- **`dispatcher.clear()` 不会断开 sink。** entt 3.13.2 的 `clear()` 只丢弃排队中的事件（`dispatcher.hpp:56-61/351-356`）。F009 / F083 要靠成对的 `disconnect`，或者干脆不绑 `*this`。
- **不要用「补一句 `dispatcher.update()`」修 enqueue 问题。** entt 3.13 派发时把队列元素的引用交给监听者，任务链又会往同一个队列里塞事件，队列一扩容这个引用就悬空了。F028 因此改成了同步 `trigger`。
- **F010 / F100 / F006 / F082 / F011 / F012 / F022 / C008 的 gRPC 部分**：CQ、Stub 和流式调用组件已随 gRPC→muduo 迁移删除（2026-10-16），原先「先 TryCancel、Shutdown CQ 并排空」的修法不再适用。节点实体上只剩一个 MuduoGrpcChannelPtr，摘节点时先移除通道，~Channel 把在途调用以 UNAVAILABLE 回调。验收条件见 [grpc-muduo-transport.md](grpc-muduo-transport.md) §10.1 与 §14。
- **收窄 Kafka 白名单（F001 / F002）之前，先盘清 Go 和 Java 两版里所有往这个入口发事件的地方。** 漏掉一个，合法事件就会被静默丢弃。
- **F059 要改 `player_lifecycle.cpp`。** 2026-10-15 时入场链正在被另一个会话大改，等它落地后再做。存盘侧应改为只读加 fail-closed，同时修改生成器模板。
- `NodeContextManager::GetRegistry` 只有 `assert` 边界检查。几处入口直接拿对端传来的 `node_type` 选 registry（F093、C007、`registration_manager.cpp:151`），Release 下会越界，应加上 Release 也生效的检查。（2026-10-15：这三处都已不再用对端的 node_type 选 registry —— Route 一族改为显式拒绝，握手应答改为按本端发起时的 nodeType 选 registry、对端自报值只核对，见 muduo-rpc-channel.md §8.3 / §9.3。）

## 查过、没有问题的

- **线程亲和。** 全进程只有一个 EventLoop，ECS 只在这个线程上访问。具体核对过：
  - gRPC 同步服务的每个方法都用 `runInLoop` 加 promise/future 投回 loop，投递之前不碰 ECS（2026-10-16：第 4 批起服务端改为 loop 上的 `muduo_grpc::Server`，守护段与 `Handle*` 本来就在 loop 线程，不再跨线程投递，见 [grpc-muduo-transport.md](grpc-muduo-transport.md) §15；代码未编译）；
  - Kafka poll 线程经 `queueInLoop` 投回；
  - etcd 的 CQ 由 loop 上的定时器轮询（2026-10-16：已改为 MuduoEtcdTransport，见 [grpc-muduo-transport.md](grpc-muduo-transport.md) §13）；
  - 信号与控制台处理只置标志或 `queueInLoop`；
  - 没有跨线程 connect / trigger 的 sink。
- **battle 节点与 `libs/services/battle`**：不用 entt。
- **API 细节。**
  - `view.get` 取的类型都在 view 内；
  - `exclude` 用法正确，遍历中改当前实体的组件属于 entt 允许的范围；
  - 没有用 `size_hint` 当精确数量；
  - `in_place_delete` 的定制与指针假设一致；
  - `ENTT_ID_TYPE` 在所有 x64 配置里一致。

## 与 Java 版的关系

Java 版不用 entt，本审计的 ECS 部分对它不适用。不过有两条是客户端可见的行为，Java 版是否等价**尚未核对**：
- 技能目标必须同场景，冷却真正生效；
- no_caster buff 应跨施法者叠层（C003）。

## 附录：全部条目

F 开头的是查找阶段的发现，C 开头的是补漏阶段的发现。「结论」列含义：确认、降级（问题真实但严重度或描述有更正）、推翻；带「二审」的是 high 级的独立复核。行号以 `2f897bd07` / `ebccaf04d` 为准。「HEAD 状态」列于 2026-10-16 由审查方按当前源码逐条更新（源码层面，运行证据待负责人编译与测试）；「审查 Wx」指向审查报告中与该条相关、仍需修的问题。

### 成立(确认/降级)(102 条)

| ID | 严重度 | 结论 | 位置 | 问题 | HEAD 状态 |
|---|---|---|---|---|---|
| F028 | high | 确认(二审) | `cpp/libs/modules/mission/system/mission.cpp:358` | 任务完成后用 dispatcher.enqueue 入队，但生产代码从不调用 dispatcher.update()，事件永远不派发，还会无限堆积 | 已修（ebccaf04d：状态落定后同步 trigger）；审查 W2-cross_sem-02 |
| F001 | medium | 降级 | `cpp/generated/rpc/service_metadata/rpc_event_registry.cpp:1721` | DispatchProtoEvent(rpc_event_registry.cpp:1783)把全部 47 个事件都开放给 Kafka 命令入口，其中多数事件的载荷是进程内 entt 句柄整数(生成器问题；与 F002 同一问题) | 已修（第 4 批：OptionKafkaIngress 白名单）；审查 W1-16、W1-17、W1-19等 |
| F002 | medium | 确认 | `cpp/generated/rpc/service_metadata/rpc_event_registry.cpp:1783` | Kafka 入口能分派载荷为本进程 entity 整数的进程内事件，外进程句柄被直接拿来索引本地 registry | 已修（第 4 批：白名单+进程内事件改结构体）；审查 W1-16、W1-17 |
| F003 | medium | 确认 | `cpp/libs/engine/core/macros/return_define.h:67` | 挂在 GlobalEntity 上的 TipInfoMessage 单例被 TRANSFER_ERROR_MESSAGE 整体覆盖进应答，ReleaseSkill 的拒绝码被抹成成功 | 已修（第 2 批：删全局 tip，handler 直写 response）；审查 W2-b2-02 |
| F010 | medium | 确认 | `cpp/libs/engine/core/node/system/node/node.cpp:1491` | 节点摘除时直接 destroy 挂着 grpc::CompletionQueue 组件的节点实体,没有先 Shutdown 再排空;在途 gRPC 调用的应答和失败回调永远不会投递(生成器模板问题) | 不再存在（muduo 迁移删 CQ，摘除时通道回 UNAVAILABLE） |
| F008 | medium | 确认 | `cpp/libs/engine/core/node/system/node/node.cpp:472` | 进程级单例挂在 thread_local registry 的懒创建实体上，到处用 get_or_emplace 取：线程不对或 Clear 之后会静默造出空的 NodeInfo/ServiceNodeList | 已修（第 2 批：NodeInfo 归 Node，发现快照归管理器）；审查 W2-b2-03、W2-b2-08、W2-b2-06 |
| F012 | medium | 确认 | `cpp/libs/engine/core/node/system/node/node_entry.h:165` | 退出时只清了 tlsEcs，从没清 tlsNodeContextManager：节点 registry 里的 RpcClient/TcpClient 要等 EventLoop 析构之后才销毁 | 已修（§8 收尾：teardown 内整体 Clear 节点池） |
| F013 | medium | 确认 | `cpp/libs/engine/core/node/system/node/node_kafka_command_handler.h:163` | scene 的 Kafka 入口会把所有 proto 事件原样 trigger，包括携带进程内 entity 句柄的事件，跨进程整数被直接 to_entity 去索引 actorRegistry | 已修（第 4 批：删默认全量派发，hook 显式指定）；审查 W1-16、W1-20 |
| F022 | medium | 确认 | `cpp/libs/engine/thread_context/node_context_manager.h:56` | tlsNodeContextManager 停机时从不 Clear，里面的 RpcClient/gRPC/protobuf 组件要等 thread_local 析构才释放，此时 EventLoop 和 protobuf 都已关闭 | 已修（§8 收尾：FinalizeTeardownInLoop 调 Clear）；审查 W2-b2-01 |
| F027 | medium | 确认 | `cpp/libs/modules/mission/system/mission.cpp:341` | OnMissionCompletion 把 64 位实体句柄截成 uint32，丢掉 version，槽位复用过的玩家收不到任务事件 | 已修（ebccaf04d 保留64位；第4批事件改为 entt::entity 结构体）；审查 W1-30 |
| C003 | medium | 确认 | `cpp/libs/services/scene/combat/buff/system/buff.cpp:278` | processed_caster 用 entt::null 表示「不区分施法者」，HandleExistingBuff 却拿它和原始 caster 整数比较，no_caster 的 buff 永远叠不上、每次都新建条目 | 已修（第 1 批：ProcessedCasterOf，no_caster 记 0） |
| F040 | medium | 确认 | `cpp/libs/services/scene/combat/buff/system/buff_impl.cpp:35` | 遍历 caster 的 BuffListComp 时,经 AddSubBuffs -> AddOrUpdateBuff 往同一个容器插入或删除元素 | 已修（先收集 id 再逐个重查） |
| F041 | medium | 确认 | `cpp/libs/services/scene/combat/buff/system/modifier_buff_impl.cpp:39` | 每帧 buff tick 路径里对 parent 无守卫地 get 三个组件 | 已修（改 try_get，缺属性组件跳过） |
| F046 | medium | 降级(二审) | `cpp/libs/services/scene/combat/skill/system/skill.cpp:139` | 客户端上报的 target_id 直接当 entt::entity 用,只做 valid() 校验,可以跨场景、对冻结或战斗中的玩家生效 | 已修（ActorNetIds 反查+同场景校验）；审查 W1-26 |
| F047 | medium | 确认 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:308` | HandleSkillFinish 在 caster==target 时,两次 try_get 拿到同一个 SkillContextCompMap,先按 key 删掉节点,再用已失效的迭代器 erase,构成重复删除 | 已修（第 5 批删目标镜像，只删施法者一次） |
| F048 | medium | 确认 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:377` | 用 0 当「没有目标/没有施法者」的哨兵,但 entt 第一个创建的实体就是整数 0(本节点第一个登录的玩家) | 已修（EcsContext 构造时退役 0 号句柄） |
| F049 | medium | 降级 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:395` | ReleaseSkill 把客户端 target_id 直接当 entt 句柄，只做 valid() 检查、不校验同场景；当前可跨场景对任意玩家触发 OnSkillHit(剥离脱战加成 buff)和 LookAt，伤害链因 SkillContextCompMap 未挂载而休眠，修 F050 后会升级为跨场景伤害 | 已修（同 F046:ResolveSkillTarget）；审查 W1-26 |
| F050 | medium | 降级 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:449` | SkillContextCompMap 和 CooldownTimeListComp 生产代码里没人挂,技能路径用 try_get 宏静默放行:冷却不生效,施法链全部空转 | 已修（同 F044：写入口延迟创建、缺数据收尾）；审查 W2-b1_runtime-01、W2-b1_runtime-03、W2-b1_runtime-04等 |
| F044 | medium | 确认 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:85` | 技能链路依赖的 CooldownTimeListComp / SkillContextCompMap 在生产代码里没有任何创建点，所有读写都用 try_get/ECS_GET_OR_* 静默早退，导致冷却从不生效、技能上下文从不入表 | 已修（第 1 批：StartCooldown/上下文写入口延迟创建）；审查 W2-b1_runtime-03、W2-cross_sem-11 |
| F059 | medium | 确认 | `cpp/libs/services/scene/player/system/player_database_loader.cpp:141` | 存盘 Marshal 对所有持久化组件用 get_or_emplace：组件一旦缺失就会造出零值账本和货币并写进 Redis 与 DB | 已修（第1批：Marshal 改 try_get，缺组件整份拒存）；审查 W1-01、W1-02、W1-03 |
| F060 | medium | 确认 | `cpp/libs/services/scene/player/system/player_database_loader.cpp:156` | 存盘 Marshal 对全部持久化组件 get_or_emplace：组件缺失时会把零值或空账本当真实数据写进 Redis/DB（规则源自生成器模板） | 已修（第1批：模板同步 FindPersistedComp 拒存）；审查 W1-01、W1-02、W1-03 |
| F080 | medium | 确认 | `cpp/libs/services/scene/spatial/system/view.cpp:90` | 把 entt::entity 原样当成客户端可见的 actor ID 下发,客户端再原样回传当目标 | 已修（第4批 4d：改用 ActorNetIds 不透明网络号）；审查 W1-25、W1-26、W1-31 |
| F090 | medium | 确认 | `cpp/nodes/scene/handler/rpc/player/player_skill_handler.cpp:25` | ReleaseSkill 直接写 response->error_message,被生成层用 globalRegistry 上的空 TipInfoMessage 覆盖 | 已修（第 2 批：同 F003，ReleaseSkill 直写应答）；审查 W2-b2-02 |
| F091 | medium | 确认 | `cpp/nodes/scene/handler/rpc/player/s2s_player_scene_handler.cpp:24` | 把 snowflake 的 scene_id 直接 entt::to_entity 当成 sceneRegistry 句柄用 | 已修（第1批：按 SceneInfo 反查 scene_id，找不到即拒绝）；审查 W2-b5_types-02、W2-fit-02 |
| F100 | medium | 确认 | `tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl:150` | 生成的 gRPC Send* 对 nodeEntity 直接 get<CompletionQueue>/get<StubPtr>，而选点函数只按 view<NodeInfo> 挑，registry 里还混着 TCP 握手建的、缺这两个组件的实体 | 已修（§8 收尾守卫，muduo 迁移模板 FindGrpcChannel） |
| F005 | low | 确认 | `cpp/libs/engine/core/macros/return_define.h:119` | 5 个 registry 的句柄同为 entt::entity，ECS_GET_OR_* 写死查 actorRegistry；scene_crowd 已出现拿 actor 句柄查 sceneRegistry | 已修（第 5 批：SceneEntity 类型隔离，删 scene_crowd） |
| F004 | low | 降级 | `cpp/libs/engine/core/macros/return_define.h:71` | TipInfoMessage 以 get_or_emplace 挂在 GlobalEntity 上，充当进程级隐式输出邮箱，正确性依赖每条写路径都同步走非 Empty 的 CallMethod 并由 TRANSFER 清空；目前没有可达的残留路径，但约定很脆弱，本应使用 ctx() | 已修（第 2 批：删全局 tip 邮箱，应答即请求级所有者）；审查 W2-b2-05、W2-b2-06、W2-cross_sem-07 |
| F006 | low | 确认 | `cpp/libs/engine/core/network/node_utils.cpp:248` | GetSceneManagerEntity 明知会选出没挂 CompletionQueue/Stub 的实体，交给生成的发送函数做未守卫的 registry.get | 已修（§8 收尾：选点只收有可用通道的实例） |
| F007 | low | 降级 | `cpp/libs/engine/core/network/player_message_utils.cpp:300` | ResolvePlayerNodeSession 把业务 node_id 直接构造成 entt::entity 查节点注册表（死代码中的 R03 残留） | 不再存在（RPC 通道重构删除 ResolvePlayerNodeSession） |
| F009 | low | 确认 | `cpp/libs/engine/core/node/system/node/node.cpp:1239` | thread_local dispatcher 绑定了 Node / RpcClientSessionHandler 的实例，从不断开，Clear() 也不清 sink | 已修（第 5 批：Node 订阅改 scoped_connection）；审查 W2-fit-07 |
| F011 | low | 降级 | `cpp/libs/engine/core/node/system/node/node_connector.cpp:47` | 节点 registry 把出站 gRPC 客户端拆成多个组件，又和 TCP 入站会话混放在同一 registry；生成的 Send* 不加守卫直接 get。只有内网对端握手时谎报 node_type=SceneManager 才会触发，选择器分级已把这种实体压到最后 | 已修（§8 拒入站纯 gRPC 类型；迁移后单通道组件）；审查 W2-fit-06 |
| F014 | low | 降级 | `cpp/libs/engine/core/node/system/registration/registration_manager.cpp:120` | 节点类型 registry 里出站实体和入站握手实体只靠组件组合区分，gRPC 的去重和已连接判断只看 view<NodeInfo>；当前拓扑下触发不到 | 部分修：node_util.cpp:197 gRPC 去重仍只看 NodeInfo；审查 W2-fit-06 |
| F015 | low | 确认 | `cpp/libs/engine/core/node/system/registration/registration_manager.cpp:204` | ConnectToNodeEvent.entity 定义成 uint32，64 位节点句柄写进去会丢 version | 已修（第 4 批：ConnectToNodeEvent 随零监听删除） |
| F017 | low | 降级 | `cpp/libs/engine/core/utils/registry/game_registry.h:17` | namespace entt 里注入了与官方 entt::to_entity 同名、语义相反的 to_entity(uint64_t)；唯一把 GUID 当句柄的 s2s EnterScene 没有任何发送方，查不到时也会安全失败 | 已修（第 4 批：删自定义 to_entity，s2s 改 FindSceneById） |
| F016 | low | 确认 | `cpp/libs/engine/core/utils/registry/game_registry.h:6` | EventOwner 带隐式实体转换，CreateEntityIfNotExists/RecreateEntity 用外部整数 create(hint)，两者都是无人使用的地雷 | 已修（第 4/5 批：删 EventOwner/Create*/Recreate*） |
| F019 | low | 确认 | `cpp/libs/engine/thread_context/ecs_context.cpp:13` | 单例一律挂在 GlobalEntity/GrpcNodeEntity 上并用 get_or_emplace 访问（全仓没有用 ctx()）；ErrorEntity/OperatorEntity 是死代码；AOI 每次可见性变化都对 GlobalEntity 做 get_or_emplace 取临时消息 | 已修（第 2 批：单例归位，删死实体，AOI 改 TLS 缓冲）；审查 W2-b2-01、W2-b2-07 |
| F018 | low | 降级 | `cpp/libs/engine/thread_context/ecs_context.cpp:5` | globalRegistry / nodeGlobalRegistry 实际各只装一个实体，当「类型→单例」容器用（全仓没有 ctx()），单例访问一律走 get_or_emplace，包括每条客户端消息和 AOI 路径 | 已修（第 2 批：删 globalRegistry/nodeGlobalRegistry）；审查 W2-b2-08 |
| F020 | low | 降级 | `cpp/libs/engine/thread_context/ecs_context.h:18` | 单例统一用「懒建实体 + get_or_emplace」实现；itemRegistry、ErrorEntity、OperatorEntity 闲置，nodeGlobalRegistry 只为放一个单例（设计异味，目前没有实际后果） | 已修（第 2 批：删懒建单例实体与 itemRegistry 等）；审查 W2-b2-01、W2-b2-08 |
| F021 | low | 确认 | `cpp/libs/engine/thread_context/ecs_context.h:21` | registry 的划分和实际用法对不上:itemRegistry 是死的，物品实体其实在每个 Bag 私有的 registry 里，句柄却对外暴露 | 已修（第 3 批 ItemStore 去 entt;itemRegistry 已删） |
| F023 | low | 确认 | `cpp/libs/modules/bag/item_store.cpp:241` | 扣除、并堆、合并选哪一堆完全取决于 entt pool 的内部迭代序(逆序 + swap-and-pop 重排) | 已修（第 3 批：实例序+槽位 rank 显式排序）；审查 W1-05、W1-09、W1-13 |
| F024 | low | 确认 | `cpp/libs/modules/bag/item_store.h:188` | 每个 Bag 私有一个完整 entt::registry 只放一种 ItemComp,实体仅当 map 值;registry 嵌在组件里,公开 API 还外泄私有 registry 的句柄 | 已修（第 3 批：ItemStore 改 Guid→ItemComp 字典）；审查 W1-12 |
| F025 | low | 确认 | `cpp/libs/modules/currency/system/currency_system.cpp:149` | 货币增减路径里 try_get 之后再 get_or_emplace PlayerCurrencyComp:非初始化路径悄悄补组件,掩盖缺失初始化 | 已修（第 1 批：增减路径只取不建，缺失拒绝） |
| F026 | low | 确认 | `cpp/libs/modules/mission/comp/mission_comp.h:19` | MissionsComp 继承 EventOwner:从不设置的实体字段 + 非 explicit 的 entity/id_type 隐式转换 | 已修（第 4/5 批：MissionsComp 去掉 EventOwner） |
| C001 | low | 确认 | `cpp/libs/modules/mission/system/mission.cpp:82` | 任务系统日志把 entt 句柄整数标成 "playerId" 输出（F089 的同类，而且标签更直接地冒充了玩家号） | 已修（第4批 4c-1：改用 ActorLog） |
| F029 | low | 确认 | `cpp/libs/modules/scene/comp/scene_comp.h:5` | SceneEntityComp 在 actorRegistry 组件里存 sceneRegistry 的裸 entt::entity,类型上无法区分属于哪个 registry | 已修（第5批 B3:SceneEntity 自定义实体类型）；审查 W2-b5_types-06 |
| F030 | low | 确认 | `cpp/libs/modules/transaction_log/anomaly_detector.h:90` | AnomalyDetector 用 thread_local map 按 entt::entity 建键做旁路表,自定义哈希把 64 位实体截成 32 位 | 已修（第 4/5 批：旁路表改组件，EntityHash 删）；审查 W1-38 |
| F032 | low | 确认 | `cpp/libs/modules/transaction_log/transaction_log_system.cpp:19` | 用基础类型别名 Guid(=uint64_t)当 ECS 组件类型:所有 uint64_t 组件共享同一个 pool | 已修（第 5 批 B1:PlayerGuidComp）；审查 W2-b5_types-01、W1-30 |
| F033 | low | 确认 | `cpp/libs/services/gate/session/comp/session_info_comp.h:24` | 会话用裸 uint64 存 entt 句柄,靠 UINT64_MAX 碰巧等于 entt::null,各处反复 entt::entity{int} 再自己做 valid 校验 | 已修（第4批 4c-4:SessionInfo 改存 entt::entity）；审查 W1-28、W1-29 |
| F034 | low | 确认 | `cpp/libs/services/scene/actor/action_state/system/actor_action_state.cpp:110` | 施法校验路径上 AddState 用 get_or_emplace<ActorStateComp>,且不校验实体 | 已修（第1批：AddState 先 valid，作唯一按需创建入口）；审查 W2-b1_runtime-04 |
| F035 | low | 确认 | `cpp/libs/services/scene/actor/attribute/system/actor_attribute_calculator.cpp:94` | 属性脏标记组件只加不删,每 tick 的 view 实际遍历全部 actor;循环体内还有多处 get_or_emplace | 已修（脏标记消费即摘，生成同步脏位也已改模板）；审查 W2-b1_runtime-05、W2-b1_runtime-09、W2-b5_types-05 |
| F036 | low | 降级 | `cpp/libs/services/scene/battle/system/player_battle.cpp:331` | 结算幂等账本的读取入口用 get_or_emplace,与资产账本的 fail-closed 约定不一致(目前不可达) | 已修（第 1 批：FindSettlementLedger 只取不建） |
| F037 | low | 确认 | `cpp/libs/services/scene/combat/buff/system/buff.cpp:123` | 战斗路径(加 buff、叠层判断)里用 get_or_emplace,违反「只在初始化用」的规则 | 已修（第 1 批：叠层只读，唯一写入口延迟创建）；审查 W2-b1_runtime-06 |
| F039 | low | 降级 | `cpp/libs/services/scene/combat/buff/system/buff.cpp:410` | OnAfterTakeDamage 把攻击者当受击者传给 ResetCombatIdleBuff;另外从 DamageEventComp.attacker_id 走 uint64 -> to_entity 往返取实体是多余的(该参数未使用,整条链目前不可达) | 已修（受击方改 targetEntity，去 to_entity 往返） |
| F042 | low | 确认 | `cpp/libs/services/scene/combat/skill/comp/skill_comp.h:11` | TimerTaskComp 的移动语义会取消定时器,“包含它的组件必须手写 in_place_delete = true”只靠约定维持,漏写一次就会悄悄取消别的实体的定时器 | 已修（第 5 批：TimerTaskComp 移动转移定时器）；审查 W1-38 |
| F045 | low | 确认 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:126` | 技能上下文被镜像到目标实体的 SkillContextCompMap,但施法者在施法或后摇期间被销毁时没有人回收,目标侧条目泄漏到目标下线为止 | 已修（第 5 批：删除目标侧镜像）；审查 W1-38、W2-b1_runtime-02 |
| F051 | low | 确认 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:491` | 施法路径里用 get_or_emplace 懒建四种定时器组件,CheckTimerPhase 每次施法又把它们 remove 掉 | 已修（第 1 批：相位开始 emplace、结束 remove）；审查 W2-b1_runtime-02 |
| F043 | low | 确认 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:61` | SkillContextCompMap 同时承担「我施放的技能」和「指向我的技能镜像」两种语义，shared_ptr 在两个实体的组件之间共享；并且生产代码从未 emplace 这个组件 | 部分修：BuffEntry.skillContext 仍共享 shared_ptr；审查 W2-fit-04、W1-38 |
| F052 | low | 确认 | `cpp/libs/services/scene/combat/skill/system/skill.cpp:648` | 进程内事件都用 protobuf 携带 uint64 形式的实体句柄,两种转换写法混用 | 部分修：SkillContext/DamageEventComp 仍存 uint64 句柄；审查 W1-27 |
| F053 | low | 降级 | `cpp/libs/services/scene/combat_state/system/combat_state.cpp:15` | Kafka SceneCommand 路由对 DispatchProtoEvent 不设白名单，带进程内 entt 句柄的事件(CombatStateAdded/BeforeLeaveScene 等)理论上可被跨进程派发;目前没有生产者 | 已修（第 4 批：CombatStateAdded 改结构体，不在白名单）；审查 W1-22、W1-23 |
| F054 | low | 降级 | `cpp/libs/services/scene/generated/attribute/actorbaseattributess2c_attribute_sync.cpp:31` | 生成器 attribute_sync 模板用原始 uint64_t 作为 entt 组件类型取 entity_id/player_id(与 Guid 同一个 pool,可读性差，日后新增 uint64 别名组件会静默撞 pool) | 已修（第 5 批 B1：模板改取 PlayerGuidComp 并已重生成）；审查 W1-30、W1-24 |
| F055 | low | 确认 | `cpp/libs/services/scene/generated/attribute/actorbaseattributess2c_attribute_sync.cpp:88` | 生成的 Set*AttrDirtyBit 用 get_or_emplace,在 MovementSystem 等 per-tick 视图循环里调用 | 已修（第 5 批 B1：按需写入口+同步后摘除掩码）；审查 W2-b5_types-05 |
| C004 | low | 确认 | `cpp/libs/services/scene/player/system/asset_op_system.cpp:527` | PlayerItemBlockList（单玩家 GM 物品封禁）只有读的地方、没有写的地方：5 个发放入口都 try_get 后回落到空表，封禁功能实际不存在 | 已修（删 PlayerItemBlockList;proto 残留见 W1-11）；审查 W1-11 |
| F058 | low | 确认 | `cpp/libs/services/scene/player/system/player_attribute.cpp:1070` | GmSetLevel 用 to_integral 把进程内的 entt::entity 塞进 protobuf 事件，而这个事件类型也能从 Kafka 反序列化后派发 | 已修（第 4 批：PlayerUpgradeEvent 改结构体）；审查 W1-22 |
| F057 | low | 确认 | `cpp/libs/services/scene/player/system/player_attribute.cpp:573` | EnsureComp 用 get_or_emplace，注释说只用于初始化，实际是所有读写路径的通用访问器；Recalculate 和 GmSetLevel 里也有 get_or_emplace | 已修（第1批：FindComp 只读，重算与 GM 调级不补建）；审查 W1-22 |
| F061 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2285` | EnterScene 是公开入口,却不做 valid() 校验,上来就无守卫 get<Guid>,随后连续 get_or_emplace | 已修（EnterScene 先 valid，再 try_get 取 PlayerGuidComp）；审查 W2-b1_runtime-08 |
| F063 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2457` | 进程内 dispatcher 事件用 protobuf 定义,实体被压成 uint64 再靠 namespace entt 里自定义的 to_entity 还原 | 已修（第 4 批：进程内事件改 C++ 结构体）；审查 W1-23 |
| F064 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2533` | 玩家维度的异常检测状态放在 ECS 外部、以 entt::entity 为键的 thread_local map 里,只能在 DestroyPlayer 手工清理 | 已修（第 5 批 A3:PlayerAnomalyWindowComp）；审查 W1-38 |
| F065 | low | 降级 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2547` | DestroyPlayer 的兜底只摘了 ScenePlayers,没清 AOI 格子和观察者兴趣表;现有销毁路径都会先走 DetachFromScene,这是一处没做完的纵深防御 | 已修（第 5 批 B2:DestroyPlayer 兜底改走 DetachFromScene）；审查 W1-34、W1-33、W1-23 |
| F066 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2549` | ScenePlayers 是存放在 sceneRegistry 里的 actorRegistry 实体集合,反向索引全靠各处手工同步 | 已修（第 5 批 B2：删除 ScenePlayers，成员以 SceneEntityComp 为准）；审查 W1-39 |
| F067 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2580` | try_get 拿到的组件指针跨越同步 dispatcher.trigger 和外部系统调用后继续解引用,目前安全只是因为监听者恰好不删组件 | 已修（第 5 批 B2:DetachFromScene 不再跨 trigger 持指针）；审查 W1-23 |
| F068 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2647` | 把基础类型或类型别名直接当组件类型用(Guid = uint64_t、ScenePlayers = unordered_set<entity>) | 已修（第 5 批 B1/B2:PlayerGuidComp，删除 ScenePlayers）；审查 W2-b5_types-01 |
| F069 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2716` | 注释对 entt 3.13.2 语义的理解与源码相反:安全的读操作被当成 UB 加了防护,真正危险的写操作反而没有防护 | 已修（第 5 批 B2：三处注释改为陈旧句柄安全口径）；审查 W1-35、W2-b5_types-03 |
| F070 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:3507` | InitPlayerFromAllData 先 create 实体再查 playerList,重复时把刚建的实体留成孤儿 | 已修（第1批：先查 playerList 再 create）；审查 W2-b1_runtime-09 |
| F071 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:3535` | 初始化事件用 proto 的 uint64 字段携带 entt::entity 整数,这些事件在生成的 DispatchProtoEvent 里也能被 Kafka SceneCommand 触发 | 已修（第 4 批：初始化事件改结构体）；审查 W1-23 |
| F072 | low | 降级 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:3536` | 新号默认技能只经 dispatcher 的 RegisterPlayerEvent 下发,必需的初始化依赖 sink 已注册;InitializePlayerCompsEvent 的任务初始化与反序列化路径重复 | 已修（第5批：直接调 RegisterPlayer，事件只通知）；审查 W1-23、W2-b1_runtime-08 |
| F073 | low | 确认 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:3573` | SavePlayerToRedisImpl 只做了 valid() 就直接 get<Guid>,和调用方 HandleExitGameNode 对缺 Guid 的容错不一致 | 已修（第1批：try_get<PlayerGuidComp> 守卫，缺则拒存）；审查 W1-01、W1-04 |
| F074 | low | 降级 | `cpp/libs/services/scene/player/system/player_pet.cpp:211` | PetSystem::EnsureComp 用 get_or_emplace 包住全部 14 个读写入口(含战斗快照/结算、只读面板),与『初始化路径』注释及项目 get_or_emplace 规则不符;当前加载路径保证组件先挂上,emplace 分支走不到,无实害 | 已修（第1批：EnsureComp 改为只读 FindComp）；审查 W2-b1_runtime-11 |
| F075 | low | 确认 | `cpp/libs/services/scene/spatial/system/aoi.cpp:169` | 每 tick 把 S2C 消息缓冲当成 global entity 上的组件,用 get_or_emplace 取用 | 已修（第1/2批：改为函数内 static thread_local 发送缓冲）；审查 W2-b2-07 |
| F076 | low | 确认 | `cpp/libs/services/scene/spatial/system/aoi.cpp:317` | 格子集合和兴趣表里存的是裸 entity 句柄,没有 on_destroy 钩子,只靠 BeforeLeaveScene 事件清理;Pin 后又 Unpin 降级、留在远处观察者表里的条目永远清不掉 | 已修（第5批 B4：离场做远端清扫，Unpin 立即驱逐）；审查 W1-33、W1-34、W1-37 |
| F077 | low | 降级 | `cpp/libs/services/scene/spatial/system/grid.cpp:57` | grid.cpp 空间查询对场景句柄做 get_or_emplace<SceneGridListComp>,且不先做 sceneRegistry.valid();SceneGridListComp 只在热路径上懒创建 | 已修（第1批：查询改 valid+try_get，仅入格写入口建表）；审查 W2-b5_types-03 |
| C006 | low | 降级 | `cpp/libs/services/scene/spatial/system/interest.cpp:71` | ScenePriorityPolicyComp / AoiClientCapacityComp / ScenePressureComp 在生产代码里没有创建点，try_get 恒回落到默认值（死代码，与设计文档不符）；由于生产只产生 kNormal/kTeammate，即便挂上策略组件，淘汰结果目前也完全相同 | 部分修：只加了代码注释，设计文档和组件头仍写建场景时挂 |
| F078 | low | 确认 | `cpp/libs/services/scene/spatial/system/interest.cpp:84` | AddAoiEntity 在每 tick 的 AOI 路径上 get_or_emplace<AoiListComp>,且不校验 watcher 是否存活 | 已修（第1批：先校验双方存活，插入首个条目时才建表） |
| F079 | low | 降级 | `cpp/libs/services/scene/spatial/system/scene_crowd.cpp:23` | SceneCrowdSystem::AfterEnterSceneHandler 拿 actorRegistry 的玩家句柄去 sceneRegistry 查 DtCrowdPtr(跨 registry 查询,目前是死代码) | 已修（第4/5批：删除 scene_crowd 和 AfterEnterScene，SceneEntity 分型） |
| F081 | low | 降级 | `cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp:21` | gate Kafka GateCommand 的 payload 分支走全局 DispatchProtoEvent,没有白名单;携带进程内 entt 句柄的 OnNodeRemoveEvent 也能被跨进程投递 | 已修（第 4 批：gate 白名单；OnNodeRemove 改结构体）；审查 W1-16、W1-27 |
| F083 | low | 确认 | `cpp/nodes/gate/handler/rpc/client_message_processor.cpp:182` | RpcClientSessionHandler 把带 *this 的成员监听器挂到 thread_local dispatcher 上,但从不 disconnect | 已修（第 5 批：gate 订阅改 scoped_connection） |
| F084 | low | 确认 | `cpp/nodes/gate/handler/rpc/client_message_processor.cpp:405` | 把 entt 实体整数当成 scene_node_id 打日志,而且截断到 uint32,只剩 index | 已修（第4批 4c-2:scene_node_id 改取 NodeInfo.node_id）；审查 W1-28 |
| F085 | low | 确认 | `cpp/nodes/gate/handler/rpc/client_message_processor.cpp:413` | gate 把 SceneNodeService 节点 registry 命名为 sceneRegistry,并把节点 entity 整数截成 uint32 当 scene_node_id 打日志 | 部分修：scene_entry_dispatch.cpp:97/392 仍叫 sceneRegistry；审查 W2-b5_types-06 |
| F082 | low | 确认 | `cpp/nodes/gate/handler/rpc/client_message_processor.cpp:57` | PickRandomNode 只按 view<NodeInfo> 选节点,生成的 sender 随后无守卫地 get<CompletionQueue>/get<StubPtr> | 已修（§8 收尾：PickRandomNode 要求可用通道） |
| C007 | low | 确认 | `cpp/nodes/gate/handler/rpc/gate_service_handler.cpp:193` | 另外几处入口把对端或跨进程消息里的 node_type 未经校验直接交给 NodeContextManager::GetRegistry(只有 assert),Release 下数组越界（F093 的同类） | 不再存在（RPC 通道重构删 Route 一族、握手按本端类型） |
| F086 | low | 确认 | `cpp/nodes/scene/handler/event/player_event_handler.cpp:32` | 进程内 entt 事件用 protobuf + uint64 entity 字段传句柄，类型安全和所属 registry 信息都丢了 | 已修（第 4 批：旧 handler 删，改 scene_local_events）；审查 W1-23 |
| F087 | low | 确认 | `cpp/nodes/scene/handler/grpc/scene_node_service.cpp:41` | HandleCreateScene 持有 SceneInfoComp& 跨过 dispatcher.trigger,靠「监听者不删场景」才不悬空 | 已修（第 1 批：引用限在 trigger 前，之后 try_get 重取）；审查 W1-36、W1-30、W2-cross_sem-01 |
| F088 | low | 降级 | `cpp/nodes/scene/handler/grpc/scene_node_service.cpp:56` | 组件引用/指针跨越同步 dispatcher.trigger 继续使用，只有订阅者删除同 pool 组件(swap_and_pop)时才会悬空;emplace 不会使其失效 | 已修（第 1 批：同 F087，trigger 后重取再回包）；审查 W1-36、W1-30、W2-cross_sem-01 |
| F089 | low | 确认 | `cpp/nodes/scene/handler/rpc/player/player_lifecycle_handler.cpp:28` | 日志用 entt::to_integral(player) 冒充玩家身份 | 已修（第4批 4c-1：改用 ActorLog） |
| F092 | low | 确认 | `cpp/nodes/scene/handler/rpc/scene_handler.cpp:556` | 把请求级单例（TipInfoMessage、SessionList）挂在 GlobalEntity 上，每条消息都 get_or_emplace | 已修（第 2 批：tip 直写应答，会话索引归 EcsContext）；审查 W2-b2-04 |
| F093 | low | 降级 | `cpp/nodes/scene/handler/rpc/scene_handler.cpp:808` | NodeContextManager::GetRegistry 只有 assert 边界检查，RoutePlayerStringMsg 用对端传来的 node_type 选 registry（Release 下可越界；当前仓内没有发起方） | 不再存在（RPC 通道重构批 7 删 Route 一族） |
| F094 | low | 降级 | `cpp/nodes/scene/handler/rpc/scene_handler.cpp:856` | UpdateSessionDetail 对可缺失的 PlayerSessionSnapshotComp 无守卫 get（当前无发送方）；472 行 LastActiveFrameComp 的 get 无实害 | 已修（第 1 批：try_get 并拒绝；LastActiveFrame 保留） |
| F095 | low | 确认 | `cpp/nodes/scene/handler/rpc/scene_handler.cpp:893` | scene_id→场景实体靠全表扫描 view<SceneInfoComp>，同一段查找复制了多份 | 部分修：scene_handler/scene_node_service 仍 4 份全表扫描；审查 W2-b5_types-02、W2-fit-02 |
| F096 | low | 降级 | `cpp/nodes/scene/handler/rpc/scene_handler.cpp:912` | CreateScene 持有 SceneInfoComp& 跨越同步 dispatcher.trigger（只有监听者删除 SceneInfoComp 时才会失效） | 已修（第 1 批：CreateScene 引用限块内，trigger 后重取）；审查 W1-36、W1-30、W2-cross_sem-01等 |
| F097 | low | 确认 | `proto/common/event/node_event.proto:19` | ConnectToNodeEvent.entity 是 uint32,发送端塞进的是 64 位 to_integral(entity),version 被截掉(潜在问题) | 已修（第 4 批：node_event.proto 删除） |
| F098 | low | 降级 | `tools/proto_generator/protogen/internal/generator/cpp/event_id.go:188` | DispatchProtoEvent 只在 Kafka 路由里调用,却把携带 entity 整数的进程内事件也收进了分发表,Kafka 入口没有事件白名单(纵深防御缺口) | 已修（第 4 批：生成器按标注拆两份派发）；审查 W1-16、W1-18、W1-19等 |
| F099 | low | 确认 | `tools/proto_generator/protogen/internal/template/attribute_sync.cpp.tmpl:33` | 把基础类型或 std 别名直接当组件类型(Guid=uint64_t、SessionList、ScenePlayers 等),生成器还靠 try_get<uint64_t> 恰好取到 Guid 池 | 部分修：节点表仍挂 RpcClientPtr/MuduoGrpcChannelPtr；审查 W2-b5_types-04 |
| C008 | low | 确认 | `tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl:92` | 生成的流式 gRPC 客户端把 registry 里 protobuf 响应组件的地址交给异步 Read，而这个组件的存储是 swap-and-pop，指针稳定只靠每个 registry 恰好只有一个实体挂它 | 不再存在（muduo 迁移后不再生成流式客户端） |

### 被复核推翻(8 条)

| ID | 严重度 | 结论 | 位置 | 问题 | HEAD 状态 |
|---|---|---|---|---|---|
| F031 | low | 推翻 | `cpp/libs/modules/transaction_log/anomaly_detector.h:107` | 异常检测的滑动窗口放在以 entt::entity 为键的 thread_local 外部表里,需要手动清理,而且窗口随实体重建而重置 | 原审计已推翻（窗口现为玩家组件）；审查 W1-38 |
| C002 | low | 推翻 | `cpp/libs/services/scene/battle/system/player_battle.cpp:1032` | BuffComp 施法者哨兵混用 0 与 entt::null，第一个玩家实体为 0 导致快照 caster 丢失 | 原审计已推翻（0 号句柄退役，caster 不写 null） |
| F038 | low | 推翻 | `cpp/libs/services/scene/combat/buff/system/buff.cpp:382` | OnIntervalThink 把 buffIt->second 依次传给三个回调,第一个回调可能已经通过驱散把该 buff 从表里删掉 | 原审计已推翻（现每路回调前按 id 重查） |
| F056 | low | 推翻 | `cpp/libs/services/scene/player/system/asset_op_system.cpp:226` | 资产 RPC 路径上用 get_or_emplace 懒建运行时限频组件 PlayerAssetOpPersistRequestComp | 原审计已推翻（写入口懒建符合访问规则） |
| F062 | low | 推翻 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2316` | get_or_emplace 用在非初始化路径(存盘回调、进场),其中两处与 Init 重复,会掩盖漏初始化 | 原审计已推翻（现代码中仍不成立） |
| C005 | low | 推翻 | `cpp/libs/services/scene/player/system/player_scene.cpp:183` | HandleEnterScene 用 try_get 拿到 SceneEntityComp* 和 SceneInfoComp* 后，跨过同步的 dispatcher.trigger(BeforeLeaveScene) 和外部系统调用继续解引用 | 原审计已推翻（现用 SceneInfo 值副本，不跨通知借用）；审查 W1-23、W1-34 |
| C009 | low | 推翻 | `tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl:265` | 生成的流式 gRPC 客户端把收发缓冲拆成一组松散组件挂在节点实体上(连裸 protobuf 类型也直接当组件)，异步 Read 的写入目标就是 entt 池里组件的地址 | 原审计已推翻（流式组件已随 muduo 迁移删除） |
| C010 | low | 推翻 | `tools/proto_generator/protogen/internal/template/grpc_init_total.cpp.tmpl:80` | CQ 泵在 view<grpc::CompletionQueue>().each() 里同步调用业务应答/失败处理器，同时握着当前节点实体 CQ 组件的引用；安全完全依赖“节点摘除一律 queueInLoop 延后”这条没写进契约的约定 | 原审计已推翻（CQ 泵已随 muduo 迁移删除） |
