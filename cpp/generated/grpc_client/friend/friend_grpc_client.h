#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/friend_service_metadata.h"

namespace friendpb {
#pragma region ClientPlayerFriendAddFriend
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendAddFriendGrpcPath = "/friendpb.ClientPlayerFriend/AddFriend";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request, GrpcDone<::friendpb::AddFriendResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendAddFriendErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendAcceptFriend
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendAcceptFriendGrpcPath = "/friendpb.ClientPlayerFriend/AcceptFriend";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request, GrpcDone<::friendpb::AcceptFriendResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendAcceptFriendErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendRejectFriend
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendRejectFriendGrpcPath = "/friendpb.ClientPlayerFriend/RejectFriend";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request, GrpcDone<::friendpb::RejectFriendResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendRejectFriendErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendRemoveFriend
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendRemoveFriendGrpcPath = "/friendpb.ClientPlayerFriend/RemoveFriend";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request, GrpcDone<::friendpb::RemoveFriendResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendRemoveFriendErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendGetFriendList
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendGetFriendListGrpcPath = "/friendpb.ClientPlayerFriend/GetFriendList";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request, GrpcDone<::friendpb::GetFriendListResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendGetFriendListErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendGetPendingRequests
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendGetPendingRequestsGrpcPath = "/friendpb.ClientPlayerFriend/GetPendingRequests";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request, GrpcDone<::friendpb::GetPendingRequestsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendGetPendingRequestsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendBlock
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendBlockGrpcPath = "/friendpb.ClientPlayerFriend/Block";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request, GrpcDone<::friendpb::BlockResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendBlockErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendUnblock
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendUnblockGrpcPath = "/friendpb.ClientPlayerFriend/Unblock";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request, GrpcDone<::friendpb::UnblockResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendUnblockErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendListBlocks
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendListBlocksGrpcPath = "/friendpb.ClientPlayerFriend/ListBlocks";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request, GrpcDone<::friendpb::ListBlocksResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendListBlocksErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendRecommendFriends
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendRecommendFriendsGrpcPath = "/friendpb.ClientPlayerFriend/RecommendFriends";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request, GrpcDone<::friendpb::RecommendFriendsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendRecommendFriendsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendNotifyFriendEvent
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendNotifyFriendEventGrpcPath = "/friendpb.ClientPlayerFriend/NotifyFriendEvent";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendNotifyFriendEventErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region ClientPlayerFriendBatchCheckSocialRelations
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kClientPlayerFriendBatchCheckSocialRelationsGrpcPath = "/friendpb.ClientPlayerFriend/BatchCheckSocialRelations";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendClientPlayerFriendBatchCheckSocialRelations(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BatchCheckSocialRelationsRequest& request, GrpcDone<::friendpb::BatchCheckSocialRelationsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendClientPlayerFriendBatchCheckSocialRelationsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetFriendCallDeadline(std::chrono::milliseconds deadline);

}// namespace friendpb
