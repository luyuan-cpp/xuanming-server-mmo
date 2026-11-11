#pragma once

// node_id 选号的纯计算部分(docs/design/connection-fanout-subsetting-20261106.md §3.4)。
//
// 分配键 `<Svc>.rpc/allocated/node_type/<t>/node_id/<n>` 全服唯一(跨 zone),CAS 决定谁拿到号;选号只是挑一个
// 「多半能拿到」的候选,规则是最小空闲号:号保持紧凑,gate 的号还要放进会话号的 15 位里。
// 已占的号来自三处:本节点发现快照里的同类节点、本次启动输掉过的号、分配键 CAS 失败时读回的全服清单。
// 纯头文件,不碰 etcd、日志与全局状态,单测直接调(cpp/tests/cross_zone_test/discovery_set_test.cpp)。

#include <charconv>
#include <cstdint>
#include <optional>
#include <string_view>
#include <system_error>
#include <unordered_set>

namespace node_id_selection
{
	// 分配键 key 在前缀 allocationPrefix 之后的号。不是这个前缀、后缀不是纯十进制、为 0 或超过 maxNodeId 时返回 nullopt:
	// 清单里认不出的键不当作已占,也不让它影响选号。
	inline std::optional<uint32_t> ParseAllocatedId(std::string_view allocationPrefix, std::string_view key,
													uint32_t maxNodeId)
	{
		if (key.size() <= allocationPrefix.size() || key.substr(0, allocationPrefix.size()) != allocationPrefix)
		{
			return std::nullopt;
		}
		const std::string_view digits = key.substr(allocationPrefix.size());
		uint32_t id = 0;
		const auto [end, error] = std::from_chars(digits.data(), digits.data() + digits.size(), id);
		if (error != std::errc{} || end != digits.data() + digits.size() || id == 0 || id > maxNodeId)
		{
			return std::nullopt;
		}
		return id;
	}

	// [1, maxNodeId] 里最小的、不在 used 里的号;全被占了返回 nullopt。
	inline std::optional<uint32_t> SmallestFreeId(const std::unordered_set<uint32_t> &used, uint32_t maxNodeId)
	{
		// 64 位循环变量:maxNodeId 取到 uint32 上限时也不会回绕成死循环。
		for (uint64_t id = 1; id <= maxNodeId; ++id)
		{
			if (!used.contains(static_cast<uint32_t>(id)))
			{
				return static_cast<uint32_t>(id);
			}
		}
		return std::nullopt;
	}
} // namespace node_id_selection
