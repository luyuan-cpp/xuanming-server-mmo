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

constexpr uint32_t GuildServiceSetGuildMemberRoleMessageId = 19;
constexpr uint32_t GuildServiceSetGuildMemberRoleIndex = 6;
#define GuildServiceSetGuildMemberRoleMethod  ::GuildService_Stub::descriptor()->method(6)

constexpr uint32_t GuildServiceKickGuildMemberMessageId = 217;
constexpr uint32_t GuildServiceKickGuildMemberIndex = 7;
#define GuildServiceKickGuildMemberMethod  ::GuildService_Stub::descriptor()->method(7)

constexpr uint32_t GuildServiceTransferGuildLeaderMessageId = 216;
constexpr uint32_t GuildServiceTransferGuildLeaderIndex = 8;
#define GuildServiceTransferGuildLeaderMethod  ::GuildService_Stub::descriptor()->method(8)

constexpr uint32_t GuildServiceApplyJoinGuildMessageId = 218;
constexpr uint32_t GuildServiceApplyJoinGuildIndex = 9;
#define GuildServiceApplyJoinGuildMethod  ::GuildService_Stub::descriptor()->method(9)

constexpr uint32_t GuildServiceCancelGuildApplicationMessageId = 219;
constexpr uint32_t GuildServiceCancelGuildApplicationIndex = 10;
#define GuildServiceCancelGuildApplicationMethod  ::GuildService_Stub::descriptor()->method(10)

constexpr uint32_t GuildServiceListMyGuildApplicationsMessageId = 222;
constexpr uint32_t GuildServiceListMyGuildApplicationsIndex = 11;
#define GuildServiceListMyGuildApplicationsMethod  ::GuildService_Stub::descriptor()->method(11)

constexpr uint32_t GuildServiceListGuildApplicationsMessageId = 221;
constexpr uint32_t GuildServiceListGuildApplicationsIndex = 12;
#define GuildServiceListGuildApplicationsMethod  ::GuildService_Stub::descriptor()->method(12)

constexpr uint32_t GuildServiceReviewGuildApplicationMessageId = 223;
constexpr uint32_t GuildServiceReviewGuildApplicationIndex = 13;
#define GuildServiceReviewGuildApplicationMethod  ::GuildService_Stub::descriptor()->method(13)

constexpr uint32_t GuildServiceNotifyGuildChangedMessageId = 220;
constexpr uint32_t GuildServiceNotifyGuildChangedIndex = 14;
#define GuildServiceNotifyGuildChangedMethod  ::GuildService_Stub::descriptor()->method(14)

constexpr uint32_t GuildServiceUpdateGuildScoreMessageId = 8;
constexpr uint32_t GuildServiceUpdateGuildScoreIndex = 15;
#define GuildServiceUpdateGuildScoreMethod  ::GuildService_Stub::descriptor()->method(15)

constexpr uint32_t GuildServiceGetGuildRankMessageId = 27;
constexpr uint32_t GuildServiceGetGuildRankIndex = 16;
#define GuildServiceGetGuildRankMethod  ::GuildService_Stub::descriptor()->method(16)

constexpr uint32_t GuildServiceGetGuildRankByGuildMessageId = 52;
constexpr uint32_t GuildServiceGetGuildRankByGuildIndex = 17;
#define GuildServiceGetGuildRankByGuildMethod  ::GuildService_Stub::descriptor()->method(17)
