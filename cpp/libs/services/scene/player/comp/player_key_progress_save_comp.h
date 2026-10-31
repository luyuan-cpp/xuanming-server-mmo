#pragma once

#include <chrono>

// 关键进度存盘的节流状态(docs/design/player-data-tidb-scaling-plan.md §4)。
//
// 按需状态:
//   创建:唯一入口是 PlayerLifecycleSystem::RequestKeyProgressSave(get_or_emplace)。
//   读写:同一个函数和它挂的定时器,定时器按 Guid 回查实体后 try_get。
// 纯运行时:不入 PlayerAllData、不序列化,随实体销毁回收。
struct PlayerKeyProgressSaveComp
{
	using Clock = std::chrono::steady_clock;

	// 上一次由本机制触发存盘的时刻。默认值是时钟纪元,第一次请求不等待。
	Clock::time_point lastSaveAt{};
	// 已经挂了定时器:这期间的变化由到期时的那次存盘带上,不再挂。
	bool armed{false};
};

namespace key_progress_save
{
	// 窗口内的多次变化合并成一次存盘。
	inline constexpr std::chrono::seconds kWindow{1};

	// 距离可以再存一次还要等多久。节流的全部规则,写成纯函数单测不用等墙钟。
	inline PlayerKeyProgressSaveComp::Clock::duration DelayUntilNextSave(PlayerKeyProgressSaveComp::Clock::time_point lastSaveAt,
																		 PlayerKeyProgressSaveComp::Clock::time_point now)
	{
		const auto elapsed = now - lastSaveAt;
		if (elapsed >= kWindow)
		{
			return PlayerKeyProgressSaveComp::Clock::duration::zero();
		}
		return std::chrono::duration_cast<PlayerKeyProgressSaveComp::Clock::duration>(kWindow) - elapsed;
	}
} // namespace key_progress_save
