#pragma once

// 同 slot 停等队列与单条命令的重试预算(docs/design/redis-player-store-cluster.md §5.6),纯逻辑:不碰网络,时间由调用方传入。
//
// 核心契约:每个队列至多一条命令在途(队头),后续按调用顺序在本地排队,队头拿到最终结果之后才轮到下一条。
// 所以同 slot 的执行顺序与回调顺序都等于调用顺序,与重定向类型、迁移方式无关。不同队列之间互不影响。
//
// 单线程使用(每 loop 线程一个)。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <optional>
#include <unordered_map>
#include <utility>
#include <vector>

#include "redis_slot.h"

namespace redis_cluster
{
	using Clock = std::chrono::steady_clock;
	using TimePoint = Clock::time_point;
	using Duration = Clock::duration;

	// 队列编号:0..16383 是 slot,kAnyNodeQueue 给「发往任一已连接节点」的命令(SCRIPT LOAD)共用。
	using QueueIndex = uint16_t;
	inline constexpr QueueIndex kAnyNodeQueue = redis_slot::kSlotCount;

	struct RetryPolicy
	{
		// 单条命令的总预算,从它成为队头起算:排队等待不消耗预算,否则一次长迁移会让后面的命令还没发出就超时。
		// 取 10s 覆盖原子迁移交接停写与 cluster-node-timeout 5s 的故障转移。
		Duration budget = std::chrono::seconds(10);
		Duration initialBackoff = std::chrono::milliseconds(50);
		Duration maxBackoff = std::chrono::seconds(1);
		// MOVED / ASK 连续立即重发的上限,超过后按退避走。正常迁移一两次就收敛;
		// 只有两个节点互相指向(拓扑过渡期)才会碰到,不设上限会以 RTT 为周期空转到预算耗尽。
		uint32_t maxImmediateRedirects = 5;
	};

	struct QueueLimits
	{
		std::size_t perQueue = 256;   // 每个队列排队的条数,不含队头
		std::size_t total = 65536;    // 全部队列排队的条数之和,不含队头
	};

	enum class RetryMode : uint8_t
	{
		kImmediate,   // MOVED / ASK
		kBackoff,     // 其余「未执行」错误,以及暂时没有可用节点
	};

	struct RetryDecision
	{
		enum class Kind : uint8_t
		{
			kResendNow,
			kRetryAfter,
			kExhausted,
		};

		Kind kind = Kind::kExhausted;
		Duration delay = Duration::zero();   // 只有 kRetryAfter 有意义
	};

	// 队头命令的重试预算。退避从 initialBackoff 起翻倍、封顶 maxBackoff,并截断到剩余预算:
	// 最后一次重试恰好落在截止时刻,预算不会被最后一段退避白白浪费。
	class HeadRetryBudget
	{
	public:
		HeadRetryBudget(const RetryPolicy& policy, TimePoint start)
			: deadline_(start + policy.budget)
			, nextBackoff_(policy.initialBackoff)
		{
		}

		RetryDecision Next(const RetryPolicy& policy, RetryMode mode, TimePoint now)
		{
			if (now >= deadline_)
			{
				return {RetryDecision::Kind::kExhausted, Duration::zero()};
			}
			if (mode == RetryMode::kImmediate && immediateRedirects_ < policy.maxImmediateRedirects)
			{
				++immediateRedirects_;
				return {RetryDecision::Kind::kResendNow, Duration::zero()};
			}
			const Duration remaining = deadline_ - now;
			const Duration delay = nextBackoff_ < remaining ? nextBackoff_ : remaining;
			const Duration doubled = nextBackoff_ * 2;
			nextBackoff_ = doubled < policy.maxBackoff ? doubled : policy.maxBackoff;
			return {RetryDecision::Kind::kRetryAfter, delay};
		}

	private:
		TimePoint deadline_;
		Duration nextBackoff_;
		uint32_t immediateRedirects_ = 0;
	};

	enum class SubmitResult : uint8_t
	{
		kDispatchNow,      // 队列原本空闲,这条成为队头,调用方立刻发送
		kQueued,           // 排在后面,轮到时由调用方发送
		kQueueFull,        // 超过每队列上限,没有入队
		kTotalQueueFull,   // 超过全进程上限,没有入队
	};

	enum class HeadState : uint8_t
	{
		kPending,    // 刚成为队头或等待立即重发,还没发出
		kInFlight,   // 已发出,等应答
		kBackoff,    // 退避中,等重试定时器
	};

	// Command 是队列持有的命令句柄,只要求可移动(测试用 int,路由层用 shared_ptr)。
	template <typename Command>
	class SlotCommandQueues
	{
	public:
		SlotCommandQueues(const RetryPolicy& policy, const QueueLimits& limits)
			: policy_(policy)
			, limits_(limits)
		{
		}

		SubmitResult Submit(QueueIndex queue, Command command, TimePoint now)
		{
			const auto found = queues_.find(queue);
			if (found == queues_.end())
			{
				queues_.try_emplace(queue, std::move(command), policy_, now);
				return SubmitResult::kDispatchNow;
			}
			Entry& entry = found->second;
			if (entry.waiting.size() >= limits_.perQueue)
			{
				return SubmitResult::kQueueFull;
			}
			if (queuedTotal_ >= limits_.total)
			{
				return SubmitResult::kTotalQueueFull;
			}
			entry.waiting.push_back(std::move(command));
			++queuedTotal_;
			return SubmitResult::kQueued;
		}

		bool Busy(QueueIndex queue) const
		{
			return queues_.find(queue) != queues_.end();
		}

		// 队头句柄;队列空闲时为空。指针只在下一次修改本对象之前有效。
		const Command* Head(QueueIndex queue) const
		{
			const auto found = queues_.find(queue);
			return found == queues_.end() ? nullptr : &found->second.head;
		}

		std::optional<HeadState> State(QueueIndex queue) const
		{
			const auto found = queues_.find(queue);
			if (found == queues_.end())
			{
				return std::nullopt;
			}
			return found->second.state;
		}

		void MarkInFlight(QueueIndex queue)
		{
			const auto found = queues_.find(queue);
			if (found != queues_.end())
			{
				found->second.state = HeadState::kInFlight;
			}
		}

		// 队头被判定「没有执行」,决定下一步。kResendNow 之后队头是 kPending,kRetryAfter 之后是 kBackoff;
		// kExhausted 不改状态,调用方接着 Abort。
		RetryDecision OnNotExecuted(QueueIndex queue, RetryMode mode, TimePoint now)
		{
			const auto found = queues_.find(queue);
			if (found == queues_.end())
			{
				return {};
			}
			Entry& entry = found->second;
			const RetryDecision decision = entry.budget.Next(policy_, mode, now);
			if (decision.kind == RetryDecision::Kind::kResendNow)
			{
				entry.state = HeadState::kPending;
			}
			else if (decision.kind == RetryDecision::Kind::kRetryAfter)
			{
				entry.state = HeadState::kBackoff;
			}
			return decision;
		}

		// 队头拿到最终应答:出队,下一条(若有)成为 kPending 的队头、预算从 now 起算。
		// 返回 true 表示有新队头,调用方负责发送。先把业务回调跑完再发送,回调顺序才等于调用顺序。
		bool CompleteHead(QueueIndex queue, TimePoint now)
		{
			const auto found = queues_.find(queue);
			if (found == queues_.end())
			{
				return false;
			}
			Entry& entry = found->second;
			if (entry.waiting.empty())
			{
				queues_.erase(found);
				return false;
			}
			entry.head = std::move(entry.waiting.front());
			entry.waiting.pop_front();
			--queuedTotal_;
			entry.state = HeadState::kPending;
			entry.budget = HeadRetryBudget(policy_, now);
			return true;
		}

		// 收尾(断连、预算耗尽、停机):取走队头和全部排队命令,按调用顺序返回,队列回到空闲。
		std::vector<Command> Abort(QueueIndex queue)
		{
			std::vector<Command> aborted;
			const auto found = queues_.find(queue);
			if (found == queues_.end())
			{
				return aborted;
			}
			Entry& entry = found->second;
			aborted.reserve(entry.waiting.size() + 1);
			aborted.push_back(std::move(entry.head));
			for (Command& command : entry.waiting)
			{
				aborted.push_back(std::move(command));
			}
			queuedTotal_ -= entry.waiting.size();
			queues_.erase(found);
			return aborted;
		}

		std::vector<QueueIndex> BusyQueues() const
		{
			std::vector<QueueIndex> busy;
			busy.reserve(queues_.size());
			for (const auto& entry : queues_)
			{
				busy.push_back(entry.first);
			}
			return busy;
		}

		std::size_t QueuedTotal() const
		{
			return queuedTotal_;
		}

		std::size_t QueuedIn(QueueIndex queue) const
		{
			const auto found = queues_.find(queue);
			return found == queues_.end() ? 0 : found->second.waiting.size();
		}

	private:
		struct Entry
		{
			Entry(Command command, const RetryPolicy& policy, TimePoint now)
				: head(std::move(command))
				, budget(policy, now)
			{
			}

			Command head;
			HeadState state = HeadState::kPending;
			HeadRetryBudget budget;
			std::deque<Command> waiting;
		};

		RetryPolicy policy_;
		QueueLimits limits_;
		// 只存有命令的队列:一个进程同时忙的 slot 远少于 16384,常驻 16384 个 deque 太浪费。
		std::unordered_map<QueueIndex, Entry> queues_;
		std::size_t queuedTotal_ = 0;
	};
}
