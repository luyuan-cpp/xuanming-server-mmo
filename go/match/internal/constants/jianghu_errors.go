package constants

import "shared/generated/pb/table"

// 江湖恩怨与天墉生死状一期错误码（独立 //jianghu_error 段 [40000, 41000)，由导表器生成，严禁手写数字）。
const (
	ErrJianghuDuelDisabled            = uint32(table.JianghuError_kJianghuDuelDisabled)
	ErrJianghuBlockedByPeer           = uint32(table.JianghuError_kJianghuBlockedByPeer)
	ErrJianghuTargetRefusesStrangers  = uint32(table.JianghuError_kJianghuTargetRefusesStrangers)
	ErrJianghuSelfRefusesStrangers    = uint32(table.JianghuError_kJianghuSelfRefusesStrangers)
	ErrJianghuSameTargetCooldown      = uint32(table.JianghuError_kJianghuSameTargetCooldown)
	ErrJianghuDailyInviteLimitReached = uint32(table.JianghuError_kJianghuDailyInviteLimitReached)
	ErrJianghuTeamSizeMismatch        = uint32(table.JianghuError_kJianghuTeamSizeMismatch)
	ErrJianghuTeamChangedAborted      = uint32(table.JianghuError_kJianghuTeamChangedAborted)
	ErrJianghuInsufficientWritOrSilver = uint32(table.JianghuError_kJianghuInsufficientWritOrSilver)
	ErrJianghuInvitePendingOrExpired  = uint32(table.JianghuError_kJianghuInvitePendingOrExpired)
	ErrJianghuNotInviteParticipant    = uint32(table.JianghuError_kJianghuNotInviteParticipant)
	ErrJianghuNoActiveDefeatMark      = uint32(table.JianghuError_kJianghuNoActiveDefeatMark)
	ErrJianghuGrudgeNotFound          = uint32(table.JianghuError_kJianghuGrudgeNotFound)
	ErrJianghuServiceBusy             = uint32(table.JianghuError_kJianghuServiceBusy)
)
