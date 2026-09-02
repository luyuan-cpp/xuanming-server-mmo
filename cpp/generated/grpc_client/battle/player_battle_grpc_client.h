#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/battle/player_battle.grpc.pb.h"

#include "rpc/service_metadata/player_battle_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

using BattleClientPlayerStubPtr = std::unique_ptr<BattleClientPlayer::Stub>;
#pragma region BattleClientPlayerSubmitBattleAction

struct AsyncBattleClientPlayerSubmitBattleActionGrpcClient {
    uint32_t messageId{ BattleClientPlayerSubmitBattleActionMessageId };
    ClientContext context;
    Status status;
    ::SubmitBattleActionResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::SubmitBattleActionResponse>> response_reader;
};

class ::SubmitBattleActionRequest;
using AsyncBattleClientPlayerSubmitBattleActionHandlerFunctionType =
    std::function<void(const ClientContext&, const ::SubmitBattleActionResponse&)>;
extern AsyncBattleClientPlayerSubmitBattleActionHandlerFunctionType AsyncBattleClientPlayerSubmitBattleActionHandler;

void SendBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, const ::SubmitBattleActionRequest& request);
void SendBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, const ::SubmitBattleActionRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerGetBattleState

struct AsyncBattleClientPlayerGetBattleStateGrpcClient {
    uint32_t messageId{ BattleClientPlayerGetBattleStateMessageId };
    ClientContext context;
    Status status;
    ::BattleStateS2C reply;
    std::unique_ptr<ClientAsyncResponseReader<::BattleStateS2C>> response_reader;
};

class ::GetBattleStateRequest;
using AsyncBattleClientPlayerGetBattleStateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::BattleStateS2C&)>;
extern AsyncBattleClientPlayerGetBattleStateHandlerFunctionType AsyncBattleClientPlayerGetBattleStateHandler;

void SendBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, const ::GetBattleStateRequest& request);
void SendBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, const ::GetBattleStateRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleStart

struct AsyncBattleClientPlayerNotifyBattleStartGrpcClient {
    uint32_t messageId{ BattleClientPlayerNotifyBattleStartMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::BattleStartS2C;
using AsyncBattleClientPlayerNotifyBattleStartHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyBattleStartHandlerFunctionType AsyncBattleClientPlayerNotifyBattleStartHandler;

void SendBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, const ::BattleStartS2C& request);
void SendBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, const ::BattleStartS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerNotifyTurnResult

struct AsyncBattleClientPlayerNotifyTurnResultGrpcClient {
    uint32_t messageId{ BattleClientPlayerNotifyTurnResultMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::TurnResultS2C;
using AsyncBattleClientPlayerNotifyTurnResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyTurnResultHandlerFunctionType AsyncBattleClientPlayerNotifyTurnResultHandler;

void SendBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request);
void SendBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleEnd

struct AsyncBattleClientPlayerNotifyBattleEndGrpcClient {
    uint32_t messageId{ BattleClientPlayerNotifyBattleEndMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::BattleEndS2C;
using AsyncBattleClientPlayerNotifyBattleEndHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyBattleEndHandlerFunctionType AsyncBattleClientPlayerNotifyBattleEndHandler;

void SendBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, const ::BattleEndS2C& request);
void SendBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, const ::BattleEndS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleReconnect

struct AsyncBattleClientPlayerNotifyBattleReconnectGrpcClient {
    uint32_t messageId{ BattleClientPlayerNotifyBattleReconnectMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::BattleReconnectS2C;
using AsyncBattleClientPlayerNotifyBattleReconnectHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyBattleReconnectHandlerFunctionType AsyncBattleClientPlayerNotifyBattleReconnectHandler;

void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, const ::BattleReconnectS2C& request);
void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, const ::BattleReconnectS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerStopWatchBattle

struct AsyncBattleClientPlayerStopWatchBattleGrpcClient {
    uint32_t messageId{ BattleClientPlayerStopWatchBattleMessageId };
    ClientContext context;
    Status status;
    ::StopWatchBattleResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::StopWatchBattleResponse>> response_reader;
};

class ::StopWatchBattleRequest;
using AsyncBattleClientPlayerStopWatchBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::StopWatchBattleResponse&)>;
extern AsyncBattleClientPlayerStopWatchBattleHandlerFunctionType AsyncBattleClientPlayerStopWatchBattleHandler;

void SendBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::StopWatchBattleRequest& request);
void SendBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::StopWatchBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerSetAutoBattle

struct AsyncBattleClientPlayerSetAutoBattleGrpcClient {
    uint32_t messageId{ BattleClientPlayerSetAutoBattleMessageId };
    ClientContext context;
    Status status;
    ::SetAutoBattleResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::SetAutoBattleResponse>> response_reader;
};

class ::SetAutoBattleRequest;
using AsyncBattleClientPlayerSetAutoBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::SetAutoBattleResponse&)>;
extern AsyncBattleClientPlayerSetAutoBattleHandlerFunctionType AsyncBattleClientPlayerSetAutoBattleHandler;

void SendBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, const ::SetAutoBattleRequest& request);
void SendBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, const ::SetAutoBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateState

struct AsyncBattleClientPlayerNotifySpectateStateGrpcClient {
    uint32_t messageId{ BattleClientPlayerNotifySpectateStateMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::SpectateStateS2C;
using AsyncBattleClientPlayerNotifySpectateStateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifySpectateStateHandlerFunctionType AsyncBattleClientPlayerNotifySpectateStateHandler;

void SendBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, const ::SpectateStateS2C& request);
void SendBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, const ::SpectateStateS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateTurnResult

struct AsyncBattleClientPlayerNotifySpectateTurnResultGrpcClient {
    uint32_t messageId{ BattleClientPlayerNotifySpectateTurnResultMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::TurnResultS2C;
using AsyncBattleClientPlayerNotifySpectateTurnResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifySpectateTurnResultHandlerFunctionType AsyncBattleClientPlayerNotifySpectateTurnResultHandler;

void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request);
void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateEnd

struct AsyncBattleClientPlayerNotifySpectateEndGrpcClient {
    uint32_t messageId{ BattleClientPlayerNotifySpectateEndMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::SpectateEndS2C;
using AsyncBattleClientPlayerNotifySpectateEndHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifySpectateEndHandlerFunctionType AsyncBattleClientPlayerNotifySpectateEndHandler;

void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request);
void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetPlayerBattleHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetPlayerBattleIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandlePlayerBattleCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitPlayerBattleGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
