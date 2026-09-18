#include "interest.h"

#include <algorithm>
#include <entt/src/entt/entity/entity.hpp>

#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/aoi_priority.h"

#include <modules/scene/comp/scene_comp.h>
#include <thread_context/ecs_context.h>

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// Find the entry with the lowest policy weight.  On tie, pick any.
static entt::entity FindLowestWeightEntity(const AoiListComp& comp,
                                           const AoiPriorityPolicy& policy)
{
    entt::entity worst = entt::null;
    uint8_t worstWeight = 255; // start high so anything is lower

    for (const auto& [entity, entry] : comp.entries)
    {
        const auto w = policy.GetWeight(entry.priority);
        if (w < worstWeight)
        {
            worstWeight = w;
            worst = entity;
        }
    }
    return worst;
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

std::size_t InterestSystem::GetEffectiveCapacity(entt::entity watcher)
{
    // Client-reported desired count (per-entity).
    std::size_t clientCap = kAoiListCapacityDefault;
    if (const auto* clientComp = tlsEcs.actorRegistry.try_get<AoiClientCapacityComp>(watcher))
    {
        clientCap = std::clamp(clientComp->clientDesiredCount,
                               kAoiListCapacityMin, kAoiListCapacityMax);
    }

    // Server pressure-adjusted ceiling (per-scene).
    std::size_t serverCap = kAoiListCapacityMax;
    if (const auto* sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(watcher))
    {
        if (tlsEcs.sceneRegistry.valid(sceneComp->sceneEntity))
        {
            if (const auto* pressure = tlsEcs.sceneRegistry.try_get<ScenePressureComp>(sceneComp->sceneEntity))
            {
                serverCap = pressure->GetServerCapacity();
            }
        }
    }

    return std::min(clientCap, serverCap);
}

const AoiPriorityPolicy& InterestSystem::GetPriorityPolicy(entt::entity watcher)
{
    if (const auto* sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(watcher))
    {
        if (tlsEcs.sceneRegistry.valid(sceneComp->sceneEntity))
        {
            if (const auto* policyComp = tlsEcs.sceneRegistry.try_get<ScenePriorityPolicyComp>(sceneComp->sceneEntity))
            {
                return PolicyOf(policyComp->policy);
            }
        }
    }
    return kPolicyOpenWorld;
}

bool InterestSystem::AddAoiEntity(entt::entity watcher, entt::entity target, AoiPriority priority)
{
    if (watcher == entt::null || target == entt::null) return false;

    auto& comp = tlsEcs.actorRegistry.get_or_emplace<AoiListComp>(watcher);
    const auto& policy = GetPriorityPolicy(watcher);

    // If target already present, upgrade priority if the new tag has higher weight.
    if (auto it = comp.entries.find(target); it != comp.entries.end())
    {
        if (policy.GetWeight(priority) > policy.GetWeight(it->second.priority))
        {
            it->second.priority = priority;
        }
        return true;
    }

    // Room available — insert directly.
    const auto capacity = GetEffectiveCapacity(watcher);
    if (comp.entries.size() < capacity)
    {
        comp.entries.emplace(target, AoiEntry{priority});
        return true;
    }

    // At capacity: try to evict the lowest-weight entry if new one outranks it.
    const auto lowestEntity = FindLowestWeightEntity(comp, policy);
    if (lowestEntity == entt::null) return false;

    const auto lowestWeight = policy.GetWeight(comp.entries.at(lowestEntity).priority);
    if (policy.GetWeight(priority) <= lowestWeight) return false; // not important enough

    comp.entries.erase(lowestEntity);
    comp.entries.emplace(target, AoiEntry{priority});
    return true;
}

void InterestSystem::RemoveAoiEntity(entt::entity watcher, entt::entity target)
{
    if (watcher == entt::null || target == entt::null) return;

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (comp == nullptr) return;
    comp->entries.erase(target);
}

bool InterestSystem::PinAoiEntity(entt::entity watcher, entt::entity target)
{
    return AddAoiEntity(watcher, target, AoiPriority::kPinned);
}

void InterestSystem::UnpinAoiEntity(entt::entity watcher, entt::entity target)
{
    if (watcher == entt::null || target == entt::null) return;

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (comp == nullptr) return;

    auto it = comp->entries.find(target);
    if (it == comp->entries.end()) return;

    // Downgrade from pinned — next Update() will evict if out of range.
    it->second.priority = AoiPriority::kNormal;
}

void InterestSystem::UpgradePriority(entt::entity watcher, entt::entity target, AoiPriority priority)
{
    if (watcher == entt::null || target == entt::null) return;

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (comp == nullptr) return;

    auto it = comp->entries.find(target);
    if (it == comp->entries.end()) return;

    const auto& policy = GetPriorityPolicy(watcher);
    if (policy.GetWeight(priority) > policy.GetWeight(it->second.priority))
    {
        it->second.priority = priority;
    }
}

void InterestSystem::DowngradePriority(entt::entity watcher, entt::entity target, AoiPriority from, AoiPriority to)
{
    if (watcher == entt::null || target == entt::null) return;

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (comp == nullptr) return;

    auto it = comp->entries.find(target);
    if (it == comp->entries.end()) return;

    // 只认标签不认权重:条目已被别的来源改成其它标签(如 kPinned / kAttacker)时,
    // 那个来源才是它的所有者,这里不能越权回退。
    if (it->second.priority != from) return;
    it->second.priority = to;
}

