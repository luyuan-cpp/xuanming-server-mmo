#pragma once

#include "spatial/comp/grid_comp.h"
#include "engine/core/utils/registry/scene_entity.h"


struct Hex;

class AoiSystem {
public:
    static void Update(double delta);

    // 离场只清自己的兴趣表及当前格/邻格观察者,不扫描全节点。
    static void OnLeaveScene(entt::entity entity);

    // 实体在**同一场景内**被瞬移之后调用(MovementSystem::TeleportWithinScene),调用前 Transform 必须
    // 已经写成新位置。效果等价于"从旧位置离开视野、在新位置重新进场":
    //   - 立刻:它看到的非 kPinned 条目全部摘掉并通知它自己的客户端销毁;旧格子及邻格里看着它的
    //     观察者同样摘掉并收到销毁(对方把它钉住的条目不动);它被移出格子并摘掉 Hex。
    //   - 下一次 Update:没有 Hex 的实体走"首次进场"分支,按新位置对当前格及邻格重新做双向 CanSee。
    // 为什么不能只改 Transform:Update 只在**跨格**时重新评估可见性(同格内直接早退),同一个六边形内
    // 的瞬移既不会让新位置的邻居看到它,也不会让它看到邻居。
    // 还没进过格子的实体(刚进场、首次 Update 之前)只清自己的兴趣表,其余交给首次 Update。
    // 已知边界:把它钉住(kPinned)的观察者既不收销毁也不收重建;移动广播只发给互相可见的玩家,
    // 所以它在那些客户端上会一直停在瞬移前的位置,直到双方重新互相可见。
    // 目前没有任何玩法会钉住玩家,真有了要给钉住方补一条瞬移通知。
    static void ResetEntityVisibility(entt::entity entity);
private:
    // 首次真实入格时延迟创建 SceneGridListComp;位置未变返回 nullptr,不创建空表。
    static SceneGridListComp* UpdateGridState(entt::entity entity, SceneEntity sceneEntity,
                                const Hex& currentHex, GridId currentGridId,
                                GridSet& gridsToEnter, GridSet& gridsToLeave);
    static void HandleEntityVisibility(entt::entity entity, SceneGridListComp& gridList,
                                       const GridSet& gridsToEnter, const GridSet& gridsToLeave);
    static void NotifyEntityVisibilityChanges(entt::entity entity,
                                              const EntityUnorderedSet& enteringEntities, 
                                              const EntityUnorderedSet& leavingEntities);
    // 同一实体的进出通知按观察者集合广播，自身的视野增删列表仍单独发送。
    static void BroadcastEntityVisibilityChanges(entt::entity entity,
                                                 const EntityUnorderedSet& enteringObservers,
                                                 const EntityUnorderedSet& leavingObservers);
    static void RemoveEntityFromGrid(const Hex& hex, SceneGridListComp& gridList, entt::entity entity);

    static void BroadcastEntityLeave(const SceneGridListComp& gridList, entt::entity entity, const GridSet& gridsToLeave);
};
