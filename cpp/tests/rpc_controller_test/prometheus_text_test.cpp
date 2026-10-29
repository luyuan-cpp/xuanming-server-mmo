// C++ 节点 Prometheus 出口的纯逻辑单测:文本格式写出器、端口环境变量解析、注册表。
// HTTP 收发由 muduo HttpServer 负责,出口只把 render() 的文本原样作为 200 应答体,这里不起监听。

#include <gtest/gtest.h>

#include <string>

#include "metrics/metrics_http_exporter.h"
#include "metrics/metrics_registry.h"
#include "metrics/prometheus_text.h"

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
