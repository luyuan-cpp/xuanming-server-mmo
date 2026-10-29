#include "muduo/base/Logging.h"

#include "player_inbox_grpc_client.h"
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
#pragma region PlayerInboxFetchItems
namespace {
void QueueLocalPlayerInboxFetchItemsFailure(GrpcDone<::player_inbox::FetchPlayerInboxResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxFetchItemsCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::FetchPlayerInboxRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::FetchPlayerInboxResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInbox.FetchItems requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxFetchItemsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxFetchItemsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxFetchItemsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::FetchPlayerInboxResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInbox.FetchItems response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxFetchItems(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::FetchPlayerInboxRequest& request,
                                            GrpcDone<::player_inbox::FetchPlayerInboxResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxFetchItemsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxFetchItemsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInbox.FetchItems requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::FetchPlayerInboxRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInbox.FetchItems: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::FetchPlayerInboxRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInbox.FetchItems: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxFetchItemsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::FetchPlayerInboxResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxAckItems
namespace {
void QueueLocalPlayerInboxAckItemsFailure(GrpcDone<::player_inbox::AckPlayerInboxResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxAckItemsCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::AckPlayerInboxRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::AckPlayerInboxResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInbox.AckItems requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxAckItemsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxAckItemsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxAckItemsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::AckPlayerInboxResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInbox.AckItems response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxAckItems(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::AckPlayerInboxRequest& request,
                                            GrpcDone<::player_inbox::AckPlayerInboxResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxAckItemsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxAckItemsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInbox.AckItems requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::AckPlayerInboxRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInbox.AckItems: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::AckPlayerInboxRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInbox.AckItems: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxAckItemsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::AckPlayerInboxResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxEnqueueBattleSettlement
namespace {
void QueueLocalPlayerInboxEnqueueBattleSettlementFailure(GrpcDone<::player_inbox::BattleSettlementInboxResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxEnqueueBattleSettlementCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::BattleSettlementInboxRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::BattleSettlementInboxResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInbox.EnqueueBattleSettlement requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxEnqueueBattleSettlementGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxEnqueueBattleSettlementFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxEnqueueBattleSettlementFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::BattleSettlementInboxResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInbox.EnqueueBattleSettlement response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxEnqueueBattleSettlement(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::BattleSettlementInboxRequest& request,
                                            GrpcDone<::player_inbox::BattleSettlementInboxResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxEnqueueBattleSettlementCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxEnqueueBattleSettlementErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInbox.EnqueueBattleSettlement requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::BattleSettlementInboxRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInbox.EnqueueBattleSettlement: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::BattleSettlementInboxRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInbox.EnqueueBattleSettlement: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxEnqueueBattleSettlementCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::BattleSettlementInboxResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerInboxLookupBattleSettlement
namespace {
void QueueLocalPlayerInboxLookupBattleSettlementFailure(GrpcDone<::player_inbox::BattleSettlementInboxResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerInboxLookupBattleSettlementCall(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::BattleSettlementInboxRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::player_inbox::BattleSettlementInboxResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInbox.LookupBattleSettlement requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerInboxLookupBattleSettlementGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerInboxLookupBattleSettlementFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerInboxLookupBattleSettlementFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::player_inbox::BattleSettlementInboxResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerInbox.LookupBattleSettlement response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerInboxLookupBattleSettlement(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::BattleSettlementInboxRequest& request,
                                            GrpcDone<::player_inbox::BattleSettlementInboxResponse> done, const GrpcCallOptions& options) {
    StartPlayerInboxLookupBattleSettlementCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerInboxLookupBattleSettlementErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerInbox.LookupBattleSettlement requires a done callback";
    }
    if (message.GetDescriptor() != ::player_inbox::BattleSettlementInboxRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerInbox.LookupBattleSettlement: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::player_inbox::BattleSettlementInboxRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerInbox.LookupBattleSettlement: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerInboxLookupBattleSettlementCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::player_inbox::BattleSettlementInboxResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetPlayerInboxCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace player_inbox
