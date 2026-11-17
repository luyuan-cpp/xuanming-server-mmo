#include "scene_node_service.h"
///<<< BEGIN WRITING YOUR CODE
#include "infra/agones/agones_gameserver_lifecycle.h"
#include "engine/core/async/coroutine_callback.h"
#include <boost/cobalt/task.hpp>
#include "proto/scene/scene_info.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "type_alias/player_session_type_alias.h"
#include "player/system/player_lifecycle.h"
#include "player/system/role_admission_system.h"
#include "player/system/player_scene.h"
#include "world/scene_lifecycle.h"
#include "world/scene_placement_fence.h"
#include "spatial/system/nav_query.h"
#include "spatial/manager/scene_nav.h"
#include "thread_context/ecs_context.h"
#include "network/node_utils.h" // GetZoneId:CreateScene 的 zone 核对
#include "muduo/base/Logging.h"
#include "battle/system/player_battle.h"
#include "player/system/asset_op_system.h"
#include "gm_player_command_security.h"
#include "../rpc/player/player_gm_guard.h"
#include "node/system/node/node.h"
#include "modules/currency/system/currency_system.h"
#include "modules/scene/comp/scene_comp.h"
#include "player/system/player_attribute.h"
#include "player/system/player_equip.h"
#include "player/system/player_pet.h"
#include "player/system/player_mutability.h"
#include "player/system/connection_lost_hold.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include <memory>

namespace
{
// 停机开始后 CreateScene 的拒绝文案(grpc-muduo-transport.md §15.3 Q9)。不是跨语言契约:
// SceneManager 收到任何错误都回滚,不匹配文案。
constexpr char kSceneNodeShuttingDownMessage[] = "scene_node_shutting_down";
// 请求的 zone 不是本节点的(SceneLifecycle::AdmitsCreateZone)时的拒绝文案,同样不是跨语言契约。
constexpr char kSceneZoneMismatchMessage[] = "scene_zone_mismatch";
boost::cobalt::task<void> CreateSceneWithPermit(muduo::net::EventLoop& loop,
    std::shared_ptr<GrpcUnaryCall<::CreateSceneRequest, ::CreateSceneResponse>> call)
{
    using Permit = agones::GameServerLifecycle::AllocationPermit;
    auto permit = co_await loop_async::AwaitCallback<std::unique_ptr<Permit>>(loop,
        [&loop](auto done) {
            agones::GameServerLifecycle::Instance().AcquireAllocationPermitAsync(loop,
                [done](Permit value) { done(std::make_unique<Permit>(std::move(value))); });
            return true;
        }, nullptr);
    // 三种拒绝都回 UNAVAILABLE,SceneManager 收到任何错误都回滚改派;日志按原因分开记,与改造前的回调一致。
    const auto sceneId = call->request().scene_id();
    if (!permit || (co_await boost::cobalt::this_coro::cancelled) != boost::asio::cancellation_type::none)
    {
        // CloseCreationAdmission 停掉了等许可的任务:维护 P1、普通停机、身份冲突或计划排空已开始。
        LOG_WARN << "[gRPC] CreateScene rejected while waiting for permit: creation admission closed, scene_id=" << sceneId;
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kSceneNodeShuttingDownMessage));
        co_return;
    }
    if (call->ServerDraining())
    {
        LOG_WARN << "[gRPC] CreateScene rejected after permit wait: scene node is shutting down, scene_id=" << sceneId;
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kSceneNodeShuttingDownMessage));
        co_return;
    }
    if (!*permit || !**permit)
    {
        LOG_ERROR << "[gRPC] CreateScene rejected: Agones allocate not confirmed, scene_id=" << sceneId;
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, "agones allocate not confirmed"));
        co_return;
    }
    call->Proceed(); // 许可的 RAII 覆盖同步创建，晚到的未消费许可自行释放。
}

} // namespace
///<<< END WRITING YOUR CODE

SceneNodeGrpcImpl::SceneNodeGrpcImpl(muduo::net::EventLoop& loop)
    : loop_(loop)
{
}

void SceneNodeGrpcImpl::HandleCreateScene(const ::CreateSceneRequest* request,
    ::CreateSceneResponse* response)
{
    ///<<< BEGIN WRITING YOUR CODE
    if (request->scene_id() == 0)
    {
        LOG_ERROR << "[gRPC] CreateScene: scene_id is 0";
        return;
    }

    if (NavQuerySystem::RequiresNavigation(request->config_id()) &&
        SceneNavManager::Instance().Get(request->config_id()) == nullptr)
    {
        LOG_ERROR << "[gRPC] CreateScene rejected: required navigation unavailable, config_id=" << request->config_id();
        return;
    }

    // 放置代次(docs/design/world-channel-placement-generation.md):本节点已按 >= 这个代次拆过这个场景时,
    // 这是一次晚到的旧建场景,不建、不复用,回空应答,scene_manager 按建场景失败处理。判在去重之前:过期的建场景不走
    // ReuseExistingScene,不重算归属对账的宽限期。Agones 许可在本函数之前的协程里等,等待期间到达的拆场景已经记下。
    if (auto& fence = SceneLifecycle::PlacementFence();
        !fence.AdmitCreate(request->scene_id(), request->placement_generation()))
    {
        LOG_WARN << "[gRPC] CreateScene: scene_id=" << request->scene_id()
                 << " placement_generation=" << request->placement_generation()
                 << " is stale (destroyed through " << fence.Find(request->scene_id())->destroyedThrough << "), rejected";
        return;
    }

    if (const SceneEntity existing = PlayerSceneSystem::FindSceneById(request->scene_id()); existing != entt::null)
    {
        // 命中已有实体可能是 scene_manager「先建后改」迁回本节点的预建:归属核对从头算宽限期(见 ReuseExistingScene)。
        response->mutable_scene_info()->CopyFrom(SceneLifecycle::ReuseExistingScene(existing));
        LOG_INFO << "[gRPC] CreateScene: scene_id=" << request->scene_id()
                 << " already exists (entity=" << entt::to_integral(existing) << "), returning existing";
        return;
    }

    response->mutable_scene_info()->CopyFrom(SceneLifecycle::CreateScene(*request));

    LOG_INFO << "[gRPC] CreateScene: created config_id=" << request->config_id()
             << " scene_id=" << request->scene_id()
             << " mirror_config_id=" << request->mirror_config_id()
             << " dungeon_config_id=" << request->dungeon_config_id();
    ///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleDestroyScene(const ::DestroySceneRequest* request)
{
    ///<<< BEGIN WRITING YOUR CODE
    const auto sceneId = request->scene_id();
    if (sceneId == 0)
    {
        LOG_ERROR << "[gRPC] DestroyScene: scene_id is 0";
        return;
    }

    // 放置代次:本节点已按更新的代次建过这个场景时,这是一次晚到的旧拆除(之后有过一次合法的放置),忽略、照常回 OK。
    // 代次 0(世界频道的退役拆除、副本与镜像)无条件执行。判在查找实体之前:实体不在也要记下这次拆除,挡住同一次尝试
    // 晚到的建场景。
    if (auto& fence = SceneLifecycle::PlacementFence(); !fence.AdmitDestroy(sceneId, request->placement_generation()))
    {
        LOG_WARN << "[gRPC] DestroyScene: scene_id=" << sceneId
                 << " placement_generation=" << request->placement_generation()
                 << " is older than the newest create " << fence.Find(sceneId)->newestCreate << ", ignored";
        return;
    }

    const SceneEntity targetEntity = PlayerSceneSystem::FindSceneById(sceneId);

    if (targetEntity == entt::null)
    {
        LOG_WARN << "[gRPC] DestroyScene: scene_id=" << sceneId << " not found, idempotent OK";
        return;
    }

    if (const std::size_t remainingBindings = SceneLifecycle::DestroyScene(targetEntity); remainingBindings > 0)
    {
        LOG_WARN << "[gRPC] DestroyScene: scene_id=" << sceneId << " still has "
                 << remainingBindings << " scene binding(s), entity kept until drained";
        return;
    }

    LOG_INFO << "[gRPC] DestroyScene: destroyed scene_id=" << sceneId;
    ///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleReleasePlayer(const ::scene_node::ReleasePlayerRequest* request)
{
    ///<<< BEGIN WRITING YOUR CODE
    // 已在 loop 线程(gRPC 服务端在节点 EventLoop 上派发)。查找、交接守卫、按期望 owner_epoch 的归属栅栏与退出都在库里:
    // PlayerLifecycleSystem::HandleReleaseFromSceneManager(docs/design/scene-enter-chain-residuals.md §4,
    // 纯判定见 player/system/release_fence.h)。session_ended 是会话结束释放(没有接手节点,
    // docs/design/location-release-binding.md §3.3),走 HandleSessionEndedRelease;covered_* 是这次释放覆盖到的最新登录身份
    // (§13,代次 0 = 旧版 scene_manager),实体已绑上更新的登录时不退出。交接释放不看 covered_*。
    // keep_held 是托管归位撞门时的释放(player-connection-lost-hold.md §4.5):本节点持有就什么都不做,只在完全不持有时补凭证。
    if (request->session_ended())
    {
        PlayerLifecycleSystem::HandleSessionEndedRelease(request->player_id(), request->expected_owner_epoch(),
                                                         request->covered_session_id(),
                                                         request->covered_login_generation(), request->keep_held());
        return;
    }
    PlayerLifecycleSystem::HandleReleaseFromSceneManager(request->player_id(), request->expected_owner_epoch(),
                                                         request->target_scene_id(), request->target_node_id());
    ///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandlePrepareBattle(const ::PrepareBattleRequest* request,
    ::PrepareBattleResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    // match -> scene 备战冻结(gRPC 服务端在 loop 线程上派发,可直接同步访问 ECS;
    // 失败原因经 response.error_message 返回,gRPC status 恒为 OK)
    PlayerBattleSystem::PrepareBattle(*request, *response);
///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleCancelBattlePrepare(const ::CancelBattlePrepareRequest* request)
{
///<<< BEGIN WRITING YOUR CODE
    // gather 失败补偿解冻:battle_id 匹配才摘 InBattleComp + DEL battle:lock,幂等
    PlayerBattleSystem::CancelBattlePrepare(*request);
///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleSealAccountRoleTrade(const ::SealAccountRoleTradeRequest* request)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleAssetDebit(const ::AssetOpRequest* request,
    ::AssetOpResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    // 通用资产通道(guild-phase2/04-asset-channel.md §S4):Go 服务扣玩家资产。
    // gRPC 服务端在 loop 线程上派发,系统层可直接同步访问 ECS;
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

void SceneNodeGrpcImpl::HandleAssetAckReceipt(const ::AssetOpRequest* request,
    ::AssetOpResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    PlayerAssetOpSystem::AckReceipt(*request, *response);
///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::HandleGmExecutePlayerCommand(const ::scene_node::GmExecutePlayerCommandRequest* request,
    ::scene_node::GmExecutePlayerCommandResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    // 全部同步运行在同一个 Scene loop：查归属、会话、去重占位和域写入之间没有异步间隙。
    static gm_player_command::CommandCache commands;
    response->set_command_id(request->command_id());
    const auto admission = commands.Begin(*request, *response);
    if (admission == gm_player_command::CommandCache::Admission::kReplay) return;
    if (admission != gm_player_command::CommandCache::Admission::kNew) {
        response->mutable_error_message()->set_id(kFeatureUnavailable);
        LOG_WARN << "[GmPlayerCommand] rejected command_id=" << request->command_id()
                 << " reason=command_conflict_or_capacity";
        return;
    }
    const auto finish = [&] (uint32_t error) {
        if (error != kSuccess) response->mutable_error_message()->set_id(error);
        response->set_executed(error == kSuccess);
        commands.Complete(*request, *response);
        LOG_INFO << "[GmPlayerCommand] completed command_id=" << request->command_id()
                 << " operator=" << gm_player_command::AuditText(request->operator_())
                 << " player_id=" << request->player_id() << " command=" << request->command_case()
                 << " reason=" << gm_player_command::AuditText(request->reason()) << " error=" << error;
    };
    if (scene_gm_guard::RejectGmClientRpc("GmExecutePlayerCommand")) {
        finish(kFeatureUnavailable);
        return;
    }
    const auto player = tlsEcs.GetPlayer(request->player_id());
    auto& registry = tlsEcs.actorRegistry;
    if (!registry.valid(player) || PlayerLifecycleSystem::GetPendingEnterMap().contains(request->player_id()) ||
        !registry.all_of<Player, SceneEntityComp>(player) ||
        !tlsEcs.sceneRegistry.valid(registry.get<SceneEntityComp>(player).sceneEntity) ||
        registry.any_of<PlayerConnectionLostComp>(player) ||
        !player_mutability::IsPlayerMutable(player) || PlayerBattleSystem::IsInBattle(player) ||
        !RoleAdmissionSystem::ConfirmedIdentity(request->player_id())) {
        finish(kFeatureUnavailable);
        return;
    }
    const auto* snapshot = registry.try_get<PlayerSessionSnapshotComp>(player);
    const auto* identity = registry.try_get<PlayerLoginIdentityComp>(player);
    const auto session = SessionMap().find(request->expected_session_id());
    if (snapshot == nullptr || identity == nullptr ||
        snapshot->gate_session_id() != request->expected_session_id() ||
        snapshot->player_id() != request->player_id() || identity->sessionId != request->expected_session_id() ||
        identity->loginGeneration != request->expected_login_generation() ||
        session == SessionMap().end() || session->second != request->player_id()) {
        finish(kFeatureUnavailable);
        return;
    }
    // 审计先于副作用；只写可信操作者与已签名原因，不记录 auth 或密钥。
    LOG_INFO << "[GmPlayerCommand] executing command_id=" << request->command_id()
             << " operator=" << gm_player_command::AuditText(request->operator_())
             << " player_id=" << request->player_id() << " command=" << request->command_case()
             << " reason=" << gm_player_command::AuditText(request->reason());
    uint32_t error = kInvalidParameter;
    using Request = scene_node::GmExecutePlayerCommandRequest;
    switch (request->command_case()) {
    case Request::kAddCurrency: {
        const auto& args = request->add_currency();
        const auto type = static_cast<CurrencyType>(args.currency_type());
        error = CurrencySystem::AddCurrency(player, type, args.amount(), TX_GM_GRANT);
        if (error == kSuccess) response->set_balance_after(CurrencySystem::GetBalance(player, type));
        break;
    }
    case Request::kDeductCurrency: {
        const auto& args = request->deduct_currency();
        const auto type = static_cast<CurrencyType>(args.currency_type());
        error = CurrencySystem::DeductCurrency(player, type, args.amount(), TX_GM_DEDUCT);
        if (error == kSuccess) response->set_balance_after(CurrencySystem::GetBalance(player, type));
        break;
    }
    case Request::kBlockCurrency:
        error = CurrencySystem::BlockCurrency(player, static_cast<CurrencyType>(request->block_currency().currency_type()));
        break;
    case Request::kUnblockCurrency:
        error = CurrencySystem::UnblockCurrency(player, static_cast<CurrencyType>(request->unblock_currency().currency_type()));
        break;
    case Request::kGrantItem:
        error = PlayerEquipSystem::GmGrantItem(player, request->grant_item().config_id(), request->grant_item().count());
        break;
    case Request::kGrantPet: {
        uint64_t petId = 0;
        error = PetSystem::GrantPet(player, request->grant_pet().pet_table_id(), petId);
        if (error == kSuccess) {
            response->set_pet_id(petId);
            // 网页调用没有客户端 RPC 的 pets 响应，复用既有通知刷新在线客户端。
            PetSystem::PushList(player);
        }
        break;
    }
    case Request::kSetLevel:
        error = PlayerAttributeSystem::GmSetLevel(player, request->set_level().level());
        break;
    default: break;
    }
    finish(error);
///<<< END WRITING YOUR CODE
}

void SceneNodeGrpcImpl::CreateScene(const std::shared_ptr<CreateSceneCall>& call)
{
    [[maybe_unused]] const ::CreateSceneRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.CreateScene.guard
    // 停机已开始(Node 在 beforeShutdown 之后进入 gRPC 排空):不再建新场景 —— 建出的场景会随进程退出丢失,
    // SceneManager 却以为它在。回 UNAVAILABLE 让 SceneManager 回滚(它对任何错误都回滚,不匹配文案)。
    if (call->ServerDraining())
    {
        LOG_WARN << "[gRPC] CreateScene rejected: scene node is shutting down, scene_id=" << request->scene_id();
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kSceneNodeShuttingDownMessage));
        return;
    }
    // zone 不是本节点的就拒建(SceneLifecycle::AdmitsCreateZone),判在等 Agones 许可之前:别的 zone 的建场景不能让本进程
    // 被 Allocate。这是调用方的错,不是暂时故障:回 FAILED_PRECONDITION,scene_manager 对任何错误都回滚。
    if (!SceneLifecycle::AdmitsCreateZone(request->zone_id(), GetZoneId()))
    {
        LOG_ERROR << "[gRPC] CreateScene rejected: scene zone does not match this node, scene_id=" << request->scene_id()
                  << " request_zone_id=" << request->zone_id() << " node_zone_id=" << GetZoneId()
                  << " (metric=scene_create_zone_mismatch)";
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kFailedPrecondition, kSceneZoneMismatchMessage));
        return;
    }
    // Agones 高密度模式:进程必须先被 Allocate,才能创建第一个场景。
    // POST /allocate 在 lifecycle worker 线程,结果经 queueInLoop 回 loop,不阻塞逻辑帧;等待上限
    // LifecycleOptions::allocateWaitTimeout。失败一律 fail-closed:不建实体,回非 OK,让 SceneManager 回滚重试,
    // 许可移入协程的自有 RAII 对象，活到 Proceed() 返回(Handle* 同步建完场景)，
    // 在途创建才能阻止提前回到 Ready。任务由服务对象持有，传输回调不捕获 this(AGENTS §11.7)。
    //
    // 2026-09-15 恢复:原块在 commit 6c4021ae5 被 proto 重生成吞掉(当时位于守护段外)。
    if (!creationTasks_.Start(CreateSceneWithPermit(loop_, call), "SceneCreationPermit"))
    {
        LOG_WARN << "[gRPC] CreateScene rejected: creation admission closed (node draining), scene_id=" << request->scene_id();
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kSceneNodeShuttingDownMessage));
    }
    return;
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::DestroyScene(const std::shared_ptr<DestroySceneCall>& call)
{
    [[maybe_unused]] const ::DestroySceneRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.DestroyScene.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::ReleasePlayer(const std::shared_ptr<ReleasePlayerCall>& call)
{
    [[maybe_unused]] const ::scene_node::ReleasePlayerRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.ReleasePlayer.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::PrepareBattle(const std::shared_ptr<PrepareBattleCall>& call)
{
    [[maybe_unused]] const ::PrepareBattleRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.PrepareBattle.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::CancelBattlePrepare(const std::shared_ptr<CancelBattlePrepareCall>& call)
{
    [[maybe_unused]] const ::CancelBattlePrepareRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.CancelBattlePrepare.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::SealAccountRoleTrade(const std::shared_ptr<SealAccountRoleTradeCall>& call)
{
    [[maybe_unused]] const ::SealAccountRoleTradeRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.SealAccountRoleTrade.guard
    if (!RoleAdmissionSystem::AcceptSeal(*request)) {
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kPermissionDenied, "角色封存原件或内部证明无效"));
        return;
    }
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::AssetDebit(const std::shared_ptr<AssetDebitCall>& call)
{
    [[maybe_unused]] const ::AssetOpRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.AssetDebit.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::AssetAbortDebit(const std::shared_ptr<AssetAbortDebitCall>& call)
{
    [[maybe_unused]] const ::AssetOpRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.AssetAbortDebit.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::AssetCredit(const std::shared_ptr<AssetCreditCall>& call)
{
    [[maybe_unused]] const ::AssetOpRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.AssetCredit.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::AssetAckReceipt(const std::shared_ptr<AssetAckReceiptCall>& call)
{
    [[maybe_unused]] const ::AssetOpRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.AssetAckReceipt.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::GmExecutePlayerCommand(const std::shared_ptr<GmExecutePlayerCommandCall>& call)
{
    [[maybe_unused]] const ::scene_node::GmExecutePlayerCommandRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.GmExecutePlayerCommand.guard
    if (call->ServerDraining() || gNode->PlannedDrainStarted() ||
        !gNode->GetServiceDiscoveryManager().etcdService.IsIdentityConfirmedFresh()) {
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, "scene_node_not_ready"));
        return;
    }
    const char* secret = std::getenv(gm_player_command::kSecretEnv);
    static gate_security::GmNonceCache commandNonces;
    const auto auth = gm_player_command::Verify(*request, gNode->GetNodeId(), secret != nullptr ? secret : "",
        static_cast<int64_t>(std::time(nullptr)), gate_security::ResolveGmSkewSeconds(), commandNonces);
    if (auth.result != gate_security::GmAuthResult::kOk) {
        LOG_WARN << "[GmPlayerCommand] authentication rejected reason=" << gate_security::GmAuthResultName(auth.result);
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kPermissionDenied, "GM player command rejected"));
        return;
    }
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void SceneNodeGrpcImpl::RegisterMethods(muduo_grpc::MethodTable& table)
{
    table.AddUnary(std::string(kCreateScenePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = CreateSceneCall::Create(std::move(serverCall), &HandleCreateScene);
            if (call->ParseRequest(requestBytes))
            {
                CreateScene(call);
            }
        });
    table.AddUnary(std::string(kDestroyScenePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = DestroySceneCall::Create(std::move(serverCall),
                [](const ::DestroySceneRequest* request, ::Empty* /*response*/) { HandleDestroyScene(request); });
            if (call->ParseRequest(requestBytes))
            {
                DestroyScene(call);
            }
        });
    table.AddUnary(std::string(kReleasePlayerPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = ReleasePlayerCall::Create(std::move(serverCall),
                [](const ::scene_node::ReleasePlayerRequest* request, ::Empty* /*response*/) { HandleReleasePlayer(request); });
            if (call->ParseRequest(requestBytes))
            {
                ReleasePlayer(call);
            }
        });
    table.AddUnary(std::string(kPrepareBattlePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = PrepareBattleCall::Create(std::move(serverCall), &HandlePrepareBattle);
            if (call->ParseRequest(requestBytes))
            {
                PrepareBattle(call);
            }
        });
    table.AddUnary(std::string(kCancelBattlePreparePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = CancelBattlePrepareCall::Create(std::move(serverCall),
                [](const ::CancelBattlePrepareRequest* request, ::Empty* /*response*/) { HandleCancelBattlePrepare(request); });
            if (call->ParseRequest(requestBytes))
            {
                CancelBattlePrepare(call);
            }
        });
    table.AddUnary(std::string(kSealAccountRoleTradePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = SealAccountRoleTradeCall::Create(std::move(serverCall),
                [](const ::SealAccountRoleTradeRequest* request, ::Empty* /*response*/) { HandleSealAccountRoleTrade(request); });
            if (call->ParseRequest(requestBytes))
            {
                SealAccountRoleTrade(call);
            }
        });
    table.AddUnary(std::string(kAssetDebitPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = AssetDebitCall::Create(std::move(serverCall), &HandleAssetDebit);
            if (call->ParseRequest(requestBytes))
            {
                AssetDebit(call);
            }
        });
    table.AddUnary(std::string(kAssetAbortDebitPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = AssetAbortDebitCall::Create(std::move(serverCall), &HandleAssetAbortDebit);
            if (call->ParseRequest(requestBytes))
            {
                AssetAbortDebit(call);
            }
        });
    table.AddUnary(std::string(kAssetCreditPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = AssetCreditCall::Create(std::move(serverCall), &HandleAssetCredit);
            if (call->ParseRequest(requestBytes))
            {
                AssetCredit(call);
            }
        });
    table.AddUnary(std::string(kAssetAckReceiptPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = AssetAckReceiptCall::Create(std::move(serverCall), &HandleAssetAckReceipt);
            if (call->ParseRequest(requestBytes))
            {
                AssetAckReceipt(call);
            }
        });
    table.AddUnary(std::string(kGmExecutePlayerCommandPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = GmExecutePlayerCommandCall::Create(std::move(serverCall), &HandleGmExecutePlayerCommand);
            if (call->ParseRequest(requestBytes))
            {
                GmExecutePlayerCommand(call);
            }
        });
}
