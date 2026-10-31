#pragma once

#include <memory>
#include <string_view>
#include <muduo/net/EventLoop.h>
#include "muduo_grpc/method_table.h"
#include "network/grpc_unary_call.h"
#include "proto/battle/battle_node.pb.h"
///<<< BEGIN WRITING YOUR CODE: grpc.header.includes
#include "battle_admission_gate.h"
///<<< END WRITING YOUR CODE

// 由 proto 工具生成 BattleNode 的 gRPC 服务端骨架(muduo_grpc,docs/design/grpc-muduo-transport.md §15);自定义代码只写在守护段内。
// 全部在节点 EventLoop 线程执行,不创建线程。每个 RPC:解析请求 → <M>()(守护段 grpc.<M>.guard)→ Handle<M>(同步)→ 回包。
// 守护段:同步拒绝写 `call->Reject(status); return;`;异步决定时先发起异步操作再 `return;`,在回调里调
// `call->Proceed()` 或 `call->Reject(...)`。回调只捕获 call 与 weak 引用,不得捕获 this(AGENTS §11.7)。
// 要活到业务完成的对象(如 Agones 许可)作为回调参数,活到 Proceed() 返回即可(Handle* 同步)。不得阻塞;业务错误经响应字段返回。
class BattleNodeImpl final : public muduo_grpc::Service
{
public:
    explicit BattleNodeImpl(muduo::net::EventLoop& loop);
    ///<<< BEGIN WRITING YOUR CODE: grpc.header.public
    // ---- 建房准入闸(battle_admission_gate.h;只管 CreateBattle,其余 RPC 只作用于已有房间) ----
    // 两者都只在 loop 线程调用:loop 内复核与 Close 同在 loop 上,才能保证排在关闸之后的建房必被拒。

    // SetAfterStart 里 Agones lifecycle 启动之后调用。返回 false = 没有打开:停机已先开始
    // (闸已关,不会再开),或重复调用(已经是打开的)。
    bool OpenAdmission();

    // SetBeforeShutdown 第一句调用(先于 AbortAllRooms)。终态,幂等。
    void CloseAdmission();
    ///<<< END WRITING YOUR CODE

    // 方法表(按 :path 精确路由)。处理函数捕获 this:本对象必须活过注册它的 Node。
    void RegisterMethods(muduo_grpc::MethodTable& table) override;

    static constexpr std::string_view kQueryObserverPath = "/BattleNode/QueryObserver";
    static constexpr std::string_view kQueryBattleCreationPath = "/BattleNode/QueryBattleCreation";
    static constexpr std::string_view kCreateBattlePath = "/BattleNode/CreateBattle";
    static constexpr std::string_view kDestroyBattlePath = "/BattleNode/DestroyBattle";
    static constexpr std::string_view kAddObserverPath = "/BattleNode/AddObserver";
    static constexpr std::string_view kRemoveObserverPath = "/BattleNode/RemoveObserver";
    static constexpr std::string_view kIssueBattleTicketPath = "/BattleNode/IssueBattleTicket";
    static constexpr std::string_view kAttachRelayGatePath = "/BattleNode/AttachRelayGate";
    static constexpr std::string_view kPromoteSpectateBroadcastPath = "/BattleNode/PromoteSpectateBroadcast";

private:
    using QueryObserverCall = GrpcUnaryCall<::QueryObserverRequest, ::QueryObserverResponse>;
    using QueryBattleCreationCall = GrpcUnaryCall<::QueryBattleCreationRequest, ::QueryBattleCreationResponse>;
    using CreateBattleCall = GrpcUnaryCall<::CreateBattleRequest, ::CreateBattleResponse>;
    using DestroyBattleCall = GrpcUnaryCall<::DestroyBattleRequest, ::Empty>;
    using AddObserverCall = GrpcUnaryCall<::AddObserverRequest, ::AddObserverResponse>;
    using RemoveObserverCall = GrpcUnaryCall<::RemoveObserverRequest, ::RemoveObserverResponse>;
    using IssueBattleTicketCall = GrpcUnaryCall<::IssueBattleTicketRequest, ::IssueBattleTicketResponse>;
    using AttachRelayGateCall = GrpcUnaryCall<::AttachRelayGateRequest, ::AttachRelayGateResponse>;
    using PromoteSpectateBroadcastCall = GrpcUnaryCall<::PromoteSpectateBroadcastRequest, ::PromoteSpectateBroadcastResponse>;
    void QueryObserver(const std::shared_ptr<QueryObserverCall>& call);
    void QueryBattleCreation(const std::shared_ptr<QueryBattleCreationCall>& call);
    void CreateBattle(const std::shared_ptr<CreateBattleCall>& call);
    void DestroyBattle(const std::shared_ptr<DestroyBattleCall>& call);
    void AddObserver(const std::shared_ptr<AddObserverCall>& call);
    void RemoveObserver(const std::shared_ptr<RemoveObserverCall>& call);
    void IssueBattleTicket(const std::shared_ptr<IssueBattleTicketCall>& call);
    void AttachRelayGate(const std::shared_ptr<AttachRelayGateCall>& call);
    void PromoteSpectateBroadcast(const std::shared_ptr<PromoteSpectateBroadcastCall>& call);
    static void HandleQueryObserver(const ::QueryObserverRequest* request, ::QueryObserverResponse* response);
    static void HandleQueryBattleCreation(const ::QueryBattleCreationRequest* request, ::QueryBattleCreationResponse* response);
    static void HandleCreateBattle(const ::CreateBattleRequest* request, ::CreateBattleResponse* response);
    static void HandleDestroyBattle(const ::DestroyBattleRequest* request);
    static void HandleAddObserver(const ::AddObserverRequest* request, ::AddObserverResponse* response);
    static void HandleRemoveObserver(const ::RemoveObserverRequest* request, ::RemoveObserverResponse* response);
    static void HandleIssueBattleTicket(const ::IssueBattleTicketRequest* request, ::IssueBattleTicketResponse* response);
    static void HandleAttachRelayGate(const ::AttachRelayGateRequest* request, ::AttachRelayGateResponse* response);
    static void HandlePromoteSpectateBroadcast(const ::PromoteSpectateBroadcastRequest* request, ::PromoteSpectateBroadcastResponse* response);
    muduo::net::EventLoop& loop_;
    ///<<< BEGIN WRITING YOUR CODE: grpc.header.private
    // 建房准入闸。用 shared_ptr 持有:grpc.CreateBattle.guard 的许可回调只弱持有它、不捕获 this(AGENTS §11.7),
    // 回调晚于本对象执行时 lock 失败即按「不接新房间」拒绝。
    std::shared_ptr<battle_admission::AdmissionGate> admission_ = std::make_shared<battle_admission::AdmissionGate>();
    ///<<< END WRITING YOUR CODE
};
