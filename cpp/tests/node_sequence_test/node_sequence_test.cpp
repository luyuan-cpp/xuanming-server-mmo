#include <gtest/gtest.h>
#include <functional>
#include <iostream>
#include <thread>
#include <unordered_set>
#include <vector>

#include "core/utils/id/snow_flake.h"
#include "core/utils/id/node_id_generator.h"

using GuidVector = std::vector<Guid>;
using GuidSet = std::unordered_set<Guid>;

using TransientNode32BitCompositeIdGenerator = TransientNodeCompositeIdGenerator<uint64_t, 32>;

TransientNode32BitCompositeIdGenerator idGenAtomic;

// 原值 0xffffff(1677 万)× 10 轮:Debug 下 unordered_set 要跑几分钟、吃 1GB 以上内存,
// 统一测试入口(tools/scripts/run_cpp_tests.ps1)按超时判死;而 32 位序列号要 42 亿次才回绕,
// 1677 万同样碰不到回绕,这个规模只有成本没有覆盖。压到 20 万 × 3 轮:唯一性与跨轮连续性
// 照样验,单次 <1s。真要验回绕请单独写针对 TransientNodeCompositeIdGenerator<uint64_t, N>
// 小位宽实例的用例,而不是硬跑。
static const std::size_t kTotalIds = 200000;
static const int32_t kRounds = 3;

void GenerateIdsIntoVector(GuidVector& out)
{
	out.reserve(kTotalIds);
	for (std::size_t i = 0; i < kTotalIds; ++i)
	{
		out.emplace_back(idGenAtomic.Generate());
	}
}

// ---------------------------------------------------------------------------
// NodeCompositeIdGenerator 测试
// ---------------------------------------------------------------------------

TEST(NodeCompositeIdTest, SingleGeneration)
{
	idGenAtomic.set_node_id(1);
	Guid id = idGenAtomic.Generate();
	EXPECT_NE(id, 0);
}

TEST(NodeCompositeIdTest, AllGeneratedIdsAreUnique)
{
	for (int32_t round = 0; round < kRounds; ++round)
	{
		GuidVector ids;
		GenerateIdsIntoVector(ids);

		GuidSet uniqueIds(ids.begin(), ids.end());
		ASSERT_EQ(uniqueIds.size(), ids.size())
			<< "Duplicate ID detected in round " << round;
	}
}

int main(int argc, char** argv)
{
	testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}
