#include "spatial/system/interest.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include <modules/scene/comp/scene_comp.h>
#include <thread_context/ecs_context.h>

// ---------------------------------------------------------------------------
// Test mock — mirrors production logic with policy-based weight comparison
// and dynamic capacity.  Simplified: no eviction scan (just reject at cap).
// ---------------------------------------------------------------------------

static const AoiPriorityPolicy& MockGetPolicy(entt::entity watcher)
{
    if (const auto* sc = tlsEcs.actorRegistry.try_get<SceneEntityComp>(watcher))
    {
        if (tlsEcs.sceneRegistry.valid(sc->sceneEntity))
        {
            if (const auto* pc = tlsEcs.sceneRegistry.try_get<ScenePriorityPolicyComp>(sc->sceneEntity))
            {
                return PolicyOf(pc->policy);
            }
        }
    }
    return kPolicyOpenWorld;
}

std::size_t InterestSystem::GetEffectiveCapacity(entt::entity watcher)
{
    std::size_t clientCap = kAoiListCapacityDefault;
    if (const auto* cc = tlsEcs.actorRegistry.try_get<AoiClientCapacityComp>(watcher))
    {
        clientCap = std::clamp(cc->clientDesiredCount,
                               kAoiListCapacityMin, kAoiListCapacityMax);
    }

    std::size_t serverCap = kAoiListCapacityMax;
    if (const auto* sc = tlsEcs.actorRegistry.try_get<SceneEntityComp>(watcher))
    {
        if (tlsEcs.sceneRegistry.valid(sc->sceneEntity))
        {
            if (const auto* p = tlsEcs.sceneRegistry.try_get<ScenePressureComp>(sc->sceneEntity))
            {
                serverCap = p->GetServerCapacity();
            }
        }
    }
    return std::min(clientCap, serverCap);
}

const AoiPriorityPolicy& InterestSystem::GetPriorityPolicy(entt::entity watcher)
{
    return MockGetPolicy(watcher);
}

bool InterestSystem::AddAoiEntity(entt::entity watcher, entt::entity target, AoiPriority priority)
{
    if (watcher == entt::null || target == entt::null) return false;
    auto& comp = tlsEcs.actorRegistry.get_or_emplace<AoiListComp>(watcher);
    const auto& policy = MockGetPolicy(watcher);

    if (auto it = comp.entries.find(target); it != comp.entries.end())
    {
        if (policy.GetWeight(priority) > policy.GetWeight(it->second.priority))
            it->second.priority = priority;
        return true;
    }
    if (comp.entries.size() >= GetEffectiveCapacity(watcher)) return false;
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
    it->second.priority = AoiPriority::kNormal;
}

void InterestSystem::UpgradePriority(entt::entity watcher, entt::entity target, AoiPriority priority)
{
    if (watcher == entt::null || target == entt::null) return;
    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (comp == nullptr) return;
    auto it = comp->entries.find(target);
    if (it == comp->entries.end()) return;
    const auto& policy = MockGetPolicy(watcher);
    if (policy.GetWeight(priority) > policy.GetWeight(it->second.priority))
        it->second.priority = priority;
}

// 与生产实现同语义:条目标签恰好等于 from 才改成 to(team-system.md §F.4)。
void InterestSystem::DowngradePriority(entt::entity watcher, entt::entity target, AoiPriority from, AoiPriority to)
{
    if (watcher == entt::null || target == entt::null) return;
    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (comp == nullptr) return;
    auto it = comp->entries.find(target);
    if (it == comp->entries.end()) return;
    if (it->second.priority != from) return;
    it->second.priority = to;
}
