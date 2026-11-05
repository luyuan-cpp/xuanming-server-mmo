#include "muduo/base/Logging.h"

#include "rank_admin_grpc_client.h"
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

namespace rankpb {
#pragma region RankAdminOpenRankGeneration
namespace {
void QueueLocalRankAdminOpenRankGenerationFailure(GrpcDone<::rankpb::OpenRankGenerationResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartRankAdminOpenRankGenerationCall(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::OpenRankGenerationRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::rankpb::OpenRankGenerationResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.OpenRankGeneration requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kRankAdminOpenRankGenerationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalRankAdminOpenRankGenerationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalRankAdminOpenRankGenerationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::rankpb::OpenRankGenerationResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the RankAdmin.OpenRankGeneration response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendRankAdminOpenRankGeneration(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::OpenRankGenerationRequest& request,
                                            GrpcDone<::rankpb::OpenRankGenerationResponse> done, const GrpcCallOptions& options) {
    StartRankAdminOpenRankGenerationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendRankAdminOpenRankGenerationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.OpenRankGeneration requires a done callback";
    }
    if (message.GetDescriptor() != ::rankpb::OpenRankGenerationRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC RankAdmin.OpenRankGeneration: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::rankpb::OpenRankGenerationRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC RankAdmin.OpenRankGeneration: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartRankAdminOpenRankGenerationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::rankpb::OpenRankGenerationResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region RankAdminSubmitRankScores
namespace {
void QueueLocalRankAdminSubmitRankScoresFailure(GrpcDone<::rankpb::SubmitRankScoresResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartRankAdminSubmitRankScoresCall(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::SubmitRankScoresRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::rankpb::SubmitRankScoresResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.SubmitRankScores requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kRankAdminSubmitRankScoresGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalRankAdminSubmitRankScoresFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalRankAdminSubmitRankScoresFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::rankpb::SubmitRankScoresResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the RankAdmin.SubmitRankScores response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendRankAdminSubmitRankScores(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::SubmitRankScoresRequest& request,
                                            GrpcDone<::rankpb::SubmitRankScoresResponse> done, const GrpcCallOptions& options) {
    StartRankAdminSubmitRankScoresCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendRankAdminSubmitRankScoresErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.SubmitRankScores requires a done callback";
    }
    if (message.GetDescriptor() != ::rankpb::SubmitRankScoresRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC RankAdmin.SubmitRankScores: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::rankpb::SubmitRankScoresRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC RankAdmin.SubmitRankScores: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartRankAdminSubmitRankScoresCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::rankpb::SubmitRankScoresResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region RankAdminFreezeRankGeneration
namespace {
void QueueLocalRankAdminFreezeRankGenerationFailure(GrpcDone<::rankpb::FreezeRankGenerationResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartRankAdminFreezeRankGenerationCall(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::FreezeRankGenerationRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::rankpb::FreezeRankGenerationResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.FreezeRankGeneration requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kRankAdminFreezeRankGenerationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalRankAdminFreezeRankGenerationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalRankAdminFreezeRankGenerationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::rankpb::FreezeRankGenerationResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the RankAdmin.FreezeRankGeneration response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendRankAdminFreezeRankGeneration(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::FreezeRankGenerationRequest& request,
                                            GrpcDone<::rankpb::FreezeRankGenerationResponse> done, const GrpcCallOptions& options) {
    StartRankAdminFreezeRankGenerationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendRankAdminFreezeRankGenerationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.FreezeRankGeneration requires a done callback";
    }
    if (message.GetDescriptor() != ::rankpb::FreezeRankGenerationRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC RankAdmin.FreezeRankGeneration: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::rankpb::FreezeRankGenerationRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC RankAdmin.FreezeRankGeneration: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartRankAdminFreezeRankGenerationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::rankpb::FreezeRankGenerationResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region RankAdminRemoveRankEntry
namespace {
void QueueLocalRankAdminRemoveRankEntryFailure(GrpcDone<::rankpb::RankAdminResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartRankAdminRemoveRankEntryCall(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::RemoveRankEntryRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::rankpb::RankAdminResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.RemoveRankEntry requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kRankAdminRemoveRankEntryGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalRankAdminRemoveRankEntryFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalRankAdminRemoveRankEntryFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::rankpb::RankAdminResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the RankAdmin.RemoveRankEntry response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendRankAdminRemoveRankEntry(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::RemoveRankEntryRequest& request,
                                            GrpcDone<::rankpb::RankAdminResponse> done, const GrpcCallOptions& options) {
    StartRankAdminRemoveRankEntryCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendRankAdminRemoveRankEntryErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.RemoveRankEntry requires a done callback";
    }
    if (message.GetDescriptor() != ::rankpb::RemoveRankEntryRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC RankAdmin.RemoveRankEntry: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::rankpb::RemoveRankEntryRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC RankAdmin.RemoveRankEntry: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartRankAdminRemoveRankEntryCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::rankpb::RankAdminResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region RankAdminDeleteRankGeneration
namespace {
void QueueLocalRankAdminDeleteRankGenerationFailure(GrpcDone<::rankpb::RankAdminResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartRankAdminDeleteRankGenerationCall(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::RemoveRankEntryRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::rankpb::RankAdminResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.DeleteRankGeneration requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kRankAdminDeleteRankGenerationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalRankAdminDeleteRankGenerationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalRankAdminDeleteRankGenerationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::rankpb::RankAdminResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the RankAdmin.DeleteRankGeneration response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendRankAdminDeleteRankGeneration(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::RemoveRankEntryRequest& request,
                                            GrpcDone<::rankpb::RankAdminResponse> done, const GrpcCallOptions& options) {
    StartRankAdminDeleteRankGenerationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendRankAdminDeleteRankGenerationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC RankAdmin.DeleteRankGeneration requires a done callback";
    }
    if (message.GetDescriptor() != ::rankpb::RemoveRankEntryRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC RankAdmin.DeleteRankGeneration: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::rankpb::RemoveRankEntryRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC RankAdmin.DeleteRankGeneration: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartRankAdminDeleteRankGenerationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::rankpb::RankAdminResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetRankAdminCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace rankpb
