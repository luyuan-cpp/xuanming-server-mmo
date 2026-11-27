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
#include "player/system/enter_fence.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_ownership_metrics.h"
#include "player/system/release_fence.h"
#include "player/system/save_seq.h"
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
		"rollback_unconfirmed",
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

// 存盘最终被拒的计数(player_store_refused_stats):kCounterNames 同时是 [PlayerStoreRefused] 汇总行的 key 与指标的 result 标签,
// 告警(deploy/k8s/player-store-alerts.yaml 的 ScenePlayerStoreSaveRefused)按名字选取,改名会让告警静默失效,所以钉死;
// 每个 result 都导出一条样本(0 也导出,increase 才抓得到第一次),Inc 之后对应那一条加一。
TEST(PlayerOwnershipMetrics, PlayerStoreRefusedResultsArePinnedAndExported)
{
	const std::vector<std::string_view> expected = {"too_large", "invalid", "other"};
	std::vector<std::string_view> names;
	for (const char *name : player_store_refused_stats::kCounterNames)
	{
		names.emplace_back(name);
	}
	EXPECT_EQ(names, expected);

	const auto before = RenderAll();
	EXPECT_NE(before.find("# TYPE scene_player_store_save_refused_total counter\n"), std::string::npos);
	for (const auto name : expected)
	{
		const std::string series = std::string("scene_player_store_save_refused_total{result=\"") + std::string(name) + "\"}";
		EXPECT_NE(SampleValue(before, series), UINT64_MAX) << name;
	}

	const std::string tooLarge = "scene_player_store_save_refused_total{result=\"too_large\"}";
	const std::string invalid = "scene_player_store_save_refused_total{result=\"invalid\"}";
	const uint64_t tooLargeBefore = SampleValue(before, tooLarge);
	const uint64_t invalidBefore = SampleValue(before, invalid);
	player_store_refused_stats::Inc(player_store_refused_stats::Counter::kTooLarge);
	const auto after = RenderAll();
	EXPECT_EQ(SampleValue(after, tooLarge), tooLargeBefore + 1);
	EXPECT_EQ(SampleValue(after, invalid), invalidBefore);
}

// 2026-11-27 起退出收尾、断线保留、托管落点看护、两道归属栅栏、存盘发号异常与持久 schema 拒写也经 /metrics 导出。
// key 与各自的汇总行同名(同一个 Visit / 名表),值就是汇总行读的那份计数;0 也导出,increase 才抓得到第一次。
TEST(PlayerOwnershipMetrics, ExitHoldFenceAndSchemaCountersAreExportedUnderTheSummaryLineKeys)
{
	exit_persist_stats::Inc(exit_persist_stats::Get().resave);
	exit_release_stats::Inc(exit_release_stats::Get().written);
	relocate_confirm_stats::Inc(relocate_confirm_stats::Get().hostedResent);
	enter_fence_stats::Inc(enter_fence_stats::Get().bindRejected);
	release_fence_stats::Inc(release_fence_stats::Get().notFound);
	save_seq_stats::Inc(save_seq_stats::Counter::kExhausted);
	persist_schema_stats::IncRegressionRejected();

	const auto text = RenderAll();
	const auto event = [](std::string_view family, std::string_view key)
	{ return std::string(family) + "{event=\"" + std::string(key) + "\"}"; };
	EXPECT_EQ(SampleValue(text, event("scene_exit_persist_events_total", "exit_resave")), exit_persist_stats::Read().resave);
	// 2026-11-27 追加在 [ExitPersist] 行尾的 key:退出存盘超过重连租约才落地(原来只有 "save outran reconnect lease" WARN)。
	EXPECT_EQ(SampleValue(text, event("scene_exit_persist_events_total", "exit_save_outran_lease")),
			  exit_persist_stats::Read().saveOutranLease);
	EXPECT_EQ(SampleValue(text, event("scene_exit_release_events_total", "written")), exit_release_stats::Read().written);
	EXPECT_EQ(SampleValue(text, event("scene_relocate_confirm_events_total", "hosted_resent")),
			  relocate_confirm_stats::Read().hostedResent);
	EXPECT_EQ(SampleValue(text, "scene_persist_schema_regression_rejected_total"),
			  persist_schema_stats::Read().regressionRejected);
	EXPECT_EQ(SampleValue(text, event("scene_save_seq_events_total", "exhausted")),
			  save_seq_stats::Read().values[static_cast<std::size_t>(save_seq_stats::Counter::kExhausted)]);

	// 两道栅栏的每个 key(含带前缀的数组项)都按汇总行的 Visit 导出,值逐项相同。
	enter_fence_stats::Visit(enter_fence_stats::Read(), [&](std::string_view key, uint64_t value)
	{ EXPECT_EQ(SampleValue(text, event("scene_enter_fence_events_total", key)), value) << key; });
	release_fence_stats::Visit(release_fence_stats::Read(), [&](std::string_view key, uint64_t value)
	{ EXPECT_EQ(SampleValue(text, event("scene_release_fence_events_total", key)), value) << key; });
	for (const char *name : exit_release_mark::kExitReleaseDecisionNames)
	{
		EXPECT_NE(SampleValue(text, event("scene_exit_release_events_total", name)), UINT64_MAX) << name;
	}
	for (const char *name : relocate_confirm::kGatePushResultNames)
	{
		EXPECT_NE(SampleValue(text, event("scene_relocate_confirm_events_total", std::string("push_") + name)), UINT64_MAX) << name;
	}

	// 断线保留与托管落点看护是本线程计数:这个用例的线程没走过那些路径,也照样逐项以当前值导出。
	for (const char *key : {"held", "resumed", "expired", "superseded_by_exit", "exit_now", "hosted_arrived", "hosted_relocating"})
	{
		EXPECT_NE(SampleValue(text, event("scene_connection_lost_hold_events_total", key)), UINT64_MAX) << key;
	}
	for (const char *key : {"watched", "replaced", "retried", "landed", "resolved", "gave_up", "untracked"})
	{
		EXPECT_NE(SampleValue(text, event("scene_hosted_placement_watch_events_total", key)), UINT64_MAX) << key;
	}
	EXPECT_EQ(text.find("player_id=\""), std::string::npos) << "玩家号不做标签(AGENTS.md §9)";
}
