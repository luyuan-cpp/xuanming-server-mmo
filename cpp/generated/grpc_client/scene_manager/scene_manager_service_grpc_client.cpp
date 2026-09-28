#include "muduo/base/Logging.h"

#include "scene_manager_service_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetSceneManagerServiceCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
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
AsyncSceneManagerCreateSceneFailedHandlerFunctionType AsyncSceneManagerCreateSceneFailedHandler;

void AsyncCompleteGrpcSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneManagerCreateSceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneManagerCreateSceneHandler) {
            AsyncSceneManagerCreateSceneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneManager.CreateScene reply dropped: AsyncSceneManagerCreateSceneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneManagerCreateSceneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneManager.CreateScene", call->context, call->status, call->sentMetadata};
        AsyncSceneManagerCreateSceneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneManager.CreateScene failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneManagerCreateScenePool.destroy(call);
}

void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::CreateSceneRequest& request) {

    SendSceneManagerCreateScene(registry, nodeEntity, request, {}, {});

}

void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::CreateSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneManagerCreateScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncCreateScene(&call->context, call->request,
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
AsyncSceneManagerDestroySceneFailedHandlerFunctionType AsyncSceneManagerDestroySceneFailedHandler;

void AsyncCompleteGrpcSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneManagerDestroySceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneManagerDestroySceneHandler) {
            AsyncSceneManagerDestroySceneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneManager.DestroyScene reply dropped: AsyncSceneManagerDestroySceneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneManagerDestroySceneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneManager.DestroyScene", call->context, call->status, call->sentMetadata};
        AsyncSceneManagerDestroySceneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneManager.DestroyScene failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneManagerDestroyScenePool.destroy(call);
}

void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::DestroySceneRequest& request) {

    SendSceneManagerDestroyScene(registry, nodeEntity, request, {}, {});

}

void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::DestroySceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneManagerDestroyScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyScene(&call->context, call->request,
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
AsyncSceneManagerEnterSceneFailedHandlerFunctionType AsyncSceneManagerEnterSceneFailedHandler;

void AsyncCompleteGrpcSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneManagerEnterSceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneManagerEnterSceneHandler) {
            AsyncSceneManagerEnterSceneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneManager.EnterScene reply dropped: AsyncSceneManagerEnterSceneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneManagerEnterSceneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneManager.EnterScene", call->context, call->status, call->sentMetadata};
        AsyncSceneManagerEnterSceneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneManager.EnterScene failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneManagerEnterScenePool.destroy(call);
}

void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request) {

    SendSceneManagerEnterScene(registry, nodeEntity, request, {}, {});

}

void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneManagerEnterScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncEnterScene(&call->context, call->request,
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
AsyncSceneManagerLeaveSceneFailedHandlerFunctionType AsyncSceneManagerLeaveSceneFailedHandler;

void AsyncCompleteGrpcSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneManagerLeaveSceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneManagerLeaveSceneHandler) {
            AsyncSceneManagerLeaveSceneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneManager.LeaveScene reply dropped: AsyncSceneManagerLeaveSceneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneManagerLeaveSceneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneManager.LeaveScene", call->context, call->status, call->sentMetadata};
        AsyncSceneManagerLeaveSceneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneManager.LeaveScene failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneManagerLeaveScenePool.destroy(call);
}

void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::LeaveSceneRequest& request) {

    SendSceneManagerLeaveScene(registry, nodeEntity, request, {}, {});

}

void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::LeaveSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneManagerLeaveScenePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<SceneManagerStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveScene(&call->context, call->request,
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

void SetSceneManagerServiceFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncSceneManagerCreateSceneFailedHandler = handler;
    AsyncSceneManagerDestroySceneFailedHandler = handler;
    AsyncSceneManagerEnterSceneFailedHandler = handler;
    AsyncSceneManagerLeaveSceneFailedHandler = handler;
}

void SetSceneManagerServiceIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncSceneManagerCreateSceneFailedHandler) {
        AsyncSceneManagerCreateSceneFailedHandler = handler;
    }
    if (!AsyncSceneManagerDestroySceneFailedHandler) {
        AsyncSceneManagerDestroySceneFailedHandler = handler;
    }
    if (!AsyncSceneManagerEnterSceneFailedHandler) {
        AsyncSceneManagerEnterSceneFailedHandler = handler;
    }
    if (!AsyncSceneManagerLeaveSceneFailedHandler) {
        AsyncSceneManagerLeaveSceneFailedHandler = handler;
    }
}

void SetSceneManagerServiceCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitSceneManagerServiceGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<SceneManagerStubPtr>(nodeEntity, SceneManager::NewStub(channel));

}

}// namespace scene_manager
