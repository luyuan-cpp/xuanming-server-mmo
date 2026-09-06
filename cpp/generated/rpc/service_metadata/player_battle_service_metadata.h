#pragma once
#include <cstdint>

#include "proto/battle/player_battle.pb.h"

constexpr uint32_t BattleClientPlayerSubmitBattleActionMessageId = 149;
constexpr uint32_t BattleClientPlayerSubmitBattleActionIndex = 0;
#define BattleClientPlayerSubmitBattleActionMethod  ::BattleClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t BattleClientPlayerGetBattleStateMessageId = 140;
constexpr uint32_t BattleClientPlayerGetBattleStateIndex = 1;
#define BattleClientPlayerGetBattleStateMethod  ::BattleClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t BattleClientPlayerNotifyBattleStartMessageId = 143;
constexpr uint32_t BattleClientPlayerNotifyBattleStartIndex = 2;
#define BattleClientPlayerNotifyBattleStartMethod  ::BattleClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t BattleClientPlayerNotifyTurnResultMessageId = 139;
constexpr uint32_t BattleClientPlayerNotifyTurnResultIndex = 3;
#define BattleClientPlayerNotifyTurnResultMethod  ::BattleClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t BattleClientPlayerNotifyBattleEndMessageId = 150;
constexpr uint32_t BattleClientPlayerNotifyBattleEndIndex = 4;
#define BattleClientPlayerNotifyBattleEndMethod  ::BattleClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t BattleClientPlayerNotifyBattleReconnectMessageId = 144;
constexpr uint32_t BattleClientPlayerNotifyBattleReconnectIndex = 5;
#define BattleClientPlayerNotifyBattleReconnectMethod  ::BattleClientPlayer_Stub::descriptor()->method(5)

constexpr uint32_t BattleClientPlayerStopWatchBattleMessageId = 165;
constexpr uint32_t BattleClientPlayerStopWatchBattleIndex = 6;
#define BattleClientPlayerStopWatchBattleMethod  ::BattleClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t BattleClientPlayerSetAutoBattleMessageId = 162;
constexpr uint32_t BattleClientPlayerSetAutoBattleIndex = 7;
#define BattleClientPlayerSetAutoBattleMethod  ::BattleClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t BattleClientPlayerNotifySpectateStateMessageId = 161;
constexpr uint32_t BattleClientPlayerNotifySpectateStateIndex = 8;
#define BattleClientPlayerNotifySpectateStateMethod  ::BattleClientPlayer_Stub::descriptor()->method(8)

constexpr uint32_t BattleClientPlayerNotifySpectateTurnResultMessageId = 158;
constexpr uint32_t BattleClientPlayerNotifySpectateTurnResultIndex = 9;
#define BattleClientPlayerNotifySpectateTurnResultMethod  ::BattleClientPlayer_Stub::descriptor()->method(9)

constexpr uint32_t BattleClientPlayerNotifySpectateEndMessageId = 166;
constexpr uint32_t BattleClientPlayerNotifySpectateEndIndex = 10;
#define BattleClientPlayerNotifySpectateEndMethod  ::BattleClientPlayer_Stub::descriptor()->method(10)

constexpr uint32_t BattleClientPlayerNotifyBattleAssignedMessageId = 177;
constexpr uint32_t BattleClientPlayerNotifyBattleAssignedIndex = 11;
#define BattleClientPlayerNotifyBattleAssignedMethod  ::BattleClientPlayer_Stub::descriptor()->method(11)
