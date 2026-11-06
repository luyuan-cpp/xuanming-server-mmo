#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/player_inbox_admin_service_metadata.h"

namespace player_inbox {
#pragma region PlayerInboxAdminGmSanctionPlayer
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxAdminGmSanctionPlayerGrpcPath = "/player_inbox.PlayerInboxAdmin/GmSanctionPlayer";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxAdminGmSanctionPlayer(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GmSanctionPlayerRequest& request, GrpcDone<::player_inbox::GmSanctionPlayerResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxAdminGmSanctionPlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxAdminGetSummary
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxAdminGetSummaryGrpcPath = "/player_inbox.PlayerInboxAdmin/GetSummary";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxAdminGetSummary(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GetPlayerInboxSummaryRequest& request, GrpcDone<::player_inbox::GetPlayerInboxSummaryResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxAdminGetSummaryErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxAdminListItems
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxAdminListItemsGrpcPath = "/player_inbox.PlayerInboxAdmin/ListItems";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxAdminListItems(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::ListPlayerInboxItemsRequest& request, GrpcDone<::player_inbox::ListPlayerInboxItemsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxAdminListItemsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxAdminLookupByRequest
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxAdminLookupByRequestGrpcPath = "/player_inbox.PlayerInboxAdmin/LookupByRequest";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxAdminLookupByRequest(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::LookupPlayerInboxRequest& request, GrpcDone<::player_inbox::LookupPlayerInboxResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxAdminLookupByRequestErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxAdminVoidItem
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxAdminVoidItemGrpcPath = "/player_inbox.PlayerInboxAdmin/VoidItem";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxAdminVoidItem(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::VoidPlayerInboxItemRequest& request, GrpcDone<::player_inbox::VoidPlayerInboxItemResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxAdminVoidItemErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxAdminReconcilePlayer
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxAdminReconcilePlayerGrpcPath = "/player_inbox.PlayerInboxAdmin/ReconcilePlayer";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxAdminReconcilePlayer(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::ReconcilePlayerInboxRequest& request, GrpcDone<::player_inbox::ReconcilePlayerInboxResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxAdminReconcilePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxAdminGetItemGainBlocks
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxAdminGetItemGainBlocksGrpcPath = "/player_inbox.PlayerInboxAdmin/GetItemGainBlocks";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxAdminGetItemGainBlocks(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::GetPlayerItemGainBlocksRequest& request, GrpcDone<::player_inbox::GetPlayerItemGainBlocksResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxAdminGetItemGainBlocksErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetPlayerInboxAdminCallDeadline(std::chrono::milliseconds deadline);

}// namespace player_inbox
