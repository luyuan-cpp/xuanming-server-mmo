#pragma once

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <thread>
#include <vector>
#include <cassert>

#include "muduo/base/Logging.h"

#include "core/type_define/type_define.h"

// ID structure based on Snowflake algorithm
// Reference:
// https://github.com/yitter/IdGenerator
// https://github.com/bwmarrin/snowflake

#ifdef ENABLE_SNOWFLAKE_TESTING
constexpr uint64_t kEpoch = 0;
constexpr uint64_t kNodeBits = 9;
constexpr uint64_t kStepBits = 21;
#else
constexpr uint64_t kEpoch = 1773446400; // 2026-03-14 00:00:00 UTC
constexpr uint64_t kNodeBits = 17;
constexpr uint64_t kStepBits = 15;
#endif

// Layout: [time : 32 bits (~136 years, until 2162)] [node : 17 bits (131072)] [step : 15 bits (32768/sec)]
// Each node type assigns node_id independently (0..262143)
// Different generators per purpose (item/buff/session) avoid cross-type collision
// "等真实时钟越过高水位秒"的上限。到点就借下一个逻辑秒继续发号,绝不回写高水位。
//
// 必须按**时间**判定,不能按重试次数:原实现是 `retry > 3000` 配 sleep_for(1ms),
// 而 Windows 默认定时器精度是 15.6ms,同一段代码在 Linux 上约 3 秒、在 Windows 上
// 实测约 42 秒 —— 号称"3 秒有界"的路径在开发机上会把调用线程按住 40 多秒。
constexpr std::chrono::seconds kWaitBudget{3};

constexpr uint64_t kTimeShift = kNodeBits + kStepBits;
constexpr uint64_t kNodeShift = kStepBits;
constexpr uint64_t kStepMask = (1ULL << kStepBits) - 1;
constexpr uint64_t kNodeMask = (1ULL << kNodeBits) - 1;

class SnowFlake
{
public:
	inline void set_node_id(uint32_t node_id)
	{
		if (node_id > kNodeMask) {
			// fail-closed:node 段被截断就会与别的节点撞号,宁可不发号也不发错号。
			LOG_FATAL << "Node ID overflow: max allowed is " << kNodeMask;
			return;
		}
		node_id_ = node_id;
	}

	inline uint64_t node_id() const { return node_id_; }

	inline void set_epoch(uint64_t epoch) { epoch_ = epoch; }

	// 防止与上一个持有同一 node_id 的进程撞号:把 last_time_ 抬到 guard 秒并把该秒的
	// step 池标记为耗尽,于是下一次 Generate() 一定落在 guard 之后的某一秒。
	//
	// guardUtcSeconds 必须是 UTC 秒。早于 epoch_ 的值以前会 uint64 下溢成一个天文数字,
	// 把 last_time_ 顶到远未来 —— 之后所有 ID 的时间段都是错的,且再也不会推进。
	// 这里 fail-closed:非法 guard 直接忽略并打 ERROR,而不是写坏发号器状态。
	inline void SetGuardTime(uint64_t guardUtcSeconds)
	{
		if (guardUtcSeconds < epoch_) {
			LOG_ERROR << "SnowFlake guard time " << guardUtcSeconds
				<< " is before epoch " << epoch_ << "; ignoring guard";
			return;
		}
		uint64_t guardEpoch = guardUtcSeconds - epoch_;
		if (guardEpoch > last_time_) {
			last_time_ = guardEpoch;
			step_ = kStepMask; // 耗尽这一秒,Generate() 只能推进到下一秒
			// 这次"耗尽"是 guard 有意预置的,不是撞到容量墙。标记一下,
			// 让紧随其后的第一次 Generate() 不要误报成容量告警(见 Generate 的耗尽分支)。
			guardPending_ = true;
		}
	}

	// last_time_ 是**高水位**,任何情况下都不允许回退 —— 一旦回退就会把已经发出去的
	// (秒, step) 组合重新发一遍,产生静默重号。时钟回拨/停摆都只影响"ID 里的时间字段
	// 有多接近真实时间",不影响唯一性与单调性。
	Guid Generate()
	{
		const uint64_t now = NowEpoch();

		// guard 预置只影响**紧随其后的第一次**发号,所以标记由第一次 Generate 无条件消化 ——
		// 不能只在耗尽分支里清:若时钟在首次发号前就已跨秒,这里走的是"换新秒"分支,
		// 标记会一直留着,把之后一次**真实**的容量耗尽静音掉。
		const bool guardPending = guardPending_;
		guardPending_ = false;

		if (now > last_time_) {
			clockRollbackLogged_ = false;
			last_time_ = now;
			step_ = 0;
			return ComposeID(last_time_, step_);
		}

		// now <= last_time_:同一秒,或者时钟回拨。两种情况都继续消费 last_time_ 这一秒
		// 剩下的 step 池。回拨时**不**自旋等待:旧实现会把整个 EventLoop 卡住整个回拨
		// 幅度(最坏 3 秒后 LOG_FATAL 直接 abort 掉整个 scene/gate 进程),一次 NTP 回跳
		// 就能把全服 C++ 节点一起打死。
		if (now < last_time_ && !clockRollbackLogged_) {
			clockRollbackLogged_ = true;
			LOG_ERROR << "System clock moved backwards: high-water=" << last_time_
				<< " now=" << now << " (behind by " << (last_time_ - now)
				<< "s); minting from the high-water second, IDs stay unique and monotonic";
		}

		if (step_ < kStepMask) {
			step_ += 1;
			return ComposeID(last_time_, step_);
		}

		// 高水位这一秒的 32768 个 step 用完了,必须等真实时钟越过它。
		// 等待有界;超时也绝不把 last_time_ 往回写,而是借下一个逻辑秒继续发,
		// 保证"停摆的时钟"最多影响时间字段精度,不会破坏唯一性。
		//
		// 耗尽本身必须记 ERROR:它意味着本节点撞到了 32768/s 的容量墙,或处在时钟回拨
		// 窗口内(回拨期间整个窗口共享同一个 step 池,是耗尽的主要放大因素)。
		// clock_behind>0 即可判定是回拨叠加而非纯粹量大,两种成因处置完全不同。
		// 不需要额外限频:耗尽后 last_time_ 必然前进,同一个逻辑秒不可能耗尽两次。
		//
		// ⚠️ 唯一的例外是 SetGuardTime 预置的那一次:guard 特意把 step 池置满,好让启动后的
		// 第一个号必然落到 guard 秒之后。那次"耗尽"是设计动作而非容量问题,**每次进程启动
		// 都会发生**;若也报 ERROR,运维会被训练成忽略这条告警,真正撞容量墙时反而看不见。
		if (!guardPending) {
			LOG_ERROR << "Snowflake step pool exhausted: node_id=" << node_id_
				<< " logical_second=" << last_time_
				<< " cap=" << (kStepMask + 1)
				<< " clock_behind=" << (now < last_time_ ? last_time_ - now : 0) << "s"
				<< "; waiting up to " << kWaitBudget.count()
				<< "s for the clock, then borrowing the next logical second";
		}

		const uint64_t advanced = WaitNextTime(last_time_);
		last_time_ = (advanced > last_time_) ? advanced : (last_time_ + 1);
		step_ = 0;
		return ComposeID(last_time_, step_);
	}

	// 逐个调用 Generate()。旧实现自己维护了一套 step 推进逻辑,而且与 Generate() 对
	// step_ 的语义相反(Generate 里 step_ 是"最后用掉的",批量里当成"下一个可用的"),
	// 混用同一个生成器时批量的第一个 ID 会与上一次 Generate() 完全相同。
	// 该批量接口没有任何生产调用方,这里按 CLAUDE.md §15 收敛成单一实现,
	// 让"同一 Node 内唯一"这条不变量只有一处实现、不可能再漂移。
	std::vector<Guid> GenerateBatch(size_t count)
	{
		std::vector<Guid> ids;
		ids.reserve(count);
		for (size_t i = 0; i < count; ++i) {
			ids.push_back(Generate());
		}
		return ids;
	}

#ifdef ENABLE_SNOWFLAKE_TESTING
	void set_mock_now(uint64_t mock_now)
	{
		mock_now_ = mock_now;
		use_mock_time_ = true;
	}

	// 冻结虚拟时钟(每次读都返回同一个值),用于验证"时钟停摆 / 回拨"路径。
	// 与 SnowFlakeAtomic::set_mock_static_time 对应。
	void set_mock_static_time(uint64_t mock_now)
	{
		mock_now_ = mock_now;
		use_mock_time_ = true;
		static_mock_mode_ = true;
	}
#endif

private:
	uint64_t ComposeID(uint64_t time, uint64_t step)
	{
		return (time << kTimeShift) |
			(static_cast<uint64_t>(node_id_) << kNodeShift) |
			step;
	}

	// 有界等待真实时钟越过 last。返回值可能仍然 <= last(时钟停摆/回拨),
	// 调用方必须自己保证不把 last_time_ 往回写。
	// 这里刻意不再 LOG_FATAL:muduo 的 FATAL 会 abort 进程,等于"时钟抖一下就全服自杀"。
	uint64_t WaitNextTime(uint64_t last)
	{
		uint64_t now = NowEpoch();
		const auto deadline = std::chrono::steady_clock::now() + kWaitBudget;
		while (now <= last) {
			if (std::chrono::steady_clock::now() >= deadline) {
				LOG_ERROR << "System time not advancing within the wait budget (high-water=" << last
					<< ", now=" << now << "); borrowing the next logical second to keep minting";
				break;
			}
			std::this_thread::sleep_for(std::chrono::milliseconds(1));
			now = NowEpoch();
		}
		return now;
	}

	uint64_t NowEpoch()
	{
#ifdef ENABLE_SNOWFLAKE_TESTING
		if (use_mock_time_) {
			if (static_mock_mode_) {
				return mock_now_ - epoch_;
			}
			auto now_epoch = mock_now_++ - epoch_;
			return now_epoch;
		}
#endif

		const int64_t nowSeconds = static_cast<int64_t>(
			std::chrono::duration_cast<std::chrono::seconds>(
				std::chrono::system_clock::now().time_since_epoch())
			.count());

		// 系统时钟早于 epoch(容器时钟没同步、回到 1970)时,uint64 减法会下溢成一个
		// 天文数字,把 ID 的时间段顶到远未来且再也不会推进。钳到 0,让 Generate()
		// 走"回拨"分支从高水位继续发号。
		if (nowSeconds < static_cast<int64_t>(epoch_)) {
			LOG_ERROR << "System clock " << nowSeconds << " is before snowflake epoch " << epoch_
				<< "; clamping to epoch";
			return 0;
		}

		return static_cast<uint64_t>(nowSeconds) - epoch_;
	}

private:
	uint64_t epoch_ = kEpoch;
	uint32_t node_id_ = 0;
	uint64_t last_time_ = 0;
	uint64_t step_ = 0;
	bool clockRollbackLogged_ = false;
	// SetGuardTime 刚把 step 池预置满、且那一次预置尚未被首个 Generate() 消化。
	// 只用来抑制"guard 造成的首次耗尽"的误报,不影响发号语义。
	bool guardPending_ = false;
#ifdef ENABLE_SNOWFLAKE_TESTING
	uint64_t mock_now_ = 0;
	bool use_mock_time_ = false;
	bool static_mock_mode_ = false;
#endif

};

class SnowFlakeAtomic {
public:
	void set_node_id(uint32_t node_id) {
		if (node_id > kNodeMask) {
			LOG_FATAL << "Node ID overflow: max allowed is " << kNodeMask;
			return;
		}
		node_id_ = node_id;
	}

	inline void set_epoch(uint64_t epoch) { epoch_ = epoch; }

#ifdef ENABLE_SNOWFLAKE_TESTING
public:
	void set_mock_static_time(uint64_t t) {
		mock_now_.store(t, std::memory_order_relaxed);
		static_mock_mode_.store(true, std::memory_order_relaxed);
		use_mock_time_.store(true, std::memory_order_relaxed);
	}
#endif


	// 与 SnowFlake::Generate 同一套语义:time_step_ 里的时间段是**高水位**,
	// 只允许前进。时钟回拨不自旋、不 abort,继续消费高水位那一秒的 step 池。
	Guid Generate() {
		while (true) {
			const uint64_t now = NowEpoch();

			uint64_t current = time_step_.load(std::memory_order_relaxed);
			const uint64_t last_time = (current >> kStepBits);
			const uint64_t last_step = (current & kStepMask);

			uint64_t mint_time;
			uint64_t step_to_use;

			if (now > last_time) {
				mint_time = now;
				step_to_use = 0;
			}
			else if (last_step < kStepMask) {
				// 同一秒,或时钟回拨:继续用高水位这一秒
				mint_time = last_time;
				step_to_use = last_step + 1;
			}
			else {
				// 高水位这一秒发满了,等真实时钟越过;等不到就借下一个逻辑秒,
				// 绝不把高水位往回写。
				LogStepPoolExhausted(last_time, now);
				const uint64_t advanced = WaitUntilTimeAdvance(last_time);
				mint_time = (advanced > last_time) ? advanced : (last_time + 1);
				step_to_use = 0;
			}

			const uint64_t next = (mint_time << kStepBits) | step_to_use;
			if (time_step_.compare_exchange_weak(current, next, std::memory_order_acq_rel)) {
				return ComposeID(mint_time, step_to_use);
			}
		}
	}

	std::vector<Guid> GenerateBatch(size_t count) {
		std::vector<Guid> ids;
		ids.reserve(count);

		while (count > 0) {
			const uint64_t now = NowEpoch();

			uint64_t current = time_step_.load(std::memory_order_relaxed);
			const uint64_t last_time = (current >> kStepBits);
			const uint64_t last_step = (current & kStepMask);

			uint64_t mint_time;
			uint64_t step_start;
			uint64_t step_count;

			if (now > last_time) {
				mint_time = now;
				step_start = 0;
				step_count = std::min(static_cast<size_t>(kStepMask + 1), count);
			}
			else if (last_step < kStepMask) {
				mint_time = last_time;
				step_start = last_step + 1;
				step_count = std::min(static_cast<size_t>(kStepMask - last_step), count);
			}
			else {
				LogStepPoolExhausted(last_time, now);
				const uint64_t advanced = WaitUntilTimeAdvance(last_time);
				mint_time = (advanced > last_time) ? advanced : (last_time + 1);
				step_start = 0;
				step_count = std::min(static_cast<size_t>(kStepMask + 1), count);
			}

			const uint64_t next = (mint_time << kStepBits) | (step_start + step_count - 1);
			if (time_step_.compare_exchange_weak(current, next, std::memory_order_acq_rel)) {
				for (uint64_t i = 0; i < step_count; ++i) {
					ids.push_back(ComposeID(mint_time, step_start + i));
				}
				count -= step_count;
			}
		}

		return ids;
	}

private:
	Guid ComposeID(uint64_t time, uint64_t step) {
		return (time << kTimeShift) |
			(static_cast<uint64_t>(node_id_) << kNodeShift) |
			step;
	}

	// 记录"某个逻辑秒的 step 池耗尽"。含义与 SnowFlake::Generate 里那条一致:
	// 撞到 32768/s 容量墙,或处在时钟回拨窗口(整个窗口共享一个 step 池)。
	//
	// 与非原子版不同,这里**必须限频**:耗尽分支位于 CAS 重试循环内,多个线程会同时
	// 撞进来、且 CAS 失败还会重跑,直接打就会按并发度刷屏。用一次 exchange 选出该逻辑秒
	// 的唯一记录者,其余线程静默。+1 是为了让"逻辑秒 0"也能被正常记录一次。
	void LogStepPoolExhausted(uint64_t last_time, uint64_t now) {
		const uint64_t token = last_time + 1;
		if (exhaust_logged_sec_.exchange(token, std::memory_order_relaxed) == token) {
			return;
		}
		LOG_ERROR << "Snowflake step pool exhausted: node_id=" << node_id_
			<< " logical_second=" << last_time
			<< " cap=" << (kStepMask + 1)
			<< " clock_behind=" << (now < last_time ? last_time - now : 0) << "s"
			<< "; waiting up to " << kWaitBudget.count()
			<< "s for the clock, then borrowing the next logical second";
	}

	uint64_t NowEpoch() {
#ifdef ENABLE_SNOWFLAKE_TESTING
		if (use_mock_time_.load(std::memory_order_relaxed)) {
			if (static_mock_mode_.load(std::memory_order_relaxed)) {
				return mock_now_.load(std::memory_order_relaxed) - epoch_;
			}
			return mock_now_.fetch_add(1, std::memory_order_relaxed) - epoch_;
		}
#endif
		const int64_t nowSeconds = static_cast<int64_t>(
			std::chrono::duration_cast<std::chrono::seconds>(
				std::chrono::system_clock::now().time_since_epoch())
			.count());

		// 见 SnowFlake::NowEpoch:早于 epoch 的时钟会下溢成天文数字,钳到 0。
		if (nowSeconds < static_cast<int64_t>(epoch_)) {
			LOG_ERROR << "System clock " << nowSeconds << " is before snowflake epoch " << epoch_
				<< "; clamping to epoch";
			return 0;
		}
		return static_cast<uint64_t>(nowSeconds) - epoch_;
	}


	// 有界等待;返回值可能仍 <= last_time,调用方负责借逻辑秒而不是回写高水位。
	uint64_t WaitUntilTimeAdvance(uint64_t last_time) {
		uint64_t now = NowEpoch();
		const auto deadline = std::chrono::steady_clock::now() + kWaitBudget;
		while (now <= last_time) {
			if (std::chrono::steady_clock::now() >= deadline) {
				LOG_ERROR << "System time not advancing within the wait budget (high-water=" << last_time
					<< ", now=" << now << "); borrowing the next logical second to keep minting";
				break;
			}
			std::this_thread::sleep_for(std::chrono::milliseconds(1));
			now = NowEpoch();
		}
		return now;
	}

private:
	uint64_t epoch_ = kEpoch;

	uint32_t node_id_{ 0 };
	std::atomic<uint64_t> time_step_{ 0 };
	// 已为哪个逻辑秒打过耗尽日志(存的是 logical_second+1,0 表示还没打过)。
	std::atomic<uint64_t> exhaust_logged_sec_{ 0 };

#ifdef ENABLE_SNOWFLAKE_TESTING
	std::atomic<uint64_t> mock_now_{ 0 };
	std::atomic<bool> use_mock_time_{ false };
	std::atomic<bool> static_mock_mode_{ false };
#endif

};


struct SnowFlakeComponents {
	uint64_t timestamp;
	uint64_t node_id;
	uint64_t sequence;
};

inline SnowFlakeComponents ParseGuid(Guid id, uint64_t epoch = kEpoch)
{
	SnowFlakeComponents components;
	components.timestamp = (id >> kTimeShift);
	components.node_id = (id >> kNodeShift) & kNodeMask;
	components.sequence = id & kStepMask;
	return components;
}

inline std::time_t GetRealTimeFromGuid(Guid id, uint64_t epoch = kEpoch)
{
	uint64_t seconds_since_epoch = (id >> kTimeShift);
	return static_cast<std::time_t>(seconds_since_epoch + epoch);
}
