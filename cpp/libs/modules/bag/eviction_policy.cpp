#include "eviction_policy.h"

#include <algorithm>
#include <utility>
#include <vector>

uint64_t EvictOldestFirst::AcquisitionOrderOf(Guid guid, const ItemComp & /*item*/)
{
	// 今天:snowflake guid 升序 ≈ 入包先后(高位是时间)。已知近似,两条例外
	// 路径写在头文件里。等 ItemComp 有了显式的获得序号字段,这里改成读它 ——
	// **整个淘汰轴只有这一个函数需要改**。
	return static_cast<uint64_t>(guid);
}

GuidVector EvictOldestFirst::SelectVictims(std::size_t needed, const ItemStore &store,
										   const IContainerLayout & /*layout*/) const
{
	if (needed == 0)
	{
		return {};
	}

	// 腾不够就一件都不挤 —— 见接口注释:销毁了一半仍然放不下,等于白白弄丢
	// 玩家的东西。这里先用实例总数快速判掉,省一次遍历排序。
	if (store.Size() < needed)
	{
		return {};
	}

	std::vector<std::pair<uint64_t, Guid>> byAge;
	byAge.reserve(store.Size());
	store.ForEach([&byAge](Guid guid, const ItemComp &item)
				  { byAge.emplace_back(AcquisitionOrderOf(guid, item), guid); });

	// 只需要最早的 needed 个,不必全序 —— partial_sort 就够。
	// 次键取 guid 保证同序号时结果稳定(遍历来自 unordered 容器,不定序)。
	const auto nth = byAge.begin() + static_cast<std::ptrdiff_t>(needed);
	std::partial_sort(byAge.begin(), nth, byAge.end(),
					  [](const auto &lhs, const auto &rhs)
					  { return lhs.first != rhs.first ? lhs.first < rhs.first
													  : lhs.second < rhs.second; });

	GuidVector victims;
	victims.reserve(needed);
	for (auto it = byAge.begin(); it != nth; ++it)
	{
		victims.push_back(it->second);
	}
	return victims;
}
