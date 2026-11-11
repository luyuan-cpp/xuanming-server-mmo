#include "grid.h"

#include <muduo/base/Logging.h>

#include "hexagons_grid.h"
#include "view.h"
#include "proto/common/component/actor_comp.pb.h"
#include "spatial/constants/view.h"

#include <modules/scene/comp/scene_comp.h>
#include <thread_context/ecs_context.h>

const Point kDefaultSize(kAoiHexSize, kAoiHexSize);
const Point kOrigin(0.0, 0.0);
const auto kHexLayout = Layout(layout_flat, kDefaultSize, kOrigin);

absl::uint128 GridSystem::GetGridId(const Vector3& pos) {
    return GetGridId(hex_round(pixel_to_hex(kHexLayout, Point(pos.x(), pos.y()))));
}

absl::uint128 GridSystem::GetGridId(const Hex& hex) {
    return static_cast<absl::uint128>(hex.q) << 64 | static_cast<uint64_t>(hex.r);
}

Hex GridSystem::CalculateHexPosition(const Transform& transform) {
    return hex_round(pixel_to_hex(kHexLayout, Point(transform.location().x(), transform.location().y())));
}

void GridSystem::GetNeighborGridIds(const Hex& hex, GridSet& gridSet) {
    for (int i = 0; i < 6; ++i) {
        gridSet.emplace(GridSystem::GetGridId(hex_neighbor(hex, i)));
    }
}

void GridSystem::GetCurrentAndNeighborGridIds(const Hex& hex, GridSet& gridSet) {
    auto currentGridId = GridSystem::GetGridId(hex);
    gridSet.emplace(currentGridId);
    GetNeighborGridIds(hex, gridSet);
}

namespace {
// 空间查询是只读路径,不得改变场景实体的组件集合。
// 格子表在首次真实入格时创建;活场景没有表表示尚无入格记录,正常返回空。
// 场景失效仍是错误,返回 nullptr,由调用方按"查不到任何实体"处理。
// try_get 对陈旧句柄安全返回空；先 valid() 只为区分场景已销毁（错误）与尚无格子表（正常空结果）。
const SceneGridListComp* FindSceneGridList(entt::entity actor, SceneEntity sceneEntity)
{
    if (!tlsEcs.sceneRegistry.valid(sceneEntity)) {
        LOG_ERROR << "Scene not found for entity " << entt::to_integral(actor)
                  << ", scene " << entt::to_integral(sceneEntity);
        return nullptr;
    }

    return tlsEcs.sceneRegistry.try_get<SceneGridListComp>(sceneEntity);
}
}

void GridSystem::GetEntitiesInGridAndNeighbors(entt::entity entity, EntityUnorderedSet& entities, bool excludingSelf)
{
    if (!tlsEcs.actorRegistry.valid(entity)) {
        LOG_ERROR << "Entity not found in scene";
        return;
    }
  
    const auto hexPosition = tlsEcs.actorRegistry.try_get<Hex>(entity);
    if (!hexPosition) {
        return;
    }

    const auto sceneComponent = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
    if (!sceneComponent) {
        return;
    }

    const auto* gridList = FindSceneGridList(entity, sceneComponent->sceneEntity);
    if (gridList == nullptr) {
        return;
    }

    GridSet grids;
    GetCurrentAndNeighborGridIds(*hexPosition, grids);

    for (const auto& gridId : grids)
    {
        auto gridIt = gridList->find(gridId);
        if (gridIt == gridList->end())
        {
            continue;
        }

        for (const auto& gridEntity : gridIt->second.entities)
        {
            entities.emplace(gridEntity);
        }
    }

    if (excludingSelf)
    {
        entities.erase(entity);
    }
}

void GridSystem::CollectNearbyObserversInRange(entt::entity entity, EntityUnorderedSet& entities)
{
    if (!tlsEcs.actorRegistry.valid(entity)) {
        LOG_ERROR << "Entity not found in scene";
        return;
    }
  
    const auto hexPosition = tlsEcs.actorRegistry.try_get<Hex>(entity);
    if (!hexPosition) {
        return;
    }

    const auto sceneComponent = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
    if (!sceneComponent) {
        return;
    }

    const auto* gridList = FindSceneGridList(entity, sceneComponent->sceneEntity);
    if (gridList == nullptr) {
        return;
    }

    GridSet inViewGrids;
    GetCurrentAndNeighborGridIds(*hexPosition, inViewGrids);

    for (const auto& gridId : inViewGrids)
    {
        auto gridIt = gridList->find(gridId);
        if (gridIt == gridList->end())
        {
            continue;
        }

        for (const auto& gridEntity : gridIt->second.entities)
        {
            if (gridEntity == entity)
            {
                continue;
            }

            double viewRadius = ViewSystem::GetMaxViewRadius(gridEntity);
            
            if (!ViewSystem::IsWithinViewRadius(gridEntity, entity, viewRadius))
            {
                continue;
            }
            
            entities.emplace(gridEntity);
        }
    }
}

void GridSystem::LogEmptyGridErrors() {
    for (auto&& [sceneEntity, gridList] : tlsEcs.sceneRegistry.view<SceneGridListComp>().each()) {
        for (const auto& [gridId, entityList] : gridList) {
            if (entityList.entities.empty()) {
                LOG_ERROR << "Grid is empty but not removed";
            }
        }
    }
}
