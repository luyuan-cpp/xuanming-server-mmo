package team

import "shared/generated/pb/table"

// 组队业务错误码,经 TeamResponse.error_message(TipInfoMessage.id)返回客户端。
//
// 码值只能由导表器从 data/tip/Tip.xlsx 的 //team_error 组(base=4000)发号
// (AGENTS §4 / §7 #5,设计文档 docs/design/team-system.md §D.4)。
// 这些码不能放进 match 的 constants/errors.go:match 段是 16000 起,段校验会失败。
// 「哪个码算故障」由表的 fault 列生成到 shared/generated/tip/faults.go,这里不另写集合。
const (
	// ---- 复用 team_error 组既有码 ----

	// ErrPlayerId:缺 session / 目标 id 为 0 / 对自己申请或邀请。
	ErrPlayerId = uint32(table.TeamError_kTeamPlayerId)
	// ErrMembersFull:申请、同意、接受邀请、邀请、开战时队伍已满。
	ErrMembersFull = uint32(table.TeamError_kTeamMembersFull)
	// ErrMemberInTeam:建队时自己已在队;申请、同意、邀请、接受时自己或对方已在队。
	ErrMemberInTeam = uint32(table.TeamError_kTeamMemberInTeam)
	// ErrMemberNotInTeam:踢人或转让的目标不在本队。
	ErrMemberNotInTeam = uint32(table.TeamError_kTeamMemberNotInTeam)
	// ErrKickSelf:踢自己。
	ErrKickSelf = uint32(table.TeamError_kTeamKickSelf)
	// ErrKickNotLeader:非队长踢人。
	ErrKickNotLeader = uint32(table.TeamError_kTeamKickNotLeader)
	// ErrAppointSelf:转让给自己。
	ErrAppointSelf = uint32(table.TeamError_kTeamAppointSelf)
	// ErrAppointNotLeader:非队长转让。
	ErrAppointNotLeader = uint32(table.TeamError_kTeamAppointLeaderNotLeader)
	// ErrApplicationNotFound:审批的申请不存在或已过期。
	ErrApplicationNotFound = uint32(table.TeamError_kTeamNotInApplicantList)
	// ErrNoTeam:自己或目标没有队伍、队伍已解散、expected_team_id 不匹配。
	ErrNoTeam = uint32(table.TeamError_kTeamHasNotTeamId)
	// ErrDisbandNotLeader:非队长解散。
	ErrDisbandNotLeader = uint32(table.TeamError_kTeamDismissNotLeader)
	// ErrTargetOffline:邀请目标不在线。
	ErrTargetOffline = uint32(table.TeamError_kTeamPlayerNotFound)

	// ---- 本期新增码(已在 Tip.xlsx //team_error 组发号,4018..4030,§D.4) ----

	// ErrNotLeader:非队长审批、邀请、开战。
	ErrNotLeader = uint32(table.TeamError_kTeamNotLeader)
	// ErrHomeZoneUnknown:查不到 home zone(fail-closed,zone 必须写进记录)。
	ErrHomeZoneUnknown = uint32(table.TeamError_kTeamHomeZoneUnknown)
	// ErrCrossZoneDenied:AllowCrossZone=false 时跨区组队。
	ErrCrossZoneDenied = uint32(table.TeamError_kTeamCrossZoneDenied)
	// ErrInviteNotFound:邀请不存在或已过期。
	ErrInviteNotFound = uint32(table.TeamError_kTeamInviteNotFound)
	// ErrInviteLimit:被邀请人待处理的邀请已达上限(S_COMMIT 返回 {-3,i})。
	ErrInviteLimit = uint32(table.TeamError_kTeamInviteLimit)
	// ErrInMatch:开战锁有效期间变更名单。
	ErrInMatch = uint32(table.TeamError_kTeamInMatch)
	// ErrMemberOffline:开战或转让时队员不在线(parameters[0] = player_id)。
	ErrMemberOffline = uint32(table.TeamError_kTeamMemberOffline)
	// ErrMemberInBattle:开战时队员在战斗中(parameters[0] = player_id)。
	ErrMemberInBattle = uint32(table.TeamError_kTeamMemberInBattle)
	// ErrMemberNotReady:开战时队员在排队、不在场景或票据冲突(parameters[0] = player_id)。
	ErrMemberNotReady = uint32(table.TeamError_kTeamMemberNotReady)
	// ErrDungeonNotOpen:该副本没有配置组队人数。
	ErrDungeonNotOpen = uint32(table.TeamError_kTeamDungeonNotOpen)
	// ErrSizeExceeded:队伍人数超过副本上限。
	ErrSizeExceeded = uint32(table.TeamError_kTeamSizeExceeded)
	// ErrStateChanged:提交重试耗尽 / 请求 ctx 已过期放弃提交。
	ErrStateChanged = uint32(table.TeamError_kTeamStateChanged)
	// ErrInternal:Redis、data_service、发号故障(表中 fault=1)。
	ErrInternal = uint32(table.TeamError_kTeamInternal)
)
