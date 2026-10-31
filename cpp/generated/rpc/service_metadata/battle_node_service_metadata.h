#pragma once
#include <cstdint>

#include "proto/battle/battle_node.pb.h"

constexpr uint32_t BattleNodeQueryObserverMessageId = 344;
constexpr uint32_t BattleNodeQueryObserverIndex = 0;
#define BattleNodeQueryObserverMethod  ::BattleNode_Stub::descriptor()->method(0)

constexpr uint32_t BattleNodeQueryBattleCreationMessageId = 283;
constexpr uint32_t BattleNodeQueryBattleCreationIndex = 1;
#define BattleNodeQueryBattleCreationMethod  ::BattleNode_Stub::descriptor()->method(1)

constexpr uint32_t BattleNodeCreateBattleMessageId = 146;
constexpr uint32_t BattleNodeCreateBattleIndex = 2;
#define BattleNodeCreateBattleMethod  ::BattleNode_Stub::descriptor()->method(2)

constexpr uint32_t BattleNodeDestroyBattleMessageId = 147;
constexpr uint32_t BattleNodeDestroyBattleIndex = 3;
#define BattleNodeDestroyBattleMethod  ::BattleNode_Stub::descriptor()->method(3)

constexpr uint32_t BattleNodeAddObserverMessageId = 160;
constexpr uint32_t BattleNodeAddObserverIndex = 4;
#define BattleNodeAddObserverMethod  ::BattleNode_Stub::descriptor()->method(4)

constexpr uint32_t BattleNodeRemoveObserverMessageId = 159;
constexpr uint32_t BattleNodeRemoveObserverIndex = 5;
#define BattleNodeRemoveObserverMethod  ::BattleNode_Stub::descriptor()->method(5)

constexpr uint32_t BattleNodeIssueBattleTicketMessageId = 178;
constexpr uint32_t BattleNodeIssueBattleTicketIndex = 6;
#define BattleNodeIssueBattleTicketMethod  ::BattleNode_Stub::descriptor()->method(6)

constexpr uint32_t BattleNodeAttachRelayGateMessageId = 349;
constexpr uint32_t BattleNodeAttachRelayGateIndex = 7;
#define BattleNodeAttachRelayGateMethod  ::BattleNode_Stub::descriptor()->method(7)

constexpr uint32_t BattleNodePromoteSpectateBroadcastMessageId = 350;
constexpr uint32_t BattleNodePromoteSpectateBroadcastIndex = 8;
#define BattleNodePromoteSpectateBroadcastMethod  ::BattleNode_Stub::descriptor()->method(8)
