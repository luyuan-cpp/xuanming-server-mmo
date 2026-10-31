#include "muduo/base/Logging.h"

#include "player_store_grpc_client.h"
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

namespace playerstore {
#pragma region PlayerStoreLoadAndClaim
namespace {
void QueueLocalPlayerStoreLoadAndClaimFailure(GrpcDone<::playerstore::LoadAndClaimResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerStoreLoadAndClaimCall(entt::registry& registry, entt::entity nodeEntity, const ::playerstore::LoadAndClaimRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::playerstore::LoadAndClaimResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerStore.LoadAndClaim requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerStoreLoadAndClaimGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerStoreLoadAndClaimFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerStoreLoadAndClaimFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::playerstore::LoadAndClaimResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerStore.LoadAndClaim response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerStoreLoadAndClaim(entt::registry& registry, entt::entity nodeEntity, const ::playerstore::LoadAndClaimRequest& request,
                                            GrpcDone<::playerstore::LoadAndClaimResponse> done, const GrpcCallOptions& options) {
    StartPlayerStoreLoadAndClaimCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerStoreLoadAndClaimErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerStore.LoadAndClaim requires a done callback";
    }
    if (message.GetDescriptor() != ::playerstore::LoadAndClaimRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerStore.LoadAndClaim: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerstore::LoadAndClaimRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerStore.LoadAndClaim: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerStoreLoadAndClaimCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::playerstore::LoadAndClaimResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region PlayerStoreSavePlayer
namespace {
void QueueLocalPlayerStoreSavePlayerFailure(GrpcDone<::playerstore::SavePlayerResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartPlayerStoreSavePlayerCall(entt::registry& registry, entt::entity nodeEntity, const ::playerstore::SavePlayerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::playerstore::SavePlayerResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerStore.SavePlayer requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kPlayerStoreSavePlayerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalPlayerStoreSavePlayerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalPlayerStoreSavePlayerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::playerstore::SavePlayerResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the PlayerStore.SavePlayer response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendPlayerStoreSavePlayer(entt::registry& registry, entt::entity nodeEntity, const ::playerstore::SavePlayerRequest& request,
                                            GrpcDone<::playerstore::SavePlayerResponse> done, const GrpcCallOptions& options) {
    StartPlayerStoreSavePlayerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendPlayerStoreSavePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC PlayerStore.SavePlayer requires a done callback";
    }
    if (message.GetDescriptor() != ::playerstore::SavePlayerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC PlayerStore.SavePlayer: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::playerstore::SavePlayerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC PlayerStore.SavePlayer: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartPlayerStoreSavePlayerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::playerstore::SavePlayerResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetPlayerStoreCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace playerstore
