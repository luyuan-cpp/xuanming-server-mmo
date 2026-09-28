#pragma once
#include <chrono>
#include <functional>
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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::CreateTeamRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamCreateTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamCreateTeamHandlerFunctionType AsyncClientPlayerTeamCreateTeamHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamCreateTeamFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::CreateTeamRequest&)>;
extern AsyncClientPlayerTeamCreateTeamFailedHandlerFunctionType AsyncClientPlayerTeamCreateTeamFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::GetMyTeamRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamGetMyTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamGetMyTeamHandlerFunctionType AsyncClientPlayerTeamGetMyTeamHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamGetMyTeamFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::GetMyTeamRequest&)>;
extern AsyncClientPlayerTeamGetMyTeamFailedHandlerFunctionType AsyncClientPlayerTeamGetMyTeamFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::ApplyJoinTeamRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamApplyJoinTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamApplyJoinTeamHandlerFunctionType AsyncClientPlayerTeamApplyJoinTeamHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamApplyJoinTeamFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::ApplyJoinTeamRequest&)>;
extern AsyncClientPlayerTeamApplyJoinTeamFailedHandlerFunctionType AsyncClientPlayerTeamApplyJoinTeamFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::HandleApplicationRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamHandleApplicationHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamHandleApplicationHandlerFunctionType AsyncClientPlayerTeamHandleApplicationHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamHandleApplicationFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::HandleApplicationRequest&)>;
extern AsyncClientPlayerTeamHandleApplicationFailedHandlerFunctionType AsyncClientPlayerTeamHandleApplicationFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::InviteToTeamRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamInviteToTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamInviteToTeamHandlerFunctionType AsyncClientPlayerTeamInviteToTeamHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamInviteToTeamFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::InviteToTeamRequest&)>;
extern AsyncClientPlayerTeamInviteToTeamFailedHandlerFunctionType AsyncClientPlayerTeamInviteToTeamFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::RespondInviteRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamRespondInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamRespondInviteHandlerFunctionType AsyncClientPlayerTeamRespondInviteHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamRespondInviteFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::RespondInviteRequest&)>;
extern AsyncClientPlayerTeamRespondInviteFailedHandlerFunctionType AsyncClientPlayerTeamRespondInviteFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::ListMyInvitesRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamListMyInvitesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::ListMyInvitesResponse&)>;
extern AsyncClientPlayerTeamListMyInvitesHandlerFunctionType AsyncClientPlayerTeamListMyInvitesHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamListMyInvitesFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::ListMyInvitesRequest&)>;
extern AsyncClientPlayerTeamListMyInvitesFailedHandlerFunctionType AsyncClientPlayerTeamListMyInvitesFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::LeaveTeamRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamLeaveTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamLeaveTeamHandlerFunctionType AsyncClientPlayerTeamLeaveTeamHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamLeaveTeamFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::LeaveTeamRequest&)>;
extern AsyncClientPlayerTeamLeaveTeamFailedHandlerFunctionType AsyncClientPlayerTeamLeaveTeamFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::KickMemberRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamKickMemberHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamKickMemberHandlerFunctionType AsyncClientPlayerTeamKickMemberHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamKickMemberFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::KickMemberRequest&)>;
extern AsyncClientPlayerTeamKickMemberFailedHandlerFunctionType AsyncClientPlayerTeamKickMemberFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::TransferLeaderRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamTransferLeaderHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamTransferLeaderHandlerFunctionType AsyncClientPlayerTeamTransferLeaderHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamTransferLeaderFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::TransferLeaderRequest&)>;
extern AsyncClientPlayerTeamTransferLeaderFailedHandlerFunctionType AsyncClientPlayerTeamTransferLeaderFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::DisbandTeamRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamDisbandTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamDisbandTeamHandlerFunctionType AsyncClientPlayerTeamDisbandTeamHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamDisbandTeamFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::DisbandTeamRequest&)>;
extern AsyncClientPlayerTeamDisbandTeamFailedHandlerFunctionType AsyncClientPlayerTeamDisbandTeamFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::StartTeamMatchRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamStartTeamMatchHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
extern AsyncClientPlayerTeamStartTeamMatchHandlerFunctionType AsyncClientPlayerTeamStartTeamMatchHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamStartTeamMatchFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::StartTeamMatchRequest&)>;
extern AsyncClientPlayerTeamStartTeamMatchFailedHandlerFunctionType AsyncClientPlayerTeamStartTeamMatchFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::TeamSnapshotS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamNotifyTeamSnapshotHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncClientPlayerTeamNotifyTeamSnapshotHandlerFunctionType AsyncClientPlayerTeamNotifyTeamSnapshotHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::TeamSnapshotS2C&)>;
extern AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandlerFunctionType AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::TeamInviteS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamNotifyTeamInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncClientPlayerTeamNotifyTeamInviteHandlerFunctionType AsyncClientPlayerTeamNotifyTeamInviteHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamNotifyTeamInviteFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::TeamInviteS2C&)>;
extern AsyncClientPlayerTeamNotifyTeamInviteFailedHandlerFunctionType AsyncClientPlayerTeamNotifyTeamInviteFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::teampb::TeamEventS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerTeamNotifyTeamEventHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncClientPlayerTeamNotifyTeamEventHandlerFunctionType AsyncClientPlayerTeamNotifyTeamEventHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerTeamNotifyTeamEventFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::teampb::TeamEventS2C&)>;
extern AsyncClientPlayerTeamNotifyTeamEventFailedHandlerFunctionType AsyncClientPlayerTeamNotifyTeamEventFailedHandler;

void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request);
void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetTeamHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetTeamIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetTeamFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetTeamIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetTeamCallDeadline(std::chrono::milliseconds deadline);
void HandleTeamCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitTeamGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace teampb
