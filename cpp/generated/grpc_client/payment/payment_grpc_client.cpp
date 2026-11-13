#include "muduo/base/Logging.h"

#include "payment_grpc_client.h"
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

namespace paymentpb {
#pragma region ClientPlayerPaymentListPayChannels
namespace {
void QueueLocalClientPlayerPaymentListPayChannelsFailure(GrpcDone<::paymentpb::ListPayChannelsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerPaymentListPayChannelsCall(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::ListPayChannelsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::paymentpb::ListPayChannelsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerPayment.ListPayChannels requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerPaymentListPayChannelsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerPaymentListPayChannelsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerPaymentListPayChannelsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::paymentpb::ListPayChannelsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerPayment.ListPayChannels response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerPaymentListPayChannels(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::ListPayChannelsRequest& request,
                                            GrpcDone<::paymentpb::ListPayChannelsResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerPaymentListPayChannelsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerPaymentListPayChannelsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerPayment.ListPayChannels requires a done callback";
    }
    if (message.GetDescriptor() != ::paymentpb::ListPayChannelsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerPayment.ListPayChannels: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::paymentpb::ListPayChannelsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerPayment.ListPayChannels: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerPaymentListPayChannelsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::paymentpb::ListPayChannelsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerPaymentCreatePayIntent
namespace {
void QueueLocalClientPlayerPaymentCreatePayIntentFailure(GrpcDone<::paymentpb::CreatePayIntentResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerPaymentCreatePayIntentCall(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::CreatePayIntentRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::paymentpb::CreatePayIntentResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerPayment.CreatePayIntent requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerPaymentCreatePayIntentGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerPaymentCreatePayIntentFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerPaymentCreatePayIntentFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::paymentpb::CreatePayIntentResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerPayment.CreatePayIntent response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerPaymentCreatePayIntent(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::CreatePayIntentRequest& request,
                                            GrpcDone<::paymentpb::CreatePayIntentResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerPaymentCreatePayIntentCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerPaymentCreatePayIntentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerPayment.CreatePayIntent requires a done callback";
    }
    if (message.GetDescriptor() != ::paymentpb::CreatePayIntentRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerPayment.CreatePayIntent: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::paymentpb::CreatePayIntentRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerPayment.CreatePayIntent: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerPaymentCreatePayIntentCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::paymentpb::CreatePayIntentResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerPaymentGetPayIntent
namespace {
void QueueLocalClientPlayerPaymentGetPayIntentFailure(GrpcDone<::paymentpb::GetPayIntentResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerPaymentGetPayIntentCall(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::GetPayIntentRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::paymentpb::GetPayIntentResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerPayment.GetPayIntent requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerPaymentGetPayIntentGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerPaymentGetPayIntentFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerPaymentGetPayIntentFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::paymentpb::GetPayIntentResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerPayment.GetPayIntent response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerPaymentGetPayIntent(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::GetPayIntentRequest& request,
                                            GrpcDone<::paymentpb::GetPayIntentResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerPaymentGetPayIntentCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerPaymentGetPayIntentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerPayment.GetPayIntent requires a done callback";
    }
    if (message.GetDescriptor() != ::paymentpb::GetPayIntentRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerPayment.GetPayIntent: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::paymentpb::GetPayIntentRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerPayment.GetPayIntent: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerPaymentGetPayIntentCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::paymentpb::GetPayIntentResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerPaymentSubmitPurchase
namespace {
void QueueLocalClientPlayerPaymentSubmitPurchaseFailure(GrpcDone<::paymentpb::SubmitPurchaseResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerPaymentSubmitPurchaseCall(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::SubmitPurchaseRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::paymentpb::SubmitPurchaseResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerPayment.SubmitPurchase requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerPaymentSubmitPurchaseGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerPaymentSubmitPurchaseFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerPaymentSubmitPurchaseFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::paymentpb::SubmitPurchaseResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerPayment.SubmitPurchase response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerPaymentSubmitPurchase(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::SubmitPurchaseRequest& request,
                                            GrpcDone<::paymentpb::SubmitPurchaseResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerPaymentSubmitPurchaseCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerPaymentSubmitPurchaseErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerPayment.SubmitPurchase requires a done callback";
    }
    if (message.GetDescriptor() != ::paymentpb::SubmitPurchaseRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerPayment.SubmitPurchase: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::paymentpb::SubmitPurchaseRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerPayment.SubmitPurchase: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerPaymentSubmitPurchaseCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::paymentpb::SubmitPurchaseResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetPaymentCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace paymentpb
