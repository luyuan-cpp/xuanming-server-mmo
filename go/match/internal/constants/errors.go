package constants

import "shared/generated/pb/table"

// match 服务业务错误码,经 JoinQueueResponse.error_code 与 TipInfoMessage.id
// 返回客户端。
//
// # 2026-09-02 修:这些码原本全部撞号
//
// 本文件以前从 1 开始手写(ErrInBattle=1、ErrAlreadyQueued=2、
// ErrChallengeSelf=20 ……),而 TipInfoMessage.id 是**全仓唯一一条数轴**,
// 客户端拿 id 去查文案:1..19 是 common 段、20..51 是 login 段。
// 于是「你正在战斗中」在客户端被查成 common 段的 kSuccess、
// 「不能挑战自己」被查成 login 段的某个登录错误 —— 全都不是本意。
//
// guild / friend 早先发现过同类问题,当时的修法是手写一个「私有段」约定
// (guild 200-219 / friend 220-239)。那只是**约定**:它拦不住新来的服务,
// 本文件就是证据 —— 没人告诉过 match 有这条轴。
//
// 现在改成机制:段由 data/tip/Tip.xlsx 的组头行声明
// (`//match_error base=16000 width=1000`),发号器按段分配并在生成期自检
// (段不重叠、码不越界、名字不重名),段表与中文文案表都是生成产物。
//
// 加一个新码 = 往 Tip.xlsx 的 //match_error 组里加一行(名字 + 中文),
// 重跑导表器,然后在下面加一行引用。不要再手写数字。
const (
	// ---- 排队 ----

	// ErrInBattle:battle:lock:{player_id} 存在,玩家已有在途战斗
	// (咨询性检查;权威判定在 scene 的 InBattleComp,见设计文档 §6)。
	ErrInBattle = uint32(table.MatchError_kMatchInBattle)
	// ErrAlreadyQueued:已有排队 ticket,不允许重复入队。
	ErrAlreadyQueued = uint32(table.MatchError_kMatchAlreadyQueued)
	// ErrModeNotOpen:该匹配模式一期未开放(5v5/3v3)或不接受直接入队(切磋)。
	ErrModeNotOpen = uint32(table.MatchError_kMatchModeNotOpen)
	// ErrTeamSizeNotConfigured:PVE 组队的 battle_config_id 未配置凑满人数。
	ErrTeamSizeNotConfigured = uint32(table.MatchError_kMatchTeamSizeNotConfigured)
	// ErrInternal:Redis / snowflake 等内部错误。
	ErrInternal = uint32(table.MatchError_kMatchInternal)
	// ErrTicketMismatch:CancelQueue 携带的 ticket 与当前记录不符。
	ErrTicketMismatch = uint32(table.MatchError_kMatchTicketMismatch)
	// ErrCancelTooLate:ticket 已进入 MATCHED 及之后状态,取消太迟。
	ErrCancelTooLate = uint32(table.MatchError_kMatchCancelTooLate)

	// ---- 场景发起 PK(切磋) ----

	// ErrChallengeSelf:不能挑战自己。
	ErrChallengeSelf = uint32(table.MatchError_kMatchChallengeSelf)
	// ErrChallengeTargetOffline:被挑战者不在线。
	ErrChallengeTargetOffline = uint32(table.MatchError_kMatchChallengeTargetOffline)
	// ErrChallengeTargetBusy:被挑战者已在战斗中(battle:lock 存在)。
	ErrChallengeTargetBusy = uint32(table.MatchError_kMatchChallengeTargetBusy)
	// ErrChallengeSelfBusy:发起者/应战者自己已在战斗中。
	ErrChallengeSelfBusy = uint32(table.MatchError_kMatchChallengeSelfBusy)
	// ErrChallengePending:同一目标已挂着一个待应答挑战,后来者拒绝。
	ErrChallengePending = uint32(table.MatchError_kMatchChallengePending)
	// ErrChallengeExpired:挑战记录不存在或已过期。
	ErrChallengeExpired = uint32(table.MatchError_kMatchChallengeExpired)
	// ErrChallengeNotTarget:应答者不是该挑战的被挑战者。
	ErrChallengeNotTarget = uint32(table.MatchError_kMatchChallengeNotTarget)

	// ---- 观战(二期,设计文档 §10) ----
	// 观众绑定与参战绑定共用 SessionInfo 的 BattleNodeService 槽位,
	// 排队/战斗/观战三态互斥(设计决策 D11)。

	// ErrSpectateWhileQueued:持有 match ticket(排队/开局中)不能观战。
	ErrSpectateWhileQueued = uint32(table.MatchError_kMatchSpectateWhileQueued)
	// ErrSpectateWhileInBattle:battle:lock 存在,战斗中不能观战。
	ErrSpectateWhileInBattle = uint32(table.MatchError_kMatchSpectateWhileInBattle)
	// ErrAlreadyWatching:并发 WatchBattle 冲突(观战标记原子抢占失败);
	// 换场由服务端自动清退旧场,客户端无需先 StopWatchBattle。
	ErrAlreadyWatching = uint32(table.MatchError_kMatchAlreadyWatching)
	// ErrNoWatchableBattle:随机观战当前没有可观战的活跃战斗。
	ErrNoWatchableBattle = uint32(table.MatchError_kMatchNoWatchableBattle)
	// ErrBattleNotWatchable:指定战斗不存在/已结束,或 battle 节点拒绝接入观众。
	ErrBattleNotWatchable = uint32(table.MatchError_kMatchBattleNotWatchable)
	// ErrSpectateOffline:观战者会话不在线,观战首帧无法路由。
	ErrSpectateOffline = uint32(table.MatchError_kMatchSpectateOffline)
)
