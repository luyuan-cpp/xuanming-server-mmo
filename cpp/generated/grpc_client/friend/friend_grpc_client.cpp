#include "muduo/base/Logging.h"

#include "friend_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace friendpb {
struct FriendCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientPlayerFriendAddFriend
boost::object_pool<AsyncClientPlayerFriendAddFriendGrpcClient> ClientPlayerFriendAddFriendPool;
using AsyncClientPlayerFriendAddFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AddFriendResponse&)>;
AsyncClientPlayerFriendAddFriendHandlerFunctionType AsyncClientPlayerFriendAddFriendHandler;

void AsyncCompleteGrpcClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendAddFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendAddFriendHandler) {
            AsyncClientPlayerFriendAddFriendHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendAddFriendPool.destroy(call);
}

void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendAddFriendPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncAddFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendAddFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendAddFriendPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncAddFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendAddFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::AddFriendRequest& derived = static_cast<const ::friendpb::AddFriendRequest&>(message);
    SendClientPlayerFriendAddFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendAcceptFriend
boost::object_pool<AsyncClientPlayerFriendAcceptFriendGrpcClient> ClientPlayerFriendAcceptFriendPool;
using AsyncClientPlayerFriendAcceptFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AcceptFriendResponse&)>;
AsyncClientPlayerFriendAcceptFriendHandlerFunctionType AsyncClientPlayerFriendAcceptFriendHandler;

void AsyncCompleteGrpcClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendAcceptFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendAcceptFriendHandler) {
            AsyncClientPlayerFriendAcceptFriendHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendAcceptFriendPool.destroy(call);
}

void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendAcceptFriendPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncAcceptFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendAcceptFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendAcceptFriendPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncAcceptFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendAcceptFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::AcceptFriendRequest& derived = static_cast<const ::friendpb::AcceptFriendRequest&>(message);
    SendClientPlayerFriendAcceptFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendRejectFriend
boost::object_pool<AsyncClientPlayerFriendRejectFriendGrpcClient> ClientPlayerFriendRejectFriendPool;
using AsyncClientPlayerFriendRejectFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RejectFriendResponse&)>;
AsyncClientPlayerFriendRejectFriendHandlerFunctionType AsyncClientPlayerFriendRejectFriendHandler;

void AsyncCompleteGrpcClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendRejectFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendRejectFriendHandler) {
            AsyncClientPlayerFriendRejectFriendHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendRejectFriendPool.destroy(call);
}

void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendRejectFriendPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRejectFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRejectFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendRejectFriendPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRejectFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRejectFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::RejectFriendRequest& derived = static_cast<const ::friendpb::RejectFriendRequest&>(message);
    SendClientPlayerFriendRejectFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendRemoveFriend
boost::object_pool<AsyncClientPlayerFriendRemoveFriendGrpcClient> ClientPlayerFriendRemoveFriendPool;
using AsyncClientPlayerFriendRemoveFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RemoveFriendResponse&)>;
AsyncClientPlayerFriendRemoveFriendHandlerFunctionType AsyncClientPlayerFriendRemoveFriendHandler;

void AsyncCompleteGrpcClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendRemoveFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendRemoveFriendHandler) {
            AsyncClientPlayerFriendRemoveFriendHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendRemoveFriendPool.destroy(call);
}

void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendRemoveFriendPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRemoveFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRemoveFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendRemoveFriendPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRemoveFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRemoveFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::RemoveFriendRequest& derived = static_cast<const ::friendpb::RemoveFriendRequest&>(message);
    SendClientPlayerFriendRemoveFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendGetFriendList
boost::object_pool<AsyncClientPlayerFriendGetFriendListGrpcClient> ClientPlayerFriendGetFriendListPool;
using AsyncClientPlayerFriendGetFriendListHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetFriendListResponse&)>;
AsyncClientPlayerFriendGetFriendListHandlerFunctionType AsyncClientPlayerFriendGetFriendListHandler;

void AsyncCompleteGrpcClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendGetFriendListGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendGetFriendListHandler) {
            AsyncClientPlayerFriendGetFriendListHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendGetFriendListPool.destroy(call);
}

void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendGetFriendListPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncGetFriendList(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendGetFriendListMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendGetFriendListPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncGetFriendList(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendGetFriendListMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::GetFriendListRequest& derived = static_cast<const ::friendpb::GetFriendListRequest&>(message);
    SendClientPlayerFriendGetFriendList(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendGetPendingRequests
boost::object_pool<AsyncClientPlayerFriendGetPendingRequestsGrpcClient> ClientPlayerFriendGetPendingRequestsPool;
using AsyncClientPlayerFriendGetPendingRequestsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetPendingRequestsResponse&)>;
AsyncClientPlayerFriendGetPendingRequestsHandlerFunctionType AsyncClientPlayerFriendGetPendingRequestsHandler;

void AsyncCompleteGrpcClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendGetPendingRequestsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendGetPendingRequestsHandler) {
            AsyncClientPlayerFriendGetPendingRequestsHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendGetPendingRequestsPool.destroy(call);
}

void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendGetPendingRequestsPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncGetPendingRequests(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendGetPendingRequestsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendGetPendingRequestsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncGetPendingRequests(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendGetPendingRequestsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::GetPendingRequestsRequest& derived = static_cast<const ::friendpb::GetPendingRequestsRequest&>(message);
    SendClientPlayerFriendGetPendingRequests(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendBlock
boost::object_pool<AsyncClientPlayerFriendBlockGrpcClient> ClientPlayerFriendBlockPool;
using AsyncClientPlayerFriendBlockHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::BlockResponse&)>;
AsyncClientPlayerFriendBlockHandlerFunctionType AsyncClientPlayerFriendBlockHandler;

void AsyncCompleteGrpcClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendBlockGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendBlockHandler) {
            AsyncClientPlayerFriendBlockHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendBlockPool.destroy(call);
}

void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendBlockPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncBlock(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendBlockMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendBlockPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncBlock(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendBlockMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::BlockRequest& derived = static_cast<const ::friendpb::BlockRequest&>(message);
    SendClientPlayerFriendBlock(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendUnblock
boost::object_pool<AsyncClientPlayerFriendUnblockGrpcClient> ClientPlayerFriendUnblockPool;
using AsyncClientPlayerFriendUnblockHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::UnblockResponse&)>;
AsyncClientPlayerFriendUnblockHandlerFunctionType AsyncClientPlayerFriendUnblockHandler;

void AsyncCompleteGrpcClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendUnblockGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendUnblockHandler) {
            AsyncClientPlayerFriendUnblockHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendUnblockPool.destroy(call);
}

void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendUnblockPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncUnblock(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendUnblockMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendUnblockPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncUnblock(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendUnblockMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::UnblockRequest& derived = static_cast<const ::friendpb::UnblockRequest&>(message);
    SendClientPlayerFriendUnblock(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendListBlocks
boost::object_pool<AsyncClientPlayerFriendListBlocksGrpcClient> ClientPlayerFriendListBlocksPool;
using AsyncClientPlayerFriendListBlocksHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::ListBlocksResponse&)>;
AsyncClientPlayerFriendListBlocksHandlerFunctionType AsyncClientPlayerFriendListBlocksHandler;

void AsyncCompleteGrpcClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendListBlocksGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendListBlocksHandler) {
            AsyncClientPlayerFriendListBlocksHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendListBlocksPool.destroy(call);
}

void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendListBlocksPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncListBlocks(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendListBlocksMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendListBlocksPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncListBlocks(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendListBlocksMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::ListBlocksRequest& derived = static_cast<const ::friendpb::ListBlocksRequest&>(message);
    SendClientPlayerFriendListBlocks(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendRecommendFriends
boost::object_pool<AsyncClientPlayerFriendRecommendFriendsGrpcClient> ClientPlayerFriendRecommendFriendsPool;
using AsyncClientPlayerFriendRecommendFriendsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RecommendFriendsResponse&)>;
AsyncClientPlayerFriendRecommendFriendsHandlerFunctionType AsyncClientPlayerFriendRecommendFriendsHandler;

void AsyncCompleteGrpcClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendRecommendFriendsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendRecommendFriendsHandler) {
            AsyncClientPlayerFriendRecommendFriendsHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendRecommendFriendsPool.destroy(call);
}

void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendRecommendFriendsPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRecommendFriends(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRecommendFriendsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendRecommendFriendsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRecommendFriends(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRecommendFriendsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::RecommendFriendsRequest& derived = static_cast<const ::friendpb::RecommendFriendsRequest&>(message);
    SendClientPlayerFriendRecommendFriends(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendNotifyFriendEvent
boost::object_pool<AsyncClientPlayerFriendNotifyFriendEventGrpcClient> ClientPlayerFriendNotifyFriendEventPool;
using AsyncClientPlayerFriendNotifyFriendEventHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncClientPlayerFriendNotifyFriendEventHandlerFunctionType AsyncClientPlayerFriendNotifyFriendEventHandler;

void AsyncCompleteGrpcClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendNotifyFriendEventGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendNotifyFriendEventHandler) {
            AsyncClientPlayerFriendNotifyFriendEventHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerFriendNotifyFriendEventPool.destroy(call);
}

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerFriendNotifyFriendEventPool.construct());
    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyFriendEvent(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendNotifyFriendEventMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendNotifyFriendEventPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyFriendEvent(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendNotifyFriendEventMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::FriendEventS2C& derived = static_cast<const ::friendpb::FriendEventS2C&>(message);
    SendClientPlayerFriendNotifyFriendEvent(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleFriendCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientPlayerFriendAddFriendMessageId:
            AsyncCompleteGrpcClientPlayerFriendAddFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendAcceptFriendMessageId:
            AsyncCompleteGrpcClientPlayerFriendAcceptFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendRejectFriendMessageId:
            AsyncCompleteGrpcClientPlayerFriendRejectFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendRemoveFriendMessageId:
            AsyncCompleteGrpcClientPlayerFriendRemoveFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendGetFriendListMessageId:
            AsyncCompleteGrpcClientPlayerFriendGetFriendList(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendGetPendingRequestsMessageId:
            AsyncCompleteGrpcClientPlayerFriendGetPendingRequests(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendBlockMessageId:
            AsyncCompleteGrpcClientPlayerFriendBlock(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendUnblockMessageId:
            AsyncCompleteGrpcClientPlayerFriendUnblock(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendListBlocksMessageId:
            AsyncCompleteGrpcClientPlayerFriendListBlocks(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendRecommendFriendsMessageId:
            AsyncCompleteGrpcClientPlayerFriendRecommendFriends(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendNotifyFriendEventMessageId:
            AsyncCompleteGrpcClientPlayerFriendNotifyFriendEvent(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetFriendHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientPlayerFriendAddFriendHandler = handler;
    AsyncClientPlayerFriendAcceptFriendHandler = handler;
    AsyncClientPlayerFriendRejectFriendHandler = handler;
    AsyncClientPlayerFriendRemoveFriendHandler = handler;
    AsyncClientPlayerFriendGetFriendListHandler = handler;
    AsyncClientPlayerFriendGetPendingRequestsHandler = handler;
    AsyncClientPlayerFriendBlockHandler = handler;
    AsyncClientPlayerFriendUnblockHandler = handler;
    AsyncClientPlayerFriendListBlocksHandler = handler;
    AsyncClientPlayerFriendRecommendFriendsHandler = handler;
    AsyncClientPlayerFriendNotifyFriendEventHandler = handler;
}

void SetFriendIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientPlayerFriendAddFriendHandler) {
        AsyncClientPlayerFriendAddFriendHandler = handler;
    }
    if (!AsyncClientPlayerFriendAcceptFriendHandler) {
        AsyncClientPlayerFriendAcceptFriendHandler = handler;
    }
    if (!AsyncClientPlayerFriendRejectFriendHandler) {
        AsyncClientPlayerFriendRejectFriendHandler = handler;
    }
    if (!AsyncClientPlayerFriendRemoveFriendHandler) {
        AsyncClientPlayerFriendRemoveFriendHandler = handler;
    }
    if (!AsyncClientPlayerFriendGetFriendListHandler) {
        AsyncClientPlayerFriendGetFriendListHandler = handler;
    }
    if (!AsyncClientPlayerFriendGetPendingRequestsHandler) {
        AsyncClientPlayerFriendGetPendingRequestsHandler = handler;
    }
    if (!AsyncClientPlayerFriendBlockHandler) {
        AsyncClientPlayerFriendBlockHandler = handler;
    }
    if (!AsyncClientPlayerFriendUnblockHandler) {
        AsyncClientPlayerFriendUnblockHandler = handler;
    }
    if (!AsyncClientPlayerFriendListBlocksHandler) {
        AsyncClientPlayerFriendListBlocksHandler = handler;
    }
    if (!AsyncClientPlayerFriendRecommendFriendsHandler) {
        AsyncClientPlayerFriendRecommendFriendsHandler = handler;
    }
    if (!AsyncClientPlayerFriendNotifyFriendEventHandler) {
        AsyncClientPlayerFriendNotifyFriendEventHandler = handler;
    }
}

void InitFriendGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerFriendStubPtr>(nodeEntity, ClientPlayerFriend::NewStub(channel));

}

}// namespace friendpb
