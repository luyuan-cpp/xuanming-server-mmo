#include "muduo/base/Logging.h"

#include "mail_grpc_client.h"
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

namespace mailpb {
#pragma region ClientPlayerMailListMails
namespace {
void QueueLocalClientPlayerMailListMailsFailure(GrpcDone<::mailpb::ListMailsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerMailListMailsCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ListMailsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::ListMailsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.ListMails requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerMailListMailsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerMailListMailsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerMailListMailsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::ListMailsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerMail.ListMails response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerMailListMails(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ListMailsRequest& request,
                                            GrpcDone<::mailpb::ListMailsResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerMailListMailsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerMailListMailsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.ListMails requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::ListMailsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerMail.ListMails: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::ListMailsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerMail.ListMails: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerMailListMailsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::ListMailsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerMailReadMail
namespace {
void QueueLocalClientPlayerMailReadMailFailure(GrpcDone<::mailpb::ReadMailResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerMailReadMailCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ReadMailRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::ReadMailResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.ReadMail requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerMailReadMailGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerMailReadMailFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerMailReadMailFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::ReadMailResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerMail.ReadMail response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerMailReadMail(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ReadMailRequest& request,
                                            GrpcDone<::mailpb::ReadMailResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerMailReadMailCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerMailReadMailErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.ReadMail requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::ReadMailRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerMail.ReadMail: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::ReadMailRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerMail.ReadMail: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerMailReadMailCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::ReadMailResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerMailClaimMailAttachments
namespace {
void QueueLocalClientPlayerMailClaimMailAttachmentsFailure(GrpcDone<::mailpb::ClaimMailAttachmentsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerMailClaimMailAttachmentsCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ClaimMailAttachmentsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::ClaimMailAttachmentsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.ClaimMailAttachments requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerMailClaimMailAttachmentsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerMailClaimMailAttachmentsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerMailClaimMailAttachmentsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::ClaimMailAttachmentsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerMail.ClaimMailAttachments response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerMailClaimMailAttachments(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ClaimMailAttachmentsRequest& request,
                                            GrpcDone<::mailpb::ClaimMailAttachmentsResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerMailClaimMailAttachmentsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerMailClaimMailAttachmentsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.ClaimMailAttachments requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::ClaimMailAttachmentsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerMail.ClaimMailAttachments: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::ClaimMailAttachmentsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerMail.ClaimMailAttachments: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerMailClaimMailAttachmentsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::ClaimMailAttachmentsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerMailDeleteMails
namespace {
void QueueLocalClientPlayerMailDeleteMailsFailure(GrpcDone<::mailpb::DeleteMailsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerMailDeleteMailsCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::DeleteMailsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::DeleteMailsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.DeleteMails requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerMailDeleteMailsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerMailDeleteMailsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerMailDeleteMailsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::DeleteMailsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerMail.DeleteMails response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerMailDeleteMails(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::DeleteMailsRequest& request,
                                            GrpcDone<::mailpb::DeleteMailsResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerMailDeleteMailsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerMailDeleteMailsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.DeleteMails requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::DeleteMailsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerMail.DeleteMails: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::DeleteMailsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerMail.DeleteMails: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerMailDeleteMailsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::DeleteMailsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerMailClearReadMails
namespace {
void QueueLocalClientPlayerMailClearReadMailsFailure(GrpcDone<::mailpb::ClearReadMailsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerMailClearReadMailsCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ClearReadMailsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::ClearReadMailsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.ClearReadMails requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerMailClearReadMailsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerMailClearReadMailsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerMailClearReadMailsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::ClearReadMailsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerMail.ClearReadMails response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerMailClearReadMails(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ClearReadMailsRequest& request,
                                            GrpcDone<::mailpb::ClearReadMailsResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerMailClearReadMailsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerMailClearReadMailsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.ClearReadMails requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::ClearReadMailsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerMail.ClearReadMails: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::ClearReadMailsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerMail.ClearReadMails: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerMailClearReadMailsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::ClearReadMailsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerMailNotifyMailChanged
namespace {
void QueueLocalClientPlayerMailNotifyMailChangedFailure(GrpcDone<::mailpb::MailChangedEvent> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerMailNotifyMailChangedCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::MailChangedEvent& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::MailChangedEvent> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.NotifyMailChanged requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerMailNotifyMailChangedGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerMailNotifyMailChangedFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerMailNotifyMailChangedFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::MailChangedEvent reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerMail.NotifyMailChanged response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerMailNotifyMailChanged(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::MailChangedEvent& request,
                                            GrpcDone<::mailpb::MailChangedEvent> done, const GrpcCallOptions& options) {
    StartClientPlayerMailNotifyMailChangedCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerMailNotifyMailChangedErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerMail.NotifyMailChanged requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::MailChangedEvent::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerMail.NotifyMailChanged: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::MailChangedEvent*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerMail.NotifyMailChanged: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerMailNotifyMailChangedCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::MailChangedEvent* reply) { done(outcome, reply); });
}
#pragma endregion

void SetMailCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace mailpb
