#pragma once
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
};

class ::match::JoinQueueRequest;
using AsyncMatchServiceJoinQueueHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::JoinQueueResponse&)>;
extern AsyncMatchServiceJoinQueueHandlerFunctionType AsyncMatchServiceJoinQueueHandler;

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
};

class ::match::CancelQueueRequest;
using AsyncMatchServiceCancelQueueHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncMatchServiceCancelQueueHandlerFunctionType AsyncMatchServiceCancelQueueHandler;

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
};

class ::match::GetQueueStatusRequest;
using AsyncMatchServiceGetQueueStatusHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::GetQueueStatusResponse&)>;
extern AsyncMatchServiceGetQueueStatusHandlerFunctionType AsyncMatchServiceGetQueueStatusHandler;

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
};

class ::match::ChallengePlayerRequest;
using AsyncMatchServiceChallengePlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::ChallengePlayerResponse&)>;
extern AsyncMatchServiceChallengePlayerHandlerFunctionType AsyncMatchServiceChallengePlayerHandler;

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
};

class ::match::RespondChallengeRequest;
using AsyncMatchServiceRespondChallengeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::RespondChallengeResponse&)>;
extern AsyncMatchServiceRespondChallengeHandlerFunctionType AsyncMatchServiceRespondChallengeHandler;

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
};

class ::match::ChallengeInviteS2C;
using AsyncMatchServiceNotifyChallengeInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncMatchServiceNotifyChallengeInviteHandlerFunctionType AsyncMatchServiceNotifyChallengeInviteHandler;

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
};

class ::match::ChallengeResultS2C;
using AsyncMatchServiceNotifyChallengeResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncMatchServiceNotifyChallengeResultHandlerFunctionType AsyncMatchServiceNotifyChallengeResultHandler;

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
};

class ::match::WatchBattleRequest;
using AsyncMatchServiceWatchBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::WatchBattleResponse&)>;
extern AsyncMatchServiceWatchBattleHandlerFunctionType AsyncMatchServiceWatchBattleHandler;

void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request);
void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region MatchServiceListWatchableBattles

struct AsyncMatchServiceListWatchableBattlesGrpcClient {
    uint32_t messageId{ MatchServiceListWatchableBattlesMessageId };
    ClientContext context;
    Status status;
    ::match::ListWatchableBattlesResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::match::ListWatchableBattlesResponse>> response_reader;
};

class ::match::ListWatchableBattlesRequest;
using AsyncMatchServiceListWatchableBattlesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::ListWatchableBattlesResponse&)>;
extern AsyncMatchServiceListWatchableBattlesHandlerFunctionType AsyncMatchServiceListWatchableBattlesHandler;

void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request);
void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetMatchServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetMatchServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleMatchServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitMatchServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace match
