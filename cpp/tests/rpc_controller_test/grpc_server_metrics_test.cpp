// 节点 gRPC 服务端统计的 Prometheus 导出(core/network/grpc_server_metrics.h,2026-11-27 起;
// docs/design/grpc-muduo-transport.md §15.11)。muduo_grpc 只给快照 ServerStatsSnapshot,core 把它写成 cpp_node_grpc_server_*。
// 这里钉三件事:每个字段导出的值等于快照里的计数;每个原因、每个状态码、每个已登记方法 × 状态码都以 0 起步导出;
// 指标名与标签取值(以后的告警按这些名字写,改名后规则不报错、只会永远不响)。

#include <gtest/gtest.h>

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <string>
#include <string_view>

#include "metrics/prometheus_text.h"
#include "muduo_grpc/server.h"
#include "muduo_grpc/status.h"
#include "network/grpc_server_metrics.h"

namespace
{
	std::string Render(const muduo_grpc::ServerStatsSnapshot &stats)
	{
		prometheus_text::Writer writer;
		grpc_server_metrics::Write(writer, stats);
		EXPECT_EQ(writer.DroppedSamples(), 0u);
		return writer.Text();
	}

	// 样本行在行首;# HELP 行里也出现族名,按 "\n<序列> <值>\n" 找才不会落到 HELP 上。
	bool HasSample(const std::string &text, const std::string &series, uint64_t value)
	{
		return text.find("\n" + series + " " + std::to_string(value) + "\n") != std::string::npos;
	}

	// 状态码的规范名,下标即数值(muduo_grpc/status.h 的 StatusCodeName)。
	constexpr std::string_view kCodeNames[] = {
		"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND", "ALREADY_EXISTS",
		"PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED",
		"INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED",
	};
	static_assert(std::size(kCodeNames) == muduo_grpc::kMaxStatusCodeValue + 1);

	std::string Code(std::string_view family, std::string_view code)
	{
		return std::string(family) + "{code=\"" + std::string(code) + "\"}";
	}

	std::string MethodCode(std::string_view method, std::string_view code)
	{
		return "cpp_node_grpc_server_method_handled_total{method=\"" + std::string(method) + "\",code=\"" + std::string(code) + "\"}";
	}
} // namespace

// 与 muduo_grpc_test 的 ServerStatsFormatTest.FieldOrderIsStable 同一组数:汇总行里的每个字段都导出,值就是快照里的计数。
TEST(GrpcServerMetrics, EveryFieldIsExportedWithTheSnapshotValue)
{
	muduo_grpc::ServerStatsSnapshot stats;
	stats.listen = "10.0.0.7:7001";
	stats.connections = 1;
	stats.connectionsPeak = 2;
	stats.accepted = 3;
	stats.closedBeforePreface = 4;
	stats.handshakeTimeout = 5;
	stats.protocolErrors = 6;
	stats.inFlight = 7;
	stats.inFlightPeak = 8;
	stats.dispatched = 9;
	stats.maxBacklog = 10;
	stats.maxDispatchedPerTurn = 11;
	stats.rejectedOverload = 12;
	stats.rejectedProtocol = 13;
	stats.unimplemented = 14;
	stats.deadlineBeforeStart = 15;
	stats.deadlineAfterStart = 16;
	stats.cancelledBeforeStart = 17;
	stats.abortedAfterStart = 18;
	stats.refusedOnShutdown = 19;
	stats.unavailableOnShutdown = 20;
	stats.lateFinish = 21;
	stats.dropped = 22;
	stats.droppedOffLoop = 23;
	stats.codes = {{0, 100}, {14, 2}};
	stats.methods = {{"CreateBattle", {{0, 3}, {14, 1}}}, {"DestroyBattle", {{0, 2}}}};
	const std::string text = Render(stats);

	EXPECT_NE(text.find("# TYPE cpp_node_grpc_server_connections gauge\n"), std::string::npos) << text;
	EXPECT_NE(text.find("# TYPE cpp_node_grpc_server_calls_dispatched_total counter\n"), std::string::npos) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_connections", 1)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_connections_max", 2)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_connections_accepted_total", 3)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_connections_closed_before_preface_total", 4)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_handshake_timeouts_total", 5)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_connection_protocol_errors_total", 6)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_in_flight", 7)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_in_flight_max", 8)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_dispatched_total", 9)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_dispatch_backlog_max", 10)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_dispatched_per_turn_max", 11)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_rejected_total{reason=\"overload\"}", 12)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_rejected_total{reason=\"protocol\"}", 13)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_rejected_total{reason=\"unimplemented\"}", 14)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_interrupted_total{cause=\"deadline\",stage=\"before_start\"}", 15));
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_interrupted_total{cause=\"deadline\",stage=\"after_start\"}", 16));
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_interrupted_total{cause=\"peer_cancel\",stage=\"before_start\"}", 17));
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_interrupted_total{cause=\"peer_cancel\",stage=\"after_start\"}", 18));
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_interrupted_total{cause=\"shutdown\",stage=\"before_start\"}", 19));
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_interrupted_total{cause=\"shutdown\",stage=\"after_start\"}", 20));
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_call_anomalies_total{kind=\"late_finish\"}", 21)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_call_anomalies_total{kind=\"dropped\"}", 22)) << text;
	EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_call_anomalies_total{kind=\"dropped_off_loop\"}", 23)) << text;
	EXPECT_TRUE(HasSample(text, Code("cpp_node_grpc_server_handled_total", "OK"), 100)) << text;
	EXPECT_TRUE(HasSample(text, Code("cpp_node_grpc_server_handled_total", "UNAVAILABLE"), 2)) << text;
	EXPECT_TRUE(HasSample(text, MethodCode("CreateBattle", "OK"), 3)) << text;
	EXPECT_TRUE(HasSample(text, MethodCode("CreateBattle", "UNAVAILABLE"), 1)) << text;
	EXPECT_TRUE(HasSample(text, MethodCode("DestroyBattle", "OK"), 2)) << text;
	EXPECT_EQ(text.find("10.0.0.7"), std::string::npos) << "listen 地址不做标签";
}

// 服务端刚起、什么都没发生:每个单值、每个原因、每个状态码、每个已登记方法(快照里内层为空)× 状态码都以 0 导出,
// increase() 才抓得到第一次;没有名单之外的样本。
TEST(GrpcServerMetrics, EveryReasonCodeAndRegisteredMethodIsExportedFromZero)
{
	muduo_grpc::ServerStatsSnapshot stats;
	stats.methods = {{"CreateScene", {}}, {"DestroyScene", {}}};
	const std::string text = Render(stats);

	for (const std::string_view family :
		 {"cpp_node_grpc_server_connections", "cpp_node_grpc_server_connections_max",
		  "cpp_node_grpc_server_connections_accepted_total", "cpp_node_grpc_server_connections_closed_before_preface_total",
		  "cpp_node_grpc_server_handshake_timeouts_total", "cpp_node_grpc_server_connection_protocol_errors_total",
		  "cpp_node_grpc_server_calls_in_flight", "cpp_node_grpc_server_calls_in_flight_max",
		  "cpp_node_grpc_server_calls_dispatched_total", "cpp_node_grpc_server_dispatch_backlog_max",
		  "cpp_node_grpc_server_dispatched_per_turn_max"})
	{
		EXPECT_TRUE(HasSample(text, std::string(family), 0)) << family;
	}
	for (const std::string_view reason : {"overload", "protocol", "unimplemented"})
	{
		EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_calls_rejected_total{reason=\"" + std::string(reason) + "\"}", 0)) << reason;
	}
	for (const std::string_view cause : {"deadline", "peer_cancel", "shutdown"})
	{
		for (const std::string_view stage : {"before_start", "after_start"})
		{
			const std::string series = "cpp_node_grpc_server_calls_interrupted_total{cause=\"" + std::string(cause) + "\",stage=\"" +
									   std::string(stage) + "\"}";
			EXPECT_TRUE(HasSample(text, series, 0)) << series;
		}
	}
	for (const std::string_view kind : {"late_finish", "dropped", "dropped_off_loop"})
	{
		EXPECT_TRUE(HasSample(text, "cpp_node_grpc_server_call_anomalies_total{kind=\"" + std::string(kind) + "\"}", 0)) << kind;
	}
	for (const std::string_view code : kCodeNames)
	{
		EXPECT_TRUE(HasSample(text, Code("cpp_node_grpc_server_handled_total", code), 0)) << code;
		EXPECT_TRUE(HasSample(text, MethodCode("CreateScene", code), 0)) << code;
		EXPECT_TRUE(HasSample(text, MethodCode("DestroyScene", code), 0)) << code;
	}
	// 16 族各一行 HELP、一行 TYPE;样本:11 个单值 + 3 个拒绝原因 + 6 个中断 + 3 个异常 + 17 个码 + 2 个方法 × 17 个码。
	const std::size_t samples = 11 + 3 + 6 + 3 + 17 + 2 * 17;
	EXPECT_EQ(static_cast<std::size_t>(std::count(text.begin(), text.end(), '\n')), samples + 2 * 16) << text;
}

// 规范之外的状态码按 UNKNOWN 计(muduo_grpc/status.h),状态码标签的取值集合保持 17 个。
TEST(GrpcServerMetrics, OutOfRangeCodesCountAsUnknown)
{
	muduo_grpc::ServerStatsSnapshot stats;
	stats.codes = {{2, 1}, {99, 4}};
	stats.methods = {{"Echo", {{99, 5}}}};
	const std::string text = Render(stats);
	EXPECT_TRUE(HasSample(text, Code("cpp_node_grpc_server_handled_total", "UNKNOWN"), 5)) << text;
	EXPECT_TRUE(HasSample(text, MethodCode("Echo", "UNKNOWN"), 5)) << text;
	EXPECT_EQ(text.find("99"), std::string::npos) << text;
}
