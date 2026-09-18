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
#include "player/system/asset_op_system.h"
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

    // 归属交接已发起(handoff 标记已写、EnterScene 已发)的实体,不走退出流程。
    //
    // scene_manager 是**先**异步发 ReleasePlayer、**后**提交落点(铸造 epoch + 写 location)和路由。
    // 落点或路由随后失败(Lua 撤回 / epoch 冲突 / Kafka 路由失败并回滚)时,location 与 gate 会话都
    // 还指向本节点;若这里已按"退出优先"把实体销毁,玩家的消息全部落空,只能重登。
    // 交接中的实体去留只由 EnterScene 应答与看门狗裁决(PlayerLifecycleSystem::ResolveTravelOutcome):
    // 放行了它会销毁实体,没放行它会解冻,两头都不需要这条通知。盘上已是最新状态,不存在
    // "不退出就丢存盘"的问题。
    //
    // **不要**在这里调 ResolveTravelOutcome:它第一步是 DEL handoff 标记,而 ReleasePlayer 可能早于
    // scene_manager 的铸造 Lua 到达 —— 等于源端自己把即将发生的放行撤回。
    //
    // 已知副作用(预期现象,压测时别当 bug 追):交接被放行到别的节点、而 EnterScene 应答又丢了
    // (scene_manager 在路由 ACK 之后重启 / 断连,生成的 gRPC 客户端 status 非 OK 不回调)时,
    // 本节点的源实体要等 30s 应答看门狗才销毁(日志 "travel_granted_without_reply")。这 30s 里它
    // 以冻结态留在源场景的 AOI 内,周围玩家会看到一个不动的分身;真身已在目标节点。数据安全:
    // 交接发起后本实体不再存盘,玩家再被派回本节点时 DiscardStaleHandoffEntity 会先销毁它再重载。
    // **也不要**改成"收到 ReleasePlayer 后几秒只读一次 owner_epoch、变了就销毁"来缩短它:
    // scene_manager 是先铸造 epoch、后发 Kafka 路由,路由失败(KafkaWriteTimeoutSeconds,默认 5s)
    // 会把 epoch 与 location 一起回滚到本节点。探测落在这段窗口里会读到一个即将被回滚的新 epoch,
    // 销毁实体之后 location 又指回本节点 —— 玩家在线却没有实体,只能重登;用一个观感问题换来
    // 一个卡死问题。应答路径没有这个竞态(scene_manager 在路由 ACK / 回滚完成之后才回应答),
    // 看门狗的 30s 则远大于那段窗口。
    if (PlayerLifecycleSystem::IsHandoffRequested(playerIt->second))
    {
        LOG_INFO << "[gRPC] ReleasePlayer: player " << playerId
                 << " has an ownership handoff in flight; leaving the outcome to the EnterScene reply / watchdog"
                 << " (target scene " << request->target_scene_id()
                 << " on node " << request->target_node_id()
                 << "; if the reply is lost the frozen source entity lingers until the reply watchdog fires)";
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

void SceneNodeGrpcImpl::HandleAssetDebit(const ::AssetOpRequest* request,
    ::AssetOpResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    // 通用资产通道(guild-phase2/04-asset-channel.md §S4):Go 服务扣玩家资产。
    // 外层已 runInLoop 投递到 loop 线程,系统层可直接同步访问 ECS;
    // 结局写在 response.outcome,gRPC status 恒为 OK。同 seq 重复调用只读答复。
    PlayerAssetOpSystem::Debit(*request, *response);
///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleAssetAbortDebit(const ::AssetOpRequest* request,
    ::AssetOpResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    // 中止占位:未见过的 seq 记 REJECTED(reason=0),此后这条 seq 永远拒绝;
    // 已见过则回原结局。允许用于任何流,不校验 tx_type。
    PlayerAssetOpSystem::AbortDebit(*request, *response);
///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleAssetCredit(const ::AssetOpRequest* request,
    ::AssetOpResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    // 通用资产通道:Go 服务给玩家发货币 / 物品。只发放了一部分时回
    // APPLIED + partial=true,Go 不做对侧入账,转人工补偿(§4.33)。
    PlayerAssetOpSystem::Credit(*request, *response);
///<<< END WRITING YOUR CODE
}

grpc::Status SceneNodeGrpcImpl::CreateScene(grpc::ServerContext* /*context*/,
    const ::CreateSceneRequest* request,
    ::CreateSceneResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    // Agones 高密度模式:进程必须先被 Allocate,才能创建第一个场景。
    //
    // 刻意放在 runInLoop 之前(gRPC 线程上):POST /allocate 是网络调用,
    // 放进事件循环会卡住整个逻辑帧;这里阻塞的只是一个 gRPC 池线程,
    // 且等待有上限(LifecycleOptions::allocateWaitTimeout)。
    //
    // 失败一律 fail-closed:不建实体,返回非 OK,让 SceneManager 回滚重试,
    // 而不是留下一个 Agones 不知道的房间。createPermit 必须活到本函数返回
    // (覆盖 future.get()),在途创建才能阻止提前回到 Ready。
    //
    // 2026-09-15 恢复:原块在 commit 6c4021ae5 被 proto 重生成吞掉(当时位于守护段外)。
    auto createPermit = agones::SceneLifecycle::Instance().AcquireCreatePermitBlocking();
    if (!createPermit)
    {
        LOG_ERROR << "[gRPC] CreateScene rejected: Agones allocate not confirmed, scene_id="
                  << request->scene_id() << " state="
                  << agones::ToString(agones::SceneLifecycle::Instance().State());
        return grpc::Status(grpc::StatusCode::UNAVAILABLE, "agones allocate not confirmed");
    }
///<<< END WRITING YOUR CODE
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
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
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
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
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
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
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
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, &promise]
                    {
        HandleCancelBattlePrepare(request);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}
