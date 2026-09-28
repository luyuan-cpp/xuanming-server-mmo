#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/chat/chat.grpc.pb.h"

#include "rpc/service_metadata/chat_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace chatpb {
using ClientPlayerChatStubPtr = std::unique_ptr<ClientPlayerChat::Stub>;
#pragma region ClientPlayerChatSendChat

struct AsyncClientPlayerChatSendChatGrpcClient {
    uint32_t messageId{ ClientPlayerChatSendChatMessageId };
    ClientContext context;
    Status status;
    ::chatpb::SendChatResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::chatpb::SendChatResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::chatpb::SendChatRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerChatSendChatHandlerFunctionType =
    std::function<void(const ClientContext&, const ::chatpb::SendChatResponse&)>;
extern AsyncClientPlayerChatSendChatHandlerFunctionType AsyncClientPlayerChatSendChatHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerChatSendChatFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::chatpb::SendChatRequest&)>;
extern AsyncClientPlayerChatSendChatFailedHandlerFunctionType AsyncClientPlayerChatSendChatFailedHandler;

void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::SendChatRequest& request);
void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::SendChatRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerChatPullChatHistory

struct AsyncClientPlayerChatPullChatHistoryGrpcClient {
    uint32_t messageId{ ClientPlayerChatPullChatHistoryMessageId };
    ClientContext context;
    Status status;
    ::chatpb::PullChatHistoryResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::chatpb::PullChatHistoryResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::chatpb::PullChatHistoryRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerChatPullChatHistoryHandlerFunctionType =
    std::function<void(const ClientContext&, const ::chatpb::PullChatHistoryResponse&)>;
extern AsyncClientPlayerChatPullChatHistoryHandlerFunctionType AsyncClientPlayerChatPullChatHistoryHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerChatPullChatHistoryFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::chatpb::PullChatHistoryRequest&)>;
extern AsyncClientPlayerChatPullChatHistoryFailedHandlerFunctionType AsyncClientPlayerChatPullChatHistoryFailedHandler;

void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::PullChatHistoryRequest& request);
void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::PullChatHistoryRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetChatHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetChatIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetChatFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetChatIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetChatCallDeadline(std::chrono::milliseconds deadline);
void HandleChatCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitChatGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace chatpb
