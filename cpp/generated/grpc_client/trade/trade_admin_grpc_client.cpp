#include "muduo/base/Logging.h"

#include "trade_admin_grpc_client.h"
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

namespace trade {
#pragma region TradeAdminSeedListing
namespace {
void QueueLocalTradeAdminSeedListingFailure(GrpcDone<::trade::SeedListingResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartTradeAdminSeedListingCall(entt::registry& registry, entt::entity nodeEntity, const ::trade::SeedListingRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::trade::SeedListingResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC TradeAdmin.SeedListing requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kTradeAdminSeedListingGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalTradeAdminSeedListingFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalTradeAdminSeedListingFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::trade::SeedListingResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the TradeAdmin.SeedListing response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const ::trade::SeedListingRequest& request,
                                            GrpcDone<::trade::SeedListingResponse> done, const GrpcCallOptions& options) {
    StartTradeAdminSeedListingCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendTradeAdminSeedListingErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC TradeAdmin.SeedListing requires a done callback";
    }
    if (message.GetDescriptor() != ::trade::SeedListingRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC TradeAdmin.SeedListing: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::trade::SeedListingRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC TradeAdmin.SeedListing: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartTradeAdminSeedListingCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::trade::SeedListingResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region TradeAdminListManualContracts
namespace {
void QueueLocalTradeAdminListManualContractsFailure(GrpcDone<::trade::ListManualContractsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartTradeAdminListManualContractsCall(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListManualContractsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::trade::ListManualContractsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC TradeAdmin.ListManualContracts requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kTradeAdminListManualContractsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalTradeAdminListManualContractsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalTradeAdminListManualContractsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::trade::ListManualContractsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the TradeAdmin.ListManualContracts response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendTradeAdminListManualContracts(entt::registry& registry, entt::entity nodeEntity, const ::trade::ListManualContractsRequest& request,
                                            GrpcDone<::trade::ListManualContractsResponse> done, const GrpcCallOptions& options) {
    StartTradeAdminListManualContractsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendTradeAdminListManualContractsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC TradeAdmin.ListManualContracts requires a done callback";
    }
    if (message.GetDescriptor() != ::trade::ListManualContractsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC TradeAdmin.ListManualContracts: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::trade::ListManualContractsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC TradeAdmin.ListManualContracts: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartTradeAdminListManualContractsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::trade::ListManualContractsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region TradeAdminResolveManualContract
namespace {
void QueueLocalTradeAdminResolveManualContractFailure(GrpcDone<::trade::ResolveManualContractResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartTradeAdminResolveManualContractCall(entt::registry& registry, entt::entity nodeEntity, const ::trade::ResolveManualContractRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::trade::ResolveManualContractResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC TradeAdmin.ResolveManualContract requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kTradeAdminResolveManualContractGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalTradeAdminResolveManualContractFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalTradeAdminResolveManualContractFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::trade::ResolveManualContractResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the TradeAdmin.ResolveManualContract response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendTradeAdminResolveManualContract(entt::registry& registry, entt::entity nodeEntity, const ::trade::ResolveManualContractRequest& request,
                                            GrpcDone<::trade::ResolveManualContractResponse> done, const GrpcCallOptions& options) {
    StartTradeAdminResolveManualContractCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendTradeAdminResolveManualContractErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC TradeAdmin.ResolveManualContract requires a done callback";
    }
    if (message.GetDescriptor() != ::trade::ResolveManualContractRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC TradeAdmin.ResolveManualContract: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::trade::ResolveManualContractRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC TradeAdmin.ResolveManualContract: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartTradeAdminResolveManualContractCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::trade::ResolveManualContractResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetTradeAdminCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace trade
