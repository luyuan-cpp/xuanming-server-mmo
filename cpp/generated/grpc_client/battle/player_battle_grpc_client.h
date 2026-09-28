#pragma once
#include <chrono>
#include <functional>
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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::SubmitBattleActionRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerSubmitBattleActionHandlerFunctionType =
    std::function<void(const ClientContext&, const ::SubmitBattleActionResponse&)>;
extern AsyncBattleClientPlayerSubmitBattleActionHandlerFunctionType AsyncBattleClientPlayerSubmitBattleActionHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerSubmitBattleActionFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::SubmitBattleActionRequest&)>;
extern AsyncBattleClientPlayerSubmitBattleActionFailedHandlerFunctionType AsyncBattleClientPlayerSubmitBattleActionFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::GetBattleStateRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerGetBattleStateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::BattleStateS2C&)>;
extern AsyncBattleClientPlayerGetBattleStateHandlerFunctionType AsyncBattleClientPlayerGetBattleStateHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerGetBattleStateFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::GetBattleStateRequest&)>;
extern AsyncBattleClientPlayerGetBattleStateFailedHandlerFunctionType AsyncBattleClientPlayerGetBattleStateFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::BattleStartS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerNotifyBattleStartHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyBattleStartHandlerFunctionType AsyncBattleClientPlayerNotifyBattleStartHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerNotifyBattleStartFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::BattleStartS2C&)>;
extern AsyncBattleClientPlayerNotifyBattleStartFailedHandlerFunctionType AsyncBattleClientPlayerNotifyBattleStartFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::TurnResultS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerNotifyTurnResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyTurnResultHandlerFunctionType AsyncBattleClientPlayerNotifyTurnResultHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerNotifyTurnResultFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::TurnResultS2C&)>;
extern AsyncBattleClientPlayerNotifyTurnResultFailedHandlerFunctionType AsyncBattleClientPlayerNotifyTurnResultFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::BattleEndS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerNotifyBattleEndHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyBattleEndHandlerFunctionType AsyncBattleClientPlayerNotifyBattleEndHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerNotifyBattleEndFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::BattleEndS2C&)>;
extern AsyncBattleClientPlayerNotifyBattleEndFailedHandlerFunctionType AsyncBattleClientPlayerNotifyBattleEndFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::BattleReconnectS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerNotifyBattleReconnectHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyBattleReconnectHandlerFunctionType AsyncBattleClientPlayerNotifyBattleReconnectHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerNotifyBattleReconnectFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::BattleReconnectS2C&)>;
extern AsyncBattleClientPlayerNotifyBattleReconnectFailedHandlerFunctionType AsyncBattleClientPlayerNotifyBattleReconnectFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::StopWatchBattleRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerStopWatchBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::StopWatchBattleResponse&)>;
extern AsyncBattleClientPlayerStopWatchBattleHandlerFunctionType AsyncBattleClientPlayerStopWatchBattleHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerStopWatchBattleFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::StopWatchBattleRequest&)>;
extern AsyncBattleClientPlayerStopWatchBattleFailedHandlerFunctionType AsyncBattleClientPlayerStopWatchBattleFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::SetAutoBattleRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerSetAutoBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::SetAutoBattleResponse&)>;
extern AsyncBattleClientPlayerSetAutoBattleHandlerFunctionType AsyncBattleClientPlayerSetAutoBattleHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerSetAutoBattleFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::SetAutoBattleRequest&)>;
extern AsyncBattleClientPlayerSetAutoBattleFailedHandlerFunctionType AsyncBattleClientPlayerSetAutoBattleFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::SpectateStateS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerNotifySpectateStateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifySpectateStateHandlerFunctionType AsyncBattleClientPlayerNotifySpectateStateHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerNotifySpectateStateFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::SpectateStateS2C&)>;
extern AsyncBattleClientPlayerNotifySpectateStateFailedHandlerFunctionType AsyncBattleClientPlayerNotifySpectateStateFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::TurnResultS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerNotifySpectateTurnResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifySpectateTurnResultHandlerFunctionType AsyncBattleClientPlayerNotifySpectateTurnResultHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerNotifySpectateTurnResultFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::TurnResultS2C&)>;
extern AsyncBattleClientPlayerNotifySpectateTurnResultFailedHandlerFunctionType AsyncBattleClientPlayerNotifySpectateTurnResultFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::SpectateEndS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerNotifySpectateEndHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifySpectateEndHandlerFunctionType AsyncBattleClientPlayerNotifySpectateEndHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerNotifySpectateEndFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::SpectateEndS2C&)>;
extern AsyncBattleClientPlayerNotifySpectateEndFailedHandlerFunctionType AsyncBattleClientPlayerNotifySpectateEndFailedHandler;

void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request);
void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleAssigned

struct AsyncBattleClientPlayerNotifyBattleAssignedGrpcClient {
    uint32_t messageId{ BattleClientPlayerNotifyBattleAssignedMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::BattleAssignedS2C request;
    GrpcSentMetadata sentMetadata;
};

using AsyncBattleClientPlayerNotifyBattleAssignedHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleClientPlayerNotifyBattleAssignedHandlerFunctionType AsyncBattleClientPlayerNotifyBattleAssignedHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncBattleClientPlayerNotifyBattleAssignedFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::BattleAssignedS2C&)>;
extern AsyncBattleClientPlayerNotifyBattleAssignedFailedHandlerFunctionType AsyncBattleClientPlayerNotifyBattleAssignedFailedHandler;

void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, const ::BattleAssignedS2C& request);
void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, const ::BattleAssignedS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetPlayerBattleHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetPlayerBattleIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetPlayerBattleFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetPlayerBattleIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetPlayerBattleCallDeadline(std::chrono::milliseconds deadline);
void HandlePlayerBattleCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitPlayerBattleGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
