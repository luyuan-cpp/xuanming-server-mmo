#pragma once

// 场景 → gate 广播的扇出:每次广播实际发给了几台 gate(docs/design/connection-fanout-subsetting-20261106.md §3.6)。
//
// 用途:决定要不要做「场景亲和」(同一场景的玩家尽量落在同几台 gate 上)。广播已经按 gate 分组、每台 gate 一份,
// 亲和只能减少一次广播涉及的 gate 数;这个直方图长期接近本 zone 的 gate 总数、而且 gate 的入站广播是瓶颈时才值得做。
//
// 两条广播路径分开记(kind 标签):players = 按收件人集合广播(AOI 进出视野、移动、视野内消息,BroadcastToPlayerClients),
// scene = 整场景广播(BroadcastToSceneClients)。场景类型(主世界 / 副本)不进标签:引擎层只有 scene_id,
// 而主世界与副本跑在不同的 scene 部署上(docs/ops/scene-node-role-split.md),抓取目标自带的标签已经能分开。
// 只记至少发给了一台 gate 的广播:收件人一个都不在线的空广播说明不了扇出。
//
// relaxed 原子,任何线程记都安全;采集回调在 /metrics 出口线程读,桶与总和分别读,两次读之间差一两次可以接受。

#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <string_view>

#include "metrics/prometheus_text.h"

namespace broadcast_fanout_stats
{
	enum class Kind : uint8_t
	{
		kPlayers,
		kScene,
		kCount,
	};

	inline constexpr std::array<std::string_view, static_cast<size_t>(Kind::kCount)> kKindNames{
		"players",
		"scene",
	};

	// 桶上界(台 gate)。单 zone 的 gate 数由开服容量限定,64 台以上并进溢出桶。
	inline constexpr std::array<uint64_t, 7> kUpperBounds{1, 2, 4, 8, 16, 32, 64};
	inline constexpr size_t kBucketCount = kUpperBounds.size() + 1;

	struct KindCounters
	{
		std::array<std::atomic<uint64_t>, kBucketCount> buckets{};
		std::atomic<uint64_t> gateSends{0}; // 各次广播的 gate 数之和(直方图的 _sum)
	};

	inline std::array<KindCounters, static_cast<size_t>(Kind::kCount)> &Counters()
	{
		static std::array<KindCounters, static_cast<size_t>(Kind::kCount)> counters;
		return counters;
	}

	// gates 落进的桶:第一个不小于它的上界;比全部上界都大时是最后的溢出桶。
	constexpr size_t BucketIndex(uint64_t gates)
	{
		size_t index = 0;
		while (index < kUpperBounds.size() && gates > kUpperBounds[index])
		{
			++index;
		}
		return index;
	}

	// 记一次广播发给了 gates 台 gate;0 台不记。
	inline void Record(Kind kind, uint64_t gates)
	{
		if (gates == 0)
		{
			return;
		}
		auto &counters = Counters()[static_cast<size_t>(kind)];
		counters.buckets[BucketIndex(gates)].fetch_add(1, std::memory_order_relaxed);
		counters.gateSends.fetch_add(gates, std::memory_order_relaxed);
	}

	// 写出 scene_broadcast_gate_fanout 直方图(MetricsRegistry 的采集回调,client_message_sender.h 的
	// RegisterBroadcastFanoutMetrics 登记)。
	inline void Collect(prometheus_text::Writer &writer)
	{
		writer.BeginFamily("scene_broadcast_gate_fanout",
						   "Gates each scene-to-gate broadcast was sent to since process start, by broadcast kind "
						   "(players = recipient set, scene = whole scene). Broadcasts that reached no gate are not counted.",
						   prometheus_text::MetricType::kHistogram);
		for (size_t kind = 0; kind < kKindNames.size(); ++kind)
		{
			const auto &counters = Counters()[kind];
			std::array<uint64_t, kBucketCount> buckets{};
			for (size_t i = 0; i < kBucketCount; ++i)
			{
				buckets[i] = counters.buckets[i].load(std::memory_order_relaxed);
			}
			writer.Histogram({{"kind", kKindNames[kind]}}, kUpperBounds, buckets,
							 counters.gateSends.load(std::memory_order_relaxed));
		}
	}
} // namespace broadcast_fanout_stats
