#pragma once

// FIGHTING 冻结到期之后「续冻还是解冻」的纯判定(零项目依赖,单测直接盯)。
// 背景、时序与代价见 docs/design/turn-battle-gap-closure.md §11;常量与到期时刻在 player_battle.h
// (FreezeExpiresAtMs / kFreezeHoldCapMs / FreezeHoldDeadlineMs)。
//
// 为什么要续冻:COMPLETE 不受截止约束(Match 的终局宽限只推迟 ABORT),Match 发件箱积压时结算可能晚于冻结到期才到。
// 那时资产通道的战斗闸(只看 InBattleComp)已经放开,迟到的结算只能按当时持有量夹紧消耗 —— 「药白喝」,可逐局刷。
//
// 冻结到期(nowMs 严格大于 FreezeExpiresAtMs)时 reaper 先看本实体:这一局的结算已在本地应用过
// (收件箱战斗域最近结局环里有它的 APPLIED),就照常解冻,不必查;已越过续冻上限就告警后照常解冻,
// 不查、也不等在途的查询;否则向 match 只读查这一局的 SQL 终局(MatchInternal.GetBattleTerminalDecision),按下表处置:
//   ABORT                                   → 照常解冻(现行规则:记作废局 → 条件删锁 → 摘冻结)
//   NO_ORIGINAL(没有终局,且没有可裁决的原件) → 照常解冻:COMPLETE 永远落不了库,这一局不会有任何结算
//   PENDING(没有终局,但有可裁决的 v1 原件)   → 续冻一拍,锁跟着续(下面说明为什么不是终局证据)
//   COMPLETE(本地还没应用)                  → 续冻一拍,锁跟着续
//   UNKNOWN(查询失败 / 验签不过)            → 续冻一拍
//   后三种越过续冻上限(FreezeHoldDeadlineMs)→ 告警 + 照常解冻
// 「一拍」= reaper 的一个节拍(PlayerBattleSystem::kReaperIntervalSec):下一拍再查一次。
//
// 解冻只放开「人」,不放开这一局带进战斗的道具(2026-11-10,docs/design/battle-item-reservation.md):
// 道具预留只在「不会再从预留里扣」时释放 —— 结算已应用、备战被取消、ABORT、没有可裁决原件(IsBattleHoldReleasable)。
// 越过上限解冻之后,预留由 reaper 的预留复核按同一张终局表收尾,没有上限:它只占住那几样道具,不会把人卡住。
//
// NONE(SQL 里还没有终局决定)要分两种(2026-11-06 负责人定):
//   - 这一局有可裁决的 v1 Create 原件(match 回 terminal_expected = true)→ PENDING,按「结果未知」续冻。
//     冻结到期 = 截止 + 60s 终局宽限 + 60s 投递余量。match 过了截止 + 宽限不立刻补写 ABORT,先向原 battle 实例取证,
//     有「没有房间」或「原进程已不在」的证据才写;没有证据就等到硬上限(截止 + 宽限 + 10 分钟,越过后第一次看到房间还在
//     再给 2 分钟)。match 可用时 ABORT 最晚约在截止 + 14.2 分钟落定(另算恢复循环两次访问的延迟),仍早于续冻上限(截止 + 17 分钟)
//     (2026-11-10,docs/design/cross-zone-matchmaking.md §13.3)。所以冻结到期时查到 NONE 很常见,只说明 ABORT 还没写成
//     (match 正在取证、不可用或恢复任务滞后),而 COMPLETE 不判截止、仍可能先落库,NONE 不是终局证据,照常续冻。
//     这样「解冻之后 COMPLETE 才落库」只剩续冻上限之后的一段,归入有告警的那条残余。
//   - 没有成局行,或原件不可裁决(终局协议 0、旧局、battle 明确采用旧协议、坏形状、operation 为空、缺终局的旧 state 5、
//     只有归档没有决定的终局行;与截止 ABORT 同一判据 autoTerminalOriginal;match 回 terminal_expected = false)
//     → NO_ORIGINAL,照常解冻,不能被 NONE 拖满上限:COMPLETE 与截止 ABORT 两条写终局的路径都要求原件可裁决
//     (go/match/internal/authority/terminal_read.go 的 TerminalOutcomeExpected),这种局永远不会有结算。
//     现网 gather 只经 SaveSupportedIntent 落 v1 原件,这一类只剩上线前的旧成局与锁值对不上任何成局的情形。

#include <cstddef>
#include <cstdint>
#include <iterator>

namespace battle_freeze_hold
{
	// match 对一局 SQL 终局的回答在 scene 侧的归一化结果。底层类型按枚举项个数选;kCount 恒为最后一项。
	enum class TerminalAnswer : uint8_t
	{
		kUnknown,    // 没问到:没有场景密钥 / 没有就绪的 match 节点 / 传输失败 / 回包验签不过 / match 带内 retry / 取值不认识
		kNoOriginal, // 没有终局决定,且这一局没有可裁决的 SQL 原件(terminal_expected = false):永远不会有结算
		kPending,    // 没有终局决定,但有可裁决的 v1 原件(terminal_expected = true):终局迟早落定,COMPLETE 仍可能先落库
		kComplete,   // 已 COMPLETE:全员个人结算已在 Match 发件箱里,迟早投到
		kAbort,      // 已 ABORT:不会再有结算

		kCount
	};

	inline constexpr const char* kTerminalAnswerNames[] = {"UNKNOWN", "NO_ORIGINAL", "PENDING", "COMPLETE", "ABORT"};
	static_assert(std::size(kTerminalAnswerNames) == static_cast<std::size_t>(TerminalAnswer::kCount),
				  "kTerminalAnswerNames 与 TerminalAnswer 逐项对应");

	constexpr const char* TerminalAnswerName(const TerminalAnswer answer)
	{
		const auto index = static_cast<std::size_t>(answer);
		return index < std::size(kTerminalAnswerNames) ? kTerminalAnswerNames[index] : "?";
	}

	// 到期之后的一步处置。底层类型按枚举项个数选;kCount 恒为最后一项。
	enum class ExpiredFightingStep : uint8_t
	{
		kRelease,      // 照常解冻
		kHold,         // 续冻一拍:冻结保留,锁续到续冻上限
		kReleaseAtCap, // 已越过续冻上限:打告警指标,再照常解冻

		kCount
	};

	// 续冻相关的可计数事件(PlayerBattleSystem::FreezeHoldEventCount 读,进程内 relaxed 计数)。告警走同名的结构化日志
	// metric=battle_freeze_held / battle_freeze_hold_cap_reached / battle_terminal_query_lost。kCount 恒为最后一项。
	enum class FreezeHoldEvent : uint8_t
	{
		kHeldComplete, // SQL 终局 COMPLETE 而本地还没应用:续冻一拍
		kHeldUnknown,  // 终局查不到:续冻一拍
		kCapReached,   // 续冻到上限:告警 + 照常解冻
		kQueryLost,    // 查询在途超过一拍加一次调用期限仍没回:按未知处置
		kHeldPending,  // 有可裁决原件而终局还没落定(NONE):续冻一拍

		kCount
	};

	// answer:这一次查询的结论;nowMs:作判定的时刻(发出查询的那一拍);holdDeadlineMs:续冻上限
	// (PlayerBattleSystem::FreezeHoldDeadlineMs)。nowMs 严格大于上限才算越过,与 reaper「严格大于才摘」同一口径。
	// 只回答这一步怎么做,不碰实体、不发请求。纯函数。
	constexpr ExpiredFightingStep DecideExpiredFighting(const TerminalAnswer answer, const uint64_t nowMs,
														const uint64_t holdDeadlineMs)
	{
		if (answer == TerminalAnswer::kAbort || answer == TerminalAnswer::kNoOriginal)
		{
			return ExpiredFightingStep::kRelease;
		}
		// COMPLETE(本地还没应用)、PENDING(有原件的 NONE)与 UNKNOWN 都续冻;越界取值按 UNKNOWN 处理(fail-closed)。
		return nowMs > holdDeadlineMs ? ExpiredFightingStep::kReleaseAtCap : ExpiredFightingStep::kHold;
	}

	// 冻结已不在、这一局的道具预留还在时(续冻到上限解冻、备战到期、重登时锁已过期、离线时被取消),
	// 这一次终局回答能不能放掉预留(docs/design/battle-item-reservation.md)。只有「不会再有结算」的两种回答放:
	// ABORT、没有可裁决的原件(NO_ORIGINAL)。COMPLETE(结算迟早到,到了从预留里扣)、PENDING(终局还没落定)、
	// UNKNOWN(没问到)与越界取值一律留着,而且**没有上限** —— 留着只占住这一局带进战斗的道具,玩家其余的东西照常能动,
	// 不会像冻结那样把人整个卡住;放早了,晚到的结算就只能按持有量夹紧(药白喝)。纯函数。
	constexpr bool IsBattleHoldReleasable(const TerminalAnswer answer)
	{
		return answer == TerminalAnswer::kAbort || answer == TerminalAnswer::kNoOriginal;
	}

	// 预留越过「冻结到期 + 续冻上限」仍没收尾时 ERROR(metric=battle_item_hold_overdue)的节拍:每这么久一行。
	// 与 player_inbox_rules::kStuckWarnEveryFailures 同一个考虑:落在 [10m] 日志窗口之内,还卡着的预留在任意 10 分钟窗口里都查得到一行。
	inline constexpr uint64_t kHoldOverdueLogEveryMs = 10ULL * 60ULL * 1000ULL;
	// overdueMs = 越过那一刻已经多久;tickMs = 调用方的节拍(reaper 一拍)。每个窗口开头的一拍为真,
	// 节拍均匀时同一份预留每个窗口恰好一行。按时长取模、不存状态,确定、可测。tickMs 为 0 恒为假。纯函数。
	constexpr bool IsHoldOverdueLogDue(const uint64_t overdueMs, const uint64_t tickMs)
	{
		return tickMs != 0 && overdueMs % kHoldOverdueLogEveryMs < tickMs;
	}
} // namespace battle_freeze_hold
