#include "muduo/base/Logging.h"

#include "player_battle_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

struct PlayerBattleCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region BattleClientPlayerSubmitBattleAction
boost::object_pool<AsyncBattleClientPlayerSubmitBattleActionGrpcClient> BattleClientPlayerSubmitBattleActionPool;
using AsyncBattleClientPlayerSubmitBattleActionHandlerFunctionType =
    std::function<void(const ClientContext&, const ::SubmitBattleActionResponse&)>;
AsyncBattleClientPlayerSubmitBattleActionHandlerFunctionType AsyncBattleClientPlayerSubmitBattleActionHandler;

void AsyncCompleteGrpcBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerSubmitBattleActionGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerSubmitBattleActionHandler) {
            AsyncBattleClientPlayerSubmitBattleActionHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerSubmitBattleActionPool.destroy(call);
}

void SendBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, const ::SubmitBattleActionRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerSubmitBattleActionPool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncSubmitBattleAction(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerSubmitBattleActionMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, const ::SubmitBattleActionRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerSubmitBattleActionPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncSubmitBattleAction(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerSubmitBattleActionMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::SubmitBattleActionRequest& derived = static_cast<const ::SubmitBattleActionRequest&>(message);
    SendBattleClientPlayerSubmitBattleAction(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerGetBattleState
boost::object_pool<AsyncBattleClientPlayerGetBattleStateGrpcClient> BattleClientPlayerGetBattleStatePool;
using AsyncBattleClientPlayerGetBattleStateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::BattleStateS2C&)>;
AsyncBattleClientPlayerGetBattleStateHandlerFunctionType AsyncBattleClientPlayerGetBattleStateHandler;

void AsyncCompleteGrpcBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerGetBattleStateGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerGetBattleStateHandler) {
            AsyncBattleClientPlayerGetBattleStateHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerGetBattleStatePool.destroy(call);
}

void SendBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, const ::GetBattleStateRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerGetBattleStatePool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncGetBattleState(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerGetBattleStateMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, const ::GetBattleStateRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerGetBattleStatePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncGetBattleState(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerGetBattleStateMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::GetBattleStateRequest& derived = static_cast<const ::GetBattleStateRequest&>(message);
    SendBattleClientPlayerGetBattleState(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleStart
boost::object_pool<AsyncBattleClientPlayerNotifyBattleStartGrpcClient> BattleClientPlayerNotifyBattleStartPool;
using AsyncBattleClientPlayerNotifyBattleStartHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleClientPlayerNotifyBattleStartHandlerFunctionType AsyncBattleClientPlayerNotifyBattleStartHandler;

void AsyncCompleteGrpcBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerNotifyBattleStartGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerNotifyBattleStartHandler) {
            AsyncBattleClientPlayerNotifyBattleStartHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerNotifyBattleStartPool.destroy(call);
}

void SendBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, const ::BattleStartS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerNotifyBattleStartPool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyBattleStart(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyBattleStartMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, const ::BattleStartS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerNotifyBattleStartPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyBattleStart(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyBattleStartMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::BattleStartS2C& derived = static_cast<const ::BattleStartS2C&>(message);
    SendBattleClientPlayerNotifyBattleStart(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerNotifyTurnResult
boost::object_pool<AsyncBattleClientPlayerNotifyTurnResultGrpcClient> BattleClientPlayerNotifyTurnResultPool;
using AsyncBattleClientPlayerNotifyTurnResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleClientPlayerNotifyTurnResultHandlerFunctionType AsyncBattleClientPlayerNotifyTurnResultHandler;

void AsyncCompleteGrpcBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerNotifyTurnResultGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerNotifyTurnResultHandler) {
            AsyncBattleClientPlayerNotifyTurnResultHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerNotifyTurnResultPool.destroy(call);
}

void SendBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerNotifyTurnResultPool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTurnResult(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyTurnResultMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerNotifyTurnResultPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTurnResult(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyTurnResultMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::TurnResultS2C& derived = static_cast<const ::TurnResultS2C&>(message);
    SendBattleClientPlayerNotifyTurnResult(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleEnd
boost::object_pool<AsyncBattleClientPlayerNotifyBattleEndGrpcClient> BattleClientPlayerNotifyBattleEndPool;
using AsyncBattleClientPlayerNotifyBattleEndHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleClientPlayerNotifyBattleEndHandlerFunctionType AsyncBattleClientPlayerNotifyBattleEndHandler;

void AsyncCompleteGrpcBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerNotifyBattleEndGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerNotifyBattleEndHandler) {
            AsyncBattleClientPlayerNotifyBattleEndHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerNotifyBattleEndPool.destroy(call);
}

void SendBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, const ::BattleEndS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerNotifyBattleEndPool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyBattleEnd(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyBattleEndMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, const ::BattleEndS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerNotifyBattleEndPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyBattleEnd(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyBattleEndMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::BattleEndS2C& derived = static_cast<const ::BattleEndS2C&>(message);
    SendBattleClientPlayerNotifyBattleEnd(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleReconnect
boost::object_pool<AsyncBattleClientPlayerNotifyBattleReconnectGrpcClient> BattleClientPlayerNotifyBattleReconnectPool;
using AsyncBattleClientPlayerNotifyBattleReconnectHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleClientPlayerNotifyBattleReconnectHandlerFunctionType AsyncBattleClientPlayerNotifyBattleReconnectHandler;

void AsyncCompleteGrpcBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerNotifyBattleReconnectGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerNotifyBattleReconnectHandler) {
            AsyncBattleClientPlayerNotifyBattleReconnectHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerNotifyBattleReconnectPool.destroy(call);
}

void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, const ::BattleReconnectS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerNotifyBattleReconnectPool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyBattleReconnect(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyBattleReconnectMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, const ::BattleReconnectS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerNotifyBattleReconnectPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyBattleReconnect(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyBattleReconnectMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::BattleReconnectS2C& derived = static_cast<const ::BattleReconnectS2C&>(message);
    SendBattleClientPlayerNotifyBattleReconnect(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandlePlayerBattleCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case BattleClientPlayerSubmitBattleActionMessageId:
            AsyncCompleteGrpcBattleClientPlayerSubmitBattleAction(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerGetBattleStateMessageId:
            AsyncCompleteGrpcBattleClientPlayerGetBattleState(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerNotifyBattleStartMessageId:
            AsyncCompleteGrpcBattleClientPlayerNotifyBattleStart(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerNotifyTurnResultMessageId:
            AsyncCompleteGrpcBattleClientPlayerNotifyTurnResult(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerNotifyBattleEndMessageId:
            AsyncCompleteGrpcBattleClientPlayerNotifyBattleEnd(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerNotifyBattleReconnectMessageId:
            AsyncCompleteGrpcBattleClientPlayerNotifyBattleReconnect(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetPlayerBattleHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncBattleClientPlayerSubmitBattleActionHandler = handler;
    AsyncBattleClientPlayerGetBattleStateHandler = handler;
    AsyncBattleClientPlayerNotifyBattleStartHandler = handler;
    AsyncBattleClientPlayerNotifyTurnResultHandler = handler;
    AsyncBattleClientPlayerNotifyBattleEndHandler = handler;
    AsyncBattleClientPlayerNotifyBattleReconnectHandler = handler;
}

void SetPlayerBattleIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncBattleClientPlayerSubmitBattleActionHandler) {
        AsyncBattleClientPlayerSubmitBattleActionHandler = handler;
    }
    if (!AsyncBattleClientPlayerGetBattleStateHandler) {
        AsyncBattleClientPlayerGetBattleStateHandler = handler;
    }
    if (!AsyncBattleClientPlayerNotifyBattleStartHandler) {
        AsyncBattleClientPlayerNotifyBattleStartHandler = handler;
    }
    if (!AsyncBattleClientPlayerNotifyTurnResultHandler) {
        AsyncBattleClientPlayerNotifyTurnResultHandler = handler;
    }
    if (!AsyncBattleClientPlayerNotifyBattleEndHandler) {
        AsyncBattleClientPlayerNotifyBattleEndHandler = handler;
    }
    if (!AsyncBattleClientPlayerNotifyBattleReconnectHandler) {
        AsyncBattleClientPlayerNotifyBattleReconnectHandler = handler;
    }
}

void InitPlayerBattleGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<BattleClientPlayerStubPtr>(nodeEntity, BattleClientPlayer::NewStub(channel));

}
