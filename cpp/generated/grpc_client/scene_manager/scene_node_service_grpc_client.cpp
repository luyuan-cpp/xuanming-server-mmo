#include "muduo/base/Logging.h"

#include "scene_node_service_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace scene_node {
struct SceneNodeServiceCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region SceneNodeGrpcCreateScene
boost::object_pool<AsyncSceneNodeGrpcCreateSceneGrpcClient> SceneNodeGrpcCreateScenePool;
using AsyncSceneNodeGrpcCreateSceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::CreateSceneResponse&)>;
AsyncSceneNodeGrpcCreateSceneHandlerFunctionType AsyncSceneNodeGrpcCreateSceneHandler;

void AsyncCompleteGrpcSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcCreateSceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcCreateSceneHandler) {
            AsyncSceneNodeGrpcCreateSceneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneNodeGrpcCreateScenePool.destroy(call);
}

void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneNodeGrpcCreateScenePool.construct());
    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncCreateScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcCreateSceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcCreateScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncCreateScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcCreateSceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::CreateSceneRequest& derived = static_cast<const ::CreateSceneRequest&>(message);
    SendSceneNodeGrpcCreateScene(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneNodeGrpcDestroyScene
boost::object_pool<AsyncSceneNodeGrpcDestroySceneGrpcClient> SceneNodeGrpcDestroyScenePool;
using AsyncSceneNodeGrpcDestroySceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncSceneNodeGrpcDestroySceneHandlerFunctionType AsyncSceneNodeGrpcDestroySceneHandler;

void AsyncCompleteGrpcSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcDestroySceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcDestroySceneHandler) {
            AsyncSceneNodeGrpcDestroySceneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneNodeGrpcDestroyScenePool.destroy(call);
}

void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneNodeGrpcDestroyScenePool.construct());
    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcDestroySceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcDestroyScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcDestroySceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::DestroySceneRequest& derived = static_cast<const ::DestroySceneRequest&>(message);
    SendSceneNodeGrpcDestroyScene(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneNodeGrpcReleasePlayer
boost::object_pool<AsyncSceneNodeGrpcReleasePlayerGrpcClient> SceneNodeGrpcReleasePlayerPool;
using AsyncSceneNodeGrpcReleasePlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncSceneNodeGrpcReleasePlayerHandlerFunctionType AsyncSceneNodeGrpcReleasePlayerHandler;

void AsyncCompleteGrpcSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcReleasePlayerGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcReleasePlayerHandler) {
            AsyncSceneNodeGrpcReleasePlayerHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneNodeGrpcReleasePlayerPool.destroy(call);
}

void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneNodeGrpcReleasePlayerPool.construct());
    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncReleasePlayer(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcReleasePlayerMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcReleasePlayerPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncReleasePlayer(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcReleasePlayerMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::scene_node::ReleasePlayerRequest& derived = static_cast<const ::scene_node::ReleasePlayerRequest&>(message);
    SendSceneNodeGrpcReleasePlayer(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneNodeGrpcPrepareBattle
boost::object_pool<AsyncSceneNodeGrpcPrepareBattleGrpcClient> SceneNodeGrpcPrepareBattlePool;
using AsyncSceneNodeGrpcPrepareBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::PrepareBattleResponse&)>;
AsyncSceneNodeGrpcPrepareBattleHandlerFunctionType AsyncSceneNodeGrpcPrepareBattleHandler;

void AsyncCompleteGrpcSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcPrepareBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcPrepareBattleHandler) {
            AsyncSceneNodeGrpcPrepareBattleHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneNodeGrpcPrepareBattlePool.destroy(call);
}

void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneNodeGrpcPrepareBattlePool.construct());
    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncPrepareBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcPrepareBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcPrepareBattlePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncPrepareBattle(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcPrepareBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::PrepareBattleRequest& derived = static_cast<const ::PrepareBattleRequest&>(message);
    SendSceneNodeGrpcPrepareBattle(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneNodeGrpcCancelBattlePrepare
boost::object_pool<AsyncSceneNodeGrpcCancelBattlePrepareGrpcClient> SceneNodeGrpcCancelBattlePreparePool;
using AsyncSceneNodeGrpcCancelBattlePrepareHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncSceneNodeGrpcCancelBattlePrepareHandlerFunctionType AsyncSceneNodeGrpcCancelBattlePrepareHandler;

void AsyncCompleteGrpcSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcCancelBattlePrepareGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcCancelBattlePrepareHandler) {
            AsyncSceneNodeGrpcCancelBattlePrepareHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneNodeGrpcCancelBattlePreparePool.destroy(call);
}

void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneNodeGrpcCancelBattlePreparePool.construct());
    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncCancelBattlePrepare(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcCancelBattlePrepareMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcCancelBattlePreparePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncCancelBattlePrepare(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcCancelBattlePrepareMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::CancelBattlePrepareRequest& derived = static_cast<const ::CancelBattlePrepareRequest&>(message);
    SendSceneNodeGrpcCancelBattlePrepare(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleSceneNodeServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case SceneNodeGrpcCreateSceneMessageId:
            AsyncCompleteGrpcSceneNodeGrpcCreateScene(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneNodeGrpcDestroySceneMessageId:
            AsyncCompleteGrpcSceneNodeGrpcDestroyScene(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneNodeGrpcReleasePlayerMessageId:
            AsyncCompleteGrpcSceneNodeGrpcReleasePlayer(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneNodeGrpcPrepareBattleMessageId:
            AsyncCompleteGrpcSceneNodeGrpcPrepareBattle(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneNodeGrpcCancelBattlePrepareMessageId:
            AsyncCompleteGrpcSceneNodeGrpcCancelBattlePrepare(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetSceneNodeServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncSceneNodeGrpcCreateSceneHandler = handler;
    AsyncSceneNodeGrpcDestroySceneHandler = handler;
    AsyncSceneNodeGrpcReleasePlayerHandler = handler;
    AsyncSceneNodeGrpcPrepareBattleHandler = handler;
    AsyncSceneNodeGrpcCancelBattlePrepareHandler = handler;
}

void SetSceneNodeServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncSceneNodeGrpcCreateSceneHandler) {
        AsyncSceneNodeGrpcCreateSceneHandler = handler;
    }
    if (!AsyncSceneNodeGrpcDestroySceneHandler) {
        AsyncSceneNodeGrpcDestroySceneHandler = handler;
    }
    if (!AsyncSceneNodeGrpcReleasePlayerHandler) {
        AsyncSceneNodeGrpcReleasePlayerHandler = handler;
    }
    if (!AsyncSceneNodeGrpcPrepareBattleHandler) {
        AsyncSceneNodeGrpcPrepareBattleHandler = handler;
    }
    if (!AsyncSceneNodeGrpcCancelBattlePrepareHandler) {
        AsyncSceneNodeGrpcCancelBattlePrepareHandler = handler;
    }
}

void InitSceneNodeServiceGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<SceneNodeGrpcStubPtr>(nodeEntity, SceneNodeGrpc::NewStub(channel));

}

}// namespace scene_node
