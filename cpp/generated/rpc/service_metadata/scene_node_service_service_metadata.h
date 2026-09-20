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

constexpr uint32_t SceneNodeGrpcAssetDebitMessageId = 224;
constexpr uint32_t SceneNodeGrpcAssetDebitIndex = 5;
#define SceneNodeGrpcAssetDebitMethod  ::SceneNodeGrpc_Stub::descriptor()->method(5)

constexpr uint32_t SceneNodeGrpcAssetAbortDebitMessageId = 227;
constexpr uint32_t SceneNodeGrpcAssetAbortDebitIndex = 6;
#define SceneNodeGrpcAssetAbortDebitMethod  ::SceneNodeGrpc_Stub::descriptor()->method(6)

constexpr uint32_t SceneNodeGrpcAssetCreditMessageId = 225;
constexpr uint32_t SceneNodeGrpcAssetCreditIndex = 7;
#define SceneNodeGrpcAssetCreditMethod  ::SceneNodeGrpc_Stub::descriptor()->method(7)
