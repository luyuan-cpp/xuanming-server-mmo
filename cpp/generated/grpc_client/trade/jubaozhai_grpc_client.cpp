#include "muduo/base/Logging.h"

#include "jubaozhai_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace trade {
struct JubaozhaiCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientPlayerJubaozhaiBrowseListings
boost::object_pool<AsyncClientPlayerJubaozhaiBrowseListingsGrpcClient> ClientPlayerJubaozhaiBrowseListingsPool;
using AsyncClientPlayerJubaozhaiBrowseListingsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::BrowseListingsResponse&)>;
AsyncClientPlayerJubaozhaiBrowseListingsHandlerFunctionType AsyncClientPlayerJubaozhaiBrowseListingsHandler;

void AsyncCompleteGrpcClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerJubaozhaiBrowseListingsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerJubaozhaiBrowseListingsHandler) {
            AsyncClientPlayerJubaozhaiBrowseListingsHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerJubaozhaiBrowseListingsPool.destroy(call);
}

void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const ::trade::BrowseListingsRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerJubaozhaiBrowseListingsPool.construct());
    call->response_reader = registry
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncBrowseListings(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiBrowseListingsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const ::trade::BrowseListingsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerJubaozhaiBrowseListingsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncBrowseListings(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiBrowseListingsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::BrowseListingsRequest& derived = static_cast<const ::trade::BrowseListingsRequest&>(message);
    SendClientPlayerJubaozhaiBrowseListings(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetListingDetail
boost::object_pool<AsyncClientPlayerJubaozhaiGetListingDetailGrpcClient> ClientPlayerJubaozhaiGetListingDetailPool;
using AsyncClientPlayerJubaozhaiGetListingDetailHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::GetListingDetailResponse&)>;
AsyncClientPlayerJubaozhaiGetListingDetailHandlerFunctionType AsyncClientPlayerJubaozhaiGetListingDetailHandler;

void AsyncCompleteGrpcClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerJubaozhaiGetListingDetailGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerJubaozhaiGetListingDetailHandler) {
            AsyncClientPlayerJubaozhaiGetListingDetailHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerJubaozhaiGetListingDetailPool.destroy(call);
}

void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetListingDetailRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerJubaozhaiGetListingDetailPool.construct());
    call->response_reader = registry
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncGetListingDetail(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiGetListingDetailMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetListingDetailRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerJubaozhaiGetListingDetailPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncGetListingDetail(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiGetListingDetailMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::GetListingDetailRequest& derived = static_cast<const ::trade::GetListingDetailRequest&>(message);
    SendClientPlayerJubaozhaiGetListingDetail(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerJubaozhaiSetFavorite
boost::object_pool<AsyncClientPlayerJubaozhaiSetFavoriteGrpcClient> ClientPlayerJubaozhaiSetFavoritePool;
using AsyncClientPlayerJubaozhaiSetFavoriteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::SetFavoriteResponse&)>;
AsyncClientPlayerJubaozhaiSetFavoriteHandlerFunctionType AsyncClientPlayerJubaozhaiSetFavoriteHandler;

void AsyncCompleteGrpcClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerJubaozhaiSetFavoriteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerJubaozhaiSetFavoriteHandler) {
            AsyncClientPlayerJubaozhaiSetFavoriteHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerJubaozhaiSetFavoritePool.destroy(call);
}

void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const ::trade::SetFavoriteRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerJubaozhaiSetFavoritePool.construct());
    call->response_reader = registry
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncSetFavorite(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiSetFavoriteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const ::trade::SetFavoriteRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerJubaozhaiSetFavoritePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncSetFavorite(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiSetFavoriteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::SetFavoriteRequest& derived = static_cast<const ::trade::SetFavoriteRequest&>(message);
    SendClientPlayerJubaozhaiSetFavorite(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetMyShelf
boost::object_pool<AsyncClientPlayerJubaozhaiGetMyShelfGrpcClient> ClientPlayerJubaozhaiGetMyShelfPool;
using AsyncClientPlayerJubaozhaiGetMyShelfHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::GetMyShelfResponse&)>;
AsyncClientPlayerJubaozhaiGetMyShelfHandlerFunctionType AsyncClientPlayerJubaozhaiGetMyShelfHandler;

void AsyncCompleteGrpcClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerJubaozhaiGetMyShelfGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerJubaozhaiGetMyShelfHandler) {
            AsyncClientPlayerJubaozhaiGetMyShelfHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerJubaozhaiGetMyShelfPool.destroy(call);
}

void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerJubaozhaiGetMyShelfPool.construct());
    call->response_reader = registry
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncGetMyShelf(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiGetMyShelfMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerJubaozhaiGetMyShelfPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncGetMyShelf(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiGetMyShelfMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::GetMyShelfRequest& derived = static_cast<const ::trade::GetMyShelfRequest&>(message);
    SendClientPlayerJubaozhaiGetMyShelf(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleJubaozhaiCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientPlayerJubaozhaiBrowseListingsMessageId:
            AsyncCompleteGrpcClientPlayerJubaozhaiBrowseListings(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerJubaozhaiGetListingDetailMessageId:
            AsyncCompleteGrpcClientPlayerJubaozhaiGetListingDetail(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerJubaozhaiSetFavoriteMessageId:
            AsyncCompleteGrpcClientPlayerJubaozhaiSetFavorite(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerJubaozhaiGetMyShelfMessageId:
            AsyncCompleteGrpcClientPlayerJubaozhaiGetMyShelf(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetJubaozhaiHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientPlayerJubaozhaiBrowseListingsHandler = handler;
    AsyncClientPlayerJubaozhaiGetListingDetailHandler = handler;
    AsyncClientPlayerJubaozhaiSetFavoriteHandler = handler;
    AsyncClientPlayerJubaozhaiGetMyShelfHandler = handler;
}

void SetJubaozhaiIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientPlayerJubaozhaiBrowseListingsHandler) {
        AsyncClientPlayerJubaozhaiBrowseListingsHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiGetListingDetailHandler) {
        AsyncClientPlayerJubaozhaiGetListingDetailHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiSetFavoriteHandler) {
        AsyncClientPlayerJubaozhaiSetFavoriteHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiGetMyShelfHandler) {
        AsyncClientPlayerJubaozhaiGetMyShelfHandler = handler;
    }
}

void InitJubaozhaiGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerJubaozhaiStubPtr>(nodeEntity, ClientPlayerJubaozhai::NewStub(channel));

}

}// namespace trade
