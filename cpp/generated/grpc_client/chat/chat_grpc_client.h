#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/chat_service_metadata.h"

namespace chatpb {
#pragma region ClientPlayerChatSendChat
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerChatSendChatGrpcPath = "/chatpb.ClientPlayerChat/SendChat";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::SendChatRequest& request, GrpcDone<::chatpb::SendChatResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerChatSendChatErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerChatPullChatHistory
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerChatPullChatHistoryGrpcPath = "/chatpb.ClientPlayerChat/PullChatHistory";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::PullChatHistoryRequest& request, GrpcDone<::chatpb::PullChatHistoryResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerChatPullChatHistoryErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerChatNotifyWorldChat
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerChatNotifyWorldChatGrpcPath = "/chatpb.ClientPlayerChat/NotifyWorldChat";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerChatNotifyWorldChat(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::ChatMessage& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerChatNotifyWorldChatErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetChatCallDeadline(std::chrono::milliseconds deadline);

}// namespace chatpb
