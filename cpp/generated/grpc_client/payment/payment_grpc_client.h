#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/payment_service_metadata.h"

namespace paymentpb {
#pragma region ClientPlayerPaymentListPayChannels
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerPaymentListPayChannelsGrpcPath = "/paymentpb.ClientPlayerPayment/ListPayChannels";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerPaymentListPayChannels(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::ListPayChannelsRequest& request, GrpcDone<::paymentpb::ListPayChannelsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerPaymentListPayChannelsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerPaymentCreatePayIntent
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerPaymentCreatePayIntentGrpcPath = "/paymentpb.ClientPlayerPayment/CreatePayIntent";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerPaymentCreatePayIntent(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::CreatePayIntentRequest& request, GrpcDone<::paymentpb::CreatePayIntentResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerPaymentCreatePayIntentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerPaymentGetPayIntent
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerPaymentGetPayIntentGrpcPath = "/paymentpb.ClientPlayerPayment/GetPayIntent";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerPaymentGetPayIntent(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::GetPayIntentRequest& request, GrpcDone<::paymentpb::GetPayIntentResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerPaymentGetPayIntentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerPaymentSubmitPurchase
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerPaymentSubmitPurchaseGrpcPath = "/paymentpb.ClientPlayerPayment/SubmitPurchase";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerPaymentSubmitPurchase(entt::registry& registry, entt::entity nodeEntity, const ::paymentpb::SubmitPurchaseRequest& request, GrpcDone<::paymentpb::SubmitPurchaseResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerPaymentSubmitPurchaseErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetPaymentCallDeadline(std::chrono::milliseconds deadline);

}// namespace paymentpb
