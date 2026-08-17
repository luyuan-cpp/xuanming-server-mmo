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
