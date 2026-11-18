#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/scene_node_service_service_metadata.h"

namespace scene_node {
#pragma region SceneNodeGrpcCreateScene
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcCreateSceneGrpcPath = "/scene_node.SceneNodeGrpc/CreateScene";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request, GrpcDone<::CreateSceneResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcCreateSceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcDestroyScene
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcDestroySceneGrpcPath = "/scene_node.SceneNodeGrpc/DestroyScene";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcDestroySceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcReleasePlayer
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcReleasePlayerGrpcPath = "/scene_node.SceneNodeGrpc/ReleasePlayer";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcReleasePlayerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcPrepareBattle
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcPrepareBattleGrpcPath = "/scene_node.SceneNodeGrpc/PrepareBattle";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request, GrpcDone<::PrepareBattleResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcPrepareBattleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcCancelBattlePrepare
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcCancelBattlePrepareGrpcPath = "/scene_node.SceneNodeGrpc/CancelBattlePrepare";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcCancelBattlePrepareErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcSealAccountRoleTrade
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcSealAccountRoleTradeGrpcPath = "/scene_node.SceneNodeGrpc/SealAccountRoleTrade";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcSealAccountRoleTrade(entt::registry& registry, entt::entity nodeEntity, const ::SealAccountRoleTradeRequest& request, GrpcDone<::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcSealAccountRoleTradeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcAssetDebit
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcAssetDebitGrpcPath = "/scene_node.SceneNodeGrpc/AssetDebit";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcAssetDebit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request, GrpcDone<::AssetOpResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcAssetDebitErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcAssetAbortDebit
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcAssetAbortDebitGrpcPath = "/scene_node.SceneNodeGrpc/AssetAbortDebit";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcAssetAbortDebit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request, GrpcDone<::AssetOpResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcAssetAbortDebitErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcAssetCredit
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcAssetCreditGrpcPath = "/scene_node.SceneNodeGrpc/AssetCredit";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcAssetCredit(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request, GrpcDone<::AssetOpResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcAssetCreditErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcAssetAckReceipt
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcAssetAckReceiptGrpcPath = "/scene_node.SceneNodeGrpc/AssetAckReceipt";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcAssetAckReceipt(entt::registry& registry, entt::entity nodeEntity, const ::AssetOpRequest& request, GrpcDone<::AssetOpResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcAssetAckReceiptErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region SceneNodeGrpcGmExecutePlayerCommand
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kSceneNodeGrpcGmExecutePlayerCommandGrpcPath = "/scene_node.SceneNodeGrpc/GmExecutePlayerCommand";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendSceneNodeGrpcGmExecutePlayerCommand(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::GmExecutePlayerCommandRequest& request, GrpcDone<::scene_node::GmExecutePlayerCommandResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendSceneNodeGrpcGmExecutePlayerCommandErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetSceneNodeServiceCallDeadline(std::chrono::milliseconds deadline);

}// namespace scene_node
