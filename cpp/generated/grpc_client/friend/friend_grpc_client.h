#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/friend/friend.grpc.pb.h"

#include "rpc/service_metadata/friend_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace friendpb {
using ClientPlayerFriendStubPtr = std::unique_ptr<ClientPlayerFriend::Stub>;
#pragma region ClientPlayerFriendAddFriend

struct AsyncClientPlayerFriendAddFriendGrpcClient {
    uint32_t messageId{ ClientPlayerFriendAddFriendMessageId };
    ClientContext context;
    Status status;
    ::friendpb::AddFriendResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::AddFriendResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::AddFriendRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendAddFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AddFriendResponse&)>;
extern AsyncClientPlayerFriendAddFriendHandlerFunctionType AsyncClientPlayerFriendAddFriendHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendAddFriendFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::AddFriendRequest&)>;
extern AsyncClientPlayerFriendAddFriendFailedHandlerFunctionType AsyncClientPlayerFriendAddFriendFailedHandler;

void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request);
void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendAcceptFriend

struct AsyncClientPlayerFriendAcceptFriendGrpcClient {
    uint32_t messageId{ ClientPlayerFriendAcceptFriendMessageId };
    ClientContext context;
    Status status;
    ::friendpb::AcceptFriendResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::AcceptFriendResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::AcceptFriendRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendAcceptFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AcceptFriendResponse&)>;
extern AsyncClientPlayerFriendAcceptFriendHandlerFunctionType AsyncClientPlayerFriendAcceptFriendHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendAcceptFriendFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::AcceptFriendRequest&)>;
extern AsyncClientPlayerFriendAcceptFriendFailedHandlerFunctionType AsyncClientPlayerFriendAcceptFriendFailedHandler;

void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request);
void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendRejectFriend

struct AsyncClientPlayerFriendRejectFriendGrpcClient {
    uint32_t messageId{ ClientPlayerFriendRejectFriendMessageId };
    ClientContext context;
    Status status;
    ::friendpb::RejectFriendResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::RejectFriendResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::RejectFriendRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendRejectFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RejectFriendResponse&)>;
extern AsyncClientPlayerFriendRejectFriendHandlerFunctionType AsyncClientPlayerFriendRejectFriendHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendRejectFriendFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::RejectFriendRequest&)>;
extern AsyncClientPlayerFriendRejectFriendFailedHandlerFunctionType AsyncClientPlayerFriendRejectFriendFailedHandler;

void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request);
void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendRemoveFriend

struct AsyncClientPlayerFriendRemoveFriendGrpcClient {
    uint32_t messageId{ ClientPlayerFriendRemoveFriendMessageId };
    ClientContext context;
    Status status;
    ::friendpb::RemoveFriendResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::RemoveFriendResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::RemoveFriendRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendRemoveFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RemoveFriendResponse&)>;
extern AsyncClientPlayerFriendRemoveFriendHandlerFunctionType AsyncClientPlayerFriendRemoveFriendHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendRemoveFriendFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::RemoveFriendRequest&)>;
extern AsyncClientPlayerFriendRemoveFriendFailedHandlerFunctionType AsyncClientPlayerFriendRemoveFriendFailedHandler;

void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request);
void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendGetFriendList

struct AsyncClientPlayerFriendGetFriendListGrpcClient {
    uint32_t messageId{ ClientPlayerFriendGetFriendListMessageId };
    ClientContext context;
    Status status;
    ::friendpb::GetFriendListResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::GetFriendListResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::GetFriendListRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendGetFriendListHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetFriendListResponse&)>;
extern AsyncClientPlayerFriendGetFriendListHandlerFunctionType AsyncClientPlayerFriendGetFriendListHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendGetFriendListFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::GetFriendListRequest&)>;
extern AsyncClientPlayerFriendGetFriendListFailedHandlerFunctionType AsyncClientPlayerFriendGetFriendListFailedHandler;

void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request);
void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendGetPendingRequests

struct AsyncClientPlayerFriendGetPendingRequestsGrpcClient {
    uint32_t messageId{ ClientPlayerFriendGetPendingRequestsMessageId };
    ClientContext context;
    Status status;
    ::friendpb::GetPendingRequestsResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::GetPendingRequestsResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::GetPendingRequestsRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendGetPendingRequestsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetPendingRequestsResponse&)>;
extern AsyncClientPlayerFriendGetPendingRequestsHandlerFunctionType AsyncClientPlayerFriendGetPendingRequestsHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendGetPendingRequestsFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::GetPendingRequestsRequest&)>;
extern AsyncClientPlayerFriendGetPendingRequestsFailedHandlerFunctionType AsyncClientPlayerFriendGetPendingRequestsFailedHandler;

void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request);
void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendBlock

struct AsyncClientPlayerFriendBlockGrpcClient {
    uint32_t messageId{ ClientPlayerFriendBlockMessageId };
    ClientContext context;
    Status status;
    ::friendpb::BlockResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::BlockResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::BlockRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendBlockHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::BlockResponse&)>;
extern AsyncClientPlayerFriendBlockHandlerFunctionType AsyncClientPlayerFriendBlockHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendBlockFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::BlockRequest&)>;
extern AsyncClientPlayerFriendBlockFailedHandlerFunctionType AsyncClientPlayerFriendBlockFailedHandler;

void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request);
void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendUnblock

struct AsyncClientPlayerFriendUnblockGrpcClient {
    uint32_t messageId{ ClientPlayerFriendUnblockMessageId };
    ClientContext context;
    Status status;
    ::friendpb::UnblockResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::UnblockResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::UnblockRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendUnblockHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::UnblockResponse&)>;
extern AsyncClientPlayerFriendUnblockHandlerFunctionType AsyncClientPlayerFriendUnblockHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendUnblockFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::UnblockRequest&)>;
extern AsyncClientPlayerFriendUnblockFailedHandlerFunctionType AsyncClientPlayerFriendUnblockFailedHandler;

void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request);
void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendListBlocks

struct AsyncClientPlayerFriendListBlocksGrpcClient {
    uint32_t messageId{ ClientPlayerFriendListBlocksMessageId };
    ClientContext context;
    Status status;
    ::friendpb::ListBlocksResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::ListBlocksResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::ListBlocksRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendListBlocksHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::ListBlocksResponse&)>;
extern AsyncClientPlayerFriendListBlocksHandlerFunctionType AsyncClientPlayerFriendListBlocksHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendListBlocksFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::ListBlocksRequest&)>;
extern AsyncClientPlayerFriendListBlocksFailedHandlerFunctionType AsyncClientPlayerFriendListBlocksFailedHandler;

void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request);
void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendRecommendFriends

struct AsyncClientPlayerFriendRecommendFriendsGrpcClient {
    uint32_t messageId{ ClientPlayerFriendRecommendFriendsMessageId };
    ClientContext context;
    Status status;
    ::friendpb::RecommendFriendsResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::friendpb::RecommendFriendsResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::RecommendFriendsRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendRecommendFriendsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RecommendFriendsResponse&)>;
extern AsyncClientPlayerFriendRecommendFriendsHandlerFunctionType AsyncClientPlayerFriendRecommendFriendsHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendRecommendFriendsFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::RecommendFriendsRequest&)>;
extern AsyncClientPlayerFriendRecommendFriendsFailedHandlerFunctionType AsyncClientPlayerFriendRecommendFriendsFailedHandler;

void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request);
void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerFriendNotifyFriendEvent

struct AsyncClientPlayerFriendNotifyFriendEventGrpcClient {
    uint32_t messageId{ ClientPlayerFriendNotifyFriendEventMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::friendpb::FriendEventS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerFriendNotifyFriendEventHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncClientPlayerFriendNotifyFriendEventHandlerFunctionType AsyncClientPlayerFriendNotifyFriendEventHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerFriendNotifyFriendEventFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::friendpb::FriendEventS2C&)>;
extern AsyncClientPlayerFriendNotifyFriendEventFailedHandlerFunctionType AsyncClientPlayerFriendNotifyFriendEventFailedHandler;

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request);
void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetFriendHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetFriendIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetFriendFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetFriendIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetFriendCallDeadline(std::chrono::milliseconds deadline);
void HandleFriendCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitFriendGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace friendpb
