#include "player/system/player_save_metrics.h"

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

#include "muduo/base/Logging.h"

#include "engine/core/metrics/metrics_registry.h"
#include "modules/audit/audit_outbox_stats.h"
#include "player/system/dirty_save_stats.h"

namespace
{
	// 纳秒写成「微秒.一位小数」,不经过浮点格式化。
	void AppendMicros(std::string &line, uint64_t ns)
	{
		const uint64_t tenths = ns / 100;
		line += std::to_string(tenths / 10);
		line += '.';
		line += static_cast<char>('0' + tenths % 10);
	}

	void AppendKey(std::string &line, std::string_view stage, std::string_view suffix)
	{
		line += ' ';
		line += stage;
		line += suffix;
	}

	void CollectPlayerSave(prometheus_text::Writer &writer)
	{
		using save_timing_stats::kStageCount;
		using save_timing_stats::kStageNames;
		const auto timing = save_timing_stats::Read();

		writer.BeginFamily("scene_player_save_stage_observations_total",
						   "Player save stage executions on this scene node since process start, by stage "
						   "(same stages as the [SaveTiming] summary log line).",
						   prometheus_text::MetricType::kCounter);
		for (std::size_t i = 0; i < kStageCount; ++i)
		{
			writer.Sample({{"stage", kStageNames[i]}}, timing.stages[i].count);
		}
		writer.BeginFamily("scene_player_save_stage_duration_ns_total",
						   "Total nanoseconds spent in each player save stage since process start.",
						   prometheus_text::MetricType::kCounter);
		for (std::size_t i = 0; i < kStageCount; ++i)
		{
			writer.Sample({{"stage", kStageNames[i]}}, timing.stages[i].totalNs);
		}
		writer.BeginFamily("scene_player_save_stage_duration_ns_max",
						   "Longest single execution of each player save stage since process start, in nanoseconds.",
						   prometheus_text::MetricType::kGauge);
		for (std::size_t i = 0; i < kStageCount; ++i)
		{
			writer.Sample({{"stage", kStageNames[i]}}, timing.stages[i].maxNs);
		}
		const std::string budgetHelp = "Periodic save timer callbacks that took longer than " +
									   std::to_string(save_timing_stats::kPeriodicTickBudget.count()) +
									   " ms since process start.";
		writer.BeginFamily("scene_player_save_periodic_tick_over_budget_total", budgetHelp,
						   prometheus_text::MetricType::kCounter);
		writer.Sample({}, timing.periodicTickOverBudget);

		const auto saves = dirty_save_stats::Read();
		writer.BeginFamily("scene_player_save_events_total",
						   "Player save attempts on this scene node since process start, by event "
						   "(same counters as the [DirtySave] and [PlayerSave] summary log lines).",
						   prometheus_text::MetricType::kCounter);
		writer.Sample({{"event", "attempt"}}, saves.total);
		writer.Sample({{"event", "nothing_to_write"}}, saves.skipped);
		writer.Sample({{"event", "marshal_refused"}}, saves.marshalRefused);
		// 定时存盘只遇到位置这类只顺带的行有变化、推迟写入(player-save-direct-commit.md §1.1)。
		writer.Sample({{"event", "piggyback_deferred"}}, saves.piggybackDeferred);

		// 审计记录随存盘提交(docs/design/player-audit-outbox.md §5.6)。两个计数健康时都恒 0,告警在 deploy/k8s/audit-outbox-alerts.yaml。
		writer.BeginFamily("scene_audit_outbox_records_dropped_total",
						   "Audit records (transaction logs, snapshot requests) dropped on this scene node since process start, by reason; "
						   "the change each one describes still takes effect and commits with the save, so every drop is a committed change "
						   "without its audit record. Should stay 0.",
						   prometheus_text::MetricType::kCounter);
		for (std::size_t i = 0; i < audit_outbox_stats::kDropReasonCount; ++i)
		{
			writer.Sample({{"reason", audit_outbox_stats::kDropReasonNames[i]}},
						  audit_outbox_stats::ReadDropped(static_cast<audit_outbox_stats::DropReason>(i)));
		}
		writer.BeginFamily("scene_audit_outbox_store_too_old_total",
						   "Saves carrying audit records that committed without the store writing them to the audit outbox "
						   "(the store is older than this scene); the records are kept and sent again with the next save. Should stay 0.",
						   prometheus_text::MetricType::kCounter);
		writer.Sample({}, audit_outbox_stats::ReadStoreTooOld());
	}
} // namespace

std::string player_save_metrics::FormatSaveTimingLine(const save_timing_stats::Snapshot &now,
													  const save_timing_stats::Snapshot &last,
													  const save_timing_stats::WindowMax &windowMaxNs)
{
	using save_timing_stats::kStageCount;
	using save_timing_stats::kStageNames;
	bool observed = false;
	std::string line = "[SaveTiming]";
	for (std::size_t i = 0; i < kStageCount; ++i)
	{
		const uint64_t count = now.stages[i].count - last.stages[i].count;
		const uint64_t totalNs = now.stages[i].totalNs - last.stages[i].totalNs;
		observed = observed || count > 0;
		AppendKey(line, kStageNames[i], "_n=");
		line += std::to_string(count);
		AppendKey(line, kStageNames[i], "_avg_us=");
		AppendMicros(line, count > 0 ? totalNs / count : 0);
		AppendKey(line, kStageNames[i], "_max_us=");
		AppendMicros(line, windowMaxNs[i]);
	}
	line += " periodic_tick_over_budget=";
	line += std::to_string(now.periodicTickOverBudget - last.periodicTickOverBudget);
	return observed ? line : std::string{};
}

void RegisterPlayerSaveMetrics()
{
	if (!GlobalMetricsRegistry().Register("scene_player_save", &CollectPlayerSave))
	{
		LOG_ERROR << "[Metrics] collector scene_player_save is already registered";
	}
}
