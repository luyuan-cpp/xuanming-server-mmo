#include "muduo/base/Logging.h"

#include "login_admin_grpc_client.h"
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

namespace loginpb {
#pragma region LoginAdminRemovePlayersFromAccounts
namespace {
void QueueLocalLoginAdminRemovePlayersFromAccountsFailure(GrpcDone<::loginpb::RemovePlayersFromAccountsResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginAdminRemovePlayersFromAccountsGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginAdminRemovePlayersFromAccountsCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::RemovePlayersFromAccountsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.RemovePlayersFromAccounts requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginAdminRemovePlayersFromAccountsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginAdminRemovePlayersFromAccountsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginAdminRemovePlayersFromAccountsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::RemovePlayersFromAccountsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginAdmin.RemovePlayersFromAccounts response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request,
                                            GrpcDone<::loginpb::RemovePlayersFromAccountsResponse> done, const GrpcCallOptions& options) {
    StartLoginAdminRemovePlayersFromAccountsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginAdminRemovePlayersFromAccountsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.RemovePlayersFromAccounts requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::RemovePlayersFromAccountsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginAdmin.RemovePlayersFromAccounts: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminRemovePlayersFromAccountsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::RemovePlayersFromAccountsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginAdmin.RemovePlayersFromAccounts: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminRemovePlayersFromAccountsGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginAdminRemovePlayersFromAccountsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::RemovePlayersFromAccountsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginAdminScheduleAccountDeletion
namespace {
void QueueLocalLoginAdminScheduleAccountDeletionFailure(GrpcDone<::loginpb::AccountDeletionResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginAdminScheduleAccountDeletionGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginAdminScheduleAccountDeletionCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ScheduleAccountDeletionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::AccountDeletionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.ScheduleAccountDeletion requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginAdminScheduleAccountDeletionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginAdminScheduleAccountDeletionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginAdminScheduleAccountDeletionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::AccountDeletionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginAdmin.ScheduleAccountDeletion response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginAdminScheduleAccountDeletion(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ScheduleAccountDeletionRequest& request,
                                            GrpcDone<::loginpb::AccountDeletionResponse> done, const GrpcCallOptions& options) {
    StartLoginAdminScheduleAccountDeletionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginAdminScheduleAccountDeletionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.ScheduleAccountDeletion requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::ScheduleAccountDeletionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginAdmin.ScheduleAccountDeletion: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminScheduleAccountDeletionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::ScheduleAccountDeletionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginAdmin.ScheduleAccountDeletion: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminScheduleAccountDeletionGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginAdminScheduleAccountDeletionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::AccountDeletionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LoginAdminResetAccountRealName
namespace {
void QueueLocalLoginAdminResetAccountRealNameFailure(GrpcDone<::loginpb::ResetAccountRealNameResponse> done, uint32_t code, std::string message) {
    RecordGrpcLocalFailureSpan(kLoginAdminResetAccountRealNameGrpcPath, code, message);
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLoginAdminResetAccountRealNameCall(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ResetAccountRealNameRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::loginpb::ResetAccountRealNameResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.ResetAccountRealName requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLoginAdminResetAccountRealNameGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLoginAdminResetAccountRealNameFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLoginAdminResetAccountRealNameFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::loginpb::ResetAccountRealNameResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the LoginAdmin.ResetAccountRealName response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLoginAdminResetAccountRealName(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ResetAccountRealNameRequest& request,
                                            GrpcDone<::loginpb::ResetAccountRealNameResponse> done, const GrpcCallOptions& options) {
    StartLoginAdminResetAccountRealNameCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLoginAdminResetAccountRealNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC LoginAdmin.ResetAccountRealName requires a done callback";
    }
    if (message.GetDescriptor() != ::loginpb::ResetAccountRealNameRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC LoginAdmin.ResetAccountRealName: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminResetAccountRealNameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::loginpb::ResetAccountRealNameRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC LoginAdmin.ResetAccountRealName: request C++ type does not match; not sent";
        RecordGrpcLocalFailureSpan(kLoginAdminResetAccountRealNameGrpcPath, kGrpcStatusInternal, "request type mismatch");
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLoginAdminResetAccountRealNameCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::loginpb::ResetAccountRealNameResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetLoginAdminCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace loginpb
