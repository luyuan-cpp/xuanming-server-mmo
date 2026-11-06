#include "aoi.h"
#include "actor/actor_net_id.h"

#include <map>
#include <utility>

#include "hexagons_grid.h"
#include "spatial/comp/grid_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/aoi_priority.h"
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
#include "network/client_message_sender.h"
#include <modules/scene/comp/scene_comp.h>

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

// 对 target 所在的当前格及邻格,让能看见它的实体补上空间来源(与首次入格同一判定)。
void RebuildNearbyObservers(entt::entity target, AoiVisibilityChanges& changes)
{
    const auto* hex = tlsEcs.actorRegistry.try_get<Hex>(target);
    const auto* sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(target);
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
            if (other != target && ViewSystem::CanSee(other, target))
            {
                InterestSystem::AddAoiEntity(other, target, DetermineAoiPriority(other, target), &changes);
            }
        }
    }
}
} // namespace

void AoiSystem::Update(double delta) {
    for (auto&& [entity, transform, sceneComp] : tlsEcs.actorRegistry.view<Transform, SceneEntityComp>().each()) {
        // Skip invalid scenes
        if (!tlsEcs.sceneRegistry.valid(sceneComp.sceneEntity)) {
            LOG_ERROR << "Scene not found for entity " << entt::to_integral(entity);
            continue;
        }

        // Get current hex grid position
        const auto currentHex = GridSystem::CalculateHexPosition(transform);
        const auto currentGridId = GridSystem::GetGridId(currentHex);

        GridSet gridsToEnter, gridsToLeave;
        auto* gridList = UpdateGridState(entity, sceneComp.sceneEntity, currentHex,
                                        currentGridId, gridsToEnter, gridsToLeave);
        if (gridList == nullptr) continue;

        // Handle entity enter/leave visibility
        HandleEntityVisibility(entity, *gridList, gridsToEnter, gridsToLeave);
    }
}

SceneGridListComp* AoiSystem::UpdateGridState(const entt::entity entity, const SceneEntity sceneEntity, const Hex& currentHex,
                                const GridId currentGridId, GridSet& gridsToEnter, GridSet& gridsToLeave) {
    const auto* previousHexPtr = tlsEcs.actorRegistry.try_get<Hex>(entity);
    if (previousHexPtr != nullptr && hex_distance(*previousHexPtr, currentHex) == 0) {
        return nullptr;
    }

    // 此处延迟创建:真实入格写入口是 SceneGridListComp 的唯一创建点。
    // 空场景和只读空间查询不需要表;首次入格与跨格移动都在这里完成占位。
    auto& gridList = tlsEcs.sceneRegistry.get_or_emplace<SceneGridListComp>(sceneEntity);
    if (previousHexPtr == nullptr) {
        // First time entering scene
        // AoiListComp 不在这里预挂;实际可见关系由 InterestSystem 按需创建。
        gridList[currentGridId].entities.insert(entity);
        tlsEcs.actorRegistry.emplace<Hex>(entity, currentHex);
        GridSystem::GetCurrentAndNeighborGridIds(currentHex, gridsToEnter);
    } else {
        // Position update
        const auto previousHex = *previousHexPtr;

        GridSystem::GetCurrentAndNeighborGridIds(previousHex, gridsToLeave);
        GridSystem::GetCurrentAndNeighborGridIds(currentHex, gridsToEnter);

        // Remove common grids (no need to process enter/leave for unchanged grids)
        for (auto it = gridsToLeave.begin(); it != gridsToLeave.end(); ) {
            if (gridsToEnter.erase(*it)) {
                it = gridsToLeave.erase(it);
            } else {
                ++it;
            }
        }

        const auto previousGridId = GridSystem::GetGridId(previousHex);
        if (auto previousGrid = gridList.find(previousGridId); previousGrid != gridList.end()) {
            previousGrid->second.entities.erase(entity);
        }
        gridList[currentGridId].entities.insert(entity);

        // Hex has const members -> copy assignment is deleted -> emplace_or_replace won't compile.
        // Use remove + emplace instead.
        tlsEcs.actorRegistry.remove<Hex>(entity);
        tlsEcs.actorRegistry.emplace<Hex>(entity, currentHex);
    }
    return &gridList;
}

void AoiSystem::HandleEntityVisibility(entt::entity entity, SceneGridListComp& gridList,
                                       const GridSet& gridsToEnter, const GridSet& gridsToLeave) {
    // 两个方向各自独立判定 CanSee、容量与来源;可见性变化统一收集后下发。
    AoiVisibilityChanges changes;

    for (const auto& gridId : gridsToEnter) {
        auto gridIt = gridList.find(gridId);
        if (gridIt == gridList.end()) continue;

        for (const auto& otherEntity : gridIt->second.entities) {
            if (otherEntity == entity) continue;

            if (ViewSystem::CanSee(entity, otherEntity)) {
                InterestSystem::AddAoiEntity(entity, otherEntity, DetermineAoiPriority(entity, otherEntity), &changes);
            }
            // 静止观察者可能一直不跨格:反向的空间来源在跨格者这一侧一并建立。
            if (ViewSystem::CanSee(otherEntity, entity)) {
                InterestSystem::AddAoiEntity(otherEntity, entity, DetermineAoiPriority(otherEntity, entity), &changes);
            }
        }
    }

    // 走出的格子只撤空间来源:显式关注(技能追踪、队伍等)继续有效,客户端不收移除。
    for (const auto& gridId : gridsToLeave) {
        auto gridIt = gridList.find(gridId);
        if (gridIt == gridList.end()) continue;

        for (const auto& otherEntity : gridIt->second.entities) {
            if (otherEntity == entity) continue;
            InterestSystem::RemoveAoiEntity(entity, otherEntity, &changes);
            InterestSystem::RemoveAoiEntity(otherEntity, entity, &changes);
        }
    }

    SendVisibilityChanges(changes);
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

void AoiSystem::RefreshTargetVisibility(entt::entity target) {
    if (!tlsEcs.actorRegistry.valid(target)) {
        return;
    }
    AoiVisibilityChanges changes;
    InterestSystem::RefreshTargetVisibility(target, &changes);
    // Update 只在跨格时评估空间可见性:隐身结束不能等任何一方跨格,附近实体在这里立即重新评估。
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
