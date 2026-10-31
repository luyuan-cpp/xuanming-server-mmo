#include "muduo/base/Logging.h"

#include "data_service_grpc_client.h"
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

namespace data_service {
#pragma region DataServiceReadAccountDirectory
namespace {
void QueueLocalDataServiceReadAccountDirectoryFailure(GrpcDone<::data_service::AccountDirectoryResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceReadAccountDirectoryCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReadAccountDirectoryRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AccountDirectoryResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ReadAccountDirectory requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceReadAccountDirectoryGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceReadAccountDirectoryFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceReadAccountDirectoryFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AccountDirectoryResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.ReadAccountDirectory response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceReadAccountDirectory(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReadAccountDirectoryRequest& request,
                                            GrpcDone<::data_service::AccountDirectoryResponse> done, const GrpcCallOptions& options) {
    StartDataServiceReadAccountDirectoryCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceReadAccountDirectoryErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ReadAccountDirectory requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::ReadAccountDirectoryRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.ReadAccountDirectory: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::ReadAccountDirectoryRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.ReadAccountDirectory: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceReadAccountDirectoryCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AccountDirectoryResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceCreateAccountRole
namespace {
void QueueLocalDataServiceCreateAccountRoleFailure(GrpcDone<::data_service::AccountDirectoryResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceCreateAccountRoleCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateAccountRoleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AccountDirectoryResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.CreateAccountRole requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceCreateAccountRoleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceCreateAccountRoleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceCreateAccountRoleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AccountDirectoryResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.CreateAccountRole response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceCreateAccountRole(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateAccountRoleRequest& request,
                                            GrpcDone<::data_service::AccountDirectoryResponse> done, const GrpcCallOptions& options) {
    StartDataServiceCreateAccountRoleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceCreateAccountRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.CreateAccountRole requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::CreateAccountRoleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.CreateAccountRole: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::CreateAccountRoleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.CreateAccountRole: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceCreateAccountRoleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AccountDirectoryResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceLookupAccountRole
namespace {
void QueueLocalDataServiceLookupAccountRoleFailure(GrpcDone<::data_service::AccountRoleOwnership> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceLookupAccountRoleCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LookupAccountRoleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AccountRoleOwnership> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.LookupAccountRole requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceLookupAccountRoleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceLookupAccountRoleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceLookupAccountRoleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AccountRoleOwnership reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.LookupAccountRole response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceLookupAccountRole(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LookupAccountRoleRequest& request,
                                            GrpcDone<::data_service::AccountRoleOwnership> done, const GrpcCallOptions& options) {
    StartDataServiceLookupAccountRoleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceLookupAccountRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.LookupAccountRole requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::LookupAccountRoleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.LookupAccountRole: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::LookupAccountRoleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.LookupAccountRole: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceLookupAccountRoleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AccountRoleOwnership* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceRepairAccountRole
namespace {
void QueueLocalDataServiceRepairAccountRoleFailure(GrpcDone<::data_service::AccountDirectoryResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceRepairAccountRoleCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RepairAccountRoleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AccountDirectoryResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.RepairAccountRole requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceRepairAccountRoleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceRepairAccountRoleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceRepairAccountRoleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AccountDirectoryResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.RepairAccountRole response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceRepairAccountRole(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RepairAccountRoleRequest& request,
                                            GrpcDone<::data_service::AccountDirectoryResponse> done, const GrpcCallOptions& options) {
    StartDataServiceRepairAccountRoleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceRepairAccountRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.RepairAccountRole requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::RepairAccountRoleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.RepairAccountRole: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::RepairAccountRoleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.RepairAccountRole: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceRepairAccountRoleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AccountDirectoryResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceRemoveAccountRoles
namespace {
void QueueLocalDataServiceRemoveAccountRolesFailure(GrpcDone<::google::protobuf::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceRemoveAccountRolesCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemoveAccountRolesRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::google::protobuf::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.RemoveAccountRoles requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceRemoveAccountRolesGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceRemoveAccountRolesFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceRemoveAccountRolesFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::google::protobuf::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.RemoveAccountRoles response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceRemoveAccountRoles(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemoveAccountRolesRequest& request,
                                            GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options) {
    StartDataServiceRemoveAccountRolesCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceRemoveAccountRolesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.RemoveAccountRoles requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::RemoveAccountRolesRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.RemoveAccountRoles: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::RemoveAccountRolesRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.RemoveAccountRoles: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceRemoveAccountRolesCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::google::protobuf::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceBeginAccountRoleTrade
namespace {
void QueueLocalDataServiceBeginAccountRoleTradeFailure(GrpcDone<::data_service::AccountRoleTradeReceipt> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceBeginAccountRoleTradeCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BeginAccountRoleTradeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AccountRoleTradeReceipt> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BeginAccountRoleTrade requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceBeginAccountRoleTradeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceBeginAccountRoleTradeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceBeginAccountRoleTradeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AccountRoleTradeReceipt reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.BeginAccountRoleTrade response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceBeginAccountRoleTrade(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BeginAccountRoleTradeRequest& request,
                                            GrpcDone<::data_service::AccountRoleTradeReceipt> done, const GrpcCallOptions& options) {
    StartDataServiceBeginAccountRoleTradeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceBeginAccountRoleTradeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BeginAccountRoleTrade requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::BeginAccountRoleTradeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.BeginAccountRoleTrade: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::BeginAccountRoleTradeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.BeginAccountRoleTrade: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceBeginAccountRoleTradeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AccountRoleTradeReceipt* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceConfirmAccountRoleTradeFreeze
namespace {
void QueueLocalDataServiceConfirmAccountRoleTradeFreezeFailure(GrpcDone<::data_service::AccountRoleTradeReceipt> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceConfirmAccountRoleTradeFreezeCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ConfirmAccountRoleTradeFreezeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AccountRoleTradeReceipt> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ConfirmAccountRoleTradeFreeze requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceConfirmAccountRoleTradeFreezeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceConfirmAccountRoleTradeFreezeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceConfirmAccountRoleTradeFreezeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AccountRoleTradeReceipt reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.ConfirmAccountRoleTradeFreeze response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceConfirmAccountRoleTradeFreeze(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ConfirmAccountRoleTradeFreezeRequest& request,
                                            GrpcDone<::data_service::AccountRoleTradeReceipt> done, const GrpcCallOptions& options) {
    StartDataServiceConfirmAccountRoleTradeFreezeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceConfirmAccountRoleTradeFreezeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ConfirmAccountRoleTradeFreeze requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::ConfirmAccountRoleTradeFreezeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.ConfirmAccountRoleTradeFreeze: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::ConfirmAccountRoleTradeFreezeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.ConfirmAccountRoleTradeFreeze: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceConfirmAccountRoleTradeFreezeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AccountRoleTradeReceipt* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceCancelAccountRoleTrade
namespace {
void QueueLocalDataServiceCancelAccountRoleTradeFailure(GrpcDone<::data_service::AccountRoleTradeReceipt> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceCancelAccountRoleTradeCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CancelAccountRoleTradeRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AccountRoleTradeReceipt> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.CancelAccountRoleTrade requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceCancelAccountRoleTradeGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceCancelAccountRoleTradeFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceCancelAccountRoleTradeFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AccountRoleTradeReceipt reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.CancelAccountRoleTrade response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceCancelAccountRoleTrade(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CancelAccountRoleTradeRequest& request,
                                            GrpcDone<::data_service::AccountRoleTradeReceipt> done, const GrpcCallOptions& options) {
    StartDataServiceCancelAccountRoleTradeCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceCancelAccountRoleTradeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.CancelAccountRoleTrade requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::CancelAccountRoleTradeRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.CancelAccountRoleTrade: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::CancelAccountRoleTradeRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.CancelAccountRoleTrade: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceCancelAccountRoleTradeCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AccountRoleTradeReceipt* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceTransferAccountRole
namespace {
void QueueLocalDataServiceTransferAccountRoleFailure(GrpcDone<::data_service::AccountRoleTransferReceipt> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceTransferAccountRoleCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::TransferAccountRoleRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AccountRoleTransferReceipt> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.TransferAccountRole requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceTransferAccountRoleGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceTransferAccountRoleFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceTransferAccountRoleFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AccountRoleTransferReceipt reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.TransferAccountRole response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceTransferAccountRole(entt::registry& registry, entt::entity nodeEntity, const ::data_service::TransferAccountRoleRequest& request,
                                            GrpcDone<::data_service::AccountRoleTransferReceipt> done, const GrpcCallOptions& options) {
    StartDataServiceTransferAccountRoleCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceTransferAccountRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.TransferAccountRole requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::TransferAccountRoleRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.TransferAccountRole: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::TransferAccountRoleRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.TransferAccountRole: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceTransferAccountRoleCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AccountRoleTransferReceipt* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceRegisterRoleAdmission
namespace {
void QueueLocalDataServiceRegisterRoleAdmissionFailure(GrpcDone<::data_service::RegisterRoleAdmissionResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceRegisterRoleAdmissionCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterRoleAdmissionRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::RegisterRoleAdmissionResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.RegisterRoleAdmission requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceRegisterRoleAdmissionGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceRegisterRoleAdmissionFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceRegisterRoleAdmissionFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::RegisterRoleAdmissionResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.RegisterRoleAdmission response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceRegisterRoleAdmission(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterRoleAdmissionRequest& request,
                                            GrpcDone<::data_service::RegisterRoleAdmissionResponse> done, const GrpcCallOptions& options) {
    StartDataServiceRegisterRoleAdmissionCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceRegisterRoleAdmissionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.RegisterRoleAdmission requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::RegisterRoleAdmissionRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.RegisterRoleAdmission: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::RegisterRoleAdmissionRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.RegisterRoleAdmission: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceRegisterRoleAdmissionCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::RegisterRoleAdmissionResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceCloseRoleAdmissions
namespace {
void QueueLocalDataServiceCloseRoleAdmissionsFailure(GrpcDone<::data_service::CloseRoleAdmissionsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceCloseRoleAdmissionsCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CloseRoleAdmissionsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::CloseRoleAdmissionsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.CloseRoleAdmissions requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceCloseRoleAdmissionsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceCloseRoleAdmissionsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceCloseRoleAdmissionsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::CloseRoleAdmissionsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.CloseRoleAdmissions response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceCloseRoleAdmissions(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CloseRoleAdmissionsRequest& request,
                                            GrpcDone<::data_service::CloseRoleAdmissionsResponse> done, const GrpcCallOptions& options) {
    StartDataServiceCloseRoleAdmissionsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceCloseRoleAdmissionsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.CloseRoleAdmissions requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::CloseRoleAdmissionsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.CloseRoleAdmissions: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::CloseRoleAdmissionsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.CloseRoleAdmissions: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceCloseRoleAdmissionsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::CloseRoleAdmissionsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceAckRoleTradeDrain
namespace {
void QueueLocalDataServiceAckRoleTradeDrainFailure(GrpcDone<::google::protobuf::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceAckRoleTradeDrainCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AckRoleTradeDrainRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::google::protobuf::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.AckRoleTradeDrain requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceAckRoleTradeDrainGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceAckRoleTradeDrainFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceAckRoleTradeDrainFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::google::protobuf::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.AckRoleTradeDrain response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceAckRoleTradeDrain(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AckRoleTradeDrainRequest& request,
                                            GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options) {
    StartDataServiceAckRoleTradeDrainCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceAckRoleTradeDrainErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.AckRoleTradeDrain requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::AckRoleTradeDrainRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.AckRoleTradeDrain: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::AckRoleTradeDrainRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.AckRoleTradeDrain: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceAckRoleTradeDrainCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::google::protobuf::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceReleaseDeadPlayerStoreHolder
namespace {
void QueueLocalDataServiceReleaseDeadPlayerStoreHolderFailure(GrpcDone<::google::protobuf::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceReleaseDeadPlayerStoreHolderCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleaseDeadPlayerStoreHolderRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::google::protobuf::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ReleaseDeadPlayerStoreHolder requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceReleaseDeadPlayerStoreHolderGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceReleaseDeadPlayerStoreHolderFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceReleaseDeadPlayerStoreHolderFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::google::protobuf::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.ReleaseDeadPlayerStoreHolder response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceReleaseDeadPlayerStoreHolder(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleaseDeadPlayerStoreHolderRequest& request,
                                            GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options) {
    StartDataServiceReleaseDeadPlayerStoreHolderCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceReleaseDeadPlayerStoreHolderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ReleaseDeadPlayerStoreHolder requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::ReleaseDeadPlayerStoreHolderRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.ReleaseDeadPlayerStoreHolder: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::ReleaseDeadPlayerStoreHolderRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.ReleaseDeadPlayerStoreHolder: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceReleaseDeadPlayerStoreHolderCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::google::protobuf::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceRegisterPlayerZone
namespace {
void QueueLocalDataServiceRegisterPlayerZoneFailure(GrpcDone<::google::protobuf::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceRegisterPlayerZoneCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::google::protobuf::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.RegisterPlayerZone requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceRegisterPlayerZoneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceRegisterPlayerZoneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceRegisterPlayerZoneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::google::protobuf::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.RegisterPlayerZone response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request,
                                            GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options) {
    StartDataServiceRegisterPlayerZoneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceRegisterPlayerZoneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.RegisterPlayerZone requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::RegisterPlayerZoneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.RegisterPlayerZone: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::RegisterPlayerZoneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.RegisterPlayerZone: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceRegisterPlayerZoneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::google::protobuf::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceGetPlayerHomeZone
namespace {
void QueueLocalDataServiceGetPlayerHomeZoneFailure(GrpcDone<::data_service::GetPlayerHomeZoneResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceGetPlayerHomeZoneCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::GetPlayerHomeZoneResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.GetPlayerHomeZone requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceGetPlayerHomeZoneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceGetPlayerHomeZoneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceGetPlayerHomeZoneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::GetPlayerHomeZoneResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.GetPlayerHomeZone response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request,
                                            GrpcDone<::data_service::GetPlayerHomeZoneResponse> done, const GrpcCallOptions& options) {
    StartDataServiceGetPlayerHomeZoneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceGetPlayerHomeZoneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.GetPlayerHomeZone requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::GetPlayerHomeZoneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.GetPlayerHomeZone: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::GetPlayerHomeZoneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.GetPlayerHomeZone: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceGetPlayerHomeZoneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::GetPlayerHomeZoneResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceBatchGetPlayerHomeZone
namespace {
void QueueLocalDataServiceBatchGetPlayerHomeZoneFailure(GrpcDone<::data_service::BatchGetPlayerHomeZoneResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceBatchGetPlayerHomeZoneCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::BatchGetPlayerHomeZoneResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BatchGetPlayerHomeZone requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceBatchGetPlayerHomeZoneGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceBatchGetPlayerHomeZoneFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceBatchGetPlayerHomeZoneFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::BatchGetPlayerHomeZoneResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.BatchGetPlayerHomeZone response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request,
                                            GrpcDone<::data_service::BatchGetPlayerHomeZoneResponse> done, const GrpcCallOptions& options) {
    StartDataServiceBatchGetPlayerHomeZoneCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceBatchGetPlayerHomeZoneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BatchGetPlayerHomeZone requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::BatchGetPlayerHomeZoneRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.BatchGetPlayerHomeZone: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::BatchGetPlayerHomeZoneRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.BatchGetPlayerHomeZone: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceBatchGetPlayerHomeZoneCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::BatchGetPlayerHomeZoneResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceGetMergedZones
namespace {
void QueueLocalDataServiceGetMergedZonesFailure(GrpcDone<::data_service::GetMergedZonesResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceGetMergedZonesCall(entt::registry& registry, entt::entity nodeEntity, const ::google::protobuf::Empty& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::GetMergedZonesResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.GetMergedZones requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceGetMergedZonesGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceGetMergedZonesFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceGetMergedZonesFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::GetMergedZonesResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.GetMergedZones response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceGetMergedZones(entt::registry& registry, entt::entity nodeEntity, const ::google::protobuf::Empty& request,
                                            GrpcDone<::data_service::GetMergedZonesResponse> done, const GrpcCallOptions& options) {
    StartDataServiceGetMergedZonesCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceGetMergedZonesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.GetMergedZones requires a done callback";
    }
    if (message.GetDescriptor() != ::google::protobuf::Empty::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.GetMergedZones: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::google::protobuf::Empty*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.GetMergedZones: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceGetMergedZonesCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::GetMergedZonesResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceListPlayerSnapshots
namespace {
void QueueLocalDataServiceListPlayerSnapshotsFailure(GrpcDone<::data_service::ListPlayerSnapshotsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceListPlayerSnapshotsCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::ListPlayerSnapshotsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ListPlayerSnapshots requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceListPlayerSnapshotsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceListPlayerSnapshotsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceListPlayerSnapshotsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::ListPlayerSnapshotsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.ListPlayerSnapshots response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request,
                                            GrpcDone<::data_service::ListPlayerSnapshotsResponse> done, const GrpcCallOptions& options) {
    StartDataServiceListPlayerSnapshotsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceListPlayerSnapshotsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ListPlayerSnapshots requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::ListPlayerSnapshotsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.ListPlayerSnapshots: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::ListPlayerSnapshotsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.ListPlayerSnapshots: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceListPlayerSnapshotsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::ListPlayerSnapshotsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceBatchRecallItems
namespace {
void QueueLocalDataServiceBatchRecallItemsFailure(GrpcDone<::data_service::BatchRecallItemsResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceBatchRecallItemsCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::BatchRecallItemsResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BatchRecallItems requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceBatchRecallItemsGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceBatchRecallItemsFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceBatchRecallItemsFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::BatchRecallItemsResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.BatchRecallItems response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request,
                                            GrpcDone<::data_service::BatchRecallItemsResponse> done, const GrpcCallOptions& options) {
    StartDataServiceBatchRecallItemsCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceBatchRecallItemsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BatchRecallItems requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::BatchRecallItemsRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.BatchRecallItems: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::BatchRecallItemsRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.BatchRecallItems: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceBatchRecallItemsCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::BatchRecallItemsResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceQueryTransactionLog
namespace {
void QueueLocalDataServiceQueryTransactionLogFailure(GrpcDone<::data_service::QueryTransactionLogResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceQueryTransactionLogCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::QueryTransactionLogResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.QueryTransactionLog requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceQueryTransactionLogGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceQueryTransactionLogFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceQueryTransactionLogFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::QueryTransactionLogResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.QueryTransactionLog response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request,
                                            GrpcDone<::data_service::QueryTransactionLogResponse> done, const GrpcCallOptions& options) {
    StartDataServiceQueryTransactionLogCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceQueryTransactionLogErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.QueryTransactionLog requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::QueryTransactionLogRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.QueryTransactionLog: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::QueryTransactionLogRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.QueryTransactionLog: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceQueryTransactionLogCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::QueryTransactionLogResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceAllocateIdSegment
namespace {
void QueueLocalDataServiceAllocateIdSegmentFailure(GrpcDone<::data_service::AllocateIdSegmentResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceAllocateIdSegmentCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AllocateIdSegmentRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::AllocateIdSegmentResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.AllocateIdSegment requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceAllocateIdSegmentGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceAllocateIdSegmentFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceAllocateIdSegmentFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::AllocateIdSegmentResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.AllocateIdSegment response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AllocateIdSegmentRequest& request,
                                            GrpcDone<::data_service::AllocateIdSegmentResponse> done, const GrpcCallOptions& options) {
    StartDataServiceAllocateIdSegmentCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceAllocateIdSegmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.AllocateIdSegment requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::AllocateIdSegmentRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.AllocateIdSegment: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::AllocateIdSegmentRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.AllocateIdSegment: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceAllocateIdSegmentCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::AllocateIdSegmentResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceReservePlayerName
namespace {
void QueueLocalDataServiceReservePlayerNameFailure(GrpcDone<::data_service::ReservePlayerNameResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceReservePlayerNameCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReservePlayerNameRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::ReservePlayerNameResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ReservePlayerName requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceReservePlayerNameGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceReservePlayerNameFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceReservePlayerNameFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::ReservePlayerNameResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.ReservePlayerName response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReservePlayerNameRequest& request,
                                            GrpcDone<::data_service::ReservePlayerNameResponse> done, const GrpcCallOptions& options) {
    StartDataServiceReservePlayerNameCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceReservePlayerNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ReservePlayerName requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::ReservePlayerNameRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.ReservePlayerName: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::ReservePlayerNameRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.ReservePlayerName: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceReservePlayerNameCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::ReservePlayerNameResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceReleasePlayerName
namespace {
void QueueLocalDataServiceReleasePlayerNameFailure(GrpcDone<::google::protobuf::Empty> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceReleasePlayerNameCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleasePlayerNameRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::google::protobuf::Empty> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ReleasePlayerName requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceReleasePlayerNameGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceReleasePlayerNameFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceReleasePlayerNameFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::google::protobuf::Empty reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.ReleasePlayerName response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleasePlayerNameRequest& request,
                                            GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options) {
    StartDataServiceReleasePlayerNameCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceReleasePlayerNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.ReleasePlayerName requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::ReleasePlayerNameRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.ReleasePlayerName: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::ReleasePlayerNameRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.ReleasePlayerName: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceReleasePlayerNameCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::google::protobuf::Empty* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceBatchGetPlayerName
namespace {
void QueueLocalDataServiceBatchGetPlayerNameFailure(GrpcDone<::data_service::BatchGetPlayerNameResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceBatchGetPlayerNameCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerNameRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::BatchGetPlayerNameResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BatchGetPlayerName requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceBatchGetPlayerNameGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceBatchGetPlayerNameFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceBatchGetPlayerNameFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::BatchGetPlayerNameResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.BatchGetPlayerName response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerNameRequest& request,
                                            GrpcDone<::data_service::BatchGetPlayerNameResponse> done, const GrpcCallOptions& options) {
    StartDataServiceBatchGetPlayerNameCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceBatchGetPlayerNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BatchGetPlayerName requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::BatchGetPlayerNameRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.BatchGetPlayerName: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::BatchGetPlayerNameRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.BatchGetPlayerName: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceBatchGetPlayerNameCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::BatchGetPlayerNameResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceBatchGetPlayerDisplay
namespace {
void QueueLocalDataServiceBatchGetPlayerDisplayFailure(GrpcDone<::data_service::BatchGetPlayerDisplayResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceBatchGetPlayerDisplayCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerDisplayRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::BatchGetPlayerDisplayResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BatchGetPlayerDisplay requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceBatchGetPlayerDisplayGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceBatchGetPlayerDisplayFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceBatchGetPlayerDisplayFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::BatchGetPlayerDisplayResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.BatchGetPlayerDisplay response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceBatchGetPlayerDisplay(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerDisplayRequest& request,
                                            GrpcDone<::data_service::BatchGetPlayerDisplayResponse> done, const GrpcCallOptions& options) {
    StartDataServiceBatchGetPlayerDisplayCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceBatchGetPlayerDisplayErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.BatchGetPlayerDisplay requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::BatchGetPlayerDisplayRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.BatchGetPlayerDisplay: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::BatchGetPlayerDisplayRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.BatchGetPlayerDisplay: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceBatchGetPlayerDisplayCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::BatchGetPlayerDisplayResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceGetPlayerAssetOpLedger
namespace {
void QueueLocalDataServiceGetPlayerAssetOpLedgerFailure(GrpcDone<::data_service::GetPlayerAssetOpLedgerResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceGetPlayerAssetOpLedgerCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerAssetOpLedgerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::GetPlayerAssetOpLedgerResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.GetPlayerAssetOpLedger requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceGetPlayerAssetOpLedgerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceGetPlayerAssetOpLedgerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceGetPlayerAssetOpLedgerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::GetPlayerAssetOpLedgerResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.GetPlayerAssetOpLedger response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceGetPlayerAssetOpLedger(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerAssetOpLedgerRequest& request,
                                            GrpcDone<::data_service::GetPlayerAssetOpLedgerResponse> done, const GrpcCallOptions& options) {
    StartDataServiceGetPlayerAssetOpLedgerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceGetPlayerAssetOpLedgerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.GetPlayerAssetOpLedger requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::GetPlayerAssetOpLedgerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.GetPlayerAssetOpLedger: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::GetPlayerAssetOpLedgerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.GetPlayerAssetOpLedger: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceGetPlayerAssetOpLedgerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::GetPlayerAssetOpLedgerResponse* reply) { done(outcome, reply); });
}
#pragma endregion
#pragma region DataServiceGetPlayerInboxLedger
namespace {
void QueueLocalDataServiceGetPlayerInboxLedgerFailure(GrpcDone<::data_service::GetPlayerInboxLedgerResponse> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}

// 请求必须在这里同步序列化:gate 的共享原型在下次调用会被覆盖。
// 若将来改成异步序列化,必须先恢复请求的独立副本。
void StartDataServiceGetPlayerInboxLedgerCall(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerInboxLedgerRequest& request,
                                             GrpcSentMetadata metadata, std::chrono::milliseconds deadline,
                                             GrpcDone<::data_service::GetPlayerInboxLedgerResponse> done) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.GetPlayerInboxLedger requires a done callback";
    }
    muduo_grpc::UnaryCallRequest unaryRequest;
    unaryRequest.method = std::string(kDataServiceGetPlayerInboxLedgerGrpcPath);
    if (!request.SerializeToString(&unaryRequest.message)) {
        QueueLocalDataServiceGetPlayerInboxLedgerFailure(std::move(done), kGrpcStatusInternal, "failed to serialize the request");
        return;
    }
    const auto channel = FindGrpcChannel(registry, nodeEntity);
    if (!channel) {
        QueueLocalDataServiceGetPlayerInboxLedgerFailure(std::move(done), kGrpcStatusUnavailable, "node transport missing or closing");
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
        ::data_service::GetPlayerInboxLedgerResponse reply;
        if (!reply.ParseFromString(result.response)) {
            outcome.code = kGrpcStatusInternal;
            outcome.message = "failed to parse the DataService.GetPlayerInboxLedger response";
            done(outcome, nullptr);
            return;
        }
        done(outcome, &reply);
    });
}

void SendDataServiceGetPlayerInboxLedger(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerInboxLedgerRequest& request,
                                            GrpcDone<::data_service::GetPlayerInboxLedgerResponse> done, const GrpcCallOptions& options) {
    StartDataServiceGetPlayerInboxLedgerCall(registry, nodeEntity, request, options.metadata, options.deadline, std::move(done));
}

void SendDataServiceGetPlayerInboxLedgerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                                                  GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC DataService.GetPlayerInboxLedger requires a done callback";
    }
    if (message.GetDescriptor() != ::data_service::GetPlayerInboxLedgerRequest::descriptor()) {
        const auto requestTypeName = message.GetDescriptor()->full_name();
        LOG_ERROR << "gRPC DataService.GetPlayerInboxLedger: request type " << std::string(requestTypeName.data(), requestTypeName.size())
                  << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    // descriptor 相同仍可能是 DynamicMessage;进入 typed 路径前必须核对真实 C++ 类型。
    const auto* typedRequest = dynamic_cast<const ::data_service::GetPlayerInboxLedgerRequest*>(&message);
    if (typedRequest == nullptr) {
        LOG_ERROR << "gRPC DataService.GetPlayerInboxLedger: request C++ type does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    StartDataServiceGetPlayerInboxLedgerCall(registry, nodeEntity, *typedRequest, options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const ::data_service::GetPlayerInboxLedgerResponse* reply) { done(outcome, reply); });
}
#pragma endregion

void SetDataServiceCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

}// namespace data_service
