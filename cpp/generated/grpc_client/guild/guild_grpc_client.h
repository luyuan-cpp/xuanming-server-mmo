#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/guild/guild.grpc.pb.h"

#include "rpc/service_metadata/guild_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace guildpb {
using GuildServiceStubPtr = std::unique_ptr<GuildService::Stub>;
#pragma region GuildServiceCreateGuild

struct AsyncGuildServiceCreateGuildGrpcClient {
    uint32_t messageId{ GuildServiceCreateGuildMessageId };
    ClientContext context;
    Status status;
    ::guildpb::CreateGuildResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::CreateGuildResponse>> response_reader;
};

class ::guildpb::CreateGuildRequest;
using AsyncGuildServiceCreateGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::CreateGuildResponse&)>;
extern AsyncGuildServiceCreateGuildHandlerFunctionType AsyncGuildServiceCreateGuildHandler;

void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request);
void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceGetGuild

struct AsyncGuildServiceGetGuildGrpcClient {
    uint32_t messageId{ GuildServiceGetGuildMessageId };
    ClientContext context;
    Status status;
    ::guildpb::GetGuildResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::GetGuildResponse>> response_reader;
};

class ::guildpb::GetGuildRequest;
using AsyncGuildServiceGetGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildResponse&)>;
extern AsyncGuildServiceGetGuildHandlerFunctionType AsyncGuildServiceGetGuildHandler;

void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request);
void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceGetPlayerGuild

struct AsyncGuildServiceGetPlayerGuildGrpcClient {
    uint32_t messageId{ GuildServiceGetPlayerGuildMessageId };
    ClientContext context;
    Status status;
    ::guildpb::GetPlayerGuildResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::GetPlayerGuildResponse>> response_reader;
};

class ::guildpb::GetPlayerGuildRequest;
using AsyncGuildServiceGetPlayerGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetPlayerGuildResponse&)>;
extern AsyncGuildServiceGetPlayerGuildHandlerFunctionType AsyncGuildServiceGetPlayerGuildHandler;

void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request);
void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceLeaveGuild

struct AsyncGuildServiceLeaveGuildGrpcClient {
    uint32_t messageId{ GuildServiceLeaveGuildMessageId };
    ClientContext context;
    Status status;
    ::guildpb::LeaveGuildResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::LeaveGuildResponse>> response_reader;
};

class ::guildpb::LeaveGuildRequest;
using AsyncGuildServiceLeaveGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::LeaveGuildResponse&)>;
extern AsyncGuildServiceLeaveGuildHandlerFunctionType AsyncGuildServiceLeaveGuildHandler;

void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request);
void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceDisbandGuild

struct AsyncGuildServiceDisbandGuildGrpcClient {
    uint32_t messageId{ GuildServiceDisbandGuildMessageId };
    ClientContext context;
    Status status;
    ::guildpb::DisbandGuildResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::DisbandGuildResponse>> response_reader;
};

class ::guildpb::DisbandGuildRequest;
using AsyncGuildServiceDisbandGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::DisbandGuildResponse&)>;
extern AsyncGuildServiceDisbandGuildHandlerFunctionType AsyncGuildServiceDisbandGuildHandler;

void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request);
void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceSetAnnouncement

struct AsyncGuildServiceSetAnnouncementGrpcClient {
    uint32_t messageId{ GuildServiceSetAnnouncementMessageId };
    ClientContext context;
    Status status;
    ::guildpb::SetAnnouncementResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::SetAnnouncementResponse>> response_reader;
};

class ::guildpb::SetAnnouncementRequest;
using AsyncGuildServiceSetAnnouncementHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::SetAnnouncementResponse&)>;
extern AsyncGuildServiceSetAnnouncementHandlerFunctionType AsyncGuildServiceSetAnnouncementHandler;

void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request);
void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceSetGuildMemberRole

struct AsyncGuildServiceSetGuildMemberRoleGrpcClient {
    uint32_t messageId{ GuildServiceSetGuildMemberRoleMessageId };
    ClientContext context;
    Status status;
    ::guildpb::SetGuildMemberRoleResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::SetGuildMemberRoleResponse>> response_reader;
};

class ::guildpb::SetGuildMemberRoleRequest;
using AsyncGuildServiceSetGuildMemberRoleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::SetGuildMemberRoleResponse&)>;
extern AsyncGuildServiceSetGuildMemberRoleHandlerFunctionType AsyncGuildServiceSetGuildMemberRoleHandler;

void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request);
void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceKickGuildMember

struct AsyncGuildServiceKickGuildMemberGrpcClient {
    uint32_t messageId{ GuildServiceKickGuildMemberMessageId };
    ClientContext context;
    Status status;
    ::guildpb::KickGuildMemberResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::KickGuildMemberResponse>> response_reader;
};

class ::guildpb::KickGuildMemberRequest;
using AsyncGuildServiceKickGuildMemberHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::KickGuildMemberResponse&)>;
extern AsyncGuildServiceKickGuildMemberHandlerFunctionType AsyncGuildServiceKickGuildMemberHandler;

void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request);
void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceTransferGuildLeader

struct AsyncGuildServiceTransferGuildLeaderGrpcClient {
    uint32_t messageId{ GuildServiceTransferGuildLeaderMessageId };
    ClientContext context;
    Status status;
    ::guildpb::TransferGuildLeaderResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::TransferGuildLeaderResponse>> response_reader;
};

class ::guildpb::TransferGuildLeaderRequest;
using AsyncGuildServiceTransferGuildLeaderHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::TransferGuildLeaderResponse&)>;
extern AsyncGuildServiceTransferGuildLeaderHandlerFunctionType AsyncGuildServiceTransferGuildLeaderHandler;

void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request);
void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceApplyJoinGuild

struct AsyncGuildServiceApplyJoinGuildGrpcClient {
    uint32_t messageId{ GuildServiceApplyJoinGuildMessageId };
    ClientContext context;
    Status status;
    ::guildpb::ApplyJoinGuildResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::ApplyJoinGuildResponse>> response_reader;
};

class ::guildpb::ApplyJoinGuildRequest;
using AsyncGuildServiceApplyJoinGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ApplyJoinGuildResponse&)>;
extern AsyncGuildServiceApplyJoinGuildHandlerFunctionType AsyncGuildServiceApplyJoinGuildHandler;

void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request);
void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceCancelGuildApplication

struct AsyncGuildServiceCancelGuildApplicationGrpcClient {
    uint32_t messageId{ GuildServiceCancelGuildApplicationMessageId };
    ClientContext context;
    Status status;
    ::guildpb::CancelGuildApplicationResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::CancelGuildApplicationResponse>> response_reader;
};

class ::guildpb::CancelGuildApplicationRequest;
using AsyncGuildServiceCancelGuildApplicationHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::CancelGuildApplicationResponse&)>;
extern AsyncGuildServiceCancelGuildApplicationHandlerFunctionType AsyncGuildServiceCancelGuildApplicationHandler;

void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request);
void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceListMyGuildApplications

struct AsyncGuildServiceListMyGuildApplicationsGrpcClient {
    uint32_t messageId{ GuildServiceListMyGuildApplicationsMessageId };
    ClientContext context;
    Status status;
    ::guildpb::ListMyGuildApplicationsResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::ListMyGuildApplicationsResponse>> response_reader;
};

class ::guildpb::ListMyGuildApplicationsRequest;
using AsyncGuildServiceListMyGuildApplicationsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ListMyGuildApplicationsResponse&)>;
extern AsyncGuildServiceListMyGuildApplicationsHandlerFunctionType AsyncGuildServiceListMyGuildApplicationsHandler;

void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request);
void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceListGuildApplications

struct AsyncGuildServiceListGuildApplicationsGrpcClient {
    uint32_t messageId{ GuildServiceListGuildApplicationsMessageId };
    ClientContext context;
    Status status;
    ::guildpb::ListGuildApplicationsResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::ListGuildApplicationsResponse>> response_reader;
};

class ::guildpb::ListGuildApplicationsRequest;
using AsyncGuildServiceListGuildApplicationsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ListGuildApplicationsResponse&)>;
extern AsyncGuildServiceListGuildApplicationsHandlerFunctionType AsyncGuildServiceListGuildApplicationsHandler;

void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request);
void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceReviewGuildApplication

struct AsyncGuildServiceReviewGuildApplicationGrpcClient {
    uint32_t messageId{ GuildServiceReviewGuildApplicationMessageId };
    ClientContext context;
    Status status;
    ::guildpb::ReviewGuildApplicationResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::ReviewGuildApplicationResponse>> response_reader;
};

class ::guildpb::ReviewGuildApplicationRequest;
using AsyncGuildServiceReviewGuildApplicationHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ReviewGuildApplicationResponse&)>;
extern AsyncGuildServiceReviewGuildApplicationHandlerFunctionType AsyncGuildServiceReviewGuildApplicationHandler;

void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request);
void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceNotifyGuildChanged

struct AsyncGuildServiceNotifyGuildChangedGrpcClient {
    uint32_t messageId{ GuildServiceNotifyGuildChangedMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::guildpb::GuildChangedS2C;
using AsyncGuildServiceNotifyGuildChangedHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncGuildServiceNotifyGuildChangedHandlerFunctionType AsyncGuildServiceNotifyGuildChangedHandler;

void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request);
void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceUpdateGuildScore

struct AsyncGuildServiceUpdateGuildScoreGrpcClient {
    uint32_t messageId{ GuildServiceUpdateGuildScoreMessageId };
    ClientContext context;
    Status status;
    ::guildpb::UpdateGuildScoreResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::UpdateGuildScoreResponse>> response_reader;
};

class ::guildpb::UpdateGuildScoreRequest;
using AsyncGuildServiceUpdateGuildScoreHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::UpdateGuildScoreResponse&)>;
extern AsyncGuildServiceUpdateGuildScoreHandlerFunctionType AsyncGuildServiceUpdateGuildScoreHandler;

void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request);
void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceGetGuildRank

struct AsyncGuildServiceGetGuildRankGrpcClient {
    uint32_t messageId{ GuildServiceGetGuildRankMessageId };
    ClientContext context;
    Status status;
    ::guildpb::GetGuildRankResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::GetGuildRankResponse>> response_reader;
};

class ::guildpb::GetGuildRankRequest;
using AsyncGuildServiceGetGuildRankHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildRankResponse&)>;
extern AsyncGuildServiceGetGuildRankHandlerFunctionType AsyncGuildServiceGetGuildRankHandler;

void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request);
void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region GuildServiceGetGuildRankByGuild

struct AsyncGuildServiceGetGuildRankByGuildGrpcClient {
    uint32_t messageId{ GuildServiceGetGuildRankByGuildMessageId };
    ClientContext context;
    Status status;
    ::guildpb::GetGuildRankByGuildResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::guildpb::GetGuildRankByGuildResponse>> response_reader;
};

class ::guildpb::GetGuildRankByGuildRequest;
using AsyncGuildServiceGetGuildRankByGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildRankByGuildResponse&)>;
extern AsyncGuildServiceGetGuildRankByGuildHandlerFunctionType AsyncGuildServiceGetGuildRankByGuildHandler;

void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request);
void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetGuildHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetGuildIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleGuildCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitGuildGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace guildpb
