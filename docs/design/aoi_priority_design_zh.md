# AOI 优先级、容量与可见性设计

**参考文献**: [如何实现一个强大的MMO技能系统——AOI (kasan)](https://zhuanlan.zhihu.com/p/148077453)

**后续需求**：[AOI 单向关注与范围外同步](aoi-directed-interest.md)（待实现）。本文继续记录现有优先级、容量与可见性规则。

## 核心原则

> 网格只是手段，关注者列表才是结果。

AOI 空间查询（六边形网格邻居）填充的是**关注列表（Interest List）**，
但关注列表也可以被技能、Buff 和其他游戏系统直接操控。
客户端显示的是关注列表中的内容——而非原始网格数据。

## 优先级标签与策略权重

优先级分为两个层次：
1. **语义标签** (`AoiPriority` 枚举) — 实体所属的分类。
2. **优先级策略** (`AoiPriorityPolicy`) — 场景级别的权重表，为每个标签分配一个淘汰权重。

### 标签定义

| 标签 | 值 | 来源 |
|------|-----|------|
| `kNormal` | 0 | 默认：通过空间网格查询发现的实体 |
| `kTeammate` | 1 | 同队：拥有相同的 `TeamId` 组件 |
| `kAttacker` | 2 | 攻击者：来自战斗/仇恨系统（TODO：接入） |
| `kQuestNpc` | 3 | 任务NPC：由任务系统标记（TODO：接入） |
| `kBoss` | 4 | Boss / 精英怪 |
| `kPinned` | 5 | 预留锁定标签，权重恒为 255；玩法尚未接入 |

### 内置策略

| 策略 | 权重顺序（低 → 高） |
|------|---------------------|
| `kPolicyOpenWorld`（开放世界） | normal(0) < teammate(1) < attacker(2) < questNpc(3) < boss(4) < pinned(255) |
| `kPolicyDungeon`（副本） | normal(0) < questNpc(1) < teammate(2) < attacker(3) < boss(4) < pinned(255) |
| `kPolicyPvpArena`（PvP 竞技场） | normal(0) < questNpc(1) < teammate(2) < boss(3) < attacker(4) < pinned(255) |

场景在创建时附加 `ScenePriorityPolicyComp` 来选择使用哪种策略。
`kPinned` 在所有策略中始终为权重 255 —— 它**永远不会**被空间逻辑淘汰。

### 淘汰规则

容量只数**正在显示**的条目（`AoiListComp::visibleCount`）。一个条目的权重取它的空间标签与各显式来源标签中最高的一个。当显示名额已满、又有关系需要显示时，比较它与已显示条目中**最低权重**者的**策略权重**：
- 新关系权重 > 最低条目权重 → 淘汰最低条目，显示新关系。被淘汰者只剩空间来源时整条删除，下次跨格重新评估；带显式来源时转为暂不显示，关系保留。
- 否则：新的空间关系被拒，不建条目；新的显式关注照常登记，暂不显示。

淘汰只改本观察者的表，并只通知本观察者的客户端移除一次；目标那一侧的关系各自独立，不要求互相占位。观察者让出名额后（空间离开、撤销关注、目标离场），按权重补上满足可见条件的暂隐显式关注，并发送创建与当前位置。

## 动态容量

容量不再是编译期常量，而是按实体、每帧动态计算：

```
effectiveCapacity = min(clientReportedCapacity, serverPressureCapacity)
```

### 相关组件

| 组件 | 作用域 | 用途 |
|------|--------|------|
| `AoiClientCapacityComp` | 单个实体（角色） | 客户端上报的期望显示数量 |
| `ScenePressureComp` | 单个场景 | 服务器压力因子 → 派生容量上限 |

### 常量

| 常量 | 值 | 用途 |
|------|-----|------|
| `kAoiListCapacityDefault` | 100 | 缺省值：没有容量组件时使用 |
| `kAoiListCapacityMin` | 20 | 下限：即使最大压力也不低于此值 |
| `kAoiListCapacityMax` | 200 | 上限：客户端不能请求超过此值 |

### 服务器压力

`ScenePressureComp.pressureFactor` ∈ [0, 1]：
- 0 = 空闲 → 服务器容量 = `kAoiListCapacityMax`（200）
- 1 = 最大压力 → 服务器容量 = `kAoiListCapacityMin`（20）
- 中间值线性插值。

场景的 tick/负载监控器每帧设置 `pressureFactor`（集成 TODO）。

### 客户端上报数量

客户端通过设置 RPC 发送期望显示数量，服务器存入
`AoiClientCapacityComp.clientDesiredCount`，并钳制到
[`kAoiListCapacityMin`, `kAoiListCapacityMax`] 范围。

### 最终计算

`InterestSystem::GetEffectiveCapacity(watcher)` 返回
`min(clamped_client, server_pressure_cap)`。

## 显式关注

需求与验收见 [AOI 单向关注与范围外同步](aoi-directed-interest.md)。原先预留的 Pin/Unpin 已由显式关注取代：

- 玩法调用 `AoiSystem::Follow(watcher, target, grant)` 建立关注，`AoiSystem::Unfollow(watcher, target, source)` 撤销。`target` 须与 `watcher` 同场景，可以在网格范围外。
- `AoiFollowGrant` 写明来源身份（`AoiFollowSource` = 种类 + 实例号，如队伍号、Buff 实例号）、淘汰权重标签和是否授予侦测隐身。同一来源重复登记只更新这一份；撤销只撤调用方指定的来源。
- 一条关系可以同时有空间来源和多个显式来源，客户端只有一份可见状态。空间离开只撤空间来源；最后一个来源消失时才通知客户端移除一次。
- `kPinned` 仍是最高权重标签，供技能追踪这类不能被挤掉的显式关注使用；空间判定不再特殊对待它。

## 隐身可见性

`ViewSystem::CanSee(observer, target)` 用于空间关注的进入检查：

1. 距离检查。
2. 目标带 `StealthedTagComp`（`kBuffTypeStealth` Buff 维护）时不可见，**除非**观察者对它的某个显式来源授予了侦测能力（`AoiFollowGrant::revealsStealth`）。普通关注、队伍关系和空间邻近都不能看穿隐身。

隐身标签从无到有、从有到无时，`BuffSystem` 调用 `AoiSystem::RefreshTargetVisibility`：

- 隐身开始：按反向索引重判它的观察者。没有侦测授权的停止投递并收到一次移除；空间来源一并撤掉，显式来源保留为暂不显示。
- 隐身结束：暂不显示的显式关注恢复显示；当前格及邻格里能看见它的实体立即重新建立空间关注，不等任何一方跨格。两者都先收到带最新位置的创建消息。

## 方向性与投递

关注关系是**单向的**：A 关注 B 不代表 B 关注 A。两张索引描述同一批有向关系，只由 `InterestSystem` 写：

| 索引 | 所在实体 | 内容 |
|------|----------|------|
| `AoiListComp`（正向） | 观察者 | 目标 → `AoiEntry`（空间来源、显式来源、是否显示） |
| `AoiWatchersComp`（反向） | 目标 | 观察者 → 是否显示 |

目标的移动（`MovementSystem::BroadcastMove`）、属性同步（`ActorStateAttributeSyncSystem`）和技能广播（`ViewSystem::BroadcastMessageToVisiblePlayers`）都发给反向索引里正在显示它、且有网关会话的观察者。接收与否只看对方是否关注并显示了它，与它是否看得见对方无关。视野半径外的显式关注者按最低档接收属性增量。

离场、换场景、销毁统一经 `AoiSystem::OnLeaveScene`：按两张索引清掉该实体的全部出边与入边，正在显示它的观察者各收一次 `ActorDestroyS2C`；只访问与它直接关联的关系，不扫描全节点，也不依赖 `Hex`。

## 关键 API

| 方法 | 说明 |
|------|------|
| `AoiSystem::Follow(watcher, target, grant)` | 玩法建立显式关注，首次可见时下发创建与当前位置 |
| `AoiSystem::Unfollow(watcher, target, source)` | 撤销一个显式来源，最后一个来源消失且不在附近时下发一次移除 |
| `AoiSystem::RefreshTargetVisibility(target)` | 隐身状态变化后重判观察者 |
| `AoiSystem::OnLeaveScene(entity)` | 离场、换场景、销毁前按正反索引清理全部关系 |
| `InterestSystem::AddAoiEntity(watcher, target, priority, changes)` | 登记空间来源；新关系按策略权重与动态容量决定显示或拒绝 |
| `InterestSystem::RemoveAoiEntity(watcher, target, changes)` | 撤销空间来源；显式来源仍在时继续显示 |
| `InterestSystem::UpgradePriority` / `DowngradePriority` | 调整空间来源的标签（队伍刷新使用），不影响显式来源 |
| `InterestSystem::Follow` / `Unfollow` / `ClearRelations` | 数据层写入口，可见性变化写进 `AoiVisibilityChanges`，由 `AoiSystem` 下发 |
| `InterestSystem::GetVisibleWatchers(target, out)` | 正在显示 target 的观察者，移动、属性、技能广播的接收者 |
| `InterestSystem::GetEffectiveCapacity(watcher)` | 动态解析有效容量 |
| `InterestSystem::GetPriorityPolicy(watcher)` | 获取场景的策略（默认：开放世界） |
| `ViewSystem::CanSee(observer, target)` | 空间可见判定：距离 + 隐身（侦测授权例外） |
| `ViewSystem::IsStealthed(entity)` | 检查是否有隐身 Buff |

## 相关文件

| 文件 | 变更内容 |
|------|----------|
| `spatial/constants/aoi_priority.h` | 优先级标签、`AoiPriorityPolicy` 结构体、内置策略、动态容量常量 |
| `spatial/comp/scene_node_scene_comp.h` | `AoiEntry`、`AoiListComp`（正向）、`AoiWatchersComp`（反向）、`AoiFollowSource` / `AoiFollowGrant`、容量与策略组件 |
| `spatial/system/interest.h/.cpp` | 两张索引的唯一写者：来源合并、容量淘汰与补位、隐身可见条件、离场清理 |
| `spatial/system/aoi.h/.cpp` | 网格评估、显式关注入口、可见性变化的净化与组包下发 |
| `spatial/system/view.h/.cpp` | `CanSee()`、`IsStealthed()`、技能广播接收者 |
| `spatial/system/movement.cpp` | 移动广播接收者、同场景瞬移后给显式关注者补发新位置 |
| `actor/attribute/system/actor_state_attribute_sync.cpp` | 属性同步接收者改为反向索引中的可见观察者 |
| `combat/buff/system/buff.cpp` | 隐身标签变化时调用 `RefreshTargetVisibility` |
| `tests/aoi_test/*`、`tests/aoi_delivery_test/*` | 优先级、容量、索引一致性、队伍刷新；消息级的单向投递、来源生命周期、隐身、离场与淘汰 |

## 后续计划

- **攻击者优先级**：接入战斗/伤害系统，受击时调用 `InterestSystem::UpgradePriority(target, attacker, kAttacker)`。
- **任务NPC优先级**：接受任务时调用 `InterestSystem::UpgradePriority(player, npc, kQuestNpc)`。
- **Boss 标记**：场景创建时标记 Boss 实体；网格进入时检测并分配 `kBoss` 标签。
- **压力集成**：场景 tick 监控器根据实体数量 / CPU 负载写入 `ScenePressureComp.pressureFactor`。
- **客户端设置 RPC**：将客户端上报的显示数量接入 `AoiClientCapacityComp`。
- **滞后/缓冲期**：为在网格边界反复振荡的实体添加一个宽限期，避免频繁添加/移除。只作用于空间来源，不能绑到显式关注的存续上。
- **玩法接入显式关注**：技能追踪（鹰眼术）、任务目标、切磋对象等按各自来源调用 `AoiSystem::Follow` / `Unfollow`；队伍需要视野外同步时由队伍逻辑分别建立两个方向的关注。
- **自定义策略**：从配置表数据驱动加载策略。

## 2026-10-01 周围玩家邀请验收：双向视野通知

联机验收发现：新入场者能看到静止玩家，静止玩家的服务器兴趣表也已登记新入场者，但客户端没有收到 ActorListCreate；反向移出时同样缺 ActorListDestroy。原因是 HandleEntityVisibility 只给当前跨格实体发通知，反向关系仅改兴趣表。

修复在反向 AddAoiEntity 成功且原先不可见时立即通知对应观察者，反向移除非 pinned 条目后同步通知销毁。两方向仍独立遵循 CanSee、容量和 pinned 规则；稳定视野不反复发送。

回归在 cpp/tests/aoi_delivery_test，直接编译生产 AOI/Grid/Interest/View，仅在网络发送边界截获收件人和序列化消息。正式入口：`pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter aoi_delivery_test`。覆盖静止观察者收到后来者、移出双向销毁、隐身、容量拒收、pinned 不销毁五项。2026-10-01 原实现 2 项失败、3 项通过，修复及正式入口均 5/5 通过。真实 Scene 完整构建与联网验收结果另见当日 PROGRESS 和组队邀请验收记录。

## 2026-11-05 反向视野通知按观察者集合广播

空间候选范围为本格与六个相邻格，共七格。`HandleEntityVisibility` 仍逐方向维护关注关系：反向进入必须满足 `CanSee`、容量允许且原先不可见；反向离开只移除已有的非 `kPinned` 关系。符合条件的观察者分别汇总后，经 `BroadcastToPlayerClients` 发送同一个实体的创建或销毁消息，底层按 Gate 合并收件人并沿用身份栅栏。不能把整片候选网格直接作为广播收件人。

`BroadcastEntityVisibilityChanges` 统一处理反向通知和 `ResetEntityVisibility` 的旧位置清理，保留原有 `ActorListCreateS2C`、`ActorListDestroyS2C` 消息、消息号与 actor 网络编号。空收件集合不发送，缺少网络编号时记录错误并跳过。实体自身看到的多个对象增删，继续由 `NotifyEntityVisibilityChanges` 聚合成列表后单发；正式离场的 `BroadcastEntityLeave` 继续使用已有广播入口。

回归在网络边界同时记录广播调用、收件集合和序列化消息，覆盖多人进入、满容量拒收、多人离开、无关注关系、`kPinned` 保留、稳定帧不重复，以及重置后下一帧重新建立视野。Java `0.1.0-SNAPSHOT` 对应进入、离开和瞬移路径已有按 Gate 链路分组广播，本次无需新增客户端契约或 Java 功能。

### 广播函数的提取取舍

**决定（2026-11-05）**：保留 `BroadcastEntityVisibilityChanges`，暂不继续提取内部创建、销毁两个短分支。该函数封装同一实体向观察者投递可见性变化的完整职责，集中处理空收件集合、网络编号校验及消息构造，并由普通网格移动和瞬移清理共同复用，具有实际的复用价值。

两个分支的控制流形状相似，但业务规则不同：进入需要构造实体创建数据，离开只需携带实体网络编号。DRY 针对需要共同维护的业务知识；相似的 `Clear`、填充和广播语句本身不足以支持一个通用抽象。当前各分支都短而直白，再拆一层会增加阅读跳转；用布尔开关、消息号、模板或回调拼成通用组包函数，还会增加调用者必须了解的细节。

未来的特殊逻辑先在对应职责中演进。出现下列实际需求时，重新评估分别提取进入通知、离开通知两个私有函数：

- 某个分支形成可独立命名、独立变化的业务规则。
- 分支内部细节已经遮住外层的进入、离开发送流程，提取后能够降低阅读负担。
- 其他调用点确实需要复用其中一段完整行为。

函数行数和未来可能出现的特殊化都不单独作为提取依据。消息数据的特殊填充仍由相应组包职责承接，不把领域字段规则扩散到通用发送模块；任何拆分继续保持现有收件人筛选、同次调用的发送顺序、身份栅栏、EventLoop 线程及同步序列化约束。

共享消息体广播的前提是各观察者收到的内容相同。若特殊逻辑使消息内容因观察者而异，应按相同内容分组广播或分别投递，不能把个性化数据混入共享消息体；逐观察者的可见性与容量判定仍须保留。

## 2026-11-05 单向关注、反向索引与范围外同步

按 [AOI 单向关注与范围外同步](aoi-directed-interest.md) 落码（未编译，待 Codex 验证）。正文「淘汰规则」「显式关注」「隐身可见性」「方向性与投递」「关键 API」已按新实现改写，要点：

- 新增反向索引 `AoiWatchersComp`，`AoiEntry` 记录空间来源、显式来源与显示状态。移动、属性、技能广播改按反向索引投递，修掉了 [team-system.md](team-system.md) V.2 记录的缺口：对方能看见我、但我的表因容量已满没收下对方时，对方收不到我的移动。
- 上两节提到的 `BroadcastEntityVisibilityChanges`、`NotifyEntityVisibilityChanges`、`BroadcastEntityLeave` 合并为 `AoiSystem::SendVisibilityChanges`：先求每对（观察者，目标）的净变化，再按"单目标按目标合并广播、多目标单发列表"组包，消息号与消息体不变。离场者的移除仍是 `ActorDestroyS2C`，但只发给正在显示它的观察者，不再发给七格内全部实体。
- 容量淘汰此前静默删除条目、客户端留下不再更新的残影；现在被淘汰者通知观察者本人一次。
- 隐身此前只在跨格时生效，已在显示的观察者会一直收到隐身者的坐标；现在隐身开始与结束都立即重判。
- 内存：每个条目多一个空的 `std::vector<AoiFollowGrant>` 和两个布尔，约增加 24～32 字节；反向索引与正向条目一一对应。没有压测数据，上线前随 AOI 压测复核。
