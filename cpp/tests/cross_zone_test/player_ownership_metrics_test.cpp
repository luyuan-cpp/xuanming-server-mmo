// 归属交接 / owner_epoch 计数的 Prometheus 导出(player_ownership_metrics.cpp,runbook 观测缺口 G1 / G2 / G3)。
//
// 字段表 travel_handoff_stats::kFields / owner_epoch_stats::kFields 同时决定 [TravelHandoff] / [OwnerEpoch] 汇总行的
// key 与顺序(按位置解析的脚本依赖它),也决定指标的 event 标签。这里把两张表钉死:改名、重排、漏项都先在这里失败。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

#include "engine/core/metrics/metrics_registry.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_ownership_metrics.h"
#include "thread_context/ecs_context.h"
#include "time/system/time.h"

namespace
{
	// 汇总行的历史顺序(runbook §2.3 的字段清单);新 key 只能追加在末尾。
	const std::vector<std::string_view> kTravelHandoffKeys = {
		"started", "started_cross_zone", "granted", "granted_without_reply", "resolved_in_place", "aborted", "exit_wins",
		"save_watchdog_fired", "reply_watchdog_fired", "verify_rearmed", "frozen_ms_total", "frozen_ms_max",
		"withdraw_deferred", "withdraw_expired", "granted_client_reset", "reply_uncorrelated", "reply_unmatched",
		"freeze_cap_reached", "dispatch_window_closed", "mark_sent_destroyed", "mark_sent_client_reset",
		"destroy_deferred_unsettled_save", "freeze_unstamped", "watchdog_early_fire", "handoff_fastpath_forced",
		"rolled_back_adopted", "returned_after_grant", "rollback_receipt_anomaly", "sql_owner_rejected",
	};

	const std::vector<std::string_view> kOwnerEpochKeys = {
		"stale_owner_write_rejected", "stale_owner_rejected_exiting",
	};

	std::string RenderAll()
	{
		RegisterPlayerOwnershipMetrics(); // 重复登记被注册表按名字拒绝,只打一行 ERROR
		return GlobalMetricsRegistry().Render();
	}

	// 取 "name value\n" 这一行的数值;找不到返回 UINT64_MAX。
	uint64_t SampleValue(const std::string &text, const std::string &series)
	{
		const auto needle = series + " ";
		auto at = text.find(needle);
		while (at != std::string::npos && at > 0 && text[at - 1] != '\n')
		{
			at = text.find(needle, at + needle.size());
		}
		if (at == std::string::npos)
		{
			return UINT64_MAX;
		}
		const auto begin = at + series.size() + 1;
		const auto end = text.find('\n', begin);
		return std::stoull(text.substr(begin, end - begin));
	}
} // namespace

TEST(PlayerOwnershipMetrics, TravelHandoffFieldTableIsPinned)
{
	std::vector<std::string_view> keys;
	std::vector<uint64_t travel_handoff_stats::Snapshot::*> members;
	for (const auto &field : travel_handoff_stats::kFields)
	{
		keys.emplace_back(field.key);
		// 成员指针没有大小比较,逐个比相等;两个 key 指向同一个计数成员就漏了另一个成员。
		for (const auto member : members)
		{
			EXPECT_TRUE(member != field.value) << field.key << " 与前面的 key 指向同一个计数成员";
		}
		members.push_back(field.value);
	}
	EXPECT_EQ(keys, kTravelHandoffKeys);
}

TEST(PlayerOwnershipMetrics, OwnerEpochFieldTableIsPinned)
{
	std::vector<std::string_view> keys;
	for (const auto &field : owner_epoch_stats::kFields)
	{
		keys.emplace_back(field.key);
	}
	EXPECT_EQ(keys, kOwnerEpochKeys);
}

// 每个事件 key 都导出为一条 events_total 样本;冻结时长两项不混进 event 标签。
TEST(PlayerOwnershipMetrics, ExportsEveryEventKeyAndOwnerEpochKey)
{
	const auto text = RenderAll();
	EXPECT_NE(text.find("# TYPE scene_travel_handoff_events_total counter\n"), std::string::npos);
	for (const auto &field : travel_handoff_stats::kFields)
	{
		const std::string series = std::string("scene_travel_handoff_events_total{event=\"") + std::string(field.key) + "\"}";
		const bool isEvent = field.kind == travel_handoff_stats::FieldKind::kEvent;
		EXPECT_EQ(text.find(series + " ") != std::string::npos, isEvent) << field.key;
	}
	EXPECT_NE(SampleValue(text, "scene_travel_handoff_frozen_ms_total"), UINT64_MAX);
	EXPECT_NE(SampleValue(text, "scene_travel_handoff_frozen_ms_max"), UINT64_MAX);
	for (const auto &field : owner_epoch_stats::kFields)
	{
		const std::string series = std::string("scene_owner_epoch_events_total{event=\"") + std::string(field.key) + "\"}";
		EXPECT_NE(text.find(series + " "), std::string::npos) << field.key;
	}
}

// G2:此刻冻结中的玩家数与最长冻结时长从 ECS 现读,不靠终态累计。
TEST(PlayerOwnershipMetrics, ReportsCurrentlyFrozenPlayers)
{
	const auto before = RenderAll();
	const uint64_t frozenBefore = SampleValue(before, "scene_travel_handoff_frozen_players");
	ASSERT_NE(frozenBefore, UINT64_MAX);

	const int64_t nowMs = static_cast<int64_t>(TimeSystem::NowMillisecondsUTC());
	const auto older = tlsEcs.actorRegistry.create();
	const auto newer = tlsEcs.actorRegistry.create();
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(older).frozenAtMs = nowMs - 60'000;
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(newer).frozenAtMs = nowMs - 1'000;

	const auto during = RenderAll();
	EXPECT_EQ(SampleValue(during, "scene_travel_handoff_frozen_players"), frozenBefore + 2);
	EXPECT_GE(SampleValue(during, "scene_travel_handoff_oldest_frozen_ms"), 60'000u);

	tlsEcs.actorRegistry.destroy(older);
	tlsEcs.actorRegistry.destroy(newer);
	const auto after = RenderAll();
	EXPECT_EQ(SampleValue(after, "scene_travel_handoff_frozen_players"), frozenBefore);
}
