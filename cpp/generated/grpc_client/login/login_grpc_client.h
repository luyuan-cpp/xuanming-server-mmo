#pragma once
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
};

using AsyncClientPlayerLoginLoginHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginResponse&)>;
extern AsyncClientPlayerLoginLoginHandlerFunctionType AsyncClientPlayerLoginLoginHandler;

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
};

using AsyncClientPlayerLoginCreatePlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::CreatePlayerResponse&)>;
extern AsyncClientPlayerLoginCreatePlayerHandlerFunctionType AsyncClientPlayerLoginCreatePlayerHandler;

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
};

using AsyncClientPlayerLoginEnterGameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::EnterGameResponse&)>;
extern AsyncClientPlayerLoginEnterGameHandlerFunctionType AsyncClientPlayerLoginEnterGameHandler;

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
};

using AsyncClientPlayerLoginLeaveGameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginEmptyResponse&)>;
extern AsyncClientPlayerLoginLeaveGameHandlerFunctionType AsyncClientPlayerLoginLeaveGameHandler;

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
};

using AsyncClientPlayerLoginDisconnectHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginEmptyResponse&)>;
extern AsyncClientPlayerLoginDisconnectHandlerFunctionType AsyncClientPlayerLoginDisconnectHandler;

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
};

using AsyncClientPlayerLoginRefreshTokenHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::RefreshTokenResponse&)>;
extern AsyncClientPlayerLoginRefreshTokenHandlerFunctionType AsyncClientPlayerLoginRefreshTokenHandler;

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
};

using AsyncLoginPreGateAssignGateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::AssignGateResponse&)>;
extern AsyncLoginPreGateAssignGateHandlerFunctionType AsyncLoginPreGateAssignGateHandler;

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
};

using AsyncLoginPreGateQueryQueueStatusHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::QueryQueueStatusResponse&)>;
extern AsyncLoginPreGateQueryQueueStatusHandlerFunctionType AsyncLoginPreGateQueryQueueStatusHandler;

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
};

using AsyncLoginAdminRemovePlayersFromAccountsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::RemovePlayersFromAccountsResponse&)>;
extern AsyncLoginAdminRemovePlayersFromAccountsHandlerFunctionType AsyncLoginAdminRemovePlayersFromAccountsHandler;

void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request);
void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetLoginHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetLoginIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleLoginCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitLoginGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace loginpb
