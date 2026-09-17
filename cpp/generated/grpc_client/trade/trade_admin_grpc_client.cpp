#include "muduo/base/Logging.h"

#include "trade_admin_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace trade {
struct TradeAdminCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region TradeAdminSeedListing
boost::object_pool<AsyncTradeAdminSeedListingGrpcClient> TradeAdminSeedListingPool;
using AsyncTradeAdminSeedListingHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::SeedListingResponse&)>;
AsyncTradeAdminSeedListingHandlerFunctionType AsyncTradeAdminSeedListingHandler;

void AsyncCompleteGrpcTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncTradeAdminSeedListingGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncTradeAdminSeedListingHandler) {
            AsyncTradeAdminSeedListingHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	TradeAdminSeedListingPool.destroy(call);
}

void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const ::trade::SeedListingRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(TradeAdminSeedListingPool.construct());
    call->response_reader = registry
        .get<TradeAdminStubPtr>(nodeEntity)
        ->PrepareAsyncSeedListing(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(TradeAdminSeedListingMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const ::trade::SeedListingRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(TradeAdminSeedListingPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<TradeAdminStubPtr>(nodeEntity)
        ->PrepareAsyncSeedListing(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(TradeAdminSeedListingMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::SeedListingRequest& derived = static_cast<const ::trade::SeedListingRequest&>(message);
    SendTradeAdminSeedListing(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleTradeAdminCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case TradeAdminSeedListingMessageId:
            AsyncCompleteGrpcTradeAdminSeedListing(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetTradeAdminHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncTradeAdminSeedListingHandler = handler;
}

void SetTradeAdminIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncTradeAdminSeedListingHandler) {
        AsyncTradeAdminSeedListingHandler = handler;
    }
}

void InitTradeAdminGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<TradeAdminStubPtr>(nodeEntity, TradeAdmin::NewStub(channel));

}

}// namespace trade
