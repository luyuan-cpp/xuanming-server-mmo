#include "muduo/base/Logging.h"

#include "match_service_grpc_client.h"
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
#pragma region MatchServiceGetNetworkProbePlan
namespace {
void QueueLocalMatchServiceGetNetworkProbePlanFailure(GrpcDone<::match::GetNetworkProbePlanResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetNetworkProbePlanGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetNetworkProbePlanCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetNetworkProbePlanRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetNetworkProbePlanResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetNetworkProbePlan requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetNetworkProbePlanGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetNetworkProbePlanFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetNetworkProbePlanFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetNetworkProbePlanResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetNetworkProbePlan response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetNetworkProbePlan(entt::registry& registry, entt::entity nodeEntity, const ::match::GetNetworkProbePlanRequest& request,
                                            GrpcDone<::match::GetNetworkProbePlanResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetNetworkProbePlanCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetNetworkProbePlanErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetNetworkProbePlan requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetNetworkProbePlanRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetNetworkProbePlan: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetNetworkProbePlanGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetNetworkProbePlanRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetNetworkProbePlan: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetNetworkProbePlanGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetNetworkProbePlanCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetNetworkProbePlanResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceCommitNetworkProbe
namespace {
void QueueLocalMatchServiceCommitNetworkProbeFailure(GrpcDone<::match::CommitNetworkProbeResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceCommitNetworkProbeGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceCommitNetworkProbeCall(entt::registry& registry, entt::entity nodeEntity, const ::match::CommitNetworkProbeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::CommitNetworkProbeResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CommitNetworkProbe requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceCommitNetworkProbeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceCommitNetworkProbeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceCommitNetworkProbeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::CommitNetworkProbeResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.CommitNetworkProbe response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceCommitNetworkProbe(entt::registry& registry, entt::entity nodeEntity, const ::match::CommitNetworkProbeRequest& request,
                                            GrpcDone<::match::CommitNetworkProbeResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceCommitNetworkProbeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceCommitNetworkProbeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CommitNetworkProbe requires a done callback";
    }
    if (message.GetDescriptor() != ::match::CommitNetworkProbeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.CommitNetworkProbe: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCommitNetworkProbeGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::CommitNetworkProbeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.CommitNetworkProbe: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCommitNetworkProbeGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceCommitNetworkProbeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::CommitNetworkProbeResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceJoinQueue
namespace {
void QueueLocalMatchServiceJoinQueueFailure(GrpcDone<::match::JoinQueueResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceJoinQueueGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceJoinQueueCall(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::JoinQueueResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.JoinQueue requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceJoinQueueGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceJoinQueueFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceJoinQueueFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::JoinQueueResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.JoinQueue response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request,
                                            GrpcDone<::match::JoinQueueResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceJoinQueueCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceJoinQueueErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.JoinQueue requires a done callback";
    }
    if (message.GetDescriptor() != ::match::JoinQueueRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.JoinQueue: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceJoinQueueGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::JoinQueueRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.JoinQueue: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceJoinQueueGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceJoinQueueCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::JoinQueueResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceCancelQueue
namespace {
void QueueLocalMatchServiceCancelQueueFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceCancelQueueGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceCancelQueueCall(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CancelQueue requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceCancelQueueGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceCancelQueueFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceCancelQueueFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the MatchService.CancelQueue response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartMatchServiceCancelQueueCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceCancelQueueErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CancelQueue requires a done callback";
    }
    if (message.GetDescriptor() != ::match::CancelQueueRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.CancelQueue: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCancelQueueGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::CancelQueueRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.CancelQueue: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCancelQueueGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceCancelQueueCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetQueueStatus
namespace {
void QueueLocalMatchServiceGetQueueStatusFailure(GrpcDone<::match::GetQueueStatusResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetQueueStatusGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetQueueStatusCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetQueueStatusResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetQueueStatus requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetQueueStatusGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetQueueStatusFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetQueueStatusFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetQueueStatusResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetQueueStatus response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request,
                                            GrpcDone<::match::GetQueueStatusResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetQueueStatusCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetQueueStatusErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetQueueStatus requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetQueueStatusRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetQueueStatus: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetQueueStatusGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetQueueStatusRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetQueueStatus: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetQueueStatusGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetQueueStatusCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetQueueStatusResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceChallengePlayer
namespace {
void QueueLocalMatchServiceChallengePlayerFailure(GrpcDone<::match::ChallengePlayerResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceChallengePlayerGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceChallengePlayerCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ChallengePlayerResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ChallengePlayer requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceChallengePlayerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceChallengePlayerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceChallengePlayerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ChallengePlayerResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ChallengePlayer response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request,
                                            GrpcDone<::match::ChallengePlayerResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceChallengePlayerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceChallengePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ChallengePlayer requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ChallengePlayerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ChallengePlayer: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceChallengePlayerGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ChallengePlayerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ChallengePlayer: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceChallengePlayerGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceChallengePlayerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ChallengePlayerResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceRespondChallenge
namespace {
void QueueLocalMatchServiceRespondChallengeFailure(GrpcDone<::match::RespondChallengeResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceRespondChallengeGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceRespondChallengeCall(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::RespondChallengeResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RespondChallenge requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceRespondChallengeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceRespondChallengeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceRespondChallengeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::RespondChallengeResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.RespondChallenge response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request,
                                            GrpcDone<::match::RespondChallengeResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceRespondChallengeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceRespondChallengeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RespondChallenge requires a done callback";
    }
    if (message.GetDescriptor() != ::match::RespondChallengeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.RespondChallenge: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRespondChallengeGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::RespondChallengeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.RespondChallenge: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRespondChallengeGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceRespondChallengeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::RespondChallengeResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceNotifyChallengeInvite
namespace {
void QueueLocalMatchServiceNotifyChallengeInviteFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceNotifyChallengeInviteGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceNotifyChallengeInviteCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.NotifyChallengeInvite requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceNotifyChallengeInviteGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceNotifyChallengeInviteFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceNotifyChallengeInviteFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the MatchService.NotifyChallengeInvite response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartMatchServiceNotifyChallengeInviteCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceNotifyChallengeInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.NotifyChallengeInvite requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ChallengeInviteS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.NotifyChallengeInvite: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceNotifyChallengeInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ChallengeInviteS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.NotifyChallengeInvite: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceNotifyChallengeInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceNotifyChallengeInviteCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceNotifyChallengeResult
namespace {
void QueueLocalMatchServiceNotifyChallengeResultFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceNotifyChallengeResultGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceNotifyChallengeResultCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.NotifyChallengeResult requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceNotifyChallengeResultGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceNotifyChallengeResultFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceNotifyChallengeResultFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the MatchService.NotifyChallengeResult response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartMatchServiceNotifyChallengeResultCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceNotifyChallengeResultErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.NotifyChallengeResult requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ChallengeResultS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.NotifyChallengeResult: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceNotifyChallengeResultGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ChallengeResultS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.NotifyChallengeResult: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceNotifyChallengeResultGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceNotifyChallengeResultCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceWatchBattle
namespace {
void QueueLocalMatchServiceWatchBattleFailure(GrpcDone<::match::WatchBattleResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceWatchBattleGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceWatchBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::WatchBattleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.WatchBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceWatchBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceWatchBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceWatchBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::WatchBattleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.WatchBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request,
                                            GrpcDone<::match::WatchBattleResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceWatchBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceWatchBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.WatchBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::match::WatchBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.WatchBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceWatchBattleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::WatchBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.WatchBattle: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceWatchBattleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceWatchBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::WatchBattleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceRequestBattleTicket
namespace {
void QueueLocalMatchServiceRequestBattleTicketFailure(GrpcDone<::RequestBattleTicketResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceRequestBattleTicketGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceRequestBattleTicketCall(entt::registry& registry, entt::entity nodeEntity, const ::RequestBattleTicketRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::RequestBattleTicketResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RequestBattleTicket requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceRequestBattleTicketGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceRequestBattleTicketFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceRequestBattleTicketFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::RequestBattleTicketResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.RequestBattleTicket response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::RequestBattleTicketRequest& request,
                                            GrpcDone<::RequestBattleTicketResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceRequestBattleTicketCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceRequestBattleTicketErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RequestBattleTicket requires a done callback";
    }
    if (message.GetDescriptor() != ::RequestBattleTicketRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.RequestBattleTicket: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRequestBattleTicketGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::RequestBattleTicketRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.RequestBattleTicket: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRequestBattleTicketGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceRequestBattleTicketCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::RequestBattleTicketResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceListWatchableBattles
namespace {
void QueueLocalMatchServiceListWatchableBattlesFailure(GrpcDone<::match::ListWatchableBattlesResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceListWatchableBattlesGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceListWatchableBattlesCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ListWatchableBattlesResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListWatchableBattles requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceListWatchableBattlesGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceListWatchableBattlesFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceListWatchableBattlesFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ListWatchableBattlesResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ListWatchableBattles response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request,
                                            GrpcDone<::match::ListWatchableBattlesResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceListWatchableBattlesCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceListWatchableBattlesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListWatchableBattles requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ListWatchableBattlesRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ListWatchableBattles: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListWatchableBattlesGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ListWatchableBattlesRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ListWatchableBattles: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListWatchableBattlesGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceListWatchableBattlesCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ListWatchableBattlesResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceListBattleReports
namespace {
void QueueLocalMatchServiceListBattleReportsFailure(GrpcDone<::match::ListBattleReportsResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceListBattleReportsGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceListBattleReportsCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ListBattleReportsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ListBattleReportsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListBattleReports requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceListBattleReportsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceListBattleReportsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceListBattleReportsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ListBattleReportsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ListBattleReports response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceListBattleReports(entt::registry& registry, entt::entity nodeEntity, const ::match::ListBattleReportsRequest& request,
                                            GrpcDone<::match::ListBattleReportsResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceListBattleReportsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceListBattleReportsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListBattleReports requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ListBattleReportsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ListBattleReports: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListBattleReportsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ListBattleReportsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ListBattleReports: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListBattleReportsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceListBattleReportsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ListBattleReportsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetBattleReport
namespace {
void QueueLocalMatchServiceGetBattleReportFailure(GrpcDone<::match::GetBattleReportResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetBattleReportGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetBattleReportCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetBattleReportRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetBattleReportResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetBattleReport requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetBattleReportGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetBattleReportFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetBattleReportFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetBattleReportResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetBattleReport response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetBattleReport(entt::registry& registry, entt::entity nodeEntity, const ::match::GetBattleReportRequest& request,
                                            GrpcDone<::match::GetBattleReportResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetBattleReportCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetBattleReportErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetBattleReport requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetBattleReportRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetBattleReport: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetBattleReportGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetBattleReportRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetBattleReport: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetBattleReportGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetBattleReportCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetBattleReportResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceListBattleScripts
namespace {
void QueueLocalMatchServiceListBattleScriptsFailure(GrpcDone<::match::ListBattleScriptsResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceListBattleScriptsGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceListBattleScriptsCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ListBattleScriptsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ListBattleScriptsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListBattleScripts requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceListBattleScriptsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceListBattleScriptsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceListBattleScriptsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ListBattleScriptsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ListBattleScripts response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceListBattleScripts(entt::registry& registry, entt::entity nodeEntity, const ::match::ListBattleScriptsRequest& request,
                                            GrpcDone<::match::ListBattleScriptsResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceListBattleScriptsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceListBattleScriptsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListBattleScripts requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ListBattleScriptsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ListBattleScripts: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListBattleScriptsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ListBattleScriptsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ListBattleScripts: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListBattleScriptsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceListBattleScriptsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ListBattleScriptsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceSubmitBattleScript
namespace {
void QueueLocalMatchServiceSubmitBattleScriptFailure(GrpcDone<::match::SubmitBattleScriptResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceSubmitBattleScriptGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceSubmitBattleScriptCall(entt::registry& registry, entt::entity nodeEntity, const ::match::SubmitBattleScriptRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::SubmitBattleScriptResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SubmitBattleScript requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceSubmitBattleScriptGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceSubmitBattleScriptFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceSubmitBattleScriptFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::SubmitBattleScriptResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.SubmitBattleScript response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceSubmitBattleScript(entt::registry& registry, entt::entity nodeEntity, const ::match::SubmitBattleScriptRequest& request,
                                            GrpcDone<::match::SubmitBattleScriptResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceSubmitBattleScriptCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceSubmitBattleScriptErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SubmitBattleScript requires a done callback";
    }
    if (message.GetDescriptor() != ::match::SubmitBattleScriptRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.SubmitBattleScript: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSubmitBattleScriptGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::SubmitBattleScriptRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.SubmitBattleScript: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSubmitBattleScriptGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceSubmitBattleScriptCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::SubmitBattleScriptResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceDeleteBattleScript
namespace {
void QueueLocalMatchServiceDeleteBattleScriptFailure(GrpcDone<::match::DeleteBattleScriptResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceDeleteBattleScriptGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceDeleteBattleScriptCall(entt::registry& registry, entt::entity nodeEntity, const ::match::DeleteBattleScriptRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::DeleteBattleScriptResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.DeleteBattleScript requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceDeleteBattleScriptGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceDeleteBattleScriptFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceDeleteBattleScriptFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::DeleteBattleScriptResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.DeleteBattleScript response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceDeleteBattleScript(entt::registry& registry, entt::entity nodeEntity, const ::match::DeleteBattleScriptRequest& request,
                                            GrpcDone<::match::DeleteBattleScriptResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceDeleteBattleScriptCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceDeleteBattleScriptErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.DeleteBattleScript requires a done callback";
    }
    if (message.GetDescriptor() != ::match::DeleteBattleScriptRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.DeleteBattleScript: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceDeleteBattleScriptGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::DeleteBattleScriptRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.DeleteBattleScript: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceDeleteBattleScriptGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceDeleteBattleScriptCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::DeleteBattleScriptResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceSetBattleScriptLoadout
namespace {
void QueueLocalMatchServiceSetBattleScriptLoadoutFailure(GrpcDone<::match::SetBattleScriptLoadoutResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceSetBattleScriptLoadoutGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceSetBattleScriptLoadoutCall(entt::registry& registry, entt::entity nodeEntity, const ::match::SetBattleScriptLoadoutRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::SetBattleScriptLoadoutResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SetBattleScriptLoadout requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceSetBattleScriptLoadoutGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceSetBattleScriptLoadoutFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceSetBattleScriptLoadoutFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::SetBattleScriptLoadoutResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.SetBattleScriptLoadout response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceSetBattleScriptLoadout(entt::registry& registry, entt::entity nodeEntity, const ::match::SetBattleScriptLoadoutRequest& request,
                                            GrpcDone<::match::SetBattleScriptLoadoutResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceSetBattleScriptLoadoutCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceSetBattleScriptLoadoutErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SetBattleScriptLoadout requires a done callback";
    }
    if (message.GetDescriptor() != ::match::SetBattleScriptLoadoutRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.SetBattleScriptLoadout: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSetBattleScriptLoadoutGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::SetBattleScriptLoadoutRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.SetBattleScriptLoadout: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSetBattleScriptLoadoutGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceSetBattleScriptLoadoutCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::SetBattleScriptLoadoutResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetRankedProfile
namespace {
void QueueLocalMatchServiceGetRankedProfileFailure(GrpcDone<::match::GetRankedProfileResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetRankedProfileGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetRankedProfileCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetRankedProfileRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetRankedProfileResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetRankedProfile requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetRankedProfileGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetRankedProfileFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetRankedProfileFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetRankedProfileResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetRankedProfile response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetRankedProfile(entt::registry& registry, entt::entity nodeEntity, const ::match::GetRankedProfileRequest& request,
                                            GrpcDone<::match::GetRankedProfileResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetRankedProfileCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetRankedProfileErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetRankedProfile requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetRankedProfileRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetRankedProfile: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetRankedProfileGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetRankedProfileRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetRankedProfile: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetRankedProfileGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetRankedProfileCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetRankedProfileResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceSaveRankedPreset
namespace {
void QueueLocalMatchServiceSaveRankedPresetFailure(GrpcDone<::match::SaveRankedPresetResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceSaveRankedPresetGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceSaveRankedPresetCall(entt::registry& registry, entt::entity nodeEntity, const ::match::SaveRankedPresetRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::SaveRankedPresetResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SaveRankedPreset requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceSaveRankedPresetGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceSaveRankedPresetFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceSaveRankedPresetFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::SaveRankedPresetResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.SaveRankedPreset response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceSaveRankedPreset(entt::registry& registry, entt::entity nodeEntity, const ::match::SaveRankedPresetRequest& request,
                                            GrpcDone<::match::SaveRankedPresetResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceSaveRankedPresetCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceSaveRankedPresetErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SaveRankedPreset requires a done callback";
    }
    if (message.GetDescriptor() != ::match::SaveRankedPresetRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.SaveRankedPreset: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSaveRankedPresetGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::SaveRankedPresetRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.SaveRankedPreset: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSaveRankedPresetGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceSaveRankedPresetCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::SaveRankedPresetResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceUpdateRankedCosmetic
namespace {
void QueueLocalMatchServiceUpdateRankedCosmeticFailure(GrpcDone<::match::UpdateRankedCosmeticResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceUpdateRankedCosmeticGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceUpdateRankedCosmeticCall(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateRankedCosmeticRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::UpdateRankedCosmeticResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.UpdateRankedCosmetic requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceUpdateRankedCosmeticGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceUpdateRankedCosmeticFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceUpdateRankedCosmeticFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::UpdateRankedCosmeticResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.UpdateRankedCosmetic response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceUpdateRankedCosmetic(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateRankedCosmeticRequest& request,
                                            GrpcDone<::match::UpdateRankedCosmeticResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceUpdateRankedCosmeticCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceUpdateRankedCosmeticErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.UpdateRankedCosmetic requires a done callback";
    }
    if (message.GetDescriptor() != ::match::UpdateRankedCosmeticRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.UpdateRankedCosmetic: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceUpdateRankedCosmeticGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::UpdateRankedCosmeticRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.UpdateRankedCosmetic: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceUpdateRankedCosmeticGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceUpdateRankedCosmeticCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::UpdateRankedCosmeticResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetRankedBattleResult
namespace {
void QueueLocalMatchServiceGetRankedBattleResultFailure(GrpcDone<::match::GetRankedBattleResultResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetRankedBattleResultGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetRankedBattleResultCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetRankedBattleResultRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetRankedBattleResultResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetRankedBattleResult requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetRankedBattleResultGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetRankedBattleResultFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetRankedBattleResultFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetRankedBattleResultResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetRankedBattleResult response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetRankedBattleResult(entt::registry& registry, entt::entity nodeEntity, const ::match::GetRankedBattleResultRequest& request,
                                            GrpcDone<::match::GetRankedBattleResultResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetRankedBattleResultCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetRankedBattleResultErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetRankedBattleResult requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetRankedBattleResultRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetRankedBattleResult: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetRankedBattleResultGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetRankedBattleResultRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetRankedBattleResult: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetRankedBattleResultGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetRankedBattleResultCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetRankedBattleResultResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetTournamentOverview
namespace {
void QueueLocalMatchServiceGetTournamentOverviewFailure(GrpcDone<::match::GetTournamentOverviewResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentOverviewGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetTournamentOverviewCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentOverviewRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetTournamentOverviewResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTournamentOverview requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetTournamentOverviewGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetTournamentOverviewFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetTournamentOverviewFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetTournamentOverviewResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetTournamentOverview response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetTournamentOverview(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentOverviewRequest& request,
                                            GrpcDone<::match::GetTournamentOverviewResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetTournamentOverviewCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetTournamentOverviewErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTournamentOverview requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetTournamentOverviewRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetTournamentOverview: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentOverviewGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetTournamentOverviewRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetTournamentOverview: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentOverviewGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetTournamentOverviewCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetTournamentOverviewResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceCreateTournamentSquad
namespace {
void QueueLocalMatchServiceCreateTournamentSquadFailure(GrpcDone<::match::CreateTournamentSquadResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceCreateTournamentSquadGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceCreateTournamentSquadCall(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateTournamentSquadRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::CreateTournamentSquadResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CreateTournamentSquad requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceCreateTournamentSquadGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceCreateTournamentSquadFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceCreateTournamentSquadFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::CreateTournamentSquadResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.CreateTournamentSquad response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceCreateTournamentSquad(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateTournamentSquadRequest& request,
                                            GrpcDone<::match::CreateTournamentSquadResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceCreateTournamentSquadCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceCreateTournamentSquadErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CreateTournamentSquad requires a done callback";
    }
    if (message.GetDescriptor() != ::match::CreateTournamentSquadRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.CreateTournamentSquad: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCreateTournamentSquadGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::CreateTournamentSquadRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.CreateTournamentSquad: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCreateTournamentSquadGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceCreateTournamentSquadCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::CreateTournamentSquadResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceInviteTournamentMember
namespace {
void QueueLocalMatchServiceInviteTournamentMemberFailure(GrpcDone<::match::InviteTournamentMemberResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceInviteTournamentMemberGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceInviteTournamentMemberCall(entt::registry& registry, entt::entity nodeEntity, const ::match::InviteTournamentMemberRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::InviteTournamentMemberResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.InviteTournamentMember requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceInviteTournamentMemberGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceInviteTournamentMemberFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceInviteTournamentMemberFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::InviteTournamentMemberResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.InviteTournamentMember response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceInviteTournamentMember(entt::registry& registry, entt::entity nodeEntity, const ::match::InviteTournamentMemberRequest& request,
                                            GrpcDone<::match::InviteTournamentMemberResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceInviteTournamentMemberCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceInviteTournamentMemberErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.InviteTournamentMember requires a done callback";
    }
    if (message.GetDescriptor() != ::match::InviteTournamentMemberRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.InviteTournamentMember: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceInviteTournamentMemberGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::InviteTournamentMemberRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.InviteTournamentMember: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceInviteTournamentMemberGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceInviteTournamentMemberCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::InviteTournamentMemberResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceRespondTournamentInvite
namespace {
void QueueLocalMatchServiceRespondTournamentInviteFailure(GrpcDone<::match::RespondTournamentInviteResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceRespondTournamentInviteGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceRespondTournamentInviteCall(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondTournamentInviteRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::RespondTournamentInviteResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RespondTournamentInvite requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceRespondTournamentInviteGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceRespondTournamentInviteFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceRespondTournamentInviteFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::RespondTournamentInviteResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.RespondTournamentInvite response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceRespondTournamentInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondTournamentInviteRequest& request,
                                            GrpcDone<::match::RespondTournamentInviteResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceRespondTournamentInviteCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceRespondTournamentInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RespondTournamentInvite requires a done callback";
    }
    if (message.GetDescriptor() != ::match::RespondTournamentInviteRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.RespondTournamentInvite: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRespondTournamentInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::RespondTournamentInviteRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.RespondTournamentInvite: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRespondTournamentInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceRespondTournamentInviteCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::RespondTournamentInviteResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceLeaveTournamentSquad
namespace {
void QueueLocalMatchServiceLeaveTournamentSquadFailure(GrpcDone<::match::LeaveTournamentSquadResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceLeaveTournamentSquadGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceLeaveTournamentSquadCall(entt::registry& registry, entt::entity nodeEntity, const ::match::LeaveTournamentSquadRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::LeaveTournamentSquadResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.LeaveTournamentSquad requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceLeaveTournamentSquadGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceLeaveTournamentSquadFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceLeaveTournamentSquadFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::LeaveTournamentSquadResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.LeaveTournamentSquad response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceLeaveTournamentSquad(entt::registry& registry, entt::entity nodeEntity, const ::match::LeaveTournamentSquadRequest& request,
                                            GrpcDone<::match::LeaveTournamentSquadResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceLeaveTournamentSquadCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceLeaveTournamentSquadErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.LeaveTournamentSquad requires a done callback";
    }
    if (message.GetDescriptor() != ::match::LeaveTournamentSquadRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.LeaveTournamentSquad: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceLeaveTournamentSquadGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::LeaveTournamentSquadRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.LeaveTournamentSquad: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceLeaveTournamentSquadGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceLeaveTournamentSquadCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::LeaveTournamentSquadResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceUpdateTournamentLineup
namespace {
void QueueLocalMatchServiceUpdateTournamentLineupFailure(GrpcDone<::match::UpdateTournamentLineupResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceUpdateTournamentLineupGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceUpdateTournamentLineupCall(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateTournamentLineupRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::UpdateTournamentLineupResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.UpdateTournamentLineup requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceUpdateTournamentLineupGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceUpdateTournamentLineupFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceUpdateTournamentLineupFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::UpdateTournamentLineupResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.UpdateTournamentLineup response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceUpdateTournamentLineup(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateTournamentLineupRequest& request,
                                            GrpcDone<::match::UpdateTournamentLineupResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceUpdateTournamentLineupCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceUpdateTournamentLineupErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.UpdateTournamentLineup requires a done callback";
    }
    if (message.GetDescriptor() != ::match::UpdateTournamentLineupRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.UpdateTournamentLineup: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceUpdateTournamentLineupGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::UpdateTournamentLineupRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.UpdateTournamentLineup: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceUpdateTournamentLineupGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceUpdateTournamentLineupCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::UpdateTournamentLineupResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceRegisterTournamentSquad
namespace {
void QueueLocalMatchServiceRegisterTournamentSquadFailure(GrpcDone<::match::RegisterTournamentSquadResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceRegisterTournamentSquadGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceRegisterTournamentSquadCall(entt::registry& registry, entt::entity nodeEntity, const ::match::RegisterTournamentSquadRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::RegisterTournamentSquadResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RegisterTournamentSquad requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceRegisterTournamentSquadGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceRegisterTournamentSquadFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceRegisterTournamentSquadFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::RegisterTournamentSquadResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.RegisterTournamentSquad response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceRegisterTournamentSquad(entt::registry& registry, entt::entity nodeEntity, const ::match::RegisterTournamentSquadRequest& request,
                                            GrpcDone<::match::RegisterTournamentSquadResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceRegisterTournamentSquadCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceRegisterTournamentSquadErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RegisterTournamentSquad requires a done callback";
    }
    if (message.GetDescriptor() != ::match::RegisterTournamentSquadRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.RegisterTournamentSquad: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRegisterTournamentSquadGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::RegisterTournamentSquadRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.RegisterTournamentSquad: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRegisterTournamentSquadGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceRegisterTournamentSquadCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::RegisterTournamentSquadResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceListTournamentStandings
namespace {
void QueueLocalMatchServiceListTournamentStandingsFailure(GrpcDone<::match::ListTournamentStandingsResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceListTournamentStandingsGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceListTournamentStandingsCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ListTournamentStandingsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ListTournamentStandingsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListTournamentStandings requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceListTournamentStandingsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceListTournamentStandingsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceListTournamentStandingsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ListTournamentStandingsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ListTournamentStandings response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceListTournamentStandings(entt::registry& registry, entt::entity nodeEntity, const ::match::ListTournamentStandingsRequest& request,
                                            GrpcDone<::match::ListTournamentStandingsResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceListTournamentStandingsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceListTournamentStandingsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListTournamentStandings requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ListTournamentStandingsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ListTournamentStandings: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListTournamentStandingsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ListTournamentStandingsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ListTournamentStandings: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListTournamentStandingsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceListTournamentStandingsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ListTournamentStandingsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceStartTournamentMatch
namespace {
void QueueLocalMatchServiceStartTournamentMatchFailure(GrpcDone<::match::StartTournamentMatchResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceStartTournamentMatchGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceStartTournamentMatchCall(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTournamentMatchRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::StartTournamentMatchResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.StartTournamentMatch requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceStartTournamentMatchGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceStartTournamentMatchFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceStartTournamentMatchFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::StartTournamentMatchResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.StartTournamentMatch response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceStartTournamentMatch(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTournamentMatchRequest& request,
                                            GrpcDone<::match::StartTournamentMatchResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceStartTournamentMatchCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceStartTournamentMatchErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.StartTournamentMatch requires a done callback";
    }
    if (message.GetDescriptor() != ::match::StartTournamentMatchRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.StartTournamentMatch: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceStartTournamentMatchGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::StartTournamentMatchRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.StartTournamentMatch: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceStartTournamentMatchGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceStartTournamentMatchCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::StartTournamentMatchResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceCheerTournamentSquad
namespace {
void QueueLocalMatchServiceCheerTournamentSquadFailure(GrpcDone<::match::CheerTournamentSquadResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceCheerTournamentSquadGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceCheerTournamentSquadCall(entt::registry& registry, entt::entity nodeEntity, const ::match::CheerTournamentSquadRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::CheerTournamentSquadResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CheerTournamentSquad requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceCheerTournamentSquadGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceCheerTournamentSquadFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceCheerTournamentSquadFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::CheerTournamentSquadResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.CheerTournamentSquad response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceCheerTournamentSquad(entt::registry& registry, entt::entity nodeEntity, const ::match::CheerTournamentSquadRequest& request,
                                            GrpcDone<::match::CheerTournamentSquadResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceCheerTournamentSquadCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceCheerTournamentSquadErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CheerTournamentSquad requires a done callback";
    }
    if (message.GetDescriptor() != ::match::CheerTournamentSquadRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.CheerTournamentSquad: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCheerTournamentSquadGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::CheerTournamentSquadRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.CheerTournamentSquad: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCheerTournamentSquadGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceCheerTournamentSquadCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::CheerTournamentSquadResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceClaimTournamentReward
namespace {
void QueueLocalMatchServiceClaimTournamentRewardFailure(GrpcDone<::match::ClaimTournamentRewardResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceClaimTournamentRewardGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceClaimTournamentRewardCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ClaimTournamentRewardRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ClaimTournamentRewardResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ClaimTournamentReward requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceClaimTournamentRewardGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceClaimTournamentRewardFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceClaimTournamentRewardFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ClaimTournamentRewardResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ClaimTournamentReward response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceClaimTournamentReward(entt::registry& registry, entt::entity nodeEntity, const ::match::ClaimTournamentRewardRequest& request,
                                            GrpcDone<::match::ClaimTournamentRewardResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceClaimTournamentRewardCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceClaimTournamentRewardErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ClaimTournamentReward requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ClaimTournamentRewardRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ClaimTournamentReward: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceClaimTournamentRewardGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ClaimTournamentRewardRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ClaimTournamentReward: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceClaimTournamentRewardGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceClaimTournamentRewardCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ClaimTournamentRewardResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceSendTournamentSpectateCheer
namespace {
void QueueLocalMatchServiceSendTournamentSpectateCheerFailure(GrpcDone<::match::SendTournamentSpectateCheerResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceSendTournamentSpectateCheerGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceSendTournamentSpectateCheerCall(entt::registry& registry, entt::entity nodeEntity, const ::match::SendTournamentSpectateCheerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::SendTournamentSpectateCheerResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SendTournamentSpectateCheer requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceSendTournamentSpectateCheerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceSendTournamentSpectateCheerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceSendTournamentSpectateCheerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::SendTournamentSpectateCheerResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.SendTournamentSpectateCheer response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceSendTournamentSpectateCheer(entt::registry& registry, entt::entity nodeEntity, const ::match::SendTournamentSpectateCheerRequest& request,
                                            GrpcDone<::match::SendTournamentSpectateCheerResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceSendTournamentSpectateCheerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceSendTournamentSpectateCheerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SendTournamentSpectateCheer requires a done callback";
    }
    if (message.GetDescriptor() != ::match::SendTournamentSpectateCheerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.SendTournamentSpectateCheer: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSendTournamentSpectateCheerGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::SendTournamentSpectateCheerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.SendTournamentSpectateCheer: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSendTournamentSpectateCheerGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceSendTournamentSpectateCheerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::SendTournamentSpectateCheerResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetTournamentSpectateCheer
namespace {
void QueueLocalMatchServiceGetTournamentSpectateCheerFailure(GrpcDone<::match::GetTournamentSpectateCheerResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentSpectateCheerGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetTournamentSpectateCheerCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentSpectateCheerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetTournamentSpectateCheerResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTournamentSpectateCheer requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetTournamentSpectateCheerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetTournamentSpectateCheerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetTournamentSpectateCheerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetTournamentSpectateCheerResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetTournamentSpectateCheer response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetTournamentSpectateCheer(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentSpectateCheerRequest& request,
                                            GrpcDone<::match::GetTournamentSpectateCheerResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetTournamentSpectateCheerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetTournamentSpectateCheerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTournamentSpectateCheer requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetTournamentSpectateCheerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetTournamentSpectateCheer: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentSpectateCheerGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetTournamentSpectateCheerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetTournamentSpectateCheer: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentSpectateCheerGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetTournamentSpectateCheerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetTournamentSpectateCheerResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceUpdateTournamentSquadEmblem
namespace {
void QueueLocalMatchServiceUpdateTournamentSquadEmblemFailure(GrpcDone<::match::UpdateTournamentSquadEmblemResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceUpdateTournamentSquadEmblemGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceUpdateTournamentSquadEmblemCall(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateTournamentSquadEmblemRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::UpdateTournamentSquadEmblemResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.UpdateTournamentSquadEmblem requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceUpdateTournamentSquadEmblemGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceUpdateTournamentSquadEmblemFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceUpdateTournamentSquadEmblemFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::UpdateTournamentSquadEmblemResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.UpdateTournamentSquadEmblem response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceUpdateTournamentSquadEmblem(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateTournamentSquadEmblemRequest& request,
                                            GrpcDone<::match::UpdateTournamentSquadEmblemResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceUpdateTournamentSquadEmblemCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceUpdateTournamentSquadEmblemErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.UpdateTournamentSquadEmblem requires a done callback";
    }
    if (message.GetDescriptor() != ::match::UpdateTournamentSquadEmblemRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.UpdateTournamentSquadEmblem: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceUpdateTournamentSquadEmblemGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::UpdateTournamentSquadEmblemRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.UpdateTournamentSquadEmblem: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceUpdateTournamentSquadEmblemGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceUpdateTournamentSquadEmblemCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::UpdateTournamentSquadEmblemResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceStartTournamentWarmup
namespace {
void QueueLocalMatchServiceStartTournamentWarmupFailure(GrpcDone<::match::StartTournamentWarmupResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceStartTournamentWarmupGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceStartTournamentWarmupCall(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTournamentWarmupRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::StartTournamentWarmupResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.StartTournamentWarmup requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceStartTournamentWarmupGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceStartTournamentWarmupFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceStartTournamentWarmupFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::StartTournamentWarmupResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.StartTournamentWarmup response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceStartTournamentWarmup(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTournamentWarmupRequest& request,
                                            GrpcDone<::match::StartTournamentWarmupResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceStartTournamentWarmupCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceStartTournamentWarmupErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.StartTournamentWarmup requires a done callback";
    }
    if (message.GetDescriptor() != ::match::StartTournamentWarmupRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.StartTournamentWarmup: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceStartTournamentWarmupGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::StartTournamentWarmupRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.StartTournamentWarmup: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceStartTournamentWarmupGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceStartTournamentWarmupCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::StartTournamentWarmupResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServicePredictTournamentMatch
namespace {
void QueueLocalMatchServicePredictTournamentMatchFailure(GrpcDone<::match::PredictTournamentMatchResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServicePredictTournamentMatchGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServicePredictTournamentMatchCall(entt::registry& registry, entt::entity nodeEntity, const ::match::PredictTournamentMatchRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::PredictTournamentMatchResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.PredictTournamentMatch requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServicePredictTournamentMatchGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServicePredictTournamentMatchFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServicePredictTournamentMatchFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::PredictTournamentMatchResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.PredictTournamentMatch response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServicePredictTournamentMatch(entt::registry& registry, entt::entity nodeEntity, const ::match::PredictTournamentMatchRequest& request,
                                            GrpcDone<::match::PredictTournamentMatchResponse> done, const GrpcCallOptions& options) {
    StartMatchServicePredictTournamentMatchCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServicePredictTournamentMatchErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.PredictTournamentMatch requires a done callback";
    }
    if (message.GetDescriptor() != ::match::PredictTournamentMatchRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.PredictTournamentMatch: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServicePredictTournamentMatchGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::PredictTournamentMatchRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.PredictTournamentMatch: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServicePredictTournamentMatchGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServicePredictTournamentMatchCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::PredictTournamentMatchResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetTournamentPredictionBoard
namespace {
void QueueLocalMatchServiceGetTournamentPredictionBoardFailure(GrpcDone<::match::GetTournamentPredictionBoardResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentPredictionBoardGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetTournamentPredictionBoardCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentPredictionBoardRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetTournamentPredictionBoardResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTournamentPredictionBoard requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetTournamentPredictionBoardGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetTournamentPredictionBoardFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetTournamentPredictionBoardFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetTournamentPredictionBoardResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetTournamentPredictionBoard response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetTournamentPredictionBoard(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentPredictionBoardRequest& request,
                                            GrpcDone<::match::GetTournamentPredictionBoardResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetTournamentPredictionBoardCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetTournamentPredictionBoardErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTournamentPredictionBoard requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetTournamentPredictionBoardRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetTournamentPredictionBoard: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentPredictionBoardGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetTournamentPredictionBoardRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetTournamentPredictionBoard: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentPredictionBoardGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetTournamentPredictionBoardCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetTournamentPredictionBoardResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceBanTournamentLeyline
namespace {
void QueueLocalMatchServiceBanTournamentLeylineFailure(GrpcDone<::match::BanTournamentLeylineResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceBanTournamentLeylineGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceBanTournamentLeylineCall(entt::registry& registry, entt::entity nodeEntity, const ::match::BanTournamentLeylineRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::BanTournamentLeylineResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.BanTournamentLeyline requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceBanTournamentLeylineGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceBanTournamentLeylineFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceBanTournamentLeylineFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::BanTournamentLeylineResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.BanTournamentLeyline response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceBanTournamentLeyline(entt::registry& registry, entt::entity nodeEntity, const ::match::BanTournamentLeylineRequest& request,
                                            GrpcDone<::match::BanTournamentLeylineResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceBanTournamentLeylineCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceBanTournamentLeylineErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.BanTournamentLeyline requires a done callback";
    }
    if (message.GetDescriptor() != ::match::BanTournamentLeylineRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.BanTournamentLeyline: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceBanTournamentLeylineGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::BanTournamentLeylineRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.BanTournamentLeyline: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceBanTournamentLeylineGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceBanTournamentLeylineCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::BanTournamentLeylineResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetTournamentHighlights
namespace {
void QueueLocalMatchServiceGetTournamentHighlightsFailure(GrpcDone<::match::GetTournamentHighlightsResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentHighlightsGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetTournamentHighlightsCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentHighlightsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetTournamentHighlightsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTournamentHighlights requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetTournamentHighlightsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetTournamentHighlightsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetTournamentHighlightsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetTournamentHighlightsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetTournamentHighlights response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetTournamentHighlights(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentHighlightsRequest& request,
                                            GrpcDone<::match::GetTournamentHighlightsResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetTournamentHighlightsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetTournamentHighlightsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTournamentHighlights requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetTournamentHighlightsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetTournamentHighlights: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentHighlightsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetTournamentHighlightsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetTournamentHighlights: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTournamentHighlightsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetTournamentHighlightsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetTournamentHighlightsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceCreateJianghuDuelInvite
namespace {
void QueueLocalMatchServiceCreateJianghuDuelInviteFailure(GrpcDone<::match::CreateJianghuDuelInviteResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceCreateJianghuDuelInviteGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceCreateJianghuDuelInviteCall(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateJianghuDuelInviteRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::CreateJianghuDuelInviteResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CreateJianghuDuelInvite requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceCreateJianghuDuelInviteGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceCreateJianghuDuelInviteFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceCreateJianghuDuelInviteFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::CreateJianghuDuelInviteResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.CreateJianghuDuelInvite response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceCreateJianghuDuelInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateJianghuDuelInviteRequest& request,
                                            GrpcDone<::match::CreateJianghuDuelInviteResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceCreateJianghuDuelInviteCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceCreateJianghuDuelInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CreateJianghuDuelInvite requires a done callback";
    }
    if (message.GetDescriptor() != ::match::CreateJianghuDuelInviteRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.CreateJianghuDuelInvite: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCreateJianghuDuelInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::CreateJianghuDuelInviteRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.CreateJianghuDuelInvite: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCreateJianghuDuelInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceCreateJianghuDuelInviteCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::CreateJianghuDuelInviteResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceConfirmJianghuDuelMember
namespace {
void QueueLocalMatchServiceConfirmJianghuDuelMemberFailure(GrpcDone<::match::ConfirmJianghuDuelMemberResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceConfirmJianghuDuelMemberGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceConfirmJianghuDuelMemberCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ConfirmJianghuDuelMemberRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ConfirmJianghuDuelMemberResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ConfirmJianghuDuelMember requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceConfirmJianghuDuelMemberGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceConfirmJianghuDuelMemberFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceConfirmJianghuDuelMemberFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ConfirmJianghuDuelMemberResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ConfirmJianghuDuelMember response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceConfirmJianghuDuelMember(entt::registry& registry, entt::entity nodeEntity, const ::match::ConfirmJianghuDuelMemberRequest& request,
                                            GrpcDone<::match::ConfirmJianghuDuelMemberResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceConfirmJianghuDuelMemberCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceConfirmJianghuDuelMemberErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ConfirmJianghuDuelMember requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ConfirmJianghuDuelMemberRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ConfirmJianghuDuelMember: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceConfirmJianghuDuelMemberGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ConfirmJianghuDuelMemberRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ConfirmJianghuDuelMember: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceConfirmJianghuDuelMemberGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceConfirmJianghuDuelMemberCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ConfirmJianghuDuelMemberResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceRespondJianghuDuelInvite
namespace {
void QueueLocalMatchServiceRespondJianghuDuelInviteFailure(GrpcDone<::match::RespondJianghuDuelInviteResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceRespondJianghuDuelInviteGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceRespondJianghuDuelInviteCall(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondJianghuDuelInviteRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::RespondJianghuDuelInviteResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RespondJianghuDuelInvite requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceRespondJianghuDuelInviteGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceRespondJianghuDuelInviteFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceRespondJianghuDuelInviteFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::RespondJianghuDuelInviteResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.RespondJianghuDuelInvite response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceRespondJianghuDuelInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondJianghuDuelInviteRequest& request,
                                            GrpcDone<::match::RespondJianghuDuelInviteResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceRespondJianghuDuelInviteCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceRespondJianghuDuelInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.RespondJianghuDuelInvite requires a done callback";
    }
    if (message.GetDescriptor() != ::match::RespondJianghuDuelInviteRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.RespondJianghuDuelInvite: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRespondJianghuDuelInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::RespondJianghuDuelInviteRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.RespondJianghuDuelInvite: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceRespondJianghuDuelInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceRespondJianghuDuelInviteCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::RespondJianghuDuelInviteResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceListJianghuGrudges
namespace {
void QueueLocalMatchServiceListJianghuGrudgesFailure(GrpcDone<::match::ListJianghuGrudgesResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceListJianghuGrudgesGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceListJianghuGrudgesCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ListJianghuGrudgesRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ListJianghuGrudgesResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListJianghuGrudges requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceListJianghuGrudgesGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceListJianghuGrudgesFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceListJianghuGrudgesFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ListJianghuGrudgesResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ListJianghuGrudges response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceListJianghuGrudges(entt::registry& registry, entt::entity nodeEntity, const ::match::ListJianghuGrudgesRequest& request,
                                            GrpcDone<::match::ListJianghuGrudgesResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceListJianghuGrudgesCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceListJianghuGrudgesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListJianghuGrudges requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ListJianghuGrudgesRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ListJianghuGrudges: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListJianghuGrudgesGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ListJianghuGrudgesRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ListJianghuGrudges: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListJianghuGrudgesGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceListJianghuGrudgesCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ListJianghuGrudgesResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceDismissJianghuGrudge
namespace {
void QueueLocalMatchServiceDismissJianghuGrudgeFailure(GrpcDone<::match::DismissJianghuGrudgeResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceDismissJianghuGrudgeGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceDismissJianghuGrudgeCall(entt::registry& registry, entt::entity nodeEntity, const ::match::DismissJianghuGrudgeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::DismissJianghuGrudgeResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.DismissJianghuGrudge requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceDismissJianghuGrudgeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceDismissJianghuGrudgeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceDismissJianghuGrudgeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::DismissJianghuGrudgeResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.DismissJianghuGrudge response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceDismissJianghuGrudge(entt::registry& registry, entt::entity nodeEntity, const ::match::DismissJianghuGrudgeRequest& request,
                                            GrpcDone<::match::DismissJianghuGrudgeResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceDismissJianghuGrudgeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceDismissJianghuGrudgeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.DismissJianghuGrudge requires a done callback";
    }
    if (message.GetDescriptor() != ::match::DismissJianghuGrudgeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.DismissJianghuGrudge: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceDismissJianghuGrudgeGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::DismissJianghuGrudgeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.DismissJianghuGrudge: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceDismissJianghuGrudgeGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceDismissJianghuGrudgeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::DismissJianghuGrudgeResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceNotifyJianghuDuelInvite
namespace {
void QueueLocalMatchServiceNotifyJianghuDuelInviteFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceNotifyJianghuDuelInviteGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceNotifyJianghuDuelInviteCall(entt::registry& registry, entt::entity nodeEntity, const ::match::JianghuDuelInviteS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.NotifyJianghuDuelInvite requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceNotifyJianghuDuelInviteGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceNotifyJianghuDuelInviteFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceNotifyJianghuDuelInviteFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the MatchService.NotifyJianghuDuelInvite response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceNotifyJianghuDuelInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::JianghuDuelInviteS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartMatchServiceNotifyJianghuDuelInviteCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceNotifyJianghuDuelInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.NotifyJianghuDuelInvite requires a done callback";
    }
    if (message.GetDescriptor() != ::match::JianghuDuelInviteS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.NotifyJianghuDuelInvite: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceNotifyJianghuDuelInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::JianghuDuelInviteS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.NotifyJianghuDuelInvite: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceNotifyJianghuDuelInviteGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceNotifyJianghuDuelInviteCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceNotifyJianghuDuelStatus
namespace {
void QueueLocalMatchServiceNotifyJianghuDuelStatusFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceNotifyJianghuDuelStatusGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceNotifyJianghuDuelStatusCall(entt::registry& registry, entt::entity nodeEntity, const ::match::JianghuDuelStatusS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.NotifyJianghuDuelStatus requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceNotifyJianghuDuelStatusGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceNotifyJianghuDuelStatusFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceNotifyJianghuDuelStatusFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the MatchService.NotifyJianghuDuelStatus response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceNotifyJianghuDuelStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::JianghuDuelStatusS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartMatchServiceNotifyJianghuDuelStatusCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceNotifyJianghuDuelStatusErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.NotifyJianghuDuelStatus requires a done callback";
    }
    if (message.GetDescriptor() != ::match::JianghuDuelStatusS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.NotifyJianghuDuelStatus: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceNotifyJianghuDuelStatusGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::JianghuDuelStatusS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.NotifyJianghuDuelStatus: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceNotifyJianghuDuelStatusGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceNotifyJianghuDuelStatusCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceCreateTrialChallengeCard
namespace {
void QueueLocalMatchServiceCreateTrialChallengeCardFailure(GrpcDone<::match::CreateTrialChallengeCardResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceCreateTrialChallengeCardGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceCreateTrialChallengeCardCall(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateTrialChallengeCardRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::CreateTrialChallengeCardResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CreateTrialChallengeCard requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceCreateTrialChallengeCardGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceCreateTrialChallengeCardFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceCreateTrialChallengeCardFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::CreateTrialChallengeCardResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.CreateTrialChallengeCard response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceCreateTrialChallengeCard(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateTrialChallengeCardRequest& request,
                                            GrpcDone<::match::CreateTrialChallengeCardResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceCreateTrialChallengeCardCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceCreateTrialChallengeCardErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.CreateTrialChallengeCard requires a done callback";
    }
    if (message.GetDescriptor() != ::match::CreateTrialChallengeCardRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.CreateTrialChallengeCard: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCreateTrialChallengeCardGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::CreateTrialChallengeCardRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.CreateTrialChallengeCard: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceCreateTrialChallengeCardGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceCreateTrialChallengeCardCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::CreateTrialChallengeCardResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetTrialChallengeCard
namespace {
void QueueLocalMatchServiceGetTrialChallengeCardFailure(GrpcDone<::match::GetTrialChallengeCardResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetTrialChallengeCardGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetTrialChallengeCardCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTrialChallengeCardRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetTrialChallengeCardResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTrialChallengeCard requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetTrialChallengeCardGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetTrialChallengeCardFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetTrialChallengeCardFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetTrialChallengeCardResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetTrialChallengeCard response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetTrialChallengeCard(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTrialChallengeCardRequest& request,
                                            GrpcDone<::match::GetTrialChallengeCardResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetTrialChallengeCardCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetTrialChallengeCardErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTrialChallengeCard requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetTrialChallengeCardRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetTrialChallengeCard: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTrialChallengeCardGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetTrialChallengeCardRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetTrialChallengeCard: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTrialChallengeCardGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetTrialChallengeCardCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetTrialChallengeCardResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetSharedTrialReplay
namespace {
void QueueLocalMatchServiceGetSharedTrialReplayFailure(GrpcDone<::match::GetSharedTrialReplayResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetSharedTrialReplayGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetSharedTrialReplayCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetSharedTrialReplayRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetSharedTrialReplayResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetSharedTrialReplay requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetSharedTrialReplayGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetSharedTrialReplayFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetSharedTrialReplayFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetSharedTrialReplayResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetSharedTrialReplay response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetSharedTrialReplay(entt::registry& registry, entt::entity nodeEntity, const ::match::GetSharedTrialReplayRequest& request,
                                            GrpcDone<::match::GetSharedTrialReplayResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetSharedTrialReplayCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetSharedTrialReplayErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetSharedTrialReplay requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetSharedTrialReplayRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetSharedTrialReplay: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetSharedTrialReplayGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetSharedTrialReplayRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetSharedTrialReplay: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetSharedTrialReplayGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetSharedTrialReplayCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetSharedTrialReplayResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceListTacticalPuzzles
namespace {
void QueueLocalMatchServiceListTacticalPuzzlesFailure(GrpcDone<::match::ListTacticalPuzzlesResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceListTacticalPuzzlesGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceListTacticalPuzzlesCall(entt::registry& registry, entt::entity nodeEntity, const ::match::ListTacticalPuzzlesRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::ListTacticalPuzzlesResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListTacticalPuzzles requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceListTacticalPuzzlesGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceListTacticalPuzzlesFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceListTacticalPuzzlesFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::ListTacticalPuzzlesResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.ListTacticalPuzzles response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceListTacticalPuzzles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListTacticalPuzzlesRequest& request,
                                            GrpcDone<::match::ListTacticalPuzzlesResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceListTacticalPuzzlesCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceListTacticalPuzzlesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.ListTacticalPuzzles requires a done callback";
    }
    if (message.GetDescriptor() != ::match::ListTacticalPuzzlesRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.ListTacticalPuzzles: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListTacticalPuzzlesGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::ListTacticalPuzzlesRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.ListTacticalPuzzles: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceListTacticalPuzzlesGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceListTacticalPuzzlesCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::ListTacticalPuzzlesResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetTacticalPuzzle
namespace {
void QueueLocalMatchServiceGetTacticalPuzzleFailure(GrpcDone<::match::GetTacticalPuzzleResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetTacticalPuzzleGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetTacticalPuzzleCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTacticalPuzzleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetTacticalPuzzleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTacticalPuzzle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetTacticalPuzzleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetTacticalPuzzleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetTacticalPuzzleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetTacticalPuzzleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetTacticalPuzzle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetTacticalPuzzle(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTacticalPuzzleRequest& request,
                                            GrpcDone<::match::GetTacticalPuzzleResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetTacticalPuzzleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetTacticalPuzzleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTacticalPuzzle requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetTacticalPuzzleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetTacticalPuzzle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTacticalPuzzleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetTacticalPuzzleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetTacticalPuzzle: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTacticalPuzzleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetTacticalPuzzleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetTacticalPuzzleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceSaveTacticalPuzzleDraft
namespace {
void QueueLocalMatchServiceSaveTacticalPuzzleDraftFailure(GrpcDone<::match::SaveTacticalPuzzleDraftResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceSaveTacticalPuzzleDraftGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceSaveTacticalPuzzleDraftCall(entt::registry& registry, entt::entity nodeEntity, const ::match::SaveTacticalPuzzleDraftRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::SaveTacticalPuzzleDraftResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SaveTacticalPuzzleDraft requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceSaveTacticalPuzzleDraftGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceSaveTacticalPuzzleDraftFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceSaveTacticalPuzzleDraftFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::SaveTacticalPuzzleDraftResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.SaveTacticalPuzzleDraft response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceSaveTacticalPuzzleDraft(entt::registry& registry, entt::entity nodeEntity, const ::match::SaveTacticalPuzzleDraftRequest& request,
                                            GrpcDone<::match::SaveTacticalPuzzleDraftResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceSaveTacticalPuzzleDraftCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceSaveTacticalPuzzleDraftErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SaveTacticalPuzzleDraft requires a done callback";
    }
    if (message.GetDescriptor() != ::match::SaveTacticalPuzzleDraftRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.SaveTacticalPuzzleDraft: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSaveTacticalPuzzleDraftGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::SaveTacticalPuzzleDraftRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.SaveTacticalPuzzleDraft: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSaveTacticalPuzzleDraftGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceSaveTacticalPuzzleDraftCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::SaveTacticalPuzzleDraftResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceDeleteTacticalPuzzle
namespace {
void QueueLocalMatchServiceDeleteTacticalPuzzleFailure(GrpcDone<::match::DeleteTacticalPuzzleResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceDeleteTacticalPuzzleGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceDeleteTacticalPuzzleCall(entt::registry& registry, entt::entity nodeEntity, const ::match::DeleteTacticalPuzzleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::DeleteTacticalPuzzleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.DeleteTacticalPuzzle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceDeleteTacticalPuzzleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceDeleteTacticalPuzzleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceDeleteTacticalPuzzleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::DeleteTacticalPuzzleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.DeleteTacticalPuzzle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceDeleteTacticalPuzzle(entt::registry& registry, entt::entity nodeEntity, const ::match::DeleteTacticalPuzzleRequest& request,
                                            GrpcDone<::match::DeleteTacticalPuzzleResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceDeleteTacticalPuzzleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceDeleteTacticalPuzzleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.DeleteTacticalPuzzle requires a done callback";
    }
    if (message.GetDescriptor() != ::match::DeleteTacticalPuzzleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.DeleteTacticalPuzzle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceDeleteTacticalPuzzleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::DeleteTacticalPuzzleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.DeleteTacticalPuzzle: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceDeleteTacticalPuzzleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceDeleteTacticalPuzzleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::DeleteTacticalPuzzleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceStartTacticalPuzzleBattle
namespace {
void QueueLocalMatchServiceStartTacticalPuzzleBattleFailure(GrpcDone<::match::StartTacticalPuzzleBattleResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceStartTacticalPuzzleBattleGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceStartTacticalPuzzleBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTacticalPuzzleBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::StartTacticalPuzzleBattleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.StartTacticalPuzzleBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceStartTacticalPuzzleBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceStartTacticalPuzzleBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceStartTacticalPuzzleBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::StartTacticalPuzzleBattleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.StartTacticalPuzzleBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceStartTacticalPuzzleBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTacticalPuzzleBattleRequest& request,
                                            GrpcDone<::match::StartTacticalPuzzleBattleResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceStartTacticalPuzzleBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceStartTacticalPuzzleBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.StartTacticalPuzzleBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::match::StartTacticalPuzzleBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.StartTacticalPuzzleBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceStartTacticalPuzzleBattleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::StartTacticalPuzzleBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.StartTacticalPuzzleBattle: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceStartTacticalPuzzleBattleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceStartTacticalPuzzleBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::StartTacticalPuzzleBattleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceLikeTacticalPuzzle
namespace {
void QueueLocalMatchServiceLikeTacticalPuzzleFailure(GrpcDone<::match::LikeTacticalPuzzleResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceLikeTacticalPuzzleGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceLikeTacticalPuzzleCall(entt::registry& registry, entt::entity nodeEntity, const ::match::LikeTacticalPuzzleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::LikeTacticalPuzzleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.LikeTacticalPuzzle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceLikeTacticalPuzzleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceLikeTacticalPuzzleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceLikeTacticalPuzzleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::LikeTacticalPuzzleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.LikeTacticalPuzzle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceLikeTacticalPuzzle(entt::registry& registry, entt::entity nodeEntity, const ::match::LikeTacticalPuzzleRequest& request,
                                            GrpcDone<::match::LikeTacticalPuzzleResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceLikeTacticalPuzzleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceLikeTacticalPuzzleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.LikeTacticalPuzzle requires a done callback";
    }
    if (message.GetDescriptor() != ::match::LikeTacticalPuzzleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.LikeTacticalPuzzle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceLikeTacticalPuzzleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::LikeTacticalPuzzleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.LikeTacticalPuzzle: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceLikeTacticalPuzzleGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceLikeTacticalPuzzleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::LikeTacticalPuzzleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceGetTacticalPuzzleSolutionReplay
namespace {
void QueueLocalMatchServiceGetTacticalPuzzleSolutionReplayFailure(GrpcDone<::match::GetTacticalPuzzleSolutionReplayResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceGetTacticalPuzzleSolutionReplayGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceGetTacticalPuzzleSolutionReplayCall(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTacticalPuzzleSolutionReplayRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::GetTacticalPuzzleSolutionReplayResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTacticalPuzzleSolutionReplay requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceGetTacticalPuzzleSolutionReplayGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceGetTacticalPuzzleSolutionReplayFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceGetTacticalPuzzleSolutionReplayFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::GetTacticalPuzzleSolutionReplayResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.GetTacticalPuzzleSolutionReplay response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceGetTacticalPuzzleSolutionReplay(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTacticalPuzzleSolutionReplayRequest& request,
                                            GrpcDone<::match::GetTacticalPuzzleSolutionReplayResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceGetTacticalPuzzleSolutionReplayCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceGetTacticalPuzzleSolutionReplayErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.GetTacticalPuzzleSolutionReplay requires a done callback";
    }
    if (message.GetDescriptor() != ::match::GetTacticalPuzzleSolutionReplayRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.GetTacticalPuzzleSolutionReplay: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTacticalPuzzleSolutionReplayGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::GetTacticalPuzzleSolutionReplayRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.GetTacticalPuzzleSolutionReplay: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceGetTacticalPuzzleSolutionReplayGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceGetTacticalPuzzleSolutionReplayCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::GetTacticalPuzzleSolutionReplayResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServiceSendBattleSpectateChat
namespace {
void QueueLocalMatchServiceSendBattleSpectateChatFailure(GrpcDone<::match::SendBattleSpectateChatResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServiceSendBattleSpectateChatGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServiceSendBattleSpectateChatCall(entt::registry& registry, entt::entity nodeEntity, const ::match::SendBattleSpectateChatRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::SendBattleSpectateChatResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SendBattleSpectateChat requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServiceSendBattleSpectateChatGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServiceSendBattleSpectateChatFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServiceSendBattleSpectateChatFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::SendBattleSpectateChatResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.SendBattleSpectateChat response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServiceSendBattleSpectateChat(entt::registry& registry, entt::entity nodeEntity, const ::match::SendBattleSpectateChatRequest& request,
                                            GrpcDone<::match::SendBattleSpectateChatResponse> done, const GrpcCallOptions& options) {
    StartMatchServiceSendBattleSpectateChatCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServiceSendBattleSpectateChatErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.SendBattleSpectateChat requires a done callback";
    }
    if (message.GetDescriptor() != ::match::SendBattleSpectateChatRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.SendBattleSpectateChat: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSendBattleSpectateChatGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::SendBattleSpectateChatRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.SendBattleSpectateChat: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServiceSendBattleSpectateChatGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServiceSendBattleSpectateChatCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::SendBattleSpectateChatResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MatchServicePullBattleSpectateChat
namespace {
void QueueLocalMatchServicePullBattleSpectateChatFailure(GrpcDone<::match::PullBattleSpectateChatResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kMatchServicePullBattleSpectateChatGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMatchServicePullBattleSpectateChatCall(entt::registry& registry, entt::entity nodeEntity, const ::match::PullBattleSpectateChatRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::match::PullBattleSpectateChatResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.PullBattleSpectateChat requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMatchServicePullBattleSpectateChatGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMatchServicePullBattleSpectateChatFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMatchServicePullBattleSpectateChatFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::match::PullBattleSpectateChatResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MatchService.PullBattleSpectateChat response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMatchServicePullBattleSpectateChat(entt::registry& registry, entt::entity nodeEntity, const ::match::PullBattleSpectateChatRequest& request,
                                            GrpcDone<::match::PullBattleSpectateChatResponse> done, const GrpcCallOptions& options) {
    StartMatchServicePullBattleSpectateChatCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMatchServicePullBattleSpectateChatErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MatchService.PullBattleSpectateChat requires a done callback";
    }
    if (message.GetDescriptor() != ::match::PullBattleSpectateChatRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MatchService.PullBattleSpectateChat: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServicePullBattleSpectateChatGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::match::PullBattleSpectateChatRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MatchService.PullBattleSpectateChat: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kMatchServicePullBattleSpectateChatGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMatchServicePullBattleSpectateChatCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::match::PullBattleSpectateChatResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetMatchServiceCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace match
