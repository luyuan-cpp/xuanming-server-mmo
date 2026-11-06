#include "muduo/base/Logging.h"

#include "player_inbox_admin_grpc_client.h"
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

namespace player_inbox {
#pragma region PlayerInboxAdminGmSanctionPlayer
namespace {
void QueueLocalPlayerInboxAdminGmSanctionPlayerFailure(GrpcDone<::player_inbox::GmSanctionPlayerResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxAdminGmSanctionPlayerCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GmSanctionPlayerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::GmSanctionPlayerResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.GmSanctionPlayer requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxAdminGmSanctionPlayerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxAdminGmSanctionPlayerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxAdminGmSanctionPlayerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::GmSanctionPlayerResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInboxAdmin.GmSanctionPlayer response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxAdminGmSanctionPlayer(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GmSanctionPlayerRequest& request,
                                            GrpcDone<::player_inbox::GmSanctionPlayerResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxAdminGmSanctionPlayerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxAdminGmSanctionPlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.GmSanctionPlayer requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::GmSanctionPlayerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInboxAdmin.GmSanctionPlayer: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::GmSanctionPlayerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInboxAdmin.GmSanctionPlayer: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxAdminGmSanctionPlayerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::GmSanctionPlayerResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxAdminGetSummary
namespace {
void QueueLocalPlayerInboxAdminGetSummaryFailure(GrpcDone<::player_inbox::GetPlayerInboxSummaryResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxAdminGetSummaryCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GetPlayerInboxSummaryRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::GetPlayerInboxSummaryResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.GetSummary requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxAdminGetSummaryGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxAdminGetSummaryFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxAdminGetSummaryFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::GetPlayerInboxSummaryResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInboxAdmin.GetSummary response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxAdminGetSummary(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GetPlayerInboxSummaryRequest& request,
                                            GrpcDone<::player_inbox::GetPlayerInboxSummaryResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxAdminGetSummaryCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxAdminGetSummaryErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.GetSummary requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::GetPlayerInboxSummaryRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInboxAdmin.GetSummary: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::GetPlayerInboxSummaryRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInboxAdmin.GetSummary: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxAdminGetSummaryCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::GetPlayerInboxSummaryResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxAdminListItems
namespace {
void QueueLocalPlayerInboxAdminListItemsFailure(GrpcDone<::player_inbox::ListPlayerInboxItemsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxAdminListItemsCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::ListPlayerInboxItemsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::ListPlayerInboxItemsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.ListItems requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxAdminListItemsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxAdminListItemsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxAdminListItemsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::ListPlayerInboxItemsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInboxAdmin.ListItems response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxAdminListItems(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::ListPlayerInboxItemsRequest& request,
                                            GrpcDone<::player_inbox::ListPlayerInboxItemsResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxAdminListItemsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxAdminListItemsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.ListItems requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::ListPlayerInboxItemsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInboxAdmin.ListItems: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::ListPlayerInboxItemsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInboxAdmin.ListItems: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxAdminListItemsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::ListPlayerInboxItemsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxAdminLookupByRequest
namespace {
void QueueLocalPlayerInboxAdminLookupByRequestFailure(GrpcDone<::player_inbox::LookupPlayerInboxResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxAdminLookupByRequestCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::LookupPlayerInboxRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::LookupPlayerInboxResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.LookupByRequest requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxAdminLookupByRequestGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxAdminLookupByRequestFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxAdminLookupByRequestFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::LookupPlayerInboxResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInboxAdmin.LookupByRequest response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxAdminLookupByRequest(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::LookupPlayerInboxRequest& request,
                                            GrpcDone<::player_inbox::LookupPlayerInboxResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxAdminLookupByRequestCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxAdminLookupByRequestErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.LookupByRequest requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::LookupPlayerInboxRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInboxAdmin.LookupByRequest: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::LookupPlayerInboxRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInboxAdmin.LookupByRequest: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxAdminLookupByRequestCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::LookupPlayerInboxResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxAdminVoidItem
namespace {
void QueueLocalPlayerInboxAdminVoidItemFailure(GrpcDone<::player_inbox::VoidPlayerInboxItemResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxAdminVoidItemCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::VoidPlayerInboxItemRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::VoidPlayerInboxItemResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.VoidItem requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxAdminVoidItemGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxAdminVoidItemFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxAdminVoidItemFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::VoidPlayerInboxItemResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInboxAdmin.VoidItem response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxAdminVoidItem(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::VoidPlayerInboxItemRequest& request,
                                            GrpcDone<::player_inbox::VoidPlayerInboxItemResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxAdminVoidItemCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxAdminVoidItemErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.VoidItem requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::VoidPlayerInboxItemRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInboxAdmin.VoidItem: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::VoidPlayerInboxItemRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInboxAdmin.VoidItem: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxAdminVoidItemCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::VoidPlayerInboxItemResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxAdminReconcilePlayer
namespace {
void QueueLocalPlayerInboxAdminReconcilePlayerFailure(GrpcDone<::player_inbox::ReconcilePlayerInboxResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxAdminReconcilePlayerCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::ReconcilePlayerInboxRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::ReconcilePlayerInboxResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.ReconcilePlayer requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxAdminReconcilePlayerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxAdminReconcilePlayerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxAdminReconcilePlayerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::ReconcilePlayerInboxResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInboxAdmin.ReconcilePlayer response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxAdminReconcilePlayer(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::ReconcilePlayerInboxRequest& request,
                                            GrpcDone<::player_inbox::ReconcilePlayerInboxResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxAdminReconcilePlayerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxAdminReconcilePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.ReconcilePlayer requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::ReconcilePlayerInboxRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInboxAdmin.ReconcilePlayer: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::ReconcilePlayerInboxRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInboxAdmin.ReconcilePlayer: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxAdminReconcilePlayerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::ReconcilePlayerInboxResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxAdminGetItemGainBlocks
namespace {
void QueueLocalPlayerInboxAdminGetItemGainBlocksFailure(GrpcDone<::player_inbox::GetPlayerItemGainBlocksResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxAdminGetItemGainBlocksCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GetPlayerItemGainBlocksRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::GetPlayerItemGainBlocksResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.GetItemGainBlocks requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxAdminGetItemGainBlocksGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxAdminGetItemGainBlocksFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxAdminGetItemGainBlocksFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::GetPlayerItemGainBlocksResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInboxAdmin.GetItemGainBlocks response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxAdminGetItemGainBlocks(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GetPlayerItemGainBlocksRequest& request,
                                            GrpcDone<::player_inbox::GetPlayerItemGainBlocksResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxAdminGetItemGainBlocksCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxAdminGetItemGainBlocksErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInboxAdmin.GetItemGainBlocks requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::GetPlayerItemGainBlocksRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInboxAdmin.GetItemGainBlocks: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::GetPlayerItemGainBlocksRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInboxAdmin.GetItemGainBlocks: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxAdminGetItemGainBlocksCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::GetPlayerItemGainBlocksResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetPlayerInboxAdminCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace player_inbox
