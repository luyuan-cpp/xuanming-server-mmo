# ECS Component Access Rules

## Overview

entt uses a **sparse-set** storage model. Each component type has its own packed array. Entities that do not have a component use **zero memory** in that pool and are never visited by `view<T>` iteration. This is a core design advantage for instances/dungeons where entities intentionally omit components (AI, bag, buff, skills) to save memory and CPU.

## 哪些数据进入 registry

- 进入：大量同构实体、需要 `view` 按组件组合批处理的数据，或需按实体 O(1) 挂载并随实体销毁回收的数据。
- 不进入：进程单例、请求级数据、连接与网络资源、跨实体共享对象、私有索引。
- registry 内的单例只允许是该 registry 的派生索引，放 `ctx()`（现例 `ActorNetIdIndex`）。
- 多个 registry 用自定义实体类型区分（现例 `SceneEntity`）。
- 派生索引只有一个维护入口（`ActorNetIdIndex` 只由 `actor_net_id` 的 Assign 与 on_destroy 维护）。
- dispatcher 判据见下文：没有消费者不发事件，dispatcher 只做可有可无的一对多通知，必需步骤直接调用。
- 现存例外：节点 registry 上的 `RpcClientPtr`、`MuduoGrpcChannelPtr` 属存量，迁出见 [审查报告](../handoff/ecs-refactor/review-report-20261015.md) §4.2。

## Three Access Patterns

| API | Behavior | Use When |
|-----|----------|----------|
| `get<T>(entity)` | Asserts existence; **crashes** if absent | Inside `view<T,...>` iteration, or after `any_of<T>` / `all_of<T>` guard |
| `try_get<T>(entity)` | Returns `nullptr` when absent | Cross-entity lookups, optional components, any entity whose archetype is not guaranteed |
| `get_or_emplace<T>(entity)` | Silently **creates default** if absent | 只在两处:实体创建点,以及「按需状态」组件的写入口(见下节)；按需写入口调用前须确认 `valid(entity)` |

## 组件的三种初始化方式(2026-10-15 起,取代旧规则「get_or_emplace 只许在初始化时用」)

**延迟初始化是本仓库有意采用的做法**:功能没被用到时,实体上就没有它的组件——不占内存,也不会被 `view` 遍历到(上面 sparse-set 的优势)。所以规则不是「禁止延迟」,而是**按组件类别决定在哪里创建、读的时候拿不到意味着什么**。

旧规则把延迟初始化整个禁掉,实际效果反而更糟:有人把按需组件的**写入口**也从 `get_or_emplace` 改成 `try_get`,于是再也没人创建它,读路径 `try_get` 拿不到就静默放行——技能冷却和技能上下文因此整条链空转(审计 F044/F050)。与此同时,真正危险的「存盘时补默认值」却被列在旧版的「可接受用法」里(审计 F059)。

| 类别 | 判据 | 典型组件 | 在哪里创建 | 读路径拿不到时 |
|---|---|---|---|---|
| **持久数据** | 会写进 DB / Redis | `CurrencyComp`、`PlayerCurrencyComp`、资产与结算账本、`LevelComp`、`BaseAttributesComp`、`PlayerPetComp`、背包、任务、`PlayerItemGainBlockComp` | **只在加载时**(`*MessageFieldsUnmarshal` / `InitPlayerFromAllData`) | 不变量被破坏:`LOG_ERROR` + fail-closed,**绝不补默认值**(`PlayerItemGainBlockComp` 的读路径例外见下文「单玩家物品获取封禁」一段) |
| **按需状态**(延迟初始化) | 「没有」本身有明确含义:空、未激活、无冷却、不脏 | `CooldownTimeListComp`、`SkillContextCompMap`、`AoiListComp`、`SceneGridListComp`、施法阶段定时器、属性脏标记 | **该功能写入口第一次写时**,集中在一个命名函数里;结束时可以 `remove` | 正常分支:当作空 / 未激活处理,不是错误 |
| **派生运行时数据** | 由其他组件算出来,实体存在期间必须有 | `DerivedAttributesComp`、`CombatStateFlagsComp` | 加载 / 创建流程直接调用初始化函数,在首个通知前创建并计算;后续重算只更新 | `LOG_ERROR` + 跳过本次处理;有返回码的请求明确失败 |

硬规则:

1. **读路径不得改变实体的组件集合。** 查询、存盘、快照、面板、日志、每 tick 的只读计算都不得补建组件；可选或跨实体查询用 `try_get` / `all_of` / `any_of`，已确认句柄有效且组件存在（或由 `view` 保证）时可以直接 `get`。存盘时补默认值,会把零值写进 DB。
2. **`get_or_emplace` 只出现在两处**:创建点,以及按需状态组件的写入口。写入口加一行注释说明「此处延迟创建」。
3. **一个按需组件只有一个延迟创建入口。** 其他写路径要么经过它,要么在它之后执行。**不要把写入口改成 `try_get`**,那样就再也没人创建它,功能会静默失效。
4. **持久数据绝不延迟创建。** 缺了说明加载有问题,补零值等于凭空造账。
5. **校验前置。** 先确认需要的组件齐全,再做副作用,不要做到一半才发现缺组件。

### 第 1 批核对后的具体创建入口

以下按需组件保留延迟初始化,不在 actor / scene 创建时预挂空表。读取缺失都按空或未激活处理,不会借查询创建组件。

| 按需组件 | 第一次创建的时机与入口 | 结束 / 消费 |
|---|---|---|
| `CooldownTimeListComp` | `SkillSystem::StartCooldown`,真正启动冷却时 | 冷却查询按时间判定;无表表示无冷却 |
| `SkillContextCompMap` | `SkillSystem::AddSkillContext`,登记合法施法的施法者上下文时 | `HandleSkillFinish` 删除本次上下文;目标不保存镜像 |
| `ActorStateComp` | `ActorActionStateSystem::AddState`,首次激活合法状态时 | 状态移除;无表表示无激活状态 |
| `BuffListComp` | `BuffSystem::AddOrUpdateBuff`,完成驱散 / 免疫等判断后首次真正插入时 | 帧末摘除空列表 |
| `AoiListComp` | `InterestSystem::AddAoiEntity`(空间来源被接纳)或 `InterestSystem::Follow`(显式关注登记),完成实体 / 容量 / 同场景校验后首次建立关系时 | 移除观察关系;无表表示无观察对象 |
| `AoiWatchersComp` | `InterestSystem` 第一次登记以该实体为目标的关系时;与观察者的 `AoiListComp` 条目一一对应 | 关系删除时同步摘除;无表表示没有观察者 |
| `SceneGridListComp` | `AoiSystem::UpdateGridState`,首次写入场景格子时 | 场景销毁时随实体销毁;空场景可无表 |
| `CastingTimerComp` | `SetupCastingTimer`,进入前摇时 | 前摇结束 / 打断时摘除 |
| `RecoveryTimerComp` | `SkillSystem::HandleSkillRecovery`,进入后摇时 | 后摇结束 / 打断时摘除 |
| `ChannelFinishTimerComp`、`ChannelIntervalTimerComp` | `SkillSystem::HandleChannelSkillSpell`,开始引导时 | 引导结束 / 打断时一起摘除 |
| `AttributeDirtyFlagsComp` | `ActorAttributeCalculatorSystem::MarkAttributeForUpdate`,首次标脏时 | 重算消费后摘除;冻结时保留待处理 |
| `PlayerAnomalyWindowComp` | `AnomalyDetector::WindowForWrite`,合法玩家首次记收益时 | 实体销毁自然清理;查询不创建,无效句柄不记录 |
| 生成的各 `*DirtyMaskComp` | `Set*AttrDirtyBit`,先校验位范围与实体再标脏 | 有接收者时消费后摘除;接收者为空保留 |
| `BattleFreezeExpiredComp`(`player_battle.cpp` 内部,单槽 battle_id) | `RememberExpiredFightingBattle`,reaper 判定 FIGHTING 到期的收尾 `ReleaseExpiredFightingFreeze` 里、条件删锁之前(2026-10-19) | 不摘:下一次 FIGHTING 到期覆盖;实体销毁自然清理。按锁重建的两个入口只读它 |
| `BattleLoginRecheckComp`(`player_battle.cpp` 内部空标记) | `MarkLoginRecheck`,登录钩子读挂起结算 / 读战斗锁没问到时(空应答、读记录 ERROR、命令没发出去、Redis 不可用;2026-10-16) | reaper 的 `RunLoginRechecks` 在 Redis 可用时摘除并重跑登录检查,又没问到就重新挂上;实体销毁自然清理 |
| `BattleResultAwaitingCommitComp`(`player_battle.cpp` 内部,待公开的 battle_id) | `RecordBattleResult`,`ApplySettlementToEntity` 把本人结果写进近期结果的同一帧(2026-11-05,[战斗结果重登恢复](battle-result-recovery.md) §4.3) | `CompleteInboxSettlement` 在存档确认提交之后摘对应 battle_id,空了摘组件;结果被淘汰时一并摘。查询与确认只读它;缺失 = 没有待公开的结果。同功能的持久组件 `PlayerBattleResultComp` 属持久数据,只在加载时创建 |
| `PlayerConnectionLostComp`(`connection_lost_hold.h`) | `PlayerLifecycleSystem::HandleClientConnectionLost`,gate 报连接意外断开、判定可以保留时(先 valid;2026-11-05,[断线保留](player-connection-lost-hold.md) §4) | 新会话接回(`EnterScene` 第 0.1 步 `ResumeConnectionLostHold`)、保留到期(`ExpireConnectionLostHolds`)、任何退出发起(`HandleExitGameNode` 入口)时摘;实体销毁自然清理。读路径只用 `try_get` / `any_of`,缺失 = 连接正常或不在保留中 |
| `SceneOwnershipCheckComp`(scene 实体,`world/scene_ownership_reconciler.h`) | `scene_ownership::CollectChecks`,归属对账第一次核对到这个场景实体时(2026-11-06,[场景归属对账](scene-node-ownership-reconcile.md) §4.2) | CreateScene 按号命中已有实体时 `SceneLifecycle::ReuseExistingScene` 摘掉、下一轮重新登记;实体销毁自然清理。应答回来与指标采集只用 `try_get` / 视图读,缺失 = 还没核对过 |

`PlayerMirrorCreateInFlightComp` 是按需状态。由 `RequestEnterMirrorScene` 发起的 `CreateMirrorAndEnter` 任务在发送之前登记，恢复后按令牌认领。`IsSceneChangeBusy` 只读它，缺失或过 TTL 表示没有建镜像在途。过期、UTC 回拨或本地停止只摘本次令牌并丢弃结果。普通换图、交接、退出提交或权威落点成功时也主动摘掉旧镜像组件，永久取消旧请求的提示和自动进场资格。

`CombatStateCollectionComp`、`InBattleComp` 也属于按需状态:分别在实际添加战斗状态、进入战斗或从权威战斗锁恢复时创建；`InBattleComp` 的三处业务入口各自先确认不存在、再 `emplace`；查询缺失表示无对应状态。不要把状态存在与派生投影混为一谈:`CombatStateFlagsComp` 是创建 actor 时就应存在的同步投影。

`DerivedAttributesComp` 只由 `PlayerAttributeSystem::InitializeOnLoad` 创建并重算；`ActorAttributeCalculatorSystem::InitializeOnCreate` 在新建实体时调用一次，只挂空的 `CombatStateFlagsComp`。`InitPlayerFromAllData` 在反序列化之后直接调用该入口，不依赖事件订阅；未来新增 NPC / 怪物创建入口也必须直接调用。

默认技能、任务 scope、派生投影由加载链直接完成，不发初始化通知。后续重算缺少必需派生组件时跳过,不能临时补一个零值组件。面板 / 列表若缺少实际使用的持久输入,返回域内既有故障码;空的已加载组件才代表“没有数据”。

缺组件拒存不是已落盘证据：退出、异步比较和交接不得据此宣称收敛，不得写 A1′、交接标记或发改派。拒存原因都是永久的，保留的实体永远存不了盘，所以退出流程遇到拒存时用 `DestroyDeposedPlayer` 不存盘丢弃，玩家重登时从最后一次完好落盘重载。具体实施与未验证项见 [第 1 批 Codex 回执](../handoff/ecs-refactor/batch1-codex-receipt.md)。

每处组件的现状与修复进度见 [ecs-entt-misuse-audit-20261015.md](ecs-entt-misuse-audit-20261015.md)。

## 会话路由索引的所有者

`SessionMap()` 返回 `EcsContext::sessionPlayers` 普通成员，保存 gate 会话号到玩家号的映射，不注册为组件。它与 `playerList` 同属节点主 EventLoop，由 `EcsContext::Clear()` 一并清理；独立清理组件池不影响该索引。其他线程拥有各自的上下文，不能借此跨线程访问节点的在线状态。

节点身份由 `Node::localNodeInfo_` 持有，`GetNodeInfo()` 只读已安装的身份，未安装返回 protobuf 默认实例；地址发布不使 protobuf 变成线程安全对象，内容修改与 owner 寿命仍由节点 EventLoop 保证。服务发现快照由 `ServiceDiscoveryManager` 持有。请求拒绝码直接写本次 response。原 `EcsContext::globalRegistry` / `nodeGlobalRegistry` 及惰性 getter 已从源码删除。

## 实体类型与生命周期边界

| 数据 | 权威存储与身份 | 访问边界 |
|---|---|---|
| 场景中的 actor | `actorRegistry`，`entt::entity` | 玩家业务身份用 `PlayerGuidComp.value`，日志用 `ActorLog` |
| 场景实例 | `SceneRegistry = entt::basic_registry<SceneEntity>` | `SceneEntityComp`、瞬移等待与场景事件均用 `SceneEntity`，不与 actor 混传 |
| 客户端 actor 编号 | `ActorNetIdComp` + registry ctx 内的私有反查 | 创建时 `Assign`，出站 `Of`，入站 `Resolve`；不透明、非 0、不落库，本进程不复用 |
| 背包物品实例 | `ItemStore` 的 `Guid -> ItemComp` 字典 | 不再使用私有 registry，不暴露物品实体句柄 |
| 节点路由句柄 | 对应节点类型 registry 的 `entt::entity` | `SessionInfo::SetNodeEntity/GetNodeEntity` 保留版本位，缺失用 `entt::null`；节点 registry 是待移出的存量，不得再往节点实体上新增组件种类 |

进程内 dispatcher 事件是模块内的普通结构体，字段明确所属 registry。Kafka 只能进入显式标注的 proto 事件，详见 [控制面事件入口](control-plane-topic-partitioning-20260908.md#2026-10-15-事件入口边界第-4-批源码方案)。同一业务同时需要两种传播时，在网络入口把业务 ID 解析成本地句柄再通知。

没有消费者不发事件；dispatcher 只做可有可无的一对多通知，必需步骤直接调用；监听者不得销毁或替换实体，不得改 `SceneEntityComp`、`playerList` 或会话；`trigger` 之后不写恢复分支。必要的 AOI 离场清理由离场入口直接调用（`AoiSystem::OnLeaveScene`），按正向 `AoiListComp` 与反向 `AoiWatchersComp` 清掉该实体的全部出边和入边，只访问与它直接关联的关系，不扫描全节点，也不依赖 `Hex`。场景成员只以 actor 上的 `SceneEntityComp` 为准，排空时扫描玩家，不另维护反向集合。

`SceneEntityComp` 保存非拥有的带版本场景句柄，不保活场景，也不会随场景销毁自动清空。业务上的同场景判定必须确认 `sceneRegistry.valid()`，不能只比较两个非空句柄；技能在施法预检和延迟回调入口都重验施法者场景，失效时拒绝或终止该次施法。`SceneLifecycle::DestroyScene` 发起排空后按全部 `SceneEntityComp` 的剩余绑定数决定能否销毁；改派票据数不是居民数，断线或身份不完整的绑定同样阻止销毁。

### 可直接用于陈旧句柄

| 操作 | 句柄与借用约束 |
|---|---|
| `valid` | 只检查实体池中的完整句柄与版本，不创建数据 |
| `all_of<T>` / `any_of<T>` / `try_get<T>` / `storage.contains` | 按完整句柄与版本检查组件归属；缺失按三类规则处理，不创建 |
| `remove<T>` | 内部先 `contains`，陈旧句柄不删除同槽位活实体的组件；swap-and-pop 可能搬移其它实体的该组件，原借用不能继续用 |

### 必须先 valid()

| 操作 | 句柄与借用约束 |
|---|---|
| `emplace<T>` / `emplace_or_replace<T>` / `get_or_emplace<T>` | `try_emplace` 不看版本，Release 下覆盖同槽位活实体的稀疏映射；槽位空闲时把组件挂到死句柄上，Debug 也不报错 |
| `get<T>` / `patch<T>` / `replace<T>` | `index()` 在 Release 下忽略版本，读写同槽位活实体的组件或越界；还须确认组件存在，或由 `view<T>` 保证 |
| `erase<T>` / `storage.erase` | 按 index 删除同槽位活实体的组件，还须确认组件存在 |
| `destroy` | 陈旧句柄可能按 index 释放已复用的活实体，不能当作幂等空操作；actor / 节点使用带 `valid` 保护的 [DestroyEntity](../../cpp/libs/engine/core/utils/registry/game_registry.cpp)，`SceneRegistry` 没有对应重载，须手写 `valid` |

`TimerTaskComp` 的调度闭包只弱持有内部 State，移动转移定时器，复制得到空定时器；回调开火时保活 State。外层 owner 若有业务回调捕获 `this`，该 owner 必须禁止移动。包装组件可保留 `in_place_delete` 维持指针稳定，但不再靠它保证定时器正确性。

新增组件使用有业务含义的独立类型，不能用基础类型或通用 std 容器别名共享池。已有 `BuffListComp`、`SkillContextCompMap`、`SceneGridListComp` 已改为具名类型，不提供从基类容器到组件类型的转换（继承构造天然排除基类的拷贝 / 移动构造）。传输侧 `RpcClientPtr`、`MuduoGrpcChannelPtr` 属于明确存量边界，不能据此新增同类业务组件。完整替换按 [gRPC 迁移验收](grpc-muduo-transport.md#101-来自-entt-审计的验收条件) 处理，源码 / 生成 / 运行状态以任务总索引为准。

内部 SkillContext/Buff 暂仍以 uint64 本地句柄及 0 表示空来源，因此保留 actor 的 0 号句柄退役兼容。该历史约束不再承担客户端编号职责。AOI 容量、压力和策略组件缺失时采用既定默认值，`Npc` 标签保留给未来生成入口。

单玩家物品获取封禁(2026-11-05,[收件箱设计](player-inbox-service.md) §2.2)是持久数据 `PlayerItemGainBlockComp`(`player_database.item_gain_block`):唯一创建点是加载,存盘缺它整份拒存;唯一写入方是收件箱 GM 域执行器,经 `GainBlockService::BlockPlayerItemGain / UnblockPlayerItemGain` 写,缺组件即拒绝、不补建。各物品获得入口只经 `GainBlockService::IsItemGainBlocked` 读它。**读路径是持久数据「读缺失即 fail-closed」的一处有记录的例外**:无效句柄只判全服,有效实体缺组件按「没有单人封禁」回答、不补建。理由与代价见收件箱设计 §2.2「读缺失的口径」:`BagService` 接受没有玩家上下文的容器级调用;缺组件的实体存盘被整份拒绝、资产通道与收件箱只在落盘后终结,发出的物品成不了盘上事实。2026-10-15 删除的旧 `PlayerItemBlockList` 及 `blockList` 参数没有恢复。

上述规则包含第 6 批审查修复；生成产物与运行验证状态以 [任务总索引](../handoff/ecs-refactor/README.md) 及本批回执为准，不能用静态阅读代替构建或测试证据。

## `std::optional` Return Pattern

Functions that depend on optional components should return `std::optional<T>`:

```cpp
// GOOD: caller decides how to handle missing Transform
std::optional<double> GetDistanceBetweenEntities(entt::entity e1, entt::entity e2)
{
    const auto *t1 = registry.try_get<Transform>(e1);
    const auto *t2 = registry.try_get<Transform>(e2);
    if (!t1 || !t2) return std::nullopt;
    return ComputeDistance(*t1, *t2);
}

// BAD: crashes if either entity lacks Transform
double GetDistanceBetweenEntities(entt::entity e1, entt::entity e2)
{
    const auto &t1 = registry.get<Transform>(e1); // assert crash
    const auto &t2 = registry.get<Transform>(e2); // assert crash
    return ComputeDistance(t1, t2);
}
```

## Dungeon/Instance Memory Savings

Entities that intentionally omit components:
- Use **zero memory** in those component pools (sparse-set design)
- Are **never iterated** by `view<T>` — zero CPU cost
- Keep **packed arrays dense** — better cache line utilization for entities that do have the component

Example: 200 NPC桩体 in a dungeon with only `Transform` + `Npc` tag, no AI/bag/buff/skills → each saves ~10 components × dozens to hundreds of bytes each.
