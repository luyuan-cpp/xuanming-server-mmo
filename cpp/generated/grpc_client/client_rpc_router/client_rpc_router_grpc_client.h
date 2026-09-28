#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/client_rpc_router/client_rpc_router.grpc.pb.h"

#include "rpc/service_metadata/client_rpc_router_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace client_rpc_router {
using ClientRpcRouterStubPtr = std::unique_ptr<ClientRpcRouter::Stub>;
#pragma region ClientRpcRouterForward

struct AsyncClientRpcRouterForwardGrpcClient {
    uint32_t messageId{ ClientRpcRouterForwardMessageId };
    ClientContext context;
    Status status;
    ::MessageContent reply;
    std::unique_ptr<ClientAsyncResponseReader<::MessageContent>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::client_rpc_router::ForwardRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientRpcRouterForwardHandlerFunctionType =
    std::function<void(const ClientContext&, const ::MessageContent&)>;
extern AsyncClientRpcRouterForwardHandlerFunctionType AsyncClientRpcRouterForwardHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientRpcRouterForwardFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::client_rpc_router::ForwardRequest&)>;
extern AsyncClientRpcRouterForwardFailedHandlerFunctionType AsyncClientRpcRouterForwardFailedHandler;

void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const ::client_rpc_router::ForwardRequest& request);
void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const ::client_rpc_router::ForwardRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetClientRpcRouterHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetClientRpcRouterIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetClientRpcRouterFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetClientRpcRouterIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetClientRpcRouterCallDeadline(std::chrono::milliseconds deadline);
void HandleClientRpcRouterCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitClientRpcRouterGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace client_rpc_router
