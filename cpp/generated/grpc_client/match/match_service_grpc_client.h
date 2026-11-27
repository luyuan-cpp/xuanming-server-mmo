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
#pragma region MatchServiceListBattleScripts
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceListBattleScriptsGrpcPath = "/match.MatchService/ListBattleScripts";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceListBattleScripts(entt::registry& registry, entt::entity nodeEntity, const ::match::ListBattleScriptsRequest& request, GrpcDone<::match::ListBattleScriptsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceListBattleScriptsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceSubmitBattleScript
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceSubmitBattleScriptGrpcPath = "/match.MatchService/SubmitBattleScript";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceSubmitBattleScript(entt::registry& registry, entt::entity nodeEntity, const ::match::SubmitBattleScriptRequest& request, GrpcDone<::match::SubmitBattleScriptResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceSubmitBattleScriptErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceDeleteBattleScript
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceDeleteBattleScriptGrpcPath = "/match.MatchService/DeleteBattleScript";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceDeleteBattleScript(entt::registry& registry, entt::entity nodeEntity, const ::match::DeleteBattleScriptRequest& request, GrpcDone<::match::DeleteBattleScriptResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceDeleteBattleScriptErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceSetBattleScriptLoadout
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceSetBattleScriptLoadoutGrpcPath = "/match.MatchService/SetBattleScriptLoadout";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceSetBattleScriptLoadout(entt::registry& registry, entt::entity nodeEntity, const ::match::SetBattleScriptLoadoutRequest& request, GrpcDone<::match::SetBattleScriptLoadoutResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceSetBattleScriptLoadoutErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetRankedProfile
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetRankedProfileGrpcPath = "/match.MatchService/GetRankedProfile";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetRankedProfile(entt::registry& registry, entt::entity nodeEntity, const ::match::GetRankedProfileRequest& request, GrpcDone<::match::GetRankedProfileResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetRankedProfileErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceSaveRankedPreset
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceSaveRankedPresetGrpcPath = "/match.MatchService/SaveRankedPreset";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceSaveRankedPreset(entt::registry& registry, entt::entity nodeEntity, const ::match::SaveRankedPresetRequest& request, GrpcDone<::match::SaveRankedPresetResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceSaveRankedPresetErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceUpdateRankedCosmetic
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceUpdateRankedCosmeticGrpcPath = "/match.MatchService/UpdateRankedCosmetic";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceUpdateRankedCosmetic(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateRankedCosmeticRequest& request, GrpcDone<::match::UpdateRankedCosmeticResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceUpdateRankedCosmeticErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetRankedBattleResult
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetRankedBattleResultGrpcPath = "/match.MatchService/GetRankedBattleResult";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetRankedBattleResult(entt::registry& registry, entt::entity nodeEntity, const ::match::GetRankedBattleResultRequest& request, GrpcDone<::match::GetRankedBattleResultResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetRankedBattleResultErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetTournamentOverview
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetTournamentOverviewGrpcPath = "/match.MatchService/GetTournamentOverview";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetTournamentOverview(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentOverviewRequest& request, GrpcDone<::match::GetTournamentOverviewResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetTournamentOverviewErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceCreateTournamentSquad
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceCreateTournamentSquadGrpcPath = "/match.MatchService/CreateTournamentSquad";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceCreateTournamentSquad(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateTournamentSquadRequest& request, GrpcDone<::match::CreateTournamentSquadResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceCreateTournamentSquadErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceInviteTournamentMember
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceInviteTournamentMemberGrpcPath = "/match.MatchService/InviteTournamentMember";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceInviteTournamentMember(entt::registry& registry, entt::entity nodeEntity, const ::match::InviteTournamentMemberRequest& request, GrpcDone<::match::InviteTournamentMemberResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceInviteTournamentMemberErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceRespondTournamentInvite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceRespondTournamentInviteGrpcPath = "/match.MatchService/RespondTournamentInvite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceRespondTournamentInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondTournamentInviteRequest& request, GrpcDone<::match::RespondTournamentInviteResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceRespondTournamentInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceLeaveTournamentSquad
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceLeaveTournamentSquadGrpcPath = "/match.MatchService/LeaveTournamentSquad";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceLeaveTournamentSquad(entt::registry& registry, entt::entity nodeEntity, const ::match::LeaveTournamentSquadRequest& request, GrpcDone<::match::LeaveTournamentSquadResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceLeaveTournamentSquadErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceUpdateTournamentLineup
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceUpdateTournamentLineupGrpcPath = "/match.MatchService/UpdateTournamentLineup";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceUpdateTournamentLineup(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateTournamentLineupRequest& request, GrpcDone<::match::UpdateTournamentLineupResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceUpdateTournamentLineupErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceRegisterTournamentSquad
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceRegisterTournamentSquadGrpcPath = "/match.MatchService/RegisterTournamentSquad";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceRegisterTournamentSquad(entt::registry& registry, entt::entity nodeEntity, const ::match::RegisterTournamentSquadRequest& request, GrpcDone<::match::RegisterTournamentSquadResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceRegisterTournamentSquadErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceListTournamentStandings
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceListTournamentStandingsGrpcPath = "/match.MatchService/ListTournamentStandings";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceListTournamentStandings(entt::registry& registry, entt::entity nodeEntity, const ::match::ListTournamentStandingsRequest& request, GrpcDone<::match::ListTournamentStandingsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceListTournamentStandingsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceStartTournamentMatch
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceStartTournamentMatchGrpcPath = "/match.MatchService/StartTournamentMatch";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceStartTournamentMatch(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTournamentMatchRequest& request, GrpcDone<::match::StartTournamentMatchResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceStartTournamentMatchErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceCheerTournamentSquad
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceCheerTournamentSquadGrpcPath = "/match.MatchService/CheerTournamentSquad";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceCheerTournamentSquad(entt::registry& registry, entt::entity nodeEntity, const ::match::CheerTournamentSquadRequest& request, GrpcDone<::match::CheerTournamentSquadResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceCheerTournamentSquadErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceClaimTournamentReward
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceClaimTournamentRewardGrpcPath = "/match.MatchService/ClaimTournamentReward";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceClaimTournamentReward(entt::registry& registry, entt::entity nodeEntity, const ::match::ClaimTournamentRewardRequest& request, GrpcDone<::match::ClaimTournamentRewardResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceClaimTournamentRewardErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceSendTournamentSpectateCheer
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceSendTournamentSpectateCheerGrpcPath = "/match.MatchService/SendTournamentSpectateCheer";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceSendTournamentSpectateCheer(entt::registry& registry, entt::entity nodeEntity, const ::match::SendTournamentSpectateCheerRequest& request, GrpcDone<::match::SendTournamentSpectateCheerResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceSendTournamentSpectateCheerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetTournamentSpectateCheer
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetTournamentSpectateCheerGrpcPath = "/match.MatchService/GetTournamentSpectateCheer";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetTournamentSpectateCheer(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentSpectateCheerRequest& request, GrpcDone<::match::GetTournamentSpectateCheerResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetTournamentSpectateCheerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceUpdateTournamentSquadEmblem
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceUpdateTournamentSquadEmblemGrpcPath = "/match.MatchService/UpdateTournamentSquadEmblem";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceUpdateTournamentSquadEmblem(entt::registry& registry, entt::entity nodeEntity, const ::match::UpdateTournamentSquadEmblemRequest& request, GrpcDone<::match::UpdateTournamentSquadEmblemResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceUpdateTournamentSquadEmblemErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceStartTournamentWarmup
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceStartTournamentWarmupGrpcPath = "/match.MatchService/StartTournamentWarmup";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceStartTournamentWarmup(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTournamentWarmupRequest& request, GrpcDone<::match::StartTournamentWarmupResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceStartTournamentWarmupErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServicePredictTournamentMatch
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServicePredictTournamentMatchGrpcPath = "/match.MatchService/PredictTournamentMatch";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServicePredictTournamentMatch(entt::registry& registry, entt::entity nodeEntity, const ::match::PredictTournamentMatchRequest& request, GrpcDone<::match::PredictTournamentMatchResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServicePredictTournamentMatchErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetTournamentPredictionBoard
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetTournamentPredictionBoardGrpcPath = "/match.MatchService/GetTournamentPredictionBoard";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetTournamentPredictionBoard(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentPredictionBoardRequest& request, GrpcDone<::match::GetTournamentPredictionBoardResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetTournamentPredictionBoardErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceBanTournamentLeyline
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceBanTournamentLeylineGrpcPath = "/match.MatchService/BanTournamentLeyline";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceBanTournamentLeyline(entt::registry& registry, entt::entity nodeEntity, const ::match::BanTournamentLeylineRequest& request, GrpcDone<::match::BanTournamentLeylineResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceBanTournamentLeylineErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetTournamentHighlights
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetTournamentHighlightsGrpcPath = "/match.MatchService/GetTournamentHighlights";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetTournamentHighlights(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTournamentHighlightsRequest& request, GrpcDone<::match::GetTournamentHighlightsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetTournamentHighlightsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceCreateJianghuDuelInvite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceCreateJianghuDuelInviteGrpcPath = "/match.MatchService/CreateJianghuDuelInvite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceCreateJianghuDuelInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateJianghuDuelInviteRequest& request, GrpcDone<::match::CreateJianghuDuelInviteResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceCreateJianghuDuelInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceConfirmJianghuDuelMember
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceConfirmJianghuDuelMemberGrpcPath = "/match.MatchService/ConfirmJianghuDuelMember";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceConfirmJianghuDuelMember(entt::registry& registry, entt::entity nodeEntity, const ::match::ConfirmJianghuDuelMemberRequest& request, GrpcDone<::match::ConfirmJianghuDuelMemberResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceConfirmJianghuDuelMemberErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceRespondJianghuDuelInvite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceRespondJianghuDuelInviteGrpcPath = "/match.MatchService/RespondJianghuDuelInvite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceRespondJianghuDuelInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondJianghuDuelInviteRequest& request, GrpcDone<::match::RespondJianghuDuelInviteResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceRespondJianghuDuelInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceListJianghuGrudges
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceListJianghuGrudgesGrpcPath = "/match.MatchService/ListJianghuGrudges";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceListJianghuGrudges(entt::registry& registry, entt::entity nodeEntity, const ::match::ListJianghuGrudgesRequest& request, GrpcDone<::match::ListJianghuGrudgesResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceListJianghuGrudgesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceDismissJianghuGrudge
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceDismissJianghuGrudgeGrpcPath = "/match.MatchService/DismissJianghuGrudge";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceDismissJianghuGrudge(entt::registry& registry, entt::entity nodeEntity, const ::match::DismissJianghuGrudgeRequest& request, GrpcDone<::match::DismissJianghuGrudgeResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceDismissJianghuGrudgeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceNotifyJianghuDuelInvite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceNotifyJianghuDuelInviteGrpcPath = "/match.MatchService/NotifyJianghuDuelInvite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceNotifyJianghuDuelInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::JianghuDuelInviteS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceNotifyJianghuDuelInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceNotifyJianghuDuelStatus
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceNotifyJianghuDuelStatusGrpcPath = "/match.MatchService/NotifyJianghuDuelStatus";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceNotifyJianghuDuelStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::JianghuDuelStatusS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceNotifyJianghuDuelStatusErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceCreateTrialChallengeCard
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceCreateTrialChallengeCardGrpcPath = "/match.MatchService/CreateTrialChallengeCard";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceCreateTrialChallengeCard(entt::registry& registry, entt::entity nodeEntity, const ::match::CreateTrialChallengeCardRequest& request, GrpcDone<::match::CreateTrialChallengeCardResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceCreateTrialChallengeCardErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetTrialChallengeCard
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetTrialChallengeCardGrpcPath = "/match.MatchService/GetTrialChallengeCard";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetTrialChallengeCard(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTrialChallengeCardRequest& request, GrpcDone<::match::GetTrialChallengeCardResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetTrialChallengeCardErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetSharedTrialReplay
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetSharedTrialReplayGrpcPath = "/match.MatchService/GetSharedTrialReplay";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetSharedTrialReplay(entt::registry& registry, entt::entity nodeEntity, const ::match::GetSharedTrialReplayRequest& request, GrpcDone<::match::GetSharedTrialReplayResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetSharedTrialReplayErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceListTacticalPuzzles
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceListTacticalPuzzlesGrpcPath = "/match.MatchService/ListTacticalPuzzles";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceListTacticalPuzzles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListTacticalPuzzlesRequest& request, GrpcDone<::match::ListTacticalPuzzlesResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceListTacticalPuzzlesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetTacticalPuzzle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetTacticalPuzzleGrpcPath = "/match.MatchService/GetTacticalPuzzle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetTacticalPuzzle(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTacticalPuzzleRequest& request, GrpcDone<::match::GetTacticalPuzzleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetTacticalPuzzleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceSaveTacticalPuzzleDraft
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceSaveTacticalPuzzleDraftGrpcPath = "/match.MatchService/SaveTacticalPuzzleDraft";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceSaveTacticalPuzzleDraft(entt::registry& registry, entt::entity nodeEntity, const ::match::SaveTacticalPuzzleDraftRequest& request, GrpcDone<::match::SaveTacticalPuzzleDraftResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceSaveTacticalPuzzleDraftErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceDeleteTacticalPuzzle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceDeleteTacticalPuzzleGrpcPath = "/match.MatchService/DeleteTacticalPuzzle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceDeleteTacticalPuzzle(entt::registry& registry, entt::entity nodeEntity, const ::match::DeleteTacticalPuzzleRequest& request, GrpcDone<::match::DeleteTacticalPuzzleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceDeleteTacticalPuzzleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceStartTacticalPuzzleBattle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceStartTacticalPuzzleBattleGrpcPath = "/match.MatchService/StartTacticalPuzzleBattle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceStartTacticalPuzzleBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::StartTacticalPuzzleBattleRequest& request, GrpcDone<::match::StartTacticalPuzzleBattleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceStartTacticalPuzzleBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceLikeTacticalPuzzle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceLikeTacticalPuzzleGrpcPath = "/match.MatchService/LikeTacticalPuzzle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceLikeTacticalPuzzle(entt::registry& registry, entt::entity nodeEntity, const ::match::LikeTacticalPuzzleRequest& request, GrpcDone<::match::LikeTacticalPuzzleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceLikeTacticalPuzzleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceGetTacticalPuzzleSolutionReplay
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceGetTacticalPuzzleSolutionReplayGrpcPath = "/match.MatchService/GetTacticalPuzzleSolutionReplay";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceGetTacticalPuzzleSolutionReplay(entt::registry& registry, entt::entity nodeEntity, const ::match::GetTacticalPuzzleSolutionReplayRequest& request, GrpcDone<::match::GetTacticalPuzzleSolutionReplayResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceGetTacticalPuzzleSolutionReplayErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServiceSendBattleSpectateChat
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServiceSendBattleSpectateChatGrpcPath = "/match.MatchService/SendBattleSpectateChat";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServiceSendBattleSpectateChat(entt::registry& registry, entt::entity nodeEntity, const ::match::SendBattleSpectateChatRequest& request, GrpcDone<::match::SendBattleSpectateChatResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServiceSendBattleSpectateChatErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region MatchServicePullBattleSpectateChat
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kMatchServicePullBattleSpectateChatGrpcPath = "/match.MatchService/PullBattleSpectateChat";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendMatchServicePullBattleSpectateChat(entt::registry& registry, entt::entity nodeEntity, const ::match::PullBattleSpectateChatRequest& request, GrpcDone<::match::PullBattleSpectateChatResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendMatchServicePullBattleSpectateChatErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetMatchServiceCallDeadline(std::chrono::milliseconds deadline);

}// namespace match
