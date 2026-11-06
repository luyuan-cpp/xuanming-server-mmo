#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/match_service_service_metadata.h"

namespace match {
#pragma region MatchServiceGetNetworkProbePlan
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetNetworkProbePlanGrpcPath = "/match.MatchService/GetNetworkProbePlan";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetNetworkProbePlan(entt::registry& registry, entt::entity nodeEntity, const ::match::GetNetworkProbePlanRequest& request, GrpcDone<::match::GetNetworkProbePlanResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetNetworkProbePlanErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceCommitNetworkProbe
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceCommitNetworkProbeGrpcPath = "/match.MatchService/CommitNetworkProbe";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceCommitNetworkProbe(entt::registry& registry, entt::entity nodeEntity, const ::match::CommitNetworkProbeRequest& request, GrpcDone<::match::CommitNetworkProbeResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceCommitNetworkProbeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceJoinQueue
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceJoinQueueGrpcPath = "/match.MatchService/JoinQueue";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request, GrpcDone<::match::JoinQueueResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceJoinQueueErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceCancelQueue
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceCancelQueueGrpcPath = "/match.MatchService/CancelQueue";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceCancelQueueErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetQueueStatus
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetQueueStatusGrpcPath = "/match.MatchService/GetQueueStatus";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request, GrpcDone<::match::GetQueueStatusResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetQueueStatusErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceChallengePlayer
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceChallengePlayerGrpcPath = "/match.MatchService/ChallengePlayer";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request, GrpcDone<::match::ChallengePlayerResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceChallengePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceRespondChallenge
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceRespondChallengeGrpcPath = "/match.MatchService/RespondChallenge";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request, GrpcDone<::match::RespondChallengeResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceRespondChallengeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceNotifyChallengeInvite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceNotifyChallengeInviteGrpcPath = "/match.MatchService/NotifyChallengeInvite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceNotifyChallengeInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceNotifyChallengeResult
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceNotifyChallengeResultGrpcPath = "/match.MatchService/NotifyChallengeResult";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceNotifyChallengeResultErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceWatchBattle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceWatchBattleGrpcPath = "/match.MatchService/WatchBattle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request, GrpcDone<::match::WatchBattleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceWatchBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceRequestBattleTicket
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceRequestBattleTicketGrpcPath = "/match.MatchService/RequestBattleTicket";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::RequestBattleTicketRequest& request, GrpcDone<::RequestBattleTicketResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceRequestBattleTicketErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceListWatchableBattles
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceListWatchableBattlesGrpcPath = "/match.MatchService/ListWatchableBattles";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request, GrpcDone<::match::ListWatchableBattlesResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceListWatchableBattlesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceListBattleReports
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceListBattleReportsGrpcPath = "/match.MatchService/ListBattleReports";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceListBattleReports(entt::registry& registry, entt::entity nodeEntity, const ::match::ListBattleReportsRequest& request, GrpcDone<::match::ListBattleReportsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceListBattleReportsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetBattleReport
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetBattleReportGrpcPath = "/match.MatchService/GetBattleReport";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetBattleReport(entt::registry& registry, entt::entity nodeEntity, const ::match::GetBattleReportRequest& request, GrpcDone<::match::GetBattleReportResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetBattleReportErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetMatchServiceCallDeadline(std::chrono::milliseconds deadline);

}// namespace match
