#include "combat_engagement.h"

#include <algorithm>
#include <utility>
#include <vector>

#include "combat/engagement/comp/combat_engagement_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/component/npc_comp.pb.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/system/aoi.h"
#include "thread_context/ecs_context.h"

namespace
{
// 交手来源的身份:每对观察者与目标最多一份,id 恒为 0。敌对、治疗、一起打怪共用这一份。
constexpr AoiFollowSource kCombatSource{AoiFollowKind::kCombat, 0};

bool IsNpc(entt::entity entity)
{
    return tlsEcs.actorRegistry.any_of<Npc>(entity);
}

// 调用方保证两个句柄都存活。
bool InSameScene(entt::entity lhs, entt::entity rhs)
{
    const auto* lhsScene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(lhs);
    const auto* rhsScene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(rhs);
    return lhsScene != nullptr && rhsScene != nullptr && lhsScene->sceneEntity == rhsScene->sceneEntity &&
           tlsEcs.sceneRegistry.valid(lhsScene->sceneEntity);
}

// 两个实体此刻能不能算交手:不是同一个、都存活、在同一场景。
bool CanEngage(entt::entity lhs, entt::entity rhs)
{
    return lhs != rhs && tlsEcs.actorRegistry.valid(lhs) && tlsEcs.actorRegistry.valid(rhs) && InSameScene(lhs, rhs);
}

// watcher 一侧登记或续期对 other 的交手记录,并确保 watcher 显式关注 other。
// Follow 对同一来源幂等:续期时不重复创建;万一关注被离场清理撤过,这里顺手补回。
void EngageOneSide(entt::entity watcher, entt::entity other, bool hostile)
{
    // 此处延迟创建:CombatEngagementComp 的唯一创建点,调用方已确认 watcher 存活。
    auto& record = tlsEcs.actorRegistry.get_or_emplace<CombatEngagementComp>(watcher).records[other];
    record.secondsUntilDisengage = kCombatDisengageSeconds;
    record.hostile = record.hostile || hostile;
    AoiSystem::Follow(watcher, other, AoiFollowGrant{kCombatSource, AoiPriority::kCombat, false});
}

// 一对实体两边一起登记或续期。调用方已用 CanEngage 确认。
void Engage(entt::entity lhs, entt::entity rhs, bool hostile)
{
    EngageOneSide(lhs, rhs, hostile);
    EngageOneSide(rhs, lhs, hostile);
}

// fighter 刚和 npc 敌对交手:fighter 进(或已在)npc 的仇恨名单,并与名单里的其他人续期友方交手。
// 名单已满、fighter 又不在名单里时什么都不做,等有人离开名单后它下一次出手再补进来。
void ShareFight(entt::entity npc, entt::entity fighter)
{
    auto* roster = tlsEcs.actorRegistry.try_get<CombatAllyRosterComp>(npc);
    const bool member = roster != nullptr && std::ranges::find(roster->members, fighter) != roster->members.end();
    if (!member)
    {
        if (roster != nullptr && roster->members.size() >= kMaxCombatAllyRoster)
        {
            return;
        }
        // 此处延迟创建:CombatAllyRosterComp 的唯一创建点,调用方已确认 npc 存活。
        roster = &tlsEcs.actorRegistry.get_or_emplace<CombatAllyRosterComp>(npc);
        roster->members.push_back(fighter);
    }
    // Engage 只改交手记录与 AOI 关注,不改仇恨名单,可以边遍历边调。
    for (const auto ally : roster->members)
    {
        if (ally != fighter && CanEngage(fighter, ally))
        {
            Engage(fighter, ally, false);
        }
    }
}

// fighter 离开 npc 的仇恨名单;npc 已无效、没有名单或 fighter 不在名单里时无操作。名单空了摘除组件。
// 名单里其他人与 fighter 的友方交手不在这里撤销:它们各自计时,不再一起打就按脱战时间结束。
void LeaveRoster(entt::entity npc, entt::entity fighter)
{
    if (!tlsEcs.actorRegistry.valid(npc))
    {
        return;
    }
    auto* roster = tlsEcs.actorRegistry.try_get<CombatAllyRosterComp>(npc);
    if (roster == nullptr)
    {
        return;
    }
    std::erase(roster->members, fighter);
    if (roster->members.empty())
    {
        tlsEcs.actorRegistry.remove<CombatAllyRosterComp>(npc);
    }
}

// 结束 watcher 一侧对 other 的交手记录:摘记录,撤销战斗关注;最后一份记录结束时摘除组件。
// watcher 已无效时什么都不用做:它的组件随实体销毁,AOI 关系在离场时已清。
void EndOneSide(entt::entity watcher, entt::entity other)
{
    if (!tlsEcs.actorRegistry.valid(watcher))
    {
        return;
    }
    auto* engagement = tlsEcs.actorRegistry.try_get<CombatEngagementComp>(watcher);
    if (engagement == nullptr || engagement->records.erase(other) == 0)
    {
        return;
    }
    if (engagement->records.empty())
    {
        tlsEcs.actorRegistry.remove<CombatEngagementComp>(watcher);
    }
    AoiSystem::Unfollow(watcher, other, kCombatSource);
}

// 一对实体的交手关系两边一起结束;若一方因此进过另一方(NPC)的仇恨名单,一并移出。
void EndEngagement(entt::entity lhs, entt::entity rhs)
{
    LeaveRoster(lhs, rhs);
    LeaveRoster(rhs, lhs);
    EndOneSide(lhs, rhs);
    EndOneSide(rhs, lhs);
}
} // namespace

void CombatEngagementSystem::RecordDamage(entt::entity attacker, entt::entity target)
{
    if (!CanEngage(attacker, target))
    {
        return;
    }
    Engage(attacker, target, true);

    const bool attackerIsNpc = IsNpc(attacker);
    const bool targetIsNpc = IsNpc(target);
    if (targetIsNpc && !attackerIsNpc)
    {
        ShareFight(target, attacker);
    }
    else if (attackerIsNpc && !targetIsNpc)
    {
        ShareFight(attacker, target);
    }
}

void CombatEngagementSystem::RecordHeal(entt::entity healer, entt::entity target)
{
    if (!CanEngage(healer, target))
    {
        return;
    }
    Engage(healer, target, false);

    // 治疗仇恨只对 NPC 敌人推:NPC 之间互相治疗没有阵营规则,先不推。
    if (IsNpc(healer))
    {
        return;
    }
    // 先拷名单:下面的 Engage / ShareFight 会续期 target 自己的记录表。
    EntityVector hostileNpcs;
    if (const auto* engagement = tlsEcs.actorRegistry.try_get<CombatEngagementComp>(target))
    {
        for (const auto& [other, record] : engagement->records)
        {
            if (record.hostile && other != healer && IsNpc(other))
            {
                hostileNpcs.push_back(other);
            }
        }
    }
    for (const auto npc : hostileNpcs)
    {
        if (CanEngage(healer, npc))
        {
            Engage(healer, npc, true);
            ShareFight(npc, healer);
        }
    }
}

void CombatEngagementSystem::Update(double delta)
{
    // 先收集再结束:结束会摘记录、摘组件,改到正在遍历的这个 view。
    std::vector<std::pair<entt::entity, entt::entity>> ended;
    for (auto&& [entity, engagement] : tlsEcs.actorRegistry.view<CombatEngagementComp>().each())
    {
        for (auto& [other, record] : engagement.records)
        {
            record.secondsUntilDisengage -= delta;
            if (record.secondsUntilDisengage <= 0 || !tlsEcs.actorRegistry.valid(other) || !InSameScene(entity, other))
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
    others.reserve(engagement->records.size());
    for (const auto& [other, record] : engagement->records)
    {
        others.push_back(other);
    }
    for (const auto other : others)
    {
        EndEngagement(entity, other);
    }
}
