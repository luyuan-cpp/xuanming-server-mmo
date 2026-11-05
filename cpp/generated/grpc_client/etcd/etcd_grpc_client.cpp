#include "muduo/base/Logging.h"

#include "etcd_grpc_client.h"
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

namespace etcdserverpb {
#pragma region KVRange
namespace {
void QueueLocalKVRangeFailure(GrpcDone<::etcdserverpb::RangeResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartKVRangeCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::RangeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::RangeResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC KV.Range requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kKVRangeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalKVRangeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalKVRangeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::RangeResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the KV.Range response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendKVRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::RangeRequest& request,
                                            GrpcDone<::etcdserverpb::RangeResponse> done, const GrpcCallOptions& options) {
    StartKVRangeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendKVRangeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC KV.Range requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::RangeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC KV.Range: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::RangeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC KV.Range: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartKVRangeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::RangeResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region KVPut
namespace {
void QueueLocalKVPutFailure(GrpcDone<::etcdserverpb::PutResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartKVPutCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::PutRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::PutResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC KV.Put requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kKVPutGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalKVPutFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalKVPutFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::PutResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the KV.Put response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendKVPut(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::PutRequest& request,
                                            GrpcDone<::etcdserverpb::PutResponse> done, const GrpcCallOptions& options) {
    StartKVPutCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendKVPutErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC KV.Put requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::PutRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC KV.Put: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::PutRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC KV.Put: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartKVPutCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::PutResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region KVDeleteRange
namespace {
void QueueLocalKVDeleteRangeFailure(GrpcDone<::etcdserverpb::DeleteRangeResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartKVDeleteRangeCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::DeleteRangeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::DeleteRangeResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC KV.DeleteRange requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kKVDeleteRangeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalKVDeleteRangeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalKVDeleteRangeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::DeleteRangeResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the KV.DeleteRange response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendKVDeleteRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::DeleteRangeRequest& request,
                                            GrpcDone<::etcdserverpb::DeleteRangeResponse> done, const GrpcCallOptions& options) {
    StartKVDeleteRangeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendKVDeleteRangeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC KV.DeleteRange requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::DeleteRangeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC KV.DeleteRange: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::DeleteRangeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC KV.DeleteRange: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartKVDeleteRangeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::DeleteRangeResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region KVTxn
namespace {
void QueueLocalKVTxnFailure(GrpcDone<::etcdserverpb::TxnResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartKVTxnCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::TxnRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::TxnResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC KV.Txn requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kKVTxnGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalKVTxnFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalKVTxnFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::TxnResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the KV.Txn response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendKVTxn(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::TxnRequest& request,
                                            GrpcDone<::etcdserverpb::TxnResponse> done, const GrpcCallOptions& options) {
    StartKVTxnCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendKVTxnErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC KV.Txn requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::TxnRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC KV.Txn: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::TxnRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC KV.Txn: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartKVTxnCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::TxnResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region KVCompact
namespace {
void QueueLocalKVCompactFailure(GrpcDone<::etcdserverpb::CompactionResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartKVCompactCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::CompactionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::CompactionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC KV.Compact requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kKVCompactGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalKVCompactFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalKVCompactFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::CompactionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the KV.Compact response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendKVCompact(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::CompactionRequest& request,
                                            GrpcDone<::etcdserverpb::CompactionResponse> done, const GrpcCallOptions& options) {
    StartKVCompactCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendKVCompactErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC KV.Compact requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::CompactionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC KV.Compact: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::CompactionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC KV.Compact: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartKVCompactCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::CompactionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LeaseLeaseGrant
namespace {
void QueueLocalLeaseLeaseGrantFailure(GrpcDone<::etcdserverpb::LeaseGrantResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLeaseLeaseGrantCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseGrantRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::LeaseGrantResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC Lease.LeaseGrant requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLeaseLeaseGrantGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLeaseLeaseGrantFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLeaseLeaseGrantFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::LeaseGrantResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the Lease.LeaseGrant response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLeaseLeaseGrant(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseGrantRequest& request,
                                            GrpcDone<::etcdserverpb::LeaseGrantResponse> done, const GrpcCallOptions& options) {
    StartLeaseLeaseGrantCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLeaseLeaseGrantErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC Lease.LeaseGrant requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::LeaseGrantRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC Lease.LeaseGrant: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::LeaseGrantRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC Lease.LeaseGrant: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLeaseLeaseGrantCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::LeaseGrantResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LeaseLeaseRevoke
namespace {
void QueueLocalLeaseLeaseRevokeFailure(GrpcDone<::etcdserverpb::LeaseRevokeResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLeaseLeaseRevokeCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseRevokeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::LeaseRevokeResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC Lease.LeaseRevoke requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLeaseLeaseRevokeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLeaseLeaseRevokeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLeaseLeaseRevokeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::LeaseRevokeResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the Lease.LeaseRevoke response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLeaseLeaseRevoke(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseRevokeRequest& request,
                                            GrpcDone<::etcdserverpb::LeaseRevokeResponse> done, const GrpcCallOptions& options) {
    StartLeaseLeaseRevokeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLeaseLeaseRevokeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC Lease.LeaseRevoke requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::LeaseRevokeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC Lease.LeaseRevoke: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::LeaseRevokeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC Lease.LeaseRevoke: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLeaseLeaseRevokeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::LeaseRevokeResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LeaseLeaseTimeToLive
namespace {
void QueueLocalLeaseLeaseTimeToLiveFailure(GrpcDone<::etcdserverpb::LeaseTimeToLiveResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLeaseLeaseTimeToLiveCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseTimeToLiveRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::LeaseTimeToLiveResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC Lease.LeaseTimeToLive requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLeaseLeaseTimeToLiveGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLeaseLeaseTimeToLiveFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLeaseLeaseTimeToLiveFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::LeaseTimeToLiveResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the Lease.LeaseTimeToLive response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLeaseLeaseTimeToLive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseTimeToLiveRequest& request,
                                            GrpcDone<::etcdserverpb::LeaseTimeToLiveResponse> done, const GrpcCallOptions& options) {
    StartLeaseLeaseTimeToLiveCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLeaseLeaseTimeToLiveErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC Lease.LeaseTimeToLive requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::LeaseTimeToLiveRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC Lease.LeaseTimeToLive: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::LeaseTimeToLiveRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC Lease.LeaseTimeToLive: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLeaseLeaseTimeToLiveCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::LeaseTimeToLiveResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region LeaseLeaseLeases
namespace {
void QueueLocalLeaseLeaseLeasesFailure(GrpcDone<::etcdserverpb::LeaseLeasesResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartLeaseLeaseLeasesCall(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseLeasesRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::etcdserverpb::LeaseLeasesResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC Lease.LeaseLeases requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kLeaseLeaseLeasesGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalLeaseLeaseLeasesFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalLeaseLeaseLeasesFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::etcdserverpb::LeaseLeasesResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the Lease.LeaseLeases response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendLeaseLeaseLeases(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseLeasesRequest& request,
                                            GrpcDone<::etcdserverpb::LeaseLeasesResponse> done, const GrpcCallOptions& options) {
    StartLeaseLeaseLeasesCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendLeaseLeaseLeasesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC Lease.LeaseLeases requires a done callback";
    }
    if (message.GetDescriptor() != ::etcdserverpb::LeaseLeasesRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC Lease.LeaseLeases: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::etcdserverpb::LeaseLeasesRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC Lease.LeaseLeases: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartLeaseLeaseLeasesCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::etcdserverpb::LeaseLeasesResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetEtcdCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace etcdserverpb
