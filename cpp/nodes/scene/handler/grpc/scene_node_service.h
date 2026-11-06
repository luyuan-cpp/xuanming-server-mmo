#pragma once

#include <memory>
#include <string_view>
#include <muduo/net/EventLoop.h>
#include "muduo_grpc/method_table.h"
#include "network/grpc_unary_call.h"
#include "proto/scene_manager/scene_node_service.pb.h"
///<<< BEGIN WRITING YOUR CODE: grpc.header.includes
#include "engine/core/async/loop_task_scope.h"
///<<< END WRITING YOUR CODE

// 由 proto 工具生成 SceneNodeGrpc 的 gRPC 服务端骨架(muduo_grpc,docs/design/grpc-muduo-transport.md §15);自定义代码只写在守护段内。
// 全部在节点 EventLoop 线程执行,不创建线程。每个 RPC:解析请求 → <M>()(守护段 grpc.<M>.guard)→ Handle<M>(同步)→ 回包。
// 守护段:同步拒绝写 `call->Reject(status); return;`;异步决定时先发起异步操作再 `return;`,在回调里调
// `call->Proceed()` 或 `call->Reject(...)`。回调只捕获 call 与 weak 引用,不得捕获 this(AGENTS §11.7)。
// 要活到业务完成的对象(如 Agones 许可)作为回调参数,活到 Proceed() 返回即可(Handle* 同步)。不得阻塞;业务错误经响应字段返回。
class SceneNodeGrpcImpl final : public muduo_grpc::Service
{
public:
    explicit SceneNodeGrpcImpl(muduo::net::EventLoop& loop);
    ///<<< BEGIN WRITING YOUR CODE: grpc.header.public
    void CloseCreationAdmission() { creationTasks_.Stop(); }
    ///<<< END WRITING YOUR CODE

    // 方法表(按 :path 精确路由)。处理函数捕获 this:本对象必须活过注册它的 Node。
    void RegisterMethods(muduo_grpc::MethodTable& table) override;

    static constexpr std::string_view kCreateScenePath = "/scene_node.SceneNodeGrpc/CreateScene";
    static constexpr std::string_view kDestroyScenePath = "/scene_node.SceneNodeGrpc/DestroyScene";
    static constexpr std::string_view kReleasePlayerPath = "/scene_node.SceneNodeGrpc/ReleasePlayer";
    static constexpr std::string_view kPrepareBattlePath = "/scene_node.SceneNodeGrpc/PrepareBattle";
    static constexpr std::string_view kCancelBattlePreparePath = "/scene_node.SceneNodeGrpc/CancelBattlePrepare";
    static constexpr std::string_view kSealAccountRoleTradePath = "/scene_node.SceneNodeGrpc/SealAccountRoleTrade";
    static constexpr std::string_view kAssetDebitPath = "/scene_node.SceneNodeGrpc/AssetDebit";
    static constexpr std::string_view kAssetAbortDebitPath = "/scene_node.SceneNodeGrpc/AssetAbortDebit";
    static constexpr std::string_view kAssetCreditPath = "/scene_node.SceneNodeGrpc/AssetCredit";
    static constexpr std::string_view kAssetAckReceiptPath = "/scene_node.SceneNodeGrpc/AssetAckReceipt";

private:
    using CreateSceneCall = GrpcUnaryCall<::CreateSceneRequest, ::CreateSceneResponse>;
    using DestroySceneCall = GrpcUnaryCall<::DestroySceneRequest, ::Empty>;
    using ReleasePlayerCall = GrpcUnaryCall<::scene_node::ReleasePlayerRequest, ::Empty>;
    using PrepareBattleCall = GrpcUnaryCall<::PrepareBattleRequest, ::PrepareBattleResponse>;
    using CancelBattlePrepareCall = GrpcUnaryCall<::CancelBattlePrepareRequest, ::Empty>;
    using SealAccountRoleTradeCall = GrpcUnaryCall<::SealAccountRoleTradeRequest, ::Empty>;
    using AssetDebitCall = GrpcUnaryCall<::AssetOpRequest, ::AssetOpResponse>;
    using AssetAbortDebitCall = GrpcUnaryCall<::AssetOpRequest, ::AssetOpResponse>;
    using AssetCreditCall = GrpcUnaryCall<::AssetOpRequest, ::AssetOpResponse>;
    using AssetAckReceiptCall = GrpcUnaryCall<::AssetOpRequest, ::AssetOpResponse>;
    void CreateScene(const std::shared_ptr<CreateSceneCall>& call);
    void DestroyScene(const std::shared_ptr<DestroySceneCall>& call);
    void ReleasePlayer(const std::shared_ptr<ReleasePlayerCall>& call);
    void PrepareBattle(const std::shared_ptr<PrepareBattleCall>& call);
    void CancelBattlePrepare(const std::shared_ptr<CancelBattlePrepareCall>& call);
    void SealAccountRoleTrade(const std::shared_ptr<SealAccountRoleTradeCall>& call);
    void AssetDebit(const std::shared_ptr<AssetDebitCall>& call);
    void AssetAbortDebit(const std::shared_ptr<AssetAbortDebitCall>& call);
    void AssetCredit(const std::shared_ptr<AssetCreditCall>& call);
    void AssetAckReceipt(const std::shared_ptr<AssetAckReceiptCall>& call);
    static void HandleCreateScene(const ::CreateSceneRequest* request, ::CreateSceneResponse* response);
    static void HandleDestroyScene(const ::DestroySceneRequest* request);
    static void HandleReleasePlayer(const ::scene_node::ReleasePlayerRequest* request);
    static void HandlePrepareBattle(const ::PrepareBattleRequest* request, ::PrepareBattleResponse* response);
    static void HandleCancelBattlePrepare(const ::CancelBattlePrepareRequest* request);
    static void HandleSealAccountRoleTrade(const ::SealAccountRoleTradeRequest* request);
    static void HandleAssetDebit(const ::AssetOpRequest* request, ::AssetOpResponse* response);
    static void HandleAssetAbortDebit(const ::AssetOpRequest* request, ::AssetOpResponse* response);
    static void HandleAssetCredit(const ::AssetOpRequest* request, ::AssetOpResponse* response);
    static void HandleAssetAckReceipt(const ::AssetOpRequest* request, ::AssetOpResponse* response);
    muduo::net::EventLoop& loop_;
    ///<<< BEGIN WRITING YOUR CODE: grpc.header.private
    LoopTaskScope creationTasks_{loop_};
    ///<<< END WRITING YOUR CODE
};
