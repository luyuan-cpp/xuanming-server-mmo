#include "player_ownership_metrics.h"

#include <cstddef>
#include <cstdint>
#include <string_view>

#include "muduo/base/Logging.h"

#include "engine/core/metrics/metrics_registry.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/enter_fence.h"
#include "player/system/player_lifecycle.h"
#include "player/system/release_fence.h"
#include "player/system/save_seq.h"
#include "thread_context/ecs_context.h"
#include "time/system/time.h"

namespace
{
	void CollectTravelHandoff(prometheus_text::Writer &writer)
	{
		const auto stats = travel_handoff_stats::Read();
		writer.BeginFamily("scene_travel_handoff_events_total",
						   "Ownership handoff events on this scene node since process start, by event "
						   "(same keys as the [TravelHandoff] summary log line).",
						   prometheus_text::MetricType::kCounter);
		for (const auto &field : travel_handoff_stats::kFields)
		{
			if (field.kind == travel_handoff_stats::FieldKind::kEvent)
			{
				writer.Sample({{"event", field.key}}, stats.*field.value);
			}
		}
		writer.BeginFamily("scene_travel_handoff_frozen_ms_total",
						   "Total milliseconds players were frozen by handoffs that reached a terminal state.",
						   prometheus_text::MetricType::kCounter);
		writer.Sample({}, stats.frozenMsTotal);
		writer.BeginFamily("scene_travel_handoff_frozen_ms_max",
						   "Longest single handoff freeze in milliseconds since process start.",
						   prometheus_text::MetricType::kGauge);
		writer.Sample({}, stats.frozenMsMax);

		// G2:此刻仍冻结着的玩家。只有终态才进上面的累计值,一直冻着的玩家只能从这里看到。
		// 没盖冻结起点的组件(frozenAtMs <= 0,对应 freeze_unstamped)只计人数,不参与最长时长 —— 它的年龄无从算起。
		uint64_t frozenPlayers = 0;
		bool anyStamped = false;
		int64_t oldestFrozenAtMs = 0;
		for (const auto &[entity, frozen] : tlsEcs.actorRegistry.view<PlayerFrozenComp>().each())
		{
			(void)entity;
			++frozenPlayers;
			if (frozen.frozenAtMs > 0 && (!anyStamped || frozen.frozenAtMs < oldestFrozenAtMs))
			{
				anyStamped = true;
				oldestFrozenAtMs = frozen.frozenAtMs;
			}
		}
		const int64_t nowMs = static_cast<int64_t>(TimeSystem::NowMillisecondsUTC());
		// 墙钟回拨让差值为负时按 0 报,与 ObserveTravelHandoffEnded 的处理一致。
		const uint64_t oldestFrozenMs =
			(anyStamped && nowMs > oldestFrozenAtMs) ? static_cast<uint64_t>(nowMs - oldestFrozenAtMs) : 0;
		writer.BeginFamily("scene_travel_handoff_frozen_players",
						   "Players currently frozen by an in-flight ownership handoff on this scene node.",
						   prometheus_text::MetricType::kGauge);
		writer.Sample({}, frozenPlayers);
		writer.BeginFamily("scene_travel_handoff_oldest_frozen_ms",
						   "Milliseconds the longest-frozen player on this scene node has been frozen (0 when none).",
						   prometheus_text::MetricType::kGauge);
		writer.Sample({}, oldestFrozenMs);
	}

	void CollectOwnerEpoch(prometheus_text::Writer &writer)
	{
		const auto stats = owner_epoch_stats::Read();
		writer.BeginFamily("scene_owner_epoch_events_total",
						   "Ownership-epoch health events on this scene node since process start, by event "
						   "(same keys as the [OwnerEpoch] summary log line); every one should stay 0.",
						   prometheus_text::MetricType::kCounter);
		for (const auto &field : owner_epoch_stats::kFields)
		{
			writer.Sample({{"event", field.key}}, stats.*field.value);
		}
	}

	// 存盘最终被确定性拒绝(player_store_refused_stats)。被拒的玩家之后每次存盘都会再被拒、退出时回到上一次提交;
	// 超过传输上限的那一笔 scene 发送前就拒掉,go/db 的 db_player_store_requests_total 看不到,只有这里有。
	void CollectPlayerStoreRefused(prometheus_text::Writer &writer)
	{
		const auto stats = player_store_refused_stats::Read();
		writer.BeginFamily("scene_player_store_save_refused_total",
						   "Player saves finally refused by a deterministic player store verdict on this scene node since process start, "
						   "by result (same keys as the [PlayerStoreRefused] summary log line); every one should stay 0. "
						   "too_large includes saves refused locally before sending.",
						   prometheus_text::MetricType::kCounter);
		for (std::size_t i = 0; i < stats.values.size(); ++i)
		{
			writer.Sample({{"result", player_store_refused_stats::kCounterNames[i]}}, stats.values[i]);
		}
	}

	// 持久 schema 世代倒退被拒写(persist_schema_stats)。被拒的玩家本次在线进度不落盘;健康时恒 0。
	void CollectPersistSchema(prometheus_text::Writer &writer)
	{
		writer.BeginFamily("scene_persist_schema_regression_rejected_total",
						   "Player saves on this scene node since process start that the player store refused because their "
						   "persistent schema generation went backwards (same counter as the [PersistSchema] summary log line); "
						   "should stay 0.",
						   prometheus_text::MetricType::kCounter);
		writer.Sample({}, persist_schema_stats::Read().regressionRejected);
	}

	// 入场 / 释放归属栅栏的结论与存盘发号异常(2026-11-27 起导出,原来只有 [EnterFence] / [ReleaseFence] / [SaveSeq] 汇总行)。
	// key 取自各自的 Visit / kCounterNames,与汇总行同一份;各项含义见 enter_fence.h、release_fence.h、save_seq.h。
	void CollectFencesAndSaveSeq(prometheus_text::Writer &writer)
	{
		const auto sampleEvent = [&writer](std::string_view key, uint64_t value) { writer.Sample({{"event", key}}, value); };
		writer.BeginFamily("scene_enter_fence_events_total",
						   "Enter session fence verdicts and rejections on this scene node since process start, by event "
						   "(same keys as the [EnterFence] summary log line).",
						   prometheus_text::MetricType::kCounter);
		enter_fence_stats::Visit(enter_fence_stats::Read(), sampleEvent);
		writer.BeginFamily("scene_release_fence_events_total",
						   "Release fence verdicts and session-ended release results on this scene node since process start, by event "
						   "(same keys as the [ReleaseFence] summary log line).",
						   prometheus_text::MetricType::kCounter);
		release_fence_stats::Visit(release_fence_stats::Read(), sampleEvent);
		const auto saveSeq = save_seq_stats::Read();
		writer.BeginFamily("scene_save_seq_events_total",
						   "Save sequence anomalies on this scene node since process start, by event (same keys as the [SaveSeq] "
						   "summary log line); every one should stay 0.",
						   prometheus_text::MetricType::kCounter);
		for (std::size_t i = 0; i < saveSeq.values.size(); ++i)
		{
			sampleEvent(save_seq_stats::kCounterNames[i], saveSeq.values[i]);
		}
	}
} // namespace

void RegisterPlayerOwnershipMetrics()
{
	auto &registry = GlobalMetricsRegistry();
	if (!registry.Register("scene_travel_handoff", &CollectTravelHandoff))
	{
		LOG_ERROR << "[Metrics] collector scene_travel_handoff is already registered";
	}
	if (!registry.Register("scene_owner_epoch", &CollectOwnerEpoch))
	{
		LOG_ERROR << "[Metrics] collector scene_owner_epoch is already registered";
	}
	if (!registry.Register("scene_player_store_refused", &CollectPlayerStoreRefused))
	{
		LOG_ERROR << "[Metrics] collector scene_player_store_refused is already registered";
	}
	if (!registry.Register("scene_persist_schema", &CollectPersistSchema))
	{
		LOG_ERROR << "[Metrics] collector scene_persist_schema is already registered";
	}
	if (!registry.Register("scene_player_exit", &WritePlayerExitMetrics))
	{
		LOG_ERROR << "[Metrics] collector scene_player_exit is already registered";
	}
	if (!registry.Register("scene_fences_and_save_seq", &CollectFencesAndSaveSeq))
	{
		LOG_ERROR << "[Metrics] collector scene_fences_and_save_seq is already registered";
	}
}
