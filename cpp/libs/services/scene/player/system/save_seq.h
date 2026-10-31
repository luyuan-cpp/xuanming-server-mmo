#pragma once

#include <algorithm>
#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <limits>
#include <optional>

// 玩家存盘序号 save_seq 的发号(docs/design/db-write-fencing-mysql.md §7.1–§7.3)。
//
// 用途:SavePlayerRequest.save_seq 与 PlayerAllData.last_save_seq。go/db 按 (owner_epoch, save_seq) 判定写权和幂等,
// 所以同一玩家后发出的存盘必须拿到更大的 save_seq —— 不管中间实体重建过几次、
// scene 进程是否换代(§7.1)。
//
// 形状:进程级发号器 SaveSeqAllocator(生产实例是 tlsIdGeneratorManager.saveSeq,scene 逻辑线程独占),不设每玩家计数
// (每实体计数在实体重建、进程换代后会复用或倒退,§0.2 第 16 行、§15)。Next = max(last + 1, nowMs << 10):last 让本进程
// 内严格递增,墙钟下限让崩溃重启后的新进程越过旧进程已发出、但种子里看不到的序号(§7.4)。各玩家共用一条数轴,
// 每人看到的是有空洞的递增序列,围栏只要求递增、不要求连续。
//
// 播种:加载玩家时 SaveSeqFloorFrom(三路 max,见 save_seq_floor.h)经 Observe 进发号器。Observe 只采纳不超过「(nowMs + 60s) << 10」的种子;
// 超界的种子不抬进程水位,只作为该玩家实体上的个人下限(PlayerSaveSeqFloorComp + ApplyPersonalFloor),不污染其他玩家,
// 也不让这名玩家丢盘(§7.2)。
//
// 本头的发号部分只有值类型与纯函数,时间一律由调用方传入,便于单测;不读墙钟、不碰 ECS、不打日志,也不依赖 protobuf
// (它经 core/system/id_generator.h 被大量编译单元引入;读 PlayerAllData 的种子函数在 save_seq_floor.h)。
// 末尾的 save_seq_stats 是进程级计数(relaxed atomic),由调用方在拒绝 / 超界时递增。
namespace save_seq
{
	// 墙钟毫秒左移的位数:同一毫秒内留 2^10 = 1024 个号。nowMs << 10 约为 2^51,离 uint64 上限很远。
	inline constexpr unsigned kWallClockShift = 10;

	// Observe 的上界容差:种子超过「(nowMs + 容差) << 10」即判超界。
	// 来源:docs/design/db-write-fencing-mysql.md §14 第 13 条(已定,60 秒)。它远大于正常 NTP 偏差(毫秒级);
	// 容差只决定一个种子能不能抬高整个进程的水位,超界的种子由个人下限兜住、不丢数据,所以越小越安全(§7.2)。
	// 设计要求写进 scene 配置作缺省值;配置键尚未落码,先用常量(交付说明里记为未完成项)。
	inline constexpr uint64_t kObserveToleranceMs = 60'000;

	inline constexpr uint64_t kMaxSaveSeq = std::numeric_limits<uint64_t>::max();

	// nowMs << kWallClockShift。左移会溢出时饱和到 kMaxSaveSeq(墙钟坏到约公元 57 万年才会发生),不回绕成小值。
	constexpr uint64_t WallClockFloor(uint64_t nowMs)
	{
		return nowMs > (kMaxSaveSeq >> kWallClockShift) ? kMaxSaveSeq : (nowMs << kWallClockShift);
	}

	// Observe 能采纳的最大种子:(nowMs + kObserveToleranceMs) << kWallClockShift,加法与左移都饱和。
	constexpr uint64_t ObserveUpperBound(uint64_t nowMs)
	{
		const uint64_t boundMs =
			nowMs > kMaxSaveSeq - kObserveToleranceMs ? kMaxSaveSeq : nowMs + kObserveToleranceMs;
		return WallClockFloor(boundMs);
	}

	// 下一个序号:max(last + 1, nowMs << 10)。last 已是 kMaxSaveSeq 时返回 nullopt —— 显式失败,不回绕
	// (回绕成 0 会退回墙钟,同 epoch 内之后的存盘全被 go/db 判 older_seq,§7.2)。
	constexpr std::optional<uint64_t> NextSaveSeq(uint64_t last, uint64_t nowMs)
	{
		if (last == kMaxSaveSeq)
		{
			return std::nullopt;
		}
		return std::max(last + 1, WallClockFloor(nowMs));
	}

	// 个人下限:max(globalSeq, personalFloor + 1)。personalFloor 已是 kMaxSaveSeq 时返回 nullopt(显式失败,不回绕)。
	constexpr std::optional<uint64_t> ApplyPersonalFloor(uint64_t globalSeq, uint64_t personalFloor)
	{
		if (personalFloor == kMaxSaveSeq)
		{
			return std::nullopt;
		}
		return std::max(globalSeq, personalFloor + 1);
	}

	enum class ObserveResult : uint8_t
	{
		kAdopted,     // 种子不超界:进程水位取 max(水位, 种子)
		kBeyondBound, // 种子超界:进程水位不变,调用方把种子挂成该玩家的个人下限

		kCount
	};

	// 进程级发号器。普通值类型,不是单例:生产实例是 ThreadLocalIdGeneratorManager::saveSeq(core/system/id_generator.h,
	// scene 逻辑线程独占、不需要锁;不在 tlsEcs 里,tlsEcs.Clear() 不触及它),单测各自构造局部实例。
	class SaveSeqAllocator
	{
	public:
		// 发一个号并推进水位;饱和时返回 nullopt、水位不变。
		std::optional<uint64_t> Next(uint64_t nowMs)
		{
			const auto next = NextSaveSeq(last_, nowMs);
			if (next)
			{
				last_ = *next;
			}
			return next;
		}

		// 用加载到的种子抬高水位。超过 ObserveUpperBound(nowMs) 的种子不采纳、返回 kBeyondBound:一行来自导入或手工改库的
		// 近 2^64 围栏、或一台时钟超前的主机发出的序号,不能把整个进程的水位抬到墙钟下限失效的量级(§7.2「为什么要上界」)。
		ObserveResult Observe(uint64_t seed, uint64_t nowMs)
		{
			if (seed > ObserveUpperBound(nowMs))
			{
				return ObserveResult::kBeyondBound;
			}
			last_ = std::max(last_, seed);
			return ObserveResult::kAdopted;
		}

		// 当前水位:最近一次发出或采纳的值;0 = 尚未发号也未采纳。只供日志与单测。
		uint64_t Current() const { return last_; }

	private:
		uint64_t last_{0};
	};
} // namespace save_seq

// 发号异常计数。与 owner_epoch_stats 同一套写法(relaxed atomic、进程启动以来的累计值、不接 Prometheus、不带玩家维度),
// 由 RedisSystem 的 30s 快照定时器在任一非 0 时打成一行 [SaveSeq](WARN)。三项正常都应恒为 0:
//   observe_beyond_bound   加载时种子超界、给该玩家挂了个人下限的次数。非 0 = 有坏数据或某台 scene 主机时钟超前;
//   personal_floor_issued  按个人下限发号的存盘次数(挂着个人下限的玩家每次真写盘各计一次);
//   exhausted              发号饱和、拒绝存盘的次数(进程水位或个人下限到了 UINT64_MAX)。
namespace save_seq_stats
{
	enum class Counter : uint8_t
	{
		kObserveBeyondBound,
		kPersonalFloorIssued,
		kExhausted,

		kCount
	};

	inline constexpr std::size_t kCounterCount = static_cast<std::size_t>(Counter::kCount);

	inline constexpr const char *kCounterNames[] = {
		"observe_beyond_bound",
		"personal_floor_issued",
		"exhausted",
	};
	static_assert(std::size(kCounterNames) == kCounterCount, "kCounterNames 必须与 Counter 一一对应");

	inline std::atomic<uint64_t> &Slot(Counter counter)
	{
		static std::atomic<uint64_t> g_counters[kCounterCount]{};
		return g_counters[static_cast<std::size_t>(counter)];
	}

	inline void Inc(Counter counter) { Slot(counter).fetch_add(1, std::memory_order_relaxed); }

	struct Snapshot
	{
		std::array<uint64_t, kCounterCount> values{};

		bool Any() const
		{
			for (const uint64_t value : values)
			{
				if (value != 0)
				{
					return true;
				}
			}
			return false;
		}
	};

	inline Snapshot Read()
	{
		Snapshot snapshot;
		for (std::size_t i = 0; i < kCounterCount; ++i)
		{
			snapshot.values[i] = Slot(static_cast<Counter>(i)).load(std::memory_order_relaxed);
		}
		return snapshot;
	}
} // namespace save_seq_stats
