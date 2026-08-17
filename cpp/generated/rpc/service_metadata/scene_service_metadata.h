#pragma once
#include <cstdint>

#include "proto/scene/scene.pb.h"

constexpr uint32_t ScenePlayerEnterGameNodeMessageId = 83;
constexpr uint32_t ScenePlayerEnterGameNodeIndex = 0;
#define ScenePlayerEnterGameNodeMethod  ::Scene_Stub::descriptor()->method(0)

constexpr uint32_t SceneSendMessageToPlayerMessageId = 1;
constexpr uint32_t SceneSendMessageToPlayerIndex = 1;
#define SceneSendMessageToPlayerMethod  ::Scene_Stub::descriptor()->method(1)

constexpr uint32_t SceneProcessClientPlayerMessageMessageId = 10;
constexpr uint32_t SceneProcessClientPlayerMessageIndex = 2;
#define SceneProcessClientPlayerMessageMethod  ::Scene_Stub::descriptor()->method(2)

constexpr uint32_t SceneInvokePlayerServiceMessageId = 40;
constexpr uint32_t SceneInvokePlayerServiceIndex = 3;
#define SceneInvokePlayerServiceMethod  ::Scene_Stub::descriptor()->method(3)

constexpr uint32_t SceneRouteNodeStringMsgMessageId = 78;
constexpr uint32_t SceneRouteNodeStringMsgIndex = 4;
#define SceneRouteNodeStringMsgMethod  ::Scene_Stub::descriptor()->method(4)

constexpr uint32_t SceneRoutePlayerStringMsgMessageId = 81;
constexpr uint32_t SceneRoutePlayerStringMsgIndex = 5;
#define SceneRoutePlayerStringMsgMethod  ::Scene_Stub::descriptor()->method(5)

constexpr uint32_t SceneUpdateSessionDetailMessageId = 24;
constexpr uint32_t SceneUpdateSessionDetailIndex = 6;
#define SceneUpdateSessionDetailMethod  ::Scene_Stub::descriptor()->method(6)

constexpr uint32_t SceneCreateSceneMessageId = 36;
constexpr uint32_t SceneCreateSceneIndex = 7;
#define SceneCreateSceneMethod  ::Scene_Stub::descriptor()->method(7)

constexpr uint32_t SceneDestroySceneMessageId = 121;
constexpr uint32_t SceneDestroySceneIndex = 8;
#define SceneDestroySceneMethod  ::Scene_Stub::descriptor()->method(8)

constexpr uint32_t ScenePrepareBattleMessageId = 141;
constexpr uint32_t ScenePrepareBattleIndex = 9;
#define ScenePrepareBattleMethod  ::Scene_Stub::descriptor()->method(9)

constexpr uint32_t SceneCancelBattlePrepareMessageId = 155;
constexpr uint32_t SceneCancelBattlePrepareIndex = 10;
#define SceneCancelBattlePrepareMethod  ::Scene_Stub::descriptor()->method(10)

constexpr uint32_t SceneNodeHandshakeMessageId = 62;
constexpr uint32_t SceneNodeHandshakeIndex = 11;
#define SceneNodeHandshakeMethod  ::Scene_Stub::descriptor()->method(11)
