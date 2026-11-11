#include "aoi.h"
#include "actor/actor_net_id.h"

#include <map>
#include <utility>

#include "hexagons_grid.h"
#include "spatial/comp/grid_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/aoi_priority.h"
#include "spatial/constants/view.h"
#include "spatial/system/aoi_load.h"
#include "spatial/system/grid.h"
#include "spatial/system/interest.h"
#include "spatial/system/view.h"
#include "macros/return_define.h"
#include "muduo/base/Logging.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/team_comp.pb.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"
#include "core/utils/stat/stat.h"
#include "proto/scene/player_scene.pb.h"
#include "proto/scene/scene_info.pb.h"  // SceneInfoComp:每个场景实体都有,用来遍历本线程的场景
#include "network/client_message_sender.h"
#include <modules/scene/comp/scene_comp.h>

#include <unordered_set>

// Determine the interest priority tag that |observer| should assign to |target|.
// The tag is a semantic category; the active AoiPriorityPolicy turns it into a
// numeric weight for eviction decisions.
static AoiPriority DetermineAoiPriority(entt::entity observer, entt::entity target)
{
    // Same team -> kTeammate
    const auto* observerTeam = tlsEcs.actorRegistry.try_get<TeamId>(observer);
    const auto* targetTeam   = tlsEcs.actorRegistry.try_get<TeamId>(target);
    if (observerTeam && targetTeam &&
        observerTeam->team_id() != 0 &&
        observerTeam->team_id() == targetTeam->team_id())
    {
        return AoiPriority::kTeammate;
    }

    return AoiPriority::kNormal;
}

namespace
{
// 发送缓冲用函数内 static thread_local:与 tlsEcs 同线程、同寿命,Clear() 保留已分配的子消息供下次复用;
// 填好后同步序列化发送,发送路径不会回调进 AOI,不存在重入覆盖。
bool AppendActorCreate(ActorListCreateS2C& message, entt::entity target)
{
    const auto netId = actor_net_id::Of(tlsEcs.actorRegistry, target);
    if (netId == 0)
    {
        LOG_ERROR << "AOI: actor create skipped, network id missing";
        return false;
    }
    // 创建消息携带当前 Transform:首次有效可见时先发创建和当前位置,之后才接收增量。
    ViewSystem::FillActorCreateMessageInfo(target, netId, *message.add_actor_list());
    return true;
}

bool AppendActorDestroy(ActorListDestroyS2C& message, entt::entity target)
{
    const auto netId = actor_net_id::Of(tlsEcs.actorRegistry, target);
    if (netId == 0)
    {
        LOG_ERROR << "AOI: actor removal skipped, network id missing";
        return false;
    }
    message.add_entity(netId);
    return true;
}

// 速度非零就算在移动(与 MovementSystem::BroadcastMove 的 move_state 同一口径)。
// 没有 Velocity 组件的实体(NPC 等)按静止处理:位置一变就评估。
bool IsMoving(entt::entity entity)
{
    const auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(entity);
    return velocity != nullptr && (velocity->x() != 0.0 || velocity->y() != 0.0 || velocity->z() != 0.0);
}

// 位置自上次评估之后要不要重新评估。没变不评估;静止的(刚停步、被挪位)立即评估,
// 停下来的那一刻一定按精确位置判一次;移动中的累计位移到 kAoiReevaluateDistance 才评估。
bool IsReevaluationDue(entt::entity entity, const Transform& transform)
{
    const auto* evaluatedAt = tlsEcs.actorRegistry.try_get<AoiEvaluatedAtComp>(entity);
    if (evaluatedAt == nullptr)
    {
        return true;
    }
    const auto& location = transform.location();
    const double dx = location.x() - evaluatedAt->x;
    const double dy = location.y() - evaluatedAt->y;
    const double dz = location.z() - evaluatedAt->z;
    const double movedSquared = dx * dx + dy * dy + dz * dz;
    if (movedSquared == 0.0)
    {
        return false;
    }
    return !IsMoving(entity) || movedSquared >= kAoiReevaluateDistance * kAoiReevaluateDistance;
}

// 有附近候选因名额被拒收或被挤掉(AoiNearbyRefillComp),现在又有空名额:重扫一次补回。
// 位置不变也要补:打完架站着不动,战斗期间被挤掉的附近的人要回来。
bool IsNearbyRefillDue(entt::entity entity)
{
    if (!tlsEcs.actorRegistry.any_of<AoiNearbyRefillComp>(entity))
    {
        return false;
    }
    const auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(entity);
    return list == nullptr || list->VisibleCount() < InterestSystem::GetEffectiveCapacity(entity);
}

// watcher 对 target 还没有空间来源、且距离进入视野半径时登记空间来源;容量与优先级由 InterestSystem 裁决。
void TryAddSpatialSource(entt::entity watcher, entt::entity target, AoiVisibilityChanges& changes)
{
    const auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    if (list != nullptr && list->HasSpatialSource(target))
    {
        return;
    }
    if (ViewSystem::CanSee(watcher, target))
    {
        InterestSystem::AddAoiEntity(watcher, target, DetermineAoiPriority(watcher, target), &changes);
    }
}

// 出视野:entity 关注的、关注 entity 的空间来源,距离超出离开半径(或目标隐身且没有侦测授权)的撤掉。
// 只撤空间来源,显式关注照常显示。按距离判,与格子无关。先收集再撤:撤销会改两张索引。
void DropDistantSpatialSources(entt::entity entity, AoiVisibilityChanges& changes)
{
    EntityVector stale;
    if (const auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(entity))
    {
        for (const auto& [target, entry] : list->entries)
        {
            if (entry.spatial && !ViewSystem::CanKeepSeeing(entity, target))
            {
                stale.push_back(target);
            }
        }
    }
    for (const auto target : stale)
    {
        InterestSystem::RemoveAoiEntity(entity, target, &changes);
    }

    stale.clear();
    if (const auto* index = tlsEcs.actorRegistry.try_get<AoiWatchersComp>(entity))
    {
        for (const auto& [watcher, visible] : index->watchers)
        {
            const auto* watcherList = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
            if (watcherList != nullptr && watcherList->HasSpatialSource(entity) &&
                !ViewSystem::CanKeepSeeing(watcher, entity))
            {
                stale.push_back(watcher);
            }
        }
    }
    for (const auto watcher : stale)
    {
        InterestSystem::RemoveAoiEntity(watcher, entity, &changes);
    }
}

// 对 entity 本格及六个邻格里的每个其他实体调用 visit(entity 必须已在格子里)。只读格子表。
template <typename Visit>
void ForEachNearbyEntity(entt::entity entity, Visit&& visit)
{
    const auto* hex = tlsEcs.actorRegistry.try_get<Hex>(entity);
    const auto* sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
    if (hex == nullptr || sceneComp == nullptr || !tlsEcs.sceneRegistry.valid(sceneComp->sceneEntity))
    {
        return;
    }
    const auto* gridList = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(sceneComp->sceneEntity);
    if (gridList == nullptr)
    {
        return;
    }
    GridSet grids;
    GridSystem::GetCurrentAndNeighborGridIds(*hex, grids);
    for (const auto& gridId : grids)
    {
        const auto gridIt = gridList->find(gridId);
        if (gridIt == gridList->end())
        {
            continue;
        }
        for (const auto other : gridIt->second.entities)
        {
            if (other != entity)
            {
                visit(other);
            }
        }
    }
}

// 进视野:entity 本格及六个邻格里的其他实体,两个方向各尝试建立空间来源。
// 静止的一方不会自己评估,反向关系在位置变化的这一方评估时一并建立。
void AddNearbySpatialSources(entt::entity entity, AoiVisibilityChanges& changes)
{
    ForEachNearbyEntity(entity, [entity, &changes](entt::entity other)
    {
        TryAddSpatialSource(entity, other, changes);
        TryAddSpatialSource(other, entity, changes);
    });
}

// 隐身结束:target 当前格及邻格里能看见它的实体补上空间来源(与入格评估同一判定)。
void RebuildNearbyObservers(entt::entity target, AoiVisibilityChanges& changes)
{
    ForEachNearbyEntity(target, [target, &changes](entt::entity other)
    {
        TryAddSpatialSource(other, target, changes);
    });
}
} // namespace

void AoiSystem::Update(double delta) {
    for (auto&& [entity, transform, sceneComp] : tlsEcs.actorRegistry.view<Transform, SceneEntityComp>().each()) {
        // Skip invalid scenes
        if (!tlsEcs.sceneRegistry.valid(sceneComp.sceneEntity)) {
            LOG_ERROR << "Scene not found for entity " << entt::to_integral(entity);
            continue;
        }

        const bool firstEntry = UpdateGridMembership(entity, sceneComp.sceneEntity,
                                                     GridSystem::CalculateHexPosition(transform));
        if (firstEntry || IsReevaluationDue(entity, transform) || IsNearbyRefillDue(entity)) {
            ReevaluateVisibility(entity, transform);
        }
    }
}

bool AoiSystem::UpdateGridMembership(const entt::entity entity, const SceneEntity sceneEntity, const Hex& currentHex) {
    const auto* previousHexPtr = tlsEcs.actorRegistry.try_get<Hex>(entity);
    if (previousHexPtr != nullptr && hex_distance(*previousHexPtr, currentHex) == 0) {
        return false;
    }

    // 此处延迟创建:真实入格写入口是 SceneGridListComp 的唯一创建点。
    // 空场景和只读空间查询不需要表;首次入格与跨格移动都在这里完成占位。
    auto& gridList = tlsEcs.sceneRegistry.get_or_emplace<SceneGridListComp>(sceneEntity);
    const auto currentGridId = GridSystem::GetGridId(currentHex);
    if (previousHexPtr == nullptr) {
        // First time entering scene
        // AoiListComp 不在这里预挂;实际可见关系由 InterestSystem 按需创建。
        gridList[currentGridId].entities.insert(entity);
        tlsEcs.actorRegistry.emplace<Hex>(entity, currentHex);
        return true;
    }

    // 跨格只挪格子成员;进出视野按距离判,由调用方的评估处理。
    const auto previousGridId = GridSystem::GetGridId(*previousHexPtr);
    if (auto previousGrid = gridList.find(previousGridId); previousGrid != gridList.end()) {
        previousGrid->second.entities.erase(entity);
    }
    gridList[currentGridId].entities.insert(entity);

    // Hex has const members -> copy assignment is deleted -> emplace_or_replace won't compile.
    // Use remove + emplace instead.
    tlsEcs.actorRegistry.remove<Hex>(entity);
    tlsEcs.actorRegistry.emplace<Hex>(entity, currentHex);
    return false;
}

void AoiSystem::ReevaluateVisibility(const entt::entity entity, const Transform& transform) {
    // 这次评估会扫一遍附近候选,待补回的标记在此消费;评估中再被拒会由 InterestSystem 重新打上。
    tlsEcs.actorRegistry.remove<AoiNearbyRefillComp>(entity);

    // 先撤后建:走远的先让出名额,同一次评估里走近的就能用上。两个方向各自判定距离、容量与来源。
    AoiVisibilityChanges changes;
    DropDistantSpatialSources(entity, changes);
    AddNearbySpatialSources(entity, changes);
    SendVisibilityChanges(changes);

    // 此处延迟创建:AoiEvaluatedAtComp 的唯一创建点,之后的评估只覆盖位置。
    auto& evaluatedAt = tlsEcs.actorRegistry.get_or_emplace<AoiEvaluatedAtComp>(entity);
    const auto& location = transform.location();
    evaluatedAt.x = location.x();
    evaluatedAt.y = location.y();
    evaluatedAt.z = location.z();
}

bool AoiSystem::Follow(entt::entity watcher, entt::entity target, const AoiFollowGrant& grant) {
    AoiVisibilityChanges changes;
    const bool related = InterestSystem::Follow(watcher, target, grant, &changes);
    SendVisibilityChanges(changes);
    return related;
}

void AoiSystem::Unfollow(entt::entity watcher, entt::entity target, const AoiFollowSource& source) {
    AoiVisibilityChanges changes;
    InterestSystem::Unfollow(watcher, target, source, &changes);
    SendVisibilityChanges(changes);
}

void AoiSystem::ApplyServerPressure(double pressureFactor) {
    // 1) 写进本线程的每个场景,记下值真有变化(含刚建、还没有压力组件)的场景。
    const auto pressure = static_cast<float>(pressureFactor);
    std::unordered_set<SceneEntity> changedScenes;
    for (const auto scene : tlsEcs.sceneRegistry.view<SceneInfoComp>()) {
        const auto* current = tlsEcs.sceneRegistry.try_get<ScenePressureComp>(scene);
        if (current != nullptr && current->pressureFactor == pressure) {
            continue;
        }
        // 此处延迟创建:ScenePressureComp 的唯一创建点(也是唯一写入口)。
        tlsEcs.sceneRegistry.get_or_emplace<ScenePressureComp>(scene).pressureFactor = pressure;
        changedScenes.insert(scene);
    }
    if (changedScenes.empty()) {
        return;
    }

    // 2) 这些场景里的观察者按新容量校正名额,变化合成一批下发(每个观察者一条列表消息)。
    //    校正只改各自兴趣表的条目与目标的反向索引,不增删 AoiListComp,可以边遍历边做。
    AoiVisibilityChanges changes;
    std::size_t trimmed = 0;
    for (auto&& [watcher, list, sceneComp] : tlsEcs.actorRegistry.view<AoiListComp, SceneEntityComp>().each()) {
        if (changedScenes.contains(sceneComp.sceneEntity)) {
            trimmed += InterestSystem::ApplyCapacity(watcher, &changes);
        }
    }
    SendVisibilityChanges(changes);
    aoi_load_stats::RecordTrimmedEntries(trimmed);
}

void AoiSystem::RefreshTargetVisibility(entt::entity target) {
    if (!tlsEcs.actorRegistry.valid(target)) {
        return;
    }
    AoiVisibilityChanges changes;
    InterestSystem::RefreshTargetVisibility(target, &changes);
    // Update 只在位置变化后重新评估:隐身结束不能等任何一方走动,附近实体在这里立即重新评估。
    if (!ViewSystem::IsStealthed(target)) {
        RebuildNearbyObservers(target, changes);
    }
    SendVisibilityChanges(changes);
}

void AoiSystem::SendVisibilityChanges(const AoiVisibilityChanges& changes, entt::entity departed) {
    if (changes.Empty()) return;

    // 1) 净变化:同一对按发生顺序来回翻转,次数为偶数时客户端状态与批前相同,不发消息。
    struct NetChange {
        uint32_t flips = 0;
        bool shown = false;
    };
    std::map<std::pair<entt::entity, entt::entity>, NetChange> net;
    for (const auto& change : changes.changes) {
        auto& state = net[{change.watcher, change.target}];
        ++state.flips;
        state.shown = change.shown;
    }

    // 2) 按观察者分组;离场者的移除单独汇总。
    struct WatcherDelta {
        EntityVector created;
        EntityVector destroyed;
    };
    std::map<entt::entity, WatcherDelta> byWatcher;
    EntityUnorderedSet departedObservers;
    for (const auto& [pair, state] : net) {
        if (state.flips % 2 == 0) continue;
        const auto& [watcher, target] = pair;
        if (state.shown) {
            byWatcher[watcher].created.push_back(target);
        } else if (target == departed) {
            departedObservers.insert(watcher);
        } else {
            byWatcher[watcher].destroyed.push_back(target);
        }
    }

    // 3) 先移除后创建。只涉及一个目标的观察者按目标合并:各观察者收到同一个单实体消息,只组包一次,
    //    发送入口同步序列化并按 Gate 合并收件人;涉及多个目标的观察者收一条列表消息。
    std::map<entt::entity, EntityUnorderedSet> destroyByTarget;
    std::map<entt::entity, EntityUnorderedSet> createByTarget;
    static thread_local ActorListDestroyS2C destroyMsg;
    static thread_local ActorListCreateS2C createMsg;
    for (const auto& [watcher, delta] : byWatcher) {
        if (delta.destroyed.size() == 1) {
            destroyByTarget[delta.destroyed.front()].insert(watcher);
        } else if (!delta.destroyed.empty()) {
            destroyMsg.Clear();
            for (const auto target : delta.destroyed) AppendActorDestroy(destroyMsg, target);
            if (!destroyMsg.entity().empty()) {
                SendToPlayerClient(SceneSceneClientPlayerNotifyActorListDestroyMessageId, destroyMsg, watcher);
            }
        }
    }
    for (const auto& [target, watchers] : destroyByTarget) {
        destroyMsg.Clear();
        if (AppendActorDestroy(destroyMsg, target)) {
            BroadcastToPlayerClients(SceneSceneClientPlayerNotifyActorListDestroyMessageId, destroyMsg, watchers);
        }
    }
    if (!departedObservers.empty()) {
        const auto netId = actor_net_id::Of(tlsEcs.actorRegistry, departed);
        if (netId != 0) {
            static thread_local ActorDestroyS2C departedMsg;
            departedMsg.Clear();
            departedMsg.set_entity(netId);
            BroadcastToPlayerClients(SceneSceneClientPlayerNotifyActorDestroyMessageId, departedMsg, departedObservers);
        } else {
            LOG_ERROR << "AOI: actor leave notification skipped, network id missing";
        }
    }

    for (const auto& [watcher, delta] : byWatcher) {
        if (delta.created.size() == 1) {
            createByTarget[delta.created.front()].insert(watcher);
        } else if (!delta.created.empty()) {
            createMsg.Clear();
            for (const auto target : delta.created) AppendActorCreate(createMsg, target);
            if (!createMsg.actor_list().empty()) {
                SendToPlayerClient(SceneSceneClientPlayerNotifyActorListCreateMessageId, createMsg, watcher);
            }
        }
    }
    for (const auto& [target, watchers] : createByTarget) {
        createMsg.Clear();
        if (AppendActorCreate(createMsg, target)) {
            BroadcastToPlayerClients(SceneSceneClientPlayerNotifyActorListCreateMessageId, createMsg, watchers);
        }
    }
}

void AoiSystem::OnLeaveScene(entt::entity entity) {
    if (!tlsEcs.actorRegistry.valid(entity)) {
        return;
    }

    // 1) 关系:按正反索引清掉全部出边和入边。正在显示它的观察者(含网格外的显式关注者)各收一次移除;
    //    它自己的表也必须清空,否则旧场景的条目会占满新场景的观察容量。
    AoiVisibilityChanges changes;
    InterestSystem::ClearRelations(entity, &changes);
    SendVisibilityChanges(changes, entity);

    // 2) 格子:有空间记录才摘。格子表是按需状态;缺表表示没有空间记录,不得补建空表。
    const auto* hex = tlsEcs.actorRegistry.try_get<Hex>(entity);
    const auto* scene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
    if (hex != nullptr && scene != nullptr && tlsEcs.sceneRegistry.valid(scene->sceneEntity)) {
        if (auto* gridList = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(scene->sceneEntity)) {
            RemoveEntityFromGrid(*hex, *gridList, entity);
        }
    }
}

void AoiSystem::ResetEntityVisibility(entt::entity entity) {
    if (!tlsEcs.actorRegistry.valid(entity)) {
        return;
    }

    AoiVisibilityChanges changes;

    // 1) 它看别人:撤掉全部空间来源,失去全部来源的目标通知它自己的客户端移除。
    //    不通知的话客户端会把旧位置的 actor 永远留在世界里(之后同一实体再进视野时
    //    客户端按 entity 去重,连位置都不会更新)。先拷名单:撤销会改表。
    if (const auto* ownList = tlsEcs.actorRegistry.try_get<AoiListComp>(entity)) {
        EntityVector spatialTargets;
        for (const auto& [target, entry] : ownList->entries) {
            if (entry.spatial) spatialTargets.push_back(target);
        }
        for (const auto target : spatialTargets) {
            InterestSystem::RemoveAoiEntity(entity, target, &changes);
        }
    }

    // 2) 别人看它:按反向索引撤掉空间来源。显式关注者保留关系,新位置由调用方随后的移动广播送达。
    if (const auto* index = tlsEcs.actorRegistry.try_get<AoiWatchersComp>(entity)) {
        EntityVector watchers;
        for (const auto& [watcher, visible] : index->watchers) {
            watchers.push_back(watcher);
        }
        for (const auto watcher : watchers) {
            InterestSystem::RemoveAoiEntity(watcher, entity, &changes);
        }
    }
    SendVisibilityChanges(changes);

    // 3) 移出格子并摘掉 Hex:下一次 Update 把它当作首次进场,在新位置重建双向空间关注。
    //    还没进过格子的实体(刚进场、首次 Update 之前)没有 Hex,交给首次 Update。
    const auto* hex = tlsEcs.actorRegistry.try_get<Hex>(entity);
    if (hex == nullptr) {
        return;
    }
    const auto* sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
    if (sceneEntityComp != nullptr && tlsEcs.sceneRegistry.valid(sceneEntityComp->sceneEntity)) {
        if (auto* gridList = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(sceneEntityComp->sceneEntity)) {
            RemoveEntityFromGrid(*hex, *gridList, entity);
        }
    }
    // hex 指针在这一行之后失效,必须放在最后。
    tlsEcs.actorRegistry.remove<Hex>(entity);
}

void AoiSystem::RemoveEntityFromGrid(const Hex& hex, SceneGridListComp& gridList, entt::entity entity) {
    const auto gridId = GridSystem::GetGridId(hex);
    auto gridIt = gridList.find(gridId);
    if (gridIt == gridList.end()) return;

    auto& grid = gridIt->second;
    grid.entities.erase(entity);

    if (gFeatureSwitches[kTestClearEmptyTiles])
    {
        if (grid.entities.empty()) {
            gridList.erase(gridIt);
        }
    }
}
