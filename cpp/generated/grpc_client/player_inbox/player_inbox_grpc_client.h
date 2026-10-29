#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/player_inbox_service_metadata.h"

namespace player_inbox {
#pragma region PlayerInboxFetchItems
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxFetchItemsGrpcPath = "/player_inbox.PlayerInbox/FetchItems";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxFetchItems(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::FetchPlayerInboxRequest& request, GrpcDone<::player_inbox::FetchPlayerInboxResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxFetchItemsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxAckItems
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxAckItemsGrpcPath = "/player_inbox.PlayerInbox/AckItems";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxAckItems(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::AckPlayerInboxRequest& request, GrpcDone<::player_inbox::AckPlayerInboxResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxAckItemsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxEnqueueBattleSettlement
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxEnqueueBattleSettlementGrpcPath = "/player_inbox.PlayerInbox/EnqueueBattleSettlement";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxEnqueueBattleSettlement(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::BattleSettlementInboxRequest& request, GrpcDone<::player_inbox::BattleSettlementInboxResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxEnqueueBattleSettlementErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerInboxLookupBattleSettlement
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerInboxLookupBattleSettlementGrpcPath = "/player_inbox.PlayerInbox/LookupBattleSettlement";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerInboxLookupBattleSettlement(entt::registry& registry, entt::entity nodeEntity, const ::player_inbox::BattleSettlementInboxRequest& request, GrpcDone<::player_inbox::BattleSettlementInboxResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerInboxLookupBattleSettlementErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetPlayerInboxCallDeadline(std::chrono::milliseconds deadline);

}// namespace player_inbox
