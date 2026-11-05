#include "muduo/base/Logging.h"

#include "friend_grpc_client.h"
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

namespace friendpb {
#pragma region ClientPlayerFriendAddFriend
namespace {
void QueueLocalClientPlayerFriendAddFriendFailure(GrpcDone<::friendpb::AddFriendResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendAddFriendCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::AddFriendResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.AddFriend requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendAddFriendGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendAddFriendFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendAddFriendFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::AddFriendResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.AddFriend response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request,
                                            GrpcDone<::friendpb::AddFriendResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendAddFriendCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendAddFriendErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.AddFriend requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::AddFriendRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.AddFriend: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::AddFriendRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.AddFriend: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendAddFriendCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::AddFriendResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendAcceptFriend
namespace {
void QueueLocalClientPlayerFriendAcceptFriendFailure(GrpcDone<::friendpb::AcceptFriendResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendAcceptFriendCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::AcceptFriendResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.AcceptFriend requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendAcceptFriendGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendAcceptFriendFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendAcceptFriendFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::AcceptFriendResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.AcceptFriend response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request,
                                            GrpcDone<::friendpb::AcceptFriendResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendAcceptFriendCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendAcceptFriendErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.AcceptFriend requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::AcceptFriendRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.AcceptFriend: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::AcceptFriendRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.AcceptFriend: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendAcceptFriendCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::AcceptFriendResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendRejectFriend
namespace {
void QueueLocalClientPlayerFriendRejectFriendFailure(GrpcDone<::friendpb::RejectFriendResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendRejectFriendCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::RejectFriendResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.RejectFriend requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendRejectFriendGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendRejectFriendFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendRejectFriendFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::RejectFriendResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.RejectFriend response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request,
                                            GrpcDone<::friendpb::RejectFriendResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendRejectFriendCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendRejectFriendErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.RejectFriend requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::RejectFriendRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.RejectFriend: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::RejectFriendRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.RejectFriend: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendRejectFriendCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::RejectFriendResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendRemoveFriend
namespace {
void QueueLocalClientPlayerFriendRemoveFriendFailure(GrpcDone<::friendpb::RemoveFriendResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendRemoveFriendCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::RemoveFriendResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.RemoveFriend requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendRemoveFriendGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendRemoveFriendFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendRemoveFriendFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::RemoveFriendResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.RemoveFriend response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request,
                                            GrpcDone<::friendpb::RemoveFriendResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendRemoveFriendCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendRemoveFriendErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.RemoveFriend requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::RemoveFriendRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.RemoveFriend: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::RemoveFriendRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.RemoveFriend: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendRemoveFriendCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::RemoveFriendResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendGetFriendList
namespace {
void QueueLocalClientPlayerFriendGetFriendListFailure(GrpcDone<::friendpb::GetFriendListResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendGetFriendListCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::GetFriendListResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.GetFriendList requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendGetFriendListGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendGetFriendListFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendGetFriendListFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::GetFriendListResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.GetFriendList response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request,
                                            GrpcDone<::friendpb::GetFriendListResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendGetFriendListCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendGetFriendListErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.GetFriendList requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::GetFriendListRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.GetFriendList: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::GetFriendListRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.GetFriendList: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendGetFriendListCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::GetFriendListResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendGetPendingRequests
namespace {
void QueueLocalClientPlayerFriendGetPendingRequestsFailure(GrpcDone<::friendpb::GetPendingRequestsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendGetPendingRequestsCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::GetPendingRequestsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.GetPendingRequests requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendGetPendingRequestsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendGetPendingRequestsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendGetPendingRequestsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::GetPendingRequestsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.GetPendingRequests response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request,
                                            GrpcDone<::friendpb::GetPendingRequestsResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendGetPendingRequestsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendGetPendingRequestsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.GetPendingRequests requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::GetPendingRequestsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.GetPendingRequests: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::GetPendingRequestsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.GetPendingRequests: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendGetPendingRequestsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::GetPendingRequestsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendBlock
namespace {
void QueueLocalClientPlayerFriendBlockFailure(GrpcDone<::friendpb::BlockResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendBlockCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::BlockResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.Block requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendBlockGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendBlockFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendBlockFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::BlockResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.Block response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request,
                                            GrpcDone<::friendpb::BlockResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendBlockCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendBlockErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.Block requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::BlockRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.Block: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::BlockRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.Block: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendBlockCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::BlockResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendUnblock
namespace {
void QueueLocalClientPlayerFriendUnblockFailure(GrpcDone<::friendpb::UnblockResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendUnblockCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::UnblockResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.Unblock requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendUnblockGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendUnblockFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendUnblockFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::UnblockResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.Unblock response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request,
                                            GrpcDone<::friendpb::UnblockResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendUnblockCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendUnblockErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.Unblock requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::UnblockRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.Unblock: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::UnblockRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.Unblock: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendUnblockCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::UnblockResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendListBlocks
namespace {
void QueueLocalClientPlayerFriendListBlocksFailure(GrpcDone<::friendpb::ListBlocksResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendListBlocksCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::ListBlocksResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.ListBlocks requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendListBlocksGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendListBlocksFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendListBlocksFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::ListBlocksResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.ListBlocks response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request,
                                            GrpcDone<::friendpb::ListBlocksResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendListBlocksCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendListBlocksErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.ListBlocks requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::ListBlocksRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.ListBlocks: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::ListBlocksRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.ListBlocks: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendListBlocksCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::ListBlocksResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendRecommendFriends
namespace {
void QueueLocalClientPlayerFriendRecommendFriendsFailure(GrpcDone<::friendpb::RecommendFriendsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendRecommendFriendsCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::friendpb::RecommendFriendsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.RecommendFriends requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendRecommendFriendsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendRecommendFriendsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendRecommendFriendsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::friendpb::RecommendFriendsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the ClientPlayerFriend.RecommendFriends response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request,
                                            GrpcDone<::friendpb::RecommendFriendsResponse> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendRecommendFriendsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendRecommendFriendsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.RecommendFriends requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::RecommendFriendsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.RecommendFriends: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::RecommendFriendsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.RecommendFriends: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendRecommendFriendsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::friendpb::RecommendFriendsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region ClientPlayerFriendNotifyFriendEvent
namespace {
void QueueLocalClientPlayerFriendNotifyFriendEventFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartClientPlayerFriendNotifyFriendEventCall(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.NotifyFriendEvent requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kClientPlayerFriendNotifyFriendEventGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalClientPlayerFriendNotifyFriendEventFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalClientPlayerFriendNotifyFriendEventFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
            outcome.message = "failed to parse the ClientPlayerFriend.NotifyFriendEvent response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartClientPlayerFriendNotifyFriendEventCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendClientPlayerFriendNotifyFriendEventErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC ClientPlayerFriend.NotifyFriendEvent requires a done callback";
    }
    if (message.GetDescriptor() != ::friendpb::FriendEventS2C::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC ClientPlayerFriend.NotifyFriendEvent: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::friendpb::FriendEventS2C*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC ClientPlayerFriend.NotifyFriendEvent: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartClientPlayerFriendNotifyFriendEventCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion

void SetFriendCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace friendpb
