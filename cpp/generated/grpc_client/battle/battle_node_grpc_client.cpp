#include "muduo/base/Logging.h"

#include "battle_node_grpc_client.h"
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

#pragma region BattleNodeQueryObserver
namespace {
void QueueLocalBattleNodeQueryObserverFailure(GrpcDone<::QueryObserverResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodeQueryObserverCall(entt::registry& registry, entt::entity nodeEntity, const ::QueryObserverRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::QueryObserverResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.QueryObserver requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodeQueryObserverGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodeQueryObserverFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodeQueryObserverFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::QueryObserverResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.QueryObserver response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodeQueryObserver(entt::registry& registry, entt::entity nodeEntity, const ::QueryObserverRequest& request,
                                            GrpcDone<::QueryObserverResponse> done, const GrpcCallOptions& options) {
    StartBattleNodeQueryObserverCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodeQueryObserverErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.QueryObserver requires a done callback";
    }
    if (message.GetDescriptor() != ::QueryObserverRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.QueryObserver: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::QueryObserverRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.QueryObserver: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodeQueryObserverCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::QueryObserverResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleNodeQueryBattleCreation
namespace {
void QueueLocalBattleNodeQueryBattleCreationFailure(GrpcDone<::QueryBattleCreationResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodeQueryBattleCreationCall(entt::registry& registry, entt::entity nodeEntity, const ::QueryBattleCreationRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::QueryBattleCreationResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.QueryBattleCreation requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodeQueryBattleCreationGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodeQueryBattleCreationFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodeQueryBattleCreationFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::QueryBattleCreationResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.QueryBattleCreation response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodeQueryBattleCreation(entt::registry& registry, entt::entity nodeEntity, const ::QueryBattleCreationRequest& request,
                                            GrpcDone<::QueryBattleCreationResponse> done, const GrpcCallOptions& options) {
    StartBattleNodeQueryBattleCreationCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodeQueryBattleCreationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.QueryBattleCreation requires a done callback";
    }
    if (message.GetDescriptor() != ::QueryBattleCreationRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.QueryBattleCreation: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::QueryBattleCreationRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.QueryBattleCreation: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodeQueryBattleCreationCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::QueryBattleCreationResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleNodeCreateBattle
namespace {
void QueueLocalBattleNodeCreateBattleFailure(GrpcDone<::CreateBattleResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodeCreateBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::CreateBattleResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.CreateBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodeCreateBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodeCreateBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodeCreateBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::CreateBattleResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.CreateBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request,
                                            GrpcDone<::CreateBattleResponse> done, const GrpcCallOptions& options) {
    StartBattleNodeCreateBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodeCreateBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.CreateBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::CreateBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.CreateBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::CreateBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.CreateBattle: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodeCreateBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::CreateBattleResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleNodeDestroyBattle
namespace {
void QueueLocalBattleNodeDestroyBattleFailure(GrpcDone<::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodeDestroyBattleCall(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.DestroyBattle requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodeDestroyBattleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodeDestroyBattleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodeDestroyBattleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.DestroyBattle response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request,
                                            GrpcDone<::Empty> done, const GrpcCallOptions& options) {
    StartBattleNodeDestroyBattleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodeDestroyBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.DestroyBattle requires a done callback";
    }
    if (message.GetDescriptor() != ::DestroyBattleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.DestroyBattle: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::DestroyBattleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.DestroyBattle: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodeDestroyBattleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleNodeAddObserver
namespace {
void QueueLocalBattleNodeAddObserverFailure(GrpcDone<::AddObserverResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodeAddObserverCall(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::AddObserverResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.AddObserver requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodeAddObserverGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodeAddObserverFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodeAddObserverFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::AddObserverResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.AddObserver response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request,
                                            GrpcDone<::AddObserverResponse> done, const GrpcCallOptions& options) {
    StartBattleNodeAddObserverCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodeAddObserverErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.AddObserver requires a done callback";
    }
    if (message.GetDescriptor() != ::AddObserverRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.AddObserver: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::AddObserverRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.AddObserver: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodeAddObserverCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::AddObserverResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleNodeRemoveObserver
namespace {
void QueueLocalBattleNodeRemoveObserverFailure(GrpcDone<::RemoveObserverResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodeRemoveObserverCall(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::RemoveObserverResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.RemoveObserver requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodeRemoveObserverGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodeRemoveObserverFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodeRemoveObserverFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::RemoveObserverResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.RemoveObserver response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request,
                                            GrpcDone<::RemoveObserverResponse> done, const GrpcCallOptions& options) {
    StartBattleNodeRemoveObserverCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodeRemoveObserverErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.RemoveObserver requires a done callback";
    }
    if (message.GetDescriptor() != ::RemoveObserverRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.RemoveObserver: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::RemoveObserverRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.RemoveObserver: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodeRemoveObserverCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::RemoveObserverResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleNodeIssueBattleTicket
namespace {
void QueueLocalBattleNodeIssueBattleTicketFailure(GrpcDone<::IssueBattleTicketResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodeIssueBattleTicketCall(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::IssueBattleTicketResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.IssueBattleTicket requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodeIssueBattleTicketGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodeIssueBattleTicketFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodeIssueBattleTicketFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::IssueBattleTicketResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.IssueBattleTicket response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request,
                                            GrpcDone<::IssueBattleTicketResponse> done, const GrpcCallOptions& options) {
    StartBattleNodeIssueBattleTicketCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodeIssueBattleTicketErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.IssueBattleTicket requires a done callback";
    }
    if (message.GetDescriptor() != ::IssueBattleTicketRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.IssueBattleTicket: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::IssueBattleTicketRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.IssueBattleTicket: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodeIssueBattleTicketCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::IssueBattleTicketResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleNodeAttachRelayGate
namespace {
void QueueLocalBattleNodeAttachRelayGateFailure(GrpcDone<::AttachRelayGateResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodeAttachRelayGateCall(entt::registry& registry, entt::entity nodeEntity, const ::AttachRelayGateRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::AttachRelayGateResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.AttachRelayGate requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodeAttachRelayGateGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodeAttachRelayGateFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodeAttachRelayGateFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::AttachRelayGateResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.AttachRelayGate response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodeAttachRelayGate(entt::registry& registry, entt::entity nodeEntity, const ::AttachRelayGateRequest& request,
                                            GrpcDone<::AttachRelayGateResponse> done, const GrpcCallOptions& options) {
    StartBattleNodeAttachRelayGateCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodeAttachRelayGateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.AttachRelayGate requires a done callback";
    }
    if (message.GetDescriptor() != ::AttachRelayGateRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.AttachRelayGate: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::AttachRelayGateRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.AttachRelayGate: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodeAttachRelayGateCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::AttachRelayGateResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region BattleNodePromoteSpectateBroadcast
namespace {
void QueueLocalBattleNodePromoteSpectateBroadcastFailure(GrpcDone<::PromoteSpectateBroadcastResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartBattleNodePromoteSpectateBroadcastCall(entt::registry& registry, entt::entity nodeEntity, const ::PromoteSpectateBroadcastRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::PromoteSpectateBroadcastResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.PromoteSpectateBroadcast requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kBattleNodePromoteSpectateBroadcastGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalBattleNodePromoteSpectateBroadcastFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalBattleNodePromoteSpectateBroadcastFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::PromoteSpectateBroadcastResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the BattleNode.PromoteSpectateBroadcast response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendBattleNodePromoteSpectateBroadcast(entt::registry& registry, entt::entity nodeEntity, const ::PromoteSpectateBroadcastRequest& request,
                                            GrpcDone<::PromoteSpectateBroadcastResponse> done, const GrpcCallOptions& options) {
    StartBattleNodePromoteSpectateBroadcastCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendBattleNodePromoteSpectateBroadcastErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC BattleNode.PromoteSpectateBroadcast requires a done callback";
    }
    if (message.GetDescriptor() != ::PromoteSpectateBroadcastRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC BattleNode.PromoteSpectateBroadcast: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::PromoteSpectateBroadcastRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC BattleNode.PromoteSpectateBroadcast: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartBattleNodePromoteSpectateBroadcastCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::PromoteSpectateBroadcastResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetBattleNodeCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}
