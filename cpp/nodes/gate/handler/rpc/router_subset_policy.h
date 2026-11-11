#pragma once

// gate 的路由服子集:纯计算部分(docs/design/connection-fanout-subsetting-20261106.md §3.2)。
//
// 不碰 gNode、注册表、通道和时钟,单测直接喂数据(cpp/nodes/gate/tests/router_subset_test.cpp)。
// 有状态的部分(准入、对账、延后关闭、选点)在 router_subset.{h,cpp}。

#include <algorithm>
#include <charconv>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <system_error>
#include <vector>

#include "node/system/discovery/rendezvous_subset.h"
#include "node/system/node/release_track.h"

namespace gate_router_subset
{
	// 每个 zone 连几台路由服。部署参数,与路由模式开关 GATE_CLIENT_RPC_ROUTER 同一类,不进 proto 配置。
	inline constexpr const char *kSubsetSizeEnv = "GATE_ROUTER_SUBSET_SIZE";
	inline constexpr std::size_t kDefaultSubsetSize = 3;
	// 下限 2:一台路由服重启时,本 zone 至少还有一台可用。
	inline constexpr std::size_t kMinSubsetSize = 2;
	inline constexpr std::size_t kMaxSubsetSize = 64;

	// 解析 GATE_ROUTER_SUBSET_SIZE:不设或空串取缺省;纯十进制整数且在 [kMinSubsetSize, kMaxSubsetSize] 内照用;
	// 其余(带空白、正负号、小数、越界)返回 nullopt,调用方拒启。宽松解析会让写错的部署悄悄落到别的值上。
	inline std::optional<std::size_t> ParseSubsetSize(const char *raw)
	{
		if (raw == nullptr || *raw == '\0')
		{
			return kDefaultSubsetSize;
		}
		const std::string_view text{raw};
		std::size_t value = 0;
		const auto [end, error] = std::from_chars(text.data(), text.data() + text.size(), value);
		if (error != std::errc{} || end != text.data() + text.size())
		{
			return std::nullopt;
		}
		if (value < kMinSubsetSize || value > kMaxSubsetSize)
		{
			return std::nullopt;
		}
		return value;
	}

	// 发现快照里的一台路由服。uuid 借用快照里的字符串,只在一次计算里用。
	struct RouterMember
	{
		std::string_view uuid;
		// nullopt = 轨道认不出:不进任何组(选点本来就跳过它)。
		std::optional<release_track::Track> track;
	};

	// HRW 的 key:本 gate 的 zone 号,十进制串。同 zone 的 gate 用同一个 key,算出同一个子集。
	inline std::string SubsetKey(uint32_t zoneId)
	{
		return std::to_string(zoneId);
	}

	// 本 gate 该连、该挑的路由服 uuid:按轨道分组,各组取 HRW 前 subsetSize 台,再并起来;结果按 uuid 升序、无重复。
	// stable gate 只取 stable 组。canary gate 取 canary 组与 stable 组:release_track::Select 在 canary 组没有可用实例时
	// 退回 stable,退回的那几台也要连着。
	inline std::vector<std::string> ComputeSubset(uint32_t zoneId, release_track::Track selfTrack,
												  const std::vector<RouterMember> &members, std::size_t subsetSize)
	{
		const std::string key = SubsetKey(zoneId);
		std::vector<std::string> subset;
		const auto takeTrack = [&](release_track::Track want)
		{
			std::vector<std::string_view> group;
			for (const auto &member : members)
			{
				if (member.track == want)
				{
					group.push_back(member.uuid);
				}
			}
			for (const std::size_t index : rendezvous_subset::TopIndices(key, group, subsetSize))
			{
				subset.emplace_back(group[index]);
			}
		};
		if (selfTrack == release_track::Track::kCanary)
		{
			takeTrack(release_track::Track::kCanary);
		}
		takeTrack(release_track::Track::kStable);
		std::sort(subset.begin(), subset.end());
		subset.erase(std::unique(subset.begin(), subset.end()), subset.end());
		return subset;
	}

	// subset 是 ComputeSubset 的结果(升序、无重复)。
	inline bool SubsetContains(const std::vector<std::string> &subset, std::string_view uuid)
	{
		return std::binary_search(subset.begin(), subset.end(), uuid,
								  [](std::string_view left, std::string_view right) { return left < right; });
	}

	// 一次对账要做的事,Subsetter::Reconcile 照着执行。各列表按入参顺序给出。
	struct ReconcilePlan
	{
		// 在子集里、在发现快照里、还没建连:建连(服务启动之后才建,启动时由 ConnectAllNodes 按准入建)。
		std::vector<std::string> connect;
		// 已建连、仍在快照里、掉出了子集、还没排过关闭:排一次延后关闭。
		std::vector<std::string> scheduleClose;
		// 已排关闭,但回到了子集、或已不在快照里(节点摘除会拆它)、或连接已经没了:撤销。
		std::vector<std::string> cancelClose;
	};

	// discovered:发现快照里的路由服;connected:本节点已建连(注册表里有实体)的路由服;pendingClose:已排延后关闭的。
	inline ReconcilePlan PlanReconcile(const std::vector<std::string> &subset,
									   const std::vector<std::string_view> &discovered,
									   const std::vector<std::string_view> &connected,
									   const std::vector<std::string_view> &pendingClose)
	{
		const auto contains = [](const std::vector<std::string_view> &uuids, std::string_view uuid)
		{
			return std::find(uuids.begin(), uuids.end(), uuid) != uuids.end();
		};
		ReconcilePlan plan;
		for (const auto uuid : discovered)
		{
			if (SubsetContains(subset, uuid) && !contains(connected, uuid))
			{
				plan.connect.emplace_back(uuid);
			}
		}
		for (const auto uuid : connected)
		{
			if (!SubsetContains(subset, uuid) && contains(discovered, uuid) && !contains(pendingClose, uuid))
			{
				plan.scheduleClose.emplace_back(uuid);
			}
		}
		for (const auto uuid : pendingClose)
		{
			if (SubsetContains(subset, uuid) || !contains(discovered, uuid) || !contains(connected, uuid))
			{
				plan.cancelClose.emplace_back(uuid);
			}
		}
		return plan;
	}

	// 选点时候选通道的状态分级,枚举顺序即优先顺序。
	enum class ChannelHealth : uint8_t
	{
		kReady,       // 已收到对端 SETTINGS
		kConnectable, // IDLE / CONNECTING:调用会触发或等待建连
		kOther,       // TRANSIENT_FAILURE 等:调用多半立即以 UNAVAILABLE 失败
		kCount,
	};

	// 在候选里挑一个,返回下标:取最好的那一级,级内按 randomValue 对该级个数取模,序号沿用 candidates 的顺序。
	// 空表返回 nullopt,非空表一定返回其中一个。子集只有 k 台,一台进程挂了、租约到期之前它的通道处在
	// TRANSIENT_FAILURE,不分级就有 1/k 的请求失败;分级后只有整组都不健康时才会挑到它。
	inline std::optional<std::size_t> PickByHealth(const std::vector<ChannelHealth> &candidates, uint64_t randomValue)
	{
		for (std::size_t tier = 0; tier < static_cast<std::size_t>(ChannelHealth::kCount); ++tier)
		{
			const auto want = static_cast<ChannelHealth>(tier);
			const auto tierSize = static_cast<std::size_t>(std::count(candidates.begin(), candidates.end(), want));
			if (tierSize == 0)
			{
				continue;
			}
			auto remaining = randomValue % tierSize;
			for (std::size_t index = 0; index < candidates.size(); ++index)
			{
				if (candidates[index] != want)
				{
					continue;
				}
				if (remaining == 0)
				{
					return index;
				}
				--remaining;
			}
		}
		return std::nullopt;
	}
} // namespace gate_router_subset
