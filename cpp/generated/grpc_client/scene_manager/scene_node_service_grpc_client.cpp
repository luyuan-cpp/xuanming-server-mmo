#include "muduo/base/Logging.h"

#include "scene_node_service_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetSceneNodeServiceCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
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
AsyncSceneNodeGrpcCreateSceneFailedHandlerFunctionType AsyncSceneNodeGrpcCreateSceneFailedHandler;

void AsyncCompleteGrpcSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcCreateSceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcCreateSceneHandler) {
            AsyncSceneNodeGrpcCreateSceneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneNodeGrpc.CreateScene reply dropped: AsyncSceneNodeGrpcCreateSceneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneNodeGrpcCreateSceneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneNodeGrpc.CreateScene", call->context, call->status, call->sentMetadata};
        AsyncSceneNodeGrpcCreateSceneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneNodeGrpc.CreateScene failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneNodeGrpcCreateScenePool.destroy(call);
}

void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request) {

    SendSceneNodeGrpcCreateScene(registry, nodeEntity, request, {}, {});

}

void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcCreateScenePool.construct());
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
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncCreateScene(&call->context, call->request,
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
AsyncSceneNodeGrpcDestroySceneFailedHandlerFunctionType AsyncSceneNodeGrpcDestroySceneFailedHandler;

void AsyncCompleteGrpcSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcDestroySceneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcDestroySceneHandler) {
            AsyncSceneNodeGrpcDestroySceneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneNodeGrpc.DestroyScene reply dropped: AsyncSceneNodeGrpcDestroySceneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneNodeGrpcDestroySceneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneNodeGrpc.DestroyScene", call->context, call->status, call->sentMetadata};
        AsyncSceneNodeGrpcDestroySceneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneNodeGrpc.DestroyScene failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneNodeGrpcDestroyScenePool.destroy(call);
}

void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request) {

    SendSceneNodeGrpcDestroyScene(registry, nodeEntity, request, {}, {});

}

void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcDestroyScenePool.construct());
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
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyScene(&call->context, call->request,
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
AsyncSceneNodeGrpcReleasePlayerFailedHandlerFunctionType AsyncSceneNodeGrpcReleasePlayerFailedHandler;

void AsyncCompleteGrpcSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcReleasePlayerGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcReleasePlayerHandler) {
            AsyncSceneNodeGrpcReleasePlayerHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneNodeGrpc.ReleasePlayer reply dropped: AsyncSceneNodeGrpcReleasePlayerHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneNodeGrpcReleasePlayerFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneNodeGrpc.ReleasePlayer", call->context, call->status, call->sentMetadata};
        AsyncSceneNodeGrpcReleasePlayerFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneNodeGrpc.ReleasePlayer failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneNodeGrpcReleasePlayerPool.destroy(call);
}

void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request) {

    SendSceneNodeGrpcReleasePlayer(registry, nodeEntity, request, {}, {});

}

void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcReleasePlayerPool.construct());
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
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncReleasePlayer(&call->context, call->request,
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
AsyncSceneNodeGrpcPrepareBattleFailedHandlerFunctionType AsyncSceneNodeGrpcPrepareBattleFailedHandler;

void AsyncCompleteGrpcSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcPrepareBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcPrepareBattleHandler) {
            AsyncSceneNodeGrpcPrepareBattleHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneNodeGrpc.PrepareBattle reply dropped: AsyncSceneNodeGrpcPrepareBattleHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneNodeGrpcPrepareBattleFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneNodeGrpc.PrepareBattle", call->context, call->status, call->sentMetadata};
        AsyncSceneNodeGrpcPrepareBattleFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneNodeGrpc.PrepareBattle failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneNodeGrpcPrepareBattlePool.destroy(call);
}

void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request) {

    SendSceneNodeGrpcPrepareBattle(registry, nodeEntity, request, {}, {});

}

void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcPrepareBattlePool.construct());
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
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncPrepareBattle(&call->context, call->request,
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
AsyncSceneNodeGrpcCancelBattlePrepareFailedHandlerFunctionType AsyncSceneNodeGrpcCancelBattlePrepareFailedHandler;

void AsyncCompleteGrpcSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcCancelBattlePrepareGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcCancelBattlePrepareHandler) {
            AsyncSceneNodeGrpcCancelBattlePrepareHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneNodeGrpc.CancelBattlePrepare reply dropped: AsyncSceneNodeGrpcCancelBattlePrepareHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneNodeGrpcCancelBattlePrepareFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneNodeGrpc.CancelBattlePrepare", call->context, call->status, call->sentMetadata};
        AsyncSceneNodeGrpcCancelBattlePrepareFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneNodeGrpc.CancelBattlePrepare failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneNodeGrpcCancelBattlePreparePool.destroy(call);
}

void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request) {

    SendSceneNodeGrpcCancelBattlePrepare(registry, nodeEntity, request, {}, {});

}

void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcCancelBattlePreparePool.construct());
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
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncCancelBattlePrepare(&call->context, call->request,
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
#pragma region SceneNodeGrpcAssetDebit
boost::object_pool<AsyncSceneNodeGrpcAssetDebitGrpcClient> SceneNodeGrpcAssetDebitPool;
using AsyncSceneNodeGrpcAssetDebitHandlerFunctionType =
    std::function<void(const ClientContext&, const ::AssetOpResponse&)>;
AsyncSceneNodeGrpcAssetDebitHandlerFunctionType AsyncSceneNodeGrpcAssetDebitHandler;
AsyncSceneNodeGrpcAssetDebitFailedHandlerFunctionType AsyncSceneNodeGrpcAssetDebitFailedHandler;

void AsyncCompleteGrpcSceneNodeGrpcAssetDebit(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcAssetDebitGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcAssetDebitHandler) {
            AsyncSceneNodeGrpcAssetDebitHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneNodeGrpc.AssetDebit reply dropped: AsyncSceneNodeGrpcAssetDebitHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneNodeGrpcAssetDebitFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneNodeGrpc.AssetDebit", call->context, call->status, call->sentMetadata};
        AsyncSceneNodeGrpcAssetDebitFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetDebit failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneNodeGrpcAssetDebitPool.destroy(call);
}

void SendSceneNodeGrpcAssetDebit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request) {

    SendSceneNodeGrpcAssetDebit(registry, nodeEntity, request, {}, {});

}

void SendSceneNodeGrpcAssetDebit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcAssetDebitPool.construct());
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
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncAssetDebit(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcAssetDebitMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcAssetDebit(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::AssetOpRequest& derived = static_cast<const ::AssetOpRequest&>(message);
    SendSceneNodeGrpcAssetDebit(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneNodeGrpcAssetAbortDebit
boost::object_pool<AsyncSceneNodeGrpcAssetAbortDebitGrpcClient> SceneNodeGrpcAssetAbortDebitPool;
using AsyncSceneNodeGrpcAssetAbortDebitHandlerFunctionType =
    std::function<void(const ClientContext&, const ::AssetOpResponse&)>;
AsyncSceneNodeGrpcAssetAbortDebitHandlerFunctionType AsyncSceneNodeGrpcAssetAbortDebitHandler;
AsyncSceneNodeGrpcAssetAbortDebitFailedHandlerFunctionType AsyncSceneNodeGrpcAssetAbortDebitFailedHandler;

void AsyncCompleteGrpcSceneNodeGrpcAssetAbortDebit(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcAssetAbortDebitGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcAssetAbortDebitHandler) {
            AsyncSceneNodeGrpcAssetAbortDebitHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneNodeGrpc.AssetAbortDebit reply dropped: AsyncSceneNodeGrpcAssetAbortDebitHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneNodeGrpcAssetAbortDebitFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneNodeGrpc.AssetAbortDebit", call->context, call->status, call->sentMetadata};
        AsyncSceneNodeGrpcAssetAbortDebitFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetAbortDebit failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneNodeGrpcAssetAbortDebitPool.destroy(call);
}

void SendSceneNodeGrpcAssetAbortDebit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request) {

    SendSceneNodeGrpcAssetAbortDebit(registry, nodeEntity, request, {}, {});

}

void SendSceneNodeGrpcAssetAbortDebit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcAssetAbortDebitPool.construct());
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
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncAssetAbortDebit(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcAssetAbortDebitMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcAssetAbortDebit(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::AssetOpRequest& derived = static_cast<const ::AssetOpRequest&>(message);
    SendSceneNodeGrpcAssetAbortDebit(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region SceneNodeGrpcAssetCredit
boost::object_pool<AsyncSceneNodeGrpcAssetCreditGrpcClient> SceneNodeGrpcAssetCreditPool;
using AsyncSceneNodeGrpcAssetCreditHandlerFunctionType =
    std::function<void(const ClientContext&, const ::AssetOpResponse&)>;
AsyncSceneNodeGrpcAssetCreditHandlerFunctionType AsyncSceneNodeGrpcAssetCreditHandler;
AsyncSceneNodeGrpcAssetCreditFailedHandlerFunctionType AsyncSceneNodeGrpcAssetCreditFailedHandler;

void AsyncCompleteGrpcSceneNodeGrpcAssetCredit(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncSceneNodeGrpcAssetCreditGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncSceneNodeGrpcAssetCreditHandler) {
            AsyncSceneNodeGrpcAssetCreditHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC SceneNodeGrpc.AssetCredit reply dropped: AsyncSceneNodeGrpcAssetCreditHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncSceneNodeGrpcAssetCreditFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "SceneNodeGrpc.AssetCredit", call->context, call->status, call->sentMetadata};
        AsyncSceneNodeGrpcAssetCreditFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetCredit failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	SceneNodeGrpcAssetCreditPool.destroy(call);
}

void SendSceneNodeGrpcAssetCredit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request) {

    SendSceneNodeGrpcAssetCredit(registry, nodeEntity, request, {}, {});

}

void SendSceneNodeGrpcAssetCredit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(SceneNodeGrpcAssetCreditPool.construct());
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
        .get<SceneNodeGrpcStubPtr>(nodeEntity)
        ->PrepareAsyncAssetCredit(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(SceneNodeGrpcAssetCreditMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendSceneNodeGrpcAssetCredit(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::AssetOpRequest& derived = static_cast<const ::AssetOpRequest&>(message);
    SendSceneNodeGrpcAssetCredit(registry, nodeEntity, derived, metaKeys, metaValues);
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
        case SceneNodeGrpcAssetDebitMessageId:
            AsyncCompleteGrpcSceneNodeGrpcAssetDebit(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneNodeGrpcAssetAbortDebitMessageId:
            AsyncCompleteGrpcSceneNodeGrpcAssetAbortDebit(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case SceneNodeGrpcAssetCreditMessageId:
            AsyncCompleteGrpcSceneNodeGrpcAssetCredit(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
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
    AsyncSceneNodeGrpcAssetDebitHandler = handler;
    AsyncSceneNodeGrpcAssetAbortDebitHandler = handler;
    AsyncSceneNodeGrpcAssetCreditHandler = handler;
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
    if (!AsyncSceneNodeGrpcAssetDebitHandler) {
        AsyncSceneNodeGrpcAssetDebitHandler = handler;
    }
    if (!AsyncSceneNodeGrpcAssetAbortDebitHandler) {
        AsyncSceneNodeGrpcAssetAbortDebitHandler = handler;
    }
    if (!AsyncSceneNodeGrpcAssetCreditHandler) {
        AsyncSceneNodeGrpcAssetCreditHandler = handler;
    }
}

void SetSceneNodeServiceFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncSceneNodeGrpcCreateSceneFailedHandler = handler;
    AsyncSceneNodeGrpcDestroySceneFailedHandler = handler;
    AsyncSceneNodeGrpcReleasePlayerFailedHandler = handler;
    AsyncSceneNodeGrpcPrepareBattleFailedHandler = handler;
    AsyncSceneNodeGrpcCancelBattlePrepareFailedHandler = handler;
    AsyncSceneNodeGrpcAssetDebitFailedHandler = handler;
    AsyncSceneNodeGrpcAssetAbortDebitFailedHandler = handler;
    AsyncSceneNodeGrpcAssetCreditFailedHandler = handler;
}

void SetSceneNodeServiceIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncSceneNodeGrpcCreateSceneFailedHandler) {
        AsyncSceneNodeGrpcCreateSceneFailedHandler = handler;
    }
    if (!AsyncSceneNodeGrpcDestroySceneFailedHandler) {
        AsyncSceneNodeGrpcDestroySceneFailedHandler = handler;
    }
    if (!AsyncSceneNodeGrpcReleasePlayerFailedHandler) {
        AsyncSceneNodeGrpcReleasePlayerFailedHandler = handler;
    }
    if (!AsyncSceneNodeGrpcPrepareBattleFailedHandler) {
        AsyncSceneNodeGrpcPrepareBattleFailedHandler = handler;
    }
    if (!AsyncSceneNodeGrpcCancelBattlePrepareFailedHandler) {
        AsyncSceneNodeGrpcCancelBattlePrepareFailedHandler = handler;
    }
    if (!AsyncSceneNodeGrpcAssetDebitFailedHandler) {
        AsyncSceneNodeGrpcAssetDebitFailedHandler = handler;
    }
    if (!AsyncSceneNodeGrpcAssetAbortDebitFailedHandler) {
        AsyncSceneNodeGrpcAssetAbortDebitFailedHandler = handler;
    }
    if (!AsyncSceneNodeGrpcAssetCreditFailedHandler) {
        AsyncSceneNodeGrpcAssetCreditFailedHandler = handler;
    }
}

void SetSceneNodeServiceCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitSceneNodeServiceGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<SceneNodeGrpcStubPtr>(nodeEntity, SceneNodeGrpc::NewStub(channel));

}

}// namespace scene_node
