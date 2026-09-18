#pragma once
#include <cstdint>

#include "proto/team/team.pb.h"

constexpr uint32_t ClientPlayerTeamCreateTeamMessageId = 214;
constexpr uint32_t ClientPlayerTeamCreateTeamIndex = 0;
#define ClientPlayerTeamCreateTeamMethod  ::ClientPlayerTeam_Stub::descriptor()->method(0)

constexpr uint32_t ClientPlayerTeamGetMyTeamMessageId = 207;
constexpr uint32_t ClientPlayerTeamGetMyTeamIndex = 1;
#define ClientPlayerTeamGetMyTeamMethod  ::ClientPlayerTeam_Stub::descriptor()->method(1)

constexpr uint32_t ClientPlayerTeamApplyJoinTeamMessageId = 206;
constexpr uint32_t ClientPlayerTeamApplyJoinTeamIndex = 2;
#define ClientPlayerTeamApplyJoinTeamMethod  ::ClientPlayerTeam_Stub::descriptor()->method(2)

constexpr uint32_t ClientPlayerTeamHandleApplicationMessageId = 208;
constexpr uint32_t ClientPlayerTeamHandleApplicationIndex = 3;
#define ClientPlayerTeamHandleApplicationMethod  ::ClientPlayerTeam_Stub::descriptor()->method(3)

constexpr uint32_t ClientPlayerTeamInviteToTeamMessageId = 201;
constexpr uint32_t ClientPlayerTeamInviteToTeamIndex = 4;
#define ClientPlayerTeamInviteToTeamMethod  ::ClientPlayerTeam_Stub::descriptor()->method(4)

constexpr uint32_t ClientPlayerTeamRespondInviteMessageId = 204;
constexpr uint32_t ClientPlayerTeamRespondInviteIndex = 5;
#define ClientPlayerTeamRespondInviteMethod  ::ClientPlayerTeam_Stub::descriptor()->method(5)

constexpr uint32_t ClientPlayerTeamListMyInvitesMessageId = 205;
constexpr uint32_t ClientPlayerTeamListMyInvitesIndex = 6;
#define ClientPlayerTeamListMyInvitesMethod  ::ClientPlayerTeam_Stub::descriptor()->method(6)

constexpr uint32_t ClientPlayerTeamLeaveTeamMessageId = 210;
constexpr uint32_t ClientPlayerTeamLeaveTeamIndex = 7;
#define ClientPlayerTeamLeaveTeamMethod  ::ClientPlayerTeam_Stub::descriptor()->method(7)

constexpr uint32_t ClientPlayerTeamKickMemberMessageId = 202;
constexpr uint32_t ClientPlayerTeamKickMemberIndex = 8;
#define ClientPlayerTeamKickMemberMethod  ::ClientPlayerTeam_Stub::descriptor()->method(8)

constexpr uint32_t ClientPlayerTeamTransferLeaderMessageId = 212;
constexpr uint32_t ClientPlayerTeamTransferLeaderIndex = 9;
#define ClientPlayerTeamTransferLeaderMethod  ::ClientPlayerTeam_Stub::descriptor()->method(9)

constexpr uint32_t ClientPlayerTeamDisbandTeamMessageId = 209;
constexpr uint32_t ClientPlayerTeamDisbandTeamIndex = 10;
#define ClientPlayerTeamDisbandTeamMethod  ::ClientPlayerTeam_Stub::descriptor()->method(10)

constexpr uint32_t ClientPlayerTeamStartTeamMatchMessageId = 211;
constexpr uint32_t ClientPlayerTeamStartTeamMatchIndex = 11;
#define ClientPlayerTeamStartTeamMatchMethod  ::ClientPlayerTeam_Stub::descriptor()->method(11)

constexpr uint32_t ClientPlayerTeamNotifyTeamSnapshotMessageId = 213;
constexpr uint32_t ClientPlayerTeamNotifyTeamSnapshotIndex = 12;
#define ClientPlayerTeamNotifyTeamSnapshotMethod  ::ClientPlayerTeam_Stub::descriptor()->method(12)

constexpr uint32_t ClientPlayerTeamNotifyTeamInviteMessageId = 215;
constexpr uint32_t ClientPlayerTeamNotifyTeamInviteIndex = 13;
#define ClientPlayerTeamNotifyTeamInviteMethod  ::ClientPlayerTeam_Stub::descriptor()->method(13)

constexpr uint32_t ClientPlayerTeamNotifyTeamEventMessageId = 203;
constexpr uint32_t ClientPlayerTeamNotifyTeamEventIndex = 14;
#define ClientPlayerTeamNotifyTeamEventMethod  ::ClientPlayerTeam_Stub::descriptor()->method(14)
