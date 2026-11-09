#pragma once

#include <absl/hash/hash.h>
#include <absl/numeric/int128.h>

#include "spatial/comp/grid_comp.h"

class Transform;
struct Hex;
class Vector3;

class GridSystem
{
public:
    static absl::uint128 GetGridId(const Vector3& pos);
    static absl::uint128 GetGridId(const Hex& hex);
    static Hex CalculateHexPosition(const Transform& transform);
    static void GetNeighborGridIds(const Hex& hex, GridSet& gridSet);
    static void GetCurrentAndNeighborGridIds(const Hex& hex, GridSet& gridSet);
    static void GetEntitiesInGridAndNeighbors(entt::entity entity, EntityUnorderedSet& entities, bool excludingSelf);
    // 追加本格及六个邻格中、自身视野半径覆盖 entity 的观察者距离候选，不含 entity。
    // 不判定关注关系、隐身或容量，不能当作实际可见收件人。
    static void CollectNearbyObserversInRange(entt::entity entity, EntityUnorderedSet& entities);
    // 诊断空格子并记错，不更新格子或计算尺寸。
    static void LogEmptyGridErrors();
};
