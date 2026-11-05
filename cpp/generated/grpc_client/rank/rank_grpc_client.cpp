#include "muduo/base/Logging.h"

#include "rank_grpc_client.h"
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

namespace rankpb {
#pragma region ClientPlayerRankGetRankPage
namespace {
void QueueLocalClientPlayerRankGetRankPageFailure(GrpcDone<::rankpb::GetRankPageResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerRankGetRankPageCall(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::GetRankPageRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::rankpb::GetRankPageResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerRank.GetRankPage requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerRankGetRankPageGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerRankGetRankPageFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerRankGetRankPageFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::rankpb::GetRankPageResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerRank.GetRankPage response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerRankGetRankPage(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::GetRankPageRequest& request,
                                            GrpcDone<::rankpb::GetRankPageResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerRankGetRankPageCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerRankGetRankPageErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerRank.GetRankPage requires a done callback";
    }
    if (message.GetDescriptor() != ::rankpb::GetRankPageRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerRank.GetRankPage: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::rankpb::GetRankPageRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerRank.GetRankPage: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerRankGetRankPageCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::rankpb::GetRankPageResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerRankGetRankAround
namespace {
void QueueLocalClientPlayerRankGetRankAroundFailure(GrpcDone<::rankpb::GetRankAroundResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerRankGetRankAroundCall(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::GetRankAroundRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::rankpb::GetRankAroundResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerRank.GetRankAround requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerRankGetRankAroundGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerRankGetRankAroundFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerRankGetRankAroundFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::rankpb::GetRankAroundResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerRank.GetRankAround response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerRankGetRankAround(entt::registry& registry, entt::entity nodeEntity, const ::rankpb::GetRankAroundRequest& request,
                                            GrpcDone<::rankpb::GetRankAroundResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerRankGetRankAroundCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerRankGetRankAroundErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerRank.GetRankAround requires a done callback";
    }
    if (message.GetDescriptor() != ::rankpb::GetRankAroundRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerRank.GetRankAround: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::rankpb::GetRankAroundRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerRank.GetRankAround: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerRankGetRankAroundCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::rankpb::GetRankAroundResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetRankCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace rankpb
