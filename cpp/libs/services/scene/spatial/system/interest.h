#pragma once

#include <vector>

#include <entt/src/entt/entity/entity.hpp>
#include "engine/core/type_define/type_define.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/aoi_priority.h"

// 一批 AOI 写操作引起的客户端可见性变化,按发生顺序记录。
// InterestSystem 只记录;AoiSystem 求出每对(观察者, 目标)的净变化后组包下发,
// 所以同一批里"显示又隐藏"的对不会发出任何消息。
struct AoiVisibilityChanges
{
    struct Change
    {
        entt::entity watcher;
        entt::entity target;
        bool shown;  // true:需要给观察者发创建和当前位置;false:需要发一次移除
    };
    std::vector<Change> changes;

    [[nodiscard]] bool Empty() const { return changes.empty(); }
};

// AOI 关注关系的数据层:正向索引 AoiListComp(观察者身上)与反向索引 AoiWatchersComp(目标身上)
// 的唯一写入口,负责来源合并、容量与优先级淘汰、隐身可见条件。不发网络消息。
// 所有写函数的 changes 参数用来接收可见性变化;传空表示调用方不需要下发(只用于测试与纯数据场合),
// 生产路径经 AoiSystem 的入口调用,由它下发消息。
// 线程模型:只在所属场景线程上调用(tlsEcs)。复杂度只与该实体直接关联的关系数有关,不扫描全节点。
class InterestSystem {
public:
    // ── 空间来源(AoiSystem 的网格评估使用)──────────────────────────────────

    // 登记空间来源:网格评估认定 watcher 能看见 target。返回调用后 watcher 是否显示 target。
    // 已有关系时只补上空间来源并按权重升级标签(只升不降);新关系在容量满时
    // 只能淘汰权重更低的已显示条目,否则被拒且不建条目。
    // 前置条件:两个句柄都存活且不相同,否则返回 false 且不创建任何组件。
    static bool AddAoiEntity(entt::entity watcher, entt::entity target,
                             AoiPriority priority = AoiPriority::kNormal,
                             AoiVisibilityChanges* changes = nullptr);

    // 撤销空间来源。还有显式来源时关系保留并继续显示;否则删除关系,
    // 原先显示中的记一次移除,并把让出的名额补给暂隐的显式关注。
    static void RemoveAoiEntity(entt::entity watcher, entt::entity target,
                                AoiVisibilityChanges* changes = nullptr);

    // 升级空间来源的标签(条目不存在、没有空间来源或新标签权重不更高时静默返回)。
    static void UpgradePriority(entt::entity watcher, entt::entity target, AoiPriority priority);

    // 空间来源的标签恰好等于 |from| 时才改成 |to|;其余情况静默返回。
    // 专供"语义标签失效"的回退使用(例:离队后 kTeammate -> kNormal,team-system.md §F.4)。
    // 只认标签不认权重;显式来源的权重登记在各自的 AoiFollowGrant 里,不受影响。
    static void DowngradePriority(entt::entity watcher, entt::entity target, AoiPriority from, AoiPriority to);

    // ── 显式来源(玩法使用,生产路径经 AoiSystem::Follow / Unfollow)───────────

    // 登记一个显式来源。target 须是与 watcher 同场景的存活实体(可以在网格范围外)。
    // 同一来源重复登记只更新其权重与侦测授权,不产生重复关系。
    // 返回关系是否已登记:已登记但暂不可见(目标隐身且无侦测授权、容量被更高权重占满)时也返回 true,
    // 条件满足后自动显示。
    static bool Follow(entt::entity watcher, entt::entity target, const AoiFollowGrant& grant,
                       AoiVisibilityChanges* changes = nullptr);

    // 撤销一个显式来源,只撤调用方指定的这一份。还有其他来源时继续显示;
    // 最后一个来源撤销时,空间来源仍在则继续显示,否则删除关系并记一次移除。
    static void Unfollow(entt::entity watcher, entt::entity target, const AoiFollowSource& source,
                         AoiVisibilityChanges* changes = nullptr);

    // ── 可见条件变化与离场 ──────────────────────────────────────────────────

    // 目标隐身状态变化后,按反向索引重判它的每个观察者:不再满足条件的暂停显示
    // (空间来源一并撤掉,显式来源保留),重新满足条件的暂隐显式关注恢复显示。
    // 隐身结束后附近实体的空间来源由 AoiSystem::RefreshTargetVisibility 按网格重新评估。
    static void RefreshTargetVisibility(entt::entity target, AoiVisibilityChanges* changes = nullptr);

    // 离场、换场景或销毁前调用:清掉 entity 的全部出边和入边。
    // 关注它且正在显示它的观察者各记一次移除;entity 自己的客户端不记(离场后整体重建)。
    // 只访问两张索引里与它直接关联的关系;可重复调用,之后任何索引都不再引用该句柄。
    static void ClearRelations(entt::entity entity, AoiVisibilityChanges* changes = nullptr);

    // ── 只读查询 ────────────────────────────────────────────────────────────

    // 追加当前显示 target 的存活观察者(反向索引里可见的那些)。移动、属性与技能广播的接收者来源。
    static void GetVisibleWatchers(entt::entity target, EntityVector& out);

    // Effective AOI list capacity for |watcher|:
    //   min(client-reported desired count, server pressure-adjusted ceiling).
    // Falls back to kAoiListCapacityDefault when components are absent.
    static std::size_t GetEffectiveCapacity(entt::entity watcher);

    // Resolve the priority policy active for |watcher|'s scene.
    // Returns kPolicyOpenWorld when the scene has no explicit policy.
    static const AoiPriorityPolicy& GetPriorityPolicy(entt::entity watcher);
};
