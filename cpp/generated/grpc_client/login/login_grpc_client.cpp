#include "muduo/base/Logging.h"

#include "login_grpc_client.h"
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

namespace loginpb {
#pragma region ClientPlayerLoginLogin
namespace {
void QueueLocalClientPlayerLoginLoginFailure(GrpcDone<::loginpb::LoginResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kClientPlayerLoginLoginGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerLoginLoginCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::LoginResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.Login requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerLoginLoginGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerLoginLoginFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerLoginLoginFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::LoginResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerLogin.Login response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginRequest& request,
                                            GrpcDone<::loginpb::LoginResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerLoginLoginCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerLoginLoginErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.Login requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::LoginRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerLogin.Login: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginLoginGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::LoginRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerLogin.Login: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginLoginGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerLoginLoginCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::LoginResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerLoginCreatePlayer
namespace {
void QueueLocalClientPlayerLoginCreatePlayerFailure(GrpcDone<::loginpb::CreatePlayerResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kClientPlayerLoginCreatePlayerGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerLoginCreatePlayerCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CreatePlayerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::CreatePlayerResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.CreatePlayer requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerLoginCreatePlayerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerLoginCreatePlayerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerLoginCreatePlayerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::CreatePlayerResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerLogin.CreatePlayer response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CreatePlayerRequest& request,
                                            GrpcDone<::loginpb::CreatePlayerResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerLoginCreatePlayerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerLoginCreatePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.CreatePlayer requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::CreatePlayerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerLogin.CreatePlayer: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginCreatePlayerGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::CreatePlayerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerLogin.CreatePlayer: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginCreatePlayerGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerLoginCreatePlayerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::CreatePlayerResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerLoginEnterGame
namespace {
void QueueLocalClientPlayerLoginEnterGameFailure(GrpcDone<::loginpb::EnterGameResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kClientPlayerLoginEnterGameGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerLoginEnterGameCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::EnterGameRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::EnterGameResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.EnterGame requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerLoginEnterGameGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerLoginEnterGameFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerLoginEnterGameFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::EnterGameResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerLogin.EnterGame response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::EnterGameRequest& request,
                                            GrpcDone<::loginpb::EnterGameResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerLoginEnterGameCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerLoginEnterGameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.EnterGame requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::EnterGameRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerLogin.EnterGame: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginEnterGameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::EnterGameRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerLogin.EnterGame: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginEnterGameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerLoginEnterGameCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::EnterGameResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerLoginLeaveGame
namespace {
void QueueLocalClientPlayerLoginLeaveGameFailure(GrpcDone<::loginpb::LoginEmptyResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kClientPlayerLoginLeaveGameGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerLoginLeaveGameCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LeaveGameRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::LoginEmptyResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.LeaveGame requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerLoginLeaveGameGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerLoginLeaveGameFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerLoginLeaveGameFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::LoginEmptyResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerLogin.LeaveGame response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LeaveGameRequest& request,
                                            GrpcDone<::loginpb::LoginEmptyResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerLoginLeaveGameCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerLoginLeaveGameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.LeaveGame requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::LeaveGameRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerLogin.LeaveGame: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginLeaveGameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::LeaveGameRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerLogin.LeaveGame: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginLeaveGameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerLoginLeaveGameCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::LoginEmptyResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerLoginDisconnect
namespace {
void QueueLocalClientPlayerLoginDisconnectFailure(GrpcDone<::loginpb::LoginEmptyResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kClientPlayerLoginDisconnectGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerLoginDisconnectCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginNodeDisconnectRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::LoginEmptyResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.Disconnect requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerLoginDisconnectGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerLoginDisconnectFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerLoginDisconnectFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::LoginEmptyResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerLogin.Disconnect response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginNodeDisconnectRequest& request,
                                            GrpcDone<::loginpb::LoginEmptyResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerLoginDisconnectCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerLoginDisconnectErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.Disconnect requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::LoginNodeDisconnectRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerLogin.Disconnect: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginDisconnectGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::LoginNodeDisconnectRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerLogin.Disconnect: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginDisconnectGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerLoginDisconnectCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::LoginEmptyResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerLoginRefreshToken
namespace {
void QueueLocalClientPlayerLoginRefreshTokenFailure(GrpcDone<::loginpb::RefreshTokenResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kClientPlayerLoginRefreshTokenGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerLoginRefreshTokenCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RefreshTokenRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::RefreshTokenResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.RefreshToken requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerLoginRefreshTokenGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerLoginRefreshTokenFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerLoginRefreshTokenFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::RefreshTokenResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerLogin.RefreshToken response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RefreshTokenRequest& request,
                                            GrpcDone<::loginpb::RefreshTokenResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerLoginRefreshTokenCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerLoginRefreshTokenErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.RefreshToken requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::RefreshTokenRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerLogin.RefreshToken: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginRefreshTokenGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::RefreshTokenRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerLogin.RefreshToken: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginRefreshTokenGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerLoginRefreshTokenCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::RefreshTokenResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerLoginRequestGateMigration
namespace {
void QueueLocalClientPlayerLoginRequestGateMigrationFailure(GrpcDone<::loginpb::GateMigrationResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kClientPlayerLoginRequestGateMigrationGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerLoginRequestGateMigrationCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::GateMigrationRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::GateMigrationResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.RequestGateMigration requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerLoginRequestGateMigrationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerLoginRequestGateMigrationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerLoginRequestGateMigrationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::GateMigrationResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerLogin.RequestGateMigration response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerLoginRequestGateMigration(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::GateMigrationRequest& request,
                                            GrpcDone<::loginpb::GateMigrationResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerLoginRequestGateMigrationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerLoginRequestGateMigrationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.RequestGateMigration requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::GateMigrationRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerLogin.RequestGateMigration: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginRequestGateMigrationGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::GateMigrationRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerLogin.RequestGateMigration: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginRequestGateMigrationGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerLoginRequestGateMigrationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::GateMigrationResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerLoginRenewSession
namespace {
void QueueLocalClientPlayerLoginRenewSessionFailure(GrpcDone<::loginpb::RenewSessionResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kClientPlayerLoginRenewSessionGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerLoginRenewSessionCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RenewSessionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::RenewSessionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.RenewSession requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerLoginRenewSessionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerLoginRenewSessionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerLoginRenewSessionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::RenewSessionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerLogin.RenewSession response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerLoginRenewSession(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RenewSessionRequest& request,
                                            GrpcDone<::loginpb::RenewSessionResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerLoginRenewSessionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerLoginRenewSessionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerLogin.RenewSession requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::RenewSessionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerLogin.RenewSession: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginRenewSessionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::RenewSessionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerLogin.RenewSession: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kClientPlayerLoginRenewSessionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerLoginRenewSessionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::RenewSessionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginPreGateAssignGate
namespace {
void QueueLocalLoginPreGateAssignGateFailure(GrpcDone<::loginpb::AssignGateResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginPreGateAssignGateGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginPreGateAssignGateCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AssignGateRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::AssignGateResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.AssignGate requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginPreGateAssignGateGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginPreGateAssignGateFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginPreGateAssignGateFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::AssignGateResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginPreGate.AssignGate response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AssignGateRequest& request,
                                            GrpcDone<::loginpb::AssignGateResponse> done, const GrpcCallOptions& options) {
    StartLoginPreGateAssignGateCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginPreGateAssignGateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.AssignGate requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::AssignGateRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginPreGate.AssignGate: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateAssignGateGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::AssignGateRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginPreGate.AssignGate: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateAssignGateGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginPreGateAssignGateCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::AssignGateResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginPreGateQueryQueueStatus
namespace {
void QueueLocalLoginPreGateQueryQueueStatusFailure(GrpcDone<::loginpb::QueryQueueStatusResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginPreGateQueryQueueStatusGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginPreGateQueryQueueStatusCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::QueryQueueStatusRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::QueryQueueStatusResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.QueryQueueStatus requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginPreGateQueryQueueStatusGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginPreGateQueryQueueStatusFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginPreGateQueryQueueStatusFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::QueryQueueStatusResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginPreGate.QueryQueueStatus response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::QueryQueueStatusRequest& request,
                                            GrpcDone<::loginpb::QueryQueueStatusResponse> done, const GrpcCallOptions& options) {
    StartLoginPreGateQueryQueueStatusCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginPreGateQueryQueueStatusErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.QueryQueueStatus requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::QueryQueueStatusRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginPreGate.QueryQueueStatus: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateQueryQueueStatusGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::QueryQueueStatusRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginPreGate.QueryQueueStatus: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateQueryQueueStatusGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginPreGateQueryQueueStatusCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::QueryQueueStatusResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginPreGateGetZoneRoleCount
namespace {
void QueueLocalLoginPreGateGetZoneRoleCountFailure(GrpcDone<::loginpb::ZoneRoleCountResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginPreGateGetZoneRoleCountGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginPreGateGetZoneRoleCountCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ZoneRoleCountRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::ZoneRoleCountResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.GetZoneRoleCount requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginPreGateGetZoneRoleCountGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginPreGateGetZoneRoleCountFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginPreGateGetZoneRoleCountFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::ZoneRoleCountResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginPreGate.GetZoneRoleCount response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginPreGateGetZoneRoleCount(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ZoneRoleCountRequest& request,
                                            GrpcDone<::loginpb::ZoneRoleCountResponse> done, const GrpcCallOptions& options) {
    StartLoginPreGateGetZoneRoleCountCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginPreGateGetZoneRoleCountErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.GetZoneRoleCount requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::ZoneRoleCountRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginPreGate.GetZoneRoleCount: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateGetZoneRoleCountGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::ZoneRoleCountRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginPreGate.GetZoneRoleCount: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateGetZoneRoleCountGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginPreGateGetZoneRoleCountCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::ZoneRoleCountResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginPreGateSubmitRealName
namespace {
void QueueLocalLoginPreGateSubmitRealNameFailure(GrpcDone<::loginpb::SubmitRealNameResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginPreGateSubmitRealNameGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginPreGateSubmitRealNameCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::SubmitRealNameRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::SubmitRealNameResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.SubmitRealName requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginPreGateSubmitRealNameGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginPreGateSubmitRealNameFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginPreGateSubmitRealNameFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::SubmitRealNameResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginPreGate.SubmitRealName response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginPreGateSubmitRealName(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::SubmitRealNameRequest& request,
                                            GrpcDone<::loginpb::SubmitRealNameResponse> done, const GrpcCallOptions& options) {
    StartLoginPreGateSubmitRealNameCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginPreGateSubmitRealNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.SubmitRealName requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::SubmitRealNameRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginPreGate.SubmitRealName: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateSubmitRealNameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::SubmitRealNameRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginPreGate.SubmitRealName: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateSubmitRealNameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginPreGateSubmitRealNameCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::SubmitRealNameResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginPreGateRequestAccountDeletion
namespace {
void QueueLocalLoginPreGateRequestAccountDeletionFailure(GrpcDone<::loginpb::AccountDeletionResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginPreGateRequestAccountDeletionGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginPreGateRequestAccountDeletionCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AccountDeletionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::AccountDeletionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.RequestAccountDeletion requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginPreGateRequestAccountDeletionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginPreGateRequestAccountDeletionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginPreGateRequestAccountDeletionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::AccountDeletionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginPreGate.RequestAccountDeletion response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginPreGateRequestAccountDeletion(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AccountDeletionRequest& request,
                                            GrpcDone<::loginpb::AccountDeletionResponse> done, const GrpcCallOptions& options) {
    StartLoginPreGateRequestAccountDeletionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginPreGateRequestAccountDeletionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.RequestAccountDeletion requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::AccountDeletionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginPreGate.RequestAccountDeletion: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateRequestAccountDeletionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::AccountDeletionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginPreGate.RequestAccountDeletion: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateRequestAccountDeletionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginPreGateRequestAccountDeletionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::AccountDeletionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginPreGateCancelAccountDeletion
namespace {
void QueueLocalLoginPreGateCancelAccountDeletionFailure(GrpcDone<::loginpb::AccountDeletionResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginPreGateCancelAccountDeletionGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginPreGateCancelAccountDeletionCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CancelAccountDeletionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::AccountDeletionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.CancelAccountDeletion requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginPreGateCancelAccountDeletionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginPreGateCancelAccountDeletionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginPreGateCancelAccountDeletionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::AccountDeletionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginPreGate.CancelAccountDeletion response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginPreGateCancelAccountDeletion(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CancelAccountDeletionRequest& request,
                                            GrpcDone<::loginpb::AccountDeletionResponse> done, const GrpcCallOptions& options) {
    StartLoginPreGateCancelAccountDeletionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginPreGateCancelAccountDeletionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginPreGate.CancelAccountDeletion requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::CancelAccountDeletionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginPreGate.CancelAccountDeletion: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateCancelAccountDeletionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::CancelAccountDeletionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginPreGate.CancelAccountDeletion: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginPreGateCancelAccountDeletionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginPreGateCancelAccountDeletionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::AccountDeletionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginAdminRemovePlayersFromAccounts
namespace {
void QueueLocalLoginAdminRemovePlayersFromAccountsFailure(GrpcDone<::loginpb::RemovePlayersFromAccountsResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginAdminRemovePlayersFromAccountsGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginAdminRemovePlayersFromAccountsCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::RemovePlayersFromAccountsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.RemovePlayersFromAccounts requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginAdminRemovePlayersFromAccountsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginAdminRemovePlayersFromAccountsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginAdminRemovePlayersFromAccountsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::RemovePlayersFromAccountsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginAdmin.RemovePlayersFromAccounts response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request,
                                            GrpcDone<::loginpb::RemovePlayersFromAccountsResponse> done, const GrpcCallOptions& options) {
    StartLoginAdminRemovePlayersFromAccountsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginAdminRemovePlayersFromAccountsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.RemovePlayersFromAccounts requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::RemovePlayersFromAccountsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginAdmin.RemovePlayersFromAccounts: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminRemovePlayersFromAccountsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::RemovePlayersFromAccountsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginAdmin.RemovePlayersFromAccounts: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminRemovePlayersFromAccountsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginAdminRemovePlayersFromAccountsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::RemovePlayersFromAccountsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginAdminScheduleAccountDeletion
namespace {
void QueueLocalLoginAdminScheduleAccountDeletionFailure(GrpcDone<::loginpb::AccountDeletionResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginAdminScheduleAccountDeletionGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginAdminScheduleAccountDeletionCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ScheduleAccountDeletionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::AccountDeletionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.ScheduleAccountDeletion requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginAdminScheduleAccountDeletionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginAdminScheduleAccountDeletionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginAdminScheduleAccountDeletionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::AccountDeletionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginAdmin.ScheduleAccountDeletion response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginAdminScheduleAccountDeletion(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ScheduleAccountDeletionRequest& request,
                                            GrpcDone<::loginpb::AccountDeletionResponse> done, const GrpcCallOptions& options) {
    StartLoginAdminScheduleAccountDeletionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginAdminScheduleAccountDeletionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.ScheduleAccountDeletion requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::ScheduleAccountDeletionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginAdmin.ScheduleAccountDeletion: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminScheduleAccountDeletionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::ScheduleAccountDeletionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginAdmin.ScheduleAccountDeletion: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminScheduleAccountDeletionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginAdminScheduleAccountDeletionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::AccountDeletionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginAdminResetAccountRealName
namespace {
void QueueLocalLoginAdminResetAccountRealNameFailure(GrpcDone<::loginpb::ResetAccountRealNameResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginAdminResetAccountRealNameGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginAdminResetAccountRealNameCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ResetAccountRealNameRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::ResetAccountRealNameResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.ResetAccountRealName requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginAdminResetAccountRealNameGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginAdminResetAccountRealNameFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginAdminResetAccountRealNameFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::ResetAccountRealNameResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginAdmin.ResetAccountRealName response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginAdminResetAccountRealName(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ResetAccountRealNameRequest& request,
                                            GrpcDone<::loginpb::ResetAccountRealNameResponse> done, const GrpcCallOptions& options) {
    StartLoginAdminResetAccountRealNameCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginAdminResetAccountRealNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.ResetAccountRealName requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::ResetAccountRealNameRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginAdmin.ResetAccountRealName: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminResetAccountRealNameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::ResetAccountRealNameRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginAdmin.ResetAccountRealName: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminResetAccountRealNameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginAdminResetAccountRealNameCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::ResetAccountRealNameResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetLoginCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace loginpb
