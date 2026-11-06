#pragma once

#include "spatial/comp/grid_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "engine/core/utils/registry/scene_entity.h"

struct Hex;
struct AoiVisibilityChanges;

// AOI 的场景层:六边形网格发现附近候选(空间来源),玩法登记显式关注,
// 关系与容量由 InterestSystem 维护,这里负责把可见性变化下发给客户端。
// 设计见 docs/design/aoi-directed-interest.md。只在所属场景线程上调用。
class AoiSystem {
public:
    static void Update(double delta);

    // 玩法建立显式关注:watcher 的客户端显示 target 并接收它的移动、属性与技能广播,
    // 与距离和网格无关;target 不因此看到 watcher。target 须与 watcher 同场景且存活。
    // 首次有效可见时立即给 watcher 发创建和当前位置。同一来源重复登记不重复创建。
    // 返回关系是否已登记(暂因隐身或容量不可见时也返回 true,条件满足后自动显示)。
    static bool Follow(entt::entity watcher, entt::entity target, const AoiFollowGrant& grant);

    // 撤销 watcher 对 target 的一个显式来源。其他来源仍在时继续同步;
    // 最后一个来源撤销时,target 仍在附近且可见就继续显示,否则给 watcher 发一次移除。
    static void Unfollow(entt::entity watcher, entt::entity target, const AoiFollowSource& source);

    // target 隐身状态变化后调用(BuffSystem 维护 StealthedTagComp 时):
    //   - 隐身开始:没有侦测授权的观察者暂停投递并收到一次移除,显式来源保留;
    //   - 隐身结束:暂停的显式关注恢复,附近能看见它的实体重新建立空间关注,都先发当前状态。
    static void RefreshTargetVisibility(entt::entity target);

    // 离场、换场景、销毁前的唯一 AOI 清理入口:按正反索引清掉该实体的全部出边与入边,
    // 正在显示它的观察者各收到一次移除;再把它移出格子。只访问与它直接关联的关系,
    // 不扫描全节点,也不依赖它仍有 Hex;可重复调用。所有 actor 销毁路径都必须先调它
    // (玩家经 PlayerSceneSystem::LeaveCurrentScene,DestroyPlayer 兜底补调)。
    static void OnLeaveScene(entt::entity entity);

    // 实体在**同一场景内**被瞬移之后调用(MovementSystem::TeleportWithinScene),调用前 Transform 必须
    // 已经写成新位置。效果等价于"从旧位置离开视野、在新位置重新进场":
    //   - 立刻:撤掉它看别人、别人看它的空间来源,失去全部来源的关系通知双方客户端移除;
    //     它被移出格子并摘掉 Hex。显式关注不受影响,新位置由调用方随后的移动广播送达。
    //   - 下一次 Update:没有 Hex 的实体走"首次进场"分支,按新位置对当前格及邻格重新做双向 CanSee。
    // 为什么不能只改 Transform:Update 只在**跨格**时重新评估可见性(同格内直接早退),同一个六边形内
    // 的瞬移既不会让新位置的邻居看到它,也不会让它看到邻居。
    static void ResetEntityVisibility(entt::entity entity);
private:
    // 首次真实入格时延迟创建 SceneGridListComp;位置未变返回 nullptr,不创建空表。
    static SceneGridListComp* UpdateGridState(entt::entity entity, SceneEntity sceneEntity,
                                const Hex& currentHex, GridId currentGridId,
                                GridSet& gridsToEnter, GridSet& gridsToLeave);
    static void HandleEntityVisibility(entt::entity entity, SceneGridListComp& gridList,
                                       const GridSet& gridsToEnter, const GridSet& gridsToLeave);
    static void RemoveEntityFromGrid(const Hex& hex, SceneGridListComp& gridList, entt::entity entity);

    // 把一批可见性变化下发给各观察者的客户端。先按(观察者, 目标)求净变化,同一批里
    // 显示又隐藏的对不发;再按观察者分组:只涉及一个目标的按目标合并成一次广播(同一消息体只组包一次),
    // 涉及多个目标的一条列表消息单发。departed 是正在离场的实体:它的移除改用 ActorDestroyS2C 广播。
    static void SendVisibilityChanges(const AoiVisibilityChanges& changes, entt::entity departed = entt::null);
};
