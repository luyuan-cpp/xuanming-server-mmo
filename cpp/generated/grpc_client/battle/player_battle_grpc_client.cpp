#include "muduo/base/Logging.h"

#include "player_battle_grpc_client.h"
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

#pragma region BattleClientPlayerSubmitBattleAction
namespace {
void QueueLocalBattleClientPlayerSubmitBattleActionFailure(GrpcDone<::SubmitBattleActionResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerSubmitBattleActionCall(entt::registry& registry, entt::entity nodeEntity, const ::SubmitBattleActionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::SubmitBattleActionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.SubmitBattleAction requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerSubmitBattleActionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerSubmitBattleActionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerSubmitBattleActionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::SubmitBattleActionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleClientPlayer.SubmitBattleAction response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, const ::SubmitBattleActionRequest& request,
                                            GrpcDone<::SubmitBattleActionResponse> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerSubmitBattleActionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerSubmitBattleActionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.SubmitBattleAction requires a done callback";
    }
    if (message.GetDescriptor() != ::SubmitBattleActionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.SubmitBattleAction: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::SubmitBattleActionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.SubmitBattleAction: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerSubmitBattleActionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::SubmitBattleActionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerGetBattleState
namespace {
void QueueLocalBattleClientPlayerGetBattleStateFailure(GrpcDone<::BattleStateS2C> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerGetBattleStateCall(entt::registry& registry, entt::entity nodeEntity, const ::GetBattleStateRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::BattleStateS2C> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.GetBattleState requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerGetBattleStateGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerGetBattleStateFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerGetBattleStateFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::BattleStateS2C reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleClientPlayer.GetBattleState response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, const ::GetBattleStateRequest& request,
                                            GrpcDone<::BattleStateS2C> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerGetBattleStateCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerGetBattleStateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.GetBattleState requires a done callback";
    }
    if (message.GetDescriptor() != ::GetBattleStateRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.GetBattleState: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::GetBattleStateRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.GetBattleState: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerGetBattleStateCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::BattleStateS2C* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleStart
namespace {
void QueueLocalBattleClientPlayerNotifyBattleStartFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerNotifyBattleStartCall(entt::registry& registry, entt::entity nodeEntity, const ::BattleStartS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyBattleStart requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerNotifyBattleStartGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerNotifyBattleStartFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerNotifyBattleStartFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the BattleClientPlayer.NotifyBattleStart response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, const ::BattleStartS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerNotifyBattleStartCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerNotifyBattleStartErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyBattleStart requires a done callback";
    }
    if (message.GetDescriptor() != ::BattleStartS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyBattleStart: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::BattleStartS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyBattleStart: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerNotifyBattleStartCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerNotifyTurnResult
namespace {
void QueueLocalBattleClientPlayerNotifyTurnResultFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerNotifyTurnResultCall(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyTurnResult requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerNotifyTurnResultGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerNotifyTurnResultFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerNotifyTurnResultFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the BattleClientPlayer.NotifyTurnResult response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerNotifyTurnResultCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerNotifyTurnResultErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyTurnResult requires a done callback";
    }
    if (message.GetDescriptor() != ::TurnResultS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyTurnResult: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::TurnResultS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyTurnResult: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerNotifyTurnResultCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleEnd
namespace {
void QueueLocalBattleClientPlayerNotifyBattleEndFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerNotifyBattleEndCall(entt::registry& registry, entt::entity nodeEntity, const ::BattleEndS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyBattleEnd requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerNotifyBattleEndGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerNotifyBattleEndFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerNotifyBattleEndFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the BattleClientPlayer.NotifyBattleEnd response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, const ::BattleEndS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerNotifyBattleEndCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerNotifyBattleEndErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyBattleEnd requires a done callback";
    }
    if (message.GetDescriptor() != ::BattleEndS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyBattleEnd: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::BattleEndS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyBattleEnd: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerNotifyBattleEndCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleReconnect
namespace {
void QueueLocalBattleClientPlayerNotifyBattleReconnectFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerNotifyBattleReconnectCall(entt::registry& registry, entt::entity nodeEntity, const ::BattleReconnectS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyBattleReconnect requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerNotifyBattleReconnectGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerNotifyBattleReconnectFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerNotifyBattleReconnectFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the BattleClientPlayer.NotifyBattleReconnect response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, const ::BattleReconnectS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerNotifyBattleReconnectCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerNotifyBattleReconnectErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyBattleReconnect requires a done callback";
    }
    if (message.GetDescriptor() != ::BattleReconnectS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyBattleReconnect: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::BattleReconnectS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyBattleReconnect: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerNotifyBattleReconnectCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerStopWatchBattle
namespace {
void QueueLocalBattleClientPlayerStopWatchBattleFailure(GrpcDone<::StopWatchBattleResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerStopWatchBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::StopWatchBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::StopWatchBattleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.StopWatchBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerStopWatchBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerStopWatchBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerStopWatchBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::StopWatchBattleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleClientPlayer.StopWatchBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::StopWatchBattleRequest& request,
                                            GrpcDone<::StopWatchBattleResponse> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerStopWatchBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerStopWatchBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.StopWatchBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::StopWatchBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.StopWatchBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::StopWatchBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.StopWatchBattle: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerStopWatchBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::StopWatchBattleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerSetAutoBattle
namespace {
void QueueLocalBattleClientPlayerSetAutoBattleFailure(GrpcDone<::SetAutoBattleResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerSetAutoBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::SetAutoBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::SetAutoBattleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.SetAutoBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerSetAutoBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerSetAutoBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerSetAutoBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::SetAutoBattleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleClientPlayer.SetAutoBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, const ::SetAutoBattleRequest& request,
                                            GrpcDone<::SetAutoBattleResponse> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerSetAutoBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerSetAutoBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.SetAutoBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::SetAutoBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.SetAutoBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::SetAutoBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.SetAutoBattle: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerSetAutoBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::SetAutoBattleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateState
namespace {
void QueueLocalBattleClientPlayerNotifySpectateStateFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerNotifySpectateStateCall(entt::registry& registry, entt::entity nodeEntity, const ::SpectateStateS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifySpectateState requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerNotifySpectateStateGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerNotifySpectateStateFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerNotifySpectateStateFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the BattleClientPlayer.NotifySpectateState response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, const ::SpectateStateS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerNotifySpectateStateCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerNotifySpectateStateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifySpectateState requires a done callback";
    }
    if (message.GetDescriptor() != ::SpectateStateS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.NotifySpectateState: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::SpectateStateS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.NotifySpectateState: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerNotifySpectateStateCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateTurnResult
namespace {
void QueueLocalBattleClientPlayerNotifySpectateTurnResultFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerNotifySpectateTurnResultCall(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifySpectateTurnResult requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerNotifySpectateTurnResultGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerNotifySpectateTurnResultFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerNotifySpectateTurnResultFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the BattleClientPlayer.NotifySpectateTurnResult response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerNotifySpectateTurnResultCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerNotifySpectateTurnResultErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifySpectateTurnResult requires a done callback";
    }
    if (message.GetDescriptor() != ::TurnResultS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.NotifySpectateTurnResult: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::TurnResultS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.NotifySpectateTurnResult: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerNotifySpectateTurnResultCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateEnd
namespace {
void QueueLocalBattleClientPlayerNotifySpectateEndFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerNotifySpectateEndCall(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifySpectateEnd requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerNotifySpectateEndGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerNotifySpectateEndFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerNotifySpectateEndFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the BattleClientPlayer.NotifySpectateEnd response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerNotifySpectateEndCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerNotifySpectateEndErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifySpectateEnd requires a done callback";
    }
    if (message.GetDescriptor() != ::SpectateEndS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.NotifySpectateEnd: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::SpectateEndS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.NotifySpectateEnd: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerNotifySpectateEndCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleAssigned
namespace {
void QueueLocalBattleClientPlayerNotifyBattleAssignedFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleClientPlayerNotifyBattleAssignedCall(entt::registry& registry, entt::entity nodeEntity, const ::BattleAssignedS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyBattleAssigned requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleClientPlayerNotifyBattleAssignedGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleClientPlayerNotifyBattleAssignedFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleClientPlayerNotifyBattleAssignedFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the BattleClientPlayer.NotifyBattleAssigned response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, const ::BattleAssignedS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleClientPlayerNotifyBattleAssignedCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleClientPlayerNotifyBattleAssignedErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleClientPlayer.NotifyBattleAssigned requires a done callback";
    }
    if (message.GetDescriptor() != ::BattleAssignedS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyBattleAssigned: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::BattleAssignedS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleClientPlayer.NotifyBattleAssigned: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleClientPlayerNotifyBattleAssignedCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion

void SetPlayerBattleCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}
