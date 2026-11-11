// C++ 节点 Prometheus 出口的纯逻辑单测:文本格式写出器、端口环境变量解析、注册表。
// HTTP 收发由 muduo HttpServer 负责,出口只把 render() 的文本原样作为 200 应答体,这里不起监听。

#include <gtest/gtest.h>

#include <array>
#include <cstdint>
#include <string>

#include "metrics/metrics_http_exporter.h"
#include "metrics/metrics_registry.h"
#include "metrics/prometheus_text.h"
#include "network/broadcast_fanout_stats.h"
#include "node/system/node/node_command_consume_stats.h"

namespace
{
	TEST(PrometheusText, WritesFamilyHeaderAndSamples)
	{
		prometheus_text::Writer writer;
		ASSERT_TRUE(writer.BeginFamily("demo_events_total", "Demo events.", prometheus_text::MetricType::kCounter));
		writer.Sample({{"event", "started"}}, 3);
		writer.Sample({{"event", "aborted"}, {"zone_id", "1"}}, 0);
		ASSERT_TRUE(writer.BeginFamily("demo_players", "Demo gauge.", prometheus_text::MetricType::kGauge));
		writer.Sample({}, 7);
		EXPECT_EQ(writer.Text(),
				  "# HELP demo_events_total Demo events.\n"
				  "# TYPE demo_events_total counter\n"
				  "demo_events_total{event=\"started\"} 3\n"
				  "demo_events_total{event=\"aborted\",zone_id=\"1\"} 0\n"
				  "# HELP demo_players Demo gauge.\n"
				  "# TYPE demo_players gauge\n"
				  "demo_players 7\n");
		EXPECT_EQ(writer.DroppedSamples(), 0u);
	}

	TEST(PrometheusText, EscapesHelpAndLabelValues)
	{
		prometheus_text::Writer writer;
		ASSERT_TRUE(writer.BeginFamily("demo", "line1\nback\\slash", prometheus_text::MetricType::kGauge));
		writer.Sample({{"path", "a\"b\\c\nd"}}, 1);
		EXPECT_EQ(writer.Text(),
				  "# HELP demo line1\\nback\\\\slash\n"
				  "# TYPE demo gauge\n"
				  "demo{path=\"a\\\"b\\\\c\\nd\"} 1\n");
	}

	TEST(PrometheusText, DropsSamplesOfInvalidFamiliesAndLabels)
	{
		prometheus_text::Writer writer;
		EXPECT_FALSE(writer.BeginFamily("9starts_with_digit", "x", prometheus_text::MetricType::kGauge));
		writer.Sample({}, 1);
		ASSERT_TRUE(writer.BeginFamily("ok_metric", "x", prometheus_text::MetricType::kGauge));
		writer.Sample({{"__reserved", "v"}}, 1);
		writer.Sample({{"bad-name", "v"}}, 1);
		writer.Sample({{"good", "v"}}, 2);
		EXPECT_EQ(writer.DroppedSamples(), 3u);
		EXPECT_EQ(writer.Text(), "# HELP ok_metric x\n# TYPE ok_metric gauge\nok_metric{good=\"v\"} 2\n");
	}

	// 直方图:桶计数按上界累计,+Inf 桶与 _count 都是总数,_sum 原样写出。
	TEST(PrometheusText, WritesHistogramSeries)
	{
		prometheus_text::Writer writer;
		ASSERT_TRUE(writer.BeginFamily("demo_fanout", "Demo histogram.", prometheus_text::MetricType::kHistogram));
		const std::array<uint64_t, 2> bounds{1, 4};
		const std::array<uint64_t, 3> counts{5, 2, 1}; // ≤1:5 次;(1,4]:2 次;>4:1 次
		writer.Histogram({{"kind", "players"}}, bounds, counts, 17);
		EXPECT_EQ(writer.Text(),
				  "# HELP demo_fanout Demo histogram.\n"
				  "# TYPE demo_fanout histogram\n"
				  "demo_fanout_bucket{kind=\"players\",le=\"1\"} 5\n"
				  "demo_fanout_bucket{kind=\"players\",le=\"4\"} 7\n"
				  "demo_fanout_bucket{kind=\"players\",le=\"+Inf\"} 8\n"
				  "demo_fanout_sum{kind=\"players\"} 17\n"
				  "demo_fanout_count{kind=\"players\"} 8\n");
		EXPECT_EQ(writer.DroppedSamples(), 0u);
	}

	// 直方图写错就整组不写:桶数不匹配、上界不升序、公共标签带 le、写进非直方图族;直方图族也不收普通样本。
	TEST(PrometheusText, DropsMalformedHistograms)
	{
		prometheus_text::Writer writer;
		ASSERT_TRUE(writer.BeginFamily("demo_fanout", "x", prometheus_text::MetricType::kHistogram));
		const std::array<uint64_t, 2> bounds{1, 4};
		const std::array<uint64_t, 2> tooFewCounts{1, 2};
		const std::array<uint64_t, 2> unsortedBounds{4, 1};
		const std::array<uint64_t, 3> counts{1, 2, 3};
		writer.Histogram({}, bounds, tooFewCounts, 0);
		writer.Histogram({}, unsortedBounds, counts, 0);
		writer.Histogram({{"le", "5"}}, bounds, counts, 0);
		writer.Sample({}, 1);
		ASSERT_TRUE(writer.BeginFamily("demo_gauge", "x", prometheus_text::MetricType::kGauge));
		writer.Histogram({}, bounds, counts, 0);
		EXPECT_EQ(writer.DroppedSamples(), 5u);
		EXPECT_EQ(writer.Text(),
				  "# HELP demo_fanout x\n# TYPE demo_fanout histogram\n"
				  "# HELP demo_gauge x\n# TYPE demo_gauge gauge\n");
	}

	// 广播扇出的桶:第一个不小于 gate 数的上界;超过 64 台进溢出桶。
	TEST(BroadcastFanoutStats, BucketIndexUsesFirstUpperBoundNotBelow)
	{
		EXPECT_EQ(broadcast_fanout_stats::BucketIndex(1), 0u);
		EXPECT_EQ(broadcast_fanout_stats::BucketIndex(2), 1u);
		EXPECT_EQ(broadcast_fanout_stats::BucketIndex(3), 2u);
		EXPECT_EQ(broadcast_fanout_stats::BucketIndex(4), 2u);
		EXPECT_EQ(broadcast_fanout_stats::BucketIndex(64), 6u);
		EXPECT_EQ(broadcast_fanout_stats::BucketIndex(65), broadcast_fanout_stats::kBucketCount - 1);
	}

	// 只记至少发给了一台 gate 的广播;两种广播各一组直方图序列。计数是进程级的,按增量断言,不依赖测试顺序。
	TEST(BroadcastFanoutStats, RecordsNonEmptyBroadcastsPerKind)
	{
		using broadcast_fanout_stats::Kind;
		auto &players = broadcast_fanout_stats::Counters()[static_cast<size_t>(Kind::kPlayers)];
		auto &scene = broadcast_fanout_stats::Counters()[static_cast<size_t>(Kind::kScene)];
		const uint64_t playersBucketBefore = players.buckets[2].load();
		const uint64_t playersSumBefore = players.gateSends.load();
		const uint64_t sceneOverflowBefore = scene.buckets[broadcast_fanout_stats::kBucketCount - 1].load();

		broadcast_fanout_stats::Record(Kind::kPlayers, 3);
		broadcast_fanout_stats::Record(Kind::kPlayers, 0);
		broadcast_fanout_stats::Record(Kind::kScene, 100);

		EXPECT_EQ(players.buckets[2].load(), playersBucketBefore + 1) << "3 台落进 le=4 的桶";
		EXPECT_EQ(players.gateSends.load(), playersSumBefore + 3) << "0 台的广播不记";
		EXPECT_EQ(scene.buckets[broadcast_fanout_stats::kBucketCount - 1].load(), sceneOverflowBefore + 1);

		prometheus_text::Writer writer;
		broadcast_fanout_stats::Collect(writer);
		EXPECT_EQ(writer.DroppedSamples(), 0u);
		const auto &text = writer.Text();
		EXPECT_NE(text.find("# TYPE scene_broadcast_gate_fanout histogram\n"), std::string::npos);
		EXPECT_NE(text.find("scene_broadcast_gate_fanout_bucket{kind=\"players\",le=\"1\"} "), std::string::npos);
		EXPECT_NE(text.find("scene_broadcast_gate_fanout_bucket{kind=\"scene\",le=\"+Inf\"} "), std::string::npos);
		EXPECT_NE(text.find("scene_broadcast_gate_fanout_count{kind=\"scene\"} "), std::string::npos);
	}

	// 命令 topic 读放大计数:同一 (节点类型, topic) 只有一份计数,own 与 foreign 各一条序列。
	TEST(CommandConsumeStats, SameTopicSharesOneSeriesAndRendersOwnAndForeign)
	{
		node::kafka::CommandConsumeStats stats;
		auto &gateCommands = stats.CountersFor("gate", "gate-cmd");
		EXPECT_EQ(&gateCommands, &stats.CountersFor("gate", "gate-cmd")) << "同一 topic 注册两次也只有一条序列";
		auto &worldChat = stats.CountersFor("gate", "chat-world-bcast");
		EXPECT_NE(&gateCommands, &worldChat);
		gateCommands.own.fetch_add(2);
		gateCommands.foreign.fetch_add(5);
		worldChat.own.fetch_add(1);

		prometheus_text::Writer writer;
		stats.Collect(writer);
		EXPECT_EQ(writer.DroppedSamples(), 0u);
		EXPECT_EQ(writer.Text().substr(writer.Text().find("# TYPE")),
				  "# TYPE node_command_consumed_total counter\n"
				  "node_command_consumed_total{node_type=\"gate\",topic=\"gate-cmd\",target=\"own\"} 2\n"
				  "node_command_consumed_total{node_type=\"gate\",topic=\"gate-cmd\",target=\"foreign\"} 5\n"
				  "node_command_consumed_total{node_type=\"gate\",topic=\"chat-world-bcast\",target=\"own\"} 1\n"
				  "node_command_consumed_total{node_type=\"gate\",topic=\"chat-world-bcast\",target=\"foreign\"} 0\n");
	}

	TEST(PrometheusText, NameValidation)
	{
		EXPECT_TRUE(prometheus_text::IsValidMetricName("scene_travel_handoff_events_total"));
		EXPECT_TRUE(prometheus_text::IsValidMetricName("a:b_c9"));
		EXPECT_FALSE(prometheus_text::IsValidMetricName(""));
		EXPECT_FALSE(prometheus_text::IsValidMetricName("1abc"));
		EXPECT_FALSE(prometheus_text::IsValidMetricName("a-b"));
		EXPECT_TRUE(prometheus_text::IsValidLabelName("event"));
		EXPECT_TRUE(prometheus_text::IsValidLabelName("_x9"));
		EXPECT_FALSE(prometheus_text::IsValidLabelName("__name__"));
		EXPECT_FALSE(prometheus_text::IsValidLabelName("a:b"));
		EXPECT_FALSE(prometheus_text::IsValidLabelName("9a"));
	}

	TEST(MetricsPortEnv, ParsesOnlyCanonicalPorts)
	{
		EXPECT_EQ(ParseMetricsPortEnv(nullptr), std::optional<uint16_t>(0));
		EXPECT_EQ(ParseMetricsPortEnv(""), std::optional<uint16_t>(0));
		EXPECT_EQ(ParseMetricsPortEnv("9400"), std::optional<uint16_t>(9400));
		EXPECT_EQ(ParseMetricsPortEnv("1"), std::optional<uint16_t>(1));
		EXPECT_EQ(ParseMetricsPortEnv("65535"), std::optional<uint16_t>(65535));
		for (const char *bad : {"0", "65536", "09400", " 9400", "9400 ", "94a0", "123456", "-1"})
		{
			EXPECT_FALSE(ParseMetricsPortEnv(bad).has_value()) << bad;
		}
	}

	TEST(MetricsRegistry, RendersInRegistrationOrderAndRejectsDuplicates)
	{
		MetricsRegistry registry;
		EXPECT_TRUE(registry.Register("first", [](prometheus_text::Writer &writer)
		{
			writer.BeginFamily("first_metric", "first", prometheus_text::MetricType::kGauge);
			writer.Sample({}, 1);
		}));
		EXPECT_TRUE(registry.Register("second", [](prometheus_text::Writer &writer)
		{
			writer.BeginFamily("second_metric", "second", prometheus_text::MetricType::kGauge);
			writer.Sample({}, 2);
		}));
		EXPECT_FALSE(registry.Register("first", [](prometheus_text::Writer &) {})) << "同名重复登记必须拒绝";
		EXPECT_FALSE(registry.Register("empty", MetricsRegistry::Collector{})) << "空回调拒绝";
		const auto text = registry.Render();
		const auto first = text.find("first_metric 1");
		const auto second = text.find("second_metric 2");
		ASSERT_NE(first, std::string::npos);
		ASSERT_NE(second, std::string::npos);
		EXPECT_LT(first, second);
	}
} // namespace
