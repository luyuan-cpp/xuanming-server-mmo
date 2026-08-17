#pragma once
#include <cstdint>

#include "proto/battle/battle_node.pb.h"

constexpr uint32_t BattleNodeCreateBattleMessageId = 146;
constexpr uint32_t BattleNodeCreateBattleIndex = 0;
#define BattleNodeCreateBattleMethod  ::BattleNode_Stub::descriptor()->method(0)

constexpr uint32_t BattleNodeDestroyBattleMessageId = 147;
constexpr uint32_t BattleNodeDestroyBattleIndex = 1;
#define BattleNodeDestroyBattleMethod  ::BattleNode_Stub::descriptor()->method(1)
