#include "player_ownership_metrics.h"

#include <cstdint>

#include "muduo/base/Logging.h"

#include "engine/core/metrics/metrics_registry.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/player_lifecycle.h"
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
}
