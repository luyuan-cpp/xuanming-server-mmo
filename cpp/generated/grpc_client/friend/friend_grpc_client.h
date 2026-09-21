#pragma once
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
};

class ::friendpb::AddFriendRequest;
using AsyncClientPlayerFriendAddFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AddFriendResponse&)>;
extern AsyncClientPlayerFriendAddFriendHandlerFunctionType AsyncClientPlayerFriendAddFriendHandler;

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
};

class ::friendpb::AcceptFriendRequest;
using AsyncClientPlayerFriendAcceptFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AcceptFriendResponse&)>;
extern AsyncClientPlayerFriendAcceptFriendHandlerFunctionType AsyncClientPlayerFriendAcceptFriendHandler;

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
};

class ::friendpb::RejectFriendRequest;
using AsyncClientPlayerFriendRejectFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RejectFriendResponse&)>;
extern AsyncClientPlayerFriendRejectFriendHandlerFunctionType AsyncClientPlayerFriendRejectFriendHandler;

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
};

class ::friendpb::RemoveFriendRequest;
using AsyncClientPlayerFriendRemoveFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RemoveFriendResponse&)>;
extern AsyncClientPlayerFriendRemoveFriendHandlerFunctionType AsyncClientPlayerFriendRemoveFriendHandler;

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
};

class ::friendpb::GetFriendListRequest;
using AsyncClientPlayerFriendGetFriendListHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetFriendListResponse&)>;
extern AsyncClientPlayerFriendGetFriendListHandlerFunctionType AsyncClientPlayerFriendGetFriendListHandler;

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
};

class ::friendpb::GetPendingRequestsRequest;
using AsyncClientPlayerFriendGetPendingRequestsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetPendingRequestsResponse&)>;
extern AsyncClientPlayerFriendGetPendingRequestsHandlerFunctionType AsyncClientPlayerFriendGetPendingRequestsHandler;

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
};

class ::friendpb::BlockRequest;
using AsyncClientPlayerFriendBlockHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::BlockResponse&)>;
extern AsyncClientPlayerFriendBlockHandlerFunctionType AsyncClientPlayerFriendBlockHandler;

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
};

class ::friendpb::UnblockRequest;
using AsyncClientPlayerFriendUnblockHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::UnblockResponse&)>;
extern AsyncClientPlayerFriendUnblockHandlerFunctionType AsyncClientPlayerFriendUnblockHandler;

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
};

class ::friendpb::ListBlocksRequest;
using AsyncClientPlayerFriendListBlocksHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::ListBlocksResponse&)>;
extern AsyncClientPlayerFriendListBlocksHandlerFunctionType AsyncClientPlayerFriendListBlocksHandler;

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
};

class ::friendpb::RecommendFriendsRequest;
using AsyncClientPlayerFriendRecommendFriendsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RecommendFriendsResponse&)>;
extern AsyncClientPlayerFriendRecommendFriendsHandlerFunctionType AsyncClientPlayerFriendRecommendFriendsHandler;

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
};

class ::friendpb::FriendEventS2C;
using AsyncClientPlayerFriendNotifyFriendEventHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncClientPlayerFriendNotifyFriendEventHandlerFunctionType AsyncClientPlayerFriendNotifyFriendEventHandler;

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request);
void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetFriendHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetFriendIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleFriendCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitFriendGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace friendpb
