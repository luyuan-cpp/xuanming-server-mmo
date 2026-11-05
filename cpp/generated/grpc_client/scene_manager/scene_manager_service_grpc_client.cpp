#include "muduo/base/Logging.h"

#include "scene_manager_service_grpc_client.h"
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

namespace scene_manager {
#pragma region SceneManagerCreateScene
namespace {
void QueueLocalSceneManagerCreateSceneFailure(GrpcDone<::scene_manager::CreateSceneResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneManagerCreateSceneCall(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::CreateSceneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::scene_manager::CreateSceneResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneManager.CreateScene requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneManagerCreateSceneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneManagerCreateSceneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneManagerCreateSceneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::scene_manager::CreateSceneResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneManager.CreateScene response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::CreateSceneRequest& request,
                                            GrpcDone<::scene_manager::CreateSceneResponse> done, const GrpcCallOptions& options) {
    StartSceneManagerCreateSceneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneManagerCreateSceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneManager.CreateScene requires a done callback";
    }
    if (message.GetDescriptor() != ::scene_manager::CreateSceneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneManager.CreateScene: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::scene_manager::CreateSceneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneManager.CreateScene: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneManagerCreateSceneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::scene_manager::CreateSceneResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneManagerDestroyScene
namespace {
void QueueLocalSceneManagerDestroySceneFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneManagerDestroySceneCall(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::DestroySceneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneManager.DestroyScene requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneManagerDestroySceneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneManagerDestroySceneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneManagerDestroySceneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the SceneManager.DestroyScene response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::DestroySceneRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartSceneManagerDestroySceneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneManagerDestroySceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneManager.DestroyScene requires a done callback";
    }
    if (message.GetDescriptor() != ::scene_manager::DestroySceneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneManager.DestroyScene: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::scene_manager::DestroySceneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneManager.DestroyScene: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneManagerDestroySceneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneManagerEnterScene
namespace {
void QueueLocalSceneManagerEnterSceneFailure(GrpcDone<::scene_manager::EnterSceneResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneManagerEnterSceneCall(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::scene_manager::EnterSceneResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneManager.EnterScene requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneManagerEnterSceneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneManagerEnterSceneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneManagerEnterSceneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::scene_manager::EnterSceneResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the SceneManager.EnterScene response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request,
                                            GrpcDone<::scene_manager::EnterSceneResponse> done, const GrpcCallOptions& options) {
    StartSceneManagerEnterSceneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneManagerEnterSceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneManager.EnterScene requires a done callback";
    }
    if (message.GetDescriptor() != ::scene_manager::EnterSceneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneManager.EnterScene: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::scene_manager::EnterSceneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneManager.EnterScene: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneManagerEnterSceneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::scene_manager::EnterSceneResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region SceneManagerLeaveScene
namespace {
void QueueLocalSceneManagerLeaveSceneFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartSceneManagerLeaveSceneCall(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::LeaveSceneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC SceneManager.LeaveScene requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kSceneManagerLeaveSceneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalSceneManagerLeaveSceneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalSceneManagerLeaveSceneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the SceneManager.LeaveScene response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::LeaveSceneRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartSceneManagerLeaveSceneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendSceneManagerLeaveSceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC SceneManager.LeaveScene requires a done callback";
    }
    if (message.GetDescriptor() != ::scene_manager::LeaveSceneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC SceneManager.LeaveScene: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::scene_manager::LeaveSceneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC SceneManager.LeaveScene: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartSceneManagerLeaveSceneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion

void SetSceneManagerServiceCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace scene_manager
