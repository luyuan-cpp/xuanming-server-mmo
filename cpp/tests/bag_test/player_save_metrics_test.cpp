// 存盘分段耗时的计数与两个出口(save_timing_stats.h、player_save_metrics.cpp)。
//
// 计数是进程级的,同一进程里别的用例也会走存盘,所以这里一律比「前后差值」,不比绝对值。
// 段名表同时决定 [SaveTiming] 汇总行的 key 和指标的 stage 标签,stress_summarize.ps1 按名字解析:先在这里钉死。

#include <gtest/gtest.h>

#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

#include "engine/core/metrics/metrics_registry.h"
#include "player/system/player_save_metrics.h"
#include "player/system/save_timing_stats.h"

namespace
{
	using save_timing_stats::Stage;

	save_timing_stats::Clock::duration Ns(int64_t nanoseconds)
	{
		return std::chrono::duration_cast<save_timing_stats::Clock::duration>(std::chrono::nanoseconds(nanoseconds));
	}

	constexpr std::size_t Index(Stage stage)
	{
		return static_cast<std::size_t>(stage);
	}
} // namespace

TEST(SaveTimingStats, StageNamesArePinned)
{
	const std::array<std::string_view, save_timing_stats::kStageCount> expected{
		"marshal", "project", "compare", "submit", "periodic_tick"};
	EXPECT_EQ(save_timing_stats::kStageNames, expected) << "改名或重排要同步改 stress_summarize.ps1 的 [SaveTiming] 解析";
}

TEST(SaveTimingStats, ObserveAddsCountTotalAndMax)
{
	const auto before = save_timing_stats::Read();
	save_timing_stats::Observe(Stage::kMarshal, Ns(1500));
	save_timing_stats::Observe(Stage::kMarshal, Ns(500));
	save_timing_stats::Observe(Stage::kMarshal, Ns(-20)); // 时钟不会倒走;真出现负值按 0 记,不让累计值回绕
	const auto after = save_timing_stats::Read();

	const auto &was = before.stages[Index(Stage::kMarshal)];
	const auto &now = after.stages[Index(Stage::kMarshal)];
	EXPECT_EQ(now.count - was.count, 3u);
	EXPECT_EQ(now.totalNs - was.totalNs, 2000u);
	EXPECT_GE(now.maxNs, 1500u);
	EXPECT_EQ(after.stages[Index(Stage::kSubmit)].count, before.stages[Index(Stage::kSubmit)].count) << "只记到观测的那一段";
}

TEST(SaveTimingStats, WindowMaxIsTakenOnce)
{
	(void)save_timing_stats::TakeWindowMax(); // 清掉别的用例留下的窗口
	save_timing_stats::Observe(Stage::kProject, Ns(700));
	save_timing_stats::Observe(Stage::kProject, Ns(300));

	EXPECT_EQ(save_timing_stats::TakeWindowMax()[Index(Stage::kProject)], 700u);
	EXPECT_EQ(save_timing_stats::TakeWindowMax()[Index(Stage::kProject)], 0u) << "取走之后清零";
	EXPECT_GE(save_timing_stats::Read().stages[Index(Stage::kProject)].maxNs, 700u) << "进程累计的最大值不跟着清";
}

TEST(SaveTimingStats, OnlySlowPeriodicTicksCountAsOverBudget)
{
	const uint64_t before = save_timing_stats::Read().periodicTickOverBudget;
	save_timing_stats::Observe(Stage::kPeriodicTick, save_timing_stats::kPeriodicTickBudget); // 等于预算不算超
	save_timing_stats::Observe(Stage::kPeriodicTick, save_timing_stats::kPeriodicTickBudget + Ns(1));
	save_timing_stats::Observe(Stage::kMarshal, save_timing_stats::kPeriodicTickBudget + Ns(1)); // 别的段不计
	EXPECT_EQ(save_timing_stats::Read().periodicTickOverBudget - before, 1u);
}

TEST(SaveTimingLine, FormatsWindowDeltasInMicros)
{
	save_timing_stats::Snapshot last;
	last.stages[Index(Stage::kMarshal)] = {10, 50000, 9000};
	last.periodicTickOverBudget = 4;

	save_timing_stats::Snapshot now = last;
	now.stages[Index(Stage::kMarshal)] = {12, 53000, 9000}; // 窗口里 2 次、共 3000ns
	now.stages[Index(Stage::kProject)] = {1, 250, 250};
	now.stages[Index(Stage::kSubmit)] = {4, 4000, 1999};
	now.stages[Index(Stage::kPeriodicTick)] = {1, 12345678, 12345678};
	now.periodicTickOverBudget = 5;

	save_timing_stats::WindowMax windowMax{};
	windowMax[Index(Stage::kMarshal)] = 2000;
	windowMax[Index(Stage::kProject)] = 250;
	windowMax[Index(Stage::kSubmit)] = 1999;
	windowMax[Index(Stage::kPeriodicTick)] = 12345678;

	EXPECT_EQ(player_save_metrics::FormatSaveTimingLine(now, last, windowMax),
			  "[SaveTiming]"
			  " marshal_n=2 marshal_avg_us=1.5 marshal_max_us=2.0"
			  " project_n=1 project_avg_us=0.2 project_max_us=0.2"
			  " compare_n=0 compare_avg_us=0.0 compare_max_us=0.0"
			  " submit_n=4 submit_avg_us=1.0 submit_max_us=1.9"
			  " periodic_tick_n=1 periodic_tick_avg_us=12345.6 periodic_tick_max_us=12345.6"
			  " periodic_tick_over_budget=1");
}

TEST(SaveTimingLine, EmptyWhenTheWindowHasNoSave)
{
	save_timing_stats::Snapshot last;
	last.stages[Index(Stage::kMarshal)] = {10, 50000, 9000};
	EXPECT_TRUE(player_save_metrics::FormatSaveTimingLine(last, last, save_timing_stats::WindowMax{}).empty());
}

TEST(PlayerSaveMetrics, RendersEveryStageAndEvent)
{
	RegisterPlayerSaveMetrics(); // 重复登记被注册表按名字拒绝,只打一行 ERROR
	const std::string text = GlobalMetricsRegistry().Render();

	for (const std::string_view family : {"scene_player_save_stage_observations_total",
										  "scene_player_save_stage_duration_ns_total",
										  "scene_player_save_stage_duration_ns_max"})
	{
		for (const std::string_view stage : save_timing_stats::kStageNames)
		{
			const std::string series = std::string(family) + "{stage=\"" + std::string(stage) + "\"} ";
			EXPECT_NE(text.find(series), std::string::npos) << series;
		}
	}
	EXPECT_NE(text.find("\nscene_player_save_periodic_tick_over_budget_total "), std::string::npos);
	for (const std::string_view event : {"attempt", "nothing_to_write", "marshal_refused", "piggyback_deferred"})
	{
		const std::string series = "scene_player_save_events_total{event=\"" + std::string(event) + "\"} ";
		EXPECT_NE(text.find(series), std::string::npos) << series;
	}
}
