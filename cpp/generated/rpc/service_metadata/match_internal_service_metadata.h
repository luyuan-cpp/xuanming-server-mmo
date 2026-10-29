#pragma once
#include <cstdint>

#include "proto/match/match_internal.pb.h"

constexpr uint32_t MatchInternalStartActivityBattleMessageId = 245;
constexpr uint32_t MatchInternalStartActivityBattleIndex = 0;
#define MatchInternalStartActivityBattleMethod  ::MatchInternal_Stub::descriptor()->method(0)

constexpr uint32_t MatchInternalCancelActivityBattleMessageId = 330;
constexpr uint32_t MatchInternalCancelActivityBattleIndex = 1;
#define MatchInternalCancelActivityBattleMethod  ::MatchInternal_Stub::descriptor()->method(1)

constexpr uint32_t MatchInternalCommitBattleOutcomeMessageId = 345;
constexpr uint32_t MatchInternalCommitBattleOutcomeIndex = 2;
#define MatchInternalCommitBattleOutcomeMethod  ::MatchInternal_Stub::descriptor()->method(2)
