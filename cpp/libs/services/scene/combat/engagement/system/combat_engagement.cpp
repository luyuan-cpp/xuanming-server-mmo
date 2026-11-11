#include "combat_engagement.h"

#include <utility>
#include <vector>

#include "combat/engagement/comp/combat_engagement_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/scene/comp/scene_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/system/aoi.h"
#include "thread_context/ecs_context.h"

namespace
{
// 交手来源的身份:每对观察者与目标最多一份,id 恒为 0。
constexpr AoiFollowSource kCombatSource{AoiFollowKind::kCombat, 0};

// 调用方保证两个句柄都存活。
bool InSameScene(entt::entity lhs, entt::entity rhs)
{
    const auto* lhsScene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(lhs);
    const auto* rhsScene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(rhs);
    return lhsScene != nullptr && rhsScene != nullptr && lhsScene->sceneEntity == rhsScene->sceneEntity &&
           tlsEcs.sceneRegistry.valid(lhsScene->sceneEntity);
}

// watcher 一侧登记或续期对 other 的交手关系,并确保 watcher 显式关注 other。
// Follow 对同一来源幂等:续期时不重复创建;万一关注被离场清理撤过,这里顺手补回。
void Engage(entt::entity watcher, entt::entity other)
{
    // 此处延迟创建:CombatEngagementComp 的唯一创建点,调用方已确认 watcher 存活。
    auto& engagement = tlsEcs.actorRegistry.get_or_emplace<CombatEngagementComp>(watcher);
    engagement.secondsUntilDisengage[other] = kCombatDisengageSeconds;
    AoiSystem::Follow(watcher, other, AoiFollowGrant{kCombatSource, AoiPriority::kCombat, false});
}

// 结束 watcher 一侧对 other 的交手关系:摘记录,撤销战斗关注;最后一份记录结束时摘除组件。
// watcher 已无效时什么都不用做:它的组件随实体销毁,AOI 关系在离场时已清。
void EndOneSide(entt::entity watcher, entt::entity other)
{
    if (!tlsEcs.actorRegistry.valid(watcher))
    {
        return;
    }
    auto* engagement = tlsEcs.actorRegistry.try_get<CombatEngagementComp>(watcher);
    if (engagement == nullptr || engagement->secondsUntilDisengage.erase(other) == 0)
    {
        return;
    }
    const bool lastOne = engagement->secondsUntilDisengage.empty();
    if (lastOne)
    {
        tlsEcs.actorRegistry.remove<CombatEngagementComp>(watcher);
    }
    AoiSystem::Unfollow(watcher, other, kCombatSource);
}

// 一对实体的交手关系两边一起结束。
void EndEngagement(entt::entity lhs, entt::entity rhs)
{
    EndOneSide(lhs, rhs);
    EndOneSide(rhs, lhs);
}
} // namespace

void CombatEngagementSystem::RecordInteraction(entt::entity source, entt::entity target)
{
    if (source == target || !tlsEcs.actorRegistry.valid(source) || !tlsEcs.actorRegistry.valid(target) ||
        !InSameScene(source, target))
    {
        return;
    }
    Engage(source, target);
    Engage(target, source);
}

void CombatEngagementSystem::Update(double delta)
{
    // 先收集再结束:结束会摘记录、摘组件,改到正在遍历的这个 view。
    std::vector<std::pair<entt::entity, entt::entity>> ended;
    for (auto&& [entity, engagement] : tlsEcs.actorRegistry.view<CombatEngagementComp>().each())
    {
        for (auto& [other, secondsLeft] : engagement.secondsUntilDisengage)
        {
            secondsLeft -= delta;
            if (secondsLeft <= 0 || !tlsEcs.actorRegistry.valid(other) || !InSameScene(entity, other))
            {
                ended.emplace_back(entity, other);
            }
        }
    }
    // 镜像的那一份也在名单里,第二次结束时记录已不在,是无操作。
    for (const auto& [entity, other] : ended)
    {
        EndEngagement(entity, other);
    }
}

void CombatEngagementSystem::OnDeath(entt::entity entity)
{
    const auto* engagement = tlsEcs.actorRegistry.try_get<CombatEngagementComp>(entity);
    if (engagement == nullptr)
    {
        return;
    }
    // 先拷名单:结束会摘掉这份组件。
    EntityVector others;
    others.reserve(engagement->secondsUntilDisengage.size());
    for (const auto& [other, secondsLeft] : engagement->secondsUntilDisengage)
    {
        others.push_back(other);
    }
    for (const auto other : others)
    {
        EndEngagement(entity, other);
    }
}
