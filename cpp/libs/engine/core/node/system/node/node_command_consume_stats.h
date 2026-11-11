#pragma once

// 控制面命令 topic 的读放大计数(docs/design/connection-fanout-subsetting-20261106.md §3.5)。
//
// 每个节点只 assign `<类型>-cmd_g<N>` 的 node_id % P 号分区(node_kafka_command_handler.h),同分区上别的节点的命令
// 也要解码一遍再丢掉,读放大约等于同分区的节点数。按 (节点类型, topic 基名) 记:
//   own     = 目标是本节点,已派发;
//   foreign = 目标过滤判定不是本节点(同分区的别的节点,或同号前任实例的残留命令),解码后丢弃。
// 解码失败的不计(解码器自己打日志)。广播 topic(assignAllPartitions)不带目标,只会有 own。
// 告警在 deploy/k8s/node-command-topic-alerts.yaml:持续超标就按 node_command_topic.h 的契约换代号、换更大的 P,
// 不在同名 topic 上扩分区。
//
// 计数表只在注册命令消费时追加(启动期),deque 保证元素地址不变,派发路径只做 relaxed 原子加。

#include <atomic>
#include <cstdint>
#include <deque>
#include <mutex>
#include <string>
#include <utility>

#include "metrics/prometheus_text.h"

namespace node::kafka
{
	struct CommandConsumeCounters
	{
		CommandConsumeCounters(std::string nodeTypeName, std::string topicBaseName)
			: nodeType(std::move(nodeTypeName)), topicBase(std::move(topicBaseName))
		{
		}

		const std::string nodeType;	 // 节点类型短名(gate / scene / …)
		const std::string topicBase; // topic 基名,不带代号:换代号时序列不断
		std::atomic<uint64_t> own{0};
		std::atomic<uint64_t> foreign{0};
	};

	class CommandConsumeStats
	{
	public:
		// 取 (节点类型, topic 基名) 的计数。同一对再取返回同一份:同一 topic 注册两次也只有一条序列。
		CommandConsumeCounters &CountersFor(const std::string &nodeType, const std::string &topicBase)
		{
			std::lock_guard lock(mutex_);
			for (auto &counters : counters_)
			{
				if (counters.nodeType == nodeType && counters.topicBase == topicBase)
				{
					return counters;
				}
			}
			return counters_.emplace_back(nodeType, topicBase);
		}

		// 写出 node_command_consumed_total(MetricsRegistry 的采集回调)。锁只在启动期注册时有竞争。
		void Collect(prometheus_text::Writer &writer) const
		{
			writer.BeginFamily("node_command_consumed_total",
							   "Control-plane commands decoded from this node's command topic partition since process start, "
							   "by topic base and whether the command targeted this node (own) or another node on the same "
							   "partition (foreign).",
							   prometheus_text::MetricType::kCounter);
			std::lock_guard lock(mutex_);
			for (const auto &counters : counters_)
			{
				writer.Sample({{"node_type", counters.nodeType}, {"topic", counters.topicBase}, {"target", "own"}},
							  counters.own.load(std::memory_order_relaxed));
				writer.Sample({{"node_type", counters.nodeType}, {"topic", counters.topicBase}, {"target", "foreign"}},
							  counters.foreign.load(std::memory_order_relaxed));
			}
		}

	private:
		mutable std::mutex mutex_;
		std::deque<CommandConsumeCounters> counters_;
	};
} // namespace node::kafka
