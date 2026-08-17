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
