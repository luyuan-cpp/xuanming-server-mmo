#pragma once

#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <string_view>

#include "prometheus_text.h"

// C++ 节点到各依赖的连接状态指标(断线重连 R17,docs/design/infra-reconnect-overview.md §1.8、§4.1 R17)。
//
// 每类依赖三组数:此刻可用的连接有几条(gauge)、可用的连接意外断掉过几次、建连失败过几次(按原因)。标签只有依赖种类
// 与失败原因两个固定集合(AGENTS.md §9)。钩子在各连接自己的线程上调用(目前都在节点主 EventLoop 上),计数是进程级
// relaxed 原子,抓取时在出口所在的 loop 上读。
//
// 本头文件只有计数与写出,不依赖 muduo / muduo_grpc:thread_context 的 RedisManager、core 的 RpcClient 直接 include;
// muduo_grpc 通道经 ChannelOptions::linkObserver 由 core 接进来(network/grpc_link_metrics.h)。采集回调由 Node 在启动
// 指标出口时登记(node.cpp)。告警在 deploy/k8s/cpp-node-dependency-alerts.yaml,指标名由
// tools/scripts/tests/cpp_node_dependency_alerts_contract.tests.ps1 对照本文件钉住。
//
// 不在这里的(只有日志):Kafka(librdkafka 自管连接,生产者刻意不挂 event_cb,见 kafka_producer.h);节点链路的 TCP 建连失败
// (muduo 的 Connector 自己重试、不回调)。
namespace dependency_link_metrics
{
	enum class Dependency : uint8_t
	{
		kRedisPlayer,  // 玩家域 Redis:tlsPlayerRedis 的每条节点连接
		kRedisControl, // 控制集群 Redis:tlsControlRedis 的每条节点连接
		kRedisOther,   // 其余直接用 RedisManager 的连接(没标域的集群客户端、测试)
		kEtcd,         // etcd 的 gRPC 通道:注册与发现、热关停各一条
		kGrpc,         // 调 Go 服务的 gRPC 通道
		kNodeLink,     // 节点之间的 RPC 链路,身份握手完成才算可用
		kCount,
	};

	// 下标 = Dependency 的值,即 dependency 标签的取值。
	inline constexpr std::string_view kDependencyNames[] = {
		"redis_player", "redis_control", "redis_other", "etcd", "grpc", "node_link"};
	static_assert(std::size(kDependencyNames) == static_cast<size_t>(Dependency::kCount));

	enum class FailureReason : uint8_t
	{
		kConnect,   // 连不上、建连超时、握手完成前被对端关闭
		kAuth,      // Redis AUTH 被拒、发不出或截止前没有应答
		kHandshake, // 节点链路的身份握手失败
		kCount,
	};

	// 下标 = FailureReason 的值,即 reason 标签的取值。
	inline constexpr std::string_view kFailureReasonNames[] = {"connect", "auth", "handshake"};
	static_assert(std::size(kFailureReasonNames) == static_cast<size_t>(FailureReason::kCount));

	struct Counters
	{
		std::atomic<int64_t> connections{0};
		std::atomic<uint64_t> lost{0};
		std::array<std::atomic<uint64_t>, static_cast<size_t>(FailureReason::kCount)> failures{};
	};

	// 进程唯一的一份(函数内静态,首次使用时构造,静态存储期保证全零起步)。
	inline std::array<Counters, static_cast<size_t>(Dependency::kCount)> &AllCounters()
	{
		static std::array<Counters, static_cast<size_t>(Dependency::kCount)> counters;
		return counters;
	}

	inline Counters &Of(Dependency dependency) { return AllCounters()[static_cast<size_t>(dependency)]; }

	// 一条连接进入可用状态(Redis 握手与 AUTH 通过、gRPC 收到对端 SETTINGS、节点链路身份握手完成)。
	inline void OnUp(Dependency dependency) { Of(dependency).connections.fetch_add(1, std::memory_order_relaxed); }

	// 一条可用的连接没了。expected 为真表示本进程主动关的(停机、换连接周期、对端 GOAWAY 优雅退役),只让 connections 减一;
	// 否则另计一次意外断开。调用方保证每次 OnUp 恰好配一次 OnDown。
	inline void OnDown(Dependency dependency, bool expected)
	{
		Counters &counters = Of(dependency);
		counters.connections.fetch_sub(1, std::memory_order_relaxed);
		if (!expected)
		{
			counters.lost.fetch_add(1, std::memory_order_relaxed);
		}
	}

	inline void OnConnectFailed(Dependency dependency, FailureReason reason)
	{
		Of(dependency).failures[static_cast<size_t>(reason)].fetch_add(1, std::memory_order_relaxed);
	}

	// 写出三族指标。每类依赖、每个原因都以 0 导出:进程一启动就有序列,increase() 抓得到第一次。
	// 节点没用到的依赖 connections 恒为 0,所以告警只看失败与意外断开的增量,不看 connections 是否为 0。
	inline void Write(prometheus_text::Writer &writer)
	{
		using prometheus_text::MetricType;
		writer.BeginFamily("cpp_node_dependency_connections",
						   "Connections to this dependency that are currently usable (Redis past AUTH, gRPC past the HTTP/2 "
						   "handshake, node links past the identity handshake).",
						   MetricType::kGauge);
		for (size_t i = 0; i < std::size(kDependencyNames); ++i)
		{
			const int64_t connections = AllCounters()[i].connections.load(std::memory_order_relaxed);
			writer.Sample({{"dependency", kDependencyNames[i]}}, connections > 0 ? static_cast<uint64_t>(connections) : 0);
		}
		writer.BeginFamily("cpp_node_dependency_lost_total",
						   "Usable connections to this dependency that dropped without this process closing them "
						   "(peer closed, network error, liveness or keepalive timeout).",
						   MetricType::kCounter);
		for (size_t i = 0; i < std::size(kDependencyNames); ++i)
		{
			writer.Sample({{"dependency", kDependencyNames[i]}}, AllCounters()[i].lost.load(std::memory_order_relaxed));
		}
		writer.BeginFamily("cpp_node_dependency_connect_failures_total",
						   "Failed attempts to make a connection to this dependency usable: reason=connect (TCP or transport "
						   "handshake), auth (Redis AUTH rejected, unsendable or unanswered), handshake (node link identity).",
						   MetricType::kCounter);
		for (size_t i = 0; i < std::size(kDependencyNames); ++i)
		{
			for (size_t r = 0; r < std::size(kFailureReasonNames); ++r)
			{
				writer.Sample({{"dependency", kDependencyNames[i]}, {"reason", kFailureReasonNames[r]}},
							  AllCounters()[i].failures[r].load(std::memory_order_relaxed));
			}
		}
	}
} // namespace dependency_link_metrics
