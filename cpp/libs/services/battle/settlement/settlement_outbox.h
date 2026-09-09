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
// 这个头只放**判定**:纯函数、零依赖,能被单测直接盯住(与 node_kafka_command_filter.h
// 拆出来的理由一样 —— 否则要测就得把 Redis、Kafka、muduo loop 整条链拖进测试工程)。

#include <cstdint>

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
		// 注意这不等于结算丢失 —— 持久记录仍在(TTL 7 天),玩家下次进场景会补应用。
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

	// ---- Redis key 契约(与 scene 侧 player_battle.cpp 的常量必须逐字一致)----
	//
	// 值 = 序列化的 BattleSettlementEvent;伴生 id 键 = 十进制 battle_id。
	// 两个键**同 SET / 同 TTL / 同 DEL**,与既有的 battle:lock + battle:ctx 同一形态:
	// 单靠 blob 无法在 Lua 里判断它属于哪一局(Lua 不会解 protobuf),伴生 id 键就是
	// 「条件销账」与「ACK 探测」共同的那把尺子。
	inline constexpr char kPendingSettlementKeyFmt[] = "battle:settlement:pending:%llu";
	inline constexpr char kPendingSettlementIdKeyFmt[] = "battle:settlement:pending:id:%llu";

	// 落库(两键同 TTL)。KEYS[1]=blob 键,KEYS[2]=id 键;ARGV[1]=blob,ARGV[2]=battle_id,
	// ARGV[3]=ttl 秒。
	inline constexpr char kSetPendingSettlementScript[] =
		"redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[3]); "
		"redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[3]); return 1";

	// 条件销账:id 键 == battle_id 才删两键。返回 1 = 销账成功,0 = 记录属于别的局
	// (迟到的结算不能把玩家**下一场**的待结算记录删掉 —— 与 battle:lock 的条件删同一条纪律)。
	inline constexpr char kDeletePendingSettlementIfMatchScript[] =
		"if redis.call('GET', KEYS[2]) == ARGV[1] then "
		"redis.call('DEL', KEYS[1]); redis.call('DEL', KEYS[2]); return 1 else return 0 end";
} // namespace battle_settlement
