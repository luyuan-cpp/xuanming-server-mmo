#include "battle_node.h"
///<<< BEGIN WRITING YOUR CODE
#include "infra/agones/agones_gameserver_lifecycle.h"
#include "logic/battle_room_manager.h"
#include "muduo/base/Logging.h"

namespace
{

// CreateBattle 节点级准入拒绝(建房准入闸未开 / 已关,或拿不到 Agones 分配许可)时的 gRPC status message。
// 跨语言字符串契约(集群外入口 D82):match 按 codes.Unavailable + 此消息精确匹配,
// 跳过 DestroyBattle、换一个没试过的节点重试一次(go/match/internal/logic/gather.go
// 的 battleNotAllocatableMessage)。两边必须同改。
constexpr char kBattleNotAllocatableMessage[] = "battle_not_allocatable";

muduo_grpc::Status NotAllocatable()
{
    return muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kBattleNotAllocatableMessage);
}

// 第 3 道:许可回到 loop 后复核建房准入闸(grpc.CreateBattle.guard)。许可回调只弱持有闸、不捕获 this;
// 闸已随 BattleNodeImpl 销毁(lock 失败)或不在 kOpen,都按「不接新房间」处理(fail-closed)。
bool AdmissionOpen(const std::weak_ptr<battle_admission::AdmissionGate>& gate)
{
    const auto admission = gate.lock();
    return admission != nullptr && admission->Phase() == battle_admission::AdmissionPhase::kOpen;
}

} // namespace

bool BattleNodeImpl::OpenAdmission()
{
    const bool opened = admission_->Open();
    LOG_INFO << "battle room admission gate: " << (opened ? "opened" : "not opened(shutdown started or repeated call)")
             << " phase=" << battle_admission::ToString(admission_->Phase());
    return opened;
}

void BattleNodeImpl::CloseAdmission()
{
    admission_->Close();
    LOG_INFO << "battle room admission gate closed: subsequent CreateBattle calls return UNAVAILABLE " << kBattleNotAllocatableMessage;
}
///<<< END WRITING YOUR CODE

BattleNodeImpl::BattleNodeImpl(muduo::net::EventLoop& loop)
    : loop_(loop)
{
}

void BattleNodeImpl::HandleQueryObserver(const ::QueryObserverRequest* request,
    ::QueryObserverResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    BattleRoomManager::Instance().HandleQueryObserver(*request, *response);
///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::HandleQueryBattleCreation(const ::QueryBattleCreationRequest* request,
    ::QueryBattleCreationResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    BattleRoomManager::Instance().HandleQueryBattleCreation(*request,*response);
///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::HandleCreateBattle(const ::CreateBattleRequest* request,
    ::CreateBattleResponse* response)
{
    ///<<< BEGIN WRITING YOUR CODE
    // 业务逻辑全部在手写类 BattleRoomManager(cpp/nodes/battle/logic/),此处只做委托
    BattleRoomManager::Instance().HandleCreateBattle(*request, *response);
    ///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::HandleDestroyBattle(const ::DestroyBattleRequest* request)
{
    ///<<< BEGIN WRITING YOUR CODE
    // 幂等销毁,不结算(补偿/回滚路径),委托手写房间管理器
    BattleRoomManager::Instance().HandleDestroyBattle(*request);
    ///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::HandleAddObserver(const ::AddObserverRequest* request,
    ::AddObserverResponse* response)
{
    ///<<< BEGIN WRITING YOUR CODE
    // 观战接入(设计文档 §10.5),错误经 response tip 返回,status 恒 OK
    BattleRoomManager::Instance().HandleAddObserver(*request, *response);
    ///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::HandleRemoveObserver(const ::RemoveObserverRequest* request,
    ::RemoveObserverResponse* response)
{
    ///<<< BEGIN WRITING YOUR CODE
    // 幂等清退(match 互斥路径),委托手写房间管理器
    BattleRoomManager::Instance().HandleRemoveObserver(*request, *response);
    ///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::HandleIssueBattleTicket(const ::IssueBattleTicketRequest* request,
    ::IssueBattleTicketResponse* response)
{
    ///<<< BEGIN WRITING YOUR CODE
    // 丢票补签(设计文档 §18 D25):名单核对 + 自签在手写房间管理器,错误经 response tip 返回,status 恒 OK
    BattleRoomManager::Instance().HandleIssueBattleTicket(*request, *response);
    ///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::HandleAttachRelayGate(const ::AttachRelayGateRequest* request,
    ::AttachRelayGateResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    BattleRoomManager::Instance().HandleAttachRelayGate(*request, *response);
///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::HandlePromoteSpectateBroadcast(const ::PromoteSpectateBroadcastRequest* request,
    ::PromoteSpectateBroadcastResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    BattleRoomManager::Instance().HandlePromoteSpectateBroadcast(*request, *response);
///<<< END WRITING YOUR CODE
}

void BattleNodeImpl::QueryObserver(const std::shared_ptr<QueryObserverCall>& call)
{
    [[maybe_unused]] const ::QueryObserverRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.QueryObserver.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::QueryBattleCreation(const std::shared_ptr<QueryBattleCreationCall>& call)
{
    [[maybe_unused]] const ::QueryBattleCreationRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.QueryBattleCreation.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::CreateBattle(const std::shared_ptr<CreateBattleCall>& call)
{
    [[maybe_unused]] const ::CreateBattleRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.CreateBattle.guard
    // 节点级准入分三道,全部在任何副作用之前;任一道不过即回 UNAVAILABLE battle_not_allocatable,
    // match 据此跳过 DestroyBattle、换节点重试一次。
    // 顺序:准入闸 → 分配许可(异步:HTTP 在 lifecycle worker,结果经 queueInLoop 回 loop)→ 许可回到 loop 后复核准入闸
    // → 预签票据(HandleCreateBattle 内 BuildAssignment)→ 插表(EmplaceRoom)。全部在 loop 线程。
    // 1) 建房准入闸(battle_admission_gate.h):启动完成之前与停机开始之后都不接新房间。闸没开就连许可都不取。
    if (const auto phase = admission_->Phase(); phase != battle_admission::AdmissionPhase::kOpen)
    {
        LOG_WARN << "CreateBattle rejected: this node is not accepting new rooms, battle_id=" << request->battle_id()
                 << " admission=" << battle_admission::ToString(phase)
                 << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
        call->Reject(NotAllocatable());
        return;
    }
    // 2) 分配许可(集群外入口 D82)。拿不到(排空标签在、allocate 未确认、lifecycle 已停 / 仍在 Starting、等待超时)即拒绝:
    //    此刻还没进 Handle*,房间、定时器、推送、Kafka 事件一概没有发生。非 Agones 环境许可恒放行(启停窗口由第 1、3 道兜住)。
    //    回调只捕获 call 与 weak 闸,不捕获 this(AGENTS §11.7)。
    agones::GameServerLifecycle::Instance().AcquireAllocationPermitAsync(
        loop_,
        [call, gate = std::weak_ptr<battle_admission::AdmissionGate>(admission_)](
            agones::GameServerLifecycle::AllocationPermit permit)
        {
            auto &lifecycle = agones::GameServerLifecycle::Instance();
            if (!permit)
            {
                LOG_WARN << "CreateBattle rejected: this instance is not allocatable, battle_id=" << call->request().battle_id()
                         << " agones_state=" << agones::ToString(lifecycle.State())
                         << " draining=" << (lifecycle.IsDraining() ? 1 : 0)
                         << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
                call->Reject(NotAllocatable());
                return;
            }
            // 3) 许可回到 loop 后复核准入闸:等许可期间 SetBeforeShutdown 可能已关闸并作废全部房间(关闸与 AbortAllRooms
            //    在同一个 loop 任务里,排在后面的本回调必然看到 kClosed);放行的话房间会在进程退出时无声丢失。
            if (!AdmissionOpen(gate))
            {
                LOG_WARN << "CreateBattle rejected: shutdown started during dispatch(rechecked in loop), battle_id="
                         << call->request().battle_id() << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
                call->Reject(NotAllocatable());
                return;   // permit 随回调返回析构:零房间时自动收口回 Ready
            }
            // permit 活到 Proceed() 返回:在途创建计入 lifecycle,防止"最后一个旧房间刚移除"时 worker 抢先回 Ready;
            // 调用已失效 / 幂等命中而零房间时,析构自动收口回 Ready。
            call->Proceed();
        });
    return;
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::DestroyBattle(const std::shared_ptr<DestroyBattleCall>& call)
{
    [[maybe_unused]] const ::DestroyBattleRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.DestroyBattle.guard
    if(!BattleRoomManager::Instance().AcceptsPlacementOperation(request->battle_id(),request->placement_operation_id(),request->target_instance_id()))
    {
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kFailedPrecondition,"placement_operation_mismatch"));
        return;
    }
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::AddObserver(const std::shared_ptr<AddObserverCall>& call)
{
    [[maybe_unused]] const ::AddObserverRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.AddObserver.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::RemoveObserver(const std::shared_ptr<RemoveObserverCall>& call)
{
    [[maybe_unused]] const ::RemoveObserverRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.RemoveObserver.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::IssueBattleTicket(const std::shared_ptr<IssueBattleTicketCall>& call)
{
    [[maybe_unused]] const ::IssueBattleTicketRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.IssueBattleTicket.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::AttachRelayGate(const std::shared_ptr<AttachRelayGateCall>& call)
{
    [[maybe_unused]] const ::AttachRelayGateRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.AttachRelayGate.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::PromoteSpectateBroadcast(const std::shared_ptr<PromoteSpectateBroadcastCall>& call)
{
    [[maybe_unused]] const ::PromoteSpectateBroadcastRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.PromoteSpectateBroadcast.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::RegisterMethods(muduo_grpc::MethodTable& table)
{
    table.AddUnary(std::string(kQueryObserverPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = QueryObserverCall::Create(std::move(serverCall), &HandleQueryObserver);
            if (call->ParseRequest(requestBytes))
            {
                QueryObserver(call);
            }
        });
    table.AddUnary(std::string(kQueryBattleCreationPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = QueryBattleCreationCall::Create(std::move(serverCall), &HandleQueryBattleCreation);
            if (call->ParseRequest(requestBytes))
            {
                QueryBattleCreation(call);
            }
        });
    table.AddUnary(std::string(kCreateBattlePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = CreateBattleCall::Create(std::move(serverCall), &HandleCreateBattle);
            if (call->ParseRequest(requestBytes))
            {
                CreateBattle(call);
            }
        });
    table.AddUnary(std::string(kDestroyBattlePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = DestroyBattleCall::Create(std::move(serverCall),
                [](const ::DestroyBattleRequest* request, ::Empty* /*response*/) { HandleDestroyBattle(request); });
            if (call->ParseRequest(requestBytes))
            {
                DestroyBattle(call);
            }
        });
    table.AddUnary(std::string(kAddObserverPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = AddObserverCall::Create(std::move(serverCall), &HandleAddObserver);
            if (call->ParseRequest(requestBytes))
            {
                AddObserver(call);
            }
        });
    table.AddUnary(std::string(kRemoveObserverPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = RemoveObserverCall::Create(std::move(serverCall), &HandleRemoveObserver);
            if (call->ParseRequest(requestBytes))
            {
                RemoveObserver(call);
            }
        });
    table.AddUnary(std::string(kIssueBattleTicketPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = IssueBattleTicketCall::Create(std::move(serverCall), &HandleIssueBattleTicket);
            if (call->ParseRequest(requestBytes))
            {
                IssueBattleTicket(call);
            }
        });
    table.AddUnary(std::string(kAttachRelayGatePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = AttachRelayGateCall::Create(std::move(serverCall), &HandleAttachRelayGate);
            if (call->ParseRequest(requestBytes))
            {
                AttachRelayGate(call);
            }
        });
    table.AddUnary(std::string(kPromoteSpectateBroadcastPath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = PromoteSpectateBroadcastCall::Create(std::move(serverCall), &HandlePromoteSpectateBroadcast);
            if (call->ParseRequest(requestBytes))
            {
                PromoteSpectateBroadcast(call);
            }
        });
}
