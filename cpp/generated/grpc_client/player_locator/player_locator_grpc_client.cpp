#include "muduo/base/Logging.h"

#include "player_locator_grpc_client.h"
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

namespace playerlocator {
#pragma region PlayerLocatorSetLocation
namespace {
void QueueLocalPlayerLocatorSetLocationFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerLocatorSetLocationCall(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::PlayerLocation& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.SetLocation requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerLocatorSetLocationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerLocatorSetLocationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerLocatorSetLocationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the PlayerLocator.SetLocation response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerLocatorSetLocation(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::PlayerLocation& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartPlayerLocatorSetLocationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerLocatorSetLocationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.SetLocation requires a done callback";
    }
    if (message.GetDescriptor() != ::playerlocator::PlayerLocation::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerLocator.SetLocation: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerlocator::PlayerLocation*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerLocator.SetLocation: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerLocatorSetLocationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerLocatorGetLocation
namespace {
void QueueLocalPlayerLocatorGetLocationFailure(GrpcDone<::playerlocator::PlayerLocation> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerLocatorGetLocationCall(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::PlayerId& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::playerlocator::PlayerLocation> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.GetLocation requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerLocatorGetLocationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerLocatorGetLocationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerLocatorGetLocationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::playerlocator::PlayerLocation reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerLocator.GetLocation response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerLocatorGetLocation(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::PlayerId& request,
                                            GrpcDone<::playerlocator::PlayerLocation> done, const GrpcCallOptions& options) {
    StartPlayerLocatorGetLocationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerLocatorGetLocationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.GetLocation requires a done callback";
    }
    if (message.GetDescriptor() != ::playerlocator::PlayerId::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerLocator.GetLocation: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerlocator::PlayerId*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerLocator.GetLocation: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerLocatorGetLocationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::playerlocator::PlayerLocation* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerLocatorMarkOffline
namespace {
void QueueLocalPlayerLocatorMarkOfflineFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerLocatorMarkOfflineCall(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::MarkOfflineRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.MarkOffline requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerLocatorMarkOfflineGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerLocatorMarkOfflineFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerLocatorMarkOfflineFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the PlayerLocator.MarkOffline response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerLocatorMarkOffline(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::MarkOfflineRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartPlayerLocatorMarkOfflineCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerLocatorMarkOfflineErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.MarkOffline requires a done callback";
    }
    if (message.GetDescriptor() != ::playerlocator::MarkOfflineRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerLocator.MarkOffline: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerlocator::MarkOfflineRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerLocator.MarkOffline: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerLocatorMarkOfflineCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerLocatorSetSession
namespace {
void QueueLocalPlayerLocatorSetSessionFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerLocatorSetSessionCall(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::SetSessionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.SetSession requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerLocatorSetSessionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerLocatorSetSessionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerLocatorSetSessionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the PlayerLocator.SetSession response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerLocatorSetSession(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::SetSessionRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartPlayerLocatorSetSessionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerLocatorSetSessionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.SetSession requires a done callback";
    }
    if (message.GetDescriptor() != ::playerlocator::SetSessionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerLocator.SetSession: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerlocator::SetSessionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerLocator.SetSession: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerLocatorSetSessionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerLocatorGetSession
namespace {
void QueueLocalPlayerLocatorGetSessionFailure(GrpcDone<::playerlocator::GetSessionResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerLocatorGetSessionCall(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::GetSessionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::playerlocator::GetSessionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.GetSession requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerLocatorGetSessionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerLocatorGetSessionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerLocatorGetSessionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::playerlocator::GetSessionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerLocator.GetSession response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerLocatorGetSession(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::GetSessionRequest& request,
                                            GrpcDone<::playerlocator::GetSessionResponse> done, const GrpcCallOptions& options) {
    StartPlayerLocatorGetSessionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerLocatorGetSessionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.GetSession requires a done callback";
    }
    if (message.GetDescriptor() != ::playerlocator::GetSessionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerLocator.GetSession: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerlocator::GetSessionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerLocator.GetSession: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerLocatorGetSessionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::playerlocator::GetSessionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerLocatorSetDisconnecting
namespace {
void QueueLocalPlayerLocatorSetDisconnectingFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerLocatorSetDisconnectingCall(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::SetDisconnectingRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.SetDisconnecting requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerLocatorSetDisconnectingGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerLocatorSetDisconnectingFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerLocatorSetDisconnectingFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the PlayerLocator.SetDisconnecting response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerLocatorSetDisconnecting(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::SetDisconnectingRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartPlayerLocatorSetDisconnectingCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerLocatorSetDisconnectingErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.SetDisconnecting requires a done callback";
    }
    if (message.GetDescriptor() != ::playerlocator::SetDisconnectingRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerLocator.SetDisconnecting: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerlocator::SetDisconnectingRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerLocator.SetDisconnecting: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerLocatorSetDisconnectingCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerLocatorReconnect
namespace {
void QueueLocalPlayerLocatorReconnectFailure(GrpcDone<::playerlocator::ReconnectResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerLocatorReconnectCall(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::ReconnectRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::playerlocator::ReconnectResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.Reconnect requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerLocatorReconnectGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerLocatorReconnectFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerLocatorReconnectFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::playerlocator::ReconnectResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerLocator.Reconnect response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerLocatorReconnect(entt::registry& registry, entt::entity nodeEntity, const ::playerlocator::ReconnectRequest& request,
                                            GrpcDone<::playerlocator::ReconnectResponse> done, const GrpcCallOptions& options) {
    StartPlayerLocatorReconnectCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerLocatorReconnectErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerLocator.Reconnect requires a done callback";
    }
    if (message.GetDescriptor() != ::playerlocator::ReconnectRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerLocator.Reconnect: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerlocator::ReconnectRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerLocator.Reconnect: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerLocatorReconnectCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::playerlocator::ReconnectResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetPlayerLocatorCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace playerlocator
