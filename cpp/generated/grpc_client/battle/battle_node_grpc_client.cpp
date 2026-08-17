#include "muduo/base/Logging.h"

#include "battle_node_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

struct BattleNodeCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region BattleNodeCreateBattle
boost::object_pool<AsyncBattleNodeCreateBattleGrpcClient> BattleNodeCreateBattlePool;
using AsyncBattleNodeCreateBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::CreateBattleResponse&)>;
AsyncBattleNodeCreateBattleHandlerFunctionType AsyncBattleNodeCreateBattleHandler;

void AsyncCompleteGrpcBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleNodeCreateBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleNodeCreateBattleHandler) {
            AsyncBattleNodeCreateBattleHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleNodeCreateBattlePool.destroy(call);
}

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleNodeCreateBattlePool.construct());
    call->response_reader = registry
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncCreateBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeCreateBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleNodeCreateBattlePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncCreateBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeCreateBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::CreateBattleRequest& derived = static_cast<const ::CreateBattleRequest&>(message);
    SendBattleNodeCreateBattle(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleNodeDestroyBattle
boost::object_pool<AsyncBattleNodeDestroyBattleGrpcClient> BattleNodeDestroyBattlePool;
using AsyncBattleNodeDestroyBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleNodeDestroyBattleHandlerFunctionType AsyncBattleNodeDestroyBattleHandler;

void AsyncCompleteGrpcBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleNodeDestroyBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleNodeDestroyBattleHandler) {
            AsyncBattleNodeDestroyBattleHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleNodeDestroyBattlePool.destroy(call);
}

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleNodeDestroyBattlePool.construct());
    call->response_reader = registry
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeDestroyBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleNodeDestroyBattlePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeDestroyBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::DestroyBattleRequest& derived = static_cast<const ::DestroyBattleRequest&>(message);
    SendBattleNodeDestroyBattle(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleBattleNodeCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case BattleNodeCreateBattleMessageId:
            AsyncCompleteGrpcBattleNodeCreateBattle(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleNodeDestroyBattleMessageId:
            AsyncCompleteGrpcBattleNodeDestroyBattle(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetBattleNodeHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncBattleNodeCreateBattleHandler = handler;
    AsyncBattleNodeDestroyBattleHandler = handler;
}

void SetBattleNodeIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncBattleNodeCreateBattleHandler) {
        AsyncBattleNodeCreateBattleHandler = handler;
    }
    if (!AsyncBattleNodeDestroyBattleHandler) {
        AsyncBattleNodeDestroyBattleHandler = handler;
    }
}

void InitBattleNodeGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<BattleNodeStubPtr>(nodeEntity, BattleNode::NewStub(channel));

}
