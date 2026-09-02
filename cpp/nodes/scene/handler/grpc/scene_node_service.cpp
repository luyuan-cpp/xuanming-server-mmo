#include "scene_node_service.h"
#include <future>
///<<< BEGIN WRITING YOUR CODE
#include "agones/agones_scene_lifecycle.h"
#include "modules/scene/comp/scene_node_comp.h"
#include "player/system/player_lifecycle.h"
#include "proto/common/event/scene_event.pb.h"
#include "thread_context/ecs_context.h"
#include "muduo/base/Logging.h"
#include "battle/system/player_battle.h"
///<<< END WRITING YOUR CODE

SceneNodeGrpcImpl::SceneNodeGrpcImpl(muduo::net::EventLoop& loop)
    : loop_(loop)
{
}

void SceneNodeGrpcImpl::HandleCreateScene(const ::CreateSceneRequest* request,
    ::CreateSceneResponse* response)
{
    ///<<< BEGIN WRITING YOUR CODE
    // Idempotent: deduplicate by scene_id.
    {
        auto view = tlsEcs.sceneRegistry.view<SceneInfoComp>();
        for (auto entity : view)
        {
            const auto &info = view.get<SceneInfoComp>(entity);
            if (info.scene_id() == request->scene_id())
            {
                response->mutable_scene_info()->CopyFrom(info);
                LOG_INFO << "[gRPC] CreateScene: scene_id=" << request->scene_id()
                         << " already exists, returning existing";
                return;
            }
        }
    }

    auto sceneEntity = tlsEcs.sceneRegistry.create();

    auto &sceneInfo = tlsEcs.sceneRegistry.emplace<SceneInfoComp>(sceneEntity);
    sceneInfo.set_scene_config_id(request->config_id());
    sceneInfo.set_mirror_config_id(request->mirror_config_id());
    sceneInfo.set_dungeon_config_id(request->dungeon_config_id());
	sceneInfo.set_scene_id(request->scene_id());

    for (const auto creator_id : request->creator_ids())
    {
        sceneInfo.mutable_creators()->insert({creator_id, true});
    }

    tlsEcs.sceneRegistry.emplace<ScenePlayers>(sceneEntity);

    OnSceneCreated event;
    event.set_entity(entt::to_integral(sceneEntity));
    tlsEcs.dispatcher.trigger(event);

    response->mutable_scene_info()->CopyFrom(sceneInfo);

    LOG_INFO << "[gRPC] CreateScene: created entity=" << entt::to_integral(sceneEntity)
             << " config_id=" << request->config_id()
             << " scene_id=" << sceneInfo.scene_id()
             << " mirror_config_id=" << sceneInfo.mirror_config_id()
             << " dungeon_config_id=" << sceneInfo.dungeon_config_id();
    ///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleDestroyScene(const ::DestroySceneRequest* request)
{
    ///<<< BEGIN WRITING YOUR CODE
    const auto sceneId = request->scene_id();

    entt::entity targetEntity = entt::null;
    auto view = tlsEcs.sceneRegistry.view<SceneInfoComp>();
    for (auto entity : view)
    {
        const auto &info = view.get<SceneInfoComp>(entity);
        if (info.scene_id() == sceneId)
        {
            targetEntity = entity;
            break;
        }
    }

    if (targetEntity == entt::null)
    {
        LOG_WARN << "[gRPC] DestroyScene: scene_id=" << sceneId << " not found, idempotent OK";
        return;
    }

    // Drain-then-destroy.
    //
    // A scene that still has residents must NOT be dropped on the floor: the
    // players would keep a gate session pointing at an entity that no longer
    // exists. Relocate them to the main world first (same path as the node
    // evacuation: persist -> re-home via SceneManager.EnterScene(0,0) ->
    // drop the local entity), and leave the scene entity alone for now.
    //
    // Returning without destroying is deliberate. Relocation is asynchronous
    // (it completes on the persist callback), so the scene cannot be empty
    // yet. SceneManager's DestroyScene is idempotent and its autoscaler
    // retries on the next tick -- by then the residents are gone and the
    // second call takes the normal path. This is convergence, not a retry
    // hack: every tick re-observes real state instead of assuming success.
    //
    // Instances never hit this branch (the Lua CAS refuses to destroy a scene
    // with player_count > 0). It fires for world-channel scale-in and for
    // mirror cascade destroys, where residents genuinely can be present.
    if (const std::size_t relocating = PlayerLifecycleSystem::BeginSceneDrain(targetEntity); relocating > 0)
    {
        LOG_WARN << "[gRPC] DestroyScene: scene_id=" << sceneId << " still has residents; "
                 << "relocating " << relocating << " player(s) to the main world, "
                 << "entity kept until drained";
        return;
    }

    OnSceneDestroyed event;
    event.set_entity(entt::to_integral(targetEntity));
    tlsEcs.dispatcher.trigger(event);

    tlsEcs.sceneRegistry.destroy(targetEntity);

    LOG_INFO << "[gRPC] DestroyScene: destroyed scene_id=" << sceneId;
    ///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleReleasePlayer(const ::scene_node::ReleasePlayerRequest* request)
{
    ///<<< BEGIN WRITING YOUR CODE
    const auto playerId = request->player_id();
    auto playerIt = tlsEcs.playerList.find(playerId);
    if (playerIt == tlsEcs.playerList.end())
    {
        LOG_DEBUG << "[gRPC] ReleasePlayer: player " << playerId
                  << " not on this node, idempotent OK";
        return;
    }

    LOG_INFO << "[gRPC] ReleasePlayer: releasing player " << playerId
             << " moving to scene " << request->target_scene_id()
             << " on node " << request->target_node_id();

    PlayerLifecycleSystem::HandleExitGameNode(playerIt->second);
    ///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandlePrepareBattle(const ::PrepareBattleRequest* request,
    ::PrepareBattleResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    // match -> scene 备战冻结(外层已 runInLoop 投递到 loop 线程,可直接同步访问 ECS;
    // 失败原因经 response.error_message 返回,gRPC status 恒为 OK)
    PlayerBattleSystem::PrepareBattle(*request, *response);
///<<< END WRITING YOUR CODE}
}

void SceneNodeGrpcImpl::HandleCancelBattlePrepare(const ::CancelBattlePrepareRequest* request)
{
///<<< BEGIN WRITING YOUR CODE
    // gather 失败补偿解冻:battle_id 匹配才摘 InBattleComp + DEL battle:lock,幂等
    PlayerBattleSystem::CancelBattlePrepare(*request);
///<<< END WRITING YOUR CODE}
}

grpc::Status SceneNodeGrpcImpl::CreateScene(grpc::ServerContext* /*context*/,
    const ::CreateSceneRequest* request,
    ::CreateSceneResponse* response)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, response, &promise]
                    {
        HandleCreateScene(request, response);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

grpc::Status SceneNodeGrpcImpl::DestroyScene(grpc::ServerContext* /*context*/,
    const ::DestroySceneRequest* request,
    ::Empty* response)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, &promise]
                    {
        HandleDestroyScene(request);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

grpc::Status SceneNodeGrpcImpl::ReleasePlayer(grpc::ServerContext* /*context*/,
    const ::scene_node::ReleasePlayerRequest* request,
    ::Empty* response)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, &promise]
                    {
        HandleReleasePlayer(request);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

grpc::Status SceneNodeGrpcImpl::PrepareBattle(grpc::ServerContext* /*context*/,
    const ::PrepareBattleRequest* request,
    ::PrepareBattleResponse* response)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, response, &promise]
                    {
        HandlePrepareBattle(request, response);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

grpc::Status SceneNodeGrpcImpl::CancelBattlePrepare(grpc::ServerContext* /*context*/,
    const ::CancelBattlePrepareRequest* request,
    ::Empty* response)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, &promise]
                    {
        HandleCancelBattlePrepare(request);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}
