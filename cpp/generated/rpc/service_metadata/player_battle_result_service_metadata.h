#pragma once
#include <cstdint>

#include "proto/scene/player_battle_result.pb.h"

constexpr uint32_t SceneBattleResultClientPlayerGetRecentBattleResultsMessageId = 371;
constexpr uint32_t SceneBattleResultClientPlayerGetRecentBattleResultsIndex = 0;
#define SceneBattleResultClientPlayerGetRecentBattleResultsMethod  ::SceneBattleResultClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneBattleResultClientPlayerAckBattleResultsMessageId = 372;
constexpr uint32_t SceneBattleResultClientPlayerAckBattleResultsIndex = 1;
#define SceneBattleResultClientPlayerAckBattleResultsMethod  ::SceneBattleResultClientPlayer_Stub::descriptor()->method(1)
