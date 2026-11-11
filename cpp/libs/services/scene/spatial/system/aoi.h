#pragma once

#include "spatial/comp/grid_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "engine/core/utils/registry/scene_entity.h"

struct Hex;
struct AoiVisibilityChanges;
class Transform;

// AOI 的场景层:六边形网格发现附近候选,按距离维护空间来源;玩法登记显式关注;
// 关系与容量由 InterestSystem 维护,这里负责把可见性变化下发给客户端。
// 设计见 docs/design/aoi-directed-interest.md。只在所属场景线程上调用。
class AoiSystem {
public:
    // 每个模拟步调用一次。对每个在场实体:
    //   1. 按当前位置更新它所在的格子(格子只用来找进视野的候选);
    //   2. 位置自上次评估变了、到了该评估的时候(规则见 spatial/constants/view.h 的 kAoiReevaluateDistance),
    //      或者这是它第一次入格,或者它有附近候选因名额被拒、现在又有空名额(AoiNearbyRefillComp),
    //      就重新评估它与附近实体的空间关注,两个方向都判:
    //      距离超出离开半径的撤掉,本格及邻格里进入视野半径的建立。
    // 距离是连续变化的,所以必须按位移重判,不能只在跨格时判:否则同一组格子里从 15 m 走到 5 m 永远进不了视野。
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
    //   - 下一次 Update:没有 Hex 的实体走"首次入格"分支,按新位置对当前格及邻格重新做双向评估。
    // 为什么不交给 Update 的位移重判:重判按滞后区间处理,旧位置 20 m 内的观察者会继续看见它;
    // 瞬移不是走过去的,旧位置的观察者必须立刻收到移除,新位置只按进视野半径重建。
    static void ResetEntityVisibility(entt::entity entity);
private:
    // 把 entity 登记到 currentHex 对应的格子(格子变了才挪)。返回 true 表示这是它第一次进入
    // 本场景的格子(刚进场、换场景、同场景瞬移之后),调用方要立即做一次完整评估。
    // 首次真实入格时延迟创建 SceneGridListComp;位置所在格不变时不创建空表。
    static bool UpdateGridMembership(entt::entity entity, SceneEntity sceneEntity, const Hex& currentHex);

    // 按 entity 的当前位置重新评估它与附近实体的空间关注(两个方向),下发变化,并记下评估位置。
    static void ReevaluateVisibility(entt::entity entity, const Transform& transform);

    static void RemoveEntityFromGrid(const Hex& hex, SceneGridListComp& gridList, entt::entity entity);

    // 把一批可见性变化下发给各观察者的客户端。先按(观察者, 目标)求净变化,同一批里
    // 显示又隐藏的对不发;再按观察者分组:只涉及一个目标的按目标合并成一次广播(同一消息体只组包一次),
    // 涉及多个目标的一条列表消息单发。departed 是正在离场的实体:它的移除改用 ActorDestroyS2C 广播。
    static void SendVisibilityChanges(const AoiVisibilityChanges& changes, entt::entity departed = entt::null);
};
