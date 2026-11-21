#include "muduo/base/Logging.h"

#include "content_guard_grpc_client.h"
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

namespace content_guard {
#pragma region ContentGuardCheckText
namespace {
void QueueLocalContentGuardCheckTextFailure(GrpcDone<::content_guard::CheckTextResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kContentGuardCheckTextGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartContentGuardCheckTextCall(entt::registry& registry, entt::entity nodeEntity, const ::content_guard::CheckTextRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::content_guard::CheckTextResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ContentGuard.CheckText requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kContentGuardCheckTextGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalContentGuardCheckTextFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalContentGuardCheckTextFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::content_guard::CheckTextResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ContentGuard.CheckText response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendContentGuardCheckText(entt::registry& registry, entt::entity nodeEntity, const ::content_guard::CheckTextRequest& request,
                                            GrpcDone<::content_guard::CheckTextResponse> done, const GrpcCallOptions& options) {
    StartContentGuardCheckTextCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendContentGuardCheckTextErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ContentGuard.CheckText requires a done callback";
    }
    if (message.GetDescriptor() != ::content_guard::CheckTextRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ContentGuard.CheckText: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kContentGuardCheckTextGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::content_guard::CheckTextRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ContentGuard.CheckText: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kContentGuardCheckTextGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartContentGuardCheckTextCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::content_guard::CheckTextResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ContentGuardCheckPlatformText
namespace {
void QueueLocalContentGuardCheckPlatformTextFailure(GrpcDone<::content_guard::CheckTextResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kContentGuardCheckPlatformTextGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartContentGuardCheckPlatformTextCall(entt::registry& registry, entt::entity nodeEntity, const ::content_guard::CheckTextRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::content_guard::CheckTextResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ContentGuard.CheckPlatformText requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kContentGuardCheckPlatformTextGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalContentGuardCheckPlatformTextFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalContentGuardCheckPlatformTextFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::content_guard::CheckTextResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ContentGuard.CheckPlatformText response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendContentGuardCheckPlatformText(entt::registry& registry, entt::entity nodeEntity, const ::content_guard::CheckTextRequest& request,
                                            GrpcDone<::content_guard::CheckTextResponse> done, const GrpcCallOptions& options) {
    StartContentGuardCheckPlatformTextCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendContentGuardCheckPlatformTextErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ContentGuard.CheckPlatformText requires a done callback";
    }
    if (message.GetDescriptor() != ::content_guard::CheckTextRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ContentGuard.CheckPlatformText: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kContentGuardCheckPlatformTextGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::content_guard::CheckTextRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ContentGuard.CheckPlatformText: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kContentGuardCheckPlatformTextGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartContentGuardCheckPlatformTextCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::content_guard::CheckTextResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetContentGuardCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace content_guard
