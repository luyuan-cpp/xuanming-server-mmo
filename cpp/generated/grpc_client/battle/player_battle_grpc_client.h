#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/player_battle_service_metadata.h"

#pragma region BattleClientPlayerSubmitBattleAction
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerSubmitBattleActionGrpcPath = "/BattleClientPlayer/SubmitBattleAction";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerSubmitBattleAction(entt::registry& registry, entt::entity nodeEntity, const ::SubmitBattleActionRequest& request, GrpcDone<::SubmitBattleActionResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerSubmitBattleActionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerGetBattleState
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerGetBattleStateGrpcPath = "/BattleClientPlayer/GetBattleState";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerGetBattleState(entt::registry& registry, entt::entity nodeEntity, const ::GetBattleStateRequest& request, GrpcDone<::BattleStateS2C> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerGetBattleStateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleStart
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifyBattleStartGrpcPath = "/BattleClientPlayer/NotifyBattleStart";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifyBattleStart(entt::registry& registry, entt::entity nodeEntity, const ::BattleStartS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifyBattleStartErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifyTurnResult
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifyTurnResultGrpcPath = "/BattleClientPlayer/NotifyTurnResult";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifyTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifyTurnResultErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleEnd
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifyBattleEndGrpcPath = "/BattleClientPlayer/NotifyBattleEnd";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifyBattleEnd(entt::registry& registry, entt::entity nodeEntity, const ::BattleEndS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifyBattleEndErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleReconnect
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifyBattleReconnectGrpcPath = "/BattleClientPlayer/NotifyBattleReconnect";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& registry, entt::entity nodeEntity, const ::BattleReconnectS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifyBattleReconnectErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerStopWatchBattle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerStopWatchBattleGrpcPath = "/BattleClientPlayer/StopWatchBattle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerStopWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::StopWatchBattleRequest& request, GrpcDone<::StopWatchBattleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerStopWatchBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerSetAutoBattle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerSetAutoBattleGrpcPath = "/BattleClientPlayer/SetAutoBattle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerSetAutoBattle(entt::registry& registry, entt::entity nodeEntity, const ::SetAutoBattleRequest& request, GrpcDone<::SetAutoBattleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerSetAutoBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateState
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifySpectateStateGrpcPath = "/BattleClientPlayer/NotifySpectateState";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifySpectateState(entt::registry& registry, entt::entity nodeEntity, const ::SpectateStateS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifySpectateStateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateTurnResult
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifySpectateTurnResultGrpcPath = "/BattleClientPlayer/NotifySpectateTurnResult";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& registry, entt::entity nodeEntity, const ::TurnResultS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifySpectateTurnResultErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifySpectateEnd
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifySpectateEndGrpcPath = "/BattleClientPlayer/NotifySpectateEnd";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifySpectateEnd(entt::registry& registry, entt::entity nodeEntity, const ::SpectateEndS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifySpectateEndErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattleAssigned
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifyBattleAssignedGrpcPath = "/BattleClientPlayer/NotifyBattleAssigned";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& registry, entt::entity nodeEntity, const ::BattleAssignedS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifyBattleAssignedErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerSurrenderBattle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerSurrenderBattleGrpcPath = "/BattleClientPlayer/SurrenderBattle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerSurrenderBattle(entt::registry& registry, entt::entity nodeEntity, const ::SurrenderBattleRequest& request, GrpcDone<::SurrenderBattleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerSurrenderBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifySurrenderVote
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifySurrenderVoteGrpcPath = "/BattleClientPlayer/NotifySurrenderVote";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifySurrenderVote(entt::registry& registry, entt::entity nodeEntity, const ::SurrenderVoteS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifySurrenderVoteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerUpdateBattlePrep
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerUpdateBattlePrepGrpcPath = "/BattleClientPlayer/UpdateBattlePrep";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerUpdateBattlePrep(entt::registry& registry, entt::entity nodeEntity, const ::UpdateBattlePrepRequest& request, GrpcDone<::UpdateBattlePrepResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerUpdateBattlePrepErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattlePrepUpdate
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifyBattlePrepUpdateGrpcPath = "/BattleClientPlayer/NotifyBattlePrepUpdate";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifyBattlePrepUpdate(entt::registry& registry, entt::entity nodeEntity, const ::BattlePrepUpdateS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifyBattlePrepUpdateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerSendBattlePing
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerSendBattlePingGrpcPath = "/BattleClientPlayer/SendBattlePing";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerSendBattlePing(entt::registry& registry, entt::entity nodeEntity, const ::SendBattlePingRequest& request, GrpcDone<::SendBattlePingResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerSendBattlePingErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleClientPlayerNotifyBattlePing
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleClientPlayerNotifyBattlePingGrpcPath = "/BattleClientPlayer/NotifyBattlePing";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleClientPlayerNotifyBattlePing(entt::registry& registry, entt::entity nodeEntity, const ::BattlePingS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleClientPlayerNotifyBattlePingErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetPlayerBattleCallDeadline(std::chrono::milliseconds deadline);
