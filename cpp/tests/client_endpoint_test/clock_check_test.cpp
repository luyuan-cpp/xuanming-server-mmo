#include <gtest/gtest.h>

#include <cstdint>

#include "node/system/node/clock_check.h"

// clock_check 的单元测试:Node 启动时钟门槛的纯判定(偏差怎么算、什么时候拒启、warn 开关)。
// 发请求、读 Date 头在 node.cpp 的 EnsureClockOrDie,不在这里跑。

TEST(ClockCheck, OffsetComparesTheMidpointOfTheRequestWithTheMidpointOfTheDateSecond)
{
	// 请求前后本机是 1000.000 秒、1000.200 秒,Date 是第 996 秒:本机中点 1000.100,参照中点 996.500,本机快 3.6 秒。
	EXPECT_EQ(clock_check::OffsetMillis(1'000'000, 1'000'200, 996), 3'600);
}

TEST(ClockCheck, JudgeRefusesBeyondTheLimitUnlessTheModeIsWarn)
{
	using clock_check::Verdict;
	constexpr int64_t kOneYearMillis = int64_t{365} * 24 * 3600 * 1000;
	EXPECT_EQ(clock_check::Judge(clock_check::kMaxOffsetMillis, nullptr), Verdict::kOk);
	EXPECT_EQ(clock_check::Judge(-clock_check::kMaxOffsetMillis, nullptr), Verdict::kOk);
	EXPECT_EQ(clock_check::Judge(clock_check::kMaxOffsetMillis + 1, nullptr), Verdict::kRefuse);
	EXPECT_EQ(clock_check::Judge(-clock_check::kMaxOffsetMillis - 1, "enforce"), Verdict::kRefuse);
	EXPECT_EQ(clock_check::Judge(-kOneYearMillis, "warn"), Verdict::kWarnOnly);
	EXPECT_EQ(clock_check::Judge(-kOneYearMillis, "WARN"), Verdict::kRefuse);
}
