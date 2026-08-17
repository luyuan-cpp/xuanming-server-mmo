#include "muduo/base/Logging.h"

#include "scene_manager_service_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace scene_manager {
struct SceneManagerServiceCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region SceneManagerCreateScene
boost::object_pool<AsyncSceneManagerCreateSceneGrpcClient> SceneManagerCreateScenePool;
using AsyncSceneManagerCreateSceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::scene_manager::CreateSceneResponse&)>;
AsyncSceneManagerCreateSceneHandlerFunctionType AsyncSceneManagerCreateSceneHandler;

void AsyncCompleteGrpcSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneManagerCreateSceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneManagerCreateSceneHandler) {
            AsyncSceneManagerCreateSceneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneManagerCreateScenePool.destroy(call);
}

void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::CreateSceneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneManagerCreateScenePool.construct());
    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncCreateScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneManagerCreateSceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::CreateSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneManagerCreateScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncCreateScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneManagerCreateSceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::scene_manager::CreateSceneRequest& derived = static_cast<const ::scene_manager::CreateSceneRequest&>(message);
    SendSceneManagerCreateScene(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneManagerDestroyScene
boost::object_pool<AsyncSceneManagerDestroySceneGrpcClient> SceneManagerDestroyScenePool;
using AsyncSceneManagerDestroySceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncSceneManagerDestroySceneHandlerFunctionType AsyncSceneManagerDestroySceneHandler;

void AsyncCompleteGrpcSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneManagerDestroySceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneManagerDestroySceneHandler) {
            AsyncSceneManagerDestroySceneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneManagerDestroyScenePool.destroy(call);
}

void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::DestroySceneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneManagerDestroyScenePool.construct());
    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneManagerDestroySceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::DestroySceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneManagerDestroyScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneManagerDestroySceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::scene_manager::DestroySceneRequest& derived = static_cast<const ::scene_manager::DestroySceneRequest&>(message);
    SendSceneManagerDestroyScene(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneManagerEnterScene
boost::object_pool<AsyncSceneManagerEnterSceneGrpcClient> SceneManagerEnterScenePool;
using AsyncSceneManagerEnterSceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::scene_manager::EnterSceneResponse&)>;
AsyncSceneManagerEnterSceneHandlerFunctionType AsyncSceneManagerEnterSceneHandler;

void AsyncCompleteGrpcSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneManagerEnterSceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneManagerEnterSceneHandler) {
            AsyncSceneManagerEnterSceneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneManagerEnterScenePool.destroy(call);
}

void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneManagerEnterScenePool.construct());
    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncEnterScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneManagerEnterSceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneManagerEnterScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncEnterScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneManagerEnterSceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::scene_manager::EnterSceneRequest& derived = static_cast<const ::scene_manager::EnterSceneRequest&>(message);
    SendSceneManagerEnterScene(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneManagerLeaveScene
boost::object_pool<AsyncSceneManagerLeaveSceneGrpcClient> SceneManagerLeaveScenePool;
using AsyncSceneManagerLeaveSceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncSceneManagerLeaveSceneHandlerFunctionType AsyncSceneManagerLeaveSceneHandler;

void AsyncCompleteGrpcSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneManagerLeaveSceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneManagerLeaveSceneHandler) {
            AsyncSceneManagerLeaveSceneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	SceneManagerLeaveScenePool.destroy(call);
}

void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::LeaveSceneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(SceneManagerLeaveScenePool.construct());
    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneManagerLeaveSceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::LeaveSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneManagerLeaveScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveScene(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneManagerLeaveSceneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::scene_manager::LeaveSceneRequest& derived = static_cast<const ::scene_manager::LeaveSceneRequest&>(message);
    SendSceneManagerLeaveScene(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleSceneManagerServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case SceneManagerCreateSceneMessageId:
            AsyncCompleteGrpcSceneManagerCreateScene(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneManagerDestroySceneMessageId:
            AsyncCompleteGrpcSceneManagerDestroyScene(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneManagerEnterSceneMessageId:
            AsyncCompleteGrpcSceneManagerEnterScene(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneManagerLeaveSceneMessageId:
            AsyncCompleteGrpcSceneManagerLeaveScene(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetSceneManagerServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncSceneManagerCreateSceneHandler = handler;
    AsyncSceneManagerDestroySceneHandler = handler;
    AsyncSceneManagerEnterSceneHandler = handler;
    AsyncSceneManagerLeaveSceneHandler = handler;
}

void SetSceneManagerServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncSceneManagerCreateSceneHandler) {
        AsyncSceneManagerCreateSceneHandler = handler;
    }
    if (!AsyncSceneManagerDestroySceneHandler) {
        AsyncSceneManagerDestroySceneHandler = handler;
    }
    if (!AsyncSceneManagerEnterSceneHandler) {
        AsyncSceneManagerEnterSceneHandler = handler;
    }
    if (!AsyncSceneManagerLeaveSceneHandler) {
        AsyncSceneManagerLeaveSceneHandler = handler;
    }
}

void InitSceneManagerServiceGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<SceneManagerStubPtr>(nodeEntity, SceneManager::NewStub(channel));

}

}// namespace scene_manager
