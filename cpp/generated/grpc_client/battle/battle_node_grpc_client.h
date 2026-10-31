#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/battle_node_service_metadata.h"

#pragma region BattleNodeQueryObserver
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodeQueryObserverGrpcPath = "/BattleNode/QueryObserver";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodeQueryObserver(entt::registry& registry, entt::entity nodeEntity, const ::QueryObserverRequest& request, GrpcDone<::QueryObserverResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodeQueryObserverErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleNodeQueryBattleCreation
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodeQueryBattleCreationGrpcPath = "/BattleNode/QueryBattleCreation";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodeQueryBattleCreation(entt::registry& registry, entt::entity nodeEntity, const ::QueryBattleCreationRequest& request, GrpcDone<::QueryBattleCreationResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodeQueryBattleCreationErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleNodeCreateBattle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodeCreateBattleGrpcPath = "/BattleNode/CreateBattle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request, GrpcDone<::CreateBattleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodeCreateBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleNodeDestroyBattle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodeDestroyBattleGrpcPath = "/BattleNode/DestroyBattle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodeDestroyBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleNodeAddObserver
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodeAddObserverGrpcPath = "/BattleNode/AddObserver";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request, GrpcDone<::AddObserverResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodeAddObserverErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleNodeRemoveObserver
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodeRemoveObserverGrpcPath = "/BattleNode/RemoveObserver";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request, GrpcDone<::RemoveObserverResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodeRemoveObserverErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleNodeIssueBattleTicket
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodeIssueBattleTicketGrpcPath = "/BattleNode/IssueBattleTicket";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request, GrpcDone<::IssueBattleTicketResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodeIssueBattleTicketErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleNodeAttachRelayGate
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodeAttachRelayGateGrpcPath = "/BattleNode/AttachRelayGate";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodeAttachRelayGate(entt::registry& registry, entt::entity nodeEntity, const ::AttachRelayGateRequest& request, GrpcDone<::AttachRelayGateResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodeAttachRelayGateErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region BattleNodePromoteSpectateBroadcast
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kBattleNodePromoteSpectateBroadcastGrpcPath = "/BattleNode/PromoteSpectateBroadcast";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendBattleNodePromoteSpectateBroadcast(entt::registry& registry, entt::entity nodeEntity, const ::PromoteSpectateBroadcastRequest& request, GrpcDone<::PromoteSpectateBroadcastResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendBattleNodePromoteSpectateBroadcastErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetBattleNodeCallDeadline(std::chrono::milliseconds deadline);
