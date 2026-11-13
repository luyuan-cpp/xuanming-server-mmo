#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/login_service_metadata.h"

namespace loginpb {
#pragma region ClientPlayerLoginLogin
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerLoginLoginGrpcPath = "/loginpb.ClientPlayerLogin/Login";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginRequest& request, GrpcDone<::loginpb::LoginResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerLoginLoginErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerLoginCreatePlayer
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerLoginCreatePlayerGrpcPath = "/loginpb.ClientPlayerLogin/CreatePlayer";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CreatePlayerRequest& request, GrpcDone<::loginpb::CreatePlayerResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerLoginCreatePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerLoginEnterGame
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerLoginEnterGameGrpcPath = "/loginpb.ClientPlayerLogin/EnterGame";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::EnterGameRequest& request, GrpcDone<::loginpb::EnterGameResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerLoginEnterGameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerLoginLeaveGame
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerLoginLeaveGameGrpcPath = "/loginpb.ClientPlayerLogin/LeaveGame";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LeaveGameRequest& request, GrpcDone<::loginpb::LoginEmptyResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerLoginLeaveGameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerLoginDisconnect
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerLoginDisconnectGrpcPath = "/loginpb.ClientPlayerLogin/Disconnect";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginNodeDisconnectRequest& request, GrpcDone<::loginpb::LoginEmptyResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerLoginDisconnectErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerLoginRefreshToken
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerLoginRefreshTokenGrpcPath = "/loginpb.ClientPlayerLogin/RefreshToken";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RefreshTokenRequest& request, GrpcDone<::loginpb::RefreshTokenResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerLoginRefreshTokenErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerLoginRequestGateMigration
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerLoginRequestGateMigrationGrpcPath = "/loginpb.ClientPlayerLogin/RequestGateMigration";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerLoginRequestGateMigration(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::GateMigrationRequest& request, GrpcDone<::loginpb::GateMigrationResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerLoginRequestGateMigrationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerLoginRenewSession
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerLoginRenewSessionGrpcPath = "/loginpb.ClientPlayerLogin/RenewSession";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerLoginRenewSession(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RenewSessionRequest& request, GrpcDone<::loginpb::RenewSessionResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerLoginRenewSessionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region LoginPreGateAssignGate
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kLoginPreGateAssignGateGrpcPath = "/loginpb.LoginPreGate/AssignGate";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AssignGateRequest& request, GrpcDone<::loginpb::AssignGateResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendLoginPreGateAssignGateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region LoginPreGateQueryQueueStatus
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kLoginPreGateQueryQueueStatusGrpcPath = "/loginpb.LoginPreGate/QueryQueueStatus";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::QueryQueueStatusRequest& request, GrpcDone<::loginpb::QueryQueueStatusResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendLoginPreGateQueryQueueStatusErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region LoginPreGateGetZoneRoleCount
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kLoginPreGateGetZoneRoleCountGrpcPath = "/loginpb.LoginPreGate/GetZoneRoleCount";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendLoginPreGateGetZoneRoleCount(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::ZoneRoleCountRequest& request, GrpcDone<::loginpb::ZoneRoleCountResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendLoginPreGateGetZoneRoleCountErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region LoginAdminRemovePlayersFromAccounts
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kLoginAdminRemovePlayersFromAccountsGrpcPath = "/loginpb.LoginAdmin/RemovePlayersFromAccounts";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request, GrpcDone<::loginpb::RemovePlayersFromAccountsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendLoginAdminRemovePlayersFromAccountsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetLoginCallDeadline(std::chrono::milliseconds deadline);

}// namespace loginpb
