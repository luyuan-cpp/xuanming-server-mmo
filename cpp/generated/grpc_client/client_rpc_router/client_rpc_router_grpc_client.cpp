#include "muduo/base/Logging.h"

#include "client_rpc_router_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace client_rpc_router {
struct ClientRpcRouterCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientRpcRouterForward
boost::object_pool<AsyncClientRpcRouterForwardGrpcClient> ClientRpcRouterForwardPool;
using AsyncClientRpcRouterForwardHandlerFunctionType =
    std::function<void(const ClientContext&, const ::MessageContent&)>;
AsyncClientRpcRouterForwardHandlerFunctionType AsyncClientRpcRouterForwardHandler;

void AsyncCompleteGrpcClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientRpcRouterForwardGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientRpcRouterForwardHandler) {
            AsyncClientRpcRouterForwardHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientRpcRouterForwardPool.destroy(call);
}

void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const ::client_rpc_router::ForwardRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientRpcRouterForwardPool.construct());
    call->response_reader = registry
        .get<ClientRpcRouterStubPtr>(nodeEntity)
        ->PrepareAsyncForward(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientRpcRouterForwardMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const ::client_rpc_router::ForwardRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientRpcRouterForwardPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientRpcRouterStubPtr>(nodeEntity)
        ->PrepareAsyncForward(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientRpcRouterForwardMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::client_rpc_router::ForwardRequest& derived = static_cast<const ::client_rpc_router::ForwardRequest&>(message);
    SendClientRpcRouterForward(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleClientRpcRouterCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientRpcRouterForwardMessageId:
            AsyncCompleteGrpcClientRpcRouterForward(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetClientRpcRouterHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientRpcRouterForwardHandler = handler;
}

void SetClientRpcRouterIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientRpcRouterForwardHandler) {
        AsyncClientRpcRouterForwardHandler = handler;
    }
}

void InitClientRpcRouterGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientRpcRouterStubPtr>(nodeEntity, ClientRpcRouter::NewStub(channel));

}

}// namespace client_rpc_router
