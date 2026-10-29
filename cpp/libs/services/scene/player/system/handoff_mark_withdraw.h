#pragma once
#include "services/scene/player/system/handoff_sql_barrier.h"

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <optional>
#include <string>
#include <utility>
#include <vector>

// scene逻辑线程独占的撤资格重试表。Entry保存原H/F与发出前固定的R，不保存连接或对象指针。
// Redis成功/旧资格已被更新世代取代才销账；标记TTL到期不能证明迟到publisher已无资格。
// 满表拒绝新增而不淘汰未确认原件。在线Abort由原组件保持冻结并自行重试；退出后条目仍按原token收尾。
// 任何更新实体的回调另核原entity/holder/冻结代次，旧Confirm不能消掉新资格的同毫秒条目。
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
	inline const std::string& kLuaDelIfEqual = handoff_sql_barrier::kRevokeFenceLua;

	struct Entry
	{
		uint64_t playerId{0};
		std::string holderFence;
        std::string publishedFence;
        std::string revokedFence;
        std::string markValue;		   // 标记原文 "{epoch}:{ms}",唯一标识"我写的那一份"
		Clock::time_point deadline; // 原标记观察截止，仅诊断使用，不是资格过期
		Clock::time_point nextAttemptAt; // 周期重试的最早时刻
		uint32_t attempts{0};			 // 已被 CollectDue 取走几次;调用方据此只在首发失败时打 ERROR
	};

	class Queue
	{
	public:
        // 同(player,mark,原holder)幂等；满表返回false，绝不淘汰未确认撤资格。
		bool Add(uint64_t playerId, std::string markValue, Clock::time_point now, std::chrono::seconds ttl,
				 std::optional<Entry> &evictedOut, std::string holderFence = {},
                 std::string publishedFence = {}, std::string revokedFence = {})
		{
			evictedOut.reset();
			for (const auto &entry : entries_)
			{
				if (entry.playerId == playerId && entry.markValue == markValue && entry.holderFence == holderFence)
				{
					return false;
				}
			}
			if (entries_.size() >= kMaxPending) return false;
			Entry entry;
			entry.playerId = playerId;
			entry.markValue = std::move(markValue);
            entry.holderFence = std::move(holderFence);
            entry.publishedFence = std::move(publishedFence);
            entry.revokedFence = std::move(revokedFence);
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

        // 只取到期重试副本，不因标记TTL删除条目；销账必须确认同一原holder。
		std::vector<Entry> CollectDue(Clock::time_point now, bool force, std::size_t &expiredOut)
		{
			expiredOut = 0; // 资格没有 TTL，未确认撤销不能因标记过期而消账。

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
		bool Confirm(uint64_t playerId, const std::string &markValue, const std::string& holderFence = {})
		{
			const auto it = std::find_if(entries_.begin(), entries_.end(),
										 [&](const Entry &entry)
										 { return entry.playerId == playerId && entry.markValue == markValue && entry.holderFence == holderFence; });
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
