# AOI 单向关注与范围外同步

状态：服务端已落码（2026-11-05），未编译，待 Codex 按文末「验证」执行；Java 版待做。网格、容量和优先级规则见 [AOI 优先级、容量与可见性设计](aoi_priority_design_zh.md)。

## 前言

本项目的 AOI 关注机制采用 kasan 在知乎文章[《如何实现一个强大的 MMO 技能系统——AOI》](https://zhuanlan.zhihu.com/p/148077453)里提出的做法：空间查询负责发现候选对象，实际关注关系决定客户端显示谁、接收谁的动态。原文用微博举例：A 关注 B，A 就能收到 B 的更新；B 是否关注 A，是另一件事。另一篇由负责人提供的参考：[知乎专栏文章（147681650）](https://zhuanlan.zhihu.com/p/147681650)。

比如任务允许玩家通过鹰眼术追踪远处目标：技能生效时，玩法把该目标加入玩家的关注列表；效果结束时，只撤销鹰眼术加上的这条关注。即使目标离得很远，玩家也能持续收到它的移动；如果效果结束时目标仍在附近且满足可见条件，客户端继续显示它。好友、队友、任务 NPC、切磋对象或其他玩法指定的实体，都可以使用同一机制。

## 关注关系

- **空间关注**来自同场景的六边形网格查询（本格及相邻六格），按现有规则筛选可见对象并计算优先级。网格候选可以按场景节奏刷新；附近列表的刷新频率与已关注目标的移动发送频率分开。
- **显式关注**由服务端玩法按需建立和撤销。目标为任意符合条件的同场景实体，范围外也可以关注。玩法负责目标授权和关注时机；AOI 不负责查询好友关系，也不自动关注全部好友。队伍要互相同步时，由队伍逻辑分别建立两个方向的关注。
- 同一观察者和目标之间可以同时有空间关注和多个显式来源。每个显式来源有自己的身份，例如队伍、任务或技能实例；重复添加同一来源不会产生重复关系。取消关注只撤销调用方指定的来源。
- 关注按有向关系维护。观察者的正向索引用于查询自己关注的目标，目标的反向索引用于找到它的观察者。两个索引描述同一批关系，创建、撤销、淘汰、离场和销毁时都要保持一致。

## 可见与同步

首次有效可见时，先发创建和当前位置，再接收后续移动。目标每次发布移动时，按反向索引找到当前有效且可见的观察者。目标或观察者跨格时，只更新空间来源；显式关注仍有效就继续同步。同一个目标只有一份客户端可见状态，不能因为同时来自网格、队伍和技能就重复创建或重复发送。

距离、隐身和容量是不同的条件。显式关注允许超出网格距离，普通关注仍遵守隐身规则；只有明确具备侦测能力的玩法才能授权看见隐身目标。隐藏期间暂停投递；如果业务来源还在，恢复可见后先发送当前状态。容量沿用现有优先级策略，已有关注的有效性和淘汰通知按每个观察者独立判断，不要求互相占位。

取消一个来源后，只要还有其他有效来源，就继续同步。最后一个有效来源消失或因隐身、淘汰而不再可见时，通知客户端移除一次。空间边界有明显抖动时，可以单独评估候选刷新或短暂延迟移除，不能把边界行为绑到显式关注的存续上。

## 离场和销毁

玩家、NPC 或其他实体离场、换场景或销毁时，从该实体的两张索引清理所有入边和出边，并通知仍需收到移除消息的观察者。清理只访问与该实体直接关联的关系，不遍历全节点的 `AoiListComp`，也不依赖实体仍有网格组件。清理应可重复调用，旧实体关系不能泄漏给复用的实体句柄。需要在新场景继续关注的关系，由玩法在双方重新满足场景条件后建立。

## 验收场景

| 场景 | 预期结果 |
| --- | --- |
| A 在邻格外关注 NPC B 或玩家 B | A 先收到 B 的创建和当前位置，再连续收到 B 的移动；B 不因此收到 A 的移动 |
| B 进出 A 的邻格，显式来源未变 | A 的客户端持续显示 B，不重复创建或移除 |
| A 同时因队伍与技能关注 B | 结束技能不影响队伍来源；最后一个来源撤销时，只发送一次移除 |
| A 解除对 B 的最后一个显式关注 | B 仍在附近且可见时继续显示，否则立即移除 |
| B 隐身，随后恢复可见 | 普通关注期间暂停投递；恢复时用最新状态重新创建。侦测能力按玩法授权判断 |
| B 在范围外离场或销毁，且网格组件已清 | A 收到一次移除；双方索引清干净，其他实体的关系不受影响 |

## 实现

### 数据结构

两张索引描述同一批有向关系，只由 [`InterestSystem`](../../cpp/libs/services/scene/spatial/system/interest.h) 写，定义在 [`scene_node_scene_comp.h`](../../cpp/libs/services/scene/spatial/comp/scene_node_scene_comp.h)：

| 组件 | 所在实体 | 内容 | 创建 |
| --- | --- | --- | --- |
| `AoiListComp`（正向） | 观察者 | 目标 → `AoiEntry`；`visibleCount` 为已占用的容量 | 第一次建立关系时（空间来源被接纳或显式关注登记） |
| `AoiWatchersComp`（反向） | 目标 | 观察者 → 是否显示 | 第一次有人关注它时 |

`AoiEntry` 记录空间来源（`spatial` 与空间标签 `priority`）、显式来源列表 `follows` 和显示状态 `visible`。显式来源 `AoiFollowGrant` 由身份 `AoiFollowSource`（种类 `AoiFollowKind` + 实例号，如 `team_id`、`buff_id`）、淘汰权重标签和侦测授权 `revealsStealth` 组成。两个组件都是按需状态，不入库。

不变量：条目存在当且仅当有空间来源或显式来源；有空间来源必然在显示；在显示必然满足可见条件（未隐身，或某个显式来源授予侦测）并占一个名额；正向条目与反向记录一一对应，显示状态一致。所有改动经 `interest.cpp` 里的插入、删除、显示、隐藏四个原语完成。

### 入口

| 入口 | 用途 |
| --- | --- |
| `AoiSystem::Follow(watcher, target, grant)` / `Unfollow(watcher, target, source)` | 玩法建立和撤销显式关注，必要时立即下发创建或移除 |
| `AoiSystem::RefreshTargetVisibility(target)` | `BuffSystem` 在隐身标签出现、消失时调用 |
| `AoiSystem::OnLeaveScene(entity)` | 离场、换场景、销毁前的唯一 AOI 清理入口；`DestroyPlayer` 兜底补调 `LeaveCurrentScene` |
| `AoiSystem::Update` | 网格评估，只增删空间来源 |
| `InterestSystem::GetVisibleWatchers(target, out)` | 移动、属性、技能广播的接收者 |

`InterestSystem` 的写函数把可见性变化按发生顺序写进 `AoiVisibilityChanges`。`AoiSystem::SendVisibilityChanges` 求出每对（观察者，目标）的净变化，同一批里显示又隐藏的不发；只涉及一个目标的观察者按目标合并成一次广播，涉及多个目标的收一条列表消息；离场者的移除用 `ActorDestroyS2C`。消息号、消息体与客户端契约不变。

### 投递

- 移动（`MovementSystem::BroadcastMove`）、属性同步（`ActorStateAttributeSyncSystem`）和技能广播（`ViewSystem::BroadcastMessageToVisiblePlayers`）都发给反向索引里正在显示目标、且有网关会话的观察者。视野半径外的显式关注者按最低档接收属性增量。
- 同场景瞬移只撤空间来源；`TeleportWithinScene` 随后补发一次移动广播，让仍在显示它的显式关注者立即拿到新位置。
- 队伍逻辑没有改：队友在视野内仍按空间标签 `kTeammate` 提高优先级（[team-system.md](team-system.md) §F.4），视野外不同步。需要视野外同步时，由队伍逻辑按两个方向分别 `Follow`，来源种类用 `AoiFollowKind::kTeam`。

### 代价

- 离场清理只访问该实体的出边与入边，复杂度与它的关系数成正比，不扫描全节点。
- 补位只在观察者有暂不显示的显式关注时才扫描它自己的表；多数观察者一条都没有，直接返回。
- 每个条目多一个空的 `std::vector` 和两个布尔，约增加 24～32 字节，反向索引与正向条目一一对应。没有压测数据，上线前随 AOI 压测复核。

### 验证

未编译。交给 Codex，在仓库根目录用 VS 2026 Developer PowerShell 串行执行：

1. 先编库：`msbuild cpp/libs/modules/modules.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`，再 `msbuild cpp/libs/services/scene/scene.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`。测试工程链接预编好的库，改了库不先编库就会跑旧代码。
2. 测试：`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter aoi_test`、`-Filter aoi_delivery_test`、`-Filter cross_zone_test`、`-Filter buff_test`、`-Filter skill_test`。通过标准为全部用例通过，新增用例见 `aoi_delivery_test.cpp` 与 `aoi_system_test.cpp` 中「显式关注」「正反索引一致性」相关用例。
3. 编 Scene 节点，确认 `actor_state_attribute_sync.cpp`、`buff.cpp`、`movement.cpp` 链接无误。

旧 `Pin/Unpin` 的删除见[第六批修复记录](../handoff/ecs-refactor/batch6-review-fixes.md) T3.4，那次删掉的是只在离场时全节点扫描的远端清理；现在由反向索引承担同一职责。
