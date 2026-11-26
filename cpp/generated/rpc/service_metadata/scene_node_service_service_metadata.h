#pragma once
#include <cstdint>

#include "proto/scene_manager/scene_node_service.pb.h"

constexpr uint32_t SceneNodeGrpcCreateSceneMessageId = 122;
constexpr uint32_t SceneNodeGrpcCreateSceneIndex = 0;
#define SceneNodeGrpcCreateSceneMethod  ::SceneNodeGrpc_Stub::descriptor()->method(0)

constexpr uint32_t SceneNodeGrpcDestroySceneMessageId = 123;
constexpr uint32_t SceneNodeGrpcDestroySceneIndex = 1;
#define SceneNodeGrpcDestroySceneMethod  ::SceneNodeGrpc_Stub::descriptor()->method(1)

constexpr uint32_t SceneNodeGrpcReleasePlayerMessageId = 128;
constexpr uint32_t SceneNodeGrpcReleasePlayerIndex = 2;
#define SceneNodeGrpcReleasePlayerMethod  ::SceneNodeGrpc_Stub::descriptor()->method(2)

constexpr uint32_t SceneNodeGrpcPrepareBattleMessageId = 142;
constexpr uint32_t SceneNodeGrpcPrepareBattleIndex = 3;
#define SceneNodeGrpcPrepareBattleMethod  ::SceneNodeGrpc_Stub::descriptor()->method(3)

constexpr uint32_t SceneNodeGrpcCancelBattlePrepareMessageId = 145;
constexpr uint32_t SceneNodeGrpcCancelBattlePrepareIndex = 4;
#define SceneNodeGrpcCancelBattlePrepareMethod  ::SceneNodeGrpc_Stub::descriptor()->method(4)

constexpr uint32_t SceneNodeGrpcSealAccountRoleTradeMessageId = 331;
constexpr uint32_t SceneNodeGrpcSealAccountRoleTradeIndex = 5;
#define SceneNodeGrpcSealAccountRoleTradeMethod  ::SceneNodeGrpc_Stub::descriptor()->method(5)

constexpr uint32_t SceneNodeGrpcAssetDebitMessageId = 224;
constexpr uint32_t SceneNodeGrpcAssetDebitIndex = 6;
#define SceneNodeGrpcAssetDebitMethod  ::SceneNodeGrpc_Stub::descriptor()->method(6)

constexpr uint32_t SceneNodeGrpcAssetAbortDebitMessageId = 227;
constexpr uint32_t SceneNodeGrpcAssetAbortDebitIndex = 7;
#define SceneNodeGrpcAssetAbortDebitMethod  ::SceneNodeGrpc_Stub::descriptor()->method(7)

constexpr uint32_t SceneNodeGrpcAssetCreditMessageId = 225;
constexpr uint32_t SceneNodeGrpcAssetCreditIndex = 8;
#define SceneNodeGrpcAssetCreditMethod  ::SceneNodeGrpc_Stub::descriptor()->method(8)

constexpr uint32_t SceneNodeGrpcAssetAckReceiptMessageId = 270;
constexpr uint32_t SceneNodeGrpcAssetAckReceiptIndex = 9;
#define SceneNodeGrpcAssetAckReceiptMethod  ::SceneNodeGrpc_Stub::descriptor()->method(9)

constexpr uint32_t SceneNodeGrpcGmExecutePlayerCommandMessageId = 409;
constexpr uint32_t SceneNodeGrpcGmExecutePlayerCommandIndex = 10;
#define SceneNodeGrpcGmExecutePlayerCommandMethod  ::SceneNodeGrpc_Stub::descriptor()->method(10)

constexpr uint32_t SceneNodeGrpcCheckJianghuDuelEligibilityMessageId = 566;
constexpr uint32_t SceneNodeGrpcCheckJianghuDuelEligibilityIndex = 11;
#define SceneNodeGrpcCheckJianghuDuelEligibilityMethod  ::SceneNodeGrpc_Stub::descriptor()->method(11)
