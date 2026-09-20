#pragma once

#include <algorithm>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <utility>
#include <vector>

// handoff 标记的"待撤回表"—— 纯容器,不含 entt / redis / muduo,可单测
// (cpp/tests/cross_zone_test 的 HandoffMarkWithdrawQueue.*)。
//
// 为什么需要:源 scene 写下的 player:{id}:handoff 标记("{epoch}:{ms}",EX 300)表示"这一刻的状态
// 已落盘、可以交接"。交接作废(退出优先 / AbortTravelHandoff)后玩家会继续产生新状态,标记必须撤回;
// 而撤回那一刻 Redis 可能正好不通,或命令已发出但应答丢了(结果未知)。旧实现是一条 best-effort DEL、
// 失败零日志,标记带着当前 owner_epoch 残活到 TTL:这段时间内该玩家任何一次跨节点 EnterScene 都会凭
// 旧标记免存盘过 scene_manager 的换手门 —— 目标节点读到旧档(回档),全程只有一条像双主的
// stale_owner_write_rejected。
//
// 契约(粘合代码在 player_lifecycle.cpp 的 WithdrawHandoffMark / RetryPendingHandoffWithdrawals):
//   * 先登记、后发命令;只有拿到 Redis 的整数应答(条件删除确实执行了)才 Confirm 销账。
//   * 表里还有某玩家的条目 = 他的旧标记可能还活着 → PlayerLifecycleSystem::IsSceneChangeBusy 对他
//     返回 true,**本节点**不替他发任何 EnterScene(fail-closed,AGENTS §11.3)。这道闸只管得住本节点
//     替在线玩家发出的请求;不经本节点的跨节点落点(玩家断线后重登落到别的节点)它拦不到,那条路
//     在撤回确认之前仍只有标记 TTL 兜底 —— 已知残余风险,要根治得在 scene_manager 侧处理。
//   * 截止时刻用单调时钟:标记自带 300s TTL,过了"TTL + 余量"Redis 侧的标记必然已过期,
//     放弃重试是安全的,但要计数(withdraw_expired)。
//   * 表只活在 scene 逻辑线程的 thread_local 里。进程退出丢表无害:location 指向已死节点,
//     scene_manager 接管时必然铸造新 epoch,旧标记自然失配。
//
// 时间一律由调用方传入(显式依赖,AGENTS §11.2),本文件不读任何时钟。
namespace handoff_mark_withdraw
{
	using Clock = std::chrono::steady_clock;

	// 上限只为给内存封顶:正常情况下表是空的,Redis 长时间不通时每个交接作废的玩家占一条。
	inline constexpr std::size_t kMaxPending = 4096;
	// 同一条目两次重试的最小间隔(重连时的强制重试不受它限制)。
	inline constexpr std::chrono::seconds kRetryInterval{5};
	// 截止时刻在标记 TTL 之上再留的余量(本机单调时钟与 Redis 过期时钟不是同一只表)。
	inline constexpr std::chrono::seconds kDeadlineMargin{5};

	// 条件删除:只删"值仍是我写的那一份"的标记。KEYS[1] = handoff key,ARGV[1] = 标记原文。
	// 不能退回无条件 DEL:撤回会被延迟重发,而这期间同一玩家可能已经写了新标记(重登后再次交接、
	// 疏散改派 DispatchEmergencyRelocate),精确比对才不会误删。标记里的 ms 取自墙钟,正常情况下新旧
	// 两份不同;同值碰撞(同一毫秒内重写 / 墙钟回拨)代码并不排除,其后果只是新标记被这次撤回删掉、
	// 那次落点被 scene_manager 以 18 拒回、玩家重登,不会回档。
	inline constexpr const char *kLuaDelIfEqual =
		"if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0";

	struct Entry
	{
		uint64_t playerId{0};
		std::string markValue;		   // 标记原文 "{epoch}:{ms}",唯一标识"我写的那一份"
		Clock::time_point deadline;	   // 过了就放弃(Redis 侧的标记此时也已过期)
		Clock::time_point nextAttemptAt; // 周期重试的最早时刻
		uint32_t attempts{0};			 // 已被 CollectDue 取走几次;调用方据此只在首发失败时打 ERROR
	};

	class Queue
	{
	public:
		// 登记一条待撤回。返回 true = 新登记;false = 同 (playerId, markValue) 已在表里(不改它的
		// deadline:截止时刻跟的是标记写入时刻,重复登记不该把它往后推)。
		// 表满时淘汰 deadline 最早的一条(最接近自然过期、风险最小),被淘汰的条目经 evictedOut 带出,
		// 由调用方记日志(要能定位到是哪个玩家)+ 计数;其余情况 evictedOut 置空。
		bool Add(uint64_t playerId, std::string markValue, Clock::time_point now, std::chrono::seconds ttl,
				 std::optional<Entry> &evictedOut)
		{
			evictedOut.reset();
			for (const auto &entry : entries_)
			{
				if (entry.playerId == playerId && entry.markValue == markValue)
				{
					return false;
				}
			}
			if (entries_.size() >= kMaxPending)
			{
				const auto earliest = std::min_element(entries_.begin(), entries_.end(),
													   [](const Entry &lhs, const Entry &rhs)
													   { return lhs.deadline < rhs.deadline; });
				evictedOut = std::move(*earliest);
				entries_.erase(earliest);
			}
			Entry entry;
			entry.playerId = playerId;
			entry.markValue = std::move(markValue);
			entry.deadline = now + ttl;
			entry.nextAttemptAt = now; // 立刻到期:调用方登记后紧接着 CollectDue 取走并发出第一次撤回
			entries_.push_back(std::move(entry));
			return true;
		}

		bool HasPlayer(uint64_t playerId) const
		{
			return std::any_of(entries_.begin(), entries_.end(),
							   [playerId](const Entry &entry) { return entry.playerId == playerId; });
		}

		// 1. 删掉 deadline <= now 的条目,个数写入 expiredOut。
		// 2. 返回该重试的条目副本(nextAttemptAt <= now;force = true 时忽略 nextAttemptAt),
		//    并把它们的 nextAttemptAt 推到 now + kRetryInterval、attempts 加一(副本里是加过之后的值)。
		// 返回副本而不是引用:调用方发命令的过程中不该攥着表内指针。
		std::vector<Entry> CollectDue(Clock::time_point now, bool force, std::size_t &expiredOut)
		{
			const auto firstExpired = std::remove_if(entries_.begin(), entries_.end(),
													 [now](const Entry &entry) { return entry.deadline <= now; });
			expiredOut = static_cast<std::size_t>(entries_.end() - firstExpired);
			entries_.erase(firstExpired, entries_.end());

			std::vector<Entry> due;
			for (auto &entry : entries_)
			{
				if (!force && entry.nextAttemptAt > now)
				{
					continue;
				}
				entry.nextAttemptAt = now + kRetryInterval;
				++entry.attempts;
				due.push_back(entry);
			}
			return due;
		}

		// 销账:只删精确匹配的那一条。返回 false = 表里没有(已过期丢弃 / 已被另一次应答销过)。
		bool Confirm(uint64_t playerId, const std::string &markValue)
		{
			const auto it = std::find_if(entries_.begin(), entries_.end(),
										 [&](const Entry &entry)
										 { return entry.playerId == playerId && entry.markValue == markValue; });
			if (it == entries_.end())
			{
				return false;
			}
			entries_.erase(it);
			return true;
		}

		std::size_t size() const { return entries_.size(); }

	private:
		// 线性表:平时为空,只有 Redis 不通时才有条目;查找点(IsSceneChangeBusy)不是 per-tick 路径。
		std::vector<Entry> entries_;
	};
} // namespace handoff_mark_withdraw
