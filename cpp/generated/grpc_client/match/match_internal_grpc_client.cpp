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
        // Go 侧依赖这层应用编码;-bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, Base64Encode(value));
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
        // Go 侧依赖这层应用编码;-bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, Base64Encode(value));
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
        // Go 侧依赖这层应用编码;-bin 的线上编码另由传输层负责。
        unaryRequest.metadata.emplace_back(key, Base64Encode(value));
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

void SetMatchInternalCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace match
