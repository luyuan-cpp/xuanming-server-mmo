#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/login_admin_service_metadata.h"

namespace loginpb {
#pragma region LoginAdminRemovePlayersFromAccounts
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kLoginAdminRemovePlayersFromAccountsGrpcPath = "/loginpb.LoginAdmin/RemovePlayersFromAccounts";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request, GrpcDone<::loginpb::RemovePlayersFromAccountsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendLoginAdminRemovePlayersFromAccountsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region LoginAdminScheduleAccountDeletion
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kLoginAdminScheduleAccountDeletionGrpcPath = "/loginpb.LoginAdmin/ScheduleAccountDeletion";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendLoginAdminScheduleAccountDeletion(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ScheduleAccountDeletionRequest& request, GrpcDone<::loginpb::AccountDeletionResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendLoginAdminScheduleAccountDeletionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region LoginAdminResetAccountRealName
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kLoginAdminResetAccountRealNameGrpcPath = "/loginpb.LoginAdmin/ResetAccountRealName";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendLoginAdminResetAccountRealName(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ResetAccountRealNameRequest& request, GrpcDone<::loginpb::ResetAccountRealNameResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendLoginAdminResetAccountRealNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetLoginAdminCallDeadline(std::chrono::milliseconds deadline);

}// namespace loginpb
