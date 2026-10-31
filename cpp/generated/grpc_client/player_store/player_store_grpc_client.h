#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/player_store_service_metadata.h"

namespace playerstore {
#pragma region PlayerStoreLoadAndClaim
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerStoreLoadAndClaimGrpcPath = "/playerstore.PlayerStore/LoadAndClaim";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerStoreLoadAndClaim(entt::registry& registry, entt::entity nodeEntity, const ::playerstore::LoadAndClaimRequest& request, GrpcDone<::playerstore::LoadAndClaimResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerStoreLoadAndClaimErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region PlayerStoreSavePlayer
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kPlayerStoreSavePlayerGrpcPath = "/playerstore.PlayerStore/SavePlayer";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendPlayerStoreSavePlayer(entt::registry& registry, entt::entity nodeEntity, const ::playerstore::SavePlayerRequest& request, GrpcDone<::playerstore::SavePlayerResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendPlayerStoreSavePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetPlayerStoreCallDeadline(std::chrono::milliseconds deadline);

}// namespace playerstore
