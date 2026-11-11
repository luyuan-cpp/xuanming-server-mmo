#include "interest.h"

#include <algorithm>
#include <limits>

#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/aoi_priority.h"
#include "spatial/system/view.h"

#include <modules/scene/comp/scene_comp.h>
#include <muduo/base/Logging.h>
#include <thread_context/ecs_context.h>

// 设计见 docs/design/aoi-directed-interest.md。本文件是正向索引 AoiListComp 与反向索引
// AoiWatchersComp 的唯一写者:所有改动经下面的 InsertEntry / EraseEntry / Show / Hide 四个原语,
// 两张索引因此始终描述同一批关系。

namespace
{
using EntryIterator = std::unordered_map<entt::entity, AoiEntry>::iterator;

void Record(AoiVisibilityChanges* changes, entt::entity watcher, entt::entity target, bool shown)
{
    if (changes != nullptr)
    {
        changes->changes.push_back({watcher, target, shown});
    }
}

// ---------------------------------------------------------------------------
// 索引原语
// ---------------------------------------------------------------------------

void MarkWatcherVisible(entt::entity target, entt::entity watcher, bool visible)
{
    if (auto* index = tlsEcs.actorRegistry.try_get<AoiWatchersComp>(target))
    {
        if (const auto it = index->watchers.find(watcher); it != index->watchers.end())
        {
            it->second = visible;
        }
    }
}

// 新建一条暂不可见的关系。调用方保证 watcher 与 target 存活且条目尚不存在。
EntryIterator InsertEntry(entt::entity watcher, AoiListComp& list, entt::entity target)
{
    // 反向索引的唯一创建点:第一次有人关注 target 时延迟创建。
    tlsEcs.actorRegistry.get_or_emplace<AoiWatchersComp>(target).watchers.emplace(watcher, false);
    return list.entries.emplace(target, AoiEntry{}).first;
}

void Show(entt::entity watcher, AoiListComp& list, entt::entity target, AoiEntry& entry,
          AoiVisibilityChanges* changes)
{
    entry.visible = true;
    ++list.visibleCount;
    MarkWatcherVisible(target, watcher, true);
    Record(changes, watcher, target, true);
}

void Hide(entt::entity watcher, AoiListComp& list, entt::entity target, AoiEntry& entry,
          AoiVisibilityChanges* changes)
{
    entry.visible = false;
    --list.visibleCount;
    MarkWatcherVisible(target, watcher, false);
    Record(changes, watcher, target, false);
}

// 删除一条关系;显示中的先记一次移除。调用后 it 失效。
void EraseEntry(entt::entity watcher, AoiListComp& list, EntryIterator it, AoiVisibilityChanges* changes)
{
    const auto target = it->first;
    if (it->second.visible)
    {
        Hide(watcher, list, target, it->second, changes);
    }
    if (auto* index = tlsEcs.actorRegistry.try_get<AoiWatchersComp>(target))
    {
        index->watchers.erase(watcher);
    }
    list.entries.erase(it);
}

// ---------------------------------------------------------------------------
// 判定
// ---------------------------------------------------------------------------

void DropSpatial(AoiEntry& entry)
{
    entry.spatial = false;
    entry.priority = AoiPriority::kNormal;
}

// 条目的淘汰权重:空间标签与各显式来源标签里最高的一个。
uint8_t EntryWeight(const AoiEntry& entry, const AoiPriorityPolicy& policy)
{
    uint8_t weight = entry.spatial ? policy.GetWeight(entry.priority) : 0;
    for (const auto& grant : entry.follows)
    {
        weight = std::max(weight, policy.GetWeight(grant.priority));
    }
    return weight;
}

// 距离只约束空间来源(由 AoiSystem 按进出视野半径把关);显式关注可以超出视野,但仍受隐身约束。
bool IsEligible(entt::entity target, const AoiEntry& entry)
{
    return !ViewSystem::IsStealthed(target) || entry.RevealsStealth();
}

bool InSameScene(entt::entity lhs, entt::entity rhs)
{
    const auto* lhsScene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(lhs);
    const auto* rhsScene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(rhs);
    return lhsScene != nullptr && rhsScene != nullptr && lhsScene->sceneEntity == rhsScene->sceneEntity &&
           tlsEcs.sceneRegistry.valid(lhsScene->sceneEntity);
}

// 先校验再动表:已销毁的句柄既不能当观察者(会在空槽位挂出孤儿组件),
// 也不能被登记成目标(下发给客户端的网络号会指向不存在的 actor)。
bool IsValidPair(entt::entity watcher, entt::entity target, const char* caller)
{
    if (watcher == entt::null || target == entt::null || watcher == target)
    {
        return false;
    }
    if (!tlsEcs.actorRegistry.valid(watcher) || !tlsEcs.actorRegistry.valid(target))
    {
        LOG_ERROR << caller << ": invalid entity, watcher=" << entt::to_integral(watcher)
                  << " target=" << entt::to_integral(target);
        return false;
    }
    return true;
}

// ---------------------------------------------------------------------------
// 容量
// ---------------------------------------------------------------------------

// 已显示条目里权重最低的一个(不含 exclude);没有返回 end。
EntryIterator FindLowestVisible(AoiListComp& list, const AoiPriorityPolicy& policy, entt::entity exclude)
{
    auto lowest = list.entries.end();
    uint8_t lowestWeight = std::numeric_limits<uint8_t>::max();
    for (auto it = list.entries.begin(); it != list.entries.end(); ++it)
    {
        if (!it->second.visible || it->first == exclude)
        {
            continue;
        }
        const auto weight = EntryWeight(it->second, policy);
        if (lowest == list.entries.end() || weight < lowestWeight)
        {
            lowest = it;
            lowestWeight = weight;
        }
    }
    return lowest;
}

// 容量已满时,为权重 weight 的 incoming 让出一个名额:只淘汰权重严格更低的已显示条目。
// 被淘汰者只剩空间来源时整条删除(任一方位置变化后重新评估);有显式来源时转为暂隐,名额空出后补回。
// 淘汰只改本观察者的表,目标那一侧的关系各自独立,不要求互相占位。
bool EvictForWeight(entt::entity watcher, AoiListComp& list, uint8_t weight, entt::entity incoming,
                    const AoiPriorityPolicy& policy, AoiVisibilityChanges* changes)
{
    // 权重 0 挤不掉任何条目(要求严格更低),不必扫表:满表的人群里每次位移重判,
    // 普通候选都会走到这里。
    if (weight == 0)
    {
        return false;
    }
    const auto lowest = FindLowestVisible(list, policy, incoming);
    if (lowest == list.entries.end() || weight <= EntryWeight(lowest->second, policy))
    {
        return false;
    }
    DropSpatial(lowest->second);
    if (lowest->second.follows.empty())
    {
        EraseEntry(watcher, list, lowest, changes);
    }
    else
    {
        Hide(watcher, list, lowest->first, lowest->second, changes);
    }
    return true;
}

// 尝试显示一条暂不可见的关系:可见条件满足且有名额(必要时按权重淘汰)才显示。
bool TryShow(entt::entity watcher, AoiListComp& list, entt::entity target, AoiEntry& entry,
             AoiVisibilityChanges* changes)
{
    if (!IsEligible(target, entry))
    {
        return false;
    }
    const auto& policy = InterestSystem::GetPriorityPolicy(watcher);
    if (list.visibleCount >= InterestSystem::GetEffectiveCapacity(watcher) &&
        !EvictForWeight(watcher, list, EntryWeight(entry, policy), target, policy, changes))
    {
        return false;
    }
    Show(watcher, list, target, entry, changes);
    return true;
}

// 观察者让出名额后,按权重从高到低补上满足可见条件的暂隐关系。
// 暂隐关系只来自显式关注,多数观察者一条都没有,此时不扫描。
void FillFreeSlots(entt::entity watcher, AoiListComp& list, AoiVisibilityChanges* changes)
{
    if (list.entries.size() == list.visibleCount)
    {
        return;
    }
    const auto capacity = InterestSystem::GetEffectiveCapacity(watcher);
    const auto& policy = InterestSystem::GetPriorityPolicy(watcher);
    while (list.visibleCount < capacity)
    {
        auto best = list.entries.end();
        uint8_t bestWeight = 0;
        for (auto it = list.entries.begin(); it != list.entries.end(); ++it)
        {
            if (it->second.visible || !IsEligible(it->first, it->second))
            {
                continue;
            }
            const auto weight = EntryWeight(it->second, policy);
            if (best == list.entries.end() || weight > bestWeight)
            {
                best = it;
                bestWeight = weight;
            }
        }
        if (best == list.entries.end())
        {
            return;
        }
        Show(watcher, list, best->first, best->second, changes);
    }
}

// 按当前可见条件校正一条关系:不再满足就暂停显示(空间来源以能看见为前提,一并撤掉),
// 重新满足就尝试显示。可能删除条目,调用后不得再使用 it。
void Reconcile(entt::entity watcher, AoiListComp& list, EntryIterator it, AoiVisibilityChanges* changes)
{
    const auto target = it->first;
    auto& entry = it->second;
    const bool eligible = IsEligible(target, entry);
    if (entry.visible && !eligible)
    {
        DropSpatial(entry);
        if (entry.follows.empty())
        {
            EraseEntry(watcher, list, it, changes);
        }
        else
        {
            Hide(watcher, list, target, entry, changes);
        }
        FillFreeSlots(watcher, list, changes);
        return;
    }
    if (!entry.visible && eligible)
    {
        TryShow(watcher, list, target, entry, changes);
    }
}
} // namespace

// ---------------------------------------------------------------------------
// 容量与策略
// ---------------------------------------------------------------------------

std::size_t InterestSystem::GetEffectiveCapacity(entt::entity watcher)
{
    // 三种容量/策略组件是稀疏可选配置,生产尚未接线。缺失表示使用默认值,不补建。
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
    // ScenePriorityPolicyComp 仅是预留入口,生产不预挂;缺省即 OpenWorld 策略。
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

// ---------------------------------------------------------------------------
// 空间来源
// ---------------------------------------------------------------------------

bool InterestSystem::AddAoiEntity(entt::entity watcher, entt::entity target, AoiPriority priority,
                                  AoiVisibilityChanges* changes)
{
    if (!IsValidPair(watcher, target, "AddAoiEntity"))
    {
        return false;
    }

    auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    const auto& policy = GetPriorityPolicy(watcher);
    if (list != nullptr)
    {
        if (const auto it = list->entries.find(target); it != list->entries.end())
        {
            auto& entry = it->second;
            if (entry.visible)
            {
                // 已在显示(可能只因显式来源):补上空间来源,标签只升不降,不重复创建。
                if (!entry.spatial)
                {
                    entry.spatial = true;
                    entry.priority = priority;
                }
                else if (policy.GetWeight(priority) > policy.GetWeight(entry.priority))
                {
                    entry.priority = priority;
                }
                return true;
            }
            // 暂隐的显式关注:按空间与显式来源合并后的权重争取名额;争不到就不单独保留空间来源。
            entry.spatial = true;
            entry.priority = priority;
            if (!TryShow(watcher, *list, target, entry, changes))
            {
                DropSpatial(entry);
            }
            return entry.visible;
        }
    }

    // 新关系:先确认可见条件与名额,确实要显示时才创建条目(以及兴趣表本身)。
    // 新的空间关系没有显式来源,也就没有侦测授权。
    if (ViewSystem::IsStealthed(target))
    {
        return false;
    }
    const auto capacity = GetEffectiveCapacity(watcher);
    if (capacity == 0)
    {
        return false;
    }
    if (list != nullptr && list->visibleCount >= capacity &&
        !EvictForWeight(watcher, *list, policy.GetWeight(priority), target, policy, changes))
    {
        return false;
    }
    if (list == nullptr)
    {
        // 按需状态的创建点之一(另一个是 Follow):第一次真正建立关系时才挂兴趣表。
        list = &tlsEcs.actorRegistry.emplace<AoiListComp>(watcher);
    }
    const auto it = InsertEntry(watcher, *list, target);
    it->second.spatial = true;
    it->second.priority = priority;
    Show(watcher, *list, target, it->second, changes);
    return true;
}

void InterestSystem::RemoveAoiEntity(entt::entity watcher, entt::entity target, AoiVisibilityChanges* changes)
{
    if (watcher == entt::null || target == entt::null)
    {
        return;
    }
    auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (list == nullptr)
    {
        return;
    }
    const auto it = list->entries.find(target);
    if (it == list->entries.end() || !it->second.spatial)
    {
        return;
    }

    DropSpatial(it->second);
    if (!it->second.follows.empty())
    {
        // 显式来源仍在:走出邻格也继续显示,客户端不收移除。
        return;
    }
    const bool freedSlot = it->second.visible;
    EraseEntry(watcher, *list, it, changes);
    if (freedSlot)
    {
        FillFreeSlots(watcher, *list, changes);
    }
}

void InterestSystem::UpgradePriority(entt::entity watcher, entt::entity target, AoiPriority priority)
{
    if (watcher == entt::null || target == entt::null)
    {
        return;
    }
    auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (list == nullptr)
    {
        return;
    }
    const auto it = list->entries.find(target);
    if (it == list->entries.end() || !it->second.spatial)
    {
        return;
    }
    const auto& policy = GetPriorityPolicy(watcher);
    if (policy.GetWeight(priority) > policy.GetWeight(it->second.priority))
    {
        it->second.priority = priority;
    }
}

void InterestSystem::DowngradePriority(entt::entity watcher, entt::entity target, AoiPriority from, AoiPriority to)
{
    if (watcher == entt::null || target == entt::null)
    {
        return;
    }
    auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (list == nullptr)
    {
        return;
    }
    const auto it = list->entries.find(target);
    if (it == list->entries.end() || !it->second.spatial)
    {
        return;
    }
    // 只认标签不认权重:标签已被别的空间判定改成其他值时,那次判定才是它的所有者,这里不越权回退。
    if (it->second.priority != from)
    {
        return;
    }
    it->second.priority = to;
}

// ---------------------------------------------------------------------------
// 显式来源
// ---------------------------------------------------------------------------

bool InterestSystem::Follow(entt::entity watcher, entt::entity target, const AoiFollowGrant& grant,
                            AoiVisibilityChanges* changes)
{
    if (!IsValidPair(watcher, target, "Follow"))
    {
        return false;
    }
    if (!InSameScene(watcher, target))
    {
        LOG_WARN << "Follow: watcher and target are not in the same scene, watcher="
                 << entt::to_integral(watcher) << " target=" << entt::to_integral(target);
        return false;
    }

    // 按需状态的另一个创建点:显式关系即使暂不可见也要登记,条件满足后才能自动显示。
    auto& list = tlsEcs.actorRegistry.get_or_emplace<AoiListComp>(watcher);
    auto it = list.entries.find(target);
    if (it == list.entries.end())
    {
        it = InsertEntry(watcher, list, target);
    }
    auto& follows = it->second.follows;
    if (const auto same = std::ranges::find(follows, grant.source, &AoiFollowGrant::source); same != follows.end())
    {
        *same = grant;
    }
    else
    {
        follows.push_back(grant);
    }
    Reconcile(watcher, list, it, changes);
    return true;
}

void InterestSystem::Unfollow(entt::entity watcher, entt::entity target, const AoiFollowSource& source,
                              AoiVisibilityChanges* changes)
{
    if (watcher == entt::null || target == entt::null)
    {
        return;
    }
    auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (list == nullptr)
    {
        return;
    }
    const auto it = list->entries.find(target);
    if (it == list->entries.end())
    {
        return;
    }
    auto& follows = it->second.follows;
    const auto same = std::ranges::find(follows, source, &AoiFollowGrant::source);
    if (same == follows.end())
    {
        return;
    }
    follows.erase(same);

    if (follows.empty() && !it->second.spatial)
    {
        const bool freedSlot = it->second.visible;
        EraseEntry(watcher, *list, it, changes);
        if (freedSlot)
        {
            FillFreeSlots(watcher, *list, changes);
        }
        return;
    }
    // 还有其他来源:撤掉的若是唯一的侦测授权,隐身目标随之不可见。
    Reconcile(watcher, *list, it, changes);
}

// ---------------------------------------------------------------------------
// 可见条件变化与离场
// ---------------------------------------------------------------------------

void InterestSystem::RefreshTargetVisibility(entt::entity target, AoiVisibilityChanges* changes)
{
    const auto* index = tlsEcs.actorRegistry.try_get<AoiWatchersComp>(target);
    if (index == nullptr || index->watchers.empty())
    {
        return;
    }
    // 先拷名单:校正可能删除关系,会改到这张反向索引。
    EntityVector watchers;
    watchers.reserve(index->watchers.size());
    for (const auto& [watcher, visible] : index->watchers)
    {
        watchers.push_back(watcher);
    }
    for (const auto watcher : watchers)
    {
        auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
        if (list == nullptr)
        {
            continue;
        }
        if (const auto it = list->entries.find(target); it != list->entries.end())
        {
            Reconcile(watcher, *list, it, changes);
        }
    }
}

void InterestSystem::ClearRelations(entt::entity entity, AoiVisibilityChanges* changes)
{
    if (entity == entt::null)
    {
        return;
    }

    // 出边:从每个目标的反向索引里摘掉自己。自己的客户端不记移除:离场、换场景后由新场景整体重建。
    if (auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(entity))
    {
        for (const auto& [target, entry] : list->entries)
        {
            if (auto* targetIndex = tlsEcs.actorRegistry.try_get<AoiWatchersComp>(target))
            {
                targetIndex->watchers.erase(entity);
            }
        }
        list->entries.clear();
        list->visibleCount = 0;
    }

    // 入边:从每个观察者的正向表里摘掉自己。正在显示的记一次移除,并把让出的名额补给它的暂隐关注。
    if (auto* index = tlsEcs.actorRegistry.try_get<AoiWatchersComp>(entity))
    {
        const auto watchers = std::move(index->watchers);
        index->watchers.clear();
        for (const auto& [watcher, visible] : watchers)
        {
            auto* watcherList = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
            if (watcherList == nullptr)
            {
                continue;
            }
            const auto it = watcherList->entries.find(entity);
            if (it == watcherList->entries.end())
            {
                continue;
            }
            const bool freedSlot = it->second.visible;
            if (freedSlot)
            {
                Hide(watcher, *watcherList, entity, it->second, changes);
            }
            watcherList->entries.erase(it);
            if (freedSlot)
            {
                FillFreeSlots(watcher, *watcherList, changes);
            }
        }
    }
}

// ---------------------------------------------------------------------------
// 只读查询
// ---------------------------------------------------------------------------

void InterestSystem::GetVisibleWatchers(entt::entity target, EntityVector& out)
{
    const auto* index = tlsEcs.actorRegistry.try_get<AoiWatchersComp>(target);
    if (index == nullptr)
    {
        return;
    }
    out.reserve(out.size() + index->watchers.size());
    for (const auto& [watcher, visible] : index->watchers)
    {
        if (visible && tlsEcs.actorRegistry.valid(watcher))
        {
            out.push_back(watcher);
        }
    }
}
