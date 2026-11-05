#include "muduo/base/Logging.h"

#include "guild_grpc_client.h"
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

namespace guildpb {
#pragma region GuildServiceCreateGuild
namespace {
void QueueLocalGuildServiceCreateGuildFailure(GrpcDone<::guildpb::CreateGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceCreateGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::CreateGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.CreateGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceCreateGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceCreateGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceCreateGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::CreateGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.CreateGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request,
                                            GrpcDone<::guildpb::CreateGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceCreateGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceCreateGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.CreateGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::CreateGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.CreateGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::CreateGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.CreateGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceCreateGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::CreateGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceGetGuild
namespace {
void QueueLocalGuildServiceGetGuildFailure(GrpcDone<::guildpb::GetGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceGetGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::GetGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceGetGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceGetGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceGetGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::GetGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.GetGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request,
                                            GrpcDone<::guildpb::GetGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceGetGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceGetGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::GetGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.GetGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::GetGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.GetGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceGetGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::GetGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceGetPlayerGuild
namespace {
void QueueLocalGuildServiceGetPlayerGuildFailure(GrpcDone<::guildpb::GetPlayerGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceGetPlayerGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::GetPlayerGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetPlayerGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceGetPlayerGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceGetPlayerGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceGetPlayerGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::GetPlayerGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.GetPlayerGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request,
                                            GrpcDone<::guildpb::GetPlayerGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceGetPlayerGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceGetPlayerGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetPlayerGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::GetPlayerGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.GetPlayerGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::GetPlayerGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.GetPlayerGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceGetPlayerGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::GetPlayerGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceLeaveGuild
namespace {
void QueueLocalGuildServiceLeaveGuildFailure(GrpcDone<::guildpb::LeaveGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceLeaveGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::LeaveGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.LeaveGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceLeaveGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceLeaveGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceLeaveGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::LeaveGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.LeaveGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request,
                                            GrpcDone<::guildpb::LeaveGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceLeaveGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceLeaveGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.LeaveGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::LeaveGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.LeaveGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::LeaveGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.LeaveGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceLeaveGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::LeaveGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceDisbandGuild
namespace {
void QueueLocalGuildServiceDisbandGuildFailure(GrpcDone<::guildpb::DisbandGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceDisbandGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::DisbandGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.DisbandGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceDisbandGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceDisbandGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceDisbandGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::DisbandGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.DisbandGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request,
                                            GrpcDone<::guildpb::DisbandGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceDisbandGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceDisbandGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.DisbandGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::DisbandGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.DisbandGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::DisbandGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.DisbandGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceDisbandGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::DisbandGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceSetAnnouncement
namespace {
void QueueLocalGuildServiceSetAnnouncementFailure(GrpcDone<::guildpb::SetAnnouncementResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceSetAnnouncementCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::SetAnnouncementResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.SetAnnouncement requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceSetAnnouncementGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceSetAnnouncementFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceSetAnnouncementFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::SetAnnouncementResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.SetAnnouncement response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request,
                                            GrpcDone<::guildpb::SetAnnouncementResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceSetAnnouncementCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceSetAnnouncementErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.SetAnnouncement requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::SetAnnouncementRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.SetAnnouncement: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::SetAnnouncementRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.SetAnnouncement: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceSetAnnouncementCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::SetAnnouncementResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceSetGuildMemberRole
namespace {
void QueueLocalGuildServiceSetGuildMemberRoleFailure(GrpcDone<::guildpb::SetGuildMemberRoleResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceSetGuildMemberRoleCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::SetGuildMemberRoleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.SetGuildMemberRole requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceSetGuildMemberRoleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceSetGuildMemberRoleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceSetGuildMemberRoleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::SetGuildMemberRoleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.SetGuildMemberRole response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request,
                                            GrpcDone<::guildpb::SetGuildMemberRoleResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceSetGuildMemberRoleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceSetGuildMemberRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.SetGuildMemberRole requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::SetGuildMemberRoleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.SetGuildMemberRole: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::SetGuildMemberRoleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.SetGuildMemberRole: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceSetGuildMemberRoleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::SetGuildMemberRoleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceKickGuildMember
namespace {
void QueueLocalGuildServiceKickGuildMemberFailure(GrpcDone<::guildpb::KickGuildMemberResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceKickGuildMemberCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::KickGuildMemberResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.KickGuildMember requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceKickGuildMemberGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceKickGuildMemberFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceKickGuildMemberFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::KickGuildMemberResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.KickGuildMember response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request,
                                            GrpcDone<::guildpb::KickGuildMemberResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceKickGuildMemberCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceKickGuildMemberErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.KickGuildMember requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::KickGuildMemberRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.KickGuildMember: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::KickGuildMemberRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.KickGuildMember: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceKickGuildMemberCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::KickGuildMemberResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceTransferGuildLeader
namespace {
void QueueLocalGuildServiceTransferGuildLeaderFailure(GrpcDone<::guildpb::TransferGuildLeaderResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceTransferGuildLeaderCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::TransferGuildLeaderResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.TransferGuildLeader requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceTransferGuildLeaderGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceTransferGuildLeaderFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceTransferGuildLeaderFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::TransferGuildLeaderResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.TransferGuildLeader response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request,
                                            GrpcDone<::guildpb::TransferGuildLeaderResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceTransferGuildLeaderCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceTransferGuildLeaderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.TransferGuildLeader requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::TransferGuildLeaderRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.TransferGuildLeader: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::TransferGuildLeaderRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.TransferGuildLeader: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceTransferGuildLeaderCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::TransferGuildLeaderResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceApplyJoinGuild
namespace {
void QueueLocalGuildServiceApplyJoinGuildFailure(GrpcDone<::guildpb::ApplyJoinGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceApplyJoinGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::ApplyJoinGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ApplyJoinGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceApplyJoinGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceApplyJoinGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceApplyJoinGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::ApplyJoinGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.ApplyJoinGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request,
                                            GrpcDone<::guildpb::ApplyJoinGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceApplyJoinGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceApplyJoinGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ApplyJoinGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::ApplyJoinGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.ApplyJoinGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::ApplyJoinGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.ApplyJoinGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceApplyJoinGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::ApplyJoinGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceCancelGuildApplication
namespace {
void QueueLocalGuildServiceCancelGuildApplicationFailure(GrpcDone<::guildpb::CancelGuildApplicationResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceCancelGuildApplicationCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::CancelGuildApplicationResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.CancelGuildApplication requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceCancelGuildApplicationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceCancelGuildApplicationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceCancelGuildApplicationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::CancelGuildApplicationResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.CancelGuildApplication response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request,
                                            GrpcDone<::guildpb::CancelGuildApplicationResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceCancelGuildApplicationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceCancelGuildApplicationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.CancelGuildApplication requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::CancelGuildApplicationRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.CancelGuildApplication: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::CancelGuildApplicationRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.CancelGuildApplication: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceCancelGuildApplicationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::CancelGuildApplicationResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceListMyGuildApplications
namespace {
void QueueLocalGuildServiceListMyGuildApplicationsFailure(GrpcDone<::guildpb::ListMyGuildApplicationsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceListMyGuildApplicationsCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::ListMyGuildApplicationsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ListMyGuildApplications requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceListMyGuildApplicationsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceListMyGuildApplicationsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceListMyGuildApplicationsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::ListMyGuildApplicationsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.ListMyGuildApplications response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request,
                                            GrpcDone<::guildpb::ListMyGuildApplicationsResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceListMyGuildApplicationsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceListMyGuildApplicationsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ListMyGuildApplications requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::ListMyGuildApplicationsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.ListMyGuildApplications: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::ListMyGuildApplicationsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.ListMyGuildApplications: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceListMyGuildApplicationsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::ListMyGuildApplicationsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceListGuildApplications
namespace {
void QueueLocalGuildServiceListGuildApplicationsFailure(GrpcDone<::guildpb::ListGuildApplicationsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceListGuildApplicationsCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::ListGuildApplicationsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ListGuildApplications requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceListGuildApplicationsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceListGuildApplicationsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceListGuildApplicationsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::ListGuildApplicationsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.ListGuildApplications response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request,
                                            GrpcDone<::guildpb::ListGuildApplicationsResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceListGuildApplicationsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceListGuildApplicationsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ListGuildApplications requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::ListGuildApplicationsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.ListGuildApplications: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::ListGuildApplicationsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.ListGuildApplications: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceListGuildApplicationsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::ListGuildApplicationsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceReviewGuildApplication
namespace {
void QueueLocalGuildServiceReviewGuildApplicationFailure(GrpcDone<::guildpb::ReviewGuildApplicationResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceReviewGuildApplicationCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::ReviewGuildApplicationResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ReviewGuildApplication requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceReviewGuildApplicationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceReviewGuildApplicationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceReviewGuildApplicationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::ReviewGuildApplicationResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.ReviewGuildApplication response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request,
                                            GrpcDone<::guildpb::ReviewGuildApplicationResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceReviewGuildApplicationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceReviewGuildApplicationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ReviewGuildApplication requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::ReviewGuildApplicationRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.ReviewGuildApplication: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::ReviewGuildApplicationRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.ReviewGuildApplication: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceReviewGuildApplicationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::ReviewGuildApplicationResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceNotifyGuildChanged
namespace {
void QueueLocalGuildServiceNotifyGuildChangedFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceNotifyGuildChangedCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.NotifyGuildChanged requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceNotifyGuildChangedGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceNotifyGuildChangedFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceNotifyGuildChangedFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the GuildService.NotifyGuildChanged response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartGuildServiceNotifyGuildChangedCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceNotifyGuildChangedErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.NotifyGuildChanged requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::GuildChangedS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.NotifyGuildChanged: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::GuildChangedS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.NotifyGuildChanged: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceNotifyGuildChangedCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceUpdateGuildScore
namespace {
void QueueLocalGuildServiceUpdateGuildScoreFailure(GrpcDone<::guildpb::UpdateGuildScoreResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceUpdateGuildScoreCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::UpdateGuildScoreResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.UpdateGuildScore requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceUpdateGuildScoreGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceUpdateGuildScoreFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceUpdateGuildScoreFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::UpdateGuildScoreResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.UpdateGuildScore response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request,
                                            GrpcDone<::guildpb::UpdateGuildScoreResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceUpdateGuildScoreCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceUpdateGuildScoreErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.UpdateGuildScore requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::UpdateGuildScoreRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.UpdateGuildScore: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::UpdateGuildScoreRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.UpdateGuildScore: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceUpdateGuildScoreCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::UpdateGuildScoreResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceGetGuildRank
namespace {
void QueueLocalGuildServiceGetGuildRankFailure(GrpcDone<::guildpb::GetGuildRankResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceGetGuildRankCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::GetGuildRankResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildRank requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceGetGuildRankGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceGetGuildRankFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceGetGuildRankFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::GetGuildRankResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.GetGuildRank response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request,
                                            GrpcDone<::guildpb::GetGuildRankResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceGetGuildRankCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceGetGuildRankErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildRank requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::GetGuildRankRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.GetGuildRank: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::GetGuildRankRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.GetGuildRank: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceGetGuildRankCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::GetGuildRankResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceGetGuildRankByGuild
namespace {
void QueueLocalGuildServiceGetGuildRankByGuildFailure(GrpcDone<::guildpb::GetGuildRankByGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceGetGuildRankByGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::GetGuildRankByGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildRankByGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceGetGuildRankByGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceGetGuildRankByGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceGetGuildRankByGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::GetGuildRankByGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.GetGuildRankByGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request,
                                            GrpcDone<::guildpb::GetGuildRankByGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceGetGuildRankByGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceGetGuildRankByGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildRankByGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::GetGuildRankByGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.GetGuildRankByGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::GetGuildRankByGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.GetGuildRankByGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceGetGuildRankByGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::GetGuildRankByGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceGetGuildDonateOptions
namespace {
void QueueLocalGuildServiceGetGuildDonateOptionsFailure(GrpcDone<::guildpb::GetGuildDonateOptionsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceGetGuildDonateOptionsCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildDonateOptionsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::GetGuildDonateOptionsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildDonateOptions requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceGetGuildDonateOptionsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceGetGuildDonateOptionsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceGetGuildDonateOptionsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::GetGuildDonateOptionsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.GetGuildDonateOptions response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceGetGuildDonateOptions(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildDonateOptionsRequest& request,
                                            GrpcDone<::guildpb::GetGuildDonateOptionsResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceGetGuildDonateOptionsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceGetGuildDonateOptionsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildDonateOptions requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::GetGuildDonateOptionsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.GetGuildDonateOptions: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::GetGuildDonateOptionsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.GetGuildDonateOptions: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceGetGuildDonateOptionsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::GetGuildDonateOptionsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceDonateToGuild
namespace {
void QueueLocalGuildServiceDonateToGuildFailure(GrpcDone<::guildpb::DonateToGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceDonateToGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DonateToGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::DonateToGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.DonateToGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceDonateToGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceDonateToGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceDonateToGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::DonateToGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.DonateToGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceDonateToGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DonateToGuildRequest& request,
                                            GrpcDone<::guildpb::DonateToGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceDonateToGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceDonateToGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.DonateToGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::DonateToGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.DonateToGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::DonateToGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.DonateToGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceDonateToGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::DonateToGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceUpgradeGuild
namespace {
void QueueLocalGuildServiceUpgradeGuildFailure(GrpcDone<::guildpb::UpgradeGuildResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceUpgradeGuildCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpgradeGuildRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::UpgradeGuildResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.UpgradeGuild requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceUpgradeGuildGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceUpgradeGuildFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceUpgradeGuildFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::UpgradeGuildResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.UpgradeGuild response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceUpgradeGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpgradeGuildRequest& request,
                                            GrpcDone<::guildpb::UpgradeGuildResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceUpgradeGuildCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceUpgradeGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.UpgradeGuild requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::UpgradeGuildRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.UpgradeGuild: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::UpgradeGuildRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.UpgradeGuild: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceUpgradeGuildCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::UpgradeGuildResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceGetGuildShop
namespace {
void QueueLocalGuildServiceGetGuildShopFailure(GrpcDone<::guildpb::GetGuildShopResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceGetGuildShopCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildShopRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::GetGuildShopResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildShop requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceGetGuildShopGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceGetGuildShopFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceGetGuildShopFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::GetGuildShopResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.GetGuildShop response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceGetGuildShop(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildShopRequest& request,
                                            GrpcDone<::guildpb::GetGuildShopResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceGetGuildShopCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceGetGuildShopErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildShop requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::GetGuildShopRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.GetGuildShop: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::GetGuildShopRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.GetGuildShop: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceGetGuildShopCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::GetGuildShopResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceBuyGuildShopGoods
namespace {
void QueueLocalGuildServiceBuyGuildShopGoodsFailure(GrpcDone<::guildpb::BuyGuildShopGoodsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceBuyGuildShopGoodsCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::BuyGuildShopGoodsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::BuyGuildShopGoodsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.BuyGuildShopGoods requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceBuyGuildShopGoodsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceBuyGuildShopGoodsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceBuyGuildShopGoodsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::BuyGuildShopGoodsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.BuyGuildShopGoods response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceBuyGuildShopGoods(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::BuyGuildShopGoodsRequest& request,
                                            GrpcDone<::guildpb::BuyGuildShopGoodsResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceBuyGuildShopGoodsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceBuyGuildShopGoodsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.BuyGuildShopGoods requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::BuyGuildShopGoodsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.BuyGuildShopGoods: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::BuyGuildShopGoodsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.BuyGuildShopGoods: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceBuyGuildShopGoodsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::BuyGuildShopGoodsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceGetGuildActivities
namespace {
void QueueLocalGuildServiceGetGuildActivitiesFailure(GrpcDone<::guildpb::GetGuildActivitiesResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceGetGuildActivitiesCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildActivitiesRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::GetGuildActivitiesResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildActivities requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceGetGuildActivitiesGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceGetGuildActivitiesFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceGetGuildActivitiesFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::GetGuildActivitiesResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.GetGuildActivities response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceGetGuildActivities(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildActivitiesRequest& request,
                                            GrpcDone<::guildpb::GetGuildActivitiesResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceGetGuildActivitiesCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceGetGuildActivitiesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.GetGuildActivities requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::GetGuildActivitiesRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.GetGuildActivities: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::GetGuildActivitiesRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.GetGuildActivities: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceGetGuildActivitiesCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::GetGuildActivitiesResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceLightGuildLantern
namespace {
void QueueLocalGuildServiceLightGuildLanternFailure(GrpcDone<::guildpb::LightGuildLanternResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceLightGuildLanternCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LightGuildLanternRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::LightGuildLanternResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.LightGuildLantern requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceLightGuildLanternGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceLightGuildLanternFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceLightGuildLanternFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::LightGuildLanternResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.LightGuildLantern response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceLightGuildLantern(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LightGuildLanternRequest& request,
                                            GrpcDone<::guildpb::LightGuildLanternResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceLightGuildLanternCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceLightGuildLanternErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.LightGuildLantern requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::LightGuildLanternRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.LightGuildLantern: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::LightGuildLanternRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.LightGuildLantern: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceLightGuildLanternCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::LightGuildLanternResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceClaimGuildReunion
namespace {
void QueueLocalGuildServiceClaimGuildReunionFailure(GrpcDone<::guildpb::ClaimGuildReunionResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceClaimGuildReunionCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ClaimGuildReunionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::ClaimGuildReunionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ClaimGuildReunion requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceClaimGuildReunionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceClaimGuildReunionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceClaimGuildReunionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::ClaimGuildReunionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.ClaimGuildReunion response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceClaimGuildReunion(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ClaimGuildReunionRequest& request,
                                            GrpcDone<::guildpb::ClaimGuildReunionResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceClaimGuildReunionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceClaimGuildReunionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.ClaimGuildReunion requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::ClaimGuildReunionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.ClaimGuildReunion: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::ClaimGuildReunionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.ClaimGuildReunion: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceClaimGuildReunionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::ClaimGuildReunionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceStartGuildTrial
namespace {
void QueueLocalGuildServiceStartGuildTrialFailure(GrpcDone<::guildpb::StartGuildTrialResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceStartGuildTrialCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::StartGuildTrialRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::StartGuildTrialResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.StartGuildTrial requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceStartGuildTrialGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceStartGuildTrialFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceStartGuildTrialFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::StartGuildTrialResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.StartGuildTrial response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceStartGuildTrial(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::StartGuildTrialRequest& request,
                                            GrpcDone<::guildpb::StartGuildTrialResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceStartGuildTrialCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceStartGuildTrialErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.StartGuildTrial requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::StartGuildTrialRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.StartGuildTrial: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::StartGuildTrialRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.StartGuildTrial: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceStartGuildTrialCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::StartGuildTrialResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region GuildServiceRespondGuildTrialInvite
namespace {
void QueueLocalGuildServiceRespondGuildTrialInviteFailure(GrpcDone<::guildpb::RespondGuildTrialInviteResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartGuildServiceRespondGuildTrialInviteCall(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::RespondGuildTrialInviteRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::guildpb::RespondGuildTrialInviteResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.RespondGuildTrialInvite requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kGuildServiceRespondGuildTrialInviteGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalGuildServiceRespondGuildTrialInviteFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalGuildServiceRespondGuildTrialInviteFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::guildpb::RespondGuildTrialInviteResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the GuildService.RespondGuildTrialInvite response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendGuildServiceRespondGuildTrialInvite(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::RespondGuildTrialInviteRequest& request,
                                            GrpcDone<::guildpb::RespondGuildTrialInviteResponse> done, const GrpcCallOptions& options) {
    StartGuildServiceRespondGuildTrialInviteCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendGuildServiceRespondGuildTrialInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC GuildService.RespondGuildTrialInvite requires a done callback";
    }
    if (message.GetDescriptor() != ::guildpb::RespondGuildTrialInviteRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC GuildService.RespondGuildTrialInvite: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::guildpb::RespondGuildTrialInviteRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC GuildService.RespondGuildTrialInvite: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartGuildServiceRespondGuildTrialInviteCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::guildpb::RespondGuildTrialInviteResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetGuildCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace guildpb
