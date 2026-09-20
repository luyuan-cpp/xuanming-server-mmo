#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/team/team.grpc.pb.h"

#include "rpc/service_metadata/team_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace teampb {
using ClientPlayerTeamStubPtr = std::unique_ptr<ClientPlayerTeam::Stub>;
#pragma region ClientPlayerTeamCreateTeam

struct AsyncClientPlayerTeamCreateTeamGrpcClient {
    uint32_t messageId{ ClientPlayerTeamCreateTeamMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::CreateTeamRequest;
using AsyncClientPlayerTeamCreateTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamCreateTeamHandlerFunctionType AsyncClientPlayerTeamCreateTeamHandler;

void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::CreateTeamRequest& request);
void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::CreateTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamGetMyTeam

struct AsyncClientPlayerTeamGetMyTeamGrpcClient {
    uint32_t messageId{ ClientPlayerTeamGetMyTeamMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::GetMyTeamRequest;
using AsyncClientPlayerTeamGetMyTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamGetMyTeamHandlerFunctionType AsyncClientPlayerTeamGetMyTeamHandler;

void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::GetMyTeamRequest& request);
void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::GetMyTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamApplyJoinTeam

struct AsyncClientPlayerTeamApplyJoinTeamGrpcClient {
    uint32_t messageId{ ClientPlayerTeamApplyJoinTeamMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::ApplyJoinTeamRequest;
using AsyncClientPlayerTeamApplyJoinTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamApplyJoinTeamHandlerFunctionType AsyncClientPlayerTeamApplyJoinTeamHandler;

void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ApplyJoinTeamRequest& request);
void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ApplyJoinTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamHandleApplication

struct AsyncClientPlayerTeamHandleApplicationGrpcClient {
    uint32_t messageId{ ClientPlayerTeamHandleApplicationMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::HandleApplicationRequest;
using AsyncClientPlayerTeamHandleApplicationHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamHandleApplicationHandlerFunctionType AsyncClientPlayerTeamHandleApplicationHandler;

void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const ::teampb::HandleApplicationRequest& request);
void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const ::teampb::HandleApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamInviteToTeam

struct AsyncClientPlayerTeamInviteToTeamGrpcClient {
    uint32_t messageId{ ClientPlayerTeamInviteToTeamMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::InviteToTeamRequest;
using AsyncClientPlayerTeamInviteToTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamInviteToTeamHandlerFunctionType AsyncClientPlayerTeamInviteToTeamHandler;

void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::InviteToTeamRequest& request);
void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::InviteToTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamRespondInvite

struct AsyncClientPlayerTeamRespondInviteGrpcClient {
    uint32_t messageId{ ClientPlayerTeamRespondInviteMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::RespondInviteRequest;
using AsyncClientPlayerTeamRespondInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamRespondInviteHandlerFunctionType AsyncClientPlayerTeamRespondInviteHandler;

void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::RespondInviteRequest& request);
void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::RespondInviteRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamListMyInvites

struct AsyncClientPlayerTeamListMyInvitesGrpcClient {
    uint32_t messageId{ ClientPlayerTeamListMyInvitesMessageId };
    ClientContext context;
    Status status;
    ::teampb::ListMyInvitesResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::ListMyInvitesResponse>> response_reader;
};

class ::teampb::ListMyInvitesRequest;
using AsyncClientPlayerTeamListMyInvitesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::ListMyInvitesResponse&)>;
extern AsyncClientPlayerTeamListMyInvitesHandlerFunctionType AsyncClientPlayerTeamListMyInvitesHandler;

void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ListMyInvitesRequest& request);
void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ListMyInvitesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamLeaveTeam

struct AsyncClientPlayerTeamLeaveTeamGrpcClient {
    uint32_t messageId{ ClientPlayerTeamLeaveTeamMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::LeaveTeamRequest;
using AsyncClientPlayerTeamLeaveTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamLeaveTeamHandlerFunctionType AsyncClientPlayerTeamLeaveTeamHandler;

void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::LeaveTeamRequest& request);
void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::LeaveTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamKickMember

struct AsyncClientPlayerTeamKickMemberGrpcClient {
    uint32_t messageId{ ClientPlayerTeamKickMemberMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::KickMemberRequest;
using AsyncClientPlayerTeamKickMemberHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamKickMemberHandlerFunctionType AsyncClientPlayerTeamKickMemberHandler;

void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const ::teampb::KickMemberRequest& request);
void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const ::teampb::KickMemberRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamTransferLeader

struct AsyncClientPlayerTeamTransferLeaderGrpcClient {
    uint32_t messageId{ ClientPlayerTeamTransferLeaderMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::TransferLeaderRequest;
using AsyncClientPlayerTeamTransferLeaderHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamTransferLeaderHandlerFunctionType AsyncClientPlayerTeamTransferLeaderHandler;

void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TransferLeaderRequest& request);
void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TransferLeaderRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamDisbandTeam

struct AsyncClientPlayerTeamDisbandTeamGrpcClient {
    uint32_t messageId{ ClientPlayerTeamDisbandTeamMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::DisbandTeamRequest;
using AsyncClientPlayerTeamDisbandTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamDisbandTeamHandlerFunctionType AsyncClientPlayerTeamDisbandTeamHandler;

void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::DisbandTeamRequest& request);
void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::DisbandTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamStartTeamMatch

struct AsyncClientPlayerTeamStartTeamMatchGrpcClient {
    uint32_t messageId{ ClientPlayerTeamStartTeamMatchMessageId };
    ClientContext context;
    Status status;
    ::teampb::TeamResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::teampb::TeamResponse>> response_reader;
};

class ::teampb::StartTeamMatchRequest;
using AsyncClientPlayerTeamStartTeamMatchHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamStartTeamMatchHandlerFunctionType AsyncClientPlayerTeamStartTeamMatchHandler;

void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const ::teampb::StartTeamMatchRequest& request);
void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const ::teampb::StartTeamMatchRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamSnapshot

struct AsyncClientPlayerTeamNotifyTeamSnapshotGrpcClient {
    uint32_t messageId{ ClientPlayerTeamNotifyTeamSnapshotMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::teampb::TeamSnapshotS2C;
using AsyncClientPlayerTeamNotifyTeamSnapshotHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncClientPlayerTeamNotifyTeamSnapshotHandlerFunctionType AsyncClientPlayerTeamNotifyTeamSnapshotHandler;

void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamSnapshotS2C& request);
void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamSnapshotS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamInvite

struct AsyncClientPlayerTeamNotifyTeamInviteGrpcClient {
    uint32_t messageId{ ClientPlayerTeamNotifyTeamInviteMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::teampb::TeamInviteS2C;
using AsyncClientPlayerTeamNotifyTeamInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncClientPlayerTeamNotifyTeamInviteHandlerFunctionType AsyncClientPlayerTeamNotifyTeamInviteHandler;

void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamInviteS2C& request);
void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamInviteS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamEvent

struct AsyncClientPlayerTeamNotifyTeamEventGrpcClient {
    uint32_t messageId{ ClientPlayerTeamNotifyTeamEventMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::teampb::TeamEventS2C;
using AsyncClientPlayerTeamNotifyTeamEventHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncClientPlayerTeamNotifyTeamEventHandlerFunctionType AsyncClientPlayerTeamNotifyTeamEventHandler;

void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request);
void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetTeamHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetTeamIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleTeamCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitTeamGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace teampb
