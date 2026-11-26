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

constexpr uint32_t MatchServiceListBattleScriptsMessageId = 389;
constexpr uint32_t MatchServiceListBattleScriptsIndex = 14;
#define MatchServiceListBattleScriptsMethod  ::MatchService_Stub::descriptor()->method(14)

constexpr uint32_t MatchServiceSubmitBattleScriptMessageId = 390;
constexpr uint32_t MatchServiceSubmitBattleScriptIndex = 15;
#define MatchServiceSubmitBattleScriptMethod  ::MatchService_Stub::descriptor()->method(15)

constexpr uint32_t MatchServiceDeleteBattleScriptMessageId = 391;
constexpr uint32_t MatchServiceDeleteBattleScriptIndex = 16;
#define MatchServiceDeleteBattleScriptMethod  ::MatchService_Stub::descriptor()->method(16)

constexpr uint32_t MatchServiceSetBattleScriptLoadoutMessageId = 392;
constexpr uint32_t MatchServiceSetBattleScriptLoadoutIndex = 17;
#define MatchServiceSetBattleScriptLoadoutMethod  ::MatchService_Stub::descriptor()->method(17)

constexpr uint32_t MatchServiceGetRankedProfileMessageId = 448;
constexpr uint32_t MatchServiceGetRankedProfileIndex = 18;
#define MatchServiceGetRankedProfileMethod  ::MatchService_Stub::descriptor()->method(18)

constexpr uint32_t MatchServiceSaveRankedPresetMessageId = 449;
constexpr uint32_t MatchServiceSaveRankedPresetIndex = 19;
#define MatchServiceSaveRankedPresetMethod  ::MatchService_Stub::descriptor()->method(19)

constexpr uint32_t MatchServiceUpdateRankedCosmeticMessageId = 450;
constexpr uint32_t MatchServiceUpdateRankedCosmeticIndex = 20;
#define MatchServiceUpdateRankedCosmeticMethod  ::MatchService_Stub::descriptor()->method(20)

constexpr uint32_t MatchServiceGetRankedBattleResultMessageId = 451;
constexpr uint32_t MatchServiceGetRankedBattleResultIndex = 21;
#define MatchServiceGetRankedBattleResultMethod  ::MatchService_Stub::descriptor()->method(21)

constexpr uint32_t MatchServiceGetTournamentOverviewMessageId = 465;
constexpr uint32_t MatchServiceGetTournamentOverviewIndex = 22;
#define MatchServiceGetTournamentOverviewMethod  ::MatchService_Stub::descriptor()->method(22)

constexpr uint32_t MatchServiceCreateTournamentSquadMessageId = 466;
constexpr uint32_t MatchServiceCreateTournamentSquadIndex = 23;
#define MatchServiceCreateTournamentSquadMethod  ::MatchService_Stub::descriptor()->method(23)

constexpr uint32_t MatchServiceInviteTournamentMemberMessageId = 467;
constexpr uint32_t MatchServiceInviteTournamentMemberIndex = 24;
#define MatchServiceInviteTournamentMemberMethod  ::MatchService_Stub::descriptor()->method(24)

constexpr uint32_t MatchServiceRespondTournamentInviteMessageId = 468;
constexpr uint32_t MatchServiceRespondTournamentInviteIndex = 25;
#define MatchServiceRespondTournamentInviteMethod  ::MatchService_Stub::descriptor()->method(25)

constexpr uint32_t MatchServiceLeaveTournamentSquadMessageId = 469;
constexpr uint32_t MatchServiceLeaveTournamentSquadIndex = 26;
#define MatchServiceLeaveTournamentSquadMethod  ::MatchService_Stub::descriptor()->method(26)

constexpr uint32_t MatchServiceUpdateTournamentLineupMessageId = 470;
constexpr uint32_t MatchServiceUpdateTournamentLineupIndex = 27;
#define MatchServiceUpdateTournamentLineupMethod  ::MatchService_Stub::descriptor()->method(27)

constexpr uint32_t MatchServiceRegisterTournamentSquadMessageId = 471;
constexpr uint32_t MatchServiceRegisterTournamentSquadIndex = 28;
#define MatchServiceRegisterTournamentSquadMethod  ::MatchService_Stub::descriptor()->method(28)

constexpr uint32_t MatchServiceListTournamentStandingsMessageId = 472;
constexpr uint32_t MatchServiceListTournamentStandingsIndex = 29;
#define MatchServiceListTournamentStandingsMethod  ::MatchService_Stub::descriptor()->method(29)

constexpr uint32_t MatchServiceStartTournamentMatchMessageId = 473;
constexpr uint32_t MatchServiceStartTournamentMatchIndex = 30;
#define MatchServiceStartTournamentMatchMethod  ::MatchService_Stub::descriptor()->method(30)

constexpr uint32_t MatchServiceCheerTournamentSquadMessageId = 474;
constexpr uint32_t MatchServiceCheerTournamentSquadIndex = 31;
#define MatchServiceCheerTournamentSquadMethod  ::MatchService_Stub::descriptor()->method(31)

constexpr uint32_t MatchServiceClaimTournamentRewardMessageId = 475;
constexpr uint32_t MatchServiceClaimTournamentRewardIndex = 32;
#define MatchServiceClaimTournamentRewardMethod  ::MatchService_Stub::descriptor()->method(32)

constexpr uint32_t MatchServiceSaluteTournamentChampionMessageId = 506;
constexpr uint32_t MatchServiceSaluteTournamentChampionIndex = 33;
#define MatchServiceSaluteTournamentChampionMethod  ::MatchService_Stub::descriptor()->method(33)

constexpr uint32_t MatchServiceSendTournamentSpectateCheerMessageId = 507;
constexpr uint32_t MatchServiceSendTournamentSpectateCheerIndex = 34;
#define MatchServiceSendTournamentSpectateCheerMethod  ::MatchService_Stub::descriptor()->method(34)

constexpr uint32_t MatchServiceGetTournamentSpectateCheerMessageId = 508;
constexpr uint32_t MatchServiceGetTournamentSpectateCheerIndex = 35;
#define MatchServiceGetTournamentSpectateCheerMethod  ::MatchService_Stub::descriptor()->method(35)

constexpr uint32_t MatchServiceUpdateTournamentSquadEmblemMessageId = 511;
constexpr uint32_t MatchServiceUpdateTournamentSquadEmblemIndex = 36;
#define MatchServiceUpdateTournamentSquadEmblemMethod  ::MatchService_Stub::descriptor()->method(36)

constexpr uint32_t MatchServiceStartTournamentWarmupMessageId = 512;
constexpr uint32_t MatchServiceStartTournamentWarmupIndex = 37;
#define MatchServiceStartTournamentWarmupMethod  ::MatchService_Stub::descriptor()->method(37)

constexpr uint32_t MatchServicePredictTournamentMatchMessageId = 513;
constexpr uint32_t MatchServicePredictTournamentMatchIndex = 38;
#define MatchServicePredictTournamentMatchMethod  ::MatchService_Stub::descriptor()->method(38)

constexpr uint32_t MatchServiceGetTournamentPredictionBoardMessageId = 514;
constexpr uint32_t MatchServiceGetTournamentPredictionBoardIndex = 39;
#define MatchServiceGetTournamentPredictionBoardMethod  ::MatchService_Stub::descriptor()->method(39)

constexpr uint32_t MatchServiceBanTournamentLeylineMessageId = 534;
constexpr uint32_t MatchServiceBanTournamentLeylineIndex = 40;
#define MatchServiceBanTournamentLeylineMethod  ::MatchService_Stub::descriptor()->method(40)

constexpr uint32_t MatchServiceGetTournamentHighlightsMessageId = 535;
constexpr uint32_t MatchServiceGetTournamentHighlightsIndex = 41;
#define MatchServiceGetTournamentHighlightsMethod  ::MatchService_Stub::descriptor()->method(41)
