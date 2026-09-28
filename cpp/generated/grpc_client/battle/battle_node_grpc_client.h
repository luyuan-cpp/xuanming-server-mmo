#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/battle/battle_node.grpc.pb.h"

#include "rpc/service_metadata/battle_node_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

using BattleNodeStubPtr = std::unique_ptr<BattleNode::Stub>;
#pragma region BattleNodeCreateBattle

struct AsyncBattleNodeCreateBattleGrpcClient {
    uint32_t messageId{ BattleNodeCreateBattleMessageId };
    ClientContext context;
    Status status;
    ::CreateBattleResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::CreateBattleResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::CreateBattleRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleNodeCreateBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::CreateBattleResponse&)>;
extern AsyncBattleNodeCreateBattleHandlerFunctionType AsyncBattleNodeCreateBattleHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleNodeCreateBattleFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::CreateBattleRequest&)>;
extern AsyncBattleNodeCreateBattleFailedHandlerFunctionType AsyncBattleNodeCreateBattleFailedHandler;

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request);
void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleNodeDestroyBattle

struct AsyncBattleNodeDestroyBattleGrpcClient {
    uint32_t messageId{ BattleNodeDestroyBattleMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::DestroyBattleRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleNodeDestroyBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleNodeDestroyBattleHandlerFunctionType AsyncBattleNodeDestroyBattleHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleNodeDestroyBattleFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::DestroyBattleRequest&)>;
extern AsyncBattleNodeDestroyBattleFailedHandlerFunctionType AsyncBattleNodeDestroyBattleFailedHandler;

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request);
void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleNodeAddObserver

struct AsyncBattleNodeAddObserverGrpcClient {
    uint32_t messageId{ BattleNodeAddObserverMessageId };
    ClientContext context;
    Status status;
    ::AddObserverResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::AddObserverResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::AddObserverRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleNodeAddObserverHandlerFunctionType =
    std::function<void(const ClientContext&, const ::AddObserverResponse&)>;
extern AsyncBattleNodeAddObserverHandlerFunctionType AsyncBattleNodeAddObserverHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleNodeAddObserverFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::AddObserverRequest&)>;
extern AsyncBattleNodeAddObserverFailedHandlerFunctionType AsyncBattleNodeAddObserverFailedHandler;

void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request);
void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleNodeRemoveObserver

struct AsyncBattleNodeRemoveObserverGrpcClient {
    uint32_t messageId{ BattleNodeRemoveObserverMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::RemoveObserverRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleNodeRemoveObserverHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleNodeRemoveObserverHandlerFunctionType AsyncBattleNodeRemoveObserverHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleNodeRemoveObserverFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::RemoveObserverRequest&)>;
extern AsyncBattleNodeRemoveObserverFailedHandlerFunctionType AsyncBattleNodeRemoveObserverFailedHandler;

void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request);
void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleNodeIssueBattleTicket

struct AsyncBattleNodeIssueBattleTicketGrpcClient {
    uint32_t messageId{ BattleNodeIssueBattleTicketMessageId };
    ClientContext context;
    Status status;
    ::IssueBattleTicketResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::IssueBattleTicketResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::IssueBattleTicketRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleNodeIssueBattleTicketHandlerFunctionType =
    std::function<void(const ClientContext&, const ::IssueBattleTicketResponse&)>;
extern AsyncBattleNodeIssueBattleTicketHandlerFunctionType AsyncBattleNodeIssueBattleTicketHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleNodeIssueBattleTicketFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::IssueBattleTicketRequest&)>;
extern AsyncBattleNodeIssueBattleTicketFailedHandlerFunctionType AsyncBattleNodeIssueBattleTicketFailedHandler;

void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request);
void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetBattleNodeHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetBattleNodeIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetBattleNodeFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetBattleNodeIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetBattleNodeCallDeadline(std::chrono::milliseconds deadline);
void HandleBattleNodeCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitBattleNodeGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
