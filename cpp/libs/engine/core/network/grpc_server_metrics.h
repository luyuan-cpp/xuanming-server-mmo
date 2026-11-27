#pragma once

#include <array>
#include <cstdint>
#include <map>
#include <string_view>

#include "../metrics/prometheus_text.h"
#include "muduo_grpc/server.h"
#include "muduo_grpc/status.h"

// 节点 gRPC 服务端(muduo_grpc::Server)的统计写成 Prometheus 指标(2026-11-27 起;docs/design/grpc-muduo-transport.md §15.11)。
//
// muduo_grpc 是传输库,不认指标(infra-reconnect-overview.md §1.8「传输层不认指标」):它只给值类型的快照 ServerStatsSnapshot
// 和每 30s 一行的 [GrpcServer] summary,由 core 在这里把快照写成 cpp_node_grpc_server_*。Node 的 /metrics 出口每次抓取时
// 在 loop 线程上取一次快照(Server::Stats 断言 loop 线程)再调 Write。
//
// 维度只有两个封闭集合:方法(Start 之后的已登记方法,≤15 个;快照里没被调用过的方法也在,见 ServerStatsSnapshot::methods)
// 与 gRPC 状态码(0..16 共 17 个)。listen 地址与 peer 不做标签。每个方法 × 每个状态码、每个原因进程一启动就以 0 导出,
// increase() 抓得到第一次。汇总行与这里是同一份快照的两个读者,字段一一对应(单测钉住)。
// 纯逻辑:只读快照、不碰 loop 与网络,单测在 cpp/tests/rpc_controller_test/grpc_server_metrics_test.cpp。
namespace grpc_server_metrics
{
	// 状态码 → 次数,摊到 0..16 各一格;越界的码按规范当 UNKNOWN(status.h 的 kMaxStatusCodeValue),保证取值集合封闭。
	inline std::array<uint64_t, muduo_grpc::kMaxStatusCodeValue + 1> CountsByCode(const std::map<uint32_t, uint64_t> &codes)
	{
		std::array<uint64_t, muduo_grpc::kMaxStatusCodeValue + 1> counts{};
		for (const auto &[code, count] : codes)
		{
			const uint32_t slot = code <= muduo_grpc::kMaxStatusCodeValue ? code : static_cast<uint32_t>(muduo_grpc::StatusCode::kUnknown);
			counts[slot] += count;
		}
		return counts;
	}

	inline std::string_view CodeName(uint32_t code)
	{
		return muduo_grpc::StatusCodeName(static_cast<muduo_grpc::StatusCode>(code));
	}

	inline void Write(prometheus_text::Writer &writer, const muduo_grpc::ServerStatsSnapshot &stats)
	{
		using prometheus_text::MetricType;
		const auto single = [&writer](std::string_view name, std::string_view help, MetricType type, uint64_t value)
		{
			writer.BeginFamily(name, help, type);
			writer.Sample({}, value);
		};

		// 连接(汇总行的 conns= accepted= closed_before_preface= handshake_timeout= protocol_errors=)。
		single("cpp_node_grpc_server_connections", "Connections currently open to this node's gRPC server.", MetricType::kGauge,
			   stats.connections);
		single("cpp_node_grpc_server_connections_max", "Most connections open at once since the gRPC server started.",
			   MetricType::kGauge, stats.connectionsPeak);
		single("cpp_node_grpc_server_connections_accepted_total", "Connections the gRPC server accepted.", MetricType::kCounter,
			   stats.accepted);
		single("cpp_node_grpc_server_connections_closed_before_preface_total",
			   "Connections removed before they sent a single byte (TCP probes, handshake timeouts, connections closed at "
			   "shutdown).",
			   MetricType::kCounter, stats.closedBeforePreface);
		single("cpp_node_grpc_server_handshake_timeouts_total",
			   "Connections closed because no non-ACK SETTINGS frame arrived within the handshake timeout.", MetricType::kCounter,
			   stats.handshakeTimeout);
		single("cpp_node_grpc_server_connection_protocol_errors_total",
			   "Connections ended by a connection-level error: bad client magic, an HTTP/2 framing error or flooding.",
			   MetricType::kCounter, stats.protocolErrors);

		// 调用(inflight= dispatched= max_backlog= max_dispatched_per_turn=)。
		single("cpp_node_grpc_server_calls_in_flight", "Calls currently queued or dispatched and not yet completed.",
			   MetricType::kGauge, stats.inFlight);
		single("cpp_node_grpc_server_calls_in_flight_max", "Most calls in flight at once since the gRPC server started.",
			   MetricType::kGauge, stats.inFlightPeak);
		single("cpp_node_grpc_server_calls_dispatched_total", "Calls handed to a handler.", MetricType::kCounter, stats.dispatched);
		single("cpp_node_grpc_server_dispatch_backlog_max", "Longest dispatch queue since the gRPC server started.",
			   MetricType::kGauge, stats.maxBacklog);
		single("cpp_node_grpc_server_dispatched_per_turn_max",
			   "Most calls dispatched in one dispatch turn since the gRPC server started.", MetricType::kGauge,
			   stats.maxDispatchedPerTurn);

		// 派发前拒绝(rejected_overload= rejected_protocol= unimplemented=):三者互斥,一条调用至多计一个。
		writer.BeginFamily("cpp_node_grpc_server_calls_rejected_total",
						   "Calls refused before dispatch, the handler never ran: overload (the node-wide in-flight limit, "
						   "RESOURCE_EXHAUSTED), protocol (a request the HTTP/2 session layer rejected), unimplemented (unknown "
						   "service or method).",
						   MetricType::kCounter);
		writer.Sample({{"reason", "overload"}}, stats.rejectedOverload);
		writer.Sample({{"reason", "protocol"}}, stats.rejectedProtocol);
		writer.Sample({{"reason", "unimplemented"}}, stats.unimplemented);

		// 没以 handler 自己的应答结束的调用(deadline_* / cancelled_before_start / aborted_after_start / *_on_shutdown):
		// 按原因 × 业务开始与否;after_start 表示业务可能已经执行。
		writer.BeginFamily("cpp_node_grpc_server_calls_interrupted_total",
						   "Calls that did not end with the handler's own reply, by cause and by whether the business had "
						   "started: deadline (DEADLINE_EXCEEDED; before_start includes grpc-timeout <= 0 and expiry while "
						   "queued or waiting for a permit), peer_cancel (the peer reset the stream or disconnected), shutdown "
						   "(the drain expired: before_start got REFUSED_STREAM, after_start got UNAVAILABLE). after_start "
						   "means the business may have run.",
						   MetricType::kCounter);
		writer.Sample({{"cause", "deadline"}, {"stage", "before_start"}}, stats.deadlineBeforeStart);
		writer.Sample({{"cause", "deadline"}, {"stage", "after_start"}}, stats.deadlineAfterStart);
		writer.Sample({{"cause", "peer_cancel"}, {"stage", "before_start"}}, stats.cancelledBeforeStart);
		writer.Sample({{"cause", "peer_cancel"}, {"stage", "after_start"}}, stats.abortedAfterStart);
		writer.Sample({{"cause", "shutdown"}, {"stage", "before_start"}}, stats.refusedOnShutdown);
		writer.Sample({{"cause", "shutdown"}, {"stage", "after_start"}}, stats.unavailableOnShutdown);

		// 收口异常(late_finish= dropped= dropped_off_loop=)。
		writer.BeginFamily("cpp_node_grpc_server_call_anomalies_total",
						   "Completion anomalies: late_finish (a handler finished a call that had already completed, usually "
						   "after its deadline), dropped (every handle of a call was released without completing it, the peer "
						   "got UNAVAILABLE; a program defect), dropped_off_loop (a call handle was destroyed off the loop "
						   "thread, process-wide; should stay 0).",
						   MetricType::kCounter);
		writer.Sample({{"kind", "late_finish"}}, stats.lateFinish);
		writer.Sample({{"kind", "dropped"}}, stats.dropped);
		writer.Sample({{"kind", "dropped_off_loop"}}, stats.droppedOffLoop);

		// 回给对端的状态码(codes=):17 个码逐个导出。
		writer.BeginFamily("cpp_node_grpc_server_handled_total",
						   "gRPC statuses the gRPC server sent, by code; REFUSED_STREAM resets and calls the peer had already "
						   "abandoned are not counted.",
						   MetricType::kCounter);
		const auto codes = CountsByCode(stats.codes);
		for (uint32_t code = 0; code < codes.size(); ++code)
		{
			writer.Sample({{"code", CodeName(code)}}, codes[code]);
		}

		// 按方法的状态码(methods={}):每个已登记的方法 × 17 个码。找不到方法的调用(未知服务 / 方法、协议拒绝)
		// 只在上一族里。
		writer.BeginFamily("cpp_node_grpc_server_method_handled_total",
						   "gRPC statuses the gRPC server sent for each registered method, by method and code; every "
						   "registered method is exported with every code.",
						   MetricType::kCounter);
		for (const auto &[method, methodCodes] : stats.methods)
		{
			const auto counts = CountsByCode(methodCodes);
			for (uint32_t code = 0; code < counts.size(); ++code)
			{
				writer.Sample({{"method", method}, {"code", CodeName(code)}}, counts[code]);
			}
		}
	}
} // namespace grpc_server_metrics
