// scene 两个 Redis 集群句柄(tlsPlayerRedis / tlsControlRedis)路由层计数的 /metrics 导出(2026-11-27 起;
// core/system/redis.cpp 的 WriteRedisClusterMetrics,docs/design/redis-player-store-cluster.md §5.6「可观测」)。
//
// [PlayerRedis] / [ControlRedis] 汇总行与 event 标签取同一组 key(redis.cpp 的 VisitRedisClusterStats)。这里把整行与
// key 序列钉死:改名、重排、漏项都先在这里失败。运维手册按汇总行原文判读,行的写法不能变。

#include <gtest/gtest.h>

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

#include "core/system/redis.h"
#include "engine/core/metrics/metrics_registry.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h"

namespace
{
	// 汇总行的 key 与顺序;新计数只能追加在末尾。
	const std::vector<std::string_view> kRedisClusterKeys = {
		"rejected_by_guard", "rejected_not_connected", "rejected_queue_full", "moved", "ask", "backoff_retries",
		"budget_exhausted", "null_replies", "aborted", "topology_applied", "topology_refresh_failures",
	};

	// 每项取不同的值(按上表顺序 base+1、base+2……),接错字段看得出来。
	RedisClusterStats DistinctStats(uint64_t base)
	{
		RedisClusterStats stats;
		stats.rejectedByGuard = base + 1;
		stats.rejectedNotConnected = base + 2;
		stats.rejectedQueueFull = base + 3;
		stats.movedRedirects = base + 4;
		stats.askRedirects = base + 5;
		stats.backoffRetries = base + 6;
		stats.budgetExhausted = base + 7;
		stats.nullReplies = base + 8;
		stats.abortedCommands = base + 9;
		stats.topologyApplied = base + 10;
		stats.topologyRefreshFailures = base + 11;
		return stats;
	}

	std::string Series(std::string_view client, std::string_view event)
	{
		return "scene_redis_cluster_events_total{client=\"" + std::string(client) + "\",event=\"" + std::string(event) + "\"}";
	}

	std::string Render(const RedisClusterStats &player, const RedisClusterStats &control)
	{
		prometheus_text::Writer writer;
		WriteRedisClusterMetrics(writer, player, control);
		EXPECT_EQ(writer.DroppedSamples(), 0u);
		return writer.Text();
	}
} // namespace

// 两行汇总与原来逐项拼写时逐字相同。
TEST(SceneRedisClusterMetrics, SummaryLinesAreUnchanged)
{
	EXPECT_EQ(FormatRedisClusterStats("[PlayerRedis]", DistinctStats(0)),
			  "[PlayerRedis] rejected_by_guard=1 rejected_not_connected=2 rejected_queue_full=3 moved=4 ask=5 backoff_retries=6 "
			  "budget_exhausted=7 null_replies=8 aborted=9 topology_applied=10 topology_refresh_failures=11");
	EXPECT_EQ(FormatRedisClusterStats("[ControlRedis]", RedisClusterStats{}),
			  "[ControlRedis] rejected_by_guard=0 rejected_not_connected=0 rejected_queue_full=0 moved=0 ask=0 backoff_retries=0 "
			  "budget_exhausted=0 null_replies=0 aborted=0 topology_applied=0 topology_refresh_failures=0");
}

// 每个 client × event 都导出一条,值就是汇总行里同名 key 的值,两个句柄各取各的;没有名单之外的样本。
TEST(SceneRedisClusterMetrics, ExportsEveryKeyForBothClientsWithTheSummaryLineValues)
{
	const std::string text = Render(DistinctStats(0), DistinctStats(100));
	EXPECT_NE(text.find("# TYPE scene_redis_cluster_events_total counter\n"), std::string::npos);
	for (std::size_t i = 0; i < kRedisClusterKeys.size(); ++i)
	{
		const std::string_view key = kRedisClusterKeys[i];
		EXPECT_NE(text.find(Series("player", key) + " " + std::to_string(i + 1) + "\n"), std::string::npos) << key;
		EXPECT_NE(text.find(Series("control", key) + " " + std::to_string(100 + i + 1) + "\n"), std::string::npos) << key;
	}
	// 一行 HELP、一行 TYPE,其余每行一条样本。
	EXPECT_EQ(static_cast<std::size_t>(std::count(text.begin(), text.end(), '\n')), 2 + 2 * kRedisClusterKeys.size()) << text;
}

// 计数全为 0(进程刚起、句柄还没连上)时每一条也照样导出,increase() 才抓得到第一次。
TEST(SceneRedisClusterMetrics, EveryClientAndEventIsExportedFromZero)
{
	const std::string text = Render(RedisClusterStats{}, RedisClusterStats{});
	for (const std::string_view key : kRedisClusterKeys)
	{
		EXPECT_NE(text.find(Series("player", key) + " 0\n"), std::string::npos) << key;
		EXPECT_NE(text.find(Series("control", key) + " 0\n"), std::string::npos) << key;
	}
}

// 登记进全局注册表的采集器读的就是本线程的两个句柄:整页里的这一族与直接拿两个句柄的 stats() 写出的逐字相同。
TEST(SceneRedisClusterMetrics, RegisteredCollectorReadsBothHandles)
{
	RegisterRedisClusterMetrics(); // 重复登记被注册表按名字拒绝,只打一行 ERROR
	const std::string page = GlobalMetricsRegistry().Render();
	const std::string expected = Render(tlsPlayerRedis.stats(), tlsControlRedis.stats());
	EXPECT_NE(page.find(expected), std::string::npos) << page;
}
