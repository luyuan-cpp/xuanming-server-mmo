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

constexpr uint32_t MatchInternalSetSpectateFeaturedMessageId = 353;
constexpr uint32_t MatchInternalSetSpectateFeaturedIndex = 3;
#define MatchInternalSetSpectateFeaturedMethod  ::MatchInternal_Stub::descriptor()->method(3)

constexpr uint32_t MatchInternalAcquireTrialOccupancyMessageId = 368;
constexpr uint32_t MatchInternalAcquireTrialOccupancyIndex = 4;
#define MatchInternalAcquireTrialOccupancyMethod  ::MatchInternal_Stub::descriptor()->method(4)

constexpr uint32_t MatchInternalReleaseTrialOccupancyMessageId = 369;
constexpr uint32_t MatchInternalReleaseTrialOccupancyIndex = 5;
#define MatchInternalReleaseTrialOccupancyMethod  ::MatchInternal_Stub::descriptor()->method(5)

constexpr uint32_t MatchInternalGetBattleCompletionMessageId = 370;
constexpr uint32_t MatchInternalGetBattleCompletionIndex = 6;
#define MatchInternalGetBattleCompletionMethod  ::MatchInternal_Stub::descriptor()->method(6)

constexpr uint32_t MatchInternalGetBattleTerminalDecisionMessageId = 376;
constexpr uint32_t MatchInternalGetBattleTerminalDecisionIndex = 7;
#define MatchInternalGetBattleTerminalDecisionMethod  ::MatchInternal_Stub::descriptor()->method(7)

constexpr uint32_t MatchInternalListStrandedFormationsMessageId = 377;
constexpr uint32_t MatchInternalListStrandedFormationsIndex = 8;
#define MatchInternalListStrandedFormationsMethod  ::MatchInternal_Stub::descriptor()->method(8)

constexpr uint32_t MatchInternalResolveStrandedFormationMessageId = 378;
constexpr uint32_t MatchInternalResolveStrandedFormationIndex = 9;
#define MatchInternalResolveStrandedFormationMethod  ::MatchInternal_Stub::descriptor()->method(9)
