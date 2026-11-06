#include "muduo/base/Logging.h"

#include "match_internal_grpc_client.h"
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

namespace match {
#pragma region MatchInternalStartActivityBattle
namespace {
void QueueLocalMatchInternalStartActivityBattleFailure(GrpcDone<::match::StartActivityBattleResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalStartActivityBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::match::StartActivityBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::StartActivityBattleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.StartActivityBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalStartActivityBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalStartActivityBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalStartActivityBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::StartActivityBattleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.StartActivityBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalStartActivityBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::StartActivityBattleRequest& request,
                                            GrpcDone<::match::StartActivityBattleResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalStartActivityBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalStartActivityBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.StartActivityBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::match::StartActivityBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.StartActivityBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::StartActivityBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.StartActivityBattle: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalStartActivityBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::StartActivityBattleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalCancelActivityBattle
namespace {
void QueueLocalMatchInternalCancelActivityBattleFailure(GrpcDone<::match::CancelActivityBattleResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalCancelActivityBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelActivityBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::CancelActivityBattleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.CancelActivityBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalCancelActivityBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalCancelActivityBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalCancelActivityBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::CancelActivityBattleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.CancelActivityBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalCancelActivityBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelActivityBattleRequest& request,
                                            GrpcDone<::match::CancelActivityBattleResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalCancelActivityBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalCancelActivityBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.CancelActivityBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::match::CancelActivityBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.CancelActivityBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::CancelActivityBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.CancelActivityBattle: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalCancelActivityBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::CancelActivityBattleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalCommitBattleOutcome
namespace {
void QueueLocalMatchInternalCommitBattleOutcomeFailure(GrpcDone<::match::CommitBattleOutcomeResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalCommitBattleOutcomeCall(entt::registry& registry, entt::entity nodeEntity, const ::match::CommitBattleOutcomeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::CommitBattleOutcomeResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.CommitBattleOutcome requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalCommitBattleOutcomeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalCommitBattleOutcomeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalCommitBattleOutcomeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::CommitBattleOutcomeResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.CommitBattleOutcome response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalCommitBattleOutcome(entt::registry& registry, entt::entity nodeEntity, const ::match::CommitBattleOutcomeRequest& request,
                                            GrpcDone<::match::CommitBattleOutcomeResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalCommitBattleOutcomeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalCommitBattleOutcomeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.CommitBattleOutcome requires a done callback";
    }
    if (message.GetDescriptor() != ::match::CommitBattleOutcomeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.CommitBattleOutcome: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::CommitBattleOutcomeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.CommitBattleOutcome: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalCommitBattleOutcomeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::CommitBattleOutcomeResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalSetSpectateFeatured
namespace {
void QueueLocalMatchInternalSetSpectateFeaturedFailure(GrpcDone<::match::SetSpectateFeaturedResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalSetSpectateFeaturedCall(entt::registry& registry, entt::entity nodeEntity, const ::match::SetSpectateFeaturedRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::SetSpectateFeaturedResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.SetSpectateFeatured requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalSetSpectateFeaturedGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalSetSpectateFeaturedFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalSetSpectateFeaturedFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::SetSpectateFeaturedResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.SetSpectateFeatured response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalSetSpectateFeatured(entt::registry& registry, entt::entity nodeEntity, const ::match::SetSpectateFeaturedRequest& request,
                                            GrpcDone<::match::SetSpectateFeaturedResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalSetSpectateFeaturedCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalSetSpectateFeaturedErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.SetSpectateFeatured requires a done callback";
    }
    if (message.GetDescriptor() != ::match::SetSpectateFeaturedRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.SetSpectateFeatured: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::SetSpectateFeaturedRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.SetSpectateFeatured: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalSetSpectateFeaturedCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::SetSpectateFeaturedResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalAcquireTrialOccupancy
namespace {
void QueueLocalMatchInternalAcquireTrialOccupancyFailure(GrpcDone<::match::AcquireTrialOccupancyResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalAcquireTrialOccupancyCall(entt::registry& registry, entt::entity nodeEntity, const ::match::AcquireTrialOccupancyRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::AcquireTrialOccupancyResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.AcquireTrialOccupancy requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalAcquireTrialOccupancyGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalAcquireTrialOccupancyFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalAcquireTrialOccupancyFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::AcquireTrialOccupancyResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.AcquireTrialOccupancy response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalAcquireTrialOccupancy(entt::registry& registry, entt::entity nodeEntity, const ::match::AcquireTrialOccupancyRequest& request,
                                            GrpcDone<::match::AcquireTrialOccupancyResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalAcquireTrialOccupancyCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalAcquireTrialOccupancyErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.AcquireTrialOccupancy requires a done callback";
    }
    if (message.GetDescriptor() != ::match::AcquireTrialOccupancyRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.AcquireTrialOccupancy: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::AcquireTrialOccupancyRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.AcquireTrialOccupancy: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalAcquireTrialOccupancyCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::AcquireTrialOccupancyResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalReleaseTrialOccupancy
namespace {
void QueueLocalMatchInternalReleaseTrialOccupancyFailure(GrpcDone<::match::ReleaseTrialOccupancyResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalReleaseTrialOccupancyCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ReleaseTrialOccupancyRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ReleaseTrialOccupancyResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.ReleaseTrialOccupancy requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalReleaseTrialOccupancyGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalReleaseTrialOccupancyFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalReleaseTrialOccupancyFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ReleaseTrialOccupancyResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.ReleaseTrialOccupancy response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalReleaseTrialOccupancy(entt::registry& registry, entt::entity nodeEntity, const ::match::ReleaseTrialOccupancyRequest& request,
                                            GrpcDone<::match::ReleaseTrialOccupancyResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalReleaseTrialOccupancyCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalReleaseTrialOccupancyErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.ReleaseTrialOccupancy requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ReleaseTrialOccupancyRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.ReleaseTrialOccupancy: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ReleaseTrialOccupancyRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.ReleaseTrialOccupancy: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalReleaseTrialOccupancyCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ReleaseTrialOccupancyResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalGetBattleCompletion
namespace {
void QueueLocalMatchInternalGetBattleCompletionFailure(GrpcDone<::match::GetBattleCompletionResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalGetBattleCompletionCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetBattleCompletionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetBattleCompletionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.GetBattleCompletion requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalGetBattleCompletionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalGetBattleCompletionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalGetBattleCompletionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetBattleCompletionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.GetBattleCompletion response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalGetBattleCompletion(entt::registry& registry, entt::entity nodeEntity, const ::match::GetBattleCompletionRequest& request,
                                            GrpcDone<::match::GetBattleCompletionResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalGetBattleCompletionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalGetBattleCompletionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.GetBattleCompletion requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetBattleCompletionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.GetBattleCompletion: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetBattleCompletionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.GetBattleCompletion: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalGetBattleCompletionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetBattleCompletionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalGetBattleTerminalDecision
namespace {
void QueueLocalMatchInternalGetBattleTerminalDecisionFailure(GrpcDone<::match::GetBattleTerminalDecisionResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalGetBattleTerminalDecisionCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetBattleTerminalDecisionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetBattleTerminalDecisionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.GetBattleTerminalDecision requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalGetBattleTerminalDecisionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalGetBattleTerminalDecisionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalGetBattleTerminalDecisionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetBattleTerminalDecisionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.GetBattleTerminalDecision response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalGetBattleTerminalDecision(entt::registry& registry, entt::entity nodeEntity, const ::match::GetBattleTerminalDecisionRequest& request,
                                            GrpcDone<::match::GetBattleTerminalDecisionResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalGetBattleTerminalDecisionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalGetBattleTerminalDecisionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.GetBattleTerminalDecision requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetBattleTerminalDecisionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.GetBattleTerminalDecision: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetBattleTerminalDecisionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.GetBattleTerminalDecision: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalGetBattleTerminalDecisionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetBattleTerminalDecisionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalListStrandedFormations
namespace {
void QueueLocalMatchInternalListStrandedFormationsFailure(GrpcDone<::match::ListStrandedFormationsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalListStrandedFormationsCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ListStrandedFormationsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ListStrandedFormationsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.ListStrandedFormations requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalListStrandedFormationsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalListStrandedFormationsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalListStrandedFormationsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ListStrandedFormationsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.ListStrandedFormations response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalListStrandedFormations(entt::registry& registry, entt::entity nodeEntity, const ::match::ListStrandedFormationsRequest& request,
                                            GrpcDone<::match::ListStrandedFormationsResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalListStrandedFormationsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalListStrandedFormationsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.ListStrandedFormations requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ListStrandedFormationsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.ListStrandedFormations: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ListStrandedFormationsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.ListStrandedFormations: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalListStrandedFormationsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ListStrandedFormationsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchInternalResolveStrandedFormation
namespace {
void QueueLocalMatchInternalResolveStrandedFormationFailure(GrpcDone<::match::ResolveStrandedFormationResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchInternalResolveStrandedFormationCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ResolveStrandedFormationRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ResolveStrandedFormationResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.ResolveStrandedFormation requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchInternalResolveStrandedFormationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchInternalResolveStrandedFormationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchInternalResolveStrandedFormationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ResolveStrandedFormationResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchInternal.ResolveStrandedFormation response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchInternalResolveStrandedFormation(entt::registry& registry, entt::entity nodeEntity, const ::match::ResolveStrandedFormationRequest& request,
                                            GrpcDone<::match::ResolveStrandedFormationResponse> done, const GrpcCallOptions& options) {
    StartMatchInternalResolveStrandedFormationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchInternalResolveStrandedFormationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchInternal.ResolveStrandedFormation requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ResolveStrandedFormationRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchInternal.ResolveStrandedFormation: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ResolveStrandedFormationRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchInternal.ResolveStrandedFormation: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchInternalResolveStrandedFormationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ResolveStrandedFormationResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetMatchInternalCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace match
