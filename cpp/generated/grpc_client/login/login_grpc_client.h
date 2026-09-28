#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/login/login.grpc.pb.h"

#include "rpc/service_metadata/login_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace loginpb {
using ClientPlayerLoginStubPtr = std::unique_ptr<ClientPlayerLogin::Stub>;
#pragma region ClientPlayerLoginLogin

struct AsyncClientPlayerLoginLoginGrpcClient {
    uint32_t messageId{ ClientPlayerLoginLoginMessageId };
    ClientContext context;
    Status status;
    ::loginpb::LoginResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::LoginResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::LoginRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerLoginLoginHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginResponse&)>;
extern AsyncClientPlayerLoginLoginHandlerFunctionType AsyncClientPlayerLoginLoginHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerLoginLoginFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::LoginRequest&)>;
extern AsyncClientPlayerLoginLoginFailedHandlerFunctionType AsyncClientPlayerLoginLoginFailedHandler;

void SendClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginRequest& request);
void SendClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerLoginCreatePlayer

struct AsyncClientPlayerLoginCreatePlayerGrpcClient {
    uint32_t messageId{ ClientPlayerLoginCreatePlayerMessageId };
    ClientContext context;
    Status status;
    ::loginpb::CreatePlayerResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::CreatePlayerResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::CreatePlayerRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerLoginCreatePlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::CreatePlayerResponse&)>;
extern AsyncClientPlayerLoginCreatePlayerHandlerFunctionType AsyncClientPlayerLoginCreatePlayerHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerLoginCreatePlayerFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::CreatePlayerRequest&)>;
extern AsyncClientPlayerLoginCreatePlayerFailedHandlerFunctionType AsyncClientPlayerLoginCreatePlayerFailedHandler;

void SendClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CreatePlayerRequest& request);
void SendClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CreatePlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerLoginEnterGame

struct AsyncClientPlayerLoginEnterGameGrpcClient {
    uint32_t messageId{ ClientPlayerLoginEnterGameMessageId };
    ClientContext context;
    Status status;
    ::loginpb::EnterGameResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::EnterGameResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::EnterGameRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerLoginEnterGameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::EnterGameResponse&)>;
extern AsyncClientPlayerLoginEnterGameHandlerFunctionType AsyncClientPlayerLoginEnterGameHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerLoginEnterGameFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::EnterGameRequest&)>;
extern AsyncClientPlayerLoginEnterGameFailedHandlerFunctionType AsyncClientPlayerLoginEnterGameFailedHandler;

void SendClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::EnterGameRequest& request);
void SendClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::EnterGameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerLoginLeaveGame

struct AsyncClientPlayerLoginLeaveGameGrpcClient {
    uint32_t messageId{ ClientPlayerLoginLeaveGameMessageId };
    ClientContext context;
    Status status;
    ::loginpb::LoginEmptyResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::LoginEmptyResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::LeaveGameRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerLoginLeaveGameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginEmptyResponse&)>;
extern AsyncClientPlayerLoginLeaveGameHandlerFunctionType AsyncClientPlayerLoginLeaveGameHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerLoginLeaveGameFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::LeaveGameRequest&)>;
extern AsyncClientPlayerLoginLeaveGameFailedHandlerFunctionType AsyncClientPlayerLoginLeaveGameFailedHandler;

void SendClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LeaveGameRequest& request);
void SendClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LeaveGameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerLoginDisconnect

struct AsyncClientPlayerLoginDisconnectGrpcClient {
    uint32_t messageId{ ClientPlayerLoginDisconnectMessageId };
    ClientContext context;
    Status status;
    ::loginpb::LoginEmptyResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::LoginEmptyResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::LoginNodeDisconnectRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerLoginDisconnectHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginEmptyResponse&)>;
extern AsyncClientPlayerLoginDisconnectHandlerFunctionType AsyncClientPlayerLoginDisconnectHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerLoginDisconnectFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::LoginNodeDisconnectRequest&)>;
extern AsyncClientPlayerLoginDisconnectFailedHandlerFunctionType AsyncClientPlayerLoginDisconnectFailedHandler;

void SendClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginNodeDisconnectRequest& request);
void SendClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginNodeDisconnectRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerLoginRefreshToken

struct AsyncClientPlayerLoginRefreshTokenGrpcClient {
    uint32_t messageId{ ClientPlayerLoginRefreshTokenMessageId };
    ClientContext context;
    Status status;
    ::loginpb::RefreshTokenResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::RefreshTokenResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::RefreshTokenRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerLoginRefreshTokenHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::RefreshTokenResponse&)>;
extern AsyncClientPlayerLoginRefreshTokenHandlerFunctionType AsyncClientPlayerLoginRefreshTokenHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerLoginRefreshTokenFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::RefreshTokenRequest&)>;
extern AsyncClientPlayerLoginRefreshTokenFailedHandlerFunctionType AsyncClientPlayerLoginRefreshTokenFailedHandler;

void SendClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RefreshTokenRequest& request);
void SendClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RefreshTokenRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
using LoginPreGateStubPtr = std::unique_ptr<LoginPreGate::Stub>;
#pragma region LoginPreGateAssignGate

struct AsyncLoginPreGateAssignGateGrpcClient {
    uint32_t messageId{ LoginPreGateAssignGateMessageId };
    ClientContext context;
    Status status;
    ::loginpb::AssignGateResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::AssignGateResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::AssignGateRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncLoginPreGateAssignGateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::AssignGateResponse&)>;
extern AsyncLoginPreGateAssignGateHandlerFunctionType AsyncLoginPreGateAssignGateHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncLoginPreGateAssignGateFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::AssignGateRequest&)>;
extern AsyncLoginPreGateAssignGateFailedHandlerFunctionType AsyncLoginPreGateAssignGateFailedHandler;

void SendLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AssignGateRequest& request);
void SendLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AssignGateRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region LoginPreGateQueryQueueStatus

struct AsyncLoginPreGateQueryQueueStatusGrpcClient {
    uint32_t messageId{ LoginPreGateQueryQueueStatusMessageId };
    ClientContext context;
    Status status;
    ::loginpb::QueryQueueStatusResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::QueryQueueStatusResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::QueryQueueStatusRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncLoginPreGateQueryQueueStatusHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::QueryQueueStatusResponse&)>;
extern AsyncLoginPreGateQueryQueueStatusHandlerFunctionType AsyncLoginPreGateQueryQueueStatusHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncLoginPreGateQueryQueueStatusFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::QueryQueueStatusRequest&)>;
extern AsyncLoginPreGateQueryQueueStatusFailedHandlerFunctionType AsyncLoginPreGateQueryQueueStatusFailedHandler;

void SendLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::QueryQueueStatusRequest& request);
void SendLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::QueryQueueStatusRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
using LoginAdminStubPtr = std::unique_ptr<LoginAdmin::Stub>;
#pragma region LoginAdminRemovePlayersFromAccounts

struct AsyncLoginAdminRemovePlayersFromAccountsGrpcClient {
    uint32_t messageId{ LoginAdminRemovePlayersFromAccountsMessageId };
    ClientContext context;
    Status status;
    ::loginpb::RemovePlayersFromAccountsResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::loginpb::RemovePlayersFromAccountsResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::loginpb::RemovePlayersFromAccountsRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncLoginAdminRemovePlayersFromAccountsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::RemovePlayersFromAccountsResponse&)>;
extern AsyncLoginAdminRemovePlayersFromAccountsHandlerFunctionType AsyncLoginAdminRemovePlayersFromAccountsHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncLoginAdminRemovePlayersFromAccountsFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::loginpb::RemovePlayersFromAccountsRequest&)>;
extern AsyncLoginAdminRemovePlayersFromAccountsFailedHandlerFunctionType AsyncLoginAdminRemovePlayersFromAccountsFailedHandler;

void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request);
void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetLoginHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetLoginIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetLoginFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetLoginIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetLoginCallDeadline(std::chrono::milliseconds deadline);
void HandleLoginCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitLoginGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace loginpb
