#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/guild_service_metadata.h"

namespace guildpb {
#pragma region GuildServiceCreateGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceCreateGuildGrpcPath = "/guildpb.GuildService/CreateGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request, GrpcDone<::guildpb::CreateGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceCreateGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceGetGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceGetGuildGrpcPath = "/guildpb.GuildService/GetGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request, GrpcDone<::guildpb::GetGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceGetGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceGetPlayerGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceGetPlayerGuildGrpcPath = "/guildpb.GuildService/GetPlayerGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request, GrpcDone<::guildpb::GetPlayerGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceGetPlayerGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceLeaveGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceLeaveGuildGrpcPath = "/guildpb.GuildService/LeaveGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request, GrpcDone<::guildpb::LeaveGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceLeaveGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceDisbandGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceDisbandGuildGrpcPath = "/guildpb.GuildService/DisbandGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request, GrpcDone<::guildpb::DisbandGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceDisbandGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceSetAnnouncement
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceSetAnnouncementGrpcPath = "/guildpb.GuildService/SetAnnouncement";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request, GrpcDone<::guildpb::SetAnnouncementResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceSetAnnouncementErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceRenameGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceRenameGuildGrpcPath = "/guildpb.GuildService/RenameGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceRenameGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::RenameGuildRequest& request, GrpcDone<::guildpb::RenameGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceRenameGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceSetGuildMemberRole
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceSetGuildMemberRoleGrpcPath = "/guildpb.GuildService/SetGuildMemberRole";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request, GrpcDone<::guildpb::SetGuildMemberRoleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceSetGuildMemberRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceKickGuildMember
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceKickGuildMemberGrpcPath = "/guildpb.GuildService/KickGuildMember";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request, GrpcDone<::guildpb::KickGuildMemberResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceKickGuildMemberErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceTransferGuildLeader
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceTransferGuildLeaderGrpcPath = "/guildpb.GuildService/TransferGuildLeader";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request, GrpcDone<::guildpb::TransferGuildLeaderResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceTransferGuildLeaderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceApplyJoinGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceApplyJoinGuildGrpcPath = "/guildpb.GuildService/ApplyJoinGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request, GrpcDone<::guildpb::ApplyJoinGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceApplyJoinGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceCancelGuildApplication
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceCancelGuildApplicationGrpcPath = "/guildpb.GuildService/CancelGuildApplication";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request, GrpcDone<::guildpb::CancelGuildApplicationResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceCancelGuildApplicationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceListMyGuildApplications
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceListMyGuildApplicationsGrpcPath = "/guildpb.GuildService/ListMyGuildApplications";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request, GrpcDone<::guildpb::ListMyGuildApplicationsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceListMyGuildApplicationsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceListGuildApplications
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceListGuildApplicationsGrpcPath = "/guildpb.GuildService/ListGuildApplications";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request, GrpcDone<::guildpb::ListGuildApplicationsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceListGuildApplicationsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceReviewGuildApplication
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceReviewGuildApplicationGrpcPath = "/guildpb.GuildService/ReviewGuildApplication";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request, GrpcDone<::guildpb::ReviewGuildApplicationResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceReviewGuildApplicationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceNotifyGuildChanged
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceNotifyGuildChangedGrpcPath = "/guildpb.GuildService/NotifyGuildChanged";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceNotifyGuildChangedErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceUpdateGuildScore
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceUpdateGuildScoreGrpcPath = "/guildpb.GuildService/UpdateGuildScore";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request, GrpcDone<::guildpb::UpdateGuildScoreResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceUpdateGuildScoreErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceGetGuildRank
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceGetGuildRankGrpcPath = "/guildpb.GuildService/GetGuildRank";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request, GrpcDone<::guildpb::GetGuildRankResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceGetGuildRankErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceGetGuildRankByGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceGetGuildRankByGuildGrpcPath = "/guildpb.GuildService/GetGuildRankByGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request, GrpcDone<::guildpb::GetGuildRankByGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceGetGuildRankByGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceGetGuildDonateOptions
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceGetGuildDonateOptionsGrpcPath = "/guildpb.GuildService/GetGuildDonateOptions";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceGetGuildDonateOptions(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildDonateOptionsRequest& request, GrpcDone<::guildpb::GetGuildDonateOptionsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceGetGuildDonateOptionsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceDonateToGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceDonateToGuildGrpcPath = "/guildpb.GuildService/DonateToGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceDonateToGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DonateToGuildRequest& request, GrpcDone<::guildpb::DonateToGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceDonateToGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceUpgradeGuild
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceUpgradeGuildGrpcPath = "/guildpb.GuildService/UpgradeGuild";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceUpgradeGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpgradeGuildRequest& request, GrpcDone<::guildpb::UpgradeGuildResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceUpgradeGuildErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceGetGuildShop
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceGetGuildShopGrpcPath = "/guildpb.GuildService/GetGuildShop";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceGetGuildShop(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildShopRequest& request, GrpcDone<::guildpb::GetGuildShopResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceGetGuildShopErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceBuyGuildShopGoods
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceBuyGuildShopGoodsGrpcPath = "/guildpb.GuildService/BuyGuildShopGoods";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceBuyGuildShopGoods(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::BuyGuildShopGoodsRequest& request, GrpcDone<::guildpb::BuyGuildShopGoodsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceBuyGuildShopGoodsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceGetGuildActivities
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceGetGuildActivitiesGrpcPath = "/guildpb.GuildService/GetGuildActivities";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceGetGuildActivities(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildActivitiesRequest& request, GrpcDone<::guildpb::GetGuildActivitiesResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceGetGuildActivitiesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceLightGuildLantern
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceLightGuildLanternGrpcPath = "/guildpb.GuildService/LightGuildLantern";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceLightGuildLantern(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LightGuildLanternRequest& request, GrpcDone<::guildpb::LightGuildLanternResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceLightGuildLanternErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceClaimGuildReunion
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceClaimGuildReunionGrpcPath = "/guildpb.GuildService/ClaimGuildReunion";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceClaimGuildReunion(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ClaimGuildReunionRequest& request, GrpcDone<::guildpb::ClaimGuildReunionResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceClaimGuildReunionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceStartGuildTrial
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceStartGuildTrialGrpcPath = "/guildpb.GuildService/StartGuildTrial";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceStartGuildTrial(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::StartGuildTrialRequest& request, GrpcDone<::guildpb::StartGuildTrialResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceStartGuildTrialErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region GuildServiceRespondGuildTrialInvite
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kGuildServiceRespondGuildTrialInviteGrpcPath = "/guildpb.GuildService/RespondGuildTrialInvite";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendGuildServiceRespondGuildTrialInvite(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::RespondGuildTrialInviteRequest& request, GrpcDone<::guildpb::RespondGuildTrialInviteResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendGuildServiceRespondGuildTrialInviteErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetGuildCallDeadline(std::chrono::milliseconds deadline);

}// namespace guildpb
