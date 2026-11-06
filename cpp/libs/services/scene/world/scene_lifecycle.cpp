#include "scene_lifecycle.h"

#include "infra/agones/agones_gameserver_lifecycle.h"
#include "modules/scene/comp/scene_comp.h"
#include "player/system/player_lifecycle.h"
#include "proto/scene/scene.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "thread_context/ecs_context.h"
#include "world/scene_ownership_reconciler.h"
#include "world/scene_placement_fence.h"

const SceneInfoComp& SceneLifecycle::CreateScene(const CreateSceneRequest& request)
{
    const auto scene = tlsEcs.sceneRegistry.create();
    auto& info = tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene);
    info.set_scene_config_id(request.config_id());
    info.set_scene_id(request.scene_id());
    info.set_mirror_config_id(request.mirror_config_id());
    info.set_dungeon_config_id(request.dungeon_config_id());
    for (const auto creatorId : request.creator_ids())
    {
        info.mutable_creators()->insert({creatorId, true});
    }
    agones::GameServerLifecycle::Instance().OnUnitCreated(entt::to_integral(scene));
    return info;
}

std::size_t SceneLifecycle::DestroyScene(SceneEntity scene)
{
    if (!tlsEcs.sceneRegistry.valid(scene))
    {
        return 0;
    }

    // 有住户时先存盘、改派回主世界,不能把仍由 gate 路由到本节点的玩家留在无效场景里。
    // 排空由存盘回调异步收敛,本次保留场景;SceneManager 下一次幂等销毁重新观察真实住户状态。
    // 这条分支覆盖世界频道缩容与镜像级联;普通副本的上游 Lua 已拒绝销毁人数非零的实例。
    static_cast<void>(PlayerLifecycleSystem::BeginSceneDrain(scene));

    // 票据数不等于居民数：断线、退出中或身份不完整的实体仍可能保留绑定。
    // 排空可能同步解绑，必须在排空后按唯一成员事实重新确认，不能提前销毁场景。
    std::size_t remainingBindings = 0;
    for (const auto& [actor, binding] : tlsEcs.actorRegistry.view<SceneEntityComp>().each())
    {
        if (binding.sceneEntity == scene)
        {
            ++remainingBindings;
        }
    }
    if (remainingBindings != 0)
    {
        return remainingBindings;
    }
    if (!tlsEcs.sceneRegistry.valid(scene))
    {
        return 0;
    }
    agones::GameServerLifecycle::Instance().OnUnitDestroyed(entt::to_integral(scene));
    tlsEcs.sceneRegistry.destroy(scene);
    return 0;
}

const SceneInfoComp& SceneLifecycle::ReuseExistingScene(SceneEntity scene)
{
    scene_ownership::RestartCheck(scene);
    return tlsEcs.sceneRegistry.get<SceneInfoComp>(scene);
}

ScenePlacementFence& SceneLifecycle::PlacementFence()
{
    // 与 tlsEcs 同一个线程归属:只有节点主 EventLoop 线程建、拆场景。
    thread_local ScenePlacementFence fence;
    return fence;
}
