#include "muduo/base/Logging.h"

#include "scene_node_service_grpc_client.h"
#include <atomic>
#include <chrono>
#include <string>
#include <utility>
#include "core/network/grpc_client_runtime.h"
#include "core/utils/encode/base64.h"
#include "muduo_grpc/channel.h"

namespace {
// 每个 proto 文件独立的进程级 deadline;启动时按目标节点类型写入。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::milliseconds DefaultCallDeadline() {
    return std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}

GrpcCallOutcome MakeLocalFailureOutcome(uint32_t code, std::string message) {
    GrpcCallOutcome outcome;
    outcome.code = code;
    outcome.message = std::move(message);
    return outcome;
}
}

namespace scene_node {
#pragma region SceneNodeGrpcCreateScene
namespace {
void QueueLocalSceneNodeGrpcCreateSceneFailure(GrpcDone<::CreateSceneResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcCreateSceneCall(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::CreateSceneResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.CreateScene requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcCreateSceneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcCreateSceneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcCreateSceneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::CreateSceneResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.CreateScene response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request,
                                            GrpcDone<::CreateSceneResponse> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcCreateSceneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcCreateSceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.CreateScene requires a done callback";
    }
    if (message.GetDescriptor() != ::CreateSceneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.CreateScene: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::CreateSceneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.CreateScene: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcCreateSceneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::CreateSceneResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcDestroyScene
namespace {
void QueueLocalSceneNodeGrpcDestroySceneFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcDestroySceneCall(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.DestroyScene requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcDestroySceneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcDestroySceneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcDestroySceneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.DestroyScene response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcDestroySceneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcDestroySceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.DestroyScene requires a done callback";
    }
    if (message.GetDescriptor() != ::DestroySceneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.DestroyScene: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::DestroySceneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.DestroyScene: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcDestroySceneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcReleasePlayer
namespace {
void QueueLocalSceneNodeGrpcReleasePlayerFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcReleasePlayerCall(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.ReleasePlayer requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcReleasePlayerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcReleasePlayerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcReleasePlayerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.ReleasePlayer response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcReleasePlayerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcReleasePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.ReleasePlayer requires a done callback";
    }
    if (message.GetDescriptor() != ::scene_node::ReleasePlayerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.ReleasePlayer: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::scene_node::ReleasePlayerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.ReleasePlayer: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcReleasePlayerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcPrepareBattle
namespace {
void QueueLocalSceneNodeGrpcPrepareBattleFailure(GrpcDone<::PrepareBattleResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcPrepareBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::PrepareBattleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.PrepareBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcPrepareBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcPrepareBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcPrepareBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::PrepareBattleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.PrepareBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request,
                                            GrpcDone<::PrepareBattleResponse> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcPrepareBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcPrepareBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.PrepareBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::PrepareBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.PrepareBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::PrepareBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.PrepareBattle: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcPrepareBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::PrepareBattleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcCancelBattlePrepare
namespace {
void QueueLocalSceneNodeGrpcCancelBattlePrepareFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcCancelBattlePrepareCall(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.CancelBattlePrepare requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcCancelBattlePrepareGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcCancelBattlePrepareFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcCancelBattlePrepareFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.CancelBattlePrepare response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcCancelBattlePrepareCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcCancelBattlePrepareErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.CancelBattlePrepare requires a done callback";
    }
    if (message.GetDescriptor() != ::CancelBattlePrepareRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.CancelBattlePrepare: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::CancelBattlePrepareRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.CancelBattlePrepare: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcCancelBattlePrepareCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcSealAccountRoleTrade
namespace {
void QueueLocalSceneNodeGrpcSealAccountRoleTradeFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcSealAccountRoleTradeCall(entt::registry& registry, entt::entity nodeEntity, const ::SealAccountRoleTradeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.SealAccountRoleTrade requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcSealAccountRoleTradeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcSealAccountRoleTradeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcSealAccountRoleTradeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.SealAccountRoleTrade response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcSealAccountRoleTrade(entt::registry& registry, entt::entity nodeEntity, const ::SealAccountRoleTradeRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcSealAccountRoleTradeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcSealAccountRoleTradeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.SealAccountRoleTrade requires a done callback";
    }
    if (message.GetDescriptor() != ::SealAccountRoleTradeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.SealAccountRoleTrade: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::SealAccountRoleTradeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.SealAccountRoleTrade: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcSealAccountRoleTradeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcAssetDebit
namespace {
void QueueLocalSceneNodeGrpcAssetDebitFailure(GrpcDone<::AssetOpResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcAssetDebitCall(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::AssetOpResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.AssetDebit requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcAssetDebitGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcAssetDebitFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcAssetDebitFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::AssetOpResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.AssetDebit response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcAssetDebit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request,
                                            GrpcDone<::AssetOpResponse> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcAssetDebitCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcAssetDebitErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.AssetDebit requires a done callback";
    }
    if (message.GetDescriptor() != ::AssetOpRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetDebit: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::AssetOpRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetDebit: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcAssetDebitCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::AssetOpResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcAssetAbortDebit
namespace {
void QueueLocalSceneNodeGrpcAssetAbortDebitFailure(GrpcDone<::AssetOpResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcAssetAbortDebitCall(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::AssetOpResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.AssetAbortDebit requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcAssetAbortDebitGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcAssetAbortDebitFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcAssetAbortDebitFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::AssetOpResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.AssetAbortDebit response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcAssetAbortDebit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request,
                                            GrpcDone<::AssetOpResponse> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcAssetAbortDebitCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcAssetAbortDebitErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.AssetAbortDebit requires a done callback";
    }
    if (message.GetDescriptor() != ::AssetOpRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetAbortDebit: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::AssetOpRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetAbortDebit: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcAssetAbortDebitCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::AssetOpResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcAssetCredit
namespace {
void QueueLocalSceneNodeGrpcAssetCreditFailure(GrpcDone<::AssetOpResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcAssetCreditCall(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::AssetOpResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.AssetCredit requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcAssetCreditGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcAssetCreditFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcAssetCreditFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::AssetOpResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.AssetCredit response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcAssetCredit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request,
                                            GrpcDone<::AssetOpResponse> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcAssetCreditCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcAssetCreditErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.AssetCredit requires a done callback";
    }
    if (message.GetDescriptor() != ::AssetOpRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetCredit: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::AssetOpRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetCredit: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcAssetCreditCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::AssetOpResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneNodeGrpcAssetAckReceipt
namespace {
void QueueLocalSceneNodeGrpcAssetAckReceiptFailure(GrpcDone<::AssetOpResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneNodeGrpcAssetAckReceiptCall(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::AssetOpResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.AssetAckReceipt requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneNodeGrpcAssetAckReceiptGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneNodeGrpcAssetAckReceiptFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneNodeGrpcAssetAckReceiptFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
        return;
    }
    unaryRequest.metadata.reserve(metadata.size());
    for (const auto& [key, value] : metadata) {
        // 仅 SessionMD 的 Go 解码器依赖额外应用编码;ASCII 认证头必须保留原值。
        // 所有 -bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, key == kSessionBinMetaKey ? Base64Encode(value) : value);
    }
    unaryRequest.deadline = std::chrono::steady_clock::now() + (deadline.count() > 0 ? deadline : DefaultCallDeadline());
    channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {
        GrpcCallOutcome outcome;
        outcome.code = result.status.code_value();
        outcome.message = result.status.message();
        outcome.initialMetadata = std::move(result.initialMetadata);
        outcome.trailingMetadata = std::move(result.trailingMetadata);
        if (!outcome.ok()) {
            done(outcome, nullptr);
            return;
        }
        ::AssetOpResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneNodeGrpc.AssetAckReceipt response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneNodeGrpcAssetAckReceipt(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request,
                                            GrpcDone<::AssetOpResponse> done, const GrpcCallOptions& options) {
    StartSceneNodeGrpcAssetAckReceiptCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneNodeGrpcAssetAckReceiptErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneNodeGrpc.AssetAckReceipt requires a done callback";
    }
    if (message.GetDescriptor() != ::AssetOpRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetAckReceipt: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::AssetOpRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneNodeGrpc.AssetAckReceipt: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneNodeGrpcAssetAckReceiptCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::AssetOpResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetSceneNodeServiceCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace scene_node
