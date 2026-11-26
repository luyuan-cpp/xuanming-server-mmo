#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/team_service_metadata.h"

namespace teampb {
#pragma region ClientPlayerTeamCreateTeam
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamCreateTeamGrpcPath = "/teampb.ClientPlayerTeam/CreateTeam";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::CreateTeamRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamCreateTeamErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamGetMyTeam
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamGetMyTeamGrpcPath = "/teampb.ClientPlayerTeam/GetMyTeam";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::GetMyTeamRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamGetMyTeamErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamApplyJoinTeam
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamApplyJoinTeamGrpcPath = "/teampb.ClientPlayerTeam/ApplyJoinTeam";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ApplyJoinTeamRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamApplyJoinTeamErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamHandleApplication
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamHandleApplicationGrpcPath = "/teampb.ClientPlayerTeam/HandleApplication";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const ::teampb::HandleApplicationRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamHandleApplicationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamInviteToTeam
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamInviteToTeamGrpcPath = "/teampb.ClientPlayerTeam/InviteToTeam";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::InviteToTeamRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamInviteToTeamErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamRespondInvite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamRespondInviteGrpcPath = "/teampb.ClientPlayerTeam/RespondInvite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::RespondInviteRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamRespondInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamListMyInvites
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamListMyInvitesGrpcPath = "/teampb.ClientPlayerTeam/ListMyInvites";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ListMyInvitesRequest& request, GrpcDone<::teampb::ListMyInvitesResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamListMyInvitesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamLeaveTeam
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamLeaveTeamGrpcPath = "/teampb.ClientPlayerTeam/LeaveTeam";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::LeaveTeamRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamLeaveTeamErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamKickMember
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamKickMemberGrpcPath = "/teampb.ClientPlayerTeam/KickMember";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const ::teampb::KickMemberRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamKickMemberErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamTransferLeader
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamTransferLeaderGrpcPath = "/teampb.ClientPlayerTeam/TransferLeader";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TransferLeaderRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamTransferLeaderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamDisbandTeam
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamDisbandTeamGrpcPath = "/teampb.ClientPlayerTeam/DisbandTeam";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::DisbandTeamRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamDisbandTeamErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamStartTeamMatch
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamStartTeamMatchGrpcPath = "/teampb.ClientPlayerTeam/StartTeamMatch";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const ::teampb::StartTeamMatchRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamStartTeamMatchErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamSnapshot
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamNotifyTeamSnapshotGrpcPath = "/teampb.ClientPlayerTeam/NotifyTeamSnapshot";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamSnapshotS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamNotifyTeamSnapshotErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamInvite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamNotifyTeamInviteGrpcPath = "/teampb.ClientPlayerTeam/NotifyTeamInvite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamInviteS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamNotifyTeamInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamEvent
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamNotifyTeamEventGrpcPath = "/teampb.ClientPlayerTeam/NotifyTeamEvent";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamNotifyTeamEventErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamConfirmRankedPreset
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamConfirmRankedPresetGrpcPath = "/teampb.ClientPlayerTeam/ConfirmRankedPreset";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamConfirmRankedPreset(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ConfirmRankedPresetRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamConfirmRankedPresetErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamSetTeamFormation
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamSetTeamFormationGrpcPath = "/teampb.ClientPlayerTeam/SetTeamFormation";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamSetTeamFormation(entt::registry& registry, entt::entity nodeEntity, const ::teampb::SetTeamFormationRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamSetTeamFormationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerTeamRecallTeamMembers
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerTeamRecallTeamMembersGrpcPath = "/teampb.ClientPlayerTeam/RecallTeamMembers";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerTeamRecallTeamMembers(entt::registry& registry, entt::entity nodeEntity, const ::teampb::RecallTeamMembersRequest& request, GrpcDone<::teampb::TeamResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerTeamRecallTeamMembersErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetTeamCallDeadline(std::chrono::milliseconds deadline);

}// namespace teampb
