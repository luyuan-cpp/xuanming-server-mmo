#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/content_guard_service_metadata.h"

namespace content_guard {
#pragma region ContentGuardCheckText
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kContentGuardCheckTextGrpcPath = "/content_guard.ContentGuard/CheckText";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendContentGuardCheckText(entt::registry& registry, entt::entity nodeEntity, const ::content_guard::CheckTextRequest& request, GrpcDone<::content_guard::CheckTextResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendContentGuardCheckTextErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ContentGuardCheckPlatformText
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kContentGuardCheckPlatformTextGrpcPath = "/content_guard.ContentGuard/CheckPlatformText";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendContentGuardCheckPlatformText(entt::registry& registry, entt::entity nodeEntity, const ::content_guard::CheckTextRequest& request, GrpcDone<::content_guard::CheckTextResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendContentGuardCheckPlatformTextErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetContentGuardCallDeadline(std::chrono::milliseconds deadline);

}// namespace content_guard
