#pragma once
#include <cstdint>

#include "proto/scene/player_movement.pb.h"

constexpr uint32_t SceneMovementClientPlayerMoveStartMessageId = 134;
constexpr uint32_t SceneMovementClientPlayerMoveStartIndex = 0;
#define SceneMovementClientPlayerMoveStartMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneMovementClientPlayerMoveStopMessageId = 131;
constexpr uint32_t SceneMovementClientPlayerMoveStopIndex = 1;
#define SceneMovementClientPlayerMoveStopMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneMovementClientPlayerMoveSyncMessageId = 132;
constexpr uint32_t SceneMovementClientPlayerMoveSyncIndex = 2;
#define SceneMovementClientPlayerMoveSyncMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t SceneMovementClientPlayerTeleportRequestMessageId = 136;
constexpr uint32_t SceneMovementClientPlayerTeleportRequestIndex = 3;
#define SceneMovementClientPlayerTeleportRequestMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t SceneMovementClientPlayerNotifyMoveAckMessageId = 137;
constexpr uint32_t SceneMovementClientPlayerNotifyMoveAckIndex = 4;
#define SceneMovementClientPlayerNotifyMoveAckMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t SceneMovementClientPlayerNotifyActorMoveMessageId = 133;
constexpr uint32_t SceneMovementClientPlayerNotifyActorMoveIndex = 5;
#define SceneMovementClientPlayerNotifyActorMoveMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(5)

constexpr uint32_t SceneMovementClientPlayerNotifyActorMoveListMessageId = 135;
constexpr uint32_t SceneMovementClientPlayerNotifyActorMoveListIndex = 6;
#define SceneMovementClientPlayerNotifyActorMoveListMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t SceneMovementClientPlayerNotifyTeleportMessageId = 130;
constexpr uint32_t SceneMovementClientPlayerNotifyTeleportIndex = 7;
#define SceneMovementClientPlayerNotifyTeleportMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t SceneMovementClientPlayerTeamFollowMessageId = 381;
constexpr uint32_t SceneMovementClientPlayerTeamFollowIndex = 8;
#define SceneMovementClientPlayerTeamFollowMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(8)

constexpr uint32_t SceneMovementClientPlayerNotifyTeamFollowStateMessageId = 382;
constexpr uint32_t SceneMovementClientPlayerNotifyTeamFollowStateIndex = 9;
#define SceneMovementClientPlayerNotifyTeamFollowStateMethod  ::SceneMovementClientPlayer_Stub::descriptor()->method(9)
