#pragma once
#include <cstdint>

#include "proto/battle/battle_node.pb.h"

constexpr uint32_t BattleNodeCreateBattleMessageId = 146;
constexpr uint32_t BattleNodeCreateBattleIndex = 0;
#define BattleNodeCreateBattleMethod  ::BattleNode_Stub::descriptor()->method(0)

constexpr uint32_t BattleNodeDestroyBattleMessageId = 147;
constexpr uint32_t BattleNodeDestroyBattleIndex = 1;
#define BattleNodeDestroyBattleMethod  ::BattleNode_Stub::descriptor()->method(1)

constexpr uint32_t BattleNodeAddObserverMessageId = 160;
constexpr uint32_t BattleNodeAddObserverIndex = 2;
#define BattleNodeAddObserverMethod  ::BattleNode_Stub::descriptor()->method(2)

constexpr uint32_t BattleNodeRemoveObserverMessageId = 159;
constexpr uint32_t BattleNodeRemoveObserverIndex = 3;
#define BattleNodeRemoveObserverMethod  ::BattleNode_Stub::descriptor()->method(3)

constexpr uint32_t BattleNodeIssueBattleTicketMessageId = 178;
constexpr uint32_t BattleNodeIssueBattleTicketIndex = 4;
#define BattleNodeIssueBattleTicketMethod  ::BattleNode_Stub::descriptor()->method(4)
