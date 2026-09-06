#pragma once
#include <cstdint>

#include "proto/match/match_service.pb.h"

constexpr uint32_t MatchServiceJoinQueueMessageId = 157;
constexpr uint32_t MatchServiceJoinQueueIndex = 0;
#define MatchServiceJoinQueueMethod  ::MatchService_Stub::descriptor()->method(0)

constexpr uint32_t MatchServiceCancelQueueMessageId = 148;
constexpr uint32_t MatchServiceCancelQueueIndex = 1;
#define MatchServiceCancelQueueMethod  ::MatchService_Stub::descriptor()->method(1)

constexpr uint32_t MatchServiceGetQueueStatusMessageId = 153;
constexpr uint32_t MatchServiceGetQueueStatusIndex = 2;
#define MatchServiceGetQueueStatusMethod  ::MatchService_Stub::descriptor()->method(2)

constexpr uint32_t MatchServiceChallengePlayerMessageId = 152;
constexpr uint32_t MatchServiceChallengePlayerIndex = 3;
#define MatchServiceChallengePlayerMethod  ::MatchService_Stub::descriptor()->method(3)

constexpr uint32_t MatchServiceRespondChallengeMessageId = 151;
constexpr uint32_t MatchServiceRespondChallengeIndex = 4;
#define MatchServiceRespondChallengeMethod  ::MatchService_Stub::descriptor()->method(4)

constexpr uint32_t MatchServiceNotifyChallengeInviteMessageId = 156;
constexpr uint32_t MatchServiceNotifyChallengeInviteIndex = 5;
#define MatchServiceNotifyChallengeInviteMethod  ::MatchService_Stub::descriptor()->method(5)

constexpr uint32_t MatchServiceNotifyChallengeResultMessageId = 154;
constexpr uint32_t MatchServiceNotifyChallengeResultIndex = 6;
#define MatchServiceNotifyChallengeResultMethod  ::MatchService_Stub::descriptor()->method(6)

constexpr uint32_t MatchServiceWatchBattleMessageId = 163;
constexpr uint32_t MatchServiceWatchBattleIndex = 7;
#define MatchServiceWatchBattleMethod  ::MatchService_Stub::descriptor()->method(7)

constexpr uint32_t MatchServiceRequestBattleTicketMessageId = 179;
constexpr uint32_t MatchServiceRequestBattleTicketIndex = 8;
#define MatchServiceRequestBattleTicketMethod  ::MatchService_Stub::descriptor()->method(8)

constexpr uint32_t MatchServiceListWatchableBattlesMessageId = 164;
constexpr uint32_t MatchServiceListWatchableBattlesIndex = 9;
#define MatchServiceListWatchableBattlesMethod  ::MatchService_Stub::descriptor()->method(9)
