#include "muduo/base/Logging.h"

#include "chat_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace chatpb {
struct ChatCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientPlayerChatSendChat
boost::object_pool<AsyncClientPlayerChatSendChatGrpcClient> ClientPlayerChatSendChatPool;
using AsyncClientPlayerChatSendChatHandlerFunctionType =
    std::function<void(const ClientContext&, const ::chatpb::SendChatResponse&)>;
AsyncClientPlayerChatSendChatHandlerFunctionType AsyncClientPlayerChatSendChatHandler;

void AsyncCompleteGrpcClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerChatSendChatGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerChatSendChatHandler) {
            AsyncClientPlayerChatSendChatHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerChatSendChatPool.destroy(call);
}

void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::SendChatRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerChatSendChatPool.construct());
    call->response_reader = registry
        .get<ClientPlayerChatStubPtr>(nodeEntity)
        ->PrepareAsyncSendChat(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerChatSendChatMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::SendChatRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerChatSendChatPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerChatStubPtr>(nodeEntity)
        ->PrepareAsyncSendChat(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerChatSendChatMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::chatpb::SendChatRequest& derived = static_cast<const ::chatpb::SendChatRequest&>(message);
    SendClientPlayerChatSendChat(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerChatPullChatHistory
boost::object_pool<AsyncClientPlayerChatPullChatHistoryGrpcClient> ClientPlayerChatPullChatHistoryPool;
using AsyncClientPlayerChatPullChatHistoryHandlerFunctionType =
    std::function<void(const ClientContext&, const ::chatpb::PullChatHistoryResponse&)>;
AsyncClientPlayerChatPullChatHistoryHandlerFunctionType AsyncClientPlayerChatPullChatHistoryHandler;

void AsyncCompleteGrpcClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerChatPullChatHistoryGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerChatPullChatHistoryHandler) {
            AsyncClientPlayerChatPullChatHistoryHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerChatPullChatHistoryPool.destroy(call);
}

void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::PullChatHistoryRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerChatPullChatHistoryPool.construct());
    call->response_reader = registry
        .get<ClientPlayerChatStubPtr>(nodeEntity)
        ->PrepareAsyncPullChatHistory(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerChatPullChatHistoryMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::PullChatHistoryRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerChatPullChatHistoryPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerChatStubPtr>(nodeEntity)
        ->PrepareAsyncPullChatHistory(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerChatPullChatHistoryMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::chatpb::PullChatHistoryRequest& derived = static_cast<const ::chatpb::PullChatHistoryRequest&>(message);
    SendClientPlayerChatPullChatHistory(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleChatCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientPlayerChatSendChatMessageId:
            AsyncCompleteGrpcClientPlayerChatSendChat(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerChatPullChatHistoryMessageId:
            AsyncCompleteGrpcClientPlayerChatPullChatHistory(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetChatHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientPlayerChatSendChatHandler = handler;
    AsyncClientPlayerChatPullChatHistoryHandler = handler;
}

void SetChatIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientPlayerChatSendChatHandler) {
        AsyncClientPlayerChatSendChatHandler = handler;
    }
    if (!AsyncClientPlayerChatPullChatHistoryHandler) {
        AsyncClientPlayerChatPullChatHistoryHandler = handler;
    }
}

void InitChatGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerChatStubPtr>(nodeEntity, ClientPlayerChat::NewStub(channel));

}

}// namespace chatpb
