#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>

// 托管落点提交之后、目标节点没把人建起来时的有界补救 —— 纯常量、纯判定与纯状态迁移,不含 entt / redis / muduo / protobuf,
// 不读任何时钟,可单测(cpp/tests/cross_zone_test 的 HostedPlacementRetry*)。风格同 relocate_confirm.h。
// 粘合代码在 player_lifecycle.cpp:HandleHostedPlacement 登记、InitPlayerFromAllData 建出实体时结清、
// SweepHostedPlacementRetries 每拍推进、重试前的权威读与再发起入场链。
// 设计与方案比较:docs/design/player-connection-lost-hold.md §4.4。
//
// ── 为什么需要 ──
// 托管落点(同文 §4.2)的提交点是 scene_manager 推给目标节点的 PlayerHostedPlacementEvent(broker ACK)。提交之后 location
// 已指向目标节点,源节点已销毁实体;目标节点的入场链(全局 SQL 角色登记 → 交接栅栏 → 认领载入 → EnterScene)任何一步
// 失败,角色就不在任何节点上。在线玩家此时收到 kEnterSceneFailed、客户端自己重登;离线托管的角色没有客户端,原先要等
// 下一次登录,或挂机月卡到期后由租约清理收尾。
//
// ── 做法 ──
// 目标节点给这条落点登记一条看护。入场链结束而没建出实体时,按退避有限次地用**同一条落点**再走一遍入场链。每次重试之前
// 先读一次权威落点(zone Redis 上同一个 slot 的 owner_epoch 与 location):仍在本 zone、本节点、这个场景,会话身份仍是
// 这条落点的那一次登录,owner_epoch 仍是落点时的值,场景在本节点上也还在 —— 全部成立才重试;任何一项变了都说明别人已经
// 做了决定(新登录、别的落点、租约清理),结清不动。
// 重试走与第一次完全相同的入场链,既有栅栏照常把关:交接栅栏登记要求 owner_epoch 等于落点的代次(kRegisterFenceLua),
// 继承标记清理按代次核对(A2′),TiDB 写权行按代次认领,入场会话栅栏按登录代次挡陈旧入场。读完之后才变的过期重试被它们拒掉;
// 先读一次是为了不在已经过时的落点上白白登记一份全局 SQL 角色原件。
//
// ── 契约(粘合代码必须守住,改任何一条都要重过一遍单测与设计)──
//   * 键 = player_id,同一玩家最多一条看护。同一玩家又来一条托管落点就用新的整条替换(scene-cmd 按目标节点分区、
//     key = player_id,同一玩家的事件保序,后到的是更新的提交)。
//   * 只在"落点到达时本节点没有这名玩家的实体"时登记。有实体的是同节点换图:失败了他还在原场景里、照常托管,由组队跟随的
//     兜底重试(team-follow-server-trail.md §3.7)再带他走,用不着这里。
//   * 一次尝试"在途" = 角色登记还有没结清的步骤(RoleAdmissionSystem::HasAdmissionInFlight)或待入场表里有他。在途时不动,
//     也不区分是不是本看护发起的那一次(新登录的入场链同样会建出他,建出来就结清)。
//   * 结清(成功):托管落点的载入建出实体时当场结清;每拍扫描时本节点已有他的实体(别的入场链建的)也结清。
//   * 重试只在权威读数仍指着这条落点时发起(DecideAfterRead == kRetry)。读不到就过 kReadRetryInterval 再读,不盲试。
//   * 节点疏散中 / 计划排空中 / 持久任务已停时不发起任何读与重试(节点要走了,新建的实体只会被再搬一次或随停机退出)。
//   * 表只活在 scene 逻辑线程的 thread_local 里,无锁。进程退出丢表 = 与"托管中的角色随节点崩溃一起不在世界里"同一类,
//     数据在盘上,下次登录照常载入。
// 时间一律由调用方传入(显式依赖,AGENTS §11.2),本文件不读任何时钟。
namespace hosted_placement_retry
{
	using Clock = std::chrono::steady_clock;

	// ── 预算(全部按单调时钟)────────────────────────────────────────────────

	// 第 i 次重试之前等 kRetryDelays[i](首次尝试不算重试)。间隔递增:角色登记 / 交接栅栏 / 认领失败多半是下游短暂不可用。
	inline constexpr std::chrono::seconds kRetryDelays[] = {std::chrono::seconds{5}, std::chrono::seconds{20},
															std::chrono::seconds{60}};
	inline constexpr uint32_t kMaxRetries = static_cast<uint32_t>(std::size(kRetryDelays));

	// 一次尝试的入场链最长多久(角色登记 15 秒,交接栅栏随它;认领 30 秒;继承标记清理 5 秒;取整 90 秒)。只用来推总预算。
	inline constexpr std::chrono::seconds kAttemptBudget{90};

	constexpr std::chrono::seconds TotalRetryDelay()
	{
		std::chrono::seconds total{0};
		for (const auto delay : kRetryDelays)
		{
			total += delay;
		}
		return total;
	}

	// 一条看护从登记起最长活多久:到点放弃,不论在等什么。必须盖住首次与每次重试的入场链上限再加上全部退避
	// (4 × 90 + 85 = 445 秒),取整 8 分钟。
	inline constexpr std::chrono::seconds kWatchBudget{480};
	static_assert(kWatchBudget >= kAttemptBudget * (kMaxRetries + 1) + TotalRetryDelay(),
				  "总预算要盖住首次与每次重试的入场链上限,再加上全部退避");

	// 重试前的权威读没读到(Redis 不可用 / 命令发不出 / 空应答 / ERROR / 形状不对),或读到时恰有别的入场链在途:过多久再读。
	inline constexpr std::chrono::seconds kReadRetryInterval{1};

	// 上限只为给内存封顶。满了新的落点不登记看护(只打 ERROR,入场链照常发起),不淘汰在看的。
	inline constexpr std::size_t kMaxTracked = 4096;

	// ── 枚举 ──────────────────────────────────────────────────────────────
	// 底层类型 uint8_t 数的是枚举项个数(AGENTS §11.2)。kCount 固定为最后一项,同时是名表长度。名表原文同时是日志取值。

	// 一条看护所处阶段(现 3 种)。
	enum class Phase : uint8_t
	{
		kAttempting, // 一次尝试已发起:入场链在途,或刚结束、下一拍才判
		kBackoff,	 // 上一次尝试没建出实体,等退避(或重读)到点
		kReading,	 // 重试前的权威读在途

		kCount
	};

	constexpr std::size_t kPhaseCount = static_cast<std::size_t>(Phase::kCount);

	inline constexpr const char *kPhaseNames[] = {
		"attempting",
		"backoff",
		"reading",
	};
	static_assert(std::size(kPhaseNames) == kPhaseCount, "kPhaseNames 必须与 Phase 一一对应");

	constexpr const char *PhaseName(Phase phase)
	{
		const auto index = static_cast<std::size_t>(phase);
		return index < kPhaseCount ? kPhaseNames[index] : "?";
	}

	// 每拍扫描对一条看护的判定(现 6 种)。
	enum class TickAction : uint8_t
	{
		kWait,			  // 继续等
		kResolveLanded,	  // 本节点已有他的实体(这条看护建的,或别的入场链建的):结清
		kGiveUpTimeout,	  // 超出总预算:放弃
		kStartBackoff,	  // 一次尝试结束而没建出实体:进退避,等到点再读
		kGiveUpExhausted, // 一次尝试结束而没建出实体,重试次数已用尽:放弃
		kRead,			  // 退避到点:发重试前的权威读

		kCount
	};

	constexpr std::size_t kTickActionCount = static_cast<std::size_t>(TickAction::kCount);

	inline constexpr const char *kTickActionNames[] = {
		"wait", "resolve_landed", "give_up_timeout", "start_backoff", "give_up_exhausted", "read",
	};
	static_assert(std::size(kTickActionNames) == kTickActionCount, "kTickActionNames 必须与 TickAction 一一对应");

	constexpr const char *TickActionName(TickAction action)
	{
		const auto index = static_cast<std::size_t>(action);
		return index < kTickActionCount ? kTickActionNames[index] : "?";
	}

	// 权威读读到之后怎么办(现 7 种)。只有 kRetry 发起重试,其余都结清这条看护(kResolve* 是"别人已经做了决定",
	// kGiveUp* 是"这条落点已经不可能在本节点落地",日志级别不同)。
	enum class ReadVerdict : uint8_t
	{
		kRetry,				  // 仍指着这条落点:用同一条落点再走一遍入场链
		kResolveAbsent,		  // location 不在了(登出、租约到期的清理):别的流程已经替他收了尾
		kResolveMoved,		  // 已在别的 zone / 节点 / 场景:别的落点已经提交
		kResolveNewLogin,	  // location 的会话身份已不是这条落点的那一次登录:新登录在接管
		kResolveEpochMoved,	  // owner_epoch 已不是落点时的值:归属已推进
		kGiveUpIndeterminate, // 读到了但判不清(location 解析失败、zone 为 0):不冒险重试
		kGiveUpSceneGone,	  // 仍指着这条落点,但场景已不在本节点上:重试也进不去(残余见设计 §4.4)

		kCount
	};

	constexpr std::size_t kReadVerdictCount = static_cast<std::size_t>(ReadVerdict::kCount);

	inline constexpr const char *kReadVerdictNames[] = {
		"retry", "resolve_absent", "resolve_moved", "resolve_new_login", "resolve_epoch_moved", "give_up_indeterminate",
		"give_up_scene_gone",
	};
	static_assert(std::size(kReadVerdictNames) == kReadVerdictCount, "kReadVerdictNames 必须与 ReadVerdict 一一对应");

	constexpr const char *ReadVerdictName(ReadVerdict verdict)
	{
		const auto index = static_cast<std::size_t>(verdict);
		return index < kReadVerdictCount ? kReadVerdictNames[index] : "?";
	}

	// ── 一条看护 ────────────────────────────────────────────────────────────

	// 看护的状态。落点本身(玩家、场景、代次、身份、修订)由粘合层与它放在同一个表项里;单测经
	// PlayerLifecycleSystem::PeekHostedPlacementRetry 取副本。
	struct Watch
	{
		Phase phase{Phase::kAttempting};
		uint32_t retriesDone{0};		// 已发起的重试次数(首次尝试不算)
		Clock::time_point createdAt{};	// 登记时刻(只进日志)
		Clock::time_point deadline{};	// createdAt + kWatchBudget:到点放弃
		Clock::time_point nextAt{};		// kBackoff:最早何时发重试前的权威读
		uint32_t readGen{0};			// 权威读的代际:只由 MarkReadSent 自增,应答凭它对号
	};

	// 登记(落点到达、第一次尝试即将发起)。
	constexpr Watch Begin(Clock::time_point now)
	{
		Watch watch;
		watch.phase = Phase::kAttempting;
		watch.createdAt = now;
		watch.deadline = now + kWatchBudget;
		watch.nextAt = now;
		return watch;
	}

	// 每拍扫描的输入,全部由粘合层在这一刻采集好传入。默认值取"不动"的一侧。
	struct TickFacts
	{
		bool entityPresent{false};	 // 本节点此刻有这名玩家的有效实体
		bool attemptInFlight{true};	 // 角色登记还有没结清的步骤,或待入场表里有他
		bool pastWatchDeadline{false};
		bool pastNextAt{false}; // now >= watch.nextAt(只在 kBackoff 有意义)
	};

	// 每拍的判定,按顺序:有实体就结清;超出总预算就放弃;入场链在途就等;kAttempting 结束而没建出实体 → 还有重试次数进退避,
	// 否则放弃;kBackoff 到点 → 读;kReading → 等读的应答。
	constexpr TickAction DecideTick(const Watch &watch, const TickFacts &facts)
	{
		if (facts.entityPresent)
		{
			return TickAction::kResolveLanded;
		}
		if (facts.pastWatchDeadline)
		{
			return TickAction::kGiveUpTimeout;
		}
		if (facts.attemptInFlight)
		{
			return TickAction::kWait;
		}
		switch (watch.phase)
		{
		case Phase::kAttempting:
			return watch.retriesDone >= kMaxRetries ? TickAction::kGiveUpExhausted : TickAction::kStartBackoff;
		case Phase::kBackoff:
			return facts.pastNextAt ? TickAction::kRead : TickAction::kWait;
		default:
			return TickAction::kWait; // kReading:等读的应答
		}
	}

	// 一次尝试结束而没建出实体:进退避,下一次读在 kRetryDelays[retriesDone] 之后。调用方保证 retriesDone < kMaxRetries
	// (DecideTick 返回 kStartBackoff 时成立);越界时按最后一档等,不越界读数组。
	constexpr void StartBackoff(Watch &watch, Clock::time_point now)
	{
		const uint32_t index = watch.retriesDone < kMaxRetries ? watch.retriesDone : kMaxRetries - 1;
		watch.phase = Phase::kBackoff;
		watch.nextAt = now + kRetryDelays[index];
	}

	// 权威读即将发出,返回它的代际(回调按值带着它)。
	constexpr uint32_t MarkReadSent(Watch &watch)
	{
		watch.phase = Phase::kReading;
		return ++watch.readGen;
	}

	// 一条读的应答现在还作不作数:看护仍在 kReading,且代际就是最近发出的那一条。看护被新落点替换(Begin 从 0 起)或已结清时,
	// 粘合层按 player_id 找不到 / 代际对不上,应答一律丢弃。
	constexpr bool AcceptsReadReply(const Watch &watch, uint32_t gen)
	{
		return watch.phase == Phase::kReading && watch.readGen == gen;
	}

	// 读没读到,或读到时恰有别的入场链在途:回到退避,过 kReadRetryInterval 再读。
	constexpr void DeferRead(Watch &watch, Clock::time_point now)
	{
		watch.phase = Phase::kBackoff;
		watch.nextAt = now + kReadRetryInterval;
	}

	// 读数确认仍指着这条落点,重试即将发起。
	constexpr void MarkRetryStarted(Watch &watch)
	{
		watch.phase = Phase::kAttempting;
		++watch.retriesDone;
	}

	// ── 权威读的解释 ────────────────────────────────────────────────────────

	// 这条看护在等的落点(落点事件里的四项,重试要它们原样不变)。
	struct ExpectedPlacement
	{
		uint64_t sceneId{0};
		uint64_t ownerEpoch{0};
		uint32_t sessionId{0};
		uint64_t loginGeneration{0};
	};

	// 一次权威读的读数,加上本节点此刻的事实。全部由粘合层从同一次应答与本节点状态里采集好传入。
	// 默认值取"不重试"的一侧:漏填时判 kGiveUpIndeterminate(present 为真、parsed 为假)。
	struct ReadFacts
	{
		bool present{true};				  // location 元素是 STRING(键存在)。按元素类型取,不把解析失败当成不存在
		bool parsed{false};				  // present 且 PlayerLocation 解析成功
		uint32_t locationZoneId{0};		  // location.zone_id
		uint32_t selfZoneId{0};			  // 本节点的 zone
		bool nodeIsSelf{false};			  // location.node_id 是本节点号(node_id 只在 zone 内唯一,另比 zone)
		uint64_t locationSceneId{0};	  // location.scene_id
		uint32_t locationSessionId{0};	  // location.session_id(落点会话身份)
		uint64_t locationLoginGeneration{0}; // location.login_generation
		uint64_t ownerEpoch{0};			  // 同一次读到的 owner_epoch 键(键不存在 / 写坏 = 0)
		bool sceneOnThisNode{false};	  // 这条落点的场景此刻在本节点上
	};

	// 读数说明了什么。按顺序:
	//   location 不存在 → kResolveAbsent;解析失败 → kGiveUpIndeterminate;
	//   节点号不是本节点 → kResolveMoved;任一 zone 为 0 → kGiveUpIndeterminate(zone 0 证明不了是本节点,与改派核实同向);
	//   zone 不同 / 场景不同 → kResolveMoved;
	//   会话号或登录代次不同 → kResolveNewLogin;owner_epoch 不同 → kResolveEpochMoved;
	//   场景已不在本节点上 → kGiveUpSceneGone;其余 → kRetry。
	constexpr ReadVerdict DecideAfterRead(const ReadFacts &facts, const ExpectedPlacement &expected)
	{
		if (!facts.present)
		{
			return ReadVerdict::kResolveAbsent;
		}
		if (!facts.parsed)
		{
			return ReadVerdict::kGiveUpIndeterminate;
		}
		if (!facts.nodeIsSelf)
		{
			return ReadVerdict::kResolveMoved;
		}
		if (facts.locationZoneId == 0 || facts.selfZoneId == 0)
		{
			return ReadVerdict::kGiveUpIndeterminate;
		}
		if (facts.locationZoneId != facts.selfZoneId || facts.locationSceneId != expected.sceneId)
		{
			return ReadVerdict::kResolveMoved;
		}
		if (facts.locationSessionId != expected.sessionId || facts.locationLoginGeneration != expected.loginGeneration)
		{
			return ReadVerdict::kResolveNewLogin;
		}
		if (facts.ownerEpoch != expected.ownerEpoch)
		{
			return ReadVerdict::kResolveEpochMoved;
		}
		if (!facts.sceneOnThisNode)
		{
			return ReadVerdict::kGiveUpSceneGone;
		}
		return ReadVerdict::kRetry;
	}
} // namespace hosted_placement_retry
