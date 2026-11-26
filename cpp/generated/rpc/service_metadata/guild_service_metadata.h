#pragma once
#include <cstdint>

#include "proto/guild/guild.pb.h"

constexpr uint32_t GuildServiceCreateGuildMessageId = 15;
constexpr uint32_t GuildServiceCreateGuildIndex = 0;
#define GuildServiceCreateGuildMethod  ::GuildService_Stub::descriptor()->method(0)

constexpr uint32_t GuildServiceGetGuildMessageId = 60;
constexpr uint32_t GuildServiceGetGuildIndex = 1;
#define GuildServiceGetGuildMethod  ::GuildService_Stub::descriptor()->method(1)

constexpr uint32_t GuildServiceGetPlayerGuildMessageId = 35;
constexpr uint32_t GuildServiceGetPlayerGuildIndex = 2;
#define GuildServiceGetPlayerGuildMethod  ::GuildService_Stub::descriptor()->method(2)

constexpr uint32_t GuildServiceLeaveGuildMessageId = 29;
constexpr uint32_t GuildServiceLeaveGuildIndex = 3;
#define GuildServiceLeaveGuildMethod  ::GuildService_Stub::descriptor()->method(3)

constexpr uint32_t GuildServiceDisbandGuildMessageId = 38;
constexpr uint32_t GuildServiceDisbandGuildIndex = 4;
#define GuildServiceDisbandGuildMethod  ::GuildService_Stub::descriptor()->method(4)

constexpr uint32_t GuildServiceSetAnnouncementMessageId = 39;
constexpr uint32_t GuildServiceSetAnnouncementIndex = 5;
#define GuildServiceSetAnnouncementMethod  ::GuildService_Stub::descriptor()->method(5)

constexpr uint32_t GuildServiceRenameGuildMessageId = 439;
constexpr uint32_t GuildServiceRenameGuildIndex = 6;
#define GuildServiceRenameGuildMethod  ::GuildService_Stub::descriptor()->method(6)

constexpr uint32_t GuildServiceSetGuildMemberRoleMessageId = 19;
constexpr uint32_t GuildServiceSetGuildMemberRoleIndex = 7;
#define GuildServiceSetGuildMemberRoleMethod  ::GuildService_Stub::descriptor()->method(7)

constexpr uint32_t GuildServiceKickGuildMemberMessageId = 217;
constexpr uint32_t GuildServiceKickGuildMemberIndex = 8;
#define GuildServiceKickGuildMemberMethod  ::GuildService_Stub::descriptor()->method(8)

constexpr uint32_t GuildServiceTransferGuildLeaderMessageId = 216;
constexpr uint32_t GuildServiceTransferGuildLeaderIndex = 9;
#define GuildServiceTransferGuildLeaderMethod  ::GuildService_Stub::descriptor()->method(9)

constexpr uint32_t GuildServiceApplyJoinGuildMessageId = 218;
constexpr uint32_t GuildServiceApplyJoinGuildIndex = 10;
#define GuildServiceApplyJoinGuildMethod  ::GuildService_Stub::descriptor()->method(10)

constexpr uint32_t GuildServiceCancelGuildApplicationMessageId = 219;
constexpr uint32_t GuildServiceCancelGuildApplicationIndex = 11;
#define GuildServiceCancelGuildApplicationMethod  ::GuildService_Stub::descriptor()->method(11)

constexpr uint32_t GuildServiceListMyGuildApplicationsMessageId = 222;
constexpr uint32_t GuildServiceListMyGuildApplicationsIndex = 12;
#define GuildServiceListMyGuildApplicationsMethod  ::GuildService_Stub::descriptor()->method(12)

constexpr uint32_t GuildServiceListGuildApplicationsMessageId = 221;
constexpr uint32_t GuildServiceListGuildApplicationsIndex = 13;
#define GuildServiceListGuildApplicationsMethod  ::GuildService_Stub::descriptor()->method(13)

constexpr uint32_t GuildServiceReviewGuildApplicationMessageId = 223;
constexpr uint32_t GuildServiceReviewGuildApplicationIndex = 14;
#define GuildServiceReviewGuildApplicationMethod  ::GuildService_Stub::descriptor()->method(14)

constexpr uint32_t GuildServiceNotifyGuildChangedMessageId = 220;
constexpr uint32_t GuildServiceNotifyGuildChangedIndex = 15;
#define GuildServiceNotifyGuildChangedMethod  ::GuildService_Stub::descriptor()->method(15)

constexpr uint32_t GuildServiceUpdateGuildScoreMessageId = 8;
constexpr uint32_t GuildServiceUpdateGuildScoreIndex = 16;
#define GuildServiceUpdateGuildScoreMethod  ::GuildService_Stub::descriptor()->method(16)

constexpr uint32_t GuildServiceGetGuildRankMessageId = 27;
constexpr uint32_t GuildServiceGetGuildRankIndex = 17;
#define GuildServiceGetGuildRankMethod  ::GuildService_Stub::descriptor()->method(17)

constexpr uint32_t GuildServiceGetGuildRankByGuildMessageId = 52;
constexpr uint32_t GuildServiceGetGuildRankByGuildIndex = 18;
#define GuildServiceGetGuildRankByGuildMethod  ::GuildService_Stub::descriptor()->method(18)

constexpr uint32_t GuildServiceGetGuildDonateOptionsMessageId = 120;
constexpr uint32_t GuildServiceGetGuildDonateOptionsIndex = 19;
#define GuildServiceGetGuildDonateOptionsMethod  ::GuildService_Stub::descriptor()->method(19)

constexpr uint32_t GuildServiceDonateToGuildMessageId = 53;
constexpr uint32_t GuildServiceDonateToGuildIndex = 20;
#define GuildServiceDonateToGuildMethod  ::GuildService_Stub::descriptor()->method(20)

constexpr uint32_t GuildServiceUpgradeGuildMessageId = 76;
constexpr uint32_t GuildServiceUpgradeGuildIndex = 21;
#define GuildServiceUpgradeGuildMethod  ::GuildService_Stub::descriptor()->method(21)

constexpr uint32_t GuildServiceGetGuildShopMessageId = 228;
constexpr uint32_t GuildServiceGetGuildShopIndex = 22;
#define GuildServiceGetGuildShopMethod  ::GuildService_Stub::descriptor()->method(22)

constexpr uint32_t GuildServiceBuyGuildShopGoodsMessageId = 233;
constexpr uint32_t GuildServiceBuyGuildShopGoodsIndex = 23;
#define GuildServiceBuyGuildShopGoodsMethod  ::GuildService_Stub::descriptor()->method(23)

constexpr uint32_t GuildServiceGetGuildActivitiesMessageId = 241;
constexpr uint32_t GuildServiceGetGuildActivitiesIndex = 24;
#define GuildServiceGetGuildActivitiesMethod  ::GuildService_Stub::descriptor()->method(24)

constexpr uint32_t GuildServiceLightGuildLanternMessageId = 239;
constexpr uint32_t GuildServiceLightGuildLanternIndex = 25;
#define GuildServiceLightGuildLanternMethod  ::GuildService_Stub::descriptor()->method(25)

constexpr uint32_t GuildServiceClaimGuildReunionMessageId = 240;
constexpr uint32_t GuildServiceClaimGuildReunionIndex = 26;
#define GuildServiceClaimGuildReunionMethod  ::GuildService_Stub::descriptor()->method(26)

constexpr uint32_t GuildServiceStartGuildTrialMessageId = 242;
constexpr uint32_t GuildServiceStartGuildTrialIndex = 27;
#define GuildServiceStartGuildTrialMethod  ::GuildService_Stub::descriptor()->method(27)

constexpr uint32_t GuildServiceRespondGuildTrialInviteMessageId = 243;
constexpr uint32_t GuildServiceRespondGuildTrialInviteIndex = 28;
#define GuildServiceRespondGuildTrialInviteMethod  ::GuildService_Stub::descriptor()->method(28)

constexpr uint32_t GuildServiceGetGuildWarOverviewMessageId = 485;
constexpr uint32_t GuildServiceGetGuildWarOverviewIndex = 29;
#define GuildServiceGetGuildWarOverviewMethod  ::GuildService_Stub::descriptor()->method(29)

constexpr uint32_t GuildServiceSetGuildWarStanceMessageId = 486;
constexpr uint32_t GuildServiceSetGuildWarStanceIndex = 30;
#define GuildServiceSetGuildWarStanceMethod  ::GuildService_Stub::descriptor()->method(30)

constexpr uint32_t GuildServiceRegisterGuildWarGarrisonMessageId = 487;
constexpr uint32_t GuildServiceRegisterGuildWarGarrisonIndex = 31;
#define GuildServiceRegisterGuildWarGarrisonMethod  ::GuildService_Stub::descriptor()->method(31)

constexpr uint32_t GuildServiceStartGuildWarBattleMessageId = 488;
constexpr uint32_t GuildServiceStartGuildWarBattleIndex = 32;
#define GuildServiceStartGuildWarBattleMethod  ::GuildService_Stub::descriptor()->method(32)

constexpr uint32_t GuildServiceClaimGuildWarMilestoneMessageId = 489;
constexpr uint32_t GuildServiceClaimGuildWarMilestoneIndex = 33;
#define GuildServiceClaimGuildWarMilestoneMethod  ::GuildService_Stub::descriptor()->method(33)

constexpr uint32_t GuildServiceClaimGuildWarDividendMessageId = 490;
constexpr uint32_t GuildServiceClaimGuildWarDividendIndex = 34;
#define GuildServiceClaimGuildWarDividendMethod  ::GuildService_Stub::descriptor()->method(34)

constexpr uint32_t GuildServicePostGuildWarBountyMessageId = 491;
constexpr uint32_t GuildServicePostGuildWarBountyIndex = 35;
#define GuildServicePostGuildWarBountyMethod  ::GuildService_Stub::descriptor()->method(35)

constexpr uint32_t GuildServiceTrackGuildWarBountyMessageId = 492;
constexpr uint32_t GuildServiceTrackGuildWarBountyIndex = 36;
#define GuildServiceTrackGuildWarBountyMethod  ::GuildService_Stub::descriptor()->method(36)

constexpr uint32_t GuildServiceGetGuildWarBattleResultMessageId = 493;
constexpr uint32_t GuildServiceGetGuildWarBattleResultIndex = 37;
#define GuildServiceGetGuildWarBattleResultMethod  ::GuildService_Stub::descriptor()->method(37)

constexpr uint32_t GuildServiceNotifyGuildWarAlertMessageId = 494;
constexpr uint32_t GuildServiceNotifyGuildWarAlertIndex = 38;
#define GuildServiceNotifyGuildWarAlertMethod  ::GuildService_Stub::descriptor()->method(38)

constexpr uint32_t GuildServiceInspectFieldWarTargetsMessageId = 499;
constexpr uint32_t GuildServiceInspectFieldWarTargetsIndex = 39;
#define GuildServiceInspectFieldWarTargetsMethod  ::GuildService_Stub::descriptor()->method(39)

constexpr uint32_t GuildServiceIssueGuildWarFeudMessageId = 567;
constexpr uint32_t GuildServiceIssueGuildWarFeudIndex = 40;
#define GuildServiceIssueGuildWarFeudMethod  ::GuildService_Stub::descriptor()->method(40)

constexpr uint32_t GuildServiceRespondGuildWarFeudMessageId = 568;
constexpr uint32_t GuildServiceRespondGuildWarFeudIndex = 41;
#define GuildServiceRespondGuildWarFeudMethod  ::GuildService_Stub::descriptor()->method(41)

constexpr uint32_t GuildServiceGetGuildJianghuGrudgeWallMessageId = 547;
constexpr uint32_t GuildServiceGetGuildJianghuGrudgeWallIndex = 42;
#define GuildServiceGetGuildJianghuGrudgeWallMethod  ::GuildService_Stub::descriptor()->method(42)
