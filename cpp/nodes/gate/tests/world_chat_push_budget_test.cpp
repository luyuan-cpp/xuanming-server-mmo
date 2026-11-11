// 世界频道推送的每台 gate 转发预算(handler/event/world_chat_push_budget.h,docs/design/chat-world-channel-fanout.md §4)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj。只测限速本身(GCRA):起步就有一个完整的
// 突发、按速率放行、空闲再久也不超过突发、时间倒退只会多拒绝。整数时长,边界值是精确的。
// 接到 Kafka 回调与 GateEventHandler 的那一层在 main.cpp,靠冒烟看 over_budget_total 的 WARN 采样行。

#include <gtest/gtest.h>

#include <chrono>

#include "handler/event/world_chat_push_budget.h"

namespace
{
	using Clock = std::chrono::steady_clock;

	int TakeAll(WorldChatPushBudget &budget, Clock::time_point now)
	{
		int taken = 0;
		while (budget.TryTake(now) && taken < 1000)
		{
			++taken;
		}
		return taken;
	}

	TEST(WorldChatPushBudget, StartsWithAFullBurst)
	{
		WorldChatPushBudget budget;
		const auto now = Clock::time_point{} + std::chrono::hours(1);
		EXPECT_EQ(TakeAll(budget, now), WorldChatPushBudget::kBurst);
		EXPECT_FALSE(budget.TryTake(now)) << "同一时刻用完之后不能再推";
	}

	TEST(WorldChatPushBudget, ReleasesOneMessagePerIntervalAfterTheBurst)
	{
		WorldChatPushBudget budget;
		const auto start = Clock::time_point{} + std::chrono::hours(1);
		TakeAll(budget, start);
		// 每秒 10 条:每 100ms 放行一条。
		EXPECT_TRUE(budget.TryTake(start + std::chrono::milliseconds(100)));
		EXPECT_FALSE(budget.TryTake(start + std::chrono::milliseconds(100)));
		EXPECT_FALSE(budget.TryTake(start + std::chrono::milliseconds(150))) << "不到一个间隔不放行";
		EXPECT_TRUE(budget.TryTake(start + std::chrono::milliseconds(200)));
		EXPECT_EQ(WorldChatPushBudget::kInterval, std::chrono::milliseconds(100));
	}

	TEST(WorldChatPushBudget, NeverAccumulatesBeyondTheBurst)
	{
		WorldChatPushBudget budget;
		const auto start = Clock::time_point{} + std::chrono::hours(1);
		TakeAll(budget, start);
		EXPECT_EQ(TakeAll(budget, start + std::chrono::minutes(10)), WorldChatPushBudget::kBurst)
			<< "空闲再久也只攒满一个突发";
	}

	TEST(WorldChatPushBudget, ClockGoingBackwardsOnlyRejects)
	{
		WorldChatPushBudget budget;
		const auto start = Clock::time_point{} + std::chrono::hours(1);
		TakeAll(budget, start);
		EXPECT_FALSE(budget.TryTake(start - std::chrono::seconds(5))) << "读数倒退不多放行";
		EXPECT_TRUE(budget.TryTake(start + std::chrono::milliseconds(100))) << "倒退被拒的那一次不改状态,之后照常按真实流逝放行";
	}
}
