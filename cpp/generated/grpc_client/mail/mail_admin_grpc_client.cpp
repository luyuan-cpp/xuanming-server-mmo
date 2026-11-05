#include "muduo/base/Logging.h"

#include "mail_admin_grpc_client.h"
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
#pragma region MailAdminSendPersonalMail
namespace {
void QueueLocalMailAdminSendPersonalMailFailure(GrpcDone<::mailpb::SendMailResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMailAdminSendPersonalMailCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::SendMailRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::SendMailResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.SendPersonalMail requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMailAdminSendPersonalMailGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMailAdminSendPersonalMailFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMailAdminSendPersonalMailFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::SendMailResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MailAdmin.SendPersonalMail response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMailAdminSendPersonalMail(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::SendMailRequest& request,
                                            GrpcDone<::mailpb::SendMailResponse> done, const GrpcCallOptions& options) {
    StartMailAdminSendPersonalMailCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMailAdminSendPersonalMailErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.SendPersonalMail requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::SendMailRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MailAdmin.SendPersonalMail: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::SendMailRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MailAdmin.SendPersonalMail: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMailAdminSendPersonalMailCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::SendMailResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MailAdminSendSystemMail
namespace {
void QueueLocalMailAdminSendSystemMailFailure(GrpcDone<::mailpb::SendMailResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMailAdminSendSystemMailCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::SendMailRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::SendMailResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.SendSystemMail requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMailAdminSendSystemMailGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMailAdminSendSystemMailFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMailAdminSendSystemMailFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::SendMailResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MailAdmin.SendSystemMail response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMailAdminSendSystemMail(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::SendMailRequest& request,
                                            GrpcDone<::mailpb::SendMailResponse> done, const GrpcCallOptions& options) {
    StartMailAdminSendSystemMailCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMailAdminSendSystemMailErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.SendSystemMail requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::SendMailRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MailAdmin.SendSystemMail: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::SendMailRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MailAdmin.SendSystemMail: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMailAdminSendSystemMailCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::SendMailResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MailAdminSendGuildMail
namespace {
void QueueLocalMailAdminSendGuildMailFailure(GrpcDone<::mailpb::SendMailResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMailAdminSendGuildMailCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::SendMailRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::SendMailResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.SendGuildMail requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMailAdminSendGuildMailGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMailAdminSendGuildMailFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMailAdminSendGuildMailFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::SendMailResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MailAdmin.SendGuildMail response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMailAdminSendGuildMail(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::SendMailRequest& request,
                                            GrpcDone<::mailpb::SendMailResponse> done, const GrpcCallOptions& options) {
    StartMailAdminSendGuildMailCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMailAdminSendGuildMailErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.SendGuildMail requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::SendMailRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MailAdmin.SendGuildMail: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::SendMailRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MailAdmin.SendGuildMail: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMailAdminSendGuildMailCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::SendMailResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MailAdminImportMailRecipients
namespace {
void QueueLocalMailAdminImportMailRecipientsFailure(GrpcDone<::mailpb::ImportMailRecipientsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMailAdminImportMailRecipientsCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ImportMailRecipientsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::ImportMailRecipientsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.ImportMailRecipients requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMailAdminImportMailRecipientsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMailAdminImportMailRecipientsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMailAdminImportMailRecipientsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::ImportMailRecipientsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MailAdmin.ImportMailRecipients response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMailAdminImportMailRecipients(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::ImportMailRecipientsRequest& request,
                                            GrpcDone<::mailpb::ImportMailRecipientsResponse> done, const GrpcCallOptions& options) {
    StartMailAdminImportMailRecipientsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMailAdminImportMailRecipientsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.ImportMailRecipients requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::ImportMailRecipientsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MailAdmin.ImportMailRecipients: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::ImportMailRecipientsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MailAdmin.ImportMailRecipients: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMailAdminImportMailRecipientsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::ImportMailRecipientsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region MailAdminSealMailCampaign
namespace {
void QueueLocalMailAdminSealMailCampaignFailure(GrpcDone<::mailpb::SendMailResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartMailAdminSealMailCampaignCall(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::SealMailCampaignRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::mailpb::SendMailResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.SealMailCampaign requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kMailAdminSealMailCampaignGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalMailAdminSealMailCampaignFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalMailAdminSealMailCampaignFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::mailpb::SendMailResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the MailAdmin.SealMailCampaign response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendMailAdminSealMailCampaign(entt::registry& registry, entt::entity nodeEntity, const ::mailpb::SealMailCampaignRequest& request,
                                            GrpcDone<::mailpb::SendMailResponse> done, const GrpcCallOptions& options) {
    StartMailAdminSealMailCampaignCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendMailAdminSealMailCampaignErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC MailAdmin.SealMailCampaign requires a done callback";
    }
    if (message.GetDescriptor() != ::mailpb::SealMailCampaignRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC MailAdmin.SealMailCampaign: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::mailpb::SealMailCampaignRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC MailAdmin.SealMailCampaign: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartMailAdminSealMailCampaignCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::mailpb::SendMailResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetMailAdminCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace mailpb
