#pragma once

#include <chrono>
#include <cstdint>
#include <cstring>
#include <iterator>
#include "services/scene/player/system/handoff_sql_barrier.h"

#include "services/scene/player/system/player_exit_intent.h" // ExitCause

// 断线释放标记(A1′)与载入时清标记(A2′)的纯判定 —— 不含 entt / redis / muduo,可单测
// (cpp/tests/cross_zone_test 的 ExitRelease*)。风格同 handoff_mark_withdraw.h。
//
// 设计:docs/design/cross-zone-scene-travel.md §12.6.3 第二步 / 第三步、§12.6.4 M2 / M3 / M6 / M7 / M10 / M11 / M14。
// 粘合代码在 player_lifecycle.cpp(WriteExitReleaseMark / BeginInheritedMarkClear / RetryInheritedMarkClears)。
//
// A1、弃载补写、疏散与改派凭证都必须消费实体/原载入缓存的 H 资格，不从新 Redis owner 读值补权。
// SQL 入场登记在 Activate 前原子核 owner、原 scene instance、原 admission generation 并清继承标记；
// 后续 A2 只核原 H，不再次 DEL。旧加载读到新 prior 仍因 generation floor 被拒。
// 撤销即使 mark 缺席也轮换资格；未确认前保持冻结。未知 owner 的旧入口不再有清标记资格。
// 交错与跨语言金样见 owner_epoch_crosslang_test.go；Redis 键当前不是 Cluster 同槽合同。
namespace exit_release_mark
{
	// ── A1′:退出收尾时写不写 ─────────────────────────────────────────────────

	// 能写标记的退出原因:客户端断线 / 本节点停机 / 单场景排空。
	//   kIdentityConflict   身份冲突期间写标记可能给接手的新进程开放行口(M3);
	//   kReleasedByTransfer scene_manager 要把玩家交给别的节点,落点自己会铸造,不需要本节点的标记;
	//   kUnspecified        来源不明,漏标只会少写。
	constexpr bool IsReleaseCause(ExitCause cause)
	{
		return cause == ExitCause::kClientDisconnect || cause == ExitCause::kNodeShutdown ||
			   cause == ExitCause::kSceneDrain;
	}

	// 判定的输入。全部由调用方在 FinishExitAfterPersist 里采集好传入,本文件不读任何全局状态。
	struct ExitReleaseFacts
	{
		bool featureEnabled{false};		   // 运行期开关(SCENE_EXIT_RELEASE_MARK,默认开,M14)
		bool entityValid{false};		   // 实体仍有效
		bool hasIntent{false};			   // 带 PlayerExitIntentComp
		ExitCause cause{ExitCause::kUnspecified};
		bool releaseMarkSuppressed{false};  // 意图组件上的粘性压制位
		ExitCause suppressedBy{ExitCause::kCount}; // 第一次触发压制的原因(kCount = 未压制)
		// 本次 DispatchEmergencyRelocate 消费了疏散票据:这次的标记归改派负责(条件写,可能没写成;改派没生效要踢线时
		// 由待确认表兜底补写)。票据作废(没发改派)时为 false,A1′ 照常判定。
		bool relocateTicketConsumed{false};
		bool handoffMarkInflight{false};	   // "退出优先"且交接标记已写、交接 EnterScene 可能在途(M11)
		uint64_t ownerEpoch{0};			   // PlayerOwnerEpochComp.epoch
		bool identityValid{false};		   // 本节点身份当前确认有效(M3)
	};

	// 判定结果。除 kWrite 外每一项都是一个"不写"的原因,按原因计数(exit_release_stats)。
	// 底层类型 uint8_t 数的是结果种类数(现 12 种),AGENTS §11.2。kCount 固定为最后一项,同时是名表长度。
	enum class ExitReleaseDecision : uint8_t
	{
		kWrite,
		kSkipDisabled,			   // 运行期开关关闭
		kSkipEntityInvalid,		   // 实体已无效
		kSkipIntentMissing,		   // 没有意图组件(成对约定被破坏,fail-closed 不写)
		kSkipCause,				   // 原因不在可写集合里(released_by_transfer / identity_conflict / unspecified)
		kSkipSuppressedRelease,	   // 退出中又收到 ReleasePlayer 且 dev 旁路开关开着(M6)
		kSkipSuppressedIdentity,   // 退出中又遇到整节点疏散(身份冲突)
		kSkipSuppressedUnspecified, // 退出中又收到来源不明的退出请求
		kSkipRelocate,			   // 本次消费了疏散票据:改派负责这次的标记(条件写,可能没写成;踢线前由待确认表兜底补写),A1′ 不得覆盖
		kSkipHandoffInflight,	   // 退出优先且交接标记已写(M11)
		kSkipEpochUnknown,		   // owner_epoch 为 0(兼容窗口,换手门对它本来就不设防)
		kSkipIdentityConflict,	   // 本节点身份不确认有效(疏散中 / etcd 租约推定过期,M3)

		kCount
	};

	constexpr std::size_t kExitReleaseDecisionCount = static_cast<std::size_t>(ExitReleaseDecision::kCount);

	// 日志与汇总行里的名字,与枚举一一对应(也是 [ExitRelease] 汇总行里的 key)。
	inline constexpr const char *kExitReleaseDecisionNames[] = {
		"attempted",
		"skip_disabled",
		"skip_entity_invalid",
		"skip_intent_missing",
		"skip_cause",
		"skip_suppressed_release",
		"skip_suppressed_identity",
		"skip_suppressed_unspecified",
		"skip_relocate",
		"skip_handoff_inflight",
		"skip_epoch_unknown",
		"skip_identity_conflict",
	};
	static_assert(std::size(kExitReleaseDecisionNames) == kExitReleaseDecisionCount,
				  "kExitReleaseDecisionNames 必须与 ExitReleaseDecision 一一对应");

	constexpr const char *ExitReleaseDecisionName(ExitReleaseDecision decision)
	{
		const auto index = static_cast<std::size_t>(decision);
		return index < kExitReleaseDecisionCount ? kExitReleaseDecisionNames[index] : "?";
	}

	// 全部满足才写;第一个不满足的条件就是计数的原因。顺序只影响"计到哪个原因",不影响写不写。
	constexpr ExitReleaseDecision DecideExitReleaseMark(const ExitReleaseFacts &facts)
	{
		if (!facts.featureEnabled)
		{
			return ExitReleaseDecision::kSkipDisabled;
		}
		if (!facts.entityValid)
		{
			return ExitReleaseDecision::kSkipEntityInvalid;
		}
		if (!facts.hasIntent)
		{
			return ExitReleaseDecision::kSkipIntentMissing;
		}
		if (!IsReleaseCause(facts.cause))
		{
			return ExitReleaseDecision::kSkipCause;
		}
		if (facts.releaseMarkSuppressed)
		{
			switch (facts.suppressedBy)
			{
			case ExitCause::kReleasedByTransfer:
				return ExitReleaseDecision::kSkipSuppressedRelease;
			case ExitCause::kIdentityConflict:
				return ExitReleaseDecision::kSkipSuppressedIdentity;
			default:
				return ExitReleaseDecision::kSkipSuppressedUnspecified; // 含越界值:按来源不明
			}
		}
		if (facts.relocateTicketConsumed)
		{
			return ExitReleaseDecision::kSkipRelocate;
		}
		if (facts.handoffMarkInflight)
		{
			return ExitReleaseDecision::kSkipHandoffInflight;
		}
		if (facts.ownerEpoch == 0)
		{
			return ExitReleaseDecision::kSkipEpochUnknown;
		}
		if (!facts.identityValid)
		{
			return ExitReleaseDecision::kSkipIdentityConflict;
		}
		return ExitReleaseDecision::kWrite;
	}

    // KEYS=owner/save_barrier/handoff，ARGV=E/原mark/300/原H/本轮F。
    // owner与原H同时成立才HSET资格+SET标记；完全相同F只认幂等，不延长TTL。
    // 返回 1 写成 / 0 owner 已不是 E / -2 原 H 已被轮换。会话结束凭证改用 kCredentialMarkLua(同一组返回码)。
	inline const std::string& kLuaWriteIfOwnerEpoch = handoff_sql_barrier::kReleaseMarkLua;

	// 条件写的应答分类。一律不重试(R7):写不成的后果只是玩家这一次重登可能回 18、下一次重试放行,
	// 而重试会把"这一刻已落盘"的凭证推迟到一个不再成立的时刻。
	// kEpochMoved 与 kSuperseded 都是"不写是对的",分开是为了排障:前者是归属已推进,后者是本次消费的原资格已被
	// 更新的登记 / 撤销 / 发布轮换(save_barrier.handoff_fence,r2-lc#1 / r2-lc#2 之后的主要拒绝路径)。
	enum class MarkWriteResult : uint8_t
	{
		kWritten,	 // 返回 1
		kEpochMoved, // 返回 0:owner_epoch 已变,不写是对的
		kSuperseded, // 返回 -2:原资格已被轮换,不写是对的
		kFailed,	 // 空应答 / ERROR / 非整数 / 其它整数

		kCount
	};

	// 应答的形状,由粘合代码从 redisReply 翻译过来(本文件不依赖 hiredis)。
	enum class ReplyShape : uint8_t
	{
		kNone,	  // 空应答:连接在应答前断了,结果未知
		kInteger, // 整数应答
		kError,	  // ERROR 应答:Redis 明确没执行(脚本被拒 / 只读副本 / 正在加载数据集)
		kOther,	  // 其它类型(不该出现)

		kCount
	};

	constexpr MarkWriteResult ClassifyMarkWriteReply(ReplyShape shape, int64_t value)
	{
		if (shape != ReplyShape::kInteger)
		{
			return MarkWriteResult::kFailed;
		}
		switch (value)
		{
		case 1:
			return MarkWriteResult::kWritten;
		case 0:
			return MarkWriteResult::kEpochMoved;
		case -2:
			return MarkWriteResult::kSuperseded;
		default:
			return MarkWriteResult::kFailed;
		}
	}

	// 运行期开关的取值(M14):nullptr / 空串 / 不认识的值 → defaultValue;"0" / "false" / "off" → false;
	// "1" / "true" / "on" → true。调用方(player_lifecycle.cpp)在进程内第一次用到时读一次环境变量并打日志。
	inline bool ParseSwitch(const char *value, bool defaultValue)
	{
		if (value == nullptr || value[0] == '\0')
		{
			return defaultValue;
		}
		if (std::strcmp(value, "0") == 0 || std::strcmp(value, "false") == 0 || std::strcmp(value, "off") == 0)
		{
			return false;
		}
		if (std::strcmp(value, "1") == 0 || std::strcmp(value, "true") == 0 || std::strcmp(value, "on") == 0)
		{
			return true;
		}
		return defaultValue;
	}

	// 环境变量名。
	//   kEnvExitReleaseMark      A1′ 总开关,默认开。回滚步骤:先把它置 0 滚动重启 → 等 ≥ kHandoffMarkTtlSec(300s)
	//                            → 再回滚二进制(旧版没有 A2′,新版写下的标记 300s 内对旧版进程上的同代持有有效)。
	//                            路由链(gate / scene_manager)新旧混跑、PlayerEnterGameNodeRequest.owner_epoch 为 0
	//                            的窗口不需要关它:A2′ 对这种路由改走 kLuaInheritClearUnknownEpoch 照样清(见上)。
	//                            该窗口的代价只剩活性:删掉当前代际后载入又被放弃时不补写(不知道 N,见 M7)。
	//   kEnvDevUnsafeCrossNode   dev 旁路开关,默认关。必须与 scene_manager 的 AllowUnsafeCrossNodeHandoff 同步打开:
	//                            旁路下 ReleasePlayer 之后的落点不铸造,A1′ 的 owner_epoch 条件拦不住,
	//                            开着时"退出中又收到 ReleasePlayer"会粘性压制 A1′(M6)。
	inline constexpr const char *kEnvExitReleaseMark = "SCENE_EXIT_RELEASE_MARK";
	inline constexpr const char *kEnvDevUnsafeCrossNode = "SCENE_DEV_UNSAFE_CROSS_NODE_HANDOFF";

	// ── A2′:新载入前"先核归属再删"继承来的标记 ─────────────────────────────────

    // 统一登记已清旧标记。KEYS=owner/save_barrier/handoff；ARGV=E/原ctx的H。
    // 返回0=原资格仍有效；-1=原路由/资格已过时。历史分类码仍保留用于原回包统计。
    inline const std::string& kLuaInheritClear = handoff_sql_barrier::kConfirmFenceLua;
    inline constexpr const char *kLuaInheritClearUnknownEpoch = "return -1";

	// A2′ 一次 EVAL 的结果。前五种是"核对通过"(闸门可放行),kEpochMismatch 是"核对不过"(拒建实体、不补发),
	// 最后两种是失败(有上限重发)。
	enum class InheritClearResult : uint8_t
	{
		kAbsent,		   // 0
		kDeletedOlder,	   // 1
		kDeletedCurrent,   // 2
		kKeptNewer,		   // 3
		kDeletedMalformed, // 4
		kEpochMismatch,	   // -1
		kReplyError,	   // ERROR 应答 / 非整数 / 不认识的整数
		kReplyLost,		   // 空应答:结果未知(可能已经删了)

		kCount
	};

	constexpr std::size_t kInheritClearResultCount = static_cast<std::size_t>(InheritClearResult::kCount);

	inline constexpr const char *kInheritClearResultNames[] = {
		"inherit_absent",
		"inherit_deleted_older",
		"inherit_deleted_exact",  // 与 §12.6.3 配套清单 / 验证步骤 6 的名字一致
		"inherit_kept_newer",
		"inherit_deleted_malformed",
		"inherit_epoch_mismatch",
		"inherit_reply_error",
		"inherit_reply_lost",
	};
	static_assert(std::size(kInheritClearResultNames) == kInheritClearResultCount,
				  "kInheritClearResultNames 必须与 InheritClearResult 一一对应");

	constexpr const char *InheritClearResultName(InheritClearResult result)
	{
		const auto index = static_cast<std::size_t>(result);
		return index < kInheritClearResultCount ? kInheritClearResultNames[index] : "?";
	}

	constexpr InheritClearResult ClassifyInheritClearReply(ReplyShape shape, int64_t value)
	{
		if (shape == ReplyShape::kNone)
		{
			return InheritClearResult::kReplyLost;
		}
		if (shape != ReplyShape::kInteger)
		{
			return InheritClearResult::kReplyError;
		}
		switch (value)
		{
		case -1:
			return InheritClearResult::kEpochMismatch;
		case 0:
			return InheritClearResult::kAbsent;
		case 1:
			return InheritClearResult::kDeletedOlder;
		case 2:
			return InheritClearResult::kDeletedCurrent;
		case 3:
			return InheritClearResult::kKeptNewer;
		case 4:
			return InheritClearResult::kDeletedMalformed;
		default:
			return InheritClearResult::kReplyError;
		}
	}

	// 核对通过(本 epoch 的这一次 A2′ 返回了非 -1 的整数):闸门据此放行。
	constexpr bool IsInheritClearConfirmed(InheritClearResult result)
	{
		return result == InheritClearResult::kAbsent || result == InheritClearResult::kDeletedOlder ||
			   result == InheritClearResult::kDeletedCurrent || result == InheritClearResult::kKeptNewer ||
			   result == InheritClearResult::kDeletedMalformed;
	}

	constexpr bool IsInheritClearFailure(InheritClearResult result)
	{
		return result == InheritClearResult::kReplyError || result == InheritClearResult::kReplyLost;
	}

	// 这次 EVAL 可能删掉了 epoch == N 的那一份(M7 补写的依据):确定删了,或应答丢失、结果未知。
	// ERROR 应答 = Redis 明确没执行,不算。
	constexpr bool MayHaveDeletedCurrent(InheritClearResult result)
	{
		return result == InheritClearResult::kDeletedCurrent || result == InheritClearResult::kReplyLost;
	}

	// 已放弃那一轮载入的 A2′ 晚到应答删掉了(或可能删掉了)epoch == N 的标记时怎么补写(M7 + 复审 ownership-major)。
	// 同节点重登不铸造,更新的一轮与已放弃的那一轮同为 epoch N,补写的 owner_epoch == N 条件拦不住;更新一轮的
	// A2′ 若排在已放弃那一轮的 EVAL 之后,它看不到标记就放行建实体,此刻当场补写等于给活持有者发放行证(回档)。
	enum class AbandonedRewriteDecision : uint8_t
	{
		kRewriteNow,		// 本节点既没有该玩家实体、也没有更新一轮的载入:当场条件补写
		kHandToNewerLoad,	// 有更新一轮的待入场条目:把补写责任交给它(建出实体即作废,也放弃时由它补写)
		kDropNodeHolds,		// 本节点已有该玩家实体 = 本节点就是持有者:丢弃补写(少一枚标记只会让一次重登回 18)

		kCount
	};

	constexpr AbandonedRewriteDecision DecideAbandonedRewrite(bool nodeHoldsPlayer, bool newerLoadPending)
	{
		if (nodeHoldsPlayer)
		{
			return AbandonedRewriteDecision::kDropNodeHolds;
		}
		return newerLoadPending ? AbandonedRewriteDecision::kHandToNewerLoad : AbandonedRewriteDecision::kRewriteNow;
	}

	// 一次载入生命周期里 A2′ 的进度(只描述"针对 targetEpoch 的那一次",targetEpoch 变了就重新开始)。
	enum class InheritClearPhase : uint8_t
	{
		kNone,		// 没发过(还没来得及发)
		kInFlight,	// EVAL 已发出、应答未到
		kRetryWait, // 上一次失败,等下一次重发
		kConfirmed, // 本 targetEpoch 的这一次返回了非 -1

		kCount
	};

	// 载入完成(HandlePlayerAsyncLoaded)时建不建实体(M10 三态)。
	enum class InheritGateDecision : uint8_t
	{
		kProceed, // 建实体
		kWait,	  // 暂存载入结果,等 A2′ 应答 / 重发
		kRefuse,  // 拒建实体、回 kEnterSceneFailed

		kCount
	};

	// ctxEpoch    这次待入场上下文带来的 owner_epoch(0 = 旧版路由,A2′ 改用 kLuaInheritClearUnknownEpoch)
	// targetEpoch A2′ 当前针对的 epoch(0 = 针对"路由不带 owner_epoch"的那一种清理)
	// phase       A2′ 针对 targetEpoch 的进度
	// 只认"本 ctx.ownerEpoch 的那一次":targetEpoch ≠ ctxEpoch 时不能拿别的 epoch 的确认冒充(M2 / M10)。
	// ctxEpoch == 0 不再直接放行(复审 ownership-major):同样要等针对 0 的那一次清理确认,没发过就拒。
	// 核对不过(-1)与重发耗尽不在这里判:它们在应答 / 定时器里当场拒绝,待入场条目随之擦除,走不到闸门。
	constexpr InheritGateDecision DecideInheritGate(uint64_t ctxEpoch, uint64_t targetEpoch, InheritClearPhase phase)
	{
		if (targetEpoch != ctxEpoch)
		{
			return InheritGateDecision::kRefuse; // 没有针对这个 epoch 发过 A2′:fail-closed
		}
		switch (phase)
		{
		case InheritClearPhase::kConfirmed:
			return InheritGateDecision::kProceed;
		case InheritClearPhase::kInFlight:
		case InheritClearPhase::kRetryWait:
			return InheritGateDecision::kWait;
		default:
			return InheritGateDecision::kRefuse;
		}
	}

	// 失败重发的上限(M10):总共最多 kInheritClearMaxAttempts 次(首发 + 3 次重发),且从首发起不超过
	// kInheritClearDeadline(单调时钟)。在途超过截止时刻同样放弃(黑洞连接不会回空应答)。
	// 重发由 RedisSystem 的 1s 定时器 / 重连回调驱动,实际间隔按 1s 取整。
	using Clock = std::chrono::steady_clock;
	inline constexpr uint32_t kInheritClearMaxAttempts = 4;
	inline constexpr std::chrono::milliseconds kInheritClearDeadline{5000};

	// 第 attemptsSent 次失败后到下一次重发的最短间隔:250ms → 500ms → 1000ms(封顶)。
	constexpr std::chrono::milliseconds InheritClearRetryDelay(uint32_t attemptsSent)
	{
		const uint32_t shift = attemptsSent == 0 ? 0 : (attemptsSent - 1 > 2 ? 2 : attemptsSent - 1);
		return std::chrono::milliseconds(250 << shift);
	}

	constexpr bool IsInheritClearExhausted(uint32_t attemptsSent, Clock::duration elapsedSinceFirstSend)
	{
		return attemptsSent >= kInheritClearMaxAttempts || elapsedSinceFirstSend >= kInheritClearDeadline;
	}
} // namespace exit_release_mark
