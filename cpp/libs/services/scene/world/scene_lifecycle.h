#pragma once

#include "engine/core/utils/registry/scene_entity.h"

#include <cstddef>
#include <cstdint>

class CreateSceneRequest;
class SceneInfoComp;
class ScenePlacementFence;

class SceneLifecycle
{
public:
    // 仅在节点 EventLoop 调用。入口先校验非零 scene_id、按号去重,并持有 Agones 创建许可。
    // config_id 的传输层差异暂保留:muduo 要求非零,gRPC 允许零,此处不另加准入规则。
    // 返回值借用 sceneRegistry 中的组件,调用方须在下一次 registry 修改前复制回包。
    [[nodiscard]] static const SceneInfoComp& CreateScene(const CreateSceneRequest& request);

    // 返回发起排空后仍保留的场景绑定数，非零时禁止销毁；无效句柄幂等返回零。
    // 改派票据数不能代替绑定数，断线或身份不完整的居民同样必须先解绑。
    [[nodiscard]] static std::size_t DestroyScene(SceneEntity scene);

    // CreateScene 按 scene_id 命中本节点已有实体时调用(两个 CreateScene 入口的去重分支),scene 必须有效且挂着
    // SceneInfoComp(FindSceneById 的结果)。scene_manager 改派世界频道是「先建后改」:先 CreateScene、返回后再 CAS
    // scene:{id}:node,命中已有实体可能正是一次迁回本节点的预建,映射还没改过来。这里把该实体的归属核对从头算起
    // (SceneOwnershipReconciler 的宽限期,docs/design/scene-node-ownership-reconcile.md),预建与 CAS 之间它不会被
    // 当成「不归本节点」拆掉。返回已有实体的 SceneInfoComp 供回包,借用规则同 CreateScene。
    [[nodiscard]] static const SceneInfoComp& ReuseExistingScene(SceneEntity scene);

    // 本线程(节点 EventLoop)的放置代次栅栏(world/scene_placement_fence.h,docs/design/world-channel-placement-generation.md)。
    // 两个 CreateScene 入口在按 scene_id 去重之前、两个 DestroyScene 入口在查找实体之前判定:过期的建场景回空应答,
    // 过期的拆场景忽略。节点自己的归属对账器直接调 DestroyScene,不经这里。
    [[nodiscard]] static ScenePlacementFence& PlacementFence();

    // CreateScene 的 zone 核对(docs/design/redis-cluster-all-domains-20261110.md §3.4),两个入口在任何副作用之前判定
    // (gRPC 面在等 Agones 创建许可之前)。scene_manager 把场景的控制域键写在请求 zone 的 {ctl:<z>} 下,本节点的归属对账
    // 只按自己的 zone 拼键:建了别的 zone 的场景,对账读不到它的属主,确认两次就会拆掉这个活场景。所以 zone 不是本节点的
    // 就拒建,没填(0)同样拒建。纯函数。
    [[nodiscard]] static constexpr bool AdmitsCreateZone(std::uint32_t requestZoneId, std::uint32_t nodeZoneId)
    {
        return requestZoneId != 0 && requestZoneId == nodeZoneId;
    }
};
