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
#pragma region BattleClientPlayerStopWatchBattle
boost::object_pool<AsyncBattleClientPlayerStopWatchBattleGrpcClient> BattleClientPlayerStopWatchBattlePool;
using AsyncBattleClientPlayerStopWatchBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::StopWatchBattleResponse&)>;
AsyncBattleClientPlayerStopWatchBattleHandlerFunctionType AsyncBattleClientPlayerStopWatchBattleHandler;

void AsyncCompleteGrpcBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerStopWatchBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerStopWatchBattleHandler) {
            AsyncBattleClientPlayerStopWatchBattleHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerStopWatchBattlePool.destroy(call);
}

void SendBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::StopWatchBattleRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerStopWatchBattlePool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncStopWatchBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerStopWatchBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::StopWatchBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerStopWatchBattlePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncStopWatchBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerStopWatchBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::StopWatchBattleRequest& derived = static_cast<const ::StopWatchBattleRequest&>(message);
    SendBattleClientPlayerStopWatchBattle(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerSetAutoBattle
boost::object_pool<AsyncBattleClientPlayerSetAutoBattleGrpcClient> BattleClientPlayerSetAutoBattlePool;
using AsyncBattleClientPlayerSetAutoBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::SetAutoBattleResponse&)>;
AsyncBattleClientPlayerSetAutoBattleHandlerFunctionType AsyncBattleClientPlayerSetAutoBattleHandler;

void AsyncCompleteGrpcBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerSetAutoBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerSetAutoBattleHandler) {
            AsyncBattleClientPlayerSetAutoBattleHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerSetAutoBattlePool.destroy(call);
}

void SendBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, const ::SetAutoBattleRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerSetAutoBattlePool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncSetAutoBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerSetAutoBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, const ::SetAutoBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerSetAutoBattlePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncSetAutoBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerSetAutoBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::SetAutoBattleRequest& derived = static_cast<const ::SetAutoBattleRequest&>(message);
    SendBattleClientPlayerSetAutoBattle(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateState
boost::object_pool<AsyncBattleClientPlayerNotifySpectateStateGrpcClient> BattleClientPlayerNotifySpectateStatePool;
using AsyncBattleClientPlayerNotifySpectateStateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleClientPlayerNotifySpectateStateHandlerFunctionType AsyncBattleClientPlayerNotifySpectateStateHandler;

void AsyncCompleteGrpcBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerNotifySpectateStateGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerNotifySpectateStateHandler) {
            AsyncBattleClientPlayerNotifySpectateStateHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerNotifySpectateStatePool.destroy(call);
}

void SendBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, const ::SpectateStateS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerNotifySpectateStatePool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifySpectateState(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifySpectateStateMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, const ::SpectateStateS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerNotifySpectateStatePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifySpectateState(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifySpectateStateMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::SpectateStateS2C& derived = static_cast<const ::SpectateStateS2C&>(message);
    SendBattleClientPlayerNotifySpectateState(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateTurnResult
boost::object_pool<AsyncBattleClientPlayerNotifySpectateTurnResultGrpcClient> BattleClientPlayerNotifySpectateTurnResultPool;
using AsyncBattleClientPlayerNotifySpectateTurnResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleClientPlayerNotifySpectateTurnResultHandlerFunctionType AsyncBattleClientPlayerNotifySpectateTurnResultHandler;

void AsyncCompleteGrpcBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerNotifySpectateTurnResultGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerNotifySpectateTurnResultHandler) {
            AsyncBattleClientPlayerNotifySpectateTurnResultHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerNotifySpectateTurnResultPool.destroy(call);
}

void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerNotifySpectateTurnResultPool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifySpectateTurnResult(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifySpectateTurnResultMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerNotifySpectateTurnResultPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifySpectateTurnResult(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifySpectateTurnResultMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::TurnResultS2C& derived = static_cast<const ::TurnResultS2C&>(message);
    SendBattleClientPlayerNotifySpectateTurnResult(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateEnd
boost::object_pool<AsyncBattleClientPlayerNotifySpectateEndGrpcClient> BattleClientPlayerNotifySpectateEndPool;
using AsyncBattleClientPlayerNotifySpectateEndHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleClientPlayerNotifySpectateEndHandlerFunctionType AsyncBattleClientPlayerNotifySpectateEndHandler;

void AsyncCompleteGrpcBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerNotifySpectateEndGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerNotifySpectateEndHandler) {
            AsyncBattleClientPlayerNotifySpectateEndHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerNotifySpectateEndPool.destroy(call);
}

void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerNotifySpectateEndPool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifySpectateEnd(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifySpectateEndMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerNotifySpectateEndPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifySpectateEnd(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifySpectateEndMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::SpectateEndS2C& derived = static_cast<const ::SpectateEndS2C&>(message);
    SendBattleClientPlayerNotifySpectateEnd(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleAssigned
boost::object_pool<AsyncBattleClientPlayerNotifyBattleAssignedGrpcClient> BattleClientPlayerNotifyBattleAssignedPool;
using AsyncBattleClientPlayerNotifyBattleAssignedHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleClientPlayerNotifyBattleAssignedHandlerFunctionType AsyncBattleClientPlayerNotifyBattleAssignedHandler;

void AsyncCompleteGrpcBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleClientPlayerNotifyBattleAssignedGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleClientPlayerNotifyBattleAssignedHandler) {
            AsyncBattleClientPlayerNotifyBattleAssignedHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	BattleClientPlayerNotifyBattleAssignedPool.destroy(call);
}

void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, const ::BattleAssignedS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(BattleClientPlayerNotifyBattleAssignedPool.construct());
    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyBattleAssigned(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyBattleAssignedMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, const ::BattleAssignedS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleClientPlayerNotifyBattleAssignedPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<BattleClientPlayerStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyBattleAssigned(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleClientPlayerNotifyBattleAssignedMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::BattleAssignedS2C& derived = static_cast<const ::BattleAssignedS2C&>(message);
    SendBattleClientPlayerNotifyBattleAssigned(registry, nodeEntity, derived, metaKeys, metaValues);
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
        case BattleClientPlayerStopWatchBattleMessageId:
            AsyncCompleteGrpcBattleClientPlayerStopWatchBattle(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerSetAutoBattleMessageId:
            AsyncCompleteGrpcBattleClientPlayerSetAutoBattle(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerNotifySpectateStateMessageId:
            AsyncCompleteGrpcBattleClientPlayerNotifySpectateState(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerNotifySpectateTurnResultMessageId:
            AsyncCompleteGrpcBattleClientPlayerNotifySpectateTurnResult(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerNotifySpectateEndMessageId:
            AsyncCompleteGrpcBattleClientPlayerNotifySpectateEnd(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleClientPlayerNotifyBattleAssignedMessageId:
            AsyncCompleteGrpcBattleClientPlayerNotifyBattleAssigned(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
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
    AsyncBattleClientPlayerStopWatchBattleHandler = handler;
    AsyncBattleClientPlayerSetAutoBattleHandler = handler;
    AsyncBattleClientPlayerNotifySpectateStateHandler = handler;
    AsyncBattleClientPlayerNotifySpectateTurnResultHandler = handler;
    AsyncBattleClientPlayerNotifySpectateEndHandler = handler;
    AsyncBattleClientPlayerNotifyBattleAssignedHandler = handler;
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
    if (!AsyncBattleClientPlayerStopWatchBattleHandler) {
        AsyncBattleClientPlayerStopWatchBattleHandler = handler;
    }
    if (!AsyncBattleClientPlayerSetAutoBattleHandler) {
        AsyncBattleClientPlayerSetAutoBattleHandler = handler;
    }
    if (!AsyncBattleClientPlayerNotifySpectateStateHandler) {
        AsyncBattleClientPlayerNotifySpectateStateHandler = handler;
    }
    if (!AsyncBattleClientPlayerNotifySpectateTurnResultHandler) {
        AsyncBattleClientPlayerNotifySpectateTurnResultHandler = handler;
    }
    if (!AsyncBattleClientPlayerNotifySpectateEndHandler) {
        AsyncBattleClientPlayerNotifySpectateEndHandler = handler;
    }
    if (!AsyncBattleClientPlayerNotifyBattleAssignedHandler) {
        AsyncBattleClientPlayerNotifyBattleAssignedHandler = handler;
    }
}

void InitPlayerBattleGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<BattleClientPlayerStubPtr>(nodeEntity, BattleClientPlayer::NewStub(channel));

}
