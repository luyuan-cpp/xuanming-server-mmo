#pragma once
#include <cstdint>

#include "proto/match/match_service.pb.h"

constexpr uint32_t MatchServiceGetNetworkProbePlanMessageId = 275;
constexpr uint32_t MatchServiceGetNetworkProbePlanIndex = 0;
#define MatchServiceGetNetworkProbePlanMethod  ::MatchService_Stub::descriptor()->method(0)

constexpr uint32_t MatchServiceCommitNetworkProbeMessageId = 265;
constexpr uint32_t MatchServiceCommitNetworkProbeIndex = 1;
#define MatchServiceCommitNetworkProbeMethod  ::MatchService_Stub::descriptor()->method(1)

constexpr uint32_t MatchServiceJoinQueueMessageId = 157;
constexpr uint32_t MatchServiceJoinQueueIndex = 2;
#define MatchServiceJoinQueueMethod  ::MatchService_Stub::descriptor()->method(2)

constexpr uint32_t MatchServiceCancelQueueMessageId = 148;
constexpr uint32_t MatchServiceCancelQueueIndex = 3;
#define MatchServiceCancelQueueMethod  ::MatchService_Stub::descriptor()->method(3)

constexpr uint32_t MatchServiceGetQueueStatusMessageId = 153;
constexpr uint32_t MatchServiceGetQueueStatusIndex = 4;
#define MatchServiceGetQueueStatusMethod  ::MatchService_Stub::descriptor()->method(4)

constexpr uint32_t MatchServiceChallengePlayerMessageId = 152;
constexpr uint32_t MatchServiceChallengePlayerIndex = 5;
#define MatchServiceChallengePlayerMethod  ::MatchService_Stub::descriptor()->method(5)

constexpr uint32_t MatchServiceRespondChallengeMessageId = 151;
constexpr uint32_t MatchServiceRespondChallengeIndex = 6;
#define MatchServiceRespondChallengeMethod  ::MatchService_Stub::descriptor()->method(6)

constexpr uint32_t MatchServiceNotifyChallengeInviteMessageId = 156;
constexpr uint32_t MatchServiceNotifyChallengeInviteIndex = 7;
#define MatchServiceNotifyChallengeInviteMethod  ::MatchService_Stub::descriptor()->method(7)

constexpr uint32_t MatchServiceNotifyChallengeResultMessageId = 154;
constexpr uint32_t MatchServiceNotifyChallengeResultIndex = 8;
#define MatchServiceNotifyChallengeResultMethod  ::MatchService_Stub::descriptor()->method(8)

constexpr uint32_t MatchServiceWatchBattleMessageId = 163;
constexpr uint32_t MatchServiceWatchBattleIndex = 9;
#define MatchServiceWatchBattleMethod  ::MatchService_Stub::descriptor()->method(9)

constexpr uint32_t MatchServiceRequestBattleTicketMessageId = 179;
constexpr uint32_t MatchServiceRequestBattleTicketIndex = 10;
#define MatchServiceRequestBattleTicketMethod  ::MatchService_Stub::descriptor()->method(10)

constexpr uint32_t MatchServiceListWatchableBattlesMessageId = 164;
constexpr uint32_t MatchServiceListWatchableBattlesIndex = 11;
#define MatchServiceListWatchableBattlesMethod  ::MatchService_Stub::descriptor()->method(11)

constexpr uint32_t MatchServiceListBattleReportsMessageId = 379;
constexpr uint32_t MatchServiceListBattleReportsIndex = 12;
#define MatchServiceListBattleReportsMethod  ::MatchService_Stub::descriptor()->method(12)

constexpr uint32_t MatchServiceGetBattleReportMessageId = 380;
constexpr uint32_t MatchServiceGetBattleReportIndex = 13;
#define MatchServiceGetBattleReportMethod  ::MatchService_Stub::descriptor()->method(13)
