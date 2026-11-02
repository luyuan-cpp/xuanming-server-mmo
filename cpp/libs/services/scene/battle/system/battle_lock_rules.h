#pragma once

// 战斗串行化锁 battle:lock 的 scene 侧判定(纯函数,零项目依赖,单测直接盯:cpp/tests/routing_identity_test)。
//
// 键:battle:lock:{player_id} = 十进制 battle_id,带 TTL;伴生 battle:ctx:{player_id} 与它同生共死。
// 写入与续期的脚本在本头(单测对真实 Redis 直接执行同一份),读与条件删的脚本在 player_battle.cpp。
// 锁是协调键(TiDB 决策 §9.3 表 C):match 读它拦排队,scene 在「实体没了但锁还在」时靠它重建冻结。
// 结算原件不在 Redis:结算只走 match SQL + player_inbox(player-data-tidb-scaling-plan.md §5.4)。
// 所以锁丢了不丢奖,但排队闸与冻结重建会失明,写与续都带有界重试(turn-battle-gap-closure.md §10)。
//
// 谁放锁(所有删除都按 battle_id 条件删,迟到的一方不会误伤下一局):
//   - 结算随存档提交之后:PlayerBattleSystem::CompleteInboxSettlement(收件箱状态机在同 E/q 的双 SQL 确认之后才调);
//   - 备战取消:CancelBattlePrepare;
//   - FIGHTING 冻结到期(battle 崩溃、match 判 ABORT、结算迟迟不到):reaper 的 ReleaseExpiredFightingFreeze;
//   - 其余情况(玩家离线、删锁命令没落地、PREPARING 到期)随 TTL 过期,TTL 比冻结到期多 kLockExtraTtlSec。

#include <cstdint>

namespace battle_lock
{
	// 战斗锁键(值 = 十进制 battle_id,带 TTL)与伴生上下文键,hiredis 格式串,%llu = player_id。玩家域键,带 hash tag,
	// 两把键同 slot 才能进同一段 Lua。与 go/shared/redisslot 的 BattleLockKey / BattleCtxKey 逐字一致(该包测试解析本头文件比对)。
	inline constexpr char kBattleLockKeyFmt[] = "battle:lock:{%llu}";
	inline constexpr char kBattleCtxKeyFmt[] = "battle:ctx:{%llu}";

	// 写锁 + ctx(PrepareBattle 首发与重试共用),一条 Lua 原子完成:两键要么都写、要么都没写,同值同 TTL。
	// KEYS[1]=锁,KEYS[2]=ctx;ARGV[1]=battle_id,ARGV[2]=TTL 秒,ARGV[3]=ctx 序列化。返回 1。
	// 无条件写:备战成功那一刻本节点的 InBattleComp 是权威。
	// 重试由调用方在重发前确认实体上的 InBattleComp 仍是这一局 —— 备战被取消 / 结算 / 判废之后再写,会复活一把已删的锁。
	inline constexpr char kWriteBattleLockScript[] =
		"redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2]) "
		"redis.call('SET', KEYS[2], ARGV[3], 'EX', ARGV[2]) "
		"return 1";

	// 条件续期 + 覆写 ctx,**只延长、不缩短**(确认升级、重复确认、离线确认、按锁重建后的复核共用)。
	// KEYS[1]=锁,KEYS[2]=ctx;ARGV[1]=battle_id,ARGV[2]=TTL 秒,ARGV[3]=ctx 序列化。
	// 锁值 == battle_id 才动:新 TTL = max(锁剩余 TTL, ARGV[2]),ctx 用同一个 TTL。返回 1 = 命中,0 = 锁不在或已易主。
	// 为什么只延长:重复确认每 10 秒一次、离线确认按事件到达时刻重算 TTL,都可能比锁现有的剩余时间短;
	// 锁必须活得比冻结久,一条迟到的确认不能把它缩回去。
	// 锁不在时**不补写**:续期只负责延长;在这里补写,一条迟到的确认就能复活被取消 / 结算 / 判废删掉的锁。
	inline constexpr char kRenewBattleLockIfMatchScript[] =
		"if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end "
		"local ttl = tonumber(ARGV[2]) "
		"local remaining = redis.call('TTL', KEYS[1]) "
		"if remaining > ttl then ttl = remaining end "
		"redis.call('SET', KEYS[2], ARGV[3], 'EX', ttl) "
		"redis.call('EXPIRE', KEYS[1], ttl) "
		"return 1";

	// ---- zone Redis 应答的形状:空应答 / ERROR 不是业务结论(2026-10-16) ----
	//
	// hiredis 在连接断开或被释放时,把所有在途命令的回调以空应答(redisReply* == nullptr)调用一次。
	// Redis 主切换时代理掐连接、存活探测 15 秒没有应答就摘连接(thread_context/redis_manager.h「存活契约」)、
	// 停机 Shutdown 都会触发。空应答的意思是「这次没问到」,不是 NIL(键不存在),更不是「没有锁」这个结论。
	// 下面的判定只认形状,不碰 hiredis 类型;从 redisReply 换算成形状的唯一适配在 battle_lock_reply_shape.h。
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

	// 只进日志。
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
		return "other";
	}

	// ---- 战斗锁写入 / 续期的有界重试(2026-10-22 负责人拍板 (c),turn-battle-gap-closure.md §10) ----
	//
	// 适用的写都幂等:「锁 + ctx」无条件写(重发同值,kWriteBattleLockScript)、按锁值条件只延长的续期
	// (kRenewBattleLockIfMatchScript)。空应答 / ERROR 只说明这次没问到:命令可能执行了也可能没执行,
	// 再发一次不会改变已经执行的结果。脚本的整数应答(0 / 1)是结论,不重试:0 = 锁不在或已易主,那是事实而不是失败。
	// 预算用单调时钟:从首发起算,最后一次也必须落在预算内;次数另有上限,两者先到先停。
	inline constexpr uint32_t kLockWriteMaxAttempts = 6;       // 含首发
	inline constexpr uint64_t kLockWriteRetryBudgetMs = 30000; // 一个 reaper 节拍:再晚,登录复查与确认周期补发已经接手
	inline constexpr uint64_t kLockWriteRetryBaseDelayMs = 500;
	inline constexpr uint64_t kLockWriteRetryMaxDelayMs = 8000;

	inline bool IsLockWriteAnswered(const ReplyShape shape)
	{
		return shape == ReplyShape::kInteger;
	}

	// 第 attempt 次(从 1 起)没问到之后,下一次在多少毫秒后发;返回 0 = 放弃(次数或预算用尽)。
	// elapsedMs = 此刻距首发的单调时钟毫秒。退避 base × 2^(attempt-1),封顶 max;抖动在 [0, delay/2) 里,
	// 由 jitterSeed 确定(调用方传 player_id ^ battle_id):不同玩家错开,同一输入结果固定,不引入随机数源,单测可复现。
	// attempt 为 0 是调用方缺陷:直接放弃,保证有界。
	inline uint64_t NextLockWriteRetryDelayMs(const uint32_t attempt, const uint64_t elapsedMs, const uint64_t jitterSeed)
	{
		if (attempt == 0 || attempt >= kLockWriteMaxAttempts)
		{
			return 0;
		}
		uint64_t delay = kLockWriteRetryBaseDelayMs;
		for (uint32_t step = 1; step < attempt && delay < kLockWriteRetryMaxDelayMs; ++step)
		{
			delay *= 2;
		}
		if (delay > kLockWriteRetryMaxDelayMs)
		{
			delay = kLockWriteRetryMaxDelayMs;
		}
		// splitmix64 的末段混合:把相邻的 seed / attempt 打散到整个区间。
		uint64_t mixed = jitterSeed + 0x9E3779B97F4A7C15ULL * attempt;
		mixed = (mixed ^ (mixed >> 30)) * 0xBF58476D1CE4E5B9ULL;
		mixed = (mixed ^ (mixed >> 27)) * 0x94D049BB133111EBULL;
		mixed ^= mixed >> 31;
		delay += mixed % (delay / 2);
		if (elapsedMs >= kLockWriteRetryBudgetMs || delay > kLockWriteRetryBudgetMs - elapsedMs)
		{
			return 0;
		}
		return delay;
	}

	// ---- scene 登录钩子对战斗锁的读数判定 ----
	//
	// OnPlayerEnterScene 读 battle:lock + ctx 重建冻结(player_battle.cpp 的 kGetLockAndCtxScript:
	// 锁不在返回 nil,在则返回数组)。要分清「没有」与「没问到」:没问到时把玩家记入登录复查,由 scene reaper 的节拍重读。
	enum class LoginLockRead : uint8_t
	{
		kNoLock,  // NIL,以及 ERROR / 意外类型(见下)
		kLock,    // 数组:{锁值, ctx 或 nil, TTL}
		kRecheck, // 空应答:没问到,记入登录复查
	};

	// 只有空应答进复查。ERROR 按「无锁」是既有取舍:锁是咨询性的(权威是 InBattleComp),
	// EVAL 收到 ERROR 是服务端答复了但拒绝执行;把它也改成复查会让脚本层面的持续性错误变成每 30 秒一次的无尽重试。
	// 代价:ERROR 那一次登录不重建冻结,战斗闸(资产通道、收件箱 requiresOutOfBattle)在战斗剩余时间里看不到这场战斗,
	// 直到收件箱结算到达或玩家下次进场。空应答不同:它只说明连接断了,断线恢复后一定问得到,值得重试。
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
		// 锁指向一场还没被本实体判作废的战斗:按锁 + ctx 重建冻结。这一局若其实已经结算,收件箱重放原结局时
		// CompleteInboxSettlement 按同一局解冻放锁;结算不再到来时 reaper 按 ctx 里的期限收尾,冻结时长有界。
		kRebuild,
		// 锁指向本实体上 reaper 已按期限判作废(FIGHTING 到期)的那一局:不重建,补一次条件删锁。
		// 只有删锁没落地(Redis 抖)才会再读到这把锁。重建会把玩家冻进已结束的战斗(最长约一个 reaper 节拍)、
		// 推一次重连提示让客户端弹「战斗已结束」,还把锁续到「现在 + 60 秒」;删锁一直失败时这个回路没有硬上限。
		kSkipExpired,
	};

	// expiredBattleId 为 0 = 本实体没有被判作废的局:这时不论锁值是什么(锁值也是 0 时同样)都不判作废。
	inline LockRebuildDecision ClassifyLockRebuild(const uint64_t lockBattleId, const uint64_t expiredBattleId)
	{
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
		bool travelHandoffOrFrozen = false; // 跨 zone 冻结 / 交接中
		bool redisReady = false;
	};

	enum class LoginRecheckStep : uint8_t
	{
		kRun,  // 摘标记,重新读锁;又没问到会重新挂上
		kKeep, // 标记保留,本轮不跑
		kDrop, // 摘标记,不重跑
	};

	// 顺序不能换:退出中 > 交接中 > Redis 不可用 > 重跑。
	//  - 退出中视同离线:下次进场登录钩子重跑;
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
} // namespace battle_lock
