#pragma once

// 战斗结算**发件箱**的纯判定逻辑(docs/design/routing-identity-audit-20260908.md R07)。
//
// 缺陷原状:battle 结算时向 `SceneCommand{target_instance_id=CreateBattle 时抓的 scene uuid}`
// 发一次就不管了。那个 scene 进程如果在战斗期间重启,继任者按代次校验丢弃命令并提交 offset,
// **没有任何重投**;而"待结算"的 Redis 记录一直以来是由 *scene* 写的
// (player_battle.cpp ApplySettlement 的离线分支)—— 那个 scene 从来没跑过这段代码。
// 净结果:奖励永久丢失,玩家冻结到 deadline+60s 被 reaper 解开。
//
// 修法(审计推荐的 ①「落库再投递」):
//   1. battle 先把结算写进跨进程存储(shared Redis 的 battle:settlement:pending:{player_id}
//      + 伴生 battle:settlement:pending:id:{player_id} = battle_id),**写成功之后**才投递命令。
//      这条记录本来就有消费者:scene 的 OnPlayerEnterScene 登录钩子会读它并补应用。
//      也就是说即使投递整条丢失,奖励最终仍会在玩家下次进场景时落地。
//   2. scene 应用结算后**条件销账**(按 battle_id 匹配才删),销账即 ACK。
//   3. 未销账的由 battle 有界重投,重投前**重新解析**目标(player:{id}:location),
//      不复用开局时抓的那个 scene uuid —— 那个 uuid 恰恰是过期的那一个。
//   4. 次数用尽响一声(结构化 metric),记录留给登录补应用兜底。
//
// 2026-10-14 起待结算记录**不再过期**(docs/design/landing-recovery-sync-digest.md §7,不变量 L-6):
// 原来的 7 天 TTL 到期由 Redis 静默删除,奖励丢失且没有任何信号。现在记录一直保留到应用并销账为止,
// 另有积压索引 kPendingSettlementIndexKey 让 battle 节点定期报积压数与最老年龄。
//
// 这个头只放**判定与契约**:纯函数 + Redis key / Lua 常量、零项目依赖,能被单测直接盯住
// (与 node_kafka_command_filter.h 拆出来的理由一样 —— 否则要测就得把 Redis、Kafka、muduo loop
// 整条链拖进测试工程)。所有读写待结算记录的 Lua 都集中在这里:「写入必 ZADD 索引、销账必 ZREM」
// 这条规则只有放在一处才审得过来。

#include <charconv>
#include <cstdint>
#include <string_view>
#include <system_error>

namespace battle_settlement
{
	enum class RetryAction
	{
		// 持久记录已不指向本局(scene 销账了,或玩家已经进了下一场):投递成功,摘除条目。
		kDone,
		// 记录仍在、且解析出了玩家当前所在的 scene node_id:按**重新解析出的**目标重投。
		kResend,
		// 记录仍在,但解析不到玩家当前位置(离线 / 还没落进任何场景):
		// 本轮不投。硬要投也没有目标,而持久记录会被登录钩子兜住。
		kSkipNoTarget,
		// 重试次数用尽:摘除条目并打一条**响亮**的结构化日志。
		// 注意这不等于结算丢失 —— 持久记录仍在(不再过期),玩家下次进场景会补应用;
		// 在线玩家战斗冻结到期时 scene reaper 也会补读一次。
		kExhausted,
	};

	struct RetryInput
	{
		// battle:settlement:pending:id:{player_id} 读回来是否仍等于本局 battle_id。
		// false 有两种来源,对 battle 来说是同一件事(不必再投):scene 已销账,
		// 或者玩家已经打完了下一场并被那一场覆盖。
		bool pendingRecordStillOurs = false;
		// player:{id}:location 是否解析出了一个可用(非 0)的 scene node_id。
		bool locationResolved = false;
		// 已经消耗掉的重投轮次(本轮之前)。
		uint32_t attemptsSoFar = 0;
		uint32_t maxAttempts = 0;
	};

	// 判定顺序不能换:
	//   销账 > 次数用尽 > 无目标 > 重投。
	// 「销账」必须排在「次数用尽」之前 —— 最后一轮恰好读到已销账时应当算成功收尾,
	// 而不是打一条"未送达"的假警报。
	inline RetryAction ClassifyRetry(const RetryInput &input)
	{
		if (!input.pendingRecordStillOurs)
		{
			return RetryAction::kDone;
		}
		if (input.attemptsSoFar >= input.maxAttempts)
		{
			return RetryAction::kExhausted;
		}
		if (!input.locationResolved)
		{
			return RetryAction::kSkipNoTarget;
		}
		return RetryAction::kResend;
	}

	inline const char *RetryActionName(const RetryAction action)
	{
		switch (action)
		{
		case RetryAction::kDone:
			return "done";
		case RetryAction::kResend:
			return "resend";
		case RetryAction::kSkipNoTarget:
			return "skip_no_target";
		case RetryAction::kExhausted:
			return "exhausted";
		}
		return "unknown";
	}

	// 次数用尽时的分类(metric=battle_settlement_undelivered 的 reason 字段),便于分开设阈值:
	//   kNoTarget —— 最后一轮解析不到玩家位置(离线 / 还没落进任何场景)。离线玩家 120 秒后
	//                必然走到这里,是预期内的;记录留给登录钩子。
	//   kNotAcked —— 最后一轮解析得到位置、也重投了,scene 仍没销账。玩家在线却收不到奖励,是异常。
	// 只看**最后一轮**而不看「窗口内是否有过目标」:玩家打完立刻下线时,早期几轮还能读到退出中的位置
	// (scene 视同离线、只暂存不销账),按「有过目标」会把这类预期内的情形误报成 not_acked。
	enum class UndeliveredReason
	{
		kNoTarget,
		kNotAcked,
		// 这一条记录累计「没问到」(空应答 / ERROR)的轮次到上限(ClassifyUndeterminedProbeRound;累计不清零,
		// 问到的轮次另有 attempts 上限,所以条目寿命两头都有界)。不经 ClassifyUndelivered,
		// 由调用方直接给出:这类轮次根本没走到 ClassifyRetry。
		kProbeUndetermined,
	};

	inline UndeliveredReason ClassifyUndelivered(const RetryInput &exhaustedRound)
	{
		return exhaustedRound.locationResolved ? UndeliveredReason::kNotAcked : UndeliveredReason::kNoTarget;
	}

	inline const char *UndeliveredReasonName(const UndeliveredReason reason)
	{
		switch (reason)
		{
		case UndeliveredReason::kNoTarget:
			return "no_target";
		case UndeliveredReason::kNotAcked:
			return "not_acked";
		case UndeliveredReason::kProbeUndetermined:
			return "probe_undetermined";
		}
		return "unknown";
	}

	// ---- zone Redis 应答的形状:空应答 / ERROR 不是业务结论(2026-10-16) ----
	//
	// hiredis 在连接断开或被释放时,把所有在途命令的回调以空应答(redisReply* == nullptr)调用一次。
	// Redis 主切换时代理掐连接、存活探测 15 秒没有应答就摘连接(thread_context/redis_manager.h「存活契约」)、
	// 停机 Shutdown 都会触发。空应答的意思是「这次没问到」:命令可能执行了,也可能没执行。
	// 它不是 NIL(键不存在),更不是「已销账」「没有挂起结算」「没有锁」这类结论。
	// ERROR 应答也一样:读命令收到 ERROR 什么都没告诉我们;写脚本收到 ERROR 时前半段可能已经执行
	// (Redis 不回滚 Lua),同样不能断定「没写进去」。
	// 下面的判定函数只认形状,不碰 hiredis 类型;从 redisReply 换算成形状的唯一适配在 settlement_reply_shape.h。
	enum class ReplyShape : uint8_t
	{
		kNoReply, // 空应答:连接断开 / 被释放 / 停机
		kNil,
		kString,
		kStatus,
		kInteger,
		kArray,
		kError,
		kOther, // RESP3 的 double / bool / map 等,本仓的调用点都不该收到
	};

	inline const char *ReplyShapeName(const ReplyShape shape)
	{
		switch (shape)
		{
		case ReplyShape::kNoReply:
			return "no_reply";
		case ReplyShape::kNil:
			return "nil";
		case ReplyShape::kString:
			return "string";
		case ReplyShape::kStatus:
			return "status";
		case ReplyShape::kInteger:
			return "integer";
		case ReplyShape::kArray:
			return "array";
		case ReplyShape::kError:
			return "error";
		case ReplyShape::kOther:
			return "other";
		}
		return "unknown";
	}

	// 发件箱第一跳:读伴生 id 键(GET battle:settlement:pending:id:{player_id})探测销账。
	enum class AckProbeVerdict : uint8_t
	{
		// NIL:scene 已销账,或者记录从来没落库(落库应答没确认时也会登记进发件箱,见 ClassifyPersistReply);
		// STRING 但不等于本局:玩家已经打完下一场,单槽被那一局覆盖。两者对 battle 是同一件事:不必再投。
		kAcknowledged,
		// STRING 且等于本局:还没销账,进第二跳。
		kStillOurs,
		// 空应答 / ERROR / 意外类型:本轮什么都没问到。调用方本轮直接返回,不摘条目、不消耗次数 ——
		// 与 RetryPendingSettlements「探测不了销账就不能判定」同一原则。
		// 以前这里把空应答当成「已销账」并删条目:一次断线就让在线玩家收不到重投,只能等下次登录。
		kUndetermined,
	};

	// pendingIdValue 只在 shape == kString 时有意义。比较按十进制字符串逐字节(伴生 id 键只由本头的脚本以 %llu 写入)。
	inline AckProbeVerdict ClassifyAckProbe(const ReplyShape shape, const std::string_view pendingIdValue,
											const uint64_t battleId)
	{
		switch (shape)
		{
		case ReplyShape::kNil:
			return AckProbeVerdict::kAcknowledged;
		case ReplyShape::kString:
		{
			char ours[20]; // uint64 十进制最多 20 位,to_chars 不会失败
			const auto formatted = std::to_chars(ours, ours + sizeof(ours), battleId);
			const std::string_view oursView(ours, static_cast<size_t>(formatted.ptr - ours));
			return pendingIdValue == oursView ? AckProbeVerdict::kStillOurs : AckProbeVerdict::kAcknowledged;
		}
		default:
			return AckProbeVerdict::kUndetermined;
		}
	}

	inline const char *AckProbeVerdictName(const AckProbeVerdict verdict)
	{
		switch (verdict)
		{
		case AckProbeVerdict::kAcknowledged:
			return "acknowledged";
		case AckProbeVerdict::kStillOurs:
			return "still_ours";
		case AckProbeVerdict::kUndetermined:
			return "undetermined";
		}
		return "unknown";
	}

	// 发件箱第二跳:读 player:{id}:location 的应答能不能拿来下「有没有目标」的结论。
	// 只有 NIL(玩家不在任何场景)与 STRING(有位置记录,内容由调用方解析)算问到了;
	// STRING 解析不出 node_id 仍按「无目标」处理(数据问题,不是没问到)。
	// 空应答 / ERROR / 意外类型返回 false:调用方在 ClassifyRetry 之前返回,不消耗次数。
	// 以前这里走 kSkipNoTarget 并 ++attempts:一次断线白烧一轮重投,耗尽时还会误记成 reason=no_target。
	inline bool IsLocationReplyDecisive(const ReplyShape shape)
	{
		return shape == ReplyShape::kNil || shape == ReplyShape::kString;
	}

	// 「没问到」的轮次不消耗重投次数,但单独计数、单独设上限(AGENTS §11.3 重试必须有上限):
	// 持续 ERROR 时(口令轮换后 NOAUTH、ACL 拒绝、长时间 LOADING)不设上限的话,每局结算都进发件箱、永远到不了
	// 次数用尽,payload 随对局数无界增长,也永远不响 undelivered。摘条目不丢数据:记录若已落库仍在,登录钩子兜底。
	enum class UndeterminedProbeAction : uint8_t
	{
		kWaitNextRound,
		kGiveUp, // 摘条目,metric=battle_settlement_undelivered reason=probe_undetermined
	};

	// undeterminedRounds 是这一条目寿命内的累计轮数(含本轮,不因问到而清零)。maxRounds 为 0 是调用方缺陷:按立即放弃处理,保证有界。
	inline UndeterminedProbeAction ClassifyUndeterminedProbeRound(const uint32_t undeterminedRounds,
																   const uint32_t maxRounds)
	{
		return undeterminedRounds >= maxRounds ? UndeterminedProbeAction::kGiveUp
											   : UndeterminedProbeAction::kWaitNextRound;
	}

	// 「先落库、后投递」的写入(battle 结算的 kSetPendingSettlementScript、活动局结果的 SET)回来之后的结论。
	enum class PersistVerdict : uint8_t
	{
		// 应答就是这条命令成功时的形状(EVAL 脚本 return 1 → 整数;SET → 状态 OK)。
		kStored,
		// 空应答 / ERROR / 意外形状:记录可能落了,也可能没落(空应答时命令可能已执行;脚本 ERROR 时前半段可能已执行)。
		kUnconfirmed,
	};

	// 两种结论的后续动作**相同**:投递一次 + 登记发件箱;kUnconfirmed 只多打一条 ..._not_durable。
	// 登记是安全的:发件箱每轮先探测记录(结算 GET 伴生 id 键、活动局 EXISTS),记录不在就按已送达摘掉,
	// 记录在就按原字节重投。以前空应答 / ERROR 时只投一次、不登记:记录其实落了而投递又丢了的话,
	// 在线玩家要等到下次登录才拿到奖励。
	inline PersistVerdict ClassifyPersistReply(const ReplyShape actual, const ReplyShape expectedOnSuccess)
	{
		return actual == expectedOnSuccess && actual != ReplyShape::kNoReply ? PersistVerdict::kStored
																			  : PersistVerdict::kUnconfirmed;
	}

	// 确认模式(全球对局)发结果之前的个人结算落库(kStagePendingSettlementOnceScript)回来之后的结论
	// (审计 r2-battle#1,2026-10-19)。前三种是「该成员已落定」,ResultCommit 把他移出待写集合、后续轮次不再写;
	// 只有 kUnconfirmed 下一轮重写。
	enum class StageVerdict : uint8_t
	{
		kStored,        // 脚本返回 1:本次写入
		kAlreadyStored, // 脚本返回 2:本局早先已为该成员写过 —— 上一轮的应答丢了或报了 ERROR,命令其实执行了。不重写
		// 脚本返回 3:锁属于别的局,或锁不在而 pending 槽属于别的局(2026-10-22 评审补),这一局对该成员已被取代。
		// 不写,免得旧局覆盖新局的单槽 pending
		kSuperseded,
		// 空应答 / ERROR / 意外形状或取值:不知道写没写。下一轮重写是安全的:命令若其实执行过,脚本会返回 2。
		kUnconfirmed,
	};

	// integer 只在 shape == kInteger 时有意义。
	inline StageVerdict ClassifyStageReply(const ReplyShape shape, const int64_t integer)
	{
		if (shape != ReplyShape::kInteger)
		{
			return StageVerdict::kUnconfirmed;
		}
		switch (integer)
		{
		case 1:
			return StageVerdict::kStored;
		case 2:
			return StageVerdict::kAlreadyStored;
		case 3:
			return StageVerdict::kSuperseded;
		default:
			return StageVerdict::kUnconfirmed;
		}
	}

	inline bool IsStageSettled(const StageVerdict verdict)
	{
		return verdict != StageVerdict::kUnconfirmed;
	}

	inline const char *StageVerdictName(const StageVerdict verdict)
	{
		switch (verdict)
		{
		case StageVerdict::kStored:
			return "stored";
		case StageVerdict::kAlreadyStored:
			return "already_stored";
		case StageVerdict::kSuperseded:
			return "superseded";
		case StageVerdict::kUnconfirmed:
			return "unconfirmed";
		}
		return "unknown";
	}

	// ---- scene 登录钩子对待结算记录与战斗锁的读数判定 ----
	//
	// OnPlayerEnterScene 先 GET 待结算记录,没有才读 battle:lock + ctx 重建冻结(player_battle.cpp)。
	// 两步都要分清「没有」与「没问到」:没问到时把玩家记入登录复查,由 scene reaper 的节拍重跑整个第一步,
	// 复查完成前不按锁重建冻结(重建依赖「确实没有挂起结算」这个前提)。
	enum class LoginPendingRead : uint8_t
	{
		kNoPending, // NIL:没有挂起结算,接着读锁
		kPending,   // STRING:有挂起结算,解析后补应用
		// 空应答 / ERROR / 意外类型:没问到。以前空应答与 NIL 走同一分支,当成「没有挂起结算」去按锁重建,
		// 这一次登录就漏掉了补发奖励(记录不过期,要等下一次登录;玩家一直在线就一直拿不到)。
		// ERROR 也进复查(与下面读锁时「ERROR 按无锁」相反):读不到挂起结算就不知道锁指向的那一局结束没有,
		// 锁要等落盘销账才删,按锁重建可能把玩家冻进一场已经结算、奖励还躺在记录里的战斗 —— 与登录钩子读到坏记录时
		// 刻意不按锁重建是同一个理由。代价:GET 持续 ERROR(只可能是键被改坏或鉴权异常)时每 30 秒复查一次,
		// 冻结一直不重建。
		kRecheck,
	};

	inline LoginPendingRead ClassifyLoginPendingRead(const ReplyShape shape)
	{
		switch (shape)
		{
		case ReplyShape::kNil:
			return LoginPendingRead::kNoPending;
		case ReplyShape::kString:
			return LoginPendingRead::kPending;
		default:
			return LoginPendingRead::kRecheck;
		}
	}

	// 登录重建读锁(player_battle.cpp 的 kGetLockAndCtxScript:锁不在返回 nil,在则返回数组)。
	enum class LoginLockRead : uint8_t
	{
		kNoLock,  // NIL,以及 ERROR / 意外类型(见下)
		kLock,    // 数组:{锁值, ctx 或 nil, TTL}
		kRecheck, // 空应答:没问到,记入登录复查
	};

	// 只有空应答进复查。ERROR 按「无锁」是既有取舍,这里保留:锁是咨询性的(权威是 InBattleComp),
	// EVAL 收到 ERROR 是服务端答复了但拒绝执行;把它也改成复查会让脚本层面的持续性错误变成每 30 秒一次的无尽重试。
	// 代价:ERROR 那一次登录不重建冻结,战斗闸(资产通道、收件箱 requiresOutOfBattle)在战斗剩余时间里看不到这场战斗,
	// 直到结算到达(按锁匹配应用)或玩家下次进场。空应答不同:它只说明连接断了,断线恢复后一定问得到,值得重试。
	inline LoginLockRead ClassifyLoginLockRead(const ReplyShape shape)
	{
		switch (shape)
		{
		case ReplyShape::kArray:
			return LoginLockRead::kLock;
		case ReplyShape::kNoReply:
			return LoginLockRead::kRecheck;
		default:
			return LoginLockRead::kNoLock;
		}
	}

	// 按锁重建冻结之前的裁决(2026-10-19,第二、三轮评审 P3)。登录重建 RestoreBattleFreezeOnLogin 与
	// 迟到确认重建 RebuildBattleFreezeFromLock 共用。
	enum class LockRebuildDecision : uint8_t
	{
		// 锁指向一场还没结算、也没被本实体判作废的战斗:按锁 + ctx 重建冻结。
		kRebuild,
		// 锁指向的这一局已在活账本里(已应用、还没落盘销账):不重建,改为推进销账。
		kAdvanceAck,
		// 锁指向本实体上 reaper 已按期限判作废(FIGHTING 到期)的那一局:不重建,补一次条件删锁。
		// 只有删锁没落地(Redis 抖)才会再读到这把锁。重建会把玩家冻进已结束的战斗(最长约一个 reaper 节拍)、
		// 推一次重连提示让客户端弹「战斗已结束」,还把锁续到「现在 + 60 秒」;删锁一直失败时这个回路没有硬上限。
		// 读到它的路径不止一条:冻结到期补读没读到挂的复查、补应用另一局挂起结算之后的按锁重建、
		// 登录挂的复查恰好赶上冻结到期 —— 守卫放在重建入口,一处盖住所有路径。
		kSkipExpired,
	};

	// 顺序不能换:已应用 > 已作废 > 重建。
	// 已应用必须排在已作废前面:作废之后删锁若没落地,迟到的结算会按锁匹配应用(ApplySettlement 的在线无组件分支),
	// 而锁要留到这一局落盘销账才删(锁是「这一局还没完」的唯一跨进程证据)—— 这时补删锁会提前放开排队。
	// expiredBattleId 为 0 = 本实体没有被判作废的局:这时不论锁值是什么(锁值也是 0 时同样)都不判作废。
	inline LockRebuildDecision ClassifyLockRebuild(const uint64_t lockBattleId, const uint64_t expiredBattleId,
												   const bool appliedInLedger)
	{
		if (appliedInLedger)
		{
			return LockRebuildDecision::kAdvanceAck;
		}
		if (expiredBattleId != 0 && lockBattleId == expiredBattleId)
		{
			return LockRebuildDecision::kSkipExpired;
		}
		return LockRebuildDecision::kRebuild;
	}

	// reaper 节拍对一个复查标记怎么处理。
	struct LoginRecheckEntryState
	{
		bool exitingOrNoIdentity = false; // 退出中(UnregisterPlayer)或没有 player_id
		bool travelHandoffOrFrozen = false; // 跨 zone 冻结 / 交接中(结算此刻不可应用)
		bool redisReady = false;
	};

	enum class LoginRecheckStep : uint8_t
	{
		kRun,  // 摘标记,按标记里的范围重跑;又没问到会重新挂上
		kKeep, // 标记保留,本轮不跑
		kDrop, // 摘标记,不重跑
	};

	// 顺序不能换:退出中 > 交接中 > Redis 不可用 > 重跑。
	//  - 退出中视同离线:挂起结算留在 Redis,下次进场登录钩子重跑,与冻结到期补读跳过 UnregisterPlayer 同一口径;
	//  - 交接中保留:这时重跑会在即将销毁的源实体上按锁重建冻结、推重连提示;交接完成随实体销毁,撤销则下个节拍再跑;
	//  - Redis 不可用保留:恢复后的第一个节拍再跑。
	inline LoginRecheckStep ClassifyLoginRecheckEntry(const LoginRecheckEntryState &state)
	{
		if (state.exitingOrNoIdentity)
		{
			return LoginRecheckStep::kDrop;
		}
		if (state.travelHandoffOrFrozen || !state.redisReady)
		{
			return LoginRecheckStep::kKeep;
		}
		return LoginRecheckStep::kRun;
	}

	// ---- 积压报数的纯计算 ----

	// 最老一条待结算记录的年龄(秒)。oldestEnqueuedAtMs 是索引里的分数(写入方的 Unix 毫秒);
	// 0 = 索引为空。分数晚于 nowMs(写入方与报数方墙钟有偏差)时按 0 处理,不报负数也不回绕。
	inline uint64_t PendingBacklogOldestAgeSec(const uint64_t oldestEnqueuedAtMs, const uint64_t nowMs)
	{
		if (oldestEnqueuedAtMs == 0 || oldestEnqueuedAtMs >= nowMs)
		{
			return 0;
		}
		return (nowMs - oldestEnqueuedAtMs) / 1000;
	}

	// 孤儿清理的扫描窗口游标(按排名,0 = 最老)。
	//
	// 设计 §7 原文是「每轮查最老的 10 条」。只查头部有一个会卡死的情形:记录不再过期,打完就再也不上线的
	// 玩家会让有效记录永久占住索引头部;头部一旦有 ≥ 一批有效记录,排在后面的孤儿就永远查不到,
	// count 永久偏大。所以两种窗口交替:
	//   偶数轮 —— 查头部 [0, batch):头部孤儿最多两轮就清掉,oldest_age_s 不会被它们抬高(设计的原意);
	//   奇数轮 —— 查轮转窗口 [rotatingStart, rotatingStart + batch):逐轮后移,扫到底绕回 0,
	//             保证每个孤儿最终都会被查到。清理会让后面的排名前移,一趟里可能跳过几条,下一趟补上。
	// 游标只在 ZRANGE 的回复到达时推进(AdvancePendingIndexSweep):回复没来,下一轮重查同一个窗口。
	struct PendingIndexSweepCursor
	{
		uint64_t round = 0;
		uint64_t rotatingStart = 0;
	};

	// 本轮窗口的起始排名。
	inline uint64_t PendingIndexSweepStart(const PendingIndexSweepCursor &cursor)
	{
		return cursor.round % 2 == 0 ? 0 : cursor.rotatingStart;
	}

	// 本轮 ZRANGE 返回 returned 个成员之后推进游标。奇数轮满批则轮转窗口后移一批,不满(扫到底)则绕回 0。
	// batch 为 0 是调用方缺陷:游标不动,避免原地空转把轮转窗口钉死在一处。
	inline void AdvancePendingIndexSweep(PendingIndexSweepCursor &cursor, const uint64_t returned,
										 const uint32_t batch)
	{
		if (batch == 0)
		{
			return;
		}
		if (cursor.round % 2 != 0)
		{
			cursor.rotatingStart = returned < batch ? 0 : cursor.rotatingStart + batch;
		}
		++cursor.round;
	}

	// 解析积压索引的成员(十进制 player_id)。整串都是十进制且 > 0 才返回 true;
	// 空串、符号、空白、溢出一律 false。成员只由本头的写入脚本以 %llu 写入,解析失败说明索引被手工改过。
	inline bool ParsePendingIndexMember(const std::string_view member, uint64_t &playerId)
	{
		if (member.empty())
		{
			return false;
		}
		uint64_t parsed = 0;
		const char *end = member.data() + member.size();
		const auto [ptr, ec] = std::from_chars(member.data(), end, parsed, 10);
		if (ec != std::errc{} || ptr != end || parsed == 0)
		{
			return false;
		}
		playerId = parsed;
		return true;
	}

	// ---- Redis key 契约(battle 写、scene 读与销账,两端都引用本头,不得各抄一份)----
	//
	// 值 = 序列化的 BattleSettlementEvent;伴生 id 键 = 十进制 battle_id。
	// 两个键**同 SET / 同 DEL**,与既有的 battle:lock + battle:ctx 同一形态:
	// 单靠 blob 无法在 Lua 里判断它属于哪一局(Lua 不会解 protobuf),伴生 id 键就是
	// 「条件销账」与「ACK 探测」共同的那把尺子。
	// 两键都**不带 TTL**(landing-recovery §7):SET 不带 EX 会顺带清掉旧版本写入时留下的 TTL。
	// 每玩家单槽:同一时间每个玩家最多一局待结算,靠战斗锁一直保留到落盘销账来保证,下一局的写入覆盖上一局。
	// 内存上界 = 有待结算的玩家数 × 一份结算 blob。
	inline constexpr char kPendingSettlementKeyFmt[] = "battle:settlement:pending:%llu";
	inline constexpr char kPendingSettlementIdKeyFmt[] = "battle:settlement:pending:id:%llu";

	// 积压索引:ZSET,member = 十进制 player_id,score = 写入时刻(写入方 Unix 毫秒)。
	// 只用于报数(积压条数、最老年龄),不参与任何投递或销账判定 —— 索引丢了、多了、少了都不影响结算正确性。
	//
	// 滚动升级兼容(设计 §7):
	//   - 旧版 battle / scene 写入仍带 7 天 TTL、且不 ZADD:这批记录不进索引,7 天后照旧过期,行为与改造前相同;
	//   - 新版写入不过期、且 ZADD;旧版 scene 暂存时会给它重新挂上 TTL,到期后索引里留下孤儿;
	//   - 旧版 scene 销账不 ZREM,索引里也会留下孤儿。
	// 孤儿统一由 battle 节点的报数任务按 kSweepPendingIndexOrphanScript 清理(每轮查一批,窗口见
	// PendingIndexSweepCursor)。孤儿只让报数偏大,不影响任何结算。
	// 不与上面两个按玩家的键共用前缀格式:"index" 不是十进制,不会被 %llu 的键撞上。
	inline constexpr char kPendingSettlementIndexKey[] = "battle:settlement:pending:index";

	// battle 落库(两键不过期 + 进积压索引),一条 Lua 原子完成。
	// 只给本地(非确认)模式用:每名成员只写这一次、不重写。确认模式的分轮落库会重写结论未知的成员,
	// 改用下面的 kStagePendingSettlementOnceScript(审计 r2-battle#1)。
	// KEYS[1]=blob 键,KEYS[2]=id 键,KEYS[3]=积压索引;
	// ARGV[1]=blob,ARGV[2]=battle_id,ARGV[3]=now_ms(写入方 Unix 毫秒),ARGV[4]=player_id。
	// now 由调用方传入而不是在 Lua 里取 TIME:脚本保持确定性;各节点墙钟偏差是秒级,对以天计的年龄报数无影响。
	// ZADD 不带 NX:同一玩家再写一次(下一局覆盖单槽)就是一条新记录,年龄理应从这一刻重新算。
	inline constexpr char kSetPendingSettlementScript[] =
		"redis.call('SET', KEYS[1], ARGV[1]); "
		"redis.call('SET', KEYS[2], ARGV[2]); "
		"redis.call('ZADD', KEYS[3], ARGV[3], ARGV[4]); return 1";

	// scene 离线暂存:锁值 == battle_id 才写 pending 两键并进积压索引,一条 Lua 原子完成。
	// KEYS[1]=battle:lock,KEYS[2]=pending blob,KEYS[3]=pending:id,KEYS[4]=积压索引;
	// ARGV[1]=battle_id,ARGV[2]=blob,ARGV[3]=now_ms,ARGV[4]=player_id。
	// 返回 0 = 锁不在,1 = 已写入,2 = 锁属于别的局。
	// 为什么不能「先 GET 锁、回调里再 SET」:锁在结算应用后一直留到落盘销账,所以「看到锁 == 本局」
	// 不再证明本局还没应用。两步之间若插进一次销账(同一条 Lua 删 pending + 锁,可能来自另一个节点),
	// 随后的 SET 会把已销账的 pending 重新写回来 —— 销账回调随即从账本摘掉这一局,下次登录就会再发一次奖
	// (第二轮评审 w0zw729x1 #0)。合成一条脚本后,销账之后到达的暂存必然看到「锁不在」,什么都不写。
	// battle 已经写过同一份,这里是幂等覆盖;ZADD 会把年龄刷新到暂存时刻(通常在发件箱重投窗口 120 秒以内)。
	inline constexpr char kStorePendingSettlementIfLockMatchScript[] =
		"local v = redis.call('GET', KEYS[1]) "
		"if not v then return 0 end "
		"if v ~= ARGV[1] then return 2 end "
		"redis.call('SET', KEYS[2], ARGV[2]) "
		"redis.call('SET', KEYS[3], ARGV[1]) "
		"redis.call('ZADD', KEYS[4], ARGV[3], ARGV[4]) "
		"return 1";

	// 战斗锁键(scene PrepareBattle 写,值 = 十进制 battle_id,带 TTL)。确认模式落库读它判「已被别的局取代」。
	// scene 的 player_battle.cpp 引用本常量;player_team.cpp 组队跟随链里还有一份同字面量的本地常量(既有)。
	inline constexpr char kBattleLockKeyFmt[] = "battle:lock:%llu";

	// 确认模式落库标记:SET,member = 十进制 player_id,每局一个键(按 battle_id)。
	// 只表达「本局已经为这名成员写过 pending」:与 pending 两键在同一条脚本里写下,之后没有任何脚本摘成员
	// (销账脚本不碰它),所以销账之后它仍能证明「写过」。
	// 寿命:只在落库轮次之间有用。全部成员落定后 battle 立即 DEL(不会再有下一轮);停机、崩溃、落库判用尽时留下的
	// 随 TTL 过期。TTL 必须活过整个落库阶段:2026-10-22 起 ResultCommit 从第一次问起落库算起满 kMaxStagingSpan
	// (6 小时,单调时钟,与 Redis 连不连得上无关)即不再开轮,而标记的写入不早于这个起点,所以任何一轮开出时标记都还在。
	// 取 24 小时,留 4 倍余量;battle_room_manager.cpp 的 static_assert 与 turn_battle_engine_test 守住两者关系。
	// (原先只数轮次、没有墙钟上限,battle 节点单边连不上 Redis 超过 24 小时时标记会先过期,已随跨度上限堵上。)
	// 前缀与按玩家的 pending 两键、积压索引都不同,不会被它们的格式撞上。
	inline constexpr char kStagedSettlementMembersKeyFmt[] = "battle:settlement:staged:%llu";
	inline constexpr uint32_t kStagedSettlementMembersTtlSec = 86400;

	// 确认模式(全球对局)发结果之前的个人结算落库,一条 Lua 原子完成(审计 r2-battle#1,2026-10-19)。
	// KEYS[1]=pending blob,KEYS[2]=pending:id,KEYS[3]=积压索引,KEYS[4]=battle:lock,KEYS[5]=本局落库标记;
	// ARGV[1]=blob,ARGV[2]=battle_id,ARGV[3]=now_ms,ARGV[4]=player_id,ARGV[5]=标记 TTL 秒。
	// 返回 1 = 本次写入,2 = 本局早先已为该成员写过(不重写),3 = 锁属于别的局、或锁不在而 pending 槽属于别的局(不写)。
	// 结论见 ClassifyStageReply。
	//
	// 为什么需要它:确认模式按轮落库。原来每轮对全部成员无条件 SET,一轮里只要有一人没拿到确认,下一轮就把所有人
	// 重写一遍 —— 已被 scene 应用并销账的成员 pending 复活,账本条目已随销账摘除,下次登录整笔再发一次。
	// ResultCommit 现在只重写结论未知的成员;但「未知」(空应答、脚本 ERROR)可能是命令其实执行了、scene 随后
	// 也应用并销账了。这一类由标记判出来,返回 2,不重写。
	//
	// 为什么不只看锁(「锁 == battle_id 才写,锁不在按已收尾」):全球确认模式下锁在落库之前就可能合法缺席,
	// 锁不在证明不了「已销账」:
	//   - 锁由 scene PrepareBattle 写,TTL = 备战期限 + 60s;CreateBattle 确认后 ConfirmBattle 才条件续到正式期限 + 60s。
	//     续期是 fire-and-forget(Redis 断开时跳过、EVAL 失败只打日志、离线玩家 deadline 为 0 时跳过),
	//     没续上时锁会在战斗进行中自然过期;
	//   - 整场期限到时,scene reaper(30s 一拍)按同一个 deadline 条件删锁,与 battle 的强制平局几乎同时;
	//     reaper 先删时,强制平局的落库(尤其是退避之后的重试轮)看不到锁;
	//   - 重试轮拖过正式期限 + 60s 时,锁自然过期。
	// 这些情形按「已收尾」处理会把合法结算静默丢掉(与 scene 侧 StorePendingSettlementIfLockMatch 注释里
	// 「离线结算常晚于锁到达」是同一个结论)。所以锁不在时照写,与改造前一致;只有锁属于别的局才不写 ——
	// 那一局能拿到锁,说明这一局要么已销账、要么已被判废。scene 的**派发路径**都会丢弃这一局的结算
	// (在线时 InBattleComp 不符、或无组件时锁值不符 → 销账;离线暂存锁不符返回 2 → 销账)。
	// 不走派发的两条读路径不看锁:登录钩子(CheckPendingSettlementOnEnter → ApplyPendingSettlement)与冻结到期补读。
	// 改造前旧局 pending 写下之后、新局覆盖之前若走到它们,旧局奖励原本可能被应用;现在返回 3 不写,
	// 这个窗口里的旧局奖励**有意放弃**,换的是旧局不再覆盖新局的单槽 pending(2026-10-22 措辞更正)。
	//
	// 锁不在也不能盲写,还要看 pending 槽属于哪一局(2026-10-22 评审补):锁不在只说明「此刻没有哪一局持锁」,
	// 证明不了槽里那份 pending 比本局旧。时序:本局 B1 落库时成员 X 一直没写进去(battle 单边连不上 zone Redis),
	// X 的冻结在 B1 截止时解除,接着打完 B2 并下线;B2 的 pending 两键已写、还没消费,锁 B2 按 TTL 自然过期。
	// B1 的低频重试轮(ResultCommit 的 kSlowStagingRetryInterval)恢复后若照写,就把 B2 的 pending 覆盖了;
	// B2 的发件箱探测到 pending:id ≠ B2,按已销账停投,B2 的奖励永久丢失(旧盖新)。
	// 所以锁不在且 pending:id 属于别的局时同样返回 3,不写。锁 == 本局时照旧覆盖:锁证明本局是该玩家的当前局,
	// 槽里如果是别的局,只会更旧。锁不在且槽空、或槽已属于本局(scene 离线暂存写过同一份)时照写。
	// 返回 3 之后本局对 X 的派发都碰不到 B2:在线派发的条件销账只删 pending:id == 本局的记录,离线暂存锁不在时什么都不写,
	// 发件箱按 pending:id ≠ 本局停投。
	// 代价:锁不在而槽里恰好是更旧一局(已应用、等落盘销账;或被冻结 / 封禁暂缓应用)时,本局对该成员的奖励放弃。
	// 单槽 pending 下两局只能留一份(turn-battle-gap-closure.md §9.3 残余),这里选不覆盖:资产路径 fail-closed,
	// 宁可这一局不发,也不让迟到的一局抹掉已落定的另一局。
	//
	// 顺序:先判标记,再判锁与槽位归属,两键 SET 之后才写标记,ZADD 放最后。
	//   - 标记写在两键之后,标记在就说明两键已写。OOM 只在第一条写之前判一次(之后的写为保原子性放行),
	//     NOAUTH、READONLY、MISCONF 都在第一条写之前失败,所以不会出现「两键写了、标记没写」。
	//     按命令粒度的 ACL 误配置(放行 SET、拒绝 SADD)是例外,不在保证范围内;
	//   - ZADD 放最后:积压索引被写坏(键类型错)时 ZADD 报 WRONGTYPE,脚本回 ERROR,但两键与标记已落,
	//     下一轮读到标记返回 2,落库照常收口;索引只影响报数。
	// pending 两键照旧不带 TTL(landing-recovery §7),EXPIRE 只作用在标记上。
	inline constexpr char kStagePendingSettlementOnceScript[] =
		"if redis.call('SISMEMBER', KEYS[5], ARGV[4]) == 1 then return 2 end "
		"local lock = redis.call('GET', KEYS[4]) "
		"if lock and lock ~= ARGV[2] then return 3 end "
		"local owner = redis.call('GET', KEYS[2]) "
		"if lock == false and owner and owner ~= ARGV[2] then return 3 end "
		"redis.call('SET', KEYS[1], ARGV[1]) "
		"redis.call('SET', KEYS[2], ARGV[2]) "
		"redis.call('SADD', KEYS[5], ARGV[4]) "
		"redis.call('EXPIRE', KEYS[5], ARGV[5]) "
		"redis.call('ZADD', KEYS[3], ARGV[3], ARGV[4]) "
		"return 1";

	// scene 落盘确认后的销账 + 放锁,一条 Lua 原子完成(= 给 battle 侧发件箱的 ACK)。
	// KEYS[1]=pending blob,KEYS[2]=pending:id,KEYS[3]=battle:lock,KEYS[4]=battle:ctx,KEYS[5]=积压索引;
	// ARGV[1]=battle_id,ARGV[2]=player_id。
	// pending:id == battle_id 才删 pending 两键并 ZREM 索引;lock == battle_id 才删 lock + ctx。
	// 返回位掩码:1 = 删了 pending,2 = 删了锁。
	// 必须**条件删**:迟到的结算不能把玩家**下一场**的待结算记录 / 索引成员删掉(与 battle:lock 的条件删
	// 同一条纪律)。pending:id 不匹配时不 ZREM —— 那时索引成员代表的是下一局的记录。
	// 两件事必须在同一条脚本里:锁是「这一局还没完」的唯一跨进程证据,分两条命令时若只落地了删锁,
	// 玩家就能开下一场,而 battle 仍在重投这一局、下一局的无条件 SET 还会覆盖掉这一局的 pending。
	inline constexpr char kAckSettlementScript[] =
		"local r = 0 "
		"if redis.call('GET', KEYS[2]) == ARGV[1] then "
		"redis.call('DEL', KEYS[1]); redis.call('DEL', KEYS[2]); "
		"redis.call('ZREM', KEYS[5], ARGV[2]); r = 1 end "
		"if redis.call('GET', KEYS[3]) == ARGV[1] then "
		"redis.call('DEL', KEYS[4]); redis.call('DEL', KEYS[3]); r = r + 2 end "
		"return r";

	// scene 登录钩子读到解不开的 blob:无从得知 battle_id,无法条件删,两键与索引成员一起无条件删。
	// KEYS[1]=pending blob,KEYS[2]=pending:id,KEYS[3]=积压索引;ARGV[1]=player_id。
	// 伴生 id 键必须一起删,否则它会永远指向一个再也读不出内容的记录,battle 侧发件箱据此判定
	// "未销账"而重投到次数用尽;索引成员一起删,否则就是一个只能靠报数任务清理的孤儿。
	inline constexpr char kDeleteCorruptPendingSettlementScript[] =
		"redis.call('DEL', KEYS[1], KEYS[2]); "
		"redis.call('ZREM', KEYS[3], ARGV[1]); return 1";

	// battle 报数任务的孤儿清理:pending:id 键不存在才 ZREM 该玩家。
	// KEYS[1]=积压索引,KEYS[2]=该玩家的 pending:id 键;ARGV[1]=player_id。返回 1 = 删了孤儿,0 = 记录仍在。
	// EXISTS 与 ZREM 必须在同一条 Lua 里:分两条命令时,两步之间若恰好写入了新一局(SET + ZADD),
	// 随后的 ZREM 会把一条有效记录从索引里摘掉,它要等下一次写入才会重新进索引。
	inline constexpr char kSweepPendingIndexOrphanScript[] =
		"if redis.call('EXISTS', KEYS[2]) == 0 then "
		"return redis.call('ZREM', KEYS[1], ARGV[1]) end return 0";

	// battle 报数任务的读数:返回 {ZCARD, 最老一条的分数(毫秒,索引为空时 0)}。只读。
	// KEYS[1]=积压索引。分数按 tonumber 转成整数应答(毫秒时间戳在 double 内精确)。
	inline constexpr char kReadPendingBacklogScript[] =
		"local oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES') "
		"local oldestMs = 0 "
		"if oldest[2] then oldestMs = tonumber(oldest[2]) end "
		"return {redis.call('ZCARD', KEYS[1]), oldestMs}";
} // namespace battle_settlement
