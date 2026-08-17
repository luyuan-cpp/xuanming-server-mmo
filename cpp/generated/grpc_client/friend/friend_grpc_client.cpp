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
#pragma region FriendServiceAddFriend
boost::object_pool<AsyncFriendServiceAddFriendGrpcClient> FriendServiceAddFriendPool;
using AsyncFriendServiceAddFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AddFriendResponse&)>;
AsyncFriendServiceAddFriendHandlerFunctionType AsyncFriendServiceAddFriendHandler;

void AsyncCompleteGrpcFriendServiceAddFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncFriendServiceAddFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncFriendServiceAddFriendHandler) {
            AsyncFriendServiceAddFriendHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	FriendServiceAddFriendPool.destroy(call);
}

void SendFriendServiceAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(FriendServiceAddFriendPool.construct());
    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncAddFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceAddFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(FriendServiceAddFriendPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncAddFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceAddFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceAddFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::AddFriendRequest& derived = static_cast<const ::friendpb::AddFriendRequest&>(message);
    SendFriendServiceAddFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region FriendServiceAcceptFriend
boost::object_pool<AsyncFriendServiceAcceptFriendGrpcClient> FriendServiceAcceptFriendPool;
using AsyncFriendServiceAcceptFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AcceptFriendResponse&)>;
AsyncFriendServiceAcceptFriendHandlerFunctionType AsyncFriendServiceAcceptFriendHandler;

void AsyncCompleteGrpcFriendServiceAcceptFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncFriendServiceAcceptFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncFriendServiceAcceptFriendHandler) {
            AsyncFriendServiceAcceptFriendHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	FriendServiceAcceptFriendPool.destroy(call);
}

void SendFriendServiceAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(FriendServiceAcceptFriendPool.construct());
    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncAcceptFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceAcceptFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(FriendServiceAcceptFriendPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncAcceptFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceAcceptFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::AcceptFriendRequest& derived = static_cast<const ::friendpb::AcceptFriendRequest&>(message);
    SendFriendServiceAcceptFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region FriendServiceRejectFriend
boost::object_pool<AsyncFriendServiceRejectFriendGrpcClient> FriendServiceRejectFriendPool;
using AsyncFriendServiceRejectFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RejectFriendResponse&)>;
AsyncFriendServiceRejectFriendHandlerFunctionType AsyncFriendServiceRejectFriendHandler;

void AsyncCompleteGrpcFriendServiceRejectFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncFriendServiceRejectFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncFriendServiceRejectFriendHandler) {
            AsyncFriendServiceRejectFriendHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	FriendServiceRejectFriendPool.destroy(call);
}

void SendFriendServiceRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(FriendServiceRejectFriendPool.construct());
    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRejectFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceRejectFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(FriendServiceRejectFriendPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRejectFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceRejectFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceRejectFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::RejectFriendRequest& derived = static_cast<const ::friendpb::RejectFriendRequest&>(message);
    SendFriendServiceRejectFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region FriendServiceRemoveFriend
boost::object_pool<AsyncFriendServiceRemoveFriendGrpcClient> FriendServiceRemoveFriendPool;
using AsyncFriendServiceRemoveFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RemoveFriendResponse&)>;
AsyncFriendServiceRemoveFriendHandlerFunctionType AsyncFriendServiceRemoveFriendHandler;

void AsyncCompleteGrpcFriendServiceRemoveFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncFriendServiceRemoveFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncFriendServiceRemoveFriendHandler) {
            AsyncFriendServiceRemoveFriendHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	FriendServiceRemoveFriendPool.destroy(call);
}

void SendFriendServiceRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(FriendServiceRemoveFriendPool.construct());
    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRemoveFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceRemoveFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(FriendServiceRemoveFriendPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRemoveFriend(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceRemoveFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::RemoveFriendRequest& derived = static_cast<const ::friendpb::RemoveFriendRequest&>(message);
    SendFriendServiceRemoveFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region FriendServiceGetFriendList
boost::object_pool<AsyncFriendServiceGetFriendListGrpcClient> FriendServiceGetFriendListPool;
using AsyncFriendServiceGetFriendListHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetFriendListResponse&)>;
AsyncFriendServiceGetFriendListHandlerFunctionType AsyncFriendServiceGetFriendListHandler;

void AsyncCompleteGrpcFriendServiceGetFriendList(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncFriendServiceGetFriendListGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncFriendServiceGetFriendListHandler) {
            AsyncFriendServiceGetFriendListHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	FriendServiceGetFriendListPool.destroy(call);
}

void SendFriendServiceGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(FriendServiceGetFriendListPool.construct());
    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetFriendList(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceGetFriendListMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(FriendServiceGetFriendListPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetFriendList(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceGetFriendListMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceGetFriendList(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::GetFriendListRequest& derived = static_cast<const ::friendpb::GetFriendListRequest&>(message);
    SendFriendServiceGetFriendList(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region FriendServiceGetPendingRequests
boost::object_pool<AsyncFriendServiceGetPendingRequestsGrpcClient> FriendServiceGetPendingRequestsPool;
using AsyncFriendServiceGetPendingRequestsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetPendingRequestsResponse&)>;
AsyncFriendServiceGetPendingRequestsHandlerFunctionType AsyncFriendServiceGetPendingRequestsHandler;

void AsyncCompleteGrpcFriendServiceGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncFriendServiceGetPendingRequestsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncFriendServiceGetPendingRequestsHandler) {
            AsyncFriendServiceGetPendingRequestsHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	FriendServiceGetPendingRequestsPool.destroy(call);
}

void SendFriendServiceGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(FriendServiceGetPendingRequestsPool.construct());
    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPendingRequests(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceGetPendingRequestsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(FriendServiceGetPendingRequestsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPendingRequests(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceGetPendingRequestsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::GetPendingRequestsRequest& derived = static_cast<const ::friendpb::GetPendingRequestsRequest&>(message);
    SendFriendServiceGetPendingRequests(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region FriendServiceNotifyOnline
boost::object_pool<AsyncFriendServiceNotifyOnlineGrpcClient> FriendServiceNotifyOnlinePool;
using AsyncFriendServiceNotifyOnlineHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::NotifyOnlineResponse&)>;
AsyncFriendServiceNotifyOnlineHandlerFunctionType AsyncFriendServiceNotifyOnlineHandler;

void AsyncCompleteGrpcFriendServiceNotifyOnline(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncFriendServiceNotifyOnlineGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncFriendServiceNotifyOnlineHandler) {
            AsyncFriendServiceNotifyOnlineHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	FriendServiceNotifyOnlinePool.destroy(call);
}

void SendFriendServiceNotifyOnline(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::NotifyOnlineRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(FriendServiceNotifyOnlinePool.construct());
    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyOnline(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceNotifyOnlineMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceNotifyOnline(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::NotifyOnlineRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(FriendServiceNotifyOnlinePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyOnline(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceNotifyOnlineMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceNotifyOnline(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::NotifyOnlineRequest& derived = static_cast<const ::friendpb::NotifyOnlineRequest&>(message);
    SendFriendServiceNotifyOnline(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region FriendServiceNotifyOffline
boost::object_pool<AsyncFriendServiceNotifyOfflineGrpcClient> FriendServiceNotifyOfflinePool;
using AsyncFriendServiceNotifyOfflineHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::NotifyOfflineResponse&)>;
AsyncFriendServiceNotifyOfflineHandlerFunctionType AsyncFriendServiceNotifyOfflineHandler;

void AsyncCompleteGrpcFriendServiceNotifyOffline(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncFriendServiceNotifyOfflineGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncFriendServiceNotifyOfflineHandler) {
            AsyncFriendServiceNotifyOfflineHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	FriendServiceNotifyOfflinePool.destroy(call);
}

void SendFriendServiceNotifyOffline(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::NotifyOfflineRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(FriendServiceNotifyOfflinePool.construct());
    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyOffline(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceNotifyOfflineMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceNotifyOffline(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::NotifyOfflineRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(FriendServiceNotifyOfflinePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<FriendServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyOffline(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(FriendServiceNotifyOfflineMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendFriendServiceNotifyOffline(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::NotifyOfflineRequest& derived = static_cast<const ::friendpb::NotifyOfflineRequest&>(message);
    SendFriendServiceNotifyOffline(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleFriendCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case FriendServiceAddFriendMessageId:
            AsyncCompleteGrpcFriendServiceAddFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case FriendServiceAcceptFriendMessageId:
            AsyncCompleteGrpcFriendServiceAcceptFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case FriendServiceRejectFriendMessageId:
            AsyncCompleteGrpcFriendServiceRejectFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case FriendServiceRemoveFriendMessageId:
            AsyncCompleteGrpcFriendServiceRemoveFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case FriendServiceGetFriendListMessageId:
            AsyncCompleteGrpcFriendServiceGetFriendList(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case FriendServiceGetPendingRequestsMessageId:
            AsyncCompleteGrpcFriendServiceGetPendingRequests(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case FriendServiceNotifyOnlineMessageId:
            AsyncCompleteGrpcFriendServiceNotifyOnline(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case FriendServiceNotifyOfflineMessageId:
            AsyncCompleteGrpcFriendServiceNotifyOffline(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetFriendHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncFriendServiceAddFriendHandler = handler;
    AsyncFriendServiceAcceptFriendHandler = handler;
    AsyncFriendServiceRejectFriendHandler = handler;
    AsyncFriendServiceRemoveFriendHandler = handler;
    AsyncFriendServiceGetFriendListHandler = handler;
    AsyncFriendServiceGetPendingRequestsHandler = handler;
    AsyncFriendServiceNotifyOnlineHandler = handler;
    AsyncFriendServiceNotifyOfflineHandler = handler;
}

void SetFriendIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncFriendServiceAddFriendHandler) {
        AsyncFriendServiceAddFriendHandler = handler;
    }
    if (!AsyncFriendServiceAcceptFriendHandler) {
        AsyncFriendServiceAcceptFriendHandler = handler;
    }
    if (!AsyncFriendServiceRejectFriendHandler) {
        AsyncFriendServiceRejectFriendHandler = handler;
    }
    if (!AsyncFriendServiceRemoveFriendHandler) {
        AsyncFriendServiceRemoveFriendHandler = handler;
    }
    if (!AsyncFriendServiceGetFriendListHandler) {
        AsyncFriendServiceGetFriendListHandler = handler;
    }
    if (!AsyncFriendServiceGetPendingRequestsHandler) {
        AsyncFriendServiceGetPendingRequestsHandler = handler;
    }
    if (!AsyncFriendServiceNotifyOnlineHandler) {
        AsyncFriendServiceNotifyOnlineHandler = handler;
    }
    if (!AsyncFriendServiceNotifyOfflineHandler) {
        AsyncFriendServiceNotifyOfflineHandler = handler;
    }
}

void InitFriendGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<FriendServiceStubPtr>(nodeEntity, FriendService::NewStub(channel));

}

}// namespace friendpb
