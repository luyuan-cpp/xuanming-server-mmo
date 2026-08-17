package constants

// match 服务业务错误码,经 JoinQueueResponse.error_code 与 TipInfoMessage.id
// 返回客户端(照 friend/scene_manager 的 per-service 本地常量模式)。
const (
	// ---- 排队 ----

	// ErrInBattle:battle:lock:{player_id} 存在,玩家已有在途战斗
	// (咨询性检查;权威判定在 scene 的 InBattleComp,见设计文档 §6)。
	ErrInBattle uint32 = 1
	// ErrAlreadyQueued:已有排队 ticket,不允许重复入队。
	ErrAlreadyQueued uint32 = 2
	// ErrModeNotOpen:该匹配模式一期未开放(5v5/3v3)或不接受直接入队(切磋)。
	ErrModeNotOpen uint32 = 3
	// ErrTeamSizeNotConfigured:PVE 组队的 battle_config_id 未配置凑满人数。
	ErrTeamSizeNotConfigured uint32 = 4
	// ErrInternal:Redis / snowflake 等内部错误。
	ErrInternal uint32 = 5
	// ErrTicketMismatch:CancelQueue 携带的 ticket 与当前记录不符。
	ErrTicketMismatch uint32 = 6
	// ErrCancelTooLate:ticket 已进入 MATCHED 及之后状态,取消太迟。
	ErrCancelTooLate uint32 = 7

	// ---- 场景发起 PK(切磋) ----

	// ErrChallengeSelf:不能挑战自己。
	ErrChallengeSelf uint32 = 20
	// ErrChallengeTargetOffline:被挑战者不在线。
	ErrChallengeTargetOffline uint32 = 21
	// ErrChallengeTargetBusy:被挑战者已在战斗中(battle:lock 存在)。
	ErrChallengeTargetBusy uint32 = 22
	// ErrChallengeSelfBusy:发起者/应战者自己已在战斗中。
	ErrChallengeSelfBusy uint32 = 23
	// ErrChallengePending:同一目标已挂着一个待应答挑战,后来者拒绝。
	ErrChallengePending uint32 = 24
	// ErrChallengeExpired:挑战记录不存在或已过期。
	ErrChallengeExpired uint32 = 25
	// ErrChallengeNotTarget:应答者不是该挑战的被挑战者。
	ErrChallengeNotTarget uint32 = 26
)
