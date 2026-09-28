#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/match/match_service.grpc.pb.h"

#include "rpc/service_metadata/match_service_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace match {
using MatchServiceStubPtr = std::unique_ptr<MatchService::Stub>;
#pragma region MatchServiceJoinQueue

struct AsyncMatchServiceJoinQueueGrpcClient {
    uint32_t messageId{ MatchServiceJoinQueueMessageId };
    ClientContext context;
    Status status;
    ::match::JoinQueueResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::match::JoinQueueResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::JoinQueueRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceJoinQueueHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::JoinQueueResponse&)>;
extern AsyncMatchServiceJoinQueueHandlerFunctionType AsyncMatchServiceJoinQueueHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceJoinQueueFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::JoinQueueRequest&)>;
extern AsyncMatchServiceJoinQueueFailedHandlerFunctionType AsyncMatchServiceJoinQueueFailedHandler;

void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request);
void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceCancelQueue

struct AsyncMatchServiceCancelQueueGrpcClient {
    uint32_t messageId{ MatchServiceCancelQueueMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::CancelQueueRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceCancelQueueHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncMatchServiceCancelQueueHandlerFunctionType AsyncMatchServiceCancelQueueHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceCancelQueueFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::CancelQueueRequest&)>;
extern AsyncMatchServiceCancelQueueFailedHandlerFunctionType AsyncMatchServiceCancelQueueFailedHandler;

void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request);
void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceGetQueueStatus

struct AsyncMatchServiceGetQueueStatusGrpcClient {
    uint32_t messageId{ MatchServiceGetQueueStatusMessageId };
    ClientContext context;
    Status status;
    ::match::GetQueueStatusResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::match::GetQueueStatusResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::GetQueueStatusRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceGetQueueStatusHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::GetQueueStatusResponse&)>;
extern AsyncMatchServiceGetQueueStatusHandlerFunctionType AsyncMatchServiceGetQueueStatusHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceGetQueueStatusFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::GetQueueStatusRequest&)>;
extern AsyncMatchServiceGetQueueStatusFailedHandlerFunctionType AsyncMatchServiceGetQueueStatusFailedHandler;

void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request);
void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceChallengePlayer

struct AsyncMatchServiceChallengePlayerGrpcClient {
    uint32_t messageId{ MatchServiceChallengePlayerMessageId };
    ClientContext context;
    Status status;
    ::match::ChallengePlayerResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::match::ChallengePlayerResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::ChallengePlayerRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceChallengePlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::ChallengePlayerResponse&)>;
extern AsyncMatchServiceChallengePlayerHandlerFunctionType AsyncMatchServiceChallengePlayerHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceChallengePlayerFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::ChallengePlayerRequest&)>;
extern AsyncMatchServiceChallengePlayerFailedHandlerFunctionType AsyncMatchServiceChallengePlayerFailedHandler;

void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request);
void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceRespondChallenge

struct AsyncMatchServiceRespondChallengeGrpcClient {
    uint32_t messageId{ MatchServiceRespondChallengeMessageId };
    ClientContext context;
    Status status;
    ::match::RespondChallengeResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::match::RespondChallengeResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::RespondChallengeRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceRespondChallengeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::RespondChallengeResponse&)>;
extern AsyncMatchServiceRespondChallengeHandlerFunctionType AsyncMatchServiceRespondChallengeHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceRespondChallengeFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::RespondChallengeRequest&)>;
extern AsyncMatchServiceRespondChallengeFailedHandlerFunctionType AsyncMatchServiceRespondChallengeFailedHandler;

void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request);
void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceNotifyChallengeInvite

struct AsyncMatchServiceNotifyChallengeInviteGrpcClient {
    uint32_t messageId{ MatchServiceNotifyChallengeInviteMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::ChallengeInviteS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceNotifyChallengeInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncMatchServiceNotifyChallengeInviteHandlerFunctionType AsyncMatchServiceNotifyChallengeInviteHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceNotifyChallengeInviteFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::ChallengeInviteS2C&)>;
extern AsyncMatchServiceNotifyChallengeInviteFailedHandlerFunctionType AsyncMatchServiceNotifyChallengeInviteFailedHandler;

void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request);
void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceNotifyChallengeResult

struct AsyncMatchServiceNotifyChallengeResultGrpcClient {
    uint32_t messageId{ MatchServiceNotifyChallengeResultMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::ChallengeResultS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceNotifyChallengeResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncMatchServiceNotifyChallengeResultHandlerFunctionType AsyncMatchServiceNotifyChallengeResultHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceNotifyChallengeResultFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::ChallengeResultS2C&)>;
extern AsyncMatchServiceNotifyChallengeResultFailedHandlerFunctionType AsyncMatchServiceNotifyChallengeResultFailedHandler;

void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request);
void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceWatchBattle

struct AsyncMatchServiceWatchBattleGrpcClient {
    uint32_t messageId{ MatchServiceWatchBattleMessageId };
    ClientContext context;
    Status status;
    ::match::WatchBattleResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::match::WatchBattleResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::WatchBattleRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceWatchBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::WatchBattleResponse&)>;
extern AsyncMatchServiceWatchBattleHandlerFunctionType AsyncMatchServiceWatchBattleHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceWatchBattleFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::WatchBattleRequest&)>;
extern AsyncMatchServiceWatchBattleFailedHandlerFunctionType AsyncMatchServiceWatchBattleFailedHandler;

void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request);
void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceRequestBattleTicket

struct AsyncMatchServiceRequestBattleTicketGrpcClient {
    uint32_t messageId{ MatchServiceRequestBattleTicketMessageId };
    ClientContext context;
    Status status;
    ::RequestBattleTicketResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::RequestBattleTicketResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::RequestBattleTicketRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceRequestBattleTicketHandlerFunctionType =
    std::function<void(const ClientContext&, const ::RequestBattleTicketResponse&)>;
extern AsyncMatchServiceRequestBattleTicketHandlerFunctionType AsyncMatchServiceRequestBattleTicketHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceRequestBattleTicketFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::RequestBattleTicketRequest&)>;
extern AsyncMatchServiceRequestBattleTicketFailedHandlerFunctionType AsyncMatchServiceRequestBattleTicketFailedHandler;

void SendMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::RequestBattleTicketRequest& request);
void SendMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::RequestBattleTicketRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceListWatchableBattles

struct AsyncMatchServiceListWatchableBattlesGrpcClient {
    uint32_t messageId{ MatchServiceListWatchableBattlesMessageId };
    ClientContext context;
    Status status;
    ::match::ListWatchableBattlesResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::match::ListWatchableBattlesResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::match::ListWatchableBattlesRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncMatchServiceListWatchableBattlesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::ListWatchableBattlesResponse&)>;
extern AsyncMatchServiceListWatchableBattlesHandlerFunctionType AsyncMatchServiceListWatchableBattlesHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncMatchServiceListWatchableBattlesFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::match::ListWatchableBattlesRequest&)>;
extern AsyncMatchServiceListWatchableBattlesFailedHandlerFunctionType AsyncMatchServiceListWatchableBattlesFailedHandler;

void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request);
void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetMatchServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetMatchServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetMatchServiceFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetMatchServiceIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetMatchServiceCallDeadline(std::chrono::milliseconds deadline);
void HandleMatchServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitMatchServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace match
