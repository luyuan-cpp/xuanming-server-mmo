#pragma once

// 确定性子集(HRW / rendezvous hashing,docs/design/connection-fanout-subsetting-20261106.md §3.1)。
//
// 一组成员按 (key, 成员 id) 打分,取分数最高的前 size 个。同一组输入在任何进程、任何平台上都得到同一个子集;
// 成员增减时,只有「前 size 名」因此变了的 key 换人,其余 key 的子集不动。
//
// 分数 = FNV-1a-64(key + "/" + 成员 id),再过一遍 splitmix64 的终混。不用 std::hash:它的结果随编译器和平台变,
// Windows 与 Linux 构建会算出不同的子集,不影响正确性,但会让问题难查。HRW 比的是整个 64 位分数,FNV-1a 末尾几个
// 字节对高位的扩散偏弱,终混是这类用法的常见做法。改打分规则会让滚动期间新旧进程选出不同的子集,只影响连接分布,
// 改之前先改用例里钉住的分数。
//
// 纯头文件,不依赖 muduo / proto,任何测试工程都能直接包含。

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <string_view>
#include <vector>

namespace rendezvous_subset
{
	inline constexpr uint64_t kFnvOffsetBasis = 14695981039346656037ull;
	inline constexpr uint64_t kFnvPrime = 1099511628211ull;

	// FNV-1a-64:从 state 起把 bytes 逐字节累加进去,返回新的状态。
	constexpr uint64_t Fnv1aAppend(uint64_t state, std::string_view bytes)
	{
		for (const char c : bytes)
		{
			state ^= static_cast<uint8_t>(c);
			state *= kFnvPrime;
		}
		return state;
	}

	constexpr uint64_t Fnv1a64(std::string_view bytes)
	{
		return Fnv1aAppend(kFnvOffsetBasis, bytes);
	}

	// splitmix64 的终混步。
	constexpr uint64_t Mix64(uint64_t z)
	{
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ull;
		z = (z ^ (z >> 27)) * 0x94d049bb133111ebull;
		return z ^ (z >> 31);
	}

	// 成员 member 在 key 下的分数。
	constexpr uint64_t Score(std::string_view key, std::string_view member)
	{
		uint64_t state = Fnv1aAppend(kFnvOffsetBasis, key);
		state = Fnv1aAppend(state, "/");
		state = Fnv1aAppend(state, member);
		return Mix64(state);
	}

	// members 里分数最高的前 size 个成员的下标,按分数从高到低;分数相同按成员 id 字典序升序。
	// 成员不足 size 个时返回全部下标(同样排好序);size 为 0 返回空。不修改 members。
	// 成员 id 应当互不相同(发现快照按 uuid 去过重);重复的 id 各算一个。
	inline std::vector<std::size_t> TopIndices(std::string_view key, const std::vector<std::string_view> &members,
											   std::size_t size)
	{
		struct Ranked
		{
			uint64_t score;
			std::size_t index;
		};
		std::vector<Ranked> ranked;
		ranked.reserve(members.size());
		for (std::size_t index = 0; index < members.size(); ++index)
		{
			ranked.push_back(Ranked{Score(key, members[index]), index});
		}
		const auto better = [&members](const Ranked &left, const Ranked &right)
		{
			if (left.score != right.score)
			{
				return left.score > right.score;
			}
			return members[left.index] < members[right.index];
		};
		const std::size_t keep = std::min(size, ranked.size());
		std::partial_sort(ranked.begin(), ranked.begin() + static_cast<std::ptrdiff_t>(keep), ranked.end(), better);

		std::vector<std::size_t> indices;
		indices.reserve(keep);
		for (std::size_t position = 0; position < keep; ++position)
		{
			indices.push_back(ranked[position].index);
		}
		return indices;
	}
} // namespace rendezvous_subset
