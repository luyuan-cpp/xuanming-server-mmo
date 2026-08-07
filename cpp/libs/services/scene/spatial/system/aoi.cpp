#include "aoi.h"

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
#include "proto/common/event/scene_event.pb.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"
#include "core/utils/stat/stat.h"
#include "proto/scene/player_scene.pb.h"
#include "network/player_message_utils.h"
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


void AoiSystem::Update(double delta) {
    for (auto&& [entity, transform, sceneComp] : tlsEcs.actorRegistry.view<Transform, SceneEntityComp>().each()) {
        // Skip invalid scenes
        if (!tlsEcs.sceneRegistry.valid(sceneComp.sceneEntity)) {
            LOG_ERROR << "Scene not found for entity " << entt::to_integral(entity);
            continue;
        }

        auto& gridList = tlsEcs.sceneRegistry.get_or_emplace<SceneGridListComp>(sceneComp.sceneEntity);

        // Get current hex grid position
        const auto currentHex = GridSystem::CalculateHexPosition(transform);
        const auto currentGridId = GridSystem::GetGridId(currentHex);

        GridSet gridsToEnter, gridsToLeave;
        UpdateGridState(entity, gridList, currentHex, currentGridId, gridsToEnter, gridsToLeave);

        // Handle entity enter/leave visibility
        HandleEntityVisibility(entity, gridList, gridsToEnter, gridsToLeave);
    }
}

void AoiSystem::UpdateGridState(const entt::entity entity, SceneGridListComp& gridList, const Hex& currentHex,
                                const GridId currentGridId, GridSet& gridsToEnter, GridSet& gridsToLeave) {
    if (!tlsEcs.actorRegistry.any_of<Hex>(entity)) {
        // First time entering scene
        gridList[currentGridId].entities.insert(entity);
        tlsEcs.actorRegistry.emplace<Hex>(entity, currentHex);
        GridSystem::GetCurrentAndNeighborGridIds(currentHex, gridsToEnter);
    } else {
        // Position update
        const auto previousHex = tlsEcs.actorRegistry.get<Hex>(entity);
        if (hex_distance(previousHex, currentHex) == 0) {
            return;
        }

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
        gridList[previousGridId].entities.erase(entity);
        gridList[currentGridId].entities.insert(entity);

        // Hex has const members -> copy assignment is deleted -> emplace_or_replace won't compile.
        // Use remove + emplace instead.
        tlsEcs.actorRegistry.remove<Hex>(entity);
        tlsEcs.actorRegistry.emplace<Hex>(entity, currentHex);
    }
}

void AoiSystem::HandleEntityVisibility(entt::entity entity, SceneGridListComp& gridList, 
                                       const GridSet& gridsToEnter, const GridSet& gridsToLeave) {
    EntityUnorderedSet entitiesEnteringView, entitiesLeavingView;

    // Entities in grids the observer is entering
    for (const auto& gridId : gridsToEnter) {
        auto gridIt = gridList.find(gridId);
        if (gridIt == gridList.end()) continue;

        for (const auto& otherEntity : gridIt->second.entities) {
            if (otherEntity == entity) continue;

            if (ViewSystem::CanSee(entity, otherEntity)) {
                const auto priority = DetermineAoiPriority(entity, otherEntity);
                if (InterestSystem::AddAoiEntity(entity, otherEntity, priority)) {
                    entitiesEnteringView.insert(otherEntity);
                }
            }

            if (ViewSystem::CanSee(otherEntity, entity)) {
                const auto priority = DetermineAoiPriority(otherEntity, entity);
                InterestSystem::AddAoiEntity(otherEntity, entity, priority);
            }
        }
    }

    // Entities in grids the observer is leaving.
    // The observer's own AoiListComp does not change identity during this loop
    // (RemoveAoiEntity only erases entries), so look it up once.
    auto* entityAoi = tlsEcs.actorRegistry.try_get<AoiListComp>(entity);
    for (const auto& gridId : gridsToLeave) {
        auto gridIt = gridList.find(gridId);
        if (gridIt == gridList.end()) continue;

        for (const auto& otherEntity : gridIt->second.entities) {
            // Drop the observer's view of otherEntity, unless that entry is
            // pinned (pinned entries are managed by buff/skill lifecycle).
            if (entityAoi) {
                auto it = entityAoi->entries.find(otherEntity);
                if (it != entityAoi->entries.end() && it->second.priority != AoiPriority::kPinned) {
                    entitiesLeavingView.insert(otherEntity);
                    InterestSystem::RemoveAoiEntity(entity, otherEntity);
                }
            }

            // Reverse direction: remove entity from other's list (unless pinned there).
            auto* otherAoi = tlsEcs.actorRegistry.try_get<AoiListComp>(otherEntity);
            if (otherAoi) {
                auto it = otherAoi->entries.find(entity);
                if (it != otherAoi->entries.end() && it->second.priority != AoiPriority::kPinned) {
                    InterestSystem::RemoveAoiEntity(otherEntity, entity);
                }
            }
        }
    }

    NotifyEntityVisibilityChanges(entity, entitiesEnteringView, entitiesLeavingView);
}

void AoiSystem::NotifyEntityVisibilityChanges(entt::entity entity, 
                                              const EntityUnorderedSet& enteringEntities, 
                                              const EntityUnorderedSet& leavingEntities) {
    // Notify entities entering view
    if (!enteringEntities.empty()) {
        auto &createMsg = tlsEcs.globalRegistry.get_or_emplace<ActorListCreateS2C>(tlsEcs.GlobalEntity());
        createMsg.Clear();
        for (auto& otherEntity : enteringEntities) {
            ViewSystem::FillActorCreateMessageInfo(entity, otherEntity, *createMsg.add_actor_list());
        }
        SendMessageToClientViaGate(SceneSceneClientPlayerNotifyActorListCreateMessageId, createMsg, entity);
    }

    // Notify entities leaving view
    if (!leavingEntities.empty()) {
        auto &destroyMsg = tlsEcs.globalRegistry.get_or_emplace<ActorListDestroyS2C>(tlsEcs.GlobalEntity());
        destroyMsg.Clear();
        for (auto& otherEntity : leavingEntities) {
            destroyMsg.add_entity(entt::to_integral(otherEntity));
        }
        SendMessageToClientViaGate(SceneSceneClientPlayerNotifyActorListDestroyMessageId, destroyMsg, entity);
    }
}

void AoiSystem::BeforeLeaveSceneHandler(const BeforeLeaveScene& message) {
    const auto entity = entt::to_entity(message.entity());
    if (!tlsEcs.actorRegistry.valid(entity)) {
        LOG_ERROR << "Entity not found in scene";
        return;
    }

    // 把离场者自己的兴趣列表清空。
    //
    // 下面的 BroadcastEntityLeave 只做了反方向 —— 把离场者从各观察者的 AoiListComp
    // 里摘掉;离场者自己那张表从来没人清过。换场景时实体不销毁(player_scene.cpp
    // 只删了 Hex),于是旧场景的条目会原样带进新场景,而新场景的格子永远不会出现在
    // gridsToLeave 里,这些陈旧条目再也没有机会被删掉:
    //   1) 无上界泄漏,每换一次场景涨一截;
    //   2) 更要命的是它们占着 AOI 容量 —— 旧场景人多时列表直接是满的,
    //      新场景的实体在 AddAoiEntity 里因为权重不够被全部拒收,
    //      entitiesEnteringView 恒空 => 客户端收不到 ActorListCreate,
    //      表现为"进了新场景但世界是空的"。
    // 登出那条路径(player_lifecycle.cpp)实体随后就销毁,清一下也无副作用。
    if (auto* leaverAoiList = tlsEcs.actorRegistry.try_get<AoiListComp>(entity)) {
        leaverAoiList->entries.clear();
    }

    ECS_GET_OR_VOID(hex, Hex, entity);

    const auto *sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
    if (!sceneEntityComp)
    {
        LOG_ERROR << "Entity has no SceneEntityComp: " << entt::to_integral(entity);
        return;
    }
    auto sceneEntity = sceneEntityComp->sceneEntity;
    if (!tlsEcs.sceneRegistry.valid(sceneEntity))
    {
        LOG_ERROR << "Scene entity not found for entity " << entt::to_integral(entity);
		return;
    }

    auto& gridList = tlsEcs.sceneRegistry.get_or_emplace<SceneGridListComp>(sceneEntity);
    GridSet gridsToLeave;
    GridSystem::GetCurrentAndNeighborGridIds(*hex, gridsToLeave);

    RemoveEntityFromGrid(*hex, gridList, entity);
    BroadcastEntityLeave(gridList, entity, gridsToLeave);
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

void AoiSystem::BroadcastEntityLeave(const SceneGridListComp& gridList, entt::entity entity, const GridSet& gridsToLeave) {
    if (gridsToLeave.empty()) return;

    auto &destroyMsg = tlsEcs.globalRegistry.get_or_emplace<ActorDestroyS2C>(tlsEcs.GlobalEntity());
    destroyMsg.Clear();
    destroyMsg.set_entity(entt::to_integral(entity));

    EntityUnorderedSet observersToNotify;
    for (const auto& gridId : gridsToLeave) {
        auto gridIt = gridList.find(gridId);
        if (gridIt == gridList.end()) continue;

        for (const auto& observer : gridIt->second.entities) {
            observersToNotify.insert(observer);
            InterestSystem::RemoveAoiEntity(observer, entity);
        }
    }

    BroadcastMessageToPlayers(SceneSceneClientPlayerNotifyActorDestroyMessageId, destroyMsg, observersToNotify);
}

